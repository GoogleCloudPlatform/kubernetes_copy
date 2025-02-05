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

//go:generate mockery
package container

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"reflect"
	"strings"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/remotecommand"
	"k8s.io/client-go/util/flowcontrol"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/volume"
)

// Version interface allow to consume the runtime versions - compare and format to string.
type Version interface {
	// Compare compares two versions of the runtime. On success it returns -1
	// if the version is less than the other, 1 if it is greater than the other,
	// or 0 if they are equal.
	Compare(other string) (int, error)
	// String returns a string that represents the version.
	String() string
}

// ImageSpec is an internal representation of an image.  Currently, it wraps the
// value of a Container's Image field, but in the future it will include more detailed
// information about the different image types.
type ImageSpec struct {
	// ID of the image.
	Image string
	// Runtime handler used to pull this image
	RuntimeHandler string
	// The annotations for the image.
	// This should be passed to CRI during image pulls and returned when images are listed.
	Annotations []Annotation
}

// ImageStats contains statistics about all the images currently available.
type ImageStats struct {
	// Total amount of storage consumed by existing images.
	TotalStorageBytes uint64
}

// Runtime interface defines the interfaces that should be implemented
// by a container runtime.
// Thread safety is required from implementations of this interface.
type Runtime interface {
	// Type returns the type of the container runtime.
	Type() string

	// Version returns the version information of the container runtime.
	Version(ctx context.Context) (Version, error)

	// APIVersion returns the cached API version information of the container
	// runtime. Implementation is expected to update this cache periodically.
	// This may be different from the runtime engine's version.
	// TODO(random-liu): We should fold this into Version()
	APIVersion() (Version, error)
	// Status returns the status of the runtime. An error is returned if the Status
	// function itself fails, nil otherwise.
	Status(ctx context.Context) (*RuntimeStatus, error)
	// GetPods returns a list of containers grouped by pods. The boolean parameter
	// specifies whether the runtime returns all containers including those already
	// exited and dead containers (used for garbage collection).
	GetPods(ctx context.Context, all bool) ([]*Pod, error)
	// GarbageCollect removes dead containers using the specified container gc policy
	// If allSourcesReady is not true, it means that kubelet doesn't have the
	// complete list of pods from all available sources (e.g., apiserver, http,
	// file). In this case, garbage collector should refrain itself from aggressive
	// behavior such as removing all containers of unrecognized pods (yet).
	// If evictNonDeletedPods is set to true, containers and sandboxes belonging to pods
	// that are terminated, but not deleted will be evicted.  Otherwise, only deleted pods
	// will be GC'd.
	// TODO: Revisit this method and make it cleaner.
	GarbageCollect(ctx context.Context, gcPolicy GCPolicy, allSourcesReady bool, evictNonDeletedPods bool) error
	// SyncPod syncs the running pod into the desired pod.
	SyncPod(ctx context.Context, pod *v1.Pod, podStatus *PodStatus, pullSecrets []v1.Secret, backOff *flowcontrol.Backoff) PodSyncResult
	// KillPod kills all the containers of a pod. Pod may be nil, running pod must not be.
	// TODO(random-liu): Return PodSyncResult in KillPod.
	// gracePeriodOverride if specified allows the caller to override the pod default grace period.
	// only hard kill paths are allowed to specify a gracePeriodOverride in the kubelet in order to not corrupt user data.
	// it is useful when doing SIGKILL for hard eviction scenarios, or max grace period during soft eviction scenarios.
	KillPod(ctx context.Context, pod *v1.Pod, runningPod Pod, gracePeriodOverride *int64) error
	// GetPodStatus retrieves the status of the pod, including the
	// information of all containers in the pod that are visible in Runtime.
	GetPodStatus(ctx context.Context, uid types.UID, name, namespace string) (*PodStatus, error)
	// TODO(vmarmol): Unify pod and containerID args.
	// GetContainerLogs returns logs of a specific container. By
	// default, it returns a snapshot of the container log. Set 'follow' to true to
	// stream the log. Set 'follow' to false and specify the number of lines (e.g.
	// "100" or "all") to tail the log.
	GetContainerLogs(ctx context.Context, pod *v1.Pod, containerID ContainerID, logOptions *v1.PodLogOptions, stdout, stderr io.Writer) (err error)
	// DeleteContainer deletes a container. If the container is still running, an error is returned.
	DeleteContainer(ctx context.Context, containerID ContainerID) error
	// ImageService provides methods to image-related methods.
	ImageService
	// UpdatePodCIDR sends a new podCIDR to the runtime.
	// This method just proxies a new runtimeConfig with the updated
	// CIDR value down to the runtime shim.
	UpdatePodCIDR(ctx context.Context, podCIDR string) error
	// CheckpointContainer tells the runtime to checkpoint a container
	// and store the resulting archive to the checkpoint directory.
	CheckpointContainer(ctx context.Context, options *runtimeapi.CheckpointContainerRequest) error
	// Generate pod status from the CRI event
	GeneratePodStatus(event *runtimeapi.ContainerEventResponse) (*PodStatus, error)
	// ListMetricDescriptors gets the descriptors for the metrics that will be returned in ListPodSandboxMetrics.
	// This list should be static at startup: either the client and server restart together when
	// adding or removing metrics descriptors, or they should not change.
	// Put differently, if ListPodSandboxMetrics references a name that is not described in the initial
	// ListMetricDescriptors call, then the metric will not be broadcasted.
	ListMetricDescriptors(ctx context.Context) ([]*runtimeapi.MetricDescriptor, error)
	// ListPodSandboxMetrics retrieves the metrics for all pod sandboxes.
	ListPodSandboxMetrics(ctx context.Context) ([]*runtimeapi.PodSandboxMetrics, error)
	// GetContainerStatus returns the status for the container.
	GetContainerStatus(ctx context.Context, id ContainerID) (*Status, error)
}

