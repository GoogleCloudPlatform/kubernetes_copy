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

package network

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/onsi/ginkgo/v2"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	utilnet "k8s.io/apimachinery/pkg/util/net"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/apimachinery/pkg/util/wait"
	clientset "k8s.io/client-go/kubernetes"
	coreclientset "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/kubernetes/test/e2e/framework"
	e2ekubectl "k8s.io/kubernetes/test/e2e/framework/kubectl"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	e2epodoutput "k8s.io/kubernetes/test/e2e/framework/pod/output"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"
	imageutils "k8s.io/kubernetes/test/utils/image"
	netutils "k8s.io/utils/net"
)

const (
	// EndpointHTTPPort is an endpoint HTTP port for testing.
	EndpointHTTPPort = 8083
	// EndpointUDPPort is an endpoint UDP port for testing.
	EndpointUDPPort = 8081
	// EndpointSCTPPort is an endpoint SCTP port for testing.
	EndpointSCTPPort = 8082
	// testContainerHTTPPort is the test container http port.
	testContainerHTTPPort = 9080
	// ClusterHTTPPort is a cluster HTTP port for testing.
	ClusterHTTPPort = 80
	// ClusterUDPPort is a cluster UDP port for testing.
	ClusterUDPPort = 90
	// ClusterSCTPPort is a cluster SCTP port for testing.
	ClusterSCTPPort            = 95
	testPodName                = "test-container-pod"
	hostTestPodName            = "host-test-container-pod"
	nodePortServiceName        = "node-port-service"
	sessionAffinityServiceName = "session-affinity-service"
	// wait time between poll attempts of a Service vip and/or nodePort.
	// coupled with testTries to produce a net timeout value.
	hitEndpointRetryDelay = 2 * time.Second
	// Number of retries to hit a given set of endpoints. Needs to be high
	// because we verify iptables statistical rr loadbalancing.
	testTries = 30
	// Maximum number of pods in a test, to make test work in large clusters.
	maxNetProxyPodsCount = 10
	// SessionAffinityChecks is number of checks to hit a given set of endpoints when enable session affinity.
	SessionAffinityChecks = 10
	// RegexIPv4 is a regex to match IPv4 addresses
	RegexIPv4 = "(?:\\d+)\\.(?:\\d+)\\.(?:\\d+)\\.(?:\\d+)"
	// RegexIPv6 is a regex to match IPv6 addresses
	RegexIPv6                 = "(?:(?:(?:(?:(?:(?:(?:[0-9a-fA-F]{1,4})):){6})(?:(?:(?:(?:(?:[0-9a-fA-F]{1,4})):(?:(?:[0-9a-fA-F]{1,4})))|(?:(?:(?:(?:(?:25[0-5]|(?:[1-9]|1[0-9]|2[0-4])?[0-9]))\\.){3}(?:(?:25[0-5]|(?:[1-9]|1[0-9]|2[0-4])?[0-9])))))))|(?:(?:::(?:(?:(?:[0-9a-fA-F]{1,4})):){5})(?:(?:(?:(?:(?:[0-9a-fA-F]{1,4})):(?:(?:[0-9a-fA-F]{1,4})))|(?:(?:(?:(?:(?:25[0-5]|(?:[1-9]|1[0-9]|2[0-4])?[0-9]))\\.){3}(?:(?:25[0-5]|(?:[1-9]|1[0-9]|2[0-4])?[0-9])))))))|(?:(?:(?:(?:(?:[0-9a-fA-F]{1,4})))?::(?:(?:(?:[0-9a-fA-F]{1,4})):){4})(?:(?:(?:(?:(?:[0-9a-fA-F]{1,4})):(?:(?:[0-9a-fA-F]{1,4})))|(?:(?:(?:(?:(?:25[0-5]|(?:[1-9]|1[0-9]|2[0-4])?[0-9]))\\.){3}(?:(?:25[0-5]|(?:[1-9]|1[0-9]|2[0-4])?[0-9])))))))|(?:(?:(?:(?:(?:(?:[0-9a-fA-F]{1,4})):){0,1}(?:(?:[0-9a-fA-F]{1,4})))?::(?:(?:(?:[0-9a-fA-F]{1,4})):){3})(?:(?:(?:(?:(?:[0-9a-fA-F]{1,4})):(?:(?:[0-9a-fA-F]{1,4})))|(?:(?:(?:(?:(?:25[0-5]|(?:[1-9]|1[0-9]|2[0-4])?[0-9]))\\.){3}(?:(?:25[0-5]|(?:[1-9]|1[0-9]|2[0-4])?[0-9])))))))|(?:(?:(?:(?:(?:(?:[0-9a-fA-F]{1,4})):){0,2}(?:(?:[0-9a-fA-F]{1,4})))?::(?:(?:(?:[0-9a-fA-F]{1,4})):){2})(?:(?:(?:(?:(?:[0-9a-fA-F]{1,4})):(?:(?:[0-9a-fA-F]{1,4})))|(?:(?:(?:(?:(?:25[0-5]|(?:[1-9]|1[0-9]|2[0-4])?[0-9]))\\.){3}(?:(?:25[0-5]|(?:[1-9]|1[0-9]|2[0-4])?[0-9])))))))|(?:(?:(?:(?:(?:(?:[0-9a-fA-F]{1,4})):){0,3}(?:(?:[0-9a-fA-F]{1,4})))?::(?:(?:[0-9a-fA-F]{1,4})):)(?:(?:(?:(?:(?:[0-9a-fA-F]{1,4})):(?:(?:[0-9a-fA-F]{1,4})))|(?:(?:(?:(?:(?:25[0-5]|(?:[1-9]|1[0-9]|2[0-4])?[0-9]))\\.){3}(?:(?:25[0-5]|(?:[1-9]|1[0-9]|2[0-4])?[0-9])))))))|(?:(?:(?:(?:(?:(?:[0-9a-fA-F]{1,4})):){0,4}(?:(?:[0-9a-fA-F]{1,4})))?::)(?:(?:(?:(?:(?:[0-9a-fA-F]{1,4})):(?:(?:[0-9a-fA-F]{1,4})))|(?:(?:(?:(?:(?:25[0-5]|(?:[1-9]|1[0-9]|2[0-4])?[0-9]))\\.){3}(?:(?:25[0-5]|(?:[1-9]|1[0-9]|2[0-4])?[0-9])))))))|(?:(?:(?:(?:(?:(?:[0-9a-fA-F]{1,4})):){0,5}(?:(?:[0-9a-fA-F]{1,4})))?::)(?:(?:[0-9a-fA-F]{1,4})))|(?:(?:(?:(?:(?:(?:[0-9a-fA-F]{1,4})):){0,6}(?:(?:[0-9a-fA-F]{1,4})))?::))))"
	resizeNodeReadyTimeout    = 2 * time.Minute
	resizeNodeNotReadyTimeout = 2 * time.Minute
	// netexec dial commands
	// the destination will echo its hostname.
	echoHostname = "hostname"
	// the destination will echo its node name.
	echoNodeName = "nodename"
)

