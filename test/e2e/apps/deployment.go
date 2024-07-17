/*
Copyright 2015 The Kubernetes Authors.

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

package apps

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"strings"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/tools/cache"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv1 "k8s.io/api/autoscaling/v1" //Added new
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	unstructuredv1 "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/dump"
	"k8s.io/apimachinery/pkg/util/intstr"
	utilrand "k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	clientset "k8s.io/client-go/kubernetes"
	appsclient "k8s.io/client-go/kubernetes/typed/apps/v1"
	watchtools "k8s.io/client-go/tools/watch"
	"k8s.io/client-go/util/retry"
	appsinternal "k8s.io/kubernetes/pkg/apis/apps"
	deploymentutil "k8s.io/kubernetes/pkg/controller/deployment/util"
	"k8s.io/kubernetes/pkg/features"
	e2efeatures "k8s.io/kubernetes/test/e2e/feature"
	"k8s.io/kubernetes/test/e2e/framework"
	e2edeployment "k8s.io/kubernetes/test/e2e/framework/deployment"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	e2ereplicaset "k8s.io/kubernetes/test/e2e/framework/replicaset"
	e2eresource "k8s.io/kubernetes/test/e2e/framework/resource"
	e2eservice "k8s.io/kubernetes/test/e2e/framework/service"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"
	testutil "k8s.io/kubernetes/test/utils"
	imageutils "k8s.io/kubernetes/test/utils/image"
	admissionapi "k8s.io/pod-security-admission/api"
	"k8s.io/utils/ptr"
)

const (
	poll            = 2 * time.Second
	pollLongTimeout = 5 * time.Minute
	dRetryPeriod    = 2 * time.Second
	dRetryTimeout   = 5 * time.Minute
)

var (
	nilRs *appsv1.ReplicaSet
)

var _ = SIGDescribe("Deployment", func() {
	var ns string
	var c clientset.Interface
	var dc dynamic.Interface

	ginkgo.AfterEach(func(ctx context.Context) {
		failureTrap(ctx, c, ns)
	})

	f := framework.NewDefaultFramework("deployment")
	f.NamespacePodSecurityLevel = admissionapi.LevelBaseline

	ginkgo.BeforeEach(func() {
		c = f.ClientSet
		ns = f.Namespace.Name
		dc = f.DynamicClient
	})

	ginkgo.It("deployment reaping should cascade to its replica sets and pods", func(ctx context.Context) {
		testDeleteDeployment(ctx, f)
	})
	/*
	  Release: v1.12
	  Testname: Deployment RollingUpdate
	  Description: A conformant Kubernetes distribution MUST support the Deployment with RollingUpdate strategy.
	*/
	framework.ConformanceIt("RollingUpdateDeployment should delete old pods and create new ones", func(ctx context.Context) {
		testRollingUpdateDeployment(ctx, f)
	})
	/*
	  Release: v1.12
	  Testname: Deployment Recreate
	  Description: A conformant Kubernetes distribution MUST support the Deployment with Recreate strategy.
	*/
	framework.ConformanceIt("RecreateDeployment should delete old pods and create new ones", func(ctx context.Context) {
		testRecreateDeployment(ctx, f)
	})

	framework.It("RecreateDeployment with TerminationStarted PodReplacementPolicy should delete old pods and create new ones before the old ones terminate", e2efeatures.DeploymentPodReplacementPolicy, f.WithFeatureGate(features.DeploymentPodReplacementPolicy), func(ctx context.Context) {
		testRecreateDeploymentWithTerminationStartedPodReplacementPolicy(ctx, f)
	})
	/*
	  Release: v1.12
	  Testname: Deployment RevisionHistoryLimit
	  Description: A conformant Kubernetes distribution MUST clean up Deployment's ReplicaSets based on
	  the Deployment's `.spec.revisionHistoryLimit`.
	*/
	framework.ConformanceIt("deployment should delete old replica sets", func(ctx context.Context) {
		testDeploymentCleanUpPolicy(ctx, f)
	})
	/*
	  Release: v1.12
	  Testname: Deployment Rollover
	  Description: A conformant Kubernetes distribution MUST support Deployment rollover,
	    i.e. allow arbitrary number of changes to desired state during rolling update
	    before the rollout finishes.
	*/
	framework.ConformanceIt("deployment should support rollover", func(ctx context.Context) {
		testRolloverDeployment(ctx, f)
	})
	ginkgo.It("iterative rollouts should eventually progress", func(ctx context.Context) {
		testIterativeDeployments(ctx, f)
	})
	ginkgo.It("test Deployment ReplicaSet orphaning and adoption regarding controllerRef", func(ctx context.Context) {
		testDeploymentsControllerRef(ctx, f)
	})

	/*
	   Release: v1.21
	   Testname: Deployment, completes the scaling of a Deployment subresource
	   Description: Create a Deployment with a single Pod. The Pod MUST be verified
	   that it is running. The Deployment MUST get and verify the scale subresource count.
	   The Deployment MUST update and verify the scale subresource. The Deployment MUST patch and verify
	   a scale subresource.
	*/
	framework.ConformanceIt("Deployment should have a working scale subresource", func(ctx context.Context) {
		testDeploymentSubresources(ctx, f)
	})
	/*
	  Release: v1.12
	  Testname: Deployment Proportional Scaling
	  Description: A conformant Kubernetes distribution MUST support Deployment
	    proportional scaling, i.e. proportionally scale a Deployment's ReplicaSets
	    when a Deployment is scaled.
	*/
	framework.ConformanceIt("deployment should support proportional scaling", func(ctx context.Context) {
		testProportionalScalingDeployment(ctx, f, false)
	})

	framework.It("deployment should take terminating pods into account during a rollout with TerminationComplete PodReplacementPolicy and should support proportional scaling", e2efeatures.DeploymentPodReplacementPolicy, f.WithFeatureGate(features.DeploymentPodReplacementPolicy), func(ctx context.Context) {
		testProportionalScalingDeployment(ctx, f, true)
	})

	ginkgo.It("should not disrupt a cloud load-balancer's connectivity during rollout", func(ctx context.Context) {
		e2eskipper.SkipUnlessProviderIs("aws", "azure", "gce")
		e2eskipper.SkipIfIPv6("aws")
		nodes, err := e2enode.GetReadySchedulableNodes(ctx, c)
		framework.ExpectNoError(err)
		e2eskipper.SkipUnlessAtLeast(len(nodes.Items), 3, "load-balancer test requires at least 3 schedulable nodes")
		testRollingUpdateDeploymentWithLocalTrafficLoadBalancer(ctx, f)
	})
	// TODO: add tests that cover deployment.Spec.MinReadySeconds once we solved clock-skew issues
	// See https://github.com/kubernetes/kubernetes/issues/29229
	// Add UnavailableReplicas check because ReadyReplicas or UpdatedReplicas might not represent
	// the actual number of pods running successfully if some pods failed to start after update or patch.
	// See issue ##100192

	/*
		Release: v1.20
		Testname: Deployment, completes the lifecycle of a Deployment
		Description: When a Deployment is created it MUST succeed with the required number of replicas.
		It MUST succeed when the Deployment is patched. When scaling the deployment is MUST succeed.
		When fetching and patching the DeploymentStatus it MUST succeed. It MUST succeed when deleting
		the Deployment.
	*/
	framework.ConformanceIt("should run the lifecycle of a Deployment", func(ctx context.Context) {
		one := int64(1)
		two := int64(2)
		deploymentResource := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
		testNamespaceName := f.Namespace.Name
		testDeploymentName := "test-deployment"
		testDeploymentInitialImage := imageutils.GetE2EImage(imageutils.Agnhost)
		testDeploymentPatchImage := imageutils.GetE2EImage(imageutils.Pause)
		testDeploymentUpdateImage := imageutils.GetE2EImage(imageutils.Httpd)
		testDeploymentDefaultReplicas := int32(2)
		testDeploymentMinimumReplicas := int32(1)
		testDeploymentNoReplicas := int32(0)
		testDeploymentAvailableReplicas := int32(0)
		testDeploymentLabels := map[string]string{"test-deployment-static": "true"}
		testDeploymentLabelsFlat := "test-deployment-static=true"
		w := &cache.ListWatch{
			WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
				options.LabelSelector = testDeploymentLabelsFlat
				return f.ClientSet.AppsV1().Deployments(testNamespaceName).Watch(ctx, options)
			},
		}
		deploymentsList, err := f.ClientSet.AppsV1().Deployments("").List(ctx, metav1.ListOptions{LabelSelector: testDeploymentLabelsFlat})
		framework.ExpectNoError(err, "failed to list Deployments")

		ginkgo.By("creating a Deployment")
		testDeployment := e2edeployment.NewDeployment(
			testDeploymentName, testDeploymentDefaultReplicas, testDeploymentLabels,
			testDeploymentName, testDeploymentInitialImage, appsv1.RollingUpdateDeploymentStrategyType)
		testDeployment.ObjectMeta.Labels = map[string]string{"test-deployment-static": "true"}
		testDeployment.Spec.Template.Spec.TerminationGracePeriodSeconds = &one

		_, err = f.ClientSet.AppsV1().Deployments(testNamespaceName).Create(ctx, testDeployment, metav1.CreateOptions{})
		framework.ExpectNoError(err, "failed to create Deployment %v in namespace %v", testDeploymentName, testNamespaceName)

		ginkgo.By("waiting for Deployment to be created")
		ctxUntil, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		_, err = watchtools.Until(ctxUntil, deploymentsList.ResourceVersion, w, func(event watch.Event) (bool, error) {
			switch event.Type {
			case watch.Added:
				if deployment, ok := event.Object.(*appsv1.Deployment); ok {
					found := deployment.ObjectMeta.Name == testDeployment.Name &&
						deployment.ObjectMeta.Labels["test-deployment-static"] == "true"
					return found, nil
				}
			default:
				framework.Logf("observed event type %v", event.Type)
			}
			return false, nil
		})
		framework.ExpectNoError(err, "failed to see %v event", watch.Added)

		ginkgo.By("waiting for all Replicas to be Ready")
		ctxUntil, cancel = context.WithTimeout(ctx, f.Timeouts.PodStart)
		defer cancel()
		_, err = watchtools.Until(ctxUntil, deploymentsList.ResourceVersion, w, func(event watch.Event) (bool, error) {
			if deployment, ok := event.Object.(*appsv1.Deployment); ok {
				found := deployment.ObjectMeta.Name == testDeployment.Name &&
					deployment.ObjectMeta.Labels["test-deployment-static"] == "true" &&
					deployment.Status.ReadyReplicas == testDeploymentDefaultReplicas
				if !found {
					framework.Logf("observed Deployment %v in namespace %v with ReadyReplicas %v and labels %v", deployment.ObjectMeta.Name, deployment.ObjectMeta.Namespace, deployment.Status.ReadyReplicas, deployment.ObjectMeta.Labels)
				}
				framework.Logf("observed Deployment %v in namespace %v with ReadyReplicas %v and labels %v", deployment.ObjectMeta.Name, deployment.ObjectMeta.Namespace, deployment.Status.ReadyReplicas, deployment.ObjectMeta.Labels)
				return found, nil
			}
			return false, nil
		})
		framework.ExpectNoError(err, "failed to see replicas of %v in namespace %v scale to requested amount of %v", testDeployment.Name, testNamespaceName, testDeploymentDefaultReplicas)

		ginkgo.By("patching the Deployment")
		deploymentPatch, err := json.Marshal(map[string]interface{}{
			"metadata": map[string]interface{}{
				"labels": map[string]string{"test-deployment": "patched"},
			},
			"spec": map[string]interface{}{
				"replicas": testDeploymentMinimumReplicas,
				"template": map[string]interface{}{
					"spec": map[string]interface{}{
						"terminationGracePeriodSeconds": &two,
						"containers": [1]map[string]interface{}{{
							"name":  testDeploymentName,
							"image": testDeploymentPatchImage,
						}},
					},
				},
			},
		})
		framework.ExpectNoError(err, "failed to Marshal Deployment JSON patch")
		_, err = f.ClientSet.AppsV1().Deployments(testNamespaceName).Patch(ctx, testDeploymentName, types.StrategicMergePatchType, []byte(deploymentPatch), metav1.PatchOptions{})
		framework.ExpectNoError(err, "failed to patch Deployment")
		ctxUntil, cancel = context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		_, err = watchtools.Until(ctxUntil, deploymentsList.ResourceVersion, w, func(event watch.Event) (bool, error) {
			switch event.Type {
			case watch.Modified:
				if deployment, ok := event.Object.(*appsv1.Deployment); ok {
					found := deployment.ObjectMeta.Name == testDeployment.Name &&
						deployment.ObjectMeta.Labels["test-deployment-static"] == "true"
					if !found {
						framework.Logf("observed Deployment %v in namespace %v with ReadyReplicas %v", deployment.ObjectMeta.Name, deployment.ObjectMeta.Namespace, deployment.Status.ReadyReplicas)
					}
					return found, nil
				}
			default:
				framework.Logf("observed event type %v", event.Type)
			}
			return false, nil
		})
		framework.ExpectNoError(err, "failed to see %v event", watch.Modified)

		ginkgo.By("waiting for Replicas to scale")
		ctxUntil, cancel = context.WithTimeout(ctx, f.Timeouts.PodStart)
		defer cancel()
		_, err = watchtools.Until(ctxUntil, deploymentsList.ResourceVersion, w, func(event watch.Event) (bool, error) {
			if deployment, ok := event.Object.(*appsv1.Deployment); ok {
				found := deployment.ObjectMeta.Name == testDeployment.Name &&
					deployment.ObjectMeta.Labels["test-deployment-static"] == "true" &&
					deployment.Status.ReadyReplicas == testDeploymentMinimumReplicas &&
					deployment.Status.UpdatedReplicas == testDeploymentMinimumReplicas &&
					deployment.Status.UnavailableReplicas == 0 &&
					deployment.Spec.Template.Spec.Containers[0].Image == testDeploymentPatchImage &&
					*deployment.Spec.Template.Spec.TerminationGracePeriodSeconds == two
				if !found {
					framework.Logf("observed Deployment %v in namespace %v with ReadyReplicas %v", deployment.ObjectMeta.Name, deployment.ObjectMeta.Namespace, deployment.Status.ReadyReplicas)
				}
				framework.Logf("observed Deployment %v in namespace %v with ReadyReplicas %v", deployment.ObjectMeta.Name, deployment.ObjectMeta.Namespace, deployment.Status.ReadyReplicas)
				return found, nil
			}
			return false, nil
		})
		framework.ExpectNoError(err, "failed to see replicas of %v in namespace %v scale to requested amount of %v", testDeployment.Name, testNamespaceName, testDeploymentMinimumReplicas)

		ginkgo.By("listing Deployments")
		deploymentsList, err = f.ClientSet.AppsV1().Deployments("").List(ctx, metav1.ListOptions{LabelSelector: testDeploymentLabelsFlat})
		framework.ExpectNoError(err, "failed to list Deployments")
		foundDeployment := false
		for _, deploymentItem := range deploymentsList.Items {
			if deploymentItem.ObjectMeta.Name == testDeploymentName &&
				deploymentItem.ObjectMeta.Namespace == testNamespaceName &&
				deploymentItem.ObjectMeta.Labels["test-deployment-static"] == "true" {
				foundDeployment = true
				framework.Logf("Found %v with labels: %v", deploymentItem.ObjectMeta.Name, deploymentItem.ObjectMeta.Labels)
				break
			}
		}
		if !foundDeployment {
			framework.Failf("unable to find the Deployment in the following list %v", deploymentsList)
		}

		ginkgo.By("updating the Deployment")
		testDeploymentUpdate := testDeployment
		testDeploymentUpdate.ObjectMeta.Labels["test-deployment"] = "updated"
		testDeploymentUpdate.Spec.Template.Spec.Containers[0].Image = testDeploymentUpdateImage
		testDeploymentDefaultReplicasPointer := &testDeploymentDefaultReplicas
		testDeploymentUpdate.Spec.Replicas = testDeploymentDefaultReplicasPointer
		testDeploymentUpdateUnstructuredMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&testDeploymentUpdate)
		framework.ExpectNoError(err, "failed to convert to unstructured")
		testDeploymentUpdateUnstructured := unstructuredv1.Unstructured{
			Object: testDeploymentUpdateUnstructuredMap,
		}
		// currently this hasn't been able to hit the endpoint replaceAppsV1NamespacedDeploymentStatus
		_, err = dc.Resource(deploymentResource).Namespace(testNamespaceName).Update(ctx, &testDeploymentUpdateUnstructured, metav1.UpdateOptions{}) //, "status")
		framework.ExpectNoError(err, "failed to update the DeploymentStatus")
		ctxUntil, cancel = context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		_, err = watchtools.Until(ctxUntil, deploymentsList.ResourceVersion, w, func(event watch.Event) (bool, error) {
			switch event.Type {
			case watch.Modified:
				if deployment, ok := event.Object.(*appsv1.Deployment); ok {
					found := deployment.ObjectMeta.Name == testDeployment.Name &&
						deployment.ObjectMeta.Labels["test-deployment-static"] == "true"
					if !found {
						framework.Logf("observed Deployment %v in namespace %v with ReadyReplicas %v", deployment.ObjectMeta.Name, deployment.ObjectMeta.Namespace, deployment.Status.ReadyReplicas)
					}
					framework.Logf("observed Deployment %v in namespace %v with ReadyReplicas %v", deployment.ObjectMeta.Name, deployment.ObjectMeta.Namespace, deployment.Status.ReadyReplicas)
					return found, nil
				}
			default:
				framework.Logf("observed event type %v", event.Type)
			}
			return false, nil
		})
		framework.ExpectNoError(err, "failed to see %v event", watch.Modified)

		ginkgo.By("fetching the DeploymentStatus")
		deploymentGetUnstructured, err := dc.Resource(deploymentResource).Namespace(testNamespaceName).Get(ctx, testDeploymentName, metav1.GetOptions{}, "status")
		framework.ExpectNoError(err, "failed to fetch the Deployment")
		deploymentGet := appsv1.Deployment{}
		err = runtime.DefaultUnstructuredConverter.FromUnstructured(deploymentGetUnstructured.Object, &deploymentGet)
		framework.ExpectNoError(err, "failed to convert the unstructured response to a Deployment")
		gomega.Expect(deploymentGet.Spec.Template.Spec.Containers[0].Image).To(gomega.Equal(testDeploymentUpdateImage), "failed to update image")
		gomega.Expect(deploymentGet.ObjectMeta.Labels).To(gomega.HaveKeyWithValue("test-deployment", "updated"), "failed to update labels")

		ctxUntil, cancel = context.WithTimeout(ctx, f.Timeouts.PodStart)
		defer cancel()
		_, err = watchtools.Until(ctxUntil, deploymentsList.ResourceVersion, w, func(event watch.Event) (bool, error) {
			if deployment, ok := event.Object.(*appsv1.Deployment); ok {
				found := deployment.ObjectMeta.Name == testDeployment.Name &&
					deployment.ObjectMeta.Labels["test-deployment-static"] == "true" &&
					deployment.Status.ReadyReplicas == testDeploymentDefaultReplicas &&
					deployment.Status.UpdatedReplicas == testDeploymentDefaultReplicas &&
					deployment.Status.UnavailableReplicas == 0
				if !found {
					framework.Logf("observed Deployment %v in namespace %v with ReadyReplicas %v and labels %v", deployment.ObjectMeta.Name, deployment.ObjectMeta.Namespace, deployment.Status.ReadyReplicas, deployment.ObjectMeta.Labels)
				}
				return found, nil
			}
			return false, nil
		})
		framework.ExpectNoError(err, "failed to see replicas of %v in namespace %v scale to requested amount of %v", testDeployment.Name, testNamespaceName, testDeploymentDefaultReplicas)

		ginkgo.By("patching the DeploymentStatus")
		deploymentStatusPatch, err := json.Marshal(map[string]interface{}{
			"status": map[string]interface{}{
				"readyReplicas":     testDeploymentNoReplicas,
				"availableReplicas": testDeploymentAvailableReplicas,
			},
		})
		framework.ExpectNoError(err, "failed to Marshal Deployment JSON patch")

		_, err = dc.Resource(deploymentResource).Namespace(testNamespaceName).Patch(ctx, testDeploymentName, types.StrategicMergePatchType, []byte(deploymentStatusPatch), metav1.PatchOptions{}, "status")
		framework.ExpectNoError(err)

		ctxUntil, cancel = context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		_, err = watchtools.Until(ctxUntil, deploymentsList.ResourceVersion, w, func(event watch.Event) (bool, error) {
			switch event.Type {
			case watch.Modified:
				if deployment, ok := event.Object.(*appsv1.Deployment); ok {
					found := deployment.ObjectMeta.Name == testDeployment.Name &&
						deployment.Status.ReadyReplicas == testDeploymentNoReplicas &&
						deployment.Status.AvailableReplicas == testDeploymentAvailableReplicas

					return found, nil
				}
			default:
				framework.Logf("observed event type %v", event.Type)
			}
			return false, nil
		})
		framework.ExpectNoError(err, "failed to see %v event", watch.Modified)

		ginkgo.By("fetching the DeploymentStatus")
		deploymentGetUnstructured, err = dc.Resource(deploymentResource).Namespace(testNamespaceName).Get(ctx, testDeploymentName, metav1.GetOptions{}, "status")
		framework.ExpectNoError(err, "failed to fetch the DeploymentStatus")
		deploymentGet = appsv1.Deployment{}
		err = runtime.DefaultUnstructuredConverter.FromUnstructured(deploymentGetUnstructured.Object, &deploymentGet)
		framework.ExpectNoError(err, "failed to convert the unstructured response to a Deployment")
		gomega.Expect(deploymentGet.Spec.Template.Spec.Containers[0].Image).To(gomega.Equal(testDeploymentUpdateImage), "failed to update image")
		gomega.Expect(deploymentGet.ObjectMeta.Labels).To(gomega.HaveKeyWithValue("test-deployment", "updated"), "failed to update labels")

		ctxUntil, cancel = context.WithTimeout(ctx, f.Timeouts.PodStart)
		defer cancel()
		_, err = watchtools.Until(ctxUntil, deploymentsList.ResourceVersion, w, func(event watch.Event) (bool, error) {
			if deployment, ok := event.Object.(*appsv1.Deployment); ok {
				found := deployment.ObjectMeta.Name == testDeployment.Name &&
					deployment.ObjectMeta.Labels["test-deployment-static"] == "true" &&
					deployment.Status.ReadyReplicas == testDeploymentDefaultReplicas &&
					deployment.Status.UpdatedReplicas == testDeploymentDefaultReplicas &&
					deployment.Status.UnavailableReplicas == 0 &&
					deployment.Spec.Template.Spec.Containers[0].Image == testDeploymentUpdateImage
				if !found {
					framework.Logf("observed Deployment %v in namespace %v with ReadyReplicas %v", deployment.ObjectMeta.Name, deployment.ObjectMeta.Namespace, deployment.Status.ReadyReplicas)
				}
				return found, nil
			}
			return false, nil
		})
		framework.ExpectNoError(err, "failed to see replicas of %v in namespace %v scale to requested amount of %v", testDeployment.Name, testNamespaceName, testDeploymentDefaultReplicas)

		ginkgo.By("deleting the Deployment")
		err = f.ClientSet.AppsV1().Deployments(testNamespaceName).DeleteCollection(ctx, metav1.DeleteOptions{GracePeriodSeconds: &one}, metav1.ListOptions{LabelSelector: testDeploymentLabelsFlat})
		framework.ExpectNoError(err, "failed to delete Deployment via collection")

		ctxUntil, cancel = context.WithTimeout(ctx, 1*time.Minute)
		defer cancel()
		_, err = watchtools.Until(ctxUntil, deploymentsList.ResourceVersion, w, func(event watch.Event) (bool, error) {
			switch event.Type {
			case watch.Deleted:
				if deployment, ok := event.Object.(*appsv1.Deployment); ok {
					found := deployment.ObjectMeta.Name == testDeployment.Name &&
						deployment.ObjectMeta.Labels["test-deployment-static"] == "true"
					if !found {
						framework.Logf("observed Deployment %v in namespace %v with ReadyReplicas %v", deployment.ObjectMeta.Name, deployment.ObjectMeta.Namespace, deployment.Status.ReadyReplicas)
					}
					return found, nil
				}
			default:
				framework.Logf("observed event type %v", event.Type)
			}
			return false, nil
		})
		framework.ExpectNoError(err, "failed to see %v event", watch.Deleted)
	})

	/*
		Release: v1.22
		Testname: Deployment, status sub-resource
		Description: When a Deployment is created it MUST succeed.
		Attempt to read, update and patch its status sub-resource; all
		mutating sub-resource operations MUST be visible to subsequent reads.
	*/
	framework.ConformanceIt("should validate Deployment Status endpoints", func(ctx context.Context) {
		dClient := c.AppsV1().Deployments(ns)
		dName := "test-deployment-" + utilrand.String(5)
		labelSelector := "e2e=testing"

		w := &cache.ListWatch{
			WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
				options.LabelSelector = labelSelector
				return dClient.Watch(ctx, options)
			},
		}
		dList, err := c.AppsV1().Deployments("").List(ctx, metav1.ListOptions{LabelSelector: labelSelector})
		framework.ExpectNoError(err, "failed to list Deployments")

		ginkgo.By("creating a Deployment")

		podLabels := map[string]string{"name": WebserverImageName, "e2e": "testing"}
		replicas := int32(1)
		framework.Logf("Creating simple deployment %s", dName)
		d := e2edeployment.NewDeployment(dName, replicas, podLabels, WebserverImageName, WebserverImage, appsv1.RollingUpdateDeploymentStrategyType)
		deploy, err := c.AppsV1().Deployments(ns).Create(ctx, d, metav1.CreateOptions{})
		framework.ExpectNoError(err)

		// Wait for it to be updated to revision 1
		err = e2edeployment.WaitForDeploymentRevisionAndImage(c, ns, dName, "1", WebserverImage)
		framework.ExpectNoError(err)

		err = e2edeployment.WaitForDeploymentComplete(c, deploy)
		framework.ExpectNoError(err)

		testDeployment, err := dClient.Get(ctx, dName, metav1.GetOptions{})
		framework.ExpectNoError(err)

		ginkgo.By("Getting /status")
		dResource := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
		dStatusUnstructured, err := f.DynamicClient.Resource(dResource).Namespace(ns).Get(ctx, dName, metav1.GetOptions{}, "status")
		framework.ExpectNoError(err, "Failed to fetch the status of deployment %s in namespace %s", dName, ns)
		dStatusBytes, err := json.Marshal(dStatusUnstructured)
		framework.ExpectNoError(err, "Failed to marshal unstructured response. %v", err)

		var dStatus appsv1.Deployment
		err = json.Unmarshal(dStatusBytes, &dStatus)
		framework.ExpectNoError(err, "Failed to unmarshal JSON bytes to a deployment object type")
		framework.Logf("Deployment %s has Conditions: %v", dName, dStatus.Status.Conditions)

		ginkgo.By("updating Deployment Status")
		var statusToUpdate, updatedStatus *appsv1.Deployment

		err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
			statusToUpdate, err = dClient.Get(ctx, dName, metav1.GetOptions{})
			framework.ExpectNoError(err, "Unable to retrieve deployment %s", dName)

			statusToUpdate.Status.Conditions = append(statusToUpdate.Status.Conditions, appsv1.DeploymentCondition{
				Type:    "StatusUpdate",
				Status:  "True",
				Reason:  "E2E",
				Message: "Set from e2e test",
			})

			updatedStatus, err = dClient.UpdateStatus(ctx, statusToUpdate, metav1.UpdateOptions{})
			return err
		})
		framework.ExpectNoError(err, "Failed to update status. %v", err)
		framework.Logf("updatedStatus.Conditions: %#v", updatedStatus.Status.Conditions)

		ginkgo.By("watching for the Deployment status to be updated")
		ctxUntil, cancel := context.WithTimeout(ctx, dRetryTimeout)
		defer cancel()

		_, err = watchtools.Until(ctxUntil, dList.ResourceVersion, w, func(event watch.Event) (bool, error) {
			if d, ok := event.Object.(*appsv1.Deployment); ok {
				found := d.ObjectMeta.Name == testDeployment.ObjectMeta.Name &&
					d.ObjectMeta.Namespace == testDeployment.ObjectMeta.Namespace &&
					d.Labels["e2e"] == "testing"

				if !found {
					framework.Logf("Observed Deployment %v in namespace %v with annotations: %v & Conditions: %v\n", d.ObjectMeta.Name, d.ObjectMeta.Namespace, d.Annotations, d.Status.Conditions)
					return false, nil
				}
				for _, cond := range d.Status.Conditions {
					if cond.Type == "StatusUpdate" &&
						cond.Reason == "E2E" &&
						cond.Message == "Set from e2e test" {
						framework.Logf("Found Deployment %v in namespace %v with labels: %v annotations: %v & Conditions: %v", d.ObjectMeta.Name, d.ObjectMeta.Namespace, d.ObjectMeta.Labels, d.Annotations, cond)
						return found, nil
					}
					framework.Logf("Observed Deployment %v in namespace %v with annotations: %v & Conditions: %v", d.ObjectMeta.Name, d.ObjectMeta.Namespace, d.Annotations, cond)
				}
			}
			object := strings.Split(fmt.Sprintf("%v", event.Object), "{")[0]
			framework.Logf("Observed %v event: %+v", object, event.Type)
			return false, nil
		})
		framework.ExpectNoError(err, "failed to locate Deployment %v in namespace %v", testDeployment.ObjectMeta.Name, ns)
		framework.Logf("Deployment %s has an updated status", dName)

		ginkgo.By("patching the Statefulset Status")
		payload := []byte(`{"status":{"conditions":[{"type":"StatusPatched","status":"True"}]}}`)
		framework.Logf("Patch payload: %v", string(payload))

		patchedDeployment, err := dClient.Patch(ctx, dName, types.MergePatchType, payload, metav1.PatchOptions{}, "status")
		framework.ExpectNoError(err, "Failed to patch status. %v", err)
		framework.Logf("Patched status conditions: %#v", patchedDeployment.Status.Conditions)

		ginkgo.By("watching for the Deployment status to be patched")
		ctxUntil, cancel = context.WithTimeout(ctx, dRetryTimeout)
		defer cancel()

		_, err = watchtools.Until(ctxUntil, dList.ResourceVersion, w, func(event watch.Event) (bool, error) {

			if e, ok := event.Object.(*appsv1.Deployment); ok {
				found := e.ObjectMeta.Name == testDeployment.ObjectMeta.Name &&
					e.ObjectMeta.Namespace == testDeployment.ObjectMeta.Namespace &&
					e.ObjectMeta.Labels["e2e"] == testDeployment.ObjectMeta.Labels["e2e"]
				if !found {
					framework.Logf("Observed deployment %v in namespace %v with annotations: %v & Conditions: %v", testDeployment.ObjectMeta.Name, testDeployment.ObjectMeta.Namespace, testDeployment.Annotations, testDeployment.Status.Conditions)
					return false, nil
				}
				for _, cond := range e.Status.Conditions {
					if cond.Type == "StatusPatched" {
						framework.Logf("Found deployment %v in namespace %v with labels: %v annotations: %v & Conditions: %v", testDeployment.ObjectMeta.Name, testDeployment.ObjectMeta.Namespace, testDeployment.ObjectMeta.Labels, testDeployment.Annotations, cond)
						return found, nil
					}
					framework.Logf("Observed deployment %v in namespace %v with annotations: %v & Conditions: %v", testDeployment.ObjectMeta.Name, testDeployment.ObjectMeta.Namespace, testDeployment.Annotations, cond)
				}
			}
			object := strings.Split(fmt.Sprintf("%v", event.Object), "{")[0]
			framework.Logf("Observed %v event: %+v", object, event.Type)
			return false, nil
		})
		framework.ExpectNoError(err, "failed to locate deployment %v in namespace %v", testDeployment.ObjectMeta.Name, ns)
		framework.Logf("Deployment %s has a patched status", dName)
	})
})

