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

package server

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	cadvisorapi "github.com/google/cadvisor/info/v1"
	cadvisorapiv2 "github.com/google/cadvisor/info/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/utils/ptr"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/httpstream"
	"k8s.io/apimachinery/pkg/util/httpstream/spdy"
	"k8s.io/apiserver/pkg/authentication/authenticator"
	"k8s.io/apiserver/pkg/authentication/user"
	"k8s.io/apiserver/pkg/authorization/authorizer"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/tools/remotecommand"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
	statsapi "k8s.io/kubelet/pkg/apis/stats/v1alpha1"
	api "k8s.io/kubernetes/pkg/apis/core"

	// Do some initialization to decode the query parameters correctly.
	"k8s.io/apiserver/pkg/server/healthz"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	zpagesfeatures "k8s.io/component-base/zpages/features"
	"k8s.io/component-base/zpages/flagz"
	"k8s.io/kubelet/pkg/cri/streaming"
	"k8s.io/kubelet/pkg/cri/streaming/portforward"
	remotecommandserver "k8s.io/kubelet/pkg/cri/streaming/remotecommand"
	_ "k8s.io/kubernetes/pkg/apis/core/install"
	"k8s.io/kubernetes/pkg/features"
	kubeletconfiginternal "k8s.io/kubernetes/pkg/kubelet/apis/config"
	"k8s.io/kubernetes/pkg/kubelet/cm"
	"k8s.io/kubernetes/pkg/kubelet/server/stats"
	"k8s.io/kubernetes/pkg/volume"
)

const (
	testUID          = "9b01b80f-8fb4-11e4-95ab-4200af06647"
	testContainerID  = "container789"
	testPodSandboxID = "pod0987"
)

type fakeKubelet struct {
	podByNameFunc       func(namespace, name string) (*v1.Pod, bool)
	machineInfoFunc     func() (*cadvisorapi.MachineInfo, error)
	podsFunc            func() []*v1.Pod
	runningPodsFunc     func(ctx context.Context) ([]*v1.Pod, error)
	logFunc             func(w http.ResponseWriter, req *http.Request)
	runFunc             func(podFullName string, uid types.UID, containerName string, cmd []string) ([]byte, error)
	getExecCheck        func(string, types.UID, string, []string, remotecommandserver.Options)
	getAttachCheck      func(string, types.UID, string, remotecommandserver.Options)
	getPortForwardCheck func(string, string, types.UID, portforward.V4Options)

	containerLogsFunc func(ctx context.Context, podFullName, containerName string, logOptions *v1.PodLogOptions, stdout, stderr io.Writer) error
	hostnameFunc      func() string
	resyncInterval    time.Duration
	loopEntryTime     time.Time
	plegHealth        bool
	streamingRuntime  streaming.Server
}

func (fk *fakeKubelet) LatestLoopEntryTime() time.Time {
	return fk.loopEntryTime
}

func (fk *fakeKubelet) GetPodByName(namespace, name string) (*v1.Pod, bool) {
	return fk.podByNameFunc(namespace, name)
}

func (fk *fakeKubelet) GetRequestedContainersInfo(containerName string, options cadvisorapiv2.RequestOptions) (map[string]*cadvisorapi.ContainerInfo, error) {
	return map[string]*cadvisorapi.ContainerInfo{}, nil
}

func (fk *fakeKubelet) GetCachedMachineInfo() (*cadvisorapi.MachineInfo, error) {
	return fk.machineInfoFunc()
}

func (*fakeKubelet) GetVersionInfo() (*cadvisorapi.VersionInfo, error) {
	return &cadvisorapi.VersionInfo{}, nil
}

func (fk *fakeKubelet) GetPods() []*v1.Pod {
	return fk.podsFunc()
}

func (fk *fakeKubelet) GetRunningPods(ctx context.Context) ([]*v1.Pod, error) {
	return fk.runningPodsFunc(ctx)
}

func (fk *fakeKubelet) ServeLogs(w http.ResponseWriter, req *http.Request) {
	fk.logFunc(w, req)
}

func (fk *fakeKubelet) GetKubeletContainerLogs(ctx context.Context, podFullName, containerName string, logOptions *v1.PodLogOptions, stdout, stderr io.Writer) error {
	return fk.containerLogsFunc(ctx, podFullName, containerName, logOptions, stdout, stderr)
}

func (fk *fakeKubelet) GetHostname() string {
	return fk.hostnameFunc()
}

func (fk *fakeKubelet) RunInContainer(_ context.Context, podFullName string, uid types.UID, containerName string, cmd []string) ([]byte, error) {
	return fk.runFunc(podFullName, uid, containerName, cmd)
}

func (fk *fakeKubelet) CheckpointContainer(_ context.Context, podUID types.UID, podFullName, containerName string, options *runtimeapi.CheckpointContainerRequest) error {
	if containerName == "checkpointingFailure" {
		return fmt.Errorf("Returning error for test")
	}
	return nil
}

func (fk *fakeKubelet) ListMetricDescriptors(ctx context.Context) ([]*runtimeapi.MetricDescriptor, error) {
	return nil, nil
}

func (fk *fakeKubelet) ListPodSandboxMetrics(ctx context.Context) ([]*runtimeapi.PodSandboxMetrics, error) {
	return nil, nil
}

func (fk *fakeKubelet) SyncLoopHealthCheck(req *http.Request) error {
	duration := fk.resyncInterval * 2
	minDuration := time.Minute * 5
	if duration < minDuration {
		duration = minDuration
	}
	enterLoopTime := fk.LatestLoopEntryTime()
	if !enterLoopTime.IsZero() && time.Now().After(enterLoopTime.Add(duration)) {
		return fmt.Errorf("sync Loop took longer than expected")
	}
	return nil
}

type fakeRuntime struct {
	execFunc        func(string, []string, io.Reader, io.WriteCloser, io.WriteCloser, bool, <-chan remotecommand.TerminalSize) error
	attachFunc      func(string, io.Reader, io.WriteCloser, io.WriteCloser, bool, <-chan remotecommand.TerminalSize) error
	portForwardFunc func(string, int32, io.ReadWriteCloser) error
}

func (f *fakeRuntime) Exec(_ context.Context, containerID string, cmd []string, stdin io.Reader, stdout, stderr io.WriteCloser, tty bool, resize <-chan remotecommand.TerminalSize) error {
	return f.execFunc(containerID, cmd, stdin, stdout, stderr, tty, resize)
}

func (f *fakeRuntime) Attach(_ context.Context, containerID string, stdin io.Reader, stdout, stderr io.WriteCloser, tty bool, resize <-chan remotecommand.TerminalSize) error {
	return f.attachFunc(containerID, stdin, stdout, stderr, tty, resize)
}

func (f *fakeRuntime) PortForward(_ context.Context, podSandboxID string, port int32, stream io.ReadWriteCloser) error {
	return f.portForwardFunc(podSandboxID, port, stream)
}

type testStreamingServer struct {
	streaming.Server
	fakeRuntime    *fakeRuntime
	testHTTPServer *httptest.Server
}

func newTestStreamingServer(streamIdleTimeout time.Duration) (s *testStreamingServer, err error) {
	s = &testStreamingServer{}
	s.testHTTPServer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.ServeHTTP(w, r)
	}))
	defer func() {
		if err != nil {
			s.testHTTPServer.Close()
		}
	}()

	testURL, err := url.Parse(s.testHTTPServer.URL)
	if err != nil {
		return nil, err
	}

	s.fakeRuntime = &fakeRuntime{}
	config := streaming.DefaultConfig
	config.BaseURL = testURL
	if streamIdleTimeout != 0 {
		config.StreamIdleTimeout = streamIdleTimeout
	}
	s.Server, err = streaming.NewServer(config, s.fakeRuntime)
	if err != nil {
		return nil, err
	}
	return s, nil
}

func (fk *fakeKubelet) GetExec(_ context.Context, podFullName string, podUID types.UID, containerName string, cmd []string, streamOpts remotecommandserver.Options) (*url.URL, error) {
	if fk.getExecCheck != nil {
		fk.getExecCheck(podFullName, podUID, containerName, cmd, streamOpts)
	}
	// Always use testContainerID
	resp, err := fk.streamingRuntime.GetExec(&runtimeapi.ExecRequest{
		ContainerId: testContainerID,
		Cmd:         cmd,
		Tty:         streamOpts.TTY,
		Stdin:       streamOpts.Stdin,
		Stdout:      streamOpts.Stdout,
		Stderr:      streamOpts.Stderr,
	})
	if err != nil {
		return nil, err
	}
	return url.Parse(resp.GetUrl())
}

