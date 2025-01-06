// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package instrumentation // import "go.opentelemetry.io/otel/sdk/instrumentation"

import "go.opentelemetry.io/otel/attribute"

// Scope represents the instrumentation scope.
type Scope struct {
	// Name is the name of the instrumentation scope. This should be the
	// Go package name of that scope.
	Name string
	// Version is the version of the instrumentation scope.
	Version string
	// SchemaURL of the telemetry emitted by the scope.
	SchemaURL string
	// Attributes of the telemetry emitted by the scope.
	Attributes attribute.Set
}
