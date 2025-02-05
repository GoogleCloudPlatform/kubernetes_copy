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

package kubelet

import (
	"bytes"
	"context"
	goerrors "errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	utilvalidation "k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/version"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
	"k8s.io/klog/v2"
	"k8s.io/kubelet/pkg/cri/streaming/portforward"
	remotecommandserver "k8s.io/kubelet/pkg/cri/streaming/remotecommand"
	podutil "k8s.io/kubernetes/pkg/api/v1/pod"
	"k8s.io/kubernetes/pkg/api/v1/resource"
	podshelper "k8s.io/kubernetes/pkg/apis/core/pods"
	v1helper "k8s.io/kubernetes/pkg/apis/core/v1/helper"
	v1qos "k8s.io/kubernetes/pkg/apis/core/v1/helper/qos"
	"k8s.io/kubernetes/pkg/features"
	"k8s.io/kubernetes/pkg/fieldpath"
	"k8s.io/kubernetes/pkg/kubelet/cm"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
	"k8s.io/kubernetes/pkg/kubelet/envvars"
	"k8s.io/kubernetes/pkg/kubelet/images"
	"k8s.io/kubernetes/pkg/kubelet/metrics"
	"k8s.io/kubernetes/pkg/kubelet/status"
	kubetypes "k8s.io/kubernetes/pkg/kubelet/types"
	"k8s.io/kubernetes/pkg/kubelet/util/format"
	utilfs "k8s.io/kubernetes/pkg/util/filesystem"
	utilkernel "k8s.io/kubernetes/pkg/util/kernel"
	utilpod "k8s.io/kubernetes/pkg/util/pod"
	volumeutil "k8s.io/kubernetes/pkg/volume/util"
	"k8s.io/kubernetes/pkg/volume/util/hostutil"
	"k8s.io/kubernetes/pkg/volume/util/subpath"
	"k8s.io/kubernetes/pkg/volume/util/volumepathhandler"
	volumevalidation "k8s.io/kubernetes/pkg/volume/validation"
	"k8s.io/kubernetes/third_party/forked/golang/expansion"
	utilnet "k8s.io/utils/net"
)

const (
	managedHostsHeader                = "# Kubernetes-managed hosts file.\n"
	managedHostsHeaderWithHostNetwork = "# Kubernetes-managed hosts file (host network).\n"
)

// Container state reason list
const (
	PodInitializing   = "PodInitializing"
	ContainerCreating = "ContainerCreating"

	kubeletUser = "kubelet"
)

// parseGetSubIdsOutput parses the output from the `getsubids` tool, which is used to query subordinate user or group ID ranges for
// a given user or group. getsubids produces a line for each mapping configured.
// Here we expect that there is a single mapping, and the same values are used for the subordinate user and group ID ranges.
// The output is something like:
// $ getsubids kubelet
// 0: kubelet 65536 2147483648
// $ getsubids -g kubelet
// 0: kubelet 65536 2147483648
func parseGetSubIdsOutput(input string) (uint32, uint32, error) {
	lines := strings.Split(strings.Trim(input, "\n"), "\n")
	if len(lines) != 1 {
		return 0, 0, fmt.Errorf("error parsing line %q: it must contain only one line", input)
	}

	parts := strings.Fields(lines[0])
	if len(parts) != 4 {
		return 0, 0, fmt.Errorf("invalid line %q", input)
	}

	// Parsing the numbers
	num1, err := strconv.ParseUint(parts[2], 10, 32)
	if err != nil {
		return 0, 0, fmt.Errorf("error parsing line %q: %w", input, err)
	}

	num2, err := strconv.ParseUint(parts[3], 10, 32)
	if err != nil {
		return 0, 0, fmt.Errorf("error parsing line %q: %w", input, err)
	}

	return uint32(num1), uint32(num2), nil
}

// getKubeletMappings returns the range of IDs that can be used to configure user namespaces.
// If subordinate user or group ID ranges are specified for the kubelet user and the getsubids tool
// is installed, then the single mapping specified both for user and group IDs will be used.
// If the tool is not installed, or there are no IDs configured, the default mapping is returned.
// The default mapping includes the entire IDs range except IDs below 65536.
func (kl *Kubelet) getKubeletMappings() (uint32, uint32, error) {
	// default mappings to return if there is no specific configuration
	const defaultFirstID = 1 << 16
	const defaultLen = 1<<32 - defaultFirstID

	if !utilfeature.DefaultFeatureGate.Enabled(features.UserNamespacesSupport) {
		return defaultFirstID, defaultLen, nil
	} else {
		kernelVersion, err := utilkernel.GetVersion()
		if err != nil {
			return 0, 0, fmt.Errorf("failed to get kernel version, unable to determine if feature %s can be supported : %w",
				features.UserNamespacesSupport, err)
		}
		if kernelVersion != nil && !kernelVersion.AtLeast(version.MustParseGeneric(utilkernel.UserNamespacesSupportKernelVersion)) {
			klog.InfoS("WARNING: the kernel version is incompatible with the feature gate, which needs as a minimum kernel version",
				"kernelVersion", kernelVersion, "feature", features.UserNamespacesSupport, "minKernelVersion", utilkernel.UserNamespacesSupportKernelVersion)
		}
	}

	_, err := user.Lookup(kubeletUser)
	if err != nil {
		var unknownUserErr user.UnknownUserError
		if goerrors.As(err, &unknownUserErr) {
			// if the user is not found, we assume that the user is not configured
			klog.V(5).InfoS("user namespaces: user not found, using default mappings", "user", kubeletUser)
			return defaultFirstID, defaultLen, nil
		}
		return 0, 0, err
	}

	execName := "getsubids"
	cmd, err := exec.LookPath(execName)
	if err != nil {
		if os.IsNotExist(err) {
			klog.V(2).InfoS("user namespaces: executable not found, using default mappings", "executable", execName, "err", err)
			return defaultFirstID, defaultLen, nil
		}
		return 0, 0, err
	}
	outUids, err := exec.Command(cmd, kubeletUser).Output()
	if err != nil {
		return 0, 0, fmt.Errorf("error retrieving additional uids for user %q: %w", kubeletUser, err)
	}
	outGids, err := exec.Command(cmd, "-g", kubeletUser).Output()
	if err != nil {
		return 0, 0, fmt.Errorf("error retrieving additional gids for user %q", kubeletUser)
	}
	if string(outUids) != string(outGids) {
		return 0, 0, fmt.Errorf("mismatched subuids and subgids for user %q", kubeletUser)
	}
	klog.V(5).InfoS("user namespaces: user found, using mappings from getsubids", "user", kubeletUser)
	return parseGetSubIdsOutput(string(outUids))
}

// Get a list of pods that have data directories.
func (kl *Kubelet) listPodsFromDisk() ([]types.UID, error) {
	podInfos, err := os.ReadDir(kl.getPodsDir())
	if err != nil {
		return nil, err
	}
	pods := []types.UID{}
	for i := range podInfos {
		if podInfos[i].IsDir() {
			pods = append(pods, types.UID(podInfos[i].Name()))
		}
	}
	return pods, nil
}

// GetActivePods returns pods that have been admitted to the kubelet that
// are not fully terminated. This is mapped to the "desired state" of the
// kubelet - what pods should be running.
//
// WARNING: Currently this list does not include pods that have been force
// deleted but may still be terminating, which means resources assigned to
// those pods during admission may still be in use. See
// https://github.com/kubernetes/kubernetes/issues/104824
func (kl *Kubelet) GetActivePods() []*v1.Pod {
	allPods := kl.podManager.GetPods()
	activePods := kl.filterOutInactivePods(allPods)
	return activePods
}

// getAllocatedPods returns the active pods (see GetActivePods), but updates the pods to their
// allocated state.
func (kl *Kubelet) getAllocatedPods() []*v1.Pod {
	activePods := kl.GetActivePods()
	if !utilfeature.DefaultFeatureGate.Enabled(features.InPlacePodVerticalScaling) {
		return activePods
	}

	allocatedPods := make([]*v1.Pod, len(activePods))
	for i, pod := range activePods {
		allocatedPods[i], _ = kl.statusManager.UpdatePodFromAllocation(pod)
	}
	return allocatedPods
}

// makeBlockVolumes maps the raw block devices specified in the path of the container
// Experimental
func (kl *Kubelet) makeBlockVolumes(pod *v1.Pod, container *v1.Container, podVolumes kubecontainer.VolumeMap, blkutil volumepathhandler.BlockVolumePathHandler) ([]kubecontainer.DeviceInfo, error) {
	var devices []kubecontainer.DeviceInfo
	for _, device := range container.VolumeDevices {
		// check path is absolute
		if !utilfs.IsAbs(device.DevicePath) {
			return nil, fmt.Errorf("error DevicePath `%s` must be an absolute path", device.DevicePath)
		}
		vol, ok := podVolumes[device.Name]
		if !ok || vol.BlockVolumeMapper == nil {
			klog.ErrorS(nil, "Block volume cannot be satisfied for container, because the volume is missing or the volume mapper is nil", "containerName", container.Name, "device", device)
			return nil, fmt.Errorf("cannot find volume %q to pass into container %q", device.Name, container.Name)
		}
		// Get a symbolic link associated to a block device under pod device path
		dirPath, volName := vol.BlockVolumeMapper.GetPodDeviceMapPath()
		symlinkPath := filepath.Join(dirPath, volName)
		if islinkExist, checkErr := blkutil.IsSymlinkExist(symlinkPath); checkErr != nil {
			return nil, checkErr
		} else if islinkExist {
			// Check readOnly in PVCVolumeSource and set read only permission if it's true.
			permission := "mrw"
			if vol.ReadOnly {
				permission = "r"
			}
			klog.V(4).InfoS("Device will be attached to container in the corresponding path on host", "containerName", container.Name, "path", symlinkPath)
			devices = append(devices, kubecontainer.DeviceInfo{PathOnHost: symlinkPath, PathInContainer: device.DevicePath, Permissions: permission})
		}
	}

	return devices, nil
}

// shouldMountHostsFile checks if the nodes /etc/hosts should be mounted
// Kubernetes only mounts on /etc/hosts if:
// - container is not an infrastructure (pause) container
// - container is not already mounting on /etc/hosts
// Kubernetes will not mount /etc/hosts if:
// - the Pod is on the pod network and PodIP has not yet been set (e.g., Pod sandbox is being created).
// - the Pod is on Windows, and contains a hostProcess container.
func shouldMountHostsFile(pod *v1.Pod, podIPs []string) bool {
	shouldMount := len(podIPs) > 0 || pod.Spec.HostNetwork
	if runtime.GOOS == "windows" {
		return shouldMount && !kubecontainer.HasWindowsHostProcessContainer(pod)
	}
	return shouldMount
}