// Option is used to configure the NetworkingTest object
type Option func(*NetworkingTestConfig)

// EnableSCTP listen on SCTP ports on the endpoints
func EnableSCTP(config *NetworkingTestConfig) {
	config.SCTPEnabled = true
}

// EnableDualStack create Dual Stack services
func EnableDualStack(config *NetworkingTestConfig) {
	config.DualStackEnabled = true
}

// UseHostNetwork run the test container with HostNetwork=true.
func UseHostNetwork(config *NetworkingTestConfig) {
	config.HostNetwork = true
}

// EchoNodeName will query the node name (rather than the hostname).
func EchoNodeName(config *NetworkingTestConfig) {
	config.EchoCommand = echoNodeName
}

// EndpointsUseHostNetwork run the endpoints pods with HostNetwork=true.
func EndpointsUseHostNetwork(config *NetworkingTestConfig) {
	config.EndpointsHostNetwork = true
}

// PreferExternalAddresses prefer node External Addresses for the tests
func PreferExternalAddresses(config *NetworkingTestConfig) {
	config.PreferExternalAddresses = true
}

// NewNetworkingTestConfig creates and sets up a new test config helper.
func NewNetworkingTestConfig(ctx context.Context, f *framework.Framework, setters ...Option) *NetworkingTestConfig {
	// default options
	config := &NetworkingTestConfig{
		f:           f,
		Namespace:   f.Namespace.Name,
		EchoCommand: echoHostname,
	}
	for _, setter := range setters {
		setter(config)
	}
	ginkgo.By(fmt.Sprintf("Performing setup for networking test in namespace %v", config.Namespace))
	config.setup(ctx, getServiceSelector())
	return config
}

// NewCoreNetworkingTestConfig creates and sets up a new test config helper for Node E2E.
func NewCoreNetworkingTestConfig(ctx context.Context, f *framework.Framework, hostNetwork bool) *NetworkingTestConfig {
	// default options
	config := &NetworkingTestConfig{
		f:           f,
		Namespace:   f.Namespace.Name,
		HostNetwork: hostNetwork,
	}
	ginkgo.By(fmt.Sprintf("Performing setup for networking test in namespace %v", config.Namespace))
	config.setupCore(ctx, getServiceSelector())
	return config
}

func getServiceSelector() map[string]string {
	ginkgo.By("creating a selector")
	selectorName := "selector-" + string(uuid.NewUUID())
	serviceSelector := map[string]string{
		selectorName: "true",
	}
	return serviceSelector
}

// NetworkingTestConfig is a convenience class around some utility methods
// for testing kubeproxy/networking/services/endpoints.
type NetworkingTestConfig struct {
	// TestContainerPod is a test pod running the netexec image. It is capable
	// of executing tcp/udp requests against ip:port.
	TestContainerPod *v1.Pod
	// HostTestContainerPod is a pod running using the hostexec image.
	HostTestContainerPod *v1.Pod
	// if the HostTestContainerPod is running with HostNetwork=true.
	HostNetwork bool
	// EchoCommand is the command to run (nodename to get the node name, hostname to get the hostname)
	EchoCommand string
	// if the endpoints Pods are running with HostNetwork=true.
	EndpointsHostNetwork bool
	// if the test pods are listening on sctp port. We need this as sctp tests
	// are marked as disruptive as they may load the sctp module.
	SCTPEnabled bool
	// DualStackEnabled enables dual stack on services
	DualStackEnabled bool
	// EndpointPods are the pods belonging to the Service created by this
	// test config. Each invocation of `setup` creates a service with
	// 1 pod per node running the netexecImage.
	EndpointPods []*v1.Pod
	f            *framework.Framework
	podClient    *e2epod.PodClient
	// NodePortService is a Service with Type=NodePort spanning over all
	// endpointPods.
	NodePortService *v1.Service
	// SessionAffinityService is a Service with SessionAffinity=ClientIP
	// spanning over all endpointPods.
	SessionAffinityService *v1.Service
	// Nodes is a list of nodes in the cluster.
	Nodes []v1.Node
	// MaxTries is the number of retries tolerated for tests run against
	// endpoints and services created by this config.
	MaxTries int
	// The ClusterIP of the Service created by this test config.
	ClusterIP string
	// The SecondaryClusterIP of the Service created by this test config.
	SecondaryClusterIP string
	// NodeIP it's an ExternalIP if the node has one,
	// or an InternalIP if not, for use in nodePort testing.
	NodeIP string
	// SecondaryNodeIP it's an ExternalIP of the secondary IP family if the node has one,
	// or an InternalIP if not, for usein nodePort testing.
	SecondaryNodeIP string
	// The http/udp/sctp nodePorts of the Service.
	NodeHTTPPort int
	NodeUDPPort  int
	NodeSCTPPort int
	// The kubernetes namespace within which all resources for this
	// config are created
	Namespace string
	// Whether to prefer node External Addresses for the tests
	PreferExternalAddresses bool
}

// NetexecDialResponse represents the response returned by the `netexec` subcommand of `agnhost`
type NetexecDialResponse struct {
	Responses []string `json:"responses"`
	Errors    []string `json:"errors"`
}

// DialFromEndpointContainer executes a curl via kubectl exec in an endpoint container.   Returns an error to be handled by the caller.
func (config *NetworkingTestConfig) DialFromEndpointContainer(ctx context.Context, protocol, targetIP string, targetPort, maxTries, minTries int, expectedEps sets.Set[string]) error {
	return config.DialFromContainer(ctx, protocol, config.EchoCommand, config.EndpointPods[0].Status.PodIP, targetIP, EndpointHTTPPort, targetPort, maxTries, minTries, expectedEps)
}

// DialFromTestContainer executes a curl via kubectl exec in a test container. Returns an error to be handled by the caller.
func (config *NetworkingTestConfig) DialFromTestContainer(ctx context.Context, protocol, targetIP string, targetPort, maxTries, minTries int, expectedEps sets.Set[string]) error {
	return config.DialFromContainer(ctx, protocol, config.EchoCommand, config.TestContainerPod.Status.PodIP, targetIP, testContainerHTTPPort, targetPort, maxTries, minTries, expectedEps)
}

// DialEchoFromTestContainer executes a curl via kubectl exec in a test container. The response is expected to match the echoMessage,  Returns an error to be handled by the caller.
func (config *NetworkingTestConfig) DialEchoFromTestContainer(ctx context.Context, protocol, targetIP string, targetPort, maxTries, minTries int, echoMessage string) error {
	expectedResponse := sets.New[string]()
	expectedResponse.Insert(echoMessage)
	var dialCommand string

	// NOTE(claudiub): netexec /dialCommand will send a request to the given targetIP and targetPort as follows:
	// for HTTP: it will send a request to: http://targetIP:targetPort/dialCommand
	// for UDP: it will send targetCommand as a message. The consumer receives the data message and looks for
	// a few starting strings, including echo, and treats it accordingly.
	if protocol == "http" {
		dialCommand = fmt.Sprintf("echo?msg=%s", echoMessage)
	} else {
		dialCommand = fmt.Sprintf("echo%%20%s", echoMessage)
	}
	return config.DialFromContainer(ctx, protocol, dialCommand, config.TestContainerPod.Status.PodIP, targetIP, testContainerHTTPPort, targetPort, maxTries, minTries, expectedResponse)
}

