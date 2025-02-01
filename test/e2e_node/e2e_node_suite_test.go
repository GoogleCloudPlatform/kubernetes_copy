//go:build linux
// +build linux

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

// To run tests in this suite
// NOTE: This test suite requires password-less sudo capabilities to run the kubelet and kube-apiserver.
package e2enode

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"

	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	clientset "k8s.io/client-go/kubernetes"
	cliflag "k8s.io/component-base/cli/flag"
	"k8s.io/kubernetes/pkg/util/rlimit"
	commontest "k8s.io/kubernetes/test/e2e/common"
	"k8s.io/kubernetes/test/e2e/framework"
	e2econfig "k8s.io/kubernetes/test/e2e/framework/config"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"
	e2etestfiles "k8s.io/kubernetes/test/e2e/framework/testfiles"
	e2etestingmanifests "k8s.io/kubernetes/test/e2e/testing-manifests"
	"k8s.io/kubernetes/test/e2e_node/criproxy"
	"k8s.io/kubernetes/test/e2e_node/services"
	e2enodetestingmanifests "k8s.io/kubernetes/test/e2e_node/testing-manifests"
	system "k8s.io/system-validators/validators"

	// define and freeze constants
	_ "k8s.io/kubernetes/test/e2e/feature"

	// reconfigure framework
	_ "k8s.io/kubernetes/test/e2e/framework/debug/init"
	_ "k8s.io/kubernetes/test/e2e/framework/metrics/init"
	_ "k8s.io/kubernetes/test/e2e/framework/node/init"
	_ "k8s.io/kubernetes/test/utils/format"
	"k8s.io/kubernetes/test/utils/ktesting"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"github.com/spf13/pflag"
	"k8s.io/klog/v2"
)

var (
	e2eCriProxy *criproxy.RemoteRuntime
	e2es        *services.E2EServices
	// featureGates is a map of feature names to bools that enable or disable alpha/experimental features.
	featureGates map[string]bool
	// serviceFeatureGates is a map of feature names to bools that enable or
	// disable alpha/experimental features for API service.
	serviceFeatureGates map[string]bool

	// TODO(random-liu): Change the following modes to sub-command.
	runServicesMode    = flag.Bool("run-services-mode", false, "If true, only run services (etcd, apiserver) in current process, and not run test.")
	runKubeletMode     = flag.Bool("run-kubelet-mode", false, "If true, only start kubelet, and not run test.")
	systemValidateMode = flag.Bool("system-validate-mode", false, "If true, only run system validation in current process, and not run test.")
	systemSpecFile     = flag.String("system-spec-file", "", "The name of the system spec file that will be used for node conformance test. If it's unspecified or empty, the default system spec (system.DefaultSysSpec) will be used.")
)

// registerNodeFlags registers flags specific to the node e2e test suite.
func registerNodeFlags(flags *flag.FlagSet) {
	// Mark the test as node e2e when node flags are api.Registry.
	framework.TestContext.NodeE2E = true
	flags.StringVar(&framework.TestContext.BearerToken, "bearer-token", "", "The bearer token to authenticate with. If not specified, it would be a random token. Currently this token is only used in node e2e tests.")
	flags.StringVar(&framework.TestContext.NodeName, "node-name", "", "Name of the node to run tests on.")
	flags.StringVar(&framework.TestContext.KubeletConfigDropinDir, "config-dir", "", "Path to a directory containing drop-in configurations for the kubelet.")
	// TODO(random-liu): Move kubelet start logic out of the test.
	// TODO(random-liu): Move log fetch logic out of the test.
	// There are different ways to start kubelet (systemd, initd, docker, manually started etc.)
	// and manage logs (journald, upstart etc.).
	// For different situation we need to mount different things into the container, run different commands.
	// It is hard and unnecessary to deal with the complexity inside the test suite.
	flags.BoolVar(&framework.TestContext.NodeConformance, "conformance", false, "If true, the test suite will not start kubelet, and fetch system log (kernel, docker, kubelet log etc.) to the report directory.")
	flags.BoolVar(&framework.TestContext.PrepullImages, "prepull-images", true, "If true, prepull images so image pull failures do not cause test failures.")
	flags.BoolVar(&framework.TestContext.RestartKubelet, "restart-kubelet", false, "If true, restart Kubelet unit when the process is killed.")
	flags.StringVar(&framework.TestContext.ImageDescription, "image-description", "", "The description of the image which the test will be running on.")
	flags.StringVar(&framework.TestContext.SystemSpecName, "system-spec-name", "", "The name of the system spec (e.g., gke) that's used in the node e2e test. The system specs are in test/e2e_node/system/specs/. This is used by the test framework to determine which tests to run for validating the system requirements.")
	flags.Var(cliflag.NewMapStringString(&framework.TestContext.ExtraEnvs), "extra-envs", "The extra environment variables needed for node e2e tests. Format: a list of key=value pairs, e.g., env1=val1,env2=val2")
	flags.StringVar(&framework.TestContext.SriovdpConfigMapFile, "sriovdp-configmap-file", "", "The name of the SRIOV device plugin Config Map to load.")
	flag.StringVar(&framework.TestContext.ClusterDNSDomain, "dns-domain", "", "The DNS Domain of the cluster.")
	flag.Var(cliflag.NewMapStringString(&framework.TestContext.RuntimeConfig), "runtime-config", "The runtime configuration used on node e2e tests.")
	flags.BoolVar(&framework.TestContext.RequireDevices, "require-devices", false, "If true, require device plugins to be installed in the running environment.")
	flags.Var(cliflag.NewMapStringBool(&featureGates), "feature-gates", "A set of key=value pairs that describe feature gates for alpha/experimental features.")
	flags.Var(cliflag.NewMapStringBool(&serviceFeatureGates), "service-feature-gates", "A set of key=value pairs that describe feature gates for alpha/experimental features for API service.")
	flags.BoolVar(&framework.TestContext.StandaloneMode, "standalone-mode", false, "If true, starts kubelet in standalone mode.")
	flags.BoolVar(&framework.TestContext.CriProxyEnabled, "cri-proxy-enabled", false, "If true, enable CRI API proxy for failure injection.")
}

