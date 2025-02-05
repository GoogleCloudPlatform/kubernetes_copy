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

// Package framework contains provider-independent helper code for
// building and running E2E tests with Ginkgo. The actual Ginkgo test
// suites gets assembled by combining this framework, the optional
// provider support code and specific tests via a separate .go file
// like Kubernetes' test/e2e.go.
package framework

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"path"
	"reflect"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/runtime"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	v1svc "k8s.io/client-go/applyconfigurations/core/v1"
	"k8s.io/client-go/discovery"
	cacheddiscovery "k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	scaleclient "k8s.io/client-go/scale"
	admissionapi "k8s.io/pod-security-admission/api"

	"github.com/onsi/ginkgo/v2"
)

const (
	// DefaultNamespaceDeletionTimeout is timeout duration for waiting for a namespace deletion.
	DefaultNamespaceDeletionTimeout = 5 * time.Minute
	defaultServiceAccountName       = "default"
	DefaultObservationTimeout       = 4 * time.Minute
)

var (
	// NewFrameworkExtensions lists functions that get called by
	// NewFramework after constructing a new framework and after
	// calling ginkgo.BeforeEach for the framework.
	//
	// This can be used by extensions of the core framework to modify
	// settings in the framework instance or to add additional callbacks
	// with ginkgo.BeforeEach/AfterEach/DeferCleanup.
	//
	// When a test runs, functions will be invoked in this order:
	// - BeforeEaches defined by tests before f.NewDefaultFramework
	//   in the order in which they were defined (first-in-first-out)
	// - f.BeforeEach
	// - BeforeEaches defined by tests after f.NewDefaultFramework
	// - It callback
	// - all AfterEaches in the order in which they were defined
	// - all DeferCleanups with the order reversed (first-in-last-out)
	// - f.AfterEach
	//
	// Because a test might skip test execution in a BeforeEach that runs
	// before f.BeforeEach, AfterEach callbacks that depend on the
	// framework instance must check whether it was initialized. They can
	// do that by checking f.ClientSet for nil. DeferCleanup callbacks
	// don't need to do this because they get defined when the test
	// runs.
	NewFrameworkExtensions []func(f *Framework)
)

// Framework supports common operations used by e2e tests; it will keep a client & a namespace for you.
// Eventual goal is to merge this with integration test framework.
//
// You can configure the pod security level for your test by setting the `NamespacePodSecurityLevel`
// which will set all three of pod security admission enforce, warn and audit labels on the namespace.
// The default pod security profile is "restricted".
// Each of the labels can be overridden by using more specific NamespacePodSecurity* attributes of this
// struct.
type Framework struct {
	BaseName string

	// Set together with creating the ClientSet and the namespace.
	// Guaranteed to be unique in the cluster even when running the same
	// test multiple times in parallel.
	UniqueName string

	clientConfig                     *rest.Config
	ClientSet                        clientset.Interface
	KubemarkExternalClusterClientSet clientset.Interface

	DynamicClient dynamic.Interface

	ScalesGetter scaleclient.ScalesGetter

	SkipNamespaceCreation            bool            // Whether to skip creating a namespace
	SkipSecretCreation               bool            // Whether to skip creating secret for a test
	Namespace                        *v1.Namespace   // Every test has at least one namespace unless creation is skipped
	namespacesToDelete               []*v1.Namespace // Some tests have more than one.
	NamespaceDeletionTimeout         time.Duration
	NamespacePodSecurityEnforceLevel admissionapi.Level // The pod security enforcement level for namespaces to be applied.
	NamespacePodSecurityWarnLevel    admissionapi.Level // The pod security warn (client logging) level for namespaces to be applied.
	NamespacePodSecurityAuditLevel   admissionapi.Level // The pod security audit (server logging) level for namespaces to be applied.
	NamespacePodSecurityLevel        admissionapi.Level // The pod security level to be used for all of enforcement, warn and audit. Can be rewritten by more specific configuration attributes.

	// Flaky operation failures in an e2e test can be captured through this.
	flakeReport *FlakeReport

	// configuration for framework's client
	Options Options

	// Place where various additional data is stored during test run to be printed to ReportDir,
	// or stdout if ReportDir is not set once test ends.
	TestSummaries []TestDataSummary

	// Timeouts contains the custom timeouts used during the test execution.
	Timeouts *TimeoutContext

	// DumpAllNamespaceInfo is invoked by the framework to record
	// information about a namespace after a test failure.
	DumpAllNamespaceInfo DumpAllNamespaceInfoAction
}

