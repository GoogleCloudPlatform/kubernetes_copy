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
	"fmt"
	"strconv"
	"strings"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	utilwait "k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/kubernetes/pkg/cluster/ports"
	"k8s.io/kubernetes/test/e2e/feature"
	"k8s.io/kubernetes/test/e2e/framework"
	e2enetwork "k8s.io/kubernetes/test/e2e/framework/network"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"
	e2essh "k8s.io/kubernetes/test/e2e/framework/ssh"
	"k8s.io/kubernetes/test/e2e/network/common"
	"k8s.io/kubernetes/test/e2e/storage/utils"
	admissionapi "k8s.io/pod-security-admission/api"

	"github.com/onsi/ginkgo/v2"
)

// checkConnectivityToHost launches a pod to test connectivity to the specified
// host. An error will be returned if the host is not reachable from the pod.
//
// An empty nodeName will use the schedule to choose where the pod is executed.
func checkConnectivityToHost(ctx context.Context, f *framework.Framework, nodeName, podName, host string, port, timeout int) error {
	command := []string{
		"nc",
		"-vz",
		"-w", strconv.Itoa(timeout),
		host,
		strconv.Itoa(port),
	}

	pod := e2epod.NewAgnhostPod(f.Namespace.Name, podName, nil, nil, nil)
	pod.Spec.Containers[0].Command = command
	pod.Spec.Containers[0].Args = nil // otherwise 'pause` is magically an argument to nc, which causes all hell to break loose
	nodeSelection := e2epod.NodeSelection{Name: nodeName}
	e2epod.SetNodeSelection(&pod.Spec, nodeSelection)
	pod.Spec.RestartPolicy = v1.RestartPolicyNever

	podClient := f.ClientSet.CoreV1().Pods(f.Namespace.Name)
	_, err := podClient.Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		return err
	}
	err = e2epod.WaitForPodSuccessInNamespace(ctx, f.ClientSet, podName, f.Namespace.Name)

	if err != nil {
		logs, logErr := e2epod.GetPodLogs(ctx, f.ClientSet, f.Namespace.Name, pod.Name, pod.Spec.Containers[0].Name)
		if logErr != nil {
			framework.Logf("Warning: Failed to get logs from pod %q: %v", pod.Name, logErr)
		} else {
			framework.Logf("pod %s/%s logs:\n%s", f.Namespace.Name, pod.Name, logs)
		}
	}

	return err
}