func init() {
	// Enable embedded FS file lookup as fallback
	e2etestfiles.AddFileSource(e2etestingmanifests.GetE2ETestingManifestsFS())
	e2etestfiles.AddFileSource(e2enodetestingmanifests.GetE2ENodeTestingManifestsFS())
}

func TestMain(m *testing.M) {
	// Copy go flags in TestMain, to ensure go test flags are registered (no longer available in init() as of go1.13)
	e2econfig.CopyFlags(e2econfig.Flags, flag.CommandLine)
	framework.RegisterCommonFlags(flag.CommandLine)
	registerNodeFlags(flag.CommandLine)
	pflag.CommandLine.AddGoFlagSet(flag.CommandLine)
	// Mark the run-services-mode flag as hidden to prevent user from using it.
	pflag.CommandLine.MarkHidden("run-services-mode")
	// It's weird that if I directly use pflag in TestContext, it will report error.
	// It seems that someone is using flag.Parse() after init() and TestMain().
	// TODO(random-liu): Find who is using flag.Parse() and cause errors and move the following logic
	// into TestContext.
	// TODO(pohly): remove RegisterNodeFlags from test_context.go enable Viper config support here?

	pflag.Parse()
	if pflag.CommandLine.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "unknown additional command line arguments: %s", pflag.CommandLine.Args())
		os.Exit(1)
	}
	framework.AfterReadingAllFlags(&framework.TestContext)
	if err := e2eskipper.InitFeatureGates(utilfeature.DefaultFeatureGate, featureGates); err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: initialize feature gates: %v", err)
		os.Exit(1)
	}

	if err := services.SetFeatureGatesForInProcessComponents(serviceFeatureGates); err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: initialize process feature gates for API service: %v", err)
		os.Exit(1)
	}

	setExtraEnvs()
	os.Exit(m.Run())
}

// When running the containerized conformance test, we'll mount the
// host root filesystem as readonly to /rootfs.
const rootfs = "/rootfs"

