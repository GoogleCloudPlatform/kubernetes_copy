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

package apimachinery

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1beta1"
	schedulingv1 "k8s.io/api/scheduling/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	utilrand "k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
	quota "k8s.io/apiserver/pkg/quota/v1"
	clientset "k8s.io/client-go/kubernetes"
	clientscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/cache"
	watchtools "k8s.io/client-go/tools/watch"
	"k8s.io/client-go/util/retry"
	"k8s.io/kubernetes/pkg/features"
	"k8s.io/kubernetes/pkg/quota/v1/evaluator/core"
	"k8s.io/kubernetes/test/e2e/feature"
	"k8s.io/kubernetes/test/e2e/framework"
	"k8s.io/kubernetes/test/utils/crd"
	imageutils "k8s.io/kubernetes/test/utils/image"
	admissionapi "k8s.io/pod-security-admission/api"
	"k8s.io/utils/pointer"
	"k8s.io/utils/ptr"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
)

const (
	// how long to wait for a resource quota update to occur
	resourceQuotaTimeout = time.Minute
	podName              = "pfpod"
)

var classGold = "gold"
var classSilver = "silver"
var extendedResourceName = "example.com/dongle"

var _ = SIGDescribe("ResourceQuota", func() {
	f := framework.NewDefaultFramework("resourcequota")
	f.NamespacePodSecurityLevel = admissionapi.LevelBaseline

	/*
		Release: v1.16
		Testname: ResourceQuota, object count quota, resourcequotas
		Description: Create a ResourceQuota. Creation MUST be successful and its ResourceQuotaStatus MUST match to expected used and total allowed resource quota count within namespace.
	*/
	framework.ConformanceIt("should create a ResourceQuota and ensure its status is promptly calculated.", func(ctx context.Context) {
		ginkgo.By("Counting existing ResourceQuota")
		c, err := countResourceQuota(ctx, f.ClientSet, f.Namespace.Name)
		framework.ExpectNoError(err)

		ginkgo.By("Creating a ResourceQuota")
		quotaName := "test-quota"
		resourceQuota := newTestResourceQuota(quotaName)
		_, err = createResourceQuota(ctx, f.ClientSet, f.Namespace.Name, resourceQuota)
		framework.ExpectNoError(err)

		ginkgo.By("Ensuring resource quota status is calculated")
		usedResources := v1.ResourceList{}
		usedResources[v1.ResourceQuotas] = resource.MustParse(strconv.Itoa(c + 1))
		err = waitForResourceQuota(ctx, f.ClientSet, f.Namespace.Name, quotaName, usedResources)
		framework.ExpectNoError(err)
	})

	/*
		Release: v1.16
		Testname: ResourceQuota, object count quota, service
		Description: Create a ResourceQuota. Creation MUST be successful and its ResourceQuotaStatus MUST match to expected used and total allowed resource quota count within namespace.
		Create a Service. Its creation MUST be successful and resource usage count against the Service object and resourceQuota object MUST be captured in ResourceQuotaStatus of the ResourceQuota.
		Delete the Service. Deletion MUST succeed and resource usage count against the Service object MUST be released from ResourceQuotaStatus of the ResourceQuota.
	*/
	framework.ConformanceIt("should create a ResourceQuota and capture the life of a service.", func(ctx context.Context) {
		ginkgo.By("Counting existing ResourceQuota")
		c, err := countResourceQuota(ctx, f.ClientSet, f.Namespace.Name)
		framework.ExpectNoError(err)

		ginkgo.By("Creating a ResourceQuota")
		quotaName := "test-quota"
		resourceQuota := newTestResourceQuota(quotaName)
		_, err = createResourceQuota(ctx, f.ClientSet, f.Namespace.Name, resourceQuota)
		framework.ExpectNoError(err)

		ginkgo.By("Ensuring resource quota status is calculated")
		usedResources := v1.ResourceList{}
		usedResources[v1.ResourceQuotas] = resource.MustParse(strconv.Itoa(c + 1))
		err = waitForResourceQuota(ctx, f.ClientSet, f.Namespace.Name, quotaName, usedResources)
		framework.ExpectNoError(err)

		ginkgo.By("Creating a Service")
		service := newTestServiceForQuota("test-service", v1.ServiceTypeClusterIP, false)
		service, err = f.ClientSet.CoreV1().Services(f.Namespace.Name).Create(ctx, service, metav1.CreateOptions{})
		framework.ExpectNoError(err)

		ginkgo.By("Creating a NodePort Service")
		nodeport := newTestServiceForQuota("test-service-np", v1.ServiceTypeNodePort, false)
		nodeport, err = f.ClientSet.CoreV1().Services(f.Namespace.Name).Create(ctx, nodeport, metav1.CreateOptions{})
		framework.ExpectNoError(err)

		ginkgo.By("Not allowing a LoadBalancer Service with NodePort to be created that exceeds remaining quota")
		loadbalancer := newTestServiceForQuota("test-service-lb", v1.ServiceTypeLoadBalancer, true)
		_, err = f.ClientSet.CoreV1().Services(f.Namespace.Name).Create(ctx, loadbalancer, metav1.CreateOptions{})
		gomega.Expect(err).To(gomega.HaveOccurred())

		ginkgo.By("Ensuring resource quota status captures service creation")
		usedResources = v1.ResourceList{}
		usedResources[v1.ResourceQuotas] = resource.MustParse(strconv.Itoa(c + 1))
		usedResources[v1.ResourceServices] = resource.MustParse("2")
		usedResources[v1.ResourceServicesNodePorts] = resource.MustParse("1")
		err = waitForResourceQuota(ctx, f.ClientSet, f.Namespace.Name, quotaName, usedResources)
		framework.ExpectNoError(err)

		ginkgo.By("Deleting Services")
		err = f.ClientSet.CoreV1().Services(f.Namespace.Name).Delete(ctx, service.Name, metav1.DeleteOptions{})
		framework.ExpectNoError(err)
		err = f.ClientSet.CoreV1().Services(f.Namespace.Name).Delete(ctx, nodeport.Name, metav1.DeleteOptions{})
		framework.ExpectNoError(err)

		ginkgo.By("Ensuring resource quota status released usage")
		usedResources[v1.ResourceServices] = resource.MustParse("0")
		usedResources[v1.ResourceServicesNodePorts] = resource.MustParse("0")
		err = waitForResourceQuota(ctx, f.ClientSet, f.Namespace.Name, quotaName, usedResources)
		framework.ExpectNoError(err)
	})

	/*
		Release: v1.16
		Testname: ResourceQuota, object count quota, secret
		Description: Create a ResourceQuota. Creation MUST be successful and its ResourceQuotaStatus MUST match to expected used and total allowed resource quota count within namespace.
		Create a Secret. Its creation MUST be successful and resource usage count against the Secret object and resourceQuota object MUST be captured in ResourceQuotaStatus of the ResourceQuota.
		Delete the Secret. Deletion MUST succeed and resource usage count against the Secret object MUST be released from ResourceQuotaStatus of the ResourceQuota.
	*/
	framework.ConformanceIt("should create a ResourceQuota and capture the life of a secret.", func(ctx context.Context) {
		ginkgo.By("Discovering how many secrets are in namespace by default")
		found, unchanged := 0, 0
		// On contended servers the service account controller can slow down, leading to the count changing during a run.
		// Wait up to 5s for the count to stabilize, assuming that updates come at a consistent rate, and are not held indefinitely.
		err := wait.PollUntilContextTimeout(ctx, 1*time.Second, 30*time.Second, false, func(ctx context.Context) (bool, error) {
			secrets, err := f.ClientSet.CoreV1().Secrets(f.Namespace.Name).List(ctx, metav1.ListOptions{})
			framework.ExpectNoError(err)
			if len(secrets.Items) == found {
				// loop until the number of secrets has stabilized for 5 seconds
				unchanged++
				return unchanged > 4, nil
			}
			unchanged = 0
			found = len(secrets.Items)
			return false, nil
		})
		framework.ExpectNoError(err)
		defaultSecrets := fmt.Sprintf("%d", found)
		hardSecrets := fmt.Sprintf("%d", found+1)

		ginkgo.By("Counting existing ResourceQuota")
		c, err := countResourceQuota(ctx, f.ClientSet, f.Namespace.Name)
		framework.ExpectNoError(err)

		ginkgo.By("Creating a ResourceQuota")
		quotaName := "test-quota"
		resourceQuota := newTestResourceQuota(quotaName)
		resourceQuota.Spec.Hard[v1.ResourceSecrets] = resource.MustParse(hardSecrets)
		_, err = createResourceQuota(ctx, f.ClientSet, f.Namespace.Name, resourceQuota)
		framework.ExpectNoError(err)

		ginkgo.By("Ensuring resource quota status is calculated")
		usedResources := v1.ResourceList{}
		usedResources[v1.ResourceQuotas] = resource.MustParse(strconv.Itoa(c + 1))
		usedResources[v1.ResourceSecrets] = resource.MustParse(defaultSecrets)
		err = waitForResourceQuota(ctx, f.ClientSet, f.Namespace.Name, quotaName, usedResources)
		framework.ExpectNoError(err)

		ginkgo.By("Creating a Secret")
		secret := newTestSecretForQuota("test-secret")
		secret, err = f.ClientSet.CoreV1().Secrets(f.Namespace.Name).Create(ctx, secret, metav1.CreateOptions{})
		framework.ExpectNoError(err)

		ginkgo.By("Ensuring resource quota status captures secret creation")
		usedResources = v1.ResourceList{}
		usedResources[v1.ResourceSecrets] = resource.MustParse(hardSecrets)
		// we expect there to be two secrets because each namespace will receive
		// a service account token secret by default
		err = waitForResourceQuota(ctx, f.ClientSet, f.Namespace.Name, quotaName, usedResources)
		framework.ExpectNoError(err)

		ginkgo.By("Deleting a secret")
		err = f.ClientSet.CoreV1().Secrets(f.Namespace.Name).Delete(ctx, secret.Name, metav1.DeleteOptions{})
		framework.ExpectNoError(err)

		ginkgo.By("Ensuring resource quota status released usage")
		usedResources[v1.ResourceSecrets] = resource.MustParse(defaultSecrets)
		err = waitForResourceQuota(ctx, f.ClientSet, f.Namespace.Name, quotaName, usedResources)
		framework.ExpectNoError(err)
	})

	/*
		Release: v1.16
		Testname: ResourceQuota, object count quota, pod
		Description: Create a ResourceQuota. Creation MUST be successful and its ResourceQuotaStatus MUST match to expected used and total allowed resource quota count within namespace.
		Create a Pod with resource request count for CPU, Memory, EphemeralStorage and ExtendedResourceName. Pod creation MUST be successful and respective resource usage count MUST be captured in ResourceQuotaStatus of the ResourceQuota.
		Create another Pod with resource request exceeding remaining quota. Pod creation MUST fail as the request exceeds ResourceQuota limits.
		Update the successfully created pod's resource requests. Updation MUST fail as a Pod can not dynamically update its resource requirements.
		Delete the successfully created Pod. Pod Deletion MUST be scuccessful and it MUST release the allocated resource counts from ResourceQuotaStatus of the ResourceQuota.
	*/
	framework.ConformanceIt("should create a ResourceQuota and capture the life of a pod.", func(ctx context.Context) {
		ginkgo.By("Counting existing ResourceQuota")
		c, err := countResourceQuota(ctx, f.ClientSet, f.Namespace.Name)
		framework.ExpectNoError(err)

		ginkgo.By("Creating a ResourceQuota")
		quotaName := "test-quota"
		resourceQuota := newTestResourceQuota(quotaName)
		_, err = createResourceQuota(ctx, f.ClientSet, f.Namespace.Name, resourceQuota)
		framework.ExpectNoError(err)

		ginkgo.By("Ensuring resource quota status is calculated")
		usedResources := v1.ResourceList{}
		usedResources[v1.ResourceQuotas] = resource.MustParse(strconv.Itoa(c + 1))
		err = waitForResourceQuota(ctx, f.ClientSet, f.Namespace.Name, quotaName, usedResources)
		framework.ExpectNoError(err)

		ginkgo.By("Creating a Pod that fits quota")
		podName := "test-pod"
		requests := v1.ResourceList{}
		limits := v1.ResourceList{}
		requests[v1.ResourceCPU] = resource.MustParse("500m")
		requests[v1.ResourceMemory] = resource.MustParse("252Mi")
		requests[v1.ResourceEphemeralStorage] = resource.MustParse("30Gi")
		requests[v1.ResourceName(extendedResourceName)] = resource.MustParse("2")
		limits[v1.ResourceName(extendedResourceName)] = resource.MustParse("2")
		pod := newTestPodForQuota(f, podName, requests, limits)
		pod, err = f.ClientSet.CoreV1().Pods(f.Namespace.Name).Create(ctx, pod, metav1.CreateOptions{})
		framework.ExpectNoError(err)
		podToUpdate := pod

		ginkgo.By("Ensuring ResourceQuota status captures the pod usage")
		usedResources[v1.ResourceQuotas] = resource.MustParse(strconv.Itoa(c + 1))
		usedResources[v1.ResourcePods] = resource.MustParse("1")
		usedResources[v1.ResourceCPU] = requests[v1.ResourceCPU]
		usedResources[v1.ResourceMemory] = requests[v1.ResourceMemory]
		usedResources[v1.ResourceEphemeralStorage] = requests[v1.ResourceEphemeralStorage]
		usedResources[v1.ResourceName(v1.DefaultResourceRequestsPrefix+extendedResourceName)] = requests[v1.ResourceName(extendedResourceName)]
		err = waitForResourceQuota(ctx, f.ClientSet, f.Namespace.Name, quotaName, usedResources)
		framework.ExpectNoError(err)

		ginkgo.By("Not allowing a pod to be created that exceeds remaining quota")
		requests = v1.ResourceList{}
		requests[v1.ResourceCPU] = resource.MustParse("600m")
		requests[v1.ResourceMemory] = resource.MustParse("100Mi")
		pod = newTestPodForQuota(f, "fail-pod", requests, v1.ResourceList{})
		_, err = f.ClientSet.CoreV1().Pods(f.Namespace.Name).Create(ctx, pod, metav1.CreateOptions{})
		gomega.Expect(err).To(gomega.HaveOccurred())

		ginkgo.By("Not allowing a pod to be created that exceeds remaining quota(validation on extended resources)")
		requests = v1.ResourceList{}
		limits = v1.ResourceList{}
		requests[v1.ResourceCPU] = resource.MustParse("500m")
		requests[v1.ResourceMemory] = resource.MustParse("100Mi")
		requests[v1.ResourceEphemeralStorage] = resource.MustParse("30Gi")
		requests[v1.ResourceName(extendedResourceName)] = resource.MustParse("2")
		limits[v1.ResourceName(extendedResourceName)] = resource.MustParse("2")
		pod = newTestPodForQuota(f, "fail-pod-for-extended-resource", requests, limits)
		_, err = f.ClientSet.CoreV1().Pods(f.Namespace.Name).Create(ctx, pod, metav1.CreateOptions{})
		gomega.Expect(err).To(gomega.HaveOccurred())

		ginkgo.By("Ensuring a pod cannot update its resource requirements")
		// a pod cannot dynamically update its resource requirements.
		requests = v1.ResourceList{}
		requests[v1.ResourceCPU] = resource.MustParse("100m")
		requests[v1.ResourceMemory] = resource.MustParse("100Mi")
		requests[v1.ResourceEphemeralStorage] = resource.MustParse("10Gi")
		podToUpdate.Spec.Containers[0].Resources.Requests = requests
		_, err = f.ClientSet.CoreV1().Pods(f.Namespace.Name).Update(ctx, podToUpdate, metav1.UpdateOptions{})
		gomega.Expect(err).To(gomega.HaveOccurred())

		ginkgo.By("Ensuring attempts to update pod resource requirements did not change quota usage")
		err = waitForResourceQuota(ctx, f.ClientSet, f.Namespace.Name, quotaName, usedResources)
		framework.ExpectNoError(err)

		ginkgo.By("Deleting the pod")
		err = f.ClientSet.CoreV1().Pods(f.Namespace.Name).Delete(ctx, podName, *metav1.NewDeleteOptions(0))
		framework.ExpectNoError(err)

		ginkgo.By("Ensuring resource quota status released the pod usage")
		usedResources[v1.ResourceQuotas] = resource.MustParse(strconv.Itoa(c + 1))
		usedResources[v1.ResourcePods] = resource.MustParse("0")
		usedResources[v1.ResourceCPU] = resource.MustParse("0")
		usedResources[v1.ResourceMemory] = resource.MustParse("0")
		usedResources[v1.ResourceEphemeralStorage] = resource.MustParse("0")
		usedResources[v1.ResourceName(v1.DefaultResourceRequestsPrefix+extendedResourceName)] = resource.MustParse("0")
		err = waitForResourceQuota(ctx, f.ClientSet, f.Namespace.Name, quotaName, usedResources)
		framework.ExpectNoError(err)
	})
	/*
		Release: v1.16
		Testname: ResourceQuota, object count quota, configmap
		Description: Create a ResourceQuota. Creation MUST be successful and its ResourceQuotaStatus MUST match to expected used and total allowed resource quota count within namespace.
		Create a ConfigMap. Its creation MUST be successful and resource usage count against the ConfigMap object MUST be captured in ResourceQuotaStatus of the ResourceQuota.
		Delete the ConfigMap. Deletion MUST succeed and resource usage count against the ConfigMap object MUST be released from ResourceQuotaStatus of the ResourceQuota.
	*/
	framework.ConformanceIt("should create a ResourceQuota and capture the life of a configMap.", func(ctx context.Context) {
		found, unchanged := 0, 0
		// On contended servers the service account controller can slow down, leading to the count changing during a run.
		// Wait up to 15s for the count to stabilize, assuming that updates come at a consistent rate, and are not held indefinitely.
		err := wait.PollUntilContextTimeout(ctx, 1*time.Second, time.Minute, false, func(ctx context.Context) (bool, error) {
			configmaps, err := f.ClientSet.CoreV1().ConfigMaps(f.Namespace.Name).List(ctx, metav1.ListOptions{})
			framework.ExpectNoError(err)
			if len(configmaps.Items) == found {
				// loop until the number of configmaps has stabilized for 15 seconds
				unchanged++
				return unchanged > 15, nil
			}
			unchanged = 0
			found = len(configmaps.Items)
			return false, nil
		})
		framework.ExpectNoError(err)
		defaultConfigMaps := fmt.Sprintf("%d", found)
		hardConfigMaps := fmt.Sprintf("%d", found+1)

		ginkgo.By("Counting existing ResourceQuota")
		c, err := countResourceQuota(ctx, f.ClientSet, f.Namespace.Name)
		framework.ExpectNoError(err)

		ginkgo.By("Creating a ResourceQuota")
		quotaName := "test-quota"
		resourceQuota := newTestResourceQuota(quotaName)
		resourceQuota.Spec.Hard[v1.ResourceConfigMaps] = resource.MustParse(hardConfigMaps)
		_, err = createResourceQuota(ctx, f.ClientSet, f.Namespace.Name, resourceQuota)
		framework.ExpectNoError(err)

		ginkgo.By("Ensuring resource quota status is calculated")
		usedResources := v1.ResourceList{}
		usedResources[v1.ResourceQuotas] = resource.MustParse(strconv.Itoa(c + 1))
		usedResources[v1.ResourceConfigMaps] = resource.MustParse(defaultConfigMaps)
		err = waitForResourceQuota(ctx, f.ClientSet, f.Namespace.Name, quotaName, usedResources)
		framework.ExpectNoError(err)

		ginkgo.By("Creating a ConfigMap")
		configMap := newTestConfigMapForQuota("test-configmap")
		configMap, err = f.ClientSet.CoreV1().ConfigMaps(f.Namespace.Name).Create(ctx, configMap, metav1.CreateOptions{})
		framework.ExpectNoError(err)

		ginkgo.By("Ensuring resource quota status captures configMap creation")
		usedResources = v1.ResourceList{}
		usedResources[v1.ResourceQuotas] = resource.MustParse(strconv.Itoa(c + 1))
		usedResources[v1.ResourceConfigMaps] = resource.MustParse(hardConfigMaps)
		err = waitForResourceQuota(ctx, f.ClientSet, f.Namespace.Name, quotaName, usedResources)
		framework.ExpectNoError(err)

		ginkgo.By("Deleting a ConfigMap")
		err = f.ClientSet.CoreV1().ConfigMaps(f.Namespace.Name).Delete(ctx, configMap.Name, metav1.DeleteOptions{})
		framework.ExpectNoError(err)

		ginkgo.By("Ensuring resource quota status released usage")
		usedResources[v1.ResourceConfigMaps] = resource.MustParse(defaultConfigMaps)
		err = waitForResourceQuota(ctx, f.ClientSet, f.Namespace.Name, quotaName, usedResources)
		framework.ExpectNoError(err)
	})

	/*
		Release: v1.16
		Testname: ResourceQuota, object count quota, replicationController
		Description: Create a ResourceQuota. Creation MUST be successful and its ResourceQuotaStatus MUST match to expected used and total allowed resource quota count within namespace.
		Create a ReplicationController. Its creation MUST be successful and resource usage count against the ReplicationController object MUST be captured in ResourceQuotaStatus of the ResourceQuota.
		Delete the ReplicationController. Deletion MUST succeed and resource usage count against the ReplicationController object MUST be released from ResourceQuotaStatus of the ResourceQuota.
	*/
	framework.ConformanceIt("should create a ResourceQuota and capture the life of a replication controller.", func(ctx context.Context) {
		ginkgo.By("Counting existing ResourceQuota")
		c, err := countResourceQuota(ctx, f.ClientSet, f.Namespace.Name)
		framework.ExpectNoError(err)

		ginkgo.By("Creating a ResourceQuota")
		quotaName := "test-quota"
		resourceQuota := newTestResourceQuota(quotaName)
		_, err = createResourceQuota(ctx, f.ClientSet, f.Namespace.Name, resourceQuota)
		framework.ExpectNoError(err)

		ginkgo.By("Ensuring resource quota status is calculated")
		usedResources := v1.ResourceList{}
		usedResources[v1.ResourceQuotas] = resource.MustParse(strconv.Itoa(c + 1))
		usedResources[v1.ResourceReplicationControllers] = resource.MustParse("0")
		err = waitForResourceQuota(ctx, f.ClientSet, f.Namespace.Name, quotaName, usedResources)
		framework.ExpectNoError(err)

		ginkgo.By("Creating a ReplicationController")
		replicationController := newTestReplicationControllerForQuota("test-rc", "nginx", 0)
		replicationController, err = f.ClientSet.CoreV1().ReplicationControllers(f.Namespace.Name).Create(ctx, replicationController, metav1.CreateOptions{})
		framework.ExpectNoError(err)

		ginkgo.By("Ensuring resource quota status captures replication controller creation")
		usedResources = v1.ResourceList{}
		usedResources[v1.ResourceReplicationControllers] = resource.MustParse("1")
		err = waitForResourceQuota(ctx, f.ClientSet, f.Namespace.Name, quotaName, usedResources)
		framework.ExpectNoError(err)

		ginkgo.By("Deleting a ReplicationController")
		// Without the delete options, the object isn't actually
		// removed until the GC verifies that all children have been
		// detached. ReplicationControllers default to "orphan", which
		// is different from most resources. (Why? To preserve a common
		// workflow from prior to the GC's introduction.)
		err = f.ClientSet.CoreV1().ReplicationControllers(f.Namespace.Name).Delete(ctx, replicationController.Name, metav1.DeleteOptions{
			PropagationPolicy: func() *metav1.DeletionPropagation {
				p := metav1.DeletePropagationBackground
				return &p
			}(),
		})
		framework.ExpectNoError(err)

		ginkgo.By("Ensuring resource quota status released usage")
		usedResources[v1.ResourceReplicationControllers] = resource.MustParse("0")
		err = waitForResourceQuota(ctx, f.ClientSet, f.Namespace.Name, quotaName, usedResources)
		framework.ExpectNoError(err)
	})

	/*
		Release: v1.16
		Testname: ResourceQuota, object count quota, replicaSet
		Description: Create a ResourceQuota. Creation MUST be successful and its ResourceQuotaStatus MUST match to expected used and total allowed resource quota count within namespace.
		Create a ReplicaSet. Its creation MUST be successful and resource usage count against the ReplicaSet object MUST be captured in ResourceQuotaStatus of the ResourceQuota.
		Delete the ReplicaSet. Deletion MUST succeed and resource usage count against the ReplicaSet object MUST be released from ResourceQuotaStatus of the ResourceQuota.
	*/
	framework.ConformanceIt("should create a ResourceQuota and capture the life of a replica set.", func(ctx context.Context) {
		ginkgo.By("Counting existing ResourceQuota")
		c, err := countResourceQuota(ctx, f.ClientSet, f.Namespace.Name)
		framework.ExpectNoError(err)

		ginkgo.By("Creating a ResourceQuota")
		quotaName := "test-quota"
		resourceQuota := newTestResourceQuota(quotaName)
		_, err = createResourceQuota(ctx, f.ClientSet, f.Namespace.Name, resourceQuota)
		framework.ExpectNoError(err)

		ginkgo.By("Ensuring resource quota status is calculated")
		usedResources := v1.ResourceList{}
		usedResources[v1.ResourceQuotas] = resource.MustParse(strconv.Itoa(c + 1))
		usedResources[v1.ResourceName("count/replicasets.apps")] = resource.MustParse("0")
		err = waitForResourceQuota(ctx, f.ClientSet, f.Namespace.Name, quotaName, usedResources)
		framework.ExpectNoError(err)

		ginkgo.By("Creating a ReplicaSet")
		replicaSet := newTestReplicaSetForQuota("test-rs", "nginx", 0)
		replicaSet, err = f.ClientSet.AppsV1().ReplicaSets(f.Namespace.Name).Create(ctx, replicaSet, metav1.CreateOptions{})
		framework.ExpectNoError(err)

		ginkgo.By("Ensuring resource quota status captures replicaset creation")
		usedResources = v1.ResourceList{}
		usedResources[v1.ResourceName("count/replicasets.apps")] = resource.MustParse("1")
		err = waitForResourceQuota(ctx, f.ClientSet, f.Namespace.Name, quotaName, usedResources)
		framework.ExpectNoError(err)

		ginkgo.By("Deleting a ReplicaSet")
		err = f.ClientSet.AppsV1().ReplicaSets(f.Namespace.Name).Delete(ctx, replicaSet.Name, metav1.DeleteOptions{})
		framework.ExpectNoError(err)

		ginkgo.By("Ensuring resource quota status released usage")
		usedResources[v1.ResourceName("count/replicasets.apps")] = resource.MustParse("0")
		err = waitForResourceQuota(ctx, f.ClientSet, f.Namespace.Name, quotaName, usedResources)
		framework.ExpectNoError(err)
	})

	/*
		Release: v1.31
		Testname: ResourceQuota, object count quota, ResourceClaim
		Description: Create a ResourceQuota. Creation MUST be successful and its ResourceQuotaStatus MUST match to expected used and total allowed resource quota count within namespace.
		Create ResourceClaim. Creation MUST be successful and resource usage count against the ResourceClaim object MUST be captured in ResourceQuotaStatus of the ResourceQuota.
		Delete the ResourceClaim. Deletion MUST succeed and resource usage count against the ResourceClaim object MUST be released from ResourceQuotaStatus of the ResourceQuota.
		[NotConformancePromotable] alpha feature
	*/
	f.It("should create a ResourceQuota and capture the life of a ResourceClaim", feature.DynamicResourceAllocation, func(ctx context.Context) {
		ginkgo.By("Counting existing ResourceQuota")
		c, err := countResourceQuota(ctx, f.ClientSet, f.Namespace.Name)
		framework.ExpectNoError(err)

		ginkgo.By("Creating a ResourceQuota")
		quotaName := "test-quota"
		resourceQuota := newTestResourceQuotaDRA(quotaName)
		_, err = createResourceQuota(ctx, f.ClientSet, f.Namespace.Name, resourceQuota)
		framework.ExpectNoError(err)

		ginkgo.By("Ensuring resource quota status is calculated")
		usedResources := v1.ResourceList{}
		usedResources[v1.ResourceQuotas] = resource.MustParse(strconv.Itoa(c + 1))
		usedResources[core.ClaimObjectCountName] = resource.MustParse("0")
		usedResources[core.V1ResourceByDeviceClass(classGold)] = resource.MustParse("0")
		err = waitForResourceQuota(ctx, f.ClientSet, f.Namespace.Name, quotaName, usedResources)
		framework.ExpectNoError(err)

		ginkgo.By("Creating a ResourceClaim")
		claim := newTestResourceClaimForQuota("test-claim")
		claim, err = f.ClientSet.ResourceV1beta1().ResourceClaims(f.Namespace.Name).Create(ctx, claim, metav1.CreateOptions{})
		framework.ExpectNoError(err)

		ginkgo.By("Ensuring resource quota status captures resource claim creation")
		usedResources = v1.ResourceList{}
		usedResources[core.ClaimObjectCountName] = resource.MustParse("1")
		usedResources[core.V1ResourceByDeviceClass(classGold)] = resource.MustParse("1")
		err = waitForResourceQuota(ctx, f.ClientSet, f.Namespace.Name, quotaName, usedResources)
		framework.ExpectNoError(err)

		ginkgo.By("Deleting a ResourceClaim")
		err = f.ClientSet.ResourceV1beta1().ResourceClaims(f.Namespace.Name).Delete(ctx, claim.Name, metav1.DeleteOptions{})
		framework.ExpectNoError(err)

		ginkgo.By("Ensuring resource quota status released usage")
		usedResources[core.ClaimObjectCountName] = resource.MustParse("0")
		usedResources[core.V1ResourceByDeviceClass(classGold)] = resource.MustParse("0")
		err = waitForResourceQuota(ctx, f.ClientSet, f.Namespace.Name, quotaName, usedResources)
		framework.ExpectNoError(err)
	})

	/*
		Release: v1.16
		Testname: ResourceQuota, object count quota, pvc
		Description: Create a ResourceQuota. Creation MUST be successful and its ResourceQuotaStatus MUST match to expected used and total allowed resource quota count within namespace.
		Create PersistentVolumeClaim (PVC) to request storage capacity of 1G. PVC creation MUST be successful and resource usage count against the PVC and storage object MUST be captured in ResourceQuotaStatus of the ResourceQuota.
		Delete the PVC. Deletion MUST succeed and resource usage count against its PVC and storage object MUST be released from ResourceQuotaStatus of the ResourceQuota.
		[NotConformancePromotable] as test suite do not have any e2e at this moment which are explicitly verifying PV and PVC behaviour.
	*/
	ginkgo.It("should create a ResourceQuota and capture the life of a persistent volume claim", func(ctx context.Context) {
		ginkgo.By("Counting existing ResourceQuota")
		c, err := countResourceQuota(ctx, f.ClientSet, f.Namespace.Name)
		framework.ExpectNoError(err)

		ginkgo.By("Creating a ResourceQuota")
		quotaName := "test-quota"
		resourceQuota := newTestResourceQuota(quotaName)
		_, err = createResourceQuota(ctx, f.ClientSet, f.Namespace.Name, resourceQuota)
		framework.ExpectNoError(err)

		ginkgo.By("Ensuring resource quota status is calculated")
		usedResources := v1.ResourceList{}
		usedResources[v1.ResourceQuotas] = resource.MustParse(strconv.Itoa(c + 1))
		usedResources[v1.ResourcePersistentVolumeClaims] = resource.MustParse("0")
		usedResources[v1.ResourceRequestsStorage] = resource.MustParse("0")
		err = waitForResourceQuota(ctx, f.ClientSet, f.Namespace.Name, quotaName, usedResources)
		framework.ExpectNoError(err)

		ginkgo.By("Creating a PersistentVolumeClaim")
		pvc := newTestPersistentVolumeClaimForQuota("test-claim")
		pvc, err = f.ClientSet.CoreV1().PersistentVolumeClaims(f.Namespace.Name).Create(ctx, pvc, metav1.CreateOptions{})
		framework.ExpectNoError(err)

		ginkgo.By("Ensuring resource quota status captures persistent volume claim creation")
		usedResources = v1.ResourceList{}
		usedResources[v1.ResourcePersistentVolumeClaims] = resource.MustParse("1")
		usedResources[v1.ResourceRequestsStorage] = resource.MustParse("1Gi")
		err = waitForResourceQuota(ctx, f.ClientSet, f.Namespace.Name, quotaName, usedResources)
		framework.ExpectNoError(err)

		ginkgo.By("Deleting a PersistentVolumeClaim")
		err = f.ClientSet.CoreV1().PersistentVolumeClaims(f.Namespace.Name).Delete(ctx, pvc.Name, metav1.DeleteOptions{})
		framework.ExpectNoError(err)

		ginkgo.By("Ensuring resource quota status released usage")
		usedResources[v1.ResourcePersistentVolumeClaims] = resource.MustParse("0")
		usedResources[v1.ResourceRequestsStorage] = resource.MustParse("0")
		err = waitForResourceQuota(ctx, f.ClientSet, f.Namespace.Name, quotaName, usedResources)
		framework.ExpectNoError(err)
	})

	/*
		Release: v1.16
		Testname: ResourceQuota, object count quota, storageClass
		Description: Create a ResourceQuota. Creation MUST be successful and its ResourceQuotaStatus MUST match to expected used and total allowed resource quota count within namespace.
		Create PersistentVolumeClaim (PVC) with specified storageClass to request storage capacity of 1G. PVC creation MUST be successful and resource usage count against PVC, storageClass and storage object MUST be captured in ResourceQuotaStatus of the ResourceQuota.
		Delete the PVC. Deletion MUST succeed and resource usage count against  PVC, storageClass and storage object MUST be released from ResourceQuotaStatus of the ResourceQuota.
		[NotConformancePromotable] as test suite do not have any e2e at this moment which are explicitly verifying PV and PVC behaviour.
	*/
	ginkgo.It("should create a ResourceQuota and capture the life of a persistent volume claim with a storage class", func(ctx context.Context) {
		ginkgo.By("Counting existing ResourceQuota")
		c, err := countResourceQuota(ctx, f.ClientSet, f.Namespace.Name)
		framework.ExpectNoError(err)

		ginkgo.By("Creating a ResourceQuota")
		quotaName := "test-quota"
		resourceQuota := newTestResourceQuota(quotaName)
		_, err = createResourceQuota(ctx, f.ClientSet, f.Namespace.Name, resourceQuota)
		framework.ExpectNoError(err)

		ginkgo.By("Ensuring resource quota status is calculated")
		usedResources := v1.ResourceList{}
		usedResources[v1.ResourceQuotas] = resource.MustParse(strconv.Itoa(c + 1))
		usedResources[v1.ResourcePersistentVolumeClaims] = resource.MustParse("0")
		usedResources[v1.ResourceRequestsStorage] = resource.MustParse("0")
		usedResources[core.V1ResourceByStorageClass(classGold, v1.ResourcePersistentVolumeClaims)] = resource.MustParse("0")
		usedResources[core.V1ResourceByStorageClass(classGold, v1.ResourceRequestsStorage)] = resource.MustParse("0")

		err = waitForResourceQuota(ctx, f.ClientSet, f.Namespace.Name, quotaName, usedResources)
		framework.ExpectNoError(err)

		ginkgo.By("Creating a PersistentVolumeClaim with storage class")
		pvc := newTestPersistentVolumeClaimForQuota("test-claim")
		pvc.Spec.StorageClassName = &classGold
		pvc, err = f.ClientSet.CoreV1().PersistentVolumeClaims(f.Namespace.Name).Create(ctx, pvc, metav1.CreateOptions{})
		framework.ExpectNoError(err)

		ginkgo.By("Ensuring resource quota status captures persistent volume claim creation")
		usedResources = v1.ResourceList{}
		usedResources[v1.ResourcePersistentVolumeClaims] = resource.MustParse("1")
		usedResources[v1.ResourceRequestsStorage] = resource.MustParse("1Gi")
		usedResources[core.V1ResourceByStorageClass(classGold, v1.ResourcePersistentVolumeClaims)] = resource.MustParse("1")
		usedResources[core.V1ResourceByStorageClass(classGold, v1.ResourceRequestsStorage)] = resource.MustParse("1Gi")

		err = waitForResourceQuota(ctx, f.ClientSet, f.Namespace.Name, quotaName, usedResources)
		framework.ExpectNoError(err)

		ginkgo.By("Deleting a PersistentVolumeClaim")
		err = f.ClientSet.CoreV1().PersistentVolumeClaims(f.Namespace.Name).Delete(ctx, pvc.Name, metav1.DeleteOptions{})
		framework.ExpectNoError(err)

		ginkgo.By("Ensuring resource quota status released usage")
		usedResources[v1.ResourcePersistentVolumeClaims] = resource.MustParse("0")
		usedResources[v1.ResourceRequestsStorage] = resource.MustParse("0")
		usedResources[core.V1ResourceByStorageClass(classGold, v1.ResourcePersistentVolumeClaims)] = resource.MustParse("0")
		usedResources[core.V1ResourceByStorageClass(classGold, v1.ResourceRequestsStorage)] = resource.MustParse("0")

		err = waitForResourceQuota(ctx, f.ClientSet, f.Namespace.Name, quotaName, usedResources)
		framework.ExpectNoError(err)
	})

	ginkgo.It("should create a ResourceQuota and capture the life of a custom resource.", func(ctx context.Context) {
		ginkgo.By("Creating a Custom Resource Definition")
		testcrd, err := crd.CreateTestCRD(f)
		framework.ExpectNoError(err)
		ginkgo.DeferCleanup(testcrd.CleanUp)
		countResourceName := "count/" + testcrd.Crd.Spec.Names.Plural + "." + testcrd.Crd.Spec.Group
		// resourcequota controller needs to take 30 seconds at most to detect the new custom resource.
		// in order to make sure the resourcequota controller knows this resource, we create one test
		// resourcequota object, and triggering updates on it until the status is updated.
		quotaName := "quota-for-" + testcrd.Crd.Spec.Names.Plural
		_, err = createResourceQuota(ctx, f.ClientSet, f.Namespace.Name, &v1.ResourceQuota{
			ObjectMeta: metav1.ObjectMeta{Name: quotaName},
			Spec: v1.ResourceQuotaSpec{
				Hard: v1.ResourceList{
					v1.ResourceName(countResourceName): resource.MustParse("0"),
				},
			},
		})
		framework.ExpectNoError(err)
		err = updateResourceQuotaUntilUsageAppears(ctx, f.ClientSet, f.Namespace.Name, quotaName, v1.ResourceName(countResourceName))
		framework.ExpectNoError(err)
		err = f.ClientSet.CoreV1().ResourceQuotas(f.Namespace.Name).Delete(ctx, quotaName, metav1.DeleteOptions{})
		framework.ExpectNoError(err)

		ginkgo.By("Counting existing ResourceQuota")
		c, err := countResourceQuota(ctx, f.ClientSet, f.Namespace.Name)
		framework.ExpectNoError(err)

		ginkgo.By("Creating a ResourceQuota")
		quotaName = "test-quota"
		resourceQuota := newTestResourceQuota(quotaName)
		resourceQuota.Spec.Hard[v1.ResourceName(countResourceName)] = resource.MustParse("1")
		_, err = createResourceQuota(ctx, f.ClientSet, f.Namespace.Name, resourceQuota)
		framework.ExpectNoError(err)

		ginkgo.By("Ensuring resource quota status is calculated")
		usedResources := v1.ResourceList{}
		usedResources[v1.ResourceQuotas] = resource.MustParse(strconv.Itoa(c + 1))
		usedResources[v1.ResourceName(countResourceName)] = resource.MustParse("0")
		err = waitForResourceQuota(ctx, f.ClientSet, f.Namespace.Name, quotaName, usedResources)
		framework.ExpectNoError(err)

		ginkgo.By("Creating a custom resource")
		resourceClient := testcrd.DynamicClients["v1"]
		testcr, err := instantiateCustomResource(ctx, &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": testcrd.Crd.Spec.Group + "/" + testcrd.Crd.Spec.Versions[0].Name,
				"kind":       testcrd.Crd.Spec.Names.Kind,
				"metadata": map[string]interface{}{
					"name": "test-cr-1",
				},
			},
		}, resourceClient, testcrd.Crd)
		framework.ExpectNoError(err)

		ginkgo.By("Ensuring resource quota status captures custom resource creation")
		usedResources = v1.ResourceList{}
		usedResources[v1.ResourceName(countResourceName)] = resource.MustParse("1")
		err = waitForResourceQuota(ctx, f.ClientSet, f.Namespace.Name, quotaName, usedResources)
		framework.ExpectNoError(err)

		ginkgo.By("Creating a second custom resource")
		_, err = instantiateCustomResource(ctx, &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": testcrd.Crd.Spec.Group + "/" + testcrd.Crd.Spec.Versions[0].Name,
				"kind":       testcrd.Crd.Spec.Names.Kind,
				"metadata": map[string]interface{}{
					"name": "test-cr-2",
				},
			},
		}, resourceClient, testcrd.Crd)
		// since we only give one quota, this creation should fail.
		gomega.Expect(err).To(gomega.HaveOccurred())

		ginkgo.By("Deleting a custom resource")
		err = deleteCustomResource(ctx, resourceClient, testcr.GetName())
		framework.ExpectNoError(err)

		ginkgo.By("Ensuring resource quota status released usage")
		usedResources[v1.ResourceName(countResourceName)] = resource.MustParse("0")
		err = waitForResourceQuota(ctx, f.ClientSet, f.Namespace.Name, quotaName, usedResources)
		framework.ExpectNoError(err)
	})

	/*
		Release: v1.16
		Testname: ResourceQuota, quota scope, Terminating and NotTerminating scope
		Description: Create two ResourceQuotas, one with 'Terminating' scope and another 'NotTerminating' scope. Request and the limit counts for CPU and Memory resources are set for the ResourceQuota. Creation MUST be successful and their ResourceQuotaStatus MUST match to expected used and total allowed resource quota count within namespace.
		Create a Pod with specified CPU and Memory ResourceRequirements fall within quota limits. Pod creation MUST be successful and usage count MUST be captured in ResourceQuotaStatus of 'NotTerminating' scoped ResourceQuota but MUST NOT in 'Terminating' scoped ResourceQuota.
		Delete the Pod. Pod deletion MUST succeed and Pod resource usage count MUST be released from ResourceQuotaStatus of 'NotTerminating' scoped ResourceQuota.
		Create a pod with specified activeDeadlineSeconds and resourceRequirements for CPU and Memory fall within quota limits. Pod creation MUST be successful and usage count MUST be captured in ResourceQuotaStatus of 'Terminating' scoped ResourceQuota but MUST NOT in 'NotTerminating' scoped ResourceQuota.
		Delete the Pod. Pod deletion MUST succeed and Pod resource usage count MUST be released from ResourceQuotaStatus of 'Terminating' scoped ResourceQuota.
	*/
	framework.ConformanceIt("should verify ResourceQuota with terminating scopes.", func(ctx context.Context) {
		ginkgo.By("Creating a ResourceQuota with terminating scope")
		quotaTerminatingName := "quota-terminating"
		resourceQuotaTerminating, err := createResourceQuota(ctx, f.ClientSet, f.Namespace.Name, newTestResourceQuotaWithScope(quotaTerminatingName, v1.ResourceQuotaScopeTerminating))
		framework.ExpectNoError(err)

		ginkgo.By("Ensuring ResourceQuota status is calculated")
		usedResources := v1.ResourceList{}
		usedResources[v1.ResourcePods] = resource.MustParse("0")
		err = waitForResourceQuota(ctx, f.ClientSet, f.Namespace.Name, resourceQuotaTerminating.Name, usedResources)
		framework.ExpectNoError(err)

		ginkgo.By("Creating a ResourceQuota with not terminating scope")
		quotaNotTerminatingName := "quota-not-terminating"
		resourceQuotaNotTerminating, err := createResourceQuota(ctx, f.ClientSet, f.Namespace.Name, newTestResourceQuotaWithScope(quotaNotTerminatingName, v1.ResourceQuotaScopeNotTerminating))
		framework.ExpectNoError(err)

		ginkgo.By("Ensuring ResourceQuota status is calculated")
		err = waitForResourceQuota(ctx, f.ClientSet, f.Namespace.Name, resourceQuotaNotTerminating.Name, usedResources)
		framework.ExpectNoError(err)

		ginkgo.By("Creating a long running pod")
		podName := "test-pod"
		requests := v1.ResourceList{}
		requests[v1.ResourceCPU] = resource.MustParse("500m")
		requests[v1.ResourceMemory] = resource.MustParse("200Mi")
		limits := v1.ResourceList{}
		limits[v1.ResourceCPU] = resource.MustParse("1")
		limits[v1.ResourceMemory] = resource.MustParse("400Mi")
		pod := newTestPodForQuota(f, podName, requests, limits)
		_, err = f.ClientSet.CoreV1().Pods(f.Namespace.Name).Create(ctx, pod, metav1.CreateOptions{})
		framework.ExpectNoError(err)

		ginkgo.By("Ensuring resource quota with not terminating scope captures the pod usage")
		usedResources[v1.ResourcePods] = resource.MustParse("1")
		usedResources[v1.ResourceRequestsCPU] = requests[v1.ResourceCPU]
		usedResources[v1.ResourceRequestsMemory] = requests[v1.ResourceMemory]
		usedResources[v1.ResourceLimitsCPU] = limits[v1.ResourceCPU]
		usedResources[v1.ResourceLimitsMemory] = limits[v1.ResourceMemory]
		err = waitForResourceQuota(ctx, f.ClientSet, f.Namespace.Name, resourceQuotaNotTerminating.Name, usedResources)
		framework.ExpectNoError(err)

		ginkgo.By("Ensuring resource quota with terminating scope ignored the pod usage")
		usedResources[v1.ResourcePods] = resource.MustParse("0")
		usedResources[v1.ResourceRequestsCPU] = resource.MustParse("0")
		usedResources[v1.ResourceRequestsMemory] = resource.MustParse("0")
		usedResources[v1.ResourceLimitsCPU] = resource.MustParse("0")
		usedResources[v1.ResourceLimitsMemory] = resource.MustParse("0")
		err = waitForResourceQuota(ctx, f.ClientSet, f.Namespace.Name, resourceQuotaTerminating.Name, usedResources)
		framework.ExpectNoError(err)

		ginkgo.By("Deleting the pod")
		err = f.ClientSet.CoreV1().Pods(f.Namespace.Name).Delete(ctx, podName, *metav1.NewDeleteOptions(0))
		framework.ExpectNoError(err)

		ginkgo.By("Ensuring resource quota status released the pod usage")
		usedResources[v1.ResourcePods] = resource.MustParse("0")
		usedResources[v1.ResourceRequestsCPU] = resource.MustParse("0")
		usedResources[v1.ResourceRequestsMemory] = resource.MustParse("0")
		usedResources[v1.ResourceLimitsCPU] = resource.MustParse("0")
		usedResources[v1.ResourceLimitsMemory] = resource.MustParse("0")
		err = waitForResourceQuota(ctx, f.ClientSet, f.Namespace.Name, resourceQuotaNotTerminating.Name, usedResources)
		framework.ExpectNoError(err)

		ginkgo.By("Creating a terminating pod")
		podName = "terminating-pod"
		pod = newTestPodForQuota(f, podName, requests, limits)
		activeDeadlineSeconds := int64(3600)
		pod.Spec.ActiveDeadlineSeconds = &activeDeadlineSeconds
		_, err = f.ClientSet.CoreV1().Pods(f.Namespace.Name).Create(ctx, pod, metav1.CreateOptions{})
		framework.ExpectNoError(err)

		ginkgo.By("Ensuring resource quota with terminating scope captures the pod usage")
		usedResources[v1.ResourcePods] = resource.MustParse("1")
		usedResources[v1.ResourceRequestsCPU] = requests[v1.ResourceCPU]
		usedResources[v1.ResourceRequestsMemory] = requests[v1.ResourceMemory]
		usedResources[v1.ResourceLimitsCPU] = limits[v1.ResourceCPU]
		usedResources[v1.ResourceLimitsMemory] = limits[v1.ResourceMemory]
		err = waitForResourceQuota(ctx, f.ClientSet, f.Namespace.Name, resourceQuotaTerminating.Name, usedResources)
		framework.ExpectNoError(err)

		ginkgo.By("Ensuring resource quota with not terminating scope ignored the pod usage")
		usedResources[v1.ResourcePods] = resource.MustParse("0")
		usedResources[v1.ResourceRequestsCPU] = resource.MustParse("0")
		usedResources[v1.ResourceRequestsMemory] = resource.MustParse("0")
		usedResources[v1.ResourceLimitsCPU] = resource.MustParse("0")
		usedResources[v1.ResourceLimitsMemory] = resource.MustParse("0")
		err = waitForResourceQuota(ctx, f.ClientSet, f.Namespace.Name, resourceQuotaNotTerminating.Name, usedResources)
		framework.ExpectNoError(err)

		ginkgo.By("Deleting the pod")
		err = f.ClientSet.CoreV1().Pods(f.Namespace.Name).Delete(ctx, podName, *metav1.NewDeleteOptions(0))
		framework.ExpectNoError(err)

		ginkgo.By("Ensuring resource quota status released the pod usage")
		usedResources[v1.ResourcePods] = resource.MustParse("0")
		usedResources[v1.ResourceRequestsCPU] = resource.MustParse("0")
		usedResources[v1.ResourceRequestsMemory] = resource.MustParse("0")
		usedResources[v1.ResourceLimitsCPU] = resource.MustParse("0")
		usedResources[v1.ResourceLimitsMemory] = resource.MustParse("0")
		err = waitForResourceQuota(ctx, f.ClientSet, f.Namespace.Name, resourceQuotaTerminating.Name, usedResources)
		framework.ExpectNoError(err)
	})

	/*
		Release: v1.16
		Testname: ResourceQuota, quota scope, BestEffort and NotBestEffort scope
		Description: Create two ResourceQuotas, one with 'BestEffort' scope and another with 'NotBestEffort' scope. Creation MUST be successful and their ResourceQuotaStatus MUST match to expected used and total allowed resource quota count within namespace.
		Create a 'BestEffort' Pod by not explicitly specifying resource limits and requests. Pod creation MUST be successful and usage count MUST be captured in ResourceQuotaStatus of 'BestEffort' scoped ResourceQuota but MUST NOT in 'NotBestEffort' scoped ResourceQuota.
		Delete the Pod. Pod deletion MUST succeed and Pod resource usage count MUST be released from ResourceQuotaStatus of 'BestEffort' scoped ResourceQuota.
		Create a 'NotBestEffort' Pod by explicitly specifying resource limits and requests. Pod creation MUST be successful and usage count MUST be captured in ResourceQuotaStatus of 'NotBestEffort' scoped ResourceQuota but MUST NOT in 'BestEffort' scoped ResourceQuota.
		Delete the Pod. Pod deletion MUST succeed and Pod resource usage count MUST be released from ResourceQuotaStatus of 'NotBestEffort' scoped ResourceQuota.
	*/
	framework.ConformanceIt("should verify ResourceQuota with best effort scope.", func(ctx context.Context) {
		ginkgo.By("Creating a ResourceQuota with best effort scope")
		resourceQuotaBestEffort, err := createResourceQuota(ctx, f.ClientSet, f.Namespace.Name, newTestResourceQuotaWithScope("quota-besteffort", v1.ResourceQuotaScopeBestEffort))
		framework.ExpectNoError(err)

		ginkgo.By("Ensuring ResourceQuota status is calculated")
		usedResources := v1.ResourceList{}
		usedResources[v1.ResourcePods] = resource.MustParse("0")
		err = waitForResourceQuota(ctx, f.ClientSet, f.Namespace.Name, resourceQuotaBestEffort.Name, usedResources)
		framework.ExpectNoError(err)

		ginkgo.By("Creating a ResourceQuota with not best effort scope")
		resourceQuotaNotBestEffort, err := createResourceQuota(ctx, f.ClientSet, f.Namespace.Name, newTestResourceQuotaWithScope("quota-not-besteffort", v1.ResourceQuotaScopeNotBestEffort))
		framework.ExpectNoError(err)

		ginkgo.By("Ensuring ResourceQuota status is calculated")
		err = waitForResourceQuota(ctx, f.ClientSet, f.Namespace.Name, resourceQuotaNotBestEffort.Name, usedResources)
		framework.ExpectNoError(err)

		ginkgo.By("Creating a best-effort pod")
		pod := newTestPodForQuota(f, podName, v1.ResourceList{}, v1.ResourceList{})
		pod, err = f.ClientSet.CoreV1().Pods(f.Namespace.Name).Create(ctx, pod, metav1.CreateOptions{})
		framework.ExpectNoError(err)

		ginkgo.By("Ensuring resource quota with best effort scope captures the pod usage")
		usedResources[v1.ResourcePods] = resource.MustParse("1")
		err = waitForResourceQuota(ctx, f.ClientSet, f.Namespace.Name, resourceQuotaBestEffort.Name, usedResources)
		framework.ExpectNoError(err)

		ginkgo.By("Ensuring resource quota with not best effort ignored the pod usage")
		usedResources[v1.ResourcePods] = resource.MustParse("0")
		err = waitForResourceQuota(ctx, f.ClientSet, f.Namespace.Name, resourceQuotaNotBestEffort.Name, usedResources)
		framework.ExpectNoError(err)

		ginkgo.By("Deleting the pod")
		err = f.ClientSet.CoreV1().Pods(f.Namespace.Name).Delete(ctx, pod.Name, *metav1.NewDeleteOptions(0))
		framework.ExpectNoError(err)

		ginkgo.By("Ensuring resource quota status released the pod usage")
		usedResources[v1.ResourcePods] = resource.MustParse("0")
		err = waitForResourceQuota(ctx, f.ClientSet, f.Namespace.Name, resourceQuotaBestEffort.Name, usedResources)
		framework.ExpectNoError(err)

		ginkgo.By("Creating a not best-effort pod")
		requests := v1.ResourceList{}
		requests[v1.ResourceCPU] = resource.MustParse("500m")
		requests[v1.ResourceMemory] = resource.MustParse("200Mi")
		limits := v1.ResourceList{}
		limits[v1.ResourceCPU] = resource.MustParse("1")
		limits[v1.ResourceMemory] = resource.MustParse("400Mi")
		pod = newTestPodForQuota(f, "burstable-pod", requests, limits)
		pod, err = f.ClientSet.CoreV1().Pods(f.Namespace.Name).Create(ctx, pod, metav1.CreateOptions{})
		framework.ExpectNoError(err)

		ginkgo.By("Ensuring resource quota with not best effort scope captures the pod usage")
		usedResources[v1.ResourcePods] = resource.MustParse("1")
		err = waitForResourceQuota(ctx, f.ClientSet, f.Namespace.Name, resourceQuotaNotBestEffort.Name, usedResources)
		framework.ExpectNoError(err)

		ginkgo.By("Ensuring resource quota with best effort scope ignored the pod usage")
		usedResources[v1.ResourcePods] = resource.MustParse("0")
		err = waitForResourceQuota(ctx, f.ClientSet, f.Namespace.Name, resourceQuotaBestEffort.Name, usedResources)
		framework.ExpectNoError(err)

		ginkgo.By("Deleting the pod")
		err = f.ClientSet.CoreV1().Pods(f.Namespace.Name).Delete(ctx, pod.Name, *metav1.NewDeleteOptions(0))
		framework.ExpectNoError(err)

		ginkgo.By("Ensuring resource quota status released the pod usage")
		usedResources[v1.ResourcePods] = resource.MustParse("0")
		err = waitForResourceQuota(ctx, f.ClientSet, f.Namespace.Name, resourceQuotaNotBestEffort.Name, usedResources)
		framework.ExpectNoError(err)
	})

	/*
		Release: v1.16
		Testname: ResourceQuota, update and delete
		Description: Create a ResourceQuota for CPU and Memory quota limits. Creation MUST be successful.
		When ResourceQuota is updated to modify CPU and Memory quota limits, update MUST succeed with updated values for CPU and Memory limits.
		When ResourceQuota is deleted, it MUST not be available in the namespace.
	*/
	framework.ConformanceIt("should be able to update and delete ResourceQuota.", func(ctx context.Context) {
		client := f.ClientSet
		ns := f.Namespace.Name

		ginkgo.By("Creating a ResourceQuota")
		quotaName := "test-quota"
		resourceQuota := &v1.ResourceQuota{
			Spec: v1.ResourceQuotaSpec{
				Hard: v1.ResourceList{},
			},
		}
		resourceQuota.ObjectMeta.Name = quotaName
		resourceQuota.Spec.Hard[v1.ResourceCPU] = resource.MustParse("1")
		resourceQuota.Spec.Hard[v1.ResourceMemory] = resource.MustParse("500Mi")
		_, err := createResourceQuota(ctx, client, ns, resourceQuota)
		framework.ExpectNoError(err)

		ginkgo.By("Getting a ResourceQuota")
		resourceQuotaResult, err := client.CoreV1().ResourceQuotas(ns).Get(ctx, quotaName, metav1.GetOptions{})
		framework.ExpectNoError(err)
		gomega.Expect(resourceQuotaResult.Spec.Hard).To(gomega.HaveKeyWithValue(v1.ResourceCPU, resource.MustParse("1")))
		gomega.Expect(resourceQuotaResult.Spec.Hard).To(gomega.HaveKeyWithValue(v1.ResourceMemory, resource.MustParse("500Mi")))

		ginkgo.By("Updating a ResourceQuota")
		resourceQuota.Spec.Hard[v1.ResourceCPU] = resource.MustParse("2")
		resourceQuota.Spec.Hard[v1.ResourceMemory] = resource.MustParse("1Gi")
		resourceQuotaResult, err = client.CoreV1().ResourceQuotas(ns).Update(ctx, resourceQuota, metav1.UpdateOptions{})
		framework.ExpectNoError(err)
		gomega.Expect(resourceQuotaResult.Spec.Hard).To(gomega.HaveKeyWithValue(v1.ResourceCPU, resource.MustParse("2")))
		gomega.Expect(resourceQuotaResult.Spec.Hard).To(gomega.HaveKeyWithValue(v1.ResourceMemory, resource.MustParse("1Gi")))

		ginkgo.By("Verifying a ResourceQuota was modified")
		resourceQuotaResult, err = client.CoreV1().ResourceQuotas(ns).Get(ctx, quotaName, metav1.GetOptions{})
		framework.ExpectNoError(err)
		gomega.Expect(resourceQuotaResult.Spec.Hard).To(gomega.HaveKeyWithValue(v1.ResourceCPU, resource.MustParse("2")))
		gomega.Expect(resourceQuotaResult.Spec.Hard).To(gomega.HaveKeyWithValue(v1.ResourceMemory, resource.MustParse("1Gi")))

		ginkgo.By("Deleting a ResourceQuota")
		err = deleteResourceQuota(ctx, client, ns, quotaName)
		framework.ExpectNoError(err)

		ginkgo.By("Verifying the deleted ResourceQuota")
		_, err = client.CoreV1().ResourceQuotas(ns).Get(ctx, quotaName, metav1.GetOptions{})
		if !apierrors.IsNotFound(err) {
			framework.Failf("Expected `not found` error, got: %v", err)
		}
	})

	/*
		Release: v1.25
		Testname: ResourceQuota, manage lifecycle of a ResourceQuota
		Description: Attempt to create a ResourceQuota for CPU and Memory
		quota limits. Creation MUST be successful. Attempt to list all
		namespaces with a label selector which MUST succeed. One list
		MUST be found. The ResourceQuota when patched MUST succeed.
		Given the patching of the ResourceQuota, the fields MUST equal
		the new values. It MUST succeed at deleting a collection of
		ResourceQuota via a label selector.
	*/
	framework.ConformanceIt("should manage the lifecycle of a ResourceQuota", func(ctx context.Context) {
		client := f.ClientSet
		ns := f.Namespace.Name

		rqName := "e2e-quota-" + utilrand.String(5)
		label := map[string]string{"e2e-rq-label": rqName}
		labelSelector := labels.SelectorFromSet(label).String()

		ginkgo.By("Creating a ResourceQuota")
		resourceQuota := &v1.ResourceQuota{
			ObjectMeta: metav1.ObjectMeta{
				Name:   rqName,
				Labels: label,
			},
			Spec: v1.ResourceQuotaSpec{
				Hard: v1.ResourceList{},
			},
		}
		resourceQuota.Spec.Hard[v1.ResourceCPU] = resource.MustParse("1")
		resourceQuota.Spec.Hard[v1.ResourceMemory] = resource.MustParse("500Mi")
		_, err := createResourceQuota(ctx, client, ns, resourceQuota)
		framework.ExpectNoError(err)

		ginkgo.By("Getting a ResourceQuota")
		resourceQuotaResult, err := client.CoreV1().ResourceQuotas(ns).Get(ctx, rqName, metav1.GetOptions{})
		framework.ExpectNoError(err)
		gomega.Expect(resourceQuotaResult.Spec.Hard[v1.ResourceCPU]).To(gomega.Equal(resource.MustParse("1")))
		gomega.Expect(resourceQuotaResult.Spec.Hard[v1.ResourceMemory]).To(gomega.Equal(resource.MustParse("500Mi")))

		ginkgo.By("Listing all ResourceQuotas with LabelSelector")
		rq, err := client.CoreV1().ResourceQuotas("").List(ctx, metav1.ListOptions{LabelSelector: labelSelector})
		framework.ExpectNoError(err, "Failed to list job. %v", err)
		gomega.Expect(rq.Items).To(gomega.HaveLen(1), "Failed to find ResourceQuotes %v", rqName)

		ginkgo.By("Patching the ResourceQuota")
		payload := "{\"metadata\":{\"labels\":{\"" + rqName + "\":\"patched\"}},\"spec\":{\"hard\":{ \"memory\":\"750Mi\"}}}"
		patchedResourceQuota, err := client.CoreV1().ResourceQuotas(ns).Patch(ctx, rqName, types.StrategicMergePatchType, []byte(payload), metav1.PatchOptions{})
		framework.ExpectNoError(err, "failed to patch ResourceQuota %s in namespace %s", rqName, ns)
		gomega.Expect(patchedResourceQuota.Labels[rqName]).To(gomega.Equal("patched"), "Failed to find the label for this ResourceQuota. Current labels: %v", patchedResourceQuota.Labels)
		gomega.Expect(*patchedResourceQuota.Spec.Hard.Memory()).To(gomega.Equal(resource.MustParse("750Mi")), "Hard memory value for ResourceQuota %q is %s not 750Mi.", patchedResourceQuota.ObjectMeta.Name, patchedResourceQuota.Spec.Hard.Memory().String())

		ginkgo.By("Deleting a Collection of ResourceQuotas")
		err = client.CoreV1().ResourceQuotas(ns).DeleteCollection(ctx, metav1.DeleteOptions{}, metav1.ListOptions{LabelSelector: labelSelector})
		framework.ExpectNoError(err)

		ginkgo.By("Verifying the deleted ResourceQuota")
		_, err = client.CoreV1().ResourceQuotas(ns).Get(ctx, rqName, metav1.GetOptions{})
		if !apierrors.IsNotFound(err) {
			framework.Failf("Expected `not found` error, got: %v", err)
		}
	})

	/*
		Release: v1.26
		Testname: ResourceQuota, apply changes to a ResourceQuota status
		Description: Attempt to create a ResourceQuota for CPU and Memory
		quota limits. Creation MUST be successful. Updating the hard
		status values MUST succeed and the new values MUST be found. The
		reported hard status values MUST equal the spec hard values.
		Patching the spec hard values MUST succeed and the new values MUST
		be found. Patching the hard status values MUST succeed. The
		reported hard status values MUST equal the new spec hard values.
		Getting the /status MUST succeed and the reported hard status
		values MUST equal the spec hard values. Repatching the hard status
		values MUST succeed. The spec MUST NOT be changed when
		patching /status.
	*/
	framework.ConformanceIt("should apply changes to a resourcequota status", func(ctx context.Context) {
		ns := f.Namespace.Name
		rqClient := f.ClientSet.CoreV1().ResourceQuotas(ns)
		rqName := "e2e-rq-status-" + utilrand.String(5)
		label := map[string]string{"e2e-rq-label": rqName}
		labelSelector := labels.SelectorFromSet(label).String()

		w := &cache.ListWatch{
			WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
				options.LabelSelector = labelSelector
				return rqClient.Watch(ctx, options)
			},
		}

		rqList, err := f.ClientSet.CoreV1().ResourceQuotas("").List(ctx, metav1.ListOptions{LabelSelector: labelSelector})
		framework.ExpectNoError(err, "failed to list Services")

		ginkgo.By(fmt.Sprintf("Creating resourceQuota %q", rqName))
		resourceQuota := &v1.ResourceQuota{
			ObjectMeta: metav1.ObjectMeta{
				Name:   rqName,
				Labels: label,
			},
			Spec: v1.ResourceQuotaSpec{
				Hard: v1.ResourceList{
					v1.ResourceCPU:    resource.MustParse("500m"),
					v1.ResourceMemory: resource.MustParse("500Mi"),
				},
			},
		}
		_, err = createResourceQuota(ctx, f.ClientSet, ns, resourceQuota)
		framework.ExpectNoError(err)

		initialResourceQuota, err := rqClient.Get(ctx, rqName, metav1.GetOptions{})
		framework.ExpectNoError(err)
		gomega.Expect(*initialResourceQuota.Spec.Hard.Cpu()).To(gomega.Equal(resource.MustParse("500m")), "Hard cpu value for ResourceQuota %q is %s not 500m.", initialResourceQuota.Name, initialResourceQuota.Spec.Hard.Cpu().String())
		framework.Logf("Resource quota %q reports spec: hard cpu limit of %s", rqName, initialResourceQuota.Spec.Hard.Cpu())
		gomega.Expect(*initialResourceQuota.Spec.Hard.Memory()).To(gomega.Equal(resource.MustParse("500Mi")), "Hard memory value for ResourceQuota %q is %s not 500Mi.", initialResourceQuota.Name, initialResourceQuota.Spec.Hard.Memory().String())
		framework.Logf("Resource quota %q reports spec: hard memory limit of %s", rqName, initialResourceQuota.Spec.Hard.Memory())

		ginkgo.By(fmt.Sprintf("Updating resourceQuota %q /status", rqName))
		var updatedResourceQuota *v1.ResourceQuota
		hardLimits := quota.Add(v1.ResourceList{}, initialResourceQuota.Spec.Hard)

		err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
			updateStatus, err := rqClient.Get(ctx, rqName, metav1.GetOptions{})
			framework.ExpectNoError(err, "Unable to get ResourceQuota %q", rqName)
			updateStatus.Status = v1.ResourceQuotaStatus{
				Hard: hardLimits,
			}
			updatedResourceQuota, err = rqClient.UpdateStatus(ctx, updateStatus, metav1.UpdateOptions{})
			return err
		})
		framework.ExpectNoError(err, "Failed to update resourceQuota")

		ginkgo.By(fmt.Sprintf("Confirm /status for %q resourceQuota via watch", rqName))
		ctxUntil, cancel := context.WithTimeout(ctx, f.Timeouts.PodStartShort)
		defer cancel()

		_, err = watchtools.Until(ctxUntil, rqList.ResourceVersion, w, func(event watch.Event) (bool, error) {
			if rq, ok := event.Object.(*v1.ResourceQuota); ok {
				found := rq.Name == updatedResourceQuota.Name &&
					rq.Namespace == ns &&
					apiequality.Semantic.DeepEqual(rq.Status.Hard, updatedResourceQuota.Spec.Hard)
				if !found {
					framework.Logf("observed resourceQuota %q in namespace %q with hard status: %#v", rq.Name, rq.Namespace, rq.Status.Hard)
					return false, nil
				}
				framework.Logf("Found resourceQuota %q in namespace %q with hard status: %#v", rq.Name, rq.Namespace, rq.Status.Hard)
				return found, nil
			}
			framework.Logf("Observed event: %+v", event.Object)
			return false, nil
		})
		framework.ExpectNoError(err, "failed to locate ResourceQuota %q in namespace %q", updatedResourceQuota.Name, ns)
		framework.Logf("ResourceQuota %q /status was updated", updatedResourceQuota.Name)

		// Sync resourceQuota list before patching /status
		rqList, err = f.ClientSet.CoreV1().ResourceQuotas("").List(ctx, metav1.ListOptions{LabelSelector: labelSelector})
		framework.ExpectNoError(err, "failed to list Services")

		ginkgo.By("Patching hard spec values for cpu & memory")
		xResourceQuota, err := rqClient.Patch(ctx, updatedResourceQuota.Name, types.StrategicMergePatchType,
			[]byte(`{"spec":{"hard":{"cpu":"1","memory":"1Gi"}}}`),
			metav1.PatchOptions{})
		framework.ExpectNoError(err, "Could not patch resourcequota %q. Error: %v", xResourceQuota.Name, err)
		framework.Logf("Resource quota %q reports spec: hard cpu limit of %s", rqName, xResourceQuota.Spec.Hard.Cpu())
		framework.Logf("Resource quota %q reports spec: hard memory limit of %s", rqName, xResourceQuota.Spec.Hard.Memory())

		ginkgo.By(fmt.Sprintf("Patching %q /status", rqName))
		hardLimits = quota.Add(v1.ResourceList{}, xResourceQuota.Spec.Hard)

		rqStatusJSON, err := json.Marshal(hardLimits)
		framework.ExpectNoError(err)
		patchedResourceQuota, err := rqClient.Patch(ctx, rqName, types.StrategicMergePatchType,
			[]byte(`{"status": {"hard": `+string(rqStatusJSON)+`}}`),
			metav1.PatchOptions{}, "status")
		framework.ExpectNoError(err)

		ginkgo.By(fmt.Sprintf("Confirm /status for %q resourceQuota via watch", rqName))
		ctxUntil, cancel = context.WithTimeout(ctx, f.Timeouts.PodStartShort)
		defer cancel()

		_, err = watchtools.Until(ctxUntil, rqList.ResourceVersion, w, func(event watch.Event) (bool, error) {
			if rq, ok := event.Object.(*v1.ResourceQuota); ok {
				found := rq.Name == patchedResourceQuota.Name &&
					rq.Namespace == ns &&
					apiequality.Semantic.DeepEqual(rq.Status.Hard, patchedResourceQuota.Spec.Hard)
				if !found {
					framework.Logf("observed resourceQuota %q in namespace %q with hard status: %#v", rq.Name, rq.Namespace, rq.Status.Hard)
					return false, nil
				}
				framework.Logf("Found resourceQuota %q in namespace %q with hard status: %#v", rq.Name, rq.Namespace, rq.Status.Hard)
				return found, nil
			}
			framework.Logf("Observed event: %+v", event.Object)
			return false, nil
		})
		framework.ExpectNoError(err, "failed to locate ResourceQuota %q in namespace %q", patchedResourceQuota.Name, ns)
		framework.Logf("ResourceQuota %q /status was patched", patchedResourceQuota.Name)

		ginkgo.By(fmt.Sprintf("Get %q /status", rqName))
		rqResource := schema.GroupVersionResource{Group: "", Version: "v1", Resource: "resourcequotas"}
		unstruct, err := f.DynamicClient.Resource(rqResource).Namespace(ns).Get(ctx, resourceQuota.Name, metav1.GetOptions{}, "status")
		framework.ExpectNoError(err)

		rq, err := unstructuredToResourceQuota(unstruct)
		framework.ExpectNoError(err, "Getting the status of the resource quota %q", rq.Name)

		gomega.Expect(*rq.Status.Hard.Cpu()).To(gomega.Equal(resource.MustParse("1")), "Hard cpu value for ResourceQuota %q is %s not 1.", rq.Name, rq.Status.Hard.Cpu().String())
		framework.Logf("Resourcequota %q reports status: hard cpu of %s", rqName, rq.Status.Hard.Cpu())
		gomega.Expect(*rq.Status.Hard.Memory()).To(gomega.Equal(resource.MustParse("1Gi")), "Hard memory value for ResourceQuota %q is %s not 1Gi.", rq.Name, rq.Status.Hard.Memory().String())
		framework.Logf("Resourcequota %q reports status: hard memory of %s", rqName, rq.Status.Hard.Memory())

		// Sync resourceQuota list before repatching /status
		rqList, err = f.ClientSet.CoreV1().ResourceQuotas("").List(ctx, metav1.ListOptions{LabelSelector: labelSelector})
		framework.ExpectNoError(err, "failed to list Services")

		ginkgo.By(fmt.Sprintf("Repatching %q /status before checking Spec is unchanged", rqName))
		newHardLimits := v1.ResourceList{
			v1.ResourceCPU:    resource.MustParse("2"),
			v1.ResourceMemory: resource.MustParse("2Gi"),
		}
		rqStatusJSON, err = json.Marshal(newHardLimits)
		framework.ExpectNoError(err)

		repatchedResourceQuota, err := rqClient.Patch(ctx, rqName, types.StrategicMergePatchType,
			[]byte(`{"status": {"hard": `+string(rqStatusJSON)+`}}`),
			metav1.PatchOptions{}, "status")
		framework.ExpectNoError(err)

		gomega.Expect(*repatchedResourceQuota.Status.Hard.Cpu()).To(gomega.Equal(resource.MustParse("2")), "Hard cpu value for ResourceQuota %q is %s not 2.", repatchedResourceQuota.Name, repatchedResourceQuota.Status.Hard.Cpu().String())
		framework.Logf("Resourcequota %q reports status: hard cpu of %s", repatchedResourceQuota.Name, repatchedResourceQuota.Status.Hard.Cpu())
		gomega.Expect(*repatchedResourceQuota.Status.Hard.Memory()).To(gomega.Equal(resource.MustParse("2Gi")), "Hard memory value for ResourceQuota %q is %s not 2Gi.", repatchedResourceQuota.Name, repatchedResourceQuota.Status.Hard.Memory().String())
		framework.Logf("Resourcequota %q reports status: hard memory of %s", repatchedResourceQuota.Name, repatchedResourceQuota.Status.Hard.Memory())

		_, err = watchtools.Until(ctxUntil, rqList.ResourceVersion, w, func(event watch.Event) (bool, error) {
			if rq, ok := event.Object.(*v1.ResourceQuota); ok {
				found := rq.Name == patchedResourceQuota.Name &&
					rq.Namespace == ns &&
					*rq.Status.Hard.Cpu() == resource.MustParse("2") &&
					*rq.Status.Hard.Memory() == resource.MustParse("2Gi")
				if !found {
					framework.Logf("observed resourceQuota %q in namespace %q with hard status: %#v", rq.Name, rq.Namespace, rq.Status.Hard)
					return false, nil
				}
				framework.Logf("Found resourceQuota %q in namespace %q with hard status: %#v", rq.Name, rq.Namespace, rq.Status.Hard)
				return found, nil
			}
			framework.Logf("Observed event: %+v", event.Object)
			return false, nil
		})
		framework.ExpectNoError(err, "failed to locate ResourceQuota %q in namespace %q", patchedResourceQuota.Name, ns)

		// the resource_quota_controller ignores changes to the status so we have to wait for a full resync of the controller
		// to reconcile the status again, this full resync is set every 5 minutes by default so we need to poll at least one
		// minute more just in case we we start to poll just after the full resync has happened and he have to wait until
		// next full resync.
		// Ref: https://issues.k8s.io/121911
		err = wait.PollUntilContextTimeout(ctx, 5*time.Second, 6*time.Minute, true, func(ctx context.Context) (bool, error) {
			resourceQuotaResult, err := rqClient.Get(ctx, rqName, metav1.GetOptions{})
			if err != nil {
				return false, nil
			}

			if *resourceQuotaResult.Spec.Hard.Cpu() == *resourceQuotaResult.Status.Hard.Cpu() {
				if *resourceQuotaResult.Status.Hard.Cpu() != resource.MustParse("1") {
					framework.Logf("Hard cpu status value for ResourceQuota %q is %s not 1.", repatchedResourceQuota.Name, resourceQuotaResult.Status.Hard.Cpu().String())
					return false, nil
				}
				if *resourceQuotaResult.Status.Hard.Memory() != resource.MustParse("1Gi") {
					framework.Logf("Hard memory status value for ResourceQuota %q is %s not 1Gi.", repatchedResourceQuota.Name, resourceQuotaResult.Status.Hard.Memory().String())
					return false, nil
				}
				framework.Logf("ResourceQuota %q Spec was unchanged and /status reset", resourceQuotaResult.Name)
				return true, nil
			}
			framework.Logf("ResourceQuota %q Spec and Status does not match: %#v", resourceQuotaResult.Name, resourceQuotaResult)
			return false, nil
		})
		if err != nil {
			framework.Failf("Error waiting for ResourceQuota %q to reset its Status: %v", patchedResourceQuota.Name, err)
		}

	})
})

