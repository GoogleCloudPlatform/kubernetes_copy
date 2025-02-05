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

package kubelet

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	core "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/record"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	"k8s.io/component-base/metrics/testutil"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
	"k8s.io/kubelet/pkg/cri/streaming/portforward"
	"k8s.io/kubelet/pkg/cri/streaming/remotecommand"
	_ "k8s.io/kubernetes/pkg/apis/core/install"
	"k8s.io/kubernetes/pkg/features"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
	containertest "k8s.io/kubernetes/pkg/kubelet/container/testing"
	"k8s.io/kubernetes/pkg/kubelet/metrics"
	"k8s.io/kubernetes/pkg/kubelet/prober/results"
	"k8s.io/kubernetes/pkg/kubelet/secret"
	"k8s.io/kubernetes/pkg/kubelet/status"
	kubetypes "k8s.io/kubernetes/pkg/kubelet/types"
	netutils "k8s.io/utils/net"
	"k8s.io/utils/ptr"
)

var containerRestartPolicyAlways = v1.ContainerRestartPolicyAlways

func TestNodeHostsFileContent(t *testing.T) {
	testCases := []struct {
		hostsFileName            string
		hostAliases              []v1.HostAlias
		rawHostsFileContent      string
		expectedHostsFileContent string
	}{
		{
			hostsFileName: "hosts_test_file1",
			hostAliases:   []v1.HostAlias{},
			rawHostsFileContent: `# hosts file for testing.
127.0.0.1	localhost
::1	localhost ip6-localhost ip6-loopback
fe00::0	ip6-localnet
fe00::0	ip6-mcastprefix
fe00::1	ip6-allnodes
fe00::2	ip6-allrouters
123.45.67.89	some.domain
`,
			expectedHostsFileContent: `# Kubernetes-managed hosts file (host network).
# hosts file for testing.
127.0.0.1	localhost
::1	localhost ip6-localhost ip6-loopback
fe00::0	ip6-localnet
fe00::0	ip6-mcastprefix
fe00::1	ip6-allnodes
fe00::2	ip6-allrouters
123.45.67.89	some.domain
`,
		},
		{
			hostsFileName: "hosts_test_file2",
			hostAliases:   []v1.HostAlias{},
			rawHostsFileContent: `# another hosts file for testing.
127.0.0.1	localhost
::1	localhost ip6-localhost ip6-loopback
fe00::0	ip6-localnet
fe00::0	ip6-mcastprefix
fe00::1	ip6-allnodes
fe00::2	ip6-allrouters
12.34.56.78	another.domain
`,
			expectedHostsFileContent: `# Kubernetes-managed hosts file (host network).
# another hosts file for testing.
127.0.0.1	localhost
::1	localhost ip6-localhost ip6-loopback
fe00::0	ip6-localnet
fe00::0	ip6-mcastprefix
fe00::1	ip6-allnodes
fe00::2	ip6-allrouters
12.34.56.78	another.domain
`,
		},
		{
			hostsFileName: "hosts_test_file1_with_host_aliases",
			hostAliases: []v1.HostAlias{
				{IP: "123.45.67.89", Hostnames: []string{"foo", "bar", "baz"}},
			},
			rawHostsFileContent: `# hosts file for testing.
127.0.0.1	localhost
::1	localhost ip6-localhost ip6-loopback
fe00::0	ip6-localnet
fe00::0	ip6-mcastprefix
fe00::1	ip6-allnodes
fe00::2	ip6-allrouters
123.45.67.89	some.domain
`,
			expectedHostsFileContent: `# Kubernetes-managed hosts file (host network).
# hosts file for testing.
127.0.0.1	localhost
::1	localhost ip6-localhost ip6-loopback
fe00::0	ip6-localnet
fe00::0	ip6-mcastprefix
fe00::1	ip6-allnodes
fe00::2	ip6-allrouters
123.45.67.89	some.domain

# Entries added by HostAliases.
123.45.67.89	foo	bar	baz
`,
		},
		{
			hostsFileName: "hosts_test_file2_with_host_aliases",
			hostAliases: []v1.HostAlias{
				{IP: "123.45.67.89", Hostnames: []string{"foo", "bar", "baz"}},
				{IP: "456.78.90.123", Hostnames: []string{"park", "doo", "boo"}},
			},
			rawHostsFileContent: `# another hosts file for testing.
127.0.0.1	localhost
::1	localhost ip6-localhost ip6-loopback
fe00::0	ip6-localnet
fe00::0	ip6-mcastprefix
fe00::1	ip6-allnodes
fe00::2	ip6-allrouters
12.34.56.78	another.domain
`,
			expectedHostsFileContent: `# Kubernetes-managed hosts file (host network).
# another hosts file for testing.
127.0.0.1	localhost
::1	localhost ip6-localhost ip6-loopback
fe00::0	ip6-localnet
fe00::0	ip6-mcastprefix
fe00::1	ip6-allnodes
fe00::2	ip6-allrouters
12.34.56.78	another.domain

# Entries added by HostAliases.
123.45.67.89	foo	bar	baz
456.78.90.123	park	doo	boo
`,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.hostsFileName, func(t *testing.T) {
			tmpdir, err := writeHostsFile(testCase.hostsFileName, testCase.rawHostsFileContent)
			require.NoError(t, err, "could not create a temp hosts file")
			defer os.RemoveAll(tmpdir)

			actualContent, fileReadErr := nodeHostsFileContent(filepath.Join(tmpdir, testCase.hostsFileName), testCase.hostAliases)
			require.NoError(t, fileReadErr, "could not create read hosts file")
			assert.Equal(t, testCase.expectedHostsFileContent, string(actualContent), "hosts file content not expected")
		})
	}
}

// writeHostsFile will write a hosts file into a temporary dir, and return that dir.
// Caller is responsible for deleting the dir and its contents.
func writeHostsFile(filename string, cfg string) (string, error) {
	tmpdir, err := os.MkdirTemp("", "kubelet=kubelet_pods_test.go=")
	if err != nil {
		return "", err
	}
	return tmpdir, os.WriteFile(filepath.Join(tmpdir, filename), []byte(cfg), 0644)
}

func TestManagedHostsFileContent(t *testing.T) {
	testCases := []struct {
		hostIPs         []string
		hostName        string
		hostDomainName  string
		hostAliases     []v1.HostAlias
		expectedContent string
	}{
		{
			hostIPs:     []string{"123.45.67.89"},
			hostName:    "podFoo",
			hostAliases: []v1.HostAlias{},
			expectedContent: `# Kubernetes-managed hosts file.
127.0.0.1	localhost
::1	localhost ip6-localhost ip6-loopback
fe00::0	ip6-localnet
fe00::0	ip6-mcastprefix
fe00::1	ip6-allnodes
fe00::2	ip6-allrouters
123.45.67.89	podFoo
`,
		},
		{
			hostIPs:        []string{"203.0.113.1"},
			hostName:       "podFoo",
			hostDomainName: "domainFoo",
			hostAliases:    []v1.HostAlias{},
			expectedContent: `# Kubernetes-managed hosts file.
127.0.0.1	localhost
::1	localhost ip6-localhost ip6-loopback
fe00::0	ip6-localnet
fe00::0	ip6-mcastprefix
fe00::1	ip6-allnodes
fe00::2	ip6-allrouters
203.0.113.1	podFoo.domainFoo	podFoo
`,
		},
		{
			hostIPs:        []string{"203.0.113.1"},
			hostName:       "podFoo",
			hostDomainName: "domainFoo",
			hostAliases: []v1.HostAlias{
				{IP: "123.45.67.89", Hostnames: []string{"foo", "bar", "baz"}},
			},
			expectedContent: `# Kubernetes-managed hosts file.
127.0.0.1	localhost
::1	localhost ip6-localhost ip6-loopback
fe00::0	ip6-localnet
fe00::0	ip6-mcastprefix
fe00::1	ip6-allnodes
fe00::2	ip6-allrouters
203.0.113.1	podFoo.domainFoo	podFoo

# Entries added by HostAliases.
123.45.67.89	foo	bar	baz
`,
		},
		{
			hostIPs:        []string{"203.0.113.1"},
			hostName:       "podFoo",
			hostDomainName: "domainFoo",
			hostAliases: []v1.HostAlias{
				{IP: "123.45.67.89", Hostnames: []string{"foo", "bar", "baz"}},
				{IP: "456.78.90.123", Hostnames: []string{"park", "doo", "boo"}},
			},
			expectedContent: `# Kubernetes-managed hosts file.
127.0.0.1	localhost
::1	localhost ip6-localhost ip6-loopback
fe00::0	ip6-localnet
fe00::0	ip6-mcastprefix
fe00::1	ip6-allnodes
fe00::2	ip6-allrouters
203.0.113.1	podFoo.domainFoo	podFoo

# Entries added by HostAliases.
123.45.67.89	foo	bar	baz
456.78.90.123	park	doo	boo
`,
		},
		{
			hostIPs:        []string{"203.0.113.1", "fd00::6"},
			hostName:       "podFoo",
			hostDomainName: "domainFoo",
			hostAliases:    []v1.HostAlias{},
			expectedContent: `# Kubernetes-managed hosts file.
127.0.0.1	localhost
::1	localhost ip6-localhost ip6-loopback
fe00::0	ip6-localnet
fe00::0	ip6-mcastprefix
fe00::1	ip6-allnodes
fe00::2	ip6-allrouters
203.0.113.1	podFoo.domainFoo	podFoo
fd00::6	podFoo.domainFoo	podFoo
`,
		},
	}

	for _, testCase := range testCases {
		actualContent := managedHostsFileContent(testCase.hostIPs, testCase.hostName, testCase.hostDomainName, testCase.hostAliases)
		assert.Equal(t, testCase.expectedContent, string(actualContent), "hosts file content not expected")
	}
}

func TestRunInContainerNoSuchPod(t *testing.T) {
	ctx := context.Background()
	testKubelet := newTestKubelet(t, false /* controllerAttachDetachEnabled */)
	defer testKubelet.Cleanup()
	kubelet := testKubelet.kubelet
	fakeRuntime := testKubelet.fakeRuntime
	fakeRuntime.PodList = []*containertest.FakePod{}

	podName := "podFoo"
	podNamespace := "nsFoo"
	containerName := "containerFoo"
	output, err := kubelet.RunInContainer(
		ctx,
		kubecontainer.GetPodFullName(&v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: podName, Namespace: podNamespace}}),
		"",
		containerName,
		[]string{"ls"})
	assert.Error(t, err)
	assert.Nil(t, output, "output should be nil")
}

func TestRunInContainer(t *testing.T) {
	ctx := context.Background()
	for _, testError := range []error{nil, errors.New("bar")} {
		testKubelet := newTestKubelet(t, false /* controllerAttachDetachEnabled */)
		defer testKubelet.Cleanup()
		kubelet := testKubelet.kubelet
		fakeRuntime := testKubelet.fakeRuntime
		fakeCommandRunner := containertest.FakeContainerCommandRunner{
			Err:    testError,
			Stdout: "foo",
		}
		kubelet.runner = &fakeCommandRunner

		containerID := kubecontainer.ContainerID{Type: "test", ID: "abc1234"}
		fakeRuntime.PodList = []*containertest.FakePod{
			{Pod: &kubecontainer.Pod{
				ID:        "12345678",
				Name:      "podFoo",
				Namespace: "nsFoo",
				Containers: []*kubecontainer.Container{
					{Name: "containerFoo",
						ID: containerID,
					},
				},
			}},
		}
		cmd := []string{"ls"}
		actualOutput, err := kubelet.RunInContainer(ctx, "podFoo_nsFoo", "", "containerFoo", cmd)
		assert.Equal(t, containerID, fakeCommandRunner.ContainerID, "(testError=%v) ID", testError)
		assert.Equal(t, cmd, fakeCommandRunner.Cmd, "(testError=%v) command", testError)
		// this isn't 100% foolproof as a bug in a real CommandRunner where it fails to copy to stdout/stderr wouldn't be caught by this test
		assert.Equal(t, "foo", string(actualOutput), "(testError=%v) output", testError)
		assert.Equal(t, err, testError, "(testError=%v) err", testError)
	}
}

type testServiceLister struct {
	services []*v1.Service
}

func (ls testServiceLister) List(labels.Selector) ([]*v1.Service, error) {
	return ls.services, nil
}

type envs []kubecontainer.EnvVar

func (e envs) Len() int {
	return len(e)
}

func (e envs) Swap(i, j int) { e[i], e[j] = e[j], e[i] }

func (e envs) Less(i, j int) bool { return e[i].Name < e[j].Name }

func buildService(name, namespace, clusterIP, protocol string, port int) *v1.Service {
	return &v1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: v1.ServiceSpec{
			Ports: []v1.ServicePort{{
				Protocol: v1.Protocol(protocol),
				Port:     int32(port),
			}},
			ClusterIP: clusterIP,
		},
	}
}

