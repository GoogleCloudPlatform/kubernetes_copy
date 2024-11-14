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

package container

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"strings"

	"k8s.io/klog/v2"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/record"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
	podutil "k8s.io/kubernetes/pkg/api/v1/pod"
	sc "k8s.io/kubernetes/pkg/securitycontext"
	hashutil "k8s.io/kubernetes/pkg/util/hash"
	"k8s.io/kubernetes/third_party/forked/golang/expansion"
	utilsnet "k8s.io/utils/net"
)

// HandlerRunner runs a lifecycle handler for a container.
type HandlerRunner interface {
	Run(ctx context.Context, containerID ContainerID, pod *v1.Pod, container *v1.Container, handler *v1.LifecycleHandler) (string, error)
}

// RuntimeHelper wraps kubelet to make container runtime
// able to get necessary informations like the RunContainerOptions, DNS settings, Host IP.
type RuntimeHelper interface {
	GenerateRunContainerOptions(ctx context.Context, pod *v1.Pod, container *v1.Container, podIP string, podIPs []string, imageVolumes ImageVolumes) (contOpts *RunContainerOptions, cleanupAction func(), err error)
	GetPodDNS(pod *v1.Pod) (dnsConfig *runtimeapi.DNSConfig, err error)
	// GetPodCgroupParent returns the CgroupName identifier, and its literal cgroupfs form on the host
	// of a pod.
	GetPodCgroupParent(pod *v1.Pod) string
	GetPodDir(podUID types.UID) string
	GeneratePodHostNameAndDomain(pod *v1.Pod) (hostname string, hostDomain string, err error)
	// GetExtraSupplementalGroupsForPod returns a list of the extra
	// supplemental groups for the Pod. These extra supplemental groups come
	// from annotations on persistent volumes that the pod depends on.
	GetExtraSupplementalGroupsForPod(pod *v1.Pod) []int64

	// GetOrCreateUserNamespaceMappings returns the configuration for the sandbox user namespace
	GetOrCreateUserNamespaceMappings(pod *v1.Pod, runtimeHandler string) (*runtimeapi.UserNamespace, error)

	// PrepareDynamicResources prepares resources for a pod.
	PrepareDynamicResources(ctx context.Context, pod *v1.Pod) error

	// UnprepareDynamicResources unprepares resources for a a pod.
	UnprepareDynamicResources(ctx context.Context, pod *v1.Pod) error

	// SetPodWatchCondition flags a pod to be inspected until the condition is met.
	SetPodWatchCondition(types.UID, string, func(*PodStatus) bool)
}

// ShouldContainerBeRestarted checks whether a container needs to be restarted.
// TODO(yifan): Think about how to refactor this.
func ShouldContainerBeRestarted(container *v1.Container, pod *v1.Pod, podStatus *PodStatus) bool {
	// Once a pod has been marked deleted, it should not be restarted
	if pod.DeletionTimestamp != nil {
		return false
	}
	// Get latest container status.
	status := podStatus.FindContainerStatusByName(container.Name)
	// If the container was never started before, we should start it.
	// NOTE(random-liu): If all historical containers were GC'd, we'll also return true here.
	if status == nil {
		return true
	}
	// Check whether container is running
	if status.State == ContainerStateRunning {
		return false
	}
	// Always restart container in the unknown, or in the created state.
	if status.State == ContainerStateUnknown || status.State == ContainerStateCreated {
		return true
	}
	// Check RestartPolicy for dead container
	if pod.Spec.RestartPolicy == v1.RestartPolicyNever {
		klog.V(4).InfoS("Already ran container, do nothing", "pod", klog.KObj(pod), "containerName", container.Name)
		return false
	}
	if pod.Spec.RestartPolicy == v1.RestartPolicyOnFailure {
		// Check the exit code.
		if status.ExitCode == 0 {
			klog.V(4).InfoS("Already successfully ran container, do nothing", "pod", klog.KObj(pod), "containerName", container.Name)
			return false
		}
	}
	return true
}

