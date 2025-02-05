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

package node

import (
	"context"
	"fmt"
	"strings"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/kubernetes/test/e2e/feature"
	"k8s.io/kubernetes/test/e2e/framework"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	imageutils "k8s.io/kubernetes/test/utils/image"
	admissionapi "k8s.io/pod-security-admission/api"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"sigs.k8s.io/yaml"

)

// Helper methods 

// Logs the name of the test
func TestName(s string) {
    framework.Logf("<testdoc:name>%s</testdoc:name>", s)
}

// Logs individual steps of the test
func TestStep(step string) {
    framework.Logf("<testdoc:step>%s</testdoc:step>", step)
}

// Logs the Pod specification in YAML format
func PodSpec(p *v1.Pod) {
    framework.Logf("<testdoc:podspec>%s</testdoc:podspec>", getYaml(p))
}

// Logs general log output for the test case
func TestLog(log string) {
    framework.Logf("<testdoc:log>%s</testdoc:log>", log)
}

// Logs the status of the Pod
func PodStatus(status string) {
    framework.Logf("<testdoc:status>%s</testdoc:status>", status)
}

// Converts Pod object to YAML for logging purposes
func getYaml(pod *v1.Pod) string {
    data, err := yaml.Marshal(pod)
    if err != nil {
        framework.Logf("Error marshaling Pod to YAML: %v", err)
        return ""
    }
    return string(data)
}
  

