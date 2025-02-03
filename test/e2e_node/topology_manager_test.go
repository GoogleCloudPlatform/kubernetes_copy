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

package e2enode

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	kubeletconfig "k8s.io/kubernetes/pkg/kubelet/apis/config"
	"k8s.io/kubernetes/pkg/kubelet/cm/cpumanager"
	"k8s.io/kubernetes/pkg/kubelet/cm/topologymanager"
	admissionapi "k8s.io/pod-security-admission/api"

	"k8s.io/kubernetes/test/e2e/feature"
	"k8s.io/kubernetes/test/e2e/framework"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"
	e2etestfiles "k8s.io/kubernetes/test/e2e/framework/testfiles"
	testutils "k8s.io/kubernetes/test/utils"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
)

const (
	numaAlignmentCommand      = `export CPULIST_ALLOWED=$( awk -F":\t*" '/Cpus_allowed_list/ { print $2 }' /proc/self/status); env;`
	numaAlignmentSleepCommand = numaAlignmentCommand + `sleep 1d;`
	podScopeTopology          = "pod"
	containerScopeTopology    = "container"

	minNumaNodes                  = 2
	minNumaNodesPreferClosestNUMA = 4
	minCoreCount                  = 4
	minSriovResource              = 7 // This is the min number of SRIOV VFs needed on the system under test.
)

// Helper for makeTopologyManagerPod().
type tmCtnAttribute struct {
	ctnName       string
	cpuRequest    string
	cpuLimit      string
	deviceName    string
	deviceRequest string
	deviceLimit   string
	restartPolicy *v1.ContainerRestartPolicy
}

func detectNUMANodes() int {
	outData, err := exec.Command("/bin/sh", "-c", "lscpu | grep \"NUMA node(s):\" | cut -d \":\" -f 2").Output()
	framework.ExpectNoError(err)

	numaNodes, err := strconv.Atoi(strings.TrimSpace(string(outData)))
	framework.ExpectNoError(err)

	return numaNodes
}

func detectCoresPerSocket() int {
	outData, err := exec.Command("/bin/sh", "-c", "lscpu | grep \"Core(s) per socket:\" | cut -d \":\" -f 2").Output()
	framework.ExpectNoError(err)

	coreCount, err := strconv.Atoi(strings.TrimSpace(string(outData)))
	framework.ExpectNoError(err)

	return coreCount
}

func detectThreadPerCore() int {
	outData, err := exec.Command("/bin/sh", "-c", "lscpu | grep \"Thread(s) per core:\" | cut -d \":\" -f 2").Output()
	framework.ExpectNoError(err)

	threadCount, err := strconv.Atoi(strings.TrimSpace(string(outData)))
	framework.ExpectNoError(err)

	return threadCount
}

// for now we only look for pairs of nodes with minimum distance, we also assume that distance table is symmetric.
func getMinRemoteDistanceForNode(nodeToDistances map[int][]int) int {
	var minDistance int = 99
	for myID, distances := range nodeToDistances {
		if len(distances) == 0 {
			continue
		}
		for nodeID, distance := range distances {
			// it'll always equal 10, which means local access.
			if myID == nodeID {
				continue
			}
			if distance < minDistance {
				minDistance = distance
			}
		}
	}

	return minDistance
}

func detectNUMADistances(numaNodes int) map[int][]int {
	ginkgo.GinkgoHelper()

	nodeToDistances := make(map[int][]int)
	for i := 0; i < numaNodes; i++ {
		outData, err := os.ReadFile(fmt.Sprintf("/sys/devices/system/node/node%d/distance", i))
		framework.ExpectNoError(err)

		nodeToDistances[i] = make([]int, 0, numaNodes)

		for _, distance := range strings.Split(strings.TrimSpace(string(outData)), " ") {
			distanceValue, err := strconv.Atoi(strings.TrimSpace(distance))
			framework.ExpectNoError(err)

			nodeToDistances[i] = append(nodeToDistances[i], distanceValue)
		}
	}

	return nodeToDistances
}

func makeContainers(ctnCmd string, ctnAttributes []tmCtnAttribute) (ctns []v1.Container) {
	for _, ctnAttr := range ctnAttributes {
		ctn := v1.Container{
			Name:  ctnAttr.ctnName,
			Image: busyboxImage,
			Resources: v1.ResourceRequirements{
				Requests: v1.ResourceList{
					v1.ResourceName(v1.ResourceCPU):    resource.MustParse(ctnAttr.cpuRequest),
					v1.ResourceName(v1.ResourceMemory): resource.MustParse("100Mi"),
				},
				Limits: v1.ResourceList{
					v1.ResourceName(v1.ResourceCPU):    resource.MustParse(ctnAttr.cpuLimit),
					v1.ResourceName(v1.ResourceMemory): resource.MustParse("100Mi"),
				},
			},
			Command:       []string{"sh", "-c", ctnCmd},
			RestartPolicy: ctnAttr.restartPolicy,
		}
		if ctnAttr.deviceName != "" {
			ctn.Resources.Requests[v1.ResourceName(ctnAttr.deviceName)] = resource.MustParse(ctnAttr.deviceRequest)
			ctn.Resources.Limits[v1.ResourceName(ctnAttr.deviceName)] = resource.MustParse(ctnAttr.deviceLimit)
		}
		ctns = append(ctns, ctn)
	}
	return
}

func makeTopologyManagerTestPod(podName string, tmCtnAttributes, tmInitCtnAttributes []tmCtnAttribute) *v1.Pod {
	var containers, initContainers []v1.Container
	for _, attr := range tmInitCtnAttributes {
		cmd := numaAlignmentCommand
		if attr.restartPolicy != nil && *attr.restartPolicy == v1.ContainerRestartPolicyAlways {
			cmd = numaAlignmentSleepCommand
		}
		initContainers = append(initContainers, makeContainers(cmd, []tmCtnAttribute{attr})...)
	}
	containers = makeContainers(numaAlignmentSleepCommand, tmCtnAttributes)

	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: podName,
		},
		Spec: v1.PodSpec{
			RestartPolicy:  v1.RestartPolicyNever,
			InitContainers: initContainers,
			Containers:     containers,
		},
	}
}

func findNUMANodeWithoutSRIOVDevicesFromConfigMap(configMap *v1.ConfigMap, numaNodes int) (int, bool) {
	for nodeNum := 0; nodeNum < numaNodes; nodeNum++ {
		value, ok := configMap.Annotations[fmt.Sprintf("pcidevice_node%d", nodeNum)]
		if !ok {
			framework.Logf("missing pcidevice annotation for NUMA node %d", nodeNum)
			return -1, false
		}
		v, err := strconv.Atoi(value)
		if err != nil {
			framework.Failf("error getting the PCI device count on NUMA node %d: %v", nodeNum, err)
		}
		if v == 0 {
			framework.Logf("NUMA node %d has no SRIOV devices attached", nodeNum)
			return nodeNum, true
		}
		framework.Logf("NUMA node %d has %d SRIOV devices attached", nodeNum, v)
	}
	return -1, false
}