func (fk *fakeKubelet) GetAttach(_ context.Context, podFullName string, podUID types.UID, containerName string, streamOpts remotecommandserver.Options) (*url.URL, error) {
	if fk.getAttachCheck != nil {
		fk.getAttachCheck(podFullName, podUID, containerName, streamOpts)
	}
	// Always use testContainerID
	resp, err := fk.streamingRuntime.GetAttach(&runtimeapi.AttachRequest{
		ContainerId: testContainerID,
		Tty:         streamOpts.TTY,
		Stdin:       streamOpts.Stdin,
		Stdout:      streamOpts.Stdout,
		Stderr:      streamOpts.Stderr,
	})
	if err != nil {
		return nil, err
	}
	return url.Parse(resp.GetUrl())
}

func (fk *fakeKubelet) GetPortForward(ctx context.Context, podName, podNamespace string, podUID types.UID, portForwardOpts portforward.V4Options) (*url.URL, error) {
	if fk.getPortForwardCheck != nil {
		fk.getPortForwardCheck(podName, podNamespace, podUID, portForwardOpts)
	}
	// Always use testPodSandboxID
	resp, err := fk.streamingRuntime.GetPortForward(&runtimeapi.PortForwardRequest{
		PodSandboxId: testPodSandboxID,
		Port:         portForwardOpts.Ports,
	})
	if err != nil {
		return nil, err
	}
	return url.Parse(resp.GetUrl())
}

// Unused functions
func (*fakeKubelet) GetNode() (*v1.Node, error)                       { return nil, nil }
func (*fakeKubelet) GetNodeConfig() cm.NodeConfig                     { return cm.NodeConfig{} }
func (*fakeKubelet) GetPodCgroupRoot() string                         { return "" }
func (*fakeKubelet) GetPodByCgroupfs(cgroupfs string) (*v1.Pod, bool) { return nil, false }
func (fk *fakeKubelet) ListVolumesForPod(podUID types.UID) (map[string]volume.Volume, bool) {
	return map[string]volume.Volume{}, true
}
func (*fakeKubelet) ListBlockVolumesForPod(podUID types.UID) (map[string]volume.BlockVolume, bool) {
	return map[string]volume.BlockVolume{}, true
}
func (*fakeKubelet) RootFsStats() (*statsapi.FsStats, error)                     { return nil, nil }
func (*fakeKubelet) ListPodStats(_ context.Context) ([]statsapi.PodStats, error) { return nil, nil }
func (*fakeKubelet) ListPodStatsAndUpdateCPUNanoCoreUsage(_ context.Context) ([]statsapi.PodStats, error) {
	return nil, nil
}
func (*fakeKubelet) ListPodCPUAndMemoryStats(_ context.Context) ([]statsapi.PodStats, error) {
	return nil, nil
}
func (*fakeKubelet) ImageFsStats(_ context.Context) (*statsapi.FsStats, *statsapi.FsStats, error) {
	return nil, nil, nil
}
func (*fakeKubelet) RlimitStats() (*statsapi.RlimitStats, error) { return nil, nil }
func (*fakeKubelet) GetCgroupStats(cgroupName string, updateStats bool) (*statsapi.ContainerStats, *statsapi.NetworkStats, error) {
	return nil, nil, nil
}
func (*fakeKubelet) GetCgroupCPUAndMemoryStats(cgroupName string, updateStats bool) (*statsapi.ContainerStats, error) {
	return nil, nil
}

type fakeAuth struct {
	authenticateFunc func(*http.Request) (*authenticator.Response, bool, error)
	attributesFunc   func(user.Info, *http.Request) []authorizer.Attributes
	authorizeFunc    func(authorizer.Attributes) (authorized authorizer.Decision, reason string, err error)
}

func (f *fakeAuth) AuthenticateRequest(req *http.Request) (*authenticator.Response, bool, error) {
	return f.authenticateFunc(req)
}
func (f *fakeAuth) GetRequestAttributes(u user.Info, req *http.Request) []authorizer.Attributes {
	return f.attributesFunc(u, req)
}
func (f *fakeAuth) Authorize(ctx context.Context, a authorizer.Attributes) (authorized authorizer.Decision, reason string, err error) {
	return f.authorizeFunc(a)
}

type serverTestFramework struct {
	serverUnderTest *Server
	fakeKubelet     *fakeKubelet
	fakeAuth        *fakeAuth
	testHTTPServer  *httptest.Server
}

func newServerTest() *serverTestFramework {
	return newServerTestWithDebug(true, nil)
}

func newServerTestWithDebug(enableDebugging bool, streamingServer streaming.Server) *serverTestFramework {
	kubeCfg := &kubeletconfiginternal.KubeletConfiguration{
		EnableDebuggingHandlers: enableDebugging,
		EnableSystemLogHandler:  enableDebugging,
		EnableProfilingHandler:  enableDebugging,
		EnableDebugFlagsHandler: enableDebugging,
	}
	return newServerTestWithDebuggingHandlers(kubeCfg, streamingServer)
}

func newServerTestWithDebuggingHandlers(kubeCfg *kubeletconfiginternal.KubeletConfiguration, streamingServer streaming.Server) *serverTestFramework {

	fw := &serverTestFramework{}
	fw.fakeKubelet = &fakeKubelet{
		hostnameFunc: func() string {
			return "127.0.0.1"
		},
		podByNameFunc: func(namespace, name string) (*v1.Pod, bool) {
			return &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: namespace,
					Name:      name,
					UID:       testUID,
				},
			}, true
		},
		plegHealth:       true,
		streamingRuntime: streamingServer,
	}
	fw.fakeAuth = &fakeAuth{
		authenticateFunc: func(req *http.Request) (*authenticator.Response, bool, error) {
			return &authenticator.Response{User: &user.DefaultInfo{Name: "test"}}, true, nil
		},
		attributesFunc: func(u user.Info, req *http.Request) []authorizer.Attributes {
			return []authorizer.Attributes{&authorizer.AttributesRecord{User: u}}
		},
		authorizeFunc: func(a authorizer.Attributes) (decision authorizer.Decision, reason string, err error) {
			return authorizer.DecisionAllow, "", nil
		},
	}
	server := NewServer(
		fw.fakeKubelet,
		stats.NewResourceAnalyzer(fw.fakeKubelet, time.Minute, &record.FakeRecorder{}),
		[]healthz.HealthChecker{},
		flagz.NamedFlagSetsReader{},
		fw.fakeAuth,
		kubeCfg,
	)
	fw.serverUnderTest = &server
	fw.testHTTPServer = httptest.NewServer(fw.serverUnderTest)
	return fw
}

// A helper function to return the correct pod name.
func getPodName(name, namespace string) string {
	if namespace == "" {
		namespace = metav1.NamespaceDefault
	}
	return name + "_" + namespace
}

func TestServeLogs(t *testing.T) {
	fw := newServerTest()
	defer fw.testHTTPServer.Close()

	content := string(`<pre><a href="kubelet.log">kubelet.log</a><a href="google.log">google.log</a></pre>`)

	fw.fakeKubelet.logFunc = func(w http.ResponseWriter, req *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Header().Add("Content-Type", "text/html")
		w.Write([]byte(content))
	}

	resp, err := http.Get(fw.testHTTPServer.URL + "/logs/")
	if err != nil {
		t.Fatalf("Got error GETing: %v", err)
	}
	defer resp.Body.Close()

	body, err := httputil.DumpResponse(resp, true)
	if err != nil {
		// copying the response body did not work
		t.Errorf("Cannot copy resp: %#v", err)
	}
	result := string(body)
	if !strings.Contains(result, "kubelet.log") || !strings.Contains(result, "google.log") {
		t.Errorf("Received wrong data: %s", result)
	}
}

