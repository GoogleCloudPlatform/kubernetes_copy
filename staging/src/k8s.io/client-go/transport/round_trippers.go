/*
Copyright 2015 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package transport

import (
	"crypto/tls"
	"fmt"
	"net/http"
	"net/http/httptrace"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"golang.org/x/oauth2"

	utilnet "k8s.io/apimachinery/pkg/util/net"
	"k8s.io/klog/v2"
)

// HTTPWrappersForConfig wraps a round tripper with any relevant layered
// behavior from the config. Exposed to allow more clients that need HTTP-like
// behavior but then must hijack the underlying connection (like WebSocket or
// HTTP2 clients). Pure HTTP clients should use the RoundTripper returned from
// New.
func HTTPWrappersForConfig(config *Config, rt http.RoundTripper) (http.RoundTripper, error) {
	if config.WrapTransport != nil {
		rt = config.WrapTransport(rt)
	}

	rt = DebugWrappers(rt)

	// Set authentication wrappers
	switch {
	case config.HasBasicAuth() && config.HasTokenAuth():
		return nil, fmt.Errorf("username/password or bearer token may be set, but not both")
	case config.HasTokenAuth():
		var err error
		rt, err = NewBearerAuthWithRefreshRoundTripper(config.BearerToken, config.BearerTokenFile, rt)
		if err != nil {
			return nil, err
		}
	case config.HasBasicAuth():
		rt = NewBasicAuthRoundTripper(config.Username, config.Password, rt)
	}
	if len(config.UserAgent) > 0 {
		rt = NewUserAgentRoundTripper(config.UserAgent, rt)
	}
	if len(config.Impersonate.UserName) > 0 ||
		len(config.Impersonate.UID) > 0 ||
		len(config.Impersonate.Groups) > 0 ||
		len(config.Impersonate.Extra) > 0 {
		rt = NewImpersonatingRoundTripper(config.Impersonate, rt)
	}
	return rt, nil
}

// DebugWrappers always wraps a round tripper
// and then logs based on the log level in the context of each individual
// request.
func DebugWrappers(rt http.RoundTripper) http.RoundTripper {
	return NewDebuggingRoundTripper(rt, DebugByContext)
}

type authProxyRoundTripper struct {
	username string
	uid      string
	groups   []string
	extra    map[string][]string

	rt http.RoundTripper
}

var _ utilnet.RoundTripperWrapper = &authProxyRoundTripper{}

// NewAuthProxyRoundTripper provides a roundtripper which will add auth proxy fields to requests for
// authentication terminating proxy cases
// assuming you pull the user from the context:
// username is the user.Info.GetName() of the user
// uid is the user.Info.GetUID() of the user
// groups is the user.Info.GetGroups() of the user
// extra is the user.Info.GetExtra() of the user
// extra can contain any additional information that the authenticator
// thought was interesting, for example authorization scopes.
// In order to faithfully round-trip through an impersonation flow, these keys
// MUST be lowercase.
func NewAuthProxyRoundTripper(username, uid string, groups []string, extra map[string][]string, rt http.RoundTripper) http.RoundTripper {
	return &authProxyRoundTripper{
		username: username,
		uid:      uid,
		groups:   groups,
		extra:    extra,
		rt:       rt,
	}
}

func (rt *authProxyRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	req = utilnet.CloneRequest(req)
	SetAuthProxyHeaders(req, rt.username, rt.uid, rt.groups, rt.extra)

	return rt.rt.RoundTrip(req)
}

// SetAuthProxyHeaders stomps the auth proxy header fields.  It mutates its argument.
func SetAuthProxyHeaders(req *http.Request, username, uid string, groups []string, extra map[string][]string) {
	req.Header.Del("X-Remote-User")
	req.Header.Del("X-Remote-Uid")
	req.Header.Del("X-Remote-Group")
	for key := range req.Header {
		if strings.HasPrefix(strings.ToLower(key), strings.ToLower("X-Remote-Extra-")) {
			req.Header.Del(key)
		}
	}

	req.Header.Set("X-Remote-User", username)
	if len(uid) > 0 {
		req.Header.Set("X-Remote-Uid", uid)
	}
	for _, group := range groups {
		req.Header.Add("X-Remote-Group", group)
	}
	for key, values := range extra {
		for _, value := range values {
			req.Header.Add("X-Remote-Extra-"+headerKeyEscape(key), value)
		}
	}
}

func (rt *authProxyRoundTripper) CancelRequest(req *http.Request) {
	tryCancelRequest(rt.WrappedRoundTripper(), req)
}

func (rt *authProxyRoundTripper) WrappedRoundTripper() http.RoundTripper { return rt.rt }

type userAgentRoundTripper struct {
	agent string
	rt    http.RoundTripper
}

var _ utilnet.RoundTripperWrapper = &userAgentRoundTripper{}

// NewUserAgentRoundTripper will add User-Agent header to a request unless it has already been set.
func NewUserAgentRoundTripper(agent string, rt http.RoundTripper) http.RoundTripper {
	return &userAgentRoundTripper{agent, rt}
}

func (rt *userAgentRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if len(req.Header.Get("User-Agent")) != 0 {
		return rt.rt.RoundTrip(req)
	}
	req = utilnet.CloneRequest(req)
	req.Header.Set("User-Agent", rt.agent)
	return rt.rt.RoundTrip(req)
}

func (rt *userAgentRoundTripper) CancelRequest(req *http.Request) {
	tryCancelRequest(rt.WrappedRoundTripper(), req)
}

func (rt *userAgentRoundTripper) WrappedRoundTripper() http.RoundTripper { return rt.rt }

type basicAuthRoundTripper struct {
	username string
	password string `datapolicy:"password"`
	rt       http.RoundTripper
}

var _ utilnet.RoundTripperWrapper = &basicAuthRoundTripper{}

// NewBasicAuthRoundTripper will apply a BASIC auth authorization header to a
// request unless it has already been set.
func NewBasicAuthRoundTripper(username, password string, rt http.RoundTripper) http.RoundTripper {
	return &basicAuthRoundTripper{username, password, rt}
}

func (rt *basicAuthRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if len(req.Header.Get("Authorization")) != 0 {
		return rt.rt.RoundTrip(req)
	}
	req = utilnet.CloneRequest(req)
	req.SetBasicAuth(rt.username, rt.password)
	return rt.rt.RoundTrip(req)
}

func (rt *basicAuthRoundTripper) CancelRequest(req *http.Request) {
	tryCancelRequest(rt.WrappedRoundTripper(), req)
}

func (rt *basicAuthRoundTripper) WrappedRoundTripper() http.RoundTripper { return rt.rt }

// These correspond to the headers used in pkg/apis/authentication.  We don't want the package dependency,
// but you must not change the values.
const (
	// ImpersonateUserHeader is used to impersonate a particular user during an API server request
	ImpersonateUserHeader = "Impersonate-User"

	// ImpersonateUIDHeader is used to impersonate a particular UID during an API server request
	ImpersonateUIDHeader = "Impersonate-Uid"

	// ImpersonateGroupHeader is used to impersonate a particular group during an API server request.
	// It can be repeated multiplied times for multiple groups.
	ImpersonateGroupHeader = "Impersonate-Group"

	// ImpersonateUserExtraHeaderPrefix is a prefix for a header used to impersonate an entry in the
	// extra map[string][]string for user.Info.  The key for the `extra` map is suffix.
	// The same key can be repeated multiple times to have multiple elements in the slice under a single key.
	// For instance:
	// Impersonate-Extra-Foo: one
	// Impersonate-Extra-Foo: two
	// results in extra["Foo"] = []string{"one", "two"}
	ImpersonateUserExtraHeaderPrefix = "Impersonate-Extra-"
)

type impersonatingRoundTripper struct {
	impersonate ImpersonationConfig
	delegate    http.RoundTripper
}

var _ utilnet.RoundTripperWrapper = &impersonatingRoundTripper{}

// NewImpersonatingRoundTripper will add an Act-As header to a request unless it has already been set.
func NewImpersonatingRoundTripper(impersonate ImpersonationConfig, delegate http.RoundTripper) http.RoundTripper {
	return &impersonatingRoundTripper{impersonate, delegate}
}

func (rt *impersonatingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	// use the user header as marker for the rest.
	if len(req.Header.Get(ImpersonateUserHeader)) != 0 {
		return rt.delegate.RoundTrip(req)
	}
	req = utilnet.CloneRequest(req)
	req.Header.Set(ImpersonateUserHeader, rt.impersonate.UserName)
	if rt.impersonate.UID != "" {
		req.Header.Set(ImpersonateUIDHeader, rt.impersonate.UID)
	}
	for _, group := range rt.impersonate.Groups {
		req.Header.Add(ImpersonateGroupHeader, group)
	}
	for k, vv := range rt.impersonate.Extra {
		for _, v := range vv {
			req.Header.Add(ImpersonateUserExtraHeaderPrefix+headerKeyEscape(k), v)
		}
	}

	return rt.delegate.RoundTrip(req)
}

func (rt *impersonatingRoundTripper) CancelRequest(req *http.Request) {
	tryCancelRequest(rt.WrappedRoundTripper(), req)
}

func (rt *impersonatingRoundTripper) WrappedRoundTripper() http.RoundTripper { return rt.delegate }

type bearerAuthRoundTripper struct {
	bearer string
	source oauth2.TokenSource
	rt     http.RoundTripper
}

var _ utilnet.RoundTripperWrapper = &bearerAuthRoundTripper{}

// NewBearerAuthRoundTripper adds the provided bearer token to a request
// unless the authorization header has already been set.
func NewBearerAuthRoundTripper(bearer string, rt http.RoundTripper) http.RoundTripper {
	return &bearerAuthRoundTripper{bearer, nil, rt}
}

// NewBearerAuthWithRefreshRoundTripper adds the provided bearer token to a request
// unless the authorization header has already been set.
// If tokenFile is non-empty, it is periodically read,
// and the last successfully read content is used as the bearer token.
// If tokenFile is non-empty and bearer is empty, the tokenFile is read
// immediately to populate the initial bearer token.
func NewBearerAuthWithRefreshRoundTripper(bearer string, tokenFile string, rt http.RoundTripper) (http.RoundTripper, error) {
	if len(tokenFile) == 0 {
		return &bearerAuthRoundTripper{bearer, nil, rt}, nil
	}
	source := NewCachedFileTokenSource(tokenFile)
	if len(bearer) == 0 {
		token, err := source.Token()
		if err != nil {
			return nil, err
		}
		bearer = token.AccessToken
	}
	return &bearerAuthRoundTripper{bearer, source, rt}, nil
}

func (rt *bearerAuthRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if len(req.Header.Get("Authorization")) != 0 {
		return rt.rt.RoundTrip(req)
	}

	req = utilnet.CloneRequest(req)
	token := rt.bearer
	if rt.source != nil {
		if refreshedToken, err := rt.source.Token(); err == nil {
			token = refreshedToken.AccessToken
		}
	}
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", token))
	return rt.rt.RoundTrip(req)
}

func (rt *bearerAuthRoundTripper) CancelRequest(req *http.Request) {
	tryCancelRequest(rt.WrappedRoundTripper(), req)
}

func (rt *bearerAuthRoundTripper) WrappedRoundTripper() http.RoundTripper { return rt.rt }

// requestInfo keeps track of information about a request/response combination
type requestInfo struct {
	RequestHeaders http.Header `datapolicy:"token"`
	RequestVerb    string
	RequestURL     string

	ResponseStatus  string
	ResponseHeaders http.Header
	ResponseErr     error

	muTrace          sync.Mutex // Protect trace fields
	DNSLookup        time.Duration
	Dialing          time.Duration
	GetConnection    time.Duration
	TLSHandshake     time.Duration
	ServerProcessing time.Duration
	ConnectionReused bool

	Duration time.Duration
}

// newRequestInfo creates a new RequestInfo based on an http request
func newRequestInfo(req *http.Request) *requestInfo {
	return &requestInfo{
		RequestURL:     req.URL.String(),
		RequestVerb:    req.Method,
		RequestHeaders: req.Header,
	}
}

// complete adds information about the response to the requestInfo
func (r *requestInfo) complete(response *http.Response, err error) {
	if err != nil {
		r.ResponseErr = err
		return
	}
	r.ResponseStatus = response.Status
	r.ResponseHeaders = response.Header
}

// toCurl returns a string that can be run as a command in a terminal (minus the body)
func (r *requestInfo) toCurl() string {
	headers := ""
	for key, values := range r.RequestHeaders {
		for _, value := range values {
			value = maskValue(key, value)
			headers += fmt.Sprintf(` -H %q`, fmt.Sprintf("%s: %s", key, value))
		}
	}

	// Newline at the end makes this look better in the text log output (the
	// only usage of this method) because it becomes a multi-line string with
	// no quoting.
	return fmt.Sprintf("curl -v -X%s %s '%s'\n", r.RequestVerb, headers, r.RequestURL)
}

// debuggingRoundTripper will display information about the requests passing
// through it based on what is configured
type debuggingRoundTripper struct {
	delegatedRoundTripper http.RoundTripper
	levels                int
}

var _ utilnet.RoundTripperWrapper = &debuggingRoundTripper{}

// DebugLevel is used to enable debugging of certain
// HTTP requests and responses fields via the debuggingRoundTripper.
type DebugLevel int

const (
	// DebugJustURL will add to the debug output HTTP requests method and url.
	DebugJustURL DebugLevel = iota
	// DebugURLTiming will add to the debug output the duration of HTTP requests.
	DebugURLTiming
	// DebugCurlCommand will add to the debug output the curl command equivalent to the
	// HTTP request.
	DebugCurlCommand
	// DebugRequestHeaders will add to the debug output the HTTP requests headers.
	DebugRequestHeaders
	// DebugResponseStatus will add to the debug output the HTTP response status.
	DebugResponseStatus
	// DebugResponseHeaders will add to the debug output the HTTP response headers.
	DebugResponseHeaders
	// DebugDetailedTiming will add to the debug output the duration of the HTTP requests events.
	DebugDetailedTiming
	// DebugByContext will add any of the above depending on the verbosity of the per-request logger obtained from the requests context.
	//
	// Can be combined in NewDebuggingRoundTripper with some of the other options, in which case the
	// debug roundtripper will always log what is requested there plus the information that gets
	// enabled by the context's log verbosity.
	DebugByContext
)

// Different log levels include different sets of information.
//
// Not exported because the exact content of log messages is not part
// of of the package API.
const (
	levelsV6 = (1 << DebugURLTiming)
	// Logging *less* information for the response at level 7 compared to 6 replicates prior behavior:
	//  https://github.com/kubernetes/kubernetes/blob/2b472fe4690c83a2b343995f88050b2a3e9ff0fa/staging/src/k8s.io/client-go/transport/round_trippers.go#L79
	// Presumably that was done because verb and URL are already in the request log entry.
	levelsV7 = (1 << DebugJustURL) | (1 << DebugRequestHeaders) | (1 << DebugResponseStatus)
	levelsV8 = (1 << DebugJustURL) | (1 << DebugRequestHeaders) | (1 << DebugResponseStatus) | (1 << DebugResponseHeaders)
	levelsV9 = (1 << DebugCurlCommand) | (1 << DebugURLTiming) | (1 << DebugDetailedTiming) | (1 << DebugResponseHeaders)
)

// NewDebuggingRoundTripper allows to display in the logs output debug information
// on the API requests performed by the client.
func NewDebuggingRoundTripper(rt http.RoundTripper, levels ...DebugLevel) http.RoundTripper {
	drt := &debuggingRoundTripper{
		delegatedRoundTripper: rt,
	}
	for _, v := range levels {
		drt.levels |= 1 << v
	}
	return drt
}

func (rt *debuggingRoundTripper) CancelRequest(req *http.Request) {
	tryCancelRequest(rt.WrappedRoundTripper(), req)
}

var knownAuthTypes = map[string]bool{
	"bearer":    true,
	"basic":     true,
	"negotiate": true,
}

// maskValue masks credential content from authorization headers
// See https://developer.mozilla.org/en-US/docs/Web/HTTP/Headers/Authorization
func maskValue(key string, value string) string {
	if !strings.EqualFold(key, "Authorization") {
		return value
	}
	if len(value) == 0 {
		return ""
	}
	var authType string
	if i := strings.Index(value, " "); i > 0 {
		authType = value[0:i]
	} else {
		authType = value
	}
	if !knownAuthTypes[strings.ToLower(authType)] {
		return "<masked>"
	}
	if len(value) > len(authType)+1 {
		value = authType + " <masked>"
	} else {
		value = authType
	}
	return value
}

func (rt *debuggingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	logger := klog.FromContext(req.Context())
	levels := rt.levels

	// When logging depends on the context, it uses the verbosity of the per-context logger
	// and a hard-coded mapping of verbosity to debug details. Otherwise all messages
	// are logged as V(0).
	if levels&(1<<DebugByContext) != 0 {
		loggerV6 := logger.V(6)
		loggerV6Enabled := loggerV6.Enabled()
		if levels == (1<<DebugByContext) && !loggerV6Enabled {
			// Fast path: no extra logging at level < 6.
			return rt.delegatedRoundTripper.RoundTrip(req)
		}

		if loggerV := logger.V(9); loggerV.Enabled() {
			logger = loggerV
			// The curl command replaces logging of the URL.
			levels |= levelsV9
		} else if loggerV := logger.V(8); loggerV.Enabled() {
			logger = loggerV
			levels |= levelsV8
		} else if loggerV := logger.V(7); loggerV.Enabled() {
			logger = loggerV
			levels |= levelsV7
		} else if loggerV6Enabled {
			logger = loggerV6
			levels |= levelsV6
		}
	}

	reqInfo := newRequestInfo(req)

	kvs := make([]any, 0, 8) // Exactly large enough for all appends below.
	if levels&(1<<DebugJustURL) != 0 {
		kvs = append(kvs,
			"verb", reqInfo.RequestVerb,
			"url", reqInfo.RequestURL,
		)
	}
	if levels&(1<<DebugCurlCommand) != 0 {
		kvs = append(kvs, "curlCommand", reqInfo.toCurl())
	}
	if levels&(1<<DebugRequestHeaders) != 0 {
		kvs = append(kvs, "headers", newHeadersMap(reqInfo.RequestHeaders))
	}
	if len(kvs) > 0 {
		logger.Info("Request", kvs...)
	}

	startTime := time.Now()

	if levels&(1<<DebugDetailedTiming) != 0 {
		var getConn, dnsStart, dialStart, tlsStart, serverStart time.Time
		var host string
		trace := &httptrace.ClientTrace{
			// DNS
			DNSStart: func(info httptrace.DNSStartInfo) {
				reqInfo.muTrace.Lock()
				defer reqInfo.muTrace.Unlock()
				dnsStart = time.Now()
				host = info.Host
			},
			DNSDone: func(info httptrace.DNSDoneInfo) {
				reqInfo.muTrace.Lock()
				defer reqInfo.muTrace.Unlock()
				reqInfo.DNSLookup = time.Since(dnsStart)
				logger.Info("HTTP Trace: DNS Lookup resolved", "host", host, "address", info.Addrs)
			},
			// Dial
			ConnectStart: func(network, addr string) {
				reqInfo.muTrace.Lock()
				defer reqInfo.muTrace.Unlock()
				dialStart = time.Now()
			},
			ConnectDone: func(network, addr string, err error) {
				reqInfo.muTrace.Lock()
				defer reqInfo.muTrace.Unlock()
				reqInfo.Dialing = time.Since(dialStart)
				if err != nil {
					logger.Info("HTTP Trace: Dial failed", "network", network, "address", addr, "err", err)
				} else {
					logger.Info("HTTP Trace: Dial succeed", "network", network, "address", addr)
				}
			},
			// TLS
			TLSHandshakeStart: func() {
				tlsStart = time.Now()
			},
			TLSHandshakeDone: func(_ tls.ConnectionState, _ error) {
				reqInfo.muTrace.Lock()
				defer reqInfo.muTrace.Unlock()
				reqInfo.TLSHandshake = time.Since(tlsStart)
			},
			// Connection (it can be DNS + Dial or just the time to get one from the connection pool)
			GetConn: func(hostPort string) {
				getConn = time.Now()
			},
			GotConn: func(info httptrace.GotConnInfo) {
				reqInfo.muTrace.Lock()
				defer reqInfo.muTrace.Unlock()
				reqInfo.GetConnection = time.Since(getConn)
				reqInfo.ConnectionReused = info.Reused
			},
			// Server Processing (time since we wrote the request until first byte is received)
			WroteRequest: func(info httptrace.WroteRequestInfo) {
				reqInfo.muTrace.Lock()
				defer reqInfo.muTrace.Unlock()
				serverStart = time.Now()
			},
			GotFirstResponseByte: func() {
				reqInfo.muTrace.Lock()
				defer reqInfo.muTrace.Unlock()
				reqInfo.ServerProcessing = time.Since(serverStart)
			},
		}
		req = req.WithContext(httptrace.WithClientTrace(req.Context(), trace))
	}

	response, err := rt.delegatedRoundTripper.RoundTrip(req)
	reqInfo.Duration = time.Since(startTime)

	reqInfo.complete(response, err)

	kvs = make([]any, 0, 20) // Exactly large enough for all appends below.
	if levels&(1<<DebugURLTiming) != 0 {
		kvs = append(kvs, "verb", reqInfo.RequestVerb, "url", reqInfo.RequestURL)
	}
	if levels&(1<<DebugURLTiming|1<<DebugResponseStatus) != 0 {
		kvs = append(kvs, "status", reqInfo.ResponseStatus)
	}
	if levels&(1<<DebugResponseHeaders) != 0 {
		kvs = append(kvs, "headers", newHeadersMap(reqInfo.ResponseHeaders))
	}
	if levels&(1<<DebugURLTiming|1<<DebugDetailedTiming|1<<DebugResponseStatus) != 0 {
		kvs = append(kvs, "milliseconds", reqInfo.Duration.Nanoseconds()/int64(time.Millisecond))
	}
	if levels&(1<<DebugDetailedTiming) != 0 {
		if !reqInfo.ConnectionReused {
			kvs = append(kvs,
				"dnsLookupMilliseconds", reqInfo.DNSLookup.Nanoseconds()/int64(time.Millisecond),
				"dialMilliseconds", reqInfo.Dialing.Nanoseconds()/int64(time.Millisecond),
				"tlsHandshakeMilliseconds", reqInfo.TLSHandshake.Nanoseconds()/int64(time.Millisecond),
			)
		} else {
			kvs = append(kvs, "getConnectionMilliseconds", reqInfo.GetConnection.Nanoseconds()/int64(time.Millisecond))
		}
		if reqInfo.ServerProcessing != 0 {
			kvs = append(kvs, "serverProcessingMilliseconds", reqInfo.ServerProcessing.Nanoseconds()/int64(time.Millisecond))
		}
	}
	if len(kvs) > 0 {
		logger.Info("Response", kvs...)
	}

	return response, err
}

// headerMap formats headers sorted and across multiple lines with no quoting
// when using string output and as JSON when using zapr.
type headersMap http.Header

// newHeadersMap masks all sensitive values. This has to be done before
// passing the map to a logger because while in practice all loggers
// either use String or MarshalLog, that is not guaranteed.
func newHeadersMap(header http.Header) headersMap {
	h := make(headersMap, len(header))
	for key, values := range header {
		maskedValues := make([]string, 0, len(values))
		for _, value := range values {
			maskedValues = append(maskedValues, maskValue(key, value))
		}
		h[key] = maskedValues
	}
	return h
}

var _ fmt.Stringer = headersMap{}
var _ logr.Marshaler = headersMap{}

func (h headersMap) String() string {
	// The fixed size typically avoids memory allocations when it is large enough.
	keys := make([]string, 0, 20)
	for key := range h {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var buffer strings.Builder
	for _, key := range keys {
		for _, value := range h[key] {
			_, _ = buffer.WriteString(key)
			_, _ = buffer.WriteString(": ")
			_, _ = buffer.WriteString(value)
			_, _ = buffer.WriteString("\n")
		}
	}
	return buffer.String()
}

func (h headersMap) MarshalLog() any {
	return map[string][]string(h)
}

func (rt *debuggingRoundTripper) WrappedRoundTripper() http.RoundTripper {
	return rt.delegatedRoundTripper
}

func legalHeaderByte(b byte) bool {
	return int(b) < len(legalHeaderKeyBytes) && legalHeaderKeyBytes[b]
}

func shouldEscape(b byte) bool {
	// url.PathUnescape() returns an error if any '%' is not followed by two
	// hexadecimal digits, so we'll intentionally encode it.
	return !legalHeaderByte(b) || b == '%'
}

func headerKeyEscape(key string) string {
	buf := strings.Builder{}
	for i := 0; i < len(key); i++ {
		b := key[i]
		if shouldEscape(b) {
			// %-encode bytes that should be escaped:
			// https://tools.ietf.org/html/rfc3986#section-2.1
			fmt.Fprintf(&buf, "%%%02X", b)
			continue
		}
		buf.WriteByte(b)
	}
	return buf.String()
}

// legalHeaderKeyBytes was copied from net/http/lex.go's isTokenTable.
// See https://httpwg.github.io/specs/rfc7230.html#rule.token.separators
var legalHeaderKeyBytes = [127]bool{
	'%':  true,
	'!':  true,
	'#':  true,
	'$':  true,
	'&':  true,
	'\'': true,
	'*':  true,
	'+':  true,
	'-':  true,
	'.':  true,
	'0':  true,
	'1':  true,
	'2':  true,
	'3':  true,
	'4':  true,
	'5':  true,
	'6':  true,
	'7':  true,
	'8':  true,
	'9':  true,
	'A':  true,
	'B':  true,
	'C':  true,
	'D':  true,
	'E':  true,
	'F':  true,
	'G':  true,
	'H':  true,
	'I':  true,
	'J':  true,
	'K':  true,
	'L':  true,
	'M':  true,
	'N':  true,
	'O':  true,
	'P':  true,
	'Q':  true,
	'R':  true,
	'S':  true,
	'T':  true,
	'U':  true,
	'W':  true,
	'V':  true,
	'X':  true,
	'Y':  true,
	'Z':  true,
	'^':  true,
	'_':  true,
	'`':  true,
	'a':  true,
	'b':  true,
	'c':  true,
	'd':  true,
	'e':  true,
	'f':  true,
	'g':  true,
	'h':  true,
	'i':  true,
	'j':  true,
	'k':  true,
	'l':  true,
	'm':  true,
	'n':  true,
	'o':  true,
	'p':  true,
	'q':  true,
	'r':  true,
	's':  true,
	't':  true,
	'u':  true,
	'v':  true,
	'w':  true,
	'x':  true,
	'y':  true,
	'z':  true,
	'|':  true,
	'~':  true,
}
