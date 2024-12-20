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

package remotecommand

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"math"
	mrand "math/rand"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	gwebsocket "github.com/gorilla/websocket"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/httpstream"
	"k8s.io/apimachinery/pkg/util/httpstream/wsstream"
	"k8s.io/apimachinery/pkg/util/remotecommand"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/rest"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"k8s.io/klog/v2/ktesting"
)

// TestWebSocketClient_LoopbackStdinToStdout returns random data sent on the STDIN channel
// back down the STDOUT channel. A subsequent comparison checks if the data
// sent on the STDIN channel is the same as the data returned on the STDOUT
// channel. This test can be run many times by the "stress" tool to check
// if there is any data which would cause problems with the WebSocket streams.
func TestWebSocketClient_LoopbackStdinToStdout(t *testing.T) {
	// Create fake WebSocket server. Copy received STDIN data back onto STDOUT stream.
	websocketServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		conns, err := webSocketServerStreams(req, w, streamOptionsFromRequest(req))
		if err != nil {
			t.Fatalf("error on webSocketServerStreams: %v", err)
		}
		defer conns.conn.Close()
		// Loopback the STDIN stream onto the STDOUT stream.
		_, err = io.Copy(conns.stdoutStream, conns.stdinStream)
		if err != nil {
			t.Fatalf("error copying STDIN to STDOUT: %v", err)
		}
	}))
	defer websocketServer.Close()

	// Now create the WebSocket client (executor), and point it to the "websocketServer".
	// Must add STDIN and STDOUT query params for the WebSocket client request.
	websocketServer.URL = websocketServer.URL + "?" + "stdin=true" + "&" + "stdout=true"
	websocketLocation, err := url.Parse(websocketServer.URL)
	if err != nil {
		t.Fatalf("Unable to parse WebSocket server URL: %s", websocketServer.URL)
	}
	exec, err := NewWebSocketExecutor(&rest.Config{Host: websocketLocation.Host}, "GET", websocketServer.URL)
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
	options := &StreamOptions{
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
			t.Errorf("unexpected error")
		}
		// Validate remote command v5 protocol was negotiated.
		streamExec := exec.(*wsStreamExecutor)
		if remotecommand.StreamProtocolV5Name != streamExec.negotiated {
			t.Fatalf("expected remote command v5 protocol, got (%s)", streamExec.negotiated)
		}
	}
	data, err := io.ReadAll(bytes.NewReader(stdout.Bytes()))
	if err != nil {
		t.Fatalf("error reading the stream: %v", err)
	}
	// Check the random data sent on STDIN was the same returned on STDOUT.
	if !bytes.Equal(randomData, data) {
		t.Errorf("unexpected data received: %d sent: %d", len(data), len(randomData))
	}
}

// TestWebSocketClient_DifferentBufferSizes runs the previous loopback (STDIN -> STDOUT) test with different
// buffer sizes for reading from the opposite end of the websocket connection (in the websocket server).
func TestWebSocketClient_DifferentBufferSizes(t *testing.T) {
	// 1k, 4k, 64k, and 128k buffer sizes for reading STDIN at websocket server endpoint.
	// The standard buffer size for io.Copy is 32k.
	bufferSizes := []int{1 * 1024, 4 * 1024, 64 * 1024, 128 * 1024}
	for _, bufferSize := range bufferSizes {
		// Create fake WebSocket server. Copy received STDIN data back onto STDOUT stream.
		websocketServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			conns, err := webSocketServerStreams(req, w, streamOptionsFromRequest(req))
			if err != nil {
				t.Fatalf("error on webSocketServerStreams: %v", err)
			}
			defer conns.conn.Close()
			// Loopback the STDIN stream onto the STDOUT stream, using buffer with size.
			buffer := make([]byte, bufferSize)
			_, err = io.CopyBuffer(conns.stdoutStream, conns.stdinStream, buffer)
			if err != nil {
				t.Fatalf("error copying STDIN to STDOUT: %v", err)
			}
		}))
		defer websocketServer.Close()

		// Now create the WebSocket client (executor), and point it to the "websocketServer".
		// Must add STDIN and STDOUT query params for the WebSocket client request.
		websocketServer.URL = websocketServer.URL + "?" + "stdin=true" + "&" + "stdout=true"
		websocketLocation, err := url.Parse(websocketServer.URL)
		if err != nil {
			t.Fatalf("Unable to parse WebSocket server URL: %s", websocketServer.URL)
		}
		exec, err := NewWebSocketExecutor(&rest.Config{Host: websocketLocation.Host}, "GET", websocketServer.URL)
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
		options := &StreamOptions{
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
				t.Errorf("unexpected error")
			}
			// Validate remote command v5 protocol was negotiated.
			streamExec := exec.(*wsStreamExecutor)
			if remotecommand.StreamProtocolV5Name != streamExec.negotiated {
				t.Fatalf("expected remote command v5 protocol, got (%s)", streamExec.negotiated)
			}
		}
		data, err := io.ReadAll(bytes.NewReader(stdout.Bytes()))
		if err != nil {
			t.Errorf("error reading the stream: %v", err)
			return
		}
		// Check all the random data sent on STDIN was the same returned on STDOUT.
		if !bytes.Equal(randomData, data) {
			t.Errorf("unexpected data received: %d sent: %d", len(data), len(randomData))
		}
	}
}

