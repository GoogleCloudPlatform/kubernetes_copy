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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
	v1core "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/util/retry"
	"k8s.io/kubernetes/pkg/kubelet/events"
	"k8s.io/kubernetes/test/e2e/framework"
	e2ekubelet "k8s.io/kubernetes/test/e2e/framework/kubelet"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	imageutils "k8s.io/kubernetes/test/utils/image"
	admissionapi "k8s.io/pod-security-admission/api"
	utilpointer "k8s.io/utils/pointer"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/expfmt"
)

var _ = SIGDescribe("Pods Extended", func() {
	f := framework.NewDefaultFramework("pods")
	f.NamespacePodSecurityLevel = admissionapi.LevelBaseline

	ginkgo.Describe("Delete Grace Period", func() {
		var podClient *e2epod.PodClient
		ginkgo.BeforeEach(func() {
			podClient = e2epod.NewPodClient(f)
		})

		/*
			Release: v1.15
			Testname: Pods, delete grace period
			Description: Create a pod, make sure it is running. Using the http client send a 'delete' with gracePeriodSeconds=30. Pod SHOULD get terminated within gracePeriodSeconds and removed from API server within a window.
		*/
		ginkgo.It("should be submitted and removed", func(ctx context.Context) {
			ginkgo.By("creating the pod")
			name := "pod-submit-remove-" + string(uuid.NewUUID())
			value := strconv.Itoa(time.Now().Nanosecond())
			pod := e2epod.NewAgnhostPod(f.Namespace.Name, name, nil, nil, nil)
			pod.ObjectMeta.Labels = map[string]string{
				"name": "foo",
				"time": value,
			}

			ginkgo.By("setting up selector")
			selector := labels.SelectorFromSet(labels.Set(map[string]string{"time": value}))
			options := metav1.ListOptions{LabelSelector: selector.String()}
			pods, err := podClient.List(ctx, options)
			framework.ExpectNoError(err, "failed to query for pod")
			gomega.Expect(pods.Items).To(gomega.BeEmpty())

			ginkgo.By("submitting the pod to kubernetes")
			podClient.Create(ctx, pod)

			ginkgo.By("verifying the pod is in kubernetes")
			selector = labels.SelectorFromSet(labels.Set(map[string]string{"time": value}))
			options = metav1.ListOptions{LabelSelector: selector.String()}
			pods, err = podClient.List(ctx, options)
			framework.ExpectNoError(err, "failed to query for pod")
			gomega.Expect(pods.Items).To(gomega.HaveLen(1))

			// We need to wait for the pod to be running, otherwise the deletion
			// may be carried out immediately rather than gracefully.
			framework.ExpectNoError(e2epod.WaitForPodNameRunningInNamespace(ctx, f.ClientSet, pod.Name, f.Namespace.Name))
			// save the running pod
			pod, err = podClient.Get(ctx, pod.Name, metav1.GetOptions{})
			framework.ExpectNoError(err, "failed to GET scheduled pod")

			ginkgo.By("deleting the pod gracefully")
			var lastPod v1.Pod
			var statusCode int
			err = f.ClientSet.CoreV1().RESTClient().Delete().AbsPath("/api/v1/namespaces", pod.Namespace, "pods", pod.Name).Param("gracePeriodSeconds", "30").Do(ctx).StatusCode(&statusCode).Into(&lastPod)
			framework.ExpectNoError(err, "failed to use http client to send delete")
			gomega.Expect(statusCode).To(gomega.Equal(http.StatusOK), "failed to delete gracefully by client request")

			ginkgo.By("verifying the kubelet observed the termination notice")

			// allow up to 3x grace period (which allows process termination)
			// for the kubelet to remove from api.  need to follow-up on if this
			// latency between termination and reportal can be isolated further.
			start := time.Now()
			err = wait.Poll(time.Second*5, time.Second*30*3, func() (bool, error) {
				podList, err := e2ekubelet.GetKubeletPods(ctx, f.ClientSet, pod.Spec.NodeName)
				if err != nil {
					framework.Logf("Unable to retrieve kubelet pods for node %v: %v", pod.Spec.NodeName, err)
					return false, nil
				}
				for _, kubeletPod := range podList.Items {
					if pod.Name != kubeletPod.Name || pod.Namespace != kubeletPod.Namespace {
						continue
					}
					if kubeletPod.ObjectMeta.DeletionTimestamp == nil {
						framework.Logf("deletion has not yet been observed")
						return false, nil
					}
					data, _ := json.Marshal(kubeletPod)
					framework.Logf("start=%s, now=%s, kubelet pod: %s", start, time.Now(), string(data))
					return false, nil
				}
				framework.Logf("no pod exists with the name we were looking for, assuming the termination request was observed and completed")
				return true, nil
			})
			framework.ExpectNoError(err, "kubelet never observed the termination notice")

			gomega.Expect(lastPod.DeletionTimestamp).ToNot(gomega.BeNil())
			gomega.Expect(lastPod.Spec.TerminationGracePeriodSeconds).ToNot(gomega.BeZero())

			selector = labels.SelectorFromSet(labels.Set(map[string]string{"time": value}))
			options = metav1.ListOptions{LabelSelector: selector.String()}
			pods, err = podClient.List(ctx, options)
			framework.ExpectNoError(err, "failed to query for pods")
			gomega.Expect(pods.Items).To(gomega.BeEmpty())
		})
	})

	ginkgo.Describe("Pods Set QOS Class", func() {
		var podClient *e2epod.PodClient
		ginkgo.BeforeEach(func() {
			podClient = e2epod.NewPodClient(f)
		})

		/*
			Release: v1.9
			Testname: Pods, QOS
			Description:  Create a Pod with CPU and Memory request and limits. Pod status MUST have QOSClass set to PodQOSGuaranteed.
		*/
		framework.ConformanceIt("should be set on Pods with matching resource requests and limits for memory and cpu", func(ctx context.Context) {
			ginkgo.By("creating the pod")
			name := "pod-qos-class-" + string(uuid.NewUUID())
			pod := &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: name,
					Labels: map[string]string{
						"name": name,
					},
				},
				Spec: v1.PodSpec{
					Containers: []v1.Container{
						{
							Name:  "agnhost",
							Image: imageutils.GetE2EImage(imageutils.Agnhost),
							Args:  []string{"pause"},
							Resources: v1.ResourceRequirements{
								Limits: v1.ResourceList{
									v1.ResourceCPU:    resource.MustParse("100m"),
									v1.ResourceMemory: resource.MustParse("100Mi"),
								},
								Requests: v1.ResourceList{
									v1.ResourceCPU:    resource.MustParse("100m"),
									v1.ResourceMemory: resource.MustParse("100Mi"),
								},
							},
						},
					},
				},
			}

			ginkgo.By("submitting the pod to kubernetes")
			podClient.Create(ctx, pod)

			ginkgo.By("verifying QOS class is set on the pod")
			pod, err := podClient.Get(ctx, name, metav1.GetOptions{})
			framework.ExpectNoError(err, "failed to query for pod")
			gomega.Expect(pod.Status.QOSClass).To(gomega.Equal(v1.PodQOSGuaranteed))
		})
	})

	ginkgo.Describe("Pod Container Status", func() {
		var podClient *e2epod.PodClient
		ginkgo.BeforeEach(func() {
			podClient = e2epod.NewPodClient(f)
		})

		ginkgo.It("should never report success for a pending container", func(ctx context.Context) {
			ginkgo.By("creating pods that should always exit 1 and terminating the pod after a random delay")
			createAndTestPodRepeatedly(ctx,
				3, 15,
				podFastDeleteScenario{client: podClient.PodInterface, delayMs: 2000},
				podClient.PodInterface,
			)
		})
		ginkgo.It("should never report container start when an init container fails", func(ctx context.Context) {
			ginkgo.By("creating pods with an init container that always exit 1 and terminating the pod after a random delay")
			createAndTestPodRepeatedly(ctx,
				3, 15,
				podFastDeleteScenario{client: podClient.PodInterface, delayMs: 2000, initContainer: true},
				podClient.PodInterface,
			)
		})
	})

	ginkgo.Describe("Pod Container lifecycle", func() {
		var podClient *e2epod.PodClient
		ginkgo.BeforeEach(func() {
			podClient = e2epod.NewPodClient(f)
		})

		ginkgo.It("should not create extra sandbox if all containers are done", func(ctx context.Context) {
			ginkgo.By("creating the pod that should always exit 0")

			name := "pod-always-succeed" + string(uuid.NewUUID())
			image := imageutils.GetE2EImage(imageutils.BusyBox)
			pod := &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: name,
				},
				Spec: v1.PodSpec{
					RestartPolicy: v1.RestartPolicyOnFailure,
					InitContainers: []v1.Container{
						{
							Name:  "foo",
							Image: image,
							Command: []string{
								"/bin/true",
							},
						},
					},
					Containers: []v1.Container{
						{
							Name:  "bar",
							Image: image,
							Command: []string{
								"/bin/true",
							},
						},
					},
				},
			}

			ginkgo.By("submitting the pod to kubernetes")
			createdPod := podClient.Create(ctx, pod)
			ginkgo.DeferCleanup(func(ctx context.Context) error {
				ginkgo.By("deleting the pod")
				return podClient.Delete(ctx, pod.Name, metav1.DeleteOptions{})
			})

			framework.ExpectNoError(e2epod.WaitForPodSuccessInNamespace(ctx, f.ClientSet, pod.Name, f.Namespace.Name))

			var eventList *v1.EventList
			var err error
			ginkgo.By("Getting events about the pod")
			framework.ExpectNoError(wait.Poll(time.Second*2, time.Second*60, func() (bool, error) {
				selector := fields.Set{
					"involvedObject.kind":      "Pod",
					"involvedObject.uid":       string(createdPod.UID),
					"involvedObject.namespace": f.Namespace.Name,
					"source":                   "kubelet",
				}.AsSelector().String()
				options := metav1.ListOptions{FieldSelector: selector}
				eventList, err = f.ClientSet.CoreV1().Events(f.Namespace.Name).List(ctx, options)
				if err != nil {
					return false, err
				}
				if len(eventList.Items) > 0 {
					return true, nil
				}
				return false, nil
			}))

			ginkgo.By("Checking events about the pod")
			for _, event := range eventList.Items {
				if event.Reason == events.SandboxChanged {
					framework.Fail("Unexpected SandboxChanged event")
				}
			}
		})

		ginkgo.It("evicted pods should be terminal", func(ctx context.Context) {
			ginkgo.By("creating the pod that should be evicted")

			name := "pod-should-be-evicted" + string(uuid.NewUUID())
			image := imageutils.GetE2EImage(imageutils.BusyBox)
			pod := &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: name,
				},
				Spec: v1.PodSpec{
					RestartPolicy: v1.RestartPolicyOnFailure,
					Containers: []v1.Container{
						{
							Name:  "bar",
							Image: image,
							Command: []string{
								"/bin/sh", "-c", "sleep 10; dd if=/dev/zero of=file bs=1M count=10; sleep 10000",
							},
							Resources: v1.ResourceRequirements{
								Limits: v1.ResourceList{
									"ephemeral-storage": resource.MustParse("5Mi"),
								},
							}},
					},
				},
			}

			ginkgo.By("submitting the pod to kubernetes")
			podClient.Create(ctx, pod)
			ginkgo.DeferCleanup(func(ctx context.Context) error {
				ginkgo.By("deleting the pod")
				return podClient.Delete(ctx, pod.Name, metav1.DeleteOptions{})
			})

			err := e2epod.WaitForPodTerminatedInNamespace(ctx, f.ClientSet, pod.Name, "Evicted", f.Namespace.Name)
			if err != nil {
				framework.Failf("error waiting for pod to be evicted: %v", err)
			}
			err = e2epod.EnsureValidContainerTerminatedReason(ctx, f.ClientSet, f.Namespace.Name, pod.Name, "bar", 2*time.Minute)
			if err != nil {
				framework.Failf("error waiting for a valid terminated reason: %v", err)
			}
		})
	})

	ginkgo.Describe("Pod TerminationGracePeriodSeconds is negative", func() {
		var podClient *e2epod.PodClient
		ginkgo.BeforeEach(func() {
			podClient = e2epod.NewPodClient(f)
		})

		ginkgo.It("pod with negative grace period", func(ctx context.Context) {
			name := "pod-negative-grace-period" + string(uuid.NewUUID())
			image := imageutils.GetE2EImage(imageutils.BusyBox)
			pod := &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: name,
				},
				Spec: v1.PodSpec{
					RestartPolicy: v1.RestartPolicyOnFailure,
					Containers: []v1.Container{
						{
							Name:  "foo",
							Image: image,
							Command: []string{
								"/bin/sh", "-c", "sleep 10000",
							},
						},
					},
					TerminationGracePeriodSeconds: utilpointer.Int64(-1),
				},
			}

			ginkgo.By("submitting the pod to kubernetes")
			podClient.Create(ctx, pod)

			pod, err := podClient.Get(ctx, pod.Name, metav1.GetOptions{})
			framework.ExpectNoError(err, "failed to query for pod")

			if pod.Spec.TerminationGracePeriodSeconds == nil {
				framework.Failf("pod spec TerminationGracePeriodSeconds is nil")
			}

			if *pod.Spec.TerminationGracePeriodSeconds != 1 {
				framework.Failf("pod spec TerminationGracePeriodSeconds is not 1: %d", *pod.Spec.TerminationGracePeriodSeconds)
			}

			// retry if the TerminationGracePeriodSeconds is overrided
			// see more in https://github.com/kubernetes/kubernetes/pull/115606
			err = retry.RetryOnConflict(retry.DefaultBackoff, func() error {
				pod, err := podClient.Get(ctx, pod.Name, metav1.GetOptions{})
				framework.ExpectNoError(err, "failed to query for pod")
				ginkgo.By("updating the pod to have a negative TerminationGracePeriodSeconds")
				pod.Spec.TerminationGracePeriodSeconds = utilpointer.Int64(-1)
				_, err = podClient.PodInterface.Update(ctx, pod, metav1.UpdateOptions{})
				return err
			})
			framework.ExpectNoError(err, "failed to update pod")

			pod, err = podClient.Get(ctx, pod.Name, metav1.GetOptions{})
			framework.ExpectNoError(err, "failed to query for pod")

			if pod.Spec.TerminationGracePeriodSeconds == nil {
				framework.Failf("pod spec TerminationGracePeriodSeconds is nil")
			}

			if *pod.Spec.TerminationGracePeriodSeconds != 1 {
				framework.Failf("pod spec TerminationGracePeriodSeconds is not 1: %d", *pod.Spec.TerminationGracePeriodSeconds)
			}

			ginkgo.DeferCleanup(func(ctx context.Context) error {
				ginkgo.By("deleting the pod")
				return podClient.Delete(ctx, pod.Name, metav1.DeleteOptions{})
			})
		})
	})

})

