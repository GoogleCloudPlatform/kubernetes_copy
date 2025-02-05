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

package kuberuntime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	goruntime "runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	crierror "k8s.io/cri-api/pkg/errors"

	"github.com/opencontainers/selinux/go-selinux"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/armon/circbuf"
	"k8s.io/klog/v2"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubetypes "k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
	remote "k8s.io/cri-client/pkg"
	kubelettypes "k8s.io/kubelet/pkg/types"
	podutil "k8s.io/kubernetes/pkg/api/v1/pod"
	"k8s.io/kubernetes/pkg/features"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
	"k8s.io/kubernetes/pkg/kubelet/events"
	proberesults "k8s.io/kubernetes/pkg/kubelet/prober/results"
	"k8s.io/kubernetes/pkg/kubelet/types"
	"k8s.io/kubernetes/pkg/kubelet/util/format"
	"k8s.io/kubernetes/pkg/util/tail"
	volumeutil "k8s.io/kubernetes/pkg/volume/util"
)

var (
	// ErrCreateContainerConfig - failed to create container config
	ErrCreateContainerConfig = errors.New("CreateContainerConfigError")
	// ErrPreCreateHook - failed to execute PreCreateHook
	ErrPreCreateHook = errors.New("PreCreateHookError")
	// ErrCreateContainer - failed to create container
	ErrCreateContainer = errors.New("CreateContainerError")
	// ErrPreStartHook - failed to execute PreStartHook
	ErrPreStartHook = errors.New("PreStartHookError")
	// ErrPostStartHook - failed to execute PostStartHook
	ErrPostStartHook = errors.New("PostStartHookError")
)

// recordContainerEvent should be used by the runtime manager for all container related events.
// it has sanity checks to ensure that we do not write events that can abuse our masters.
// in particular, it ensures that a containerID never appears in an event message as that
// is prone to causing a lot of distinct events that do not count well.
// it replaces any reference to a containerID with the containerName which is stable, and is what users know.
func (m *kubeGenericRuntimeManager) recordContainerEvent(pod *v1.Pod, container *v1.Container, containerID, eventType, reason, message string, args ...interface{}) {
	ref, err := kubecontainer.GenerateContainerRef(pod, container)
	if err != nil {
		klog.ErrorS(err, "Can't make a container ref", "pod", klog.KObj(pod), "podUID", pod.UID, "containerName", container.Name)
		return
	}
	eventMessage := message
	if len(args) > 0 {
		eventMessage = fmt.Sprintf(message, args...)
	}
	// this is a hack, but often the error from the runtime includes the containerID
	// which kills our ability to deduplicate events.  this protection makes a huge
	// difference in the number of unique events
	if containerID != "" {
		eventMessage = strings.Replace(eventMessage, containerID, container.Name, -1)
	}
	m.recorder.Event(ref, eventType, reason, eventMessage)
}

// startSpec wraps the spec required to start a container, either a regular/init container
// or an ephemeral container. Ephemeral containers contain all the fields of regular/init
// containers, plus some additional fields. In both cases startSpec.container will be set.
type startSpec struct {
	container          *v1.Container
	ephemeralContainer *v1.EphemeralContainer
}

func containerStartSpec(c *v1.Container) *startSpec {
	return &startSpec{container: c}
}

func ephemeralContainerStartSpec(ec *v1.EphemeralContainer) *startSpec {
	return &startSpec{
		container:          (*v1.Container)(&ec.EphemeralContainerCommon),
		ephemeralContainer: ec,
	}
}

// getTargetID returns the kubecontainer.ContainerID for ephemeral container namespace
// targeting. The target is stored as EphemeralContainer.TargetContainerName, which must be
// resolved to a ContainerID using podStatus. The target container must already exist, which
// usually isn't a problem since ephemeral containers aren't allowed at pod creation time.
func (s *startSpec) getTargetID(podStatus *kubecontainer.PodStatus) (*kubecontainer.ContainerID, error) {
	if s.ephemeralContainer == nil || s.ephemeralContainer.TargetContainerName == "" {
		return nil, nil
	}

	targetStatus := podStatus.FindContainerStatusByName(s.ephemeralContainer.TargetContainerName)
	if targetStatus == nil {
		return nil, fmt.Errorf("unable to find target container %v", s.ephemeralContainer.TargetContainerName)
	}

	return &targetStatus.ID, nil
}

func calcRestartCountByLogDir(path string) (int, error) {
	// if the path doesn't exist then it's not an error
	if _, err := os.Stat(path); err != nil {
		return 0, nil
	}
	files, err := os.ReadDir(path)
	if err != nil {
		return 0, err
	}
	if len(files) == 0 {
		return 0, nil
	}
	restartCount := 0
	restartCountLogFileRegex := regexp.MustCompile(`^(\d+)\.log(\..*)?`)
	for _, file := range files {
		if file.IsDir() {
			continue
		}
		matches := restartCountLogFileRegex.FindStringSubmatch(file.Name())
		if len(matches) == 0 {
			continue
		}
		count, err := strconv.Atoi(matches[1])
		if err != nil {
			// unlikely kubelet created this file,
			// likely custom file with random numbers as a name
			continue
		}
		count++
		if count > restartCount {
			restartCount = count
		}
	}
	return restartCount, nil
}

func (m *kubeGenericRuntimeManager) getPodRuntimeHandler(pod *v1.Pod) (podRuntimeHandler string, err error) {
	// If RuntimeClassInImageCriAPI feature gate is enabled, pass runtimehandler
	// information for the runtime class specified. If not runtime class is
	// specified, then pass ""
	if utilfeature.DefaultFeatureGate.Enabled(features.RuntimeClassInImageCriAPI) {
		if pod.Spec.RuntimeClassName != nil && *pod.Spec.RuntimeClassName != "" {
			podRuntimeHandler, err = m.runtimeClassManager.LookupRuntimeHandler(pod.Spec.RuntimeClassName)
			if err != nil {
				msg := fmt.Sprintf("Failed to lookup runtimeHandler for runtimeClassName %v", pod.Spec.RuntimeClassName)
				return msg, err
			}
		}
	}

	return podRuntimeHandler, nil
}