func TestMakeEnvironmentVariables(t *testing.T) {
	trueVal := true
	services := []*v1.Service{
		buildService("kubernetes", metav1.NamespaceDefault, "1.2.3.1", "TCP", 8081),
		buildService("test", "test1", "1.2.3.3", "TCP", 8083),
		buildService("kubernetes", "test2", "1.2.3.4", "TCP", 8084),
		buildService("test", "test2", "1.2.3.5", "TCP", 8085),
		buildService("test", "test2", "None", "TCP", 8085),
		buildService("test", "test2", "", "TCP", 8085),
		buildService("not-special", metav1.NamespaceDefault, "1.2.3.8", "TCP", 8088),
		buildService("not-special", metav1.NamespaceDefault, "None", "TCP", 8088),
		buildService("not-special", metav1.NamespaceDefault, "", "TCP", 8088),
	}

	trueValue := true
	falseValue := false
	testCases := []struct {
		name                                       string                 // the name of the test case
		ns                                         string                 // the namespace to generate environment for
		enableServiceLinks                         *bool                  // enabling service links
		enableRelaxedEnvironmentVariableValidation bool                   // enable enableRelaxedEnvironmentVariableValidation feature gate
		container                                  *v1.Container          // the container to use
		nilLister                                  bool                   // whether the lister should be nil
		staticPod                                  bool                   // whether the pod should be a static pod (versus an API pod)
		unsyncedServices                           bool                   // whether the services should NOT be synced
		configMap                                  *v1.ConfigMap          // an optional ConfigMap to pull from
		secret                                     *v1.Secret             // an optional Secret to pull from
		podIPs                                     []string               // the pod IPs
		expectedEnvs                               []kubecontainer.EnvVar // a set of expected environment vars
		expectedError                              bool                   // does the test fail
		expectedEvent                              string                 // does the test emit an event
	}{
		{
			name:               "if services aren't synced, non-static pods should fail",
			ns:                 "test1",
			enableServiceLinks: &falseValue,
			container:          &v1.Container{Env: []v1.EnvVar{}},
			nilLister:          false,
			staticPod:          false,
			unsyncedServices:   true,
			expectedEnvs:       []kubecontainer.EnvVar{},
			expectedError:      true,
		},
		{
			name:               "if services aren't synced, static pods should succeed", // if there is no service
			ns:                 "test1",
			enableServiceLinks: &falseValue,
			container:          &v1.Container{Env: []v1.EnvVar{}},
			nilLister:          false,
			staticPod:          true,
			unsyncedServices:   true,
		},
		{
			name:               "api server = Y, kubelet = Y",
			ns:                 "test1",
			enableServiceLinks: &falseValue,
			container: &v1.Container{
				Env: []v1.EnvVar{
					{Name: "FOO", Value: "BAR"},
					{Name: "TEST_SERVICE_HOST", Value: "1.2.3.3"},
					{Name: "TEST_SERVICE_PORT", Value: "8083"},
					{Name: "TEST_PORT", Value: "tcp://1.2.3.3:8083"},
					{Name: "TEST_PORT_8083_TCP", Value: "tcp://1.2.3.3:8083"},
					{Name: "TEST_PORT_8083_TCP_PROTO", Value: "tcp"},
					{Name: "TEST_PORT_8083_TCP_PORT", Value: "8083"},
					{Name: "TEST_PORT_8083_TCP_ADDR", Value: "1.2.3.3"},
				},
			},
			nilLister: false,
			expectedEnvs: []kubecontainer.EnvVar{
				{Name: "FOO", Value: "BAR"},
				{Name: "TEST_SERVICE_HOST", Value: "1.2.3.3"},
				{Name: "TEST_SERVICE_PORT", Value: "8083"},
				{Name: "TEST_PORT", Value: "tcp://1.2.3.3:8083"},
				{Name: "TEST_PORT_8083_TCP", Value: "tcp://1.2.3.3:8083"},
				{Name: "TEST_PORT_8083_TCP_PROTO", Value: "tcp"},
				{Name: "TEST_PORT_8083_TCP_PORT", Value: "8083"},
				{Name: "TEST_PORT_8083_TCP_ADDR", Value: "1.2.3.3"},
				{Name: "KUBERNETES_SERVICE_PORT", Value: "8081"},
				{Name: "KUBERNETES_SERVICE_HOST", Value: "1.2.3.1"},
				{Name: "KUBERNETES_PORT", Value: "tcp://1.2.3.1:8081"},
				{Name: "KUBERNETES_PORT_8081_TCP", Value: "tcp://1.2.3.1:8081"},
				{Name: "KUBERNETES_PORT_8081_TCP_PROTO", Value: "tcp"},
				{Name: "KUBERNETES_PORT_8081_TCP_PORT", Value: "8081"},
				{Name: "KUBERNETES_PORT_8081_TCP_ADDR", Value: "1.2.3.1"},
			},
		},
		{
			name:               "api server = Y, kubelet = N",
			ns:                 "test1",
			enableServiceLinks: &falseValue,
			container: &v1.Container{
				Env: []v1.EnvVar{
					{Name: "FOO", Value: "BAR"},
					{Name: "TEST_SERVICE_HOST", Value: "1.2.3.3"},
					{Name: "TEST_SERVICE_PORT", Value: "8083"},
					{Name: "TEST_PORT", Value: "tcp://1.2.3.3:8083"},
					{Name: "TEST_PORT_8083_TCP", Value: "tcp://1.2.3.3:8083"},
					{Name: "TEST_PORT_8083_TCP_PROTO", Value: "tcp"},
					{Name: "TEST_PORT_8083_TCP_PORT", Value: "8083"},
					{Name: "TEST_PORT_8083_TCP_ADDR", Value: "1.2.3.3"},
				},
			},
			nilLister: true,
			expectedEnvs: []kubecontainer.EnvVar{
				{Name: "FOO", Value: "BAR"},
				{Name: "TEST_SERVICE_HOST", Value: "1.2.3.3"},
				{Name: "TEST_SERVICE_PORT", Value: "8083"},
				{Name: "TEST_PORT", Value: "tcp://1.2.3.3:8083"},
				{Name: "TEST_PORT_8083_TCP", Value: "tcp://1.2.3.3:8083"},
				{Name: "TEST_PORT_8083_TCP_PROTO", Value: "tcp"},
				{Name: "TEST_PORT_8083_TCP_PORT", Value: "8083"},
				{Name: "TEST_PORT_8083_TCP_ADDR", Value: "1.2.3.3"},
			},
		},
		{
			name:               "api server = N; kubelet = Y",
			ns:                 "test1",
			enableServiceLinks: &falseValue,
			container: &v1.Container{
				Env: []v1.EnvVar{
					{Name: "FOO", Value: "BAZ"},
				},
			},
			nilLister: false,
			expectedEnvs: []kubecontainer.EnvVar{
				{Name: "FOO", Value: "BAZ"},
				{Name: "KUBERNETES_SERVICE_HOST", Value: "1.2.3.1"},
				{Name: "KUBERNETES_SERVICE_PORT", Value: "8081"},
				{Name: "KUBERNETES_PORT", Value: "tcp://1.2.3.1:8081"},
				{Name: "KUBERNETES_PORT_8081_TCP", Value: "tcp://1.2.3.1:8081"},
				{Name: "KUBERNETES_PORT_8081_TCP_PROTO", Value: "tcp"},
				{Name: "KUBERNETES_PORT_8081_TCP_PORT", Value: "8081"},
				{Name: "KUBERNETES_PORT_8081_TCP_ADDR", Value: "1.2.3.1"},
			},
		},
		{
			name:               "api server = N; kubelet = Y; service env vars",
			ns:                 "test1",
			enableServiceLinks: &trueValue,
			container: &v1.Container{
				Env: []v1.EnvVar{
					{Name: "FOO", Value: "BAZ"},
				},
			},
			nilLister: false,
			expectedEnvs: []kubecontainer.EnvVar{
				{Name: "FOO", Value: "BAZ"},
				{Name: "TEST_SERVICE_HOST", Value: "1.2.3.3"},
				{Name: "TEST_SERVICE_PORT", Value: "8083"},
				{Name: "TEST_PORT", Value: "tcp://1.2.3.3:8083"},
				{Name: "TEST_PORT_8083_TCP", Value: "tcp://1.2.3.3:8083"},
				{Name: "TEST_PORT_8083_TCP_PROTO", Value: "tcp"},
				{Name: "TEST_PORT_8083_TCP_PORT", Value: "8083"},
				{Name: "TEST_PORT_8083_TCP_ADDR", Value: "1.2.3.3"},
				{Name: "KUBERNETES_SERVICE_HOST", Value: "1.2.3.1"},
				{Name: "KUBERNETES_SERVICE_PORT", Value: "8081"},
				{Name: "KUBERNETES_PORT", Value: "tcp://1.2.3.1:8081"},
				{Name: "KUBERNETES_PORT_8081_TCP", Value: "tcp://1.2.3.1:8081"},
				{Name: "KUBERNETES_PORT_8081_TCP_PROTO", Value: "tcp"},
				{Name: "KUBERNETES_PORT_8081_TCP_PORT", Value: "8081"},
				{Name: "KUBERNETES_PORT_8081_TCP_ADDR", Value: "1.2.3.1"},
			},
		},
		{
			name:               "master service in pod ns",
			ns:                 "test2",
			enableServiceLinks: &falseValue,
			container: &v1.Container{
				Env: []v1.EnvVar{
					{Name: "FOO", Value: "ZAP"},
				},
			},
			nilLister: false,
			expectedEnvs: []kubecontainer.EnvVar{
				{Name: "FOO", Value: "ZAP"},
				{Name: "KUBERNETES_SERVICE_HOST", Value: "1.2.3.1"},
				{Name: "KUBERNETES_SERVICE_PORT", Value: "8081"},
				{Name: "KUBERNETES_PORT", Value: "tcp://1.2.3.1:8081"},
				{Name: "KUBERNETES_PORT_8081_TCP", Value: "tcp://1.2.3.1:8081"},
				{Name: "KUBERNETES_PORT_8081_TCP_PROTO", Value: "tcp"},
				{Name: "KUBERNETES_PORT_8081_TCP_PORT", Value: "8081"},
				{Name: "KUBERNETES_PORT_8081_TCP_ADDR", Value: "1.2.3.1"},
			},
		},
		{
			name:               "master service in pod ns, service env vars",
			ns:                 "test2",
			enableServiceLinks: &trueValue,
			container: &v1.Container{
				Env: []v1.EnvVar{
					{Name: "FOO", Value: "ZAP"},
				},
			},
			nilLister: false,
			expectedEnvs: []kubecontainer.EnvVar{
				{Name: "FOO", Value: "ZAP"},
				{Name: "TEST_SERVICE_HOST", Value: "1.2.3.5"},
				{Name: "TEST_SERVICE_PORT", Value: "8085"},
				{Name: "TEST_PORT", Value: "tcp://1.2.3.5:8085"},
				{Name: "TEST_PORT_8085_TCP", Value: "tcp://1.2.3.5:8085"},
				{Name: "TEST_PORT_8085_TCP_PROTO", Value: "tcp"},
				{Name: "TEST_PORT_8085_TCP_PORT", Value: "8085"},
				{Name: "TEST_PORT_8085_TCP_ADDR", Value: "1.2.3.5"},
				{Name: "KUBERNETES_SERVICE_HOST", Value: "1.2.3.4"},
				{Name: "KUBERNETES_SERVICE_PORT", Value: "8084"},
				{Name: "KUBERNETES_PORT", Value: "tcp://1.2.3.4:8084"},
				{Name: "KUBERNETES_PORT_8084_TCP", Value: "tcp://1.2.3.4:8084"},
				{Name: "KUBERNETES_PORT_8084_TCP_PROTO", Value: "tcp"},
				{Name: "KUBERNETES_PORT_8084_TCP_PORT", Value: "8084"},
				{Name: "KUBERNETES_PORT_8084_TCP_ADDR", Value: "1.2.3.4"},
			},
		},
		{
			name:               "pod in master service ns",
			ns:                 metav1.NamespaceDefault,
			enableServiceLinks: &falseValue,
			container:          &v1.Container{},
			nilLister:          false,
			expectedEnvs: []kubecontainer.EnvVar{
				{Name: "KUBERNETES_SERVICE_HOST", Value: "1.2.3.1"},
				{Name: "KUBERNETES_SERVICE_PORT", Value: "8081"},
				{Name: "KUBERNETES_PORT", Value: "tcp://1.2.3.1:8081"},
				{Name: "KUBERNETES_PORT_8081_TCP", Value: "tcp://1.2.3.1:8081"},
				{Name: "KUBERNETES_PORT_8081_TCP_PROTO", Value: "tcp"},
				{Name: "KUBERNETES_PORT_8081_TCP_PORT", Value: "8081"},
				{Name: "KUBERNETES_PORT_8081_TCP_ADDR", Value: "1.2.3.1"},
			},
		},
		{
			name:               "pod in master service ns, service env vars",
			ns:                 metav1.NamespaceDefault,
			enableServiceLinks: &trueValue,
			container:          &v1.Container{},
			nilLister:          false,
			expectedEnvs: []kubecontainer.EnvVar{
				{Name: "NOT_SPECIAL_SERVICE_HOST", Value: "1.2.3.8"},
				{Name: "NOT_SPECIAL_SERVICE_PORT", Value: "8088"},
				{Name: "NOT_SPECIAL_PORT", Value: "tcp://1.2.3.8:8088"},
				{Name: "NOT_SPECIAL_PORT_8088_TCP", Value: "tcp://1.2.3.8:8088"},
				{Name: "NOT_SPECIAL_PORT_8088_TCP_PROTO", Value: "tcp"},
				{Name: "NOT_SPECIAL_PORT_8088_TCP_PORT", Value: "8088"},
				{Name: "NOT_SPECIAL_PORT_8088_TCP_ADDR", Value: "1.2.3.8"},
				{Name: "KUBERNETES_SERVICE_HOST", Value: "1.2.3.1"},
				{Name: "KUBERNETES_SERVICE_PORT", Value: "8081"},
				{Name: "KUBERNETES_PORT", Value: "tcp://1.2.3.1:8081"},
				{Name: "KUBERNETES_PORT_8081_TCP", Value: "tcp://1.2.3.1:8081"},
				{Name: "KUBERNETES_PORT_8081_TCP_PROTO", Value: "tcp"},
				{Name: "KUBERNETES_PORT_8081_TCP_PORT", Value: "8081"},
				{Name: "KUBERNETES_PORT_8081_TCP_ADDR", Value: "1.2.3.1"},
			},
		},
		{
			name:               "downward api pod",
			ns:                 "downward-api",
			enableServiceLinks: &falseValue,
			container: &v1.Container{
				Env: []v1.EnvVar{
					{
						Name: "POD_NAME",
						ValueFrom: &v1.EnvVarSource{
							FieldRef: &v1.ObjectFieldSelector{
								APIVersion: "v1",
								FieldPath:  "metadata.name",
							},
						},
					},
					{
						Name: "POD_NAMESPACE",
						ValueFrom: &v1.EnvVarSource{
							FieldRef: &v1.ObjectFieldSelector{
								APIVersion: "v1",
								FieldPath:  "metadata.namespace",
							},
						},
					},
					{
						Name: "POD_NODE_NAME",
						ValueFrom: &v1.EnvVarSource{
							FieldRef: &v1.ObjectFieldSelector{
								APIVersion: "v1",
								FieldPath:  "spec.nodeName",
							},
						},
					},
					{
						Name: "POD_SERVICE_ACCOUNT_NAME",
						ValueFrom: &v1.EnvVarSource{
							FieldRef: &v1.ObjectFieldSelector{
								APIVersion: "v1",
								FieldPath:  "spec.serviceAccountName",
							},
						},
					},
					{
						Name: "POD_IP",
						ValueFrom: &v1.EnvVarSource{
							FieldRef: &v1.ObjectFieldSelector{
								APIVersion: "v1",
								FieldPath:  "status.podIP",
							},
						},
					},
					{
						Name: "POD_IPS",
						ValueFrom: &v1.EnvVarSource{
							FieldRef: &v1.ObjectFieldSelector{
								APIVersion: "v1",
								FieldPath:  "status.podIPs",
							},
						},
					},
					{
						Name: "HOST_IP",
						ValueFrom: &v1.EnvVarSource{
							FieldRef: &v1.ObjectFieldSelector{
								APIVersion: "v1",
								FieldPath:  "status.hostIP",
							},
						},
					},
					{
						Name: "HOST_IPS",
						ValueFrom: &v1.EnvVarSource{
							FieldRef: &v1.ObjectFieldSelector{
								APIVersion: "v1",
								FieldPath:  "status.hostIPs",
							},
						},
					},
				},
			},
			podIPs:    []string{"1.2.3.4", "fd00::6"},
			nilLister: true,
			expectedEnvs: []kubecontainer.EnvVar{
				{Name: "POD_NAME", Value: "dapi-test-pod-name"},
				{Name: "POD_NAMESPACE", Value: "downward-api"},
				{Name: "POD_NODE_NAME", Value: "node-name"},
				{Name: "POD_SERVICE_ACCOUNT_NAME", Value: "special"},
				{Name: "POD_IP", Value: "1.2.3.4"},
				{Name: "POD_IPS", Value: "1.2.3.4,fd00::6"},
				{Name: "HOST_IP", Value: testKubeletHostIP},
				{Name: "HOST_IPS", Value: testKubeletHostIP + "," + testKubeletHostIPv6},
			},
		},
		{
			name:               "downward api pod ips reverse order",
			ns:                 "downward-api",
			enableServiceLinks: &falseValue,
			container: &v1.Container{
				Env: []v1.EnvVar{
					{
						Name: "POD_IP",
						ValueFrom: &v1.EnvVarSource{
							FieldRef: &v1.ObjectFieldSelector{
								APIVersion: "v1",
								FieldPath:  "status.podIP",
							},
						},
					},
					{
						Name: "POD_IPS",
						ValueFrom: &v1.EnvVarSource{
							FieldRef: &v1.ObjectFieldSelector{
								APIVersion: "v1",
								FieldPath:  "status.podIPs",
							},
						},
					},
					{
						Name: "HOST_IP",
						ValueFrom: &v1.EnvVarSource{
							FieldRef: &v1.ObjectFieldSelector{
								APIVersion: "v1",
								FieldPath:  "status.hostIP",
							},
						},
					},
					{
						Name: "HOST_IPS",
						ValueFrom: &v1.EnvVarSource{
							FieldRef: &v1.ObjectFieldSelector{
								APIVersion: "v1",
								FieldPath:  "status.hostIPs",
							},
						},
					},
				},
			},
			podIPs:    []string{"fd00::6", "1.2.3.4"},
			nilLister: true,
			expectedEnvs: []kubecontainer.EnvVar{
				{Name: "POD_IP", Value: "1.2.3.4"},
				{Name: "POD_IPS", Value: "1.2.3.4,fd00::6"},
				{Name: "HOST_IP", Value: testKubeletHostIP},
				{Name: "HOST_IPS", Value: testKubeletHostIP + "," + testKubeletHostIPv6},
			},
		},
		{
			name:               "downward api pod ips multiple ips",
			ns:                 "downward-api",
			enableServiceLinks: &falseValue,
			container: &v1.Container{
				Env: []v1.EnvVar{
					{
						Name: "POD_IP",
						ValueFrom: &v1.EnvVarSource{
							FieldRef: &v1.ObjectFieldSelector{
								APIVersion: "v1",
								FieldPath:  "status.podIP",
							},
						},
					},
					{
						Name: "POD_IPS",
						ValueFrom: &v1.EnvVarSource{
							FieldRef: &v1.ObjectFieldSelector{
								APIVersion: "v1",
								FieldPath:  "status.podIPs",
							},
						},
					},
					{
						Name: "HOST_IP",
						ValueFrom: &v1.EnvVarSource{
							FieldRef: &v1.ObjectFieldSelector{
								APIVersion: "v1",
								FieldPath:  "status.hostIP",
							},
						},
					},
					{
						Name: "HOST_IPS",
						ValueFrom: &v1.EnvVarSource{
							FieldRef: &v1.ObjectFieldSelector{
								APIVersion: "v1",
								FieldPath:  "status.hostIPs",
							},
						},
					},
				},
			},
			podIPs:    []string{"1.2.3.4", "192.168.1.1.", "fd00::6"},
			nilLister: true,
			expectedEnvs: []kubecontainer.EnvVar{
				{Name: "POD_IP", Value: "1.2.3.4"},
				{Name: "POD_IPS", Value: "1.2.3.4,fd00::6"},
				{Name: "HOST_IP", Value: testKubeletHostIP},
				{Name: "HOST_IPS", Value: testKubeletHostIP + "," + testKubeletHostIPv6},
			},
		},
		{
			name:               "env expansion",
			ns:                 "test1",
			enableServiceLinks: &falseValue,
			container: &v1.Container{
				Env: []v1.EnvVar{
					{
						Name:  "TEST_LITERAL",
						Value: "test-test-test",
					},
					{
						Name: "POD_NAME",
						ValueFrom: &v1.EnvVarSource{
							FieldRef: &v1.ObjectFieldSelector{
								APIVersion: "v1", //legacyscheme.Registry.GroupOrDie(v1.GroupName).GroupVersion.String(),
								FieldPath:  "metadata.name",
							},
						},
					},
					{
						Name:  "OUT_OF_ORDER_TEST",
						Value: "$(OUT_OF_ORDER_TARGET)",
					},
					{
						Name:  "OUT_OF_ORDER_TARGET",
						Value: "FOO",
					},
					{
						Name: "EMPTY_VAR",
					},
					{
						Name:  "EMPTY_TEST",
						Value: "foo-$(EMPTY_VAR)",
					},
					{
						Name:  "POD_NAME_TEST2",
						Value: "test2-$(POD_NAME)",
					},
					{
						Name:  "POD_NAME_TEST3",
						Value: "$(POD_NAME_TEST2)-3",
					},
					{
						Name:  "LITERAL_TEST",
						Value: "literal-$(TEST_LITERAL)",
					},
					{
						Name:  "TEST_UNDEFINED",
						Value: "$(UNDEFINED_VAR)",
					},
				},
			},
			nilLister: false,
			expectedEnvs: []kubecontainer.EnvVar{
				{
					Name:  "TEST_LITERAL",
					Value: "test-test-test",
				},
				{
					Name:  "POD_NAME",
					Value: "dapi-test-pod-name",
				},
				{
					Name:  "POD_NAME_TEST2",
					Value: "test2-dapi-test-pod-name",
				},
				{
					Name:  "POD_NAME_TEST3",
					Value: "test2-dapi-test-pod-name-3",
				},
				{
					Name:  "LITERAL_TEST",
					Value: "literal-test-test-test",
				},
				{
					Name:  "OUT_OF_ORDER_TEST",
					Value: "$(OUT_OF_ORDER_TARGET)",
				},
				{
					Name:  "OUT_OF_ORDER_TARGET",
					Value: "FOO",
				},
				{
					Name:  "TEST_UNDEFINED",
					Value: "$(UNDEFINED_VAR)",
				},
				{
					Name: "EMPTY_VAR",
				},
				{
					Name:  "EMPTY_TEST",
					Value: "foo-",
				},
				{
					Name:  "KUBERNETES_SERVICE_HOST",
					Value: "1.2.3.1",
				},
				{
					Name:  "KUBERNETES_SERVICE_PORT",
					Value: "8081",
				},
				{
					Name:  "KUBERNETES_PORT",
					Value: "tcp://1.2.3.1:8081",
				},
				{
					Name:  "KUBERNETES_PORT_8081_TCP",
					Value: "tcp://1.2.3.1:8081",
				},
				{
					Name:  "KUBERNETES_PORT_8081_TCP_PROTO",
					Value: "tcp",
				},
				{
					Name:  "KUBERNETES_PORT_8081_TCP_PORT",
					Value: "8081",
				},
				{
					Name:  "KUBERNETES_PORT_8081_TCP_ADDR",
					Value: "1.2.3.1",
				},
			},
		},
		{
			name:               "env expansion, service env vars",
			ns:                 "test1",
			enableServiceLinks: &trueValue,
			container: &v1.Container{
				Env: []v1.EnvVar{
					{
						Name:  "TEST_LITERAL",
						Value: "test-test-test",
					},
					{
						Name: "POD_NAME",
						ValueFrom: &v1.EnvVarSource{
							FieldRef: &v1.ObjectFieldSelector{
								APIVersion: "v1",
								FieldPath:  "metadata.name",
							},
						},
					},
					{
						Name:  "OUT_OF_ORDER_TEST",
						Value: "$(OUT_OF_ORDER_TARGET)",
					},
					{
						Name:  "OUT_OF_ORDER_TARGET",
						Value: "FOO",
					},
					{
						Name: "EMPTY_VAR",
					},
					{
						Name:  "EMPTY_TEST",
						Value: "foo-$(EMPTY_VAR)",
					},
					{
						Name:  "POD_NAME_TEST2",
						Value: "test2-$(POD_NAME)",
					},
					{
						Name:  "POD_NAME_TEST3",
						Value: "$(POD_NAME_TEST2)-3",
					},
					{
						Name:  "LITERAL_TEST",
						Value: "literal-$(TEST_LITERAL)",
					},
					{
						Name:  "SERVICE_VAR_TEST",
						Value: "$(TEST_SERVICE_HOST):$(TEST_SERVICE_PORT)",
					},
					{
						Name:  "TEST_UNDEFINED",
						Value: "$(UNDEFINED_VAR)",
					},
				},
			},
			nilLister: false,
			expectedEnvs: []kubecontainer.EnvVar{
				{
					Name:  "TEST_LITERAL",
					Value: "test-test-test",
				},
				{
					Name:  "POD_NAME",
					Value: "dapi-test-pod-name",
				},
				{
					Name:  "POD_NAME_TEST2",
					Value: "test2-dapi-test-pod-name",
				},
				{
					Name:  "POD_NAME_TEST3",
					Value: "test2-dapi-test-pod-name-3",
				},
				{
					Name:  "LITERAL_TEST",
					Value: "literal-test-test-test",
				},
				{
					Name:  "TEST_SERVICE_HOST",
					Value: "1.2.3.3",
				},
				{
					Name:  "TEST_SERVICE_PORT",
					Value: "8083",
				},
				{
					Name:  "TEST_PORT",
					Value: "tcp://1.2.3.3:8083",
				},
				{
					Name:  "TEST_PORT_8083_TCP",
					Value: "tcp://1.2.3.3:8083",
				},
				{
					Name:  "TEST_PORT_8083_TCP_PROTO",
					Value: "tcp",
				},
				{
					Name:  "TEST_PORT_8083_TCP_PORT",
					Value: "8083",
				},
				{
					Name:  "TEST_PORT_8083_TCP_ADDR",
					Value: "1.2.3.3",
				},
				{
					Name:  "SERVICE_VAR_TEST",
					Value: "1.2.3.3:8083",
				},
				{
					Name:  "OUT_OF_ORDER_TEST",
					Value: "$(OUT_OF_ORDER_TARGET)",
				},
				{
					Name:  "OUT_OF_ORDER_TARGET",
					Value: "FOO",
				},
				{
					Name:  "TEST_UNDEFINED",
					Value: "$(UNDEFINED_VAR)",
				},
				{
					Name: "EMPTY_VAR",
				},
				{
					Name:  "EMPTY_TEST",
					Value: "foo-",
				},
				{
					Name:  "KUBERNETES_SERVICE_HOST",
					Value: "1.2.3.1",
				},
				{
					Name:  "KUBERNETES_SERVICE_PORT",
					Value: "8081",
				},
				{
					Name:  "KUBERNETES_PORT",
					Value: "tcp://1.2.3.1:8081",
				},
				{
					Name:  "KUBERNETES_PORT_8081_TCP",
					Value: "tcp://1.2.3.1:8081",
				},
				{
					Name:  "KUBERNETES_PORT_8081_TCP_PROTO",
					Value: "tcp",
				},
				{
					Name:  "KUBERNETES_PORT_8081_TCP_PORT",
					Value: "8081",
				},
				{
					Name:  "KUBERNETES_PORT_8081_TCP_ADDR",
					Value: "1.2.3.1",
				},
			},
		},
		{
			name:               "configmapkeyref_missing_optional",
			ns:                 "test",
			enableServiceLinks: &falseValue,
			container: &v1.Container{
				Env: []v1.EnvVar{
					{
						Name: "POD_NAME",
						ValueFrom: &v1.EnvVarSource{
							ConfigMapKeyRef: &v1.ConfigMapKeySelector{
								LocalObjectReference: v1.LocalObjectReference{Name: "missing-config-map"},
								Key:                  "key",
								Optional:             &trueVal,
							},
						},
					},
				},
			},
			expectedEnvs: []kubecontainer.EnvVar{
				{Name: "KUBERNETES_SERVICE_HOST", Value: "1.2.3.1"},
				{Name: "KUBERNETES_SERVICE_PORT", Value: "8081"},
				{Name: "KUBERNETES_PORT", Value: "tcp://1.2.3.1:8081"},
				{Name: "KUBERNETES_PORT_8081_TCP", Value: "tcp://1.2.3.1:8081"},
				{Name: "KUBERNETES_PORT_8081_TCP_PROTO", Value: "tcp"},
				{Name: "KUBERNETES_PORT_8081_TCP_PORT", Value: "8081"},
				{Name: "KUBERNETES_PORT_8081_TCP_ADDR", Value: "1.2.3.1"},
			},
		},
		{
			name:               "configmapkeyref_missing_key_optional",
			ns:                 "test",
			enableServiceLinks: &falseValue,
			container: &v1.Container{
				Env: []v1.EnvVar{
					{
						Name: "POD_NAME",
						ValueFrom: &v1.EnvVarSource{
							ConfigMapKeyRef: &v1.ConfigMapKeySelector{
								LocalObjectReference: v1.LocalObjectReference{Name: "test-config-map"},
								Key:                  "key",
								Optional:             &trueVal,
							},
						},
					},
				},
			},
			nilLister: true,
			configMap: &v1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "test1",
					Name:      "test-configmap",
				},
				Data: map[string]string{
					"a": "b",
				},
			},
			expectedEnvs: nil,
		},
		{
			name:               "secretkeyref_missing_optional",
			ns:                 "test",
			enableServiceLinks: &falseValue,
			container: &v1.Container{
				Env: []v1.EnvVar{
					{
						Name: "POD_NAME",
						ValueFrom: &v1.EnvVarSource{
							SecretKeyRef: &v1.SecretKeySelector{
								LocalObjectReference: v1.LocalObjectReference{Name: "missing-secret"},
								Key:                  "key",
								Optional:             &trueVal,
							},
						},
					},
				},
			},
			expectedEnvs: []kubecontainer.EnvVar{
				{Name: "KUBERNETES_SERVICE_HOST", Value: "1.2.3.1"},
				{Name: "KUBERNETES_SERVICE_PORT", Value: "8081"},
				{Name: "KUBERNETES_PORT", Value: "tcp://1.2.3.1:8081"},
				{Name: "KUBERNETES_PORT_8081_TCP", Value: "tcp://1.2.3.1:8081"},
				{Name: "KUBERNETES_PORT_8081_TCP_PROTO", Value: "tcp"},
				{Name: "KUBERNETES_PORT_8081_TCP_PORT", Value: "8081"},
				{Name: "KUBERNETES_PORT_8081_TCP_ADDR", Value: "1.2.3.1"},
			},
		},
		{
			name:               "secretkeyref_missing_key_optional",
			ns:                 "test",
			enableServiceLinks: &falseValue,
			container: &v1.Container{
				Env: []v1.EnvVar{
					{
						Name: "POD_NAME",
						ValueFrom: &v1.EnvVarSource{
							SecretKeyRef: &v1.SecretKeySelector{
								LocalObjectReference: v1.LocalObjectReference{Name: "test-secret"},
								Key:                  "key",
								Optional:             &trueVal,
							},
						},
					},
				},
			},
			nilLister: true,
			secret: &v1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "test1",
					Name:      "test-secret",
				},
				Data: map[string][]byte{
					"a": []byte("b"),
				},
			},
			expectedEnvs: nil,
		},
		{
			name:               "configmap",
			ns:                 "test1",
			enableServiceLinks: &falseValue,
			container: &v1.Container{
				EnvFrom: []v1.EnvFromSource{
					{
						ConfigMapRef: &v1.ConfigMapEnvSource{LocalObjectReference: v1.LocalObjectReference{Name: "test-config-map"}},
					},
					{
						Prefix:       "p_",
						ConfigMapRef: &v1.ConfigMapEnvSource{LocalObjectReference: v1.LocalObjectReference{Name: "test-config-map"}},
					},
				},
				Env: []v1.EnvVar{
					{
						Name:  "TEST_LITERAL",
						Value: "test-test-test",
					},
					{
						Name:  "EXPANSION_TEST",
						Value: "$(REPLACE_ME)",
					},
					{
						Name:  "DUPE_TEST",
						Value: "ENV_VAR",
					},
				},
			},
			nilLister: false,
			configMap: &v1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "test1",
					Name:      "test-configmap",
				},
				Data: map[string]string{
					"REPLACE_ME": "FROM_CONFIG_MAP",
					"DUPE_TEST":  "CONFIG_MAP",
				},
			},
			expectedEnvs: []kubecontainer.EnvVar{
				{
					Name:  "TEST_LITERAL",
					Value: "test-test-test",
				},
				{
					Name:  "REPLACE_ME",
					Value: "FROM_CONFIG_MAP",
				},
				{
					Name:  "EXPANSION_TEST",
					Value: "FROM_CONFIG_MAP",
				},
				{
					Name:  "DUPE_TEST",
					Value: "ENV_VAR",
				},
				{
					Name:  "p_REPLACE_ME",
					Value: "FROM_CONFIG_MAP",
				},
				{
					Name:  "p_DUPE_TEST",
					Value: "CONFIG_MAP",
				},
				{
					Name:  "KUBERNETES_SERVICE_HOST",
					Value: "1.2.3.1",
				},
				{
					Name:  "KUBERNETES_SERVICE_PORT",
					Value: "8081",
				},
				{
					Name:  "KUBERNETES_PORT",
					Value: "tcp://1.2.3.1:8081",
				},
				{
					Name:  "KUBERNETES_PORT_8081_TCP",
					Value: "tcp://1.2.3.1:8081",
				},
				{
					Name:  "KUBERNETES_PORT_8081_TCP_PROTO",
					Value: "tcp",
				},
				{
					Name:  "KUBERNETES_PORT_8081_TCP_PORT",
					Value: "8081",
				},
				{
					Name:  "KUBERNETES_PORT_8081_TCP_ADDR",
					Value: "1.2.3.1",
				},
			},
		},
		{
			name:               "configmap allow prefix to start with a digital",
			ns:                 "test1",
			enableServiceLinks: &falseValue,
			enableRelaxedEnvironmentVariableValidation: true,
			container: &v1.Container{
				EnvFrom: []v1.EnvFromSource{
					{
						ConfigMapRef: &v1.ConfigMapEnvSource{LocalObjectReference: v1.LocalObjectReference{Name: "test-config-map"}},
					},
					{
						Prefix:       "1_",
						ConfigMapRef: &v1.ConfigMapEnvSource{LocalObjectReference: v1.LocalObjectReference{Name: "test-config-map"}},
					},
				},
				Env: []v1.EnvVar{
					{
						Name:  "TEST_LITERAL",
						Value: "test-test-test",
					},
					{
						Name:  "EXPANSION_TEST",
						Value: "$(REPLACE_ME)",
					},
					{
						Name:  "DUPE_TEST",
						Value: "ENV_VAR",
					},
				},
			},
			nilLister: false,
			configMap: &v1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "test1",
					Name:      "test-configmap",
				},
				Data: map[string]string{
					"REPLACE_ME": "FROM_CONFIG_MAP",
					"DUPE_TEST":  "CONFIG_MAP",
				},
			},
			expectedEnvs: []kubecontainer.EnvVar{
				{
					Name:  "TEST_LITERAL",
					Value: "test-test-test",
				},
				{
					Name:  "REPLACE_ME",
					Value: "FROM_CONFIG_MAP",
				},
				{
					Name:  "EXPANSION_TEST",
					Value: "FROM_CONFIG_MAP",
				},
				{
					Name:  "DUPE_TEST",
					Value: "ENV_VAR",
				},
				{
					Name:  "1_REPLACE_ME",
					Value: "FROM_CONFIG_MAP",
				},
				{
					Name:  "1_DUPE_TEST",
					Value: "CONFIG_MAP",
				},
				{
					Name:  "KUBERNETES_SERVICE_HOST",
					Value: "1.2.3.1",
				},
				{
					Name:  "KUBERNETES_SERVICE_PORT",
					Value: "8081",
				},
				{
					Name:  "KUBERNETES_PORT",
					Value: "tcp://1.2.3.1:8081",
				},
				{
					Name:  "KUBERNETES_PORT_8081_TCP",
					Value: "tcp://1.2.3.1:8081",
				},
				{
					Name:  "KUBERNETES_PORT_8081_TCP_PROTO",
					Value: "tcp",
				},
				{
					Name:  "KUBERNETES_PORT_8081_TCP_PORT",
					Value: "8081",
				},
				{
					Name:  "KUBERNETES_PORT_8081_TCP_ADDR",
					Value: "1.2.3.1",
				},
			},
		},
		{
			name:               "configmap, service env vars",
			ns:                 "test1",
			enableServiceLinks: &trueValue,
			container: &v1.Container{
				EnvFrom: []v1.EnvFromSource{
					{
						ConfigMapRef: &v1.ConfigMapEnvSource{LocalObjectReference: v1.LocalObjectReference{Name: "test-config-map"}},
					},
					{
						Prefix:       "p_",
						ConfigMapRef: &v1.ConfigMapEnvSource{LocalObjectReference: v1.LocalObjectReference{Name: "test-config-map"}},
					},
				},
				Env: []v1.EnvVar{
					{
						Name:  "TEST_LITERAL",
						Value: "test-test-test",
					},
					{
						Name:  "EXPANSION_TEST",
						Value: "$(REPLACE_ME)",
					},
					{
						Name:  "DUPE_TEST",
						Value: "ENV_VAR",
					},
				},
			},
			nilLister: false,
			configMap: &v1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "test1",
					Name:      "test-configmap",
				},
				Data: map[string]string{
					"REPLACE_ME": "FROM_CONFIG_MAP",
					"DUPE_TEST":  "CONFIG_MAP",
				},
			},
			expectedEnvs: []kubecontainer.EnvVar{
				{
					Name:  "TEST_LITERAL",
					Value: "test-test-test",
				},
				{
					Name:  "TEST_SERVICE_HOST",
					Value: "1.2.3.3",
				},
				{
					Name:  "TEST_SERVICE_PORT",
					Value: "8083",
				},
				{
					Name:  "TEST_PORT",
					Value: "tcp://1.2.3.3:8083",
				},
				{
					Name:  "TEST_PORT_8083_TCP",
					Value: "tcp://1.2.3.3:8083",
				},
				{
					Name:  "TEST_PORT_8083_TCP_PROTO",
					Value: "tcp",
				},
				{
					Name:  "TEST_PORT_8083_TCP_PORT",
					Value: "8083",
				},
				{
					Name:  "TEST_PORT_8083_TCP_ADDR",
					Value: "1.2.3.3",
				},
				{
					Name:  "REPLACE_ME",
					Value: "FROM_CONFIG_MAP",
				},
				{
					Name:  "EXPANSION_TEST",
					Value: "FROM_CONFIG_MAP",
				},
				{
					Name:  "DUPE_TEST",
					Value: "ENV_VAR",
				},
				{
					Name:  "p_REPLACE_ME",
					Value: "FROM_CONFIG_MAP",
				},
				{
					Name:  "p_DUPE_TEST",
					Value: "CONFIG_MAP",
				},
				{
					Name:  "KUBERNETES_SERVICE_HOST",
					Value: "1.2.3.1",
				},
				{
					Name:  "KUBERNETES_SERVICE_PORT",
					Value: "8081",
				},
				{
					Name:  "KUBERNETES_PORT",
					Value: "tcp://1.2.3.1:8081",
				},
				{
					Name:  "KUBERNETES_PORT_8081_TCP",
					Value: "tcp://1.2.3.1:8081",
				},
				{
					Name:  "KUBERNETES_PORT_8081_TCP_PROTO",
					Value: "tcp",
				},
				{
					Name:  "KUBERNETES_PORT_8081_TCP_PORT",
					Value: "8081",
				},
				{
					Name:  "KUBERNETES_PORT_8081_TCP_ADDR",
					Value: "1.2.3.1",
				},
			},
		},
		{
			name:               "configmap_missing",
			ns:                 "test1",
			enableServiceLinks: &falseValue,
			container: &v1.Container{
				EnvFrom: []v1.EnvFromSource{
					{ConfigMapRef: &v1.ConfigMapEnvSource{LocalObjectReference: v1.LocalObjectReference{Name: "test-config-map"}}},
				},
			},
			expectedError: true,
		},
		{
			name:               "configmap_missing_optional",
			ns:                 "test",
			enableServiceLinks: &falseValue,
			container: &v1.Container{
				EnvFrom: []v1.EnvFromSource{
					{ConfigMapRef: &v1.ConfigMapEnvSource{
						Optional:             &trueVal,
						LocalObjectReference: v1.LocalObjectReference{Name: "missing-config-map"}}},
				},
			},
			expectedEnvs: []kubecontainer.EnvVar{
				{Name: "KUBERNETES_SERVICE_HOST", Value: "1.2.3.1"},
				{Name: "KUBERNETES_SERVICE_PORT", Value: "8081"},
				{Name: "KUBERNETES_PORT", Value: "tcp://1.2.3.1:8081"},
				{Name: "KUBERNETES_PORT_8081_TCP", Value: "tcp://1.2.3.1:8081"},
				{Name: "KUBERNETES_PORT_8081_TCP_PROTO", Value: "tcp"},
				{Name: "KUBERNETES_PORT_8081_TCP_PORT", Value: "8081"},
				{Name: "KUBERNETES_PORT_8081_TCP_ADDR", Value: "1.2.3.1"},
			},
		},
		{
			name:               "configmap_invalid_keys_valid",
			ns:                 "test",
			enableServiceLinks: &falseValue,
			container: &v1.Container{
				EnvFrom: []v1.EnvFromSource{
					{
						Prefix:       "p_",
						ConfigMapRef: &v1.ConfigMapEnvSource{LocalObjectReference: v1.LocalObjectReference{Name: "test-config-map"}},
					},
				},
			},
			configMap: &v1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "test1",
					Name:      "test-configmap",
				},
				Data: map[string]string{
					"1234": "abc",
				},
			},
			expectedEnvs: []kubecontainer.EnvVar{
				{
					Name:  "p_1234",
					Value: "abc",
				},
				{
					Name:  "KUBERNETES_SERVICE_HOST",
					Value: "1.2.3.1",
				},
				{
					Name:  "KUBERNETES_SERVICE_PORT",
					Value: "8081",
				},
				{
					Name:  "KUBERNETES_PORT",
					Value: "tcp://1.2.3.1:8081",
				},
				{
					Name:  "KUBERNETES_PORT_8081_TCP",
					Value: "tcp://1.2.3.1:8081",
				},
				{
					Name:  "KUBERNETES_PORT_8081_TCP_PROTO",
					Value: "tcp",
				},
				{
					Name:  "KUBERNETES_PORT_8081_TCP_PORT",
					Value: "8081",
				},
				{
					Name:  "KUBERNETES_PORT_8081_TCP_ADDR",
					Value: "1.2.3.1",
				},
			},
		},
		{
			name:               "secret",
			ns:                 "test1",
			enableServiceLinks: &falseValue,
			container: &v1.Container{
				EnvFrom: []v1.EnvFromSource{
					{
						SecretRef: &v1.SecretEnvSource{LocalObjectReference: v1.LocalObjectReference{Name: "test-secret"}},
					},
					{
						Prefix:    "p_",
						SecretRef: &v1.SecretEnvSource{LocalObjectReference: v1.LocalObjectReference{Name: "test-secret"}},
					},
				},
				Env: []v1.EnvVar{
					{
						Name:  "TEST_LITERAL",
						Value: "test-test-test",
					},
					{
						Name:  "EXPANSION_TEST",
						Value: "$(REPLACE_ME)",
					},
					{
						Name:  "DUPE_TEST",
						Value: "ENV_VAR",
					},
				},
			},
			nilLister: false,
			secret: &v1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "test1",
					Name:      "test-secret",
				},
				Data: map[string][]byte{
					"REPLACE_ME": []byte("FROM_SECRET"),
					"DUPE_TEST":  []byte("SECRET"),
				},
			},
			expectedEnvs: []kubecontainer.EnvVar{
				{
					Name:  "TEST_LITERAL",
					Value: "test-test-test",
				},
				{
					Name:  "REPLACE_ME",
					Value: "FROM_SECRET",
				},
				{
					Name:  "EXPANSION_TEST",
					Value: "FROM_SECRET",
				},
				{
					Name:  "DUPE_TEST",
					Value: "ENV_VAR",
				},
				{
					Name:  "p_REPLACE_ME",
					Value: "FROM_SECRET",
				},
				{
					Name:  "p_DUPE_TEST",
					Value: "SECRET",
				},
				{
					Name:  "KUBERNETES_SERVICE_HOST",
					Value: "1.2.3.1",
				},
				{
					Name:  "KUBERNETES_SERVICE_PORT",
					Value: "8081",
				},
				{
					Name:  "KUBERNETES_PORT",
					Value: "tcp://1.2.3.1:8081",
				},
				{
					Name:  "KUBERNETES_PORT_8081_TCP",
					Value: "tcp://1.2.3.1:8081",
				},
				{
					Name:  "KUBERNETES_PORT_8081_TCP_PROTO",
					Value: "tcp",
				},
				{
					Name:  "KUBERNETES_PORT_8081_TCP_PORT",
					Value: "8081",
				},
				{
					Name:  "KUBERNETES_PORT_8081_TCP_ADDR",
					Value: "1.2.3.1",
				},
			},
		},
		{
			name:               "secret, service env vars",
			ns:                 "test1",
			enableServiceLinks: &trueValue,
			container: &v1.Container{
				EnvFrom: []v1.EnvFromSource{
					{
						SecretRef: &v1.SecretEnvSource{LocalObjectReference: v1.LocalObjectReference{Name: "test-secret"}},
					},
					{
						Prefix:    "p_",
						SecretRef: &v1.SecretEnvSource{LocalObjectReference: v1.LocalObjectReference{Name: "test-secret"}},
					},
				},
				Env: []v1.EnvVar{
					{
						Name:  "TEST_LITERAL",
						Value: "test-test-test",
					},
					{
						Name:  "EXPANSION_TEST",
						Value: "$(REPLACE_ME)",
					},
					{
						Name:  "DUPE_TEST",
						Value: "ENV_VAR",
					},
				},
			},
			nilLister: false,
			secret: &v1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "test1",
					Name:      "test-secret",
				},
				Data: map[string][]byte{
					"REPLACE_ME": []byte("FROM_SECRET"),
					"DUPE_TEST":  []byte("SECRET"),
				},
			},
			expectedEnvs: []kubecontainer.EnvVar{
				{
					Name:  "TEST_LITERAL",
					Value: "test-test-test",
				},
				{
					Name:  "TEST_SERVICE_HOST",
					Value: "1.2.3.3",
				},
				{
					Name:  "TEST_SERVICE_PORT",
					Value: "8083",
				},
				{
					Name:  "TEST_PORT",
					Value: "tcp://1.2.3.3:8083",
				},
				{
					Name:  "TEST_PORT_8083_TCP",
					Value: "tcp://1.2.3.3:8083",
				},
				{
					Name:  "TEST_PORT_8083_TCP_PROTO",
					Value: "tcp",
				},
				{
					Name:  "TEST_PORT_8083_TCP_PORT",
					Value: "8083",
				},
				{
					Name:  "TEST_PORT_8083_TCP_ADDR",
					Value: "1.2.3.3",
				},
				{
					Name:  "REPLACE_ME",
					Value: "FROM_SECRET",
				},
				{
					Name:  "EXPANSION_TEST",
					Value: "FROM_SECRET",
				},
				{
					Name:  "DUPE_TEST",
					Value: "ENV_VAR",
				},
				{
					Name:  "p_REPLACE_ME",
					Value: "FROM_SECRET",
				},
				{
					Name:  "p_DUPE_TEST",
					Value: "SECRET",
				},
				{
					Name:  "KUBERNETES_SERVICE_HOST",
					Value: "1.2.3.1",
				},
				{
					Name:  "KUBERNETES_SERVICE_PORT",
					Value: "8081",
				},
				{
					Name:  "KUBERNETES_PORT",
					Value: "tcp://1.2.3.1:8081",
				},
				{
					Name:  "KUBERNETES_PORT_8081_TCP",
					Value: "tcp://1.2.3.1:8081",
				},
				{
					Name:  "KUBERNETES_PORT_8081_TCP_PROTO",
					Value: "tcp",
				},
				{
					Name:  "KUBERNETES_PORT_8081_TCP_PORT",
					Value: "8081",
				},
				{
					Name:  "KUBERNETES_PORT_8081_TCP_ADDR",
					Value: "1.2.3.1",
				},
			},
		},
		{
			name:               "secret_missing",
			ns:                 "test1",
			enableServiceLinks: &falseValue,
			container: &v1.Container{
				EnvFrom: []v1.EnvFromSource{
					{SecretRef: &v1.SecretEnvSource{LocalObjectReference: v1.LocalObjectReference{Name: "test-secret"}}},
				},
			},
			expectedError: true,
		},
		{
			name:               "secret_missing_optional",
			ns:                 "test",
			enableServiceLinks: &falseValue,
			container: &v1.Container{
				EnvFrom: []v1.EnvFromSource{
					{SecretRef: &v1.SecretEnvSource{
						LocalObjectReference: v1.LocalObjectReference{Name: "missing-secret"},
						Optional:             &trueVal}},
				},
			},
			expectedEnvs: []kubecontainer.EnvVar{
				{Name: "KUBERNETES_SERVICE_HOST", Value: "1.2.3.1"},
				{Name: "KUBERNETES_SERVICE_PORT", Value: "8081"},
				{Name: "KUBERNETES_PORT", Value: "tcp://1.2.3.1:8081"},
				{Name: "KUBERNETES_PORT_8081_TCP", Value: "tcp://1.2.3.1:8081"},
				{Name: "KUBERNETES_PORT_8081_TCP_PROTO", Value: "tcp"},
				{Name: "KUBERNETES_PORT_8081_TCP_PORT", Value: "8081"},
				{Name: "KUBERNETES_PORT_8081_TCP_ADDR", Value: "1.2.3.1"},
			},
		},
		{
			name:               "secret_invalid_keys_valid",
			ns:                 "test",
			enableServiceLinks: &falseValue,
			container: &v1.Container{
				EnvFrom: []v1.EnvFromSource{
					{
						Prefix:    "p_",
						SecretRef: &v1.SecretEnvSource{LocalObjectReference: v1.LocalObjectReference{Name: "test-secret"}},
					},
				},
			},
			secret: &v1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "test1",
					Name:      "test-secret",
				},
				Data: map[string][]byte{
					"1234.name": []byte("abc"),
				},
			},
			expectedEnvs: []kubecontainer.EnvVar{
				{
					Name:  "p_1234.name",
					Value: "abc",
				},
				{
					Name:  "KUBERNETES_SERVICE_HOST",
					Value: "1.2.3.1",
				},
				{
					Name:  "KUBERNETES_SERVICE_PORT",
					Value: "8081",
				},
				{
					Name:  "KUBERNETES_PORT",
					Value: "tcp://1.2.3.1:8081",
				},
				{
					Name:  "KUBERNETES_PORT_8081_TCP",
					Value: "tcp://1.2.3.1:8081",
				},
				{
					Name:  "KUBERNETES_PORT_8081_TCP_PROTO",
					Value: "tcp",
				},
				{
					Name:  "KUBERNETES_PORT_8081_TCP_PORT",
					Value: "8081",
				},
				{
					Name:  "KUBERNETES_PORT_8081_TCP_ADDR",
					Value: "1.2.3.1",
				},
			},
		},
		{
			name:               "nil_enableServiceLinks",
			ns:                 "test",
			enableServiceLinks: nil,
			container: &v1.Container{
				EnvFrom: []v1.EnvFromSource{
					{
						Prefix:    "p_",
						SecretRef: &v1.SecretEnvSource{LocalObjectReference: v1.LocalObjectReference{Name: "test-secret"}},
					},
				},
			},
			secret: &v1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "test1",
					Name:      "test-secret",
				},
				Data: map[string][]byte{
					"1234.name": []byte("abc"),
				},
			},
			expectedError: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.RelaxedEnvironmentVariableValidation, tc.enableRelaxedEnvironmentVariableValidation)

			fakeRecorder := record.NewFakeRecorder(1)
			testKubelet := newTestKubelet(t, false /* controllerAttachDetachEnabled */)
			testKubelet.kubelet.recorder = fakeRecorder
			defer testKubelet.Cleanup()
			kl := testKubelet.kubelet
			if tc.nilLister {
				kl.serviceLister = nil
			} else if tc.unsyncedServices {
				kl.serviceLister = testServiceLister{}
				kl.serviceHasSynced = func() bool { return false }
			} else {
				kl.serviceLister = testServiceLister{services}
				kl.serviceHasSynced = func() bool { return true }
			}

			testKubelet.fakeKubeClient.AddReactor("get", "configmaps", func(action core.Action) (bool, runtime.Object, error) {
				var err error
				if tc.configMap == nil {
					err = apierrors.NewNotFound(action.GetResource().GroupResource(), "configmap-name")
				}
				return true, tc.configMap, err
			})
			testKubelet.fakeKubeClient.AddReactor("get", "secrets", func(action core.Action) (bool, runtime.Object, error) {
				var err error
				if tc.secret == nil {
					err = apierrors.NewNotFound(action.GetResource().GroupResource(), "secret-name")
				}
				return true, tc.secret, err
			})

			testKubelet.fakeKubeClient.AddReactor("get", "secrets", func(action core.Action) (bool, runtime.Object, error) {
				var err error
				if tc.secret == nil {
					err = errors.New("no secret defined")
				}
				return true, tc.secret, err
			})

			testPod := &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Namespace:   tc.ns,
					Name:        "dapi-test-pod-name",
					Annotations: map[string]string{},
				},
				Spec: v1.PodSpec{
					ServiceAccountName: "special",
					NodeName:           "node-name",
					EnableServiceLinks: tc.enableServiceLinks,
				},
			}
			podIP := ""
			if len(tc.podIPs) > 0 {
				podIP = tc.podIPs[0]
			}
			if tc.staticPod {
				testPod.Annotations[kubetypes.ConfigSourceAnnotationKey] = "file"
			}

			result, err := kl.makeEnvironmentVariables(testPod, tc.container, podIP, tc.podIPs)
			select {
			case e := <-fakeRecorder.Events:
				assert.Equal(t, tc.expectedEvent, e)
			default:
				assert.Equal(t, "", tc.expectedEvent)
			}
			if tc.expectedError {
				assert.Error(t, err, tc.name)
			} else {
				assert.NoError(t, err, "[%s]", tc.name)

				sort.Sort(envs(result))
				sort.Sort(envs(tc.expectedEnvs))
				assert.Equal(t, tc.expectedEnvs, result, "[%s] env entries", tc.name)
			}
		})

	}
}