// diagnoseMissingEndpoints prints debug information about the endpoints that
// are NOT in the given list of foundEndpoints. These are the endpoints we
// expected a response from.
func (config *NetworkingTestConfig) diagnoseMissingEndpoints(foundEndpoints sets.Set[string]) {
	for _, e := range config.EndpointPods {
		if foundEndpoints.Has(e.Name) {
			continue
		}
		framework.Logf("\nOutput of kubectl describe pod %v/%v:\n", e.Namespace, e.Name)
		desc, _ := e2ekubectl.RunKubectl(
			e.Namespace, "describe", "pod", e.Name, fmt.Sprintf("--namespace=%v", e.Namespace))
		framework.Logf("%s", desc)
	}
}

// EndpointHostnames returns a set of hostnames for existing endpoints.
func (config *NetworkingTestConfig) EndpointHostnames() sets.Set[string] {
	expectedEps := sets.New[string]()
	for _, p := range config.EndpointPods {

		if config.EndpointsHostNetwork {
			// Hostname behavior for hostNetwork pods is not well defined and when
			// using the flag hostname-override in the kubelet, the node reported
			// hostname on host network pods will not match the node's hostanme.
			// It seems that the node.status.addresses hostname value is the only
			// one that matches the value returned by os.Hostname
			// used by the agnhost web handler, so we'll use that value.
			// If by any circumstances the node does not provide that hostnae address
			// we use the value of the node name.
			// xref: https://issues.k8s.io/126087
			hostname := p.Spec.NodeSelector["kubernetes.io/hostname"]
			for _, n := range config.Nodes {
				if n.Name == p.Spec.NodeSelector["kubernetes.io/hostname"] {
					for _, address := range n.Status.Addresses {
						if address.Type == v1.NodeHostName {
							hostname = address.Address
							break
						}
					}
					break
				}
			}
			expectedEps.Insert(hostname)
		} else {
			expectedEps.Insert(p.Name)
		}
	}
	return expectedEps
}

// EndpointNodeNames returns a set of nodeNames for existing endpoints.
func (config *NetworkingTestConfig) EndpointNodeNames() sets.Set[string] {
	expectedEps := sets.New[string]()
	for _, endpointPod := range config.EndpointPods {
		expectedEps.Insert(endpointPod.Spec.NodeName)
	}
	return expectedEps
}

func makeCURLDialCommand(ipPort, dialCmd, protocol, targetIP string, targetPort int) string {
	// The current versions of curl included in CentOS and RHEL distros
	// misinterpret square brackets around IPv6 as globbing, so use the -g
	// argument to disable globbing to handle the IPv6 case.
	return fmt.Sprintf("curl -g -q -s 'http://%s/dial?request=%s&protocol=%s&host=%s&port=%d&tries=1'",
		ipPort,
		dialCmd,
		protocol,
		targetIP,
		targetPort)
}

// DialFromContainer executes a curl via kubectl exec in a test container,
// which might then translate to a tcp or udp request based on the protocol
// argument in the url.
//   - minTries is the minimum number of curl attempts required before declaring
//     success. Set to 0 if you'd like to return as soon as all endpoints respond
//     at least once.
//   - maxTries is the maximum number of curl attempts. If this many attempts pass
//     and we don't see all expected endpoints, the test fails.
//   - targetIP is the source Pod IP that will dial the given dialCommand using the given protocol.
//   - dialCommand is the command that the targetIP will send to the targetIP using the given protocol.
//     the dialCommand should be formatted properly for the protocol (http: URL path+parameters,
//     udp: command%20parameters, where parameters are optional)
//   - expectedResponses is the unordered set of responses to wait for. The responses are based on
//     the dialCommand; for example, for the dialCommand "hostname", the expectedResponses
//     should contain the hostnames reported by each pod in the service through /hostName.
//
// maxTries == minTries will confirm that we see the expected endpoints and no
// more for maxTries. Use this if you want to eg: fail a readiness check on a
// pod and confirm it doesn't show up as an endpoint.
// Returns nil if no error, or error message if failed after trying maxTries.
func (config *NetworkingTestConfig) DialFromContainer(ctx context.Context, protocol, dialCommand, containerIP, targetIP string, containerHTTPPort, targetPort, maxTries, minTries int, expectedResponses sets.Set[string]) error {
	ipPort := net.JoinHostPort(containerIP, strconv.Itoa(containerHTTPPort))
	cmd := makeCURLDialCommand(ipPort, dialCommand, protocol, targetIP, targetPort)

	responses := sets.New[string]()

	for i := 0; i < maxTries; i++ {
		resp, err := config.GetResponseFromContainer(ctx, protocol, dialCommand, containerIP, targetIP, containerHTTPPort, targetPort)
		if err != nil {
			// A failure to kubectl exec counts as a try, not a hard fail.
			// Also note that we will keep failing for maxTries in tests where
			// we confirm unreachability.
			framework.Logf("GetResponseFromContainer: %s", err)
			continue
		}
		for _, response := range resp.Responses {
			trimmed := strings.TrimSpace(response)
			if trimmed != "" {
				responses.Insert(trimmed)
			}
		}
		if responses.Difference(expectedResponses).Len() > 0 {
			returnMsg := fmt.Errorf("received unexpected responses... \nAttempt %d\nCommand %v\nretrieved %v\nexpected %v", i, cmd, responses, expectedResponses)
			framework.Logf("encountered error during dial (%v)", returnMsg)
			return returnMsg
		}

		framework.Logf("Waiting for responses: %v", expectedResponses.Difference(responses))

		// Check against i+1 so we exit if minTries == maxTries.
		if (responses.Equal(expectedResponses) || responses.Len() == 0 && expectedResponses.Len() == 0) && i+1 >= minTries {
			framework.Logf("reached %v after %v/%v tries", targetIP, i, maxTries)
			return nil
		}
		// TODO: get rid of this delay #36281
		time.Sleep(hitEndpointRetryDelay)
	}
	if dialCommand == echoHostname || dialCommand == echoNodeName {
		config.diagnoseMissingEndpoints(responses)
	}
	returnMsg := fmt.Errorf("did not find expected responses... \nTries %d\nCommand %v\nretrieved %v\nexpected %v", maxTries, cmd, responses, expectedResponses)
	framework.Logf("encountered error during dial (%v)", returnMsg)
	return returnMsg

}

// GetEndpointsFromTestContainer executes a curl via kubectl exec in a test container.
func (config *NetworkingTestConfig) GetEndpointsFromTestContainer(ctx context.Context, protocol, targetIP string, targetPort, tries int) (sets.String, error) {
	return config.GetEndpointsFromContainer(ctx, protocol, config.TestContainerPod.Status.PodIP, targetIP, testContainerHTTPPort, targetPort, tries)
}

