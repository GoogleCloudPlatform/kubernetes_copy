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
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/tools/cache"
	watchtools "k8s.io/client-go/tools/watch"
	podutil "k8s.io/kubernetes/pkg/api/v1/pod"
	"k8s.io/kubernetes/pkg/client/conditions"
	"k8s.io/kubernetes/test/e2e/framework"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	imageutils "k8s.io/kubernetes/test/utils/image"
	admissionapi "k8s.io/pod-security-admission/api"
)

func recordEvents(events []watch.Event, f func(watch.Event) (bool, error)) func(watch.Event) (bool, error) {
	return func(e watch.Event) (bool, error) {
		events = append(events, e)
		return f(e)
	}
}

// invariantFunc is a func that checks for invariant.
type invariantFunc func(older, newer runtime.Object) error

// checkInvariants checks for invariant of the each events.
func checkInvariants(events []watch.Event, fns ...invariantFunc) error {
	errs := sets.NewString()
	for i := range events {
		j := i + 1
		if j >= len(events) {
			continue
		}
		for _, fn := range fns {
			if err := fn(events[i].Object, events[j].Object); err != nil {
				errs.Insert(err.Error())
			}
		}
	}
	if errs.Len() > 0 {
		return fmt.Errorf("invariants violated:\n* %s", strings.Join(errs.List(), "\n* "))
	}
	return nil
}

// containerInitInvariant checks for an init containers are initialized and invariant on both older and newer.
func containerInitInvariant(older, newer runtime.Object) error {
	oldPod := older.(*v1.Pod)
	newPod := newer.(*v1.Pod)
	if len(oldPod.Spec.InitContainers) == 0 {
		return nil
	}
	if len(oldPod.Spec.InitContainers) != len(newPod.Spec.InitContainers) {
		return fmt.Errorf("init container list changed")
	}
	if oldPod.UID != newPod.UID {
		return fmt.Errorf("two different pods exist in the condition: %s vs %s", oldPod.UID, newPod.UID)
	}
	if err := initContainersInvariants(oldPod); err != nil {
		return err
	}
	if err := initContainersInvariants(newPod); err != nil {
		return err
	}
	oldInit, _, _ := initialized(oldPod)
	newInit, _, _ := initialized(newPod)
	if oldInit && !newInit {
		// TODO: we may in the future enable resetting initialized = false if the kubelet needs to restart it
		// from scratch
		return fmt.Errorf("pod cannot be initialized and then regress to not being initialized")
	}
	return nil
}

// initialized checks the state of all init containers in the pod.
func initialized(pod *v1.Pod) (ok bool, failed bool, err error) {
	allInit := true
	initFailed := false
	for _, s := range pod.Status.InitContainerStatuses {
		switch {
		case initFailed && s.State.Waiting == nil:
			return allInit, initFailed, fmt.Errorf("container %s is after a failed container but isn't waiting", s.Name)
		case allInit && s.State.Waiting == nil:
			return allInit, initFailed, fmt.Errorf("container %s is after an initializing container but isn't waiting", s.Name)
		case s.State.Terminated == nil:
			allInit = false
		case s.State.Terminated.ExitCode != 0:
			allInit = false
			initFailed = true
		case !s.Ready:
			return allInit, initFailed, fmt.Errorf("container %s initialized but isn't marked as ready", s.Name)
		}
	}
	return allInit, initFailed, nil
}

func initContainersInvariants(pod *v1.Pod) error {
	allInit, initFailed, err := initialized(pod)
	if err != nil {
		return err
	}
	if !allInit || initFailed {
		for _, s := range pod.Status.ContainerStatuses {
			if s.State.Waiting == nil || s.RestartCount != 0 {
				return fmt.Errorf("container %s is not waiting but initialization not complete", s.Name)
			}
			if s.State.Waiting.Reason != "PodInitializing" {
				return fmt.Errorf("container %s should have reason PodInitializing: %s", s.Name, s.State.Waiting.Reason)
			}
		}
	}
	_, c := podutil.GetPodCondition(&pod.Status, v1.PodInitialized)
	if c == nil {
		return fmt.Errorf("pod does not have initialized condition")
	}
	if c.LastTransitionTime.IsZero() {
		return fmt.Errorf("PodInitialized condition should always have a transition time")
	}
	switch {
	case c.Status == v1.ConditionUnknown:
		return fmt.Errorf("PodInitialized condition should never be Unknown")
	case c.Status == v1.ConditionTrue && (initFailed || !allInit):
		return fmt.Errorf("PodInitialized condition was True but all not all containers initialized")
	case c.Status == v1.ConditionFalse && (!initFailed && allInit):
		return fmt.Errorf("PodInitialized condition was False but all containers initialized")
	}
	return nil
}