func TestServeRunInContainer(t *testing.T) {
	fw := newServerTest()
	defer fw.testHTTPServer.Close()
	output := "foo bar"
	podNamespace := "other"
	podName := "foo"
	expectedPodName := getPodName(podName, podNamespace)
	expectedContainerName := "baz"
	expectedCommand := "ls -a"
	fw.fakeKubelet.runFunc = func(podFullName string, uid types.UID, containerName string, cmd []string) ([]byte, error) {
		if podFullName != expectedPodName {
			t.Errorf("expected %s, got %s", expectedPodName, podFullName)
		}
		if containerName != expectedContainerName {
			t.Errorf("expected %s, got %s", expectedContainerName, containerName)
		}
		if strings.Join(cmd, " ") != expectedCommand {
			t.Errorf("expected: %s, got %v", expectedCommand, cmd)
		}

		return []byte(output), nil
	}

	resp, err := http.Post(fw.testHTTPServer.URL+"/run/"+podNamespace+"/"+podName+"/"+expectedContainerName+"?cmd=ls%20-a", "", nil)

	if err != nil {
		t.Fatalf("Got error POSTing: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		// copying the response body did not work
		t.Errorf("Cannot copy resp: %#v", err)
	}
	result := string(body)
	if result != output {
		t.Errorf("expected %s, got %s", output, result)
	}
}

func TestServeRunInContainerWithUID(t *testing.T) {
	fw := newServerTest()
	defer fw.testHTTPServer.Close()
	output := "foo bar"
	podNamespace := "other"
	podName := "foo"
	expectedPodName := getPodName(podName, podNamespace)
	expectedContainerName := "baz"
	expectedCommand := "ls -a"
	fw.fakeKubelet.runFunc = func(podFullName string, uid types.UID, containerName string, cmd []string) ([]byte, error) {
		if podFullName != expectedPodName {
			t.Errorf("expected %s, got %s", expectedPodName, podFullName)
		}
		if string(uid) != testUID {
			t.Errorf("expected %s, got %s", testUID, uid)
		}
		if containerName != expectedContainerName {
			t.Errorf("expected %s, got %s", expectedContainerName, containerName)
		}
		if strings.Join(cmd, " ") != expectedCommand {
			t.Errorf("expected: %s, got %v", expectedCommand, cmd)
		}

		return []byte(output), nil
	}

	resp, err := http.Post(fw.testHTTPServer.URL+"/run/"+podNamespace+"/"+podName+"/"+testUID+"/"+expectedContainerName+"?cmd=ls%20-a", "", nil)
	if err != nil {
		t.Fatalf("Got error POSTing: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		// copying the response body did not work
		t.Errorf("Cannot copy resp: %#v", err)
	}
	result := string(body)
	if result != output {
		t.Errorf("expected %s, got %s", output, result)
	}
}

func TestHealthCheck(t *testing.T) {
	fw := newServerTest()
	defer fw.testHTTPServer.Close()
	fw.fakeKubelet.hostnameFunc = func() string {
		return "127.0.0.1"
	}

	// Test with correct hostname, Docker version
	assertHealthIsOk(t, fw.testHTTPServer.URL+"/healthz")

	// Test with incorrect hostname
	fw.fakeKubelet.hostnameFunc = func() string {
		return "fake"
	}
	assertHealthIsOk(t, fw.testHTTPServer.URL+"/healthz")
}

func assertHealthFails(t *testing.T, httpURL string, expectedErrorCode int) {
	resp, err := http.Get(httpURL)
	if err != nil {
		t.Fatalf("Got error GETing: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != expectedErrorCode {
		t.Errorf("expected status code %d, got %d", expectedErrorCode, resp.StatusCode)
	}
}

// Ensure all registered handlers & services have an associated testcase.
func TestAuthzCoverage(t *testing.T) {
	fw := newServerTest()
	defer fw.testHTTPServer.Close()

	for _, fineGrained := range []bool{false, true} {
		t.Run(fmt.Sprintf("fineGrained=%v", fineGrained), func(t *testing.T) {
			featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.KubeletFineGrainedAuthz, fineGrained)
			// method:path -> has coverage
			expectedCases := map[string]bool{}

			// Test all the non-web-service handlers
			for _, path := range fw.serverUnderTest.restfulCont.RegisteredHandlePaths() {
				expectedCases["GET:"+path] = false
				expectedCases["POST:"+path] = false
			}

			// Test all the generated web-service paths
			for _, ws := range fw.serverUnderTest.restfulCont.RegisteredWebServices() {
				for _, r := range ws.Routes() {
					expectedCases[r.Method+":"+r.Path] = false
				}
			}

			// This is a sanity check that the Handle->HandleWithFilter() delegation is working
			// Ideally, these would move to registered web services and this list would get shorter
			expectedPaths := []string{"/healthz", "/metrics", "/metrics/cadvisor"}
			for _, expectedPath := range expectedPaths {
				if _, expected := expectedCases["GET:"+expectedPath]; !expected {
					t.Errorf("Expected registered handle path %s was missing", expectedPath)
				}
			}

			for _, tc := range AuthzTestCases(fineGrained) {
				expectedCases[tc.Method+":"+tc.Path] = true
			}

			for tc, found := range expectedCases {
				if !found {
					t.Errorf("Missing authz test case for %s", tc)
				}
			}
		})
	}
}

func TestAuthFilters(t *testing.T) {
	// Enable features.ContainerCheckpoint during test
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.ContainerCheckpoint, true)
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, zpagesfeatures.ComponentStatusz, true)
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, zpagesfeatures.ComponentFlagz, true)

	fw := newServerTest()
	defer fw.testHTTPServer.Close()

	attributesGetter := NewNodeAuthorizerAttributesGetter(authzTestNodeName)

	for _, fineGraned := range []bool{false, true} {
		featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.KubeletFineGrainedAuthz, fineGraned)
		for _, tc := range AuthzTestCases(fineGraned) {
			t.Run(fmt.Sprintf("method=%v:path=%v:fineGrained=%v", tc.Method, tc.Method, fineGraned), func(t *testing.T) {
				var (
					expectedUser = AuthzTestUser()

					calledAuthenticate = false
					calledAuthorize    = false
					calledAttributes   = false
				)

				fw.fakeAuth.authenticateFunc = func(req *http.Request) (*authenticator.Response, bool, error) {
					calledAuthenticate = true
					return &authenticator.Response{User: expectedUser}, true, nil
				}
				fw.fakeAuth.attributesFunc = func(u user.Info, req *http.Request) []authorizer.Attributes {
					calledAttributes = true
					require.Equal(t, expectedUser, u)
					attrs := attributesGetter.GetRequestAttributes(u, req)
					tc.AssertAttributes(t, attrs)
					return attrs
				}
				fw.fakeAuth.authorizeFunc = func(a authorizer.Attributes) (decision authorizer.Decision, reason string, err error) {
					calledAuthorize = true
					return authorizer.DecisionNoOpinion, "", nil
				}

				req, err := http.NewRequest(tc.Method, fw.testHTTPServer.URL+tc.Path, nil)
				require.NoError(t, err)

				resp, err := http.DefaultClient.Do(req)
				require.NoError(t, err)
				defer resp.Body.Close() //nolint:errcheck

				assert.Equal(t, http.StatusForbidden, resp.StatusCode)
				assert.True(t, calledAuthenticate, "Authenticate was not called")
				assert.True(t, calledAttributes, "Attributes were not called")
				assert.True(t, calledAuthorize, "Authorize was not called")
			})
		}
	}
}

func TestAuthenticationError(t *testing.T) {
	var (
		expectedUser       = &user.DefaultInfo{Name: "test"}
		expectedAttributes = []authorizer.Attributes{&authorizer.AttributesRecord{User: expectedUser}}

		calledAuthenticate = false
		calledAuthorize    = false
		calledAttributes   = false
	)

	fw := newServerTest()
	defer fw.testHTTPServer.Close()
	fw.fakeAuth.authenticateFunc = func(req *http.Request) (*authenticator.Response, bool, error) {
		calledAuthenticate = true
		return &authenticator.Response{User: expectedUser}, true, nil
	}
	fw.fakeAuth.attributesFunc = func(u user.Info, req *http.Request) []authorizer.Attributes {
		calledAttributes = true
		return expectedAttributes
	}
	fw.fakeAuth.authorizeFunc = func(a authorizer.Attributes) (decision authorizer.Decision, reason string, err error) {
		calledAuthorize = true
		return authorizer.DecisionNoOpinion, "", errors.New("Failed")
	}

	assertHealthFails(t, fw.testHTTPServer.URL+"/healthz", http.StatusInternalServerError)

	if !calledAuthenticate {
		t.Fatalf("Authenticate was not called")
	}
	if !calledAttributes {
		t.Fatalf("Attributes was not called")
	}
	if !calledAuthorize {
		t.Fatalf("Authorize was not called")
	}
}

func TestAuthenticationFailure(t *testing.T) {
	var (
		expectedUser       = &user.DefaultInfo{Name: "test"}
		expectedAttributes = []authorizer.Attributes{&authorizer.AttributesRecord{User: expectedUser}}

		calledAuthenticate = false
		calledAuthorize    = false
		calledAttributes   = false
	)

	fw := newServerTest()
	defer fw.testHTTPServer.Close()
	fw.fakeAuth.authenticateFunc = func(req *http.Request) (*authenticator.Response, bool, error) {
		calledAuthenticate = true
		return nil, false, nil
	}
	fw.fakeAuth.attributesFunc = func(u user.Info, req *http.Request) []authorizer.Attributes {
		calledAttributes = true
		return expectedAttributes
	}
	fw.fakeAuth.authorizeFunc = func(a authorizer.Attributes) (decision authorizer.Decision, reason string, err error) {
		calledAuthorize = true
		return authorizer.DecisionNoOpinion, "", nil
	}

	assertHealthFails(t, fw.testHTTPServer.URL+"/healthz", http.StatusUnauthorized)

	if !calledAuthenticate {
		t.Fatalf("Authenticate was not called")
	}
	if calledAttributes {
		t.Fatalf("Attributes was called unexpectedly")
	}
	if calledAuthorize {
		t.Fatalf("Authorize was called unexpectedly")
	}
}

