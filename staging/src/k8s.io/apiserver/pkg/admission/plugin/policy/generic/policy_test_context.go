/*
Copyright 2024 The Kubernetes Authors.

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

package generic

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
	"k8s.io/component-base/featuregate"

	"k8s.io/apiserver/pkg/admission"
	"k8s.io/apiserver/pkg/admission/initializer"
	"k8s.io/apiserver/pkg/authorization/authorizer"
)

// PolicyTestContext is everything you need to unit test a policy plugin
type PolicyTestContext[P runtime.Object, B runtime.Object, E Evaluator] struct {
	context.Context
	Plugin            *Plugin[PolicyHook[P, B, E]]
	Source            Source[PolicyHook[P, B, E]]
	Start             func() error
	MetricHookTracker *metricHookTracker[P, B, E]

	scheme     *runtime.Scheme
	restMapper *meta.DefaultRESTMapper
	policyGVR  schema.GroupVersionResource
	bindingGVR schema.GroupVersionResource

	policyGVK  schema.GroupVersionKind
	bindingGVK schema.GroupVersionKind

	nativeTracker           clienttesting.ObjectTracker
	policyAndBindingTracker clienttesting.ObjectTracker
	unstructuredTracker     clienttesting.ObjectTracker
}

func NewPolicyTestContext[P, B runtime.Object, E Evaluator](
	newPolicyAccessor func(P) PolicyAccessor,
	newBindingAccessor func(B) BindingAccessor,
	compileFunc func(P) E,
	dispatcher dispatcherFactory[PolicyHook[P, B, E]],
	initialObjects []runtime.Object,
	paramMappings []meta.RESTMapping,
) (*PolicyTestContext[P, B, E], func(), error) {
	var Pexample P
	var Bexample B

	// Create a fake resource and kind for the provided policy and binding types
	fakePolicyGVR := schema.GroupVersionResource{
		Group:    "policy.example.com",
		Version:  "v1",
		Resource: "fakepolicies",
	}
	fakeBindingGVR := schema.GroupVersionResource{
		Group:    "policy.example.com",
		Version:  "v1",
		Resource: "fakebindings",
	}
	fakePolicyGVK := fakePolicyGVR.GroupVersion().WithKind("FakePolicy")
	fakeBindingGVK := fakeBindingGVR.GroupVersion().WithKind("FakeBinding")

	policySourceTestScheme, err := func() (*runtime.Scheme, error) {
		scheme := runtime.NewScheme()

		if err := fake.AddToScheme(scheme); err != nil {
			return nil, err
		}

		scheme.AddKnownTypeWithName(fakePolicyGVK, Pexample)
		scheme.AddKnownTypeWithName(fakeBindingGVK, Bexample)
		scheme.AddKnownTypeWithName(fakePolicyGVK.GroupVersion().WithKind(fakePolicyGVK.Kind+"List"), &FakeList[P]{})
		scheme.AddKnownTypeWithName(fakeBindingGVK.GroupVersion().WithKind(fakeBindingGVK.Kind+"List"), &FakeList[B]{})

		for _, mapping := range paramMappings {
			// Skip if it is in the scheme already
			if scheme.Recognizes(mapping.GroupVersionKind) {
				continue
			}
			scheme.AddKnownTypeWithName(mapping.GroupVersionKind, &unstructured.Unstructured{})
			scheme.AddKnownTypeWithName(mapping.GroupVersionKind.GroupVersion().WithKind(mapping.GroupVersionKind.Kind+"List"), &unstructured.UnstructuredList{})
		}

		return scheme, nil
	}()
	if err != nil {
		return nil, nil, err
	}

	fakeRestMapper := func() *meta.DefaultRESTMapper {
		res := meta.NewDefaultRESTMapper([]schema.GroupVersion{
			{
				Group:   "",
				Version: "v1",
			},
		})

		res.Add(fakePolicyGVK, meta.RESTScopeRoot)
		res.Add(fakeBindingGVK, meta.RESTScopeRoot)
		res.Add(corev1.SchemeGroupVersion.WithKind("ConfigMap"), meta.RESTScopeNamespace)

		for _, mapping := range paramMappings {
			res.AddSpecific(mapping.GroupVersionKind, mapping.Resource, mapping.Resource, mapping.Scope)
		}

		return res
	}()

	nativeClient := fake.NewSimpleClientset()
	dynamicClient := dynamicfake.NewSimpleDynamicClient(policySourceTestScheme)
	fakeInformerFactory := informers.NewSharedInformerFactory(nativeClient, 30*time.Second)

	// Make an object tracker specifically for our policies and bindings
	policiesAndBindingsTracker := clienttesting.NewObjectTracker(
		policySourceTestScheme,
		serializer.NewCodecFactory(policySourceTestScheme).UniversalDecoder())

	// Make an informer for our policies and bindings

	policyInformer := cache.NewSharedIndexInformer(
		&cache.ListWatch{
			ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
				return policiesAndBindingsTracker.List(fakePolicyGVR, fakePolicyGVK, "")
			},
			WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
				return policiesAndBindingsTracker.Watch(fakePolicyGVR, "")
			},
		},
		Pexample,
		30*time.Second,
		cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc},
	)
	bindingInformer := cache.NewSharedIndexInformer(
		&cache.ListWatch{
			ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
				return policiesAndBindingsTracker.List(fakeBindingGVR, fakeBindingGVK, "")
			},
			WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
				return policiesAndBindingsTracker.Watch(fakeBindingGVR, "")
			},
		},
		Bexample,
		30*time.Second,
		cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc},
	)

	hookTracker := &metricHookTracker[P, B, E]{}
	var source Source[PolicyHook[P, B, E]]
	plugin := NewPlugin[PolicyHook[P, B, E]](
		admission.NewHandler(admission.Connect, admission.Create, admission.Delete, admission.Update),
		func(sif informers.SharedInformerFactory, i1 kubernetes.Interface, i2 dynamic.Interface, r meta.RESTMapper) Source[PolicyHook[P, B, E]] {
			source = NewPolicySource[P, B, E](
				policyInformer,
				bindingInformer,
				newPolicyAccessor,
				newBindingAccessor,
				compileFunc,
				sif,
				hookTracker.updateHookMetrics,
				i2,
				r,
			)
			return source
		}, dispatcher)
	plugin.SetEnabled(true)

	featureGate := featuregate.NewFeatureGate()
	testContext, testCancel := context.WithCancel(context.Background())
	genericInitializer := initializer.New(
		nativeClient,
		dynamicClient,
		fakeInformerFactory,
		fakeAuthorizer{},
		featureGate,
		testContext.Done(),
		fakeRestMapper,
	)
	genericInitializer.Initialize(plugin)
	plugin.SetRESTMapper(fakeRestMapper)

	if err := plugin.ValidateInitialization(); err != nil {
		testCancel()
		return nil, nil, err
	}

	res := &PolicyTestContext[P, B, E]{
		Context:           testContext,
		Plugin:            plugin,
		Source:            source,
		MetricHookTracker: hookTracker,

		restMapper:              fakeRestMapper,
		scheme:                  policySourceTestScheme,
		policyGVK:               fakePolicyGVK,
		bindingGVK:              fakeBindingGVK,
		policyGVR:               fakePolicyGVR,
		bindingGVR:              fakeBindingGVR,
		nativeTracker:           nativeClient.Tracker(),
		policyAndBindingTracker: policiesAndBindingsTracker,
		unstructuredTracker:     dynamicClient.Tracker(),
	}

	for _, obj := range initialObjects {
		err := res.updateOne(obj)
		if err != nil {
			testCancel()
			return nil, nil, err
		}
	}

	res.Start = func() error {
		fakeInformerFactory.Start(res.Done())
		go policyInformer.Run(res.Done())
		go bindingInformer.Run(res.Done())

		if !cache.WaitForCacheSync(res.Done(), res.Source.HasSynced) {
			return fmt.Errorf("timed out waiting for initial cache sync")
		}
		return nil
	}
	return res, testCancel, nil
}

// UpdateAndWait updates the given object in the test, or creates it if it doesn't exist
// Depending upon object type, waits afterward until the object is synced
// by the policy source
//
// Be aware the UpdateAndWait will modify the ResourceVersion of the
// provided objects.
func (p *PolicyTestContext[P, B, E]) UpdateAndWait(objects ...runtime.Object) error {
	return p.update(true, objects...)
}

// Update updates the given object in the test, or creates it if it doesn't exist
//
// Be aware the Update will modify the ResourceVersion of the
// provided objects.
func (p *PolicyTestContext[P, B, E]) Update(objects ...runtime.Object) error {
	return p.update(false, objects...)
}

// Objects the given object in the test, or creates it if it doesn't exist
// Depending upon object type, waits afterward until the object is synced
// by the policy source
func (p *PolicyTestContext[P, B, E]) update(wait bool, objects ...runtime.Object) error {
	for _, object := range objects {
		if err := p.updateOne(object); err != nil {
			return err
		}
	}

	if wait {
		timeoutCtx, timeoutCancel := context.WithTimeout(p, 3*time.Second)
		defer timeoutCancel()

		for _, object := range objects {
			if err := p.WaitForReconcile(timeoutCtx, object); err != nil {
				return fmt.Errorf("error waiting for reconcile of %v: %v", object, err)
			}
		}
	}
	return nil
}

// Depending upon object type, waits afterward until the object is synced
// by the policy source. Note that policies that are not bound are skipped,
// so you should not try to wait for an unbound policy. Create both the binding
// and policy, then wait.
func (p *PolicyTestContext[P, B, E]) WaitForReconcile(timeoutCtx context.Context, object runtime.Object) error {
	if !p.Source.HasSynced() {
		return nil
	}

	objectMeta, err := meta.Accessor(object)
	if err != nil {
		return err
	}

	objectGVK, _, err := p.inferGVK(object)
	if err != nil {
		return err
	}

	switch objectGVK {
	case p.policyGVK:
		return wait.PollUntilContextCancel(timeoutCtx, 100*time.Millisecond, true, func(ctx context.Context) (done bool, err error) {
			policies := p.Source.Hooks()
			for _, policy := range policies {
				policyMeta, err := meta.Accessor(policy.Policy)
				if err != nil {
					return true, err
				} else if policyMeta.GetName() == objectMeta.GetName() && policyMeta.GetResourceVersion() == objectMeta.GetResourceVersion() {
					return true, nil
				}
			}
			return false, nil
		})
	case p.bindingGVK:
		return wait.PollUntilContextCancel(timeoutCtx, 100*time.Millisecond, true, func(ctx context.Context) (done bool, err error) {
			policies := p.Source.Hooks()
			for _, policy := range policies {
				for _, binding := range policy.Bindings {
					bindingMeta, err := meta.Accessor(binding)
					if err != nil {
						return true, err
					} else if bindingMeta.GetName() == objectMeta.GetName() && bindingMeta.GetResourceVersion() == objectMeta.GetResourceVersion() {
						return true, nil
					}
				}
			}
			return false, nil
		})

	default:
		// Do nothing, params are visible immediately
		// Loop until one of the params is visible via get of the param informer
		return wait.PollUntilContextCancel(timeoutCtx, 100*time.Millisecond, true, func(ctx context.Context) (done bool, err error) {
			informer, scope := p.Source.(*policySource[P, B, E]).getParamInformer(objectGVK)
			if informer == nil {
				// Informer does not exist yet, keep waiting for sync
				return false, nil
			}

			if !cache.WaitForCacheSync(timeoutCtx.Done(), informer.Informer().HasSynced) {
				return false, fmt.Errorf("timed out waiting for cache sync of param informer")
			}

			var lister cache.GenericNamespaceLister = informer.Lister()
			if scope == meta.RESTScopeNamespace {
				lister = informer.Lister().ByNamespace(objectMeta.GetNamespace())
			}

			fetched, err := lister.Get(objectMeta.GetName())
			if err != nil {
				if errors.IsNotFound(err) {
					return false, nil
				}
				return true, err
			}

			// Ensure RV matches
			fetchedMeta, err := meta.Accessor(fetched)
			if err != nil {
				return true, err
			} else if fetchedMeta.GetResourceVersion() != objectMeta.GetResourceVersion() {
				return false, nil
			}

			return true, nil
		})
	}
}

func (p *PolicyTestContext[P, B, E]) waitForDelete(ctx context.Context, objectGVK schema.GroupVersionKind, name types.NamespacedName) error {
	srce := p.Source.(*policySource[P, B, E])

	return wait.PollUntilContextCancel(ctx, 100*time.Millisecond, true, func(ctx context.Context) (done bool, err error) {
		switch objectGVK {
		case p.policyGVK:
			for _, hook := range p.Source.Hooks() {
				accessor := srce.newPolicyAccessor(hook.Policy)
				if accessor.GetName() == name.Name && accessor.GetNamespace() == name.Namespace {
					return false, nil
				}
			}

			return true, nil
		case p.bindingGVK:
			for _, hook := range p.Source.Hooks() {
				for _, binding := range hook.Bindings {
					accessor := srce.newBindingAccessor(binding)
					if accessor.GetName() == name.Name && accessor.GetNamespace() == name.Namespace {
						return false, nil
					}
				}
			}
			return true, nil
		default:
			// Do nothing, params are visible immediately
			// Loop until one of the params is visible via get of the param informer
			informer, scope := p.Source.(*policySource[P, B, E]).getParamInformer(objectGVK)
			if informer == nil {
				return true, nil
			}

			var lister cache.GenericNamespaceLister = informer.Lister()
			if scope == meta.RESTScopeNamespace {
				lister = informer.Lister().ByNamespace(name.Namespace)
			}

			_, err = lister.Get(name.Name)
			if err != nil {
				if errors.IsNotFound(err) {
					return true, nil
				}
				return false, err
			}
			return false, nil
		}
	})
}

func (p *PolicyTestContext[P, B, E]) updateOne(object runtime.Object) error {
	objectMeta, err := meta.Accessor(object)
	if err != nil {
		return err
	}
	objectMeta.SetResourceVersion(string(uuid.NewUUID()))
	objectGVK, gvr, err := p.inferGVK(object)
	if err != nil {
		return err
	}

	switch objectGVK {
	case p.policyGVK:
		err := p.policyAndBindingTracker.Update(p.policyGVR, object, objectMeta.GetNamespace())
		if errors.IsNotFound(err) {
			err = p.policyAndBindingTracker.Create(p.policyGVR, object, objectMeta.GetNamespace())
		}

		return err
	case p.bindingGVK:
		err := p.policyAndBindingTracker.Update(p.bindingGVR, object, objectMeta.GetNamespace())
		if errors.IsNotFound(err) {
			err = p.policyAndBindingTracker.Create(p.bindingGVR, object, objectMeta.GetNamespace())
		}

		return err
	default:
		if _, ok := object.(*unstructured.Unstructured); ok {
			if err := p.unstructuredTracker.Create(gvr, object, objectMeta.GetNamespace()); err != nil {
				if errors.IsAlreadyExists(err) {
					return p.unstructuredTracker.Update(gvr, object, objectMeta.GetNamespace())
				}
				return err
			}
			return nil
		} else if err := p.nativeTracker.Create(gvr, object, objectMeta.GetNamespace()); err != nil {
			if errors.IsAlreadyExists(err) {
				return p.nativeTracker.Update(gvr, object, objectMeta.GetNamespace())
			}
		}
		return nil
	}
}

// Depending upon object type, waits afterward until the object is synced
// by the policy source
func (p *PolicyTestContext[P, B, E]) DeleteAndWait(object ...runtime.Object) error {
	for _, object := range object {
		if err := p.deleteOne(object); err != nil && !errors.IsNotFound(err) {
			return err
		}
	}

	timeoutCtx, timeoutCancel := context.WithTimeout(p, 3*time.Second)
	defer timeoutCancel()

	for _, object := range object {
		accessor, err := meta.Accessor(object)
		if err != nil {
			return err
		}

		objectGVK, _, err := p.inferGVK(object)
		if err != nil {
			return err
		}

		if err := p.waitForDelete(
			timeoutCtx,
			objectGVK,
			types.NamespacedName{Name: accessor.GetName(), Namespace: accessor.GetNamespace()}); err != nil {
			return err
		}
	}
	return nil
}

func (p *PolicyTestContext[P, B, E]) deleteOne(object runtime.Object) error {
	objectMeta, err := meta.Accessor(object)
	if err != nil {
		return err
	}
	objectMeta.SetResourceVersion(string(uuid.NewUUID()))
	objectGVK, gvr, err := p.inferGVK(object)
	if err != nil {
		return err
	}

	switch objectGVK {
	case p.policyGVK:
		return p.policyAndBindingTracker.Delete(p.policyGVR, objectMeta.GetNamespace(), objectMeta.GetName())
	case p.bindingGVK:
		return p.policyAndBindingTracker.Delete(p.bindingGVR, objectMeta.GetNamespace(), objectMeta.GetName())
	default:
		if _, ok := object.(*unstructured.Unstructured); ok {
			return p.unstructuredTracker.Delete(gvr, objectMeta.GetNamespace(), objectMeta.GetName())
		}
		return p.nativeTracker.Delete(gvr, objectMeta.GetNamespace(), objectMeta.GetName())
	}
}

func (p *PolicyTestContext[P, B, E]) Dispatch(
	new, old runtime.Object,
	operation admission.Operation,
) error {
	if old == nil && new == nil {
		return fmt.Errorf("both old and new objects cannot be nil")
	}

	nonNilObject := new
	if nonNilObject == nil {
		nonNilObject = old
	}

	gvk, gvr, err := p.inferGVK(nonNilObject)
	if err != nil {
		return err
	}

	nonNilMeta, err := meta.Accessor(nonNilObject)
	if err != nil {
		return err
	}

	return p.Plugin.Dispatch(
		p,
		admission.NewAttributesRecord(
			new,
			old,
			gvk,
			nonNilMeta.GetName(),
			nonNilMeta.GetNamespace(),
			gvr,
			"",
			operation,
			nil,
			false,
			nil,
		), admission.NewObjectInterfacesFromScheme(p.scheme))
}

func (p *PolicyTestContext[P, B, E]) inferGVK(object runtime.Object) (schema.GroupVersionKind, schema.GroupVersionResource, error) {
	objectGVK := object.GetObjectKind().GroupVersionKind()
	if objectGVK.Empty() {
		// If the object doesn't have a GVK, ask the schema for preferred GVK
		knownKinds, _, err := p.scheme.ObjectKinds(object)
		if err != nil {
			return schema.GroupVersionKind{}, schema.GroupVersionResource{}, err
		} else if len(knownKinds) == 0 {
			return schema.GroupVersionKind{}, schema.GroupVersionResource{}, fmt.Errorf("no known GVKs for object in schema: %T", object)
		}
		toTake := 0

		// Prefer GVK if it is our fake policy or binding
		for i, knownKind := range knownKinds {
			if knownKind == p.policyGVK || knownKind == p.bindingGVK {
				toTake = i
				break
			}
		}

		objectGVK = knownKinds[toTake]
	}

	// Make sure GVK is known to the fake rest mapper. To prevent cryptic error
	mapping, err := p.restMapper.RESTMapping(objectGVK.GroupKind(), objectGVK.Version)
	if err != nil {
		return schema.GroupVersionKind{}, schema.GroupVersionResource{}, err
	}
	return objectGVK, mapping.Resource, nil
}

type FakeList[T runtime.Object] struct {
	metav1.TypeMeta
	metav1.ListMeta
	Items []T
}

func (fl *FakeList[P]) DeepCopyObject() runtime.Object {
	copiedItems := make([]P, len(fl.Items))
	for i, item := range fl.Items {
		copiedItems[i] = item.DeepCopyObject().(P)
	}
	return &FakeList[P]{
		TypeMeta: fl.TypeMeta,
		ListMeta: fl.ListMeta,
		Items:    copiedItems,
	}
}

type fakeAuthorizer struct{}

func (f fakeAuthorizer) Authorize(ctx context.Context, a authorizer.Attributes) (authorizer.Decision, string, error) {
	return authorizer.DecisionAllow, "", nil
}

type metricHookTracker[P runtime.Object, B runtime.Object, E Evaluator] struct {
	MetricHooks []PolicyHook[P, B, E]
}

func (m *metricHookTracker[P, B, E]) updateHookMetrics(hooks []PolicyHook[P, B, E]) {
	m.MetricHooks = hooks
}
