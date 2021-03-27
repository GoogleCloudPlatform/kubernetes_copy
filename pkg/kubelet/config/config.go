/*
Copyright 2014 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package config

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	toolsevents "k8s.io/client-go/tools/events"
	"k8s.io/klog/v2"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
	"k8s.io/kubernetes/pkg/kubelet/events"
	kubetypes "k8s.io/kubernetes/pkg/kubelet/types"
	"k8s.io/kubernetes/pkg/kubelet/util/format"
)

// PodConfigNotificationMode describes how changes are sent to the update channel.
type PodConfigNotificationMode int

const (
	// PodConfigNotificationUnknown is the default value for
	// PodConfigNotificationMode when uninitialized.
	PodConfigNotificationUnknown PodConfigNotificationMode = iota
	// PodConfigNotificationSnapshot delivers the full configuration as a SET whenever
	// any change occurs.
	PodConfigNotificationSnapshot
	// PodConfigNotificationSnapshotAndUpdates delivers an UPDATE and DELETE message whenever pods are
	// changed, and a SET message if there are any additions or removals.
	PodConfigNotificationSnapshotAndUpdates
	// PodConfigNotificationIncremental delivers ADD, UPDATE, DELETE, REMOVE, RECONCILE to the update channel.
	PodConfigNotificationIncremental
)

type podStartupSLIObserver interface {
	ObservedPodOnWatch(pod *v1.Pod, when time.Time)
}

// PodConfig is a configuration mux that merges many sources of pod configuration into a single
// consistent structure, and then delivers incremental change notifications to listeners
// in order.
type PodConfig struct {
	pods *podStorage
	mux  *mux

	// the channel of denormalized changes passed to listeners
	updates chan kubetypes.PodUpdate

	// contains the list of all configured sources
	sourcesLock sync.Mutex
	sources     sets.Set[string]
}

// NewPodConfig creates an object that can merge many configuration sources into a stream
// of normalized updates to a pod configuration.
func NewPodConfig(mode PodConfigNotificationMode, recorder toolsevents.EventRecorder, startupSLIObserver podStartupSLIObserver) *PodConfig {
	updates := make(chan kubetypes.PodUpdate, 50)
	storage := newPodStorage(updates, mode, recorder, startupSLIObserver)
	podConfig := &PodConfig{
		pods:    storage,
		mux:     newMux(storage),
		updates: updates,
		sources: sets.Set[string]{},
	}
	return podConfig
}

// Channel creates or returns a config source channel.  The channel
// only accepts PodUpdates
func (c *PodConfig) Channel(ctx context.Context, source string) chan<- interface{} {
	c.sourcesLock.Lock()
	defer c.sourcesLock.Unlock()
	c.sources.Insert(source)
	return c.mux.ChannelWithContext(ctx, source)
}

// SeenAllSources returns true if seenSources contains all sources in the
// config, and also this config has received a SET message from each source.
func (c *PodConfig) SeenAllSources(seenSources sets.Set[string]) bool {
	if c.pods == nil {
		return false
	}
	c.sourcesLock.Lock()
	defer c.sourcesLock.Unlock()
	klog.V(5).InfoS("Looking for sources, have seen", "sources", sets.List(c.sources), "seenSources", seenSources)
	return seenSources.HasAll(sets.List(c.sources)...) && c.pods.seenSources(sets.List(c.sources)...)
}

// Updates returns a channel of updates to the configuration, properly denormalized.
func (c *PodConfig) Updates() <-chan kubetypes.PodUpdate {
	return c.updates
}

// Sync requests the full configuration be delivered to the update channel.
func (c *PodConfig) Sync() {
	c.pods.sync()
}

// podStorage manages the current pod state at any point in time and ensures updates
// to the channel are delivered in order.  Note that this object is an in-memory source of
// "truth" and on creation contains zero entries.  Once all previously read sources are
// available, then this object should be considered authoritative.
type podStorage struct {
	podLock sync.RWMutex
	// map of source name to pod uid to pod reference
	pods map[string]map[types.UID]*v1.Pod
	mode PodConfigNotificationMode

	// ensures that updates are delivered in strict order
	// on the updates channel
	updateLock sync.Mutex
	updates    chan<- kubetypes.PodUpdate

	// contains the set of all sources that have sent at least one SET
	sourcesSeenLock sync.RWMutex
	sourcesSeen     sets.Set[string]

	// the EventRecorder to use
	recorder toolsevents.EventRecorder

	startupSLIObserver podStartupSLIObserver
}

// TODO: PodConfigNotificationMode could be handled by a listener to the updates channel
// in the future, especially with multiple listeners.
// TODO: allow initialization of the current state of the store with snapshotted version.
func newPodStorage(updates chan<- kubetypes.PodUpdate, mode PodConfigNotificationMode, recorder toolsevents.EventRecorder, startupSLIObserver podStartupSLIObserver) *podStorage {
	return &podStorage{
		pods:               make(map[string]map[types.UID]*v1.Pod),
		mode:               mode,
		updates:            updates,
		sourcesSeen:        sets.Set[string]{},
		recorder:           recorder,
		startupSLIObserver: startupSLIObserver,
	}
}

// Merge normalizes a set of incoming changes from different sources into a map of all Pods
// and ensures that redundant changes are filtered out, and then pushes zero or more minimal
// updates onto the update channel.  Ensures that updates are delivered in order.
func (s *podStorage) Merge(source string, change interface{}) error {
	s.updateLock.Lock()
	defer s.updateLock.Unlock()

	seenBefore := s.sourcesSeen.Has(source)
	adds, updates, deletes, removes, reconciles := s.merge(source, change)
	firstSet := !seenBefore && s.sourcesSeen.Has(source)

	// deliver update notifications
	switch s.mode {
	case PodConfigNotificationIncremental:
		if len(removes.Pods) > 0 {
			s.updates <- *removes
		}
		if len(adds.Pods) > 0 {
			s.updates <- *adds
		}
		if len(updates.Pods) > 0 {
			s.updates <- *updates
		}
		if len(deletes.Pods) > 0 {
			s.updates <- *deletes
		}
		if firstSet && len(adds.Pods) == 0 && len(updates.Pods) == 0 && len(deletes.Pods) == 0 {
			// Send an empty update when first seeing the source and there are
			// no ADD or UPDATE or DELETE pods from the source. This signals kubelet that
			// the source is ready.
			s.updates <- *adds
		}
		// Only add reconcile support here, because kubelet doesn't support Snapshot update now.
		if len(reconciles.Pods) > 0 {
			s.updates <- *reconciles
		}

	case PodConfigNotificationSnapshotAndUpdates:
		if len(removes.Pods) > 0 || len(adds.Pods) > 0 || firstSet {
			s.updates <- kubetypes.PodUpdate{Pods: s.mergedState().([]*v1.Pod), Op: kubetypes.SET, Source: source}
		}
		if len(updates.Pods) > 0 {
			s.updates <- *updates
		}
		if len(deletes.Pods) > 0 {
			s.updates <- *deletes
		}

	case PodConfigNotificationSnapshot:
		if len(updates.Pods) > 0 || len(deletes.Pods) > 0 || len(adds.Pods) > 0 || len(removes.Pods) > 0 || firstSet {
			s.updates <- kubetypes.PodUpdate{Pods: s.mergedState().([]*v1.Pod), Op: kubetypes.SET, Source: source}
		}

	case PodConfigNotificationUnknown:
		fallthrough
	default:
		panic(fmt.Sprintf("unsupported PodConfigNotificationMode: %#v", s.mode))
	}

	return nil
}

func (s *podStorage) merge(source string, change interface{}) (adds, updates, deletes, removes, reconciles *kubetypes.PodUpdate) {
	s.podLock.Lock()
	defer s.podLock.Unlock()

	addPods := []*v1.Pod{}
	updatePods := []*v1.Pod{}
	deletePods := []*v1.Pod{}
	removePods := []*v1.Pod{}
	reconcilePods := []*v1.Pod{}

	pods := s.pods[source]
	if pods == nil {
		pods = make(map[types.UID]*v1.Pod)
	}

	// updatePodFunc is the local function which updates the pod cache *oldPods* with new pods *newPods*.
	// After updated, new pod will be stored in the pod cache *pods*.
	// Notice that *pods* and *oldPods* could be the same cache.
	updatePodsFunc := func(newPods []*v1.Pod, oldPods, pods map[types.UID]*v1.Pod) {
		filtered := filterInvalidPods(newPods, source, s.recorder)
		for _, ref := range filtered {
			// Annotate the pod with the source before any comparison.
			if ref.Annotations == nil {
				ref.Annotations = make(map[string]string)
			}
			ref.Annotations[kubetypes.ConfigSourceAnnotationKey] = source
			// ignore static pods
			if !kubetypes.IsStaticPod(ref) {
				s.startupSLIObserver.ObservedPodOnWatch(ref, time.Now())
			}
			if existing, found := oldPods[ref.UID]; found {
				pods[ref.UID] = existing
				needUpdate, needReconcile, needGracefulDelete := checkAndUpdatePod(existing, ref)
				if needUpdate {
					updatePods = append(updatePods, existing)
				} else if needReconcile {
					reconcilePods = append(reconcilePods, existing)
				} else if needGracefulDelete {
					deletePods = append(deletePods, existing)
				}
				continue
			}
			recordFirstSeenTime(ref)
			pods[ref.UID] = ref
			addPods = append(addPods, ref)
		}
	}

	update := change.(kubetypes.PodUpdate)
	switch update.Op {
	case kubetypes.ADD, kubetypes.UPDATE, kubetypes.DELETE:
		if update.Op == kubetypes.ADD {
			klog.V(4).InfoS("Adding new pods from source", "source", source, "pods", klog.KObjSlice(update.Pods))
		} else if update.Op == kubetypes.DELETE {
			klog.V(4).InfoS("Gracefully deleting pods from source", "source", source, "pods", klog.KObjSlice(update.Pods))
		} else {
			klog.V(4).InfoS("Updating pods from source", "source", source, "pods", klog.KObjSlice(update.Pods))
		}
		updatePodsFunc(update.Pods, pods, pods)

	case kubetypes.REMOVE:
		klog.V(4).InfoS("Removing pods from source", "source", source, "pods", klog.KObjSlice(update.Pods))
		for _, value := range update.Pods {
			if existing, found := pods[value.UID]; found {
				// this is a delete
				delete(pods, value.UID)
				removePods = append(removePods, existing)
				continue
			}
			// this is a no-op
		}

	case kubetypes.SET:
		klog.V(4).InfoS("Setting pods for source", "source", source)
		s.markSourceSet(source)
		// Clear the old map entries by just creating a new map
		oldPods := pods
		pods = make(map[types.UID]*v1.Pod)
		updatePodsFunc(update.Pods, oldPods, pods)
		for uid, existing := range oldPods {
			if _, found := pods[uid]; !found {
				// this is a delete
				removePods = append(removePods, existing)
			}
		}

	default:
		klog.InfoS("Received invalid update type", "type", update)

	}

	s.pods[source] = pods

	adds = &kubetypes.PodUpdate{Op: kubetypes.ADD, Pods: copyPods(addPods), Source: source}
	updates = &kubetypes.PodUpdate{Op: kubetypes.UPDATE, Pods: copyPods(updatePods), Source: source}
	deletes = &kubetypes.PodUpdate{Op: kubetypes.DELETE, Pods: copyPods(deletePods), Source: source}
	removes = &kubetypes.PodUpdate{Op: kubetypes.REMOVE, Pods: copyPods(removePods), Source: source}
	reconciles = &kubetypes.PodUpdate{Op: kubetypes.RECONCILE, Pods: copyPods(reconcilePods), Source: source}

	return adds, updates, deletes, removes, reconciles
}

func (s *podStorage) markSourceSet(source string) {
	s.sourcesSeenLock.Lock()
	defer s.sourcesSeenLock.Unlock()
	s.sourcesSeen.Insert(source)
}

func (s *podStorage) seenSources(sources ...string) bool {
	s.sourcesSeenLock.RLock()
	defer s.sourcesSeenLock.RUnlock()
	return s.sourcesSeen.HasAll(sources...)
}

func filterInvalidPods(pods []*v1.Pod, source string, recorder toolsevents.EventRecorder) (filtered []*v1.Pod) {
	names := sets.Set[string]{}
	for i, pod := range pods {
		// Pods from each source are assumed to have passed validation individually.
		// This function only checks if there is any naming conflict.
		name := kubecontainer.GetPodFullName(pod)
		if names.Has(name) {
			klog.InfoS("Pod failed validation due to duplicate pod name, ignoring", "index", i, "pod", klog.KObj(pod), "source", source)
			recorder.Eventf(pod, nil, v1.EventTypeWarning, events.FailedValidation, "ValidatingPod", "Error validating pod %s from %s due to duplicate pod name %q, ignoring", format.Pod(pod), source, pod.Name)
			continue
		} else {
			names.Insert(name)
		}

		filtered = append(filtered, pod)
	}
	return
}

// Annotations that the kubelet adds to the pod.
var localAnnotations = []string{
	kubetypes.ConfigSourceAnnotationKey,
	kubetypes.ConfigMirrorAnnotationKey,
	kubetypes.ConfigFirstSeenAnnotationKey,
}

func isLocalAnnotationKey(key string) bool {
	for _, localKey := range localAnnotations {
		if key == localKey {
			return true
		}
	}
	return false
}

// isAnnotationMapEqual returns true if the existing annotation Map is equal to candidate except
// for local annotations.
func isAnnotationMapEqual(existingMap, candidateMap map[string]string) bool {
	if candidateMap == nil {
		candidateMap = make(map[string]string)
	}
	for k, v := range candidateMap {
		if isLocalAnnotationKey(k) {
			continue
		}
		if existingValue, ok := existingMap[k]; ok && existingValue == v {
			continue
		}
		return false
	}
	for k := range existingMap {
		if isLocalAnnotationKey(k) {
			continue
		}
		// stale entry in existing map.
		if _, exists := candidateMap[k]; !exists {
			return false
		}
	}
	return true
}

// recordFirstSeenTime records the first seen time of this pod.
func recordFirstSeenTime(pod *v1.Pod) {
	klog.V(4).InfoS("Receiving a new pod", "pod", klog.KObj(pod))
	pod.Annotations[kubetypes.ConfigFirstSeenAnnotationKey] = kubetypes.NewTimestamp().GetString()
}

// updateAnnotations returns an Annotation map containing the api annotation map plus
// locally managed annotations
func updateAnnotations(existing, ref *v1.Pod) {
	annotations := make(map[string]string, len(ref.Annotations)+len(localAnnotations))
	for k, v := range ref.Annotations {
		annotations[k] = v
	}
	for _, k := range localAnnotations {
		if v, ok := existing.Annotations[k]; ok {
			annotations[k] = v
		}
	}
	existing.Annotations = annotations
}

func podsDifferSemantically(existing, ref *v1.Pod) bool {
	if reflect.DeepEqual(existing.Spec, ref.Spec) &&
		reflect.DeepEqual(existing.Labels, ref.Labels) &&
		reflect.DeepEqual(existing.DeletionTimestamp, ref.DeletionTimestamp) &&
		reflect.DeepEqual(existing.DeletionGracePeriodSeconds, ref.DeletionGracePeriodSeconds) &&
		isAnnotationMapEqual(existing.Annotations, ref.Annotations) {
		return false
	}
	return true
}

// checkAndUpdatePod updates existing, and:
//   - if ref makes a meaningful change, returns needUpdate=true
//   - if ref makes a meaningful change, and this change is graceful deletion, returns needGracefulDelete=true
//   - if ref makes no meaningful change, but changes the pod status, returns needReconcile=true
//   - else return all false
//     Now, needUpdate, needGracefulDelete and needReconcile should never be both true
func checkAndUpdatePod(existing, ref *v1.Pod) (needUpdate, needReconcile, needGracefulDelete bool) {

	// 1. this is a reconcile
	// TODO: it would be better to update the whole object and only preserve certain things
	//       like the source annotation or the UID (to ensure safety)
	if !podsDifferSemantically(existing, ref) {
		// this is not an update
		// Only check reconcile when it is not an update, because if the pod is going to
		// be updated, an extra reconcile is unnecessary
		if !reflect.DeepEqual(existing.Status, ref.Status) {
			// Pod with changed pod status needs reconcile, because kubelet should
			// be the source of truth of pod status.
			existing.Status = ref.Status
			needReconcile = true
		}
		return
	}

	// Overwrite the first-seen time with the existing one. This is our own
	// internal annotation, there is no need to update.
	ref.Annotations[kubetypes.ConfigFirstSeenAnnotationKey] = existing.Annotations[kubetypes.ConfigFirstSeenAnnotationKey]

	existing.Spec = ref.Spec
	existing.Labels = ref.Labels
	existing.DeletionTimestamp = ref.DeletionTimestamp
	existing.DeletionGracePeriodSeconds = ref.DeletionGracePeriodSeconds
	existing.Status = ref.Status
	updateAnnotations(existing, ref)

	// 2. this is an graceful delete
	if ref.DeletionTimestamp != nil {
		needGracefulDelete = true
	} else {
		// 3. this is an update
		needUpdate = true
	}

	return
}

// sync sends a copy of the current state through the update channel.
func (s *podStorage) sync() {
	s.updateLock.Lock()
	defer s.updateLock.Unlock()
	s.updates <- kubetypes.PodUpdate{Pods: s.mergedState().([]*v1.Pod), Op: kubetypes.SET, Source: kubetypes.AllSource}
}

func (s *podStorage) mergedState() interface{} {
	s.podLock.RLock()
	defer s.podLock.RUnlock()
	pods := make([]*v1.Pod, 0)
	for _, sourcePods := range s.pods {
		for _, podRef := range sourcePods {
			pods = append(pods, podRef.DeepCopy())
		}
	}
	return pods
}

func copyPods(sourcePods []*v1.Pod) []*v1.Pod {
	pods := []*v1.Pod{}
	for _, source := range sourcePods {
		// Use a deep copy here just in case
		pods = append(pods, source.DeepCopy())
	}
	return pods
}
