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

package admissionwebhook

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
	admissionreviewv1 "k8s.io/api/admission/v1"
	"k8s.io/api/admission/v1beta1"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	admissionregistrationv1beta1 "k8s.io/api/admissionregistration/v1beta1"
	appsv1beta1 "k8s.io/api/apps/v1beta1"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	extensionsv1beta1 "k8s.io/api/extensions/v1beta1"
	policyv1 "k8s.io/api/policy/v1"
	apiextensionsclientset "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/json"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	genericapirequest "k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/features"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/client-go/dynamic"
	clientfeatures "k8s.io/client-go/features"
	clientfeaturestesting "k8s.io/client-go/features/testing"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/retry"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	kubeapiservertesting "k8s.io/kubernetes/cmd/kube-apiserver/app/testing"
	apisv1beta1 "k8s.io/kubernetes/pkg/apis/admissionregistration/v1beta1"
	"k8s.io/kubernetes/test/integration/etcd"
	"k8s.io/kubernetes/test/integration/framework"
)

const (
	testNamespace      = "webhook-integration"
	testClientUsername = "webhook-integration-client"

	mutation   = "mutation"
	validation = "validation"
)

var (
	noSideEffects = admissionregistrationv1.SideEffectClassNone
)

type testContext struct {
	t *testing.T

	admissionHolder *holder

	client    dynamic.Interface
	clientset clientset.Interface
	verb      string
	gvr       schema.GroupVersionResource
	resource  metav1.APIResource
	resources map[schema.GroupVersionResource]metav1.APIResource
}

type testFunc func(*testContext)

var (
	// defaultResourceFuncs holds the default test functions.
	// may be overridden for specific resources by customTestFuncs.
	defaultResourceFuncs = map[string]testFunc{
		"create":           testResourceCreate,
		"update":           testResourceUpdate,
		"patch":            testResourcePatch,
		"delete":           testResourceDelete,
		"deletecollection": testResourceDeletecollection,
	}

	// defaultSubresourceFuncs holds default subresource test functions.
	// may be overridden for specific resources by customTestFuncs.
	defaultSubresourceFuncs = map[string]testFunc{
		"update": testSubresourceUpdate,
		"patch":  testSubresourcePatch,
	}

	// customTestFuncs holds custom test functions by resource and verb.
	customTestFuncs = map[schema.GroupVersionResource]map[string]testFunc{
		gvr("", "v1", "namespaces"): {"delete": testNamespaceDelete},

		gvr("apps", "v1beta1", "deployments/rollback"):       {"create": testDeploymentRollback},
		gvr("extensions", "v1beta1", "deployments/rollback"): {"create": testDeploymentRollback},

		gvr("", "v1", "pods/attach"):      {"create": testPodConnectSubresource},
		gvr("", "v1", "pods/exec"):        {"create": testPodConnectSubresource},
		gvr("", "v1", "pods/portforward"): {"create": testPodConnectSubresource},

		gvr("", "v1", "bindings"):      {"create": testPodBindingEviction},
		gvr("", "v1", "pods/binding"):  {"create": testPodBindingEviction},
		gvr("", "v1", "pods/eviction"): {"create": testPodBindingEviction},

		gvr("", "v1", "nodes/proxy"):    {"*": testSubresourceProxy},
		gvr("", "v1", "pods/proxy"):     {"*": testSubresourceProxy},
		gvr("", "v1", "services/proxy"): {"*": testSubresourceProxy},

		gvr("", "v1", "serviceaccounts/token"): {"create": testTokenCreate},

		gvr("random.numbers.com", "v1", "integers"): {"create": testPruningRandomNumbers},
		gvr("custom.fancy.com", "v2", "pants"):      {"create": testNoPruningCustomFancy},
	}

	// admissionExemptResources lists objects which are exempt from admission validation/mutation,
	// only resources exempted from admission processing by API server should be listed here.
	admissionExemptResources = map[schema.GroupVersionResource]bool{
		gvr("admissionregistration.k8s.io", "v1beta1", "mutatingwebhookconfigurations"):       true,
		gvr("admissionregistration.k8s.io", "v1beta1", "validatingwebhookconfigurations"):     true,
		gvr("admissionregistration.k8s.io", "v1", "mutatingwebhookconfigurations"):            true,
		gvr("admissionregistration.k8s.io", "v1", "validatingwebhookconfigurations"):          true,
		gvr("admissionregistration.k8s.io", "v1alpha1", "validatingadmissionpolicies"):        true,
		gvr("admissionregistration.k8s.io", "v1alpha1", "validatingadmissionpolicies/status"): true,
		gvr("admissionregistration.k8s.io", "v1alpha1", "validatingadmissionpolicybindings"):  true,
		gvr("admissionregistration.k8s.io", "v1beta1", "validatingadmissionpolicies"):         true,
		gvr("admissionregistration.k8s.io", "v1beta1", "validatingadmissionpolicies/status"):  true,
		gvr("admissionregistration.k8s.io", "v1beta1", "validatingadmissionpolicybindings"):   true,
		gvr("admissionregistration.k8s.io", "v1", "validatingadmissionpolicies"):              true,
		gvr("admissionregistration.k8s.io", "v1", "validatingadmissionpolicies/status"):       true,
		gvr("admissionregistration.k8s.io", "v1", "validatingadmissionpolicybindings"):        true,
		gvr("admissionregistration.k8s.io", "v1alpha1", "mutatingadmissionpolicies"):          true,
		gvr("admissionregistration.k8s.io", "v1alpha1", "mutatingadmissionpolicybindings"):    true,
	}

	parentResources = map[schema.GroupVersionResource]schema.GroupVersionResource{
		gvr("extensions", "v1beta1", "replicationcontrollers/scale"): gvr("", "v1", "replicationcontrollers"),
	}

	// stubDataOverrides holds either non persistent resources' definitions or resources where default stub needs to be overridden.
	stubDataOverrides = map[schema.GroupVersionResource]string{
		// Non persistent Reviews resource
		gvr("authentication.k8s.io", "v1", "tokenreviews"):                  `{"metadata": {"name": "tokenreview"}, "spec": {"token": "token", "audience": ["audience1","audience2"]}}`,
		gvr("authentication.k8s.io", "v1beta1", "tokenreviews"):             `{"metadata": {"name": "tokenreview"}, "spec": {"token": "token", "audience": ["audience1","audience2"]}}`,
		gvr("authentication.k8s.io", "v1alpha1", "selfsubjectreviews"):      `{"metadata": {"name": "SelfSubjectReview"},"status":{"userInfo":{}}}`,
		gvr("authentication.k8s.io", "v1beta1", "selfsubjectreviews"):       `{"metadata": {"name": "SelfSubjectReview"},"status":{"userInfo":{}}}`,
		gvr("authentication.k8s.io", "v1", "selfsubjectreviews"):            `{"metadata": {"name": "SelfSubjectReview"},"status":{"userInfo":{}}}`,
		gvr("authorization.k8s.io", "v1", "localsubjectaccessreviews"):      `{"metadata": {"name": "", "namespace":"` + testNamespace + `"}, "spec": {"uid": "token", "user": "user1","groups": ["group1","group2"],"resourceAttributes": {"name":"name1","namespace":"` + testNamespace + `"}}}`,
		gvr("authorization.k8s.io", "v1", "subjectaccessreviews"):           `{"metadata": {"name": "", "namespace":""}, "spec": {"user":"user1","resourceAttributes": {"name":"name1", "namespace":"` + testNamespace + `"}}}`,
		gvr("authorization.k8s.io", "v1", "selfsubjectaccessreviews"):       `{"metadata": {"name": "", "namespace":""}, "spec": {"resourceAttributes": {"name":"name1", "namespace":""}}}`,
		gvr("authorization.k8s.io", "v1", "selfsubjectrulesreviews"):        `{"metadata": {"name": "", "namespace":"` + testNamespace + `"}, "spec": {"namespace":"` + testNamespace + `"}}`,
		gvr("authorization.k8s.io", "v1beta1", "localsubjectaccessreviews"): `{"metadata": {"name": "", "namespace":"` + testNamespace + `"}, "spec": {"uid": "token", "user": "user1","groups": ["group1","group2"],"resourceAttributes": {"name":"name1","namespace":"` + testNamespace + `"}}}`,
		gvr("authorization.k8s.io", "v1beta1", "subjectaccessreviews"):      `{"metadata": {"name": "", "namespace":""}, "spec": {"user":"user1","resourceAttributes": {"name":"name1", "namespace":"` + testNamespace + `"}}}`,
		gvr("authorization.k8s.io", "v1beta1", "selfsubjectaccessreviews"):  `{"metadata": {"name": "", "namespace":""}, "spec": {"resourceAttributes": {"name":"name1", "namespace":""}}}`,
		gvr("authorization.k8s.io", "v1beta1", "selfsubjectrulesreviews"):   `{"metadata": {"name": "", "namespace":"` + testNamespace + `"}, "spec": {"namespace":"` + testNamespace + `"}}`,

		// Other Non persistent resources
	}
)

type webhookOptions struct {
	version string

	// phase indicates whether this is a mutating or validating webhook
	phase string
	// converted indicates if this webhook makes use of matchPolicy:equivalent and expects conversion.
	// if true, recordGVR and expectGVK are mapped through gvrToConvertedGVR/gvrToConvertedGVK.
	// if false, recordGVR and expectGVK are compared directly to the admission review.
	converted bool
}