func waitingState(cName string) v1.ContainerStatus {
	return waitingStateWithReason(cName, "")
}
func waitingStateWithReason(cName, reason string) v1.ContainerStatus {
	return v1.ContainerStatus{
		Name: cName,
		State: v1.ContainerState{
			Waiting: &v1.ContainerStateWaiting{Reason: reason},
		},
	}
}
func waitingStateWithLastTermination(cName string) v1.ContainerStatus {
	return v1.ContainerStatus{
		Name: cName,
		State: v1.ContainerState{
			Waiting: &v1.ContainerStateWaiting{},
		},
		LastTerminationState: v1.ContainerState{
			Terminated: &v1.ContainerStateTerminated{
				ExitCode: 0,
			},
		},
	}
}
func waitingStateWithNonZeroTermination(cName string) v1.ContainerStatus {
	return v1.ContainerStatus{
		Name: cName,
		State: v1.ContainerState{
			Waiting: &v1.ContainerStateWaiting{},
		},
		LastTerminationState: v1.ContainerState{
			Terminated: &v1.ContainerStateTerminated{
				ExitCode: -1,
			},
		},
	}
}
func runningState(cName string) v1.ContainerStatus {
	return v1.ContainerStatus{
		Name: cName,
		State: v1.ContainerState{
			Running: &v1.ContainerStateRunning{},
		},
	}
}
func startedState(cName string) v1.ContainerStatus {
	started := true
	return v1.ContainerStatus{
		Name: cName,
		State: v1.ContainerState{
			Running: &v1.ContainerStateRunning{},
		},
		Started: &started,
	}
}
func runningStateWithStartedAt(cName string, startedAt time.Time) v1.ContainerStatus {
	return v1.ContainerStatus{
		Name: cName,
		State: v1.ContainerState{
			Running: &v1.ContainerStateRunning{StartedAt: metav1.Time{Time: startedAt}},
		},
	}
}
func stoppedState(cName string) v1.ContainerStatus {
	return v1.ContainerStatus{
		Name: cName,
		State: v1.ContainerState{
			Terminated: &v1.ContainerStateTerminated{},
		},
	}
}
func succeededState(cName string) v1.ContainerStatus {
	return v1.ContainerStatus{
		Name: cName,
		State: v1.ContainerState{
			Terminated: &v1.ContainerStateTerminated{
				ExitCode: 0,
			},
		},
	}
}
func failedState(cName string) v1.ContainerStatus {
	return v1.ContainerStatus{
		Name: cName,
		State: v1.ContainerState{
			Terminated: &v1.ContainerStateTerminated{
				ExitCode: -1,
			},
		},
	}
}
func waitingWithLastTerminationUnknown(cName string, restartCount int32) v1.ContainerStatus {
	return v1.ContainerStatus{
		Name: cName,
		State: v1.ContainerState{
			Waiting: &v1.ContainerStateWaiting{Reason: "ContainerCreating"},
		},
		LastTerminationState: v1.ContainerState{
			Terminated: &v1.ContainerStateTerminated{
				Reason:   kubecontainer.ContainerReasonStatusUnknown,
				Message:  "The container could not be located when the pod was deleted.  The container used to be Running",
				ExitCode: 137,
			},
		},
		RestartCount: restartCount,
	}
}
func ready(status v1.ContainerStatus) v1.ContainerStatus {
	status.Ready = true
	return status
}
func withID(status v1.ContainerStatus, id string) v1.ContainerStatus {
	status.ContainerID = id
	return status
}

func TestPodPhaseWithRestartAlways(t *testing.T) {
	desiredState := v1.PodSpec{
		NodeName: "machine",
		Containers: []v1.Container{
			{Name: "containerA"},
			{Name: "containerB"},
		},
		RestartPolicy: v1.RestartPolicyAlways,
	}

	tests := []struct {
		pod           *v1.Pod
		podIsTerminal bool
		status        v1.PodPhase
		test          string
	}{
		{
			&v1.Pod{Spec: desiredState, Status: v1.PodStatus{}},
			false,
			v1.PodPending,
			"waiting",
		},
		{
			&v1.Pod{
				Spec: desiredState,
				Status: v1.PodStatus{
					ContainerStatuses: []v1.ContainerStatus{
						runningState("containerA"),
						runningState("containerB"),
					},
				},
			},
			false,
			v1.PodRunning,
			"all running",
		},
		{
			&v1.Pod{
				Spec: desiredState,
				Status: v1.PodStatus{
					ContainerStatuses: []v1.ContainerStatus{
						stoppedState("containerA"),
						stoppedState("containerB"),
					},
				},
			},
			false,
			v1.PodRunning,
			"all stopped with restart always",
		},
		{
			&v1.Pod{
				Spec: desiredState,
				Status: v1.PodStatus{
					ContainerStatuses: []v1.ContainerStatus{
						succeededState("containerA"),
						succeededState("containerB"),
					},
				},
			},
			true,
			v1.PodSucceeded,
			"all succeeded with restart always, but the pod is terminal",
		},
		{
			&v1.Pod{
				Spec: desiredState,
				Status: v1.PodStatus{
					ContainerStatuses: []v1.ContainerStatus{
						succeededState("containerA"),
						failedState("containerB"),
					},
				},
			},
			true,
			v1.PodFailed,
			"all stopped with restart always, but the pod is terminal",
		},
		{
			&v1.Pod{
				Spec: desiredState,
				Status: v1.PodStatus{
					ContainerStatuses: []v1.ContainerStatus{
						runningState("containerA"),
						stoppedState("containerB"),
					},
				},
			},
			false,
			v1.PodRunning,
			"mixed state #1 with restart always",
		},
		{
			&v1.Pod{
				Spec: desiredState,
				Status: v1.PodStatus{
					ContainerStatuses: []v1.ContainerStatus{
						runningState("containerA"),
					},
				},
			},
			false,
			v1.PodPending,
			"mixed state #2 with restart always",
		},
		{
			&v1.Pod{
				Spec: desiredState,
				Status: v1.PodStatus{
					ContainerStatuses: []v1.ContainerStatus{
						runningState("containerA"),
						waitingState("containerB"),
					},
				},
			},
			false,
			v1.PodPending,
			"mixed state #3 with restart always",
		},
		{
			&v1.Pod{
				Spec: desiredState,
				Status: v1.PodStatus{
					ContainerStatuses: []v1.ContainerStatus{
						runningState("containerA"),
						waitingStateWithLastTermination("containerB"),
					},
				},
			},
			false,
			v1.PodRunning,
			"backoff crashloop container with restart always",
		},
	}
	for _, test := range tests {
		status := getPhase(test.pod, test.pod.Status.ContainerStatuses, test.podIsTerminal)
		assert.Equal(t, test.status, status, "[test %s]", test.test)
	}
}

func TestPodPhaseWithRestartAlwaysInitContainers(t *testing.T) {
	desiredState := v1.PodSpec{
		NodeName: "machine",
		InitContainers: []v1.Container{
			{Name: "containerX"},
		},
		Containers: []v1.Container{
			{Name: "containerA"},
			{Name: "containerB"},
		},
		RestartPolicy: v1.RestartPolicyAlways,
	}

	tests := []struct {
		pod    *v1.Pod
		status v1.PodPhase
		test   string
	}{
		{&v1.Pod{Spec: desiredState, Status: v1.PodStatus{}}, v1.PodPending, "empty, waiting"},
		{
			&v1.Pod{
				Spec: desiredState,
				Status: v1.PodStatus{
					InitContainerStatuses: []v1.ContainerStatus{
						runningState("containerX"),
					},
				},
			},
			v1.PodPending,
			"init container running",
		},
		{
			&v1.Pod{
				Spec: desiredState,
				Status: v1.PodStatus{
					InitContainerStatuses: []v1.ContainerStatus{
						failedState("containerX"),
					},
				},
			},
			v1.PodPending,
			"init container terminated non-zero",
		},
		{
			&v1.Pod{
				Spec: desiredState,
				Status: v1.PodStatus{
					InitContainerStatuses: []v1.ContainerStatus{
						waitingStateWithLastTermination("containerX"),
					},
				},
			},
			v1.PodPending,
			"init container waiting, terminated zero",
		},
		{
			&v1.Pod{
				Spec: desiredState,
				Status: v1.PodStatus{
					InitContainerStatuses: []v1.ContainerStatus{
						waitingStateWithNonZeroTermination("containerX"),
					},
				},
			},
			v1.PodPending,
			"init container waiting, terminated non-zero",
		},
		{
			&v1.Pod{
				Spec: desiredState,
				Status: v1.PodStatus{
					InitContainerStatuses: []v1.ContainerStatus{
						waitingState("containerX"),
					},
				},
			},
			v1.PodPending,
			"init container waiting, not terminated",
		},
		{
			&v1.Pod{
				Spec: desiredState,
				Status: v1.PodStatus{
					InitContainerStatuses: []v1.ContainerStatus{
						succeededState("containerX"),
					},
					ContainerStatuses: []v1.ContainerStatus{
						runningState("containerA"),
						runningState("containerB"),
					},
				},
			},
			v1.PodRunning,
			"init container succeeded",
		},
	}
	for _, test := range tests {
		statusInfo := test.pod.Status.InitContainerStatuses
		statusInfo = append(statusInfo, test.pod.Status.ContainerStatuses...)
		status := getPhase(test.pod, statusInfo, false)
		assert.Equal(t, test.status, status, "[test %s]", test.test)
	}
}

func TestPodPhaseWithRestartAlwaysRestartableInitContainers(t *testing.T) {
	desiredState := v1.PodSpec{
		NodeName: "machine",
		InitContainers: []v1.Container{
			{Name: "containerX", RestartPolicy: &containerRestartPolicyAlways},
		},
		Containers: []v1.Container{
			{Name: "containerA"},
			{Name: "containerB"},
		},
		RestartPolicy: v1.RestartPolicyAlways,
	}

	tests := []struct {
		pod           *v1.Pod
		podIsTerminal bool
		status        v1.PodPhase
		test          string
	}{
		{&v1.Pod{Spec: desiredState, Status: v1.PodStatus{}}, false, v1.PodPending, "empty, waiting"},
		{
			&v1.Pod{
				Spec: desiredState,
				Status: v1.PodStatus{
					InitContainerStatuses: []v1.ContainerStatus{
						runningState("containerX"),
					},
				},
			},
			false,
			v1.PodPending,
			"restartable init container running",
		},
		{
			&v1.Pod{
				Spec: desiredState,
				Status: v1.PodStatus{
					InitContainerStatuses: []v1.ContainerStatus{
						stoppedState("containerX"),
					},
				},
			},
			false,
			v1.PodPending,
			"restartable init container stopped",
		},
		{
			&v1.Pod{
				Spec: desiredState,
				Status: v1.PodStatus{
					InitContainerStatuses: []v1.ContainerStatus{
						waitingStateWithLastTermination("containerX"),
					},
				},
			},
			false,
			v1.PodPending,
			"restartable init container waiting, terminated zero",
		},
		{
			&v1.Pod{
				Spec: desiredState,
				Status: v1.PodStatus{
					InitContainerStatuses: []v1.ContainerStatus{
						waitingStateWithNonZeroTermination("containerX"),
					},
				},
			},
			false,
			v1.PodPending,
			"restartable init container waiting, terminated non-zero",
		},
		{
			&v1.Pod{
				Spec: desiredState,
				Status: v1.PodStatus{
					InitContainerStatuses: []v1.ContainerStatus{
						waitingState("containerX"),
					},
				},
			},
			false,
			v1.PodPending,
			"restartable init container waiting, not terminated",
		},
		{
			&v1.Pod{
				Spec: desiredState,
				Status: v1.PodStatus{
					InitContainerStatuses: []v1.ContainerStatus{
						startedState("containerX"),
					},
					ContainerStatuses: []v1.ContainerStatus{
						runningState("containerA"),
					},
				},
			},
			false,
			v1.PodPending,
			"restartable init container started, 1/2 regular container running",
		},
		{
			&v1.Pod{
				Spec: desiredState,
				Status: v1.PodStatus{
					InitContainerStatuses: []v1.ContainerStatus{
						startedState("containerX"),
					},
					ContainerStatuses: []v1.ContainerStatus{
						runningState("containerA"),
						runningState("containerB"),
					},
				},
			},
			false,
			v1.PodRunning,
			"restartable init container started, all regular containers running",
		},
		{
			&v1.Pod{
				Spec: desiredState,
				Status: v1.PodStatus{
					InitContainerStatuses: []v1.ContainerStatus{
						runningState("containerX"),
					},
					ContainerStatuses: []v1.ContainerStatus{
						runningState("containerA"),
						runningState("containerB"),
					},
				},
			},
			false,
			v1.PodRunning,
			"restartable init container running, all regular containers running",
		},
		{
			&v1.Pod{
				Spec: desiredState,
				Status: v1.PodStatus{
					InitContainerStatuses: []v1.ContainerStatus{
						stoppedState("containerX"),
					},
					ContainerStatuses: []v1.ContainerStatus{
						runningState("containerA"),
						runningState("containerB"),
					},
				},
			},
			false,
			v1.PodRunning,
			"restartable init container stopped, all regular containers running",
		},
		{
			&v1.Pod{
				Spec: desiredState,
				Status: v1.PodStatus{
					InitContainerStatuses: []v1.ContainerStatus{
						waitingStateWithLastTermination("containerX"),
					},
					ContainerStatuses: []v1.ContainerStatus{
						runningState("containerA"),
						runningState("containerB"),
					},
				},
			},
			false,
			v1.PodRunning,
			"backoff crashloop restartable init container, all regular containers running",
		},
		{
			&v1.Pod{
				Spec: desiredState,
				Status: v1.PodStatus{
					InitContainerStatuses: []v1.ContainerStatus{
						failedState("containerX"),
					},
					ContainerStatuses: []v1.ContainerStatus{
						succeededState("containerA"),
						succeededState("containerB"),
					},
				},
			},
			true,
			v1.PodSucceeded,
			"all regular containers succeeded and restartable init container failed with restart always, but the pod is terminal",
		},
		{
			&v1.Pod{
				Spec: desiredState,
				Status: v1.PodStatus{
					InitContainerStatuses: []v1.ContainerStatus{
						succeededState("containerX"),
					},
					ContainerStatuses: []v1.ContainerStatus{
						succeededState("containerA"),
						succeededState("containerB"),
					},
				},
			},
			true,
			v1.PodSucceeded,
			"all regular containers succeeded and restartable init container succeeded with restart always, but the pod is terminal",
		},
	}
	for _, test := range tests {
		statusInfo := test.pod.Status.InitContainerStatuses
		statusInfo = append(statusInfo, test.pod.Status.ContainerStatuses...)
		status := getPhase(test.pod, statusInfo, test.podIsTerminal)
		assert.Equal(t, test.status, status, "[test %s]", test.test)
	}
}

func TestPodPhaseWithRestartAlwaysAndPodHasRun(t *testing.T) {
	desiredState := v1.PodSpec{
		NodeName: "machine",
		InitContainers: []v1.Container{
			{Name: "containerX"},
			{Name: "containerY", RestartPolicy: &containerRestartPolicyAlways},
		},
		Containers: []v1.Container{
			{Name: "containerA"},
		},
		RestartPolicy: v1.RestartPolicyAlways,
	}

	tests := []struct {
		pod    *v1.Pod
		status v1.PodPhase
		test   string
	}{
		{
			&v1.Pod{
				Spec: desiredState,
				Status: v1.PodStatus{
					InitContainerStatuses: []v1.ContainerStatus{
						runningState("containerX"),
						runningState("containerY"),
					},
					ContainerStatuses: []v1.ContainerStatus{
						runningState("containerA"),
					},
				},
			},
			v1.PodPending,
			"regular init containers, restartable init container and regular container are all running",
		},
		{
			&v1.Pod{
				Spec: desiredState,
				Status: v1.PodStatus{
					InitContainerStatuses: []v1.ContainerStatus{
						runningState("containerX"),
						runningState("containerY"),
					},
					ContainerStatuses: []v1.ContainerStatus{
						stoppedState("containerA"),
					},
				},
			},
			v1.PodPending,
			"regular containers is stopped, restartable init container and regular int container are both running",
		},
		{
			&v1.Pod{
				Spec: desiredState,
				Status: v1.PodStatus{
					InitContainerStatuses: []v1.ContainerStatus{
						succeededState("containerX"),
						runningState("containerY"),
					},
					ContainerStatuses: []v1.ContainerStatus{
						stoppedState("containerA"),
					},
				},
			},
			v1.PodRunning,
			"regular init container is succeeded, restartable init container is running, regular containers is stopped",
		},
		{
			&v1.Pod{
				Spec: desiredState,
				Status: v1.PodStatus{
					InitContainerStatuses: []v1.ContainerStatus{
						succeededState("containerX"),
						runningState("containerY"),
					},
					ContainerStatuses: []v1.ContainerStatus{
						runningState("containerA"),
					},
				},
			},
			v1.PodRunning,
			"regular init container is succeeded, restartable init container and regular containers are both running",
		},
	}
	for _, test := range tests {
		statusInfo := test.pod.Status.InitContainerStatuses
		statusInfo = append(statusInfo, test.pod.Status.ContainerStatuses...)
		status := getPhase(test.pod, statusInfo, false)
		assert.Equal(t, test.status, status, "[test %s]", test.test)
	}
}

func TestPodPhaseWithRestartNever(t *testing.T) {
	desiredState := v1.PodSpec{
		NodeName: "machine",
		Containers: []v1.Container{
			{Name: "containerA"},
			{Name: "containerB"},
		},
		RestartPolicy: v1.RestartPolicyNever,
	}

	tests := []struct {
		pod    *v1.Pod
		status v1.PodPhase
		test   string
	}{
		{&v1.Pod{Spec: desiredState, Status: v1.PodStatus{}}, v1.PodPending, "waiting"},
		{
			&v1.Pod{
				Spec: desiredState,
				Status: v1.PodStatus{
					ContainerStatuses: []v1.ContainerStatus{
						runningState("containerA"),
						runningState("containerB"),
					},
				},
			},
			v1.PodRunning,
			"all running with restart never",
		},
		{
			&v1.Pod{
				Spec: desiredState,
				Status: v1.PodStatus{
					ContainerStatuses: []v1.ContainerStatus{
						succeededState("containerA"),
						succeededState("containerB"),
					},
				},
			},
			v1.PodSucceeded,
			"all succeeded with restart never",
		},
		{
			&v1.Pod{
				Spec: desiredState,
				Status: v1.PodStatus{
					ContainerStatuses: []v1.ContainerStatus{
						failedState("containerA"),
						failedState("containerB"),
					},
				},
			},
			v1.PodFailed,
			"all failed with restart never",
		},
		{
			&v1.Pod{
				Spec: desiredState,
				Status: v1.PodStatus{
					ContainerStatuses: []v1.ContainerStatus{
						runningState("containerA"),
						succeededState("containerB"),
					},
				},
			},
			v1.PodRunning,
			"mixed state #1 with restart never",
		},
		{
			&v1.Pod{
				Spec: desiredState,
				Status: v1.PodStatus{
					ContainerStatuses: []v1.ContainerStatus{
						runningState("containerA"),
					},
				},
			},
			v1.PodPending,
			"mixed state #2 with restart never",
		},
		{
			&v1.Pod{
				Spec: desiredState,
				Status: v1.PodStatus{
					ContainerStatuses: []v1.ContainerStatus{
						runningState("containerA"),
						waitingState("containerB"),
					},
				},
			},
			v1.PodPending,
			"mixed state #3 with restart never",
		},
	}
	for _, test := range tests {
		status := getPhase(test.pod, test.pod.Status.ContainerStatuses, false)
		assert.Equal(t, test.status, status, "[test %s]", test.test)
	}
}

func TestPodPhaseWithRestartNeverInitContainers(t *testing.T) {
	desiredState := v1.PodSpec{
		NodeName: "machine",
		InitContainers: []v1.Container{
			{Name: "containerX"},
		},
		Containers: []v1.Container{
			{Name: "containerA"},
			{Name: "containerB"},
		},
		RestartPolicy: v1.RestartPolicyNever,
	}

	tests := []struct {
		pod    *v1.Pod
		status v1.PodPhase
		test   string
	}{
		{&v1.Pod{Spec: desiredState, Status: v1.PodStatus{}}, v1.PodPending, "empty, waiting"},
		{
			&v1.Pod{
				Spec: desiredState,
				Status: v1.PodStatus{
					InitContainerStatuses: []v1.ContainerStatus{
						runningState("containerX"),
					},
				},
			},
			v1.PodPending,
			"init container running",
		},
		{
			&v1.Pod{
				Spec: desiredState,
				Status: v1.PodStatus{
					InitContainerStatuses: []v1.ContainerStatus{
						failedState("containerX"),
					},
				},
			},
			v1.PodFailed,
			"init container terminated non-zero",
		},
		{
			&v1.Pod{
				Spec: desiredState,
				Status: v1.PodStatus{
					InitContainerStatuses: []v1.ContainerStatus{
						waitingStateWithLastTermination("containerX"),
					},
				},
			},
			v1.PodPending,
			"init container waiting, terminated zero",
		},
		{
			&v1.Pod{
				Spec: desiredState,
				Status: v1.PodStatus{
					InitContainerStatuses: []v1.ContainerStatus{
						waitingStateWithNonZeroTermination("containerX"),
					},
				},
			},
			v1.PodFailed,
			"init container waiting, terminated non-zero",
		},
		{
			&v1.Pod{
				Spec: desiredState,
				Status: v1.PodStatus{
					InitContainerStatuses: []v1.ContainerStatus{
						waitingState("containerX"),
					},
				},
			},
			v1.PodPending,
			"init container waiting, not terminated",
		},
		{
			&v1.Pod{
				Spec: desiredState,
				Status: v1.PodStatus{
					InitContainerStatuses: []v1.ContainerStatus{
						succeededState("containerX"),
					},
					ContainerStatuses: []v1.ContainerStatus{
						runningState("containerA"),
						runningState("containerB"),
					},
				},
			},
			v1.PodRunning,
			"init container succeeded",
		},
	}
	for _, test := range tests {
		statusInfo := test.pod.Status.InitContainerStatuses
		statusInfo = append(statusInfo, test.pod.Status.ContainerStatuses...)
		status := getPhase(test.pod, statusInfo, false)
		assert.Equal(t, test.status, status, "[test %s]", test.test)
	}
}

func TestPodPhaseWithRestartNeverRestartableInitContainers(t *testing.T) {
	desiredState := v1.PodSpec{
		NodeName: "machine",
		InitContainers: []v1.Container{
			{Name: "containerX", RestartPolicy: &containerRestartPolicyAlways},
		},
		Containers: []v1.Container{
			{Name: "containerA"},
			{Name: "containerB"},
		},
		RestartPolicy: v1.RestartPolicyNever,
	}

	tests := []struct {
		pod    *v1.Pod
		status v1.PodPhase
		test   string
	}{
		{&v1.Pod{Spec: desiredState, Status: v1.PodStatus{}}, v1.PodPending, "empty, waiting"},
		{
			&v1.Pod{
				Spec: desiredState,
				Status: v1.PodStatus{
					InitContainerStatuses: []v1.ContainerStatus{
						runningState("containerX"),
					},
				},
			},
			v1.PodPending,
			"restartable init container running",
		},
		{
			&v1.Pod{
				Spec: desiredState,
				Status: v1.PodStatus{
					InitContainerStatuses: []v1.ContainerStatus{
						stoppedState("containerX"),
					},
				},
			},
			v1.PodPending,
			"restartable init container stopped",
		},
		{
			&v1.Pod{
				Spec: desiredState,
				Status: v1.PodStatus{
					InitContainerStatuses: []v1.ContainerStatus{
						waitingStateWithLastTermination("containerX"),
					},
				},
			},
			v1.PodPending,
			"restartable init container waiting, terminated zero",
		},
		{
			&v1.Pod{
				Spec: desiredState,
				Status: v1.PodStatus{
					InitContainerStatuses: []v1.ContainerStatus{
						waitingStateWithNonZeroTermination("containerX"),
					},
				},
			},
			v1.PodPending,
			"restartable init container waiting, terminated non-zero",
		},
		{
			&v1.Pod{
				Spec: desiredState,
				Status: v1.PodStatus{
					InitContainerStatuses: []v1.ContainerStatus{
						waitingState("containerX"),
					},
				},
			},
			v1.PodPending,
			"restartable init container waiting, not terminated",
		},
		{
			&v1.Pod{
				Spec: desiredState,
				Status: v1.PodStatus{
					InitContainerStatuses: []v1.ContainerStatus{
						startedState("containerX"),
					},
					ContainerStatuses: []v1.ContainerStatus{
						runningState("containerA"),
					},
				},
			},
			v1.PodPending,
			"restartable init container started, one main container running",
		},
		{
			&v1.Pod{
				Spec: desiredState,
				Status: v1.PodStatus{
					InitContainerStatuses: []v1.ContainerStatus{
						startedState("containerX"),
					},
					ContainerStatuses: []v1.ContainerStatus{
						succeededState("containerA"),
						succeededState("containerB"),
					},
				},
			},
			v1.PodRunning,
			"restartable init container started, main containers succeeded",
		},
		{
			&v1.Pod{
				Spec: desiredState,
				Status: v1.PodStatus{
					InitContainerStatuses: []v1.ContainerStatus{
						runningState("containerX"),
					},
					ContainerStatuses: []v1.ContainerStatus{
						succeededState("containerA"),
						succeededState("containerB"),
					},
				},
			},
			v1.PodRunning,
			"restartable init container running, main containers succeeded",
		},
		{
			&v1.Pod{
				Spec: desiredState,
				Status: v1.PodStatus{
					InitContainerStatuses: []v1.ContainerStatus{
						succeededState("containerX"),
					},
					ContainerStatuses: []v1.ContainerStatus{
						succeededState("containerA"),
						succeededState("containerB"),
					},
				},
			},
			v1.PodSucceeded,
			"all containers succeeded",
		},
		{
			&v1.Pod{
				Spec: desiredState,
				Status: v1.PodStatus{
					InitContainerStatuses: []v1.ContainerStatus{
						failedState("containerX"),
					},
					ContainerStatuses: []v1.ContainerStatus{
						succeededState("containerA"),
						succeededState("containerB"),
					},
				},
			},
			v1.PodSucceeded,
			"restartable init container terminated non-zero, main containers succeeded",
		},
		{
			&v1.Pod{
				Spec: desiredState,
				Status: v1.PodStatus{
					InitContainerStatuses: []v1.ContainerStatus{
						waitingStateWithLastTermination("containerX"),
					},
					ContainerStatuses: []v1.ContainerStatus{
						succeededState("containerA"),
						succeededState("containerB"),
					},
				},
			},
			v1.PodSucceeded,
			"backoff crashloop restartable init container, main containers succeeded",
		},
		{
			&v1.Pod{
				Spec: desiredState,
				Status: v1.PodStatus{
					InitContainerStatuses: []v1.ContainerStatus{
						waitingStateWithNonZeroTermination("containerX"),
					},
					ContainerStatuses: []v1.ContainerStatus{
						succeededState("containerA"),
						succeededState("containerB"),
					},
				},
			},
			v1.PodSucceeded,
			"backoff crashloop with non-zero restartable init container, main containers succeeded",
		},
	}
	for _, test := range tests {
		statusInfo := test.pod.Status.InitContainerStatuses
		statusInfo = append(statusInfo, test.pod.Status.ContainerStatuses...)
		status := getPhase(test.pod, statusInfo, false)
		assert.Equal(t, test.status, status, "[test %s]", test.test)
	}
}

