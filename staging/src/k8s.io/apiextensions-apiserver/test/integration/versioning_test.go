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

package integration

import (
	"context"
	stderrors "errors"
	"fmt"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apiextensions-apiserver/test/integration/fixtures"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
)

func TestInternalVersionIsHandlerVersion(t *testing.T) {
	tearDown, apiExtensionClient, dynamicClient, err := fixtures.StartDefaultServerWithClients(t)
	if err != nil {
		t.Fatal(err)
	}
	defer tearDown()

	noxuDefinition := fixtures.NewMultipleVersionNoxuCRD(apiextensionsv1.NamespaceScoped)

	assert.Equal(t, "v1beta1", noxuDefinition.Spec.Versions[0].Name)
	assert.Equal(t, "v1beta2", noxuDefinition.Spec.Versions[1].Name)
	assert.True(t, noxuDefinition.Spec.Versions[1].Storage)

	noxuDefinition, err = fixtures.CreateNewV1CustomResourceDefinition(noxuDefinition, apiExtensionClient, dynamicClient)
	if err != nil {
		t.Fatal(err)
	}

	ns := "not-the-default"

	noxuNamespacedResourceClientV1beta1 := newNamespacedCustomResourceVersionedClient(ns, dynamicClient, noxuDefinition, "v1beta1") // use the non-storage version v1beta1

	t.Logf("Creating foo")
	noxuInstanceToCreate := fixtures.NewNoxuInstance(ns, "foo")
	_, err = noxuNamespacedResourceClientV1beta1.Create(context.TODO(), noxuInstanceToCreate, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}

	// update validation via update because the cache priming in CreateCRDUsingRemovedAPI will fail otherwise
	t.Logf("Updating CRD to validate apiVersion")
	noxuDefinition, err = UpdateCustomResourceDefinitionWithRetry(apiExtensionClient, noxuDefinition.Name, func(crd *apiextensionsv1.CustomResourceDefinition) {
		for i := range crd.Spec.Versions {
			crd.Spec.Versions[i].Schema = &apiextensionsv1.CustomResourceValidation{
				OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
					Type: "object",
					Properties: map[string]apiextensionsv1.JSONSchemaProps{
						"apiVersion": {
							Pattern: "^mygroup.example.com/v1beta1$", // this means we can only patch via the v1beta1 handler version
							Type:    "string",
						},
					},
					Required: []string{"apiVersion"},
				},
			}
		}
	})
	assert.NoError(t, err)

	time.Sleep(time.Second)

	// patches via handler version v1beta1 should succeed (validation allows that API version)
	{
		t.Logf("patch of handler version v1beta1 (non-storage version) should succeed")
		i := 0
		err = wait.PollUntilContextTimeout(context.Background(), time.Millisecond*100, wait.ForeverTestTimeout, true, func(ctx context.Context) (done bool, err error) {
			patch := []byte(fmt.Sprintf(`{"i": %d}`, i))
			i++

			_, patchErr := noxuNamespacedResourceClientV1beta1.Patch(ctx, "foo", types.MergePatchType, patch, metav1.PatchOptions{})
			if patchErr != nil {
				// work around "grpc: the client connection is closing" error
				// TODO: fix the grpc error
				var statusErr *errors.StatusError
				if stderrors.As(patchErr, &statusErr) && statusErr.Status().Code == http.StatusInternalServerError {
					return false, nil
				}
				return false, patchErr
			}
			return true, nil
		})
		assert.NoError(t, err)
	}

	// patches via handler version matching storage version should fail (validation does not allow that API version)
	{
		t.Logf("patch of handler version v1beta2 (storage version) should fail")
		i := 0
		noxuNamespacedResourceClientV1beta2 := newNamespacedCustomResourceVersionedClient(ns, dynamicClient, noxuDefinition, "v1beta2") // use the storage version v1beta2
		err = wait.PollUntilContextTimeout(context.Background(), time.Millisecond*100, wait.ForeverTestTimeout, true, func(ctx context.Context) (done bool, err error) {
			patch := []byte(fmt.Sprintf(`{"i": %d}`, i))
			i++

			_, patchErr := noxuNamespacedResourceClientV1beta2.Patch(ctx, "foo", types.MergePatchType, patch, metav1.PatchOptions{})
			require.Error(t, patchErr)

			// work around "grpc: the client connection is closing" error
			// TODO: fix the grpc error
			var statusErr *errors.StatusError
			if stderrors.As(patchErr, &statusErr) && statusErr.Status().Code == http.StatusInternalServerError {
				return false, nil
			}

			assert.ErrorContains(t, patchErr, "apiVersion")
			return true, nil
		})
		assert.NoError(t, err)
	}
}

