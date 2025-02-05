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

package generic_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"k8s.io/api/admissionregistration/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apiserver/pkg/admission"
	"k8s.io/apiserver/pkg/admission/plugin/policy/generic"
	"k8s.io/apiserver/pkg/admission/plugin/policy/matching"
	"k8s.io/apiserver/pkg/authorization/authorizer"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

type fakeDispatcher struct{}

func (fd *fakeDispatcher) Dispatch(context.Context, admission.Attributes, admission.ObjectInterfaces, []generic.PolicyHook[*FakePolicy, *FakeBinding, generic.Evaluator]) error {
	return nil
}
func (fd *fakeDispatcher) Start(context.Context) error {
	return nil
}

func makeTestDispatcher(authorizer.Authorizer, *matching.Matcher, kubernetes.Interface) generic.Dispatcher[generic.PolicyHook[*FakePolicy, *FakeBinding, generic.Evaluator]] {
	return &fakeDispatcher{}
}

func TestPolicySourceHasSyncedEmpty(t *testing.T) {
	testContext, testCancel, err := generic.NewPolicyTestContext(
		func(fp *FakePolicy) generic.PolicyAccessor { return fp },
		func(fb *FakeBinding) generic.BindingAccessor { return fb },
		func(fp *FakePolicy) generic.Evaluator { return nil },
		makeTestDispatcher,
		nil,
		nil,
	)
	require.NoError(t, err)
	defer testCancel()
	require.NoError(t, testContext.Start())

	// Should be able to wait for cache sync
	require.True(t, cache.WaitForCacheSync(testContext.Done(), testContext.Source.HasSynced), "cache should sync after informer running")
}

func TestPolicySourceHasSyncedInitialList(t *testing.T) {
	// Create a list of fake policies
	initialObjects := []runtime.Object{
		&FakePolicy{
			ObjectMeta: metav1.ObjectMeta{
				Name: "policy1",
			},
		},
		&FakeBinding{
			ObjectMeta: metav1.ObjectMeta{
				Name: "binding1",
			},
			PolicyName: "policy1",
		},
	}

	testContext, testCancel, err := generic.NewPolicyTestContext(
		func(fp *FakePolicy) generic.PolicyAccessor { return fp },
		func(fb *FakeBinding) generic.BindingAccessor { return fb },
		func(fp *FakePolicy) generic.Evaluator { return nil },
		makeTestDispatcher,
		initialObjects,
		nil,
	)
	require.NoError(t, err)
	defer testCancel()
	require.NoError(t, testContext.Start())
	// Should be able to wait for cache sync
	require.Len(t, testContext.Source.Hooks(), 1, "should have one policy")
	require.NoError(t, testContext.UpdateAndWait(
		&FakePolicy{
			ObjectMeta: metav1.ObjectMeta{
				Name: "policy2",
			},
		},
		&FakeBinding{
			ObjectMeta: metav1.ObjectMeta{
				Name: "binding2",
			},
			PolicyName: "policy2",
		},
	))
	require.Len(t, testContext.Source.Hooks(), 2, "should have two policies")
	require.NoError(t, testContext.UpdateAndWait(
		&FakePolicy{
			ObjectMeta: metav1.ObjectMeta{
				Name: "policy3",
			},
		},
		&FakeBinding{
			ObjectMeta: metav1.ObjectMeta{
				Name: "binding3",
			},
			PolicyName: "policy3",
		},
		&FakePolicy{
			ObjectMeta: metav1.ObjectMeta{
				Name: "policy2",
			},
			ParamKind: &v1.ParamKind{
				APIVersion: "policy.example.com/v1",
				Kind:       "FakeParam",
			},
		},
	))
	require.Len(t, testContext.Source.Hooks(), 3, "should have 3 policies")
	require.Len(t, testContext.MetricHookTracker.MetricHooks, 3, "should have 3 metric policies")
	require.Equal(t, testContext.Source.Hooks()[0].Bindings[0].GetName(),
		testContext.MetricHookTracker.MetricHooks[0].Bindings[0].GetName(),
		"First binding names should be the same")
	require.Equal(t, testContext.Source.Hooks()[1].Bindings[0].GetName(),
		testContext.MetricHookTracker.MetricHooks[1].Bindings[0].GetName(),
		"Second binding names should be the same")
	require.Equal(t, testContext.Source.Hooks()[2].Bindings[0].GetName(),
		testContext.MetricHookTracker.MetricHooks[2].Bindings[0].GetName(),
		"Third binding names should be the same")
}

