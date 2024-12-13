// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package semconv // import "go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp/internal/semconv"

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
)

type ResponseTelemetry struct {
	StatusCode int
	ReadBytes  int64
	ReadError  error
	WriteBytes int64
	WriteError error
}

type HTTPServer struct {
	duplicate bool

	// Old metrics
	requestBytesCounter  metric.Int64Counter
	responseBytesCounter metric.Int64Counter
	serverLatencyMeasure metric.Float64Histogram
}

// RequestTraceAttrs returns trace attributes for an HTTP request received by a
// server.
//
// The server must be the primary server name if it is known. For example this
// would be the ServerName directive
// (https://httpd.apache.org/docs/2.4/mod/core.html#servername) for an Apache
// server, and the server_name directive
// (http://nginx.org/en/docs/http/ngx_http_core_module.html#server_name) for an
// nginx server. More generically, the primary server name would be the host
// header value that matches the default virtual host of an HTTP server. It
// should include the host identifier and if a port is used to route to the
// server that port identifier should be included as an appropriate port
// suffix.
//
// If the primary server name is not known, server should be an empty string.
// The req Host will be used to determine the server instead.
func (s HTTPServer) RequestTraceAttrs(server string, req *http.Request) []attribute.KeyValue {
	if s.duplicate {
		return append(oldHTTPServer{}.RequestTraceAttrs(server, req), newHTTPServer{}.RequestTraceAttrs(server, req)...)
	}
	return oldHTTPServer{}.RequestTraceAttrs(server, req)
}

// ResponseTraceAttrs returns trace attributes for telemetry from an HTTP response.
//
// If any of the fields in the ResponseTelemetry are not set the attribute will be omitted.
func (s HTTPServer) ResponseTraceAttrs(resp ResponseTelemetry) []attribute.KeyValue {
	if s.duplicate {
		return append(oldHTTPServer{}.ResponseTraceAttrs(resp), newHTTPServer{}.ResponseTraceAttrs(resp)...)
	}
	return oldHTTPServer{}.ResponseTraceAttrs(resp)
}

// Route returns the attribute for the route.
func (s HTTPServer) Route(route string) attribute.KeyValue {
	return oldHTTPServer{}.Route(route)
}

// Status returns a span status code and message for an HTTP status code
// value returned by a server. Status codes in the 400-499 range are not
// returned as errors.
func (s HTTPServer) Status(code int) (codes.Code, string) {
	if code < 100 || code >= 600 {
		return codes.Error, fmt.Sprintf("Invalid HTTP status code %d", code)
	}
	if code >= 500 {
		return codes.Error, ""
	}
	return codes.Unset, ""
}

type ServerMetricData struct {
	ServerName   string
	ResponseSize int64

	MetricData
	MetricAttributes
}

type MetricAttributes struct {
	Req                  *http.Request
	StatusCode           int
	AdditionalAttributes []attribute.KeyValue
}

type MetricData struct {
	RequestSize int64
	ElapsedTime float64
}

func (s HTTPServer) RecordMetrics(ctx context.Context, md ServerMetricData) {
	if s.requestBytesCounter == nil || s.responseBytesCounter == nil || s.serverLatencyMeasure == nil {
		// This will happen if an HTTPServer{} is used insted of NewHTTPServer.
		return
	}

	attributes := oldHTTPServer{}.MetricAttributes(md.ServerName, md.Req, md.StatusCode, md.AdditionalAttributes)
	o := metric.WithAttributeSet(attribute.NewSet(attributes...))
	addOpts := []metric.AddOption{o}
	s.requestBytesCounter.Add(ctx, md.RequestSize, addOpts...)
	s.responseBytesCounter.Add(ctx, md.ResponseSize, addOpts...)
	s.serverLatencyMeasure.Record(ctx, md.ElapsedTime, o)

	// TODO: Duplicate Metrics
}

