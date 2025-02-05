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

package apimachinery

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"math/big"
	"net"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/discovery"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
	apiregistrationv1 "k8s.io/kube-aggregator/pkg/apis/apiregistration/v1"
	aggregatorclient "k8s.io/kube-aggregator/pkg/client/clientset_generated/clientset"
	rbacv1helpers "k8s.io/kubernetes/pkg/apis/rbac/v1"
	"k8s.io/kubernetes/test/e2e/framework"
	e2eauth "k8s.io/kubernetes/test/e2e/framework/auth"
	e2edeployment "k8s.io/kubernetes/test/e2e/framework/deployment"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	"k8s.io/kubernetes/test/utils/format"
	imageutils "k8s.io/kubernetes/test/utils/image"
	admissionapi "k8s.io/pod-security-admission/api"
	samplev1alpha1 "k8s.io/sample-apiserver/pkg/apis/wardle/v1alpha1"
	"k8s.io/utils/pointer"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
)

const (
	aggregatorServicePort = 7443

	apiServiceRetryPeriod  = 1 * time.Second
	apiServiceRetryTimeout = 2 * time.Minute

	defaultApiServiceGroupName = samplev1alpha1.GroupName
	defaultApiServiceVersion   = "v1alpha1"
)

var _ = SIGDescribe("Aggregator", func() {
	var aggrclient *aggregatorclient.Clientset

	f := framework.NewDefaultFramework("aggregator")
	f.NamespacePodSecurityLevel = admissionapi.LevelBaseline

	// We want namespace initialization BeforeEach inserted by
	// NewDefaultFramework to happen before this, so we put this BeforeEach
	// after NewDefaultFramework.
	ginkgo.BeforeEach(func() {
		config, err := framework.LoadConfig()
		if err != nil {
			framework.Failf("could not load config: %v", err)
		}
		aggrclient, err = aggregatorclient.NewForConfig(config)
		if err != nil {
			framework.Failf("could not create aggregator client: %v", err)
		}
		apiServiceName := defaultApiServiceVersion + "." + defaultApiServiceGroupName
		ginkgo.DeferCleanup(cleanupSampleAPIServer, f.ClientSet, aggrclient, generateSampleAPIServerObjectNames(f.Namespace.Name), apiServiceName)
	})

	/*
		Release: v1.17, v1.21, v1.27
		Testname: aggregator-supports-the-sample-apiserver
		Description: Ensure that the sample-apiserver code from 1.17 and compiled against 1.17
		will work on the current Aggregator/API-Server.
	*/
	framework.ConformanceIt("Should be able to support the 1.17 Sample API Server using the current Aggregator", func(ctx context.Context) {
		// Testing a 1.17 version of the sample-apiserver
		TestSampleAPIServer(ctx, f, aggrclient, imageutils.GetE2EImage(imageutils.APIServer), defaultApiServiceGroupName, defaultApiServiceVersion)
	})
})

func cleanupSampleAPIServer(ctx context.Context, client clientset.Interface, aggrclient *aggregatorclient.Clientset, n sampleAPIServerObjectNames, apiServiceName string) {
	// delete the APIService first to avoid causing discovery errors
	_ = aggrclient.ApiregistrationV1().APIServices().Delete(ctx, apiServiceName, metav1.DeleteOptions{})

	_ = client.AppsV1().Deployments(n.namespace).Delete(ctx, "sample-apiserver-deployment", metav1.DeleteOptions{})
	_ = client.CoreV1().Secrets(n.namespace).Delete(ctx, "sample-apiserver-secret", metav1.DeleteOptions{})
	_ = client.CoreV1().Services(n.namespace).Delete(ctx, "sample-api", metav1.DeleteOptions{})
	_ = client.CoreV1().ServiceAccounts(n.namespace).Delete(ctx, "sample-apiserver", metav1.DeleteOptions{})
	_ = client.RbacV1().RoleBindings("kube-system").Delete(ctx, n.roleBinding, metav1.DeleteOptions{})
	_ = client.RbacV1().ClusterRoleBindings().Delete(ctx, "wardler:"+n.namespace+":auth-delegator", metav1.DeleteOptions{})
	_ = client.RbacV1().ClusterRoles().Delete(ctx, n.clusterRole, metav1.DeleteOptions{})
	_ = client.RbacV1().ClusterRoleBindings().Delete(ctx, n.clusterRoleBinding, metav1.DeleteOptions{})
}

type sampleAPIServerObjectNames struct {
	namespace          string
	roleBinding        string
	clusterRole        string
	clusterRoleBinding string
}

func generateSampleAPIServerObjectNames(namespace string) sampleAPIServerObjectNames {
	return sampleAPIServerObjectNames{
		namespace:          namespace,
		roleBinding:        "wardler-auth-reader-" + namespace,
		clusterRole:        "sample-apiserver-reader-" + namespace,
		clusterRoleBinding: "wardler:" + namespace + "sample-apiserver-reader-" + namespace,
	}
}

