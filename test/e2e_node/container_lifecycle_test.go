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

package e2enode

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
	admissionapi "k8s.io/pod-security-admission/api"

	"k8s.io/kubernetes/test/e2e/feature"
	"k8s.io/kubernetes/test/e2e/framework"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	imageutils "k8s.io/kubernetes/test/utils/image"
	"k8s.io/utils/ptr"
)

const (
	LivenessPrefix  = "Liveness"
	PostStartPrefix = "PostStart"
	PreStopPrefix   = "PreStop"
	ReadinessPrefix = "Readiness"
	StartupPrefix   = "Startup"
)

var containerRestartPolicyAlways = v1.ContainerRestartPolicyAlways

func prefixedName(namePrefix string, name string) string {
	return fmt.Sprintf("%s-%s", namePrefix, name)
}

type podTerminationContainerStatus struct {
	exitCode int32
	reason   string
}

func expectPodTerminationContainerStatuses(statuses []v1.ContainerStatus, to map[string]podTerminationContainerStatus) {
	ginkgo.GinkgoHelper()

	if len(statuses) != len(to) {
		ginkgo.Fail(fmt.Sprintf("mismatched lengths in pod termination container statuses. got %d, expected %d", len(statuses), len(to)))
	}
	for _, status := range statuses {
		expected, ok := to[status.Name]
		if !ok {
			ginkgo.Fail(fmt.Sprintf("container %q not found in expected pod termination container statuses", status.Name))
		}
		gomega.Expect(status.State.Terminated).NotTo(gomega.BeNil())
		gomega.Expect(status.State.Terminated.ExitCode).To(gomega.Equal(expected.exitCode))
		gomega.Expect(status.State.Terminated.Reason).To(gomega.Equal(expected.reason))
	}
}

var _ = SIGDescribe(framework.WithNodeConformance(), "Containers Lifecycle", func() {
	f := framework.NewDefaultFramework("containers-lifecycle-test")
	addAfterEachForCleaningUpPods(f)
	f.NamespacePodSecurityLevel = admissionapi.LevelPrivileged

	ginkgo.When("Running a pod with init containers and regular containers, restartPolicy=Never", func() {
		ginkgo.When("A pod initializes successfully", func() {
			ginkgo.It("should launch init container serially before a regular container", func(ctx context.Context) {

				init1 := "init-1"
				init2 := "init-2"
				init3 := "init-3"
				regular1 := "regular-1"

				podSpec := &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name: "initcontainer-test-pod",
					},
					Spec: v1.PodSpec{
						RestartPolicy: v1.RestartPolicyNever,
						InitContainers: []v1.Container{
							{
								Name:  init1,
								Image: busyboxImage,
								Command: ExecCommand(init1, execCommand{
									Delay:    1,
									ExitCode: 0,
								}),
							},
							{
								Name:  init2,
								Image: busyboxImage,
								Command: ExecCommand(init2, execCommand{
									Delay:    1,
									ExitCode: 0,
								}),
							},
							{
								Name:  init3,
								Image: busyboxImage,
								Command: ExecCommand(init3, execCommand{
									Delay:    1,
									ExitCode: 0,
								}),
							},
						},
						Containers: []v1.Container{
							{
								Name:  regular1,
								Image: busyboxImage,
								Command: ExecCommand(regular1, execCommand{
									StartDelay: 5,
									Delay:      1,
									ExitCode:   0,
								}),
								StartupProbe: &v1.Probe{
									ProbeHandler: v1.ProbeHandler{
										Exec: &v1.ExecAction{
											Command: []string{
												"test",
												"-f",
												"started",
											},
										},
									},
								},
							},
						},
					},
				}

				preparePod(podSpec)

				/// generates an out file output like:
				//
				// 1682076093 4905.79 init-1 Starting 0
				// 1682076093 4905.80 init-1 Started
				// 1682076093 4905.80 init-1 Delaying 1
				// 1682076094 4906.80 init-1 Exiting
				// 1682076095 4907.70 init-2 Starting 0
				// 1682076095 4907.71 init-2 Started
				// 1682076095 4907.71 init-2 Delaying 1
				// 1682076096 4908.71 init-2 Exiting
				// 1682076097 4909.74 init-3 Starting 0
				// 1682076097 4909.74 init-3 Started
				// 1682076097 4909.74 init-3 Delaying 1
				// 1682076098 4910.75 init-3 Exiting
				// 1682076099 4911.70 regular-1 Starting 5
				// 1682076104 4916.71 regular-1 Started
				// 1682076104 4916.71 regular-1 Delaying 1
				// 1682076105 4917.72 regular-1 Exiting

				client := e2epod.NewPodClient(f)
				podSpec = client.Create(ctx, podSpec)
				ginkgo.By("Waiting for the pod to finish")
				err := e2epod.WaitTimeoutForPodNoLongerRunningInNamespace(ctx, f.ClientSet, podSpec.Name, podSpec.Namespace, 1*time.Minute)
				framework.ExpectNoError(err)

				ginkgo.By("Parsing results")
				podSpec, err = client.Get(context.Background(), podSpec.Name, metav1.GetOptions{})
				framework.ExpectNoError(err)
				results := parseOutput(ctx, f, podSpec)

				// which we then use to make assertions regarding container ordering
				ginkgo.By("Analyzing results")
				framework.ExpectNoError(results.StartsBefore(init1, init2))
				framework.ExpectNoError(results.ExitsBefore(init1, init2))

				framework.ExpectNoError(results.StartsBefore(init2, init3))
				framework.ExpectNoError(results.ExitsBefore(init2, init3))

				framework.ExpectNoError(results.StartsBefore(init3, regular1))
				framework.ExpectNoError(results.ExitsBefore(init3, regular1))
			})
		})

		ginkgo.When("an init container fails", func() {
			ginkgo.It("should not launch regular containers if an init container fails", func(ctx context.Context) {

				init1 := "init-1"
				regular1 := "regular-1"

				podSpec := &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name: "initcontainer-test-pod-failure",
					},
					Spec: v1.PodSpec{
						RestartPolicy: v1.RestartPolicyNever,
						InitContainers: []v1.Container{
							{
								Name:  init1,
								Image: busyboxImage,
								Command: ExecCommand(init1, execCommand{
									Delay:    1,
									ExitCode: 1,
								}),
							},
						},
						Containers: []v1.Container{
							{
								Name:  regular1,
								Image: busyboxImage,
								Command: ExecCommand(regular1, execCommand{
									Delay:    1,
									ExitCode: 0,
								}),
							},
						},
					},
				}

				preparePod(podSpec)

				client := e2epod.NewPodClient(f)
				podSpec = client.Create(ctx, podSpec)
				ginkgo.By("Waiting for the pod to fail")
				err := e2epod.WaitForPodFailedReason(ctx, f.ClientSet, podSpec, "", 1*time.Minute)
				framework.ExpectNoError(err)

				ginkgo.By("Parsing results")
				podSpec, err = client.Get(context.Background(), podSpec.Name, metav1.GetOptions{})
				framework.ExpectNoError(err)
				results := parseOutput(ctx, f, podSpec)

				ginkgo.By("Analyzing results")
				// init container should start and exit with an error, and the regular container should never start
				framework.ExpectNoError(results.Starts(init1))
				framework.ExpectNoError(results.Exits(init1))

				framework.ExpectNoError(results.DoesntStart(regular1))
			})
		})

		ginkgo.When("The regular container has a PostStart hook", func() {
			ginkgo.It("should run Init container to completion before call to PostStart of regular container", func(ctx context.Context) {
				init1 := "init-1"
				regular1 := "regular-1"

				podSpec := &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name: "initcontainer-test-pod-with-post-start",
					},
					Spec: v1.PodSpec{
						RestartPolicy: v1.RestartPolicyNever,
						InitContainers: []v1.Container{
							{
								Name:  init1,
								Image: busyboxImage,
								Command: ExecCommand(init1, execCommand{
									Delay:    1,
									ExitCode: 0,
								}),
							},
						},
						Containers: []v1.Container{
							{
								Name:  regular1,
								Image: busyboxImage,
								Command: ExecCommand(regular1, execCommand{
									// Allocate sufficient time for its postStart hook
									// to complete.
									// Note that we've observed approximately a 2s
									// delay before the postStart hook is called.
									// 10s > 1s + 2s(estimated maximum delay) + other possible delays
									Delay:    10,
									ExitCode: 0,
								}),
								Lifecycle: &v1.Lifecycle{
									PostStart: &v1.LifecycleHandler{
										Exec: &v1.ExecAction{
											Command: ExecCommand(prefixedName(PostStartPrefix, regular1), execCommand{
												Delay:         1,
												ExitCode:      0,
												ContainerName: regular1,
											}),
										},
									},
								},
							},
						},
					},
				}

				preparePod(podSpec)

				client := e2epod.NewPodClient(f)
				podSpec = client.Create(ctx, podSpec)
				ginkgo.By("Waiting for the pod to finish")
				err := e2epod.WaitTimeoutForPodNoLongerRunningInNamespace(ctx, f.ClientSet, podSpec.Name, podSpec.Namespace, 1*time.Minute)
				framework.ExpectNoError(err)

				ginkgo.By("Parsing results")
				podSpec, err = client.Get(context.Background(), podSpec.Name, metav1.GetOptions{})
				framework.ExpectNoError(err)
				results := parseOutput(ctx, f, podSpec)

				ginkgo.By("Analyzing results")
				// init container should start and exit with an error, and the regular container should never start
				framework.ExpectNoError(results.StartsBefore(init1, prefixedName(PostStartPrefix, regular1)))
				framework.ExpectNoError(results.ExitsBefore(init1, prefixedName(PostStartPrefix, regular1)))

				framework.ExpectNoError(results.RunTogether(regular1, prefixedName(PostStartPrefix, regular1)))
			})
		})

		ginkgo.When("running a Pod with a failed regular container", func() {
			ginkgo.It("should restart failing container when pod restartPolicy is Always", func(ctx context.Context) {

				regular1 := "regular-1"

				podSpec := &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name: "container-must-be-restarted",
					},
					Spec: v1.PodSpec{
						RestartPolicy: v1.RestartPolicyAlways,
						Containers: []v1.Container{
							{
								Name:  regular1,
								Image: busyboxImage,
								Command: ExecCommand(regular1, execCommand{
									Delay:    1,
									ExitCode: 1,
								}),
							},
						},
					},
				}

				preparePod(podSpec)

				client := e2epod.NewPodClient(f)
				podSpec = client.Create(ctx, podSpec)
				ginkgo.By("Waiting for the pod, it will not finish")
				err := WaitForPodContainerRestartCount(ctx, f.ClientSet, podSpec.Namespace, podSpec.Name, 0, 3, 2*time.Minute)
				framework.ExpectNoError(err)

				ginkgo.By("Parsing results")
				podSpec, err = client.Get(context.Background(), podSpec.Name, metav1.GetOptions{})
				framework.ExpectNoError(err)
				results := parseOutput(ctx, f, podSpec)

				ginkgo.By("Analyzing results")
				// container must be restarted
				framework.ExpectNoError(results.Starts(regular1))
				framework.ExpectNoError(results.StartsBefore(regular1, regular1))
				framework.ExpectNoError(results.ExitsBefore(regular1, regular1))
			})
		})

		ginkgo.When("Running a pod with multiple containers and a PostStart hook", func() {
			ginkgo.It("should not launch second container before PostStart of the first container completed", func(ctx context.Context) {

				regular1 := "regular-1"
				regular2 := "regular-2"

				podSpec := &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name: "post-start-blocks-second-container",
					},
					Spec: v1.PodSpec{
						RestartPolicy: v1.RestartPolicyNever,
						Containers: []v1.Container{
							{
								Name:  regular1,
								Image: busyboxImage,
								Command: ExecCommand(regular1, execCommand{
									// Allocate sufficient time for its postStart hook
									// to complete.
									// Note that we've observed approximately a 2s
									// delay before the postStart hook is called.
									// 10s > 1s + 2s(estimated maximum delay) + other possible delays
									Delay:    10,
									ExitCode: 0,
								}),
								Lifecycle: &v1.Lifecycle{
									PostStart: &v1.LifecycleHandler{
										Exec: &v1.ExecAction{
											Command: ExecCommand(prefixedName(PostStartPrefix, regular1), execCommand{
												Delay:         1,
												ExitCode:      0,
												ContainerName: regular1,
											}),
										},
									},
								},
							},
							{
								Name:  regular2,
								Image: busyboxImage,
								Command: ExecCommand(regular2, execCommand{
									Delay:    1,
									ExitCode: 0,
								}),
							},
						},
					},
				}

				preparePod(podSpec)

				client := e2epod.NewPodClient(f)
				podSpec = client.Create(ctx, podSpec)
				ginkgo.By("Waiting for the pod to finish")
				err := e2epod.WaitTimeoutForPodNoLongerRunningInNamespace(ctx, f.ClientSet, podSpec.Name, podSpec.Namespace, 1*time.Minute)
				framework.ExpectNoError(err)

				ginkgo.By("Parsing results")
				podSpec, err = client.Get(context.Background(), podSpec.Name, metav1.GetOptions{})
				framework.ExpectNoError(err)
				results := parseOutput(ctx, f, podSpec)

				ginkgo.By("Analyzing results")
				// second container should not start before the PostStart of a first container completed
				framework.ExpectNoError(results.StartsBefore(prefixedName(PostStartPrefix, regular1), regular2))
				framework.ExpectNoError(results.ExitsBefore(prefixedName(PostStartPrefix, regular1), regular2))
			})
		})

		ginkgo.When("have init container in a Pod with restartPolicy=Never", func() {
			ginkgo.When("an init container fails to start because of a bad image", ginkgo.Ordered, func() {

				init1 := "init1-1"
				regular1 := "regular-1"

				podSpec := &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name: "bad-image",
					},
					Spec: v1.PodSpec{
						RestartPolicy: v1.RestartPolicyNever,
						InitContainers: []v1.Container{
							{
								Name:  init1,
								Image: imageutils.GetE2EImage(imageutils.InvalidRegistryImage),
								Command: ExecCommand(init1, execCommand{
									Delay:    600,
									ExitCode: 0,
								}),
							},
						},
						Containers: []v1.Container{
							{
								Name:  regular1,
								Image: busyboxImage,
								Command: ExecCommand(regular1, execCommand{
									Delay:    1,
									ExitCode: 0,
								}),
							},
						},
					},
				}

				preparePod(podSpec)
				var results containerOutputList

				ginkgo.It("should mark a Pod as failed and produce log", func(ctx context.Context) {
					client := e2epod.NewPodClient(f)
					podSpec = client.Create(ctx, podSpec)

					err := WaitForPodInitContainerToFail(ctx, f.ClientSet, podSpec.Namespace, podSpec.Name, 0, "ImagePullBackOff", f.Timeouts.PodStart)
					framework.ExpectNoError(err)

					podSpec, err = client.Get(context.Background(), podSpec.Name, metav1.GetOptions{})
					framework.ExpectNoError(err)
					results = parseOutput(ctx, f, podSpec)
				})
				ginkgo.It("should not start an init container", func() {
					framework.ExpectNoError(results.DoesntStart(init1))
				})
				ginkgo.It("should not start a regular container", func() {
					framework.ExpectNoError(results.DoesntStart(regular1))
				})
			})
		})

		ginkgo.When("A regular container restarts with init containers", func() {
			ginkgo.It("shouldn't restart init containers upon regular container restart", func(ctx context.Context) {
				init1 := "init-1"
				init2 := "init-2"
				init3 := "init-3"
				regular1 := "regular-1"

				podSpec := &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name: "initcontainer-test-pod",
					},
					Spec: v1.PodSpec{
						RestartPolicy: v1.RestartPolicyAlways,
						InitContainers: []v1.Container{
							{
								Name:  init1,
								Image: busyboxImage,
								Command: ExecCommand(init1, execCommand{
									Delay:    1,
									ExitCode: 0,
								}),
							},
							{
								Name:  init2,
								Image: busyboxImage,
								Command: ExecCommand(init2, execCommand{
									Delay:    1,
									ExitCode: 0,
								}),
							},
							{
								Name:  init3,
								Image: busyboxImage,
								Command: ExecCommand(init3, execCommand{
									Delay:    1,
									ExitCode: 0,
								}),
							},
						},
						Containers: []v1.Container{
							{
								Name:  regular1,
								Image: busyboxImage,
								Command: ExecCommand(regular1, execCommand{
									Delay:    10,
									ExitCode: -1,
								}),
							},
						},
					},
				}

				preparePod(podSpec)

				client := e2epod.NewPodClient(f)
				podSpec = client.Create(ctx, podSpec)
				ginkgo.By("Waiting for the pod to restart a few times")
				err := WaitForPodContainerRestartCount(ctx, f.ClientSet, podSpec.Namespace, podSpec.Name, 0, 3, 2*time.Minute)
				framework.ExpectNoError(err)

				ginkgo.By("Parsing results")
				podSpec, err = client.Get(context.Background(), podSpec.Name, metav1.GetOptions{})
				framework.ExpectNoError(err)
				results := parseOutput(ctx, f, podSpec)

				ginkgo.By("Analyzing results")
				framework.ExpectNoError(results.StartsBefore(init1, init2))
				framework.ExpectNoError(results.ExitsBefore(init1, init2))

				framework.ExpectNoError(results.StartsBefore(init2, init3))
				framework.ExpectNoError(results.ExitsBefore(init2, init3))

				framework.ExpectNoError(results.StartsBefore(init3, regular1))
				framework.ExpectNoError(results.ExitsBefore(init3, regular1))

				// ensure that the init containers never restarted
				framework.ExpectNoError(results.HasNotRestarted(init1))
				framework.ExpectNoError(results.HasNotRestarted(init2))
				framework.ExpectNoError(results.HasNotRestarted(init3))
				// while the regular container did
				framework.ExpectNoError(results.HasRestarted(regular1))
			})
		})

		ginkgo.When("a pod cannot terminate gracefully", func() {
			testPod := func(name string, gracePeriod int64) *v1.Pod {
				return &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name: name,
					},
					Spec: v1.PodSpec{
						Containers: []v1.Container{
							{
								Name:  "busybox",
								Image: imageutils.GetE2EImage(imageutils.BusyBox),
								Command: []string{
									"sleep",
									"10000",
								},
							},
						},
						TerminationGracePeriodSeconds: &gracePeriod,
					},
				}
			}

			// To account for the time it takes to delete the pod, we add a buffer. Its sized
			// so that we allow up to 2x the grace time to delete the pod. Its extra large to
			// reduce test flakes.
			bufferSeconds := int64(30)

			f.It("should respect termination grace period seconds", f.WithNodeConformance(), func(ctx context.Context) {
				client := e2epod.NewPodClient(f)
				gracePeriod := int64(30)

				ginkgo.By("creating a pod with a termination grace period seconds")
				pod := testPod("pod-termination-grace-period", gracePeriod)
				pod = client.Create(ctx, pod)

				ginkgo.By("ensuring the pod is running")
				err := e2epod.WaitForPodRunningInNamespace(ctx, f.ClientSet, pod)
				framework.ExpectNoError(err)

				ginkgo.By("deleting the pod gracefully")
				err = client.Delete(ctx, pod.Name, metav1.DeleteOptions{})
				framework.ExpectNoError(err)

				ginkgo.By("ensuring the pod is terminated within the grace period seconds + buffer seconds")
				err = e2epod.WaitForPodNotFoundInNamespace(ctx, f.ClientSet, pod.Name, pod.Namespace, time.Duration(gracePeriod+bufferSeconds)*time.Second)
				framework.ExpectNoError(err)
			})

			f.It("should respect termination grace period seconds with long-running preStop hook", f.WithNodeConformance(), func(ctx context.Context) {
				client := e2epod.NewPodClient(f)
				gracePeriod := int64(30)

				ginkgo.By("creating a pod with a termination grace period seconds and long-running preStop hook")
				pod := testPod("pod-termination-grace-period", gracePeriod)
				pod.Spec.Containers[0].Lifecycle = &v1.Lifecycle{
					PreStop: &v1.LifecycleHandler{
						Exec: &v1.ExecAction{
							Command: []string{
								"sleep",
								"10000",
							},
						},
					},
				}
				pod = client.Create(ctx, pod)

				ginkgo.By("ensuring the pod is running")
				err := e2epod.WaitForPodRunningInNamespace(ctx, f.ClientSet, pod)
				framework.ExpectNoError(err)

				ginkgo.By("deleting the pod gracefully")
				err = client.Delete(ctx, pod.Name, metav1.DeleteOptions{})
				framework.ExpectNoError(err)

				ginkgo.By("ensuring the pod is terminated within the grace period seconds + buffer seconds")
				err = e2epod.WaitForPodNotFoundInNamespace(ctx, f.ClientSet, pod.Name, pod.Namespace, time.Duration(gracePeriod+bufferSeconds)*time.Second)
				framework.ExpectNoError(err)
			})
		})

		ginkgo.When("A regular container has a PreStop hook", func() {
			ginkgo.When("A regular container fails a startup probe", func() {
				ginkgo.It("should call the container's preStop hook and terminate it if its startup probe fails", func(ctx context.Context) {
					regular1 := "regular-1"

					podSpec := &v1.Pod{
						ObjectMeta: metav1.ObjectMeta{
							Name: "test-pod",
						},
						Spec: v1.PodSpec{
							RestartPolicy: v1.RestartPolicyNever,
							Containers: []v1.Container{
								{
									Name:  regular1,
									Image: busyboxImage,
									Command: ExecCommand(regular1, execCommand{
										Delay:              100,
										TerminationSeconds: 15,
										ExitCode:           0,
									}),
									StartupProbe: &v1.Probe{
										ProbeHandler: v1.ProbeHandler{
											Exec: &v1.ExecAction{
												Command: []string{
													"sh",
													"-c",
													"exit 1",
												},
											},
										},
										InitialDelaySeconds: 10,
										FailureThreshold:    1,
									},
									Lifecycle: &v1.Lifecycle{
										PreStop: &v1.LifecycleHandler{
											Exec: &v1.ExecAction{
												Command: ExecCommand(prefixedName(PreStopPrefix, regular1), execCommand{
													Delay:         1,
													ExitCode:      0,
													ContainerName: regular1,
												}),
											},
										},
									},
								},
							},
						},
					}

					preparePod(podSpec)

					client := e2epod.NewPodClient(f)
					podSpec = client.Create(ctx, podSpec)

					ginkgo.By("Waiting for the pod to complete")
					err := e2epod.WaitForPodNoLongerRunningInNamespace(ctx, f.ClientSet, podSpec.Name, podSpec.Namespace)
					framework.ExpectNoError(err)

					ginkgo.By("Parsing results")
					podSpec, err = client.Get(ctx, podSpec.Name, metav1.GetOptions{})
					framework.ExpectNoError(err)
					results := parseOutput(ctx, f, podSpec)

					ginkgo.By("Analyzing results")
					framework.ExpectNoError(results.RunTogether(regular1, prefixedName(PreStopPrefix, regular1)))
					framework.ExpectNoError(results.Starts(prefixedName(PreStopPrefix, regular1)))
					framework.ExpectNoError(results.Exits(regular1))
				})
			})

			ginkgo.When("A regular container fails a liveness probe", func() {
				ginkgo.It("should call the container's preStop hook and terminate it if its liveness probe fails", func(ctx context.Context) {
					regular1 := "regular-1"

					podSpec := &v1.Pod{
						ObjectMeta: metav1.ObjectMeta{
							Name: "test-pod",
						},
						Spec: v1.PodSpec{
							RestartPolicy: v1.RestartPolicyNever,
							Containers: []v1.Container{
								{
									Name:  regular1,
									Image: busyboxImage,
									Command: ExecCommand(regular1, execCommand{
										Delay:              100,
										TerminationSeconds: 15,
										ExitCode:           0,
									}),
									LivenessProbe: &v1.Probe{
										ProbeHandler: v1.ProbeHandler{
											Exec: &v1.ExecAction{
												Command: []string{
													"sh",
													"-c",
													"exit 1",
												},
											},
										},
										InitialDelaySeconds: 10,
										FailureThreshold:    1,
									},
									Lifecycle: &v1.Lifecycle{
										PreStop: &v1.LifecycleHandler{
											Exec: &v1.ExecAction{
												Command: ExecCommand(prefixedName(PreStopPrefix, regular1), execCommand{
													Delay:         1,
													ExitCode:      0,
													ContainerName: regular1,
												}),
											},
										},
									},
								},
							},
						},
					}

					preparePod(podSpec)

					client := e2epod.NewPodClient(f)
					podSpec = client.Create(ctx, podSpec)

					ginkgo.By("Waiting for the pod to complete")
					err := e2epod.WaitForPodNoLongerRunningInNamespace(ctx, f.ClientSet, podSpec.Name, podSpec.Namespace)
					framework.ExpectNoError(err)

					ginkgo.By("Parsing results")
					podSpec, err = client.Get(ctx, podSpec.Name, metav1.GetOptions{})
					framework.ExpectNoError(err)
					results := parseOutput(ctx, f, podSpec)

					ginkgo.By("Analyzing results")
					framework.ExpectNoError(results.RunTogether(regular1, prefixedName(PreStopPrefix, regular1)))
					framework.ExpectNoError(results.Starts(prefixedName(PreStopPrefix, regular1)))
					framework.ExpectNoError(results.Exits(regular1))
				})

				ginkgo.When("a pod is terminating because its liveness probe fails", func() {
					regular1 := "regular-1"

					testPod := func() *v1.Pod {
						return &v1.Pod{
							ObjectMeta: metav1.ObjectMeta{
								Name: "test-pod",
							},
							Spec: v1.PodSpec{
								RestartPolicy:                 v1.RestartPolicyNever,
								TerminationGracePeriodSeconds: ptr.To(int64(100)),
								Containers: []v1.Container{
									{
										Name:  regular1,
										Image: imageutils.GetE2EImage(imageutils.BusyBox),
										Command: ExecCommand(regular1, execCommand{
											Delay:              100,
											TerminationSeconds: 15,
											ExitCode:           0,
										}),
										LivenessProbe: &v1.Probe{
											ProbeHandler: v1.ProbeHandler{
												Exec: &v1.ExecAction{
													Command: ExecCommand(prefixedName(LivenessPrefix, regular1), execCommand{
														ExitCode:      1,
														ContainerName: regular1,
													}),
												},
											},
											InitialDelaySeconds: 10,
											PeriodSeconds:       1,
											FailureThreshold:    1,
										},
									},
								},
							},
						}
					}

					f.It("should execute readiness probe while in preStop, but not liveness", f.WithNodeConformance(), func(ctx context.Context) {
						client := e2epod.NewPodClient(f)
						podSpec := testPod()

						ginkgo.By("creating a pod with a readiness probe and a preStop hook")
						podSpec.Spec.Containers[0].Lifecycle = &v1.Lifecycle{
							PreStop: &v1.LifecycleHandler{
								Exec: &v1.ExecAction{
									Command: ExecCommand(prefixedName(PreStopPrefix, regular1), execCommand{
										Delay:         10,
										ExitCode:      0,
										ContainerName: regular1,
									}),
								},
							},
						}
						podSpec.Spec.Containers[0].ReadinessProbe = &v1.Probe{
							ProbeHandler: v1.ProbeHandler{
								Exec: &v1.ExecAction{
									Command: ExecCommand(prefixedName(ReadinessPrefix, regular1), execCommand{
										ExitCode:      0,
										ContainerName: regular1,
									}),
								},
							},
							InitialDelaySeconds: 1,
							PeriodSeconds:       1,
						}

						preparePod(podSpec)

						podSpec = client.Create(ctx, podSpec)

						ginkgo.By("Waiting for the pod to complete")
						err := e2epod.WaitForPodNoLongerRunningInNamespace(ctx, f.ClientSet, podSpec.Name, podSpec.Namespace)
						framework.ExpectNoError(err)

						ginkgo.By("Parsing results")
						podSpec, err = client.Get(ctx, podSpec.Name, metav1.GetOptions{})
						framework.ExpectNoError(err)
						results := parseOutput(ctx, f, podSpec)

						ginkgo.By("Analyzing results")
						// readiness probes are called during pod termination
						framework.ExpectNoError(results.RunTogetherLhsFirst(prefixedName(PreStopPrefix, regular1), prefixedName(ReadinessPrefix, regular1)))
						// liveness probes are not called during pod termination
						err = results.RunTogetherLhsFirst(prefixedName(PreStopPrefix, regular1), prefixedName(LivenessPrefix, regular1))
						gomega.Expect(err).To(gomega.HaveOccurred())
					})

					f.It("should not continue running liveness probes for restartable init containers and restart them while in preStop", f.WithNodeConformance(), func(ctx context.Context) {
						client := e2epod.NewPodClient(f)
						podSpec := testPod()
						restartableInit1 := "restartable-init-1"

						ginkgo.By("creating a pod with a restartable init container and a preStop hook")
						podSpec.Spec.InitContainers = []v1.Container{{
							RestartPolicy: &containerRestartPolicyAlways,
							Name:          restartableInit1,
							Image:         imageutils.GetE2EImage(imageutils.BusyBox),
							Command: ExecCommand(restartableInit1, execCommand{
								Delay:              100,
								TerminationSeconds: 1,
								ExitCode:           0,
							}),
							LivenessProbe: &v1.Probe{
								ProbeHandler: v1.ProbeHandler{
									Exec: &v1.ExecAction{
										Command: ExecCommand(prefixedName(LivenessPrefix, restartableInit1), execCommand{
											ExitCode:      1,
											ContainerName: restartableInit1,
										}),
									},
								},
								InitialDelaySeconds: 1,
								PeriodSeconds:       1,
								FailureThreshold:    1,
							},
						}}
						podSpec.Spec.Containers[0].Lifecycle = &v1.Lifecycle{
							PreStop: &v1.LifecycleHandler{
								Exec: &v1.ExecAction{
									Command: ExecCommand(prefixedName(PreStopPrefix, regular1), execCommand{
										// Delay 10 secs not to make a race condition.
										StartDelay:    10,
										Delay:         40,
										ExitCode:      0,
										ContainerName: regular1,
									}),
								},
							},
						}

						preparePod(podSpec)

						podSpec = client.Create(ctx, podSpec)

						ginkgo.By("Waiting for the pod to complete")
						err := e2epod.WaitForPodNoLongerRunningInNamespace(ctx, f.ClientSet, podSpec.Name, podSpec.Namespace)
						framework.ExpectNoError(err)

						ginkgo.By("Parsing results")
						podSpec, err = client.Get(ctx, podSpec.Name, metav1.GetOptions{})
						framework.ExpectNoError(err)
						results := parseOutput(ctx, f, podSpec)

						ginkgo.By("Analyzing results")
						// FIXME ExpectNoError: this will be implemented in KEP 4438
						// liveness probes are called for restartable init containers during pod termination
						err = results.StartsBefore(prefixedName(PreStopPrefix, regular1), prefixedName(LivenessPrefix, restartableInit1))
						gomega.Expect(err).To(gomega.HaveOccurred(),
							"%s should not start before %s", prefixedName(PreStopPrefix, regular1), prefixedName(LivenessPrefix, restartableInit1))
						// FIXME ExpectNoError: this will be implemented in KEP 4438
						// restartable init containers are restarted during pod termination
						err = results.RunTogetherLhsFirst(prefixedName(PreStopPrefix, regular1), restartableInit1)
						gomega.Expect(err).To(gomega.HaveOccurred())
					})
				})
			})
		})
	})

	ginkgo.When("Running a pod with init containers and regular containers, restartPolicy=Always", func() {
		ginkgo.When("the init container is updated with a new image", func() {
			init1 := "init-1"
			regular1 := "regular-1"
			updatedImage := busyboxImage

			originalPodSpec := &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: "initcontainer-update-img",
				},
				Spec: v1.PodSpec{
					RestartPolicy: v1.RestartPolicyAlways,
					InitContainers: []v1.Container{
						{
							Name:  init1,
							Image: agnhostImage,
							Command: ExecCommand(init1, execCommand{
								Delay:    1,
								ExitCode: 0,
							}),
						},
					},
					Containers: []v1.Container{
						{
							Name:  regular1,
							Image: agnhostImage,
							Command: ExecCommand(regular1, execCommand{
								Delay:    20,
								ExitCode: 0,
							}),
						},
					},
				},
			}
			preparePod(originalPodSpec)

			ginkgo.It("should not restart init container when updated with a new image after finishing", func(ctx context.Context) {
				client := e2epod.NewPodClient(f)
				podSpec := client.Create(ctx, originalPodSpec)

				ginkgo.By("running the pod", func() {
					err := e2epod.WaitForPodNameRunningInNamespace(ctx, f.ClientSet, podSpec.Name, podSpec.Namespace)
					framework.ExpectNoError(err)
				})

				ginkgo.By("updating the image", func() {
					client.Update(ctx, podSpec.Name, func(pod *v1.Pod) {
						pod.Spec.InitContainers[0].Image = updatedImage
					})
				})

				ginkgo.By("verifying that the containers do not restart", func() {
					podSpec, err := client.Get(ctx, podSpec.Name, metav1.GetOptions{})
					framework.ExpectNoError(err)

					err = e2epod.WaitForPodCondition(ctx, f.ClientSet, podSpec.Namespace, podSpec.Name, "the init container terminated regularly", 30*time.Second, func(pod *v1.Pod) (bool, error) {
						containerStatus := pod.Status.InitContainerStatuses[0]
						return containerStatus.State.Terminated != nil && containerStatus.State.Terminated.ExitCode == 0 &&
							containerStatus.Image != updatedImage && containerStatus.RestartCount < 1, nil
					})
					framework.ExpectNoError(err)

					results := parseOutput(ctx, f, podSpec)
					framework.ExpectNoError(results.HasNotRestarted(init1))
				})
			})

			ginkgo.It("should not restart the init container if the image is updated during initialization", func(ctx context.Context) {
				invalidImage := imageutils.GetE2EImage(imageutils.InvalidRegistryImage)

				originalPodSpec := &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name: "initcontainer-update-img-initialization",
					},
					Spec: v1.PodSpec{
						RestartPolicy: v1.RestartPolicyAlways,
						InitContainers: []v1.Container{
							{
								Name:  init1,
								Image: agnhostImage,
								Command: ExecCommand(init1, execCommand{
									Delay:    20,
									ExitCode: 0,
								}),
							},
						},
						Containers: []v1.Container{
							{
								Name:  regular1,
								Image: agnhostImage,
								Command: ExecCommand(regular1, execCommand{
									Delay:    20,
									ExitCode: -1,
								}),
							},
						},
					},
				}
				preparePod(originalPodSpec)

				client := e2epod.NewPodClient(f)
				podSpec := client.Create(ctx, originalPodSpec)

				ginkgo.By("running the pod", func() {
					err := e2epod.WaitForPodNameRunningInNamespace(ctx, f.ClientSet, podSpec.Name, podSpec.Namespace)
					framework.ExpectNoError(err)
				})

				ginkgo.By("updating the image", func() {
					client.Update(ctx, podSpec.Name, func(pod *v1.Pod) {
						pod.Spec.InitContainers[0].Image = invalidImage
					})
				})

				ginkgo.By("verifying that the containers do not restart", func() {
					podSpec, err := client.Get(ctx, podSpec.Name, metav1.GetOptions{})
					framework.ExpectNoError(err)

					err = e2epod.WaitForPodCondition(ctx, f.ClientSet, podSpec.Namespace, podSpec.Name, "the init container terminated regularly", 30*time.Second, func(pod *v1.Pod) (bool, error) {
						containerStatus := pod.Status.InitContainerStatuses[0]
						return containerStatus.State.Terminated != nil && containerStatus.State.Terminated.ExitCode == 0 &&
							containerStatus.Image != invalidImage && containerStatus.RestartCount < 1, nil
					})
					framework.ExpectNoError(err)

					results := parseOutput(ctx, f, podSpec)
					framework.ExpectNoError(results.HasNotRestarted(init1))
				})
			})

			ginkgo.It("should not restart the init container if the image is updated during termination", func(ctx context.Context) {
				originalPodSpec := &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name: "initcontainer-update-img-termination",
					},
					Spec: v1.PodSpec{
						RestartPolicy: v1.RestartPolicyNever,
						InitContainers: []v1.Container{
							{
								Name:  init1,
								Image: agnhostImage,
								Command: ExecCommand(init1, execCommand{
									Delay:    1,
									ExitCode: 0,
								}),
							},
						},
						Containers: []v1.Container{
							{
								Name:  regular1,
								Image: agnhostImage,
								Command: ExecCommand(regular1, execCommand{
									Delay:              20,
									TerminationSeconds: 20,
									ExitCode:           1,
								}),
							},
						},
					},
				}
				preparePod(originalPodSpec)

				client := e2epod.NewPodClient(f)
				podSpec := client.Create(ctx, originalPodSpec)

				ginkgo.By("deleting the pod", func() {
					err := e2epod.WaitForPodRunningInNamespace(ctx, f.ClientSet, podSpec)
					framework.ExpectNoError(err)
					err = client.Delete(ctx, podSpec.Name, metav1.DeleteOptions{})
					framework.ExpectNoError(err)

				})

				ginkgo.By("updating the image", func() {
					client.Update(ctx, podSpec.Name, func(pod *v1.Pod) {
						pod.Spec.InitContainers[0].Image = updatedImage
					})
				})

				ginkgo.By("verifying that the containers do not restart", func() {
					podSpec, err := client.Get(ctx, podSpec.Name, metav1.GetOptions{})
					framework.ExpectNoError(err)

					// subtract a buffer so that the pod still exists
					gomega.Consistently(ctx, func() bool {
						podSpec, err = client.Get(ctx, podSpec.Name, metav1.GetOptions{})
						framework.ExpectNoError(err)
						for _, status := range podSpec.Status.InitContainerStatuses {
							if status.State.Terminated == nil || status.State.Terminated.ExitCode != 0 {
								continue
							}

							if status.RestartCount > 0 || status.Image == updatedImage {
								return false
							}
						}
						return true
					}, 15*time.Second, f.Timeouts.Poll).Should(gomega.BeTrueBecause("no completed init container should be restarted"))
				})
			})
		})
	})
})

