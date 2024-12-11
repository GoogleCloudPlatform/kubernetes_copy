// Copyright 2016 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package promhttp provides tooling around HTTP servers and clients.
//
// First, the package allows the creation of http.Handler instances to expose
// Prometheus metrics via HTTP. promhttp.Handler acts on the
// prometheus.DefaultGatherer. With HandlerFor, you can create a handler for a
// custom registry or anything that implements the Gatherer interface. It also
// allows the creation of handlers that act differently on errors or allow to
// log errors.
//
// Second, the package provides tooling to instrument instances of http.Handler
// via middleware. Middleware wrappers follow the naming scheme
// InstrumentHandlerX, where X describes the intended use of the middleware.
// See each function's doc comment for specific details.
//
// Finally, the package allows for an http.RoundTripper to be instrumented via
// middleware. Middleware wrappers follow the naming scheme
// InstrumentRoundTripperX, where X describes the intended use of the
// middleware. See each function's doc comment for specific details.
package promhttp

import (
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/prometheus/common/expfmt"

	"github.com/prometheus/client_golang/internal/github.com/golang/gddo/httputil"
	"github.com/prometheus/client_golang/prometheus"
)

const (
	contentTypeHeader      = "Content-Type"
	contentEncodingHeader  = "Content-Encoding"
	acceptEncodingHeader   = "Accept-Encoding"
	processStartTimeHeader = "Process-Start-Time-Unix"
)

// Compression represents the content encodings handlers support for the HTTP
// responses.
type Compression string

const (
	Identity Compression = "identity"
	Gzip     Compression = "gzip"
	Zstd     Compression = "zstd"
)

var defaultCompressionFormats = []Compression{Identity, Gzip, Zstd}

var gzipPool = sync.Pool{
	New: func() interface{} {
		return gzip.NewWriter(nil)
	},
}

// Handler returns an http.Handler for the prometheus.DefaultGatherer, using
// default HandlerOpts, i.e. it reports the first error as an HTTP error, it has
// no error logging, and it applies compression if requested by the client.
//
// The returned http.Handler is already instrumented using the
// InstrumentMetricHandler function and the prometheus.DefaultRegisterer. If you
// create multiple http.Handlers by separate calls of the Handler function, the
// metrics used for instrumentation will be shared between them, providing
// global scrape counts.
//
// This function is meant to cover the bulk of basic use cases. If you are doing
// anything that requires more customization (including using a non-default
// Gatherer, different instrumentation, and non-default HandlerOpts), use the
// HandlerFor function. See there for details.
func Handler() http.Handler {
	return InstrumentMetricHandler(
		prometheus.DefaultRegisterer, HandlerFor(prometheus.DefaultGatherer, HandlerOpts{}),
	)
}

// HandlerFor returns an uninstrumented http.Handler for the provided
// Gatherer. The behavior of the Handler is defined by the provided
// HandlerOpts. Thus, HandlerFor is useful to create http.Handlers for custom
// Gatherers, with non-default HandlerOpts, and/or with custom (or no)
// instrumentation. Use the InstrumentMetricHandler function to apply the same
// kind of instrumentation as it is used by the Handler function.
func HandlerFor(reg prometheus.Gatherer, opts HandlerOpts) http.Handler {
	return HandlerForTransactional(prometheus.ToTransactionalGatherer(reg), opts)
}

