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
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	v1 "k8s.io/api/core/v1"
	apiextensionsclientset "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	apiservertesting "k8s.io/kubernetes/cmd/kube-apiserver/app/testing"

	"k8s.io/kubernetes/test/integration/etcd"
	"k8s.io/kubernetes/test/integration/framework"
	"k8s.io/kubernetes/test/utils/image"
)

// namespace used for all tests, do not change this
const resetFieldsNamespace = "reset-fields-namespace"

// resetFieldsStatusData contains statuses for all the resources in the
// statusData list with slightly different data to create a field manager
// conflict.
var resetFieldsStatusData = map[schema.GroupVersionResource]string{
	gvr("", "v1", "persistentvolumes"):                              `{"status": {"message": "hello2"}}`,
	gvr("", "v1", "resourcequotas"):                                 `{"status": {"used": {"cpu": "25M"}}}`,
	gvr("", "v1", "services"):                                       `{"status": {"loadBalancer": {"ingress": [{"ip": "127.0.0.2", "ipMode": "VIP"}]}}}`,
	gvr("extensions", "v1beta1", "ingresses"):                       `{"status": {"loadBalancer": {"ingress": [{"ip": "127.0.0.2"}]}}}`,
	gvr("networking.k8s.io", "v1beta1", "ingresses"):                `{"status": {"loadBalancer": {"ingress": [{"ip": "127.0.0.2"}]}}}`,
	gvr("networking.k8s.io", "v1", "ingresses"):                     `{"status": {"loadBalancer": {"ingress": [{"ip": "127.0.0.2"}]}}}`,
	gvr("autoscaling", "v1", "horizontalpodautoscalers"):            `{"status": {"currentReplicas": 25}}`,
	gvr("autoscaling", "v2", "horizontalpodautoscalers"):            `{"status": {"currentReplicas": 25}}`,
	gvr("batch", "v1", "cronjobs"):                                  `{"status": {"lastScheduleTime":  "2020-01-01T00:00:00Z"}}`,
	gvr("batch", "v1beta1", "cronjobs"):                             `{"status": {"lastScheduleTime":  "2020-01-01T00:00:00Z"}}`,
	gvr("storage.k8s.io", "v1", "volumeattachments"):                `{"status": {"attached": false}}`,
	gvr("policy", "v1", "poddisruptionbudgets"):                     `{"status": {"currentHealthy": 25}}`,
	gvr("policy", "v1beta1", "poddisruptionbudgets"):                `{"status": {"currentHealthy": 25}}`,
	gvr("resource.k8s.io", "v1alpha3", "resourceclaims"):            `{"status": {"allocation": {"nodeSelector": {"nodeSelectorTerms": [{"matchExpressions": [{"key": "some-label", "operator": "In", "values": ["some-other-value"]}] }]}}}}`,
	gvr("resource.k8s.io", "v1beta1", "resourceclaims"):             `{"status": {"allocation": {"nodeSelector": {"nodeSelectorTerms": [{"matchExpressions": [{"key": "some-label", "operator": "In", "values": ["some-other-value"]}] }]}}}}`,
	gvr("internal.apiserver.k8s.io", "v1alpha1", "storageversions"): `{"status": {"commonEncodingVersion":"v1","storageVersions":[{"apiServerID":"1","decodableVersions":["v1","v2"],"encodingVersion":"v1"}],"conditions":[{"type":"AllEncodingVersionsEqual","status":"False","lastTransitionTime":"2020-01-01T00:00:00Z","reason":"allEncodingVersionsEqual","message":"all encoding versions are set to v1"}]}}`,
	// standard for []metav1.Condition
	gvr("admissionregistration.k8s.io", "v1alpha1", "validatingadmissionpolicies"): `{"status": {"conditions":[{"type":"Accepted","status":"True","lastTransitionTime":"2020-01-01T00:00:00Z","reason":"RuleApplied","message":"Rule was applied"}]}}`,
	gvr("admissionregistration.k8s.io", "v1beta1", "validatingadmissionpolicies"):  `{"status": {"conditions":[{"type":"Accepted","status":"True","lastTransitionTime":"2020-01-01T00:00:00Z","reason":"RuleApplied","message":"Rule was applied"}]}}`,
	gvr("admissionregistration.k8s.io", "v1", "validatingadmissionpolicies"):       `{"status": {"conditions":[{"type":"Accepted","status":"True","lastTransitionTime":"2020-01-01T00:00:00Z","reason":"RuleApplied","message":"Rule was applied"}]}}`,
	gvr("networking.k8s.io", "v1alpha1", "servicecidrs"):                           `{"status": {"conditions":[{"type":"Accepted","status":"True","lastTransitionTime":"2020-01-01T00:00:00Z","reason":"RuleApplied","message":"Rule was applied"}]}}`,
	gvr("networking.k8s.io", "v1beta1", "servicecidrs"):                            `{"status": {"conditions":[{"type":"Accepted","status":"True","lastTransitionTime":"2020-01-01T00:00:00Z","reason":"RuleApplied","message":"Rule was applied"}]}}`,
	gvr("networking.k8s.io", "v1", "servicecidrs"):                                 `{"status": {"conditions":[{"type":"Accepted","status":"True","lastTransitionTime":"2020-01-01T00:00:00Z","reason":"RuleApplied","message":"Rule was applied"}]}}`,
}