var _ = SIGDescribe(framework.WithSerial(), "Containers Lifecycle", func() {
	f := framework.NewDefaultFramework("containers-lifecycle-test-serial")
	addAfterEachForCleaningUpPods(f)
	f.NamespacePodSecurityLevel = admissionapi.LevelPrivileged

	ginkgo.When("A node reboots", func() {
		ginkgo.It("should restart the containers in right order after the node reboot", func(ctx context.Context) {
			init1 := "init-1"
			init2 := "init-2"
			init3 := "init-3"
			regular1 := "regular-1"

			podLabels := map[string]string{
				"test":      "containers-lifecycle-test-serial",
				"namespace": f.Namespace.Name,
			}
			pod := &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "initialized-pod",
					Labels: podLabels,
				},
				Spec: v1.PodSpec{
					RestartPolicy: v1.RestartPolicyAlways,
					InitContainers: []v1.Container{
						{
							Name:  init1,
							Image: busyboxImage,
							Command: ExecCommand(init1, execCommand{
								Delay:    5,
								ExitCode: 0,
							}),
						},
						{
							Name:  init2,
							Image: busyboxImage,
							Command: ExecCommand(init2, execCommand{
								Delay:    5,
								ExitCode: 0,
							}),
						},
						{
							Name:  init3,
							Image: busyboxImage,
							Command: ExecCommand(init3, execCommand{
								Delay:    5,
								ExitCode: 0,
							}),
						},
					},
					Containers: []v1.Container{
						{
							Name:  regular1,
							Image: busyboxImage,
							Command: ExecCommand(regular1, execCommand{
								Delay:    30,
								ExitCode: 0,
							}),
						},
					},
				},
			}
			preparePod(pod)

			client := e2epod.NewPodClient(f)
			pod = client.Create(ctx, pod)
			ginkgo.By("Waiting for the pod to be initialized and run")
			err := e2epod.WaitForPodRunningInNamespace(ctx, f.ClientSet, pod)
			framework.ExpectNoError(err)

			ginkgo.By("Getting the current pod sandbox ID")
			rs, _, err := getCRIClient()
			framework.ExpectNoError(err)

			sandboxes, err := rs.ListPodSandbox(ctx, &runtimeapi.PodSandboxFilter{
				LabelSelector: podLabels,
			})
			framework.ExpectNoError(err)
			gomega.Expect(sandboxes).To(gomega.HaveLen(1))
			podSandboxID := sandboxes[0].Id

			ginkgo.By("Stopping the kubelet")
			restartKubelet := mustStopKubelet(ctx, f)

			ginkgo.By("Stopping the pod sandbox to simulate the node reboot")
			err = rs.StopPodSandbox(ctx, podSandboxID)
			framework.ExpectNoError(err)

			ginkgo.By("Restarting the kubelet")
			restartKubelet(ctx)
			gomega.Eventually(ctx, func() bool {
				return kubeletHealthCheck(kubeletHealthCheckURL)
			}, f.Timeouts.PodStart, f.Timeouts.Poll).Should(gomega.BeTrueBecause("kubelet was expected to be healthy"))

			ginkgo.By("Waiting for the pod to be re-initialized and run")
			err = e2epod.WaitForPodCondition(ctx, f.ClientSet, pod.Namespace, pod.Name, "re-initialized", f.Timeouts.PodStart, func(pod *v1.Pod) (bool, error) {
				if pod.Status.ContainerStatuses[0].RestartCount < 2 {
					return false, nil
				}
				if pod.Status.Phase != v1.PodRunning {
					return false, nil
				}
				return true, nil
			})
			framework.ExpectNoError(err)

			ginkgo.By("Parsing results")
			pod, err = client.Get(ctx, pod.Name, metav1.GetOptions{})
			framework.ExpectNoError(err)
			results := parseOutput(ctx, f, pod)

			ginkgo.By("Analyzing results")
			init1Started, err := results.FindIndex(init1, "Started", 0)
			framework.ExpectNoError(err)
			init2Started, err := results.FindIndex(init2, "Started", 0)
			framework.ExpectNoError(err)
			init3Started, err := results.FindIndex(init3, "Started", 0)
			framework.ExpectNoError(err)
			regular1Started, err := results.FindIndex(regular1, "Started", 0)
			framework.ExpectNoError(err)

			init1Restarted, err := results.FindIndex(init1, "Started", init1Started+1)
			framework.ExpectNoError(err)
			init2Restarted, err := results.FindIndex(init2, "Started", init2Started+1)
			framework.ExpectNoError(err)
			init3Restarted, err := results.FindIndex(init3, "Started", init3Started+1)
			framework.ExpectNoError(err)
			regular1Restarted, err := results.FindIndex(regular1, "Started", regular1Started+1)
			framework.ExpectNoError(err)

			framework.ExpectNoError(init1Started.IsBefore(init2Started))
			framework.ExpectNoError(init2Started.IsBefore(init3Started))
			framework.ExpectNoError(init3Started.IsBefore(regular1Started))

			framework.ExpectNoError(init1Restarted.IsBefore(init2Restarted))
			framework.ExpectNoError(init2Restarted.IsBefore(init3Restarted))
			framework.ExpectNoError(init3Restarted.IsBefore(regular1Restarted))
		})
	})

	ginkgo.When("The kubelet restarts", func() {
		ginkgo.When("a Pod is initialized and running", func() {
			var client *e2epod.PodClient
			var err error
			var pod *v1.Pod
			init1 := "init-1"
			init2 := "init-2"
			init3 := "init-3"
			regular1 := "regular-1"

			ginkgo.BeforeEach(func(ctx context.Context) {
				pod = &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name: "initialized-pod",
					},
					Spec: v1.PodSpec{
						RestartPolicy: v1.RestartPolicyAlways,
						InitContainers: []v1.Container{
							{
								Name:  init1,
								Image: busyboxImage,
								Command: ExecCommand(init1, execCommand{
									Delay:    1,
									ExitCode: 0,
								}),
							},
							{
								Name:  init2,
								Image: busyboxImage,
								Command: ExecCommand(init2, execCommand{
									Delay:    1,
									ExitCode: 0,
								}),
							},
							{
								Name:  init3,
								Image: busyboxImage,
								Command: ExecCommand(init3, execCommand{
									Delay:    1,
									ExitCode: 0,
								}),
							},
						},
						Containers: []v1.Container{
							{
								Name:  regular1,
								Image: busyboxImage,
								Command: ExecCommand(regular1, execCommand{
									Delay:    300,
									ExitCode: 0,
								}),
							},
						},
					},
				}
				preparePod(pod)

				client = e2epod.NewPodClient(f)
				pod = client.Create(ctx, pod)
				ginkgo.By("Waiting for the pod to be initialized and run")
				err := e2epod.WaitForPodRunningInNamespace(ctx, f.ClientSet, pod)
				framework.ExpectNoError(err)
			})

			ginkgo.It("should not restart any completed init container after the kubelet restart", func(ctx context.Context) {
				ginkgo.By("stopping the kubelet")
				restartKubelet := mustStopKubelet(ctx, f)

				ginkgo.By("restarting the kubelet")
				restartKubelet(ctx)
				// wait until the kubelet health check will succeed
				gomega.Eventually(ctx, func() bool {
					return kubeletHealthCheck(kubeletHealthCheckURL)
				}, f.Timeouts.PodStart, f.Timeouts.Poll).Should(gomega.BeTrueBecause("kubelet should be started"))

				ginkgo.By("ensuring that no completed init container is restarted")
				gomega.Consistently(ctx, func() bool {
					pod, err = client.Get(ctx, pod.Name, metav1.GetOptions{})
					framework.ExpectNoError(err)
					for _, status := range pod.Status.InitContainerStatuses {
						if status.State.Terminated == nil || status.State.Terminated.ExitCode != 0 {
							continue
						}

						if status.RestartCount > 0 {
							return false
						}
					}
					return true
				}, 1*time.Minute, f.Timeouts.Poll).Should(gomega.BeTrueBecause("no completed init container should be restarted"))

				ginkgo.By("Parsing results")
				pod, err = client.Get(ctx, pod.Name, metav1.GetOptions{})
				framework.ExpectNoError(err)
				results := parseOutput(ctx, f, pod)

				ginkgo.By("Analyzing results")
				framework.ExpectNoError(results.StartsBefore(init1, init2))
				framework.ExpectNoError(results.ExitsBefore(init1, init2))

				framework.ExpectNoError(results.StartsBefore(init2, init3))
				framework.ExpectNoError(results.ExitsBefore(init2, init3))

				gomega.Expect(pod.Status.InitContainerStatuses[0].RestartCount).To(gomega.Equal(int32(0)))
				gomega.Expect(pod.Status.InitContainerStatuses[1].RestartCount).To(gomega.Equal(int32(0)))
				gomega.Expect(pod.Status.InitContainerStatuses[2].RestartCount).To(gomega.Equal(int32(0)))
			})

			ginkgo.It("should not restart any completed init container, even after the completed init container statuses have been removed and the kubelet restarted", func(ctx context.Context) {
				ginkgo.By("stopping the kubelet")
				restartKubelet := mustStopKubelet(ctx, f)

				ginkgo.By("removing the completed init container statuses from the container runtime")
				rs, _, err := getCRIClient()
				framework.ExpectNoError(err)

				pod, err = client.Get(ctx, pod.Name, metav1.GetOptions{})
				framework.ExpectNoError(err)

				for _, c := range pod.Status.InitContainerStatuses {
					if c.State.Terminated == nil || c.State.Terminated.ExitCode != 0 {
						continue
					}

					tokens := strings.Split(c.ContainerID, "://")
					gomega.Expect(tokens).To(gomega.HaveLen(2))

					containerID := tokens[1]

					err := rs.RemoveContainer(ctx, containerID)
					framework.ExpectNoError(err)
				}

				ginkgo.By("restarting the kubelet")
				restartKubelet(ctx)
				// wait until the kubelet health check will succeed
				gomega.Eventually(ctx, func() bool {
					return kubeletHealthCheck(kubeletHealthCheckURL)
				}, f.Timeouts.PodStart, f.Timeouts.Poll).Should(gomega.BeTrueBecause("kubelet should be restarted"))

				ginkgo.By("ensuring that no completed init container is restarted")
				gomega.Consistently(ctx, func() bool {
					pod, err = client.Get(ctx, pod.Name, metav1.GetOptions{})
					framework.ExpectNoError(err)
					for _, status := range pod.Status.InitContainerStatuses {
						if status.State.Terminated == nil || status.State.Terminated.ExitCode != 0 {
							continue
						}

						if status.RestartCount > 0 {
							return false
						}
					}
					return true
				}, 1*time.Minute, f.Timeouts.Poll).Should(gomega.BeTrueBecause("no completed init container should be restarted"))

				ginkgo.By("Analyzing results")
				// Cannot analyze the results with the container logs as the
				// container statuses have been removed from container runtime.
				gomega.Expect(pod.Status.InitContainerStatuses[0].RestartCount).To(gomega.Equal(int32(0)))
				gomega.Expect(pod.Status.InitContainerStatuses[1].RestartCount).To(gomega.Equal(int32(0)))
				gomega.Expect(pod.Status.InitContainerStatuses[2].RestartCount).To(gomega.Equal(int32(0)))
				gomega.Expect(pod.Status.ContainerStatuses[0].State.Running).ToNot(gomega.BeNil())
			})
		})

		ginkgo.When("a Pod is initializing the long-running init container", func() {
			var client *e2epod.PodClient
			var err error
			var pod *v1.Pod
			init1 := "init-1"
			init2 := "init-2"
			longRunningInit3 := "long-running-init-3"
			regular1 := "regular-1"

			ginkgo.BeforeEach(func(ctx context.Context) {
				pod = &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name: "initializing-long-running-init-container",
					},
					Spec: v1.PodSpec{
						RestartPolicy: v1.RestartPolicyAlways,
						InitContainers: []v1.Container{
							{
								Name:  init1,
								Image: busyboxImage,
								Command: ExecCommand(init1, execCommand{
									Delay:    1,
									ExitCode: 0,
								}),
							},
							{
								Name:  init2,
								Image: busyboxImage,
								Command: ExecCommand(init2, execCommand{
									Delay:    1,
									ExitCode: 0,
								}),
							},
							{
								Name:  longRunningInit3,
								Image: busyboxImage,
								Command: ExecCommand(longRunningInit3, execCommand{
									Delay:    300,
									ExitCode: 0,
								}),
							},
						},
						Containers: []v1.Container{
							{
								Name:  regular1,
								Image: busyboxImage,
								Command: ExecCommand(regular1, execCommand{
									Delay:    300,
									ExitCode: 0,
								}),
							},
						},
					},
				}
				preparePod(pod)

				client = e2epod.NewPodClient(f)
				pod = client.Create(ctx, pod)
				ginkgo.By("Waiting for the pod to be initializing the long-running init container")
				err := e2epod.WaitForPodCondition(ctx, f.ClientSet, pod.Namespace, pod.Name, "long-running init container initializing", 1*time.Minute, func(pod *v1.Pod) (bool, error) {
					for _, c := range pod.Status.InitContainerStatuses {
						if c.Name != longRunningInit3 {
							continue
						}
						if c.State.Running != nil && (c.Started != nil && *c.Started == true) {
							return true, nil
						}
					}
					return false, nil
				})
				framework.ExpectNoError(err)
			})

			ginkgo.It("should not restart any completed init container after the kubelet restart", func(ctx context.Context) {
				ginkgo.By("stopping the kubelet")
				restartKubelet := mustStopKubelet(ctx, f)

				ginkgo.By("restarting the kubelet")
				restartKubelet(ctx)

				ginkgo.By("ensuring that no completed init container is restarted")
				gomega.Consistently(ctx, func() bool {
					pod, err = client.Get(ctx, pod.Name, metav1.GetOptions{})
					framework.ExpectNoError(err)
					for _, status := range pod.Status.InitContainerStatuses {
						if status.State.Terminated == nil || status.State.Terminated.ExitCode != 0 {
							continue
						}

						if status.RestartCount > 0 {
							return false
						}
					}
					return true
				}, 1*time.Minute, f.Timeouts.Poll).Should(gomega.BeTrueBecause("no completed init container should be restarted"))

				ginkgo.By("Parsing results")
				pod, err = client.Get(ctx, pod.Name, metav1.GetOptions{})
				framework.ExpectNoError(err)
				results := parseOutput(ctx, f, pod)

				ginkgo.By("Analyzing results")
				framework.ExpectNoError(results.StartsBefore(init1, init2))
				framework.ExpectNoError(results.ExitsBefore(init1, init2))

				gomega.Expect(pod.Status.InitContainerStatuses[0].RestartCount).To(gomega.Equal(int32(0)))
				gomega.Expect(pod.Status.InitContainerStatuses[1].RestartCount).To(gomega.Equal(int32(0)))
			})

			ginkgo.It("should not restart any completed init container, even after the completed init container statuses have been removed and the kubelet restarted", func(ctx context.Context) {
				ginkgo.By("stopping the kubelet")
				restartKubelet := mustStopKubelet(ctx, f)

				ginkgo.By("removing the completed init container statuses from the container runtime")
				rs, _, err := getCRIClient()
				framework.ExpectNoError(err)

				pod, err = client.Get(ctx, pod.Name, metav1.GetOptions{})
				framework.ExpectNoError(err)

				for _, c := range pod.Status.InitContainerStatuses {
					if c.State.Terminated == nil || c.State.Terminated.ExitCode != 0 {
						continue
					}

					tokens := strings.Split(c.ContainerID, "://")
					gomega.Expect(tokens).To(gomega.HaveLen(2))

					containerID := tokens[1]

					err := rs.RemoveContainer(ctx, containerID)
					framework.ExpectNoError(err)
				}

				ginkgo.By("restarting the kubelet")
				restartKubelet(ctx)

				ginkgo.By("ensuring that no completed init container is restarted")
				gomega.Consistently(ctx, func() bool {
					pod, err = client.Get(ctx, pod.Name, metav1.GetOptions{})
					framework.ExpectNoError(err)
					for _, status := range pod.Status.InitContainerStatuses {
						if status.State.Terminated == nil || status.State.Terminated.ExitCode != 0 {
							continue
						}

						if status.RestartCount > 0 {
							return false
						}
					}
					return true
				}, 1*time.Minute, f.Timeouts.Poll).Should(gomega.BeTrueBecause("no completed init container should be restarted"))

				ginkgo.By("Analyzing results")
				// Cannot analyze the results with the container logs as the
				// container statuses have been removed from container runtime.
				gomega.Expect(pod.Status.InitContainerStatuses[0].RestartCount).To(gomega.Equal(int32(0)))
				gomega.Expect(pod.Status.InitContainerStatuses[1].RestartCount).To(gomega.Equal(int32(0)))
			})
		})
	})
})

