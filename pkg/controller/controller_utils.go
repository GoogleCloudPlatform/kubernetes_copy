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

package controller

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"math"
	"sync"
	"sync/atomic"
	"time"

	apps "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	"k8s.io/apimachinery/pkg/util/wait"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	clientretry "k8s.io/client-go/util/retry"
	podutil "k8s.io/kubernetes/pkg/api/v1/pod"
	"k8s.io/kubernetes/pkg/apis/core/helper"
	_ "k8s.io/kubernetes/pkg/apis/core/install"
	"k8s.io/kubernetes/pkg/apis/core/validation"
	"k8s.io/kubernetes/pkg/features"
	hashutil "k8s.io/kubernetes/pkg/util/hash"
	taintutils "k8s.io/kubernetes/pkg/util/taints"
	"k8s.io/utils/clock"

	"k8s.io/klog/v2"
)

const (
	// If a watch drops a delete event for a pod, it'll take this long
	// before a dormant controller waiting for those packets is woken up anyway. It is
	// specifically targeted at the case where some problem prevents an update
	// of expectations, without it the controller could stay asleep forever. This should
	// be set based on the expected latency of watch events.
	//
	// Currently a controller can service (create *and* observe the watch events for said
	// creation) about 10 pods a second, so it takes about 1 min to service
	// 500 pods. Just creation is limited to 20qps, and watching happens with ~10-30s
	// latency/pod at the scale of 3000 pods over 100 nodes.
	ExpectationsTimeout = 5 * time.Minute
	// When batching pod creates, SlowStartInitialBatchSize is the size of the
	// initial batch.  The size of each successive batch is twice the size of
	// the previous batch.  For example, for a value of 1, batch sizes would be
	// 1, 2, 4, 8, ...  and for a value of 10, batch sizes would be
	// 10, 20, 40, 80, ...  Setting the value higher means that quota denials
	// will result in more doomed API calls and associated event spam.  Setting
	// the value lower will result in more API call round trip periods for
	// large batches.
	//
	// Given a number of pods to start "N":
	// The number of doomed calls per sync once quota is exceeded is given by:
	//      min(N,SlowStartInitialBatchSize)
	// The number of batches is given by:
	//      1+floor(log_2(ceil(N/SlowStartInitialBatchSize)))
	SlowStartInitialBatchSize = 1
	// When scaling down the pods of a ReplicaSet, prioritize removing the specified pod.
	SpecifiedToDeleteAnnotation string = "k8s.io/specified-to-delete"
)

var UpdateTaintBackoff = wait.Backoff{
	Steps:    5,
	Duration: 100 * time.Millisecond,
	Jitter:   1.0,
}

var UpdateLabelBackoff = wait.Backoff{
	Steps:    5,
	Duration: 100 * time.Millisecond,
	Jitter:   1.0,
}

var (
	KeyFunc           = cache.DeletionHandlingMetaNamespaceKeyFunc
	podPhaseToOrdinal = map[v1.PodPhase]int{v1.PodPending: 0, v1.PodUnknown: 1, v1.PodRunning: 2}
)

type ResyncPeriodFunc func() time.Duration

// Returns 0 for resyncPeriod in case resyncing is not needed.
func NoResyncPeriodFunc() time.Duration {
	return 0
}

// StaticResyncPeriodFunc returns the resync period specified
func StaticResyncPeriodFunc(resyncPeriod time.Duration) ResyncPeriodFunc {
	return func() time.Duration {
		return resyncPeriod
	}
}

// Expectations are a way for controllers to tell the controller manager what they expect. eg:
//	ControllerExpectations: {
//		controller1: expects  2 adds in 2 minutes
//		controller2: expects  2 dels in 2 minutes
//		controller3: expects -1 adds in 2 minutes => controller3's expectations have already been met
//	}
//
// Implementation:
//	ControlleeExpectation = pair of atomic counters to track controllee's creation/deletion
//	ControllerExpectationsStore = TTLStore + a ControlleeExpectation per controller
//
// * Once set expectations can only be lowered
// * A controller isn't synced till its expectations are either fulfilled, or expire
// * Controllers that don't set expectations will get woken up for every matching controllee

// ExpKeyFunc to parse out the key from a ControlleeExpectation
var ExpKeyFunc = func(obj interface{}) (string, error) {
	if e, ok := obj.(*ControlleeExpectations); ok {
		return e.key, nil
	}
	return "", fmt.Errorf("could not find key for obj %#v", obj)
}

// ControllerExpectationsInterface is an interface that allows users to set and wait on expectations.
// Only abstracted out for testing.
// Warning: if using KeyFunc it is not safe to use a single ControllerExpectationsInterface with different
// types of controllers, because the keys might conflict across types.
type ControllerExpectationsInterface interface {
	GetExpectations(controllerKey string) (*ControlleeExpectations, bool, error)
	SatisfiedExpectations(logger klog.Logger, controllerKey string) bool
	DeleteExpectations(logger klog.Logger, controllerKey string)
	SetExpectations(logger klog.Logger, controllerKey string, add, del int) error
	ExpectCreations(logger klog.Logger, controllerKey string, adds int) error
	ExpectDeletions(logger klog.Logger, controllerKey string, dels int) error
	CreationObserved(logger klog.Logger, controllerKey string)
	DeletionObserved(logger klog.Logger, controllerKey string)
	RaiseExpectations(logger klog.Logger, controllerKey string, add, del int)
	LowerExpectations(logger klog.Logger, controllerKey string, add, del int)
}

// ControllerExpectations is a cache mapping controllers to what they expect to see before being woken up for a sync.
type ControllerExpectations struct {
	cache.Store
}

// GetExpectations returns the ControlleeExpectations of the given controller.
func (r *ControllerExpectations) GetExpectations(controllerKey string) (*ControlleeExpectations, bool, error) {
	exp, exists, err := r.GetByKey(controllerKey)
	if err == nil && exists {
		return exp.(*ControlleeExpectations), true, nil
	}
	return nil, false, err
}

// DeleteExpectations deletes the expectations of the given controller from the TTLStore.
func (r *ControllerExpectations) DeleteExpectations(logger klog.Logger, controllerKey string) {
	if exp, exists, err := r.GetByKey(controllerKey); err == nil && exists {
		if err := r.Delete(exp); err != nil {

			logger.V(2).Info("Error deleting expectations", "controller", controllerKey, "err", err)
		}
	}
}