func TestAuthorizationSuccess(t *testing.T) {
	var (
		expectedUser       = &user.DefaultInfo{Name: "test"}
		expectedAttributes = []authorizer.Attributes{&authorizer.AttributesRecord{User: expectedUser}}

		calledAuthenticate = false
		calledAuthorize    = false
		calledAttributes   = false
	)

	fw := newServerTest()
	defer fw.testHTTPServer.Close()
	fw.fakeAuth.authenticateFunc = func(req *http.Request) (*authenticator.Response, bool, error) {
		calledAuthenticate = true
		return &authenticator.Response{User: expectedUser}, true, nil
	}
	fw.fakeAuth.attributesFunc = func(u user.Info, req *http.Request) []authorizer.Attributes {
		calledAttributes = true
		return expectedAttributes
	}
	fw.fakeAuth.authorizeFunc = func(a authorizer.Attributes) (decision authorizer.Decision, reason string, err error) {
		calledAuthorize = true
		return authorizer.DecisionAllow, "", nil
	}

	assertHealthIsOk(t, fw.testHTTPServer.URL+"/healthz")

	if !calledAuthenticate {
		t.Fatalf("Authenticate was not called")
	}
	if !calledAttributes {
		t.Fatalf("Attributes were not called")
	}
	if !calledAuthorize {
		t.Fatalf("Authorize was not called")
	}
}

func TestSyncLoopCheck(t *testing.T) {
	fw := newServerTest()
	defer fw.testHTTPServer.Close()
	fw.fakeKubelet.hostnameFunc = func() string {
		return "127.0.0.1"
	}

	fw.fakeKubelet.resyncInterval = time.Minute
	fw.fakeKubelet.loopEntryTime = time.Now()

	// Test with correct hostname, Docker version
	assertHealthIsOk(t, fw.testHTTPServer.URL+"/healthz")

	fw.fakeKubelet.loopEntryTime = time.Now().Add(time.Minute * -10)
	assertHealthFails(t, fw.testHTTPServer.URL+"/healthz", http.StatusInternalServerError)
}

// returns http response status code from the HTTP GET
func assertHealthIsOk(t *testing.T, httpURL string) {
	resp, err := http.Get(httpURL)
	if err != nil {
		t.Fatalf("Got error GETing: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected status code %d, got %d", http.StatusOK, resp.StatusCode)
	}
	body, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		// copying the response body did not work
		t.Fatalf("Cannot copy resp: %#v", readErr)
	}
	result := string(body)
	if !strings.Contains(result, "ok") {
		t.Errorf("expected body contains ok, got %s", result)
	}
}

func setPodByNameFunc(fw *serverTestFramework, namespace, pod, container string) {
	fw.fakeKubelet.podByNameFunc = func(namespace, name string) (*v1.Pod, bool) {
		return &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: namespace,
				Name:      pod,
			},
			Spec: v1.PodSpec{
				Containers: []v1.Container{
					{
						Name: container,
					},
				},
			},
		}, true
	}
}

func setGetContainerLogsFunc(fw *serverTestFramework, t *testing.T, expectedPodName, expectedContainerName string, expectedLogOptions *v1.PodLogOptions, output string) {
	fw.fakeKubelet.containerLogsFunc = func(_ context.Context, podFullName, containerName string, logOptions *v1.PodLogOptions, stdout, stderr io.Writer) error {
		if podFullName != expectedPodName {
			t.Errorf("expected %s, got %s", expectedPodName, podFullName)
		}
		if containerName != expectedContainerName {
			t.Errorf("expected %s, got %s", expectedContainerName, containerName)
		}
		if !reflect.DeepEqual(expectedLogOptions, logOptions) {
			t.Errorf("expected %#v, got %#v", expectedLogOptions, logOptions)
		}

		io.WriteString(stdout, output)
		return nil
	}
}

func TestContainerLogs(t *testing.T) {
	fw := newServerTest()
	defer fw.testHTTPServer.Close()

	tests := map[string]struct {
		query        string
		podLogOption *v1.PodLogOptions
	}{
		"without tail":     {"", &v1.PodLogOptions{}},
		"with tail":        {"?tailLines=5", &v1.PodLogOptions{TailLines: ptr.To[int64](5)}},
		"with legacy tail": {"?tail=5", &v1.PodLogOptions{TailLines: ptr.To[int64](5)}},
		"with tail all":    {"?tail=all", &v1.PodLogOptions{}},
		"with follow":      {"?follow=1", &v1.PodLogOptions{Follow: true}},
	}

	for desc, test := range tests {
		// To make sure the original behavior doesn't change no matter the feature PodLogsQuerySplitStreams is enabled or not.
		for _, enablePodLogsQuerySplitStreams := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s (enablePodLogsQuerySplitStreams=%v)", desc, enablePodLogsQuerySplitStreams), func(t *testing.T) {
				featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.PodLogsQuerySplitStreams, enablePodLogsQuerySplitStreams)
				expectedLogOptions := test.podLogOption.DeepCopy()
				if enablePodLogsQuerySplitStreams && expectedLogOptions.Stream == nil {
					// The HTTP handler will internally set the default stream value.
					expectedLogOptions.Stream = ptr.To(v1.LogStreamAll)
				}

				output := "foo bar"
				podNamespace := "other"
				podName := "foo"
				expectedPodName := getPodName(podName, podNamespace)
				expectedContainerName := "baz"
				setPodByNameFunc(fw, podNamespace, podName, expectedContainerName)
				setGetContainerLogsFunc(fw, t, expectedPodName, expectedContainerName, expectedLogOptions, output)
				resp, err := http.Get(fw.testHTTPServer.URL + "/containerLogs/" + podNamespace + "/" + podName + "/" + expectedContainerName + test.query)
				if err != nil {
					t.Errorf("Got error GETing: %v", err)
				}
				defer func() {
					_ = resp.Body.Close()
				}()

				body, err := io.ReadAll(resp.Body)
				if err != nil {
					t.Errorf("Error reading container logs: %v", err)
				}
				result := string(body)
				if result != output {
					t.Errorf("Expected: '%v', got: '%v'", output, result)
				}
			})
		}
	}
}

