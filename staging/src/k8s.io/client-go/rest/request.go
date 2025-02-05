/*
Copyright 2014 The Kubernetes Authors.

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

package rest

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"os"
	"path"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/http2"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/conversion"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer/streaming"
	"k8s.io/apimachinery/pkg/util/net"
	"k8s.io/apimachinery/pkg/watch"
	clientfeatures "k8s.io/client-go/features"
	restclientwatch "k8s.io/client-go/rest/watch"
	"k8s.io/client-go/tools/metrics"
	"k8s.io/client-go/util/flowcontrol"
	"k8s.io/klog/v2"
	"k8s.io/utils/clock"
)

const (
	// longThrottleLatency defines threshold for logging requests. All requests being
	// throttled (via the provided rateLimiter) for more than longThrottleLatency will
	// be logged.
	longThrottleLatency = 50 * time.Millisecond

	// extraLongThrottleLatency defines the threshold for logging requests at log level 2.
	extraLongThrottleLatency = 1 * time.Second
)

// HTTPClient is an interface for testing a request object.
type HTTPClient interface {
	Do(req *http.Request) (*http.Response, error)
}

// ResponseWrapper is an interface for getting a response.
// The response may be either accessed as a raw data (the whole output is put into memory) or as a stream.
type ResponseWrapper interface {
	DoRaw(context.Context) ([]byte, error)
	Stream(context.Context) (io.ReadCloser, error)
}

// RequestConstructionError is returned when there's an error assembling a request.
type RequestConstructionError struct {
	Err error
}

// Error returns a textual description of 'r'.
func (r *RequestConstructionError) Error() string {
	return fmt.Sprintf("request construction error: '%v'", r.Err)
}

var noBackoff = &NoBackoff{}

type requestRetryFunc func(maxRetries int) WithRetry

func defaultRequestRetryFn(maxRetries int) WithRetry {
	return &withRetry{maxRetries: maxRetries}
}

// Request allows for building up a request to a server in a chained fashion.
// Any errors are stored until the end of your call, so you only have to
// check once.
type Request struct {
	c *RESTClient

	contentConfig     ClientContentConfig
	contentTypeNotSet bool

	warningHandler WarningHandlerWithContext

	rateLimiter flowcontrol.RateLimiter
	backoff     BackoffManagerWithContext
	timeout     time.Duration
	maxRetries  int

	// generic components accessible via method setters
	verb       string
	pathPrefix string
	subpath    string
	params     url.Values
	headers    http.Header

	// structural elements of the request that are part of the Kubernetes API conventions
	namespace    string
	namespaceSet bool
	resource     string
	resourceName string
	subresource  string

	// output
	err error

	// only one of body / bodyBytes may be set. requests using body are not retryable.
	body      io.Reader
	bodyBytes []byte

	retryFn requestRetryFunc
}

// NewRequest creates a new request helper object for accessing runtime.Objects on a server.
func NewRequest(c *RESTClient) *Request {
	var backoff BackoffManagerWithContext
	if c.createBackoffMgr != nil {
		backoff = c.createBackoffMgr()
	}
	if backoff == nil {
		backoff = noBackoff
	}

	var pathPrefix string
	if c.base != nil {
		pathPrefix = path.Join("/", c.base.Path, c.versionedAPIPath)
	} else {
		pathPrefix = path.Join("/", c.versionedAPIPath)
	}

	var timeout time.Duration
	if c.Client != nil {
		timeout = c.Client.Timeout
	}

	// A request needs to know whether the content type was explicitly configured or selected by
	// default in order to support the per-request Protobuf override used by clients generated
	// with --prefers-protobuf.
	contentConfig, contentTypeDefaulted := c.content.GetClientContentConfig()

	r := &Request{
		c:              c,
		rateLimiter:    c.rateLimiter,
		backoff:        backoff,
		timeout:        timeout,
		pathPrefix:     pathPrefix,
		maxRetries:     10,
		retryFn:        defaultRequestRetryFn,
		warningHandler: c.warningHandler,

		contentConfig:     contentConfig,
		contentTypeNotSet: contentTypeDefaulted,
	}

	r.setAcceptHeader()
	return r
}

// NewRequestWithClient creates a Request with an embedded RESTClient for use in test scenarios.
func NewRequestWithClient(base *url.URL, versionedAPIPath string, content ClientContentConfig, client *http.Client) *Request {
	return NewRequest(&RESTClient{
		base:             base,
		versionedAPIPath: versionedAPIPath,
		content:          requestClientContentConfigProvider{base: content},
		Client:           client,
	})
}

func (r *Request) UseProtobufAsDefaultIfPreferred(prefersProtobuf bool) *Request {
	if prefersProtobuf {
		return r.UseProtobufAsDefault()
	}
	return r
}

func (r *Request) UseProtobufAsDefault() *Request {
	if r.contentTypeNotSet && len(r.contentConfig.AcceptContentTypes) == 0 {
		r.contentConfig.AcceptContentTypes = "application/vnd.kubernetes.protobuf,application/json"
		r.contentConfig.ContentType = "application/vnd.kubernetes.protobuf"
		r.setAcceptHeader()
	}
	return r
}

func (r *Request) setAcceptHeader() {
	switch {
	case len(r.contentConfig.AcceptContentTypes) > 0:
		r.SetHeader("Accept", r.contentConfig.AcceptContentTypes)
	case len(r.contentConfig.ContentType) > 0:
		r.SetHeader("Accept", r.contentConfig.ContentType+", */*")
	}
}

// Verb sets the verb this request will use.
func (r *Request) Verb(verb string) *Request {
	r.verb = verb
	return r
}

// Prefix adds segments to the relative beginning to the request path. These
// items will be placed before the optional Namespace, Resource, or Name sections.
// Setting AbsPath will clear any previously set Prefix segments
func (r *Request) Prefix(segments ...string) *Request {
	if r.err != nil {
		return r
	}
	r.pathPrefix = path.Join(r.pathPrefix, path.Join(segments...))
	return r
}

// Suffix appends segments to the end of the path. These items will be placed after the prefix and optional
// Namespace, Resource, or Name sections.
func (r *Request) Suffix(segments ...string) *Request {
	if r.err != nil {
		return r
	}
	r.subpath = path.Join(r.subpath, path.Join(segments...))
	return r
}

// Resource sets the resource to access (<resource>/[ns/<namespace>/]<name>)
func (r *Request) Resource(resource string) *Request {
	if r.err != nil {
		return r
	}
	if len(r.resource) != 0 {
		r.err = fmt.Errorf("resource already set to %q, cannot change to %q", r.resource, resource)
		return r
	}
	if msgs := IsValidPathSegmentName(resource); len(msgs) != 0 {
		r.err = fmt.Errorf("invalid resource %q: %v", resource, msgs)
		return r
	}
	r.resource = resource
	return r
}

// BackOff sets the request's backoff manager to the one specified,
// or defaults to the stub implementation if nil is provided.
//
// Deprecated: BackoffManager.Sleep ignores the caller's context. Use BackOffWithContext and BackoffManagerWithContext instead.
func (r *Request) BackOff(manager BackoffManager) *Request {
	if manager == nil {
		r.backoff = &NoBackoff{}
		return r
	}

	r.backoff = &backoffManagerNopContext{BackoffManager: manager}
	return r
}

