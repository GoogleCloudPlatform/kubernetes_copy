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

package events

// Container event reason list
const (
	CreatedContainer        = "Created"
	StartedContainer        = "Started"
	RestartedContainer      = "Restarted"
	FailedToCreateContainer = "Failed"
	FailedToStartContainer  = "Failed"
	KillingContainer        = "Killing"
	PreemptContainer        = "Preempting"
	BackOffStartContainer   = "BackOff"
	ExceededGracePeriod     = "ExceededGracePeriod"
)

// Pod event reason list
const (
	FailedToKillPod                = "FailedKillPod"
	FailedToCreatePodContainer     = "FailedCreatePodContainer"
	FailedToMakePodDataDirectories = "Failed"
	NetworkNotReady                = "NetworkNotReady"
	ResizeDeferred                 = "ResizeDeferred"
	ResizeInfeasible               = "ResizeInfeasible"
)

// Image event reason list
const (
	PullingImage            = "Pulling"
	PulledImage             = "Pulled"
	FailedToPullImage       = "Failed"
	FailedToInspectImage    = "InspectFailed"
	ErrImageNeverPullPolicy = "ErrImageNeverPull"
	BackOffPullImage        = "BackOff"
)

// kubelet event reason list
const (
	NodeReady                            = "NodeReady"
	NodeNotReady                         = "NodeNotReady"
	NodeSchedulable                      = "NodeSchedulable"
	NodeNotSchedulable                   = "NodeNotSchedulable"
	StartingKubelet                      = "Starting"
	KubeletSetupFailed                   = "KubeletSetupFailed"
	FailedAttachVolume                   = "FailedAttachVolume"
	FailedMountVolume                    = "FailedMount"
	VolumeResizeFailed                   = "VolumeResizeFailed"
	VolumeResizeSuccess                  = "VolumeResizeSuccessful"
	FileSystemResizeFailed               = "FileSystemResizeFailed"
	FileSystemResizeSuccess              = "FileSystemResizeSuccessful"
	FailedMapVolume                      = "FailedMapVolume"
	WarnAlreadyMountedVolume             = "AlreadyMountedVolume"
	SuccessfulAttachVolume               = "SuccessfulAttachVolume"
	SuccessfulMountVolume                = "SuccessfulMountVolume"
	NodeRebooted                         = "Rebooted"
	NodeShutdown                         = "Shutdown"
	ContainerGCFailed                    = "ContainerGCFailed"
	ImageGCFailed                        = "ImageGCFailed"
	FailedNodeAllocatableEnforcement     = "FailedNodeAllocatableEnforcement"
	SuccessfulNodeAllocatableEnforcement = "NodeAllocatableEnforced"
	SandboxChanged                       = "SandboxChanged"
	FailedCreatePodSandBox               = "FailedCreatePodSandBox"
	FailedStatusPodSandBox               = "FailedPodSandBoxStatus"
	FailedMountOnFilesystemMismatch      = "FailedMountOnFilesystemMismatch"
	FailedPrepareDynamicResources        = "FailedPrepareDynamicResources"
	PossibleMemoryBackedVolumesOnDisk    = "PossibleMemoryBackedVolumesOnDisk"
	CgroupV1                             = "CgroupV1"
)

// Image manager event reason list
const (
	InvalidDiskCapacity = "InvalidDiskCapacity"
	FreeDiskSpaceFailed = "FreeDiskSpaceFailed"
)

// Probe event reason list
const (
	ContainerUnhealthy    = "Unhealthy"
	ContainerProbeWarning = "ProbeWarning"
)

// Pod worker event reason list
const (
	FailedSync = "FailedSync"
)

// Config event reason list
const (
	FailedValidation = "FailedValidation"
)

// Lifecycle hooks
const (
	FailedPostStartHook = "FailedPostStartHook"
	FailedPreStopHook   = "FailedPreStopHook"
)