// HashContainer returns the hash of the container. It is used to compare
// the running container with its desired spec.
// Note: remember to update hashValues in container_hash_test.go as well.
func HashContainer(container *v1.Container) uint64 {
	hash := fnv.New32a()
	containerJSON, _ := json.Marshal(pickFieldsToHash(container))
	hashutil.DeepHashObject(hash, containerJSON)
	return uint64(hash.Sum32())
}

// pickFieldsToHash pick fields that will affect the running status of the container for hash,
// currently this field range only contains `image` and `name`.
// Note: this list must be updated if ever kubelet wants to allow mutations to other fields.
func pickFieldsToHash(container *v1.Container) map[string]string {
	retval := map[string]string{
		"name":  container.Name,
		"image": container.Image,
	}
	return retval
}

// envVarsToMap constructs a map of environment name to value from a slice
// of env vars.
func envVarsToMap(envs []EnvVar) map[string]string {
	result := map[string]string{}
	for _, env := range envs {
		result[env.Name] = env.Value
	}
	return result
}

// v1EnvVarsToMap constructs a map of environment name to value from a slice
// of env vars.
func v1EnvVarsToMap(envs []v1.EnvVar) map[string]string {
	result := map[string]string{}
	for _, env := range envs {
		result[env.Name] = env.Value
	}

	return result
}

// ExpandContainerCommandOnlyStatic substitutes only static environment variable values from the
// container environment definitions. This does *not* include valueFrom substitutions.
// TODO: callers should use ExpandContainerCommandAndArgs with a fully resolved list of environment.
func ExpandContainerCommandOnlyStatic(containerCommand []string, envs []v1.EnvVar) (command []string) {
	mapping := expansion.MappingFuncFor(v1EnvVarsToMap(envs))
	if len(containerCommand) != 0 {
		for _, cmd := range containerCommand {
			command = append(command, expansion.Expand(cmd, mapping))
		}
	}
	return command
}

// ExpandContainerVolumeMounts expands the subpath of the given VolumeMount by replacing variable references with the values of given EnvVar.
func ExpandContainerVolumeMounts(mount v1.VolumeMount, envs []EnvVar) (string, error) {

	envmap := envVarsToMap(envs)
	missingKeys := sets.New[string]()
	expanded := expansion.Expand(mount.SubPathExpr, func(key string) string {
		value, ok := envmap[key]
		if !ok || len(value) == 0 {
			missingKeys.Insert(key)
		}
		return value
	})

	if len(missingKeys) > 0 {
		return "", fmt.Errorf("missing value for %s", strings.Join(sets.List(missingKeys), ", "))
	}
	return expanded, nil
}

// ExpandContainerCommandAndArgs expands the given Container's command by replacing variable references `with the values of given EnvVar.
func ExpandContainerCommandAndArgs(container *v1.Container, envs []EnvVar) (command []string, args []string) {
	mapping := expansion.MappingFuncFor(envVarsToMap(envs))

	if len(container.Command) != 0 {
		for _, cmd := range container.Command {
			command = append(command, expansion.Expand(cmd, mapping))
		}
	}

	if len(container.Args) != 0 {
		for _, arg := range container.Args {
			args = append(args, expansion.Expand(arg, mapping))
		}
	}

	return command, args
}

// FilterEventRecorder creates an event recorder to record object's event except implicitly required container's, like infra container.
func FilterEventRecorder(recorder record.EventRecorder) record.EventRecorder {
	return &innerEventRecorder{
		recorder: recorder,
	}
}

type innerEventRecorder struct {
	recorder record.EventRecorder
}

func (irecorder *innerEventRecorder) shouldRecordEvent(object runtime.Object) (*v1.ObjectReference, bool) {
	if ref, ok := object.(*v1.ObjectReference); ok {
		// this check is needed AFTER the cast. See https://github.com/kubernetes/kubernetes/issues/95552
		if ref == nil {
			return nil, false
		}
		if !strings.HasPrefix(ref.FieldPath, ImplicitContainerPrefix) {
			return ref, true
		}
	}
	return nil, false
}