// StreamingRuntime is the interface implemented by runtimes that handle the serving of the
// streaming calls (exec/attach/port-forward) themselves. In this case, Kubelet should redirect to
// the runtime server.
type StreamingRuntime interface {
	GetExec(ctx context.Context, id ContainerID, cmd []string, stdin, stdout, stderr, tty bool) (*url.URL, error)
	GetAttach(ctx context.Context, id ContainerID, stdin, stdout, stderr, tty bool) (*url.URL, error)
	GetPortForward(ctx context.Context, podName, podNamespace string, podUID types.UID, ports []int32) (*url.URL, error)
}

// ImageService interfaces allows to work with image service.
type ImageService interface {
	// PullImage pulls an image from the network to local storage using the supplied
	// secrets if necessary. It returns a reference (digest or ID) to the pulled image.
	PullImage(ctx context.Context, image ImageSpec, pullSecrets []v1.Secret, podSandboxConfig *runtimeapi.PodSandboxConfig, serviceAccountName string) (string, error)
	// GetImageRef gets the reference (digest or ID) of the image which has already been in
	// the local storage. It returns ("", nil) if the image isn't in the local storage.
	GetImageRef(ctx context.Context, image ImageSpec) (string, error)
	// ListImages gets all images currently on the machine.
	ListImages(ctx context.Context) ([]Image, error)
	// RemoveImage removes the specified image.
	RemoveImage(ctx context.Context, image ImageSpec) error
	// ImageStats returns Image statistics.
	ImageStats(ctx context.Context) (*ImageStats, error)
	// ImageFsInfo returns a list of file systems for containers/images
	ImageFsInfo(ctx context.Context) (*runtimeapi.ImageFsInfoResponse, error)
	// GetImageSize returns the size of the image
	GetImageSize(ctx context.Context, image ImageSpec) (uint64, error)
}

// Attacher interface allows to attach a container.
type Attacher interface {
	AttachContainer(ctx context.Context, id ContainerID, stdin io.Reader, stdout, stderr io.WriteCloser, tty bool, resize <-chan remotecommand.TerminalSize) (err error)
}