// makeMounts determines the mount points for the given container.
func makeMounts(pod *v1.Pod, podDir string, container *v1.Container, hostName, hostDomain string, podIPs []string, podVolumes kubecontainer.VolumeMap, hu hostutil.HostUtils, subpather subpath.Interface, expandEnvs []kubecontainer.EnvVar, supportsRRO bool, imageVolumes kubecontainer.ImageVolumes) ([]kubecontainer.Mount, func(), error) {
	mountEtcHostsFile := shouldMountHostsFile(pod, podIPs)
	klog.V(3).InfoS("Creating hosts mount for container", "pod", klog.KObj(pod), "containerName", container.Name, "podIPs", podIPs, "path", mountEtcHostsFile)
	mounts := []kubecontainer.Mount{}
	var cleanupAction func()
	for i, mount := range container.VolumeMounts {
		// do not mount /etc/hosts if container is already mounting on the path
		mountEtcHostsFile = mountEtcHostsFile && (mount.MountPath != etcHostsPath)
		vol, ok := podVolumes[mount.Name]
		if !ok || vol.Mounter == nil {
			klog.ErrorS(nil, "Mount cannot be satisfied for the container, because the volume is missing or the volume mounter (vol.Mounter) is nil",
				"containerName", container.Name, "ok", ok, "volumeMounter", mount)
			return nil, cleanupAction, fmt.Errorf("cannot find volume %q to mount into container %q", mount.Name, container.Name)
		}

		relabelVolume := false
		// If the volume supports SELinux and it has not been
		// relabeled already and it is not a read-only volume,
		// relabel it and mark it as labeled
		if vol.Mounter.GetAttributes().Managed && vol.Mounter.GetAttributes().SELinuxRelabel && !vol.SELinuxLabeled {
			vol.SELinuxLabeled = true
			relabelVolume = true
		}

		var (
			hostPath string
			image    *runtimeapi.ImageSpec
			err      error
		)

		if imageVolumes != nil && utilfeature.DefaultFeatureGate.Enabled(features.ImageVolume) {
			image = imageVolumes[mount.Name]
		}

		if image == nil {
			hostPath, err = volumeutil.GetPath(vol.Mounter)
			if err != nil {
				return nil, cleanupAction, err
			}

			subPath := mount.SubPath
			if mount.SubPathExpr != "" {
				subPath, err = kubecontainer.ExpandContainerVolumeMounts(mount, expandEnvs)

				if err != nil {
					return nil, cleanupAction, err
				}
			}

			if subPath != "" {
				if utilfs.IsAbs(subPath) {
					return nil, cleanupAction, fmt.Errorf("error SubPath `%s` must not be an absolute path", subPath)
				}

				err = volumevalidation.ValidatePathNoBacksteps(subPath)
				if err != nil {
					return nil, cleanupAction, fmt.Errorf("unable to provision SubPath `%s`: %w", subPath, err)
				}

				volumePath := hostPath
				hostPath = filepath.Join(volumePath, subPath)

				if subPathExists, err := hu.PathExists(hostPath); err != nil {
					klog.ErrorS(nil, "Could not determine if subPath exists, will not attempt to change its permissions", "path", hostPath)
				} else if !subPathExists {
					// Create the sub path now because if it's auto-created later when referenced, it may have an
					// incorrect ownership and mode. For example, the sub path directory must have at least g+rwx
					// when the pod specifies an fsGroup, and if the directory is not created here, Docker will
					// later auto-create it with the incorrect mode 0750
					// Make extra care not to escape the volume!
					perm, err := hu.GetMode(volumePath)
					if err != nil {
						return nil, cleanupAction, err
					}
					if err := subpather.SafeMakeDir(subPath, volumePath, perm); err != nil {
						// Don't pass detailed error back to the user because it could give information about host filesystem
						klog.ErrorS(err, "Failed to create subPath directory for volumeMount of the container", "containerName", container.Name, "volumeMountName", mount.Name)
						return nil, cleanupAction, fmt.Errorf("failed to create subPath directory for volumeMount %q of container %q", mount.Name, container.Name)
					}
				}
				hostPath, cleanupAction, err = subpather.PrepareSafeSubpath(subpath.Subpath{
					VolumeMountIndex: i,
					Path:             hostPath,
					VolumeName:       vol.InnerVolumeSpecName,
					VolumePath:       volumePath,
					PodDir:           podDir,
					ContainerName:    container.Name,
				})
				if err != nil {
					// Don't pass detailed error back to the user because it could give information about host filesystem
					klog.ErrorS(err, "Failed to prepare subPath for volumeMount of the container", "containerName", container.Name, "volumeMountName", mount.Name)
					return nil, cleanupAction, fmt.Errorf("failed to prepare subPath for volumeMount %q of container %q", mount.Name, container.Name)
				}
			}

			// Docker Volume Mounts fail on Windows if it is not of the form C:/
			if hostPath != "" && volumeutil.IsWindowsLocalPath(runtime.GOOS, hostPath) {
				hostPath = volumeutil.MakeAbsolutePath(runtime.GOOS, hostPath)
			}
		}

		containerPath := mount.MountPath
		// IsAbs returns false for UNC path/SMB shares/named pipes in Windows. So check for those specifically and skip MakeAbsolutePath
		if !volumeutil.IsWindowsUNCPath(runtime.GOOS, containerPath) && !utilfs.IsAbs(containerPath) {
			containerPath = volumeutil.MakeAbsolutePath(runtime.GOOS, containerPath)
		}

		propagation, err := translateMountPropagation(mount.MountPropagation)
		if err != nil {
			return nil, cleanupAction, err
		}
		klog.V(5).InfoS("Mount has propagation", "pod", klog.KObj(pod), "containerName", container.Name, "volumeMountName", mount.Name, "propagation", propagation)
		mustMountRO := vol.Mounter.GetAttributes().ReadOnly

		rro, err := resolveRecursiveReadOnly(mount, supportsRRO)
		if err != nil {
			return nil, cleanupAction, fmt.Errorf("failed to resolve recursive read-only mode: %w", err)
		}
		if rro && !utilfeature.DefaultFeatureGate.Enabled(features.RecursiveReadOnlyMounts) {
			return nil, cleanupAction, fmt.Errorf("recursive read-only mount needs feature gate %q to be enabled", features.RecursiveReadOnlyMounts)
		}

		mounts = append(mounts, kubecontainer.Mount{
			Name:              mount.Name,
			ContainerPath:     containerPath,
			HostPath:          hostPath,
			Image:             image,
			ReadOnly:          mount.ReadOnly || mustMountRO,
			RecursiveReadOnly: rro,
			SELinuxRelabel:    relabelVolume,
			Propagation:       propagation,
		})
	}
	if mountEtcHostsFile {
		hostAliases := pod.Spec.HostAliases
		hostsMount, err := makeHostsMount(podDir, podIPs, hostName, hostDomain, hostAliases, pod.Spec.HostNetwork)
		if err != nil {
			return nil, cleanupAction, err
		}
		mounts = append(mounts, *hostsMount)
	}
	return mounts, cleanupAction, nil
}

// translateMountPropagation transforms v1.MountPropagationMode to
// runtimeapi.MountPropagation.
func translateMountPropagation(mountMode *v1.MountPropagationMode) (runtimeapi.MountPropagation, error) {
	if runtime.GOOS == "windows" {
		// Windows containers doesn't support mount propagation, use private for it.
		// Refer https://docs.docker.com/storage/bind-mounts/#configure-bind-propagation.
		return runtimeapi.MountPropagation_PROPAGATION_PRIVATE, nil
	}

	switch {
	case mountMode == nil:
		// PRIVATE is the default
		return runtimeapi.MountPropagation_PROPAGATION_PRIVATE, nil
	case *mountMode == v1.MountPropagationHostToContainer:
		return runtimeapi.MountPropagation_PROPAGATION_HOST_TO_CONTAINER, nil
	case *mountMode == v1.MountPropagationBidirectional:
		return runtimeapi.MountPropagation_PROPAGATION_BIDIRECTIONAL, nil
	case *mountMode == v1.MountPropagationNone:
		return runtimeapi.MountPropagation_PROPAGATION_PRIVATE, nil
	default:
		return 0, fmt.Errorf("invalid MountPropagation mode: %q", *mountMode)
	}
}

// getEtcHostsPath returns the full host-side path to a pod's generated /etc/hosts file
func getEtcHostsPath(podDir string) string {
	hostsFilePath := filepath.Join(podDir, "etc-hosts")
	// Volume Mounts fail on Windows if it is not of the form C:/
	return volumeutil.MakeAbsolutePath(runtime.GOOS, hostsFilePath)
}

// makeHostsMount makes the mountpoint for the hosts file that the containers
// in a pod are injected with. podIPs is provided instead of podIP as podIPs
// are present even if dual-stack feature flag is not enabled.
func makeHostsMount(podDir string, podIPs []string, hostName, hostDomainName string, hostAliases []v1.HostAlias, useHostNetwork bool) (*kubecontainer.Mount, error) {
	hostsFilePath := getEtcHostsPath(podDir)
	if err := ensureHostsFile(hostsFilePath, podIPs, hostName, hostDomainName, hostAliases, useHostNetwork); err != nil {
		return nil, err
	}
	return &kubecontainer.Mount{
		Name:           "k8s-managed-etc-hosts",
		ContainerPath:  etcHostsPath,
		HostPath:       hostsFilePath,
		ReadOnly:       false,
		SELinuxRelabel: true,
	}, nil
}

// ensureHostsFile ensures that the given host file has an up-to-date ip, host
// name, and domain name.
func ensureHostsFile(fileName string, hostIPs []string, hostName, hostDomainName string, hostAliases []v1.HostAlias, useHostNetwork bool) error {
	var hostsFileContent []byte
	var err error

	if useHostNetwork {
		// if Pod is using host network, read hosts file from the node's filesystem.
		// `etcHostsPath` references the location of the hosts file on the node.
		// `/etc/hosts` for *nix systems.
		hostsFileContent, err = nodeHostsFileContent(etcHostsPath, hostAliases)
		if err != nil {
			return err
		}
	} else {
		// if Pod is not using host network, create a managed hosts file with Pod IP and other information.
		hostsFileContent = managedHostsFileContent(hostIPs, hostName, hostDomainName, hostAliases)
	}

	hostsFilePerm := os.FileMode(0644)
	if err := os.WriteFile(fileName, hostsFileContent, hostsFilePerm); err != nil {
		return err
	}
	return os.Chmod(fileName, hostsFilePerm)
}

// nodeHostsFileContent reads the content of node's hosts file.
func nodeHostsFileContent(hostsFilePath string, hostAliases []v1.HostAlias) ([]byte, error) {
	hostsFileContent, err := os.ReadFile(hostsFilePath)
	if err != nil {
		return nil, err
	}
	var buffer bytes.Buffer
	buffer.WriteString(managedHostsHeaderWithHostNetwork)
	buffer.Write(hostsFileContent)
	buffer.Write(hostsEntriesFromHostAliases(hostAliases))
	return buffer.Bytes(), nil
}

// managedHostsFileContent generates the content of the managed etc hosts based on Pod IPs and other
// information.
func managedHostsFileContent(hostIPs []string, hostName, hostDomainName string, hostAliases []v1.HostAlias) []byte {
	var buffer bytes.Buffer
	buffer.WriteString(managedHostsHeader)
	buffer.WriteString("127.0.0.1\tlocalhost\n")                      // ipv4 localhost
	buffer.WriteString("::1\tlocalhost ip6-localhost ip6-loopback\n") // ipv6 localhost
	buffer.WriteString("fe00::0\tip6-localnet\n")
	buffer.WriteString("fe00::0\tip6-mcastprefix\n")
	buffer.WriteString("fe00::1\tip6-allnodes\n")
	buffer.WriteString("fe00::2\tip6-allrouters\n")
	if len(hostDomainName) > 0 {
		// host entry generated for all IPs in podIPs
		// podIPs field is populated for clusters even
		// dual-stack feature flag is not enabled.
		for _, hostIP := range hostIPs {
			buffer.WriteString(fmt.Sprintf("%s\t%s.%s\t%s\n", hostIP, hostName, hostDomainName, hostName))
		}
	} else {
		for _, hostIP := range hostIPs {
			buffer.WriteString(fmt.Sprintf("%s\t%s\n", hostIP, hostName))
		}
	}
	buffer.Write(hostsEntriesFromHostAliases(hostAliases))
	return buffer.Bytes()
}

func hostsEntriesFromHostAliases(hostAliases []v1.HostAlias) []byte {
	if len(hostAliases) == 0 {
		return []byte{}
	}

	var buffer bytes.Buffer
	buffer.WriteString("\n")
	buffer.WriteString("# Entries added by HostAliases.\n")
	// for each IP, write all aliases onto single line in hosts file
	for _, hostAlias := range hostAliases {
		buffer.WriteString(fmt.Sprintf("%s\t%s\n", hostAlias.IP, strings.Join(hostAlias.Hostnames, "\t")))
	}
	return buffer.Bytes()
}

// truncatePodHostnameIfNeeded truncates the pod hostname if it's longer than 63 chars.
func truncatePodHostnameIfNeeded(podName, hostname string) (string, error) {
	// Cap hostname at 63 chars (specification is 64bytes which is 63 chars and the null terminating char).
	const hostnameMaxLen = 63
	if len(hostname) <= hostnameMaxLen {
		return hostname, nil
	}
	truncated := hostname[:hostnameMaxLen]
	klog.ErrorS(nil, "Hostname for pod was too long, truncated it", "podName", podName, "hostnameMaxLen", hostnameMaxLen, "truncatedHostname", truncated)
	// hostname should not end with '-' or '.'
	truncated = strings.TrimRight(truncated, "-.")
	if len(truncated) == 0 {
		// This should never happen.
		return "", fmt.Errorf("hostname for pod %q was invalid: %q", podName, hostname)
	}
	return truncated, nil
}

// GetOrCreateUserNamespaceMappings returns the configuration for the sandbox user namespace
func (kl *Kubelet) GetOrCreateUserNamespaceMappings(pod *v1.Pod, runtimeHandler string) (*runtimeapi.UserNamespace, error) {
	return kl.usernsManager.GetOrCreateUserNamespaceMappings(pod, runtimeHandler)
}

// GeneratePodHostNameAndDomain creates a hostname and domain name for a pod,
// given that pod's spec and annotations or returns an error.
func (kl *Kubelet) GeneratePodHostNameAndDomain(pod *v1.Pod) (string, string, error) {
	clusterDomain := kl.dnsConfigurer.ClusterDomain

	hostname := pod.Name
	if len(pod.Spec.Hostname) > 0 {
		if msgs := utilvalidation.IsDNS1123Label(pod.Spec.Hostname); len(msgs) != 0 {
			return "", "", fmt.Errorf("pod Hostname %q is not a valid DNS label: %s", pod.Spec.Hostname, strings.Join(msgs, ";"))
		}
		hostname = pod.Spec.Hostname
	}

	hostname, err := truncatePodHostnameIfNeeded(pod.Name, hostname)
	if err != nil {
		return "", "", err
	}

	hostDomain := ""
	if len(pod.Spec.Subdomain) > 0 {
		if msgs := utilvalidation.IsDNS1123Label(pod.Spec.Subdomain); len(msgs) != 0 {
			return "", "", fmt.Errorf("pod Subdomain %q is not a valid DNS label: %s", pod.Spec.Subdomain, strings.Join(msgs, ";"))
		}
		hostDomain = fmt.Sprintf("%s.%s.svc.%s", pod.Spec.Subdomain, pod.Namespace, clusterDomain)
	}

	return hostname, hostDomain, nil
}

// GetPodCgroupParent gets pod cgroup parent from container manager.
func (kl *Kubelet) GetPodCgroupParent(pod *v1.Pod) string {
	pcm := kl.containerManager.NewPodContainerManager()
	_, cgroupParent := pcm.GetPodContainerName(pod)
	return cgroupParent
}

// GenerateRunContainerOptions generates the RunContainerOptions, which can be used by
// the container runtime to set parameters for launching a container.
func (kl *Kubelet) GenerateRunContainerOptions(ctx context.Context, pod *v1.Pod, container *v1.Container, podIP string, podIPs []string, imageVolumes kubecontainer.ImageVolumes) (*kubecontainer.RunContainerOptions, func(), error) {
	supportsRRO := kl.runtimeClassSupportsRecursiveReadOnlyMounts(pod)

	opts, err := kl.containerManager.GetResources(ctx, pod, container)
	if err != nil {
		return nil, nil, err
	}
	// The value of hostname is the short host name and it is sent to makeMounts to create /etc/hosts file.
	hostname, hostDomainName, err := kl.GeneratePodHostNameAndDomain(pod)
	if err != nil {
		return nil, nil, err
	}

	podName := volumeutil.GetUniquePodName(pod)
	volumes := kl.volumeManager.GetMountedVolumesForPod(podName)

	blkutil := volumepathhandler.NewBlockVolumePathHandler()
	blkVolumes, err := kl.makeBlockVolumes(pod, container, volumes, blkutil)
	if err != nil {
		return nil, nil, err
	}
	opts.Devices = append(opts.Devices, blkVolumes...)

	envs, err := kl.makeEnvironmentVariables(pod, container, podIP, podIPs)
	if err != nil {
		return nil, nil, err
	}
	opts.Envs = append(opts.Envs, envs...)

	// only podIPs is sent to makeMounts, as podIPs is populated even if dual-stack feature flag is not enabled.
	mounts, cleanupAction, err := makeMounts(pod, kl.getPodDir(pod.UID), container, hostname, hostDomainName, podIPs, volumes, kl.hostutil, kl.subpather, opts.Envs, supportsRRO, imageVolumes)
	if err != nil {
		return nil, cleanupAction, err
	}
	opts.Mounts = append(opts.Mounts, mounts...)

	// adding TerminationMessagePath on Windows is only allowed if ContainerD is used. Individual files cannot
	// be mounted as volumes using Docker for Windows.
	if len(container.TerminationMessagePath) != 0 {
		p := kl.getPodContainerDir(pod.UID, container.Name)
		if err := os.MkdirAll(p, 0750); err != nil {
			klog.ErrorS(err, "Error on creating dir", "path", p)
		} else {
			opts.PodContainerDir = p
		}
	}

	return opts, cleanupAction, nil
}