// TestWebSocketClient_LoopbackStdinAsPipe uses a pipe to send random data on the STDIN
// channel, then closes the pipe. The fake server simply returns all STDIN data back
// onto the STDOUT channel, and the received data on STDOUT is validated against the
// random data initially sent.
func TestWebSocketClient_LoopbackStdinAsPipe(t *testing.T) {
	// Create fake WebSocket server. Copy received STDIN data back onto STDOUT stream.
	websocketServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		conns, err := webSocketServerStreams(req, w, streamOptionsFromRequest(req))
		if err != nil {
			t.Fatalf("error on webSocketServerStreams: %v", err)
		}
		defer conns.conn.Close()
		// Loopback the STDIN stream onto the STDOUT stream.
		_, err = io.Copy(conns.stdoutStream, conns.stdinStream)
		if err != nil {
			t.Fatalf("error copying STDIN to STDOUT: %v", err)
		}
	}))
	defer websocketServer.Close()

	// Now create the WebSocket client (executor), and point it to the "websocketServer".
	// Must add STDIN and STDOUT query params for the WebSocket client request.
	websocketServer.URL = websocketServer.URL + "?" + "stdin=true" + "&" + "stdout=true"
	websocketLocation, err := url.Parse(websocketServer.URL)
	if err != nil {
		t.Fatalf("Unable to parse WebSocket server URL: %s", websocketServer.URL)
	}
	exec, err := NewWebSocketExecutor(&rest.Config{Host: websocketLocation.Host}, "GET", websocketServer.URL)
	if err != nil {
		t.Errorf("unexpected error creating websocket executor: %v", err)
	}
	// Generate random data, and it will be written on the STDIN pipe. The same
	// data will be returned on the STDOUT channel.
	randomSize := 1024 * 1024
	randomData := make([]byte, randomSize)
	if _, err := rand.Read(randomData); err != nil {
		t.Errorf("unexpected error reading random data: %v", err)
	}
	reader, writer := io.Pipe()
	var stdout bytes.Buffer
	options := &StreamOptions{
		Stdin:  reader,
		Stdout: &stdout,
	}
	errorChan := make(chan error)
	go func() {
		// Start the streaming on the WebSocket "exec" client.
		errorChan <- exec.StreamWithContext(context.Background(), *options)
	}()
	// Write the random data onto the pipe connected to STDIN, then close the pipe.
	_, err = writer.Write(randomData)
	if err != nil {
		t.Fatalf("unable to write random data to STDIN pipe: %v", err)
	}
	writer.Close()

	select {
	case <-time.After(wait.ForeverTestTimeout):
		t.Fatalf("expect stream to be closed after connection is closed.")
	case err := <-errorChan:
		if err != nil {
			t.Errorf("unexpected error")
		}
		// Validate remote command v5 protocol was negotiated.
		streamExec := exec.(*wsStreamExecutor)
		if remotecommand.StreamProtocolV5Name != streamExec.negotiated {
			t.Fatalf("expected remote command v5 protocol, got (%s)", streamExec.negotiated)
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
}

// TestWebSocketClient_LoopbackStdinToStderr returns random data sent on the STDIN channel
// back down the STDERR channel. A subsequent comparison checks if the data
// sent on the STDIN channel is the same as the data returned on the STDERR
// channel. This test can be run many times by the "stress" tool to check
// if there is any data which would cause problems with the WebSocket streams.
func TestWebSocketClient_LoopbackStdinToStderr(t *testing.T) {
	// Create fake WebSocket server. Copy received STDIN data back onto STDERR stream.
	websocketServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		conns, err := webSocketServerStreams(req, w, streamOptionsFromRequest(req))
		if err != nil {
			t.Fatalf("error on webSocketServerStreams: %v", err)
		}
		defer conns.conn.Close()
		// Loopback the STDIN stream onto the STDERR stream.
		_, err = io.Copy(conns.stderrStream, conns.stdinStream)
		if err != nil {
			t.Fatalf("error copying STDIN to STDERR: %v", err)
		}
	}))
	defer websocketServer.Close()

	// Now create the WebSocket client (executor), and point it to the "websocketServer".
	// Must add STDIN and STDERR query params for the WebSocket client request.
	websocketServer.URL = websocketServer.URL + "?" + "stdin=true" + "&" + "stderr=true"
	websocketLocation, err := url.Parse(websocketServer.URL)
	if err != nil {
		t.Fatalf("Unable to parse WebSocket server URL: %s", websocketServer.URL)
	}
	exec, err := NewWebSocketExecutor(&rest.Config{Host: websocketLocation.Host}, "GET", websocketServer.URL)
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
	options := &StreamOptions{
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
			t.Errorf("unexpected error")
		}
		// Validate remote command v5 protocol was negotiated.
		streamExec := exec.(*wsStreamExecutor)
		if remotecommand.StreamProtocolV5Name != streamExec.negotiated {
			t.Fatalf("expected remote command v5 protocol, got (%s)", streamExec.negotiated)
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
}