var _ = SIGDescribe("ResourceQuota", feature.VolumeAttributesClass, framework.WithFeatureGate(features.VolumeAttributesClass), func() {
	f := framework.NewDefaultFramework("resourcequota-volumeattributesclass")
	f.NamespacePodSecurityLevel = admissionapi.LevelBaseline

	ginkgo.It("should verify ResourceQuota's volume attributes class scope (quota set to pvc count: 1) against a pvc with same volume attributes class.", func(ctx context.Context) {
		hard := v1.ResourceList{}
		hard[v1.ResourcePersistentVolumeClaims] = resource.MustParse("1")

		ginkgo.By("Creating a ResourceQuota with volume attributes class scope")
		quota, err := createResourceQuota(ctx, f.ClientSet, f.Namespace.Name, newTestResourceQuotaWithScopeForVolumeAttributesClass("quota-volumeattributesclass", hard, v1.ScopeSelectorOpIn, []string{classGold}))
		framework.ExpectNoError(err)

		ginkgo.By("Ensuring ResourceQuota status is calculated")
		usedResources := v1.ResourceList{}
		usedResources[v1.ResourcePersistentVolumeClaims] = resource.MustParse("0")
		err = waitForResourceQuota(ctx, f.ClientSet, f.Namespace.Name, quota.Name, usedResources)
		framework.ExpectNoError(err)

		ginkgo.By("Creating a pvc with volume attributes class")
		pvc1 := newTestPersistentVolumeClaimForQuota("test-claim-1")
		pvc1.Spec.StorageClassName = ptr.To("")
		pvc1.Spec.VolumeAttributesClassName = &classGold
		pvc1, err = f.ClientSet.CoreV1().PersistentVolumeClaims(f.Namespace.Name).Create(ctx, pvc1, metav1.CreateOptions{})
		framework.ExpectNoError(err)

		ginkgo.By("Ensuring resource quota with volume attributes class scope captures the pvc usage")
		usedResources[v1.ResourcePersistentVolumeClaims] = resource.MustParse("1")
		err = waitForResourceQuota(ctx, f.ClientSet, f.Namespace.Name, quota.Name, usedResources)
		framework.ExpectNoError(err)

		ginkgo.By("Updating the pvc modify volume status with volume attributes class")
		pvc1.Status.ModifyVolumeStatus = &v1.ModifyVolumeStatus{
			TargetVolumeAttributesClassName: classGold,
			Status:                          v1.PersistentVolumeClaimModifyVolumePending,
		}
		pvc1, err = f.ClientSet.CoreV1().PersistentVolumeClaims(f.Namespace.Name).UpdateStatus(ctx, pvc1, metav1.UpdateOptions{})
		framework.ExpectNoError(err)

		ginkgo.By("Ensuring resource quota not changed after updating the pvc status")
		gomega.Consistently(ctx,
			framework.GetObject(f.ClientSet.CoreV1().ResourceQuotas(f.Namespace.Name).Get, quota.Name, metav1.GetOptions{})).
			WithPolling(5 * time.Second).
			WithTimeout(30 * time.Second).
			Should(gomega.HaveField("Status.Used", gomega.Equal(usedResources)))

		ginkgo.By("Updating the pvc status with volume attributes class")
		pvc1.Status.ModifyVolumeStatus = nil
		pvc1.Status.CurrentVolumeAttributesClassName = &classGold
		pvc1, err = f.ClientSet.CoreV1().PersistentVolumeClaims(f.Namespace.Name).UpdateStatus(ctx, pvc1, metav1.UpdateOptions{})
		framework.ExpectNoError(err)

		ginkgo.By("Ensuring resource quota not changed after updating the pvc status")
		gomega.Consistently(ctx,
			framework.GetObject(f.ClientSet.CoreV1().ResourceQuotas(f.Namespace.Name).Get, quota.Name, metav1.GetOptions{})).
			WithPolling(5 * time.Second).
			WithTimeout(30 * time.Second).
			Should(gomega.HaveField("Status.Used", gomega.Equal(usedResources)))

		ginkgo.By("Deleting the pvc")
		err = f.ClientSet.CoreV1().PersistentVolumeClaims(f.Namespace.Name).Delete(ctx, pvc1.Name, metav1.DeleteOptions{})
		framework.ExpectNoError(err)

		ginkgo.By("Ensuring resource quota status released the pvc usage")
		usedResources[v1.ResourcePersistentVolumeClaims] = resource.MustParse("0")
		err = waitForResourceQuota(ctx, f.ClientSet, f.Namespace.Name, quota.Name, usedResources)
		framework.ExpectNoError(err)
	})

	ginkgo.It("should verify ResourceQuota's volume attributes class scope (quota set to pvc count: 1) against a pvc with different volume attributes class.", func(ctx context.Context) {
		hard := v1.ResourceList{}
		hard[v1.ResourcePersistentVolumeClaims] = resource.MustParse("1")

		ginkgo.By("Creating 2 ResourceQuotas with volume attributes class scope")
		quotaGold, err := createResourceQuota(ctx, f.ClientSet, f.Namespace.Name, newTestResourceQuotaWithScopeForVolumeAttributesClass("quota-volumeattributesclass-gold", hard, v1.ScopeSelectorOpIn, []string{classGold}))
		framework.ExpectNoError(err)
		quotaSilver, err := createResourceQuota(ctx, f.ClientSet, f.Namespace.Name, newTestResourceQuotaWithScopeForVolumeAttributesClass("quota-volumeattributesclass-silver", hard, v1.ScopeSelectorOpIn, []string{classSilver}))
		framework.ExpectNoError(err)

		ginkgo.By("Ensuring all ResourceQuotas status is calculated")
		usedResources := v1.ResourceList{}
		usedResources[v1.ResourcePersistentVolumeClaims] = resource.MustParse("0")
		err = waitForResourceQuota(ctx, f.ClientSet, f.Namespace.Name, quotaGold.Name, usedResources)
		framework.ExpectNoError(err)
		err = waitForResourceQuota(ctx, f.ClientSet, f.Namespace.Name, quotaSilver.Name, usedResources)
		framework.ExpectNoError(err)

		ginkgo.By("Creating a pvc with gold volume attributes class")
		pvc1 := newTestPersistentVolumeClaimForQuota("test-claim-1")
		pvc1.Spec.StorageClassName = ptr.To("")
		pvc1.Spec.VolumeAttributesClassName = &classGold
		pvc1, err = f.ClientSet.CoreV1().PersistentVolumeClaims(f.Namespace.Name).Create(ctx, pvc1, metav1.CreateOptions{})
		framework.ExpectNoError(err)

		ginkgo.By("Ensuring first resource quota with volume attributes class scope captures the pvc usage")
		usedResources[v1.ResourcePersistentVolumeClaims] = resource.MustParse("1")
		err = waitForResourceQuota(ctx, f.ClientSet, f.Namespace.Name, quotaGold.Name, usedResources)
		framework.ExpectNoError(err)

		ginkgo.By("Updating the pvc status with volume attributes class")
		pvc1.Status.ModifyVolumeStatus = nil
		pvc1.Status.CurrentVolumeAttributesClassName = &classGold
		pvc1, err = f.ClientSet.CoreV1().PersistentVolumeClaims(f.Namespace.Name).UpdateStatus(ctx, pvc1, metav1.UpdateOptions{})
		framework.ExpectNoError(err)

		ginkgo.By("Updating the pvc with silver volume attributes class")
		pvc1.Spec.VolumeAttributesClassName = &classSilver
		pvc1, err = f.ClientSet.CoreV1().PersistentVolumeClaims(f.Namespace.Name).Update(ctx, pvc1, metav1.UpdateOptions{})
		framework.ExpectNoError(err)

		ginkgo.By("Ensuring resource quota not changed after updating the pvc status and spec")
		gomega.Consistently(ctx,
			framework.GetObject(f.ClientSet.CoreV1().ResourceQuotas(f.Namespace.Name).Get, quotaGold.Name, metav1.GetOptions{})).
			WithPolling(5 * time.Second).
			WithTimeout(30 * time.Second).
			Should(gomega.HaveField("Status.Used", gomega.Equal(usedResources)))

		ginkgo.By("Ensuring 2nd resource quota with volume attributes class scope captures the pvc usage")
		usedResources[v1.ResourcePersistentVolumeClaims] = resource.MustParse("1")
		err = waitForResourceQuota(ctx, f.ClientSet, f.Namespace.Name, quotaSilver.Name, usedResources)
		framework.ExpectNoError(err)

		ginkgo.By("Replacing the pvc status with silver volume attributes class")
		pvc1.Status.ModifyVolumeStatus = nil
		pvc1.Status.CurrentVolumeAttributesClassName = &classSilver
		_, err = f.ClientSet.CoreV1().PersistentVolumeClaims(f.Namespace.Name).Update(ctx, pvc1, metav1.UpdateOptions{})
		framework.ExpectNoError(err)

		ginkgo.By("Ensuring first resource quota status released the pvc usage")
		usedResources[v1.ResourcePersistentVolumeClaims] = resource.MustParse("0")
		err = waitForResourceQuota(ctx, f.ClientSet, f.Namespace.Name, quotaGold.Name, usedResources)
		framework.ExpectNoError(err)
	})

	ginkgo.It("should verify ResourceQuota's volume attributes class scope (quota set to pvc count: 1) against 2 pvcs with same volume attributes class.", func(ctx context.Context) {
		hard := v1.ResourceList{}
		hard[v1.ResourcePersistentVolumeClaims] = resource.MustParse("1")

		ginkgo.By("Creating a ResourceQuota with volume attributes class scope")
		quota, err := createResourceQuota(ctx, f.ClientSet, f.Namespace.Name, newTestResourceQuotaWithScopeForVolumeAttributesClass("quota-volumeattributesclass", hard, v1.ScopeSelectorOpIn, []string{classGold}))
		framework.ExpectNoError(err)

		ginkgo.By("Ensuring ResourceQuota status is calculated")
		usedResources := v1.ResourceList{}
		usedResources[v1.ResourcePersistentVolumeClaims] = resource.MustParse("0")
		err = waitForResourceQuota(ctx, f.ClientSet, f.Namespace.Name, quota.Name, usedResources)
		framework.ExpectNoError(err)

		ginkgo.By("Creating a pvc with volume attributes class")
		pvc1 := newTestPersistentVolumeClaimForQuota("test-claim-1")
		pvc1.Spec.StorageClassName = ptr.To("")
		pvc1.Spec.VolumeAttributesClassName = &classGold
		pvc1, err = f.ClientSet.CoreV1().PersistentVolumeClaims(f.Namespace.Name).Create(ctx, pvc1, metav1.CreateOptions{})
		framework.ExpectNoError(err)

		ginkgo.By("Ensuring resource quota with volume attributes class scope captures the pvc usage")
		usedResources[v1.ResourcePersistentVolumeClaims] = resource.MustParse("1")
		err = waitForResourceQuota(ctx, f.ClientSet, f.Namespace.Name, quota.Name, usedResources)
		framework.ExpectNoError(err)

		ginkgo.By("Creating 2nd pod with priority class should fail")
		pvc2 := newTestPersistentVolumeClaimForQuota("test-claim-2")
		pvc2.Spec.StorageClassName = ptr.To("")
		pvc2.Spec.VolumeAttributesClassName = &classGold
		_, err = f.ClientSet.CoreV1().PersistentVolumeClaims(f.Namespace.Name).Create(ctx, pvc2, metav1.CreateOptions{})
		gomega.Expect(err).To(gomega.MatchError(apierrors.IsForbidden, "expect a forbidden error when creating a PVC that exceeds quota"))

		ginkgo.By("Deleting first pvc")
		err = f.ClientSet.CoreV1().PersistentVolumeClaims(f.Namespace.Name).Delete(ctx, pvc1.Name, metav1.DeleteOptions{})
		framework.ExpectNoError(err)

		ginkgo.By("Ensuring resource quota status released the pvc usage")
		usedResources[v1.ResourcePersistentVolumeClaims] = resource.MustParse("0")
		err = waitForResourceQuota(ctx, f.ClientSet, f.Namespace.Name, quota.Name, usedResources)
		framework.ExpectNoError(err)
	})
})