func failureTrap(ctx context.Context, c clientset.Interface, ns string) {
	deployments, err := c.AppsV1().Deployments(ns).List(ctx, metav1.ListOptions{LabelSelector: labels.Everything().String()})
	if err != nil {
		framework.Logf("Could not list Deployments in namespace %q: %v", ns, err)
		return
	}
	for i := range deployments.Items {
		d := deployments.Items[i]

		framework.Logf("Deployment %q:\n%s\n", d.Name, dump.Pretty(d))
		_, allOldRSs, newRS, err := testutil.GetAllReplicaSets(&d, c)
		if err != nil {
			framework.Logf("Could not list ReplicaSets for Deployment %q: %v", d.Name, err)
			return
		}
		testutil.LogReplicaSetsOfDeployment(&d, allOldRSs, newRS, framework.Logf)
		rsList := allOldRSs
		if newRS != nil {
			rsList = append(rsList, newRS)
		}
		testutil.LogPodsOfDeployment(c, &d, rsList, framework.Logf)
	}
	// We need print all the ReplicaSets if there are no Deployment object created
	if len(deployments.Items) != 0 {
		return
	}
	framework.Logf("Log out all the ReplicaSets if there is no deployment created")
	rss, err := c.AppsV1().ReplicaSets(ns).List(ctx, metav1.ListOptions{LabelSelector: labels.Everything().String()})
	if err != nil {
		framework.Logf("Could not list ReplicaSets in namespace %q: %v", ns, err)
		return
	}
	for _, rs := range rss.Items {
		framework.Logf("ReplicaSet %q:\n%s\n", rs.Name, dump.Pretty(rs))
		selector, err := metav1.LabelSelectorAsSelector(rs.Spec.Selector)
		if err != nil {
			framework.Logf("failed to get selector of ReplicaSet %s: %v", rs.Name, err)
		}
		options := metav1.ListOptions{LabelSelector: selector.String()}
		podList, err := c.CoreV1().Pods(rs.Namespace).List(ctx, options)
		if err != nil {
			framework.Logf("Failed to list Pods in namespace %s: %v", rs.Namespace, err)
			continue
		}
		for _, pod := range podList.Items {
			framework.Logf("pod: %q:\n%s\n", pod.Name, dump.Pretty(pod))
		}
	}
}