// startContainer starts a container and returns a message indicates why it is failed on error.
// It starts the container through the following steps:
// * pull the image
// * create the container
// * start the container
// * run the post start lifecycle hooks (if applicable)
func (m *kubeGenericRuntimeManager) startContainer(ctx context.Context, podSandboxID string, podSandboxConfig *runtimeapi.PodSandboxConfig, spec *startSpec, pod *v1.Pod, podStatus *kubecontainer.PodStatus, pullSecrets []v1.Secret, podIP string, podIPs []string, imageVolumes kubecontainer.ImageVolumes) (string, error) {
	container := spec.container

	// Step 1: pull the image.
	podRuntimeHandler, err := m.getPodRuntimeHandler(pod)
	if err != nil {
		return "", err
	}

	ref, err := kubecontainer.GenerateContainerRef(pod, container)
	if err != nil {
		klog.ErrorS(err, "Couldn't make a ref to pod", "pod", klog.KObj(pod), "containerName", container.Name)
	}

	imageRef, msg, err := m.imagePuller.EnsureImageExists(ctx, ref, pod, container.Image, pullSecrets, podSandboxConfig, podRuntimeHandler, container.ImagePullPolicy)
	if err != nil {
		s, _ := grpcstatus.FromError(err)
		m.recordContainerEvent(pod, container, "", v1.EventTypeWarning, events.FailedToCreateContainer, "Error: %v", s.Message())
		return msg, err
	}

	// Step 2: create the container.
	// For a new container, the RestartCount should be 0
	restartCount := 0
	containerStatus := podStatus.FindContainerStatusByName(container.Name)
	if containerStatus != nil {
		restartCount = containerStatus.RestartCount + 1
	} else {
		// The container runtime keeps state on container statuses and
		// what the container restart count is. When nodes are rebooted
		// some container runtimes clear their state which causes the
		// restartCount to be reset to 0. This causes the logfile to
		// start at 0.log, which either overwrites or appends to the
		// already existing log.
		//
		// We are checking to see if the log directory exists, and find
		// the latest restartCount by checking the log name -
		// {restartCount}.log - and adding 1 to it.
		logDir := BuildContainerLogsDirectory(m.podLogsDirectory, pod.Namespace, pod.Name, pod.UID, container.Name)
		restartCount, err = calcRestartCountByLogDir(logDir)
		if err != nil {
			klog.InfoS("Cannot calculate restartCount from the log directory", "logDir", logDir, "err", err)
			restartCount = 0
		}
	}

	target, err := spec.getTargetID(podStatus)
	if err != nil {
		s, _ := grpcstatus.FromError(err)
		m.recordContainerEvent(pod, container, "", v1.EventTypeWarning, events.FailedToCreateContainer, "Error: %v", s.Message())
		return s.Message(), ErrCreateContainerConfig
	}

	containerConfig, cleanupAction, err := m.generateContainerConfig(ctx, container, pod, restartCount, podIP, imageRef, podIPs, target, imageVolumes)
	if cleanupAction != nil {
		defer cleanupAction()
	}
	if err != nil {
		s, _ := grpcstatus.FromError(err)
		m.recordContainerEvent(pod, container, "", v1.EventTypeWarning, events.FailedToCreateContainer, "Error: %v", s.Message())
		return s.Message(), ErrCreateContainerConfig
	}

	err = m.internalLifecycle.PreCreateContainer(pod, container, containerConfig)
	if err != nil {
		s, _ := grpcstatus.FromError(err)
		m.recordContainerEvent(pod, container, "", v1.EventTypeWarning, events.FailedToCreateContainer, "Internal PreCreateContainer hook failed: %v", s.Message())
		return s.Message(), ErrPreCreateHook
	}

	containerID, err := m.runtimeService.CreateContainer(ctx, podSandboxID, containerConfig, podSandboxConfig)
	if err != nil {
		s, _ := grpcstatus.FromError(err)
		m.recordContainerEvent(pod, container, containerID, v1.EventTypeWarning, events.FailedToCreateContainer, "Error: %v", s.Message())
		return s.Message(), ErrCreateContainer
	}
	err = m.internalLifecycle.PreStartContainer(pod, container, containerID)
	if err != nil {
		s, _ := grpcstatus.FromError(err)
		m.recordContainerEvent(pod, container, containerID, v1.EventTypeWarning, events.FailedToStartContainer, "Internal PreStartContainer hook failed: %v", s.Message())
		return s.Message(), ErrPreStartHook
	}
	m.recordContainerEvent(pod, container, containerID, v1.EventTypeNormal, events.CreatedContainer, "Created container: %v", container.Name)

	// Step 3: start the container.
	err = m.runtimeService.StartContainer(ctx, containerID)
	if err != nil {
		s, _ := grpcstatus.FromError(err)
		m.recordContainerEvent(pod, container, containerID, v1.EventTypeWarning, events.FailedToStartContainer, "Error: %v", s.Message())
		return s.Message(), kubecontainer.ErrRunContainer
	}
	m.recordContainerEvent(pod, container, containerID, v1.EventTypeNormal, events.StartedContainer, "Started container %v", container.Name)

	// Symlink container logs to the legacy container log location for cluster logging
	// support.
	// TODO(random-liu): Remove this after cluster logging supports CRI container log path.
	containerMeta := containerConfig.GetMetadata()
	sandboxMeta := podSandboxConfig.GetMetadata()
	legacySymlink := legacyLogSymlink(containerID, containerMeta.Name, sandboxMeta.Name,
		sandboxMeta.Namespace)
	containerLog := filepath.Join(podSandboxConfig.LogDirectory, containerConfig.LogPath)
	// only create legacy symlink if containerLog path exists (or the error is not IsNotExist).
	// Because if containerLog path does not exist, only dangling legacySymlink is created.
	// This dangling legacySymlink is later removed by container gc, so it does not make sense
	// to create it in the first place. it happens when journald logging driver is used with docker.
	if _, err := m.osInterface.Stat(containerLog); !os.IsNotExist(err) {
		if err := m.osInterface.Symlink(containerLog, legacySymlink); err != nil {
			klog.ErrorS(err, "Failed to create legacy symbolic link", "path", legacySymlink,
				"containerID", containerID, "containerLogPath", containerLog)
		}
	}

	// Step 4: execute the post start hook.
	if container.Lifecycle != nil && container.Lifecycle.PostStart != nil {
		kubeContainerID := kubecontainer.ContainerID{
			Type: m.runtimeName,
			ID:   containerID,
		}
		msg, handlerErr := m.runner.Run(ctx, kubeContainerID, pod, container, container.Lifecycle.PostStart)
		if handlerErr != nil {
			klog.ErrorS(handlerErr, "Failed to execute PostStartHook", "pod", klog.KObj(pod),
				"podUID", pod.UID, "containerName", container.Name, "containerID", kubeContainerID.String())
			// do not record the message in the event so that secrets won't leak from the server.
			m.recordContainerEvent(pod, container, kubeContainerID.ID, v1.EventTypeWarning, events.FailedPostStartHook, "PostStartHook failed")
			if err := m.killContainer(ctx, pod, kubeContainerID, container.Name, "FailedPostStartHook", reasonFailedPostStartHook, nil, nil); err != nil {
				klog.ErrorS(err, "Failed to kill container", "pod", klog.KObj(pod),
					"podUID", pod.UID, "containerName", container.Name, "containerID", kubeContainerID.String())
			}
			return msg, ErrPostStartHook
		}
	}

	return "", nil
}