func TestE2eNode(t *testing.T) {
	tCtx := ktesting.Init(t)
	// Make sure we are not limited by sshd when it comes to open files
	if err := rlimit.SetNumFiles(1000000); err != nil {
		klog.Infof("failed to set rlimit on max file handles: %v", err)
	}

	if *runServicesMode {
		// If run-services-mode is specified, only run services in current process.
		services.RunE2EServices(t)
		return
	}
	if *runKubeletMode {
		// If run-kubelet-mode is specified, only start kubelet.
		services.RunKubelet(tCtx, featureGates)
		return
	}
	if *systemValidateMode {
		// If system-validate-mode is specified, only run system validation in current process.
		spec := &system.DefaultSysSpec
		if *systemSpecFile != "" {
			var err error
			spec, err = loadSystemSpecFromFile(*systemSpecFile)
			if err != nil {
				klog.Exitf("Failed to load system spec: %v", err)
			}
		}
		if framework.TestContext.NodeConformance {
			// Chroot to /rootfs to make system validation can check system
			// as in the root filesystem.
			// TODO(random-liu): Consider to chroot the whole test process to make writing
			// test easier.
			if err := syscall.Chroot(rootfs); err != nil {
				klog.Exitf("chroot %q failed: %v", rootfs, err)
			}
		}
		warns, errs := system.ValidateSpec(*spec, "remote")
		if len(warns) != 0 {
			klog.Warningf("system validation warns: %v", warns)
		}
		if len(errs) != 0 {
			klog.Exitf("system validation failed: %v", errs)
		}
		return
	}

	// We're not running in a special mode so lets run tests.
	gomega.RegisterFailHandler(ginkgo.Fail)
	// Initialize the KubeletConfigDropinDir again if the test doesn't run in run-kubelet-mode.
	if framework.TestContext.KubeletConfigDropinDir == "" {
		var err error
		framework.TestContext.KubeletConfigDropinDir, err = services.KubeletConfigDirCWDDir()
		if err != nil {
			klog.Errorf("failed to create kubelet config directory: %v", err)
		}
	}
	reportDir := framework.TestContext.ReportDir
	if reportDir != "" {
		// Create the directory if it doesn't already exist
		// NOTE: junit report can be simply created by executing your tests with the new --junit-report flags instead.
		if err := os.MkdirAll(reportDir, 0755); err != nil {
			klog.Errorf("Failed creating report directory: %v", err)
		}
	}

	// annotate created pods with source code location to make it easier to find tests
	// which do insufficient cleanup and pollute the node state with lingering pods
	e2epod.GlobalOwnerTracking = true

	suiteConfig, reporterConfig := framework.CreateGinkgoConfig()
	ginkgo.RunSpecs(t, "E2eNode Suite", suiteConfig, reporterConfig)
}

// Setup the kubelet on the node
var _ = ginkgo.SynchronizedBeforeSuite(func(ctx context.Context) []byte {
	// Run system validation test.
	gomega.Expect(validateSystem()).To(gomega.Succeed(), "system validation")

	// Pre-pull the images tests depend on so we can fail immediately if there is an image pull issue
	// This helps with debugging test flakes since it is hard to tell when a test failure is due to image pulling.
	if framework.TestContext.PrepullImages {
		klog.Infof("Pre-pulling images so that they are cached for the tests.")
		updateImageAllowList(ctx)
		err := PrePullAllImages(ctx)
		gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
	}

	// TODO(yifan): Temporary workaround to disable coreos from auto restart
	// by masking the locksmithd.
	// We should mask locksmithd when provisioning the machine.
	maskLocksmithdOnCoreos()

	if framework.TestContext.CriProxyEnabled {
		framework.Logf("Start cri proxy")
		rs, is, err := getCRIClient()
		framework.ExpectNoError(err)

		e2eCriProxy = criproxy.NewRemoteRuntimeProxy(rs, is)
		endpoint, err := criproxy.GenerateEndpoint()
		framework.ExpectNoError(err)

		err = e2eCriProxy.Start(endpoint)
		framework.ExpectNoError(err)

		framework.TestContext.ContainerRuntimeEndpoint = endpoint
		framework.TestContext.ImageServiceEndpoint = endpoint
	}

	if *startServices {
		// If the services are expected to stop after test, they should monitor the test process.
		// If the services are expected to keep running after test, they should not monitor the test process.
		e2es = services.NewE2EServices(*stopServices)
		gomega.Expect(e2es.Start(ctx, featureGates)).To(gomega.Succeed(), "should be able to start node services.")
	} else {
		klog.Infof("Running tests without starting services.")
	}

	if !framework.TestContext.StandaloneMode {
		klog.Infof("Wait for the node to be ready")
		waitForNodeReady(ctx)
	}

	// Reference common test to make the import valid.
	commontest.CurrentSuite = commontest.NodeE2E

	// ginkgo would spawn multiple processes to run tests.
	// Since the bearer token is generated randomly at run time,
	// we need to distribute the bearer token to other processes to make them use the same token.
	return []byte(framework.TestContext.BearerToken)
}, func(ctx context.Context, token []byte) {
	framework.TestContext.BearerToken = string(token)
	// update test context with node configuration.
	gomega.Expect(updateTestContext(ctx)).To(gomega.Succeed(), "update test context with node config.")

	// Store current Kubelet configuration in the package variable
	// This assumes all tests which dynamically change kubelet configuration
	// must: 1) run in serial; 2) restore kubelet configuration after test.
	var err error
	kubeletCfg, err = getCurrentKubeletConfig(ctx)
	framework.ExpectNoError(err)
})

// Tear down the kubelet on the node
var _ = ginkgo.SynchronizedAfterSuite(func() {}, func() {
	if e2es != nil {
		if *startServices && *stopServices {
			klog.Infof("Stopping node services...")
			e2es.Stop()
		}
	}

	if e2eCriProxy != nil {
		framework.Logf("Stopping cri proxy service...")
		e2eCriProxy.Stop()
	}

	klog.Infof("Tests Finished")
})