func stopDeployment(ctx context.Context, c clientset.Interface, ns, deploymentName string) {
	deployment, err := c.AppsV1().Deployments(ns).Get(ctx, deploymentName, metav1.GetOptions{})
	framework.ExpectNoError(err)

	framework.Logf("Deleting deployment %s", deploymentName)
	err = e2eresource.DeleteResourceAndWaitForGC(ctx, c, appsinternal.Kind("Deployment"), ns, deployment.Name)
	framework.ExpectNoError(err)

	framework.Logf("Ensuring deployment %s was deleted", deploymentName)
	_, err = c.AppsV1().Deployments(ns).Get(ctx, deployment.Name, metav1.GetOptions{})
	gomega.Expect(err).To(gomega.MatchError(apierrors.IsNotFound, fmt.Sprintf("Expected deployment %s to be deleted", deploymentName)))
	framework.Logf("Ensuring deployment %s's RSes were deleted", deploymentName)
	selector, err := metav1.LabelSelectorAsSelector(deployment.Spec.Selector)
	framework.ExpectNoError(err)
	options := metav1.ListOptions{LabelSelector: selector.String()}
	rss, err := c.AppsV1().ReplicaSets(ns).List(ctx, options)
	framework.ExpectNoError(err)
	gomega.Expect(rss.Items).Should(gomega.BeEmpty())
	framework.Logf("Ensuring deployment %s's Pods were deleted", deploymentName)
	var pods *v1.PodList
	if err := wait.PollImmediate(time.Second, timeout, func() (bool, error) {
		pods, err = c.CoreV1().Pods(ns).List(ctx, options)
		if err != nil {
			return false, err
		}
		// Pods may be created by overlapping deployments right after this deployment is deleted, ignore them
		if len(pods.Items) == 0 {
			return true, nil
		}
		return false, nil
	}); err != nil {
		framework.Failf("Err : %s\n. Failed to remove deployment %s pods : %+v", err, deploymentName, pods)
	}
}

