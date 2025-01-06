// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

// Package otelrestful instruments github.com/emicklei/go-restful.
//
// Instrumentation is provided to trace the emicklei/go-restful/v3
// package (https://github.com/emicklei/go-restful).
//
// Instrumentation of an incoming request is achieved via a go-restful
// FilterFunc called `OTelFilterFunc` which may be applied at any one of
//   - the container level
//   - webservice level
//   - route level
package otelrestful // import "go.opentelemetry.io/contrib/instrumentation/github.com/emicklei/go-restful/otelrestful"