// generateContainerConfig generates container config for kubelet runtime v1.
func (m *kubeGenericRuntimeManager) generateContainerConfig(ctx context.Context, container *v1.Container, pod *v1.Pod, restartCount int, podIP, imageRef string, podIPs []string, nsTarget *kubecontainer.ContainerID, imageVolumes kubecontainer.ImageVolumes) (*runtimeapi.ContainerConfig, func(), error) {
	opts, cleanupAction, err := m.runtimeHelper.GenerateRunContainerOptions(ctx, pod, container, podIP, podIPs, imageVolumes)
	if err != nil {
		return nil, nil, err
	}

	uid, username, err := m.getImageUser(ctx, container.Image)
	if err != nil {
		return nil, cleanupAction, err
	}

	// Verify RunAsNonRoot. Non-root verification only supports numeric user.
	if err := verifyRunAsNonRoot(pod, container, uid, username); err != nil {
		return nil, cleanupAction, err
	}

	command, args := kubecontainer.ExpandContainerCommandAndArgs(container, opts.Envs)
	logDir := BuildContainerLogsDirectory(m.podLogsDirectory, pod.Namespace, pod.Name, pod.UID, container.Name)
	err = m.osInterface.MkdirAll(logDir, 0755)
	if err != nil {
		return nil, cleanupAction, fmt.Errorf("create container log directory for container %s failed: %v", container.Name, err)
	}
	containerLogsPath := buildContainerLogsPath(container.Name, restartCount)
	restartCountUint32 := uint32(restartCount)
	config := &runtimeapi.ContainerConfig{
		Metadata: &runtimeapi.ContainerMetadata{
			Name:    container.Name,
			Attempt: restartCountUint32,
		},
		Image:       &runtimeapi.ImageSpec{Image: imageRef, UserSpecifiedImage: container.Image},
		Command:     command,
		Args:        args,
		WorkingDir:  container.WorkingDir,
		Labels:      newContainerLabels(container, pod),
		Annotations: newContainerAnnotations(container, pod, restartCount, opts),
		Devices:     makeDevices(opts),
		CDIDevices:  makeCDIDevices(opts),
		Mounts:      m.makeMounts(opts, container),
		LogPath:     containerLogsPath,
		Stdin:       container.Stdin,
		StdinOnce:   container.StdinOnce,
		Tty:         container.TTY,
	}

	// set platform specific configurations.
	if err := m.applyPlatformSpecificContainerConfig(config, container, pod, uid, username, nsTarget); err != nil {
		return nil, cleanupAction, err
	}

	// set environment variables
	envs := make([]*runtimeapi.KeyValue, len(opts.Envs))
	for idx := range opts.Envs {
		e := opts.Envs[idx]
		envs[idx] = &runtimeapi.KeyValue{
			Key:   e.Name,
			Value: e.Value,
		}
	}
	config.Envs = envs

	return config, cleanupAction, nil
}

func (m *kubeGenericRuntimeManager) updateContainerResources(pod *v1.Pod, container *v1.Container, containerID kubecontainer.ContainerID) error {
	containerResources := m.generateContainerResources(pod, container)
	if containerResources == nil {
		return fmt.Errorf("container %q updateContainerResources failed: cannot generate resources config", containerID.String())
	}
	ctx := context.Background()
	err := m.runtimeService.UpdateContainerResources(ctx, containerID.ID, containerResources)
	if err != nil {
		klog.ErrorS(err, "UpdateContainerResources failed", "container", containerID.String())
	}
	return err
}

// makeDevices generates container devices for kubelet runtime v1.
func makeDevices(opts *kubecontainer.RunContainerOptions) []*runtimeapi.Device {
	devices := make([]*runtimeapi.Device, len(opts.Devices))

	for idx := range opts.Devices {
		device := opts.Devices[idx]
		devices[idx] = &runtimeapi.Device{
			HostPath:      device.PathOnHost,
			ContainerPath: device.PathInContainer,
			Permissions:   device.Permissions,
		}
	}

	return devices
}

// makeCDIDevices generates container CDIDevices for kubelet runtime v1.
func makeCDIDevices(opts *kubecontainer.RunContainerOptions) []*runtimeapi.CDIDevice {
	devices := make([]*runtimeapi.CDIDevice, len(opts.CDIDevices))

	for i, device := range opts.CDIDevices {
		devices[i] = &runtimeapi.CDIDevice{
			Name: device.Name,
		}
	}

	return devices
}

// makeMounts generates container volume mounts for kubelet runtime v1.
func (m *kubeGenericRuntimeManager) makeMounts(opts *kubecontainer.RunContainerOptions, container *v1.Container) []*runtimeapi.Mount {
	volumeMounts := []*runtimeapi.Mount{}

	for idx := range opts.Mounts {
		v := opts.Mounts[idx]
		selinuxRelabel := v.SELinuxRelabel && selinux.GetEnabled()
		mount := &runtimeapi.Mount{
			HostPath:          v.HostPath,
			ContainerPath:     v.ContainerPath,
			Readonly:          v.ReadOnly,
			SelinuxRelabel:    selinuxRelabel,
			Propagation:       v.Propagation,
			RecursiveReadOnly: v.RecursiveReadOnly,
			Image:             v.Image,
		}

		volumeMounts = append(volumeMounts, mount)
	}

	// The reason we create and mount the log file in here (not in kubelet) is because
	// the file's location depends on the ID of the container, and we need to create and
	// mount the file before actually starting the container.
	if opts.PodContainerDir != "" && len(container.TerminationMessagePath) != 0 {
		// Because the PodContainerDir contains pod uid and container name which is unique enough,
		// here we just add a random id to make the path unique for different instances
		// of the same container.
		cid := makeUID()
		containerLogPath := filepath.Join(opts.PodContainerDir, cid)
		fs, err := m.osInterface.Create(containerLogPath)
		if err != nil {
			utilruntime.HandleError(fmt.Errorf("error on creating termination-log file %q: %v", containerLogPath, err))
		} else {
			fs.Close()

			// Chmod is needed because os.Create() ends up calling
			// open(2) to create the file, so the final mode used is "mode &
			// ~umask". But we want to make sure the specified mode is used
			// in the file no matter what the umask is.
			if err := m.osInterface.Chmod(containerLogPath, 0666); err != nil {
				utilruntime.HandleError(fmt.Errorf("unable to set termination-log file permissions %q: %v", containerLogPath, err))
			}

			// Volume Mounts fail on Windows if it is not of the form C:/
			containerLogPath = volumeutil.MakeAbsolutePath(goruntime.GOOS, containerLogPath)
			terminationMessagePath := volumeutil.MakeAbsolutePath(goruntime.GOOS, container.TerminationMessagePath)
			selinuxRelabel := selinux.GetEnabled()
			volumeMounts = append(volumeMounts, &runtimeapi.Mount{
				HostPath:       containerLogPath,
				ContainerPath:  terminationMessagePath,
				SelinuxRelabel: selinuxRelabel,
			})
		}
	}

	return volumeMounts
}

// getKubeletContainers lists containers managed by kubelet.
// The boolean parameter specifies whether returns all containers including
// those already exited and dead containers (used for garbage collection).
func (m *kubeGenericRuntimeManager) getKubeletContainers(ctx context.Context, allContainers bool) ([]*runtimeapi.Container, error) {
	filter := &runtimeapi.ContainerFilter{}
	if !allContainers {
		filter.State = &runtimeapi.ContainerStateValue{
			State: runtimeapi.ContainerState_CONTAINER_RUNNING,
		}
	}

	containers, err := m.runtimeService.ListContainers(ctx, filter)
	if err != nil {
		klog.ErrorS(err, "ListContainers failed")
		return nil, err
	}

	return containers, nil
}

// makeUID returns a randomly generated string.
func makeUID() string {
	return fmt.Sprintf("%08x", rand.Uint32())
}