func findNUMANodeWithoutSRIOVDevicesFromSysfs(numaNodes int) (int, bool) {
	pciDevs, err := getPCIDeviceInfo("/sys/bus/pci/devices")
	if err != nil {
		framework.Failf("error detecting the PCI device NUMA node: %v", err)
	}

	pciPerNuma := make(map[int]int)
	for _, pciDev := range pciDevs {
		if pciDev.IsVFn {
			pciPerNuma[pciDev.NUMANode]++
		}
	}

	if len(pciPerNuma) == 0 {
		framework.Logf("failed to find any VF device from %v", pciDevs)
		return -1, false
	}

	for nodeNum := 0; nodeNum < numaNodes; nodeNum++ {
		v := pciPerNuma[nodeNum]
		if v == 0 {
			framework.Logf("NUMA node %d has no SRIOV devices attached", nodeNum)
			return nodeNum, true
		}
		framework.Logf("NUMA node %d has %d SRIOV devices attached", nodeNum, v)
	}
	return -1, false
}

func findNUMANodeWithoutSRIOVDevices(configMap *v1.ConfigMap, numaNodes int) (int, bool) {
	// if someone annotated the configMap, let's use this information
	if nodeNum, found := findNUMANodeWithoutSRIOVDevicesFromConfigMap(configMap, numaNodes); found {
		return nodeNum, found
	}
	// no annotations, try to autodetect
	// NOTE: this assumes all the VFs in the box can be used for the tests.
	return findNUMANodeWithoutSRIOVDevicesFromSysfs(numaNodes)
}

func configureTopologyManagerInKubelet(oldCfg *kubeletconfig.KubeletConfiguration, policy, scope string, topologyOptions map[string]string, configMap *v1.ConfigMap, numaNodes int) (*kubeletconfig.KubeletConfiguration, string) {
	// Configure Topology Manager in Kubelet with policy.
	newCfg := oldCfg.DeepCopy()
	if newCfg.FeatureGates == nil {
		newCfg.FeatureGates = make(map[string]bool)
	}

	if topologyOptions != nil {
		newCfg.TopologyManagerPolicyOptions = topologyOptions
	}

	// Set the Topology Manager policy
	newCfg.TopologyManagerPolicy = policy

	newCfg.TopologyManagerScope = scope

	// Set the CPU Manager policy to static.
	newCfg.CPUManagerPolicy = string(cpumanager.PolicyStatic)

	// Set the CPU Manager reconcile period to 1 second.
	newCfg.CPUManagerReconcilePeriod = metav1.Duration{Duration: 1 * time.Second}

	if nodeNum, ok := findNUMANodeWithoutSRIOVDevices(configMap, numaNodes); ok {
		cpus, err := getCPUsPerNUMANode(nodeNum)
		framework.Logf("NUMA Node %d doesn't seem to have attached SRIOV devices and has cpus=%v", nodeNum, cpus)
		framework.ExpectNoError(err)
		newCfg.ReservedSystemCPUs = fmt.Sprintf("%d", cpus[len(cpus)-1])
	} else {
		// The Kubelet panics if either kube-reserved or system-reserved is not set
		// when CPU Manager is enabled. Set cpu in kube-reserved > 0 so that
		// kubelet doesn't panic.
		if newCfg.KubeReserved == nil {
			newCfg.KubeReserved = map[string]string{}
		}

		if _, ok := newCfg.KubeReserved["cpu"]; !ok {
			newCfg.KubeReserved["cpu"] = "200m"
		}
	}
	// Dump the config -- debug
	framework.Logf("New kubelet config is %s", newCfg.String())

	return newCfg, newCfg.ReservedSystemCPUs
}

// getSRIOVDevicePluginPod returns the Device Plugin pod for sriov resources in e2e tests.
func getSRIOVDevicePluginPod() *v1.Pod {
	data, err := e2etestfiles.Read(SRIOVDevicePluginDSYAML)
	if err != nil {
		framework.Fail(err.Error())
	}

	ds := readDaemonSetV1OrDie(data)
	p := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      SRIOVDevicePluginName,
			Namespace: metav1.NamespaceSystem,
		},

		Spec: ds.Spec.Template.Spec,
	}

	return p
}

func readConfigMapV1OrDie(objBytes []byte) *v1.ConfigMap {
	v1.AddToScheme(appsScheme)
	requiredObj, err := runtime.Decode(appsCodecs.UniversalDecoder(v1.SchemeGroupVersion), objBytes)
	if err != nil {
		panic(err)
	}
	return requiredObj.(*v1.ConfigMap)
}

func readServiceAccountV1OrDie(objBytes []byte) *v1.ServiceAccount {
	v1.AddToScheme(appsScheme)
	requiredObj, err := runtime.Decode(appsCodecs.UniversalDecoder(v1.SchemeGroupVersion), objBytes)
	if err != nil {
		panic(err)
	}
	return requiredObj.(*v1.ServiceAccount)
}

func findSRIOVResource(node *v1.Node) (string, int64) {
	framework.Logf("Node status allocatable: %v", node.Status.Allocatable)
	re := regexp.MustCompile(`^intel.com/.*sriov.*`)
	for key, val := range node.Status.Allocatable {
		resource := string(key)
		if re.MatchString(resource) {
			v := val.Value()
			if v > 0 {
				return resource, v
			}
		}
	}
	return "", 0
}

func validatePodAlignment(ctx context.Context, f *framework.Framework, pod *v1.Pod, envInfo *testEnvInfo) {
	for _, cnt := range pod.Spec.InitContainers {
		ginkgo.By(fmt.Sprintf("validating the init container %s on Gu pod %s", cnt.Name, pod.Name))

		logs, err := e2epod.GetPodLogs(ctx, f.ClientSet, f.Namespace.Name, pod.Name, cnt.Name)
		framework.ExpectNoError(err, "expected log not found in init container [%s] of pod [%s]", cnt.Name, pod.Name)

		framework.Logf("got init container logs: %v", logs)
		numaRes, err := checkNUMAAlignment(f, pod, &cnt, logs, envInfo)
		framework.ExpectNoError(err, "NUMA Alignment check failed for init container [%s] of pod [%s]", cnt.Name, pod.Name)
		if numaRes != nil {
			framework.Logf("NUMA resources for init container %s/%s: %s", pod.Name, cnt.Name, numaRes.String())
		}
	}

	for _, cnt := range pod.Spec.Containers {
		ginkgo.By(fmt.Sprintf("validating the container %s on Gu pod %s", cnt.Name, pod.Name))

		logs, err := e2epod.GetPodLogs(ctx, f.ClientSet, f.Namespace.Name, pod.Name, cnt.Name)
		framework.ExpectNoError(err, "expected log not found in container [%s] of pod [%s]", cnt.Name, pod.Name)

		framework.Logf("got pod logs: %v", logs)
		numaRes, err := checkNUMAAlignment(f, pod, &cnt, logs, envInfo)
		framework.ExpectNoError(err, "NUMA Alignment check failed for [%s] of pod [%s]", cnt.Name, pod.Name)
		if numaRes != nil {
			framework.Logf("NUMA resources for %s/%s: %s", pod.Name, cnt.Name, numaRes.String())
		}
	}
}

