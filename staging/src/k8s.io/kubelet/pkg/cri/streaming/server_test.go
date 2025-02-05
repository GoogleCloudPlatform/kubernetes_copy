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

package streaming

import (
	"context"
	"crypto/tls"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	api "k8s.io/api/core/v1"
	restclient "k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	"k8s.io/client-go/transport/spdy"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
	kubeletportforward "k8s.io/kubelet/pkg/cri/streaming/portforward"
)

const (
	testAddr         = "localhost:12345"
	testContainerID  = "container789"
	testPodSandboxID = "pod0987"
)

func TestGetExec(t *testing.T) {
	serv, err := NewServer(Config{
		Addr: testAddr,
	}, nil)
	assert.NoError(t, err)

	tlsServer, err := NewServer(Config{
		Addr:      testAddr,
		TLSConfig: &tls.Config{},
	}, nil)
	assert.NoError(t, err)

	const pathPrefix = "cri/shim"
	prefixServer, err := NewServer(Config{
		Addr: testAddr,
		BaseURL: &url.URL{
			Scheme: "http",
			Host:   testAddr,
			Path:   "/" + pathPrefix + "/",
		},
	}, nil)
	assert.NoError(t, err)

	assertRequestToken := func(expectedReq *runtimeapi.ExecRequest, cache *requestCache, token string) {
		req, ok := cache.Consume(token)
		require.True(t, ok, "token %s not found!", token)
		assert.Equal(t, expectedReq, req)
	}
	request := &runtimeapi.ExecRequest{
		ContainerId: testContainerID,
		Cmd:         []string{"echo", "foo"},
		Tty:         true,
		Stdin:       true,
	}
	{ // Non-TLS
		resp, err := serv.GetExec(request)
		assert.NoError(t, err)
		expectedURL := "http://" + testAddr + "/exec/"
		assert.Contains(t, resp.Url, expectedURL)
		token := strings.TrimPrefix(resp.Url, expectedURL)
		assertRequestToken(request, serv.(*server).cache, token)
	}

	{ // TLS
		resp, err := tlsServer.GetExec(request)
		assert.NoError(t, err)
		expectedURL := "https://" + testAddr + "/exec/"
		assert.Contains(t, resp.Url, expectedURL)
		token := strings.TrimPrefix(resp.Url, expectedURL)
		assertRequestToken(request, tlsServer.(*server).cache, token)
	}

	{ // Path prefix
		resp, err := prefixServer.GetExec(request)
		assert.NoError(t, err)
		expectedURL := "http://" + testAddr + "/" + pathPrefix + "/exec/"
		assert.Contains(t, resp.Url, expectedURL)
		token := strings.TrimPrefix(resp.Url, expectedURL)
		assertRequestToken(request, prefixServer.(*server).cache, token)
	}
}

func TestValidateExecAttachRequest(t *testing.T) {
	type config struct {
		tty    bool
		stdin  bool
		stdout bool
		stderr bool
	}
	for _, tc := range []struct {
		desc      string
		configs   []config
		expectErr bool
	}{
		{
			desc:      "at least one stream must be true",
			expectErr: true,
			configs: []config{
				{false, false, false, false},
				{true, false, false, false}},
		},
		{
			desc:      "tty and stderr cannot both be true",
			expectErr: true,
			configs: []config{
				{true, false, false, true},
				{true, false, true, true},
				{true, true, false, true},
				{true, true, true, true},
			},
		},
		{
			desc:      "a valid config should pass",
			expectErr: false,
			configs: []config{
				{false, false, false, true},
				{false, false, true, false},
				{false, false, true, true},
				{false, true, false, false},
				{false, true, false, true},
				{false, true, true, false},
				{false, true, true, true},
				{true, false, true, false},
				{true, true, false, false},
				{true, true, true, false},
			},
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			for _, c := range tc.configs {
				// validate the exec request.
				execReq := &runtimeapi.ExecRequest{
					ContainerId: testContainerID,
					Cmd:         []string{"date"},
					Tty:         c.tty,
					Stdin:       c.stdin,
					Stdout:      c.stdout,
					Stderr:      c.stderr,
				}
				err := validateExecRequest(execReq)
				assert.Equal(t, tc.expectErr, err != nil, "config: %v,  err: %v", c, err)

				// validate the attach request.
				attachReq := &runtimeapi.AttachRequest{
					ContainerId: testContainerID,
					Tty:         c.tty,
					Stdin:       c.stdin,
					Stdout:      c.stdout,
					Stderr:      c.stderr,
				}
				err = validateAttachRequest(attachReq)
				assert.Equal(t, tc.expectErr, err != nil, "config: %v, err: %v", c, err)
			}
		})
	}
}