func NewHTTPServer(meter metric.Meter) HTTPServer {
	env := strings.ToLower(os.Getenv("OTEL_SEMCONV_STABILITY_OPT_IN"))
	duplicate := env == "http/dup"
	server := HTTPServer{
		duplicate: duplicate,
	}
	server.requestBytesCounter, server.responseBytesCounter, server.serverLatencyMeasure = oldHTTPServer{}.createMeasures(meter)
	return server
}

type HTTPClient struct {
	duplicate bool

	// old metrics
	requestBytesCounter  metric.Int64Counter
	responseBytesCounter metric.Int64Counter
	latencyMeasure       metric.Float64Histogram
}

func NewHTTPClient(meter metric.Meter) HTTPClient {
	env := strings.ToLower(os.Getenv("OTEL_SEMCONV_STABILITY_OPT_IN"))
	client := HTTPClient{
		duplicate: env == "http/dup",
	}
	client.requestBytesCounter, client.responseBytesCounter, client.latencyMeasure = oldHTTPClient{}.createMeasures(meter)
	return client
}

// RequestTraceAttrs returns attributes for an HTTP request made by a client.
func (c HTTPClient) RequestTraceAttrs(req *http.Request) []attribute.KeyValue {
	if c.duplicate {
		return append(oldHTTPClient{}.RequestTraceAttrs(req), newHTTPClient{}.RequestTraceAttrs(req)...)
	}
	return oldHTTPClient{}.RequestTraceAttrs(req)
}

// ResponseTraceAttrs returns metric attributes for an HTTP request made by a client.
func (c HTTPClient) ResponseTraceAttrs(resp *http.Response) []attribute.KeyValue {
	if c.duplicate {
		return append(oldHTTPClient{}.ResponseTraceAttrs(resp), newHTTPClient{}.ResponseTraceAttrs(resp)...)
	}

	return oldHTTPClient{}.ResponseTraceAttrs(resp)
}

func (c HTTPClient) Status(code int) (codes.Code, string) {
	if code < 100 || code >= 600 {
		return codes.Error, fmt.Sprintf("Invalid HTTP status code %d", code)
	}
	if code >= 400 {
		return codes.Error, ""
	}
	return codes.Unset, ""
}

func (c HTTPClient) ErrorType(err error) attribute.KeyValue {
	if c.duplicate {
		return newHTTPClient{}.ErrorType(err)
	}

	return attribute.KeyValue{}
}

type MetricOpts struct {
	measurement metric.MeasurementOption
	addOptions  metric.AddOption
}

func (o MetricOpts) MeasurementOption() metric.MeasurementOption {
	return o.measurement
}

func (o MetricOpts) AddOptions() metric.AddOption {
	return o.addOptions
}

func (c HTTPClient) MetricOptions(ma MetricAttributes) MetricOpts {
	attributes := oldHTTPClient{}.MetricAttributes(ma.Req, ma.StatusCode, ma.AdditionalAttributes)
	// TODO: Duplicate Metrics
	set := metric.WithAttributeSet(attribute.NewSet(attributes...))
	return MetricOpts{
		measurement: set,
		addOptions:  set,
	}
}

func (s HTTPClient) RecordMetrics(ctx context.Context, md MetricData, opts MetricOpts) {
	if s.requestBytesCounter == nil || s.latencyMeasure == nil {
		// This will happen if an HTTPClient{} is used insted of NewHTTPClient().
		return
	}

	s.requestBytesCounter.Add(ctx, md.RequestSize, opts.AddOptions())
	s.latencyMeasure.Record(ctx, md.ElapsedTime, opts.MeasurementOption())

	// TODO: Duplicate Metrics
}

func (s HTTPClient) RecordResponseSize(ctx context.Context, responseData int64, opts metric.AddOption) {
	if s.responseBytesCounter == nil {
		// This will happen if an HTTPClient{} is used insted of NewHTTPClient().
		return
	}

	s.responseBytesCounter.Add(ctx, responseData, opts)
	// TODO: Duplicate Metrics
}
