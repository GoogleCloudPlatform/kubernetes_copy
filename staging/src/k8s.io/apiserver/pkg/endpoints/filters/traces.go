/*
Copyright 2021 The Kubernetes Authors.

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
	"net/http"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/attribute"
	semconv "go.opentelemetry.io/otel/semconv/v1.17.0"
	"go.opentelemetry.io/otel/trace"
	"k8s.io/apiserver/pkg/audit"
	"k8s.io/apiserver/pkg/endpoints/request"

	tracing "k8s.io/component-base/tracing"
)

// WithTracing adds tracing to requests if the incoming request is sampled
func WithTracing(handler http.Handler, tp trace.TracerProvider) http.Handler {
	opts := []otelhttp.Option{
		otelhttp.WithPropagators(tracing.Propagators()),
		otelhttp.WithPublicEndpoint(),
		otelhttp.WithTracerProvider(tp),
		otelhttp.WithSpanNameFormatter(func(operation string, r *http.Request) string {
			ctx := r.Context()
			info, exist := request.RequestInfoFrom(ctx)
			if !exist || !info.IsResourceRequest {
				return r.Method
			}
			return getSpanNameFromRequestInfo(info, r)
		}),
	}
	wrappedHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Add the http.target attribute to the otelhttp span
		// Workaround for https://github.com/open-telemetry/opentelemetry-go-contrib/issues/3743
		span := trace.SpanFromContext(r.Context())
		if r.URL != nil {
			span.SetAttributes(semconv.HTTPTarget(r.URL.RequestURI()))
		}
		// Add auditID attribute if available. This helps us connect traces to audit log entries
		span.SetAttributes(attribute.String("audit-id", audit.GetAuditIDTruncated(r.Context())))
		handler.ServeHTTP(w, r)
	})
	// With Noop TracerProvider, the otelhttp still handles context propagation.
	// See https://github.com/open-telemetry/opentelemetry-go/tree/main/example/passthrough
	return otelhttp.NewHandler(wrappedHandler, "KubernetesAPI", opts...)
}

func getSpanNameFromRequestInfo(info *request.RequestInfo, r *http.Request) string {
	spanName := "/" + info.APIPrefix
	if info.APIGroup != "" {
		spanName += "/" + info.APIGroup
	}
	spanName += "/" + info.APIVersion
	if info.Namespace != "" {
		spanName += "/namespaces/{:namespace}"
	}
	spanName += "/" + info.Resource
	if info.Name != "" {
		spanName += "/" + "{:name}"
	}
	if info.Subresource != "" {
		spanName += "/" + info.Subresource
	}
	return r.Method + " " + spanName
}
