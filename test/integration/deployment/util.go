/*
Copyright 2017 The Kubernetes Authors.

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

package deployment

import (
	"context"
	"fmt"
	"math"
	"sync"
	"testing"
	"time"

	apps "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/informers"
	clientset "k8s.io/client-go/kubernetes"
	restclient "k8s.io/client-go/rest"
	kubeapiservertesting "k8s.io/kubernetes/cmd/kube-apiserver/app/testing"
	podutil "k8s.io/kubernetes/pkg/api/v1/pod"
	"k8s.io/kubernetes/pkg/controller/deployment"
	deploymentutil "k8s.io/kubernetes/pkg/controller/deployment/util"
	"k8s.io/kubernetes/pkg/controller/replicaset"
	"k8s.io/kubernetes/test/integration/framework"
	testutil "k8s.io/kubernetes/test/utils"
	"k8s.io/utils/ptr"
)

const (
	pollInterval = 100 * time.Millisecond
	pollTimeout  = 60 * time.Second

	fakeContainerName = "fake-name"
	fakeImage         = "fakeimage"
)

var pauseFn = func(update *apps.Deployment) {
	update.Spec.Paused = true
}

var resumeFn = func(update *apps.Deployment) {
	update.Spec.Paused = false
}

type deploymentTester struct {
	t          *testing.T
	c          clientset.Interface
	deployment *apps.Deployment
}

func testLabels() map[string]string {
	return map[string]string{"name": "test"}
}

// newDeployment returns a RollingUpdate Deployment with a fake container image
func newDeployment(name, ns string, replicas int32) *apps.Deployment {
	return &apps.Deployment{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Deployment",
			APIVersion: "apps/v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns,
			Name:      name,
		},
		Spec: apps.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: testLabels()},
			Strategy: apps.DeploymentStrategy{
				Type:          apps.RollingUpdateDeploymentStrategyType,
				RollingUpdate: new(apps.RollingUpdateDeployment),
			},
			Template: v1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: testLabels(),
				},
				Spec: v1.PodSpec{
					Containers: []v1.Container{
						{
							Name:  fakeContainerName,
							Image: fakeImage,
						},
					},
				},
			},
		},
	}
}

// dcSetup sets up necessities for Deployment integration test, including control plane, apiserver, informers, and clientset
func dcSetup(ctx context.Context, t *testing.T) (kubeapiservertesting.TearDownFunc, *replicaset.ReplicaSetController, *deployment.DeploymentController, informers.SharedInformerFactory, clientset.Interface) {
	// Disable ServiceAccount admission plugin as we don't have serviceaccount controller running.
	server := kubeapiservertesting.StartTestServerOrDie(t, nil, framework.DefaultTestServerFlags(), framework.SharedEtcd())

	config := restclient.CopyConfig(server.ClientConfig)
	clientSet, err := clientset.NewForConfig(config)
	if err != nil {
		t.Fatalf("error in create clientset: %v", err)
	}
	resyncPeriod := 12 * time.Hour
	informers := informers.NewSharedInformerFactory(clientset.NewForConfigOrDie(restclient.AddUserAgent(config, "deployment-informers")), resyncPeriod)

	dc, err := deployment.NewDeploymentController(
		ctx,
		informers.Apps().V1().Deployments(),
		informers.Apps().V1().ReplicaSets(),
		informers.Core().V1().Pods(),
		clientset.NewForConfigOrDie(restclient.AddUserAgent(config, "deployment-controller")),
	)
	if err != nil {
		t.Fatalf("error creating Deployment controller: %v", err)
	}
	rm := replicaset.NewReplicaSetController(
		ctx,
		informers.Apps().V1().ReplicaSets(),
		informers.Core().V1().Pods(),
		clientset.NewForConfigOrDie(restclient.AddUserAgent(config, "replicaset-controller")),
		replicaset.BurstReplicas,
	)
	return server.TearDownFn, rm, dc, informers, clientSet
}

// dcSimpleSetup sets up necessities for Deployment integration test, including control plane, apiserver,
// and clientset, but not controllers and informers
func dcSimpleSetup(t *testing.T) (kubeapiservertesting.TearDownFunc, clientset.Interface) {
	// Disable ServiceAccount admission plugin as we don't have serviceaccount controller running.
	server := kubeapiservertesting.StartTestServerOrDie(t, nil, framework.DefaultTestServerFlags(), framework.SharedEtcd())

	config := restclient.CopyConfig(server.ClientConfig)
	clientSet, err := clientset.NewForConfig(config)
	if err != nil {
		t.Fatalf("error in create clientset: %v", err)
	}
	return server.TearDownFn, clientSet
}

// runControllersAndInformers runs RS and deployment controllers and informers
func runControllersAndInformers(t *testing.T, rm *replicaset.ReplicaSetController, dc *deployment.DeploymentController, informers informers.SharedInformerFactory) func() {
	ctx, cancelFn := context.WithCancel(context.Background())
	informers.Start(ctx.Done())
	go rm.Run(ctx, 5)
	go dc.Run(ctx, 5)
	return cancelFn
}

// addPodConditionReady sets given pod status to ready at given time
func addPodConditionReady(pod *v1.Pod, time metav1.Time) {
	pod.Status = v1.PodStatus{
		Phase: v1.PodRunning,
		Conditions: []v1.PodCondition{
			{
				Type:               v1.PodReady,
				Status:             v1.ConditionTrue,
				LastTransitionTime: time,
			},
		},
	}
}

func (d *deploymentTester) waitForDeploymentRevisionAndImage(revision, image string) error {
	if err := testutil.WaitForDeploymentRevisionAndImage(d.c, d.deployment.Namespace, d.deployment.Name, revision, image, d.t.Logf, pollInterval, pollTimeout); err != nil {
		return fmt.Errorf("failed to wait for Deployment revision %s: %v", d.deployment.Name, err)
	}
	return nil
}

func markPodReady(c clientset.Interface, ns string, pod *v1.Pod) error {
	addPodConditionReady(pod, metav1.Now())
	_, err := c.CoreV1().Pods(ns).UpdateStatus(context.TODO(), pod, metav1.UpdateOptions{})
	return err
}

// markUpdatedPodsReady manually marks updated Deployment pods status to ready,
// until the deployment is complete
func (d *deploymentTester) markUpdatedPodsReady(wg *sync.WaitGroup) {
	defer wg.Done()

	ns := d.deployment.Namespace
	err := wait.PollImmediate(pollInterval, pollTimeout, func() (bool, error) {
		// We're done when the deployment is complete
		if completed, err := d.deploymentComplete(); err != nil {
			return false, err
		} else if completed {
			return true, nil
		}
		// Otherwise, mark remaining pods as ready
		pods, err := d.listUpdatedPods()
		if err != nil {
			d.t.Log(err)
			return false, nil
		}
		d.t.Logf("%d/%d of deployment pods are created", len(pods), *d.deployment.Spec.Replicas)
		for i := range pods {
			pod := pods[i]
			if podutil.IsPodReady(&pod) {
				continue
			}
			if err = markPodReady(d.c, ns, &pod); err != nil {
				d.t.Logf("failed to update Deployment pod %s, will retry later: %v", pod.Name, err)
			}
		}
		return false, nil
	})
	if err != nil {
		d.t.Errorf("failed to mark updated Deployment pods to ready: %v", err)
	}
}

// removeTerminatedPods manually removes terminated Deployment pods,
// until the deployment is complete
func (d *deploymentTester) removeTerminatedPods(ctx context.Context, wg *sync.WaitGroup) {
	defer wg.Done()

	err := wait.PollUntilContextTimeout(ctx, pollInterval, pollTimeout, true, func(ctx context.Context) (bool, error) {
		// We're done when the deployment is complete
		if completed, err := d.deploymentComplete(); err != nil {
			return false, err
		} else if completed {
			return true, nil
		}
		// Otherwise, mark remaining pods as ready
		rsList, err := deploymentutil.ListReplicaSets(d.deployment, deploymentutil.RsListFromClient(d.c.AppsV1()))
		if err != nil {
			d.t.Log(err)
			return false, nil
		}
		for _, rs := range rsList {
			if err := d.removeRSPods(ctx, rs, math.MaxInt, true, 0); err != nil {
				d.t.Log(err)
				return false, nil
			}
		}
		return false, nil
	})
	if err != nil {
		d.t.Errorf("failed to remove terminated Deployment pods: %v", err)
	}
}

func (d *deploymentTester) deploymentComplete() (bool, error) {
	latest, err := d.c.AppsV1().Deployments(d.deployment.Namespace).Get(context.TODO(), d.deployment.Name, metav1.GetOptions{})
	if err != nil {
		return false, err
	}
	return deploymentutil.DeploymentComplete(d.deployment, &latest.Status), nil
}

// Waits for the deployment to complete, and check rolling update strategy isn't broken at any times.
// Rolling update strategy should not be broken during a rolling update.
func (d *deploymentTester) waitForDeploymentCompleteAndCheckRolling() error {
	return testutil.WaitForDeploymentCompleteAndCheckRolling(d.c, d.deployment, d.t.Logf, pollInterval, pollTimeout)
}

// Waits for the deployment to complete, and don't check if rolling update strategy is broken.
// Rolling update strategy is used only during a rolling update, and can be violated in other situations,
// such as shortly after a scaling event or the deployment is just created.
func (d *deploymentTester) waitForDeploymentComplete() error {
	return testutil.WaitForDeploymentComplete(d.c, d.deployment, d.t.Logf, pollInterval, pollTimeout)
}

// waitForDeploymentCompleteAndCheckRollingAndMarkPodsReady waits for the Deployment to complete
// while marking updated Deployment pods as ready at the same time.
// Uses hard check to make sure rolling update strategy is not violated at any times.
func (d *deploymentTester) waitForDeploymentCompleteAndCheckRollingAndMarkPodsReady() error {
	var wg sync.WaitGroup

	// Manually mark updated Deployment pods as ready in a separate goroutine
	wg.Add(1)
	go d.markUpdatedPodsReady(&wg)
	// Wait for goroutine to finish, for all return paths.
	defer wg.Wait()

	// Wait for the Deployment status to complete while Deployment pods are becoming ready
	err := d.waitForDeploymentCompleteAndCheckRolling()
	if err != nil {
		return fmt.Errorf("failed to wait for Deployment %s to complete: %v", d.deployment.Name, err)
	}

	return nil
}

// waitForDeploymentCompleteAndMarkPodsReady waits for the Deployment to complete
// while marking updated Deployment pods as ready at the same time.
func (d *deploymentTester) waitForDeploymentCompleteAndMarkPodsReady() error {
	var wg sync.WaitGroup

	// Manually mark updated Deployment pods as ready in a separate goroutine
	wg.Add(1)
	go d.markUpdatedPodsReady(&wg)

	// Wait for the Deployment status to complete using soft check, while Deployment pods are becoming ready
	err := d.waitForDeploymentComplete()
	if err != nil {
		return fmt.Errorf("failed to wait for Deployment status %s: %v", d.deployment.Name, err)
	}

	// Wait for goroutine to finish
	wg.Wait()

	return nil
}

// waitForDeploymentCompleteAndMarkPodsReadyAndRemoveTerminating waits for the Deployment to complete
// while marking updated Deployment pods as ready and removes terminated pods at the same time.
func (d *deploymentTester) waitForDeploymentCompleteAndMarkPodsReadyAndRemoveTerminated(ctx context.Context) error {
	var wg sync.WaitGroup

	// Manually mark updated Deployment pods as ready in a separate goroutine
	wg.Add(1)
	go d.markUpdatedPodsReady(&wg)

	wg.Add(1)
	go d.removeTerminatedPods(ctx, &wg)

	// Wait for the Deployment status to complete using soft check, while Deployment pods are becoming ready
	err := d.waitForDeploymentComplete()
	if err != nil {
		return fmt.Errorf("failed to wait for Deployment status %s: %w", d.deployment.Name, err)
	}

	// Wait for goroutine to finish
	wg.Wait()

	return nil
}

func (d *deploymentTester) updateDeployment(applyUpdate testutil.UpdateDeploymentFunc) (*apps.Deployment, error) {
	return testutil.UpdateDeploymentWithRetries(d.c, d.deployment.Namespace, d.deployment.Name, applyUpdate, d.t.Logf, pollInterval, pollTimeout)
}

func (d *deploymentTester) waitForObservedDeployment(desiredGeneration int64) error {
	if err := testutil.WaitForObservedDeployment(d.c, d.deployment.Namespace, d.deployment.Name, desiredGeneration); err != nil {
		return fmt.Errorf("failed waiting for ObservedGeneration of deployment %s to become %d: %v", d.deployment.Name, desiredGeneration, err)
	}
	return nil
}

func (d *deploymentTester) getNewReplicaSet() (*apps.ReplicaSet, error) {
	deployment, err := d.c.AppsV1().Deployments(d.deployment.Namespace).Get(context.TODO(), d.deployment.Name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed retrieving deployment %s: %v", d.deployment.Name, err)
	}
	rs, err := testutil.GetNewReplicaSet(deployment, d.c)
	if err != nil {
		return nil, fmt.Errorf("failed retrieving new replicaset of deployment %s: %v", d.deployment.Name, err)
	}
	return rs, nil
}

func (d *deploymentTester) expectNoNewReplicaSet() error {
	rs, err := d.getNewReplicaSet()
	if err != nil {
		return err
	}
	if rs != nil {
		return fmt.Errorf("expected deployment %s not to create a new replicaset, got %v", d.deployment.Name, rs)
	}
	return nil
}

func (d *deploymentTester) expectNewReplicaSet() (*apps.ReplicaSet, error) {
	rs, err := d.getNewReplicaSet()
	if err != nil {
		return nil, err
	}
	if rs == nil {
		return nil, fmt.Errorf("expected deployment %s to create a new replicaset, got nil", d.deployment.Name)
	}
	return rs, nil
}

func (d *deploymentTester) updateReplicaSet(name string, applyUpdate testutil.UpdateReplicaSetFunc) (*apps.ReplicaSet, error) {
	return testutil.UpdateReplicaSetWithRetries(d.c, d.deployment.Namespace, name, applyUpdate, d.t.Logf, pollInterval, pollTimeout)
}

func (d *deploymentTester) waitForDeploymentUpdatedReplicasGTE(minUpdatedReplicas int32) error {
	return testutil.WaitForDeploymentUpdatedReplicasGTE(d.c, d.deployment.Namespace, d.deployment.Name, minUpdatedReplicas, d.deployment.Generation, pollInterval, pollTimeout)
}

func (d *deploymentTester) waitForDeploymentWithCondition(reason string, condType apps.DeploymentConditionType) error {
	return testutil.WaitForDeploymentWithCondition(d.c, d.deployment.Namespace, d.deployment.Name, reason, condType, d.t.Logf, pollInterval, pollTimeout)
}

func (d *deploymentTester) listUpdatedPods() ([]v1.Pod, error) {
	selector, err := metav1.LabelSelectorAsSelector(d.deployment.Spec.Selector)
	if err != nil {
		return nil, fmt.Errorf("failed to parse deployment selector: %v", err)
	}
	pods, err := d.c.CoreV1().Pods(d.deployment.Namespace).List(context.TODO(), metav1.ListOptions{LabelSelector: selector.String()})
	if err != nil {
		return nil, fmt.Errorf("failed to list deployment pods, will retry later: %v", err)
	}
	newRS, err := d.getNewReplicaSet()
	if err != nil {
		return nil, fmt.Errorf("failed to get new replicaset of deployment %q: %v", d.deployment.Name, err)
	}
	if newRS == nil {
		return nil, fmt.Errorf("unable to find new replicaset of deployment %q", d.deployment.Name)
	}

	var ownedPods []v1.Pod
	for _, pod := range pods.Items {
		rs := metav1.GetControllerOf(&pod)
		if rs.UID == newRS.UID {
			ownedPods = append(ownedPods, pod)
		}
	}
	return ownedPods, nil
}

func (d *deploymentTester) waitRSStable(replicaset *apps.ReplicaSet) error {
	return testutil.WaitRSStable(d.t, d.c, replicaset, pollInterval, pollTimeout)
}

func (d *deploymentTester) scaleDeployment(newReplicas int32) error {
	var err error
	d.deployment, err = d.updateDeployment(func(update *apps.Deployment) {
		update.Spec.Replicas = &newReplicas
	})
	if err != nil {
		return fmt.Errorf("failed updating deployment %q: %v", d.deployment.Name, err)
	}

	if err := d.waitForDeploymentCompleteAndMarkPodsReady(); err != nil {
		return err
	}

	rs, err := d.expectNewReplicaSet()
	if err != nil {
		return err
	}
	if *rs.Spec.Replicas != newReplicas {
		return fmt.Errorf("expected new replicaset replicas = %d, got %d", newReplicas, *rs.Spec.Replicas)
	}
	return nil
}

// waitForReadyReplicas waits for number of ready replicas to equal number of replicas.
func (d *deploymentTester) waitForReadyReplicas() error {
	if err := wait.PollImmediate(pollInterval, pollTimeout, func() (bool, error) {
		deployment, err := d.c.AppsV1().Deployments(d.deployment.Namespace).Get(context.TODO(), d.deployment.Name, metav1.GetOptions{})
		if err != nil {
			return false, fmt.Errorf("failed to get deployment %q: %v", d.deployment.Name, err)
		}
		return deployment.Status.ReadyReplicas == *deployment.Spec.Replicas, nil
	}); err != nil {
		return fmt.Errorf("failed to wait for .readyReplicas to equal .replicas: %v", err)
	}
	return nil
}

// markUpdatedPodsReadyWithoutComplete marks updated Deployment pods as ready without waiting for deployment to complete.
func (d *deploymentTester) markUpdatedPodsReadyWithoutComplete() error {
	if err := wait.PollImmediate(pollInterval, pollTimeout, func() (bool, error) {
		pods, err := d.listUpdatedPods()
		if err != nil {
			return false, err
		}
		for i := range pods {
			pod := pods[i]
			if podutil.IsPodReady(&pod) {
				continue
			}
			if err = markPodReady(d.c, d.deployment.Namespace, &pod); err != nil {
				d.t.Logf("failed to update Deployment pod %q, will retry later: %v", pod.Name, err)
				return false, nil
			}
		}
		return true, nil
	}); err != nil {
		return fmt.Errorf("failed to mark all updated pods as ready: %v", err)
	}
	return nil
}

// waitForReadyReplicas waits for number of ready replicas to equal number of replicas.
func (d *deploymentTester) waitForDeploymentStatusReplicasFields(ctx context.Context, replicas, updatedReplicas, readyReplicas, availableReplicas, unavailableReplicas int32, terminatingReplicas *int32) error {
	var lastErr error
	err := wait.PollUntilContextTimeout(ctx, pollInterval, pollTimeout, true, func(_ context.Context) (bool, error) {
		// do not pass ctx to checkDeploymentStatusReplicasFields (Deployments().Get) so we always obtain the latest error and not the one from deadline exceeded
		lastErr = d.checkDeploymentStatusReplicasFields(replicas, updatedReplicas, readyReplicas, availableReplicas, unavailableReplicas, terminatingReplicas)
		return lastErr == nil, nil
	})
	if err != nil {
		return fmt.Errorf("%w: %w", lastErr, err)
	}
	return nil
}

// Verify all replicas fields of DeploymentStatus have desired count.
// Immediately return an error when found a non-matching replicas field.
func (d *deploymentTester) checkDeploymentStatusReplicasFields(replicas, updatedReplicas, readyReplicas, availableReplicas, unavailableReplicas int32, terminatingReplicas *int32) error {
	deployment, err := d.c.AppsV1().Deployments(d.deployment.Namespace).Get(context.TODO(), d.deployment.Name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get deployment %q: %v", d.deployment.Name, err)
	}
	if deployment.Status.Replicas != replicas {
		return fmt.Errorf("unexpected .replicas: expect %d, got %d", replicas, deployment.Status.Replicas)
	}
	if deployment.Status.UpdatedReplicas != updatedReplicas {
		return fmt.Errorf("unexpected .updatedReplicas: expect %d, got %d", updatedReplicas, deployment.Status.UpdatedReplicas)
	}
	if deployment.Status.ReadyReplicas != readyReplicas {
		return fmt.Errorf("unexpected .readyReplicas: expect %d, got %d", readyReplicas, deployment.Status.ReadyReplicas)
	}
	if deployment.Status.AvailableReplicas != availableReplicas {
		return fmt.Errorf("unexpected .availableReplicas: expect %d, got %d", availableReplicas, deployment.Status.AvailableReplicas)
	}
	if deployment.Status.UnavailableReplicas != unavailableReplicas {
		return fmt.Errorf("unexpected .unavailableReplicas: expect %d, got %d", unavailableReplicas, deployment.Status.UnavailableReplicas)
	}
	if !ptr.Equal(deployment.Status.TerminatingReplicas, terminatingReplicas) {
		return fmt.Errorf("unexpected .terminatingReplicas: expect %v, got %v", ptr.Deref(terminatingReplicas, -1), ptr.Deref(deployment.Status.TerminatingReplicas, -1))

	}
	return nil
}

func (d *deploymentTester) removeRSPods(ctx context.Context, replicaset *apps.ReplicaSet, count int, targetOnlyTerminating bool, gracePeriodSeconds int64) error {
	selector, err := metav1.LabelSelectorAsSelector(replicaset.Spec.Selector)
	if err != nil {
		return fmt.Errorf("could not parse a selector for a replica set %q: %w", replicaset.Name, err)
	}
	pods, err := d.c.CoreV1().Pods(replicaset.Namespace).List(ctx, metav1.ListOptions{LabelSelector: selector.String()})
	if err != nil {
		return fmt.Errorf("failed to get pods for a replica set %q: %w", replicaset.Name, err)
	}

	for i, pod := range pods.Items {
		if i >= count {
			break
		}
		if targetOnlyTerminating && pod.DeletionTimestamp == nil {
			continue
		}
		err := d.c.CoreV1().Pods(replicaset.Namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{GracePeriodSeconds: ptr.To(gracePeriodSeconds)})
		if err != nil {
			return fmt.Errorf("failed to delete pod %q for a replica set %q: %w", pod.Name, replicaset.Name, err)
		}
	}

	return nil
}