func (irecorder *innerEventRecorder) Event(object runtime.Object, eventtype, reason, message string) {
	if ref, ok := irecorder.shouldRecordEvent(object); ok {
		irecorder.recorder.Event(ref, eventtype, reason, message)
	}
}

func (irecorder *innerEventRecorder) Eventf(object runtime.Object, eventtype, reason, messageFmt string, args ...interface{}) {
	if ref, ok := irecorder.shouldRecordEvent(object); ok {
		irecorder.recorder.Eventf(ref, eventtype, reason, messageFmt, args...)
	}

}

func (irecorder *innerEventRecorder) AnnotatedEventf(object runtime.Object, annotations map[string]string, eventtype, reason, messageFmt string, args ...interface{}) {
	if ref, ok := irecorder.shouldRecordEvent(object); ok {
		irecorder.recorder.AnnotatedEventf(ref, annotations, eventtype, reason, messageFmt, args...)
	}

}

// IsHostNetworkPod returns whether the host networking requested for the given Pod.
// Pod must not be nil.
func IsHostNetworkPod(pod *v1.Pod) bool {
	return pod.Spec.HostNetwork
}

// ConvertPodStatusToRunningPod returns Pod given PodStatus and container runtime string.
// TODO(random-liu): Convert PodStatus to running Pod, should be deprecated soon
func ConvertPodStatusToRunningPod(runtimeName string, podStatus *PodStatus) Pod {
	runningPod := Pod{
		ID:        podStatus.ID,
		Name:      podStatus.Name,
		Namespace: podStatus.Namespace,
	}
	for _, containerStatus := range podStatus.ContainerStatuses {
		if containerStatus.State != ContainerStateRunning {
			continue
		}
		container := &Container{
			ID:                  containerStatus.ID,
			Name:                containerStatus.Name,
			Image:               containerStatus.Image,
			ImageID:             containerStatus.ImageID,
			ImageRef:            containerStatus.ImageRef,
			ImageRuntimeHandler: containerStatus.ImageRuntimeHandler,
			Hash:                containerStatus.Hash,
			State:               containerStatus.State,
		}
		runningPod.Containers = append(runningPod.Containers, container)
	}

	// Populate sandboxes in kubecontainer.Pod
	for _, sandbox := range podStatus.SandboxStatuses {
		runningPod.Sandboxes = append(runningPod.Sandboxes, &Container{
			ID:    ContainerID{Type: runtimeName, ID: sandbox.Id},
			State: SandboxToContainerState(sandbox.State),
		})
	}
	return runningPod
}

// SandboxToContainerState converts runtimeapi.PodSandboxState to
// kubecontainer.State.
// This is only needed because we need to return sandboxes as if they were
// kubecontainer.Containers to avoid substantial changes to PLEG.
// TODO: Remove this once it becomes obsolete.
func SandboxToContainerState(state runtimeapi.PodSandboxState) State {
	switch state {
	case runtimeapi.PodSandboxState_SANDBOX_READY:
		return ContainerStateRunning
	case runtimeapi.PodSandboxState_SANDBOX_NOTREADY:
		return ContainerStateExited
	}
	return ContainerStateUnknown
}

// GetContainerSpec gets the container spec by containerName.
func GetContainerSpec(pod *v1.Pod, containerName string) *v1.Container {
	var containerSpec *v1.Container
	podutil.VisitContainers(&pod.Spec, podutil.AllFeatureEnabledContainers(), func(c *v1.Container, containerType podutil.ContainerType) bool {
		if containerName == c.Name {
			containerSpec = c
			return false
		}
		return true
	})
	return containerSpec
}