type holder struct {
	lock sync.RWMutex

	t *testing.T

	warningHandler *warningHandler

	recordGVR       metav1.GroupVersionResource
	recordOperation string
	recordNamespace string
	recordName      string

	expectGVK        schema.GroupVersionKind
	expectObject     bool
	expectOldObject  bool
	expectOptionsGVK schema.GroupVersionKind
	expectOptions    bool

	// gvrToConvertedGVR maps the GVR submitted to the API server to the expected GVR when converted to the webhook-recognized resource.
	// When a converted request is recorded, gvrToConvertedGVR[recordGVR] is compared to the GVR seen by the webhook.
	gvrToConvertedGVR map[metav1.GroupVersionResource]metav1.GroupVersionResource
	// gvrToConvertedGVR maps the GVR submitted to the API server to the expected GVK when converted to the webhook-recognized resource.
	// When a converted request is recorded, gvrToConvertedGVR[expectGVK] is compared to the GVK seen by the webhook.
	gvrToConvertedGVK map[metav1.GroupVersionResource]schema.GroupVersionKind

	recorded map[webhookOptions]*admissionRequest
}

func (h *holder) reset(t *testing.T) {
	h.lock.Lock()
	defer h.lock.Unlock()
	h.t = t
	h.recordGVR = metav1.GroupVersionResource{}
	h.expectGVK = schema.GroupVersionKind{}
	h.recordOperation = ""
	h.recordName = ""
	h.recordNamespace = ""
	h.expectObject = false
	h.expectOldObject = false
	h.expectOptionsGVK = schema.GroupVersionKind{}
	h.expectOptions = false
	h.warningHandler.reset()

	// Set up the recorded map with nil records for all combinations
	h.recorded = map[webhookOptions]*admissionRequest{}
	for _, phase := range []string{mutation, validation} {
		for _, converted := range []bool{true, false} {
			for _, version := range []string{"v1", "v1beta1"} {
				h.recorded[webhookOptions{version: version, phase: phase, converted: converted}] = nil
			}
		}
	}
}
func (h *holder) expect(gvr schema.GroupVersionResource, gvk, optionsGVK schema.GroupVersionKind, operation v1beta1.Operation, name, namespace string, object, oldObject, options bool) {
	// Special-case namespaces, since the object name shows up in request attributes
	if len(namespace) == 0 && gvk.Group == "" && gvk.Version == "v1" && gvk.Kind == "Namespace" {
		namespace = name
	}

	h.lock.Lock()
	defer h.lock.Unlock()
	h.recordGVR = metav1.GroupVersionResource{Group: gvr.Group, Version: gvr.Version, Resource: gvr.Resource}
	h.expectGVK = gvk
	h.recordOperation = string(operation)
	h.recordName = name
	h.recordNamespace = namespace
	h.expectObject = object
	h.expectOldObject = oldObject
	h.expectOptionsGVK = optionsGVK
	h.expectOptions = options
	h.warningHandler.reset()

	// Set up the recorded map with nil records for all combinations
	h.recorded = map[webhookOptions]*admissionRequest{}
	for _, phase := range []string{mutation, validation} {
		for _, converted := range []bool{true, false} {
			for _, version := range []string{"v1", "v1beta1"} {
				h.recorded[webhookOptions{version: version, phase: phase, converted: converted}] = nil
			}
		}
	}
}

type admissionRequest struct {
	Operation   string
	Resource    metav1.GroupVersionResource
	SubResource string
	Namespace   string
	Name        string
	Object      runtime.RawExtension
	OldObject   runtime.RawExtension
	Options     runtime.RawExtension
}

func (h *holder) record(version string, phase string, converted bool, request *admissionRequest) {
	h.lock.Lock()
	defer h.lock.Unlock()

	// this is useful to turn on if items aren't getting recorded and you need to figure out why
	debug := false
	if debug {
		h.t.Logf("%s %#v %v", request.Operation, request.Resource, request.SubResource)
	}

	resource := request.Resource
	if len(request.SubResource) > 0 {
		resource.Resource += "/" + request.SubResource
	}

	// See if we should record this
	gvrToRecord := h.recordGVR
	if converted {
		// If this is a converted webhook, map to the GVR we expect the webhook to see
		gvrToRecord = h.gvrToConvertedGVR[h.recordGVR]
	}
	if resource != gvrToRecord {
		if debug {
			h.t.Log(resource, "!=", gvrToRecord)
		}
		return
	}

	if request.Operation != h.recordOperation {
		if debug {
			h.t.Log(request.Operation, "!=", h.recordOperation)
		}
		return
	}
	if request.Namespace != h.recordNamespace {
		if debug {
			h.t.Log(request.Namespace, "!=", h.recordNamespace)
		}
		return
	}

	name := request.Name
	if name != h.recordName {
		if debug {
			h.t.Log(name, "!=", h.recordName)
		}
		return
	}

	if debug {
		h.t.Logf("recording: %#v = %s %#v %v", webhookOptions{version: version, phase: phase, converted: converted}, request.Operation, request.Resource, request.SubResource)
	}
	h.recorded[webhookOptions{version: version, phase: phase, converted: converted}] = request
}

func (h *holder) verify(t *testing.T) {
	h.lock.Lock()
	defer h.lock.Unlock()

	for options, value := range h.recorded {
		if err := h.verifyRequest(options, value); err != nil {
			t.Errorf("version: %v, phase:%v, converted:%v error: %v", options.version, options.phase, options.converted, err)
		}
	}
}

func (h *holder) verifyRequest(webhookOptions webhookOptions, request *admissionRequest) error {
	converted := webhookOptions.converted

	// Check if current resource should be exempted from Admission processing
	if admissionExemptResources[gvr(h.recordGVR.Group, h.recordGVR.Version, h.recordGVR.Resource)] {
		if request == nil {
			return nil
		}
		return fmt.Errorf("admission webhook was called, but not supposed to")
	}

	if request == nil {
		return fmt.Errorf("no request received")
	}

	if h.expectObject {
		if err := h.verifyObject(converted, request.Object.Object); err != nil {
			return fmt.Errorf("object error: %v", err)
		}
	} else if request.Object.Object != nil {
		return fmt.Errorf("unexpected object: %#v", request.Object.Object)
	}

	if h.expectOldObject {
		if err := h.verifyObject(converted, request.OldObject.Object); err != nil {
			return fmt.Errorf("old object error: %v", err)
		}
	} else if request.OldObject.Object != nil {
		return fmt.Errorf("unexpected old object: %#v", request.OldObject.Object)
	}

	if h.expectOptions {
		if err := h.verifyOptions(request.Options.Object); err != nil {
			return fmt.Errorf("options error: %v", err)
		}
	} else if request.Options.Object != nil {
		return fmt.Errorf("unexpected options: %#v", request.Options.Object)
	}

	if !h.warningHandler.hasWarning(makeWarning(webhookOptions.version, webhookOptions.phase, webhookOptions.converted)) {
		return fmt.Errorf("no warning received from webhook")
	}

	return nil
}

func (h *holder) verifyObject(converted bool, obj runtime.Object) error {
	if obj == nil {
		return fmt.Errorf("no object sent")
	}
	expectGVK := h.expectGVK
	if converted {
		expectGVK = h.gvrToConvertedGVK[h.recordGVR]
	}
	if obj.GetObjectKind().GroupVersionKind() != expectGVK {
		return fmt.Errorf("expected %#v, got %#v", expectGVK, obj.GetObjectKind().GroupVersionKind())
	}
	return nil
}

func (h *holder) verifyOptions(options runtime.Object) error {
	if options == nil {
		return fmt.Errorf("no options sent")
	}
	if options.GetObjectKind().GroupVersionKind() != h.expectOptionsGVK {
		return fmt.Errorf("expected %#v, got %#v", h.expectOptionsGVK, options.GetObjectKind().GroupVersionKind())
	}
	return nil
}

type warningHandler struct {
	lock     sync.Mutex
	warnings map[string]bool
}

func (w *warningHandler) reset() {
	w.lock.Lock()
	defer w.lock.Unlock()
	w.warnings = map[string]bool{}
}
func (w *warningHandler) hasWarning(warning string) bool {
	w.lock.Lock()
	defer w.lock.Unlock()
	return w.warnings[warning]
}
func makeWarning(version string, phase string, converted bool) string {
	return fmt.Sprintf("%v/%v/%v", version, phase, converted)
}

func (w *warningHandler) HandleWarningHeader(code int, agent string, message string) {
	if code != 299 || len(message) == 0 {
		return
	}
	w.lock.Lock()
	defer w.lock.Unlock()
	w.warnings[message] = true
}

// TestWebhookAdmissionWithWatchCache tests communication between API server and webhook process.
func TestWebhookAdmissionWithWatchCache(t *testing.T) {
	testWebhookAdmission(t, true, func(testing.TB, *rest.Config) {})
}

// TestWebhookAdmissionWithoutWatchCache tests communication between API server and webhook process.
func TestWebhookAdmissionWithoutWatchCache(t *testing.T) {
	testWebhookAdmission(t, false, func(testing.TB, *rest.Config) {})
}

func TestWebhookAdmissionWithCBOR(t *testing.T) {
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.CBORServingAndStorage, true)
	clientfeaturestesting.SetFeatureDuringTest(t, clientfeatures.ClientsAllowCBOR, true)
	clientfeaturestesting.SetFeatureDuringTest(t, clientfeatures.ClientsPreferCBOR, true)
	testWebhookAdmission(t, false, func(t testing.TB, config *rest.Config) {
		config.Wrap(framework.AssertRequestResponseAsCBOR(t))
	})
}