// CommandRunner interface allows to run command in a container.
type CommandRunner interface {
	// RunInContainer synchronously executes the command in the container, and returns the output.
	// If the command completes with a non-0 exit code, a k8s.io/utils/exec.ExitError will be returned.
	RunInContainer(ctx context.Context, id ContainerID, cmd []string, timeout time.Duration) ([]byte, error)
}

// Pod is a group of containers.
type Pod struct {
	// The ID of the pod, which can be used to retrieve a particular pod
	// from the pod list returned by GetPods().
	ID types.UID
	// The name and namespace of the pod, which is readable by human.
	Name      string
	Namespace string
	// Creation timestamps of the Pod in nanoseconds.
	CreatedAt uint64
	// List of containers that belongs to this pod. It may contain only
	// running containers, or mixed with dead ones (when GetPods(true)).
	Containers []*Container
	// List of sandboxes associated with this pod. The sandboxes are converted
	// to Container temporarily to avoid substantial changes to other
	// components. This is only populated by kuberuntime.
	// TODO: use the runtimeApi.PodSandbox type directly.
	Sandboxes []*Container
}

// PodPair contains both runtime#Pod and api#Pod
type PodPair struct {
	// APIPod is the v1.Pod
	APIPod *v1.Pod
	// RunningPod is the pod defined in pkg/kubelet/container/runtime#Pod
	RunningPod *Pod
}

// ContainerID is a type that identifies a container.
type ContainerID struct {
	// The type of the container runtime. e.g. 'docker'.
	Type string
	// The identification of the container, this is comsumable by
	// the underlying container runtime. (Note that the container
	// runtime interface still takes the whole struct as input).
	ID string
}

// BuildContainerID returns the ContainerID given type and id.
func BuildContainerID(typ, ID string) ContainerID {
	return ContainerID{Type: typ, ID: ID}
}

// ParseContainerID is a convenience method for creating a ContainerID from an ID string.
func ParseContainerID(containerID string) ContainerID {
	var id ContainerID
	if err := id.ParseString(containerID); err != nil {
		klog.ErrorS(err, "Parsing containerID failed")
	}
	return id
}

// ParseString converts given string into ContainerID
func (c *ContainerID) ParseString(data string) error {
	// Trim the quotes and split the type and ID.
	parts := strings.Split(strings.Trim(data, "\""), "://")
	if len(parts) != 2 {
		return fmt.Errorf("invalid container ID: %q", data)
	}
	c.Type, c.ID = parts[0], parts[1]
	return nil
}

func (c *ContainerID) String() string {
	return fmt.Sprintf("%s://%s", c.Type, c.ID)
}

// IsEmpty returns whether given ContainerID is empty.
func (c *ContainerID) IsEmpty() bool {
	return *c == ContainerID{}
}

// MarshalJSON formats a given ContainerID into a byte array.
func (c *ContainerID) MarshalJSON() ([]byte, error) {
	return []byte(fmt.Sprintf("%q", c.String())), nil
}

// UnmarshalJSON parses ContainerID from a given array of bytes.
func (c *ContainerID) UnmarshalJSON(data []byte) error {
	return c.ParseString(string(data))
}

// State represents the state of a container
type State string

const (
	// ContainerStateCreated indicates a container that has been created (e.g. with docker create) but not started.
	ContainerStateCreated State = "created"
	// ContainerStateRunning indicates a currently running container.
	ContainerStateRunning State = "running"
	// ContainerStateExited indicates a container that ran and completed ("stopped" in other contexts, although a created container is technically also "stopped").
	ContainerStateExited State = "exited"
	// ContainerStateUnknown encompasses all the states that we currently don't care about (like restarting, paused, dead).
	ContainerStateUnknown State = "unknown"
)

// ContainerReasonStatusUnknown indicates a container the status of the container cannot be determined.
const ContainerReasonStatusUnknown string = "ContainerStatusUnknown"