func testDeleteDeployment(ctx context.Context, f *framework.Framework) {
	ns := f.Namespace.Name
	c := f.ClientSet

	deploymentName := "test-new-deployment"
	podLabels := map[string]string{"name": WebserverImageName}
	replicas := int32(1)
	framework.Logf("Creating simple deployment %s", deploymentName)
	d := e2edeployment.NewDeployment(deploymentName, replicas, podLabels, WebserverImageName, WebserverImage, appsv1.RollingUpdateDeploymentStrategyType)
	d.Annotations = map[string]string{"test": "should-copy-to-replica-set", v1.LastAppliedConfigAnnotation: "should-not-copy-to-replica-set"}
	deploy, err := c.AppsV1().Deployments(ns).Create(ctx, d, metav1.CreateOptions{})
	framework.ExpectNoError(err)

	// Wait for it to be updated to revision 1
	err = e2edeployment.WaitForDeploymentRevisionAndImage(c, ns, deploymentName, "1", WebserverImage)
	framework.ExpectNoError(err)

	err = e2edeployment.WaitForDeploymentComplete(c, deploy)
	framework.ExpectNoError(err)

	deployment, err := c.AppsV1().Deployments(ns).Get(ctx, deploymentName, metav1.GetOptions{})
	framework.ExpectNoError(err)
	newRS, err := testutil.GetNewReplicaSet(deployment, c)
	framework.ExpectNoError(err)
	gomega.Expect(newRS).NotTo(gomega.Equal(nilRs))
	stopDeployment(ctx, c, ns, deploymentName)
}

func testRollingUpdateDeployment(ctx context.Context, f *framework.Framework) {
	ns := f.Namespace.Name
	c := f.ClientSet
	// Create webserver pods.
	deploymentPodLabels := map[string]string{"name": "sample-pod"}
	rsPodLabels := map[string]string{
		"name": "sample-pod",
		"pod":  WebserverImageName,
	}

	rsName := "test-rolling-update-controller"
	replicas := int32(1)
	rsRevision := "3546343826724305832"
	annotations := make(map[string]string)
	annotations[deploymentutil.RevisionAnnotation] = rsRevision
	rs := newRS(rsName, replicas, rsPodLabels, WebserverImageName, WebserverImage, nil)
	rs.Annotations = annotations
	framework.Logf("Creating replica set %q (going to be adopted)", rs.Name)
	_, err := c.AppsV1().ReplicaSets(ns).Create(ctx, rs, metav1.CreateOptions{})
	framework.ExpectNoError(err)
	// Verify that the required pods have come up.
	err = e2epod.VerifyPodsRunning(ctx, c, ns, "sample-pod", false, replicas)
	framework.ExpectNoError(err, "error in waiting for pods to come up: %s", err)

	// Create a deployment to delete webserver pods and instead bring up agnhost pods.
	deploymentName := "test-rolling-update-deployment"
	framework.Logf("Creating deployment %q", deploymentName)
	d := e2edeployment.NewDeployment(deploymentName, replicas, deploymentPodLabels, AgnhostImageName, AgnhostImage, appsv1.RollingUpdateDeploymentStrategyType)
	deploy, err := c.AppsV1().Deployments(ns).Create(ctx, d, metav1.CreateOptions{})
	framework.ExpectNoError(err)

	// Wait for it to be updated to revision 3546343826724305833.
	framework.Logf("Ensuring deployment %q gets the next revision from the one the adopted replica set %q has", deploy.Name, rs.Name)
	err = e2edeployment.WaitForDeploymentRevisionAndImage(c, ns, deploymentName, "3546343826724305833", AgnhostImage)
	framework.ExpectNoError(err)

	framework.Logf("Ensuring status for deployment %q is the expected", deploy.Name)
	err = e2edeployment.WaitForDeploymentComplete(c, deploy)
	framework.ExpectNoError(err)

	// There should be 1 old RS (webserver-controller, which is adopted)
	framework.Logf("Ensuring deployment %q has one old replica set (the one it adopted)", deploy.Name)
	deployment, err := c.AppsV1().Deployments(ns).Get(ctx, deploymentName, metav1.GetOptions{})
	framework.ExpectNoError(err)
	_, allOldRSs, err := testutil.GetOldReplicaSets(deployment, c)
	framework.ExpectNoError(err)
	gomega.Expect(allOldRSs).To(gomega.HaveLen(1))
}

func testRecreateDeployment(ctx context.Context, f *framework.Framework) {
	ns := f.Namespace.Name
	c := f.ClientSet

	// Create a deployment that brings up agnhost pods.
	deploymentName := "test-recreate-deployment"
	framework.Logf("Creating deployment %q", deploymentName)
	d := e2edeployment.NewDeployment(deploymentName, int32(1), map[string]string{"name": "sample-pod-3"}, AgnhostImageName, AgnhostImage, appsv1.RecreateDeploymentStrategyType)
	deployment, err := c.AppsV1().Deployments(ns).Create(ctx, d, metav1.CreateOptions{})
	framework.ExpectNoError(err)

	// Wait for it to be updated to revision 1
	framework.Logf("Waiting deployment %q to be updated to revision 1", deploymentName)
	err = e2edeployment.WaitForDeploymentRevisionAndImage(c, ns, deploymentName, "1", AgnhostImage)
	framework.ExpectNoError(err)

	framework.Logf("Waiting deployment %q to complete", deploymentName)
	err = e2edeployment.WaitForDeploymentComplete(c, deployment)
	framework.ExpectNoError(err)

	// Update deployment to delete agnhost pods and bring up webserver pods.
	framework.Logf("Triggering a new rollout for deployment %q", deploymentName)
	deployment, err = e2edeployment.UpdateDeploymentWithRetries(c, ns, deploymentName, func(update *appsv1.Deployment) {
		update.Spec.Template.Spec.Containers[0].Name = WebserverImageName
		update.Spec.Template.Spec.Containers[0].Image = WebserverImage
	})
	framework.ExpectNoError(err)

	framework.Logf("Watching deployment %q to verify that new pods will not run with olds pods", deploymentName)
	err = watchRecreateDeployment(ctx, c, deployment)
	framework.ExpectNoError(err)
}

func testRecreateDeploymentWithTerminationStartedPodReplacementPolicy(ctx context.Context, f *framework.Framework) {
	ns := f.Namespace.Name
	c := f.ClientSet

	// Create a deployment that brings up agnhost pods.
	deploymentName := "test-recreate-deployment-with-termination-complete-policy"
	framework.Logf("Creating deployment %q", deploymentName)
	replicas := int32(2)
	d := e2edeployment.NewDeployment(deploymentName, replicas, map[string]string{"name": "sample-pod-3"}, AgnhostImageName, AgnhostImage, appsv1.RecreateDeploymentStrategyType)
	d.Spec.Template.Spec.Containers[0].Lifecycle = &v1.Lifecycle{
		PreStop: &v1.LifecycleHandler{
			Sleep: &v1.SleepAction{
				Seconds: int64(framework.PodStartTimeout / time.Second),
			},
		},
	}
	d.Spec.Template.Spec.TerminationGracePeriodSeconds = ptr.To(int64(framework.PodStartTimeout/time.Second) + 30)
	d.Spec.PodReplacementPolicy = ptr.To(appsv1.TerminationStarted)

	deployment, err := c.AppsV1().Deployments(ns).Create(ctx, d, metav1.CreateOptions{})
	framework.ExpectNoError(err)

	// Wait for it to be updated to revision 1
	framework.Logf("Waiting deployment %q to be updated to revision 1", deploymentName)
	err = e2edeployment.WaitForDeploymentRevisionAndImage(c, ns, deploymentName, "1", AgnhostImage)
	framework.ExpectNoError(err)

	framework.Logf("Waiting deployment %q to complete", deploymentName)
	err = e2edeployment.WaitForDeploymentComplete(c, deployment)
	framework.ExpectNoError(err)

	// Update deployment to delete agnhost pods and bring up webserver pods.
	framework.Logf("Triggering a new rollout for deployment %q", deploymentName)
	deployment, err = e2edeployment.UpdateDeploymentWithRetries(c, ns, deploymentName, func(update *appsv1.Deployment) {
		update.Spec.Template.Spec.Containers[0].Name = WebserverImageName
		update.Spec.Template.Spec.Containers[0].Image = WebserverImage
	})
	framework.ExpectNoError(err)

	framework.Logf("Waiting deployment %q to complete", deploymentName)
	err = e2edeployment.WaitForDeploymentComplete(c, deployment)
	framework.ExpectNoError(err)

	framework.Logf("Verifying that deployment %q can still have old terminating pods once the deployment has completed", deploymentName)
	deployment, err = c.AppsV1().Deployments(ns).Get(ctx, deploymentName, metav1.GetOptions{})
	framework.ExpectNoError(err)

	expectedTerminatingReplicas := replicas
	gomega.Expect(deployment.Status.TerminatingReplicas).To(gomega.Equal(&expectedTerminatingReplicas))
}

// testDeploymentCleanUpPolicy tests that deployment supports cleanup policy
func testDeploymentCleanUpPolicy(ctx context.Context, f *framework.Framework) {
	ns := f.Namespace.Name
	c := f.ClientSet
	// Create webserver pods.
	deploymentPodLabels := map[string]string{"name": "cleanup-pod"}
	rsPodLabels := map[string]string{
		"name": "cleanup-pod",
		"pod":  WebserverImageName,
	}
	rsName := "test-cleanup-controller"
	replicas := int32(1)
	revisionHistoryLimit := ptr.To[int32](0)
	_, err := c.AppsV1().ReplicaSets(ns).Create(ctx, newRS(rsName, replicas, rsPodLabels, WebserverImageName, WebserverImage, nil), metav1.CreateOptions{})
	framework.ExpectNoError(err)

	// Verify that the required pods have come up.
	err = e2epod.VerifyPodsRunning(ctx, c, ns, "cleanup-pod", false, replicas)
	framework.ExpectNoError(err, "error in waiting for pods to come up: %v", err)

	// Create a deployment to delete webserver pods and instead bring up agnhost pods.
	deploymentName := "test-cleanup-deployment"
	framework.Logf("Creating deployment %s", deploymentName)

	pods, err := c.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: labels.Everything().String()})
	framework.ExpectNoError(err, "Failed to query for pods: %v", err)

	options := metav1.ListOptions{
		ResourceVersion: pods.ListMeta.ResourceVersion,
	}
	stopCh := make(chan struct{})
	defer close(stopCh)
	w, err := c.CoreV1().Pods(ns).Watch(ctx, options)
	framework.ExpectNoError(err)
	go func() {
		defer ginkgo.GinkgoRecover()
		// There should be only one pod being created, which is the pod with the agnhost image.
		// The old RS shouldn't create new pod when deployment controller adding pod template hash label to its selector.
		numPodCreation := 1
		for {
			select {
			case event := <-w.ResultChan():
				if event.Type != watch.Added {
					continue
				}
				numPodCreation--
				if numPodCreation < 0 {
					framework.Failf("Expect only one pod creation, the second creation event: %#v\n", event)
				}
				pod, ok := event.Object.(*v1.Pod)
				if !ok {
					framework.Failf("Expect event Object to be a pod")
				}
				if pod.Spec.Containers[0].Name != AgnhostImageName {
					framework.Failf("Expect the created pod to have container name %s, got pod %#v\n", AgnhostImageName, pod)
				}
			case <-stopCh:
				return
			}
		}
	}()
	d := e2edeployment.NewDeployment(deploymentName, replicas, deploymentPodLabels, AgnhostImageName, AgnhostImage, appsv1.RollingUpdateDeploymentStrategyType)
	d.Spec.RevisionHistoryLimit = revisionHistoryLimit
	_, err = c.AppsV1().Deployments(ns).Create(ctx, d, metav1.CreateOptions{})
	framework.ExpectNoError(err)

	ginkgo.By(fmt.Sprintf("Waiting for deployment %s history to be cleaned up", deploymentName))
	err = waitForDeploymentOldRSsNum(ctx, c, ns, deploymentName, int(*revisionHistoryLimit))
	framework.ExpectNoError(err)
}

