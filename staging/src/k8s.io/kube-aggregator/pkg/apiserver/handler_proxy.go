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
	"net/http"
	"net/url"
	"sync/atomic"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/httpstream"
	"k8s.io/apimachinery/pkg/util/proxy"
	"k8s.io/apiserver/pkg/endpoints/handlers/responsewriters"
	endpointmetrics "k8s.io/apiserver/pkg/endpoints/metrics"
	genericapirequest "k8s.io/apiserver/pkg/endpoints/request"
	genericfeatures "k8s.io/apiserver/pkg/features"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	utilflowcontrol "k8s.io/apiserver/pkg/util/flowcontrol"
	apiserverproxyutil "k8s.io/apiserver/pkg/util/proxy"
	"k8s.io/apiserver/pkg/util/x509metrics"
	"k8s.io/client-go/transport"
	"k8s.io/component-base/tracing"
	"k8s.io/klog/v2"
	apiregistrationv1api "k8s.io/kube-aggregator/pkg/apis/apiregistration/v1"
	apiregistrationv1apihelper "k8s.io/kube-aggregator/pkg/apis/apiregistration/v1/helper"
)

const (
	aggregatorComponent string = "aggregator"
)

type certKeyFunc func() ([]byte, []byte)

// proxyHandler provides a http.Handler which will proxy traffic to locations
// specified by items implementing Redirector.
type proxyHandler struct {
	// localDelegate is used to satisfy local APIServices
	localDelegate http.Handler

	// proxyCurrentCertKeyContent holds the client cert used to identify this proxy. Backing APIServices use this to confirm the proxy's identity
	proxyCurrentCertKeyContent certKeyFunc
	proxyTransportDial         *transport.DialHolder

	// Endpoints based routing to map from cluster IP to routable IP
	serviceResolver ServiceResolver

	handlingInfo atomic.Value

	// reject to forward redirect response
	rejectForwardingRedirects bool

	// tracerProvider is used to wrap the proxy transport and handler with tracing
	tracerProvider tracing.TracerProvider
}

type proxyHandlingInfo struct {
	// local indicates that this APIService is locally satisfied
	local bool

	// name is the name of the APIService
	name string
	// transportConfig holds the information for building a roundtripper
	transportConfig *transport.Config
	// transportBuildingError is an error produced while building the transport.  If this
	// is non-nil, it will be reported to clients.
	transportBuildingError error
	// proxyRoundTripper is the re-usable portion of the transport.  It does not vary with any request.
	proxyRoundTripper http.RoundTripper
	// serviceName is the name of the service this handler proxies to
	serviceName string
	// namespace is the namespace the service lives in
	serviceNamespace string
	// serviceAvailable indicates this APIService is available or not
	serviceAvailable bool
	// servicePort is the port of the service this handler proxies to
	servicePort int32
}

func proxyError(w http.ResponseWriter, req *http.Request, error string, code int) {
	http.Error(w, error, code)

	ctx := req.Context()
	info, ok := genericapirequest.RequestInfoFrom(ctx)
	if !ok {
		klog.Warning("no RequestInfo found in the context")
		return
	}
	// TODO: record long-running request differently? The long-running check func does not necessarily match the one of the aggregated apiserver
	endpointmetrics.RecordRequestTermination(req, info, aggregatorComponent, code)
}