// BackOffWithContext sets the request's backoff manager to the one specified,
// or defaults to the stub implementation if nil is provided.
func (r *Request) BackOffWithContext(manager BackoffManagerWithContext) *Request {
	if manager == nil {
		r.backoff = &NoBackoff{}
		return r
	}

	r.backoff = manager
	return r
}

// WarningHandler sets the handler this client uses when warning headers are encountered.
// If set to nil, this client will use the default warning handler (see [SetDefaultWarningHandler]).
//
//logcheck:context // WarningHandlerWithContext should be used instead of WarningHandler in code which supports contextual logging.
func (r *Request) WarningHandler(handler WarningHandler) *Request {
	if handler == nil {
		r.warningHandler = nil
		return r
	}
	r.warningHandler = warningLoggerNopContext{l: handler}
	return r
}

// WarningHandlerWithContext sets the handler this client uses when warning headers are encountered.
// If set to nil, this client will use the default warning handler (see [SetDefaultWarningHandlerWithContext]).
func (r *Request) WarningHandlerWithContext(handler WarningHandlerWithContext) *Request {
	r.warningHandler = handler
	return r
}

// Throttle receives a rate-limiter and sets or replaces an existing request limiter
func (r *Request) Throttle(limiter flowcontrol.RateLimiter) *Request {
	r.rateLimiter = limiter
	return r
}

// SubResource sets a sub-resource path which can be multiple segments after the resource
// name but before the suffix.
func (r *Request) SubResource(subresources ...string) *Request {
	if r.err != nil {
		return r
	}
	subresource := path.Join(subresources...)
	if len(r.subresource) != 0 {
		r.err = fmt.Errorf("subresource already set to %q, cannot change to %q", r.subresource, subresource)
		return r
	}
	for _, s := range subresources {
		if msgs := IsValidPathSegmentName(s); len(msgs) != 0 {
			r.err = fmt.Errorf("invalid subresource %q: %v", s, msgs)
			return r
		}
	}
	r.subresource = subresource
	return r
}

// Name sets the name of a resource to access (<resource>/[ns/<namespace>/]<name>)
func (r *Request) Name(resourceName string) *Request {
	if r.err != nil {
		return r
	}
	if len(resourceName) == 0 {
		r.err = fmt.Errorf("resource name may not be empty")
		return r
	}
	if len(r.resourceName) != 0 {
		r.err = fmt.Errorf("resource name already set to %q, cannot change to %q", r.resourceName, resourceName)
		return r
	}
	if msgs := IsValidPathSegmentName(resourceName); len(msgs) != 0 {
		r.err = fmt.Errorf("invalid resource name %q: %v", resourceName, msgs)
		return r
	}
	r.resourceName = resourceName
	return r
}

// Namespace applies the namespace scope to a request (<resource>/[ns/<namespace>/]<name>)
func (r *Request) Namespace(namespace string) *Request {
	if r.err != nil {
		return r
	}
	if r.namespaceSet {
		r.err = fmt.Errorf("namespace already set to %q, cannot change to %q", r.namespace, namespace)
		return r
	}
	if msgs := IsValidPathSegmentName(namespace); len(msgs) != 0 {
		r.err = fmt.Errorf("invalid namespace %q: %v", namespace, msgs)
		return r
	}
	r.namespaceSet = true
	r.namespace = namespace
	return r
}

// NamespaceIfScoped is a convenience function to set a namespace if scoped is true
func (r *Request) NamespaceIfScoped(namespace string, scoped bool) *Request {
	if scoped {
		return r.Namespace(namespace)
	}
	return r
}

// AbsPath overwrites an existing path with the segments provided. Trailing slashes are preserved
// when a single segment is passed.
func (r *Request) AbsPath(segments ...string) *Request {
	if r.err != nil {
		return r
	}
	r.pathPrefix = path.Join(r.c.base.Path, path.Join(segments...))
	if len(segments) == 1 && (len(r.c.base.Path) > 1 || len(segments[0]) > 1) && strings.HasSuffix(segments[0], "/") {
		// preserve any trailing slashes for legacy behavior
		r.pathPrefix += "/"
	}
	return r
}

// RequestURI overwrites existing path and parameters with the value of the provided server relative
// URI.
func (r *Request) RequestURI(uri string) *Request {
	if r.err != nil {
		return r
	}
	locator, err := url.Parse(uri)
	if err != nil {
		r.err = err
		return r
	}
	r.pathPrefix = locator.Path
	if len(locator.Query()) > 0 {
		if r.params == nil {
			r.params = make(url.Values)
		}
		for k, v := range locator.Query() {
			r.params[k] = v
		}
	}
	return r
}

// Param creates a query parameter with the given string value.
func (r *Request) Param(paramName, s string) *Request {
	if r.err != nil {
		return r
	}
	return r.setParam(paramName, s)
}

// VersionedParams will take the provided object, serialize it to a map[string][]string using the
// implicit RESTClient API version and the default parameter codec, and then add those as parameters
// to the request. Use this to provide versioned query parameters from client libraries.
// VersionedParams will not write query parameters that have omitempty set and are empty. If a
// parameter has already been set it is appended to (Params and VersionedParams are additive).
func (r *Request) VersionedParams(obj runtime.Object, codec runtime.ParameterCodec) *Request {
	return r.SpecificallyVersionedParams(obj, codec, r.contentConfig.GroupVersion)
}

func (r *Request) SpecificallyVersionedParams(obj runtime.Object, codec runtime.ParameterCodec, version schema.GroupVersion) *Request {
	if r.err != nil {
		return r
	}
	params, err := codec.EncodeParameters(obj, version)
	if err != nil {
		r.err = err
		return r
	}
	for k, v := range params {
		if r.params == nil {
			r.params = make(url.Values)
		}
		r.params[k] = append(r.params[k], v...)
	}
	return r
}

func (r *Request) setParam(paramName, value string) *Request {
	if r.params == nil {
		r.params = make(url.Values)
	}
	r.params[paramName] = append(r.params[paramName], value)
	return r
}

func (r *Request) SetHeader(key string, values ...string) *Request {
	if r.headers == nil {
		r.headers = http.Header{}
	}
	r.headers.Del(key)
	for _, value := range values {
		r.headers.Add(key, value)
	}
	return r
}

// Timeout makes the request use the given duration as an overall timeout for the
// request. Additionally, if set passes the value as "timeout" parameter in URL.
func (r *Request) Timeout(d time.Duration) *Request {
	if r.err != nil {
		return r
	}
	r.timeout = d
	return r
}

// MaxRetries makes the request use the given integer as a ceiling of retrying upon receiving
// "Retry-After" headers and 429 status-code in the response. The default is 10 unless this
// function is specifically called with a different value.
// A zero maxRetries prevent it from doing retires and return an error immediately.
func (r *Request) MaxRetries(maxRetries int) *Request {
	if maxRetries < 0 {
		maxRetries = 0
	}
	r.maxRetries = maxRetries
	return r
}