// testWebhookAdmission tests communication between API server and webhook process.
func testWebhookAdmission(t *testing.T, watchCache bool, reconfigureClient func(testing.TB, *rest.Config)) {

	// holder communicates expectations to webhooks, and results from webhooks
	holder := &holder{
		t:                 t,
		warningHandler:    &warningHandler{warnings: map[string]bool{}},
		gvrToConvertedGVR: map[metav1.GroupVersionResource]metav1.GroupVersionResource{},
		gvrToConvertedGVK: map[metav1.GroupVersionResource]schema.GroupVersionKind{},
	}

	// set up webhook server
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(localhostCert) {
		t.Fatal("Failed to append Cert from PEM")
	}
	cert, err := tls.X509KeyPair(localhostCert, localhostKey)
	if err != nil {
		t.Fatalf("Failed to build cert with error: %+v", err)
	}

	webhookMux := http.NewServeMux()
	webhookMux.Handle("/v1beta1/"+mutation, newV1beta1WebhookHandler(t, holder, mutation, false))
	webhookMux.Handle("/v1beta1/convert/"+mutation, newV1beta1WebhookHandler(t, holder, mutation, true))
	webhookMux.Handle("/v1beta1/"+validation, newV1beta1WebhookHandler(t, holder, validation, false))
	webhookMux.Handle("/v1beta1/convert/"+validation, newV1beta1WebhookHandler(t, holder, validation, true))
	webhookMux.Handle("/v1/"+mutation, newV1WebhookHandler(t, holder, mutation, false))
	webhookMux.Handle("/v1/convert/"+mutation, newV1WebhookHandler(t, holder, mutation, true))
	webhookMux.Handle("/v1/"+validation, newV1WebhookHandler(t, holder, validation, false))
	webhookMux.Handle("/v1/convert/"+validation, newV1WebhookHandler(t, holder, validation, true))
	webhookMux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		holder.t.Errorf("unexpected request to %v", req.URL.Path)
	}))
	webhookServer := httptest.NewUnstartedServer(webhookMux)
	webhookServer.TLS = &tls.Config{
		RootCAs:      roots,
		Certificates: []tls.Certificate{cert},
	}
	webhookServer.StartTLS()
	defer webhookServer.Close()

	// start API server
	etcdConfig := framework.SharedEtcd()
	server := kubeapiservertesting.StartTestServerOrDie(t, nil, []string{
		fmt.Sprintf("--watch-cache=%v", watchCache),
		// turn off admission plugins that add finalizers
		"--disable-admission-plugins=ServiceAccount,StorageObjectInUseProtection",
		// force enable all resources so we can check storage.
		"--runtime-config=api/all=true",
		// enable feature-gates that protect resources to check their storage, too.
		// e.g. "--feature-gates=EphemeralContainers=true",
	}, etcdConfig)
	defer server.TearDownFn()

	// Configure a client with a distinct user name so that it is easy to distinguish requests
	// made by the client from requests made by controllers. We use this to filter out requests
	// before recording them to ensure we don't accidentally mistake requests from controllers
	// as requests made by the client.
	clientConfig := rest.CopyConfig(server.ClientConfig)
	clientConfig.Impersonate.UserName = testClientUsername
	clientConfig.Impersonate.Groups = []string{"system:masters", "system:authenticated"}
	clientConfig.WarningHandler = holder.warningHandler
	client, err := clientset.NewForConfig(clientConfig)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// create CRDs
	etcd.CreateTestCRDs(t, apiextensionsclientset.NewForConfigOrDie(server.ClientConfig), false, etcd.GetCustomResourceDefinitionData()...)

	if _, err := client.CoreV1().Namespaces().Create(context.TODO(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: testNamespace}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	// gather resources to test
	_, resources, err := client.Discovery().ServerGroupsAndResources()
	if err != nil {
		t.Fatalf("Failed to get ServerGroupsAndResources with error: %+v", err)
	}

	gvrsToTest := []schema.GroupVersionResource{}
	resourcesByGVR := map[schema.GroupVersionResource]metav1.APIResource{}

	for _, list := range resources {
		defaultGroupVersion, err := schema.ParseGroupVersion(list.GroupVersion)
		if err != nil {
			t.Errorf("Failed to get GroupVersion for: %+v", list)
			continue
		}
		for _, resource := range list.APIResources {
			if resource.Group == "" {
				resource.Group = defaultGroupVersion.Group
			}
			if resource.Version == "" {
				resource.Version = defaultGroupVersion.Version
			}
			gvr := defaultGroupVersion.WithResource(resource.Name)
			resourcesByGVR[gvr] = resource
			if shouldTestResource(gvr, resource) {
				gvrsToTest = append(gvrsToTest, gvr)
			}
		}
	}

	sort.SliceStable(gvrsToTest, func(i, j int) bool {
		if gvrsToTest[i].Group < gvrsToTest[j].Group {
			return true
		}
		if gvrsToTest[i].Group > gvrsToTest[j].Group {
			return false
		}
		if gvrsToTest[i].Version < gvrsToTest[j].Version {
			return true
		}
		if gvrsToTest[i].Version > gvrsToTest[j].Version {
			return false
		}
		if gvrsToTest[i].Resource < gvrsToTest[j].Resource {
			return true
		}
		if gvrsToTest[i].Resource > gvrsToTest[j].Resource {
			return false
		}
		return true
	})

	// map unqualified resource names to the fully qualified resource we will expect to be converted to
	// Note: this only works because there are no overlapping resource names in-process that are not co-located
	convertedResources := map[string]schema.GroupVersionResource{}
	// build the webhook rules enumerating the specific group/version/resources we want
	convertedV1beta1Rules := []admissionregistrationv1beta1.RuleWithOperations{}
	convertedV1Rules := []admissionregistrationv1.RuleWithOperations{}
	for _, gvr := range gvrsToTest {
		metaGVR := metav1.GroupVersionResource{Group: gvr.Group, Version: gvr.Version, Resource: gvr.Resource}

		convertedGVR, ok := convertedResources[gvr.Resource]
		if !ok {
			// this is the first time we've seen this resource
			// record the fully qualified resource we expect
			convertedGVR = gvr
			convertedResources[gvr.Resource] = gvr
			// add an admission rule indicating we can receive this version
			convertedV1beta1Rules = append(convertedV1beta1Rules, admissionregistrationv1beta1.RuleWithOperations{
				Operations: []admissionregistrationv1beta1.OperationType{admissionregistrationv1beta1.OperationAll},
				Rule:       admissionregistrationv1beta1.Rule{APIGroups: []string{gvr.Group}, APIVersions: []string{gvr.Version}, Resources: []string{gvr.Resource}},
			})
			convertedV1Rules = append(convertedV1Rules, admissionregistrationv1.RuleWithOperations{
				Operations: []admissionregistrationv1.OperationType{admissionregistrationv1.OperationAll},
				Rule:       admissionregistrationv1.Rule{APIGroups: []string{gvr.Group}, APIVersions: []string{gvr.Version}, Resources: []string{gvr.Resource}},
			})
		}

		// record the expected resource and kind
		holder.gvrToConvertedGVR[metaGVR] = metav1.GroupVersionResource{Group: convertedGVR.Group, Version: convertedGVR.Version, Resource: convertedGVR.Resource}
		holder.gvrToConvertedGVK[metaGVR] = schema.GroupVersionKind{Group: resourcesByGVR[convertedGVR].Group, Version: resourcesByGVR[convertedGVR].Version, Kind: resourcesByGVR[convertedGVR].Kind}
	}

	if err := createV1beta1MutationWebhook(server.EtcdClient, server.EtcdStoragePrefix, client, webhookServer.URL+"/v1beta1/"+mutation, webhookServer.URL+"/v1beta1/convert/"+mutation, convertedV1beta1Rules); err != nil {
		t.Fatal(err)
	}
	if err := createV1beta1ValidationWebhook(server.EtcdClient, server.EtcdStoragePrefix, client, webhookServer.URL+"/v1beta1/"+validation, webhookServer.URL+"/v1beta1/convert/"+validation, convertedV1beta1Rules); err != nil {
		t.Fatal(err)
	}
	if err := createV1MutationWebhook(client, webhookServer.URL+"/v1/"+mutation, webhookServer.URL+"/v1/convert/"+mutation, convertedV1Rules); err != nil {
		t.Fatal(err)
	}
	if err := createV1ValidationWebhook(client, webhookServer.URL+"/v1/"+validation, webhookServer.URL+"/v1/convert/"+validation, convertedV1Rules); err != nil {
		t.Fatal(err)
	}

	// Allow the webhook to establish
	time.Sleep(time.Second)

	start := time.Now()
	count := 0

	// Test admission on all resources, subresources, and verbs
	for _, gvr := range gvrsToTest {
		resource := resourcesByGVR[gvr]
		t.Run(gvr.Group+"."+gvr.Version+"."+strings.ReplaceAll(resource.Name, "/", "."), func(t *testing.T) {
			for _, verb := range []string{"create", "update", "patch", "connect", "delete", "deletecollection"} {
				if shouldTestResourceVerb(gvr, resource, verb) {
					t.Run(verb, func(t *testing.T) {
						clientConfig := rest.CopyConfig(clientConfig)
						reconfigureClient(t, clientConfig)
						dynamicClient, err := dynamic.NewForConfig(clientConfig)
						if err != nil {
							t.Fatal(err)
						}

						count++
						holder.reset(t)
						testFunc := getTestFunc(gvr, verb)
						testFunc(&testContext{
							t:               t,
							admissionHolder: holder,
							client:          dynamicClient,
							clientset:       client,
							verb:            verb,
							gvr:             gvr,
							resource:        resource,
							resources:       resourcesByGVR,
						})
						holder.verify(t)
					})
				}
			}
		})
	}

	duration := time.Since(start)
	perResourceDuration := time.Duration(int(duration) / count)
	if perResourceDuration >= 150*time.Millisecond {
		t.Errorf("expected resources to process in < 150ms, average was %v", perResourceDuration)
	}
}

//
// generic resource testing
//

func testResourceCreate(c *testContext) {
	stubObj, err := getStubObj(c.gvr, c.resource)
	if err != nil {
		c.t.Error(err)
		return
	}
	ns := ""
	if c.resource.Namespaced {
		ns = testNamespace
	}
	c.admissionHolder.expect(c.gvr, gvk(c.resource.Group, c.resource.Version, c.resource.Kind), gvkCreateOptions, v1beta1.Create, stubObj.GetName(), ns, true, false, true)
	_, err = c.client.Resource(c.gvr).Namespace(ns).Create(context.TODO(), stubObj, metav1.CreateOptions{})
	if err != nil {
		c.t.Error(err)
		return
	}
}

