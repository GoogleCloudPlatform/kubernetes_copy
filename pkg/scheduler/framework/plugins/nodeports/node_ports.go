/*
Copyright 2019 The Kubernetes Authors.

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

package nodeports

import (
	"context"
	"fmt"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/feature"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/names"
	"k8s.io/kubernetes/pkg/scheduler/util"
)

// NodePorts is a plugin that checks if a node has free ports for the requested pod ports.
type NodePorts struct {
	enableSchedulingQueueHint bool
}

var _ framework.PreFilterPlugin = &NodePorts{}
var _ framework.FilterPlugin = &NodePorts{}
var _ framework.EnqueueExtensions = &NodePorts{}

const (
	// Name is the name of the plugin used in the plugin registry and configurations.
	Name = names.NodePorts

	// preFilterStateKey is the key in CycleState to NodePorts pre-computed data.
	// Using the name of the plugin will likely help us avoid collisions with other plugins.
	preFilterStateKey = "PreFilter" + Name

	// ErrReason when node ports aren't available.
	ErrReason = "node(s) didn't have free ports for the requested pod ports"
)

type preFilterState []*v1.ContainerPort

// Clone the prefilter state.
func (s preFilterState) Clone() framework.StateData {
	// The state is not impacted by adding/removing existing pods, hence we don't need to make a deep copy.
	return s
}

// Name returns name of the plugin. It is used in logs, etc.
func (pl *NodePorts) Name() string {
	return Name
}

// getContainerPorts returns the used host ports of Pods: if 'port' was used, a 'port:true' pair
// will be in the result; but it does not resolve port conflict.
func getContainerPorts(pods ...*v1.Pod) []*v1.ContainerPort {
	ports := []*v1.ContainerPort{}
	for _, pod := range pods {
		for j := range pod.Spec.Containers {
			container := &pod.Spec.Containers[j]
			for k := range container.Ports {
				// Only return ports with a host port specified.
				if container.Ports[k].HostPort <= 0 {
					continue
				}
				ports = append(ports, &container.Ports[k])
			}
		}
	}
	return ports
}

// PreFilter invoked at the prefilter extension point.
func (pl *NodePorts) PreFilter(ctx context.Context, cycleState *framework.CycleState, pod *v1.Pod) (*framework.PreFilterResult, *framework.Status) {
	s := getContainerPorts(pod)
	// Skip if a pod has no ports.
	if len(s) == 0 {
		return nil, framework.NewStatus(framework.Skip)
	}
	cycleState.Write(preFilterStateKey, preFilterState(s))
	return nil, nil
}

// PreFilterExtensions do not exist for this plugin.
func (pl *NodePorts) PreFilterExtensions() framework.PreFilterExtensions {
	return nil
}

func getPreFilterState(cycleState *framework.CycleState) (preFilterState, error) {
	c, err := cycleState.Read(preFilterStateKey)
	if err != nil {
		// preFilterState doesn't exist, likely PreFilter wasn't invoked.
		return nil, fmt.Errorf("reading %q from cycleState: %w", preFilterStateKey, err)
	}

	s, ok := c.(preFilterState)
	if !ok {
		return nil, fmt.Errorf("%+v  convert to nodeports.preFilterState error", c)
	}
	return s, nil
}

// EventsToRegister returns the possible events that may make a Pod
// failed by this plugin schedulable.
func (pl *NodePorts) EventsToRegister(_ context.Context) ([]framework.ClusterEventWithHint, error) {
	// A note about UpdateNodeTaint/UpdateNodeLabel event:
	// Ideally, it's supposed to register only Add because NodeUpdated event never means to have any free ports for the Pod.
	// But, we may miss Node/Add event due to preCheck, and we decided to register UpdateNodeTaint | UpdateNodeLabel for all plugins registering Node/Add.
	// See: https://github.com/kubernetes/kubernetes/issues/109437
	nodeActionType := framework.Add | framework.UpdateNodeTaint | framework.UpdateNodeLabel
	if pl.enableSchedulingQueueHint {
		// preCheck is not used when QHint is enabled, and hence Update event isn't necessary.
		nodeActionType = framework.Add
	}

	return []framework.ClusterEventWithHint{
		// Due to immutable fields `spec.containers[*].ports`, pod update events are ignored.
		{Event: framework.ClusterEvent{Resource: framework.Pod, ActionType: framework.Delete}, QueueingHintFn: pl.isSchedulableAfterPodDeleted},
		// We don't need the QueueingHintFn here because the scheduling of Pods will be always retried with backoff when this Event happens.
		// (the same as Queue)
		{Event: framework.ClusterEvent{Resource: framework.Node, ActionType: nodeActionType}},
	}, nil
}

// isSchedulableAfterPodDeleted is invoked whenever a pod deleted. It checks whether
// that change made a previously unschedulable pod schedulable.
func (pl *NodePorts) isSchedulableAfterPodDeleted(logger klog.Logger, pod *v1.Pod, oldObj, newObj interface{}) (framework.QueueingHint, error) {
	deletedPod, _, err := util.As[*v1.Pod](oldObj, nil)
	if err != nil {
		return framework.Queue, err
	}

	// If the deleted pod is unscheduled, it doesn't make the target pod schedulable.
	if deletedPod.Spec.NodeName == "" && deletedPod.Status.NominatedNodeName == "" {
		logger.V(4).Info("the deleted pod is unscheduled and it doesn't make the target pod schedulable", "pod", klog.KObj(pod), "deletedPod", klog.KObj(deletedPod))
		return framework.QueueSkip, nil
	}

	// Get the used host ports of the deleted pod.
	usedPorts := make(framework.HostPortInfo)
	for _, container := range deletedPod.Spec.Containers {
		for _, podPort := range container.Ports {
			if podPort.HostPort > 0 {
				usedPorts.Add(podPort.HostIP, string(podPort.Protocol), podPort.HostPort)
			}
		}
	}

	// If the deleted pod doesn't use any host ports, it doesn't make the target pod schedulable.
	if len(usedPorts) == 0 {
		return framework.QueueSkip, nil
	}

	// Construct a fake NodeInfo that only has the deleted Pod.
	// If we can schedule `pod` to this fake node, it means that `pod` and the deleted pod don't have any common port(s).
	// So, deleting that pod couldn't make `pod` schedulable.
	nodeInfo := framework.NodeInfo{UsedPorts: usedPorts}
	if Fits(pod, &nodeInfo) {
		logger.V(4).Info("the deleted pod and the target pod don't have any common port(s), returning QueueSkip as deleting this Pod won't make the Pod schedulable", "pod", klog.KObj(pod), "deletedPod", klog.KObj(deletedPod))
		return framework.QueueSkip, nil
	}

	logger.V(4).Info("the deleted pod and the target pod have any common port(s), returning Queue as deleting this Pod may make the Pod schedulable", "pod", klog.KObj(pod), "deletedPod", klog.KObj(deletedPod))
	return framework.Queue, nil
}

// Filter invoked at the filter extension point.
func (pl *NodePorts) Filter(ctx context.Context, cycleState *framework.CycleState, pod *v1.Pod, nodeInfo *framework.NodeInfo) *framework.Status {
	wantPorts, err := getPreFilterState(cycleState)
	if err != nil {
		return framework.AsStatus(err)
	}

	fits := fitsPorts(wantPorts, nodeInfo)
	if !fits {
		return framework.NewStatus(framework.Unschedulable, ErrReason)
	}

	return nil
}

// Fits checks if the pod fits the node.
func Fits(pod *v1.Pod, nodeInfo *framework.NodeInfo) bool {
	return fitsPorts(getContainerPorts(pod), nodeInfo)
}

func fitsPorts(wantPorts []*v1.ContainerPort, nodeInfo *framework.NodeInfo) bool {
	// try to see whether existingPorts and wantPorts will conflict or not
	existingPorts := nodeInfo.UsedPorts
	for _, cp := range wantPorts {
		if existingPorts.CheckConflict(cp.HostIP, string(cp.Protocol), cp.HostPort) {
			return false
		}
	}
	return true
}

// New initializes a new plugin and returns it.
func New(_ context.Context, _ runtime.Object, _ framework.Handle, fts feature.Features) (framework.Plugin, error) {
	return &NodePorts{
		enableSchedulingQueueHint: fts.EnableSchedulingQueueHint,
	}, nil
}