// HandlerForTransactional is like HandlerFor, but it uses transactional gather, which
// can safely change in-place returned *dto.MetricFamily before call to `Gather` and after
// call to `done` of that `Gather`.
func HandlerForTransactional(reg prometheus.TransactionalGatherer, opts HandlerOpts) http.Handler {
	var (
		inFlightSem chan struct{}
		errCnt      = prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "promhttp_metric_handler_errors_total",
				Help: "Total number of internal errors encountered by the promhttp metric handler.",
			},
			[]string{"cause"},
		)
	)

	if opts.MaxRequestsInFlight > 0 {
		inFlightSem = make(chan struct{}, opts.MaxRequestsInFlight)
	}
	if opts.Registry != nil {
		// Initialize all possibilities that can occur below.
		errCnt.WithLabelValues("gathering")
		errCnt.WithLabelValues("encoding")
		if err := opts.Registry.Register(errCnt); err != nil {
			are := &prometheus.AlreadyRegisteredError{}
			if errors.As(err, are) {
				errCnt = are.ExistingCollector.(*prometheus.CounterVec)
			} else {
				panic(err)
			}
		}
	}

	// Select compression formats to offer based on default or user choice.
	var compressions []string
	if !opts.DisableCompression {
		offers := defaultCompressionFormats
		if len(opts.OfferedCompressions) > 0 {
			offers = opts.OfferedCompressions
		}
		for _, comp := range offers {
			compressions = append(compressions, string(comp))
		}
	}

	h := http.HandlerFunc(func(rsp http.ResponseWriter, req *http.Request) {
		if !opts.ProcessStartTime.IsZero() {
			rsp.Header().Set(processStartTimeHeader, strconv.FormatInt(opts.ProcessStartTime.Unix(), 10))
		}
		if inFlightSem != nil {
			select {
			case inFlightSem <- struct{}{}: // All good, carry on.
				defer func() { <-inFlightSem }()
			default:
				http.Error(rsp, fmt.Sprintf(
					"Limit of concurrent requests reached (%d), try again later.", opts.MaxRequestsInFlight,
				), http.StatusServiceUnavailable)
				return
			}
		}
		mfs, done, err := reg.Gather()
		defer done()
		if err != nil {
			if opts.ErrorLog != nil {
				opts.ErrorLog.Println("error gathering metrics:", err)
			}
			errCnt.WithLabelValues("gathering").Inc()
			switch opts.ErrorHandling {
			case PanicOnError:
				panic(err)
			case ContinueOnError:
				if len(mfs) == 0 {
					// Still report the error if no metrics have been gathered.
					httpError(rsp, err)
					return
				}
			case HTTPErrorOnError:
				httpError(rsp, err)
				return
			}
		}

		var contentType expfmt.Format
		if opts.EnableOpenMetrics {
			contentType = expfmt.NegotiateIncludingOpenMetrics(req.Header)
		} else {
			contentType = expfmt.Negotiate(req.Header)
		}
		rsp.Header().Set(contentTypeHeader, string(contentType))

		w, encodingHeader, closeWriter, err := negotiateEncodingWriter(req, rsp, compressions)
		if err != nil {
			if opts.ErrorLog != nil {
				opts.ErrorLog.Println("error getting writer", err)
			}
			w = io.Writer(rsp)
			encodingHeader = string(Identity)
		}

		defer closeWriter()

		// Set Content-Encoding only when data is compressed
		if encodingHeader != string(Identity) {
			rsp.Header().Set(contentEncodingHeader, encodingHeader)
		}
		enc := expfmt.NewEncoder(w, contentType)

		// handleError handles the error according to opts.ErrorHandling
		// and returns true if we have to abort after the handling.
		handleError := func(err error) bool {
			if err == nil {
				return false
			}
			if opts.ErrorLog != nil {
				opts.ErrorLog.Println("error encoding and sending metric family:", err)
			}
			errCnt.WithLabelValues("encoding").Inc()
			switch opts.ErrorHandling {
			case PanicOnError:
				panic(err)
			case HTTPErrorOnError:
				// We cannot really send an HTTP error at this
				// point because we most likely have written
				// something to rsp already. But at least we can
				// stop sending.
				return true
			}
			// Do nothing in all other cases, including ContinueOnError.
			return false
		}

		for _, mf := range mfs {
			if handleError(enc.Encode(mf)) {
				return
			}
		}
		if closer, ok := enc.(expfmt.Closer); ok {
			// This in particular takes care of the final "# EOF\n" line for OpenMetrics.
			if handleError(closer.Close()) {
				return
			}
		}
	})

	if opts.Timeout <= 0 {
		return h
	}
	return http.TimeoutHandler(h, opts.Timeout, fmt.Sprintf(
		"Exceeded configured timeout of %v.\n",
		opts.Timeout,
	))
}