func testResourceUpdate(c *testContext) {
	if err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		obj, err := createOrGetResource(c.client, c.gvr, c.resource)
		if err != nil {
			return err
		}
		obj.SetAnnotations(map[string]string{"update": "true"})
		c.admissionHolder.expect(c.gvr, gvk(c.resource.Group, c.resource.Version, c.resource.Kind), gvkUpdateOptions, v1beta1.Update, obj.GetName(), obj.GetNamespace(), true, true, true)
		_, err = c.client.Resource(c.gvr).Namespace(obj.GetNamespace()).Update(context.TODO(), obj, metav1.UpdateOptions{})
		return err
	}); err != nil {
		c.t.Error(err)
		return
	}
}

func testResourcePatch(c *testContext) {
	obj, err := createOrGetResource(c.client, c.gvr, c.resource)
	if err != nil {
		c.t.Error(err)
		return
	}
	c.admissionHolder.expect(c.gvr, gvk(c.resource.Group, c.resource.Version, c.resource.Kind), gvkUpdateOptions, v1beta1.Update, obj.GetName(), obj.GetNamespace(), true, true, true)
	_, err = c.client.Resource(c.gvr).Namespace(obj.GetNamespace()).Patch(
		context.TODO(),
		obj.GetName(),
		types.MergePatchType,
		[]byte(`{"metadata":{"annotations":{"patch":"true"}}}`),
		metav1.PatchOptions{})
	if err != nil {
		c.t.Error(err)
		return
	}
}

