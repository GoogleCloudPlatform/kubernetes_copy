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
	"reflect"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	apps "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/apimachinery/pkg/util/wait"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/client-go/util/retry"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	"k8s.io/klog/v2/ktesting"
	deploymentutil "k8s.io/kubernetes/pkg/controller/deployment/util"
	"k8s.io/kubernetes/pkg/features"
	"k8s.io/kubernetes/test/integration/framework"
	testutil "k8s.io/kubernetes/test/utils"
	"k8s.io/utils/ptr"
)

func TestNewDeployment(t *testing.T) {
	_, ctx := ktesting.NewTestContext(t)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	closeFn, rm, dc, informers, c := dcSetup(ctx, t)
	defer closeFn()
	name := "test-new-deployment"

	ns := framework.CreateNamespaceOrDie(c, name, t)
	defer framework.DeleteNamespaceOrDie(c, ns, t)

	replicas := int32(20)
	tester := &deploymentTester{t: t, c: c, deployment: newDeployment(name, ns.Name, replicas)}
	tester.deployment.Spec.MinReadySeconds = 4

	tester.deployment.Annotations = map[string]string{"test": "should-copy-to-replica-set", v1.LastAppliedConfigAnnotation: "should-not-copy-to-replica-set"}
	var err error
	tester.deployment, err = c.AppsV1().Deployments(ns.Name).Create(context.TODO(), tester.deployment, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("failed to create deployment %s: %v", tester.deployment.Name, err)
	}

	// Start informer and controllers
	stopControllers := runControllersAndInformers(t, rm, dc, informers)
	defer stopControllers()

	// Wait for the Deployment to be updated to revision 1
	if err := tester.waitForDeploymentRevisionAndImage("1", fakeImage); err != nil {
		t.Fatal(err)
	}

	// Make sure the Deployment completes while manually marking Deployment pods as ready at the same time.
	// Use soft check because this deployment was just created and rolling update strategy might be violated.
	if err := tester.waitForDeploymentCompleteAndMarkPodsReady(); err != nil {
		t.Fatal(err)
	}

	// Check new RS annotations
	newRS, err := tester.expectNewReplicaSet()
	if err != nil {
		t.Fatal(err)
	}
	if newRS.Annotations["test"] != "should-copy-to-replica-set" {
		t.Errorf("expected new ReplicaSet annotations copied from Deployment %s, got: %v", tester.deployment.Name, newRS.Annotations)
	}
	if newRS.Annotations[v1.LastAppliedConfigAnnotation] != "" {
		t.Errorf("expected new ReplicaSet last-applied annotation not copied from Deployment %s", tester.deployment.Name)
	}

	// New RS should contain pod-template-hash in its selector, label, and template label
	rsHash, err := checkRSHashLabels(newRS)
	if err != nil {
		t.Error(err)
	}

	// All pods targeted by the deployment should contain pod-template-hash in their labels
	selector, err := metav1.LabelSelectorAsSelector(tester.deployment.Spec.Selector)
	if err != nil {
		t.Fatalf("failed to parse deployment %s selector: %v", name, err)
	}
	pods, err := c.CoreV1().Pods(ns.Name).List(context.TODO(), metav1.ListOptions{LabelSelector: selector.String()})
	if err != nil {
		t.Fatalf("failed to list pods of deployment %s: %v", name, err)
	}
	if len(pods.Items) != int(replicas) {
		t.Errorf("expected %d pods, got %d pods", replicas, len(pods.Items))
	}
	podHash, err := checkPodsHashLabel(pods)
	if err != nil {
		t.Error(err)
	}
	if rsHash != podHash {
		t.Errorf("found mismatching pod-template-hash value: rs hash = %s whereas pod hash = %s", rsHash, podHash)
	}
}

// Deployments should support roll out, roll back, and roll over.
func TestDeploymentRollingUpdate(t *testing.T) {
	_, ctx := ktesting.NewTestContext(t)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	closeFn, rm, dc, informers, c := dcSetup(ctx, t)
	defer closeFn()

	name := "test-rolling-update-deployment"
	ns := framework.CreateNamespaceOrDie(c, name, t)
	defer framework.DeleteNamespaceOrDie(c, ns, t)

	// Start informer and controllers
	stopControllers := runControllersAndInformers(t, rm, dc, informers)
	defer stopControllers()

	replicas := int32(20)
	tester := &deploymentTester{t: t, c: c, deployment: newDeployment(name, ns.Name, replicas)}
	tester.deployment.Spec.MinReadySeconds = 4
	quarter := intstr.FromString("25%")
	tester.deployment.Spec.Strategy.RollingUpdate = &apps.RollingUpdateDeployment{
		MaxUnavailable: &quarter,
		MaxSurge:       &quarter,
	}

	// Create a deployment.
	var err error
	tester.deployment, err = c.AppsV1().Deployments(ns.Name).Create(context.TODO(), tester.deployment, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("failed to create deployment %s: %v", tester.deployment.Name, err)
	}
	oriImage := tester.deployment.Spec.Template.Spec.Containers[0].Image
	if err := tester.waitForDeploymentRevisionAndImage("1", oriImage); err != nil {
		t.Fatal(err)
	}
	if err := tester.waitForDeploymentCompleteAndMarkPodsReady(); err != nil {
		t.Fatal(err)
	}

	// 1. Roll out a new image.
	image := "new-image"
	if oriImage == image {
		t.Fatalf("bad test setup, deployment %s roll out with the same image", tester.deployment.Name)
	}
	imageFn := func(update *apps.Deployment) {
		update.Spec.Template.Spec.Containers[0].Image = image
	}
	tester.deployment, err = tester.updateDeployment(imageFn)
	if err != nil {
		t.Fatalf("failed to update deployment %s: %v", tester.deployment.Name, err)
	}
	if err := tester.waitForDeploymentRevisionAndImage("2", image); err != nil {
		t.Fatal(err)
	}
	if err := tester.waitForDeploymentCompleteAndCheckRollingAndMarkPodsReady(); err != nil {
		t.Fatal(err)
	}

	// 2. Roll over a deployment before the previous rolling update finishes.
	image = "dont-finish"
	imageFn = func(update *apps.Deployment) {
		update.Spec.Template.Spec.Containers[0].Image = image
	}
	tester.deployment, err = tester.updateDeployment(imageFn)
	if err != nil {
		t.Fatalf("failed to update deployment %s: %v", tester.deployment.Name, err)
	}
	if err := tester.waitForDeploymentRevisionAndImage("3", image); err != nil {
		t.Fatal(err)
	}

	// We don't mark pods as ready so that rollout won't finish.
	// Before the rollout finishes, trigger another rollout.
	image = "rollover"
	imageFn = func(update *apps.Deployment) {
		update.Spec.Template.Spec.Containers[0].Image = image
	}
	tester.deployment, err = tester.updateDeployment(imageFn)
	if err != nil {
		t.Fatalf("failed to update deployment %s: %v", tester.deployment.Name, err)
	}
	if err := tester.waitForDeploymentRevisionAndImage("4", image); err != nil {
		t.Fatal(err)
	}
	if err := tester.waitForDeploymentCompleteAndCheckRollingAndMarkPodsReady(); err != nil {
		t.Fatal(err)
	}
	_, allOldRSs, err := testutil.GetOldReplicaSets(tester.deployment, c)
	if err != nil {
		t.Fatalf("failed retrieving old replicasets of deployment %s: %v", tester.deployment.Name, err)
	}
	for _, oldRS := range allOldRSs {
		if *oldRS.Spec.Replicas != 0 {
			t.Errorf("expected old replicaset %s of deployment %s to have 0 replica, got %d", oldRS.Name, tester.deployment.Name, *oldRS.Spec.Replicas)
		}
	}
}

// selectors are IMMUTABLE for all API versions except apps/v1beta1 and extensions/v1beta1
func TestDeploymentSelectorImmutability(t *testing.T) {
	closeFn, c := dcSimpleSetup(t)
	defer closeFn()

	name := "test-deployment-selector-immutability"
	ns := framework.CreateNamespaceOrDie(c, name, t)
	defer framework.DeleteNamespaceOrDie(c, ns, t)

	tester := &deploymentTester{t: t, c: c, deployment: newDeployment(name, ns.Name, int32(20))}
	var err error
	tester.deployment, err = c.AppsV1().Deployments(ns.Name).Create(context.TODO(), tester.deployment, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("failed to create apps/v1 deployment %s: %v", tester.deployment.Name, err)
	}

	// test to ensure apps/v1 selector is immutable
	deploymentAppsV1, err := c.AppsV1().Deployments(ns.Name).Get(context.TODO(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed to get apps/v1 deployment %s: %v", name, err)
	}
	newSelectorLabels := map[string]string{"name_apps_v1": "test_apps_v1"}
	deploymentAppsV1.Spec.Selector.MatchLabels = newSelectorLabels
	deploymentAppsV1.Spec.Template.Labels = newSelectorLabels
	_, err = c.AppsV1().Deployments(ns.Name).Update(context.TODO(), deploymentAppsV1, metav1.UpdateOptions{})
	if err == nil {
		t.Fatalf("failed to provide validation error when changing immutable selector when updating apps/v1 deployment %s", deploymentAppsV1.Name)
	}
	expectedErrType := "Invalid value"
	expectedErrDetail := "field is immutable"
	if !strings.Contains(err.Error(), expectedErrType) || !strings.Contains(err.Error(), expectedErrDetail) {
		t.Errorf("error message does not match, expected type: %s, expected detail: %s, got: %s", expectedErrType, expectedErrDetail, err.Error())
	}
}

// Paused deployment should not start new rollout
func TestPausedDeployment(t *testing.T) {
	_, ctx := ktesting.NewTestContext(t)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	closeFn, rm, dc, informers, c := dcSetup(ctx, t)
	defer closeFn()

	name := "test-paused-deployment"
	ns := framework.CreateNamespaceOrDie(c, name, t)
	defer framework.DeleteNamespaceOrDie(c, ns, t)

	replicas := int32(1)
	tester := &deploymentTester{t: t, c: c, deployment: newDeployment(name, ns.Name, replicas)}
	tester.deployment.Spec.Paused = true
	tgps := int64(1)
	tester.deployment.Spec.Template.Spec.TerminationGracePeriodSeconds = &tgps

	var err error
	tester.deployment, err = c.AppsV1().Deployments(ns.Name).Create(context.TODO(), tester.deployment, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("failed to create deployment %s: %v", tester.deployment.Name, err)
	}

	// Start informer and controllers
	stopControllers := runControllersAndInformers(t, rm, dc, informers)
	defer stopControllers()

	// Verify that the paused deployment won't create new replica set.
	if err := tester.expectNoNewReplicaSet(); err != nil {
		t.Fatal(err)
	}

	// Resume the deployment
	tester.deployment, err = tester.updateDeployment(resumeFn)
	if err != nil {
		t.Fatalf("failed to resume deployment %s: %v", tester.deployment.Name, err)
	}

	// Wait for the controller to notice the resume.
	if err := tester.waitForObservedDeployment(tester.deployment.Generation); err != nil {
		t.Fatal(err)
	}

	// Wait for the Deployment to be updated to revision 1
	if err := tester.waitForDeploymentRevisionAndImage("1", fakeImage); err != nil {
		t.Fatal(err)
	}

	// Make sure the Deployment completes while manually marking Deployment pods as ready at the same time.
	// Use soft check because this deployment was just created and rolling update strategy might be violated.
	if err := tester.waitForDeploymentCompleteAndMarkPodsReady(); err != nil {
		t.Fatal(err)
	}

	// A new replicaset should be created.
	if _, err := tester.expectNewReplicaSet(); err != nil {
		t.Fatal(err)
	}

	// Pause the deployment.
	// The paused deployment shouldn't trigger a new rollout.
	tester.deployment, err = tester.updateDeployment(pauseFn)
	if err != nil {
		t.Fatalf("failed to pause deployment %s: %v", tester.deployment.Name, err)
	}

	// Wait for the controller to notice the pause.
	if err := tester.waitForObservedDeployment(tester.deployment.Generation); err != nil {
		t.Fatal(err)
	}

	// Update the deployment template
	newTGPS := int64(0)
	tester.deployment, err = tester.updateDeployment(func(update *apps.Deployment) {
		update.Spec.Template.Spec.TerminationGracePeriodSeconds = &newTGPS
	})
	if err != nil {
		t.Fatalf("failed updating template of deployment %s: %v", tester.deployment.Name, err)
	}

	// Wait for the controller to notice the rollout.
	if err := tester.waitForObservedDeployment(tester.deployment.Generation); err != nil {
		t.Fatal(err)
	}

	// Verify that the paused deployment won't create new replica set.
	if err := tester.expectNoNewReplicaSet(); err != nil {
		t.Fatal(err)
	}

	_, allOldRs, err := testutil.GetOldReplicaSets(tester.deployment, c)
	if err != nil {
		t.Fatalf("failed retrieving old replicasets of deployment %s: %v", tester.deployment.Name, err)
	}
	if len(allOldRs) != 1 {
		t.Errorf("expected an old replica set, got %v", allOldRs)
	}
	if *allOldRs[0].Spec.Template.Spec.TerminationGracePeriodSeconds == newTGPS {
		t.Errorf("TerminationGracePeriodSeconds on the replica set should be %d, got %d", tgps, newTGPS)
	}
}

func TestScalePausedDeployment(t *testing.T) {
	tests := []struct {
		name                                 string
		enableDeploymentPodReplacementPolicy bool
		terminatingReplicas                  int32
		podReplacementPolicy                 *apps.DeploymentPodReplacementPolicy
		expectReplicasAfterScale             int32
	}{
		{
			name:                     "scale paused deployment",
			expectReplicasAfterScale: 10,
		},
		{
			name:                                 "scale paused deployment with terminating replicas and empty policy",
			enableDeploymentPodReplacementPolicy: true,
			terminatingReplicas:                  2,
			expectReplicasAfterScale:             10,
		},
		{
			name:                                 "scale paused deployment with terminating replicas and TerminationStarted policy",
			enableDeploymentPodReplacementPolicy: true,
			podReplacementPolicy:                 ptr.To(apps.TerminationStarted),
			terminatingReplicas:                  2,
			expectReplicasAfterScale:             10,
		},
		{
			name:                                 "scale paused deployment with terminating replicas and TerminationComplete policy",
			enableDeploymentPodReplacementPolicy: true,
			podReplacementPolicy:                 ptr.To(apps.TerminationComplete),
			terminatingReplicas:                  2,
			expectReplicasAfterScale:             8,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.DeploymentPodReplacementPolicy, test.enableDeploymentPodReplacementPolicy)
			testScalePausedDeployment(t, test.terminatingReplicas, test.expectReplicasAfterScale, test.podReplacementPolicy)
		})
	}
}