// Container provides the runtime information for a container, such as ID, hash,
// state of the container.
type Container struct {
	// The ID of the container, used by the container runtime to identify
	// a container.
	ID ContainerID
	// The name of the container, which should be the same as specified by
	// v1.Container.
	Name string
	// The image name of the container, this also includes the tag of the image,
	// the expected form is "NAME:TAG".
	Image string
	// The id of the image used by the container.
	ImageID string
	// The digested reference of the image used by the container.
	ImageRef string
	// Runtime handler used to pull the image if any.
	ImageRuntimeHandler string
	// Hash of the container, used for comparison. Optional for containers
	// not managed by kubelet.
	Hash uint64
	// State is the state of the container.
	State State
}

// PodStatus represents the status of the pod and its containers.
// v1.PodStatus can be derived from examining PodStatus and v1.Pod.
type PodStatus struct {
	// ID of the pod.
	ID types.UID
	// Name of the pod.
	Name string
	// Namespace of the pod.
	Namespace string
	// All IPs assigned to this pod
	IPs []string
	// Status of containers in the pod.
	ContainerStatuses []*Status
	// Status of the pod sandbox.
	// Only for kuberuntime now, other runtime may keep it nil.
	SandboxStatuses []*runtimeapi.PodSandboxStatus
	// Timestamp at which container and pod statuses were recorded
	TimeStamp time.Time
}

// ContainerResources represents the Resources allocated to the running container.
type ContainerResources struct {
	// CPU capacity reserved for the container
	CPURequest *resource.Quantity
	// CPU limit enforced on the container
	CPULimit *resource.Quantity
	// Memory capaacity reserved for the container
	MemoryRequest *resource.Quantity
	// Memory limit enforced on the container
	MemoryLimit *resource.Quantity
}

// Status represents the status of a container.
//
// Status does not contain VolumeMap because CRI API is unaware of volume names.
type Status struct {
	// ID of the container.
	ID ContainerID
	// Name of the container.
	Name string
	// Status of the container.
	State State
	// Creation time of the container.
	CreatedAt time.Time
	// Start time of the container.
	StartedAt time.Time
	// Finish time of the container.
	FinishedAt time.Time
	// Exit code of the container.
	ExitCode int
	// Name of the image, this also includes the tag of the image,
	// the expected form is "NAME:TAG".
	Image string
	// ID of the image.
	ImageID string
	// The digested reference of the image used by the container.
	ImageRef string
	// Runtime handler used to pull the image if any.
	ImageRuntimeHandler string
	// Hash of the container, used for comparison.
	Hash uint64
	// Number of times that the container has been restarted.
	RestartCount int
	// A string explains why container is in such a status.
	Reason string
	// Message written by the container before exiting (stored in
	// TerminationMessagePath).
	Message string
	// CPU and memory resources for this container
	Resources *ContainerResources
	// User identity information of the first process of this container
	User *ContainerUser
	// Mounts are the volume mounts of the container
	Mounts []Mount
}

// ContainerUser represents user identity information
type ContainerUser struct {
	// Linux holds user identity information of the first process of the containers in Linux.
	// Note that this field cannot be set when spec.os.name is windows.
	Linux *LinuxContainerUser

	// Windows holds user identity information of the first process of the containers in Windows
	// This is just reserved for future use.
	// Windows *WindowsContainerUser
}

// LinuxContainerUser represents user identity information in Linux containers
type LinuxContainerUser struct {
	// UID is the primary uid of the first process in the container
	UID int64
	// GID is the primary gid of the first process in the container
	GID int64
	// SupplementalGroups are the supplemental groups attached to the first process in the container
	SupplementalGroups []int64
}

// FindContainerStatusByName returns container status in the pod status with the given name.
// When there are multiple containers' statuses with the same name, the first match will be returned.
func (podStatus *PodStatus) FindContainerStatusByName(containerName string) *Status {
	for _, containerStatus := range podStatus.ContainerStatuses {
		if containerStatus.Name == containerName {
			return containerStatus
		}
	}
	return nil
}