// validatePodAligmentWithPodScope validates whether all pod's CPUs are affined to the same NUMA node.
func validatePodAlignmentWithPodScope(ctx context.Context, f *framework.Framework, pod *v1.Pod, envInfo *testEnvInfo) error {
	// Mapping between CPU IDs and NUMA node IDs.
	podsNUMA := make(map[int]int)

	ginkgo.By(fmt.Sprintf("validate pod scope alignment for %s pod", pod.Name))
	for _, cnt := range pod.Spec.InitContainers {
		logs, err := e2epod.GetPodLogs(ctx, f.ClientSet, f.Namespace.Name, pod.Name, cnt.Name)
		framework.ExpectNoError(err, "NUMA alignment failed for init container [%s] of pod [%s]", cnt.Name, pod.Name)
		envMap, err := makeEnvMap(logs)
		framework.ExpectNoError(err, "NUMA alignment failed for init container [%s] of pod [%s]", cnt.Name, pod.Name)
		cpuToNUMA, err := getCPUToNUMANodeMapFromEnv(f, pod, &cnt, envMap, envInfo.numaNodes)
		framework.ExpectNoError(err, "NUMA alignment failed for init container [%s] of pod [%s]", cnt.Name, pod.Name)
		for cpuID, numaID := range cpuToNUMA {
			podsNUMA[cpuID] = numaID
		}
	}

	for _, cnt := range pod.Spec.Containers {
		logs, err := e2epod.GetPodLogs(ctx, f.ClientSet, f.Namespace.Name, pod.Name, cnt.Name)
		framework.ExpectNoError(err, "NUMA alignment failed for container [%s] of pod [%s]", cnt.Name, pod.Name)
		envMap, err := makeEnvMap(logs)
		framework.ExpectNoError(err, "NUMA alignment failed for container [%s] of pod [%s]", cnt.Name, pod.Name)
		cpuToNUMA, err := getCPUToNUMANodeMapFromEnv(f, pod, &cnt, envMap, envInfo.numaNodes)
		framework.ExpectNoError(err, "NUMA alignment failed for container [%s] of pod [%s]", cnt.Name, pod.Name)
		for cpuID, numaID := range cpuToNUMA {
			podsNUMA[cpuID] = numaID
		}
	}

	numaRes := numaPodResources{
		CPUToNUMANode: podsNUMA,
	}
	aligned := numaRes.CheckAlignment()
	if !aligned {
		return fmt.Errorf("resources were assigned from different NUMA nodes")
	}

	framework.Logf("NUMA locality confirmed: all pod's CPUs aligned to the same NUMA node")
	return nil
}

func runTopologyManagerPolicySuiteTests(ctx context.Context, f *framework.Framework) {
	var cpuCap, cpuAlloc int64

	cpuCap, cpuAlloc, _ = getLocalNodeCPUDetails(ctx, f)
	ginkgo.By(fmt.Sprintf("checking node CPU capacity (%d) and allocatable CPUs (%d)", cpuCap, cpuAlloc))

	// Albeit even the weakest CI machines usually have 2 cpus, let's be extra careful and
	// check explicitly. We prefer to skip than a false negative (and a failed test).
	if cpuAlloc < 1 {
		e2eskipper.Skipf("Skipping basic CPU Manager tests since CPU capacity < 2")
	}

	ginkgo.By("running a non-Gu pod")
	runNonGuPodTest(ctx, f, cpuCap)

	ginkgo.By("running a Gu pod")
	runGuPodTest(ctx, f, 1)

	// Skip rest of the tests if CPU allocatable < 3.
	if cpuAlloc < 3 {
		e2eskipper.Skipf("Skipping rest of the CPU Manager tests since CPU capacity < 3")
	}

	ginkgo.By("running multiple Gu and non-Gu pods")
	runMultipleGuNonGuPods(ctx, f, cpuCap, cpuAlloc)

	ginkgo.By("running a Gu pod requesting multiple CPUs")
	runMultipleCPUGuPod(ctx, f)

	ginkgo.By("running a Gu pod with multiple containers requesting integer CPUs")
	runMultipleCPUContainersGuPod(ctx, f)

	ginkgo.By("running multiple Gu pods")
	runMultipleGuPods(ctx, f)

	ginkgo.By("running a Gu pod with restartable init container requesting integer CPUs")
	runRestartableInitContainerGuPod(ctx, f)
}

func runTopologyManagerPositiveTest(ctx context.Context, f *framework.Framework, numPods int, ctnAttrs, initCtnAttrs []tmCtnAttribute, envInfo *testEnvInfo) {
	podMap := make(map[string]*v1.Pod)

	for podID := 0; podID < numPods; podID++ {
		podName := fmt.Sprintf("gu-pod-%d", podID)
		framework.Logf("creating pod %s attrs %v", podName, ctnAttrs)
		pod := makeTopologyManagerTestPod(podName, ctnAttrs, initCtnAttrs)
		pod = e2epod.NewPodClient(f).CreateSync(ctx, pod)
		framework.Logf("created pod %s", podName)
		podMap[podName] = pod
	}

	// per https://github.com/kubernetes/enhancements/blob/master/keps/sig-node/693-topology-manager/README.md#multi-numa-systems-tests
	// we can do a meaningful validation only when using the single-numa node policy
	if envInfo.policy == topologymanager.PolicySingleNumaNode {
		for _, pod := range podMap {
			validatePodAlignment(ctx, f, pod, envInfo)
		}
		if envInfo.scope == podScopeTopology {
			for _, pod := range podMap {
				err := validatePodAlignmentWithPodScope(ctx, f, pod, envInfo)
				framework.ExpectNoError(err)
			}
		}
	}

	deletePodsAsync(ctx, f, podMap)
}

func deletePodsAsync(ctx context.Context, f *framework.Framework, podMap map[string]*v1.Pod) {
	var wg sync.WaitGroup
	for _, pod := range podMap {
		wg.Add(1)
		go func(podNS, podName string) {
			defer ginkgo.GinkgoRecover()
			defer wg.Done()

			deletePodSyncByName(ctx, f, podName)
			waitForAllContainerRemoval(ctx, podName, podNS)
		}(pod.Namespace, pod.Name)
	}
	wg.Wait()
}