// SetUpSampleAPIServer sets up a sample-apiserver.
//
// Important:
// Test cases that call this function should be marked as serial due to potential conflicts
// with other test cases that also set up a sample-apiserver. For more information, see
// https://github.com/kubernetes/kubernetes/issues/119582#issuecomment-2215054411.
func SetUpSampleAPIServer(ctx context.Context, f *framework.Framework, aggrclient *aggregatorclient.Clientset, image string, n sampleAPIServerObjectNames, apiServiceGroupName, apiServiceVersion string) {
	ginkgo.By("Registering the sample API server.")
	client := f.ClientSet
	restClient := client.Discovery().RESTClient()
	certCtx := setupServerCert(n.namespace, "sample-api")
	apiServiceName := apiServiceVersion + "." + apiServiceGroupName

	// kubectl create -f namespace.yaml
	// NOTE: aggregated apis should generally be set up in their own namespace. As the test framework is setting up a new namespace, we are just using that.

	// kubectl create -f secret.yaml
	secretName := "sample-apiserver-secret"
	secret := &v1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: secretName,
		},
		Type: v1.SecretTypeOpaque,
		Data: map[string][]byte{
			"tls.crt": certCtx.cert,
			"tls.key": certCtx.key,
		},
	}
	_, err := client.CoreV1().Secrets(n.namespace).Create(ctx, secret, metav1.CreateOptions{})
	framework.ExpectNoError(err, "creating secret %s in.namespace %s", secretName, n.namespace)

	if e2eauth.IsRBACEnabled(ctx, client.RbacV1()) {
		// kubectl create -f clusterrole.yaml
		_, err = client.RbacV1().ClusterRoles().Create(ctx, &rbacv1.ClusterRole{

			ObjectMeta: metav1.ObjectMeta{Name: n.clusterRole},
			Rules: []rbacv1.PolicyRule{
				rbacv1helpers.NewRule("get", "list", "watch").Groups("").Resources("namespaces").RuleOrDie(),
				rbacv1helpers.NewRule("get", "list", "watch").Groups("admissionregistration.k8s.io").Resources("*").RuleOrDie(),
				rbacv1helpers.NewRule("get", "list", "watch").Groups("flowcontrol.apiserver.k8s.io").Resources("prioritylevelconfigurations", "flowschemas").RuleOrDie(),
			},
		}, metav1.CreateOptions{})
		framework.ExpectNoError(err, "creating cluster role %s", n.clusterRole)

		_, err = client.RbacV1().ClusterRoleBindings().Create(ctx, &rbacv1.ClusterRoleBinding{
			ObjectMeta: metav1.ObjectMeta{
				Name: n.clusterRoleBinding,
			},
			RoleRef: rbacv1.RoleRef{
				APIGroup: "rbac.authorization.k8s.io",
				Kind:     "ClusterRole",
				Name:     n.clusterRole,
			},
			Subjects: []rbacv1.Subject{
				{
					APIGroup:  "",
					Kind:      "ServiceAccount",
					Name:      "default",
					Namespace: n.namespace,
				},
			},
		}, metav1.CreateOptions{})
		framework.ExpectNoError(err, "creating cluster role binding %s", n.clusterRoleBinding)

		// kubectl create -f authDelegator.yaml
		_, err = client.RbacV1().ClusterRoleBindings().Create(ctx, &rbacv1.ClusterRoleBinding{
			ObjectMeta: metav1.ObjectMeta{
				Name: "wardler:" + n.namespace + ":auth-delegator",
			},
			RoleRef: rbacv1.RoleRef{
				APIGroup: "rbac.authorization.k8s.io",
				Kind:     "ClusterRole",
				Name:     "system:auth-delegator",
			},
			Subjects: []rbacv1.Subject{
				{
					APIGroup:  "",
					Kind:      "ServiceAccount",
					Name:      "default",
					Namespace: n.namespace,
				},
			},
		}, metav1.CreateOptions{})
		framework.ExpectNoError(err, "creating cluster role binding %s", "wardler:"+n.namespace+":auth-delegator")
	}

	// kubectl create -f deploy.yaml
	deploymentName := "sample-apiserver-deployment"
	etcdImage := imageutils.GetE2EImage(imageutils.Etcd)
	podLabels := map[string]string{"app": "sample-apiserver", "apiserver": "true"}
	replicas := int32(1)
	etcdLocalhostAddress := "127.0.0.1"
	if framework.TestContext.ClusterIsIPv6() {
		etcdLocalhostAddress = "::1"
	}
	etcdURL := fmt.Sprintf("http://%s", net.JoinHostPort(etcdLocalhostAddress, "2379"))

	mounts := []v1.VolumeMount{
		{
			Name:      "apiserver-certs",
			ReadOnly:  true,
			MountPath: "/apiserver.local.config/certificates",
		},
	}
	volumes := []v1.Volume{
		{
			Name: "apiserver-certs",
			VolumeSource: v1.VolumeSource{
				Secret: &v1.SecretVolumeSource{SecretName: secretName},
			},
		},
	}
	containers := []v1.Container{
		{
			Name:         "sample-apiserver",
			VolumeMounts: mounts,
			Args: []string{
				fmt.Sprintf("--etcd-servers=%s", etcdURL),
				"--tls-cert-file=/apiserver.local.config/certificates/tls.crt",
				"--tls-private-key-file=/apiserver.local.config/certificates/tls.key",
				"--audit-log-path=-",
				"--audit-log-maxage=0",
				"--audit-log-maxbackup=0",
			},
			Image: image,
			ReadinessProbe: &v1.Probe{
				ProbeHandler: v1.ProbeHandler{
					HTTPGet: &v1.HTTPGetAction{
						Scheme: v1.URISchemeHTTPS,
						Port:   intstr.FromInt32(443),
						Path:   "/readyz",
					},
				},
				InitialDelaySeconds: 20,
				PeriodSeconds:       1,
				SuccessThreshold:    1,
				FailureThreshold:    3,
			},
		},
		{
			Name:  "etcd",
			Image: etcdImage,
			Command: []string{
				"/usr/local/bin/etcd",
				"--listen-client-urls",
				etcdURL,
				"--advertise-client-urls",
				etcdURL,
			},
		},
	}
	d := e2edeployment.NewDeployment(deploymentName, replicas, podLabels, "", "", appsv1.RollingUpdateDeploymentStrategyType)
	d.Spec.Template.Spec.Containers = containers
	d.Spec.Template.Spec.Volumes = volumes

	deployment, err := client.AppsV1().Deployments(n.namespace).Create(ctx, d, metav1.CreateOptions{})
	framework.ExpectNoError(err, "creating deployment %s in namespace %s", deploymentName, n.namespace)

	err = e2edeployment.WaitForDeploymentRevisionAndImage(client, n.namespace, deploymentName, "1", image)
	framework.ExpectNoError(err, "waiting for the deployment of image %s in %s in %s to complete", image, deploymentName, n.namespace)

	err = e2edeployment.WaitForDeploymentRevisionAndImage(client, n.namespace, deploymentName, "1", etcdImage)
	framework.ExpectNoError(err, "waiting for the deployment of image %s in %s in %s to complete", etcdImage, deploymentName, n.namespace)

	// kubectl create -f service.yaml
	serviceLabels := map[string]string{"apiserver": "true"}
	service := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: n.namespace,
			Name:      "sample-api",
			Labels:    map[string]string{"test": "aggregator"},
		},
		Spec: v1.ServiceSpec{
			Selector: serviceLabels,
			Ports: []v1.ServicePort{
				{
					Protocol:   v1.ProtocolTCP,
					Port:       aggregatorServicePort,
					TargetPort: intstr.FromInt32(443),
				},
			},
		},
	}
	_, err = client.CoreV1().Services(n.namespace).Create(ctx, service, metav1.CreateOptions{})
	framework.ExpectNoError(err, "creating service %s in namespace %s", "sample-api", n.namespace)

	// kubectl create -f serviceAccount.yaml
	sa := &v1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "sample-apiserver"}}
	_, err = client.CoreV1().ServiceAccounts(n.namespace).Create(ctx, sa, metav1.CreateOptions{})
	framework.ExpectNoError(err, "creating service account %s in namespace %s", "sample-apiserver", n.namespace)

	if e2eauth.IsRBACEnabled(ctx, client.RbacV1()) {
		// kubectl create -f auth-reader.yaml
		_, err = client.RbacV1().RoleBindings("kube-system").Create(ctx, &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{
				Name: n.roleBinding,
				Annotations: map[string]string{
					rbacv1.AutoUpdateAnnotationKey: "true",
				},
			},
			RoleRef: rbacv1.RoleRef{
				APIGroup: "",
				Kind:     "Role",
				Name:     "extension-apiserver-authentication-reader",
			},
			Subjects: []rbacv1.Subject{
				{
					Kind:      "ServiceAccount",
					Name:      "default",
					Namespace: n.namespace,
				},
			},
		}, metav1.CreateOptions{})
		framework.ExpectNoError(err, "creating role binding %s in namespace %s", n.roleBinding, "kube-system")
	}

	// Wait for the extension apiserver to be up and healthy
	// kubectl get deployments -n <aggregated-api-namespace> && status == Running
	// NOTE: aggregated apis should generally be set up in their own namespace (<aggregated-api-namespace>). As the test framework
	// is setting up a new namespace, we are just using that.
	err = e2edeployment.WaitForDeploymentComplete(client, deployment)
	framework.ExpectNoError(err, "deploying extension apiserver in namespace %s", n.namespace)

	// kubectl create -f apiservice.yaml
	_, err = aggrclient.ApiregistrationV1().APIServices().Create(ctx, &apiregistrationv1.APIService{
		ObjectMeta: metav1.ObjectMeta{Name: apiServiceName},
		Spec: apiregistrationv1.APIServiceSpec{
			Service: &apiregistrationv1.ServiceReference{
				Namespace: n.namespace,
				Name:      "sample-api",
				Port:      pointer.Int32(aggregatorServicePort),
			},
			Group:                apiServiceGroupName,
			Version:              apiServiceVersion,
			CABundle:             certCtx.signingCert,
			GroupPriorityMinimum: 2000,
			VersionPriority:      200,
		},
	}, metav1.CreateOptions{})
	framework.ExpectNoError(err, "creating apiservice %s", apiServiceName)

	var (
		currentAPIService *apiregistrationv1.APIService
		currentPods       *v1.PodList
	)

	err = pollTimed(ctx, 100*time.Millisecond, 60*time.Second, func(ctx context.Context) (bool, error) {

		currentAPIService, _ = aggrclient.ApiregistrationV1().APIServices().Get(ctx, apiServiceName, metav1.GetOptions{})
		currentPods, _ = client.CoreV1().Pods(n.namespace).List(ctx, metav1.ListOptions{})

		request := restClient.Get().AbsPath("/apis/" + apiServiceGroupName + "/" + apiServiceVersion + "/namespaces/default/flunders")
		request.SetHeader("Accept", "application/json")
		_, err := request.DoRaw(ctx)
		if err != nil {
			status, ok := err.(*apierrors.StatusError)
			if !ok {
				return false, err
			}
			if status.Status().Code == 403 || status.Status().Code == 503 {
				return false, nil
			}
			if status.Status().Code == 404 && strings.HasPrefix(err.Error(), "the server could not find the requested resource") {
				return false, nil
			}
			return false, err
		}
		return true, nil
	}, "Waited %s for the sample-apiserver to be ready to handle requests.")
	if err != nil {
		currentAPIServiceJSON, _ := json.Marshal(currentAPIService)
		framework.Logf("current APIService: %s", string(currentAPIServiceJSON))

		currentPodsJSON, _ := json.Marshal(currentPods)
		framework.Logf("current pods: %s", string(currentPodsJSON))

		if currentPods != nil {
			for _, pod := range currentPods.Items {
				for _, container := range pod.Spec.Containers {
					logs, err := e2epod.GetPodLogs(ctx, client, n.namespace, pod.Name, container.Name)
					framework.Logf("logs of %s/%s (error: %v): %s", pod.Name, container.Name, err, logs)
				}
			}
		}
	}
	framework.ExpectNoError(err, "gave up waiting for apiservice wardle to come up successfully")
}

