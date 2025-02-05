/*
Copyright 2019 The Kubernetes Authors.

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

package pod

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"github.com/onsi/gomega/gcustom"
	"github.com/onsi/gomega/types"

	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	apitypes "k8s.io/apimachinery/pkg/types"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/kubectl/pkg/util/podutils"
	"k8s.io/kubernetes/test/e2e/framework"
	testutils "k8s.io/kubernetes/test/utils"
	"k8s.io/kubernetes/test/utils/format"
)

const (
	// defaultPodDeletionTimeout is the default timeout for deleting pod.
	defaultPodDeletionTimeout = 3 * time.Minute

	// podListTimeout is how long to wait for the pod to be listable.
	podListTimeout = time.Minute

	podRespondingTimeout = 15 * time.Minute

	// How long pods have to become scheduled onto nodes
	podScheduledBeforeTimeout = podListTimeout + (20 * time.Second)

	// podStartTimeout is how long to wait for the pod to be started.
	podStartTimeout = 5 * time.Minute

	// singleCallTimeout is how long to try single API calls (like 'get' or 'list'). Used to prevent
	// transient failures from failing tests.
	singleCallTimeout = 5 * time.Minute

	// Some pods can take much longer to get ready due to volume attach/detach latency.
	slowPodStartTimeout = 15 * time.Minute
)

type podCondition func(pod *v1.Pod) (bool, error)

// BeRunningNoRetries verifies that a pod starts running. It's a permanent
// failure when the pod enters some other permanent phase.
func BeRunningNoRetries() types.GomegaMatcher {
	return gomega.And(
		// This additional matcher checks for the final error condition.
		gcustom.MakeMatcher(func(pod *v1.Pod) (bool, error) {
			switch pod.Status.Phase {
			case v1.PodFailed, v1.PodSucceeded:
				return false, gomega.StopTrying(fmt.Sprintf("Expected pod to reach phase %q, got final phase %q instead:\n%s", v1.PodRunning, pod.Status.Phase, format.Object(pod, 1)))
			default:
				return true, nil
			}
		}),
		BeInPhase(v1.PodRunning),
	)
}

// BeInPhase matches if pod.status.phase is the expected phase.
func BeInPhase(phase v1.PodPhase) types.GomegaMatcher {
	// A simple implementation of this would be:
	// return gomega.HaveField("Status.Phase", phase)
	//
	// But that produces a fairly generic
	//     Value for field 'Status.Phase' failed to satisfy matcher.
	// failure message and doesn't show the pod. We can do better than
	// that with a custom matcher.

	return gcustom.MakeMatcher(func(pod *v1.Pod) (bool, error) {
		return pod.Status.Phase == phase, nil
	}).WithTemplate("Expected Pod {{.To}} be in {{format .Data}}\nGot instead:\n{{.FormattedActual}}").WithTemplateData(phase)
}

// WaitForAlmostAllReady waits up to timeout for the following conditions:
// 1. At least minPods Pods in Namespace ns are Running and Ready
// 2. All Pods in Namespace ns are either Ready or Succeeded
// 3. All Pods part of a ReplicaSet or ReplicationController in Namespace ns are Ready
//
// After the timeout has elapsed, an error is returned if the number of Pods in a Pending Phase
// is greater than allowedNotReadyPods.
//
// It is generally recommended to use WaitForPodsRunningReady instead of this function
// whenever possible, because its behavior is more intuitive. Similar to WaitForPodsRunningReady,
// this function requests the list of pods on every iteration, making it useful for situations
// where the set of Pods is likely changing, such as during cluster startup.
//
// If minPods or allowedNotReadyPods are -1, this method returns immediately
// without waiting.
func WaitForAlmostAllPodsReady(ctx context.Context, c clientset.Interface, ns string, minPods, allowedNotReadyPods int, timeout time.Duration) error {
	if minPods == -1 || allowedNotReadyPods == -1 {
		return nil
	}

	// We get the new list of pods, replication controllers, and replica
	// sets in every iteration because more pods come online during startup
	// and we want to ensure they are also checked.
	//
	// This struct gets populated while polling, then gets checked, and in
	// case of a timeout is included in the failure message.
	type state struct {
		ReplicationControllers []v1.ReplicationController
		ReplicaSets            []appsv1.ReplicaSet
		Pods                   []v1.Pod
	}

	nOk := 0
	badPods := []v1.Pod{}
	otherPods := []v1.Pod{}
	succeededPods := []string{}

	err := framework.Gomega().Eventually(ctx, framework.HandleRetry(func(ctx context.Context) (*state, error) {

		rcList, err := c.CoreV1().ReplicationControllers(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("listing replication controllers in namespace %s: %w", ns, err)
		}
		rsList, err := c.AppsV1().ReplicaSets(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("listing replication sets in namespace %s: %w", ns, err)
		}
		podList, err := c.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("listing pods in namespace %s: %w", ns, err)
		}
		return &state{
			ReplicationControllers: rcList.Items,
			ReplicaSets:            rsList.Items,
			Pods:                   podList.Items,
		}, nil
	})).WithTimeout(timeout).Should(framework.MakeMatcher(func(s *state) (func() string, error) {
		replicas, replicaOk := int32(0), int32(0)
		for _, rc := range s.ReplicationControllers {
			replicas += *rc.Spec.Replicas
			replicaOk += rc.Status.ReadyReplicas
		}
		for _, rs := range s.ReplicaSets {
			replicas += *rs.Spec.Replicas
			replicaOk += rs.Status.ReadyReplicas
		}

		nOk = 0
		badPods = []v1.Pod{}
		otherPods = []v1.Pod{}
		succeededPods = []string{}
		for _, pod := range s.Pods {
			res, err := testutils.PodRunningReady(&pod)
			switch {
			case res && err == nil:
				nOk++
			case pod.Status.Phase == v1.PodSucceeded:
				// it doesn't make sense to wait for this pod
				succeededPods = append(succeededPods, pod.Name)
			case pod.Status.Phase == v1.PodFailed:
				// ignore failed pods that are controlled by some controller
				if metav1.GetControllerOf(&pod) == nil {
					badPods = append(badPods, pod)
				}
			default:
				otherPods = append(otherPods, pod)
			}
		}
		done := replicaOk == replicas && nOk >= minPods && (len(badPods)+len(otherPods)) == 0
		if done {
			return nil, nil
		}

		// Delayed formatting of a failure message.
		return func() string {
			var buffer strings.Builder
			buffer.WriteString(fmt.Sprintf("Expected all pods (need at least %d) in namespace %q to be running and ready (except for %d).\n", minPods, ns, allowedNotReadyPods))
			buffer.WriteString(fmt.Sprintf("%d / %d pods were running and ready.\n", nOk, len(s.Pods)))
			buffer.WriteString(fmt.Sprintf("Expected %d pod replicas, %d are Running and Ready.\n", replicas, replicaOk))
			if len(succeededPods) > 0 {
				buffer.WriteString(fmt.Sprintf("Pods that completed successfully:\n%s", format.Object(succeededPods, 1)))
			}
			if len(badPods) > 0 {
				buffer.WriteString(fmt.Sprintf("Pods that failed and were not controlled by some controller:\n%s", format.Object(badPods, 1)))
			}
			if len(otherPods) > 0 {
				buffer.WriteString(fmt.Sprintf("Pods that were neither completed nor running:\n%s", format.Object(otherPods, 1)))
			}
			return buffer.String()
		}, nil
	}))

	// An error might not be fatal.
	if len(otherPods) <= allowedNotReadyPods {
		return nil
	}
	return err
}

// WaitForPodsRunningReady waits up to timeout for the following conditions:
//  1. At least minPods Pods in Namespace ns are Running and Ready
//  2. No Pods in Namespace ns are Failed and not owned by a controller or Pending
//
// An error is returned if either of these conditions are not met within the timeout.
//
// It has separate behavior from other 'wait for' pods functions in
// that it requests the list of pods on every iteration. This is useful, for
// example, in cluster startup, because the number of pods increases while
// waiting. All pods that are in SUCCESS state are not counted.
func WaitForPodsRunningReady(ctx context.Context, c clientset.Interface, ns string, minPods int, timeout time.Duration) error {

	return framework.Gomega().Eventually(ctx, framework.HandleRetry(func(ctx context.Context) ([]v1.Pod, error) {

		podList, err := c.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("listing pods in namespace %s: %w", ns, err)
		}
		return podList.Items, nil
	})).WithTimeout(timeout).Should(framework.MakeMatcher(func(pods []v1.Pod) (func() string, error) {

		nOk := 0
		badPods := []v1.Pod{}
		otherPods := []v1.Pod{}
		succeededPods := []string{}

		for _, pod := range pods {
			res, err := testutils.PodRunningReady(&pod)
			switch {
			case res && err == nil:
				nOk++
			case pod.Status.Phase == v1.PodSucceeded:
				// ignore succeeded pods
				succeededPods = append(succeededPods, pod.Name)
			case pod.Status.Phase == v1.PodFailed:
				// ignore failed pods that are controlled by some controller
				if metav1.GetControllerOf(&pod) == nil {
					badPods = append(badPods, pod)
				}
			default:
				otherPods = append(otherPods, pod)
			}
		}
		if nOk >= minPods && len(badPods)+len(otherPods) == 0 {
			return nil, nil
		}

		// Delayed formatting of a failure message.
		return func() string {
			var buffer strings.Builder
			buffer.WriteString(fmt.Sprintf("Expected all pods (need at least %d) in namespace %q to be running and ready \n", minPods, ns))
			buffer.WriteString(fmt.Sprintf("%d / %d pods were running and ready.\n", nOk, len(pods)))
			if len(succeededPods) > 0 {
				buffer.WriteString(fmt.Sprintf("Pods that completed successfully:\n%s", format.Object(succeededPods, 1)))
			}
			if len(badPods) > 0 {
				buffer.WriteString(fmt.Sprintf("Pods that failed and were not controlled by some controller:\n%s", format.Object(badPods, 1)))
			}
			if len(otherPods) > 0 {
				buffer.WriteString(fmt.Sprintf("Pods that were neither completed nor running:\n%s", format.Object(otherPods, 1)))
			}
			return buffer.String()
		}, nil
	}))

}

// WaitForPodCondition waits a pods to be matched to the given condition.
// The condition callback may use gomega.StopTrying to abort early.
func WaitForPodCondition(ctx context.Context, c clientset.Interface, ns, podName, conditionDesc string, timeout time.Duration, condition podCondition) error {
	return framework.Gomega().
		Eventually(ctx, framework.RetryNotFound(framework.GetObject(c.CoreV1().Pods(ns).Get, podName, metav1.GetOptions{}))).
		WithTimeout(timeout).
		Should(framework.MakeMatcher(func(pod *v1.Pod) (func() string, error) {
			done, err := condition(pod)
			if err != nil {
				return nil, err
			}
			if done {
				return nil, nil
			}
			return func() string {
				return fmt.Sprintf("expected pod to be %s, got instead:\n%s", conditionDesc, format.Object(pod, 1))
			}, nil
		}))
}

// Range determines how many items must exist and how many must match a certain
// condition. Values <= 0 are ignored.
// TODO (?): move to test/e2e/framework/range
type Range struct {
	// MinMatching must be <= actual matching items or <= 0.
	MinMatching int
	// MaxMatching must be >= actual matching items or <= 0.
	// To check for "no matching items", set NonMatching.
	MaxMatching int
	// NoneMatching indicates that no item must match.
	NoneMatching bool
	// AllMatching indicates that all items must match.
	AllMatching bool
	// MinFound must be <= existing items or <= 0.
	MinFound int
}

// Min returns how many items must exist.
func (r Range) Min() int {
	min := r.MinMatching
	if min < r.MinFound {
		min = r.MinFound
	}
	return min
}

// WaitForPods waits for pods in the given namespace to match the given
// condition. How many pods must exist and how many must match the condition
// is determined by the range parameter. The condition callback may use
// gomega.StopTrying(...).Now() to abort early. The condition description
// will be used with "expected pods to <description>".
func WaitForPods(ctx context.Context, c clientset.Interface, ns string, opts metav1.ListOptions, r Range, timeout time.Duration, conditionDesc string, condition func(*v1.Pod) bool) (*v1.PodList, error) {
	var finalPods *v1.PodList
	minPods := r.Min()
	match := func(pods *v1.PodList) (func() string, error) {
		finalPods = pods

		if len(pods.Items) < minPods {
			return func() string {
				return fmt.Sprintf("expected at least %d pods, only got %d", minPods, len(pods.Items))
			}, nil
		}

		var nonMatchingPods, matchingPods []v1.Pod
		for _, pod := range pods.Items {
			if condition(&pod) {
				matchingPods = append(matchingPods, pod)
			} else {
				nonMatchingPods = append(nonMatchingPods, pod)
			}
		}
		matching := len(pods.Items) - len(nonMatchingPods)
		if matching < r.MinMatching && r.MinMatching > 0 {
			return func() string {
				return fmt.Sprintf("expected at least %d pods to %s, %d out of %d were not:\n%s",
					r.MinMatching, conditionDesc, len(nonMatchingPods), len(pods.Items),
					format.Object(nonMatchingPods, 1))
			}, nil
		}
		if len(nonMatchingPods) > 0 && r.AllMatching {
			return func() string {
				return fmt.Sprintf("expected all pods to %s, %d out of %d were not:\n%s",
					conditionDesc, len(nonMatchingPods), len(pods.Items),
					format.Object(nonMatchingPods, 1))
			}, nil
		}
		if matching > r.MaxMatching && r.MaxMatching > 0 {
			return func() string {
				return fmt.Sprintf("expected at most %d pods to %s, %d out of %d were:\n%s",
					r.MinMatching, conditionDesc, len(matchingPods), len(pods.Items),
					format.Object(matchingPods, 1))
			}, nil
		}
		if matching > 0 && r.NoneMatching {
			return func() string {
				return fmt.Sprintf("expected no pods to %s, %d out of %d were:\n%s",
					conditionDesc, len(matchingPods), len(pods.Items),
					format.Object(matchingPods, 1))
			}, nil
		}
		return nil, nil
	}

	err := framework.Gomega().
		Eventually(ctx, framework.ListObjects(c.CoreV1().Pods(ns).List, opts)).
		WithTimeout(timeout).
		Should(framework.MakeMatcher(match))
	return finalPods, err
}

// RunningReady checks whether pod p's phase is running and it has a ready
// condition of status true.
func RunningReady(p *v1.Pod) bool {
	return p.Status.Phase == v1.PodRunning && podutils.IsPodReady(p)
}

// WaitForPodsRunning waits for a given `timeout` to evaluate if a certain amount of pods in given `ns` are running.
func WaitForPodsRunning(ctx context.Context, c clientset.Interface, ns string, num int, timeout time.Duration) error {
	_, err := WaitForPods(ctx, c, ns, metav1.ListOptions{}, Range{MinMatching: num, MaxMatching: num}, timeout,
		"be running and ready", func(pod *v1.Pod) bool {
			ready, _ := testutils.PodRunningReady(pod)
			return ready
		})
	return err
}

// WaitForPodsSchedulingGated waits for a given `timeout` to evaluate if a certain amount of pods in given `ns` stay in scheduling gated state.
func WaitForPodsSchedulingGated(ctx context.Context, c clientset.Interface, ns string, num int, timeout time.Duration) error {
	_, err := WaitForPods(ctx, c, ns, metav1.ListOptions{}, Range{MinMatching: num, MaxMatching: num}, timeout,
		"be in scheduling gated state", func(pod *v1.Pod) bool {
			for _, condition := range pod.Status.Conditions {
				if condition.Type == v1.PodScheduled && condition.Reason == v1.PodReasonSchedulingGated {
					return true
				}
			}
			return false
		})
	return err
}

// WaitForPodsWithSchedulingGates waits for a given `timeout` to evaluate if a certain amount of pods in given `ns`
// match the given `schedulingGates`stay in scheduling gated state.
func WaitForPodsWithSchedulingGates(ctx context.Context, c clientset.Interface, ns string, num int, timeout time.Duration, schedulingGates []v1.PodSchedulingGate) error {
	_, err := WaitForPods(ctx, c, ns, metav1.ListOptions{}, Range{MinMatching: num, MaxMatching: num}, timeout,
		"have certain scheduling gates", func(pod *v1.Pod) bool {
			return reflect.DeepEqual(pod.Spec.SchedulingGates, schedulingGates)
		})
	return err
}

// WaitForPodTerminatedInNamespace returns an error if it takes too long for the pod to terminate,
// if the pod Get api returns an error (IsNotFound or other), or if the pod failed (and thus did not
// terminate) with an unexpected reason. Typically called to test that the passed-in pod is fully
// terminated (reason==""), but may be called to detect if a pod did *not* terminate according to
// the supplied reason.
func WaitForPodTerminatedInNamespace(ctx context.Context, c clientset.Interface, podName, reason, namespace string) error {
	return WaitForPodCondition(ctx, c, namespace, podName, fmt.Sprintf("terminated with reason %s", reason), podStartTimeout, func(pod *v1.Pod) (bool, error) {
		// Only consider Failed pods. Successful pods will be deleted and detected in
		// waitForPodCondition's Get call returning `IsNotFound`
		if pod.Status.Phase == v1.PodFailed {
			if pod.Status.Reason == reason { // short-circuit waitForPodCondition's loop
				return true, nil
			}
			return true, fmt.Errorf("Expected pod %q in namespace %q to be terminated with reason %q, got reason: %q", podName, namespace, reason, pod.Status.Reason)
		}
		return false, nil
	})
}

// WaitForPodTerminatingInNamespaceTimeout returns if the pod is terminating, or an error if it is not after the timeout.
func WaitForPodTerminatingInNamespaceTimeout(ctx context.Context, c clientset.Interface, podName, namespace string, timeout time.Duration) error {
	return WaitForPodCondition(ctx, c, namespace, podName, "is terminating", timeout, func(pod *v1.Pod) (bool, error) {
		if pod.DeletionTimestamp != nil {
			return true, nil
		}
		return false, nil
	})
}

// WaitForPodSuccessInNamespaceTimeout returns nil if the pod reached state success, or an error if it reached failure or ran too long.
func WaitForPodSuccessInNamespaceTimeout(ctx context.Context, c clientset.Interface, podName, namespace string, timeout time.Duration) error {
	return WaitForPodCondition(ctx, c, namespace, podName, fmt.Sprintf("%s or %s", v1.PodSucceeded, v1.PodFailed), timeout, func(pod *v1.Pod) (bool, error) {
		if pod.DeletionTimestamp == nil && pod.Spec.RestartPolicy == v1.RestartPolicyAlways {
			return true, gomega.StopTrying(fmt.Sprintf("pod %q will never terminate with a succeeded state since its restart policy is Always", podName))
		}
		switch pod.Status.Phase {
		case v1.PodSucceeded:
			ginkgo.By("Saw pod success")
			return true, nil
		case v1.PodFailed:
			return true, gomega.StopTrying(fmt.Sprintf("pod %q failed with status: \n%s", podName, format.Object(pod.Status, 1)))
		default:
			return false, nil
		}
	})
}

// WaitForPodNameUnschedulableInNamespace returns an error if it takes too long for the pod to become Pending
// and have condition Status equal to Unschedulable,
// if the pod Get api returns an error (IsNotFound or other), or if the pod failed with an unexpected reason.
// Typically called to test that the passed-in pod is Pending and Unschedulable.
func WaitForPodNameUnschedulableInNamespace(ctx context.Context, c clientset.Interface, podName, namespace string) error {
	return WaitForPodCondition(ctx, c, namespace, podName, v1.PodReasonUnschedulable, podStartTimeout, func(pod *v1.Pod) (bool, error) {
		// Only consider Failed pods. Successful pods will be deleted and detected in
		// waitForPodCondition's Get call returning `IsNotFound`
		if pod.Status.Phase == v1.PodPending {
			for _, cond := range pod.Status.Conditions {
				if cond.Type == v1.PodScheduled && cond.Status == v1.ConditionFalse && cond.Reason == v1.PodReasonUnschedulable {
					return true, nil
				}
			}
		}
		if pod.Status.Phase == v1.PodRunning || pod.Status.Phase == v1.PodSucceeded || pod.Status.Phase == v1.PodFailed {
			return true, fmt.Errorf("Expected pod %q in namespace %q to be in phase Pending, but got phase: %v", podName, namespace, pod.Status.Phase)
		}
		return false, nil
	})
}

// WaitForPodNameRunningInNamespace waits default amount of time (PodStartTimeout) for the specified pod to become running.
// Returns an error if timeout occurs first, or pod goes in to failed state.
func WaitForPodNameRunningInNamespace(ctx context.Context, c clientset.Interface, podName, namespace string) error {
	return WaitTimeoutForPodRunningInNamespace(ctx, c, podName, namespace, podStartTimeout)
}

// WaitForPodRunningInNamespaceSlow waits an extended amount of time (slowPodStartTimeout) for the specified pod to become running.
// The resourceVersion is used when Watching object changes, it tells since when we care
// about changes to the pod. Returns an error if timeout occurs first, or pod goes in to failed state.
func WaitForPodRunningInNamespaceSlow(ctx context.Context, c clientset.Interface, podName, namespace string) error {
	return WaitTimeoutForPodRunningInNamespace(ctx, c, podName, namespace, slowPodStartTimeout)
}

// WaitTimeoutForPodRunningInNamespace waits the given timeout duration for the specified pod to become running.
// It does not need to exist yet when this function gets called and the pod is not expected to be recreated
// when it succeeds or fails.
func WaitTimeoutForPodRunningInNamespace(ctx context.Context, c clientset.Interface, podName, namespace string, timeout time.Duration) error {
	return framework.Gomega().Eventually(ctx, framework.RetryNotFound(framework.GetObject(c.CoreV1().Pods(namespace).Get, podName, metav1.GetOptions{}))).
		WithTimeout(timeout).
		Should(BeRunningNoRetries())
}

// WaitForPodRunningInNamespace waits default amount of time (podStartTimeout) for the specified pod to become running.
// Returns an error if timeout occurs first, or pod goes in to failed state.
func WaitForPodRunningInNamespace(ctx context.Context, c clientset.Interface, pod *v1.Pod) error {
	if pod.Status.Phase == v1.PodRunning {
		return nil
	}
	return WaitTimeoutForPodRunningInNamespace(ctx, c, pod.Name, pod.Namespace, podStartTimeout)
}

// WaitTimeoutForPodNoLongerRunningInNamespace waits the given timeout duration for the specified pod to stop.
func WaitTimeoutForPodNoLongerRunningInNamespace(ctx context.Context, c clientset.Interface, podName, namespace string, timeout time.Duration) error {
	return WaitForPodCondition(ctx, c, namespace, podName, "completed", timeout, func(pod *v1.Pod) (bool, error) {
		switch pod.Status.Phase {
		case v1.PodFailed, v1.PodSucceeded:
			return true, nil
		}
		return false, nil
	})
}

// WaitForPodNoLongerRunningInNamespace waits default amount of time (defaultPodDeletionTimeout) for the specified pod to stop running.
// Returns an error if timeout occurs first.
func WaitForPodNoLongerRunningInNamespace(ctx context.Context, c clientset.Interface, podName, namespace string) error {
	return WaitTimeoutForPodNoLongerRunningInNamespace(ctx, c, podName, namespace, defaultPodDeletionTimeout)
}

// WaitTimeoutForPodReadyInNamespace waits the given timeout duration for the
// specified pod to be ready and running.
func WaitTimeoutForPodReadyInNamespace(ctx context.Context, c clientset.Interface, podName, namespace string, timeout time.Duration) error {
	return WaitForPodCondition(ctx, c, namespace, podName, "running and ready", timeout, func(pod *v1.Pod) (bool, error) {
		switch pod.Status.Phase {
		case v1.PodFailed, v1.PodSucceeded:
			return false, gomega.StopTrying(fmt.Sprintf("The phase of Pod %s is %s which is unexpected.", pod.Name, pod.Status.Phase))
		case v1.PodRunning:
			return podutils.IsPodReady(pod), nil
		}
		return false, nil
	})
}

// WaitForPodNotPending returns an error if it took too long for the pod to go out of pending state.
// The resourceVersion is used when Watching object changes, it tells since when we care
// about changes to the pod.
func WaitForPodNotPending(ctx context.Context, c clientset.Interface, ns, podName string) error {
	return WaitForPodCondition(ctx, c, ns, podName, "not pending", podStartTimeout, func(pod *v1.Pod) (bool, error) {
		switch pod.Status.Phase {
		case v1.PodPending:
			return false, nil
		default:
			return true, nil
		}
	})
}

// WaitForPodSuccessInNamespace returns nil if the pod reached state success, or an error if it reached failure or until podStartupTimeout.
func WaitForPodSuccessInNamespace(ctx context.Context, c clientset.Interface, podName string, namespace string) error {
	return WaitForPodSuccessInNamespaceTimeout(ctx, c, podName, namespace, podStartTimeout)
}

// WaitForPodNotFoundInNamespace returns an error if it takes too long for the pod to fully terminate.
// Unlike `waitForPodTerminatedInNamespace`, the pod's Phase and Reason are ignored. If the pod Get
// api returns IsNotFound then the wait stops and nil is returned. If the Get api returns an error other
// than "not found" and that error is final, that error is returned and the wait stops.
func WaitForPodNotFoundInNamespace(ctx context.Context, c clientset.Interface, podName, ns string, timeout time.Duration) error {
	err := framework.Gomega().Eventually(ctx, framework.HandleRetry(func(ctx context.Context) (*v1.Pod, error) {
		pod, err := c.CoreV1().Pods(ns).Get(ctx, podName, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return pod, err
	})).WithTimeout(timeout).Should(gomega.BeNil())
	if err != nil {
		return fmt.Errorf("expected pod to not be found: %w", err)
	}
	return nil
}

// WaitForPodsResponding waits for the pods to response.
func WaitForPodsResponding(ctx context.Context, c clientset.Interface, ns string, controllerName string, wantName bool, timeout time.Duration, pods *v1.PodList) error {
	if timeout == 0 {
		timeout = podRespondingTimeout
	}
	ginkgo.By("trying to dial each unique pod")
	label := labels.SelectorFromSet(labels.Set(map[string]string{"name": controllerName}))
	options := metav1.ListOptions{LabelSelector: label.String()}

	type response struct {
		podName  string
		response string
	}

	get := func(ctx context.Context) ([]response, error) {
		currentPods, err := c.CoreV1().Pods(ns).List(ctx, options)
		if err != nil {
			return nil, fmt.Errorf("list pods: %w", err)
		}

		var responses []response
		for _, pod := range pods.Items {
			// Check that the replica list remains unchanged, otherwise we have problems.
			if !isElementOf(pod.UID, currentPods) {
				return nil, gomega.StopTrying(fmt.Sprintf("Pod with UID %s is no longer a member of the replica set. Must have been restarted for some reason.\nCurrent replica set:\n%s", pod.UID, format.Object(currentPods, 1)))
			}

			ctxUntil, cancel := context.WithTimeout(ctx, singleCallTimeout)
			defer cancel()

			body, err := c.CoreV1().RESTClient().Get().
				Namespace(ns).
				Resource("pods").
				SubResource("proxy").
				Name(string(pod.Name)).
				Do(ctxUntil).
				Raw()

			if err != nil {
				// We may encounter errors here because of a race between the pod readiness and apiserver
				// proxy or because of temporary failures. The error gets wrapped for framework.HandleRetry.
				// Gomega+Ginkgo will handle logging.
				return nil, fmt.Errorf("controller %s: failed to Get from replica pod %s:\n%w\nPod status:\n%s",
					controllerName, pod.Name,
					err, format.Object(pod.Status, 1))
			}
			responses = append(responses, response{podName: pod.Name, response: string(body)})
		}
		return responses, nil
	}

	match := func(responses []response) (func() string, error) {
		// The response checker expects the pod's name unless !respondName, in
		// which case it just checks for a non-empty response.
		var unexpected []response
		for _, response := range responses {
			if wantName {
				if response.response != response.podName {
					unexpected = append(unexpected, response)
				}
			} else {
				if len(response.response) == 0 {
					unexpected = append(unexpected, response)
				}
			}
		}
		if len(unexpected) > 0 {
			return func() string {
				what := "some response"
				if wantName {
					what = "the pod's own name as response"
				}
				return fmt.Sprintf("Wanted %s, but the following pods replied with something else:\n%s", what, format.Object(unexpected, 1))
			}, nil
		}
		return nil, nil
	}

	err := framework.Gomega().
		Eventually(ctx, framework.HandleRetry(get)).
		WithTimeout(timeout).
		Should(framework.MakeMatcher(match))
	if err != nil {
		return fmt.Errorf("checking pod responses: %w", err)
	}
	return nil
}

func isElementOf(podUID apitypes.UID, pods *v1.PodList) bool {
	for _, pod := range pods.Items {
		if pod.UID == podUID {
			return true
		}
	}
	return false
}

// WaitForNumberOfPods waits up to timeout to ensure there are exact
// `num` pods in namespace `ns`.
// It returns the matching Pods or a timeout error.
func WaitForNumberOfPods(ctx context.Context, c clientset.Interface, ns string, num int, timeout time.Duration) (pods *v1.PodList, err error) {
	return WaitForPods(ctx, c, ns, metav1.ListOptions{}, Range{MinMatching: num, MaxMatching: num}, podScheduledBeforeTimeout, "exist", func(pod *v1.Pod) bool {
		return true
	})
}

// WaitForPodsWithLabelScheduled waits for all matching pods to become scheduled and at least one
// matching pod exists.  Return the list of matching pods.
func WaitForPodsWithLabelScheduled(ctx context.Context, c clientset.Interface, ns string, label labels.Selector) (pods *v1.PodList, err error) {
	opts := metav1.ListOptions{LabelSelector: label.String()}
	return WaitForPods(ctx, c, ns, opts, Range{MinFound: 1, AllMatching: true}, podScheduledBeforeTimeout, "be scheduled", func(pod *v1.Pod) bool {
		return pod.Spec.NodeName != ""
	})
}

// WaitForPodsWithLabel waits up to podListTimeout for getting pods with certain label
func WaitForPodsWithLabel(ctx context.Context, c clientset.Interface, ns string, label labels.Selector) (*v1.PodList, error) {
	opts := metav1.ListOptions{LabelSelector: label.String()}
	return WaitForPods(ctx, c, ns, opts, Range{MinFound: 1}, podListTimeout, "exist", func(pod *v1.Pod) bool {
		return true
	})
}

// WaitForPodsWithLabelRunningReady waits for exact amount of matching pods to become running and ready.
// Return the list of matching pods.
func WaitForPodsWithLabelRunningReady(ctx context.Context, c clientset.Interface, ns string, label labels.Selector, num int, timeout time.Duration) (pods *v1.PodList, err error) {
	opts := metav1.ListOptions{LabelSelector: label.String()}
	return WaitForPods(ctx, c, ns, opts, Range{MinFound: num, AllMatching: true}, timeout, "be running and ready", RunningReady)
}

// WaitForNRestartablePods tries to list restarting pods using ps until it finds expect of them,
// returning their names if it can do so before timeout.
func WaitForNRestartablePods(ctx context.Context, ps *testutils.PodStore, expect int, timeout time.Duration) ([]string, error) {
	var pods []*v1.Pod

	get := func(ctx context.Context) ([]*v1.Pod, error) {
		return ps.List(), nil
	}

	match := func(allPods []*v1.Pod) (func() string, error) {
		pods = FilterNonRecreatablePods(allPods)
		if len(pods) != expect {
			return func() string {
				return fmt.Sprintf("expected to find non-restartable %d pods, but found %d:\n%s", expect, len(pods), format.Object(pods, 1))
			}, nil
		}
		return nil, nil
	}

	err := framework.Gomega().
		Eventually(ctx, framework.HandleRetry(get)).
		WithTimeout(timeout).
		Should(framework.MakeMatcher(match))
	if err != nil {
		return nil, err
	}

	podNames := make([]string, len(pods))
	for i, p := range pods {
		podNames[i] = p.Name
	}
	return podNames, nil
}

// WaitForPodContainerToFail waits for the given Pod container to fail with the given reason, specifically due to
// invalid container configuration. In this case, the container will remain in a waiting state with a specific
// reason set, which should match the given reason.
func WaitForPodContainerToFail(ctx context.Context, c clientset.Interface, namespace, podName string, containerIndex int, reason string, timeout time.Duration) error {
	conditionDesc := fmt.Sprintf("container %d failed with reason %s", containerIndex, reason)
	return WaitForPodCondition(ctx, c, namespace, podName, conditionDesc, timeout, func(pod *v1.Pod) (bool, error) {
		switch pod.Status.Phase {
		case v1.PodPending:
			if len(pod.Status.ContainerStatuses) == 0 {
				return false, nil
			}
			containerStatus := pod.Status.ContainerStatuses[containerIndex]
			if containerStatus.State.Waiting != nil && containerStatus.State.Waiting.Reason == reason {
				return true, nil
			}
			return false, nil
		case v1.PodFailed, v1.PodRunning, v1.PodSucceeded:
			return false, fmt.Errorf("pod was expected to be pending, but it is in the state: %s", pod.Status.Phase)
		}
		return false, nil
	})
}

// WaitForPodScheduled waits for the pod to be schedule, ie. the .spec.nodeName is set
func WaitForPodScheduled(ctx context.Context, c clientset.Interface, namespace, podName string) error {
	return WaitForPodCondition(ctx, c, namespace, podName, "pod is scheduled", podScheduledBeforeTimeout, func(pod *v1.Pod) (bool, error) {
		return pod.Spec.NodeName != "", nil
	})
}

// WaitForPodContainerStarted waits for the given Pod container to start, after a successful run of the startupProbe.
func WaitForPodContainerStarted(ctx context.Context, c clientset.Interface, namespace, podName string, containerIndex int, timeout time.Duration) error {
	conditionDesc := fmt.Sprintf("container %d started", containerIndex)
	return WaitForPodCondition(ctx, c, namespace, podName, conditionDesc, timeout, func(pod *v1.Pod) (bool, error) {
		if containerIndex >= len(pod.Status.ContainerStatuses) {
			return false, nil
		}
		containerStatus := pod.Status.ContainerStatuses[containerIndex]
		return *containerStatus.Started, nil
	})
}

// WaitForPodInitContainerStarted waits for the given Pod init container to start.
func WaitForPodInitContainerStarted(ctx context.Context, c clientset.Interface, namespace, podName string, initContainerIndex int, timeout time.Duration) error {
	conditionDesc := fmt.Sprintf("init container %d started", initContainerIndex)
	return WaitForPodCondition(ctx, c, namespace, podName, conditionDesc, timeout, func(pod *v1.Pod) (bool, error) {
		if initContainerIndex >= len(pod.Status.InitContainerStatuses) {
			return false, nil
		}
		initContainerStatus := pod.Status.InitContainerStatuses[initContainerIndex]
		return *initContainerStatus.Started, nil
	})
}

// WaitForPodFailedReason wait for pod failed reason in status, for example "SysctlForbidden".
func WaitForPodFailedReason(ctx context.Context, c clientset.Interface, pod *v1.Pod, reason string, timeout time.Duration) error {
	conditionDesc := fmt.Sprintf("failed with reason %s", reason)
	return WaitForPodCondition(ctx, c, pod.Namespace, pod.Name, conditionDesc, timeout, func(pod *v1.Pod) (bool, error) {
		switch pod.Status.Phase {
		case v1.PodSucceeded:
			return true, errors.New("pod succeeded unexpectedly")
		case v1.PodFailed:
			if pod.Status.Reason == reason {
				return true, nil
			} else {
				return true, fmt.Errorf("pod failed with reason %s", pod.Status.Reason)
			}
		}
		return false, nil
	})
}

// WaitForContainerRunning waits for the given Pod container to have a state of running
func WaitForContainerRunning(ctx context.Context, c clientset.Interface, namespace, podName, containerName string, timeout time.Duration) error {
	conditionDesc := fmt.Sprintf("container %s running", containerName)
	return WaitForPodCondition(ctx, c, namespace, podName, conditionDesc, timeout, func(pod *v1.Pod) (bool, error) {
		for _, statuses := range [][]v1.ContainerStatus{pod.Status.ContainerStatuses, pod.Status.InitContainerStatuses, pod.Status.EphemeralContainerStatuses} {
			for _, cs := range statuses {
				if cs.Name == containerName {
					return cs.State.Running != nil, nil
				}
			}
		}
		return false, nil
	})
}

// WaitForContainerTerminated waits for the given Pod container to have a state of terminated
func WaitForContainerTerminated(ctx context.Context, c clientset.Interface, namespace, podName, containerName string, timeout time.Duration) error {
	conditionDesc := fmt.Sprintf("container %s terminated", containerName)
	return WaitForPodCondition(ctx, c, namespace, podName, conditionDesc, timeout, func(pod *v1.Pod) (bool, error) {
		for _, statuses := range [][]v1.ContainerStatus{pod.Status.ContainerStatuses, pod.Status.InitContainerStatuses, pod.Status.EphemeralContainerStatuses} {
			for _, cs := range statuses {
				if cs.Name == containerName {
					return cs.State.Terminated != nil, nil
				}
			}
		}
		return false, nil
	})
}