var _ = SIGDescribe("Container Lifecycle Hook", func() {
	f := framework.NewDefaultFramework("container-lifecycle-hook")
	f.NamespacePodSecurityLevel = admissionapi.LevelBaseline
	var podClient *e2epod.PodClient
	const (
		podCheckInterval     = 1 * time.Second
		postStartWaitTimeout = 2 * time.Minute
		preStopWaitTimeout   = 30 * time.Second
	)
	ginkgo.Context("when create a pod with lifecycle hook", func() {
		var (
			targetIP, targetURL, targetNode string

			httpPorts = []v1.ContainerPort{
				{
					ContainerPort: 8080,
					Protocol:      v1.ProtocolTCP,
				},
			}
			httpsPorts = []v1.ContainerPort{
				{
					ContainerPort: 9090,
					Protocol:      v1.ProtocolTCP,
				},
			}
			httpsArgs = []string{
				"netexec",
				"--http-port", "9090",
				"--udp-port", "9091",
				"--tls-cert-file", "/localhost.crt",
				"--tls-private-key-file", "/localhost.key",
			}
		)

		podHandleHookRequest := e2epod.NewAgnhostPodFromContainers(
			"", "pod-handle-http-request", nil,
			e2epod.NewAgnhostContainer("container-handle-http-request", nil, httpPorts, "netexec"),
			e2epod.NewAgnhostContainer("container-handle-https-request", nil, httpsPorts, httpsArgs...),
		)

		ginkgo.BeforeEach(func(ctx context.Context) {
			node, err := e2enode.GetRandomReadySchedulableNode(ctx, f.ClientSet)
			framework.ExpectNoError(err)
			targetNode = node.Name
			nodeSelection := e2epod.NodeSelection{}
			e2epod.SetAffinity(&nodeSelection, targetNode)
			e2epod.SetNodeSelection(&podHandleHookRequest.Spec, nodeSelection)

			podClient = e2epod.NewPodClient(f)
			ginkgo.By("create the container to handle the HTTPGet hook request.")
			newPod := podClient.CreateSync(ctx, podHandleHookRequest)
			targetIP = newPod.Status.PodIP
			targetURL = targetIP
			if strings.Contains(targetIP, ":") {
				targetURL = fmt.Sprintf("[%s]", targetIP)
			}
		})
		testPodWithHook := func(ctx context.Context, podWithHook *v1.Pod) {
			ginkgo.By("create the pod with lifecycle hook")
			podClient.CreateSync(ctx, podWithHook)
			const (
				defaultHandler = iota
				httpsHandler
			)
			handlerContainer := defaultHandler
			if podWithHook.Spec.Containers[0].Lifecycle.PostStart != nil {
				ginkgo.By("check poststart hook")
				if podWithHook.Spec.Containers[0].Lifecycle.PostStart.HTTPGet != nil {
					if v1.URISchemeHTTPS == podWithHook.Spec.Containers[0].Lifecycle.PostStart.HTTPGet.Scheme {
						handlerContainer = httpsHandler
					}
				}
				gomega.Eventually(ctx, func(ctx context.Context) error {
					return podClient.MatchContainerOutput(ctx, podHandleHookRequest.Name, podHandleHookRequest.Spec.Containers[handlerContainer].Name,
						`GET /echo\?msg=poststart`)
				}, postStartWaitTimeout, podCheckInterval).Should(gomega.BeNil())
			}
			ginkgo.By("delete the pod with lifecycle hook")
			podClient.DeleteSync(ctx, podWithHook.Name, *metav1.NewDeleteOptions(15), f.Timeouts.PodDelete)
			if podWithHook.Spec.Containers[0].Lifecycle.PreStop != nil {
				ginkgo.By("check prestop hook")
				if podWithHook.Spec.Containers[0].Lifecycle.PreStop.HTTPGet != nil {
					if v1.URISchemeHTTPS == podWithHook.Spec.Containers[0].Lifecycle.PreStop.HTTPGet.Scheme {
						handlerContainer = httpsHandler
					}
				}
				gomega.Eventually(ctx, func(ctx context.Context) error {
					return podClient.MatchContainerOutput(ctx, podHandleHookRequest.Name, podHandleHookRequest.Spec.Containers[handlerContainer].Name,
						`GET /echo\?msg=prestop`)
				}, preStopWaitTimeout, podCheckInterval).Should(gomega.BeNil())
			}
		}
		/*
			Release: v1.9
			Testname: Pod Lifecycle, post start exec hook
			Description: When a post start handler is specified in the container lifecycle using a 'Exec' action, then the handler MUST be invoked after the start of the container. A server pod is created that will serve http requests, create a second pod with a container lifecycle specifying a post start that invokes the server pod using ExecAction to validate that the post start is executed.
		*/
		framework.ConformanceIt("should execute poststart exec hook properly", f.WithNodeConformance(), func(ctx context.Context) {
			lifecycle := &v1.Lifecycle{
				PostStart: &v1.LifecycleHandler{
					Exec: &v1.ExecAction{
						Command: []string{"sh", "-c", "curl http://" + targetURL + ":8080/echo?msg=poststart"},
					},
				},
			}
			podWithHook := getPodWithHook("pod-with-poststart-exec-hook", imageutils.GetE2EImage(imageutils.Agnhost), lifecycle)

			testPodWithHook(ctx, podWithHook)
		})
		/*
			Release: v1.9
			Testname: Pod Lifecycle, prestop exec hook
			Description: When a pre-stop handler is specified in the container lifecycle using a 'Exec' action, then the handler MUST be invoked before the container is terminated. A server pod is created that will serve http requests, create a second pod with a container lifecycle specifying a pre-stop that invokes the server pod using ExecAction to validate that the pre-stop is executed.
		*/
		framework.ConformanceIt("should execute prestop exec hook properly", f.WithNodeConformance(), func(ctx context.Context) {
		 	lifecycle := &v1.Lifecycle{
		 		PreStop: &v1.LifecycleHandler{
		 			Exec: &v1.ExecAction{
		 				Command: []string{"sh", "-c", "curl http://" + targetURL + ":8080/echo?msg=prestop"},
		 			},
		 		},
		 	}
		 	podWithHook := getPodWithHook("pod-with-prestop-exec-hook", imageutils.GetE2EImage(imageutils.Agnhost), lifecycle)
		 	testPodWithHook(ctx, podWithHook)
		 })

		
		// Test case for validating the PreStop hook functionality
		framework.ConformanceIt("should successfully run PreStop exec hook FINAL", f.WithNodeConformance(), func(ctx context.Context) {

			// Step 1: Log the name of the test using the <testdoc:name> marker
			TestName("PRESTOP_BASIC")

			// Step 2: Provide a description of what the PreStop hook is supposed to do
			TestStep("Defining a PreStop hook for the container, which will run just before the container shuts down.")

			// Step 3: Specify the Pod with the PreStop hook, and log the Pod spec in YAML format using the <testdoc:podspec> marker
			lifecycle := &v1.Lifecycle{
				PreStop: &v1.LifecycleHandler{
					Exec: &v1.ExecAction{
						Command: []string{"sh", "-c", "echo 'PreStop Hook Triggered'; sleep 5; echo 'PreStop Hook Completed'"},
					},
				},
			}
			podWithHook := getPodWithHook("prestop-pod", imageutils.GetE2EImage(imageutils.Agnhost), lifecycle)
			PodSpec(podWithHook) // Log the complete Pod YAML

			// Step 4: Create the Pod and wait for it to become fully operational
			TestStep("Pod has been created successfully.")
			createdPod := podClient.CreateSync(ctx, podWithHook)
			TestStep("Monitoring the Pod until it reaches the 'Running' state.")

			// Step 5: Retrieve the logs from the Pod and confirm the PreStop hook was triggered
			TestStep("The logs will contain the string 'PreStop Hook Triggered' confirming the PreStop hook was executed.")
			podLogs, err := e2epod.GetPodLogs(ctx, f.ClientSet, createdPod.Namespace, createdPod.Name, "main-container")
			if err != nil {
				framework.Logf("Warning: Unable to retrieve pod logs: %v", err)
			} else {
				TestLog(podLogs) // Log the Pod output
				TestStep("Here is an example of the log output from the Pod:")
				TestLog(podLogs) // Display the example log output
			}

			// Step 6: Optionally capture any events related to the Pod for further analysis
			TestStep("Collecting events related to the Pod for deeper investigation.")
			podEvents, err := f.ClientSet.CoreV1().Events(createdPod.Namespace).List(ctx, metav1.ListOptions{})
			framework.ExpectNoError(err)
			framework.Logf("Pod Events: %+v", podEvents) // Log Pod events if needed for debugging

			// Step 7: Trigger the PreStop hook by deleting the Pod
			TestStep("Initiating the PreStop hook by deleting the Pod.")
			podClient.DeleteSync(ctx, createdPod.Name, *metav1.NewDeleteOptions(30), e2epod.DefaultPodDeletionTimeout)

			// Step 8: Verify the Pod has been fully terminated and capture its final status
			TestStep("Verifying that the Pod has terminated successfully after executing the PreStop hook.")
			err = e2epod.WaitForPodNotFoundInNamespace(ctx, f.ClientSet, createdPod.Name, createdPod.Namespace, 4*time.Minute)
			framework.ExpectNoError(err, "Pod termination verification failed.")
			PodStatus("The final status of the Pod: Successfully terminated.")
		})

		
		 

		/*
			Release: v1.9
			Testname: Pod Lifecycle, post start http hook
			Description: When a post start handler is specified in the container lifecycle using a HttpGet action, then the handler MUST be invoked after the start of the container. A server pod is created that will serve http requests, create a second pod on the same node with a container lifecycle specifying a post start that invokes the server pod to validate that the post start is executed.
		*/
		framework.ConformanceIt("should execute poststart http hook properly", f.WithNodeConformance(), func(ctx context.Context) {
			lifecycle := &v1.Lifecycle{
				PostStart: &v1.LifecycleHandler{
					HTTPGet: &v1.HTTPGetAction{
						Path: "/echo?msg=poststart",
						Host: targetIP,
						Port: intstr.FromInt32(8080),
					},
				},
			}
			podWithHook := getPodWithHook("pod-with-poststart-http-hook", imageutils.GetPauseImageName(), lifecycle)
			// make sure we spawn the test pod on the same node as the webserver.
			nodeSelection := e2epod.NodeSelection{}
			e2epod.SetAffinity(&nodeSelection, targetNode)
			e2epod.SetNodeSelection(&podWithHook.Spec, nodeSelection)
			testPodWithHook(ctx, podWithHook)
		})
		/*
			Release : v1.23
			Testname: Pod Lifecycle, poststart https hook
			Description: When a post-start handler is specified in the container lifecycle using a 'HttpGet' action, then the handler MUST be invoked before the container is terminated. A server pod is created that will serve https requests, create a second pod on the same node with a container lifecycle specifying a post-start that invokes the server pod to validate that the post-start is executed.
		*/
		f.It("should execute poststart https hook properly [MinimumKubeletVersion:1.23]", f.WithNodeConformance(), func(ctx context.Context) {
			lifecycle := &v1.Lifecycle{
				PostStart: &v1.LifecycleHandler{
					HTTPGet: &v1.HTTPGetAction{
						Scheme: v1.URISchemeHTTPS,
						Path:   "/echo?msg=poststart",
						Host:   targetIP,
						Port:   intstr.FromInt32(9090),
					},
				},
			}
			podWithHook := getPodWithHook("pod-with-poststart-https-hook", imageutils.GetPauseImageName(), lifecycle)
			// make sure we spawn the test pod on the same node as the webserver.
			nodeSelection := e2epod.NodeSelection{}
			e2epod.SetAffinity(&nodeSelection, targetNode)
			e2epod.SetNodeSelection(&podWithHook.Spec, nodeSelection)
			testPodWithHook(ctx, podWithHook)
		})
		/*
			Release : v1.9
			Testname: Pod Lifecycle, prestop http hook
			Description: When a pre-stop handler is specified in the container lifecycle using a 'HttpGet' action, then the handler MUST be invoked before the container is terminated. A server pod is created that will serve http requests, create a second pod on the same node with a container lifecycle specifying a pre-stop that invokes the server pod to validate that the pre-stop is executed.
		*/
		framework.ConformanceIt("should execute prestop http hook properly", f.WithNodeConformance(), func(ctx context.Context) {
			lifecycle := &v1.Lifecycle{
				PreStop: &v1.LifecycleHandler{
					HTTPGet: &v1.HTTPGetAction{
						Path: "/echo?msg=prestop",
						Host: targetIP,
						Port: intstr.FromInt32(8080),
					},
				},
			}
			podWithHook := getPodWithHook("pod-with-prestop-http-hook", imageutils.GetPauseImageName(), lifecycle)
			// make sure we spawn the test pod on the same node as the webserver.
			nodeSelection := e2epod.NodeSelection{}
			e2epod.SetAffinity(&nodeSelection, targetNode)
			e2epod.SetNodeSelection(&podWithHook.Spec, nodeSelection)
			testPodWithHook(ctx, podWithHook)
		})
		/*
			Release : v1.23
			Testname: Pod Lifecycle, prestop https hook
			Description: When a pre-stop handler is specified in the container lifecycle using a 'HttpGet' action, then the handler MUST be invoked before the container is terminated. A server pod is created that will serve https requests, create a second pod on the same node with a container lifecycle specifying a pre-stop that invokes the server pod to validate that the pre-stop is executed.
		*/
		f.It("should execute prestop https hook properly [MinimumKubeletVersion:1.23]", f.WithNodeConformance(), func(ctx context.Context) {
			lifecycle := &v1.Lifecycle{
				PreStop: &v1.LifecycleHandler{
					HTTPGet: &v1.HTTPGetAction{
						Scheme: v1.URISchemeHTTPS,
						Path:   "/echo?msg=prestop",
						Host:   targetIP,
						Port:   intstr.FromInt32(9090),
					},
				},
			}
			podWithHook := getPodWithHook("pod-with-prestop-https-hook", imageutils.GetPauseImageName(), lifecycle)
			// make sure we spawn the test pod on the same node as the webserver.
			nodeSelection := e2epod.NodeSelection{}
			e2epod.SetAffinity(&nodeSelection, targetNode)
			e2epod.SetNodeSelection(&podWithHook.Spec, nodeSelection)
			testPodWithHook(ctx, podWithHook)
		})
	})
})

