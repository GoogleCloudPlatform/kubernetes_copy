/*
Copyright 2021 The Kubernetes Authors.

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
	"fmt"
	"reflect"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiextensionsv1beta1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1beta1"
	"k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	"k8s.io/apiextensions-apiserver/test/integration/fixtures"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	apiservertesting "k8s.io/kubernetes/cmd/kube-apiserver/app/testing"
	"k8s.io/kubernetes/test/integration/framework"
)

// TestApplyCRDNoSchema tests that CRDs and CRs can both be applied to with a PATCH request with the apply content type
// when there is no validation field provided.
func TestApplyCRDNoSchema(t *testing.T) {
	server, err := apiservertesting.StartTestServer(t, apiservertesting.NewDefaultTestServerOptions(), nil, framework.SharedEtcd())
	if err != nil {
		t.Fatal(err)
	}
	defer server.TearDownFn()
	config := server.ClientConfig

	apiExtensionClient, err := clientset.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}

	noxuBetaDefinition := nearlyRemovedBetaMultipleVersionNoxuCRD(apiextensionsv1beta1.ClusterScoped)

	noxuDefinition, err := fixtures.CreateCRDUsingRemovedAPI(server.EtcdClient, server.EtcdStoragePrefix, noxuBetaDefinition, apiExtensionClient, dynamicClient)
	if err != nil {
		t.Fatal(err)
	}

	kind := noxuDefinition.Spec.Names.Kind
	apiVersion := noxuDefinition.Spec.Group + "/" + noxuDefinition.Spec.Versions[0].Name
	name := "mytest"

	rest := apiExtensionClient.Discovery().RESTClient()
	yamlBody := []byte(fmt.Sprintf(`
apiVersion: %s
kind: %s
metadata:
  name: %s
spec:
  replicas: 1`, apiVersion, kind, name))
	result, err := rest.Patch(types.ApplyPatchType).
		AbsPath("/apis", noxuDefinition.Spec.Group, noxuDefinition.Spec.Versions[0].Name, noxuDefinition.Spec.Names.Plural).
		Name(name).
		Param("fieldManager", "apply_test").
		Body(yamlBody).
		DoRaw(context.TODO())
	if err != nil {
		t.Fatalf("failed to create custom resource with apply: %v:\n%v", err, string(result))
	}
	verifyReplicas(t, result, 1)

	// Patch object to change the number of replicas
	result, err = rest.Patch(types.MergePatchType).
		AbsPath("/apis", noxuDefinition.Spec.Group, noxuDefinition.Spec.Versions[0].Name, noxuDefinition.Spec.Names.Plural).
		Name(name).
		Body([]byte(`{"spec":{"replicas": 5}}`)).
		DoRaw(context.TODO())
	if err != nil {
		t.Fatalf("failed to update number of replicas with merge patch: %v:\n%v", err, string(result))
	}
	verifyReplicas(t, result, 5)

	// Re-apply, we should get conflicts now, since the number of replicas was changed.
	result, err = rest.Patch(types.ApplyPatchType).
		AbsPath("/apis", noxuDefinition.Spec.Group, noxuDefinition.Spec.Versions[0].Name, noxuDefinition.Spec.Names.Plural).
		Name(name).
		Param("fieldManager", "apply_test").
		Body(yamlBody).
		DoRaw(context.TODO())
	if err == nil {
		t.Fatalf("Expecting to get conflicts when applying object after updating replicas, got no error: %s", result)
	}
	status, ok := err.(*apierrors.StatusError)
	if !ok {
		t.Fatalf("Expecting to get conflicts as API error")
	}
	if len(status.Status().Details.Causes) != 1 {
		t.Fatalf("Expecting to get one conflict when applying object after updating replicas, got: %v", status.Status().Details.Causes)
	}

	// Re-apply with force, should work fine.
	result, err = rest.Patch(types.ApplyPatchType).
		AbsPath("/apis", noxuDefinition.Spec.Group, noxuDefinition.Spec.Versions[0].Name, noxuDefinition.Spec.Names.Plural).
		Name(name).
		Param("force", "true").
		Param("fieldManager", "apply_test").
		Body(yamlBody).
		DoRaw(context.TODO())
	if err != nil {
		t.Fatalf("failed to apply object with force after updating replicas: %v:\n%v", err, string(result))
	}
	verifyReplicas(t, result, 1)

	// Try to set managed fields using a subresource and verify that it has no effect
	existingManagedFields, err := getManagedFields(result)
	if err != nil {
		t.Fatalf("failed to get managedFields from response: %v", err)
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
		}
	}`)
	result, err = rest.Patch(types.MergePatchType).
		AbsPath("/apis", noxuDefinition.Spec.Group, noxuDefinition.Spec.Versions[0].Name, noxuDefinition.Spec.Names.Plural).
		SubResource("status").
		Name(name).
		Param("fieldManager", "subresource_test").
		Body(updateBytes).
		DoRaw(context.TODO())
	if err != nil {
		t.Fatalf("Error updating subresource: %v ", err)
	}
	newManagedFields, err := getManagedFields(result)
	if err != nil {
		t.Fatalf("failed to get managedFields from response: %v", err)
	}
	if !reflect.DeepEqual(existingManagedFields, newManagedFields) {
		t.Fatalf("Expected managed fields to not have changed when trying manually setting them via subresources.\n\nExpected: %#v\n\nGot: %#v", existingManagedFields, newManagedFields)
	}

	// However, it is possible to modify managed fields using the main resource
	result, err = rest.Patch(types.MergePatchType).
		AbsPath("/apis", noxuDefinition.Spec.Group, noxuDefinition.Spec.Versions[0].Name, noxuDefinition.Spec.Names.Plural).
		Name(name).
		Param("fieldManager", "subresource_test").
		Body([]byte(`{"metadata":{"managedFields":[{}]}}`)).
		DoRaw(context.TODO())
	if err != nil {
		t.Fatalf("Error updating managed fields of the main resource: %v ", err)
	}
	newManagedFields, err = getManagedFields(result)
	if err != nil {
		t.Fatalf("failed to get managedFields from response: %v", err)
	}

	if len(newManagedFields) != 0 {
		t.Fatalf("Expected managed fields to have been reset, but got: %v", newManagedFields)
	}
}

func nearlyRemovedBetaMultipleVersionNoxuCRD(scope apiextensionsv1beta1.ResourceScope) *apiextensionsv1beta1.CustomResourceDefinition {
	return &apiextensionsv1beta1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "noxus.mygroup.example.com"},
		Spec: apiextensionsv1beta1.CustomResourceDefinitionSpec{
			Group:   "mygroup.example.com",
			Version: "v1beta1",
			Names: apiextensionsv1beta1.CustomResourceDefinitionNames{
				Plural:     "noxus",
				Singular:   "nonenglishnoxu",
				Kind:       "WishIHadChosenNoxu",
				ShortNames: []string{"foo", "bar", "abc", "def"},
				ListKind:   "NoxuItemList",
				Categories: []string{"all"},
			},
			Scope: scope,
			Versions: []apiextensionsv1beta1.CustomResourceDefinitionVersion{
				{
					Name:    "v1beta1",
					Served:  true,
					Storage: false,
				},
				{
					Name:    "v1beta2",
					Served:  true,
					Storage: true,
				},
				{
					Name:    "v0",
					Served:  false,
					Storage: false,
				},
			},
			Subresources: &apiextensionsv1beta1.CustomResourceSubresources{
				Status: &apiextensionsv1beta1.CustomResourceSubresourceStatus{},
			},
		},
	}
}