func testResourceDelete(c *testContext) {
	// Verify that an immediate delete triggers the webhook and populates the admissionRequest.oldObject.
	obj, err := createOrGetResource(c.client, c.gvr, c.resource)
	if err != nil {
		c.t.Error(err)
		return
	}
	background := metav1.DeletePropagationBackground
	zero := int64(0)
	c.admissionHolder.expect(c.gvr, gvk(c.resource.Group, c.resource.Version, c.resource.Kind), gvkDeleteOptions, v1beta1.Delete, obj.GetName(), obj.GetNamespace(), false, true, true)
	err = c.client.Resource(c.gvr).Namespace(obj.GetNamespace()).Delete(context.TODO(), obj.GetName(), metav1.DeleteOptions{GracePeriodSeconds: &zero, PropagationPolicy: &background})
	if err != nil {
		c.t.Error(err)
		return
	}
	c.admissionHolder.verify(c.t)

	// wait for the item to be gone
	err = wait.PollImmediate(100*time.Millisecond, 10*time.Second, func() (bool, error) {
		obj, err := c.client.Resource(c.gvr).Namespace(obj.GetNamespace()).Get(context.TODO(), obj.GetName(), metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		if err == nil {
			c.t.Logf("waiting for %#v to be deleted (name: %s, finalizers: %v)...\n", c.gvr, obj.GetName(), obj.GetFinalizers())
			return false, nil
		}
		return false, err
	})
	if err != nil {
		c.t.Error(err)
		return
	}

	// Verify that an update-on-delete triggers the webhook and populates the admissionRequest.oldObject.
	obj, err = createOrGetResource(c.client, c.gvr, c.resource)
	if err != nil {
		c.t.Error(err)
		return
	}
	// Adding finalizer to the object, then deleting it.
	// We don't add finalizers by setting DeleteOptions.PropagationPolicy
	// because some resource (e.g., events) do not support garbage
	// collector finalizers.
	_, err = c.client.Resource(c.gvr).Namespace(obj.GetNamespace()).Patch(
		context.TODO(),
		obj.GetName(),
		types.MergePatchType,
		[]byte(`{"metadata":{"finalizers":["test/k8s.io"]}}`),
		metav1.PatchOptions{})
	if err != nil {
		c.t.Error(err)
		return
	}
	c.admissionHolder.expect(c.gvr, gvk(c.resource.Group, c.resource.Version, c.resource.Kind), gvkDeleteOptions, v1beta1.Delete, obj.GetName(), obj.GetNamespace(), false, true, true)
	err = c.client.Resource(c.gvr).Namespace(obj.GetNamespace()).Delete(context.TODO(), obj.GetName(), metav1.DeleteOptions{GracePeriodSeconds: &zero, PropagationPolicy: &background})
	if err != nil {
		c.t.Error(err)
		return
	}
	c.admissionHolder.verify(c.t)

	// wait other finalizers (e.g., crd's customresourcecleanup finalizer) to be removed.
	err = wait.PollImmediate(100*time.Millisecond, 10*time.Second, func() (bool, error) {
		obj, err := c.client.Resource(c.gvr).Namespace(obj.GetNamespace()).Get(context.TODO(), obj.GetName(), metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		finalizers := obj.GetFinalizers()
		if len(finalizers) != 1 {
			c.t.Logf("waiting for other finalizers on %#v %s to be removed, existing finalizers are %v", c.gvr, obj.GetName(), obj.GetFinalizers())
			return false, nil
		}
		if finalizers[0] != "test/k8s.io" {
			return false, fmt.Errorf("expected the single finalizer on %#v %s to be test/k8s.io, got %v", c.gvr, obj.GetName(), obj.GetFinalizers())
		}
		return true, nil
	})
	if err != nil {
		c.t.Error(err)
		return
	}

	// remove the finalizer
	_, err = c.client.Resource(c.gvr).Namespace(obj.GetNamespace()).Patch(
		context.TODO(),
		obj.GetName(),
		types.MergePatchType,
		[]byte(`{"metadata":{"finalizers":[]}}`),
		metav1.PatchOptions{})
	if err != nil {
		c.t.Error(err)
		return
	}
	// wait for the item to be gone
	err = wait.PollImmediate(100*time.Millisecond, 10*time.Second, func() (bool, error) {
		obj, err := c.client.Resource(c.gvr).Namespace(obj.GetNamespace()).Get(context.TODO(), obj.GetName(), metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		if err == nil {
			c.t.Logf("waiting for %#v to be deleted (name: %s, finalizers: %v)...\n", c.gvr, obj.GetName(), obj.GetFinalizers())
			return false, nil
		}
		return false, err
	})
	if err != nil {
		c.t.Error(err)
		return
	}
}

func testResourceDeletecollection(c *testContext) {
	obj, err := createOrGetResource(c.client, c.gvr, c.resource)
	if err != nil {
		c.t.Error(err)
		return
	}
	background := metav1.DeletePropagationBackground
	zero := int64(0)

	// update the object with a label that matches our selector
	_, err = c.client.Resource(c.gvr).Namespace(obj.GetNamespace()).Patch(
		context.TODO(),
		obj.GetName(),
		types.MergePatchType,
		[]byte(`{"metadata":{"labels":{"webhooktest":"true"}}}`),
		metav1.PatchOptions{})
	if err != nil {
		c.t.Error(err)
		return
	}

	// set expectations
	c.admissionHolder.expect(c.gvr, gvk(c.resource.Group, c.resource.Version, c.resource.Kind), gvkDeleteOptions, v1beta1.Delete, "", obj.GetNamespace(), false, true, true)

	// delete
	err = c.client.Resource(c.gvr).Namespace(obj.GetNamespace()).DeleteCollection(context.TODO(), metav1.DeleteOptions{GracePeriodSeconds: &zero, PropagationPolicy: &background}, metav1.ListOptions{LabelSelector: "webhooktest=true"})
	if err != nil {
		c.t.Error(err)
		return
	}

	// wait for the item to be gone
	err = wait.PollImmediate(100*time.Millisecond, 10*time.Second, func() (bool, error) {
		obj, err := c.client.Resource(c.gvr).Namespace(obj.GetNamespace()).Get(context.TODO(), obj.GetName(), metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		if err == nil {
			c.t.Logf("waiting for %#v to be deleted (name: %s, finalizers: %v)...\n", c.gvr, obj.GetName(), obj.GetFinalizers())
			return false, nil
		}
		return false, err
	})
	if err != nil {
		c.t.Error(err)
		return
	}
}

func getParentGVR(gvr schema.GroupVersionResource) schema.GroupVersionResource {
	parentGVR, found := parentResources[gvr]
	// if no special override is found, just drop the subresource
	if !found {
		parentGVR = gvr
		parentGVR.Resource = strings.Split(parentGVR.Resource, "/")[0]
	}
	return parentGVR
}

func testTokenCreate(c *testContext) {
	saGVR := gvr("", "v1", "serviceaccounts")
	sa, err := createOrGetResource(c.client, saGVR, c.resources[saGVR])
	if err != nil {
		c.t.Error(err)
		return
	}

	c.admissionHolder.expect(c.gvr, gvk(c.resource.Group, c.resource.Version, c.resource.Kind), gvkCreateOptions, v1beta1.Create, sa.GetName(), sa.GetNamespace(), true, false, true)
	if err = c.clientset.CoreV1().RESTClient().Post().Namespace(sa.GetNamespace()).Resource("serviceaccounts").Name(sa.GetName()).SubResource("token").Body(&authenticationv1.TokenRequest{
		ObjectMeta: metav1.ObjectMeta{Name: sa.GetName()},
		Spec: authenticationv1.TokenRequestSpec{
			Audiences: []string{"api"},
		},
	}).Do(context.TODO()).Error(); err != nil {
		c.t.Error(err)
		return
	}
	c.admissionHolder.verify(c.t)
}

func testSubresourceUpdate(c *testContext) {
	if err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		parentGVR := getParentGVR(c.gvr)
		parentResource := c.resources[parentGVR]
		obj, err := createOrGetResource(c.client, parentGVR, parentResource)
		if err != nil {
			return err
		}

		// Save the parent object as what we submit
		submitObj := obj

		gvrWithoutSubresources := c.gvr
		gvrWithoutSubresources.Resource = strings.Split(gvrWithoutSubresources.Resource, "/")[0]
		subresources := strings.Split(c.gvr.Resource, "/")[1:]

		// If the subresource supports get, fetch that as the object to submit (namespaces/finalize, */scale, etc)
		if sets.NewString(c.resource.Verbs...).Has("get") {
			submitObj, err = c.client.Resource(gvrWithoutSubresources).Namespace(obj.GetNamespace()).Get(context.TODO(), obj.GetName(), metav1.GetOptions{}, subresources...)
			if err != nil {
				return err
			}
		}

		// Modify the object
		submitObj.SetAnnotations(map[string]string{"subresourceupdate": "true"})

		// set expectations
		c.admissionHolder.expect(c.gvr, gvk(c.resource.Group, c.resource.Version, c.resource.Kind), gvkUpdateOptions, v1beta1.Update, obj.GetName(), obj.GetNamespace(), true, true, true)

		_, err = c.client.Resource(gvrWithoutSubresources).Namespace(obj.GetNamespace()).Update(
			context.TODO(),
			submitObj,
			metav1.UpdateOptions{},
			subresources...,
		)
		return err
	}); err != nil {
		c.t.Error(err)
	}
}

func testSubresourcePatch(c *testContext) {
	parentGVR := getParentGVR(c.gvr)
	parentResource := c.resources[parentGVR]
	obj, err := createOrGetResource(c.client, parentGVR, parentResource)
	if err != nil {
		c.t.Error(err)
		return
	}

	gvrWithoutSubresources := c.gvr
	gvrWithoutSubresources.Resource = strings.Split(gvrWithoutSubresources.Resource, "/")[0]
	subresources := strings.Split(c.gvr.Resource, "/")[1:]

	// set expectations
	c.admissionHolder.expect(c.gvr, gvk(c.resource.Group, c.resource.Version, c.resource.Kind), gvkUpdateOptions, v1beta1.Update, obj.GetName(), obj.GetNamespace(), true, true, true)

	_, err = c.client.Resource(gvrWithoutSubresources).Namespace(obj.GetNamespace()).Patch(
		context.TODO(),
		obj.GetName(),
		types.MergePatchType,
		[]byte(`{"metadata":{"annotations":{"subresourcepatch":"true"}}}`),
		metav1.PatchOptions{},
		subresources...,
	)
	if err != nil {
		c.t.Error(err)
		return
	}
}

func unimplemented(c *testContext) {
	c.t.Errorf("Test function for %+v has not been implemented...", c.gvr)
}

//
// custom methods
//

// testNamespaceDelete verifies namespace-specific delete behavior:
// - ensures admission is called on first delete (which only sets deletionTimestamp and terminating state)
// - removes finalizer from namespace
// - ensures admission is called on final delete once finalizers are removed
func testNamespaceDelete(c *testContext) {
	obj, err := createOrGetResource(c.client, c.gvr, c.resource)
	if err != nil {
		c.t.Error(err)
		return
	}
	background := metav1.DeletePropagationBackground
	zero := int64(0)

	c.admissionHolder.expect(c.gvr, gvk(c.resource.Group, c.resource.Version, c.resource.Kind), gvkDeleteOptions, v1beta1.Delete, obj.GetName(), obj.GetNamespace(), false, true, true)
	err = c.client.Resource(c.gvr).Namespace(obj.GetNamespace()).Delete(context.TODO(), obj.GetName(), metav1.DeleteOptions{GracePeriodSeconds: &zero, PropagationPolicy: &background})
	if err != nil {
		c.t.Error(err)
		return
	}
	c.admissionHolder.verify(c.t)

	// do the finalization so the namespace can be deleted
	obj, err = c.client.Resource(c.gvr).Namespace(obj.GetNamespace()).Get(context.TODO(), obj.GetName(), metav1.GetOptions{})
	if err != nil {
		c.t.Error(err)
		return
	}
	err = unstructured.SetNestedField(obj.Object, nil, "spec", "finalizers")
	if err != nil {
		c.t.Error(err)
		return
	}
	_, err = c.client.Resource(c.gvr).Namespace(obj.GetNamespace()).Update(context.TODO(), obj, metav1.UpdateOptions{}, "finalize")
	if err != nil {
		c.t.Error(err)
		return
	}
	// verify namespace is gone
	obj, err = c.client.Resource(c.gvr).Namespace(obj.GetNamespace()).Get(context.TODO(), obj.GetName(), metav1.GetOptions{})
	if err == nil || !apierrors.IsNotFound(err) {
		c.t.Errorf("expected namespace to be gone, got %#v, %v", obj, err)
	}
}

// testDeploymentRollback verifies rollback-specific behavior:
// - creates a parent deployment
// - creates a rollback object and posts it
func testDeploymentRollback(c *testContext) {
	deploymentGVR := gvr("apps", "v1", "deployments")
	obj, err := createOrGetResource(c.client, deploymentGVR, c.resources[deploymentGVR])
	if err != nil {
		c.t.Error(err)
		return
	}

	gvrWithoutSubresources := c.gvr
	gvrWithoutSubresources.Resource = strings.Split(gvrWithoutSubresources.Resource, "/")[0]
	subresources := strings.Split(c.gvr.Resource, "/")[1:]

	c.admissionHolder.expect(c.gvr, gvk(c.resource.Group, c.resource.Version, c.resource.Kind), gvkCreateOptions, v1beta1.Create, obj.GetName(), obj.GetNamespace(), true, false, true)

	var rollbackObj runtime.Object
	switch c.gvr {
	case gvr("apps", "v1beta1", "deployments/rollback"):
		rollbackObj = &appsv1beta1.DeploymentRollback{
			TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1beta1", Kind: "DeploymentRollback"},
			Name:       obj.GetName(),
			RollbackTo: appsv1beta1.RollbackConfig{Revision: 0},
		}
	case gvr("extensions", "v1beta1", "deployments/rollback"):
		rollbackObj = &extensionsv1beta1.DeploymentRollback{
			TypeMeta:   metav1.TypeMeta{APIVersion: "extensions/v1beta1", Kind: "DeploymentRollback"},
			Name:       obj.GetName(),
			RollbackTo: extensionsv1beta1.RollbackConfig{Revision: 0},
		}
	default:
		c.t.Errorf("unknown rollback resource %#v", c.gvr)
		return
	}

	rollbackUnstructuredBody, err := runtime.DefaultUnstructuredConverter.ToUnstructured(rollbackObj)
	if err != nil {
		c.t.Errorf("ToUnstructured failed: %v", err)
		return
	}
	rollbackUnstructuredObj := &unstructured.Unstructured{Object: rollbackUnstructuredBody}
	rollbackUnstructuredObj.SetName(obj.GetName())

	_, err = c.client.Resource(gvrWithoutSubresources).Namespace(obj.GetNamespace()).Create(context.TODO(), rollbackUnstructuredObj, metav1.CreateOptions{}, subresources...)
	if err != nil {
		c.t.Error(err)
		return
	}
}

// testPodConnectSubresource verifies connect subresources
func testPodConnectSubresource(c *testContext) {
	podGVR := gvr("", "v1", "pods")
	pod, err := createOrGetResource(c.client, podGVR, c.resources[podGVR])
	if err != nil {
		c.t.Error(err)
		return
	}

	// check all upgradeable verbs
	for _, httpMethod := range []string{"GET", "POST"} {
		c.t.Logf("verifying %v", httpMethod)

		c.admissionHolder.expect(c.gvr, gvk(c.resource.Group, c.resource.Version, c.resource.Kind), schema.GroupVersionKind{}, v1beta1.Connect, pod.GetName(), pod.GetNamespace(), true, false, false)
		var err error
		switch c.gvr {
		case gvr("", "v1", "pods/exec"):
			err = c.clientset.CoreV1().RESTClient().Verb(httpMethod).Namespace(pod.GetNamespace()).Resource("pods").Name(pod.GetName()).SubResource("exec").Do(context.TODO()).Error()
		case gvr("", "v1", "pods/attach"):
			err = c.clientset.CoreV1().RESTClient().Verb(httpMethod).Namespace(pod.GetNamespace()).Resource("pods").Name(pod.GetName()).SubResource("attach").Do(context.TODO()).Error()
		case gvr("", "v1", "pods/portforward"):
			err = c.clientset.CoreV1().RESTClient().Verb(httpMethod).Namespace(pod.GetNamespace()).Resource("pods").Name(pod.GetName()).SubResource("portforward").Do(context.TODO()).Error()
		default:
			c.t.Errorf("unknown subresource %#v", c.gvr)
			return
		}

		if err != nil {
			c.t.Logf("debug: result of subresource connect: %v", err)
		}
		c.admissionHolder.verify(c.t)

	}
}

// testPodBindingEviction verifies pod binding and eviction admission
func testPodBindingEviction(c *testContext) {
	podGVR := gvr("", "v1", "pods")
	pod, err := createOrGetResource(c.client, podGVR, c.resources[podGVR])
	if err != nil {
		c.t.Error(err)
		return
	}

	background := metav1.DeletePropagationBackground
	zero := int64(0)
	forceDelete := metav1.DeleteOptions{GracePeriodSeconds: &zero, PropagationPolicy: &background}
	defer func() {
		err := c.clientset.CoreV1().Pods(pod.GetNamespace()).Delete(context.TODO(), pod.GetName(), forceDelete)
		if err != nil && !apierrors.IsNotFound(err) {
			c.t.Error(err)
			return
		}
	}()

	c.admissionHolder.expect(c.gvr, gvk(c.resource.Group, c.resource.Version, c.resource.Kind), gvkCreateOptions, v1beta1.Create, pod.GetName(), pod.GetNamespace(), true, false, true)

	switch c.gvr {
	case gvr("", "v1", "bindings"):
		err = c.clientset.CoreV1().RESTClient().Post().Namespace(pod.GetNamespace()).Resource("bindings").Body(&corev1.Binding{
			ObjectMeta: metav1.ObjectMeta{Name: pod.GetName()},
			Target:     corev1.ObjectReference{Name: "foo", Kind: "Node", APIVersion: "v1"},
		}).Do(context.TODO()).Error()

	case gvr("", "v1", "pods/binding"):
		err = c.clientset.CoreV1().RESTClient().Post().Namespace(pod.GetNamespace()).Resource("pods").Name(pod.GetName()).SubResource("binding").Body(&corev1.Binding{
			ObjectMeta: metav1.ObjectMeta{Name: pod.GetName()},
			Target:     corev1.ObjectReference{Name: "foo", Kind: "Node", APIVersion: "v1"},
		}).Do(context.TODO()).Error()

	case gvr("", "v1", "pods/eviction"):
		err = c.clientset.CoreV1().RESTClient().Post().Namespace(pod.GetNamespace()).Resource("pods").Name(pod.GetName()).SubResource("eviction").Body(&policyv1.Eviction{
			ObjectMeta:    metav1.ObjectMeta{Name: pod.GetName()},
			DeleteOptions: &forceDelete,
		}).Do(context.TODO()).Error()

	default:
		c.t.Errorf("unhandled resource %#v", c.gvr)
		return
	}

	if err != nil {
		c.t.Error(err)
		return
	}
}

// testSubresourceProxy verifies proxy subresources
func testSubresourceProxy(c *testContext) {
	parentGVR := getParentGVR(c.gvr)
	parentResource := c.resources[parentGVR]
	obj, err := createOrGetResource(c.client, parentGVR, parentResource)
	if err != nil {
		c.t.Error(err)
		return
	}

	gvrWithoutSubresources := c.gvr
	gvrWithoutSubresources.Resource = strings.Split(gvrWithoutSubresources.Resource, "/")[0]
	subresources := strings.Split(c.gvr.Resource, "/")[1:]

	verbToHTTPMethods := map[string][]string{
		"create": {"POST", "GET", "HEAD", "OPTIONS"}, // also test read-only verbs map to Connect admission
		"update": {"PUT"},
		"patch":  {"PATCH"},
		"delete": {"DELETE"},
	}
	httpMethodsToTest, ok := verbToHTTPMethods[c.verb]
	if !ok {
		c.t.Errorf("unknown verb %v", c.verb)
		return
	}

	for _, httpMethod := range httpMethodsToTest {
		c.t.Logf("testing %v", httpMethod)
		request := c.clientset.CoreV1().RESTClient().Verb(httpMethod)

		// add the namespace if required
		if len(obj.GetNamespace()) > 0 {
			request = request.Namespace(obj.GetNamespace())
		}

		// set expectations
		c.admissionHolder.expect(c.gvr, gvk(c.resource.Group, c.resource.Version, c.resource.Kind), schema.GroupVersionKind{}, v1beta1.Connect, obj.GetName(), obj.GetNamespace(), true, false, false)
		// run the request. we don't actually care if the request is successful, just that admission gets called as expected
		err = request.Resource(gvrWithoutSubresources.Resource).Name(obj.GetName()).SubResource(subresources...).Do(context.TODO()).Error()
		if err != nil {
			c.t.Logf("debug: result of subresource proxy (error expected): %v", err)
		}
		// verify the result
		c.admissionHolder.verify(c.t)
	}
}

func testPruningRandomNumbers(c *testContext) {
	testResourceCreate(c)

	cr2pant, err := c.client.Resource(c.gvr).Get(context.TODO(), "fortytwo", metav1.GetOptions{})
	if err != nil {
		c.t.Error(err)
		return
	}

	foo, found, err := unstructured.NestedString(cr2pant.Object, "foo")
	if err != nil {
		c.t.Error(err)
		return
	}
	if found {
		c.t.Errorf("expected .foo to be pruned, but got: %s", foo)
	}
}

func testNoPruningCustomFancy(c *testContext) {
	testResourceCreate(c)

	cr2pant, err := c.client.Resource(c.gvr).Get(context.TODO(), "cr2pant", metav1.GetOptions{})
	if err != nil {
		c.t.Error(err)
		return
	}

	foo, _, err := unstructured.NestedString(cr2pant.Object, "foo")
	if err != nil {
		c.t.Error(err)
		return
	}

	// check that no pruning took place
	if expected, got := "test", foo; expected != got {
		c.t.Errorf("expected /foo to be %q, got: %q", expected, got)
	}
}

//
// utility methods
//

func newV1beta1WebhookHandler(t *testing.T, holder *holder, phase string, converted bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		data, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}

		if contentType := r.Header.Get("Content-Type"); contentType != "application/json" {
			t.Errorf("contentType=%s, expect application/json", contentType)
			return
		}

		review := v1beta1.AdmissionReview{}
		if err := json.Unmarshal(data, &review); err != nil {
			t.Errorf("Fail to deserialize object: %s with error: %v", string(data), err)
			http.Error(w, err.Error(), 400)
			return
		}

		if review.GetObjectKind().GroupVersionKind() != gvk("admission.k8s.io", "v1beta1", "AdmissionReview") {
			t.Errorf("Invalid admission review kind: %#v", review.GetObjectKind().GroupVersionKind())
			http.Error(w, err.Error(), 400)
			return
		}

		if len(review.Request.Object.Raw) > 0 {
			u := &unstructured.Unstructured{Object: map[string]interface{}{}}
			if err := json.Unmarshal(review.Request.Object.Raw, u); err != nil {
				t.Errorf("Fail to deserialize object: %s with error: %v", string(review.Request.Object.Raw), err)
				http.Error(w, err.Error(), 400)
				return
			}
			review.Request.Object.Object = u
		}
		if len(review.Request.OldObject.Raw) > 0 {
			u := &unstructured.Unstructured{Object: map[string]interface{}{}}
			if err := json.Unmarshal(review.Request.OldObject.Raw, u); err != nil {
				t.Errorf("Fail to deserialize object: %s with error: %v", string(review.Request.OldObject.Raw), err)
				http.Error(w, err.Error(), 400)
				return
			}
			review.Request.OldObject.Object = u
		}

		if len(review.Request.Options.Raw) > 0 {
			u := &unstructured.Unstructured{Object: map[string]interface{}{}}
			if err := json.Unmarshal(review.Request.Options.Raw, u); err != nil {
				t.Errorf("Fail to deserialize options object: %s for admission request %#+v with error: %v", string(review.Request.Options.Raw), review.Request, err)
				http.Error(w, err.Error(), 400)
				return
			}
			review.Request.Options.Object = u
		}

		if review.Request.UserInfo.Username == testClientUsername {
			// only record requests originating from this integration test's client
			reviewRequest := &admissionRequest{
				Operation:   string(review.Request.Operation),
				Resource:    review.Request.Resource,
				SubResource: review.Request.SubResource,
				Namespace:   review.Request.Namespace,
				Name:        review.Request.Name,
				Object:      review.Request.Object,
				OldObject:   review.Request.OldObject,
				Options:     review.Request.Options,
			}
			holder.record("v1beta1", phase, converted, reviewRequest)
		}

		review.Response = &v1beta1.AdmissionResponse{
			Allowed: true,
			Result:  &metav1.Status{Message: "admitted"},
		}

		// v1beta1 webhook handler tolerated these not being set. verify the server continues to accept these as unset.
		review.APIVersion = ""
		review.Kind = ""
		review.Response.UID = ""

		// test plumbing warnings back to the client
		review.Response.Warnings = []string{makeWarning("v1beta1", phase, converted)}

		// If we're mutating, and have an object, return a patch to exercise conversion
		if phase == mutation && len(review.Request.Object.Raw) > 0 {
			review.Response.Patch = []byte(`[{"op":"add","path":"/foo","value":"test"}]`)
			jsonPatch := v1beta1.PatchTypeJSONPatch
			review.Response.PatchType = &jsonPatch
		}

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(review); err != nil {
			t.Errorf("Marshal of response failed with error: %v", err)
		}
	})
}