// SatisfiedExpectations returns true if the required adds/dels for the given controller have been observed.
// Add/del counts are established by the controller at sync time, and updated as controllees are observed by the controller
// manager.
func (r *ControllerExpectations) SatisfiedExpectations(logger klog.Logger, controllerKey string) bool {
	if exp, exists, err := r.GetExpectations(controllerKey); exists {
		if exp.Fulfilled() {
			logger.V(4).Info("Controller expectations fulfilled", "expectations", exp)
			return true
		} else if exp.isExpired() {
			logger.V(4).Info("Controller expectations expired", "expectations", exp)
			return true
		} else {
			logger.V(4).Info("Controller still waiting on expectations", "expectations", exp)
			return false
		}
	} else if err != nil {
		logger.V(2).Info("Error encountered while checking expectations, forcing sync", "err", err)
	} else {
		// When a new controller is created, it doesn't have expectations.
		// When it doesn't see expected watch events for > TTL, the expectations expire.
		//	- In this case it wakes up, creates/deletes controllees, and sets expectations again.
		// When it has satisfied expectations and no controllees need to be created/destroyed > TTL, the expectations expire.
		//	- In this case it continues without setting expectations till it needs to create/delete controllees.
		logger.V(4).Info("Controller either never recorded expectations, or the ttl expired", "controller", controllerKey)
	}
	// Trigger a sync if we either encountered and error (which shouldn't happen since we're
	// getting from local store) or this controller hasn't established expectations.
	return true
}

// TODO: Extend ExpirationCache to support explicit expiration.
// TODO: Make this possible to disable in tests.
// TODO: Support injection of clock.
func (exp *ControlleeExpectations) isExpired() bool {
	return clock.RealClock{}.Since(exp.timestamp) > ExpectationsTimeout
}

// SetExpectations registers new expectations for the given controller. Forgets existing expectations.
func (r *ControllerExpectations) SetExpectations(logger klog.Logger, controllerKey string, add, del int) error {
	exp := &ControlleeExpectations{add: int64(add), del: int64(del), key: controllerKey, timestamp: clock.RealClock{}.Now()}
	logger.V(4).Info("Setting expectations", "expectations", exp)
	return r.Add(exp)
}

func (r *ControllerExpectations) ExpectCreations(logger klog.Logger, controllerKey string, adds int) error {
	return r.SetExpectations(logger, controllerKey, adds, 0)
}

func (r *ControllerExpectations) ExpectDeletions(logger klog.Logger, controllerKey string, dels int) error {
	return r.SetExpectations(logger, controllerKey, 0, dels)
}

// Decrements the expectation counts of the given controller.
func (r *ControllerExpectations) LowerExpectations(logger klog.Logger, controllerKey string, add, del int) {
	if exp, exists, err := r.GetExpectations(controllerKey); err == nil && exists {
		exp.Add(int64(-add), int64(-del))
		// The expectations might've been modified since the update on the previous line.
		logger.V(4).Info("Lowered expectations", "expectations", exp)
	}
}

// Increments the expectation counts of the given controller.
func (r *ControllerExpectations) RaiseExpectations(logger klog.Logger, controllerKey string, add, del int) {
	if exp, exists, err := r.GetExpectations(controllerKey); err == nil && exists {
		exp.Add(int64(add), int64(del))
		// The expectations might've been modified since the update on the previous line.
		logger.V(4).Info("Raised expectations", "expectations", exp)
	}
}

// CreationObserved atomically decrements the `add` expectation count of the given controller.
func (r *ControllerExpectations) CreationObserved(logger klog.Logger, controllerKey string) {
	r.LowerExpectations(logger, controllerKey, 1, 0)
}

// DeletionObserved atomically decrements the `del` expectation count of the given controller.
func (r *ControllerExpectations) DeletionObserved(logger klog.Logger, controllerKey string) {
	r.LowerExpectations(logger, controllerKey, 0, 1)
}

// ControlleeExpectations track controllee creates/deletes.
type ControlleeExpectations struct {
	// Important: Since these two int64 fields are using sync/atomic, they have to be at the top of the struct due to a bug on 32-bit platforms
	// See: https://golang.org/pkg/sync/atomic/ for more information
	add       int64
	del       int64
	key       string
	timestamp time.Time
}

// Add increments the add and del counters.
func (e *ControlleeExpectations) Add(add, del int64) {
	atomic.AddInt64(&e.add, add)
	atomic.AddInt64(&e.del, del)
}

// Fulfilled returns true if this expectation has been fulfilled.
func (e *ControlleeExpectations) Fulfilled() bool {
	// TODO: think about why this line being atomic doesn't matter
	return atomic.LoadInt64(&e.add) <= 0 && atomic.LoadInt64(&e.del) <= 0
}

// GetExpectations returns the add and del expectations of the controllee.
func (e *ControlleeExpectations) GetExpectations() (int64, int64) {
	return atomic.LoadInt64(&e.add), atomic.LoadInt64(&e.del)
}

// MarshalLog makes a thread-safe copy of the values of the expectations that
// can be used for logging.
func (e *ControlleeExpectations) MarshalLog() interface{} {
	return struct {
		add int64
		del int64
		key string
	}{
		add: atomic.LoadInt64(&e.add),
		del: atomic.LoadInt64(&e.del),
		key: e.key,
	}
}

// NewControllerExpectations returns a store for ControllerExpectations.
func NewControllerExpectations() *ControllerExpectations {
	return &ControllerExpectations{cache.NewStore(ExpKeyFunc)}
}

// UIDSetKeyFunc to parse out the key from a UIDSet.
var UIDSetKeyFunc = func(obj interface{}) (string, error) {
	if u, ok := obj.(*UIDSet); ok {
		return u.key, nil
	}
	return "", fmt.Errorf("could not find key for obj %#v", obj)
}

// UIDSet holds a key and a set of UIDs. Used by the
// UIDTrackingControllerExpectations to remember which UID it has seen/still
// waiting for.
type UIDSet struct {
	sets.String
	key string
}

// UIDTrackingControllerExpectations tracks the UID of the pods it deletes.
// This cache is needed over plain old expectations to safely handle graceful
// deletion. The desired behavior is to treat an update that sets the
// DeletionTimestamp on an object as a delete. To do so consistently, one needs
// to remember the expected deletes so they aren't double counted.
// TODO: Track creates as well (#22599)
type UIDTrackingControllerExpectations struct {
	ControllerExpectationsInterface
	// TODO: There is a much nicer way to do this that involves a single store,
	// a lock per entry, and a ControlleeExpectationsInterface type.
	uidStoreLock sync.Mutex
	// Store used for the UIDs associated with any expectation tracked via the
	// ControllerExpectationsInterface.
	uidStore cache.Store
}

// GetUIDs is a convenience method to avoid exposing the set of expected uids.
// The returned set is not thread safe, all modifications must be made holding
// the uidStoreLock.
func (u *UIDTrackingControllerExpectations) GetUIDs(controllerKey string) sets.String {
	if uid, exists, err := u.uidStore.GetByKey(controllerKey); err == nil && exists {
		return uid.(*UIDSet).String
	}
	return nil
}