// testRolloverDeployment tests that deployment supports rollover.
// i.e. we can change desired state and kick off rolling update, then change desired state again before it finishes.
func testRolloverDeployment(ctx context.Context, f *framework.Framework) {
	ns := f.Namespace.Name
	c := f.ClientSet
	podName := "rollover-pod"
	deploymentPodLabels := map[string]string{"name": podName}
	rsPodLabels := map[string]string{
		"name": podName,
		"pod":  WebserverImageName,
	}

	rsName := "test-rollover-controller"
	rsReplicas := int32(1)
	_, err := c.AppsV1().ReplicaSets(ns).Create(ctx, newRS(rsName, rsReplicas, rsPodLabels, WebserverImageName, WebserverImage, nil), metav1.CreateOptions{})
	framework.ExpectNoError(err)
	// Verify that the required pods have come up.
	err = e2epod.VerifyPodsRunning(ctx, c, ns, podName, false, rsReplicas)
	framework.ExpectNoError(err, "error in waiting for pods to come up: %v", err)

	// Wait for replica set to become ready before adopting it.
	framework.Logf("Waiting for pods owned by replica set %q to become ready", rsName)
	err = e2ereplicaset.WaitForReadyReplicaSet(ctx, c, ns, rsName)
	framework.ExpectNoError(err)

	// Create a deployment to delete webserver pods and instead bring up redis-slave pods.
	// We use a nonexistent image here, so that we make sure it won't finish
	deploymentName, deploymentImageName := "test-rollover-deployment", "redis-slave"
	deploymentReplicas := int32(1)
	deploymentImage := "gcr.io/google_samples/gb-redisslave:nonexistent"
	deploymentStrategyType := appsv1.RollingUpdateDeploymentStrategyType
	framework.Logf("Creating deployment %q", deploymentName)
	newDeployment := e2edeployment.NewDeployment(deploymentName, deploymentReplicas, deploymentPodLabels, deploymentImageName, deploymentImage, deploymentStrategyType)
	newDeployment.Spec.Strategy.RollingUpdate = &appsv1.RollingUpdateDeployment{
		MaxUnavailable: ptr.To(intstr.FromInt32(0)),
		MaxSurge:       ptr.To(intstr.FromInt32(1)),
	}
	newDeployment.Spec.MinReadySeconds = int32(10)
	_, err = c.AppsV1().Deployments(ns).Create(ctx, newDeployment, metav1.CreateOptions{})
	framework.ExpectNoError(err)

	// Verify that the pods were scaled up and down as expected.
	deployment, err := c.AppsV1().Deployments(ns).Get(ctx, deploymentName, metav1.GetOptions{})
	framework.ExpectNoError(err)
	framework.Logf("Make sure deployment %q performs scaling operations", deploymentName)
	// Make sure the deployment starts to scale up and down replica sets by checking if its updated replicas >= 1
	err = waitForDeploymentUpdatedReplicasGTE(c, ns, deploymentName, deploymentReplicas, deployment.Generation)
	framework.ExpectNoError(err)
	// Check if it's updated to revision 1 correctly
	framework.Logf("Check revision of new replica set for deployment %q", deploymentName)
	err = checkDeploymentRevisionAndImage(c, ns, deploymentName, "1", deploymentImage)
	framework.ExpectNoError(err)

	framework.Logf("Ensure that both replica sets have 1 created replica")
	oldRS, err := c.AppsV1().ReplicaSets(ns).Get(ctx, rsName, metav1.GetOptions{})
	framework.ExpectNoError(err)
	ensureReplicas(oldRS, int32(1))
	newRS, err := testutil.GetNewReplicaSet(deployment, c)
	framework.ExpectNoError(err)
	ensureReplicas(newRS, int32(1))

	// The deployment is stuck, update it to rollover the above 2 ReplicaSets and bring up agnhost pods.
	framework.Logf("Rollover old replica sets for deployment %q with new image update", deploymentName)
	updatedDeploymentImageName, updatedDeploymentImage := AgnhostImageName, AgnhostImage
	deployment, err = e2edeployment.UpdateDeploymentWithRetries(c, ns, newDeployment.Name, func(update *appsv1.Deployment) {
		update.Spec.Template.Spec.Containers[0].Name = updatedDeploymentImageName
		update.Spec.Template.Spec.Containers[0].Image = updatedDeploymentImage
	})
	framework.ExpectNoError(err)

	// Use observedGeneration to determine if the controller noticed the pod template update.
	framework.Logf("Wait deployment %q to be observed by the deployment controller", deploymentName)
	err = waitForObservedDeployment(c, ns, deploymentName, deployment.Generation)
	framework.ExpectNoError(err)

	// Wait for it to be updated to revision 2
	framework.Logf("Wait for revision update of deployment %q to 2", deploymentName)
	err = e2edeployment.WaitForDeploymentRevisionAndImage(c, ns, deploymentName, "2", updatedDeploymentImage)
	framework.ExpectNoError(err)

	framework.Logf("Make sure deployment %q is complete", deploymentName)
	err = waitForDeploymentCompleteAndCheckRolling(c, deployment)
	framework.ExpectNoError(err)

	framework.Logf("Ensure that both old replica sets have no replicas")
	oldRS, err = c.AppsV1().ReplicaSets(ns).Get(ctx, rsName, metav1.GetOptions{})
	framework.ExpectNoError(err)
	ensureReplicas(oldRS, int32(0))
	// Not really the new replica set anymore but we GET by name so that's fine.
	newRS, err = c.AppsV1().ReplicaSets(ns).Get(ctx, newRS.Name, metav1.GetOptions{})
	framework.ExpectNoError(err)
	ensureReplicas(newRS, int32(0))
}

func ensureReplicas(rs *appsv1.ReplicaSet, replicas int32) {
	gomega.Expect(*rs.Spec.Replicas).To(gomega.Equal(replicas))
	gomega.Expect(rs.Status.Replicas).To(gomega.Equal(replicas))
}

func randomScale(d *appsv1.Deployment, i int) {
	switch r := rand.Float32(); {
	case r < 0.3:
		framework.Logf("%02d: scaling up", i)
		*(d.Spec.Replicas)++
	case r < 0.6:
		if *(d.Spec.Replicas) > 1 {
			framework.Logf("%02d: scaling down", i)
			*(d.Spec.Replicas)--
		}
	}
}

func testIterativeDeployments(ctx context.Context, f *framework.Framework) {
	ns := f.Namespace.Name
	c := f.ClientSet

	podLabels := map[string]string{"name": WebserverImageName}
	replicas := int32(6)
	zero := int64(0)
	two := int32(2)

	// Create a webserver deployment.
	deploymentName := "webserver"
	fiveMinutes := int32(5 * 60)
	d := e2edeployment.NewDeployment(deploymentName, replicas, podLabels, WebserverImageName, WebserverImage, appsv1.RollingUpdateDeploymentStrategyType)
	d.Spec.ProgressDeadlineSeconds = &fiveMinutes
	d.Spec.RevisionHistoryLimit = &two
	d.Spec.Template.Spec.TerminationGracePeriodSeconds = &zero
	framework.Logf("Creating deployment %q", deploymentName)
	deployment, err := c.AppsV1().Deployments(ns).Create(ctx, d, metav1.CreateOptions{})
	framework.ExpectNoError(err)

	iterations := 20
	for i := 0; i < iterations; i++ {
		if r := rand.Float32(); r < 0.6 {
			time.Sleep(time.Duration(float32(i) * r * float32(time.Second)))
		}

		switch n := rand.Float32(); {
		case n < 0.2:
			// trigger a new deployment
			framework.Logf("%02d: triggering a new rollout for deployment %q", i, deployment.Name)
			deployment, err = e2edeployment.UpdateDeploymentWithRetries(c, ns, deployment.Name, func(update *appsv1.Deployment) {
				newEnv := v1.EnvVar{Name: fmt.Sprintf("A%d", i), Value: fmt.Sprintf("%d", i)}
				update.Spec.Template.Spec.Containers[0].Env = append(update.Spec.Template.Spec.Containers[0].Env, newEnv)
				randomScale(update, i)
			})
			framework.ExpectNoError(err)

		case n < 0.4:
			// rollback to the previous version
			framework.Logf("%02d: rolling back a rollout for deployment %q", i, deployment.Name)
			deployment, err = e2edeployment.UpdateDeploymentWithRetries(c, ns, deployment.Name, func(update *appsv1.Deployment) {
				if update.Annotations == nil {
					update.Annotations = make(map[string]string)
				}
				update.Annotations[appsv1.DeprecatedRollbackTo] = "0"
			})
			framework.ExpectNoError(err)

		case n < 0.6:
			// just scaling
			framework.Logf("%02d: scaling deployment %q", i, deployment.Name)
			deployment, err = e2edeployment.UpdateDeploymentWithRetries(c, ns, deployment.Name, func(update *appsv1.Deployment) {
				randomScale(update, i)
			})
			framework.ExpectNoError(err)

		case n < 0.8:
			// toggling the deployment
			if deployment.Spec.Paused {
				framework.Logf("%02d: resuming deployment %q", i, deployment.Name)
				deployment, err = e2edeployment.UpdateDeploymentWithRetries(c, ns, deployment.Name, func(update *appsv1.Deployment) {
					update.Spec.Paused = false
					randomScale(update, i)
				})
				framework.ExpectNoError(err)
			} else {
				framework.Logf("%02d: pausing deployment %q", i, deployment.Name)
				deployment, err = e2edeployment.UpdateDeploymentWithRetries(c, ns, deployment.Name, func(update *appsv1.Deployment) {
					update.Spec.Paused = true
					randomScale(update, i)
				})
				framework.ExpectNoError(err)
			}

		default:
			// arbitrarily delete deployment pods
			framework.Logf("%02d: arbitrarily deleting one or more deployment pods for deployment %q", i, deployment.Name)
			selector, err := metav1.LabelSelectorAsSelector(deployment.Spec.Selector)
			framework.ExpectNoError(err)
			opts := metav1.ListOptions{LabelSelector: selector.String()}
			podList, err := c.CoreV1().Pods(ns).List(ctx, opts)
			framework.ExpectNoError(err)
			if len(podList.Items) == 0 {
				framework.Logf("%02d: no deployment pods to delete", i)
				continue
			}
			for p := range podList.Items {
				if rand.Float32() < 0.5 {
					continue
				}
				name := podList.Items[p].Name
				framework.Logf("%02d: deleting deployment pod %q", i, name)
				err := c.CoreV1().Pods(ns).Delete(ctx, name, metav1.DeleteOptions{})
				if err != nil && !apierrors.IsNotFound(err) {
					framework.ExpectNoError(err)
				}
			}
		}
	}

	// unpause the deployment if we end up pausing it
	deployment, err = c.AppsV1().Deployments(ns).Get(ctx, deployment.Name, metav1.GetOptions{})
	framework.ExpectNoError(err)
	if deployment.Spec.Paused {
		framework.Logf("Resuming deployment %q", deployment.Name)
		deployment, err = e2edeployment.UpdateDeploymentWithRetries(c, ns, deployment.Name, func(update *appsv1.Deployment) {
			update.Spec.Paused = false
		})
		framework.ExpectNoError(err)
	}

	framework.Logf("Waiting for deployment %q to be observed by the controller", deploymentName)
	err = waitForObservedDeployment(c, ns, deploymentName, deployment.Generation)
	framework.ExpectNoError(err)

	framework.Logf("Waiting for deployment %q status", deploymentName)
	err = e2edeployment.WaitForDeploymentComplete(c, deployment)
	framework.ExpectNoError(err)

	framework.Logf("Checking deployment %q for a complete condition", deploymentName)
	err = waitForDeploymentWithCondition(c, ns, deploymentName, deploymentutil.NewRSAvailableReason, appsv1.DeploymentProgressing)
	framework.ExpectNoError(err)
}

