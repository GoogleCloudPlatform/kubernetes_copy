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

package apiserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path"
	"reflect"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	v1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/version"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apiserver/pkg/authentication/serviceaccount"
	"k8s.io/apiserver/pkg/authentication/user"
	genericapirequest "k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/features"
	"k8s.io/apiserver/pkg/server/dynamiccertificates"
	genericapiserveroptions "k8s.io/apiserver/pkg/server/options"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	client "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"k8s.io/client-go/transport"
	"k8s.io/client-go/util/cert"
	basecompatibility "k8s.io/component-base/compatibility"
	"k8s.io/component-base/featuregate"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	apiregistrationv1 "k8s.io/kube-aggregator/pkg/apis/apiregistration/v1"
	aggregatorclient "k8s.io/kube-aggregator/pkg/client/clientset_generated/clientset"
	"k8s.io/kubernetes/cmd/kube-apiserver/app"
	kastesting "k8s.io/kubernetes/cmd/kube-apiserver/app/testing"
	"k8s.io/kubernetes/test/integration"
	"k8s.io/kubernetes/test/integration/framework"
	wardlev1alpha1 "k8s.io/sample-apiserver/pkg/apis/wardle/v1alpha1"
	wardlev1beta1 "k8s.io/sample-apiserver/pkg/apis/wardle/v1beta1"
	"k8s.io/sample-apiserver/pkg/apiserver"
	sampleserver "k8s.io/sample-apiserver/pkg/cmd/server"
	wardlev1alpha1client "k8s.io/sample-apiserver/pkg/generated/clientset/versioned/typed/wardle/v1alpha1"
	netutils "k8s.io/utils/net"
)