// validateSystem runs system validation in a separate process and returns error if validation fails.
func validateSystem() error {
	testBin, err := os.Executable()
	if err != nil {
		return fmt.Errorf("can't get current binary: %w", err)
	}
	// Pass all flags into the child process, so that it will see the same flag set.
	output, err := exec.Command(testBin, append([]string{"--system-validate-mode"}, os.Args[1:]...)...).CombinedOutput()
	// The output of system validation should have been formatted, directly print here.
	fmt.Print(string(output))
	if err != nil {
		return fmt.Errorf("system validation failed: %w", err)
	}
	return nil
}

func maskLocksmithdOnCoreos() {
	data, err := os.ReadFile("/etc/os-release")
	if err != nil {
		// Not all distros contain this file.
		klog.Infof("Could not read /etc/os-release: %v", err)
		return
	}
	if bytes.Contains(data, []byte("ID=coreos")) {
		output, err := exec.Command("systemctl", "mask", "--now", "locksmithd").CombinedOutput()
		framework.ExpectNoError(err, fmt.Sprintf("should be able to mask locksmithd - output: %q", string(output)))
		klog.Infof("Locksmithd is masked successfully")
	}
}

func waitForNodeReady(ctx context.Context) {
	const (
		// nodeReadyTimeout is the time to wait for node to become ready.
		nodeReadyTimeout = 2 * time.Minute
		// nodeReadyPollInterval is the interval to check node ready.
		nodeReadyPollInterval = 1 * time.Second
	)
	client, err := getAPIServerClient()
	framework.ExpectNoError(err, "should be able to get apiserver client.")
	gomega.Eventually(ctx, func() error {
		node, err := getNode(client)
		if err != nil {
			return fmt.Errorf("failed to get node: %w", err)
		}
		if !isNodeReady(node) {
			return fmt.Errorf("node is not ready: %+v", node)
		}
		return nil
	}, nodeReadyTimeout, nodeReadyPollInterval).Should(gomega.Succeed())
}

// updateTestContext updates the test context with the node name.
func updateTestContext(ctx context.Context) error {
	setExtraEnvs()
	updateImageAllowList(ctx)

	client, err := getAPIServerClient()
	if err != nil {
		return fmt.Errorf("failed to get apiserver client: %w", err)
	}

	if !framework.TestContext.StandaloneMode {
		// Update test context with current node object.
		node, err := getNode(client)
		if err != nil {
			return fmt.Errorf("failed to get node: %w", err)
		}
		framework.TestContext.NodeName = node.Name // Set node name from API server, it is already set to the computer name by default.
	}

	framework.Logf("Node name: %s", framework.TestContext.NodeName)

	return nil
}

// getNode gets node object from the apiserver.
func getNode(c *clientset.Clientset) (*v1.Node, error) {
	nodes, err := c.CoreV1().Nodes().List(context.TODO(), metav1.ListOptions{})
	framework.ExpectNoError(err, "should be able to list nodes.")
	if nodes == nil {
		return nil, fmt.Errorf("the node list is nil")
	}
	gomega.Expect(len(nodes.Items)).To(gomega.BeNumerically("<=", 1), "the number of nodes is more than 1.")
	if len(nodes.Items) == 0 {
		return nil, fmt.Errorf("empty node list: %+v", nodes)
	}
	return &nodes.Items[0], nil
}

// getAPIServerClient gets a apiserver client.
func getAPIServerClient() (*clientset.Clientset, error) {
	config, err := framework.LoadConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to load config: %w", err)
	}
	client, err := clientset.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create client: %w", err)
	}
	return client, nil
}

// loadSystemSpecFromFile returns the system spec from the file with the
// filename.
func loadSystemSpecFromFile(filename string) (*system.SysSpec, error) {
	b, err := os.ReadFile(filename)
	if err != nil {
		return nil, err
	}
	data, err := utilyaml.ToJSON(b)
	if err != nil {
		return nil, err
	}
	spec := new(system.SysSpec)
	if err := json.Unmarshal(data, spec); err != nil {
		return nil, err
	}
	return spec, nil
}

// isNodeReady returns true if a node is ready; false otherwise.
func isNodeReady(node *v1.Node) bool {
	for _, c := range node.Status.Conditions {
		if c.Type == v1.NodeReady {
			return c.Status == v1.ConditionTrue
		}
	}
	return false
}

func setExtraEnvs() {
	for name, value := range framework.TestContext.ExtraEnvs {
		os.Setenv(name, value)
	}
}