func runTopologyManagerNegativeTest(ctx context.Context, f *framework.Framework, ctnAttrs, initCtnAttrs []tmCtnAttribute, envInfo *testEnvInfo) {
	podName := "gu-pod"
	framework.Logf("creating pod %s attrs %v", podName, ctnAttrs)
	pod := makeTopologyManagerTestPod(podName, ctnAttrs, initCtnAttrs)

	pod = e2epod.NewPodClient(f).Create(ctx, pod)
	err := e2epod.WaitForPodCondition(ctx, f.ClientSet, f.Namespace.Name, pod.Name, "Failed", 30*time.Second, func(pod *v1.Pod) (bool, error) {
		if pod.Status.Phase != v1.PodPending {
			return true, nil
		}
		return false, nil
	})
	framework.ExpectNoError(err)
	pod, err = e2epod.NewPodClient(f).Get(ctx, pod.Name, metav1.GetOptions{})
	framework.ExpectNoError(err)

	if pod.Status.Phase != v1.PodFailed {
		framework.Failf("pod %s not failed: %v", pod.Name, pod.Status)
	}
	if !isTopologyAffinityError(pod) {
		framework.Failf("pod %s failed for wrong reason: %q", pod.Name, pod.Status.Reason)
	}

	deletePodSyncByName(ctx, f, pod.Name)
}

func isTopologyAffinityError(pod *v1.Pod) bool {
	re := regexp.MustCompile(`Topology.*Affinity.*Error`)
	return re.MatchString(pod.Status.Reason)
}

func getSRIOVDevicePluginConfigMap(cmFile string) *v1.ConfigMap {
	data, err := e2etestfiles.Read(SRIOVDevicePluginCMYAML)
	if err != nil {
		framework.Fail(err.Error())
	}

	// the SRIOVDP configuration is hw-dependent, so we allow per-test-host customization.
	framework.Logf("host-local SRIOV Device Plugin Config Map %q", cmFile)
	if cmFile != "" {
		data, err = os.ReadFile(cmFile)
		if err != nil {
			framework.Failf("unable to load the SRIOV Device Plugin ConfigMap: %v", err)
		}
	} else {
		framework.Logf("Using built-in SRIOV Device Plugin Config Map")
	}

	return readConfigMapV1OrDie(data)
}

type sriovData struct {
	configMap      *v1.ConfigMap
	serviceAccount *v1.ServiceAccount
	pod            *v1.Pod

	resourceName   string
	resourceAmount int64
}

func setupSRIOVConfigOrFail(ctx context.Context, f *framework.Framework, configMap *v1.ConfigMap) *sriovData {
	sd := createSRIOVConfigOrFail(ctx, f, configMap)

	e2enode.WaitForNodeToBeReady(ctx, f.ClientSet, framework.TestContext.NodeName, 5*time.Minute)

	sd.pod = createSRIOVPodOrFail(ctx, f)
	return sd
}

func createSRIOVConfigOrFail(ctx context.Context, f *framework.Framework, configMap *v1.ConfigMap) *sriovData {
	var err error

	ginkgo.By(fmt.Sprintf("Creating configMap %v/%v", metav1.NamespaceSystem, configMap.Name))
	if _, err = f.ClientSet.CoreV1().ConfigMaps(metav1.NamespaceSystem).Create(ctx, configMap, metav1.CreateOptions{}); err != nil {
		framework.Failf("unable to create test configMap %s: %v", configMap.Name, err)
	}

	data, err := e2etestfiles.Read(SRIOVDevicePluginSAYAML)
	if err != nil {
		framework.Fail(err.Error())
	}
	serviceAccount := readServiceAccountV1OrDie(data)
	ginkgo.By(fmt.Sprintf("Creating serviceAccount %v/%v", metav1.NamespaceSystem, serviceAccount.Name))
	if _, err = f.ClientSet.CoreV1().ServiceAccounts(metav1.NamespaceSystem).Create(ctx, serviceAccount, metav1.CreateOptions{}); err != nil {
		framework.Failf("unable to create test serviceAccount %s: %v", serviceAccount.Name, err)
	}

	return &sriovData{
		configMap:      configMap,
		serviceAccount: serviceAccount,
	}
}

func createSRIOVPodOrFail(ctx context.Context, f *framework.Framework) *v1.Pod {
	dp := getSRIOVDevicePluginPod()
	dp.Spec.NodeName = framework.TestContext.NodeName

	ginkgo.By("Create SRIOV device plugin pod")
	dpPod, err := f.ClientSet.CoreV1().Pods(metav1.NamespaceSystem).Create(ctx, dp, metav1.CreateOptions{})
	framework.ExpectNoError(err)

	if err = e2epod.WaitForPodCondition(ctx, f.ClientSet, metav1.NamespaceSystem, dp.Name, "Ready", 120*time.Second, testutils.PodRunningReady); err != nil {
		framework.Logf("SRIOV Pod %v took too long to enter running/ready: %v", dp.Name, err)
	}
	framework.ExpectNoError(err)

	return dpPod
}

// waitForSRIOVResources waits until enough SRIOV resources are available, expecting to complete within the timeout.
// if exits successfully, updates the sriovData with the resources which were found.
func waitForSRIOVResources(ctx context.Context, f *framework.Framework, sd *sriovData) {
	sriovResourceName := ""
	var sriovResourceAmount int64
	ginkgo.By("Waiting for devices to become available on the local node")
	gomega.Eventually(ctx, func(ctx context.Context) bool {
		node := getLocalNode(ctx, f)
		sriovResourceName, sriovResourceAmount = findSRIOVResource(node)
		return sriovResourceAmount > minSriovResource
	}, 2*time.Minute, framework.Poll).Should(gomega.BeTrueBecause("expected SRIOV resources to be available within the timout"))

	sd.resourceName = sriovResourceName
	sd.resourceAmount = sriovResourceAmount
	framework.Logf("Detected SRIOV allocatable devices name=%q amount=%d", sd.resourceName, sd.resourceAmount)
}

func deleteSRIOVPodOrFail(ctx context.Context, f *framework.Framework, sd *sriovData) {
	var err error
	gp := int64(0)
	deleteOptions := metav1.DeleteOptions{
		GracePeriodSeconds: &gp,
	}

	ginkgo.By(fmt.Sprintf("Delete SRIOV device plugin pod %s/%s", sd.pod.Namespace, sd.pod.Name))
	err = f.ClientSet.CoreV1().Pods(sd.pod.Namespace).Delete(ctx, sd.pod.Name, deleteOptions)
	framework.ExpectNoError(err)
	waitForAllContainerRemoval(ctx, sd.pod.Name, sd.pod.Namespace)
}

func removeSRIOVConfigOrFail(ctx context.Context, f *framework.Framework, sd *sriovData) {
	var err error
	gp := int64(0)
	deleteOptions := metav1.DeleteOptions{
		GracePeriodSeconds: &gp,
	}

	ginkgo.By(fmt.Sprintf("Deleting configMap %v/%v", metav1.NamespaceSystem, sd.configMap.Name))
	err = f.ClientSet.CoreV1().ConfigMaps(metav1.NamespaceSystem).Delete(ctx, sd.configMap.Name, deleteOptions)
	framework.ExpectNoError(err)

	ginkgo.By(fmt.Sprintf("Deleting serviceAccount %v/%v", metav1.NamespaceSystem, sd.serviceAccount.Name))
	err = f.ClientSet.CoreV1().ServiceAccounts(metav1.NamespaceSystem).Delete(ctx, sd.serviceAccount.Name, deleteOptions)
	framework.ExpectNoError(err)
}