// GetRunningContainerStatuses returns container status of all the running containers in a pod
func (podStatus *PodStatus) GetRunningContainerStatuses() []*Status {
	runningContainerStatuses := []*Status{}
	for _, containerStatus := range podStatus.ContainerStatuses {
		if containerStatus.State == ContainerStateRunning {
			runningContainerStatuses = append(runningContainerStatuses, containerStatus)
		}
	}
	return runningContainerStatuses
}

// Image contains basic information about a container image.
type Image struct {
	// ID of the image.
	ID string
	// Other names by which this image is known.
	RepoTags []string
	// Digests by which this image is known.
	RepoDigests []string
	// The size of the image in bytes.
	Size int64
	// ImageSpec for the image which include annotations.
	Spec ImageSpec
	// Pin for preventing garbage collection
	Pinned bool
}

// EnvVar represents the environment variable.
type EnvVar struct {
	Name  string
	Value string
}

// Annotation represents an annotation.
type Annotation struct {
	Name  string
	Value string
}

// Mount represents a volume mount.
type Mount struct {
	// Name of the volume mount.
	// TODO(yifan): Remove this field, as this is not representing the unique name of the mount,
	// but the volume name only.
	Name string
	// Path of the mount within the container.
	ContainerPath string
	// Path of the mount on the host.
	HostPath string
	// Whether the mount is read-only.
	ReadOnly bool
	// Whether the mount is recursive read-only.
	// Must not be true if ReadOnly is false.
	RecursiveReadOnly bool
	// Whether the mount needs SELinux relabeling
	SELinuxRelabel bool
	// Requested propagation mode
	Propagation runtimeapi.MountPropagation
	// Image is set if an OCI volume as image ID or digest should get mounted (special case).
	Image *runtimeapi.ImageSpec
}

// ImageVolumes is a map of image specs by volume name.
type ImageVolumes = map[string]*runtimeapi.ImageSpec

// PortMapping contains information about the port mapping.
type PortMapping struct {
	// Protocol of the port mapping.
	Protocol v1.Protocol
	// The port number within the container.
	ContainerPort int
	// The port number on the host.
	HostPort int
	// The host IP.
	HostIP string
}

// DeviceInfo contains information about the device.
type DeviceInfo struct {
	// Path on host for mapping
	PathOnHost string
	// Path in Container to map
	PathInContainer string
	// Cgroup permissions
	Permissions string
}

// CDIDevice contains information about CDI device
type CDIDevice struct {
	// Name is a fully qualified device name according to
	// https://github.com/cncf-tags/container-device-interface/blob/e66544063aa7760c4ea6330ce9e6c757f8e61df2/README.md?plain=1#L9-L15
	Name string
}

// RunContainerOptions specify the options which are necessary for running containers
type RunContainerOptions struct {
	// The environment variables list.
	Envs []EnvVar
	// The mounts for the containers.
	Mounts []Mount
	// The host devices mapped into the containers.
	Devices []DeviceInfo
	// The CDI devices for the container
	CDIDevices []CDIDevice
	// The annotations for the container
	// These annotations are generated by other components (i.e.,
	// not users). Currently, only device plugins populate the annotations.
	Annotations []Annotation
	// If the container has specified the TerminationMessagePath, then
	// this directory will be used to create and mount the log file to
	// container.TerminationMessagePath
	PodContainerDir string
	// The type of container rootfs
	ReadOnly bool
}

// VolumeInfo contains information about the volume.
type VolumeInfo struct {
	// Mounter is the volume's mounter
	Mounter volume.Mounter
	// BlockVolumeMapper is the Block volume's mapper
	BlockVolumeMapper volume.BlockVolumeMapper
	// SELinuxLabeled indicates whether this volume has had the
	// pod's SELinux label applied to it or not
	SELinuxLabeled bool
	// Whether the volume permission is set to read-only or not
	// This value is passed from volume.spec
	ReadOnly bool
	// Inner volume spec name, which is the PV name if used, otherwise
	// it is the same as the outer volume spec name.
	InnerVolumeSpecName string
}