// HasPrivilegedContainer returns true if any of the containers in the pod are privileged.
func HasPrivilegedContainer(pod *v1.Pod) bool {
	var hasPrivileged bool
	podutil.VisitContainers(&pod.Spec, podutil.AllFeatureEnabledContainers(), func(c *v1.Container, containerType podutil.ContainerType) bool {
		if c.SecurityContext != nil && c.SecurityContext.Privileged != nil && *c.SecurityContext.Privileged {
			hasPrivileged = true
			return false
		}
		return true
	})
	return hasPrivileged
}

// HasWindowsHostProcessContainer returns true if any of the containers in a pod are HostProcess containers.
func HasWindowsHostProcessContainer(pod *v1.Pod) bool {
	var hasHostProcess bool
	podutil.VisitContainers(&pod.Spec, podutil.AllFeatureEnabledContainers(), func(c *v1.Container, containerType podutil.ContainerType) bool {
		if sc.HasWindowsHostProcessRequest(pod, c) {
			hasHostProcess = true
			return false
		}
		return true
	})

	return hasHostProcess
}

// AllContainersAreWindowsHostProcess returns true if all containers in a pod are HostProcess containers.
func AllContainersAreWindowsHostProcess(pod *v1.Pod) bool {
	allHostProcess := true
	podutil.VisitContainers(&pod.Spec, podutil.AllFeatureEnabledContainers(), func(c *v1.Container, containerType podutil.ContainerType) bool {
		if !sc.HasWindowsHostProcessRequest(pod, c) {
			allHostProcess = false
			return false
		}
		return true
	})

	return allHostProcess
}

// MakePortMappings creates internal port mapping from api port mapping.
func MakePortMappings(container *v1.Container) (ports []PortMapping) {
	names := make(map[string]struct{})
	for _, p := range container.Ports {
		pm := PortMapping{
			HostPort:      int(p.HostPort),
			ContainerPort: int(p.ContainerPort),
			Protocol:      p.Protocol,
			HostIP:        p.HostIP,
		}

		// We need to determine the address family this entry applies to. We do this to ensure
		// duplicate containerPort / protocol rules work across different address families.
		// https://github.com/kubernetes/kubernetes/issues/82373
		family := "any"
		if p.HostIP != "" {
			if utilsnet.IsIPv6String(p.HostIP) {
				family = "v6"
			} else {
				family = "v4"
			}
		}

		var name = p.Name
		if name == "" {
			name = fmt.Sprintf("%s-%s-%s:%d:%d", family, p.Protocol, p.HostIP, p.ContainerPort, p.HostPort)
		}

		// Protect against a port name being used more than once in a container.
		if _, ok := names[name]; ok {
			klog.InfoS("Port name conflicted, it is defined more than once", "portName", name)
			continue
		}
		ports = append(ports, pm)
		names[name] = struct{}{}
	}
	return
}

// HasAnyRegularContainerStarted returns true if any regular container has
// started, which indicates all init containers have been initialized.
// Deprecated: This function is not accurate when its pod sandbox is recreated.
// Use HasAnyActiveRegularContainerStarted instead.
func HasAnyRegularContainerStarted(spec *v1.PodSpec, statuses []v1.ContainerStatus) bool {
	if len(statuses) == 0 {
		return false
	}

	containerNames := make(map[string]struct{})
	for _, c := range spec.Containers {
		containerNames[c.Name] = struct{}{}
	}

	for _, status := range statuses {
		if _, ok := containerNames[status.Name]; !ok {
			continue
		}
		if status.State.Running != nil || status.State.Terminated != nil {
			return true
		}
	}

	return false
}

// HasAnyActiveRegularContainerStarted returns true if any regular container of
// the current pod sandbox has started, which indicates all init containers
// have been initialized.
func HasAnyActiveRegularContainerStarted(spec *v1.PodSpec, podStatus *PodStatus) bool {
	if podStatus == nil {
		return false
	}

	containerNames := sets.New[string]()
	for _, c := range spec.Containers {
		containerNames.Insert(c.Name)
	}

	for _, status := range podStatus.ActiveContainerStatuses {
		if !containerNames.Has(status.Name) {
			continue
		}
		return true
	}

	return false
}