var _ = SIGDescribe(feature.SidecarContainers, "Containers Lifecycle", func() {
	f := framework.NewDefaultFramework("containers-lifecycle-test")
	addAfterEachForCleaningUpPods(f)
	f.NamespacePodSecurityLevel = admissionapi.LevelPrivileged

	ginkgo.When("using a Pod with restartPolicy=Never, three init container and two restartable init containers", ginkgo.Ordered, func() {

		init1 := "init-1"
		restartableInit1 := "restartable-init-1"
		init2 := "init-2"
		restartableInit2 := "restartable-init-2"
		init3 := "init-3"
		regular1 := "regular-1"

		podSpec := &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: "restartable-init-containers-start-serially",
			},
			Spec: v1.PodSpec{
				RestartPolicy: v1.RestartPolicyNever,
				InitContainers: []v1.Container{
					{
						Name:  init1,
						Image: busyboxImage,
						Command: ExecCommand(init1, execCommand{
							Delay:    1,
							ExitCode: 0,
						}),
					},
					{
						Name:  restartableInit1,
						Image: busyboxImage,
						Command: ExecCommand(restartableInit1, execCommand{
							Delay:    600,
							ExitCode: 0,
						}),
						RestartPolicy: &containerRestartPolicyAlways,
					},
					{
						Name:  init2,
						Image: busyboxImage,
						Command: ExecCommand(init2, execCommand{
							Delay:    1,
							ExitCode: 0,
						}),
					},
					{
						Name:  restartableInit2,
						Image: busyboxImage,
						Command: ExecCommand(restartableInit2, execCommand{
							Delay:    600,
							ExitCode: 0,
						}),
						RestartPolicy: &containerRestartPolicyAlways,
					},
					{
						Name:  init3,
						Image: busyboxImage,
						Command: ExecCommand(init3, execCommand{
							Delay:    1,
							ExitCode: 0,
						}),
					},
				},
				Containers: []v1.Container{
					{
						Name:  regular1,
						Image: busyboxImage,
						Command: ExecCommand(regular1, execCommand{
							Delay:    1,
							ExitCode: 0,
						}),
					},
				},
			},
		}

		preparePod(podSpec)
		var results containerOutputList

		ginkgo.It("should finish and produce log", func(ctx context.Context) {
			client := e2epod.NewPodClient(f)
			podSpec = client.Create(ctx, podSpec)

			err := e2epod.WaitTimeoutForPodNoLongerRunningInNamespace(ctx, f.ClientSet, podSpec.Name, podSpec.Namespace, 5*time.Minute)
			framework.ExpectNoError(err)

			podSpec, err := client.Get(context.Background(), podSpec.Name, metav1.GetOptions{})
			framework.ExpectNoError(err)

			// pod should exit successfully
			gomega.Expect(podSpec.Status.Phase).To(gomega.Equal(v1.PodSucceeded))

			results = parseOutput(ctx, f, podSpec)
		})

		ginkgo.It("should run the first init container to completion before starting first restartable init container", func() {
			framework.ExpectNoError(results.StartsBefore(init1, restartableInit1))
			framework.ExpectNoError(results.ExitsBefore(init1, restartableInit1))
		})

		ginkgo.It("should run first init container and first restartable init container together", func() {
			framework.ExpectNoError(results.RunTogetherLhsFirst(restartableInit1, init2))
		})

		ginkgo.It("should run second init container to completion before starting second restartable init container", func() {
			framework.ExpectNoError(results.StartsBefore(init2, restartableInit2))
			framework.ExpectNoError(results.ExitsBefore(init2, restartableInit2))
		})

		ginkgo.It("should start second restartable init container before third init container", func() {
			framework.ExpectNoError(results.StartsBefore(restartableInit2, init3))
		})

		ginkgo.It("should run both restartable init containers and third init container together", func() {
			framework.ExpectNoError(results.RunTogether(restartableInit1, restartableInit2))
			framework.ExpectNoError(results.RunTogether(restartableInit1, init3))
			framework.ExpectNoError(results.RunTogether(restartableInit2, init3))
		})

		ginkgo.It("should run third init container to completion before starting regular container", func() {
			framework.ExpectNoError(results.StartsBefore(init3, regular1))
			framework.ExpectNoError(results.ExitsBefore(init3, regular1))
		})

		ginkgo.It("should run both restartable init containers and a regular container together", func() {
			framework.ExpectNoError(results.RunTogether(restartableInit1, regular1))
			framework.ExpectNoError(results.RunTogether(restartableInit2, regular1))
		})
	})

	ginkgo.When("initializing an init container after a restartable init container", func() {
		init1 := "init-1"
		restartableInit1 := "restartable-init-1"
		init2 := "init-2"
		restartableInit2 := "restartable-init-2"
		regular1 := "regular-1"

		containerTerminationSeconds := 5
		updatedImage := busyboxImage

		var originalPodSpec *v1.Pod

		ginkgo.BeforeEach(func(ctx context.Context) {
			originalPodSpec = &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: "update-restartable-init-img-during-initialize",
				},
				Spec: v1.PodSpec{
					RestartPolicy: v1.RestartPolicyAlways,
					InitContainers: []v1.Container{
						{
							Name:  init1,
							Image: agnhostImage,
							Command: ExecCommand(init1, execCommand{
								Delay:    1,
								ExitCode: 0,
							}),
						},
						{
							Name:  restartableInit1,
							Image: agnhostImage,
							Command: ExecCommand(restartableInit1, execCommand{
								Delay:              600,
								TerminationSeconds: containerTerminationSeconds,
								ExitCode:           0,
							}),
							RestartPolicy: &containerRestartPolicyAlways,
						},
						{
							Name:  init2,
							Image: agnhostImage,
							Command: ExecCommand(init2, execCommand{
								Delay:    600,
								ExitCode: 0,
							}),
						},
						{
							Name:  restartableInit2,
							Image: agnhostImage,
							Command: ExecCommand(restartableInit2, execCommand{
								Delay:              1,
								TerminationSeconds: 1,
								ExitCode:           0,
							}),
							RestartPolicy: &containerRestartPolicyAlways,
						},
					},
					Containers: []v1.Container{
						{
							Name:  regular1,
							Image: agnhostImage,
							Command: ExecCommand(regular1, execCommand{
								Delay:              1,
								TerminationSeconds: 1,
								ExitCode:           0,
							}),
						},
					},
				},
			}
		})

		ginkgo.It("Should begin initializing the pod, restartPolicy=Always", func(ctx context.Context) {
			preparePod(originalPodSpec)

			client := e2epod.NewPodClient(f)
			pod := client.Create(ctx, originalPodSpec)

			ginkgo.By("Running the pod", func() {
				err := e2epod.WaitForPodCondition(ctx, f.ClientSet, pod.Namespace, pod.Name, "the second init container is running but not started", 2*time.Minute, func(pod *v1.Pod) (bool, error) {
					if pod.Status.Phase != v1.PodPending {
						return false, fmt.Errorf("pod should be in pending phase")
					}
					if len(pod.Status.InitContainerStatuses) < 3 {
						return false, nil
					}
					containerStatus := pod.Status.InitContainerStatuses[2]
					return containerStatus.State.Running != nil && *containerStatus.Started, nil
				})
				framework.ExpectNoError(err)
			})

			ginkgo.By("Changing the image of the restartable init container", func() {
				client.Update(ctx, pod.Name, func(pod *v1.Pod) {
					pod.Spec.InitContainers[1].Image = updatedImage
				})
			})

			ginkgo.By("verifying the image changed", func() {
				err := e2epod.WaitForPodCondition(ctx, f.ClientSet, pod.Namespace, pod.Name, "the restartable init container image changed", 1*time.Minute, func(pod *v1.Pod) (bool, error) {
					containerStatus := pod.Status.InitContainerStatuses[1]
					return containerStatus.State.Running != nil &&
						containerStatus.RestartCount > 0 && containerStatus.Image == updatedImage, nil
				})
				framework.ExpectNoError(err)
			})

			pod, err := client.Get(ctx, pod.Name, metav1.GetOptions{})
			framework.ExpectNoError(err)

			results := parseOutput(ctx, f, pod)

			ginkgo.By("verifying started the containers in order", func() {
				framework.ExpectNoError(results.StartsBefore(init1, restartableInit1))
				framework.ExpectNoError(results.StartsBefore(restartableInit1, init2))
				framework.ExpectNoError(results.Starts(init1))
				framework.ExpectNoError(results.Starts(restartableInit1))
				framework.ExpectNoError(results.Starts(init2))
				framework.ExpectNoError(results.DoesntStart(restartableInit2))
				framework.ExpectNoError(results.DoesntStart(regular1))
			})

			ginkgo.By("verifying not restarted any regular init container", func() {
				framework.ExpectNoError(results.HasNotRestarted(init1))
				framework.ExpectNoError(results.HasNotRestarted(init2))
			})

			ginkgo.By("verifying restarted the restartable init container whose image changed", func() {
				framework.ExpectNoError(results.HasRestarted(restartableInit1))
			})
		})

		// Same as restartPolicy=Always
		ginkgo.It("Should begin initializing the pod, restartPolicy=OnFailure", func(ctx context.Context) {
			originalPodSpec.Spec.RestartPolicy = v1.RestartPolicyOnFailure
			originalPodSpec.Name = "restartable-init-container-initialization-imgupdate-onfailure"

			preparePod(originalPodSpec)

			client := e2epod.NewPodClient(f)
			pod := client.Create(ctx, originalPodSpec)

			ginkgo.By("Running the pod", func() {
				err := e2epod.WaitForPodCondition(ctx, f.ClientSet, pod.Namespace, pod.Name, "the second init container is running but not started", 2*time.Minute, func(pod *v1.Pod) (bool, error) {
					if pod.Status.Phase != v1.PodPending {
						return false, fmt.Errorf("pod should be in pending phase")
					}
					if len(pod.Status.InitContainerStatuses) < 3 {
						return false, nil
					}
					containerStatus := pod.Status.InitContainerStatuses[2]
					return containerStatus.State.Running != nil && *containerStatus.Started, nil
				})
				framework.ExpectNoError(err)
			})

			ginkgo.By("Changing the image of the restartable init container", func() {
				client.Update(ctx, pod.Name, func(pod *v1.Pod) {
					pod.Spec.InitContainers[1].Image = updatedImage
				})
			})

			ginkgo.By("verifying the image changed", func() {
				err := e2epod.WaitForPodCondition(ctx, f.ClientSet, pod.Namespace, pod.Name, "the restartable init container image changed", 1*time.Minute, func(pod *v1.Pod) (bool, error) {
					containerStatus := pod.Status.InitContainerStatuses[1]
					return containerStatus.State.Running != nil &&
						containerStatus.RestartCount > 0 && containerStatus.Image == updatedImage, nil
				})
				framework.ExpectNoError(err)
			})

			pod, err := client.Get(ctx, pod.Name, metav1.GetOptions{})
			framework.ExpectNoError(err)

			results := parseOutput(ctx, f, pod)

			ginkgo.By("verifying it started the containers in order", func() {
				framework.ExpectNoError(results.StartsBefore(init1, restartableInit1))
				framework.ExpectNoError(results.StartsBefore(restartableInit1, init2))
				framework.ExpectNoError(results.Starts(init1))
				framework.ExpectNoError(results.Starts(restartableInit1))
				framework.ExpectNoError(results.Starts(init2))
				framework.ExpectNoError(results.DoesntStart(restartableInit2))
				framework.ExpectNoError(results.DoesntStart(regular1))
			})

			ginkgo.By("verifying not restarted any regular init container", func() {
				framework.ExpectNoError(results.HasNotRestarted(init1))
				framework.ExpectNoError(results.HasNotRestarted(init2))
			})

			ginkgo.By("verifying restarted the restartable init container whose image changed", func() {
				framework.ExpectNoError(results.HasRestarted(restartableInit1))
			})
		})

		ginkgo.It("Should begin initializing the pod, restartPolicy=Never", func(ctx context.Context) {
			originalPodSpec.Spec.RestartPolicy = v1.RestartPolicyNever
			originalPodSpec.Name = "restartable-init-container-initialization-imgupdate-never"
			originalPodSpec.Spec.InitContainers[2].Command = ExecCommand(init2,
				execCommand{Delay: 30, ExitCode: 1})

			preparePod(originalPodSpec)

			client := e2epod.NewPodClient(f)
			pod := client.Create(ctx, originalPodSpec)

			ginkgo.By("Running the pod", func() {
				err := e2epod.WaitForPodCondition(ctx, f.ClientSet, pod.Namespace, pod.Name, "the second init container is running but not started", 2*time.Minute, func(pod *v1.Pod) (bool, error) {
					if pod.Status.Phase != v1.PodPending {
						return false, fmt.Errorf("pod should be in pending phase")
					}
					if len(pod.Status.InitContainerStatuses) < 3 {
						return false, nil
					}
					containerStatus := pod.Status.InitContainerStatuses[2]
					return containerStatus.State.Running != nil && *containerStatus.Started, nil
				})
				framework.ExpectNoError(err)
			})

			ginkgo.By("Changing the image of the restartable init container", func() {
				client.Update(ctx, pod.Name, func(pod *v1.Pod) {
					pod.Spec.InitContainers[1].Image = updatedImage
				})
			})

			ginkgo.By("verifying the image changed", func() {
				err := e2epod.WaitForPodCondition(ctx, f.ClientSet, pod.Namespace, pod.Name, "the restartable init container image changed", 1*time.Minute, func(pod *v1.Pod) (bool, error) {
					containerStatus := pod.Status.InitContainerStatuses[1]
					return containerStatus.State.Running != nil &&
						containerStatus.RestartCount > 0 && containerStatus.Image == updatedImage, nil
				})
				framework.ExpectNoError(err)
			})

			// Init containers don't restart when restartPolicy=Never
			ginkgo.By("Waiting for the pod to fail", func() {
				err := e2epod.WaitForPodTerminatedInNamespace(ctx, f.ClientSet, pod.Name, "", pod.Namespace)
				framework.ExpectNoError(err)
			})

			pod, err := client.Get(ctx, pod.Name, metav1.GetOptions{})
			framework.ExpectNoError(err)

			results := parseOutput(ctx, f, pod)

			ginkgo.By("verifying started the containers in order", func() {
				framework.ExpectNoError(results.StartsBefore(init1, restartableInit1))
				framework.ExpectNoError(results.StartsBefore(restartableInit1, init2))
				framework.ExpectNoError(results.Starts(init1))
				framework.ExpectNoError(results.Starts(restartableInit1))
				framework.ExpectNoError(results.Starts(init2))
				framework.ExpectNoError(results.DoesntStart(restartableInit2))
				framework.ExpectNoError(results.DoesntStart(regular1))
			})

			ginkgo.By("verifying not restarted any regular init container", func() {
				framework.ExpectNoError(results.HasNotRestarted(init1))
				framework.ExpectNoError(results.HasNotRestarted(init2))
			})

			ginkgo.By("verifying restarted the restartable init container whose image changed", func() {
				framework.ExpectNoError(results.HasRestarted(restartableInit1))
			})

			ginkgo.By("verifying terminated init containers in reverse order", func() {
				framework.ExpectNoError(results.Exits(init2))
				framework.ExpectNoError(results.Exits(restartableInit1))
				framework.ExpectNoError(results.Exits(init1))
				framework.ExpectNoError(results.ExitsBefore(init2, restartableInit1))

			})
		})
	})

	ginkgo.When("using a restartable init container in a Pod with restartPolicy=Never", func() {
		ginkgo.When("a restartable init container runs continuously", ginkgo.Ordered, func() {

			restartableInit1 := "restartable-init-1"
			regular1 := "regular-1"

			podSpec := &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: "restartable-init-container-run-continuously",
				},
				Spec: v1.PodSpec{
					RestartPolicy: v1.RestartPolicyNever,
					InitContainers: []v1.Container{
						{
							Name:  restartableInit1,
							Image: busyboxImage,
							Command: ExecCommand(restartableInit1, execCommand{
								Delay:    600,
								ExitCode: 0,
							}),
							RestartPolicy: &containerRestartPolicyAlways,
						},
					},
					Containers: []v1.Container{
						{
							Name:  regular1,
							Image: busyboxImage,
							Command: ExecCommand(regular1, execCommand{
								Delay:    1,
								ExitCode: 0,
							}),
						},
					},
				},
			}

			preparePod(podSpec)
			var results containerOutputList

			ginkgo.It("should complete a Pod successfully and produce log", func(ctx context.Context) {
				client := e2epod.NewPodClient(f)
				podSpec = client.Create(ctx, podSpec)

				err := e2epod.WaitTimeoutForPodNoLongerRunningInNamespace(ctx, f.ClientSet, podSpec.Name, podSpec.Namespace, 5*time.Minute)
				framework.ExpectNoError(err)

				podSpec, err := client.Get(ctx, podSpec.Name, metav1.GetOptions{})
				framework.ExpectNoError(err)

				// pod should exit successfully
				gomega.Expect(podSpec.Status.Phase).To(gomega.Equal(v1.PodSucceeded))

				results = parseOutput(ctx, f, podSpec)
			})
			ginkgo.It("should not restart a restartable init container", func() {
				framework.ExpectNoError(results.HasNotRestarted(restartableInit1))
			})
			ginkgo.It("should run a regular container to completion", func() {
				framework.ExpectNoError(results.Exits(regular1))
			})

			ginkgo.It("should restart when updated with a new image", func(ctx context.Context) {
				restartableInit1 := "restartable-init-1"
				restartableInit2 := "restartable-init-2"
				regular1 := "regular-1"
				updatedImage := busyboxImage

				podSpec := &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name: "restartable-init-container-imgupdate-never",
					},
					Spec: v1.PodSpec{
						RestartPolicy: v1.RestartPolicyNever,
						InitContainers: []v1.Container{
							{
								Name:  restartableInit1,
								Image: agnhostImage,
								Command: ExecCommand(restartableInit1, execCommand{
									Delay:    600,
									ExitCode: 0,
								}),
								RestartPolicy: &containerRestartPolicyAlways,
							},
							{
								Name:  restartableInit2,
								Image: agnhostImage,
								Command: ExecCommand(restartableInit2, execCommand{
									Delay:    600,
									ExitCode: 0,
								}),
								RestartPolicy: &containerRestartPolicyAlways,
							},
						},
						Containers: []v1.Container{
							{
								Name:  regular1,
								Image: agnhostImage,
								Command: ExecCommand(regular1, execCommand{
									Delay:    60,
									ExitCode: 0,
								}),
							},
						},
					},
				}

				preparePod(podSpec)

				client := e2epod.NewPodClient(f)
				podSpec = client.Create(ctx, podSpec)

				ginkgo.By("running the pod", func() {
					err := e2epod.WaitForPodNameRunningInNamespace(ctx, f.ClientSet, podSpec.Name, podSpec.Namespace)
					framework.ExpectNoError(err)
				})

				ginkgo.By("updating the image", func() {
					client.Update(ctx, podSpec.Name, func(pod *v1.Pod) {
						pod.Spec.InitContainers[0].Image = updatedImage
					})
				})

				ginkgo.By("analyzing results", func() {
					err := e2epod.WaitForPodCondition(ctx, f.ClientSet, podSpec.Namespace, podSpec.Name, "wait for container to update image",
						time.Duration(2)*time.Minute, func(pod *v1.Pod) (bool, error) {
							containerStatus := pod.Status.InitContainerStatuses[0]
							return containerStatus.State.Running != nil && containerStatus.Image == updatedImage, nil
						})
					framework.ExpectNoError(err)

					podSpec, err := client.Get(ctx, podSpec.Name, metav1.GetOptions{})
					framework.ExpectNoError(err)

					results := parseOutput(ctx, f, podSpec)
					ginkgo.By("Verifying not restarted the regular container", func() {
						framework.ExpectNoError(results.HasNotRestarted(regular1))
					})
					ginkgo.By("Verifying has restarted the restartable init container", func() {
						framework.ExpectNoError(results.HasRestarted(restartableInit1))
					})
					ginkgo.By("Verifying not restarted the other restartable init container", func() {
						framework.ExpectNoError(results.HasNotRestarted(restartableInit2))
					})
				})
			})
		})

		ginkgo.When("a restartable init container fails to start because of a bad image", ginkgo.Ordered, func() {

			restartableInit1 := "restartable-init-1"
			regular1 := "regular-1"

			podSpec := &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: "restartable-init-runs-with-pod",
				},
				Spec: v1.PodSpec{
					RestartPolicy: v1.RestartPolicyNever,
					InitContainers: []v1.Container{
						{
							Name:  restartableInit1,
							Image: imageutils.GetE2EImage(imageutils.InvalidRegistryImage),
							Command: ExecCommand(restartableInit1, execCommand{
								Delay:    600,
								ExitCode: 0,
							}),
							RestartPolicy: &containerRestartPolicyAlways,
						},
					},
					Containers: []v1.Container{
						{
							Name:  regular1,
							Image: busyboxImage,
							Command: ExecCommand(regular1, execCommand{
								Delay:    1,
								ExitCode: 0,
							}),
						},
					},
				},
			}

			preparePod(podSpec)
			var results containerOutputList

			ginkgo.It("should mark a Pod as failed and produce log", func(ctx context.Context) {
				client := e2epod.NewPodClient(f)
				podSpec = client.Create(ctx, podSpec)

				// restartable init container should be in image pull backoff
				err := WaitForPodInitContainerToFail(ctx, f.ClientSet, podSpec.Namespace, podSpec.Name, 0, "ImagePullBackOff", f.Timeouts.PodStart)
				framework.ExpectNoError(err)

				podSpec, err = client.Get(context.Background(), podSpec.Name, metav1.GetOptions{})
				framework.ExpectNoError(err)
				results = parseOutput(ctx, f, podSpec)
			})
			ginkgo.It("should not start a restartable init container", func() {
				framework.ExpectNoError(results.DoesntStart(restartableInit1))
			})
			ginkgo.It("should not start a regular container", func() {
				framework.ExpectNoError(results.DoesntStart(regular1))
			})
		})

		ginkgo.When("a restartable init container starts and exits with exit code 0 continuously", ginkgo.Ordered, func() {

			restartableInit1 := "restartable-init-1"
			init1 := "init-1"
			regular1 := "regular-1"

			podSpec := &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: "restartable-init-container-exit-0-continuously",
				},
				Spec: v1.PodSpec{
					RestartPolicy: v1.RestartPolicyNever,
					InitContainers: []v1.Container{
						{
							Name:  restartableInit1,
							Image: busyboxImage,
							Command: ExecCommand(restartableInit1, execCommand{
								Delay:    5,
								ExitCode: 0,
							}),
							RestartPolicy: &containerRestartPolicyAlways,
						},
						{
							Name:  init1,
							Image: busyboxImage,
							Command: ExecCommand(init1, execCommand{
								Delay:    5,
								ExitCode: 0,
							}),
						},
					},
					Containers: []v1.Container{
						{
							Name:  regular1,
							Image: busyboxImage,
							Command: ExecCommand(regular1, execCommand{
								Delay:    60,
								ExitCode: 0,
							}),
						},
					},
				},
			}

			preparePod(podSpec)
			var results containerOutputList

			ginkgo.It("should complete a Pod successfully and produce log", func(ctx context.Context) {
				client := e2epod.NewPodClient(f)
				podSpec = client.Create(ctx, podSpec)

				err := e2epod.WaitTimeoutForPodNoLongerRunningInNamespace(ctx, f.ClientSet, podSpec.Name, podSpec.Namespace, 5*time.Minute)
				framework.ExpectNoError(err)

				podSpec, err := client.Get(ctx, podSpec.Name, metav1.GetOptions{})
				framework.ExpectNoError(err)

				// pod should exit successfully
				gomega.Expect(podSpec.Status.Phase).To(gomega.Equal(v1.PodSucceeded))
				results = parseOutput(ctx, f, podSpec)
			})
			ginkgo.It("should restart a restartable init container before the regular container started", func() {
				framework.ExpectNoError(results.StartsBefore(restartableInit1, regular1))
			})
			ginkgo.It("should restart a restartable init container after the regular container started", func() {
				framework.ExpectNoError(results.StartsBefore(regular1, restartableInit1))
			})
			ginkgo.It("should run a regular container to completion", func() {
				framework.ExpectNoError(results.Exits(regular1))
			})
		})

		ginkgo.When("a restartable init container starts and exits with exit code 1 continuously", ginkgo.Ordered, func() {
			restartableInit1 := "restartable-init-1"
			init1 := "init-1"
			regular1 := "regular-1"

			podSpec := &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: "restartable-init-container-exit-1-continuously",
				},
				Spec: v1.PodSpec{
					RestartPolicy: v1.RestartPolicyNever,
					InitContainers: []v1.Container{
						{
							Name:  restartableInit1,
							Image: busyboxImage,
							Command: ExecCommand(restartableInit1, execCommand{
								Delay:    5,
								ExitCode: 1,
							}),
							RestartPolicy: &containerRestartPolicyAlways,
						},
						{
							Name:  init1,
							Image: busyboxImage,
							Command: ExecCommand(init1, execCommand{
								Delay:    5,
								ExitCode: 0,
							}),
						},
					},
					Containers: []v1.Container{
						{
							Name:  regular1,
							Image: busyboxImage,
							Command: ExecCommand(regular1, execCommand{
								Delay:    60,
								ExitCode: 0,
							}),
						},
					},
				},
			}

			preparePod(podSpec)
			var results containerOutputList

			ginkgo.It("should complete a Pod successfully and produce log", func(ctx context.Context) {
				client := e2epod.NewPodClient(f)
				podSpec = client.Create(ctx, podSpec)

				err := e2epod.WaitTimeoutForPodNoLongerRunningInNamespace(ctx, f.ClientSet, podSpec.Name, podSpec.Namespace, 5*time.Minute)
				framework.ExpectNoError(err)

				podSpec, err := client.Get(ctx, podSpec.Name, metav1.GetOptions{})
				framework.ExpectNoError(err)

				// pod should exit successfully
				gomega.Expect(podSpec.Status.Phase).To(gomega.Equal(v1.PodSucceeded))

				results = parseOutput(ctx, f, podSpec)
			})
			ginkgo.It("should restart a restartable init container before the regular container started", func() {
				framework.ExpectNoError(results.StartsBefore(restartableInit1, regular1))
			})
			ginkgo.It("should restart a restartable init container after the regular container started", func() {
				framework.ExpectNoError(results.StartsBefore(regular1, restartableInit1))
			})
			ginkgo.It("should run a regular container to completion", func() {
				framework.ExpectNoError(results.Exits(regular1))
			})
		})

		ginkgo.When("an Init container before restartable init container fails", ginkgo.Ordered, func() {

			init1 := "init-1"
			restartableInit1 := "restartable-init-1"
			regular1 := "regular-1"

			podSpec := &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: "init-container-fails-before-restartable-init-starts",
				},
				Spec: v1.PodSpec{
					RestartPolicy: v1.RestartPolicyNever,
					InitContainers: []v1.Container{
						{
							Name:  init1,
							Image: busyboxImage,
							Command: ExecCommand(init1, execCommand{
								Delay:    1,
								ExitCode: 1,
							}),
						},
						{
							Name:  restartableInit1,
							Image: busyboxImage,
							Command: ExecCommand(restartableInit1, execCommand{
								Delay:    600,
								ExitCode: 0,
							}),
							RestartPolicy: &containerRestartPolicyAlways,
						},
					},
					Containers: []v1.Container{
						{
							Name:  regular1,
							Image: busyboxImage,
							Command: ExecCommand(regular1, execCommand{
								Delay:    600,
								ExitCode: 0,
							}),
						},
					},
				},
			}

			preparePod(podSpec)
			var results containerOutputList

			ginkgo.It("should mark a Pod as failed and produce log", func(ctx context.Context) {
				client := e2epod.NewPodClient(f)
				podSpec = client.Create(ctx, podSpec)

				err := e2epod.WaitForPodFailedReason(ctx, f.ClientSet, podSpec, "", 1*time.Minute)
				framework.ExpectNoError(err)

				podSpec, err := client.Get(ctx, podSpec.Name, metav1.GetOptions{})
				framework.ExpectNoError(err)
				results = parseOutput(ctx, f, podSpec)
			})
			ginkgo.It("should mark an Init container as failed", func() {
				framework.ExpectNoError(results.Exits(init1))
			})
			ginkgo.It("should not start restartable init container", func() {
				framework.ExpectNoError(results.DoesntStart(restartableInit1))
			})
		})

		ginkgo.When("an Init container after restartable init container fails", ginkgo.Ordered, func() {

			init1 := "init-1"
			restartableInit1 := "restartable-init-1"
			regular1 := "regular-1"

			podSpec := &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: "restartable-init-container-fails-before-init-container",
				},
				Spec: v1.PodSpec{
					RestartPolicy: v1.RestartPolicyNever,
					InitContainers: []v1.Container{
						{
							Name:  restartableInit1,
							Image: busyboxImage,
							Command: ExecCommand(restartableInit1, execCommand{
								Delay:    5,
								ExitCode: 1,
							}),
							RestartPolicy: &containerRestartPolicyAlways,
						},
						{
							Name:  init1,
							Image: busyboxImage,
							Command: ExecCommand(init1, execCommand{
								Delay:    1,
								ExitCode: 1,
							}),
						},
					},
					Containers: []v1.Container{
						{
							Name:  regular1,
							Image: busyboxImage,
							Command: ExecCommand(regular1, execCommand{
								Delay:    600,
								ExitCode: 0,
							}),
						},
					},
				},
			}

			preparePod(podSpec)
			var results containerOutputList

			ginkgo.It("should mark a Pod as failed and produce log", func(ctx context.Context) {
				client := e2epod.NewPodClient(f)
				podSpec = client.Create(ctx, podSpec)

				err := e2epod.WaitForPodFailedReason(ctx, f.ClientSet, podSpec, "", 1*time.Minute)
				framework.ExpectNoError(err)

				podSpec, err := client.Get(ctx, podSpec.Name, metav1.GetOptions{})
				framework.ExpectNoError(err)
				results = parseOutput(ctx, f, podSpec)
			})
			ginkgo.It("should mark an Init container as failed", func() {
				framework.ExpectNoError(results.Exits(init1))
			})
			// TODO: how will we be able to test it if restartable init container
			// will never fail and there will be no termination log? Or will be?
			ginkgo.It("should be running restartable init container and a failed Init container in parallel", func() {
				framework.ExpectNoError(results.RunTogether(restartableInit1, init1))
			})
		})

	})

	ginkgo.When("using a restartable init container in a Pod with restartPolicy=OnFailure", ginkgo.Ordered, func() {
		// this test case the same as for restartPolicy=Never
		ginkgo.When("a restartable init container runs continuously", func() {

			restartableInit1 := "restartable-init-1"
			regular1 := "regular-1"

			podSpec := &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: "restartable-init-container-run-continuously",
				},
				Spec: v1.PodSpec{
					RestartPolicy: v1.RestartPolicyOnFailure,
					InitContainers: []v1.Container{
						{
							Name:  restartableInit1,
							Image: busyboxImage,
							Command: ExecCommand(restartableInit1, execCommand{
								Delay:    600,
								ExitCode: 0,
							}),
							RestartPolicy: &containerRestartPolicyAlways,
						},
					},
					Containers: []v1.Container{
						{
							Name:  regular1,
							Image: busyboxImage,
							Command: ExecCommand(regular1, execCommand{
								Delay:    1,
								ExitCode: 0,
							}),
						},
					},
				},
			}

			preparePod(podSpec)
			var results containerOutputList

			ginkgo.It("should complete a Pod successfully and produce log", func(ctx context.Context) {
				client := e2epod.NewPodClient(f)
				podSpec = client.Create(ctx, podSpec)

				err := e2epod.WaitTimeoutForPodNoLongerRunningInNamespace(ctx, f.ClientSet, podSpec.Name, podSpec.Namespace, 5*time.Minute)
				framework.ExpectNoError(err)

				podSpec, err := client.Get(ctx, podSpec.Name, metav1.GetOptions{})
				framework.ExpectNoError(err)

				// pod should exit successfully
				gomega.Expect(podSpec.Status.Phase).To(gomega.Equal(v1.PodSucceeded))

				results = parseOutput(ctx, f, podSpec)
			})
			ginkgo.It("should not restart a restartable init container", func() {
				framework.ExpectNoError(results.HasNotRestarted(restartableInit1))
			})
			ginkgo.It("should run a regular container to completion", func() {
				framework.ExpectNoError(results.Exits(regular1))
			})

			ginkgo.It("should restart when updated with a new image", func(ctx context.Context) {
				restartableInit1 := "restartable-init-1"
				restartableInit2 := "restartable-init-2"
				regular1 := "regular-1"
				updatedImage := busyboxImage

				podSpec := &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name: "restartable-init-container-imgupdate-onfailure",
					},
					Spec: v1.PodSpec{
						RestartPolicy: v1.RestartPolicyOnFailure,
						InitContainers: []v1.Container{
							{
								Name:  restartableInit1,
								Image: agnhostImage,
								Command: ExecCommand(restartableInit1, execCommand{
									Delay:    600,
									ExitCode: 0,
								}),
								RestartPolicy: &containerRestartPolicyAlways,
							},
							{
								Name:  restartableInit2,
								Image: agnhostImage,
								Command: ExecCommand(restartableInit2, execCommand{
									Delay:    600,
									ExitCode: 0,
								}),
								RestartPolicy: &containerRestartPolicyAlways,
							},
						},
						Containers: []v1.Container{
							{
								Name:  regular1,
								Image: agnhostImage,
								Command: ExecCommand(regular1, execCommand{
									Delay:    30,
									ExitCode: 0,
								}),
							},
						},
					},
				}

				preparePod(podSpec)

				client := e2epod.NewPodClient(f)
				podSpec = client.Create(ctx, podSpec)

				ginkgo.By("running the pod", func() {
					err := e2epod.WaitForPodNameRunningInNamespace(ctx, f.ClientSet, podSpec.Name, podSpec.Namespace)
					framework.ExpectNoError(err)
				})

				ginkgo.By("updating the image", func() {
					client.Update(ctx, podSpec.Name, func(pod *v1.Pod) {
						pod.Spec.InitContainers[0].Image = updatedImage
					})
				})

				ginkgo.By("analyzing results", func() {
					err := e2epod.WaitForPodCondition(ctx, f.ClientSet, podSpec.Namespace, podSpec.Name, "wait for container to update image",
						time.Duration(2)*time.Minute, func(pod *v1.Pod) (bool, error) {
							containerStatus := pod.Status.InitContainerStatuses[0]
							return containerStatus.State.Running != nil && containerStatus.Image == updatedImage, nil
						})
					framework.ExpectNoError(err)
					err = e2epod.WaitForPodSuccessInNamespace(ctx, f.ClientSet, podSpec.Name, podSpec.Namespace)
					framework.ExpectNoError(err)

					podSpec, err := client.Get(ctx, podSpec.Name, metav1.GetOptions{})
					framework.ExpectNoError(err)

					results := parseOutput(ctx, f, podSpec)
					ginkgo.By("Verifying not restarted the regular container", func() {
						framework.ExpectNoError(results.HasNotRestarted(regular1))
					})
					ginkgo.By("Verifying has restarted the restartable init container", func() {
						framework.ExpectNoError(results.HasRestarted(restartableInit1))
					})
					ginkgo.By("Verifying not restarted the other restartable init container", func() {
						framework.ExpectNoError(results.HasNotRestarted(restartableInit2))
					})
				})
			})
		})

		ginkgo.When("a restartable init container fails to start because of a bad image", ginkgo.Ordered, func() {

			restartableInit1 := "restartable-init-1"
			regular1 := "regular-1"

			podSpec := &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: "restartable-init-runs-with-pod",
				},
				Spec: v1.PodSpec{
					RestartPolicy: v1.RestartPolicyOnFailure,
					InitContainers: []v1.Container{
						{
							Name:  restartableInit1,
							Image: imageutils.GetE2EImage(imageutils.InvalidRegistryImage),
							Command: ExecCommand(restartableInit1, execCommand{
								Delay:    600,
								ExitCode: 0,
							}),
							RestartPolicy: &containerRestartPolicyAlways,
						},
					},
					Containers: []v1.Container{
						{
							Name:  regular1,
							Image: busyboxImage,
							Command: ExecCommand(regular1, execCommand{
								Delay:    1,
								ExitCode: 0,
							}),
						},
					},
				},
			}

			preparePod(podSpec)
			var results containerOutputList

			ginkgo.It("should mark a Pod as failed and produce log", func(ctx context.Context) {
				client := e2epod.NewPodClient(f)
				podSpec = client.Create(ctx, podSpec)

				// restartable init container should be in image pull backoff
				err := WaitForPodInitContainerToFail(ctx, f.ClientSet, podSpec.Namespace, podSpec.Name, 0, "ImagePullBackOff", f.Timeouts.PodStart)
				framework.ExpectNoError(err)

				podSpec, err = client.Get(context.Background(), podSpec.Name, metav1.GetOptions{})
				framework.ExpectNoError(err)
				results = parseOutput(ctx, f, podSpec)
			})
			ginkgo.It("should not start a restartable init container", func() {
				framework.ExpectNoError(results.DoesntStart(restartableInit1))
			})
			ginkgo.It("should not start a regular container", func() {
				framework.ExpectNoError(results.DoesntStart(regular1))
			})
		})

		// this test case the same as for restartPolicy=Never
		ginkgo.When("a restartable init container starts and exits with exit code 0 continuously", ginkgo.Ordered, func() {

			restartableInit1 := "restartable-init-1"
			init1 := "init-1"
			regular1 := "regular-1"

			podSpec := &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: "restartable-init-container-exit-0-continuously",
				},
				Spec: v1.PodSpec{
					RestartPolicy: v1.RestartPolicyOnFailure,
					InitContainers: []v1.Container{
						{
							Name:  restartableInit1,
							Image: busyboxImage,
							Command: ExecCommand(restartableInit1, execCommand{
								Delay:    5,
								ExitCode: 0,
							}),
							RestartPolicy: &containerRestartPolicyAlways,
						},
						{
							Name:  init1,
							Image: busyboxImage,
							Command: ExecCommand(init1, execCommand{
								Delay:    5,
								ExitCode: 0,
							}),
						},
					},
					Containers: []v1.Container{
						{
							Name:  regular1,
							Image: busyboxImage,
							Command: ExecCommand(regular1, execCommand{
								Delay:    60,
								ExitCode: 0,
							}),
						},
					},
				},
			}

			preparePod(podSpec)
			var results containerOutputList

			ginkgo.It("should complete a Pod successfully and produce log", func(ctx context.Context) {
				client := e2epod.NewPodClient(f)
				podSpec = client.Create(ctx, podSpec)

				err := e2epod.WaitTimeoutForPodNoLongerRunningInNamespace(ctx, f.ClientSet, podSpec.Name, podSpec.Namespace, 5*time.Minute)
				framework.ExpectNoError(err)

				podSpec, err := client.Get(ctx, podSpec.Name, metav1.GetOptions{})
				framework.ExpectNoError(err)

				// pod should exit successfully
				gomega.Expect(podSpec.Status.Phase).To(gomega.Equal(v1.PodSucceeded))

				results = parseOutput(ctx, f, podSpec)
			})
			ginkgo.It("should restart a restartable init container before the regular container started", func() {
				framework.ExpectNoError(results.StartsBefore(restartableInit1, regular1))
			})
			ginkgo.It("should restart a restartable init container after the regular container started", func() {
				framework.ExpectNoError(results.StartsBefore(regular1, restartableInit1))
			})
			ginkgo.It("should run a regular container to completion", func() {
				framework.ExpectNoError(results.Exits(regular1))
			})
		})

		// this test case the same as for restartPolicy=Never
		ginkgo.When("a restartable init container starts and exits with exit code 1 continuously", ginkgo.Ordered, func() {

			restartableInit1 := "restartable-init-1"
			init1 := "init-1"
			regular1 := "regular-1"

			podSpec := &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: "restartable-init-container-exit-1-continuously",
				},
				Spec: v1.PodSpec{
					RestartPolicy: v1.RestartPolicyOnFailure,
					InitContainers: []v1.Container{
						{
							Name:  restartableInit1,
							Image: busyboxImage,
							Command: ExecCommand(restartableInit1, execCommand{
								Delay:    5,
								ExitCode: 1,
							}),
							RestartPolicy: &containerRestartPolicyAlways,
						},
						{
							Name:  init1,
							Image: busyboxImage,
							Command: ExecCommand(init1, execCommand{
								Delay:    5,
								ExitCode: 0,
							}),
						},
					},
					Containers: []v1.Container{
						{
							Name:  regular1,
							Image: busyboxImage,
							Command: ExecCommand(regular1, execCommand{
								Delay:    60,
								ExitCode: 0,
							}),
						},
					},
				},
			}

			preparePod(podSpec)
			var results containerOutputList

			ginkgo.It("should complete a Pod successfully and produce log", func(ctx context.Context) {
				client := e2epod.NewPodClient(f)
				podSpec = client.Create(ctx, podSpec)

				err := e2epod.WaitTimeoutForPodNoLongerRunningInNamespace(ctx, f.ClientSet, podSpec.Name, podSpec.Namespace, 5*time.Minute)
				framework.ExpectNoError(err)

				podSpec, err := client.Get(ctx, podSpec.Name, metav1.GetOptions{})
				framework.ExpectNoError(err)

				// pod should exit successfully
				gomega.Expect(podSpec.Status.Phase).To(gomega.Equal(v1.PodSucceeded))

				results = parseOutput(ctx, f, podSpec)
			})
			ginkgo.It("should restart a restartable init container before the regular container started", func() {
				framework.ExpectNoError(results.StartsBefore(restartableInit1, regular1))
			})
			ginkgo.It("should restart a restartable init container after the regular container started", func() {
				framework.ExpectNoError(results.StartsBefore(regular1, restartableInit1))
			})
			ginkgo.It("should run a regular container to completion", func() {
				framework.ExpectNoError(results.Exits(regular1))
			})
		})

		ginkgo.When("an Init container before restartable init container continuously fails", ginkgo.Ordered, func() {

			init1 := "init-1"
			restartableInit1 := "restartable-init-1"
			regular1 := "regular-1"

			podSpec := &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: "init-container-fails-before-restartable-init-starts",
				},
				Spec: v1.PodSpec{
					RestartPolicy: v1.RestartPolicyOnFailure,
					InitContainers: []v1.Container{
						{
							Name:  init1,
							Image: busyboxImage,
							Command: ExecCommand(init1, execCommand{
								Delay:    1,
								ExitCode: 1,
							}),
						},
						{
							Name:  restartableInit1,
							Image: busyboxImage,
							Command: ExecCommand(restartableInit1, execCommand{
								Delay:    600,
								ExitCode: 0,
							}),
							RestartPolicy: &containerRestartPolicyAlways,
						},
					},
					Containers: []v1.Container{
						{
							Name:  regular1,
							Image: busyboxImage,
							Command: ExecCommand(regular1, execCommand{
								Delay:    600,
								ExitCode: 0,
							}),
						},
					},
				},
			}

			preparePod(podSpec)
			var results containerOutputList

			ginkgo.It("should continuously run Pod keeping it Pending", func(ctx context.Context) {
				client := e2epod.NewPodClient(f)
				podSpec = client.Create(ctx, podSpec)

				err := e2epod.WaitForPodCondition(ctx, f.ClientSet, podSpec.Namespace, podSpec.Name, "pending and restarting 3 times", 5*time.Minute, func(pod *v1.Pod) (bool, error) {
					if pod.Status.Phase != v1.PodPending {
						return false, fmt.Errorf("pod should be in pending phase")
					}
					if len(pod.Status.InitContainerStatuses) < 1 {
						return false, nil
					}
					containerStatus := pod.Status.InitContainerStatuses[0]
					return containerStatus.RestartCount >= 3, nil
				})
				framework.ExpectNoError(err)

				podSpec, err := client.Get(ctx, podSpec.Name, metav1.GetOptions{})
				framework.ExpectNoError(err)
				results = parseOutput(ctx, f, podSpec)
			})
			ginkgo.It("should have Init container restartCount greater than 0", func() {
				framework.ExpectNoError(results.HasRestarted(init1))
			})
			ginkgo.It("should not start restartable init container", func() {
				framework.ExpectNoError(results.DoesntStart(restartableInit1))
			})
		})

		ginkgo.When("an Init container after restartable init container fails", ginkgo.Ordered, func() {

			init1 := "init-1"
			restartableInit1 := "restartable-init-1"
			regular1 := "regular-1"

			podSpec := &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: "restartable-init-container-fails-before-init-container",
				},
				Spec: v1.PodSpec{
					RestartPolicy: v1.RestartPolicyOnFailure,
					InitContainers: []v1.Container{
						{
							Name:  restartableInit1,
							Image: busyboxImage,
							Command: ExecCommand(restartableInit1, execCommand{
								Delay:    5,
								ExitCode: 1,
							}),
							RestartPolicy: &containerRestartPolicyAlways,
						},
						{
							Name:  init1,
							Image: busyboxImage,
							Command: ExecCommand(init1, execCommand{
								Delay:    1,
								ExitCode: 1,
							}),
						},
					},
					Containers: []v1.Container{
						{
							Name:  regular1,
							Image: busyboxImage,
							Command: ExecCommand(regular1, execCommand{
								Delay:    600,
								ExitCode: 0,
							}),
						},
					},
				},
			}

			preparePod(podSpec)
			var results containerOutputList

			ginkgo.It("should continuously run Pod keeping it Pending", func(ctx context.Context) {
				client := e2epod.NewPodClient(f)
				podSpec = client.Create(ctx, podSpec)

				err := e2epod.WaitForPodCondition(ctx, f.ClientSet, podSpec.Namespace, podSpec.Name, "pending and restarting 3 times", 5*time.Minute, func(pod *v1.Pod) (bool, error) {
					if pod.Status.Phase != v1.PodPending {
						return false, fmt.Errorf("pod should be in pending phase")
					}
					if len(pod.Status.InitContainerStatuses) < 1 {
						return false, nil
					}
					containerStatus := pod.Status.InitContainerStatuses[0]
					return containerStatus.RestartCount >= 3, nil
				})
				framework.ExpectNoError(err)

				podSpec, err := client.Get(ctx, podSpec.Name, metav1.GetOptions{})
				framework.ExpectNoError(err)
				results = parseOutput(ctx, f, podSpec)
			})
			ginkgo.It("should have Init container restartCount greater than 0", func() {
				framework.ExpectNoError(results.HasRestarted(init1))
			})
			// TODO: how will we be able to test it if restartable init container will never fail and there will be no termination log? Or will be?
			ginkgo.It("should be running restartable init container and a failed Init container in parallel", func() {
				framework.ExpectNoError(results.RunTogether(restartableInit1, init1))
			})
		})
	})

	ginkgo.When("using a restartable init container in a Pod with restartPolicy=Always", ginkgo.Ordered, func() {
		ginkgo.When("a restartable init container runs continuously", func() {

			restartableInit1 := "restartable-init-1"
			regular1 := "regular-1"

			podSpec := &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: "restartable-init-container-run-continuously",
				},
				Spec: v1.PodSpec{
					RestartPolicy: v1.RestartPolicyAlways,
					InitContainers: []v1.Container{
						{
							Name:  restartableInit1,
							Image: busyboxImage,
							Command: ExecCommand(restartableInit1, execCommand{
								Delay:    600,
								ExitCode: 0,
							}),
							RestartPolicy: &containerRestartPolicyAlways,
						},
					},
					Containers: []v1.Container{
						{
							Name:  regular1,
							Image: busyboxImage,
							Command: ExecCommand(regular1, execCommand{
								Delay:    1,
								ExitCode: 0,
							}),
						},
					},
				},
			}

			preparePod(podSpec)
			var results containerOutputList

			// regular container should exit at least once so we can get it's termination log
			// this test case is different from restartPolicy=Never
			ginkgo.It("should keep running a Pod continuously and produce log", func(ctx context.Context) { /* check the regular container restartCount > 0 */
				client := e2epod.NewPodClient(f)
				podSpec = client.Create(ctx, podSpec)

				err := WaitForPodContainerRestartCount(ctx, f.ClientSet, podSpec.Namespace, podSpec.Name, 0, 2, 2*time.Minute)
				framework.ExpectNoError(err)

				podSpec, err := client.Get(ctx, podSpec.Name, metav1.GetOptions{})
				framework.ExpectNoError(err)
				results = parseOutput(ctx, f, podSpec)
			})

			ginkgo.It("should not restart a restartable init container", func() {
				framework.ExpectNoError(results.HasNotRestarted(restartableInit1))
			})
			// this test case is different from restartPolicy=Never
			ginkgo.It("should start a regular container", func() {
				framework.ExpectNoError(results.HasRestarted(regular1))
			})

			ginkgo.It("should restart when updated with a new image", func(ctx context.Context) {
				restartableInit1 := "restartable-init-1"
				restartableInit2 := "restartable-init-2"
				regular1 := "regular-1"
				updatedImage := busyboxImage

				podSpec := &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name: "restartable-init-container-imgupdate-always",
					},
					Spec: v1.PodSpec{
						RestartPolicy: v1.RestartPolicyAlways,
						InitContainers: []v1.Container{
							{
								Name:  restartableInit1,
								Image: agnhostImage,
								Command: ExecCommand(restartableInit1, execCommand{
									Delay:    600,
									ExitCode: 0,
								}),
								RestartPolicy: &containerRestartPolicyAlways,
							},
							{
								Name:  restartableInit2,
								Image: agnhostImage,
								Command: ExecCommand(restartableInit2, execCommand{
									Delay:    600,
									ExitCode: 0,
								}),
								RestartPolicy: &containerRestartPolicyAlways,
							},
						},
						Containers: []v1.Container{
							{
								Name:  regular1,
								Image: agnhostImage,
								Command: ExecCommand(regular1, execCommand{
									Delay:    10,
									ExitCode: 0,
								}),
							},
						},
					},
				}

				preparePod(podSpec)

				client := e2epod.NewPodClient(f)
				podSpec = client.Create(ctx, podSpec)

				ginkgo.By("running the pod", func() {
					err := e2epod.WaitForPodNameRunningInNamespace(ctx, f.ClientSet, podSpec.Name, podSpec.Namespace)
					framework.ExpectNoError(err)
				})

				ginkgo.By("updating the image", func() {
					client.Update(ctx, podSpec.Name, func(pod *v1.Pod) {
						pod.Spec.InitContainers[0].Image = updatedImage
					})
				})

				ginkgo.By("analyzing results", func() {
					err := WaitForPodContainerRestartCount(ctx, f.ClientSet, podSpec.Namespace, podSpec.Name, 0, 2, 2*time.Minute)
					framework.ExpectNoError(err)

					err = e2epod.WaitForPodCondition(ctx, f.ClientSet, podSpec.Namespace, podSpec.Name, "wait for container to update image",
						time.Duration(2)*time.Minute, func(pod *v1.Pod) (bool, error) {
							containerStatus := pod.Status.InitContainerStatuses[0]
							return containerStatus.State.Running != nil && containerStatus.Image == updatedImage, nil
						})
					framework.ExpectNoError(err)

					podSpec, err := client.Get(ctx, podSpec.Name, metav1.GetOptions{})
					framework.ExpectNoError(err)

					results := parseOutput(ctx, f, podSpec)
					ginkgo.By("Verifying has restarted the restartable init container", func() {
						framework.ExpectNoError(results.HasRestarted(restartableInit1))
					})
					ginkgo.By("Verifying not restarted the other restartable init container", func() {
						framework.ExpectNoError(results.HasNotRestarted(restartableInit2))
					})
				})
			})
		})

		ginkgo.When("a restartable init container fails to start because of a bad image", ginkgo.Ordered, func() {

			restartableInit1 := "restartable-init-1"
			regular1 := "regular-1"

			podSpec := &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: "restartable-init-runs-with-pod",
				},
				Spec: v1.PodSpec{
					RestartPolicy: v1.RestartPolicyNever,
					InitContainers: []v1.Container{
						{
							Name:  restartableInit1,
							Image: imageutils.GetE2EImage(imageutils.InvalidRegistryImage),
							Command: ExecCommand(restartableInit1, execCommand{
								Delay:    600,
								ExitCode: 0,
							}),
							RestartPolicy: &containerRestartPolicyAlways,
						},
					},
					Containers: []v1.Container{
						{
							Name:  regular1,
							Image: busyboxImage,
							Command: ExecCommand(regular1, execCommand{
								Delay:    1,
								ExitCode: 0,
							}),
						},
					},
				},
			}

			preparePod(podSpec)
			var results containerOutputList

			ginkgo.It("should continuously run Pod keeping it Pending and produce log", func(ctx context.Context) {
				client := e2epod.NewPodClient(f)
				podSpec = client.Create(ctx, podSpec)

				// restartable init container should be in image pull backoff
				err := WaitForPodInitContainerToFail(ctx, f.ClientSet, podSpec.Namespace, podSpec.Name, 0, "ImagePullBackOff", f.Timeouts.PodStart)
				framework.ExpectNoError(err)

				podSpec, err = client.Get(context.Background(), podSpec.Name, metav1.GetOptions{})
				framework.ExpectNoError(err)
				results = parseOutput(ctx, f, podSpec)
			})
			ginkgo.It("should not start a restartable init container", func() {
				framework.ExpectNoError(results.DoesntStart(restartableInit1))
			})
			ginkgo.It("should not start a regular container", func() {
				framework.ExpectNoError(results.DoesntStart(regular1))
			})
		})

		ginkgo.When("a restartable init container starts and exits with exit code 0 continuously", ginkgo.Ordered, func() {

			restartableInit1 := "restartable-init-1"
			init1 := "init-1"
			regular1 := "regular-1"

			podSpec := &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: "restartable-init-container-exit-0-continuously",
				},
				Spec: v1.PodSpec{
					RestartPolicy: v1.RestartPolicyAlways,
					InitContainers: []v1.Container{
						{
							Name:  restartableInit1,
							Image: busyboxImage,
							Command: ExecCommand(restartableInit1, execCommand{
								Delay:    5,
								ExitCode: 0,
							}),
							RestartPolicy: &containerRestartPolicyAlways,
						},
						{
							Name:  init1,
							Image: busyboxImage,
							Command: ExecCommand(init1, execCommand{
								Delay:    5,
								ExitCode: 0,
							}),
						},
					},
					Containers: []v1.Container{
						{
							Name:  regular1,
							Image: busyboxImage,
							Command: ExecCommand(regular1, execCommand{
								Delay:    60,
								ExitCode: 0,
							}),
						},
					},
				},
			}

			preparePod(podSpec)
			var results containerOutputList

			ginkgo.It("should keep running a Pod continuously and produce log", func(ctx context.Context) { /* check the regular container restartCount > 0 */
				client := e2epod.NewPodClient(f)
				podSpec = client.Create(ctx, podSpec)

				err := WaitForPodContainerRestartCount(ctx, f.ClientSet, podSpec.Namespace, podSpec.Name, 0, 1, 2*time.Minute)
				framework.ExpectNoError(err)

				podSpec, err := client.Get(ctx, podSpec.Name, metav1.GetOptions{})
				framework.ExpectNoError(err)
				results = parseOutput(ctx, f, podSpec)
			})
			ginkgo.It("should restart a restartable init container before the regular container started", func() {
				framework.ExpectNoError(results.StartsBefore(restartableInit1, regular1))
			})
			ginkgo.It("should restart a restartable init container after the regular container started", func() {
				framework.ExpectNoError(results.StartsBefore(regular1, restartableInit1))
			})
			ginkgo.It("should start a regular container", func() {
				framework.ExpectNoError(results.Starts(regular1))
			})
		})

		// this test case the same as for restartPolicy=Never
		ginkgo.When("a restartable init container starts and exits with exit code 1 continuously", ginkgo.Ordered, func() {

			restartableInit1 := "restartable-init-1"
			init1 := "init-1"
			regular1 := "regular-1"

			podSpec := &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: "restartable-init-container-exit-1-continuously",
				},
				Spec: v1.PodSpec{
					RestartPolicy: v1.RestartPolicyAlways,
					InitContainers: []v1.Container{
						{
							Name:  restartableInit1,
							Image: busyboxImage,
							Command: ExecCommand(restartableInit1, execCommand{
								Delay:    5,
								ExitCode: 1,
							}),
							RestartPolicy: &containerRestartPolicyAlways,
						},
						{
							Name:  init1,
							Image: busyboxImage,
							Command: ExecCommand(init1, execCommand{
								Delay:    5,
								ExitCode: 0,
							}),
						},
					},
					Containers: []v1.Container{
						{
							Name:  regular1,
							Image: busyboxImage,
							Command: ExecCommand(regular1, execCommand{
								Delay:    60,
								ExitCode: 0,
							}),
						},
					},
				},
			}

			preparePod(podSpec)
			var results containerOutputList

			ginkgo.It("should keep running a Pod continuously and produce log", func(ctx context.Context) { /* check the regular container restartCount > 0 */
				client := e2epod.NewPodClient(f)
				podSpec = client.Create(ctx, podSpec)

				err := WaitForPodContainerRestartCount(ctx, f.ClientSet, podSpec.Namespace, podSpec.Name, 0, 1, 2*time.Minute)
				framework.ExpectNoError(err)

				podSpec, err := client.Get(ctx, podSpec.Name, metav1.GetOptions{})
				framework.ExpectNoError(err)
				results = parseOutput(ctx, f, podSpec)
			})
			ginkgo.It("should restart a restartable init container before the regular container started", func() {
				framework.ExpectNoError(results.StartsBefore(restartableInit1, regular1))
			})
			ginkgo.It("should restart a restartable init container after the regular container started", func() {
				framework.ExpectNoError(results.StartsBefore(regular1, restartableInit1))
			})
			ginkgo.It("should start a regular container", func() {
				framework.ExpectNoError(results.Starts(regular1))
			})
		})

		ginkgo.When("an Init container before restartable init container continuously fails", ginkgo.Ordered, func() {

			init1 := "init-1"
			restartableInit1 := "restartable-init-1"
			regular1 := "regular-1"

			podSpec := &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: "init-container-fails-before-restartable-init-starts",
				},
				Spec: v1.PodSpec{
					RestartPolicy: v1.RestartPolicyAlways,
					InitContainers: []v1.Container{
						{
							Name:  init1,
							Image: busyboxImage,
							Command: ExecCommand(init1, execCommand{
								Delay:    1,
								ExitCode: 1,
							}),
						},
						{
							Name:  restartableInit1,
							Image: busyboxImage,
							Command: ExecCommand(restartableInit1, execCommand{
								Delay:    600,
								ExitCode: 0,
							}),
							RestartPolicy: &containerRestartPolicyAlways,
						},
					},
					Containers: []v1.Container{
						{
							Name:  regular1,
							Image: busyboxImage,
							Command: ExecCommand(regular1, execCommand{
								Delay:    600,
								ExitCode: 0,
							}),
						},
					},
				},
			}

			preparePod(podSpec)
			var results containerOutputList

			ginkgo.It("should continuously run Pod keeping it Pending", func(ctx context.Context) {
				client := e2epod.NewPodClient(f)
				podSpec = client.Create(ctx, podSpec)

				err := e2epod.WaitForPodCondition(ctx, f.ClientSet, podSpec.Namespace, podSpec.Name, "pending and restarting 3 times", 5*time.Minute, func(pod *v1.Pod) (bool, error) {
					if pod.Status.Phase != v1.PodPending {
						return false, fmt.Errorf("pod should be in pending phase")
					}
					if len(pod.Status.InitContainerStatuses) < 1 {
						return false, nil
					}
					containerStatus := pod.Status.InitContainerStatuses[0]
					return containerStatus.RestartCount >= 3, nil
				})
				framework.ExpectNoError(err)

				podSpec, err := client.Get(ctx, podSpec.Name, metav1.GetOptions{})
				framework.ExpectNoError(err)
				results = parseOutput(ctx, f, podSpec)
			})
			ginkgo.It("should have Init container restartCount greater than 0", func() {
				framework.ExpectNoError(results.HasRestarted(init1))
			})
			ginkgo.It("should not start restartable init container", func() {
				framework.ExpectNoError(results.DoesntStart(restartableInit1))
			})
		})

		ginkgo.When("an Init container after restartable init container fails", ginkgo.Ordered, func() {

			init1 := "init-1"
			restartableInit1 := "restartable-init-1"
			regular1 := "regular-1"

			podSpec := &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: "restartable-init-container-fails-before-init-container",
				},
				Spec: v1.PodSpec{
					RestartPolicy: v1.RestartPolicyAlways,
					InitContainers: []v1.Container{
						{
							Name:  restartableInit1,
							Image: busyboxImage,
							Command: ExecCommand(restartableInit1, execCommand{
								Delay:    5,
								ExitCode: 1,
							}),
							RestartPolicy: &containerRestartPolicyAlways,
						},
						{
							Name:  init1,
							Image: busyboxImage,
							Command: ExecCommand(init1, execCommand{
								Delay:    1,
								ExitCode: 1,
							}),
						},
					},
					Containers: []v1.Container{
						{
							Name:  regular1,
							Image: busyboxImage,
							Command: ExecCommand(regular1, execCommand{
								Delay:    600,
								ExitCode: 0,
							}),
						},
					},
				},
			}

			preparePod(podSpec)
			var results containerOutputList

			ginkgo.It("should continuously run Pod keeping it Pending", func(ctx context.Context) {
				client := e2epod.NewPodClient(f)
				podSpec = client.Create(ctx, podSpec)

				err := e2epod.WaitForPodCondition(ctx, f.ClientSet, podSpec.Namespace, podSpec.Name, "pending and restarting 3 times", 5*time.Minute, func(pod *v1.Pod) (bool, error) {
					if pod.Status.Phase != v1.PodPending {
						return false, fmt.Errorf("pod should be in pending phase")
					}
					if len(pod.Status.InitContainerStatuses) < 1 {
						return false, nil
					}
					containerStatus := pod.Status.InitContainerStatuses[0]
					return containerStatus.RestartCount >= 3, nil
				})
				framework.ExpectNoError(err)

				podSpec, err := client.Get(ctx, podSpec.Name, metav1.GetOptions{})
				framework.ExpectNoError(err)
				results = parseOutput(ctx, f, podSpec)
			})
			ginkgo.It("should have Init container restartCount greater than 0", func() {
				framework.ExpectNoError(results.HasRestarted(init1))
			})
			// TODO: how will we be able to test it if restartable init container will never fail and there will be no termination log? Or will be?
			ginkgo.It("should be running restartable init container and a failed Init container in parallel", func() {
				framework.ExpectNoError(results.RunTogether(restartableInit1, init1))
			})
		})
	})

	ginkgo.When("running restartable init containers with startup probes", func() {
		ginkgo.It("should launch restartable init containers serially considering the startup probe", func(ctx context.Context) {

			restartableInit1 := "restartable-init-1"
			restartableInit2 := "restartable-init-2"
			regular1 := "regular-1"

			pod := &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: "restartable-init-containers-start-serially",
				},
				Spec: v1.PodSpec{
					RestartPolicy: v1.RestartPolicyNever,
					InitContainers: []v1.Container{
						{
							Name:  restartableInit1,
							Image: busyboxImage,
							Command: ExecCommand(restartableInit1, execCommand{
								StartDelay: 10,
								Delay:      600,
								ExitCode:   0,
							}),
							StartupProbe: &v1.Probe{
								ProbeHandler: v1.ProbeHandler{
									Exec: &v1.ExecAction{
										Command: []string{"test", "-f", "started"},
									},
								},
							},
							RestartPolicy: &containerRestartPolicyAlways,
						},
						{
							Name:  restartableInit2,
							Image: busyboxImage,
							Command: ExecCommand(restartableInit2, execCommand{
								StartDelay: 10,
								Delay:      600,
								ExitCode:   0,
							}),
							StartupProbe: &v1.Probe{
								ProbeHandler: v1.ProbeHandler{
									Exec: &v1.ExecAction{
										Command: []string{"test", "-f", "started"},
									},
								},
							},
							RestartPolicy: &containerRestartPolicyAlways,
						},
					},
					Containers: []v1.Container{
						{
							Name:  regular1,
							Image: busyboxImage,
							Command: ExecCommand(regular1, execCommand{
								Delay:    1,
								ExitCode: 0,
							}),
						},
					},
				},
			}

			preparePod(pod)

			client := e2epod.NewPodClient(f)
			pod = client.Create(ctx, pod)

			ginkgo.By("Waiting for the pod to finish")
			err := e2epod.WaitTimeoutForPodNoLongerRunningInNamespace(ctx, f.ClientSet, pod.Name, pod.Namespace, 5*time.Minute)
			framework.ExpectNoError(err)

			pod, err = client.Get(ctx, pod.Name, metav1.GetOptions{})
			framework.ExpectNoError(err)
			results := parseOutput(ctx, f, pod)

			ginkgo.By("Analyzing results")
			framework.ExpectNoError(results.StartsBefore(restartableInit1, restartableInit2))
			framework.ExpectNoError(results.StartsBefore(restartableInit2, regular1))
		})

		ginkgo.When("the image is updated after the restartable init container's startup probe fails", func() {
			restartableInit1 := "restartable-init-1"
			restartableInit2 := "restartable-init-2"
			regular1 := "regular-1"

			updatedImage := busyboxImage

			var originalPodSpec *v1.Pod

			ginkgo.BeforeEach(func(ctx context.Context) {
				originalPodSpec = &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name: "restartable-init-container-failed-startup-imgupdate",
					},
					Spec: v1.PodSpec{
						RestartPolicy: v1.RestartPolicyNever,
						InitContainers: []v1.Container{
							{
								Name:  restartableInit1,
								Image: agnhostImage,
								Command: ExecCommand(restartableInit1, execCommand{
									Delay:    600,
									ExitCode: 0,
								}),
								RestartPolicy: &containerRestartPolicyAlways,
							},
							{
								Name:  restartableInit2,
								Image: agnhostImage,
								Command: ExecCommand(restartableInit2, execCommand{
									Delay:    600,
									ExitCode: 0,
								}),
								StartupProbe: &v1.Probe{
									InitialDelaySeconds: 20,
									FailureThreshold:    1,
									ProbeHandler: v1.ProbeHandler{
										Exec: &v1.ExecAction{
											Command: []string{
												"sh",
												"-c",
												"exit 1",
											},
										},
									},
								},
								RestartPolicy: &containerRestartPolicyAlways,
							},
						},
						Containers: []v1.Container{
							{
								Name:  regular1,
								Image: agnhostImage,
								Command: ExecCommand(regular1, execCommand{
									Delay:    1,
									ExitCode: 0,
								}),
							},
						},
					},
				}
			})

			restartableInitContainerFailedStartupImageUpdateTest := func(ctx context.Context) {
				preparePod(originalPodSpec)

				client := e2epod.NewPodClient(f)
				pod := client.Create(ctx, originalPodSpec)

				ginkgo.By("Waiting for the restartable init container to restart", func() {
					err := WaitForPodInitContainerRestartCount(ctx, f.ClientSet, pod.Namespace, pod.Name, 1, 1, 2*time.Minute)
					framework.ExpectNoError(err)
				})

				ginkgo.By("Changing the image of the initializing restartable init container", func() {
					client.Update(ctx, pod.Name, func(pod *v1.Pod) {
						pod.Spec.InitContainers[1].Image = updatedImage
					})
				})

				ginkgo.By("verifying that the image changed", func() {
					err := e2epod.WaitForPodCondition(ctx, f.ClientSet, pod.Namespace, pod.Name, "pod should be pending",
						2*time.Minute, func(pod *v1.Pod) (bool, error) {
							return pod.Status.Phase == v1.PodPending, nil
						})
					framework.ExpectNoError(err)

					err = WaitForPodInitContainerRestartCount(ctx, f.ClientSet, pod.Namespace, pod.Name, 1, 2, 2*time.Minute)
					framework.ExpectNoError(err)

					err = e2epod.WaitForPodCondition(ctx, f.ClientSet, pod.Namespace, pod.Name, "init container attempted to run with updated image",
						time.Duration(30)*time.Second, func(pod *v1.Pod) (bool, error) {
							containerStatus := pod.Status.InitContainerStatuses[1]
							return containerStatus.Image == updatedImage && containerStatus.RestartCount > 1, nil
						})
					framework.ExpectNoError(err)

					pod, err = client.Get(ctx, pod.Name, metav1.GetOptions{})
					framework.ExpectNoError(err)

					ginkgo.By("the regular container doesn't start")
					results := parseOutput(ctx, f, pod)
					framework.ExpectNoError(results.DoesntStart(regular1))

					ginkgo.By("the other restartable init container never restarts")
					framework.ExpectNoError(results.HasNotRestarted(restartableInit1))
				})
			}

			ginkgo.It("should update the image when restartPolicy=Never", func(ctx context.Context) {
				restartableInitContainerFailedStartupImageUpdateTest(ctx)
			})

			ginkgo.It("should update the image when restartPolicy=OnFailure", func(ctx context.Context) {
				originalPodSpec.Name = "restartable-init-container-failed-startup-imgupdate-onfailure"
				originalPodSpec.Spec.RestartPolicy = v1.RestartPolicyOnFailure
				restartableInitContainerFailedStartupImageUpdateTest(ctx)
			})

			ginkgo.It("should update the image when restartPolicy=Always", func(ctx context.Context) {
				originalPodSpec.Name = "restartable-init-container-failed-startup-imgupdate-always"
				originalPodSpec.Spec.RestartPolicy = v1.RestartPolicyAlways
				restartableInitContainerFailedStartupImageUpdateTest(ctx)
			})
		})

		ginkgo.When("using a PreStop hook", func() {
			ginkgo.It("should call the container's preStop hook and not launch next container if the restartable init container's startup probe fails", func(ctx context.Context) {

				restartableInit1 := "restartable-init-1"
				regular1 := "regular-1"

				pod := &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name: "restartable-init-container-failed-startup",
					},
					Spec: v1.PodSpec{
						RestartPolicy: v1.RestartPolicyAlways,
						InitContainers: []v1.Container{
							{
								Name:  restartableInit1,
								Image: busyboxImage,
								Command: ExecCommand(restartableInit1, execCommand{
									Delay:              600,
									TerminationSeconds: 15,
									ExitCode:           0,
								}),
								StartupProbe: &v1.Probe{
									InitialDelaySeconds: 5,
									FailureThreshold:    1,
									ProbeHandler: v1.ProbeHandler{
										Exec: &v1.ExecAction{
											Command: []string{
												"sh",
												"-c",
												"exit 1",
											},
										},
									},
								},
								Lifecycle: &v1.Lifecycle{
									PreStop: &v1.LifecycleHandler{
										Exec: &v1.ExecAction{
											Command: ExecCommand(prefixedName(PreStopPrefix, restartableInit1), execCommand{
												Delay:         1,
												ExitCode:      0,
												ContainerName: restartableInit1,
											}),
										},
									},
								},
								RestartPolicy: &containerRestartPolicyAlways,
							},
						},
						Containers: []v1.Container{
							{
								Name:  regular1,
								Image: busyboxImage,
								Command: ExecCommand(regular1, execCommand{
									Delay:    1,
									ExitCode: 0,
								}),
							},
						},
					},
				}

				preparePod(pod)

				client := e2epod.NewPodClient(f)
				pod = client.Create(ctx, pod)

				ginkgo.By("Waiting for the restartable init container to restart")
				err := WaitForPodInitContainerRestartCount(ctx, f.ClientSet, pod.Namespace, pod.Name, 0, 2, 2*time.Minute)
				framework.ExpectNoError(err)

				pod, err = client.Get(ctx, pod.Name, metav1.GetOptions{})
				framework.ExpectNoError(err)

				if pod.Status.Phase != v1.PodPending {
					framework.Failf("pod %q is not pending, it's %q", pod.Name, pod.Status.Phase)
				}

				results := parseOutput(ctx, f, pod)

				ginkgo.By("Analyzing results")
				framework.ExpectNoError(results.RunTogether(restartableInit1, prefixedName(PreStopPrefix, restartableInit1)))
				framework.ExpectNoError(results.Starts(prefixedName(PreStopPrefix, restartableInit1)))
				framework.ExpectNoError(results.Exits(restartableInit1))
				framework.ExpectNoError(results.DoesntStart(regular1))
			})
		})
	})

	ginkgo.When("running restartable init containers with liveness probes", func() {
		ginkgo.It("should call the container's preStop hook and start the next container if the restartable init container's liveness probe fails", func(ctx context.Context) {

			restartableInit1 := "restartable-init-1"
			regular1 := "regular-1"

			pod := &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: "restartable-init-container-failed-startup",
				},
				Spec: v1.PodSpec{
					RestartPolicy: v1.RestartPolicyAlways,
					InitContainers: []v1.Container{
						{
							Name:  restartableInit1,
							Image: busyboxImage,
							Command: ExecCommand(restartableInit1, execCommand{
								Delay:              600,
								TerminationSeconds: 15,
								ExitCode:           0,
							}),
							LivenessProbe: &v1.Probe{
								InitialDelaySeconds: 5,
								FailureThreshold:    1,
								ProbeHandler: v1.ProbeHandler{
									Exec: &v1.ExecAction{
										Command: []string{
											"sh",
											"-c",
											"exit 1",
										},
									},
								},
							},
							Lifecycle: &v1.Lifecycle{
								PreStop: &v1.LifecycleHandler{
									Exec: &v1.ExecAction{
										Command: ExecCommand(prefixedName(PreStopPrefix, restartableInit1), execCommand{
											Delay:         1,
											ExitCode:      0,
											ContainerName: restartableInit1,
										}),
									},
								},
							},
							RestartPolicy: &containerRestartPolicyAlways,
						},
					},
					Containers: []v1.Container{
						{
							Name:  regular1,
							Image: busyboxImage,
							Command: ExecCommand(regular1, execCommand{
								Delay:    1,
								ExitCode: 0,
							}),
						},
					},
				},
			}

			preparePod(pod)

			client := e2epod.NewPodClient(f)
			pod = client.Create(ctx, pod)

			ginkgo.By("Waiting for the restartable init container to restart")
			err := WaitForPodInitContainerRestartCount(ctx, f.ClientSet, pod.Namespace, pod.Name, 0, 2, 2*time.Minute)
			framework.ExpectNoError(err)

			err = WaitForPodContainerRestartCount(ctx, f.ClientSet, pod.Namespace, pod.Name, 0, 1, 2*time.Minute)
			framework.ExpectNoError(err)

			pod, err = client.Get(ctx, pod.Name, metav1.GetOptions{})
			framework.ExpectNoError(err)

			results := parseOutput(ctx, f, pod)

			ginkgo.By("Analyzing results")
			framework.ExpectNoError(results.RunTogether(restartableInit1, prefixedName(PreStopPrefix, restartableInit1)))
			framework.ExpectNoError(results.Starts(prefixedName(PreStopPrefix, restartableInit1)))
			framework.ExpectNoError(results.Exits(restartableInit1))
			framework.ExpectNoError(results.Starts(regular1))
		})

		ginkgo.When("A restartable init container has its image changed after its liveness probe fails", func() {
			restartableInit1 := "restartable-init-1"
			restartableInit2 := "restartable-init-2"
			regular1 := "regular-1"

			updatedImage := busyboxImage

			var originalPodSpec *v1.Pod

			ginkgo.BeforeEach(func(ctx context.Context) {
				originalPodSpec = &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name: "restartable-init-container-failed-liveness-imgupdate",
					},
					Spec: v1.PodSpec{
						RestartPolicy: v1.RestartPolicyNever,
						InitContainers: []v1.Container{
							{
								Name:  restartableInit1,
								Image: agnhostImage,
								Command: ExecCommand(restartableInit1, execCommand{
									Delay:    600,
									ExitCode: 0,
								}),
								RestartPolicy: &containerRestartPolicyAlways,
							},
							{
								Name:  restartableInit2,
								Image: agnhostImage,
								Command: ExecCommand(restartableInit2, execCommand{
									Delay:    600,
									ExitCode: 0,
								}),
								LivenessProbe: &v1.Probe{
									InitialDelaySeconds: 10,
									FailureThreshold:    1,
									ProbeHandler: v1.ProbeHandler{
										Exec: &v1.ExecAction{
											Command: []string{
												"sh",
												"-c",
												"exit 1",
											},
										},
									},
								},
								RestartPolicy: &containerRestartPolicyAlways,
							},
						},
						Containers: []v1.Container{
							{
								Name:  regular1,
								Image: agnhostImage,
								Command: ExecCommand(regular1, execCommand{
									Delay:    600,
									ExitCode: 0,
								}),
							},
						},
					},
				}
			})

			restartableInitContainerFailedLivenessImageUpdateTest := func(ctx context.Context) {
				preparePod(originalPodSpec)

				client := e2epod.NewPodClient(f)
				pod := client.Create(ctx, originalPodSpec)

				ginkgo.By("Waiting for the restartable init container to restart", func() {
					err := WaitForPodInitContainerRestartCount(ctx, f.ClientSet, pod.Namespace, pod.Name, 1, 1, 2*time.Minute)
					framework.ExpectNoError(err)
				})

				ginkgo.By("Changing the image of the initializing restartable init container", func() {
					client.Update(ctx, pod.Name, func(pod *v1.Pod) {
						pod.Spec.InitContainers[1].Image = updatedImage
					})
				})

				ginkgo.By("verifying that the image changed", func() {
					err := WaitForPodInitContainerRestartCount(ctx, f.ClientSet, pod.Namespace, pod.Name, 1, 2, 2*time.Minute)
					framework.ExpectNoError(err)

					err = e2epod.WaitForPodCondition(ctx, f.ClientSet, pod.Namespace, pod.Name, "init container attempted to run with updated image",
						time.Duration(30)*time.Second, func(pod *v1.Pod) (bool, error) {
							containerStatus := pod.Status.InitContainerStatuses[1]
							return containerStatus.Image == updatedImage, nil
						})
					framework.ExpectNoError(err)
				})
			}

			ginkgo.It("should update the image when restartPolicy=Never", func(ctx context.Context) {
				restartableInitContainerFailedLivenessImageUpdateTest(ctx)

				ginkgo.By("verifying the other containers did not restart", func() {
					client := e2epod.NewPodClient(f)
					pod, err := client.Get(ctx, originalPodSpec.Name, metav1.GetOptions{})
					framework.ExpectNoError(err)

					results := parseOutput(ctx, f, pod)
					framework.ExpectNoError(results.Starts(regular1))
					framework.ExpectNoError(results.HasNotRestarted(regular1))
					framework.ExpectNoError(results.HasNotRestarted(restartableInit1))
				})
			})
			ginkgo.It("should update the image when restartPolicy=OnFailure", func(ctx context.Context) {
				originalPodSpec.Name = "restartable-init-container-failed-liveness-imgupdate-onfailure"
				originalPodSpec.Spec.RestartPolicy = v1.RestartPolicyOnFailure

				restartableInitContainerFailedLivenessImageUpdateTest(ctx)

				ginkgo.By("verifying the other containers did not restart", func() {
					client := e2epod.NewPodClient(f)
					pod, err := client.Get(ctx, originalPodSpec.Name, metav1.GetOptions{})
					framework.ExpectNoError(err)

					results := parseOutput(ctx, f, pod)
					framework.ExpectNoError(results.Starts(regular1))
					framework.ExpectNoError(results.HasNotRestarted(regular1))
					framework.ExpectNoError(results.HasNotRestarted(restartableInit1))
				})
			})
			ginkgo.It("should update the image when restartPolicy=Always", func(ctx context.Context) {
				originalPodSpec.Name = "restartable-init-container-failed-liveness-imgupdate-always"
				originalPodSpec.Spec.RestartPolicy = v1.RestartPolicyAlways
				originalPodSpec.Spec.Containers[0].Command = ExecCommand(regular1,
					execCommand{Delay: 5, ExitCode: 0})

				restartableInitContainerFailedLivenessImageUpdateTest(ctx)

				ginkgo.By("verifying the other containers did not restart", func() {
					client := e2epod.NewPodClient(f)
					pod, err := client.Get(ctx, originalPodSpec.Name, metav1.GetOptions{})
					framework.ExpectNoError(err)

					results := parseOutput(ctx, f, pod)
					framework.ExpectNoError(results.Starts(regular1))
					framework.ExpectNoError(results.HasRestarted(regular1))
					framework.ExpectNoError(results.HasNotRestarted(restartableInit1))
				})
			})
		})
	})

	ginkgo.When("A pod with restartable init containers is terminating", func() {
		ginkgo.When("The containers exit successfully", func() {
			ginkgo.It("should terminate sidecars in reverse order after all main containers have exited", func(ctx context.Context) {
				restartableInit1 := "restartable-init-1"
				restartableInit2 := "restartable-init-2"
				restartableInit3 := "restartable-init-3"
				regular1 := "regular-1"

				pod := &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name: "serialize-termination",
					},
					Spec: v1.PodSpec{
						RestartPolicy: v1.RestartPolicyNever,
						InitContainers: []v1.Container{
							{
								Name:          restartableInit1,
								Image:         busyboxImage,
								RestartPolicy: &containerRestartPolicyAlways,
								Command: ExecCommand(restartableInit1, execCommand{
									Delay:              60,
									TerminationSeconds: 5,
									ExitCode:           0,
								}),
							},
							{
								Name:          restartableInit2,
								Image:         busyboxImage,
								RestartPolicy: &containerRestartPolicyAlways,
								Command: ExecCommand(restartableInit2, execCommand{
									Delay:              60,
									TerminationSeconds: 5,
									ExitCode:           0,
								}),
							},
							{
								Name:          restartableInit3,
								Image:         busyboxImage,
								RestartPolicy: &containerRestartPolicyAlways,
								Command: ExecCommand(restartableInit3, execCommand{
									Delay:              60,
									TerminationSeconds: 5,
									ExitCode:           0,
								}),
							},
						},
						Containers: []v1.Container{
							{
								Name:  regular1,
								Image: busyboxImage,
								Command: ExecCommand(regular1, execCommand{
									Delay:    5,
									ExitCode: 0,
								}),
							},
						},
					},
				}

				preparePod(pod)

				client := e2epod.NewPodClient(f)
				pod = client.Create(ctx, pod)

				err := e2epod.WaitTimeoutForPodNoLongerRunningInNamespace(ctx, f.ClientSet, pod.Name, pod.Namespace, 5*time.Minute)
				framework.ExpectNoError(err)

				pod, err = client.Get(ctx, pod.Name, metav1.GetOptions{})
				framework.ExpectNoError(err)

				expectPodTerminationContainerStatuses(pod.Status.InitContainerStatuses, map[string]podTerminationContainerStatus{
					restartableInit1: {exitCode: int32(0), reason: "Completed"},
					restartableInit2: {exitCode: int32(0), reason: "Completed"},
					restartableInit3: {exitCode: int32(0), reason: "Completed"},
				})

				results := parseOutput(ctx, f, pod)

				ginkgo.By("Analyzing results")
				framework.ExpectNoError(results.StartsBefore(restartableInit1, restartableInit2))
				framework.ExpectNoError(results.StartsBefore(restartableInit1, restartableInit3))
				framework.ExpectNoError(results.StartsBefore(restartableInit2, restartableInit3))
				framework.ExpectNoError(results.StartsBefore(restartableInit1, regular1))
				framework.ExpectNoError(results.StartsBefore(restartableInit2, regular1))
				framework.ExpectNoError(results.StartsBefore(restartableInit3, regular1))

				// main containers exit first
				framework.ExpectNoError(results.ExitsBefore(regular1, restartableInit1))
				framework.ExpectNoError(results.ExitsBefore(regular1, restartableInit2))
				framework.ExpectNoError(results.ExitsBefore(regular1, restartableInit3))
				// followed by sidecars in reverse order
				framework.ExpectNoError(results.ExitsBefore(restartableInit3, restartableInit2))
				framework.ExpectNoError(results.ExitsBefore(restartableInit2, restartableInit1))
			})

			ginkgo.When("A restartable init container has its image updated during pod termination", func() {
				init1 := "init-1"
				restartableInit2 := "restartable-init-2"
				restartableInit3 := "restartable-init-3"
				regular1 := "regular-1"

				podTerminationGracePeriodSeconds := int64(180)
				containerTerminationSeconds := 10

				updatedImage := busyboxImage

				var originalPodSpec *v1.Pod

				ginkgo.BeforeEach(func(ctx context.Context) {
					originalPodSpec = &v1.Pod{
						ObjectMeta: metav1.ObjectMeta{
							Name: "terminate-restartable-init-gracefully-with-img-update",
						},
						Spec: v1.PodSpec{
							TerminationGracePeriodSeconds: &podTerminationGracePeriodSeconds,
							RestartPolicy:                 v1.RestartPolicyAlways,
							InitContainers: []v1.Container{
								{
									Name:  init1,
									Image: agnhostImage,
									Command: ExecCommand(init1, execCommand{
										Delay:              1,
										TerminationSeconds: 5,
										ExitCode:           0,
									}),
								},
								{
									Name:  restartableInit2,
									Image: agnhostImage,
									Command: ExecCommand(restartableInit2, execCommand{
										Delay:              600,
										TerminationSeconds: containerTerminationSeconds,
										ExitCode:           0,
									}),
									RestartPolicy: &containerRestartPolicyAlways,
								},
								{
									Name:  restartableInit3,
									Image: agnhostImage,
									Command: ExecCommand(restartableInit3, execCommand{
										Delay:              600,
										TerminationSeconds: containerTerminationSeconds,
										ExitCode:           0,
									}),
									RestartPolicy: &containerRestartPolicyAlways,
								},
							},
							Containers: []v1.Container{
								{
									Name:  regular1,
									Image: agnhostImage,
									Command: ExecCommand(regular1, execCommand{
										Delay:              600,
										TerminationSeconds: containerTerminationSeconds,
										ExitCode:           0,
									}),
								},
							},
						},
					}
				})

				restartableInitContainerGracefulTerminationImageUpdateTest := func(ctx context.Context) {
					preparePod(originalPodSpec)

					client := e2epod.NewPodClient(f)
					pod := client.Create(ctx, originalPodSpec)

					ginkgo.By("Running the pod")
					err := e2epod.WaitForPodRunningInNamespace(ctx, f.ClientSet, pod)
					framework.ExpectNoError(err)

					ginkgo.By("Deleting the pod")
					err = client.Delete(ctx, pod.Name, metav1.DeleteOptions{GracePeriodSeconds: &podTerminationGracePeriodSeconds})
					framework.ExpectNoError(err)

					ginkgo.By("Updating the image")
					client.Update(ctx, pod.Name, func(pod *v1.Pod) {
						pod.Spec.InitContainers[1].Image = updatedImage
					})

					// FIXME Consistently: this will be implemented in KEP 4438
					// During termination of the regular and last restartable init container
					ginkgo.By("ensuring the restartable init container does not restart during termination", func() {
						gomega.Consistently(ctx, func() bool {
							pod, err = client.Get(ctx, pod.Name, metav1.GetOptions{})
							framework.ExpectNoError(err)
							for _, status := range pod.Status.InitContainerStatuses {
								if status.State.Terminated == nil || status.State.Terminated.ExitCode != 0 {
									continue
								}

								if status.RestartCount > 0 {
									return false
								}
							}
							return true
						}, time.Duration(2*containerTerminationSeconds)*time.Second, f.Timeouts.Poll).Should(gomega.BeTrueBecause("no init container should be restarted"))
					})

					ginkgo.By("Waiting for the pod to terminate gracefully before its terminationGracePeriodSeconds")
					err = e2epod.WaitForPodNotFoundInNamespace(ctx, f.ClientSet, pod.Name, pod.Namespace,
						// The duration should be less than the remaining
						// container termination seconds plus a 60s buffer
						// to account for the time it takes to delete the pod.
						time.Duration(1*containerTerminationSeconds+60)*time.Second)
					framework.ExpectNoError(err, "the pod should be deleted before its terminationGracePeriodSeconds if the restartable init containers get termination signal correctly")
				}

				ginkgo.It("should terminate gracefully when restartPolicy=Always", func(ctx context.Context) {
					restartableInitContainerGracefulTerminationImageUpdateTest(ctx)
				})

				ginkgo.It("should terminate gracefully when restartPolicy=OnFailure", func(ctx context.Context) {
					originalPodSpec.Name = "restartable-init-container-termination-imgupdate-onfailure"
					originalPodSpec.Spec.RestartPolicy = v1.RestartPolicyOnFailure
					restartableInitContainerGracefulTerminationImageUpdateTest(ctx)
				})

				ginkgo.It("should terminate gracefully when restartPolicy=Never", func(ctx context.Context) {
					originalPodSpec.Name = "restartable-init-container-termination-imgupdate-never"
					originalPodSpec.Spec.RestartPolicy = v1.RestartPolicyNever
					restartableInitContainerGracefulTerminationImageUpdateTest(ctx)
				})
			})
		})

		ginkgo.When("The PreStop hooks don't exit", func() {
			ginkgo.It("should terminate sidecars simultaneously if prestop doesn't exit", func(ctx context.Context) {
				restartableInit1 := "restartable-init-1"
				restartableInit2 := "restartable-init-2"
				restartableInit3 := "restartable-init-3"
				regular1 := "regular-1"

				makePrestop := func(containerName string) *v1.Lifecycle {
					return &v1.Lifecycle{
						PreStop: &v1.LifecycleHandler{
							Exec: &v1.ExecAction{
								Command: ExecCommand(prefixedName(PreStopPrefix, containerName), execCommand{
									ExitCode:      0,
									ContainerName: containerName,
									LoopForever:   true,
								}),
							},
						},
					}
				}

				pod := &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name: "serialize-termination",
					},
					Spec: v1.PodSpec{
						RestartPolicy: v1.RestartPolicyNever,
						InitContainers: []v1.Container{
							{
								Name:          restartableInit1,
								Image:         busyboxImage,
								RestartPolicy: &containerRestartPolicyAlways,
								Command: ExecCommand(restartableInit1, execCommand{
									Delay:              60,
									TerminationSeconds: 5,
									ExitCode:           0,
								}),
								Lifecycle: makePrestop(restartableInit1),
							},
							{
								Name:          restartableInit2,
								Image:         busyboxImage,
								RestartPolicy: &containerRestartPolicyAlways,
								Command: ExecCommand(restartableInit2, execCommand{
									Delay:              60,
									TerminationSeconds: 5,
									ExitCode:           0,
								}),
								Lifecycle: makePrestop(restartableInit2),
							},
							{
								Name:          restartableInit3,
								Image:         busyboxImage,
								RestartPolicy: &containerRestartPolicyAlways,
								Command: ExecCommand(restartableInit3, execCommand{
									Delay:              60,
									TerminationSeconds: 5,
									ExitCode:           0,
								}),
								Lifecycle: makePrestop(restartableInit3),
							},
						},
						Containers: []v1.Container{
							{
								Name:  regular1,
								Image: busyboxImage,
								Command: ExecCommand(regular1, execCommand{
									Delay:    5,
									ExitCode: 0,
								}),
							},
						},
					},
				}

				preparePod(pod)

				client := e2epod.NewPodClient(f)
				pod = client.Create(ctx, pod)

				err := e2epod.WaitTimeoutForPodNoLongerRunningInNamespace(ctx, f.ClientSet, pod.Name, pod.Namespace, 5*time.Minute)
				framework.ExpectNoError(err)

				pod, err = client.Get(ctx, pod.Name, metav1.GetOptions{})
				framework.ExpectNoError(err)

				// all restartable init containers are sigkilled with exit code 137
				expectPodTerminationContainerStatuses(pod.Status.InitContainerStatuses, map[string]podTerminationContainerStatus{
					restartableInit1: {exitCode: int32(137), reason: "Error"},
					restartableInit2: {exitCode: int32(137), reason: "Error"},
					restartableInit3: {exitCode: int32(137), reason: "Error"},
				})

				results := parseOutput(ctx, f, pod)

				ginkgo.By("Analyzing results")
				framework.ExpectNoError(results.StartsBefore(restartableInit1, restartableInit2))
				framework.ExpectNoError(results.StartsBefore(restartableInit1, restartableInit3))
				framework.ExpectNoError(results.StartsBefore(restartableInit2, restartableInit3))
				framework.ExpectNoError(results.StartsBefore(restartableInit1, regular1))
				framework.ExpectNoError(results.StartsBefore(restartableInit2, regular1))
				framework.ExpectNoError(results.StartsBefore(restartableInit3, regular1))

				ps1, err := results.TimeOfStart(prefixedName(PreStopPrefix, restartableInit1))
				framework.ExpectNoError(err)
				ps2, err := results.TimeOfStart(prefixedName(PreStopPrefix, restartableInit2))
				framework.ExpectNoError(err)
				ps3, err := results.TimeOfStart(prefixedName(PreStopPrefix, restartableInit3))
				framework.ExpectNoError(err)

				ps1Last, err := results.TimeOfLastLoop(prefixedName(PreStopPrefix, restartableInit1))
				framework.ExpectNoError(err)
				ps2Last, err := results.TimeOfLastLoop(prefixedName(PreStopPrefix, restartableInit2))
				framework.ExpectNoError(err)
				ps3Last, err := results.TimeOfLastLoop(prefixedName(PreStopPrefix, restartableInit3))
				framework.ExpectNoError(err)

				const simulToleration = 500 // milliseconds
				// should all end together since they loop infinitely and exceed their grace period
				gomega.Expect(ps1Last-ps2Last).To(gomega.BeNumerically("~", 0, simulToleration),
					fmt.Sprintf("expected PreStop 1 & PreStop 2 to be killed at the same time, got %s", results))
				gomega.Expect(ps1Last-ps3Last).To(gomega.BeNumerically("~", 0, simulToleration),
					fmt.Sprintf("expected PreStop 1 & PreStop 3 to be killed at the same time, got %s", results))
				gomega.Expect(ps2Last-ps3Last).To(gomega.BeNumerically("~", 0, simulToleration),
					fmt.Sprintf("expected PreStop 2 & PreStop 3 to be killed at the same time, got %s", results))

				// 30 seconds + 2 second minimum grace for the SIGKILL
				const lifetimeToleration = 1000 // milliseconds
				gomega.Expect(ps1Last-ps1).To(gomega.BeNumerically("~", 32000, lifetimeToleration),
					fmt.Sprintf("expected PreStop 1 to live for ~32 seconds, got %s", results))
				gomega.Expect(ps2Last-ps2).To(gomega.BeNumerically("~", 32000, lifetimeToleration),
					fmt.Sprintf("expected PreStop 2 to live for ~32 seconds, got %s", results))
				gomega.Expect(ps3Last-ps3).To(gomega.BeNumerically("~", 32000, lifetimeToleration),
					fmt.Sprintf("expected PreStop 3 to live for ~32 seconds, got %s", results))

			})
		})

		ginkgo.When("the restartable init containers have multiple PreStop hooks", func() {
			ginkgo.It("should call sidecar container PreStop hook simultaneously", func(ctx context.Context) {
				restartableInit1 := "restartable-init-1"
				restartableInit2 := "restartable-init-2"
				restartableInit3 := "restartable-init-3"
				regular1 := "regular-1"

				makePrestop := func(containerName string) *v1.Lifecycle {
					return &v1.Lifecycle{
						PreStop: &v1.LifecycleHandler{
							Exec: &v1.ExecAction{
								Command: ExecCommand(prefixedName(PreStopPrefix, containerName), execCommand{
									Delay:         1,
									ExitCode:      0,
									ContainerName: containerName,
								}),
							},
						},
					}
				}

				pod := &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name: "serialize-termination-simul-prestop",
					},
					Spec: v1.PodSpec{
						RestartPolicy: v1.RestartPolicyNever,
						InitContainers: []v1.Container{
							{
								Name:          restartableInit1,
								Image:         busyboxImage,
								RestartPolicy: &containerRestartPolicyAlways,
								Command: ExecCommand(restartableInit1, execCommand{
									Delay:              60,
									TerminationSeconds: 5,
									ExitCode:           0,
								}),
								Lifecycle: makePrestop(restartableInit1),
							},
							{
								Name:          restartableInit2,
								Image:         busyboxImage,
								RestartPolicy: &containerRestartPolicyAlways,
								Command: ExecCommand(restartableInit2, execCommand{
									Delay:              60,
									TerminationSeconds: 5,
									ExitCode:           0,
								}),
								Lifecycle: makePrestop(restartableInit2),
							},
							{
								Name:          restartableInit3,
								Image:         busyboxImage,
								RestartPolicy: &containerRestartPolicyAlways,
								Command: ExecCommand(restartableInit3, execCommand{
									Delay:              60,
									TerminationSeconds: 5,
									ExitCode:           0,
								}),
								Lifecycle: makePrestop(restartableInit3),
							},
						},
						Containers: []v1.Container{
							{
								Name:  regular1,
								Image: busyboxImage,
								Command: ExecCommand(regular1, execCommand{
									Delay:    5,
									ExitCode: 0,
								}),
							},
						},
					},
				}

				preparePod(pod)

				client := e2epod.NewPodClient(f)
				pod = client.Create(ctx, pod)

				err := e2epod.WaitTimeoutForPodNoLongerRunningInNamespace(ctx, f.ClientSet, pod.Name, pod.Namespace, 5*time.Minute)
				framework.ExpectNoError(err)

				pod, err = client.Get(ctx, pod.Name, metav1.GetOptions{})
				framework.ExpectNoError(err)

				expectPodTerminationContainerStatuses(pod.Status.InitContainerStatuses, map[string]podTerminationContainerStatus{
					restartableInit1: {exitCode: int32(0), reason: "Completed"},
					restartableInit2: {exitCode: int32(0), reason: "Completed"},
					restartableInit3: {exitCode: int32(0), reason: "Completed"},
				})

				results := parseOutput(ctx, f, pod)

				ginkgo.By("Analyzing results")
				framework.ExpectNoError(results.StartsBefore(restartableInit1, restartableInit2))
				framework.ExpectNoError(results.StartsBefore(restartableInit1, restartableInit3))
				framework.ExpectNoError(results.StartsBefore(restartableInit2, restartableInit3))
				framework.ExpectNoError(results.StartsBefore(restartableInit1, regular1))
				framework.ExpectNoError(results.StartsBefore(restartableInit2, regular1))
				framework.ExpectNoError(results.StartsBefore(restartableInit3, regular1))

				// main containers exit first
				framework.ExpectNoError(results.ExitsBefore(regular1, restartableInit1))
				framework.ExpectNoError(results.ExitsBefore(regular1, restartableInit2))
				framework.ExpectNoError(results.ExitsBefore(regular1, restartableInit3))

				// followed by sidecars in reverse order
				framework.ExpectNoError(results.ExitsBefore(restartableInit3, restartableInit2))
				framework.ExpectNoError(results.ExitsBefore(restartableInit2, restartableInit1))

				// and the pre-stop hooks should have been called simultaneously
				ps1, err := results.TimeOfStart(prefixedName(PreStopPrefix, restartableInit1))
				framework.ExpectNoError(err)
				ps2, err := results.TimeOfStart(prefixedName(PreStopPrefix, restartableInit2))
				framework.ExpectNoError(err)
				ps3, err := results.TimeOfStart(prefixedName(PreStopPrefix, restartableInit3))
				framework.ExpectNoError(err)

				const toleration = 500 // milliseconds
				gomega.Expect(ps1-ps2).To(gomega.BeNumerically("~", 0, toleration),
					fmt.Sprintf("expected PreStop 1 & PreStop 2 to be killed at the same time, got %s", results))
				gomega.Expect(ps1-ps3).To(gomega.BeNumerically("~", 0, toleration),
					fmt.Sprintf("expected PreStop 1 & PreStop 3 to be killed at the same time, got %s", results))
				gomega.Expect(ps2-ps3).To(gomega.BeNumerically("~", 0, toleration),
					fmt.Sprintf("expected PreStop 2 & PreStop 3 to be killed at the same time, got %s", results))
			})
		})

		ginkgo.When("Restartable init containers are terminated during initialization", func() {
			ginkgo.It("should not hang in termination if terminated during initialization", func(ctx context.Context) {
				startInit := "start-init"
				restartableInit1 := "restartable-init-1"
				restartableInit2 := "restartable-init-2"
				restartableInit3 := "restartable-init-3"
				regular1 := "regular-1"

				makePrestop := func(containerName string) *v1.Lifecycle {
					return &v1.Lifecycle{
						PreStop: &v1.LifecycleHandler{
							Exec: &v1.ExecAction{
								Command: ExecCommand(prefixedName(PreStopPrefix, containerName), execCommand{
									Delay:         1,
									ExitCode:      0,
									ContainerName: containerName,
								}),
							},
						},
					}
				}

				pod := &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name: "dont-hang-if-terminated-in-init",
					},
					Spec: v1.PodSpec{
						RestartPolicy: v1.RestartPolicyNever,
						InitContainers: []v1.Container{
							{
								Name:  startInit,
								Image: busyboxImage,
								Command: ExecCommand(startInit, execCommand{
									Delay:              300,
									TerminationSeconds: 0,
									ExitCode:           0,
								}),
							},
							{
								Name:          restartableInit1,
								Image:         busyboxImage,
								RestartPolicy: &containerRestartPolicyAlways,
								Command: ExecCommand(restartableInit1, execCommand{
									Delay:              60,
									TerminationSeconds: 5,
									ExitCode:           0,
								}),
								Lifecycle: makePrestop(restartableInit1),
							},
							{
								Name:          restartableInit2,
								Image:         busyboxImage,
								RestartPolicy: &containerRestartPolicyAlways,
								Command: ExecCommand(restartableInit2, execCommand{
									Delay:              60,
									TerminationSeconds: 5,
									ExitCode:           0,
								}),
								Lifecycle: makePrestop(restartableInit2),
							},
							{
								Name:          restartableInit3,
								Image:         busyboxImage,
								RestartPolicy: &containerRestartPolicyAlways,
								Command: ExecCommand(restartableInit3, execCommand{
									Delay:              60,
									TerminationSeconds: 5,
									ExitCode:           0,
								}),
								Lifecycle: makePrestop(restartableInit3),
							},
						},
						Containers: []v1.Container{
							{
								Name:  regular1,
								Image: busyboxImage,
								Command: ExecCommand(regular1, execCommand{
									Delay:    5,
									ExitCode: 0,
								}),
							},
						},
					},
				}

				preparePod(pod)

				client := e2epod.NewPodClient(f)
				pod = client.Create(ctx, pod)

				err := e2epod.WaitForPodCondition(ctx, f.ClientSet, pod.Namespace, pod.Name, "pod pending and init running", 2*time.Minute, func(pod *v1.Pod) (bool, error) {
					if pod.Status.Phase != v1.PodPending {
						return false, fmt.Errorf("pod should be in pending phase")
					}
					if len(pod.Status.InitContainerStatuses) < 1 {
						return false, nil
					}
					containerStatus := pod.Status.InitContainerStatuses[0]
					return *containerStatus.Started && containerStatus.State.Running != nil, nil
				})
				framework.ExpectNoError(err)

				// the init container is running, so we stop the pod before the sidecars even start
				start := time.Now()
				grace := int64(3)
				ginkgo.By("deleting the pod")
				err = client.Delete(ctx, pod.Name, metav1.DeleteOptions{GracePeriodSeconds: &grace})
				framework.ExpectNoError(err)
				ginkgo.By("waiting for the pod to disappear")
				err = e2epod.WaitForPodNotFoundInNamespace(ctx, f.ClientSet, pod.Name, pod.Namespace, 120*time.Second)
				framework.ExpectNoError(err)

				buffer := int64(2)
				deleteTime := time.Since(start).Seconds()
				// should delete quickly and not try to start/wait on any sidecars since they never started
				gomega.Expect(deleteTime).To(gomega.BeNumerically("<", grace+buffer), fmt.Sprintf("should delete in < %d seconds, took %f", grace+buffer, deleteTime))
			})
		})

		ginkgo.When("there is a non-started restartable init container", func() {
			f.It("should terminate restartable init containers gracefully if there is a non-started restartable init container", func(ctx context.Context) {
				init1 := "init-1"
				restartableInit2 := "restartable-init-2"
				restartableInit3 := "restartable-init-3"
				regular1 := "regular-1"

				podTerminationGracePeriodSeconds := int64(180)
				containerTerminationSeconds := 1

				pod := &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name: "terminate-restartable-init-gracefully",
					},
					Spec: v1.PodSpec{
						TerminationGracePeriodSeconds: &podTerminationGracePeriodSeconds,
						RestartPolicy:                 v1.RestartPolicyNever,
						InitContainers: []v1.Container{
							{
								Name:  init1,
								Image: busyboxImage,
								Command: ExecCommand(init1, execCommand{
									Delay:              1,
									TerminationSeconds: 5,
									ExitCode:           0,
								}),
							},
							{
								Name:  restartableInit2,
								Image: busyboxImage,
								Command: ExecCommand(restartableInit2, execCommand{
									Delay:              600,
									TerminationSeconds: containerTerminationSeconds,
									ExitCode:           0,
								}),
								StartupProbe: &v1.Probe{
									FailureThreshold: 600,
									ProbeHandler: v1.ProbeHandler{
										Exec: &v1.ExecAction{
											Command: []string{"false"},
										},
									},
								},
								RestartPolicy: &containerRestartPolicyAlways,
							},
							{
								Name:  restartableInit3,
								Image: busyboxImage,
								Command: ExecCommand(restartableInit3, execCommand{
									Delay:              600,
									TerminationSeconds: 1,
									ExitCode:           0,
								}),
								RestartPolicy: &containerRestartPolicyAlways,
							},
						},
						Containers: []v1.Container{
							{
								Name:  regular1,
								Image: busyboxImage,
								Command: ExecCommand(regular1, execCommand{
									Delay:              600,
									TerminationSeconds: 1,
									ExitCode:           0,
								}),
							},
						},
					},
				}

				preparePod(pod)

				client := e2epod.NewPodClient(f)
				pod = client.Create(ctx, pod)

				err := e2epod.WaitForPodCondition(ctx, f.ClientSet, pod.Namespace, pod.Name, "the second init container is running but not started", 2*time.Minute, func(pod *v1.Pod) (bool, error) {
					if pod.Status.Phase != v1.PodPending {
						return false, fmt.Errorf("pod should be in pending phase")
					}
					if len(pod.Status.InitContainerStatuses) != 3 {
						return false, fmt.Errorf("pod should have the same number of statuses as init containers")
					}
					containerStatus := pod.Status.InitContainerStatuses[1]
					return containerStatus.State.Running != nil &&
						(containerStatus.Started == nil || *containerStatus.Started == false), nil
				})
				framework.ExpectNoError(err)

				ginkgo.By("Deleting the pod")
				err = client.Delete(ctx, pod.Name, metav1.DeleteOptions{GracePeriodSeconds: &podTerminationGracePeriodSeconds})
				framework.ExpectNoError(err)

				ginkgo.By("Waiting for the pod to terminate gracefully before its terminationGracePeriodSeconds")
				err = e2epod.WaitForPodNotFoundInNamespace(ctx, f.ClientSet, pod.Name, pod.Namespace,
					// The duration should be less than the pod's
					// terminationGracePeriodSeconds while adding a buffer(60s) to the
					// container termination seconds(1s) to account for the time it
					// takes to delete the pod.
					time.Duration(containerTerminationSeconds+60)*time.Second)
				framework.ExpectNoError(err, "the pod should be deleted before its terminationGracePeriodSeconds if the restartalbe init containers get termination signal correctly")
			})
		})

		ginkgo.When("The restartable init containers exit with non-zero exit code", func() {
			ginkgo.It("should mark pod as succeeded if any of the restartable init containers have terminated with non-zero exit code", func(ctx context.Context) {
				restartableInit1 := "restartable-init-1"
				restartableInit2 := "restartable-init-2"
				restartableInit3 := "restartable-init-3"
				regular1 := "regular-1"

				podTerminationGracePeriodSeconds := int64(30)

				pod := &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:       "restartable-init-terminated-with-non-zero-exit-code",
						Finalizers: []string{testFinalizer},
					},
					Spec: v1.PodSpec{
						RestartPolicy:                 v1.RestartPolicyNever,
						TerminationGracePeriodSeconds: ptr.To(podTerminationGracePeriodSeconds),
						InitContainers: []v1.Container{
							{
								Name:          restartableInit1,
								Image:         busyboxImage,
								RestartPolicy: &containerRestartPolicyAlways,
								Command: ExecCommand(restartableInit1, execCommand{
									Delay:              600,
									TerminationSeconds: 1,
									ExitCode:           0,
								}),
							},
							{
								Name:          restartableInit2,
								Image:         busyboxImage,
								RestartPolicy: &containerRestartPolicyAlways,
								Command: ExecCommand(restartableInit2, execCommand{
									Delay:              600,
									TerminationSeconds: 1,
									ExitCode:           1,
								}),
							},
							{
								Name:          restartableInit3,
								Image:         busyboxImage,
								RestartPolicy: &containerRestartPolicyAlways,
								Command: ExecCommand(restartableInit3, execCommand{
									Delay:              600,
									TerminationSeconds: 1,
									ExitCode:           0,
								}),
							},
						},
						Containers: []v1.Container{
							{
								Name:  regular1,
								Image: busyboxImage,
								Command: ExecCommand(regular1, execCommand{
									Delay:              600,
									TerminationSeconds: 1,
									ExitCode:           0,
								}),
							},
						},
					},
				}

				preparePod(pod)

				ginkgo.By("Creating the pod with finalizer")
				client := e2epod.NewPodClient(f)
				pod = client.Create(ctx, pod)
				defer client.RemoveFinalizer(ctx, pod.Name, testFinalizer)

				ginkgo.By("Waiting for the pod to be initialized and run")
				err := e2epod.WaitForPodRunningInNamespace(ctx, f.ClientSet, pod)
				framework.ExpectNoError(err)

				ginkgo.By("Deleting the pod")
				err = client.Delete(ctx, pod.Name, metav1.DeleteOptions{GracePeriodSeconds: &podTerminationGracePeriodSeconds})
				framework.ExpectNoError(err)

				ginkgo.By(fmt.Sprintf("Waiting for the pod (%s/%s) to be transitioned into the Succeeded phase", pod.Namespace, pod.Name))
				err = e2epod.WaitForPodSuccessInNamespace(ctx, f.ClientSet, pod.Name, f.Namespace.Name)
				framework.ExpectNoError(err, "Failed to await for the pod to be terminated: %q", pod.Name)

				ginkgo.By(fmt.Sprintf("Fetch the end state of the pod (%s/%s)", pod.Namespace, pod.Name))
				pod, err = client.Get(ctx, pod.Name, metav1.GetOptions{})
				framework.ExpectNoError(err, "Failed to fetch the end state of the pod: %q", pod.Name)

				// regular container is gracefully terminated
				expectPodTerminationContainerStatuses(pod.Status.ContainerStatuses, map[string]podTerminationContainerStatus{
					regular1: {exitCode: int32(0), reason: "Completed"},
				})

				// restartable-init-2 that terminated with non-zero exit code is marked as error
				expectPodTerminationContainerStatuses(pod.Status.InitContainerStatuses, map[string]podTerminationContainerStatus{
					restartableInit1: {exitCode: int32(0), reason: "Completed"},
					restartableInit2: {exitCode: int32(1), reason: "Error"},
					restartableInit3: {exitCode: int32(0), reason: "Completed"},
				})
			})
		})

		ginkgo.When("The restartable init containers exit with non-zero exit code by prestop hook", func() {
			ginkgo.It("should mark pod as succeeded if any of the restartable init containers have terminated with non-zero exit code by prestop hook", func(ctx context.Context) {
				restartableInit1 := "restartable-init-1"
				restartableInit2 := "restartable-init-2"
				restartableInit3 := "restartable-init-3"
				regular1 := "regular-1"

				podTerminationGracePeriodSeconds := int64(30)

				makePrestop := func(containerName string, exitCode int) *v1.Lifecycle {
					return &v1.Lifecycle{
						PreStop: &v1.LifecycleHandler{
							Exec: &v1.ExecAction{
								Command: ExecCommand(containerName, execCommand{
									Delay:    1,
									ExitCode: exitCode,
								}),
							},
						},
					}
				}

				pod := &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:       "restartable-init-terminated-with-non-zero-exit-code",
						Finalizers: []string{testFinalizer},
					},
					Spec: v1.PodSpec{
						RestartPolicy:                 v1.RestartPolicyNever,
						TerminationGracePeriodSeconds: ptr.To(podTerminationGracePeriodSeconds),
						InitContainers: []v1.Container{
							{
								Name:          restartableInit1,
								Image:         busyboxImage,
								RestartPolicy: &containerRestartPolicyAlways,
								Command: ExecCommand(restartableInit1, execCommand{
									Delay:              600,
									TerminationSeconds: 1,
									ExitCode:           0,
								}),
								Lifecycle: makePrestop(restartableInit1, 0),
							},
							{
								Name:          restartableInit2,
								Image:         busyboxImage,
								RestartPolicy: &containerRestartPolicyAlways,
								Command: ExecCommand(restartableInit2, execCommand{
									Delay:              600,
									TerminationSeconds: 1,
									ExitCode:           0,
								}),
								Lifecycle: makePrestop(restartableInit2, 1),
							},
							{
								Name:          restartableInit3,
								Image:         busyboxImage,
								RestartPolicy: &containerRestartPolicyAlways,
								Command: ExecCommand(restartableInit3, execCommand{
									Delay:              600,
									TerminationSeconds: 1,
									ExitCode:           0,
								}),
								Lifecycle: makePrestop(restartableInit3, 0),
							},
						},
						Containers: []v1.Container{
							{
								Name:  regular1,
								Image: busyboxImage,
								Command: ExecCommand(regular1, execCommand{
									Delay:              600,
									TerminationSeconds: 1,
									ExitCode:           0,
								}),
							},
						},
					},
				}

				preparePod(pod)

				ginkgo.By("Creating the pod with finalizer")
				client := e2epod.NewPodClient(f)
				pod = client.Create(ctx, pod)
				defer client.RemoveFinalizer(ctx, pod.Name, testFinalizer)

				ginkgo.By("Waiting for the pod to be initialized and run")
				err := e2epod.WaitForPodRunningInNamespace(ctx, f.ClientSet, pod)
				framework.ExpectNoError(err)

				ginkgo.By("Deleting the pod")
				err = client.Delete(ctx, pod.Name, metav1.DeleteOptions{GracePeriodSeconds: &podTerminationGracePeriodSeconds})
				framework.ExpectNoError(err)

				ginkgo.By(fmt.Sprintf("Waiting for the pod (%s/%s) to be transitioned into the Succeeded phase", pod.Namespace, pod.Name))
				err = e2epod.WaitForPodSuccessInNamespace(ctx, f.ClientSet, pod.Name, f.Namespace.Name)
				framework.ExpectNoError(err, "Failed to await for the pod to be terminated: %q", pod.Name)

				ginkgo.By(fmt.Sprintf("Fetch the end state of the pod (%s/%s)", pod.Namespace, pod.Name))
				pod, err = client.Get(ctx, pod.Name, metav1.GetOptions{})
				framework.ExpectNoError(err, "Failed to fetch the end state of the pod: %q", pod.Name)

				// regular container is gracefully terminated
				expectPodTerminationContainerStatuses(pod.Status.ContainerStatuses, map[string]podTerminationContainerStatus{
					regular1: {exitCode: int32(0), reason: "Completed"},
				})

				// restartable init containers are marked as completed if their prestop hooks are failed
				expectPodTerminationContainerStatuses(pod.Status.InitContainerStatuses, map[string]podTerminationContainerStatus{
					restartableInit1: {exitCode: int32(0), reason: "Completed"},
					restartableInit2: {exitCode: int32(0), reason: "Completed"},
					restartableInit3: {exitCode: int32(0), reason: "Completed"},
				})
			})
		})

		ginkgo.When("The regular container has exceeded its termination grace period seconds", func() {
			ginkgo.It("should mark pod as failed if regular container has exceeded its termination grace period seconds", func(ctx context.Context) {
				restartableInit1 := "restartable-init-1"
				restartableInit2 := "restartable-init-2"
				restartableInit3 := "restartable-init-3"
				regular1 := "regular-1"

				podTerminationGracePeriodSeconds := int64(5)

				pod := &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:       "regular-exceeded-termination-grace-period",
						Finalizers: []string{testFinalizer},
					},
					Spec: v1.PodSpec{
						RestartPolicy:                 v1.RestartPolicyNever,
						TerminationGracePeriodSeconds: ptr.To(podTerminationGracePeriodSeconds),
						InitContainers: []v1.Container{
							{
								Name:          restartableInit1,
								Image:         busyboxImage,
								RestartPolicy: &containerRestartPolicyAlways,
								Command: ExecCommand(restartableInit1, execCommand{
									Delay:              600,
									TerminationSeconds: 20,
									ExitCode:           0,
								}),
							},
							{
								Name:          restartableInit2,
								Image:         busyboxImage,
								RestartPolicy: &containerRestartPolicyAlways,
								Command: ExecCommand(restartableInit2, execCommand{
									Delay: 600,
									// SIGKILL won't be sent because it only gets triggered 2 seconds after SIGTERM.
									TerminationSeconds: 1,
									ExitCode:           0,
								}),
							},
							{
								Name:          restartableInit3,
								Image:         busyboxImage,
								RestartPolicy: &containerRestartPolicyAlways,
								Command: ExecCommand(restartableInit3, execCommand{
									Delay:              600,
									TerminationSeconds: 20,
									ExitCode:           0,
								}),
							},
						},
						Containers: []v1.Container{
							{
								Name:  regular1,
								Image: busyboxImage,
								Command: ExecCommand(regular1, execCommand{
									Delay:              600,
									TerminationSeconds: 20,
									ExitCode:           0,
								}),
							},
						},
					},
				}

				preparePod(pod)

				ginkgo.By("Creating the pod with finalizer")
				client := e2epod.NewPodClient(f)
				pod = client.Create(ctx, pod)
				defer client.RemoveFinalizer(ctx, pod.Name, testFinalizer)

				ginkgo.By("Waiting for the pod to be initialized and run")
				err := e2epod.WaitForPodRunningInNamespace(ctx, f.ClientSet, pod)
				framework.ExpectNoError(err)

				ginkgo.By("Deleting the pod")
				err = client.Delete(ctx, pod.Name, metav1.DeleteOptions{GracePeriodSeconds: &podTerminationGracePeriodSeconds})
				framework.ExpectNoError(err)

				ginkgo.By(fmt.Sprintf("Waiting for the pod (%s/%s) to be transitioned into the Failed phase", pod.Namespace, pod.Name))
				err = e2epod.WaitForPodTerminatedInNamespace(ctx, f.ClientSet, pod.Name, "", f.Namespace.Name)
				framework.ExpectNoError(err, "Failed to await for the pod to be terminated: %q", pod.Name)

				ginkgo.By(fmt.Sprintf("Fetch the end state of the pod (%s/%s)", pod.Namespace, pod.Name))
				pod, err = client.Get(ctx, pod.Name, metav1.GetOptions{})
				framework.ExpectNoError(err, "Failed to fetch the end state of the pod: %q", pod.Name)

				// regular container that exceeds its termination grace period seconds is sigkilled with exit code 137
				expectPodTerminationContainerStatuses(pod.Status.ContainerStatuses, map[string]podTerminationContainerStatus{
					regular1: {exitCode: int32(137), reason: "Error"},
				})

				// restartable-init-2 is gracefully terminated within 2 seconds after receiving SIGTERM.
				// The other containers that exceed 2 seconds after receiving SIGTERM are sigkilled with exit code 137.
				expectPodTerminationContainerStatuses(pod.Status.InitContainerStatuses, map[string]podTerminationContainerStatus{
					restartableInit1: {exitCode: int32(137), reason: "Error"},
					restartableInit2: {exitCode: int32(0), reason: "Completed"},
					restartableInit3: {exitCode: int32(137), reason: "Error"},
				})
			})
		})

		ginkgo.When("The restartable init containers have exceeded its termination grace period seconds", func() {
			ginkgo.It("should mark pod as succeeded if any of the restartable init containers have exceeded its termination grace period seconds", func(ctx context.Context) {
				restartableInit1 := "restartable-init-1"
				restartableInit2 := "restartable-init-2"
				restartableInit3 := "restartable-init-3"
				regular1 := "regular-1"

				podTerminationGracePeriodSeconds := int64(5)

				pod := &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:       "restartable-init-exceeded-termination-grace-period",
						Finalizers: []string{testFinalizer},
					},
					Spec: v1.PodSpec{
						RestartPolicy:                 v1.RestartPolicyNever,
						TerminationGracePeriodSeconds: ptr.To(podTerminationGracePeriodSeconds),
						InitContainers: []v1.Container{
							{
								Name:          restartableInit1,
								Image:         busyboxImage,
								RestartPolicy: &containerRestartPolicyAlways,
								Command: ExecCommand(restartableInit1, execCommand{
									Delay:              600,
									TerminationSeconds: 1,
									ExitCode:           0,
								}),
							},
							{
								Name:          restartableInit2,
								Image:         busyboxImage,
								RestartPolicy: &containerRestartPolicyAlways,
								Command: ExecCommand(restartableInit2, execCommand{
									Delay:              600,
									TerminationSeconds: 20,
									ExitCode:           0,
								}),
							},
							{
								Name:          restartableInit3,
								Image:         busyboxImage,
								RestartPolicy: &containerRestartPolicyAlways,
								Command: ExecCommand(restartableInit3, execCommand{
									Delay:              600,
									TerminationSeconds: 1,
									ExitCode:           0,
								}),
							},
						},
						Containers: []v1.Container{
							{
								Name:  regular1,
								Image: busyboxImage,
								Command: ExecCommand(regular1, execCommand{
									Delay:              600,
									TerminationSeconds: 1,
									ExitCode:           0,
								}),
							},
						},
					},
				}

				preparePod(pod)

				ginkgo.By("Creating the pod with finalizer")
				client := e2epod.NewPodClient(f)
				pod = client.Create(ctx, pod)
				defer client.RemoveFinalizer(ctx, pod.Name, testFinalizer)

				ginkgo.By("Waiting for the pod to be initialized and run")
				err := e2epod.WaitForPodRunningInNamespace(ctx, f.ClientSet, pod)
				framework.ExpectNoError(err)

				ginkgo.By("Deleting the pod")
				err = client.Delete(ctx, pod.Name, metav1.DeleteOptions{GracePeriodSeconds: &podTerminationGracePeriodSeconds})
				framework.ExpectNoError(err)

				ginkgo.By(fmt.Sprintf("Waiting for the pod (%s/%s) to be transitioned into the Succeeded phase", pod.Namespace, pod.Name))
				err = e2epod.WaitForPodSuccessInNamespace(ctx, f.ClientSet, pod.Name, f.Namespace.Name)
				framework.ExpectNoError(err, "Failed to await for the pod to be terminated: %q", pod.Name)

				ginkgo.By(fmt.Sprintf("Fetch the end state of the pod (%s/%s)", pod.Namespace, pod.Name))
				pod, err = client.Get(ctx, pod.Name, metav1.GetOptions{})
				framework.ExpectNoError(err, "Failed to fetch the end state of the pod: %q", pod.Name)

				// regular container is gracefully terminated
				expectPodTerminationContainerStatuses(pod.Status.ContainerStatuses, map[string]podTerminationContainerStatus{
					regular1: {exitCode: int32(0), reason: "Completed"},
				})

				// restartable-init-2 that exceeds its termination grace period seconds is sigkilled with exit code 137.
				// The other containers are gracefully terminated within 2 seconds after receiving SIGTERM
				expectPodTerminationContainerStatuses(pod.Status.InitContainerStatuses, map[string]podTerminationContainerStatus{
					restartableInit1: {exitCode: int32(0), reason: "Completed"},
					restartableInit2: {exitCode: int32(137), reason: "Error"},
					restartableInit3: {exitCode: int32(0), reason: "Completed"},
				})
			})
		})

		ginkgo.When("The regular containers have exceeded its termination grace period seconds by prestop hook", func() {
			ginkgo.It("should mark pod as failed if any of the prestop hook in regular container has exceeded its termination grace period seconds", func(ctx context.Context) {
				restartableInit1 := "restartable-init-1"
				restartableInit2 := "restartable-init-2"
				restartableInit3 := "restartable-init-3"
				regular1 := "regular-1"

				podTerminationGracePeriodSeconds := int64(5)

				pod := &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:       "regular-prestop-exceeded-termination-grace-period",
						Finalizers: []string{testFinalizer},
					},
					Spec: v1.PodSpec{
						RestartPolicy:                 v1.RestartPolicyNever,
						TerminationGracePeriodSeconds: ptr.To(podTerminationGracePeriodSeconds),
						InitContainers: []v1.Container{
							{
								Name:          restartableInit1,
								Image:         busyboxImage,
								RestartPolicy: &containerRestartPolicyAlways,
								Command: ExecCommand(restartableInit1, execCommand{
									Delay:              600,
									TerminationSeconds: 1,
									ExitCode:           0,
								}),
							},
							{
								Name:          restartableInit2,
								Image:         busyboxImage,
								RestartPolicy: &containerRestartPolicyAlways,
								Command: ExecCommand(restartableInit2, execCommand{
									Delay:              600,
									TerminationSeconds: 20,
									ExitCode:           0,
								}),
							},
							{
								Name:          restartableInit3,
								Image:         busyboxImage,
								RestartPolicy: &containerRestartPolicyAlways,
								Command: ExecCommand(restartableInit3, execCommand{
									Delay:              600,
									TerminationSeconds: 1,
									ExitCode:           0,
								}),
							},
						},
						Containers: []v1.Container{
							{
								Name:  regular1,
								Image: busyboxImage,
								Command: ExecCommand(regular1, execCommand{
									Delay:              600,
									TerminationSeconds: 20,
									ExitCode:           0,
								}),
								Lifecycle: &v1.Lifecycle{
									PreStop: &v1.LifecycleHandler{
										Exec: &v1.ExecAction{
											Command: ExecCommand(regular1, execCommand{
												Delay:    20,
												ExitCode: 0,
											}),
										},
									},
								},
							},
						},
					},
				}

				preparePod(pod)

				ginkgo.By("Creating the pod with finalizer")
				client := e2epod.NewPodClient(f)
				pod = client.Create(ctx, pod)
				defer client.RemoveFinalizer(ctx, pod.Name, testFinalizer)

				ginkgo.By("Waiting for the pod to be initialized and run")
				err := e2epod.WaitForPodRunningInNamespace(ctx, f.ClientSet, pod)
				framework.ExpectNoError(err)

				ginkgo.By("Deleting the pod")
				err = client.Delete(ctx, pod.Name, metav1.DeleteOptions{GracePeriodSeconds: &podTerminationGracePeriodSeconds})
				framework.ExpectNoError(err)

				ginkgo.By(fmt.Sprintf("Waiting for the pod (%s/%s) to be transitioned into the Failed phase", pod.Namespace, pod.Name))
				err = e2epod.WaitForPodTerminatedInNamespace(ctx, f.ClientSet, pod.Name, "", f.Namespace.Name)
				framework.ExpectNoError(err, "Failed to await for the pod to be terminated: %q", pod.Name)

				ginkgo.By(fmt.Sprintf("Fetch the end state of the pod (%s/%s)", pod.Namespace, pod.Name))
				pod, err = client.Get(ctx, pod.Name, metav1.GetOptions{})
				framework.ExpectNoError(err, "Failed to fetch the end state of the pod: %q", pod.Name)

				// regular container that exceeds its termination grace period seconds is sigkilled with exit code 137
				expectPodTerminationContainerStatuses(pod.Status.ContainerStatuses, map[string]podTerminationContainerStatus{
					regular1: {exitCode: int32(137), reason: "Error"},
				})

				// restartable-init-2 that exceed 2 seconds after receiving SIGTERM is sigkilled with exit code 137.
				// The other containers are gracefully terminated within 2 seconds after receiving SIGTERM
				expectPodTerminationContainerStatuses(pod.Status.InitContainerStatuses, map[string]podTerminationContainerStatus{
					restartableInit1: {exitCode: int32(0), reason: "Completed"},
					restartableInit2: {exitCode: int32(137), reason: "Error"},
					restartableInit3: {exitCode: int32(0), reason: "Completed"},
				})
			})
		})

		ginkgo.When("The restartable init containers have exceeded its termination grace period seconds by prestop hook", func() {
			ginkgo.It("should mark pod as succeeded if any of the prestop hook in restartable init containers have exceeded its termination grace period seconds", func(ctx context.Context) {
				restartableInit1 := "restartable-init-1"
				restartableInit2 := "restartable-init-2"
				restartableInit3 := "restartable-init-3"
				regular1 := "regular-1"

				podTerminationGracePeriodSeconds := int64(5)

				makePrestop := func(containerName string, delay int) *v1.Lifecycle {
					return &v1.Lifecycle{
						PreStop: &v1.LifecycleHandler{
							Exec: &v1.ExecAction{
								Command: ExecCommand(containerName, execCommand{
									Delay:    delay,
									ExitCode: 0,
								}),
							},
						},
					}
				}

				pod := &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:       "restartable-init-prestop-exceeded-termination-grace-period",
						Finalizers: []string{testFinalizer},
					},
					Spec: v1.PodSpec{
						RestartPolicy:                 v1.RestartPolicyNever,
						TerminationGracePeriodSeconds: ptr.To(podTerminationGracePeriodSeconds),
						InitContainers: []v1.Container{
							{
								Name:          restartableInit1,
								Image:         busyboxImage,
								RestartPolicy: &containerRestartPolicyAlways,
								Command: ExecCommand(restartableInit1, execCommand{
									Delay:              600,
									TerminationSeconds: 1,
									ExitCode:           0,
								}),
								Lifecycle: makePrestop(restartableInit1, 1),
							},
							{
								Name:          restartableInit2,
								Image:         busyboxImage,
								RestartPolicy: &containerRestartPolicyAlways,
								Command: ExecCommand(restartableInit2, execCommand{
									Delay:              600,
									TerminationSeconds: 20,
									ExitCode:           0,
								}),
								Lifecycle: makePrestop(restartableInit1, 30),
							},
							{
								Name:          restartableInit3,
								Image:         busyboxImage,
								RestartPolicy: &containerRestartPolicyAlways,
								Command: ExecCommand(restartableInit3, execCommand{
									Delay:              600,
									TerminationSeconds: 1,
									ExitCode:           0,
								}),
								Lifecycle: makePrestop(restartableInit1, 1),
							},
						},
						Containers: []v1.Container{
							{
								Name:  regular1,
								Image: busyboxImage,
								Command: ExecCommand(regular1, execCommand{
									Delay:              600,
									TerminationSeconds: 1,
									ExitCode:           0,
								}),
							},
						},
					},
				}

				preparePod(pod)

				ginkgo.By("Creating the pod with finalizer")
				client := e2epod.NewPodClient(f)
				pod = client.Create(ctx, pod)
				defer client.RemoveFinalizer(ctx, pod.Name, testFinalizer)

				ginkgo.By("Waiting for the pod to be initialized and run")
				err := e2epod.WaitForPodRunningInNamespace(ctx, f.ClientSet, pod)
				framework.ExpectNoError(err)

				ginkgo.By("Deleting the pod")
				err = client.Delete(ctx, pod.Name, metav1.DeleteOptions{GracePeriodSeconds: &podTerminationGracePeriodSeconds})
				framework.ExpectNoError(err)

				ginkgo.By(fmt.Sprintf("Waiting for the pod (%s/%s) to be transitioned into the Succeeded phase", pod.Namespace, pod.Name))
				err = e2epod.WaitForPodSuccessInNamespace(ctx, f.ClientSet, pod.Name, f.Namespace.Name)
				framework.ExpectNoError(err, "Failed to await for the pod to be terminated: %q", pod.Name)

				ginkgo.By(fmt.Sprintf("Fetch the end state of the pod (%s/%s)", pod.Namespace, pod.Name))
				pod, err = client.Get(ctx, pod.Name, metav1.GetOptions{})
				framework.ExpectNoError(err, "Failed to fetch the end state of the pod: %q", pod.Name)

				// regular container is gracefully terminated
				expectPodTerminationContainerStatuses(pod.Status.ContainerStatuses, map[string]podTerminationContainerStatus{
					regular1: {exitCode: int32(0), reason: "Completed"},
				})

				// restartable-init-2 that exceed its termination grace period seconds by prestop hook is sigkilled
				// with exit code 137.
				// The other containers are gracefully terminated within their termination grace period seconds
				expectPodTerminationContainerStatuses(pod.Status.InitContainerStatuses, map[string]podTerminationContainerStatus{
					restartableInit1: {exitCode: int32(0), reason: "Completed"},
					restartableInit2: {exitCode: int32(137), reason: "Error"},
					restartableInit3: {exitCode: int32(0), reason: "Completed"},
				})
			})
		})
	})
})