// TestWebSocketClient_MultipleReadChannels tests two streams (STDOUT, STDERR) reading from
// the websocket connection at the same time.
func TestWebSocketClient_MultipleReadChannels(t *testing.T) {
	// Create fake WebSocket server, which uses a TeeReader to copy the same data
	// onto the STDOUT stream onto the STDERR stream as well.
	websocketServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		conns, err := webSocketServerStreams(req, w, streamOptionsFromRequest(req))
		if err != nil {
			t.Fatalf("error on webSocketServerStreams: %v", err)
		}
		defer conns.conn.Close()
		// TeeReader copies data read on STDIN onto STDERR.
		stdinReader := io.TeeReader(conns.stdinStream, conns.stderrStream)
		// Also copy STDIN to STDOUT.
		_, err = io.Copy(conns.stdoutStream, stdinReader)
		if err != nil {
			t.Errorf("error copying STDIN to STDOUT: %v", err)
		}
	}))
	defer websocketServer.Close()
	// Now create the WebSocket client (executor), and point it to the "websocketServer".
	// Must add stdin, stdout, and stderr query param for the WebSocket client request.
	websocketServer.URL = websocketServer.URL + "?" + "stdin=true" + "&" + "stdout=true" + "&" + "stderr=true"
	websocketLocation, err := url.Parse(websocketServer.URL)
	if err != nil {
		t.Fatalf("Unable to parse WebSocket server URL: %s", websocketServer.URL)
	}
	exec, err := NewWebSocketExecutor(&rest.Config{Host: websocketLocation.Host}, "GET", websocketServer.URL)
	if err != nil {
		t.Errorf("unexpected error creating websocket executor: %v", err)
	}
	// Generate 1MB of random data, and set it up to stream on STDIN. The data will be
	// returned on the STDOUT and STDERR buffers.
	randomSize := 1024 * 1024
	randomData := make([]byte, randomSize)
	if _, err := rand.Read(randomData); err != nil {
		t.Errorf("unexpected error reading random data: %v", err)
	}
	var stdout, stderr bytes.Buffer
	options := &StreamOptions{
		Stdin:  bytes.NewReader(randomData),
		Stdout: &stdout,
		Stderr: &stderr,
	}
	errorChan := make(chan error)
	go func() {
		errorChan <- exec.StreamWithContext(context.Background(), *options)
	}()

	select {
	case <-time.After(wait.ForeverTestTimeout):
		t.Fatalf("expect stream to be closed after connection is closed.")
	case err := <-errorChan:
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
		// Validate remote command v5 protocol was negotiated.
		streamExec := exec.(*wsStreamExecutor)
		if remotecommand.StreamProtocolV5Name != streamExec.negotiated {
			t.Fatalf("expected remote command v5 protocol, got (%s)", streamExec.negotiated)
		}
	}
	// Validate the data read from the STDOUT stream is the same as sent on the STDIN stream.
	stdoutBytes, err := io.ReadAll(bytes.NewReader(stdout.Bytes()))
	if err != nil {
		t.Fatalf("error reading the stream: %v", err)
	}
	if !bytes.Equal(stdoutBytes, randomData) {
		t.Errorf("unexpected data received (%d) sent (%d)", len(stdoutBytes), len(randomData))
	}
	// Validate the data read from the STDERR stream is the same as sent on the STDIN stream.
	stderrBytes, err := io.ReadAll(bytes.NewReader(stderr.Bytes()))
	if err != nil {
		t.Fatalf("error reading the stream: %v", err)
	}
	if !bytes.Equal(stderrBytes, randomData) {
		t.Errorf("unexpected data received (%d) sent (%d)", len(stderrBytes), len(randomData))
	}
}

// Returns a random exit code in the range(1-127).
func randomExitCode() int {
	errorCode := mrand.Intn(128)
	if errorCode == 0 {
		errorCode = 1
	}
	return errorCode
}

// TestWebSocketClient_ErrorStream tests the websocket error stream by hard-coding a
// structured non-zero exit code error from the websocket server to the websocket client.
func TestWebSocketClient_ErrorStream(t *testing.T) {
	expectedExitCode := randomExitCode()
	// Create fake WebSocket server. Returns structured exit code error on error stream.
	websocketServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		conns, err := webSocketServerStreams(req, w, streamOptionsFromRequest(req))
		if err != nil {
			t.Fatalf("error on webSocketServerStreams: %v", err)
		}
		defer conns.conn.Close()
		_, err = io.Copy(conns.stderrStream, conns.stdinStream)
		if err != nil {
			t.Fatalf("error copying STDIN to STDERR: %v", err)
		}
		// Force an non-zero exit code error returned on the error stream.
		err = conns.writeStatus(&apierrors.StatusError{ErrStatus: metav1.Status{
			Status: metav1.StatusFailure,
			Reason: remotecommand.NonZeroExitCodeReason,
			Details: &metav1.StatusDetails{
				Causes: []metav1.StatusCause{
					{
						Type:    remotecommand.ExitCodeCauseType,
						Message: fmt.Sprintf("%d", expectedExitCode),
					},
				},
			},
		}})
		if err != nil {
			t.Fatalf("error writing status: %v", err)
		}
	}))
	defer websocketServer.Close()

	// Now create the WebSocket client (executor), and point it to the "websocketServer".
	websocketServer.URL = websocketServer.URL + "?" + "stdin=true" + "&" + "stderr=true"
	websocketLocation, err := url.Parse(websocketServer.URL)
	if err != nil {
		t.Fatalf("Unable to parse WebSocket server URL: %s", websocketServer.URL)
	}
	exec, err := NewWebSocketExecutor(&rest.Config{Host: websocketLocation.Host}, "GET", websocketServer.URL)
	if err != nil {
		t.Errorf("unexpected error creating websocket executor: %v", err)
	}
	randomData := make([]byte, 256)
	if _, err := rand.Read(randomData); err != nil {
		t.Errorf("unexpected error reading random data: %v", err)
	}
	var stderr bytes.Buffer
	options := &StreamOptions{
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
		// Validate remote command v5 protocol was negotiated.
		streamExec := exec.(*wsStreamExecutor)
		if remotecommand.StreamProtocolV5Name != streamExec.negotiated {
			t.Fatalf("expected remote command v5 protocol, got (%s)", streamExec.negotiated)
		}
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
}

// fakeTerminalSizeQueue implements TerminalSizeQueue, returning a random set of
// "maxSizes" number of TerminalSizes, storing the TerminalSizes in "sizes" slice.
type fakeTerminalSizeQueue struct {
	maxSizes      int
	terminalSizes []TerminalSize
}

// newTerminalSizeQueue returns a pointer to a fakeTerminalSizeQueue passing
// "max" number of random TerminalSizes created.
func newTerminalSizeQueue(max int) *fakeTerminalSizeQueue {
	return &fakeTerminalSizeQueue{
		maxSizes:      max,
		terminalSizes: make([]TerminalSize, 0, max),
	}
}

// Next returns a pointer to the next random TerminalSize, or nil if we have
// already returned "maxSizes" TerminalSizes already. Stores the randomly
// created TerminalSize in "terminalSizes" field for later validation.
func (f *fakeTerminalSizeQueue) Next() *TerminalSize {
	if len(f.terminalSizes) >= f.maxSizes {
		return nil
	}
	size := randomTerminalSize()
	f.terminalSizes = append(f.terminalSizes, size)
	return &size
}

// randomTerminalSize returns a TerminalSize with random values in the
// range (0-65535) for the fields Width and Height.
func randomTerminalSize() TerminalSize {
	randWidth := uint16(mrand.Intn(int(math.Pow(2, 16))))
	randHeight := uint16(mrand.Intn(int(math.Pow(2, 16))))
	return TerminalSize{
		Width:  randWidth,
		Height: randHeight,
	}
}