// ExpectDeletions records expectations for the given deleteKeys, against the given controller.
func (u *UIDTrackingControllerExpectations) ExpectDeletions(logger klog.Logger, rcKey string, deletedKeys []string) error {
	expectedUIDs := sets.NewString()
	for _, k := range deletedKeys {
		expectedUIDs.Insert(k)
	}
	logger.V(4).Info("Controller waiting on deletions", "controller", rcKey, "keys", deletedKeys)
	u.uidStoreLock.Lock()
	defer u.uidStoreLock.Unlock()

	if existing := u.GetUIDs(rcKey); existing != nil && existing.Len() != 0 {
		logger.Error(nil, "Clobbering existing delete keys", "keys", existing)
	}
	if err := u.uidStore.Add(&UIDSet{expectedUIDs, rcKey}); err != nil {
		return err
	}
	return u.ControllerExpectationsInterface.ExpectDeletions(logger, rcKey, expectedUIDs.Len())
}

// DeletionObserved records the given deleteKey as a deletion, for the given rc.
func (u *UIDTrackingControllerExpectations) DeletionObserved(logger klog.Logger, rcKey, deleteKey string) {
	u.uidStoreLock.Lock()
	defer u.uidStoreLock.Unlock()

	uids := u.GetUIDs(rcKey)
	if uids != nil && uids.Has(deleteKey) {
		logger.V(4).Info("Controller received delete for pod", "controller", rcKey, "key", deleteKey)
		u.ControllerExpectationsInterface.DeletionObserved(logger, rcKey)
		uids.Delete(deleteKey)
	}
}

// DeleteExpectations deletes the UID set and invokes DeleteExpectations on the
// underlying ControllerExpectationsInterface.
func (u *UIDTrackingControllerExpectations) DeleteExpectations(logger klog.Logger, rcKey string) {
	u.uidStoreLock.Lock()
	defer u.uidStoreLock.Unlock()

	u.ControllerExpectationsInterface.DeleteExpectations(logger, rcKey)
	if uidExp, exists, err := u.uidStore.GetByKey(rcKey); err == nil && exists {
		if err := u.uidStore.Delete(uidExp); err != nil {
			logger.V(2).Info("Error deleting uid expectations", "controller", rcKey, "err", err)
		}
	}
}

// NewUIDTrackingControllerExpectations returns a wrapper around
// ControllerExpectations that is aware of deleteKeys.
func NewUIDTrackingControllerExpectations(ce ControllerExpectationsInterface) *UIDTrackingControllerExpectations {
	return &UIDTrackingControllerExpectations{ControllerExpectationsInterface: ce, uidStore: cache.NewStore(UIDSetKeyFunc)}
}

// Reasons for pod events
const (
	// FailedCreatePodReason is added in an event and in a replica set condition
	// when a pod for a replica set is failed to be created.
	FailedCreatePodReason = "FailedCreate"
	// SuccessfulCreatePodReason is added in an event when a pod for a replica set
	// is successfully created.
	SuccessfulCreatePodReason = "SuccessfulCreate"
	// FailedDeletePodReason is added in an event and in a replica set condition
	// when a pod for a replica set is failed to be deleted.
	FailedDeletePodReason = "FailedDelete"
	// SuccessfulDeletePodReason is added in an event when a pod for a replica set
	// is successfully deleted.
	SuccessfulDeletePodReason = "SuccessfulDelete"
)

// RSControlInterface is an interface that knows how to add or delete
// ReplicaSets, as well as increment or decrement them. It is used
// by the deployment controller to ease testing of actions that it takes.
type RSControlInterface interface {
	PatchReplicaSet(ctx context.Context, namespace, name string, data []byte) error
}

// RealRSControl is the default implementation of RSControllerInterface.
type RealRSControl struct {
	KubeClient clientset.Interface
	Recorder   record.EventRecorder
}

var _ RSControlInterface = &RealRSControl{}

func (r RealRSControl) PatchReplicaSet(ctx context.Context, namespace, name string, data []byte) error {
	_, err := r.KubeClient.AppsV1().ReplicaSets(namespace).Patch(ctx, name, types.StrategicMergePatchType, data, metav1.PatchOptions{})
	return err
}

// TODO: merge the controller revision interface in controller_history.go with this one
// ControllerRevisionControlInterface is an interface that knows how to patch
// ControllerRevisions, as well as increment or decrement them. It is used
// by the daemonset controller to ease testing of actions that it takes.
type ControllerRevisionControlInterface interface {
	PatchControllerRevision(ctx context.Context, namespace, name string, data []byte) error
}

// RealControllerRevisionControl is the default implementation of ControllerRevisionControlInterface.
type RealControllerRevisionControl struct {
	KubeClient clientset.Interface
}

var _ ControllerRevisionControlInterface = &RealControllerRevisionControl{}

func (r RealControllerRevisionControl) PatchControllerRevision(ctx context.Context, namespace, name string, data []byte) error {
	_, err := r.KubeClient.AppsV1().ControllerRevisions(namespace).Patch(ctx, name, types.StrategicMergePatchType, data, metav1.PatchOptions{})
	return err
}

// PodControlInterface is an interface that knows how to add or delete pods
// created as an interface to allow testing.
type PodControlInterface interface {
	// CreatePods creates new pods according to the spec, and sets object as the pod's controller.
	CreatePods(ctx context.Context, namespace string, template *v1.PodTemplateSpec, object runtime.Object, controllerRef *metav1.OwnerReference) error
	// CreatePodsWithGenerateName creates new pods according to the spec, sets object as the pod's controller and sets pod's generateName.
	CreatePodsWithGenerateName(ctx context.Context, namespace string, template *v1.PodTemplateSpec, object runtime.Object, controllerRef *metav1.OwnerReference, generateName string) error
	// DeletePod deletes the pod identified by podID.
	DeletePod(ctx context.Context, namespace string, podID string, object runtime.Object) error
	// PatchPod patches the pod.
	PatchPod(ctx context.Context, namespace, name string, data []byte) error
}

// RealPodControl is the default implementation of PodControlInterface.
type RealPodControl struct {
	KubeClient clientset.Interface
	Recorder   record.EventRecorder
}

var _ PodControlInterface = &RealPodControl{}

func getPodsLabelSet(template *v1.PodTemplateSpec) labels.Set {
	desiredLabels := make(labels.Set)
	for k, v := range template.Labels {
		desiredLabels[k] = v
	}
	return desiredLabels
}

func getPodsFinalizers(template *v1.PodTemplateSpec) []string {
	desiredFinalizers := make([]string, len(template.Finalizers))
	copy(desiredFinalizers, template.Finalizers)
	return desiredFinalizers
}

func getPodsAnnotationSet(template *v1.PodTemplateSpec) labels.Set {
	desiredAnnotations := make(labels.Set)
	for k, v := range template.Annotations {
		desiredAnnotations[k] = v
	}
	return desiredAnnotations
}

func getPodsPrefix(controllerName string) string {
	// use the dash (if the name isn't too long) to make the pod name a bit prettier
	prefix := fmt.Sprintf("%s-", controllerName)
	if len(validation.ValidatePodName(prefix, true)) != 0 {
		prefix = controllerName
	}
	return prefix
}