// VolumeMap represents the map of volumes.
type VolumeMap map[string]VolumeInfo

// RuntimeConditionType is the types of required runtime conditions.
type RuntimeConditionType string

const (
	// RuntimeReady means the runtime is up and ready to accept basic containers.
	RuntimeReady RuntimeConditionType = "RuntimeReady"
	// NetworkReady means the runtime network is up and ready to accept containers which require network.
	NetworkReady RuntimeConditionType = "NetworkReady"
)

// RuntimeStatus contains the status of the runtime.
type RuntimeStatus struct {
	// Conditions is an array of current observed runtime conditions.
	Conditions []RuntimeCondition
	// Handlers is an array of current available handlers
	Handlers []RuntimeHandler
	// Features is the set of features implemented by the runtime
	Features *RuntimeFeatures
}

// GetRuntimeCondition gets a specified runtime condition from the runtime status.
func (r *RuntimeStatus) GetRuntimeCondition(t RuntimeConditionType) *RuntimeCondition {
	for i := range r.Conditions {
		c := &r.Conditions[i]
		if c.Type == t {
			return c
		}
	}
	return nil
}

// String formats the runtime status into human readable string.
func (r *RuntimeStatus) String() string {
	var ss []string
	var sh []string
	for _, c := range r.Conditions {
		ss = append(ss, c.String())
	}
	for _, h := range r.Handlers {
		sh = append(sh, h.String())
	}
	return fmt.Sprintf("Runtime Conditions: %s; Handlers: %s, Features: %s", strings.Join(ss, ", "), strings.Join(sh, ", "), r.Features.String())
}

// RuntimeHandler contains condition information for the runtime handler.
type RuntimeHandler struct {
	// Name is the handler name.
	Name string
	// SupportsRecursiveReadOnlyMounts is true if the handler has support for
	// recursive read-only mounts.
	SupportsRecursiveReadOnlyMounts bool
	// SupportsUserNamespaces is true if the handler has support for
	// user namespaces.
	SupportsUserNamespaces bool
}

// String formats the runtime handler into human readable string.
func (h *RuntimeHandler) String() string {
	return fmt.Sprintf("Name=%s SupportsRecursiveReadOnlyMounts: %v SupportsUserNamespaces: %v",
		h.Name, h.SupportsRecursiveReadOnlyMounts, h.SupportsUserNamespaces)
}

// RuntimeCondition contains condition information for the runtime.
type RuntimeCondition struct {
	// Type of runtime condition.
	Type RuntimeConditionType
	// Status of the condition, one of true/false.
	Status bool
	// Reason is brief reason for the condition's last transition.
	Reason string
	// Message is human readable message indicating details about last transition.
	Message string
}

// String formats the runtime condition into human readable string.
func (c *RuntimeCondition) String() string {
	return fmt.Sprintf("%s=%t reason:%s message:%s", c.Type, c.Status, c.Reason, c.Message)
}

// RuntimeFeatures contains the set of features implemented by the runtime
type RuntimeFeatures struct {
	SupplementalGroupsPolicy bool
}

// String formats the runtime condition into a human readable string.
func (f *RuntimeFeatures) String() string {
	if f == nil {
		return "nil"
	}
	return fmt.Sprintf("SupplementalGroupsPolicy: %v", f.SupplementalGroupsPolicy)
}

// Pods represents the list of pods
type Pods []*Pod

// FindPodByID finds and returns a pod in the pod list by UID. It will return an empty pod
// if not found.
func (p Pods) FindPodByID(podUID types.UID) Pod {
	for i := range p {
		if p[i].ID == podUID {
			return *p[i]
		}
	}
	return Pod{}
}

// FindPodByFullName finds and returns a pod in the pod list by the full name.
// It will return an empty pod if not found.
func (p Pods) FindPodByFullName(podFullName string) Pod {
	for i := range p {
		if BuildPodFullName(p[i].Name, p[i].Namespace) == podFullName {
			return *p[i]
		}
	}
	return Pod{}
}