var masterServices = sets.New[string]("kubernetes")

// getServiceEnvVarMap makes a map[string]string of env vars for services a
// pod in namespace ns should see.
func (kl *Kubelet) getServiceEnvVarMap(ns string, enableServiceLinks bool) (map[string]string, error) {
	var (
		serviceMap = make(map[string]*v1.Service)
		m          = make(map[string]string)
	)

	// Get all service resources from the master (via a cache),
	// and populate them into service environment variables.
	if kl.serviceLister == nil {
		// Kubelets without masters (e.g. plain GCE ContainerVM) don't set env vars.
		return m, nil
	}
	services, err := kl.serviceLister.List(labels.Everything())
	if err != nil {
		return m, fmt.Errorf("failed to list services when setting up env vars")
	}

	// project the services in namespace ns onto the master services
	for i := range services {
		service := services[i]
		// ignore services where ClusterIP is "None" or empty
		if !v1helper.IsServiceIPSet(service) {
			continue
		}
		serviceName := service.Name

		// We always want to add environment variabled for master services
		// from the default namespace, even if enableServiceLinks is false.
		// We also add environment variables for other services in the same
		// namespace, if enableServiceLinks is true.
		if service.Namespace == metav1.NamespaceDefault && masterServices.Has(serviceName) {
			if _, exists := serviceMap[serviceName]; !exists {
				serviceMap[serviceName] = service
			}
		} else if service.Namespace == ns && enableServiceLinks {
			serviceMap[serviceName] = service
		}
	}

	mappedServices := []*v1.Service{}
	for key := range serviceMap {
		mappedServices = append(mappedServices, serviceMap[key])
	}

	for _, e := range envvars.FromServices(mappedServices) {
		m[e.Name] = e.Value
	}
	return m, nil
}

// Make the environment variables for a pod in the given namespace.
func (kl *Kubelet) makeEnvironmentVariables(pod *v1.Pod, container *v1.Container, podIP string, podIPs []string) ([]kubecontainer.EnvVar, error) {
	if pod.Spec.EnableServiceLinks == nil {
		return nil, fmt.Errorf("nil pod.spec.enableServiceLinks encountered, cannot construct envvars")
	}

	// If the pod originates from the kube-api, when we know that the kube-apiserver is responding and the kubelet's credentials are valid.
	// Knowing this, it is reasonable to wait until the service lister has synchronized at least once before attempting to build
	// a service env var map.  This doesn't present the race below from happening entirely, but it does prevent the "obvious"
	// failure case of services simply not having completed a list operation that can reasonably be expected to succeed.
	// One common case this prevents is a kubelet restart reading pods before services and some pod not having the
	// KUBERNETES_SERVICE_HOST injected because we didn't wait a short time for services to sync before proceeding.
	// The KUBERNETES_SERVICE_HOST link is special because it is unconditionally injected into pods and is read by the
	// in-cluster-config for pod clients
	if !kubetypes.IsStaticPod(pod) && !kl.serviceHasSynced() {
		return nil, fmt.Errorf("services have not yet been read at least once, cannot construct envvars")
	}

	var result []kubecontainer.EnvVar
	// Note:  These are added to the docker Config, but are not included in the checksum computed
	// by kubecontainer.HashContainer(...).  That way, we can still determine whether an
	// v1.Container is already running by its hash. (We don't want to restart a container just
	// because some service changed.)
	//
	// Note that there is a race between Kubelet seeing the pod and kubelet seeing the service.
	// To avoid this users can: (1) wait between starting a service and starting; or (2) detect
	// missing service env var and exit and be restarted; or (3) use DNS instead of env vars
	// and keep trying to resolve the DNS name of the service (recommended).
	serviceEnv, err := kl.getServiceEnvVarMap(pod.Namespace, *pod.Spec.EnableServiceLinks)
	if err != nil {
		return result, err
	}

	var (
		configMaps = make(map[string]*v1.ConfigMap)
		secrets    = make(map[string]*v1.Secret)
		tmpEnv     = make(map[string]string)
	)

	// Env will override EnvFrom variables.
	// Process EnvFrom first then allow Env to replace existing values.
	for _, envFrom := range container.EnvFrom {
		switch {
		case envFrom.ConfigMapRef != nil:
			cm := envFrom.ConfigMapRef
			name := cm.Name
			configMap, ok := configMaps[name]
			if !ok {
				if kl.kubeClient == nil {
					return result, fmt.Errorf("couldn't get configMap %v/%v, no kubeClient defined", pod.Namespace, name)
				}
				optional := cm.Optional != nil && *cm.Optional
				configMap, err = kl.configMapManager.GetConfigMap(pod.Namespace, name)
				if err != nil {
					if errors.IsNotFound(err) && optional {
						// ignore error when marked optional
						continue
					}
					return result, err
				}
				configMaps[name] = configMap
			}

			for k, v := range configMap.Data {
				if len(envFrom.Prefix) > 0 {
					k = envFrom.Prefix + k
				}

				tmpEnv[k] = v
			}
		case envFrom.SecretRef != nil:
			s := envFrom.SecretRef
			name := s.Name
			secret, ok := secrets[name]
			if !ok {
				if kl.kubeClient == nil {
					return result, fmt.Errorf("couldn't get secret %v/%v, no kubeClient defined", pod.Namespace, name)
				}
				optional := s.Optional != nil && *s.Optional
				secret, err = kl.secretManager.GetSecret(pod.Namespace, name)
				if err != nil {
					if errors.IsNotFound(err) && optional {
						// ignore error when marked optional
						continue
					}
					return result, err
				}
				secrets[name] = secret
			}

			for k, v := range secret.Data {
				if len(envFrom.Prefix) > 0 {
					k = envFrom.Prefix + k
				}

				tmpEnv[k] = string(v)
			}
		}
	}

	// Determine the final values of variables:
	//
	// 1.  Determine the final value of each variable:
	//     a.  If the variable's Value is set, expand the `$(var)` references to other
	//         variables in the .Value field; the sources of variables are the declared
	//         variables of the container and the service environment variables
	//     b.  If a source is defined for an environment variable, resolve the source
	// 2.  Create the container's environment in the order variables are declared
	// 3.  Add remaining service environment vars
	var (
		mappingFunc = expansion.MappingFuncFor(tmpEnv, serviceEnv)
	)
	for _, envVar := range container.Env {
		runtimeVal := envVar.Value
		if runtimeVal != "" {
			// Step 1a: expand variable references
			runtimeVal = expansion.Expand(runtimeVal, mappingFunc)
		} else if envVar.ValueFrom != nil {
			// Step 1b: resolve alternate env var sources
			switch {
			case envVar.ValueFrom.FieldRef != nil:
				runtimeVal, err = kl.podFieldSelectorRuntimeValue(envVar.ValueFrom.FieldRef, pod, podIP, podIPs)
				if err != nil {
					return result, err
				}
			case envVar.ValueFrom.ResourceFieldRef != nil:
				defaultedPod, defaultedContainer, err := kl.defaultPodLimitsForDownwardAPI(pod, container)
				if err != nil {
					return result, err
				}
				runtimeVal, err = containerResourceRuntimeValue(envVar.ValueFrom.ResourceFieldRef, defaultedPod, defaultedContainer)
				if err != nil {
					return result, err
				}
			case envVar.ValueFrom.ConfigMapKeyRef != nil:
				cm := envVar.ValueFrom.ConfigMapKeyRef
				name := cm.Name
				key := cm.Key
				optional := cm.Optional != nil && *cm.Optional
				configMap, ok := configMaps[name]
				if !ok {
					if kl.kubeClient == nil {
						return result, fmt.Errorf("couldn't get configMap %v/%v, no kubeClient defined", pod.Namespace, name)
					}
					configMap, err = kl.configMapManager.GetConfigMap(pod.Namespace, name)
					if err != nil {
						if errors.IsNotFound(err) && optional {
							// ignore error when marked optional
							continue
						}
						return result, err
					}
					configMaps[name] = configMap
				}
				runtimeVal, ok = configMap.Data[key]
				if !ok {
					if optional {
						continue
					}
					return result, fmt.Errorf("couldn't find key %v in ConfigMap %v/%v", key, pod.Namespace, name)
				}
			case envVar.ValueFrom.SecretKeyRef != nil:
				s := envVar.ValueFrom.SecretKeyRef
				name := s.Name
				key := s.Key
				optional := s.Optional != nil && *s.Optional
				secret, ok := secrets[name]
				if !ok {
					if kl.kubeClient == nil {
						return result, fmt.Errorf("couldn't get secret %v/%v, no kubeClient defined", pod.Namespace, name)
					}
					secret, err = kl.secretManager.GetSecret(pod.Namespace, name)
					if err != nil {
						if errors.IsNotFound(err) && optional {
							// ignore error when marked optional
							continue
						}
						return result, err
					}
					secrets[name] = secret
				}
				runtimeValBytes, ok := secret.Data[key]
				if !ok {
					if optional {
						continue
					}
					return result, fmt.Errorf("couldn't find key %v in Secret %v/%v", key, pod.Namespace, name)
				}
				runtimeVal = string(runtimeValBytes)
			}
		}

		tmpEnv[envVar.Name] = runtimeVal
	}

	// Append the env vars
	for k, v := range tmpEnv {
		result = append(result, kubecontainer.EnvVar{Name: k, Value: v})
	}

	// Append remaining service env vars.
	for k, v := range serviceEnv {
		// Accesses apiserver+Pods.
		// So, the master may set service env vars, or kubelet may.  In case both are doing
		// it, we skip the key from the kubelet-generated ones so we don't have duplicate
		// env vars.
		// TODO: remove this next line once all platforms use apiserver+Pods.
		if _, present := tmpEnv[k]; !present {
			result = append(result, kubecontainer.EnvVar{Name: k, Value: v})
		}
	}
	return result, nil
}

// podFieldSelectorRuntimeValue returns the runtime value of the given
// selector for a pod.
func (kl *Kubelet) podFieldSelectorRuntimeValue(fs *v1.ObjectFieldSelector, pod *v1.Pod, podIP string, podIPs []string) (string, error) {
	internalFieldPath, _, err := podshelper.ConvertDownwardAPIFieldLabel(fs.APIVersion, fs.FieldPath, "")
	if err != nil {
		return "", err
	}

	// make podIPs order match node IP family preference #97979
	podIPs = kl.sortPodIPs(podIPs)
	if len(podIPs) > 0 {
		podIP = podIPs[0]
	}

	switch internalFieldPath {
	case "spec.nodeName":
		return pod.Spec.NodeName, nil
	case "spec.serviceAccountName":
		return pod.Spec.ServiceAccountName, nil
	case "status.hostIP":
		hostIPs, err := kl.getHostIPsAnyWay()
		if err != nil {
			return "", err
		}
		return hostIPs[0].String(), nil
	case "status.hostIPs":
		hostIPs, err := kl.getHostIPsAnyWay()
		if err != nil {
			return "", err
		}
		ips := make([]string, 0, len(hostIPs))
		for _, ip := range hostIPs {
			ips = append(ips, ip.String())
		}
		return strings.Join(ips, ","), nil
	case "status.podIP":
		return podIP, nil
	case "status.podIPs":
		return strings.Join(podIPs, ","), nil
	}
	return fieldpath.ExtractFieldPathAsString(pod, internalFieldPath)
}

// containerResourceRuntimeValue returns the value of the provided container resource
func containerResourceRuntimeValue(fs *v1.ResourceFieldSelector, pod *v1.Pod, container *v1.Container) (string, error) {
	containerName := fs.ContainerName
	if len(containerName) == 0 {
		return resource.ExtractContainerResourceValue(fs, container)
	}
	return resource.ExtractResourceValueByContainerName(fs, pod, containerName)
}

// killPod instructs the container runtime to kill the pod. This method requires that
// the pod status contains the result of the last syncPod, otherwise it may fail to
// terminate newly created containers and sandboxes.
func (kl *Kubelet) killPod(ctx context.Context, pod *v1.Pod, p kubecontainer.Pod, gracePeriodOverride *int64) error {
	// Call the container runtime KillPod method which stops all known running containers of the pod
	if err := kl.containerRuntime.KillPod(ctx, pod, p, gracePeriodOverride); err != nil {
		return err
	}
	if err := kl.containerManager.UpdateQOSCgroups(); err != nil {
		klog.V(2).InfoS("Failed to update QoS cgroups while killing pod", "err", err)
	}
	return nil
}

// makePodDataDirs creates the dirs for the pod datas.
func (kl *Kubelet) makePodDataDirs(pod *v1.Pod) error {
	uid := pod.UID
	if err := os.MkdirAll(kl.getPodDir(uid), 0750); err != nil && !os.IsExist(err) {
		return err
	}
	if err := os.MkdirAll(kl.getPodVolumesDir(uid), 0750); err != nil && !os.IsExist(err) {
		return err
	}
	if err := os.MkdirAll(kl.getPodPluginsDir(uid), 0750); err != nil && !os.IsExist(err) {
		return err
	}
	return nil
}