func TestPodPhaseWithRestartOnFailure(t *testing.T) {
	desiredState := v1.PodSpec{
		NodeName: "machine",
		Containers: []v1.Container{
			{Name: "containerA"},
			{Name: "containerB"},
		},
		RestartPolicy: v1.RestartPolicyOnFailure,
	}

	tests := []struct {
		pod    *v1.Pod
		status v1.PodPhase
		test   string
	}{
		{&v1.Pod{Spec: desiredState, Status: v1.PodStatus{}}, v1.PodPending, "waiting"},
		{
			&v1.Pod{
				Spec: desiredState,
				Status: v1.PodStatus{
					ContainerStatuses: []v1.ContainerStatus{
						runningState("containerA"),
						runningState("containerB"),
					},
				},
			},
			v1.PodRunning,
			"all running with restart onfailure",
		},
		{
			&v1.Pod{
				Spec: desiredState,
				Status: v1.PodStatus{
					ContainerStatuses: []v1.ContainerStatus{
						succeededState("containerA"),
						succeededState("containerB"),
					},
				},
			},
			v1.PodSucceeded,
			"all succeeded with restart onfailure",
		},
		{
			&v1.Pod{
				Spec: desiredState,
				Status: v1.PodStatus{
					ContainerStatuses: []v1.ContainerStatus{
						failedState("containerA"),
						failedState("containerB"),
					},
				},
			},
			v1.PodRunning,
			"all failed with restart never",
		},
		{
			&v1.Pod{
				Spec: desiredState,
				Status: v1.PodStatus{
					ContainerStatuses: []v1.ContainerStatus{
						runningState("containerA"),
						succeededState("containerB"),
					},
				},
			},
			v1.PodRunning,
			"mixed state #1 with restart onfailure",
		},
		{
			&v1.Pod{
				Spec: desiredState,
				Status: v1.PodStatus{
					ContainerStatuses: []v1.ContainerStatus{
						runningState("containerA"),
					},
				},
			},
			v1.PodPending,
			"mixed state #2 with restart onfailure",
		},
		{
			&v1.Pod{
				Spec: desiredState,
				Status: v1.PodStatus{
					ContainerStatuses: []v1.ContainerStatus{
						runningState("containerA"),
						waitingState("containerB"),
					},
				},
			},
			v1.PodPending,
			"mixed state #3 with restart onfailure",
		},
		{
			&v1.Pod{
				Spec: desiredState,
				Status: v1.PodStatus{
					ContainerStatuses: []v1.ContainerStatus{
						runningState("containerA"),
						waitingStateWithLastTermination("containerB"),
					},
				},
			},
			v1.PodRunning,
			"backoff crashloop container with restart onfailure",
		},
	}
	for _, test := range tests {
		status := getPhase(test.pod, test.pod.Status.ContainerStatuses, false)
		assert.Equal(t, test.status, status, "[test %s]", test.test)
	}
}

// No special init-specific logic for this, see RestartAlways case
// func TestPodPhaseWithRestartOnFailureInitContainers(t *testing.T) {
// }

func TestConvertToAPIContainerStatuses(t *testing.T) {
	desiredState := v1.PodSpec{
		NodeName: "machine",
		Containers: []v1.Container{
			{Name: "containerA"},
			{Name: "containerB"},
		},
		RestartPolicy: v1.RestartPolicyAlways,
	}
	now := metav1.Now()

	tests := []struct {
		name              string
		pod               *v1.Pod
		currentStatus     *kubecontainer.PodStatus
		previousStatus    []v1.ContainerStatus
		containers        []v1.Container
		hasInitContainers bool
		isInitContainer   bool
		expected          []v1.ContainerStatus
	}{
		{
			name: "no current status, with previous statuses and deletion",
			pod: &v1.Pod{
				Spec: desiredState,
				Status: v1.PodStatus{
					ContainerStatuses: []v1.ContainerStatus{
						runningState("containerA"),
						runningState("containerB"),
					},
				},
				ObjectMeta: metav1.ObjectMeta{Name: "my-pod", DeletionTimestamp: &now},
			},
			currentStatus: &kubecontainer.PodStatus{},
			previousStatus: []v1.ContainerStatus{
				runningState("containerA"),
				runningState("containerB"),
			},
			containers: desiredState.Containers,
			// no init containers
			// is not an init container
			expected: []v1.ContainerStatus{
				waitingWithLastTerminationUnknown("containerA", 0),
				waitingWithLastTerminationUnknown("containerB", 0),
			},
		},
		{
			name: "no current status, with previous statuses and no deletion",
			pod: &v1.Pod{
				Spec: desiredState,
				Status: v1.PodStatus{
					ContainerStatuses: []v1.ContainerStatus{
						runningState("containerA"),
						runningState("containerB"),
					},
				},
			},
			currentStatus: &kubecontainer.PodStatus{},
			previousStatus: []v1.ContainerStatus{
				runningState("containerA"),
				runningState("containerB"),
			},
			containers: desiredState.Containers,
			// no init containers
			// is not an init container
			expected: []v1.ContainerStatus{
				waitingWithLastTerminationUnknown("containerA", 1),
				waitingWithLastTerminationUnknown("containerB", 1),
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			testKubelet := newTestKubelet(t, false /* controllerAttachDetachEnabled */)
			defer testKubelet.Cleanup()
			kl := testKubelet.kubelet
			containerStatuses := kl.convertToAPIContainerStatuses(
				test.pod,
				test.currentStatus,
				test.previousStatus,
				test.containers,
				test.hasInitContainers,
				test.isInitContainer,
			)
			for i, status := range containerStatuses {
				assert.Equal(t, test.expected[i], status, "[test %s]", test.name)
			}
		})
	}
}

func Test_generateAPIPodStatus(t *testing.T) {
	desiredState := v1.PodSpec{
		NodeName: "machine",
		Containers: []v1.Container{
			{Name: "containerA"},
			{Name: "containerB"},
		},
		RestartPolicy: v1.RestartPolicyAlways,
	}
	sandboxReadyStatus := &kubecontainer.PodStatus{
		SandboxStatuses: []*runtimeapi.PodSandboxStatus{
			{
				Network: &runtimeapi.PodSandboxNetworkStatus{
					Ip: "10.0.0.10",
				},
				Metadata: &runtimeapi.PodSandboxMetadata{Attempt: uint32(0)},
				State:    runtimeapi.PodSandboxState_SANDBOX_READY,
			},
		},
	}

	now := metav1.Now()
	normalized_now := now.Rfc3339Copy()

	tests := []struct {
		name                                       string
		pod                                        *v1.Pod
		currentStatus                              *kubecontainer.PodStatus
		unreadyContainer                           []string
		previousStatus                             v1.PodStatus
		isPodTerminal                              bool
		expected                                   v1.PodStatus
		expectedPodDisruptionCondition             *v1.PodCondition
		expectedPodReadyToStartContainersCondition v1.PodCondition
	}{
		{
			name: "pod disruption condition is copied over and the phase is set to failed when deleted",
			pod: &v1.Pod{
				Spec: desiredState,
				Status: v1.PodStatus{
					ContainerStatuses: []v1.ContainerStatus{
						runningState("containerA"),
						runningState("containerB"),
					},
					Conditions: []v1.PodCondition{{
						Type:               v1.DisruptionTarget,
						Status:             v1.ConditionTrue,
						LastTransitionTime: normalized_now,
					}},
				},
				ObjectMeta: metav1.ObjectMeta{Name: "my-pod", DeletionTimestamp: &now},
			},
			currentStatus: sandboxReadyStatus,
			previousStatus: v1.PodStatus{
				ContainerStatuses: []v1.ContainerStatus{
					runningState("containerA"),
					runningState("containerB"),
				},
				Conditions: []v1.PodCondition{{
					Type:               v1.DisruptionTarget,
					Status:             v1.ConditionTrue,
					LastTransitionTime: normalized_now,
				}},
			},
			isPodTerminal: true,
			expected: v1.PodStatus{
				Phase:    v1.PodFailed,
				HostIP:   "127.0.0.1",
				HostIPs:  []v1.HostIP{{IP: "127.0.0.1"}, {IP: "::1"}},
				QOSClass: v1.PodQOSBestEffort,
				Conditions: []v1.PodCondition{
					{Type: v1.PodInitialized, Status: v1.ConditionTrue},
					{Type: v1.PodReady, Status: v1.ConditionFalse, Reason: "PodFailed"},
					{Type: v1.ContainersReady, Status: v1.ConditionFalse, Reason: "PodFailed"},
					{Type: v1.PodScheduled, Status: v1.ConditionTrue},
				},
				ContainerStatuses: []v1.ContainerStatus{
					ready(waitingWithLastTerminationUnknown("containerA", 0)),
					ready(waitingWithLastTerminationUnknown("containerB", 0)),
				},
			},
			expectedPodDisruptionCondition: &v1.PodCondition{
				Type:               v1.DisruptionTarget,
				Status:             v1.ConditionTrue,
				LastTransitionTime: normalized_now,
			},
			expectedPodReadyToStartContainersCondition: v1.PodCondition{
				Type:   v1.PodReadyToStartContainers,
				Status: v1.ConditionTrue,
			},
		},
		{
			name: "current status ready, with previous statuses and deletion",
			pod: &v1.Pod{
				Spec: desiredState,
				Status: v1.PodStatus{
					ContainerStatuses: []v1.ContainerStatus{
						runningState("containerA"),
						runningState("containerB"),
					},
				},
				ObjectMeta: metav1.ObjectMeta{Name: "my-pod", DeletionTimestamp: &now},
			},
			currentStatus: sandboxReadyStatus,
			previousStatus: v1.PodStatus{
				ContainerStatuses: []v1.ContainerStatus{
					runningState("containerA"),
					runningState("containerB"),
				},
			},
			expected: v1.PodStatus{
				Phase:    v1.PodRunning,
				HostIP:   "127.0.0.1",
				HostIPs:  []v1.HostIP{{IP: "127.0.0.1"}, {IP: "::1"}},
				QOSClass: v1.PodQOSBestEffort,
				Conditions: []v1.PodCondition{
					{Type: v1.PodInitialized, Status: v1.ConditionTrue},
					{Type: v1.PodReady, Status: v1.ConditionTrue},
					{Type: v1.ContainersReady, Status: v1.ConditionTrue},
					{Type: v1.PodScheduled, Status: v1.ConditionTrue},
				},
				ContainerStatuses: []v1.ContainerStatus{
					ready(waitingWithLastTerminationUnknown("containerA", 0)),
					ready(waitingWithLastTerminationUnknown("containerB", 0)),
				},
			},
			expectedPodReadyToStartContainersCondition: v1.PodCondition{
				Type:   v1.PodReadyToStartContainers,
				Status: v1.ConditionTrue,
			},
		},
		{
			name: "current status ready, with previous statuses and no deletion",
			pod: &v1.Pod{
				Spec: desiredState,
				Status: v1.PodStatus{
					ContainerStatuses: []v1.ContainerStatus{
						runningState("containerA"),
						runningState("containerB"),
					},
				},
			},
			currentStatus: sandboxReadyStatus,
			previousStatus: v1.PodStatus{
				ContainerStatuses: []v1.ContainerStatus{
					runningState("containerA"),
					runningState("containerB"),
				},
			},
			expected: v1.PodStatus{
				Phase:    v1.PodRunning,
				HostIP:   "127.0.0.1",
				HostIPs:  []v1.HostIP{{IP: "127.0.0.1"}, {IP: "::1"}},
				QOSClass: v1.PodQOSBestEffort,
				Conditions: []v1.PodCondition{
					{Type: v1.PodInitialized, Status: v1.ConditionTrue},
					{Type: v1.PodReady, Status: v1.ConditionTrue},
					{Type: v1.ContainersReady, Status: v1.ConditionTrue},
					{Type: v1.PodScheduled, Status: v1.ConditionTrue},
				},
				ContainerStatuses: []v1.ContainerStatus{
					ready(waitingWithLastTerminationUnknown("containerA", 1)),
					ready(waitingWithLastTerminationUnknown("containerB", 1)),
				},
			},
			expectedPodReadyToStartContainersCondition: v1.PodCondition{
				Type:   v1.PodReadyToStartContainers,
				Status: v1.ConditionTrue,
			},
		},
		{
			name: "terminal phase cannot be changed (apiserver previous is succeeded)",
			pod: &v1.Pod{
				Spec: desiredState,
				Status: v1.PodStatus{
					Phase: v1.PodSucceeded,
					ContainerStatuses: []v1.ContainerStatus{
						runningState("containerA"),
						runningState("containerB"),
					},
				},
			},
			currentStatus: &kubecontainer.PodStatus{},
			previousStatus: v1.PodStatus{
				ContainerStatuses: []v1.ContainerStatus{
					runningState("containerA"),
					runningState("containerB"),
				},
			},
			expected: v1.PodStatus{
				Phase:    v1.PodSucceeded,
				HostIP:   "127.0.0.1",
				HostIPs:  []v1.HostIP{{IP: "127.0.0.1"}, {IP: "::1"}},
				QOSClass: v1.PodQOSBestEffort,
				Conditions: []v1.PodCondition{
					{Type: v1.PodInitialized, Status: v1.ConditionTrue, Reason: "PodCompleted"},
					{Type: v1.PodReady, Status: v1.ConditionFalse, Reason: "PodCompleted"},
					{Type: v1.ContainersReady, Status: v1.ConditionFalse, Reason: "PodCompleted"},
					{Type: v1.PodScheduled, Status: v1.ConditionTrue},
				},
				ContainerStatuses: []v1.ContainerStatus{
					ready(waitingWithLastTerminationUnknown("containerA", 1)),
					ready(waitingWithLastTerminationUnknown("containerB", 1)),
				},
			},
			expectedPodReadyToStartContainersCondition: v1.PodCondition{
				Type:   v1.PodReadyToStartContainers,
				Status: v1.ConditionFalse,
			},
		},
		{
			name: "terminal phase from previous status must remain terminal, restartAlways",
			pod: &v1.Pod{
				Spec: desiredState,
				Status: v1.PodStatus{
					Phase: v1.PodRunning,
					ContainerStatuses: []v1.ContainerStatus{
						runningState("containerA"),
						runningState("containerB"),
					},
				},
			},
			currentStatus: &kubecontainer.PodStatus{},
			previousStatus: v1.PodStatus{
				Phase: v1.PodSucceeded,
				ContainerStatuses: []v1.ContainerStatus{
					runningState("containerA"),
					runningState("containerB"),
				},
				// Reason and message should be preserved
				Reason:  "Test",
				Message: "test",
			},
			expected: v1.PodStatus{
				Phase:    v1.PodSucceeded,
				HostIP:   "127.0.0.1",
				HostIPs:  []v1.HostIP{{IP: "127.0.0.1"}, {IP: "::1"}},
				QOSClass: v1.PodQOSBestEffort,
				Conditions: []v1.PodCondition{
					{Type: v1.PodInitialized, Status: v1.ConditionTrue, Reason: "PodCompleted"},
					{Type: v1.PodReady, Status: v1.ConditionFalse, Reason: "PodCompleted"},
					{Type: v1.ContainersReady, Status: v1.ConditionFalse, Reason: "PodCompleted"},
					{Type: v1.PodScheduled, Status: v1.ConditionTrue},
				},
				ContainerStatuses: []v1.ContainerStatus{
					ready(waitingWithLastTerminationUnknown("containerA", 1)),
					ready(waitingWithLastTerminationUnknown("containerB", 1)),
				},
				Reason:  "Test",
				Message: "test",
			},
			expectedPodReadyToStartContainersCondition: v1.PodCondition{
				Type:   v1.PodReadyToStartContainers,
				Status: v1.ConditionFalse,
			},
		},
		{
			name: "terminal phase from previous status must remain terminal, restartNever",
			pod: &v1.Pod{
				Spec: v1.PodSpec{
					NodeName: "machine",
					Containers: []v1.Container{
						{Name: "containerA"},
						{Name: "containerB"},
					},
					RestartPolicy: v1.RestartPolicyNever,
				},
				Status: v1.PodStatus{
					Phase: v1.PodRunning,
					ContainerStatuses: []v1.ContainerStatus{
						runningState("containerA"),
						runningState("containerB"),
					},
				},
			},
			currentStatus: &kubecontainer.PodStatus{},
			previousStatus: v1.PodStatus{
				Phase: v1.PodSucceeded,
				ContainerStatuses: []v1.ContainerStatus{
					succeededState("containerA"),
					succeededState("containerB"),
				},
				// Reason and message should be preserved
				Reason:  "Test",
				Message: "test",
			},
			expected: v1.PodStatus{
				Phase:    v1.PodSucceeded,
				HostIP:   "127.0.0.1",
				HostIPs:  []v1.HostIP{{IP: "127.0.0.1"}, {IP: "::1"}},
				QOSClass: v1.PodQOSBestEffort,
				Conditions: []v1.PodCondition{
					{Type: v1.PodInitialized, Status: v1.ConditionTrue, Reason: "PodCompleted"},
					{Type: v1.PodReady, Status: v1.ConditionFalse, Reason: "PodCompleted"},
					{Type: v1.ContainersReady, Status: v1.ConditionFalse, Reason: "PodCompleted"},
					{Type: v1.PodScheduled, Status: v1.ConditionTrue},
				},
				ContainerStatuses: []v1.ContainerStatus{
					ready(succeededState("containerA")),
					ready(succeededState("containerB")),
				},
				Reason:  "Test",
				Message: "test",
			},
			expectedPodReadyToStartContainersCondition: v1.PodCondition{
				Type:   v1.PodReadyToStartContainers,
				Status: v1.ConditionFalse,
			},
		},
		{
			name: "running can revert to pending",
			pod: &v1.Pod{
				Spec: desiredState,
				Status: v1.PodStatus{
					Phase: v1.PodRunning,
					ContainerStatuses: []v1.ContainerStatus{
						runningState("containerA"),
						runningState("containerB"),
					},
				},
			},
			currentStatus: sandboxReadyStatus,
			previousStatus: v1.PodStatus{
				ContainerStatuses: []v1.ContainerStatus{
					waitingState("containerA"),
					waitingState("containerB"),
				},
			},
			expected: v1.PodStatus{
				Phase:    v1.PodPending,
				HostIP:   "127.0.0.1",
				HostIPs:  []v1.HostIP{{IP: "127.0.0.1"}, {IP: "::1"}},
				QOSClass: v1.PodQOSBestEffort,
				Conditions: []v1.PodCondition{
					{Type: v1.PodInitialized, Status: v1.ConditionTrue},
					{Type: v1.PodReady, Status: v1.ConditionTrue},
					{Type: v1.ContainersReady, Status: v1.ConditionTrue},
					{Type: v1.PodScheduled, Status: v1.ConditionTrue},
				},
				ContainerStatuses: []v1.ContainerStatus{
					ready(waitingStateWithReason("containerA", "ContainerCreating")),
					ready(waitingStateWithReason("containerB", "ContainerCreating")),
				},
			},
			expectedPodReadyToStartContainersCondition: v1.PodCondition{
				Type:   v1.PodReadyToStartContainers,
				Status: v1.ConditionTrue,
			},
		},
		{
			name: "reason and message are preserved when phase doesn't change",
			pod: &v1.Pod{
				Spec: desiredState,
				Status: v1.PodStatus{
					Phase: v1.PodRunning,
					ContainerStatuses: []v1.ContainerStatus{
						waitingState("containerA"),
						waitingState("containerB"),
					},
				},
			},
			currentStatus: &kubecontainer.PodStatus{
				SandboxStatuses: sandboxReadyStatus.SandboxStatuses,
				ContainerStatuses: []*kubecontainer.Status{
					{
						ID:        kubecontainer.ContainerID{ID: "foo"},
						Name:      "containerB",
						StartedAt: time.Unix(1, 0).UTC(),
						State:     kubecontainer.ContainerStateRunning,
					},
				},
			},
			previousStatus: v1.PodStatus{
				Phase:   v1.PodPending,
				Reason:  "Test",
				Message: "test",
				ContainerStatuses: []v1.ContainerStatus{
					waitingState("containerA"),
					runningState("containerB"),
				},
			},
			expected: v1.PodStatus{
				Phase:    v1.PodPending,
				Reason:   "Test",
				Message:  "test",
				HostIP:   "127.0.0.1",
				HostIPs:  []v1.HostIP{{IP: "127.0.0.1"}, {IP: "::1"}},
				QOSClass: v1.PodQOSBestEffort,
				Conditions: []v1.PodCondition{
					{Type: v1.PodInitialized, Status: v1.ConditionTrue},
					{Type: v1.PodReady, Status: v1.ConditionTrue},
					{Type: v1.ContainersReady, Status: v1.ConditionTrue},
					{Type: v1.PodScheduled, Status: v1.ConditionTrue},
				},
				ContainerStatuses: []v1.ContainerStatus{
					ready(waitingStateWithReason("containerA", "ContainerCreating")),
					ready(withID(runningStateWithStartedAt("containerB", time.Unix(1, 0).UTC()), "://foo")),
				},
			},
			expectedPodReadyToStartContainersCondition: v1.PodCondition{
				Type:   v1.PodReadyToStartContainers,
				Status: v1.ConditionTrue,
			},
		},
		{
			name: "reason and message are cleared when phase changes",
			pod: &v1.Pod{
				Spec: desiredState,
				Status: v1.PodStatus{
					Phase: v1.PodPending,
					ContainerStatuses: []v1.ContainerStatus{
						waitingState("containerA"),
						waitingState("containerB"),
					},
				},
			},
			currentStatus: &kubecontainer.PodStatus{
				SandboxStatuses: sandboxReadyStatus.SandboxStatuses,
				ContainerStatuses: []*kubecontainer.Status{
					{
						ID:        kubecontainer.ContainerID{ID: "c1"},
						Name:      "containerA",
						StartedAt: time.Unix(1, 0).UTC(),
						State:     kubecontainer.ContainerStateRunning,
					},
					{
						ID:        kubecontainer.ContainerID{ID: "c2"},
						Name:      "containerB",
						StartedAt: time.Unix(2, 0).UTC(),
						State:     kubecontainer.ContainerStateRunning,
					},
				},
			},
			previousStatus: v1.PodStatus{
				Phase:   v1.PodPending,
				Reason:  "Test",
				Message: "test",
				ContainerStatuses: []v1.ContainerStatus{
					runningState("containerA"),
					runningState("containerB"),
				},
			},
			expected: v1.PodStatus{
				Phase:    v1.PodRunning,
				HostIP:   "127.0.0.1",
				HostIPs:  []v1.HostIP{{IP: "127.0.0.1"}, {IP: "::1"}},
				QOSClass: v1.PodQOSBestEffort,
				Conditions: []v1.PodCondition{
					{Type: v1.PodInitialized, Status: v1.ConditionTrue},
					{Type: v1.PodReady, Status: v1.ConditionTrue},
					{Type: v1.ContainersReady, Status: v1.ConditionTrue},
					{Type: v1.PodScheduled, Status: v1.ConditionTrue},
				},
				ContainerStatuses: []v1.ContainerStatus{
					ready(withID(runningStateWithStartedAt("containerA", time.Unix(1, 0).UTC()), "://c1")),
					ready(withID(runningStateWithStartedAt("containerB", time.Unix(2, 0).UTC()), "://c2")),
				},
			},
			expectedPodReadyToStartContainersCondition: v1.PodCondition{
				Type:   v1.PodReadyToStartContainers,
				Status: v1.ConditionTrue,
			},
		},
	}
	for _, test := range tests {
		for _, enablePodReadyToStartContainersCondition := range []bool{false, true} {
			t.Run(test.name, func(t *testing.T) {
				featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.PodReadyToStartContainersCondition, enablePodReadyToStartContainersCondition)
				testKubelet := newTestKubelet(t, false /* controllerAttachDetachEnabled */)
				defer testKubelet.Cleanup()
				kl := testKubelet.kubelet
				kl.statusManager.SetPodStatus(test.pod, test.previousStatus)
				for _, name := range test.unreadyContainer {
					kl.readinessManager.Set(kubecontainer.BuildContainerID("", findContainerStatusByName(test.expected, name).ContainerID), results.Failure, test.pod)
				}
				expected := test.expected.DeepCopy()
				actual := kl.generateAPIPodStatus(test.pod, test.currentStatus, test.isPodTerminal)
				if enablePodReadyToStartContainersCondition {
					expected.Conditions = append([]v1.PodCondition{test.expectedPodReadyToStartContainersCondition}, expected.Conditions...)
				}
				if test.expectedPodDisruptionCondition != nil {
					expected.Conditions = append([]v1.PodCondition{*test.expectedPodDisruptionCondition}, expected.Conditions...)
				}
				if !apiequality.Semantic.DeepEqual(*expected, actual) {
					t.Fatalf("Unexpected status: %s", cmp.Diff(*expected, actual))
				}
			})
		}
	}
}

func Test_generateAPIPodStatusForInPlaceVPAEnabled(t *testing.T) {
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.InPlacePodVerticalScaling, true)
	testContainerName := "ctr0"
	testContainerID := kubecontainer.ContainerID{Type: "test", ID: testContainerName}

	CPU1AndMem1G := v1.ResourceList{v1.ResourceCPU: resource.MustParse("1"), v1.ResourceMemory: resource.MustParse("1Gi")}
	CPU1AndMem1GAndStorage2G := CPU1AndMem1G.DeepCopy()
	CPU1AndMem1GAndStorage2G[v1.ResourceEphemeralStorage] = resource.MustParse("2Gi")
	CPU1AndMem1GAndStorage2GAndCustomResource := CPU1AndMem1GAndStorage2G.DeepCopy()
	CPU1AndMem1GAndStorage2GAndCustomResource["unknown-resource"] = resource.MustParse("1")

	testKubecontainerPodStatus := kubecontainer.PodStatus{
		ContainerStatuses: []*kubecontainer.Status{
			{
				ID:   testContainerID,
				Name: testContainerName,
				Resources: &kubecontainer.ContainerResources{
					CPURequest:    CPU1AndMem1G.Cpu(),
					MemoryRequest: CPU1AndMem1G.Memory(),
					CPULimit:      CPU1AndMem1G.Cpu(),
					MemoryLimit:   CPU1AndMem1G.Memory(),
				},
			},
		},
	}

	tests := []struct {
		name      string
		pod       *v1.Pod
		oldStatus *v1.PodStatus
	}{
		{
			name: "custom resource in ResourcesAllocated, resize should be null",
			pod: &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					UID:       "1234560",
					Name:      "foo0",
					Namespace: "bar0",
				},
				Spec: v1.PodSpec{
					NodeName: "machine",
					Containers: []v1.Container{
						{
							Name:      testContainerName,
							Image:     "img",
							Resources: v1.ResourceRequirements{Limits: CPU1AndMem1GAndStorage2GAndCustomResource, Requests: CPU1AndMem1GAndStorage2GAndCustomResource},
						},
					},
					RestartPolicy: v1.RestartPolicyAlways,
				},
				Status: v1.PodStatus{
					ContainerStatuses: []v1.ContainerStatus{
						{
							Name:               testContainerName,
							Resources:          &v1.ResourceRequirements{Limits: CPU1AndMem1GAndStorage2G, Requests: CPU1AndMem1GAndStorage2G},
							AllocatedResources: CPU1AndMem1GAndStorage2GAndCustomResource,
						},
					},
					Resize: "InProgress",
				},
			},
		},
		{
			name: "cpu/memory resource in ResourcesAllocated, resize should be null",
			pod: &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					UID:       "1234560",
					Name:      "foo0",
					Namespace: "bar0",
				},
				Spec: v1.PodSpec{
					NodeName: "machine",
					Containers: []v1.Container{
						{
							Name:      testContainerName,
							Image:     "img",
							Resources: v1.ResourceRequirements{Limits: CPU1AndMem1GAndStorage2G, Requests: CPU1AndMem1GAndStorage2G},
						},
					},
					RestartPolicy: v1.RestartPolicyAlways,
				},
				Status: v1.PodStatus{
					ContainerStatuses: []v1.ContainerStatus{
						{
							Name:               testContainerName,
							Resources:          &v1.ResourceRequirements{Limits: CPU1AndMem1GAndStorage2G, Requests: CPU1AndMem1GAndStorage2G},
							AllocatedResources: CPU1AndMem1GAndStorage2G,
						},
					},
					Resize: "InProgress",
				},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			testKubelet := newTestKubelet(t, false /* controllerAttachDetachEnabled */)
			defer testKubelet.Cleanup()
			kl := testKubelet.kubelet

			oldStatus := test.pod.Status
			kl.statusManager = status.NewFakeManager()
			kl.statusManager.SetPodStatus(test.pod, oldStatus)
			actual := kl.generateAPIPodStatus(test.pod, &testKubecontainerPodStatus /* criStatus */, false /* test.isPodTerminal */)

			if actual.Resize != "" {
				t.Fatalf("Unexpected Resize status: %s", actual.Resize)
			}
		})
	}
}

func findContainerStatusByName(status v1.PodStatus, name string) *v1.ContainerStatus {
	for i, c := range status.InitContainerStatuses {
		if c.Name == name {
			return &status.InitContainerStatuses[i]
		}
	}
	for i, c := range status.ContainerStatuses {
		if c.Name == name {
			return &status.ContainerStatuses[i]
		}
	}
	for i, c := range status.EphemeralContainerStatuses {
		if c.Name == name {
			return &status.EphemeralContainerStatuses[i]
		}
	}
	return nil
}

func TestGetExec(t *testing.T) {
	const (
		podName                = "podFoo"
		podNamespace           = "nsFoo"
		podUID       types.UID = "12345678"
		containerID            = "containerFoo"
		tty                    = true
	)
	var (
		podFullName = kubecontainer.GetPodFullName(podWithUIDNameNs(podUID, podName, podNamespace))
	)

	testcases := []struct {
		description string
		podFullName string
		container   string
		command     []string
		expectError bool
	}{{
		description: "success case",
		podFullName: podFullName,
		container:   containerID,
		command:     []string{"ls"},
		expectError: false,
	}, {
		description: "no such pod",
		podFullName: "bar" + podFullName,
		container:   containerID,
		command:     []string{"ls"},
		expectError: true,
	}, {
		description: "no such container",
		podFullName: podFullName,
		container:   "containerBar",
		command:     []string{"ls"},
		expectError: true,
	}, {
		description: "null exec command",
		podFullName: podFullName,
		container:   containerID,
		expectError: false,
	}, {
		description: "multi exec commands",
		podFullName: podFullName,
		container:   containerID,
		command:     []string{"bash", "-c", "ls"},
		expectError: false,
	}}

	for _, tc := range testcases {
		t.Run(tc.description, func(t *testing.T) {
			ctx := context.Background()
			testKubelet := newTestKubelet(t, false /* controllerAttachDetachEnabled */)
			defer testKubelet.Cleanup()
			kubelet := testKubelet.kubelet
			testKubelet.fakeRuntime.PodList = []*containertest.FakePod{
				{Pod: &kubecontainer.Pod{
					ID:        podUID,
					Name:      podName,
					Namespace: podNamespace,
					Containers: []*kubecontainer.Container{
						{Name: containerID,
							ID: kubecontainer.ContainerID{Type: "test", ID: containerID},
						},
					},
				}},
			}

			description := "streaming - " + tc.description
			fakeRuntime := &containertest.FakeStreamingRuntime{FakeRuntime: testKubelet.fakeRuntime}
			kubelet.containerRuntime = fakeRuntime
			kubelet.streamingRuntime = fakeRuntime

			redirect, err := kubelet.GetExec(ctx, tc.podFullName, podUID, tc.container, tc.command, remotecommand.Options{})
			if tc.expectError {
				assert.Error(t, err, description)
			} else {
				assert.NoError(t, err, description)
				assert.Equal(t, containertest.FakeHost, redirect.Host, description+": redirect")
			}
		})
	}
}

func TestGetPortForward(t *testing.T) {
	const (
		podName                = "podFoo"
		podNamespace           = "nsFoo"
		podUID       types.UID = "12345678"
		port         int32     = 5000
	)

	testcases := []struct {
		description string
		podName     string
		expectError bool
	}{{
		description: "success case",
		podName:     podName,
	}, {
		description: "no such pod",
		podName:     "bar",
		expectError: true,
	}}

	for _, tc := range testcases {
		ctx := context.Background()
		testKubelet := newTestKubelet(t, false /* controllerAttachDetachEnabled */)
		defer testKubelet.Cleanup()
		kubelet := testKubelet.kubelet
		testKubelet.fakeRuntime.PodList = []*containertest.FakePod{
			{Pod: &kubecontainer.Pod{
				ID:        podUID,
				Name:      podName,
				Namespace: podNamespace,
				Containers: []*kubecontainer.Container{
					{Name: "foo",
						ID: kubecontainer.ContainerID{Type: "test", ID: "foo"},
					},
				},
			}},
		}

		description := "streaming - " + tc.description
		fakeRuntime := &containertest.FakeStreamingRuntime{FakeRuntime: testKubelet.fakeRuntime}
		kubelet.containerRuntime = fakeRuntime
		kubelet.streamingRuntime = fakeRuntime

		redirect, err := kubelet.GetPortForward(ctx, tc.podName, podNamespace, podUID, portforward.V4Options{})
		if tc.expectError {
			assert.Error(t, err, description)
		} else {
			assert.NoError(t, err, description)
			assert.Equal(t, containertest.FakeHost, redirect.Host, description+": redirect")
		}
	}
}

