/*
Copyright 2024 The Kubernetes Authors.

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

package lifecyclehooks

import (
    "context"
    "fmt"

    v1 "k8s.io/api/core/v1"
    metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
    "k8s.io/kubernetes/test/e2e/framework"
    "k8s.io/kubernetes/test/e2e/framework/ssh"
    e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
    "k8s.io/kubernetes/test/e2e_node/testdoc"
    admissionapi "k8s.io/pod-security-admission/api"
    imageutils "k8s.io/kubernetes/test/utils/image"
    "github.com/onsi/ginkgo/v2"
    "github.com/onsi/gomega"
    "k8s.io/utils/ptr"
)

var _ =  SIGDescribe(framework.WithNodeConformance(),"PreStop Hook Test Suite", func() {
    f := framework.NewDefaultFramework("prestop-hook-test-suite")
    f.NamespacePodSecurityLevel = admissionapi.LevelPrivileged

    ginkgo.It("should run prestop basic with the grace period", func(ctx context.Context) {
        testdoc.TestName("Hooks:prestop_basic_execution_test")

	    testdoc.TestStep("When you have a PreStop hook defined on your container, it will execute before the container is terminated.")
        const gracePeriod int64 = 30
        client := e2epod.NewPodClient(f)
        const volumeName = "host-data"
        const hostPath = "/tmp/prestop-hook-test.log"
        ginkgo.By("creating a pod with a termination grace period and a long-running PreStop hook")
        pod := &v1.Pod{
            ObjectMeta: metav1.ObjectMeta{
                Name: "pod-termination-grace-period",
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
                TerminationGracePeriodSeconds: ptr.To(gracePeriod),
            },
        }

        // Add the PreStop hook to write a message to the hostPath
        pod.Spec.Containers[0].Lifecycle = &v1.Lifecycle{
            PreStop: &v1.LifecycleHandler{
                Exec: &v1.ExecAction{
                    Command: []string{"sh", "-c", fmt.Sprintf("echo 'PreStop Hook Executed' > %s", hostPath)},
                },
            },
        }

        // Define the hostPath volume
        pod.Spec.Volumes = append(pod.Spec.Volumes, v1.Volume{
            Name: volumeName,
            VolumeSource: v1.VolumeSource{
                HostPath: &v1.HostPathVolumeSource{
                    Path: hostPath,
                    Type: ptr.To(v1.HostPathFileOrCreate),
                },
            },
        })

        // Mount the hostPath volume to the container
        pod.Spec.Containers[0].VolumeMounts = []v1.VolumeMount{
            {Name: volumeName, MountPath: hostPath},
        }
        testdoc.TestStep("Imagine You Have a Pod with the Following Specification:")
        testdoc.PodSpec(pod)
        testdoc.TestStep("This Pod should start successfully and run for some time")
        createdPod := client.CreateSync(ctx, pod)

        testdoc.TestStep("When the container is terminated, the PreStop hook will be triggered within the grace period")

        // Delete pod to trigger PreStop hook
		client.DeleteSync(ctx, createdPod.Name, metav1.DeleteOptions{}, f.Timeouts.PodDelete)

        // Verify PreStop hook output on the host via SSH
        node, err := f.ClientSet.CoreV1().Nodes().Get(ctx, createdPod.Spec.NodeName, metav1.GetOptions{})
        framework.ExpectNoError(err, "Failed to get node details")

        cmd := fmt.Sprintf("cat %s", hostPath)
        result, err := ssh.IssueSSHCommandWithResult(ctx, cmd, framework.TestContext.Provider, node)
        framework.ExpectNoError(err, "SSH command failed")
        gomega.Expect(result).ToNot(gomega.BeNil(), "SSH command returned nil result")
        gomega.Expect(result.Code).To(gomega.Equal(0), "SSH command failed with error code")

        testdoc.TestStep("The following log output confirms the successful execution of the PreStop hook:")
	    testdoc.TestLog(fmt.Sprintf("%s", result.Stdout))
		gomega.Expect(result.Stdout).To(gomega.Equal("PreStop Hook Executed\n"))

        testdoc.TestStep("Once the PreStop hook is successfully executed, the container terminates successfully.")
    })

    ginkgo.When("A regular container has a PreStop hook", func() {
        ginkgo.When("A regular container fails a startup probe", func() {
            ginkgo.It("should call the container's preStop hook and terminate it if its startup probe fails", func(ctx context.Context) {
                testdoc.TestName("Hooks:Startup_Probe_Failure")
                
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


                testdoc.TestStep("Create a pod with a startup probe that always fails and a PreStop hook for graceful termination.")
                testdoc.PodSpec(podSpec)
                preparePod(podSpec)

                client := e2epod.NewPodClient(f)
                podSpec = client.Create(ctx, podSpec)

                testdoc.TestStep("The pod should terminate, and its PreStop hook should execute before the container is stopped.")
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
                testdoc.TestLog("The PreStop hook was executed successfully after the startup probe failure.")
            })
        })

        ginkgo.When("A regular container fails a liveness probe", func() {
            ginkgo.It("should call the container's preStop hook and terminate it if its liveness probe fails", func(ctx context.Context) {
                
                testdoc.TestName("Hooks:Liveness_Probe_Failure")
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

                testdoc.TestStep("Create a pod with a liveness probe that always fails and a PreStop hook for graceful termination.")
                testdoc.PodSpec(podSpec)
                preparePod(podSpec)

                client := e2epod.NewPodClient(f)
                podSpec = client.Create(ctx, podSpec)

                testdoc.TestStep("The pod should terminate, and its PreStop hook should execute before the container is stopped.")
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
                testdoc.TestLog("The PreStop hook was executed successfully after the liveness probe failure.")
            })

        

    ginkgo.When("the restartable init containers have multiple PreStop hooks", func() {
        ginkgo.It("should call sidecar container PreStop hook simultaneously", func(ctx context.Context) {
            
            testdoc.TestName("Hooks:Multiple_Restartable_Init_sidecar")

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

            testdoc.TestStep("When creating a Pod with multiple restartable init containers and PreStop hooks, the hooks should execute simultaneously.")
            testdoc.PodSpec(pod)
            preparePod(pod)

            client := e2epod.NewPodClient(f)
            pod = client.Create(ctx, pod)

            testdoc.TestStep("Wait for the Pod to terminate gracefully and validate simultaneous execution of PreStop hooks.")
            err := e2epod.WaitTimeoutForPodNoLongerRunningInNamespace(ctx, f.ClientSet, pod.Name, pod.Namespace, 5*time.Minute)
            framework.ExpectNoError(err)

            pod, err = client.Get(ctx, pod.Name, metav1.GetOptions{})
            framework.ExpectNoError(err)

            testdoc.TestStep("Validate the termination of all restartable init containers and ensure PreStop hooks executed simultaneously.")

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

            testdoc.TestLog(fmt.Sprintf("PreStop hooks execution times: %s:%s: %s", ps1, ps2, ps3))
            gomega.Expect(ps1-ps2).To(gomega.BeNumerically("~", 0, toleration),
                fmt.Sprintf("expected PostStart 1 & PostStart 2 to start at the same time, got %s", results))
            gomega.Expect(ps1-ps3).To(gomega.BeNumerically("~", 0, toleration),
                fmt.Sprintf("expected PostStart 1 & PostStart 3 to start at the same time, got %s", results))
            gomega.Expect(ps2-ps3).To(gomega.BeNumerically("~", 0, toleration),
                fmt.Sprintf("expected PostStart 2 & PostStart 3 to start at the same time, got %s", results))
            
            testdoc.TestStep("The Pod should terminate successfully, with all PreStop hooks executed as expected.")
            
        })
    })

    ginkgo.When("Restartable init containers are terminated during initialization", func() {
        ginkgo.It("should not hang in termination if terminated during initialization", func(ctx context.Context) {
            
            testdoc.TestName("Hooks:Init_Container_Termination_Initialization")
            
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

            testdoc.TestStep("Define a Pod with a starting init container and multiple restartable init containers. Each restartable init container has a PreStop hook.")
            testdoc.PodSpec(pod)
            testdoc.TestStep("Create the Pod and wait until the starting init container begins execution.")
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

            testdoc.TestStep("Terminate the Pod during initialization to ensure it doesn't hang during termination.")
            // the init container is running, so we stop the pod before the sidecars even start
            start := time.Now()
            grace := int64(3)
            ginkgo.By("deleting the pod")
            err = client.Delete(ctx, pod.Name, metav1.DeleteOptions{GracePeriodSeconds: &grace})
            framework.ExpectNoError(err)
            ginkgo.By("waiting for the pod to disappear")
            err = e2epod.WaitForPodNotFoundInNamespace(ctx, f.ClientSet, pod.Name, pod.Namespace, 120*time.Second)
            framework.ExpectNoError(err)

            testdoc.TestStep("Verify the Pod terminates within the expected grace period and does not hang.")
            buffer := int64(2)
            deleteTime := time.Since(start).Seconds()
            testdoc.TestLog(fmt.Sprintf("Pod deletion completed in %f seconds.", deleteTime))
            // should delete quickly and not try to start/wait on any sidecars since they never started
            gomega.Expect(deleteTime).To(gomega.BeNumerically("<", grace+buffer), fmt.Sprintf("should delete in < %d seconds, took %f", grace+buffer, deleteTime))
        })
    })

    
    ginkgo.When("The restartable init containers exit with non-zero exit code", func() {
        ginkgo.It("should mark pod as succeeded if any of the restartable init containers have terminated with non-zero exit code", func(ctx context.Context) {
            testdoc.TestName("Hooks:Restartable_Init_Container_NonZeroExitCode")
            
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

            testdoc.TestStep("Lets Define a Pod with multiple restartable init containers and a regular container. One init container is configured to exit with a non-zero code.")
            testdoc.PodSpec(pod)

   
               testdoc.TestStep("when  the Pod created and it is ensured all containers, including the restartable init containers, are initialized and running.")
            preparePod(pod)

            ginkgo.By("Creating the pod with finalizer")
            client := e2epod.NewPodClient(f)
            pod = client.Create(ctx, pod)
            defer client.RemoveFinalizer(ctx, pod.Name, testFinalizer)

            ginkgo.By("Waiting for the pod to be initialized and run")
            err := e2epod.WaitForPodRunningInNamespace(ctx, f.ClientSet, pod)
            framework.ExpectNoError(err)


            testdoc.TestStep("Delete the Pod to trigger termination. During termination, the restartable init container with a non-zero exit code is expected to complete.")
            ginkgo.By("Deleting the pod")
            err = client.Delete(ctx, pod.Name, metav1.DeleteOptions{GracePeriodSeconds: &podTerminationGracePeriodSeconds})
            framework.ExpectNoError(err)

            ginkgo.By(fmt.Sprintf("Waiting for the pod (%s/%s) to be transitioned into the Succeeded phase", pod.Namespace, pod.Name))
            err = e2epod.WaitForPodSuccessInNamespace(ctx, f.ClientSet, pod.Name, f.Namespace.Name)
            framework.ExpectNoError(err, "Failed to await for the pod to be terminated: %q", pod.Name)

            ginkgo.By(fmt.Sprintf("Fetch the end state of the pod (%s/%s)", pod.Namespace, pod.Name))
            pod, err = client.Get(ctx, pod.Name, metav1.GetOptions{})
            framework.ExpectNoError(err, "Failed to fetch the end state of the pod: %q", pod.Name)

            testdoc.TestLog(fmt.Sprintf("Final Phase of Pod: %s", pod.Status.Phase))

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


    

    ginkgo.When("The regular containers have exceeded its termination grace period seconds by prestop hook", func() {
        ginkgo.It("should mark pod as failed if any of the prestop hook in regular container has exceeded its termination grace period seconds", func(ctx context.Context) {
            testdoc.TestName("Hooks:Regular_Container_ExceedingGracePeriod")
            
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

            testdoc.TestStep("Lets define a Pod with multiple init containers and a regular container. The regular container has a PreStop hook configured to execute before termination.")
            testdoc.PodSpec(pod)

            preparePod(pod)

            testdoc.TestStep("When the Pod is created and it is ensured all containers, including the init containers and the regular container, are initialized and running.")
            ginkgo.By("Creating the pod with finalizer")
            client := e2epod.NewPodClient(f)
            pod = client.Create(ctx, pod)
            defer client.RemoveFinalizer(ctx, pod.Name, testFinalizer)

            ginkgo.By("Waiting for the pod to be initialized and run")
            err := e2epod.WaitForPodRunningInNamespace(ctx, f.ClientSet, pod)
            framework.ExpectNoError(err)

            testdoc.TestStep("when the Pod deleted  it triggers the termination process. The PreStop hook in the regular container will execute during this phase.")
            ginkgo.By("Deleting the pod")
            err = client.Delete(ctx, pod.Name, metav1.DeleteOptions{GracePeriodSeconds: &podTerminationGracePeriodSeconds})
            framework.ExpectNoError(err)

            testdoc.TestStep("If the PreStop hook execution exceeds the grace period, the Pod transitions to the 'Failed' phase.")
            ginkgo.By(fmt.Sprintf("Waiting for the pod (%s/%s) to be transitioned into the Failed phase", pod.Namespace, pod.Name))
            err = e2epod.WaitForPodTerminatedInNamespace(ctx, f.ClientSet, pod.Name, "", f.Namespace.Name)
            framework.ExpectNoError(err, "Failed to await for the pod to be terminated: %q", pod.Name)


            testdoc.TestStep("Lets Fetch the final state of the Pod to validate the termination behavior for all containers.")
            ginkgo.By(fmt.Sprintf("Fetch the end state of the pod (%s/%s)", pod.Namespace, pod.Name))
            pod, err = client.Get(ctx, pod.Name, metav1.GetOptions{})
            framework.ExpectNoError(err, "Failed to fetch the end state of the pod: %q", pod.Name)

            testdoc.TestLog(fmt.Sprintf("Final Phase of Pod: %s", pod.Status.Phase))
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
             
            testdoc.TestName("Hooks:Restartable_Init_Container_ExceedingGracePeriod")

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
            
            testdoc.TestStep("When creating a Pod with multiple restartable init containers, the PreStop hooks execute upon termination.")

            testdoc.TestStep("Imagine You Have a Pod with the Following Specification:")
            testdoc.PodSpec(pod)
            preparePod(pod)

            ginkgo.By("Creating the pod with finalizer")
            client := e2epod.NewPodClient(f)
            pod = client.Create(ctx, pod)
            defer client.RemoveFinalizer(ctx, pod.Name, testFinalizer)

            testdoc.TestStep("When the Pod runs, all containers complete initialization, and termination triggers PreStop hooks.")
            ginkgo.By("Waiting for the pod to be initialized and run")
            err := e2epod.WaitForPodRunningInNamespace(ctx, f.ClientSet, pod)
            framework.ExpectNoError(err)


            testdoc.TestStep("If PreStop hook execution exceeds grace period, the Pod transitions to the 'Succeeded' phase.")
            ginkgo.By("Deleting the pod")
            err = client.Delete(ctx, pod.Name, metav1.DeleteOptions{GracePeriodSeconds: &podTerminationGracePeriodSeconds})
            framework.ExpectNoError(err)

            ginkgo.By(fmt.Sprintf("Waiting for the pod (%s/%s) to be transitioned into the Succeeded phase", pod.Namespace, pod.Name))
            err = e2epod.WaitForPodSuccessInNamespace(ctx, f.ClientSet, pod.Name, f.Namespace.Name)
            framework.ExpectNoError(err, "Failed to await for the pod to be terminated: %q", pod.Name)

            testdoc.TestStep("The Pod final state reflects which containers completed gracefully or were forcibly terminated.")
            ginkgo.By(fmt.Sprintf("Fetch the end state of the pod (%s/%s)", pod.Namespace, pod.Name))
            pod, err = client.Get(ctx, pod.Name, metav1.GetOptions{})
            framework.ExpectNoError(err, "Failed to fetch the end state of the pod: %q", pod.Name)

            testdoc.TestLog(fmt.Sprintf("Final Phase of Pod: %s", pod.Status.Phase))

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