// getPullSecretsForPod inspects the Pod and retrieves the referenced pull
// secrets.
func (kl *Kubelet) getPullSecretsForPod(pod *v1.Pod) []v1.Secret {
	pullSecrets := []v1.Secret{}
	failedPullSecrets := []string{}

	for _, secretRef := range pod.Spec.ImagePullSecrets {
		if len(secretRef.Name) == 0 {
			// API validation permitted entries with empty names (https://issue.k8s.io/99454#issuecomment-787838112).
			// Ignore to avoid unnecessary warnings.
			continue
		}
		secret, err := kl.secretManager.GetSecret(pod.Namespace, secretRef.Name)
		if err != nil {
			klog.InfoS("Unable to retrieve pull secret, the image pull may not succeed.", "pod", klog.KObj(pod), "secret", klog.KObj(secret), "err", err)
			failedPullSecrets = append(failedPullSecrets, secretRef.Name)
			continue
		}

		pullSecrets = append(pullSecrets, *secret)
	}

	if len(failedPullSecrets) > 0 {
		kl.recorder.Eventf(pod, v1.EventTypeWarning, "FailedToRetrieveImagePullSecret", "Unable to retrieve some image pull secrets (%s); attempting to pull the image may not succeed.", strings.Join(failedPullSecrets, ", "))
	}

	return pullSecrets
}

// PodCouldHaveRunningContainers returns true if the pod with the given UID could still have running
// containers. This returns false if the pod has not yet been started or the pod is unknown.
func (kl *Kubelet) PodCouldHaveRunningContainers(pod *v1.Pod) bool {
	if kl.podWorkers.CouldHaveRunningContainers(pod.UID) {
		return true
	}

	// Check if pod might need to unprepare resources before termination
	// NOTE: This is a temporary solution. This call is here to avoid changing
	// status manager and its tests.
	// TODO: extend PodDeletionSafetyProvider interface and implement it
	// in a separate Kubelet method.
	if utilfeature.DefaultFeatureGate.Enabled(features.DynamicResourceAllocation) {
		if kl.containerManager.PodMightNeedToUnprepareResources(pod.UID) {
			return true
		}
	}
	return false
}

// PodIsFinished returns true if SyncTerminatedPod is finished, ie.
// all required node-level resources that a pod was consuming have
// been reclaimed by the kubelet.
func (kl *Kubelet) PodIsFinished(pod *v1.Pod) bool {
	return kl.podWorkers.ShouldPodBeFinished(pod.UID)
}

// filterOutInactivePods returns pods that are not in a terminal phase
// or are known to be fully terminated. This method should only be used
// when the set of pods being filtered is upstream of the pod worker, i.e.
// the pods the pod manager is aware of.
func (kl *Kubelet) filterOutInactivePods(pods []*v1.Pod) []*v1.Pod {
	filteredPods := make([]*v1.Pod, 0, len(pods))
	for _, p := range pods {
		// if a pod is fully terminated by UID, it should be excluded from the
		// list of pods
		if kl.podWorkers.IsPodKnownTerminated(p.UID) {
			continue
		}

		// terminal pods are considered inactive UNLESS they are actively terminating
		if kl.isAdmittedPodTerminal(p) && !kl.podWorkers.IsPodTerminationRequested(p.UID) {
			continue
		}

		filteredPods = append(filteredPods, p)
	}
	return filteredPods
}

// isAdmittedPodTerminal returns true if the provided config source pod is in
// a terminal phase, or if the Kubelet has already indicated the pod has reached
// a terminal phase but the config source has not accepted it yet. This method
// should only be used within the pod configuration loops that notify the pod
// worker, other components should treat the pod worker as authoritative.
func (kl *Kubelet) isAdmittedPodTerminal(pod *v1.Pod) bool {
	// pods are considered inactive if the config source has observed a
	// terminal phase (if the Kubelet recorded that the pod reached a terminal
	// phase the pod should never be restarted)
	if pod.Status.Phase == v1.PodSucceeded || pod.Status.Phase == v1.PodFailed {
		return true
	}
	// a pod that has been marked terminal within the Kubelet is considered
	// inactive (may have been rejected by Kubelet admission)
	if status, ok := kl.statusManager.GetPodStatus(pod.UID); ok {
		if status.Phase == v1.PodSucceeded || status.Phase == v1.PodFailed {
			return true
		}
	}
	return false
}

// removeOrphanedPodStatuses removes obsolete entries in podStatus where
// the pod is no longer considered bound to this node.
func (kl *Kubelet) removeOrphanedPodStatuses(pods []*v1.Pod, mirrorPods []*v1.Pod) {
	podUIDs := make(map[types.UID]bool)
	for _, pod := range pods {
		podUIDs[pod.UID] = true
	}
	for _, pod := range mirrorPods {
		podUIDs[pod.UID] = true
	}
	kl.statusManager.RemoveOrphanedStatuses(podUIDs)
}

// HandlePodCleanups performs a series of cleanup work, including terminating
// pod workers, killing unwanted pods, and removing orphaned volumes/pod
// directories. No config changes are sent to pod workers while this method
// is executing which means no new pods can appear. After this method completes
// the desired state of the kubelet should be reconciled with the actual state
// in the pod worker and other pod-related components.
//
// This function is executed by the main sync loop, so it must execute quickly
// and all nested calls should be asynchronous. Any slow reconciliation actions
// should be performed by other components (like the volume manager). The duration
// of this call is the minimum latency for static pods to be restarted if they
// are updated with a fixed UID (most should use a dynamic UID), and no config
// updates are delivered to the pod workers while this method is running.
func (kl *Kubelet) HandlePodCleanups(ctx context.Context) error {
	// The kubelet lacks checkpointing, so we need to introspect the set of pods
	// in the cgroup tree prior to inspecting the set of pods in our pod manager.
	// this ensures our view of the cgroup tree does not mistakenly observe pods
	// that are added after the fact...
	var (
		cgroupPods map[types.UID]cm.CgroupName
		err        error
	)
	if kl.cgroupsPerQOS {
		pcm := kl.containerManager.NewPodContainerManager()
		cgroupPods, err = pcm.GetAllPodsFromCgroups()
		if err != nil {
			return fmt.Errorf("failed to get list of pods that still exist on cgroup mounts: %v", err)
		}
	}

	allPods, mirrorPods, orphanedMirrorPodFullnames := kl.podManager.GetPodsAndMirrorPods()

	// Pod phase progresses monotonically. Once a pod has reached a final state,
	// it should never leave regardless of the restart policy. The statuses
	// of such pods should not be changed, and there is no need to sync them.
	// TODO: the logic here does not handle two cases:
	//   1. If the containers were removed immediately after they died, kubelet
	//      may fail to generate correct statuses, let alone filtering correctly.
	//   2. If kubelet restarted before writing the terminated status for a pod
	//      to the apiserver, it could still restart the terminated pod (even
	//      though the pod was not considered terminated by the apiserver).
	// These two conditions could be alleviated by checkpointing kubelet.

	// Stop the workers for terminated pods not in the config source
	klog.V(3).InfoS("Clean up pod workers for terminated pods")
	workingPods := kl.podWorkers.SyncKnownPods(allPods)

	// Reconcile: At this point the pod workers have been pruned to the set of
	// desired pods. Pods that must be restarted due to UID reuse, or leftover
	// pods from previous runs, are not known to the pod worker.

	allPodsByUID := make(map[types.UID]*v1.Pod)
	for _, pod := range allPods {
		allPodsByUID[pod.UID] = pod
	}

	// Identify the set of pods that have workers, which should be all pods
	// from config that are not terminated, as well as any terminating pods
	// that have already been removed from config. Pods that are terminating
	// will be added to possiblyRunningPods, to prevent overly aggressive
	// cleanup of pod cgroups.
	stringIfTrue := func(t bool) string {
		if t {
			return "true"
		}
		return ""
	}
	runningPods := make(map[types.UID]sets.Empty)
	possiblyRunningPods := make(map[types.UID]sets.Empty)
	for uid, sync := range workingPods {
		switch sync.State {
		case SyncPod:
			runningPods[uid] = struct{}{}
			possiblyRunningPods[uid] = struct{}{}
		case TerminatingPod:
			possiblyRunningPods[uid] = struct{}{}
		default:
		}
	}

	// Retrieve the list of running containers from the runtime to perform cleanup.
	// We need the latest state to avoid delaying restarts of static pods that reuse
	// a UID.
	if err := kl.runtimeCache.ForceUpdateIfOlder(ctx, kl.clock.Now()); err != nil {
		klog.ErrorS(err, "Error listing containers")
		return err
	}
	runningRuntimePods, err := kl.runtimeCache.GetPods(ctx)
	if err != nil {
		klog.ErrorS(err, "Error listing containers")
		return err
	}

	// Stop probing pods that are not running
	klog.V(3).InfoS("Clean up probes for terminated pods")
	kl.probeManager.CleanupPods(possiblyRunningPods)

	// Remove orphaned pod statuses not in the total list of known config pods
	klog.V(3).InfoS("Clean up orphaned pod statuses")
	kl.removeOrphanedPodStatuses(allPods, mirrorPods)

	// Remove orphaned pod user namespace allocations (if any).
	klog.V(3).InfoS("Clean up orphaned pod user namespace allocations")
	if err = kl.usernsManager.CleanupOrphanedPodUsernsAllocations(allPods, runningRuntimePods); err != nil {
		klog.ErrorS(err, "Failed cleaning up orphaned pod user namespaces allocations")
	}

	// Remove orphaned volumes from pods that are known not to have any
	// containers. Note that we pass all pods (including terminated pods) to
	// the function, so that we don't remove volumes associated with terminated
	// but not yet deleted pods.
	// TODO: this method could more aggressively cleanup terminated pods
	// in the future (volumes, mount dirs, logs, and containers could all be
	// better separated)
	klog.V(3).InfoS("Clean up orphaned pod directories")
	err = kl.cleanupOrphanedPodDirs(allPods, runningRuntimePods)
	if err != nil {
		// We want all cleanup tasks to be run even if one of them failed. So
		// we just log an error here and continue other cleanup tasks.
		// This also applies to the other clean up tasks.
		klog.ErrorS(err, "Failed cleaning up orphaned pod directories")
	}

	// Remove any orphaned mirror pods (mirror pods are tracked by name via the
	// pod worker)
	klog.V(3).InfoS("Clean up orphaned mirror pods")
	for _, podFullname := range orphanedMirrorPodFullnames {
		if !kl.podWorkers.IsPodForMirrorPodTerminatingByFullName(podFullname) {
			_, err := kl.mirrorPodClient.DeleteMirrorPod(podFullname, nil)
			if err != nil {
				klog.ErrorS(err, "Encountered error when deleting mirror pod", "podName", podFullname)
			} else {
				klog.V(3).InfoS("Deleted mirror pod", "podName", podFullname)
			}
		}
	}

	// After pruning pod workers for terminated pods get the list of active pods for
	// metrics and to determine restarts.
	activePods := kl.filterOutInactivePods(allPods)
	allRegularPods, allStaticPods := splitPodsByStatic(allPods)
	activeRegularPods, activeStaticPods := splitPodsByStatic(activePods)
	metrics.DesiredPodCount.WithLabelValues("").Set(float64(len(allRegularPods)))
	metrics.DesiredPodCount.WithLabelValues("true").Set(float64(len(allStaticPods)))
	metrics.ActivePodCount.WithLabelValues("").Set(float64(len(activeRegularPods)))
	metrics.ActivePodCount.WithLabelValues("true").Set(float64(len(activeStaticPods)))
	metrics.MirrorPodCount.Set(float64(len(mirrorPods)))

	// At this point, the pod worker is aware of which pods are not desired (SyncKnownPods).
	// We now look through the set of active pods for those that the pod worker is not aware of
	// and deliver an update. The most common reason a pod is not known is because the pod was
	// deleted and recreated with the same UID while the pod worker was driving its lifecycle (very
	// very rare for API pods, common for static pods with fixed UIDs). Containers that may still
	// be running from a previous execution must be reconciled by the pod worker's sync method.
	// We must use active pods because that is the set of admitted pods (podManager includes pods
	// that will never be run, and statusManager tracks already rejected pods).
	var restartCount, restartCountStatic int
	for _, desiredPod := range activePods {
		if _, knownPod := workingPods[desiredPod.UID]; knownPod {
			continue
		}

		klog.V(3).InfoS("Pod will be restarted because it is in the desired set and not known to the pod workers (likely due to UID reuse)", "podUID", desiredPod.UID)
		isStatic := kubetypes.IsStaticPod(desiredPod)
		pod, mirrorPod, wasMirror := kl.podManager.GetPodAndMirrorPod(desiredPod)
		if pod == nil || wasMirror {
			klog.V(2).InfoS("Programmer error, restartable pod was a mirror pod but activePods should never contain a mirror pod", "podUID", desiredPod.UID)
			continue
		}
		kl.podWorkers.UpdatePod(UpdatePodOptions{
			UpdateType: kubetypes.SyncPodCreate,
			Pod:        pod,
			MirrorPod:  mirrorPod,
		})

		// the desired pod is now known as well
		workingPods[desiredPod.UID] = PodWorkerSync{State: SyncPod, HasConfig: true, Static: isStatic}
		if isStatic {
			// restartable static pods are the normal case
			restartCountStatic++
		} else {
			// almost certainly means shenanigans, as API pods should never have the same UID after being deleted and recreated
			// unless there is a major API violation
			restartCount++
		}
	}
	metrics.RestartedPodTotal.WithLabelValues("true").Add(float64(restartCountStatic))
	metrics.RestartedPodTotal.WithLabelValues("").Add(float64(restartCount))

	// Complete termination of deleted pods that are not runtime pods (don't have
	// running containers), are terminal, and are not known to pod workers.
	// An example is pods rejected during kubelet admission that have never
	// started before (i.e. does not have an orphaned pod).
	// Adding the pods with SyncPodKill to pod workers allows to proceed with
	// force-deletion of such pods, yet preventing re-entry of the routine in the
	// next invocation of HandlePodCleanups.
	for _, pod := range kl.filterTerminalPodsToDelete(allPods, runningRuntimePods, workingPods) {
		klog.V(3).InfoS("Handling termination and deletion of the pod to pod workers", "pod", klog.KObj(pod), "podUID", pod.UID)
		kl.podWorkers.UpdatePod(UpdatePodOptions{
			UpdateType: kubetypes.SyncPodKill,
			Pod:        pod,
		})
	}

	// Finally, terminate any pods that are observed in the runtime but not present in the list of
	// known running pods from config. If we do terminate running runtime pods that will happen
	// asynchronously in the background and those will be processed in the next invocation of
	// HandlePodCleanups.
	var orphanCount int
	for _, runningPod := range runningRuntimePods {
		// If there are orphaned pod resources in CRI that are unknown to the pod worker, terminate them
		// now. Since housekeeping is exclusive to other pod worker updates, we know that no pods have
		// been added to the pod worker in the meantime. Note that pods that are not visible in the runtime
		// but which were previously known are terminated by SyncKnownPods().
		_, knownPod := workingPods[runningPod.ID]
		if !knownPod {
			one := int64(1)
			killPodOptions := &KillPodOptions{
				PodTerminationGracePeriodSecondsOverride: &one,
			}
			klog.V(2).InfoS("Clean up containers for orphaned pod we had not seen before", "podUID", runningPod.ID, "killPodOptions", killPodOptions)
			kl.podWorkers.UpdatePod(UpdatePodOptions{
				UpdateType:     kubetypes.SyncPodKill,
				RunningPod:     runningPod,
				KillPodOptions: killPodOptions,
			})

			// the running pod is now known as well
			workingPods[runningPod.ID] = PodWorkerSync{State: TerminatingPod, Orphan: true}
			orphanCount++
		}
	}
	metrics.OrphanedRuntimePodTotal.Add(float64(orphanCount))

	// Now that we have recorded any terminating pods, and added new pods that should be running,
	// record a summary here. Not all possible combinations of PodWorkerSync values are valid.
	counts := make(map[PodWorkerSync]int)
	for _, sync := range workingPods {
		counts[sync]++
	}
	for validSync, configState := range map[PodWorkerSync]string{
		{HasConfig: true, Static: true}:                "desired",
		{HasConfig: true, Static: false}:               "desired",
		{Orphan: true, HasConfig: true, Static: true}:  "orphan",
		{Orphan: true, HasConfig: true, Static: false}: "orphan",
		{Orphan: true, HasConfig: false}:               "runtime_only",
	} {
		for _, state := range []PodWorkerState{SyncPod, TerminatingPod, TerminatedPod} {
			validSync.State = state
			count := counts[validSync]
			delete(counts, validSync)
			staticString := stringIfTrue(validSync.Static)
			if !validSync.HasConfig {
				staticString = "unknown"
			}
			metrics.WorkingPodCount.WithLabelValues(state.String(), configState, staticString).Set(float64(count))
		}
	}
	if len(counts) > 0 {
		// in case a combination is lost
		klog.V(3).InfoS("Programmer error, did not report a kubelet_working_pods metric for a value returned by SyncKnownPods", "counts", counts)
	}

	// Remove any cgroups in the hierarchy for pods that are definitely no longer
	// running (not in the container runtime).
	if kl.cgroupsPerQOS {
		pcm := kl.containerManager.NewPodContainerManager()
		klog.V(3).InfoS("Clean up orphaned pod cgroups")
		kl.cleanupOrphanedPodCgroups(pcm, cgroupPods, possiblyRunningPods)
	}

	// Cleanup any backoff entries.
	kl.backOff.GC()
	return nil
}