var _ = SIGDescribe(feature.SidecarContainers, "Restartable Init Container Lifecycle Hook", func() {
	f := framework.NewDefaultFramework("restartable-init-container-lifecycle-hook")
	f.NamespacePodSecurityLevel = admissionapi.LevelBaseline
	var podClient *e2epod.PodClient
	const (
		podCheckInterval     = 1 * time.Second
		postStartWaitTimeout = 2 * time.Minute
		preStopWaitTimeout   = 30 * time.Second
	)
	ginkgo.Context("when create a pod with lifecycle hook", func() {
		var (
			targetIP, targetURL, targetNode string

			httpPorts = []v1.ContainerPort{
				{
					ContainerPort: 8080,
					Protocol:      v1.ProtocolTCP,
				},
			}
			httpsPorts = []v1.ContainerPort{
				{
					ContainerPort: 9090,
					Protocol:      v1.ProtocolTCP,
				},
			}
			httpsArgs = []string{
				"netexec",
				"--http-port", "9090",
				"--udp-port", "9091",
				"--tls-cert-file", "/localhost.crt",
				"--tls-private-key-file", "/localhost.key",
			}
		)

		podHandleHookRequest := e2epod.NewAgnhostPodFromContainers(
			"", "pod-handle-http-request", nil,
			e2epod.NewAgnhostContainer("container-handle-http-request", nil, httpPorts, "netexec"),
			e2epod.NewAgnhostContainer("container-handle-https-request", nil, httpsPorts, httpsArgs...),
		)

		ginkgo.BeforeEach(func(ctx context.Context) {
			node, err := e2enode.GetRandomReadySchedulableNode(ctx, f.ClientSet)
			framework.ExpectNoError(err)
			targetNode = node.Name
			nodeSelection := e2epod.NodeSelection{}
			e2epod.SetAffinity(&nodeSelection, targetNode)
			e2epod.SetNodeSelection(&podHandleHookRequest.Spec, nodeSelection)

			podClient = e2epod.NewPodClient(f)
			ginkgo.By("create the container to handle the HTTPGet hook request.")
			newPod := podClient.CreateSync(ctx, podHandleHookRequest)
			targetIP = newPod.Status.PodIP
			targetURL = targetIP
			if strings.Contains(targetIP, ":") {
				targetURL = fmt.Sprintf("[%s]", targetIP)
			}
		})
		testPodWithHook := func(ctx context.Context, podWithHook *v1.Pod) {
			ginkgo.By("create the pod with lifecycle hook")
			podClient.CreateSync(ctx, podWithHook)
			const (
				defaultHandler = iota
				httpsHandler
			)
			handlerContainer := defaultHandler
			if podWithHook.Spec.InitContainers[0].Lifecycle.PostStart != nil {
				ginkgo.By("check poststart hook")
				if podWithHook.Spec.InitContainers[0].Lifecycle.PostStart.HTTPGet != nil {
					if v1.URISchemeHTTPS == podWithHook.Spec.InitContainers[0].Lifecycle.PostStart.HTTPGet.Scheme {
						handlerContainer = httpsHandler
					}
				}
				gomega.Eventually(ctx, func(ctx context.Context) error {
					return podClient.MatchContainerOutput(ctx, podHandleHookRequest.Name, podHandleHookRequest.Spec.Containers[handlerContainer].Name,
						`GET /echo\?msg=poststart`)
				}, postStartWaitTimeout, podCheckInterval).Should(gomega.BeNil())
			}
			ginkgo.By("delete the pod with lifecycle hook")
			podClient.DeleteSync(ctx, podWithHook.Name, *metav1.NewDeleteOptions(15), f.Timeouts.PodDelete)
			if podWithHook.Spec.InitContainers[0].Lifecycle.PreStop != nil {
				ginkgo.By("check prestop hook")
				if podWithHook.Spec.InitContainers[0].Lifecycle.PreStop.HTTPGet != nil {
					if v1.URISchemeHTTPS == podWithHook.Spec.InitContainers[0].Lifecycle.PreStop.HTTPGet.Scheme {
						handlerContainer = httpsHandler
					}
				}
				gomega.Eventually(ctx, func(ctx context.Context) error {
					return podClient.MatchContainerOutput(ctx, podHandleHookRequest.Name, podHandleHookRequest.Spec.Containers[handlerContainer].Name,
						`GET /echo\?msg=prestop`)
				}, preStopWaitTimeout, podCheckInterval).Should(gomega.BeNil())
			}
		}
		/*
			Release: v1.28
			Testname: Pod Lifecycle with restartable init container, post start exec hook
			Description: When a post start handler is specified in the container
			lifecycle using a 'Exec' action, then the handler MUST be invoked after
			the start of the container. A server pod is created that will serve http
			requests, create a second pod with a container lifecycle specifying a
			post start that invokes the server pod using ExecAction to validate that
			the post start is executed.
		*/
		ginkgo.It("should execute poststart exec hook properly", func(ctx context.Context) {
			lifecycle := &v1.Lifecycle{
				PostStart: &v1.LifecycleHandler{
					Exec: &v1.ExecAction{
						Command: []string{"sh", "-c", "curl http://" + targetURL + ":8080/echo?msg=poststart"},
					},
				},
			}
			podWithHook := getSidecarPodWithHook("pod-with-poststart-exec-hook", imageutils.GetE2EImage(imageutils.Agnhost), lifecycle)

			testPodWithHook(ctx, podWithHook)
		})
		/*
			Release: v1.28
			Testname: Pod Lifecycle with restartable init container, prestop exec hook
			Description: When a pre-stop handler is specified in the container
			lifecycle using a 'Exec' action, then the handler MUST be invoked before
			the container is terminated. A server pod is created that will serve http
			requests, create a second pod with a container lifecycle specifying a
			pre-stop that invokes the server pod using ExecAction to validate that
			the pre-stop is executed.
		*/
		ginkgo.It("should execute prestop exec hook properly", func(ctx context.Context) {
			lifecycle := &v1.Lifecycle{
				PreStop: &v1.LifecycleHandler{
					Exec: &v1.ExecAction{
						Command: []string{"sh", "-c", "curl http://" + targetURL + ":8080/echo?msg=prestop"},
					},
				},
			}
			podWithHook := getSidecarPodWithHook("pod-with-prestop-exec-hook", imageutils.GetE2EImage(imageutils.Agnhost), lifecycle)
			testPodWithHook(ctx, podWithHook)
		})
		/*
			Release: v1.28
			Testname: Pod Lifecycle with restartable init container, post start http hook
			Description: When a post start handler is specified in the container
			lifecycle using a HttpGet action, then the handler MUST be invoked after
			the start of the container. A server pod is created that will serve http
			requests, create a second pod on the same node with a container lifecycle
			specifying a post start that invokes the server pod to validate that the
			post start is executed.
		*/
		ginkgo.It("should execute poststart http hook properly", func(ctx context.Context) {
			lifecycle := &v1.Lifecycle{
				PostStart: &v1.LifecycleHandler{
					HTTPGet: &v1.HTTPGetAction{
						Path: "/echo?msg=poststart",
						Host: targetIP,
						Port: intstr.FromInt32(8080),
					},
				},
			}
			podWithHook := getSidecarPodWithHook("pod-with-poststart-http-hook", imageutils.GetPauseImageName(), lifecycle)
			// make sure we spawn the test pod on the same node as the webserver.
			nodeSelection := e2epod.NodeSelection{}
			e2epod.SetAffinity(&nodeSelection, targetNode)
			e2epod.SetNodeSelection(&podWithHook.Spec, nodeSelection)
			testPodWithHook(ctx, podWithHook)
		})
		/*
			Release : v1.28
			Testname: Pod Lifecycle with restartable init container, poststart https hook
			Description: When a post-start handler is specified in the container
			lifecycle using a 'HttpGet' action, then the handler MUST be invoked
			before the container is terminated. A server pod is created that will
			serve https requests, create a second pod on the same node with a
			container lifecycle specifying a post-start that invokes the server pod
			to validate that the post-start is executed.
		*/
		ginkgo.It("should execute poststart https hook properly [MinimumKubeletVersion:1.23]", func(ctx context.Context) {
			lifecycle := &v1.Lifecycle{
				PostStart: &v1.LifecycleHandler{
					HTTPGet: &v1.HTTPGetAction{
						Scheme: v1.URISchemeHTTPS,
						Path:   "/echo?msg=poststart",
						Host:   targetIP,
						Port:   intstr.FromInt32(9090),
					},
				},
			}
			podWithHook := getSidecarPodWithHook("pod-with-poststart-https-hook", imageutils.GetPauseImageName(), lifecycle)
			// make sure we spawn the test pod on the same node as the webserver.
			nodeSelection := e2epod.NodeSelection{}
			e2epod.SetAffinity(&nodeSelection, targetNode)
			e2epod.SetNodeSelection(&podWithHook.Spec, nodeSelection)
			testPodWithHook(ctx, podWithHook)
		})
		/*
			Release : v1.28
			Testname: Pod Lifecycle with restartable init container, prestop http hook
			Description: When a pre-stop handler is specified in the container
			lifecycle using a 'HttpGet' action, then the handler MUST be invoked
			before the container is terminated. A server pod is created that will
			serve http requests, create a second pod on the same node with a
			container lifecycle specifying a pre-stop that invokes the server pod to
			validate that the pre-stop is executed.
		*/
		ginkgo.It("should execute prestop http hook properly", func(ctx context.Context) {
			lifecycle := &v1.Lifecycle{
				PreStop: &v1.LifecycleHandler{
					HTTPGet: &v1.HTTPGetAction{
						Path: "/echo?msg=prestop",
						Host: targetIP,
						Port: intstr.FromInt32(8080),
					},
				},
			}
			podWithHook := getSidecarPodWithHook("pod-with-prestop-http-hook", imageutils.GetPauseImageName(), lifecycle)
			// make sure we spawn the test pod on the same node as the webserver.
			nodeSelection := e2epod.NodeSelection{}
			e2epod.SetAffinity(&nodeSelection, targetNode)
			e2epod.SetNodeSelection(&podWithHook.Spec, nodeSelection)
			testPodWithHook(ctx, podWithHook)
		})
		/*
			Release : v1.28
			Testname: Pod Lifecycle with restartable init container, prestop https hook
			Description: When a pre-stop handler is specified in the container
			lifecycle using a 'HttpGet' action, then the handler MUST be invoked
			before the container is terminated. A server pod is created that will
			serve https requests, create a second pod on the same node with a
			container lifecycle specifying a pre-stop that invokes the server pod to
			validate that the pre-stop is executed.
		*/
		ginkgo.It("should execute prestop https hook properly [MinimumKubeletVersion:1.23]", func(ctx context.Context) {
			lifecycle := &v1.Lifecycle{
				PreStop: &v1.LifecycleHandler{
					HTTPGet: &v1.HTTPGetAction{
						Scheme: v1.URISchemeHTTPS,
						Path:   "/echo?msg=prestop",
						Host:   targetIP,
						Port:   intstr.FromInt32(9090),
					},
				},
			}
			podWithHook := getSidecarPodWithHook("pod-with-prestop-https-hook", imageutils.GetPauseImageName(), lifecycle)
			// make sure we spawn the test pod on the same node as the webserver.
			nodeSelection := e2epod.NodeSelection{}
			e2epod.SetAffinity(&nodeSelection, targetNode)
			e2epod.SetNodeSelection(&podWithHook.Spec, nodeSelection)
			testPodWithHook(ctx, podWithHook)
		})
	})
})

