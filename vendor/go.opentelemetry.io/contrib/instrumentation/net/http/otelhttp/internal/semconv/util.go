// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package semconv // import "go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp/internal/semconv"

import (
	"net"
	"net/http"
	"strconv"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	semconvNew "go.opentelemetry.io/otel/semconv/v1.26.0"
)

// SplitHostPort splits a network address hostport of the form "host",
// "host%zone", "[host]", "[host%zone], "host:port", "host%zone:port",
// "[host]:port", "[host%zone]:port", or ":port" into host or host%zone and
// port.
//
// An empty host is returned if it is not provided or unparsable. A negative
// port is returned if it is not provided or unparsable.
func SplitHostPort(hostport string) (host string, port int) {
	port = -1

	if strings.HasPrefix(hostport, "[") {
		addrEnd := strings.LastIndex(hostport, "]")
		if addrEnd < 0 {
			// Invalid hostport.
			return
		}
		if i := strings.LastIndex(hostport[addrEnd:], ":"); i < 0 {
			host = hostport[1:addrEnd]
			return
		}
	} else {
		if i := strings.LastIndex(hostport, ":"); i < 0 {
			host = hostport
			return
		}
	}

	host, pStr, err := net.SplitHostPort(hostport)
	if err != nil {
		return
	}

	p, err := strconv.ParseUint(pStr, 10, 16)
	if err != nil {
		return
	}
	return host, int(p) // nolint: gosec  // Byte size checked 16 above.
}

func requiredHTTPPort(https bool, port int) int { // nolint:revive
	if https {
		if port > 0 && port != 443 {
			return port
		}
	} else {
		if port > 0 && port != 80 {
			return port
		}
	}
	return -1
}

func serverClientIP(xForwardedFor string) string {
	if idx := strings.Index(xForwardedFor, ","); idx >= 0 {
		xForwardedFor = xForwardedFor[:idx]
	}
	return xForwardedFor
}

func netProtocol(proto string) (name string, version string) {
	name, version, _ = strings.Cut(proto, "/")
	name = strings.ToLower(name)
	return name, version
}

var methodLookup = map[string]attribute.KeyValue{
	http.MethodConnect: semconvNew.HTTPRequestMethodConnect,
	http.MethodDelete:  semconvNew.HTTPRequestMethodDelete,
	http.MethodGet:     semconvNew.HTTPRequestMethodGet,
	http.MethodHead:    semconvNew.HTTPRequestMethodHead,
	http.MethodOptions: semconvNew.HTTPRequestMethodOptions,
	http.MethodPatch:   semconvNew.HTTPRequestMethodPatch,
	http.MethodPost:    semconvNew.HTTPRequestMethodPost,
	http.MethodPut:     semconvNew.HTTPRequestMethodPut,
	http.MethodTrace:   semconvNew.HTTPRequestMethodTrace,
}

func handleErr(err error) {
	if err != nil {
		otel.Handle(err)
	}
}