func testDeploymentsControllerRef(ctx context.Context, f *framework.Framework) {
	ns := f.Namespace.Name
	c := f.ClientSet

	deploymentName := "test-orphan-deployment"
	framework.Logf("Creating Deployment %q", deploymentName)
	podLabels := map[string]string{"name": WebserverImageName}
	replicas := int32(1)
	d := e2edeployment.NewDeployment(deploymentName, replicas, podLabels, WebserverImageName, WebserverImage, appsv1.RollingUpdateDeploymentStrategyType)
	deploy, err := c.AppsV1().Deployments(ns).Create(ctx, d, metav1.CreateOptions{})
	framework.ExpectNoError(err)
	err = e2edeployment.WaitForDeploymentComplete(c, deploy)
	framework.ExpectNoError(err)

	framework.Logf("Verifying Deployment %q has only one ReplicaSet", deploymentName)
	rsList := listDeploymentReplicaSets(ctx, c, ns, podLabels)
	gomega.Expect(rsList.Items).To(gomega.HaveLen(1))

	framework.Logf("Obtaining the ReplicaSet's UID")
	orphanedRSUID := rsList.Items[0].UID

	framework.Logf("Checking the ReplicaSet has the right controllerRef")
	err = checkDeploymentReplicaSetsControllerRef(ctx, c, ns, deploy.UID, podLabels)
	framework.ExpectNoError(err)

	framework.Logf("Deleting Deployment %q and orphaning its ReplicaSet", deploymentName)
	err = orphanDeploymentReplicaSets(ctx, c, deploy)
	framework.ExpectNoError(err)

	ginkgo.By("Wait for the ReplicaSet to be orphaned")
	err = wait.PollUntilContextTimeout(ctx, dRetryPeriod, dRetryTimeout, false, waitDeploymentReplicaSetsOrphaned(c, ns, podLabels))
	framework.ExpectNoError(err, "error waiting for Deployment ReplicaSet to be orphaned")

	deploymentName = "test-adopt-deployment"
	framework.Logf("Creating Deployment %q to adopt the ReplicaSet", deploymentName)
	d = e2edeployment.NewDeployment(deploymentName, replicas, podLabels, WebserverImageName, WebserverImage, appsv1.RollingUpdateDeploymentStrategyType)
	deploy, err = c.AppsV1().Deployments(ns).Create(ctx, d, metav1.CreateOptions{})
	framework.ExpectNoError(err)
	err = e2edeployment.WaitForDeploymentComplete(c, deploy)
	framework.ExpectNoError(err)

	framework.Logf("Waiting for the ReplicaSet to have the right controllerRef")
	err = checkDeploymentReplicaSetsControllerRef(ctx, c, ns, deploy.UID, podLabels)
	framework.ExpectNoError(err)

	framework.Logf("Verifying no extra ReplicaSet is created (Deployment %q still has only one ReplicaSet after adoption)", deploymentName)
	rsList = listDeploymentReplicaSets(ctx, c, ns, podLabels)
	gomega.Expect(rsList.Items).To(gomega.HaveLen(1))

	framework.Logf("Verifying the ReplicaSet has the same UID as the orphaned ReplicaSet")
	gomega.Expect(rsList.Items[0].UID).To(gomega.Equal(orphanedRSUID))
}

// testProportionalScalingDeployment tests that when a RollingUpdate Deployment is scaled in the middle
// of a rollout (either in progress or paused), then the Deployment will balance additional replicas
// in existing active ReplicaSets (ReplicaSets with more than 0 replica) in order to mitigate risk.
func testProportionalScalingDeployment(ctx context.Context, f *framework.Framework, testTerminationCompletePodReplacementPolicy bool) {
	ns := f.Namespace.Name
	c := f.ClientSet

	podLabels := map[string]string{"name": WebserverImageName}
	replicas := int32(10)

	// Create a webserver deployment.
	deploymentName := "webserver-deployment"
	d := e2edeployment.NewDeployment(deploymentName, replicas, podLabels, WebserverImageName, WebserverImage, appsv1.RollingUpdateDeploymentStrategyType)
	d.Spec.Strategy.RollingUpdate = new(appsv1.RollingUpdateDeployment)
	d.Spec.Strategy.RollingUpdate.MaxSurge = ptr.To(intstr.FromInt32(3))
	d.Spec.Strategy.RollingUpdate.MaxUnavailable = ptr.To(intstr.FromInt32(2))
	if testTerminationCompletePodReplacementPolicy {
		d.Spec.Template.Spec.Containers[0].Lifecycle = &v1.Lifecycle{
			PreStop: &v1.LifecycleHandler{
				Sleep: &v1.SleepAction{
					Seconds: int64(framework.PodStartTimeout / time.Second),
				},
			},
		}
		d.Spec.Template.Spec.TerminationGracePeriodSeconds = ptr.To(int64(framework.PodStartTimeout/time.Second) + 30)
		d.Spec.PodReplacementPolicy = ptr.To(appsv1.TerminationComplete)
	}

	framework.Logf("Creating deployment %q", deploymentName)
	deployment, err := c.AppsV1().Deployments(ns).Create(ctx, d, metav1.CreateOptions{})
	framework.ExpectNoError(err)

	framework.Logf("Waiting for observed generation %d", deployment.Generation)
	err = waitForObservedDeployment(c, ns, deploymentName, deployment.Generation)
	framework.ExpectNoError(err)

	// Verify that the required pods have come up.
	framework.Logf("Waiting for all required pods to come up")
	err = e2epod.VerifyPodsRunning(ctx, c, ns, WebserverImageName, false, *(deployment.Spec.Replicas))
	framework.ExpectNoError(err, "error in waiting for pods to come up: %v", err)

	framework.Logf("Waiting for deployment %q to complete", deployment.Name)
	err = e2edeployment.WaitForDeploymentComplete(c, deployment)
	framework.ExpectNoError(err)

	firstRS, err := testutil.GetNewReplicaSet(deployment, c)
	framework.ExpectNoError(err)

	// Update the deployment with a non-existent image so that the new replica set
	// will be blocked to simulate a partial rollout.
	framework.Logf("Updating deployment %q with a non-existent image", deploymentName)
	deployment, err = e2edeployment.UpdateDeploymentWithRetries(c, ns, d.Name, func(update *appsv1.Deployment) {
		update.Spec.Template.Spec.Containers[0].Image = "webserver:404"
	})
	framework.ExpectNoError(err)

	framework.Logf("Waiting for observed generation %d", deployment.Generation)
	err = waitForObservedDeployment(c, ns, deploymentName, deployment.Generation)
	framework.ExpectNoError(err)

	// Checking state of first rollout's replicaset.
	maxUnavailable, err := intstr.GetScaledValueFromIntOrPercent(deployment.Spec.Strategy.RollingUpdate.MaxUnavailable, int(*(deployment.Spec.Replicas)), false)
	framework.ExpectNoError(err)

	// First rollout's replicaset should have Deployment's (replicas - maxUnavailable) = 10 - 2 = 8 available replicas.
	minAvailableReplicas := replicas - int32(maxUnavailable)
	framework.Logf("Waiting for the first rollout's replicaset to have .status.availableReplicas = %d", minAvailableReplicas)
	err = e2ereplicaset.WaitForReplicaSetTargetAvailableReplicas(ctx, c, firstRS, minAvailableReplicas)
	framework.ExpectNoError(err)

	// First rollout's replicaset should have .spec.replicas = 8 too.
	framework.Logf("Waiting for the first rollout's replicaset to have .spec.replicas = %d", minAvailableReplicas)
	err = waitForReplicaSetTargetSpecReplicas(ctx, c, firstRS, minAvailableReplicas)
	framework.ExpectNoError(err)

	// The desired replicas wait makes sure that the RS controller has created expected number of pods.
	framework.Logf("Waiting for the first rollout's replicaset of deployment %q to have desired number of replicas", deploymentName)
	firstRS, err = c.AppsV1().ReplicaSets(ns).Get(ctx, firstRS.Name, metav1.GetOptions{})
	framework.ExpectNoError(err)
	err = waitForReplicaSetDesiredReplicas(ctx, c.AppsV1(), firstRS)
	framework.ExpectNoError(err)

	// Checking state of second rollout's replicaset.
	secondRS, err := testutil.GetNewReplicaSet(deployment, c)
	framework.ExpectNoError(err)

	maxSurge, err := intstr.GetScaledValueFromIntOrPercent(deployment.Spec.Strategy.RollingUpdate.MaxSurge, int(*(deployment.Spec.Replicas)), false)
	framework.ExpectNoError(err)

	// Second rollout's replicaset should have 0 available replicas.
	framework.Logf("Verifying that the second rollout's replicaset has .status.availableReplicas = 0")
	gomega.Expect(secondRS.Status.AvailableReplicas).To(gomega.Equal(int32(0)))

	// Second rollout's replicaset should have Deployment's (replicas + maxSurge - first RS's replicas) = 10 + 3 - 8 = 5 for .spec.replicas.
	newReplicas := replicas + int32(maxSurge) - minAvailableReplicas
	if testTerminationCompletePodReplacementPolicy {
		// maxUnavailable causes pods to become terminating, so the second rollout will have 2 replicas less
		newReplicas -= int32(maxUnavailable)
	}
	framework.Logf("Waiting for the second rollout's replicaset to have .spec.replicas = %d", newReplicas)
	err = waitForReplicaSetTargetSpecReplicas(ctx, c, secondRS, newReplicas)
	framework.ExpectNoError(err)

	// The desired replicas wait makes sure that the RS controller has created expected number of pods.
	framework.Logf("Waiting for the second rollout's replicaset of deployment %q to have desired number of replicas", deploymentName)
	secondRS, err = c.AppsV1().ReplicaSets(ns).Get(ctx, secondRS.Name, metav1.GetOptions{})
	framework.ExpectNoError(err)
	err = waitForReplicaSetDesiredReplicas(ctx, c.AppsV1(), secondRS)
	framework.ExpectNoError(err)

	// Check the deployment's minimum availability.
	framework.Logf("Verifying that deployment %q has minimum required number of available replicas", deploymentName)
	if deployment.Status.AvailableReplicas < minAvailableReplicas {
		err = fmt.Errorf("observed %d available replicas, less than min required %d", deployment.Status.AvailableReplicas, minAvailableReplicas)
		framework.ExpectNoError(err)
	}

	// Check the deployment is correctly tracking terminating replicas
	if testTerminationCompletePodReplacementPolicy {
		framework.Logf("Verifying that deployment %q has .status.terminatingReplicas = 0", deploymentName)
		deployment, err = c.AppsV1().Deployments(ns).Get(ctx, deploymentName, metav1.GetOptions{})
		framework.ExpectNoError(err)
		expectedTerminatingReplicas := int32(maxUnavailable)
		gomega.Expect(deployment.Status.TerminatingReplicas).To(gomega.Equal(&expectedTerminatingReplicas))
	}

	// Scale the deployment to 30 replicas.
	newReplicas = int32(30)
	framework.Logf("Scaling up the deployment %q from %d to %d", deploymentName, replicas, newReplicas)
	_, err = e2edeployment.UpdateDeploymentWithRetries(c, ns, deployment.Name, func(update *appsv1.Deployment) {
		update.Spec.Replicas = &newReplicas
	})
	framework.ExpectNoError(err)

	framework.Logf("Waiting for the replicasets of deployment %q to have desired number of replicas", deploymentName)
	firstRS, err = c.AppsV1().ReplicaSets(ns).Get(ctx, firstRS.Name, metav1.GetOptions{})
	framework.ExpectNoError(err)
	secondRS, err = c.AppsV1().ReplicaSets(ns).Get(ctx, secondRS.Name, metav1.GetOptions{})
	framework.ExpectNoError(err)

	// First rollout's replicaset should have .spec.replicas = 8 + (30-10)*(8/13) = 8 + 12 = 20 replicas.
	// Note that 12 comes from rounding (30-10)*(8/13) to nearest integer.
	expectedFirstRolloutReplicas := int32(20)
	if testTerminationCompletePodReplacementPolicy {
		// There are 2 terminating replicas. Second rs is scaled as 3 * 33/13 ~= 3 * 2.54 ~= 8
		// Total number of replicas should be 33 - 2 = 31. So First rs should be 31 - 8 = 23
		expectedFirstRolloutReplicas = 23
	}
	framework.Logf("Verifying that first rollout's replicaset has .spec.replicas = %d", expectedFirstRolloutReplicas)
	err = waitForReplicaSetTargetSpecReplicas(ctx, c, firstRS, expectedFirstRolloutReplicas)
	framework.ExpectNoError(err)
	if testTerminationCompletePodReplacementPolicy {
		framework.Logf("Verifying that first rollout's replicaset has correct annotations and .status.terminatingReplicas = %d", maxUnavailable)
		firstRS, err = c.AppsV1().ReplicaSets(ns).Get(ctx, firstRS.Name, metav1.GetOptions{})
		framework.ExpectNoError(err)
		expectedTerminatingReplicas := int32(maxUnavailable)
		gomega.Expect(firstRS.Status.TerminatingReplicas).To(gomega.Equal(&expectedTerminatingReplicas))
		// not fully scaled
		gomega.Expect(firstRS.Annotations).To(gomega.Equal(map[string]string{
			deploymentutil.RevisionAnnotation:                      "1",
			deploymentutil.DesiredReplicasAnnotation:               "10",
			deploymentutil.MaxReplicasAnnotation:                   "13",
			deploymentutil.ReplicaSetReplicasBeforeScaleAnnotation: "8",
		}))
	}

	// Second rollout's replicaset should have .spec.replicas = 5 + (30-10)*(5/13) = 5 + 8 = 13 replicas.
	// Note that 8 comes from rounding (30-10)*(5/13) to nearest integer.
	expectedSecondRolloutReplicas := int32(13)
	if testTerminationCompletePodReplacementPolicy {
		// There are 2 terminating replicas. Second rs is scaled as 3 * 33/13 ~= 3 * 2.54 ~= 8
		expectedSecondRolloutReplicas = 8
	}
	framework.Logf("Verifying that second rollout's replicaset has .spec.replicas = %d", expectedSecondRolloutReplicas)
	err = waitForReplicaSetTargetSpecReplicas(ctx, c, secondRS, expectedSecondRolloutReplicas)
	framework.ExpectNoError(err)
	if testTerminationCompletePodReplacementPolicy {
		framework.Logf("Verifying that second rollout's replicaset has correct annotations and .status.terminatingReplicas = 0")
		secondRS, err = c.AppsV1().ReplicaSets(ns).Get(ctx, secondRS.Name, metav1.GetOptions{})
		framework.ExpectNoError(err)
		expectedTerminatingReplicas := int32(0)
		gomega.Expect(secondRS.Status.TerminatingReplicas).To(gomega.Equal(&expectedTerminatingReplicas))
		// fully scaled
		gomega.Expect(secondRS.Annotations).To(gomega.Equal(map[string]string{
			deploymentutil.RevisionAnnotation:        "2",
			deploymentutil.DesiredReplicasAnnotation: "30",
			deploymentutil.MaxReplicasAnnotation:     "33",
		}))
	}

	if testTerminationCompletePodReplacementPolicy {
		framework.Logf("Verifying that partial scaleup completes for first rollout's replicaset once all terminating pods terminate")
		err = forcefullyTerminateTerminatingRSPods(ctx, c, firstRS)
		framework.ExpectNoError(err)

		// maxUnavailable equals to number of terminating pods that have just been forcefully terminated
		expectedFirstRolloutReplicas += int32(maxUnavailable)
		framework.Logf("Verifying that first rollout's replicaset has .spec.replicas = %d", expectedFirstRolloutReplicas)
		err = waitForReplicaSetTargetSpecReplicas(ctx, c, firstRS, expectedFirstRolloutReplicas)
		framework.ExpectNoError(err)

		framework.Logf("Verifying that first rollout's replicaset has correct annotations and .status.terminatingReplicas = 0")
		firstRS, err = c.AppsV1().ReplicaSets(ns).Get(ctx, firstRS.Name, metav1.GetOptions{})
		framework.ExpectNoError(err)
		expectedTerminatingReplicas := int32(0)
		gomega.Expect(secondRS.Status.TerminatingReplicas).To(gomega.Equal(&expectedTerminatingReplicas))
		// not fully scaled
		gomega.Expect(firstRS.Annotations).To(gomega.Equal(map[string]string{
			deploymentutil.RevisionAnnotation:        "1",
			deploymentutil.DesiredReplicasAnnotation: "30",
			deploymentutil.MaxReplicasAnnotation:     "33",
		}))
	}
}