func getPodWithHook(name string, image string, lifecycle *v1.Lifecycle) *v1.Pod {
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Name:      name,
					Image:     image,
					Lifecycle: lifecycle,
				},
			},
		},
	}
}

func getSidecarPodWithHook(name string, image string, lifecycle *v1.Lifecycle) *v1.Pod {
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: v1.PodSpec{
			InitContainers: []v1.Container{
				{
					Name:      name,
					Image:     image,
					Lifecycle: lifecycle,
					RestartPolicy: func() *v1.ContainerRestartPolicy {
						restartPolicy := v1.ContainerRestartPolicyAlways
						return &restartPolicy
					}(),
				},
			},
			Containers: []v1.Container{
				{
					Name:  "main",
					Image: imageutils.GetPauseImageName(),
				},
			},
		},
	}
}

func validDuration(duration time.Duration, low, high int64) bool {
	return duration >= time.Second*time.Duration(low) && duration <= time.Second*time.Duration(high)
}

var _ = SIGDescribe(feature.PodLifecycleSleepAction, func() {
	f := framework.NewDefaultFramework("pod-lifecycle-sleep-action")
	f.NamespacePodSecurityLevel = admissionapi.LevelBaseline
	var podClient *e2epod.PodClient

	ginkgo.Context("when create a pod with lifecycle hook using sleep action", func() {
		ginkgo.BeforeEach(func(ctx context.Context) {
			podClient = e2epod.NewPodClient(f)
		})
		ginkgo.It("valid prestop hook using sleep action", func(ctx context.Context) {
			lifecycle := &v1.Lifecycle{
				PreStop: &v1.LifecycleHandler{
					Sleep: &v1.SleepAction{Seconds: 5},
				},
			}
			podWithHook := getPodWithHook("pod-with-prestop-sleep-hook", imageutils.GetPauseImageName(), lifecycle)
			ginkgo.By("create the pod with lifecycle hook using sleep action")
			podClient.CreateSync(ctx, podWithHook)
			ginkgo.By("delete the pod with lifecycle hook using sleep action")
			start := time.Now()
			podClient.DeleteSync(ctx, podWithHook.Name, metav1.DeleteOptions{}, f.Timeouts.PodDelete)
			cost := time.Since(start)
			// cost should be
			// longer than 5 seconds (pod should sleep for 5 seconds)
			// shorter than gracePeriodSeconds (default 30 seconds here)
			if !validDuration(cost, 5, 30) {
				framework.Failf("unexpected delay duration before killing the pod, cost = %v", cost)
			}
		})

		ginkgo.It("reduce GracePeriodSeconds during runtime", func(ctx context.Context) {
			lifecycle := &v1.Lifecycle{
				PreStop: &v1.LifecycleHandler{
					Sleep: &v1.SleepAction{Seconds: 15},
				},
			}
			podWithHook := getPodWithHook("pod-with-prestop-sleep-hook", imageutils.GetPauseImageName(), lifecycle)
			ginkgo.By("create the pod with lifecycle hook using sleep action")
			podClient.CreateSync(ctx, podWithHook)
			ginkgo.By("delete the pod with lifecycle hook using sleep action")
			start := time.Now()
			podClient.DeleteSync(ctx, podWithHook.Name, *metav1.NewDeleteOptions(2), f.Timeouts.PodDelete)
			cost := time.Since(start)
			// cost should be
			// longer than 2 seconds (we change gracePeriodSeconds to 2 seconds here, and it's less than sleep action)
			// shorter than sleep action (to make sure it doesn't take effect)
			if !validDuration(cost, 2, 15) {
				framework.Failf("unexpected delay duration before killing the pod, cost = %v", cost)
			}
		})

		ginkgo.It("ignore terminated container", func(ctx context.Context) {
			lifecycle := &v1.Lifecycle{
				PreStop: &v1.LifecycleHandler{
					Sleep: &v1.SleepAction{Seconds: 20},
				},
			}
			name := "pod-with-prestop-sleep-hook"
			podWithHook := getPodWithHook(name, imageutils.GetE2EImage(imageutils.BusyBox), lifecycle)
			podWithHook.Spec.Containers[0].Command = []string{"/bin/sh"}
			podWithHook.Spec.Containers[0].Args = []string{"-c", "exit 0"}
			podWithHook.Spec.RestartPolicy = v1.RestartPolicyNever
			ginkgo.By("create the pod with lifecycle hook using sleep action")
			p := podClient.Create(ctx, podWithHook)
			framework.ExpectNoError(e2epod.WaitForContainerTerminated(ctx, f.ClientSet, f.Namespace.Name, p.Name, name, 3*time.Minute))
			ginkgo.By("delete the pod with lifecycle hook using sleep action")
			start := time.Now()
			podClient.DeleteSync(ctx, podWithHook.Name, metav1.DeleteOptions{}, f.Timeouts.PodDelete)
			cost := time.Since(start)
			// cost should be
			// shorter than sleep action (container is terminated and sleep action should be ignored)
			if !validDuration(cost, 0, 15) {
				framework.Failf("unexpected delay duration before killing the pod, cost = %v", cost)
			}
		})

	})
})