// TestSampleAPIServer is a basic test if the sample-apiserver code from 1.29 and compiled against 1.29
// will work on the current Aggregator/API-Server.
func TestSampleAPIServer(ctx context.Context, f *framework.Framework, aggrclient *aggregatorclient.Clientset, image, apiServiceGroupName, apiServiceVersion string) {
	n := generateSampleAPIServerObjectNames(f.Namespace.Name)
	SetUpSampleAPIServer(ctx, f, aggrclient, image, n, apiServiceGroupName, apiServiceVersion)
	client := f.ClientSet
	restClient := client.Discovery().RESTClient()

	flunderName := generateFlunderName("rest-flunder")
	apiServiceName := apiServiceVersion + "." + apiServiceGroupName

	// kubectl create -f flunders-1.yaml -v 9
	// curl -k -v -XPOST https://localhost/apis/wardle.example.com/v1alpha1/namespaces/default/flunders
	// Request Body: {"apiVersion":"wardle.example.com/v1alpha1","kind":"Flunder","metadata":{"labels":{"sample-label":"true"},"name":"test-flunder","namespace":"default"}}
	flunder := `{"apiVersion":"` + apiServiceGroupName + `/` + apiServiceVersion + `","kind":"Flunder","metadata":{"labels":{"sample-label":"true"},"name":"` + flunderName + `","namespace":"default"}}`
	result := restClient.Post().AbsPath("/apis/"+apiServiceGroupName+"/"+apiServiceVersion+"/namespaces/default/flunders").Body([]byte(flunder)).SetHeader("Accept", "application/json").Do(ctx)
	framework.ExpectNoError(result.Error(), "creating a new flunders resource")
	var statusCode int
	result.StatusCode(&statusCode)
	if statusCode != 201 {
		framework.Failf("Flunders client creation response was status %d, not 201", statusCode)
	}
	u := &unstructured.Unstructured{}
	if err := result.Into(u); err != nil {
		framework.ExpectNoError(err, "reading created response")
	}

	gomega.Expect(u.GetAPIVersion()).To(gomega.Equal(apiServiceGroupName + "/" + apiServiceVersion))
	gomega.Expect(u.GetKind()).To(gomega.Equal("Flunder"))
	gomega.Expect(u.GetName()).To(gomega.Equal(flunderName))

	pods, err := client.CoreV1().Pods(n.namespace).List(ctx, metav1.ListOptions{})
	framework.ExpectNoError(err, "getting pods for flunders service")

	// kubectl get flunders -v 9
	// curl -k -v -XGET https://localhost/apis/wardle.example.com/v1alpha1/namespaces/default/flunders
	contents, err := restClient.Get().AbsPath("/apis/"+apiServiceGroupName+"/"+apiServiceVersion+"/namespaces/default/flunders").SetHeader("Accept", "application/json").DoRaw(ctx)
	framework.ExpectNoError(err, "attempting to get a newly created flunders resource")
	var flundersList samplev1alpha1.FlunderList
	err = json.Unmarshal(contents, &flundersList)
	validateErrorWithDebugInfo(ctx, f, err, pods, "Error in unmarshalling %T response from server %s", contents, "/apis/"+apiServiceGroupName+"/"+apiServiceVersion)
	if len(flundersList.Items) != 1 {
		framework.Failf("failed to get back the correct flunders list %v", flundersList)
	}

	// kubectl delete flunder test-flunder -v 9
	// curl -k -v -XDELETE  https://35.193.112.40/apis/wardle.example.com/v1alpha1/namespaces/default/flunders/test-flunder
	_, err = restClient.Delete().AbsPath("/apis/" + apiServiceGroupName + "/" + apiServiceVersion + "/namespaces/default/flunders/" + flunderName).DoRaw(ctx)
	validateErrorWithDebugInfo(ctx, f, err, pods, "attempting to delete a newly created flunders(%v) resource", flundersList.Items)

	// kubectl get flunders -v 9
	// curl -k -v -XGET https://localhost/apis/wardle.example.com/v1alpha1/namespaces/default/flunders
	contents, err = restClient.Get().AbsPath("/apis/"+apiServiceGroupName+"/"+apiServiceVersion+"/namespaces/default/flunders").SetHeader("Accept", "application/json").DoRaw(ctx)
	framework.ExpectNoError(err, "confirming delete of a newly created flunders resource")
	err = json.Unmarshal(contents, &flundersList)
	validateErrorWithDebugInfo(ctx, f, err, pods, "Error in unmarshalling %T response from server %s", contents, "/apis/"+apiServiceGroupName+"/"+apiServiceVersion)
	if len(flundersList.Items) != 0 {
		framework.Failf("failed to get back the correct deleted flunders list %v", flundersList)
	}

	flunderName = generateFlunderName("dynamic-flunder")

	// Rerun the Create/List/Delete tests using the Dynamic client.
	resources, discoveryErr := client.Discovery().ServerPreferredNamespacedResources()
	groupVersionResources, err := discovery.GroupVersionResources(resources)
	framework.ExpectNoError(err, "getting group version resources for dynamic client")
	gvr := schema.GroupVersionResource{Group: apiServiceGroupName, Version: apiServiceVersion, Resource: "flunders"}
	_, ok := groupVersionResources[gvr]
	if !ok {
		framework.Failf("could not find group version resource for dynamic client and wardle/flunders (discovery error: %v, discovery results: %#v)", discoveryErr, groupVersionResources)
	}
	dynamicClient := f.DynamicClient.Resource(gvr).Namespace(n.namespace)

	// kubectl create -f flunders-1.yaml
	// Request Body: {"apiVersion":"wardle.example.com/v1alpha1","kind":"Flunder","metadata":{"labels":{"sample-label":"true"},"name":"test-flunder","namespace":"default"}}
	testFlunder := samplev1alpha1.Flunder{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Flunder",
			APIVersion: apiServiceGroupName + "/" + apiServiceVersion,
		},
		ObjectMeta: metav1.ObjectMeta{Name: flunderName},
		Spec:       samplev1alpha1.FlunderSpec{},
	}
	jsonFlunder, err := json.Marshal(testFlunder)
	framework.ExpectNoError(err, "marshalling test-flunder for create using dynamic client")
	unstruct := &unstructured.Unstructured{}
	err = unstruct.UnmarshalJSON(jsonFlunder)
	framework.ExpectNoError(err, "unmarshalling test-flunder as unstructured for create using dynamic client")
	_, err = dynamicClient.Create(ctx, unstruct, metav1.CreateOptions{})
	framework.ExpectNoError(err, "listing flunders using dynamic client")

	// kubectl get flunders
	unstructuredList, err := dynamicClient.List(ctx, metav1.ListOptions{})
	framework.ExpectNoError(err, "listing flunders using dynamic client")
	if len(unstructuredList.Items) != 1 {
		framework.Failf("failed to get back the correct flunders list %v from the dynamic client", unstructuredList)
	}

	ginkgo.By("Read Status for " + apiServiceName)
	statusContent, err := restClient.Get().
		AbsPath("/apis/apiregistration.k8s.io/v1/apiservices/"+apiServiceName+"/status").
		SetHeader("Accept", "application/json").DoRaw(ctx)
	framework.ExpectNoError(err, "No response for .../apiservices/"+apiServiceName+"/status. Error: %v", err)

	var jr *apiregistrationv1.APIService
	err = json.Unmarshal([]byte(statusContent), &jr)
	framework.ExpectNoError(err, "Failed to process statusContent: %v | err: %v ", string(statusContent), err)
	gomega.Expect(jr.Status.Conditions[0].Message).To(gomega.Equal("all checks passed"), "The Message returned was %v", jr.Status.Conditions[0].Message)

	ginkgo.By("kubectl patch apiservice " + apiServiceName + " -p '{\"spec\":{\"versionPriority\": 400}}'")
	patchContent, err := restClient.Patch(types.MergePatchType).
		AbsPath("/apis/apiregistration.k8s.io/v1/apiservices/"+apiServiceName).
		SetHeader("Accept", "application/json").
		Body([]byte(`{"spec":{"versionPriority": 400}}`)).DoRaw(ctx)

	framework.ExpectNoError(err, "Patch failed for .../apiservices/"+apiServiceName+". Error: %v", err)
	err = json.Unmarshal([]byte(patchContent), &jr)
	framework.ExpectNoError(err, "Failed to process patchContent: %v | err: %v ", string(patchContent), err)
	gomega.Expect(jr.Spec.VersionPriority).To(gomega.Equal(int32(400)), "The VersionPriority returned was %d", jr.Spec.VersionPriority)

	ginkgo.By("List APIServices")
	listApiservices, err := restClient.Get().
		AbsPath("/apis/apiregistration.k8s.io/v1/apiservices").
		SetHeader("Accept", "application/json").DoRaw(ctx)

	framework.ExpectNoError(err, "No response for /apis/apiregistration.k8s.io/v1/apiservices Error: %v", err)

	var list *apiregistrationv1.APIServiceList
	err = json.Unmarshal([]byte(listApiservices), &list)
	framework.ExpectNoError(err, "Failed to process APIServiceList: %v | err: %v ", list, err)

	locatedWardle := false
	for _, item := range list.Items {
		if item.Name == apiServiceName {
			framework.Logf("Found %s in APIServiceList", apiServiceName)
			locatedWardle = true
			break
		}
	}
	if !locatedWardle {
		framework.Failf("Unable to find %s in APIServiceList", apiServiceName)
	}

	// As the APIService doesn't have any labels currently set we need to
	// set one so that we can select it later when we call deleteCollection
	ginkgo.By("Adding a label to the APIService")
	apiServiceClient := aggrclient.ApiregistrationV1().APIServices()
	apiServiceLabel := map[string]string{"e2e-apiservice": "patched"}
	apiServicePatch, err := json.Marshal(map[string]interface{}{
		"metadata": map[string]interface{}{
			"labels": apiServiceLabel,
		},
	})
	framework.ExpectNoError(err, "failed to Marshal APIService JSON patch")
	_, err = apiServiceClient.Patch(ctx, apiServiceName, types.StrategicMergePatchType, []byte(apiServicePatch), metav1.PatchOptions{})
	framework.ExpectNoError(err, "failed to patch APIService")

	patchedApiService, err := apiServiceClient.Get(ctx, apiServiceName, metav1.GetOptions{})
	framework.ExpectNoError(err, "Unable to retrieve api service %s", apiServiceName)
	framework.Logf("APIService labels: %v", patchedApiService.Labels)

	ginkgo.By("Updating APIService Status")
	var updatedStatus, wardle *apiregistrationv1.APIService

	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var statusToUpdate *apiregistrationv1.APIService
		statusContent, err = restClient.Get().
			AbsPath("/apis/apiregistration.k8s.io/v1/apiservices/"+apiServiceName+"/status").
			SetHeader("Accept", "application/json").DoRaw(ctx)
		framework.ExpectNoError(err, "No response for .../apiservices/"+apiServiceName+"/status. Error: %v", err)

		err = json.Unmarshal([]byte(statusContent), &statusToUpdate)
		framework.ExpectNoError(err, "Failed to process statusContent: %v | err: %v ", string(statusContent), err)

		statusToUpdate.Status.Conditions = append(statusToUpdate.Status.Conditions, apiregistrationv1.APIServiceCondition{
			Type:    "StatusUpdated",
			Status:  "True",
			Reason:  "E2E",
			Message: "Set from e2e test",
		})

		updatedStatus, err = apiServiceClient.UpdateStatus(ctx, statusToUpdate, metav1.UpdateOptions{})
		return err
	})
	framework.ExpectNoError(err, "Failed to update status. %v", err)
	framework.Logf("updatedStatus.Conditions: %#v", updatedStatus.Status.Conditions)

	ginkgo.By("Confirm that " + apiServiceName + " /status was updated")
	statusContent, err = restClient.Get().
		AbsPath("/apis/apiregistration.k8s.io/v1/apiservices/"+apiServiceName+"/status").
		SetHeader("Accept", "application/json").DoRaw(ctx)
	framework.ExpectNoError(err, "No response for .../apiservices/"+apiServiceName+"/status. Error: %v", err)

	err = json.Unmarshal([]byte(statusContent), &wardle)
	framework.ExpectNoError(err, "Failed to process statusContent: %v | err: %v ", string(statusContent), err)

	foundUpdatedStatusCondition := false
	for _, cond := range wardle.Status.Conditions {
		if cond.Type == "StatusUpdated" && cond.Reason == "E2E" && cond.Message == "Set from e2e test" {
			framework.Logf("Found APIService %v with Labels: %v & Condition: %v", wardle.ObjectMeta.Name, wardle.Labels, cond)
			foundUpdatedStatusCondition = true
			break
		} else {
			framework.Logf("Observed APIService %v with Labels: %v & Condition: %v", wardle.ObjectMeta.Name, wardle.Labels, cond)
		}
	}
	if !foundUpdatedStatusCondition {
		framework.Failf("The updated status condition was not found in:\n%s", format.Object(wardle.Status.Conditions, 1))
	}
	framework.Logf("Found updated status condition for %s", wardle.ObjectMeta.Name)

	ginkgo.By(fmt.Sprintf("Replace APIService %s", apiServiceName))
	var updatedApiService *apiregistrationv1.APIService

	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		currentApiService, err := apiServiceClient.Get(ctx, apiServiceName, metav1.GetOptions{})
		framework.ExpectNoError(err, "Unable to get APIService %s", apiServiceName)
		currentApiService.Labels = map[string]string{
			apiServiceName: "updated",
		}
		updatedApiService, err = apiServiceClient.Update(ctx, currentApiService, metav1.UpdateOptions{})
		return err
	})
	framework.ExpectNoError(err)
	gomega.Expect(updatedApiService.Labels).To(gomega.HaveKeyWithValue(apiServiceName, "updated"), "should have the updated label but have %q", updatedApiService.Labels[apiServiceName])
	framework.Logf("Found updated apiService label for %q", apiServiceName)

	// kubectl delete flunder test-flunder
	ginkgo.By(fmt.Sprintf("Delete flunders resource %q", flunderName))
	err = dynamicClient.Delete(ctx, flunderName, metav1.DeleteOptions{})
	validateErrorWithDebugInfo(ctx, f, err, pods, "deleting flunders(%v) using dynamic client", unstructuredList.Items)

	// kubectl get flunders
	unstructuredList, err = dynamicClient.List(ctx, metav1.ListOptions{})
	framework.ExpectNoError(err, "listing flunders using dynamic client")
	if len(unstructuredList.Items) != 0 {
		framework.Failf("failed to get back the correct deleted flunders list %v from the dynamic client", unstructuredList)
	}

	ginkgo.By("Recreating test-flunder before removing endpoint via deleteCollection")
	jsonFlunder, err = json.Marshal(testFlunder)
	framework.ExpectNoError(err, "marshalling test-flunder for create using dynamic client")
	unstruct = &unstructured.Unstructured{}
	err = unstruct.UnmarshalJSON(jsonFlunder)
	framework.ExpectNoError(err, "unmarshalling test-flunder as unstructured for create using dynamic client")
	_, err = dynamicClient.Create(ctx, unstruct, metav1.CreateOptions{})
	framework.ExpectNoError(err, "listing flunders using dynamic client")

	// kubectl get flunders
	unstructuredList, err = dynamicClient.List(ctx, metav1.ListOptions{})
	framework.ExpectNoError(err, "listing flunders using dynamic client")
	if len(unstructuredList.Items) != 1 {
		framework.Failf("failed to get back the correct flunders list %v from the dynamic client", unstructuredList)
	}

	ginkgo.By("Read " + apiServiceName + " /status before patching it")
	statusContent, err = restClient.Get().
		AbsPath("/apis/apiregistration.k8s.io/v1/apiservices/"+apiServiceName+"/status").
		SetHeader("Accept", "application/json").DoRaw(ctx)
	framework.ExpectNoError(err, "No response for .../apiservices/"+apiServiceName+"/status. Error: %v", err)

	wardle.Reset()
	err = json.Unmarshal([]byte(statusContent), &wardle)
	framework.ExpectNoError(err, "Failed to process statusContent: %v | err: %v ", string(statusContent), err)

	ginkgo.By("Patch APIService Status")
	patch := map[string]interface{}{
		"status": map[string]interface{}{
			"conditions": append(wardle.Status.Conditions, apiregistrationv1.APIServiceCondition{
				Type:    "StatusPatched",
				Status:  "True",
				Reason:  "E2E",
				Message: "Set by e2e test",
			}),
		},
	}
	payload, err := json.Marshal(patch)
	framework.ExpectNoError(err, "Failed to marshal JSON. %v", err)

	_, err = restClient.Patch(types.MergePatchType).
		AbsPath("/apis/apiregistration.k8s.io/v1/apiservices/"+apiServiceName+"/status").
		SetHeader("Accept", "application/json").
		Body([]byte(payload)).
		DoRaw(ctx)
	framework.ExpectNoError(err, "Patch failed for .../apiservices/"+apiServiceName+"/status. Error: %v", err)

	ginkgo.By("Confirm that " + apiServiceName + " /status was patched")
	statusContent, err = restClient.Get().
		AbsPath("/apis/apiregistration.k8s.io/v1/apiservices/"+apiServiceName+"/status").
		SetHeader("Accept", "application/json").DoRaw(ctx)
	framework.ExpectNoError(err, "No response for .../apiservices/"+apiServiceName+"/status. Error: %v", err)

	wardle.Reset()
	err = json.Unmarshal([]byte(statusContent), &wardle)
	framework.ExpectNoError(err, "Failed to process statusContent: %v | err: %v ", string(statusContent), err)

	foundPatchedStatusCondition := false
	for _, cond := range wardle.Status.Conditions {
		if cond.Type == "StatusPatched" && cond.Reason == "E2E" && cond.Message == "Set by e2e test" {
			framework.Logf("Found APIService %v with Labels: %v & Conditions: %v", wardle.ObjectMeta.Name, wardle.Labels, cond)
			foundPatchedStatusCondition = true
			break
		} else {
			framework.Logf("Observed APIService %v with Labels: %v & Conditions: %v", wardle.ObjectMeta.Name, wardle.Labels, cond)
		}
	}
	if !foundPatchedStatusCondition {
		framework.Failf("The patched status condition was not found in:\n%s", format.Object(wardle.Status.Conditions, 1))
	}
	framework.Logf("Found patched status condition for %s", wardle.ObjectMeta.Name)

	apiServiceLabelSelector := labels.SelectorFromSet(updatedApiService.Labels).String()
	ginkgo.By(fmt.Sprintf("APIService deleteCollection with labelSelector: %q", apiServiceLabelSelector))

	ginkgo.By("List StorageVersions")
	listStorageVersions, err := client.InternalV1alpha1().StorageVersions().List(ctx, metav1.ListOptions{})
	framework.Logf("Neww RICHAAA Found these SVs %s", listStorageVersions)

	framework.ExpectNoError(err, "No response for /apis/internal.apiserver.k8s.io/v1alpha1 Error: %v", err)

	err = aggrclient.ApiregistrationV1().APIServices().DeleteCollection(ctx,
		metav1.DeleteOptions{},
		metav1.ListOptions{LabelSelector: apiServiceLabelSelector})
	framework.ExpectNoError(err, "Unable to delete apiservice %s", apiServiceName)

	ginkgo.By("Confirm that the generated APIService has been deleted")
	err = wait.PollImmediate(apiServiceRetryPeriod, apiServiceRetryTimeout, checkApiServiceListQuantity(ctx, aggrclient, apiServiceLabelSelector, 0))
	framework.ExpectNoError(err, "failed to count the required APIServices")
	framework.Logf("APIService %s has been deleted.", apiServiceName)

	cleanupSampleAPIServer(ctx, client, aggrclient, n, apiServiceName)
}

