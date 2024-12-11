# OpenTelemetry-Go

[![CI](https://github.com/open-telemetry/opentelemetry-go/workflows/ci/badge.svg)](https://github.com/open-telemetry/opentelemetry-go/actions?query=workflow%3Aci+branch%3Amain)
[![codecov.io](https://codecov.io/gh/open-telemetry/opentelemetry-go/coverage.svg?branch=main)](https://app.codecov.io/gh/open-telemetry/opentelemetry-go?branch=main)
[![PkgGoDev](https://pkg.go.dev/badge/go.opentelemetry.io/otel)](https://pkg.go.dev/go.opentelemetry.io/otel)
[![Go Report Card](https://goreportcard.com/badge/go.opentelemetry.io/otel)](https://goreportcard.com/report/go.opentelemetry.io/otel)
[![Slack](https://img.shields.io/badge/slack-@cncf/otel--go-brightgreen.svg?logo=slack)](https://cloud-native.slack.com/archives/C01NPAXACKT)

OpenTelemetry-Go is the [Go](https://golang.org/) implementation of [OpenTelemetry](https://opentelemetry.io/).
It provides a set of APIs to directly measure performance and behavior of your software and send this data to observability platforms.

## Project Status

| Signal  | Status             |
|---------|--------------------|
| Traces  | Stable             |
| Metrics | Stable             |
| Logs    | Beta[^1]           |

Progress and status specific to this repository is tracked in our
[project boards](https://github.com/open-telemetry/opentelemetry-go/projects)
and
[milestones](https://github.com/open-telemetry/opentelemetry-go/milestones).

Project versioning information and stability guarantees can be found in the
[versioning documentation](VERSIONING.md).

[^1]: https://github.com/orgs/open-telemetry/projects/43

### Compatibility

OpenTelemetry-Go ensures compatibility with the current supported versions of
the [Go language](https://golang.org/doc/devel/release#policy):

> Each major Go release is supported until there are two newer major releases.
> For example, Go 1.5 was supported until the Go 1.7 release, and Go 1.6 was supported until the Go 1.8 release.

For versions of Go that are no longer supported upstream, opentelemetry-go will
stop ensuring compatibility with these versions in the following manner:

- A minor release of opentelemetry-go will be made to add support for the new
  supported release of Go.
- The following minor release of opentelemetry-go will remove compatibility
  testing for the oldest (now archived upstream) version of Go. This, and
  future, releases of opentelemetry-go may include features only supported by
  the currently supported versions of Go.

Currently, this project supports the following environments.

| OS       | Go Version | Architecture |
|----------|------------|--------------|
| Ubuntu   | 1.23       | amd64        |
| Ubuntu   | 1.22       | amd64        |
| Ubuntu   | 1.23       | 386          |
| Ubuntu   | 1.22       | 386          |
| Linux    | 1.23       | arm64        |
| Linux    | 1.22       | arm64        |
| macOS 13 | 1.23       | amd64        |
| macOS 13 | 1.22       | amd64        |
| macOS    | 1.23       | arm64        |
| macOS    | 1.22       | arm64        |
| Windows  | 1.23       | amd64        |
| Windows  | 1.22       | amd64        |
| Windows  | 1.23       | 386          |
| Windows  | 1.22       | 386          |

While this project should work for other systems, no compatibility guarantees
are made for those systems currently.

## Getting Started

You can find a getting started guide on [opentelemetry.io](https://opentelemetry.io/docs/languages/go/getting-started/).

OpenTelemetry's goal is to provide a single set of APIs to capture distributed
traces and metrics from your application and send them to an observability
platform. This project allows you to do just that for applications written in
Go. There are two steps to this process: instrument your application, and
configure an exporter.

### Instrumentation

To start capturing distributed traces and metric events from your application
it first needs to be instrumented. The easiest way to do this is by using an
instrumentation library for your code. Be sure to check out [the officially
supported instrumentation
libraries](https://github.com/open-telemetry/opentelemetry-go-contrib/tree/main/instrumentation).

If you need to extend the telemetry an instrumentation library provides or want
to build your own instrumentation for your application directly you will need
to use the
[Go otel](https://pkg.go.dev/go.opentelemetry.io/otel)
package. The [examples](https://github.com/open-telemetry/opentelemetry-go-contrib/tree/main/examples)
are a good way to see some practical uses of this process.

### Export

Now that your application is instrumented to collect telemetry, it needs an
export pipeline to send that telemetry to an observability platform.

All officially supported exporters for the OpenTelemetry project are contained in the [exporters directory](./exporters).

| Exporter                              | Logs | Metrics | Traces |
|---------------------------------------|:----:|:-------:|:------:|
| [OTLP](./exporters/otlp/)             |  ✓   |    ✓    |   ✓    |
| [Prometheus](./exporters/prometheus/) |      |    ✓    |        |
| [stdout](./exporters/stdout/)         |  ✓   |    ✓    |   ✓    |
| [Zipkin](./exporters/zipkin/)         |      |         |   ✓    |

## Contributing

See the [contributing documentation](CONTRIBUTING.md).
