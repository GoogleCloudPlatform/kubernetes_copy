# Contributing to go.opentelemetry.io/auto/sdk

The `go.opentelemetry.io/auto/sdk` module is a purpose built OpenTelemetry SDK.
It is designed to be:

0. An OpenTelemetry compliant SDK
1. Instrumented by auto-instrumentation (serializable into OTLP JSON)
2. Lightweight
3. User-friendly

These design choices are listed in the order of their importance.

The primary design goal of this module is to be an OpenTelemetry SDK.
This means that it needs to implement the Go APIs found in `go.opentelemetry.io/otel`.

Having met the requirement of SDK compliance, this module needs to provide code that the `go.opentelemetry.io/auto` module can instrument.
The chosen approach to meet this goal is to ensure the telemetry from the SDK is serializable into JSON encoded OTLP.
This ensures then that the serialized form is compatible with other OpenTelemetry systems, and the auto-instrumentation can use these systems to deserialize any telemetry it is sent.

Outside of these first two goals, the intended use becomes relevant.
This package is intended to be used in the `go.opentelemetry.io/otel` global API as a default when the auto-instrumentation is running.
Because of this, this package needs to not add unnecessary dependencies to that API.
Ideally, it adds none.
It also needs to operate efficiently.

Finally, this module is designed to be user-friendly to Go development.
It hides complexity in order to provide simpler APIs when the previous goals can all still be met.
