// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package resource // import "go.opentelemetry.io/otel/sdk/resource"

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/google/uuid"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

type (
	// telemetrySDK is a Detector that provides information about
	// the OpenTelemetry SDK used.  This Detector is included as a
	// builtin. If these resource attributes are not wanted, use
	// resource.New() to explicitly disable them.
	telemetrySDK struct{}

	// host is a Detector that provides information about the host
	// being run on. This Detector is included as a builtin. If
	// these resource attributes are not wanted, use the
	// resource.New() to explicitly disable them.
	host struct{}

	stringDetector struct {
		schemaURL string
		K         attribute.Key
		F         func() (string, error)
	}

	defaultServiceNameDetector struct{}

	defaultServiceInstanceIDDetector struct{}
)

var (
	_ Detector = telemetrySDK{}
	_ Detector = host{}
	_ Detector = stringDetector{}
	_ Detector = defaultServiceNameDetector{}
	_ Detector = defaultServiceInstanceIDDetector{}
)

// Detect returns a *Resource that describes the OpenTelemetry SDK used.
func (telemetrySDK) Detect(context.Context) (*Resource, error) {
	return NewWithAttributes(
		semconv.SchemaURL,
		semconv.TelemetrySDKName("opentelemetry"),
		semconv.TelemetrySDKLanguageGo,
		semconv.TelemetrySDKVersion(sdk.Version()),
	), nil
}

// Detect returns a *Resource that describes the host being run on.
func (host) Detect(ctx context.Context) (*Resource, error) {
	return StringDetector(semconv.SchemaURL, semconv.HostNameKey, os.Hostname).Detect(ctx)
}

// StringDetector returns a Detector that will produce a *Resource
// containing the string as a value corresponding to k. The resulting Resource
// will have the specified schemaURL.
func StringDetector(schemaURL string, k attribute.Key, f func() (string, error)) Detector {
	return stringDetector{schemaURL: schemaURL, K: k, F: f}
}

// Detect returns a *Resource that describes the string as a value
// corresponding to attribute.Key as well as the specific schemaURL.
func (sd stringDetector) Detect(ctx context.Context) (*Resource, error) {
	value, err := sd.F()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", string(sd.K), err)
	}
	a := sd.K.String(value)
	if !a.Valid() {
		return nil, fmt.Errorf("invalid attribute: %q -> %q", a.Key, a.Value.Emit())
	}
	return NewWithAttributes(sd.schemaURL, sd.K.String(value)), nil
}

// Detect implements Detector.
func (defaultServiceNameDetector) Detect(ctx context.Context) (*Resource, error) {
	return StringDetector(
		semconv.SchemaURL,
		semconv.ServiceNameKey,
		func() (string, error) {
			executable, err := os.Executable()
			if err != nil {
				return "unknown_service:go", nil
			}
			return "unknown_service:" + filepath.Base(executable), nil
		},
	).Detect(ctx)
}

// Detect implements Detector.
func (defaultServiceInstanceIDDetector) Detect(ctx context.Context) (*Resource, error) {
	return StringDetector(
		semconv.SchemaURL,
		semconv.ServiceInstanceIDKey,
		func() (string, error) {
			version4Uuid, err := uuid.NewRandom()
			if err != nil {
				return "", err
			}

			return version4Uuid.String(), nil
		},
	).Detect(ctx)
}