func teardownSRIOVConfigOrFail(ctx context.Context, f *framework.Framework, sd *sriovData) {
	deleteSRIOVPodOrFail(ctx, f, sd)
	removeSRIOVConfigOrFail(ctx, f, sd)
}

func runTMScopeResourceAlignmentTestSuite(ctx context.Context, f *framework.Framework, configMap *v1.ConfigMap, reservedSystemCPUs, policy string, numaNodes, coreCount int) {
	threadsPerCore := getSMTLevel()
	sd := setupSRIOVConfigOrFail(ctx, f, configMap)
	var ctnAttrs, initCtnAttrs []tmCtnAttribute

	waitForSRIOVResources(ctx, f, sd)

	envInfo := &testEnvInfo{
		numaNodes:         numaNodes,
		sriovResourceName: sd.resourceName,
		policy:            policy,
		scope:             podScopeTopology,
	}

	ginkgo.By(fmt.Sprintf("Admit two guaranteed pods. Both consist of 2 containers, each container with 1 CPU core. Use 1 %s device.", sd.resourceName))
	ctnAttrs = []tmCtnAttribute{
		{
			ctnName:       "ps-container-0",
			cpuRequest:    "1000m",
			cpuLimit:      "1000m",
			deviceName:    sd.resourceName,
			deviceRequest: "1",
			deviceLimit:   "1",
		},
		{
			ctnName:       "ps-container-1",
			cpuRequest:    "1000m",
			cpuLimit:      "1000m",
			deviceName:    sd.resourceName,
			deviceRequest: "1",
			deviceLimit:   "1",
		},
	}
	runTopologyManagerPositiveTest(ctx, f, 2, ctnAttrs, initCtnAttrs, envInfo)

	numCores := threadsPerCore * coreCount
	coresReq := fmt.Sprintf("%dm", numCores*1000)
	ginkgo.By(fmt.Sprintf("Admit a guaranteed pod requesting %d CPU cores, i.e., more than can be provided at every single NUMA node. Therefore, the request should be rejected.", numCores+1))
	ctnAttrs = []tmCtnAttribute{
		{
			ctnName:       "gu-container-1",
			cpuRequest:    coresReq,
			cpuLimit:      coresReq,
			deviceRequest: "1",
			deviceLimit:   "1",
		},
		{
			ctnName:       "gu-container-2",
			cpuRequest:    "1000m",
			cpuLimit:      "1000m",
			deviceRequest: "1",
			deviceLimit:   "1",
		},
	}
	runTopologyManagerNegativeTest(ctx, f, ctnAttrs, initCtnAttrs, envInfo)

	// The Topology Manager with pod scope should calculate how many CPUs it needs to admit a pod basing on two requests:
	// the maximum of init containers' demand for CPU and sum of app containers' requests for CPU.
	// The Topology Manager should use higher value of these. Therefore, both pods from below test case should get number of CPUs
	// requested by init-container of highest demand for it. Since demand for CPU of each pod is slightly higher than half of resources
	// available on one node, both pods should be placed on distinct NUMA nodes.
	coresReq = fmt.Sprintf("%dm", (numCores/2+1)*1000)
	ginkgo.By(fmt.Sprintf("Admit two guaranteed pods, each pod requests %d cores - the pods should be placed on different NUMA nodes", numCores/2+1))
	initCtnAttrs = []tmCtnAttribute{
		{
			ctnName:       "init-container-1",
			cpuRequest:    coresReq,
			cpuLimit:      coresReq,
			deviceRequest: "1",
			deviceLimit:   "1",
		},
		{
			ctnName:       "init-container-2",
			cpuRequest:    "1000m",
			cpuLimit:      "1000m",
			deviceRequest: "1",
			deviceLimit:   "1",
		},
	}
	ctnAttrs = []tmCtnAttribute{
		{
			ctnName:       "gu-container-0",
			cpuRequest:    "1000m",
			cpuLimit:      "1000m",
			deviceRequest: "1",
			deviceLimit:   "1",
		},
		{
			ctnName:       "gu-container-1",
			cpuRequest:    "1000m",
			cpuLimit:      "1000m",
			deviceRequest: "1",
			deviceLimit:   "1",
		},
	}
	runTopologyManagerPositiveTest(ctx, f, 2, ctnAttrs, initCtnAttrs, envInfo)

	ginkgo.By(fmt.Sprintf("Successfully admit one guaranteed pod with restartable init container, 1 core and 1 %s device", sd.resourceName))
	initCtnAttrs = []tmCtnAttribute{
		{
			ctnName:       "restartable-init-container",
			cpuRequest:    "1000m",
			cpuLimit:      "1000m",
			deviceRequest: "1",
			deviceLimit:   "1",
			restartPolicy: &containerRestartPolicyAlways,
		},
	}
	ctnAttrs = []tmCtnAttribute{
		{
			ctnName:       "gu-container",
			cpuRequest:    "1000m",
			cpuLimit:      "1000m",
			deviceRequest: "1",
			deviceLimit:   "1",
		},
	}
	runTopologyManagerPositiveTest(ctx, f, 2, ctnAttrs, initCtnAttrs, envInfo)

	teardownSRIOVConfigOrFail(ctx, f, sd)
}

