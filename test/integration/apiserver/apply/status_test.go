/*
Copyright 2020 The Kubernetes Authors.

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

package apiserver

import (
	"context"
	"strings"
	"testing"

	v1 "k8s.io/api/core/v1"
	apiextensionsclientset "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/json"
	"k8s.io/apiserver/pkg/features"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/client-go/dynamic"
	clientfeatures "k8s.io/client-go/features"
	clientfeaturestesting "k8s.io/client-go/features/testing"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	apiservertesting "k8s.io/kubernetes/cmd/kube-apiserver/app/testing"
	"k8s.io/kubernetes/test/integration/etcd"
	"k8s.io/kubernetes/test/integration/framework"
)

// namespace used for all tests, do not change this
const testNamespace = "statusnamespace"

var statusData = map[schema.GroupVersionResource]string{
	gvr("", "v1", "persistentvolumes"):                              `{"status": {"message": "hello"}}`,
	gvr("", "v1", "resourcequotas"):                                 `{"status": {"used": {"cpu": "5M"}}}`,
	gvr("", "v1", "services"):                                       `{"status": {"loadBalancer": {"ingress": [{"ip": "127.0.0.1"}]}}}`,
	gvr("extensions", "v1beta1", "ingresses"):                       `{"status": {"loadBalancer": {"ingress": [{"ip": "127.0.0.1"}]}}}`,
	gvr("networking.k8s.io", "v1beta1", "ingresses"):                `{"status": {"loadBalancer": {"ingress": [{"ip": "127.0.0.1"}]}}}`,
	gvr("networking.k8s.io", "v1", "ingresses"):                     `{"status": {"loadBalancer": {"ingress": [{"ip": "127.0.0.1"}]}}}`,
	gvr("autoscaling", "v1", "horizontalpodautoscalers"):            `{"status": {"currentReplicas": 5}}`,
	gvr("autoscaling", "v2", "horizontalpodautoscalers"):            `{"status": {"currentReplicas": 5}}`,
	gvr("batch", "v1", "cronjobs"):                                  `{"status": {"lastScheduleTime": null}}`,
	gvr("batch", "v1beta1", "cronjobs"):                             `{"status": {"lastScheduleTime": null}}`,
	gvr("storage.k8s.io", "v1", "volumeattachments"):                `{"status": {"attached": true}}`,
	gvr("policy", "v1", "poddisruptionbudgets"):                     `{"status": {"currentHealthy": 5}}`,
	gvr("policy", "v1beta1", "poddisruptionbudgets"):                `{"status": {"currentHealthy": 5}}`,
	gvr("resource.k8s.io", "v1alpha3", "resourceclaims"):            `{"status": {"allocation": {"nodeSelector": {"nodeSelectorTerms": [{"matchExpressions": [{"key": "some-label", "operator": "In", "values": ["some-value"]}] }]}}}}`,
	gvr("resource.k8s.io", "v1beta1", "resourceclaims"):             `{"status": {"allocation": {"nodeSelector": {"nodeSelectorTerms": [{"matchExpressions": [{"key": "some-label", "operator": "In", "values": ["some-value"]}] }]}}}}`,
	gvr("resource.k8s.io", "v1beta2", "resourceclaims"):             `{"status": {"allocation": {"nodeSelector": {"nodeSelectorTerms": [{"matchExpressions": [{"key": "some-label", "operator": "In", "values": ["some-value"]}] }]}}}}`,
	gvr("internal.apiserver.k8s.io", "v1alpha1", "storageversions"): `{"status": {"commonEncodingVersion":"v1","storageVersions":[{"apiServerID":"1","decodableVersions":["v1","v2"],"encodingVersion":"v1"}],"conditions":[{"type":"AllEncodingVersionsEqual","status":"True","lastTransitionTime":"2020-01-01T00:00:00Z","reason":"allEncodingVersionsEqual","message":"all encoding versions are set to v1"}]}}`,
	// standard for []metav1.Condition
	gvr("admissionregistration.k8s.io", "v1alpha1", "validatingadmissionpolicies"): `{"status": {"conditions":[{"type":"Accepted","status":"False","lastTransitionTime":"2020-01-01T00:00:00Z","reason":"RuleApplied","message":"Rule was applied"}]}}`,
	gvr("admissionregistration.k8s.io", "v1beta1", "validatingadmissionpolicies"):  `{"status": {"conditions":[{"type":"Accepted","status":"False","lastTransitionTime":"2020-01-01T00:00:00Z","reason":"RuleApplied","message":"Rule was applied"}]}}`,
	gvr("admissionregistration.k8s.io", "v1", "validatingadmissionpolicies"):       `{"status": {"conditions":[{"type":"Accepted","status":"False","lastTransitionTime":"2020-01-01T00:00:00Z","reason":"RuleApplied","message":"Rule was applied"}]}}`,
}

const statusDefault = `{"status": {"conditions": [{"type": "MyStatus", "status":"True"}]}}`

func gvr(g, v, r string) schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: g, Version: v, Resource: r}
}

func createMapping(groupVersion string, resource metav1.APIResource) (*meta.RESTMapping, error) {
	gv, err := schema.ParseGroupVersion(groupVersion)
	if err != nil {
		return nil, err
	}
	if len(resource.Group) > 0 || len(resource.Version) > 0 {
		gv = schema.GroupVersion{
			Group:   resource.Group,
			Version: resource.Version,
		}
	}
	gvk := gv.WithKind(resource.Kind)
	gvr := gv.WithResource(strings.TrimSuffix(resource.Name, "/status"))
	scope := meta.RESTScopeRoot
	if resource.Namespaced {
		scope = meta.RESTScopeNamespace
	}
	return &meta.RESTMapping{
		Resource:         gvr,
		GroupVersionKind: gvk,
		Scope:            scope,
	}, nil
}

// TestApplyStatus makes sure that applying the status works for all known types.
func TestApplyStatus(t *testing.T) {
	testApplyStatus(t, func(testing.TB, *rest.Config) {})
}

// TestApplyStatus makes sure that applying the status works for all known types.
func TestApplyStatusWithCBOR(t *testing.T) {
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.CBORServingAndStorage, true)
	clientfeaturestesting.SetFeatureDuringTest(t, clientfeatures.ClientsAllowCBOR, true)
	clientfeaturestesting.SetFeatureDuringTest(t, clientfeatures.ClientsPreferCBOR, true)
	testApplyStatus(t, func(t testing.TB, config *rest.Config) {
		config.Wrap(framework.AssertRequestResponseAsCBOR(t))
	})
}

func testApplyStatus(t *testing.T, reconfigureClient func(testing.TB, *rest.Config)) {
	server, err := apiservertesting.StartTestServer(t, apiservertesting.NewDefaultTestServerOptions(), []string{"--disable-admission-plugins", "ServiceAccount,TaintNodesByCondition"}, framework.SharedEtcd())
	if err != nil {
		t.Fatal(err)
	}
	defer server.TearDownFn()

	client, err := kubernetes.NewForConfig(server.ClientConfig)
	if err != nil {
		t.Fatal(err)
	}

	// create CRDs so we can make sure that custom resources do not get lost
	etcd.CreateTestCRDs(t, apiextensionsclientset.NewForConfigOrDie(server.ClientConfig), false, etcd.GetCustomResourceDefinitionData()...)
	if _, err := client.CoreV1().Namespaces().Create(context.TODO(), &v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: testNamespace}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	createData := etcd.GetEtcdStorageData()

	// gather resources to test
	_, resourceLists, err := client.Discovery().ServerGroupsAndResources()
	if err != nil {
		t.Fatalf("Failed to get ServerGroupsAndResources with error: %+v", err)
	}

	for _, resourceList := range resourceLists {
		for _, resource := range resourceList.APIResources {
			if !strings.HasSuffix(resource.Name, "/status") {
				continue
			}
			mapping, err := createMapping(resourceList.GroupVersion, resource)
			if err != nil {
				t.Fatal(err)
			}
			t.Run(mapping.Resource.String(), func(t *testing.T) {
				// both spec and status get wiped for CSRs,
				// nothing is expected to be managed for it, skip it
				if mapping.Resource.Resource == "certificatesigningrequests" {
					t.SkipNow()
				}

				status, ok := statusData[mapping.Resource]
				if !ok {
					status = statusDefault
				}
				newResource, ok := createData[mapping.Resource]
				if !ok {
					t.Fatalf("no test data for %s.  Please add a test for your new type to etcd.GetEtcdStorageData().", mapping.Resource)
				}
				newObj := unstructured.Unstructured{}
				if err := json.Unmarshal([]byte(newResource.Stub), &newObj.Object); err != nil {
					t.Fatal(err)
				}

				namespace := testNamespace
				if mapping.Scope == meta.RESTScopeRoot {
					namespace = ""
				}
				name := newObj.GetName()

				// etcd test stub data doesn't contain apiVersion/kind (!), but apply requires it
				newObj.SetGroupVersionKind(mapping.GroupVersionKind)

				dynamicClientConfig := rest.CopyConfig(server.ClientConfig)
				reconfigureClient(t, dynamicClientConfig)
				dynamicClient, err := dynamic.NewForConfig(dynamicClientConfig)
				if err != nil {
					t.Fatal(err)
				}

				rsc := dynamicClient.Resource(mapping.Resource).Namespace(namespace)
				// apply to create
				_, err = rsc.Apply(context.TODO(), name, &newObj, metav1.ApplyOptions{FieldManager: "create_test"})
				if err != nil {
					t.Fatal(err)
				}

				statusObj := unstructured.Unstructured{}
				if err := json.Unmarshal([]byte(status), &statusObj.Object); err != nil {
					t.Fatal(err)
				}
				statusObj.SetAPIVersion(mapping.GroupVersionKind.GroupVersion().String())
				statusObj.SetKind(mapping.GroupVersionKind.Kind)
				statusObj.SetName(name)

				obj, err := dynamicClient.
					Resource(mapping.Resource).
					Namespace(namespace).
					ApplyStatus(context.TODO(), name, &statusObj, metav1.ApplyOptions{FieldManager: "apply_status_test", Force: true})
				if err != nil {
					t.Fatalf("Failed to apply: %v", err)
				}

				accessor, err := meta.Accessor(obj)
				if err != nil {
					t.Fatalf("Failed to get meta accessor: %v:\n%v", err, obj)
				}

				managedFields := accessor.GetManagedFields()
				if managedFields == nil {
					t.Fatal("Empty managed fields")
				}
				if !findManager(managedFields, "apply_status_test") {
					t.Fatalf("Couldn't find apply_status_test: %v", managedFields)
				}
				if !findManager(managedFields, "create_test") {
					t.Fatalf("Couldn't find create_test: %v", managedFields)
				}

				if err := rsc.Delete(context.TODO(), name, *metav1.NewDeleteOptions(0)); err != nil {
					t.Fatalf("deleting final object failed: %v", err)
				}
			})
		}
	}
}

func findManager(managedFields []metav1.ManagedFieldsEntry, manager string) bool {
	for _, entry := range managedFields {
		if entry.Manager == manager {
			return true
		}
	}
	return false
}