// DumpAllNamespaceInfoAction is called after each failed test for namespaces
// created for the test.
type DumpAllNamespaceInfoAction func(ctx context.Context, f *Framework, namespace string)

// TestDataSummary is an interface for managing test data.
type TestDataSummary interface {
	SummaryKind() string
	PrintHumanReadable() string
	PrintJSON() string
}

// Options is a struct for managing test framework options.
type Options struct {
	ClientQPS    float32
	ClientBurst  int
	GroupVersion *schema.GroupVersion
}

// NewFrameworkWithCustomTimeouts makes a framework with custom timeouts.
// For timeout values that are zero the normal default value continues to
// be used.
func NewFrameworkWithCustomTimeouts(baseName string, timeouts *TimeoutContext) *Framework {
	f := NewDefaultFramework(baseName)
	in := reflect.ValueOf(timeouts).Elem()
	out := reflect.ValueOf(f.Timeouts).Elem()
	for i := 0; i < in.NumField(); i++ {
		value := in.Field(i)
		if !value.IsZero() {
			out.Field(i).Set(value)
		}
	}
	return f
}

// NewDefaultFramework makes a new framework and sets up a BeforeEach which
// initializes the framework instance. It cleans up with a DeferCleanup,
// which runs last, so a AfterEach in the test still has a valid framework
// instance.
func NewDefaultFramework(baseName string) *Framework {
	options := Options{
		ClientQPS:   20,
		ClientBurst: 50,
	}
	return NewFramework(baseName, options, nil)
}

// NewFramework creates a test framework.
func NewFramework(baseName string, options Options, client clientset.Interface) *Framework {
	f := &Framework{
		BaseName:  baseName,
		Options:   options,
		ClientSet: client,
		Timeouts:  NewTimeoutContext(),
	}

	// The order is important here: if the extension calls ginkgo.BeforeEach
	// itself, then it can be sure that f.BeforeEach already ran when its
	// own callback gets invoked.
	ginkgo.BeforeEach(f.BeforeEach, AnnotatedLocation("set up framework"))
	for _, extension := range NewFrameworkExtensions {
		extension(f)
	}

	return f
}

// BeforeEach gets a client and makes a namespace.
func (f *Framework) BeforeEach(ctx context.Context) {
	// DeferCleanup, in contrast to AfterEach, triggers execution in
	// first-in-last-out order. This ensures that the framework instance
	// remains valid as long as possible.
	//
	// In addition, AfterEach will not be called if a test never gets here.
	ginkgo.DeferCleanup(f.AfterEach, AnnotatedLocation("tear down framework"))

	// Registered later and thus runs before deleting namespaces.
	ginkgo.DeferCleanup(f.dumpNamespaceInfo, AnnotatedLocation("dump namespaces"))

	ginkgo.By("Creating a kubernetes client")
	config, err := LoadConfig()
	ExpectNoError(err)

	config.QPS = f.Options.ClientQPS
	config.Burst = f.Options.ClientBurst
	if f.Options.GroupVersion != nil {
		config.GroupVersion = f.Options.GroupVersion
	}
	if TestContext.KubeAPIContentType != "" {
		config.ContentType = TestContext.KubeAPIContentType
	}
	f.clientConfig = rest.CopyConfig(config)
	f.ClientSet, err = clientset.NewForConfig(config)
	ExpectNoError(err)
	f.DynamicClient, err = dynamic.NewForConfig(config)
	ExpectNoError(err)

	// create scales getter, set GroupVersion and NegotiatedSerializer to default values
	// as they are required when creating a REST client.
	if config.GroupVersion == nil {
		config.GroupVersion = &schema.GroupVersion{}
	}
	if config.NegotiatedSerializer == nil {
		config.NegotiatedSerializer = scheme.Codecs
	}
	restClient, err := rest.RESTClientFor(config)
	ExpectNoError(err)
	discoClient, err := discovery.NewDiscoveryClientForConfig(config)
	ExpectNoError(err)
	cachedDiscoClient := cacheddiscovery.NewMemCacheClient(discoClient)
	restMapper := restmapper.NewDeferredDiscoveryRESTMapper(cachedDiscoClient)
	restMapper.Reset()
	resolver := scaleclient.NewDiscoveryScaleKindResolver(cachedDiscoClient)
	f.ScalesGetter = scaleclient.New(restClient, restMapper, dynamic.LegacyAPIPathResolverFunc, resolver)

	TestContext.CloudConfig.Provider.FrameworkBeforeEach(f)

	if !f.SkipNamespaceCreation {
		ginkgo.By(fmt.Sprintf("Building a namespace api object, basename %s", f.BaseName))
		namespace, err := f.CreateNamespace(ctx, f.BaseName, map[string]string{
			"e2e-framework": f.BaseName,
		})
		ExpectNoError(err)

		f.Namespace = namespace

		if TestContext.VerifyServiceAccount {
			ginkgo.By("Waiting for a default service account to be provisioned in namespace")
			err = WaitForDefaultServiceAccountInNamespace(ctx, f.ClientSet, namespace.Name)
			ExpectNoError(err)
			ginkgo.By("Waiting for kube-root-ca.crt to be provisioned in namespace")
			err = WaitForKubeRootCAInNamespace(ctx, f.ClientSet, namespace.Name)
			ExpectNoError(err)
		} else {
			Logf("Skipping waiting for service account")
		}

		f.UniqueName = f.Namespace.GetName()
	} else {
		// not guaranteed to be unique, but very likely
		f.UniqueName = fmt.Sprintf("%s-%08x", f.BaseName, rand.Int31())
	}

	f.flakeReport = NewFlakeReport()
}

