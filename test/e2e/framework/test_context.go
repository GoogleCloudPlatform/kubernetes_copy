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

package framework

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/ginkgo/v2/reporters"
	"github.com/onsi/ginkgo/v2/types"
	"github.com/onsi/gomega"
	gomegaformat "github.com/onsi/gomega/format"

	"k8s.io/apimachinery/pkg/util/sets"
	restclient "k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	cliflag "k8s.io/component-base/cli/flag"
	"k8s.io/klog/v2"

	"k8s.io/kubernetes/test/e2e/framework/internal/junit"
	"k8s.io/kubernetes/test/utils/image"
	"k8s.io/kubernetes/test/utils/kubeconfig"
)

const (
	defaultHost = "https://127.0.0.1:6443"

	// DefaultNumNodes is the number of nodes. If not specified, then number of nodes is auto-detected
	DefaultNumNodes = -1
)

var (
	// Output is used for output when not running tests, for example in -list-tests.
	// Test output should go to ginkgo.GinkgoWriter.
	Output io.Writer = os.Stdout

	// Exit is called when the framework detects fatal errors or when
	// it is done with the execution of e.g. -list-tests.
	Exit = os.Exit

	// CheckForBugs determines whether the framework bails out when
	// test initialization found any bugs.
	CheckForBugs = true
)

// TestContextType contains test settings and global state. Due to
// historic reasons, it is a mixture of items managed by the test
// framework itself, cloud providers and individual tests.
// The goal is to move anything not required by the framework
// into the code which uses the settings.
//
// The recommendation for those settings is:
//   - They are stored in their own context structure or local
//     variables.
//   - The standard `flag` package is used to register them.
//     The flag name should follow the pattern <part1>.<part2>....<partn>
//     where the prefix is unlikely to conflict with other tests or
//     standard packages and each part is in lower camel case. For
//     example, test/e2e/storage/csi/context.go could define
//     storage.csi.numIterations.
//   - framework/config can be used to simplify the registration of
//     multiple options with a single function call:
//     var storageCSI {
//     NumIterations `default:"1" usage:"number of iterations"`
//     }
//     _ config.AddOptions(&storageCSI, "storage.csi")
//   - The direct use Viper in tests is possible, but discouraged because
//     it only works in test suites which use Viper (which is not
//     required) and the supported options cannot be
//     discovered by a test suite user.
//
// Test suite authors can use framework/viper to make all command line
// parameters also configurable via a configuration file.
type TestContextType struct {
	KubeConfig             string
	KubeContext            string
	KubeAPIContentType     string
	KubeletRootDir         string
	KubeletConfigDropinDir string
	CertDir                string
	Host                   string
	BearerToken            string `datapolicy:"token"`
	// TODO: Deprecating this over time... instead just use gobindata_util.go , see #23987.
	RepoRoot string
	// ListImages will list off all images that are used then quit
	ListImages bool

	listTests, listLabels bool

	// ListConformanceTests will list off all conformance tests that are available then quit
	ListConformanceTests bool

	// Provider identifies the infrastructure provider (gce, gke, aws)
	Provider string

	// Tooling is the tooling in use (e.g. kops, gke).  Provider is the cloud provider and might not uniquely identify the tooling.
	Tooling string

	// timeouts contains user-configurable timeouts for various operations.
	// Individual Framework instance also have such timeouts which may be
	// different from these here. To avoid confusion, this field is not
	// exported. Its values can be accessed through
	// NewTimeoutContext.
	timeouts TimeoutContext

	CloudConfig                 CloudConfig
	KubectlPath                 string
	OutputDir                   string
	ReportDir                   string
	ReportPrefix                string
	ReportCompleteGinkgo        bool
	ReportCompleteJUnit         bool
	Prefix                      string
	MinStartupPods              int
	EtcdUpgradeStorage          string
	EtcdUpgradeVersion          string
	GCEUpgradeScript            string
	ContainerRuntimeEndpoint    string
	ContainerRuntimeProcessName string
	ContainerRuntimePidFile     string
	// SystemdServices are comma separated list of systemd services the test framework
	// will dump logs for.
	SystemdServices string
	// DumpSystemdJournal controls whether to dump the full systemd journal.
	DumpSystemdJournal       bool
	ImageServiceEndpoint     string
	MasterOSDistro           string
	NodeOSDistro             string
	NodeOSArch               string
	VerifyServiceAccount     bool
	DeleteNamespace          bool
	DeleteNamespaceOnFailure bool
	CleanupOnFailure         bool
	AllowedNotReadyNodes     int
	CleanStart               bool
	// If set to 'true' or 'all' framework will start a goroutine monitoring resource usage of system add-ons.
	// It will read the data every 30 seconds from all Nodes and print summary during afterEach. If set to 'master'
	// only master Node will be monitored.
	GatherKubeSystemResourceUsageData string
	GatherLogsSizes                   bool
	GatherMetricsAfterTest            string
	GatherSuiteMetricsAfterTest       bool
	MaxNodesToGather                  int
	// If set to 'true' framework will gather ClusterAutoscaler metrics when gathering them for other components.
	IncludeClusterAutoscalerMetrics bool
	// Currently supported values are 'hr' for human-readable and 'json'. It's a comma separated list.
	OutputPrintType string
	// CreateTestingNS is responsible for creating namespace used for executing e2e tests.
	// It accepts namespace base name, which will be prepended with e2e prefix, kube client
	// and labels to be applied to a namespace.
	CreateTestingNS CreateTestingNSFn
	// If set to true test will dump data about the namespace in which test was running.
	DumpLogsOnFailure bool
	// Disables dumping cluster log from master and nodes after all tests.
	DisableLogDump bool
	// Path to the GCS artifacts directory to dump logs from nodes. Logexporter gets enabled if this is non-empty.
	LogexporterGCSPath string
	// Node e2e specific test context
	NodeTestContextType

	// The DNS Domain of the cluster.
	ClusterDNSDomain string

	// The configuration of NodeKiller.
	NodeKiller NodeKillerConfig

	// The Default IP Family of the cluster ("ipv4" or "ipv6")
	IPFamily string

	// NonblockingTaints is the comma-delimeted string given by the user to specify taints which should not stop the test framework from running tests.
	NonblockingTaints string

	// ProgressReportURL is the URL which progress updates will be posted to as tests complete. If empty, no updates are sent.
	ProgressReportURL string

	// SriovdpConfigMapFile is the path to the ConfigMap to configure the SRIOV device plugin on this host.
	SriovdpConfigMapFile string

	// SpecSummaryOutput is the file to write ginkgo.SpecSummary objects to as tests complete. Useful for debugging and test introspection.
	SpecSummaryOutput string

	// DockerConfigFile is a file that contains credentials which can be used to pull images from certain private registries, needed for a test.
	DockerConfigFile string

	// E2EDockerConfigFile is a docker credentials configuration file used which contains authorization token that can be used to pull images from certain private registries provided by the users.
	// For more details refer https://kubernetes.io/docs/tasks/configure-pod-container/pull-image-private-registry/#log-in-to-docker-hub
	E2EDockerConfigFile string

	// KubeTestRepoConfigFile is a yaml file used for overriding registries for test images.
	KubeTestRepoList string

	// SnapshotControllerPodName is the name used for identifying the snapshot controller pod.
	SnapshotControllerPodName string

	// SnapshotControllerHTTPPort the port used for communicating with the snapshot controller HTTP endpoint.
	SnapshotControllerHTTPPort int

	// RequireDevices makes mandatory on the environment on which tests are run 1+ devices exposed through device plugins.
	// With this enabled The e2e tests requiring devices for their operation can assume that if devices aren't reported, the test can fail
	RequireDevices bool

	// Enable volume drivers which are disabled by default. See test/e2e/storage/in_tree_volumes.go for details.
	EnabledVolumeDrivers []string
}