// pollTimed will call Poll but time how long Poll actually took.
// It will then framework.Logf the msg with the duration of the Poll.
// It is assumed that msg will contain one %s for the elapsed time.
func pollTimed(ctx context.Context, interval, timeout time.Duration, condition wait.ConditionWithContextFunc, msg string) error {
	defer func(start time.Time, msg string) {
		elapsed := time.Since(start)
		framework.Logf(msg, elapsed)
	}(time.Now(), msg)
	return wait.PollUntilContextTimeout(ctx, interval, timeout, false, condition)
}

func validateErrorWithDebugInfo(ctx context.Context, f *framework.Framework, err error, pods *v1.PodList, msg string, fields ...interface{}) {
	if err != nil {
		namespace := f.Namespace.Name
		msg := fmt.Sprintf(msg, fields...)
		msg += fmt.Sprintf(" but received unexpected error:\n%v", err)
		client := f.ClientSet
		slices, err := client.DiscoveryV1().EndpointSlices(namespace).List(ctx, metav1.ListOptions{LabelSelector: fmt.Sprintf("%s=%s", discoveryv1.LabelServiceName, "sample-api")})
		if err == nil {
			msg += fmt.Sprintf("\nFound endpoint slices for sample-api:\n%v", slices)
		}
		pds, err := client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{})
		if err == nil {
			msg += fmt.Sprintf("\nFound pods in %s:\n%v", namespace, pds)
			msg += fmt.Sprintf("\nOriginal pods in %s:\n%v", namespace, pods)
		}

		framework.Fail(msg)
	}
}

func generateFlunderName(base string) string {
	id, err := rand.Int(rand.Reader, big.NewInt(2147483647))
	if err != nil {
		return base
	}
	return fmt.Sprintf("%s-%d", base, id)
}

func checkApiServiceListQuantity(ctx context.Context, aggrclient *aggregatorclient.Clientset, label string, quantity int) func() (bool, error) {
	return func() (bool, error) {
		var err error

		framework.Logf("Requesting list of APIServices to confirm quantity")

		list, err := aggrclient.ApiregistrationV1().APIServices().List(ctx, metav1.ListOptions{LabelSelector: label})
		if err != nil {
			return false, err
		}

		if len(list.Items) != quantity {
			return false, err
		}
		framework.Logf("Found %d APIService with label %q", quantity, label)
		return true, nil
	}
}
