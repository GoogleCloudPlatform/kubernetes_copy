// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package otelrestful // import "go.opentelemetry.io/contrib/instrumentation/github.com/emicklei/go-restful/otelrestful"

// Version is the current release version of the go-restful instrumentation.
func Version() string {
	return "0.58.0"
	// This string is updated by the pre_release.sh script during release
}

// SemVersion is the semantic version to be supplied to tracer/meter creation.
//
// Deprecated: Use [Version] instead.
func SemVersion() string {
	return Version()
}