// NodeKillerConfig describes configuration of NodeKiller -- a utility to
// simulate node failures.
//
// TODO: move this and the corresponding command line flags into
// test/e2e/framework/node.
type NodeKillerConfig struct {
	// Enabled determines whether NodeKill should do anything at all.
	// All other options below are ignored if Enabled = false.
	Enabled bool
	// FailureRatio is a percentage of all nodes that could fail simultinously.
	FailureRatio float64
	// Interval is time between node failures.
	Interval time.Duration
	// JitterFactor is factor used to jitter node failures.
	// Node will be killed between [Interval, Interval + (1.0 + JitterFactor)].
	JitterFactor float64
	// SimulatedDowntime is a duration between node is killed and recreated.
	SimulatedDowntime time.Duration
	// NodeKillerStopCtx is a context that is used to notify NodeKiller to stop killing nodes.
	NodeKillerStopCtx context.Context
	// NodeKillerStop is the cancel function for NodeKillerStopCtx.
	NodeKillerStop func()
}

// NodeTestContextType is part of TestContextType, it is shared by all node e2e test.
type NodeTestContextType struct {
	// NodeE2E indicates whether it is running node e2e.
	NodeE2E bool
	// Name of the node to run tests on.
	NodeName string
	// NodeConformance indicates whether the test is running in node conformance mode.
	NodeConformance bool
	// PrepullImages indicates whether node e2e framework should prepull images.
	PrepullImages bool
	// ImageDescription is the description of the image on which the test is running.
	ImageDescription string
	// RuntimeConfig is a map of API server runtime configuration values.
	RuntimeConfig map[string]string
	// SystemSpecName is the name of the system spec (e.g., gke) that's used in
	// the node e2e test. If empty, the default one (system.DefaultSpec) is
	// used. The system specs are in test/e2e_node/system/specs/.
	SystemSpecName string
	// RestartKubelet restarts Kubelet unit when the process is killed.
	RestartKubelet bool
	// ExtraEnvs is a map of environment names to values.
	ExtraEnvs map[string]string
	// StandaloneMode indicates whether the test is running kubelet in a standalone mode.
	StandaloneMode bool
	// CriProxyEnabled indicates whether enable CRI API proxy for failure injection.
	CriProxyEnabled bool
}