var _ = SIGDescribe("ResourceQuota", func() {
	f := framework.NewDefaultFramework("scope-selectors")
	f.NamespacePodSecurityLevel = admissionapi.LevelBaseline
	ginkgo.It("should verify ResourceQuota with best effort scope using scope-selectors.", func(ctx context.Context) {
		ginkgo.By("Creating a ResourceQuota with best effort scope")
		resourceQuotaBestEffort, err := createResourceQuota(ctx, f.ClientSet, f.Namespace.Name, newTestResourceQuotaWithScopeSelector("quota-besteffort", v1.ResourceQuotaScopeBestEffort))
		framework.ExpectNoError(err)

		ginkgo.By("Ensuring ResourceQuota status is calculated")
		usedResources := v1.ResourceList{}
		usedResources[v1.ResourcePods] = resource.MustParse("0")
		err = waitForResourceQuota(ctx, f.ClientSet, f.Namespace.Name, resourceQuotaBestEffort.Name, usedResources)
		framework.ExpectNoError(err)

		ginkgo.By("Creating a ResourceQuota with not best effort scope")
		resourceQuotaNotBestEffort, err := createResourceQuota(ctx, f.ClientSet, f.Namespace.Name, newTestResourceQuotaWithScopeSelector("quota-not-besteffort", v1.ResourceQuotaScopeNotBestEffort))
		framework.ExpectNoError(err)

		ginkgo.By("Ensuring ResourceQuota status is calculated")
		err = waitForResourceQuota(ctx, f.ClientSet, f.Namespace.Name, resourceQuotaNotBestEffort.Name, usedResources)
		framework.ExpectNoError(err)

		ginkgo.By("Creating a best-effort pod")
		pod := newTestPodForQuota(f, podName, v1.ResourceList{}, v1.ResourceList{})
		pod, err = f.ClientSet.CoreV1().Pods(f.Namespace.Name).Create(ctx, pod, metav1.CreateOptions{})
		framework.ExpectNoError(err)

		ginkgo.By("Ensuring resource quota with best effort scope captures the pod usage")
		usedResources[v1.ResourcePods] = resource.MustParse("1")
		err = waitForResourceQuota(ctx, f.ClientSet, f.Namespace.Name, resourceQuotaBestEffort.Name, usedResources)
		framework.ExpectNoError(err)

		ginkgo.By("Ensuring resource quota with not best effort ignored the pod usage")
		usedResources[v1.ResourcePods] = resource.MustParse("0")
		err = waitForResourceQuota(ctx, f.ClientSet, f.Namespace.Name, resourceQuotaNotBestEffort.Name, usedResources)
		framework.ExpectNoError(err)

		ginkgo.By("Deleting the pod")
		err = f.ClientSet.CoreV1().Pods(f.Namespace.Name).Delete(ctx, pod.Name, *metav1.NewDeleteOptions(0))
		framework.ExpectNoError(err)

		ginkgo.By("Ensuring resource quota status released the pod usage")
		usedResources[v1.ResourcePods] = resource.MustParse("0")
		err = waitForResourceQuota(ctx, f.ClientSet, f.Namespace.Name, resourceQuotaBestEffort.Name, usedResources)
		framework.ExpectNoError(err)

		ginkgo.By("Creating a not best-effort pod")
		requests := v1.ResourceList{}
		requests[v1.ResourceCPU] = resource.MustParse("500m")
		requests[v1.ResourceMemory] = resource.MustParse("200Mi")
		limits := v1.ResourceList{}
		limits[v1.ResourceCPU] = resource.MustParse("1")
		limits[v1.ResourceMemory] = resource.MustParse("400Mi")
		pod = newTestPodForQuota(f, "burstable-pod", requests, limits)
		pod, err = f.ClientSet.CoreV1().Pods(f.Namespace.Name).Create(ctx, pod, metav1.CreateOptions{})
		framework.ExpectNoError(err)

		ginkgo.By("Ensuring resource quota with not best effort scope captures the pod usage")
		usedResources[v1.ResourcePods] = resource.MustParse("1")
		err = waitForResourceQuota(ctx, f.ClientSet, f.Namespace.Name, resourceQuotaNotBestEffort.Name, usedResources)
		framework.ExpectNoError(err)

		ginkgo.By("Ensuring resource quota with best effort scope ignored the pod usage")
		usedResources[v1.ResourcePods] = resource.MustParse("0")
		err = waitForResourceQuota(ctx, f.ClientSet, f.Namespace.Name, resourceQuotaBestEffort.Name, usedResources)
		framework.ExpectNoError(err)

		ginkgo.By("Deleting the pod")
		err = f.ClientSet.CoreV1().Pods(f.Namespace.Name).Delete(ctx, pod.Name, *metav1.NewDeleteOptions(0))
		framework.ExpectNoError(err)

		ginkgo.By("Ensuring resource quota status released the pod usage")
		usedResources[v1.ResourcePods] = resource.MustParse("0")
		err = waitForResourceQuota(ctx, f.ClientSet, f.Namespace.Name, resourceQuotaNotBestEffort.Name, usedResources)
		framework.ExpectNoError(err)
	})
	ginkgo.It("should verify ResourceQuota with terminating scopes through scope selectors.", func(ctx context.Context) {
		ginkgo.By("Creating a ResourceQuota with terminating scope")
		quotaTerminatingName := "quota-terminating"
		resourceQuotaTerminating, err := createResourceQuota(ctx, f.ClientSet, f.Namespace.Name, newTestResourceQuotaWithScopeSelector(quotaTerminatingName, v1.ResourceQuotaScopeTerminating))
		framework.ExpectNoError(err)

		ginkgo.By("Ensuring ResourceQuota status is calculated")
		usedResources := v1.ResourceList{}
		usedResources[v1.ResourcePods] = resource.MustParse("0")
		err = waitForResourceQuota(ctx, f.ClientSet, f.Namespace.Name, resourceQuotaTerminating.Name, usedResources)
		framework.ExpectNoError(err)

		ginkgo.By("Creating a ResourceQuota with not terminating scope")
		quotaNotTerminatingName := "quota-not-terminating"
		resourceQuotaNotTerminating, err := createResourceQuota(ctx, f.ClientSet, f.Namespace.Name, newTestResourceQuotaWithScopeSelector(quotaNotTerminatingName, v1.ResourceQuotaScopeNotTerminating))
		framework.ExpectNoError(err)

		ginkgo.By("Ensuring ResourceQuota status is calculated")
		err = waitForResourceQuota(ctx, f.ClientSet, f.Namespace.Name, resourceQuotaNotTerminating.Name, usedResources)
		framework.ExpectNoError(err)

		ginkgo.By("Creating a long running pod")
		podName := "test-pod"
		requests := v1.ResourceList{}
		requests[v1.ResourceCPU] = resource.MustParse("500m")
		requests[v1.ResourceMemory] = resource.MustParse("200Mi")
		limits := v1.ResourceList{}
		limits[v1.ResourceCPU] = resource.MustParse("1")
		limits[v1.ResourceMemory] = resource.MustParse("400Mi")
		pod := newTestPodForQuota(f, podName, requests, limits)
		_, err = f.ClientSet.CoreV1().Pods(f.Namespace.Name).Create(ctx, pod, metav1.CreateOptions{})
		framework.ExpectNoError(err)

		ginkgo.By("Ensuring resource quota with not terminating scope captures the pod usage")
		usedResources[v1.ResourcePods] = resource.MustParse("1")
		usedResources[v1.ResourceRequestsCPU] = requests[v1.ResourceCPU]
		usedResources[v1.ResourceRequestsMemory] = requests[v1.ResourceMemory]
		usedResources[v1.ResourceLimitsCPU] = limits[v1.ResourceCPU]
		usedResources[v1.ResourceLimitsMemory] = limits[v1.ResourceMemory]
		err = waitForResourceQuota(ctx, f.ClientSet, f.Namespace.Name, resourceQuotaNotTerminating.Name, usedResources)
		framework.ExpectNoError(err)

		ginkgo.By("Ensuring resource quota with terminating scope ignored the pod usage")
		usedResources[v1.ResourcePods] = resource.MustParse("0")
		usedResources[v1.ResourceRequestsCPU] = resource.MustParse("0")
		usedResources[v1.ResourceRequestsMemory] = resource.MustParse("0")
		usedResources[v1.ResourceLimitsCPU] = resource.MustParse("0")
		usedResources[v1.ResourceLimitsMemory] = resource.MustParse("0")
		err = waitForResourceQuota(ctx, f.ClientSet, f.Namespace.Name, resourceQuotaTerminating.Name, usedResources)
		framework.ExpectNoError(err)

		ginkgo.By("Deleting the pod")
		err = f.ClientSet.CoreV1().Pods(f.Namespace.Name).Delete(ctx, podName, *metav1.NewDeleteOptions(0))
		framework.ExpectNoError(err)

		ginkgo.By("Ensuring resource quota status released the pod usage")
		usedResources[v1.ResourcePods] = resource.MustParse("0")
		usedResources[v1.ResourceRequestsCPU] = resource.MustParse("0")
		usedResources[v1.ResourceRequestsMemory] = resource.MustParse("0")
		usedResources[v1.ResourceLimitsCPU] = resource.MustParse("0")
		usedResources[v1.ResourceLimitsMemory] = resource.MustParse("0")
		err = waitForResourceQuota(ctx, f.ClientSet, f.Namespace.Name, resourceQuotaNotTerminating.Name, usedResources)
		framework.ExpectNoError(err)

		ginkgo.By("Creating a terminating pod")
		podName = "terminating-pod"
		pod = newTestPodForQuota(f, podName, requests, limits)
		activeDeadlineSeconds := int64(3600)
		pod.Spec.ActiveDeadlineSeconds = &activeDeadlineSeconds
		_, err = f.ClientSet.CoreV1().Pods(f.Namespace.Name).Create(ctx, pod, metav1.CreateOptions{})
		framework.ExpectNoError(err)

		ginkgo.By("Ensuring resource quota with terminating scope captures the pod usage")
		usedResources[v1.ResourcePods] = resource.MustParse("1")
		usedResources[v1.ResourceRequestsCPU] = requests[v1.ResourceCPU]
		usedResources[v1.ResourceRequestsMemory] = requests[v1.ResourceMemory]
		usedResources[v1.ResourceLimitsCPU] = limits[v1.ResourceCPU]
		usedResources[v1.ResourceLimitsMemory] = limits[v1.ResourceMemory]
		err = waitForResourceQuota(ctx, f.ClientSet, f.Namespace.Name, resourceQuotaTerminating.Name, usedResources)
		framework.ExpectNoError(err)

		ginkgo.By("Ensuring resource quota with not terminating scope ignored the pod usage")
		usedResources[v1.ResourcePods] = resource.MustParse("0")
		usedResources[v1.ResourceRequestsCPU] = resource.MustParse("0")
		usedResources[v1.ResourceRequestsMemory] = resource.MustParse("0")
		usedResources[v1.ResourceLimitsCPU] = resource.MustParse("0")
		usedResources[v1.ResourceLimitsMemory] = resource.MustParse("0")
		err = waitForResourceQuota(ctx, f.ClientSet, f.Namespace.Name, resourceQuotaNotTerminating.Name, usedResources)
		framework.ExpectNoError(err)

		ginkgo.By("Deleting the pod")
		err = f.ClientSet.CoreV1().Pods(f.Namespace.Name).Delete(ctx, podName, *metav1.NewDeleteOptions(0))
		framework.ExpectNoError(err)

		ginkgo.By("Ensuring resource quota status released the pod usage")
		usedResources[v1.ResourcePods] = resource.MustParse("0")
		usedResources[v1.ResourceRequestsCPU] = resource.MustParse("0")
		usedResources[v1.ResourceRequestsMemory] = resource.MustParse("0")
		usedResources[v1.ResourceLimitsCPU] = resource.MustParse("0")
		usedResources[v1.ResourceLimitsMemory] = resource.MustParse("0")
		err = waitForResourceQuota(ctx, f.ClientSet, f.Namespace.Name, resourceQuotaTerminating.Name, usedResources)
		framework.ExpectNoError(err)
	})
})