// Paused deployment can be scaled
func testScalePausedDeployment(t *testing.T, terminatingReplicas, expectReplicasAfterScale int32, podReplacementPolicy *apps.DeploymentPodReplacementPolicy) {
	_, ctx := ktesting.NewTestContext(t)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	closeFn, rm, dc, informers, c := dcSetup(ctx, t)
	defer closeFn()

	name := "test-scale-paused-deployment"
	ns := framework.CreateNamespaceOrDie(c, name, t)
	defer framework.DeleteNamespaceOrDie(c, ns, t)

	replicas := int32(2)
	tester := &deploymentTester{t: t, c: c, deployment: newDeployment(name, ns.Name, replicas)}
	tester.deployment.Spec.Strategy.RollingUpdate.MaxSurge = ptr.To(intstr.FromInt32(0))
	tester.deployment.Spec.PodReplacementPolicy = podReplacementPolicy
	tester.deployment.Spec.Template.Spec.NodeName = "fake-node"
	tester.deployment.Spec.Template.Spec.TerminationGracePeriodSeconds = ptr.To(int64(300))

	var err error
	tester.deployment, err = c.AppsV1().Deployments(ns.Name).Create(context.TODO(), tester.deployment, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("failed to create deployment %s: %v", tester.deployment.Name, err)
	}

	// Start informer and controllers
	stopControllers := runControllersAndInformers(t, rm, dc, informers)
	defer stopControllers()

	// Wait for the Deployment to be updated to revision 1
	if err := tester.waitForDeploymentRevisionAndImage("1", fakeImage); err != nil {
		t.Fatal(err)
	}

	// Make sure the Deployment completes while manually marking Deployment pods as ready at the same time.
	// Use soft check because this deployment was just created and rolling update strategy might be violated.
	if err := tester.waitForDeploymentCompleteAndMarkPodsReady(); err != nil {
		t.Fatal(err)
	}

	// A new replicaset should be created.
	firstRS, err := tester.expectNewReplicaSet()
	if err != nil {
		t.Fatal(err)
	}

	// Pause the deployment.
	tester.deployment, err = tester.updateDeployment(pauseFn)
	if err != nil {
		t.Fatalf("failed to pause deployment %s: %v", tester.deployment.Name, err)
	}

	// Wait for the controller to notice the scale.
	if err := tester.waitForObservedDeployment(tester.deployment.Generation); err != nil {
		t.Fatal(err)
	}

	if terminatingReplicas > 0 {
		// start terminating pods
		err = tester.removeRSPods(ctx, firstRS, int(terminatingReplicas), false, 300)
		if err != nil {
			t.Fatal(err)
		}
		// Verify all replicas fields of DeploymentStatus have desired counts after pod termination
		if err = tester.waitForDeploymentStatusReplicasFields(ctx, replicas, replicas, 0, 0, replicas, ptr.To(terminatingReplicas)); err != nil {
			t.Fatal(err)
		}
	}

	// Scale the paused deployment.
	newReplicas := int32(10)
	tester.deployment, err = tester.updateDeployment(func(update *apps.Deployment) {
		update.Spec.Replicas = &newReplicas
	})
	if err != nil {
		t.Fatalf("failed updating deployment %s: %v", tester.deployment.Name, err)
	}

	// Wait for the controller to notice the scale.
	if err := tester.waitForObservedDeployment(tester.deployment.Generation); err != nil {
		t.Fatal(err)
	}

	if terminatingReplicas > 0 {
		// Verify that the new replicaset is scaled while potentially taking terminating pods into account.
		rs, err := tester.expectNewReplicaSet()
		if err != nil {
			t.Fatal(err)
		}
		if *rs.Spec.Replicas != expectReplicasAfterScale {
			t.Errorf("expected new replicaset replicas = %d, got %d", expectReplicasAfterScale, *rs.Spec.Replicas)
		}

		// remove terminating pods and skip graceful termination of the RS
		if err := tester.removeRSPods(ctx, firstRS, math.MaxInt, true, 0); err != nil {
			t.Fatal(err)
		}
		// Verify all replicas fields of DeploymentStatus have desired counts after pod termination
		if err = tester.waitForDeploymentStatusReplicasFields(ctx, newReplicas, newReplicas, 0, 0, newReplicas, ptr.To[int32](0)); err != nil {
			t.Fatal(err)
		}
	}

	// Verify that the new replicaset is scaled.
	rs, err := tester.expectNewReplicaSet()
	if err != nil {
		t.Fatal(err)
	}
	if *rs.Spec.Replicas != newReplicas {
		t.Errorf("expected new replicaset replicas = %d, got %d", newReplicas, *rs.Spec.Replicas)
	}

	// Make sure the Deployment completes while manually marking Deployment pods as ready at the same time.
	// Use soft check because this deployment was just scaled and rolling update strategy might be violated.
	if err := tester.waitForDeploymentCompleteAndMarkPodsReady(); err != nil {
		t.Fatal(err)
	}
}

// Deployment rollout shouldn't be blocked on hash collisions
func TestDeploymentHashCollision(t *testing.T) {
	_, ctx := ktesting.NewTestContext(t)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	closeFn, rm, dc, informers, c := dcSetup(ctx, t)
	defer closeFn()

	name := "test-hash-collision-deployment"
	ns := framework.CreateNamespaceOrDie(c, name, t)
	defer framework.DeleteNamespaceOrDie(c, ns, t)

	replicas := int32(1)
	tester := &deploymentTester{t: t, c: c, deployment: newDeployment(name, ns.Name, replicas)}

	var err error
	tester.deployment, err = c.AppsV1().Deployments(ns.Name).Create(context.TODO(), tester.deployment, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("failed to create deployment %s: %v", tester.deployment.Name, err)
	}

	// Start informer and controllers
	stopControllers := runControllersAndInformers(t, rm, dc, informers)
	defer stopControllers()

	// Wait for the Deployment to be updated to revision 1
	if err := tester.waitForDeploymentRevisionAndImage("1", fakeImage); err != nil {
		t.Fatal(err)
	}

	// Mock a hash collision
	newRS, err := testutil.GetNewReplicaSet(tester.deployment, c)
	if err != nil {
		t.Fatalf("failed getting new replicaset of deployment %s: %v", tester.deployment.Name, err)
	}
	if newRS == nil {
		t.Fatalf("unable to find new replicaset of deployment %s", tester.deployment.Name)
	}
	_, err = tester.updateReplicaSet(newRS.Name, func(update *apps.ReplicaSet) {
		*update.Spec.Template.Spec.TerminationGracePeriodSeconds = int64(5)
	})
	if err != nil {
		t.Fatalf("failed updating replicaset %s template: %v", newRS.Name, err)
	}

	// Expect deployment collision counter to increment
	if err := wait.PollImmediate(pollInterval, pollTimeout, func() (bool, error) {
		d, err := c.AppsV1().Deployments(ns.Name).Get(context.TODO(), tester.deployment.Name, metav1.GetOptions{})
		if err != nil {
			return false, nil
		}
		return d.Status.CollisionCount != nil && *d.Status.CollisionCount == int32(1), nil
	}); err != nil {
		t.Fatalf("Failed to increment collision counter for deployment %q: %v", tester.deployment.Name, err)
	}

	// Expect a new ReplicaSet to be created
	if err := tester.waitForDeploymentRevisionAndImage("2", fakeImage); err != nil {
		t.Fatal(err)
	}
}

func checkRSHashLabels(rs *apps.ReplicaSet) (string, error) {
	hash := rs.Labels[apps.DefaultDeploymentUniqueLabelKey]
	selectorHash := rs.Spec.Selector.MatchLabels[apps.DefaultDeploymentUniqueLabelKey]
	templateLabelHash := rs.Spec.Template.Labels[apps.DefaultDeploymentUniqueLabelKey]

	if hash != selectorHash || selectorHash != templateLabelHash {
		return "", fmt.Errorf("mismatching hash value found in replicaset %s: %#v", rs.Name, rs)
	}
	if len(hash) == 0 {
		return "", fmt.Errorf("unexpected replicaset %s missing required pod-template-hash labels", rs.Name)
	}

	if !strings.HasSuffix(rs.Name, hash) {
		return "", fmt.Errorf("unexpected replicaset %s name suffix doesn't match hash %s", rs.Name, hash)
	}

	return hash, nil
}

func checkPodsHashLabel(pods *v1.PodList) (string, error) {
	if len(pods.Items) == 0 {
		return "", fmt.Errorf("no pods given")
	}
	var hash string
	for _, pod := range pods.Items {
		podHash := pod.Labels[apps.DefaultDeploymentUniqueLabelKey]
		if len(podHash) == 0 {
			return "", fmt.Errorf("found pod %s missing pod-template-hash label: %#v", pod.Name, pods)
		}
		// Save the first valid hash
		if len(hash) == 0 {
			hash = podHash
		}
		if podHash != hash {
			return "", fmt.Errorf("found pod %s with mismatching pod-template-hash value %s: %#v", pod.Name, podHash, pods)
		}
	}
	return hash, nil
}