// CloudConfig holds the cloud configuration for e2e test suites.
type CloudConfig struct {
	APIEndpoint       string
	ProjectID         string
	Zone              string   // for multizone tests, arbitrarily chosen zone
	Zones             []string // for multizone tests, use this set of zones instead of querying the cloud provider. Must include Zone.
	Region            string
	MultiZone         bool
	MultiMaster       bool
	Cluster           string
	MasterName        string
	NodeInstanceGroup string // comma-delimited list of groups' names
	NumNodes          int
	ClusterIPRange    string
	ClusterTag        string
	Network           string
	ConfigFile        string // for azure
	NodeTag           string
	MasterTag         string

	Provider ProviderInterface
}

// TestContext should be used by all tests to access common context data.
var TestContext = TestContextType{
	timeouts: defaultTimeouts,
}

// StringArrayValue is used with flag.Var for a comma-separated list of strings placed into a string array.
type stringArrayValue struct {
	stringArray *[]string
}

func (v stringArrayValue) String() string {
	if v.stringArray != nil {
		return strings.Join(*v.stringArray, ",")
	}
	return ""
}

func (v stringArrayValue) Set(s string) error {
	if len(s) == 0 {
		*v.stringArray = []string{}
	} else {
		*v.stringArray = strings.Split(s, ",")
	}
	return nil
}

// ClusterIsIPv6 returns true if the cluster is IPv6
func (tc TestContextType) ClusterIsIPv6() bool {
	return tc.IPFamily == "ipv6"
}