// filterTerminalPodsToDelete returns terminal pods which are ready to be
// deleted by the status manager, but are not in pod workers.
// First, the check for deletionTimestamp is a performance optimization as we
// don't need to do anything with terminal pods without deletionTimestamp.
// Second, the check for terminal pods is to avoid race conditions of triggering
// deletion on Pending pods which are not yet added to pod workers.
// Third, the check to skip pods known to pod workers is that the lifecycle of
// such pods is already handled by pod workers.
// Finally, we skip runtime pods as their termination is handled separately in
// the HandlePodCleanups routine.
func (kl *Kubelet) filterTerminalPodsToDelete(allPods []*v1.Pod, runningRuntimePods []*kubecontainer.Pod, workingPods map[types.UID]PodWorkerSync) map[types.UID]*v1.Pod {
	terminalPodsToDelete := make(map[types.UID]*v1.Pod)
	for _, pod := range allPods {
		if pod.DeletionTimestamp == nil {
			// skip pods which don't have a deletion timestamp
			continue
		}
		if !podutil.IsPodPhaseTerminal(pod.Status.Phase) {
			// skip the non-terminal pods
			continue
		}
		if _, knownPod := workingPods[pod.UID]; knownPod {
			// skip pods known to pod workers
			continue
		}
		terminalPodsToDelete[pod.UID] = pod
	}
	for _, runningRuntimePod := range runningRuntimePods {
		// skip running runtime pods - they are handled by a dedicated routine
		// which terminates the containers
		delete(terminalPodsToDelete, runningRuntimePod.ID)
	}
	return terminalPodsToDelete
}

// splitPodsByStatic separates a list of desired pods from the pod manager into
// regular or static pods. Mirror pods are not valid config sources (a mirror pod
// being created cannot cause the Kubelet to start running a static pod) and are
// excluded.
func splitPodsByStatic(pods []*v1.Pod) (regular, static []*v1.Pod) {
	regular, static = make([]*v1.Pod, 0, len(pods)), make([]*v1.Pod, 0, len(pods))
	for _, pod := range pods {
		if kubetypes.IsMirrorPod(pod) {
			continue
		}
		if kubetypes.IsStaticPod(pod) {
			static = append(static, pod)
		} else {
			regular = append(regular, pod)
		}
	}
	return regular, static
}

// validateContainerLogStatus returns the container ID for the desired container to retrieve logs for, based on the state
// of the container. The previous flag will only return the logs for the last terminated container, otherwise, the current
// running container is preferred over a previous termination. If info about the container is not available then a specific
// error is returned to the end user.
func (kl *Kubelet) validateContainerLogStatus(podName string, podStatus *v1.PodStatus, containerName string, previous bool) (containerID kubecontainer.ContainerID, err error) {
	var cID string

	cStatus, found := podutil.GetContainerStatus(podStatus.ContainerStatuses, containerName)
	if !found {
		cStatus, found = podutil.GetContainerStatus(podStatus.InitContainerStatuses, containerName)
	}
	if !found {
		cStatus, found = podutil.GetContainerStatus(podStatus.EphemeralContainerStatuses, containerName)
	}
	if !found {
		return kubecontainer.ContainerID{}, fmt.Errorf("container %q in pod %q is not available", containerName, podName)
	}
	lastState := cStatus.LastTerminationState
	waiting, running, terminated := cStatus.State.Waiting, cStatus.State.Running, cStatus.State.Terminated

	switch {
	case previous:
		if lastState.Terminated == nil || lastState.Terminated.ContainerID == "" {
			return kubecontainer.ContainerID{}, fmt.Errorf("previous terminated container %q in pod %q not found", containerName, podName)
		}
		cID = lastState.Terminated.ContainerID

	case running != nil:
		cID = cStatus.ContainerID

	case terminated != nil:
		// in cases where the next container didn't start, terminated.ContainerID will be empty, so get logs from the lastState.Terminated.
		if terminated.ContainerID == "" {
			if lastState.Terminated != nil && lastState.Terminated.ContainerID != "" {
				cID = lastState.Terminated.ContainerID
			} else {
				return kubecontainer.ContainerID{}, fmt.Errorf("container %q in pod %q is terminated", containerName, podName)
			}
		} else {
			cID = terminated.ContainerID
		}

	case lastState.Terminated != nil:
		if lastState.Terminated.ContainerID == "" {
			return kubecontainer.ContainerID{}, fmt.Errorf("container %q in pod %q is terminated", containerName, podName)
		}
		cID = lastState.Terminated.ContainerID

	case waiting != nil:
		// output some info for the most common pending failures
		switch reason := waiting.Reason; reason {
		case images.ErrImagePull.Error():
			return kubecontainer.ContainerID{}, fmt.Errorf("container %q in pod %q is waiting to start: image can't be pulled", containerName, podName)
		case images.ErrImagePullBackOff.Error():
			return kubecontainer.ContainerID{}, fmt.Errorf("container %q in pod %q is waiting to start: trying and failing to pull image", containerName, podName)
		default:
			return kubecontainer.ContainerID{}, fmt.Errorf("container %q in pod %q is waiting to start: %v", containerName, podName, reason)
		}
	default:
		// unrecognized state
		return kubecontainer.ContainerID{}, fmt.Errorf("container %q in pod %q is waiting to start - no logs yet", containerName, podName)
	}

	return kubecontainer.ParseContainerID(cID), nil
}

// GetKubeletContainerLogs returns logs from the container
// TODO: this method is returning logs of random container attempts, when it should be returning the most recent attempt
// or all of them.
func (kl *Kubelet) GetKubeletContainerLogs(ctx context.Context, podFullName, containerName string, logOptions *v1.PodLogOptions, stdout, stderr io.Writer) error {
	// Pod workers periodically write status to statusManager. If status is not
	// cached there, something is wrong (or kubelet just restarted and hasn't
	// caught up yet). Just assume the pod is not ready yet.
	name, namespace, err := kubecontainer.ParsePodFullName(podFullName)
	if err != nil {
		return fmt.Errorf("unable to parse pod full name %q: %v", podFullName, err)
	}

	pod, ok := kl.GetPodByName(namespace, name)
	if !ok {
		return fmt.Errorf("pod %q cannot be found - no logs available", name)
	}

	// TODO: this should be using the podWorker's pod store as authoritative, since
	// the mirrorPod might still exist, the pod may have been force deleted but
	// is still terminating (users should be able to view logs of force deleted static pods
	// based on full name).
	var podUID types.UID
	pod, mirrorPod, wasMirror := kl.podManager.GetPodAndMirrorPod(pod)
	if wasMirror {
		if pod == nil {
			return fmt.Errorf("mirror pod %q does not have a corresponding pod", name)
		}
		podUID = mirrorPod.UID
	} else {
		podUID = pod.UID
	}

	podStatus, found := kl.statusManager.GetPodStatus(podUID)
	if !found {
		// If there is no cached status, use the status from the
		// config source (apiserver). This is useful if kubelet
		// has recently been restarted.
		podStatus = pod.Status
	}

	// TODO: Consolidate the logic here with kuberuntime.GetContainerLogs, here we convert container name to containerID,
	// but inside kuberuntime we convert container id back to container name and restart count.
	// TODO: After separate container log lifecycle management, we should get log based on the existing log files
	// instead of container status.
	containerID, err := kl.validateContainerLogStatus(pod.Name, &podStatus, containerName, logOptions.Previous)
	if err != nil {
		return err
	}

	// Since v1.32, stdout may be nil if the stream is not requested.
	if stdout != nil {
		// Do a zero-byte write to stdout before handing off to the container runtime.
		// This ensures at least one Write call is made to the writer when copying starts,
		// even if we then block waiting for log output from the container.
		if _, err := stdout.Write([]byte{}); err != nil {
			return err
		}
	}

	return kl.containerRuntime.GetContainerLogs(ctx, pod, containerID, logOptions, stdout, stderr)
}