// InstrumentMetricHandler is usually used with an http.Handler returned by the
// HandlerFor function. It instruments the provided http.Handler with two
// metrics: A counter vector "promhttp_metric_handler_requests_total" to count
// scrapes partitioned by HTTP status code, and a gauge
// "promhttp_metric_handler_requests_in_flight" to track the number of
// simultaneous scrapes. This function idempotently registers collectors for
// both metrics with the provided Registerer. It panics if the registration
// fails. The provided metrics are useful to see how many scrapes hit the
// monitored target (which could be from different Prometheus servers or other
// scrapers), and how often they overlap (which would result in more than one
// scrape in flight at the same time). Note that the scrapes-in-flight gauge
// will contain the scrape by which it is exposed, while the scrape counter will
// only get incremented after the scrape is complete (as only then the status
// code is known). For tracking scrape durations, use the
// "scrape_duration_seconds" gauge created by the Prometheus server upon each
// scrape.
func InstrumentMetricHandler(reg prometheus.Registerer, handler http.Handler) http.Handler {
	cnt := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "promhttp_metric_handler_requests_total",
			Help: "Total number of scrapes by HTTP status code.",
		},
		[]string{"code"},
	)
	// Initialize the most likely HTTP status codes.
	cnt.WithLabelValues("200")
	cnt.WithLabelValues("500")
	cnt.WithLabelValues("503")
	if err := reg.Register(cnt); err != nil {
		are := &prometheus.AlreadyRegisteredError{}
		if errors.As(err, are) {
			cnt = are.ExistingCollector.(*prometheus.CounterVec)
		} else {
			panic(err)
		}
	}

	gge := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "promhttp_metric_handler_requests_in_flight",
		Help: "Current number of scrapes being served.",
	})
	if err := reg.Register(gge); err != nil {
		are := &prometheus.AlreadyRegisteredError{}
		if errors.As(err, are) {
			gge = are.ExistingCollector.(prometheus.Gauge)
		} else {
			panic(err)
		}
	}

	return InstrumentHandlerCounter(cnt, InstrumentHandlerInFlight(gge, handler))
}

// HandlerErrorHandling defines how a Handler serving metrics will handle
// errors.
type HandlerErrorHandling int

// These constants cause handlers serving metrics to behave as described if
// errors are encountered.
const (
	// Serve an HTTP status code 500 upon the first error
	// encountered. Report the error message in the body. Note that HTTP
	// errors cannot be served anymore once the beginning of a regular
	// payload has been sent. Thus, in the (unlikely) case that encoding the
	// payload into the negotiated wire format fails, serving the response
	// will simply be aborted. Set an ErrorLog in HandlerOpts to detect
	// those errors.
	HTTPErrorOnError HandlerErrorHandling = iota
	// Ignore errors and try to serve as many metrics as possible.  However,
	// if no metrics can be served, serve an HTTP status code 500 and the
	// last error message in the body. Only use this in deliberate "best
	// effort" metrics collection scenarios. In this case, it is highly
	// recommended to provide other means of detecting errors: By setting an
	// ErrorLog in HandlerOpts, the errors are logged. By providing a
	// Registry in HandlerOpts, the exposed metrics include an error counter
	// "promhttp_metric_handler_errors_total", which can be used for
	// alerts.
	ContinueOnError
	// Panic upon the first error encountered (useful for "crash only" apps).
	PanicOnError
)

// Logger is the minimal interface HandlerOpts needs for logging. Note that
// log.Logger from the standard library implements this interface, and it is
// easy to implement by custom loggers, if they don't do so already anyway.
type Logger interface {
	Println(v ...interface{})
}