func checkDeploymentReplicaSetsControllerRef(ctx context.Context, c clientset.Interface, ns string, uid types.UID, label map[string]string) error {
	rsList := listDeploymentReplicaSets(ctx, c, ns, label)
	for _, rs := range rsList.Items {
		// This rs is adopted only when its controller ref is update
		if controllerRef := metav1.GetControllerOf(&rs); controllerRef == nil || controllerRef.UID != uid {
			return fmt.Errorf("ReplicaSet %s has unexpected controllerRef %v", rs.Name, controllerRef)
		}
	}
	return nil
}

func waitDeploymentReplicaSetsOrphaned(c clientset.Interface, ns string, label map[string]string) func(ctx context.Context) (bool, error) {
	return func(ctx context.Context) (bool, error) {
		rsList := listDeploymentReplicaSets(ctx, c, ns, label)
		for _, rs := range rsList.Items {
			// This rs is orphaned only when controller ref is cleared
			if controllerRef := metav1.GetControllerOf(&rs); controllerRef != nil {
				return false, nil
			}
		}
		return true, nil
	}
}

func listDeploymentReplicaSets(ctx context.Context, c clientset.Interface, ns string, label map[string]string) *appsv1.ReplicaSetList {
	selector := labels.Set(label).AsSelector()
	options := metav1.ListOptions{LabelSelector: selector.String()}
	rsList, err := c.AppsV1().ReplicaSets(ns).List(ctx, options)
	framework.ExpectNoError(err)
	gomega.Expect(rsList.Items).ToNot(gomega.BeEmpty())
	return rsList
}

func orphanDeploymentReplicaSets(ctx context.Context, c clientset.Interface, d *appsv1.Deployment) error {
	trueVar := true
	deleteOptions := metav1.DeleteOptions{OrphanDependents: &trueVar}
	deleteOptions.Preconditions = metav1.NewUIDPreconditions(string(d.UID))
	return c.AppsV1().Deployments(d.Namespace).Delete(ctx, d.Name, deleteOptions)
}

func forcefullyTerminateTerminatingRSPods(ctx context.Context, c clientset.Interface, replicaSet *appsv1.ReplicaSet) error {
	selector, err := metav1.LabelSelectorAsSelector(replicaSet.Spec.Selector)
	if err != nil {
		return err
	}

	pods, err := c.CoreV1().Pods(replicaSet.Namespace).List(ctx, metav1.ListOptions{LabelSelector: selector.String()})
	if err != nil {
		return err
	}

	for _, pod := range pods.Items {
		if pod.DeletionTimestamp == nil {
			continue
		}
		if err = c.CoreV1().Pods(replicaSet.Namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{GracePeriodSeconds: ptr.To[int64](0)}); err != nil {
			return err
		}
	}
	return nil
}

func testRollingUpdateDeploymentWithLocalTrafficLoadBalancer(ctx context.Context, f *framework.Framework) {
	ns := f.Namespace.Name
	c := f.ClientSet

	name := "test-rolling-update-with-lb"
	framework.Logf("Creating Deployment %q", name)
	podLabels := map[string]string{"name": name}
	replicas := int32(3)
	d := e2edeployment.NewDeployment(name, replicas, podLabels, AgnhostImageName, AgnhostImage, appsv1.RollingUpdateDeploymentStrategyType)
	// NewDeployment assigned the same value to both d.Spec.Selector and
	// d.Spec.Template.Labels, so mutating the one would mutate the other.
	// Thus we need to set d.Spec.Template.Labels to a new value if we want
	// to mutate it alone.
	d.Spec.Template.Labels = map[string]string{
		"iteration": "0",
		"name":      name,
	}
	d.Spec.Template.Spec.Containers[0].Args = []string{"netexec", "--http-port=80", "--udp-port=80"}
	// To ensure that a node that had a local endpoint prior to a rolling
	// update continues to have a local endpoint throughout the rollout, we
	// need an affinity policy that will cause pods to be scheduled on the
	// same nodes as old pods, and we need the deployment to scale up a new
	// pod before deleting an old pod.  This affinity policy will define
	// inter-pod affinity for pods of different rollouts and anti-affinity
	// for pods of the same rollout, so it will need to be updated when
	// performing a rollout.
	setAffinities(d, false)
	d.Spec.Strategy.RollingUpdate = &appsv1.RollingUpdateDeployment{
		MaxSurge:       ptr.To(intstr.FromInt32(1)),
		MaxUnavailable: ptr.To(intstr.FromInt32(0)),
	}
	deployment, err := c.AppsV1().Deployments(ns).Create(ctx, d, metav1.CreateOptions{})
	framework.ExpectNoError(err)
	err = e2edeployment.WaitForDeploymentComplete(c, deployment)
	framework.ExpectNoError(err)

	framework.Logf("Creating a service %s with type=LoadBalancer and externalTrafficPolicy=Local in namespace %s", name, ns)
	jig := e2eservice.NewTestJig(c, ns, name)
	jig.Labels = podLabels
	service, err := jig.CreateLoadBalancerService(ctx, e2eservice.GetServiceLoadBalancerCreationTimeout(ctx, c), func(svc *v1.Service) {
		svc.Spec.ExternalTrafficPolicy = v1.ServiceExternalTrafficPolicyLocal
	})
	framework.ExpectNoError(err)

	lbNameOrAddress := e2eservice.GetIngressPoint(&service.Status.LoadBalancer.Ingress[0])
	svcPort := int(service.Spec.Ports[0].Port)

	framework.Logf("Hitting the replica set's pods through the service's load balancer")
	timeout := e2eservice.LoadBalancerLagTimeoutDefault
	if framework.ProviderIs("aws") {
		timeout = e2eservice.LoadBalancerLagTimeoutAWS
	}
	e2eservice.TestReachableHTTP(ctx, lbNameOrAddress, svcPort, timeout)

	expectedNodes, err := jig.GetEndpointNodeNames(ctx)
	framework.ExpectNoError(err)

	framework.Logf("Starting a goroutine to watch the service's endpoints in the background")
	done := make(chan struct{})
	failed := make(chan struct{})
	defer close(done)
	go func() {
		defer ginkgo.GinkgoRecover()
		// The affinity policy should ensure that before an old pod is
		// deleted, a new pod will have been created on the same node.
		// Thus the set of nodes with local endpoints for the service
		// should remain unchanged.
		wait.Until(func() {
			actualNodes, err := jig.GetEndpointNodeNames(ctx)
			if err != nil {
				framework.Logf("The previous set of nodes with local endpoints was %v, now the lookup failed: %v", expectedNodes.List(), err)
				failed <- struct{}{}
				return
			}
			if !actualNodes.Equal(expectedNodes) {
				framework.Logf("The set of nodes with local endpoints changed; started with %v, now have %v", expectedNodes.List(), actualNodes.List())
				failed <- struct{}{}
			}
		}, framework.Poll, done)
	}()

	framework.Logf("Triggering a rolling deployment several times")
	for i := 1; i <= 3; i++ {
		framework.Logf("Updating label deployment %q pod spec (iteration #%d)", name, i)
		deployment, err = e2edeployment.UpdateDeploymentWithRetries(c, ns, d.Name, func(update *appsv1.Deployment) {
			update.Spec.Template.Labels["iteration"] = fmt.Sprintf("%d", i)
			setAffinities(update, true)
		})
		framework.ExpectNoError(err)

		framework.Logf("Waiting for observed generation %d", deployment.Generation)
		err = waitForObservedDeployment(c, ns, name, deployment.Generation)
		framework.ExpectNoError(err)

		framework.Logf("Make sure deployment %q is complete", name)
		err = waitForDeploymentCompleteAndCheckRolling(c, deployment)
		framework.ExpectNoError(err)
	}

	select {
	case <-failed:
		framework.Failf("Connectivity to the load balancer was interrupted")
	case <-time.After(1 * time.Minute):
	}
}

