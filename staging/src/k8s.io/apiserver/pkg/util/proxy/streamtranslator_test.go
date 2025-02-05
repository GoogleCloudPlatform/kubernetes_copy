/*
Copyright 2023 The Kubernetes Authors.

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

package proxy

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	mrand "math/rand"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/httpstream"
	"k8s.io/apimachinery/pkg/util/httpstream/spdy"
	rcconstants "k8s.io/apimachinery/pkg/util/remotecommand"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apiserver/pkg/util/proxy/metrics"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	"k8s.io/client-go/transport"
	"k8s.io/component-base/metrics/legacyregistry"
	"k8s.io/component-base/metrics/testutil"
)

// TestStreamTranslator_LoopbackStdinToStdout returns random data sent on the client's
// STDIN channel back onto the client's STDOUT channel. There are two servers in this test: the
// upstream fake SPDY server, and the StreamTranslator server. The StreamTranslator proxys the
// data received from the websocket client upstream to the SPDY server (by translating the
// websocket data into spdy). The returned data read on the websocket client STDOUT is then
// compared the random data sent on STDIN to ensure they are the same.
func TestStreamTranslator_LoopbackStdinToStdout(t *testing.T) {
	metrics.Register()
	metrics.ResetForTest()
	t.Cleanup(metrics.ResetForTest)
	// Create upstream fake SPDY server which copies STDIN back onto STDOUT stream.
	spdyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		ctx, err := createSPDYServerStreams(w, req, Options{
			Stdin:  true,
			Stdout: true,
		})
		if err != nil {
			t.Errorf("error on createHTTPStreams: %v", err)
			return
		}
		defer ctx.conn.Close()
		// Loopback STDIN data onto STDOUT stream.
		_, err = io.Copy(ctx.stdoutStream, ctx.stdinStream)
		if err != nil {
			t.Fatalf("error copying STDIN to STDOUT: %v", err)
		}

	}))
	defer spdyServer.Close()
	// Create StreamTranslatorHandler, which points upstream to fake SPDY server with
	// streams STDIN and STDOUT. Create test server from StreamTranslatorHandler.
	spdyLocation, err := url.Parse(spdyServer.URL)
	if err != nil {
		t.Fatalf("Unable to parse spdy server URL: %s", spdyServer.URL)
	}
	spdyTransport, err := fakeTransport()
	if err != nil {
		t.Fatalf("Unexpected error creating transport: %v", err)
	}
	streams := Options{Stdin: true, Stdout: true}
	streamTranslator := NewStreamTranslatorHandler(spdyLocation, spdyTransport, 0, streams)
	streamTranslatorServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		streamTranslator.ServeHTTP(w, req)
	}))
	defer streamTranslatorServer.Close()
	// Now create the websocket client (executor), and point it to the "streamTranslatorServer".
	streamTranslatorLocation, err := url.Parse(streamTranslatorServer.URL)
	if err != nil {
		t.Fatalf("Unable to parse StreamTranslator server URL: %s", streamTranslatorServer.URL)
	}
	exec, err := remotecommand.NewWebSocketExecutor(&rest.Config{Host: streamTranslatorLocation.Host}, "GET", streamTranslatorServer.URL)
	if err != nil {
		t.Errorf("unexpected error creating websocket executor: %v", err)
	}
	// Generate random data, and set it up to stream on STDIN. The data will be
	// returned on the STDOUT buffer.
	randomSize := 1024 * 1024
	randomData := make([]byte, randomSize)
	if _, err := rand.Read(randomData); err != nil {
		t.Errorf("unexpected error reading random data: %v", err)
	}
	var stdout bytes.Buffer
	options := &remotecommand.StreamOptions{
		Stdin:  bytes.NewReader(randomData),
		Stdout: &stdout,
	}
	errorChan := make(chan error)
	go func() {
		// Start the streaming on the WebSocket "exec" client.
		errorChan <- exec.StreamWithContext(context.Background(), *options)
	}()

	select {
	case <-time.After(wait.ForeverTestTimeout):
		t.Fatalf("expect stream to be closed after connection is closed.")
	case err := <-errorChan:
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	}
	data, err := io.ReadAll(bytes.NewReader(stdout.Bytes()))
	if err != nil {
		t.Errorf("error reading the stream: %v", err)
		return
	}
	// Check the random data sent on STDIN was the same returned on STDOUT.
	if !bytes.Equal(randomData, data) {
		t.Errorf("unexpected data received: %d sent: %d", len(data), len(randomData))
	}
	// Validate the streamtranslator metrics; should be one 200 success.
	metricNames := []string{"apiserver_stream_translator_requests_total"}
	expected := `
# HELP apiserver_stream_translator_requests_total [ALPHA] Total number of requests that were handled by the StreamTranslatorProxy, which processes streaming RemoteCommand/V5
# TYPE apiserver_stream_translator_requests_total counter
apiserver_stream_translator_requests_total{code="200"} 1
`
	if err := testutil.GatherAndCompare(legacyregistry.DefaultGatherer, strings.NewReader(expected), metricNames...); err != nil {
		t.Fatal(err)
	}
}

// TestStreamTranslator_LoopbackStdinToStderr returns random data sent on the client's
// STDIN channel back onto the client's STDERR channel. There are two servers in this test: the
// upstream fake SPDY server, and the StreamTranslator server. The StreamTranslator proxys the
// data received from the websocket client upstream to the SPDY server (by translating the
// websocket data into spdy). The returned data read on the websocket client STDERR is then
// compared the random data sent on STDIN to ensure they are the same.
func TestStreamTranslator_LoopbackStdinToStderr(t *testing.T) {
	metrics.Register()
	metrics.ResetForTest()
	t.Cleanup(metrics.ResetForTest)
	// Create upstream fake SPDY server which copies STDIN back onto STDERR stream.
	spdyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		ctx, err := createSPDYServerStreams(w, req, Options{
			Stdin:  true,
			Stderr: true,
		})
		if err != nil {
			t.Errorf("error on createHTTPStreams: %v", err)
			return
		}
		defer ctx.conn.Close()
		// Loopback STDIN data onto STDERR stream.
		_, err = io.Copy(ctx.stderrStream, ctx.stdinStream)
		if err != nil {
			t.Fatalf("error copying STDIN to STDERR: %v", err)
		}
	}))
	defer spdyServer.Close()
	// Create StreamTranslatorHandler, which points upstream to fake SPDY server with
	// streams STDIN and STDERR. Create test server from StreamTranslatorHandler.
	spdyLocation, err := url.Parse(spdyServer.URL)
	if err != nil {
		t.Fatalf("Unable to parse spdy server URL: %s", spdyServer.URL)
	}
	spdyTransport, err := fakeTransport()
	if err != nil {
		t.Fatalf("Unexpected error creating transport: %v", err)
	}
	streams := Options{Stdin: true, Stderr: true}
	streamTranslator := NewStreamTranslatorHandler(spdyLocation, spdyTransport, 0, streams)
	streamTranslatorServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		streamTranslator.ServeHTTP(w, req)
	}))
	defer streamTranslatorServer.Close()
	// Now create the websocket client (executor), and point it to the "streamTranslatorServer".
	streamTranslatorLocation, err := url.Parse(streamTranslatorServer.URL)
	if err != nil {
		t.Fatalf("Unable to parse StreamTranslator server URL: %s", streamTranslatorServer.URL)
	}
	exec, err := remotecommand.NewWebSocketExecutor(&rest.Config{Host: streamTranslatorLocation.Host}, "GET", streamTranslatorServer.URL)
	if err != nil {
		t.Errorf("unexpected error creating websocket executor: %v", err)
	}
	// Generate random data, and set it up to stream on STDIN. The data will be
	// returned on the STDERR buffer.
	randomSize := 1024 * 1024
	randomData := make([]byte, randomSize)
	if _, err := rand.Read(randomData); err != nil {
		t.Errorf("unexpected error reading random data: %v", err)
	}
	var stderr bytes.Buffer
	options := &remotecommand.StreamOptions{
		Stdin:  bytes.NewReader(randomData),
		Stderr: &stderr,
	}
	errorChan := make(chan error)
	go func() {
		// Start the streaming on the WebSocket "exec" client.
		errorChan <- exec.StreamWithContext(context.Background(), *options)
	}()

	select {
	case <-time.After(wait.ForeverTestTimeout):
		t.Fatalf("expect stream to be closed after connection is closed.")
	case err := <-errorChan:
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	}
	data, err := io.ReadAll(bytes.NewReader(stderr.Bytes()))
	if err != nil {
		t.Errorf("error reading the stream: %v", err)
		return
	}
	// Check the random data sent on STDIN was the same returned on STDERR.
	if !bytes.Equal(randomData, data) {
		t.Errorf("unexpected data received: %d sent: %d", len(data), len(randomData))
	}
	// Validate the streamtranslator metrics; should be one 200 success.
	metricNames := []string{"apiserver_stream_translator_requests_total"}
	expected := `
# HELP apiserver_stream_translator_requests_total [ALPHA] Total number of requests that were handled by the StreamTranslatorProxy, which processes streaming RemoteCommand/V5
# TYPE apiserver_stream_translator_requests_total counter
apiserver_stream_translator_requests_total{code="200"} 1
`
	if err := testutil.GatherAndCompare(legacyregistry.DefaultGatherer, strings.NewReader(expected), metricNames...); err != nil {
		t.Fatal(err)
	}
}

// Returns a random exit code in the range(1-127).
func randomExitCode() int {
	errorCode := mrand.Intn(127) // Range: (0 - 126)
	errorCode += 1               // Range: (1 - 127)
	return errorCode
}

// TestStreamTranslator_ErrorStream tests the error stream by sending an error with a random
// exit code, then validating the error arrives on the error stream.
func TestStreamTranslator_ErrorStream(t *testing.T) {
	metrics.Register()
	metrics.ResetForTest()
	t.Cleanup(metrics.ResetForTest)
	expectedExitCode := randomExitCode()
	// Create upstream fake SPDY server, returning a non-zero exit code
	// on error stream within the structured error.
	spdyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		ctx, err := createSPDYServerStreams(w, req, Options{
			Stdout: true,
		})
		if err != nil {
			t.Errorf("error on createHTTPStreams: %v", err)
			return
		}
		defer ctx.conn.Close()
		// Read/discard STDIN data before returning error on error stream.
		_, err = io.Copy(io.Discard, ctx.stdinStream)
		if err != nil {
			t.Fatalf("error copying STDIN to DISCARD: %v", err)
		}
		// Force an non-zero exit code error returned on the error stream.
		err = ctx.writeStatus(&apierrors.StatusError{ErrStatus: metav1.Status{
			Status: metav1.StatusFailure,
			Reason: rcconstants.NonZeroExitCodeReason,
			Details: &metav1.StatusDetails{
				Causes: []metav1.StatusCause{
					{
						Type:    rcconstants.ExitCodeCauseType,
						Message: fmt.Sprintf("%d", expectedExitCode),
					},
				},
			},
		}})
		if err != nil {
			t.Fatalf("error writing status: %v", err)
		}
	}))
	defer spdyServer.Close()
	// Create StreamTranslatorHandler, which points upstream to fake SPDY server, and
	// create a test server using the  StreamTranslatorHandler.
	spdyLocation, err := url.Parse(spdyServer.URL)
	if err != nil {
		t.Fatalf("Unable to parse spdy server URL: %s", spdyServer.URL)
	}
	spdyTransport, err := fakeTransport()
	if err != nil {
		t.Fatalf("Unexpected error creating transport: %v", err)
	}
	streams := Options{Stdin: true}
	streamTranslator := NewStreamTranslatorHandler(spdyLocation, spdyTransport, 0, streams)
	streamTranslatorServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		streamTranslator.ServeHTTP(w, req)
	}))
	defer streamTranslatorServer.Close()
	// Now create the websocket client (executor), and point it to the "streamTranslatorServer".
	streamTranslatorLocation, err := url.Parse(streamTranslatorServer.URL)
	if err != nil {
		t.Fatalf("Unable to parse StreamTranslator server URL: %s", streamTranslatorServer.URL)
	}
	exec, err := remotecommand.NewWebSocketExecutor(&rest.Config{Host: streamTranslatorLocation.Host}, "GET", streamTranslatorServer.URL)
	if err != nil {
		t.Errorf("unexpected error creating websocket executor: %v", err)
	}
	// Generate random data, and set it up to stream on STDIN. The data will be discarded at
	// upstream SDPY server.
	randomSize := 1024 * 1024
	randomData := make([]byte, randomSize)
	if _, err := rand.Read(randomData); err != nil {
		t.Errorf("unexpected error reading random data: %v", err)
	}
	options := &remotecommand.StreamOptions{
		Stdin: bytes.NewReader(randomData),
	}
	errorChan := make(chan error)
	go func() {
		// Start the streaming on the WebSocket "exec" client.
		errorChan <- exec.StreamWithContext(context.Background(), *options)
	}()

	select {
	case <-time.After(wait.ForeverTestTimeout):
		t.Fatalf("expect stream to be closed after connection is closed.")
	case err := <-errorChan:
		// Expect exit code error on error stream.
		if err == nil {
			t.Errorf("expected error, but received none")
		}
		expectedError := fmt.Sprintf("command terminated with exit code %d", expectedExitCode)
		// Compare expected error with exit code to actual error.
		if expectedError != err.Error() {
			t.Errorf("expected error (%s), got (%s)", expectedError, err)
		}
	}
	// Validate the streamtranslator metrics; an exit code error is considered 200 success.
	metricNames := []string{"apiserver_stream_translator_requests_total"}
	expected := `
# HELP apiserver_stream_translator_requests_total [ALPHA] Total number of requests that were handled by the StreamTranslatorProxy, which processes streaming RemoteCommand/V5
# TYPE apiserver_stream_translator_requests_total counter
apiserver_stream_translator_requests_total{code="200"} 1
`
	if err := testutil.GatherAndCompare(legacyregistry.DefaultGatherer, strings.NewReader(expected), metricNames...); err != nil {
		t.Fatal(err)
	}
}

// TestStreamTranslator_MultipleReadChannels tests two streams (STDOUT, STDERR) reading from
// the connections at the same time.
func TestStreamTranslator_MultipleReadChannels(t *testing.T) {
	metrics.Register()
	metrics.ResetForTest()
	t.Cleanup(metrics.ResetForTest)
	// Create upstream fake SPDY server which copies STDIN back onto STDOUT and STDERR stream.
	spdyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		ctx, err := createSPDYServerStreams(w, req, Options{
			Stdin:  true,
			Stdout: true,
			Stderr: true,
		})
		if err != nil {
			t.Errorf("error on createHTTPStreams: %v", err)
			return
		}
		defer ctx.conn.Close()
		// TeeReader copies data read on STDIN onto STDERR.
		stdinReader := io.TeeReader(ctx.stdinStream, ctx.stderrStream)
		// Also copy STDIN to STDOUT.
		_, err = io.Copy(ctx.stdoutStream, stdinReader)
		if err != nil {
			t.Errorf("error copying STDIN to STDOUT: %v", err)
		}
	}))
	defer spdyServer.Close()
	// Create StreamTranslatorHandler, which points upstream to fake SPDY server with
	// streams STDIN, STDOUT, and STDERR. Create test server from StreamTranslatorHandler.
	spdyLocation, err := url.Parse(spdyServer.URL)
	if err != nil {
		t.Fatalf("Unable to parse spdy server URL: %s", spdyServer.URL)
	}
	spdyTransport, err := fakeTransport()
	if err != nil {
		t.Fatalf("Unexpected error creating transport: %v", err)
	}
	streams := Options{Stdin: true, Stdout: true, Stderr: true}
	streamTranslator := NewStreamTranslatorHandler(spdyLocation, spdyTransport, 0, streams)
	streamTranslatorServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		streamTranslator.ServeHTTP(w, req)
	}))
	defer streamTranslatorServer.Close()
	// Now create the websocket client (executor), and point it to the "streamTranslatorServer".
	streamTranslatorLocation, err := url.Parse(streamTranslatorServer.URL)
	if err != nil {
		t.Fatalf("Unable to parse StreamTranslator server URL: %s", streamTranslatorServer.URL)
	}
	exec, err := remotecommand.NewWebSocketExecutor(&rest.Config{Host: streamTranslatorLocation.Host}, "GET", streamTranslatorServer.URL)
	if err != nil {
		t.Errorf("unexpected error creating websocket executor: %v", err)
	}
	// Generate random data, and set it up to stream on STDIN. The data will be
	// returned on the STDOUT and STDERR buffer.
	randomSize := 1024 * 1024
	randomData := make([]byte, randomSize)
	if _, err := rand.Read(randomData); err != nil {
		t.Errorf("unexpected error reading random data: %v", err)
	}
	var stdout, stderr bytes.Buffer
	options := &remotecommand.StreamOptions{
		Stdin:  bytes.NewReader(randomData),
		Stdout: &stdout,
		Stderr: &stderr,
	}
	errorChan := make(chan error)
	go func() {
		// Start the streaming on the WebSocket "exec" client.
		errorChan <- exec.StreamWithContext(context.Background(), *options)
	}()

	select {
	case <-time.After(wait.ForeverTestTimeout):
		t.Fatalf("expect stream to be closed after connection is closed.")
	case err := <-errorChan:
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	}
	stdoutBytes, err := io.ReadAll(bytes.NewReader(stdout.Bytes()))
	if err != nil {
		t.Errorf("error reading the stream: %v", err)
		return
	}
	// Check the random data sent on STDIN was the same returned on STDOUT.
	if !bytes.Equal(stdoutBytes, randomData) {
		t.Errorf("unexpected data received: %d sent: %d", len(stdoutBytes), len(randomData))
	}
	stderrBytes, err := io.ReadAll(bytes.NewReader(stderr.Bytes()))
	if err != nil {
		t.Errorf("error reading the stream: %v", err)
		return
	}
	// Check the random data sent on STDIN was the same returned on STDERR.
	if !bytes.Equal(stderrBytes, randomData) {
		t.Errorf("unexpected data received: %d sent: %d", len(stderrBytes), len(randomData))
	}
	// Validate the streamtranslator metrics; should have one 200 success.
	metricNames := []string{"apiserver_stream_translator_requests_total"}
	expected := `
# HELP apiserver_stream_translator_requests_total [ALPHA] Total number of requests that were handled by the StreamTranslatorProxy, which processes streaming RemoteCommand/V5
# TYPE apiserver_stream_translator_requests_total counter
apiserver_stream_translator_requests_total{code="200"} 1
`
	if err := testutil.GatherAndCompare(legacyregistry.DefaultGatherer, strings.NewReader(expected), metricNames...); err != nil {
		t.Fatal(err)
	}
}

// TestStreamTranslator_ThrottleReadChannels tests two streams (STDOUT, STDERR) using rate limited streams.
func TestStreamTranslator_ThrottleReadChannels(t *testing.T) {
	// Create upstream fake SPDY server which copies STDIN back onto STDOUT and STDERR stream.
	spdyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		ctx, err := createSPDYServerStreams(w, req, Options{
			Stdin:  true,
			Stdout: true,
			Stderr: true,
		})
		if err != nil {
			t.Errorf("error on createHTTPStreams: %v", err)
			return
		}
		defer ctx.conn.Close()
		// TeeReader copies data read on STDIN onto STDERR.
		stdinReader := io.TeeReader(ctx.stdinStream, ctx.stderrStream)
		// Also copy STDIN to STDOUT.
		_, err = io.Copy(ctx.stdoutStream, stdinReader)
		if err != nil {
			t.Errorf("error copying STDIN to STDOUT: %v", err)
		}
	}))
	defer spdyServer.Close()
	// Create StreamTranslatorHandler, which points upstream to fake SPDY server with
	// streams STDIN, STDOUT, and STDERR. Create test server from StreamTranslatorHandler.
	spdyLocation, err := url.Parse(spdyServer.URL)
	if err != nil {
		t.Fatalf("Unable to parse spdy server URL: %s", spdyServer.URL)
	}
	spdyTransport, err := fakeTransport()
	if err != nil {
		t.Fatalf("Unexpected error creating transport: %v", err)
	}
	streams := Options{Stdin: true, Stdout: true, Stderr: true}
	maxBytesPerSec := 900 * 1024 // slightly less than the 1MB that is being transferred to exercise throttling.
	streamTranslator := NewStreamTranslatorHandler(spdyLocation, spdyTransport, int64(maxBytesPerSec), streams)
	streamTranslatorServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		streamTranslator.ServeHTTP(w, req)
	}))
	defer streamTranslatorServer.Close()
	// Now create the websocket client (executor), and point it to the "streamTranslatorServer".
	streamTranslatorLocation, err := url.Parse(streamTranslatorServer.URL)
	if err != nil {
		t.Fatalf("Unable to parse StreamTranslator server URL: %s", streamTranslatorServer.URL)
	}
	exec, err := remotecommand.NewWebSocketExecutor(&rest.Config{Host: streamTranslatorLocation.Host}, "GET", streamTranslatorServer.URL)
	if err != nil {
		t.Errorf("unexpected error creating websocket executor: %v", err)
	}
	// Generate random data, and set it up to stream on STDIN. The data will be
	// returned on the STDOUT and STDERR buffer.
	randomSize := 1024 * 1024
	randomData := make([]byte, randomSize)
	if _, err := rand.Read(randomData); err != nil {
		t.Errorf("unexpected error reading random data: %v", err)
	}
	var stdout, stderr bytes.Buffer
	options := &remotecommand.StreamOptions{
		Stdin:  bytes.NewReader(randomData),
		Stdout: &stdout,
		Stderr: &stderr,
	}
	errorChan := make(chan error)
	go func() {
		// Start the streaming on the WebSocket "exec" client.
		errorChan <- exec.StreamWithContext(context.Background(), *options)
	}()

	select {
	case <-time.After(wait.ForeverTestTimeout):
		t.Fatalf("expect stream to be closed after connection is closed.")
	case err := <-errorChan:
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	}
	stdoutBytes, err := io.ReadAll(bytes.NewReader(stdout.Bytes()))
	if err != nil {
		t.Errorf("error reading the stream: %v", err)
		return
	}
	// Check the random data sent on STDIN was the same returned on STDOUT.
	if !bytes.Equal(stdoutBytes, randomData) {
		t.Errorf("unexpected data received: %d sent: %d", len(stdoutBytes), len(randomData))
	}
	stderrBytes, err := io.ReadAll(bytes.NewReader(stderr.Bytes()))
	if err != nil {
		t.Errorf("error reading the stream: %v", err)
		return
	}
	// Check the random data sent on STDIN was the same returned on STDERR.
	if !bytes.Equal(stderrBytes, randomData) {
		t.Errorf("unexpected data received: %d sent: %d", len(stderrBytes), len(randomData))
	}
}

// fakeTerminalSizeQueue implements TerminalSizeQueue, returning a random set of
// "maxSizes" number of TerminalSizes, storing the TerminalSizes in "sizes" slice.
type fakeTerminalSizeQueue struct {
	maxSizes      int
	terminalSizes []remotecommand.TerminalSize
}

// newTerminalSizeQueue returns a pointer to a fakeTerminalSizeQueue passing
// "max" number of random TerminalSizes created.
func newTerminalSizeQueue(max int) *fakeTerminalSizeQueue {
	return &fakeTerminalSizeQueue{
		maxSizes:      max,
		terminalSizes: make([]remotecommand.TerminalSize, 0, max),
	}
}

// Next returns a pointer to the next random TerminalSize, or nil if we have
// already returned "maxSizes" TerminalSizes already. Stores the randomly
// created TerminalSize in "terminalSizes" field for later validation.
func (f *fakeTerminalSizeQueue) Next() *remotecommand.TerminalSize {
	if len(f.terminalSizes) >= f.maxSizes {
		return nil
	}
	size := randomTerminalSize()
	f.terminalSizes = append(f.terminalSizes, size)
	return &size
}

// randomTerminalSize returns a TerminalSize with random values in the
// range (0-65535) for the fields Width and Height.
func randomTerminalSize() remotecommand.TerminalSize {
	randWidth := uint16(mrand.Intn(int(math.Pow(2, 16))))
	randHeight := uint16(mrand.Intn(int(math.Pow(2, 16))))
	return remotecommand.TerminalSize{
		Width:  randWidth,
		Height: randHeight,
	}
}

// TestStreamTranslator_MultipleWriteChannels
func TestStreamTranslator_TTYResizeChannel(t *testing.T) {
	metrics.Register()
	metrics.ResetForTest()
	t.Cleanup(metrics.ResetForTest)
	// Create the fake terminal size queue and the actualTerminalSizes which
	// will be received at the opposite websocket endpoint.
	numSizeQueue := 10000
	sizeQueue := newTerminalSizeQueue(numSizeQueue)
	actualTerminalSizes := make([]remotecommand.TerminalSize, 0, numSizeQueue)
	// Create upstream fake SPDY server which copies STDIN back onto STDERR stream.
	spdyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		ctx, err := createSPDYServerStreams(w, req, Options{
			Tty: true,
		})
		if err != nil {
			t.Errorf("error on createHTTPStreams: %v", err)
			return
		}
		defer ctx.conn.Close()
		// Read the terminal resize requests, storing them in actualTerminalSizes
		for i := 0; i < numSizeQueue; i++ {
			actualTerminalSize := <-ctx.resizeChan
			actualTerminalSizes = append(actualTerminalSizes, actualTerminalSize)
		}
	}))
	defer spdyServer.Close()
	// Create StreamTranslatorHandler, which points upstream to fake SPDY server with
	// resize (TTY resize) stream. Create test server from StreamTranslatorHandler.
	spdyLocation, err := url.Parse(spdyServer.URL)
	if err != nil {
		t.Fatalf("Unable to parse spdy server URL: %s", spdyServer.URL)
	}
	spdyTransport, err := fakeTransport()
	if err != nil {
		t.Fatalf("Unexpected error creating transport: %v", err)
	}
	streams := Options{Tty: true}
	streamTranslator := NewStreamTranslatorHandler(spdyLocation, spdyTransport, 0, streams)
	streamTranslatorServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		streamTranslator.ServeHTTP(w, req)
	}))
	defer streamTranslatorServer.Close()
	// Now create the websocket client (executor), and point it to the "streamTranslatorServer".
	streamTranslatorLocation, err := url.Parse(streamTranslatorServer.URL)
	if err != nil {
		t.Fatalf("Unable to parse StreamTranslator server URL: %s", streamTranslatorServer.URL)
	}
	exec, err := remotecommand.NewWebSocketExecutor(&rest.Config{Host: streamTranslatorLocation.Host}, "GET", streamTranslatorServer.URL)
	if err != nil {
		t.Errorf("unexpected error creating websocket executor: %v", err)
	}
	options := &remotecommand.StreamOptions{
		Tty:               true,
		TerminalSizeQueue: sizeQueue,
	}
	errorChan := make(chan error)
	go func() {
		// Start the streaming on the WebSocket "exec" client.
		errorChan <- exec.StreamWithContext(context.Background(), *options)
	}()

	select {
	case <-time.After(wait.ForeverTestTimeout):
		t.Fatalf("expect stream to be closed after connection is closed.")
	case err := <-errorChan:
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	}
	// Validate the random TerminalSizes sent on the resize stream are the same
	// as the actual TerminalSizes received at the websocket server.
	if len(actualTerminalSizes) != numSizeQueue {
		t.Fatalf("expected to receive num terminal resizes (%d), got (%d)",
			numSizeQueue, len(actualTerminalSizes))
	}
	for i, actual := range actualTerminalSizes {
		expected := sizeQueue.terminalSizes[i]
		if !reflect.DeepEqual(expected, actual) {
			t.Errorf("expected terminal resize window %v, got %v", expected, actual)
		}
	}
	// Validate the streamtranslator metrics; should have one 200 success.
	metricNames := []string{"apiserver_stream_translator_requests_total"}
	expected := `
# HELP apiserver_stream_translator_requests_total [ALPHA] Total number of requests that were handled by the StreamTranslatorProxy, which processes streaming RemoteCommand/V5
# TYPE apiserver_stream_translator_requests_total counter
apiserver_stream_translator_requests_total{code="200"} 1
`
	if err := testutil.GatherAndCompare(legacyregistry.DefaultGatherer, strings.NewReader(expected), metricNames...); err != nil {
		t.Fatal(err)
	}
}

// TestStreamTranslator_WebSocketServerErrors validates that when there is a problem creating
// the websocket server as the first step of the StreamTranslator an error is properly returned.
func TestStreamTranslator_WebSocketServerErrors(t *testing.T) {
	metrics.Register()
	metrics.ResetForTest()
	t.Cleanup(metrics.ResetForTest)
	spdyLocation, err := url.Parse("http://127.0.0.1")
	if err != nil {
		t.Fatalf("Unable to parse spdy server URL")
	}
	spdyTransport, err := fakeTransport()
	if err != nil {
		t.Fatalf("Unexpected error creating transport: %v", err)
	}
	streamTranslator := NewStreamTranslatorHandler(spdyLocation, spdyTransport, 0, Options{})
	streamTranslatorServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		streamTranslator.ServeHTTP(w, req)
	}))
	defer streamTranslatorServer.Close()
	// Now create the websocket client (executor), and point it to the "streamTranslatorServer".
	streamTranslatorLocation, err := url.Parse(streamTranslatorServer.URL)
	if err != nil {
		t.Fatalf("Unable to parse StreamTranslator server URL: %s", streamTranslatorServer.URL)
	}
	exec, err := remotecommand.NewWebSocketExecutorForProtocols(
		&rest.Config{Host: streamTranslatorLocation.Host},
		"GET",
		streamTranslatorServer.URL,
		rcconstants.StreamProtocolV4Name, // RemoteCommand V4 protocol is unsupported
	)
	if err != nil {
		t.Errorf("unexpected error creating websocket executor: %v", err)
	}
	errorChan := make(chan error)
	go func() {
		// Start the streaming on the WebSocket "exec" client. The WebSocket server within the
		// StreamTranslator propagates an error here because the V4 protocol is not supported.
		errorChan <- exec.StreamWithContext(context.Background(), remotecommand.StreamOptions{})
	}()

	select {
	case <-time.After(wait.ForeverTestTimeout):
		t.Fatalf("expect stream to be closed after connection is closed.")
	case err := <-errorChan:
		// Must return "websocket unable to upgrade" (bad handshake) error.
		if err == nil {
			t.Fatalf("expected error, but received none")
		}
		if !strings.Contains(err.Error(), "unable to upgrade streaming request") {
			t.Errorf("expected websocket bad handshake error, got (%s)", err)
		}
	}
	// Validate the streamtranslator metrics; should have one 500 failure.
	metricNames := []string{"apiserver_stream_translator_requests_total"}
	expected := `
# HELP apiserver_stream_translator_requests_total [ALPHA] Total number of requests that were handled by the StreamTranslatorProxy, which processes streaming RemoteCommand/V5
# TYPE apiserver_stream_translator_requests_total counter
apiserver_stream_translator_requests_total{code="400"} 1
`
	if err := testutil.GatherAndCompare(legacyregistry.DefaultGatherer, strings.NewReader(expected), metricNames...); err != nil {
		t.Fatal(err)
	}
}

// TestStreamTranslator_BlockRedirects verifies that the StreamTranslator will *not* follow
// redirects; it will thrown an error instead.
func TestStreamTranslator_BlockRedirects(t *testing.T) {
	metrics.Register()
	metrics.ResetForTest()
	t.Cleanup(metrics.ResetForTest)
	for _, statusCode := range []int{
		http.StatusMovedPermanently,  // 301
		http.StatusFound,             // 302
		http.StatusSeeOther,          // 303
		http.StatusTemporaryRedirect, // 307
		http.StatusPermanentRedirect, // 308
	} {
		// Create upstream fake SPDY server which returns a redirect.
		spdyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			w.Header().Set("Location", "/")
			w.WriteHeader(statusCode)
		}))
		defer spdyServer.Close()
		spdyLocation, err := url.Parse(spdyServer.URL)
		if err != nil {
			t.Fatalf("Unable to parse spdy server URL: %s", spdyServer.URL)
		}
		spdyTransport, err := fakeTransport()
		if err != nil {
			t.Fatalf("Unexpected error creating transport: %v", err)
		}
		streams := Options{Stdout: true}
		streamTranslator := NewStreamTranslatorHandler(spdyLocation, spdyTransport, 0, streams)
		streamTranslatorServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			streamTranslator.ServeHTTP(w, req)
		}))
		defer streamTranslatorServer.Close()
		// Now create the websocket client (executor), and point it to the "streamTranslatorServer".
		streamTranslatorLocation, err := url.Parse(streamTranslatorServer.URL)
		if err != nil {
			t.Fatalf("Unable to parse StreamTranslator server URL: %s", streamTranslatorServer.URL)
		}
		exec, err := remotecommand.NewWebSocketExecutor(&rest.Config{Host: streamTranslatorLocation.Host}, "GET", streamTranslatorServer.URL)
		if err != nil {
			t.Errorf("unexpected error creating websocket executor: %v", err)
		}
		errorChan := make(chan error)
		go func() {
			// Start the streaming on the WebSocket "exec" client.
			// Should return "redirect not allowed" error.
			errorChan <- exec.StreamWithContext(context.Background(), remotecommand.StreamOptions{})
		}()

		select {
		case <-time.After(wait.ForeverTestTimeout):
			t.Fatalf("expect stream to be closed after connection is closed.")
		case err := <-errorChan:
			// Must return "redirect now allowed" error.
			if err == nil {
				t.Fatalf("expected error, but received none")
			}
			if !strings.Contains(err.Error(), "redirect not allowed") {
				t.Errorf("expected redirect not allowed error, got (%s)", err)
			}
		}
		// Validate the streamtranslator metrics; should have one 500 failure each loop.
		metricNames := []string{"apiserver_stream_translator_requests_total"}
		expected := `
# HELP apiserver_stream_translator_requests_total [ALPHA] Total number of requests that were handled by the StreamTranslatorProxy, which processes streaming RemoteCommand/V5
# TYPE apiserver_stream_translator_requests_total counter
apiserver_stream_translator_requests_total{code="500"} 1
`
		if err := testutil.GatherAndCompare(legacyregistry.DefaultGatherer, strings.NewReader(expected), metricNames...); err != nil {
			t.Fatal(err)
		}
		metrics.ResetForTest() // Clear metrics each loop
	}
}

// streamContext encapsulates the structures necessary to communicate through
// a SPDY connection, including the Reader/Writer streams.
type streamContext struct {
	conn         io.Closer
	stdinStream  io.ReadCloser
	stdoutStream io.WriteCloser
	stderrStream io.WriteCloser
	resizeStream io.ReadCloser
	resizeChan   chan remotecommand.TerminalSize
	writeStatus  func(status *apierrors.StatusError) error
}

type streamAndReply struct {
	httpstream.Stream
	replySent <-chan struct{}
}

// CreateSPDYServerStreams upgrades the passed HTTP request to a SPDY bi-directional streaming
// connection with remote command streams defined in passed options. Returns a streamContext
// structure containing the Reader/Writer streams to communicate through the SDPY connection.
// Returns an error if unable to upgrade the HTTP connection to a SPDY connection.
func createSPDYServerStreams(w http.ResponseWriter, req *http.Request, opts Options) (*streamContext, error) {
	_, err := httpstream.Handshake(req, w, []string{rcconstants.StreamProtocolV4Name})
	if err != nil {
		return nil, err
	}

	upgrader := spdy.NewResponseUpgrader()
	streamCh := make(chan streamAndReply)
	conn := upgrader.UpgradeResponse(w, req, func(stream httpstream.Stream, replySent <-chan struct{}) error {
		streamCh <- streamAndReply{Stream: stream, replySent: replySent}
		return nil
	})
	ctx := &streamContext{
		conn: conn,
	}

	// wait for stream
	replyChan := make(chan struct{}, 5)
	defer close(replyChan)
	receivedStreams := 0
	expectedStreams := 1 // expect at least the error stream
	if opts.Stdout {
		expectedStreams++
	}
	if opts.Stdin {
		expectedStreams++
	}
	if opts.Stderr {
		expectedStreams++
	}
	if opts.Tty {
		expectedStreams++
	}
WaitForStreams:
	for {
		select {
		case stream := <-streamCh:
			streamType := stream.Headers().Get(v1.StreamType)
			switch streamType {
			case v1.StreamTypeError:
				replyChan <- struct{}{}
				ctx.writeStatus = v4WriteStatusFunc(stream)
			case v1.StreamTypeStdout:
				replyChan <- struct{}{}
				ctx.stdoutStream = stream
			case v1.StreamTypeStdin:
				replyChan <- struct{}{}
				ctx.stdinStream = stream
			case v1.StreamTypeStderr:
				replyChan <- struct{}{}
				ctx.stderrStream = stream
			case v1.StreamTypeResize:
				replyChan <- struct{}{}
				ctx.resizeStream = stream
			default:
				// add other stream ...
				return nil, errors.New("unimplemented stream type")
			}
		case <-replyChan:
			receivedStreams++
			if receivedStreams == expectedStreams {
				break WaitForStreams
			}
		}
	}

	if ctx.resizeStream != nil {
		ctx.resizeChan = make(chan remotecommand.TerminalSize)
		go handleResizeEvents(req.Context(), ctx.resizeStream, ctx.resizeChan)
	}

	return ctx, nil
}

func v4WriteStatusFunc(stream io.Writer) func(status *apierrors.StatusError) error {
	return func(status *apierrors.StatusError) error {
		bs, err := json.Marshal(status.Status())
		if err != nil {
			return err
		}
		_, err = stream.Write(bs)
		return err
	}
}

func fakeTransport() (http.RoundTripper, error) {
	cfg := &transport.Config{
		TLS: transport.TLSConfig{
			Insecure: true,
			CAFile:   "",
		},
	}
	rt, err := transport.New(cfg)
	if err != nil {
		return nil, err
	}
	return rt, nil
}