// GetEndpointsFromContainer executes a curl via kubectl exec in a test container,
// which might then translate to a tcp or udp request based on the protocol argument
// in the url. It returns all different endpoints from multiple retries.
//   - tries is the number of curl attempts. If this many attempts pass and
//     we don't see any endpoints, the test fails.
func (config *NetworkingTestConfig) GetEndpointsFromContainer(ctx context.Context, protocol, containerIP, targetIP string, containerHTTPPort, targetPort, tries int) (sets.String, error) {
	ipPort := net.JoinHostPort(containerIP, strconv.Itoa(containerHTTPPort))
	cmd := makeCURLDialCommand(ipPort, "hostName", protocol, targetIP, targetPort)

	eps := sets.NewString()

	for i := 0; i < tries; i++ {
		stdout, stderr, err := e2epod.ExecShellInPodWithFullOutput(ctx, config.f, config.TestContainerPod.Name, cmd)
		if err != nil {
			// A failure to kubectl exec counts as a try, not a hard fail.
			// Also note that we will keep failing for maxTries in tests where
			// we confirm unreachability.
			framework.Logf("Failed to execute %q: %v, stdout: %q, stderr: %q", cmd, err, stdout, stderr)
		} else {
			podInfo := fmt.Sprintf("name: %v, namespace: %v, hostIp: %v, podIp: %v, conditions: %v", config.TestContainerPod.Name, config.TestContainerPod.Namespace, config.TestContainerPod.Status.HostIP, config.TestContainerPod.Status.PodIP, config.TestContainerPod.Status.Conditions)
			framework.Logf("Tries: %d, in try: %d, stdout: %v, stderr: %v, command run in Pod { %#v }", tries, i, stdout, stderr, podInfo)

			var output NetexecDialResponse
			if err := json.Unmarshal([]byte(stdout), &output); err != nil {
				framework.Logf("WARNING: Failed to unmarshal curl response. Cmd %v run in %v, output: %s, err: %v",
					cmd, config.TestContainerPod.Name, stdout, err)
				continue
			}

			for _, hostName := range output.Responses {
				trimmed := strings.TrimSpace(hostName)
				if trimmed != "" {
					eps.Insert(trimmed)
				}
			}
			// TODO: get rid of this delay #36281
			time.Sleep(hitEndpointRetryDelay)
		}
	}
	return eps, nil
}

// GetResponseFromContainer executes a curl via kubectl exec in a container.
func (config *NetworkingTestConfig) GetResponseFromContainer(ctx context.Context, protocol, dialCommand, containerIP, targetIP string, containerHTTPPort, targetPort int) (NetexecDialResponse, error) {
	ipPort := net.JoinHostPort(containerIP, strconv.Itoa(containerHTTPPort))
	cmd := makeCURLDialCommand(ipPort, dialCommand, protocol, targetIP, targetPort)

	stdout, stderr, err := e2epod.ExecShellInPodWithFullOutput(ctx, config.f, config.TestContainerPod.Name, cmd)
	if err != nil {
		return NetexecDialResponse{}, fmt.Errorf("failed to execute %q: %v, stdout: %q, stderr: %q", cmd, err, stdout, stderr)
	}

	var output NetexecDialResponse
	if err := json.Unmarshal([]byte(stdout), &output); err != nil {
		return NetexecDialResponse{}, fmt.Errorf("failed to unmarshal curl response. Cmd %v run in %v, output: %s, err: %v",
			cmd, config.TestContainerPod.Name, stdout, err)
	}
	return output, nil
}

// GetResponseFromTestContainer executes a curl via kubectl exec in a test container.
func (config *NetworkingTestConfig) GetResponseFromTestContainer(ctx context.Context, protocol, dialCommand, targetIP string, targetPort int) (NetexecDialResponse, error) {
	return config.GetResponseFromContainer(ctx, protocol, dialCommand, config.TestContainerPod.Status.PodIP, targetIP, testContainerHTTPPort, targetPort)
}

// GetHTTPCodeFromTestContainer executes a curl via kubectl exec in a test container and returns the status code.
func (config *NetworkingTestConfig) GetHTTPCodeFromTestContainer(ctx context.Context, path, targetIP string, targetPort int) (int, error) {
	cmd := fmt.Sprintf("curl -g -q -s -o /dev/null -w %%{http_code} http://%s%s",
		net.JoinHostPort(targetIP, strconv.Itoa(targetPort)),
		path)
	stdout, stderr, err := e2epod.ExecShellInPodWithFullOutput(ctx, config.f, config.TestContainerPod.Name, cmd)
	// We only care about the status code reported by curl,
	// and want to return any other errors, such as cannot execute command in the Pod.
	// If curl failed to connect to host, it would exit with code 7, which makes `ExecShellInPodWithFullOutput`
	// return a non-nil error and output "000" to stdout.
	if err != nil && len(stdout) == 0 {
		return 0, fmt.Errorf("failed to execute %q: %v, stderr: %q", cmd, err, stderr)
	}
	code, err := strconv.Atoi(stdout)
	if err != nil {
		return 0, fmt.Errorf("failed to parse status code returned by healthz endpoint: %w, code: %s", err, stdout)
	}
	return code, nil
}