// Deployment should have a timeout condition when it fails to progress after given deadline.
func TestFailedDeployment(t *testing.T) {
	_, ctx := ktesting.NewTestContext(t)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	closeFn, rm, dc, informers, c := dcSetup(ctx, t)
	defer closeFn()

	name := "test-failed-deployment"
	ns := framework.CreateNamespaceOrDie(c, name, t)
	defer framework.DeleteNamespaceOrDie(c, ns, t)

	deploymentName := "progress-check"
	replicas := int32(1)
	three := int32(3)
	tester := &deploymentTester{t: t, c: c, deployment: newDeployment(deploymentName, ns.Name, replicas)}
	tester.deployment.Spec.ProgressDeadlineSeconds = &three
	var err error
	tester.deployment, err = c.AppsV1().Deployments(ns.Name).Create(context.TODO(), tester.deployment, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("failed to create deployment %q: %v", deploymentName, err)
	}

	// Start informer and controllers
	stopControllers := runControllersAndInformers(t, rm, dc, informers)
	defer stopControllers()

	if err = tester.waitForDeploymentUpdatedReplicasGTE(replicas); err != nil {
		t.Fatal(err)
	}

	// Pods are not marked as Ready, therefore the deployment progress will eventually timeout after progressDeadlineSeconds has passed.
	// Wait for the deployment to have a progress timeout condition.
	if err = tester.waitForDeploymentWithCondition(deploymentutil.TimedOutReason, apps.DeploymentProgressing); err != nil {
		t.Fatal(err)
	}

	// Manually mark pods as Ready and wait for deployment to complete.
	if err := tester.waitForDeploymentCompleteAndMarkPodsReady(); err != nil {
		t.Fatalf("deployment %q fails to have its status becoming valid: %v", deploymentName, err)
	}

	// Wait for the deployment to have a progress complete condition.
	if err = tester.waitForDeploymentWithCondition(deploymentutil.NewRSAvailableReason, apps.DeploymentProgressing); err != nil {
		t.Fatal(err)
	}
}

