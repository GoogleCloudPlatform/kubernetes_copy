/*
Copyright 2019 The Kubernetes Authors.

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
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	utilnet "k8s.io/apimachinery/pkg/util/net"
	"k8s.io/apimachinery/pkg/util/wait"
)

type tcpLB struct {
	t         *testing.T
	ln        net.Listener
	serverURL string
	dials     int32
}

func (lb *tcpLB) handleConnection(in net.Conn, stopCh chan struct{}) {
	out, err := net.Dial("tcp", lb.serverURL)
	if err != nil {
		lb.t.Log(err)
		return
	}
	go io.Copy(out, in)
	go io.Copy(in, out)
	<-stopCh
	if err := out.Close(); err != nil {
		lb.t.Fatalf("failed to close connection: %v", err)
	}
}

func (lb *tcpLB) serve(stopCh chan struct{}) {
	conn, err := lb.ln.Accept()
	if err != nil {
		lb.t.Fatalf("failed to accept: %v", err)
	}
	atomic.AddInt32(&lb.dials, 1)
	go lb.handleConnection(conn, stopCh)
}

func newLB(t *testing.T, serverURL string) *tcpLB {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to bind: %v", err)
	}
	lb := tcpLB{
		serverURL: serverURL,
		ln:        ln,
		t:         t,
	}
	return &lb
}

const (
	readIdleTimeout int = 1
	pingTimeout     int = 1
)

func TestReconnectBrokenTCP(t *testing.T) {
	t.Setenv("HTTP2_READ_IDLE_TIMEOUT_SECONDS", strconv.Itoa(readIdleTimeout))
	t.Setenv("HTTP2_PING_TIMEOUT_SECONDS", strconv.Itoa(pingTimeout))
	t.Setenv("DISABLE_HTTP2", "")
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "Hello, %s", r.Proto)
	}))
	ts.EnableHTTP2 = true
	ts.StartTLS()
	defer ts.Close()

	u, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatalf("failed to parse URL from %q: %v", ts.URL, err)
	}
	lb := newLB(t, u.Host)
	defer lb.ln.Close()
	stopCh := make(chan struct{})
	go lb.serve(stopCh)
	transport, ok := ts.Client().Transport.(*http.Transport)
	if !ok {
		t.Fatalf("failed to assert *http.Transport")
	}
	config := &Config{
		Host:      "https://" + lb.ln.Addr().String(),
		Transport: utilnet.SetTransportDefaults(transport),
		Timeout:   1 * time.Second,
		// These fields are required to create a REST client.
		ContentConfig: ContentConfig{
			GroupVersion:         &schema.GroupVersion{},
			NegotiatedSerializer: &serializer.CodecFactory{},
		},
	}
	client, err := RESTClientFor(config)
	if err != nil {
		t.Fatalf("failed to create REST client: %v", err)
	}
	data, err := client.Get().AbsPath("/").DoRaw(context.TODO())
	if err != nil {
		t.Fatalf("unexpected err: %s: %v", data, err)
	}
	if string(data) != "Hello, HTTP/2.0" {
		t.Fatalf("unexpected response: %s", data)
	}

	// Deliberately let the LB stop proxying traffic for the current
	// connection. This mimics a broken TCP connection that's not properly
	// closed.
	close(stopCh)

	stopCh = make(chan struct{})
	go lb.serve(stopCh)
	// Sleep enough time for the HTTP/2 health check to detect and close
	// the broken TCP connection.
	time.Sleep(time.Duration(1+readIdleTimeout+pingTimeout) * time.Second)
	// If the HTTP/2 health check were disabled, the broken connection
	// would still be in the connection pool, the following request would
	// then reuse the broken connection instead of creating a new one, and
	// thus would fail.
	data, err = client.Get().AbsPath("/").DoRaw(context.TODO())
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if string(data) != "Hello, HTTP/2.0" {
		t.Fatalf("unexpected response: %s", data)
	}
	dials := atomic.LoadInt32(&lb.dials)
	if dials != 2 {
		t.Fatalf("expected %d dials, got %d", 2, dials)
	}
}

// 1. connect to https server with http1.1 using a TCP proxy
// 2. the connection has keepalive enabled so it will be reused
// 3. break the TCP connection stopping the proxy
// 4. close the idle connection to force creating a new connection
// 5. count that there are 2 connections to the server (we didn't reuse the original connection)
func TestReconnectBrokenTCP_HTTP1(t *testing.T) {
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "Hello, %s", r.Proto)
	}))
	ts.EnableHTTP2 = false
	ts.StartTLS()
	defer ts.Close()

	u, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatalf("failed to parse URL from %q: %v", ts.URL, err)
	}
	lb := newLB(t, u.Host)
	defer lb.ln.Close()
	stopCh := make(chan struct{})
	go lb.serve(stopCh)
	transport, ok := ts.Client().Transport.(*http.Transport)
	if !ok {
		t.Fatal("failed to assert *http.Transport")
	}
	config := &Config{
		Host:      "https://" + lb.ln.Addr().String(),
		Transport: utilnet.SetTransportDefaults(transport),
		// large timeout, otherwise the broken connection will be cleaned by it
		Timeout: wait.ForeverTestTimeout,
		// These fields are required to create a REST client.
		ContentConfig: ContentConfig{
			GroupVersion:         &schema.GroupVersion{},
			NegotiatedSerializer: &serializer.CodecFactory{},
		},
	}
	config.TLSClientConfig.NextProtos = []string{"http/1.1"}
	client, err := RESTClientFor(config)
	if err != nil {
		t.Fatalf("failed to create REST client: %v", err)
	}

	data, err := client.Get().AbsPath("/").DoRaw(context.TODO())
	if err != nil {
		t.Fatalf("unexpected err: %s: %v", data, err)
	}
	if string(data) != "Hello, HTTP/1.1" {
		t.Fatalf("unexpected response: %s", data)
	}

	// Deliberately let the LB stop proxying traffic for the current
	// connection. This mimics a broken TCP connection that's not properly
	// closed.
	close(stopCh)

	stopCh = make(chan struct{})
	go lb.serve(stopCh)
	// Close the idle connections
	utilnet.CloseIdleConnectionsFor(client.Client.Transport)

	// If the client didn't close the idle connections, the broken connection
	// would still be in the connection pool, the following request would
	// then reuse the broken connection instead of creating a new one, and
	// thus would fail.
	data, err = client.Get().AbsPath("/").DoRaw(context.TODO())
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if string(data) != "Hello, HTTP/1.1" {
		t.Fatalf("unexpected response: %s", data)
	}
	dials := atomic.LoadInt32(&lb.dials)
	if dials != 2 {
		t.Fatalf("expected %d dials, got %d", 2, dials)
	}
}

// 1. connect to https server with http1.1 using a TCP proxy making the connection to timeout
// 2. the connection has keepalive enabled so it will be reused
// 3. close the in-flight connection to force creating a new connection
// 4. count that there are 2 connections on the LB but only one succeeds
func TestReconnectBrokenTCPInFlight_HTTP1(t *testing.T) {
	done := make(chan struct{})
	defer close(done)
	received := make(chan struct{})

	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/hang" {
			conn, _, _ := w.(http.Hijacker).Hijack()
			close(received)
			<-done
			conn.Close()
		}
		fmt.Fprintf(w, "Hello, %s", r.Proto)
	}))
	ts.EnableHTTP2 = false
	ts.StartTLS()
	defer ts.Close()

	u, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatalf("failed to parse URL from %q: %v", ts.URL, err)
	}

	lb := newLB(t, u.Host)
	defer lb.ln.Close()
	stopCh := make(chan struct{})
	go lb.serve(stopCh)

	transport, ok := ts.Client().Transport.(*http.Transport)
	if !ok {
		t.Fatal("failed to assert *http.Transport")
	}
	config := &Config{
		Host:      "https://" + lb.ln.Addr().String(),
		Transport: utilnet.SetTransportDefaults(transport),
		// Use something extraordinary large to not hit the timeout
		Timeout: wait.ForeverTestTimeout,
		// These fields are required to create a REST client.
		ContentConfig: ContentConfig{
			GroupVersion:         &schema.GroupVersion{},
			NegotiatedSerializer: &serializer.CodecFactory{},
		},
	}
	config.TLSClientConfig.NextProtos = []string{"http/1.1"}

	client, err := RESTClientFor(config)
	if err != nil {
		t.Fatalf("failed to create REST client: %v", err)
	}

	// The request will connect, hang and eventually time out
	// but we can use a context to close once the test is done
	// we are only interested in have an inflight connection
	ctx, cancel := context.WithCancel(context.Background())
	reqErrCh := make(chan error, 1)
	defer close(reqErrCh)
	go func() {
		_, err = client.Get().AbsPath("/hang").DoRaw(ctx)
		reqErrCh <- err
	}()

	// wait until it connect to the server
	select {
	case <-received:
	case <-time.After(wait.ForeverTestTimeout):
		t.Fatal("Test timed out waiting for first request to fail")
	}

	// Deliberately let the LB stop proxying traffic for the current
	// connection. This mimics a broken TCP connection that's not properly
	// closed.
	close(stopCh)

	stopCh = make(chan struct{})
	go lb.serve(stopCh)

	// New request will fail if tries to reuse the connection
	data, err := client.Get().AbsPath("/").DoRaw(context.Background())
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if string(data) != "Hello, HTTP/1.1" {
		t.Fatalf("unexpected response: %s", data)
	}
	dials := atomic.LoadInt32(&lb.dials)
	if dials != 2 {
		t.Fatalf("expected %d dials, got %d", 2, dials)
	}

	// cancel the in-flight connection
	cancel()
	select {
	case <-reqErrCh:
		if err == nil {
			t.Fatal("Connection succeeded but was expected to timeout")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Test timed out waiting for the request to fail")
	}

}

func TestRestClientTimeout(t *testing.T) {
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
		fmt.Fprintf(w, "Hello, %s", r.Proto)
	}))
	ts.Start()
	defer ts.Close()

	config := &Config{
		Host:    ts.URL,
		Timeout: 1 * time.Second,
		// These fields are required to create a REST client.
		ContentConfig: ContentConfig{
			GroupVersion:         &schema.GroupVersion{},
			NegotiatedSerializer: &serializer.CodecFactory{},
		},
	}
	client, err := RESTClientFor(config)
	if err != nil {
		t.Fatalf("failed to create REST client: %v", err)
	}
	_, err = client.Get().AbsPath("/").DoRaw(context.TODO())
	if err == nil {
		t.Fatalf("timeout error expected")
	}
	// Two errors are possible:
	//
	// error(*net/url.Error) *{
	//   Op: "Get",
	//   URL: "http://127.0.0.1:34051/?timeout=1s",
	//   Err: error(context.deadlineExceededError) {},
	// }
	//
	// If CancelRequest is active (see https://cs.opensource.google/go/go/+/master:src/net/http/client.go;l=358-367),
	// then we can also get:
	// error(*net/url.Error) *{
	//   Op: "Get",
	//   URL: "http://127.0.0.1:36249/?timeout=1s",
	//   Err: error(*net/http.timeoutError) *{
	//     err: "net/http: request canceled (Client.Timeout exceeded while awaiting headers)",
	// }}
	if !strings.Contains(err.Error(), "deadline exceeded") &&
		!strings.Contains(err.Error(), "request canceled") {
		t.Fatalf("timeout error expected, received %v", err)
	}
}