var _ = SIGDescribe(feature.SidecarContainers, framework.WithSerial(), "Containers Lifecycle", func() {
	f := framework.NewDefaultFramework("containers-lifecycle-test-serial")
	addAfterEachForCleaningUpPods(f)
	f.NamespacePodSecurityLevel = admissionapi.LevelPrivileged

	ginkgo.When("A node running restartable init containers reboots", func() {
		ginkgo.It("should restart the containers in right order with the proper phase after the node reboot", func(ctx context.Context) {
			init1 := "init-1"
			restartableInit2 := "restartable-init-2"
			init3 := "init-3"
			regular1 := "regular-1"

			podLabels := map[string]string{
				"test":      "containers-lifecycle-test-serial",
				"namespace": f.Namespace.Name,
			}
			pod := &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "initialized-pod",
					Labels: podLabels,
				},
				Spec: v1.PodSpec{
					RestartPolicy: v1.RestartPolicyAlways,
					InitContainers: []v1.Container{
						{
							Name:  init1,
							Image: busyboxImage,
							Command: ExecCommand(init1, execCommand{
								Delay:    5,
								ExitCode: 0,
							}),
						},
						{
							Name:  restartableInit2,
							Image: busyboxImage,
							Command: ExecCommand(restartableInit2, execCommand{
								Delay:    300,
								ExitCode: 0,
							}),
							RestartPolicy: &containerRestartPolicyAlways,
						},
						{
							Name:  init3,
							Image: busyboxImage,
							Command: ExecCommand(init3, execCommand{
								Delay:    5,
								ExitCode: 0,
							}),
						},
					},
					Containers: []v1.Container{
						{
							Name:  regular1,
							Image: busyboxImage,
							Command: ExecCommand(regular1, execCommand{
								Delay:    300,
								ExitCode: 0,
							}),
						},
					},
				},
			}
			preparePod(pod)

			client := e2epod.NewPodClient(f)
			pod = client.Create(ctx, pod)
			ginkgo.By("Waiting for the pod to be initialized and run")
			err := e2epod.WaitForPodRunningInNamespace(ctx, f.ClientSet, pod)
			framework.ExpectNoError(err)

			ginkgo.By("Getting the current pod sandbox ID")
			rs, _, err := getCRIClient()
			framework.ExpectNoError(err)

			sandboxes, err := rs.ListPodSandbox(ctx, &runtimeapi.PodSandboxFilter{
				LabelSelector: podLabels,
			})
			framework.ExpectNoError(err)
			gomega.Expect(sandboxes).To(gomega.HaveLen(1))
			podSandboxID := sandboxes[0].Id

			ginkgo.By("Stopping the kubelet")
			restartKubelet := mustStopKubelet(ctx, f)

			ginkgo.By("Stopping the pod sandbox to simulate the node reboot")
			err = rs.StopPodSandbox(ctx, podSandboxID)
			framework.ExpectNoError(err)

			ginkgo.By("Restarting the kubelet")
			restartKubelet(ctx)
			gomega.Eventually(ctx, func() bool {
				return kubeletHealthCheck(kubeletHealthCheckURL)
			}, f.Timeouts.PodStart, f.Timeouts.Poll).Should(gomega.BeTrueBecause("kubelet was expected to be healthy"))

			ginkgo.By("Waiting for the pod to re-initialize")
			err = e2epod.WaitForPodCondition(ctx, f.ClientSet, pod.Namespace, pod.Name, "re-initialized", f.Timeouts.PodStart, func(pod *v1.Pod) (bool, error) {
				if pod.Status.InitContainerStatuses[0].RestartCount < 1 {
					return false, nil
				}
				if pod.Status.Phase != v1.PodPending {
					return false, fmt.Errorf("pod should remain in the pending phase until it is re-initialized, but it is %q", pod.Status.Phase)
				}
				return true, nil
			})
			framework.ExpectNoError(err)

			ginkgo.By("Waiting for the pod to run after re-initialization")
			err = e2epod.WaitForPodRunningInNamespace(ctx, f.ClientSet, pod)
			framework.ExpectNoError(err)

			ginkgo.By("Parsing results")
			pod, err = client.Get(ctx, pod.Name, metav1.GetOptions{})
			framework.ExpectNoError(err)
			results := parseOutput(ctx, f, pod)

			ginkgo.By("Analyzing results")
			init1Started, err := results.FindIndex(init1, "Started", 0)
			framework.ExpectNoError(err)
			restartableInit2Started, err := results.FindIndex(restartableInit2, "Started", 0)
			framework.ExpectNoError(err)
			init3Started, err := results.FindIndex(init3, "Started", 0)
			framework.ExpectNoError(err)
			regular1Started, err := results.FindIndex(regular1, "Started", 0)
			framework.ExpectNoError(err)

			init1Restarted, err := results.FindIndex(init1, "Started", init1Started+1)
			framework.ExpectNoError(err)
			restartableInit2Restarted, err := results.FindIndex(restartableInit2, "Started", restartableInit2Started+1)
			framework.ExpectNoError(err)
			init3Restarted, err := results.FindIndex(init3, "Started", init3Started+1)
			framework.ExpectNoError(err)
			regular1Restarted, err := results.FindIndex(regular1, "Started", regular1Started+1)
			framework.ExpectNoError(err)

			framework.ExpectNoError(init1Started.IsBefore(restartableInit2Started))
			framework.ExpectNoError(restartableInit2Started.IsBefore(init3Started))
			framework.ExpectNoError(init3Started.IsBefore(regular1Started))

			framework.ExpectNoError(init1Restarted.IsBefore(restartableInit2Restarted))
			framework.ExpectNoError(restartableInit2Restarted.IsBefore(init3Restarted))
			framework.ExpectNoError(init3Restarted.IsBefore(regular1Restarted))
		})

		ginkgo.When("A node is rebooting and receives an update request", func() {
			init1 := "init-1"
			restartableInit2 := "restartable-init-2"
			init3 := "init-3"
			regular1 := "regular-1"

			updatedImage := busyboxImage
			var podLabels map[string]string
			var originalPodSpec *v1.Pod

			ginkgo.BeforeEach(func(ctx context.Context) {
				podLabels = map[string]string{
					"test":      "containers-lifecycle-test-serial",
					"namespace": f.Namespace.Name,
				}

				originalPodSpec = &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:   "initialized-pod",
						Labels: podLabels,
					},
					Spec: v1.PodSpec{
						RestartPolicy: v1.RestartPolicyAlways,
						InitContainers: []v1.Container{
							{
								Name:  init1,
								Image: agnhostImage,
								Command: ExecCommand(init1, execCommand{
									Delay:    5,
									ExitCode: 0,
								}),
							},
							{
								Name:  restartableInit2,
								Image: agnhostImage,
								Command: ExecCommand(restartableInit2, execCommand{
									Delay:    300,
									ExitCode: 0,
								}),
								RestartPolicy: &containerRestartPolicyAlways,
							},
							{
								Name:  init3,
								Image: agnhostImage,
								Command: ExecCommand(init3, execCommand{
									Delay:    5,
									ExitCode: 0,
								}),
							},
						},
						Containers: []v1.Container{
							{
								Name:  regular1,
								Image: agnhostImage,
								Command: ExecCommand(regular1, execCommand{
									Delay:    300,
									ExitCode: 0,
								}),
							},
						},
					},
				}
			})

			testPodUpdateOnReboot := func(ctx context.Context, nodeReboot bool) {
				preparePod(originalPodSpec)

				client := e2epod.NewPodClient(f)
				pod := client.Create(ctx, originalPodSpec)
				ginkgo.By("Waiting for the pod to be initialized and run")
				err := e2epod.WaitForPodRunningInNamespace(ctx, f.ClientSet, pod)
				framework.ExpectNoError(err)

				ginkgo.By("Getting the current pod sandbox ID")
				rs, _, err := getCRIClient()
				framework.ExpectNoError(err)

				sandboxes, err := rs.ListPodSandbox(ctx, &runtimeapi.PodSandboxFilter{
					LabelSelector: podLabels,
				})
				framework.ExpectNoError(err)
				gomega.Expect(sandboxes).To(gomega.HaveLen(1))
				podSandboxID := sandboxes[0].Id

				ginkgo.By("Stopping the kubelet")
				restartKubelet := mustStopKubelet(ctx, f)

				if nodeReboot {
					ginkgo.By("Stopping the pod sandbox to simulate the node reboot")
					err = rs.StopPodSandbox(ctx, podSandboxID)
					framework.ExpectNoError(err)
				}

				ginkgo.By("Restarting the kubelet")
				restartKubelet(ctx)
				gomega.Eventually(ctx, func() bool {
					return kubeletHealthCheck(kubeletHealthCheckURL)
				}, f.Timeouts.PodStart, f.Timeouts.Poll).Should(gomega.BeTrueBecause("kubelet was expected to be healthy"))

				ginkgo.By("Sending an update")
				client.Update(ctx, pod.Name, func(pod *v1.Pod) {
					pod.Spec.InitContainers[1].Image = updatedImage
				})

				ginkgo.By("Waiting for the pod to be re-initialized and run")
				if nodeReboot {
					err = e2epod.WaitForPodCondition(ctx, f.ClientSet, pod.Namespace, pod.Name, "re-initialized", f.Timeouts.PodStart, func(pod *v1.Pod) (bool, error) {
						if pod.Status.ContainerStatuses[0].RestartCount < 1 {
							return false, nil
						}
						if pod.Status.Phase != v1.PodRunning {
							return false, nil
						}
						return true, nil
					})
				} else {
					err = e2epod.WaitForPodCondition(ctx, f.ClientSet, pod.Namespace, pod.Name, "re-initialized", f.Timeouts.PodStart, func(pod *v1.Pod) (bool, error) {
						if pod.Status.ContainerStatuses[0].RestartCount > 0 {
							return false, nil
						}
						if pod.Status.Phase != v1.PodRunning {
							return false, nil
						}
						return true, nil
					})
				}
				framework.ExpectNoError(err)

				ginkgo.By("Ensuring the image got updated")
				err = e2epod.WaitForPodCondition(ctx, f.ClientSet, pod.Namespace, pod.Name, "Image updated", f.Timeouts.PodStart+time.Duration(1)*time.Minute, func(pod *v1.Pod) (bool, error) {
					status := pod.Status.InitContainerStatuses[1]
					return status.RestartCount > 0 && status.Image == updatedImage, nil
				})
				framework.ExpectNoError(err)
			}

			ginkgo.It("should handle an update during the node reboot", func(ctx context.Context) {
				testPodUpdateOnReboot(ctx, true)
			})

			ginkgo.It("should handle an update during the kubelet restart", func(ctx context.Context) {
				testPodUpdateOnReboot(ctx, false)
			})
		})
	})
})
