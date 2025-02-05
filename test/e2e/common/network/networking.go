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

package network

import (
	"context"

	"github.com/onsi/ginkgo/v2"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/kubernetes/test/e2e/feature"
	"k8s.io/kubernetes/test/e2e/framework"
	e2enetwork "k8s.io/kubernetes/test/e2e/framework/network"
	admissionapi "k8s.io/pod-security-admission/api"
)

var _ = SIGDescribe("Networking", func() {
	f := framework.NewDefaultFramework("pod-network-test")
	f.NamespacePodSecurityLevel = admissionapi.LevelPrivileged

	ginkgo.Describe("Granular Checks: Pods", func() {

		checkPodToPodConnectivity := func(ctx context.Context, config *e2enetwork.NetworkingTestConfig, protocol string, port int) {
			// breadth first poll to quickly estimate failure.
			failedPodsByHost := map[string][]*v1.Pod{}
			// First time, we'll quickly try all pods, breadth first.
			for _, endpointPod := range config.EndpointPods {
				framework.Logf("Breadth first check of %v on host %v...", endpointPod.Status.PodIP, endpointPod.Status.HostIP)
				if err := config.DialFromTestContainer(ctx, protocol, endpointPod.Status.PodIP, port, 1, 0, sets.New(endpointPod.Name)); err != nil {
					if _, ok := failedPodsByHost[endpointPod.Status.HostIP]; !ok {
						failedPodsByHost[endpointPod.Status.HostIP] = []*v1.Pod{}
					}
					failedPodsByHost[endpointPod.Status.HostIP] = append(failedPodsByHost[endpointPod.Status.HostIP], endpointPod)
					framework.Logf("...failed...will try again in next pass")
				}
			}
			errors := []error{}
			// Second time, we pass through pods more carefully...
			framework.Logf("Going to retry %v out of %v pods....", len(failedPodsByHost), len(config.EndpointPods))
			for host, failedPods := range failedPodsByHost {
				framework.Logf("Doublechecking %v pods in host %v which weren't seen the first time.", len(failedPods), host)
				for _, endpointPod := range failedPods {
					framework.Logf("Now attempting to probe pod [[[ %v ]]]", endpointPod.Status.PodIP)
					if err := config.DialFromTestContainer(ctx, protocol, endpointPod.Status.PodIP, port, config.MaxTries, 0, sets.New(endpointPod.Name)); err != nil {
						errors = append(errors, err)
					} else {
						framework.Logf("Was able to reach %v on %v ", endpointPod.Status.PodIP, endpointPod.Status.HostIP)
					}
					framework.Logf("... Done probing pod [[[ %v ]]]", endpointPod.Status.PodIP)
				}
				framework.Logf("succeeded at polling %v out of %v connections", len(config.EndpointPods)-len(errors), len(config.EndpointPods))
			}
			if len(errors) > 0 {
				framework.Logf("pod polling failure summary:")
				for _, e := range errors {
					framework.Logf("Collected error: %v", e)
				}
				framework.Failf("failed,  %v out of %v connections failed", len(errors), len(config.EndpointPods))
			}
		}

		// Try to hit all endpoints through a test container, retry 5 times,
		// expect exactly one unique hostname. Each of these endpoints reports
		// its own hostname.
		/*
			Release: v1.9, v1.18
			Testname: Networking, intra pod http
			Description: Create a hostexec pod that is capable of curl to netcat commands. Create a test Pod that will act as a webserver front end exposing ports 8080 for tcp and 8081 for udp. The netserver service proxies are created on specified number of nodes.
			The kubectl exec on the webserver container MUST reach a http port on the each of service proxy endpoints in the cluster and the request MUST be successful. Container will execute curl command to reach the service port within specified max retry limit and MUST result in reporting unique hostnames.
		*/
		framework.ConformanceIt("should function for intra-pod communication: http", f.WithNodeConformance(), func(ctx context.Context) {
			config := e2enetwork.NewCoreNetworkingTestConfig(ctx, f, false)
			checkPodToPodConnectivity(ctx, config, "http", e2enetwork.EndpointHTTPPort)
		})

		/*
			Release: v1.9, v1.18
			Testname: Networking, intra pod udp
			Description: Create a hostexec pod that is capable of curl to netcat commands. Create a test Pod that will act as a webserver front end exposing ports 8080 for tcp and 8081 for udp. The netserver service proxies are created on specified number of nodes.
			The kubectl exec on the webserver container MUST reach a udp port on the each of service proxy endpoints in the cluster and the request MUST be successful. Container will execute curl command to reach the service port within specified max retry limit and MUST result in reporting unique hostnames.
		*/
		framework.ConformanceIt("should function for intra-pod communication: udp", f.WithNodeConformance(), func(ctx context.Context) {
			config := e2enetwork.NewCoreNetworkingTestConfig(ctx, f, false)
			checkPodToPodConnectivity(ctx, config, "udp", e2enetwork.EndpointUDPPort)
		})

		/*
			Release: v1.9
			Testname: Networking, intra pod http, from node
			Description: Create a hostexec pod that is capable of curl to netcat commands. Create a test Pod that will act as a webserver front end exposing ports 8080 for tcp and 8081 for udp. The netserver service proxies are created on specified number of nodes.
			The kubectl exec on the webserver container MUST reach a http port on the each of service proxy endpoints in the cluster using a http post(protocol=tcp)  and the request MUST be successful. Container will execute curl command to reach the service port within specified max retry limit and MUST result in reporting unique hostnames.
			This test is marked LinuxOnly it breaks when using Overlay networking with Windows.
		*/
		framework.ConformanceIt("should function for node-pod communication: http [LinuxOnly]", f.WithNodeConformance(), func(ctx context.Context) {
			config := e2enetwork.NewCoreNetworkingTestConfig(ctx, f, true)
			for _, endpointPod := range config.EndpointPods {
				err := config.DialFromNode(ctx, "http", endpointPod.Status.PodIP, e2enetwork.EndpointHTTPPort, config.MaxTries, 0, sets.New(endpointPod.Name))
				if err != nil {
					framework.Failf("Error dialing HTTP node to pod %v", err)
				}
			}
		})

		/*
			Release: v1.9
			Testname: Networking, intra pod http, from node
			Description: Create a hostexec pod that is capable of curl to netcat commands. Create a test Pod that will act as a webserver front end exposing ports 8080 for tcp and 8081 for udp. The netserver service proxies are created on specified number of nodes.
			The kubectl exec on the webserver container MUST reach a http port on the each of service proxy endpoints in the cluster using a http post(protocol=udp)  and the request MUST be successful. Container will execute curl command to reach the service port within specified max retry limit and MUST result in reporting unique hostnames.
			This test is marked LinuxOnly it breaks when using Overlay networking with Windows.
		*/
		framework.ConformanceIt("should function for node-pod communication: udp [LinuxOnly]", f.WithNodeConformance(), func(ctx context.Context) {
			config := e2enetwork.NewCoreNetworkingTestConfig(ctx, f, true)
			for _, endpointPod := range config.EndpointPods {
				err := config.DialFromNode(ctx, "udp", endpointPod.Status.PodIP, e2enetwork.EndpointUDPPort, config.MaxTries, 0, sets.New(endpointPod.Name))
				if err != nil {
					framework.Failf("Error dialing UDP from node to pod: %v", err)
				}
			}
		})

		f.It("should function for intra-pod communication: sctp [LinuxOnly]", feature.SCTPConnectivity, func(ctx context.Context) {
			config := e2enetwork.NewNetworkingTestConfig(ctx, f, e2enetwork.EnableSCTP)
			checkPodToPodConnectivity(ctx, config, "sctp", e2enetwork.EndpointSCTPPort)
		})

		f.It("should function for node-pod communication: sctp [LinuxOnly]", feature.SCTPConnectivity, func(ctx context.Context) {
			ginkgo.Skip("Skipping SCTP node to pod test until DialFromNode supports SCTP #96482")
			config := e2enetwork.NewNetworkingTestConfig(ctx, f, e2enetwork.EnableSCTP)
			for _, endpointPod := range config.EndpointPods {
				err := config.DialFromNode(ctx, "sctp", endpointPod.Status.PodIP, e2enetwork.EndpointSCTPPort, config.MaxTries, 0, sets.New(endpointPod.Name))
				if err != nil {
					framework.Failf("Error dialing SCTP from node to pod: %v", err)
				}
			}
		})

	})
})
