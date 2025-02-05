/*
Copyright 2021 The Kubernetes Authors.

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

package pod

import (
	"flag"
	"fmt"
	"strings"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	v1 "k8s.io/api/core/v1"
	"k8s.io/kubernetes/test/e2e/framework"
	imageutils "k8s.io/kubernetes/test/utils/image"
	psaapi "k8s.io/pod-security-admission/api"
	psapolicy "k8s.io/pod-security-admission/policy"
	"k8s.io/utils/pointer"
)

// NodeOSDistroIs returns true if the distro is the same as `--node-os-distro`
// the package framework/pod can't import the framework package (see #81245)
// we need to check if the --node-os-distro=windows is set and the framework package
// is the one that's parsing the flags, as a workaround this method is looking for the same flag again
// TODO: replace with `framework.NodeOSDistroIs` when #81245 is complete
func NodeOSDistroIs(distro string) bool {
	var nodeOsDistro *flag.Flag = flag.Lookup("node-os-distro")
	if nodeOsDistro != nil && nodeOsDistro.Value.String() == distro {
		return true
	}
	return false
}

const InfiniteSleepCommand = "trap exit TERM; while true; do sleep 1; done"

// GenerateScriptCmd generates the corresponding command lines to execute a command.
func GenerateScriptCmd(command string) []string {
	return []string{"/bin/sh", "-c", command}
}

// GetDefaultTestImage returns the default test image based on OS.
// If the node OS is windows, currently we return Agnhost image for Windows node
// due to the issue of #https://github.com/kubernetes-sigs/windows-testing/pull/35.
// If the node OS is linux, return busybox image
func GetDefaultTestImage() string {
	return imageutils.GetE2EImage(GetDefaultTestImageID())
}

// GetDefaultTestImageID returns the default test image id based on OS.
// If the node OS is windows, currently we return Agnhost image for Windows node
// due to the issue of #https://github.com/kubernetes-sigs/windows-testing/pull/35.
// If the node OS is linux, return busybox image
func GetDefaultTestImageID() imageutils.ImageID {
	return GetTestImageID(imageutils.BusyBox)
}

// GetTestImage returns the image name with the given input
// If the Node OS is windows, currently we return Agnhost image for Windows node
// due to the issue of #https://github.com/kubernetes-sigs/windows-testing/pull/35.
func GetTestImage(id imageutils.ImageID) string {
	if NodeOSDistroIs("windows") {
		return imageutils.GetE2EImage(imageutils.Agnhost)
	}
	return imageutils.GetE2EImage(id)
}

// GetTestImageID returns the image id with the given input
// If the Node OS is windows, currently we return Agnhost image for Windows node
// due to the issue of #https://github.com/kubernetes-sigs/windows-testing/pull/35.
func GetTestImageID(id imageutils.ImageID) imageutils.ImageID {
	if NodeOSDistroIs("windows") {
		return imageutils.Agnhost
	}
	return id
}

// GetDefaultNonRootUser returns default non root user
// If the Node OS is windows, we return nill due to issue with invalid permissions set on projected volumes
// https://github.com/kubernetes/kubernetes/issues/102849
func GetDefaultNonRootUser() *int64 {
	if NodeOSDistroIs("windows") {
		return nil
	}
	return pointer.Int64(DefaultNonRootUser)
}

// GeneratePodSecurityContext generates the corresponding pod security context with the given inputs
// If the Node OS is windows, currently we will ignore the inputs and return nil.
// TODO: Will modify it after windows has its own security context
func GeneratePodSecurityContext(fsGroup *int64, seLinuxOptions *v1.SELinuxOptions) *v1.PodSecurityContext {
	if NodeOSDistroIs("windows") {
		return nil
	}
	return &v1.PodSecurityContext{
		FSGroup:        fsGroup,
		SELinuxOptions: seLinuxOptions,
	}
}

// GenerateContainerSecurityContext generates the corresponding container security context with the given inputs
// If the Node OS is windows, currently we will ignore the inputs and return nil.
// TODO: Will modify it after windows has its own security context
func GenerateContainerSecurityContext(level psaapi.Level) *v1.SecurityContext {
	if NodeOSDistroIs("windows") {
		return nil
	}

	switch level {
	case psaapi.LevelBaseline:
		return &v1.SecurityContext{
			Privileged: pointer.Bool(false),
		}
	case psaapi.LevelPrivileged:
		return &v1.SecurityContext{
			Privileged: pointer.Bool(true),
		}
	case psaapi.LevelRestricted:
		return GetRestrictedContainerSecurityContext()
	default:
		ginkgo.Fail(fmt.Sprintf("unknown k8s.io/pod-security-admission/policy.Level %q", level))
		panic("not reached")
	}
}

// GetLinuxLabel returns the default SELinuxLabel based on OS.
// If the node OS is windows, it will return nil
func GetLinuxLabel() *v1.SELinuxOptions {
	if NodeOSDistroIs("windows") {
		return nil
	}
	return &v1.SELinuxOptions{
		Level: "s0:c0,c1"}
}

// DefaultNonRootUser is the default user ID used for running restricted (non-root) containers.
const DefaultNonRootUser = 1000

// DefaultNonRootUserName is the default username in Windows used for running restricted (non-root) containers
const DefaultNonRootUserName = "ContainerUser"

// GetRestrictedPodSecurityContext returns a restricted pod security context.
// This includes setting RunAsUser for convenience, to pass the RunAsNonRoot check.
// Tests that require a specific user ID should override this.
func GetRestrictedPodSecurityContext() *v1.PodSecurityContext {
	psc := &v1.PodSecurityContext{
		RunAsNonRoot:   pointer.Bool(true),
		RunAsUser:      GetDefaultNonRootUser(),
		SeccompProfile: &v1.SeccompProfile{Type: v1.SeccompProfileTypeRuntimeDefault},
	}

	if NodeOSDistroIs("windows") {
		psc.WindowsOptions = &v1.WindowsSecurityContextOptions{}
		psc.WindowsOptions.RunAsUserName = pointer.String(DefaultNonRootUserName)
	}

	return psc
}

// GetRestrictedContainerSecurityContext returns a minimal restricted container security context.
func GetRestrictedContainerSecurityContext() *v1.SecurityContext {
	return &v1.SecurityContext{
		AllowPrivilegeEscalation: pointer.Bool(false),
		Capabilities:             &v1.Capabilities{Drop: []v1.Capability{"ALL"}},
	}
}

var psaEvaluator, _ = psapolicy.NewEvaluator(psapolicy.DefaultChecks())

// MustMixinRestrictedPodSecurity makes the given pod compliant with the restricted pod security level.
// If doing so would overwrite existing non-conformant configuration, a test failure is triggered.
func MustMixinRestrictedPodSecurity(pod *v1.Pod) *v1.Pod {
	err := MixinRestrictedPodSecurity(pod)
	gomega.ExpectWithOffset(1, err).NotTo(gomega.HaveOccurred())
	return pod
}

// MixinRestrictedPodSecurity makes the given pod compliant with the restricted pod security level.
// If doing so would overwrite existing non-conformant configuration, an error is returned.
// Note that this sets a default RunAsUser. See GetRestrictedPodSecurityContext.
// TODO(#105919): Handle PodOS for windows pods.
func MixinRestrictedPodSecurity(pod *v1.Pod) error {
	if pod.Spec.SecurityContext == nil {
		pod.Spec.SecurityContext = GetRestrictedPodSecurityContext()
	} else {
		if pod.Spec.SecurityContext.RunAsNonRoot == nil {
			pod.Spec.SecurityContext.RunAsNonRoot = pointer.Bool(true)
		}
		if pod.Spec.SecurityContext.RunAsUser == nil {
			pod.Spec.SecurityContext.RunAsUser = GetDefaultNonRootUser()
		}
		if pod.Spec.SecurityContext.SeccompProfile == nil {
			pod.Spec.SecurityContext.SeccompProfile = &v1.SeccompProfile{Type: v1.SeccompProfileTypeRuntimeDefault}
		}
		if NodeOSDistroIs("windows") && pod.Spec.SecurityContext.WindowsOptions == nil {
			pod.Spec.SecurityContext.WindowsOptions = &v1.WindowsSecurityContextOptions{}
			pod.Spec.SecurityContext.WindowsOptions.RunAsUserName = pointer.String(DefaultNonRootUserName)
		}
	}
	for i := range pod.Spec.Containers {
		mixinRestrictedContainerSecurityContext(&pod.Spec.Containers[i])
	}
	for i := range pod.Spec.InitContainers {
		mixinRestrictedContainerSecurityContext(&pod.Spec.InitContainers[i])
	}

	// Validate the resulting pod against the restricted profile.
	restricted := psaapi.LevelVersion{
		Level:   psaapi.LevelRestricted,
		Version: psaapi.LatestVersion(),
	}
	if agg := psapolicy.AggregateCheckResults(psaEvaluator.EvaluatePod(restricted, &pod.ObjectMeta, &pod.Spec)); !agg.Allowed {
		return fmt.Errorf("failed to make pod %s restricted: %s", pod.Name, agg.ForbiddenDetail())
	}

	return nil
}

// mixinRestrictedContainerSecurityContext adds the required container security context options to
// be compliant with the restricted pod security level. Non-conformance checking is handled by the
// caller.
func mixinRestrictedContainerSecurityContext(container *v1.Container) {
	if container.SecurityContext == nil {
		container.SecurityContext = GetRestrictedContainerSecurityContext()
	} else {
		if container.SecurityContext.AllowPrivilegeEscalation == nil {
			container.SecurityContext.AllowPrivilegeEscalation = pointer.Bool(false)
		}
		if container.SecurityContext.Capabilities == nil {
			container.SecurityContext.Capabilities = &v1.Capabilities{}
		}
		if len(container.SecurityContext.Capabilities.Drop) == 0 {
			container.SecurityContext.Capabilities.Drop = []v1.Capability{"ALL"}
		}
	}
}

// FindPodConditionByType loops through all pod conditions in pod status and returns the specified condition.
func FindPodConditionByType(podStatus *v1.PodStatus, conditionType v1.PodConditionType) *v1.PodCondition {
	for _, cond := range podStatus.Conditions {
		if cond.Type == conditionType {
			return &cond
		}
	}
	return nil
}

// FindContainerStatusInPod finds a container status by its name in the provided pod
func FindContainerStatusInPod(pod *v1.Pod, containerName string) *v1.ContainerStatus {
	for _, containerStatus := range pod.Status.InitContainerStatuses {
		if containerStatus.Name == containerName {
			return &containerStatus
		}
	}
	for _, containerStatus := range pod.Status.ContainerStatuses {
		if containerStatus.Name == containerName {
			return &containerStatus
		}
	}
	for _, containerStatus := range pod.Status.EphemeralContainerStatuses {
		if containerStatus.Name == containerName {
			return &containerStatus
		}
	}
	return nil
}

// VerifyCgroupValue verifies that the given cgroup path has the expected value in
// the specified container of the pod. It execs into the container to retrive the
// cgroup value and compares it against the expected value.
func VerifyCgroupValue(f *framework.Framework, pod *v1.Pod, cName, cgPath, expectedCgValue string) error {
	cmd := fmt.Sprintf("head -n 1 %s", cgPath)
	framework.Logf("Namespace %s Pod %s Container %s - looking for cgroup value %s in path %s",
		pod.Namespace, pod.Name, cName, expectedCgValue, cgPath)
	cgValue, _, err := ExecCommandInContainerWithFullOutput(f, pod.Name, cName, "/bin/sh", "-c", cmd)
	if err != nil {
		return fmt.Errorf("failed to find expected value %q in container cgroup %q", expectedCgValue, cgPath)
	}
	cgValue = strings.Trim(cgValue, "\n")
	if cgValue != expectedCgValue {
		return fmt.Errorf("cgroup value %q not equal to expected %q", cgValue, expectedCgValue)
	}
	return nil
}

// IsPodOnCgroupv2Node checks whether the pod is running on cgroupv2 node.
// TODO: Deduplicate this function with NPD cluster e2e test:
// https://github.com/kubernetes/kubernetes/blob/2049360379bcc5d6467769cef112e6e492d3d2f0/test/e2e/node/node_problem_detector.go#L369
func IsPodOnCgroupv2Node(f *framework.Framework, pod *v1.Pod) bool {
	cmd := "mount -t cgroup2"
	out, _, err := ExecCommandInContainerWithFullOutput(f, pod.Name, pod.Spec.Containers[0].Name, "/bin/sh", "-c", cmd)
	if err != nil {
		return false
	}
	// Some tests mount host cgroup using HostPath for verifying pod cgroup values.
	// In this case, "<mount path>/unified" is detected by "mount -t cgroup2" if cgroup hybrid mode is configured on the host.
	// So, we need to see if "/sys/fs/cgroup" is contained in the output.
	return strings.Contains(out, "/sys/fs/cgroup")
}