func (f *Framework) dumpNamespaceInfo(ctx context.Context) {
	if !ginkgo.CurrentSpecReport().Failed() {
		return
	}
	if !TestContext.DumpLogsOnFailure {
		return
	}
	if f.DumpAllNamespaceInfo == nil {
		return
	}
	ginkgo.By("dump namespace information after failure", func() {
		if !f.SkipNamespaceCreation {
			for _, ns := range f.namespacesToDelete {
				f.DumpAllNamespaceInfo(ctx, f, ns.Name)
			}
		}
	})
}

// printSummaries prints summaries of tests.
func printSummaries(summaries []TestDataSummary, testBaseName string) {
	now := time.Now()
	for i := range summaries {
		Logf("Printing summary: %v", summaries[i].SummaryKind())
		switch TestContext.OutputPrintType {
		case "hr":
			if TestContext.ReportDir == "" {
				Logf("%s", summaries[i].PrintHumanReadable())
			} else {
				// TODO: learn to extract test name and append it to the kind instead of timestamp.
				filePath := path.Join(TestContext.ReportDir, summaries[i].SummaryKind()+"_"+testBaseName+"_"+now.Format(time.RFC3339)+".txt")
				if err := os.WriteFile(filePath, []byte(summaries[i].PrintHumanReadable()), 0644); err != nil {
					Logf("Failed to write file %v with test performance data: %v", filePath, err)
				}
			}
		case "json":
			fallthrough
		default:
			if TestContext.OutputPrintType != "json" {
				Logf("Unknown output type: %v. Printing JSON", TestContext.OutputPrintType)
			}
			if TestContext.ReportDir == "" {
				Logf("%v JSON\n%v", summaries[i].SummaryKind(), summaries[i].PrintJSON())
				Logf("Finished")
			} else {
				// TODO: learn to extract test name and append it to the kind instead of timestamp.
				filePath := path.Join(TestContext.ReportDir, summaries[i].SummaryKind()+"_"+testBaseName+"_"+now.Format(time.RFC3339)+".json")
				Logf("Writing to %s", filePath)
				if err := os.WriteFile(filePath, []byte(summaries[i].PrintJSON()), 0644); err != nil {
					Logf("Failed to write file %v with test performance data: %v", filePath, err)
				}
			}
		}
	}
}