var _ = SIGDescribe("ResourceQuota", feature.PodPriority, func() {
	f := framework.NewDefaultFramework("resourcequota-priorityclass")
	f.NamespacePodSecurityLevel = admissionapi.LevelBaseline

	ginkgo.It("should verify ResourceQuota's priority class scope (quota set to pod count: 1) against a pod with same priority class.", func(ctx context.Context) {

		_, err := f.ClientSet.SchedulingV1().PriorityClasses().Create(ctx, &schedulingv1.PriorityClass{ObjectMeta: metav1.ObjectMeta{Name: "pclass1"}, Value: int32(1000)}, metav1.CreateOptions{})
		if err != nil && !apierrors.IsAlreadyExists(err) {
			framework.Failf("unexpected error while creating priority class: %v", err)
		}

		hard := v1.ResourceList{}
		hard[v1.ResourcePods] = resource.MustParse("1")

		ginkgo.By("Creating a ResourceQuota with priority class scope")
		resourceQuotaPriorityClass, err := createResourceQuota(ctx, f.ClientSet, f.Namespace.Name, newTestResourceQuotaWithScopeForPriorityClass("quota-priorityclass", hard, v1.ScopeSelectorOpIn, []string{"pclass1"}))
		framework.ExpectNoError(err)

		ginkgo.By("Ensuring ResourceQuota status is calculated")
		usedResources := v1.ResourceList{}
		usedResources[v1.ResourcePods] = resource.MustParse("0")
		err = waitForResourceQuota(ctx, f.ClientSet, f.Namespace.Name, resourceQuotaPriorityClass.Name, usedResources)
		framework.ExpectNoError(err)

		ginkgo.By("Creating a pod with priority class")
		podName := "testpod-pclass1"
		pod := newTestPodForQuotaWithPriority(f, podName, v1.ResourceList{}, v1.ResourceList{}, "pclass1")
		pod, err = f.ClientSet.CoreV1().Pods(f.Namespace.Name).Create(ctx, pod, metav1.CreateOptions{})
		framework.ExpectNoError(err)

		ginkgo.By("Ensuring resource quota with priority class scope captures the pod usage")
		usedResources[v1.ResourcePods] = resource.MustParse("1")
		err = waitForResourceQuota(ctx, f.ClientSet, f.Namespace.Name, resourceQuotaPriorityClass.Name, usedResources)
		framework.ExpectNoError(err)

		ginkgo.By("Deleting the pod")
		err = f.ClientSet.CoreV1().Pods(f.Namespace.Name).Delete(ctx, pod.Name, *metav1.NewDeleteOptions(0))
		framework.ExpectNoError(err)

		ginkgo.By("Ensuring resource quota status released the pod usage")
		usedResources[v1.ResourcePods] = resource.MustParse("0")
		err = waitForResourceQuota(ctx, f.ClientSet, f.Namespace.Name, resourceQuotaPriorityClass.Name, usedResources)
		framework.ExpectNoError(err)
	})

	ginkgo.It("should verify ResourceQuota's priority class scope (quota set to pod count: 1) against 2 pods with same priority class.", func(ctx context.Context) {

		_, err := f.ClientSet.SchedulingV1().PriorityClasses().Create(ctx, &schedulingv1.PriorityClass{ObjectMeta: metav1.ObjectMeta{Name: "pclass2"}, Value: int32(1000)}, metav1.CreateOptions{})
		if err != nil && !apierrors.IsAlreadyExists(err) {
			framework.Failf("unexpected error while creating priority class: %v", err)
		}

		hard := v1.ResourceList{}
		hard[v1.ResourcePods] = resource.MustParse("1")

		ginkgo.By("Creating a ResourceQuota with priority class scope")
		resourceQuotaPriorityClass, err := createResourceQuota(ctx, f.ClientSet, f.Namespace.Name, newTestResourceQuotaWithScopeForPriorityClass("quota-priorityclass", hard, v1.ScopeSelectorOpIn, []string{"pclass2"}))
		framework.ExpectNoError(err)

		ginkgo.By("Ensuring ResourceQuota status is calculated")
		usedResources := v1.ResourceList{}
		usedResources[v1.ResourcePods] = resource.MustParse("0")
		err = waitForResourceQuota(ctx, f.ClientSet, f.Namespace.Name, resourceQuotaPriorityClass.Name, usedResources)
		framework.ExpectNoError(err)

		ginkgo.By("Creating first pod with priority class should pass")
		podName := "testpod-pclass2-1"
		pod := newTestPodForQuotaWithPriority(f, podName, v1.ResourceList{}, v1.ResourceList{}, "pclass2")
		pod, err = f.ClientSet.CoreV1().Pods(f.Namespace.Name).Create(ctx, pod, metav1.CreateOptions{})
		framework.ExpectNoError(err)

		ginkgo.By("Ensuring resource quota with priority class scope captures the pod usage")
		usedResources[v1.ResourcePods] = resource.MustParse("1")
		err = waitForResourceQuota(ctx, f.ClientSet, f.Namespace.Name, resourceQuotaPriorityClass.Name, usedResources)
		framework.ExpectNoError(err)

		ginkgo.By("Creating 2nd pod with priority class should fail")
		podName2 := "testpod-pclass2-2"
		pod2 := newTestPodForQuotaWithPriority(f, podName2, v1.ResourceList{}, v1.ResourceList{}, "pclass2")
		_, err = f.ClientSet.CoreV1().Pods(f.Namespace.Name).Create(ctx, pod2, metav1.CreateOptions{})
		gomega.Expect(err).To(gomega.HaveOccurred())

		ginkgo.By("Deleting first pod")
		err = f.ClientSet.CoreV1().Pods(f.Namespace.Name).Delete(ctx, pod.Name, *metav1.NewDeleteOptions(0))
		framework.ExpectNoError(err)

		ginkgo.By("Ensuring resource quota status released the pod usage")
		usedResources[v1.ResourcePods] = resource.MustParse("0")
		err = waitForResourceQuota(ctx, f.ClientSet, f.Namespace.Name, resourceQuotaPriorityClass.Name, usedResources)
		framework.ExpectNoError(err)
	})

	ginkgo.It("should verify ResourceQuota's priority class scope (quota set to pod count: 1) against 2 pods with different priority class.", func(ctx context.Context) {

		_, err := f.ClientSet.SchedulingV1().PriorityClasses().Create(ctx, &schedulingv1.PriorityClass{ObjectMeta: metav1.ObjectMeta{Name: "pclass3"}, Value: int32(1000)}, metav1.CreateOptions{})
		if err != nil && !apierrors.IsAlreadyExists(err) {
			framework.Failf("unexpected error while creating priority class: %v", err)
		}

		hard := v1.ResourceList{}
		hard[v1.ResourcePods] = resource.MustParse("1")

		ginkgo.By("Creating a ResourceQuota with priority class scope")
		resourceQuotaPriorityClass, err := createResourceQuota(ctx, f.ClientSet, f.Namespace.Name, newTestResourceQuotaWithScopeForPriorityClass("quota-priorityclass", hard, v1.ScopeSelectorOpIn, []string{"pclass4"}))
		framework.ExpectNoError(err)

		ginkgo.By("Ensuring ResourceQuota status is calculated")
		usedResources := v1.ResourceList{}
		usedResources[v1.ResourcePods] = resource.MustParse("0")
		err = waitForResourceQuota(ctx, f.ClientSet, f.Namespace.Name, resourceQuotaPriorityClass.Name, usedResources)
		framework.ExpectNoError(err)

		ginkgo.By("Creating a pod with priority class with pclass3")
		podName := "testpod-pclass3-1"
		pod := newTestPodForQuotaWithPriority(f, podName, v1.ResourceList{}, v1.ResourceList{}, "pclass3")
		pod, err = f.ClientSet.CoreV1().Pods(f.Namespace.Name).Create(ctx, pod, metav1.CreateOptions{})
		framework.ExpectNoError(err)

		ginkgo.By("Ensuring resource quota with priority class scope remains same")
		usedResources[v1.ResourcePods] = resource.MustParse("0")
		err = waitForResourceQuota(ctx, f.ClientSet, f.Namespace.Name, resourceQuotaPriorityClass.Name, usedResources)
		framework.ExpectNoError(err)

		ginkgo.By("Creating a 2nd pod with priority class pclass3")
		podName2 := "testpod-pclass2-2"
		pod2 := newTestPodForQuotaWithPriority(f, podName2, v1.ResourceList{}, v1.ResourceList{}, "pclass3")
		pod2, err = f.ClientSet.CoreV1().Pods(f.Namespace.Name).Create(ctx, pod2, metav1.CreateOptions{})
		framework.ExpectNoError(err)

		ginkgo.By("Ensuring resource quota with priority class scope remains same")
		usedResources[v1.ResourcePods] = resource.MustParse("0")
		err = waitForResourceQuota(ctx, f.ClientSet, f.Namespace.Name, resourceQuotaPriorityClass.Name, usedResources)
		framework.ExpectNoError(err)

		ginkgo.By("Deleting both pods")
		err = f.ClientSet.CoreV1().Pods(f.Namespace.Name).Delete(ctx, pod.Name, *metav1.NewDeleteOptions(0))
		framework.ExpectNoError(err)
		err = f.ClientSet.CoreV1().Pods(f.Namespace.Name).Delete(ctx, pod2.Name, *metav1.NewDeleteOptions(0))
		framework.ExpectNoError(err)
	})

	ginkgo.It("should verify ResourceQuota's multiple priority class scope (quota set to pod count: 2) against 2 pods with same priority classes.", func(ctx context.Context) {
		_, err := f.ClientSet.SchedulingV1().PriorityClasses().Create(ctx, &schedulingv1.PriorityClass{ObjectMeta: metav1.ObjectMeta{Name: "pclass5"}, Value: int32(1000)}, metav1.CreateOptions{})
		if err != nil && !apierrors.IsAlreadyExists(err) {
			framework.Failf("unexpected error while creating priority class: %v", err)
		}

		_, err = f.ClientSet.SchedulingV1().PriorityClasses().Create(ctx, &schedulingv1.PriorityClass{ObjectMeta: metav1.ObjectMeta{Name: "pclass6"}, Value: int32(1000)}, metav1.CreateOptions{})
		if err != nil && !apierrors.IsAlreadyExists(err) {
			framework.Failf("unexpected error while creating priority class: %v", err)
		}

		hard := v1.ResourceList{}
		hard[v1.ResourcePods] = resource.MustParse("2")

		ginkgo.By("Creating a ResourceQuota with priority class scope")
		resourceQuotaPriorityClass, err := createResourceQuota(ctx, f.ClientSet, f.Namespace.Name, newTestResourceQuotaWithScopeForPriorityClass("quota-priorityclass", hard, v1.ScopeSelectorOpIn, []string{"pclass5", "pclass6"}))
		framework.ExpectNoError(err)

		ginkgo.By("Ensuring ResourceQuota status is calculated")
		usedResources := v1.ResourceList{}
		usedResources[v1.ResourcePods] = resource.MustParse("0")
		err = waitForResourceQuota(ctx, f.ClientSet, f.Namespace.Name, resourceQuotaPriorityClass.Name, usedResources)
		framework.ExpectNoError(err)

		ginkgo.By("Creating a pod with priority class pclass5")
		podName := "testpod-pclass5"
		pod := newTestPodForQuotaWithPriority(f, podName, v1.ResourceList{}, v1.ResourceList{}, "pclass5")
		pod, err = f.ClientSet.CoreV1().Pods(f.Namespace.Name).Create(ctx, pod, metav1.CreateOptions{})
		framework.ExpectNoError(err)

		ginkgo.By("Ensuring resource quota with priority class is updated with the pod usage")
		usedResources[v1.ResourcePods] = resource.MustParse("1")
		err = waitForResourceQuota(ctx, f.ClientSet, f.Namespace.Name, resourceQuotaPriorityClass.Name, usedResources)
		framework.ExpectNoError(err)

		ginkgo.By("Creating 2nd pod with priority class pclass6")
		podName2 := "testpod-pclass6"
		pod2 := newTestPodForQuotaWithPriority(f, podName2, v1.ResourceList{}, v1.ResourceList{}, "pclass6")
		pod2, err = f.ClientSet.CoreV1().Pods(f.Namespace.Name).Create(ctx, pod2, metav1.CreateOptions{})
		framework.ExpectNoError(err)

		ginkgo.By("Ensuring resource quota with priority class scope is updated with the pod usage")
		usedResources[v1.ResourcePods] = resource.MustParse("2")
		err = waitForResourceQuota(ctx, f.ClientSet, f.Namespace.Name, resourceQuotaPriorityClass.Name, usedResources)
		framework.ExpectNoError(err)

		ginkgo.By("Deleting both pods")
		err = f.ClientSet.CoreV1().Pods(f.Namespace.Name).Delete(ctx, pod.Name, *metav1.NewDeleteOptions(0))
		framework.ExpectNoError(err)
		err = f.ClientSet.CoreV1().Pods(f.Namespace.Name).Delete(ctx, pod2.Name, *metav1.NewDeleteOptions(0))
		framework.ExpectNoError(err)

		ginkgo.By("Ensuring resource quota status released the pod usage")
		usedResources[v1.ResourcePods] = resource.MustParse("0")
		err = waitForResourceQuota(ctx, f.ClientSet, f.Namespace.Name, resourceQuotaPriorityClass.Name, usedResources)
		framework.ExpectNoError(err)
	})

	ginkgo.It("should verify ResourceQuota's priority class scope (quota set to pod count: 1) against a pod with different priority class (ScopeSelectorOpNotIn).", func(ctx context.Context) {

		_, err := f.ClientSet.SchedulingV1().PriorityClasses().Create(ctx, &schedulingv1.PriorityClass{ObjectMeta: metav1.ObjectMeta{Name: "pclass7"}, Value: int32(1000)}, metav1.CreateOptions{})
		if err != nil && !apierrors.IsAlreadyExists(err) {
			framework.Failf("unexpected error while creating priority class: %v", err)
		}

		hard := v1.ResourceList{}
		hard[v1.ResourcePods] = resource.MustParse("1")

		ginkgo.By("Creating a ResourceQuota with priority class scope")
		resourceQuotaPriorityClass, err := createResourceQuota(ctx, f.ClientSet, f.Namespace.Name, newTestResourceQuotaWithScopeForPriorityClass("quota-priorityclass", hard, v1.ScopeSelectorOpNotIn, []string{"pclass7"}))
		framework.ExpectNoError(err)

		ginkgo.By("Ensuring ResourceQuota status is calculated")
		usedResources := v1.ResourceList{}
		usedResources[v1.ResourcePods] = resource.MustParse("0")
		err = waitForResourceQuota(ctx, f.ClientSet, f.Namespace.Name, resourceQuotaPriorityClass.Name, usedResources)
		framework.ExpectNoError(err)

		ginkgo.By("Creating a pod with priority class pclass7")
		podName := "testpod-pclass7"
		pod := newTestPodForQuotaWithPriority(f, podName, v1.ResourceList{}, v1.ResourceList{}, "pclass7")
		pod, err = f.ClientSet.CoreV1().Pods(f.Namespace.Name).Create(ctx, pod, metav1.CreateOptions{})
		framework.ExpectNoError(err)

		ginkgo.By("Ensuring resource quota with priority class is not used")
		usedResources[v1.ResourcePods] = resource.MustParse("0")
		err = waitForResourceQuota(ctx, f.ClientSet, f.Namespace.Name, resourceQuotaPriorityClass.Name, usedResources)
		framework.ExpectNoError(err)

		ginkgo.By("Deleting the pod")
		err = f.ClientSet.CoreV1().Pods(f.Namespace.Name).Delete(ctx, pod.Name, *metav1.NewDeleteOptions(0))
		framework.ExpectNoError(err)
	})

	ginkgo.It("should verify ResourceQuota's priority class scope (quota set to pod count: 1) against a pod with different priority class (ScopeSelectorOpExists).", func(ctx context.Context) {

		_, err := f.ClientSet.SchedulingV1().PriorityClasses().Create(ctx, &schedulingv1.PriorityClass{ObjectMeta: metav1.ObjectMeta{Name: "pclass8"}, Value: int32(1000)}, metav1.CreateOptions{})
		if err != nil && !apierrors.IsAlreadyExists(err) {
			framework.Failf("unexpected error while creating priority class: %v", err)
		}

		hard := v1.ResourceList{}
		hard[v1.ResourcePods] = resource.MustParse("1")

		ginkgo.By("Creating a ResourceQuota with priority class scope")
		resourceQuotaPriorityClass, err := createResourceQuota(ctx, f.ClientSet, f.Namespace.Name, newTestResourceQuotaWithScopeForPriorityClass("quota-priorityclass", hard, v1.ScopeSelectorOpExists, []string{}))
		framework.ExpectNoError(err)

		ginkgo.By("Ensuring ResourceQuota status is calculated")
		usedResources := v1.ResourceList{}
		usedResources[v1.ResourcePods] = resource.MustParse("0")
		err = waitForResourceQuota(ctx, f.ClientSet, f.Namespace.Name, resourceQuotaPriorityClass.Name, usedResources)
		framework.ExpectNoError(err)

		ginkgo.By("Creating a pod with priority class pclass8")
		podName := "testpod-pclass8"
		pod := newTestPodForQuotaWithPriority(f, podName, v1.ResourceList{}, v1.ResourceList{}, "pclass8")
		pod, err = f.ClientSet.CoreV1().Pods(f.Namespace.Name).Create(ctx, pod, metav1.CreateOptions{})
		framework.ExpectNoError(err)

		ginkgo.By("Ensuring resource quota with priority class is updated with the pod usage")
		usedResources[v1.ResourcePods] = resource.MustParse("1")
		err = waitForResourceQuota(ctx, f.ClientSet, f.Namespace.Name, resourceQuotaPriorityClass.Name, usedResources)
		framework.ExpectNoError(err)

		ginkgo.By("Deleting the pod")
		err = f.ClientSet.CoreV1().Pods(f.Namespace.Name).Delete(ctx, pod.Name, *metav1.NewDeleteOptions(0))
		framework.ExpectNoError(err)

		ginkgo.By("Ensuring resource quota status released the pod usage")
		usedResources[v1.ResourcePods] = resource.MustParse("0")
		err = waitForResourceQuota(ctx, f.ClientSet, f.Namespace.Name, resourceQuotaPriorityClass.Name, usedResources)
		framework.ExpectNoError(err)
	})

	ginkgo.It("should verify ResourceQuota's priority class scope (cpu, memory quota set) against a pod with same priority class.", func(ctx context.Context) {

		_, err := f.ClientSet.SchedulingV1().PriorityClasses().Create(ctx, &schedulingv1.PriorityClass{ObjectMeta: metav1.ObjectMeta{Name: "pclass9"}, Value: int32(1000)}, metav1.CreateOptions{})
		if err != nil && !apierrors.IsAlreadyExists(err) {
			framework.Failf("unexpected error while creating priority class: %v", err)
		}

		hard := v1.ResourceList{}
		hard[v1.ResourcePods] = resource.MustParse("1")
		hard[v1.ResourceRequestsCPU] = resource.MustParse("1")
		hard[v1.ResourceRequestsMemory] = resource.MustParse("1Gi")
		hard[v1.ResourceLimitsCPU] = resource.MustParse("3")
		hard[v1.ResourceLimitsMemory] = resource.MustParse("3Gi")

		ginkgo.By("Creating a ResourceQuota with priority class scope")
		resourceQuotaPriorityClass, err := createResourceQuota(ctx, f.ClientSet, f.Namespace.Name, newTestResourceQuotaWithScopeForPriorityClass("quota-priorityclass", hard, v1.ScopeSelectorOpIn, []string{"pclass9"}))
		framework.ExpectNoError(err)

		ginkgo.By("Ensuring ResourceQuota status is calculated")
		usedResources := v1.ResourceList{}
		usedResources[v1.ResourcePods] = resource.MustParse("0")
		usedResources[v1.ResourceRequestsCPU] = resource.MustParse("0")
		usedResources[v1.ResourceRequestsMemory] = resource.MustParse("0Gi")
		usedResources[v1.ResourceLimitsCPU] = resource.MustParse("0")
		usedResources[v1.ResourceLimitsMemory] = resource.MustParse("0Gi")
		err = waitForResourceQuota(ctx, f.ClientSet, f.Namespace.Name, resourceQuotaPriorityClass.Name, usedResources)
		framework.ExpectNoError(err)

		ginkgo.By("Creating a pod with priority class")
		podName := "testpod-pclass9"
		request := v1.ResourceList{}
		request[v1.ResourceCPU] = resource.MustParse("1")
		request[v1.ResourceMemory] = resource.MustParse("1Gi")
		limit := v1.ResourceList{}
		limit[v1.ResourceCPU] = resource.MustParse("2")
		limit[v1.ResourceMemory] = resource.MustParse("2Gi")

		pod := newTestPodForQuotaWithPriority(f, podName, request, limit, "pclass9")
		pod, err = f.ClientSet.CoreV1().Pods(f.Namespace.Name).Create(ctx, pod, metav1.CreateOptions{})
		framework.ExpectNoError(err)

		ginkgo.By("Ensuring resource quota with priority class scope captures the pod usage")
		usedResources[v1.ResourcePods] = resource.MustParse("1")
		usedResources[v1.ResourceRequestsCPU] = resource.MustParse("1")
		usedResources[v1.ResourceRequestsMemory] = resource.MustParse("1Gi")
		usedResources[v1.ResourceLimitsCPU] = resource.MustParse("2")
		usedResources[v1.ResourceLimitsMemory] = resource.MustParse("2Gi")
		err = waitForResourceQuota(ctx, f.ClientSet, f.Namespace.Name, resourceQuotaPriorityClass.Name, usedResources)
		framework.ExpectNoError(err)

		ginkgo.By("Deleting the pod")
		err = f.ClientSet.CoreV1().Pods(f.Namespace.Name).Delete(ctx, pod.Name, *metav1.NewDeleteOptions(0))
		framework.ExpectNoError(err)

		ginkgo.By("Ensuring resource quota status released the pod usage")
		usedResources[v1.ResourcePods] = resource.MustParse("0")
		usedResources[v1.ResourceRequestsCPU] = resource.MustParse("0")
		usedResources[v1.ResourceRequestsMemory] = resource.MustParse("0Gi")
		usedResources[v1.ResourceLimitsCPU] = resource.MustParse("0")
		usedResources[v1.ResourceLimitsMemory] = resource.MustParse("0Gi")
		err = waitForResourceQuota(ctx, f.ClientSet, f.Namespace.Name, resourceQuotaPriorityClass.Name, usedResources)
		framework.ExpectNoError(err)
	})

})