// DialFromNode executes a tcp/udp curl/nc request based on protocol via kubectl exec
// in a test container running with host networking.
//   - minTries is the minimum number of curl/nc attempts required before declaring
//     success. If 0, then we return as soon as all endpoints succeed.
//   - There is no logical change to test results if faillures happen AFTER endpoints have succeeded,
//     hence over-padding minTries will NOT reverse a successful result and is thus not very useful yet
//     (See the TODO about checking probability, which isn't implemented yet).
//   - maxTries is the maximum number of curl/echo attempts before an error is returned.  The
//     smaller this number is, the less 'slack' there is for declaring success.
//   - if maxTries < expectedEps, this test is guaranteed to return an error, because all endpoints won't be hit.
//   - maxTries == minTries will return as soon as all endpoints succeed (or fail once maxTries is reached without
//     success on all endpoints).
//     In general its prudent to have a high enough level of minTries to guarantee that all pods get a fair chance at receiving traffic.
func (config *NetworkingTestConfig) DialFromNode(ctx context.Context, protocol, targetIP string, targetPort, maxTries, minTries int, expectedEps sets.Set[string]) error {
	var cmd string
	if protocol == "udp" {
		cmd = fmt.Sprintf("echo hostName | nc -w 1 -u %s %d", targetIP, targetPort)
	} else {
		ipPort := net.JoinHostPort(targetIP, strconv.Itoa(targetPort))
		// The current versions of curl included in CentOS and RHEL distros
		// misinterpret square brackets around IPv6 as globbing, so use the -g
		// argument to disable globbing to handle the IPv6 case.
		cmd = fmt.Sprintf("curl -g -q -s --max-time 15 --connect-timeout 1 http://%s/hostName", ipPort)
	}

	// TODO: This simply tells us that we can reach the endpoints. Check that
	// the probability of hitting a specific endpoint is roughly the same as
	// hitting any other.
	eps := sets.New[string]()

	filterCmd := fmt.Sprintf("%s | grep -v '^\\s*$'", cmd)
	framework.Logf("Going to poll %v on port %v at least %v times, with a maximum of %v tries before failing", targetIP, targetPort, minTries, maxTries)
	for i := 0; i < maxTries; i++ {
		stdout, stderr, err := e2epod.ExecShellInPodWithFullOutput(ctx, config.f, config.HostTestContainerPod.Name, filterCmd)
		if err != nil || len(stderr) > 0 {
			// A failure to exec command counts as a try, not a hard fail.
			// Also note that we will keep failing for maxTries in tests where
			// we confirm unreachability.
			framework.Logf("Failed to execute %q: %v, stdout: %q, stderr: %q", filterCmd, err, stdout, stderr)
		} else {
			trimmed := strings.TrimSpace(stdout)
			if trimmed != "" {
				eps.Insert(trimmed)
			}
		}

		// Check against i+1 so we exit if minTries == maxTries.
		if eps.Equal(expectedEps) && i+1 >= minTries {
			framework.Logf("Found all %d expected endpoints: %+v", eps.Len(), sets.List(eps))
			return nil
		}

		framework.Logf("Waiting for %+v endpoints (expected=%+v, actual=%+v)", sets.List(expectedEps.Difference(eps)), sets.List(expectedEps), sets.List(eps))

		// TODO: get rid of this delay #36281
		time.Sleep(hitEndpointRetryDelay)
	}

	config.diagnoseMissingEndpoints(eps)
	return fmt.Errorf("failed to find expected endpoints, \ntries %d\nCommand %v\nretrieved %v\nexpected %v", maxTries, cmd, eps, expectedEps)
}

// GetSelfURL executes a curl against the given path via kubectl exec into a
// test container running with host networking, and fails if the output
// doesn't match the expected string.
func (config *NetworkingTestConfig) GetSelfURL(ctx context.Context, port int32, path string, expected string) {
	cmd := fmt.Sprintf("curl -i -q -s --connect-timeout 1 http://localhost:%d%s", port, path)
	ginkgo.By(fmt.Sprintf("Getting kube-proxy self URL %s", path))
	config.executeCurlCmd(ctx, cmd, expected)
}

// GetSelfURLStatusCode executes a curl against the given path via kubectl exec into a
// test container running with host networking, and fails if the returned status
// code doesn't match the expected string.
func (config *NetworkingTestConfig) GetSelfURLStatusCode(ctx context.Context, port int32, path string, expected string) {
	// check status code
	cmd := fmt.Sprintf("curl -o /dev/null -i -q -s -w %%{http_code} --connect-timeout 1 http://localhost:%d%s", port, path)
	ginkgo.By(fmt.Sprintf("Checking status code against http://localhost:%d%s", port, path))
	config.executeCurlCmd(ctx, cmd, expected)
}

func (config *NetworkingTestConfig) executeCurlCmd(ctx context.Context, cmd string, expected string) {
	// These are arbitrary timeouts. The curl command should pass on first try,
	// unless remote server is starved/bootstrapping/restarting etc.
	const retryInterval = 1 * time.Second
	const retryTimeout = 30 * time.Second
	podName := config.HostTestContainerPod.Name
	var msg string
	if pollErr := wait.PollUntilContextTimeout(ctx, retryInterval, retryTimeout, true, func(ctx context.Context) (bool, error) {
		stdout, err := e2epodoutput.RunHostCmd(config.Namespace, podName, cmd)
		if err != nil {
			msg = fmt.Sprintf("failed executing cmd %v in %v/%v: %v", cmd, config.Namespace, podName, err)
			framework.Logf("%s", msg)
			return false, nil
		}
		if !strings.Contains(stdout, expected) {
			msg = fmt.Sprintf("successfully executed %v in %v/%v, but output '%v' doesn't contain expected string '%v'", cmd, config.Namespace, podName, stdout, expected)
			framework.Logf("%s", msg)
			return false, nil
		}
		return true, nil
	}); pollErr != nil {
		framework.Logf("\nOutput of kubectl describe pod %v/%v:\n", config.Namespace, podName)
		desc, _ := e2ekubectl.RunKubectl(
			config.Namespace, "describe", "pod", podName, fmt.Sprintf("--namespace=%v", config.Namespace))
		framework.Logf("%s", desc)
		framework.Failf("Timed out in %v: %v", retryTimeout, msg)
	}
}

func (config *NetworkingTestConfig) createNetShellPodSpec(podName, hostname string) *v1.Pod {
	netexecArgs := []string{
		"netexec",
		fmt.Sprintf("--http-port=%d", EndpointHTTPPort),
		fmt.Sprintf("--udp-port=%d", EndpointUDPPort),
	}
	// In case of hostnetwork endpoints, we want to bind the udp listener to specific ip addresses.
	// In order to cover legacy AND dualstack, we pass both the host ip and the two pod ips. Agnhost
	// removes duplicates and so this will listen on both addresses (or on the single existing one).
	if config.EndpointsHostNetwork {
		netexecArgs = append(netexecArgs, "--udp-listen-addresses=$(HOST_IP),$(POD_IPS)")
	}

	probe := &v1.Probe{
		InitialDelaySeconds: 10,
		TimeoutSeconds:      30,
		PeriodSeconds:       10,
		SuccessThreshold:    1,
		FailureThreshold:    3,
		ProbeHandler: v1.ProbeHandler{
			HTTPGet: &v1.HTTPGetAction{
				Path: "/healthz",
				Port: intstr.IntOrString{IntVal: EndpointHTTPPort},
			},
		},
	}
	pod := &v1.Pod{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Pod",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: config.Namespace,
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Name:            "webserver",
					Image:           imageutils.GetE2EImage(imageutils.Agnhost),
					ImagePullPolicy: v1.PullIfNotPresent,
					Args:            netexecArgs,
					Ports: []v1.ContainerPort{
						{
							Name:          "http",
							ContainerPort: EndpointHTTPPort,
						},
						{
							Name:          "udp",
							ContainerPort: EndpointUDPPort,
							Protocol:      v1.ProtocolUDP,
						},
					},
					LivenessProbe:  probe,
					ReadinessProbe: probe,
				},
			},
			NodeSelector: map[string]string{
				"kubernetes.io/hostname": hostname,
			},
		},
	}
	// we want sctp to be optional as it will load the sctp kernel module
	if config.SCTPEnabled {
		pod.Spec.Containers[0].Args = append(pod.Spec.Containers[0].Args, fmt.Sprintf("--sctp-port=%d", EndpointSCTPPort))
		pod.Spec.Containers[0].Ports = append(pod.Spec.Containers[0].Ports, v1.ContainerPort{
			Name:          "sctp",
			ContainerPort: EndpointSCTPPort,
			Protocol:      v1.ProtocolSCTP,
		})
	}

	if config.EndpointsHostNetwork {
		pod.Spec.Containers[0].Env = []v1.EnvVar{
			{
				Name: "HOST_IP",
				ValueFrom: &v1.EnvVarSource{
					FieldRef: &v1.ObjectFieldSelector{
						FieldPath: "status.hostIP",
					},
				},
			},
			{
				Name: "POD_IPS",
				ValueFrom: &v1.EnvVarSource{
					FieldRef: &v1.ObjectFieldSelector{
						FieldPath: "status.podIPs",
					},
				},
			},
		}
	}
	return pod
}