// randReader implements the ReadCloser interface, and it continuously
// returns random data until it is closed. Stores number of random
// bytes generated and returned.
type randReader struct {
	randBytes []byte
	closed    bool
	lock      sync.Mutex
}

// Read implements the Reader interface filling the passed buffer with
// random data, returning the number of bytes filled and an error
// if one occurs. Return 0 and EOF if the randReader has been closed.
func (r *randReader) Read(b []byte) (int, error) {
	r.lock.Lock()
	defer r.lock.Unlock()
	if r.closed {
		return 0, io.EOF
	}
	n, err := rand.Read(b)
	c := bytes.Clone(b)
	r.randBytes = append(r.randBytes, c...)
	return n, err
}

// Close implements the Closer interface, setting the close field true.
// Further calls to Read() after Close() will return 0, EOF. Returns
// nil error.
func (r *randReader) Close() (err error) {
	r.lock.Lock()
	defer r.lock.Unlock()
	r.closed = true
	return nil
}

// TestWebSocketClient_MultipleWriteChannels tests two streams (STDIN, TTY resize) writing to the
// websocket connection at the same time to exercise the connection write lock.
func TestWebSocketClient_MultipleWriteChannels(t *testing.T) {
	// Create the fake terminal size queue and the actualTerminalSizes which
	// will be received at the opposite websocket endpoint.
	numSizeQueue := 10000
	sizeQueue := newTerminalSizeQueue(numSizeQueue)
	actualTerminalSizes := make([]TerminalSize, 0, numSizeQueue)
	// Create ReadCloser sending random data on STDIN stream over websocket connection.
	stdinReader := randReader{randBytes: []byte{}, closed: false}
	// Create fake WebSocket server, which will receive concurrently the STDIN stream as
	// well as the resize stream (TerminalSizes). Store the TerminalSize data from the resize
	// stream for subsequent validation.
	websocketServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var wg sync.WaitGroup
		conns, err := webSocketServerStreams(req, w, streamOptionsFromRequest(req))
		if err != nil {
			t.Fatalf("error on webSocketServerStreams: %v", err)
		}
		defer conns.conn.Close()
		// Create goroutine to loopback the STDIN stream onto the STDOUT stream.
		wg.Add(1)
		go func() {
			_, err := io.Copy(conns.stdoutStream, conns.stdinStream)
			if err != nil {
				t.Errorf("error copying STDIN to STDOUT: %v", err)
			}
			wg.Done()
		}()
		// Read the terminal resize requests, storing them in actualTerminalSizes
		for i := 0; i < numSizeQueue; i++ {
			actualTerminalSize := <-conns.resizeChan
			actualTerminalSizes = append(actualTerminalSizes, actualTerminalSize)
		}
		stdinReader.Close() // Stops the random STDIN stream generation
		wg.Wait()           // Wait for all bytes copied from STDIN to STDOUT
	}))
	defer websocketServer.Close()
	// Now create the WebSocket client (executor), and point it to the "websocketServer".
	// Must add stdin, stdout, and TTY query param for the WebSocket client request.
	websocketServer.URL = websocketServer.URL + "?" + "tty=true" + "&" + "stdin=true" + "&" + "stdout=true"
	websocketLocation, err := url.Parse(websocketServer.URL)
	if err != nil {
		t.Fatalf("Unable to parse WebSocket server URL: %s", websocketServer.URL)
	}
	exec, err := NewWebSocketExecutor(&rest.Config{Host: websocketLocation.Host}, "GET", websocketServer.URL)
	if err != nil {
		t.Errorf("unexpected error creating websocket executor: %v", err)
	}
	var stdout bytes.Buffer
	options := &StreamOptions{
		Stdin:             &stdinReader,
		Stdout:            &stdout,
		Tty:               true,
		TerminalSizeQueue: sizeQueue,
	}
	errorChan := make(chan error)
	go func() {
		errorChan <- exec.StreamWithContext(context.Background(), *options)
	}()

	select {
	case <-time.After(wait.ForeverTestTimeout):
		t.Fatalf("expect stream to be closed after connection is closed.")
	case err := <-errorChan:
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
		// Validate remote command v5 protocol was negotiated.
		streamExec := exec.(*wsStreamExecutor)
		if remotecommand.StreamProtocolV5Name != streamExec.negotiated {
			t.Fatalf("expected remote command v5 protocol, got (%s)", streamExec.negotiated)
		}
	}
	// Check the random data sent on STDIN was the same returned on STDOUT *and*
	// that a minimum amount of random data was sent and received, ensuring concurrency.
	stdoutBytes, err := io.ReadAll(bytes.NewReader(stdout.Bytes()))
	if err != nil {
		t.Fatalf("error reading the stream: %v", err)
	}
	if len(stdoutBytes) == 0 {
		t.Errorf("No STDOUT bytes processed before resize stream finished: %d", len(stdoutBytes))
	}
	if !bytes.Equal(stdoutBytes, stdinReader.randBytes) {
		t.Errorf("unexpected data received (%d) sent (%d)", len(stdoutBytes), len(stdinReader.randBytes))
	}
	// Validate the random TerminalSizes sent on the resize stream are the same
	// as the actual TerminalSizes received at the websocket server.
	if len(actualTerminalSizes) != numSizeQueue {
		t.Errorf("expected received terminal size window (%d), got (%d)",
			numSizeQueue, len(actualTerminalSizes))
	}
	for i, actual := range actualTerminalSizes {
		expected := sizeQueue.terminalSizes[i]
		if !reflect.DeepEqual(expected, actual) {
			t.Errorf("expected terminal resize window %v, got %v", expected, actual)
		}
	}
}