func createAndTestPodRepeatedly(ctx context.Context, workers, iterations int, scenario podScenario, podClient v1core.PodInterface) {
	var (
		lock sync.Mutex
		errs []error

		wg sync.WaitGroup
	)

	r := prometheus.NewRegistry()
	h := prometheus.NewSummaryVec(prometheus.SummaryOpts{
		Name: "latency",
		Objectives: map[float64]float64{
			0.5:  0.05,
			0.75: 0.025,
			0.9:  0.01,
			0.99: 0.001,
		},
	}, []string{"node"})
	r.MustRegister(h)

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer ginkgo.GinkgoRecover()
			defer wg.Done()
			for retries := 0; retries < iterations; retries++ {
				pod := scenario.Pod(i, retries)

				// create the pod, capture the change events, then delete the pod
				start := time.Now()
				created, err := podClient.Create(ctx, pod, metav1.CreateOptions{})
				framework.ExpectNoError(err, "failed to create pod")

				ch := make(chan []watch.Event)
				waitForWatch := make(chan struct{})
				go func() {
					defer ginkgo.GinkgoRecover()
					defer close(ch)
					w, err := podClient.Watch(ctx, metav1.ListOptions{
						ResourceVersion: created.ResourceVersion,
						FieldSelector:   fmt.Sprintf("metadata.name=%s", pod.Name),
					})
					if err != nil {
						framework.Logf("Unable to watch pod %s: %v", pod.Name, err)
						return
					}
					defer w.Stop()
					close(waitForWatch)
					events := []watch.Event{
						{Type: watch.Added, Object: created},
					}
					for event := range w.ResultChan() {
						events = append(events, event)
						if event.Type == watch.Error {
							framework.Logf("watch error seen for %s: %#v", pod.Name, event.Object)
						}
						if scenario.IsLastEvent(event) {
							framework.Logf("watch last event seen for %s", pod.Name)
							break
						}
					}
					ch <- events
				}()

				select {
				case <-ch: // in case the goroutine above exits before establishing the watch
				case <-waitForWatch: // when the watch is established
				}

				verifier, scenario, err := scenario.Action(ctx, pod)
				framework.ExpectNoError(err, "failed to take action")

				var (
					events []watch.Event
					ok     bool
				)
				select {
				case events, ok = <-ch:
					if !ok {
						continue
					}
					if len(events) < 2 {
						framework.Fail("only got a single event")
					}
				case <-time.After(5 * time.Minute):
					framework.Failf("timed out waiting for watch events for %s", pod.Name)
				}

				end := time.Now()

				var eventErr error
				for _, event := range events[1:] {
					if err := verifier.Verify(event); err != nil {
						eventErr = err
						break
					}
				}

				total := end.Sub(start)

				var lastPod *v1.Pod = pod
				func() {
					lock.Lock()
					defer lock.Unlock()

					if eventErr != nil {
						errs = append(errs, eventErr)
						return
					}
					pod, verifyErrs := verifier.VerifyFinal(scenario, total)
					if pod != nil {
						lastPod = pod
					}
					errs = append(errs, verifyErrs...)
				}()

				h.WithLabelValues(lastPod.Spec.NodeName).Observe(total.Seconds())
			}
		}(i)
	}

	wg.Wait()

	if len(errs) > 0 {
		var messages []string
		for _, err := range errs {
			messages = append(messages, err.Error())
		}
		framework.Failf("%d errors:\n%v", len(errs), strings.Join(messages, "\n"))
	}
	values, _ := r.Gather()
	var buf bytes.Buffer
	for _, m := range values {
		expfmt.MetricFamilyToText(&buf, m)
	}
	framework.Logf("Summary of latencies:\n%s", buf.String())
}