func validateControllerRef(controllerRef *metav1.OwnerReference) error {
	if controllerRef == nil {
		return fmt.Errorf("controllerRef is nil")
	}
	if len(controllerRef.APIVersion) == 0 {
		return fmt.Errorf("controllerRef has empty APIVersion")
	}
	if len(controllerRef.Kind) == 0 {
		return fmt.Errorf("controllerRef has empty Kind")
	}
	if controllerRef.Controller == nil || !*controllerRef.Controller {
		return fmt.Errorf("controllerRef.Controller is not set to true")
	}
	if controllerRef.BlockOwnerDeletion == nil || !*controllerRef.BlockOwnerDeletion {
		return fmt.Errorf("controllerRef.BlockOwnerDeletion is not set")
	}
	return nil
}

func (r RealPodControl) CreatePods(ctx context.Context, namespace string, template *v1.PodTemplateSpec, controllerObject runtime.Object, controllerRef *metav1.OwnerReference) error {
	return r.CreatePodsWithGenerateName(ctx, namespace, template, controllerObject, controllerRef, "")
}

func (r RealPodControl) CreatePodsWithGenerateName(ctx context.Context, namespace string, template *v1.PodTemplateSpec, controllerObject runtime.Object, controllerRef *metav1.OwnerReference, generateName string) error {
	if err := validateControllerRef(controllerRef); err != nil {
		return err
	}
	pod, err := GetPodFromTemplate(template, controllerObject, controllerRef)
	if err != nil {
		return err
	}
	if len(generateName) > 0 {
		pod.ObjectMeta.GenerateName = generateName
	}
	return r.createPods(ctx, namespace, pod, controllerObject)
}

func (r RealPodControl) PatchPod(ctx context.Context, namespace, name string, data []byte) error {
	_, err := r.KubeClient.CoreV1().Pods(namespace).Patch(ctx, name, types.StrategicMergePatchType, data, metav1.PatchOptions{})
	return err
}

func GetPodFromTemplate(template *v1.PodTemplateSpec, parentObject runtime.Object, controllerRef *metav1.OwnerReference) (*v1.Pod, error) {
	desiredLabels := getPodsLabelSet(template)
	desiredFinalizers := getPodsFinalizers(template)
	desiredAnnotations := getPodsAnnotationSet(template)
	accessor, err := meta.Accessor(parentObject)
	if err != nil {
		return nil, fmt.Errorf("parentObject does not have ObjectMeta, %v", err)
	}
	prefix := getPodsPrefix(accessor.GetName())

	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Labels:       desiredLabels,
			Annotations:  desiredAnnotations,
			GenerateName: prefix,
			Finalizers:   desiredFinalizers,
		},
	}
	if controllerRef != nil {
		pod.OwnerReferences = append(pod.OwnerReferences, *controllerRef)
	}
	pod.Spec = *template.Spec.DeepCopy()
	return pod, nil
}

func (r RealPodControl) createPods(ctx context.Context, namespace string, pod *v1.Pod, object runtime.Object) error {
	if len(labels.Set(pod.Labels)) == 0 {
		return fmt.Errorf("unable to create pods, no labels")
	}
	newPod, err := r.KubeClient.CoreV1().Pods(namespace).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		// only send an event if the namespace isn't terminating
		if !apierrors.HasStatusCause(err, v1.NamespaceTerminatingCause) {
			r.Recorder.Eventf(object, v1.EventTypeWarning, FailedCreatePodReason, "Error creating: %v", err)
		}
		return err
	}
	logger := klog.FromContext(ctx)
	accessor, err := meta.Accessor(object)
	if err != nil {
		logger.Error(err, "parentObject does not have ObjectMeta")
		return nil
	}
	logger.V(4).Info("Controller created pod", "controller", accessor.GetName(), "pod", klog.KObj(newPod))
	r.Recorder.Eventf(object, v1.EventTypeNormal, SuccessfulCreatePodReason, "Created pod: %v", newPod.Name)

	return nil
}

func (r RealPodControl) DeletePod(ctx context.Context, namespace string, podID string, object runtime.Object) error {
	accessor, err := meta.Accessor(object)
	if err != nil {
		return fmt.Errorf("object does not have ObjectMeta, %v", err)
	}
	logger := klog.FromContext(ctx)
	logger.V(2).Info("Deleting pod", "controller", accessor.GetName(), "pod", klog.KRef(namespace, podID))
	if err := r.KubeClient.CoreV1().Pods(namespace).Delete(ctx, podID, metav1.DeleteOptions{}); err != nil {
		if apierrors.IsNotFound(err) {
			logger.V(4).Info("Pod has already been deleted.", "pod", klog.KRef(namespace, podID))
			return err
		}
		r.Recorder.Eventf(object, v1.EventTypeWarning, FailedDeletePodReason, "Error deleting: %v", err)
		return fmt.Errorf("unable to delete pods: %v", err)
	}
	r.Recorder.Eventf(object, v1.EventTypeNormal, SuccessfulDeletePodReason, "Deleted pod: %v", podID)

	return nil
}

type FakePodControl struct {
	sync.Mutex
	Templates       []v1.PodTemplateSpec
	ControllerRefs  []metav1.OwnerReference
	DeletePodName   []string
	Patches         [][]byte
	Err             error
	CreateLimit     int
	CreateCallCount int
}

var _ PodControlInterface = &FakePodControl{}

func (f *FakePodControl) PatchPod(ctx context.Context, namespace, name string, data []byte) error {
	f.Lock()
	defer f.Unlock()
	f.Patches = append(f.Patches, data)
	if f.Err != nil {
		return f.Err
	}
	return nil
}

func (f *FakePodControl) CreatePods(ctx context.Context, namespace string, spec *v1.PodTemplateSpec, object runtime.Object, controllerRef *metav1.OwnerReference) error {
	return f.CreatePodsWithGenerateName(ctx, namespace, spec, object, controllerRef, "")
}

func (f *FakePodControl) CreatePodsWithGenerateName(ctx context.Context, namespace string, spec *v1.PodTemplateSpec, object runtime.Object, controllerRef *metav1.OwnerReference, generateNamePrefix string) error {
	f.Lock()
	defer f.Unlock()
	f.CreateCallCount++
	if f.CreateLimit != 0 && f.CreateCallCount > f.CreateLimit {
		return fmt.Errorf("not creating pod, limit %d already reached (create call %d)", f.CreateLimit, f.CreateCallCount)
	}
	spec.GenerateName = generateNamePrefix
	f.Templates = append(f.Templates, *spec)
	f.ControllerRefs = append(f.ControllerRefs, *controllerRef)
	if f.Err != nil {
		return f.Err
	}
	return nil
}