func runTopologyManagerNodeAlignmentSuiteTests(ctx context.Context, f *framework.Framework, sd *sriovData, reservedSystemCPUs, policy string, numaNodes, coreCount int) {
	threadsPerCore := getSMTLevel()

	waitForSRIOVResources(ctx, f, sd)

	envInfo := &testEnvInfo{
		numaNodes:         numaNodes,
		sriovResourceName: sd.resourceName,
		policy:            policy,
	}

	// could have been a loop, we unroll it to explain the testcases
	var ctnAttrs, initCtnAttrs []tmCtnAttribute

	// simplest case
	ginkgo.By(fmt.Sprintf("Successfully admit one guaranteed pod with 1 core, 1 %s device", sd.resourceName))
	ctnAttrs = []tmCtnAttribute{
		{
			ctnName:       "gu-container",
			cpuRequest:    "1000m",
			cpuLimit:      "1000m",
			deviceName:    sd.resourceName,
			deviceRequest: "1",
			deviceLimit:   "1",
		},
	}
	runTopologyManagerPositiveTest(ctx, f, 1, ctnAttrs, initCtnAttrs, envInfo)

	ginkgo.By(fmt.Sprintf("Successfully admit one guaranteed pod with 2 cores, 1 %s device", sd.resourceName))
	ctnAttrs = []tmCtnAttribute{
		{
			ctnName:       "gu-container",
			cpuRequest:    "2000m",
			cpuLimit:      "2000m",
			deviceName:    sd.resourceName,
			deviceRequest: "1",
			deviceLimit:   "1",
		},
	}
	runTopologyManagerPositiveTest(ctx, f, 1, ctnAttrs, initCtnAttrs, envInfo)

	if reservedSystemCPUs != "" {
		// to avoid false negatives, we have put reserved CPUs in such a way there is at least a NUMA node
		// with 1+ SRIOV devices and not reserved CPUs.
		numCores := threadsPerCore * coreCount
		allCoresReq := fmt.Sprintf("%dm", numCores*1000)
		ginkgo.By(fmt.Sprintf("Successfully admit an entire socket (%d cores), 1 %s device", numCores, sd.resourceName))
		ctnAttrs = []tmCtnAttribute{
			{
				ctnName:       "gu-container",
				cpuRequest:    allCoresReq,
				cpuLimit:      allCoresReq,
				deviceName:    sd.resourceName,
				deviceRequest: "1",
				deviceLimit:   "1",
			},
		}
		runTopologyManagerPositiveTest(ctx, f, 1, ctnAttrs, initCtnAttrs, envInfo)
	}

	if sd.resourceAmount > 1 {
		// no matter how busses are connected to NUMA nodes and SRIOV devices are installed, this function
		// preconditions must ensure the following can be fulfilled
		ginkgo.By(fmt.Sprintf("Successfully admit two guaranteed pods, each with 1 core, 1 %s device", sd.resourceName))
		ctnAttrs = []tmCtnAttribute{
			{
				ctnName:       "gu-container",
				cpuRequest:    "1000m",
				cpuLimit:      "1000m",
				deviceName:    sd.resourceName,
				deviceRequest: "1",
				deviceLimit:   "1",
			},
		}
		runTopologyManagerPositiveTest(ctx, f, 2, ctnAttrs, initCtnAttrs, envInfo)

		ginkgo.By(fmt.Sprintf("Successfully admit two guaranteed pods, each with 2 cores, 1 %s device", sd.resourceName))
		ctnAttrs = []tmCtnAttribute{
			{
				ctnName:       "gu-container",
				cpuRequest:    "2000m",
				cpuLimit:      "2000m",
				deviceName:    sd.resourceName,
				deviceRequest: "1",
				deviceLimit:   "1",
			},
		}
		runTopologyManagerPositiveTest(ctx, f, 2, ctnAttrs, initCtnAttrs, envInfo)

		ginkgo.By(fmt.Sprintf("Successfully admit one guaranteed pod with restartable init container - each with 1 core, 1 %s device", sd.resourceName))
		initCtnAttrs = []tmCtnAttribute{
			{
				ctnName:       "init-container",
				cpuRequest:    "1000m",
				cpuLimit:      "1000m",
				deviceName:    sd.resourceName,
				deviceRequest: "1",
				deviceLimit:   "1",
				restartPolicy: &containerRestartPolicyAlways,
			},
		}
		ctnAttrs = []tmCtnAttribute{
			{
				ctnName:       "gu-container",
				cpuRequest:    "1000m",
				cpuLimit:      "1000m",
				deviceName:    sd.resourceName,
				deviceRequest: "1",
				deviceLimit:   "1",
			},
		}
		runTopologyManagerPositiveTest(ctx, f, 1, ctnAttrs, initCtnAttrs, envInfo)

		// testing more complex conditions require knowledge about the system cpu+bus topology
	}

	// multi-container tests
	if sd.resourceAmount >= 4 {
		ginkgo.By(fmt.Sprintf("Successfully admit a guaranteed pod requesting for two containers, each with 2 cores, 1 %s device", sd.resourceName))
		ctnAttrs = []tmCtnAttribute{
			{
				ctnName:       "gu-container-0",
				cpuRequest:    "2000m",
				cpuLimit:      "2000m",
				deviceName:    sd.resourceName,
				deviceRequest: "1",
				deviceLimit:   "1",
			},
			{
				ctnName:       "gu-container-1",
				cpuRequest:    "2000m",
				cpuLimit:      "2000m",
				deviceName:    sd.resourceName,
				deviceRequest: "1",
				deviceLimit:   "1",
			},
		}
		runTopologyManagerPositiveTest(ctx, f, 1, ctnAttrs, initCtnAttrs, envInfo)

		ginkgo.By(fmt.Sprintf("Successfully admit two guaranteed pods, each with two containers, each with 1 core, 1 %s device", sd.resourceName))
		ctnAttrs = []tmCtnAttribute{
			{
				ctnName:       "gu-container-0",
				cpuRequest:    "1000m",
				cpuLimit:      "1000m",
				deviceName:    sd.resourceName,
				deviceRequest: "1",
				deviceLimit:   "1",
			},
			{
				ctnName:       "gu-container-1",
				cpuRequest:    "1000m",
				cpuLimit:      "1000m",
				deviceName:    sd.resourceName,
				deviceRequest: "1",
				deviceLimit:   "1",
			},
		}
		runTopologyManagerPositiveTest(ctx, f, 2, ctnAttrs, initCtnAttrs, envInfo)

		ginkgo.By(fmt.Sprintf("Successfully admit two guaranteed pods, each with two containers, both with with 2 cores, one with 1 %s device", sd.resourceName))
		ctnAttrs = []tmCtnAttribute{
			{
				ctnName:       "gu-container-dev",
				cpuRequest:    "2000m",
				cpuLimit:      "2000m",
				deviceName:    sd.resourceName,
				deviceRequest: "1",
				deviceLimit:   "1",
			},
			{
				ctnName:    "gu-container-nodev",
				cpuRequest: "2000m",
				cpuLimit:   "2000m",
			},
		}
		runTopologyManagerPositiveTest(ctx, f, 2, ctnAttrs, initCtnAttrs, envInfo)
	}

	// this is the only policy that can guarantee reliable rejects
	if policy == topologymanager.PolicySingleNumaNode {
		// overflow NUMA node capacity: cores
		numCores := 1 + (threadsPerCore * coreCount)
		excessCoresReq := fmt.Sprintf("%dm", numCores*1000)
		ginkgo.By(fmt.Sprintf("Trying to admit a guaranteed pods, with %d cores, 1 %s device - and it should be rejected", numCores, sd.resourceName))
		ctnAttrs = []tmCtnAttribute{
			{
				ctnName:       "gu-container",
				cpuRequest:    excessCoresReq,
				cpuLimit:      excessCoresReq,
				deviceName:    sd.resourceName,
				deviceRequest: "1",
				deviceLimit:   "1",
			},
		}
		runTopologyManagerNegativeTest(ctx, f, ctnAttrs, initCtnAttrs, envInfo)
	}
}

func runPreferClosestNUMATestSuite(ctx context.Context, f *framework.Framework, numaNodes int, distances map[int][]int) {
	runPreferClosestNUMAOptimalAllocationTest(ctx, f, numaNodes, distances)
	runPreferClosestNUMASubOptimalAllocationTest(ctx, f, numaNodes, distances)
}