// Body makes the request use obj as the body. Optional.
// If obj is a string, try to read a file of that name.
// If obj is a []byte, send it directly.
// If obj is an io.Reader, use it directly.
// If obj is a runtime.Object, marshal it correctly, and set Content-Type header.
// If obj is a runtime.Object and nil, do nothing.
// Otherwise, set an error.
func (r *Request) Body(obj interface{}) *Request {
	if r.err != nil {
		return r
	}
	switch t := obj.(type) {
	case string:
		data, err := os.ReadFile(t)
		if err != nil {
			r.err = err
			return r
		}
		r.body = nil
		r.bodyBytes = data
	case []byte:
		r.body = nil
		r.bodyBytes = t
	case io.Reader:
		r.body = t
		r.bodyBytes = nil
	case runtime.Object:
		// callers may pass typed interface pointers, therefore we must check nil with reflection
		if reflect.ValueOf(t).IsNil() {
			return r
		}
		encoder, err := r.contentConfig.Negotiator.Encoder(r.contentConfig.ContentType, nil)
		if err != nil {
			r.err = err
			return r
		}
		data, err := runtime.Encode(encoder, t)
		if err != nil {
			r.err = err
			return r
		}
		r.body = nil
		r.bodyBytes = data
		r.SetHeader("Content-Type", r.contentConfig.ContentType)
	default:
		r.err = fmt.Errorf("unknown type used for body: %+v", obj)
	}
	return r
}

// Error returns any error encountered constructing the request, if any.
func (r *Request) Error() error {
	return r.err
}

// URL returns the current working URL. Check the result of Error() to ensure
// that the returned URL is valid.
func (r *Request) URL() *url.URL {
	p := r.pathPrefix
	if r.namespaceSet && len(r.namespace) > 0 {
		p = path.Join(p, "namespaces", r.namespace)
	}
	if len(r.resource) != 0 {
		p = path.Join(p, strings.ToLower(r.resource))
	}
	// Join trims trailing slashes, so preserve r.pathPrefix's trailing slash for backwards compatibility if nothing was changed
	if len(r.resourceName) != 0 || len(r.subpath) != 0 || len(r.subresource) != 0 {
		p = path.Join(p, r.resourceName, r.subresource, r.subpath)
	}

	finalURL := &url.URL{}
	if r.c.base != nil {
		*finalURL = *r.c.base
	}
	finalURL.Path = p

	query := url.Values{}
	for key, values := range r.params {
		for _, value := range values {
			query.Add(key, value)
		}
	}

	// timeout is handled specially here.
	if r.timeout != 0 {
		query.Set("timeout", r.timeout.String())
	}
	finalURL.RawQuery = query.Encode()
	return finalURL
}

// finalURLTemplate is similar to URL(), but will make all specific parameter values equal
// - instead of name or namespace, "{name}" and "{namespace}" will be used, and all query
// parameters will be reset. This creates a copy of the url so as not to change the
// underlying object.
func (r Request) finalURLTemplate() url.URL {
	newParams := url.Values{}
	v := []string{"{value}"}
	for k := range r.params {
		newParams[k] = v
	}
	r.params = newParams
	u := r.URL()
	if u == nil {
		return url.URL{}
	}

	segments := strings.Split(u.Path, "/")
	groupIndex := 0
	index := 0
	trimmedBasePath := ""
	if r.c.base != nil && strings.Contains(u.Path, r.c.base.Path) {
		p := strings.TrimPrefix(u.Path, r.c.base.Path)
		if !strings.HasPrefix(p, "/") {
			p = "/" + p
		}
		// store the base path that we have trimmed so we can append it
		// before returning the URL
		trimmedBasePath = r.c.base.Path
		segments = strings.Split(p, "/")
		groupIndex = 1
	}
	if len(segments) <= 2 {
		return *u
	}

	const CoreGroupPrefix = "api"
	const NamedGroupPrefix = "apis"
	isCoreGroup := segments[groupIndex] == CoreGroupPrefix
	isNamedGroup := segments[groupIndex] == NamedGroupPrefix
	if isCoreGroup {
		// checking the case of core group with /api/v1/... format
		index = groupIndex + 2
	} else if isNamedGroup {
		// checking the case of named group with /apis/apps/v1/... format
		index = groupIndex + 3
	} else {
		// this should not happen that the only two possibilities are /api... and /apis..., just want to put an
		// outlet here in case more API groups are added in future if ever possible:
		// https://kubernetes.io/docs/concepts/overview/kubernetes-api/#api-groups
		// if a wrong API groups name is encountered, return the {prefix} for url.Path
		u.Path = "/{prefix}"
		u.RawQuery = ""
		return *u
	}
	// switch segLength := len(segments) - index; segLength {
	switch {
	// case len(segments) - index == 1:
	// resource (with no name) do nothing
	case len(segments)-index == 2:
		// /$RESOURCE/$NAME: replace $NAME with {name}
		segments[index+1] = "{name}"
	case len(segments)-index == 3:
		if segments[index+2] == "finalize" || segments[index+2] == "status" {
			// /$RESOURCE/$NAME/$SUBRESOURCE: replace $NAME with {name}
			segments[index+1] = "{name}"
		} else {
			// /namespace/$NAMESPACE/$RESOURCE: replace $NAMESPACE with {namespace}
			segments[index+1] = "{namespace}"
		}
	case len(segments)-index >= 4:
		segments[index+1] = "{namespace}"
		// /namespace/$NAMESPACE/$RESOURCE/$NAME: replace $NAMESPACE with {namespace},  $NAME with {name}
		if segments[index+3] != "finalize" && segments[index+3] != "status" {
			// /$RESOURCE/$NAME/$SUBRESOURCE: replace $NAME with {name}
			segments[index+3] = "{name}"
		}
	}
	u.Path = path.Join(trimmedBasePath, path.Join(segments...))
	return *u
}

func (r *Request) tryThrottleWithInfo(ctx context.Context, retryInfo string) error {
	if r.rateLimiter == nil {
		return nil
	}

	now := time.Now()

	err := r.rateLimiter.Wait(ctx)
	if err != nil {
		err = fmt.Errorf("client rate limiter Wait returned an error: %w", err)
	}
	latency := time.Since(now)

	if latency > longThrottleLatency {
		if retryInfo == "" {
			retryInfo = "client-side throttling, not priority and fairness"
		}
		klog.FromContext(ctx).V(3).Info("Waited before sending request", "delay", latency, "reason", retryInfo, "verb", r.verb, "URL", r.URL())

		if latency > extraLongThrottleLatency {
			// If the rate limiter latency is very high, the log message should be printed at a higher log level,
			// but we use a throttled logger to prevent spamming.
			globalThrottledLogger.info(klog.FromContext(ctx), "Waited before sending request", "delay", latency, "reason", retryInfo, "verb", r.verb, "URL", r.URL())
		}
	}
	metrics.RateLimiterLatency.Observe(ctx, r.verb, r.finalURLTemplate(), latency)

	return err
}

func (r *Request) tryThrottle(ctx context.Context) error {
	return r.tryThrottleWithInfo(ctx, "")
}