func (f *FakePodControl) DeletePod(ctx context.Context, namespace string, podID string, object runtime.Object) error {
	f.Lock()
	defer f.Unlock()
	f.DeletePodName = append(f.DeletePodName, podID)
	if f.Err != nil {
		return f.Err
	}
	return nil
}

func (f *FakePodControl) Clear() {
	f.Lock()
	defer f.Unlock()
	f.DeletePodName = []string{}
	f.Templates = []v1.PodTemplateSpec{}
	f.ControllerRefs = []metav1.OwnerReference{}
	f.Patches = [][]byte{}
	f.CreateLimit = 0
	f.CreateCallCount = 0
}

// ByLogging allows custom sorting of pods so the best one can be picked for getting its logs.
type ByLogging []*v1.Pod

func (s ByLogging) Len() int      { return len(s) }
func (s ByLogging) Swap(i, j int) { s[i], s[j] = s[j], s[i] }

func (s ByLogging) Less(i, j int) bool {
	// 1. assigned < unassigned
	if s[i].Spec.NodeName != s[j].Spec.NodeName && (len(s[i].Spec.NodeName) == 0 || len(s[j].Spec.NodeName) == 0) {
		return len(s[i].Spec.NodeName) > 0
	}
	// 2. PodRunning < PodUnknown < PodPending
	if s[i].Status.Phase != s[j].Status.Phase {
		return podPhaseToOrdinal[s[i].Status.Phase] > podPhaseToOrdinal[s[j].Status.Phase]
	}
	// 3. ready < not ready
	if podutil.IsPodReady(s[i]) != podutil.IsPodReady(s[j]) {
		return podutil.IsPodReady(s[i])
	}
	// TODO: take availability into account when we push minReadySeconds information from deployment into pods,
	//       see https://github.com/kubernetes/kubernetes/issues/22065
	// 4. Been ready for more time < less time < empty time
	if podutil.IsPodReady(s[i]) && podutil.IsPodReady(s[j]) {
		readyTime1 := podReadyTime(s[i])
		readyTime2 := podReadyTime(s[j])
		if !readyTime1.Equal(readyTime2) {
			return afterOrZero(readyTime2, readyTime1)
		}
	}
	// 5. Pods with containers with higher restart counts < lower restart counts
	if res := compareMaxContainerRestarts(s[i], s[j]); res != nil {
		return *res
	}
	// 6. older pods < newer pods < empty timestamp pods
	if !s[i].CreationTimestamp.Equal(&s[j].CreationTimestamp) {
		return afterOrZero(&s[j].CreationTimestamp, &s[i].CreationTimestamp)
	}
	return false
}

// ActivePods type allows custom sorting of pods so a controller can pick the best ones to delete.
type ActivePods []*v1.Pod

func (s ActivePods) Len() int      { return len(s) }
func (s ActivePods) Swap(i, j int) { s[i], s[j] = s[j], s[i] }

func (s ActivePods) Less(i, j int) bool {
	// 1. Unassigned < assigned
	// If only one of the pods is unassigned, the unassigned one is smaller
	if s[i].Spec.NodeName != s[j].Spec.NodeName && (len(s[i].Spec.NodeName) == 0 || len(s[j].Spec.NodeName) == 0) {
		return len(s[i].Spec.NodeName) == 0
	}
	// 2. PodPending < PodUnknown < PodRunning
	if podPhaseToOrdinal[s[i].Status.Phase] != podPhaseToOrdinal[s[j].Status.Phase] {
		return podPhaseToOrdinal[s[i].Status.Phase] < podPhaseToOrdinal[s[j].Status.Phase]
	}
	// 3. Not ready < ready
	// If only one of the pods is not ready, the not ready one is smaller
	if podutil.IsPodReady(s[i]) != podutil.IsPodReady(s[j]) {
		return !podutil.IsPodReady(s[i])
	}
	// TODO: take availability into account when we push minReadySeconds information from deployment into pods,
	//       see https://github.com/kubernetes/kubernetes/issues/22065
	// 4. Been ready for empty time < less time < more time
	// If both pods are ready, the latest ready one is smaller
	if podutil.IsPodReady(s[i]) && podutil.IsPodReady(s[j]) {
		readyTime1 := podReadyTime(s[i])
		readyTime2 := podReadyTime(s[j])
		if !readyTime1.Equal(readyTime2) {
			return afterOrZero(readyTime1, readyTime2)
		}
	}
	// 5. Pods with containers with higher restart counts < lower restart counts
	if res := compareMaxContainerRestarts(s[i], s[j]); res != nil {
		return *res
	}
	// 6. Empty creation time pods < newer pods < older pods
	if !s[i].CreationTimestamp.Equal(&s[j].CreationTimestamp) {
		return afterOrZero(&s[i].CreationTimestamp, &s[j].CreationTimestamp)
	}
	return false
}

// ActivePodsWithRanks is a sortable list of pods and a list of corresponding
// ranks which will be considered during sorting.  The two lists must have equal
// length.  After sorting, the pods will be ordered as follows, applying each
// rule in turn until one matches:
//
//  1. If only one of the pods is assigned to a node, the pod that is not
//     assigned comes before the pod that is.
//  2. If the pods' phases differ, a pending pod comes before a pod whose phase
//     is unknown, and a pod whose phase is unknown comes before a running pod.
//  3. If exactly one of the pods is ready, the pod that is not ready comes
//     before the ready pod.
//  4. If controller.kubernetes.io/pod-deletion-cost annotation is set, then
//     the pod with the lower value will come first.
//  5. If the pods' ranks differ, the pod with greater rank comes before the pod
//     with lower rank.
//  6. If both pods are ready but have not been ready for the same amount of
//     time, the pod that has been ready for a shorter amount of time comes
//     before the pod that has been ready for longer.
//  7. If one pod has a container that has restarted more than any container in
//     the other pod, the pod with the container with more restarts comes
//     before the other pod.
//  8. If the pods' creation times differ, the pod that was created more recently
//     comes before the older pod.
//
// In 6 and 8, times are compared in a logarithmic scale. This allows a level
// of randomness among equivalent Pods when sorting. If two pods have the same
// logarithmic rank, they are sorted by UUID to provide a pseudorandom order.
//
// If none of these rules matches, the second pod comes before the first pod.
//
// The intention of this ordering is to put pods that should be preferred for
// deletion first in the list.
type ActivePodsWithRanks struct {
	// Pods is a list of pods.
	Pods []*v1.Pod

	// Rank is a ranking of pods.  This ranking is used during sorting when
	// comparing two pods that are both scheduled, in the same phase, and
	// having the same ready status.
	Rank []int

	// Now is a reference timestamp for doing logarithmic timestamp comparisons.
	// If zero, comparison happens without scaling.
	Now metav1.Time
}

func (s ActivePodsWithRanks) Len() int {
	return len(s.Pods)
}

func (s ActivePodsWithRanks) Swap(i, j int) {
	s.Pods[i], s.Pods[j] = s.Pods[j], s.Pods[i]
	s.Rank[i], s.Rank[j] = s.Rank[j], s.Rank[i]
}

