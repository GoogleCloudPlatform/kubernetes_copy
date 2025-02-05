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

package discovery

import (
	"testing"

	"github.com/stretchr/testify/assert"
	apidiscovery "k8s.io/api/apidiscovery/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestSplitGroupsAndResources(t *testing.T) {
	tests := []struct {
		name                string
		agg                 apidiscovery.APIGroupDiscoveryList
		expectedGroups      metav1.APIGroupList
		expectedGVResources map[schema.GroupVersion]*metav1.APIResourceList
		expectedFailedGVs   map[schema.GroupVersion]error
	}{
		{
			name: "Aggregated discovery: core/v1 group and pod resource",
			agg: apidiscovery.APIGroupDiscoveryList{
				Items: []apidiscovery.APIGroupDiscovery{
					{
						ObjectMeta: metav1.ObjectMeta{
							Name: "",
						},
						Versions: []apidiscovery.APIVersionDiscovery{
							{
								Version: "v1",
								Resources: []apidiscovery.APIResourceDiscovery{
									{
										Resource: "pods",
										ResponseKind: &metav1.GroupVersionKind{
											Group:   "",
											Version: "v1",
											Kind:    "Pod",
										},
										Scope: apidiscovery.ScopeNamespace,
									},
								},
							},
						},
					},
				},
			},
			expectedGroups: metav1.APIGroupList{
				Groups: []metav1.APIGroup{
					{
						Name: "",
						Versions: []metav1.GroupVersionForDiscovery{
							{
								GroupVersion: "v1",
								Version:      "v1",
							},
						},
						PreferredVersion: metav1.GroupVersionForDiscovery{
							GroupVersion: "v1",
							Version:      "v1",
						},
					},
				},
			},
			expectedGVResources: map[schema.GroupVersion]*metav1.APIResourceList{
				{Group: "", Version: "v1"}: {
					GroupVersion: "v1",
					APIResources: []metav1.APIResource{
						{
							Name:       "pods",
							Namespaced: true,
							Group:      "",
							Version:    "v1",
							Kind:       "Pod",
						},
					},
				},
			},
			expectedFailedGVs: map[schema.GroupVersion]error{},
		},
		{
			name: "Aggregated discovery: 1 group/1 resources at /api, 1 group/2 versions/1 resources at /apis",
			agg: apidiscovery.APIGroupDiscoveryList{
				Items: []apidiscovery.APIGroupDiscovery{
					{
						ObjectMeta: metav1.ObjectMeta{
							Name: "apps",
						},
						Versions: []apidiscovery.APIVersionDiscovery{
							{
								Version: "v2",
								Resources: []apidiscovery.APIResourceDiscovery{
									{
										Resource: "deployments",
										ResponseKind: &metav1.GroupVersionKind{
											Group:   "apps",
											Version: "v2",
											Kind:    "Deployment",
										},
										Scope: apidiscovery.ScopeNamespace,
									},
								},
							},
							{
								Version: "v1",
								Resources: []apidiscovery.APIResourceDiscovery{
									{
										Resource: "deployments",
										ResponseKind: &metav1.GroupVersionKind{
											Group:   "apps",
											Version: "v1",
											Kind:    "Deployment",
										},
										Scope: apidiscovery.ScopeNamespace,
									},
								},
							},
						},
					},
				},
			},
			expectedGroups: metav1.APIGroupList{
				Groups: []metav1.APIGroup{
					{
						Name: "apps",
						Versions: []metav1.GroupVersionForDiscovery{
							{
								GroupVersion: "apps/v2",
								Version:      "v2",
							},
							{
								GroupVersion: "apps/v1",
								Version:      "v1",
							},
						},
						PreferredVersion: metav1.GroupVersionForDiscovery{
							GroupVersion: "apps/v2",
							Version:      "v2",
						},
					},
				},
			},
			expectedGVResources: map[schema.GroupVersion]*metav1.APIResourceList{
				{Group: "apps", Version: "v1"}: {
					GroupVersion: "apps/v1",
					APIResources: []metav1.APIResource{
						{
							Name:       "deployments",
							Namespaced: true,
							Group:      "apps",
							Version:    "v1",
							Kind:       "Deployment",
						},
					},
				},
				{Group: "apps", Version: "v2"}: {
					GroupVersion: "apps/v2",
					APIResources: []metav1.APIResource{
						{
							Name:       "deployments",
							Namespaced: true,
							Group:      "apps",
							Version:    "v2",
							Kind:       "Deployment",
						},
					},
				},
			},
			expectedFailedGVs: map[schema.GroupVersion]error{},
		},
		{
			name: "Aggregated discovery: 1 group/2 resources at /api, 1 group/2 resources at /apis",
			agg: apidiscovery.APIGroupDiscoveryList{
				Items: []apidiscovery.APIGroupDiscovery{
					{
						Versions: []apidiscovery.APIVersionDiscovery{
							{
								Version: "v1",
								Resources: []apidiscovery.APIResourceDiscovery{
									{
										Resource: "pods",
										ResponseKind: &metav1.GroupVersionKind{
											Group:   "",
											Version: "v1",
											Kind:    "Pod",
										},
										Scope: apidiscovery.ScopeNamespace,
									},
									{
										Resource: "services",
										ResponseKind: &metav1.GroupVersionKind{
											Group:   "",
											Version: "v1",
											Kind:    "Service",
										},
										Scope: apidiscovery.ScopeNamespace,
									},
								},
							},
						},
					},
					{
						ObjectMeta: metav1.ObjectMeta{
							Name: "apps",
						},
						Versions: []apidiscovery.APIVersionDiscovery{
							{
								Version: "v1",
								Resources: []apidiscovery.APIResourceDiscovery{
									{
										Resource: "deployments",
										ResponseKind: &metav1.GroupVersionKind{
											Group:   "apps",
											Version: "v1",
											Kind:    "Deployment",
										},
										Scope: apidiscovery.ScopeNamespace,
									},
									{
										Resource: "statefulsets",
										ResponseKind: &metav1.GroupVersionKind{
											Group:   "apps",
											Version: "v1",
											Kind:    "StatefulSet",
										},
										Scope: apidiscovery.ScopeNamespace,
									},
								},
							},
						},
					},
				},
			},
			expectedGroups: metav1.APIGroupList{
				Groups: []metav1.APIGroup{
					{
						Name: "",
						Versions: []metav1.GroupVersionForDiscovery{
							{
								GroupVersion: "v1",
								Version:      "v1",
							},
						},
						PreferredVersion: metav1.GroupVersionForDiscovery{
							GroupVersion: "v1",
							Version:      "v1",
						},
					},
					{
						Name: "apps",
						Versions: []metav1.GroupVersionForDiscovery{
							{
								GroupVersion: "apps/v1",
								Version:      "v1",
							},
						},
						PreferredVersion: metav1.GroupVersionForDiscovery{
							GroupVersion: "apps/v1",
							Version:      "v1",
						},
					},
				},
			},
			expectedGVResources: map[schema.GroupVersion]*metav1.APIResourceList{
				{Group: "", Version: "v1"}: {
					GroupVersion: "v1",
					APIResources: []metav1.APIResource{
						{
							Name:       "pods",
							Namespaced: true,
							Group:      "",
							Version:    "v1",
							Kind:       "Pod",
						},
						{
							Name:       "services",
							Namespaced: true,
							Group:      "",
							Version:    "v1",
							Kind:       "Service",
						},
					},
				},
				{Group: "apps", Version: "v1"}: {
					GroupVersion: "apps/v1",
					APIResources: []metav1.APIResource{
						{
							Name:       "deployments",
							Namespaced: true,
							Group:      "apps",
							Version:    "v1",
							Kind:       "Deployment",
						},
						{
							Name:       "statefulsets",
							Namespaced: true,
							Group:      "apps",
							Version:    "v1",
							Kind:       "StatefulSet",
						},
					},
				},
			},
			expectedFailedGVs: map[schema.GroupVersion]error{},
		},
		{
			name: "Aggregated discovery: multiple groups with cluster-scoped resources",
			agg: apidiscovery.APIGroupDiscoveryList{
				Items: []apidiscovery.APIGroupDiscovery{
					{
						Versions: []apidiscovery.APIVersionDiscovery{
							{
								Version: "v1",
								Resources: []apidiscovery.APIResourceDiscovery{
									{
										Resource: "pods",
										ResponseKind: &metav1.GroupVersionKind{
											Group:   "",
											Version: "v1",
											Kind:    "Pod",
										},
										Scope: apidiscovery.ScopeNamespace,
									},
									{
										Resource: "namespaces",
										ResponseKind: &metav1.GroupVersionKind{
											Group:   "",
											Version: "v1",
											Kind:    "Namespace",
										},
										Scope: apidiscovery.ScopeCluster,
									},
								},
							},
						},
					},
					{
						ObjectMeta: metav1.ObjectMeta{
							Name: "rbac.authorization.k8s.io",
						},
						Versions: []apidiscovery.APIVersionDiscovery{
							{
								Version: "v1",
								Resources: []apidiscovery.APIResourceDiscovery{
									{
										Resource: "roles",
										ResponseKind: &metav1.GroupVersionKind{
											Group:   "rbac.authorization.k8s.io",
											Version: "v1",
											Kind:    "Role",
										},
										Scope: apidiscovery.ScopeCluster,
									},
									{
										Resource: "clusterroles",
										ResponseKind: &metav1.GroupVersionKind{
											Group:   "rbac.authorization.k8s.io",
											Version: "v1",
											Kind:    "ClusterRole",
										},
										Scope: apidiscovery.ScopeCluster,
									},
								},
							},
						},
					},
				},
			},
			expectedGroups: metav1.APIGroupList{
				Groups: []metav1.APIGroup{
					{
						Name: "",
						Versions: []metav1.GroupVersionForDiscovery{
							{
								GroupVersion: "v1",
								Version:      "v1",
							},
						},
						PreferredVersion: metav1.GroupVersionForDiscovery{
							GroupVersion: "v1",
							Version:      "v1",
						},
					},
					{
						Name: "rbac.authorization.k8s.io",
						Versions: []metav1.GroupVersionForDiscovery{
							{
								GroupVersion: "rbac.authorization.k8s.io/v1",
								Version:      "v1",
							},
						},
						PreferredVersion: metav1.GroupVersionForDiscovery{
							GroupVersion: "rbac.authorization.k8s.io/v1",
							Version:      "v1",
						},
					},
				},
			},
			expectedGVResources: map[schema.GroupVersion]*metav1.APIResourceList{
				{Group: "", Version: "v1"}: {
					GroupVersion: "v1",
					APIResources: []metav1.APIResource{
						{
							Name:       "pods",
							Namespaced: true,
							Group:      "",
							Version:    "v1",
							Kind:       "Pod",
						},
						{
							Name:       "namespaces",
							Namespaced: false,
							Group:      "",
							Version:    "v1",
							Kind:       "Namespace",
						},
					},
				},
				{Group: "rbac.authorization.k8s.io", Version: "v1"}: {
					GroupVersion: "rbac.authorization.k8s.io/v1",
					APIResources: []metav1.APIResource{
						{
							Name:       "roles",
							Namespaced: false,
							Group:      "rbac.authorization.k8s.io",
							Version:    "v1",
							Kind:       "Role",
						},
						{
							Name:       "clusterroles",
							Namespaced: false,
							Group:      "rbac.authorization.k8s.io",
							Version:    "v1",
							Kind:       "ClusterRole",
						},
					},
				},
			},
			expectedFailedGVs: map[schema.GroupVersion]error{},
		},
		{
			name: "Aggregated discovery with single subresource",
			agg: apidiscovery.APIGroupDiscoveryList{
				Items: []apidiscovery.APIGroupDiscovery{
					{
						ObjectMeta: metav1.ObjectMeta{
							Name: "apps",
						},
						Versions: []apidiscovery.APIVersionDiscovery{
							{
								Version: "v1",
								Resources: []apidiscovery.APIResourceDiscovery{
									{
										Resource: "deployments",
										ResponseKind: &metav1.GroupVersionKind{
											Group:   "apps",
											Version: "v1",
											Kind:    "Deployment",
										},
										Scope:            apidiscovery.ScopeNamespace,
										SingularResource: "deployment",
										ShortNames:       []string{"deploy"},
										Verbs:            []string{"parentverb1", "parentverb2", "parentverb3", "parentverb4"},
										Categories:       []string{"all", "testcategory"},
										Subresources: []apidiscovery.APISubresourceDiscovery{
											{
												Subresource: "scale",
												ResponseKind: &metav1.GroupVersionKind{
													Group:   "apps",
													Version: "v1",
													Kind:    "Deployment",
												},
												Verbs: []string{"get", "patch", "update"},
											},
										},
									},
								},
							},
						},
					},
				},
			},
			expectedGroups: metav1.APIGroupList{
				Groups: []metav1.APIGroup{
					{
						Name: "apps",
						Versions: []metav1.GroupVersionForDiscovery{
							{
								GroupVersion: "apps/v1",
								Version:      "v1",
							},
						},
						PreferredVersion: metav1.GroupVersionForDiscovery{
							GroupVersion: "apps/v1",
							Version:      "v1",
						},
					},
				},
			},
			expectedGVResources: map[schema.GroupVersion]*metav1.APIResourceList{
				{Group: "apps", Version: "v1"}: {
					GroupVersion: "apps/v1",
					APIResources: []metav1.APIResource{
						{
							Name:         "deployments",
							SingularName: "deployment",
							Namespaced:   true,
							Group:        "apps",
							Version:      "v1",
							Kind:         "Deployment",
							Verbs:        []string{"parentverb1", "parentverb2", "parentverb3", "parentverb4"},
							ShortNames:   []string{"deploy"},
							Categories:   []string{"all", "testcategory"},
						},
						{
							Name:         "deployments/scale",
							SingularName: "deployment",
							Namespaced:   true,
							Group:        "apps",
							Version:      "v1",
							Kind:         "Deployment",
							Verbs:        []string{"get", "patch", "update"},
						},
					},
				},
			},
			expectedFailedGVs: map[schema.GroupVersion]error{},
		},
		{
			name: "Aggregated discovery with single subresource and parent missing GVK",
			agg: apidiscovery.APIGroupDiscoveryList{
				Items: []apidiscovery.APIGroupDiscovery{
					{
						ObjectMeta: metav1.ObjectMeta{
							Name: "external.metrics.k8s.io",
						},
						Versions: []apidiscovery.APIVersionDiscovery{
							{
								Version: "v1beta1",
								Resources: []apidiscovery.APIResourceDiscovery{
									{
										// resilient to nil GVK for parent
										Resource:         "*",
										Scope:            apidiscovery.ScopeNamespace,
										SingularResource: "",
										Subresources: []apidiscovery.APISubresourceDiscovery{
											{
												Subresource: "other-external-metric",
												ResponseKind: &metav1.GroupVersionKind{
													Kind: "MetricValueList",
												},
												Verbs: []string{"get"},
											},
										},
									},
								},
							},
						},
					},
				},
			},
			expectedGroups: metav1.APIGroupList{
				Groups: []metav1.APIGroup{
					{
						Name: "external.metrics.k8s.io",
						Versions: []metav1.GroupVersionForDiscovery{
							{
								GroupVersion: "external.metrics.k8s.io/v1beta1",
								Version:      "v1beta1",
							},
						},
						PreferredVersion: metav1.GroupVersionForDiscovery{
							GroupVersion: "external.metrics.k8s.io/v1beta1",
							Version:      "v1beta1",
						},
					},
				},
			},
			expectedGVResources: map[schema.GroupVersion]*metav1.APIResourceList{
				{Group: "external.metrics.k8s.io", Version: "v1beta1"}: {
					GroupVersion: "external.metrics.k8s.io/v1beta1",
					APIResources: []metav1.APIResource{
						// Since parent GVK was nil, it is NOT returned--only the subresource.
						{
							Name:         "*/other-external-metric",
							SingularName: "",
							Namespaced:   true,
							Group:        "",
							Version:      "",
							Kind:         "MetricValueList",
							Verbs:        []string{"get"},
						},
					},
				},
			},
			expectedFailedGVs: map[schema.GroupVersion]error{},
		},
		{
			name: "Aggregated discovery with single subresource and parent empty GVK",
			agg: apidiscovery.APIGroupDiscoveryList{
				Items: []apidiscovery.APIGroupDiscovery{
					{
						ObjectMeta: metav1.ObjectMeta{
							Name: "external.metrics.k8s.io",
						},
						Versions: []apidiscovery.APIVersionDiscovery{
							{
								Version: "v1beta1",
								Resources: []apidiscovery.APIResourceDiscovery{
									{
										// resilient to empty GVK for parent
										Resource:         "*",
										Scope:            apidiscovery.ScopeNamespace,
										SingularResource: "",
										ResponseKind:     &metav1.GroupVersionKind{},
										Subresources: []apidiscovery.APISubresourceDiscovery{
											{
												Subresource: "other-external-metric",
												ResponseKind: &metav1.GroupVersionKind{
													Kind: "MetricValueList",
												},
												Verbs: []string{"get"},
											},
										},
									},
								},
							},
						},
					},
				},
			},
			expectedGroups: metav1.APIGroupList{
				Groups: []metav1.APIGroup{
					{
						Name: "external.metrics.k8s.io",
						Versions: []metav1.GroupVersionForDiscovery{
							{
								GroupVersion: "external.metrics.k8s.io/v1beta1",
								Version:      "v1beta1",
							},
						},
						PreferredVersion: metav1.GroupVersionForDiscovery{
							GroupVersion: "external.metrics.k8s.io/v1beta1",
							Version:      "v1beta1",
						},
					},
				},
			},
			expectedGVResources: map[schema.GroupVersion]*metav1.APIResourceList{
				{Group: "external.metrics.k8s.io", Version: "v1beta1"}: {
					GroupVersion: "external.metrics.k8s.io/v1beta1",
					APIResources: []metav1.APIResource{
						// Since parent GVK was nil, it is NOT returned--only the subresource.
						{
							Name:         "*/other-external-metric",
							SingularName: "",
							Namespaced:   true,
							Group:        "",
							Version:      "",
							Kind:         "MetricValueList",
							Verbs:        []string{"get"},
						},
					},
				},
			},
			expectedFailedGVs: map[schema.GroupVersion]error{},
		},
		{
			name: "Aggregated discovery with multiple subresources",
			agg: apidiscovery.APIGroupDiscoveryList{
				Items: []apidiscovery.APIGroupDiscovery{
					{
						ObjectMeta: metav1.ObjectMeta{
							Name: "apps",
						},
						Versions: []apidiscovery.APIVersionDiscovery{
							{
								Version: "v1",
								Resources: []apidiscovery.APIResourceDiscovery{
									{
										Resource: "deployments",
										ResponseKind: &metav1.GroupVersionKind{
											Group:   "apps",
											Version: "v1",
											Kind:    "Deployment",
										},
										Scope:            apidiscovery.ScopeNamespace,
										SingularResource: "deployment",
										Subresources: []apidiscovery.APISubresourceDiscovery{
											{
												Subresource: "scale",
												ResponseKind: &metav1.GroupVersionKind{
													Group:   "apps",
													Version: "v1",
													Kind:    "Deployment",
												},
												Verbs: []string{"get", "patch", "update"},
											},
											{
												Subresource: "status",
												ResponseKind: &metav1.GroupVersionKind{
													Group:   "apps",
													Version: "v1",
													Kind:    "Deployment",
												},
												Verbs: []string{"get", "patch", "update"},
											},
										},
									},
								},
							},
						},
					},
				},
			},
			expectedGroups: metav1.APIGroupList{
				Groups: []metav1.APIGroup{
					{
						Name: "apps",
						Versions: []metav1.GroupVersionForDiscovery{
							{
								GroupVersion: "apps/v1",
								Version:      "v1",
							},
						},
						PreferredVersion: metav1.GroupVersionForDiscovery{
							GroupVersion: "apps/v1",
							Version:      "v1",
						},
					},
				},
			},
			expectedGVResources: map[schema.GroupVersion]*metav1.APIResourceList{
				{Group: "apps", Version: "v1"}: {
					GroupVersion: "apps/v1",
					APIResources: []metav1.APIResource{
						{
							Name:         "deployments",
							SingularName: "deployment",
							Namespaced:   true,
							Group:        "apps",
							Version:      "v1",
							Kind:         "Deployment",
						},
						{
							Name:         "deployments/scale",
							SingularName: "deployment",
							Namespaced:   true,
							Group:        "apps",
							Version:      "v1",
							Kind:         "Deployment",
							Verbs:        []string{"get", "patch", "update"},
						},
						{
							Name:         "deployments/status",
							SingularName: "deployment",
							Namespaced:   true,
							Group:        "apps",
							Version:      "v1",
							Kind:         "Deployment",
							Verbs:        []string{"get", "patch", "update"},
						},
					},
				},
			},
			expectedFailedGVs: map[schema.GroupVersion]error{},
		},
		{
			name: "Aggregated discovery: single failed GV at /api",
			agg: apidiscovery.APIGroupDiscoveryList{
				Items: []apidiscovery.APIGroupDiscovery{
					{
						Versions: []apidiscovery.APIVersionDiscovery{
							{
								Version: "v1",
								Resources: []apidiscovery.APIResourceDiscovery{
									{
										Resource: "pods",
										ResponseKind: &metav1.GroupVersionKind{
											Group:   "",
											Version: "v1",
											Kind:    "Pod",
										},
										Scope: apidiscovery.ScopeNamespace,
									},
									{
										Resource: "services",
										ResponseKind: &metav1.GroupVersionKind{
											Group:   "",
											Version: "v1",
											Kind:    "Service",
										},
										Scope: apidiscovery.ScopeNamespace,
									},
								},
								Freshness: apidiscovery.DiscoveryFreshnessStale,
							},
						},
					},
				},
			},
			// Single core Group/Version is stale, so no Version within Group.
			expectedGroups: metav1.APIGroupList{
				Groups: []metav1.APIGroup{{Name: ""}},
			},
			// Single core Group/Version is stale, so there are no expected resources.
			expectedGVResources: map[schema.GroupVersion]*metav1.APIResourceList{},
			expectedFailedGVs: map[schema.GroupVersion]error{
				{Group: "", Version: "v1"}: StaleGroupVersionError{gv: schema.GroupVersion{Group: "", Version: "v1"}},
			},
		},
		{
			name: "Aggregated discovery: single failed GV at /apis",
			agg: apidiscovery.APIGroupDiscoveryList{
				Items: []apidiscovery.APIGroupDiscovery{
					{
						ObjectMeta: metav1.ObjectMeta{
							Name: "apps",
						},
						Versions: []apidiscovery.APIVersionDiscovery{
							{
								Version: "v1",
								Resources: []apidiscovery.APIResourceDiscovery{
									{
										Resource: "deployments",
										ResponseKind: &metav1.GroupVersionKind{
											Group:   "apps",
											Version: "v1",
											Kind:    "Deployment",
										},
										Scope: apidiscovery.ScopeNamespace,
									},
									{
										Resource: "statefulsets",
										ResponseKind: &metav1.GroupVersionKind{
											Group:   "apps",
											Version: "v1",
											Kind:    "StatefulSets",
										},
										Scope: apidiscovery.ScopeNamespace,
									},
								},
								Freshness: apidiscovery.DiscoveryFreshnessStale,
							},
						},
					},
				},
			},
			// Single apps/v1 Group/Version is stale, so no Version within Group.
			expectedGroups: metav1.APIGroupList{
				Groups: []metav1.APIGroup{{Name: "apps"}},
			},
			// Single apps/v1 Group/Version is stale, so there are no expected resources.
			expectedGVResources: map[schema.GroupVersion]*metav1.APIResourceList{},
			expectedFailedGVs: map[schema.GroupVersion]error{
				{Group: "apps", Version: "v1"}: StaleGroupVersionError{gv: schema.GroupVersion{Group: "apps", Version: "v1"}},
			},
		},
		{
			name: "Aggregated discovery: 1 group/2 versions/1 failed GV at /apis",
			agg: apidiscovery.APIGroupDiscoveryList{
				Items: []apidiscovery.APIGroupDiscovery{
					{
						ObjectMeta: metav1.ObjectMeta{
							Name: "apps",
						},
						Versions: []apidiscovery.APIVersionDiscovery{
							// Stale v2 should report failed GV.
							{
								Version: "v2",
								Resources: []apidiscovery.APIResourceDiscovery{
									{
										Resource: "daemonsets",
										ResponseKind: &metav1.GroupVersionKind{
											Group:   "apps",
											Version: "v2",
											Kind:    "DaemonSets",
										},
										Scope: apidiscovery.ScopeNamespace,
									},
								},
								Freshness: apidiscovery.DiscoveryFreshnessStale,
							},
							{
								Version: "v1",
								Resources: []apidiscovery.APIResourceDiscovery{
									{
										Resource: "deployments",
										ResponseKind: &metav1.GroupVersionKind{
											Group:   "apps",
											Version: "v1",
											Kind:    "Deployment",
										},
										Scope: apidiscovery.ScopeNamespace,
									},
								},
							},
						},
					},
				},
			},
			// Only apps/v1 is non-stale expected Group/Version
			expectedGroups: metav1.APIGroupList{
				Groups: []metav1.APIGroup{
					{
						Name: "apps",
						Versions: []metav1.GroupVersionForDiscovery{
							{
								GroupVersion: "apps/v1",
								Version:      "v1",
							},
						},
						// PreferredVersion must be apps/v1
						PreferredVersion: metav1.GroupVersionForDiscovery{
							GroupVersion: "apps/v1",
							Version:      "v1",
						},
					},
				},
			},
			// Only apps/v1 resources expected.
			expectedGVResources: map[schema.GroupVersion]*metav1.APIResourceList{
				{Group: "apps", Version: "v1"}: {
					GroupVersion: "apps/v1",
					APIResources: []metav1.APIResource{
						{
							Name:       "deployments",
							Namespaced: true,
							Group:      "apps",
							Version:    "v1",
							Kind:       "Deployment",
						},
					},
				},
			},
			expectedFailedGVs: map[schema.GroupVersion]error{
				{Group: "apps", Version: "v2"}: StaleGroupVersionError{gv: schema.GroupVersion{Group: "apps", Version: "v2"}},
			},
		},
	}

	for _, test := range tests {
		apiGroups, resourcesByGV, failedGVs := SplitGroupsAndResources(test.agg)
		assert.Equal(t, test.expectedFailedGVs, failedGVs)
		assert.Equal(t, test.expectedGroups, *apiGroups)
		assert.Equal(t, test.expectedGVResources, resourcesByGV)
	}
}