type throttleSettings struct {
	logLevel       int
	minLogInterval time.Duration

	lastLogTime time.Time
	lock        sync.RWMutex
}

type throttledLogger struct {
	clock    clock.PassiveClock
	settings []*throttleSettings
}

var globalThrottledLogger = &throttledLogger{
	clock: clock.RealClock{},
	settings: []*throttleSettings{
		{
			logLevel:       2,
			minLogInterval: 1 * time.Second,
		}, {
			logLevel:       0,
			minLogInterval: 10 * time.Second,
		},
	},
}

func (b *throttledLogger) attemptToLog(logger klog.Logger) (int, bool) {
	for _, setting := range b.settings {
		if bool(logger.V(setting.logLevel).Enabled()) {
			// Return early without write locking if possible.
			if func() bool {
				setting.lock.RLock()
				defer setting.lock.RUnlock()
				return b.clock.Since(setting.lastLogTime) >= setting.minLogInterval
			}() {
				setting.lock.Lock()
				defer setting.lock.Unlock()
				if b.clock.Since(setting.lastLogTime) >= setting.minLogInterval {
					setting.lastLogTime = b.clock.Now()
					return setting.logLevel, true
				}
			}
			return -1, false
		}
	}
	return -1, false
}

// Infof will write a log message at each logLevel specified by the receiver's throttleSettings
// as long as it hasn't written a log message more recently than minLogInterval.
func (b *throttledLogger) info(logger klog.Logger, message string, kv ...any) {
	if logLevel, ok := b.attemptToLog(logger); ok {
		logger.V(logLevel).Info(message, kv...)
	}
}

// Watch attempts to begin watching the requested location.
// Returns a watch.Interface, or an error.
func (r *Request) Watch(ctx context.Context) (watch.Interface, error) {
	w, _, e := r.watchInternal(ctx)
	return w, e
}

func (r *Request) watchInternal(ctx context.Context) (watch.Interface, runtime.Decoder, error) {
	if r.body == nil {
		logBody(klog.FromContext(ctx), 2, "Request Body", r.bodyBytes)
	}

	// We specifically don't want to rate limit watches, so we
	// don't use r.rateLimiter here.
	if r.err != nil {
		return nil, nil, r.err
	}

	client := r.c.Client
	if client == nil {
		client = http.DefaultClient
	}

	isErrRetryableFunc := func(request *http.Request, err error) bool {
		// The watch stream mechanism handles many common partial data errors, so closed
		// connections can be retried in many cases.
		if net.IsProbableEOF(err) || net.IsTimeout(err) {
			return true
		}
		return false
	}
	retry := r.retryFn(r.maxRetries)
	url := r.URL().String()
	for {
		if err := retry.Before(ctx, r); err != nil {
			return nil, nil, retry.WrapPreviousError(err)
		}

		req, err := r.newHTTPRequest(ctx)
		if err != nil {
			return nil, nil, err
		}

		resp, err := client.Do(req)
		retry.After(ctx, r, resp, err)
		if err == nil && resp.StatusCode == http.StatusOK {
			return r.newStreamWatcher(ctx, resp)
		}

		done, transformErr := func() (bool, error) {
			defer readAndCloseResponseBody(resp)

			if retry.IsNextRetry(ctx, r, req, resp, err, isErrRetryableFunc) {
				return false, nil
			}

			if resp == nil {
				// the server must have sent us an error in 'err'
				return true, nil
			}
			result := r.transformResponse(ctx, resp, req)
			if err := result.Error(); err != nil {
				return true, err
			}
			return true, fmt.Errorf("for request %s, got status: %v", url, resp.StatusCode)
		}()
		if done {
			if isErrRetryableFunc(req, err) {
				return watch.NewEmptyWatch(), nil, nil
			}
			if err == nil {
				// if the server sent us an HTTP Response object,
				// we need to return the error object from that.
				err = transformErr
			}
			return nil, nil, retry.WrapPreviousError(err)
		}
	}
}

type WatchListResult struct {
	// err holds any errors we might have received
	// during streaming.
	err error

	// items hold the collected data
	items []runtime.Object

	// initialEventsEndBookmarkRV holds the resource version
	// extracted from the bookmark event that marks
	// the end of the stream.
	initialEventsEndBookmarkRV string

	// negotiatedObjectDecoder knows how to decode
	// the initialEventsListBlueprint
	negotiatedObjectDecoder runtime.Decoder

	// base64EncodedInitialEventsListBlueprint contains an empty,
	// versioned list encoded in the requested format
	// (e.g., protobuf, JSON, CBOR) and stored as a base64-encoded string
	base64EncodedInitialEventsListBlueprint string
}

// Into stores the result into obj. The passed obj parameter must be a pointer to a list type.
//
// Note:
//
// Special attention should be given to the type *unstructured.Unstructured,
// which represents a list type but does not have an "Items" field.
// Users who directly use RESTClient may store the response in such an object.
// This particular case is not handled by the current implementation of this function,
// but may be considered for future updates.
func (r WatchListResult) Into(obj runtime.Object) error {
	if r.err != nil {
		return r.err
	}

	listItemsPtr, err := meta.GetItemsPtr(obj)
	if err != nil {
		return err
	}
	listVal, err := conversion.EnforcePtr(listItemsPtr)
	if err != nil {
		return err
	}
	if listVal.Kind() != reflect.Slice {
		return fmt.Errorf("need a pointer to slice, got %v", listVal.Kind())
	}

	encodedInitialEventsListBlueprint, err := base64.StdEncoding.DecodeString(r.base64EncodedInitialEventsListBlueprint)
	if err != nil {
		return fmt.Errorf("failed to decode the received blueprint list, err %w", err)
	}

	err = runtime.DecodeInto(r.negotiatedObjectDecoder, encodedInitialEventsListBlueprint, obj)
	if err != nil {
		return err
	}

	if len(r.items) == 0 {
		listVal.Set(reflect.MakeSlice(listVal.Type(), 0, 0))
	} else {
		listVal.Set(reflect.MakeSlice(listVal.Type(), len(r.items), len(r.items)))
		for i, o := range r.items {
			if listVal.Type().Elem() != reflect.TypeOf(o).Elem() {
				return fmt.Errorf("received object type = %v at index = %d, doesn't match the list item type = %v", reflect.TypeOf(o).Elem(), i, listVal.Type().Elem())
			}
			listVal.Index(i).Set(reflect.ValueOf(o).Elem())
		}
	}

	listMeta, err := meta.ListAccessor(obj)
	if err != nil {
		return err
	}
	listMeta.SetResourceVersion(r.initialEventsEndBookmarkRV)
	return nil
}