// Less compares two pods with corresponding ranks and returns true if the first
// one should be preferred for deletion.
func (s ActivePodsWithRanks) Less(i, j int) bool {
	// 1. Delete a Specific Pod 
	if s.Pods[i].Annotations[SpecifiedToDeleteAnnotation] != s.Pods[j].Annotations[SpecifiedToDeleteAnnotation] ||
		(s.Pods[i].Annotations[SpecifiedToDeleteAnnotation] == s.Pods[j].Annotations[SpecifiedToDeleteAnnotation] && s.Pods[j].Annotations[SpecifiedToDeleteAnnotation] == "true") {
		return s.Pods[i].Annotations[SpecifiedToDeleteAnnotation] == "true"
	}
	// 2. Unassigned < assigned
	// If only one of the pods is unassigned, the unassigned one is smaller
	if s.Pods[i].Spec.NodeName != s.Pods[j].Spec.NodeName && (len(s.Pods[i].Spec.NodeName) == 0 || len(s.Pods[j].Spec.NodeName) == 0) {
		return len(s.Pods[i].Spec.NodeName) == 0
	}
	// 3. PodPending < PodUnknown < PodRunning
	if podPhaseToOrdinal[s.Pods[i].Status.Phase] != podPhaseToOrdinal[s.Pods[j].Status.Phase] {
		return podPhaseToOrdinal[s.Pods[i].Status.Phase] < podPhaseToOrdinal[s.Pods[j].Status.Phase]
	}
	// 4. Not ready < ready
	// If only one of the pods is not ready, the not ready one is smaller
	if podutil.IsPodReady(s.Pods[i]) != podutil.IsPodReady(s.Pods[j]) {
		return !podutil.IsPodReady(s.Pods[i])
	}

	// 5. lower pod-deletion-cost < higher pod-deletion cost
	if utilfeature.DefaultFeatureGate.Enabled(features.PodDeletionCost) {
		pi, _ := helper.GetDeletionCostFromPodAnnotations(s.Pods[i].Annotations)
		pj, _ := helper.GetDeletionCostFromPodAnnotations(s.Pods[j].Annotations)
		if pi != pj {
			return pi < pj
		}
	}

	// 6. Doubled up < not doubled up
	// If one of the two pods is on the same node as one or more additional
	// ready pods that belong to the same replicaset, whichever pod has more
	// colocated ready pods is less
	if s.Rank[i] != s.Rank[j] {
		return s.Rank[i] > s.Rank[j]
	}
	// TODO: take availability into account when we push minReadySeconds information from deployment into pods,
	//       see https://github.com/kubernetes/kubernetes/issues/22065
	// 7. Been ready for empty time < less time < more time
	// If both pods are ready, the latest ready one is smaller
	if podutil.IsPodReady(s.Pods[i]) && podutil.IsPodReady(s.Pods[j]) {
		readyTime1 := podReadyTime(s.Pods[i])
		readyTime2 := podReadyTime(s.Pods[j])
		if !readyTime1.Equal(readyTime2) {
			if !utilfeature.DefaultFeatureGate.Enabled(features.LogarithmicScaleDown) {
				return afterOrZero(readyTime1, readyTime2)
			} else {
				if s.Now.IsZero() || readyTime1.IsZero() || readyTime2.IsZero() {
					return afterOrZero(readyTime1, readyTime2)
				}
				rankDiff := logarithmicRankDiff(*readyTime1, *readyTime2, s.Now)
				if rankDiff == 0 {
					return s.Pods[i].UID < s.Pods[j].UID
				}
				return rankDiff < 0
			}
		}
	}
	// 8. Pods with containers with higher restart counts < lower restart counts
	if res := compareMaxContainerRestarts(s.Pods[i], s.Pods[j]); res != nil {
		return *res
	}
	// 9. Empty creation time pods < newer pods < older pods
	if !s.Pods[i].CreationTimestamp.Equal(&s.Pods[j].CreationTimestamp) {
		if !utilfeature.DefaultFeatureGate.Enabled(features.LogarithmicScaleDown) {
			return afterOrZero(&s.Pods[i].CreationTimestamp, &s.Pods[j].CreationTimestamp)
		} else {
			if s.Now.IsZero() || s.Pods[i].CreationTimestamp.IsZero() || s.Pods[j].CreationTimestamp.IsZero() {
				return afterOrZero(&s.Pods[i].CreationTimestamp, &s.Pods[j].CreationTimestamp)
			}
			rankDiff := logarithmicRankDiff(s.Pods[i].CreationTimestamp, s.Pods[j].CreationTimestamp, s.Now)
			if rankDiff == 0 {
				return s.Pods[i].UID < s.Pods[j].UID
			}
			return rankDiff < 0
		}
	}
	return false
}

// afterOrZero checks if time t1 is after time t2; if one of them
// is zero, the zero time is seen as after non-zero time.
func afterOrZero(t1, t2 *metav1.Time) bool {
	if t1.Time.IsZero() || t2.Time.IsZero() {
		return t1.Time.IsZero()
	}
	return t1.After(t2.Time)
}

// logarithmicRankDiff calculates the base-2 logarithmic ranks of 2 timestamps,
// compared to the current timestamp
func logarithmicRankDiff(t1, t2, now metav1.Time) int64 {
	d1 := now.Sub(t1.Time)
	d2 := now.Sub(t2.Time)
	r1 := int64(-1)
	r2 := int64(-1)
	if d1 > 0 {
		r1 = int64(math.Log2(float64(d1)))
	}
	if d2 > 0 {
		r2 = int64(math.Log2(float64(d2)))
	}
	return r1 - r2
}

func podReadyTime(pod *v1.Pod) *metav1.Time {
	if podutil.IsPodReady(pod) {
		for _, c := range pod.Status.Conditions {
			// we only care about pod ready conditions
			if c.Type == v1.PodReady && c.Status == v1.ConditionTrue {
				return &c.LastTransitionTime
			}
		}
	}
	return &metav1.Time{}
}

func maxContainerRestarts(pod *v1.Pod) (regularRestarts, sidecarRestarts int) {
	for _, c := range pod.Status.ContainerStatuses {
		regularRestarts = max(regularRestarts, int(c.RestartCount))
	}
	names := sets.New[string]()
	for _, c := range pod.Spec.InitContainers {
		if c.RestartPolicy != nil && *c.RestartPolicy == v1.ContainerRestartPolicyAlways {
			names.Insert(c.Name)
		}
	}
	for _, c := range pod.Status.InitContainerStatuses {
		if names.Has(c.Name) {
			sidecarRestarts = max(sidecarRestarts, int(c.RestartCount))
		}
	}
	return
}