var _ = SIGDescribe("ResourceQuota", func() {
	f := framework.NewDefaultFramework("cross-namespace-pod-affinity")
	f.NamespacePodSecurityLevel = admissionapi.LevelBaseline
	ginkgo.It("should verify ResourceQuota with cross namespace pod affinity scope using scope-selectors.", func(ctx context.Context) {
		ginkgo.By("Creating a ResourceQuota with cross namespace pod affinity scope")
		quota, err := createResourceQuota(
			ctx, f.ClientSet, f.Namespace.Name, newTestResourceQuotaWithScopeSelector("quota-cross-namespace-pod-affinity", v1.ResourceQuotaScopeCrossNamespacePodAffinity))
		framework.ExpectNoError(err)

		ginkgo.By("Ensuring ResourceQuota status is calculated")
		wantUsedResources := v1.ResourceList{v1.ResourcePods: resource.MustParse("0")}
		err = waitForResourceQuota(ctx, f.ClientSet, f.Namespace.Name, quota.Name, wantUsedResources)
		framework.ExpectNoError(err)

		ginkgo.By("Creating a pod that does not use cross namespace affinity")
		pod := newTestPodWithAffinityForQuota(f, "no-cross-namespace-affinity", &v1.Affinity{
			PodAntiAffinity: &v1.PodAntiAffinity{
				RequiredDuringSchedulingIgnoredDuringExecution: []v1.PodAffinityTerm{{
					TopologyKey: "region",
				}}}})
		pod, err = f.ClientSet.CoreV1().Pods(f.Namespace.Name).Create(ctx, pod, metav1.CreateOptions{})
		framework.ExpectNoError(err)

		ginkgo.By("Creating a pod that uses namespaces field")
		podWithNamespaces := newTestPodWithAffinityForQuota(f, "with-namespaces", &v1.Affinity{
			PodAntiAffinity: &v1.PodAntiAffinity{
				RequiredDuringSchedulingIgnoredDuringExecution: []v1.PodAffinityTerm{{
					TopologyKey: "region",
					Namespaces:  []string{"ns1"},
				}}}})
		podWithNamespaces, err = f.ClientSet.CoreV1().Pods(f.Namespace.Name).Create(ctx, podWithNamespaces, metav1.CreateOptions{})
		framework.ExpectNoError(err)

		ginkgo.By("Ensuring resource quota captures podWithNamespaces usage")
		wantUsedResources[v1.ResourcePods] = resource.MustParse("1")
		err = waitForResourceQuota(ctx, f.ClientSet, f.Namespace.Name, quota.Name, wantUsedResources)
		framework.ExpectNoError(err)

		ginkgo.By("Creating a pod that uses namespaceSelector field")
		podWithNamespaceSelector := newTestPodWithAffinityForQuota(f, "with-namespace-selector", &v1.Affinity{
			PodAntiAffinity: &v1.PodAntiAffinity{
				RequiredDuringSchedulingIgnoredDuringExecution: []v1.PodAffinityTerm{{
					TopologyKey: "region",
					NamespaceSelector: &metav1.LabelSelector{
						MatchExpressions: []metav1.LabelSelectorRequirement{
							{
								Key:      "team",
								Operator: metav1.LabelSelectorOpIn,
								Values:   []string{"ads"},
							},
						},
					}}}}})
		podWithNamespaceSelector, err = f.ClientSet.CoreV1().Pods(f.Namespace.Name).Create(ctx, podWithNamespaceSelector, metav1.CreateOptions{})
		framework.ExpectNoError(err)

		ginkgo.By("Ensuring resource quota captures podWithNamespaceSelector usage")
		wantUsedResources[v1.ResourcePods] = resource.MustParse("2")
		err = waitForResourceQuota(ctx, f.ClientSet, f.Namespace.Name, quota.Name, wantUsedResources)
		framework.ExpectNoError(err)

		ginkgo.By("Deleting the pods")
		err = f.ClientSet.CoreV1().Pods(f.Namespace.Name).Delete(ctx, pod.Name, *metav1.NewDeleteOptions(0))
		framework.ExpectNoError(err)
		err = f.ClientSet.CoreV1().Pods(f.Namespace.Name).Delete(ctx, podWithNamespaces.Name, *metav1.NewDeleteOptions(0))
		framework.ExpectNoError(err)
		err = f.ClientSet.CoreV1().Pods(f.Namespace.Name).Delete(ctx, podWithNamespaceSelector.Name, *metav1.NewDeleteOptions(0))
		framework.ExpectNoError(err)

		ginkgo.By("Ensuring resource quota status released the pod usage")
		wantUsedResources[v1.ResourcePods] = resource.MustParse("0")
		err = waitForResourceQuota(ctx, f.ClientSet, f.Namespace.Name, quota.Name, wantUsedResources)
		framework.ExpectNoError(err)
	})
})