func newV1WebhookHandler(t *testing.T, holder *holder, phase string, converted bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		data, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}

		if contentType := r.Header.Get("Content-Type"); contentType != "application/json" {
			t.Errorf("contentType=%s, expect application/json", contentType)
			return
		}

		review := admissionreviewv1.AdmissionReview{}
		if err := json.Unmarshal(data, &review); err != nil {
			t.Errorf("Fail to deserialize object: %s with error: %v", string(data), err)
			http.Error(w, err.Error(), 400)
			return
		}

		if review.GetObjectKind().GroupVersionKind() != gvk("admission.k8s.io", "v1", "AdmissionReview") {
			err := fmt.Errorf("Invalid admission review kind: %#v", review.GetObjectKind().GroupVersionKind())
			t.Error(err)
			http.Error(w, err.Error(), 400)
			return
		}

		if len(review.Request.Object.Raw) > 0 {
			u := &unstructured.Unstructured{Object: map[string]interface{}{}}
			if err := json.Unmarshal(review.Request.Object.Raw, u); err != nil {
				t.Errorf("Fail to deserialize object: %s with error: %v", string(review.Request.Object.Raw), err)
				http.Error(w, err.Error(), 400)
				return
			}
			review.Request.Object.Object = u
		}
		if len(review.Request.OldObject.Raw) > 0 {
			u := &unstructured.Unstructured{Object: map[string]interface{}{}}
			if err := json.Unmarshal(review.Request.OldObject.Raw, u); err != nil {
				t.Errorf("Fail to deserialize object: %s with error: %v", string(review.Request.OldObject.Raw), err)
				http.Error(w, err.Error(), 400)
				return
			}
			review.Request.OldObject.Object = u
		}

		if len(review.Request.Options.Raw) > 0 {
			u := &unstructured.Unstructured{Object: map[string]interface{}{}}
			if err := json.Unmarshal(review.Request.Options.Raw, u); err != nil {
				t.Errorf("Fail to deserialize options object: %s for admission request %#+v with error: %v", string(review.Request.Options.Raw), review.Request, err)
				http.Error(w, err.Error(), 400)
				return
			}
			review.Request.Options.Object = u
		}

		if review.Request.UserInfo.Username == testClientUsername {
			// only record requests originating from this integration test's client
			reviewRequest := &admissionRequest{
				Operation:   string(review.Request.Operation),
				Resource:    review.Request.Resource,
				SubResource: review.Request.SubResource,
				Namespace:   review.Request.Namespace,
				Name:        review.Request.Name,
				Object:      review.Request.Object,
				OldObject:   review.Request.OldObject,
				Options:     review.Request.Options,
			}
			holder.record("v1", phase, converted, reviewRequest)
		}

		review.Response = &admissionreviewv1.AdmissionResponse{
			Allowed: true,
			UID:     review.Request.UID,
			Result:  &metav1.Status{Message: "admitted"},

			// test plumbing warnings back
			Warnings: []string{makeWarning("v1", phase, converted)},
		}
		// If we're mutating, and have an object, return a patch to exercise conversion
		if phase == mutation && len(review.Request.Object.Raw) > 0 {
			review.Response.Patch = []byte(`[{"op":"add","path":"/bar","value":"test"}]`)
			jsonPatch := admissionreviewv1.PatchTypeJSONPatch
			review.Response.PatchType = &jsonPatch
		}

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(review); err != nil {
			t.Errorf("Marshal of response failed with error: %v", err)
		}
	})
}