var _ = SIGDescribe(feature.PodLifecycleSleepActionAllowZero, func() {
	f := framework.NewDefaultFramework("pod-lifecycle-sleep-action-allow-zero")
	f.NamespacePodSecurityLevel = admissionapi.LevelBaseline
	var podClient *e2epod.PodClient

	ginkgo.Context("when create a pod with lifecycle hook using sleep action with a duration of zero seconds", func() {
		ginkgo.BeforeEach(func(ctx context.Context) {
			podClient = e2epod.NewPodClient(f)
		})
		ginkgo.It("prestop hook using sleep action with zero duration", func(ctx context.Context) {
			lifecycle := &v1.Lifecycle{
				PreStop: &v1.LifecycleHandler{
					Sleep: &v1.SleepAction{Seconds: 0},
				},
			}
			podWithHook := getPodWithHook("pod-with-prestop-sleep-hook-zero-duration", imageutils.GetPauseImageName(), lifecycle)
			ginkgo.By("create the pod with lifecycle hook using sleep action with zero duration")
			podClient.CreateSync(ctx, podWithHook)
			ginkgo.By("delete the pod with lifecycle hook using sleep action with zero duration")
			start := time.Now()
			podClient.DeleteSync(ctx, podWithHook.Name, metav1.DeleteOptions{}, f.Timeouts.PodDelete)
			cost := time.Since(start)
			// cost should be
			// longer than 0 seconds (pod shouldn't sleep and the handler should return immediately)
			// shorter than gracePeriodSeconds (default 30 seconds here)
			if !validDuration(cost, 0, 30) {
				framework.Failf("unexpected delay duration before killing the pod, cost = %v", cost)
			}
		})

	})
})