func TestGetAttach(t *testing.T) {
	serv, err := NewServer(Config{
		Addr: testAddr,
	}, nil)
	require.NoError(t, err)

	tlsServer, err := NewServer(Config{
		Addr:      testAddr,
		TLSConfig: &tls.Config{},
	}, nil)
	require.NoError(t, err)

	assertRequestToken := func(expectedReq *runtimeapi.AttachRequest, cache *requestCache, token string) {
		req, ok := cache.Consume(token)
		require.True(t, ok, "token %s not found!", token)
		assert.Equal(t, expectedReq, req)
	}

	request := &runtimeapi.AttachRequest{
		ContainerId: testContainerID,
		Stdin:       true,
		Tty:         true,
	}
	{ // Non-TLS
		resp, err := serv.GetAttach(request)
		assert.NoError(t, err)
		expectedURL := "http://" + testAddr + "/attach/"
		assert.Contains(t, resp.Url, expectedURL)
		token := strings.TrimPrefix(resp.Url, expectedURL)
		assertRequestToken(request, serv.(*server).cache, token)
	}

	{ // TLS
		resp, err := tlsServer.GetAttach(request)
		assert.NoError(t, err)
		expectedURL := "https://" + testAddr + "/attach/"
		assert.Contains(t, resp.Url, expectedURL)
		token := strings.TrimPrefix(resp.Url, expectedURL)
		assertRequestToken(request, tlsServer.(*server).cache, token)
	}
}

func TestGetPortForward(t *testing.T) {
	podSandboxID := testPodSandboxID
	request := &runtimeapi.PortForwardRequest{
		PodSandboxId: podSandboxID,
		Port:         []int32{1, 2, 3, 4},
	}

	{ // Non-TLS
		serv, err := NewServer(Config{
			Addr: testAddr,
		}, nil)
		assert.NoError(t, err)
		resp, err := serv.GetPortForward(request)
		assert.NoError(t, err)
		expectedURL := "http://" + testAddr + "/portforward/"
		assert.True(t, strings.HasPrefix(resp.Url, expectedURL))
		token := strings.TrimPrefix(resp.Url, expectedURL)
		req, ok := serv.(*server).cache.Consume(token)
		require.True(t, ok, "token %s not found!", token)
		assert.Equal(t, testPodSandboxID, req.(*runtimeapi.PortForwardRequest).PodSandboxId)
	}

	{ // TLS
		tlsServer, err := NewServer(Config{
			Addr:      testAddr,
			TLSConfig: &tls.Config{},
		}, nil)
		assert.NoError(t, err)
		resp, err := tlsServer.GetPortForward(request)
		assert.NoError(t, err)
		expectedURL := "https://" + testAddr + "/portforward/"
		assert.True(t, strings.HasPrefix(resp.Url, expectedURL))
		token := strings.TrimPrefix(resp.Url, expectedURL)
		req, ok := tlsServer.(*server).cache.Consume(token)
		require.True(t, ok, "token %s not found!", token)
		assert.Equal(t, testPodSandboxID, req.(*runtimeapi.PortForwardRequest).PodSandboxId)
	}
}

func TestServeExec(t *testing.T) {
	runRemoteCommandTest(t, "exec")
}

func TestServeAttach(t *testing.T) {
	runRemoteCommandTest(t, "attach")
}

func TestServePortForward(t *testing.T) {
	s, testServer := startTestServer(t)
	defer testServer.Close()

	resp, err := s.GetPortForward(&runtimeapi.PortForwardRequest{
		PodSandboxId: testPodSandboxID,
	})
	require.NoError(t, err)
	reqURL, err := url.Parse(resp.Url)
	require.NoError(t, err)

	transport, upgrader, err := spdy.RoundTripperFor(&restclient.Config{})
	require.NoError(t, err)
	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport}, "POST", reqURL)
	streamConn, _, err := dialer.Dial(kubeletportforward.ProtocolV1Name)
	require.NoError(t, err)
	defer streamConn.Close()

	// Create the streams.
	headers := http.Header{}
	// Error stream is required, but unused in this test.
	headers.Set(api.StreamType, api.StreamTypeError)
	headers.Set(api.PortHeader, strconv.Itoa(testPort))
	_, err = streamConn.CreateStream(headers)
	require.NoError(t, err)
	// Setup the data stream.
	headers.Set(api.StreamType, api.StreamTypeData)
	headers.Set(api.PortHeader, strconv.Itoa(testPort))
	stream, err := streamConn.CreateStream(headers)
	require.NoError(t, err)

	doClientStreams(t, "portforward", stream, stream, nil)
}

