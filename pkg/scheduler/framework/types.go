/*
Copyright 2015 The Kubernetes Authors.

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

package framework

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/sets"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/klog/v2"

	"k8s.io/apimachinery/pkg/api/resource"
	resourcehelper "k8s.io/component-helpers/resource"
	"k8s.io/kubernetes/pkg/features"
	schedutil "k8s.io/kubernetes/pkg/scheduler/util"
)

var generation int64

// ActionType is an integer to represent one type of resource change.
// Different ActionTypes can be bit-wised to compose new semantics.
type ActionType int64

// Constants for ActionTypes.
// CAUTION for contributors: When you add a new ActionType, you must update the following:
// - The list of basic, podOnly, and nodeOnly.
// - String() method.
const (
	Add ActionType = 1 << iota
	Delete

	// UpdateNodeXYZ is only applicable for Node events.
	// If you use UpdateNodeXYZ,
	// your plugin's QueueingHint is only executed for the specific sub-Update event.
	// It's better to narrow down the scope of the event by using them instead of just using Update event
	// for better performance in requeueing.
	UpdateNodeAllocatable
	UpdateNodeLabel
	// UpdateNodeTaint is an update for node's taints or node.Spec.Unschedulable.
	UpdateNodeTaint
	UpdateNodeCondition
	UpdateNodeAnnotation

	// UpdatePodXYZ is only applicable for Pod events.
	// If you use UpdatePodXYZ,
	// your plugin's QueueingHint is only executed for the specific sub-Update event.
	// It's better to narrow down the scope of the event by using them instead of Update event
	// for better performance in requeueing.
	UpdatePodLabel
	// UpdatePodScaleDown is an update for pod's scale down (i.e., any resource request is reduced).
	UpdatePodScaleDown
	// UpdatePodToleration is an addition for pod's tolerations.
	// (Due to API validation, we can add, but cannot modify or remove tolerations.)
	UpdatePodToleration
	// UpdatePodSchedulingGatesEliminated is an update for pod's scheduling gates, which eliminates all scheduling gates in the Pod.
	UpdatePodSchedulingGatesEliminated
	// UpdatePodGeneratedResourceClaim is an update of the list of ResourceClaims generated for the pod.
	// Depends on the DynamicResourceAllocation feature gate.
	UpdatePodGeneratedResourceClaim

	// updatePodOther is a update for pod's other fields.
	// It's used only for the internal event handling, and thus unexported.
	updatePodOther

	All ActionType = 1<<iota - 1

	// Use the general Update type if you don't either know or care the specific sub-Update type to use.
	Update = UpdateNodeAllocatable | UpdateNodeLabel | UpdateNodeTaint | UpdateNodeCondition | UpdateNodeAnnotation | UpdatePodLabel | UpdatePodScaleDown | UpdatePodToleration | UpdatePodSchedulingGatesEliminated | UpdatePodGeneratedResourceClaim | updatePodOther
	// none is a special ActionType that is only used internally.
	none ActionType = 0
)

var (
	// basicActionTypes is a list of basicActionTypes ActionTypes.
	basicActionTypes = []ActionType{Add, Delete, Update}
	// podActionTypes is a list of ActionTypes that are only applicable for Pod events.
	podActionTypes = []ActionType{UpdatePodLabel, UpdatePodScaleDown, UpdatePodToleration, UpdatePodSchedulingGatesEliminated, UpdatePodGeneratedResourceClaim}
	// nodeActionTypes is a list of ActionTypes that are only applicable for Node events.
	nodeActionTypes = []ActionType{UpdateNodeAllocatable, UpdateNodeLabel, UpdateNodeTaint, UpdateNodeCondition, UpdateNodeAnnotation}
)

func (a ActionType) String() string {
	switch a {
	case Add:
		return "Add"
	case Delete:
		return "Delete"
	case UpdateNodeAllocatable:
		return "UpdateNodeAllocatable"
	case UpdateNodeLabel:
		return "UpdateNodeLabel"
	case UpdateNodeTaint:
		return "UpdateNodeTaint"
	case UpdateNodeCondition:
		return "UpdateNodeCondition"
	case UpdateNodeAnnotation:
		return "UpdateNodeAnnotation"
	case UpdatePodLabel:
		return "UpdatePodLabel"
	case UpdatePodScaleDown:
		return "UpdatePodScaleDown"
	case UpdatePodToleration:
		return "UpdatePodToleration"
	case UpdatePodSchedulingGatesEliminated:
		return "UpdatePodSchedulingGatesEliminated"
	case UpdatePodGeneratedResourceClaim:
		return "UpdatePodGeneratedResourceClaim"
	case updatePodOther:
		return "Update"
	case All:
		return "All"
	case Update:
		return "Update"
	}

	// Shouldn't reach here.
	return ""
}

// EventResource is basically short for group/version/kind, which can uniquely represent a particular API resource.
type EventResource string

// Constants for GVKs.
//
// CAUTION for contributors: When you add a new EventResource, you must register a new one to allResources.
//
// Note:
// - UpdatePodXYZ or UpdateNodeXYZ: triggered by updating particular parts of a Pod or a Node, e.g. updatePodLabel.
// Use specific events rather than general ones (updatePodLabel vs update) can make the requeueing process more efficient
// and consume less memory as less events will be cached at scheduler.
const (
	// There are a couple of notes about how the scheduler notifies the events of Pods:
	// - Add: add events could be triggered by either a newly created Pod or an existing Pod that is scheduled to a Node.
	// - Delete: delete events could be triggered by:
	//           - a Pod that is deleted
	//           - a Pod that was assumed, but gets un-assumed due to some errors in the binding cycle.
	//           - an existing Pod that was unscheduled but gets scheduled to a Node.
	//
	// Note that the Pod event type includes the events for the unscheduled Pod itself.
	// i.e., when unscheduled Pods are updated, the scheduling queue checks with Pod/Update QueueingHint(s) whether the update may make the pods schedulable,
	// and requeues them to activeQ/backoffQ when at least one QueueingHint(s) return Queue.
	// Plugins **have to** implement a QueueingHint for Pod/Update event
	// if the rejection from them could be resolved by updating unscheduled Pods themselves.
	// Example: Pods that require excessive resources may be rejected by the noderesources plugin,
	// if this unscheduled pod is updated to require fewer resources,
	// the previous rejection from noderesources plugin can be resolved.
	// this plugin would implement QueueingHint for Pod/Update event
	// that returns Queue when such label changes are made in unscheduled Pods.
	Pod EventResource = "Pod"

	// These assignedPod and unschedulablePod are internal resources that are used to represent the type of Pod.
	// We don't expose them to the plugins deliberately because we don't publish Pod events with unschedulable Pods in the first place.
	assignedPod      EventResource = "AssignedPod"
	unschedulablePod EventResource = "UnschedulablePod"

	// A note about NodeAdd event and UpdateNodeTaint event:
	// When QHint is disabled, NodeAdd often isn't worked expectedly because of the internal feature called preCheck.
	// It's definitely not something expected for plugin developers,
	// and registering UpdateNodeTaint event is the only mitigation for now.
	// So, kube-scheduler registers UpdateNodeTaint event for plugins that has NodeAdded event, but don't have UpdateNodeTaint event.
	// It has a bad impact for the requeuing efficiency though, a lot better than some Pods being stuck in the
	// unschedulable pod pool.
	// This problematic preCheck feature is disabled when QHint is enabled,
	// and eventually will be removed along with QHint graduation.
	// See: https://github.com/kubernetes/kubernetes/issues/110175
	Node                  EventResource = "Node"
	PersistentVolume      EventResource = "PersistentVolume"
	PersistentVolumeClaim EventResource = "PersistentVolumeClaim"
	CSINode               EventResource = "storage.k8s.io/CSINode"
	CSIDriver             EventResource = "storage.k8s.io/CSIDriver"
	VolumeAttachment      EventResource = "storage.k8s.io/VolumeAttachment"
	CSIStorageCapacity    EventResource = "storage.k8s.io/CSIStorageCapacity"
	StorageClass          EventResource = "storage.k8s.io/StorageClass"
	ResourceClaim         EventResource = "resource.k8s.io/ResourceClaim"
	ResourceSlice         EventResource = "resource.k8s.io/ResourceSlice"
	DeviceClass           EventResource = "resource.k8s.io/DeviceClass"

	// WildCard is a special EventResource to match all resources.
	// e.g., If you register `{Resource: "*", ActionType: All}` in EventsToRegister,
	// all coming clusterEvents will be admitted. Be careful to register it, it will
	// increase the computing pressure in requeueing unless you really need it.
	//
	// Meanwhile, if the coming clusterEvent is a wildcard one, all pods
	// will be moved from unschedulablePod pool to activeQ/backoffQ forcibly.
	WildCard EventResource = "*"
)

var (
	// allResources is a list of all resources.
	allResources = []EventResource{
		Pod,
		assignedPod,
		unschedulablePod,
		Node,
		PersistentVolume,
		PersistentVolumeClaim,
		CSINode,
		CSIDriver,
		CSIStorageCapacity,
		StorageClass,
		VolumeAttachment,
		ResourceClaim,
		ResourceSlice,
		DeviceClass,
	}
)

type ClusterEventWithHint struct {
	Event ClusterEvent
	// QueueingHintFn is executed for the Pod rejected by this plugin when the above Event happens,
	// and filters out events to reduce useless retry of Pod's scheduling.
	// It's an optional field. If not set,
	// the scheduling of Pods will be always retried with backoff when this Event happens.
	// (the same as Queue)
	QueueingHintFn QueueingHintFn
}

// QueueingHintFn returns a hint that signals whether the event can make a Pod,
// which was rejected by this plugin in the past scheduling cycle, schedulable or not.
// It's called before a Pod gets moved from unschedulableQ to backoffQ or activeQ.
// If it returns an error, we'll take the returned QueueingHint as `Queue` at the caller whatever we returned here so that
// we can prevent the Pod from being stuck in the unschedulable pod pool.
//
// - `pod`: the Pod to be enqueued, which is rejected by this plugin in the past.
// - `oldObj` `newObj`: the object involved in that event.
//   - For example, the given event is "Node deleted", the `oldObj` will be that deleted Node.
//   - `oldObj` is nil if the event is add event.
//   - `newObj` is nil if the event is delete event.
type QueueingHintFn func(logger klog.Logger, pod *v1.Pod, oldObj, newObj interface{}) (QueueingHint, error)

type QueueingHint int

const (
	// QueueSkip implies that the cluster event has no impact on
	// scheduling of the pod.
	QueueSkip QueueingHint = iota

	// Queue implies that the Pod may be schedulable by the event.
	Queue
)

func (s QueueingHint) String() string {
	switch s {
	case QueueSkip:
		return "QueueSkip"
	case Queue:
		return "Queue"
	}
	return ""
}

// ClusterEvent abstracts how a system resource's state gets changed.
// Resource represents the standard API resources such as Pod, Node, etc.
// ActionType denotes the specific change such as Add, Update or Delete.
type ClusterEvent struct {
	Resource   EventResource
	ActionType ActionType

	// label describes this cluster event.
	// It's an optional field to control String(), which is used in logging and metrics.
	// Normally, it's not necessary to set this field; only used for special events like UnschedulableTimeout.
	label string
}

// Label is used for logging and metrics.
func (ce ClusterEvent) Label() string {
	if ce.label != "" {
		return ce.label
	}

	return fmt.Sprintf("%v%v", ce.Resource, ce.ActionType)
}

// AllClusterEventLabels returns all possible cluster event labels given to the metrics.
func AllClusterEventLabels() []string {
	labels := []string{UnschedulableTimeout, ForceActivate}
	for _, r := range allResources {
		for _, a := range basicActionTypes {
			labels = append(labels, ClusterEvent{Resource: r, ActionType: a}.Label())
		}
		if r == Pod {
			for _, a := range podActionTypes {
				labels = append(labels, ClusterEvent{Resource: r, ActionType: a}.Label())
			}
		} else if r == Node {
			for _, a := range nodeActionTypes {
				labels = append(labels, ClusterEvent{Resource: r, ActionType: a}.Label())
			}
		}
	}
	return labels
}

// IsWildCard returns true if ClusterEvent follows WildCard semantics
func (ce ClusterEvent) IsWildCard() bool {
	return ce.Resource == WildCard && ce.ActionType == All
}

// Match returns true if ClusterEvent is matched with the coming event.
// If the ce.Resource is "*", there's no requirement for the coming event' Resource.
// Contrarily, if the coming event's Resource is "*", the ce.Resource should only be "*".
//
// Note: we have a special case here when the coming event is a wildcard event,
// it will force all Pods to move to activeQ/backoffQ,
// but we take it as an unmatched event unless the ce is also a wildcard one.
func (ce ClusterEvent) Match(incomingEvent ClusterEvent) bool {
	return ce.IsWildCard() || ce.Resource.match(incomingEvent.Resource) && ce.ActionType&incomingEvent.ActionType != 0
}

// match returns true if the resource is matched with the coming resource.
func (r EventResource) match(resource EventResource) bool {
	// WildCard matches all resources
	return r == WildCard ||
		// Exact match
		r == resource ||
		// Pod matches assignedPod and unscheduledPod.
		// (assignedPod and unscheduledPod aren't exposed and hence only used for incoming events and never used in EventsToRegister)
		r == Pod && (resource == assignedPod || resource == unschedulablePod)
}

func UnrollWildCardResource() []ClusterEventWithHint {
	return []ClusterEventWithHint{
		{Event: ClusterEvent{Resource: Pod, ActionType: All}},
		{Event: ClusterEvent{Resource: Node, ActionType: All}},
		{Event: ClusterEvent{Resource: PersistentVolume, ActionType: All}},
		{Event: ClusterEvent{Resource: PersistentVolumeClaim, ActionType: All}},
		{Event: ClusterEvent{Resource: CSINode, ActionType: All}},
		{Event: ClusterEvent{Resource: CSIDriver, ActionType: All}},
		{Event: ClusterEvent{Resource: CSIStorageCapacity, ActionType: All}},
		{Event: ClusterEvent{Resource: StorageClass, ActionType: All}},
		{Event: ClusterEvent{Resource: ResourceClaim, ActionType: All}},
		{Event: ClusterEvent{Resource: DeviceClass, ActionType: All}},
	}
}

// QueuedPodInfo is a Pod wrapper with additional information related to
// the pod's status in the scheduling queue, such as the timestamp when
// it's added to the queue.
type QueuedPodInfo struct {
	*PodInfo
	// The time pod added to the scheduling queue.
	Timestamp time.Time
	// Number of all schedule attempts before successfully scheduled.
	// It's used to record the # attempts metric.
	Attempts int
	// The total number of the scheduling attempts that this Pod gets unschedulable.
	// Basically it equals Attempts, but when the Pod fails with the Error status (e.g., the network error),
	// this count won't be incremented.
	// It's used to calculate the backoff time this Pod is obliged to get before retrying.
	UnschedulableCount int
	// The number of the error status that this Pod gets sequentially.
	// This count is reset when the Pod gets another status than Error.
	//
	// If the error status is returned (e.g., kube-apiserver is unstable), we don't want to immediately retry the Pod and hence need a backoff retry mechanism
	// because that might push more burden to the kube-apiserver.
	// But, we don't want to calculate the backoff time in the same way as the normal unschedulable reason
	// since the purpose is different; the backoff for a unschedulable status etc is for the punishment of wasting the scheduling cycles,
	// whereas the backoff for the error status is for the protection of the kube-apiserver.
	// That's why we need to distinguish ConsecutiveErrorsCount for the error status and UnschedulableCount for the unschedulable status.
	// See https://github.com/kubernetes/kubernetes/issues/128744 for the discussion.
	ConsecutiveErrorsCount int
	// The time when the pod is added to the queue for the first time. The pod may be added
	// back to the queue multiple times before it's successfully scheduled.
	// It shouldn't be updated once initialized. It's used to record the e2e scheduling
	// latency for a pod.
	InitialAttemptTimestamp *time.Time
	// UnschedulablePlugins records the plugin names that the Pod failed with Unschedulable or UnschedulableAndUnresolvable status
	// at specific extension points: PreFilter, Filter, Reserve, Permit (WaitOnPermit), or PreBind.
	// If Pods are rejected at other extension points,
	// they're assumed to be unexpected errors (e.g., temporal network issue, plugin implementation issue, etc)
	// and retried soon after a backoff period.
	// That is because such failures could be solved regardless of incoming cluster events (registered in EventsToRegister).
	UnschedulablePlugins sets.Set[string]
	// PendingPlugins records the plugin names that the Pod failed with Pending status.
	PendingPlugins sets.Set[string]
	// Whether the Pod is scheduling gated (by PreEnqueuePlugins) or not.
	Gated bool
}

// DeepCopy returns a deep copy of the QueuedPodInfo object.
func (pqi *QueuedPodInfo) DeepCopy() *QueuedPodInfo {
	return &QueuedPodInfo{
		PodInfo:                 pqi.PodInfo.DeepCopy(),
		Timestamp:               pqi.Timestamp,
		Attempts:                pqi.Attempts,
		UnschedulableCount:      pqi.UnschedulableCount,
		InitialAttemptTimestamp: pqi.InitialAttemptTimestamp,
		UnschedulablePlugins:    pqi.UnschedulablePlugins.Clone(),
		PendingPlugins:          pqi.PendingPlugins.Clone(),
		Gated:                   pqi.Gated,
		ConsecutiveErrorsCount:  pqi.ConsecutiveErrorsCount,
	}
}

// podResource contains the result of calculateResource and is used only internally.
type podResource struct {
	resource Resource
	non0CPU  int64
	non0Mem  int64
}

// PodInfo is a wrapper to a Pod with additional pre-computed information to
// accelerate processing. This information is typically immutable (e.g., pre-processed
// inter-pod affinity selectors).
type PodInfo struct {
	Pod                        *v1.Pod
	RequiredAffinityTerms      []AffinityTerm
	RequiredAntiAffinityTerms  []AffinityTerm
	PreferredAffinityTerms     []WeightedAffinityTerm
	PreferredAntiAffinityTerms []WeightedAffinityTerm
	// cachedResource contains precomputed resources for Pod (podResource).
	// The value can change only if InPlacePodVerticalScaling is enabled.
	// In that case, the whole PodInfo object is recreated (for assigned pods in cache).
	// cachedResource contains a podResource, computed when adding a scheduled pod to NodeInfo.
	// When removing a pod from a NodeInfo, i.e. finding victims for preemption or removing a pod from a cluster,
	// cachedResource is used instead, what provides a noticeable performance boost.
	// Note: cachedResource field shouldn't be accessed directly.
	// Use calculateResource method to obtain it instead.
	cachedResource *podResource
}

// DeepCopy returns a deep copy of the PodInfo object.
func (pi *PodInfo) DeepCopy() *PodInfo {
	return &PodInfo{
		Pod:                        pi.Pod.DeepCopy(),
		RequiredAffinityTerms:      pi.RequiredAffinityTerms,
		RequiredAntiAffinityTerms:  pi.RequiredAntiAffinityTerms,
		PreferredAffinityTerms:     pi.PreferredAffinityTerms,
		PreferredAntiAffinityTerms: pi.PreferredAntiAffinityTerms,
		cachedResource:             pi.cachedResource,
	}
}

// Update creates a full new PodInfo by default. And only updates the pod when the PodInfo
// has been instantiated and the passed pod is the exact same one as the original pod.
func (pi *PodInfo) Update(pod *v1.Pod) error {
	if pod != nil && pi.Pod != nil && pi.Pod.UID == pod.UID {
		// PodInfo includes immutable information, and so it is safe to update the pod in place if it is
		// the exact same pod
		pi.Pod = pod
		return nil
	}
	var preferredAffinityTerms []v1.WeightedPodAffinityTerm
	var preferredAntiAffinityTerms []v1.WeightedPodAffinityTerm
	if affinity := pod.Spec.Affinity; affinity != nil {
		if a := affinity.PodAffinity; a != nil {
			preferredAffinityTerms = a.PreferredDuringSchedulingIgnoredDuringExecution
		}
		if a := affinity.PodAntiAffinity; a != nil {
			preferredAntiAffinityTerms = a.PreferredDuringSchedulingIgnoredDuringExecution
		}
	}

	// Attempt to parse the affinity terms
	var parseErrs []error
	requiredAffinityTerms, err := GetAffinityTerms(pod, GetPodAffinityTerms(pod.Spec.Affinity))
	if err != nil {
		parseErrs = append(parseErrs, fmt.Errorf("requiredAffinityTerms: %w", err))
	}
	requiredAntiAffinityTerms, err := GetAffinityTerms(pod,
		GetPodAntiAffinityTerms(pod.Spec.Affinity))
	if err != nil {
		parseErrs = append(parseErrs, fmt.Errorf("requiredAntiAffinityTerms: %w", err))
	}
	weightedAffinityTerms, err := getWeightedAffinityTerms(pod, preferredAffinityTerms)
	if err != nil {
		parseErrs = append(parseErrs, fmt.Errorf("preferredAffinityTerms: %w", err))
	}
	weightedAntiAffinityTerms, err := getWeightedAffinityTerms(pod, preferredAntiAffinityTerms)
	if err != nil {
		parseErrs = append(parseErrs, fmt.Errorf("preferredAntiAffinityTerms: %w", err))
	}

	pi.Pod = pod
	pi.RequiredAffinityTerms = requiredAffinityTerms
	pi.RequiredAntiAffinityTerms = requiredAntiAffinityTerms
	pi.PreferredAffinityTerms = weightedAffinityTerms
	pi.PreferredAntiAffinityTerms = weightedAntiAffinityTerms
	pi.cachedResource = nil
	return utilerrors.NewAggregate(parseErrs)
}

// AffinityTerm is a processed version of v1.PodAffinityTerm.
type AffinityTerm struct {
	Namespaces        sets.Set[string]
	Selector          labels.Selector
	TopologyKey       string
	NamespaceSelector labels.Selector
}

// Matches returns true if the pod matches the label selector and namespaces or namespace selector.
func (at *AffinityTerm) Matches(pod *v1.Pod, nsLabels labels.Set) bool {
	if at.Namespaces.Has(pod.Namespace) || at.NamespaceSelector.Matches(nsLabels) {
		return at.Selector.Matches(labels.Set(pod.Labels))
	}
	return false
}

// WeightedAffinityTerm is a "processed" representation of v1.WeightedAffinityTerm.
type WeightedAffinityTerm struct {
	AffinityTerm
	Weight int32
}

// ExtenderName is a fake plugin name put in UnschedulablePlugins when Extender rejected some Nodes.
const ExtenderName = "Extender"

// Diagnosis records the details to diagnose a scheduling failure.
type Diagnosis struct {
	// NodeToStatus records the status of nodes and generic status for absent ones.
	// if they're rejected in PreFilter (via PreFilterResult) or Filter plugins.
	// Nodes that pass PreFilter/Filter plugins are not included in this map.
	NodeToStatus *NodeToStatus
	// UnschedulablePlugins are plugins that returns Unschedulable or UnschedulableAndUnresolvable.
	UnschedulablePlugins sets.Set[string]
	// UnschedulablePlugins are plugins that returns Pending.
	PendingPlugins sets.Set[string]
	// PreFilterMsg records the messages returned from PreFilter plugins.
	PreFilterMsg string
	// PostFilterMsg records the messages returned from PostFilter plugins.
	PostFilterMsg string
}

// FitError describes a fit error of a pod.
type FitError struct {
	Pod         *v1.Pod
	NumAllNodes int
	Diagnosis   Diagnosis
}

const (
	// NoNodeAvailableMsg is used to format message when no nodes available.
	NoNodeAvailableMsg = "0/%v nodes are available"
)

func (d *Diagnosis) AddPluginStatus(sts *Status) {
	if sts.Plugin() == "" {
		return
	}
	if sts.IsRejected() {
		if d.UnschedulablePlugins == nil {
			d.UnschedulablePlugins = sets.New[string]()
		}
		d.UnschedulablePlugins.Insert(sts.Plugin())
	}
	if sts.Code() == Pending {
		if d.PendingPlugins == nil {
			d.PendingPlugins = sets.New[string]()
		}
		d.PendingPlugins.Insert(sts.Plugin())
	}
}

// Error returns detailed information of why the pod failed to fit on each node.
// A message format is "0/X nodes are available: <PreFilterMsg>. <FilterMsg>. <PostFilterMsg>."
func (f *FitError) Error() string {
	reasonMsg := fmt.Sprintf(NoNodeAvailableMsg+":", f.NumAllNodes)
	preFilterMsg := f.Diagnosis.PreFilterMsg
	if preFilterMsg != "" {
		// PreFilter plugin returns unschedulable.
		// Add the messages from PreFilter plugins to reasonMsg.
		reasonMsg += fmt.Sprintf(" %v.", preFilterMsg)
	}

	if preFilterMsg == "" {
		// the scheduling cycle went through PreFilter extension point successfully.
		//
		// When the prefilter plugin returns unschedulable,
		// the scheduling framework inserts the same unschedulable status to all nodes in NodeToStatusMap.
		// So, we shouldn't add the message from NodeToStatusMap when the PreFilter failed.
		// Otherwise, we will have duplicated reasons in the error message.
		reasons := make(map[string]int)
		f.Diagnosis.NodeToStatus.ForEachExplicitNode(func(_ string, status *Status) {
			for _, reason := range status.Reasons() {
				reasons[reason]++
			}
		})
		if f.Diagnosis.NodeToStatus.Len() < f.NumAllNodes {
			// Adding predefined reasons for nodes that are absent in NodeToStatusMap
			for _, reason := range f.Diagnosis.NodeToStatus.AbsentNodesStatus().Reasons() {
				reasons[reason] += f.NumAllNodes - f.Diagnosis.NodeToStatus.Len()
			}
		}

		sortReasonsHistogram := func() []string {
			var reasonStrings []string
			for k, v := range reasons {
				reasonStrings = append(reasonStrings, fmt.Sprintf("%v %v", v, k))
			}
			sort.Strings(reasonStrings)
			return reasonStrings
		}
		sortedFilterMsg := sortReasonsHistogram()
		if len(sortedFilterMsg) != 0 {
			reasonMsg += fmt.Sprintf(" %v.", strings.Join(sortedFilterMsg, ", "))
		}
	}

	// Add the messages from PostFilter plugins to reasonMsg.
	// We can add this message regardless of whether the scheduling cycle fails at PreFilter or Filter
	// since we may run PostFilter (if enabled) in both cases.
	postFilterMsg := f.Diagnosis.PostFilterMsg
	if postFilterMsg != "" {
		reasonMsg += fmt.Sprintf(" %v", postFilterMsg)
	}
	return reasonMsg
}

func newAffinityTerm(pod *v1.Pod, term *v1.PodAffinityTerm) (*AffinityTerm, error) {
	selector, err := metav1.LabelSelectorAsSelector(term.LabelSelector)
	if err != nil {
		return nil, err
	}

	namespaces := getNamespacesFromPodAffinityTerm(pod, term)
	nsSelector, err := metav1.LabelSelectorAsSelector(term.NamespaceSelector)
	if err != nil {
		return nil, err
	}

	return &AffinityTerm{Namespaces: namespaces, Selector: selector, TopologyKey: term.TopologyKey, NamespaceSelector: nsSelector}, nil
}

// GetAffinityTerms receives a Pod and affinity terms and returns the namespaces and
// selectors of the terms.
func GetAffinityTerms(pod *v1.Pod, v1Terms []v1.PodAffinityTerm) ([]AffinityTerm, error) {
	if v1Terms == nil {
		return nil, nil
	}

	var terms []AffinityTerm
	for i := range v1Terms {
		t, err := newAffinityTerm(pod, &v1Terms[i])
		if err != nil {
			// We get here if the label selector failed to process
			return nil, err
		}
		terms = append(terms, *t)
	}
	return terms, nil
}

// getWeightedAffinityTerms returns the list of processed affinity terms.
func getWeightedAffinityTerms(pod *v1.Pod, v1Terms []v1.WeightedPodAffinityTerm) ([]WeightedAffinityTerm, error) {
	if v1Terms == nil {
		return nil, nil
	}

	var terms []WeightedAffinityTerm
	for i := range v1Terms {
		t, err := newAffinityTerm(pod, &v1Terms[i].PodAffinityTerm)
		if err != nil {
			// We get here if the label selector failed to process
			return nil, err
		}
		terms = append(terms, WeightedAffinityTerm{AffinityTerm: *t, Weight: v1Terms[i].Weight})
	}
	return terms, nil
}

// NewPodInfo returns a new PodInfo.
func NewPodInfo(pod *v1.Pod) (*PodInfo, error) {
	pInfo := &PodInfo{}
	err := pInfo.Update(pod)
	return pInfo, err
}

func GetPodAffinityTerms(affinity *v1.Affinity) (terms []v1.PodAffinityTerm) {
	if affinity != nil && affinity.PodAffinity != nil {
		if len(affinity.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution) != 0 {
			terms = affinity.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution
		}
		// TODO: Uncomment this block when implement RequiredDuringSchedulingRequiredDuringExecution.
		// if len(affinity.PodAffinity.RequiredDuringSchedulingRequiredDuringExecution) != 0 {
		//	terms = append(terms, affinity.PodAffinity.RequiredDuringSchedulingRequiredDuringExecution...)
		// }
	}
	return terms
}

func GetPodAntiAffinityTerms(affinity *v1.Affinity) (terms []v1.PodAffinityTerm) {
	if affinity != nil && affinity.PodAntiAffinity != nil {
		if len(affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution) != 0 {
			terms = affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution
		}
		// TODO: Uncomment this block when implement RequiredDuringSchedulingRequiredDuringExecution.
		// if len(affinity.PodAntiAffinity.RequiredDuringSchedulingRequiredDuringExecution) != 0 {
		//	terms = append(terms, affinity.PodAntiAffinity.RequiredDuringSchedulingRequiredDuringExecution...)
		// }
	}
	return terms
}

// returns a set of names according to the namespaces indicated in podAffinityTerm.
// If namespaces is empty it considers the given pod's namespace.
func getNamespacesFromPodAffinityTerm(pod *v1.Pod, podAffinityTerm *v1.PodAffinityTerm) sets.Set[string] {
	names := sets.Set[string]{}
	if len(podAffinityTerm.Namespaces) == 0 && podAffinityTerm.NamespaceSelector == nil {
		names.Insert(pod.Namespace)
	} else {
		names.Insert(podAffinityTerm.Namespaces...)
	}
	return names
}

// ImageStateSummary provides summarized information about the state of an image.
type ImageStateSummary struct {
	// Size of the image
	Size int64
	// Used to track how many nodes have this image, it is computed from the Nodes field below
	// during the execution of Snapshot.
	NumNodes int
	// A set of node names for nodes having this image present. This field is used for
	// keeping track of the nodes during update/add/remove events.
	Nodes sets.Set[string]
}

// Snapshot returns a copy without Nodes field of ImageStateSummary
func (iss *ImageStateSummary) Snapshot() *ImageStateSummary {
	return &ImageStateSummary{
		Size:     iss.Size,
		NumNodes: iss.Nodes.Len(),
	}
}

// NodeInfo is node level aggregated information.
type NodeInfo struct {
	// Overall node information.
	node *v1.Node

	// Pods running on the node.
	Pods []*PodInfo

	// The subset of pods with affinity.
	PodsWithAffinity []*PodInfo

	// The subset of pods with required anti-affinity.
	PodsWithRequiredAntiAffinity []*PodInfo

	// Ports allocated on the node.
	UsedPorts HostPortInfo

	// Total requested resources of all pods on this node. This includes assumed
	// pods, which scheduler has sent for binding, but may not be scheduled yet.
	Requested *Resource
	// Total requested resources of all pods on this node with a minimum value
	// applied to each container's CPU and memory requests. This does not reflect
	// the actual resource requests for this node, but is used to avoid scheduling
	// many zero-request pods onto one node.
	NonZeroRequested *Resource
	// We store allocatedResources (which is Node.Status.Allocatable.*) explicitly
	// as int64, to avoid conversions and accessing map.
	Allocatable *Resource

	// ImageStates holds the entry of an image if and only if this image is on the node. The entry can be used for
	// checking an image's existence and advanced usage (e.g., image locality scheduling policy) based on the image
	// state information.
	ImageStates map[string]*ImageStateSummary

	// PVCRefCounts contains a mapping of PVC names to the number of pods on the node using it.
	// Keys are in the format "namespace/name".
	PVCRefCounts map[string]int

	// Whenever NodeInfo changes, generation is bumped.
	// This is used to avoid cloning it if the object didn't change.
	Generation int64
}

// NodeInfo implements KMetadata, so for example klog.KObjSlice(nodes) works
// when nodes is a []*NodeInfo.
var _ klog.KMetadata = &NodeInfo{}

func (n *NodeInfo) GetName() string {
	if n == nil {
		return "<nil>"
	}
	if n.node == nil {
		return "<no node>"
	}
	return n.node.Name
}
func (n *NodeInfo) GetNamespace() string { return "" }

// nextGeneration: Let's make sure history never forgets the name...
// Increments the generation number monotonically ensuring that generation numbers never collide.
// Collision of the generation numbers would be particularly problematic if a node was deleted and
// added back with the same name. See issue#63262.
func nextGeneration() int64 {
	return atomic.AddInt64(&generation, 1)
}

// Resource is a collection of compute resource.
type Resource struct {
	MilliCPU         int64
	Memory           int64
	EphemeralStorage int64
	// We store allowedPodNumber (which is Node.Status.Allocatable.Pods().Value())
	// explicitly as int, to avoid conversions and improve performance.
	AllowedPodNumber int
	// ScalarResources
	ScalarResources map[v1.ResourceName]int64
}

// NewResource creates a Resource from ResourceList
func NewResource(rl v1.ResourceList) *Resource {
	r := &Resource{}
	r.Add(rl)
	return r
}

// Add adds ResourceList into Resource.
func (r *Resource) Add(rl v1.ResourceList) {
	if r == nil {
		return
	}

	for rName, rQuant := range rl {
		switch rName {
		case v1.ResourceCPU:
			r.MilliCPU += rQuant.MilliValue()
		case v1.ResourceMemory:
			r.Memory += rQuant.Value()
		case v1.ResourcePods:
			r.AllowedPodNumber += int(rQuant.Value())
		case v1.ResourceEphemeralStorage:
			r.EphemeralStorage += rQuant.Value()
		default:
			if schedutil.IsScalarResourceName(rName) {
				r.AddScalar(rName, rQuant.Value())
			}
		}
	}
}

// Clone returns a copy of this resource.
func (r *Resource) Clone() *Resource {
	res := &Resource{
		MilliCPU:         r.MilliCPU,
		Memory:           r.Memory,
		AllowedPodNumber: r.AllowedPodNumber,
		EphemeralStorage: r.EphemeralStorage,
	}
	if r.ScalarResources != nil {
		res.ScalarResources = make(map[v1.ResourceName]int64, len(r.ScalarResources))
		for k, v := range r.ScalarResources {
			res.ScalarResources[k] = v
		}
	}
	return res
}

// AddScalar adds a resource by a scalar value of this resource.
func (r *Resource) AddScalar(name v1.ResourceName, quantity int64) {
	r.SetScalar(name, r.ScalarResources[name]+quantity)
}

// SetScalar sets a resource by a scalar value of this resource.
func (r *Resource) SetScalar(name v1.ResourceName, quantity int64) {
	// Lazily allocate scalar resource map.
	if r.ScalarResources == nil {
		r.ScalarResources = map[v1.ResourceName]int64{}
	}
	r.ScalarResources[name] = quantity
}

// SetMaxResource compares with ResourceList and takes max value for each Resource.
func (r *Resource) SetMaxResource(rl v1.ResourceList) {
	if r == nil {
		return
	}

	for rName, rQuantity := range rl {
		switch rName {
		case v1.ResourceMemory:
			r.Memory = max(r.Memory, rQuantity.Value())
		case v1.ResourceCPU:
			r.MilliCPU = max(r.MilliCPU, rQuantity.MilliValue())
		case v1.ResourceEphemeralStorage:
			r.EphemeralStorage = max(r.EphemeralStorage, rQuantity.Value())
		default:
			if schedutil.IsScalarResourceName(rName) {
				r.SetScalar(rName, max(r.ScalarResources[rName], rQuantity.Value()))
			}
		}
	}
}

// NewNodeInfo returns a ready to use empty NodeInfo object.
// If any pods are given in arguments, their information will be aggregated in
// the returned object.
func NewNodeInfo(pods ...*v1.Pod) *NodeInfo {
	ni := &NodeInfo{
		Requested:        &Resource{},
		NonZeroRequested: &Resource{},
		Allocatable:      &Resource{},
		Generation:       nextGeneration(),
		UsedPorts:        make(HostPortInfo),
		ImageStates:      make(map[string]*ImageStateSummary),
		PVCRefCounts:     make(map[string]int),
	}
	for _, pod := range pods {
		ni.AddPod(pod)
	}
	return ni
}

// Node returns overall information about this node.
func (n *NodeInfo) Node() *v1.Node {
	if n == nil {
		return nil
	}
	return n.node
}

// Snapshot returns a copy of this node, Except that ImageStates is copied without the Nodes field.
func (n *NodeInfo) Snapshot() *NodeInfo {
	clone := &NodeInfo{
		node:             n.node,
		Requested:        n.Requested.Clone(),
		NonZeroRequested: n.NonZeroRequested.Clone(),
		Allocatable:      n.Allocatable.Clone(),
		UsedPorts:        make(HostPortInfo),
		ImageStates:      make(map[string]*ImageStateSummary),
		PVCRefCounts:     make(map[string]int),
		Generation:       n.Generation,
	}
	if len(n.Pods) > 0 {
		clone.Pods = append([]*PodInfo(nil), n.Pods...)
	}
	if len(n.UsedPorts) > 0 {
		// HostPortInfo is a map-in-map struct
		// make sure it's deep copied
		for ip, portMap := range n.UsedPorts {
			clone.UsedPorts[ip] = make(map[ProtocolPort]struct{})
			for protocolPort, v := range portMap {
				clone.UsedPorts[ip][protocolPort] = v
			}
		}
	}
	if len(n.PodsWithAffinity) > 0 {
		clone.PodsWithAffinity = append([]*PodInfo(nil), n.PodsWithAffinity...)
	}
	if len(n.PodsWithRequiredAntiAffinity) > 0 {
		clone.PodsWithRequiredAntiAffinity = append([]*PodInfo(nil), n.PodsWithRequiredAntiAffinity...)
	}
	if len(n.ImageStates) > 0 {
		state := make(map[string]*ImageStateSummary, len(n.ImageStates))
		for imageName, imageState := range n.ImageStates {
			state[imageName] = imageState.Snapshot()
		}
		clone.ImageStates = state
	}
	for key, value := range n.PVCRefCounts {
		clone.PVCRefCounts[key] = value
	}
	return clone
}

// String returns representation of human readable format of this NodeInfo.
func (n *NodeInfo) String() string {
	podKeys := make([]string, len(n.Pods))
	for i, p := range n.Pods {
		podKeys[i] = p.Pod.Name
	}
	return fmt.Sprintf("&NodeInfo{Pods:%v, RequestedResource:%#v, NonZeroRequest: %#v, UsedPort: %#v, AllocatableResource:%#v}",
		podKeys, n.Requested, n.NonZeroRequested, n.UsedPorts, n.Allocatable)
}

// AddPodInfo adds pod information to this NodeInfo.
// Consider using this instead of AddPod if a PodInfo is already computed.
func (n *NodeInfo) AddPodInfo(podInfo *PodInfo) {
	n.Pods = append(n.Pods, podInfo)
	if podWithAffinity(podInfo.Pod) {
		n.PodsWithAffinity = append(n.PodsWithAffinity, podInfo)
	}
	if podWithRequiredAntiAffinity(podInfo.Pod) {
		n.PodsWithRequiredAntiAffinity = append(n.PodsWithRequiredAntiAffinity, podInfo)
	}
	n.update(podInfo, 1)
}

// AddPod is a wrapper around AddPodInfo.
func (n *NodeInfo) AddPod(pod *v1.Pod) {
	// ignore this err since apiserver doesn't properly validate affinity terms
	// and we can't fix the validation for backwards compatibility.
	podInfo, _ := NewPodInfo(pod)
	n.AddPodInfo(podInfo)
}

func podWithAffinity(p *v1.Pod) bool {
	affinity := p.Spec.Affinity
	return affinity != nil && (affinity.PodAffinity != nil || affinity.PodAntiAffinity != nil)
}

func podWithRequiredAntiAffinity(p *v1.Pod) bool {
	affinity := p.Spec.Affinity
	return affinity != nil && affinity.PodAntiAffinity != nil &&
		len(affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution) != 0
}

func removeFromSlice(logger klog.Logger, s []*PodInfo, k string) ([]*PodInfo, *PodInfo) {
	var removedPod *PodInfo
	for i := range s {
		tmpKey, err := GetPodKey(s[i].Pod)
		if err != nil {
			logger.Error(err, "Cannot get pod key", "pod", klog.KObj(s[i].Pod))
			continue
		}
		if k == tmpKey {
			removedPod = s[i]
			// delete the element
			s[i] = s[len(s)-1]
			s = s[:len(s)-1]
			break
		}
	}
	// resets the slices to nil so that we can do DeepEqual in unit tests.
	if len(s) == 0 {
		return nil, removedPod
	}
	return s, removedPod
}

// RemovePod subtracts pod information from this NodeInfo.
func (n *NodeInfo) RemovePod(logger klog.Logger, pod *v1.Pod) error {
	k, err := GetPodKey(pod)
	if err != nil {
		return err
	}
	if podWithAffinity(pod) {
		n.PodsWithAffinity, _ = removeFromSlice(logger, n.PodsWithAffinity, k)
	}
	if podWithRequiredAntiAffinity(pod) {
		n.PodsWithRequiredAntiAffinity, _ = removeFromSlice(logger, n.PodsWithRequiredAntiAffinity, k)
	}

	var removedPod *PodInfo
	if n.Pods, removedPod = removeFromSlice(logger, n.Pods, k); removedPod != nil {
		n.update(removedPod, -1)
		return nil
	}
	return fmt.Errorf("no corresponding pod %s in pods of node %s", pod.Name, n.node.Name)
}

// update node info based on the pod, and sign.
// The sign will be set to `+1` when AddPod and to `-1` when RemovePod.
func (n *NodeInfo) update(podInfo *PodInfo, sign int64) {
	podResource := podInfo.calculateResource()
	n.Requested.MilliCPU += sign * podResource.resource.MilliCPU
	n.Requested.Memory += sign * podResource.resource.Memory
	n.Requested.EphemeralStorage += sign * podResource.resource.EphemeralStorage
	if n.Requested.ScalarResources == nil && len(podResource.resource.ScalarResources) > 0 {
		n.Requested.ScalarResources = map[v1.ResourceName]int64{}
	}
	for rName, rQuant := range podResource.resource.ScalarResources {
		n.Requested.ScalarResources[rName] += sign * rQuant
	}
	n.NonZeroRequested.MilliCPU += sign * podResource.non0CPU
	n.NonZeroRequested.Memory += sign * podResource.non0Mem

	// Consume ports when pod added or release ports when pod removed.
	n.updateUsedPorts(podInfo.Pod, sign > 0)
	n.updatePVCRefCounts(podInfo.Pod, sign > 0)

	n.Generation = nextGeneration()
}

// getNonMissingContainerRequests returns the default non-zero CPU and memory
// requests for a container that the scheduler uses when container-level and
// pod-level requests are not set for a resource. It returns a ResourceList that
// includes these default non-zero requests, which are essential for the
// scheduler to function correctly.
// The method's behavior depends on whether pod-level resources are set or not:
// 1. When the pod level resources are not set, the method returns a ResourceList
// with the following defaults:
//   - CPU: schedutil.DefaultMilliCPURequest
//   - Memory: schedutil.DefaultMemoryRequest
//
// These defaults ensure that each container has a minimum resource request,
// allowing the scheduler to aggregate these requests and find a suitable node
// for the pod.
//
// 2. When the pod level resources are set, if a CPU or memory request is
// missing at the container-level *and* at the pod-level, the corresponding
// default value (schedutil.DefaultMilliCPURequest or schedutil.DefaultMemoryRequest)
// is included in the returned ResourceList.
// Note that these default values are not set in the Pod object itself, they are only used
// by the scheduler during node selection.
func getNonMissingContainerRequests(requests v1.ResourceList, podLevelResourcesSet bool) v1.ResourceList {
	if !podLevelResourcesSet {
		return v1.ResourceList{
			v1.ResourceCPU:    *resource.NewMilliQuantity(schedutil.DefaultMilliCPURequest, resource.DecimalSI),
			v1.ResourceMemory: *resource.NewQuantity(schedutil.DefaultMemoryRequest, resource.DecimalSI),
		}
	}

	nonMissingContainerRequests := make(v1.ResourceList, 2)
	// DefaultMilliCPURequest serves as the fallback value when both
	// pod-level and container-level CPU requests are not set.
	// Note that the apiserver defaulting logic will propagate a non-zero
	// container-level CPU request to the pod level if a pod-level request
	// is not explicitly set.
	if _, exists := requests[v1.ResourceCPU]; !exists {
		nonMissingContainerRequests[v1.ResourceCPU] = *resource.NewMilliQuantity(schedutil.DefaultMilliCPURequest, resource.DecimalSI)
	}

	// DefaultMemoryRequest serves as the fallback value when both
	// pod-level and container-level CPU requests are unspecified.
	// Note that the apiserver defaulting logic will propagate a non-zero
	// container-level memory request to the pod level if a pod-level request
	// is not explicitly set.
	if _, exists := requests[v1.ResourceMemory]; !exists {
		nonMissingContainerRequests[v1.ResourceMemory] = *resource.NewQuantity(schedutil.DefaultMemoryRequest, resource.DecimalSI)
	}
	return nonMissingContainerRequests

}

func (pi *PodInfo) calculateResource() podResource {
	if pi.cachedResource != nil {
		return *pi.cachedResource
	}
	inPlacePodVerticalScalingEnabled := utilfeature.DefaultFeatureGate.Enabled(features.InPlacePodVerticalScaling)
	podLevelResourcesEnabled := utilfeature.DefaultFeatureGate.Enabled(features.PodLevelResources)
	requests := resourcehelper.PodRequests(pi.Pod, resourcehelper.PodResourcesOptions{
		UseStatusResources: inPlacePodVerticalScalingEnabled,
		// SkipPodLevelResources is set to false when PodLevelResources feature is enabled.
		SkipPodLevelResources: !podLevelResourcesEnabled,
	})
	isPodLevelResourcesSet := podLevelResourcesEnabled && resourcehelper.IsPodLevelRequestsSet(pi.Pod)
	nonMissingContainerRequests := getNonMissingContainerRequests(requests, isPodLevelResourcesSet)
	non0Requests := requests
	if len(nonMissingContainerRequests) > 0 {
		non0Requests = resourcehelper.PodRequests(pi.Pod, resourcehelper.PodResourcesOptions{
			UseStatusResources: inPlacePodVerticalScalingEnabled,
			// SkipPodLevelResources is set to false when PodLevelResources feature is enabled.
			SkipPodLevelResources:       !podLevelResourcesEnabled,
			NonMissingContainerRequests: nonMissingContainerRequests,
		})
	}
	non0CPU := non0Requests[v1.ResourceCPU]
	non0Mem := non0Requests[v1.ResourceMemory]

	var res Resource
	res.Add(requests)
	podResource := podResource{
		resource: res,
		non0CPU:  non0CPU.MilliValue(),
		non0Mem:  non0Mem.Value(),
	}
	pi.cachedResource = &podResource
	return podResource
}

// updateUsedPorts updates the UsedPorts of NodeInfo.
func (n *NodeInfo) updateUsedPorts(pod *v1.Pod, add bool) {
	for _, container := range pod.Spec.Containers {
		for _, podPort := range container.Ports {
			if add {
				n.UsedPorts.Add(podPort.HostIP, string(podPort.Protocol), podPort.HostPort)
			} else {
				n.UsedPorts.Remove(podPort.HostIP, string(podPort.Protocol), podPort.HostPort)
			}
		}
	}
}

// updatePVCRefCounts updates the PVCRefCounts of NodeInfo.
func (n *NodeInfo) updatePVCRefCounts(pod *v1.Pod, add bool) {
	for _, v := range pod.Spec.Volumes {
		if v.PersistentVolumeClaim == nil {
			continue
		}

		key := GetNamespacedName(pod.Namespace, v.PersistentVolumeClaim.ClaimName)
		if add {
			n.PVCRefCounts[key] += 1
		} else {
			n.PVCRefCounts[key] -= 1
			if n.PVCRefCounts[key] <= 0 {
				delete(n.PVCRefCounts, key)
			}
		}
	}
}

// SetNode sets the overall node information.
func (n *NodeInfo) SetNode(node *v1.Node) {
	n.node = node
	n.Allocatable = NewResource(node.Status.Allocatable)
	n.Generation = nextGeneration()
}

// RemoveNode removes the node object, leaving all other tracking information.
func (n *NodeInfo) RemoveNode() {
	n.node = nil
	n.Generation = nextGeneration()
}

// GetPodKey returns the string key of a pod.
func GetPodKey(pod *v1.Pod) (string, error) {
	uid := string(pod.UID)
	if len(uid) == 0 {
		return "", errors.New("cannot get cache key for pod with empty UID")
	}
	return uid, nil
}

// GetNamespacedName returns the string format of a namespaced resource name.
func GetNamespacedName(namespace, name string) string {
	return fmt.Sprintf("%s/%s", namespace, name)
}

// DefaultBindAllHostIP defines the default ip address used to bind to all host.
const DefaultBindAllHostIP = "0.0.0.0"

// ProtocolPort represents a protocol port pair, e.g. tcp:80.
type ProtocolPort struct {
	Protocol string
	Port     int32
}

// NewProtocolPort creates a ProtocolPort instance.
func NewProtocolPort(protocol string, port int32) *ProtocolPort {
	pp := &ProtocolPort{
		Protocol: protocol,
		Port:     port,
	}

	if len(pp.Protocol) == 0 {
		pp.Protocol = string(v1.ProtocolTCP)
	}

	return pp
}

// HostPortInfo stores mapping from ip to a set of ProtocolPort
type HostPortInfo map[string]map[ProtocolPort]struct{}

// Add adds (ip, protocol, port) to HostPortInfo
func (h HostPortInfo) Add(ip, protocol string, port int32) {
	if port <= 0 {
		return
	}

	h.sanitize(&ip, &protocol)

	pp := NewProtocolPort(protocol, port)
	if _, ok := h[ip]; !ok {
		h[ip] = map[ProtocolPort]struct{}{
			*pp: {},
		}
		return
	}

	h[ip][*pp] = struct{}{}
}

// Remove removes (ip, protocol, port) from HostPortInfo
func (h HostPortInfo) Remove(ip, protocol string, port int32) {
	if port <= 0 {
		return
	}

	h.sanitize(&ip, &protocol)

	pp := NewProtocolPort(protocol, port)
	if m, ok := h[ip]; ok {
		delete(m, *pp)
		if len(h[ip]) == 0 {
			delete(h, ip)
		}
	}
}

// Len returns the total number of (ip, protocol, port) tuple in HostPortInfo
func (h HostPortInfo) Len() int {
	length := 0
	for _, m := range h {
		length += len(m)
	}
	return length
}

// CheckConflict checks if the input (ip, protocol, port) conflicts with the existing
// ones in HostPortInfo.
func (h HostPortInfo) CheckConflict(ip, protocol string, port int32) bool {
	if port <= 0 {
		return false
	}

	h.sanitize(&ip, &protocol)

	pp := NewProtocolPort(protocol, port)

	// If ip is 0.0.0.0 check all IP's (protocol, port) pair
	if ip == DefaultBindAllHostIP {
		for _, m := range h {
			if _, ok := m[*pp]; ok {
				return true
			}
		}
		return false
	}

	// If ip isn't 0.0.0.0, only check IP and 0.0.0.0's (protocol, port) pair
	for _, key := range []string{DefaultBindAllHostIP, ip} {
		if m, ok := h[key]; ok {
			if _, ok2 := m[*pp]; ok2 {
				return true
			}
		}
	}

	return false
}

// sanitize the parameters
func (h HostPortInfo) sanitize(ip, protocol *string) {
	if len(*ip) == 0 {
		*ip = DefaultBindAllHostIP
	}
	if len(*protocol) == 0 {
		*protocol = string(v1.ProtocolTCP)
	}
}