// getPhase returns the phase of a pod given its container info.
func getPhase(pod *v1.Pod, info []v1.ContainerStatus, podIsTerminal bool) v1.PodPhase {
	spec := pod.Spec
	pendingRestartableInitContainers := 0
	pendingRegularInitContainers := 0
	failedInitialization := 0

	// regular init containers
	for _, container := range spec.InitContainers {
		if podutil.IsRestartableInitContainer(&container) {
			// Skip the restartable init containers here to handle them separately as
			// they are slightly different from the init containers in terms of the
			// pod phase.
			continue
		}

		containerStatus, ok := podutil.GetContainerStatus(info, container.Name)
		if !ok {
			pendingRegularInitContainers++
			continue
		}

		switch {
		case containerStatus.State.Running != nil:
			pendingRegularInitContainers++
		case containerStatus.State.Terminated != nil:
			if containerStatus.State.Terminated.ExitCode != 0 {
				failedInitialization++
			}
		case containerStatus.State.Waiting != nil:
			if containerStatus.LastTerminationState.Terminated != nil {
				if containerStatus.LastTerminationState.Terminated.ExitCode != 0 {
					failedInitialization++
				}
			} else {
				pendingRegularInitContainers++
			}
		default:
			pendingRegularInitContainers++
		}
	}

	// counters for restartable init and regular containers
	unknown := 0
	running := 0
	waiting := 0
	stopped := 0
	succeeded := 0

	// restartable init containers
	for _, container := range spec.InitContainers {
		if !podutil.IsRestartableInitContainer(&container) {
			// Skip the regular init containers, as they have been handled above.
			continue
		}
		containerStatus, ok := podutil.GetContainerStatus(info, container.Name)
		if !ok {
			unknown++
			continue
		}

		switch {
		case containerStatus.State.Running != nil:
			if containerStatus.Started == nil || !*containerStatus.Started {
				pendingRestartableInitContainers++
			}
			running++
		case containerStatus.State.Terminated != nil:
			// Do nothing here, as terminated restartable init containers are not
			// taken into account for the pod phase.
		case containerStatus.State.Waiting != nil:
			if containerStatus.LastTerminationState.Terminated != nil {
				// Do nothing here, as terminated restartable init containers are not
				// taken into account for the pod phase.
			} else {
				pendingRestartableInitContainers++
				waiting++
			}
		default:
			pendingRestartableInitContainers++
			unknown++
		}
	}

	for _, container := range spec.Containers {
		containerStatus, ok := podutil.GetContainerStatus(info, container.Name)
		if !ok {
			unknown++
			continue
		}

		switch {
		case containerStatus.State.Running != nil:
			running++
		case containerStatus.State.Terminated != nil:
			stopped++
			if containerStatus.State.Terminated.ExitCode == 0 {
				succeeded++
			}
		case containerStatus.State.Waiting != nil:
			if containerStatus.LastTerminationState.Terminated != nil {
				stopped++
			} else {
				waiting++
			}
		default:
			unknown++
		}
	}

	if failedInitialization > 0 && spec.RestartPolicy == v1.RestartPolicyNever {
		return v1.PodFailed
	}

	switch {
	case pendingRegularInitContainers > 0 ||
		(pendingRestartableInitContainers > 0 &&
			// This is needed to handle the case where the pod has been initialized but
			// the restartable init containers are restarting and the pod should not be
			// placed back into v1.PodPending since the regular containers have run.
			!kubecontainer.HasAnyRegularContainerStarted(&spec, info)):
		fallthrough
	case waiting > 0:
		klog.V(5).InfoS("Pod waiting > 0, pending")
		// One or more containers has not been started
		return v1.PodPending
	case running > 0 && unknown == 0:
		// All containers have been started, and at least
		// one container is running
		return v1.PodRunning
	case running == 0 && stopped > 0 && unknown == 0:
		// The pod is terminal so its containers won't be restarted regardless
		// of the restart policy.
		if podIsTerminal {
			// TODO(#116484): Also assign terminal phase to static pods.
			if !kubetypes.IsStaticPod(pod) {
				// All regular containers are terminated in success and all restartable
				// init containers are stopped.
				if stopped == succeeded {
					return v1.PodSucceeded
				}
				// There is at least one failure
				return v1.PodFailed
			}
		}
		// All containers are terminated
		if spec.RestartPolicy == v1.RestartPolicyAlways {
			// All containers are in the process of restarting
			return v1.PodRunning
		}
		if stopped == succeeded {
			// RestartPolicy is not Always, all containers are terminated in success
			// and all restartable init containers are stopped.
			return v1.PodSucceeded
		}
		if spec.RestartPolicy == v1.RestartPolicyNever {
			// RestartPolicy is Never, and all containers are
			// terminated with at least one in failure
			return v1.PodFailed
		}
		// RestartPolicy is OnFailure, and at least one in failure
		// and in the process of restarting
		return v1.PodRunning
	default:
		klog.V(5).InfoS("Pod default case, pending")
		return v1.PodPending
	}
}

func (kl *Kubelet) determinePodResizeStatus(allocatedPod *v1.Pod, podStatus *kubecontainer.PodStatus, podIsTerminal bool) v1.PodResizeStatus {
	if kubetypes.IsStaticPod(allocatedPod) {
		return ""
	}

	// If pod is terminal, clear the resize status.
	if podIsTerminal {
		kl.statusManager.SetPodResizeStatus(allocatedPod.UID, "")
		return ""
	}

	resizeStatus := kl.statusManager.GetPodResizeStatus(allocatedPod.UID)
	return resizeStatus
}

// allocatedResourcesMatchStatus tests whether the resizeable resources in the pod spec match the
// resources reported in the status.
func allocatedResourcesMatchStatus(allocatedPod *v1.Pod, podStatus *kubecontainer.PodStatus) bool {
	for _, c := range allocatedPod.Spec.Containers {
		if !allocatedContainerResourcesMatchStatus(allocatedPod, &c, podStatus) {
			return false
		}
	}
	if utilfeature.DefaultFeatureGate.Enabled(features.SidecarContainers) {
		for _, c := range allocatedPod.Spec.InitContainers {
			if podutil.IsRestartableInitContainer(&c) && !allocatedContainerResourcesMatchStatus(allocatedPod, &c, podStatus) {
				return false
			}
		}
	}
	return true
}

// allocatedContainerResourcesMatchStatus returns true if the container resources matches with the container statuses resources.
func allocatedContainerResourcesMatchStatus(allocatedPod *v1.Pod, c *v1.Container, podStatus *kubecontainer.PodStatus) bool {
	if cs := podStatus.FindContainerStatusByName(c.Name); cs != nil {
		if cs.State != kubecontainer.ContainerStateRunning {
			// If the container isn't running, it isn't resizing.
			return true
		}

		cpuReq, hasCPUReq := c.Resources.Requests[v1.ResourceCPU]
		cpuLim, hasCPULim := c.Resources.Limits[v1.ResourceCPU]
		memLim, hasMemLim := c.Resources.Limits[v1.ResourceMemory]

		if cs.Resources == nil {
			if hasCPUReq || hasCPULim || hasMemLim {
				// Container status is missing Resources information, but the container does
				// have resizable resources configured.
				klog.ErrorS(nil, "Missing runtime resources information for resizing container",
					"pod", format.Pod(allocatedPod), "container", c.Name)
				return false // We don't want to clear resize status with insufficient information.
			} else {
				// No resizable resources configured; this might be ok.
				return true
			}
		}

		// Only compare resizeable resources, and only compare resources that are explicitly configured.
		if hasCPUReq {
			if cs.Resources.CPURequest == nil {
				if !cpuReq.IsZero() {
					return false
				}
			} else if !cpuReq.Equal(*cs.Resources.CPURequest) &&
				(cpuReq.MilliValue() > cm.MinShares || cs.Resources.CPURequest.MilliValue() > cm.MinShares) {
				// If both allocated & status CPU requests are at or below MinShares then they are considered equal.
				return false
			}
		}
		if hasCPULim {
			if cs.Resources.CPULimit == nil {
				if !cpuLim.IsZero() {
					return false
				}
			} else if !cpuLim.Equal(*cs.Resources.CPULimit) &&
				(cpuLim.MilliValue() > cm.MinMilliCPULimit || cs.Resources.CPULimit.MilliValue() > cm.MinMilliCPULimit) {
				// If both allocated & status CPU limits are at or below the minimum limit, then they are considered equal.
				return false
			}
		}
		if hasMemLim {
			if cs.Resources.MemoryLimit == nil {
				if !memLim.IsZero() {
					return false
				}
			} else if !memLim.Equal(*cs.Resources.MemoryLimit) {
				return false
			}
		}
	}

	return true
}

// generateAPIPodStatus creates the final API pod status for a pod, given the
// internal pod status. This method should only be called from within sync*Pod methods.
func (kl *Kubelet) generateAPIPodStatus(pod *v1.Pod, podStatus *kubecontainer.PodStatus, podIsTerminal bool) v1.PodStatus {
	klog.V(3).InfoS("Generating pod status", "podIsTerminal", podIsTerminal, "pod", klog.KObj(pod))
	// use the previous pod status, or the api status, as the basis for this pod
	oldPodStatus, found := kl.statusManager.GetPodStatus(pod.UID)
	if !found {
		oldPodStatus = pod.Status
	}
	s := kl.convertStatusToAPIStatus(pod, podStatus, oldPodStatus)
	if utilfeature.DefaultFeatureGate.Enabled(features.InPlacePodVerticalScaling) {
		s.Resize = kl.determinePodResizeStatus(pod, podStatus, podIsTerminal)
	}
	// calculate the next phase and preserve reason
	allStatus := append(append([]v1.ContainerStatus{}, s.ContainerStatuses...), s.InitContainerStatuses...)
	s.Phase = getPhase(pod, allStatus, podIsTerminal)
	klog.V(4).InfoS("Got phase for pod", "pod", klog.KObj(pod), "oldPhase", oldPodStatus.Phase, "phase", s.Phase)

	// Perform a three-way merge between the statuses from the status manager,
	// runtime, and generated status to ensure terminal status is correctly set.
	if s.Phase != v1.PodFailed && s.Phase != v1.PodSucceeded {
		switch {
		case oldPodStatus.Phase == v1.PodFailed || oldPodStatus.Phase == v1.PodSucceeded:
			klog.V(4).InfoS("Status manager phase was terminal, updating phase to match", "pod", klog.KObj(pod), "phase", oldPodStatus.Phase)
			s.Phase = oldPodStatus.Phase
		case pod.Status.Phase == v1.PodFailed || pod.Status.Phase == v1.PodSucceeded:
			klog.V(4).InfoS("API phase was terminal, updating phase to match", "pod", klog.KObj(pod), "phase", pod.Status.Phase)
			s.Phase = pod.Status.Phase
		}
	}

	if s.Phase == oldPodStatus.Phase {
		// preserve the reason and message which is associated with the phase
		s.Reason = oldPodStatus.Reason
		s.Message = oldPodStatus.Message
		if len(s.Reason) == 0 {
			s.Reason = pod.Status.Reason
		}
		if len(s.Message) == 0 {
			s.Message = pod.Status.Message
		}
	}

	// check if an internal module has requested the pod is evicted and override the reason and message
	for _, podSyncHandler := range kl.PodSyncHandlers {
		if result := podSyncHandler.ShouldEvict(pod); result.Evict {
			s.Phase = v1.PodFailed
			s.Reason = result.Reason
			s.Message = result.Message
			break
		}
	}

	// pods are not allowed to transition out of terminal phases
	if pod.Status.Phase == v1.PodFailed || pod.Status.Phase == v1.PodSucceeded {
		// API server shows terminal phase; transitions are not allowed
		if s.Phase != pod.Status.Phase {
			klog.ErrorS(nil, "Pod attempted illegal phase transition", "pod", klog.KObj(pod), "originalStatusPhase", pod.Status.Phase, "apiStatusPhase", s.Phase, "apiStatus", s)
			// Force back to phase from the API server
			s.Phase = pod.Status.Phase
		}
	}

	// ensure the probe managers have up to date status for containers
	kl.probeManager.UpdatePodStatus(pod, s)

	// update the allocated resources status
	if utilfeature.DefaultFeatureGate.Enabled(features.ResourceHealthStatus) {
		kl.containerManager.UpdateAllocatedResourcesStatus(pod, s)
	}

	// preserve all conditions not owned by the kubelet
	s.Conditions = make([]v1.PodCondition, 0, len(pod.Status.Conditions)+1)
	for _, c := range pod.Status.Conditions {
		if !kubetypes.PodConditionByKubelet(c.Type) {
			s.Conditions = append(s.Conditions, c)
		}
	}

	// copy over the pod disruption conditions from state which is already
	// updated during the eviciton (due to either node resource pressure or
	// node graceful shutdown). We do not re-generate the conditions based
	// on the container statuses as they are added based on one-time events.
	cType := v1.DisruptionTarget
	if _, condition := podutil.GetPodConditionFromList(oldPodStatus.Conditions, cType); condition != nil {
		s.Conditions = utilpod.ReplaceOrAppendPodCondition(s.Conditions, condition)
	}

	// set all Kubelet-owned conditions
	if utilfeature.DefaultFeatureGate.Enabled(features.PodReadyToStartContainersCondition) {
		s.Conditions = append(s.Conditions, status.GeneratePodReadyToStartContainersCondition(pod, podStatus))
	}
	allContainerStatuses := append(s.InitContainerStatuses, s.ContainerStatuses...)
	s.Conditions = append(s.Conditions, status.GeneratePodInitializedCondition(&pod.Spec, allContainerStatuses, s.Phase))
	s.Conditions = append(s.Conditions, status.GeneratePodReadyCondition(&pod.Spec, s.Conditions, allContainerStatuses, s.Phase))
	s.Conditions = append(s.Conditions, status.GenerateContainersReadyCondition(&pod.Spec, allContainerStatuses, s.Phase))
	s.Conditions = append(s.Conditions, v1.PodCondition{
		Type:   v1.PodScheduled,
		Status: v1.ConditionTrue,
	})
	// set HostIP/HostIPs and initialize PodIP/PodIPs for host network pods
	if kl.kubeClient != nil {
		hostIPs, err := kl.getHostIPsAnyWay()
		if err != nil {
			klog.V(4).InfoS("Cannot get host IPs", "err", err)
		} else {
			if s.HostIP != "" {
				if utilnet.IPFamilyOfString(s.HostIP) != utilnet.IPFamilyOf(hostIPs[0]) {
					kl.recorder.Eventf(pod, v1.EventTypeWarning, "HostIPsIPFamilyMismatch",
						"Kubelet detected an IPv%s node IP (%s), but the cloud provider selected an IPv%s node IP (%s); pass an explicit `--node-ip` to kubelet to fix this.",
						utilnet.IPFamilyOfString(s.HostIP), s.HostIP, utilnet.IPFamilyOf(hostIPs[0]), hostIPs[0].String())
				}
			}
			s.HostIP = hostIPs[0].String()
			s.HostIPs = []v1.HostIP{{IP: s.HostIP}}
			if len(hostIPs) == 2 {
				s.HostIPs = append(s.HostIPs, v1.HostIP{IP: hostIPs[1].String()})
			}

			// HostNetwork Pods inherit the node IPs as PodIPs. They are immutable once set,
			// other than that if the node becomes dual-stack, we add the secondary IP.
			if kubecontainer.IsHostNetworkPod(pod) {
				// Primary IP is not set
				if s.PodIP == "" {
					s.PodIP = hostIPs[0].String()
					s.PodIPs = []v1.PodIP{{IP: s.PodIP}}
				}
				// Secondary IP is not set #105320
				if len(hostIPs) == 2 && len(s.PodIPs) == 1 {
					if utilnet.IPFamilyOfString(s.PodIPs[0].IP) != utilnet.IPFamilyOf(hostIPs[1]) {
						s.PodIPs = append(s.PodIPs, v1.PodIP{IP: hostIPs[1].String()})
					}
				}
			}
		}
	}

	return *s
}