func TestTruncatePodHostname(t *testing.T) {
	for c, test := range map[string]struct {
		input  string
		output string
	}{
		"valid hostname": {
			input:  "test.pod.hostname",
			output: "test.pod.hostname",
		},
		"too long hostname": {
			input:  "1234567.1234567.1234567.1234567.1234567.1234567.1234567.1234567.1234567.", // 8*9=72 chars
			output: "1234567.1234567.1234567.1234567.1234567.1234567.1234567.1234567",          //8*8-1=63 chars
		},
		"hostname end with .": {
			input:  "1234567.1234567.1234567.1234567.1234567.1234567.1234567.123456.1234567.", // 8*9-1=71 chars
			output: "1234567.1234567.1234567.1234567.1234567.1234567.1234567.123456",          //8*8-2=62 chars
		},
		"hostname end with -": {
			input:  "1234567.1234567.1234567.1234567.1234567.1234567.1234567.123456-1234567.", // 8*9-1=71 chars
			output: "1234567.1234567.1234567.1234567.1234567.1234567.1234567.123456",          //8*8-2=62 chars
		},
	} {
		t.Logf("TestCase: %q", c)
		output, err := truncatePodHostnameIfNeeded("test-pod", test.input)
		assert.NoError(t, err)
		assert.Equal(t, test.output, output)
	}
}

func TestGenerateAPIPodStatusHostNetworkPodIPs(t *testing.T) {
	testcases := []struct {
		name          string
		nodeAddresses []v1.NodeAddress
		criPodIPs     []string
		podIPs        []v1.PodIP
	}{
		{
			name: "Simple",
			nodeAddresses: []v1.NodeAddress{
				{Type: v1.NodeInternalIP, Address: "10.0.0.1"},
			},
			podIPs: []v1.PodIP{
				{IP: "10.0.0.1"},
			},
		},
		{
			name: "InternalIP is preferred over ExternalIP",
			nodeAddresses: []v1.NodeAddress{
				{Type: v1.NodeExternalIP, Address: "192.168.0.1"},
				{Type: v1.NodeInternalIP, Address: "10.0.0.1"},
			},
			podIPs: []v1.PodIP{
				{IP: "10.0.0.1"},
			},
		},
		{
			name: "Single-stack addresses in dual-stack cluster",
			nodeAddresses: []v1.NodeAddress{
				{Type: v1.NodeInternalIP, Address: "10.0.0.1"},
			},
			podIPs: []v1.PodIP{
				{IP: "10.0.0.1"},
			},
		},
		{
			name: "Multiple single-stack addresses in dual-stack cluster",
			nodeAddresses: []v1.NodeAddress{
				{Type: v1.NodeInternalIP, Address: "10.0.0.1"},
				{Type: v1.NodeInternalIP, Address: "10.0.0.2"},
				{Type: v1.NodeExternalIP, Address: "192.168.0.1"},
			},
			podIPs: []v1.PodIP{
				{IP: "10.0.0.1"},
			},
		},
		{
			name: "Dual-stack addresses in dual-stack cluster",
			nodeAddresses: []v1.NodeAddress{
				{Type: v1.NodeInternalIP, Address: "10.0.0.1"},
				{Type: v1.NodeInternalIP, Address: "fd01::1234"},
			},
			podIPs: []v1.PodIP{
				{IP: "10.0.0.1"},
				{IP: "fd01::1234"},
			},
		},
		{
			name: "CRI PodIPs override NodeAddresses",
			nodeAddresses: []v1.NodeAddress{
				{Type: v1.NodeInternalIP, Address: "10.0.0.1"},
				{Type: v1.NodeInternalIP, Address: "fd01::1234"},
			},
			criPodIPs: []string{"192.168.0.1"},
			podIPs: []v1.PodIP{
				{IP: "192.168.0.1"},
				{IP: "fd01::1234"},
			},
		},
		{
			name: "CRI dual-stack PodIPs override NodeAddresses",
			nodeAddresses: []v1.NodeAddress{
				{Type: v1.NodeInternalIP, Address: "10.0.0.1"},
				{Type: v1.NodeInternalIP, Address: "fd01::1234"},
			},
			criPodIPs: []string{"192.168.0.1", "2001:db8::2"},
			podIPs: []v1.PodIP{
				{IP: "192.168.0.1"},
				{IP: "2001:db8::2"},
			},
		},
		{
			// by default the cluster prefers IPv4
			name: "CRI dual-stack PodIPs override NodeAddresses prefer IPv4",
			nodeAddresses: []v1.NodeAddress{
				{Type: v1.NodeInternalIP, Address: "10.0.0.1"},
				{Type: v1.NodeInternalIP, Address: "fd01::1234"},
			},
			criPodIPs: []string{"2001:db8::2", "192.168.0.1"},
			podIPs: []v1.PodIP{
				{IP: "192.168.0.1"},
				{IP: "2001:db8::2"},
			},
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			testKubelet := newTestKubelet(t, false /* controllerAttachDetachEnabled */)
			defer testKubelet.Cleanup()
			kl := testKubelet.kubelet

			kl.nodeLister = testNodeLister{nodes: []*v1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{Name: string(kl.nodeName)},
					Status: v1.NodeStatus{
						Addresses: tc.nodeAddresses,
					},
				},
			}}

			pod := podWithUIDNameNs("12345", "test-pod", "test-namespace")
			pod.Spec.HostNetwork = true

			criStatus := &kubecontainer.PodStatus{
				ID:        pod.UID,
				Name:      pod.Name,
				Namespace: pod.Namespace,
				IPs:       tc.criPodIPs,
			}

			status := kl.generateAPIPodStatus(pod, criStatus, false)
			if !reflect.DeepEqual(status.PodIPs, tc.podIPs) {
				t.Fatalf("Expected PodIPs %#v, got %#v", tc.podIPs, status.PodIPs)
			}
			if tc.criPodIPs == nil && status.HostIP != status.PodIPs[0].IP {
				t.Fatalf("Expected HostIP %q to equal PodIPs[0].IP %q", status.HostIP, status.PodIPs[0].IP)
			}
		})
	}
}

func TestNodeAddressUpdatesGenerateAPIPodStatusHostNetworkPodIPs(t *testing.T) {
	testcases := []struct {
		name           string
		nodeIPs        []string
		nodeAddresses  []v1.NodeAddress
		expectedPodIPs []v1.PodIP
	}{

		{
			name:    "Immutable after update node addresses single-stack",
			nodeIPs: []string{"10.0.0.1"},
			nodeAddresses: []v1.NodeAddress{
				{Type: v1.NodeInternalIP, Address: "1.1.1.1"},
			},
			expectedPodIPs: []v1.PodIP{
				{IP: "10.0.0.1"},
			},
		},
		{
			name:    "Immutable after update node addresses dual-stack - primary address",
			nodeIPs: []string{"10.0.0.1", "2001:db8::2"},
			nodeAddresses: []v1.NodeAddress{
				{Type: v1.NodeInternalIP, Address: "1.1.1.1"},
				{Type: v1.NodeInternalIP, Address: "2001:db8::2"},
			},
			expectedPodIPs: []v1.PodIP{
				{IP: "10.0.0.1"},
				{IP: "2001:db8::2"},
			},
		},
		{
			name:    "Immutable after update node addresses dual-stack - secondary address",
			nodeIPs: []string{"10.0.0.1", "2001:db8::2"},
			nodeAddresses: []v1.NodeAddress{
				{Type: v1.NodeInternalIP, Address: "10.0.0.1"},
				{Type: v1.NodeInternalIP, Address: "2001:db8:1:2:3::2"},
			},
			expectedPodIPs: []v1.PodIP{
				{IP: "10.0.0.1"},
				{IP: "2001:db8::2"},
			},
		},
		{
			name:    "Immutable after update node addresses dual-stack - primary and secondary address",
			nodeIPs: []string{"10.0.0.1", "2001:db8::2"},
			nodeAddresses: []v1.NodeAddress{
				{Type: v1.NodeInternalIP, Address: "1.1.1.1"},
				{Type: v1.NodeInternalIP, Address: "2001:db8:1:2:3::2"},
			},
			expectedPodIPs: []v1.PodIP{
				{IP: "10.0.0.1"},
				{IP: "2001:db8::2"},
			},
		},
		{
			name:    "Update secondary after new secondary address dual-stack",
			nodeIPs: []string{"10.0.0.1"},
			nodeAddresses: []v1.NodeAddress{
				{Type: v1.NodeInternalIP, Address: "10.0.0.1"},
				{Type: v1.NodeInternalIP, Address: "2001:db8::2"},
			},
			expectedPodIPs: []v1.PodIP{
				{IP: "10.0.0.1"},
				{IP: "2001:db8::2"},
			},
		},
		{
			name:    "Update secondary after new secondary address dual-stack - reverse order",
			nodeIPs: []string{"2001:db8::2"},
			nodeAddresses: []v1.NodeAddress{
				{Type: v1.NodeInternalIP, Address: "10.0.0.1"},
				{Type: v1.NodeInternalIP, Address: "2001:db8::2"},
			},
			expectedPodIPs: []v1.PodIP{
				{IP: "2001:db8::2"},
			},
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			testKubelet := newTestKubelet(t, false /* controllerAttachDetachEnabled */)
			defer testKubelet.Cleanup()
			kl := testKubelet.kubelet
			for _, ip := range tc.nodeIPs {
				kl.nodeIPs = append(kl.nodeIPs, netutils.ParseIPSloppy(ip))
			}
			kl.nodeLister = testNodeLister{nodes: []*v1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{Name: string(kl.nodeName)},
					Status: v1.NodeStatus{
						Addresses: tc.nodeAddresses,
					},
				},
			}}

			pod := podWithUIDNameNs("12345", "test-pod", "test-namespace")
			pod.Spec.HostNetwork = true
			for _, ip := range tc.nodeIPs {
				pod.Status.PodIPs = append(pod.Status.PodIPs, v1.PodIP{IP: ip})
			}
			if len(pod.Status.PodIPs) > 0 {
				pod.Status.PodIP = pod.Status.PodIPs[0].IP
			}

			// set old status
			podStatus := &kubecontainer.PodStatus{
				ID:        pod.UID,
				Name:      pod.Name,
				Namespace: pod.Namespace,
			}
			podStatus.IPs = tc.nodeIPs

			status := kl.generateAPIPodStatus(pod, podStatus, false)
			if !reflect.DeepEqual(status.PodIPs, tc.expectedPodIPs) {
				t.Fatalf("Expected PodIPs %#v, got %#v", tc.expectedPodIPs, status.PodIPs)
			}
			if kl.nodeIPs[0].String() != status.PodIPs[0].IP {
				t.Fatalf("Expected HostIP %q to equal PodIPs[0].IP %q", status.HostIP, status.PodIPs[0].IP)
			}
		})
	}
}

func TestGenerateAPIPodStatusPodIPs(t *testing.T) {
	testcases := []struct {
		name      string
		nodeIP    string
		criPodIPs []string
		podIPs    []v1.PodIP
	}{
		{
			name:      "Simple",
			nodeIP:    "",
			criPodIPs: []string{"10.0.0.1"},
			podIPs: []v1.PodIP{
				{IP: "10.0.0.1"},
			},
		},
		{
			name:      "Dual-stack",
			nodeIP:    "",
			criPodIPs: []string{"10.0.0.1", "fd01::1234"},
			podIPs: []v1.PodIP{
				{IP: "10.0.0.1"},
				{IP: "fd01::1234"},
			},
		},
		{
			name:      "Dual-stack with explicit node IP",
			nodeIP:    "192.168.1.1",
			criPodIPs: []string{"10.0.0.1", "fd01::1234"},
			podIPs: []v1.PodIP{
				{IP: "10.0.0.1"},
				{IP: "fd01::1234"},
			},
		},
		{
			name:      "Dual-stack with CRI returning wrong family first",
			nodeIP:    "",
			criPodIPs: []string{"fd01::1234", "10.0.0.1"},
			podIPs: []v1.PodIP{
				{IP: "10.0.0.1"},
				{IP: "fd01::1234"},
			},
		},
		{
			name:      "Dual-stack with explicit node IP with CRI returning wrong family first",
			nodeIP:    "192.168.1.1",
			criPodIPs: []string{"fd01::1234", "10.0.0.1"},
			podIPs: []v1.PodIP{
				{IP: "10.0.0.1"},
				{IP: "fd01::1234"},
			},
		},
		{
			name:      "Dual-stack with IPv6 node IP",
			nodeIP:    "fd00::5678",
			criPodIPs: []string{"10.0.0.1", "fd01::1234"},
			podIPs: []v1.PodIP{
				{IP: "fd01::1234"},
				{IP: "10.0.0.1"},
			},
		},
		{
			name:      "Dual-stack with IPv6 node IP, other CRI order",
			nodeIP:    "fd00::5678",
			criPodIPs: []string{"fd01::1234", "10.0.0.1"},
			podIPs: []v1.PodIP{
				{IP: "fd01::1234"},
				{IP: "10.0.0.1"},
			},
		},
		{
			name:      "No Pod IP matching Node IP",
			nodeIP:    "fd00::5678",
			criPodIPs: []string{"10.0.0.1"},
			podIPs: []v1.PodIP{
				{IP: "10.0.0.1"},
			},
		},
		{
			name:      "No Pod IP matching (unspecified) Node IP",
			nodeIP:    "",
			criPodIPs: []string{"fd01::1234"},
			podIPs: []v1.PodIP{
				{IP: "fd01::1234"},
			},
		},
		{
			name:      "Multiple IPv4 IPs",
			nodeIP:    "",
			criPodIPs: []string{"10.0.0.1", "10.0.0.2", "10.0.0.3"},
			podIPs: []v1.PodIP{
				{IP: "10.0.0.1"},
			},
		},
		{
			name:      "Multiple Dual-Stack IPs",
			nodeIP:    "",
			criPodIPs: []string{"10.0.0.1", "10.0.0.2", "fd01::1234", "10.0.0.3", "fd01::5678"},
			podIPs: []v1.PodIP{
				{IP: "10.0.0.1"},
				{IP: "fd01::1234"},
			},
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			testKubelet := newTestKubelet(t, false /* controllerAttachDetachEnabled */)
			defer testKubelet.Cleanup()
			kl := testKubelet.kubelet
			if tc.nodeIP != "" {
				kl.nodeIPs = []net.IP{netutils.ParseIPSloppy(tc.nodeIP)}
			}

			pod := podWithUIDNameNs("12345", "test-pod", "test-namespace")

			criStatus := &kubecontainer.PodStatus{
				ID:        pod.UID,
				Name:      pod.Name,
				Namespace: pod.Namespace,
				IPs:       tc.criPodIPs,
			}

			status := kl.generateAPIPodStatus(pod, criStatus, false)
			if !reflect.DeepEqual(status.PodIPs, tc.podIPs) {
				t.Fatalf("Expected PodIPs %#v, got %#v", tc.podIPs, status.PodIPs)
			}
			if status.PodIP != status.PodIPs[0].IP {
				t.Fatalf("Expected PodIP %q to equal PodIPs[0].IP %q", status.PodIP, status.PodIPs[0].IP)
			}
		})
	}
}

func TestSortPodIPs(t *testing.T) {
	testcases := []struct {
		name        string
		nodeIP      string
		podIPs      []string
		expectedIPs []string
	}{
		{
			name:        "Simple",
			nodeIP:      "",
			podIPs:      []string{"10.0.0.1"},
			expectedIPs: []string{"10.0.0.1"},
		},
		{
			name:        "Dual-stack",
			nodeIP:      "",
			podIPs:      []string{"10.0.0.1", "fd01::1234"},
			expectedIPs: []string{"10.0.0.1", "fd01::1234"},
		},
		{
			name:        "Dual-stack with explicit node IP",
			nodeIP:      "192.168.1.1",
			podIPs:      []string{"10.0.0.1", "fd01::1234"},
			expectedIPs: []string{"10.0.0.1", "fd01::1234"},
		},
		{
			name:        "Dual-stack with CRI returning wrong family first",
			nodeIP:      "",
			podIPs:      []string{"fd01::1234", "10.0.0.1"},
			expectedIPs: []string{"10.0.0.1", "fd01::1234"},
		},
		{
			name:        "Dual-stack with explicit node IP with CRI returning wrong family first",
			nodeIP:      "192.168.1.1",
			podIPs:      []string{"fd01::1234", "10.0.0.1"},
			expectedIPs: []string{"10.0.0.1", "fd01::1234"},
		},
		{
			name:        "Dual-stack with IPv6 node IP",
			nodeIP:      "fd00::5678",
			podIPs:      []string{"10.0.0.1", "fd01::1234"},
			expectedIPs: []string{"fd01::1234", "10.0.0.1"},
		},
		{
			name:        "Dual-stack with IPv6 node IP, other CRI order",
			nodeIP:      "fd00::5678",
			podIPs:      []string{"fd01::1234", "10.0.0.1"},
			expectedIPs: []string{"fd01::1234", "10.0.0.1"},
		},
		{
			name:        "No Pod IP matching Node IP",
			nodeIP:      "fd00::5678",
			podIPs:      []string{"10.0.0.1"},
			expectedIPs: []string{"10.0.0.1"},
		},
		{
			name:        "No Pod IP matching (unspecified) Node IP",
			nodeIP:      "",
			podIPs:      []string{"fd01::1234"},
			expectedIPs: []string{"fd01::1234"},
		},
		{
			name:        "Multiple IPv4 IPs",
			nodeIP:      "",
			podIPs:      []string{"10.0.0.1", "10.0.0.2", "10.0.0.3"},
			expectedIPs: []string{"10.0.0.1"},
		},
		{
			name:        "Multiple Dual-Stack IPs",
			nodeIP:      "",
			podIPs:      []string{"10.0.0.1", "10.0.0.2", "fd01::1234", "10.0.0.3", "fd01::5678"},
			expectedIPs: []string{"10.0.0.1", "fd01::1234"},
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			testKubelet := newTestKubelet(t, false /* controllerAttachDetachEnabled */)
			defer testKubelet.Cleanup()
			kl := testKubelet.kubelet
			if tc.nodeIP != "" {
				kl.nodeIPs = []net.IP{netutils.ParseIPSloppy(tc.nodeIP)}
			}

			podIPs := kl.sortPodIPs(tc.podIPs)
			if !reflect.DeepEqual(podIPs, tc.expectedIPs) {
				t.Fatalf("Expected PodIPs %#v, got %#v", tc.expectedIPs, podIPs)
			}
		})
	}
}

// func init() {
// 	klog.InitFlags(flag.CommandLine)
// 	flag.CommandLine.Lookup("v").Value.Set("5")
// }

func TestConvertToAPIContainerStatusesDataRace(t *testing.T) {
	pod := podWithUIDNameNs("12345", "test-pod", "test-namespace")

	testTimestamp := time.Unix(123456789, 987654321)

	criStatus := &kubecontainer.PodStatus{
		ID:        pod.UID,
		Name:      pod.Name,
		Namespace: pod.Namespace,
		ContainerStatuses: []*kubecontainer.Status{
			{Name: "containerA", CreatedAt: testTimestamp},
			{Name: "containerB", CreatedAt: testTimestamp.Add(1)},
		},
	}

	testKubelet := newTestKubelet(t, false)
	defer testKubelet.Cleanup()
	kl := testKubelet.kubelet

	// convertToAPIContainerStatuses is purely transformative and shouldn't alter the state of the kubelet
	// as there are no synchronisation events in that function (no locks, no channels, ...) each test routine
	// should have its own vector clock increased independently. Golang race detector uses pure happens-before
	// detection, so would catch a race condition consistently, despite only spawning 2 goroutines
	for i := 0; i < 2; i++ {
		go func() {
			kl.convertToAPIContainerStatuses(pod, criStatus, []v1.ContainerStatus{}, []v1.Container{}, false, false)
		}()
	}
}

func TestConvertToAPIContainerStatusesForResources(t *testing.T) {
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.InPlacePodVerticalScaling, true)

	nowTime := time.Now()
	testContainerName := "ctr0"
	testContainerID := kubecontainer.ContainerID{Type: "test", ID: testContainerName}
	testContainer := v1.Container{
		Name:  testContainerName,
		Image: "img",
	}
	testContainerStatus := v1.ContainerStatus{
		Name: testContainerName,
	}
	testPod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			UID:       "123456",
			Name:      "foo",
			Namespace: "bar",
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{testContainer},
		},
		Status: v1.PodStatus{
			ContainerStatuses: []v1.ContainerStatus{testContainerStatus},
		},
	}

	testPodStatus := func(state kubecontainer.State, resources *kubecontainer.ContainerResources) *kubecontainer.PodStatus {
		cStatus := kubecontainer.Status{
			Name:      testContainerName,
			ID:        testContainerID,
			Image:     "img",
			ImageID:   "1234",
			ImageRef:  "img1234",
			State:     state,
			Resources: resources,
		}
		switch state {
		case kubecontainer.ContainerStateRunning:
			cStatus.StartedAt = nowTime
		case kubecontainer.ContainerStateExited:
			cStatus.StartedAt = nowTime
			cStatus.FinishedAt = nowTime
		}
		return &kubecontainer.PodStatus{
			ID:                testPod.UID,
			Name:              testPod.Name,
			Namespace:         testPod.Namespace,
			ContainerStatuses: []*kubecontainer.Status{&cStatus},
		}
	}

	CPU1AndMem1G := v1.ResourceList{v1.ResourceCPU: resource.MustParse("1"), v1.ResourceMemory: resource.MustParse("1Gi")}
	CPU2AndMem2G := v1.ResourceList{v1.ResourceCPU: resource.MustParse("2"), v1.ResourceMemory: resource.MustParse("2Gi")}
	CPU1AndMem1GAndStorage2G := CPU1AndMem1G.DeepCopy()
	CPU1AndMem1GAndStorage2G[v1.ResourceEphemeralStorage] = resource.MustParse("2Gi")
	CPU1AndMem1GAndStorage2G[v1.ResourceStorage] = resource.MustParse("2Gi")
	CPU1AndMem2GAndStorage2G := CPU1AndMem1GAndStorage2G.DeepCopy()
	CPU1AndMem2GAndStorage2G[v1.ResourceMemory] = resource.MustParse("2Gi")
	CPU2AndMem2GAndStorage2G := CPU2AndMem2G.DeepCopy()
	CPU2AndMem2GAndStorage2G[v1.ResourceEphemeralStorage] = resource.MustParse("2Gi")
	CPU2AndMem2GAndStorage2G[v1.ResourceStorage] = resource.MustParse("2Gi")

	addExtendedResource := func(list v1.ResourceList) v1.ResourceList {
		const stubCustomResource = v1.ResourceName("dummy.io/dummy")

		withExtendedResource := list.DeepCopy()
		for _, resourceName := range []v1.ResourceName{v1.ResourceMemory, v1.ResourceCPU} {
			if _, exists := withExtendedResource[resourceName]; !exists {
				withExtendedResource[resourceName] = resource.MustParse("0")
			}
		}

		withExtendedResource[stubCustomResource] = resource.MustParse("1")
		return withExtendedResource
	}

	testKubelet := newTestKubelet(t, false)
	defer testKubelet.Cleanup()
	kubelet := testKubelet.kubelet
	kubelet.statusManager = status.NewFakeManager()

	idx := 0
	for tdesc, tc := range map[string]struct {
		State              kubecontainer.State // Defaults to Running
		Resources          v1.ResourceRequirements
		AllocatedResources *v1.ResourceRequirements          // Defaults to Resources
		ActualResources    *kubecontainer.ContainerResources // Defaults to Resources equivalent
		OldStatus          v1.ContainerStatus
		Expected           v1.ContainerStatus
	}{
		"GuaranteedQoSPod with CPU and memory CRI status": {
			Resources: v1.ResourceRequirements{Limits: CPU1AndMem1G, Requests: CPU1AndMem1G},
			OldStatus: v1.ContainerStatus{
				Name:      testContainerName,
				Image:     "img",
				ImageID:   "img1234",
				State:     v1.ContainerState{Running: &v1.ContainerStateRunning{}},
				Resources: &v1.ResourceRequirements{Limits: CPU1AndMem1G, Requests: CPU1AndMem1G},
			},
			Expected: v1.ContainerStatus{
				Name:               testContainerName,
				ContainerID:        testContainerID.String(),
				Image:              "img",
				ImageID:            "img1234",
				State:              v1.ContainerState{Running: &v1.ContainerStateRunning{StartedAt: metav1.NewTime(nowTime)}},
				AllocatedResources: CPU1AndMem1G,
				Resources:          &v1.ResourceRequirements{Limits: CPU1AndMem1G, Requests: CPU1AndMem1G},
			},
		},
		"BurstableQoSPod with CPU and memory CRI status": {
			Resources: v1.ResourceRequirements{Limits: CPU1AndMem1G, Requests: CPU1AndMem1G},
			OldStatus: v1.ContainerStatus{
				Name:      testContainerName,
				Image:     "img",
				ImageID:   "img1234",
				State:     v1.ContainerState{Running: &v1.ContainerStateRunning{}},
				Resources: &v1.ResourceRequirements{Limits: CPU2AndMem2G, Requests: CPU1AndMem1G},
			},
			Expected: v1.ContainerStatus{
				Name:               testContainerName,
				ContainerID:        testContainerID.String(),
				Image:              "img",
				ImageID:            "img1234",
				State:              v1.ContainerState{Running: &v1.ContainerStateRunning{StartedAt: metav1.NewTime(nowTime)}},
				AllocatedResources: CPU1AndMem1G,
				Resources:          &v1.ResourceRequirements{Limits: CPU1AndMem1G, Requests: CPU1AndMem1G},
			},
		},
		"BurstableQoSPod without CPU": {
			Resources: v1.ResourceRequirements{Requests: v1.ResourceList{
				v1.ResourceMemory: resource.MustParse("100M"),
			}},
			ActualResources: &kubecontainer.ContainerResources{
				CPURequest: resource.NewMilliQuantity(2, resource.DecimalSI),
			},
			OldStatus: v1.ContainerStatus{
				Name:    testContainerName,
				Image:   "img",
				ImageID: "img1234",
				State:   v1.ContainerState{Running: &v1.ContainerStateRunning{}},
				Resources: &v1.ResourceRequirements{Requests: v1.ResourceList{
					v1.ResourceMemory: resource.MustParse("100M"),
				}},
			},
			Expected: v1.ContainerStatus{
				Name:        testContainerName,
				ContainerID: testContainerID.String(),
				Image:       "img",
				ImageID:     "img1234",
				State:       v1.ContainerState{Running: &v1.ContainerStateRunning{StartedAt: metav1.NewTime(nowTime)}},
				AllocatedResources: v1.ResourceList{
					v1.ResourceMemory: resource.MustParse("100M"),
				},
				Resources: &v1.ResourceRequirements{Requests: v1.ResourceList{
					v1.ResourceMemory: resource.MustParse("100M"),
				}},
			},
		},
		"BurstableQoSPod with below min CPU": {
			Resources: v1.ResourceRequirements{
				Requests: v1.ResourceList{
					v1.ResourceMemory: resource.MustParse("100M"),
					v1.ResourceCPU:    resource.MustParse("1m"),
				},
				Limits: v1.ResourceList{
					v1.ResourceCPU: resource.MustParse("5m"),
				},
			},
			ActualResources: &kubecontainer.ContainerResources{
				CPURequest: resource.NewMilliQuantity(2, resource.DecimalSI),
				CPULimit:   resource.NewMilliQuantity(10, resource.DecimalSI),
			},
			OldStatus: v1.ContainerStatus{
				Name:    testContainerName,
				Image:   "img",
				ImageID: "img1234",
				State:   v1.ContainerState{Running: &v1.ContainerStateRunning{}},
				Resources: &v1.ResourceRequirements{
					Requests: v1.ResourceList{
						v1.ResourceMemory: resource.MustParse("100M"),
						v1.ResourceCPU:    resource.MustParse("1m"),
					},
					Limits: v1.ResourceList{
						v1.ResourceCPU: resource.MustParse("5m"),
					},
				},
			},
			Expected: v1.ContainerStatus{
				Name:        testContainerName,
				ContainerID: testContainerID.String(),
				Image:       "img",
				ImageID:     "img1234",
				State:       v1.ContainerState{Running: &v1.ContainerStateRunning{StartedAt: metav1.NewTime(nowTime)}},
				AllocatedResources: v1.ResourceList{
					v1.ResourceMemory: resource.MustParse("100M"),
					v1.ResourceCPU:    resource.MustParse("1m"),
				},
				Resources: &v1.ResourceRequirements{
					Requests: v1.ResourceList{
						v1.ResourceMemory: resource.MustParse("100M"),
						v1.ResourceCPU:    resource.MustParse("1m"),
					},
					Limits: v1.ResourceList{
						v1.ResourceCPU: resource.MustParse("5m"),
					},
				},
			},
		},
		"GuaranteedQoSPod with CPU and memory CRI status, with ephemeral storage": {
			Resources: v1.ResourceRequirements{Limits: CPU1AndMem1GAndStorage2G, Requests: CPU1AndMem1GAndStorage2G},
			OldStatus: v1.ContainerStatus{
				Name:      testContainerName,
				Image:     "img",
				ImageID:   "img1234",
				State:     v1.ContainerState{Running: &v1.ContainerStateRunning{}},
				Resources: &v1.ResourceRequirements{Limits: CPU1AndMem1G, Requests: CPU1AndMem1G},
			},
			Expected: v1.ContainerStatus{
				Name:               testContainerName,
				ContainerID:        testContainerID.String(),
				Image:              "img",
				ImageID:            "img1234",
				State:              v1.ContainerState{Running: &v1.ContainerStateRunning{StartedAt: metav1.NewTime(nowTime)}},
				AllocatedResources: CPU1AndMem1GAndStorage2G,
				Resources:          &v1.ResourceRequirements{Limits: CPU1AndMem1GAndStorage2G, Requests: CPU1AndMem1GAndStorage2G},
			},
		},
		"BurstableQoSPod with CPU and memory CRI status, with ephemeral storage": {
			Resources: v1.ResourceRequirements{Limits: CPU1AndMem1GAndStorage2G, Requests: CPU1AndMem1GAndStorage2G},
			OldStatus: v1.ContainerStatus{
				Name:      testContainerName,
				Image:     "img",
				ImageID:   "img1234",
				State:     v1.ContainerState{Running: &v1.ContainerStateRunning{}},
				Resources: &v1.ResourceRequirements{Limits: CPU2AndMem2GAndStorage2G, Requests: CPU2AndMem2GAndStorage2G},
			},
			Expected: v1.ContainerStatus{
				Name:               testContainerName,
				ContainerID:        testContainerID.String(),
				Image:              "img",
				ImageID:            "img1234",
				State:              v1.ContainerState{Running: &v1.ContainerStateRunning{StartedAt: metav1.NewTime(nowTime)}},
				AllocatedResources: CPU1AndMem1GAndStorage2G,
				Resources:          &v1.ResourceRequirements{Limits: CPU1AndMem1GAndStorage2G, Requests: CPU1AndMem1GAndStorage2G},
			},
		},
		"BurstableQoSPod with CPU and memory CRI status, with ephemeral storage, nil resources in OldStatus": {
			Resources: v1.ResourceRequirements{Limits: CPU1AndMem1GAndStorage2G, Requests: CPU1AndMem1GAndStorage2G},
			OldStatus: v1.ContainerStatus{
				Name:    testContainerName,
				Image:   "img",
				ImageID: "img1234",
				State:   v1.ContainerState{Running: &v1.ContainerStateRunning{}},
			},
			Expected: v1.ContainerStatus{
				Name:               testContainerName,
				ContainerID:        testContainerID.String(),
				Image:              "img",
				ImageID:            "img1234",
				State:              v1.ContainerState{Running: &v1.ContainerStateRunning{StartedAt: metav1.NewTime(nowTime)}},
				AllocatedResources: CPU1AndMem1GAndStorage2G,
				Resources:          &v1.ResourceRequirements{Limits: CPU1AndMem1GAndStorage2G, Requests: CPU1AndMem1GAndStorage2G},
			},
		},
		"BestEffortQoSPod": {
			OldStatus: v1.ContainerStatus{
				Name:      testContainerName,
				Image:     "img",
				ImageID:   "img1234",
				State:     v1.ContainerState{Running: &v1.ContainerStateRunning{}},
				Resources: &v1.ResourceRequirements{},
			},
			Expected: v1.ContainerStatus{
				Name:        testContainerName,
				ContainerID: testContainerID.String(),
				Image:       "img",
				ImageID:     "img1234",
				State:       v1.ContainerState{Running: &v1.ContainerStateRunning{StartedAt: metav1.NewTime(nowTime)}},
				Resources:   &v1.ResourceRequirements{},
			},
		},
		"BestEffort QoSPod with extended resources": {
			Resources: v1.ResourceRequirements{Requests: addExtendedResource(v1.ResourceList{})},
			OldStatus: v1.ContainerStatus{
				Name:      testContainerName,
				Image:     "img",
				ImageID:   "img1234",
				State:     v1.ContainerState{Running: &v1.ContainerStateRunning{}},
				Resources: &v1.ResourceRequirements{},
			},
			Expected: v1.ContainerStatus{
				Name:               testContainerName,
				ContainerID:        testContainerID.String(),
				Image:              "img",
				ImageID:            "img1234",
				State:              v1.ContainerState{Running: &v1.ContainerStateRunning{StartedAt: metav1.NewTime(nowTime)}},
				AllocatedResources: addExtendedResource(v1.ResourceList{}),
				Resources:          &v1.ResourceRequirements{Requests: addExtendedResource(v1.ResourceList{})},
			},
		},
		"BurstableQoSPod with extended resources": {
			Resources: v1.ResourceRequirements{Requests: addExtendedResource(CPU1AndMem1G)},
			OldStatus: v1.ContainerStatus{
				Name:      testContainerName,
				Image:     "img",
				ImageID:   "img1234",
				State:     v1.ContainerState{Running: &v1.ContainerStateRunning{}},
				Resources: &v1.ResourceRequirements{},
			},
			Expected: v1.ContainerStatus{
				Name:               testContainerName,
				ContainerID:        testContainerID.String(),
				Image:              "img",
				ImageID:            "img1234",
				State:              v1.ContainerState{Running: &v1.ContainerStateRunning{StartedAt: metav1.NewTime(nowTime)}},
				AllocatedResources: addExtendedResource(CPU1AndMem1G),
				Resources:          &v1.ResourceRequirements{Requests: addExtendedResource(CPU1AndMem1G)},
			},
		},
		"BurstableQoSPod with storage, ephemeral storage and extended resources": {
			Resources: v1.ResourceRequirements{Requests: addExtendedResource(CPU1AndMem1GAndStorage2G)},
			OldStatus: v1.ContainerStatus{
				Name:      testContainerName,
				Image:     "img",
				ImageID:   "img1234",
				State:     v1.ContainerState{Running: &v1.ContainerStateRunning{}},
				Resources: &v1.ResourceRequirements{},
			},
			Expected: v1.ContainerStatus{
				Name:               testContainerName,
				ContainerID:        testContainerID.String(),
				Image:              "img",
				ImageID:            "img1234",
				State:              v1.ContainerState{Running: &v1.ContainerStateRunning{StartedAt: metav1.NewTime(nowTime)}},
				AllocatedResources: addExtendedResource(CPU1AndMem1GAndStorage2G),
				Resources:          &v1.ResourceRequirements{Requests: addExtendedResource(CPU1AndMem1GAndStorage2G)},
			},
		},
		"GuaranteedQoSPod with extended resources": {
			Resources: v1.ResourceRequirements{Requests: addExtendedResource(CPU1AndMem1G), Limits: addExtendedResource(CPU1AndMem1G)},
			OldStatus: v1.ContainerStatus{
				Name:      testContainerName,
				Image:     "img",
				ImageID:   "img1234",
				State:     v1.ContainerState{Running: &v1.ContainerStateRunning{}},
				Resources: &v1.ResourceRequirements{},
			},
			Expected: v1.ContainerStatus{
				Name:               testContainerName,
				ContainerID:        testContainerID.String(),
				Image:              "img",
				ImageID:            "img1234",
				State:              v1.ContainerState{Running: &v1.ContainerStateRunning{StartedAt: metav1.NewTime(nowTime)}},
				AllocatedResources: addExtendedResource(CPU1AndMem1G),
				Resources:          &v1.ResourceRequirements{Requests: addExtendedResource(CPU1AndMem1G), Limits: addExtendedResource(CPU1AndMem1G)},
			},
		},
		"newly created Pod": {
			State:     kubecontainer.ContainerStateCreated,
			Resources: v1.ResourceRequirements{Limits: CPU1AndMem1GAndStorage2G, Requests: CPU1AndMem1GAndStorage2G},
			OldStatus: v1.ContainerStatus{},
			Expected: v1.ContainerStatus{
				Name:               testContainerName,
				ContainerID:        testContainerID.String(),
				Image:              "img",
				ImageID:            "img1234",
				State:              v1.ContainerState{Waiting: &v1.ContainerStateWaiting{}},
				AllocatedResources: CPU1AndMem1GAndStorage2G,
				Resources:          &v1.ResourceRequirements{Limits: CPU1AndMem1GAndStorage2G, Requests: CPU1AndMem1GAndStorage2G},
			},
		},
		"newly running Pod": {
			Resources: v1.ResourceRequirements{Limits: CPU1AndMem1GAndStorage2G, Requests: CPU1AndMem1GAndStorage2G},
			OldStatus: v1.ContainerStatus{
				Name:    testContainerName,
				Image:   "img",
				ImageID: "img1234",
				State:   v1.ContainerState{Waiting: &v1.ContainerStateWaiting{}},
			},
			Expected: v1.ContainerStatus{
				Name:               testContainerName,
				ContainerID:        testContainerID.String(),
				Image:              "img",
				ImageID:            "img1234",
				State:              v1.ContainerState{Running: &v1.ContainerStateRunning{StartedAt: metav1.NewTime(nowTime)}},
				AllocatedResources: CPU1AndMem1GAndStorage2G,
				Resources:          &v1.ResourceRequirements{Limits: CPU1AndMem1GAndStorage2G, Requests: CPU1AndMem1GAndStorage2G},
			},
		},
		"newly terminated Pod": {
			State: kubecontainer.ContainerStateExited,
			// Actual resources were different, but they should be ignored once the container is terminated.
			Resources:          v1.ResourceRequirements{Limits: CPU1AndMem1GAndStorage2G, Requests: CPU1AndMem1GAndStorage2G},
			AllocatedResources: &v1.ResourceRequirements{Limits: CPU2AndMem2GAndStorage2G, Requests: CPU2AndMem2GAndStorage2G},
			OldStatus: v1.ContainerStatus{
				Name:      testContainerName,
				Image:     "img",
				ImageID:   "img1234",
				State:     v1.ContainerState{Running: &v1.ContainerStateRunning{}},
				Resources: &v1.ResourceRequirements{Limits: CPU1AndMem1GAndStorage2G, Requests: CPU1AndMem1GAndStorage2G},
			},
			Expected: v1.ContainerStatus{
				Name:        testContainerName,
				ContainerID: testContainerID.String(),
				Image:       "img",
				ImageID:     "img1234",
				State: v1.ContainerState{Terminated: &v1.ContainerStateTerminated{
					ContainerID: testContainerID.String(),
					StartedAt:   metav1.NewTime(nowTime),
					FinishedAt:  metav1.NewTime(nowTime),
				}},
				AllocatedResources: CPU2AndMem2GAndStorage2G,
				Resources:          &v1.ResourceRequirements{Limits: CPU2AndMem2GAndStorage2G, Requests: CPU2AndMem2GAndStorage2G},
			},
		},
		"resizing Pod": {
			Resources:          v1.ResourceRequirements{Limits: CPU1AndMem1GAndStorage2G, Requests: CPU1AndMem1GAndStorage2G},
			AllocatedResources: &v1.ResourceRequirements{Limits: CPU2AndMem2GAndStorage2G, Requests: CPU2AndMem2GAndStorage2G},
			OldStatus: v1.ContainerStatus{
				Name:      testContainerName,
				Image:     "img",
				ImageID:   "img1234",
				State:     v1.ContainerState{Running: &v1.ContainerStateRunning{}},
				Resources: &v1.ResourceRequirements{Limits: CPU1AndMem1GAndStorage2G, Requests: CPU1AndMem1GAndStorage2G},
			},
			Expected: v1.ContainerStatus{
				Name:               testContainerName,
				ContainerID:        testContainerID.String(),
				Image:              "img",
				ImageID:            "img1234",
				State:              v1.ContainerState{Running: &v1.ContainerStateRunning{StartedAt: metav1.NewTime(nowTime)}},
				AllocatedResources: CPU2AndMem2GAndStorage2G,
				Resources:          &v1.ResourceRequirements{Limits: CPU1AndMem1GAndStorage2G, Requests: CPU1AndMem2GAndStorage2G},
			},
		},
	} {
		t.Run(tdesc, func(t *testing.T) {
			tPod := testPod.DeepCopy()
			tPod.Name = fmt.Sprintf("%s-%d", testPod.Name, idx)

			if tc.AllocatedResources != nil {
				tPod.Spec.Containers[0].Resources = *tc.AllocatedResources
			} else {
				tPod.Spec.Containers[0].Resources = tc.Resources
			}
			kubelet.statusManager.SetPodAllocation(tPod)
			resources := tc.ActualResources
			if resources == nil {
				resources = &kubecontainer.ContainerResources{
					MemoryLimit: tc.Resources.Limits.Memory(),
					CPULimit:    tc.Resources.Limits.Cpu(),
					CPURequest:  tc.Resources.Requests.Cpu(),
				}
			}
			state := kubecontainer.ContainerStateRunning
			if tc.State != "" {
				state = tc.State
			}
			podStatus := testPodStatus(state, resources)

			for _, enableAllocatedStatus := range []bool{true, false} {
				t.Run(fmt.Sprintf("AllocatedStatus=%t", enableAllocatedStatus), func(t *testing.T) {
					featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.InPlacePodVerticalScalingAllocatedStatus, enableAllocatedStatus)

					expected := tc.Expected
					if !enableAllocatedStatus {
						expected = *expected.DeepCopy()
						expected.AllocatedResources = nil
					}

					cStatuses := kubelet.convertToAPIContainerStatuses(tPod, podStatus, []v1.ContainerStatus{tc.OldStatus}, tPod.Spec.Containers, false, false)
					assert.Equal(t, expected, cStatuses[0])
				})
			}
		})
	}
}

