/*
Copyright 2022 The Kubernetes Authors.

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
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	apidiscoveryv2 "k8s.io/api/apidiscovery/v2"
	v1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	"k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/fake"
	"k8s.io/apiextensions-apiserver/pkg/client/informers/externalversions"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apiserver/pkg/endpoints/discovery"
	"k8s.io/apiserver/pkg/endpoints/discovery/aggregated"
)

var coolFooCRD = &v1.CustomResourceDefinition{
	TypeMeta: metav1.TypeMeta{
		APIVersion: "apiextensions.k8s.io/v1",
		Kind:       "CustomResourceDefinition",
	},
	ObjectMeta: metav1.ObjectMeta{
		Name: "coolfoo.stable.example.com",
	},
	Spec: v1.CustomResourceDefinitionSpec{
		Group: "stable.example.com",
		Names: v1.CustomResourceDefinitionNames{
			Plural:     "coolfoos",
			Singular:   "coolfoo",
			ShortNames: []string{"foo"},
			Kind:       "CoolFoo",
			ListKind:   "CoolFooList",
			Categories: []string{"cool"},
		},
		Scope: v1.ClusterScoped,
		Versions: []v1.CustomResourceDefinitionVersion{
			{
				Name:       "v1",
				Served:     true,
				Storage:    true,
				Deprecated: false,
				Subresources: &v1.CustomResourceSubresources{
					// This CRD has a /status subresource
					Status: &v1.CustomResourceSubresourceStatus{},
				},
				Schema: &v1.CustomResourceValidation{
					// Unused by discovery
					OpenAPIV3Schema: &v1.JSONSchemaProps{},
				},
			},
		},
		Conversion:            &v1.CustomResourceConversion{},
		PreserveUnknownFields: false,
	},
	Status: v1.CustomResourceDefinitionStatus{
		Conditions: []v1.CustomResourceDefinitionCondition{
			{
				Type:   v1.Established,
				Status: v1.ConditionTrue,
			},
		},
	},
}

var coolBarCRD = &v1.CustomResourceDefinition{
	TypeMeta: metav1.TypeMeta{
		APIVersion: "apiextensions.k8s.io/v1",
		Kind:       "CustomResourceDefinition",
	},
	ObjectMeta: metav1.ObjectMeta{
		Name: "coolbar.stable.example.com",
	},
	Spec: v1.CustomResourceDefinitionSpec{
		Group: "stable.example.com",
		Names: v1.CustomResourceDefinitionNames{
			Plural:     "coolbars",
			Singular:   "coolbar",
			ShortNames: []string{"bar"},
			Kind:       "CoolBar",
			ListKind:   "CoolBarList",
			Categories: []string{"cool"},
		},
		Scope: v1.ClusterScoped,
		Versions: []v1.CustomResourceDefinitionVersion{
			{
				Name:       "v1",
				Served:     true,
				Storage:    true,
				Deprecated: false,
				Schema: &v1.CustomResourceValidation{
					// Unused by discovery
					OpenAPIV3Schema: &v1.JSONSchemaProps{},
				},
			},
		},
		Conversion:            &v1.CustomResourceConversion{},
		PreserveUnknownFields: false,
	},
	Status: v1.CustomResourceDefinitionStatus{
		Conditions: []v1.CustomResourceDefinitionCondition{
			{
				Type:   v1.Established,
				Status: v1.ConditionTrue,
			},
		},
	},
}

var coolFooDiscovery apidiscoveryv2.APIVersionDiscovery = apidiscoveryv2.APIVersionDiscovery{
	Version:   "v1",
	Freshness: apidiscoveryv2.DiscoveryFreshnessCurrent,
	Resources: []apidiscoveryv2.APIResourceDiscovery{
		{
			Resource:         "coolfoos",
			Scope:            apidiscoveryv2.ScopeCluster,
			SingularResource: "coolfoo",
			Verbs:            []string{"delete", "deletecollection", "get", "list", "patch", "create", "update", "watch"},
			ShortNames:       []string{"foo"},
			Categories:       []string{"cool"},
			ResponseKind: &metav1.GroupVersionKind{
				Group:   "stable.example.com",
				Version: "v1",
				Kind:    "CoolFoo",
			},
			Subresources: []apidiscoveryv2.APISubresourceDiscovery{
				{
					Subresource:   "status",
					Verbs:         []string{"get", "patch", "update"},
					AcceptedTypes: nil, // is this correct?
					ResponseKind: &metav1.GroupVersionKind{
						Group:   "stable.example.com",
						Version: "v1",
						Kind:    "CoolFoo",
					},
				},
			},
		},
	},
}

var mergedDiscovery apidiscoveryv2.APIVersionDiscovery = apidiscoveryv2.APIVersionDiscovery{
	Version:   "v1",
	Freshness: apidiscoveryv2.DiscoveryFreshnessCurrent,
	Resources: []apidiscoveryv2.APIResourceDiscovery{
		{
			Resource:         "coolbars",
			Scope:            apidiscoveryv2.ScopeCluster,
			SingularResource: "coolbar",
			Verbs:            []string{"delete", "deletecollection", "get", "list", "patch", "create", "update", "watch"},
			ShortNames:       []string{"bar"},
			Categories:       []string{"cool"},
			ResponseKind: &metav1.GroupVersionKind{
				Group:   "stable.example.com",
				Version: "v1",
				Kind:    "CoolBar",
			},
		}, {
			Resource:         "coolfoos",
			Scope:            apidiscoveryv2.ScopeCluster,
			SingularResource: "coolfoo",
			Verbs:            []string{"delete", "deletecollection", "get", "list", "patch", "create", "update", "watch"},
			ShortNames:       []string{"foo"},
			Categories:       []string{"cool"},
			ResponseKind: &metav1.GroupVersionKind{
				Group:   "stable.example.com",
				Version: "v1",
				Kind:    "CoolFoo",
			},
			Subresources: []apidiscoveryv2.APISubresourceDiscovery{
				{
					Subresource:   "status",
					Verbs:         []string{"get", "patch", "update"},
					AcceptedTypes: nil, // is this correct?
					ResponseKind: &metav1.GroupVersionKind{
						Group:   "stable.example.com",
						Version: "v1",
						Kind:    "CoolFoo",
					},
				},
			},
		},
	},
}

func init() {
	// Not testing against an apiserver, so just assume names are accepted
	coolFooCRD.Status.AcceptedNames = coolFooCRD.Spec.Names
	coolBarCRD.Status.AcceptedNames = coolBarCRD.Spec.Names
}

// Provides an apiextensions-apiserver client
type testEnvironment struct {
	clientset.Interface

	// Discovery test details
	versionDiscoveryHandler
	groupDiscoveryHandler

	aggregated.FakeResourceManager
}

func (env *testEnvironment) Start(ctx context.Context) {
	discoverySyncedCh := make(chan struct{})

	factory := externalversions.NewSharedInformerFactoryWithOptions(
		env.Interface, 30*time.Second)

	discoveryController := NewDiscoveryController(
		factory.Apiextensions().V1().CustomResourceDefinitions(),
		&env.versionDiscoveryHandler,
		&env.groupDiscoveryHandler,
		env.FakeResourceManager,
	)

	factory.Start(ctx.Done())
	go discoveryController.Run(ctx.Done(), discoverySyncedCh)

	select {
	case <-discoverySyncedCh:
	case <-ctx.Done():
	}
}

func setup() *testEnvironment {
	env := &testEnvironment{
		Interface:           fake.NewSimpleClientset(),
		FakeResourceManager: aggregated.NewFakeResourceManager(),
		versionDiscoveryHandler: versionDiscoveryHandler{
			discovery: make(map[schema.GroupVersion]*discovery.APIVersionHandler),
		},
		groupDiscoveryHandler: groupDiscoveryHandler{
			discovery:       make(map[string]*discovery.APIGroupHandler),
			discoveryGroups: make(map[string]metav1.APIGroup),
		},
	}

	return env
}

func TestResourceManagerExistingCRD(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	env := setup()
	_, err := env.Interface.
		ApiextensionsV1().
		CustomResourceDefinitions().
		Create(
			ctx,
			coolFooCRD,
			metav1.CreateOptions{
				FieldManager: "resource-manager-test",
			},
		)

	require.NoError(t, err)

	env.FakeResourceManager.Expect().
		AddGroupVersion(coolFooCRD.Spec.Group, coolFooDiscovery)
	for _, v := range coolFooCRD.Spec.Versions {
		env.FakeResourceManager.Expect().
			SetGroupVersionPriority(metav1.GroupVersion{Group: coolFooCRD.Spec.Group, Version: v.Name}, 1000, 100)
	}

	env.FakeResourceManager.Expect().
		AddGroupVersion(coolFooCRD.Spec.Group, coolFooDiscovery)
	for _, v := range coolFooCRD.Spec.Versions {
		env.FakeResourceManager.Expect().
			SetGroupVersionPriority(metav1.GroupVersion{Group: coolFooCRD.Spec.Group, Version: v.Name}, 1000, 100)
	}

	env.Start(ctx)
	err = env.FakeResourceManager.WaitForActions(ctx, 1*time.Second)
	require.NoError(t, err)
}

// Tests that if a CRD is added a runtime, the discovery controller will
// put its information in the discovery document
func TestResourceManagerAddedCRD(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	env := setup()
	env.FakeResourceManager.Expect().
		AddGroupVersion(coolFooCRD.Spec.Group, coolFooDiscovery)
	for _, v := range coolFooCRD.Spec.Versions {
		env.FakeResourceManager.Expect().
			SetGroupVersionPriority(metav1.GroupVersion{Group: coolFooCRD.Spec.Group, Version: v.Name}, 1000, 100)
	}

	env.Start(ctx)

	// Create CRD after the controller has already started
	_, err := env.Interface.
		ApiextensionsV1().
		CustomResourceDefinitions().
		Create(
			ctx,
			coolFooCRD,
			metav1.CreateOptions{
				FieldManager: "resource-manager-test",
			},
		)

	require.NoError(t, err)

	err = env.FakeResourceManager.WaitForActions(ctx, 1*time.Second)
	require.NoError(t, err)
}

// Test that having multiple CRDs in the same version will add both
// versions to discovery.
func TestMultipleCRDSameVersion(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	env := setup()
	env.Start(ctx)

	_, err := env.Interface.
		ApiextensionsV1().
		CustomResourceDefinitions().
		Create(
			ctx,
			coolFooCRD,
			metav1.CreateOptions{
				FieldManager: "resource-manager-test",
			},
		)

	require.NoError(t, err)
	env.FakeResourceManager.Expect().
		AddGroupVersion(coolFooCRD.Spec.Group, coolFooDiscovery)
	for _, versionEntry := range coolFooCRD.Spec.Versions {
		env.FakeResourceManager.Expect().SetGroupVersionPriority(metav1.GroupVersion{Group: coolFooCRD.Spec.Group, Version: versionEntry.Name}, 1000, 100)
	}
	err = env.FakeResourceManager.WaitForActions(ctx, 1*time.Second)
	require.NoError(t, err)

	_, err = env.Interface.
		ApiextensionsV1().
		CustomResourceDefinitions().
		Create(
			ctx,
			coolBarCRD,
			metav1.CreateOptions{
				FieldManager: "resource-manager-test",
			},
		)
	require.NoError(t, err)

	env.FakeResourceManager.Expect().
		AddGroupVersion(coolFooCRD.Spec.Group, mergedDiscovery)
	for _, versionEntry := range coolFooCRD.Spec.Versions {
		env.FakeResourceManager.Expect().SetGroupVersionPriority(metav1.GroupVersion{Group: coolFooCRD.Spec.Group, Version: versionEntry.Name}, 1000, 100)
	}
	err = env.FakeResourceManager.WaitForActions(ctx, 1*time.Second)
	require.NoError(t, err)

	groups, err := env.groupDiscoveryHandler.Groups(ctx, &http.Request{})
	require.NoError(t, err)
	require.Len(t, groups, 1)
}

// Tests that if a CRD is deleted at runtime, the discovery controller will
// remove its information from its ResourceManager
func TestDiscoveryControllerResourceManagerRemovedCRD(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	env := setup()
	env.Start(ctx)

	// Create CRD after the controller has already started
	_, err := env.Interface.
		ApiextensionsV1().
		CustomResourceDefinitions().
		Create(
			ctx,
			coolFooCRD,
			metav1.CreateOptions{},
		)

	require.NoError(t, err)

	// Wait for the Controller to pick up the Create event and add it to the
	// Resource Manager
	env.FakeResourceManager.Expect().
		AddGroupVersion(coolFooCRD.Spec.Group, coolFooDiscovery)
	for _, versionEntry := range coolFooCRD.Spec.Versions {
		env.FakeResourceManager.Expect().SetGroupVersionPriority(metav1.GroupVersion{Group: coolFooCRD.Spec.Group, Version: versionEntry.Name}, 1000, 100)
	}
	err = env.FakeResourceManager.WaitForActions(ctx, 1*time.Second)
	require.NoError(t, err)

	err = env.Interface.
		ApiextensionsV1().
		CustomResourceDefinitions().
		Delete(ctx, coolFooCRD.Name, metav1.DeleteOptions{})

	require.NoError(t, err)

	// Wait for the Controller to detect there are no more CRDs of this group
	// and remove the entire group
	env.FakeResourceManager.Expect().RemoveGroup(coolFooCRD.Spec.Group)

	err = env.FakeResourceManager.WaitForActions(ctx, 1*time.Second)
	require.NoError(t, err)
}