// setAffinities set PodAntiAffinity across pods from the same generation
// of Deployment and if, explicitly requested, also affinity with pods
// from other generations.
// It is required to make those "Required" so that in large clusters where
// scheduler may not score all nodes if a lot of them are feasible, the
// test will also have a chance to pass.
func setAffinities(d *appsv1.Deployment, setAffinity bool) {
	affinity := &v1.Affinity{
		PodAntiAffinity: &v1.PodAntiAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: []v1.PodAffinityTerm{
				{
					TopologyKey: "kubernetes.io/hostname",
					LabelSelector: &metav1.LabelSelector{
						MatchExpressions: []metav1.LabelSelectorRequirement{
							{
								Key:      "name",
								Operator: metav1.LabelSelectorOpIn,
								Values:   []string{d.Spec.Template.Labels["name"]},
							},
							{
								Key:      "iteration",
								Operator: metav1.LabelSelectorOpIn,
								Values:   []string{d.Spec.Template.Labels["iteration"]},
							},
						},
					},
				},
			},
		},
	}
	if setAffinity {
		affinity.PodAffinity = &v1.PodAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: []v1.PodAffinityTerm{
				{
					TopologyKey: "kubernetes.io/hostname",
					LabelSelector: &metav1.LabelSelector{
						MatchExpressions: []metav1.LabelSelectorRequirement{
							{
								Key:      "name",
								Operator: metav1.LabelSelectorOpIn,
								Values:   []string{d.Spec.Template.Labels["name"]},
							},
							{
								Key:      "iteration",
								Operator: metav1.LabelSelectorOpNotIn,
								Values:   []string{d.Spec.Template.Labels["iteration"]},
							},
						},
					},
				},
			},
		}
	}
	d.Spec.Template.Spec.Affinity = affinity
}

// watchRecreateDeployment watches Recreate deployments and ensures no new pods will run at the same time with
// old pods.
func watchRecreateDeployment(ctx context.Context, c clientset.Interface, d *appsv1.Deployment) error {
	if d.Spec.Strategy.Type != appsv1.RecreateDeploymentStrategyType {
		return fmt.Errorf("deployment %q does not use a Recreate strategy: %s", d.Name, d.Spec.Strategy.Type)
	}

	fieldSelector := fields.OneTermEqualSelector("metadata.name", d.Name).String()
	w := &cache.ListWatch{
		WatchFunc: func(options metav1.ListOptions) (i watch.Interface, e error) {
			options.FieldSelector = fieldSelector
			return c.AppsV1().Deployments(d.Namespace).Watch(ctx, options)
		},
	}

	status := d.Status

	condition := func(event watch.Event) (bool, error) {
		d := event.Object.(*appsv1.Deployment)
		status = d.Status

		if d.Status.UpdatedReplicas > 0 && d.Status.Replicas != d.Status.UpdatedReplicas {
			_, allOldRSs, err := testutil.GetOldReplicaSets(d, c)
			newRS, nerr := testutil.GetNewReplicaSet(d, c)
			if err == nil && nerr == nil {
				framework.Logf("%+v", d)
				testutil.LogReplicaSetsOfDeployment(d, allOldRSs, newRS, framework.Logf)
				testutil.LogPodsOfDeployment(c, d, append(allOldRSs, newRS), framework.Logf)
			}
			return false, fmt.Errorf("deployment %q is running new pods alongside old pods: %#v", d.Name, status)
		}

		return *(d.Spec.Replicas) == d.Status.Replicas &&
			*(d.Spec.Replicas) == d.Status.UpdatedReplicas &&
			d.Generation <= d.Status.ObservedGeneration, nil
	}

	ctxUntil, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	_, err := watchtools.Until(ctxUntil, d.ResourceVersion, w, condition)
	if wait.Interrupted(err) {
		err = fmt.Errorf("deployment %q never completed: %#v", d.Name, status)
	}
	return err
}

// waitForDeploymentOldRSsNum waits for the deployment to clean up old rcs.
func waitForDeploymentOldRSsNum(ctx context.Context, c clientset.Interface, ns, deploymentName string, desiredRSNum int) error {
	var oldRSs []*appsv1.ReplicaSet
	var d *appsv1.Deployment

	pollErr := wait.PollImmediate(poll, 5*time.Minute, func() (bool, error) {
		deployment, err := c.AppsV1().Deployments(ns).Get(ctx, deploymentName, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		d = deployment

		_, oldRSs, err = testutil.GetOldReplicaSets(deployment, c)
		if err != nil {
			return false, err
		}
		return len(oldRSs) == desiredRSNum, nil
	})
	if wait.Interrupted(pollErr) {
		pollErr = fmt.Errorf("%d old replica sets were not cleaned up for deployment %q", len(oldRSs)-desiredRSNum, deploymentName)
		testutil.LogReplicaSetsOfDeployment(d, oldRSs, nil, framework.Logf)
	}
	return pollErr
}

// waitForReplicaSetDesiredReplicas waits until the replicaset has desired number of replicas.
func waitForReplicaSetDesiredReplicas(ctx context.Context, rsClient appsclient.ReplicaSetsGetter, replicaSet *appsv1.ReplicaSet) error {
	desiredGeneration := replicaSet.Generation
	err := wait.PollUntilContextTimeout(ctx, framework.Poll, framework.PollShortTimeout, true, func(ctx context.Context) (bool, error) {
		rs, err := rsClient.ReplicaSets(replicaSet.Namespace).Get(ctx, replicaSet.Name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		return rs.Status.ObservedGeneration >= desiredGeneration && rs.Status.Replicas == *(replicaSet.Spec.Replicas) && rs.Status.Replicas == *(rs.Spec.Replicas), nil
	})
	if wait.Interrupted(err) {
		err = fmt.Errorf("replicaset %q never had desired number of replicas", replicaSet.Name)
	}
	return err
}

// waitForReplicaSetTargetSpecReplicas waits for .spec.replicas of a RS to equal targetReplicaNum
func waitForReplicaSetTargetSpecReplicas(ctx context.Context, c clientset.Interface, replicaSet *appsv1.ReplicaSet, targetReplicaNum int32) error {
	desiredGeneration := replicaSet.Generation
	err := wait.PollUntilContextTimeout(ctx, framework.Poll, framework.PollShortTimeout, true, func(ctx context.Context) (bool, error) {
		rs, err := c.AppsV1().ReplicaSets(replicaSet.Namespace).Get(ctx, replicaSet.Name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		return rs.Status.ObservedGeneration >= desiredGeneration && *rs.Spec.Replicas == targetReplicaNum, nil
	})
	if wait.Interrupted(err) {
		err = fmt.Errorf("replicaset %q never had desired number of .spec.replicas", replicaSet.Name)
	}
	return err
}

// checkDeploymentRevisionAndImage checks if the input deployment's and its new replica set's revision and image are as expected.
func checkDeploymentRevisionAndImage(c clientset.Interface, ns, deploymentName, revision, image string) error {
	return testutil.CheckDeploymentRevisionAndImage(c, ns, deploymentName, revision, image)
}

// waitForObservedDeployment waits for the specified deployment generation.
func waitForObservedDeployment(c clientset.Interface, ns, deploymentName string, desiredGeneration int64) error {
	return testutil.WaitForObservedDeployment(c, ns, deploymentName, desiredGeneration)
}

// waitForDeploymentWithCondition waits for the specified deployment condition.
func waitForDeploymentWithCondition(c clientset.Interface, ns, deploymentName, reason string, condType appsv1.DeploymentConditionType) error {
	return testutil.WaitForDeploymentWithCondition(c, ns, deploymentName, reason, condType, framework.Logf, poll, pollLongTimeout)
}

// waitForDeploymentCompleteAndCheckRolling waits for the deployment to complete, and check rolling update strategy isn't broken at any times.
// Rolling update strategy should not be broken during a rolling update.
func waitForDeploymentCompleteAndCheckRolling(c clientset.Interface, d *appsv1.Deployment) error {
	return testutil.WaitForDeploymentCompleteAndCheckRolling(c, d, framework.Logf, poll, pollLongTimeout)
}

// waitForDeploymentUpdatedReplicasGTE waits for given deployment to be observed by the controller and has at least a number of updatedReplicas
func waitForDeploymentUpdatedReplicasGTE(c clientset.Interface, ns, deploymentName string, minUpdatedReplicas int32, desiredGeneration int64) error {
	return testutil.WaitForDeploymentUpdatedReplicasGTE(c, ns, deploymentName, minUpdatedReplicas, desiredGeneration, poll, pollLongTimeout)
}

// Deployment should have a working scale subresource
func testDeploymentSubresources(ctx context.Context, f *framework.Framework) {
	ns := f.Namespace.Name
	c := f.ClientSet

	deploymentName := "test-new-deployment"
	framework.Logf("Creating simple deployment %s", deploymentName)
	d := e2edeployment.NewDeployment("test-new-deployment", int32(1), map[string]string{"name": WebserverImageName}, WebserverImageName, WebserverImage, appsv1.RollingUpdateDeploymentStrategyType)
	deploy, err := c.AppsV1().Deployments(ns).Create(ctx, d, metav1.CreateOptions{})
	framework.ExpectNoError(err)

	// Wait for it to be updated to revision 1
	err = e2edeployment.WaitForDeploymentRevisionAndImage(c, ns, deploymentName, "1", WebserverImage)
	framework.ExpectNoError(err)

	err = e2edeployment.WaitForDeploymentComplete(c, deploy)
	framework.ExpectNoError(err)

	_, err = c.AppsV1().Deployments(ns).Get(ctx, deploymentName, metav1.GetOptions{})
	framework.ExpectNoError(err)

	ginkgo.By("getting scale subresource")
	scale, err := c.AppsV1().Deployments(ns).GetScale(ctx, deploymentName, metav1.GetOptions{})
	if err != nil {
		framework.Failf("Failed to get scale subresource: %v", err)
	}
	gomega.Expect(scale.Spec.Replicas).To(gomega.Equal(int32(1)))
	gomega.Expect(scale.Status.Replicas).To(gomega.Equal(int32(1)))

	ginkgo.By("updating a scale subresource")
	scale.ResourceVersion = "" // indicate the scale update should be unconditional
	scale.Spec.Replicas = 2
	scaleResult, err := c.AppsV1().Deployments(ns).UpdateScale(ctx, deploymentName, scale, metav1.UpdateOptions{})
	if err != nil {
		framework.Failf("Failed to put scale subresource: %v", err)
	}
	gomega.Expect(scaleResult.Spec.Replicas).To(gomega.Equal(int32(2)))

	ginkgo.By("verifying the deployment Spec.Replicas was modified")
	deployment, err := c.AppsV1().Deployments(ns).Get(ctx, deploymentName, metav1.GetOptions{})
	if err != nil {
		framework.Failf("Failed to get deployment resource: %v", err)
	}
	gomega.Expect(*(deployment.Spec.Replicas)).To(gomega.Equal(int32(2)))

	ginkgo.By("Patch a scale subresource")
	scale.ResourceVersion = "" // indicate the scale update should be unconditional
	scale.Spec.Replicas = 4    // should be 2 after "UpdateScale" operation, now Patch to 4
	deploymentScalePatchPayload, err := json.Marshal(autoscalingv1.Scale{
		Spec: autoscalingv1.ScaleSpec{
			Replicas: scale.Spec.Replicas,
		},
	})
	framework.ExpectNoError(err, "Could not Marshal JSON for patch payload")

	_, err = c.AppsV1().Deployments(ns).Patch(ctx, deploymentName, types.StrategicMergePatchType, []byte(deploymentScalePatchPayload), metav1.PatchOptions{}, "scale")
	framework.ExpectNoError(err, "Failed to patch deployment: %v", err)

	deployment, err = c.AppsV1().Deployments(ns).Get(ctx, deploymentName, metav1.GetOptions{})
	framework.ExpectNoError(err, "Failed to get deployment resource: %v", err)
	gomega.Expect(*(deployment.Spec.Replicas)).To(gomega.Equal(int32(4)), "deployment should have 4 replicas")
}