func (config *NetworkingTestConfig) createTestPodSpec() *v1.Pod {
	pod := &v1.Pod{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Pod",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      testPodName,
			Namespace: config.Namespace,
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Name:            "webserver",
					Image:           imageutils.GetE2EImage(imageutils.Agnhost),
					ImagePullPolicy: v1.PullIfNotPresent,
					Args: []string{
						"netexec",
						fmt.Sprintf("--http-port=%d", testContainerHTTPPort),
					},
					Env: []v1.EnvVar{
						{
							Name: "NODE_NAME",
							ValueFrom: &v1.EnvVarSource{
								FieldRef: &v1.ObjectFieldSelector{FieldPath: "spec.nodeName"},
							},
						},
					},
					Ports: []v1.ContainerPort{
						{
							Name:          "http",
							ContainerPort: testContainerHTTPPort,
						},
					},
				},
			},
		},
	}
	return pod
}

func (config *NetworkingTestConfig) createNodePortServiceSpec(svcName string, selector map[string]string, enableSessionAffinity bool) *v1.Service {
	sessionAffinity := v1.ServiceAffinityNone
	if enableSessionAffinity {
		sessionAffinity = v1.ServiceAffinityClientIP
	}
	res := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: svcName,
		},
		Spec: v1.ServiceSpec{
			Type: v1.ServiceTypeNodePort,
			Ports: []v1.ServicePort{
				{Port: ClusterHTTPPort, Name: "http", Protocol: v1.ProtocolTCP, TargetPort: intstr.FromInt32(EndpointHTTPPort)},
				{Port: ClusterUDPPort, Name: "udp", Protocol: v1.ProtocolUDP, TargetPort: intstr.FromInt32(EndpointUDPPort)},
			},
			Selector:        selector,
			SessionAffinity: sessionAffinity,
		},
	}

	if config.SCTPEnabled {
		res.Spec.Ports = append(res.Spec.Ports, v1.ServicePort{Port: ClusterSCTPPort, Name: "sctp", Protocol: v1.ProtocolSCTP, TargetPort: intstr.FromInt32(EndpointSCTPPort)})
	}
	if config.DualStackEnabled {
		requireDual := v1.IPFamilyPolicyRequireDualStack
		res.Spec.IPFamilyPolicy = &requireDual
	}
	return res
}

func (config *NetworkingTestConfig) createNodePortService(ctx context.Context, selector map[string]string) {
	config.NodePortService = config.CreateService(ctx, config.createNodePortServiceSpec(nodePortServiceName, selector, false))
}

func (config *NetworkingTestConfig) createSessionAffinityService(ctx context.Context, selector map[string]string) {
	config.SessionAffinityService = config.CreateService(ctx, config.createNodePortServiceSpec(sessionAffinityServiceName, selector, true))
}

// DeleteNodePortService deletes NodePort service.
func (config *NetworkingTestConfig) DeleteNodePortService(ctx context.Context) {
	err := config.getServiceClient().Delete(ctx, config.NodePortService.Name, metav1.DeleteOptions{})
	framework.ExpectNoError(err, "error while deleting NodePortService. err:%v)", err)
	time.Sleep(15 * time.Second) // wait for kube-proxy to catch up with the service being deleted.
}

func (config *NetworkingTestConfig) createTestPods(ctx context.Context) {
	testContainerPod := config.createTestPodSpec()
	hostTestContainerPod := e2epod.NewExecPodSpec(config.Namespace, hostTestPodName, config.HostNetwork)

	config.createPod(ctx, testContainerPod)
	if config.HostNetwork {
		config.createPod(ctx, hostTestContainerPod)
	}

	framework.ExpectNoError(e2epod.WaitForPodNameRunningInNamespace(ctx, config.f.ClientSet, testContainerPod.Name, config.f.Namespace.Name))

	var err error
	config.TestContainerPod, err = config.getPodClient().Get(ctx, testContainerPod.Name, metav1.GetOptions{})
	if err != nil {
		framework.Failf("Failed to retrieve %s pod: %v", testContainerPod.Name, err)
	}

	if config.HostNetwork {
		framework.ExpectNoError(e2epod.WaitForPodNameRunningInNamespace(ctx, config.f.ClientSet, hostTestContainerPod.Name, config.f.Namespace.Name))
		config.HostTestContainerPod, err = config.getPodClient().Get(ctx, hostTestContainerPod.Name, metav1.GetOptions{})
		if err != nil {
			framework.Failf("Failed to retrieve %s pod: %v", hostTestContainerPod.Name, err)
		}
	}
}

// CreateService creates the provided service in config.Namespace and returns created service
func (config *NetworkingTestConfig) CreateService(ctx context.Context, serviceSpec *v1.Service) *v1.Service {
	_, err := config.getServiceClient().Create(ctx, serviceSpec, metav1.CreateOptions{})
	framework.ExpectNoError(err, fmt.Sprintf("Failed to create %s service: %v", serviceSpec.Name, err))

	err = WaitForService(ctx, config.f.ClientSet, config.Namespace, serviceSpec.Name, true, 5*time.Second, 45*time.Second)
	framework.ExpectNoError(err, fmt.Sprintf("error while waiting for service:%s err: %v", serviceSpec.Name, err))

	createdService, err := config.getServiceClient().Get(ctx, serviceSpec.Name, metav1.GetOptions{})
	framework.ExpectNoError(err, fmt.Sprintf("Failed to create %s service: %v", serviceSpec.Name, err))

	return createdService
}

// setupCore sets up the pods and core test config
// mainly for simplified node e2e setup
func (config *NetworkingTestConfig) setupCore(ctx context.Context, selector map[string]string) {
	ginkgo.By("Creating the service pods in kubernetes")
	podName := "netserver"
	config.EndpointPods = config.createNetProxyPods(ctx, podName, selector)

	ginkgo.By("Creating test pods")
	config.createTestPods(ctx)

	epCount := len(config.EndpointPods)

	// Note that this is not O(n^2) in practice, because epCount SHOULD be < 10.  In cases that epCount is > 10, this would be prohibitively large.
	// Check maxNetProxyPodsCount for details.
	config.MaxTries = epCount*epCount + testTries
	framework.Logf("Setting MaxTries for pod polling to %v for networking test based on endpoint count %v", config.MaxTries, epCount)
}