// newTestResourceQuotaWithScopeSelector returns a quota that enforces default constraints for testing with scopeSelectors
func newTestResourceQuotaWithScopeSelector(name string, scope v1.ResourceQuotaScope) *v1.ResourceQuota {
	hard := v1.ResourceList{}
	hard[v1.ResourcePods] = resource.MustParse("5")
	switch scope {
	case v1.ResourceQuotaScopeTerminating, v1.ResourceQuotaScopeNotTerminating:
		hard[v1.ResourceRequestsCPU] = resource.MustParse("1")
		hard[v1.ResourceRequestsMemory] = resource.MustParse("500Mi")
		hard[v1.ResourceLimitsCPU] = resource.MustParse("2")
		hard[v1.ResourceLimitsMemory] = resource.MustParse("1Gi")
	}
	return &v1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1.ResourceQuotaSpec{Hard: hard,
			ScopeSelector: &v1.ScopeSelector{
				MatchExpressions: []v1.ScopedResourceSelectorRequirement{
					{
						ScopeName: scope,
						Operator:  v1.ScopeSelectorOpExists},
				},
			},
		},
	}
}

// newTestResourceQuotaWithScope returns a quota that enforces default constraints for testing with scopes
func newTestResourceQuotaWithScope(name string, scope v1.ResourceQuotaScope) *v1.ResourceQuota {
	hard := v1.ResourceList{}
	hard[v1.ResourcePods] = resource.MustParse("5")
	switch scope {
	case v1.ResourceQuotaScopeTerminating, v1.ResourceQuotaScopeNotTerminating:
		hard[v1.ResourceRequestsCPU] = resource.MustParse("1")
		hard[v1.ResourceRequestsMemory] = resource.MustParse("500Mi")
		hard[v1.ResourceLimitsCPU] = resource.MustParse("2")
		hard[v1.ResourceLimitsMemory] = resource.MustParse("1Gi")
	}
	return &v1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       v1.ResourceQuotaSpec{Hard: hard, Scopes: []v1.ResourceQuotaScope{scope}},
	}
}

