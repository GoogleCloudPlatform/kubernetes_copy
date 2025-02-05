package lifecycle_hooks

import (
	"context"
	"fmt"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
    v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
	admissionapi "k8s.io/pod-security-admission/api"

	"k8s.io/kubernetes/test/e2e/framework"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	"k8s.io/kubernetes/test/e2e/nodefeature"
	imageutils "k8s.io/kubernetes/test/utils/image"
	"k8s.io/utils/ptr"
)

var _ = SIGDescribe(framework.WithNodeConformance(), "Containers Lifecycle", func() {
	f := framework.NewDefaultFramework("containers-lifecycle-test")
	f.NamespacePodSecurityLevel = admissionapi.LevelPrivileged

	
	// PRESTOP_BASIC TEST
	ginkgo.It("should respect termination grace period seconds", f.WithNodeConformance(), func() {
		client := e2epod.NewPodClient(f)
		gracePeriod := int64(30)
		bufferSeconds := int64(30) 
				
		// Define a pod with a PreStop hook
		ginkgo.By("creating a pod with a termination grace period seconds and PreStop hook")
		pod := &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: "pod-prestop-basic",
			},
			Spec: v1.PodSpec{
				Containers: []v1.Container{
					{
						Name:  "test-container",
						Image: imageutils.GetE2EImage(imageutils.BusyBox),
						Command: []string{"sleep", "10000"},
						Lifecycle: &v1.Lifecycle{
							PreStop: &v1.LifecycleHandler{
								Exec: &v1.ExecAction{
									Command: []string{"sleep", "5"}, 
								},
							},
						},
					},
				},
				TerminationGracePeriodSeconds: &gracePeriod,
			},
		}

		pod = client.Create(context.TODO(), pod)

		ginkgo.By("ensuring the pod is running")
		err := e2epod.WaitForPodRunningInNamespace(context.TODO(), f.ClientSet, pod)
		framework.ExpectNoError(err)

		ginkgo.By("deleting the pod gracefully")
		err = client.Delete(context.TODO(), pod.Name, metav1.DeleteOptions{})
		framework.ExpectNoError(err)

		// Ensure the pod is terminated within the grace period + buffer
		ginkgo.By("ensuring the pod is terminated within the grace period seconds + buffer seconds")
		err = e2epod.WaitForPodNotFoundInNamespace(context.TODO(), f.ClientSet, pod.Name, pod.Namespace, time.Duration(gracePeriod+bufferSeconds)*time.Second)
		framework.ExpectNoError(err)
	})


	// PRESTOP_BASIC_TERMINATING FAST TEST
	ginkgo.It("should call the container's preStop hook and terminate it if its startup probe fails", func() {
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
		podSpec = client.Create(context.TODO(), podSpec)

		ginkgo.By("Waiting for the pod to complete")
		err := e2epod.WaitForPodNoLongerRunningInNamespace(context.TODO(), f.ClientSet, podSpec.Name, podSpec.Namespace)
		framework.ExpectNoError(err)

		ginkgo.By("Parsing results")
		podSpec, err = client.Get(context.TODO(), podSpec.Name, metav1.GetOptions{})
		framework.ExpectNoError(err)
		results := parseOutput(context.TODO(), f, podSpec)

		ginkgo.By("Analyzing results")
		framework.ExpectNoError(results.RunTogether(regular1, prefixedName(PreStopPrefix, regular1)))
		framework.ExpectNoError(results.Starts(prefixedName(PreStopPrefix, regular1)))
		framework.ExpectNoError(results.Exits(regular1))
	})


	// PRESTOP_FAILING TEST
	ginkgo.It("should call the container's preStop hook and terminate it if its liveness probe fails", func() {
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
		podSpec = client.Create(context.TODO(), podSpec)

		ginkgo.By("Waiting for the pod to complete")
		err := e2epod.WaitForPodNoLongerRunningInNamespace(context.TODO(), f.ClientSet, podSpec.Name, podSpec.Namespace)
		framework.ExpectNoError(err)

		ginkgo.By("Parsing results")
		podSpec, err = client.Get(context.TODO(), podSpec.Name, metav1.GetOptions{})
		framework.ExpectNoError(err)
		results := parseOutput(context.TODO(), f, podSpec)

		ginkgo.By("Analyzing results")
		framework.ExpectNoError(results.RunTogether(regular1, prefixedName(PreStopPrefix, regular1)))
		framework.ExpectNoError(results.Starts(prefixedName(PreStopPrefix, regular1)))
		framework.ExpectNoError(results.Exits(regular1))
	})
	
})