func TestPolicySourceBindsToPolicies(t *testing.T) {
	// Create a list of fake policies
	initialObjects := []runtime.Object{
		&FakePolicy{
			ObjectMeta: metav1.ObjectMeta{
				Name: "policy1",
			},
		},
		&FakeBinding{
			ObjectMeta: metav1.ObjectMeta{
				Name: "binding1",
			},
			PolicyName: "policy1",
		},
	}

	testContext, testCancel, err := generic.NewPolicyTestContext(
		func(fp *FakePolicy) generic.PolicyAccessor { return fp },
		func(fb *FakeBinding) generic.BindingAccessor { return fb },
		func(fp *FakePolicy) generic.Evaluator { return nil },
		makeTestDispatcher,
		initialObjects,
		nil,
	)
	require.NoError(t, err)
	defer testCancel()
	require.NoError(t, testContext.Start())

	require.Len(t, testContext.Source.Hooks(), 1, "should have one policy")
	require.Len(t, testContext.Source.Hooks()[0].Bindings, 1, "should have one binding")
	require.Equal(t, "binding1", testContext.Source.Hooks()[0].Bindings[0].GetName(), "binding name should be binding1")

	require.Len(t, testContext.MetricHookTracker.MetricHooks, 1, "should have one metric policy")
	require.Len(t, testContext.MetricHookTracker.MetricHooks[0].Bindings, 1, "should have one metric binding")
	require.Equal(t, "binding1", testContext.MetricHookTracker.MetricHooks[0].Bindings[0].GetName(), "binding name should be binding1")

	// Change the binding to another policy (policies without bindings should
	// be ignored, so it should remove the first
	require.NoError(t, testContext.UpdateAndWait(
		&FakePolicy{
			ObjectMeta: metav1.ObjectMeta{
				Name: "policy2",
			},
		},
		&FakeBinding{
			ObjectMeta: metav1.ObjectMeta{
				Name: "binding1",
			},
			PolicyName: "policy2",
		}))
	require.Len(t, testContext.Source.Hooks(), 1, "should have one policy")
	require.Equal(t, "policy2", testContext.Source.Hooks()[0].Policy.GetName(), "policy name should be policy2")
	require.Len(t, testContext.Source.Hooks()[0].Bindings, 1, "should have one binding")
	require.Equal(t, "binding1", testContext.Source.Hooks()[0].Bindings[0].GetName(), "binding name should be binding1")

	require.Len(t, testContext.MetricHookTracker.MetricHooks, 1, "should have one metric policy")
	require.Equal(t, "policy2", testContext.MetricHookTracker.MetricHooks[0].Policy.GetName(), "metric policy name should be policy2")
	require.Len(t, testContext.MetricHookTracker.MetricHooks[0].Bindings, 1, "should have one metric binding")
	require.Equal(t, "binding1", testContext.MetricHookTracker.MetricHooks[0].Bindings[0].GetName(), "metric binding name should be binding1")
}

type FakePolicy struct {
	metav1.TypeMeta
	metav1.ObjectMeta

	ParamKind *v1.ParamKind
}

var _ generic.PolicyAccessor = &FakePolicy{}

type FakeBinding struct {
	metav1.TypeMeta
	metav1.ObjectMeta

	PolicyName string
}

var _ generic.BindingAccessor = &FakeBinding{}

func (fp *FakePolicy) GetName() string {
	return fp.Name
}

func (fp *FakePolicy) GetNamespace() string {
	return fp.Namespace
}

func (fp *FakePolicy) GetParamKind() *v1.ParamKind {
	return fp.ParamKind
}

func (fb *FakePolicy) GetMatchConstraints() *v1.MatchResources {
	return nil
}

func (fb *FakePolicy) GetFailurePolicy() *v1.FailurePolicyType {
	return nil
}

func (fb *FakeBinding) GetName() string {
	return fb.Name
}

func (fb *FakeBinding) GetNamespace() string {
	return fb.Namespace
}

func (fb *FakeBinding) GetPolicyName() types.NamespacedName {
	return types.NamespacedName{
		Name: fb.PolicyName,
	}
}

func (fb *FakeBinding) GetMatchResources() *v1.MatchResources {
	return nil
}

func (fb *FakeBinding) GetParamRef() *v1.ParamRef {
	return nil
}

func (fp *FakePolicy) DeepCopyObject() runtime.Object {
	// totally fudged deepcopy
	newFP := &FakePolicy{}
	*newFP = *fp
	return newFP
}

func (fb *FakeBinding) DeepCopyObject() runtime.Object {
	// totally fudged deepcopy
	newFB := &FakeBinding{}
	*newFB = *fb
	return newFB
}
