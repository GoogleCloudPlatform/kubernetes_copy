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
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"

	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/util/proxy"
	"k8s.io/apiserver/pkg/endpoints/handlers/responsewriters"
	"k8s.io/apiserver/pkg/endpoints/responsewriter"
	"k8s.io/apiserver/pkg/reconcilers"
	"k8s.io/apiserver/pkg/util/peerproxy/metrics"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/transport"
	"k8s.io/klog/v2"

	apidiscoveryv2 "k8s.io/api/apidiscovery/v2"
	v1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	schema "k8s.io/apimachinery/pkg/runtime/schema"
	epmetrics "k8s.io/apiserver/pkg/endpoints/metrics"
	apirequest "k8s.io/apiserver/pkg/endpoints/request"
	apiserverproxyutil "k8s.io/apiserver/pkg/util/proxy"
	responsewriterutil "k8s.io/apiserver/pkg/util/responsewriter"
	coordinationv1 "k8s.io/client-go/listers/coordination/v1"
	restclient "k8s.io/client-go/rest"
)

const (
	PeerProxiedHeader = "x-kubernetes-peer-proxied"
)

type peerProxyHandler struct {
	name                      string
	apiserverIdentityInformer cache.SharedIndexInformer
	// ApiserverIdentityLister is used to fetch all apiservers in the cluster.
	apiserverIdentityLister coordinationv1.LeaseLister
	// identity for this server
	serverId string
	// reconciler that is used to fetch host port of peer apiserver when proxying request to a peer
	reconciler                  reconcilers.PeerEndpointLeaseReconciler
	serializer                  runtime.NegotiatedSerializer
	discoverySerializer         serializer.CodecFactory
	loopbackClientConfig        *restclient.Config
	proxyClientConfig           *transport.Config
	finishedSync                atomic.Bool
	localDiscoveryResponseCache map[schema.GroupVersion][]metav1.APIResource
	discoveryResponseCacheLock  sync.RWMutex
}

// responder implements rest.Responder for assisting a connector in writing objects or errors.
type responder struct {
	w   http.ResponseWriter
	ctx context.Context
}

func (h *peerProxyHandler) HasFinishedSync() bool {
	return h.finishedSync.Load()
}

func (h *peerProxyHandler) WaitForCacheSync(stopCh <-chan struct{}) error {
	ok := cache.WaitForNamedCacheSync("unknown-version-proxy", stopCh, h.apiserverIdentityInformer.HasSynced)
	if !ok {
		return fmt.Errorf("error while waiting for initial cache sync")
	}
	if err := h.populateLocalDiscoveryCache(); err != nil {
		return fmt.Errorf("failed to populate discovery cache: %w", err)
	}
	h.finishedSync.Store(true)
	return nil
}

// WrapHandler will fetch the apiservers that can serve the request and either serve it locally
// or route it to a peer
func (h *peerProxyHandler) WrapHandler(handler http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		requestInfo, ok := apirequest.RequestInfoFrom(ctx)
		if !ok {
			responsewriters.InternalError(w, r, errors.New("no RequestInfo found in the context"))
			return
		}

		// Allow non-resource requests
		if !requestInfo.IsResourceRequest {
			klog.V(3).Infof("Not a resource request skipping proxying")
			handler.ServeHTTP(w, r)
			return
		}

		// Request has already been proxied once, it must be served locally
		if r.Header.Get(PeerProxiedHeader) == "true" {
			klog.V(3).Infof("Already rerouted once, skipping proxying to peer")
			handler.ServeHTTP(w, r)
			return
		}

		// Apiserver Identity Informers is not synced yet, pass request to next handler
		// This will happen for self requests from the kube-apiserver because we have a poststarthook
		// to ensure that external requests are not served until the ApiserverIdentity Informer has synced
		if !h.HasFinishedSync() {
			handler.ServeHTTP(w, r)
			return
		}

		gvr := schema.GroupVersionResource{Group: requestInfo.APIGroup, Version: requestInfo.APIVersion, Resource: requestInfo.Resource}
		apiserversi, err := h.apiserverIdentityLeases()
		if err != nil {
			klog.ErrorS(err, "no apiserver identity leases found")
			handler.ServeHTTP(w, r)
			return
		}

		if h.shouldServeLocally(apiserversi, gvr) {
			handler.ServeHTTP(w, r)
			return
		}

		// find servers that are capable of serving this request
		peerEndpoint, err := h.findServiceableByServers(r.Context(), gvr, apiserversi)
		if err != nil {
			klog.ErrorS(err, "error fetching remote server details while proxying")
			responsewriters.ErrorNegotiated(apierrors.NewServiceUnavailable("Error getting ip and port info of the remote server while proxying"), h.serializer, gvr.GroupVersion(), w, r)
			return
		}

		h.proxyRequestToDestinationAPIServer(r, w, peerEndpoint)

	})
}

