/*
Copyright 2023 The Kubernetes Authors.

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

package peerproxy

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"net/http/httptest"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	v1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/apitesting"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apiserver/pkg/authentication/user"
	apifilters "k8s.io/apiserver/pkg/endpoints/filters"
	apirequest "k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/features"
	"k8s.io/apiserver/pkg/reconcilers"
	etcd3testing "k8s.io/apiserver/pkg/storage/etcd3/testing"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/apiserver/pkg/util/peerproxy/metrics"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	coordinationv1 "k8s.io/client-go/listers/coordination/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/transport"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	"k8s.io/component-base/metrics/legacyregistry"
	"k8s.io/component-base/metrics/testutil"
)

const (
	requestTimeout  = 30 * time.Second
	localServerID   = "local-apiserver"
	remoteServerID1 = "remote-apiserver-1"
	remoteServerID2 = "remote-apiserver-2"
)

type fakeGVRData struct {
	gvr       schema.GroupVersionResource
	serverIDs []string
}

type server struct {
	publicIP string
	serverID string
}

type reconciler struct {
	do      bool
	servers []server
}

type fakeApiserverIdentityLister struct {
	apiservers []interface{}
}

func (l *fakeApiserverIdentityLister) Leases(namespace string) coordinationv1.LeaseNamespaceLister {
	return l
}

func (l *fakeApiserverIdentityLister) List(selector labels.Selector) ([]*v1.Lease, error) {
	result := make([]*v1.Lease, len(l.apiservers))
	for i, lease := range l.apiservers {
		result[i] = lease.(*v1.Lease)
	}
	return result, nil
}

func (l *fakeApiserverIdentityLister) Get(name string) (*v1.Lease, error) {
	return &v1.Lease{}, nil
}

func TestPeerProxy(t *testing.T) {
	testCases := []struct {
		desc              string
		gvrdata           fakeGVRData
		requestPath       string
		peerproxiedHeader string
		expectedStatus    int
		metrics           []string
		want              string
		reconcilerConfig  reconciler
	}{
		{
			desc:           "allow non resource requests",
			requestPath:    "/foo/bar/baz",
			expectedStatus: http.StatusOK,
		},
		{
			desc:              "allow if already proxied once",
			requestPath:       "/api/bar/baz",
			expectedStatus:    http.StatusOK,
			peerproxiedHeader: "true",
		},
		{
			desc:           "allow if unsynced informers",
			requestPath:    "/api/bar/baz",
			expectedStatus: http.StatusOK,
		},
		{
			desc:           "200 if no leases found, serve locally",
			requestPath:    "/api/foo/bar",
			expectedStatus: http.StatusOK,
			gvrdata: fakeGVRData{
				gvr: schema.GroupVersionResource{
					Group:    "core",
					Version:  "foo",
					Resource: "bar"},
			},
		},
		{
			desc:           "503 if no endpoint fetched from lease",
			requestPath:    "/api/foo/bar",
			expectedStatus: http.StatusServiceUnavailable,
			gvrdata: fakeGVRData{
				gvr: schema.GroupVersionResource{
					Group:    "core",
					Version:  "foo",
					Resource: "bar"},
				serverIDs: []string{remoteServerID1, remoteServerID2},
			},
		},
		{
			desc:           "200 if locally serviceable",
			requestPath:    "/api/foo/bar",
			expectedStatus: http.StatusOK,
			gvrdata: fakeGVRData{
				gvr: schema.GroupVersionResource{
					Group:    "core",
					Version:  "foo",
					Resource: "bar"},
				serverIDs: []string{localServerID}},
		},
		{
			desc:           "503 if all peers had invalid host:port info",
			requestPath:    "/api/foo/bar",
			expectedStatus: http.StatusServiceUnavailable,
			gvrdata: fakeGVRData{
				gvr: schema.GroupVersionResource{
					Group:    "core",
					Version:  "foo",
					Resource: "bar"},
				serverIDs: []string{remoteServerID1, remoteServerID2}},
			reconcilerConfig: reconciler{
				do: true,
				servers: []server{
					{
						publicIP: "1[2.4",
						serverID: remoteServerID1,
					},
					{
						publicIP: "2.4]6",
						serverID: remoteServerID2,
					},
				},
			},
		},
		{
			desc:           "503 unreachable peer bind address",
			requestPath:    "/api/foo/bar",
			expectedStatus: http.StatusServiceUnavailable,
			gvrdata: fakeGVRData{
				gvr: schema.GroupVersionResource{
					Group:    "core",
					Version:  "foo",
					Resource: "bar"},
				serverIDs: []string{remoteServerID1, remoteServerID2}},
			reconcilerConfig: reconciler{
				do: true,
				servers: []server{
					{
						publicIP: "1.2.3.4",
						serverID: remoteServerID1,
					},
				},
			},
			metrics: []string{
				"apiserver_rerouted_request_total",
			},
			want: `
			# HELP apiserver_rerouted_request_total [ALPHA] Total number of requests that were proxied to a peer kube apiserver because the local apiserver was not capable of serving it
			# TYPE apiserver_rerouted_request_total counter
			apiserver_rerouted_request_total{code="503"} 1
			`,
		},
		{
			desc:           "503 if one apiserver's endpoint lease wasnt found but another valid (unreachable) apiserver was found",
			requestPath:    "/api/foo/bar",
			expectedStatus: http.StatusServiceUnavailable,
			gvrdata: fakeGVRData{
				gvr: schema.GroupVersionResource{
					Group:    "core",
					Version:  "foo",
					Resource: "bar"},
				serverIDs: []string{remoteServerID1, remoteServerID2}},
			reconcilerConfig: reconciler{
				do: true,
				servers: []server{
					{
						publicIP: "1.2.3.4",
						serverID: remoteServerID1,
					},
				},
			},
			metrics: []string{
				"apiserver_rerouted_request_total",
			},
			want: `
			# HELP apiserver_rerouted_request_total [ALPHA] Total number of requests that were proxied to a peer kube apiserver because the local apiserver was not capable of serving it
			# TYPE apiserver_rerouted_request_total counter
			apiserver_rerouted_request_total{code="503"} 2
			`,
		},
	}

	metrics.Register()
	for _, tt := range testCases {
		t.Run(tt.desc, func(t *testing.T) {
			lastHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Write([]byte("OK"))
			})
			fakeApiserverIdentityLister := fakeApiserverIdentityLeases(tt.gvrdata.serverIDs)
			fakeReconciler := newFakePeerEndpointReconciler(t)
			handler := newHandlerChain(t, lastHandler, fakeApiserverIdentityLister, fakeReconciler, tt.gvrdata)
			server, requestGetter := createHTTP2ServerWithClient(handler, requestTimeout*2)
			defer server.Close()

			if tt.reconcilerConfig.do {
				// need to enable feature flags first
				featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.APIServerIdentity, true)
				featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.StorageVersionAPI, true)

				for _, s := range tt.reconcilerConfig.servers {
					err := fakeReconciler.UpdateLease(s.serverID,
						s.publicIP,
						[]corev1.EndpointPort{{Name: "foo",
							Port: 8080, Protocol: "TCP"}})
					if err != nil {
						t.Errorf("Failed to update lease for server %s", s.serverID)
					}
				}
			}

			req, err := http.NewRequest(http.MethodGet, server.URL+tt.requestPath, nil)
			if err != nil {
				t.Fatalf("failed to create new http request - %v", err)
			}
			req.Header.Set(PeerProxiedHeader, tt.peerproxiedHeader)

			resp, _ := requestGetter(req)

			// compare response
			assert.Equal(t, tt.expectedStatus, resp.StatusCode)

			// compare metric
			if tt.want != "" {
				if err := testutil.GatherAndCompare(legacyregistry.DefaultGatherer, strings.NewReader(tt.want), tt.metrics...); err != nil {
					t.Fatal(err)
				}
			}
		})
	}

}

func newFakePeerEndpointReconciler(t *testing.T) reconcilers.PeerEndpointLeaseReconciler {
	server, sc := etcd3testing.NewUnsecuredEtcd3TestClientServer(t)
	t.Cleanup(func() { server.Terminate(t) })
	scheme := runtime.NewScheme()
	metav1.AddToGroupVersion(scheme, metav1.SchemeGroupVersion)
	//utilruntime.Must(core.AddToScheme(scheme))
	utilruntime.Must(corev1.AddToScheme(scheme))
	utilruntime.Must(scheme.SetVersionPriority(corev1.SchemeGroupVersion))
	codecs := serializer.NewCodecFactory(scheme)
	sc.Codec = apitesting.TestStorageCodec(codecs, corev1.SchemeGroupVersion)
	config := *sc.ForResource(schema.GroupResource{Resource: "endpoints"})
	baseKey := "/" + uuid.New().String() + "/peer-testleases/"
	leaseTime := 1 * time.Minute
	reconciler, err := reconcilers.NewPeerEndpointLeaseReconciler(&config, baseKey, leaseTime)
	if err != nil {
		t.Fatalf("Error creating storage: %v", err)
	}
	return reconciler
}

func newHandlerChain(t *testing.T, handler http.Handler, apiserverIdentityLister *fakeApiserverIdentityLister, reconciler reconcilers.PeerEndpointLeaseReconciler, gvrData fakeGVRData) http.Handler {
	// Add peerproxy handler
	s := serializer.NewCodecFactory(runtime.NewScheme()).WithoutConversion()
	peerProxyHandler, err := newFakePeerProxyHandler(apiserverIdentityLister, reconciler, gvrData, localServerID, s)
	if err != nil {
		t.Fatalf("Error creating peer proxy handler: %v", err)
	}
	handler = peerProxyHandler.WrapHandler(handler)

	// Add user info
	handler = withFakeUser(handler)

	// Add requestInfo handler
	requestInfoFactory := &apirequest.RequestInfoFactory{APIPrefixes: sets.NewString("apis", "api"), GrouplessAPIPrefixes: sets.NewString("api")}
	handler = apifilters.WithRequestInfo(handler, requestInfoFactory)
	return handler
}

func newFakePeerProxyHandler(apiserverIdentityLister *fakeApiserverIdentityLister, reconciler reconcilers.PeerEndpointLeaseReconciler, gvrdata fakeGVRData, id string, s runtime.NegotiatedSerializer) (*peerProxyHandler, error) {
	clientset := fake.NewSimpleClientset()
	informerFactory := informers.NewSharedInformerFactory(clientset, 0)
	clientConfig := &transport.Config{
		TLS: transport.TLSConfig{
			Insecure: false,
		}}
	loopbackClientConfig := &rest.Config{
		Host: "///:://localhost",
	}
	ppI := NewPeerProxyHandler(informerFactory, id, reconciler, s, loopbackClientConfig, clientConfig)
	if testDataExists(gvrdata.gvr) {
		for _, serverID := range gvrdata.serverIDs {
			if serverID == localServerID {
				ppI.discoveryResponseCacheLock.Lock()
				ppI.localDiscoveryResponseCache[gvrdata.gvr.GroupVersion()] = []metav1.APIResource{
					{Name: gvrdata.gvr.Resource, Kind: "SomeKind"},
				}
				ppI.discoveryResponseCacheLock.Unlock()
			}
		}
	}
	ppI.finishedSync.Store(true)

	ppI.apiserverIdentityLister = apiserverIdentityLister
	return ppI, nil
}

func testDataExists(gvr schema.GroupVersionResource) bool {
	return gvr.Group != "" && gvr.Version != "" && gvr.Resource != ""
}

func withFakeUser(handler http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r = r.WithContext(apirequest.WithUser(r.Context(), &user.DefaultInfo{
			Groups: r.Header["Groups"],
		}))
		handler.ServeHTTP(w, r)
	})
}

// returns a started http2 server, with a client function to send request to the server.
func createHTTP2ServerWithClient(handler http.Handler, clientTimeout time.Duration) (*httptest.Server, func(req *http.Request) (*http.Response, error)) {
	server := httptest.NewUnstartedServer(handler)
	server.EnableHTTP2 = true
	server.StartTLS()
	cli := server.Client()
	cli.Timeout = clientTimeout
	return server, func(req *http.Request) (*http.Response, error) {
		return cli.Do(req)
	}
}

func fakeApiserverIdentityLeases(apiserverIds []string) *fakeApiserverIdentityLister {
	apiserverLeases := make([]interface{}, len(apiserverIds))
	for i, id := range apiserverIds {
		apiserverLeases[i] = &v1.Lease{
			ObjectMeta: metav1.ObjectMeta{
				Name:      id,
				Namespace: metav1.NamespaceSystem,
				Labels: map[string]string{
					"apiserver.kubernetes.io/identity": "kube-apiserver",
				},
			},
		}
	}

	lister := &fakeApiserverIdentityLister{
		apiservers: apiserverLeases,
	}

	return lister
}
