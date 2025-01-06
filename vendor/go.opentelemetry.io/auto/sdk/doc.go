// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

/*
Package sdk provides an auto-instrumentable OpenTelemetry SDK.

An [go.opentelemetry.io/auto.Instrumentation] can be configured to target the
process running this SDK. In that case, all telemetry the SDK produces will be
processed and handled by that [go.opentelemetry.io/auto.Instrumentation].

By default, if there is no [go.opentelemetry.io/auto.Instrumentation] set to
auto-instrument the SDK, the SDK will not generate any telemetry.
*/
package sdk