// newTestResourceQuotaWithScopeForPriorityClass returns a quota
// that enforces default constraints for testing with ResourceQuotaScopePriorityClass scope
func newTestResourceQuotaWithScopeForPriorityClass(name string, hard v1.ResourceList, op v1.ScopeSelectorOperator, values []string) *v1.ResourceQuota {
	return &v1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1.ResourceQuotaSpec{Hard: hard,
			ScopeSelector: &v1.ScopeSelector{
				MatchExpressions: []v1.ScopedResourceSelectorRequirement{
					{
						ScopeName: v1.ResourceQuotaScopePriorityClass,
						Operator:  op,
						Values:    values,
					},
				},
			},
		},
	}
}

// newTestResourceQuotaWithScopeForVolumeAttributesClass returns a quota
// that enforces default constraints for testing with ResourceQuotaScopeVolumeAttributesClass scope
func newTestResourceQuotaWithScopeForVolumeAttributesClass(name string, hard v1.ResourceList, op v1.ScopeSelectorOperator, values []string) *v1.ResourceQuota {
	return &v1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1.ResourceQuotaSpec{Hard: hard,
			ScopeSelector: &v1.ScopeSelector{
				MatchExpressions: []v1.ScopedResourceSelectorRequirement{
					{
						ScopeName: v1.ResourceQuotaScopeVolumeAttributesClass,
						Operator:  op,
						Values:    values,
					},
				},
			},
		},
	}
}