// AfterEach deletes the namespace, after reading its events.
func (f *Framework) AfterEach(ctx context.Context) {
	// This should not happen. Given ClientSet is a public field a test must have updated it!
	// Error out early before any API calls during cleanup.
	if f.ClientSet == nil {
		Failf("The framework ClientSet must not be nil at this point")
	}

	// DeleteNamespace at the very end in defer, to avoid any
	// expectation failures preventing deleting the namespace.
	defer func() {
		nsDeletionErrors := map[string]error{}
		// Whether to delete namespace is determined by 3 factors: delete-namespace flag, delete-namespace-on-failure flag and the test result
		// if delete-namespace set to false, namespace will always be preserved.
		// if delete-namespace is true and delete-namespace-on-failure is false, namespace will be preserved if test failed.
		if TestContext.DeleteNamespace && (TestContext.DeleteNamespaceOnFailure || !ginkgo.CurrentSpecReport().Failed()) {
			for _, ns := range f.namespacesToDelete {
				ginkgo.By(fmt.Sprintf("Destroying namespace %q for this suite.", ns.Name))
				if err := f.ClientSet.CoreV1().Namespaces().Delete(ctx, ns.Name, metav1.DeleteOptions{}); err != nil {
					if !apierrors.IsNotFound(err) {
						nsDeletionErrors[ns.Name] = err

						// Dump namespace if we are unable to delete the namespace and the dump was not already performed.
						if !ginkgo.CurrentSpecReport().Failed() && TestContext.DumpLogsOnFailure && f.DumpAllNamespaceInfo != nil {
							f.DumpAllNamespaceInfo(ctx, f, ns.Name)
						}
					} else {
						Logf("Namespace %v was already deleted", ns.Name)
					}
				}
			}
		} else {
			if !TestContext.DeleteNamespace {
				Logf("Found DeleteNamespace=false, skipping namespace deletion!")
			} else {
				Logf("Found DeleteNamespaceOnFailure=false and current test failed, skipping namespace deletion!")
			}
		}

		// Unsetting this is relevant for a following test that uses
		// the same instance because it might not reach f.BeforeEach
		// when some other BeforeEach skips the test first.
		f.Namespace = nil
		f.clientConfig = nil
		f.ClientSet = nil
		f.namespacesToDelete = nil

		// if we had errors deleting, report them now.
		if len(nsDeletionErrors) != 0 {
			messages := []string{}
			for namespaceKey, namespaceErr := range nsDeletionErrors {
				messages = append(messages, fmt.Sprintf("Couldn't delete ns: %q: %s (%#v)", namespaceKey, namespaceErr, namespaceErr))
			}
			Fail(strings.Join(messages, ","))
		}
	}()

	TestContext.CloudConfig.Provider.FrameworkAfterEach(f)

	// Report any flakes that were observed in the e2e test and reset.
	if f.flakeReport != nil && f.flakeReport.GetFlakeCount() > 0 {
		f.TestSummaries = append(f.TestSummaries, f.flakeReport)
		f.flakeReport = nil
	}

	printSummaries(f.TestSummaries, f.BaseName)
}

// DeleteNamespace can be used to delete a namespace. Additionally it can be used to
// dump namespace information so as it can be used as an alternative of framework
// deleting the namespace towards the end.
func (f *Framework) DeleteNamespace(ctx context.Context, name string) {
	defer func() {
		err := f.ClientSet.CoreV1().Namespaces().Delete(ctx, name, metav1.DeleteOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			Logf("error deleting namespace %s: %v", name, err)
			return
		}
		err = WaitForNamespacesDeleted(ctx, f.ClientSet, []string{name}, DefaultNamespaceDeletionTimeout)
		if err != nil {
			Logf("error deleting namespace %s: %v", name, err)
			return
		}
		// remove deleted namespace from namespacesToDelete map
		for i, ns := range f.namespacesToDelete {
			if ns == nil {
				continue
			}
			if ns.Name == name {
				f.namespacesToDelete = append(f.namespacesToDelete[:i], f.namespacesToDelete[i+1:]...)
			}
		}
	}()
	// if current test failed then we should dump namespace information
	if !f.SkipNamespaceCreation && ginkgo.CurrentSpecReport().Failed() && TestContext.DumpLogsOnFailure && f.DumpAllNamespaceInfo != nil {
		f.DumpAllNamespaceInfo(ctx, f, name)
	}

}

