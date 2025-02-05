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
	goerrors "errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apiserver/pkg/admission/plugin/policy/internal/generic"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
)

// Interval for refreshing policies.
// TODO: Consider reducing this to a shorter duration or replacing this entirely
// with checks that detect when a policy change took effect.
const policyRefreshIntervalDefault = 1 * time.Second

var policyRefreshInterval = policyRefreshIntervalDefault

type policySource[P runtime.Object, B runtime.Object, E Evaluator] struct {
	ctx                context.Context
	policyInformer     generic.Informer[P]
	bindingInformer    generic.Informer[B]
	restMapper         meta.RESTMapper
	newPolicyAccessor  func(P) PolicyAccessor
	newBindingAccessor func(B) BindingAccessor
	updateHookMetrics  func([]PolicyHook[P, B, E])

	informerFactory informers.SharedInformerFactory
	dynamicClient   dynamic.Interface

	compiler func(P) E

	// Currently compiled list of valid/active policy-binding pairs
	policies atomic.Pointer[[]PolicyHook[P, B, E]]
	// Whether the cache of policies is dirty and needs to be recompiled
	policiesDirty atomic.Bool

	lock             sync.Mutex
	compiledPolicies map[types.NamespacedName]compiledPolicyEntry[E]

	// Temporary until we use the dynamic informer factory
	paramsCRDControllers map[schema.GroupVersionKind]*paramInfo
}

type paramInfo struct {
	mapping meta.RESTMapping

	// When the param is changed, or the informer is done being used, the cancel
	// func should be called to stop/cleanup the original informer
	cancelFunc func()

	// The lister for this param
	informer informers.GenericInformer
}

type compiledPolicyEntry[E Evaluator] struct {
	policyVersion string
	evaluator     E
}

type PolicyHook[P runtime.Object, B runtime.Object, E Evaluator] struct {
	Policy   P
	Bindings []B

	// ParamInformer is the informer for the param CRD for this policy, or nil if
	// there is no param or if there was a configuration error
	ParamInformer informers.GenericInformer
	ParamScope    meta.RESTScope

	Evaluator          E
	ConfigurationError error
}

var _ Source[PolicyHook[runtime.Object, runtime.Object, Evaluator]] = &policySource[runtime.Object, runtime.Object, Evaluator]{}

func NewPolicySource[P runtime.Object, B runtime.Object, E Evaluator](
	policyInformer cache.SharedIndexInformer,
	bindingInformer cache.SharedIndexInformer,
	newPolicyAccessor func(P) PolicyAccessor,
	newBindingAccessor func(B) BindingAccessor,
	compiler func(P) E,
	paramInformerFactory informers.SharedInformerFactory,
	updateHookMetrics func([]PolicyHook[P, B, E]),
	dynamicClient dynamic.Interface,
	restMapper meta.RESTMapper,
) Source[PolicyHook[P, B, E]] {
	res := &policySource[P, B, E]{
		compiler:             compiler,
		policyInformer:       generic.NewInformer[P](policyInformer),
		bindingInformer:      generic.NewInformer[B](bindingInformer),
		compiledPolicies:     map[types.NamespacedName]compiledPolicyEntry[E]{},
		newPolicyAccessor:    newPolicyAccessor,
		newBindingAccessor:   newBindingAccessor,
		paramsCRDControllers: map[schema.GroupVersionKind]*paramInfo{},
		informerFactory:      paramInformerFactory,
		updateHookMetrics:    updateHookMetrics,
		dynamicClient:        dynamicClient,
		restMapper:           restMapper,
	}
	return res
}

// SetPolicyRefreshIntervalForTests allows the refresh interval to be overridden during tests.
// This should only be called from tests.
func SetPolicyRefreshIntervalForTests(interval time.Duration) func() {
	policyRefreshInterval = interval
	return func() {
		policyRefreshInterval = policyRefreshIntervalDefault
	}
}