// WatchList establishes a stream to get a consistent snapshot of data
// from the server as described in https://github.com/kubernetes/enhancements/tree/master/keps/sig-api-machinery/3157-watch-list#proposal
//
// Note that the watchlist requires properly setting the ListOptions
// otherwise it just establishes a regular watch with the server.
// Check the documentation https://kubernetes.io/docs/reference/using-api/api-concepts/#streaming-lists
// to see what parameters are currently required.
func (r *Request) WatchList(ctx context.Context) WatchListResult {
	if r.body == nil {
		logBody(klog.FromContext(ctx), 2, "Request Body", r.bodyBytes)
	}

	if !clientfeatures.FeatureGates().Enabled(clientfeatures.WatchListClient) {
		return WatchListResult{err: fmt.Errorf("%q feature gate is not enabled", clientfeatures.WatchListClient)}
	}
	// TODO(#115478): consider validating request parameters (i.e sendInitialEvents).
	//  Most users use the generated client, which handles the proper setting of parameters.
	//  We don't have validation for other methods (e.g., the Watch)
	//  thus, for symmetry, we haven't added additional checks for the WatchList method.
	w, d, err := r.watchInternal(ctx)
	if err != nil {
		return WatchListResult{err: err}
	}
	return r.handleWatchList(ctx, w, d)
}

// handleWatchList holds the actual logic for easier unit testing.
// Note that this function will close the passed watch.
func (r *Request) handleWatchList(ctx context.Context, w watch.Interface, negotiatedObjectDecoder runtime.Decoder) WatchListResult {
	defer w.Stop()
	var lastKey string
	var items []runtime.Object

	for {
		select {
		case <-ctx.Done():
			return WatchListResult{err: ctx.Err()}
		case event, ok := <-w.ResultChan():
			if !ok {
				return WatchListResult{err: fmt.Errorf("unexpected watch close")}
			}
			if event.Type == watch.Error {
				return WatchListResult{err: errors.FromObject(event.Object)}
			}
			meta, err := meta.Accessor(event.Object)
			if err != nil {
				return WatchListResult{err: fmt.Errorf("failed to parse watch event: %#v", event)}
			}

			switch event.Type {
			case watch.Added:
				// the following check ensures that the response is ordered.
				// earlier servers had a bug that caused them to not sort the output.
				// in such cases, return an error which can trigger fallback logic.
				key := objectKeyFromMeta(meta)
				if len(lastKey) > 0 && lastKey > key {
					return WatchListResult{err: fmt.Errorf("cannot add the obj (%#v) with the key = %s, as it violates the ordering guarantees provided by the watchlist feature in beta phase, lastInsertedKey was = %s", event.Object, key, lastKey)}
				}
				items = append(items, event.Object)
				lastKey = key
			case watch.Bookmark:
				if meta.GetAnnotations()[metav1.InitialEventsAnnotationKey] == "true" {
					base64EncodedInitialEventsListBlueprint := meta.GetAnnotations()[metav1.InitialEventsListBlueprintAnnotationKey]
					if len(base64EncodedInitialEventsListBlueprint) == 0 {
						return WatchListResult{err: fmt.Errorf("%q annotation is missing content", metav1.InitialEventsListBlueprintAnnotationKey)}
					}
					return WatchListResult{
						items:                                   items,
						initialEventsEndBookmarkRV:              meta.GetResourceVersion(),
						negotiatedObjectDecoder:                 negotiatedObjectDecoder,
						base64EncodedInitialEventsListBlueprint: base64EncodedInitialEventsListBlueprint,
					}
				}
			default:
				return WatchListResult{err: fmt.Errorf("unexpected watch event %#v, expected to only receive watch.Added and watch.Bookmark events", event)}
			}
		}
	}
}

func (r *Request) newStreamWatcher(ctx context.Context, resp *http.Response) (watch.Interface, runtime.Decoder, error) {
	contentType := resp.Header.Get("Content-Type")
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		klog.FromContext(ctx).V(4).Info("Unexpected content type from the server", "contentType", contentType, "err", err)
	}
	objectDecoder, streamingSerializer, framer, err := r.contentConfig.Negotiator.StreamDecoder(mediaType, params)
	if err != nil {
		return nil, nil, err
	}

	handleWarnings(ctx, resp.Header, r.warningHandler)

	frameReader := framer.NewFrameReader(resp.Body)
	watchEventDecoder := streaming.NewDecoder(frameReader, streamingSerializer)

	return watch.NewStreamWatcherWithLogger(
		klog.FromContext(ctx),
		restclientwatch.NewDecoder(watchEventDecoder, objectDecoder),
		// use 500 to indicate that the cause of the error is unknown - other error codes
		// are more specific to HTTP interactions, and set a reason
		errors.NewClientErrorReporter(http.StatusInternalServerError, r.verb, "ClientWatchDecoding"),
	), objectDecoder, nil
}

// updateRequestResultMetric increments the RequestResult metric counter,
// it should be called with the (response, err) tuple from the final
// reply from the server.
func updateRequestResultMetric(ctx context.Context, req *Request, resp *http.Response, err error) {
	code, host := sanitize(req, resp, err)
	metrics.RequestResult.Increment(ctx, code, req.verb, host)
}

// updateRequestRetryMetric increments the RequestRetry metric counter,
// it should be called with the (response, err) tuple for each retry
// except for the final attempt.
func updateRequestRetryMetric(ctx context.Context, req *Request, resp *http.Response, err error) {
	code, host := sanitize(req, resp, err)
	metrics.RequestRetry.IncrementRetry(ctx, code, req.verb, host)
}

func sanitize(req *Request, resp *http.Response, err error) (string, string) {
	host := "none"
	if req.c.base != nil {
		host = req.c.base.Host
	}

	// Errors can be arbitrary strings. Unbound label cardinality is not suitable for a metric
	// system so we just report them as `<error>`.
	code := "<error>"
	if resp != nil {
		code = strconv.Itoa(resp.StatusCode)
	}

	return code, host
}

// Stream formats and executes the request, and offers streaming of the response.
// Returns io.ReadCloser which could be used for streaming of the response, or an error
// Any non-2xx http status code causes an error.  If we get a non-2xx code, we try to convert the body into an APIStatus object.
// If we can, we return that as an error.  Otherwise, we create an error that lists the http status and the content of the response.
func (r *Request) Stream(ctx context.Context) (io.ReadCloser, error) {
	if r.body == nil {
		logBody(klog.FromContext(ctx), 2, "Request Body", r.bodyBytes)
	}

	if r.err != nil {
		return nil, r.err
	}

	if err := r.tryThrottle(ctx); err != nil {
		return nil, err
	}

	client := r.c.Client
	if client == nil {
		client = http.DefaultClient
	}

	retry := r.retryFn(r.maxRetries)
	url := r.URL().String()
	for {
		if err := retry.Before(ctx, r); err != nil {
			return nil, err
		}

		req, err := r.newHTTPRequest(ctx)
		if err != nil {
			return nil, err
		}
		resp, err := client.Do(req)
		retry.After(ctx, r, resp, err)
		if err != nil {
			// we only retry on an HTTP response with 'Retry-After' header
			return nil, err
		}

		switch {
		case (resp.StatusCode >= 200) && (resp.StatusCode < 300):
			handleWarnings(ctx, resp.Header, r.warningHandler)
			return resp.Body, nil

		default:
			done, transformErr := func() (bool, error) {
				defer resp.Body.Close()

				if retry.IsNextRetry(ctx, r, req, resp, err, neverRetryError) {
					return false, nil
				}
				result := r.transformResponse(ctx, resp, req)
				if err := result.Error(); err != nil {
					return true, err
				}
				return true, fmt.Errorf("%d while accessing %v: %s", result.statusCode, url, string(result.body))
			}()
			if done {
				return nil, transformErr
			}
		}
	}
}