// We use *bool here to determine equality:
// true: pi has a higher container restart count.
// false: pj has a higher container restart count.
// nil: Both have the same container restart count.
func compareMaxContainerRestarts(pi *v1.Pod, pj *v1.Pod) *bool {
	regularRestartsI, sidecarRestartsI := maxContainerRestarts(pi)
	regularRestartsJ, sidecarRestartsJ := maxContainerRestarts(pj)
	if regularRestartsI != regularRestartsJ {
		res := regularRestartsI > regularRestartsJ
		return &res
	}
	// If pods have the same restart count, an attempt is made to compare the restart counts of sidecar containers.
	if sidecarRestartsI != sidecarRestartsJ {
		res := sidecarRestartsI > sidecarRestartsJ
		return &res
	}
	return nil
}

// FilterClaimedPods returns pods that are controlled by the controller and match the selector.
func FilterClaimedPods(controller metav1.Object, selector labels.Selector, pods []*v1.Pod) []*v1.Pod {
	var result []*v1.Pod
	for _, pod := range pods {
		if !metav1.IsControlledBy(pod, controller) {
			// It's an orphan or owned by someone else.
			continue
		}
		if selector.Matches(labels.Set(pod.Labels)) {
			result = append(result, pod)
		}
	}
	return result
}

// FilterActivePods returns pods that have not terminated.
func FilterActivePods(logger klog.Logger, pods []*v1.Pod) []*v1.Pod {
	var result []*v1.Pod
	for _, p := range pods {
		if IsPodActive(p) {
			result = append(result, p)
		}
	}
	return result
}

func FilterTerminatingPods(pods []*v1.Pod) []*v1.Pod {
	var result []*v1.Pod
	for _, p := range pods {
		if IsPodTerminating(p) {
			result = append(result, p)
		}
	}
	return result
}

func CountTerminatingPods(pods []*v1.Pod) int32 {
	numberOfTerminatingPods := 0
	for _, p := range pods {
		if IsPodTerminating(p) {
			numberOfTerminatingPods += 1
		}
	}
	return int32(numberOfTerminatingPods)
}

func IsPodActive(p *v1.Pod) bool {
	return v1.PodSucceeded != p.Status.Phase &&
		v1.PodFailed != p.Status.Phase &&
		p.DeletionTimestamp == nil
}

func IsPodTerminating(p *v1.Pod) bool {
	return !podutil.IsPodTerminal(p) &&
		p.DeletionTimestamp != nil
}

// FilterActiveReplicaSets returns replica sets that have (or at least ought to have) pods.
func FilterActiveReplicaSets(replicaSets []*apps.ReplicaSet) []*apps.ReplicaSet {
	activeFilter := func(rs *apps.ReplicaSet) bool {
		return rs != nil && *(rs.Spec.Replicas) > 0
	}
	return FilterReplicaSets(replicaSets, activeFilter)
}

type filterRS func(rs *apps.ReplicaSet) bool

// FilterReplicaSets returns replica sets that are filtered by filterFn (all returned ones should match filterFn).
func FilterReplicaSets(RSes []*apps.ReplicaSet, filterFn filterRS) []*apps.ReplicaSet {
	var filtered []*apps.ReplicaSet
	for i := range RSes {
		if filterFn(RSes[i]) {
			filtered = append(filtered, RSes[i])
		}
	}
	return filtered
}

// PodKey returns a key unique to the given pod within a cluster.
// It's used so we consistently use the same key scheme in this module.
// It does exactly what cache.MetaNamespaceKeyFunc would have done
// except there's not possibility for error since we know the exact type.
func PodKey(pod *v1.Pod) string {
	return fmt.Sprintf("%v/%v", pod.Namespace, pod.Name)
}

// ControllersByCreationTimestamp sorts a list of ReplicationControllers by creation timestamp, using their names as a tie breaker.
type ControllersByCreationTimestamp []*v1.ReplicationController

func (o ControllersByCreationTimestamp) Len() int      { return len(o) }
func (o ControllersByCreationTimestamp) Swap(i, j int) { o[i], o[j] = o[j], o[i] }
func (o ControllersByCreationTimestamp) Less(i, j int) bool {
	if o[i].CreationTimestamp.Equal(&o[j].CreationTimestamp) {
		return o[i].Name < o[j].Name
	}
	return o[i].CreationTimestamp.Before(&o[j].CreationTimestamp)
}

// ReplicaSetsByCreationTimestamp sorts a list of ReplicaSet by creation timestamp, using their names as a tie breaker.
type ReplicaSetsByCreationTimestamp []*apps.ReplicaSet

func (o ReplicaSetsByCreationTimestamp) Len() int      { return len(o) }
func (o ReplicaSetsByCreationTimestamp) Swap(i, j int) { o[i], o[j] = o[j], o[i] }
func (o ReplicaSetsByCreationTimestamp) Less(i, j int) bool {
	if o[i].CreationTimestamp.Equal(&o[j].CreationTimestamp) {
		return o[i].Name < o[j].Name
	}
	return o[i].CreationTimestamp.Before(&o[j].CreationTimestamp)
}

// ReplicaSetsBySizeOlder sorts a list of ReplicaSet by size in descending order, using their creation timestamp or name as a tie breaker.
// By using the creation timestamp, this sorts from old to new replica sets.
type ReplicaSetsBySizeOlder []*apps.ReplicaSet

func (o ReplicaSetsBySizeOlder) Len() int      { return len(o) }
func (o ReplicaSetsBySizeOlder) Swap(i, j int) { o[i], o[j] = o[j], o[i] }
func (o ReplicaSetsBySizeOlder) Less(i, j int) bool {
	if *(o[i].Spec.Replicas) == *(o[j].Spec.Replicas) {
		return ReplicaSetsByCreationTimestamp(o).Less(i, j)
	}
	return *(o[i].Spec.Replicas) > *(o[j].Spec.Replicas)
}

// ReplicaSetsBySizeNewer sorts a list of ReplicaSet by size in descending order, using their creation timestamp or name as a tie breaker.
// By using the creation timestamp, this sorts from new to old replica sets.
type ReplicaSetsBySizeNewer []*apps.ReplicaSet

func (o ReplicaSetsBySizeNewer) Len() int      { return len(o) }
func (o ReplicaSetsBySizeNewer) Swap(i, j int) { o[i], o[j] = o[j], o[i] }
func (o ReplicaSetsBySizeNewer) Less(i, j int) bool {
	if *(o[i].Spec.Replicas) == *(o[j].Spec.Replicas) {
		return ReplicaSetsByCreationTimestamp(o).Less(j, i)
	}
	return *(o[i].Spec.Replicas) > *(o[j].Spec.Replicas)
}