func (s *policySource[P, B, E]) Run(ctx context.Context) error {
	if s.ctx != nil {
		return fmt.Errorf("policy source already running")
	}

	// Wait for initial cache sync of policies and informers before reconciling
	// any
	if !cache.WaitForNamedCacheSync(fmt.Sprintf("%T", s), ctx.Done(), s.UpstreamHasSynced) {
		err := ctx.Err()
		if err == nil {
			err = fmt.Errorf("initial cache sync for %T failed", s)
		}
		return err
	}

	s.ctx = ctx

	// Perform initial policy compilation after initial list has finished
	s.notify()
	s.refreshPolicies()

	notifyFuncs := cache.ResourceEventHandlerFuncs{
		AddFunc: func(_ interface{}) {
			s.notify()
		},
		UpdateFunc: func(_, _ interface{}) {
			s.notify()
		},
		DeleteFunc: func(_ interface{}) {
			s.notify()
		},
	}
	handle, err := s.policyInformer.AddEventHandler(notifyFuncs)
	if err != nil {
		return err
	}
	defer func() {
		if err := s.policyInformer.RemoveEventHandler(handle); err != nil {
			utilruntime.HandleError(fmt.Errorf("failed to remove policy event handler: %w", err))
		}
	}()

	bindingHandle, err := s.bindingInformer.AddEventHandler(notifyFuncs)
	if err != nil {
		return err
	}
	defer func() {
		if err := s.bindingInformer.RemoveEventHandler(bindingHandle); err != nil {
			utilruntime.HandleError(fmt.Errorf("failed to remove binding event handler: %w", err))
		}
	}()

	// Start a worker that checks every second to see if policy data is dirty
	// and needs to be recompiled
	go func() {
		// Loop every 1 second until context is cancelled, refreshing policies
		wait.Until(s.refreshPolicies, policyRefreshInterval, ctx.Done())
	}()

	<-ctx.Done()
	return nil
}

func (s *policySource[P, B, E]) UpstreamHasSynced() bool {
	return s.policyInformer.HasSynced() && s.bindingInformer.HasSynced()
}

// HasSynced implements Source.
func (s *policySource[P, B, E]) HasSynced() bool {
	// As an invariant we never store `nil` into the atomic list of processed
	// policy hooks. If it is nil, then we haven't compiled all the policies
	// and stored them yet.
	return s.Hooks() != nil
}

// Hooks implements Source.
func (s *policySource[P, B, E]) Hooks() []PolicyHook[P, B, E] {
	res := s.policies.Load()

	// Error case should not happen since evaluation function never
	// returns error
	if res == nil {
		// Not yet synced
		return nil
	}

	return *res
}

func (s *policySource[P, B, E]) refreshPolicies() {
	if !s.UpstreamHasSynced() {
		return
	} else if !s.policiesDirty.Swap(false) {
		return
	}

	// It is ok the cache gets marked dirty again between us clearing the
	// flag and us calculating the policies. The dirty flag would be marked again,
	// and we'd have a no-op after comparing resource versions on the next sync.
	klog.Infof("refreshing policies")
	policies, err := s.calculatePolicyData()

	// Intentionally store policy list regardless of error. There may be
	// an error returned if there was a configuration error in one of the policies,
	// but we would still want those policies evaluated
	// (for instance to return error on failaction). Or if there was an error
	// listing all policies at all, we would want to wipe the list.
	s.policies.Store(&policies)

	if err != nil {
		// An error was generated while syncing policies. Mark it as dirty again
		// so we can retry later
		utilruntime.HandleError(fmt.Errorf("encountered error syncing policies: %w. Rescheduling policy sync", err))
		s.notify()
	}
}

func (s *policySource[P, B, E]) notify() {
	s.policiesDirty.Store(true)
}