// CreateNamespace creates a namespace for e2e testing.
func (f *Framework) CreateNamespace(ctx context.Context, baseName string, labels map[string]string) (*v1.Namespace, error) {
	createTestingNS := TestContext.CreateTestingNS
	if createTestingNS == nil {
		createTestingNS = CreateTestingNS
	}

	if labels == nil {
		labels = make(map[string]string)
	} else {
		labelsCopy := make(map[string]string)
		for k, v := range labels {
			labelsCopy[k] = v
		}
		labels = labelsCopy
	}

	labels[admissionapi.EnforceLevelLabel] = firstNonEmptyPSaLevelOrRestricted(f.NamespacePodSecurityEnforceLevel, f.NamespacePodSecurityLevel)
	labels[admissionapi.WarnLevelLabel] = firstNonEmptyPSaLevelOrRestricted(f.NamespacePodSecurityWarnLevel, f.NamespacePodSecurityLevel)
	labels[admissionapi.AuditLevelLabel] = firstNonEmptyPSaLevelOrRestricted(f.NamespacePodSecurityAuditLevel, f.NamespacePodSecurityLevel)

	ns, err := createTestingNS(ctx, baseName, f.ClientSet, labels)
	// check ns instead of err to see if it's nil as we may
	// fail to create serviceAccount in it.
	f.AddNamespacesToDelete(ns)

	if TestContext.E2EDockerConfigFile != "" && !f.SkipSecretCreation {
		// With the Secret created, the default service account (in the new namespace)
		// is patched with the secret and can then be referenced by all the pods spawned by E2E process, and repository authentication should be successful.
		secret, err := f.createSecretFromDockerConfig(ctx, ns.Name)
		if err != nil {
			return ns, fmt.Errorf("failed to create secret from docker config file: %v", err)
		}

		serviceAccountClient := f.ClientSet.CoreV1().ServiceAccounts(ns.Name)
		serviceAccountConfig := v1svc.ServiceAccount(defaultServiceAccountName, ns.Name)
		serviceAccountConfig.ImagePullSecrets = append(serviceAccountConfig.ImagePullSecrets, v1svc.LocalObjectReferenceApplyConfiguration{Name: &secret.Name})

		svc, err := serviceAccountClient.Apply(ctx, serviceAccountConfig, metav1.ApplyOptions{FieldManager: "e2e-framework"})
		if err != nil {
			return ns, fmt.Errorf("failed to patch imagePullSecret [%s] to service account [%s]: %v", secret.Name, svc.Name, err)
		}

	}

	return ns, err
}

func firstNonEmptyPSaLevelOrRestricted(levelConfig ...admissionapi.Level) string {
	for _, l := range levelConfig {
		if len(l) > 0 {
			return string(l)
		}
	}
	return string(admissionapi.LevelRestricted)
}

// createSecretFromDockerConfig creates a secret using the private image registry credentials.
// The credentials are provided by --e2e-docker-config-file flag.
func (f *Framework) createSecretFromDockerConfig(ctx context.Context, namespace string) (*v1.Secret, error) {
	contents, err := os.ReadFile(TestContext.E2EDockerConfigFile)
	if err != nil {
		return nil, fmt.Errorf("error reading docker config file: %v", err)
	}

	secretObject := &v1.Secret{
		Data: map[string][]byte{v1.DockerConfigJsonKey: contents},
		Type: v1.SecretTypeDockerConfigJson,
	}
	secretObject.GenerateName = "registry-cred"
	Logf("create image pull secret %s", secretObject.Name)

	secret, err := f.ClientSet.CoreV1().Secrets(namespace).Create(ctx, secretObject, metav1.CreateOptions{})

	return secret, err
}

// RecordFlakeIfError records flakeness info if error happens.
// NOTE: This function is not used at any places yet, but we are in progress for https://github.com/kubernetes/kubernetes/issues/66239 which requires this. Please don't remove this.
func (f *Framework) RecordFlakeIfError(err error, optionalDescription ...interface{}) {
	f.flakeReport.RecordFlakeIfError(err, optionalDescription...)
}

// AddNamespacesToDelete adds one or more namespaces to be deleted when the test
// completes.
func (f *Framework) AddNamespacesToDelete(namespaces ...*v1.Namespace) {
	for _, ns := range namespaces {
		if ns == nil {
			continue
		}
		f.namespacesToDelete = append(f.namespacesToDelete, ns)

	}
}