// sortPodIPs return the PodIPs sorted and truncated by the cluster IP family preference.
// The runtime pod status may have an arbitrary number of IPs, in an arbitrary order.
// PodIPs are obtained by: func (m *kubeGenericRuntimeManager) determinePodSandboxIPs()
// Pick out the first returned IP of the same IP family as the node IP
// first, followed by the first IP of the opposite IP family (if any)
// and use them for the Pod.Status.PodIPs and the Downward API environment variables
func (kl *Kubelet) sortPodIPs(podIPs []string) []string {
	ips := make([]string, 0, 2)
	var validPrimaryIP, validSecondaryIP func(ip string) bool
	if len(kl.nodeIPs) == 0 || utilnet.IsIPv4(kl.nodeIPs[0]) {
		validPrimaryIP = utilnet.IsIPv4String
		validSecondaryIP = utilnet.IsIPv6String
	} else {
		validPrimaryIP = utilnet.IsIPv6String
		validSecondaryIP = utilnet.IsIPv4String
	}
	for _, ip := range podIPs {
		if validPrimaryIP(ip) {
			ips = append(ips, ip)
			break
		}
	}
	for _, ip := range podIPs {
		if validSecondaryIP(ip) {
			ips = append(ips, ip)
			break
		}
	}
	return ips
}

// convertStatusToAPIStatus initialize an api PodStatus for the given pod from
// the given internal pod status and the previous state of the pod from the API.
// It is purely transformative and does not alter the kubelet state at all.
func (kl *Kubelet) convertStatusToAPIStatus(pod *v1.Pod, podStatus *kubecontainer.PodStatus, oldPodStatus v1.PodStatus) *v1.PodStatus {
	var apiPodStatus v1.PodStatus

	// copy pod status IPs to avoid race conditions with PodStatus #102806
	podIPs := make([]string, len(podStatus.IPs))
	copy(podIPs, podStatus.IPs)

	// make podIPs order match node IP family preference #97979
	podIPs = kl.sortPodIPs(podIPs)
	for _, ip := range podIPs {
		apiPodStatus.PodIPs = append(apiPodStatus.PodIPs, v1.PodIP{IP: ip})
	}
	if len(apiPodStatus.PodIPs) > 0 {
		apiPodStatus.PodIP = apiPodStatus.PodIPs[0].IP
	}

	// set status for Pods created on versions of kube older than 1.6
	apiPodStatus.QOSClass = v1qos.GetPodQOS(pod)

	apiPodStatus.ContainerStatuses = kl.convertToAPIContainerStatuses(
		pod, podStatus,
		oldPodStatus.ContainerStatuses,
		pod.Spec.Containers,
		len(pod.Spec.InitContainers) > 0,
		false,
	)
	apiPodStatus.InitContainerStatuses = kl.convertToAPIContainerStatuses(
		pod, podStatus,
		oldPodStatus.InitContainerStatuses,
		pod.Spec.InitContainers,
		len(pod.Spec.InitContainers) > 0,
		true,
	)
	var ecSpecs []v1.Container
	for i := range pod.Spec.EphemeralContainers {
		ecSpecs = append(ecSpecs, v1.Container(pod.Spec.EphemeralContainers[i].EphemeralContainerCommon))
	}

	// #80875: By now we've iterated podStatus 3 times. We could refactor this to make a single
	// pass through podStatus.ContainerStatuses
	apiPodStatus.EphemeralContainerStatuses = kl.convertToAPIContainerStatuses(
		pod, podStatus,
		oldPodStatus.EphemeralContainerStatuses,
		ecSpecs,
		len(pod.Spec.InitContainers) > 0,
		false,
	)

	return &apiPodStatus
}

// convertToAPIContainerStatuses converts the given internal container
// statuses into API container statuses.
func (kl *Kubelet) convertToAPIContainerStatuses(pod *v1.Pod, podStatus *kubecontainer.PodStatus, previousStatus []v1.ContainerStatus, containers []v1.Container, hasInitContainers, isInitContainer bool) []v1.ContainerStatus {
	convertContainerStatus := func(cs *kubecontainer.Status, oldStatus *v1.ContainerStatus) *v1.ContainerStatus {
		cid := cs.ID.String()
		status := &v1.ContainerStatus{
			Name:         cs.Name,
			RestartCount: int32(cs.RestartCount),
			Image:        cs.Image,
			// Converting the digested image ref to the Kubernetes public
			// ContainerStatus.ImageID is historically intentional and should
			// not change.
			ImageID:     cs.ImageRef,
			ContainerID: cid,
		}
		if oldStatus != nil {
			status.VolumeMounts = oldStatus.VolumeMounts // immutable
		}
		switch {
		case cs.State == kubecontainer.ContainerStateRunning:
			status.State.Running = &v1.ContainerStateRunning{StartedAt: metav1.NewTime(cs.StartedAt)}
		case cs.State == kubecontainer.ContainerStateCreated:
			// containers that are created but not running are "waiting to be running"
			status.State.Waiting = &v1.ContainerStateWaiting{}
		case cs.State == kubecontainer.ContainerStateExited:
			status.State.Terminated = &v1.ContainerStateTerminated{
				ExitCode:    int32(cs.ExitCode),
				Reason:      cs.Reason,
				Message:     cs.Message,
				StartedAt:   metav1.NewTime(cs.StartedAt),
				FinishedAt:  metav1.NewTime(cs.FinishedAt),
				ContainerID: cid,
			}

		case cs.State == kubecontainer.ContainerStateUnknown &&
			oldStatus != nil && // we have an old status
			oldStatus.State.Running != nil: // our previous status was running
			// if this happens, then we know that this container was previously running and isn't anymore (assuming the CRI isn't failing to return running containers).
			// you can imagine this happening in cases where a container failed and the kubelet didn't ask about it in time to see the result.
			// in this case, the container should not to into waiting state immediately because that can make cases like runonce pods actually run
			// twice. "container never ran" is different than "container ran and failed".  This is handled differently in the kubelet
			// and it is handled differently in higher order logic like crashloop detection and handling
			status.State.Terminated = &v1.ContainerStateTerminated{
				Reason:   kubecontainer.ContainerReasonStatusUnknown,
				Message:  "The container could not be located when the pod was terminated",
				ExitCode: 137, // this code indicates an error
			}
			// the restart count normally comes from the CRI (see near the top of this method), but since this is being added explicitly
			// for the case where the CRI did not return a status, we need to manually increment the restart count to be accurate.
			status.RestartCount = oldStatus.RestartCount + 1

		default:
			// this collapses any unknown state to container waiting.  If any container is waiting, then the pod status moves to pending even if it is running.
			// if I'm reading this correctly, then any failure to read status on any container results in the entire pod going pending even if the containers
			// are actually running.
			// see https://github.com/kubernetes/kubernetes/blob/5d1b3e26af73dde33ecb6a3e69fb5876ceab192f/pkg/kubelet/kuberuntime/kuberuntime_container.go#L497 to
			// https://github.com/kubernetes/kubernetes/blob/8976e3620f8963e72084971d9d4decbd026bf49f/pkg/kubelet/kuberuntime/helpers.go#L58-L71
			// and interpreted here https://github.com/kubernetes/kubernetes/blob/b27e78f590a0d43e4a23ca3b2bf1739ca4c6e109/pkg/kubelet/kubelet_pods.go#L1434-L1439
			status.State.Waiting = &v1.ContainerStateWaiting{}
		}
		return status
	}

	convertContainerStatusResources := func(cName string, status *v1.ContainerStatus, cStatus *kubecontainer.Status, oldStatuses map[string]v1.ContainerStatus) *v1.ResourceRequirements {
		// oldStatus should always exist if container is running
		oldStatus, oldStatusFound := oldStatuses[cName]

		// If the new status is missing resources, then if the container is running and previous
		// status was also running, preserve the resources previously reported.
		preserveOldResourcesValue := func(rName v1.ResourceName, oldStatusResource, resource v1.ResourceList) {
			if cStatus.State == kubecontainer.ContainerStateRunning &&
				oldStatusFound && oldStatus.State.Running != nil &&
				status.ContainerID == oldStatus.ContainerID &&
				oldStatusResource != nil {
				if r, exists := oldStatusResource[rName]; exists {
					resource[rName] = r.DeepCopy()
				}
			}
		}

		// Always set the status to the latest allocated resources, even if it differs from the
		// allocation used by the current sync loop.
		alloc, found := kl.statusManager.GetContainerResourceAllocation(string(pod.UID), cName)
		if !found {
			// This case is expected for non-resizable containers (ephemeral & non-restartable init containers).
			// Don't set status.Resources in this case.
			return nil
		}
		if cStatus.State != kubecontainer.ContainerStateRunning {
			// If the container isn't running, just use the allocated resources.
			return &alloc
		}
		if oldStatus.Resources == nil {
			oldStatus.Resources = &v1.ResourceRequirements{}
		}

		// Status resources default to the allocated resources.
		// For non-running containers this will be the reported values.
		// For non-resizable resources, these values will also be used.
		resources := alloc
		if resources.Limits != nil {
			if cStatus.Resources != nil && cStatus.Resources.CPULimit != nil {
				// If both the allocated & actual resources are at or below the minimum effective limit, preserve the
				// allocated value in the API to avoid confusion and simplify comparisons.
				if cStatus.Resources.CPULimit.MilliValue() > cm.MinMilliCPULimit ||
					resources.Limits.Cpu().MilliValue() > cm.MinMilliCPULimit {
					resources.Limits[v1.ResourceCPU] = cStatus.Resources.CPULimit.DeepCopy()
				}
			} else {
				preserveOldResourcesValue(v1.ResourceCPU, oldStatus.Resources.Limits, resources.Limits)
			}
			if cStatus.Resources != nil && cStatus.Resources.MemoryLimit != nil {
				resources.Limits[v1.ResourceMemory] = cStatus.Resources.MemoryLimit.DeepCopy()
			} else {
				preserveOldResourcesValue(v1.ResourceMemory, oldStatus.Resources.Limits, resources.Limits)
			}
		}
		if resources.Requests != nil {
			if cStatus.Resources != nil && cStatus.Resources.CPURequest != nil {
				// If both the allocated & actual resources are at or below MinShares, preserve the
				// allocated value in the API to avoid confusion and simplify comparisons.
				if cStatus.Resources.CPURequest.MilliValue() > cm.MinShares ||
					resources.Requests.Cpu().MilliValue() > cm.MinShares {
					resources.Requests[v1.ResourceCPU] = cStatus.Resources.CPURequest.DeepCopy()
				}
			} else {
				preserveOldResourcesValue(v1.ResourceCPU, oldStatus.Resources.Requests, resources.Requests)
			}
		}

		return &resources
	}

	convertContainerStatusUser := func(cStatus *kubecontainer.Status) *v1.ContainerUser {
		if cStatus.User == nil {
			return nil
		}

		user := &v1.ContainerUser{}
		if cStatus.User.Linux != nil {
			user.Linux = &v1.LinuxContainerUser{
				UID:                cStatus.User.Linux.UID,
				GID:                cStatus.User.Linux.GID,
				SupplementalGroups: cStatus.User.Linux.SupplementalGroups,
			}
		}

		return user
	}

	// Fetch old containers statuses from old pod status.
	oldStatuses := make(map[string]v1.ContainerStatus, len(containers))
	for _, status := range previousStatus {
		oldStatuses[status.Name] = status
	}

	// Set all container statuses to default waiting state
	statuses := make(map[string]*v1.ContainerStatus, len(containers))
	defaultWaitingState := v1.ContainerState{Waiting: &v1.ContainerStateWaiting{Reason: ContainerCreating}}
	if hasInitContainers {
		defaultWaitingState = v1.ContainerState{Waiting: &v1.ContainerStateWaiting{Reason: PodInitializing}}
	}

	supportsRRO := kl.runtimeClassSupportsRecursiveReadOnlyMounts(pod)

	for _, container := range containers {
		status := &v1.ContainerStatus{
			Name:  container.Name,
			Image: container.Image,
			State: defaultWaitingState,
		}
		// status.VolumeMounts cannot be propagated from kubecontainer.Status
		// because the CRI API is unaware of the volume names.
		if utilfeature.DefaultFeatureGate.Enabled(features.RecursiveReadOnlyMounts) {
			for _, vol := range container.VolumeMounts {
				volStatus := v1.VolumeMountStatus{
					Name:      vol.Name,
					MountPath: vol.MountPath,
					ReadOnly:  vol.ReadOnly,
				}
				if vol.ReadOnly {
					rroMode := v1.RecursiveReadOnlyDisabled
					if b, err := resolveRecursiveReadOnly(vol, supportsRRO); err != nil {
						klog.ErrorS(err, "failed to resolve recursive read-only mode", "mode", *vol.RecursiveReadOnly)
					} else if b {
						if utilfeature.DefaultFeatureGate.Enabled(features.RecursiveReadOnlyMounts) {
							rroMode = v1.RecursiveReadOnlyEnabled
						} else {
							klog.ErrorS(nil, "recursive read-only mount needs feature gate to be enabled",
								"featureGate", features.RecursiveReadOnlyMounts)
						}
					}
					volStatus.RecursiveReadOnly = &rroMode // Disabled or Enabled
				}
				status.VolumeMounts = append(status.VolumeMounts, volStatus)
			}
		}
		oldStatus, found := oldStatuses[container.Name]
		if found {
			if oldStatus.State.Terminated != nil {
				status = &oldStatus
			} else {
				// Apply some values from the old statuses as the default values.
				status.RestartCount = oldStatus.RestartCount
				status.LastTerminationState = oldStatus.LastTerminationState
			}
		}
		statuses[container.Name] = status
	}

	for _, container := range containers {
		found := false
		for _, cStatus := range podStatus.ContainerStatuses {
			if container.Name == cStatus.Name {
				found = true
				break
			}
		}
		if found {
			continue
		}
		// if no container is found, then assuming it should be waiting seems plausible, but the status code requires
		// that a previous termination be present.  If we're offline long enough or something removed the container, then
		// the previous termination may not be present.  This next code block ensures that if the container was previously running
		// then when that container status disappears, we can infer that it terminated even if we don't know the status code.
		// By setting the lasttermination state we are able to leave the container status waiting and present more accurate
		// data via the API.

		oldStatus, ok := oldStatuses[container.Name]
		if !ok {
			continue
		}
		if oldStatus.State.Terminated != nil {
			// if the old container status was terminated, the lasttermination status is correct
			continue
		}
		if oldStatus.State.Running == nil {
			// if the old container status isn't running, then waiting is an appropriate status and we have nothing to do
			continue
		}

		// If we're here, we know the pod was previously running, but doesn't have a terminated status. We will check now to
		// see if it's in a pending state.
		status := statuses[container.Name]
		// If the status we're about to write indicates the default, the Waiting status will force this pod back into Pending.
		// That isn't true, we know the pod was previously running.
		isDefaultWaitingStatus := status.State.Waiting != nil && status.State.Waiting.Reason == ContainerCreating
		if hasInitContainers {
			isDefaultWaitingStatus = status.State.Waiting != nil && status.State.Waiting.Reason == PodInitializing
		}
		if !isDefaultWaitingStatus {
			// the status was written, don't override
			continue
		}
		if status.LastTerminationState.Terminated != nil {
			// if we already have a termination state, nothing to do
			continue
		}

		// setting this value ensures that we show as stopped here, not as waiting:
		// https://github.com/kubernetes/kubernetes/blob/90c9f7b3e198e82a756a68ffeac978a00d606e55/pkg/kubelet/kubelet_pods.go#L1440-L1445
		// This prevents the pod from becoming pending
		status.LastTerminationState.Terminated = &v1.ContainerStateTerminated{
			Reason:   kubecontainer.ContainerReasonStatusUnknown,
			Message:  "The container could not be located when the pod was deleted.  The container used to be Running",
			ExitCode: 137,
		}

		// If the pod was not deleted, then it's been restarted. Increment restart count.
		if pod.DeletionTimestamp == nil {
			status.RestartCount += 1
		}

		statuses[container.Name] = status
	}

	// Copy the slice before sorting it
	containerStatusesCopy := make([]*kubecontainer.Status, len(podStatus.ContainerStatuses))
	copy(containerStatusesCopy, podStatus.ContainerStatuses)

	// Make the latest container status comes first.
	sort.Sort(sort.Reverse(kubecontainer.SortContainerStatusesByCreationTime(containerStatusesCopy)))
	// Set container statuses according to the statuses seen in pod status
	containerSeen := map[string]int{}
	for _, cStatus := range containerStatusesCopy {
		cName := cStatus.Name
		if _, ok := statuses[cName]; !ok {
			// This would also ignore the infra container.
			continue
		}
		if containerSeen[cName] >= 2 {
			continue
		}
		var oldStatusPtr *v1.ContainerStatus
		if oldStatus, ok := oldStatuses[cName]; ok {
			oldStatusPtr = &oldStatus
		}
		status := convertContainerStatus(cStatus, oldStatusPtr)
		if utilfeature.DefaultFeatureGate.Enabled(features.InPlacePodVerticalScaling) {
			status.Resources = convertContainerStatusResources(cName, status, cStatus, oldStatuses)

			if utilfeature.DefaultFeatureGate.Enabled(features.InPlacePodVerticalScalingAllocatedStatus) {
				if alloc, found := kl.statusManager.GetContainerResourceAllocation(string(pod.UID), cName); found {
					status.AllocatedResources = alloc.Requests
				}
			}
		}

		if utilfeature.DefaultFeatureGate.Enabled(features.SupplementalGroupsPolicy) {
			status.User = convertContainerStatusUser(cStatus)
		}
		if containerSeen[cName] == 0 {
			statuses[cName] = status
		} else {
			statuses[cName].LastTerminationState = status.State
		}
		containerSeen[cName] = containerSeen[cName] + 1
	}

	// Handle the containers failed to be started, which should be in Waiting state.
	for _, container := range containers {
		if isInitContainer {
			// If the init container is terminated with exit code 0, it won't be restarted.
			// TODO(random-liu): Handle this in a cleaner way.
			s := podStatus.FindContainerStatusByName(container.Name)
			if s != nil && s.State == kubecontainer.ContainerStateExited && s.ExitCode == 0 {
				continue
			}
		}
		// If a container should be restarted in next syncpod, it is *Waiting*.
		if !kubecontainer.ShouldContainerBeRestarted(&container, pod, podStatus) {
			continue
		}
		status := statuses[container.Name]
		reason, ok := kl.reasonCache.Get(pod.UID, container.Name)
		if !ok {
			// In fact, we could also apply Waiting state here, but it is less informative,
			// and the container will be restarted soon, so we prefer the original state here.
			// Note that with the current implementation of ShouldContainerBeRestarted the original state here
			// could be:
			//   * Waiting: There is no associated historical container and start failure reason record.
			//   * Terminated: The container is terminated.
			continue
		}
		if status.State.Terminated != nil {
			status.LastTerminationState = status.State
		}
		status.State = v1.ContainerState{
			Waiting: &v1.ContainerStateWaiting{
				Reason:  reason.Err.Error(),
				Message: reason.Message,
			},
		}
		statuses[container.Name] = status
	}

	// Sort the container statuses since clients of this interface expect the list
	// of containers in a pod has a deterministic order.
	if isInitContainer {
		return kubetypes.SortStatusesOfInitContainers(pod, statuses)
	}
	containerStatuses := make([]v1.ContainerStatus, 0, len(statuses))
	for _, status := range statuses {
		containerStatuses = append(containerStatuses, *status)
	}

	sort.Sort(kubetypes.SortedContainerStatuses(containerStatuses))
	return containerStatuses
}