// HandlerOpts specifies options how to serve metrics via an http.Handler. The
// zero value of HandlerOpts is a reasonable default.
type HandlerOpts struct {
	// ErrorLog specifies an optional Logger for errors collecting and
	// serving metrics. If nil, errors are not logged at all. Note that the
	// type of a reported error is often prometheus.MultiError, which
	// formats into a multi-line error string. If you want to avoid the
	// latter, create a Logger implementation that detects a
	// prometheus.MultiError and formats the contained errors into one line.
	ErrorLog Logger
	// ErrorHandling defines how errors are handled. Note that errors are
	// logged regardless of the configured ErrorHandling provided ErrorLog
	// is not nil.
	ErrorHandling HandlerErrorHandling
	// If Registry is not nil, it is used to register a metric
	// "promhttp_metric_handler_errors_total", partitioned by "cause". A
	// failed registration causes a panic. Note that this error counter is
	// different from the instrumentation you get from the various
	// InstrumentHandler... helpers. It counts errors that don't necessarily
	// result in a non-2xx HTTP status code. There are two typical cases:
	// (1) Encoding errors that only happen after streaming of the HTTP body
	// has already started (and the status code 200 has been sent). This
	// should only happen with custom collectors. (2) Collection errors with
	// no effect on the HTTP status code because ErrorHandling is set to
	// ContinueOnError.
	Registry prometheus.Registerer
	// DisableCompression disables the response encoding (compression) and
	// encoding negotiation. If true, the handler will
	// never compress the response, even if requested
	// by the client and the OfferedCompressions field is set.
	DisableCompression bool
	// OfferedCompressions is a set of encodings (compressions) handler will
	// try to offer when negotiating with the client. This defaults to identity, gzip
	// and zstd.
	// NOTE: If handler can't agree with the client on the encodings or
	// unsupported or empty encodings are set in OfferedCompressions,
	// handler always fallbacks to no compression (identity), for
	// compatibility reasons. In such cases ErrorLog will be used if set.
	OfferedCompressions []Compression
	// The number of concurrent HTTP requests is limited to
	// MaxRequestsInFlight. Additional requests are responded to with 503
	// Service Unavailable and a suitable message in the body. If
	// MaxRequestsInFlight is 0 or negative, no limit is applied.
	MaxRequestsInFlight int
	// If handling a request takes longer than Timeout, it is responded to
	// with 503 ServiceUnavailable and a suitable Message. No timeout is
	// applied if Timeout is 0 or negative. Note that with the current
	// implementation, reaching the timeout simply ends the HTTP requests as
	// described above (and even that only if sending of the body hasn't
	// started yet), while the bulk work of gathering all the metrics keeps
	// running in the background (with the eventual result to be thrown
	// away). Until the implementation is improved, it is recommended to
	// implement a separate timeout in potentially slow Collectors.
	Timeout time.Duration
	// If true, the experimental OpenMetrics encoding is added to the
	// possible options during content negotiation. Note that Prometheus
	// 2.5.0+ will negotiate OpenMetrics as first priority. OpenMetrics is
	// the only way to transmit exemplars. However, the move to OpenMetrics
	// is not completely transparent. Most notably, the values of "quantile"
	// labels of Summaries and "le" labels of Histograms are formatted with
	// a trailing ".0" if they would otherwise look like integer numbers
	// (which changes the identity of the resulting series on the Prometheus
	// server).
	EnableOpenMetrics bool
	// ProcessStartTime allows setting process start timevalue that will be exposed
	// with "Process-Start-Time-Unix" response header along with the metrics
	// payload. This allow callers to have efficient transformations to cumulative
	// counters (e.g. OpenTelemetry) or generally _created timestamp estimation per
	// scrape target.
	// NOTE: This feature is experimental and not covered by OpenMetrics or Prometheus
	// exposition format.
	ProcessStartTime time.Time
}

// httpError removes any content-encoding header and then calls http.Error with
// the provided error and http.StatusInternalServerError. Error contents is
// supposed to be uncompressed plain text. Same as with a plain http.Error, this
// must not be called if the header or any payload has already been sent.
func httpError(rsp http.ResponseWriter, err error) {
	rsp.Header().Del(contentEncodingHeader)
	http.Error(
		rsp,
		"An error has occurred while serving metrics:\n\n"+err.Error(),
		http.StatusInternalServerError,
	)
}

// negotiateEncodingWriter reads the Accept-Encoding header from a request and
// selects the right compression based on an allow-list of supported
// compressions. It returns a writer implementing the compression and an the
// correct value that the caller can set in the response header.
func negotiateEncodingWriter(r *http.Request, rw io.Writer, compressions []string) (_ io.Writer, encodingHeaderValue string, closeWriter func(), _ error) {
	if len(compressions) == 0 {
		return rw, string(Identity), func() {}, nil
	}

	// TODO(mrueg): Replace internal/github.com/gddo once https://github.com/golang/go/issues/19307 is implemented.
	selected := httputil.NegotiateContentEncoding(r, compressions)

	switch selected {
	case "zstd":
		// TODO(mrueg): Replace klauspost/compress with stdlib implementation once https://github.com/golang/go/issues/62513 is implemented.
		z, err := zstd.NewWriter(rw, zstd.WithEncoderLevel(zstd.SpeedFastest))
		if err != nil {
			return nil, "", func() {}, err
		}

		z.Reset(rw)
		return z, selected, func() { _ = z.Close() }, nil
	case "gzip":
		gz := gzipPool.Get().(*gzip.Writer)
		gz.Reset(rw)
		return gz, selected, func() { _ = gz.Close(); gzipPool.Put(gz) }, nil
	case "identity":
		// This means the content is not compressed.
		return rw, selected, func() {}, nil
	default:
		// The content encoding was not implemented yet.
		return nil, "", func() {}, fmt.Errorf("content compression format not recognized: %s. Valid formats are: %s", selected, defaultCompressionFormats)
	}
}
