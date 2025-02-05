/*
Copyright 2016 The Kubernetes Authors.

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
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/stretchr/testify/require"

	apidiscoveryv2 "k8s.io/api/apidiscovery/v2"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextensions "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	runtimeserializer "k8s.io/apimachinery/pkg/runtime/serializer"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	discoveryendpoint "k8s.io/apiserver/pkg/endpoints/discovery/aggregated"
	genericfeatures "k8s.io/apiserver/pkg/features"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	kubernetes "k8s.io/client-go/kubernetes"
	k8sscheme "k8s.io/client-go/kubernetes/scheme"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	apiregistrationv1 "k8s.io/kube-aggregator/pkg/apis/apiregistration/v1"
	aggregator "k8s.io/kube-aggregator/pkg/client/clientset_generated/clientset"
	aggregatorclientsetscheme "k8s.io/kube-aggregator/pkg/client/clientset_generated/clientset/scheme"
	kubeapiservertesting "k8s.io/kubernetes/cmd/kube-apiserver/app/testing"

	"k8s.io/kubernetes/test/integration/framework"
)

type kubeClientSet = kubernetes.Interface

type aggegatorClientSet = aggregator.Interface

type apiextensionsClientSet = apiextensions.Interface

type dynamicClientset = dynamic.Interface
type testClientSet struct {
	kubeClientSet
	aggegatorClientSet
	apiextensionsClientSet
	dynamicClientset
}

var _ testClient = testClientSet{}

func (t testClientSet) Discovery() discovery.DiscoveryInterface {
	return t.kubeClientSet.Discovery()
}

var (
	scheme    = runtime.NewScheme()
	codecs    = runtimeserializer.NewCodecFactory(scheme)
	serialize runtime.NegotiatedSerializer

	basicTestGroup = apidiscoveryv2.APIGroupDiscovery{
		ObjectMeta: metav1.ObjectMeta{
			Name: "stable.example.com",
		},
		Versions: []apidiscoveryv2.APIVersionDiscovery{
			{
				Version: "v1",
				Resources: []apidiscoveryv2.APIResourceDiscovery{
					{
						Resource:   "jobs",
						Verbs:      []string{"create", "list", "watch", "delete"},
						ShortNames: []string{"jz"},
						Categories: []string{"all"},
					},
				},
				Freshness: apidiscoveryv2.DiscoveryFreshnessCurrent,
			},
		},
	}

	basicTestGroupWithFixup = apidiscoveryv2.APIGroupDiscovery{
		ObjectMeta: metav1.ObjectMeta{
			Name: "stable.example.com",
		},
		Versions: []apidiscoveryv2.APIVersionDiscovery{
			{
				Version: "v1",
				Resources: []apidiscoveryv2.APIResourceDiscovery{
					{
						Resource:   "jobs",
						Verbs:      []string{"create", "list", "watch", "delete"},
						ShortNames: []string{"jz"},
						Categories: []string{"all"},
						// aggregator will populate this with a non-nil value
						ResponseKind: &metav1.GroupVersionKind{},
					},
				},
				Freshness: apidiscoveryv2.DiscoveryFreshnessCurrent,
			},
		},
	}

	basicTestGroupStale = apidiscoveryv2.APIGroupDiscovery{
		ObjectMeta: metav1.ObjectMeta{
			Name: "stable.example.com",
		},
		Versions: []apidiscoveryv2.APIVersionDiscovery{
			{
				Version:   "v1",
				Freshness: apidiscoveryv2.DiscoveryFreshnessStale,
			},
		},
	}

	stableGroup    = "stable.example.com"
	stableV1       = metav1.GroupVersion{Group: stableGroup, Version: "v1"}
	stableV1alpha1 = metav1.GroupVersion{Group: stableGroup, Version: "v1alpha1"}
	stableV1alpha2 = metav1.GroupVersion{Group: stableGroup, Version: "v1alpha2"}
	stableV1beta1  = metav1.GroupVersion{Group: stableGroup, Version: "v1beta1"}
	stableV2       = metav1.GroupVersion{Group: stableGroup, Version: "v2"}
)

func init() {
	// Add all builtin types to scheme
	utilruntime.Must(k8sscheme.AddToScheme(scheme))
	utilruntime.Must(aggregatorclientsetscheme.AddToScheme(scheme))
	utilruntime.Must(apiextensionsv1.AddToScheme(scheme))

	info, ok := runtime.SerializerInfoForMediaType(codecs.SupportedMediaTypes(), runtime.ContentTypeJSON)
	if !ok {
		panic("failed to create serializer info")
	}

	serialize = runtime.NewSimpleNegotiatedSerializer(info)
}

// Spins up an api server which is cleaned up at the end up the test
// Returns some kubernetes clients
func setup(t *testing.T) (context.Context, testClientSet, context.CancelFunc) {
	ctx, cancelCtx := context.WithCancel(context.Background())

	server := kubeapiservertesting.StartTestServerOrDie(t, nil, framework.DefaultTestServerFlags(), framework.SharedEtcd())
	t.Cleanup(server.TearDownFn)

	kubeClientSet, err := kubernetes.NewForConfig(server.ClientConfig)
	require.NoError(t, err)

	aggegatorClientSet, err := aggregator.NewForConfig(server.ClientConfig)
	require.NoError(t, err)

	apiextensionsClientSet, err := apiextensions.NewForConfig(server.ClientConfig)
	require.NoError(t, err)

	dynamicClientset, err := dynamic.NewForConfig(server.ClientConfig)
	require.NoError(t, err)

	client := testClientSet{
		kubeClientSet:          kubeClientSet,
		aggegatorClientSet:     aggegatorClientSet,
		apiextensionsClientSet: apiextensionsClientSet,
		dynamicClientset:       dynamicClientset,
	}
	return ctx, client, cancelCtx
}

func TestReadinessAggregatedAPIServiceDiscovery(t *testing.T) {
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, genericfeatures.AggregatedDiscoveryEndpoint, true)

	// Keep any goroutines spawned from running past the execution of this test
	ctx, client, cleanup := setup(t)
	defer cleanup()

	// Create a resource manager which serves our GroupVersion
	resourceManager := discoveryendpoint.NewResourceManager("apis")
	resourceManager.SetGroups([]apidiscoveryv2.APIGroupDiscovery{basicTestGroup})

	apiServiceWaitCh := make(chan struct{})

	// Install our ResourceManager as an Aggregated APIService to the
	// test server
	service := NewFakeService("test-server", client, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/apis/stable.example.com") {
			// Return invalid response so APIService can be marked as "available"
			w.WriteHeader(http.StatusOK)
		} else if strings.HasPrefix(r.URL.Path, "/apis") {
			select {
			case <-apiServiceWaitCh:
				// Hang responding to discovery until aggregated discovery document contains the aggregated group marked as Stale.
				resourceManager.ServeHTTP(w, r)
			case <-ctx.Done():
				return
			}
		} else {
			// reject openapi/v2, openapi/v3, apis/<group>/<version>
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	go func() {
		require.NoError(t, service.Run(ctx))
	}()
	require.NoError(t, service.WaitForReady(ctx))

	// For each groupversion served by our resourcemanager, create an APIService
	// object connected to our fake APIServer
	for _, versionInfo := range basicTestGroup.Versions {
		groupVersion := metav1.GroupVersion{
			Group:   basicTestGroup.Name,
			Version: versionInfo.Version,
		}

		require.NoError(t, registerAPIService(ctx, client, groupVersion, service))
	}

	// Keep repeatedly fetching document from aggregator.
	// Check to see if it initially contains the aggregated group as stale
	require.NoError(t, WaitForGroups(ctx, client, basicTestGroupStale))
	require.NoError(t, WaitForRootPaths(t, ctx, client, sets.New("/apis/"+basicTestGroup.Name), nil))

	// Allow the APIService to start responding and ensure that Freshness is updated when the APIService is reachable.
	close(apiServiceWaitCh)
	require.NoError(t, WaitForGroups(ctx, client, basicTestGroupWithFixup))
}

func registerAPIService(ctx context.Context, client aggregator.Interface, gv metav1.GroupVersion, service FakeService) error {
	port := service.Port()
	if port == nil {
		return errors.New("service not yet started")
	}
	// Register the APIService
	patch := apiregistrationv1.APIService{
		ObjectMeta: metav1.ObjectMeta{
			Name: gv.Version + "." + gv.Group,
		},
		TypeMeta: metav1.TypeMeta{
			Kind:       "APIService",
			APIVersion: "apiregistration.k8s.io/v1",
		},
		Spec: apiregistrationv1.APIServiceSpec{
			Group:                 gv.Group,
			Version:               gv.Version,
			InsecureSkipTLSVerify: true,
			GroupPriorityMinimum:  1000,
			VersionPriority:       15,
			Service: &apiregistrationv1.ServiceReference{
				Namespace: "default",
				Name:      service.Name(),
				Port:      port,
			},
		},
	}

	_, err := client.
		ApiregistrationV1().
		APIServices().
		Create(context.TODO(), &patch, metav1.CreateOptions{FieldManager: "test-manager"})
	return err
}

func unregisterAPIService(ctx context.Context, client aggregator.Interface, gv metav1.GroupVersion) error {
	return client.ApiregistrationV1().APIServices().Delete(ctx, gv.Version+"."+gv.Group, metav1.DeleteOptions{})
}

func TestAggregatedAPIServiceDiscovery(t *testing.T) {
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, genericfeatures.AggregatedDiscoveryEndpoint, true)

	// Keep any goroutines spawned from running past the execution of this test
	ctx, client, cleanup := setup(t)
	defer cleanup()

	// Create a resource manager which serves our GroupVersion
	resourceManager := discoveryendpoint.NewResourceManager("apis")
	resourceManager.SetGroups([]apidiscoveryv2.APIGroupDiscovery{basicTestGroup})

	// Install our ResourceManager as an Aggregated APIService to the
	// test server
	service := NewFakeService("test-server", client, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/apis") {
			resourceManager.ServeHTTP(w, r)
		} else if strings.HasPrefix(r.URL.Path, "/apis/stable.example.com") {
			// Return invalid response so APIService can be marked as "available"
			w.WriteHeader(http.StatusOK)
		} else {
			// reject openapi/v2, openapi/v3, apis/<group>/<version>
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	go func() {
		require.NoError(t, service.Run(ctx))
	}()
	require.NoError(t, service.WaitForReady(ctx))

	// For each groupversion served by our resourcemanager, create an APIService
	// object connected to our fake APIServer
	var groupVersions []metav1.GroupVersion
	for _, versionInfo := range basicTestGroup.Versions {
		groupVersion := metav1.GroupVersion{
			Group:   basicTestGroup.Name,
			Version: versionInfo.Version,
		}

		require.NoError(t, registerAPIService(ctx, client, groupVersion, service))
		groupVersions = append(groupVersions, groupVersion)
	}

	// Keep repeatedly fetching document from aggregator.
	// Check to see if it contains our service within a reasonable amount of time
	require.NoError(t, WaitForGroups(ctx, client, basicTestGroupWithFixup))
	require.NoError(t, WaitForRootPaths(t, ctx, client, sets.New("/apis/"+basicTestGroup.Name), nil))

	// Unregister and ensure the group gets dropped from root paths
	for _, groupVersion := range groupVersions {
		require.NoError(t, unregisterAPIService(ctx, client, groupVersion))
	}
	require.NoError(t, WaitForRootPaths(t, ctx, client, nil, sets.New("/apis/"+basicTestGroup.Name)))
}

func runTestCases(t *testing.T, cases []testCase) {
	// Keep any goroutines spawned from running past the execution of this test
	ctx, client, cleanup := setup(t)
	defer cleanup()

	// Fetch the original discovery information so we can wait for it to
	// reset between tests
	originalV1, err := FetchV1DiscoveryGroups(ctx, client)
	require.NoError(t, err)

	originalV2, err := FetchV2Discovery(ctx, client)
	require.NoError(t, err)

	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			func() {
				testContext, testDone := context.WithCancel(ctx)
				defer testDone()

				for i, a := range c.Actions {
					if cleaning, ok := a.(cleaningAction); ok {
						defer func() {
							require.NoError(t, cleaning.Cleanup(testContext, client), "cleanup after \"%T\" step %v", a, i)
						}()
					}
					require.NoError(t, a.Do(testContext, client), "running \"%T\" step %v", a, i)
				}
			}()

			var diff string
			err := WaitForV1GroupsWithCondition(ctx, client, func(result metav1.APIGroupList) bool {
				diff = cmp.Diff(originalV1, result)
				return reflect.DeepEqual(result, originalV1)
			})
			require.NoError(t, err, "v1 discovery must reset between tests: "+diff)

			err = WaitForResultWithCondition(ctx, client, func(result apidiscoveryv2.APIGroupDiscoveryList) bool {
				diff = cmp.Diff(originalV2, result)
				return reflect.DeepEqual(result, originalV2)
			})
			require.NoError(t, err, "v2 discovery must reset between tests: "+diff)
		})
	}
}

// Declarative tests targeting CRD integration
func TestCRD(t *testing.T) {
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, genericfeatures.AggregatedDiscoveryEndpoint, true)

	runTestCases(t, []testCase{
		{
			// Show that when a CRD is added it gets included on the discovery doc
			// within a reasonable amount of time
			Name: "CRDInclusion",
			Actions: []testAction{
				applyCRD(makeCRDSpec(stableGroup, "Foo", false, []string{"v1", "v1alpha1", "v1beta1", "v2"})),
				waitForGroupVersionsV1([]metav1.GroupVersion{stableV1, stableV1alpha1, stableV1beta1, stableV2}),
				waitForGroupVersionsV2([]metav1.GroupVersion{stableV1, stableV1alpha1, stableV1beta1, stableV2}),
				waitForGroupVersionsV2Beta1([]metav1.GroupVersion{stableV1, stableV1alpha1, stableV1beta1, stableV2}),
			},
		},
		{
			// Show that a CRD added to the discovery doc can also be removed
			Name: "CRDRemoval",
			Actions: []testAction{
				applyCRD(makeCRDSpec(stableGroup, "Foo", false, []string{"v1", "v1alpha1", "v1beta1", "v2"})),
				waitForGroupVersionsV1([]metav1.GroupVersion{stableV1, stableV1alpha1, stableV1beta1, stableV2}),
				waitForGroupVersionsV2([]metav1.GroupVersion{stableV1, stableV1alpha1, stableV1beta1, stableV2}),
				waitForGroupVersionsV2Beta1([]metav1.GroupVersion{stableV1, stableV1alpha1, stableV1beta1, stableV2}),
				deleteObject{
					GroupVersionResource: metav1.GroupVersionResource(apiextensionsv1.SchemeGroupVersion.WithResource("customresourcedefinitions")),
					Name:                 "foos.stable.example.com",
				},
				waitForAbsentGroupVersionsV1([]metav1.GroupVersion{stableV1, stableV1alpha1, stableV1beta1, stableV2}),
				waitForAbsentGroupVersionsV2([]metav1.GroupVersion{stableV1, stableV1alpha1, stableV1beta1, stableV2}),
			},
		},
		{
			// Show that if CRD and APIService share a groupversion, and the
			// APIService is deleted, and CRD updated, the APIService remains in
			// discovery.
			// This test simulates a resync of CRD controller to show that eventually
			// APIService is recreated
			Name: "CRDAPIServiceOverlap",
			Actions: []testAction{
				applyAPIService(
					apiregistrationv1.APIServiceSpec{
						Group:                 stableGroup,
						Version:               "v1",
						InsecureSkipTLSVerify: true,
						GroupPriorityMinimum:  int32(1000),
						VersionPriority:       int32(15),
						Service: &apiregistrationv1.ServiceReference{
							Name:      "unused",
							Namespace: "default",
						},
					},
				),

				// Wait for GV to appear in both discovery documents
				waitForGroupVersionsV1([]metav1.GroupVersion{stableV1}),
				waitForGroupVersionsV2([]metav1.GroupVersion{stableV1}),

				applyCRD(makeCRDSpec(stableGroup, "Bar", false, []string{"v1", "v2"})),

				// Show that we have v1 and v2 but v1 is stale
				waitForGroupVersionsV1([]metav1.GroupVersion{stableV1, stableV2}),
				waitForStaleGroupVersionsV2([]metav1.GroupVersion{stableV1}),
				waitForFreshGroupVersionsV2([]metav1.GroupVersion{stableV2}),

				// Delete APIService shared by the aggregated apiservice and
				// CRD
				deleteObject{
					GroupVersionResource: metav1.GroupVersionResource(apiregistrationv1.SchemeGroupVersion.WithResource("apiservices")),
					Name:                 "v1.stable.example.com",
				},

				// Update CRD to trigger a resync by adding a category and new groupversion
				applyCRD(makeCRDSpec(stableGroup, "Bar", false, []string{"v1", "v2", "v1alpha1"}, "all")),

				// Show that the groupversion is re-added back
				waitForGroupVersionsV1([]metav1.GroupVersion{stableV1, stableV2, stableV1alpha1}),
				waitForFreshGroupVersionsV2([]metav1.GroupVersion{stableV1, stableV2, stableV1alpha1}),
			},
		},
		{
			// Show that if CRD and Aggregated APIservice share a groupversiom,
			// The aggregated apiservice's discovery information is shown in both
			// v1 and v2 discovery
			Name: "CRDAPIServiceSameGroupDifferentVersions",
			Actions: []testAction{
				// Wait for CRD to apply
				applyCRD(makeCRDSpec(stableGroup, "Bar", false, []string{"v2", "v1alpha1"})),
				// Wait for GV to appear in both discovery documents
				waitForGroupVersionsV1([]metav1.GroupVersion{stableV2, stableV1alpha1}),
				waitForGroupVersionsV2([]metav1.GroupVersion{stableV2, stableV1alpha1}),
				waitForGroupVersionsV2Beta1([]metav1.GroupVersion{stableV2, stableV1alpha1}),

				applyAPIService(
					apiregistrationv1.APIServiceSpec{
						Group:                 stableGroup,
						Version:               "v1",
						InsecureSkipTLSVerify: true,
						GroupPriorityMinimum:  int32(1000),
						VersionPriority:       int32(100),
						Service: &apiregistrationv1.ServiceReference{
							Name:      "unused",
							Namespace: "default",
						},
					},
				),

				// We should now have stable v1 available
				waitForGroupVersionsV1([]metav1.GroupVersion{stableV1}),
				waitForGroupVersionsV2([]metav1.GroupVersion{stableV1}),
				waitForGroupVersionsV2Beta1([]metav1.GroupVersion{stableV1}),

				// The CRD group-versions not served by the aggregated
				// apiservice should still be available
				waitForGroupVersionsV1([]metav1.GroupVersion{stableV2, stableV1alpha1}),
				waitForGroupVersionsV2([]metav1.GroupVersion{stableV2, stableV1alpha1}),
				waitForGroupVersionsV2Beta1([]metav1.GroupVersion{stableV2, stableV1alpha1}),

				// Remove API service. Show we have switched to CRD
				deleteObject{
					GroupVersionResource: metav1.GroupVersionResource(apiregistrationv1.SchemeGroupVersion.WithResource("apiservices")),
					Name:                 "v1.stable.example.com",
				},

				// Show that we still have stable v1 since it is in the CRD
				waitForGroupVersionsV1([]metav1.GroupVersion{stableV2, stableV1alpha1}),
				waitForGroupVersionsV2([]metav1.GroupVersion{stableV2, stableV1alpha1}),
				waitForGroupVersionsV2Beta1([]metav1.GroupVersion{stableV2, stableV1alpha1}),

				waitForAbsentGroupVersionsV1([]metav1.GroupVersion{stableV1}),
				waitForAbsentGroupVersionsV2([]metav1.GroupVersion{stableV1}),
				waitForAbsentGroupVersionsV2Beta1([]metav1.GroupVersion{stableV1}),
			},
		},
		{
			// Show that if CRD and a builtin share a group version,
			// the builtin takes precedence in both versions of discovery
			Name: "CRDBuiltinOverlapPrecedence",
			Actions: []testAction{
				// Create CRD that overrides a builtin
				applyCRD(makeCRDSpec("apiextensions.k8s.io", "Bar", true, []string{"v1", "v2", "vfake"})),

				waitForGroupVersionsV1([]metav1.GroupVersion{{Group: "apiextensions.k8s.io", Version: "vfake"}}),
				waitForGroupVersionsV2([]metav1.GroupVersion{{Group: "apiextensions.k8s.io", Version: "vfake"}}),

				// Show that the builtin group-version is still used for V1
				// By showing presence of v1.CustomResourceDefinition
				// and absence of v1.Bar
				waitForResourcesV1([]metav1.GroupVersionResource{
					{
						Group:    "apiextensions.k8s.io",
						Version:  "v1",
						Resource: "customresourcedefinitions",
					},
					{
						Group:    "apiextensions.k8s.io",
						Version:  "vfake",
						Resource: "bars",
					},
				}),
				waitForResourcesV2([]metav1.GroupVersionResource{
					{
						Group:    "apiextensions.k8s.io",
						Version:  "v1",
						Resource: "customresourcedefinitions",
					},
					{
						Group:    "apiextensions.k8s.io",
						Version:  "vfake",
						Resource: "bars",
					},
				}),

				waitForResourcesAbsentV1([]metav1.GroupVersionResource{
					{
						Group:    "apiextensions.k8s.io",
						Version:  "v1",
						Resource: "bars",
					},
				}),
				waitForResourcesAbsentV2([]metav1.GroupVersionResource{
					{
						Group:    "apiextensions.k8s.io",
						Version:  "v1",
						Resource: "bars",
					},
				}),
			},
		},
		{
			// Tests that a race discovered during alpha phase of the feature is fixed.
			// Rare race would occur if a CRD was synced before the removal of an aggregated
			// APIService could be synced.
			// To test this we:
			//  1. Add CRD to apiserver
			// 	2. Wait for it to sync
			//  3. Add aggregated APIService with same groupversion
			//  4. Remove aggregated apiservice
			//  5. Check that we have CRD GVs in discovery document
			// Show that if CRD and APIService share a groupversion, and the
			// APIService is deleted, and CRD updated, the groupversion from
			// the CRD remains in discovery.
			Name: "Race",
			Actions: []testAction{
				// Create CRD with the same GV as the aggregated APIService
				applyCRD(makeCRDSpec(stableGroup, "Bar", false, []string{"v1", "v2"})),

				// only CRD has stable v2,  this will show that CRD has been synced
				waitForGroupVersionsV1([]metav1.GroupVersion{stableV1, stableV2}),
				waitForGroupVersionsV2([]metav1.GroupVersion{stableV1, stableV2}),

				// Add Aggregated APIService that overlaps the CRD.
				applyAPIService(
					apiregistrationv1.APIServiceSpec{
						Group:                 stableGroup,
						Version:               "v1",
						InsecureSkipTLSVerify: true,
						GroupPriorityMinimum:  int32(1000),
						VersionPriority:       int32(100),
						Service: &apiregistrationv1.ServiceReference{
							Name:      "fake",
							Namespace: "default",
						},
					},
				),

				// Delete APIService shared by the aggregated apiservice and
				// CRD
				deleteObject{
					GroupVersionResource: metav1.GroupVersionResource(apiregistrationv1.SchemeGroupVersion.WithResource("apiservices")),
					Name:                 "v1.stable.example.com",
				},

				// Show the CRD (with stablev2) is the one which is now advertised
				waitForGroupVersionsV1([]metav1.GroupVersion{stableV1, stableV2}),
				waitForGroupVersionsV2([]metav1.GroupVersion{stableV1, stableV2}),
			},
		},
	})
}

func TestFreshness(t *testing.T) {
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, genericfeatures.AggregatedDiscoveryEndpoint, true)

	requireStaleGVs := func(gvs ...metav1.GroupVersion) inlineAction {
		return inlineAction(func(ctx context.Context, client testClient) error {
			document, err := FetchV2Discovery(ctx, client)
			if err != nil {
				return nil
			}

			// Track the stale gvs in array for nice diff output upon test failure
			staleGVs := []metav1.GroupVersion{}

			// Iterate through input so order does not matter
			for _, targetGv := range gvs {
				entry := FindGroupVersionV2(document, targetGv)
				if entry == nil {
					continue
				}

				switch entry.Freshness {
				case apidiscoveryv2.DiscoveryFreshnessCurrent:
					// Skip
				case apidiscoveryv2.DiscoveryFreshnessStale:
					staleGVs = append(staleGVs, targetGv)
				default:
					return fmt.Errorf("unrecognized freshness '%v' on gv '%v'", entry.Freshness, targetGv)
				}
			}

			if !(len(staleGVs) == 0 && len(gvs) == 0) && !reflect.DeepEqual(staleGVs, gvs) {
				diff := cmp.Diff(staleGVs, gvs)
				return fmt.Errorf("expected sets of stale gvs to be equal:\n%v", diff)
			}

			return nil
		})
	}

	runTestCases(t, []testCase{
		{
			Name: "BuiltinsFresh",
			Actions: []testAction{
				// Wait for discovery ready
				waitForGroupVersionsV2{metav1.GroupVersion(apiregistrationv1.SchemeGroupVersion)},
				// Require there are no stale groupversions and no unrecognized
				// GVs
				requireStaleGVs(),
			},
		},
		{
			// CRD freshness is always current
			Name: "CRDFresh",
			Actions: []testAction{
				// Add a CRD and wait for it to appear in discovery
				applyCRD(makeCRDSpec(stableGroup, "Foo", false, []string{"v1", "v1alpha1", "v1beta1", "v2"})),
				waitForGroupVersionsV1([]metav1.GroupVersion{stableV1, stableV1alpha1, stableV1beta1, stableV2}),
				waitForGroupVersionsV2([]metav1.GroupVersion{stableV1, stableV1alpha1, stableV1beta1, stableV2}),

				// Test CRD is current by requiring there is nothing stale
				requireStaleGVs(),
			},
		},
		{
			// Make an aggregated APIService that's unreachable and show
			// that its groupversion is included in the discovery document as
			// stale
			Name: "AggregatedUnreachable",
			Actions: []testAction{
				applyAPIService{
					Group:                stableGroup,
					Version:              "v1",
					GroupPriorityMinimum: 1000,
					VersionPriority:      15,
					Service: &apiregistrationv1.ServiceReference{
						Name:      "doesnt-exist",
						Namespace: "default",
					},
				},
				waitForGroupVersionsV2([]metav1.GroupVersion{stableV1}),
				// Require there is one and only one stale GV and it is stableV1
				requireStaleGVs(stableV1),
			},
		},
	})

}

// Shows a group for which multiple APIServices specify a GroupPriorityMinimum,
// it is sorted the same in both versions of discovery
func TestGroupPriority(t *testing.T) {
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, genericfeatures.AggregatedDiscoveryEndpoint, true)

	makeApiServiceSpec := func(gv metav1.GroupVersion, groupPriorityMin, versionPriority int) apiregistrationv1.APIServiceSpec {
		return apiregistrationv1.APIServiceSpec{
			Group:                 gv.Group,
			Version:               gv.Version,
			InsecureSkipTLSVerify: true,
			GroupPriorityMinimum:  int32(groupPriorityMin),
			VersionPriority:       int32(versionPriority),
			Service: &apiregistrationv1.ServiceReference{
				Name:      "unused",
				Namespace: "default",
			},
		}
	}

	checkGVOrder := inlineAction(func(ctx context.Context, client testClient) (err error) {
		// Fetch v1 document and v2 document, and ensure they have
		// equal orderings of groupversions. and nothing missing or
		// extra.
		v1GroupsAndVersions, err := FetchV1DiscoveryGroups(ctx, client)
		if err != nil {
			return err
		}
		v2GroupsAndVersions, err := FetchV2Discovery(ctx, client)
		if err != nil {
			return err
		}

		v1Gvs := []metav1.GroupVersion{}
		v2Gvs := []metav1.GroupVersion{}

		for _, group := range v1GroupsAndVersions.Groups {
			for _, version := range group.Versions {
				v1Gvs = append(v1Gvs, metav1.GroupVersion{
					Group:   group.Name,
					Version: version.Version,
				})
			}
		}

		for _, group := range v2GroupsAndVersions.Items {
			for _, version := range group.Versions {
				v2Gvs = append(v2Gvs, metav1.GroupVersion{
					Group:   group.Name,
					Version: version.Version,
				})
			}
		}

		if !reflect.DeepEqual(v1Gvs, v2Gvs) {
			return fmt.Errorf("expected equal orderings and lists of groupversions in both v1 and v2 discovery:\n%v", cmp.Diff(v1Gvs, v2Gvs))
		}

		return nil
	})

	runTestCases(t, []testCase{
		{
			// Show that the legacy and aggregated discovery docs have the same
			// set of builtin groupversions
			Name: "BuiltinsAndOrdering",
			Actions: []testAction{
				waitForGroupVersionsV1{metav1.GroupVersion(apiregistrationv1.SchemeGroupVersion)},
				waitForGroupVersionsV2{metav1.GroupVersion(apiregistrationv1.SchemeGroupVersion)},
				checkGVOrder,
			},
		},
		{
			// Show that a very high priority group is sorted first (below apiregistration v1)
			// Also show the ordering is same for both v1 and v2 discovery apis
			// Does not vary version priority
			Name: "HighGroupPriority",
			Actions: []testAction{
				// A VERY high priority which should take precedence
				// 20000 is highest possible priority
				applyAPIService(makeApiServiceSpec(stableV1, 20000, 15)),
				// A VERY low priority which should be ignored
				applyAPIService(makeApiServiceSpec(stableV1alpha1, 1, 15)),
				// A medium-high priority (that conflicts with k8s) which should be ignored
				applyAPIService(makeApiServiceSpec(stableV1alpha2, 17300, 15)),
				// Wait for all the added group-versions to appear in both discovery documents
				waitForGroupVersionsV1([]metav1.GroupVersion{stableV1, stableV1alpha1, stableV1alpha2}),
				waitForGroupVersionsV2([]metav1.GroupVersion{stableV1, stableV1alpha1, stableV1alpha2}),
				// Check that both v1 and v2 endpoints have exactly the same
				// sets of groupversions
				checkGVOrder,
				// Check that the first group-version is the one with the highest
				// priority
				inlineAction(func(ctx context.Context, client testClient) error {
					v2GroupsAndVersions, err := FetchV2Discovery(ctx, client)
					if err != nil {
						return err
					}

					// First group should always be apiregistration.k8s.io
					secondGV := metav1.GroupVersion{
						Group:   v2GroupsAndVersions.Items[1].Name,
						Version: v2GroupsAndVersions.Items[1].Versions[0].Version,
					}

					if !reflect.DeepEqual(&stableV1, &secondGV) {
						return fmt.Errorf("expected second group's first version to be %v, not %v", stableV1, secondGV)
					}

					return nil
				}),
			},
		},
		{
			// Show that a very low group priority is ordered last
			Name: "LowGroupPriority",
			Actions: []testAction{
				// A minimal priority
				applyAPIService(makeApiServiceSpec(stableV1alpha1, 1, 15)),
				// Wait for all the added group-versions to appear in v2 discovery
				waitForGroupVersionsV2([]metav1.GroupVersion{stableV1alpha1}),
				// Check that the last group-version is the one with the lowest
				// priority
				inlineAction(func(ctx context.Context, client testClient) error {
					v2GroupsAndVersions, err := FetchV2Discovery(ctx, client)
					if err != nil {
						return err
					}
					lastGroup := v2GroupsAndVersions.Items[len(v2GroupsAndVersions.Items)-1]

					lastGV := metav1.GroupVersion{
						Group:   lastGroup.Name,
						Version: lastGroup.Versions[0].Version,
					}

					if !reflect.DeepEqual(&stableV1alpha1, &lastGV) {
						return fmt.Errorf("expected last group to be %v, not %v", stableV1alpha1, lastGV)
					}

					return nil
				}),
				// Wait for all the added group-versions to appear in both discovery documents
				waitForGroupVersionsV1([]metav1.GroupVersion{stableV1alpha1}),
				// Check that both v1 and v2 endpoints have exactly the same
				// sets of groupversions
				checkGVOrder,
			},
		},
		{
			// Show that versions within a group are sorted by priority
			Name: "VersionPriority",
			Actions: []testAction{
				applyAPIService(makeApiServiceSpec(stableV1, 1000, 2)),
				applyAPIService(makeApiServiceSpec(stableV1alpha1, 1000, 1)),
				applyAPIService(makeApiServiceSpec(stableV1alpha2, 1000, 3)),
				// Wait for all the added group-versions to appear in both discovery documents
				waitForGroupVersionsV1([]metav1.GroupVersion{stableV1, stableV1alpha1, stableV1alpha2}),
				waitForGroupVersionsV2([]metav1.GroupVersion{stableV1, stableV1alpha1, stableV1alpha2}),
				// Check that both v1 and v2 endpoints have exactly the same
				// sets of groupversions
				checkGVOrder,
				inlineAction(func(ctx context.Context, client testClient) error {
					// Find the entry for stable.example.com
					// and show the versions are ordered how we expect
					v2GroupsAndVersions, err := FetchV2Discovery(ctx, client)
					if err != nil {
						return err
					}

					// Should be ordered last for this test
					group := v2GroupsAndVersions.Items[len(v2GroupsAndVersions.Items)-1]
					if group.Name != stableGroup {
						return fmt.Errorf("group is not where we expect: found %v, expected %v", group.Name, stableGroup)
					}

					versionOrder := []string{}
					for _, version := range group.Versions {
						versionOrder = append(versionOrder, version.Version)
					}

					expectedOrder := []string{
						stableV1alpha2.Version,
						stableV1.Version,
						stableV1alpha1.Version,
					}

					if !reflect.DeepEqual(expectedOrder, versionOrder) {
						return fmt.Errorf("version in wrong order: %v", cmp.Diff(expectedOrder, versionOrder))
					}

					return nil
				}),
			},
		},
		{
			// Show that versions within a group are sorted by priority
			// and that equal versions will be sorted by a kube-aware version
			// comparator
			Name: "VersionPriorityTiebreaker",
			Actions: []testAction{
				applyAPIService(makeApiServiceSpec(stableV1, 1000, 15)),
				applyAPIService(makeApiServiceSpec(stableV1alpha1, 1000, 15)),
				applyAPIService(makeApiServiceSpec(stableV1alpha2, 1000, 15)),
				applyAPIService(makeApiServiceSpec(stableV1beta1, 1000, 15)),
				applyAPIService(makeApiServiceSpec(stableV2, 1000, 15)),
				// Wait for all the added group-versions to appear in both discovery documents
				waitForGroupVersionsV1([]metav1.GroupVersion{stableV1, stableV1alpha1, stableV1alpha2, stableV1beta1, stableV2}),
				waitForGroupVersionsV2([]metav1.GroupVersion{stableV1, stableV1alpha1, stableV1alpha2, stableV1beta1, stableV2}),
				// Check that both v1 and v2 endpoints have exactly the same
				// sets of groupversions
				checkGVOrder,
				inlineAction(func(ctx context.Context, client testClient) error {
					// Find the entry for stable.example.com
					// and show the versions are ordered how we expect
					v2GroupsAndVersions, err := FetchV2Discovery(ctx, client)
					if err != nil {
						return err
					}

					// Should be ordered last for this test
					group := v2GroupsAndVersions.Items[len(v2GroupsAndVersions.Items)-1]
					if group.Name != stableGroup {
						return fmt.Errorf("group is not where we expect: found %v, expected %v", group.Name, stableGroup)
					}

					versionOrder := []string{}
					for _, version := range group.Versions {
						versionOrder = append(versionOrder, version.Version)
					}

					expectedOrder := []string{
						stableV2.Version,
						stableV1.Version,
						stableV1beta1.Version,
						stableV1alpha2.Version,
						stableV1alpha1.Version,
					}

					if !reflect.DeepEqual(expectedOrder, versionOrder) {
						return fmt.Errorf("version in wrong order: %v", cmp.Diff(expectedOrder, versionOrder))
					}

					return nil
				}),
			},
		},
	})
}

func TestSingularNames(t *testing.T) {
	server := kubeapiservertesting.StartTestServerOrDie(t, nil, []string{"--runtime-config=api/all=true"}, framework.SharedEtcd())
	t.Cleanup(server.TearDownFn)

	kubeClientSet, err := kubernetes.NewForConfig(server.ClientConfig)
	require.NoError(t, err)

	_, resources, err := kubeClientSet.Discovery().ServerGroupsAndResources()
	require.NoError(t, err)

	for _, rr := range resources {
		for _, r := range rr.APIResources {
			if strings.Contains(r.Name, "/") {
				continue
			}
			if r.SingularName == "" {
				t.Errorf("missing singularName for resource %q in %q", r.Name, rr.GroupVersion)
				continue
			}
			if r.SingularName != strings.ToLower(r.Kind) {
				t.Errorf("expected singularName for resource %q in %q to be %q, got %q", r.Name, rr.GroupVersion, strings.ToLower(r.Kind), r.SingularName)
				continue
			}
		}
	}
}

func makeCRDSpec(group string, kind string, namespaced bool, versions []string, categories ...string) apiextensionsv1.CustomResourceDefinitionSpec {
	scope := apiextensionsv1.NamespaceScoped
	if !namespaced {
		scope = apiextensionsv1.ClusterScoped
	}

	plural, singular := meta.UnsafeGuessKindToResource(schema.GroupVersionKind{Kind: kind})
	res := apiextensionsv1.CustomResourceDefinitionSpec{
		Group: group,
		Scope: scope,
		Names: apiextensionsv1.CustomResourceDefinitionNames{
			Plural:     plural.Resource,
			Singular:   singular.Resource,
			Kind:       kind,
			Categories: categories,
		},
	}

	for i, version := range versions {
		res.Versions = append(res.Versions, apiextensionsv1.CustomResourceDefinitionVersion{
			Name:    version,
			Served:  true,
			Storage: i == 0,
			Schema: &apiextensionsv1.CustomResourceValidation{
				OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
					Type: "object",
					Properties: map[string]apiextensionsv1.JSONSchemaProps{
						"data": {
							Type: "string",
						},
					},
				},
			},
		})
	}
	return res
}