type podScenario interface {
	Pod(worker, attempt int) *v1.Pod
	Action(context.Context, *v1.Pod) (podScenarioVerifier, string, error)
	IsLastEvent(event watch.Event) bool
}

type podScenarioVerifier interface {
	Verify(event watch.Event) error
	VerifyFinal(scenario string, duration time.Duration) (*v1.Pod, []error)
}

type podFastDeleteScenario struct {
	client  v1core.PodInterface
	delayMs int

	initContainer bool
}

func (s podFastDeleteScenario) Verifier(pod *v1.Pod) podScenarioVerifier {
	return &podStartVerifier{}
}

func (s podFastDeleteScenario) IsLastEvent(event watch.Event) bool {
	if event.Type == watch.Deleted {
		return true
	}
	return false
}

func (s podFastDeleteScenario) Action(ctx context.Context, pod *v1.Pod) (podScenarioVerifier, string, error) {
	t := time.Duration(rand.Intn(s.delayMs)) * time.Millisecond
	scenario := fmt.Sprintf("t=%s", t)
	time.Sleep(t)
	return &podStartVerifier{pod: pod}, scenario, s.client.Delete(ctx, pod.Name, metav1.DeleteOptions{})
}

func (s podFastDeleteScenario) Pod(worker, attempt int) *v1.Pod {
	name := fmt.Sprintf("pod-terminate-status-%d-%d", worker, attempt)
	value := strconv.Itoa(time.Now().Nanosecond())
	one := int64(1)
	if s.initContainer {
		return &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: name,
				Labels: map[string]string{
					"name": "foo",
					"time": value,
				},
			},
			Spec: v1.PodSpec{
				RestartPolicy:                 v1.RestartPolicyNever,
				TerminationGracePeriodSeconds: &one,
				InitContainers: []v1.Container{
					{
						Name:  "fail",
						Image: imageutils.GetE2EImage(imageutils.BusyBox),
						Command: []string{
							"/bin/false",
						},
						Resources: v1.ResourceRequirements{
							Requests: v1.ResourceList{
								v1.ResourceCPU:    resource.MustParse("5m"),
								v1.ResourceMemory: resource.MustParse("10Mi"),
							},
						},
					},
				},
				Containers: []v1.Container{
					{
						Name:  "blocked",
						Image: imageutils.GetE2EImage(imageutils.BusyBox),
						Command: []string{
							"/bin/true",
						},
						Resources: v1.ResourceRequirements{
							Requests: v1.ResourceList{
								v1.ResourceCPU:    resource.MustParse("5m"),
								v1.ResourceMemory: resource.MustParse("10Mi"),
							},
						},
					},
				},
			},
		}
	}
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				"name": "foo",
				"time": value,
			},
		},
		Spec: v1.PodSpec{
			RestartPolicy:                 v1.RestartPolicyNever,
			TerminationGracePeriodSeconds: &one,
			Containers: []v1.Container{
				{
					Name:  "fail",
					Image: imageutils.GetE2EImage(imageutils.BusyBox),
					Command: []string{
						"/bin/false",
					},
					Resources: v1.ResourceRequirements{
						Requests: v1.ResourceList{
							v1.ResourceCPU:    resource.MustParse("5m"),
							v1.ResourceMemory: resource.MustParse("10Mi"),
						},
					},
				},
			},
		},
	}
}