// getTerminationMessage looks on the filesystem for the provided termination message path, returning a limited
// amount of those bytes, or returns true if the logs should be checked.
func getTerminationMessage(status *runtimeapi.ContainerStatus, terminationMessagePath string, fallbackToLogs bool) (string, bool) {
	if len(terminationMessagePath) == 0 {
		return "", fallbackToLogs
	}
	// Volume Mounts fail on Windows if it is not of the form C:/
	terminationMessagePath = volumeutil.MakeAbsolutePath(goruntime.GOOS, terminationMessagePath)
	for _, mount := range status.Mounts {
		if mount.ContainerPath != terminationMessagePath {
			continue
		}
		path := mount.HostPath
		data, _, err := tail.ReadAtMost(path, kubecontainer.MaxContainerTerminationMessageLength)
		if err != nil {
			if os.IsNotExist(err) {
				return "", fallbackToLogs
			}
			return fmt.Sprintf("Error on reading termination log %s: %v", path, err), false
		}
		return string(data), (fallbackToLogs && len(data) == 0)
	}
	return "", fallbackToLogs
}

// readLastStringFromContainerLogs attempts to read up to the max log length from the end of the CRI log represented
// by path. It reads up to max log lines.
func (m *kubeGenericRuntimeManager) readLastStringFromContainerLogs(path string) string {
	value := int64(kubecontainer.MaxContainerTerminationMessageLogLines)
	buf, _ := circbuf.NewBuffer(kubecontainer.MaxContainerTerminationMessageLogLength)
	if err := m.ReadLogs(context.Background(), path, "", &v1.PodLogOptions{TailLines: &value}, buf, buf); err != nil {
		return fmt.Sprintf("Error on reading termination message from logs: %v", err)
	}
	return buf.String()
}

func (m *kubeGenericRuntimeManager) convertToKubeContainerStatus(status *runtimeapi.ContainerStatus) (cStatus *kubecontainer.Status) {
	cStatus = toKubeContainerStatus(status, m.runtimeName)
	if status.State == runtimeapi.ContainerState_CONTAINER_EXITED {
		// Populate the termination message if needed.
		annotatedInfo := getContainerInfoFromAnnotations(status.Annotations)
		// If a container cannot even be started, it certainly does not have logs, so no need to fallbackToLogs.
		fallbackToLogs := annotatedInfo.TerminationMessagePolicy == v1.TerminationMessageFallbackToLogsOnError &&
			cStatus.ExitCode != 0 && cStatus.Reason != "ContainerCannotRun"
		tMessage, checkLogs := getTerminationMessage(status, annotatedInfo.TerminationMessagePath, fallbackToLogs)
		if checkLogs {
			tMessage = m.readLastStringFromContainerLogs(status.GetLogPath())
		}
		// Enrich the termination message written by the application is not empty
		if len(tMessage) != 0 {
			if len(cStatus.Message) != 0 {
				cStatus.Message += ": "
			}
			cStatus.Message += tMessage
		}
	}
	return cStatus
}

// getPodContainerStatuses gets all containers' statuses for the pod.
func (m *kubeGenericRuntimeManager) getPodContainerStatuses(ctx context.Context, uid kubetypes.UID, name, namespace string) ([]*kubecontainer.Status, error) {
	// Select all containers of the given pod.
	containers, err := m.runtimeService.ListContainers(ctx, &runtimeapi.ContainerFilter{
		LabelSelector: map[string]string{kubelettypes.KubernetesPodUIDLabel: string(uid)},
	})
	if err != nil {
		klog.ErrorS(err, "ListContainers error")
		return nil, err
	}

	statuses := []*kubecontainer.Status{}
	// TODO: optimization: set maximum number of containers per container name to examine.
	for _, c := range containers {
		resp, err := m.runtimeService.ContainerStatus(ctx, c.Id, false)
		// Between List (ListContainers) and check (ContainerStatus) another thread might remove a container, and that is normal.
		// The previous call (ListContainers) never fails due to a pod container not existing.
		// Therefore, this method should not either, but instead act as if the previous call failed,
		// which means the error should be ignored.
		if crierror.IsNotFound(err) {
			continue
		}
		if err != nil {
			// Merely log this here; GetPodStatus will actually report the error out.
			klog.V(4).InfoS("ContainerStatus return error", "containerID", c.Id, "err", err)
			return nil, err
		}
		status := resp.GetStatus()
		if status == nil {
			return nil, remote.ErrContainerStatusNil
		}
		cStatus := m.convertToKubeContainerStatus(status)
		statuses = append(statuses, cStatus)
	}

	sort.Sort(containerStatusByCreated(statuses))
	return statuses, nil
}

func toKubeContainerStatus(status *runtimeapi.ContainerStatus, runtimeName string) *kubecontainer.Status {
	annotatedInfo := getContainerInfoFromAnnotations(status.Annotations)
	labeledInfo := getContainerInfoFromLabels(status.Labels)
	var cStatusResources *kubecontainer.ContainerResources
	if utilfeature.DefaultFeatureGate.Enabled(features.InPlacePodVerticalScaling) {
		// If runtime reports cpu & memory resources info, add it to container status
		cStatusResources = toKubeContainerResources(status.Resources)
	}

	// Keep backwards compatibility to older runtimes, status.ImageId has been added in v1.30
	imageID := status.ImageRef
	if status.ImageId != "" {
		imageID = status.ImageId
	}

	var cStatusUser *kubecontainer.ContainerUser
	if utilfeature.DefaultFeatureGate.Enabled(features.SupplementalGroupsPolicy) {
		cStatusUser = toKubeContainerUser(status.User)
	}

	cStatus := &kubecontainer.Status{
		ID: kubecontainer.ContainerID{
			Type: runtimeName,
			ID:   status.Id,
		},
		Name:                labeledInfo.ContainerName,
		Image:               status.Image.Image,
		ImageID:             imageID,
		ImageRef:            status.ImageRef,
		ImageRuntimeHandler: status.Image.RuntimeHandler,
		Hash:                annotatedInfo.Hash,
		RestartCount:        annotatedInfo.RestartCount,
		State:               toKubeContainerState(status.State),
		CreatedAt:           time.Unix(0, status.CreatedAt),
		Resources:           cStatusResources,
		User:                cStatusUser,
	}

	if status.State != runtimeapi.ContainerState_CONTAINER_CREATED {
		// If container is not in the created state, we have tried and
		// started the container. Set the StartedAt time.
		cStatus.StartedAt = time.Unix(0, status.StartedAt)
	}
	if status.State == runtimeapi.ContainerState_CONTAINER_EXITED {
		cStatus.Reason = status.Reason
		cStatus.Message = status.Message
		cStatus.ExitCode = int(status.ExitCode)
		cStatus.FinishedAt = time.Unix(0, status.FinishedAt)
	}

	for _, mount := range status.Mounts {
		cStatus.Mounts = append(cStatus.Mounts, kubecontainer.Mount{
			HostPath:          mount.HostPath,
			ContainerPath:     mount.ContainerPath,
			ReadOnly:          mount.Readonly,
			RecursiveReadOnly: mount.RecursiveReadOnly,
			SELinuxRelabel:    mount.SelinuxRelabel,
			Propagation:       mount.Propagation,
			Image:             mount.Image,
		})
	}
	return cStatus
}