func TestConvertToAPIContainerStatusesForUser(t *testing.T) {
	nowTime := time.Now()
	testContainerName := "ctr0"
	testContainerID := kubecontainer.ContainerID{Type: "test", ID: testContainerName}
	testContainer := v1.Container{
		Name:  testContainerName,
		Image: "img",
	}
	testContainerStatus := v1.ContainerStatus{
		Name: testContainerName,
	}
	testPod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			UID:       "123456",
			Name:      "foo",
			Namespace: "bar",
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{testContainer},
		},
		Status: v1.PodStatus{
			ContainerStatuses: []v1.ContainerStatus{testContainerStatus},
		},
	}
	testPodStaus := func(user *kubecontainer.ContainerUser) *kubecontainer.PodStatus {
		testKubeContainerStatus := kubecontainer.Status{
			Name:      testContainerName,
			ID:        testContainerID,
			Image:     "img",
			State:     kubecontainer.ContainerStateRunning,
			StartedAt: nowTime,
			User:      user,
		}
		return &kubecontainer.PodStatus{
			ID:                testPod.UID,
			Name:              testPod.Name,
			Namespace:         testPod.Namespace,
			ContainerStatuses: []*kubecontainer.Status{&testKubeContainerStatus},
		}
	}
	expectedContainerStatuses := func(user *v1.ContainerUser) []v1.ContainerStatus {
		return []v1.ContainerStatus{
			{
				Name:        testContainerName,
				ContainerID: testContainerID.String(),
				Image:       "img",
				State:       v1.ContainerState{Running: &v1.ContainerStateRunning{StartedAt: metav1.NewTime(nowTime)}},
				User:        user,
			},
		}
	}
	testKubelet := newTestKubelet(t, false)
	defer testKubelet.Cleanup()
	kubelet := testKubelet.kubelet
	kubelet.statusManager = status.NewFakeManager()

	for tdesc, tc := range map[string]struct {
		testPodStatus           *kubecontainer.PodStatus
		featureEnabled          bool
		expectedContainerStatus []v1.ContainerStatus
	}{
		"nil user, SupplementalGroupsPolicy is disabled": {
			testPodStaus(nil),
			false,
			expectedContainerStatuses(nil),
		},
		"empty user, SupplementalGroupsPolicy is disabled": {
			testPodStaus(&kubecontainer.ContainerUser{}),
			false,
			expectedContainerStatuses(nil),
		},
		"linux user, SupplementalGroupsPolicy is disabled": {
			testPodStaus(&kubecontainer.ContainerUser{
				Linux: &kubecontainer.LinuxContainerUser{
					UID:                0,
					GID:                0,
					SupplementalGroups: []int64{10},
				},
			}),
			false,
			expectedContainerStatuses(nil),
		},
		"nil user, SupplementalGroupsPolicy is enabled": {
			testPodStaus(nil),
			true,
			expectedContainerStatuses(nil),
		},
		"empty user, SupplementalGroupsPolicy is enabled": {
			testPodStaus(&kubecontainer.ContainerUser{}),
			true,
			expectedContainerStatuses(&v1.ContainerUser{}),
		},
		"linux user, SupplementalGroupsPolicy is enabled": {
			testPodStaus(&kubecontainer.ContainerUser{
				Linux: &kubecontainer.LinuxContainerUser{
					UID:                0,
					GID:                0,
					SupplementalGroups: []int64{10},
				},
			}),
			true,
			expectedContainerStatuses(&v1.ContainerUser{
				Linux: &v1.LinuxContainerUser{
					UID:                0,
					GID:                0,
					SupplementalGroups: []int64{10},
				},
			}),
		},
	} {
		featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.SupplementalGroupsPolicy, tc.featureEnabled)
		tPod := testPod.DeepCopy()
		t.Logf("TestCase: %q", tdesc)
		cStatuses := kubelet.convertToAPIContainerStatuses(tPod, tc.testPodStatus, tPod.Status.ContainerStatuses, tPod.Spec.Containers, false, false)
		assert.Equal(t, tc.expectedContainerStatus, cStatuses)
	}
}