// requestPreflightCheck looks for common programmer errors on Request.
//
// We tackle here two programmer mistakes. The first one is to try to create
// something(POST) using an empty string as namespace with namespaceSet as
// true. If namespaceSet is true then namespace should also be defined. The
// second mistake is, when under the same circumstances, the programmer tries
// to GET, PUT or DELETE a named resource(resourceName != ""), again, if
// namespaceSet is true then namespace must not be empty.
func (r *Request) requestPreflightCheck() error {
	if !r.namespaceSet {
		return nil
	}
	if len(r.namespace) > 0 {
		return nil
	}

	switch r.verb {
	case "POST":
		return fmt.Errorf("an empty namespace may not be set during creation")
	case "GET", "PUT", "DELETE":
		if len(r.resourceName) > 0 {
			return fmt.Errorf("an empty namespace may not be set when a resource name is provided")
		}
	}
	return nil
}

func (r *Request) newHTTPRequest(ctx context.Context) (*http.Request, error) {
	var body io.Reader
	switch {
	case r.body != nil && r.bodyBytes != nil:
		return nil, fmt.Errorf("cannot set both body and bodyBytes")
	case r.body != nil:
		body = r.body
	case r.bodyBytes != nil:
		// Create a new reader specifically for this request.
		// Giving each request a dedicated reader allows retries to avoid races resetting the request body.
		body = bytes.NewReader(r.bodyBytes)
	}

	url := r.URL().String()
	req, err := http.NewRequestWithContext(httptrace.WithClientTrace(ctx, newDNSMetricsTrace(ctx)), r.verb, url, body)
	if err != nil {
		return nil, err
	}
	req.Header = r.headers
	return req, nil
}

// newDNSMetricsTrace returns an HTTP trace that tracks time spent on DNS lookups per host.
// This metric is available in client as "rest_client_dns_resolution_duration_seconds".
func newDNSMetricsTrace(ctx context.Context) *httptrace.ClientTrace {
	type dnsMetric struct {
		start time.Time
		host  string
		sync.Mutex
	}
	dns := &dnsMetric{}
	return &httptrace.ClientTrace{
		DNSStart: func(info httptrace.DNSStartInfo) {
			dns.Lock()
			defer dns.Unlock()
			dns.start = time.Now()
			dns.host = info.Host
		},
		DNSDone: func(info httptrace.DNSDoneInfo) {
			dns.Lock()
			defer dns.Unlock()
			metrics.ResolverLatency.Observe(ctx, dns.host, time.Since(dns.start))
		},
	}
}

// request connects to the server and invokes the provided function when a server response is
// received. It handles retry behavior and up front validation of requests. It will invoke
// fn at most once. It will return an error if a problem occurred prior to connecting to the
// server - the provided function is responsible for handling server errors.
func (r *Request) request(ctx context.Context, fn func(*http.Request, *http.Response)) error {
	// Metrics for total request latency
	start := time.Now()
	defer func() {
		metrics.RequestLatency.Observe(ctx, r.verb, r.finalURLTemplate(), time.Since(start))
	}()

	if r.err != nil {
		klog.FromContext(ctx).V(4).Info("Error in request", "err", r.err)
		return r.err
	}

	if err := r.requestPreflightCheck(); err != nil {
		return err
	}

	client := r.c.Client
	if client == nil {
		client = http.DefaultClient
	}

	// Throttle the first try before setting up the timeout configured on the
	// client. We don't want a throttled client to return timeouts to callers
	// before it makes a single request.
	if err := r.tryThrottle(ctx); err != nil {
		return err
	}

	if r.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.timeout)
		defer cancel()
	}

	isErrRetryableFunc := func(req *http.Request, err error) bool {
		// "Connection reset by peer" or "apiserver is shutting down" are usually a transient errors.
		// Thus in case of "GET" operations, we simply retry it.
		// We are not automatically retrying "write" operations, as they are not idempotent.
		if req.Method != "GET" {
			return false
		}
		// For connection errors and apiserver shutdown errors retry.
		if net.IsConnectionReset(err) || net.IsProbableEOF(err) || net.IsHTTP2ConnectionLost(err) {
			return true
		}
		return false
	}

	// Right now we make about ten retry attempts if we get a Retry-After response.
	retry := r.retryFn(r.maxRetries)
	for {
		if err := retry.Before(ctx, r); err != nil {
			return retry.WrapPreviousError(err)
		}
		req, err := r.newHTTPRequest(ctx)
		if err != nil {
			return err
		}
		resp, err := client.Do(req)
		// The value -1 or a value of 0 with a non-nil Body indicates that the length is unknown.
		// https://pkg.go.dev/net/http#Request
		if req.ContentLength >= 0 && !(req.Body != nil && req.ContentLength == 0) {
			metrics.RequestSize.Observe(ctx, r.verb, r.URL().Host, float64(req.ContentLength))
		}
		if resp != nil && resp.StatusCode == http.StatusUnsupportedMediaType {
			r.c.content.UnsupportedMediaType(resp.Request.Header.Get("Content-Type"))
		}
		retry.After(ctx, r, resp, err)

		done := func() bool {
			defer readAndCloseResponseBody(resp)

			// if the server returns an error in err, the response will be nil.
			f := func(req *http.Request, resp *http.Response) {
				if resp == nil {
					return
				}
				fn(req, resp)
			}

			if retry.IsNextRetry(ctx, r, req, resp, err, isErrRetryableFunc) {
				return false
			}

			f(req, resp)
			return true
		}()
		if done {
			return retry.WrapPreviousError(err)
		}
	}
}

// Do formats and executes the request. Returns a Result object for easy response
// processing.
//
// Error type:
//   - If the server responds with a status: *errors.StatusError or *errors.UnexpectedObjectError
//   - http.Client.Do errors are returned directly.
func (r *Request) Do(ctx context.Context) Result {
	logger := klog.FromContext(ctx)
	if r.body == nil {
		logBody(logger, 2, "Request Body", r.bodyBytes)
	}

	var result Result
	err := r.request(ctx, func(req *http.Request, resp *http.Response) {
		result = r.transformResponse(ctx, resp, req)
	})
	if err != nil {
		return Result{err: err, logger: logger}
	}
	if result.err == nil || len(result.body) > 0 {
		metrics.ResponseSize.Observe(ctx, r.verb, r.URL().Host, float64(len(result.body)))
	}
	return result
}

// DoRaw executes the request but does not process the response body.
func (r *Request) DoRaw(ctx context.Context) ([]byte, error) {
	logger := klog.FromContext(ctx)
	if r.body == nil {
		logBody(logger, 2, "Request Body", r.bodyBytes)
	}

	var result Result
	err := r.request(ctx, func(req *http.Request, resp *http.Response) {
		result.body, result.err = io.ReadAll(resp.Body)
		logBody(logger, 2, "Response Body", result.body)
		if resp.StatusCode < http.StatusOK || resp.StatusCode > http.StatusPartialContent {
			result.err = r.transformUnstructuredResponseError(resp, req, result.body)
		}
	})
	if err != nil {
		return nil, err
	}
	if result.err == nil || len(result.body) > 0 {
		metrics.ResponseSize.Observe(ctx, r.verb, r.URL().Host, float64(len(result.body)))
	}
	return result.body, result.err
}