// executePreStopHook runs the pre-stop lifecycle hooks if applicable and returns the duration it takes.
func (m *kubeGenericRuntimeManager) executePreStopHook(ctx context.Context, pod *v1.Pod, containerID kubecontainer.ContainerID, containerSpec *v1.Container, gracePeriod int64) int64 {
	klog.V(3).InfoS("Running preStop hook", "pod", klog.KObj(pod), "podUID", pod.UID, "containerName", containerSpec.Name, "containerID", containerID.String())

	start := metav1.Now()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer utilruntime.HandleCrash()
		if _, err := m.runner.Run(ctx, containerID, pod, containerSpec, containerSpec.Lifecycle.PreStop); err != nil {
			klog.ErrorS(err, "PreStop hook failed", "pod", klog.KObj(pod), "podUID", pod.UID,
				"containerName", containerSpec.Name, "containerID", containerID.String())
			// do not record the message in the event so that secrets won't leak from the server.
			m.recordContainerEvent(pod, containerSpec, containerID.ID, v1.EventTypeWarning, events.FailedPreStopHook, "PreStopHook failed")
		}
	}()

	select {
	case <-time.After(time.Duration(gracePeriod) * time.Second):
		klog.V(2).InfoS("PreStop hook not completed in grace period", "pod", klog.KObj(pod), "podUID", pod.UID,
			"containerName", containerSpec.Name, "containerID", containerID.String(), "gracePeriod", gracePeriod)
	case <-done:
		klog.V(3).InfoS("PreStop hook completed", "pod", klog.KObj(pod), "podUID", pod.UID,
			"containerName", containerSpec.Name, "containerID", containerID.String())
	}

	return int64(metav1.Now().Sub(start.Time).Seconds())
}

// restoreSpecsFromContainerLabels restores all information needed for killing a container. In some
// case we may not have pod and container spec when killing a container, e.g. pod is deleted during
// kubelet restart.
// To solve this problem, we've already written necessary information into container labels. Here we
// just need to retrieve them from container labels and restore the specs.
// TODO(random-liu): Add a node e2e test to test this behaviour.
// TODO(random-liu): Change the lifecycle handler to just accept information needed, so that we can
// just pass the needed function not create the fake object.
func (m *kubeGenericRuntimeManager) restoreSpecsFromContainerLabels(ctx context.Context, containerID kubecontainer.ContainerID) (*v1.Pod, *v1.Container, error) {
	var pod *v1.Pod
	var container *v1.Container
	resp, err := m.runtimeService.ContainerStatus(ctx, containerID.ID, false)
	if err != nil {
		return nil, nil, err
	}
	s := resp.GetStatus()
	if s == nil {
		return nil, nil, remote.ErrContainerStatusNil
	}

	l := getContainerInfoFromLabels(s.Labels)
	a := getContainerInfoFromAnnotations(s.Annotations)
	// Notice that the followings are not full spec. The container killing code should not use
	// un-restored fields.
	pod = &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			UID:                        l.PodUID,
			Name:                       l.PodName,
			Namespace:                  l.PodNamespace,
			DeletionGracePeriodSeconds: a.PodDeletionGracePeriod,
		},
		Spec: v1.PodSpec{
			TerminationGracePeriodSeconds: a.PodTerminationGracePeriod,
		},
	}
	container = &v1.Container{
		Name:                   l.ContainerName,
		Ports:                  a.ContainerPorts,
		TerminationMessagePath: a.TerminationMessagePath,
	}
	if a.PreStopHandler != nil {
		container.Lifecycle = &v1.Lifecycle{
			PreStop: a.PreStopHandler,
		}
	}
	return pod, container, nil
}

// killContainer kills a container through the following steps:
// * Run the pre-stop lifecycle hooks (if applicable).
// * Stop the container.
func (m *kubeGenericRuntimeManager) killContainer(ctx context.Context, pod *v1.Pod, containerID kubecontainer.ContainerID, containerName string, message string, reason containerKillReason, gracePeriodOverride *int64, ordering *terminationOrdering) error {
	var containerSpec *v1.Container
	if pod != nil {
		if containerSpec = kubecontainer.GetContainerSpec(pod, containerName); containerSpec == nil {
			return fmt.Errorf("failed to get containerSpec %q (id=%q) in pod %q when killing container for reason %q",
				containerName, containerID.String(), format.Pod(pod), message)
		}
	} else {
		// Restore necessary information if one of the specs is nil.
		restoredPod, restoredContainer, err := m.restoreSpecsFromContainerLabels(ctx, containerID)
		if err != nil {
			return err
		}
		pod, containerSpec = restoredPod, restoredContainer
	}

	// From this point, pod and container must be non-nil.
	gracePeriod := setTerminationGracePeriod(pod, containerSpec, containerName, containerID, reason)

	if len(message) == 0 {
		message = fmt.Sprintf("Stopping container %s", containerSpec.Name)
	}
	m.recordContainerEvent(pod, containerSpec, containerID.ID, v1.EventTypeNormal, events.KillingContainer, "%v", message)

	if gracePeriodOverride != nil {
		gracePeriod = *gracePeriodOverride
		klog.V(3).InfoS("Killing container with a grace period override", "pod", klog.KObj(pod), "podUID", pod.UID,
			"containerName", containerName, "containerID", containerID.String(), "gracePeriod", gracePeriod)
	}

	// Run the pre-stop lifecycle hooks if applicable and if there is enough time to run it
	if containerSpec.Lifecycle != nil && containerSpec.Lifecycle.PreStop != nil && gracePeriod > 0 {
		gracePeriod = gracePeriod - m.executePreStopHook(ctx, pod, containerID, containerSpec, gracePeriod)
	}

	// if we care about termination ordering, then wait for this container's turn to exit if there is
	// time remaining
	if ordering != nil && gracePeriod > 0 {
		// grace period is only in seconds, so the time we've waited gets truncated downward
		gracePeriod -= int64(ordering.waitForTurn(containerName, gracePeriod))
	}

	// always give containers a minimal shutdown window to avoid unnecessary SIGKILLs
	if gracePeriod < minimumGracePeriodInSeconds {
		gracePeriod = minimumGracePeriodInSeconds
	}

	klog.V(2).InfoS("Killing container with a grace period", "pod", klog.KObj(pod), "podUID", pod.UID,
		"containerName", containerName, "containerID", containerID.String(), "gracePeriod", gracePeriod)

	err := m.runtimeService.StopContainer(ctx, containerID.ID, gracePeriod)
	if err != nil && !crierror.IsNotFound(err) {
		klog.ErrorS(err, "Container termination failed with gracePeriod", "pod", klog.KObj(pod), "podUID", pod.UID,
			"containerName", containerName, "containerID", containerID.String(), "gracePeriod", gracePeriod)
		return err
	}
	klog.V(3).InfoS("Container exited normally", "pod", klog.KObj(pod), "podUID", pod.UID,
		"containerName", containerName, "containerID", containerID.String())

	if ordering != nil {
		ordering.containerTerminated(containerName)
	}

	return nil
}