// calculatePolicyData calculates the list of policies and bindings for each
// policy. If there is an error in generation, it will return the error and
// the partial list of policies that were able to be generated. Policies that
// have an error will have a non-nil ConfigurationError field, but still be
// included in the result.
//
// This function caches the result of the intermediate compilations
func (s *policySource[P, B, E]) calculatePolicyData() ([]PolicyHook[P, B, E], error) {
	if !s.UpstreamHasSynced() {
		return nil, fmt.Errorf("cannot calculate policy data until upstream has synced")
	}

	// Fat-fingered lock that can be made more fine-tuned if required
	s.lock.Lock()
	defer s.lock.Unlock()

	// Create a local copy of all policies and bindings
	policiesToBindings := map[types.NamespacedName][]B{}
	bindingList, err := s.bindingInformer.List(labels.Everything())
	if err != nil {
		// This should never happen unless types are misconfigured
		// (can't use meta.accessor on them)
		return nil, err
	}

	// Gather a list of all active policy bindings
	for _, bindingSpec := range bindingList {
		bindingAccessor := s.newBindingAccessor(bindingSpec)
		policyKey := bindingAccessor.GetPolicyName()

		// Add this binding to the list of bindings for this policy
		policiesToBindings[policyKey] = append(policiesToBindings[policyKey], bindingSpec)
	}

	result := make([]PolicyHook[P, B, E], 0, len(bindingList))
	usedParams := map[schema.GroupVersionKind]struct{}{}
	var errs []error
	for policyKey, bindingSpecs := range policiesToBindings {
		var inf generic.NamespacedLister[P] = s.policyInformer
		if len(policyKey.Namespace) > 0 {
			inf = s.policyInformer.Namespaced(policyKey.Namespace)
		}
		policySpec, err := inf.Get(policyKey.Name)
		if errors.IsNotFound(err) {
			// Policy for bindings doesn't exist. This can happen if the policy
			// was deleted before the binding, or the binding was created first.
			//
			// Just skip bindings that refer to non-existent policies
			// If the policy is recreated, the cache will be marked dirty and
			// this function will run again.
			continue
		} else if err != nil {
			// This should never happen since fetching from a cache should never
			// fail and this function checks that the cache was synced before
			// even getting to this point.
			errs = append(errs, err)
			continue
		}

		var parsedParamKind *schema.GroupVersionKind
		policyAccessor := s.newPolicyAccessor(policySpec)

		if paramKind := policyAccessor.GetParamKind(); paramKind != nil {
			groupVersion, err := schema.ParseGroupVersion(paramKind.APIVersion)
			if err != nil {
				errs = append(errs, fmt.Errorf("failed to parse paramKind APIVersion: %w", err))
				continue
			}
			parsedParamKind = &schema.GroupVersionKind{
				Group:   groupVersion.Group,
				Version: groupVersion.Version,
				Kind:    paramKind.Kind,
			}

			// TEMPORARY UNTIL WE HAVE SHARED PARAM INFORMERS
			usedParams[*parsedParamKind] = struct{}{}
		}

		paramInformer, paramScope, configurationError := s.ensureParamsForPolicyLocked(parsedParamKind)
		result = append(result, PolicyHook[P, B, E]{
			Policy:             policySpec,
			Bindings:           bindingSpecs,
			Evaluator:          s.compilePolicyLocked(policySpec),
			ParamInformer:      paramInformer,
			ParamScope:         paramScope,
			ConfigurationError: configurationError,
		})

		// Should queue a re-sync for policy sync error. If our shared param
		// informer can notify us when CRD discovery changes we can remove this
		// and just rely on the informer to notify us when the CRDs change
		if configurationError != nil {
			errs = append(errs, configurationError)
		}
	}

	// Clean up orphaned policies by replacing the old cache of compiled policies
	// (the map of used policies is updated by `compilePolicy`)
	for policyKey := range s.compiledPolicies {
		if _, wasSeen := policiesToBindings[policyKey]; !wasSeen {
			delete(s.compiledPolicies, policyKey)
		}
	}

	// Clean up orphaned param informers
	for paramKind, info := range s.paramsCRDControllers {
		if _, wasSeen := usedParams[paramKind]; !wasSeen {
			info.cancelFunc()
			delete(s.paramsCRDControllers, paramKind)
		}
	}

	if s.updateHookMetrics != nil {
		s.updateHookMetrics(result)
	}

	err = nil
	if len(errs) > 0 {
		err = goerrors.Join(errs...)
	}
	return result, err
}

