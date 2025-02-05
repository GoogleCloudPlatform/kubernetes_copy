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

package e2enode

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"k8s.io/kubernetes/pkg/util/procfs"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"

	"go.opentelemetry.io/otel/trace/noop"

	v1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/component-base/featuregate"
	internalapi "k8s.io/cri-api/pkg/apis"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
	remote "k8s.io/cri-client/pkg"
	"k8s.io/klog/v2"
	kubeletpodresourcesv1 "k8s.io/kubelet/pkg/apis/podresources/v1"
	kubeletpodresourcesv1alpha1 "k8s.io/kubelet/pkg/apis/podresources/v1alpha1"
	stats "k8s.io/kubelet/pkg/apis/stats/v1alpha1"
	"k8s.io/kubelet/pkg/types"
	"k8s.io/kubernetes/pkg/cluster/ports"
	kubeletconfig "k8s.io/kubernetes/pkg/kubelet/apis/config"
	"k8s.io/kubernetes/pkg/kubelet/apis/podresources"
	"k8s.io/kubernetes/pkg/kubelet/cm"
	kubeletmetrics "k8s.io/kubernetes/pkg/kubelet/metrics"
	"k8s.io/kubernetes/pkg/kubelet/util"

	"k8s.io/kubernetes/test/e2e/framework"
	e2emetrics "k8s.io/kubernetes/test/e2e/framework/metrics"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	e2enodekubelet "k8s.io/kubernetes/test/e2e_node/kubeletconfig"
	imageutils "k8s.io/kubernetes/test/utils/image"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
)

var startServices = flag.Bool("start-services", true, "If true, start local node services")
var stopServices = flag.Bool("stop-services", true, "If true, stop local node services after running tests")
var busyboxImage = imageutils.GetE2EImage(imageutils.BusyBox)
var agnhostImage = imageutils.GetE2EImage(imageutils.Agnhost)

const (
	// Kubelet internal cgroup name for node allocatable cgroup.
	defaultNodeAllocatableCgroup = "kubepods"
	// defaultPodResourcesPath is the path to the local endpoint serving the podresources GRPC service.
	defaultPodResourcesPath    = "/var/lib/kubelet/pod-resources"
	defaultPodResourcesTimeout = 10 * time.Second
	defaultPodResourcesMaxSize = 1024 * 1024 * 16 // 16 Mb
	// state files
	cpuManagerStateFile    = "/var/lib/kubelet/cpu_manager_state"
	memoryManagerStateFile = "/var/lib/kubelet/memory_manager_state"
)

var (
	kubeletHealthCheckURL    = fmt.Sprintf("http://127.0.0.1:%d/healthz", ports.KubeletHealthzPort)
	containerRuntimeUnitName = ""
	// KubeletConfig is the kubelet configuration the test is running against.
	kubeletCfg *kubeletconfig.KubeletConfiguration
)

func getNodeSummary(ctx context.Context) (*stats.Summary, error) {
	kubeletConfig, err := getCurrentKubeletConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get current kubelet config")
	}
	req, err := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("http://%s/stats/summary", net.JoinHostPort(kubeletConfig.Address, strconv.Itoa(int(kubeletConfig.ReadOnlyPort)))), nil)
	if err != nil {
		return nil, fmt.Errorf("failed to build http request: %w", err)
	}
	req.Header.Add("Accept", "application/json")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to get /stats/summary: %w", err)
	}

	defer resp.Body.Close()
	contentsBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read /stats/summary: %+v", resp)
	}

	decoder := json.NewDecoder(strings.NewReader(string(contentsBytes)))
	summary := stats.Summary{}
	err = decoder.Decode(&summary)
	if err != nil {
		return nil, fmt.Errorf("failed to parse /stats/summary to go struct: %+v", resp)
	}
	return &summary, nil
}