func getTestFunc(gvr schema.GroupVersionResource, verb string) testFunc {
	if f, found := customTestFuncs[gvr][verb]; found {
		return f
	}
	if f, found := customTestFuncs[gvr]["*"]; found {
		return f
	}
	if strings.Contains(gvr.Resource, "/") {
		if f, found := defaultSubresourceFuncs[verb]; found {
			return f
		}
		return unimplemented
	}
	if f, found := defaultResourceFuncs[verb]; found {
		return f
	}
	return unimplemented
}

func getStubObj(gvr schema.GroupVersionResource, resource metav1.APIResource) (*unstructured.Unstructured, error) {
	stub := ""
	if data, ok := etcd.GetEtcdStorageDataForNamespace(testNamespace)[gvr]; ok {
		stub = data.Stub
	}
	if data, ok := stubDataOverrides[gvr]; ok {
		stub = data
	}
	if len(stub) == 0 {
		return nil, fmt.Errorf("no stub data for %#v", gvr)
	}

	stubObj := &unstructured.Unstructured{Object: map[string]interface{}{}}
	if err := json.Unmarshal([]byte(stub), &stubObj.Object); err != nil {
		return nil, fmt.Errorf("error unmarshaling stub for %#v: %v", gvr, err)
	}
	return stubObj, nil
}

func createOrGetResource(client dynamic.Interface, gvr schema.GroupVersionResource, resource metav1.APIResource) (*unstructured.Unstructured, error) {
	stubObj, err := getStubObj(gvr, resource)
	if err != nil {
		return nil, err
	}
	ns := ""
	if resource.Namespaced {
		ns = testNamespace
	}
	obj, err := client.Resource(gvr).Namespace(ns).Get(context.TODO(), stubObj.GetName(), metav1.GetOptions{})
	if err == nil {
		return obj, nil
	}
	if !apierrors.IsNotFound(err) {
		return nil, err
	}
	return client.Resource(gvr).Namespace(ns).Create(context.TODO(), stubObj, metav1.CreateOptions{})
}

func gvr(group, version, resource string) schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: group, Version: version, Resource: resource}
}
func gvk(group, version, kind string) schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: group, Version: version, Kind: kind}
}

var (
	gvkCreateOptions = metav1.SchemeGroupVersion.WithKind("CreateOptions")
	gvkUpdateOptions = metav1.SchemeGroupVersion.WithKind("UpdateOptions")
	gvkDeleteOptions = metav1.SchemeGroupVersion.WithKind("DeleteOptions")
)

func shouldTestResource(gvr schema.GroupVersionResource, resource metav1.APIResource) bool {
	return sets.NewString(resource.Verbs...).HasAny("create", "update", "patch", "connect", "delete", "deletecollection")
}

func shouldTestResourceVerb(gvr schema.GroupVersionResource, resource metav1.APIResource, verb string) bool {
	return sets.NewString(resource.Verbs...).Has(verb)
}

//
// webhook registration helpers
//

func createV1beta1ValidationWebhook(etcdClient *clientv3.Client, etcdStoragePrefix string, client clientset.Interface, endpoint, convertedEndpoint string, convertedRules []admissionregistrationv1beta1.RuleWithOperations) error {
	fail := admissionregistrationv1beta1.Fail
	equivalent := admissionregistrationv1beta1.Equivalent
	webhookConfig := &admissionregistrationv1beta1.ValidatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: "admission.integration.test"},
		Webhooks: []admissionregistrationv1beta1.ValidatingWebhook{
			{
				Name: "admission.integration.test",
				ClientConfig: admissionregistrationv1beta1.WebhookClientConfig{
					URL:      &endpoint,
					CABundle: localhostCert,
				},
				Rules: []admissionregistrationv1beta1.RuleWithOperations{{
					Operations: []admissionregistrationv1beta1.OperationType{admissionregistrationv1beta1.OperationAll},
					Rule:       admissionregistrationv1beta1.Rule{APIGroups: []string{"*"}, APIVersions: []string{"*"}, Resources: []string{"*/*"}},
				}},
				FailurePolicy:           &fail,
				AdmissionReviewVersions: []string{"v1beta1"},
			},
			{
				Name: "admission.integration.testconversion",
				ClientConfig: admissionregistrationv1beta1.WebhookClientConfig{
					URL:      &convertedEndpoint,
					CABundle: localhostCert,
				},
				Rules:                   convertedRules,
				FailurePolicy:           &fail,
				MatchPolicy:             &equivalent,
				AdmissionReviewVersions: []string{"v1beta1"},
			},
		},
	}
	// run through to get defaulting
	apisv1beta1.SetObjectDefaults_ValidatingWebhookConfiguration(webhookConfig)
	webhookConfig.TypeMeta.Kind = "ValidatingWebhookConfiguration"
	webhookConfig.TypeMeta.APIVersion = "admissionregistration.k8s.io/v1beta1"

	// Attaching Mutation webhook to API server
	ctx := genericapirequest.WithNamespace(genericapirequest.NewContext(), metav1.NamespaceNone)
	key := path.Join("/", etcdStoragePrefix, "validatingwebhookconfigurations", webhookConfig.Name)
	val, _ := json.Marshal(webhookConfig)
	if _, err := etcdClient.Put(ctx, key, string(val)); err != nil {
		return err
	}

	// make sure we can get the webhook
	if _, err := client.AdmissionregistrationV1().ValidatingWebhookConfigurations().Get(context.TODO(), webhookConfig.Name, metav1.GetOptions{}); err != nil {
		return err
	}

	return nil
}

func createV1beta1MutationWebhook(etcdClient *clientv3.Client, etcdStoragePrefix string, client clientset.Interface, endpoint, convertedEndpoint string, convertedRules []admissionregistrationv1beta1.RuleWithOperations) error {
	fail := admissionregistrationv1beta1.Fail
	equivalent := admissionregistrationv1beta1.Equivalent
	webhookConfig := &admissionregistrationv1beta1.MutatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: "mutation.integration.test"},
		Webhooks: []admissionregistrationv1beta1.MutatingWebhook{
			{
				Name: "mutation.integration.test",
				ClientConfig: admissionregistrationv1beta1.WebhookClientConfig{
					URL:      &endpoint,
					CABundle: localhostCert,
				},
				Rules: []admissionregistrationv1beta1.RuleWithOperations{{
					Operations: []admissionregistrationv1beta1.OperationType{admissionregistrationv1beta1.OperationAll},
					Rule:       admissionregistrationv1beta1.Rule{APIGroups: []string{"*"}, APIVersions: []string{"*"}, Resources: []string{"*/*"}},
				}},
				FailurePolicy:           &fail,
				AdmissionReviewVersions: []string{"v1beta1"},
			},
			{
				Name: "mutation.integration.testconversion",
				ClientConfig: admissionregistrationv1beta1.WebhookClientConfig{
					URL:      &convertedEndpoint,
					CABundle: localhostCert,
				},
				Rules:                   convertedRules,
				FailurePolicy:           &fail,
				MatchPolicy:             &equivalent,
				AdmissionReviewVersions: []string{"v1beta1"},
			},
		},
	}
	// run through to get defaulting
	apisv1beta1.SetObjectDefaults_MutatingWebhookConfiguration(webhookConfig)
	webhookConfig.TypeMeta.Kind = "MutatingWebhookConfiguration"
	webhookConfig.TypeMeta.APIVersion = "admissionregistration.k8s.io/v1beta1"

	// Attaching Mutation webhook to API server
	ctx := genericapirequest.WithNamespace(genericapirequest.NewContext(), metav1.NamespaceNone)
	key := path.Join("/", etcdStoragePrefix, "mutatingwebhookconfigurations", webhookConfig.Name)
	val, _ := json.Marshal(webhookConfig)
	if _, err := etcdClient.Put(ctx, key, string(val)); err != nil {
		return err
	}

	// make sure we can get the webhook
	if _, err := client.AdmissionregistrationV1().MutatingWebhookConfigurations().Get(context.TODO(), webhookConfig.Name, metav1.GetOptions{}); err != nil {
		return err
	}

	return nil
}