// TestWebSocketClient_ProtocolVersions validates that remote command subprotocol versions V2-V4
// (V5 is already tested elsewhere) can be negotiated.
func TestWebSocketClient_ProtocolVersions(t *testing.T) {
	// Create a raw websocket server that accepts V2-V4 versions of
	// the remote command subprotocol.
	var upgrader = gwebsocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool {
			return true // Accepting all requests
		},
		Subprotocols: []string{
			remotecommand.StreamProtocolV4Name,
			remotecommand.StreamProtocolV3Name,
			remotecommand.StreamProtocolV2Name,
		},
	}
	// Upgrade a raw websocket server connection.
	websocketServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		conn, err := upgrader.Upgrade(w, req, nil)
		if err != nil {
			t.Fatalf("unable to upgrade to create websocket connection: %v", err)
		}
		defer conn.Close()
	}))
	defer websocketServer.Close()

	// Set up the websocket client with the STDOUT stream.
	websocketServer.URL = websocketServer.URL + "?" + "stdout=true"
	websocketLocation, err := url.Parse(websocketServer.URL)
	if err != nil {
		t.Fatalf("Unable to parse WebSocket server URL: %s", websocketServer.URL)
	}
	exec, err := NewWebSocketExecutor(&rest.Config{Host: websocketLocation.Host}, "GET", websocketServer.URL)
	if err != nil {
		t.Errorf("unexpected error creating websocket executor: %v", err)
	}
	// Iterate through previous remote command protocol versions, validating the
	// requested protocol version is the one that is negotiated.
	versions := []string{
		remotecommand.StreamProtocolV4Name,
		remotecommand.StreamProtocolV3Name,
		remotecommand.StreamProtocolV2Name,
	}
	for _, requestedVersion := range versions {
		streamExec := exec.(*wsStreamExecutor)
		streamExec.protocols = []string{requestedVersion}
		var stdout bytes.Buffer
		options := &StreamOptions{
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
		case <-errorChan:
			// Validate remote command protocol requestedVersion was negotiated.
			streamExec := exec.(*wsStreamExecutor)
			if requestedVersion != streamExec.negotiated {
				t.Fatalf("expected protocol version (%s), got (%s)", requestedVersion, streamExec.negotiated)
			}
		}
	}
}

// TestWebSocketClient_BadHandshake tests that a "bad handshake" error occurs when
// the WebSocketExecutor attempts to upgrade the connection to a subprotocol version
// (V4) that is not supported by the websocket server (only supports V5).
func TestWebSocketClient_BadHandshake(t *testing.T) {
	// Create fake WebSocket server (supports V5 subprotocol).
	websocketServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		// Bad handshake means websocket server will not completely initialize.
		_, err := webSocketServerStreams(req, w, streamOptionsFromRequest(req))
		if err == nil {
			t.Fatalf("expected error, but received none.")
		}
		if !strings.Contains(err.Error(), "websocket server finished before becoming ready") {
			t.Errorf("expected websocket server error, but got: %v", err)
		}
	}))
	defer websocketServer.Close()

	websocketServer.URL = websocketServer.URL + "?" + "stdout=true"
	websocketLocation, err := url.Parse(websocketServer.URL)
	if err != nil {
		t.Fatalf("Unable to parse WebSocket server URL: %s", websocketServer.URL)
	}
	exec, err := NewWebSocketExecutor(&rest.Config{Host: websocketLocation.Host}, "GET", websocketServer.URL)
	if err != nil {
		t.Errorf("unexpected error creating websocket executor: %v", err)
	}
	streamExec := exec.(*wsStreamExecutor)
	// Set the attempted subprotocol version to V4; websocket server only accepts V5.
	streamExec.protocols = []string{remotecommand.StreamProtocolV4Name}

	var stdout bytes.Buffer
	options := &StreamOptions{
		Stdout: &stdout,
	}
	errorChan := make(chan error)
	go func() {
		// Start the streaming on the WebSocket "exec" client.
		errorChan <- streamExec.StreamWithContext(context.Background(), *options)
	}()

	select {
	case <-time.After(wait.ForeverTestTimeout):
		t.Fatalf("expect stream to be closed after connection is closed.")
	case err := <-errorChan:
		// Expecting unable to upgrade connection -- "bad handshake" error.
		if err == nil {
			t.Errorf("expected error but received none")
		}
		if !strings.Contains(err.Error(), "bad handshake") {
			t.Errorf("expected bad handshake error, got (%s)", err)
		}
	}
}

// TestWebSocketClient_HeartbeatTimeout tests the heartbeat by forcing a
// timeout by setting the ping period greater than the deadline.
func TestWebSocketClient_HeartbeatTimeout(t *testing.T) {
	blockRequestCtx, unblockRequest := context.WithCancel(context.Background())
	defer unblockRequest()
	// Create fake WebSocket server which blocks.
	websocketServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		conns, err := webSocketServerStreams(req, w, streamOptionsFromRequest(req))
		if err != nil {
			t.Fatalf("error on webSocketServerStreams: %v", err)
		}
		defer conns.conn.Close()
		<-blockRequestCtx.Done()
	}))
	defer websocketServer.Close()
	// Create websocket client connecting to fake server.
	websocketServer.URL = websocketServer.URL + "?" + "stdin=true"
	websocketLocation, err := url.Parse(websocketServer.URL)
	if err != nil {
		t.Fatalf("Unable to parse WebSocket server URL: %s", websocketServer.URL)
	}
	exec, err := NewWebSocketExecutor(&rest.Config{Host: websocketLocation.Host}, "GET", websocketServer.URL)
	if err != nil {
		t.Errorf("unexpected error creating websocket executor: %v", err)
	}
	streamExec := exec.(*wsStreamExecutor)
	// Ping period is greater than the ping deadline, forcing the timeout to fire.
	pingPeriod := wait.ForeverTestTimeout // this lets the heartbeat deadline expire without renewing it
	pingDeadline := time.Second           // this gives setup 1 second to establish streams
	streamExec.heartbeatPeriod = pingPeriod
	streamExec.heartbeatDeadline = pingDeadline
	// Send some random data to the websocket server through STDIN.
	randomData := make([]byte, 128)
	if _, err := rand.Read(randomData); err != nil {
		t.Errorf("unexpected error reading random data: %v", err)
	}
	options := &StreamOptions{
		Stdin: bytes.NewReader(randomData),
	}
	errorChan := make(chan error)
	go func() {
		// Start the streaming on the WebSocket "exec" client.
		errorChan <- streamExec.StreamWithContext(context.Background(), *options)
	}()

	select {
	case <-time.After(wait.ForeverTestTimeout):
		t.Fatalf("expected heartbeat timeout, got none.")
	case err := <-errorChan:
		// Expecting heartbeat timeout error.
		if err == nil {
			t.Fatalf("expected error but received none")
		}
		if !strings.Contains(err.Error(), "i/o timeout") {
			t.Errorf("expected heartbeat timeout error, got (%s)", err)
		}
		// Validate remote command v5 protocol was negotiated.
		streamExec := exec.(*wsStreamExecutor)
		if remotecommand.StreamProtocolV5Name != streamExec.negotiated {
			t.Fatalf("expected remote command v5 protocol, got (%s)", streamExec.negotiated)
		}
	}
}