// killContainersWithSyncResult kills all pod's containers with sync results.
func (m *kubeGenericRuntimeManager) killContainersWithSyncResult(ctx context.Context, pod *v1.Pod, runningPod kubecontainer.Pod, gracePeriodOverride *int64) (syncResults []*kubecontainer.SyncResult) {
	containerResults := make(chan *kubecontainer.SyncResult, len(runningPod.Containers))
	wg := sync.WaitGroup{}

	wg.Add(len(runningPod.Containers))
	var termOrdering *terminationOrdering
	if types.HasRestartableInitContainer(pod) {
		var runningContainerNames []string
		for _, container := range runningPod.Containers {
			runningContainerNames = append(runningContainerNames, container.Name)
		}
		termOrdering = newTerminationOrdering(pod, runningContainerNames)
	}
	for _, container := range runningPod.Containers {
		go func(container *kubecontainer.Container) {
			defer utilruntime.HandleCrash()
			defer wg.Done()

			killContainerResult := kubecontainer.NewSyncResult(kubecontainer.KillContainer, container.Name)
			if err := m.killContainer(ctx, pod, container.ID, container.Name, "", reasonUnknown, gracePeriodOverride, termOrdering); err != nil {
				killContainerResult.Fail(kubecontainer.ErrKillContainer, err.Error())
				// Use runningPod for logging as the pod passed in could be *nil*.
				klog.ErrorS(err, "Kill container failed", "pod", klog.KRef(runningPod.Namespace, runningPod.Name), "podUID", runningPod.ID,
					"containerName", container.Name, "containerID", container.ID)
			}
			containerResults <- killContainerResult
		}(container)
	}
	wg.Wait()
	close(containerResults)

	for containerResult := range containerResults {
		syncResults = append(syncResults, containerResult)
	}
	return
}

// pruneInitContainersBeforeStart ensures that before we begin creating init
// containers, we have reduced the number of outstanding init containers still
// present. This reduces load on the container garbage collector by only
// preserving the most recent terminated init container.
func (m *kubeGenericRuntimeManager) pruneInitContainersBeforeStart(ctx context.Context, pod *v1.Pod, podStatus *kubecontainer.PodStatus) {
	// only the last execution of each init container should be preserved, and only preserve it if it is in the
	// list of init containers to keep.
	initContainerNames := sets.New[string]()
	for _, container := range pod.Spec.InitContainers {
		initContainerNames.Insert(container.Name)
	}
	for name := range initContainerNames {
		count := 0
		for _, status := range podStatus.ContainerStatuses {
			if status.Name != name ||
				(status.State != kubecontainer.ContainerStateExited &&
					status.State != kubecontainer.ContainerStateUnknown) {
				continue
			}
			// Remove init containers in unknown state. It should have
			// been stopped before pruneInitContainersBeforeStart is
			// called.
			count++
			// keep the first init container for this name
			if count == 1 {
				continue
			}
			// prune all other init containers that match this container name
			klog.V(4).InfoS("Removing init container", "containerName", status.Name, "containerID", status.ID.ID, "count", count)
			if err := m.removeContainer(ctx, status.ID.ID); err != nil {
				utilruntime.HandleError(fmt.Errorf("failed to remove pod init container %q: %v; Skipping pod %q", status.Name, err, format.Pod(pod)))
				continue
			}
		}
	}
}

// Remove all init containers. Note that this function does not check the state
// of the container because it assumes all init containers have been stopped
// before the call happens.
func (m *kubeGenericRuntimeManager) purgeInitContainers(ctx context.Context, pod *v1.Pod, podStatus *kubecontainer.PodStatus) {
	initContainerNames := sets.New[string]()
	for _, container := range pod.Spec.InitContainers {
		initContainerNames.Insert(container.Name)
	}
	for name := range initContainerNames {
		count := 0
		for _, status := range podStatus.ContainerStatuses {
			if status.Name != name {
				continue
			}
			count++
			// Purge all init containers that match this container name
			klog.V(4).InfoS("Removing init container", "containerName", status.Name, "containerID", status.ID.ID, "count", count)
			if err := m.removeContainer(ctx, status.ID.ID); err != nil {
				utilruntime.HandleError(fmt.Errorf("failed to remove pod init container %q: %v; Skipping pod %q", status.Name, err, format.Pod(pod)))
				continue
			}
		}
	}
}

// findNextInitContainerToRun returns the status of the last failed container, the
// index of next init container to start, or done if there are no further init containers.
// Status is only returned if an init container is failed, in which case next will
// point to the current container.
// TODO: Remove this function as this is a subset of the
// computeInitContainerActions.
func findNextInitContainerToRun(pod *v1.Pod, podStatus *kubecontainer.PodStatus) (status *kubecontainer.Status, next *v1.Container, done bool) {
	if len(pod.Spec.InitContainers) == 0 {
		return nil, nil, true
	}

	// If any of the main containers have status and are Running, then all init containers must
	// have been executed at some point in the past.  However, they could have been removed
	// from the container runtime now, and if we proceed, it would appear as if they
	// never ran and will re-execute improperly.
	for i := range pod.Spec.Containers {
		container := &pod.Spec.Containers[i]
		status := podStatus.FindContainerStatusByName(container.Name)
		if status != nil && status.State == kubecontainer.ContainerStateRunning {
			return nil, nil, true
		}
	}

	// If there are failed containers, return the status of the last failed one.
	for i := len(pod.Spec.InitContainers) - 1; i >= 0; i-- {
		container := &pod.Spec.InitContainers[i]
		status := podStatus.FindContainerStatusByName(container.Name)
		if status != nil && isInitContainerFailed(status) {
			return status, container, false
		}
	}

	// There are no failed containers now.
	for i := len(pod.Spec.InitContainers) - 1; i >= 0; i-- {
		container := &pod.Spec.InitContainers[i]
		status := podStatus.FindContainerStatusByName(container.Name)
		if status == nil {
			continue
		}

		// container is still running, return not done.
		if status.State == kubecontainer.ContainerStateRunning {
			return nil, nil, false
		}

		if status.State == kubecontainer.ContainerStateExited {
			// all init containers successful
			if i == (len(pod.Spec.InitContainers) - 1) {
				return nil, nil, true
			}

			// all containers up to i successful, go to i+1
			return nil, &pod.Spec.InitContainers[i+1], false
		}
	}

	return nil, &pod.Spec.InitContainers[0], false
}

// hasAnyRegularContainerCreated returns true if any regular container has been
// created, which indicates all init containers have been initialized.
func hasAnyRegularContainerCreated(pod *v1.Pod, podStatus *kubecontainer.PodStatus) bool {
	for _, container := range pod.Spec.Containers {
		status := podStatus.FindContainerStatusByName(container.Name)
		if status == nil {
			continue
		}
		switch status.State {
		case kubecontainer.ContainerStateCreated,
			kubecontainer.ContainerStateRunning,
			kubecontainer.ContainerStateExited:
			return true
		default:
			// Ignore other states
		}
	}
	return false
}