func TestAPIServiceWaitOnStart(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	t.Cleanup(cancel)

	etcdConfig := framework.SharedEtcd()

	etcd3Client, _, err := integration.GetEtcdClients(etcdConfig.Transport)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { etcd3Client.Close() })

	t.Log("Pollute CRD path in etcd so CRD lists cannot succeed and the informer cannot sync")
	bogusCRDEtcdPath := path.Join("/", etcdConfig.Prefix, "apiextensions.k8s.io/customresourcedefinitions/bogus")
	if _, err := etcd3Client.KV.Put(ctx, bogusCRDEtcdPath, `bogus data`); err != nil {
		t.Fatal(err)
	}

	t.Log("Populate a valid CRD and managed APIService in etcd")
	if _, err := etcd3Client.KV.Put(
		ctx,
		path.Join("/", etcdConfig.Prefix, "apiextensions.k8s.io/customresourcedefinitions/widgets.valid.example.com"),
		`{
			"apiVersion":"apiextensions.k8s.io/v1beta1",
			"kind":"CustomResourceDefinition",
			"metadata":{
				"name":"widgets.valid.example.com",
				"uid":"mycrd",
				"creationTimestamp": "2022-06-08T23:46:32Z"
			},
			"spec":{
				"scope": "Namespaced",
				"group":"valid.example.com",
				"version":"v1",
				"names":{
					"kind": "Widget",
					"listKind": "WidgetList",
					"plural": "widgets",
					"singular": "widget"
				}
			},
			"status": {
				"acceptedNames": {
					"kind": "Widget",
					"listKind": "WidgetList",
					"plural": "widgets",
					"singular": "widget"
				},
				"conditions": [
					{
						"lastTransitionTime": "2023-05-18T15:03:57Z",
						"message": "no conflicts found",
						"reason": "NoConflicts",
						"status": "True",
						"type": "NamesAccepted"
					},
					{
						"lastTransitionTime": "2023-05-18T15:03:57Z",
						"message": "the initial names have been accepted",
						"reason": "InitialNamesAccepted",
						"status": "True",
						"type": "Established"
					}
				],
				"storedVersions": [
					"v1"
				]
			}
		}`); err != nil {
		t.Fatal(err)
	}
	if _, err := etcd3Client.KV.Put(
		ctx,
		path.Join("/", etcdConfig.Prefix, "apiregistration.k8s.io/apiservices/v1.valid.example.com"),
		`{
				"apiVersion":"apiregistration.k8s.io/v1",
				"kind":"APIService",
				"metadata": {
					"name": "v1.valid.example.com",
					"uid":"foo",
					"creationTimestamp": "2022-06-08T23:46:32Z",
					"labels":{"kube-aggregator.kubernetes.io/automanaged":"true"}
				},
				"spec": {
					"group": "valid.example.com",
					"version": "v1",
					"groupPriorityMinimum":100,
					"versionPriority":10
				}
			}`,
	); err != nil {
		t.Fatal(err)
	}

	t.Log("Populate a stale managed APIService in etcd")
	if _, err := etcd3Client.KV.Put(
		ctx,
		path.Join("/", etcdConfig.Prefix, "apiregistration.k8s.io/apiservices/v1.stale.example.com"),
		`{
			"apiVersion":"apiregistration.k8s.io/v1",
			"kind":"APIService",
			"metadata": {
				"name": "v1.stale.example.com",
				"uid":"foo",
				"creationTimestamp": "2022-06-08T23:46:32Z",
				"labels":{"kube-aggregator.kubernetes.io/automanaged":"true"}
			},
			"spec": {
				"group": "stale.example.com",
				"version": "v1",
				"groupPriorityMinimum":100,
				"versionPriority":10
			}
		}`,
	); err != nil {
		t.Fatal(err)
	}

	t.Log("Starting server")
	options := kastesting.NewDefaultTestServerOptions()
	options.SkipHealthzCheck = true
	testServer := kastesting.StartTestServerOrDie(t, options, nil, etcdConfig)
	defer testServer.TearDownFn()

	kubeClientConfig := rest.CopyConfig(testServer.ClientConfig)
	aggregatorClient := aggregatorclient.NewForConfigOrDie(kubeClientConfig)

	t.Log("Ensure both APIService objects remain")
	for i := 0; i < 10; i++ {
		if _, err := aggregatorClient.ApiregistrationV1().APIServices().Get(ctx, "v1.valid.example.com", metav1.GetOptions{}); err != nil {
			t.Fatal(err)
		}
		if _, err := aggregatorClient.ApiregistrationV1().APIServices().Get(ctx, "v1.stale.example.com", metav1.GetOptions{}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Second)
	}

	t.Log("Clear the bogus CRD data so the informer can sync")
	if _, err := etcd3Client.KV.Delete(ctx, bogusCRDEtcdPath); err != nil {
		t.Fatal(err)
	}

	t.Log("Ensure the stale APIService object is cleaned up")
	if err := wait.Poll(time.Second, wait.ForeverTestTimeout, func() (bool, error) {
		_, err := aggregatorClient.ApiregistrationV1().APIServices().Get(ctx, "v1.stale.example.com", metav1.GetOptions{})
		if err == nil {
			t.Log("stale APIService still exists, waiting...")
			return false, nil
		}
		if !apierrors.IsNotFound(err) {
			return false, err
		}
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}

	t.Log("Ensure the valid APIService object remains")
	for i := 0; i < 5; i++ {
		time.Sleep(time.Second)
		if _, err := aggregatorClient.ApiregistrationV1().APIServices().Get(ctx, "v1.valid.example.com", metav1.GetOptions{}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestAggregatedAPIServer(t *testing.T) {
	// Testing default, BanFlunder default=true in 1.2
	t.Run("WithoutWardleFeatureGateAtV1.2", func(t *testing.T) {
		testAggregatedAPIServer(t, false, true, "1.2", "1.2")
	})
	// Testing emulation version N, BanFlunder default=true in 1.1
	t.Run("WithoutWardleFeatureGateAtV1.1", func(t *testing.T) {
		testAggregatedAPIServer(t, false, true, "1.1", "1.1")
	})
	// Testing emulation version N-1, BanFlunder default=false in 1.0
	t.Run("WithoutWardleFeatureGateAtV1.0", func(t *testing.T) {
		testAggregatedAPIServer(t, false, false, "1.1", "1.0")
	})
	// Testing emulation version N-1, Explicitly set BanFlunder=true in 1.0
	t.Run("WithWardleFeatureGateAtV1.0", func(t *testing.T) {
		testAggregatedAPIServer(t, true, true, "1.1", "1.0")
	})
}

func TestFrontProxyConfig(t *testing.T) {
	t.Run("WithoutUID", func(t *testing.T) {
		testFrontProxyConfig(t, false)
	})
	t.Run("WithUID", func(t *testing.T) {
		featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.RemoteRequestHeaderUID, true)
		testFrontProxyConfig(t, true)
	})
}

// TestFrontProxyConfig tests that the RequestHeader configuration is consumed
// correctly by the aggregated API servers.
func testFrontProxyConfig(t *testing.T, withUID bool) {
	const testNamespace = "integration-test-front-proxy-config"
	const wardleBinaryVersion = "1.1"

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	t.Cleanup(cancel)

	var extraKASFlags []string
	if withUID {
		extraKASFlags = []string{"--requestheader-uid-headers=x-remote-uid"}
	}

	// start up the KAS and prepare the options for the wardle API server
	testKAS, wardleOptions, wardlePort := prepareAggregatedWardleAPIServer(ctx, t, testNamespace, wardleBinaryVersion, extraKASFlags, withUID)
	kubeConfig := getKubeConfig(testKAS)

	// create the SA that we will use to query the aggregated API
	kubeClient := client.NewForConfigOrDie(kubeConfig)
	expectedSA, err := kubeClient.CoreV1().ServiceAccounts(testNamespace).Create(ctx, &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name: "wardle-client-sa",
		},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}

	saTokenReq, err := kubeClient.CoreV1().ServiceAccounts(testNamespace).CreateToken(ctx, "wardle-client-sa", &v1.TokenRequest{}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	saToken := saTokenReq.Status.Token
	if len(saToken) == 0 {
		t.Fatal("empty SA token in token request response")
	}

	saClientConfig := rest.AnonymousClientConfig(kubeConfig)
	saClientConfig.BearerToken = saToken

	saKubeClient := client.NewForConfigOrDie(saClientConfig)
	saDetails, err := saKubeClient.AuthenticationV1().SelfSubjectReviews().Create(ctx, &v1.SelfSubjectReview{}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("failed to retrieve details about the SA: %v", err)
	}

	saUserInfo := serviceaccount.UserInfo(expectedSA.Namespace, expectedSA.Name, string(expectedSA.UID))
	expectedSAUserInfo := user.DefaultInfo{
		Name:   saUserInfo.GetName(),
		Groups: append(saUserInfo.GetGroups(), user.AllAuthenticated),
		Extra:  saUserInfo.GetExtra(),
	}
	if withUID {
		expectedSAUserInfo.UID = saUserInfo.GetUID()
	}

	if expectedSAUserInfo.Extra == nil {
		expectedSAUserInfo.Extra = map[string][]string{}
	}
	expectedSAUserInfo.Extra[user.CredentialIDKey] = saDetails.Status.UserInfo.Extra[user.CredentialIDKey]

	var checksProcessed atomic.Uint32

	// wrap the authz round tripper to catch the request for our SA SAR to the KAS
	wardleOptions.RecommendedOptions.Authorization.WithCustomRoundTripper(
		// adding a round tripper wrapper to test default RequestHeader configuration
		transport.WrapperFunc(func(rt http.RoundTripper) http.RoundTripper {
			return roundTripperFunc(func(req *http.Request) (*http.Response, error) {
				gotUser, ok := genericapirequest.UserFrom(req.Context())
				if !ok {
					return nil, fmt.Errorf("got an unauthenticated request")
				}

				// this is likely the KAS checking the OpenAPI endpoints
				if gotUser.GetName() == "system:anonymous" || gotUser.GetName() == "system:aggregator" || gotUser.GetName() == "system:kube-aggregator" {
					return rt.RoundTrip(req)
				}

				if got, expected := gotUser.GetUID(), expectedSAUserInfo.GetUID(); expected != got {
					t.Errorf("expected UID: %q, got: %q", expected, got)
				}
				if got, expected := gotUser.GetName(), expectedSAUserInfo.GetName(); expected != got {
					t.Errorf("expected name: %q, got: %q", expected, got)
				}
				if got, expected := gotUser.GetGroups(), expectedSAUserInfo.GetGroups(); !reflect.DeepEqual(expected, got) {
					t.Errorf("expected groups: %v, got: %v", expected, got)
				}
				if got, expected := gotUser.GetExtra(), expectedSAUserInfo.GetExtra(); !apiequality.Semantic.DeepEqual(expected, got) {
					t.Errorf("expected extra to be %v, but got %v", expected, got)
				}

				checksProcessed.Add(1)
				return rt.RoundTrip(req)
			})
		}),
	)

	wardleCertDir, _ := os.MkdirTemp("", "test-integration-wardle-server")
	defer os.RemoveAll(wardleCertDir)

	runPreparedWardleServer(ctx, t, wardleOptions, wardleCertDir, wardlePort, false, true, wardleBinaryVersion, kubeConfig, withUID)
	waitForWardleAPIServiceReady(ctx, t, kubeConfig, wardleCertDir, testNamespace)

	// get the wardle API client using our SA token
	wardleClientConfig := rest.AnonymousClientConfig(kubeConfig)
	wardleClientConfig.BearerToken = saToken
	wardleClient := wardlev1alpha1client.NewForConfigOrDie(wardleClientConfig)

	_, err = wardleClient.Flunders(metav1.NamespaceSystem).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}

	if checksProcessed.Load() != 1 {
		t.Errorf("the request is in fact not being tested")
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func testAggregatedAPIServer(t *testing.T, setWardleFeatureGate, banFlunder bool, wardleBinaryVersion, wardleEmulationVersion string) {
	const testNamespace = "kube-wardle"

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	t.Cleanup(cancel)

	testKAS, wardleOptions, wardlePort := prepareAggregatedWardleAPIServer(ctx, t, testNamespace, wardleBinaryVersion, nil, false)
	kubeClientConfig := getKubeConfig(testKAS)

	wardleCertDir, _ := os.MkdirTemp("", "test-integration-wardle-server")
	defer os.RemoveAll(wardleCertDir)

	directWardleClientConfig := runPreparedWardleServer(ctx, t, wardleOptions, wardleCertDir, wardlePort, setWardleFeatureGate, banFlunder, wardleEmulationVersion, kubeClientConfig, false)

	// now we're finally ready to test. These are what's run by default now
	wardleDirectClient := client.NewForConfigOrDie(directWardleClientConfig)
	testAPIGroupList(ctx, t, wardleDirectClient.Discovery().RESTClient())
	testAPIGroup(ctx, t, wardleDirectClient.Discovery().RESTClient())
	testAPIResourceList(ctx, t, wardleDirectClient.Discovery().RESTClient())

	wardleClient := wardlev1alpha1client.NewForConfigOrDie(kubeClientConfig)

	waitForWardleAPIServiceReady(ctx, t, kubeClientConfig, wardleCertDir, testNamespace)

	// perform simple CRUD operations against the wardle resources
	_, err := wardleClient.Fischers().Create(ctx, &wardlev1alpha1.Fischer{
		ObjectMeta: metav1.ObjectMeta{
			Name: "panda",
		},
		DisallowedFlunders: []string{"badname"},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// clean up data after test is done
	defer wardleClient.Fischers().Delete(ctx, "panda", metav1.DeleteOptions{})
	fischersList, err := wardleClient.Fischers().List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(fischersList.Items) != 1 {
		t.Errorf("expected one fischer: %#v", fischersList.Items)
	}
	if len(fischersList.ResourceVersion) == 0 {
		t.Error("expected non-empty resource version for fischer list")
	}

	_, err = wardleClient.Flunders(metav1.NamespaceSystem).Create(ctx, &wardlev1alpha1.Flunder{
		ObjectMeta: metav1.ObjectMeta{
			Name: "badname",
		},
	}, metav1.CreateOptions{})
	if banFlunder && err == nil {
		t.Fatal("expect flunder:badname not admitted when wardle feature gates are specified")
	}
	if !banFlunder {
		if err != nil {
			t.Fatal("expect flunder:badname admitted when wardle feature gates are not specified")
		} else {
			defer wardleClient.Flunders(metav1.NamespaceSystem).Delete(ctx, "badname", metav1.DeleteOptions{})
		}
	}
	_, err = wardleClient.Flunders(metav1.NamespaceSystem).Create(ctx, &wardlev1alpha1.Flunder{
		ObjectMeta: metav1.ObjectMeta{
			Name: "panda",
		},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer wardleClient.Flunders(metav1.NamespaceSystem).Delete(ctx, "panda", metav1.DeleteOptions{})
	flunderList, err := wardleClient.Flunders(metav1.NamespaceSystem).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	expectedFlunderCount := 2
	if banFlunder {
		expectedFlunderCount = 1
	}
	if len(flunderList.Items) != expectedFlunderCount {
		t.Errorf("expected %d flunder: %#v", expectedFlunderCount, flunderList.Items)
	}
	if len(flunderList.ResourceVersion) == 0 {
		t.Error("expected non-empty resource version for flunder list")
	}

	// Since ClientCAs are provided by "client-ca::kube-system::extension-apiserver-authentication::client-ca-file" controller
	// we need to wait until it picks up the configmap (via a lister) otherwise the response might contain an empty result.
	// The following code waits up to ForeverTestTimeout seconds for ClientCA to show up otherwise it fails
	// maybe in the future this could be wired into the /readyz EP

	// Now we want to verify that the client CA bundles properly reflect the values for the cluster-authentication
	var firstKubeCANames []string
	err = wait.Poll(1*time.Second, wait.ForeverTestTimeout, func() (done bool, err error) {
		firstKubeCANames, err = cert.GetClientCANamesForURL(kubeClientConfig.Host)
		if err != nil {
			return false, err
		}
		return len(firstKubeCANames) != 0, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Log(firstKubeCANames)
	var firstWardleCANames []string
	err = wait.Poll(1*time.Second, wait.ForeverTestTimeout, func() (done bool, err error) {
		firstWardleCANames, err = cert.GetClientCANamesForURL(directWardleClientConfig.Host)
		if err != nil {
			return false, err
		}
		return len(firstWardleCANames) != 0, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Log(firstWardleCANames)
	// Now we want to verify that the client CA bundles properly reflect the values for the cluster-authentication
	if !reflect.DeepEqual(firstKubeCANames, firstWardleCANames) {
		t.Fatal("names don't match")
	}

	// now we update the client-ca nd request-header-client-ca-file and the kas will consume it, update the configmap
	// and then the wardle server will detect and update too.
	if err := os.WriteFile(path.Join(testKAS.TmpDir, "client-ca.crt"), differentClientCA, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path.Join(testKAS.TmpDir, "proxy-ca.crt"), differentFrontProxyCA, 0644); err != nil {
		t.Fatal(err)
	}
	// wait for it to be picked up.  there's a test in certreload_test.go that ensure this works
	time.Sleep(4 * time.Second)

	// Now we want to verify that the client CA bundles properly updated to reflect the new values written for the kube-apiserver
	secondKubeCANames, err := cert.GetClientCANamesForURL(kubeClientConfig.Host)
	if err != nil {
		t.Fatal(err)
	}
	t.Log(secondKubeCANames)
	for i := range firstKubeCANames {
		if firstKubeCANames[i] == secondKubeCANames[i] {
			t.Errorf("ca bundles should change")
		}
	}
	secondWardleCANames, err := cert.GetClientCANamesForURL(directWardleClientConfig.Host)
	if err != nil {
		t.Fatal(err)
	}
	t.Log(secondWardleCANames)

	// second wardle should contain all the certs, first and last
	numMatches := 0
	for _, needle := range firstKubeCANames {
		for _, haystack := range secondWardleCANames {
			if needle == haystack {
				numMatches++
				break
			}
		}
	}
	for _, needle := range secondKubeCANames {
		for _, haystack := range secondWardleCANames {
			if needle == haystack {
				numMatches++
				break
			}
		}
	}
	if numMatches != 4 {
		t.Fatal("names don't match")
	}
}

func TestAggregatedAPIServerRejectRedirectResponse(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	t.Cleanup(cancel)

	backendServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		if strings.HasSuffix(r.URL.Path, "redirectTarget") {
			t.Errorf("backend called unexpectedly")
		}
	}))
	defer backendServer.Close()

	redirectedURL := backendServer.URL
	redirectServer := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "tryRedirect") {
			http.Redirect(w, r, redirectedURL+"/redirectTarget", http.StatusMovedPermanently)
		} else {
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer redirectServer.Close()

	// endpoints cannot have loopback IPs so we need to override the resolver itself
	t.Cleanup(app.SetServiceResolverForTests(staticURLServiceResolver(fmt.Sprintf("https://%s", redirectServer.Listener.Addr().String()))))

	// start the server after resolver is overwritten
	redirectServer.StartTLS()

	testServer := kastesting.StartTestServerOrDie(t, &kastesting.TestServerInstanceOptions{EnableCertAuth: false}, nil, framework.SharedEtcd())
	defer testServer.TearDownFn()
	kubeClientConfig := rest.CopyConfig(testServer.ClientConfig)
	// force json because everything speaks it
	kubeClientConfig.ContentType = ""
	kubeClientConfig.AcceptContentTypes = ""
	kubeClient := client.NewForConfigOrDie(kubeClientConfig)
	aggregatorClient := aggregatorclient.NewForConfigOrDie(kubeClientConfig)

	// create the bare minimum resources required to be able to get the API service into an available state
	_, err := kubeClient.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "kube-redirect",
		},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = kubeClient.CoreV1().Services("kube-redirect").Create(ctx, &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: "api",
		},
		Spec: corev1.ServiceSpec{
			ExternalName: "needs-to-be-non-empty",
			Type:         corev1.ServiceTypeExternalName,
		},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}

	_, err = aggregatorClient.ApiregistrationV1().APIServices().Create(ctx, &apiregistrationv1.APIService{
		ObjectMeta: metav1.ObjectMeta{Name: "v1alpha1.reject.redirect.example.com"},
		Spec: apiregistrationv1.APIServiceSpec{
			Service: &apiregistrationv1.ServiceReference{
				Namespace: "kube-redirect",
				Name:      "api",
			},
			Group:                 "reject.redirect.example.com",
			Version:               "v1alpha1",
			GroupPriorityMinimum:  200,
			VersionPriority:       200,
			InsecureSkipTLSVerify: true,
		},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}

	// wait for the API service to be available
	err = wait.Poll(time.Second, wait.ForeverTestTimeout, func() (done bool, err error) {
		apiService, err := aggregatorClient.ApiregistrationV1().APIServices().Get(ctx, "v1alpha1.reject.redirect.example.com", metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		var available bool
		for _, condition := range apiService.Status.Conditions {
			if condition.Type == apiregistrationv1.Available && condition.Status == apiregistrationv1.ConditionTrue {
				available = true
				break
			}
		}
		if !available {
			t.Log("api service is not available", apiService.Status.Conditions)
			return false, nil
		}
		return available, nil
	})
	if err != nil {
		t.Errorf("%v", err)
	}

	// get raw response to check the original error and msg
	expectedMsg := "the backend attempted to redirect this request, which is not permitted"
	// add specific request path suffix to discriminate between request from client and generic pings from the aggregator
	url := url.URL{
		Path: "/apis/reject.redirect.example.com/v1alpha1/tryRedirect",
	}
	bytes, err := kubeClient.RESTClient().Get().AbsPath(url.String()).DoRaw(context.TODO())
	if err == nil {
		t.Errorf("expect server to reject redirect response, but forwarded")
	} else if !strings.Contains(string(bytes), expectedMsg) {
		t.Errorf("expect response contains %s, got %s", expectedMsg, string(bytes))
	}
}

func prepareAggregatedWardleAPIServer(ctx context.Context, t *testing.T, namespace, wardleBinaryVersion string, kubeAPIServerFlags []string, withUID bool) (*kastesting.TestServer, *sampleserver.WardleServerOptions, int) {
	// makes the kube-apiserver very responsive.  it's normally a minute
	dynamiccertificates.FileRefreshDuration = 1 * time.Second

	// we need the wardle port information first to set up the service resolver
	listener, wardlePort, err := genericapiserveroptions.CreateListener("tcp", "127.0.0.1:0", net.ListenConfig{})
	if err != nil {
		t.Fatal(err)
	}
	// endpoints cannot have loopback IPs so we need to override the resolver itself
	t.Cleanup(app.SetServiceResolverForTests(staticURLServiceResolver(fmt.Sprintf("https://127.0.0.1:%d", wardlePort))))

	testServer := kastesting.StartTestServerOrDie(t,
		&kastesting.TestServerInstanceOptions{
			EnableCertAuth: true,
		},
		kubeAPIServerFlags,
		framework.SharedEtcd())
	t.Cleanup(func() { testServer.TearDownFn() })

	componentGlobalsRegistry := testServer.ServerOpts.Options.GenericServerRunOptions.ComponentGlobalsRegistry
	_, _ = componentGlobalsRegistry.ComponentGlobalsOrRegister(
		apiserver.WardleComponentName, basecompatibility.NewEffectiveVersionFromString(wardleBinaryVersion, "", ""),
		featuregate.NewVersionedFeatureGate(version.MustParse(wardleBinaryVersion)))

	kubeClient := client.NewForConfigOrDie(getKubeConfig(testServer))

	// create the bare minimum resources required to be able to get the API service into an available state
	_, err = kubeClient.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: namespace,
		},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = kubeClient.CoreV1().Services(namespace).Create(ctx, &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: "api",
		},
		Spec: corev1.ServiceSpec{
			ExternalName: "needs-to-be-non-empty",
			Type:         corev1.ServiceTypeExternalName,
		},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}

	wardleOptions := sampleserver.NewWardleServerOptions(os.Stdout, os.Stderr)
	wardleOptions.ComponentGlobalsRegistry = componentGlobalsRegistry
	// ensure this is a SAN on the generated cert for service FQDN
	wardleOptions.AlternateDNS = []string{
		fmt.Sprintf("api.%s.svc", namespace),
	}
	wardleOptions.RecommendedOptions.SecureServing.Listener = listener
	wardleOptions.RecommendedOptions.SecureServing.BindAddress = netutils.ParseIPSloppy("127.0.0.1")

	return testServer, wardleOptions, wardlePort
}

func runPreparedWardleServer(
	ctx context.Context,
	t *testing.T,
	wardleOptions *sampleserver.WardleServerOptions,
	certDir string,
	wardlePort int,
	flunderBanningFeatureGate bool,
	banFlunder bool,
	emulationVersion string,
	kubeConfig *rest.Config,
	withUID bool,
) *rest.Config {

	// start the wardle server to prove we can aggregate it
	wardleToKASKubeConfigFile := writeKubeConfigForWardleServerToKASConnection(t, rest.CopyConfig(kubeConfig))
	t.Cleanup(func() { os.Remove(wardleToKASKubeConfigFile) })

	go func() {
		args := []string{
			"--authentication-kubeconfig", wardleToKASKubeConfigFile,
			"--authorization-kubeconfig", wardleToKASKubeConfigFile,
			"--etcd-servers", framework.GetEtcdURL(),
			"--cert-dir", certDir,
			"--kubeconfig", wardleToKASKubeConfigFile,
			"--emulated-version", fmt.Sprintf("wardle=%s", emulationVersion),
		}
		if flunderBanningFeatureGate {
			args = append(args, "--feature-gates", fmt.Sprintf("wardle:BanFlunder=%v", banFlunder))
		}
		// TODO figure out how to actually make BinaryVersion/EmulationVersion work with Wardle and KAS at the same time when Alpha FG are being set
		wardleCmd := sampleserver.NewCommandStartWardleServer(ctx, wardleOptions, withUID)
		wardleCmd.SetArgs(args)
		if err := wardleCmd.Execute(); err != nil {
			t.Error(err)
		}
	}()

	directWardleClientConfig, err := waitForWardleRunning(ctx, t, kubeConfig, certDir, wardlePort)
	if err != nil {
		t.Fatal(err)
	}

	return directWardleClientConfig
}

func waitForWardleAPIServiceReady(ctx context.Context, t *testing.T, kubeConfig *rest.Config, wardleCertDir string, namespace string) {
	kubeClient := client.NewForConfigOrDie(kubeConfig)
	aggregatorClient := aggregatorclient.NewForConfigOrDie(kubeConfig)

	wardleCA, err := os.ReadFile(wardleCAFilePath(wardleCertDir))
	if err != nil {
		t.Fatal(err)
	}
	_, err = aggregatorClient.ApiregistrationV1().APIServices().Create(ctx, &apiregistrationv1.APIService{
		ObjectMeta: metav1.ObjectMeta{Name: "v1alpha1.wardle.example.com"},
		Spec: apiregistrationv1.APIServiceSpec{
			Service: &apiregistrationv1.ServiceReference{
				Namespace: namespace,
				Name:      "api",
			},
			Group:                "wardle.example.com",
			Version:              "v1alpha1",
			CABundle:             wardleCA,
			GroupPriorityMinimum: 200,
			VersionPriority:      200,
		},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}

	// wait for the API service to be available
	err = wait.Poll(time.Second, wait.ForeverTestTimeout, func() (done bool, err error) {
		apiService, err := aggregatorClient.ApiregistrationV1().APIServices().Get(ctx, "v1alpha1.wardle.example.com", metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		var available bool
		for _, condition := range apiService.Status.Conditions {
			if condition.Type == apiregistrationv1.Available && condition.Status == apiregistrationv1.ConditionTrue {
				available = true
				break
			}
		}
		if !available {
			t.Log("api service is not available", apiService.Status.Conditions)
			return false, nil
		}

		// make sure discovery is healthy overall
		_, _, err = kubeClient.Discovery().ServerGroupsAndResources()
		if err != nil {
			t.Log("discovery failed", err)
			return false, nil
		}

		// make sure we have the wardle resources in discovery
		apiResources, err := kubeClient.Discovery().ServerResourcesForGroupVersion("wardle.example.com/v1alpha1")
		if err != nil {
			t.Log("wardle discovery failed", err)
			return false, nil
		}
		if len(apiResources.APIResources) != 2 {
			t.Log("wardle discovery has wrong resources", apiResources.APIResources)
			return false, nil
		}
		resources := make([]string, 0, 2)
		for _, resource := range apiResources.APIResources {
			resource := resource
			resources = append(resources, resource.Name)
		}
		sort.Strings(resources)
		if !reflect.DeepEqual([]string{"fischers", "flunders"}, resources) {
			return false, fmt.Errorf("unexpected resources: %v", resources)
		}

		return true, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func getKubeConfig(testServer *kastesting.TestServer) *rest.Config {
	kubeClientConfig := rest.CopyConfig(testServer.ClientConfig)
	// force json because everything speaks it
	kubeClientConfig.ContentType = ""
	kubeClientConfig.AcceptContentTypes = ""

	return kubeClientConfig
}

func waitForWardleRunning(ctx context.Context, t *testing.T, wardleToKASKubeConfig *rest.Config, wardleCertDir string, wardlePort int) (*rest.Config, error) {
	directWardleClientConfig := rest.AnonymousClientConfig(rest.CopyConfig(wardleToKASKubeConfig))
	directWardleClientConfig.CAFile = wardleCAFilePath(wardleCertDir)
	directWardleClientConfig.CAData = nil
	directWardleClientConfig.ServerName = ""
	directWardleClientConfig.BearerToken = wardleToKASKubeConfig.BearerToken
	var wardleClient client.Interface
	lastHealthContent := []byte{}
	var lastHealthErr error
	err := wait.PollImmediate(100*time.Millisecond, 10*time.Second, func() (done bool, err error) {
		if _, err := os.Stat(directWardleClientConfig.CAFile); os.IsNotExist(err) { // wait until the file trust is created
			lastHealthErr = err
			return false, nil
		}
		directWardleClientConfig.Host = fmt.Sprintf("https://127.0.0.1:%d", wardlePort)
		wardleClient, err = client.NewForConfig(directWardleClientConfig)
		if err != nil {
			// this happens because we race the API server start
			t.Log(err)
			return false, nil
		}
		healthStatus := 0
		result := wardleClient.Discovery().RESTClient().Get().AbsPath("/healthz").Do(ctx).StatusCode(&healthStatus)
		lastHealthContent, lastHealthErr = result.Raw()
		if healthStatus != http.StatusOK {
			return false, nil
		}
		return true, nil
	})
	if err != nil {
		t.Log(string(lastHealthContent))
		t.Log(lastHealthErr)
		return nil, err
	}

	return directWardleClientConfig, nil
}

func wardleCAFilePath(wardleCertDir string) string { return path.Join(wardleCertDir, "apiserver.crt") }

func writeKubeConfigForWardleServerToKASConnection(t *testing.T, kubeClientConfig *rest.Config) string {
	// write a kubeconfig out for starting other API servers with delegated auth.  remember, no in-cluster config
	// the loopback client config uses a loopback cert with different SNI.  We need to use the "real"
	// cert, so we'll hope we aren't hacked during a unit test and instead load it from the server we started.
	wardleToKASKubeClientConfig := rest.CopyConfig(kubeClientConfig)

	servingCerts, _, err := cert.GetServingCertificatesForURL(wardleToKASKubeClientConfig.Host, "")
	if err != nil {
		t.Fatal(err)
	}
	encodedServing, err := cert.EncodeCertificates(servingCerts...)
	if err != nil {
		t.Fatal(err)
	}
	wardleToKASKubeClientConfig.CAData = encodedServing

	for _, v := range servingCerts {
		t.Logf("Client: Server public key is %v\n", dynamiccertificates.GetHumanCertDetail(v))
	}
	certs, err := cert.ParseCertsPEM(wardleToKASKubeClientConfig.CAData)
	if err != nil {
		t.Fatal(err)
	}
	for _, curr := range certs {
		t.Logf("CA bundle %v\n", dynamiccertificates.GetHumanCertDetail(curr))
	}

	adminKubeConfig := createKubeConfig(wardleToKASKubeClientConfig)
	wardleToKASKubeConfigFile, _ := os.CreateTemp("", "")
	if err := clientcmd.WriteToFile(*adminKubeConfig, wardleToKASKubeConfigFile.Name()); err != nil {
		t.Fatal(err)
	}

	defer wardleToKASKubeConfigFile.Close()
	return wardleToKASKubeConfigFile.Name()
}

func createKubeConfig(clientCfg *rest.Config) *clientcmdapi.Config {
	clusterNick := "cluster"
	userNick := "user"
	contextNick := "context"

	config := clientcmdapi.NewConfig()

	credentials := clientcmdapi.NewAuthInfo()
	credentials.Token = clientCfg.BearerToken
	credentials.ClientCertificate = clientCfg.TLSClientConfig.CertFile
	if len(credentials.ClientCertificate) == 0 {
		credentials.ClientCertificateData = clientCfg.TLSClientConfig.CertData
	}
	credentials.ClientKey = clientCfg.TLSClientConfig.KeyFile
	if len(credentials.ClientKey) == 0 {
		credentials.ClientKeyData = clientCfg.TLSClientConfig.KeyData
	}
	config.AuthInfos[userNick] = credentials

	cluster := clientcmdapi.NewCluster()
	cluster.Server = clientCfg.Host
	cluster.CertificateAuthority = clientCfg.CAFile
	if len(cluster.CertificateAuthority) == 0 {
		cluster.CertificateAuthorityData = clientCfg.CAData
	}
	cluster.InsecureSkipTLSVerify = clientCfg.Insecure
	config.Clusters[clusterNick] = cluster

	context := clientcmdapi.NewContext()
	context.Cluster = clusterNick
	context.AuthInfo = userNick
	config.Contexts[contextNick] = context
	config.CurrentContext = contextNick

	return config
}

func readResponse(ctx context.Context, client rest.Interface, location string) ([]byte, error) {
	return client.Get().AbsPath(location).DoRaw(ctx)
}

func testAPIGroupList(ctx context.Context, t *testing.T, client rest.Interface) {
	contents, err := readResponse(ctx, client, "/apis")
	if err != nil {
		t.Fatalf("%v", err)
	}
	t.Log(string(contents))
	var apiGroupList metav1.APIGroupList
	err = json.Unmarshal(contents, &apiGroupList)
	if err != nil {
		t.Fatalf("Error in unmarshalling response from server %s: %v", "/apis", err)
	}
	assert.Len(t, apiGroupList.Groups, 1)
	assert.Equal(t, wardlev1alpha1.GroupName, apiGroupList.Groups[0].Name)
	assert.Len(t, apiGroupList.Groups[0].Versions, 2)

	v1alpha1 := metav1.GroupVersionForDiscovery{
		GroupVersion: wardlev1alpha1.SchemeGroupVersion.String(),
		Version:      wardlev1alpha1.SchemeGroupVersion.Version,
	}
	v1beta1 := metav1.GroupVersionForDiscovery{
		GroupVersion: wardlev1beta1.SchemeGroupVersion.String(),
		Version:      wardlev1beta1.SchemeGroupVersion.Version,
	}

	assert.Equal(t, v1beta1, apiGroupList.Groups[0].Versions[0])
	assert.Equal(t, v1alpha1, apiGroupList.Groups[0].Versions[1])
	assert.Equal(t, v1beta1, apiGroupList.Groups[0].PreferredVersion)
}

func testAPIGroup(ctx context.Context, t *testing.T, client rest.Interface) {
	contents, err := readResponse(ctx, client, "/apis/wardle.example.com")
	if err != nil {
		t.Fatalf("%v", err)
	}
	t.Log(string(contents))
	var apiGroup metav1.APIGroup
	err = json.Unmarshal(contents, &apiGroup)
	if err != nil {
		t.Fatalf("Error in unmarshalling response from server %s: %v", "/apis/wardle.example.com", err)
	}
	assert.Equal(t, wardlev1alpha1.SchemeGroupVersion.Group, apiGroup.Name)
	assert.Len(t, apiGroup.Versions, 2)
	assert.Equal(t, wardlev1alpha1.SchemeGroupVersion.String(), apiGroup.Versions[1].GroupVersion)
	assert.Equal(t, wardlev1alpha1.SchemeGroupVersion.Version, apiGroup.Versions[1].Version)
	assert.Equal(t, apiGroup.PreferredVersion, apiGroup.Versions[0])
}

func testAPIResourceList(ctx context.Context, t *testing.T, client rest.Interface) {
	contents, err := readResponse(ctx, client, "/apis/wardle.example.com/v1alpha1")
	if err != nil {
		t.Fatalf("%v", err)
	}
	t.Log(string(contents))
	var apiResourceList metav1.APIResourceList
	err = json.Unmarshal(contents, &apiResourceList)
	if err != nil {
		t.Fatalf("Error in unmarshalling response from server %s: %v", "/apis/wardle.example.com/v1alpha1", err)
	}
	assert.Equal(t, wardlev1alpha1.SchemeGroupVersion.String(), apiResourceList.GroupVersion)
	assert.Len(t, apiResourceList.APIResources, 2)
	assert.Equal(t, "fischers", apiResourceList.APIResources[0].Name)
	assert.False(t, apiResourceList.APIResources[0].Namespaced)
	assert.Equal(t, "flunders", apiResourceList.APIResources[1].Name)
	assert.True(t, apiResourceList.APIResources[1].Namespaced)
}

var (
	// I have no idea what these certs are, they just need to be different
	differentClientCA = []byte(`-----BEGIN CERTIFICATE-----
MIIDQDCCAiigAwIBAgIJANWw74P5KJk2MA0GCSqGSIb3DQEBCwUAMDQxMjAwBgNV
BAMMKWdlbmVyaWNfd2ViaG9va19hZG1pc3Npb25fcGx1Z2luX3Rlc3RzX2NhMCAX
DTE3MTExNjAwMDUzOVoYDzIyOTEwOTAxMDAwNTM5WjAjMSEwHwYDVQQDExh3ZWJo
b29rLXRlc3QuZGVmYXVsdC5zdmMwggEiMA0GCSqGSIb3DQEBAQUAA4IBDwAwggEK
AoIBAQDXd/nQ89a5H8ifEsigmMd01Ib6NVR3bkJjtkvYnTbdfYEBj7UzqOQtHoLa
dIVmefny5uIHvj93WD8WDVPB3jX2JHrXkDTXd/6o6jIXHcsUfFTVLp6/bZ+Anqe0
r/7hAPkzA2A7APyTWM3ZbEeo1afXogXhOJ1u/wz0DflgcB21gNho4kKTONXO3NHD
XLpspFqSkxfEfKVDJaYAoMnYZJtFNsa2OvsmLnhYF8bjeT3i07lfwrhUZvP+7Gsp
7UgUwc06WuNHjfx1s5e6ySzH0QioMD1rjYneqOvk0pKrMIhuAEWXqq7jlXcDtx1E
j+wnYbVqqVYheHZ8BCJoVAAQGs9/AgMBAAGjZDBiMAkGA1UdEwQCMAAwCwYDVR0P
BAQDAgXgMB0GA1UdJQQWMBQGCCsGAQUFBwMCBggrBgEFBQcDATApBgNVHREEIjAg
hwR/AAABghh3ZWJob29rLXRlc3QuZGVmYXVsdC5zdmMwDQYJKoZIhvcNAQELBQAD
ggEBAD/GKSPNyQuAOw/jsYZesb+RMedbkzs18sSwlxAJQMUrrXwlVdHrA8q5WhE6
ABLqU1b8lQ8AWun07R8k5tqTmNvCARrAPRUqls/ryER+3Y9YEcxEaTc3jKNZFLbc
T6YtcnkdhxsiO136wtiuatpYL91RgCmuSpR8+7jEHhuFU01iaASu7ypFrUzrKHTF
bKwiLRQi1cMzVcLErq5CDEKiKhUkoDucyARFszrGt9vNIl/YCcBOkcNvM3c05Hn3
M++C29JwS3Hwbubg6WO3wjFjoEhpCwU6qRYUz3MRp4tHO4kxKXx+oQnUiFnR7vW0
YkNtGc1RUDHwecCTFpJtPb7Yu/E=
-----END CERTIFICATE-----
`)
	differentFrontProxyCA = []byte(`-----BEGIN CERTIFICATE-----
MIIBqDCCAU2gAwIBAgIUfbqeieihh/oERbfvRm38XvS/xHAwCgYIKoZIzj0EAwIw
GjEYMBYGA1UEAxMPSW50ZXJtZWRpYXRlLUNBMCAXDTE2MTAxMTA1MDYwMFoYDzIx
MTYwOTE3MDUwNjAwWjAUMRIwEAYDVQQDEwlNeSBDbGllbnQwWTATBgcqhkjOPQIB
BggqhkjOPQMBBwNCAARv6N4R/sjMR65iMFGNLN1GC/vd7WhDW6J4X/iAjkRLLnNb
KbRG/AtOUZ+7upJ3BWIRKYbOabbQGQe2BbKFiap4o3UwczAOBgNVHQ8BAf8EBAMC
BaAwEwYDVR0lBAwwCgYIKwYBBQUHAwIwDAYDVR0TAQH/BAIwADAdBgNVHQ4EFgQU
K/pZOWpNcYai6eHFpmJEeFpeQlEwHwYDVR0jBBgwFoAUX6nQlxjfWnP6aM1meO/Q
a6b3a9kwCgYIKoZIzj0EAwIDSQAwRgIhAIWTKw/sjJITqeuNzJDAKU4xo1zL+xJ5
MnVCuBwfwDXCAiEAw/1TA+CjPq9JC5ek1ifR0FybTURjeQqYkKpve1dveps=
-----END CERTIFICATE-----

`)
)

type staticURLServiceResolver string

func (u staticURLServiceResolver) ResolveEndpoint(namespace, name string, port int32) (*url.URL, error) {
	return url.Parse(string(u))
}