func TestContainerLogsWithInvalidTail(t *testing.T) {
	fw := newServerTest()
	defer fw.testHTTPServer.Close()
	output := "foo bar"
	podNamespace := "other"
	podName := "foo"
	expectedPodName := getPodName(podName, podNamespace)
	expectedContainerName := "baz"
	setPodByNameFunc(fw, podNamespace, podName, expectedContainerName)
	setGetContainerLogsFunc(fw, t, expectedPodName, expectedContainerName, &v1.PodLogOptions{}, output)
	resp, err := http.Get(fw.testHTTPServer.URL + "/containerLogs/" + podNamespace + "/" + podName + "/" + expectedContainerName + "?tail=-1")
	if err != nil {
		t.Errorf("Got error GETing: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("Unexpected non-error reading container logs: %#v", resp)
	}
}

func TestContainerLogsWithSeparateStream(t *testing.T) {
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.PodLogsQuerySplitStreams, true)

	type logEntry struct {
		stream string
		msg    string
	}

	fw := newServerTest()
	defer fw.testHTTPServer.Close()

	var (
		streamStdout = v1.LogStreamStdout
		streamStderr = v1.LogStreamStderr
		streamAll    = v1.LogStreamAll
	)

	testCases := []struct {
		name               string
		query              string
		logs               []logEntry
		expectedOutput     string
		expectedLogOptions *v1.PodLogOptions
	}{
		{
			// Defaulters don't work if the query is empty.
			// See also https://github.com/kubernetes/kubernetes/issues/128589
			name: "empty query should return all logs",
			logs: []logEntry{
				{stream: v1.LogStreamStdout, msg: "foo\n"},
				{stream: v1.LogStreamStderr, msg: "bar\n"},
			},
			query: "",
			expectedLogOptions: &v1.PodLogOptions{
				Stream: &streamAll,
			},
			expectedOutput: "foo\nbar\n",
		},
		{
			name: "missing stream param should return all logs",
			logs: []logEntry{
				{stream: v1.LogStreamStdout, msg: "foo\n"},
				{stream: v1.LogStreamStderr, msg: "bar\n"},
			},
			query: "?limitBytes=100",
			expectedLogOptions: &v1.PodLogOptions{
				Stream:     &streamAll,
				LimitBytes: ptr.To[int64](100),
			},
			expectedOutput: "foo\nbar\n",
		},
		{
			name: "only stdout logs",
			logs: []logEntry{
				{stream: v1.LogStreamStdout, msg: "out1\n"},
				{stream: v1.LogStreamStderr, msg: "err1\n"},
				{stream: v1.LogStreamStdout, msg: "out2\n"},
			},
			query: "?stream=Stdout",
			expectedLogOptions: &v1.PodLogOptions{
				Stream: &streamStdout,
			},
			expectedOutput: "out1\nout2\n",
		},
		{
			name: "only stderr logs",
			logs: []logEntry{
				{stream: v1.LogStreamStderr, msg: "err1\n"},
				{stream: v1.LogStreamStderr, msg: "err2\n"},
				{stream: v1.LogStreamStdout, msg: "out1\n"},
			},
			query: "?stream=Stderr",
			expectedLogOptions: &v1.PodLogOptions{
				Stream: &streamStderr,
			},
			expectedOutput: "err1\nerr2\n",
		},
		{
			name: "return all logs",
			logs: []logEntry{
				{stream: v1.LogStreamStdout, msg: "out1\n"},
				{stream: v1.LogStreamStderr, msg: "err1\n"},
				{stream: v1.LogStreamStdout, msg: "out2\n"},
			},
			query: "?stream=All",
			expectedLogOptions: &v1.PodLogOptions{
				Stream: &streamAll,
			},
			expectedOutput: "out1\nerr1\nout2\n",
		},
		{
			name: "stdout logs with legacy tail",
			logs: []logEntry{
				{stream: v1.LogStreamStdout, msg: "out1\n"},
				{stream: v1.LogStreamStderr, msg: "err1\n"},
				{stream: v1.LogStreamStdout, msg: "out2\n"},
			},
			query: "?stream=All&tail=1",
			expectedLogOptions: &v1.PodLogOptions{
				Stream:    &streamAll,
				TailLines: ptr.To[int64](1),
			},
			expectedOutput: "out2\n",
		},
		{
			name: "return the last 2 lines of logs",
			logs: []logEntry{
				{stream: v1.LogStreamStdout, msg: "out1\n"},
				{stream: v1.LogStreamStderr, msg: "err1\n"},
				{stream: v1.LogStreamStdout, msg: "out2\n"},
			},
			query: "?stream=All&tailLines=2",
			expectedLogOptions: &v1.PodLogOptions{
				Stream:    &streamAll,
				TailLines: ptr.To[int64](2),
			},
			expectedOutput: "err1\nout2\n",
		},
		{
			name: "return the first 6 bytes of the stdout log stream",
			logs: []logEntry{
				{stream: v1.LogStreamStderr, msg: "err1\n"},
				{stream: v1.LogStreamStdout, msg: "out1\n"},
				{stream: v1.LogStreamStderr, msg: "err2\n"},
				{stream: v1.LogStreamStdout, msg: "out2\n"},
			},
			query: "?stream=Stdout&limitBytes=6",
			expectedLogOptions: &v1.PodLogOptions{
				Stream:     &streamStdout,
				LimitBytes: ptr.To[int64](6),
			},
			expectedOutput: "out1\no",
		},
		{
			name: "invalid stream",
			logs: []logEntry{
				{stream: v1.LogStreamStderr, msg: "err1\n"},
				{stream: v1.LogStreamStdout, msg: "out1\n"},
			},
			query:              "?stream=invalid",
			expectedLogOptions: nil,
			expectedOutput:     `{"message": "Invalid request."}`,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			podNamespace := "other"
			podName := "foo"
			expectedContainerName := "baz"
			setPodByNameFunc(fw, podNamespace, podName, expectedContainerName)
			fw.fakeKubelet.containerLogsFunc = func(_ context.Context, podFullName, containerName string, logOptions *v1.PodLogOptions, stdout, stderr io.Writer) error {
				if !reflect.DeepEqual(tc.expectedLogOptions, logOptions) {
					t.Errorf("expected %#v, got %#v", tc.expectedLogOptions, logOptions)
				}

				var dst io.Writer
				tailLines := len(tc.logs)
				if logOptions.TailLines != nil {
					tailLines = int(*logOptions.TailLines)
				}

				remain := 0
				if logOptions.LimitBytes != nil {
					remain = int(*logOptions.LimitBytes)
				} else {
					for _, log := range tc.logs {
						remain += len(log.msg)
					}
				}

				logs := tc.logs[len(tc.logs)-tailLines:]
				for _, log := range logs {
					switch log.stream {
					case v1.LogStreamStdout:
						dst = stdout
					case v1.LogStreamStderr:
						dst = stderr
					}
					// Skip if the stream is not requested
					if dst == nil {
						continue
					}
					line := log.msg
					if len(line) > remain {
						line = line[:remain]
					}
					_, _ = io.WriteString(dst, line)
					remain -= len(line)
					if remain <= 0 {
						return nil
					}
				}
				return nil
			}
			resp, err := http.Get(fw.testHTTPServer.URL + "/containerLogs/" + podNamespace + "/" + podName + "/" + expectedContainerName + tc.query)
			if err != nil {
				t.Errorf("Got error GETing: %v", err)
			}
			defer func() {
				_ = resp.Body.Close()
			}()

			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Errorf("Error reading container logs: %v", err)
			}
			result := string(body)
			if result != tc.expectedOutput {
				t.Errorf("Expected: %q, got: %q", tc.expectedOutput, result)
			}
		})
	}
}

func TestCheckpointContainer(t *testing.T) {
	podNamespace := "other"
	podName := "foo"
	expectedContainerName := "baz"

	setupTest := func(featureGate bool) *serverTestFramework {
		// Enable features.ContainerCheckpoint during test
		featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.ContainerCheckpoint, featureGate)

		fw := newServerTest()
		// GetPodByName() should always fail
		fw.fakeKubelet.podByNameFunc = func(namespace, name string) (*v1.Pod, bool) {
			return nil, false
		}
		return fw
	}
	fw := setupTest(true)
	defer fw.testHTTPServer.Close()

	t.Run("wrong pod namespace", func(t *testing.T) {
		resp, err := http.Post(fw.testHTTPServer.URL+"/checkpoint/"+podNamespace+"/"+podName+"/"+expectedContainerName, "", nil)
		if err != nil {
			t.Errorf("Got error POSTing: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("Unexpected non-error checkpointing container: %#v", resp)
		}
	})
	// let GetPodByName() return a result, but our container "wrongContainerName" is not part of the Pod
	setPodByNameFunc(fw, podNamespace, podName, expectedContainerName)
	t.Run("wrong container name", func(t *testing.T) {
		resp, err := http.Post(fw.testHTTPServer.URL+"/checkpoint/"+podNamespace+"/"+podName+"/wrongContainerName", "", nil)
		if err != nil {
			t.Errorf("Got error POSTing: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("Unexpected non-error checkpointing container: %#v", resp)
		}
	})
	// Now the checkpointing of the container fails
	fw.fakeKubelet.podByNameFunc = func(namespace, name string) (*v1.Pod, bool) {
		return &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: podNamespace,
				Name:      podName,
			},
			Spec: v1.PodSpec{
				Containers: []v1.Container{
					{
						Name: "checkpointingFailure",
					},
				},
			},
		}, true
	}
	t.Run("checkpointing fails", func(t *testing.T) {
		resp, err := http.Post(fw.testHTTPServer.URL+"/checkpoint/"+podNamespace+"/"+podName+"/checkpointingFailure", "", nil)
		if err != nil {
			t.Errorf("Got error POSTing: %v", err)
		}
		defer resp.Body.Close()
		assert.Equal(t, 500, resp.StatusCode)
		body, _ := io.ReadAll(resp.Body)
		assert.Equal(t, "checkpointing of other/foo/checkpointingFailure failed (Returning error for test)", string(body))
	})
	// Now test a successful checkpoint succeeds
	setPodByNameFunc(fw, podNamespace, podName, expectedContainerName)
	t.Run("checkpointing succeeds", func(t *testing.T) {
		resp, err := http.Post(fw.testHTTPServer.URL+"/checkpoint/"+podNamespace+"/"+podName+"/"+expectedContainerName, "", nil)
		if err != nil {
			t.Errorf("Got error POSTing: %v", err)
		}
		assert.Equal(t, 200, resp.StatusCode)
	})

	// Now test for 404 if checkpointing support is explicitly disabled.
	fw.testHTTPServer.Close()
	fw = setupTest(false)
	defer fw.testHTTPServer.Close()
	setPodByNameFunc(fw, podNamespace, podName, expectedContainerName)
	t.Run("checkpointing fails because disabled", func(t *testing.T) {
		resp, err := http.Post(fw.testHTTPServer.URL+"/checkpoint/"+podNamespace+"/"+podName+"/"+expectedContainerName, "", nil)
		if err != nil {
			t.Errorf("Got error POSTing: %v", err)
		}
		assert.Equal(t, 404, resp.StatusCode)
	})
}

func makeReq(t *testing.T, method, url, clientProtocol string) *http.Request {
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatalf("error creating request: %v", err)
	}
	req.Header.Set("Content-Type", "")
	req.Header.Add("X-Stream-Protocol-Version", clientProtocol)
	return req
}

