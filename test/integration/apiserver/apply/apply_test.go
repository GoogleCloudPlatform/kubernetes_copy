/*
Copyright 2018 The Kubernetes Authors.

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
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/stretchr/testify/require"

	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
	yamlutil "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/apiserver/pkg/features"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	appsv1ac "k8s.io/client-go/applyconfigurations/apps/v1"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"
	metav1ac "k8s.io/client-go/applyconfigurations/meta/v1"
	clientfeatures "k8s.io/client-go/features"
	clientfeaturestesting "k8s.io/client-go/features/testing"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	restclient "k8s.io/client-go/rest"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	kubeapiservertesting "k8s.io/kubernetes/cmd/kube-apiserver/app/testing"
	"k8s.io/kubernetes/test/integration/authutil"
	"k8s.io/kubernetes/test/integration/framework"
	"sigs.k8s.io/yaml"
)

func setup(t testing.TB) (clientset.Interface, kubeapiservertesting.TearDownFunc) {
	// Disable ServiceAccount admission plugin as we don't have serviceaccount controller running.
	server := kubeapiservertesting.StartTestServerOrDie(t, nil, framework.DefaultTestServerFlags(), framework.SharedEtcd())

	config := restclient.CopyConfig(server.ClientConfig)
	// There are some tests (in scale_test.go) that rely on the response to be returned in JSON.
	// So we overwrite it here.
	config.ContentType = runtime.ContentTypeJSON
	clientSet, err := clientset.NewForConfig(config)
	if err != nil {
		t.Fatalf("Error in create clientset: %v", err)
	}
	return clientSet, server.TearDownFn
}

// TestApplyAlsoCreates makes sure that PATCH requests with the apply content type
// will create the object if it doesn't already exist
// TODO: make a set of test cases in an easy-to-consume place (separate package?) so it's easy to test in both integration and e2e.
func TestApplyAlsoCreates(t *testing.T) {
	client, closeFn := setup(t)
	defer closeFn()

	testCases := []struct {
		resource string
		name     string
		body     string
	}{
		{
			resource: "pods",
			name:     "test-pod",
			body: `{
				"apiVersion": "v1",
				"kind": "Pod",
				"metadata": {
					"name": "test-pod"
				},
				"spec": {
					"containers": [{
						"name":  "test-container",
						"image": "test-image"
					}]
				}
			}`,
		}, {
			resource: "services",
			name:     "test-svc",
			body: `{
				"apiVersion": "v1",
				"kind": "Service",
				"metadata": {
					"name": "test-svc"
				},
				"spec": {
					"ports": [{
						"port": 8080,
						"protocol": "UDP"
					}]
				}
			}`,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := client.CoreV1().RESTClient().Patch(types.ApplyPatchType).
				Namespace("default").
				Resource(tc.resource).
				Name(tc.name).
				Param("fieldManager", "apply_test").
				Body([]byte(tc.body)).
				Do(context.TODO()).
				Get()
			if err != nil {
				t.Fatalf("Failed to create object using Apply patch: %v", err)
			}

			_, err = client.CoreV1().RESTClient().Get().Namespace("default").Resource(tc.resource).Name(tc.name).Do(context.TODO()).Get()
			if err != nil {
				t.Fatalf("Failed to retrieve object: %v", err)
			}

			// Test that we can re apply with a different field manager and don't get conflicts
			_, err = client.CoreV1().RESTClient().Patch(types.ApplyPatchType).
				Namespace("default").
				Resource(tc.resource).
				Name(tc.name).
				Param("fieldManager", "apply_test_2").
				Body([]byte(tc.body)).
				Do(context.TODO()).
				Get()
			if err != nil {
				t.Fatalf("Failed to re-apply object using Apply patch: %v", err)
			}
		})
	}
}

// TestNoOpUpdateSameResourceVersion makes sure that PUT requests which change nothing
// will not change the resource version (no write to etcd is done)
func TestNoOpUpdateSameResourceVersion(t *testing.T) {
	client, closeFn := setup(t)
	defer closeFn()

	podName := "no-op"
	podResource := "pods"
	podBytes := []byte(`{
		"apiVersion": "v1",
		"kind": "Pod",
		"metadata": {
			"name": "` + podName + `",
			"labels": {
				"a": "one",
				"c": "two",
				"b": "three"
			}
		},
		"spec": {
			"containers": [{
				"name":  "test-container-a",
				"image": "test-image-one"
			},{
				"name":  "test-container-c",
				"image": "test-image-two"
			},{
				"name":  "test-container-b",
				"image": "test-image-three"
			}]
		}
	}`)

	_, err := client.CoreV1().RESTClient().Patch(types.ApplyPatchType).
		Namespace("default").
		Param("fieldManager", "apply_test").
		Resource(podResource).
		Name(podName).
		Body(podBytes).
		Do(context.TODO()).
		Get()
	if err != nil {
		t.Fatalf("Failed to create object: %v", err)
	}

	// Sleep for one second to make sure that the times of each update operation is different.
	time.Sleep(1 * time.Second)

	createdObject, err := client.CoreV1().RESTClient().Get().Namespace("default").Resource(podResource).Name(podName).Do(context.TODO()).Get()
	if err != nil {
		t.Fatalf("Failed to retrieve created object: %v", err)
	}

	createdAccessor, err := meta.Accessor(createdObject)
	if err != nil {
		t.Fatalf("Failed to get meta accessor for created object: %v", err)
	}

	createdBytes, err := json.MarshalIndent(createdObject, "\t", "\t")
	if err != nil {
		t.Fatalf("Failed to marshal created object: %v", err)
	}

	// Test that we can put the same object and don't change the RV
	_, err = client.CoreV1().RESTClient().Put().
		Namespace("default").
		Resource(podResource).
		Name(podName).
		Body(createdBytes).
		Do(context.TODO()).
		Get()
	if err != nil {
		t.Fatalf("Failed to apply no-op update: %v", err)
	}

	updatedObject, err := client.CoreV1().RESTClient().Get().Namespace("default").Resource(podResource).Name(podName).Do(context.TODO()).Get()
	if err != nil {
		t.Fatalf("Failed to retrieve updated object: %v", err)
	}

	updatedAccessor, err := meta.Accessor(updatedObject)
	if err != nil {
		t.Fatalf("Failed to get meta accessor for updated object: %v", err)
	}

	updatedBytes, err := json.MarshalIndent(updatedObject, "\t", "\t")
	if err != nil {
		t.Fatalf("Failed to marshal updated object: %v", err)
	}

	if createdAccessor.GetResourceVersion() != updatedAccessor.GetResourceVersion() {
		t.Fatalf("Expected same resource version to be %v but got: %v\nold object:\n%v\nnew object:\n%v",
			createdAccessor.GetResourceVersion(),
			updatedAccessor.GetResourceVersion(),
			string(createdBytes),
			string(updatedBytes),
		)
	}
}

// TestNoOpApplyWithEmptyMap
func TestNoOpApplyWithEmptyMap(t *testing.T) {
	client, closeFn := setup(t)
	defer closeFn()

	deploymentName := "no-op"
	deploymentsResource := "deployments"
	deploymentBytes := []byte(`{
		"apiVersion": "apps/v1",
		"kind": "Deployment",
		"metadata": {
			"name": "` + deploymentName + `",
			"labels": {
				"app": "nginx"
			}
		},
		"spec": {
			"replicas": 1,
			"selector": {
				"matchLabels": {
					"app": "nginx"
				}
			},
			"template": {
				"metadata": {
					"annotations": {},
					"labels": {
						"app": "nginx"
					}
				},
				"spec": {
					"containers": [{
						"name": "nginx",
						"image": "nginx:1.14.2",
						"ports": [{
							"containerPort": 80
						}]
					}]
				}
			}
		}
	}`)

	_, err := client.AppsV1().RESTClient().Patch(types.ApplyPatchType).
		Namespace("default").
		Param("fieldManager", "apply_test").
		Resource(deploymentsResource).
		Name(deploymentName).
		Body(deploymentBytes).
		Do(context.TODO()).
		Get()
	if err != nil {
		t.Fatalf("Failed to create object: %v", err)
	}

	// This sleep is necessary to consistently produce different timestamps because the time field in managedFields has
	// 1 second granularity and if both apply requests happen during the same second, this test would flake.
	time.Sleep(1 * time.Second)

	createdObject, err := client.AppsV1().RESTClient().Get().Namespace("default").Resource(deploymentsResource).Name(deploymentName).Do(context.TODO()).Get()
	if err != nil {
		t.Fatalf("Failed to retrieve created object: %v", err)
	}

	createdAccessor, err := meta.Accessor(createdObject)
	if err != nil {
		t.Fatalf("Failed to get meta accessor for created object: %v", err)
	}

	createdBytes, err := json.MarshalIndent(createdObject, "\t", "\t")
	if err != nil {
		t.Fatalf("Failed to marshal created object: %v", err)
	}

	// Test that we can apply the same object and don't change the RV
	_, err = client.AppsV1().RESTClient().Patch(types.ApplyPatchType).
		Namespace("default").
		Param("fieldManager", "apply_test").
		Resource(deploymentsResource).
		Name(deploymentName).
		Body(deploymentBytes).
		Do(context.TODO()).
		Get()
	if err != nil {
		t.Fatalf("Failed to create object: %v", err)
	}

	updatedObject, err := client.AppsV1().RESTClient().Get().Namespace("default").Resource(deploymentsResource).Name(deploymentName).Do(context.TODO()).Get()
	if err != nil {
		t.Fatalf("Failed to retrieve updated object: %v", err)
	}

	updatedAccessor, err := meta.Accessor(updatedObject)
	if err != nil {
		t.Fatalf("Failed to get meta accessor for updated object: %v", err)
	}

	updatedBytes, err := json.MarshalIndent(updatedObject, "\t", "\t")
	if err != nil {
		t.Fatalf("Failed to marshal updated object: %v", err)
	}

	if createdAccessor.GetResourceVersion() != updatedAccessor.GetResourceVersion() {
		t.Fatalf("Expected same resource version to be %v but got: %v\nold object:\n%v\nnew object:\n%v",
			createdAccessor.GetResourceVersion(),
			updatedAccessor.GetResourceVersion(),
			string(createdBytes),
			string(updatedBytes),
		)
	}
}

// TestApplyEmptyMarkerStructDifferentFromNil
func TestApplyEmptyMarkerStructDifferentFromNil(t *testing.T) {
	client, closeFn := setup(t)
	defer closeFn()

	podName := "pod-with-empty-dir"
	podsResource := "pods"
	podBytesWithEmptyDir := []byte(`{
		"apiVersion": "v1",
		"kind": "Pod",
		"metadata": {
			"name": "` + podName + `"
		},
		"spec": {
			"containers": [{
				"name":  "test-container-a",
				"image": "test-image-one",
				"volumeMounts": [{
					"mountPath": "/cache",
					"name": "cache-volume"
				}],
			}],
			"volumes": [{
				"name": "cache-volume",
				"emptyDir": {}
			}]
		}
	}`)

	_, err := client.CoreV1().RESTClient().Patch(types.ApplyPatchType).
		Namespace("default").
		Param("fieldManager", "apply_test").
		Resource(podsResource).
		Name(podName).
		Body(podBytesWithEmptyDir).
		Do(context.TODO()).
		Get()
	if err != nil {
		t.Fatalf("Failed to create object: %v", err)
	}

	// This sleep is necessary to consistently produce different timestamps because the time field in managedFields has
	// 1 second granularity and if both apply requests happen during the same second, this test would flake.
	time.Sleep(1 * time.Second)

	createdObject, err := client.CoreV1().RESTClient().Get().Namespace("default").Resource(podsResource).Name(podName).Do(context.TODO()).Get()
	if err != nil {
		t.Fatalf("Failed to retrieve created object: %v", err)
	}

	createdAccessor, err := meta.Accessor(createdObject)
	if err != nil {
		t.Fatalf("Failed to get meta accessor for created object: %v", err)
	}

	createdBytes, err := json.MarshalIndent(createdObject, "\t", "\t")
	if err != nil {
		t.Fatalf("Failed to marshal created object: %v", err)
	}

	podBytesNoEmptyDir := []byte(`{
		"apiVersion": "v1",
		"kind": "Pod",
		"metadata": {
			"name": "` + podName + `"
		},
		"spec": {
			"containers": [{
				"name":  "test-container-a",
				"image": "test-image-one",
				"volumeMounts": [{
					"mountPath": "/cache",
					"name": "cache-volume"
				}],
			}],
			"volumes": [{
				"name": "cache-volume"
			}]
		}
	}`)

	// Test that an apply with no emptyDir is recognized as distinct from an empty marker struct emptyDir.
	_, err = client.CoreV1().RESTClient().Patch(types.ApplyPatchType).
		Namespace("default").
		Param("fieldManager", "apply_test").
		Resource(podsResource).
		Name(podName).
		Body(podBytesNoEmptyDir).
		Do(context.TODO()).
		Get()
	if err != nil {
		t.Fatalf("Failed to create object: %v", err)
	}

	updatedObject, err := client.CoreV1().RESTClient().Get().Namespace("default").Resource(podsResource).Name(podName).Do(context.TODO()).Get()
	if err != nil {
		t.Fatalf("Failed to retrieve updated object: %v", err)
	}

	updatedAccessor, err := meta.Accessor(updatedObject)
	if err != nil {
		t.Fatalf("Failed to get meta accessor for updated object: %v", err)
	}

	updatedBytes, err := json.MarshalIndent(updatedObject, "\t", "\t")
	if err != nil {
		t.Fatalf("Failed to marshal updated object: %v", err)
	}

	if createdAccessor.GetResourceVersion() == updatedAccessor.GetResourceVersion() {
		t.Fatalf("Expected different resource version to be %v but got: %v\nold object:\n%v\nnew object:\n%v",
			createdAccessor.GetResourceVersion(),
			updatedAccessor.GetResourceVersion(),
			string(createdBytes),
			string(updatedBytes),
		)
	}
}

func getRV(obj runtime.Object) (string, error) {
	acc, err := meta.Accessor(obj)
	if err != nil {
		return "", err
	}
	return acc.GetResourceVersion(), nil
}

func TestNoopChangeCreationTime(t *testing.T) {
	client, closeFn := setup(t)
	defer closeFn()

	ssBytes := []byte(`{
		"apiVersion": "v1",
		"kind": "ConfigMap",
		"metadata": {
			"name": "myconfig",
			"creationTimestamp": null,
			"resourceVersion": null
		},
		"data": {
			"key": "value"
		}
	}`)

	obj, err := client.CoreV1().RESTClient().Patch(types.ApplyPatchType).
		Namespace("default").
		Param("fieldManager", "apply_test").
		Resource("configmaps").
		Name("myconfig").
		Body(ssBytes).
		Do(context.TODO()).
		Get()
	if err != nil {
		t.Fatalf("Failed to create object: %v", err)
	}

	require.NoError(t, err)
	// Sleep for one second to make sure that the times of each update operation is different.
	time.Sleep(1200 * time.Millisecond)

	newObj, err := client.CoreV1().RESTClient().Patch(types.ApplyPatchType).
		Namespace("default").
		Param("fieldManager", "apply_test").
		Resource("configmaps").
		Name("myconfig").
		Body(ssBytes).
		Do(context.TODO()).
		Get()
	if err != nil {
		t.Fatalf("Failed to create object: %v", err)
	}

	require.NoError(t, err)
	require.Equal(t, obj, newObj)
}

// TestNoSemanticUpdateAppleSameResourceVersion makes sure that APPLY requests which makes no semantic changes
// will not change the resource version (no write to etcd is done)
//
// Some of the non-semantic changes are:
// - Applying an atomic struct that removes a default
// - Changing Quantity or other fields that are normalized
func TestNoSemanticUpdateApplySameResourceVersion(t *testing.T) {
	client, closeFn := setup(t)
	defer closeFn()

	ssBytes := []byte(`{
		"apiVersion": "apps/v1",
		"kind": "StatefulSet",
		"metadata": {
			"name": "nginx",
			"labels": {"app": "nginx"}
		},
		"spec": {
			"serviceName": "nginx",
			"selector": { "matchLabels": {"app": "nginx"}},
			"template": {
				"metadata": {
					"labels": {"app": "nginx"}
				},
				"spec": {
					"containers": [{
						"name":  "nginx",
						"image": "nginx",
						"resources": {
							"limits": {"memory": "2048Mi"}
						}
					}]
				}
			},
			"volumeClaimTemplates": [{
				"metadata": {"name": "nginx"},
				"spec": {
					"accessModes": ["ReadWriteOnce"],
					"resources": {"requests": {"storage": "1Gi"}}
				}
			}]
		}
	}`)

	obj, err := client.AppsV1().RESTClient().Patch(types.ApplyPatchType).
		Namespace("default").
		Param("fieldManager", "apply_test").
		Resource("statefulsets").
		Name("nginx").
		Body(ssBytes).
		Do(context.TODO()).
		Get()
	if err != nil {
		t.Fatalf("Failed to create object: %v", err)
	}

	rvCreated, err := getRV(obj)
	if err != nil {
		t.Fatalf("Failed to get RV: %v", err)
	}

	// Sleep for one second to make sure that the times of each update operation is different.
	time.Sleep(1200 * time.Millisecond)

	obj, err = client.AppsV1().RESTClient().Patch(types.ApplyPatchType).
		Namespace("default").
		Param("fieldManager", "apply_test").
		Resource("statefulsets").
		Name("nginx").
		Body(ssBytes).
		Do(context.TODO()).
		Get()
	if err != nil {
		t.Fatalf("Failed to create object: %v", err)
	}
	rvApplied, err := getRV(obj)
	if err != nil {
		t.Fatalf("Failed to get RV: %v", err)
	}
	if rvApplied != rvCreated {
		t.Fatal("ResourceVersion changed after apply")
	}
}

// TestNoSemanticUpdateAppleSameResourceVersion makes sure that PUT requests which makes no semantic changes
// will not change the resource version (no write to etcd is done)
//
// Some of the non-semantic changes are:
// - Applying an atomic struct that removes a default
// - Changing Quantity or other fields that are normalized
func TestNoSemanticUpdatePutSameResourceVersion(t *testing.T) {
	client, closeFn := setup(t)
	defer closeFn()

	ssBytes := []byte(`{
		"apiVersion": "apps/v1",
		"kind": "StatefulSet",
		"metadata": {
			"name": "nginx",
			"labels": {"app": "nginx"}
		},
		"spec": {
			"serviceName": "nginx",
			"selector": { "matchLabels": {"app": "nginx"}},
			"template": {
				"metadata": {
					"labels": {"app": "nginx"}
				},
				"spec": {
					"containers": [{
						"name":  "nginx",
						"image": "nginx",
						"resources": {
							"limits": {"memory": "2048Mi"}
						}
					}]
				}
			},
			"volumeClaimTemplates": [{
				"metadata": {"name": "nginx"},
				"spec": {
					"accessModes": ["ReadWriteOnce"],
					"resources": { "requests": { "storage": "1Gi"}}
				}
			}]
		}
	}`)

	obj, err := client.AppsV1().RESTClient().Post().
		Namespace("default").
		Param("fieldManager", "apply_test").
		Resource("statefulsets").
		Body(ssBytes).
		Do(context.TODO()).
		Get()
	if err != nil {
		t.Fatalf("Failed to create object: %v", err)
	}

	rvCreated, err := getRV(obj)
	if err != nil {
		t.Fatalf("Failed to get RV: %v", err)
	}

	// Sleep for one second to make sure that the times of each update operation is different.
	time.Sleep(1200 * time.Millisecond)

	obj, err = client.AppsV1().RESTClient().Put().
		Namespace("default").
		Param("fieldManager", "apply_test").
		Resource("statefulsets").
		Name("nginx").
		Body(ssBytes).
		Do(context.TODO()).
		Get()
	if err != nil {
		t.Fatalf("Failed to create object: %v", err)
	}
	rvApplied, err := getRV(obj)
	if err != nil {
		t.Fatalf("Failed to get RV: %v", err)
	}
	if rvApplied != rvCreated {
		t.Fatal("ResourceVersion changed after similar PUT")
	}
}

// TestCreateOnApplyFailsWithUID makes sure that PATCH requests with the apply content type
// will not create the object if it doesn't already exist and it specifies a UID
func TestCreateOnApplyFailsWithUID(t *testing.T) {
	client, closeFn := setup(t)
	defer closeFn()

	_, err := client.CoreV1().RESTClient().Patch(types.ApplyPatchType).
		Namespace("default").
		Resource("pods").
		Name("test-pod-uid").
		Param("fieldManager", "apply_test").
		Body([]byte(`{
			"apiVersion": "v1",
			"kind": "Pod",
			"metadata": {
				"name": "test-pod-uid",
				"uid":  "88e00824-7f0e-11e8-94a1-c8d3ffb15800"
			},
			"spec": {
				"containers": [{
					"name":  "test-container",
					"image": "test-image"
				}]
			}
		}`)).
		Do(context.TODO()).
		Get()
	if !apierrors.IsConflict(err) {
		t.Fatalf("Expected conflict error but got: %v", err)
	}
}

func TestApplyUpdateApplyConflictForced(t *testing.T) {
	client, closeFn := setup(t)
	defer closeFn()

	obj := []byte(`{
		"apiVersion": "apps/v1",
		"kind": "Deployment",
		"metadata": {
			"name": "deployment",
			"labels": {"app": "nginx"}
		},
		"spec": {
			"replicas": 3,
			"selector": {
				"matchLabels": {
					 "app": "nginx"
				}
			},
			"template": {
				"metadata": {
					"labels": {
						"app": "nginx"
					}
				},
				"spec": {
					"containers": [{
						"name":  "nginx",
						"image": "nginx:latest"
					}]
				}
			}
		}
	}`)

	_, err := client.CoreV1().RESTClient().Patch(types.ApplyPatchType).
		AbsPath("/apis/apps/v1").
		Namespace("default").
		Resource("deployments").
		Name("deployment").
		Param("fieldManager", "apply_test").
		Body(obj).Do(context.TODO()).Get()
	if err != nil {
		t.Fatalf("Failed to create object using Apply patch: %v", err)
	}

	_, err = client.CoreV1().RESTClient().Patch(types.MergePatchType).
		AbsPath("/apis/apps/v1").
		Namespace("default").
		Resource("deployments").
		Name("deployment").
		Body([]byte(`{"spec":{"replicas": 5}}`)).Do(context.TODO()).Get()
	if err != nil {
		t.Fatalf("Failed to patch object: %v", err)
	}

	_, err = client.CoreV1().RESTClient().Patch(types.ApplyPatchType).
		AbsPath("/apis/apps/v1").
		Namespace("default").
		Resource("deployments").
		Name("deployment").
		Param("fieldManager", "apply_test").
		Body([]byte(obj)).Do(context.TODO()).Get()
	if err == nil {
		t.Fatalf("Expecting to get conflicts when applying object")
	}
	status, ok := err.(*apierrors.StatusError)
	if !ok {
		t.Fatalf("Expecting to get conflicts as API error")
	}
	if len(status.Status().Details.Causes) < 1 {
		t.Fatalf("Expecting to get at least one conflict when applying object, got: %v", status.Status().Details.Causes)
	}

	_, err = client.CoreV1().RESTClient().Patch(types.ApplyPatchType).
		AbsPath("/apis/apps/v1").
		Namespace("default").
		Resource("deployments").
		Name("deployment").
		Param("force", "true").
		Param("fieldManager", "apply_test").
		Body([]byte(obj)).Do(context.TODO()).Get()
	if err != nil {
		t.Fatalf("Failed to apply object with force: %v", err)
	}
}

// TestApplyGroupsManySeparateUpdates tests that when many different managers update the same object,
// the number of managedFields entries will only grow to a certain size.
func TestApplyGroupsManySeparateUpdates(t *testing.T) {
	client, closeFn := setup(t)
	defer closeFn()

	obj := []byte(`{
		"apiVersion": "admissionregistration.k8s.io/v1",
		"kind": "ValidatingWebhookConfiguration",
		"metadata": {
			"name": "webhook",
			"labels": {"applier":"true"},
		},
	}`)

	object, err := client.CoreV1().RESTClient().Patch(types.ApplyPatchType).
		AbsPath("/apis/admissionregistration.k8s.io/v1").
		Resource("validatingwebhookconfigurations").
		Name("webhook").
		Param("fieldManager", "apply_test").
		Body(obj).Do(context.TODO()).Get()
	if err != nil {
		t.Fatalf("Failed to create object using Apply patch: %v", err)
	}

	for i := 0; i < 20; i++ {
		unique := fmt.Sprintf("updater%v", i)
		object, err = client.CoreV1().RESTClient().Patch(types.MergePatchType).
			AbsPath("/apis/admissionregistration.k8s.io/v1").
			Resource("validatingwebhookconfigurations").
			Name("webhook").
			Param("fieldManager", unique).
			Body([]byte(`{"metadata":{"labels":{"` + unique + `":"new"}}}`)).Do(context.TODO()).Get()
		if err != nil {
			t.Fatalf("Failed to patch object: %v", err)
		}
	}

	accessor, err := meta.Accessor(object)
	if err != nil {
		t.Fatalf("Failed to get meta accessor: %v", err)
	}

	// Expect 11 entries, because the cap for update entries is 10, and 1 apply entry
	if actual, expected := len(accessor.GetManagedFields()), 11; actual != expected {
		if b, err := json.MarshalIndent(object, "\t", "\t"); err == nil {
			t.Fatalf("Object expected to contain %v entries in managedFields, but got %v:\n%v", expected, actual, string(b))
		} else {
			t.Fatalf("Object expected to contain %v entries in managedFields, but got %v: error marshalling object: %v", expected, actual, err)
		}
	}

	// Expect the first entry to have the manager name "apply_test"
	if actual, expected := accessor.GetManagedFields()[0].Manager, "apply_test"; actual != expected {
		t.Fatalf("Expected first manager to be named %v but got %v", expected, actual)
	}

	// Expect the second entry to have the manager name "ancient-changes"
	if actual, expected := accessor.GetManagedFields()[1].Manager, "ancient-changes"; actual != expected {
		t.Fatalf("Expected first manager to be named %v but got %v", expected, actual)
	}
}

// TestCreateVeryLargeObject tests that a very large object can be created without exceeding the size limit due to managedFields
func TestCreateVeryLargeObject(t *testing.T) {
	client, closeFn := setup(t)
	defer closeFn()

	cfg := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "large-create-test-cm",
			Namespace: "default",
		},
		Data: map[string]string{},
	}

	for i := 0; i < 9999; i++ {
		unique := fmt.Sprintf("this-key-is-very-long-so-as-to-create-a-very-large-serialized-fieldset-%v", i)
		cfg.Data[unique] = "A"
	}

	// Should be able to create an object near the object size limit.
	if _, err := client.CoreV1().ConfigMaps(cfg.Namespace).Create(context.TODO(), cfg, metav1.CreateOptions{}); err != nil {
		t.Errorf("unable to create large test configMap: %v", err)
	}

	// Applying to the same object should cause managedFields to go over the object size limit, and fail.
	_, err := client.CoreV1().RESTClient().Patch(types.ApplyPatchType).
		Namespace(cfg.Namespace).
		Resource("configmaps").
		Name(cfg.Name).
		Param("fieldManager", "apply_test").
		Body([]byte(`{
			"apiVersion": "v1",
			"kind": "ConfigMap",
			"metadata": {
				"name": "large-create-test-cm",
				"namespace": "default",
			}
		}`)).
		Do(context.TODO()).
		Get()
	if err == nil {
		t.Fatalf("expected to fail to update object using Apply patch, but succeeded")
	}
}

// TestUpdateVeryLargeObject tests that a small object can be updated to be very large without exceeding the size limit due to managedFields
func TestUpdateVeryLargeObject(t *testing.T) {
	client, closeFn := setup(t)
	defer closeFn()

	cfg := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "large-update-test-cm",
			Namespace: "default",
		},
		Data: map[string]string{"k": "v"},
	}

	// Create a small config map.
	cfg, err := client.CoreV1().ConfigMaps(cfg.Namespace).Create(context.TODO(), cfg, metav1.CreateOptions{})
	if err != nil {
		t.Errorf("unable to create configMap: %v", err)
	}

	// Should be able to update a small object to be near the object size limit.
	var updateErr error
	pollErr := wait.PollImmediate(100*time.Millisecond, 30*time.Second, func() (bool, error) {
		updateCfg, err := client.CoreV1().ConfigMaps(cfg.Namespace).Get(context.TODO(), cfg.Name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}

		// Apply the large update, then attempt to push it to the apiserver.
		for i := 0; i < 9999; i++ {
			unique := fmt.Sprintf("this-key-is-very-long-so-as-to-create-a-very-large-serialized-fieldset-%v", i)
			updateCfg.Data[unique] = "A"
		}

		if _, err = client.CoreV1().ConfigMaps(cfg.Namespace).Update(context.TODO(), updateCfg, metav1.UpdateOptions{}); err == nil {
			return true, nil
		}
		updateErr = err
		return false, nil
	})
	if pollErr == wait.ErrWaitTimeout {
		t.Errorf("unable to update configMap: %v", updateErr)
	}

	// Applying to the same object should cause managedFields to go over the object size limit, and fail.
	_, err = client.CoreV1().RESTClient().Patch(types.ApplyPatchType).
		Namespace(cfg.Namespace).
		Resource("configmaps").
		Name(cfg.Name).
		Param("fieldManager", "apply_test").
		Body([]byte(`{
			"apiVersion": "v1",
			"kind": "ConfigMap",
			"metadata": {
				"name": "large-update-test-cm",
				"namespace": "default",
			}
		}`)).
		Do(context.TODO()).
		Get()
	if err == nil {
		t.Fatalf("expected to fail to update object using Apply patch, but succeeded")
	}
}

// TestPatchVeryLargeObject tests that a small object can be patched to be very large without exceeding the size limit due to managedFields
func TestPatchVeryLargeObject(t *testing.T) {
	client, closeFn := setup(t)
	defer closeFn()

	cfg := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "large-patch-test-cm",
			Namespace: "default",
		},
		Data: map[string]string{"k": "v"},
	}

	// Create a small config map.
	if _, err := client.CoreV1().ConfigMaps(cfg.Namespace).Create(context.TODO(), cfg, metav1.CreateOptions{}); err != nil {
		t.Errorf("unable to create configMap: %v", err)
	}

	patchString := `{"data":{"k":"v"`
	for i := 0; i < 9999; i++ {
		unique := fmt.Sprintf("this-key-is-very-long-so-as-to-create-a-very-large-serialized-fieldset-%v", i)
		patchString = fmt.Sprintf("%s,%q:%q", patchString, unique, "A")
	}
	patchString = fmt.Sprintf("%s}}", patchString)

	// Should be able to update a small object to be near the object size limit.
	_, err := client.CoreV1().RESTClient().Patch(types.MergePatchType).
		AbsPath("/api/v1").
		Namespace(cfg.Namespace).
		Resource("configmaps").
		Name(cfg.Name).
		Body([]byte(patchString)).Do(context.TODO()).Get()
	if err != nil {
		t.Errorf("unable to patch configMap: %v", err)
	}

	// Applying to the same object should cause managedFields to go over the object size limit, and fail.
	_, err = client.CoreV1().RESTClient().Patch(types.ApplyYAMLPatchType).
		Namespace("default").
		Resource("configmaps").
		Name("large-patch-test-cm").
		Param("fieldManager", "apply_test").
		Body([]byte(`{
			"apiVersion": "v1",
			"kind": "ConfigMap",
			"metadata": {
				"name": "large-patch-test-cm",
				"namespace": "default",
			}
		}`)).
		Do(context.TODO()).
		Get()
	if err == nil {
		t.Fatalf("expected to fail to update object using Apply patch, but succeeded")
	}
}

// TestPatchVeryLargeObjectCBORApply mirrors TestPatchVeryLargeObject using the +cbor structured
// syntax suffix for application/apply-patch and with CBOR enabled.
func TestPatchVeryLargeObjectCBORApply(t *testing.T) {
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.CBORServingAndStorage, true)
	clientfeaturestesting.SetFeatureDuringTest(t, clientfeatures.ClientsAllowCBOR, true)

	client, closeFn := setup(t)
	defer closeFn()

	cfg := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "large-patch-test-cm",
			Namespace: "default",
		},
		Data: map[string]string{"k": "v"},
	}

	// Create a small config map.
	if _, err := client.CoreV1().ConfigMaps(cfg.Namespace).Create(context.TODO(), cfg, metav1.CreateOptions{}); err != nil {
		t.Errorf("unable to create configMap: %v", err)
	}

	patchString := `{"data":{"k":"v"`
	for i := 0; i < 9999; i++ {
		unique := fmt.Sprintf("this-key-is-very-long-so-as-to-create-a-very-large-serialized-fieldset-%v", i)
		patchString = fmt.Sprintf("%s,%q:%q", patchString, unique, "A")
	}
	patchString = fmt.Sprintf("%s}}", patchString)

	// Should be able to update a small object to be near the object size limit.
	_, err := client.CoreV1().RESTClient().Patch(types.MergePatchType).
		AbsPath("/api/v1").
		Namespace(cfg.Namespace).
		Resource("configmaps").
		Name(cfg.Name).
		Body([]byte(patchString)).Do(context.TODO()).Get()
	if err != nil {
		t.Errorf("unable to patch configMap: %v", err)
	}

	// Applying to the same object should cause managedFields to go over the object size limit, and fail.
	_, err = client.CoreV1().RESTClient().Patch(types.ApplyYAMLPatchType).
		Namespace("default").
		Resource("configmaps").
		Name("large-patch-test-cm").
		Param("fieldManager", "apply_test").
		Body([]byte(`{
			"apiVersion": "v1",
			"kind": "ConfigMap",
			"metadata": {
				"name": "large-patch-test-cm",
				"namespace": "default",
			}
		}`)).
		Do(context.TODO()).
		Get()
	if err == nil {
		t.Fatalf("expected to fail to update object using Apply patch, but succeeded")
	}

	_, err = client.CoreV1().RESTClient().Patch(types.ApplyCBORPatchType).
		Namespace("default").
		Resource("configmaps").
		Name("large-patch-test-cm").
		Param("fieldManager", "apply_test").
		Body([]byte("\xa3\x4aapiVersion\x42v1\x44kind\x49ConfigMap\x48metadata\xa2\x44name\x53large-patch-test-cm\x49namespace\x47default")).
		Do(context.TODO()).
		Get()
	if err == nil {
		t.Fatalf("expected to fail to update object using Apply patch (cbor), but succeeded")
	}
}

// TestApplyManagedFields makes sure that managedFields api does not change
func TestApplyManagedFields(t *testing.T) {
	client, closeFn := setup(t)
	defer closeFn()

	_, err := client.CoreV1().RESTClient().Patch(types.ApplyPatchType).
		Namespace("default").
		Resource("configmaps").
		Name("test-cm").
		Param("fieldManager", "apply_test").
		Body([]byte(`{
			"apiVersion": "v1",
			"kind": "ConfigMap",
			"metadata": {
				"name": "test-cm",
				"namespace": "default",
				"labels": {
					"test-label": "test"
				}
			},
			"data": {
				"key": "value"
			}
		}`)).
		Do(context.TODO()).
		Get()
	if err != nil {
		t.Fatalf("Failed to create object using Apply patch: %v", err)
	}

	_, err = client.CoreV1().RESTClient().Patch(types.MergePatchType).
		Namespace("default").
		Resource("configmaps").
		Name("test-cm").
		Param("fieldManager", "updater").
		Body([]byte(`{"data":{"new-key": "value"}}`)).Do(context.TODO()).Get()
	if err != nil {
		t.Fatalf("Failed to patch object: %v", err)
	}

	// Sleep for one second to make sure that the times of each update operation is different.
	// This will let us check that update entries with the same manager name are grouped together,
	// and that the most recent update time is recorded in the grouped entry.
	time.Sleep(1 * time.Second)

	_, err = client.CoreV1().RESTClient().Patch(types.MergePatchType).
		Namespace("default").
		Resource("configmaps").
		Name("test-cm").
		Param("fieldManager", "updater").
		Body([]byte(`{"data":{"key": "new value"}}`)).Do(context.TODO()).Get()
	if err != nil {
		t.Fatalf("Failed to patch object: %v", err)
	}

	object, err := client.CoreV1().RESTClient().Get().Namespace("default").Resource("configmaps").Name("test-cm").Do(context.TODO()).Get()
	if err != nil {
		t.Fatalf("Failed to retrieve object: %v", err)
	}

	accessor, err := meta.Accessor(object)
	if err != nil {
		t.Fatalf("Failed to get meta accessor: %v", err)
	}

	actual, err := json.MarshalIndent(object, "\t", "\t")
	if err != nil {
		t.Fatalf("Failed to marshal object: %v", err)
	}

	expected := []byte(`{
		"metadata": {
			"name": "test-cm",
			"namespace": "default",
			"uid": "` + string(accessor.GetUID()) + `",
			"resourceVersion": "` + accessor.GetResourceVersion() + `",
			"creationTimestamp": "` + accessor.GetCreationTimestamp().UTC().Format(time.RFC3339) + `",
			"labels": {
				"test-label": "test"
			},
			"managedFields": [
				{
					"manager": "apply_test",
					"operation": "Apply",
					"apiVersion": "v1",
					"time": "` + accessor.GetManagedFields()[0].Time.UTC().Format(time.RFC3339) + `",
					"fieldsType": "FieldsV1",
					"fieldsV1": {
						"f:metadata": {
							"f:labels": {
								"f:test-label": {}
							}
						}
					}
				},
				{
					"manager": "updater",
					"operation": "Update",
					"apiVersion": "v1",
					"time": "` + accessor.GetManagedFields()[1].Time.UTC().Format(time.RFC3339) + `",
					"fieldsType": "FieldsV1",
					"fieldsV1": {
						"f:data": {
							"f:key": {},
							"f:new-key": {}
						}
					}
				}
			]
		},
		"data": {
			"key": "new value",
			"new-key": "value"
		}
	}`)

	if string(expected) != string(actual) {
		t.Fatalf("Expected:\n%v\nGot:\n%v", string(expected), string(actual))
	}

	if accessor.GetManagedFields()[0].Time.UTC().Format(time.RFC3339) == accessor.GetManagedFields()[1].Time.UTC().Format(time.RFC3339) {
		t.Fatalf("Expected times to be different but got:\n%v", string(actual))
	}
}

// TestApplyRemovesEmptyManagedFields there are no empty managers in managedFields
func TestApplyRemovesEmptyManagedFields(t *testing.T) {
	client, closeFn := setup(t)
	defer closeFn()

	obj := []byte(`{
		"apiVersion": "v1",
		"kind": "ConfigMap",
		"metadata": {
			"name": "test-cm",
			"namespace": "default"
		}
	}`)

	_, err := client.CoreV1().RESTClient().Patch(types.ApplyPatchType).
		Namespace("default").
		Resource("configmaps").
		Name("test-cm").
		Param("fieldManager", "apply_test").
		Body(obj).
		Do(context.TODO()).
		Get()
	if err != nil {
		t.Fatalf("Failed to create object using Apply patch: %v", err)
	}

	_, err = client.CoreV1().RESTClient().Patch(types.ApplyPatchType).
		Namespace("default").
		Resource("configmaps").
		Name("test-cm").
		Param("fieldManager", "apply_test").
		Body(obj).Do(context.TODO()).Get()
	if err != nil {
		t.Fatalf("Failed to patch object: %v", err)
	}

	object, err := client.CoreV1().RESTClient().Get().Namespace("default").Resource("configmaps").Name("test-cm").Do(context.TODO()).Get()
	if err != nil {
		t.Fatalf("Failed to retrieve object: %v", err)
	}

	accessor, err := meta.Accessor(object)
	if err != nil {
		t.Fatalf("Failed to get meta accessor: %v", err)
	}

	if managed := accessor.GetManagedFields(); managed != nil {
		t.Fatalf("Object contains unexpected managedFields: %v", managed)
	}
}

func TestApplyRequiresFieldManager(t *testing.T) {
	client, closeFn := setup(t)
	defer closeFn()

	obj := []byte(`{
		"apiVersion": "v1",
		"kind": "ConfigMap",
		"metadata": {
			"name": "test-cm",
			"namespace": "default"
		}
	}`)

	_, err := client.CoreV1().RESTClient().Patch(types.ApplyPatchType).
		Namespace("default").
		Resource("configmaps").
		Name("test-cm").
		Body(obj).
		Do(context.TODO()).
		Get()
	if err == nil {
		t.Fatalf("Apply should fail to create without fieldManager")
	}

	_, err = client.CoreV1().RESTClient().Patch(types.ApplyPatchType).
		Namespace("default").
		Resource("configmaps").
		Name("test-cm").
		Param("fieldManager", "apply_test").
		Body(obj).
		Do(context.TODO()).
		Get()
	if err != nil {
		t.Fatalf("Apply failed to create with fieldManager: %v", err)
	}
}

// TestApplyRemoveContainerPort removes a container port from a deployment
func TestApplyRemoveContainerPort(t *testing.T) {
	client, closeFn := setup(t)
	defer closeFn()

	obj := []byte(`{
		"apiVersion": "apps/v1",
		"kind": "Deployment",
		"metadata": {
			"name": "deployment",
			"labels": {"app": "nginx"}
		},
		"spec": {
			"replicas": 3,
			"selector": {
				"matchLabels": {
					"app": "nginx"
				}
			},
			"template": {
				"metadata": {
					"labels": {
						"app": "nginx"
					}
				},
				"spec": {
					"containers": [{
						"name":  "nginx",
						"image": "nginx:latest",
						"ports": [{
							"containerPort": 80,
							"protocol": "TCP"
						}]
					}]
				}
			}
		}
	}`)

	_, err := client.CoreV1().RESTClient().Patch(types.ApplyPatchType).
		AbsPath("/apis/apps/v1").
		Namespace("default").
		Resource("deployments").
		Name("deployment").
		Param("fieldManager", "apply_test").
		Body(obj).Do(context.TODO()).Get()
	if err != nil {
		t.Fatalf("Failed to create object using Apply patch: %v", err)
	}

	obj = []byte(`{
		"apiVersion": "apps/v1",
		"kind": "Deployment",
		"metadata": {
			"name": "deployment",
			"labels": {"app": "nginx"}
		},
		"spec": {
			"replicas": 3,
			"selector": {
				"matchLabels": {
					"app": "nginx"
				}
			},
			"template": {
				"metadata": {
					"labels": {
						"app": "nginx"
					}
				},
				"spec": {
					"containers": [{
						"name":  "nginx",
						"image": "nginx:latest"
					}]
				}
			}
		}
	}`)

	_, err = client.CoreV1().RESTClient().Patch(types.ApplyPatchType).
		AbsPath("/apis/apps/v1").
		Namespace("default").
		Resource("deployments").
		Name("deployment").
		Param("fieldManager", "apply_test").
		Body(obj).Do(context.TODO()).Get()
	if err != nil {
		t.Fatalf("Failed to remove container port using Apply patch: %v", err)
	}

	deployment, err := client.AppsV1().Deployments("default").Get(context.TODO(), "deployment", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Failed to retrieve object: %v", err)
	}

	if len(deployment.Spec.Template.Spec.Containers[0].Ports) > 0 {
		t.Fatalf("Expected no container ports but got: %v, object: \n%#v", deployment.Spec.Template.Spec.Containers[0].Ports, deployment)
	}
}

// TestApplyFailsWithVersionMismatch ensures that a version mismatch between the
// patch object and the live object will error
func TestApplyFailsWithVersionMismatch(t *testing.T) {
	client, closeFn := setup(t)
	defer closeFn()

	obj := []byte(`{
		"apiVersion": "apps/v1",
		"kind": "Deployment",
		"metadata": {
			"name": "deployment",
			"labels": {"app": "nginx"}
		},
		"spec": {
			"replicas": 3,
			"selector": {
				"matchLabels": {
					 "app": "nginx"
				}
			},
			"template": {
				"metadata": {
					"labels": {
						"app": "nginx"
					}
				},
				"spec": {
					"containers": [{
						"name":  "nginx",
						"image": "nginx:latest"
					}]
				}
			}
		}
	}`)

	_, err := client.CoreV1().RESTClient().Patch(types.ApplyPatchType).
		AbsPath("/apis/apps/v1").
		Namespace("default").
		Resource("deployments").
		Name("deployment").
		Param("fieldManager", "apply_test").
		Body(obj).Do(context.TODO()).Get()
	if err != nil {
		t.Fatalf("Failed to create object using Apply patch: %v", err)
	}

	obj = []byte(`{
		"apiVersion": "extensions/v1beta",
		"kind": "Deployment",
		"metadata": {
			"name": "deployment",
			"labels": {"app": "nginx"}
		},
		"spec": {
			"replicas": 100,
			"selector": {
				"matchLabels": {
					"app": "nginx"
				}
			},
			"template": {
				"metadata": {
					"labels": {
						"app": "nginx"
					}
				},
				"spec": {
					"containers": [{
						"name":  "nginx",
						"image": "nginx:latest"
					}]
				}
			}
		}
	}`)
	_, err = client.CoreV1().RESTClient().Patch(types.ApplyPatchType).
		AbsPath("/apis/apps/v1").
		Namespace("default").
		Resource("deployments").
		Name("deployment").
		Param("fieldManager", "apply_test").
		Body([]byte(obj)).Do(context.TODO()).Get()
	if err == nil {
		t.Fatalf("Expecting to get version mismatch when applying object")
	}
	status, ok := err.(*apierrors.StatusError)
	if !ok {
		t.Fatalf("Expecting to get version mismatch as API error")
	}
	if status.Status().Code != http.StatusBadRequest {
		t.Fatalf("expected status code to be %d but was %d", http.StatusBadRequest, status.Status().Code)
	}
}

// TestApplyConvertsManagedFieldsVersion checks that the apply
// converts the API group-version in the field manager
func TestApplyConvertsManagedFieldsVersion(t *testing.T) {
	client, closeFn := setup(t)
	defer closeFn()

	obj := []byte(`{
		"apiVersion": "apps/v1",
		"kind": "Deployment",
		"metadata": {
			"name": "deployment",
			"labels": {"app": "nginx"},
			"managedFields": [
				{
					"manager": "sidecar_controller",
					"operation": "Apply",
					"apiVersion": "extensions/v1beta1",
					"fieldsV1": {
						"f:metadata": {
							"f:labels": {
								"f:sidecar_version": {}
							}
						},
						"f:spec": {
							"f:template": {
								"f: spec": {
									"f:containers": {
										"k:{\"name\":\"sidecar\"}": {
											".": {},
											"f:image": {}
										}
									}
								}
							}
						}
					}
				}
			]
		},
		"spec": {
			"selector": {
				"matchLabels": {
					 "app": "nginx"
				}
			},
			"template": {
				"metadata": {
					"labels": {
						"app": "nginx"
					}
				},
				"spec": {
					"containers": [{
						"name":  "nginx",
						"image": "nginx:latest"
					}]
				}
			}
		}
	}`)

	_, err := client.CoreV1().RESTClient().Post().
		AbsPath("/apis/apps/v1").
		Namespace("default").
		Resource("deployments").
		Body(obj).Do(context.TODO()).Get()
	if err != nil {
		t.Fatalf("Failed to create object: %v", err)
	}

	obj = []byte(`{
		"apiVersion": "apps/v1",
		"kind": "Deployment",
		"metadata": {
			"name": "deployment",
			"labels": {"sidecar_version": "release"}
		},
		"spec": {
			"template": {
				"spec": {
					"containers": [{
						"name":  "sidecar",
						"image": "sidecar:latest"
					}]
				}
			}
		}
	}`)
	_, err = client.CoreV1().RESTClient().Patch(types.ApplyPatchType).
		AbsPath("/apis/apps/v1").
		Namespace("default").
		Resource("deployments").
		Name("deployment").
		Param("fieldManager", "sidecar_controller").
		Body([]byte(obj)).Do(context.TODO()).Get()
	if err != nil {
		t.Fatalf("Failed to apply object: %v", err)
	}

	object, err := client.AppsV1().Deployments("default").Get(context.TODO(), "deployment", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Failed to retrieve object: %v", err)
	}

	accessor, err := meta.Accessor(object)
	if err != nil {
		t.Fatalf("Failed to get meta accessor: %v", err)
	}

	managed := accessor.GetManagedFields()
	if len(managed) != 2 {
		t.Fatalf("Expected 2 field managers, but got managed fields: %v", managed)
	}

	var actual *metav1.ManagedFieldsEntry
	for i := range managed {
		entry := &managed[i]
		if entry.Manager == "sidecar_controller" && entry.APIVersion == "apps/v1" {
			actual = entry
		}
	}

	if actual == nil {
		t.Fatalf("Expected managed fields to contain entry with manager '%v' with converted api version '%v', but got managed fields:\n%v", "sidecar_controller", "apps/v1", managed)
	}

	expected := &metav1.ManagedFieldsEntry{
		Manager:    "sidecar_controller",
		Operation:  metav1.ManagedFieldsOperationApply,
		APIVersion: "apps/v1",
		Time:       actual.Time,
		FieldsType: "FieldsV1",
		FieldsV1: &metav1.FieldsV1{
			Raw: []byte(`{"f:metadata":{"f:labels":{"f:sidecar_version":{}}},"f:spec":{"f:template":{"f:spec":{"f:containers":{"k:{\"name\":\"sidecar\"}":{".":{},"f:image":{},"f:name":{}}}}}}}`),
		},
	}

	if !reflect.DeepEqual(actual, expected) {
		t.Fatalf("expected:\n%v\nbut got:\n%v", expected, actual)
	}
}

// TestClearManagedFieldsWithMergePatch verifies it's possible to clear the managedFields
func TestClearManagedFieldsWithMergePatch(t *testing.T) {
	client, closeFn := setup(t)
	defer closeFn()

	_, err := client.CoreV1().RESTClient().Patch(types.ApplyPatchType).
		Namespace("default").
		Resource("configmaps").
		Name("test-cm").
		Param("fieldManager", "apply_test").
		Body([]byte(`{
			"apiVersion": "v1",
			"kind": "ConfigMap",
			"metadata": {
				"name": "test-cm",
				"namespace": "default",
				"labels": {
					"test-label": "test"
				}
			},
			"data": {
				"key": "value"
			}
		}`)).
		Do(context.TODO()).
		Get()
	if err != nil {
		t.Fatalf("Failed to create object using Apply patch: %v", err)
	}

	_, err = client.CoreV1().RESTClient().Patch(types.MergePatchType).
		Namespace("default").
		Resource("configmaps").
		Name("test-cm").
		Body([]byte(`{"metadata":{"managedFields": [{}]}}`)).Do(context.TODO()).Get()
	if err != nil {
		t.Fatalf("Failed to patch object: %v", err)
	}

	object, err := client.CoreV1().RESTClient().Get().Namespace("default").Resource("configmaps").Name("test-cm").Do(context.TODO()).Get()
	if err != nil {
		t.Fatalf("Failed to retrieve object: %v", err)
	}

	accessor, err := meta.Accessor(object)
	if err != nil {
		t.Fatalf("Failed to get meta accessor: %v", err)
	}

	if managedFields := accessor.GetManagedFields(); len(managedFields) != 0 {
		t.Fatalf("Failed to clear managedFields, got: %v", managedFields)
	}
}

// TestClearManagedFieldsWithStrategicMergePatch verifies it's possible to clear the managedFields
func TestClearManagedFieldsWithStrategicMergePatch(t *testing.T) {
	client, closeFn := setup(t)
	defer closeFn()

	_, err := client.CoreV1().RESTClient().Patch(types.ApplyPatchType).
		Namespace("default").
		Resource("configmaps").
		Name("test-cm").
		Param("fieldManager", "apply_test").
		Body([]byte(`{
			"apiVersion": "v1",
			"kind": "ConfigMap",
			"metadata": {
				"name": "test-cm",
				"namespace": "default",
				"labels": {
					"test-label": "test"
				}
			},
			"data": {
				"key": "value"
			}
		}`)).
		Do(context.TODO()).
		Get()
	if err != nil {
		t.Fatalf("Failed to create object using Apply patch: %v", err)
	}

	_, err = client.CoreV1().RESTClient().Patch(types.StrategicMergePatchType).
		Namespace("default").
		Resource("configmaps").
		Name("test-cm").
		Body([]byte(`{"metadata":{"managedFields": [{}]}}`)).Do(context.TODO()).Get()
	if err != nil {
		t.Fatalf("Failed to patch object: %v", err)
	}

	object, err := client.CoreV1().RESTClient().Get().Namespace("default").Resource("configmaps").Name("test-cm").Do(context.TODO()).Get()
	if err != nil {
		t.Fatalf("Failed to retrieve object: %v", err)
	}

	accessor, err := meta.Accessor(object)
	if err != nil {
		t.Fatalf("Failed to get meta accessor: %v", err)
	}

	if managedFields := accessor.GetManagedFields(); len(managedFields) != 0 {
		t.Fatalf("Failed to clear managedFields, got: %v", managedFields)
	}

	if labels := accessor.GetLabels(); len(labels) < 1 {
		t.Fatalf("Expected other fields to stay untouched, got: %v", object)
	}
}

// TestClearManagedFieldsWithJSONPatch verifies it's possible to clear the managedFields
func TestClearManagedFieldsWithJSONPatch(t *testing.T) {
	client, closeFn := setup(t)
	defer closeFn()

	_, err := client.CoreV1().RESTClient().Patch(types.ApplyPatchType).
		Namespace("default").
		Resource("configmaps").
		Name("test-cm").
		Param("fieldManager", "apply_test").
		Body([]byte(`{
			"apiVersion": "v1",
			"kind": "ConfigMap",
			"metadata": {
				"name": "test-cm",
				"namespace": "default",
				"labels": {
					"test-label": "test"
				}
			},
			"data": {
				"key": "value"
			}
		}`)).
		Do(context.TODO()).
		Get()
	if err != nil {
		t.Fatalf("Failed to create object using Apply patch: %v", err)
	}

	_, err = client.CoreV1().RESTClient().Patch(types.JSONPatchType).
		Namespace("default").
		Resource("configmaps").
		Name("test-cm").
		Body([]byte(`[{"op": "replace", "path": "/metadata/managedFields", "value": [{}]}]`)).Do(context.TODO()).Get()
	if err != nil {
		t.Fatalf("Failed to patch object: %v", err)
	}

	object, err := client.CoreV1().RESTClient().Get().Namespace("default").Resource("configmaps").Name("test-cm").Do(context.TODO()).Get()
	if err != nil {
		t.Fatalf("Failed to retrieve object: %v", err)
	}

	accessor, err := meta.Accessor(object)
	if err != nil {
		t.Fatalf("Failed to get meta accessor: %v", err)
	}

	if managedFields := accessor.GetManagedFields(); len(managedFields) != 0 {
		t.Fatalf("Failed to clear managedFields, got: %v", managedFields)
	}
}

// TestClearManagedFieldsWithUpdate verifies it's possible to clear the managedFields
func TestClearManagedFieldsWithUpdate(t *testing.T) {
	client, closeFn := setup(t)
	defer closeFn()

	_, err := client.CoreV1().RESTClient().Patch(types.ApplyPatchType).
		Namespace("default").
		Resource("configmaps").
		Name("test-cm").
		Param("fieldManager", "apply_test").
		Body([]byte(`{
			"apiVersion": "v1",
			"kind": "ConfigMap",
			"metadata": {
				"name": "test-cm",
				"namespace": "default",
				"labels": {
					"test-label": "test"
				}
			},
			"data": {
				"key": "value"
			}
		}`)).
		Do(context.TODO()).
		Get()
	if err != nil {
		t.Fatalf("Failed to create object using Apply patch: %v", err)
	}

	_, err = client.CoreV1().RESTClient().Put().
		Namespace("default").
		Resource("configmaps").
		Name("test-cm").
		Body([]byte(`{
			"apiVersion": "v1",
			"kind": "ConfigMap",
			"metadata": {
				"name": "test-cm",
				"namespace": "default",
				"managedFields": [{}],
				"labels": {
					"test-label": "test"
				}
			},
			"data": {
				"key": "value"
			}
		}`)).Do(context.TODO()).Get()
	if err != nil {
		t.Fatalf("Failed to patch object: %v", err)
	}

	object, err := client.CoreV1().RESTClient().Get().Namespace("default").Resource("configmaps").Name("test-cm").Do(context.TODO()).Get()
	if err != nil {
		t.Fatalf("Failed to retrieve object: %v", err)
	}

	accessor, err := meta.Accessor(object)
	if err != nil {
		t.Fatalf("Failed to get meta accessor: %v", err)
	}

	if managedFields := accessor.GetManagedFields(); len(managedFields) != 0 {
		t.Fatalf("Failed to clear managedFields, got: %v", managedFields)
	}

	if labels := accessor.GetLabels(); len(labels) < 1 {
		t.Fatalf("Expected other fields to stay untouched, got: %v", object)
	}
}

// TestErrorsDontFail
func TestErrorsDontFail(t *testing.T) {
	client, closeFn := setup(t)
	defer closeFn()

	// Tries to create with a managed fields that has an empty `fieldsType`.
	_, err := client.CoreV1().RESTClient().Post().
		Namespace("default").
		Resource("configmaps").
		Param("fieldManager", "apply_test").
		Body([]byte(`{
			"apiVersion": "v1",
			"kind": "ConfigMap",
			"metadata": {
				"name": "test-cm",
				"namespace": "default",
				"managedFields": [{
					"manager": "apply_test",
					"operation": "Apply",
					"apiVersion": "v1",
					"time": "2019-07-08T09:31:18Z",
					"fieldsType": "",
					"fieldsV1": {}
				}],
				"labels": {
					"test-label": "test"
				}
			},
			"data": {
				"key": "value"
			}
		}`)).
		Do(context.TODO()).
		Get()
	if err != nil {
		t.Fatalf("Failed to create object with empty fieldsType: %v", err)
	}
}

func TestErrorsDontFailUpdate(t *testing.T) {
	client, closeFn := setup(t)
	defer closeFn()

	_, err := client.CoreV1().RESTClient().Post().
		Namespace("default").
		Resource("configmaps").
		Param("fieldManager", "apply_test").
		Body([]byte(`{
			"apiVersion": "v1",
			"kind": "ConfigMap",
			"metadata": {
				"name": "test-cm",
				"namespace": "default",
				"labels": {
					"test-label": "test"
				}
			},
			"data": {
				"key": "value"
			}
		}`)).
		Do(context.TODO()).
		Get()
	if err != nil {
		t.Fatalf("Failed to create object: %v", err)
	}

	_, err = client.CoreV1().RESTClient().Put().
		Namespace("default").
		Resource("configmaps").
		Name("test-cm").
		Param("fieldManager", "apply_test").
		Body([]byte(`{
			"apiVersion": "v1",
			"kind": "ConfigMap",
			"metadata": {
				"name": "test-cm",
				"namespace": "default",
				"managedFields": [{
					"manager": "apply_test",
					"operation": "Apply",
					"apiVersion": "v1",
					"time": "2019-07-08T09:31:18Z",
					"fieldsType": "",
					"fieldsV1": {}
				}],
				"labels": {
					"test-label": "test"
				}
			},
			"data": {
				"key": "value"
			}
		}`)).
		Do(context.TODO()).
		Get()
	if err != nil {
		t.Fatalf("Failed to update object with empty fieldsType: %v", err)
	}
}

func TestErrorsDontFailPatch(t *testing.T) {
	client, closeFn := setup(t)
	defer closeFn()

	_, err := client.CoreV1().RESTClient().Post().
		Namespace("default").
		Resource("configmaps").
		Param("fieldManager", "apply_test").
		Body([]byte(`{
			"apiVersion": "v1",
			"kind": "ConfigMap",
			"metadata": {
				"name": "test-cm",
				"namespace": "default",
				"labels": {
					"test-label": "test"
				}
			},
			"data": {
				"key": "value"
			}
		}`)).
		Do(context.TODO()).
		Get()
	if err != nil {
		t.Fatalf("Failed to create object: %v", err)
	}

	_, err = client.CoreV1().RESTClient().Patch(types.JSONPatchType).
		Namespace("default").
		Resource("configmaps").
		Name("test-cm").
		Param("fieldManager", "apply_test").
		Body([]byte(`[{"op": "replace", "path": "/metadata/managedFields", "value": [{
			"manager": "apply_test",
			"operation": "Apply",
			"apiVersion": "v1",
			"time": "2019-07-08T09:31:18Z",
			"fieldsType": "",
			"fieldsV1": {}
		}]}]`)).
		Do(context.TODO()).
		Get()
	if err != nil {
		t.Fatalf("Failed to patch object with empty FieldsType: %v", err)
	}
}

func TestApplyDoesNotChangeManagedFieldsViaSubresources(t *testing.T) {
	client, closeFn := setup(t)
	defer closeFn()

	podBytes := []byte(`{
		"apiVersion": "v1",
		"kind": "Pod",
		"metadata": {
			"name": "just-a-pod"
		},
		"spec": {
			"containers": [{
				"name":  "test-container-a",
				"image": "test-image-one"
			}]
		}
	}`)

	liveObj, err := client.CoreV1().RESTClient().
		Patch(types.ApplyPatchType).
		Namespace("default").
		Param("fieldManager", "apply_test").
		Resource("pods").
		Name("just-a-pod").
		Body(podBytes).
		Do(context.TODO()).
		Get()
	if err != nil {
		t.Fatalf("Failed to create object: %v", err)
	}

	updateBytes := []byte(`{
		"metadata": {
			"managedFields": [{
				"manager":"testing",
				"operation":"Update",
				"apiVersion":"v1",
				"fieldsType":"FieldsV1",
				"fieldsV1":{
					"f:spec":{
						"f:containers":{
							"k:{\"name\":\"testing\"}":{
								".":{},
								"f:image":{},
								"f:name":{}
							}
						}
					}
				}
			}]
		},
		"status": {
			"conditions": [{"type": "MyStatus", "status":"true"}]
		}
	}`)

	updateActor := "update_managedfields_test"
	newObj, err := client.CoreV1().RESTClient().
		Patch(types.MergePatchType).
		Namespace("default").
		Param("fieldManager", updateActor).
		Name("just-a-pod").
		Resource("pods").
		SubResource("status").
		Body(updateBytes).
		Do(context.TODO()).
		Get()

	if err != nil {
		t.Fatalf("Error updating subresource: %v ", err)
	}

	liveAccessor, err := meta.Accessor(liveObj)
	if err != nil {
		t.Fatalf("Failed to get meta accessor for live object: %v", err)
	}
	newAccessor, err := meta.Accessor(newObj)
	if err != nil {
		t.Fatalf("Failed to get meta accessor for new object: %v", err)
	}

	liveManagedFields := liveAccessor.GetManagedFields()
	if len(liveManagedFields) != 1 {
		t.Fatalf("Expected managedFields in the live object to have exactly one entry, got %d: %v", len(liveManagedFields), liveManagedFields)
	}

	newManagedFields := newAccessor.GetManagedFields()
	if len(newManagedFields) != 2 {
		t.Fatalf("Expected managedFields in the new object to have exactly two entries, got %d: %v", len(newManagedFields), newManagedFields)
	}

	if !reflect.DeepEqual(liveManagedFields[0], newManagedFields[0]) {
		t.Fatalf("managedFields updated via subresource:\n\nlive managedFields: %v\nnew managedFields: %v\n\n", liveManagedFields, newManagedFields)
	}

	if newManagedFields[1].Manager != updateActor {
		t.Fatalf(`Expected managerFields to have an entry with manager set to %q`, updateActor)
	}
}

// TestClearManagedFieldsWithUpdateEmptyList verifies it's possible to clear the managedFields by sending an empty list.
func TestClearManagedFieldsWithUpdateEmptyList(t *testing.T) {
	client, closeFn := setup(t)
	defer closeFn()

	_, err := client.CoreV1().RESTClient().Patch(types.ApplyPatchType).
		Namespace("default").
		Resource("configmaps").
		Name("test-cm").
		Param("fieldManager", "apply_test").
		Body([]byte(`{
			"apiVersion": "v1",
			"kind": "ConfigMap",
			"metadata": {
				"name": "test-cm",
				"namespace": "default",
				"labels": {
					"test-label": "test"
				}
			},
			"data": {
				"key": "value"
			}
		}`)).
		Do(context.TODO()).
		Get()
	if err != nil {
		t.Fatalf("Failed to create object using Apply patch: %v", err)
	}

	_, err = client.CoreV1().RESTClient().Put().
		Namespace("default").
		Resource("configmaps").
		Name("test-cm").
		Body([]byte(`{
			"apiVersion": "v1",
			"kind": "ConfigMap",
			"metadata": {
				"name": "test-cm",
				"namespace": "default",
				"managedFields": [],
				"labels": {
					"test-label": "test"
				}
			},
			"data": {
				"key": "value"
			}
		}`)).Do(context.TODO()).Get()
	if err != nil {
		t.Fatalf("Failed to patch object: %v", err)
	}

	_, err = client.CoreV1().RESTClient().Patch(types.MergePatchType).
		Namespace("default").
		Resource("configmaps").
		Name("test-cm").
		Body([]byte(`{"metadata":{"labels": { "test-label": "v1" }}}`)).Do(context.TODO()).Get()

	if err != nil {
		t.Fatalf("Failed to patch object: %v", err)
	}

	object, err := client.CoreV1().RESTClient().Get().Namespace("default").Resource("configmaps").Name("test-cm").Do(context.TODO()).Get()
	if err != nil {
		t.Fatalf("Failed to retrieve object: %v", err)
	}

	accessor, err := meta.Accessor(object)
	if err != nil {
		t.Fatalf("Failed to get meta accessor: %v", err)
	}

	if managedFields := accessor.GetManagedFields(); len(managedFields) != 0 {
		t.Fatalf("Failed to stop tracking managedFields, got: %v", managedFields)
	}

	if labels := accessor.GetLabels(); len(labels) < 1 {
		t.Fatalf("Expected other fields to stay untouched, got: %v", object)
	}
}

// TestApplyUnsetExclusivelyOwnedFields verifies that when owned fields are omitted from an applied
// configuration, and no other managers own the field, it is removed.
func TestApplyUnsetExclusivelyOwnedFields(t *testing.T) {
	client, closeFn := setup(t)
	defer closeFn()

	// spec.replicas is a optional, defaulted field
	// spec.template.spec.hostname is an optional, non-defaulted field
	apply := []byte(`{
		"apiVersion": "apps/v1",
		"kind": "Deployment",
		"metadata": {
			"name": "deployment-exclusive-unset",
			"labels": {"app": "nginx"}
		},
		"spec": {
			"replicas": 3,
			"selector": {
				"matchLabels": {
					"app": "nginx"
				}
			},
			"template": {
				"metadata": {
					"labels": {
						"app": "nginx"
					}
				},
				"spec": {
					"hostname": "test-hostname",
					"containers": [{
						"name":  "nginx",
						"image": "nginx:latest"
					}]
				}
			}
		}
	}`)

	_, err := client.CoreV1().RESTClient().Patch(types.ApplyPatchType).
		AbsPath("/apis/apps/v1").
		Namespace("default").
		Resource("deployments").
		Name("deployment-exclusive-unset").
		Param("fieldManager", "apply_test").
		Body(apply).
		Do(context.TODO()).
		Get()
	if err != nil {
		t.Fatalf("Failed to create object using Apply patch: %v", err)
	}

	// unset spec.replicas and spec.template.spec.hostname
	apply = []byte(`{
		"apiVersion": "apps/v1",
		"kind": "Deployment",
		"metadata": {
			"name": "deployment-exclusive-unset",
			"labels": {"app": "nginx"}
		},
		"spec": {
			"selector": {
				"matchLabels": {
					"app": "nginx"
				}
			},
			"template": {
				"metadata": {
					"labels": {
						"app": "nginx"
					}
				},
				"spec": {
					"containers": [{
						"name":  "nginx",
						"image": "nginx:latest"
					}]
				}
			}
		}
	}`)

	patched, err := client.CoreV1().RESTClient().Patch(types.ApplyPatchType).
		AbsPath("/apis/apps/v1").
		Namespace("default").
		Resource("deployments").
		Name("deployment-exclusive-unset").
		Param("fieldManager", "apply_test").
		Body(apply).
		Do(context.TODO()).
		Get()
	if err != nil {
		t.Fatalf("Failed to create object using Apply patch: %v", err)
	}

	deployment, ok := patched.(*appsv1.Deployment)
	if !ok {
		t.Fatalf("Failed to convert response object to Deployment")
	}
	if *deployment.Spec.Replicas != 1 {
		t.Errorf("Expected deployment.spec.replicas to be 1 (default value), but got %d", deployment.Spec.Replicas)
	}
	if len(deployment.Spec.Template.Spec.Hostname) != 0 {
		t.Errorf("Expected deployment.spec.template.spec.hostname to be unset, but got %s", deployment.Spec.Template.Spec.Hostname)
	}
}

// TestApplyUnsetSharedFields verifies that when owned fields are omitted from an applied
// configuration, but other managers also own the field, is it not removed.
func TestApplyUnsetSharedFields(t *testing.T) {
	client, closeFn := setup(t)
	defer closeFn()

	// spec.replicas is a optional, defaulted field
	// spec.template.spec.hostname is an optional, non-defaulted field
	apply := []byte(`{
		"apiVersion": "apps/v1",
		"kind": "Deployment",
		"metadata": {
			"name": "deployment-shared-unset",
			"labels": {"app": "nginx"}
		},
		"spec": {
			"replicas": 3,
			"selector": {
				"matchLabels": {
					"app": "nginx"
				}
			},
			"template": {
				"metadata": {
					"labels": {
						"app": "nginx"
					}
				},
				"spec": {
					"hostname": "test-hostname",
					"containers": [{
						"name":  "nginx",
						"image": "nginx:latest"
					}]
				}
			}
		}
	}`)

	for _, fieldManager := range []string{"shared_owner_1", "shared_owner_2"} {
		_, err := client.CoreV1().RESTClient().Patch(types.ApplyPatchType).
			AbsPath("/apis/apps/v1").
			Namespace("default").
			Resource("deployments").
			Name("deployment-shared-unset").
			Param("fieldManager", fieldManager).
			Body(apply).
			Do(context.TODO()).
			Get()
		if err != nil {
			t.Fatalf("Failed to create object using Apply patch: %v", err)
		}
	}

	// unset spec.replicas and spec.template.spec.hostname
	apply = []byte(`{
		"apiVersion": "apps/v1",
		"kind": "Deployment",
		"metadata": {
			"name": "deployment-shared-unset",
			"labels": {"app": "nginx"}
		},
		"spec": {
			"selector": {
				"matchLabels": {
					"app": "nginx"
				}
			},
			"template": {
				"metadata": {
					"labels": {
						"app": "nginx"
					}
				},
				"spec": {
					"containers": [{
						"name":  "nginx",
						"image": "nginx:latest"
					}]
				}
			}
		}
	}`)

	patched, err := client.CoreV1().RESTClient().Patch(types.ApplyPatchType).
		AbsPath("/apis/apps/v1").
		Namespace("default").
		Resource("deployments").
		Name("deployment-shared-unset").
		Param("fieldManager", "shared_owner_1").
		Body(apply).
		Do(context.TODO()).
		Get()
	if err != nil {
		t.Fatalf("Failed to create object using Apply patch: %v", err)
	}

	deployment, ok := patched.(*appsv1.Deployment)
	if !ok {
		t.Fatalf("Failed to convert response object to Deployment")
	}
	if *deployment.Spec.Replicas != 3 {
		t.Errorf("Expected deployment.spec.replicas to be 3, but got %d", deployment.Spec.Replicas)
	}
	if deployment.Spec.Template.Spec.Hostname != "test-hostname" {
		t.Errorf("Expected deployment.spec.template.spec.hostname to be \"test-hostname\", but got %s", deployment.Spec.Template.Spec.Hostname)
	}
}

// TestApplyCanTransferFieldOwnershipToController verifies that when an applier creates an
// object, a controller takes ownership of a field, and the applier
// then omits the field from its applied configuration, that the field value persists.
func TestApplyCanTransferFieldOwnershipToController(t *testing.T) {
	client, closeFn := setup(t)
	defer closeFn()

	// Applier creates a deployment with replicas set to 3
	apply := []byte(`{
		"apiVersion": "apps/v1",
		"kind": "Deployment",
		"metadata": {
			"name": "deployment-shared-map-item-removal",
			"labels": {"app": "nginx"}
		},
		"spec": {
			"replicas": 3,
			"selector": {
				"matchLabels": {
					"app": "nginx"
				}
			},
			"template": {
				"metadata": {
					"labels": {
						"app": "nginx"
					}
				},
				"spec": {
					"containers": [{
						"name":  "nginx",
						"image": "nginx:latest",
					}]
				}
			}
		}
	}`)

	appliedObj, err := client.CoreV1().RESTClient().Patch(types.ApplyPatchType).
		AbsPath("/apis/apps/v1").
		Namespace("default").
		Resource("deployments").
		Name("deployment-shared-map-item-removal").
		Param("fieldManager", "test_applier").
		Body(apply).
		Do(context.TODO()).
		Get()
	if err != nil {
		t.Fatalf("Failed to create object using Apply patch: %v", err)
	}

	// a controller takes over the replicas field
	applied, ok := appliedObj.(*appsv1.Deployment)
	if !ok {
		t.Fatalf("Failed to convert response object to Deployment")
	}
	replicas := int32(4)
	applied.Spec.Replicas = &replicas
	_, err = client.AppsV1().Deployments("default").
		Update(context.TODO(), applied, metav1.UpdateOptions{FieldManager: "test_updater"})
	if err != nil {
		t.Fatalf("Failed to create object using Apply patch: %v", err)
	}

	// applier omits replicas
	apply = []byte(`{
		"apiVersion": "apps/v1",
		"kind": "Deployment",
		"metadata": {
			"name": "deployment-shared-map-item-removal",
			"labels": {"app": "nginx"}
		},
		"spec": {
			"selector": {
				"matchLabels": {
					"app": "nginx"
				}
			},
			"template": {
				"metadata": {
					"labels": {
						"app": "nginx"
					}
				},
				"spec": {
					"containers": [{
						"name":  "nginx",
						"image": "nginx:latest",
					}]
				}
			}
		}
	}`)

	patched, err := client.CoreV1().RESTClient().Patch(types.ApplyPatchType).
		AbsPath("/apis/apps/v1").
		Namespace("default").
		Resource("deployments").
		Name("deployment-shared-map-item-removal").
		Param("fieldManager", "test_applier").
		Body(apply).
		Do(context.TODO()).
		Get()
	if err != nil {
		t.Fatalf("Failed to create object using Apply patch: %v", err)
	}

	// ensure the container is deleted even though a controller updated a field of the container
	deployment, ok := patched.(*appsv1.Deployment)
	if !ok {
		t.Fatalf("Failed to convert response object to Deployment")
	}
	if *deployment.Spec.Replicas != 4 {
		t.Errorf("Expected deployment.spec.replicas to be 4, but got %d", deployment.Spec.Replicas)
	}
}

// TestApplyCanRemoveMapItemsContributedToByControllers verifies that when an applier creates an
// object, a controller modifies the contents of the map item via update, and the applier
// then omits the item from its applied configuration, that the item is removed.
func TestApplyCanRemoveMapItemsContributedToByControllers(t *testing.T) {
	client, closeFn := setup(t)
	defer closeFn()

	// Applier creates a deployment with a name=nginx container
	apply := []byte(`{
		"apiVersion": "apps/v1",
		"kind": "Deployment",
		"metadata": {
			"name": "deployment-shared-map-item-removal",
			"labels": {"app": "nginx"}
		},
		"spec": {
			"selector": {
				"matchLabels": {
					"app": "nginx"
				}
			},
			"template": {
				"metadata": {
					"labels": {
						"app": "nginx"
					}
				},
				"spec": {
					"containers": [{
						"name":  "nginx",
						"image": "nginx:latest",
					}]
				}
			}
		}
	}`)

	appliedObj, err := client.CoreV1().RESTClient().Patch(types.ApplyPatchType).
		AbsPath("/apis/apps/v1").
		Namespace("default").
		Resource("deployments").
		Name("deployment-shared-map-item-removal").
		Param("fieldManager", "test_applier").
		Body(apply).
		Do(context.TODO()).
		Get()
	if err != nil {
		t.Fatalf("Failed to create object using Apply patch: %v", err)
	}

	// a controller sets container.workingDir of the name=nginx container via an update
	applied, ok := appliedObj.(*appsv1.Deployment)
	if !ok {
		t.Fatalf("Failed to convert response object to Deployment")
	}
	applied.Spec.Template.Spec.Containers[0].WorkingDir = "/home/replacement"
	_, err = client.AppsV1().Deployments("default").
		Update(context.TODO(), applied, metav1.UpdateOptions{FieldManager: "test_updater"})
	if err != nil {
		t.Fatalf("Failed to create object using Apply patch: %v", err)
	}

	// applier removes name=nginx the container
	apply = []byte(`{
		"apiVersion": "apps/v1",
		"kind": "Deployment",
		"metadata": {
			"name": "deployment-shared-map-item-removal",
			"labels": {"app": "nginx"}
		},
		"spec": {
			"replicas": 3,
			"selector": {
				"matchLabels": {
					"app": "nginx"
				}
			},
			"template": {
				"metadata": {
					"labels": {
						"app": "nginx"
					}
				},
				"spec": {
					"hostname": "test-hostname",
					"containers": [{
						"name":  "other-container",
						"image": "nginx:latest",
					}]
				}
			}
		}
	}`)

	patched, err := client.CoreV1().RESTClient().Patch(types.ApplyPatchType).
		AbsPath("/apis/apps/v1").
		Namespace("default").
		Resource("deployments").
		Name("deployment-shared-map-item-removal").
		Param("fieldManager", "test_applier").
		Body(apply).
		Do(context.TODO()).
		Get()
	if err != nil {
		t.Fatalf("Failed to create object using Apply patch: %v", err)
	}

	// ensure the container is deleted even though a controller updated a field of the container
	deployment, ok := patched.(*appsv1.Deployment)
	if !ok {
		t.Fatalf("Failed to convert response object to Deployment")
	}
	if len(deployment.Spec.Template.Spec.Containers) != 1 {
		t.Fatalf("Expected 1 container after apply, got %d", len(deployment.Spec.Template.Spec.Containers))
	}
	if deployment.Spec.Template.Spec.Containers[0].Name != "other-container" {
		t.Fatalf("Expected container to be named \"other-container\" but got %s", deployment.Spec.Template.Spec.Containers[0].Name)
	}
}

// TestDefaultMissingKeys makes sure that the missing keys default is used when merging.
func TestDefaultMissingKeys(t *testing.T) {
	client, closeFn := setup(t)
	defer closeFn()

	// Applier creates a deployment with containerPort but no protocol
	apply := []byte(`{
		"apiVersion": "apps/v1",
		"kind": "Deployment",
		"metadata": {
			"name": "deployment-shared-map-item-removal",
			"labels": {"app": "nginx"}
		},
		"spec": {
			"selector": {
				"matchLabels": {
					"app": "nginx"
				}
			},
			"template": {
				"metadata": {
					"labels": {
						"app": "nginx"
					}
				},
				"spec": {
					"containers": [{
						"name":  "nginx",
						"image": "nginx:latest",
						"ports": [{
							"name": "foo",
							"containerPort": 80
						}]
					}]
				}
			}
		}
	}`)

	_, err := client.CoreV1().RESTClient().Patch(types.ApplyPatchType).
		AbsPath("/apis/apps/v1").
		Namespace("default").
		Resource("deployments").
		Name("deployment-shared-map-item-removal").
		Param("fieldManager", "test_applier").
		Body(apply).
		Do(context.TODO()).
		Get()
	if err != nil {
		t.Fatalf("Failed to create object using Apply patch: %v", err)
	}

	// Applier updates the name, and uses the protocol, we should get a conflict.
	apply = []byte(`{
		"apiVersion": "apps/v1",
		"kind": "Deployment",
		"metadata": {
			"name": "deployment-shared-map-item-removal",
			"labels": {"app": "nginx"}
		},
		"spec": {
			"selector": {
				"matchLabels": {
					"app": "nginx"
				}
			},
			"template": {
				"metadata": {
					"labels": {
						"app": "nginx"
					}
				},
				"spec": {
					"containers": [{
						"name":  "nginx",
						"image": "nginx:latest",
						"ports": [{
							"name": "bar",
							"containerPort": 80,
							"protocol": "TCP"
						}]
					}]
				}
			}
		}
	}`)
	patched, err := client.CoreV1().RESTClient().Patch(types.ApplyPatchType).
		AbsPath("/apis/apps/v1").
		Namespace("default").
		Resource("deployments").
		Name("deployment-shared-map-item-removal").
		Param("fieldManager", "test_applier_conflict").
		Body(apply).
		Do(context.TODO()).
		Get()
	if err == nil {
		t.Fatalf("Expecting to get conflicts when a different applier updates existing list item, got no error: %s", patched)
	}
	status, ok := err.(*apierrors.StatusError)
	if !ok {
		t.Fatalf("Expecting to get conflicts as API error")
	}
	if len(status.Status().Details.Causes) != 1 {
		t.Fatalf("Expecting to get one conflict when a different applier updates existing list item, got: %v", status.Status().Details.Causes)
	}
}

var podBytes = []byte(`
apiVersion: v1
kind: Pod
metadata:
  labels:
    app: some-app
    plugin1: some-value
    plugin2: some-value
    plugin3: some-value
    plugin4: some-value
  name: some-name
  namespace: default
  ownerReferences:
  - apiVersion: apps/v1
    blockOwnerDeletion: true
    controller: true
    kind: ReplicaSet
    name: some-name
    uid: 0a9d2b9e-779e-11e7-b422-42010a8001be
spec:
  containers:
  - args:
    - one
    - two
    - three
    - four
    - five
    - six
    - seven
    - eight
    - nine
    env:
    - name: VAR_3
      valueFrom:
        secretKeyRef:
          key: some-other-key
          name: some-oher-name
    - name: VAR_2
      valueFrom:
        secretKeyRef:
          key: other-key
          name: other-name
    - name: VAR_1
      valueFrom:
        secretKeyRef:
          key: some-key
          name: some-name
    image: some-image-name
    imagePullPolicy: IfNotPresent
    name: some-name
    resources:
      requests:
        cpu: "0"
    terminationMessagePath: /dev/termination-log
    terminationMessagePolicy: File
    volumeMounts:
    - mountPath: /var/run/secrets/kubernetes.io/serviceaccount
      name: default-token-hu5jz
      readOnly: true
  dnsPolicy: ClusterFirst
  nodeName: node-name
  priority: 0
  restartPolicy: Always
  schedulerName: default-scheduler
  securityContext: {}
  serviceAccount: default
  serviceAccountName: default
  terminationGracePeriodSeconds: 30
  tolerations:
  - effect: NoExecute
    key: node.kubernetes.io/not-ready
    operator: Exists
    tolerationSeconds: 300
  - effect: NoExecute
    key: node.kubernetes.io/unreachable
    operator: Exists
    tolerationSeconds: 300
  volumes:
  - name: default-token-hu5jz
    secret:
      defaultMode: 420
      secretName: default-token-hu5jz
status:
  conditions:
  - lastProbeTime: null
    lastTransitionTime: "2019-07-08T09:31:18Z"
    status: "True"
    type: Initialized
  - lastProbeTime: null
    lastTransitionTime: "2019-07-08T09:41:59Z"
    status: "True"
    type: Ready
  - lastProbeTime: null
    lastTransitionTime: null
    status: "True"
    type: ContainersReady
  - lastProbeTime: null
    lastTransitionTime: "2019-07-08T09:31:18Z"
    status: "True"
    type: PodScheduled
  containerStatuses:
  - containerID: docker://885e82a1ed0b7356541bb410a0126921ac42439607c09875cd8097dd5d7b5376
    image: some-image-name
    imageID: docker-pullable://some-image-id
    lastState:
      terminated:
        containerID: docker://d57290f9e00fad626b20d2dd87a3cf69bbc22edae07985374f86a8b2b4e39565
        exitCode: 255
        finishedAt: "2019-07-08T09:39:09Z"
        reason: Error
        startedAt: "2019-07-08T09:38:54Z"
    name: name
    ready: true
    restartCount: 6
    state:
      running:
        startedAt: "2019-07-08T09:41:59Z"
  hostIP: 10.0.0.1
  phase: Running
  podIP: 10.0.0.1
  qosClass: BestEffort
  startTime: "2019-07-08T09:31:18Z"
`)

func decodePod(podBytes []byte) v1.Pod {
	pod := v1.Pod{}
	err := yaml.Unmarshal(podBytes, &pod)
	if err != nil {
		panic(err)
	}
	return pod
}

func encodePod(pod v1.Pod) []byte {
	podBytes, err := yaml.Marshal(pod)
	if err != nil {
		panic(err)
	}
	return podBytes
}

func getPodBytesWhenEnabled(b *testing.B, pod v1.Pod, format string) []byte {
	client, closeFn := setup(b)
	defer closeFn()
	flag.Lookup("v").Value.Set("0")

	pod.Name = "size-pod"
	podB, err := client.CoreV1().RESTClient().Patch(types.ApplyPatchType).
		Name(pod.Name).
		Namespace("default").
		Param("fieldManager", "apply_test").
		Resource("pods").
		SetHeader("Accept", format).
		Body(encodePod(pod)).DoRaw(context.TODO())
	if err != nil {
		b.Fatalf("Failed to create object: %#v", err)
	}
	return podB
}

func BenchmarkServerSideApply(b *testing.B) {
	podBytesWhenEnabled := getPodBytesWhenEnabled(b, decodePod(podBytes), "application/yaml")

	client, closeFn := setup(b)
	defer closeFn()
	flag.Lookup("v").Value.Set("0")

	benchAll(b, client, decodePod(podBytesWhenEnabled))
}

func benchAll(b *testing.B, client clientset.Interface, pod v1.Pod) {
	// Make sure pod is ready to post
	pod.ObjectMeta.CreationTimestamp = metav1.Time{}
	pod.ObjectMeta.ResourceVersion = ""
	pod.ObjectMeta.UID = ""

	// Create pod for repeated-updates
	pod.Name = "repeated-pod"
	_, err := client.CoreV1().RESTClient().Post().
		Namespace("default").
		Resource("pods").
		SetHeader("Content-Type", "application/yaml").
		Body(encodePod(pod)).Do(context.TODO()).Get()
	if err != nil {
		b.Fatalf("Failed to create object: %v", err)
	}

	b.Run("List1", benchListPod(client, pod, 1))
	b.Run("List20", benchListPod(client, pod, 20))
	b.Run("List200", benchListPod(client, pod, 200))
	b.Run("List2000", benchListPod(client, pod, 2000))

	b.Run("RepeatedUpdates", benchRepeatedUpdate(client, "repeated-pod"))
	b.Run("Post1", benchPostPod(client, pod, 1))
	b.Run("Post10", benchPostPod(client, pod, 10))
	b.Run("Post50", benchPostPod(client, pod, 50))
}

func benchPostPod(client clientset.Interface, pod v1.Pod, parallel int) func(*testing.B) {
	return func(b *testing.B) {
		b.ResetTimer()
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			c := make(chan error)
			for j := 0; j < parallel; j++ {
				j := j
				i := i
				go func(pod v1.Pod) {
					pod.Name = fmt.Sprintf("post%d-%d-%d-%d", parallel, b.N, j, i)
					_, err := client.CoreV1().RESTClient().Post().
						Namespace("default").
						Resource("pods").
						SetHeader("Content-Type", "application/yaml").
						Body(encodePod(pod)).Do(context.TODO()).Get()
					c <- err
				}(pod)
			}
			for j := 0; j < parallel; j++ {
				err := <-c
				if err != nil {
					b.Fatal(err)
				}
			}
			close(c)
		}
	}
}

func createNamespace(client clientset.Interface, name string) error {
	namespace := v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	namespaceBytes, err := yaml.Marshal(namespace)
	if err != nil {
		return fmt.Errorf("Failed to marshal namespace: %v", err)
	}
	_, err = client.CoreV1().RESTClient().Get().
		Resource("namespaces").
		SetHeader("Content-Type", "application/yaml").
		Body(namespaceBytes).Do(context.TODO()).Get()
	if err != nil {
		return fmt.Errorf("Failed to create namespace: %v", err)
	}
	return nil
}

func benchListPod(client clientset.Interface, pod v1.Pod, num int) func(*testing.B) {
	return func(b *testing.B) {
		namespace := fmt.Sprintf("get-%d-%d", num, b.N)
		if err := createNamespace(client, namespace); err != nil {
			b.Fatal(err)
		}
		// Create pods
		for i := 0; i < num; i++ {
			pod.Name = fmt.Sprintf("get-%d-%d", b.N, i)
			pod.Namespace = namespace
			_, err := client.CoreV1().RESTClient().Post().
				Namespace(namespace).
				Resource("pods").
				SetHeader("Content-Type", "application/yaml").
				Body(encodePod(pod)).Do(context.TODO()).Get()
			if err != nil {
				b.Fatalf("Failed to create object: %v", err)
			}
		}

		b.ResetTimer()
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			_, err := client.CoreV1().RESTClient().Get().
				Namespace(namespace).
				Resource("pods").
				SetHeader("Accept", "application/vnd.kubernetes.protobuf").
				Do(context.TODO()).Get()
			if err != nil {
				b.Fatalf("Failed to patch object: %v", err)
			}
		}
	}
}

func benchRepeatedUpdate(client clientset.Interface, podName string) func(*testing.B) {
	return func(b *testing.B) {
		b.ResetTimer()
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			_, err := client.CoreV1().RESTClient().Patch(types.JSONPatchType).
				Namespace("default").
				Resource("pods").
				Name(podName).
				Body([]byte(fmt.Sprintf(`[{"op": "replace", "path": "/spec/containers/0/image", "value": "image%d"}]`, i))).Do(context.TODO()).Get()
			if err != nil {
				b.Fatalf("Failed to patch object: %v", err)
			}
		}
	}
}

func TestUpgradeClientSideToServerSideApply(t *testing.T) {
	client, closeFn := setup(t)
	defer closeFn()

	obj := []byte(`
apiVersion: apps/v1
kind: Deployment
metadata:
  name: my-deployment
  annotations:
    "kubectl.kubernetes.io/last-applied-configuration": |
      {"kind":"Deployment","apiVersion":"apps/v1","metadata":{"name":"my-deployment","labels":{"app":"my-app"}},"spec":{"replicas": 3,"template":{"metadata":{"labels":{"app":"my-app"}},"spec":{"containers":[{"name":"my-c","image":"my-image"}]}}}}
  labels:
    app: my-app
spec:
  replicas: 100000
  selector:
    matchLabels:
      app: my-app
  template:
    metadata:
      labels:
        app: my-app
    spec:
      containers:
      - name: my-c
        image: my-image
`)

	deployment, err := yamlutil.ToJSON(obj)
	if err != nil {
		t.Fatalf("Failed marshal yaml: %v", err)
	}

	_, err = client.CoreV1().RESTClient().Post().
		AbsPath("/apis/apps/v1").
		Namespace("default").
		Resource("deployments").
		Body(deployment).Do(context.TODO()).Get()
	if err != nil {
		t.Fatalf("Failed to create object: %v", err)
	}

	obj = []byte(`
apiVersion: apps/v1
kind: Deployment
metadata:
  name: my-deployment
  labels:
    app: my-new-label
spec:
  replicas: 3 # expect conflict
  template:
    metadata:
      labels:
        app: my-app
    spec:
      containers:
      - name: my-c
        image: my-image
`)

	deployment, err = yamlutil.ToJSON(obj)
	if err != nil {
		t.Fatalf("Failed marshal yaml: %v", err)
	}

	_, err = client.CoreV1().RESTClient().Patch(types.ApplyPatchType).
		AbsPath("/apis/apps/v1").
		Namespace("default").
		Resource("deployments").
		Name("my-deployment").
		Param("fieldManager", "kubectl").
		Body(deployment).
		Do(context.TODO()).
		Get()
	if !apierrors.IsConflict(err) {
		t.Fatalf("Expected conflict error but got: %v", err)
	}

	obj = []byte(`
apiVersion: apps/v1
kind: Deployment
metadata:
  name: my-deployment
  labels:
    app: my-new-label
spec:
  template:
    metadata:
      labels:
        app: my-app
    spec:
      containers:
      - name: my-c
        image: my-image-new
`)

	deployment, err = yamlutil.ToJSON(obj)
	if err != nil {
		t.Fatalf("Failed marshal yaml: %v", err)
	}

	_, err = client.CoreV1().RESTClient().Patch(types.ApplyPatchType).
		AbsPath("/apis/apps/v1").
		Namespace("default").
		Resource("deployments").
		Name("my-deployment").
		Param("fieldManager", "kubectl").
		Body(deployment).
		Do(context.TODO()).
		Get()
	if err != nil {
		t.Fatalf("Failed to apply object: %v", err)
	}

	deploymentObj, err := client.AppsV1().Deployments("default").Get(context.TODO(), "my-deployment", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Failed to get object: %v", err)
	}
	if *deploymentObj.Spec.Replicas != 100000 {
		t.Fatalf("expected to get obj with replicas %d, but got %d", 100000, *deploymentObj.Spec.Replicas)
	}
	if deploymentObj.Spec.Template.Spec.Containers[0].Image != "my-image-new" {
		t.Fatalf("expected to get obj with image %s, but got %s", "my-image-new", deploymentObj.Spec.Template.Spec.Containers[0].Image)
	}
}

func TestRenamingAppliedFieldManagers(t *testing.T) {
	client, closeFn := setup(t)
	defer closeFn()

	// Creating an object
	podBytes := []byte(`{
		"apiVersion": "v1",
		"kind": "Pod",
		"metadata": {
			"name": "just-a-pod",
			"labels": {
				"a": "one"
			}
		},
		"spec": {
			"containers": [{
				"name":  "test-container-a",
				"image": "test-image-one"
			}]
		}
	}`)
	_, err := client.CoreV1().RESTClient().
		Patch(types.ApplyPatchType).
		Namespace("default").
		Param("fieldManager", "multi_manager_one").
		Resource("pods").
		Name("just-a-pod").
		Body(podBytes).
		Do(context.TODO()).
		Get()
	if err != nil {
		t.Fatalf("Failed to apply: %v", err)
	}
	_, err = client.CoreV1().RESTClient().
		Patch(types.ApplyPatchType).
		Namespace("default").
		Param("fieldManager", "multi_manager_two").
		Resource("pods").
		Name("just-a-pod").
		Body([]byte(`{"apiVersion":"v1","kind":"Pod","metadata":{"labels":{"b":"two"}}}`)).
		Do(context.TODO()).
		Get()
	if err != nil {
		t.Fatalf("Failed to apply: %v", err)
	}

	pod, err := client.CoreV1().Pods("default").Get(context.TODO(), "just-a-pod", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Failed to get object: %v", err)
	}
	expectedLabels := map[string]string{
		"a": "one",
		"b": "two",
	}
	if !reflect.DeepEqual(pod.Labels, expectedLabels) {
		t.Fatalf("Expected labels to be %v, but got %v", expectedLabels, pod.Labels)
	}

	managedFields := pod.GetManagedFields()
	for i := range managedFields {
		managedFields[i].Manager = "multi_manager"
	}
	pod.SetManagedFields(managedFields)

	obj, err := client.CoreV1().RESTClient().
		Put().
		Namespace("default").
		Resource("pods").
		Name("just-a-pod").
		Body(pod).
		Do(context.TODO()).
		Get()
	if err != nil {
		t.Fatalf("Failed to update: %v", err)
	}

	accessor, err := meta.Accessor(obj)
	if err != nil {
		t.Fatalf("Failed to get meta accessor for object: %v", err)
	}
	managedFields = accessor.GetManagedFields()
	if len(managedFields) != 1 {
		t.Fatalf("Expected object to have 1 managed fields entry, got: %d", len(managedFields))
	}
	entry := managedFields[0]
	if entry.Manager != "multi_manager" || entry.Operation != "Apply" || string(entry.FieldsV1.Raw) != `{"f:metadata":{"f:labels":{"f:b":{}}}}` {
		t.Fatalf(`Unexpected entry, got: %v`, entry)
	}
}

func TestRenamingUpdatedFieldManagers(t *testing.T) {
	client, closeFn := setup(t)
	defer closeFn()

	// Creating an object
	podBytes := []byte(`{
		"apiVersion": "v1",
		"kind": "Pod",
		"metadata": {
			"name": "just-a-pod"
		},
		"spec": {
			"containers": [{
				"name":  "test-container-a",
				"image": "test-image-one"
			}]
		}
	}`)
	_, err := client.CoreV1().RESTClient().
		Patch(types.ApplyPatchType).
		Namespace("default").
		Param("fieldManager", "first").
		Resource("pods").
		Name("just-a-pod").
		Body(podBytes).
		Do(context.TODO()).
		Get()
	if err != nil {
		t.Fatalf("Failed to create object: %v", err)
	}

	_, err = client.CoreV1().RESTClient().
		Patch(types.MergePatchType).
		Namespace("default").
		Param("fieldManager", "multi_manager_one").
		Resource("pods").
		Name("just-a-pod").
		Body([]byte(`{"metadata":{"labels":{"a":"one"}}}`)).
		Do(context.TODO()).
		Get()
	if err != nil {
		t.Fatalf("Failed to update: %v", err)
	}
	_, err = client.CoreV1().RESTClient().
		Patch(types.MergePatchType).
		Namespace("default").
		Param("fieldManager", "multi_manager_two").
		Resource("pods").
		Name("just-a-pod").
		Body([]byte(`{"metadata":{"labels":{"b":"two"}}}`)).
		Do(context.TODO()).
		Get()
	if err != nil {
		t.Fatalf("Failed to update: %v", err)
	}

	pod, err := client.CoreV1().Pods("default").Get(context.TODO(), "just-a-pod", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Failed to get object: %v", err)
	}
	expectedLabels := map[string]string{
		"a": "one",
		"b": "two",
	}
	if !reflect.DeepEqual(pod.Labels, expectedLabels) {
		t.Fatalf("Expected labels to be %v, but got %v", expectedLabels, pod.Labels)
	}

	managedFields := pod.GetManagedFields()
	for i := range managedFields {
		managedFields[i].Manager = "multi_manager"
	}
	pod.SetManagedFields(managedFields)

	obj, err := client.CoreV1().RESTClient().
		Put().
		Namespace("default").
		Resource("pods").
		Name("just-a-pod").
		Body(pod).
		Do(context.TODO()).
		Get()
	if err != nil {
		t.Fatalf("Failed to update: %v", err)
	}

	accessor, err := meta.Accessor(obj)
	if err != nil {
		t.Fatalf("Failed to get meta accessor for object: %v", err)
	}
	managedFields = accessor.GetManagedFields()
	if len(managedFields) != 2 {
		t.Fatalf("Expected object to have 2 managed fields entries, got: %d", len(managedFields))
	}
	entry := managedFields[1]
	if entry.Manager != "multi_manager" || entry.Operation != "Update" || string(entry.FieldsV1.Raw) != `{"f:metadata":{"f:labels":{"f:b":{}}}}` {
		t.Fatalf(`Unexpected entry, got: %v`, entry)
	}
}

func TestDroppingSubresourceField(t *testing.T) {
	client, closeFn := setup(t)
	defer closeFn()

	// Creating an object
	podBytes := []byte(`{
		"apiVersion": "v1",
		"kind": "Pod",
		"metadata": {
			"name": "just-a-pod"
		},
		"spec": {
			"containers": [{
				"name":  "test-container-a",
				"image": "test-image-one"
			}]
		}
	}`)
	_, err := client.CoreV1().RESTClient().
		Patch(types.ApplyPatchType).
		Namespace("default").
		Param("fieldManager", "first").
		Resource("pods").
		Name("just-a-pod").
		Body(podBytes).
		Do(context.TODO()).
		Get()
	if err != nil {
		t.Fatalf("Failed to create object: %v", err)
	}

	_, err = client.CoreV1().RESTClient().
		Patch(types.ApplyPatchType).
		Namespace("default").
		Param("fieldManager", "label_manager").
		Resource("pods").
		Name("just-a-pod").
		Body([]byte(`{"apiVersion":"v1","kind":"Pod","metadata":{"labels":{"a":"one"}}}`)).
		Do(context.TODO()).
		Get()
	if err != nil {
		t.Fatalf("Failed to apply: %v", err)
	}
	_, err = client.CoreV1().RESTClient().
		Patch(types.ApplyPatchType).
		Namespace("default").
		Param("fieldManager", "label_manager").
		Resource("pods").
		Name("just-a-pod").
		SubResource("status").
		Body([]byte(`{"apiVersion":"v1","kind":"Pod","metadata":{"labels":{"b":"two"}}}`)).
		Do(context.TODO()).
		Get()
	if err != nil {
		t.Fatalf("Failed to apply: %v", err)
	}

	pod, err := client.CoreV1().Pods("default").Get(context.TODO(), "just-a-pod", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Failed to get object: %v", err)
	}
	expectedLabels := map[string]string{
		"a": "one",
		"b": "two",
	}
	if !reflect.DeepEqual(pod.Labels, expectedLabels) {
		t.Fatalf("Expected labels to be %v, but got %v", expectedLabels, pod.Labels)
	}

	managedFields := pod.GetManagedFields()
	if len(managedFields) != 3 {
		t.Fatalf("Expected object to have 3 managed fields entries, got: %d", len(managedFields))
	}
	if managedFields[1].Manager != "label_manager" || managedFields[1].Operation != "Apply" || managedFields[1].Subresource != "" {
		t.Fatalf(`Unexpected entry, got: %v`, managedFields[1])
	}
	if managedFields[2].Manager != "label_manager" || managedFields[2].Operation != "Apply" || managedFields[2].Subresource != "status" {
		t.Fatalf(`Unexpected entry, got: %v`, managedFields[2])
	}

	for i := range managedFields {
		managedFields[i].Subresource = ""
	}
	pod.SetManagedFields(managedFields)

	obj, err := client.CoreV1().RESTClient().
		Put().
		Namespace("default").
		Resource("pods").
		Name("just-a-pod").
		Body(pod).
		Do(context.TODO()).
		Get()
	if err != nil {
		t.Fatalf("Failed to update: %v", err)
	}

	accessor, err := meta.Accessor(obj)
	if err != nil {
		t.Fatalf("Failed to get meta accessor for object: %v", err)
	}
	managedFields = accessor.GetManagedFields()
	if len(managedFields) != 2 {
		t.Fatalf("Expected object to have 2 managed fields entries, got: %d", len(managedFields))
	}
	entry := managedFields[1]
	if entry.Manager != "label_manager" || entry.Operation != "Apply" || string(entry.FieldsV1.Raw) != `{"f:metadata":{"f:labels":{"f:b":{}}}}` {
		t.Fatalf(`Unexpected entry, got: %v`, entry)
	}
}

func TestDroppingSubresourceFromSpecField(t *testing.T) {
	client, closeFn := setup(t)
	defer closeFn()

	// Creating an object
	podBytes := []byte(`{
		"apiVersion": "v1",
		"kind": "Pod",
		"metadata": {
			"name": "just-a-pod"
		},
		"spec": {
			"containers": [{
				"name":  "test-container-a",
				"image": "test-image-one"
			}]
		}
	}`)
	_, err := client.CoreV1().RESTClient().
		Patch(types.ApplyPatchType).
		Namespace("default").
		Param("fieldManager", "first").
		Resource("pods").
		Name("just-a-pod").
		Body(podBytes).
		Do(context.TODO()).
		Get()
	if err != nil {
		t.Fatalf("Failed to create object: %v", err)
	}

	_, err = client.CoreV1().RESTClient().
		Patch(types.MergePatchType).
		Namespace("default").
		Param("fieldManager", "manager").
		Resource("pods").
		Name("just-a-pod").
		Body([]byte(`{"metadata":{"labels":{"a":"two"}}}`)).
		Do(context.TODO()).
		Get()
	if err != nil {
		t.Fatalf("Failed to update: %v", err)
	}

	_, err = client.CoreV1().RESTClient().
		Patch(types.MergePatchType).
		Namespace("default").
		Param("fieldManager", "manager").
		Resource("pods").
		SubResource("status").
		Name("just-a-pod").
		Body([]byte(`{"status":{"phase":"Running"}}`)).
		Do(context.TODO()).
		Get()
	if err != nil {
		t.Fatalf("Failed to apply: %v", err)
	}

	pod, err := client.CoreV1().Pods("default").Get(context.TODO(), "just-a-pod", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Failed to get object: %v", err)
	}
	expectedLabels := map[string]string{"a": "two"}
	if !reflect.DeepEqual(pod.Labels, expectedLabels) {
		t.Fatalf("Expected labels to be %v, but got %v", expectedLabels, pod.Labels)
	}
	if pod.Status.Phase != v1.PodRunning {
		t.Fatalf("Expected phase to be %q, but got %q", v1.PodRunning, pod.Status.Phase)
	}

	managedFields := pod.GetManagedFields()
	if len(managedFields) != 3 {
		t.Fatalf("Expected object to have 3 managed fields entries, got: %d", len(managedFields))
	}
	if managedFields[1].Manager != "manager" || managedFields[1].Operation != "Update" || managedFields[1].Subresource != "" {
		t.Fatalf(`Unexpected entry, got: %v`, managedFields[1])
	}
	if managedFields[2].Manager != "manager" || managedFields[2].Operation != "Update" || managedFields[2].Subresource != "status" {
		t.Fatalf(`Unexpected entry, got: %v`, managedFields[2])
	}

	for i := range managedFields {
		managedFields[i].Subresource = ""
	}
	pod.SetManagedFields(managedFields)

	obj, err := client.CoreV1().RESTClient().
		Put().
		Namespace("default").
		Resource("pods").
		Name("just-a-pod").
		Body(pod).
		Do(context.TODO()).
		Get()
	if err != nil {
		t.Fatalf("Failed to update: %v", err)
	}

	accessor, err := meta.Accessor(obj)
	if err != nil {
		t.Fatalf("Failed to get meta accessor for object: %v", err)
	}
	managedFields = accessor.GetManagedFields()
	if len(managedFields) != 2 {
		t.Fatalf("Expected object to have 2 managed fields entries, got: %d", len(managedFields))
	}
	entry := managedFields[1]
	if entry.Manager != "manager" || entry.Operation != "Update" || string(entry.FieldsV1.Raw) != `{"f:status":{"f:phase":{}}}` {
		t.Fatalf(`Unexpected entry, got: %v`, entry)
	}
}

func TestSubresourceField(t *testing.T) {
	client, closeFn := setup(t)
	defer closeFn()

	// Creating a deployment
	deploymentBytes := []byte(`{
		"apiVersion": "apps/v1",
		"kind": "Deployment",
		"metadata": {
			"name": "deployment",
			"labels": {"app": "nginx"}
		},
		"spec": {
			"replicas": 3,
			"selector": {
				"matchLabels": {
					 "app": "nginx"
				}
			},
			"template": {
				"metadata": {
					"labels": {
						"app": "nginx"
					}
				},
				"spec": {
					"containers": [{
						"name":  "nginx",
						"image": "nginx:latest"
					}]
				}
			}
		}
	}`)
	_, err := client.CoreV1().RESTClient().
		Patch(types.ApplyPatchType).
		AbsPath("/apis/apps/v1").
		Namespace("default").
		Resource("deployments").
		Name("deployment").
		Param("fieldManager", "manager").
		Body(deploymentBytes).Do(context.TODO()).Get()
	if err != nil {
		t.Fatalf("Failed to apply object: %v", err)
	}

	_, err = client.CoreV1().RESTClient().
		Patch(types.MergePatchType).
		AbsPath("/apis/apps/v1").
		Namespace("default").
		Resource("deployments").
		SubResource("scale").
		Name("deployment").
		Body([]byte(`{"spec":{"replicas":32}}`)).
		Param("fieldManager", "manager").
		Do(context.TODO()).
		Get()
	if err != nil {
		t.Fatalf("Failed to update status: %v", err)
	}

	deployment, err := client.AppsV1().Deployments("default").Get(context.TODO(), "deployment", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Failed to get object: %v", err)
	}

	managedFields := deployment.GetManagedFields()
	if len(managedFields) != 2 {
		t.Fatalf("Expected object to have 2 managed fields entries, got: %d", len(managedFields))
	}
	if managedFields[0].Manager != "manager" || managedFields[0].Operation != "Apply" || managedFields[0].Subresource != "" {
		t.Fatalf(`Unexpected entry, got: %v`, managedFields[0])
	}
	if managedFields[1].Manager != "manager" ||
		managedFields[1].Operation != "Update" ||
		managedFields[1].Subresource != "scale" ||
		string(managedFields[1].FieldsV1.Raw) != `{"f:spec":{"f:replicas":{}}}` {
		t.Fatalf(`Unexpected entry, got: %v`, managedFields[1])
	}
}

// K8s has a bug introduced in vX.XX.X which changed the treatment of
// ObjectReferences from granular to atomic. This means that only one manager
// may own all fields of the ObjectReference. This resulted in a regression
// for the common use case of user-specified GVK, and machine-populated UID fields.
//
// This is a test to show that clusters  affected by this bug before it was fixed
// do not experience any friction when updating to a version of k8s which marks
// the fields' management again as granular.
func TestApplyFormerlyAtomicFields(t *testing.T) {
	// Start server with our populated ObjectReference. Since it is atomic its
	// ownership changed when XX populated the UID after the user specified the
	// GVKN.

	// 1. Create PersistentVolume with its claimRef owned by
	//		kube-controller-manager as in v1.22 - 1.24
	// 2. Attempt to re-apply the original PersistentVolume which does not
	//		include uid.
	// 3. Check that:
	//		a.) The operation was successful;
	//		b.) The uid is unchanged

	client, closeFn := setup(t)
	defer closeFn()

	// old PersistentVolume from last version of k8s with its claimRef owned
	// atomically
	oldPersistentVolume := []byte(`
	{
		"apiVersion": "v1",
		"kind": "PersistentVolume",
		"metadata": {
			"creationTimestamp": "2022-06-08T23:46:32Z",
			"finalizers": [
				"kubernetes.io/pv-protection"
			],
			"labels": {
				"type": "local"
			},
			"name": "pv-storage",
			"uid": "112b18f7-fde6-4e48-aa61-f5168bd576b8"
		},
		"spec": {
			"accessModes": [
				"ReadWriteOnce"
			],
			"capacity": {
				"storage": "16Mi"
			},
			"claimRef": {
				"apiVersion": "v1",
				"kind": "PersistentVolumeClaim",
				"name": "pvc-storage",
				"namespace": "default",
				"resourceVersion": "15499",
				"uid": "2018e302-7b12-406c-9fa2-e52535d29e48"
			},
			"hostPath": {
				"path": "“/tmp/mydata",
				"type": ""
			},
			"persistentVolumeReclaimPolicy": "Retain",
			"volumeMode": "Filesystem"
		},
		"status": {
			"phase": "Bound"
		}
	}`)

	managedFieldsUpdate := []byte(`{
		"apiVersion": "v1",
		"kind": "PersistentVolume",
		"metadata": {
			"name": "pv-storage",
			"managedFields": [
				{
					"apiVersion": "v1",
					"fieldsType": "FieldsV1",
					"fieldsV1": {
						"f:metadata": {
							"f:labels": {
								"f:type": {}
							}
						},
						"f:spec": {
							"f:accessModes": {},
							"f:capacity": {
								"f:storage": {}
							},
							"f:hostPath": {
								"f:path": {}
							},
							"f:storageClassName": {}
						}
					},
					"manager": "apply_test",
					"operation": "Apply",
					"time": "2022-06-08T23:46:32Z"
				},
				{
					"apiVersion": "v1",
					"fieldsType": "FieldsV1",
					"fieldsV1": {
						"f:status": {
							"f:phase": {}
						}
					},
					"manager": "kube-controller-manager",
					"operation": "Update",
					"subresource": "status",
					"time": "2022-06-08T23:46:32Z"
				},
				{
					"apiVersion": "v1",
					"fieldsType": "FieldsV1",
					"fieldsV1": {
						"f:spec": {
							"f:claimRef": {}
						}
					},
					"manager": "kube-controller-manager",
					"operation": "Update",
					"time": "2022-06-08T23:46:37Z"
				}
			]
		}
	}`)

	// Re-applies name and namespace
	originalPV := []byte(`{
		"kind": "PersistentVolume",
		"apiVersion": "v1",
		"metadata": {
			"labels": {
				"type": "local"
			},
			"name": "pv-storage",
		},
		"spec": {
			"storageClassName": "",
			"capacity": {
				"storage": "16Mi"
			},
			"accessModes": [
				"ReadWriteOnce"
			],
			"hostPath": {
				"path": "“/tmp/mydata"
			},
			"claimRef": {
				"name": "pvc-storage",
				"namespace": "default"
			}
		}
	}`)

	// Create PV
	originalObj, err := client.CoreV1().RESTClient().
		Post().
		Param("fieldManager", "apply_test").
		Resource("persistentvolumes").
		Body(oldPersistentVolume).
		Do(context.TODO()).
		Get()

	if err != nil {
		t.Fatalf("Failed to apply object: %v", err)
	} else if _, ok := originalObj.(*v1.PersistentVolume); !ok {
		t.Fatalf("returned object is incorrect type: %t", originalObj)
	}

	// Directly set managed fields to object
	newObj, err := client.CoreV1().RESTClient().
		Patch(types.StrategicMergePatchType).
		Name("pv-storage").
		Param("fieldManager", "apply_test").
		Resource("persistentvolumes").
		Body(managedFieldsUpdate).
		Do(context.TODO()).
		Get()

	if err != nil {
		t.Fatalf("Failed to apply object: %v", err)
	} else if _, ok := newObj.(*v1.PersistentVolume); !ok {
		t.Fatalf("returned object is incorrect type: %t", newObj)
	}

	// Is initialized, attempt to write to fields underneath
	//	claimRef ObjectReference.
	newObj, err = client.CoreV1().RESTClient().
		Patch(types.ApplyPatchType).
		Name("pv-storage").
		Param("fieldManager", "apply_test").
		Resource("persistentvolumes").
		Body(originalPV).
		Do(context.TODO()).
		Get()

	if err != nil {
		t.Fatalf("Failed to apply object: %v", err)
	} else if _, ok := newObj.(*v1.PersistentVolume); !ok {
		t.Fatalf("returned object is incorrect type: %t", newObj)
	}

	// Test that bug is fixed by showing no error and that uid is not cleared.
	if !reflect.DeepEqual(originalObj.(*v1.PersistentVolume).Spec.ClaimRef, newObj.(*v1.PersistentVolume).Spec.ClaimRef) {
		t.Fatalf("claimRef changed unexpectedly")
	}

	// Expect that we know own name/namespace fields
	// All other fields unowned
	// Make sure apply_test now owns claimRef.UID and that kube-controller-manager owns
	// claimRef (but its ownership is not respected due to new granular structType)
	managedFields := newObj.(*v1.PersistentVolume).ManagedFields
	var expectedManagedFields []metav1.ManagedFieldsEntry
	expectedManagedFieldsString := []byte(`[
		{
			"apiVersion": "v1",
			"fieldsType": "FieldsV1",
			"fieldsV1": {"f:metadata":{"f:labels":{"f:type":{}}},"f:spec":{"f:accessModes":{},"f:capacity":{"f:storage":{}},"f:claimRef":{"f:name":{},"f:namespace":{}},"f:hostPath":{"f:path":{}},"f:storageClassName":{}}},
			"manager": "apply_test",
			"operation": "Apply",
			"time": "2022-06-08T23:46:32Z"
		},
		{
			"apiVersion": "v1",
			"fieldsType": "FieldsV1",
			"fieldsV1": {"f:status":{"f:phase":{}}},
			"manager": "kube-controller-manager",
			"operation": "Update",
			"subresource": "status",
			"time": "2022-06-08T23:46:32Z"
		},
		{
			"apiVersion": "v1",
			"fieldsType": "FieldsV1",
			"fieldsV1": {"f:spec":{"f:claimRef":{}}},
			"manager": "kube-controller-manager",
			"operation": "Update",
			"time": "2022-06-08T23:46:37Z"
		}
	]`)

	err = json.Unmarshal(expectedManagedFieldsString, &expectedManagedFields)
	if err != nil {
		t.Fatalf("unexpectedly failed to decode expected managed fields")
	}

	// Wipe timestamps before comparison
	for i := range expectedManagedFields {
		expectedManagedFields[i].Time = nil
	}

	for i := range managedFields {
		managedFields[i].Time = nil
	}

	if !reflect.DeepEqual(expectedManagedFields, managedFields) {
		t.Fatalf("unexpected managed fields: %v", cmp.Diff(expectedManagedFields, managedFields))
	}
}

func TestDuplicatesInAssociativeLists(t *testing.T) {
	client, closeFn := setup(t)
	defer closeFn()

	ds := []byte(`{
  "apiVersion": "apps/v1",
  "kind": "DaemonSet",
  "metadata": {
    "name": "example-daemonset",
    "labels": {
      "app": "example"
    }
  },
  "spec": {
    "selector": {
      "matchLabels": {
        "app": "example"
      }
    },
    "template": {
      "metadata": {
        "labels": {
          "app": "example"
        }
      },
      "spec": {
        "containers": [
          {
            "name": "nginx",
            "image": "nginx",
            "ports": [
              {
                "name": "port0",
                "containerPort": 1
              },
              {
              	"name": "port1",
                "containerPort": 80
              },
              {
              	"name": "port2",
                "containerPort": 80
              }
            ],
            "env": [
              {
                "name": "ENV0",
                "value": "/env0value"
              },
              {
                "name": "PATH",
                "value": "/bin"
              },
              {
                "name": "PATH",
                "value": "$PATH:/usr/bin"
              }
            ]
          }
        ]
      }
    }
  }
}`)
	// Create the object
	obj, err := client.AppsV1().RESTClient().
		Post().
		Namespace("default").
		Param("fieldManager", "create").
		Resource("daemonsets").
		Body(ds).
		Do(context.TODO()).
		Get()
	if err != nil {
		t.Fatalf("Failed to create the object: %v", err)
	}
	daemon := obj.(*appsv1.DaemonSet)
	if want, got := 3, len(daemon.Spec.Template.Spec.Containers[0].Env); want != got {
		t.Fatalf("Expected %v EnvVars, got %v", want, got)
	}
	if want, got := 3, len(daemon.Spec.Template.Spec.Containers[0].Ports); want != got {
		t.Fatalf("Expected %v Ports, got %v", want, got)
	}

	expectManagedFields(t, daemon.ManagedFields, `
[
	{
		"manager": "create",
		"operation": "Update",
		"apiVersion": "apps/v1",
		"time": null,
		"fieldsType": "FieldsV1",
		"fieldsV1": {
			"f:metadata": {
				"f:annotations": {
					".": {},
					"f:deprecated.daemonset.template.generation": {}
				},
				"f:labels": {
					".": {},
					"f:app": {}
				}
			},
			"f:spec": {
			"f:revisionHistoryLimit": {},
			"f:selector": {},
			"f:template": {
				"f:metadata": {
					"f:labels": {
						".": {},
						"f:app": {}
					}
				},
				"f:spec": {
					"f:containers": {
						"k:{\"name\":\"nginx\"}": {
						".": {},
						"f:env": {
							".": {},
							"k:{\"name\":\"ENV0\"}": {
								".": {},
								"f:name": {},
								"f:value": {}
							},
							"k:{\"name\":\"PATH\"}": {}
						},
						"f:image": {},
						"f:imagePullPolicy": {},
						"f:name": {},
						"f:ports": {
							".": {},
							"k:{\"containerPort\":1,\"protocol\":\"TCP\"}": {
								".": {},
								"f:containerPort": {},
								"f:name": {},
								"f:protocol": {}
							},
							"k:{\"containerPort\":80,\"protocol\":\"TCP\"}": {}
						},
						"f:resources": {},
						"f:terminationMessagePath": {},
						"f:terminationMessagePolicy": {}
						}
					},
					"f:dnsPolicy": {},
					"f:restartPolicy": {},
					"f:schedulerName": {},
					"f:securityContext": {},
					"f:terminationGracePeriodSeconds": {}
					}
				},
				"f:updateStrategy": {
					"f:rollingUpdate": {
					".": {},
					"f:maxSurge": {},
					"f:maxUnavailable": {}
					},
					"f:type": {}
				}
			}
		}
	}
]`)

	// Apply unrelated fields, fieldmanager should be strictly additive.
	ds = []byte(`
apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: example-daemonset
  labels:
    app: example
spec:
  selector:
    matchLabels:
      app: example
  template:
    spec:
      containers:
        - name: nginx
          image: nginx
          ports:
            - name: port3
              containerPort: 443
          env:
            - name: HOME
              value: "/usr/home"
`)
	obj, err = client.AppsV1().RESTClient().
		Patch(types.ApplyPatchType).
		Namespace("default").
		Name("example-daemonset").
		Param("fieldManager", "apply").
		Resource("daemonsets").
		Body(ds).
		Do(context.TODO()).
		Get()
	if err != nil {
		t.Fatalf("Failed to apply the first object: %v", err)
	}
	daemon = obj.(*appsv1.DaemonSet)
	if want, got := 4, len(daemon.Spec.Template.Spec.Containers[0].Env); want != got {
		t.Fatalf("Expected %v EnvVars, got %v", want, got)
	}
	if want, got := 4, len(daemon.Spec.Template.Spec.Containers[0].Ports); want != got {
		t.Fatalf("Expected %v Ports, got %v", want, got)
	}

	expectManagedFields(t, daemon.ManagedFields, `
[
	{
		"manager": "apply",
		"operation": "Apply",
		"apiVersion": "apps/v1",
		"time": null,
		"fieldsType": "FieldsV1",
		"fieldsV1": {
			"f:metadata": {
				"f:labels": {
					"f:app": {}
				}
			},
			"f:spec": {
				"f:selector": {},
				"f:template": {
					"f:spec": {
						"f:containers": {
							"k:{\"name\":\"nginx\"}": {
								".": {},
								"f:env": {
									"k:{\"name\":\"HOME\"}": {
										".": {},
										"f:name": {},
										"f:value": {}
									}
								},
								"f:image": {},
								"f:name": {},
								"f:ports": {
									"k:{\"containerPort\":443,\"protocol\":\"TCP\"}": {
										".": {},
										"f:containerPort": {},
										"f:name": {}
									}
								}
							}
						}
					}
				}
			}
		}
	},
	{
		"manager": "create",
		"operation": "Update",
		"apiVersion": "apps/v1",
		"time": null,
		"fieldsType": "FieldsV1",
		"fieldsV1": {
			"f:metadata": {
				"f:annotations": {
					".": {},
					"f:deprecated.daemonset.template.generation": {}
				},
				"f:labels": {
					".": {},
					"f:app": {}
				}
			},
			"f:spec": {
				"f:revisionHistoryLimit": {},
				"f:selector": {},
				"f:template": {
					"f:metadata": {
						"f:labels": {
							".": {},
							"f:app": {}
						}
					},
					"f:spec": {
						"f:containers": {
							"k:{\"name\":\"nginx\"}": {
								".": {},
								"f:env": {
									".": {},
									"k:{\"name\":\"ENV0\"}": {
										".": {},
										"f:name": {},
										"f:value": {}
									},
									"k:{\"name\":\"PATH\"}": {}
								},
								"f:image": {},
								"f:imagePullPolicy": {},
								"f:name": {},
								"f:ports": {
									".": {},
									"k:{\"containerPort\":1,\"protocol\":\"TCP\"}": {
										".": {},
										"f:containerPort": {},
										"f:name": {},
										"f:protocol": {}
									},
									"k:{\"containerPort\":80,\"protocol\":\"TCP\"}": {}
								},
								"f:resources": {},
								"f:terminationMessagePath": {},
								"f:terminationMessagePolicy": {}
							}
						},
						"f:dnsPolicy": {},
						"f:restartPolicy": {},
						"f:schedulerName": {},
						"f:securityContext": {},
						"f:terminationGracePeriodSeconds": {}
					}
				},
				"f:updateStrategy": {
					"f:rollingUpdate": {
						".": {},
						"f:maxSurge": {},
						"f:maxUnavailable": {}
					},
					"f:type": {}
				}
			}
		}
	}
]
`)

	// Change name of some ports.
	ds = []byte(`{
  "apiVersion": "apps/v1",
  "kind": "DaemonSet",
  "metadata": {
    "name": "example-daemonset",
    "labels": {
      "app": "example"
    }
  },
  "spec": {
    "selector": {
      "matchLabels": {
        "app": "example"
      }
    },
    "template": {
      "metadata": {
        "labels": {
          "app": "example"
        }
      },
      "spec": {
        "containers": [
          {
            "name": "nginx",
            "image": "nginx",
            "ports": [
              {
                "name": "port0",
                "containerPort": 1
              },
              {
              	"name": "port3",
                "containerPort": 443
              },
              {
              	"name": "port4",
                "containerPort": 80
              },
              {
              	"name": "port5",
                "containerPort": 80
              }
            ],
            "env": [
              {
                "name": "ENV0",
                "value": "/env0value"
              },
              {
                "name": "PATH",
                "value": "/bin"
              },
              {
                "name": "PATH",
                "value": "$PATH:/usr/bin:/usr/local/bin"
              },
              {
                "name": "HOME",
                "value": "/usr/home"
              }
            ]
          }
        ]
      }
    }
  }
}`)
	obj, err = client.AppsV1().RESTClient().
		Put().
		Namespace("default").
		Name("example-daemonset").
		Param("fieldManager", "update").
		Resource("daemonsets").
		Body(ds).
		Do(context.TODO()).
		Get()
	if err != nil {
		t.Fatalf("Failed to update the object: %v", err)
	}
	daemon = obj.(*appsv1.DaemonSet)
	if want, got := 4, len(daemon.Spec.Template.Spec.Containers[0].Env); want != got {
		t.Fatalf("Expected %v EnvVars, got %v", want, got)
	}
	if want, got := 4, len(daemon.Spec.Template.Spec.Containers[0].Ports); want != got {
		t.Fatalf("Expected %v Ports, got %v", want, got)
	}

	expectManagedFields(t, daemon.ManagedFields, `
[
	{
		"manager": "apply",
		"operation": "Apply",
		"apiVersion": "apps/v1",
		"time": null,
		"fieldsType": "FieldsV1",
		"fieldsV1": {
			"f:metadata": {
				"f:labels": {
					"f:app": {}
				}
			},
			"f:spec": {
				"f:selector": {},
				"f:template": {
					"f:spec": {
						"f:containers": {
							"k:{\"name\":\"nginx\"}": {
								".": {},
								"f:env": {
									"k:{\"name\":\"HOME\"}": {
										".": {},
										"f:name": {},
										"f:value": {}
									}
								},
								"f:image": {},
								"f:name": {},
								"f:ports": {
									"k:{\"containerPort\":443,\"protocol\":\"TCP\"}": {
										".": {},
										"f:containerPort": {},
										"f:name": {}
									}
								}
							}
						}
					}
				}
			}
		}
	},
	{
		"manager": "create",
		"operation": "Update",
		"apiVersion": "apps/v1",
		"time": null,
		"fieldsType": "FieldsV1",
		"fieldsV1": {
			"f:metadata": {
				"f:annotations": {},
				"f:labels": {
					".": {},
					"f:app": {}
				}
			},
			"f:spec": {
				"f:revisionHistoryLimit": {},
				"f:selector": {},
				"f:template": {
					"f:metadata": {
						"f:labels": {
							".": {},
							"f:app": {}
						}
					},
					"f:spec": {
						"f:containers": {
							"k:{\"name\":\"nginx\"}": {
								".": {},
								"f:env": {
									".": {},
									"k:{\"name\":\"ENV0\"}": {
										".": {},
										"f:name": {},
										"f:value": {}
									}
								},
								"f:image": {},
								"f:imagePullPolicy": {},
								"f:name": {},
								"f:ports": {
									".": {},
									"k:{\"containerPort\":1,\"protocol\":\"TCP\"}": {
										".": {},
										"f:containerPort": {},
										"f:name": {},
										"f:protocol": {}
									}
								},
								"f:resources": {},
								"f:terminationMessagePath": {},
								"f:terminationMessagePolicy": {}
							}
						},
						"f:dnsPolicy": {},
						"f:restartPolicy": {},
						"f:schedulerName": {},
						"f:securityContext": {},
						"f:terminationGracePeriodSeconds": {}
					}
				},
				"f:updateStrategy": {
					"f:rollingUpdate": {
						".": {},
						"f:maxSurge": {},
						"f:maxUnavailable": {}
					},
					"f:type": {}
				}
			}
		}
	},
	{
		"manager": "update",
		"operation": "Update",
		"apiVersion": "apps/v1",
		"time": null,
		"fieldsType": "FieldsV1",
		"fieldsV1": {
			"f:metadata": {
				"f:annotations": {
				"f:deprecated.daemonset.template.generation": {}
				}
			},
			"f:spec": {
				"f:template": {
					"f:spec": {
						"f:containers": {
							"k:{\"name\":\"nginx\"}": {
								"f:env": {
									"k:{\"name\":\"PATH\"}": {}
								},
								"f:ports": {
									"k:{\"containerPort\":80,\"protocol\":\"TCP\"}": {}
								}
							}
						}
					}
				}
			}
		}
	}
]
`)

	// Replaces envvars and paths.
	ds = []byte(`
apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: example-daemonset
  labels:
    app: example
spec:
  selector:
    matchLabels:
      app: example
  template:
    spec:
      containers:
        - name: nginx
          image: nginx
          ports:
            - name: port80
              containerPort: 80
          env:
            - name: PATH
              value: "/bin:/usr/bin:/usr/local/bin"
`)
	obj, err = client.AppsV1().RESTClient().
		Patch(types.ApplyPatchType).
		Namespace("default").
		Name("example-daemonset").
		Param("fieldManager", "apply").
		Param("force", "true").
		Resource("daemonsets").
		Body(ds).
		Do(context.TODO()).
		Get()
	if err != nil {
		t.Fatalf("Failed to apply the second object: %v", err)
	}

	daemon = obj.(*appsv1.DaemonSet)
	// HOME is removed, PATH is replaced with 1.
	if want, got := 2, len(daemon.Spec.Template.Spec.Containers[0].Env); want != got {
		t.Fatalf("Expected %v EnvVars, got %v", want, got)
	}
	if want, got := 2, len(daemon.Spec.Template.Spec.Containers[0].Ports); want != got {
		t.Fatalf("Expected %v Ports, got %v", want, got)
	}

	expectManagedFields(t, daemon.ManagedFields, `
[
	{
		"manager": "apply",
		"operation": "Apply",
		"apiVersion": "apps/v1",
		"time": null,
		"fieldsType": "FieldsV1",
		"fieldsV1": {
			"f:metadata": {
				"f:labels": {
					"f:app": {}
				}
			},
			"f:spec": {
				"f:selector": {},
				"f:template": {
					"f:spec": {
						"f:containers": {
							"k:{\"name\":\"nginx\"}": {
								".": {},
								"f:env": {
									"k:{\"name\":\"PATH\"}": {
										".": {},
										"f:name": {},
										"f:value": {}
									}
								},
								"f:image": {},
								"f:name": {},
								"f:ports": {
									"k:{\"containerPort\":80,\"protocol\":\"TCP\"}": {
										".": {},
										"f:containerPort": {},
										"f:name": {}
									}
								}
							}
						}
					}
				}
			}
		}
	},
	{
		"manager": "create",
		"operation": "Update",
		"apiVersion": "apps/v1",
		"time": null,
		"fieldsType": "FieldsV1",
		"fieldsV1": {
			"f:metadata": {
				"f:annotations": {},
				"f:labels": {
					".": {},
					"f:app": {}
				}
			},
			"f:spec": {
				"f:revisionHistoryLimit": {},
				"f:selector": {},
				"f:template": {
					"f:metadata": {
						"f:labels": {
							".": {},
							"f:app": {}
						}
					},
					"f:spec": {
						"f:containers": {
							"k:{\"name\":\"nginx\"}": {
								".": {},
								"f:env": {
									".": {},
									"k:{\"name\":\"ENV0\"}": {
										".": {},
										"f:name": {},
										"f:value": {}
									}
								},
								"f:image": {},
								"f:imagePullPolicy": {},
								"f:name": {},
								"f:ports": {
									".": {},
									"k:{\"containerPort\":1,\"protocol\":\"TCP\"}": {
										".": {},
										"f:containerPort": {},
										"f:name": {},
										"f:protocol": {}
									}
								},
								"f:resources": {},
								"f:terminationMessagePath": {},
								"f:terminationMessagePolicy": {}
							}
						},
						"f:dnsPolicy": {},
						"f:restartPolicy": {},
						"f:schedulerName": {},
						"f:securityContext": {},
						"f:terminationGracePeriodSeconds": {}
					}
				},
				"f:updateStrategy": {
					"f:rollingUpdate": {
						".": {},
						"f:maxSurge": {},
						"f:maxUnavailable": {}
					},
					"f:type": {}
				}
			}
		}
	},
	{
		"manager": "update",
		"operation": "Update",
		"apiVersion": "apps/v1",
		"time": null,
		"fieldsType": "FieldsV1",
		"fieldsV1": {
			"f:metadata": {
				"f:annotations": {
					"f:deprecated.daemonset.template.generation": {}
				}
			}
		}
	}
]
`)
}

func TestApplyMatchesFakeClientsetApply(t *testing.T) {
	client, closeFn := setup(t)
	defer closeFn()
	fakeClient := fake.NewClientset()

	// The fake client does not default fields, so we set all defaulted fields directly.
	deployment := appsv1ac.Deployment("deployment", "default").
		WithLabels(map[string]string{"app": "nginx"}).
		WithSpec(appsv1ac.DeploymentSpec().
			WithReplicas(3).
			WithStrategy(appsv1ac.DeploymentStrategy().
				WithType(appsv1.RollingUpdateDeploymentStrategyType).
				WithRollingUpdate(appsv1ac.RollingUpdateDeployment().
					WithMaxUnavailable(intstr.FromString("25%")).
					WithMaxSurge(intstr.FromString("25%")))).
			WithRevisionHistoryLimit(10).
			WithProgressDeadlineSeconds(600).
			WithSelector(metav1ac.LabelSelector().
				WithMatchLabels(map[string]string{"app": "nginx"})).
			WithTemplate(corev1ac.PodTemplateSpec().
				WithLabels(map[string]string{"app": "nginx"}).
				WithSpec(corev1ac.PodSpec().
					WithRestartPolicy(v1.RestartPolicyAlways).
					WithTerminationGracePeriodSeconds(30).
					WithDNSPolicy(v1.DNSClusterFirst).
					WithSecurityContext(corev1ac.PodSecurityContext()).
					WithSchedulerName("default-scheduler").
					WithContainers(corev1ac.Container().
						WithName("nginx").
						WithImage("nginx:latest").
						WithTerminationMessagePath("/dev/termination-log").
						WithTerminationMessagePolicy("File").
						WithImagePullPolicy(v1.PullAlways)))))
	fieldManager := "m-1"

	realCreated, err := client.AppsV1().Deployments("default").Apply(context.TODO(), deployment, metav1.ApplyOptions{FieldManager: fieldManager})
	if err != nil {
		t.Fatalf("Failed to create object using Apply patch: %v", err)
	}

	fakeCreated, err := fakeClient.AppsV1().Deployments("default").Apply(context.TODO(), deployment, metav1.ApplyOptions{FieldManager: fieldManager})
	if err != nil {
		t.Fatalf("Failed to create object using Apply patch: %v", err)
	}

	// wipe metadata except name, namespace, labels and managedFields (but wipe timestamps in managedFields)
	realCreated.ObjectMeta = wipeMetadataForFakeClientTests(realCreated.ObjectMeta)
	fakeCreated.ObjectMeta = wipeMetadataForFakeClientTests(fakeCreated.ObjectMeta)
	// wipe status
	realCreated.Status = appsv1.DeploymentStatus{}
	fakeCreated.Status = appsv1.DeploymentStatus{}
	// TODO: Remove once https://github.com/kubernetes/kubernetes/issues/125671 is fixed.
	fakeCreated.TypeMeta = metav1.TypeMeta{}

	if diff := cmp.Diff(realCreated, fakeCreated); diff != "" {
		t.Errorf("Unexpected fake created: (-want +got): %v", diff)
	}

	// Force apply with a different field manager
	deploymentUpdate := appsv1ac.Deployment("deployment", "default").
		WithSpec(appsv1ac.DeploymentSpec().
			WithReplicas(4))
	updateManager := "m-2"
	realUpdated, err := client.AppsV1().Deployments("default").Apply(context.TODO(), deploymentUpdate, metav1.ApplyOptions{FieldManager: updateManager, Force: true})
	if err != nil {
		t.Fatalf("Failed to create object using Apply patch: %v", err)
	}

	fakeUpdated, err := fakeClient.AppsV1().Deployments("default").Apply(context.TODO(), deploymentUpdate, metav1.ApplyOptions{FieldManager: updateManager, Force: true})
	if err != nil {
		t.Fatalf("Failed to create object using Apply patch: %v", err)
	}

	// wipe metadata except name, namespace, labels and managedFields (but wipe timestamps in managedFields)
	realUpdated.ObjectMeta = wipeMetadataForFakeClientTests(realUpdated.ObjectMeta)
	fakeUpdated.ObjectMeta = wipeMetadataForFakeClientTests(fakeUpdated.ObjectMeta)
	// wipe status
	realUpdated.Status = appsv1.DeploymentStatus{}
	fakeUpdated.Status = appsv1.DeploymentStatus{}
	// TODO: Remove once https://github.com/kubernetes/kubernetes/issues/125671 is fixed.
	fakeUpdated.TypeMeta = metav1.TypeMeta{}

	if diff := cmp.Diff(realUpdated, fakeUpdated); diff != "" {
		t.Errorf("Unexpected fake updated: (-want +got): %v", diff)
	}
}

var wipeTime = metav1.NewTime(time.Date(2000, 1, 1, 0, 0, 0, 0, time.FixedZone("EDT", -4*60*60)))

func wipeMetadataForFakeClientTests(meta metav1.ObjectMeta) metav1.ObjectMeta {
	wipedManagedFields := make([]metav1.ManagedFieldsEntry, len(meta.ManagedFields))
	copy(meta.ManagedFields, wipedManagedFields)
	for _, mf := range wipedManagedFields {
		mf.Time = &wipeTime
	}
	return metav1.ObjectMeta{
		Name:          meta.Name,
		Namespace:     meta.Namespace,
		Labels:        meta.Labels,
		ManagedFields: wipedManagedFields,
	}
}

func expectManagedFields(t *testing.T, managedFields []metav1.ManagedFieldsEntry, expect string) {
	t.Helper()
	for i := range managedFields {
		managedFields[i].Time = &metav1.Time{}
	}
	got, err := json.MarshalIndent(managedFields, "", "  ")
	if err != nil {
		t.Fatalf("Failed to marshal managed fields: %v", err)
	}
	b := &bytes.Buffer{}
	err = json.Indent(b, []byte(expect), "", "  ")
	if err != nil {
		t.Fatalf("Failed to indent json: %v", err)
	}
	want := b.String()
	diff := cmp.Diff(strings.Split(strings.TrimSpace(string(got)), "\n"), strings.Split(strings.TrimSpace(want), "\n"))
	if len(diff) > 0 {
		t.Fatalf("Want:\n%s\nGot:\n%s\nDiff:\n%s", string(want), string(got), diff)
	}
}

// TestCreateOnApplyFailsWithForbidden makes sure that PATCH requests with the apply content type
// will not create the object if the user does not have both patch and create permissions.
func TestCreateOnApplyFailsWithForbidden(t *testing.T) {
	// Enable RBAC so we can exercise authorization errors.
	server := kubeapiservertesting.StartTestServerOrDie(t, nil, append([]string{"--authorization-mode=RBAC"}, framework.DefaultTestServerFlags()...), framework.SharedEtcd())
	t.Cleanup(server.TearDownFn)

	adminClient := clientset.NewForConfigOrDie(server.ClientConfig)

	pandaConfig := restclient.CopyConfig(server.ClientConfig)
	pandaConfig.Impersonate.UserName = "panda"
	pandaClient := clientset.NewForConfigOrDie(pandaConfig)

	errPatch := ssaPod(pandaClient)

	requireForbiddenPodErr(t, errPatch, `pods "test-pod" is forbidden: User "panda" cannot patch resource "pods" in API group "" in the namespace "default"`)

	createPodRBACAndWait(t, adminClient, "patch")

	errCreate := ssaPod(pandaClient)

	requireForbiddenPodErr(t, errCreate, `pods "test-pod" is forbidden: User "panda" cannot create resource "pods" in API group "" in the namespace "default"`)

	createPodRBACAndWait(t, adminClient, "create")

	errNone := ssaPod(pandaClient)
	require.NoError(t, errNone, "pod create via SSA should succeed now that RBAC is correct")
}

func requireForbiddenPodErr(t *testing.T, err error, message string) {
	t.Helper()

	require.Truef(t, apierrors.IsForbidden(err), "Expected forbidden error but got: %v", err)

	wantStatusErr := &apierrors.StatusError{ErrStatus: metav1.Status{
		Status:  "Failure",
		Message: message,
		Reason:  "Forbidden",
		Details: &metav1.StatusDetails{
			Name: "test-pod",
			Kind: "pods",
		},
		Code: http.StatusForbidden,
	}}
	require.Equal(t, wantStatusErr, err, "unexpected status error")
}

func ssaPod(client *clientset.Clientset) error {
	_, err := client.CoreV1().RESTClient().Patch(types.ApplyPatchType).
		Namespace("default").
		Resource("pods").
		Name("test-pod").
		Param("fieldManager", "apply_test").
		Body([]byte(`{
			"apiVersion": "v1",
			"kind": "Pod",
			"metadata": {
				"name": "test-pod"
			},
			"spec": {
				"containers": [{
					"name":  "test-container",
					"image": "test-image"
				}]
			}
		}`)).
		Do(context.TODO()).
		Get()
	return err
}

func createPodRBACAndWait(t *testing.T, client *clientset.Clientset, verb string) {
	t.Helper()

	_, err := client.RbacV1().ClusterRoles().Create(context.TODO(), &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{
			Name: fmt.Sprintf("can-%s-pods", verb),
		},
		Rules: []rbacv1.PolicyRule{
			{
				Verbs:     []string{verb},
				APIGroups: []string{""},
				Resources: []string{"pods"},
			},
		},
	}, metav1.CreateOptions{})
	require.NoError(t, err)

	_, err = client.RbacV1().RoleBindings("default").Create(context.TODO(), &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name: fmt.Sprintf("can-%s-pods", verb),
		},
		Subjects: []rbacv1.Subject{
			{
				Kind: rbacv1.UserKind,
				Name: "panda",
			},
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "ClusterRole",
			Name:     fmt.Sprintf("can-%s-pods", verb),
		},
	}, metav1.CreateOptions{})
	require.NoError(t, err)

	authutil.WaitForNamedAuthorizationUpdate(t, context.TODO(), client.AuthorizationV1(),
		"panda",
		"default",
		verb,
		"",
		schema.GroupResource{Resource: "pods"},
		true,
	)
}