func TestKubelet_HandlePodCleanups(t *testing.T) {
	one := int64(1)
	two := int64(2)
	deleted := metav1.NewTime(time.Unix(2, 0).UTC())
	type rejectedPod struct {
		uid     types.UID
		reason  string
		message string
	}
	simplePod := func() *v1.Pod {
		return &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "pod1", Namespace: "ns1", UID: types.UID("1")},
			Spec: v1.PodSpec{
				Containers: []v1.Container{
					{Name: "container-1"},
				},
			},
		}
	}
	withPhase := func(pod *v1.Pod, phase v1.PodPhase) *v1.Pod {
		pod.Status.Phase = phase
		return pod
	}
	staticPod := func() *v1.Pod {
		return &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "pod1",
				Namespace: "ns1",
				UID:       types.UID("1"),
				Annotations: map[string]string{
					kubetypes.ConfigSourceAnnotationKey: kubetypes.FileSource,
				},
			},
			Spec: v1.PodSpec{
				Containers: []v1.Container{
					{Name: "container-1"},
				},
			},
		}
	}
	runtimePod := func(pod *v1.Pod) *kubecontainer.Pod {
		runningPod := &kubecontainer.Pod{
			ID:        types.UID(pod.UID),
			Name:      pod.Name,
			Namespace: pod.Namespace,
			Containers: []*kubecontainer.Container{
				{Name: "container-1", ID: kubecontainer.ContainerID{Type: "test", ID: "c1"}},
			},
		}
		for i, container := range pod.Spec.Containers {
			runningPod.Containers = append(runningPod.Containers, &kubecontainer.Container{
				Name: container.Name,
				ID:   kubecontainer.ContainerID{Type: "test", ID: fmt.Sprintf("c%d", i)},
			})
		}
		return runningPod
	}
	mirrorPod := func(pod *v1.Pod, nodeName string, nodeUID types.UID) *v1.Pod {
		copied := pod.DeepCopy()
		if copied.Annotations == nil {
			copied.Annotations = make(map[string]string)
		}
		copied.Annotations[kubetypes.ConfigMirrorAnnotationKey] = pod.Annotations[kubetypes.ConfigHashAnnotationKey]
		isTrue := true
		copied.OwnerReferences = append(copied.OwnerReferences, metav1.OwnerReference{
			APIVersion: v1.SchemeGroupVersion.String(),
			Kind:       "Node",
			Name:       nodeName,
			UID:        nodeUID,
			Controller: &isTrue,
		})
		return copied
	}

	tests := []struct {
		name                    string
		pods                    []*v1.Pod
		runtimePods             []*containertest.FakePod
		rejectedPods            []rejectedPod
		terminatingErr          error
		prepareWorker           func(t *testing.T, w *podWorkers, records map[types.UID][]syncPodRecord)
		wantWorker              func(t *testing.T, w *podWorkers, records map[types.UID][]syncPodRecord)
		wantWorkerAfterRetry    func(t *testing.T, w *podWorkers, records map[types.UID][]syncPodRecord)
		wantErr                 bool
		expectMetrics           map[string]string
		expectMetricsAfterRetry map[string]string
	}{
		{
			name:    "missing pod is requested for termination with short grace period",
			wantErr: false,
			runtimePods: []*containertest.FakePod{
				{
					Pod: runtimePod(staticPod()),
				},
			},
			wantWorker: func(t *testing.T, w *podWorkers, records map[types.UID][]syncPodRecord) {
				drainAllWorkers(w)
				uid := types.UID("1")
				// we expect runtime pods to be cleared from the status history as soon as they
				// reach completion
				if len(w.podSyncStatuses) != 0 {
					t.Fatalf("unexpected sync statuses: %#v", w.podSyncStatuses)
				}
				r, ok := records[uid]
				if !ok || len(r) != 1 || r[0].updateType != kubetypes.SyncPodKill || r[0].terminated || r[0].runningPod == nil || r[0].gracePeriod != nil {
					t.Fatalf("unexpected pod sync records: %#v", r)
				}
			},
			expectMetrics: map[string]string{
				metrics.OrphanedRuntimePodTotal.FQName(): `# HELP kubelet_orphaned_runtime_pods_total [ALPHA] Number of pods that have been detected in the container runtime without being already known to the pod worker. This typically indicates the kubelet was restarted while a pod was force deleted in the API or in the local configuration, which is unusual.
				# TYPE kubelet_orphaned_runtime_pods_total counter
				kubelet_orphaned_runtime_pods_total 1
				`,
				metrics.WorkingPodCount.FQName(): `# HELP kubelet_working_pods [ALPHA] Number of pods the kubelet is actually running, broken down by lifecycle phase, whether the pod is desired, orphaned, or runtime only (also orphaned), and whether the pod is static. An orphaned pod has been removed from local configuration or force deleted in the API and consumes resources that are not otherwise visible.
				# TYPE kubelet_working_pods gauge
				kubelet_working_pods{config="desired",lifecycle="sync",static=""} 0
				kubelet_working_pods{config="desired",lifecycle="sync",static="true"} 0
				kubelet_working_pods{config="desired",lifecycle="terminated",static=""} 0
				kubelet_working_pods{config="desired",lifecycle="terminated",static="true"} 0
				kubelet_working_pods{config="desired",lifecycle="terminating",static=""} 0
				kubelet_working_pods{config="desired",lifecycle="terminating",static="true"} 0
				kubelet_working_pods{config="orphan",lifecycle="sync",static=""} 0
				kubelet_working_pods{config="orphan",lifecycle="sync",static="true"} 0
				kubelet_working_pods{config="orphan",lifecycle="terminated",static=""} 0
				kubelet_working_pods{config="orphan",lifecycle="terminated",static="true"} 0
				kubelet_working_pods{config="orphan",lifecycle="terminating",static=""} 0
				kubelet_working_pods{config="orphan",lifecycle="terminating",static="true"} 0
				kubelet_working_pods{config="runtime_only",lifecycle="sync",static="unknown"} 0
				kubelet_working_pods{config="runtime_only",lifecycle="terminated",static="unknown"} 0
				kubelet_working_pods{config="runtime_only",lifecycle="terminating",static="unknown"} 1
				`,
			},
		},
		{
			name:    "terminating pod that errored and is not in config is notified by the cleanup",
			wantErr: false,
			runtimePods: []*containertest.FakePod{
				{
					Pod: runtimePod(simplePod()),
				},
			},
			terminatingErr: errors.New("unable to terminate"),
			prepareWorker: func(t *testing.T, w *podWorkers, records map[types.UID][]syncPodRecord) {
				// send a create
				pod := simplePod()
				w.UpdatePod(UpdatePodOptions{
					UpdateType: kubetypes.SyncPodCreate,
					StartTime:  time.Unix(1, 0).UTC(),
					Pod:        pod,
				})
				drainAllWorkers(w)

				// send a delete update
				two := int64(2)
				deleted := metav1.NewTime(time.Unix(2, 0).UTC())
				updatedPod := &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:                       "pod1",
						Namespace:                  "ns1",
						UID:                        types.UID("1"),
						DeletionGracePeriodSeconds: &two,
						DeletionTimestamp:          &deleted,
					},
					Spec: v1.PodSpec{
						TerminationGracePeriodSeconds: &two,
						Containers: []v1.Container{
							{Name: "container-1"},
						},
					},
				}
				w.UpdatePod(UpdatePodOptions{
					UpdateType: kubetypes.SyncPodKill,
					StartTime:  time.Unix(3, 0).UTC(),
					Pod:        updatedPod,
				})
				drainAllWorkers(w)
				r, ok := records[updatedPod.UID]
				if !ok || len(r) != 2 || r[1].gracePeriod == nil || *r[1].gracePeriod != 2 {
					t.Fatalf("unexpected records: %#v", records)
				}
				// pod worker thinks pod1 exists, but the kubelet will not have it in the pod manager
			},
			wantWorker: func(t *testing.T, w *podWorkers, records map[types.UID][]syncPodRecord) {
				uid := types.UID("1")
				if len(w.podSyncStatuses) != 1 {
					t.Fatalf("unexpected sync statuses: %#v", w.podSyncStatuses)
				}
				s, ok := w.podSyncStatuses[uid]
				if !ok || !s.IsTerminationRequested() || !s.IsTerminationStarted() || s.IsFinished() || s.IsWorking() || !s.IsDeleted() {
					t.Fatalf("unexpected requested pod termination: %#v", s)
				}
				// expect we get a pod sync record for kill that should have the same grace period as before (2), but no
				// running pod because the SyncKnownPods method killed it
				if actual, expected := records[uid], []syncPodRecord{
					{name: "pod1", updateType: kubetypes.SyncPodCreate},
					{name: "pod1", updateType: kubetypes.SyncPodKill, gracePeriod: &two},
					{name: "pod1", updateType: kubetypes.SyncPodKill, gracePeriod: &two},
				}; !reflect.DeepEqual(expected, actual) {
					t.Fatalf("unexpected pod sync records: %s", cmp.Diff(expected, actual, cmp.AllowUnexported(syncPodRecord{})))
				}
			},
			expectMetrics: map[string]string{
				metrics.DesiredPodCount.FQName(): `# HELP kubelet_desired_pods [ALPHA] The number of pods the kubelet is being instructed to run. static is true if the pod is not from the apiserver.
				# TYPE kubelet_desired_pods gauge
				kubelet_desired_pods{static=""} 0
				kubelet_desired_pods{static="true"} 0
				`,
				metrics.ActivePodCount.FQName(): `# HELP kubelet_active_pods [ALPHA] The number of pods the kubelet considers active and which are being considered when admitting new pods. static is true if the pod is not from the apiserver.
				# TYPE kubelet_active_pods gauge
				kubelet_active_pods{static=""} 0
				kubelet_active_pods{static="true"} 0
				`,
				metrics.OrphanedRuntimePodTotal.FQName(): `# HELP kubelet_orphaned_runtime_pods_total [ALPHA] Number of pods that have been detected in the container runtime without being already known to the pod worker. This typically indicates the kubelet was restarted while a pod was force deleted in the API or in the local configuration, which is unusual.
				# TYPE kubelet_orphaned_runtime_pods_total counter
				kubelet_orphaned_runtime_pods_total 0
				`,
				metrics.WorkingPodCount.FQName(): `# HELP kubelet_working_pods [ALPHA] Number of pods the kubelet is actually running, broken down by lifecycle phase, whether the pod is desired, orphaned, or runtime only (also orphaned), and whether the pod is static. An orphaned pod has been removed from local configuration or force deleted in the API and consumes resources that are not otherwise visible.
				# TYPE kubelet_working_pods gauge
				kubelet_working_pods{config="desired",lifecycle="sync",static=""} 0
				kubelet_working_pods{config="desired",lifecycle="sync",static="true"} 0
				kubelet_working_pods{config="desired",lifecycle="terminated",static=""} 0
				kubelet_working_pods{config="desired",lifecycle="terminated",static="true"} 0
				kubelet_working_pods{config="desired",lifecycle="terminating",static=""} 0
				kubelet_working_pods{config="desired",lifecycle="terminating",static="true"} 0
				kubelet_working_pods{config="orphan",lifecycle="sync",static=""} 0
				kubelet_working_pods{config="orphan",lifecycle="sync",static="true"} 0
				kubelet_working_pods{config="orphan",lifecycle="terminated",static=""} 0
				kubelet_working_pods{config="orphan",lifecycle="terminated",static="true"} 0
				kubelet_working_pods{config="orphan",lifecycle="terminating",static=""} 1
				kubelet_working_pods{config="orphan",lifecycle="terminating",static="true"} 0
				kubelet_working_pods{config="runtime_only",lifecycle="sync",static="unknown"} 0
				kubelet_working_pods{config="runtime_only",lifecycle="terminated",static="unknown"} 0
				kubelet_working_pods{config="runtime_only",lifecycle="terminating",static="unknown"} 0
				`,
			},
			wantWorkerAfterRetry: func(t *testing.T, w *podWorkers, records map[types.UID][]syncPodRecord) {
				uid := types.UID("1")
				if len(w.podSyncStatuses) != 1 {
					t.Fatalf("unexpected sync statuses: %#v", w.podSyncStatuses)
				}
				s, ok := w.podSyncStatuses[uid]
				if !ok || !s.IsTerminationRequested() || !s.IsTerminationStarted() || !s.IsFinished() || s.IsWorking() || !s.IsDeleted() {
					t.Fatalf("unexpected requested pod termination: %#v", s)
				}
				// expect we get a pod sync record for kill that should have the same grace period as before (2), but no
				// running pod because the SyncKnownPods method killed it
				if actual, expected := records[uid], []syncPodRecord{
					{name: "pod1", updateType: kubetypes.SyncPodCreate},
					{name: "pod1", updateType: kubetypes.SyncPodKill, gracePeriod: &two},
					{name: "pod1", updateType: kubetypes.SyncPodKill, gracePeriod: &two},
					// after the second attempt
					{name: "pod1", updateType: kubetypes.SyncPodKill, gracePeriod: &two},
					// from termination
					{name: "pod1", terminated: true},
				}; !reflect.DeepEqual(expected, actual) {
					t.Fatalf("unexpected pod sync records: %s", cmp.Diff(expected, actual, cmp.AllowUnexported(syncPodRecord{})))
				}
			},
		},
		{
			name:    "terminating pod that errored and is not in config or worker is force killed by the cleanup",
			wantErr: false,
			runtimePods: []*containertest.FakePod{
				{
					Pod: runtimePod(simplePod()),
				},
			},
			terminatingErr: errors.New("unable to terminate"),
			wantWorker: func(t *testing.T, w *podWorkers, records map[types.UID][]syncPodRecord) {
				uid := types.UID("1")
				if len(w.podSyncStatuses) != 1 {
					t.Fatalf("unexpected sync statuses: %#v", w.podSyncStatuses)
				}
				s, ok := w.podSyncStatuses[uid]
				if !ok || !s.IsTerminationRequested() || !s.IsTerminationStarted() || s.IsFinished() || s.IsWorking() || !s.IsDeleted() {
					t.Fatalf("unexpected requested pod termination: %#v", s)
				}

				// ensure that we recorded the appropriate state for replays
				expectedRunningPod := runtimePod(simplePod())
				if actual, expected := s.activeUpdate, (&UpdatePodOptions{
					RunningPod:     expectedRunningPod,
					KillPodOptions: &KillPodOptions{PodTerminationGracePeriodSecondsOverride: &one},
				}); !reflect.DeepEqual(expected, actual) {
					t.Fatalf("unexpected pod activeUpdate: %s", cmp.Diff(expected, actual))
				}

				// expect that a pod the pod worker does not recognize is force killed with grace period 1
				if actual, expected := records[uid], []syncPodRecord{
					{name: "pod1", updateType: kubetypes.SyncPodKill, runningPod: expectedRunningPod},
				}; !reflect.DeepEqual(expected, actual) {
					t.Fatalf("unexpected pod sync records: %s", cmp.Diff(expected, actual, cmp.AllowUnexported(syncPodRecord{})))
				}
			},
			wantWorkerAfterRetry: func(t *testing.T, w *podWorkers, records map[types.UID][]syncPodRecord) {
				uid := types.UID("1")
				if len(w.podSyncStatuses) != 0 {
					t.Fatalf("unexpected sync statuses: %#v", w.podSyncStatuses)
				}

				// expect that a pod the pod worker does not recognize is force killed with grace period 1
				expectedRunningPod := runtimePod(simplePod())
				if actual, expected := records[uid], []syncPodRecord{
					// first attempt, did not succeed
					{name: "pod1", updateType: kubetypes.SyncPodKill, runningPod: expectedRunningPod},
					// second attempt, should succeed
					{name: "pod1", updateType: kubetypes.SyncPodKill, runningPod: expectedRunningPod},
					// because this is a runtime pod, we don't have enough info to invoke syncTerminatedPod and so
					// we exit after the retry succeeds
				}; !reflect.DeepEqual(expected, actual) {
					t.Fatalf("unexpected pod sync records: %s", cmp.Diff(expected, actual, cmp.AllowUnexported(syncPodRecord{})))
				}
			},
		},
		{
			name:    "pod is added to worker by sync method",
			wantErr: false,
			pods: []*v1.Pod{
				simplePod(),
			},
			wantWorker: func(t *testing.T, w *podWorkers, records map[types.UID][]syncPodRecord) {
				uid := types.UID("1")
				if len(w.podSyncStatuses) != 1 {
					t.Fatalf("unexpected sync statuses: %#v", w.podSyncStatuses)
				}
				s, ok := w.podSyncStatuses[uid]
				if !ok || s.IsTerminationRequested() || s.IsTerminationStarted() || s.IsFinished() || s.IsWorking() || s.IsDeleted() {
					t.Fatalf("unexpected requested pod termination: %#v", s)
				}

				// pod was synced once
				if actual, expected := records[uid], []syncPodRecord{
					{name: "pod1", updateType: kubetypes.SyncPodCreate},
				}; !reflect.DeepEqual(expected, actual) {
					t.Fatalf("unexpected pod sync records: %s", cmp.Diff(expected, actual, cmp.AllowUnexported(syncPodRecord{})))
				}
			},
			expectMetrics: map[string]string{
				metrics.DesiredPodCount.FQName(): `# HELP kubelet_desired_pods [ALPHA] The number of pods the kubelet is being instructed to run. static is true if the pod is not from the apiserver.
				# TYPE kubelet_desired_pods gauge
				kubelet_desired_pods{static=""} 1
				kubelet_desired_pods{static="true"} 0
				`,
				metrics.ActivePodCount.FQName(): `# HELP kubelet_active_pods [ALPHA] The number of pods the kubelet considers active and which are being considered when admitting new pods. static is true if the pod is not from the apiserver.
				# TYPE kubelet_active_pods gauge
				kubelet_active_pods{static=""} 1
				kubelet_active_pods{static="true"} 0
				`,
				metrics.OrphanedRuntimePodTotal.FQName(): `# HELP kubelet_orphaned_runtime_pods_total [ALPHA] Number of pods that have been detected in the container runtime without being already known to the pod worker. This typically indicates the kubelet was restarted while a pod was force deleted in the API or in the local configuration, which is unusual.
				# TYPE kubelet_orphaned_runtime_pods_total counter
				kubelet_orphaned_runtime_pods_total 0
				`,
				// Note that this test simulates a net-new pod being discovered during HandlePodCleanups that was not
				// delivered to the pod worker via HandlePodAdditions - there is no *known* scenario that can happen, but
				// we want to capture it in the metric. The more likely scenario is that a static pod with a predefined
				// UID is updated, which causes pod config to deliver DELETE -> ADD while the old pod is still shutting
				// down and the pod worker to ignore the ADD. The HandlePodCleanups method then is responsible for syncing
				// that pod to the pod worker so that it restarts.
				metrics.RestartedPodTotal.FQName(): `# HELP kubelet_restarted_pods_total [ALPHA] Number of pods that have been restarted because they were deleted and recreated with the same UID while the kubelet was watching them (common for static pods, extremely uncommon for API pods)
				# TYPE kubelet_restarted_pods_total counter
				kubelet_restarted_pods_total{static=""} 1
				kubelet_restarted_pods_total{static="true"} 0
				`,
				metrics.WorkingPodCount.FQName(): `# HELP kubelet_working_pods [ALPHA] Number of pods the kubelet is actually running, broken down by lifecycle phase, whether the pod is desired, orphaned, or runtime only (also orphaned), and whether the pod is static. An orphaned pod has been removed from local configuration or force deleted in the API and consumes resources that are not otherwise visible.
				# TYPE kubelet_working_pods gauge
				kubelet_working_pods{config="desired",lifecycle="sync",static=""} 1
				kubelet_working_pods{config="desired",lifecycle="sync",static="true"} 0
				kubelet_working_pods{config="desired",lifecycle="terminated",static=""} 0
				kubelet_working_pods{config="desired",lifecycle="terminated",static="true"} 0
				kubelet_working_pods{config="desired",lifecycle="terminating",static=""} 0
				kubelet_working_pods{config="desired",lifecycle="terminating",static="true"} 0
				kubelet_working_pods{config="orphan",lifecycle="sync",static=""} 0
				kubelet_working_pods{config="orphan",lifecycle="sync",static="true"} 0
				kubelet_working_pods{config="orphan",lifecycle="terminated",static=""} 0
				kubelet_working_pods{config="orphan",lifecycle="terminated",static="true"} 0
				kubelet_working_pods{config="orphan",lifecycle="terminating",static=""} 0
				kubelet_working_pods{config="orphan",lifecycle="terminating",static="true"} 0
				kubelet_working_pods{config="runtime_only",lifecycle="sync",static="unknown"} 0
				kubelet_working_pods{config="runtime_only",lifecycle="terminated",static="unknown"} 0
				kubelet_working_pods{config="runtime_only",lifecycle="terminating",static="unknown"} 0
				`,
			},
		},
		{
			name:    "pod is not added to worker by sync method because it is in a terminal phase",
			wantErr: false,
			pods: []*v1.Pod{
				withPhase(simplePod(), v1.PodFailed),
			},
			wantWorker: func(t *testing.T, w *podWorkers, records map[types.UID][]syncPodRecord) {
				uid := types.UID("1")
				if len(w.podSyncStatuses) != 0 {
					t.Fatalf("unexpected sync statuses: %#v", w.podSyncStatuses)
				}
				// no pod sync record was delivered
				if actual, expected := records[uid], []syncPodRecord(nil); !reflect.DeepEqual(expected, actual) {
					t.Fatalf("unexpected pod sync records: %s", cmp.Diff(expected, actual, cmp.AllowUnexported(syncPodRecord{})))
				}
			},
			expectMetrics: map[string]string{
				metrics.DesiredPodCount.FQName(): `# HELP kubelet_desired_pods [ALPHA] The number of pods the kubelet is being instructed to run. static is true if the pod is not from the apiserver.
				# TYPE kubelet_desired_pods gauge
				kubelet_desired_pods{static=""} 1
				kubelet_desired_pods{static="true"} 0
				`,
				metrics.ActivePodCount.FQName(): `# HELP kubelet_active_pods [ALPHA] The number of pods the kubelet considers active and which are being considered when admitting new pods. static is true if the pod is not from the apiserver.
				# TYPE kubelet_active_pods gauge
				kubelet_active_pods{static=""} 0
				kubelet_active_pods{static="true"} 0
				`,
				metrics.OrphanedRuntimePodTotal.FQName(): `# HELP kubelet_orphaned_runtime_pods_total [ALPHA] Number of pods that have been detected in the container runtime without being already known to the pod worker. This typically indicates the kubelet was restarted while a pod was force deleted in the API or in the local configuration, which is unusual.
				# TYPE kubelet_orphaned_runtime_pods_total counter
				kubelet_orphaned_runtime_pods_total 0
				`,
				// Note that this test simulates a net-new pod being discovered during HandlePodCleanups that was not
				// delivered to the pod worker via HandlePodAdditions - there is no *known* scenario that can happen, but
				// we want to capture it in the metric. The more likely scenario is that a static pod with a predefined
				// UID is updated, which causes pod config to deliver DELETE -> ADD while the old pod is still shutting
				// down and the pod worker to ignore the ADD. The HandlePodCleanups method then is responsible for syncing
				// that pod to the pod worker so that it restarts.
				metrics.RestartedPodTotal.FQName(): `# HELP kubelet_restarted_pods_total [ALPHA] Number of pods that have been restarted because they were deleted and recreated with the same UID while the kubelet was watching them (common for static pods, extremely uncommon for API pods)
				# TYPE kubelet_restarted_pods_total counter
				kubelet_restarted_pods_total{static=""} 0
				kubelet_restarted_pods_total{static="true"} 0
				`,
				metrics.WorkingPodCount.FQName(): `# HELP kubelet_working_pods [ALPHA] Number of pods the kubelet is actually running, broken down by lifecycle phase, whether the pod is desired, orphaned, or runtime only (also orphaned), and whether the pod is static. An orphaned pod has been removed from local configuration or force deleted in the API and consumes resources that are not otherwise visible.
				# TYPE kubelet_working_pods gauge
				kubelet_working_pods{config="desired",lifecycle="sync",static=""} 0
				kubelet_working_pods{config="desired",lifecycle="sync",static="true"} 0
				kubelet_working_pods{config="desired",lifecycle="terminated",static=""} 0
				kubelet_working_pods{config="desired",lifecycle="terminated",static="true"} 0
				kubelet_working_pods{config="desired",lifecycle="terminating",static=""} 0
				kubelet_working_pods{config="desired",lifecycle="terminating",static="true"} 0
				kubelet_working_pods{config="orphan",lifecycle="sync",static=""} 0
				kubelet_working_pods{config="orphan",lifecycle="sync",static="true"} 0
				kubelet_working_pods{config="orphan",lifecycle="terminated",static=""} 0
				kubelet_working_pods{config="orphan",lifecycle="terminated",static="true"} 0
				kubelet_working_pods{config="orphan",lifecycle="terminating",static=""} 0
				kubelet_working_pods{config="orphan",lifecycle="terminating",static="true"} 0
				kubelet_working_pods{config="runtime_only",lifecycle="sync",static="unknown"} 0
				kubelet_working_pods{config="runtime_only",lifecycle="terminated",static="unknown"} 0
				kubelet_working_pods{config="runtime_only",lifecycle="terminating",static="unknown"} 0
				`,
			},
		},
		{
			name:    "pod is not added to worker by sync method because it has been rejected",
			wantErr: false,
			pods: []*v1.Pod{
				simplePod(),
			},
			rejectedPods: []rejectedPod{
				{uid: "1", reason: "Test", message: "rejected"},
			},
			wantWorker: func(t *testing.T, w *podWorkers, records map[types.UID][]syncPodRecord) {
				uid := types.UID("1")
				if len(w.podSyncStatuses) != 0 {
					t.Fatalf("unexpected sync statuses: %#v", w.podSyncStatuses)
				}
				// no pod sync record was delivered
				if actual, expected := records[uid], []syncPodRecord(nil); !reflect.DeepEqual(expected, actual) {
					t.Fatalf("unexpected pod sync records: %s", cmp.Diff(expected, actual, cmp.AllowUnexported(syncPodRecord{})))
				}
			},
			expectMetrics: map[string]string{
				metrics.DesiredPodCount.FQName(): `# HELP kubelet_desired_pods [ALPHA] The number of pods the kubelet is being instructed to run. static is true if the pod is not from the apiserver.
				# TYPE kubelet_desired_pods gauge
				kubelet_desired_pods{static=""} 1
				kubelet_desired_pods{static="true"} 0
				`,
				metrics.ActivePodCount.FQName(): `# HELP kubelet_active_pods [ALPHA] The number of pods the kubelet considers active and which are being considered when admitting new pods. static is true if the pod is not from the apiserver.
				# TYPE kubelet_active_pods gauge
				kubelet_active_pods{static=""} 0
				kubelet_active_pods{static="true"} 0
				`,
				metrics.OrphanedRuntimePodTotal.FQName(): `# HELP kubelet_orphaned_runtime_pods_total [ALPHA] Number of pods that have been detected in the container runtime without being already known to the pod worker. This typically indicates the kubelet was restarted while a pod was force deleted in the API or in the local configuration, which is unusual.
				# TYPE kubelet_orphaned_runtime_pods_total counter
				kubelet_orphaned_runtime_pods_total 0
				`,
				// Note that this test simulates a net-new pod being discovered during HandlePodCleanups that was not
				// delivered to the pod worker via HandlePodAdditions - there is no *known* scenario that can happen, but
				// we want to capture it in the metric. The more likely scenario is that a static pod with a predefined
				// UID is updated, which causes pod config to deliver DELETE -> ADD while the old pod is still shutting
				// down and the pod worker to ignore the ADD. The HandlePodCleanups method then is responsible for syncing
				// that pod to the pod worker so that it restarts.
				metrics.RestartedPodTotal.FQName(): `# HELP kubelet_restarted_pods_total [ALPHA] Number of pods that have been restarted because they were deleted and recreated with the same UID while the kubelet was watching them (common for static pods, extremely uncommon for API pods)
				# TYPE kubelet_restarted_pods_total counter
				kubelet_restarted_pods_total{static=""} 0
				kubelet_restarted_pods_total{static="true"} 0
				`,
				metrics.WorkingPodCount.FQName(): `# HELP kubelet_working_pods [ALPHA] Number of pods the kubelet is actually running, broken down by lifecycle phase, whether the pod is desired, orphaned, or runtime only (also orphaned), and whether the pod is static. An orphaned pod has been removed from local configuration or force deleted in the API and consumes resources that are not otherwise visible.
				# TYPE kubelet_working_pods gauge
				kubelet_working_pods{config="desired",lifecycle="sync",static=""} 0
				kubelet_working_pods{config="desired",lifecycle="sync",static="true"} 0
				kubelet_working_pods{config="desired",lifecycle="terminated",static=""} 0
				kubelet_working_pods{config="desired",lifecycle="terminated",static="true"} 0
				kubelet_working_pods{config="desired",lifecycle="terminating",static=""} 0
				kubelet_working_pods{config="desired",lifecycle="terminating",static="true"} 0
				kubelet_working_pods{config="orphan",lifecycle="sync",static=""} 0
				kubelet_working_pods{config="orphan",lifecycle="sync",static="true"} 0
				kubelet_working_pods{config="orphan",lifecycle="terminated",static=""} 0
				kubelet_working_pods{config="orphan",lifecycle="terminated",static="true"} 0
				kubelet_working_pods{config="orphan",lifecycle="terminating",static=""} 0
				kubelet_working_pods{config="orphan",lifecycle="terminating",static="true"} 0
				kubelet_working_pods{config="runtime_only",lifecycle="sync",static="unknown"} 0
				kubelet_working_pods{config="runtime_only",lifecycle="terminated",static="unknown"} 0
				kubelet_working_pods{config="runtime_only",lifecycle="terminating",static="unknown"} 0
				`,
			},
		},
		{
			name:    "terminating pod that is known to the config gets no update during pod cleanup",
			wantErr: false,
			pods: []*v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:                       "pod1",
						Namespace:                  "ns1",
						UID:                        types.UID("1"),
						DeletionGracePeriodSeconds: &two,
						DeletionTimestamp:          &deleted,
					},
					Spec: v1.PodSpec{
						TerminationGracePeriodSeconds: &two,
						Containers: []v1.Container{
							{Name: "container-1"},
						},
					},
				},
			},
			runtimePods: []*containertest.FakePod{
				{
					Pod: runtimePod(simplePod()),
				},
			},
			terminatingErr: errors.New("unable to terminate"),
			prepareWorker: func(t *testing.T, w *podWorkers, records map[types.UID][]syncPodRecord) {
				// send a create
				pod := &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{Name: "pod1", Namespace: "ns1", UID: types.UID("1")},
					Spec: v1.PodSpec{
						Containers: []v1.Container{
							{Name: "container-1"},
						},
					},
				}
				w.UpdatePod(UpdatePodOptions{
					UpdateType: kubetypes.SyncPodCreate,
					StartTime:  time.Unix(1, 0).UTC(),
					Pod:        pod,
				})
				drainAllWorkers(w)

				// send a delete update
				updatedPod := &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:                       "pod1",
						Namespace:                  "ns1",
						UID:                        types.UID("1"),
						DeletionGracePeriodSeconds: &two,
						DeletionTimestamp:          &deleted,
					},
					Spec: v1.PodSpec{
						TerminationGracePeriodSeconds: &two,
						Containers: []v1.Container{
							{Name: "container-1"},
						},
					},
				}
				w.UpdatePod(UpdatePodOptions{
					UpdateType: kubetypes.SyncPodKill,
					StartTime:  time.Unix(3, 0).UTC(),
					Pod:        updatedPod,
				})
				drainAllWorkers(w)

				// pod worker thinks pod1 is terminated and pod1 visible to config
				if actual, expected := records[updatedPod.UID], []syncPodRecord{
					{name: "pod1", updateType: kubetypes.SyncPodCreate},
					{name: "pod1", updateType: kubetypes.SyncPodKill, gracePeriod: &two},
				}; !reflect.DeepEqual(expected, actual) {
					t.Fatalf("unexpected pod sync records: %s", cmp.Diff(expected, actual, cmp.AllowUnexported(syncPodRecord{})))
				}
			},
			wantWorker: func(t *testing.T, w *podWorkers, records map[types.UID][]syncPodRecord) {
				uid := types.UID("1")
				if len(w.podSyncStatuses) != 1 {
					t.Fatalf("unexpected sync statuses: %#v", w.podSyncStatuses)
				}
				s, ok := w.podSyncStatuses[uid]
				if !ok || !s.IsTerminationRequested() || !s.IsTerminationStarted() || s.IsFinished() || s.IsWorking() || !s.IsDeleted() {
					t.Fatalf("unexpected requested pod termination: %#v", s)
				}

				// no pod sync record was delivered
				if actual, expected := records[uid], []syncPodRecord{
					{name: "pod1", updateType: kubetypes.SyncPodCreate},
					{name: "pod1", updateType: kubetypes.SyncPodKill, gracePeriod: &two},
				}; !reflect.DeepEqual(expected, actual) {
					t.Fatalf("unexpected pod sync records: %s", cmp.Diff(expected, actual, cmp.AllowUnexported(syncPodRecord{})))
				}
			},
		},
		{
			name:    "pod that could not start and is not in config is force terminated during pod cleanup",
			wantErr: false,
			runtimePods: []*containertest.FakePod{
				{
					Pod: runtimePod(simplePod()),
				},
			},
			terminatingErr: errors.New("unable to terminate"),
			prepareWorker: func(t *testing.T, w *podWorkers, records map[types.UID][]syncPodRecord) {
				// send a create of a static pod
				pod := staticPod()
				// block startup of the static pod due to full name collision
				w.startedStaticPodsByFullname[kubecontainer.GetPodFullName(pod)] = types.UID("2")

				w.UpdatePod(UpdatePodOptions{
					UpdateType: kubetypes.SyncPodCreate,
					StartTime:  time.Unix(1, 0).UTC(),
					Pod:        pod,
				})
				drainAllWorkers(w)

				if _, ok := records[pod.UID]; ok {
					t.Fatalf("unexpected records: %#v", records)
				}
				// pod worker is unaware of pod1 yet, and the kubelet will not have it in the pod manager
			},
			wantWorker: func(t *testing.T, w *podWorkers, records map[types.UID][]syncPodRecord) {
				// the pod is not started and is cleaned, but the runtime state causes us to reenter
				// and perform a direct termination (we never observed the pod as being started by
				// us, and so it is safe to completely tear down)
				uid := types.UID("1")
				if len(w.podSyncStatuses) != 1 {
					t.Fatalf("unexpected sync statuses: %#v", w.podSyncStatuses)
				}

				s, ok := w.podSyncStatuses[uid]
				if !ok || !s.IsTerminationRequested() || !s.IsTerminationStarted() || s.IsFinished() || s.IsWorking() || !s.IsDeleted() {
					t.Errorf("unexpected requested pod termination: %#v", s)
				}

				// ensure that we recorded the appropriate state for replays
				expectedRunningPod := runtimePod(simplePod())
				if actual, expected := s.activeUpdate, (&UpdatePodOptions{
					RunningPod:     expectedRunningPod,
					KillPodOptions: &KillPodOptions{PodTerminationGracePeriodSecondsOverride: &one},
				}); !reflect.DeepEqual(expected, actual) {
					t.Fatalf("unexpected pod activeUpdate: %s", cmp.Diff(expected, actual))
				}

				// sync is never invoked
				if actual, expected := records[uid], []syncPodRecord{
					{name: "pod1", updateType: kubetypes.SyncPodKill, runningPod: expectedRunningPod},
					// this pod is detected as an orphaned running pod and will exit
				}; !reflect.DeepEqual(expected, actual) {
					t.Fatalf("unexpected pod sync records: %s", cmp.Diff(expected, actual, cmp.AllowUnexported(syncPodRecord{})))
				}
			},
			wantWorkerAfterRetry: func(t *testing.T, w *podWorkers, records map[types.UID][]syncPodRecord) {
				uid := types.UID("1")
				if len(w.podSyncStatuses) != 0 {
					t.Fatalf("unexpected sync statuses: %#v", w.podSyncStatuses)
				}

				// expect we get a pod sync record for kill that should have the default grace period
				expectedRunningPod := runtimePod(simplePod())
				if actual, expected := records[uid], []syncPodRecord{
					// first attempt, syncTerminatingPod failed with an error
					{name: "pod1", updateType: kubetypes.SyncPodKill, runningPod: expectedRunningPod},
					// second attempt
					{name: "pod1", updateType: kubetypes.SyncPodKill, runningPod: expectedRunningPod},
				}; !reflect.DeepEqual(expected, actual) {
					t.Fatalf("unexpected pod sync records: %s", cmp.Diff(expected, actual, cmp.AllowUnexported(syncPodRecord{})))
				}
			},
		},
		{
			name:    "pod that could not start still has a pending update and is tracked in metrics",
			wantErr: false,
			pods: []*v1.Pod{
				staticPod(),
			},
			prepareWorker: func(t *testing.T, w *podWorkers, records map[types.UID][]syncPodRecord) {
				// send a create of a static pod
				pod := staticPod()
				// block startup of the static pod due to full name collision
				w.startedStaticPodsByFullname[kubecontainer.GetPodFullName(pod)] = types.UID("2")

				w.UpdatePod(UpdatePodOptions{
					UpdateType: kubetypes.SyncPodCreate,
					StartTime:  time.Unix(1, 0).UTC(),
					Pod:        pod,
				})
				drainAllWorkers(w)

				if _, ok := records[pod.UID]; ok {
					t.Fatalf("unexpected records: %#v", records)
				}
				// pod worker is unaware of pod1 yet
			},
			wantWorker: func(t *testing.T, w *podWorkers, records map[types.UID][]syncPodRecord) {
				uid := types.UID("1")
				if len(w.podSyncStatuses) != 1 {
					t.Fatalf("unexpected sync statuses: %#v", w.podSyncStatuses)
				}
				s, ok := w.podSyncStatuses[uid]
				if !ok || s.IsTerminationRequested() || s.IsTerminationStarted() || s.IsFinished() || s.IsWorking() || s.IsDeleted() || s.restartRequested || s.activeUpdate != nil || s.pendingUpdate == nil {
					t.Errorf("unexpected requested pod termination: %#v", s)
				}

				// expect that no sync calls are made, since the pod doesn't ever start
				if actual, expected := records[uid], []syncPodRecord(nil); !reflect.DeepEqual(expected, actual) {
					t.Fatalf("unexpected pod sync records: %s", cmp.Diff(expected, actual, cmp.AllowUnexported(syncPodRecord{})))
				}
			},
			expectMetrics: map[string]string{
				metrics.DesiredPodCount.FQName(): `# HELP kubelet_desired_pods [ALPHA] The number of pods the kubelet is being instructed to run. static is true if the pod is not from the apiserver.
				# TYPE kubelet_desired_pods gauge
				kubelet_desired_pods{static=""} 0
				kubelet_desired_pods{static="true"} 1
				`,
				metrics.WorkingPodCount.FQName(): `# HELP kubelet_working_pods [ALPHA] Number of pods the kubelet is actually running, broken down by lifecycle phase, whether the pod is desired, orphaned, or runtime only (also orphaned), and whether the pod is static. An orphaned pod has been removed from local configuration or force deleted in the API and consumes resources that are not otherwise visible.
				# TYPE kubelet_working_pods gauge
				kubelet_working_pods{config="desired",lifecycle="sync",static=""} 0
				kubelet_working_pods{config="desired",lifecycle="sync",static="true"} 1
				kubelet_working_pods{config="desired",lifecycle="terminated",static=""} 0
				kubelet_working_pods{config="desired",lifecycle="terminated",static="true"} 0
				kubelet_working_pods{config="desired",lifecycle="terminating",static=""} 0
				kubelet_working_pods{config="desired",lifecycle="terminating",static="true"} 0
				kubelet_working_pods{config="orphan",lifecycle="sync",static=""} 0
				kubelet_working_pods{config="orphan",lifecycle="sync",static="true"} 0
				kubelet_working_pods{config="orphan",lifecycle="terminated",static=""} 0
				kubelet_working_pods{config="orphan",lifecycle="terminated",static="true"} 0
				kubelet_working_pods{config="orphan",lifecycle="terminating",static=""} 0
				kubelet_working_pods{config="orphan",lifecycle="terminating",static="true"} 0
				kubelet_working_pods{config="runtime_only",lifecycle="sync",static="unknown"} 0
				kubelet_working_pods{config="runtime_only",lifecycle="terminated",static="unknown"} 0
				kubelet_working_pods{config="runtime_only",lifecycle="terminating",static="unknown"} 0
				`,
			},
		},
		{
			name:           "pod that could not start and is not in config is force terminated without runtime during pod cleanup",
			wantErr:        false,
			terminatingErr: errors.New("unable to terminate"),
			prepareWorker: func(t *testing.T, w *podWorkers, records map[types.UID][]syncPodRecord) {
				// send a create of a static pod
				pod := staticPod()
				// block startup of the static pod due to full name collision
				w.startedStaticPodsByFullname[kubecontainer.GetPodFullName(pod)] = types.UID("2")

				w.UpdatePod(UpdatePodOptions{
					UpdateType: kubetypes.SyncPodCreate,
					StartTime:  time.Unix(1, 0).UTC(),
					Pod:        pod,
				})
				drainAllWorkers(w)

				if _, ok := records[pod.UID]; ok {
					t.Fatalf("unexpected records: %#v", records)
				}
				// pod worker is unaware of pod1 yet, and the kubelet will not have it in the pod manager
			},
			wantWorker: func(t *testing.T, w *podWorkers, records map[types.UID][]syncPodRecord) {
				uid := types.UID("1")
				if len(w.podSyncStatuses) != 0 {
					t.Fatalf("unexpected sync statuses: %#v", w.podSyncStatuses)
				}

				// expect that no sync calls are made, since the pod doesn't ever start
				if actual, expected := records[uid], []syncPodRecord(nil); !reflect.DeepEqual(expected, actual) {
					t.Fatalf("unexpected pod sync records: %s", cmp.Diff(expected, actual, cmp.AllowUnexported(syncPodRecord{})))
				}
			},
		},
		{
			name:    "pod that is terminating is recreated by config with the same UID",
			wantErr: false,
			pods: []*v1.Pod{
				func() *v1.Pod {
					pod := staticPod()
					pod.Annotations["version"] = "2"
					return pod
				}(),
			},

			runtimePods: []*containertest.FakePod{
				{
					Pod: runtimePod(staticPod()),
				},
			},
			terminatingErr: errors.New("unable to terminate"),
			prepareWorker: func(t *testing.T, w *podWorkers, records map[types.UID][]syncPodRecord) {
				// send a create of a static pod
				pod := staticPod()

				w.UpdatePod(UpdatePodOptions{
					UpdateType: kubetypes.SyncPodCreate,
					StartTime:  time.Unix(1, 0).UTC(),
					Pod:        pod,
				})
				drainAllWorkers(w)

				// terminate the pod (which won't complete) and then deliver a recreate by that same UID
				w.UpdatePod(UpdatePodOptions{
					UpdateType: kubetypes.SyncPodKill,
					StartTime:  time.Unix(2, 0).UTC(),
					Pod:        pod,
				})
				pod = staticPod()
				pod.Annotations["version"] = "2"
				w.UpdatePod(UpdatePodOptions{
					UpdateType: kubetypes.SyncPodCreate,
					StartTime:  time.Unix(3, 0).UTC(),
					Pod:        pod,
				})
				drainAllWorkers(w)

				// expect we get a pod sync record for kill that should have the default grace period
				if actual, expected := records[pod.UID], []syncPodRecord{
					{name: "pod1", updateType: kubetypes.SyncPodCreate},
					{name: "pod1", updateType: kubetypes.SyncPodKill, gracePeriod: &one},
				}; !reflect.DeepEqual(expected, actual) {
					t.Fatalf("unexpected pod sync records: %s", cmp.Diff(expected, actual, cmp.AllowUnexported(syncPodRecord{})))
				}
				// pod worker is aware of pod1, but the kubelet will not have it in the pod manager
			},
			wantWorker: func(t *testing.T, w *podWorkers, records map[types.UID][]syncPodRecord) {
				uid := types.UID("1")
				if len(w.podSyncStatuses) != 1 {
					t.Fatalf("unexpected sync statuses: %#v", w.podSyncStatuses)
				}
				s, ok := w.podSyncStatuses[uid]
				if !ok || !s.IsTerminationRequested() || !s.IsTerminationStarted() || s.IsFinished() || s.IsWorking() || s.IsDeleted() || !s.restartRequested {
					t.Errorf("unexpected requested pod termination: %#v", s)
				}

				// expect we get a pod sync record for kill that should have the default grace period
				if actual, expected := records[uid], []syncPodRecord{
					{name: "pod1", updateType: kubetypes.SyncPodCreate},
					{name: "pod1", updateType: kubetypes.SyncPodKill, gracePeriod: &one},
				}; !reflect.DeepEqual(expected, actual) {
					t.Fatalf("unexpected pod sync records: %s", cmp.Diff(expected, actual, cmp.AllowUnexported(syncPodRecord{})))
				}
			},
			expectMetrics: map[string]string{
				metrics.DesiredPodCount.FQName(): `# HELP kubelet_desired_pods [ALPHA] The number of pods the kubelet is being instructed to run. static is true if the pod is not from the apiserver.
				# TYPE kubelet_desired_pods gauge
				kubelet_desired_pods{static=""} 0
				kubelet_desired_pods{static="true"} 1
				`,
				metrics.ActivePodCount.FQName(): `# HELP kubelet_active_pods [ALPHA] The number of pods the kubelet considers active and which are being considered when admitting new pods. static is true if the pod is not from the apiserver.
				# TYPE kubelet_active_pods gauge
				kubelet_active_pods{static=""} 0
				kubelet_active_pods{static="true"} 1
				`,
				metrics.OrphanedRuntimePodTotal.FQName(): `# HELP kubelet_orphaned_runtime_pods_total [ALPHA] Number of pods that have been detected in the container runtime without being already known to the pod worker. This typically indicates the kubelet was restarted while a pod was force deleted in the API or in the local configuration, which is unusual.
				# TYPE kubelet_orphaned_runtime_pods_total counter
				kubelet_orphaned_runtime_pods_total 0
				`,
				metrics.RestartedPodTotal.FQName(): `# HELP kubelet_restarted_pods_total [ALPHA] Number of pods that have been restarted because they were deleted and recreated with the same UID while the kubelet was watching them (common for static pods, extremely uncommon for API pods)
				# TYPE kubelet_restarted_pods_total counter
				kubelet_restarted_pods_total{static=""} 0
				kubelet_restarted_pods_total{static="true"} 0
				`,
				metrics.WorkingPodCount.FQName(): `# HELP kubelet_working_pods [ALPHA] Number of pods the kubelet is actually running, broken down by lifecycle phase, whether the pod is desired, orphaned, or runtime only (also orphaned), and whether the pod is static. An orphaned pod has been removed from local configuration or force deleted in the API and consumes resources that are not otherwise visible.
				# TYPE kubelet_working_pods gauge
				kubelet_working_pods{config="desired",lifecycle="sync",static=""} 0
				kubelet_working_pods{config="desired",lifecycle="sync",static="true"} 0
				kubelet_working_pods{config="desired",lifecycle="terminated",static=""} 0
				kubelet_working_pods{config="desired",lifecycle="terminated",static="true"} 0
				kubelet_working_pods{config="desired",lifecycle="terminating",static=""} 0
				kubelet_working_pods{config="desired",lifecycle="terminating",static="true"} 1
				kubelet_working_pods{config="orphan",lifecycle="sync",static=""} 0
				kubelet_working_pods{config="orphan",lifecycle="sync",static="true"} 0
				kubelet_working_pods{config="orphan",lifecycle="terminated",static=""} 0
				kubelet_working_pods{config="orphan",lifecycle="terminated",static="true"} 0
				kubelet_working_pods{config="orphan",lifecycle="terminating",static=""} 0
				kubelet_working_pods{config="orphan",lifecycle="terminating",static="true"} 0
				kubelet_working_pods{config="runtime_only",lifecycle="sync",static="unknown"} 0
				kubelet_working_pods{config="runtime_only",lifecycle="terminated",static="unknown"} 0
				kubelet_working_pods{config="runtime_only",lifecycle="terminating",static="unknown"} 0
				`,
			},
			expectMetricsAfterRetry: map[string]string{
				metrics.RestartedPodTotal.FQName(): `# HELP kubelet_restarted_pods_total [ALPHA] Number of pods that have been restarted because they were deleted and recreated with the same UID while the kubelet was watching them (common for static pods, extremely uncommon for API pods)
				# TYPE kubelet_restarted_pods_total counter
				kubelet_restarted_pods_total{static=""} 0
				kubelet_restarted_pods_total{static="true"} 1
				`,
			},
		},
		{
			name:    "started pod that is not in config is force terminated during pod cleanup",
			wantErr: false,
			runtimePods: []*containertest.FakePod{
				{
					Pod: runtimePod(simplePod()),
				},
			},
			terminatingErr: errors.New("unable to terminate"),
			prepareWorker: func(t *testing.T, w *podWorkers, records map[types.UID][]syncPodRecord) {
				// send a create of a static pod
				pod := staticPod()

				w.UpdatePod(UpdatePodOptions{
					UpdateType: kubetypes.SyncPodCreate,
					StartTime:  time.Unix(1, 0).UTC(),
					Pod:        pod,
				})
				drainAllWorkers(w)

				// expect we get a pod sync record for kill that should have the default grace period
				if actual, expected := records[pod.UID], []syncPodRecord{
					{name: "pod1", updateType: kubetypes.SyncPodCreate},
				}; !reflect.DeepEqual(expected, actual) {
					t.Fatalf("unexpected pod sync records: %s", cmp.Diff(expected, actual, cmp.AllowUnexported(syncPodRecord{})))
				}
				// pod worker is aware of pod1, but the kubelet will not have it in the pod manager
			},
			wantWorker: func(t *testing.T, w *podWorkers, records map[types.UID][]syncPodRecord) {
				uid := types.UID("1")
				if len(w.podSyncStatuses) != 1 {
					t.Fatalf("unexpected sync statuses: %#v", w.podSyncStatuses)
				}
				s, ok := w.podSyncStatuses[uid]
				if !ok || !s.IsTerminationRequested() || !s.IsTerminationStarted() || s.IsFinished() || s.IsWorking() || !s.IsDeleted() {
					t.Errorf("unexpected requested pod termination: %#v", s)
				}

				// expect we get a pod sync record for kill that should have the default grace period
				if actual, expected := records[uid], []syncPodRecord{
					{name: "pod1", updateType: kubetypes.SyncPodCreate},
					{name: "pod1", updateType: kubetypes.SyncPodKill},
				}; !reflect.DeepEqual(expected, actual) {
					t.Fatalf("unexpected pod sync records: %s", cmp.Diff(expected, actual, cmp.AllowUnexported(syncPodRecord{})))
				}
			},
		},
		{
			name:           "started pod that is not in config or runtime is force terminated during pod cleanup",
			wantErr:        false,
			runtimePods:    []*containertest.FakePod{},
			terminatingErr: errors.New("unable to terminate"),
			prepareWorker: func(t *testing.T, w *podWorkers, records map[types.UID][]syncPodRecord) {
				// send a create of a static pod
				pod := staticPod()

				w.UpdatePod(UpdatePodOptions{
					UpdateType: kubetypes.SyncPodCreate,
					StartTime:  time.Unix(1, 0).UTC(),
					Pod:        pod,
					MirrorPod:  mirrorPod(pod, "node-1", "node-uid-1"),
				})
				drainAllWorkers(w)

				// expect we get a pod sync record for kill that should have the default grace period
				if actual, expected := records[pod.UID], []syncPodRecord{
					{name: "pod1", updateType: kubetypes.SyncPodCreate},
				}; !reflect.DeepEqual(expected, actual) {
					t.Fatalf("unexpected pod sync records: %s", cmp.Diff(expected, actual, cmp.AllowUnexported(syncPodRecord{})))
				}
				// pod worker is aware of pod1, but the kubelet will not have it in the pod manager
			},
			wantWorker: func(t *testing.T, w *podWorkers, records map[types.UID][]syncPodRecord) {
				uid := types.UID("1")
				if len(w.podSyncStatuses) != 1 {
					t.Fatalf("unexpected sync statuses: %#v", w.podSyncStatuses)
				}
				s, ok := w.podSyncStatuses[uid]
				if !ok || !s.IsTerminationRequested() || !s.IsTerminationStarted() || s.IsFinished() || s.IsWorking() || !s.IsDeleted() {
					t.Errorf("unexpected requested pod termination: %#v", s)
				}

				// ensure that we recorded the appropriate state for replays
				expectedPod := staticPod()
				if actual, expected := s.activeUpdate, (&UpdatePodOptions{
					Pod:       expectedPod,
					MirrorPod: mirrorPod(expectedPod, "node-1", "node-uid-1"),
				}); !reflect.DeepEqual(expected, actual) {
					t.Fatalf("unexpected pod activeUpdate: %s", cmp.Diff(expected, actual))
				}

				// expect we get a pod sync record for kill that should have the default grace period
				if actual, expected := records[uid], []syncPodRecord{
					{name: "pod1", updateType: kubetypes.SyncPodCreate},
					{name: "pod1", updateType: kubetypes.SyncPodKill},
				}; !reflect.DeepEqual(expected, actual) {
					t.Fatalf("unexpected pod sync records: %s", cmp.Diff(expected, actual, cmp.AllowUnexported(syncPodRecord{})))
				}
			},
			wantWorkerAfterRetry: func(t *testing.T, w *podWorkers, records map[types.UID][]syncPodRecord) {
				uid := types.UID("1")
				if len(w.podSyncStatuses) != 1 {
					t.Fatalf("unexpected sync statuses: %#v", w.podSyncStatuses)
				}
				s, ok := w.podSyncStatuses[uid]
				if !ok || !s.IsTerminationRequested() || !s.IsTerminationStarted() || !s.IsFinished() || s.IsWorking() || !s.IsDeleted() {
					t.Errorf("unexpected requested pod termination: %#v", s)
				}

				// ensure that we recorded the appropriate state for replays
				expectedPod := staticPod()
				if actual, expected := s.activeUpdate, (&UpdatePodOptions{
					Pod:       expectedPod,
					MirrorPod: mirrorPod(expectedPod, "node-1", "node-uid-1"),
				}); !reflect.DeepEqual(expected, actual) {
					t.Fatalf("unexpected pod activeUpdate: %s", cmp.Diff(expected, actual))
				}

				// expect we get a pod sync record for kill that should have the default grace period
				if actual, expected := records[uid], []syncPodRecord{
					{name: "pod1", updateType: kubetypes.SyncPodCreate},
					{name: "pod1", updateType: kubetypes.SyncPodKill},
					// second attempt at kill
					{name: "pod1", updateType: kubetypes.SyncPodKill},
					{name: "pod1", terminated: true},
				}; !reflect.DeepEqual(expected, actual) {
					t.Fatalf("unexpected pod sync records: %s", cmp.Diff(expected, actual, cmp.AllowUnexported(syncPodRecord{})))
				}
			},
		},
		{
			name:    "terminated pod is restarted in the same invocation that it is detected",
			wantErr: false,
			pods: []*v1.Pod{
				func() *v1.Pod {
					pod := staticPod()
					pod.Annotations = map[string]string{"version": "2"}
					return pod
				}(),
			},
			prepareWorker: func(t *testing.T, w *podWorkers, records map[types.UID][]syncPodRecord) {
				// simulate a delete and recreate of the static pod
				pod := simplePod()
				w.UpdatePod(UpdatePodOptions{
					UpdateType: kubetypes.SyncPodCreate,
					StartTime:  time.Unix(1, 0).UTC(),
					Pod:        pod,
				})
				drainAllWorkers(w)
				w.UpdatePod(UpdatePodOptions{
					UpdateType: kubetypes.SyncPodKill,
					Pod:        pod,
				})
				pod2 := simplePod()
				pod2.Annotations = map[string]string{"version": "2"}
				w.UpdatePod(UpdatePodOptions{
					UpdateType: kubetypes.SyncPodCreate,
					Pod:        pod2,
				})
				drainAllWorkers(w)
			},
			wantWorker: func(t *testing.T, w *podWorkers, records map[types.UID][]syncPodRecord) {
				uid := types.UID("1")
				if len(w.podSyncStatuses) != 1 {
					t.Fatalf("unexpected sync statuses: %#v", w.podSyncStatuses)
				}
				s, ok := w.podSyncStatuses[uid]
				if !ok || s.IsTerminationRequested() || s.IsTerminationStarted() || s.IsFinished() || s.IsWorking() || s.IsDeleted() {
					t.Fatalf("unexpected requested pod termination: %#v", s)
				}
				if s.pendingUpdate != nil || s.activeUpdate == nil || s.activeUpdate.Pod == nil || s.activeUpdate.Pod.Annotations["version"] != "2" {
					t.Fatalf("unexpected restarted pod: %#v", s.activeUpdate.Pod)
				}
				// expect we get a pod sync record for kill that should have the same grace period as before (2), but no
				// running pod because the SyncKnownPods method killed it
				if actual, expected := records[uid], []syncPodRecord{
					{name: "pod1", updateType: kubetypes.SyncPodCreate},
					{name: "pod1", updateType: kubetypes.SyncPodKill, gracePeriod: &one},
					{name: "pod1", terminated: true},
					{name: "pod1", updateType: kubetypes.SyncPodCreate},
				}; !reflect.DeepEqual(expected, actual) {
					t.Fatalf("unexpected pod sync records: %s", cmp.Diff(expected, actual, cmp.AllowUnexported(syncPodRecord{})))
				}
			},
			expectMetrics: map[string]string{
				metrics.DesiredPodCount.FQName(): `# HELP kubelet_desired_pods [ALPHA] The number of pods the kubelet is being instructed to run. static is true if the pod is not from the apiserver.
				# TYPE kubelet_desired_pods gauge
				kubelet_desired_pods{static=""} 1
				kubelet_desired_pods{static="true"} 0
				`,
				metrics.ActivePodCount.FQName(): `# HELP kubelet_active_pods [ALPHA] The number of pods the kubelet considers active and which are being considered when admitting new pods. static is true if the pod is not from the apiserver.
				# TYPE kubelet_active_pods gauge
				kubelet_active_pods{static=""} 1
				kubelet_active_pods{static="true"} 0
				`,
				metrics.OrphanedRuntimePodTotal.FQName(): `# HELP kubelet_orphaned_runtime_pods_total [ALPHA] Number of pods that have been detected in the container runtime without being already known to the pod worker. This typically indicates the kubelet was restarted while a pod was force deleted in the API or in the local configuration, which is unusual.
				# TYPE kubelet_orphaned_runtime_pods_total counter
				kubelet_orphaned_runtime_pods_total 0
				`,
				metrics.RestartedPodTotal.FQName(): `# HELP kubelet_restarted_pods_total [ALPHA] Number of pods that have been restarted because they were deleted and recreated with the same UID while the kubelet was watching them (common for static pods, extremely uncommon for API pods)
				# TYPE kubelet_restarted_pods_total counter
				kubelet_restarted_pods_total{static=""} 1
				kubelet_restarted_pods_total{static="true"} 0
				`,
				metrics.WorkingPodCount.FQName(): `# HELP kubelet_working_pods [ALPHA] Number of pods the kubelet is actually running, broken down by lifecycle phase, whether the pod is desired, orphaned, or runtime only (also orphaned), and whether the pod is static. An orphaned pod has been removed from local configuration or force deleted in the API and consumes resources that are not otherwise visible.
				# TYPE kubelet_working_pods gauge
				kubelet_working_pods{config="desired",lifecycle="sync",static=""} 1
				kubelet_working_pods{config="desired",lifecycle="sync",static="true"} 0
				kubelet_working_pods{config="desired",lifecycle="terminated",static=""} 0
				kubelet_working_pods{config="desired",lifecycle="terminated",static="true"} 0
				kubelet_working_pods{config="desired",lifecycle="terminating",static=""} 0
				kubelet_working_pods{config="desired",lifecycle="terminating",static="true"} 0
				kubelet_working_pods{config="orphan",lifecycle="sync",static=""} 0
				kubelet_working_pods{config="orphan",lifecycle="sync",static="true"} 0
				kubelet_working_pods{config="orphan",lifecycle="terminated",static=""} 0
				kubelet_working_pods{config="orphan",lifecycle="terminated",static="true"} 0
				kubelet_working_pods{config="orphan",lifecycle="terminating",static=""} 0
				kubelet_working_pods{config="orphan",lifecycle="terminating",static="true"} 0
				kubelet_working_pods{config="runtime_only",lifecycle="sync",static="unknown"} 0
				kubelet_working_pods{config="runtime_only",lifecycle="terminated",static="unknown"} 0
				kubelet_working_pods{config="runtime_only",lifecycle="terminating",static="unknown"} 0
				`,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// clear the metrics for testing
			metrics.Register()
			for _, metric := range []interface{ Reset() }{
				metrics.DesiredPodCount,
				metrics.ActivePodCount,
				metrics.RestartedPodTotal,
				metrics.OrphanedRuntimePodTotal,
				metrics.WorkingPodCount,
			} {
				metric.Reset()
			}
			metrics.MirrorPodCount.Set(0)

			testKubelet := newTestKubelet(t, false)
			defer testKubelet.Cleanup()
			kl := testKubelet.kubelet

			podWorkers, _, processed := createPodWorkers()
			kl.podWorkers = podWorkers
			originalPodSyncer := podWorkers.podSyncer
			syncFuncs := newPodSyncerFuncs(originalPodSyncer)
			podWorkers.podSyncer = &syncFuncs
			if tt.terminatingErr != nil {
				syncFuncs.syncTerminatingPod = func(ctx context.Context, pod *v1.Pod, podStatus *kubecontainer.PodStatus, gracePeriod *int64, podStatusFn func(*v1.PodStatus)) error {
					t.Logf("called syncTerminatingPod")
					if err := originalPodSyncer.SyncTerminatingPod(ctx, pod, podStatus, gracePeriod, podStatusFn); err != nil {
						t.Fatalf("unexpected error in syncTerminatingPodFn: %v", err)
					}
					return tt.terminatingErr
				}
				syncFuncs.syncTerminatingRuntimePod = func(ctx context.Context, runningPod *kubecontainer.Pod) error {
					if err := originalPodSyncer.SyncTerminatingRuntimePod(ctx, runningPod); err != nil {
						t.Fatalf("unexpected error in syncTerminatingRuntimePodFn: %v", err)
					}
					return tt.terminatingErr
				}
			}
			if tt.prepareWorker != nil {
				tt.prepareWorker(t, podWorkers, processed)
			}

			testKubelet.fakeRuntime.PodList = tt.runtimePods
			kl.podManager.SetPods(tt.pods)

			for _, reject := range tt.rejectedPods {
				pod, ok := kl.podManager.GetPodByUID(reject.uid)
				if !ok {
					t.Fatalf("unable to reject pod by UID %v", reject.uid)
				}
				kl.rejectPod(pod, reject.reason, reject.message)
			}

			if err := kl.HandlePodCleanups(context.Background()); (err != nil) != tt.wantErr {
				t.Errorf("Kubelet.HandlePodCleanups() error = %v, wantErr %v", err, tt.wantErr)
			}
			drainAllWorkers(podWorkers)
			if tt.wantWorker != nil {
				tt.wantWorker(t, podWorkers, processed)
			}

			for k, v := range tt.expectMetrics {
				testMetric(t, k, v)
			}

			// check after the terminating error clears
			if tt.wantWorkerAfterRetry != nil {
				podWorkers.podSyncer = originalPodSyncer
				if err := kl.HandlePodCleanups(context.Background()); (err != nil) != tt.wantErr {
					t.Errorf("Kubelet.HandlePodCleanups() second error = %v, wantErr %v", err, tt.wantErr)
				}
				drainAllWorkers(podWorkers)
				tt.wantWorkerAfterRetry(t, podWorkers, processed)

				for k, v := range tt.expectMetricsAfterRetry {
					testMetric(t, k, v)
				}
			}
		})
	}
}