func (r *proxyHandler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	value := r.handlingInfo.Load()
	if value == nil {
		r.localDelegate.ServeHTTP(w, req)
		return
	}
	handlingInfo := value.(proxyHandlingInfo)
	if handlingInfo.local {
		if r.localDelegate == nil {
			http.Error(w, "", http.StatusNotFound)
			return
		}
		r.localDelegate.ServeHTTP(w, req)
		return
	}

	if !handlingInfo.serviceAvailable {
		proxyError(w, req, "service unavailable", http.StatusServiceUnavailable)
		return
	}

	if handlingInfo.transportBuildingError != nil {
		proxyError(w, req, handlingInfo.transportBuildingError.Error(), http.StatusInternalServerError)
		return
	}

	user, ok := genericapirequest.UserFrom(req.Context())
	if !ok {
		proxyError(w, req, "missing user", http.StatusInternalServerError)
		return
	}

	// write a new location based on the existing request pointed at the target service
	location := &url.URL{}
	location.Scheme = "https"
	rloc, err := r.serviceResolver.ResolveEndpoint(handlingInfo.serviceNamespace, handlingInfo.serviceName, handlingInfo.servicePort)
	if err != nil {
		klog.Errorf("error resolving %s/%s: %v", handlingInfo.serviceNamespace, handlingInfo.serviceName, err)
		proxyError(w, req, "service unavailable", http.StatusServiceUnavailable)
		return
	}
	location.Host = rloc.Host
	location.Path = req.URL.Path
	location.RawQuery = req.URL.Query().Encode()

	newReq, cancelFn := apiserverproxyutil.NewRequestForProxy(location, req)
	defer cancelFn()

	if handlingInfo.proxyRoundTripper == nil {
		proxyError(w, req, "", http.StatusNotFound)
		return
	}

	proxyRoundTripper := handlingInfo.proxyRoundTripper
	upgrade := httpstream.IsUpgradeRequest(req)

	var userUID string
	if utilfeature.DefaultFeatureGate.Enabled(genericfeatures.RemoteRequestHeaderUID) {
		userUID = user.GetUID()
	}

	proxyRoundTripper = transport.NewAuthProxyRoundTripper(user.GetName(), userUID, user.GetGroups(), user.GetExtra(), proxyRoundTripper)

	if utilfeature.DefaultFeatureGate.Enabled(genericfeatures.APIServerTracing) && !upgrade {
		tracingWrapper := tracing.WrapperFor(r.tracerProvider)
		proxyRoundTripper = tracingWrapper(proxyRoundTripper)
	}

	// If we are upgrading, then the upgrade path tries to use this request with the TLS config we provide, but it does
	// NOT use the proxyRoundTripper.  It's a direct dial that bypasses the proxyRoundTripper.  This means that we have to
	// attach the "correct" user headers to the request ahead of time.
	if upgrade {
		transport.SetAuthProxyHeaders(newReq, user.GetName(), userUID, user.GetGroups(), user.GetExtra())
	}

	handler := proxy.NewUpgradeAwareHandler(location, proxyRoundTripper, true, upgrade, &responder{w: w})
	if r.rejectForwardingRedirects {
		handler.RejectForwardingRedirects = true
	}
	utilflowcontrol.RequestDelegated(req.Context())
	handler.ServeHTTP(w, newReq)
}

// responder implements rest.Responder for assisting a connector in writing objects or errors.
type responder struct {
	w http.ResponseWriter
}

// TODO this should properly handle content type negotiation
// if the caller asked for protobuf and you write JSON bad things happen.
func (r *responder) Object(statusCode int, obj runtime.Object) {
	responsewriters.WriteRawJSON(statusCode, obj, r.w)
}

func (r *responder) Error(_ http.ResponseWriter, _ *http.Request, err error) {
	http.Error(r.w, err.Error(), http.StatusServiceUnavailable)
}

// these methods provide locked access to fields

// Sets serviceAvailable value on proxyHandler
// not thread safe
func (r *proxyHandler) setServiceAvailable() {
	info := r.handlingInfo.Load().(proxyHandlingInfo)
	info.serviceAvailable = true
	r.handlingInfo.Store(info)
}

func (r *proxyHandler) updateAPIService(apiService *apiregistrationv1api.APIService) {
	if apiService.Spec.Service == nil {
		r.handlingInfo.Store(proxyHandlingInfo{local: true})
		return
	}

	proxyClientCert, proxyClientKey := r.proxyCurrentCertKeyContent()

	transportConfig := &transport.Config{
		TLS: transport.TLSConfig{
			Insecure:   apiService.Spec.InsecureSkipTLSVerify,
			ServerName: apiService.Spec.Service.Name + "." + apiService.Spec.Service.Namespace + ".svc",
			CertData:   proxyClientCert,
			KeyData:    proxyClientKey,
			CAData:     apiService.Spec.CABundle,
		},
		DialHolder: r.proxyTransportDial,
	}
	transportConfig.Wrap(x509metrics.NewDeprecatedCertificateRoundTripperWrapperConstructor(
		x509MissingSANCounter,
		x509InsecureSHA1Counter,
	))

	newInfo := proxyHandlingInfo{
		name:             apiService.Name,
		transportConfig:  transportConfig,
		serviceName:      apiService.Spec.Service.Name,
		serviceNamespace: apiService.Spec.Service.Namespace,
		servicePort:      *apiService.Spec.Service.Port,
		serviceAvailable: apiregistrationv1apihelper.IsAPIServiceConditionTrue(apiService, apiregistrationv1api.Available),
	}
	newInfo.proxyRoundTripper, newInfo.transportBuildingError = transport.New(newInfo.transportConfig)
	if newInfo.transportBuildingError != nil {
		klog.Warning(newInfo.transportBuildingError.Error())
	}
	r.handlingInfo.Store(newInfo)
}