func TestServeExecInContainerIdleTimeout(t *testing.T) {
	ss, err := newTestStreamingServer(100 * time.Millisecond)
	require.NoError(t, err)
	defer ss.testHTTPServer.Close()
	fw := newServerTestWithDebug(true, ss)
	defer fw.testHTTPServer.Close()

	podNamespace := "other"
	podName := "foo"
	expectedContainerName := "baz"

	url := fw.testHTTPServer.URL + "/exec/" + podNamespace + "/" + podName + "/" + expectedContainerName + "?c=ls&c=-a&" + api.ExecStdinParam + "=1"

	upgradeRoundTripper, err := spdy.NewRoundTripper(&tls.Config{})
	if err != nil {
		t.Fatalf("Error creating SpdyRoundTripper: %v", err)
	}
	c := &http.Client{Transport: upgradeRoundTripper}

	resp, err := c.Do(makeReq(t, "POST", url, "v4.channel.k8s.io"))
	if err != nil {
		t.Fatalf("Got error POSTing: %v", err)
	}
	defer resp.Body.Close()

	upgradeRoundTripper.Dialer = &net.Dialer{
		Deadline: time.Now().Add(60 * time.Second),
		Timeout:  60 * time.Second,
	}
	conn, err := upgradeRoundTripper.NewConnection(resp)
	if err != nil {
		t.Fatalf("Unexpected error creating streaming connection: %s", err)
	}
	if conn == nil {
		t.Fatal("Unexpected nil connection")
	}

	<-conn.CloseChan()
}

func testExecAttach(t *testing.T, verb string) {
	tests := map[string]struct {
		stdin              bool
		stdout             bool
		stderr             bool
		tty                bool
		responseStatusCode int
		uid                bool
	}{
		"no input or output":           {responseStatusCode: http.StatusBadRequest},
		"stdin":                        {stdin: true, responseStatusCode: http.StatusSwitchingProtocols},
		"stdout":                       {stdout: true, responseStatusCode: http.StatusSwitchingProtocols},
		"stderr":                       {stderr: true, responseStatusCode: http.StatusSwitchingProtocols},
		"stdout and stderr":            {stdout: true, stderr: true, responseStatusCode: http.StatusSwitchingProtocols},
		"stdin stdout and stderr":      {stdin: true, stdout: true, stderr: true, responseStatusCode: http.StatusSwitchingProtocols},
		"stdin stdout stderr with uid": {stdin: true, stdout: true, stderr: true, responseStatusCode: http.StatusSwitchingProtocols, uid: true},
	}

	for desc := range tests {
		test := tests[desc]
		t.Run(desc, func(t *testing.T) {
			ss, err := newTestStreamingServer(0)
			require.NoError(t, err)
			defer ss.testHTTPServer.Close()
			fw := newServerTestWithDebug(true, ss)
			defer fw.testHTTPServer.Close()
			fmt.Println(desc)

			podNamespace := "other"
			podName := "foo"
			expectedPodName := getPodName(podName, podNamespace)
			expectedContainerName := "baz"
			expectedCommand := "ls -a"
			expectedStdin := "stdin"
			expectedStdout := "stdout"
			expectedStderr := "stderr"
			done := make(chan struct{})
			clientStdoutReadDone := make(chan struct{})
			clientStderrReadDone := make(chan struct{})
			execInvoked := false
			attachInvoked := false

			checkStream := func(podFullName string, uid types.UID, containerName string, streamOpts remotecommandserver.Options) {
				assert.Equal(t, expectedPodName, podFullName, "podFullName")
				if test.uid {
					assert.Equal(t, testUID, string(uid), "uid")
				}
				assert.Equal(t, expectedContainerName, containerName, "containerName")
				assert.Equal(t, test.stdin, streamOpts.Stdin, "stdin")
				assert.Equal(t, test.stdout, streamOpts.Stdout, "stdout")
				assert.Equal(t, test.tty, streamOpts.TTY, "tty")
				assert.Equal(t, !test.tty && test.stderr, streamOpts.Stderr, "stderr")
			}

			fw.fakeKubelet.getExecCheck = func(podFullName string, uid types.UID, containerName string, cmd []string, streamOpts remotecommandserver.Options) {
				execInvoked = true
				assert.Equal(t, expectedCommand, strings.Join(cmd, " "), "cmd")
				checkStream(podFullName, uid, containerName, streamOpts)
			}

			fw.fakeKubelet.getAttachCheck = func(podFullName string, uid types.UID, containerName string, streamOpts remotecommandserver.Options) {
				attachInvoked = true
				checkStream(podFullName, uid, containerName, streamOpts)
			}

			testStream := func(containerID string, in io.Reader, out, stderr io.WriteCloser, tty bool, done chan struct{}) error {
				close(done)
				assert.Equal(t, testContainerID, containerID, "containerID")
				assert.Equal(t, test.tty, tty, "tty")
				require.Equal(t, test.stdin, in != nil, "in")
				require.Equal(t, test.stdout, out != nil, "out")
				require.Equal(t, !test.tty && test.stderr, stderr != nil, "err")

				if test.stdin {
					b := make([]byte, 10)
					n, err := in.Read(b)
					assert.NoError(t, err, "reading from stdin")
					assert.Equal(t, expectedStdin, string(b[0:n]), "content from stdin")
				}

				if test.stdout {
					_, err := out.Write([]byte(expectedStdout))
					assert.NoError(t, err, "writing to stdout")
					out.Close()
					<-clientStdoutReadDone
				}

				if !test.tty && test.stderr {
					_, err := stderr.Write([]byte(expectedStderr))
					assert.NoError(t, err, "writing to stderr")
					stderr.Close()
					<-clientStderrReadDone
				}
				return nil
			}

			ss.fakeRuntime.execFunc = func(containerID string, cmd []string, stdin io.Reader, stdout, stderr io.WriteCloser, tty bool, resize <-chan remotecommand.TerminalSize) error {
				assert.Equal(t, expectedCommand, strings.Join(cmd, " "), "cmd")
				return testStream(containerID, stdin, stdout, stderr, tty, done)
			}

			ss.fakeRuntime.attachFunc = func(containerID string, stdin io.Reader, stdout, stderr io.WriteCloser, tty bool, resize <-chan remotecommand.TerminalSize) error {
				return testStream(containerID, stdin, stdout, stderr, tty, done)
			}

			var url string
			if test.uid {
				url = fw.testHTTPServer.URL + "/" + verb + "/" + podNamespace + "/" + podName + "/" + testUID + "/" + expectedContainerName + "?ignore=1"
			} else {
				url = fw.testHTTPServer.URL + "/" + verb + "/" + podNamespace + "/" + podName + "/" + expectedContainerName + "?ignore=1"
			}
			if verb == "exec" {
				url += "&command=ls&command=-a"
			}
			if test.stdin {
				url += "&" + api.ExecStdinParam + "=1"
			}
			if test.stdout {
				url += "&" + api.ExecStdoutParam + "=1"
			}
			if test.stderr && !test.tty {
				url += "&" + api.ExecStderrParam + "=1"
			}
			if test.tty {
				url += "&" + api.ExecTTYParam + "=1"
			}

			var (
				resp                *http.Response
				upgradeRoundTripper httpstream.UpgradeRoundTripper
				c                   *http.Client
			)
			upgradeRoundTripper, err = spdy.NewRoundTripper(&tls.Config{})
			if err != nil {
				t.Fatalf("Error creating SpdyRoundTripper: %v", err)
			}
			c = &http.Client{Transport: upgradeRoundTripper}

			resp, err = c.Do(makeReq(t, "POST", url, "v4.channel.k8s.io"))
			require.NoError(t, err, "POSTing")
			defer resp.Body.Close()

			_, err = io.ReadAll(resp.Body)
			assert.NoError(t, err, "reading response body")

			require.Equal(t, test.responseStatusCode, resp.StatusCode, "response status")
			if test.responseStatusCode != http.StatusSwitchingProtocols {
				return
			}

			conn, err := upgradeRoundTripper.NewConnection(resp)
			require.NoError(t, err, "creating streaming connection")
			defer conn.Close()

			h := http.Header{}
			h.Set(api.StreamType, api.StreamTypeError)
			_, err = conn.CreateStream(h)
			require.NoError(t, err, "creating error stream")

			if test.stdin {
				h.Set(api.StreamType, api.StreamTypeStdin)
				stream, err := conn.CreateStream(h)
				require.NoError(t, err, "creating stdin stream")
				_, err = stream.Write([]byte(expectedStdin))
				require.NoError(t, err, "writing to stdin stream")
			}

			var stdoutStream httpstream.Stream
			if test.stdout {
				h.Set(api.StreamType, api.StreamTypeStdout)
				stdoutStream, err = conn.CreateStream(h)
				require.NoError(t, err, "creating stdout stream")
			}

			var stderrStream httpstream.Stream
			if test.stderr && !test.tty {
				h.Set(api.StreamType, api.StreamTypeStderr)
				stderrStream, err = conn.CreateStream(h)
				require.NoError(t, err, "creating stderr stream")
			}

			if test.stdout {
				output := make([]byte, 10)
				n, err := stdoutStream.Read(output)
				close(clientStdoutReadDone)
				assert.NoError(t, err, "reading from stdout stream")
				assert.Equal(t, expectedStdout, string(output[0:n]), "stdout")
			}

			if test.stderr && !test.tty {
				output := make([]byte, 10)
				n, err := stderrStream.Read(output)
				close(clientStderrReadDone)
				assert.NoError(t, err, "reading from stderr stream")
				assert.Equal(t, expectedStderr, string(output[0:n]), "stderr")
			}

			// wait for the server to finish before checking if the attach/exec funcs were invoked
			<-done

			if verb == "exec" {
				assert.True(t, execInvoked, "exec should be invoked")
				assert.False(t, attachInvoked, "attach should not be invoked")
			} else {
				assert.True(t, attachInvoked, "attach should be invoked")
				assert.False(t, execInvoked, "exec should not be invoked")
			}
		})
	}
}