// ServeLogs returns logs of current machine.
func (kl *Kubelet) ServeLogs(w http.ResponseWriter, req *http.Request) {
	// TODO: allowlist logs we are willing to serve
	kl.logServer.ServeHTTP(w, req)
}

// findContainer finds and returns the container with the given pod ID, full name, and container name.
// It returns nil if not found.
func (kl *Kubelet) findContainer(ctx context.Context, podFullName string, podUID types.UID, containerName string) (*kubecontainer.Container, error) {
	pods, err := kl.containerRuntime.GetPods(ctx, false)
	if err != nil {
		return nil, err
	}
	// Resolve and type convert back again.
	// We need the static pod UID but the kubecontainer API works with types.UID.
	podUID = types.UID(kl.podManager.TranslatePodUID(podUID))
	pod := kubecontainer.Pods(pods).FindPod(podFullName, podUID)
	return pod.FindContainerByName(containerName), nil
}

// RunInContainer runs a command in a container, returns the combined stdout, stderr as an array of bytes
func (kl *Kubelet) RunInContainer(ctx context.Context, podFullName string, podUID types.UID, containerName string, cmd []string) ([]byte, error) {
	container, err := kl.findContainer(ctx, podFullName, podUID, containerName)
	if err != nil {
		return nil, err
	}
	if container == nil {
		return nil, fmt.Errorf("container not found (%q)", containerName)
	}
	// TODO(tallclair): Pass a proper timeout value.
	return kl.runner.RunInContainer(ctx, container.ID, cmd, 0)
}

// GetExec gets the URL the exec will be served from, or nil if the Kubelet will serve it.
func (kl *Kubelet) GetExec(ctx context.Context, podFullName string, podUID types.UID, containerName string, cmd []string, streamOpts remotecommandserver.Options) (*url.URL, error) {
	container, err := kl.findContainer(ctx, podFullName, podUID, containerName)
	if err != nil {
		return nil, err
	}
	if container == nil {
		return nil, fmt.Errorf("container not found (%q)", containerName)
	}
	return kl.streamingRuntime.GetExec(ctx, container.ID, cmd, streamOpts.Stdin, streamOpts.Stdout, streamOpts.Stderr, streamOpts.TTY)
}

// GetAttach gets the URL the attach will be served from, or nil if the Kubelet will serve it.
func (kl *Kubelet) GetAttach(ctx context.Context, podFullName string, podUID types.UID, containerName string, streamOpts remotecommandserver.Options) (*url.URL, error) {
	container, err := kl.findContainer(ctx, podFullName, podUID, containerName)
	if err != nil {
		return nil, err
	}
	if container == nil {
		return nil, fmt.Errorf("container %s not found in pod %s", containerName, podFullName)
	}

	// The TTY setting for attach must match the TTY setting in the initial container configuration,
	// since whether the process is running in a TTY cannot be changed after it has started.  We
	// need the api.Pod to get the TTY status.
	pod, found := kl.GetPodByFullName(podFullName)
	if !found || (string(podUID) != "" && pod.UID != podUID) {
		return nil, fmt.Errorf("pod %s not found", podFullName)
	}
	containerSpec := kubecontainer.GetContainerSpec(pod, containerName)
	if containerSpec == nil {
		return nil, fmt.Errorf("container %s not found in pod %s", containerName, podFullName)
	}
	tty := containerSpec.TTY

	return kl.streamingRuntime.GetAttach(ctx, container.ID, streamOpts.Stdin, streamOpts.Stdout, streamOpts.Stderr, tty)
}

// GetPortForward gets the URL the port-forward will be served from, or nil if the Kubelet will serve it.
func (kl *Kubelet) GetPortForward(ctx context.Context, podName, podNamespace string, podUID types.UID, portForwardOpts portforward.V4Options) (*url.URL, error) {
	pods, err := kl.containerRuntime.GetPods(ctx, false)
	if err != nil {
		return nil, err
	}
	// Resolve and type convert back again.
	// We need the static pod UID but the kubecontainer API works with types.UID.
	podUID = types.UID(kl.podManager.TranslatePodUID(podUID))
	podFullName := kubecontainer.BuildPodFullName(podName, podNamespace)
	pod := kubecontainer.Pods(pods).FindPod(podFullName, podUID)
	if pod.IsEmpty() {
		return nil, fmt.Errorf("pod not found (%q)", podFullName)
	}

	return kl.streamingRuntime.GetPortForward(ctx, podName, podNamespace, podUID, portForwardOpts.Ports)
}

// cleanupOrphanedPodCgroups removes cgroups that should no longer exist.
// it reconciles the cached state of cgroupPods with the specified list of runningPods
func (kl *Kubelet) cleanupOrphanedPodCgroups(pcm cm.PodContainerManager, cgroupPods map[types.UID]cm.CgroupName, possiblyRunningPods map[types.UID]sets.Empty) {
	// Iterate over all the found pods to verify if they should be running
	for uid, val := range cgroupPods {
		// if the pod is in the running set, its not a candidate for cleanup
		if _, ok := possiblyRunningPods[uid]; ok {
			continue
		}

		// If volumes have not been unmounted/detached, do not delete the cgroup
		// so any memory backed volumes don't have their charges propagated to the
		// parent croup.  If the volumes still exist, reduce the cpu shares for any
		// process in the cgroup to the minimum value while we wait.
		if podVolumesExist := kl.podVolumesExist(uid); podVolumesExist {
			klog.V(3).InfoS("Orphaned pod found, but volumes not yet removed.  Reducing cpu to minimum", "podUID", uid)
			if err := pcm.ReduceCPULimits(val); err != nil {
				klog.InfoS("Failed to reduce cpu time for pod pending volume cleanup", "podUID", uid, "err", err)
			}
			continue
		}
		klog.V(3).InfoS("Orphaned pod found, removing pod cgroups", "podUID", uid)
		// Destroy all cgroups of pod that should not be running,
		// by first killing all the attached processes to these cgroups.
		// We ignore errors thrown by the method, as the housekeeping loop would
		// again try to delete these unwanted pod cgroups
		go pcm.Destroy(val)
	}
}

func (kl *Kubelet) runtimeClassSupportsRecursiveReadOnlyMounts(pod *v1.Pod) bool {
	if kl.runtimeClassManager == nil {
		return false
	}
	runtimeHandlerName, err := kl.runtimeClassManager.LookupRuntimeHandler(pod.Spec.RuntimeClassName)
	if err != nil {
		klog.ErrorS(err, "failed to look up the runtime handler", "runtimeClassName", pod.Spec.RuntimeClassName)
		return false
	}
	runtimeHandlers := kl.runtimeState.runtimeHandlers()
	return runtimeHandlerSupportsRecursiveReadOnlyMounts(runtimeHandlerName, runtimeHandlers)
}

// runtimeHandlerSupportsRecursiveReadOnlyMounts checks whether the runtime handler supports recursive read-only mounts.
// The kubelet feature gate is not checked here.
func runtimeHandlerSupportsRecursiveReadOnlyMounts(runtimeHandlerName string, runtimeHandlers []kubecontainer.RuntimeHandler) bool {
	if len(runtimeHandlers) == 0 {
		// The runtime does not support returning the handler list.
		// No need to print a warning here.
		return false
	}
	for _, h := range runtimeHandlers {
		if h.Name == runtimeHandlerName {
			return h.SupportsRecursiveReadOnlyMounts
		}
	}
	klog.ErrorS(nil, "Unknown runtime handler", "runtimeHandlerName", runtimeHandlerName)
	return false
}

// resolveRecursiveReadOnly resolves the recursive read-only mount mode.
func resolveRecursiveReadOnly(m v1.VolumeMount, runtimeSupportsRRO bool) (bool, error) {
	if m.RecursiveReadOnly == nil || *m.RecursiveReadOnly == v1.RecursiveReadOnlyDisabled {
		return false, nil
	}
	if !m.ReadOnly {
		return false, fmt.Errorf("volume %q requested recursive read-only mode, but it is not read-only", m.Name)
	}
	if m.MountPropagation != nil && *m.MountPropagation != v1.MountPropagationNone {
		return false, fmt.Errorf("volume %q requested recursive read-only mode, but it is not compatible with propagation %q",
			m.Name, *m.MountPropagation)
	}
	switch rroMode := *m.RecursiveReadOnly; rroMode {
	case v1.RecursiveReadOnlyIfPossible:
		return runtimeSupportsRRO, nil
	case v1.RecursiveReadOnlyEnabled:
		if !runtimeSupportsRRO {
			return false, fmt.Errorf("volume %q requested recursive read-only mode, but it is not supported by the runtime", m.Name)
		}
		return true, nil
	default:
		return false, fmt.Errorf("unknown recursive read-only mode %q", rroMode)
	}
}