// TestWebSocketClient_TextMessageTypeError tests when the wrong message type is returned
// from the other websocket endpoint. Remote command protocols use "BinaryMessage", but
// this test hard-codes returning a "TextMessage".
func TestWebSocketClient_TextMessageTypeError(t *testing.T) {
	var upgrader = gwebsocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool {
			return true // Accepting all requests
		},
		Subprotocols: []string{remotecommand.StreamProtocolV5Name},
	}
	// Upgrade a raw websocket server connection. Returns wrong message type "TextMessage".
	websocketServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		conn, err := upgrader.Upgrade(w, req, nil)
		if err != nil {
			t.Fatalf("unable to upgrade to create websocket connection: %v", err)
		}
		defer conn.Close()
		msg := []byte("test message with wrong message type.")
		stdOutMsg := append([]byte{remotecommand.StreamStdOut}, msg...)
		// Wrong message type "TextMessage".
		err = conn.WriteMessage(gwebsocket.TextMessage, stdOutMsg)
		if err != nil {
			t.Fatalf("error writing text message to websocket: %v", err)
		}

	}))
	defer websocketServer.Close()

	// Set up the websocket client with the STDOUT stream.
	websocketServer.URL = websocketServer.URL + "?" + "stdout=true"
	websocketLocation, err := url.Parse(websocketServer.URL)
	if err != nil {
		t.Fatalf("Unable to parse WebSocket server URL: %s", websocketServer.URL)
	}
	exec, err := NewWebSocketExecutor(&rest.Config{Host: websocketLocation.Host}, "GET", websocketServer.URL)
	if err != nil {
		t.Errorf("unexpected error creating websocket executor: %v", err)
	}
	var stdout bytes.Buffer
	options := &StreamOptions{
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
		// Expecting bad message type error.
		if err == nil {
			t.Fatalf("expected error but received none")
		}
		if !strings.Contains(err.Error(), "unexpected message type") {
			t.Errorf("expected bad message type error, got (%s)", err)
		}
		// Validate remote command v5 protocol was negotiated.
		streamExec := exec.(*wsStreamExecutor)
		if remotecommand.StreamProtocolV5Name != streamExec.negotiated {
			t.Fatalf("expected remote command v5 protocol, got (%s)", streamExec.negotiated)
		}
	}
}

// TestWebSocketClient_EmptyMessageHandled tests that the error of a completely empty message
// is handled correctly. If the message is completely empty, the initial read of the stream id
// should fail (followed by cleanup).
func TestWebSocketClient_EmptyMessageHandled(t *testing.T) {
	var upgrader = gwebsocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool {
			return true // Accepting all requests
		},
		Subprotocols: []string{remotecommand.StreamProtocolV5Name},
	}
	// Upgrade a raw websocket server connection. Returns wrong message type "TextMessage".
	websocketServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		conn, err := upgrader.Upgrade(w, req, nil)
		if err != nil {
			t.Fatalf("unable to upgrade to create websocket connection: %v", err)
		}
		defer conn.Close()
		// Send completely empty message, including missing initial stream id.
		conn.WriteMessage(gwebsocket.BinaryMessage, []byte{}) //nolint:errcheck
	}))
	defer websocketServer.Close()

	// Set up the websocket client with the STDOUT stream.
	websocketServer.URL = websocketServer.URL + "?" + "stdout=true"
	websocketLocation, err := url.Parse(websocketServer.URL)
	if err != nil {
		t.Fatalf("Unable to parse WebSocket server URL: %s", websocketServer.URL)
	}
	exec, err := NewWebSocketExecutor(&rest.Config{Host: websocketLocation.Host}, "GET", websocketServer.URL)
	if err != nil {
		t.Errorf("unexpected error creating websocket executor: %v", err)
	}
	var stdout bytes.Buffer
	options := &StreamOptions{
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
		// Expecting error reading initial stream id.
		if err == nil {
			t.Fatalf("expected error but received none")
		}
		if !strings.Contains(err.Error(), "read stream id") {
			t.Errorf("expected error reading stream id, got (%s)", err)
		}
		// Validate remote command v5 protocol was negotiated.
		streamExec := exec.(*wsStreamExecutor)
		if remotecommand.StreamProtocolV5Name != streamExec.negotiated {
			t.Fatalf("expected remote command v5 protocol, got (%s)", streamExec.negotiated)
		}
	}
}

func TestWebSocketClient_ExecutorErrors(t *testing.T) {
	// Invalid config causes transport creation error in websocket executor constructor.
	config := rest.Config{
		ExecProvider: &clientcmdapi.ExecConfig{},
		AuthProvider: &clientcmdapi.AuthProviderConfig{},
	}
	_, err := NewWebSocketExecutor(&config, "GET", "http://localhost")
	if err == nil {
		t.Errorf("expecting executor constructor error, but received none.")
	} else if !strings.Contains(err.Error(), "error creating websocket transports") {
		t.Errorf("expecting error creating transports, got (%s)", err.Error())
	}
	// Verify that a nil context will cause an error in StreamWithContext
	exec, err := NewWebSocketExecutor(&rest.Config{}, "GET", "http://localhost")
	if err != nil {
		t.Errorf("unexpected error creating websocket executor: %v", err)
	}
	errorChan := make(chan error)
	go func() {
		// Start the streaming on the WebSocket "exec" client.
		var ctx context.Context
		errorChan <- exec.StreamWithContext(ctx, StreamOptions{})
	}()

	select {
	case <-time.After(wait.ForeverTestTimeout):
		t.Fatalf("expect stream to be closed after connection is closed.")
	case err := <-errorChan:
		// Expecting error with nil context.
		if err == nil {
			t.Fatalf("expected error but received none")
		}
		if !strings.Contains(err.Error(), "nil Context") {
			t.Errorf("expected nil context error, got (%s)", err)
		}
	}
}