// transformResponse converts an API response into a structured API object
func (r *Request) transformResponse(ctx context.Context, resp *http.Response, req *http.Request) Result {
	logger := klog.FromContext(ctx)
	var body []byte
	if resp.Body != nil {
		data, err := io.ReadAll(resp.Body)
		switch err.(type) {
		case nil:
			body = data
		case http2.StreamError:
			// This is trying to catch the scenario that the server may close the connection when sending the
			// response body. This can be caused by server timeout due to a slow network connection.
			// TODO: Add test for this. Steps may be:
			// 1. client-go (or kubectl) sends a GET request.
			// 2. Apiserver sends back the headers and then part of the body
			// 3. Apiserver closes connection.
			// 4. client-go should catch this and return an error.
			logger.V(2).Info("Stream error when reading response body, may be caused by closed connection", "err", err)
			streamErr := fmt.Errorf("stream error when reading response body, may be caused by closed connection. Please retry. Original error: %w", err)
			return Result{
				err:    streamErr,
				logger: logger,
			}
		default:
			logger.Error(err, "Unexpected error when reading response body")
			unexpectedErr := fmt.Errorf("unexpected error when reading response body. Please retry. Original error: %w", err)
			return Result{
				err:    unexpectedErr,
				logger: logger,
			}
		}
	}

	// Call depth is tricky. This one is okay for Do and DoRaw.
	logBody(logger, 7, "Response Body", body)

	// verify the content type is accurate
	var decoder runtime.Decoder
	contentType := resp.Header.Get("Content-Type")
	if len(contentType) == 0 {
		contentType = r.contentConfig.ContentType
	}
	if len(contentType) > 0 {
		var err error
		mediaType, params, err := mime.ParseMediaType(contentType)
		if err != nil {
			return Result{err: errors.NewInternalError(err), logger: logger}
		}
		decoder, err = r.contentConfig.Negotiator.Decoder(mediaType, params)
		if err != nil {
			// if we fail to negotiate a decoder, treat this as an unstructured error
			switch {
			case resp.StatusCode == http.StatusSwitchingProtocols:
				// no-op, we've been upgraded
			case resp.StatusCode < http.StatusOK || resp.StatusCode > http.StatusPartialContent:
				return Result{err: r.transformUnstructuredResponseError(resp, req, body), logger: logger}
			}
			return Result{
				body:        body,
				contentType: contentType,
				statusCode:  resp.StatusCode,
				warnings:    handleWarnings(ctx, resp.Header, r.warningHandler),
				logger:      logger,
			}
		}
	}

	switch {
	case resp.StatusCode == http.StatusSwitchingProtocols:
		// no-op, we've been upgraded
	case resp.StatusCode < http.StatusOK || resp.StatusCode > http.StatusPartialContent:
		// calculate an unstructured error from the response which the Result object may use if the caller
		// did not return a structured error.
		retryAfter, _ := retryAfterSeconds(resp)
		err := r.newUnstructuredResponseError(body, isTextResponse(resp), resp.StatusCode, req.Method, retryAfter)
		return Result{
			body:        body,
			contentType: contentType,
			statusCode:  resp.StatusCode,
			decoder:     decoder,
			err:         err,
			warnings:    handleWarnings(ctx, resp.Header, r.warningHandler),
			logger:      logger,
		}
	}

	return Result{
		body:        body,
		contentType: contentType,
		statusCode:  resp.StatusCode,
		decoder:     decoder,
		warnings:    handleWarnings(ctx, resp.Header, r.warningHandler),
		logger:      logger,
	}
}

// truncateBody decides if the body should be truncated, based on the glog Verbosity.
func truncateBody(logger klog.Logger, body string) string {
	max := 0
	switch {
	case bool(logger.V(10).Enabled()):
		return body
	case bool(logger.V(9).Enabled()):
		max = 10240
	case bool(logger.V(8).Enabled()):
		max = 1024
	}

	if len(body) <= max {
		return body
	}

	return body[:max] + fmt.Sprintf(" [truncated %d chars]", len(body)-max)
}

// logBody logs a body output that could be either JSON or protobuf. It explicitly guards against
// allocating a new string for the body output unless necessary. Uses a simple heuristic to determine
// whether the body is printable.
//
// It needs to be called by all functions which send or receive the data.
func logBody(logger klog.Logger, callDepth int, prefix string, body []byte) {
	if loggerV := logger.V(8); loggerV.Enabled() {
		loggerV := loggerV.WithCallDepth(callDepth)
		if bytes.IndexFunc(body, func(r rune) bool {
			return r < 0x0a
		}) != -1 {
			loggerV.Info(prefix, "body", truncateBody(logger, hex.Dump(body)))
		} else {
			loggerV.Info(prefix, "body", truncateBody(logger, string(body)))
		}
	}
}

// maxUnstructuredResponseTextBytes is an upper bound on how much output to include in the unstructured error.
const maxUnstructuredResponseTextBytes = 2048

// transformUnstructuredResponseError handles an error from the server that is not in a structured form.
// It is expected to transform any response that is not recognizable as a clear server sent error from the
// K8S API using the information provided with the request. In practice, HTTP proxies and client libraries
// introduce a level of uncertainty to the responses returned by servers that in common use result in
// unexpected responses. The rough structure is:
//
// 1. Assume the server sends you something sane - JSON + well defined error objects + proper codes
//   - this is the happy path
//   - when you get this output, trust what the server sends
//     2. Guard against empty fields / bodies in received JSON and attempt to cull sufficient info from them to
//     generate a reasonable facsimile of the original failure.
//   - Be sure to use a distinct error type or flag that allows a client to distinguish between this and error 1 above
//     3. Handle true disconnect failures / completely malformed data by moving up to a more generic client error
//     4. Distinguish between various connection failures like SSL certificates, timeouts, proxy errors, unexpected
//     initial contact, the presence of mismatched body contents from posted content types
//   - Give these a separate distinct error type and capture as much as possible of the original message
//
// TODO: introduce transformation of generic http.Client.Do() errors that separates 4.
func (r *Request) transformUnstructuredResponseError(resp *http.Response, req *http.Request, body []byte) error {
	if body == nil && resp.Body != nil {
		if data, err := io.ReadAll(&io.LimitedReader{R: resp.Body, N: maxUnstructuredResponseTextBytes}); err == nil {
			body = data
		}
	}
	retryAfter, _ := retryAfterSeconds(resp)
	return r.newUnstructuredResponseError(body, isTextResponse(resp), resp.StatusCode, req.Method, retryAfter)
}