// setup includes setupCore and also sets up services
func (config *NetworkingTestConfig) setup(ctx context.Context, selector map[string]string) {
	config.setupCore(ctx, selector)

	ginkgo.By("Getting node addresses")
	framework.ExpectNoError(e2enode.WaitForAllNodesSchedulable(ctx, config.f.ClientSet, 10*time.Minute))
	nodeList, err := e2enode.GetReadySchedulableNodes(ctx, config.f.ClientSet)
	framework.ExpectNoError(err)

	e2eskipper.SkipUnlessNodeCountIsAtLeast(2)
	config.Nodes = nodeList.Items

	ginkgo.By("Creating the service on top of the pods in kubernetes")
	config.createNodePortService(ctx, selector)
	config.createSessionAffinityService(ctx, selector)

	for _, p := range config.NodePortService.Spec.Ports {
		switch p.Protocol {
		case v1.ProtocolUDP:
			config.NodeUDPPort = int(p.NodePort)
		case v1.ProtocolTCP:
			config.NodeHTTPPort = int(p.NodePort)
		case v1.ProtocolSCTP:
			config.NodeSCTPPort = int(p.NodePort)
		default:
			continue
		}
	}

	// obtain the ClusterIP
	config.ClusterIP = config.NodePortService.Spec.ClusterIP
	if config.DualStackEnabled {
		config.SecondaryClusterIP = config.NodePortService.Spec.ClusterIPs[1]
	}

	// Obtain the primary IP family of the Cluster based on the first ClusterIP
	// TODO: Eventually we should just be getting these from Spec.IPFamilies
	// but for now that would only if the feature gate is enabled.
	family := v1.IPv4Protocol
	secondaryFamily := v1.IPv6Protocol
	if netutils.IsIPv6String(config.ClusterIP) {
		family = v1.IPv6Protocol
		secondaryFamily = v1.IPv4Protocol
	}
	if config.PreferExternalAddresses {
		// Get Node IPs from the cluster, ExternalIPs take precedence
		config.NodeIP = e2enode.FirstAddressByTypeAndFamily(nodeList, v1.NodeExternalIP, family)
	}
	if config.NodeIP == "" {
		config.NodeIP = e2enode.FirstAddressByTypeAndFamily(nodeList, v1.NodeInternalIP, family)
	}
	if config.DualStackEnabled {
		if config.PreferExternalAddresses {
			config.SecondaryNodeIP = e2enode.FirstAddressByTypeAndFamily(nodeList, v1.NodeExternalIP, secondaryFamily)
		}
		if config.SecondaryNodeIP == "" {
			config.SecondaryNodeIP = e2enode.FirstAddressByTypeAndFamily(nodeList, v1.NodeInternalIP, secondaryFamily)
		}
	}

	ginkgo.By("Waiting for NodePort service to expose endpoint")
	numEndpoints := len(config.EndpointPods)
	if config.DualStackEnabled {
		numEndpoints = 2 * len(config.EndpointPods)
	}
	err = framework.WaitForServiceEndpointsNum(ctx, config.f.ClientSet, config.Namespace, nodePortServiceName, numEndpoints, time.Second, wait.ForeverTestTimeout)
	framework.ExpectNoError(err, "failed to validate endpoints for service %s in namespace: %s", nodePortServiceName, config.Namespace)
	ginkgo.By("Waiting for Session Affinity service to expose endpoint")
	err = framework.WaitForServiceEndpointsNum(ctx, config.f.ClientSet, config.Namespace, sessionAffinityServiceName, numEndpoints, time.Second, wait.ForeverTestTimeout)
	framework.ExpectNoError(err, "failed to validate endpoints for service %s in namespace: %s", sessionAffinityServiceName, config.Namespace)
}

func (config *NetworkingTestConfig) createNetProxyPods(ctx context.Context, podName string, selector map[string]string) []*v1.Pod {
	framework.ExpectNoError(e2enode.WaitForAllNodesSchedulable(ctx, config.f.ClientSet, 10*time.Minute))
	nodeList, err := e2enode.GetBoundedReadySchedulableNodes(ctx, config.f.ClientSet, maxNetProxyPodsCount)
	framework.ExpectNoError(err)
	nodes := nodeList.Items

	// create pods, one for each node
	createdPods := make([]*v1.Pod, 0, len(nodes))
	for i, n := range nodes {
		podName := fmt.Sprintf("%s-%d", podName, i)
		hostname, _ := n.Labels["kubernetes.io/hostname"]
		pod := config.createNetShellPodSpec(podName, hostname)
		pod.ObjectMeta.Labels = selector
		pod.Spec.HostNetwork = config.EndpointsHostNetwork

		// NOTE(claudiub): In order to use HostNetwork on Windows, we need to use Privileged Containers.
		if pod.Spec.HostNetwork && framework.NodeOSDistroIs("windows") {
			e2epod.WithWindowsHostProcess(pod, "")
		}
		createdPod := config.createPod(ctx, pod)
		createdPods = append(createdPods, createdPod)
	}

	// wait that all of them are up
	runningPods := make([]*v1.Pod, 0, len(nodes))
	for _, p := range createdPods {
		framework.ExpectNoError(e2epod.WaitTimeoutForPodReadyInNamespace(ctx, config.f.ClientSet, p.Name, config.f.Namespace.Name, framework.PodStartTimeout))
		rp, err := config.getPodClient().Get(ctx, p.Name, metav1.GetOptions{})
		framework.ExpectNoError(err)
		runningPods = append(runningPods, rp)
	}

	return runningPods
}

// DeleteNetProxyPod deletes the first endpoint pod and waits for it being removed.
func (config *NetworkingTestConfig) DeleteNetProxyPod(ctx context.Context) {
	pod := config.EndpointPods[0]
	framework.ExpectNoError(config.getPodClient().Delete(ctx, pod.Name, metav1.DeleteOptions{}))
	config.EndpointPods = config.EndpointPods[1:]
	// wait for pod being deleted.
	err := e2epod.WaitForPodNotFoundInNamespace(ctx, config.f.ClientSet, pod.Name, config.Namespace, config.f.Timeouts.PodDelete)
	if err != nil {
		framework.Failf("Failed to delete %s pod: %v", pod.Name, err)
	}
	// wait for endpoint being removed.
	numEndpoints := len(config.EndpointPods)
	if config.DualStackEnabled {
		numEndpoints = 2 * len(config.EndpointPods)
	}
	err = framework.WaitForServiceEndpointsNum(ctx, config.f.ClientSet, config.Namespace, nodePortServiceName, numEndpoints, time.Second, wait.ForeverTestTimeout)
	if err != nil {
		framework.Failf("Failed to remove endpoint from service: %s", nodePortServiceName)
	}
	// wait for kube-proxy to catch up with the pod being deleted.
	time.Sleep(5 * time.Second)
}

func (config *NetworkingTestConfig) createPod(ctx context.Context, pod *v1.Pod) *v1.Pod {
	return config.getPodClient().Create(ctx, pod)
}