// RegisterCommonFlags registers flags common to all e2e test suites.
// The flag set can be flag.CommandLine (if desired) or a custom
// flag set that then gets passed to viperconfig.ViperizeFlags.
//
// The other Register*Flags methods below can be used to add more
// test-specific flags. However, those settings then get added
// regardless whether the test is actually in the test suite.
//
// For tests that have been converted to registering their
// options themselves, copy flags from test/e2e/framework/config
// as shown in HandleFlags.
func RegisterCommonFlags(flags *flag.FlagSet) {
	// The default is too low for objects like pods, even when using YAML. We double the default.
	flags.IntVar(&gomegaformat.MaxLength, "gomega-max-length", 8000, "Sets the maximum size for the gomega formatter (= gomega.MaxLength). Use 0 to disable truncation.")

	flags.StringVar(&TestContext.GatherKubeSystemResourceUsageData, "gather-resource-usage", "false", "If set to 'true' or 'all' framework will be monitoring resource usage of system all add-ons in (some) e2e tests, if set to 'master' framework will be monitoring master node only, if set to 'none' of 'false' monitoring will be turned off.")
	flags.BoolVar(&TestContext.GatherLogsSizes, "gather-logs-sizes", false, "If set to true framework will be monitoring logs sizes on all machines running e2e tests.")
	flags.IntVar(&TestContext.MaxNodesToGather, "max-nodes-to-gather-from", 20, "The maximum number of nodes to gather extended info from on test failure.")
	flags.StringVar(&TestContext.GatherMetricsAfterTest, "gather-metrics-at-teardown", "false", "If set to 'true' framework will gather metrics from all components after each test. If set to 'master' only master component metrics would be gathered.")
	flags.BoolVar(&TestContext.GatherSuiteMetricsAfterTest, "gather-suite-metrics-at-teardown", false, "If set to true framework will gather metrics from all components after the whole test suite completes.")
	flags.BoolVar(&TestContext.IncludeClusterAutoscalerMetrics, "include-cluster-autoscaler", false, "If set to true, framework will include Cluster Autoscaler when gathering metrics.")
	flags.StringVar(&TestContext.OutputPrintType, "output-print-type", "json", "Format in which summaries should be printed: 'hr' for human readable, 'json' for JSON ones.")
	flags.BoolVar(&TestContext.DumpLogsOnFailure, "dump-logs-on-failure", true, "If set to true test will dump data about the namespace in which test was running.")
	flags.BoolVar(&TestContext.DisableLogDump, "disable-log-dump", false, "If set to true, logs from master and nodes won't be gathered after test run.")
	flags.StringVar(&TestContext.LogexporterGCSPath, "logexporter-gcs-path", "", "Path to the GCS artifacts directory to dump logs from nodes. Logexporter gets enabled if this is non-empty.")
	flags.BoolVar(&TestContext.DeleteNamespace, "delete-namespace", true, "If true tests will delete namespace after completion. It is only designed to make debugging easier, DO NOT turn it off by default.")
	flags.BoolVar(&TestContext.DeleteNamespaceOnFailure, "delete-namespace-on-failure", true, "If true, framework will delete test namespace on failure. Used only during test debugging.")
	flags.BoolVar(&TestContext.CleanupOnFailure, "cleanup-on-failure", true, "If true, framework will cleanup resources created by tests, after test failure. Used only during test debugging.")
	flags.IntVar(&TestContext.AllowedNotReadyNodes, "allowed-not-ready-nodes", 0, "If greater than zero, framework will allow for that many non-ready nodes when checking for all ready nodes. If -1, no waiting will be performed for ready nodes or daemonset pods.")

	flags.StringVar(&TestContext.Host, "host", "", fmt.Sprintf("The host, or apiserver, to connect to. Will default to %s if this argument and --kubeconfig are not set.", defaultHost))
	flags.StringVar(&TestContext.ReportPrefix, "report-prefix", "", "Optional prefix for JUnit XML reports. Default is empty, which doesn't prepend anything to the default name.")
	flags.StringVar(&TestContext.ReportDir, "report-dir", "", "Path to the directory where the simplified JUnit XML reports and other tests results should be saved. Default is empty, which doesn't generate these reports.  If ginkgo's -junit-report parameter is used, that parameter instead of -report-dir determines the location of a single JUnit report.")
	flags.BoolVar(&TestContext.ReportCompleteGinkgo, "report-complete-ginkgo", false, "Enables writing a complete test report as Ginkgo JSON to <report dir>/ginkgo/report.json. Ignored if --report-dir is not set.")
	flags.BoolVar(&TestContext.ReportCompleteJUnit, "report-complete-junit", false, "Enables writing a complete test report as JUnit XML to <report dir>/ginkgo/report.json. Ignored if --report-dir is not set.")
	flags.StringVar(&TestContext.ContainerRuntimeEndpoint, "container-runtime-endpoint", "unix:///run/containerd/containerd.sock", "The container runtime endpoint of cluster VM instances.")
	flags.StringVar(&TestContext.ContainerRuntimeProcessName, "container-runtime-process-name", "containerd", "The name of the container runtime process.")
	flags.StringVar(&TestContext.ContainerRuntimePidFile, "container-runtime-pid-file", "/run/containerd/containerd.pid", "The pid file of the container runtime.")
	flags.StringVar(&TestContext.SystemdServices, "systemd-services", "containerd*", "The comma separated list of systemd services the framework will dump logs for.")
	flags.BoolVar(&TestContext.DumpSystemdJournal, "dump-systemd-journal", false, "Whether to dump the full systemd journal.")
	flags.StringVar(&TestContext.ImageServiceEndpoint, "image-service-endpoint", "", "The image service endpoint of cluster VM instances.")
	flags.StringVar(&TestContext.NonblockingTaints, "non-blocking-taints", `node-role.kubernetes.io/control-plane`, "Nodes with taints in this comma-delimited list will not block the test framework from starting tests.")

	flags.BoolVar(&TestContext.ListImages, "list-images", false, "If true, will show list of images used for running tests.")
	flags.BoolVar(&TestContext.listLabels, "list-labels", false, "If true, will show the list of labels that can be used to select tests via -ginkgo.label-filter.")
	flags.BoolVar(&TestContext.listTests, "list-tests", false, "If true, will show the full names of all tests (aka specs) that can be used to select test via -ginkgo.focus/skip.")
	flags.StringVar(&TestContext.KubectlPath, "kubectl-path", "kubectl", "The kubectl binary to use. For development, you might use 'cluster/kubectl.sh' here.")

	flags.StringVar(&TestContext.ProgressReportURL, "progress-report-url", "", "The URL to POST progress updates to as the suite runs to assist in aiding integrations. If empty, no messages sent.")
	flags.StringVar(&TestContext.SpecSummaryOutput, "spec-dump", "", "The file to dump all ginkgo.SpecSummary to after tests run. If empty, no objects are saved/printed.")
	flags.StringVar(&TestContext.DockerConfigFile, "docker-config-file", "", "A docker credential file which contains authorization token that is used to perform image pull tests from an authenticated registry. For more details regarding the content of the file refer https://kubernetes.io/docs/tasks/configure-pod-container/pull-image-private-registry/#log-in-to-docker-hub")

	flags.StringVar(&TestContext.E2EDockerConfigFile, "e2e-docker-config-file", "", "A docker credentials configuration file used which contains authorization token that can be used to pull images from certain private registries provided by the users. For more details refer https://kubernetes.io/docs/tasks/configure-pod-container/pull-image-private-registry/#log-in-to-docker-hub")
	flags.StringVar(&TestContext.KubeTestRepoList, "kube-test-repo-list", "", "A yaml file used for overriding registries for test images. Alternatively, the KUBE_TEST_REPO_LIST env variable can be set.")

	flags.StringVar(&TestContext.SnapshotControllerPodName, "snapshot-controller-pod-name", "", "The pod name to use for identifying the snapshot controller in the kube-system namespace.")
	flags.IntVar(&TestContext.SnapshotControllerHTTPPort, "snapshot-controller-http-port", 0, "The port to use for snapshot controller HTTP communication.")

	flags.Var(&stringArrayValue{&TestContext.EnabledVolumeDrivers}, "enabled-volume-drivers", "Comma-separated list of in-tree volume drivers to enable for testing. This is only needed for in-tree drivers disabled by default. An example is gcepd; see test/e2e/storage/in_tree_volumes.go for full details.")
}