// Deployment should have a timeout condition when it fails to progress after given deadline.
// If the number of terminating pods decreases, the DeploymentProgressing condition can report
// a progress if a PodReplacementPolicy is used.
func TestFailedDeploymentWithTerminatingPods(t *testing.T) {
	tests := []struct {
		name                                 string
		enableDeploymentPodReplacementPolicy bool
		podReplacementPolicy                 *apps.DeploymentPodReplacementPolicy

		expectedTerminatingReplicas        *int32
		expectedProgressingConditionReason string
	}{
		{
			name:                                 "decrease of terminating pods should not progress a deployment with DeploymentPodReplacementPolicy=false",
			enableDeploymentPodReplacementPolicy: false,
			expectedTerminatingReplicas:          nil,
			expectedProgressingConditionReason:   deploymentutil.TimedOutReason,
		},
		{
			name:                                 "decrease of terminating pods should not progress a deployment with DeploymentPodReplacementPolicy=true",
			enableDeploymentPodReplacementPolicy: true,
			expectedTerminatingReplicas:          ptr.To[int32](2),
			expectedProgressingConditionReason:   deploymentutil.TimedOutReason,
		},
		{
			name:                                 "decrease of terminating pods should not progress a deployment with TerminationStarted policy",
			enableDeploymentPodReplacementPolicy: true,
			podReplacementPolicy:                 ptr.To(apps.TerminationStarted),
			expectedTerminatingReplicas:          ptr.To[int32](2),
			expectedProgressingConditionReason:   deploymentutil.TimedOutReason,
		},
		{
			name:                                 "decrease of terminating pods should progress a deployment with TerminationComplete policy",
			enableDeploymentPodReplacementPolicy: true,
			podReplacementPolicy:                 ptr.To(apps.TerminationComplete),
			expectedTerminatingReplicas:          ptr.To[int32](2),
			expectedProgressingConditionReason:   deploymentutil.ReplicaSetUpdatedReason,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.DeploymentPodReplacementPolicy, test.enableDeploymentPodReplacementPolicy)

			_, ctx := ktesting.NewTestContext(t)
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()

			closeFn, rm, dc, informers, c := dcSetup(ctx, t)
			defer closeFn()

			name := "test-failed-deployment"
			ns := framework.CreateNamespaceOrDie(c, name, t)
			defer framework.DeleteNamespaceOrDie(c, ns, t)

			deploymentName := "progress-check"
			replicas := int32(4)
			three := int32(3)
			tester := &deploymentTester{t: t, c: c, deployment: newDeployment(deploymentName, ns.Name, replicas)}
			tester.deployment.Spec.ProgressDeadlineSeconds = &three
			tester.deployment.Spec.PodReplacementPolicy = test.podReplacementPolicy
			tester.deployment.Spec.Template.Spec.NodeName = "fake-node"
			tester.deployment.Spec.Template.Spec.TerminationGracePeriodSeconds = ptr.To(int64(300))
			var err error
			tester.deployment, err = c.AppsV1().Deployments(ns.Name).Create(context.TODO(), tester.deployment, metav1.CreateOptions{})
			if err != nil {
				t.Fatalf("failed to create deployment %q: %v", deploymentName, err)
			}

			// Start informer and controllers
			stopControllers := runControllersAndInformers(t, rm, dc, informers)
			defer stopControllers()

			if err = tester.waitForDeploymentUpdatedReplicasGTE(replicas); err != nil {
				t.Fatal(err)
			}
			// Record current replicaset
			rs, err := tester.expectNewReplicaSet()
			if err != nil {
				t.Fatal(err)
			}
			// Terminate 2 replicas
			err = tester.removeRSPods(ctx, rs, 2, false, 300)
			if err != nil {
				t.Fatal(err)
			}
			if err = tester.waitForDeploymentStatusReplicasFields(ctx, replicas, replicas, 0, 0, replicas, test.expectedTerminatingReplicas); err != nil {
				t.Fatal(err)
			}
			// Pods are not marked as Ready, therefore the deployment progress will eventually timeout after progressDeadlineSeconds has passed.
			// Wait for the deployment to have a progress timeout condition.
			if err = tester.waitForDeploymentWithCondition(deploymentutil.TimedOutReason, apps.DeploymentProgressing); err != nil {
				t.Fatal(err)
			}

			// remove terminating pods and skip graceful termination of the RS
			if err := tester.removeRSPods(ctx, rs, math.MaxInt, true, 0); err != nil {
				t.Fatal(err)
			}
			// Check for a potential change in the progressing condition
			if err = tester.waitForDeploymentWithCondition(test.expectedProgressingConditionReason, apps.DeploymentProgressing); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestOverlappingDeployments(t *testing.T) {
	_, ctx := ktesting.NewTestContext(t)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	closeFn, rm, dc, informers, c := dcSetup(ctx, t)
	defer closeFn()

	name := "test-overlapping-deployments"
	ns := framework.CreateNamespaceOrDie(c, name, t)
	defer framework.DeleteNamespaceOrDie(c, ns, t)

	replicas := int32(1)
	firstDeploymentName := "first-deployment"
	secondDeploymentName := "second-deployment"
	testers := []*deploymentTester{
		{t: t, c: c, deployment: newDeployment(firstDeploymentName, ns.Name, replicas)},
		{t: t, c: c, deployment: newDeployment(secondDeploymentName, ns.Name, replicas)},
	}

	// Start informer and controllers
	stopControllers := runControllersAndInformers(t, rm, dc, informers)
	defer stopControllers()

	// Create 2 deployments with overlapping selectors
	var err error
	var rss []*apps.ReplicaSet
	for _, tester := range testers {
		tester.deployment, err = c.AppsV1().Deployments(ns.Name).Create(context.TODO(), tester.deployment, metav1.CreateOptions{})
		dname := tester.deployment.Name
		if err != nil {
			t.Fatalf("failed to create deployment %q: %v", dname, err)
		}
		// Wait for the deployment to be updated to revision 1
		if err = tester.waitForDeploymentRevisionAndImage("1", fakeImage); err != nil {
			t.Fatalf("failed to update deployment %q to revision 1: %v", dname, err)
		}
		// Make sure the deployment completes while manually marking its pods as ready at the same time
		if err = tester.waitForDeploymentCompleteAndMarkPodsReady(); err != nil {
			t.Fatalf("deployment %q failed to complete: %v", dname, err)
		}
		// Get replicaset of the deployment
		newRS, err := tester.getNewReplicaSet()
		if err != nil {
			t.Fatalf("failed to get new replicaset of deployment %q: %v", dname, err)
		}
		if newRS == nil {
			t.Fatalf("unable to find new replicaset of deployment %q", dname)
		}
		// Store the replicaset for future usage
		rss = append(rss, newRS)
	}

	// Both deployments should proceed independently, so their respective replicaset should not be the same replicaset
	if rss[0].UID == rss[1].UID {
		t.Fatalf("overlapping deployments should not share the same replicaset")
	}

	// Scale only the first deployment by 1
	newReplicas := replicas + 1
	testers[0].deployment, err = testers[0].updateDeployment(func(update *apps.Deployment) {
		update.Spec.Replicas = &newReplicas
	})
	if err != nil {
		t.Fatalf("failed updating deployment %q: %v", firstDeploymentName, err)
	}

	// Make sure the deployment completes after scaling
	if err := testers[0].waitForDeploymentCompleteAndMarkPodsReady(); err != nil {
		t.Fatalf("deployment %q failed to complete after scaling: %v", firstDeploymentName, err)
	}

	// Verify replicaset of both deployments has updated number of replicas
	for i, tester := range testers {
		rs, err := c.AppsV1().ReplicaSets(ns.Name).Get(context.TODO(), rss[i].Name, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("failed to get replicaset %q: %v", rss[i].Name, err)
		}
		if *rs.Spec.Replicas != *tester.deployment.Spec.Replicas {
			t.Errorf("expected replicaset %q of deployment %q has %d replicas, but found %d replicas", rs.Name, firstDeploymentName, *tester.deployment.Spec.Replicas, *rs.Spec.Replicas)
		}
	}
}

// Deployment should not block rollout when updating spec replica number and template at the same time.
func TestScaledRolloutDeployment(t *testing.T) {
	logger, ctx := ktesting.NewTestContext(t)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	closeFn, rm, dc, informers, c := dcSetup(ctx, t)
	defer closeFn()

	name := "test-scaled-rollout-deployment"
	ns := framework.CreateNamespaceOrDie(c, name, t)
	defer framework.DeleteNamespaceOrDie(c, ns, t)

	// Start informer and controllers
	stopControllers := runControllersAndInformers(t, rm, dc, informers)
	defer stopControllers()

	// Create a deployment with rolling update strategy, max surge = 3, and max unavailable = 2
	var err error
	replicas := int32(10)
	tester := &deploymentTester{t: t, c: c, deployment: newDeployment(name, ns.Name, replicas)}
	tester.deployment.Spec.Strategy.RollingUpdate.MaxSurge = ptr.To(intstr.FromInt32(3))
	tester.deployment.Spec.Strategy.RollingUpdate.MaxUnavailable = ptr.To(intstr.FromInt32(2))
	tester.deployment, err = c.AppsV1().Deployments(ns.Name).Create(context.TODO(), tester.deployment, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("failed to create deployment %q: %v", name, err)
	}
	if err = tester.waitForDeploymentRevisionAndImage("1", fakeImage); err != nil {
		t.Fatal(err)
	}
	if err = tester.waitForDeploymentCompleteAndMarkPodsReady(); err != nil {
		t.Fatalf("deployment %q failed to complete: %v", name, err)
	}

	// Record current replicaset before starting new rollout
	firstRS, err := tester.expectNewReplicaSet()
	if err != nil {
		t.Fatal(err)
	}

	// Update the deployment with another new image but do not mark the pods as ready to block new replicaset
	fakeImage2 := "fakeimage2"
	tester.deployment, err = tester.updateDeployment(func(update *apps.Deployment) {
		update.Spec.Template.Spec.Containers[0].Image = fakeImage2
	})
	if err != nil {
		t.Fatalf("failed updating deployment %q: %v", name, err)
	}
	if err = tester.waitForDeploymentRevisionAndImage("2", fakeImage2); err != nil {
		t.Fatal(err)
	}

	// Verify the deployment has minimum available replicas after 2nd rollout
	tester.deployment, err = c.AppsV1().Deployments(ns.Name).Get(context.TODO(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed to get deployment %q: %v", name, err)
	}
	minAvailableReplicas := deploymentutil.MinAvailable(tester.deployment)
	if tester.deployment.Status.AvailableReplicas < minAvailableReplicas {
		t.Fatalf("deployment %q does not have minimum number of available replicas after 2nd rollout", name)
	}

	// Wait for old replicaset of 1st rollout to have desired replicas
	firstRS, err = c.AppsV1().ReplicaSets(ns.Name).Get(context.TODO(), firstRS.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed to get replicaset %q: %v", firstRS.Name, err)
	}
	if err = tester.waitRSStable(firstRS); err != nil {
		t.Fatal(err)
	}

	// Wait for new replicaset of 2nd rollout to have desired replicas
	secondRS, err := tester.expectNewReplicaSet()
	if err != nil {
		t.Fatal(err)
	}
	if err = tester.waitRSStable(secondRS); err != nil {
		t.Fatal(err)
	}

	// Scale up the deployment and update its image to another new image simultaneously (this time marks all pods as ready)
	newReplicas := int32(20)
	fakeImage3 := "fakeimage3"
	tester.deployment, err = tester.updateDeployment(func(update *apps.Deployment) {
		update.Spec.Replicas = &newReplicas
		update.Spec.Template.Spec.Containers[0].Image = fakeImage3
	})
	if err != nil {
		t.Fatalf("failed updating deployment %q: %v", name, err)
	}
	if err = tester.waitForDeploymentRevisionAndImage("3", fakeImage3); err != nil {
		t.Fatal(err)
	}
	if err = tester.waitForDeploymentCompleteAndMarkPodsReady(); err != nil {
		t.Fatalf("deployment %q failed to complete: %v", name, err)
	}

	// Verify every replicaset has correct desiredReplicas annotation after 3rd rollout
	thirdRS, err := testutil.GetNewReplicaSet(tester.deployment, c)
	if err != nil {
		t.Fatalf("failed getting new revision 3 replicaset for deployment %q: %v", name, err)
	}
	rss := []*apps.ReplicaSet{firstRS, secondRS, thirdRS}
	for _, curRS := range rss {
		curRS, err = c.AppsV1().ReplicaSets(ns.Name).Get(context.TODO(), curRS.Name, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("failed to get replicaset when checking desired replicas annotation: %v", err)
		}
		desired, ok := deploymentutil.GetDesiredReplicasAnnotation(logger, curRS)
		if !ok {
			t.Fatalf("failed to retrieve desiredReplicas annotation for replicaset %q", curRS.Name)
		}
		if desired != *(tester.deployment.Spec.Replicas) {
			t.Fatalf("unexpected desiredReplicas annotation for replicaset %q: expected %d, got %d", curRS.Name, *(tester.deployment.Spec.Replicas), desired)
		}
	}

	// Update the deployment with another new image but do not mark the pods as ready to block new replicaset
	fakeImage4 := "fakeimage4"
	tester.deployment, err = tester.updateDeployment(func(update *apps.Deployment) {
		update.Spec.Template.Spec.Containers[0].Image = fakeImage4
	})
	if err != nil {
		t.Fatalf("failed updating deployment %q: %v", name, err)
	}
	if err = tester.waitForDeploymentRevisionAndImage("4", fakeImage4); err != nil {
		t.Fatal(err)
	}

	// Verify the deployment has minimum available replicas after 4th rollout
	tester.deployment, err = c.AppsV1().Deployments(ns.Name).Get(context.TODO(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed to get deployment %q: %v", name, err)
	}
	minAvailableReplicas = deploymentutil.MinAvailable(tester.deployment)
	if tester.deployment.Status.AvailableReplicas < minAvailableReplicas {
		t.Fatalf("deployment %q does not have minimum number of available replicas after 4th rollout", name)
	}

	// Wait for old replicaset of 3rd rollout to have desired replicas
	thirdRS, err = c.AppsV1().ReplicaSets(ns.Name).Get(context.TODO(), thirdRS.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed to get replicaset %q: %v", thirdRS.Name, err)
	}
	if err = tester.waitRSStable(thirdRS); err != nil {
		t.Fatal(err)
	}

	// Wait for new replicaset of 4th rollout to have desired replicas
	fourthRS, err := tester.expectNewReplicaSet()
	if err != nil {
		t.Fatal(err)
	}
	if err = tester.waitRSStable(fourthRS); err != nil {
		t.Fatal(err)
	}

	// Scale down the deployment and update its image to another new image simultaneously (this time marks all pods as ready)
	newReplicas = int32(5)
	fakeImage5 := "fakeimage5"
	tester.deployment, err = tester.updateDeployment(func(update *apps.Deployment) {
		update.Spec.Replicas = &newReplicas
		update.Spec.Template.Spec.Containers[0].Image = fakeImage5
	})
	if err != nil {
		t.Fatalf("failed updating deployment %q: %v", name, err)
	}
	if err = tester.waitForDeploymentRevisionAndImage("5", fakeImage5); err != nil {
		t.Fatal(err)
	}
	if err = tester.waitForDeploymentCompleteAndMarkPodsReady(); err != nil {
		t.Fatalf("deployment %q failed to complete: %v", name, err)
	}

	// Verify every replicaset has correct desiredReplicas annotation after 5th rollout
	fifthRS, err := testutil.GetNewReplicaSet(tester.deployment, c)
	if err != nil {
		t.Fatalf("failed getting new revision 5 replicaset for deployment %q: %v", name, err)
	}
	rss = []*apps.ReplicaSet{thirdRS, fourthRS, fifthRS}
	for _, curRS := range rss {
		curRS, err = c.AppsV1().ReplicaSets(ns.Name).Get(context.TODO(), curRS.Name, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("failed to get replicaset when checking desired replicas annotation: %v", err)
		}
		desired, ok := deploymentutil.GetDesiredReplicasAnnotation(logger, curRS)
		if !ok {
			t.Fatalf("failed to retrieve desiredReplicas annotation for replicaset %q", curRS.Name)
		}
		if desired != *(tester.deployment.Spec.Replicas) {
			t.Fatalf("unexpected desiredReplicas annotation for replicaset %q: expected %d, got %d", curRS.Name, *(tester.deployment.Spec.Replicas), desired)
		}
	}
}

func TestSpecReplicasChange(t *testing.T) {
	_, ctx := ktesting.NewTestContext(t)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	closeFn, rm, dc, informers, c := dcSetup(ctx, t)
	defer closeFn()

	name := "test-spec-replicas-change"
	ns := framework.CreateNamespaceOrDie(c, name, t)
	defer framework.DeleteNamespaceOrDie(c, ns, t)

	deploymentName := "deployment"
	replicas := int32(1)
	tester := &deploymentTester{t: t, c: c, deployment: newDeployment(deploymentName, ns.Name, replicas)}
	tester.deployment.Spec.Strategy.Type = apps.RecreateDeploymentStrategyType
	tester.deployment.Spec.Strategy.RollingUpdate = nil
	var err error
	tester.deployment, err = c.AppsV1().Deployments(ns.Name).Create(context.TODO(), tester.deployment, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("failed to create deployment %q: %v", deploymentName, err)
	}

	// Start informer and controllers
	stopControllers := runControllersAndInformers(t, rm, dc, informers)
	defer stopControllers()

	// Scale up/down deployment and verify its replicaset has matching .spec.replicas
	if err = tester.scaleDeployment(2); err != nil {
		t.Fatal(err)
	}
	if err = tester.scaleDeployment(0); err != nil {
		t.Fatal(err)
	}
	if err = tester.scaleDeployment(1); err != nil {
		t.Fatal(err)
	}

	// Add a template annotation change to test deployment's status does update
	// without .spec.replicas change
	var oldGeneration int64
	tester.deployment, err = tester.updateDeployment(func(update *apps.Deployment) {
		oldGeneration = update.Generation
		update.Spec.RevisionHistoryLimit = ptr.To[int32](4)
	})
	if err != nil {
		t.Fatalf("failed updating deployment %q: %v", tester.deployment.Name, err)
	}

	savedGeneration := tester.deployment.Generation
	if savedGeneration == oldGeneration {
		t.Fatalf("Failed to verify .Generation has incremented for deployment %q", deploymentName)
	}
	if err = tester.waitForObservedDeployment(savedGeneration); err != nil {
		t.Fatal(err)
	}
}

func TestDeploymentAvailableCondition(t *testing.T) {
	_, ctx := ktesting.NewTestContext(t)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	closeFn, rm, dc, informers, c := dcSetup(ctx, t)
	defer closeFn()

	name := "test-deployment-available-condition"
	ns := framework.CreateNamespaceOrDie(c, name, t)
	defer framework.DeleteNamespaceOrDie(c, ns, t)

	deploymentName := "deployment"
	replicas := int32(10)
	tester := &deploymentTester{t: t, c: c, deployment: newDeployment(deploymentName, ns.Name, replicas)}
	// Assign a high value to the deployment's minReadySeconds
	tester.deployment.Spec.MinReadySeconds = 3600
	// progressDeadlineSeconds must be greater than minReadySeconds
	tester.deployment.Spec.ProgressDeadlineSeconds = ptr.To[int32](7200)
	var err error
	tester.deployment, err = c.AppsV1().Deployments(ns.Name).Create(context.TODO(), tester.deployment, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("failed to create deployment %q: %v", deploymentName, err)
	}

	// Start informer and controllers
	stopControllers := runControllersAndInformers(t, rm, dc, informers)
	defer stopControllers()

	// Wait for the deployment to be observed by the controller and has at least specified number of updated replicas
	if err = tester.waitForDeploymentUpdatedReplicasGTE(replicas); err != nil {
		t.Fatal(err)
	}

	// Wait for the deployment to have MinimumReplicasUnavailable reason because the pods are not marked as ready
	if err = tester.waitForDeploymentWithCondition(deploymentutil.MinimumReplicasUnavailable, apps.DeploymentAvailable); err != nil {
		t.Fatal(err)
	}

	// Verify all replicas fields of DeploymentStatus have desired counts
	if err = tester.checkDeploymentStatusReplicasFields(10, 10, 0, 0, 10, nil); err != nil {
		t.Fatal(err)
	}

	// Mark the pods as ready without waiting for the deployment to complete
	if err = tester.markUpdatedPodsReadyWithoutComplete(); err != nil {
		t.Fatal(err)
	}

	// Wait for number of ready replicas to equal number of replicas.
	if err = tester.waitForReadyReplicas(); err != nil {
		t.Fatal(err)
	}

	// Wait for the deployment to still have MinimumReplicasUnavailable reason within minReadySeconds period
	if err = tester.waitForDeploymentWithCondition(deploymentutil.MinimumReplicasUnavailable, apps.DeploymentAvailable); err != nil {
		t.Fatal(err)
	}

	// Verify all replicas fields of DeploymentStatus have desired counts
	if err = tester.checkDeploymentStatusReplicasFields(10, 10, 10, 0, 10, nil); err != nil {
		t.Fatal(err)
	}

	// Update the deployment's minReadySeconds to a small value
	tester.deployment, err = tester.updateDeployment(func(update *apps.Deployment) {
		update.Spec.MinReadySeconds = 1
	})
	if err != nil {
		t.Fatalf("failed updating deployment %q: %v", deploymentName, err)
	}

	// Wait for the deployment to notice minReadySeconds has changed
	if err := tester.waitForObservedDeployment(tester.deployment.Generation); err != nil {
		t.Fatal(err)
	}

	// Wait for the deployment to have MinimumReplicasAvailable reason after minReadySeconds period
	if err = tester.waitForDeploymentWithCondition(deploymentutil.MinimumReplicasAvailable, apps.DeploymentAvailable); err != nil {
		t.Fatal(err)
	}

	// Verify all replicas fields of DeploymentStatus have desired counts
	if err = tester.checkDeploymentStatusReplicasFields(10, 10, 10, 10, 0, nil); err != nil {
		t.Fatal(err)
	}
}

// Wait for deployment to automatically patch incorrect ControllerRef of RS
func testRSControllerRefPatch(t *testing.T, tester *deploymentTester, rs *apps.ReplicaSet, ownerReference *metav1.OwnerReference, expectedOwnerReferenceNum int) {
	ns := rs.Namespace
	rsClient := tester.c.AppsV1().ReplicaSets(ns)
	rs, err := tester.updateReplicaSet(rs.Name, func(update *apps.ReplicaSet) {
		update.OwnerReferences = []metav1.OwnerReference{*ownerReference}
	})
	if err != nil {
		t.Fatalf("failed to update replicaset %q: %v", rs.Name, err)
	}

	if err := wait.PollImmediate(pollInterval, pollTimeout, func() (bool, error) {
		newRS, err := rsClient.Get(context.TODO(), rs.Name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		return metav1.GetControllerOf(newRS) != nil, nil
	}); err != nil {
		t.Fatalf("failed to wait for controllerRef of the replicaset %q to become nil: %v", rs.Name, err)
	}

	newRS, err := rsClient.Get(context.TODO(), rs.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed to obtain replicaset %q: %v", rs.Name, err)
	}
	controllerRef := metav1.GetControllerOf(newRS)
	if controllerRef.UID != tester.deployment.UID {
		t.Fatalf("controllerRef of replicaset %q has a different UID: Expected %v, got %v", newRS.Name, tester.deployment.UID, controllerRef.UID)
	}
	ownerReferenceNum := len(newRS.GetOwnerReferences())
	if ownerReferenceNum != expectedOwnerReferenceNum {
		t.Fatalf("unexpected number of owner references for replicaset %q: Expected %d, got %d", newRS.Name, expectedOwnerReferenceNum, ownerReferenceNum)
	}
}

func TestGeneralReplicaSetAdoption(t *testing.T) {
	_, ctx := ktesting.NewTestContext(t)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	closeFn, rm, dc, informers, c := dcSetup(ctx, t)
	defer closeFn()

	name := "test-general-replicaset-adoption"
	ns := framework.CreateNamespaceOrDie(c, name, t)
	defer framework.DeleteNamespaceOrDie(c, ns, t)

	deploymentName := "deployment"
	replicas := int32(1)
	tester := &deploymentTester{t: t, c: c, deployment: newDeployment(deploymentName, ns.Name, replicas)}
	var err error
	tester.deployment, err = c.AppsV1().Deployments(ns.Name).Create(context.TODO(), tester.deployment, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("failed to create deployment %q: %v", deploymentName, err)
	}

	// Start informer and controllers
	stopControllers := runControllersAndInformers(t, rm, dc, informers)
	defer stopControllers()

	// Wait for the Deployment to be updated to revision 1
	if err := tester.waitForDeploymentRevisionAndImage("1", fakeImage); err != nil {
		t.Fatal(err)
	}

	// Ensure the deployment completes while marking its pods as ready simultaneously
	if err := tester.waitForDeploymentCompleteAndMarkPodsReady(); err != nil {
		t.Fatal(err)
	}

	// Get replicaset of the deployment
	rs, err := testutil.GetNewReplicaSet(tester.deployment, c)
	if err != nil {
		t.Fatalf("failed to get replicaset of deployment %q: %v", deploymentName, err)
	}
	if rs == nil {
		t.Fatalf("unable to find replicaset of deployment %q", deploymentName)
	}

	// When the only OwnerReference of the RS points to another type of API object such as statefulset
	// with Controller=false, the deployment should add a second OwnerReference (ControllerRef) pointing to itself
	// with Controller=true
	var falseVar = false
	ownerReference := metav1.OwnerReference{UID: uuid.NewUUID(), APIVersion: "apps/v1", Kind: "StatefulSet", Name: deploymentName, Controller: &falseVar}
	testRSControllerRefPatch(t, tester, rs, &ownerReference, 2)

	// When the only OwnerReference of the RS points to the deployment with Controller=false,
	// the deployment should set Controller=true for the only OwnerReference
	ownerReference = metav1.OwnerReference{UID: tester.deployment.UID, APIVersion: "apps/v1", Kind: "Deployment", Name: deploymentName, Controller: &falseVar}
	testRSControllerRefPatch(t, tester, rs, &ownerReference, 1)
}

func testScalingUsingScaleSubresource(t *testing.T, tester *deploymentTester, replicas int32) {
	ns := tester.deployment.Namespace
	deploymentName := tester.deployment.Name
	deploymentClient := tester.c.AppsV1().Deployments(ns)
	deployment, err := deploymentClient.Get(context.TODO(), deploymentName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Failed to obtain deployment %q: %v", deploymentName, err)
	}
	scale, err := tester.c.AppsV1().Deployments(ns).GetScale(context.TODO(), deploymentName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Failed to obtain scale subresource for deployment %q: %v", deploymentName, err)
	}
	if scale.Spec.Replicas != *deployment.Spec.Replicas {
		t.Fatalf("Scale subresource for deployment %q does not match .Spec.Replicas: expected %d, got %d", deploymentName, *deployment.Spec.Replicas, scale.Spec.Replicas)
	}

	if err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		scale, err := tester.c.AppsV1().Deployments(ns).GetScale(context.TODO(), deploymentName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		scale.Spec.Replicas = replicas
		_, err = tester.c.AppsV1().Deployments(ns).UpdateScale(context.TODO(), deploymentName, scale, metav1.UpdateOptions{})
		return err
	}); err != nil {
		t.Fatalf("Failed to set .Spec.Replicas of scale subresource for deployment %q: %v", deploymentName, err)
	}

	deployment, err = deploymentClient.Get(context.TODO(), deploymentName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Failed to obtain deployment %q: %v", deploymentName, err)
	}
	if *deployment.Spec.Replicas != replicas {
		t.Fatalf(".Spec.Replicas of deployment %q does not match its scale subresource: expected %d, got %d", deploymentName, replicas, *deployment.Spec.Replicas)
	}
}

func TestDeploymentScaleSubresource(t *testing.T) {
	_, ctx := ktesting.NewTestContext(t)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	closeFn, rm, dc, informers, c := dcSetup(ctx, t)
	defer closeFn()

	name := "test-deployment-scale-subresource"
	ns := framework.CreateNamespaceOrDie(c, name, t)
	defer framework.DeleteNamespaceOrDie(c, ns, t)

	deploymentName := "deployment"
	replicas := int32(2)
	tester := &deploymentTester{t: t, c: c, deployment: newDeployment(deploymentName, ns.Name, replicas)}
	var err error
	tester.deployment, err = c.AppsV1().Deployments(ns.Name).Create(context.TODO(), tester.deployment, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("failed to create deployment %q: %v", deploymentName, err)
	}

	// Start informer and controllers
	stopControllers := runControllersAndInformers(t, rm, dc, informers)
	defer stopControllers()

	// Wait for the Deployment to be updated to revision 1
	if err := tester.waitForDeploymentRevisionAndImage("1", fakeImage); err != nil {
		t.Fatal(err)
	}

	// Ensure the deployment completes while marking its pods as ready simultaneously
	if err := tester.waitForDeploymentCompleteAndMarkPodsReady(); err != nil {
		t.Fatal(err)
	}

	// Use scale subresource to scale the deployment up to 3
	testScalingUsingScaleSubresource(t, tester, 3)
	// Use the scale subresource to scale the deployment down to 0
	testScalingUsingScaleSubresource(t, tester, 0)
}

// This test verifies that the Deployment does orphan a ReplicaSet when the ReplicaSet's
// .Labels field is changed to no longer match the Deployment's selector. It also partially
// verifies that collision avoidance mechanism is triggered when a Deployment's new ReplicaSet
// is orphaned, even without PodTemplateSpec change. Refer comment below for more info:
// https://github.com/kubernetes/kubernetes/pull/59212#discussion_r166465113
func TestReplicaSetOrphaningAndAdoptionWhenLabelsChange(t *testing.T) {
	_, ctx := ktesting.NewTestContext(t)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	closeFn, rm, dc, informers, c := dcSetup(ctx, t)
	defer closeFn()

	name := "test-replicaset-orphaning-and-adoption-when-labels-change"
	ns := framework.CreateNamespaceOrDie(c, name, t)
	defer framework.DeleteNamespaceOrDie(c, ns, t)

	deploymentName := "deployment"
	replicas := int32(1)
	tester := &deploymentTester{t: t, c: c, deployment: newDeployment(deploymentName, ns.Name, replicas)}
	var err error
	tester.deployment, err = c.AppsV1().Deployments(ns.Name).Create(context.TODO(), tester.deployment, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("failed to create deployment %q: %v", deploymentName, err)
	}

	// Start informer and controllers
	stopControllers := runControllersAndInformers(t, rm, dc, informers)
	defer stopControllers()

	// Wait for the Deployment to be updated to revision 1
	if err := tester.waitForDeploymentRevisionAndImage("1", fakeImage); err != nil {
		t.Fatal(err)
	}

	// Ensure the deployment completes while marking its pods as ready simultaneously
	if err := tester.waitForDeploymentCompleteAndMarkPodsReady(); err != nil {
		t.Fatal(err)
	}

	// Orphaning: deployment should remove OwnerReference from a RS when the RS's labels change to not match its labels

	// Get replicaset of the deployment
	rs, err := testutil.GetNewReplicaSet(tester.deployment, c)
	if err != nil {
		t.Fatalf("failed to get replicaset of deployment %q: %v", deploymentName, err)
	}
	if rs == nil {
		t.Fatalf("unable to find replicaset of deployment %q", deploymentName)
	}

	// Verify controllerRef of the replicaset is not nil and pointing to the deployment
	controllerRef := metav1.GetControllerOf(rs)
	if controllerRef == nil {
		t.Fatalf("controllerRef of replicaset %q is nil", rs.Name)
	}
	if controllerRef.UID != tester.deployment.UID {
		t.Fatalf("controllerRef of replicaset %q has a different UID: Expected %v, got %v", rs.Name, tester.deployment.UID, controllerRef.UID)
	}

	// Change the replicaset's labels to not match the deployment's labels
	labelMap := map[string]string{"new-name": "new-test"}
	rs, err = tester.updateReplicaSet(rs.Name, func(update *apps.ReplicaSet) {
		update.Labels = labelMap
	})
	if err != nil {
		t.Fatalf("failed to update replicaset %q: %v", rs.Name, err)
	}

	// Wait for the controllerRef of the replicaset to become nil
	rsClient := tester.c.AppsV1().ReplicaSets(ns.Name)
	if err = wait.PollImmediate(pollInterval, pollTimeout, func() (bool, error) {
		rs, err = rsClient.Get(context.TODO(), rs.Name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		return metav1.GetControllerOf(rs) == nil, nil
	}); err != nil {
		t.Fatalf("failed to wait for controllerRef of replicaset %q to become nil: %v", rs.Name, err)
	}

	// Wait for the deployment to create a new replicaset
	// This will trigger collision avoidance due to deterministic nature of replicaset name
	// i.e., the new replicaset will have a name with different hash to preserve name uniqueness
	var newRS *apps.ReplicaSet
	if err = wait.PollImmediate(pollInterval, pollTimeout, func() (bool, error) {
		newRS, err = testutil.GetNewReplicaSet(tester.deployment, c)
		if err != nil {
			return false, fmt.Errorf("failed to get new replicaset of deployment %q after orphaning: %v", deploymentName, err)
		}
		return newRS != nil, nil
	}); err != nil {
		t.Fatalf("failed to wait for deployment %q to create a new replicaset after orphaning: %v", deploymentName, err)
	}
	if newRS.UID == rs.UID {
		t.Fatalf("expect deployment %q to create a new replicaset different from the orphaned one, but it isn't", deploymentName)
	}

	// Adoption: deployment should add controllerRef to a RS when the RS's labels change to match its labels

	// Change the old replicaset's labels to match the deployment's labels
	rs, err = tester.updateReplicaSet(rs.Name, func(update *apps.ReplicaSet) {
		update.Labels = testLabels()
	})
	if err != nil {
		t.Fatalf("failed to update replicaset %q: %v", rs.Name, err)
	}

	// Wait for the deployment to adopt the old replicaset
	if err = wait.PollImmediate(pollInterval, pollTimeout, func() (bool, error) {
		rs, err := rsClient.Get(context.TODO(), rs.Name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		controllerRef = metav1.GetControllerOf(rs)
		return controllerRef != nil && controllerRef.UID == tester.deployment.UID, nil
	}); err != nil {
		t.Fatalf("failed waiting for replicaset adoption by deployment %q to complete: %v", deploymentName, err)
	}
}

func TestTerminatingReplicasDeploymentStatus(t *testing.T) {
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.DeploymentPodReplacementPolicy, false)

	_, ctx := ktesting.NewTestContext(t)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	closeFn, rm, dc, informers, c := dcSetup(ctx, t)
	defer closeFn()

	name := "test-terminating-replica-status"
	ns := framework.CreateNamespaceOrDie(c, name, t)
	defer framework.DeleteNamespaceOrDie(c, ns, t)

	deploymentName := "deployment"
	replicas := int32(6)
	tester := &deploymentTester{t: t, c: c, deployment: newDeployment(deploymentName, ns.Name, replicas)}
	tester.deployment.Spec.Strategy.Type = apps.RecreateDeploymentStrategyType
	tester.deployment.Spec.Strategy.RollingUpdate = nil
	tester.deployment.Spec.Template.Spec.NodeName = "fake-node"
	tester.deployment.Spec.Template.Spec.TerminationGracePeriodSeconds = ptr.To(int64(300))

	var err error
	tester.deployment, err = c.AppsV1().Deployments(ns.Name).Create(context.TODO(), tester.deployment, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("failed to create deployment %q: %v", deploymentName, err)
	}

	// Start informer and controllers
	stopControllers := runControllersAndInformers(t, rm, dc, informers)
	defer stopControllers()

	// Ensure the deployment completes while marking its pods as ready simultaneously
	if err := tester.waitForDeploymentCompleteAndMarkPodsReady(); err != nil {
		t.Fatal(err)
	}
	// Should not update terminating replicas when feature gate is disabled
	// Verify all replicas fields of DeploymentStatus have desired counts
	if err = tester.checkDeploymentStatusReplicasFields(6, 6, 6, 6, 0, nil); err != nil {
		t.Fatal(err)
	}

	// Scale down the deployment
	tester.deployment, err = tester.updateDeployment(func(update *apps.Deployment) {
		update.Spec.Replicas = ptr.To(int32(4))
	})
	if err != nil {
		t.Fatalf("failed updating deployment %q: %v", deploymentName, err)
	}
	// Wait for number of ready replicas to equal number of replicas.
	if err = tester.waitForReadyReplicas(); err != nil {
		t.Fatal(err)
	}
	// Verify all replicas fields of DeploymentStatus have desired counts
	if err = tester.checkDeploymentStatusReplicasFields(4, 4, 4, 4, 0, nil); err != nil {
		t.Fatal(err)
	}

	// should update terminating replicas when feature gate is enabled
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.DeploymentPodReplacementPolicy, true)
	// Scale down the deployment
	tester.deployment, err = tester.updateDeployment(func(update *apps.Deployment) {
		update.Spec.Replicas = ptr.To(int32(3))
	})
	if err != nil {
		t.Fatalf("failed updating deployment %q: %v", deploymentName, err)
	}
	// Wait for number of ready replicas to equal number of replicas.
	if err = tester.waitForReadyReplicas(); err != nil {
		t.Fatal(err)
	}
	// Verify all replicas fields of DeploymentStatus have desired counts
	if err = tester.checkDeploymentStatusReplicasFields(3, 3, 3, 3, 0, ptr.To[int32](3)); err != nil {
		t.Fatal(err)
	}

	// should not update terminating replicas when feature gate is disabled
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.DeploymentPodReplacementPolicy, false)
	// Scale down the deployment
	tester.deployment, err = tester.updateDeployment(func(update *apps.Deployment) {
		update.Spec.Replicas = ptr.To(int32(2))
	})
	if err != nil {
		t.Fatalf("failed updating deployment %q: %v", deploymentName, err)
	}
	// Wait for number of ready replicas to equal number of replicas.
	if err = tester.waitForReadyReplicas(); err != nil {
		t.Fatal(err)
	}
	// Verify all replicas fields of DeploymentStatus have desired counts
	if err = tester.checkDeploymentStatusReplicasFields(2, 2, 2, 2, 0, nil); err != nil {
		t.Fatal(err)
	}
}

func TestRecreateDeploymentForPodReplacementPolicy(t *testing.T) {
	tests := []struct {
		name                                                   string
		enableDeploymentPodReplacementPolicy                   bool
		waitForOldTerminatingPods                              bool
		waitForAllTerminatingPods                              bool
		podReplacementPolicy                                   *apps.DeploymentPodReplacementPolicy
		expectedReplicasAfterOldRSScaleDown                    int32
		expectedTerminatingReplicasAfterOldRSScaleDown         *int32
		expectedReplicasAfterNewRS                             int32
		expectedTerminatingReplicasAfterNewRS                  *int32
		expectedReplicasAfterInFlightPodTermination            int32
		expectedTerminatingReplicasAfterInFlightPodTermination *int32
		expectedReplicasAfterInFlightScaleUp                   int32
		expectedTerminatingReplicasAfterInFlightScaleUp        *int32
		expectedReplicasAfterInFlightScaleDown                 int32
		expectedTerminatingReplicasAfterInFlightScaleDown      *int32
		expectedReplicasForDeploymentComplete                  int32
		expectedTerminatingReplicasForDeploymentComplete       *int32
	}{
		{
			name:                                 "recreate should wait for terminating pods to complete in a new rollout with DeploymentPodReplacementPolicy=false",
			enableDeploymentPodReplacementPolicy: false,
			waitForOldTerminatingPods:            true,
			waitForAllTerminatingPods:            false,

			expectedReplicasAfterOldRSScaleDown:                    0,
			expectedTerminatingReplicasAfterOldRSScaleDown:         nil, // terminating counting disabled for all expectedTerminating
			expectedReplicasAfterNewRS:                             6,
			expectedTerminatingReplicasAfterNewRS:                  nil,
			expectedReplicasAfterInFlightPodTermination:            6, // 1 pod terminated
			expectedTerminatingReplicasAfterInFlightPodTermination: nil,
			expectedReplicasAfterInFlightScaleUp:                   7, // +1 scale up
			expectedTerminatingReplicasAfterInFlightScaleUp:        nil,
			expectedReplicasAfterInFlightScaleDown:                 5, // -2 scale down
			expectedTerminatingReplicasAfterInFlightScaleDown:      nil,
			expectedReplicasForDeploymentComplete:                  5,
			expectedTerminatingReplicasForDeploymentComplete:       nil,
		},
		{
			name:                                 "recreate should wait for terminating pods to complete in a new rollout with DeploymentPodReplacementPolicy=true",
			enableDeploymentPodReplacementPolicy: true,
			waitForOldTerminatingPods:            true,
			waitForAllTerminatingPods:            false,

			expectedReplicasAfterOldRSScaleDown:                    0,
			expectedTerminatingReplicasAfterOldRSScaleDown:         ptr.To[int32](6),
			expectedReplicasAfterNewRS:                             6,
			expectedTerminatingReplicasAfterNewRS:                  ptr.To[int32](0),
			expectedReplicasAfterInFlightPodTermination:            6, // 1 pod terminated
			expectedTerminatingReplicasAfterInFlightPodTermination: ptr.To[int32](1),
			expectedReplicasAfterInFlightScaleUp:                   7, // +1 scale up
			expectedTerminatingReplicasAfterInFlightScaleUp:        ptr.To[int32](1),
			expectedReplicasAfterInFlightScaleDown:                 5, // -2 scale down
			expectedTerminatingReplicasAfterInFlightScaleDown:      ptr.To[int32](3),
			expectedReplicasForDeploymentComplete:                  5,
			expectedTerminatingReplicasForDeploymentComplete:       ptr.To[int32](3),
		},
		{
			name:                                 "recreate with TerminationComplete policy should wait for terminating pods to complete in a new rollout",
			enableDeploymentPodReplacementPolicy: true,
			waitForOldTerminatingPods:            true,
			waitForAllTerminatingPods:            true,
			podReplacementPolicy:                 ptr.To(apps.TerminationComplete),

			expectedReplicasAfterOldRSScaleDown:                    0,
			expectedTerminatingReplicasAfterOldRSScaleDown:         ptr.To[int32](6),
			expectedReplicasAfterNewRS:                             6,
			expectedTerminatingReplicasAfterNewRS:                  ptr.To[int32](0),
			expectedReplicasAfterInFlightPodTermination:            6, // 1 pod terminated
			expectedTerminatingReplicasAfterInFlightPodTermination: ptr.To[int32](1),
			expectedReplicasAfterInFlightScaleUp:                   6, // +1 scale up
			expectedTerminatingReplicasAfterInFlightScaleUp:        ptr.To[int32](1),
			expectedReplicasAfterInFlightScaleDown:                 5, // -2 scale down
			expectedTerminatingReplicasAfterInFlightScaleDown:      ptr.To[int32](2),
			expectedReplicasForDeploymentComplete:                  5,
			expectedTerminatingReplicasForDeploymentComplete:       ptr.To[int32](0),
		},
		{
			name:                                 "recreate with TerminationStarted policy should not wait for terminating pods to complete in a new rollout",
			enableDeploymentPodReplacementPolicy: true,
			waitForOldTerminatingPods:            false,
			waitForAllTerminatingPods:            false,
			podReplacementPolicy:                 ptr.To(apps.TerminationStarted),

			expectedReplicasAfterOldRSScaleDown:                    6,
			expectedTerminatingReplicasAfterOldRSScaleDown:         ptr.To[int32](6),
			expectedReplicasAfterNewRS:                             6,
			expectedTerminatingReplicasAfterNewRS:                  ptr.To[int32](6),
			expectedReplicasAfterInFlightPodTermination:            6, // 1 pod terminated
			expectedTerminatingReplicasAfterInFlightPodTermination: ptr.To[int32](7),
			expectedReplicasAfterInFlightScaleUp:                   7, // +1 scale up
			expectedTerminatingReplicasAfterInFlightScaleUp:        ptr.To[int32](7),
			expectedReplicasAfterInFlightScaleDown:                 5, // -2 scale down
			expectedTerminatingReplicasAfterInFlightScaleDown:      ptr.To[int32](9),
			expectedReplicasForDeploymentComplete:                  5,
			expectedTerminatingReplicasForDeploymentComplete:       ptr.To[int32](9),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.DeploymentPodReplacementPolicy, test.enableDeploymentPodReplacementPolicy)

			_, ctx := ktesting.NewTestContext(t)
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()

			closeFn, rm, dc, informers, c := dcSetup(ctx, t)
			defer closeFn()

			name := "test-recreate-deployment-pod-replacement-policy"
			ns := framework.CreateNamespaceOrDie(c, name, t)
			defer framework.DeleteNamespaceOrDie(c, ns, t)

			// Start informer and controllers
			stopControllers := runControllersAndInformers(t, rm, dc, informers)
			defer stopControllers()

			deploymentName := "deployment"
			replicas := int32(6)
			tester := &deploymentTester{t: t, c: c, deployment: newDeployment(deploymentName, ns.Name, replicas)}
			tester.deployment.Spec.Strategy.Type = apps.RecreateDeploymentStrategyType
			tester.deployment.Spec.Strategy.RollingUpdate = nil
			tester.deployment.Spec.PodReplacementPolicy = test.podReplacementPolicy
			tester.deployment.Spec.Template.Spec.NodeName = "fake-node"
			tester.deployment.Spec.Template.Spec.TerminationGracePeriodSeconds = ptr.To(int64(300))

			var err error
			tester.deployment, err = c.AppsV1().Deployments(ns.Name).Create(context.TODO(), tester.deployment, metav1.CreateOptions{})
			if err != nil {
				t.Fatalf("failed to create deployment %q: %v", deploymentName, err)
			}

			// Ensure the deployment completes while marking its pods as ready simultaneously
			if err := tester.waitForDeploymentCompleteAndMarkPodsReady(); err != nil {
				t.Fatal(err)
			}
			// Record current replicaset before starting new rollout
			firstRS, err := tester.expectNewReplicaSet()
			if err != nil {
				t.Fatal(err)
			}

			// trigger a new rollout
			tester.deployment, err = tester.updateDeployment(func(update *apps.Deployment) {
				update.Spec.Template.Spec.Containers[0].Env = append(update.Spec.Template.Spec.Containers[0].Env, v1.EnvVar{Name: "deploy2", Value: "true"})
			})
			if err != nil {
				t.Fatalf("failed updating deployment %q: %v", deploymentName, err)
			}

			// Wait for old replicaset of 1st rollout to have 0 replicas first
			firstRS, err = c.AppsV1().ReplicaSets(ns.Name).Get(context.TODO(), firstRS.Name, metav1.GetOptions{})
			if err != nil {
				t.Fatalf("failed to get replicaset %q: %v", firstRS.Name, err)
			}
			firstRS.Spec.Replicas = ptr.To[int32](0)
			if err = tester.waitRSStable(firstRS); err != nil {
				t.Fatal(err)
			}

			// Verify all replicas fields of DeploymentStatus have desired counts after scale down phase
			expectedReplicas := test.expectedReplicasAfterOldRSScaleDown
			if err = tester.waitForDeploymentStatusReplicasFields(ctx, expectedReplicas, expectedReplicas, 0, 0, expectedReplicas, test.expectedTerminatingReplicasAfterOldRSScaleDown); err != nil {
				t.Fatal(err)
			}

			if test.waitForOldTerminatingPods {
				// Verify that the new rollout won't create new replica set, until the old pods terminate
				if err := tester.expectNoNewReplicaSet(); err != nil {
					t.Fatal(err)
				}
				// remove terminating pods and skip graceful termination of the old RS
				if err := tester.removeRSPods(ctx, firstRS, math.MaxInt, true, 0); err != nil {
					t.Fatal(err)
				}
			}
			// Verify all replicas fields of DeploymentStatus have desired counts after new RS creation
			expectedReplicas = test.expectedReplicasAfterNewRS
			if err = tester.waitForDeploymentStatusReplicasFields(ctx, expectedReplicas, expectedReplicas, 0, 0, expectedReplicas, test.expectedTerminatingReplicasAfterNewRS); err != nil {
				t.Fatal(err)
			}
			// Verify that the new rollout created new replica set
			secondRS, err := tester.expectNewReplicaSet()
			if err != nil {
				t.Fatal(err)
			}

			// start terminating 1 pod
			err = tester.removeRSPods(ctx, secondRS, 1, false, 300)
			if err != nil {
				t.Fatal(err)
			}
			// Verify all replicas fields of DeploymentStatus have desired counts after surprise pod termination
			expectedReplicas = test.expectedReplicasAfterInFlightPodTermination
			if err = tester.waitForDeploymentStatusReplicasFields(ctx, expectedReplicas, expectedReplicas, 0, 0, expectedReplicas, test.expectedTerminatingReplicasAfterInFlightPodTermination); err != nil {
				t.Fatal(err)
			}

			// Scale up during the deployment rollout
			tester.deployment, err = tester.updateDeployment(func(update *apps.Deployment) {
				update.Spec.Replicas = ptr.To[int32](7)
			})
			if err != nil {
				t.Fatalf("failed to update deployment %q: %v", deploymentName, err)
			}

			// Verify all replicas fields of DeploymentStatus have desired counts after in flight scale up
			expectedReplicas = test.expectedReplicasAfterInFlightScaleUp
			if err = tester.waitForDeploymentStatusReplicasFields(ctx, expectedReplicas, expectedReplicas, 0, 0, expectedReplicas, test.expectedTerminatingReplicasAfterInFlightScaleUp); err != nil {
				t.Fatal(err)
			}

			// Scale down during the deployment rollout
			tester.deployment, err = tester.updateDeployment(func(update *apps.Deployment) {
				update.Spec.Replicas = ptr.To[int32](5)
			})
			if err != nil {
				t.Fatalf("failed to update/scale deployment %q: %v", deploymentName, err)
			}

			// Verify all replicas fields of DeploymentStatus have desired counts after in flight scale down
			expectedReplicas = test.expectedReplicasAfterInFlightScaleDown
			if err = tester.waitForDeploymentStatusReplicasFields(ctx, expectedReplicas, expectedReplicas, 0, 0, expectedReplicas, test.expectedTerminatingReplicasAfterInFlightScaleDown); err != nil {
				t.Fatal(err)
			}

			if test.waitForAllTerminatingPods {
				// remove terminating pods and skip graceful termination of the new RS
				if err := tester.removeRSPods(ctx, secondRS, math.MaxInt, true, 0); err != nil {
					t.Fatal(err)
				}
			}
			// Verify all replicas fields of DeploymentStatus have desired counts before the deployment is completed
			expectedReplicas = test.expectedReplicasForDeploymentComplete
			if err = tester.waitForDeploymentStatusReplicasFields(ctx, expectedReplicas, expectedReplicas, 0, 0, expectedReplicas, test.expectedTerminatingReplicasForDeploymentComplete); err != nil {
				t.Fatal(err)
			}

			// Ensure the new deployment completes while marking its pods as ready simultaneously
			if err = tester.waitForDeploymentCompleteAndMarkPodsReady(); err != nil {
				t.Fatal(err)
			}
			// Verify all replicas fields of DeploymentStatus have desired counts after the deployment is completed
			expectedReplicas = test.expectedReplicasForDeploymentComplete
			if err = tester.waitForDeploymentStatusReplicasFields(ctx, expectedReplicas, expectedReplicas, expectedReplicas, expectedReplicas, 0, test.expectedTerminatingReplicasForDeploymentComplete); err != nil {
				t.Fatal(err)
			}

			// remove terminating pods (if there are any) and skip graceful termination of the old RS
			if err := tester.removeRSPods(ctx, firstRS, math.MaxInt, true, 0); err != nil {
				t.Fatal(err)
			}
			// remove terminating pods and skip graceful termination of the new RS
			if err := tester.removeRSPods(ctx, secondRS, math.MaxInt, true, 0); err != nil {
				t.Fatal(err)
			}

			// Verify all replicas fields of DeploymentStatus have desired counts after the deployment is completed and old pods have terminated
			expectedReplicas = test.expectedReplicasForDeploymentComplete
			var expectedFinalTerminatingReplicas *int32
			if test.enableDeploymentPodReplacementPolicy {
				expectedFinalTerminatingReplicas = ptr.To[int32](0)
			}
			if err = tester.waitForDeploymentStatusReplicasFields(ctx, expectedReplicas, expectedReplicas, expectedReplicas, expectedReplicas, 0, expectedFinalTerminatingReplicas); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRollingUpdateAndProportionalScalingForDeploymentPodReplacementPolicy(t *testing.T) {
	tests := []struct {
		name                                 string
		enableDeploymentPodReplacementPolicy bool
		podReplacementPolicy                 *apps.DeploymentPodReplacementPolicy
		terminatingReplicasFirstRS           int32
		terminatingReplicasSecondRS          int32

		expectedFirstRSReplicasDuringNewRollout          int32
		expectedSecondRSReplicasDuringNewRollout         int32
		expectedTerminatingReplicasDuringNewRollout      *int32
		expectedFirstRSReplicasAfterInFlightScaleUp      int32
		expectedSecondRSReplicasAfterInFlightScaleUp     int32
		expectedTerminatingReplicasDuringInFlightScaleUp *int32
		expectedFirstRSAnnotationsAfterInFlightScaleUp   map[string]string
		expectedSecondRSAnnotationsAfterInFlightScaleUp  map[string]string
	}{
		// starts with 100 replicas + 20 maxSurge
		{
			name:                                 "rolling update should not wait for terminating pods with DeploymentPodReplacementPolicy=false",
			enableDeploymentPodReplacementPolicy: false,

			expectedFirstRSReplicasDuringNewRollout:          100,
			expectedSecondRSReplicasDuringNewRollout:         20,
			expectedTerminatingReplicasDuringNewRollout:      nil,
			expectedFirstRSReplicasAfterInFlightScaleUp:      117,
			expectedSecondRSReplicasAfterInFlightScaleUp:     23,
			expectedTerminatingReplicasDuringInFlightScaleUp: nil,
			expectedFirstRSAnnotationsAfterInFlightScaleUp: map[string]string{
				deploymentutil.DesiredReplicasAnnotation: "120",
				deploymentutil.MaxReplicasAnnotation:     "140",
				deploymentutil.RevisionAnnotation:        "1",
			},
			expectedSecondRSAnnotationsAfterInFlightScaleUp: map[string]string{
				deploymentutil.DesiredReplicasAnnotation: "120",
				deploymentutil.MaxReplicasAnnotation:     "140",
				deploymentutil.RevisionAnnotation:        "2",
			},
		},
		{
			name:                                 "rolling update and scaling should not wait for terminating pods with DeploymentPodReplacementPolicy=true",
			enableDeploymentPodReplacementPolicy: true,
			terminatingReplicasFirstRS:           15,
			terminatingReplicasSecondRS:          1,

			expectedFirstRSReplicasDuringNewRollout:          100,
			expectedSecondRSReplicasDuringNewRollout:         20,
			expectedTerminatingReplicasDuringNewRollout:      ptr.To[int32](15),
			expectedFirstRSReplicasAfterInFlightScaleUp:      117,
			expectedSecondRSReplicasAfterInFlightScaleUp:     23,
			expectedTerminatingReplicasDuringInFlightScaleUp: ptr.To[int32](16),
			expectedFirstRSAnnotationsAfterInFlightScaleUp: map[string]string{
				deploymentutil.DesiredReplicasAnnotation: "120",
				deploymentutil.MaxReplicasAnnotation:     "140",
				deploymentutil.RevisionAnnotation:        "1",
			},
			expectedSecondRSAnnotationsAfterInFlightScaleUp: map[string]string{
				deploymentutil.DesiredReplicasAnnotation: "120",
				deploymentutil.MaxReplicasAnnotation:     "140",
				deploymentutil.RevisionAnnotation:        "2",
			},
		},
		{
			name:                                 "rolling update and scaling with TerminationStarted policy should not wait for terminating pods",
			enableDeploymentPodReplacementPolicy: true,
			podReplacementPolicy:                 ptr.To(apps.TerminationStarted),
			terminatingReplicasFirstRS:           15,
			terminatingReplicasSecondRS:          1,

			expectedFirstRSReplicasDuringNewRollout:          100,
			expectedSecondRSReplicasDuringNewRollout:         20,
			expectedTerminatingReplicasDuringNewRollout:      ptr.To[int32](15),
			expectedFirstRSReplicasAfterInFlightScaleUp:      117,
			expectedSecondRSReplicasAfterInFlightScaleUp:     23,
			expectedTerminatingReplicasDuringInFlightScaleUp: ptr.To[int32](16),
			expectedFirstRSAnnotationsAfterInFlightScaleUp: map[string]string{
				deploymentutil.DesiredReplicasAnnotation: "120",
				deploymentutil.MaxReplicasAnnotation:     "140",
				deploymentutil.RevisionAnnotation:        "1",
			},
			expectedSecondRSAnnotationsAfterInFlightScaleUp: map[string]string{
				deploymentutil.DesiredReplicasAnnotation: "120",
				deploymentutil.MaxReplicasAnnotation:     "140",
				deploymentutil.RevisionAnnotation:        "2",
			},
		},
		{
			name:                                 "rolling update and scaling with TerminationComplete policy should wait for terminating pods",
			enableDeploymentPodReplacementPolicy: true,
			podReplacementPolicy:                 ptr.To(apps.TerminationComplete),
			terminatingReplicasFirstRS:           15,
			terminatingReplicasSecondRS:          4,

			expectedFirstRSReplicasDuringNewRollout:          100,
			expectedSecondRSReplicasDuringNewRollout:         5,
			expectedTerminatingReplicasDuringNewRollout:      ptr.To[int32](15),
			expectedFirstRSReplicasAfterInFlightScaleUp:      116,
			expectedSecondRSReplicasAfterInFlightScaleUp:     5,
			expectedTerminatingReplicasDuringInFlightScaleUp: ptr.To[int32](19),
			expectedFirstRSAnnotationsAfterInFlightScaleUp: map[string]string{
				deploymentutil.DesiredReplicasAnnotation:               "100",
				deploymentutil.MaxReplicasAnnotation:                   "120",
				deploymentutil.ReplicaSetReplicasBeforeScaleAnnotation: "100",
				deploymentutil.RevisionAnnotation:                      "1",
			},
			expectedSecondRSAnnotationsAfterInFlightScaleUp: map[string]string{
				deploymentutil.DesiredReplicasAnnotation:               "100",
				deploymentutil.MaxReplicasAnnotation:                   "120",
				deploymentutil.ReplicaSetReplicasBeforeScaleAnnotation: "5",
				deploymentutil.RevisionAnnotation:                      "2",
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.DeploymentPodReplacementPolicy, test.enableDeploymentPodReplacementPolicy)

			_, ctx := ktesting.NewTestContext(t)
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()

			closeFn, rm, dc, informers, c := dcSetup(ctx, t)
			defer closeFn()

			name := "test-proportional-scaling"
			ns := framework.CreateNamespaceOrDie(c, name, t)
			defer framework.DeleteNamespaceOrDie(c, ns, t)

			// Start informer and controllers
			stopControllers := runControllersAndInformers(t, rm, dc, informers)
			defer stopControllers()

			deploymentName := "deployment"
			replicas := int32(100)
			maxSurge := int32(20)
			tester := &deploymentTester{t: t, c: c, deployment: newDeployment(deploymentName, ns.Name, replicas)}
			tester.deployment.Spec.Strategy.RollingUpdate.MaxSurge = ptr.To(intstr.FromInt32(maxSurge))
			tester.deployment.Spec.Strategy.RollingUpdate.MaxUnavailable = ptr.To(intstr.FromInt32(0))
			tester.deployment.Spec.PodReplacementPolicy = test.podReplacementPolicy
			tester.deployment.Spec.Template.Spec.NodeName = "fake-node"
			tester.deployment.Spec.Template.Spec.TerminationGracePeriodSeconds = ptr.To(int64(300))

			var err error
			tester.deployment, err = c.AppsV1().Deployments(ns.Name).Create(context.TODO(), tester.deployment, metav1.CreateOptions{})
			if err != nil {
				t.Fatalf("failed to create deployment %q: %v", deploymentName, err)
			}

			// Ensure the deployment completes while marking its pods as ready simultaneously
			if err := tester.waitForDeploymentCompleteAndMarkPodsReady(); err != nil {
				t.Fatal(err)
			}
			// Record current replicaset before starting new rollout
			firstRS, err := tester.expectNewReplicaSet()
			if err != nil {
				t.Fatal(err)
			}

			// Terminating some replicas
			err = tester.removeRSPods(ctx, firstRS, int(test.terminatingReplicasFirstRS), false, 300)
			if err != nil {
				t.Fatal(err)
			}
			// Check and test deployment completion.
			if test.enableDeploymentPodReplacementPolicy && ptr.Equal(test.podReplacementPolicy, ptr.To(apps.TerminationComplete)) {
				// We cannot wait for a complete deployment because TerminationComplete policy requires that no terminating pods should be present.
				// Wait for the new pods to appear.
				expectedReadyReplicas := test.expectedFirstRSReplicasDuringNewRollout - *test.expectedTerminatingReplicasDuringNewRollout
				if err = tester.waitForDeploymentStatusReplicasFields(ctx, test.expectedFirstRSReplicasDuringNewRollout, test.expectedFirstRSReplicasDuringNewRollout, expectedReadyReplicas, expectedReadyReplicas, *test.expectedTerminatingReplicasDuringNewRollout, test.expectedTerminatingReplicasDuringNewRollout); err != nil {
					t.Fatal(err)
				}
				if err := tester.markUpdatedPodsReadyWithoutComplete(); err != nil {
					t.Fatal(err)
				}
				// Wait for the deployment status to sync.
				if err = tester.waitForDeploymentStatusReplicasFields(ctx, test.expectedFirstRSReplicasDuringNewRollout, test.expectedFirstRSReplicasDuringNewRollout, test.expectedFirstRSReplicasDuringNewRollout, test.expectedFirstRSReplicasDuringNewRollout, 0, test.expectedTerminatingReplicasDuringNewRollout); err != nil {
					t.Fatal(err)
				}
				// Make sure that the deployment is not complete, due to presence of terminating pods.
				isDeploymentComplete, err := tester.deploymentComplete()
				if err != nil {
					t.Fatal(err)
				}
				if isDeploymentComplete {
					t.Fatalf("deployment %q should not be complete when terminating pods are present", deploymentName)
				}
			} else {
				// Ensure the deployment completes while marking its pods as ready simultaneously
				// Terminating pods should not prevent the deployment from become complete.
				if err := tester.waitForDeploymentCompleteAndMarkPodsReady(); err != nil {
					t.Fatal(err)
				}
			}

			// Trigger a new rollout
			tester.deployment, err = tester.updateDeployment(func(update *apps.Deployment) {
				update.Spec.Template.Spec.Containers[0].Env = append(update.Spec.Template.Spec.Containers[0].Env, v1.EnvVar{Name: "deploy2", Value: "true"})
			})
			if err != nil {
				t.Fatalf("failed updating deployment %q: %v", deploymentName, err)
			}
			expectedReplicasDuringNewRollout := test.expectedFirstRSReplicasDuringNewRollout + test.expectedSecondRSReplicasDuringNewRollout
			if err = tester.waitForDeploymentStatusReplicasFields(ctx, expectedReplicasDuringNewRollout, test.expectedSecondRSReplicasDuringNewRollout, test.expectedFirstRSReplicasDuringNewRollout, test.expectedFirstRSReplicasDuringNewRollout, test.expectedSecondRSReplicasDuringNewRollout, test.expectedTerminatingReplicasDuringNewRollout); err != nil {
				t.Fatal(err)
			}

			// Verify that the new rollout created new replica set
			secondRS, err := tester.expectNewReplicaSet()
			if err != nil {
				t.Fatal(err)
			}

			// terminating additional replicas
			err = tester.removeRSPods(ctx, secondRS, int(test.terminatingReplicasSecondRS), false, 300)
			if err != nil {
				t.Fatal(err)
			}
			if err = tester.waitForDeploymentStatusReplicasFields(ctx, expectedReplicasDuringNewRollout, test.expectedSecondRSReplicasDuringNewRollout, test.expectedFirstRSReplicasDuringNewRollout, test.expectedFirstRSReplicasDuringNewRollout, test.expectedSecondRSReplicasDuringNewRollout, test.expectedTerminatingReplicasDuringInFlightScaleUp); err != nil {
				t.Fatal(err)
			}

			// Scale up during the deployment rollout
			tester.deployment, err = tester.updateDeployment(func(update *apps.Deployment) {
				update.Spec.Replicas = ptr.To[int32](120)
			})
			if err != nil {
				t.Fatalf("failed to update/scale deployment %q: %v", deploymentName, err)
			}
			expectedReplicasDuringInFlightScaleUp := test.expectedFirstRSReplicasAfterInFlightScaleUp + test.expectedSecondRSReplicasAfterInFlightScaleUp
			expectedSurgeReplicas := expectedReplicasDuringInFlightScaleUp - test.expectedFirstRSReplicasDuringNewRollout
			if err = tester.waitForDeploymentStatusReplicasFields(ctx, expectedReplicasDuringInFlightScaleUp, test.expectedSecondRSReplicasAfterInFlightScaleUp, test.expectedFirstRSReplicasDuringNewRollout, test.expectedFirstRSReplicasDuringNewRollout, expectedSurgeReplicas, test.expectedTerminatingReplicasDuringInFlightScaleUp); err != nil {
				t.Fatal(err)
			}

			// Check pod count and annotations for all replica sets
			firstRS, err = c.AppsV1().ReplicaSets(ns.Name).Get(context.TODO(), firstRS.Name, metav1.GetOptions{})
			if err != nil {
				t.Fatalf("failed to get replicaset %q: %v", firstRS.Name, err)
			}
			if *(firstRS.Spec.Replicas) != test.expectedFirstRSReplicasAfterInFlightScaleUp {
				t.Fatalf("unexpected first RS .spec.replicas: expect %d, got %d", test.expectedFirstRSReplicasAfterInFlightScaleUp, *(firstRS.Spec.Replicas))
			}
			if !reflect.DeepEqual(test.expectedFirstRSAnnotationsAfterInFlightScaleUp, firstRS.Annotations) {
				t.Fatalf("unexpected %q replica set annotations: %s", firstRS.Name, cmp.Diff(test.expectedFirstRSAnnotationsAfterInFlightScaleUp, firstRS.Annotations))
			}

			secondRS, err = c.AppsV1().ReplicaSets(ns.Name).Get(context.TODO(), secondRS.Name, metav1.GetOptions{})
			if err != nil {
				t.Fatalf("failed to get replicaset %q: %v", secondRS.Name, err)
			}
			if *(secondRS.Spec.Replicas) != test.expectedSecondRSReplicasAfterInFlightScaleUp {
				t.Fatalf("unexpected second RS .spec.replicas: expect %d, got %d", test.expectedSecondRSReplicasAfterInFlightScaleUp, *(secondRS.Spec.Replicas))
			}
			if !reflect.DeepEqual(test.expectedSecondRSAnnotationsAfterInFlightScaleUp, secondRS.Annotations) {
				t.Fatalf("unexpected %q replica set annotations: %s", secondRS.Name, cmp.Diff(test.expectedSecondRSAnnotationsAfterInFlightScaleUp, secondRS.Annotations))
			}

			// Ensure the deployment completes while marking its pods as ready and removing terminated pods simultaneously
			if err := tester.waitForDeploymentCompleteAndMarkPodsReadyAndRemoveTerminated(ctx); err != nil {
				t.Fatal(err)
			}

			// all replica sets' annotations should be up-to-date in the end
			rss := []*apps.ReplicaSet{firstRS, secondRS}
			for idx, curRS := range rss {
				curRS, err = c.AppsV1().ReplicaSets(ns.Name).Get(context.TODO(), curRS.Name, metav1.GetOptions{})
				if err != nil {
					t.Fatalf("failed to get replicaset when checking desired replicas annotation: %v", err)
				}
				expectedFinalAnnotations := map[string]string{
					deploymentutil.DesiredReplicasAnnotation: "120",
					deploymentutil.MaxReplicasAnnotation:     "140",
					deploymentutil.RevisionAnnotation:        fmt.Sprintf("%d", idx+1),
				}
				if !reflect.DeepEqual(expectedFinalAnnotations, curRS.Annotations) {
					t.Fatalf("unexpected %q replica set annotations: %s", curRS.Name, cmp.Diff(expectedFinalAnnotations, curRS.Annotations))
				}
			}

		})
	}
}