func (h *peerProxyHandler) populateLocalDiscoveryCache() error {
	discoveryClient, err := discovery.NewDiscoveryClientForConfig(h.loopbackClientConfig)
	if err != nil {
		return fmt.Errorf("error creating discovery client: %w", err)
	}

	_, resourcesByGV, _, err := discoveryClient.GroupsAndMaybeResources()
	if err != nil {
		return fmt.Errorf("error getting API group resources from discovery: %w", err)
	}

	h.discoveryResponseCacheLock.Lock()
	defer h.discoveryResponseCacheLock.Unlock()
	for gv, resources := range resourcesByGV {
		h.localDiscoveryResponseCache[gv] = resources.APIResources
	}

	return nil
}

func (h *peerProxyHandler) apiserverIdentityLeases() ([]*v1.Lease, error) {
	var apiserversi []*v1.Lease
	apiserversi, err := h.apiserverIdentityLister.Leases(metav1.NamespaceSystem).List(labels.SelectorFromSet(labels.Set{"apiserver.kubernetes.io/identity": "kube-apiserver"}))
	if err != nil {
		return apiserversi, err
	}

	return apiserversi, nil
}

func (h *peerProxyHandler) shouldServeLocally(apiserversi []*v1.Lease, gvr schema.GroupVersionResource) bool {
	if len(apiserversi) <= 1 {
		return true
	}

	return h.localDiscoverySuccessful(gvr)
}

func (h *peerProxyHandler) localDiscoverySuccessful(gvr schema.GroupVersionResource) bool {
	resources, ok := h.localDiscoveryResponseCache[gvr.GroupVersion()]
	if !ok {
		klog.Errorf("resource not found for %v in local discovery cache\n", gvr.GroupVersion())
		return false
	}

	for _, resource := range resources {
		if gvr.GroupResource().Resource == resource.Name {
			return true
		}
	}

	return false
}

func (h *peerProxyHandler) findServiceableByServers(ctx context.Context, gvr schema.GroupVersionResource, apiserversi []*v1.Lease) (string, error) {
	var foundPeer bool
	var peerEndpoint string
	for _, identity := range apiserversi {
		apiserverKey := identity.Name
		if apiserverKey == h.serverId {
			continue
		}

		hostPort, err := getHostPortInfoFromLease(apiserverKey, h.reconciler)
		if err != nil {
			klog.Errorf("failed to get host port info from identity lease for server %s: %v", apiserverKey, err)
			continue
		}

		if h.proxyDiscoverySuccessful(ctx, hostPort, gvr) {
			peerEndpoint = hostPort
			foundPeer = true
			break
		}
	}

	if foundPeer {
		return peerEndpoint, nil
	}

	return "", fmt.Errorf("failed to fetch any apiserver capable of handling the request")
}

func getHostPortInfoFromLease(apiserverKey string, reconciler reconcilers.PeerEndpointLeaseReconciler) (string, error) {
	hostPort, err := reconciler.GetEndpoint(apiserverKey)
	if err != nil {
		return "", err
	}
	// check ip format
	_, _, err = net.SplitHostPort(hostPort)
	if err != nil {
		return "", err
	}

	return hostPort, nil
}