func createV1ValidationWebhook(client clientset.Interface, endpoint, convertedEndpoint string, convertedRules []admissionregistrationv1.RuleWithOperations) error {
	fail := admissionregistrationv1.Fail
	equivalent := admissionregistrationv1.Equivalent
	none := admissionregistrationv1.SideEffectClassNone
	// Attaching Admission webhook to API server
	_, err := client.AdmissionregistrationV1().ValidatingWebhookConfigurations().Create(context.TODO(), &admissionregistrationv1.ValidatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: "admissionregistrationv1.integration.test"},
		Webhooks: []admissionregistrationv1.ValidatingWebhook{
			{
				Name: "admissionregistrationv1.integration.test",
				ClientConfig: admissionregistrationv1.WebhookClientConfig{
					URL:      &endpoint,
					CABundle: localhostCert,
				},
				Rules: []admissionregistrationv1.RuleWithOperations{{
					Operations: []admissionregistrationv1.OperationType{admissionregistrationv1.OperationAll},
					Rule:       admissionregistrationv1.Rule{APIGroups: []string{"*"}, APIVersions: []string{"*"}, Resources: []string{"*/*"}},
				}},
				FailurePolicy:           &fail,
				AdmissionReviewVersions: []string{"v1", "v1beta1"},
				SideEffects:             &none,
			},
			{
				Name: "admissionregistrationv1.integration.testconversion",
				ClientConfig: admissionregistrationv1.WebhookClientConfig{
					URL:      &convertedEndpoint,
					CABundle: localhostCert,
				},
				Rules:                   convertedRules,
				FailurePolicy:           &fail,
				MatchPolicy:             &equivalent,
				AdmissionReviewVersions: []string{"v1", "v1beta1"},
				SideEffects:             &none,
			},
		},
	}, metav1.CreateOptions{})
	return err
}

func createV1MutationWebhook(client clientset.Interface, endpoint, convertedEndpoint string, convertedRules []admissionregistrationv1.RuleWithOperations) error {
	fail := admissionregistrationv1.Fail
	equivalent := admissionregistrationv1.Equivalent
	none := admissionregistrationv1.SideEffectClassNone
	// Attaching Mutation webhook to API server
	_, err := client.AdmissionregistrationV1().MutatingWebhookConfigurations().Create(context.TODO(), &admissionregistrationv1.MutatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: "mutationv1.integration.test"},
		Webhooks: []admissionregistrationv1.MutatingWebhook{
			{
				Name: "mutationv1.integration.test",
				ClientConfig: admissionregistrationv1.WebhookClientConfig{
					URL:      &endpoint,
					CABundle: localhostCert,
				},
				Rules: []admissionregistrationv1.RuleWithOperations{{
					Operations: []admissionregistrationv1.OperationType{admissionregistrationv1.OperationAll},
					Rule:       admissionregistrationv1.Rule{APIGroups: []string{"*"}, APIVersions: []string{"*"}, Resources: []string{"*/*"}},
				}},
				FailurePolicy:           &fail,
				AdmissionReviewVersions: []string{"v1", "v1beta1"},
				SideEffects:             &none,
			},
			{
				Name: "mutationv1.integration.testconversion",
				ClientConfig: admissionregistrationv1.WebhookClientConfig{
					URL:      &convertedEndpoint,
					CABundle: localhostCert,
				},
				Rules:                   convertedRules,
				FailurePolicy:           &fail,
				MatchPolicy:             &equivalent,
				AdmissionReviewVersions: []string{"v1", "v1beta1"},
				SideEffects:             &none,
			},
		},
	}, metav1.CreateOptions{})
	return err
}

// localhostCert was generated from crypto/tls/generate_cert.go with the following command:
//
//	go run generate_cert.go  --rsa-bits 2048 --host 127.0.0.1,::1,example.com,webhook.test.svc --ca --start-date "Jan 1 00:00:00 1970" --duration=1000000h
var localhostCert = []byte(`-----BEGIN CERTIFICATE-----
MIIDTDCCAjSgAwIBAgIRAJXp/H5o/ItwCEK9emP3NiMwDQYJKoZIhvcNAQELBQAw
EjEQMA4GA1UEChMHQWNtZSBDbzAgFw03MDAxMDEwMDAwMDBaGA8yMDg0MDEyOTE2
MDAwMFowEjEQMA4GA1UEChMHQWNtZSBDbzCCASIwDQYJKoZIhvcNAQEBBQADggEP
ADCCAQoCggEBAOCyQ/2e9SVZ3QSW1yxe9OoZeyX7N8jRRyRkWlSL/OiEIxGsDJHK
GcDrGONOm9FeKM73evSiNX+7AZEqdanT37RsvVHTbRKAKsNIilyFTYmSvPHC05iG
agcIBm/Wt+NvfNb3DFLPhCLZbeuqlKhMzc8NeWHNY6eJj1qqks70PNlcb3Q5Ufa2
ttxs3N4pUmi7/ntiFE+X42A6IGX94Zyu9E7kH+0/ajvEA0qAyIXp1TneMgybS+ox
UBLDBQvsOH5lwvVIUfJLI483geXbFaUpHc6fTKE/8/f6EuWWEN3UFvuDM6cqr51e
MPTziUVUs5NBIeHIGyTKTbF3+gTXFKDf/jECAwEAAaOBmjCBlzAOBgNVHQ8BAf8E
BAMCAqQwEwYDVR0lBAwwCgYIKwYBBQUHAwEwDwYDVR0TAQH/BAUwAwEB/zAdBgNV
HQ4EFgQURFTsa1/pfERE/WJ3YpkbnKI6NkEwQAYDVR0RBDkwN4ILZXhhbXBsZS5j
b22CEHdlYmhvb2sudGVzdC5zdmOHBH8AAAGHEAAAAAAAAAAAAAAAAAAAAAEwDQYJ
KoZIhvcNAQELBQADggEBAE60cASylHw0DsHtTkQwjhmW0Bd1Dy0+BvGngD9P85tB
fNHtcurzGG1GSGVX7ClxghDZo84WcV742qenxBlZ37WTqmD5/4pWlEvbrjKmgr3W
yWM6WJts1W4T5aR6mU2jHz1mxIFq9Fcw2XcdtwHAJKoCKpLv6pYswW4LYODdKNii
eAKBEcbEBQ3oU4529yeDpkU6ZLBKH+ZVxWI3ZUWbpv5O6vMtSB9nvtTripbWrm1t
vpCEETNAOP2hbLnPwBXUEN8KBs94UdufOFIhArNgKonY/oZoZnZYWVyRtkex+b+r
MarmcIKMrgoYweSQiCa+XVWofz2ZSOvzxta6Y9iDI74=
-----END CERTIFICATE-----`)

// localhostKey is the private key for localhostCert.
var localhostKey = []byte(`-----BEGIN RSA PRIVATE KEY-----
MIIEvQIBADANBgkqhkiG9w0BAQEFAASCBKcwggSjAgEAAoIBAQDgskP9nvUlWd0E
ltcsXvTqGXsl+zfI0UckZFpUi/zohCMRrAyRyhnA6xjjTpvRXijO93r0ojV/uwGR
KnWp09+0bL1R020SgCrDSIpchU2JkrzxwtOYhmoHCAZv1rfjb3zW9wxSz4Qi2W3r
qpSoTM3PDXlhzWOniY9aqpLO9DzZXG90OVH2trbcbNzeKVJou/57YhRPl+NgOiBl
/eGcrvRO5B/tP2o7xANKgMiF6dU53jIMm0vqMVASwwUL7Dh+ZcL1SFHySyOPN4Hl
2xWlKR3On0yhP/P3+hLllhDd1Bb7gzOnKq+dXjD084lFVLOTQSHhyBskyk2xd/oE
1xSg3/4xAgMBAAECggEAbykB5ejL0oyggPK2xKa9d0rf16xurpSKI4DaB1Wx6r3k
M4vwM/fNwdkM2Pc8stloSuu4EmplGSnE3rIov7mnxDS/fEmifjKV9UJf4OG5uEO1
4czGrYBh19Sqio2pL4UqN5bEq/spnav/a0VageBtOO+riyz3Dh1JpEsakfPWXpkk
gZ7Vl/jZ4zU27/LMfIqngOPeAGiUkLGikM6fPvm/4PbvgnSCZ4mhOSyzgCLmAWKi
Kr8zCD7BJk62/BUogk3qim+uW4Sf3RvZACTBWq6ZhWNeU2Z3CHI4G8p8sl7jtmPR
a1BWSV8Lf+83VFCfk/O+oSdb0f2z/RBAZ6uV9ZtHoQKBgQDikFsRxgXPXllSlytI
QU//19Z4S7dqWqFOX6+ap1aSyj01IsN1kvZzyGZ6ZyyAPUrNheokccijkXgooBHL
aLMxa4v0i/pHGcXAFbzIlzKwkmi0zIy7nX6cSIg2cg0sKWDGVxxJ4ODxFJRyd6Vq
Pao4/L+nUPVMRi2ME2iYe/qp/QKBgQD948teuZ4lEGTZx5IhmBpNuj45C8y5sd4W
vy+oFK8aOoTl4nCscYAAVXnS+CxInpQHI35GYRIDdjk2IL8eFThtsB+wS//Cd7h8
yY0JZC+XWhWPG5U+dSkSyzVsaK9jDJFRcnfnvHqO2+masyeq9FFTo8gX6KpF8wDL
97+UFz3xRQKBgQDa7ygx2quOodurBc2bexG1Z3smr/RD3+R0ed6VkhMEsk3HZRqA
KU3iwMrWiZDlM1VvmXKTWSjLdy0oBNZtO3W90fFilUl7H5qKbfcJ16HyIujvnaJ5
Qk4w8549DqVQAYQ05cS+V4LHNF3m51t/eKtfek4xfvgrhr1I2RCAGX42eQKBgFOw
miIgZ4vqKoRLL9VZERqcENS3GgYAJqgy31+1ab7omVQ531BInZv+kQjE+7v4Ye00
evRyHQD9IIDCLJ2a+x3VF60CcE1HL44a1h3JY5KthDvHKNwMvLxQNc0FeQLaarCB
XhsKWw/qV8fB1IqavJAohdWzwSULpDCX+xOy0Z1NAoGAPXGRPSw0p0b8zHuJ6SmM
blkpX9rdFMN08MJYIBG+ZiRobU+OOvClBZiDpYHpBnFCFpsXiStSYKOBrAAypC01
UFJJZe7Tfz1R4VcexsS3yfXOZV/+9t/PnyFofSBB8wf/dokhgfEOYq8rbiunHFVT
20/b/zX8pbSiK6Kgy9vIm7w=
-----END RSA PRIVATE KEY-----`)