// FindPod combines FindPodByID and FindPodByFullName, it finds and returns a pod in the
// pod list either by the full name or the pod ID. It will return an empty pod
// if not found.
func (p Pods) FindPod(podFullName string, podUID types.UID) Pod {
	if len(podFullName) > 0 {
		return p.FindPodByFullName(podFullName)
	}
	return p.FindPodByID(podUID)
}

// FindContainerByName returns a container in the pod with the given name.
// When there are multiple containers with the same name, the first match will
// be returned.
func (p *Pod) FindContainerByName(containerName string) *Container {
	for _, c := range p.Containers {
		if c.Name == containerName {
			return c
		}
	}
	return nil
}

// FindContainerByID returns a container in the pod with the given ContainerID.
func (p *Pod) FindContainerByID(id ContainerID) *Container {
	for _, c := range p.Containers {
		if c.ID == id {
			return c
		}
	}
	return nil
}

// FindSandboxByID returns a sandbox in the pod with the given ContainerID.
func (p *Pod) FindSandboxByID(id ContainerID) *Container {
	for _, c := range p.Sandboxes {
		if c.ID == id {
			return c
		}
	}
	return nil
}

// ToAPIPod converts Pod to v1.Pod. Note that if a field in v1.Pod has no
// corresponding field in Pod, the field would not be populated.
func (p *Pod) ToAPIPod() *v1.Pod {
	var pod v1.Pod
	pod.UID = p.ID
	pod.Name = p.Name
	pod.Namespace = p.Namespace

	for _, c := range p.Containers {
		var container v1.Container
		container.Name = c.Name
		container.Image = c.Image
		pod.Spec.Containers = append(pod.Spec.Containers, container)
	}
	return &pod
}

// IsEmpty returns true if the pod is empty.
func (p *Pod) IsEmpty() bool {
	return reflect.DeepEqual(p, &Pod{})
}

// GetPodFullName returns a name that uniquely identifies a pod.
func GetPodFullName(pod *v1.Pod) string {
	// Use underscore as the delimiter because it is not allowed in pod name
	// (DNS subdomain format), while allowed in the container name format.
	return pod.Name + "_" + pod.Namespace
}

// BuildPodFullName builds the pod full name from pod name and namespace.
func BuildPodFullName(name, namespace string) string {
	return name + "_" + namespace
}

// ParsePodFullName parsed the pod full name.
func ParsePodFullName(podFullName string) (string, string, error) {
	parts := strings.Split(podFullName, "_")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("failed to parse the pod full name %q", podFullName)
	}
	return parts[0], parts[1], nil
}

// Option is a functional option type for Runtime, useful for
// completely optional settings.
type Option func(Runtime)

// SortContainerStatusesByCreationTime sorts the container statuses by creation time.
type SortContainerStatusesByCreationTime []*Status

func (s SortContainerStatusesByCreationTime) Len() int      { return len(s) }
func (s SortContainerStatusesByCreationTime) Swap(i, j int) { s[i], s[j] = s[j], s[i] }
func (s SortContainerStatusesByCreationTime) Less(i, j int) bool {
	return s[i].CreatedAt.Before(s[j].CreatedAt)
}

const (
	// MaxPodTerminationMessageLogLength is the maximum bytes any one pod may have written
	// as termination message output across all containers. Containers will be evenly truncated
	// until output is below this limit.
	MaxPodTerminationMessageLogLength = 1024 * 12
	// MaxContainerTerminationMessageLength is the upper bound any one container may write to
	// its termination message path. Contents above this length will be truncated.
	MaxContainerTerminationMessageLength = 1024 * 4
	// MaxContainerTerminationMessageLogLength is the maximum bytes any one container will
	// have written to its termination message when the message is read from the logs.
	MaxContainerTerminationMessageLogLength = 1024 * 2
	// MaxContainerTerminationMessageLogLines is the maximum number of previous lines of
	// log output that the termination message can contain.
	MaxContainerTerminationMessageLogLines = 80
)