// computeInitContainerActions sets the actions on the given changes that need
// to be taken for the init containers. This includes actions to initialize the
// init containers and actions to keep restartable init containers running.
// computeInitContainerActions returns true if pod has been initialized.
//
// The actions include:
// - Start the first init container that has not been started.
// - Restart all restartable init containers that have started but are not running.
// - Kill the restartable init containers that are not alive or started.
//
// Note that this is a function for the SidecarContainers feature.
// Please sync with the findNextInitContainerToRun function if any changes are
// made, as either this or that function will be called.
func (m *kubeGenericRuntimeManager) computeInitContainerActions(pod *v1.Pod, podStatus *kubecontainer.PodStatus, changes *podActions) bool {
	if len(pod.Spec.InitContainers) == 0 {
		return true
	}

	// If any of the main containers have status and are Running, then all init containers must
	// have been executed at some point in the past.  However, they could have been removed
	// from the container runtime now, and if we proceed, it would appear as if they
	// never ran and will re-execute improperly except for the restartable init containers.
	podHasInitialized := false
	for _, container := range pod.Spec.Containers {
		status := podStatus.FindContainerStatusByName(container.Name)
		if status == nil {
			continue
		}
		switch status.State {
		case kubecontainer.ContainerStateCreated,
			kubecontainer.ContainerStateRunning:
			podHasInitialized = true
		case kubecontainer.ContainerStateExited:
			// This is a workaround for the issue that the kubelet cannot
			// differentiate the container statuses of the previous podSandbox
			// from the current one.
			// If the node is rebooted, all containers will be in the exited
			// state and the kubelet will try to recreate a new podSandbox.
			// In this case, the kubelet should not mistakenly think that
			// the newly created podSandbox has been initialized.
		default:
			// Ignore other states
		}
		if podHasInitialized {
			break
		}
	}

	// isPreviouslyInitialized indicates if the current init container is
	// previously initialized.
	isPreviouslyInitialized := podHasInitialized
	restartOnFailure := shouldRestartOnFailure(pod)

	// Note that we iterate through the init containers in reverse order to find
	// the next init container to run, as the completed init containers may get
	// removed from container runtime for various reasons. Therefore the kubelet
	// should rely on the minimal number of init containers - the last one.
	//
	// Once we find the next init container to run, iterate through the rest to
	// find the restartable init containers to restart.
	for i := len(pod.Spec.InitContainers) - 1; i >= 0; i-- {
		container := &pod.Spec.InitContainers[i]
		status := podStatus.FindContainerStatusByName(container.Name)
		klog.V(4).InfoS("Computing init container action", "pod", klog.KObj(pod), "container", container.Name, "status", status)
		if status == nil {
			// If the container is previously initialized but its status is not
			// found, it means its last status is removed for some reason.
			// Restart it if it is a restartable init container.
			if isPreviouslyInitialized && podutil.IsRestartableInitContainer(container) {
				changes.InitContainersToStart = append(changes.InitContainersToStart, i)
			}
			continue
		}

		if isPreviouslyInitialized && !podutil.IsRestartableInitContainer(container) {
			// after initialization, only restartable init containers need to be kept
			// running
			continue
		}

		switch status.State {
		case kubecontainer.ContainerStateCreated:
			// The main sync loop should have created and started the container
			// in one step. If the init container is in the 'created' state,
			// it is likely that the container runtime failed to start it. To
			// prevent the container from getting stuck in the 'created' state,
			// restart it.
			changes.InitContainersToStart = append(changes.InitContainersToStart, i)

		case kubecontainer.ContainerStateRunning:
			if !podutil.IsRestartableInitContainer(container) {
				break
			}

			if podutil.IsRestartableInitContainer(container) {
				if container.StartupProbe != nil {
					startup, found := m.startupManager.Get(status.ID)
					if !found {
						// If the startup probe has not been run, wait for it.
						break
					}
					if startup != proberesults.Success {
						if startup == proberesults.Failure {
							// If the restartable init container failed the startup probe,
							// restart it.
							changes.ContainersToKill[status.ID] = containerToKillInfo{
								name:      container.Name,
								container: container,
								message:   fmt.Sprintf("Init container %s failed startup probe", container.Name),
								reason:    reasonStartupProbe,
							}
							changes.InitContainersToStart = append(changes.InitContainersToStart, i)
						}
						break
					}
				}

				klog.V(4).InfoS("Init container has been initialized", "pod", klog.KObj(pod), "container", container.Name)
				if i == (len(pod.Spec.InitContainers) - 1) {
					podHasInitialized = true
				} else if !isPreviouslyInitialized {
					// this init container is initialized for the first time, start the next one
					changes.InitContainersToStart = append(changes.InitContainersToStart, i+1)
				}

				// Restart running sidecar containers which have had their definition changed.
				if _, _, changed := containerChanged(container, status); changed {
					changes.ContainersToKill[status.ID] = containerToKillInfo{
						name:      container.Name,
						container: container,
						message:   fmt.Sprintf("Init container %s definition changed", container.Name),
						reason:    "",
					}
					changes.InitContainersToStart = append(changes.InitContainersToStart, i)
					break
				}

				// A restartable init container does not have to take into account its
				// liveness probe when it determines to start the next init container.
				if container.LivenessProbe != nil {
					liveness, found := m.livenessManager.Get(status.ID)
					if !found {
						// If the liveness probe has not been run, wait for it.
						break
					}
					if liveness == proberesults.Failure {
						// If the restartable init container failed the liveness probe,
						// restart it.
						changes.ContainersToKill[status.ID] = containerToKillInfo{
							name:      container.Name,
							container: container,
							message:   fmt.Sprintf("Init container %s failed liveness probe", container.Name),
							reason:    reasonLivenessProbe,
						}
						changes.InitContainersToStart = append(changes.InitContainersToStart, i)
						// The container is restarting, so no other actions need to be taken.
						break
					}
				}

				if IsInPlacePodVerticalScalingAllowed(pod) && !m.computePodResizeAction(pod, i, true, status, changes) {
					// computePodResizeAction updates 'changes' if resize policy requires restarting this container
					break
				}
			} else { // init container
				// nothing do to but wait for it to finish
				break
			}

		// If the init container failed and the restart policy is Never, the pod is terminal.
		// Otherwise, restart the init container.
		case kubecontainer.ContainerStateExited:
			if podutil.IsRestartableInitContainer(container) {
				changes.InitContainersToStart = append(changes.InitContainersToStart, i)
			} else { // init container
				if isInitContainerFailed(status) {
					if !restartOnFailure {
						changes.KillPod = true
						changes.InitContainersToStart = nil
						return false
					}
					changes.InitContainersToStart = append(changes.InitContainersToStart, i)
					break
				}

				klog.V(4).InfoS("Init container has been initialized", "pod", klog.KObj(pod), "container", container.Name)
				if i == (len(pod.Spec.InitContainers) - 1) {
					podHasInitialized = true
				} else {
					// this init container is initialized for the first time, start the next one
					changes.InitContainersToStart = append(changes.InitContainersToStart, i+1)
				}
			}

		default: // kubecontainer.ContainerStatusUnknown or other unknown states
			if podutil.IsRestartableInitContainer(container) {
				// If the restartable init container is in unknown state, restart it.
				changes.ContainersToKill[status.ID] = containerToKillInfo{
					name:      container.Name,
					container: container,
					message: fmt.Sprintf("Init container is in %q state, try killing it before restart",
						status.State),
					reason: reasonUnknown,
				}
				changes.InitContainersToStart = append(changes.InitContainersToStart, i)
			} else { // init container
				if !isInitContainerFailed(status) {
					klog.V(4).InfoS("This should not happen, init container is in unknown state but not failed", "pod", klog.KObj(pod), "containerStatus", status)
				}

				if !restartOnFailure {
					changes.KillPod = true
					changes.InitContainersToStart = nil
					return false
				}

				// If the init container is in unknown state, restart it.
				changes.ContainersToKill[status.ID] = containerToKillInfo{
					name:      container.Name,
					container: container,
					message: fmt.Sprintf("Init container is in %q state, try killing it before restart",
						status.State),
					reason: reasonUnknown,
				}
				changes.InitContainersToStart = append(changes.InitContainersToStart, i)
			}
		}

		if !isPreviouslyInitialized {
			// the one before this init container has been initialized
			isPreviouslyInitialized = true
		}
	}

	// this means no init containers have been started,
	// start the first one
	if !isPreviouslyInitialized {
		changes.InitContainersToStart = append(changes.InitContainersToStart, 0)
	}

	// reverse the InitContainersToStart, as the above loop iterated through the
	// init containers backwards, but we want to start them as per the order in
	// the pod spec.
	l := len(changes.InitContainersToStart)
	for i := 0; i < l/2; i++ {
		changes.InitContainersToStart[i], changes.InitContainersToStart[l-1-i] =
			changes.InitContainersToStart[l-1-i], changes.InitContainersToStart[i]
	}

	return podHasInitialized
}