// newTestResourceQuota returns a quota that enforces default constraints for testing
func newTestResourceQuota(name string) *v1.ResourceQuota {
	hard := v1.ResourceList{}
	hard[v1.ResourcePods] = resource.MustParse("5")
	hard[v1.ResourceServices] = resource.MustParse("10")
	hard[v1.ResourceServicesNodePorts] = resource.MustParse("1")
	hard[v1.ResourceServicesLoadBalancers] = resource.MustParse("1")
	hard[v1.ResourceReplicationControllers] = resource.MustParse("10")
	hard[v1.ResourceQuotas] = resource.MustParse("1")
	hard[v1.ResourceCPU] = resource.MustParse("1")
	hard[v1.ResourceMemory] = resource.MustParse("500Mi")
	hard[v1.ResourceConfigMaps] = resource.MustParse("10")
	hard[v1.ResourceSecrets] = resource.MustParse("10")
	hard[v1.ResourcePersistentVolumeClaims] = resource.MustParse("10")
	hard[v1.ResourceRequestsStorage] = resource.MustParse("10Gi")
	hard[v1.ResourceEphemeralStorage] = resource.MustParse("50Gi")
	hard[core.V1ResourceByStorageClass(classGold, v1.ResourcePersistentVolumeClaims)] = resource.MustParse("10")
	hard[core.V1ResourceByStorageClass(classGold, v1.ResourceRequestsStorage)] = resource.MustParse("10Gi")
	// test quota on discovered resource type
	hard[v1.ResourceName("count/replicasets.apps")] = resource.MustParse("5")
	// test quota on extended resource
	hard[v1.ResourceName(v1.DefaultResourceRequestsPrefix+extendedResourceName)] = resource.MustParse("3")
	return &v1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       v1.ResourceQuotaSpec{Hard: hard},
	}
}

// newTestResourceQuotaDRA returns a quota that includes hard limits for ResourceClaim objects.
func newTestResourceQuotaDRA(name string) *v1.ResourceQuota {
	quota := newTestResourceQuota(name)
	quota.Spec.Hard[core.ClaimObjectCountName] = resource.MustParse("1")
	quota.Spec.Hard[core.V1ResourceByDeviceClass(classGold)] = resource.MustParse("1")
	return quota
}

// newTestPodForQuota returns a pod that has the specified requests and limits
func newTestPodForQuota(f *framework.Framework, name string, requests v1.ResourceList, limits v1.ResourceList) *v1.Pod {
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: v1.PodSpec{
			// prevent disruption to other test workloads in parallel test runs by ensuring the quota
			// test pods don't get scheduled onto a node
			NodeSelector: map[string]string{
				"x-test.k8s.io/unsatisfiable": "not-schedulable",
			},
			Containers: []v1.Container{
				{
					Name:  "pause",
					Image: imageutils.GetPauseImageName(),
					Resources: v1.ResourceRequirements{
						Requests: requests,
						Limits:   limits,
					},
				},
			},
		},
	}
}

// newTestPodForQuotaWithPriority returns a pod that has the specified requests, limits and priority class
func newTestPodForQuotaWithPriority(f *framework.Framework, name string, requests v1.ResourceList, limits v1.ResourceList, pclass string) *v1.Pod {
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: v1.PodSpec{
			// prevent disruption to other test workloads in parallel test runs by ensuring the quota
			// test pods don't get scheduled onto a node
			NodeSelector: map[string]string{
				"x-test.k8s.io/unsatisfiable": "not-schedulable",
			},
			Containers: []v1.Container{
				{
					Name:  "pause",
					Image: imageutils.GetPauseImageName(),
					Resources: v1.ResourceRequirements{
						Requests: requests,
						Limits:   limits,
					},
				},
			},
			PriorityClassName: pclass,
		},
	}
}

// newTestPodForQuota returns a pod that has the specified requests and limits
func newTestPodWithAffinityForQuota(f *framework.Framework, name string, affinity *v1.Affinity) *v1.Pod {
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: v1.PodSpec{
			// prevent disruption to other test workloads in parallel test runs by ensuring the quota
			// test pods don't get scheduled onto a node
			NodeSelector: map[string]string{
				"x-test.k8s.io/unsatisfiable": "not-schedulable",
			},
			Affinity: affinity,
			Containers: []v1.Container{
				{
					Name:      "pause",
					Image:     imageutils.GetPauseImageName(),
					Resources: v1.ResourceRequirements{},
				},
			},
		},
	}
}

// newTestPersistentVolumeClaimForQuota returns a simple persistent volume claim
func newTestPersistentVolumeClaimForQuota(name string) *v1.PersistentVolumeClaim {
	return &v1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: v1.PersistentVolumeClaimSpec{
			AccessModes: []v1.PersistentVolumeAccessMode{
				v1.ReadWriteOnce,
				v1.ReadOnlyMany,
			},
			Resources: v1.VolumeResourceRequirements{
				Requests: v1.ResourceList{
					v1.ResourceName(v1.ResourceStorage): resource.MustParse("1Gi"),
				},
			},
		},
	}
}

// newTestResourceClaimForQuota returns a simple resource claim
func newTestResourceClaimForQuota(name string) *resourceapi.ResourceClaim {
	return &resourceapi.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: resourceapi.ResourceClaimSpec{
			Devices: resourceapi.DeviceClaim{
				Requests: []resourceapi.DeviceRequest{{
					Name:            "req-0",
					DeviceClassName: classGold,
				}},
			},
		},
	}
}

// newTestReplicationControllerForQuota returns a simple replication controller
func newTestReplicationControllerForQuota(name, image string, replicas int32) *v1.ReplicationController {
	return &v1.ReplicationController{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: v1.ReplicationControllerSpec{
			Replicas: pointer.Int32(replicas),
			Selector: map[string]string{
				"name": name,
			},
			Template: &v1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"name": name},
				},
				Spec: v1.PodSpec{
					Containers: []v1.Container{
						{
							Name:  name,
							Image: image,
						},
					},
				},
			},
		},
	}
}

// newTestReplicaSetForQuota returns a simple replica set
func newTestReplicaSetForQuota(name, image string, replicas int32) *appsv1.ReplicaSet {
	zero := int64(0)
	return &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: appsv1.ReplicaSetSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"name": name}},
			Template: v1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"name": name},
				},
				Spec: v1.PodSpec{
					TerminationGracePeriodSeconds: &zero,
					Containers: []v1.Container{
						{
							Name:  name,
							Image: image,
						},
					},
				},
			},
		},
	}
}

// newTestServiceForQuota returns a simple service
func newTestServiceForQuota(name string, serviceType v1.ServiceType, allocateLoadBalancerNodePorts bool) *v1.Service {
	var allocateNPs *bool
	// Only set allocateLoadBalancerNodePorts when service type is LB
	if serviceType == v1.ServiceTypeLoadBalancer {
		allocateNPs = &allocateLoadBalancerNodePorts
	}

	return &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: v1.ServiceSpec{
			Type: serviceType,
			Ports: []v1.ServicePort{{
				Port:       80,
				TargetPort: intstr.FromInt32(80),
			}},
			AllocateLoadBalancerNodePorts: allocateNPs,
		},
	}
}

func newTestConfigMapForQuota(name string) *v1.ConfigMap {
	return &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Data: map[string]string{
			"a": "b",
		},
	}
}

func newTestSecretForQuota(name string) *v1.Secret {
	return &v1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Data: map[string][]byte{
			"data-1": []byte("value-1\n"),
			"data-2": []byte("value-2\n"),
			"data-3": []byte("value-3\n"),
		},
	}
}

// createResourceQuota in the specified namespace
func createResourceQuota(ctx context.Context, c clientset.Interface, namespace string, resourceQuota *v1.ResourceQuota) (*v1.ResourceQuota, error) {
	return c.CoreV1().ResourceQuotas(namespace).Create(ctx, resourceQuota, metav1.CreateOptions{})
}

// deleteResourceQuota with the specified name
func deleteResourceQuota(ctx context.Context, c clientset.Interface, namespace, name string) error {
	return c.CoreV1().ResourceQuotas(namespace).Delete(ctx, name, metav1.DeleteOptions{})
}

// countResourceQuota counts the number of ResourceQuota in the specified namespace
// On contended servers the service account controller can slow down, leading to the count changing during a run.
// Wait up to 5s for the count to stabilize, assuming that updates come at a consistent rate, and are not held indefinitely.
func countResourceQuota(ctx context.Context, c clientset.Interface, namespace string) (int, error) {
	found, unchanged := 0, 0
	return found, wait.PollUntilContextTimeout(ctx, 1*time.Second, 30*time.Second, false, func(ctx context.Context) (bool, error) {
		resourceQuotas, err := c.CoreV1().ResourceQuotas(namespace).List(ctx, metav1.ListOptions{})
		framework.ExpectNoError(err)
		if len(resourceQuotas.Items) == found {
			// loop until the number of resource quotas has stabilized for 5 seconds
			unchanged++
			return unchanged > 4, nil
		}
		unchanged = 0
		found = len(resourceQuotas.Items)
		return false, nil
	})
}

// wait for resource quota status to show the expected used resources value
func waitForResourceQuota(ctx context.Context, c clientset.Interface, ns, quotaName string, used v1.ResourceList) error {
	return wait.PollUntilContextTimeout(ctx, framework.Poll, resourceQuotaTimeout, false, func(ctx context.Context) (bool, error) {
		resourceQuota, err := c.CoreV1().ResourceQuotas(ns).Get(ctx, quotaName, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		// used may not yet be calculated
		if resourceQuota.Status.Used == nil {
			return false, nil
		}
		// verify that the quota shows the expected used resource values
		for k, v := range used {
			if actualValue, found := resourceQuota.Status.Used[k]; !found || (actualValue.Cmp(v) != 0) {
				framework.Logf("resource %s, expected %s, actual %s", k, v.String(), actualValue.String())
				return false, nil
			}
		}
		return true, nil
	})
}

// updateResourceQuotaUntilUsageAppears updates the resource quota object until the usage is populated
// for the specific resource name.
func updateResourceQuotaUntilUsageAppears(ctx context.Context, c clientset.Interface, ns, quotaName string, resourceName v1.ResourceName) error {
	return wait.PollUntilContextTimeout(ctx, framework.Poll, resourceQuotaTimeout, false, func(ctx context.Context) (bool, error) {
		resourceQuota, err := c.CoreV1().ResourceQuotas(ns).Get(ctx, quotaName, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		// verify that the quota shows the expected used resource values
		_, ok := resourceQuota.Status.Used[resourceName]
		if ok {
			return true, nil
		}

		current := resourceQuota.Spec.Hard[resourceName]
		current.Add(resource.MustParse("1"))
		resourceQuota.Spec.Hard[resourceName] = current
		_, err = c.CoreV1().ResourceQuotas(ns).Update(ctx, resourceQuota, metav1.UpdateOptions{})
		// ignoring conflicts since someone else may already updated it.
		if apierrors.IsConflict(err) {
			return false, nil
		}
		return false, err
	})
}

func unstructuredToResourceQuota(obj *unstructured.Unstructured) (*v1.ResourceQuota, error) {
	json, err := runtime.Encode(unstructured.UnstructuredJSONScheme, obj)
	if err != nil {
		return nil, err
	}
	rq := &v1.ResourceQuota{}
	err = runtime.DecodeInto(clientscheme.Codecs.LegacyCodec(v1.SchemeGroupVersion), json, rq)

	return rq, err
}
