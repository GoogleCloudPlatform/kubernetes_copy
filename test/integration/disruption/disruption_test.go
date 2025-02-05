/*
Copyright 2019 The Kubernetes Authors.

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

package disruption

import (
	"bytes"
	"context"
	"fmt"
	"path"
	"reflect"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	clientv3 "go.etcd.io/etcd/client/v3"
	v1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/api/policy/v1beta1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextensionsclientset "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	"k8s.io/apiextensions-apiserver/test/integration/fixtures"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer/protobuf"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
	genericapirequest "k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/registry/rest"
	cacheddiscovery "k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/informers"
	clientset "k8s.io/client-go/kubernetes"
	restclient "k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/scale"
	"k8s.io/client-go/tools/cache"
	kubeapiservertesting "k8s.io/kubernetes/cmd/kube-apiserver/app/testing"
	"k8s.io/kubernetes/pkg/api/legacyscheme"
	"k8s.io/kubernetes/pkg/controller/disruption"
	"k8s.io/kubernetes/test/integration/etcd"
	"k8s.io/kubernetes/test/integration/framework"
	"k8s.io/kubernetes/test/integration/util"
	"k8s.io/kubernetes/test/utils/ktesting"
	"k8s.io/utils/clock"
	"k8s.io/utils/ptr"
)

const stalePodDisruptionTimeout = 3 * time.Second

func setup(ctx context.Context, t *testing.T) (*kubeapiservertesting.TestServer, *disruption.DisruptionController, informers.SharedInformerFactory, clientset.Interface, *apiextensionsclientset.Clientset, dynamic.Interface) {
	server := kubeapiservertesting.StartTestServerOrDie(t, nil, framework.DefaultTestServerFlags(), framework.SharedEtcd())

	clientSet, err := clientset.NewForConfig(server.ClientConfig)
	if err != nil {
		t.Fatalf("Error creating clientset: %v", err)
	}
	resyncPeriod := 12 * time.Hour
	informers := informers.NewSharedInformerFactory(clientset.NewForConfigOrDie(restclient.AddUserAgent(server.ClientConfig, "pdb-informers")), resyncPeriod)

	client := clientset.NewForConfigOrDie(restclient.AddUserAgent(server.ClientConfig, "disruption-controller"))

	discoveryClient := cacheddiscovery.NewMemCacheClient(clientSet.Discovery())
	mapper := restmapper.NewDeferredDiscoveryRESTMapper(discoveryClient)

	scaleKindResolver := scale.NewDiscoveryScaleKindResolver(client.Discovery())
	scaleClient, err := scale.NewForConfig(server.ClientConfig, mapper, dynamic.LegacyAPIPathResolverFunc, scaleKindResolver)
	if err != nil {
		t.Fatalf("Error creating scaleClient: %v", err)
	}

	apiExtensionClient, err := apiextensionsclientset.NewForConfig(server.ClientConfig)
	if err != nil {
		t.Fatalf("Error creating extension clientset: %v", err)
	}

	dynamicClient, err := dynamic.NewForConfig(server.ClientConfig)
	if err != nil {
		t.Fatalf("Error creating dynamicClient: %v", err)
	}

	pdbc := disruption.NewDisruptionControllerInternal(
		ctx,
		informers.Core().V1().Pods(),
		informers.Policy().V1().PodDisruptionBudgets(),
		informers.Core().V1().ReplicationControllers(),
		informers.Apps().V1().ReplicaSets(),
		informers.Apps().V1().Deployments(),
		informers.Apps().V1().StatefulSets(),
		client,
		mapper,
		scaleClient,
		client.Discovery(),
		clock.RealClock{},
		stalePodDisruptionTimeout,
	)
	return server, pdbc, informers, clientSet, apiExtensionClient, dynamicClient
}

func TestPDBWithScaleSubresource(t *testing.T) {
	tCtx := ktesting.Init(t)
	s, pdbc, informers, clientSet, apiExtensionClient, dynamicClient := setup(tCtx, t)
	defer s.TearDownFn()
	defer tCtx.Cancel("test has completed")

	nsName := "pdb-scale-subresource"
	createNs(tCtx, t, nsName, clientSet)

	informers.Start(tCtx.Done())
	go pdbc.Run(tCtx)

	crdDefinition := newCustomResourceDefinition()
	etcd.CreateTestCRDs(t, apiExtensionClient, true, crdDefinition)
	gvr := schema.GroupVersionResource{Group: crdDefinition.Spec.Group, Version: crdDefinition.Spec.Versions[0].Name, Resource: crdDefinition.Spec.Names.Plural}
	resourceClient := dynamicClient.Resource(gvr).Namespace(nsName)

	replicas := 4
	maxUnavailable := int32(2)

	resource := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"kind":       crdDefinition.Spec.Names.Kind,
			"apiVersion": crdDefinition.Spec.Group + "/" + crdDefinition.Spec.Versions[0].Name,
			"metadata": map[string]interface{}{
				"name":      "resource",
				"namespace": nsName,
			},
			"spec": map[string]interface{}{
				"replicas": replicas,
			},
		},
	}
	createdResource, err := resourceClient.Create(tCtx, resource, metav1.CreateOptions{})
	if err != nil {
		t.Error(err)
	}

	trueValue := true
	ownerRefs := []metav1.OwnerReference{
		{
			Name:       resource.GetName(),
			Kind:       crdDefinition.Spec.Names.Kind,
			APIVersion: crdDefinition.Spec.Group + "/" + crdDefinition.Spec.Versions[0].Name,
			UID:        createdResource.GetUID(),
			Controller: &trueValue,
		},
	}
	for i := 0; i < replicas; i++ {
		createPod(tCtx, t, fmt.Sprintf("pod-%d", i), nsName, map[string]string{"app": "test-crd"}, clientSet, ownerRefs)
	}

	waitToObservePods(t, informers.Core().V1().Pods().Informer(), 4, v1.PodRunning)

	pdb := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-pdb",
		},
		Spec: policyv1.PodDisruptionBudgetSpec{
			MaxUnavailable: &intstr.IntOrString{
				Type:   intstr.Int,
				IntVal: maxUnavailable,
			},
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": "test-crd"},
			},
		},
	}
	if _, err := clientSet.PolicyV1().PodDisruptionBudgets(nsName).Create(tCtx, pdb, metav1.CreateOptions{}); err != nil {
		t.Errorf("Error creating PodDisruptionBudget: %v", err)
	}

	waitPDBStable(tCtx, t, clientSet, 4, nsName, pdb.Name)

	newPdb, err := clientSet.PolicyV1().PodDisruptionBudgets(nsName).Get(tCtx, pdb.Name, metav1.GetOptions{})
	if err != nil {
		t.Errorf("Error getting PodDisruptionBudget: %v", err)
	}

	if expected, found := int32(replicas), newPdb.Status.ExpectedPods; expected != found {
		t.Errorf("Expected %d, but found %d", expected, found)
	}
	if expected, found := int32(replicas)-maxUnavailable, newPdb.Status.DesiredHealthy; expected != found {
		t.Errorf("Expected %d, but found %d", expected, found)
	}
	if expected, found := maxUnavailable, newPdb.Status.DisruptionsAllowed; expected != found {
		t.Errorf("Expected %d, but found %d", expected, found)
	}
}

func TestEmptySelector(t *testing.T) {
	testcases := []struct {
		name                   string
		createPDBFunc          func(ctx context.Context, clientSet clientset.Interface, etcdClient *clientv3.Client, etcdStoragePrefix, name, nsName string, minAvailable intstr.IntOrString) error
		expectedCurrentHealthy int32
	}{
		{
			name: "v1beta1 should not target any pods",
			createPDBFunc: func(ctx context.Context, clientSet clientset.Interface, etcdClient *clientv3.Client, etcdStoragePrefix, name, nsName string, minAvailable intstr.IntOrString) error {
				pdb := &v1beta1.PodDisruptionBudget{
					ObjectMeta: metav1.ObjectMeta{
						Name: name,
					},
					Spec: v1beta1.PodDisruptionBudgetSpec{
						MinAvailable: &minAvailable,
						Selector:     &metav1.LabelSelector{},
					},
				}
				return createPDBUsingRemovedAPI(ctx, etcdClient, etcdStoragePrefix, nsName, pdb)
			},
			expectedCurrentHealthy: 0,
		},
		{
			name: "v1 should target all pods",
			createPDBFunc: func(ctx context.Context, clientSet clientset.Interface, etcdClient *clientv3.Client, etcdStoragePrefix, name, nsName string, minAvailable intstr.IntOrString) error {
				pdb := &policyv1.PodDisruptionBudget{
					ObjectMeta: metav1.ObjectMeta{
						Name: name,
					},
					Spec: policyv1.PodDisruptionBudgetSpec{
						MinAvailable: &minAvailable,
						Selector:     &metav1.LabelSelector{},
					},
				}
				_, err := clientSet.PolicyV1().PodDisruptionBudgets(nsName).Create(ctx, pdb, metav1.CreateOptions{})
				return err
			},
			expectedCurrentHealthy: 4,
		},
	}

	for i, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			tCtx := ktesting.Init(t)
			s, pdbc, informers, clientSet, _, _ := setup(tCtx, t)
			defer s.TearDownFn()
			defer tCtx.Cancel("test has completed")

			nsName := fmt.Sprintf("pdb-empty-selector-%d", i)
			createNs(tCtx, t, nsName, clientSet)

			informers.Start(tCtx.Done())
			go pdbc.Run(tCtx)

			replicas := 4
			minAvailable := intstr.FromInt32(2)

			for j := 0; j < replicas; j++ {
				createPod(tCtx, t, fmt.Sprintf("pod-%d", j), nsName, map[string]string{"app": "test-crd"},
					clientSet, []metav1.OwnerReference{})
			}

			waitToObservePods(t, informers.Core().V1().Pods().Informer(), 4, v1.PodRunning)

			pdbName := "test-pdb"
			if err := tc.createPDBFunc(tCtx, clientSet, s.EtcdClient, s.EtcdStoragePrefix, pdbName, nsName, minAvailable); err != nil {
				t.Errorf("Error creating PodDisruptionBudget: %v", err)
			}

			waitPDBStable(tCtx, t, clientSet, tc.expectedCurrentHealthy, nsName, pdbName)

			newPdb, err := clientSet.PolicyV1().PodDisruptionBudgets(nsName).Get(tCtx, pdbName, metav1.GetOptions{})
			if err != nil {
				t.Errorf("Error getting PodDisruptionBudget: %v", err)
			}

			if expected, found := tc.expectedCurrentHealthy, newPdb.Status.CurrentHealthy; expected != found {
				t.Errorf("Expected %d, but found %d", expected, found)
			}
		})
	}
}

func TestSelectorsForPodsWithoutLabels(t *testing.T) {
	testcases := []struct {
		name                   string
		createPDBFunc          func(ctx context.Context, clientSet clientset.Interface, etcdClient *clientv3.Client, etcdStoragePrefix, name, nsName string, minAvailable intstr.IntOrString) error
		expectedCurrentHealthy int32
	}{
		{
			name: "pods with no labels can be targeted by v1 PDBs with empty selector",
			createPDBFunc: func(ctx context.Context, clientSet clientset.Interface, etcdClient *clientv3.Client, etcdStoragePrefix, name, nsName string, minAvailable intstr.IntOrString) error {
				pdb := &policyv1.PodDisruptionBudget{
					ObjectMeta: metav1.ObjectMeta{
						Name: name,
					},
					Spec: policyv1.PodDisruptionBudgetSpec{
						MinAvailable: &minAvailable,
						Selector:     &metav1.LabelSelector{},
					},
				}
				_, err := clientSet.PolicyV1().PodDisruptionBudgets(nsName).Create(context.TODO(), pdb, metav1.CreateOptions{})
				return err
			},
			expectedCurrentHealthy: 1,
		},
		{
			name: "pods with no labels can be targeted by v1 PDBs with DoesNotExist selector",
			createPDBFunc: func(ctx context.Context, clientSet clientset.Interface, etcdClient *clientv3.Client, etcdStoragePrefix, name, nsName string, minAvailable intstr.IntOrString) error {
				pdb := &policyv1.PodDisruptionBudget{
					ObjectMeta: metav1.ObjectMeta{
						Name: name,
					},
					Spec: policyv1.PodDisruptionBudgetSpec{
						MinAvailable: &minAvailable,
						Selector: &metav1.LabelSelector{
							MatchExpressions: []metav1.LabelSelectorRequirement{
								{
									Key:      "DoesNotExist",
									Operator: metav1.LabelSelectorOpDoesNotExist,
								},
							},
						},
					},
				}
				_, err := clientSet.PolicyV1().PodDisruptionBudgets(nsName).Create(ctx, pdb, metav1.CreateOptions{})
				return err
			},
			expectedCurrentHealthy: 1,
		},
		{
			name: "pods with no labels can be targeted by v1beta1 PDBs with DoesNotExist selector",
			createPDBFunc: func(ctx context.Context, clientSet clientset.Interface, etcdClient *clientv3.Client, etcdStoragePrefix, name, nsName string, minAvailable intstr.IntOrString) error {
				pdb := &v1beta1.PodDisruptionBudget{
					ObjectMeta: metav1.ObjectMeta{
						Name: name,
					},
					Spec: v1beta1.PodDisruptionBudgetSpec{
						MinAvailable: &minAvailable,
						Selector: &metav1.LabelSelector{
							MatchExpressions: []metav1.LabelSelectorRequirement{
								{
									Key:      "DoesNotExist",
									Operator: metav1.LabelSelectorOpDoesNotExist,
								},
							},
						},
					},
				}
				return createPDBUsingRemovedAPI(ctx, etcdClient, etcdStoragePrefix, nsName, pdb)
			},
			expectedCurrentHealthy: 1,
		},
	}

	for i, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			tCtx := ktesting.Init(t)
			s, pdbc, informers, clientSet, _, _ := setup(tCtx, t)
			defer s.TearDownFn()
			defer tCtx.Cancel("test has completed")

			nsName := fmt.Sprintf("pdb-selectors-%d", i)
			createNs(tCtx, t, nsName, clientSet)

			informers.Start(tCtx.Done())
			go pdbc.Run(tCtx)

			minAvailable := intstr.FromInt32(1)

			// Create the PDB first and wait for it to settle.
			pdbName := "test-pdb"
			if err := tc.createPDBFunc(tCtx, clientSet, s.EtcdClient, s.EtcdStoragePrefix, pdbName, nsName, minAvailable); err != nil {
				t.Errorf("Error creating PodDisruptionBudget: %v", err)
			}
			waitPDBStable(tCtx, t, clientSet, 0, nsName, pdbName)

			// Create a pod and wait for it be reach the running phase.
			createPod(tCtx, t, "pod", nsName, map[string]string{}, clientSet, []metav1.OwnerReference{})
			waitToObservePods(t, informers.Core().V1().Pods().Informer(), 1, v1.PodRunning)

			// Then verify that the added pod are picked up by the disruption controller.
			waitPDBStable(tCtx, t, clientSet, 1, nsName, pdbName)

			newPdb, err := clientSet.PolicyV1().PodDisruptionBudgets(nsName).Get(tCtx, pdbName, metav1.GetOptions{})
			if err != nil {
				t.Errorf("Error getting PodDisruptionBudget: %v", err)
			}

			if expected, found := tc.expectedCurrentHealthy, newPdb.Status.CurrentHealthy; expected != found {
				t.Errorf("Expected %d, but found %d", expected, found)
			}
		})
	}
}

func createPod(ctx context.Context, t *testing.T, name, namespace string, labels map[string]string, clientSet clientset.Interface, ownerRefs []metav1.OwnerReference) {
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Namespace:       namespace,
			Labels:          labels,
			OwnerReferences: ownerRefs,
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Name:  "fake-name",
					Image: "fakeimage",
				},
			},
		},
	}
	_, err := clientSet.CoreV1().Pods(namespace).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		t.Error(err)
	}
	addPodConditionReady(pod)
	if _, err := clientSet.CoreV1().Pods(namespace).UpdateStatus(ctx, pod, metav1.UpdateOptions{}); err != nil {
		t.Error(err)
	}
}

func createNs(ctx context.Context, t *testing.T, name string, clientSet clientset.Interface) {
	_, err := clientSet.CoreV1().Namespaces().Create(ctx, &v1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Errorf("Error creating namespace: %v", err)
	}
}

func addPodConditionReady(pod *v1.Pod) {
	pod.Status = v1.PodStatus{
		Phase: v1.PodRunning,
		Conditions: []v1.PodCondition{
			{
				Type:   v1.PodReady,
				Status: v1.ConditionTrue,
			},
		},
	}
}

func newCustomResourceDefinition() *apiextensionsv1.CustomResourceDefinition {
	return &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "crds.mygroup.example.com"},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: "mygroup.example.com",
			Names: apiextensionsv1.CustomResourceDefinitionNames{
				Plural:   "crds",
				Singular: "crd",
				Kind:     "Crd",
				ListKind: "CrdList",
			},
			Scope: apiextensionsv1.NamespaceScoped,
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{
				{
					Name:    "v1beta1",
					Served:  true,
					Storage: true,
					Schema:  fixtures.AllowAllSchema(),
					Subresources: &apiextensionsv1.CustomResourceSubresources{
						Scale: &apiextensionsv1.CustomResourceSubresourceScale{
							SpecReplicasPath:   ".spec.replicas",
							StatusReplicasPath: ".status.replicas",
						},
					},
				},
			},
		},
	}
}

func waitPDBStable(ctx context.Context, t *testing.T, clientSet clientset.Interface, podNum int32, ns, pdbName string) {
	if err := wait.PollImmediate(2*time.Second, 60*time.Second, func() (bool, error) {
		pdb, err := clientSet.PolicyV1().PodDisruptionBudgets(ns).Get(ctx, pdbName, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		if pdb.Status.ObservedGeneration == 0 || pdb.Status.CurrentHealthy != podNum {
			return false, nil
		}
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
}

func waitToObservePods(t *testing.T, podInformer cache.SharedIndexInformer, podNum int, phase v1.PodPhase) {
	if err := wait.PollImmediate(2*time.Second, 60*time.Second, func() (bool, error) {
		objects := podInformer.GetIndexer().List()
		if len(objects) != podNum {
			return false, nil
		}
		for _, obj := range objects {
			pod := obj.(*v1.Pod)
			if pod.Status.Phase != phase {
				return false, nil
			}
		}
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
}

// createPDBUsingRemovedAPI creates a PDB directly using etcd.  This is must *ONLY* be used for checks of compatibility
// with removed data. Do not use this just because you don't want to update your test to use v1.  Only use this
// when it actually matters.
func createPDBUsingRemovedAPI(ctx context.Context, etcdClient *clientv3.Client, etcdStoragePrefix, nsName string, betaPDB *v1beta1.PodDisruptionBudget) error {
	betaPDB.APIVersion = v1beta1.SchemeGroupVersion.Group + "/" + v1beta1.SchemeGroupVersion.Version
	betaPDB.Kind = "PodDisruptionBudget"
	betaPDB.Namespace = nsName
	betaPDB.Generation = 1
	rest.FillObjectMetaSystemFields(betaPDB)
	ctx = genericapirequest.WithNamespace(ctx, nsName)
	key := path.Join("/", etcdStoragePrefix, "poddisruptionbudgets", nsName, betaPDB.Name)
	protoSerializer := protobuf.NewSerializer(legacyscheme.Scheme, legacyscheme.Scheme, protobuf.SerializerOptions{})
	buffer := bytes.NewBuffer(nil)
	if err := protoSerializer.Encode(betaPDB, buffer); err != nil {
		return err
	}
	_, err := etcdClient.Put(ctx, key, buffer.String())
	return err
}

func TestPatchCompatibility(t *testing.T) {
	tCtx := ktesting.Init(t)
	s, pdbc, _, clientSet, _, _ := setup(tCtx, t)
	defer s.TearDownFn()
	// Even though pdbc isn't used in this test, its creation is already
	// spawning some goroutines. So we need to run it to ensure they won't leak.
	// We can't cancel immediately but later, because when the context is canceled,
	// the event broadcaster will be shut down .
	defer tCtx.Cancel("cleaning up")
	go pdbc.Run(tCtx)

	testcases := []struct {
		name             string
		version          string
		startingSelector *metav1.LabelSelector
		patchType        types.PatchType
		patch            string
		force            *bool
		fieldManager     string
		expectSelector   *metav1.LabelSelector
	}{
		{
			name:      "v1-smp",
			version:   "v1",
			patchType: types.StrategicMergePatchType,
			patch:     `{"spec":{"selector":{"matchLabels":{"patchmatch":"true"},"matchExpressions":[{"key":"patchexpression","operator":"In","values":["true"]}]}}}`,
			// matchLabels and matchExpressions are both replaced (because selector patchStrategy=replace in v1)
			expectSelector: &metav1.LabelSelector{
				MatchLabels:      map[string]string{"patchmatch": "true"},
				MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "patchexpression", Operator: "In", Values: []string{"true"}}},
			},
		},
		{
			name:      "v1-mergepatch",
			version:   "v1",
			patchType: types.MergePatchType,
			patch:     `{"spec":{"selector":{"matchLabels":{"patchmatch":"true"},"matchExpressions":[{"key":"patchexpression","operator":"In","values":["true"]}]}}}`,
			// matchLabels portion is merged, matchExpressions portion is replaced (because it's a list)
			expectSelector: &metav1.LabelSelector{
				MatchLabels:      map[string]string{"basematch": "true", "patchmatch": "true"},
				MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "patchexpression", Operator: "In", Values: []string{"true"}}},
			},
		},
		{
			name:         "v1-apply",
			version:      "v1",
			patchType:    types.ApplyPatchType,
			patch:        `{"apiVersion":"policy/v1","kind":"PodDisruptionBudget","spec":{"selector":{"matchLabels":{"patchmatch":"true"},"matchExpressions":[{"key":"patchexpression","operator":"In","values":["true"]}]}}}`,
			force:        ptr.To(true),
			fieldManager: "test",
			// entire selector is replaced (because structType=atomic)
			expectSelector: &metav1.LabelSelector{
				MatchLabels:      map[string]string{"patchmatch": "true"},
				MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "patchexpression", Operator: "In", Values: []string{"true"}}},
			},
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			ns := "default"
			maxUnavailable := int32(2)
			pdb := &policyv1.PodDisruptionBudget{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-pdb",
				},
				Spec: policyv1.PodDisruptionBudgetSpec{
					MaxUnavailable: &intstr.IntOrString{Type: intstr.Int, IntVal: maxUnavailable},
					Selector: &metav1.LabelSelector{
						MatchLabels:      map[string]string{"basematch": "true"},
						MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "baseexpression", Operator: "In", Values: []string{"true"}}},
					},
				},
			}
			if _, err := clientSet.PolicyV1().PodDisruptionBudgets(ns).Create(context.TODO(), pdb, metav1.CreateOptions{}); err != nil {
				t.Fatalf("Error creating PodDisruptionBudget: %v", err)
			}
			defer func() {
				err := clientSet.PolicyV1().PodDisruptionBudgets(ns).Delete(context.TODO(), pdb.Name, metav1.DeleteOptions{})
				if err != nil {
					t.Fatal(err)
				}
			}()

			var resultSelector *metav1.LabelSelector
			switch tc.version {
			case "v1":
				result, err := clientSet.PolicyV1().PodDisruptionBudgets(ns).Patch(context.TODO(), pdb.Name, tc.patchType, []byte(tc.patch), metav1.PatchOptions{Force: tc.force, FieldManager: tc.fieldManager})
				if err != nil {
					t.Fatal(err)
				}
				resultSelector = result.Spec.Selector
			default:
				t.Error("unknown version")
			}

			if !reflect.DeepEqual(resultSelector, tc.expectSelector) {
				t.Fatalf("unexpected selector:\n%s", cmp.Diff(tc.expectSelector, resultSelector))
			}
		})
	}
}

func TestStalePodDisruption(t *testing.T) {
	tCtx := ktesting.Init(t)
	s, pdbc, informers, clientSet, _, _ := setup(tCtx, t)
	defer s.TearDownFn()
	defer tCtx.Cancel("test has completed")

	nsName := "pdb-stale-pod-disruption"
	createNs(tCtx, t, nsName, clientSet)

	informers.Start(tCtx.Done())
	informers.WaitForCacheSync(tCtx.Done())
	go pdbc.Run(tCtx)

	cases := map[string]struct {
		deletePod      bool
		podPhase       v1.PodPhase
		reason         string
		wantConditions []v1.PodCondition
	}{
		"stale-condition": {
			podPhase: v1.PodRunning,
			wantConditions: []v1.PodCondition{
				{
					Type:   v1.DisruptionTarget,
					Status: v1.ConditionFalse,
				},
			},
		},
		"deleted-pod": {
			podPhase:  v1.PodRunning,
			deletePod: true,
			wantConditions: []v1.PodCondition{
				{
					Type:   v1.DisruptionTarget,
					Status: v1.ConditionTrue,
				},
			},
		},
		"disruption-condition-by-kubelet": {
			podPhase: v1.PodFailed,
			reason:   v1.PodReasonTerminationByKubelet,
			wantConditions: []v1.PodCondition{
				{
					Type:   v1.DisruptionTarget,
					Status: v1.ConditionTrue,
					Reason: v1.PodReasonTerminationByKubelet,
				},
			},
		},
		"disruption-condition-on-failed-pod": {
			podPhase: v1.PodFailed,
			wantConditions: []v1.PodCondition{
				{
					Type:   v1.DisruptionTarget,
					Status: v1.ConditionTrue,
				},
			},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			pod := util.InitPausePod(&util.PausePodConfig{
				Name:      name,
				Namespace: nsName,
				NodeName:  "foo", // mock pod as scheduled so that it's not immediately deleted when calling Delete.
			})
			var err error
			pod, err = util.CreatePausePod(clientSet, pod)
			if err != nil {
				t.Fatalf("Failed creating pod: %v", err)
			}

			pod.Status.Phase = tc.podPhase
			pod.Status.Conditions = append(pod.Status.Conditions, v1.PodCondition{
				Type:               v1.DisruptionTarget,
				Status:             v1.ConditionTrue,
				Reason:             tc.reason,
				LastTransitionTime: metav1.Now(),
			})
			pod, err = clientSet.CoreV1().Pods(nsName).UpdateStatus(tCtx, pod, metav1.UpdateOptions{})
			if err != nil {
				t.Fatalf("Failed updating pod: %v", err)
			}

			if tc.deletePod {
				if err := clientSet.CoreV1().Pods(nsName).Delete(tCtx, name, metav1.DeleteOptions{}); err != nil {
					t.Fatalf("Failed to delete pod: %v", err)
				}
			}
			time.Sleep(stalePodDisruptionTimeout)
			diff := ""
			if err := wait.PollImmediate(100*time.Millisecond, wait.ForeverTestTimeout, func() (done bool, err error) {
				pod, err = clientSet.CoreV1().Pods(nsName).Get(tCtx, name, metav1.GetOptions{})
				if err != nil {
					return false, err
				}
				if tc.deletePod && pod.DeletionTimestamp == nil {
					return false, nil
				}
				diff = cmp.Diff(tc.wantConditions, pod.Status.Conditions, cmpopts.IgnoreFields(v1.PodCondition{}, "LastTransitionTime"))
				return diff == "", nil
			}); err != nil {
				t.Errorf("Failed waiting for status to change: %v", err)
				if diff != "" {
					t.Errorf("Pod has conditions (-want,+got):\n%s", diff)
				}
			}
		})
	}
}