func getV1alpha1NodeDevices(ctx context.Context) (*kubeletpodresourcesv1alpha1.ListPodResourcesResponse, error) {
	endpoint, err := util.LocalEndpoint(defaultPodResourcesPath, podresources.Socket)
	if err != nil {
		return nil, fmt.Errorf("Error getting local endpoint: %w", err)
	}
	client, conn, err := podresources.GetV1alpha1Client(endpoint, defaultPodResourcesTimeout, defaultPodResourcesMaxSize)
	if err != nil {
		return nil, fmt.Errorf("Error getting grpc client: %w", err)
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	resp, err := client.List(ctx, &kubeletpodresourcesv1alpha1.ListPodResourcesRequest{})
	if err != nil {
		return nil, fmt.Errorf("%v.Get(_) = _, %v", client, err)
	}
	return resp, nil
}

func getV1NodeDevices(ctx context.Context) (*kubeletpodresourcesv1.ListPodResourcesResponse, error) {
	endpoint, err := util.LocalEndpoint(defaultPodResourcesPath, podresources.Socket)
	if err != nil {
		return nil, fmt.Errorf("Error getting local endpoint: %w", err)
	}
	client, conn, err := podresources.GetV1Client(endpoint, defaultPodResourcesTimeout, defaultPodResourcesMaxSize)
	if err != nil {
		return nil, fmt.Errorf("Error getting gRPC client: %w", err)
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	resp, err := client.List(ctx, &kubeletpodresourcesv1.ListPodResourcesRequest{})
	if err != nil {
		return nil, fmt.Errorf("%v.Get(_) = _, %v", client, err)
	}
	return resp, nil
}

// Returns the current KubeletConfiguration
func getCurrentKubeletConfig(ctx context.Context) (*kubeletconfig.KubeletConfiguration, error) {
	// namespace only relevant if useProxy==true, so we don't bother
	return e2enodekubelet.GetCurrentKubeletConfig(ctx, framework.TestContext.NodeName, "", false, framework.TestContext.StandaloneMode)
}

func addAfterEachForCleaningUpPods(f *framework.Framework) {
	ginkgo.AfterEach(func(ctx context.Context) {
		ginkgo.By("Deleting any Pods created by the test in namespace: " + f.Namespace.Name)
		l, err := e2epod.NewPodClient(f).List(ctx, metav1.ListOptions{})
		framework.ExpectNoError(err)
		for _, p := range l.Items {
			if p.Namespace != f.Namespace.Name {
				continue
			}
			framework.Logf("Deleting pod: %s", p.Name)
			e2epod.NewPodClient(f).DeleteSync(ctx, p.Name, metav1.DeleteOptions{}, f.Timeouts.PodDelete)
		}
	})
}

// Must be called within a Context. Allows the function to modify the KubeletConfiguration during the BeforeEach of the context.
// The change is reverted in the AfterEach of the context.
func tempSetCurrentKubeletConfig(f *framework.Framework, updateFunction func(ctx context.Context, initialConfig *kubeletconfig.KubeletConfiguration)) {
	var oldCfg *kubeletconfig.KubeletConfiguration

	ginkgo.BeforeEach(func(ctx context.Context) {
		var err error
		oldCfg, err = getCurrentKubeletConfig(ctx)
		framework.ExpectNoError(err)

		newCfg := oldCfg.DeepCopy()
		updateFunction(ctx, newCfg)
		if apiequality.Semantic.DeepEqual(*newCfg, *oldCfg) {
			return
		}

		updateKubeletConfig(ctx, f, newCfg, true)
	})

	ginkgo.AfterEach(func(ctx context.Context) {
		if oldCfg != nil {
			// Update the Kubelet configuration.
			updateKubeletConfig(ctx, f, oldCfg, true)
		}
	})
}

func updateKubeletConfig(ctx context.Context, f *framework.Framework, kubeletConfig *kubeletconfig.KubeletConfiguration, deleteStateFiles bool) {
	// Update the Kubelet configuration.
	ginkgo.By("Stopping the kubelet")
	restartKubelet := mustStopKubelet(ctx, f)

	// Delete CPU and memory manager state files to be sure it will not prevent the kubelet restart
	if deleteStateFiles {
		deleteStateFile(cpuManagerStateFile)
		deleteStateFile(memoryManagerStateFile)
	}

	framework.ExpectNoError(e2enodekubelet.WriteKubeletConfigFile(kubeletConfig))

	ginkgo.By("Restarting the kubelet")
	restartKubelet(ctx)
}

func waitForKubeletToStart(ctx context.Context, f *framework.Framework) {
	// wait until the kubelet health check will succeed
	gomega.Eventually(ctx, func() bool {
		return kubeletHealthCheck(kubeletHealthCheckURL)
	}, 2*time.Minute, 5*time.Second).Should(gomega.BeTrueBecause("expected kubelet to be in healthy state"))

	// Wait for the Kubelet to be ready.
	gomega.Eventually(ctx, func(ctx context.Context) bool {
		nodes, err := e2enode.TotalReady(ctx, f.ClientSet)
		framework.ExpectNoError(err)
		return nodes == 1
	}, time.Minute, time.Second).Should(gomega.BeTrueBecause("expected kubelet to be in ready state"))
}

// listNamespaceEvents lists the events in the given namespace.
func listNamespaceEvents(ctx context.Context, c clientset.Interface, ns string) error {
	ls, err := c.CoreV1().Events(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	for _, event := range ls.Items {
		klog.Infof("Event(%#v): type: '%v' reason: '%v' %v", event.InvolvedObject, event.Type, event.Reason, event.Message)
	}
	return nil
}

func logPodEvents(ctx context.Context, f *framework.Framework) {
	framework.Logf("Summary of pod events during the test:")
	err := listNamespaceEvents(ctx, f.ClientSet, f.Namespace.Name)
	framework.ExpectNoError(err)
}

func logNodeEvents(ctx context.Context, f *framework.Framework) {
	framework.Logf("Summary of node events during the test:")
	err := listNamespaceEvents(ctx, f.ClientSet, "")
	framework.ExpectNoError(err)
}

func getLocalNode(ctx context.Context, f *framework.Framework) *v1.Node {
	nodeList, err := e2enode.GetReadySchedulableNodes(ctx, f.ClientSet)
	framework.ExpectNoError(err)
	gomega.Expect(nodeList.Items).Should(gomega.HaveLen(1), "Unexpected number of node objects for node e2e. Expects only one node.")
	return &nodeList.Items[0]
}

// getLocalTestNode fetches the node object describing the local worker node set up by the e2e_node infra, alongside with its ready state.
// getLocalTestNode is a variant of `getLocalNode` which reports but does not set any requirement about the node readiness state, letting
// the caller decide. The check is intentionally done like `getLocalNode` does.
// Note `getLocalNode` aborts (as in ginkgo.Expect) the test implicitly if the worker node is not ready.
func getLocalTestNode(ctx context.Context, f *framework.Framework) (*v1.Node, bool) {
	node, err := f.ClientSet.CoreV1().Nodes().Get(ctx, framework.TestContext.NodeName, metav1.GetOptions{})
	framework.ExpectNoError(err)
	ready := e2enode.IsNodeReady(node)
	schedulable := e2enode.IsNodeSchedulable(node)
	framework.Logf("node %q ready=%v schedulable=%v", node.Name, ready, schedulable)
	return node, ready && schedulable
}

// logKubeletLatencyMetrics logs KubeletLatencyMetrics computed from the Prometheus
// metrics exposed on the current node and identified by the metricNames.
// The Kubelet subsystem prefix is automatically prepended to these metric names.
func logKubeletLatencyMetrics(ctx context.Context, metricNames ...string) {
	metricSet := sets.NewString()
	for _, key := range metricNames {
		metricSet.Insert(kubeletmetrics.KubeletSubsystem + "_" + key)
	}
	metric, err := e2emetrics.GrabKubeletMetricsWithoutProxy(ctx, fmt.Sprintf("%s:%d", nodeNameOrIP(), ports.KubeletReadOnlyPort), "/metrics")
	if err != nil {
		framework.Logf("Error getting kubelet metrics: %v", err)
	} else {
		framework.Logf("Kubelet Metrics: %+v", e2emetrics.GetKubeletLatencyMetrics(metric, metricSet))
	}
}

// getCRIClient connects CRI and returns CRI runtime service clients and image service client.
func getCRIClient() (internalapi.RuntimeService, internalapi.ImageManagerService, error) {
	// connection timeout for CRI service connection
	logger := klog.Background()
	const connectionTimeout = 2 * time.Minute
	runtimeEndpoint := framework.TestContext.ContainerRuntimeEndpoint
	r, err := remote.NewRemoteRuntimeService(runtimeEndpoint, connectionTimeout, noop.NewTracerProvider(), &logger)
	if err != nil {
		return nil, nil, err
	}
	imageManagerEndpoint := runtimeEndpoint
	if framework.TestContext.ImageServiceEndpoint != "" {
		//ImageServiceEndpoint is the same as ContainerRuntimeEndpoint if not
		//explicitly specified
		imageManagerEndpoint = framework.TestContext.ImageServiceEndpoint
	}
	i, err := remote.NewRemoteImageService(imageManagerEndpoint, connectionTimeout, noop.NewTracerProvider(), &logger)
	if err != nil {
		return nil, nil, err
	}
	return r, i, nil
}

func kubeletHealthCheck(url string) bool {
	insecureTransport := http.DefaultTransport.(*http.Transport).Clone()
	insecureTransport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	insecureHTTPClient := &http.Client{
		Transport: insecureTransport,
	}

	req, err := http.NewRequest("HEAD", url, nil)
	if err != nil {
		return false
	}
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", framework.TestContext.BearerToken))
	resp, err := insecureHTTPClient.Do(req)
	if err != nil {
		klog.Warningf("Health check on %q failed, error=%v", url, err)
	} else if resp.StatusCode != http.StatusOK {
		klog.Warningf("Health check on %q failed, status=%d", url, resp.StatusCode)
	}
	return err == nil && resp.StatusCode == http.StatusOK
}

func toCgroupFsName(cgroupName cm.CgroupName) string {
	if kubeletCfg.CgroupDriver == "systemd" {
		return cgroupName.ToSystemd()
	}
	return cgroupName.ToCgroupfs()
}

// reduceAllocatableMemoryUsageIfCgroupv1 uses memory.force_empty (https://lwn.net/Articles/432224/)
// to make the kernel reclaim memory in the allocatable cgroup
// the time to reduce pressure may be unbounded, but usually finishes within a second.
// memory.force_empty is no supported in cgroupv2.
func reduceAllocatableMemoryUsageIfCgroupv1() {
	if !IsCgroup2UnifiedMode() {
		cmd := fmt.Sprintf("echo 0 > /sys/fs/cgroup/memory/%s/memory.force_empty", toCgroupFsName(cm.NewCgroupName(cm.RootCgroupName, defaultNodeAllocatableCgroup)))
		_, err := exec.Command("sudo", "sh", "-c", cmd).CombinedOutput()
		framework.ExpectNoError(err)
	}
}

// Equivalent of featuregatetesting.SetFeatureGateDuringTest
// which can't be used here because we're not in a Testing context.
// This must be in a non-"_test" file to pass
// make verify WHAT=test-featuregates
func withFeatureGate(feature featuregate.Feature, desired bool) func() {
	current := utilfeature.DefaultFeatureGate.Enabled(feature)
	utilfeature.DefaultMutableFeatureGate.Set(fmt.Sprintf("%s=%v", string(feature), desired))
	return func() {
		utilfeature.DefaultMutableFeatureGate.Set(fmt.Sprintf("%s=%v", string(feature), current))
	}
}

// waitForAllContainerRemoval waits until all the containers on a given pod are really gone.
// This is needed by the e2e tests which involve exclusive resource allocation (cpu, topology manager; podresources; etc.)
// In these cases, we need to make sure the tests clean up after themselves to make sure each test runs in
// a pristine environment. The only way known so far to do that is to introduce this wait.
// Worth noting, however, that this makes the test runtime much bigger.
func waitForAllContainerRemoval(ctx context.Context, podName, podNS string) {
	rs, _, err := getCRIClient()
	framework.ExpectNoError(err)
	gomega.Eventually(ctx, func(ctx context.Context) error {
		containers, err := rs.ListContainers(ctx, &runtimeapi.ContainerFilter{
			LabelSelector: map[string]string{
				types.KubernetesPodNameLabel:      podName,
				types.KubernetesPodNamespaceLabel: podNS,
			},
		})
		if err != nil {
			return fmt.Errorf("got error waiting for all containers to be removed from CRI: %v", err)
		}

		if len(containers) > 0 {
			return fmt.Errorf("expected all containers to be removed from CRI but %v containers still remain. Containers: %+v", len(containers), containers)
		}
		return nil
	}, 2*time.Minute, 1*time.Second).Should(gomega.Succeed())
}

func getPidsForProcess(name, pidFile string) ([]int, error) {
	if len(pidFile) > 0 {
		pid, err := getPidFromPidFile(pidFile)
		if err == nil {
			return []int{pid}, nil
		}
		// log the error and fall back to pidof
		runtime.HandleError(err)
	}
	return procfs.PidOf(name)
}

func getPidFromPidFile(pidFile string) (int, error) {
	file, err := os.Open(pidFile)
	if err != nil {
		return 0, fmt.Errorf("error opening pid file %s: %v", pidFile, err)
	}
	defer file.Close()

	data, err := io.ReadAll(file)
	if err != nil {
		return 0, fmt.Errorf("error reading pid file %s: %v", pidFile, err)
	}

	pid, err := strconv.Atoi(string(data))
	if err != nil {
		return 0, fmt.Errorf("error parsing %s as a number: %v", string(data), err)
	}

	return pid, nil
}

// WaitForPodInitContainerRestartCount waits for the given Pod init container
// to achieve at least a given restartCount
// TODO: eventually look at moving to test/e2e/framework/pod
func WaitForPodInitContainerRestartCount(ctx context.Context, c clientset.Interface, namespace, podName string, initContainerIndex int, desiredRestartCount int32, timeout time.Duration) error {
	conditionDesc := fmt.Sprintf("init container %d started", initContainerIndex)
	return e2epod.WaitForPodCondition(ctx, c, namespace, podName, conditionDesc, timeout, func(pod *v1.Pod) (bool, error) {
		if initContainerIndex >= len(pod.Status.InitContainerStatuses) {
			return false, nil
		}
		containerStatus := pod.Status.InitContainerStatuses[initContainerIndex]
		return containerStatus.RestartCount >= desiredRestartCount, nil
	})
}

// WaitForPodContainerRestartCount waits for the given Pod container to achieve at least a given restartCount
// TODO: eventually look at moving to test/e2e/framework/pod
func WaitForPodContainerRestartCount(ctx context.Context, c clientset.Interface, namespace, podName string, containerIndex int, desiredRestartCount int32, timeout time.Duration) error {
	conditionDesc := fmt.Sprintf("container %d started", containerIndex)
	return e2epod.WaitForPodCondition(ctx, c, namespace, podName, conditionDesc, timeout, func(pod *v1.Pod) (bool, error) {
		if containerIndex >= len(pod.Status.ContainerStatuses) {
			return false, nil
		}
		containerStatus := pod.Status.ContainerStatuses[containerIndex]
		return containerStatus.RestartCount >= desiredRestartCount, nil
	})
}

// WaitForPodInitContainerToFail waits for the given Pod init container to fail with the given reason, specifically due to
// invalid container configuration. In this case, the container will remain in a waiting state with a specific
// reason set, which should match the given reason.
// TODO: eventually look at moving to test/e2e/framework/pod
func WaitForPodInitContainerToFail(ctx context.Context, c clientset.Interface, namespace, podName string, containerIndex int, reason string, timeout time.Duration) error {
	conditionDesc := fmt.Sprintf("container %d failed with reason %s", containerIndex, reason)
	return e2epod.WaitForPodCondition(ctx, c, namespace, podName, conditionDesc, timeout, func(pod *v1.Pod) (bool, error) {
		switch pod.Status.Phase {
		case v1.PodPending:
			if len(pod.Status.InitContainerStatuses) == 0 {
				return false, nil
			}
			containerStatus := pod.Status.InitContainerStatuses[containerIndex]
			if containerStatus.State.Waiting != nil && containerStatus.State.Waiting.Reason == reason {
				return true, nil
			}
			return false, nil
		case v1.PodFailed, v1.PodRunning, v1.PodSucceeded:
			return false, fmt.Errorf("pod was expected to be pending, but it is in the state: %s", pod.Status.Phase)
		}
		return false, nil
	})
}

func nodeNameOrIP() string {
	return "localhost"
}