func CreateGinkgoConfig() (types.SuiteConfig, types.ReporterConfig) {
	// fetch the current config
	suiteConfig, reporterConfig := ginkgo.GinkgoConfiguration()
	// Randomize specs as well as suites
	suiteConfig.RandomizeAllSpecs = true
	// Disable skipped tests unless they are explicitly requested.
	if len(suiteConfig.FocusStrings) == 0 && len(suiteConfig.SkipStrings) == 0 && suiteConfig.LabelFilter == "" {
		suiteConfig.SkipStrings = []string{`\[Flaky\]|\[Feature:.+\]`}
	}
	return suiteConfig, reporterConfig
}

// RegisterClusterFlags registers flags specific to the cluster e2e test suite.
func RegisterClusterFlags(flags *flag.FlagSet) {
	flags.BoolVar(&TestContext.VerifyServiceAccount, "e2e-verify-service-account", true, "If true tests will verify the service account before running.")
	flags.StringVar(&TestContext.KubeConfig, clientcmd.RecommendedConfigPathFlag, os.Getenv(clientcmd.RecommendedConfigPathEnvVar), "Path to kubeconfig containing embedded authinfo.")
	flags.StringVar(&TestContext.KubeContext, clientcmd.FlagContext, "", "kubeconfig context to use/override. If unset, will use value from 'current-context'")
	flags.StringVar(&TestContext.KubeAPIContentType, "kube-api-content-type", "application/vnd.kubernetes.protobuf", "ContentType used to communicate with apiserver")

	flags.StringVar(&TestContext.KubeletRootDir, "kubelet-root-dir", "/var/lib/kubelet", "The data directory of kubelet. Some tests (for example, CSI storage tests) deploy DaemonSets which need to know this value and cannot query it. Such tests only work in clusters where the data directory is the same on all nodes.")
	flags.StringVar(&TestContext.KubeletRootDir, "volume-dir", "/var/lib/kubelet", "An alias for --kubelet-root-dir, kept for backwards compatibility.")
	flags.StringVar(&TestContext.CertDir, "cert-dir", "", "Path to the directory containing the certs. Default is empty, which doesn't use certs.")
	flags.StringVar(&TestContext.RepoRoot, "repo-root", "../../", "Root directory of kubernetes repository, for finding test files.")
	// NOTE: Node E2E tests have this flag defined as well, but true by default.
	// If this becomes true as well, they should be refactored into RegisterCommonFlags.
	flags.BoolVar(&TestContext.PrepullImages, "prepull-images", false, "If true, prepull images so image pull failures do not cause test failures.")
	flags.StringVar(&TestContext.Provider, "provider", "", "The name of the Kubernetes provider (gce, gke, local, skeleton (the fallback if not set), etc.)")
	flags.StringVar(&TestContext.Tooling, "tooling", "", "The tooling in use (kops, gke, etc.)")
	flags.StringVar(&TestContext.OutputDir, "e2e-output-dir", "/tmp", "Output directory for interesting/useful test data, like performance data, benchmarks, and other metrics.")
	flags.StringVar(&TestContext.Prefix, "prefix", "e2e", "A prefix to be added to cloud resources created during testing.")
	flags.StringVar(&TestContext.MasterOSDistro, "master-os-distro", "debian", "The OS distribution of cluster master (debian, ubuntu, gci, coreos, or custom).")
	flags.StringVar(&TestContext.NodeOSDistro, "node-os-distro", "debian", "The OS distribution of cluster VM instances (debian, ubuntu, gci, coreos, windows, or custom), which determines how specific tests are implemented.")
	flags.StringVar(&TestContext.NodeOSArch, "node-os-arch", "amd64", "The OS architecture of cluster VM instances (amd64, arm64, or custom).")
	flags.StringVar(&TestContext.ClusterDNSDomain, "dns-domain", "cluster.local", "The DNS Domain of the cluster.")

	// TODO: Flags per provider?  Rename gce-project/gce-zone?
	cloudConfig := &TestContext.CloudConfig
	flags.StringVar(&cloudConfig.MasterName, "kube-master", "", "Name of the kubernetes master. Only required if provider is gce or gke")
	flags.StringVar(&cloudConfig.APIEndpoint, "gce-api-endpoint", "", "The GCE APIEndpoint being used, if applicable")
	flags.StringVar(&cloudConfig.ProjectID, "gce-project", "", "The GCE project being used, if applicable")
	flags.StringVar(&cloudConfig.Zone, "gce-zone", "", "GCE zone being used, if applicable")
	flags.Var(cliflag.NewStringSlice(&cloudConfig.Zones), "gce-zones", "The set of zones to use in a multi-zone test instead of querying the cloud provider.")
	flags.StringVar(&cloudConfig.Region, "gce-region", "", "GCE region being used, if applicable")
	flags.BoolVar(&cloudConfig.MultiZone, "gce-multizone", false, "If true, start GCE cloud provider with multizone support.")
	flags.BoolVar(&cloudConfig.MultiMaster, "gce-multimaster", false, "If true, the underlying GCE/GKE cluster is assumed to be multi-master.")
	flags.StringVar(&cloudConfig.Cluster, "gke-cluster", "", "GKE name of cluster being used, if applicable")
	flags.StringVar(&cloudConfig.NodeInstanceGroup, "node-instance-group", "", "Name of the managed instance group for nodes. Valid only for gce, gke or aws. If there is more than one group: comma separated list of groups.")
	flags.StringVar(&cloudConfig.Network, "network", "e2e", "The cloud provider network for this e2e cluster.")
	flags.IntVar(&cloudConfig.NumNodes, "num-nodes", DefaultNumNodes, fmt.Sprintf("Number of nodes in the cluster. If the default value of '%q' is used the number of schedulable nodes is auto-detected.", DefaultNumNodes))
	flags.StringVar(&cloudConfig.ClusterIPRange, "cluster-ip-range", "10.64.0.0/14", "A CIDR notation IP range from which to assign IPs in the cluster.")
	flags.StringVar(&cloudConfig.NodeTag, "node-tag", "", "Network tags used on node instances. Valid only for gce, gke")
	flags.StringVar(&cloudConfig.MasterTag, "master-tag", "", "Network tags used on master instances. Valid only for gce, gke")

	flags.StringVar(&cloudConfig.ClusterTag, "cluster-tag", "", "Tag used to identify resources.  Only required if provider is aws.")
	flags.StringVar(&cloudConfig.ConfigFile, "cloud-config-file", "", "Cloud config file.  Only required if provider is azure or vsphere.")
	flags.IntVar(&TestContext.MinStartupPods, "minStartupPods", 0, "The number of pods which we need to see in 'Running' state with a 'Ready' condition of true, before we try running tests. This is useful in any cluster which needs some base pod-based services running before it can be used. If set to -1, no pods are checked and tests run straight away.")
	flags.DurationVar(&TestContext.timeouts.SystemPodsStartup, "system-pods-startup-timeout", TestContext.timeouts.SystemPodsStartup, "Timeout for waiting for all system pods to be running before starting tests.")
	flags.DurationVar(&TestContext.timeouts.NodeSchedulable, "node-schedulable-timeout", TestContext.timeouts.NodeSchedulable, "Timeout for waiting for all nodes to be schedulable.")
	flags.DurationVar(&TestContext.timeouts.SystemDaemonsetStartup, "system-daemonsets-startup-timeout", TestContext.timeouts.SystemDaemonsetStartup, "Timeout for waiting for all system daemonsets to be ready.")
	flags.StringVar(&TestContext.EtcdUpgradeStorage, "etcd-upgrade-storage", "", "The storage version to upgrade to (either 'etcdv2' or 'etcdv3') if doing an etcd upgrade test.")
	flags.StringVar(&TestContext.EtcdUpgradeVersion, "etcd-upgrade-version", "", "The etcd binary version to upgrade to (e.g., '3.0.14', '2.3.7') if doing an etcd upgrade test.")
	flags.StringVar(&TestContext.GCEUpgradeScript, "gce-upgrade-script", "", "Script to use to upgrade a GCE cluster.")
	flags.BoolVar(&TestContext.CleanStart, "clean-start", false, "If true, purge all namespaces except default and system before running tests. This serves to Cleanup test namespaces from failed/interrupted e2e runs in a long-lived cluster.")

	nodeKiller := &TestContext.NodeKiller
	flags.BoolVar(&nodeKiller.Enabled, "node-killer", false, "Whether NodeKiller should kill any nodes.")
	flags.Float64Var(&nodeKiller.FailureRatio, "node-killer-failure-ratio", 0.01, "Percentage of nodes to be killed")
	flags.DurationVar(&nodeKiller.Interval, "node-killer-interval", 1*time.Minute, "Time between node failures.")
	flags.Float64Var(&nodeKiller.JitterFactor, "node-killer-jitter-factor", 60, "Factor used to jitter node failures.")
	flags.DurationVar(&nodeKiller.SimulatedDowntime, "node-killer-simulated-downtime", 10*time.Minute, "A delay between node death and recreation")
}