// AddOrUpdateTaintOnNode add taints to the node. If taint was added into node, it'll issue API calls
// to update nodes; otherwise, no API calls. Return error if any.
func AddOrUpdateTaintOnNode(ctx context.Context, c clientset.Interface, nodeName string, taints ...*v1.Taint) error {
	if len(taints) == 0 {
		return nil
	}
	firstTry := true
	return clientretry.RetryOnConflict(UpdateTaintBackoff, func() error {
		var err error
		var oldNode *v1.Node
		// First we try getting node from the API server cache, as it's cheaper. If it fails
		// we get it from etcd to be sure to have fresh data.
		option := metav1.GetOptions{}
		if firstTry {
			option.ResourceVersion = "0"
			firstTry = false
		}
		oldNode, err = c.CoreV1().Nodes().Get(ctx, nodeName, option)
		if err != nil {
			return err
		}

		var newNode *v1.Node
		oldNodeCopy := oldNode
		updated := false
		for _, taint := range taints {
			curNewNode, ok, err := taintutils.AddOrUpdateTaint(oldNodeCopy, taint)
			if err != nil {
				return fmt.Errorf("failed to update taint of node")
			}
			updated = updated || ok
			newNode = curNewNode
			oldNodeCopy = curNewNode
		}
		if !updated {
			return nil
		}
		return PatchNodeTaints(ctx, c, nodeName, oldNode, newNode)
	})
}

// RemoveTaintOffNode is for cleaning up taints temporarily added to node,
// won't fail if target taint doesn't exist or has been removed.
// If passed a node it'll check if there's anything to be done, if taint is not present it won't issue
// any API calls.
func RemoveTaintOffNode(ctx context.Context, c clientset.Interface, nodeName string, node *v1.Node, taints ...*v1.Taint) error {
	if len(taints) == 0 {
		return nil
	}
	// Short circuit for limiting amount of API calls.
	if node != nil {
		match := false
		for _, taint := range taints {
			if taintutils.TaintExists(node.Spec.Taints, taint) {
				match = true
				break
			}
		}
		if !match {
			return nil
		}
	}

	firstTry := true
	return clientretry.RetryOnConflict(UpdateTaintBackoff, func() error {
		var err error
		var oldNode *v1.Node
		// First we try getting node from the API server cache, as it's cheaper. If it fails
		// we get it from etcd to be sure to have fresh data.
		option := metav1.GetOptions{}
		if firstTry {
			option.ResourceVersion = "0"
			firstTry = false
		}
		oldNode, err = c.CoreV1().Nodes().Get(ctx, nodeName, option)
		if err != nil {
			return err
		}

		var newNode *v1.Node
		oldNodeCopy := oldNode
		updated := false
		for _, taint := range taints {
			curNewNode, ok, err := taintutils.RemoveTaint(oldNodeCopy, taint)
			if err != nil {
				return fmt.Errorf("failed to remove taint of node")
			}
			updated = updated || ok
			newNode = curNewNode
			oldNodeCopy = curNewNode
		}
		if !updated {
			return nil
		}
		return PatchNodeTaints(ctx, c, nodeName, oldNode, newNode)
	})
}

// PatchNodeTaints patches node's taints.
func PatchNodeTaints(ctx context.Context, c clientset.Interface, nodeName string, oldNode *v1.Node, newNode *v1.Node) error {
	// Strip base diff node from RV to ensure that our Patch request will set RV to check for conflicts over .spec.taints.
	// This is needed because .spec.taints does not specify patchMergeKey and patchStrategy and adding them is no longer an option for compatibility reasons.
	// Using other Patch strategy works for adding new taints, however will not resolve problem with taint removal.
	oldNodeNoRV := oldNode.DeepCopy()
	oldNodeNoRV.ResourceVersion = ""
	oldDataNoRV, err := json.Marshal(&oldNodeNoRV)
	if err != nil {
		return fmt.Errorf("failed to marshal old node %#v for node %q: %v", oldNodeNoRV, nodeName, err)
	}

	newTaints := newNode.Spec.Taints
	newNodeClone := oldNode.DeepCopy()
	newNodeClone.Spec.Taints = newTaints
	newData, err := json.Marshal(newNodeClone)
	if err != nil {
		return fmt.Errorf("failed to marshal new node %#v for node %q: %v", newNodeClone, nodeName, err)
	}

	patchBytes, err := strategicpatch.CreateTwoWayMergePatch(oldDataNoRV, newData, v1.Node{})
	if err != nil {
		return fmt.Errorf("failed to create patch for node %q: %v", nodeName, err)
	}

	_, err = c.CoreV1().Nodes().Patch(ctx, nodeName, types.StrategicMergePatchType, patchBytes, metav1.PatchOptions{})
	return err
}

// ComputeHash returns a hash value calculated from pod template and
// a collisionCount to avoid hash collision. The hash will be safe encoded to
// avoid bad words.
func ComputeHash(template *v1.PodTemplateSpec, collisionCount *int32) string {
	podTemplateSpecHasher := fnv.New32a()
	hashutil.DeepHashObject(podTemplateSpecHasher, *template)

	// Add collisionCount in the hash if it exists.
	if collisionCount != nil {
		collisionCountBytes := make([]byte, 8)
		binary.LittleEndian.PutUint32(collisionCountBytes, uint32(*collisionCount))
		podTemplateSpecHasher.Write(collisionCountBytes)
	}

	return rand.SafeEncodeString(fmt.Sprint(podTemplateSpecHasher.Sum32()))
}

func AddOrUpdateLabelsOnNode(kubeClient clientset.Interface, nodeName string, labelsToUpdate map[string]string) error {
	firstTry := true
	return clientretry.RetryOnConflict(UpdateLabelBackoff, func() error {
		var err error
		var node *v1.Node
		// First we try getting node from the API server cache, as it's cheaper. If it fails
		// we get it from etcd to be sure to have fresh data.
		option := metav1.GetOptions{}
		if firstTry {
			option.ResourceVersion = "0"
			firstTry = false
		}
		node, err = kubeClient.CoreV1().Nodes().Get(context.TODO(), nodeName, option)
		if err != nil {
			return err
		}

		// Make a copy of the node and update the labels.
		newNode := node.DeepCopy()
		if newNode.Labels == nil {
			newNode.Labels = make(map[string]string)
		}
		for key, value := range labelsToUpdate {
			newNode.Labels[key] = value
		}

		oldData, err := json.Marshal(node)
		if err != nil {
			return fmt.Errorf("failed to marshal the existing node %#v: %v", node, err)
		}
		newData, err := json.Marshal(newNode)
		if err != nil {
			return fmt.Errorf("failed to marshal the new node %#v: %v", newNode, err)
		}
		patchBytes, err := strategicpatch.CreateTwoWayMergePatch(oldData, newData, &v1.Node{})
		if err != nil {
			return fmt.Errorf("failed to create a two-way merge patch: %v", err)
		}
		if _, err := kubeClient.CoreV1().Nodes().Patch(context.TODO(), node.Name, types.StrategicMergePatchType, patchBytes, metav1.PatchOptions{}); err != nil {
			return fmt.Errorf("failed to patch the node: %v", err)
		}
		return nil
	})
}