// podStartVerifier checks events for a given pod and looks for unexpected
// transitions. It assumes one container running to completion.
type podStartVerifier struct {
	pod                  *v1.Pod
	hasInitContainers    bool
	hasContainers        bool
	hasTerminated        bool
	hasRunningContainers bool
	hasTerminalPhase     bool
	duration             time.Duration
	completeDuration     time.Duration
}

var reBug88766 = regexp.MustCompile(`rootfs_linux.*kubernetes\.io~(secret|projected).*no such file or directory`)

// Verify takes successive watch events for a given pod and returns an error if the status is unexpected.
// This verifier works for any pod which has 0 init containers and 1 regular container.
func (v *podStartVerifier) Verify(event watch.Event) error {
	var ok bool
	pod, ok := event.Object.(*v1.Pod)
	if !ok {
		framework.Logf("Unexpected event object: %s %#v", event.Type, event.Object)
		return nil
	}
	v.pod = pod

	if len(pod.Spec.InitContainers) > 0 {
		if len(pod.Status.InitContainerStatuses) == 0 {
			if v.hasInitContainers {
				return fmt.Errorf("pod %s on node %s had incorrect init containers: %#v", pod.Name, pod.Spec.NodeName, pod.Status.InitContainerStatuses)
			}
			return nil
		}
		v.hasInitContainers = true
		if len(pod.Status.InitContainerStatuses) != 1 {
			return fmt.Errorf("pod %s on node %s had incorrect init containers: %#v", pod.Name, pod.Spec.NodeName, pod.Status.InitContainerStatuses)
		}

	} else {
		if len(pod.Status.InitContainerStatuses) != 0 {
			return fmt.Errorf("pod %s on node %s had incorrect init containers: %#v", pod.Name, pod.Spec.NodeName, pod.Status.InitContainerStatuses)
		}
	}

	if len(pod.Status.ContainerStatuses) == 0 {
		if v.hasContainers {
			return fmt.Errorf("pod %s on node %s had incorrect containers: %#v", pod.Name, pod.Spec.NodeName, pod.Status.ContainerStatuses)
		}
		return nil
	}
	v.hasContainers = true
	if len(pod.Status.ContainerStatuses) != 1 {
		return fmt.Errorf("pod %s on node %s had incorrect containers: %#v", pod.Name, pod.Spec.NodeName, pod.Status.ContainerStatuses)
	}

	if status := e2epod.FindContainerStatusInPod(pod, "blocked"); status != nil {
		if (status.Started != nil && *status.Started) || status.LastTerminationState.Terminated != nil || status.State.Waiting == nil {
			return fmt.Errorf("pod %s on node %s should not have started the blocked container: %#v", pod.Name, pod.Spec.NodeName, status)
		}
	}

	status := e2epod.FindContainerStatusInPod(pod, "fail")
	if status == nil {
		return fmt.Errorf("pod %s on node %s had incorrect containers: %#v", pod.Name, pod.Spec.NodeName, pod.Status)
	}

	t := status.State.Terminated
	if v.hasTerminated {
		if status.State.Waiting != nil || status.State.Running != nil {
			return fmt.Errorf("pod %s on node %s was terminated and then changed state: %#v", pod.Name, pod.Spec.NodeName, status)
		}
		if t == nil {
			return fmt.Errorf("pod %s on node %s was terminated and then had termination cleared: %#v", pod.Name, pod.Spec.NodeName, status)
		}
	}
	var hasNoStartTime bool
	v.hasRunningContainers = status.State.Waiting == nil && status.State.Terminated == nil
	if t != nil {
		if !t.FinishedAt.Time.IsZero() {
			if t.StartedAt.IsZero() {
				hasNoStartTime = true
			} else {
				v.duration = t.FinishedAt.Sub(t.StartedAt.Time)
			}
			v.completeDuration = t.FinishedAt.Sub(pod.CreationTimestamp.Time)
		}

		defer func() { v.hasTerminated = true }()
		switch {
		case t.ExitCode == 1:
			// expected
		case t.ExitCode == 137 && (t.Reason == "ContainerStatusUnknown" || t.Reason == "Error"):
			// expected, pod was force-killed after grace period
		case t.ExitCode == 128 && (t.Reason == "StartError" || t.Reason == "ContainerCannotRun") && reBug88766.MatchString(t.Message):
			// pod volume teardown races with container start in CRI, which reports a failure
			framework.Logf("pod %s on node %s failed with the symptoms of https://github.com/kubernetes/kubernetes/issues/88766", pod.Name, pod.Spec.NodeName)
		default:
			data, _ := json.MarshalIndent(pod.Status, "", "  ")
			framework.Logf("pod %s on node %s had incorrect final status:\n%s", pod.Name, pod.Spec.NodeName, string(data))
			return fmt.Errorf("pod %s on node %s container unexpected exit code %d: start=%s end=%s reason=%s message=%s", pod.Name, pod.Spec.NodeName, t.ExitCode, t.StartedAt, t.FinishedAt, t.Reason, t.Message)
		}
		switch {
		case v.duration > time.Hour:
			// problem with status reporting
			return fmt.Errorf("pod %s container %s on node %s had very long duration %s: start=%s end=%s", pod.Name, status.Name, pod.Spec.NodeName, v.duration, t.StartedAt, t.FinishedAt)
		case hasNoStartTime:
			// should never happen
			return fmt.Errorf("pod %s container %s on node %s had finish time but not start time: end=%s", pod.Name, status.Name, pod.Spec.NodeName, t.FinishedAt)
		}
	}
	if pod.Status.Phase == v1.PodFailed || pod.Status.Phase == v1.PodSucceeded {
		v.hasTerminalPhase = true
	} else {
		if v.hasTerminalPhase {
			return fmt.Errorf("pod %s on node %s was in a terminal phase and then reverted: %#v", pod.Name, pod.Spec.NodeName, pod.Status)
		}
	}
	return nil
}