// resetFieldsStatusDefault conflicts with statusDefault
const resetFieldsStatusDefault = `{"status": {"conditions": [{"type": "MyStatus", "status":"False"}]}}`

var resetFieldsSkippedResources = map[string]struct{}{}

// noConflicts is the set of resources for which
// a conflict cannot occur.
var noConflicts = map[string]struct{}{
	// both spec and status get wiped for CSRs,
	// nothing is expected to be managed for it, skip it
	"certificatesigningrequests": {},
	// storageVersions are skipped because their spec is empty
	// and thus they can never have a conflict.
	"storageversions": {},
	// servicecidrs are skipped because their spec is immutable
	// and thus they can never have a conflict.
	"servicecidrs": {},
	// namespaces only have a spec.finalizers field which is also skipped,
	// thus it will never have a conflict.
	"namespaces": {},
}

var image2 = image.GetE2EImage(image.Etcd)

// resetFieldsSpecData contains conflicting data with the objects in
// etcd.GetEtcdStorageDataForNamespace()
// It contains the minimal changes needed to conflict with all the fields
// added to resetFields by the strategy of each resource.
// In most cases, just one field on the spec is changed, but
// some also wipe metadata or other fields.
var resetFieldsSpecData = map[schema.GroupVersionResource]string{
	gvr("", "v1", "resourcequotas"):                                                `{"spec": {"hard": {"cpu": "25M"}}}`,
	gvr("", "v1", "namespaces"):                                                    `{"spec": {"finalizers": ["kubernetes2"]}}`,
	gvr("", "v1", "nodes"):                                                         `{"spec": {"unschedulable": false}}`,
	gvr("", "v1", "persistentvolumes"):                                             `{"spec": {"capacity": {"storage": "23M"}}}`,
	gvr("", "v1", "persistentvolumeclaims"):                                        `{"spec": {"resources": {"limits": {"storage": "21M"}}}}`,
	gvr("", "v1", "pods"):                                                          `{"metadata": {"deletionTimestamp": "2020-01-01T00:00:00Z", "ownerReferences":[]}, "spec": {"containers": [{"image": "` + image2 + `", "name": "container7"}]}}`,
	gvr("", "v1", "replicationcontrollers"):                                        `{"spec": {"selector": {"new": "stuff2"}}}`,
	gvr("", "v1", "resourcequotas"):                                                `{"spec": {"hard": {"cpu": "25M"}}}`,
	gvr("", "v1", "services"):                                                      `{"spec": {"type": "ClusterIP"}}`,
	gvr("apps", "v1", "daemonsets"):                                                `{"spec": {"template": {"spec": {"containers": [{"image": "` + image2 + `", "name": "container6"}]}}}}`,
	gvr("apps", "v1", "deployments"):                                               `{"metadata": {"labels": {"a":"c"}}, "spec": {"template": {"spec": {"containers": [{"image": "` + image2 + `", "name": "container6"}]}}}}`,
	gvr("apps", "v1", "replicasets"):                                               `{"spec": {"template": {"spec": {"containers": [{"image": "` + image2 + `", "name": "container4"}]}}}}`,
	gvr("apps", "v1", "statefulsets"):                                              `{"spec": {"selector": {"matchLabels": {"a2": "b2"}}}}`,
	gvr("autoscaling", "v1", "horizontalpodautoscalers"):                           `{"spec": {"maxReplicas": 23}}`,
	gvr("autoscaling", "v2", "horizontalpodautoscalers"):                           `{"spec": {"maxReplicas": 23}}`,
	gvr("autoscaling", "v2beta1", "horizontalpodautoscalers"):                      `{"spec": {"maxReplicas": 23}}`,
	gvr("autoscaling", "v2beta2", "horizontalpodautoscalers"):                      `{"spec": {"maxReplicas": 23}}`,
	gvr("batch", "v1", "jobs"):                                                     `{"spec": {"template": {"spec": {"containers": [{"image": "` + image2 + `", "name": "container1"}]}}}}`,
	gvr("batch", "v1", "cronjobs"):                                                 `{"spec": {"jobTemplate": {"spec": {"template": {"spec": {"containers": [{"image": "` + image2 + `", "name": "container0"}]}}}}}}`,
	gvr("batch", "v1beta1", "cronjobs"):                                            `{"spec": {"jobTemplate": {"spec": {"template": {"spec": {"containers": [{"image": "` + image2 + `", "name": "container0"}]}}}}}}`,
	gvr("certificates.k8s.io", "v1", "certificatesigningrequests"):                 `{}`,
	gvr("certificates.k8s.io", "v1beta1", "certificatesigningrequests"):            `{}`,
	gvr("flowcontrol.apiserver.k8s.io", "v1alpha1", "flowschemas"):                 `{"metadata": {"labels":{"a":"c"}}, "spec": {"priorityLevelConfiguration": {"name": "name2"}}}`,
	gvr("flowcontrol.apiserver.k8s.io", "v1beta1", "flowschemas"):                  `{"metadata": {"labels":{"a":"c"}}, "spec": {"priorityLevelConfiguration": {"name": "name2"}}}`,
	gvr("flowcontrol.apiserver.k8s.io", "v1beta2", "flowschemas"):                  `{"metadata": {"labels":{"a":"c"}}, "spec": {"priorityLevelConfiguration": {"name": "name2"}}}`,
	gvr("flowcontrol.apiserver.k8s.io", "v1beta3", "flowschemas"):                  `{"metadata": {"labels":{"a":"c"}}, "spec": {"priorityLevelConfiguration": {"name": "name2"}}}`,
	gvr("flowcontrol.apiserver.k8s.io", "v1", "flowschemas"):                       `{"metadata": {"labels":{"a":"c"}}, "spec": {"priorityLevelConfiguration": {"name": "name2"}}}`,
	gvr("flowcontrol.apiserver.k8s.io", "v1alpha1", "prioritylevelconfigurations"): `{"metadata": {"labels":{"a":"c"}}, "spec": {"limited": {"assuredConcurrencyShares": 23}}}`,
	gvr("flowcontrol.apiserver.k8s.io", "v1beta1", "prioritylevelconfigurations"):  `{"metadata": {"labels":{"a":"c"}}, "spec": {"limited": {"assuredConcurrencyShares": 23}}}`,
	gvr("flowcontrol.apiserver.k8s.io", "v1beta2", "prioritylevelconfigurations"):  `{"metadata": {"labels":{"a":"c"}}, "spec": {"limited": {"assuredConcurrencyShares": 23}}}`,
	gvr("flowcontrol.apiserver.k8s.io", "v1beta3", "prioritylevelconfigurations"):  `{"metadata": {"labels":{"a":"c"}}, "spec": {"limited": {"nominalConcurrencyShares": 23}}}`,
	gvr("flowcontrol.apiserver.k8s.io", "v1", "prioritylevelconfigurations"):       `{"metadata": {"labels":{"a":"c"}}, "spec": {"limited": {"nominalConcurrencyShares": 23}}}`,
	gvr("extensions", "v1beta1", "ingresses"):                                      `{"spec": {"backend": {"serviceName": "service2"}}}`,
	gvr("networking.k8s.io", "v1beta1", "ingresses"):                               `{"spec": {"backend": {"serviceName": "service2"}}}`,
	gvr("networking.k8s.io", "v1", "ingresses"):                                    `{"spec": {"defaultBackend": {"service": {"name": "service2"}}}}`,
	gvr("networking.k8s.io", "v1alpha1", "servicecidrs"):                           `{}`,
	gvr("networking.k8s.io", "v1beta1", "servicecidrs"):                            `{}`,
	gvr("networking.k8s.io", "v1", "servicecidrs"):                                 `{}`,
	gvr("policy", "v1", "poddisruptionbudgets"):                                    `{"spec": {"selector": {"matchLabels": {"anokkey2": "anokvalue"}}}}`,
	gvr("policy", "v1beta1", "poddisruptionbudgets"):                               `{"spec": {"selector": {"matchLabels": {"anokkey2": "anokvalue"}}}}`,
	gvr("storage.k8s.io", "v1alpha1", "volumeattachments"):                         `{"metadata": {"name": "va3"}, "spec": {"nodeName": "localhost2"}}`,
	gvr("storage.k8s.io", "v1", "volumeattachments"):                               `{"metadata": {"name": "va3"}, "spec": {"nodeName": "localhost2"}}`,
	gvr("apiextensions.k8s.io", "v1", "customresourcedefinitions"):                 `{"metadata": {"labels":{"a":"c"}}, "spec": {"group": "webconsole22.operator.openshift.io"}}`,
	gvr("apiextensions.k8s.io", "v1beta1", "customresourcedefinitions"):            `{"metadata": {"labels":{"a":"c"}}, "spec": {"group": "webconsole22.operator.openshift.io"}}`,
	gvr("awesome.bears.com", "v1", "pandas"):                                       `{"spec": {"replicas": 102}}`,
	gvr("awesome.bears.com", "v3", "pandas"):                                       `{"spec": {"replicas": 302}}`,
	gvr("apiregistration.k8s.io", "v1beta1", "apiservices"):                        `{"metadata": {"labels": {"a":"c"}}, "spec": {"group": "foo2.com"}}`,
	gvr("apiregistration.k8s.io", "v1", "apiservices"):                             `{"metadata": {"labels": {"a":"c"}}, "spec": {"group": "foo2.com"}}`,
	gvr("resource.k8s.io", "v1alpha3", "deviceclasses"):                            `{"metadata": {"labels":{"a":"c"}}}`,
	gvr("resource.k8s.io", "v1alpha3", "resourceclaims"):                           `{"spec": {"devices": {"requests": [{"name": "req-0", "deviceClassName": "other-class"}]}}}`, // spec is immutable, but that doesn't matter for the test.
	gvr("resource.k8s.io", "v1alpha3", "resourceclaimtemplates"):                   `{"spec": {"spec": {"resourceClassName": "class2name"}}}`,
	gvr("resource.k8s.io", "v1beta1", "deviceclasses"):                             `{"metadata": {"labels":{"a":"c"}}}`,
	gvr("resource.k8s.io", "v1beta1", "resourceclaims"):                            `{"spec": {"devices": {"requests": [{"name": "req-0", "deviceClassName": "other-class"}]}}}`, // spec is immutable, but that doesn't matter for the test.
	gvr("resource.k8s.io", "v1beta1", "resourceclaimtemplates"):                    `{"spec": {"spec": {"resourceClassName": "class2name"}}}`,
	gvr("internal.apiserver.k8s.io", "v1alpha1", "storageversions"):                `{}`,
	gvr("admissionregistration.k8s.io", "v1alpha1", "validatingadmissionpolicies"): `{"metadata": {"labels": {"a":"c"}}, "spec": {"paramKind": {"apiVersion": "apps/v1", "kind": "Deployment"}}}`,
	gvr("admissionregistration.k8s.io", "v1beta1", "validatingadmissionpolicies"):  `{"metadata": {"labels": {"a":"c"}}, "spec": {"paramKind": {"apiVersion": "apps/v1", "kind": "Deployment"}}}`,
	gvr("admissionregistration.k8s.io", "v1", "validatingadmissionpolicies"):       `{"metadata": {"labels": {"a":"c"}}, "spec": {"paramKind": {"apiVersion": "apps/v1", "kind": "Deployment"}}}`,
}