// newUnstructuredResponseError instantiates the appropriate generic error for the provided input. It also logs the body.
func (r *Request) newUnstructuredResponseError(body []byte, isTextResponse bool, statusCode int, method string, retryAfter int) error {
	// cap the amount of output we create
	if len(body) > maxUnstructuredResponseTextBytes {
		body = body[:maxUnstructuredResponseTextBytes]
	}

	message := "unknown"
	if isTextResponse {
		message = strings.TrimSpace(string(body))
	}
	var groupResource schema.GroupResource
	if len(r.resource) > 0 {
		groupResource.Group = r.contentConfig.GroupVersion.Group
		groupResource.Resource = r.resource
	}
	return errors.NewGenericServerResponse(
		statusCode,
		method,
		groupResource,
		r.resourceName,
		message,
		retryAfter,
		true,
	)
}

// isTextResponse returns true if the response appears to be a textual media type.
func isTextResponse(resp *http.Response) bool {
	contentType := resp.Header.Get("Content-Type")
	if len(contentType) == 0 {
		return true
	}
	media, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return false
	}
	return strings.HasPrefix(media, "text/")
}

// retryAfterSeconds returns the value of the Retry-After header and true, or 0 and false if
// the header was missing or not a valid number.
func retryAfterSeconds(resp *http.Response) (int, bool) {
	if h := resp.Header.Get("Retry-After"); len(h) > 0 {
		if i, err := strconv.Atoi(h); err == nil {
			return i, true
		}
	}
	return 0, false
}

// Result contains the result of calling Request.Do().
type Result struct {
	body        []byte
	warnings    []net.WarningHeader
	contentType string
	err         error
	statusCode  int
	logger      klog.Logger

	decoder runtime.Decoder
}

// Raw returns the raw result.
func (r Result) Raw() ([]byte, error) {
	return r.body, r.err
}

// Get returns the result as an object, which means it passes through the decoder.
// If the returned object is of type Status and has .Status != StatusSuccess, the
// additional information in Status will be used to enrich the error.
func (r Result) Get() (runtime.Object, error) {
	if r.err != nil {
		// Check whether the result has a Status object in the body and prefer that.
		return nil, r.Error()
	}
	if r.decoder == nil {
		return nil, fmt.Errorf("serializer for %s doesn't exist", r.contentType)
	}

	// decode, but if the result is Status return that as an error instead.
	out, _, err := r.decoder.Decode(r.body, nil, nil)
	if err != nil {
		return nil, err
	}
	switch t := out.(type) {
	case *metav1.Status:
		// any status besides StatusSuccess is considered an error.
		if t.Status != metav1.StatusSuccess {
			return nil, errors.FromObject(t)
		}
	}
	return out, nil
}

// StatusCode returns the HTTP status code of the request. (Only valid if no
// error was returned.)
func (r Result) StatusCode(statusCode *int) Result {
	*statusCode = r.statusCode
	return r
}

// ContentType returns the "Content-Type" response header into the passed
// string, returning the Result for possible chaining. (Only valid if no
// error code was returned.)
func (r Result) ContentType(contentType *string) Result {
	*contentType = r.contentType
	return r
}

// Into stores the result into obj, if possible. If obj is nil it is ignored.
// If the returned object is of type Status and has .Status != StatusSuccess, the
// additional information in Status will be used to enrich the error.
func (r Result) Into(obj runtime.Object) error {
	if r.err != nil {
		// Check whether the result has a Status object in the body and prefer that.
		return r.Error()
	}
	if r.decoder == nil {
		return fmt.Errorf("serializer for %s doesn't exist", r.contentType)
	}
	if len(r.body) == 0 {
		return fmt.Errorf("0-length response with status code: %d and content type: %s",
			r.statusCode, r.contentType)
	}

	out, _, err := r.decoder.Decode(r.body, nil, obj)
	if err != nil || out == obj {
		return err
	}
	// if a different object is returned, see if it is Status and avoid double decoding
	// the object.
	switch t := out.(type) {
	case *metav1.Status:
		// any status besides StatusSuccess is considered an error.
		if t.Status != metav1.StatusSuccess {
			return errors.FromObject(t)
		}
	}
	return nil
}

// WasCreated updates the provided bool pointer to whether the server returned
// 201 created or a different response.
func (r Result) WasCreated(wasCreated *bool) Result {
	*wasCreated = r.statusCode == http.StatusCreated
	return r
}

// Error returns the error executing the request, nil if no error occurred.
// If the returned object is of type Status and has Status != StatusSuccess, the
// additional information in Status will be used to enrich the error.
// See the Request.Do() comment for what errors you might get.
func (r Result) Error() error {
	// if we have received an unexpected server error, and we have a body and decoder, we can try to extract
	// a Status object.
	if r.err == nil || !errors.IsUnexpectedServerError(r.err) || len(r.body) == 0 || r.decoder == nil {
		return r.err
	}

	// attempt to convert the body into a Status object
	// to be backwards compatible with old servers that do not return a version, default to "v1"
	out, _, err := r.decoder.Decode(r.body, &schema.GroupVersionKind{Version: "v1"}, nil)
	if err != nil {
		r.logger.V(5).Info("Body was not decodable (unable to check for Status)", "err", err)
		return r.err
	}
	switch t := out.(type) {
	case *metav1.Status:
		// because we default the kind, we *must* check for StatusFailure
		if t.Status == metav1.StatusFailure {
			return errors.FromObject(t)
		}
	}
	return r.err
}

// Warnings returns any warning headers received in the response
func (r Result) Warnings() []net.WarningHeader {
	return r.warnings
}

// NameMayNotBe specifies strings that cannot be used as names specified as path segments (like the REST API or etcd store)
var NameMayNotBe = []string{".", ".."}

// NameMayNotContain specifies substrings that cannot be used in names specified as path segments (like the REST API or etcd store)
var NameMayNotContain = []string{"/", "%"}

// IsValidPathSegmentName validates the name can be safely encoded as a path segment
func IsValidPathSegmentName(name string) []string {
	for _, illegalName := range NameMayNotBe {
		if name == illegalName {
			return []string{fmt.Sprintf(`may not be '%s'`, illegalName)}
		}
	}

	var errors []string
	for _, illegalContent := range NameMayNotContain {
		if strings.Contains(name, illegalContent) {
			errors = append(errors, fmt.Sprintf(`may not contain '%s'`, illegalContent))
		}
	}

	return errors
}

// IsValidPathSegmentPrefix validates the name can be used as a prefix for a name which will be encoded as a path segment
// It does not check for exact matches with disallowed names, since an arbitrary suffix might make the name valid
func IsValidPathSegmentPrefix(name string) []string {
	var errors []string
	for _, illegalContent := range NameMayNotContain {
		if strings.Contains(name, illegalContent) {
			errors = append(errors, fmt.Sprintf(`may not contain '%s'`, illegalContent))
		}
	}

	return errors
}

// ValidatePathSegmentName validates the name can be safely encoded as a path segment
func ValidatePathSegmentName(name string, prefix bool) []string {
	if prefix {
		return IsValidPathSegmentPrefix(name)
	}
	return IsValidPathSegmentName(name)
}

func objectKeyFromMeta(objMeta metav1.Object) string {
	if len(objMeta.GetNamespace()) > 0 {
		return fmt.Sprintf("%s/%s", objMeta.GetNamespace(), objMeta.GetName())
	}
	return objMeta.GetName()
}
