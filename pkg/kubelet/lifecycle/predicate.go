/*
Copyright 2016 The Kubernetes Authors.

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

package lifecycle

import (
	"fmt"
	"runtime"

	v1 "k8s.io/api/core/v1"
	"k8s.io/component-helpers/scheduling/corev1"
	"k8s.io/klog/v2"
	v1helper "k8s.io/kubernetes/pkg/apis/core/v1/helper"
	"k8s.io/kubernetes/pkg/kubelet/types"
	"k8s.io/kubernetes/pkg/scheduler"
	schedulerframework "k8s.io/kubernetes/pkg/scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/tainttoleration"
)

const (
	// PodOSSelectorNodeLabelDoesNotMatch is used to denote that the pod was
	// rejected admission to the node because the pod's node selector
	// corresponding to kubernetes.io/os label didn't match the node label.
	PodOSSelectorNodeLabelDoesNotMatch = "PodOSSelectorNodeLabelDoesNotMatch"

	// PodOSNotSupported is used to denote that the pod was rejected admission
	// to the node because the pod's OS field didn't match the node OS.
	PodOSNotSupported = "PodOSNotSupported"

	// InvalidNodeInfo is used to denote that the pod was rejected admission
	// to the node because the kubelet was unable to retrieve the node info.
	InvalidNodeInfo = "InvalidNodeInfo"

	// InitContainerRestartPolicyForbidden is used to denote that the pod was
	// rejected admission to the node because it uses a restart policy other
	// than Always for some of its init containers.
	InitContainerRestartPolicyForbidden = "InitContainerRestartPolicyForbidden"

	// UnexpectedAdmissionError is used to denote that the pod was rejected
	// admission to the node because of an error during admission that could not
	// be categorized.
	UnexpectedAdmissionError = "UnexpectedAdmissionError"

	// UnknownReason is used to denote that the pod was rejected admission to
	// the node because a predicate failed for a reason that could not be
	// determined.
	UnknownReason = "UnknownReason"

	// UnexpectedPredicateFailureType is used to denote that the pod was
	// rejected admission to the node because a predicate returned a reason
	// object that was not an InsufficientResourceError or a PredicateFailureError.
	UnexpectedPredicateFailureType = "UnexpectedPredicateFailureType"

	// Prefix for admission reason when kubelet rejects a pod due to insufficient
	// resources available.
	InsufficientResourcePrefix = "OutOf"

	// These reasons are used to denote that the pod has reject admission
	// to the node because there's not enough resources to run the pod.
	OutOfCPU              = "OutOfcpu"
	OutOfMemory           = "OutOfmemory"
	OutOfEphemeralStorage = "OutOfephemeral-storage"
	OutOfPods             = "OutOfpods"
)

type getNodeAnyWayFuncType func() (*v1.Node, error)

type pluginResourceUpdateFuncType func(*schedulerframework.NodeInfo, *PodAdmitAttributes) error

// AdmissionFailureHandler is an interface which defines how to deal with a failure to admit a pod.
// This allows for the graceful handling of pod admission failure.
type AdmissionFailureHandler interface {
	HandleAdmissionFailure(admitPod *v1.Pod, failureReasons []PredicateFailureReason) ([]PredicateFailureReason, error)
}

type predicateAdmitHandler struct {
	getNodeAnyWayFunc        getNodeAnyWayFuncType
	pluginResourceUpdateFunc pluginResourceUpdateFuncType
	admissionFailureHandler  AdmissionFailureHandler
}

var _ PodAdmitHandler = &predicateAdmitHandler{}

// NewPredicateAdmitHandler returns a PodAdmitHandler which is used to evaluates
// if a pod can be admitted from the perspective of predicates.
func NewPredicateAdmitHandler(getNodeAnyWayFunc getNodeAnyWayFuncType, admissionFailureHandler AdmissionFailureHandler, pluginResourceUpdateFunc pluginResourceUpdateFuncType) PodAdmitHandler {
	return &predicateAdmitHandler{
		getNodeAnyWayFunc,
		pluginResourceUpdateFunc,
		admissionFailureHandler,
	}
}

func (w *predicateAdmitHandler) Admit(attrs *PodAdmitAttributes) PodAdmitResult {
	node, err := w.getNodeAnyWayFunc()
	if err != nil {
		klog.ErrorS(err, "Cannot get Node info")
		return PodAdmitResult{
			Admit:   false,
			Reason:  InvalidNodeInfo,
			Message: "Kubelet cannot get node info.",
		}
	}
	admitPod := attrs.Pod

	// perform the checks that preemption will not help first to avoid meaningless pod eviction
	if rejectPodAdmissionBasedOnOSSelector(admitPod, node) {
		return PodAdmitResult{
			Admit:   false,
			Reason:  PodOSSelectorNodeLabelDoesNotMatch,
			Message: "Failed to admit pod as the `kubernetes.io/os` label doesn't match node label",
		}
	}
	if rejectPodAdmissionBasedOnOSField(admitPod) {
		return PodAdmitResult{
			Admit:   false,
			Reason:  PodOSNotSupported,
			Message: "Failed to admit pod as the OS field doesn't match node OS",
		}
	}

	pods := attrs.OtherPods
	nodeInfo := schedulerframework.NewNodeInfo(pods...)
	nodeInfo.SetNode(node)

	// ensure the node has enough plugin resources for that required in pods
	if err = w.pluginResourceUpdateFunc(nodeInfo, attrs); err != nil {
		message := fmt.Sprintf("Update plugin resources failed due to %v, which is unexpected.", err)
		klog.InfoS("Failed to admit pod", "pod", klog.KObj(admitPod), "message", message)
		return PodAdmitResult{
			Admit:   false,
			Reason:  UnexpectedAdmissionError,
			Message: message,
		}
	}

	// Remove the requests of the extended resources that are missing in the
	// node info. This is required to support cluster-level resources, which
	// are extended resources unknown to nodes.
	//
	// Caveat: If a pod was manually bound to a node (e.g., static pod) where a
	// node-level extended resource it requires is not found, then kubelet will
	// not fail admission while it should. This issue will be addressed with
	// the Resource Class API in the future.
	podWithoutMissingExtendedResources := removeMissingExtendedResources(admitPod, nodeInfo)

	reasons := generalFilter(podWithoutMissingExtendedResources, nodeInfo)
	fit := len(reasons) == 0
	if !fit {
		reasons, err = w.admissionFailureHandler.HandleAdmissionFailure(admitPod, reasons)
		fit = len(reasons) == 0 && err == nil
		if err != nil {
			message := fmt.Sprintf("Unexpected error while attempting to recover from admission failure: %v", err)
			klog.InfoS("Failed to admit pod, unexpected error while attempting to recover from admission failure", "pod", klog.KObj(admitPod), "err", err)
			return PodAdmitResult{
				Admit:   fit,
				Reason:  UnexpectedAdmissionError,
				Message: message,
			}
		}
	}
	if !fit {
		var reason string
		var message string
		if len(reasons) == 0 {
			message = fmt.Sprint("GeneralPredicates failed due to unknown reason, which is unexpected.")
			klog.InfoS("Failed to admit pod: GeneralPredicates failed due to unknown reason, which is unexpected", "pod", klog.KObj(admitPod))
			return PodAdmitResult{
				Admit:   fit,
				Reason:  UnknownReason,
				Message: message,
			}
		}
		// If there are failed predicates, we only return the first one as a reason.
		r := reasons[0]
		switch re := r.(type) {
		case *PredicateFailureError:
			reason = re.PredicateName
			message = re.Error()
			klog.V(2).InfoS("Predicate failed on Pod", "pod", klog.KObj(admitPod), "err", message)
		case *InsufficientResourceError:
			switch re.ResourceName {
			case v1.ResourceCPU:
				reason = OutOfCPU
			case v1.ResourceMemory:
				reason = OutOfMemory
			case v1.ResourceEphemeralStorage:
				reason = OutOfEphemeralStorage
			case v1.ResourcePods:
				reason = OutOfPods
			default:
				reason = fmt.Sprintf("%s%s", InsufficientResourcePrefix, re.ResourceName)
			}
			message = re.Error()
			klog.V(2).InfoS("Predicate failed on Pod", "pod", klog.KObj(admitPod), "err", message)
		default:
			reason = UnexpectedPredicateFailureType
			message = fmt.Sprintf("GeneralPredicates failed due to %v, which is unexpected.", r)
			klog.InfoS("Failed to admit pod", "pod", klog.KObj(admitPod), "err", message)
		}
		return PodAdmitResult{
			Admit:   fit,
			Reason:  reason,
			Message: message,
		}
	}
	return PodAdmitResult{
		Admit: true,
	}
}

// rejectPodAdmissionBasedOnOSSelector rejects pod if it's nodeSelector doesn't match
// We expect the kubelet status reconcile which happens every 10sec to update the node labels if there is a mismatch.
func rejectPodAdmissionBasedOnOSSelector(pod *v1.Pod, node *v1.Node) bool {
	labels := node.Labels
	osName, osLabelExists := labels[v1.LabelOSStable]
	if !osLabelExists || osName != runtime.GOOS {
		if len(labels) == 0 {
			labels = make(map[string]string)
		}
		labels[v1.LabelOSStable] = runtime.GOOS
	}
	podLabelSelector, podOSLabelExists := pod.Labels[v1.LabelOSStable]
	if !podOSLabelExists {
		// If the labelselector didn't exist, let's keep the current behavior as is
		return false
	} else if podOSLabelExists && podLabelSelector != labels[v1.LabelOSStable] {
		return true
	}
	return false
}

// rejectPodAdmissionBasedOnOSField rejects pods if their OS field doesn't match runtime.GOOS.
// TODO: Relax this restriction when we start supporting LCOW in kubernetes where podOS may not match
// node's OS.
func rejectPodAdmissionBasedOnOSField(pod *v1.Pod) bool {
	if pod.Spec.OS == nil {
		return false
	}
	// If the pod OS doesn't match runtime.GOOS return false
	return string(pod.Spec.OS.Name) != runtime.GOOS
}

func removeMissingExtendedResources(pod *v1.Pod, nodeInfo *schedulerframework.NodeInfo) *v1.Pod {
	filterExtendedResources := func(containers []v1.Container) {
		for i, c := range containers {
			// We only handle requests in Requests but not Limits because the
			// PodFitsResources predicate, to which the result pod will be passed,
			// does not use Limits.
			filteredResources := make(v1.ResourceList)
			for rName, rQuant := range c.Resources.Requests {
				if v1helper.IsExtendedResourceName(rName) {
					if _, found := nodeInfo.Allocatable.ScalarResources[rName]; !found {
						continue
					}
				}
				filteredResources[rName] = rQuant
			}
			containers[i].Resources.Requests = filteredResources
		}
	}
	podCopy := pod.DeepCopy()
	filterExtendedResources(podCopy.Spec.Containers)
	filterExtendedResources(podCopy.Spec.InitContainers)
	return podCopy
}

// InsufficientResourceError is an error type that indicates what kind of resource limit is
// hit and caused the unfitting failure.
type InsufficientResourceError struct {
	ResourceName v1.ResourceName
	Requested    int64
	Used         int64
	Capacity     int64
}

func (e *InsufficientResourceError) Error() string {
	return fmt.Sprintf("Node didn't have enough resource: %s, requested: %d, used: %d, capacity: %d",
		e.ResourceName, e.Requested, e.Used, e.Capacity)
}

// PredicateFailureReason interface represents the failure reason of a predicate.
type PredicateFailureReason interface {
	GetReason() string
}

// GetReason returns the reason of the InsufficientResourceError.
func (e *InsufficientResourceError) GetReason() string {
	return fmt.Sprintf("Insufficient %v", e.ResourceName)
}

// GetInsufficientAmount returns the amount of the insufficient resource of the error.
func (e *InsufficientResourceError) GetInsufficientAmount() int64 {
	return e.Requested - (e.Capacity - e.Used)
}

// PredicateFailureError describes a failure error of predicate.
type PredicateFailureError struct {
	PredicateName string
	PredicateDesc string
}

func (e *PredicateFailureError) Error() string {
	return fmt.Sprintf("Predicate %s failed: %s", e.PredicateName, e.PredicateDesc)
}

// GetReason returns the reason of the PredicateFailureError.
func (e *PredicateFailureError) GetReason() string {
	return e.PredicateDesc
}

// generalFilter checks a group of filterings that the kubelet cares about.
func generalFilter(pod *v1.Pod, nodeInfo *schedulerframework.NodeInfo) []PredicateFailureReason {
	admissionResults := scheduler.AdmissionCheck(pod, nodeInfo, true, pod.Status.Phase == v1.PodRunning)
	var reasons []PredicateFailureReason
	for _, r := range admissionResults {
		if r.InsufficientResource != nil {
			reasons = append(reasons, &InsufficientResourceError{
				ResourceName: r.InsufficientResource.ResourceName,
				Requested:    r.InsufficientResource.Requested,
				Used:         r.InsufficientResource.Used,
				Capacity:     r.InsufficientResource.Capacity,
			})
		} else {
			reasons = append(reasons, &PredicateFailureError{r.Name, r.Reason})
		}
	}

	// Check taint/toleration except for static pods
	if !types.IsStaticPod(pod) {
		_, isUntolerated := corev1.FindMatchingUntoleratedTaint(nodeInfo.Node().Spec.Taints, pod.Spec.Tolerations, func(t *v1.Taint) bool {
			// Kubelet is only interested in the NoExecute taint.
			return t.Effect == v1.TaintEffectNoExecute
		})
		if isUntolerated {
			reasons = append(reasons, &PredicateFailureError{tainttoleration.Name, tainttoleration.ErrReasonNotMatch})
		}
	}

	return reasons
}