func TestVersionedNamespacedScopedCRD(t *testing.T) {
	tearDown, apiExtensionClient, dynamicClient, err := fixtures.StartDefaultServerWithClients(t)
	if err != nil {
		t.Fatal(err)
	}
	defer tearDown()

	noxuDefinition := fixtures.NewMultipleVersionNoxuCRD(apiextensionsv1.NamespaceScoped)
	noxuDefinition, err = fixtures.CreateNewV1CustomResourceDefinition(noxuDefinition, apiExtensionClient, dynamicClient)
	if err != nil {
		t.Fatal(err)
	}

	ns := "not-the-default"
	testSimpleCRUD(t, ns, noxuDefinition, dynamicClient)
}

func TestVersionedClusterScopedCRD(t *testing.T) {
	tearDown, apiExtensionClient, dynamicClient, err := fixtures.StartDefaultServerWithClients(t)
	if err != nil {
		t.Fatal(err)
	}
	defer tearDown()

	noxuDefinition := fixtures.NewMultipleVersionNoxuCRD(apiextensionsv1.ClusterScoped)
	noxuDefinition, err = fixtures.CreateNewV1CustomResourceDefinition(noxuDefinition, apiExtensionClient, dynamicClient)
	if err != nil {
		t.Fatal(err)
	}

	ns := ""
	testSimpleCRUD(t, ns, noxuDefinition, dynamicClient)
}

func TestStoragedVersionInNamespacedCRDStatus(t *testing.T) {
	noxuDefinition := fixtures.NewMultipleVersionNoxuCRD(apiextensionsv1.NamespaceScoped)
	ns := "not-the-default"
	testStoragedVersionInCRDStatus(t, ns, noxuDefinition)
}

func TestStoragedVersionInClusterScopedCRDStatus(t *testing.T) {
	noxuDefinition := fixtures.NewMultipleVersionNoxuCRD(apiextensionsv1.ClusterScoped)
	ns := ""
	testStoragedVersionInCRDStatus(t, ns, noxuDefinition)
}

func testStoragedVersionInCRDStatus(t *testing.T, ns string, noxuDefinition *apiextensionsv1.CustomResourceDefinition) {
	versionsV1Beta1Storage := []apiextensionsv1.CustomResourceDefinitionVersion{
		{
			Name:    "v1beta1",
			Served:  true,
			Storage: true,
			Schema:  fixtures.AllowAllSchema(),
		},
		{
			Name:    "v1beta2",
			Served:  true,
			Storage: false,
			Schema:  fixtures.AllowAllSchema(),
		},
	}
	versionsV1Beta2Storage := []apiextensionsv1.CustomResourceDefinitionVersion{
		{
			Name:    "v1beta1",
			Served:  true,
			Storage: false,
			Schema:  fixtures.AllowAllSchema(),
		},
		{
			Name:    "v1beta2",
			Served:  true,
			Storage: true,
			Schema:  fixtures.AllowAllSchema(),
		},
	}
	tearDown, apiExtensionClient, dynamicClient, err := fixtures.StartDefaultServerWithClients(t)
	if err != nil {
		t.Fatal(err)
	}
	defer tearDown()

	noxuDefinition.Spec.Versions = versionsV1Beta1Storage
	noxuDefinition, err = fixtures.CreateNewV1CustomResourceDefinition(noxuDefinition, apiExtensionClient, dynamicClient)
	if err != nil {
		t.Fatal(err)
	}

	// The storage version list should be initialized to storage version
	crd, err := apiExtensionClient.ApiextensionsV1().CustomResourceDefinitions().Get(context.TODO(), noxuDefinition.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if e, a := []string{"v1beta1"}, crd.Status.StoredVersions; !reflect.DeepEqual(e, a) {
		t.Errorf("expected %v, got %v", e, a)
	}

	// Changing CRD storage version should be reflected immediately
	crd.Spec.Versions = versionsV1Beta2Storage
	_, err = apiExtensionClient.ApiextensionsV1().CustomResourceDefinitions().Update(context.TODO(), crd, metav1.UpdateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	crd, err = apiExtensionClient.ApiextensionsV1().CustomResourceDefinitions().Get(context.TODO(), noxuDefinition.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if e, a := []string{"v1beta1", "v1beta2"}, crd.Status.StoredVersions; !reflect.DeepEqual(e, a) {
		t.Errorf("expected %v, got %v", e, a)
	}

	err = fixtures.DeleteV1CustomResourceDefinition(crd, apiExtensionClient)
	if err != nil {
		t.Fatal(err)
	}
}