// GetContainerLogs returns logs of a specific container.
func (m *kubeGenericRuntimeManager) GetContainerLogs(ctx context.Context, pod *v1.Pod, containerID kubecontainer.ContainerID, logOptions *v1.PodLogOptions, stdout, stderr io.Writer) (err error) {
	resp, err := m.runtimeService.ContainerStatus(ctx, containerID.ID, false)
	if err != nil {
		klog.V(4).InfoS("Failed to get container status", "containerID", containerID.String(), "err", err)
		return fmt.Errorf("unable to retrieve container logs for %v", containerID.String())
	}
	status := resp.GetStatus()
	if status == nil {
		return remote.ErrContainerStatusNil
	}
	return m.ReadLogs(ctx, status.GetLogPath(), containerID.ID, logOptions, stdout, stderr)
}

// GetExec gets the endpoint the runtime will serve the exec request from.
func (m *kubeGenericRuntimeManager) GetExec(ctx context.Context, id kubecontainer.ContainerID, cmd []string, stdin, stdout, stderr, tty bool) (*url.URL, error) {
	req := &runtimeapi.ExecRequest{
		ContainerId: id.ID,
		Cmd:         cmd,
		Tty:         tty,
		Stdin:       stdin,
		Stdout:      stdout,
		Stderr:      stderr,
	}
	resp, err := m.runtimeService.Exec(ctx, req)
	if err != nil {
		return nil, err
	}

	return url.Parse(resp.Url)
}

// GetAttach gets the endpoint the runtime will serve the attach request from.
func (m *kubeGenericRuntimeManager) GetAttach(ctx context.Context, id kubecontainer.ContainerID, stdin, stdout, stderr, tty bool) (*url.URL, error) {
	req := &runtimeapi.AttachRequest{
		ContainerId: id.ID,
		Stdin:       stdin,
		Stdout:      stdout,
		Stderr:      stderr,
		Tty:         tty,
	}
	resp, err := m.runtimeService.Attach(ctx, req)
	if err != nil {
		return nil, err
	}
	return url.Parse(resp.Url)
}

// RunInContainer synchronously executes the command in the container, and returns the output.
func (m *kubeGenericRuntimeManager) RunInContainer(ctx context.Context, id kubecontainer.ContainerID, cmd []string, timeout time.Duration) ([]byte, error) {
	stdout, stderr, err := m.runtimeService.ExecSync(ctx, id.ID, cmd, timeout)
	// NOTE(tallclair): This does not correctly interleave stdout & stderr, but should be sufficient
	// for logging purposes. A combined output option will need to be added to the ExecSyncRequest
	// if more precise output ordering is ever required.
	return append(stdout, stderr...), err
}

// removeContainer removes the container and the container logs.
// Notice that we remove the container logs first, so that container will not be removed if
// container logs are failed to be removed, and kubelet will retry this later. This guarantees
// that container logs to be removed with the container.
// Notice that we assume that the container should only be removed in non-running state, and
// it will not write container logs anymore in that state.
func (m *kubeGenericRuntimeManager) removeContainer(ctx context.Context, containerID string) error {
	klog.V(4).InfoS("Removing container", "containerID", containerID)
	// Call internal container post-stop lifecycle hook.
	if err := m.internalLifecycle.PostStopContainer(containerID); err != nil {
		return err
	}

	// Remove the container log.
	// TODO: Separate log and container lifecycle management.
	if err := m.removeContainerLog(ctx, containerID); err != nil {
		return err
	}
	// Remove the container.
	return m.runtimeService.RemoveContainer(ctx, containerID)
}

// removeContainerLog removes the container log.
func (m *kubeGenericRuntimeManager) removeContainerLog(ctx context.Context, containerID string) error {
	// Use log manager to remove rotated logs.
	err := m.logManager.Clean(ctx, containerID)
	if err != nil {
		return err
	}

	resp, err := m.runtimeService.ContainerStatus(ctx, containerID, false)
	if err != nil {
		return fmt.Errorf("failed to get container status %q: %v", containerID, err)
	}
	status := resp.GetStatus()
	if status == nil {
		return remote.ErrContainerStatusNil
	}
	// Remove the legacy container log symlink.
	// TODO(random-liu): Remove this after cluster logging supports CRI container log path.
	labeledInfo := getContainerInfoFromLabels(status.Labels)
	legacySymlink := legacyLogSymlink(containerID, labeledInfo.ContainerName, labeledInfo.PodName,
		labeledInfo.PodNamespace)
	if err := m.osInterface.Remove(legacySymlink); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove container %q log legacy symbolic link %q: %v",
			containerID, legacySymlink, err)
	}
	return nil
}

// DeleteContainer removes a container.
func (m *kubeGenericRuntimeManager) DeleteContainer(ctx context.Context, containerID kubecontainer.ContainerID) error {
	return m.removeContainer(ctx, containerID.ID)
}

// setTerminationGracePeriod determines the grace period to use when killing a container
func setTerminationGracePeriod(pod *v1.Pod, containerSpec *v1.Container, containerName string, containerID kubecontainer.ContainerID, reason containerKillReason) int64 {
	gracePeriod := int64(minimumGracePeriodInSeconds)
	switch {
	case pod.DeletionGracePeriodSeconds != nil:
		return *pod.DeletionGracePeriodSeconds
	case pod.Spec.TerminationGracePeriodSeconds != nil:
		switch reason {
		case reasonStartupProbe:
			if isProbeTerminationGracePeriodSecondsSet(pod, containerSpec, containerSpec.StartupProbe, containerName, containerID, "StartupProbe") {
				return *containerSpec.StartupProbe.TerminationGracePeriodSeconds
			}
		case reasonLivenessProbe:
			if isProbeTerminationGracePeriodSecondsSet(pod, containerSpec, containerSpec.LivenessProbe, containerName, containerID, "LivenessProbe") {
				return *containerSpec.LivenessProbe.TerminationGracePeriodSeconds
			}
		}
		return *pod.Spec.TerminationGracePeriodSeconds
	}
	return gracePeriod
}

func isProbeTerminationGracePeriodSecondsSet(pod *v1.Pod, containerSpec *v1.Container, probe *v1.Probe, containerName string, containerID kubecontainer.ContainerID, probeType string) bool {
	if probe != nil && probe.TerminationGracePeriodSeconds != nil {
		if *probe.TerminationGracePeriodSeconds > *pod.Spec.TerminationGracePeriodSeconds {
			klog.V(4).InfoS("Using probe-level grace period that is greater than the pod-level grace period", "pod", klog.KObj(pod), "podUID", pod.UID, "containerName", containerName, "containerID", containerID.String(), "probeType", probeType, "probeGracePeriod", *probe.TerminationGracePeriodSeconds, "podGracePeriod", *pod.Spec.TerminationGracePeriodSeconds)
		}
		return true
	}
	return false
}