func TestWebSocketClient_HeartbeatSucceeds(t *testing.T) {
	logger, _ := ktesting.NewTestContext(t)
	var upgrader = gwebsocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool {
			return true // Accepting all requests
		},
	}
	// Upgrade a raw websocket server connection, which automatically responds to Ping.
	websocketServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		conn, err := upgrader.Upgrade(w, req, nil)
		if err != nil {
			t.Fatalf("unable to upgrade to create websocket connection: %v", err)
		}
		defer conn.Close()
		for {
			_, _, err := conn.ReadMessage()
			if err != nil {
				break
			}
		}
	}))
	defer websocketServer.Close()
	// Create a raw websocket client, connecting to the websocket server.
	url := strings.ReplaceAll(websocketServer.URL, "http", "ws")
	client, _, err := gwebsocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()
	// Create a heartbeat using the client websocket connection, and start it.
	// "period" is less than "deadline", so ping/pong heartbeat will succceed.
	var expectedMsg = "test heartbeat message"
	var period = 100 * time.Millisecond
	var deadline = 200 * time.Millisecond
	heartbeat := newHeartbeat(logger, client, period, deadline)
	heartbeat.setMessage(expectedMsg)
	// Add a channel to the handler to retrieve the "pong" message.
	pongMsgCh := make(chan string)
	pongHandler := heartbeat.conn.PongHandler()
	heartbeat.conn.SetPongHandler(func(msg string) error {
		pongMsgCh <- msg
		return pongHandler(msg)
	})
	go heartbeat.start()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			_, _, err := client.ReadMessage()
			if err != nil {
				t.Logf("client err reading message: %v", err)
				return
			}
		}
	}()

	select {
	case actualMsg := <-pongMsgCh:
		close(heartbeat.closer)
		// Validate the received pong message is the same as sent in ping.
		if expectedMsg != actualMsg {
			t.Errorf("expected received pong message (%s), got (%s)", expectedMsg, actualMsg)
		}
	case <-time.After(period * 4):
		// This case should not happen.
		close(heartbeat.closer)
		t.Errorf("unexpected heartbeat timeout")
	}
	wg.Wait()
}

func TestLateStreamCreation(t *testing.T) {
	logger, _ := ktesting.NewTestContext(t)
	c := newWSStreamCreator(logger, nil)
	c.closeAllStreamReaders(nil)
	if err := c.setStream(0, nil); err == nil {
		t.Fatal("expected error adding stream after closeAllStreamReaders")
	}
}

func TestWebSocketClient_StreamsAndExpectedErrors(t *testing.T) {
	logger, _ := ktesting.NewTestContext(t)

	// Validate Stream functions.
	c := newWSStreamCreator(logger, nil)
	headers := http.Header{}
	headers.Set(v1.StreamType, v1.StreamTypeStdin)
	s, err := c.CreateStream(headers)
	if err != nil {
		t.Errorf("unexpected stream creation error: %v", err)
	}
	expectedStreamID := uint32(remotecommand.StreamStdIn)
	actualStreamID := s.Identifier()
	if expectedStreamID != actualStreamID {
		t.Errorf("expecting stream id (%d), got (%d)", expectedStreamID, actualStreamID)
	}
	actualHeaders := s.Headers()
	if !reflect.DeepEqual(headers, actualHeaders) {
		t.Errorf("expecting stream headers (%v), got (%v)", headers, actualHeaders)
	}
	// Validate stream reset does not return error.
	err = s.Reset()
	if err != nil {
		t.Errorf("unexpected error in stream reset: %v", err)
	}
	// Validate close with nil connection is an error.
	err = s.Close()
	if err == nil {
		t.Errorf("expecting stream Close error, but received none")
	}
	if !strings.Contains(err.Error(), "Close() on already closed stream") {
		t.Errorf("expected stream close error, got (%s)", err)
	}
	// Validate write with nil connection is an error.
	n, err := s.Write([]byte("not written"))
	if n != 0 {
		t.Errorf("expected zero bytes written, wrote (%d) instead", n)
	}
	if err == nil {
		t.Errorf("expecting stream Write error, but received none")
	}
	if !strings.Contains(err.Error(), "write on closed stream") {
		t.Errorf("expected stream write error, got (%s)", err)
	}
	// Validate CreateStream errors -- unknown stream
	headers = http.Header{}
	headers.Set(v1.StreamType, "UNKNOWN")
	_, err = c.CreateStream(headers)
	if err == nil {
		t.Errorf("expecting CreateStream error, but received none")
	} else if !strings.Contains(err.Error(), "unknown stream type") {
		t.Errorf("expecting unknown stream type error, got (%s)", err.Error())
	}
	// Validate CreateStream errors -- duplicate stream
	headers.Set(v1.StreamType, v1.StreamTypeError)
	c.streams[remotecommand.StreamErr] = &stream{}
	_, err = c.CreateStream(headers)
	if err == nil {
		t.Errorf("expecting CreateStream error, but received none")
	} else if !strings.Contains(err.Error(), "duplicate stream") {
		t.Errorf("expecting duplicate stream error, got (%s)", err.Error())
	}
}

// options contains details about which streams are required for
// remote command execution.
type options struct {
	stdin  bool
	stdout bool
	stderr bool
	tty    bool
}

