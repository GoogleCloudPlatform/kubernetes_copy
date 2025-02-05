/*
Copyright 2017 The Kubernetes Authors.

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
	"errors"
	"fmt"
	"net/http"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	auditinternal "k8s.io/apiserver/pkg/apis/audit"
	"k8s.io/apiserver/pkg/audit"
	"k8s.io/apiserver/pkg/endpoints/handlers/responsewriters"
)

// WithFailedAuthenticationAudit decorates a failed http.Handler used in WithAuthentication handler.
// It is meant to log only failed authentication requests.
func WithFailedAuthenticationAudit(failedHandler http.Handler, sink audit.Sink, policy audit.PolicyRuleEvaluator) http.Handler {
	if sink == nil || policy == nil {
		return failedHandler
	}
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		ac, err := evaluatePolicyAndCreateAuditEvent(req, policy)
		if err != nil {
			utilruntime.HandleError(fmt.Errorf("failed to create audit event: %v", err))
			responsewriters.InternalError(w, req, errors.New("failed to create audit event"))
			return
		}

		if !ac.Enabled() {
			failedHandler.ServeHTTP(w, req)
			return
		}

		ac.SetEventResponseStatus(&metav1.Status{
			Message: getAuthMethods(req),
		})
		ac.SetEventStage(auditinternal.StageResponseStarted)

		rw := decorateResponseWriter(req.Context(), w, sink, ac.RequestAuditConfig.OmitStages)
		failedHandler.ServeHTTP(rw, req)
	})
}

func getAuthMethods(req *http.Request) string {
	authMethods := []string{}

	if _, _, ok := req.BasicAuth(); ok {
		authMethods = append(authMethods, "basic")
	}

	auth := strings.TrimSpace(req.Header.Get("Authorization"))
	parts := strings.Split(auth, " ")
	if len(parts) > 1 && strings.ToLower(parts[0]) == "bearer" {
		authMethods = append(authMethods, "bearer")
	}

	token := strings.TrimSpace(req.URL.Query().Get("access_token"))
	if len(token) > 0 {
		authMethods = append(authMethods, "access_token")
	}

	if req.TLS != nil && len(req.TLS.PeerCertificates) > 0 {
		authMethods = append(authMethods, "x509")
	}

	if len(authMethods) > 0 {
		return fmt.Sprintf("Authentication failed, attempted: %s", strings.Join(authMethods, ", "))
	}
	return "Authentication failed, no credentials provided"
}
