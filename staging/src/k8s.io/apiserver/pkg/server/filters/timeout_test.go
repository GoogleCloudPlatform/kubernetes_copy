/*
Copyright 2016 The Kubernetes Authors.

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

package filters

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"golang.org/x/net/http2"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/endpoints/responsewriter"
	"k8s.io/klog/v2"
)

type recorder struct {
	lock  sync.Mutex
	count int
}

func (r *recorder) Record() {
	r.lock.Lock()
	defer r.lock.Unlock()
	r.count++
}

func (r *recorder) Count() int {
	r.lock.Lock()
	defer r.lock.Unlock()
	return r.count
}

func newHandler(responseCh <-chan string, panicCh <-chan interface{}, writeErrCh chan<- error) http.HandlerFunc {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case resp := <-responseCh:
			_, err := w.Write([]byte(resp))
			writeErrCh <- err
		case panicReason := <-panicCh:
			panic(panicReason)
		}
	})
}

func TestTimeout(t *testing.T) {
	origReallyCrash := runtime.ReallyCrash
	runtime.ReallyCrash = false
	defer func() {
		runtime.ReallyCrash = origReallyCrash
	}()

	sendResponse := make(chan string, 1)
	doPanic := make(chan interface{}, 1)
	writeErrors := make(chan error, 1)
	gotPanic := make(chan interface{}, 1)
	timeout := make(chan time.Time, 1)
	resp := "test response"
	timeoutErr := apierrors.NewServerTimeout(schema.GroupResource{Group: "foo", Resource: "bar"}, "get", 0)
	record := &recorder{}

	var ctx context.Context
	withDeadline := func(handler http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			req = req.WithContext(ctx)
			handler.ServeHTTP(w, req)
		})
	}

	handler := newHandler(sendResponse, doPanic, writeErrors)
	ts := httptest.NewServer(withDeadline(withPanicRecovery(
		WithTimeout(handler, func(req *http.Request) (*http.Request, bool, func(), *apierrors.StatusError) {
			return req, false, record.Record, timeoutErr
		}), func(w http.ResponseWriter, req *http.Request, err interface{}) {
			gotPanic <- err
			http.Error(w, "This request caused apiserver to panic. Look in the logs for details.", http.StatusInternalServerError)
		}),
	))
	defer ts.Close()

	// No timeouts
	ctx = context.Background()
	sendResponse <- resp
	res, err := http.Get(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusOK {
		t.Errorf("got res.StatusCode %d; expected %d", res.StatusCode, http.StatusOK)
	}
	body, _ := io.ReadAll(res.Body)
	if string(body) != resp {
		t.Errorf("got body %q; expected %q", string(body), resp)
	}
	if err := <-writeErrors; err != nil {
		t.Errorf("got unexpected Write error on first request: %v", err)
	}
	if record.Count() != 0 {
		t.Errorf("invoked record method: %#v", record)
	}

	// Times out
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	timeout <- time.Time{}
	res, err = http.Get(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusGatewayTimeout {
		t.Errorf("got res.StatusCode %d; expected %d", res.StatusCode, http.StatusGatewayTimeout)
	}
	body, _ = io.ReadAll(res.Body)
	status := &metav1.Status{}
	if err := json.Unmarshal(body, status); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(status, &timeoutErr.ErrStatus) {
		t.Errorf("unexpected object: %s", cmp.Diff(&timeoutErr.ErrStatus, status))
	}
	if record.Count() != 1 {
		t.Errorf("did not invoke record method: %#v", record)
	}

	// Now try to send a response
	ctx = context.Background()
	sendResponse <- resp
	if err := <-writeErrors; err != http.ErrHandlerTimeout {
		t.Errorf("got Write error of %v; expected %v", err, http.ErrHandlerTimeout)
	}

	// Panics
	doPanic <- "inner handler panics"
	res, err = http.Get(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusInternalServerError {
		t.Errorf("got res.StatusCode %d; expected %d due to panic", res.StatusCode, http.StatusInternalServerError)
	}
	select {
	case err := <-gotPanic:
		msg := fmt.Sprintf("%v", err)
		if !strings.Contains(msg, "newHandler") {
			t.Errorf("expected line with root cause panic in the stack trace, but didn't: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("expected to see a handler panic, but didn't")
	}

	// Panics with http.ErrAbortHandler
	ctx = context.Background()
	doPanic <- http.ErrAbortHandler
	res, err = http.Get(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusInternalServerError {
		t.Errorf("got res.StatusCode %d; expected %d due to panic", res.StatusCode, http.StatusInternalServerError)
	}
	select {
	case err := <-gotPanic:
		if err != http.ErrAbortHandler {
			t.Errorf("expected unwrapped http.ErrAbortHandler, got %#v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("expected to see a handler panic, but didn't")
	}
}

func TestTimeoutHeaders(t *testing.T) {
	origReallyCrash := runtime.ReallyCrash
	runtime.ReallyCrash = false
	defer func() {
		runtime.ReallyCrash = origReallyCrash
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	withDeadline := func(handler http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			handler.ServeHTTP(w, req.WithContext(ctx))
		})
	}

	postTimeoutCh := make(chan struct{})
	ts := httptest.NewServer(
		withDeadline(
			WithTimeout(
				http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
					h := w.Header()
					// trigger the timeout
					cancel()
					// keep mutating response Headers until the request times out
					for {
						select {
						case <-postTimeoutCh:
							return
						default:
							h.Set("Test", "post")
						}
					}
				}),
				func(req *http.Request) (*http.Request, bool, func(), *apierrors.StatusError) {
					return req, false, func() { close(postTimeoutCh) }, apierrors.NewServerTimeout(schema.GroupResource{Group: "foo", Resource: "bar"}, "get", 0)
				},
			),
		),
	)
	defer ts.Close()

	res, err := http.Get(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusGatewayTimeout {
		t.Errorf("got res.StatusCode %d; expected %d", res.StatusCode, http.StatusGatewayTimeout)
	}
	res.Body.Close()
}

func TestTimeoutRequestHeaders(t *testing.T) {
	origReallyCrash := runtime.ReallyCrash
	runtime.ReallyCrash = false
	defer func() {
		runtime.ReallyCrash = origReallyCrash
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Add dummy request info, otherwise we skip postTimeoutFn
	ctx = request.WithRequestInfo(ctx, &request.RequestInfo{})

	withDeadline := func(handler http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			handler.ServeHTTP(w, req.WithContext(ctx))
		})
	}

	testDone := make(chan struct{})
	defer close(testDone)
	ts := httptest.NewServer(
		withDeadline(
			WithTimeoutForNonLongRunningRequests(
				http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
					// trigger the timeout
					cancel()
					// mutate request Headers
					// Authorization filter does it for example
					for {
						select {
						case <-testDone:
							return
						default:
							req.Header.Set("Test", "post")
						}
					}
				}),
				func(r *http.Request, requestInfo *request.RequestInfo) bool {
					return false
				},
			),
		),
	)
	defer ts.Close()

	client := &http.Client{}
	req, err := http.NewRequest(http.MethodPatch, ts.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if actual, expected := res.StatusCode, http.StatusGatewayTimeout; actual != expected {
		t.Errorf("got status code %d; expected %d", actual, expected)
	}
	res.Body.Close()
}

func TestTimeoutWithLogging(t *testing.T) {
	origReallyCrash := runtime.ReallyCrash
	runtime.ReallyCrash = false
	defer func() {
		runtime.ReallyCrash = origReallyCrash
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	withDeadline := func(handler http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			handler.ServeHTTP(w, req.WithContext(ctx))
		})
	}

	testDone := make(chan struct{})
	defer close(testDone)
	ts := httptest.NewServer(
		WithHTTPLogging(
			withDeadline(
				WithTimeoutForNonLongRunningRequests(
					http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
						// trigger the timeout
						cancel()
						// mutate request Headers
						// Authorization filter does it for example
						for {
							select {
							case <-testDone:
								return
							default:
								req.Header.Set("Test", "post")
							}
						}
					}),
					func(r *http.Request, requestInfo *request.RequestInfo) bool {
						return false
					},
				),
			),
		),
	)
	defer ts.Close()

	client := &http.Client{}
	req, err := http.NewRequest(http.MethodPatch, ts.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if actual, expected := res.StatusCode, http.StatusGatewayTimeout; actual != expected {
		t.Errorf("got status code %d; expected %d", actual, expected)
	}
	res.Body.Close()
}

func TestErrConnKilled(t *testing.T) {
	var buf bytes.Buffer
	klog.SetOutput(&buf)
	klog.LogToStderr(false)
	defer klog.LogToStderr(true)

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// this error must be ignored by the WithPanicRecovery handler
		// it is thrown by WithTimeoutForNonLongRunningRequests handler when a response has been already sent to the client and the handler timed out
		// panicking with http.ErrAbortHandler also suppresses logging of a stack trace to the server's error log and closes the underlying connection
		w.Write([]byte("hello from the handler"))
		panic(http.ErrAbortHandler)
	})
	resolver := &request.RequestInfoFactory{
		APIPrefixes:          sets.NewString("api", "apis"),
		GrouplessAPIPrefixes: sets.NewString("api"),
	}

	ts := httptest.NewServer(WithPanicRecovery(handler, resolver))
	defer ts.Close()

	_, err := http.Get(ts.URL)
	if err == nil {
		t.Fatal("expected to receive an error")
	}

	klog.Flush()
	klog.SetOutput(&bytes.Buffer{}) // prevent further writes into buf
	capturedOutput := buf.String()

	// We don't expect stack trace from the panic to be included in the log.
	if isStackTraceLoggedByRuntime(capturedOutput) {
		t.Errorf("unexpected stack trace in log, actual = %v", capturedOutput)
	}
	// For the sake of simplicity and clarity this matches the full log line.
	// This is not part of the Kubernetes API and could change.
	if !strings.Contains(capturedOutput, `"Timeout or abort while handling" logger="UnhandledError" method="GET" URI="/" auditID=""`) {
		t.Errorf("unexpected output captured actual = %v", capturedOutput)
	}
}

type panicOnNonReuseTransport struct {
	Transport   http.RoundTripper
	gotConnSeen bool
}

func (t *panicOnNonReuseTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return t.Transport.RoundTrip(req)
}

func (t *panicOnNonReuseTransport) GotConn(info httptrace.GotConnInfo) {
	if !t.gotConnSeen {
		t.gotConnSeen = true
		return
	}
	if !info.Reused {
		panic(fmt.Sprintf("expected the connection to be reused, info %#v", info))
	}
}

// TestErrConnKilledHTTP2 check if HTTP/2 connection is not closed when an HTTP handler panics
// The net/http library recovers the panic and sends an HTTP/2 RST_STREAM.
func TestErrConnKilledHTTP2(t *testing.T) {
	var buf bytes.Buffer
	klog.SetOutput(&buf)
	klog.LogToStderr(false)
	defer klog.LogToStderr(true)

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// this error must be ignored by the WithPanicRecovery handler
		// it is thrown by WithTimeoutForNonLongRunningRequests handler when a response has been already sent to the client and the handler timed out
		// panicking with http.ErrAbortHandler also suppresses logging of a stack trace to the server's error log and closes the underlying connection
		w.Write([]byte("hello from the handler"))
		panic(http.ErrAbortHandler)
	})
	resolver := &request.RequestInfoFactory{
		APIPrefixes:          sets.NewString("api", "apis"),
		GrouplessAPIPrefixes: sets.NewString("api"),
	}

	// test server
	ts := httptest.NewUnstartedServer(WithPanicRecovery(handler, resolver))
	tsCert, err := tls.X509KeyPair(tsCrt, tsKey)
	if err != nil {
		t.Fatalf("backend: invalid x509/key pair: %v", err)
	}
	ts.TLS = &tls.Config{
		Certificates: []tls.Certificate{tsCert},
		NextProtos:   []string{http2.NextProtoTLS},
	}
	ts.StartTLS()
	defer ts.Close()

	newServerRequest := func(tr *panicOnNonReuseTransport) *http.Request {
		req, _ := http.NewRequest("GET", fmt.Sprintf("https://127.0.0.1:%d", ts.Listener.Addr().(*net.TCPAddr).Port), nil)
		trace := &httptrace.ClientTrace{
			GotConn: tr.GotConn,
		}
		return req.WithContext(httptrace.WithClientTrace(req.Context(), trace))
	}

	// client
	clientCACertPool := x509.NewCertPool()
	clientCACertPool.AppendCertsFromPEM(tsCrt)
	clientTLSConfig := &tls.Config{
		RootCAs:    clientCACertPool,
		NextProtos: []string{http2.NextProtoTLS},
	}
	tr := &panicOnNonReuseTransport{}
	client := &http.Client{}
	tr.Transport = &http2.Transport{
		TLSClientConfig: clientTLSConfig,
	}
	client.Transport = tr

	// act
	_, err = client.Do(newServerRequest(tr))
	if err == nil {
		t.Fatal("expected to receive an error")
	}

	klog.Flush()
	klog.SetOutput(&bytes.Buffer{}) // prevent further writes into buf
	capturedOutput := buf.String()

	// We don't expect stack trace from the panic to be included in the log.
	if isStackTraceLoggedByRuntime(capturedOutput) {
		t.Errorf("unexpected stack trace in log, actual = %v", capturedOutput)
	}
	// For the sake of simplicity and clarity this matches the full log line.
	// This is not part of the Kubernetes API and could change.
	if !strings.Contains(capturedOutput, `"Timeout or abort while handling" logger="UnhandledError" method="GET" URI="/" auditID=""`) {
		t.Errorf("unexpected output captured actual = %v", capturedOutput)
	}

	// make another req to the server
	// the connection should be reused
	// the client uses a custom transport that checks and panics when the con wasn't reused.
	_, err = client.Do(newServerRequest(tr))
	if err == nil {
		t.Fatal("expected to receive an error")
	}
}

func TestResponseWriterDecorator(t *testing.T) {
	decorator := &baseTimeoutWriter{
		w: &responsewriter.FakeResponseWriter{},
	}
	var w http.ResponseWriter = decorator

	if inner := w.(responsewriter.UserProvidedDecorator).Unwrap(); inner != decorator.w {
		t.Errorf("Expected the decorator to return the inner http.ResponseWriter object")
	}
}

func isStackTraceLoggedByRuntime(message string) bool {
	// Check the captured output for the following patterns to find out if the
	// stack trace is included in the log:
	// - 'Observed a panic' (apimachinery runtime.go logs panic with this message)
	// - 'goroutine 44 [running]:' (stack trace always starts with this)
	if strings.Contains(message, "Observed a panic") &&
		strings.Contains(message, "goroutine") &&
		strings.Contains(message, "[running]:") {
		return true
	}

	return false
}

var tsCrt = []byte(`-----BEGIN CERTIFICATE-----
MIIDTjCCAjagAwIBAgIJAJdcQEBN2CjoMA0GCSqGSIb3DQEBCwUAMFAxCzAJBgNV
BAYTAlBMMQ8wDQYDVQQIDAZQb2xhbmQxDzANBgNVBAcMBkdkYW5zazELMAkGA1UE
CgwCU0sxEjAQBgNVBAMMCWxvY2FsaG9zdDAeFw0yMDA5MjgxMTU1MjhaFw0zMDA5
MjYxMTU1MjhaMFAxCzAJBgNVBAYTAlBMMQ8wDQYDVQQIDAZQb2xhbmQxDzANBgNV
BAcMBkdkYW5zazELMAkGA1UECgwCU0sxEjAQBgNVBAMMCWxvY2FsaG9zdDCCASIw
DQYJKoZIhvcNAQEBBQADggEPADCCAQoCggEBAMr6b/uTHkIDEd88x3t3jnroOVwh
jWMwZ6qXN2NV/If1L9FNvtoZzZi6yCDE1uLdD1kWZ0R2XOPEwUPn+Z8A/lg9kF8J
GloLCF8q+XeYp8aWRKzwtdi+MPaKFf0wsuxEEHU4pypFrszNY0yLRbWAbMtgBFy0
KhyNGahFO9V69cRHUj6EJ9kSBg0nG5bsypon2rinzKpUrzAEl2MbM3F34Zit5yOv
rYQcbDME+9XmOJPD97XBvMZCbmPnmpst3tX7ZhdKgSKtIjoYt+d//wtPMXOhrRzM
xcc6HuIHAovtB4kvZl5wvVU8ra8DKZviYyjfW36kQHo+yFwP3XXZFWezZi0CAwEA
AaMrMCkwCQYDVR0TBAIwADALBgNVHQ8EBAMCBaAwDwYDVR0RBAgwBocEfwAAATAN
BgkqhkiG9w0BAQsFAAOCAQEAMoAlhZ6yRwPmFL2ql9ZYWqaPu2NC4dXYV6kUIXUA
pG3IqFWb3L4ePkgYBMDlJGpAJcXlSSgEGiLj+3qQojawIMzzxmqr2KX888S5Mr+9
I1qseRwcftwYqV/ewJSWE90HJ21pb1ixA6bSRJLV7DyxO6zKsdVJ4xIvehZtGbux
0RTf+8zUx8z2Goy1GUztOIqfMRt1P1hlQG0uvYsGQM84HO4+YhFwejrGaj8ajpgF
uo3B8BVHeh57FNGE6C45NkFGHq3tkNLMdAa32Az8DDvPmsJuycf6vgIfBEQxLZSF
OUKrKmtfdFv4XrInqFUYBYp5GkL8SGM2wmv6aSw9Aju4lA==
-----END CERTIFICATE-----`)

var tsKey = []byte(`-----BEGIN PRIVATE KEY-----
MIIEvgIBADANBgkqhkiG9w0BAQEFAASCBKgwggSkAgEAAoIBAQDK+m/7kx5CAxHf
PMd7d4566DlcIY1jMGeqlzdjVfyH9S/RTb7aGc2YusggxNbi3Q9ZFmdEdlzjxMFD
5/mfAP5YPZBfCRpaCwhfKvl3mKfGlkSs8LXYvjD2ihX9MLLsRBB1OKcqRa7MzWNM
i0W1gGzLYARctCocjRmoRTvVevXER1I+hCfZEgYNJxuW7MqaJ9q4p8yqVK8wBJdj
GzNxd+GYrecjr62EHGwzBPvV5jiTw/e1wbzGQm5j55qbLd7V+2YXSoEirSI6GLfn
f/8LTzFzoa0czMXHOh7iBwKL7QeJL2ZecL1VPK2vAymb4mMo31t+pEB6PshcD911
2RVns2YtAgMBAAECggEAA2Qx0MtBeyrf9pHmZ1q1B7qvkqmA2kJpyQDjzQYXxRHE
rcOVx8EcnUupolqHmJzG798e9JbhsHCOJhtPIWf71++XZO8bAJwklKp8JpJnYzsJ
hLY0450x5jyiZ2uT4by1Za//owYtCID6AsJk9MZjivZcvEvKVFXLMvONL2DxkEj1
KaGQJh6/GT4jtNX07bW9+5w069KAAf+BNuqv8+Y/FseV3ovlpLTKjMV9xCCp9i62
PJs/hs5CW2X+JCE7OCLsAiu0JTpXYyHcLwYwnCONdvj6veiMWjRyNDr3ew5NeZNf
nGU4WX7mXjPd/1OvzJy6iyrBlAA63ZfFZYjWQnfsIQKBgQDmo3AMIr+9rE79NnaD
woQyO539YSO45KSM39/Xrp/NJVpOxtzgZrYo7O6f6kQ3S5zQOddy9Oj7gN3RXhZ7
Vi+Oja78ig7KUrqxcBiBGRsKZGm5CGdZ0EFd3rIEh4Qb+f+2c4f+6NWANb4kwvfq
K24c1o71+77lEVlzE2/L33K+mQKBgQDhTFr/f2e9gnRNX9bjF4p7DQI0RsFADjx0
jgJYHfm/lCIdH9vf6SmmvJv2E76Bqx9XVilhav/egqKO/wzJWHyNo2RFBXNqfwoF
UxRZKgqhcU52y2LKAYoTYfodktatZk74rinMDLmA6arnlAWQELk3Mx48DlND43Zc
DUHTKcJEtQKBgQDYdL1c9mPjnEqJxMqXwEAXcPJG8hr3lMaGXDoVjxL1EsBdvK9h
f6QoZq1RsiiRiMpEdnSotAfQutHzhA0vdeSuMnTvGJbm9Zu3mc+1oZ1KNJEwkh2F
Ijmm4rFKJPEs3IVMc8NHzrdJW6b3k2/e+yGduRR08e7nx0+e+7fpq+1hyQKBgHY9
l4h9+hkYjSdKhEG8yh3Ybu62r5eJoSremNZcLQXhnaHBZaj2+rgaRpP4OsRc5d71
RlRtTood72iy7KgDO6MuPGKJANDEiaLPvl8pVFj0WWS5S0iPVELl6dl5hheNGSck
aKVBjF3exKYzJlQ8oqgYuOZ18jcv+p9HCePkB6P9AoGBAJSYpkNDc/lnCpfIlxVw
n+VroX6QDIMZzC7BGiUSrmVsu6xEbI+8/C7ecN2oCZZLMj96EXe6j+np4zmkQezc
c1EwB7fNAiS0fWyE2RU6QAOZJ71bDpzQa4q4DxbOkYSybGPM/nqDRwovdjUnWeuM
+vrJUjAZAPHJcvos0iylnc8E
-----END PRIVATE KEY-----`)