// Translates query params in request into options struct.
func streamOptionsFromRequest(req *http.Request) *options {
	query := req.URL.Query()
	tty := query.Get("tty") == "true"
	stdin := query.Get("stdin") == "true"
	stdout := query.Get("stdout") == "true"
	stderr := query.Get("stderr") == "true"
	return &options{
		stdin:  stdin,
		stdout: stdout,
		stderr: stderr,
		tty:    tty,
	}
}

// websocketStreams contains the WebSocket connection and streams from a server.
type websocketStreams struct {
	conn         io.Closer
	stdinStream  io.ReadCloser
	stdoutStream io.WriteCloser
	stderrStream io.WriteCloser
	writeStatus  func(status *apierrors.StatusError) error
	resizeStream io.ReadCloser
	resizeChan   chan TerminalSize
	tty          bool
}

// Create WebSocket server streams to respond to a WebSocket client. Creates the streams passed
// in the stream options.
func webSocketServerStreams(req *http.Request, w http.ResponseWriter, opts *options) (*websocketStreams, error) {
	conn, err := createWebSocketStreams(req, w, opts)
	if err != nil {
		return nil, err
	}

	if conn.resizeStream != nil {
		conn.resizeChan = make(chan TerminalSize)
		go handleResizeEvents(req.Context(), conn.resizeStream, conn.resizeChan)
	}

	return conn, nil
}

// Read terminal resize events off of passed stream and queue into passed channel.
func handleResizeEvents(ctx context.Context, stream io.Reader, channel chan<- TerminalSize) {
	defer close(channel)

	decoder := json.NewDecoder(stream)
	for {
		size := TerminalSize{}
		if err := decoder.Decode(&size); err != nil {
			break
		}

		select {
		case channel <- size:
		case <-ctx.Done():
			// To avoid leaking this routine, exit if the http request finishes. This path
			// would generally be hit if starting the process fails and nothing is started to
			// ingest these resize events.
			return
		}
	}
}

// createChannels returns the standard channel types for a shell connection (STDIN 0, STDOUT 1, STDERR 2)
// along with the approximate duplex value. It also creates the error (3) and resize (4) channels.
func createChannels(opts *options) []wsstream.ChannelType {
	// open the requested channels, and always open the error channel
	channels := make([]wsstream.ChannelType, 5)
	channels[remotecommand.StreamStdIn] = readChannel(opts.stdin)
	channels[remotecommand.StreamStdOut] = writeChannel(opts.stdout)
	channels[remotecommand.StreamStdErr] = writeChannel(opts.stderr)
	channels[remotecommand.StreamErr] = wsstream.WriteChannel
	channels[remotecommand.StreamResize] = wsstream.ReadChannel
	return channels
}

// readChannel returns wsstream.ReadChannel if real is true, or wsstream.IgnoreChannel.
func readChannel(real bool) wsstream.ChannelType {
	if real {
		return wsstream.ReadChannel
	}
	return wsstream.IgnoreChannel
}

// writeChannel returns wsstream.WriteChannel if real is true, or wsstream.IgnoreChannel.
func writeChannel(real bool) wsstream.ChannelType {
	if real {
		return wsstream.WriteChannel
	}
	return wsstream.IgnoreChannel
}

// createWebSocketStreams returns a "channels" struct containing the websocket connection and
// streams needed to perform an exec or an attach.
func createWebSocketStreams(req *http.Request, w http.ResponseWriter, opts *options) (*websocketStreams, error) {
	channels := createChannels(opts)
	conn := wsstream.NewConn(map[string]wsstream.ChannelProtocolConfig{
		remotecommand.StreamProtocolV5Name: {
			Binary:   true,
			Channels: channels,
		},
	})
	conn.SetIdleTimeout(4 * time.Hour)
	// Opening the connection responds to WebSocket client, negotiating
	// the WebSocket upgrade connection and the subprotocol.
	_, streams, err := conn.Open(w, req)
	if err != nil {
		return nil, err
	}

	// Send an empty message to the lowest writable channel to notify the client the connection is established
	//nolint:errcheck
	switch {
	case opts.stdout:
		streams[remotecommand.StreamStdOut].Write([]byte{})
	case opts.stderr:
		streams[remotecommand.StreamStdErr].Write([]byte{})
	default:
		streams[remotecommand.StreamErr].Write([]byte{})
	}

	wsStreams := &websocketStreams{
		conn:         conn,
		stdinStream:  streams[remotecommand.StreamStdIn],
		stdoutStream: streams[remotecommand.StreamStdOut],
		stderrStream: streams[remotecommand.StreamStdErr],
		tty:          opts.tty,
		resizeStream: streams[remotecommand.StreamResize],
	}

	wsStreams.writeStatus = func(stream io.Writer) func(status *apierrors.StatusError) error {
		return func(status *apierrors.StatusError) error {
			bs, err := json.Marshal(status.Status())
			if err != nil {
				return err
			}
			_, err = stream.Write(bs)
			return err
		}
	}(streams[remotecommand.StreamErr])

	return wsStreams, nil
}

// See (https://github.com/kubernetes/kubernetes/issues/126134).
func TestWebSocketClient_HTTPSProxyErrorExpected(t *testing.T) {
	urlStr := "http://127.0.0.1/never-used" + "?" + "stdin=true" + "&" + "stdout=true"
	websocketLocation, err := url.Parse(urlStr)
	if err != nil {
		t.Fatalf("Unable to parse WebSocket server URL: %s", urlStr)
	}
	// proxy url with https scheme will trigger websocket dialing error.
	httpsProxyFunc := func(req *http.Request) (*url.URL, error) { return url.Parse("https://127.0.0.1") }
	exec, err := NewWebSocketExecutor(&rest.Config{Host: websocketLocation.Host, Proxy: httpsProxyFunc}, "GET", urlStr)
	if err != nil {
		t.Errorf("unexpected error creating websocket executor: %v", err)
	}
	var stdout bytes.Buffer
	options := &StreamOptions{
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
		if err == nil {
			t.Errorf("expected error but received none")
		}
		if !httpstream.IsHTTPSProxyError(err) {
			t.Errorf("expected https proxy error, got (%s)", err)
		}
	}
}