func runPreferClosestNUMAOptimalAllocationTest(ctx context.Context, f *framework.Framework, numaNodes int, distances map[int][]int) {
	ginkgo.By("Admit two guaranteed pods. Both consist of 1 containers, each pod asks for cpus from 2 NUMA nodes. CPUs should be assigned from closest NUMA")
	podMap := make(map[string]*v1.Pod)
	for podID := 0; podID < 2; podID++ {
		numCores := 0
		for nodeNum := 0 + 2*podID; nodeNum <= 1+2*podID; nodeNum++ {
			cpus, err := getCPUsPerNUMANode(nodeNum)
			framework.ExpectNoError(err)
			// subtract one to accommodate reservedCPUs. It'll only work if more than 2 cpus per NUMA node.
			cpusPerNUMA := len(cpus)
			if cpusPerNUMA < 3 {
				e2eskipper.Skipf("Less than 3 cpus per NUMA node on this system. Skipping test.")
			}
			numCores += cpusPerNUMA - 1
		}
		coresReq := fmt.Sprintf("%dm", numCores*1000)
		ctnAttrs := []tmCtnAttribute{
			{
				ctnName:    "ps-container-0",
				cpuRequest: coresReq,
				cpuLimit:   coresReq,
			},
		}
		podName := fmt.Sprintf("gu-pod-%d", podID)
		framework.Logf("creating pod %s attrs %v", podName, nil)
		pod := makeTopologyManagerTestPod(podName, ctnAttrs, nil)
		pod = e2epod.NewPodClient(f).CreateSync(ctx, pod)
		framework.Logf("created pod %s", podName)
		podMap[podName] = pod
	}

	valiidatePreferClosestNUMAOptimalAllocation(ctx, f, podMap, numaNodes, distances)

	deletePodsAsync(ctx, f, podMap)
}

func runPreferClosestNUMASubOptimalAllocationTest(ctx context.Context, f *framework.Framework, numaNodes int, distances map[int][]int) {
	ginkgo.By("Admit two guaranteed pods. Both consist of 1 containers, each pod asks for cpus from 2 NUMA nodes. CPUs should be assigned from closest NUMA")
	cntName := "ps-container-0"

	// expect same amount of cpus per NUMA
	cpusPerNUMA, err := getCPUsPerNUMANode(0)
	framework.ExpectNoError(err)
	if len(cpusPerNUMA) < 5 {
		e2eskipper.Skipf("Less than 5 cpus per NUMA node on this system. Skipping test.")
	}
	podMap := make(map[string]*v1.Pod)
	for podID := 0; podID < 2; podID++ {
		// asks for all but one cpus from one less than half NUMA nodes, and half from the other
		// plus add one less than half NUMA nodes, to accommodate for reserved cpus
		numCores := ((numaNodes/2)-1)*(len(cpusPerNUMA)-1) + (len(cpusPerNUMA) / 2) + (numaNodes/2 - 1)
		framework.ExpectNoError(err)

		coresReq := fmt.Sprintf("%dm", numCores*1000)
		ctnAttrs := []tmCtnAttribute{
			{
				ctnName:    "ps-container-0",
				cpuRequest: coresReq,
				cpuLimit:   coresReq,
			},
		}
		podName := fmt.Sprintf("gu-pod-%d", podID)
		framework.Logf("creating pod %s", podName)
		pod := makeTopologyManagerTestPod(podName, ctnAttrs, nil)
		pod = e2epod.NewPodClient(f).CreateSync(ctx, pod)
		framework.Logf("created pod %s", podName)
		podMap[podName] = pod
	}

	valiidatePreferClosestNUMAOptimalAllocation(ctx, f, podMap, numaNodes, distances)

	ginkgo.By("Admit one guaranteed pod. Asks for cpus from 2 NUMA nodes. CPUs should be assigned from non closest NUMA")
	// ask for remaining cpus, it should only fit on sub-optimal NUMA placement.
	coresReq := fmt.Sprintf("%dm", 2*(len(cpusPerNUMA)/2)*1000)
	ctnAttrs := []tmCtnAttribute{
		{
			ctnName:    cntName,
			cpuRequest: coresReq,
			cpuLimit:   coresReq,
		},
	}
	podName := "gu-pod-2"
	framework.Logf("creating pod %s attrs %v", podName, nil)
	pod := makeTopologyManagerTestPod(podName, ctnAttrs, nil)
	pod = e2epod.NewPodClient(f).CreateSync(ctx, pod)
	framework.Logf("created pod %s", podName)

	ginkgo.By(fmt.Sprintf("validating the container %s on Gu pod %s", cntName, pod.Name))

	logs, err := e2epod.GetPodLogs(ctx, f.ClientSet, f.Namespace.Name, pod.Name, cntName)
	framework.ExpectNoError(err, "expected log not found in container [%s] of pod [%s]", cntName, pod.Name)

	framework.Logf("got pod logs: %v", logs)
	podEnv, err := makeEnvMap(logs)
	framework.ExpectNoError(err, "expected log not found in container [%s] of pod [%s]", cntName, pod.Name)

	CPUToNUMANode, err := getCPUToNUMANodeMapFromEnv(f, pod, &pod.Spec.Containers[0], podEnv, numaNodes)
	framework.ExpectNoError(err, "expected log not found in container [%s] of pod [%s]", cntName, pod.Name)

	numaUsed := sets.New[int]()
	for _, numa := range CPUToNUMANode {
		numaUsed.Insert(numa)
	}

	numaList := numaUsed.UnsortedList()
	gomega.Expect(numaList).To(gomega.HaveLen(2))

	distance := getMinRemoteDistanceForNode(distances)
	gomega.Expect(distance).NotTo(gomega.Equal(distances[numaList[0]][numaList[1]]))

	deletePodsAsync(ctx, f, podMap)
}

func valiidatePreferClosestNUMAOptimalAllocation(ctx context.Context, f *framework.Framework, podMap map[string]*v1.Pod, numaNodes int, distances map[int][]int) {
	for _, pod := range podMap {
		for _, cnt := range pod.Spec.Containers {
			ginkgo.By(fmt.Sprintf("validating the container %s on Gu pod %s", cnt.Name, pod.Name))

			logs, err := e2epod.GetPodLogs(ctx, f.ClientSet, f.Namespace.Name, pod.Name, cnt.Name)
			framework.ExpectNoError(err, "expected log not found in container [%s] of pod [%s]", cnt.Name, pod.Name)

			framework.Logf("got pod logs: %v", logs)
			podEnv, err := makeEnvMap(logs)
			framework.ExpectNoError(err, "expected log not found in container [%s] of pod [%s]", cnt.Name, pod.Name)

			CPUToNUMANode, err := getCPUToNUMANodeMapFromEnv(f, pod, &cnt, podEnv, numaNodes)
			framework.ExpectNoError(err, "expected log not found in container [%s] of pod [%s]", cnt.Name, pod.Name)

			numaUsed := sets.New[int]()
			for _, numa := range CPUToNUMANode {
				numaUsed.Insert(numa)
			}

			numaList := numaUsed.UnsortedList()
			gomega.Expect(numaList).To(gomega.HaveLen(2))

			distance := getMinRemoteDistanceForNode(distances)
			gomega.Expect(distance).To(gomega.Equal(distances[numaList[0]][numaList[1]]))
		}
	}
}