// ClientConfig an externally accessible method for reading the kube client config.
func (f *Framework) ClientConfig() *rest.Config {
	ret := rest.CopyConfig(f.clientConfig)
	// json is least common denominator
	ret.ContentType = runtime.ContentTypeJSON
	ret.AcceptContentTypes = runtime.ContentTypeJSON
	return ret
}

// KubeUser is a struct for managing kubernetes user info.
type KubeUser struct {
	Name string `yaml:"name"`
	User struct {
		Username string `yaml:"username"`
		Password string `yaml:"password" datapolicy:"password"`
		Token    string `yaml:"token" datapolicy:"token"`
	} `yaml:"user"`
}

// KubeCluster is a struct for managing kubernetes cluster info.
type KubeCluster struct {
	Name    string `yaml:"name"`
	Cluster struct {
		CertificateAuthorityData string `yaml:"certificate-authority-data"`
		Server                   string `yaml:"server"`
	} `yaml:"cluster"`
}

// KubeConfig is a struct for managing kubernetes config.
type KubeConfig struct {
	Contexts []struct {
		Name    string `yaml:"name"`
		Context struct {
			Cluster string `yaml:"cluster"`
			User    string
		} `yaml:"context"`
	} `yaml:"contexts"`

	Clusters []KubeCluster `yaml:"clusters"`

	Users []KubeUser `yaml:"users"`
}

// FindUser returns user info which is the specified user name.
func (kc *KubeConfig) FindUser(name string) *KubeUser {
	for _, user := range kc.Users {
		if user.Name == name {
			return &user
		}
	}
	return nil
}

// FindCluster returns cluster info which is the specified cluster name.
func (kc *KubeConfig) FindCluster(name string) *KubeCluster {
	for _, cluster := range kc.Clusters {
		if cluster.Name == name {
			return &cluster
		}
	}
	return nil
}

// PodStateVerification represents a verification of pod state.
// Any time you have a set of pods that you want to operate against or query,
// this struct can be used to declaratively identify those pods.
type PodStateVerification struct {
	// Optional: only pods that have k=v labels will pass this filter.
	Selectors map[string]string

	// Required: The phases which are valid for your pod.
	ValidPhases []v1.PodPhase

	// Optional: only pods passing this function will pass the filter
	// Verify a pod.
	// As an optimization, in addition to specifying filter (boolean),
	// this function allows specifying an error as well.
	// The error indicates that the polling of the pod spectrum should stop.
	Verify func(v1.Pod) (bool, error)

	// Optional: only pods with this name will pass the filter.
	PodName string
}

// ClusterVerification is a struct for a verification of cluster state.
type ClusterVerification struct {
	client    clientset.Interface
	namespace *v1.Namespace // pointer rather than string, since ns isn't created until before each.
	podState  PodStateVerification
}

// NewClusterVerification creates a new cluster verification.
func (f *Framework) NewClusterVerification(namespace *v1.Namespace, filter PodStateVerification) *ClusterVerification {
	return &ClusterVerification{
		f.ClientSet,
		namespace,
		filter,
	}
}

func passesPodNameFilter(pod v1.Pod, name string) bool {
	return name == "" || strings.Contains(pod.Name, name)
}

func passesVerifyFilter(pod v1.Pod, verify func(p v1.Pod) (bool, error)) (bool, error) {
	if verify == nil {
		return true, nil
	}

	verified, err := verify(pod)
	// If an error is returned, by definition, pod verification fails
	if err != nil {
		return false, err
	}
	return verified, nil
}

func passesPhasesFilter(pod v1.Pod, validPhases []v1.PodPhase) bool {
	passesPhaseFilter := false
	for _, phase := range validPhases {
		if pod.Status.Phase == phase {
			passesPhaseFilter = true
		}
	}
	return passesPhaseFilter
}

// filterLabels returns a list of pods which have labels.
func filterLabels(ctx context.Context, selectors map[string]string, cli clientset.Interface, ns string) (*v1.PodList, error) {
	var err error
	var selector labels.Selector
	var pl *v1.PodList
	// List pods based on selectors.  This might be a tiny optimization rather then filtering
	// everything manually.
	if len(selectors) > 0 {
		selector = labels.SelectorFromSet(labels.Set(selectors))
		options := metav1.ListOptions{LabelSelector: selector.String()}
		pl, err = cli.CoreV1().Pods(ns).List(ctx, options)
	} else {
		pl, err = cli.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
	}
	return pl, err
}