var _ = SIGDescribe("InitContainer", framework.WithNodeConformance(), func() {
	f := framework.NewDefaultFramework("init-container")
	f.NamespacePodSecurityLevel = admissionapi.LevelBaseline
	var podClient *e2epod.PodClient
	ginkgo.BeforeEach(func() {
		podClient = e2epod.NewPodClient(f)
	})

	/*
		Release: v1.12
		Testname: init-container-starts-app-restartnever-pod
		Description: Ensure that all InitContainers are started
		and all containers in pod are voluntarily terminated with exit status 0,
		and the system is not going to restart any of these containers
		when Pod has restart policy as RestartNever.
	*/
	framework.ConformanceIt("should invoke init containers on a RestartNever pod", func(ctx context.Context) {
		ginkgo.By("creating the pod")
		name := "pod-init-" + string(uuid.NewUUID())
		value := strconv.Itoa(time.Now().Nanosecond())
		pod := &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: name,
				Labels: map[string]string{
					"name": "foo",
					"time": value,
				},
			},
			Spec: v1.PodSpec{
				RestartPolicy: v1.RestartPolicyNever,
				InitContainers: []v1.Container{
					{
						Name:    "init1",
						Image:   imageutils.GetE2EImage(imageutils.BusyBox),
						Command: []string{"/bin/true"},
					},
					{
						Name:    "init2",
						Image:   imageutils.GetE2EImage(imageutils.BusyBox),
						Command: []string{"/bin/true"},
					},
				},
				Containers: []v1.Container{
					{
						Name:    "run1",
						Image:   imageutils.GetE2EImage(imageutils.BusyBox),
						Command: []string{"/bin/true"},
					},
				},
			},
		}
		framework.Logf("PodSpec: initContainers in spec.initContainers")
		startedPod := podClient.Create(ctx, pod)

		fieldSelector := fields.OneTermEqualSelector("metadata.name", startedPod.Name).String()
		w := &cache.ListWatch{
			WatchFunc: func(options metav1.ListOptions) (i watch.Interface, e error) {
				options.FieldSelector = fieldSelector
				return podClient.Watch(ctx, options)
			},
		}
		var events []watch.Event
		ctx, cancel := watchtools.ContextWithOptionalTimeout(ctx, framework.PodStartTimeout)
		defer cancel()
		event, err := watchtools.Until(ctx, startedPod.ResourceVersion, w,
			recordEvents(events, conditions.PodCompleted),
		)
		framework.ExpectNoError(err)

		checkInvariants(events, containerInitInvariant)
		endPod := event.Object.(*v1.Pod)
		gomega.Expect(endPod.Status.Phase).To(gomega.Equal(v1.PodSucceeded))
		_, init := podutil.GetPodCondition(&endPod.Status, v1.PodInitialized)
		gomega.Expect(init).NotTo(gomega.BeNil())
		gomega.Expect(init.Status).To(gomega.Equal(v1.ConditionTrue))

		gomega.Expect(endPod.Status.InitContainerStatuses).To(gomega.HaveLen(2))
		for _, status := range endPod.Status.InitContainerStatuses {
			if !status.Ready {
				framework.Failf("init container %s should be in Ready status", status.Name)
			}
			gomega.Expect(status.State.Terminated).NotTo(gomega.BeNil())
			gomega.Expect(status.State.Terminated.ExitCode).To(gomega.BeZero())
		}
	})

	/*
		Release: v1.12
		Testname: init-container-starts-app-restartalways-pod
		Description: Ensure that all InitContainers are started
		and all containers in pod started
		and at least one container is still running or is in the process of being restarted
		when Pod has restart policy as RestartAlways.
	*/
	framework.ConformanceIt("should invoke init containers on a RestartAlways pod", func(ctx context.Context) {
		ginkgo.By("creating the pod")
		name := "pod-init-" + string(uuid.NewUUID())
		value := strconv.Itoa(time.Now().Nanosecond())
		pod := &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: name,
				Labels: map[string]string{
					"name": "foo",
					"time": value,
				},
			},
			Spec: v1.PodSpec{
				InitContainers: []v1.Container{
					{
						Name:    "init1",
						Image:   imageutils.GetE2EImage(imageutils.BusyBox),
						Command: []string{"/bin/true"},
					},
					{
						Name:    "init2",
						Image:   imageutils.GetE2EImage(imageutils.BusyBox),
						Command: []string{"/bin/true"},
					},
				},
				Containers: []v1.Container{
					{
						Name:  "run1",
						Image: imageutils.GetPauseImageName(),
						Resources: v1.ResourceRequirements{
							Limits: v1.ResourceList{
								v1.ResourceCPU: *resource.NewMilliQuantity(100, resource.DecimalSI),
							},
						},
					},
				},
			},
		}
		framework.Logf("PodSpec: initContainers in spec.initContainers")
		startedPod := podClient.Create(ctx, pod)

		fieldSelector := fields.OneTermEqualSelector("metadata.name", startedPod.Name).String()
		w := &cache.ListWatch{
			WatchFunc: func(options metav1.ListOptions) (i watch.Interface, e error) {
				options.FieldSelector = fieldSelector
				return podClient.Watch(ctx, options)
			},
		}
		var events []watch.Event
		ctx, cancel := watchtools.ContextWithOptionalTimeout(ctx, framework.PodStartTimeout)
		defer cancel()
		event, err := watchtools.Until(ctx, startedPod.ResourceVersion, w, recordEvents(events, conditions.PodRunning))
		framework.ExpectNoError(err)

		checkInvariants(events, containerInitInvariant)
		endPod := event.Object.(*v1.Pod)
		gomega.Expect(endPod.Status.Phase).To(gomega.Equal(v1.PodRunning))
		_, init := podutil.GetPodCondition(&endPod.Status, v1.PodInitialized)
		gomega.Expect(init).NotTo(gomega.BeNil())
		gomega.Expect(init.Status).To(gomega.Equal(v1.ConditionTrue))

		gomega.Expect(endPod.Status.InitContainerStatuses).To(gomega.HaveLen(2))
		for _, status := range endPod.Status.InitContainerStatuses {
			if !status.Ready {
				framework.Failf("init container %s should be in Ready status", status.Name)
			}
			gomega.Expect(status.State.Terminated).NotTo(gomega.BeNil())
			gomega.Expect(status.State.Terminated.ExitCode).To(gomega.BeZero())
		}
	})

	/*
		Release: v1.12
		Testname: init-container-fails-stops-app-restartalways-pod
		Description: Ensure that app container is not started
		when all InitContainers failed to start
		and Pod has restarted for few occurrences
		and pod has restart policy as RestartAlways.
	*/
	framework.ConformanceIt("should not start app containers if init containers fail on a RestartAlways pod", func(ctx context.Context) {
		ginkgo.By("creating the pod")
		name := "pod-init-" + string(uuid.NewUUID())
		value := strconv.Itoa(time.Now().Nanosecond())

		pod := &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: name,
				Labels: map[string]string{
					"name": "foo",
					"time": value,
				},
			},
			Spec: v1.PodSpec{
				InitContainers: []v1.Container{
					{
						Name:    "init1",
						Image:   imageutils.GetE2EImage(imageutils.BusyBox),
						Command: []string{"/bin/false"},
					},
					{
						Name:    "init2",
						Image:   imageutils.GetE2EImage(imageutils.BusyBox),
						Command: []string{"/bin/true"},
					},
				},
				Containers: []v1.Container{
					{
						Name:  "run1",
						Image: imageutils.GetPauseImageName(),
						Resources: v1.ResourceRequirements{
							Limits: v1.ResourceList{
								v1.ResourceCPU: *resource.NewMilliQuantity(100, resource.DecimalSI),
							},
						},
					},
				},
			},
		}
		framework.Logf("PodSpec: initContainers in spec.initContainers")
		startedPod := podClient.Create(ctx, pod)

		fieldSelector := fields.OneTermEqualSelector("metadata.name", startedPod.Name).String()
		w := &cache.ListWatch{
			WatchFunc: func(options metav1.ListOptions) (i watch.Interface, e error) {
				options.FieldSelector = fieldSelector
				return podClient.Watch(ctx, options)
			},
		}

		var events []watch.Event
		ctx, cancel := watchtools.ContextWithOptionalTimeout(ctx, framework.PodStartTimeout)
		defer cancel()
		event, err := watchtools.Until(
			ctx,
			startedPod.ResourceVersion,
			w,
			// check for the first container to fail at least once
			func(evt watch.Event) (bool, error) {
				switch t := evt.Object.(type) {
				case *v1.Pod:
					for _, status := range t.Status.ContainerStatuses {
						if status.State.Waiting == nil {
							return false, fmt.Errorf("container %q should not be out of waiting: %s", status.Name, toDebugJSON(status))
						}
						if status.State.Waiting.Reason != "PodInitializing" {
							return false, fmt.Errorf("container %q should have reason PodInitializing: %s", status.Name, toDebugJSON(status))
						}
					}
					if len(t.Status.InitContainerStatuses) != 2 {
						return false, nil
					}
					status := t.Status.InitContainerStatuses[1]
					if status.State.Waiting == nil {
						return false, fmt.Errorf("second init container should not be out of waiting: %s", toDebugJSON(status))
					}
					if status.State.Waiting.Reason != "PodInitializing" {
						return false, fmt.Errorf("second init container should have reason PodInitializing: %s", toDebugJSON(status))
					}
					status = t.Status.InitContainerStatuses[0]
					if status.State.Terminated != nil && status.State.Terminated.ExitCode == 0 {
						return false, fmt.Errorf("first init container should have exitCode != 0: %s", toDebugJSON(status))
					}
					// continue until we see an attempt to restart the pod
					return status.LastTerminationState.Terminated != nil, nil
				default:
					return false, fmt.Errorf("unexpected object: %#v", t)
				}
			},
			// verify we get two restarts
			func(evt watch.Event) (bool, error) {
				switch t := evt.Object.(type) {
				case *v1.Pod:
					status := t.Status.InitContainerStatuses[0]
					if status.RestartCount < 3 {
						return false, nil
					}
					framework.Logf("init container has failed twice: %#v", t)
					// TODO: more conditions
					return true, nil
				default:
					return false, fmt.Errorf("unexpected object: %#v", t)
				}
			},
		)
		framework.ExpectNoError(err)

		checkInvariants(events, containerInitInvariant)
		endPod := event.Object.(*v1.Pod)
		gomega.Expect(endPod.Status.Phase).To(gomega.Equal(v1.PodPending))
		_, init := podutil.GetPodCondition(&endPod.Status, v1.PodInitialized)
		gomega.Expect(init).NotTo(gomega.BeNil())
		gomega.Expect(init.Status).To(gomega.Equal(v1.ConditionFalse))
		gomega.Expect(init.Reason).To(gomega.Equal("ContainersNotInitialized"))
		gomega.Expect(init.Message).To(gomega.Equal("containers with incomplete status: [init1 init2]"))
		gomega.Expect(endPod.Status.InitContainerStatuses).To(gomega.HaveLen(2))
	})

	/*
		Release: v1.12
		Testname: init-container-fails-stops-app-restartnever-pod
		Description: Ensure that app container is not started
		when at least one InitContainer fails to start and Pod has restart policy as RestartNever.
	*/
	framework.ConformanceIt("should not start app containers and fail the pod if init containers fail on a RestartNever pod", func(ctx context.Context) {
		ginkgo.By("creating the pod")
		name := "pod-init-" + string(uuid.NewUUID())
		value := strconv.Itoa(time.Now().Nanosecond())
		pod := &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: name,
				Labels: map[string]string{
					"name": "foo",
					"time": value,
				},
			},
			Spec: v1.PodSpec{
				RestartPolicy: v1.RestartPolicyNever,
				InitContainers: []v1.Container{
					{
						Name:    "init1",
						Image:   imageutils.GetE2EImage(imageutils.BusyBox),
						Command: []string{"/bin/true"},
					},
					{
						Name:    "init2",
						Image:   imageutils.GetE2EImage(imageutils.BusyBox),
						Command: []string{"/bin/false"},
					},
				},
				Containers: []v1.Container{
					{
						Name:    "run1",
						Image:   imageutils.GetE2EImage(imageutils.BusyBox),
						Command: []string{"/bin/true"},
						Resources: v1.ResourceRequirements{
							Limits: v1.ResourceList{
								v1.ResourceCPU: *resource.NewMilliQuantity(100, resource.DecimalSI),
							},
						},
					},
				},
			},
		}
		framework.Logf("PodSpec: initContainers in spec.initContainers")
		startedPod := podClient.Create(ctx, pod)

		fieldSelector := fields.OneTermEqualSelector("metadata.name", startedPod.Name).String()
		w := &cache.ListWatch{
			WatchFunc: func(options metav1.ListOptions) (i watch.Interface, e error) {
				options.FieldSelector = fieldSelector
				return podClient.Watch(ctx, options)
			},
		}

		var events []watch.Event
		ctx, cancel := watchtools.ContextWithOptionalTimeout(ctx, framework.PodStartTimeout)
		defer cancel()
		event, err := watchtools.Until(
			ctx, startedPod.ResourceVersion, w,
			recordEvents(events,
				// check for the second container to fail at least once
				func(evt watch.Event) (bool, error) {
					switch t := evt.Object.(type) {
					case *v1.Pod:
						for _, status := range t.Status.ContainerStatuses {
							if status.State.Waiting == nil {
								return false, fmt.Errorf("container %q should not be out of waiting: %s", status.Name, toDebugJSON(status))
							}
							if status.State.Waiting.Reason != "PodInitializing" {
								return false, fmt.Errorf("container %q should have reason PodInitializing: %s", status.Name, toDebugJSON(status))
							}
						}
						if len(t.Status.InitContainerStatuses) != 2 {
							return false, nil
						}
						status := t.Status.InitContainerStatuses[0]
						if status.State.Terminated == nil {
							if status.State.Waiting != nil && status.State.Waiting.Reason != "" && status.State.Waiting.Reason != "PodInitializing" {
								return false, fmt.Errorf("second init container should have reason PodInitializing: %s", toDebugJSON(status))
							}
							return false, nil
						}
						if status.State.Terminated != nil && status.State.Terminated.ExitCode != 0 {
							return false, fmt.Errorf("first init container should have exitCode == 0: %s", toDebugJSON(status))
						}
						status = t.Status.InitContainerStatuses[1]
						if status.State.Terminated == nil {
							return false, nil
						}
						if status.State.Terminated.ExitCode == 0 {
							return false, fmt.Errorf("second init container should have failed: %s", toDebugJSON(status))
						}
						return true, nil
					default:
						return false, fmt.Errorf("unexpected object: %#v", t)
					}
				}),
			recordEvents(events, conditions.PodCompleted),
		)
		framework.ExpectNoError(err)

		checkInvariants(events, containerInitInvariant)
		endPod := event.Object.(*v1.Pod)

		gomega.Expect(endPod.Status.Phase).To(gomega.Equal(v1.PodFailed))
		_, init := podutil.GetPodCondition(&endPod.Status, v1.PodInitialized)
		gomega.Expect(init).NotTo(gomega.BeNil())
		gomega.Expect(init.Status).To(gomega.Equal(v1.ConditionFalse))
		gomega.Expect(init.Reason).To(gomega.Equal("ContainersNotInitialized"))
		gomega.Expect(init.Message).To(gomega.Equal("containers with incomplete status: [init2]"))
		gomega.Expect(endPod.Status.InitContainerStatuses).To(gomega.HaveLen(2))
		gomega.Expect(endPod.Status.ContainerStatuses[0].State.Waiting).ToNot(gomega.BeNil())
	})
})

// toDebugJSON converts an object to its JSON representation for debug logging
// purposes instead of using a struct.
func toDebugJSON(obj interface{}) string {
	m, err := json.Marshal(obj)
	if err != nil {
		return fmt.Sprintf("<error: %v>", err)
	}
	return string(m)
}