func TestServeExecInContainer(t *testing.T) {
	testExecAttach(t, "exec")
}

func TestServeAttachContainer(t *testing.T) {
	testExecAttach(t, "attach")
}

func TestServePortForwardIdleTimeout(t *testing.T) {
	ss, err := newTestStreamingServer(100 * time.Millisecond)
	require.NoError(t, err)
	defer ss.testHTTPServer.Close()
	fw := newServerTestWithDebug(true, ss)
	defer fw.testHTTPServer.Close()

	podNamespace := "other"
	podName := "foo"

	url := fw.testHTTPServer.URL + "/portForward/" + podNamespace + "/" + podName

	upgradeRoundTripper, err := spdy.NewRoundTripper(&tls.Config{})
	if err != nil {
		t.Fatalf("Error creating SpdyRoundTripper: %v", err)
	}
	c := &http.Client{Transport: upgradeRoundTripper}

	req := makeReq(t, "POST", url, "portforward.k8s.io")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("Got error POSTing: %v", err)
	}
	defer resp.Body.Close()

	conn, err := upgradeRoundTripper.NewConnection(resp)
	if err != nil {
		t.Fatalf("Unexpected error creating streaming connection: %s", err)
	}
	if conn == nil {
		t.Fatal("Unexpected nil connection")
	}
	defer conn.Close()

	<-conn.CloseChan()
}

func TestServePortForward(t *testing.T) {
	tests := map[string]struct {
		port          string
		uid           bool
		clientData    string
		containerData string
		shouldError   bool
	}{
		"no port":                       {port: "", shouldError: true},
		"none number port":              {port: "abc", shouldError: true},
		"negative port":                 {port: "-1", shouldError: true},
		"too large port":                {port: "65536", shouldError: true},
		"0 port":                        {port: "0", shouldError: true},
		"min port":                      {port: "1", shouldError: false},
		"normal port":                   {port: "8000", shouldError: false},
		"normal port with data forward": {port: "8000", clientData: "client data", containerData: "container data", shouldError: false},
		"max port":                      {port: "65535", shouldError: false},
		"normal port with uid":          {port: "8000", uid: true, shouldError: false},
	}

	podNamespace := "other"
	podName := "foo"

	for desc := range tests {
		test := tests[desc]
		t.Run(desc, func(t *testing.T) {
			ss, err := newTestStreamingServer(0)
			require.NoError(t, err)
			defer ss.testHTTPServer.Close()
			fw := newServerTestWithDebug(true, ss)
			defer fw.testHTTPServer.Close()

			portForwardFuncDone := make(chan struct{})

			fw.fakeKubelet.getPortForwardCheck = func(name, namespace string, uid types.UID, opts portforward.V4Options) {
				assert.Equal(t, podName, name, "pod name")
				assert.Equal(t, podNamespace, namespace, "pod namespace")
				if test.uid {
					assert.Equal(t, testUID, string(uid), "uid")
				}
			}

			ss.fakeRuntime.portForwardFunc = func(podSandboxID string, port int32, stream io.ReadWriteCloser) error {
				defer close(portForwardFuncDone)
				assert.Equal(t, testPodSandboxID, podSandboxID, "pod sandbox id")
				// The port should be valid if it reaches here.
				testPort, err := strconv.ParseInt(test.port, 10, 32)
				require.NoError(t, err, "parse port")
				assert.Equal(t, int32(testPort), port, "port")

				if test.clientData != "" {
					fromClient := make([]byte, 32)
					n, err := stream.Read(fromClient)
					assert.NoError(t, err, "reading client data")
					assert.Equal(t, test.clientData, string(fromClient[0:n]), "client data")
				}

				if test.containerData != "" {
					_, err := stream.Write([]byte(test.containerData))
					assert.NoError(t, err, "writing container data")
				}

				return nil
			}

			var url string
			if test.uid {
				url = fmt.Sprintf("%s/portForward/%s/%s/%s", fw.testHTTPServer.URL, podNamespace, podName, testUID)
			} else {
				url = fmt.Sprintf("%s/portForward/%s/%s", fw.testHTTPServer.URL, podNamespace, podName)
			}

			var (
				upgradeRoundTripper httpstream.UpgradeRoundTripper
				c                   *http.Client
			)

			upgradeRoundTripper, err = spdy.NewRoundTripper(&tls.Config{})
			if err != nil {
				t.Fatalf("Error creating SpdyRoundTripper: %v", err)
			}
			c = &http.Client{Transport: upgradeRoundTripper}

			req := makeReq(t, "POST", url, "portforward.k8s.io")
			resp, err := c.Do(req)
			require.NoError(t, err, "POSTing")
			defer resp.Body.Close()

			assert.Equal(t, http.StatusSwitchingProtocols, resp.StatusCode, "status code")

			conn, err := upgradeRoundTripper.NewConnection(resp)
			require.NoError(t, err, "creating streaming connection")
			defer conn.Close()

			headers := http.Header{}
			headers.Set("streamType", "error")
			headers.Set("port", test.port)
			_, err = conn.CreateStream(headers)
			assert.Equal(t, test.shouldError, err != nil, "expect error")

			if test.shouldError {
				return
			}

			headers.Set("streamType", "data")
			headers.Set("port", test.port)
			dataStream, err := conn.CreateStream(headers)
			require.NoError(t, err, "create stream")

			if test.clientData != "" {
				_, err := dataStream.Write([]byte(test.clientData))
				assert.NoError(t, err, "writing client data")
			}

			if test.containerData != "" {
				fromContainer := make([]byte, 32)
				n, err := dataStream.Read(fromContainer)
				assert.NoError(t, err, "reading container data")
				assert.Equal(t, test.containerData, string(fromContainer[0:n]), "container data")
			}

			<-portForwardFuncDone
		})
	}
}

func TestMetricBuckets(t *testing.T) {
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, zpagesfeatures.ComponentStatusz, true)

	tests := map[string]struct {
		url    string
		bucket string
	}{
		"healthz endpoint":                {url: "/healthz", bucket: "healthz"},
		"attach":                          {url: "/attach/podNamespace/podID/containerName", bucket: "attach"},
		"attach with uid":                 {url: "/attach/podNamespace/podID/uid/containerName", bucket: "attach"},
		"configz":                         {url: "/configz", bucket: "configz"},
		"containerLogs":                   {url: "/containerLogs/podNamespace/podID/containerName", bucket: "containerLogs"},
		"debug v flags":                   {url: "/debug/flags/v", bucket: "debug"},
		"pprof with sub":                  {url: "/debug/pprof/subpath", bucket: "debug"},
		"exec":                            {url: "/exec/podNamespace/podID/containerName", bucket: "exec"},
		"exec with uid":                   {url: "/exec/podNamespace/podID/uid/containerName", bucket: "exec"},
		"healthz":                         {url: "/healthz/", bucket: "healthz"},
		"healthz log sub":                 {url: "/healthz/log", bucket: "healthz"},
		"healthz ping":                    {url: "/healthz/ping", bucket: "healthz"},
		"healthz sync loop":               {url: "/healthz/syncloop", bucket: "healthz"},
		"logs":                            {url: "/logs/", bucket: "logs"},
		"logs with path":                  {url: "/logs/logpath", bucket: "logs"},
		"metrics":                         {url: "/metrics", bucket: "metrics"},
		"metrics cadvisor sub":            {url: "/metrics/cadvisor", bucket: "metrics/cadvisor"},
		"metrics probes sub":              {url: "/metrics/probes", bucket: "metrics/probes"},
		"metrics resource sub":            {url: "/metrics/resource", bucket: "metrics/resource"},
		"pods":                            {url: "/pods/", bucket: "pods"},
		"portForward":                     {url: "/portForward/podNamespace/podID", bucket: "portForward"},
		"portForward with uid":            {url: "/portForward/podNamespace/podID/uid", bucket: "portForward"},
		"run":                             {url: "/run/podNamespace/podID/containerName", bucket: "run"},
		"run with uid":                    {url: "/run/podNamespace/podID/uid/containerName", bucket: "run"},
		"runningpods":                     {url: "/runningpods/", bucket: "runningpods"},
		"stats":                           {url: "/stats/", bucket: "stats"},
		"stats summary sub":               {url: "/stats/summary", bucket: "stats"},
		"statusz":                         {url: "/statusz", bucket: "statusz"},
		"/flagz":                          {url: "/flagz", bucket: "flagz"},
		"invalid path":                    {url: "/junk", bucket: "other"},
		"invalid path starting with good": {url: "/healthzjunk", bucket: "other"},
	}
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, zpagesfeatures.ComponentFlagz, true)
	fw := newServerTest()
	defer fw.testHTTPServer.Close()

	for _, test := range tests {
		path := test.url
		bucket := test.bucket
		require.Equal(t, fw.serverUnderTest.getMetricBucket(path), bucket)
	}
}