// filter filters pods which pass a filter.  It can be used to compose
// the more useful abstractions like ForEach, WaitFor, and so on, which
// can be used directly by tests.
func (p *PodStateVerification) filter(ctx context.Context, c clientset.Interface, namespace *v1.Namespace) ([]v1.Pod, error) {
	if len(p.ValidPhases) == 0 || namespace == nil {
		panic(fmt.Errorf("Need to specify a valid pod phases (%v) and namespace (%v). ", p.ValidPhases, namespace))
	}

	ns := namespace.Name
	pl, err := filterLabels(ctx, p.Selectors, c, ns) // Build an v1.PodList to operate against.
	Logf("Selector matched %v pods for %v", len(pl.Items), p.Selectors)
	if len(pl.Items) == 0 || err != nil {
		return pl.Items, err
	}

	unfilteredPods := pl.Items
	filteredPods := []v1.Pod{}
ReturnPodsSoFar:
	// Next: Pod must match at least one of the states that the user specified
	for _, pod := range unfilteredPods {
		if !(passesPhasesFilter(pod, p.ValidPhases) && passesPodNameFilter(pod, p.PodName)) {
			continue
		}
		passesVerify, err := passesVerifyFilter(pod, p.Verify)
		if err != nil {
			Logf("Error detected on %v : %v !", pod.Name, err)
			break ReturnPodsSoFar
		}
		if passesVerify {
			filteredPods = append(filteredPods, pod)
		}
	}
	return filteredPods, err
}

// WaitFor waits for some minimum number of pods to be verified, according to the PodStateVerification
// definition.
func (cl *ClusterVerification) WaitFor(ctx context.Context, atLeast int, timeout time.Duration) ([]v1.Pod, error) {
	pods := []v1.Pod{}
	var returnedErr error

	err := wait.PollUntilContextTimeout(ctx, 1*time.Second, timeout, false, func(ctx context.Context) (bool, error) {
		pods, returnedErr = cl.podState.filter(ctx, cl.client, cl.namespace)

		// Failure
		if returnedErr != nil {
			Logf("Cutting polling short: We got an error from the pod filtering layer.")
			// stop polling if the pod filtering returns an error.  that should never happen.
			// it indicates, for example, that the client is broken or something non-pod related.
			return false, returnedErr
		}
		Logf("Found %v / %v", len(pods), atLeast)

		// Success
		if len(pods) >= atLeast {
			return true, nil
		}
		// Keep trying...
		return false, nil
	})
	Logf("WaitFor completed with timeout %v.  Pods found = %v out of %v", timeout, len(pods), atLeast)
	return pods, err
}

// WaitForOrFail provides a shorthand WaitFor with failure as an option if anything goes wrong.
func (cl *ClusterVerification) WaitForOrFail(ctx context.Context, atLeast int, timeout time.Duration) {
	pods, err := cl.WaitFor(ctx, atLeast, timeout)
	if err != nil || len(pods) < atLeast {
		Failf("Verified %v of %v pods , error : %v", len(pods), atLeast, err)
	}
}

// ForEach runs a function against every verifiable pod.  Be warned that this doesn't wait for "n" pods to verify,
// so it may return very quickly if you have strict pod state requirements.
//
// For example, if you require at least 5 pods to be running before your test will pass,
// its smart to first call "clusterVerification.WaitFor(5)" before you call clusterVerification.ForEach.
func (cl *ClusterVerification) ForEach(ctx context.Context, podFunc func(v1.Pod)) error {
	pods, err := cl.podState.filter(ctx, cl.client, cl.namespace)
	if err == nil {
		if len(pods) == 0 {
			Failf("No pods matched the filter.")
		}
		Logf("ForEach: Found %v pods from the filter.  Now looping through them.", len(pods))
		for _, p := range pods {
			podFunc(p)
		}
	} else {
		Logf("ForEach: Something went wrong when filtering pods to execute against: %v", err)
	}

	return err
}