// ensureParamsForPolicyLocked ensures that the informer for the paramKind is
// started and returns the informer and the scope of the paramKind.
//
// Must be called under write lock
func (s *policySource[P, B, E]) ensureParamsForPolicyLocked(paramSource *schema.GroupVersionKind) (informers.GenericInformer, meta.RESTScope, error) {
	if paramSource == nil {
		return nil, nil, nil
	} else if info, ok := s.paramsCRDControllers[*paramSource]; ok {
		return info.informer, info.mapping.Scope, nil
	}

	mapping, err := s.restMapper.RESTMapping(schema.GroupKind{
		Group: paramSource.Group,
		Kind:  paramSource.Kind,
	}, paramSource.Version)

	if err != nil {
		// Failed to resolve. Return error so we retry again (rate limited)
		// Save a record of this definition with an evaluator that unconditionally
		return nil, nil, fmt.Errorf("failed to find resource referenced by paramKind: '%v'", *paramSource)
	}

	// We are not watching this param. Start an informer for it.
	instanceContext, instanceCancel := context.WithCancel(s.ctx)

	var informer informers.GenericInformer

	// Try to see if our provided informer factory has an informer for this type.
	// We assume the informer is already started, and starts all types associated
	// with it.
	if genericInformer, err := s.informerFactory.ForResource(mapping.Resource); err == nil {
		informer = genericInformer

		// Start the informer
		s.informerFactory.Start(instanceContext.Done())

	} else {
		// Dynamic JSON informer fallback.
		// Cannot use shared dynamic informer since it would be impossible
		// to clean CRD informers properly with multiple dependents
		// (cannot start ahead of time, and cannot track dependencies via stopCh)
		informer = dynamicinformer.NewFilteredDynamicInformer(
			s.dynamicClient,
			mapping.Resource,
			corev1.NamespaceAll,
			// Use same interval as is used for k8s typed sharedInformerFactory
			// https://github.com/kubernetes/kubernetes/blob/7e0923899fed622efbc8679cca6b000d43633e38/cmd/kube-apiserver/app/server.go#L430
			10*time.Minute,
			cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc},
			nil,
		)
		go informer.Informer().Run(instanceContext.Done())
	}

	klog.Infof("informer started for %v", *paramSource)
	ret := &paramInfo{
		mapping:    *mapping,
		cancelFunc: instanceCancel,
		informer:   informer,
	}
	s.paramsCRDControllers[*paramSource] = ret
	return ret.informer, mapping.Scope, nil
}

// For testing
func (s *policySource[P, B, E]) getParamInformer(param schema.GroupVersionKind) (informers.GenericInformer, meta.RESTScope) {
	s.lock.Lock()
	defer s.lock.Unlock()

	if info, ok := s.paramsCRDControllers[param]; ok {
		return info.informer, info.mapping.Scope
	}

	return nil, nil
}

// compilePolicyLocked compiles the policy and returns the evaluator for it.
// If the policy has not changed since the last compilation, it will return
// the cached evaluator.
//
// Must be called under write lock
func (s *policySource[P, B, E]) compilePolicyLocked(policySpec P) E {
	policyMeta, err := meta.Accessor(policySpec)
	if err != nil {
		// This should not happen if P, and B have ObjectMeta, but
		// unfortunately there is no way to express "able to call
		// meta.Accessor" as a type constraint
		utilruntime.HandleError(err)
		var emptyEvaluator E
		return emptyEvaluator
	}
	key := types.NamespacedName{
		Namespace: policyMeta.GetNamespace(),
		Name:      policyMeta.GetName(),
	}

	compiledPolicy, wasCompiled := s.compiledPolicies[key]

	// If the policy or binding has changed since it was last compiled,
	// and if there is no configuration error (like a missing param CRD)
	// then we recompile
	if !wasCompiled ||
		compiledPolicy.policyVersion != policyMeta.GetResourceVersion() {

		compiledPolicy = compiledPolicyEntry[E]{
			policyVersion: policyMeta.GetResourceVersion(),
			evaluator:     s.compiler(policySpec),
		}
		s.compiledPolicies[key] = compiledPolicy
	}

	return compiledPolicy.evaluator
}