// generateSecureToken returns a string of length tokenLen, consisting
// of random bytes encoded as base64 for use as a Bearer Token during
// communication with an APIServer
func generateSecureToken(tokenLen int) (string, error) {
	// Number of bytes to be tokenLen when base64 encoded.
	tokenSize := math.Ceil(float64(tokenLen) * 6 / 8)
	rawToken := make([]byte, int(tokenSize))
	if _, err := rand.Read(rawToken); err != nil {
		return "", err
	}
	encoded := base64.RawURLEncoding.EncodeToString(rawToken)
	token := encoded[:tokenLen]
	return token, nil
}

// AfterReadingAllFlags makes changes to the context after all flags
// have been read and prepares the process for a test run.
func AfterReadingAllFlags(t *TestContextType) {
	// Reconfigure klog so that output goes to the GinkgoWriter instead
	// of stderr. The advantage is that it then gets interleaved properly
	// with output that goes to GinkgoWriter (By, Logf).

	// These flags are not exposed via the normal command line flag set,
	// therefore we have to use our own private one here.
	if t.KubeTestRepoList != "" {
		image.Init(t.KubeTestRepoList)
	}

	if t.ListImages {
		for _, v := range image.GetImageConfigs() {
			fmt.Println(v.GetE2EImage())
		}
		Exit(0)
	}

	// Reconfigure gomega defaults. The poll interval should be suitable
	// for most tests. The timeouts are more subjective and tests may want
	// to override them, but these defaults are still better for E2E than the
	// ones from Gomega (1s timeout, 10ms interval).
	gomega.SetDefaultEventuallyPollingInterval(t.timeouts.Poll)
	gomega.SetDefaultConsistentlyPollingInterval(t.timeouts.Poll)
	gomega.SetDefaultEventuallyTimeout(t.timeouts.PodStart)
	gomega.SetDefaultConsistentlyDuration(t.timeouts.PodStartShort)
	gomega.EnforceDefaultTimeoutsWhenUsingContexts()

	// ginkgo.PreviewSpecs will expand all nodes and thus may find new bugs.
	report := ginkgo.PreviewSpecs("Kubernetes e2e test statistics")
	validateSpecs(report.SpecReports)
	if err := FormatBugs(); CheckForBugs && err != nil {
		// Refuse to do anything if the E2E suite is buggy.
		fmt.Fprint(Output, "ERROR: E2E suite initialization was faulty, these errors must be fixed:")
		fmt.Fprint(Output, "\n"+err.Error())
		Exit(1)
	}
	if t.listLabels || t.listTests {
		listTestInformation(report)
		Exit(0)
	}

	// Only set a default host if one won't be supplied via kubeconfig
	if len(t.Host) == 0 && len(t.KubeConfig) == 0 {
		// Check if we can use the in-cluster config
		if clusterConfig, err := restclient.InClusterConfig(); err == nil {
			if tempFile, err := os.CreateTemp(os.TempDir(), "kubeconfig-"); err == nil {
				kubeConfig := kubeconfig.CreateKubeConfig(clusterConfig)
				clientcmd.WriteToFile(*kubeConfig, tempFile.Name())
				t.KubeConfig = tempFile.Name()
				klog.V(4).Infof("Using a temporary kubeconfig file from in-cluster config : %s", tempFile.Name())
			}
		}
		if len(t.KubeConfig) == 0 {
			klog.Warningf("Unable to find in-cluster config, using default host : %s", defaultHost)
			t.Host = defaultHost
		}
	}
	if len(t.BearerToken) == 0 {
		var err error
		t.BearerToken, err = generateSecureToken(16)
		ExpectNoError(err, "Failed to generate bearer token")
	}

	// Allow 1% of nodes to be unready (statistically) - relevant for large clusters.
	if t.AllowedNotReadyNodes == 0 {
		t.AllowedNotReadyNodes = t.CloudConfig.NumNodes / 100
	}

	klog.V(4).Infof("Tolerating taints %q when considering if nodes are ready", TestContext.NonblockingTaints)

	// Make sure that all test runs have a valid TestContext.CloudConfig.Provider.
	// TODO: whether and how long this code is needed is getting discussed
	// in https://github.com/kubernetes/kubernetes/issues/70194.
	if TestContext.Provider == "" {
		// Some users of the e2e.test binary pass --provider=.
		// We need to support that, changing it would break those usages.
		Logf("The --provider flag is not set. Continuing as if --provider=skeleton had been used.")
		TestContext.Provider = "skeleton"
	}

	var err error
	TestContext.CloudConfig.Provider, err = SetupProviderConfig(TestContext.Provider)
	if err != nil {
		if os.IsNotExist(errors.Unwrap(err)) {
			// Provide a more helpful error message when the provider is unknown.
			var providers []string
			for _, name := range GetProviders() {
				// The empty string is accepted, but looks odd in the output below unless we quote it.
				if name == "" {
					name = `""`
				}
				providers = append(providers, name)
			}
			sort.Strings(providers)
			klog.Errorf("Unknown provider %q. The following providers are known: %v", TestContext.Provider, strings.Join(providers, " "))
		} else {
			klog.Errorf("Failed to setup provider config for %q: %v", TestContext.Provider, err)
		}
		Exit(1)
	}

	if TestContext.ReportDir != "" {
		// Create the directory before running the suite. If
		// --report-dir is not unusable, we should report
		// that as soon as possible. This will be done by each worker
		// in parallel, so we will get "exists" error in most of them.
		if err := os.MkdirAll(TestContext.ReportDir, 0777); err != nil && !os.IsExist(err) {
			klog.Errorf("Create report dir: %v", err)
			Exit(1)
		}
		ginkgoDir := path.Join(TestContext.ReportDir, "ginkgo")
		if TestContext.ReportCompleteGinkgo || TestContext.ReportCompleteJUnit {
			if err := os.MkdirAll(ginkgoDir, 0777); err != nil && !os.IsExist(err) {
				klog.Errorf("Create <report-dir>/ginkgo: %v", err)
				Exit(1)
			}
		}

		if TestContext.ReportCompleteGinkgo {
			ginkgo.ReportAfterSuite("Ginkgo JSON report", func(report ginkgo.Report) {
				ExpectNoError(reporters.GenerateJSONReport(report, path.Join(ginkgoDir, "report.json")))
			})
			ginkgo.ReportAfterSuite("JUnit XML report", func(report ginkgo.Report) {
				ExpectNoError(reporters.GenerateJUnitReport(report, path.Join(ginkgoDir, "report.xml")))
			})
		}

		ginkgo.ReportAfterSuite("Kubernetes e2e JUnit report", func(report ginkgo.Report) {
			// With Ginkgo v1, we used to write one file per
			// parallel node. Now Ginkgo v2 automatically merges
			// all results into a report for us. The 01 suffix is
			// kept in case that users expect files to be called
			// "junit_<prefix><number>.xml".
			junitReport := path.Join(TestContext.ReportDir, "junit_"+TestContext.ReportPrefix+"01.xml")

			// writeJUnitReport generates a JUnit file in the e2e
			// report directory that is shorter than the one
			// normally written by `ginkgo --junit-report`. This is
			// needed because the full report can become too large
			// for tools like Spyglass
			// (https://github.com/kubernetes/kubernetes/issues/111510).
			ExpectNoError(junit.WriteJUnitReport(report, junitReport))
		})
	}
}