func TestMetricMethodBuckets(t *testing.T) {
	tests := map[string]struct {
		method string
		bucket string
	}{
		"normal GET":     {method: "GET", bucket: "GET"},
		"normal POST":    {method: "POST", bucket: "POST"},
		"invalid method": {method: "WEIRD", bucket: "other"},
	}

	fw := newServerTest()
	defer fw.testHTTPServer.Close()

	for _, test := range tests {
		method := test.method
		bucket := test.bucket
		require.Equal(t, fw.serverUnderTest.getMetricMethodBucket(method), bucket)
	}
}

func TestDebuggingDisabledHandlers(t *testing.T) {
	// for backward compatibility even if enablesystemLogHandler or enableProfilingHandler is set but not
	// enableDebuggingHandler then /logs, /pprof and /flags shouldn't be served.
	kubeCfg := &kubeletconfiginternal.KubeletConfiguration{
		EnableDebuggingHandlers: false,
		EnableSystemLogHandler:  true,
		EnableDebugFlagsHandler: true,
		EnableProfilingHandler:  true,
	}
	fw := newServerTestWithDebuggingHandlers(kubeCfg, nil)
	defer fw.testHTTPServer.Close()

	paths := []string{
		"/run", "/exec", "/attach", "/portForward", "/containerLogs", "/runningpods",
		"/run/", "/exec/", "/attach/", "/portForward/", "/containerLogs/", "/runningpods/",
		"/run/xxx", "/exec/xxx", "/attach/xxx", "/debug/pprof/profile", "/logs/kubelet.log",
	}

	for _, p := range paths {
		verifyEndpointResponse(t, fw, p, "Debug endpoints are disabled.\n")
	}
}

func TestDisablingLogAndProfilingHandler(t *testing.T) {
	kubeCfg := &kubeletconfiginternal.KubeletConfiguration{
		EnableDebuggingHandlers: true,
	}
	fw := newServerTestWithDebuggingHandlers(kubeCfg, nil)
	defer fw.testHTTPServer.Close()

	// verify debug endpoints are disabled
	verifyEndpointResponse(t, fw, "/logs/kubelet.log", "logs endpoint is disabled.\n")
	verifyEndpointResponse(t, fw, "/debug/pprof/profile?seconds=2", "profiling endpoint is disabled.\n")
	verifyEndpointResponse(t, fw, "/debug/flags/v", "flags endpoint is disabled.\n")
}

func TestFailedParseParamsSummaryHandler(t *testing.T) {
	fw := newServerTest()
	defer fw.testHTTPServer.Close()

	resp, err := http.Post(fw.testHTTPServer.URL+"/stats/summary", "invalid/content/type", nil)
	assert.NoError(t, err)
	defer resp.Body.Close()
	v, err := io.ReadAll(resp.Body)
	assert.NoError(t, err)
	assert.Equal(t, http.StatusInternalServerError, resp.StatusCode)
	assert.Contains(t, string(v), "parse form failed")
}

func verifyEndpointResponse(t *testing.T, fw *serverTestFramework, path string, expectedResponse string) {
	resp, err := http.Get(fw.testHTTPServer.URL + path)
	require.NoError(t, err)
	assert.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, expectedResponse, string(body))

	resp, err = http.Post(fw.testHTTPServer.URL+path, "", nil)
	require.NoError(t, err)
	assert.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode)
	body, err = io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, expectedResponse, string(body))
}

func TestTrimURLPath(t *testing.T) {
	tests := []struct {
		path, expected string
	}{
		{"", ""},
		{"//", ""},
		{"/pods", "pods"},
		{"pods", "pods"},
		{"pods/", "pods"},
		{"good/", "good"},
		{"pods/probes", "pods"},
		{"metrics", "metrics"},
		{"metrics/resource", "metrics/resource"},
		{"metrics/hello", "metrics/hello"},
	}

	for _, test := range tests {
		assert.Equalf(t, test.expected, getURLRootPath(test.path), "path is: %s", test.path)
	}
}

func TestFineGrainedAuthz(t *testing.T) {
	// Enable features.ContainerCheckpoint during test
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.KubeletFineGrainedAuthz, true)

	fw := newServerTest()
	defer fw.testHTTPServer.Close()

	attributesGetter := NewNodeAuthorizerAttributesGetter(authzTestNodeName)

	testCases := []struct {
		name                     string
		path                     string
		expectedSubResources     []string
		authorizer               func(authorizer.Attributes) (authorized authorizer.Decision, reason string, err error)
		wantStatusCode           int
		wantCalledAuthorizeCount int
	}{
		{
			name:                 "both subresources rejected",
			path:                 "/configz",
			expectedSubResources: []string{"configz", "proxy"},
			authorizer: func(authorizer.Attributes) (authorized authorizer.Decision, reason string, err error) {
				return authorizer.DecisionNoOpinion, "", nil
			},
			wantStatusCode:           403,
			wantCalledAuthorizeCount: 2,
		},
		{
			name:                 "fine grained rejected, proxy accepted",
			path:                 "/configz",
			expectedSubResources: []string{"configz", "proxy"},
			authorizer: func(a authorizer.Attributes) (authorized authorizer.Decision, reason string, err error) {
				switch a.GetSubresource() {
				case "configz":
					return authorizer.DecisionNoOpinion, "", nil
				case "proxy":
					return authorizer.DecisionAllow, "", nil
				default:
					return authorizer.DecisionNoOpinion, "", fmt.Errorf("unexpected subresource %v", a.GetSubresource())
				}
			},
			wantStatusCode:           200,
			wantCalledAuthorizeCount: 2,
		},
		{
			name:                 "fine grained accepted",
			path:                 "/configz",
			expectedSubResources: []string{"configz", "proxy"},
			authorizer: func(a authorizer.Attributes) (authorized authorizer.Decision, reason string, err error) {
				switch a.GetSubresource() {
				case "configz":
					return authorizer.DecisionAllow, "", nil
				case "proxy":
					return authorizer.DecisionNoOpinion, "", fmt.Errorf("did not expect code to reach here")
				default:
					return authorizer.DecisionNoOpinion, "", fmt.Errorf("unexpected subresource %v", a.GetSubresource())
				}
			},
			wantStatusCode:           200,
			wantCalledAuthorizeCount: 1,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {

			calledAuthenticate := false
			calledAuthorizeCount := 0
			calledAttributes := false

			fw.fakeAuth.authenticateFunc = func(req *http.Request) (*authenticator.Response, bool, error) {
				calledAuthenticate = true
				return &authenticator.Response{User: AuthzTestUser()}, true, nil
			}
			fw.fakeAuth.attributesFunc = func(u user.Info, req *http.Request) []authorizer.Attributes {
				calledAttributes = true
				attrs := attributesGetter.GetRequestAttributes(u, req)
				var gotSubresources []string
				for _, attr := range attrs {
					gotSubresources = append(gotSubresources, attr.GetSubresource())
				}
				require.Equal(t, tc.expectedSubResources, gotSubresources)
				return attrs
			}
			fw.fakeAuth.authorizeFunc = func(a authorizer.Attributes) (authorized authorizer.Decision, reason string, err error) {
				calledAuthorizeCount += 1
				return tc.authorizer(a)
			}

			req, err := http.NewRequest("GET", fw.testHTTPServer.URL+tc.path, nil)
			require.NoError(t, err)

			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			defer resp.Body.Close()

			assert.Equal(t, tc.wantStatusCode, resp.StatusCode)
			assert.True(t, calledAuthenticate, "Authenticate was not called")
			assert.True(t, calledAttributes, "Attributes were not called")
			assert.Equal(t, tc.wantCalledAuthorizeCount, calledAuthorizeCount)
		})
	}
}

func TestNewServerRegistersMetricsSLIsEndpointTwice(t *testing.T) {
	host := &fakeKubelet{
		hostnameFunc: func() string {
			return "127.0.0.1"
		},
	}
	resourceAnalyzer := stats.NewResourceAnalyzer(nil, time.Minute, &record.FakeRecorder{})

	server1 := NewServer(host, resourceAnalyzer, []healthz.HealthChecker{}, flagz.NamedFlagSetsReader{}, nil, nil)
	server2 := NewServer(host, resourceAnalyzer, []healthz.HealthChecker{}, flagz.NamedFlagSetsReader{}, nil, nil)

	// Check if both servers registered the /metrics/slis endpoint
	assert.Contains(t, server1.restfulCont.RegisteredHandlePaths(), "/metrics/slis", "First server should register /metrics/slis")
	assert.Contains(t, server2.restfulCont.RegisteredHandlePaths(), "/metrics/slis", "Second server should register /metrics/slis")
}