func (h *peerProxyHandler) proxyDiscoverySuccessful(ctx context.Context, hostport string, gvr schema.GroupVersionResource) bool {
	discoveryPaths := []string{"/api", "/apis"}
	for _, path := range discoveryPaths {
		discoveryResponse, err := h.aggregateDiscovery(ctx, path, hostport)
		if err != nil {
			klog.Errorf("error while querying discovery endpoint: %v", err)
			return false
		}

		if foundGVRInAggDiscoveryDoc(discoveryResponse, gvr) {
			return true
		}
	}

	return false
}

func (h *peerProxyHandler) aggregateDiscovery(ctx context.Context, path string, hostport string) (*apidiscoveryv2.APIGroupDiscoveryList, error) {
	req, err := http.NewRequest(http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}

	req = req.WithContext(ctx)
	req.Header.Add("Accept", discovery.AcceptV2)

	writer := responsewriterutil.NewInMemoryResponseWriter()
	h.proxyRequestToDestinationAPIServer(req, writer, hostport)
	if writer.RespCode() != http.StatusOK {
		return nil, fmt.Errorf("discovery request failed with status: %d", writer.RespCode())
	}

	parsed := &apidiscoveryv2.APIGroupDiscoveryList{}
	if err := runtime.DecodeInto(h.discoverySerializer.UniversalDecoder(), writer.Data(), parsed); err != nil {
		return nil, fmt.Errorf("error decoding discovery response: %w", err)
	}

	return parsed, nil
}

func (h *peerProxyHandler) proxyRequestToDestinationAPIServer(req *http.Request, rw http.ResponseWriter, host string) {
	// write a new location based on the existing request pointed at the target service
	location := &url.URL{}
	location.Scheme = "https"
	location.Host = host
	location.Path = req.URL.Path
	location.RawQuery = req.URL.Query().Encode()

	newReq, cancelFn := apiserverproxyutil.NewRequestForProxy(location, req)
	newReq.Header.Add(PeerProxiedHeader, "true")
	defer cancelFn()

	proxyRoundTripper, err := h.buildProxyRoundtripper(req)
	if err != nil {
		klog.Errorf("failed to build proxy round tripper: %v", err)
		return
	}

	delegate := &epmetrics.ResponseWriterDelegator{ResponseWriter: rw}
	w := responsewriter.WrapForHTTP1Or2(delegate)
	handler := proxy.NewUpgradeAwareHandler(location, proxyRoundTripper, true, false, &responder{w: w, ctx: req.Context()})
	handler.ServeHTTP(w, newReq)
	metrics.IncPeerProxiedRequest(req.Context(), strconv.Itoa(delegate.Status()))
}

func (h *peerProxyHandler) buildProxyRoundtripper(req *http.Request) (http.RoundTripper, error) {
	proxyTransport, err := transport.New(h.proxyClientConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create proxy transport")
	}

	user, ok := apirequest.UserFrom(req.Context())
	if !ok {
		return nil, err
	}

	return transport.NewAuthProxyRoundTripper(user.GetName(), user.GetUID(), user.GetGroups(), user.GetExtra(), proxyTransport), nil
}

func foundGVRInAggDiscoveryDoc(discoveryDoc *apidiscoveryv2.APIGroupDiscoveryList, gvr schema.GroupVersionResource) bool {
	for _, groupDiscovery := range discoveryDoc.Items {
		if groupDiscovery.Name == gvr.Group {
			for _, version := range groupDiscovery.Versions {
				if version.Version == gvr.Version {
					for _, resource := range version.Resources {
						if resource.Resource == gvr.Resource {
							return true
						}
					}
				}
			}
		}
	}

	return false
}

func (r *responder) Error(w http.ResponseWriter, req *http.Request, err error) {
	klog.ErrorS(err, "Error while proxying request to destination apiserver")
	http.Error(w, err.Error(), http.StatusServiceUnavailable)
}