func runTopologyManagerTests(f *framework.Framework, topologyOptions map[string]string) {
	var oldCfg *kubeletconfig.KubeletConfiguration
	var err error

	var policies = []string{
		topologymanager.PolicySingleNumaNode,
		topologymanager.PolicyRestricted,
		topologymanager.PolicyBestEffort,
		topologymanager.PolicyNone,
	}

	ginkgo.It("run Topology Manager policy test suite", func(ctx context.Context) {
		oldCfg, err = getCurrentKubeletConfig(ctx)
		framework.ExpectNoError(err)

		scope := containerScopeTopology
		for _, policy := range policies {
			// Configure Topology Manager
			ginkgo.By(fmt.Sprintf("by configuring Topology Manager policy to %s", policy))
			framework.Logf("Configuring topology Manager policy to %s", policy)

			newCfg, _ := configureTopologyManagerInKubelet(oldCfg, policy, scope, topologyOptions, nil, 0)
			updateKubeletConfig(ctx, f, newCfg, true)
			// Run the tests
			runTopologyManagerPolicySuiteTests(ctx, f)
		}
	})

	ginkgo.It("run Topology Manager node alignment test suite", func(ctx context.Context) {
		numaNodes, coreCount := hostPrecheck()

		configMap := getSRIOVDevicePluginConfigMap(framework.TestContext.SriovdpConfigMapFile)

		oldCfg, err = getCurrentKubeletConfig(ctx)
		framework.ExpectNoError(err)

		sd := setupSRIOVConfigOrFail(ctx, f, configMap)
		ginkgo.DeferCleanup(teardownSRIOVConfigOrFail, f, sd)

		scope := containerScopeTopology
		for _, policy := range policies {
			// Configure Topology Manager
			ginkgo.By(fmt.Sprintf("by configuring Topology Manager policy to %s", policy))
			framework.Logf("Configuring topology Manager policy to %s", policy)

			newCfg, reservedSystemCPUs := configureTopologyManagerInKubelet(oldCfg, policy, scope, topologyOptions, configMap, numaNodes)
			updateKubeletConfig(ctx, f, newCfg, true)

			runTopologyManagerNodeAlignmentSuiteTests(ctx, f, sd, reservedSystemCPUs, policy, numaNodes, coreCount)
		}
	})

	ginkgo.It("run the Topology Manager pod scope alignment test suite", func(ctx context.Context) {
		numaNodes, coreCount := hostPrecheck()

		configMap := getSRIOVDevicePluginConfigMap(framework.TestContext.SriovdpConfigMapFile)

		oldCfg, err = getCurrentKubeletConfig(ctx)
		framework.ExpectNoError(err)

		policy := topologymanager.PolicySingleNumaNode
		scope := podScopeTopology

		newCfg, reservedSystemCPUs := configureTopologyManagerInKubelet(oldCfg, policy, scope, topologyOptions, configMap, numaNodes)
		updateKubeletConfig(ctx, f, newCfg, true)

		runTMScopeResourceAlignmentTestSuite(ctx, f, configMap, reservedSystemCPUs, policy, numaNodes, coreCount)
	})

	ginkgo.AfterEach(func(ctx context.Context) {
		if oldCfg != nil {
			// restore kubelet config
			updateKubeletConfig(ctx, f, oldCfg, true)
		}
	})
}

func runPreferClosestNUMATests(f *framework.Framework) {
	var oldCfg *kubeletconfig.KubeletConfiguration
	var err error

	ginkgo.It("run the Topology Manager prefer-closest-numa policy option test suite", func(ctx context.Context) {
		numaNodes := detectNUMANodes()
		if numaNodes < minNumaNodesPreferClosestNUMA {
			e2eskipper.Skipf("this test is intended to be run on at least 4 NUMA node system")
		}

		numaDistances := detectNUMADistances(numaNodes)

		oldCfg, err = getCurrentKubeletConfig(ctx)
		framework.ExpectNoError(err)

		policy := topologymanager.PolicyBestEffort
		scope := containerScopeTopology
		options := map[string]string{topologymanager.PreferClosestNUMANodes: "true"}

		newCfg, _ := configureTopologyManagerInKubelet(oldCfg, policy, scope, options, &v1.ConfigMap{}, numaNodes)
		updateKubeletConfig(ctx, f, newCfg, true)

		runPreferClosestNUMATestSuite(ctx, f, numaNodes, numaDistances)
	})

	ginkgo.AfterEach(func(ctx context.Context) {
		if oldCfg != nil {
			// restore kubelet config
			updateKubeletConfig(ctx, f, oldCfg, true)
		}
	})
}

func hostPrecheck() (int, int) {
	// this is a very rough check. We just want to rule out system that does NOT have
	// any SRIOV device. A more proper check will be done in runTopologyManagerPositiveTest

	numaNodes := detectNUMANodes()
	if numaNodes < minNumaNodes {
		e2eskipper.Skipf("this test is intended to be run on a multi-node NUMA system")
	}

	coreCount := detectCoresPerSocket()
	if coreCount < minCoreCount {
		e2eskipper.Skipf("this test is intended to be run on a system with at least %d cores per socket", minCoreCount)
	}

	requireSRIOVDevices()

	return numaNodes, coreCount
}

// Serial because the test updates kubelet configuration.
var _ = SIGDescribe("Topology Manager", framework.WithSerial(), feature.TopologyManager, func() {
	f := framework.NewDefaultFramework("topology-manager-test")
	f.NamespacePodSecurityLevel = admissionapi.LevelPrivileged

	ginkgo.Context("With kubeconfig updated to static CPU Manager policy run the Topology Manager tests", func() {
		runTopologyManagerTests(f, nil)
	})
	ginkgo.Context("With kubeconfig's topologyOptions updated to static CPU Manager policy run the Topology Manager tests", ginkgo.Label("MaxAllowableNUMANodes"), func() {
		// At present, the default value of defaultMaxAllowableNUMANode is 8, we run
		// the same tests with  2 * defaultMaxAllowableNUMANode(16). This is the
		// most basic verification that the changed setting is not breaking stuff.
		doubleDefaultMaxAllowableNUMANodes := strconv.Itoa(8 * 2)
		runTopologyManagerTests(f, map[string]string{topologymanager.MaxAllowableNUMANodes: doubleDefaultMaxAllowableNUMANodes})
	})
	ginkgo.Context("With kubeconfig's prefer-closes-numa-nodes topologyOptions enabled run the Topology Manager tests", ginkgo.Label("PreferClosestNUMANodes"), func() {
		runPreferClosestNUMATests(f)
	})
})
