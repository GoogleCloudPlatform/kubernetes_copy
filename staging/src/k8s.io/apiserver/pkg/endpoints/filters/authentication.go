/*
Copyright 2014 The Kubernetes Authors.

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

package filters

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apiserver/pkg/authentication/authenticator"
	"k8s.io/apiserver/pkg/authentication/authenticatorfactory"
	"k8s.io/apiserver/pkg/authentication/request/headerrequest"
	"k8s.io/apiserver/pkg/authentication/user"
	"k8s.io/apiserver/pkg/endpoints/handlers/responsewriters"
	genericapirequest "k8s.io/apiserver/pkg/endpoints/request"
	genericfeatures "k8s.io/apiserver/pkg/features"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/klog/v2"
)

type authenticationRecordMetricsFunc func(context.Context, *authenticator.Response, bool, error, authenticator.Audiences, time.Time, time.Time)

// WithAuthentication creates an http handler that tries to authenticate the given request as a user, and then
// stores any such user found onto the provided context for the request. If authentication fails or returns an error
// the failed handler is used. On success, "Authorization" header is removed from the request and handler
// is invoked to serve the request.
func WithAuthentication(handler http.Handler, auth authenticator.Request, failed http.Handler, apiAuds authenticator.Audiences, requestHeaderConfig *authenticatorfactory.RequestHeaderConfig) http.Handler {
	return withAuthentication(handler, auth, failed, apiAuds, requestHeaderConfig, recordAuthenticationMetrics)
}

func withAuthentication(handler http.Handler, auth authenticator.Request, failed http.Handler, apiAuds authenticator.Audiences, requestHeaderConfig *authenticatorfactory.RequestHeaderConfig, metrics authenticationRecordMetricsFunc) http.Handler {
	if auth == nil {
		klog.Warning("Authentication is disabled")
		return handler
	}
	standardRequestHeaderConfig := &authenticatorfactory.RequestHeaderConfig{
		UsernameHeaders:     headerrequest.StaticStringSlice{"X-Remote-User"},
		UIDHeaders:          headerrequest.StaticStringSlice{"X-Remote-Uid"},
		GroupHeaders:        headerrequest.StaticStringSlice{"X-Remote-Group"},
		ExtraHeaderPrefixes: headerrequest.StaticStringSlice{"X-Remote-Extra-"},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		authenticationStart := time.Now()

		if len(apiAuds) > 0 {
			req = req.WithContext(authenticator.WithAudiences(req.Context(), apiAuds))
		}
		resp, ok, err := auth.AuthenticateRequest(req)
		authenticationFinish := time.Now()
		defer func() {
			metrics(req.Context(), resp, ok, err, apiAuds, authenticationStart, authenticationFinish)
		}()
		if err != nil || !ok {
			if err != nil {
				klog.ErrorS(err, "Unable to authenticate the request")
			}
			failed.ServeHTTP(w, req)
			return
		}

		if !audiencesAreAcceptable(apiAuds, resp.Audiences) {
			err = fmt.Errorf("unable to match the audience: %v , accepted: %v", resp.Audiences, apiAuds)
			klog.Error(err)
			failed.ServeHTTP(w, req)
			return
		}

		// authorization header is not required anymore in case of a successful authentication.
		req.Header.Del("Authorization")

		// delete standard front proxy headers
		headerrequest.ClearAuthenticationHeaders(
			req.Header,
			standardRequestHeaderConfig.UsernameHeaders,
			standardRequestHeaderConfig.UIDHeaders,
			standardRequestHeaderConfig.GroupHeaders,
			standardRequestHeaderConfig.ExtraHeaderPrefixes,
		)

		// also delete any custom front proxy headers
		if requestHeaderConfig != nil {
			headerrequest.ClearAuthenticationHeaders(
				req.Header,
				requestHeaderConfig.UsernameHeaders,
				requestHeaderConfig.UIDHeaders,
				requestHeaderConfig.GroupHeaders,
				requestHeaderConfig.ExtraHeaderPrefixes,
			)
		}

		// http2 is an expensive protocol that is prone to abuse,
		// see CVE-2023-44487 and CVE-2023-39325 for an example.
		// Do not allow unauthenticated clients to keep these
		// connections open (i.e. basically degrade them to the
		// performance of http1 with keep-alive disabled).
		if shouldCloseAnonymousHTTP2Request(req, resp) {
			// limit this connection to just this request,
			// and then send a GOAWAY and tear down the TCP connection
			// https://github.com/golang/net/commit/97aa3a539ec716117a9d15a4659a911f50d13c3c
			w.Header().Set("Connection", "close")
		}

		req = req.WithContext(genericapirequest.WithUser(req.Context(), resp.User))
		handler.ServeHTTP(w, req)
	})
}

func Unauthorized(s runtime.NegotiatedSerializer) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		// http2 is an expensive protocol that is prone to abuse,
		// see CVE-2023-44487 and CVE-2023-39325 for an example.
		// Do not allow unauthenticated clients to keep these
		// connections open (i.e. basically degrade them to the
		// performance of http1 with keep-alive disabled).
		if utilfeature.DefaultFeatureGate.Enabled(genericfeatures.UnauthenticatedHTTP2DOSMitigation) && req.ProtoMajor == 2 {
			// limit this connection to just this request,
			// and then send a GOAWAY and tear down the TCP connection
			// https://github.com/golang/net/commit/97aa3a539ec716117a9d15a4659a911f50d13c3c
			w.Header().Set("Connection", "close")
		}
		ctx := req.Context()
		requestInfo, found := genericapirequest.RequestInfoFrom(ctx)
		if !found {
			responsewriters.InternalError(w, req, errors.New("no RequestInfo found in the context"))
			return
		}

		gv := schema.GroupVersion{Group: requestInfo.APIGroup, Version: requestInfo.APIVersion}
		responsewriters.ErrorNegotiated(apierrors.NewUnauthorized("Unauthorized"), s, gv, w, req)
	})
}

func audiencesAreAcceptable(apiAuds, responseAudiences authenticator.Audiences) bool {
	if len(apiAuds) == 0 || len(responseAudiences) == 0 {
		return true
	}

	return len(apiAuds.Intersect(responseAudiences)) > 0
}

func shouldCloseAnonymousHTTP2Request(req *http.Request, resp *authenticator.Response) bool {
	return utilfeature.DefaultFeatureGate.Enabled(genericfeatures.UnauthenticatedHTTP2DOSMitigation) &&
		req.ProtoMajor == 2 &&
		!resp.IsProxiedAuthentication &&
		isAnonymousUser(resp.User)
}

func isAnonymousUser(u user.Info) bool {
	if u.GetName() == user.Anonymous {
		return true
	}
	for _, group := range u.GetGroups() {
		if group == user.AllUnauthenticated {
			return true
		}
	}
	return false
}