func listTestInformation(report ginkgo.Report) {
	indent := strings.Repeat(" ", 4)

	if TestContext.listLabels {
		labels := sets.New[string]()
		for _, spec := range report.SpecReports {
			if spec.LeafNodeType == types.NodeTypeIt {
				labels.Insert(spec.Labels()...)
			}
		}
		fmt.Fprintf(Output, "The following labels can be used with 'ginkgo run --label-filter':\n%s%s\n\n", indent, strings.Join(sets.List(labels), "\n"+indent))
	}
	if TestContext.listTests {
		leafs := make([][]string, 0, len(report.SpecReports))
		wd, _ := os.Getwd()
		for _, spec := range report.SpecReports {
			if spec.LeafNodeType == types.NodeTypeIt {
				leafs = append(leafs, []string{fmt.Sprintf("%s:%d: ", relativePath(wd, spec.LeafNodeLocation.FileName), spec.LeafNodeLocation.LineNumber), spec.FullText()})
			}
		}
		// Sort by test name, not the source code location, because the test
		// name is more stable across code refactoring.
		sort.Slice(leafs, func(i, j int) bool {
			return leafs[i][1] < leafs[j][1]
		})
		fmt.Fprint(Output, "The following spec names can be used with 'ginkgo run --focus/skip':\n")
		for _, leaf := range leafs {
			fmt.Fprintf(Output, "%s%s%s\n", indent, leaf[0], leaf[1])
		}
		fmt.Fprint(Output, "\n")
	}
}

func relativePath(wd, path string) string {
	if wd == "" {
		return path
	}
	relpath, err := filepath.Rel(wd, path)
	if err != nil {
		return path
	}
	return relpath
}