func (v *podStartVerifier) VerifyFinal(scenario string, total time.Duration) (*v1.Pod, []error) {
	var errs []error
	pod := v.pod
	if !v.hasTerminalPhase {
		var names []string
		for _, status := range pod.Status.ContainerStatuses {
			if status.State.Running != nil {
				names = append(names, status.Name)
			}
		}
		switch {
		case len(names) > 0:
			errs = append(errs, fmt.Errorf("pod %s on node %s did not reach a terminal phase before being deleted but had running containers: phase=%s, running-containers=%s", pod.Name, pod.Spec.NodeName, pod.Status.Phase, strings.Join(names, ",")))
		case pod.Status.Phase != v1.PodPending:
			errs = append(errs, fmt.Errorf("pod %s on node %s was not Pending but has no running containers: phase=%s", pod.Name, pod.Spec.NodeName, pod.Status.Phase))
		}
	}
	if v.hasRunningContainers {
		data, _ := json.MarshalIndent(pod.Status.ContainerStatuses, "", "  ")
		errs = append(errs, fmt.Errorf("pod %s on node %s had running or unknown container status before being deleted:\n%s", pod.Name, pod.Spec.NodeName, string(data)))
	}

	framework.Logf("Pod %s on node %s %s total=%s run=%s execute=%s", pod.Name, pod.Spec.NodeName, scenario, total, v.completeDuration, v.duration)
	return pod, errs
}