// TestResetFields makes sure that fieldManager does not own fields reset by the storage strategy.
// It takes 2 objects obj1 and obj2 that differ by one field in the spec and one field in the status.
// It applies obj1 to the spec endpoint and obj2 to the status endpoint, the lack of conflicts
// confirms that the fieldmanager1 is wiped of the status and fieldmanager2 is wiped of the spec.
// We then attempt to apply obj2 to the spec endpoint which fails with an expected conflict.
func TestApplyResetFields(t *testing.T) {
	server, err := apiservertesting.StartTestServer(t, apiservertesting.NewDefaultTestServerOptions(), []string{"--disable-admission-plugins", "ServiceAccount,TaintNodesByCondition"}, framework.SharedEtcd())
	if err != nil {
		t.Fatal(err)
	}
	defer server.TearDownFn()

	client, err := kubernetes.NewForConfig(server.ClientConfig)
	if err != nil {
		t.Fatal(err)
	}
	dynamicClient, err := dynamic.NewForConfig(server.ClientConfig)
	if err != nil {
		t.Fatal(err)
	}

	// create CRDs so we can make sure that custom resources do not get lost
	etcd.CreateTestCRDs(t, apiextensionsclientset.NewForConfigOrDie(server.ClientConfig), false, etcd.GetCustomResourceDefinitionData()...)

	if _, err := client.CoreV1().Namespaces().Create(context.TODO(), &v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: resetFieldsNamespace}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	createData := etcd.GetEtcdStorageDataForNamespace(resetFieldsNamespace)
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
				if _, ok := resetFieldsSkippedResources[mapping.Resource.Resource]; ok {
					t.Skip()
				}

				namespace := resetFieldsNamespace
				if mapping.Scope == meta.RESTScopeRoot {
					namespace = ""
				}

				// assemble first object
				status, ok := statusData[mapping.Resource]
				if !ok {
					status = statusDefault
				}

				resource, ok := createData[mapping.Resource]
				if !ok {
					t.Fatalf("no test data for %s.  Please add a test for your new type to etcd.GetEtcdStorageData() or getResetFieldsEtcdStorageData()", mapping.Resource)
				}

				obj1 := unstructured.Unstructured{}
				if err := json.Unmarshal([]byte(resource.Stub), &obj1.Object); err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal([]byte(status), &obj1.Object); err != nil {
					t.Fatal(err)
				}

				name := obj1.GetName()
				obj1.SetAPIVersion(mapping.GroupVersionKind.GroupVersion().String())
				obj1.SetKind(mapping.GroupVersionKind.Kind)
				obj1.SetName(name)

				// apply the spec of the first object
				_, err = dynamicClient.
					Resource(mapping.Resource).
					Namespace(namespace).
					Apply(context.TODO(), name, &obj1, metav1.ApplyOptions{FieldManager: "fieldmanager1"})
				if err != nil {
					t.Fatalf("Failed to apply obj1: %v", err)
				}

				// create second object
				obj2 := &unstructured.Unstructured{}
				obj1.DeepCopyInto(obj2)
				if err := json.Unmarshal([]byte(resetFieldsSpecData[mapping.Resource]), &obj2.Object); err != nil {
					t.Fatal(err)
				}
				status2, ok := resetFieldsStatusData[mapping.Resource]
				if !ok {
					status2 = resetFieldsStatusDefault
				}
				if err := json.Unmarshal([]byte(status2), &obj2.Object); err != nil {
					t.Fatal(err)
				}

				if reflect.DeepEqual(obj1, obj2) {
					t.Fatalf("obj1 and obj2 should not be equal %v", obj2)
				}

				// apply the status of the second object
				// this won't conflict if resetfields are set correctly
				// and will conflict if they are not
				_, err = dynamicClient.
					Resource(mapping.Resource).
					Namespace(namespace).
					ApplyStatus(context.TODO(), name, obj2, metav1.ApplyOptions{FieldManager: "fieldmanager2"})
				if err != nil {
					t.Fatalf("Failed to apply obj2: %v", err)
				}

				// skip checking for conflicts on resources
				// that will never have conflicts
				if _, ok = noConflicts[mapping.Resource.Resource]; !ok {
					var objRet *unstructured.Unstructured

					// reapply second object to the spec endpoint
					// that should fail with a conflict
					objRet, err = dynamicClient.
						Resource(mapping.Resource).
						Namespace(namespace).
						Apply(context.TODO(), name, obj2, metav1.ApplyOptions{FieldManager: "fieldmanager2"})
					err = expectConflict(objRet, err, dynamicClient, mapping.Resource, namespace, name)
					if err != nil {
						t.Fatalf("Did not get expected conflict in spec of %s %s/%s: %v", mapping.Resource, namespace, name, err)
					}

					// reapply first object to the status endpoint
					// that should fail with a conflict
					objRet, err = dynamicClient.
						Resource(mapping.Resource).
						Namespace(namespace).
						ApplyStatus(context.TODO(), name, &obj1, metav1.ApplyOptions{FieldManager: "fieldmanager1"})
					err = expectConflict(objRet, err, dynamicClient, mapping.Resource, namespace, name)
					if err != nil {
						t.Fatalf("Did not get expected conflict in status of %s %s/%s: %v", mapping.Resource, namespace, name, err)
					}
				}

				// cleanup
				rsc := dynamicClient.Resource(mapping.Resource).Namespace(namespace)
				if err := rsc.Delete(context.TODO(), name, *metav1.NewDeleteOptions(0)); err != nil {
					t.Fatalf("deleting final object failed: %v", err)
				}
			})
		}
	}
}

func expectConflict(objRet *unstructured.Unstructured, err error, dynamicClient dynamic.Interface, resource schema.GroupVersionResource, namespace, name string) error {
	if err != nil && strings.Contains(err.Error(), "conflict") {
		return nil
	}
	which := "returned"
	// something unexpected is going on here, let's not assume that objRet==nil if any only if err!=nil
	if objRet == nil {
		which = "subsequently fetched"
		var err2 error
		objRet, err2 = dynamicClient.
			Resource(resource).
			Namespace(namespace).
			Get(context.TODO(), name, metav1.GetOptions{})
		if err2 != nil {
			return fmt.Errorf("instead got error %w, and failed to Get object: %v", err, err2)
		}
	}
	marshBytes, marshErr := json.Marshal(objRet)
	var gotten string
	if marshErr == nil {
		gotten = string(marshBytes)
	} else {
		gotten = fmt.Sprintf("<failed to json.Marshall(%#+v): %v>", objRet, marshErr)
	}
	return fmt.Errorf("instead got error %w; %s object is %s", err, which, gotten)
}