func (config *NetworkingTestConfig) getPodClient() *e2epod.PodClient {
	if config.podClient == nil {
		config.podClient = e2epod.NewPodClient(config.f)
	}
	return config.podClient
}

func (config *NetworkingTestConfig) getServiceClient() coreclientset.ServiceInterface {
	return config.f.ClientSet.CoreV1().Services(config.Namespace)
}

// HTTPPokeParams is a struct for HTTP poke parameters.
type HTTPPokeParams struct {
	Timeout        time.Duration // default = 10 secs
	ExpectCode     int           // default = 200
	BodyContains   string
	RetriableCodes []int
	EnableHTTPS    bool
}

// HTTPPokeResult is a struct for HTTP poke result.
type HTTPPokeResult struct {
	Status HTTPPokeStatus
	Code   int    // HTTP code: 0 if the connection was not made
	Error  error  // if there was any error
	Body   []byte // if code != 0
}

// HTTPPokeStatus is string for representing HTTP poke status.
type HTTPPokeStatus string

const (
	// HTTPSuccess is HTTP poke status which is success.
	HTTPSuccess HTTPPokeStatus = "Success"
	// HTTPError is HTTP poke status which is error.
	HTTPError HTTPPokeStatus = "UnknownError"
	// HTTPTimeout is HTTP poke status which is timeout.
	HTTPTimeout HTTPPokeStatus = "TimedOut"
	// HTTPRefused is HTTP poke status which is connection refused.
	HTTPRefused HTTPPokeStatus = "ConnectionRefused"
	// HTTPRetryCode is HTTP poke status which is retry code.
	HTTPRetryCode HTTPPokeStatus = "RetryCode"
	// HTTPWrongCode is HTTP poke status which is wrong code.
	HTTPWrongCode HTTPPokeStatus = "WrongCode"
	// HTTPBadResponse is HTTP poke status which is bad response.
	HTTPBadResponse HTTPPokeStatus = "BadResponse"
	// Any time we add new errors, we should audit all callers of this.
)

// PokeHTTP tries to connect to a host on a port for a given URL path.  Callers
// can specify additional success parameters, if desired.
//
// The result status will be characterized as precisely as possible, given the
// known users of this.
//
// The result code will be zero in case of any failure to connect, or non-zero
// if the HTTP transaction completed (even if the other test params make this a
// failure).
//
// The result error will be populated for any status other than Success.
//
// The result body will be populated if the HTTP transaction was completed, even
// if the other test params make this a failure).
func PokeHTTP(host string, port int, path string, params *HTTPPokeParams) HTTPPokeResult {
	// Set default params.
	if params == nil {
		params = &HTTPPokeParams{}
	}

	hostPort := net.JoinHostPort(host, strconv.Itoa(port))
	var url string
	if params.EnableHTTPS {
		url = fmt.Sprintf("https://%s%s", hostPort, path)
	} else {
		url = fmt.Sprintf("http://%s%s", hostPort, path)
	}

	ret := HTTPPokeResult{}

	// Sanity check inputs, because it has happened.  These are the only things
	// that should hard fail the test - they are basically ASSERT()s.
	if host == "" {
		framework.Failf("Got empty host for HTTP poke (%s)", url)
		return ret
	}
	if port == 0 {
		framework.Failf("Got port==0 for HTTP poke (%s)", url)
		return ret
	}

	if params.ExpectCode == 0 {
		params.ExpectCode = http.StatusOK
	}

	if params.Timeout == 0 {
		params.Timeout = 10 * time.Second
	}

	framework.Logf("Poking %q", url)

	resp, err := httpGetNoConnectionPoolTimeout(url, params.Timeout)
	if err != nil {
		ret.Error = err
		neterr, ok := err.(net.Error)
		if ok && neterr.Timeout() {
			ret.Status = HTTPTimeout
		} else if strings.Contains(err.Error(), "connection refused") {
			ret.Status = HTTPRefused
		} else {
			ret.Status = HTTPError
		}
		framework.Logf("Poke(%q): %v", url, err)
		return ret
	}

	ret.Code = resp.StatusCode

	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		ret.Status = HTTPError
		ret.Error = fmt.Errorf("error reading HTTP body: %w", err)
		framework.Logf("Poke(%q): %v", url, ret.Error)
		return ret
	}
	ret.Body = make([]byte, len(body))
	copy(ret.Body, body)

	if resp.StatusCode != params.ExpectCode {
		for _, code := range params.RetriableCodes {
			if resp.StatusCode == code {
				ret.Error = fmt.Errorf("retriable status code: %d", resp.StatusCode)
				ret.Status = HTTPRetryCode
				framework.Logf("Poke(%q): %v", url, ret.Error)
				return ret
			}
		}
		ret.Status = HTTPWrongCode
		ret.Error = fmt.Errorf("bad status code: %d", resp.StatusCode)
		framework.Logf("Poke(%q): %v", url, ret.Error)
		return ret
	}

	if params.BodyContains != "" && !strings.Contains(string(body), params.BodyContains) {
		ret.Status = HTTPBadResponse
		ret.Error = fmt.Errorf("response does not contain expected substring: %q", string(body))
		framework.Logf("Poke(%q): %v", url, ret.Error)
		return ret
	}

	ret.Status = HTTPSuccess
	framework.Logf("Poke(%q): success", url)
	return ret
}

// Does an HTTP GET, but does not reuse TCP connections
// This masks problems where the iptables rule has changed, but we don't see it
func httpGetNoConnectionPoolTimeout(url string, timeout time.Duration) (*http.Response, error) {
	tr := utilnet.SetTransportDefaults(&http.Transport{
		DisableKeepAlives: true,
		TLSClientConfig:   &tls.Config{InsecureSkipVerify: true},
	})
	client := &http.Client{
		Transport: tr,
		Timeout:   timeout,
	}

	return client.Get(url)
}

// WaitForService waits until the service appears (exist == true), or disappears (exist == false)
func WaitForService(ctx context.Context, c clientset.Interface, namespace, name string, exist bool, interval, timeout time.Duration) error {
	err := wait.PollUntilContextTimeout(ctx, interval, timeout, true, func(ctx context.Context) (bool, error) {
		_, err := c.CoreV1().Services(namespace).Get(ctx, name, metav1.GetOptions{})
		switch {
		case err == nil:
			framework.Logf("Service %s in namespace %s found.", name, namespace)
			return exist, nil
		case apierrors.IsNotFound(err):
			framework.Logf("Service %s in namespace %s disappeared.", name, namespace)
			return !exist, nil
		case err != nil:
			framework.Logf("Non-retryable failure while getting service.")
			return false, err
		default:
			framework.Logf("Get service %s in namespace %s failed: %v", name, namespace, err)
			return false, nil
		}
	})
	if err != nil {
		stateMsg := map[bool]string{true: "to appear", false: "to disappear"}
		return fmt.Errorf("error waiting for service %s/%s %s: %w", namespace, name, stateMsg[exist], err)
	}
	return nil
}