func testMetric(t *testing.T, metricName string, expectedMetric string) {
	t.Helper()
	err := testutil.GatherAndCompare(metrics.GetGather(), strings.NewReader(expectedMetric), metricName)
	if err != nil {
		t.Error(err)
	}
}

func TestGetNonExistentImagePullSecret(t *testing.T) {
	secrets := make([]*v1.Secret, 0)
	fakeRecorder := record.NewFakeRecorder(1)
	testKubelet := newTestKubelet(t, false /* controllerAttachDetachEnabled */)
	testKubelet.kubelet.recorder = fakeRecorder
	testKubelet.kubelet.secretManager = secret.NewFakeManagerWithSecrets(secrets)
	defer testKubelet.Cleanup()

	expectedEvent := "Warning FailedToRetrieveImagePullSecret Unable to retrieve some image pull secrets (secretFoo); attempting to pull the image may not succeed."

	testPod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:   "nsFoo",
			Name:        "podFoo",
			Annotations: map[string]string{},
		},
		Spec: v1.PodSpec{
			ImagePullSecrets: []v1.LocalObjectReference{
				{Name: "secretFoo"},
			},
		},
	}

	pullSecrets := testKubelet.kubelet.getPullSecretsForPod(testPod)
	assert.Empty(t, pullSecrets)

	assert.Len(t, fakeRecorder.Events, 1)
	event := <-fakeRecorder.Events
	assert.Equal(t, expectedEvent, event)
}

func TestParseGetSubIdsOutput(t *testing.T) {
	tests := []struct {
		name         string
		input        string
		wantFirstID  uint32
		wantRangeLen uint32
		wantErr      bool
	}{
		{
			name:         "valid",
			input:        "0: kubelet 65536 2147483648",
			wantFirstID:  65536,
			wantRangeLen: 2147483648,
		},
		{
			name:    "multiple lines",
			input:   "0: kubelet 1 2\n1: kubelet 3 4\n",
			wantErr: true,
		},
		{
			name:    "wrong format",
			input:   "0: kubelet 65536",
			wantErr: true,
		},
		{
			name:    "non numeric 1",
			input:   "0: kubelet Foo 65536",
			wantErr: true,
		},
		{
			name:    "non numeric 2",
			input:   "0: kubelet 0 Bar",
			wantErr: true,
		},
		{
			name:    "overflow 1",
			input:   "0: kubelet 4294967296 2147483648",
			wantErr: true,
		},
		{
			name:    "overflow 2",
			input:   "0: kubelet 65536 4294967296",
			wantErr: true,
		},
		{
			name:    "negative value 1",
			input:   "0: kubelet -1 2147483648",
			wantErr: true,
		},
		{
			name:    "negative value 2",
			input:   "0: kubelet 65536 -1",
			wantErr: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotFirstID, gotRangeLen, err := parseGetSubIdsOutput(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Errorf("%s: expected error, got nil", tc.name)
				}
			} else {
				if err != nil {
					t.Errorf("%s: unexpected error: %v", tc.name, err)
				}
				if gotFirstID != tc.wantFirstID || gotRangeLen != tc.wantRangeLen {
					t.Errorf("%s: got (%d, %d), want (%d, %d)", tc.name, gotFirstID, gotRangeLen, tc.wantFirstID, tc.wantRangeLen)
				}
			}
		})
	}
}

func TestResolveRecursiveReadOnly(t *testing.T) {
	testCases := []struct {
		m                  v1.VolumeMount
		runtimeSupportsRRO bool
		expected           bool
		expectedErr        string
	}{
		{
			m:                  v1.VolumeMount{Name: "rw"},
			runtimeSupportsRRO: true,
			expected:           false,
			expectedErr:        "",
		},
		{
			m:                  v1.VolumeMount{Name: "ro", ReadOnly: true},
			runtimeSupportsRRO: true,
			expected:           false,
			expectedErr:        "",
		},
		{
			m:                  v1.VolumeMount{Name: "ro", ReadOnly: true, RecursiveReadOnly: ptr.To(v1.RecursiveReadOnlyDisabled)},
			runtimeSupportsRRO: true,
			expected:           false,
			expectedErr:        "",
		},
		{
			m:                  v1.VolumeMount{Name: "rro-if-possible", ReadOnly: true, RecursiveReadOnly: ptr.To(v1.RecursiveReadOnlyIfPossible)},
			runtimeSupportsRRO: true,
			expected:           true,
			expectedErr:        "",
		},
		{
			m: v1.VolumeMount{Name: "rro-if-possible", ReadOnly: true, RecursiveReadOnly: ptr.To(v1.RecursiveReadOnlyIfPossible),
				MountPropagation: ptr.To(v1.MountPropagationNone)},
			runtimeSupportsRRO: true,
			expected:           true,
			expectedErr:        "",
		},
		{
			m: v1.VolumeMount{Name: "rro-if-possible", ReadOnly: true, RecursiveReadOnly: ptr.To(v1.RecursiveReadOnlyIfPossible),
				MountPropagation: ptr.To(v1.MountPropagationHostToContainer)},
			runtimeSupportsRRO: true,
			expected:           false,
			expectedErr:        "not compatible with propagation",
		},
		{
			m: v1.VolumeMount{Name: "rro-if-possible", ReadOnly: true, RecursiveReadOnly: ptr.To(v1.RecursiveReadOnlyIfPossible),
				MountPropagation: ptr.To(v1.MountPropagationBidirectional)},
			runtimeSupportsRRO: true,
			expected:           false,
			expectedErr:        "not compatible with propagation",
		},
		{
			m:                  v1.VolumeMount{Name: "rro-if-possible", ReadOnly: false, RecursiveReadOnly: ptr.To(v1.RecursiveReadOnlyIfPossible)},
			runtimeSupportsRRO: true,
			expected:           false,
			expectedErr:        "not read-only",
		},
		{
			m:                  v1.VolumeMount{Name: "rro-if-possible", ReadOnly: false, RecursiveReadOnly: ptr.To(v1.RecursiveReadOnlyIfPossible)},
			runtimeSupportsRRO: false,
			expected:           false,
			expectedErr:        "not read-only",
		},
		{
			m:                  v1.VolumeMount{Name: "rro", ReadOnly: true, RecursiveReadOnly: ptr.To(v1.RecursiveReadOnlyEnabled)},
			runtimeSupportsRRO: true,
			expected:           true,
			expectedErr:        "",
		},
		{
			m: v1.VolumeMount{Name: "rro", ReadOnly: true, RecursiveReadOnly: ptr.To(v1.RecursiveReadOnlyEnabled),
				MountPropagation: ptr.To(v1.MountPropagationNone)},
			runtimeSupportsRRO: true,
			expected:           true,
			expectedErr:        "",
		},
		{
			m: v1.VolumeMount{Name: "rro", ReadOnly: true, RecursiveReadOnly: ptr.To(v1.RecursiveReadOnlyEnabled),
				MountPropagation: ptr.To(v1.MountPropagationHostToContainer)},
			runtimeSupportsRRO: true,
			expected:           false,
			expectedErr:        "not compatible with propagation",
		},
		{
			m: v1.VolumeMount{Name: "rro", ReadOnly: true, RecursiveReadOnly: ptr.To(v1.RecursiveReadOnlyEnabled),
				MountPropagation: ptr.To(v1.MountPropagationBidirectional)},
			runtimeSupportsRRO: true,
			expected:           false,
			expectedErr:        "not compatible with propagation",
		},
		{
			m:                  v1.VolumeMount{Name: "rro", RecursiveReadOnly: ptr.To(v1.RecursiveReadOnlyEnabled)},
			runtimeSupportsRRO: true,
			expected:           false,
			expectedErr:        "not read-only",
		},
		{
			m:                  v1.VolumeMount{Name: "rro", ReadOnly: true, RecursiveReadOnly: ptr.To(v1.RecursiveReadOnlyEnabled)},
			runtimeSupportsRRO: false,
			expected:           false,
			expectedErr:        "not supported by the runtime",
		},
		{
			m:                  v1.VolumeMount{Name: "invalid", ReadOnly: true, RecursiveReadOnly: ptr.To(v1.RecursiveReadOnlyMode("foo"))},
			runtimeSupportsRRO: true,
			expected:           false,
			expectedErr:        "unknown recursive read-only mode",
		},
	}

	for _, tc := range testCases {
		got, err := resolveRecursiveReadOnly(tc.m, tc.runtimeSupportsRRO)
		t.Logf("resolveRecursiveReadOnly(%+v, %v) = (%v, %v)", tc.m, tc.runtimeSupportsRRO, got, err)
		if tc.expectedErr == "" {
			assert.Equal(t, tc.expected, got)
			assert.NoError(t, err)
		} else {
			assert.ErrorContains(t, err, tc.expectedErr)
		}
	}
}

func TestAllocatedResourcesMatchStatus(t *testing.T) {
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.SidecarContainers, true)
	containerRestartPolicyAlways := v1.ContainerRestartPolicyAlways
	tests := []struct {
		name               string
		allocatedResources v1.ResourceRequirements
		statusResources    *kubecontainer.ContainerResources
		statusTerminated   bool
		expectMatch        bool
	}{{
		name: "guaranteed pod: match",
		allocatedResources: v1.ResourceRequirements{
			Requests: v1.ResourceList{
				v1.ResourceCPU:    resource.MustParse("100m"),
				v1.ResourceMemory: resource.MustParse("100M"),
			},
			Limits: v1.ResourceList{
				v1.ResourceCPU:    resource.MustParse("100m"),
				v1.ResourceMemory: resource.MustParse("100M"),
			},
		},
		statusResources: &kubecontainer.ContainerResources{
			CPURequest:  resource.NewMilliQuantity(100, resource.DecimalSI),
			CPULimit:    resource.NewMilliQuantity(100, resource.DecimalSI),
			MemoryLimit: resource.NewScaledQuantity(100, 6),
		},
		expectMatch: true,
	}, {
		name: "guaranteed pod: cpu request mismatch",
		allocatedResources: v1.ResourceRequirements{
			Requests: v1.ResourceList{
				v1.ResourceCPU:    resource.MustParse("100m"),
				v1.ResourceMemory: resource.MustParse("100M"),
			},
			Limits: v1.ResourceList{
				v1.ResourceCPU:    resource.MustParse("100m"),
				v1.ResourceMemory: resource.MustParse("100M"),
			},
		},
		statusResources: &kubecontainer.ContainerResources{
			CPURequest:  resource.NewMilliQuantity(50, resource.DecimalSI),
			CPULimit:    resource.NewMilliQuantity(100, resource.DecimalSI),
			MemoryLimit: resource.NewScaledQuantity(100, 6),
		},
		expectMatch: false,
	}, {
		name: "guaranteed pod: cpu limit mismatch",
		allocatedResources: v1.ResourceRequirements{
			Requests: v1.ResourceList{
				v1.ResourceCPU:    resource.MustParse("100m"),
				v1.ResourceMemory: resource.MustParse("100M"),
			},
			Limits: v1.ResourceList{
				v1.ResourceCPU:    resource.MustParse("100m"),
				v1.ResourceMemory: resource.MustParse("100M"),
			},
		},
		statusResources: &kubecontainer.ContainerResources{
			CPURequest:  resource.NewMilliQuantity(100, resource.DecimalSI),
			CPULimit:    resource.NewMilliQuantity(50, resource.DecimalSI),
			MemoryLimit: resource.NewScaledQuantity(100, 6),
		},
		expectMatch: false,
	}, {
		name: "guaranteed pod: memory limit mismatch",
		allocatedResources: v1.ResourceRequirements{
			Requests: v1.ResourceList{
				v1.ResourceCPU:    resource.MustParse("100m"),
				v1.ResourceMemory: resource.MustParse("100M"),
			},
			Limits: v1.ResourceList{
				v1.ResourceCPU:    resource.MustParse("100m"),
				v1.ResourceMemory: resource.MustParse("100M"),
			},
		},
		statusResources: &kubecontainer.ContainerResources{
			CPURequest:  resource.NewMilliQuantity(100, resource.DecimalSI),
			CPULimit:    resource.NewMilliQuantity(100, resource.DecimalSI),
			MemoryLimit: resource.NewScaledQuantity(50, 6),
		},
		expectMatch: false,
	}, {
		name: "guaranteed pod: terminated mismatch",
		allocatedResources: v1.ResourceRequirements{
			Requests: v1.ResourceList{
				v1.ResourceCPU:    resource.MustParse("100m"),
				v1.ResourceMemory: resource.MustParse("100M"),
			},
			Limits: v1.ResourceList{
				v1.ResourceCPU:    resource.MustParse("100m"),
				v1.ResourceMemory: resource.MustParse("100M"),
			},
		},
		statusResources: &kubecontainer.ContainerResources{
			CPURequest:  resource.NewMilliQuantity(100, resource.DecimalSI),
			CPULimit:    resource.NewMilliQuantity(100, resource.DecimalSI),
			MemoryLimit: resource.NewScaledQuantity(50, 6),
		},
		statusTerminated: true,
		expectMatch:      true,
	}, {
		name: "burstable: no cpu request",
		allocatedResources: v1.ResourceRequirements{
			Requests: v1.ResourceList{
				v1.ResourceMemory: resource.MustParse("100M"),
			},
		},
		statusResources: &kubecontainer.ContainerResources{
			CPURequest: resource.NewMilliQuantity(2, resource.DecimalSI),
		},
		expectMatch: true,
	}, {
		name: "burstable: min cpu request",
		allocatedResources: v1.ResourceRequirements{
			Requests: v1.ResourceList{
				v1.ResourceMemory: resource.MustParse("100M"),
				v1.ResourceCPU:    resource.MustParse("2m"),
			},
		},
		statusResources: &kubecontainer.ContainerResources{
			CPURequest: resource.NewMilliQuantity(2, resource.DecimalSI),
		},
		expectMatch: true,
	}, {
		name: "burstable: below min cpu request",
		allocatedResources: v1.ResourceRequirements{
			Requests: v1.ResourceList{
				v1.ResourceMemory: resource.MustParse("100M"),
				v1.ResourceCPU:    resource.MustParse("1m"),
			},
		},
		statusResources: &kubecontainer.ContainerResources{
			CPURequest: resource.NewMilliQuantity(2, resource.DecimalSI),
		},
		expectMatch: true,
	}, {
		name: "burstable: min cpu limit",
		allocatedResources: v1.ResourceRequirements{
			Requests: v1.ResourceList{
				v1.ResourceCPU: resource.MustParse("10m"),
			},
			Limits: v1.ResourceList{
				v1.ResourceCPU: resource.MustParse("10m"),
			},
		},
		statusResources: &kubecontainer.ContainerResources{
			CPURequest: resource.NewMilliQuantity(10, resource.DecimalSI),
			CPULimit:   resource.NewMilliQuantity(10, resource.DecimalSI),
		},
		expectMatch: true,
	}, {
		name: "burstable: below min cpu limit",
		allocatedResources: v1.ResourceRequirements{
			Requests: v1.ResourceList{
				v1.ResourceCPU: resource.MustParse("5m"),
			},
			Limits: v1.ResourceList{
				v1.ResourceCPU: resource.MustParse("5m"),
			},
		},
		statusResources: &kubecontainer.ContainerResources{
			CPURequest: resource.NewMilliQuantity(5, resource.DecimalSI),
			CPULimit:   resource.NewMilliQuantity(10, resource.DecimalSI),
		},
		expectMatch: true,
	}, {
		name:               "best effort",
		allocatedResources: v1.ResourceRequirements{},
		statusResources: &kubecontainer.ContainerResources{
			CPURequest: resource.NewMilliQuantity(2, resource.DecimalSI),
		},
		expectMatch: true,
	}, {
		name: "nil status resources: cpu request mismatch",
		allocatedResources: v1.ResourceRequirements{
			Requests: v1.ResourceList{
				v1.ResourceCPU: resource.MustParse("100m"),
			},
		},
		statusResources: &kubecontainer.ContainerResources{},
		expectMatch:     false,
	}, {
		name: "nil status resources: cpu limit mismatch",
		allocatedResources: v1.ResourceRequirements{
			Requests: v1.ResourceList{
				v1.ResourceCPU: resource.MustParse("100m"),
			},
			Limits: v1.ResourceList{
				v1.ResourceCPU: resource.MustParse("100m"),
			},
		},
		statusResources: &kubecontainer.ContainerResources{
			CPURequest: resource.NewMilliQuantity(2, resource.DecimalSI),
		},
		expectMatch: false,
	}, {
		name: "nil status resources: memory limit mismatch",
		allocatedResources: v1.ResourceRequirements{
			Limits: v1.ResourceList{
				v1.ResourceMemory: resource.MustParse("100M"),
			},
		},
		statusResources: &kubecontainer.ContainerResources{},
		expectMatch:     false,
	}}

	for _, test := range tests {
		for _, isSidecarContainer := range []bool{false, true} {
			if isSidecarContainer {
				test.name += " " + "for sidecar containers"
			}
			t.Run(test.name, func(t *testing.T) {
				var podStatus *kubecontainer.PodStatus
				state := kubecontainer.ContainerStateRunning
				if test.statusTerminated {
					state = kubecontainer.ContainerStateExited
				}

				allocatedPod := v1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name: "test",
					},
				}

				if isSidecarContainer {
					allocatedPod.Spec = v1.PodSpec{
						InitContainers: []v1.Container{{
							Name:          "c1-init",
							Resources:     test.allocatedResources,
							RestartPolicy: &containerRestartPolicyAlways,
						}},
					}
					podStatus = &kubecontainer.PodStatus{
						Name: "test",
						ContainerStatuses: []*kubecontainer.Status{
							{
								Name:      "c1-init",
								State:     state,
								Resources: test.statusResources,
							},
						},
					}
				} else {
					allocatedPod.Spec = v1.PodSpec{
						Containers: []v1.Container{{
							Name:      "c",
							Resources: test.allocatedResources,
						}},
					}
					podStatus = &kubecontainer.PodStatus{
						Name: "test",
						ContainerStatuses: []*kubecontainer.Status{
							{
								Name:      "c",
								State:     state,
								Resources: test.statusResources,
							},
						},
					}
				}
				match := allocatedResourcesMatchStatus(&allocatedPod, podStatus)
				assert.Equal(t, test.expectMatch, match)
			})
		}
	}
}