// Run the remote command test.
// commandType is either "exec" or "attach".
func runRemoteCommandTest(t *testing.T, commandType string) {
	s, testServer := startTestServer(t)
	defer testServer.Close()

	var reqURL *url.URL
	stdin, stdout, stderr := true, true, true
	containerID := testContainerID
	switch commandType {
	case "exec":
		resp, err := s.GetExec(&runtimeapi.ExecRequest{
			ContainerId: containerID,
			Cmd:         []string{"echo"},
			Stdin:       stdin,
			Stdout:      stdout,
			Stderr:      stderr,
		})
		require.NoError(t, err)
		reqURL, err = url.Parse(resp.Url)
		require.NoError(t, err)
	case "attach":
		resp, err := s.GetAttach(&runtimeapi.AttachRequest{
			ContainerId: containerID,
			Stdin:       stdin,
			Stdout:      stdout,
			Stderr:      stderr,
		})
		require.NoError(t, err)
		reqURL, err = url.Parse(resp.Url)
		require.NoError(t, err)
	}

	wg := sync.WaitGroup{}
	wg.Add(2)

	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()
	stderrR, stderrW := io.Pipe()

	go func() {
		defer wg.Done()
		exec, err := remotecommand.NewSPDYExecutor(&restclient.Config{}, "POST", reqURL)
		require.NoError(t, err)

		opts := remotecommand.StreamOptions{
			Stdin:  stdinR,
			Stdout: stdoutW,
			Stderr: stderrW,
			Tty:    false,
		}
		require.NoError(t, exec.StreamWithContext(context.Background(), opts))
	}()

	go func() {
		defer wg.Done()
		doClientStreams(t, commandType, stdinW, stdoutR, stderrR)
	}()

	wg.Wait()

	// Repeat request with the same URL should be a 404.
	resp, err := http.Get(reqURL.String())
	require.NoError(t, err)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func startTestServer(t *testing.T) (Server, *httptest.Server) {
	var s Server
	testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.ServeHTTP(w, r)
	}))
	cleanup := true
	defer func() {
		if cleanup {
			testServer.Close()
		}
	}()

	testURL, err := url.Parse(testServer.URL)
	require.NoError(t, err)

	rt := newFakeRuntime(t)
	config := DefaultConfig
	config.BaseURL = testURL
	s, err = NewServer(config, rt)
	require.NoError(t, err)

	cleanup = false // Caller must close the test server.
	return s, testServer
}

const (
	testInput  = "abcdefg"
	testOutput = "fooBARbaz"
	testErr    = "ERROR!!!"
	testPort   = 12345
)

func newFakeRuntime(t *testing.T) *fakeRuntime {
	return &fakeRuntime{
		t: t,
	}
}

type fakeRuntime struct {
	t *testing.T
}

func (f *fakeRuntime) Exec(_ context.Context, containerID string, cmd []string, stdin io.Reader, stdout, stderr io.WriteCloser, tty bool, resize <-chan remotecommand.TerminalSize) error {
	assert.Equal(f.t, testContainerID, containerID)
	doServerStreams(f.t, "exec", stdin, stdout, stderr)
	return nil
}

func (f *fakeRuntime) Attach(_ context.Context, containerID string, stdin io.Reader, stdout, stderr io.WriteCloser, tty bool, resize <-chan remotecommand.TerminalSize) error {
	assert.Equal(f.t, testContainerID, containerID)
	doServerStreams(f.t, "attach", stdin, stdout, stderr)
	return nil
}

func (f *fakeRuntime) PortForward(_ context.Context, podSandboxID string, port int32, stream io.ReadWriteCloser) error {
	assert.Equal(f.t, testPodSandboxID, podSandboxID)
	assert.EqualValues(f.t, testPort, port)
	doServerStreams(f.t, "portforward", stream, stream, nil)
	return nil
}

// Send & receive expected input/output. Must be the inverse of doClientStreams.
// Function will block until the expected i/o is finished.
func doServerStreams(t *testing.T, prefix string, stdin io.Reader, stdout, stderr io.Writer) {
	if stderr != nil {
		writeExpected(t, "server stderr", stderr, prefix+testErr)
	}
	readExpected(t, "server stdin", stdin, prefix+testInput)
	writeExpected(t, "server stdout", stdout, prefix+testOutput)
}

// Send & receive expected input/output. Must be the inverse of doServerStreams.
// Function will block until the expected i/o is finished.
func doClientStreams(t *testing.T, prefix string, stdin io.Writer, stdout, stderr io.Reader) {
	if stderr != nil {
		readExpected(t, "client stderr", stderr, prefix+testErr)
	}
	writeExpected(t, "client stdin", stdin, prefix+testInput)
	readExpected(t, "client stdout", stdout, prefix+testOutput)
}

// Read and verify the expected string from the stream.
func readExpected(t *testing.T, streamName string, r io.Reader, expected string) {
	result := make([]byte, len(expected))
	_, err := io.ReadAtLeast(r, result, len(expected))
	assert.NoError(t, err, "stream %s", streamName)
	assert.Equal(t, expected, string(result), "stream %s", streamName)
}

// Write and verify success of the data over the stream.
func writeExpected(t *testing.T, streamName string, w io.Writer, data string) {
	n, err := io.WriteString(w, data)
	assert.NoError(t, err, "stream %s", streamName)
	assert.Len(t, data, n, "stream %s", streamName)
}