var _ = common.SIGDescribe("Networking", func() {
	var svcname = "nettest"
	f := framework.NewDefaultFramework(svcname)
	f.NamespacePodSecurityLevel = admissionapi.LevelPrivileged

	f.It("should provide Internet connection for containers", feature.NetworkingIPv4, func(ctx context.Context) {
		ginkgo.By("Running container which tries to connect to 8.8.8.8")
		framework.ExpectNoError(
			checkConnectivityToHost(ctx, f, "", "connectivity-test", "8.8.8.8", 53, 30))
	})

	f.It("should provide Internet connection for containers", feature.NetworkingIPv6, "[Experimental][LinuxOnly]", func(ctx context.Context) {
		// IPv6 is not supported on Windows.
		e2eskipper.SkipIfNodeOSDistroIs("windows")
		ginkgo.By("Running container which tries to connect to 2001:4860:4860::8888")
		framework.ExpectNoError(
			checkConnectivityToHost(ctx, f, "", "connectivity-test", "2001:4860:4860::8888", 53, 30))
	})

	f.It("should provider Internet connection for containers using DNS", feature.NetworkingDNS, func(ctx context.Context) {
		ginkgo.By("Running container which tries to connect to google.com")
		framework.ExpectNoError(
			checkConnectivityToHost(ctx, f, "", "connectivity-test", "google.com", 80, 30))
	})

	// First test because it has no dependencies on variables created later on.
	ginkgo.It("should provide unchanging, static URL paths for kubernetes api services", func(ctx context.Context) {
		tests := []struct {
			path string
		}{
			{path: "/healthz"},
			{path: "/api"},
			{path: "/apis"},
			{path: "/metrics"},
			{path: "/openapi/v2"},
			{path: "/version"},
			// TODO: test proxy links here
		}
		for _, test := range tests {
			ginkgo.By(fmt.Sprintf("testing: %s", test.path))
			data, err := f.ClientSet.CoreV1().RESTClient().Get().
				AbsPath(test.path).
				DoRaw(ctx)
			if err != nil {
				framework.Failf("ginkgo.Failed: %v\nBody: %s", err, string(data))
			}
		}
	})

	ginkgo.It("should check kube-proxy urls", func(ctx context.Context) {
		// TODO: this is overkill we just need the host networking pod
		// to hit kube-proxy urls.
		config := e2enetwork.NewNetworkingTestConfig(ctx, f, e2enetwork.UseHostNetwork)

		ginkgo.By("checking kube-proxy URLs")
		config.GetSelfURL(ctx, ports.ProxyHealthzPort, "/healthz", "200 OK")
		// Verify /healthz returns the proper content.
		config.GetSelfURL(ctx, ports.ProxyHealthzPort, "/healthz", "lastUpdated")
		// Verify /proxyMode returns http status code 200.
		config.GetSelfURLStatusCode(ctx, ports.ProxyStatusPort, "/proxyMode", "200")
	})

	ginkgo.Describe("Granular Checks: Services", func() {

		ginkgo.It("should function for pod-Service: http", func(ctx context.Context) {
			config := e2enetwork.NewNetworkingTestConfig(ctx, f)
			ginkgo.By(fmt.Sprintf("dialing(http) %v --> %v:%v (config.clusterIP)", config.TestContainerPod.Name, config.ClusterIP, e2enetwork.ClusterHTTPPort))
			err := config.DialFromTestContainer(ctx, "http", config.ClusterIP, e2enetwork.ClusterHTTPPort, config.MaxTries, 0, config.EndpointHostnames())
			if err != nil {
				framework.Failf("failed dialing endpoint, %v", err)
			}
			ginkgo.By(fmt.Sprintf("dialing(http) %v --> %v:%v (nodeIP)", config.TestContainerPod.Name, config.NodeIP, config.NodeHTTPPort))

			err = config.DialFromTestContainer(ctx, "http", config.NodeIP, config.NodeHTTPPort, config.MaxTries, 0, config.EndpointHostnames())
			if err != nil {
				framework.Failf("failed dialing endpoint, %v", err)
			}
		})

		ginkgo.It("should function for pod-Service: udp", func(ctx context.Context) {
			config := e2enetwork.NewNetworkingTestConfig(ctx, f)
			ginkgo.By(fmt.Sprintf("dialing(udp) %v --> %v:%v (config.clusterIP)", config.TestContainerPod.Name, config.ClusterIP, e2enetwork.ClusterUDPPort))
			err := config.DialFromTestContainer(ctx, "udp", config.ClusterIP, e2enetwork.ClusterUDPPort, config.MaxTries, 0, config.EndpointHostnames())
			if err != nil {
				framework.Failf("failed dialing endpoint, %v", err)
			}

			ginkgo.By(fmt.Sprintf("dialing(udp) %v --> %v:%v (nodeIP)", config.TestContainerPod.Name, config.NodeIP, config.NodeUDPPort))
			err = config.DialFromTestContainer(ctx, "udp", config.NodeIP, config.NodeUDPPort, config.MaxTries, 0, config.EndpointHostnames())
			if err != nil {
				framework.Failf("failed dialing endpoint, %v", err)
			}
		})

		f.It("should function for pod-Service: sctp", feature.SCTPConnectivity, func(ctx context.Context) {
			config := e2enetwork.NewNetworkingTestConfig(ctx, f, e2enetwork.EnableSCTP)
			ginkgo.By(fmt.Sprintf("dialing(sctp) %v --> %v:%v (config.clusterIP)", config.TestContainerPod.Name, config.ClusterIP, e2enetwork.ClusterSCTPPort))
			err := config.DialFromTestContainer(ctx, "sctp", config.ClusterIP, e2enetwork.ClusterSCTPPort, config.MaxTries, 0, config.EndpointHostnames())
			if err != nil {
				framework.Failf("failed dialing endpoint, %v", err)
			}
			ginkgo.By(fmt.Sprintf("dialing(sctp) %v --> %v:%v (nodeIP)", config.TestContainerPod.Name, config.NodeIP, config.NodeSCTPPort))
			err = config.DialFromTestContainer(ctx, "sctp", config.NodeIP, config.NodeSCTPPort, config.MaxTries, 0, config.EndpointHostnames())
			if err != nil {
				framework.Failf("failed dialing endpoint, %v", err)
			}
		})

		ginkgo.It("should function for node-Service: http", func(ctx context.Context) {
			config := e2enetwork.NewNetworkingTestConfig(ctx, f, e2enetwork.UseHostNetwork)
			ginkgo.By(fmt.Sprintf("dialing(http) %v (node) --> %v:%v (config.clusterIP)", config.NodeIP, config.ClusterIP, e2enetwork.ClusterHTTPPort))
			err := config.DialFromNode(ctx, "http", config.ClusterIP, e2enetwork.ClusterHTTPPort, config.MaxTries, 0, config.EndpointHostnames())
			if err != nil {
				framework.Failf("failed dialing endpoint, %v", err)
			}
			ginkgo.By(fmt.Sprintf("dialing(http) %v (node) --> %v:%v (nodeIP)", config.NodeIP, config.NodeIP, config.NodeHTTPPort))
			err = config.DialFromNode(ctx, "http", config.NodeIP, config.NodeHTTPPort, config.MaxTries, 0, config.EndpointHostnames())
			if err != nil {
				framework.Failf("failed dialing endpoint, %v", err)
			}
		})

		ginkgo.It("should function for node-Service: udp", func(ctx context.Context) {
			config := e2enetwork.NewNetworkingTestConfig(ctx, f, e2enetwork.UseHostNetwork)
			ginkgo.By(fmt.Sprintf("dialing(udp) %v (node) --> %v:%v (config.clusterIP)", config.NodeIP, config.ClusterIP, e2enetwork.ClusterUDPPort))
			err := config.DialFromNode(ctx, "udp", config.ClusterIP, e2enetwork.ClusterUDPPort, config.MaxTries, 0, config.EndpointHostnames())
			if err != nil {
				framework.Failf("failed dialing endpoint, %v", err)
			}
			ginkgo.By(fmt.Sprintf("dialing(udp) %v (node) --> %v:%v (nodeIP)", config.NodeIP, config.NodeIP, config.NodeUDPPort))
			err = config.DialFromNode(ctx, "udp", config.NodeIP, config.NodeUDPPort, config.MaxTries, 0, config.EndpointHostnames())
			if err != nil {
				framework.Failf("failed dialing endpoint, %v", err)
			}
		})

		f.It("should function for node-Service: sctp", feature.SCTPConnectivity, func(ctx context.Context) {
			ginkgo.Skip("Skipping SCTP node to service test until DialFromNode supports SCTP #96482")
			config := e2enetwork.NewNetworkingTestConfig(ctx, f, e2enetwork.EnableSCTP)
			ginkgo.By(fmt.Sprintf("dialing(sctp) %v (node) --> %v:%v (config.clusterIP)", config.NodeIP, config.ClusterIP, e2enetwork.ClusterSCTPPort))
			err := config.DialFromNode(ctx, "sctp", config.ClusterIP, e2enetwork.ClusterSCTPPort, config.MaxTries, 0, config.EndpointHostnames())
			if err != nil {
				framework.Failf("failed dialing endpoint, %v", err)
			}
			ginkgo.By(fmt.Sprintf("dialing(sctp) %v (node) --> %v:%v (nodeIP)", config.NodeIP, config.NodeIP, config.NodeSCTPPort))
			err = config.DialFromNode(ctx, "sctp", config.NodeIP, config.NodeSCTPPort, config.MaxTries, 0, config.EndpointHostnames())
			if err != nil {
				framework.Failf("failed dialing endpoint, %v", err)
			}
		})

		ginkgo.It("should function for endpoint-Service: http", func(ctx context.Context) {
			config := e2enetwork.NewNetworkingTestConfig(ctx, f)
			ginkgo.By(fmt.Sprintf("dialing(http) %v (endpoint) --> %v:%v (config.clusterIP)", config.EndpointPods[0].Name, config.ClusterIP, e2enetwork.ClusterHTTPPort))
			err := config.DialFromEndpointContainer(ctx, "http", config.ClusterIP, e2enetwork.ClusterHTTPPort, config.MaxTries, 0, config.EndpointHostnames())
			if err != nil {
				framework.Failf("failed dialing endpoint, %v", err)
			}
			ginkgo.By(fmt.Sprintf("dialing(http) %v (endpoint) --> %v:%v (nodeIP)", config.EndpointPods[0].Name, config.NodeIP, config.NodeHTTPPort))
			err = config.DialFromEndpointContainer(ctx, "http", config.NodeIP, config.NodeHTTPPort, config.MaxTries, 0, config.EndpointHostnames())
			if err != nil {
				framework.Failf("failed dialing endpoint, %v", err)
			}
		})

		ginkgo.It("should function for endpoint-Service: udp", func(ctx context.Context) {
			config := e2enetwork.NewNetworkingTestConfig(ctx, f)
			ginkgo.By(fmt.Sprintf("dialing(udp) %v (endpoint) --> %v:%v (config.clusterIP)", config.EndpointPods[0].Name, config.ClusterIP, e2enetwork.ClusterUDPPort))
			err := config.DialFromEndpointContainer(ctx, "udp", config.ClusterIP, e2enetwork.ClusterUDPPort, config.MaxTries, 0, config.EndpointHostnames())
			if err != nil {
				framework.Failf("failed dialing endpoint, %v", err)
			}

			ginkgo.By(fmt.Sprintf("dialing(udp) %v (endpoint) --> %v:%v (nodeIP)", config.EndpointPods[0].Name, config.NodeIP, config.NodeUDPPort))
			err = config.DialFromEndpointContainer(ctx, "udp", config.NodeIP, config.NodeUDPPort, config.MaxTries, 0, config.EndpointHostnames())
			if err != nil {
				framework.Failf("failed dialing endpoint, %v", err)
			}
		})

		f.It("should function for endpoint-Service: sctp", feature.SCTPConnectivity, func(ctx context.Context) {
			config := e2enetwork.NewNetworkingTestConfig(ctx, f, e2enetwork.EnableSCTP)
			ginkgo.By(fmt.Sprintf("dialing(sctp) %v (endpoint) --> %v:%v (config.clusterIP)", config.EndpointPods[0].Name, config.ClusterIP, e2enetwork.ClusterSCTPPort))
			err := config.DialFromEndpointContainer(ctx, "sctp", config.ClusterIP, e2enetwork.ClusterSCTPPort, config.MaxTries, 0, config.EndpointHostnames())
			if err != nil {
				framework.Failf("failed dialing endpoint, %v", err)
			}

			ginkgo.By(fmt.Sprintf("dialing(sctp) %v (endpoint) --> %v:%v (nodeIP)", config.EndpointPods[0].Name, config.NodeIP, config.NodeSCTPPort))
			err = config.DialFromEndpointContainer(ctx, "sctp", config.NodeIP, config.NodeSCTPPort, config.MaxTries, 0, config.EndpointHostnames())
			if err != nil {
				framework.Failf("failed dialing endpoint, %v", err)
			}
		})

		// This test ensures that in a situation where multiple services exist with the same selector,
		// deleting one of the services does not affect the connectivity of the remaining service
		ginkgo.It("should function for multiple endpoint-Services with same selector", func(ctx context.Context) {
			config := e2enetwork.NewNetworkingTestConfig(ctx, f)
			ginkgo.By("creating a second service with same selector")
			svc2, httpPort := createSecondNodePortService(ctx, f, config)

			// original service should work
			ginkgo.By(fmt.Sprintf("dialing(http) %v (endpoint) --> %v:%v (config.clusterIP)", config.EndpointPods[0].Name, config.ClusterIP, e2enetwork.ClusterHTTPPort))
			err := config.DialFromEndpointContainer(ctx, "http", config.ClusterIP, e2enetwork.ClusterHTTPPort, config.MaxTries, 0, config.EndpointHostnames())
			if err != nil {
				framework.Failf("failed dialing endpoint, %v", err)
			}
			ginkgo.By(fmt.Sprintf("dialing(http) %v (endpoint) --> %v:%v (nodeIP)", config.EndpointPods[0].Name, config.NodeIP, config.NodeHTTPPort))
			err = config.DialFromEndpointContainer(ctx, "http", config.NodeIP, config.NodeHTTPPort, config.MaxTries, 0, config.EndpointHostnames())
			if err != nil {
				framework.Failf("failed dialing endpoint, %v", err)
			}
			// Dial second service
			ginkgo.By(fmt.Sprintf("dialing(http) %v (endpoint) --> %v:%v (svc2.clusterIP)", config.EndpointPods[0].Name, svc2.Spec.ClusterIP, e2enetwork.ClusterHTTPPort))
			err = config.DialFromEndpointContainer(ctx, "http", svc2.Spec.ClusterIP, e2enetwork.ClusterHTTPPort, config.MaxTries, 0, config.EndpointHostnames())
			if err != nil {
				framework.Failf("failed dialing endpoint, %v", err)
			}

			ginkgo.By(fmt.Sprintf("dialing(http) %v (endpoint) --> %v:%v (nodeIP)", config.EndpointPods[0].Name, config.NodeIP, httpPort))
			err = config.DialFromEndpointContainer(ctx, "http", config.NodeIP, httpPort, config.MaxTries, 0, config.EndpointHostnames())
			if err != nil {
				framework.Failf("failed dialing endpoint, %v", err)
			}

			ginkgo.By("deleting the original node port service")
			config.DeleteNodePortService(ctx)

			// Second service should continue to function unaffected
			ginkgo.By(fmt.Sprintf("dialing(http) %v (endpoint) --> %v:%v (svc2.clusterIP)", config.EndpointPods[0].Name, svc2.Spec.ClusterIP, e2enetwork.ClusterHTTPPort))
			err = config.DialFromEndpointContainer(ctx, "http", svc2.Spec.ClusterIP, e2enetwork.ClusterHTTPPort, config.MaxTries, 0, config.EndpointHostnames())
			if err != nil {
				framework.Failf("failed dialing endpoint, %v", err)
			}
			ginkgo.By(fmt.Sprintf("dialing(http) %v (endpoint) --> %v:%v (nodeIP)", config.EndpointPods[0].Name, config.NodeIP, httpPort))
			err = config.DialFromEndpointContainer(ctx, "http", config.NodeIP, httpPort, config.MaxTries, 0, config.EndpointHostnames())
			if err != nil {
				framework.Failf("failed dialing endpoint, %v", err)
			}
		})

		ginkgo.It("should update endpoints: http", func(ctx context.Context) {
			config := e2enetwork.NewNetworkingTestConfig(ctx, f)
			ginkgo.By(fmt.Sprintf("dialing(http) %v --> %v:%v (config.clusterIP)", config.TestContainerPod.Name, config.ClusterIP, e2enetwork.ClusterHTTPPort))
			err := config.DialFromTestContainer(ctx, "http", config.ClusterIP, e2enetwork.ClusterHTTPPort, config.MaxTries, 0, config.EndpointHostnames())
			if err != nil {
				framework.Failf("failed dialing endpoint (initial), %v", err)
			}
			ginkgo.By("Deleting a pod which, will be replaced with a new endpoint")
			config.DeleteNetProxyPod(ctx)

			ginkgo.By(fmt.Sprintf("dialing(http) %v --> %v:%v (config.clusterIP) (endpoint recovery)", config.TestContainerPod.Name, config.ClusterIP, e2enetwork.ClusterHTTPPort))
			err = config.DialFromTestContainer(ctx, "http", config.ClusterIP, e2enetwork.ClusterHTTPPort, config.MaxTries, config.MaxTries, config.EndpointHostnames())
			if err != nil {
				framework.Failf("failed dialing endpoint (recovery), %v", err)
			}
		})

		ginkgo.It("should update endpoints: udp", func(ctx context.Context) {
			config := e2enetwork.NewNetworkingTestConfig(ctx, f)
			ginkgo.By(fmt.Sprintf("dialing(udp) %v --> %v:%v (config.clusterIP)", config.TestContainerPod.Name, config.ClusterIP, e2enetwork.ClusterUDPPort))
			err := config.DialFromTestContainer(ctx, "udp", config.ClusterIP, e2enetwork.ClusterUDPPort, config.MaxTries, 0, config.EndpointHostnames())
			if err != nil {
				framework.Failf("failed dialing endpoint (initial), %v", err)
			}
			ginkgo.By("Deleting a pod which, will be replaced with a new endpoint")
			config.DeleteNetProxyPod(ctx)

			ginkgo.By(fmt.Sprintf("dialing(udp) %v --> %v:%v (config.clusterIP) (endpoint recovery)", config.TestContainerPod.Name, config.ClusterIP, e2enetwork.ClusterUDPPort))
			err = config.DialFromTestContainer(ctx, "udp", config.ClusterIP, e2enetwork.ClusterUDPPort, config.MaxTries, config.MaxTries, config.EndpointHostnames())
			if err != nil {
				framework.Failf("failed dialing endpoint (recovery), %v", err)
			}
		})

		// Slow because we confirm that the nodePort doesn't serve traffic, which requires a period of polling.
		f.It("should update nodePort: http", f.WithSlow(), func(ctx context.Context) {
			config := e2enetwork.NewNetworkingTestConfig(ctx, f, e2enetwork.UseHostNetwork)
			ginkgo.By(fmt.Sprintf("dialing(http) %v (node) --> %v:%v (ctx, nodeIP) and getting ALL host endpoints", config.NodeIP, config.NodeIP, config.NodeHTTPPort))
			err := config.DialFromNode(ctx, "http", config.NodeIP, config.NodeHTTPPort, config.MaxTries, 0, config.EndpointHostnames())
			if err != nil {
				framework.Failf("Error dialing http from node: %v", err)
			}
			ginkgo.By("Deleting the node port access point")
			config.DeleteNodePortService(ctx)

			ginkgo.By(fmt.Sprintf("dialing(http) %v (node) --> %v:%v (nodeIP) and getting ZERO host endpoints", config.NodeIP, config.NodeIP, config.NodeHTTPPort))
			// #106770 MaxTries can be very large on large clusters, with the risk that a new NodePort is created by another test and start to answer traffic.
			// Since we only want to assert that traffic is not being forwarded anymore and the retry timeout is 2 seconds, consider the test is correct
			// if the service doesn't answer after 10 tries.
			err = config.DialFromNode(ctx, "http", config.NodeIP, config.NodeHTTPPort, 10, 10, sets.New[string]())
			if err != nil {
				framework.Failf("Failure validating that node port service STOPPED removed properly: %v", err)
			}
		})

		// quick validation of udp, next test confirms that this services update as well after endpoints are removed, but is slower.
		ginkgo.It("should support basic nodePort: udp functionality", func(ctx context.Context) {
			config := e2enetwork.NewNetworkingTestConfig(ctx, f, e2enetwork.UseHostNetwork)
			ginkgo.By(fmt.Sprintf("dialing(udp) %v (node) --> %v:%v (nodeIP) and getting ALL host endpoints", config.NodeIP, config.NodeIP, config.NodeUDPPort))
			err := config.DialFromNode(ctx, "udp", config.NodeIP, config.NodeUDPPort, config.MaxTries, 0, config.EndpointHostnames())
			if err != nil {
				framework.Failf("Failure validating that nodePort service WAS forwarding properly: %v", err)
			}
		})

		// Slow because we confirm that the nodePort doesn't serve traffic, which requires a period of polling.
		f.It("should update nodePort: udp", f.WithSlow(), func(ctx context.Context) {
			config := e2enetwork.NewNetworkingTestConfig(ctx, f, e2enetwork.UseHostNetwork)
			ginkgo.By(fmt.Sprintf("dialing(udp) %v (node) --> %v:%v (nodeIP) and getting ALL host endpoints", config.NodeIP, config.NodeIP, config.NodeUDPPort))
			err := config.DialFromNode(ctx, "udp", config.NodeIP, config.NodeUDPPort, config.MaxTries, 0, config.EndpointHostnames())
			if err != nil {
				framework.Failf("Failure validating that nodePort service WAS forwarding properly: %v", err)
			}

			ginkgo.By("Deleting the node port access point")
			config.DeleteNodePortService(ctx)

			ginkgo.By(fmt.Sprintf("dialing(udp) %v (node) --> %v:%v (nodeIP) and getting ZERO host endpoints", config.NodeIP, config.NodeIP, config.NodeUDPPort))
			// #106770 MaxTries can be very large on large clusters, with the risk that a new NodePort is created by another test and start to answer traffic.
			// Since we only want to assert that traffic is not being forwarded anymore and the retry timeout is 2 seconds, consider the test is correct
			// if the service doesn't answer after 10 tries.
			err = config.DialFromNode(ctx, "udp", config.NodeIP, config.NodeUDPPort, 10, 10, sets.New[string]())
			if err != nil {
				framework.Failf("Failure validating that node port service STOPPED removed properly: %v", err)
			}
		})

		// [LinuxOnly]: Windows does not support session affinity.
		ginkgo.It("should function for client IP based session affinity: http [LinuxOnly]", func(ctx context.Context) {
			config := e2enetwork.NewNetworkingTestConfig(ctx, f)
			ginkgo.By(fmt.Sprintf("dialing(http) %v --> %v:%v", config.TestContainerPod.Name, config.SessionAffinityService.Spec.ClusterIP, e2enetwork.ClusterHTTPPort))

			// Check if number of endpoints returned are exactly one.
			eps, err := config.GetEndpointsFromTestContainer(ctx, "http", config.SessionAffinityService.Spec.ClusterIP, e2enetwork.ClusterHTTPPort, e2enetwork.SessionAffinityChecks)
			if err != nil {
				framework.Failf("ginkgo.Failed to get endpoints from test container, error: %v", err)
			}
			if len(eps) == 0 {
				framework.Failf("Unexpected no endpoints return")
			}
			if len(eps) > 1 {
				framework.Failf("Unexpected endpoints return: %v, expect 1 endpoints", eps)
			}
		})

		// [LinuxOnly]: Windows does not support session affinity.
		ginkgo.It("should function for client IP based session affinity: udp [LinuxOnly]", func(ctx context.Context) {
			config := e2enetwork.NewNetworkingTestConfig(ctx, f)
			ginkgo.By(fmt.Sprintf("dialing(udp) %v --> %v:%v", config.TestContainerPod.Name, config.SessionAffinityService.Spec.ClusterIP, e2enetwork.ClusterUDPPort))

			// Check if number of endpoints returned are exactly one.
			eps, err := config.GetEndpointsFromTestContainer(ctx, "udp", config.SessionAffinityService.Spec.ClusterIP, e2enetwork.ClusterUDPPort, e2enetwork.SessionAffinityChecks)
			if err != nil {
				framework.Failf("ginkgo.Failed to get endpoints from test container, error: %v", err)
			}
			if len(eps) == 0 {
				framework.Failf("Unexpected no endpoints return")
			}
			if len(eps) > 1 {
				framework.Failf("Unexpected endpoints return: %v, expect 1 endpoints", eps)
			}
		})

		ginkgo.It("should be able to handle large requests: http", func(ctx context.Context) {
			config := e2enetwork.NewNetworkingTestConfig(ctx, f)
			ginkgo.By(fmt.Sprintf("dialing(http) %v --> %v:%v (config.clusterIP)", config.TestContainerPod.Name, config.ClusterIP, e2enetwork.ClusterHTTPPort))
			message := strings.Repeat("42", 1000)
			err := config.DialEchoFromTestContainer(ctx, "http", config.ClusterIP, e2enetwork.ClusterHTTPPort, config.MaxTries, 0, message)
			if err != nil {
				framework.Failf("failed dialing endpoint, %v", err)
			}
		})

		ginkgo.It("should be able to handle large requests: udp", func(ctx context.Context) {
			config := e2enetwork.NewNetworkingTestConfig(ctx, f)
			ginkgo.By(fmt.Sprintf("dialing(udp) %v --> %v:%v (config.clusterIP)", config.TestContainerPod.Name, config.ClusterIP, e2enetwork.ClusterUDPPort))
			message := "n" + strings.Repeat("o", 1999)
			err := config.DialEchoFromTestContainer(ctx, "udp", config.ClusterIP, e2enetwork.ClusterUDPPort, config.MaxTries, 0, message)
			if err != nil {
				framework.Failf("failed dialing endpoint, %v", err)
			}
		})

		// if the endpoints pods use hostNetwork, several tests can't run in parallel
		// because the pods will try to acquire the same port in the host.
		// We run the test in serial, to avoid port conflicts.
		ginkgo.It("should function for service endpoints using hostNetwork", func(ctx context.Context) {
			config := e2enetwork.NewNetworkingTestConfig(ctx, f, e2enetwork.UseHostNetwork, e2enetwork.EndpointsUseHostNetwork, e2enetwork.EchoNodeName)

			ginkgo.By("pod-Service(hostNetwork): http")

			ginkgo.By(fmt.Sprintf("dialing(http) %v --> %v:%v (config.clusterIP)", config.TestContainerPod.Name, config.ClusterIP, e2enetwork.ClusterHTTPPort))
			err := config.DialFromTestContainer(ctx, "http", config.ClusterIP, e2enetwork.ClusterHTTPPort, config.MaxTries, 0, config.EndpointNodeNames())
			if err != nil {
				framework.Failf("failed dialing endpoint, %v", err)
			}
			ginkgo.By(fmt.Sprintf("dialing(http) %v --> %v:%v (nodeIP)", config.TestContainerPod.Name, config.NodeIP, config.NodeHTTPPort))
			err = config.DialFromTestContainer(ctx, "http", config.NodeIP, config.NodeHTTPPort, config.MaxTries, 0, config.EndpointNodeNames())
			if err != nil {
				framework.Failf("failed dialing endpoint, %v", err)
			}

			ginkgo.By("pod-Service(hostNetwork): udp")

			ginkgo.By(fmt.Sprintf("dialing(udp) %v --> %v:%v (config.clusterIP)", config.TestContainerPod.Name, config.ClusterIP, e2enetwork.ClusterUDPPort))
			err = config.DialFromTestContainer(ctx, "udp", config.ClusterIP, e2enetwork.ClusterUDPPort, config.MaxTries, 0, config.EndpointNodeNames())
			if err != nil {
				framework.Failf("failed dialing endpoint, %v", err)
			}

			ginkgo.By(fmt.Sprintf("dialing(udp) %v --> %v:%v (nodeIP)", config.TestContainerPod.Name, config.NodeIP, config.NodeUDPPort))
			err = config.DialFromTestContainer(ctx, "udp", config.NodeIP, config.NodeUDPPort, config.MaxTries, 0, config.EndpointNodeNames())
			if err != nil {
				framework.Failf("failed dialing endpoint, %v", err)
			}

			ginkgo.By("node-Service(hostNetwork): http")

			ginkgo.By(fmt.Sprintf("dialing(http) %v (node) --> %v:%v (config.clusterIP)", config.NodeIP, config.ClusterIP, e2enetwork.ClusterHTTPPort))
			err = config.DialFromNode(ctx, "http", config.ClusterIP, e2enetwork.ClusterHTTPPort, config.MaxTries, 0, config.EndpointNodeNames())
			if err != nil {
				framework.Failf("failed dialing endpoint, %v", err)
			}
			ginkgo.By(fmt.Sprintf("dialing(http) %v (node) --> %v:%v (nodeIP)", config.NodeIP, config.NodeIP, config.NodeHTTPPort))
			err = config.DialFromNode(ctx, "http", config.NodeIP, config.NodeHTTPPort, config.MaxTries, 0, config.EndpointNodeNames())
			if err != nil {
				framework.Failf("failed dialing endpoint, %v", err)
			}
			ginkgo.By("node-Service(hostNetwork): udp")

			ginkgo.By(fmt.Sprintf("dialing(udp) %v (node) --> %v:%v (config.clusterIP)", config.NodeIP, config.ClusterIP, e2enetwork.ClusterUDPPort))
			err = config.DialFromNode(ctx, "udp", config.ClusterIP, e2enetwork.ClusterUDPPort, config.MaxTries, 0, config.EndpointNodeNames())
			if err != nil {
				framework.Failf("failed dialing endpoint, %v", err)
			}
			ginkgo.By(fmt.Sprintf("dialing(udp) %v (node) --> %v:%v (nodeIP)", config.NodeIP, config.NodeIP, config.NodeUDPPort))
			err = config.DialFromNode(ctx, "udp", config.NodeIP, config.NodeUDPPort, config.MaxTries, 0, config.EndpointNodeNames())
			if err != nil {
				framework.Failf("failed dialing endpoint, %v", err)
			}

			ginkgo.By("handle large requests: http(hostNetwork)")

			ginkgo.By(fmt.Sprintf("dialing(http) %v --> %v:%v (config.clusterIP)", config.TestContainerPod.Name, config.ClusterIP, e2enetwork.ClusterHTTPPort))
			message := strings.Repeat("42", 1000)
			err = config.DialEchoFromTestContainer(ctx, "http", config.ClusterIP, e2enetwork.ClusterHTTPPort, config.MaxTries, 0, message)
			if err != nil {
				framework.Failf("failed dialing endpoint, %v", err)
			}

			ginkgo.By("handle large requests: udp(hostNetwork)")

			ginkgo.By(fmt.Sprintf("dialing(udp) %v --> %v:%v (config.clusterIP)", config.TestContainerPod.Name, config.ClusterIP, e2enetwork.ClusterUDPPort))
			message = "n" + strings.Repeat("o", 1999)
			err = config.DialEchoFromTestContainer(ctx, "udp", config.ClusterIP, e2enetwork.ClusterUDPPort, config.MaxTries, 0, message)
			if err != nil {
				framework.Failf("failed dialing endpoint, %v", err)
			}

		})

	})

	f.It("should recreate its iptables rules if they are deleted", f.WithDisruptive(), func(ctx context.Context) {
		e2eskipper.SkipUnlessProviderIs(framework.ProvidersWithSSH...)
		e2eskipper.SkipUnlessSSHKeyPresent()

		hosts, err := e2essh.NodeSSHHosts(ctx, f.ClientSet)
		framework.ExpectNoError(err, "failed to find external/internal IPs for every node")
		if len(hosts) == 0 {
			framework.Failf("No ssh-able nodes")
		}
		host := hosts[0]

		ns := f.Namespace.Name
		numPods, servicePort := 3, defaultServeHostnameServicePort
		svc := "iptables-flush-test"

		ginkgo.DeferCleanup(StopServeHostnameService, f.ClientSet, ns, svc)
		podNames, svcIP, err := StartServeHostnameService(ctx, f.ClientSet, getServeHostnameService(svc), ns, numPods)
		framework.ExpectNoError(err, "failed to create replication controller with service: %s in the namespace: %s", svc, ns)

		// Ideally we want to reload the system firewall, but we don't necessarily
		// know how to do that on this system ("firewall-cmd --reload"? "systemctl
		// restart iptables"?). So instead we just manually delete all "KUBE-"
		// chains.

		ginkgo.By("dumping iptables rules on node " + host)
		result, err := e2essh.SSH(ctx, "sudo iptables-save", host, framework.TestContext.Provider)
		e2essh.LogResult(result)
		if err != nil || result.Code != 0 {
			framework.Failf("couldn't dump iptable rules: %v", err)
		}

		// All the commands that delete rules have to come before all the commands
		// that delete chains, since the chains can't be deleted while there are
		// still rules referencing them.
		var deleteRuleCmds, deleteChainCmds []string
		table := ""
		for _, line := range strings.Split(result.Stdout, "\n") {
			if strings.HasPrefix(line, "*") {
				table = line[1:]
			} else if table == "" {
				continue
			}

			// Delete jumps from non-KUBE chains to KUBE chains
			if !strings.HasPrefix(line, "-A KUBE-") && strings.Contains(line, "-j KUBE-") {
				deleteRuleCmds = append(deleteRuleCmds, fmt.Sprintf("sudo iptables -t %s -D %s || true", table, line[3:]))
			}
			// Flush and delete all KUBE chains
			if strings.HasPrefix(line, ":KUBE-") {
				chain := strings.Split(line, " ")[0][1:]
				deleteRuleCmds = append(deleteRuleCmds, fmt.Sprintf("sudo iptables -t %s -F %s || true", table, chain))
				deleteChainCmds = append(deleteChainCmds, fmt.Sprintf("sudo iptables -t %s -X %s || true", table, chain))
			}
		}
		cmd := strings.Join(append(deleteRuleCmds, deleteChainCmds...), "\n")

		ginkgo.By("deleting all KUBE-* iptables chains")
		result, err = e2essh.SSH(ctx, cmd, host, framework.TestContext.Provider)
		if err != nil || result.Code != 0 {
			e2essh.LogResult(result)
			framework.Failf("couldn't delete iptable rules: %v", err)
		}

		ginkgo.By("verifying that kube-proxy rules are eventually recreated")
		framework.ExpectNoError(verifyServeHostnameServiceUp(ctx, f.ClientSet, ns, podNames, svcIP, servicePort))

		ginkgo.By("verifying that kubelet rules are eventually recreated")
		err = utilwait.PollImmediate(framework.Poll, framework.RestartNodeReadyAgainTimeout, func() (bool, error) {
			result, err = e2essh.SSH(ctx, "sudo iptables-save -t mangle", host, framework.TestContext.Provider)
			if err != nil || result.Code != 0 {
				e2essh.LogResult(result)
				return false, err
			}

			if strings.Contains(result.Stdout, "\n:KUBE-IPTABLES-HINT") {
				return true, nil
			}
			return false, nil
		})
		if err != nil {
			e2essh.LogResult(result)
		}
		framework.ExpectNoError(err, "kubelet did not recreate its iptables rules")
	})

	// This is [Serial] because it can't run at the same time as the
	// [Feature:SCTPConnectivity] tests, since they may cause sctp.ko to be loaded.
	f.It("should allow creating a Pod with an SCTP HostPort [LinuxOnly]", f.WithSerial(), func(ctx context.Context) {
		node, err := e2enode.GetRandomReadySchedulableNode(ctx, f.ClientSet)
		framework.ExpectNoError(err)
		hostExec := utils.NewHostExec(f)
		ginkgo.DeferCleanup(hostExec.Cleanup)

		ginkgo.By("getting the state of the sctp module on the selected node")
		nodes := &v1.NodeList{}
		nodes.Items = append(nodes.Items, *node)
		sctpLoadedAtStart := CheckSCTPModuleLoadedOnNodes(ctx, f, nodes)

		ginkgo.By("creating a pod with hostport on the selected node")
		podName := "hostport"
		ports := []v1.ContainerPort{{Protocol: v1.ProtocolSCTP, ContainerPort: 5060, HostPort: 5060}}
		podSpec := e2epod.NewAgnhostPod(f.Namespace.Name, podName, nil, nil, ports)
		nodeSelection := e2epod.NodeSelection{Name: node.Name}
		e2epod.SetNodeSelection(&podSpec.Spec, nodeSelection)

		ginkgo.By(fmt.Sprintf("Launching the pod on node %v", node.Name))
		e2epod.NewPodClient(f).CreateSync(ctx, podSpec)
		ginkgo.DeferCleanup(func(ctx context.Context) {
			err := f.ClientSet.CoreV1().Pods(f.Namespace.Name).Delete(ctx, podName, metav1.DeleteOptions{})
			framework.ExpectNoError(err, "failed to delete pod: %s in namespace: %s", podName, f.Namespace.Name)
		})
		ginkgo.By("validating sctp module is still not loaded")
		sctpLoadedAtEnd := CheckSCTPModuleLoadedOnNodes(ctx, f, nodes)
		if !sctpLoadedAtStart && sctpLoadedAtEnd {
			framework.Failf("The state of the sctp module has changed due to the test case")
		}
	})
})
