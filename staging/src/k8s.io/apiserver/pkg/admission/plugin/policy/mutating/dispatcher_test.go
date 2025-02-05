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

package mutating

import (
	"context"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	"k8s.io/api/admissionregistration/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apiserver/pkg/admission"
	"k8s.io/apiserver/pkg/admission/plugin/policy/generic"
	"k8s.io/apiserver/pkg/admission/plugin/policy/matching"
	"k8s.io/apiserver/pkg/admission/plugin/policy/mutating/patch"
	"k8s.io/apiserver/pkg/admission/plugin/webhook/predicates/namespace"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/openapi/openapitest"
	"k8s.io/utils/ptr"
)

func TestDispatcher(t *testing.T) {
	deploymentGVK := schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"}
	deploymentGVR := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
	testCases := []struct {
		name              string
		object, oldObject runtime.Object
		gvk               schema.GroupVersionKind
		gvr               schema.GroupVersionResource
		params            []runtime.Object // All params are expected to be ConfigMap for this test.
		policyHooks       []PolicyHook
		expect            runtime.Object
	}{
		{
			name: "simple patch",
			gvk:  deploymentGVK,
			gvr:  deploymentGVR,
			object: &appsv1.Deployment{
				TypeMeta: metav1.TypeMeta{
					Kind:       "Deployment",
					APIVersion: "apps/v1",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:      "d1",
					Namespace: "default",
				},
				Spec: appsv1.DeploymentSpec{
					Replicas: ptr.To[int32](1),
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Volumes: []corev1.Volume{{Name: "x"}},
						},
					},
				}},
			policyHooks: []generic.PolicyHook[*Policy, *PolicyBinding, PolicyEvaluator]{
				{
					Policy: mutations(matchConstraints(policy("policy1"), &v1alpha1.MatchResources{
						MatchPolicy:       ptr.To(v1alpha1.Equivalent),
						NamespaceSelector: &metav1.LabelSelector{},
						ObjectSelector:    &metav1.LabelSelector{},
						ResourceRules: []v1alpha1.NamedRuleWithOperations{
							{
								RuleWithOperations: v1alpha1.RuleWithOperations{
									Rule: v1alpha1.Rule{
										APIGroups:   []string{"apps"},
										APIVersions: []string{"v1"},
										Resources:   []string{"deployments"},
									},
									Operations: []admissionregistrationv1.OperationType{"*"},
								},
							},
						},
					}), v1alpha1.Mutation{
						PatchType: v1alpha1.PatchTypeApplyConfiguration,
						ApplyConfiguration: &v1alpha1.ApplyConfiguration{
							Expression: `Object{
									spec: Object.spec{
										replicas: object.spec.replicas + 100
									}
								}`,
						}}),
					Bindings: []*PolicyBinding{{
						ObjectMeta: metav1.ObjectMeta{Name: "binding"},
						Spec: v1alpha1.MutatingAdmissionPolicyBindingSpec{
							PolicyName: "policy1",
						},
					}},
				},
			},
			expect: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "d1",
					Namespace: "default",
				},
				Spec: appsv1.DeploymentSpec{
					Replicas: ptr.To[int32](101),
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Volumes: []corev1.Volume{{Name: "x"}},
						},
					},
					Strategy: appsv1.DeploymentStrategy{
						Type: appsv1.RollingUpdateDeploymentStrategyType,
					},
				}},
		},
		{
			name: "with param",
			gvk:  deploymentGVK,
			gvr:  deploymentGVR,
			object: &appsv1.Deployment{
				TypeMeta: metav1.TypeMeta{
					Kind:       "Deployment",
					APIVersion: "apps/v1",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:      "d1",
					Namespace: "default",
				},
				Spec: appsv1.DeploymentSpec{
					Replicas: ptr.To[int32](1),
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Volumes: []corev1.Volume{{Name: "x"}},
						},
					},
					Strategy: appsv1.DeploymentStrategy{
						Type: appsv1.RollingUpdateDeploymentStrategyType,
					},
				}},
			params: []runtime.Object{
				&corev1.ConfigMap{
					TypeMeta: metav1.TypeMeta{
						APIVersion: "v1",
						Kind:       "ConfigMap",
					},
					ObjectMeta: metav1.ObjectMeta{
						Name:      "cm1",
						Namespace: "default",
					},
					Data: map[string]string{
						"key": "10",
					},
				},
			},
			policyHooks: []generic.PolicyHook[*Policy, *PolicyBinding, PolicyEvaluator]{
				{
					Policy: paramKind(mutations(matchConstraints(policy("policy1"), &v1alpha1.MatchResources{
						MatchPolicy:       ptr.To(v1alpha1.Equivalent),
						NamespaceSelector: &metav1.LabelSelector{},
						ObjectSelector:    &metav1.LabelSelector{},
						ResourceRules: []v1alpha1.NamedRuleWithOperations{
							{
								RuleWithOperations: v1alpha1.RuleWithOperations{
									Rule: v1alpha1.Rule{
										APIGroups:   []string{"apps"},
										APIVersions: []string{"v1"},
										Resources:   []string{"deployments"},
									},
									Operations: []admissionregistrationv1.OperationType{"*"},
								},
							},
						}}),
						v1alpha1.Mutation{
							PatchType: v1alpha1.PatchTypeApplyConfiguration,
							ApplyConfiguration: &v1alpha1.ApplyConfiguration{
								Expression: `Object{
									spec: Object.spec{
										replicas: object.spec.replicas + int(params.data['key'])
									}
								}`,
							}}),
						&v1alpha1.ParamKind{
							APIVersion: "v1",
							Kind:       "ConfigMap",
						}),
					Bindings: []*PolicyBinding{{
						ObjectMeta: metav1.ObjectMeta{Name: "binding"},
						Spec: v1alpha1.MutatingAdmissionPolicyBindingSpec{
							PolicyName: "policy1",
							ParamRef:   &v1alpha1.ParamRef{Name: "cm1", Namespace: "default"},
						},
					}},
				},
			},
			expect: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "d1",
					Namespace: "default",
				},
				Spec: appsv1.DeploymentSpec{
					Replicas: ptr.To[int32](11),
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Volumes: []corev1.Volume{{Name: "x"}},
						},
					},
					Strategy: appsv1.DeploymentStrategy{
						Type: appsv1.RollingUpdateDeploymentStrategyType,
					},
				}},
		},
		{
			name: "both policies reinvoked",
			gvk:  deploymentGVK,
			gvr:  deploymentGVR,
			object: &appsv1.Deployment{
				TypeMeta: metav1.TypeMeta{
					Kind:       "Deployment",
					APIVersion: "apps/v1",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:      "d1",
					Namespace: "default",
				},
				Spec: appsv1.DeploymentSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Volumes: []corev1.Volume{{Name: "x"}},
						},
					},
					Strategy: appsv1.DeploymentStrategy{
						Type: appsv1.RollingUpdateDeploymentStrategyType,
					},
				}},
			policyHooks: []generic.PolicyHook[*Policy, *PolicyBinding, PolicyEvaluator]{
				{
					Policy: mutations(matchConstraints(policy("policy1"), &v1alpha1.MatchResources{
						MatchPolicy:       ptr.To(v1alpha1.Equivalent),
						NamespaceSelector: &metav1.LabelSelector{},
						ObjectSelector:    &metav1.LabelSelector{},
						ResourceRules: []v1alpha1.NamedRuleWithOperations{
							{
								RuleWithOperations: v1alpha1.RuleWithOperations{
									Rule: v1alpha1.Rule{
										APIGroups:   []string{"apps"},
										APIVersions: []string{"v1"},
										Resources:   []string{"deployments"},
									},
									Operations: []admissionregistrationv1.OperationType{"*"},
								},
							},
						},
					}), v1alpha1.Mutation{
						PatchType: v1alpha1.PatchTypeApplyConfiguration,
						ApplyConfiguration: &v1alpha1.ApplyConfiguration{
							Expression: `Object{
									metadata: Object.metadata{
										labels: {"policy1": string(int(object.?metadata.labels["count"].orValue("1")) + 1)}
									}
								}`,
						}}),
					Bindings: []*PolicyBinding{{
						ObjectMeta: metav1.ObjectMeta{Name: "binding"},
						Spec: v1alpha1.MutatingAdmissionPolicyBindingSpec{
							PolicyName: "policy1",
						},
					}},
				},
				{
					Policy: mutations(matchConstraints(policy("policy2"), &v1alpha1.MatchResources{
						MatchPolicy:       ptr.To(v1alpha1.Equivalent),
						NamespaceSelector: &metav1.LabelSelector{},
						ObjectSelector:    &metav1.LabelSelector{},
						ResourceRules: []v1alpha1.NamedRuleWithOperations{
							{
								RuleWithOperations: v1alpha1.RuleWithOperations{
									Rule: v1alpha1.Rule{
										APIGroups:   []string{"apps"},
										APIVersions: []string{"v1"},
										Resources:   []string{"deployments"},
									},
									Operations: []admissionregistrationv1.OperationType{"*"},
								},
							},
						},
					}), v1alpha1.Mutation{
						PatchType: v1alpha1.PatchTypeApplyConfiguration,
						ApplyConfiguration: &v1alpha1.ApplyConfiguration{
							Expression: `Object{
									metadata: Object.metadata{
										labels: {"policy2": string(int(object.?metadata.labels["count"].orValue("1")) + 1)}
									}
								}`,
						}}),
					Bindings: []*PolicyBinding{{
						ObjectMeta: metav1.ObjectMeta{Name: "binding"},
						Spec: v1alpha1.MutatingAdmissionPolicyBindingSpec{
							PolicyName: "policy2",
						},
					}},
				},
			},
			expect: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "d1",
					Namespace: "default",
					Labels: map[string]string{
						"policy1": "2",
						"policy2": "2",
					},
				},
				Spec: appsv1.DeploymentSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Volumes: []corev1.Volume{{Name: "x"}},
						},
					},
					Strategy: appsv1.DeploymentStrategy{
						Type: appsv1.RollingUpdateDeploymentStrategyType,
					},
				}},
		},
		{
			name: "1st policy sets match condition that 2nd policy matches",
			gvk:  deploymentGVK,
			gvr:  deploymentGVR,
			object: &appsv1.Deployment{
				TypeMeta: metav1.TypeMeta{
					Kind:       "Deployment",
					APIVersion: "apps/v1",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:      "d1",
					Namespace: "default",
				},
				Spec: appsv1.DeploymentSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Volumes: []corev1.Volume{{Name: "x"}},
						},
					},
					Strategy: appsv1.DeploymentStrategy{
						Type: appsv1.RollingUpdateDeploymentStrategyType,
					},
				}},
			policyHooks: []generic.PolicyHook[*Policy, *PolicyBinding, PolicyEvaluator]{
				{
					Policy: &v1alpha1.MutatingAdmissionPolicy{
						ObjectMeta: metav1.ObjectMeta{
							Name: "policy1",
						},
						Spec: v1alpha1.MutatingAdmissionPolicySpec{
							MatchConstraints: &v1alpha1.MatchResources{
								MatchPolicy:       ptr.To(v1alpha1.Equivalent),
								NamespaceSelector: &metav1.LabelSelector{},
								ObjectSelector:    &metav1.LabelSelector{},
								ResourceRules: []v1alpha1.NamedRuleWithOperations{
									{
										RuleWithOperations: v1alpha1.RuleWithOperations{
											Rule: v1alpha1.Rule{
												APIGroups:   []string{"apps"},
												APIVersions: []string{"v1"},
												Resources:   []string{"deployments"},
											},
											Operations: []admissionregistrationv1.OperationType{"*"},
										},
									},
								},
							},
							Mutations: []v1alpha1.Mutation{{
								PatchType: v1alpha1.PatchTypeApplyConfiguration,
								ApplyConfiguration: &v1alpha1.ApplyConfiguration{
									Expression: `Object{
									metadata: Object.metadata{
										labels: {"environment": "production"}
									}
								}`}},
							},
						},
					},
					Bindings: []*PolicyBinding{{
						ObjectMeta: metav1.ObjectMeta{Name: "binding"},
						Spec: v1alpha1.MutatingAdmissionPolicyBindingSpec{
							PolicyName: "policy1",
						},
					}},
				},
				{
					Policy: &v1alpha1.MutatingAdmissionPolicy{
						ObjectMeta: metav1.ObjectMeta{
							Name: "policy2",
						},
						Spec: v1alpha1.MutatingAdmissionPolicySpec{
							MatchConstraints: &v1alpha1.MatchResources{
								MatchPolicy:       ptr.To(v1alpha1.Equivalent),
								NamespaceSelector: &metav1.LabelSelector{},
								ObjectSelector:    &metav1.LabelSelector{},
								ResourceRules: []v1alpha1.NamedRuleWithOperations{
									{
										RuleWithOperations: v1alpha1.RuleWithOperations{
											Rule: v1alpha1.Rule{
												APIGroups:   []string{"apps"},
												APIVersions: []string{"v1"},
												Resources:   []string{"deployments"},
											},
											Operations: []admissionregistrationv1.OperationType{"*"},
										},
									},
								},
							},
							MatchConditions: []v1alpha1.MatchCondition{
								{
									Name:       "prodonly",
									Expression: `object.?metadata.labels["environment"].orValue("") == "production"`,
								},
							},
							Mutations: []v1alpha1.Mutation{{
								PatchType: v1alpha1.PatchTypeApplyConfiguration,
								ApplyConfiguration: &v1alpha1.ApplyConfiguration{
									Expression: `Object{
									metadata: Object.metadata{
										labels: {"policy1invoked": "true"}
									}
								}`}},
							},
						},
					},
					Bindings: []*PolicyBinding{{
						ObjectMeta: metav1.ObjectMeta{Name: "binding"},
						Spec: v1alpha1.MutatingAdmissionPolicyBindingSpec{
							PolicyName: "policy2",
						},
					}},
				},
			},
			expect: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "d1",
					Namespace: "default",
					Labels: map[string]string{
						"environment":    "production",
						"policy1invoked": "true",
					},
				},
				Spec: appsv1.DeploymentSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Volumes: []corev1.Volume{{Name: "x"}},
						},
					},
					Strategy: appsv1.DeploymentStrategy{
						Type: appsv1.RollingUpdateDeploymentStrategyType,
					},
				}},
		},
		{
			// TODO: This behavior pre-exists with webhook match conditions but should be reconsidered
			name: "1st policy still does not match after 2nd policy sets match condition",
			gvk:  deploymentGVK,
			gvr:  deploymentGVR,
			object: &appsv1.Deployment{
				TypeMeta: metav1.TypeMeta{
					Kind:       "Deployment",
					APIVersion: "apps/v1",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:      "d1",
					Namespace: "default",
				},
				Spec: appsv1.DeploymentSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Volumes: []corev1.Volume{{Name: "x"}},
						},
					},
					Strategy: appsv1.DeploymentStrategy{
						Type: appsv1.RollingUpdateDeploymentStrategyType,
					},
				}},
			policyHooks: []generic.PolicyHook[*Policy, *PolicyBinding, PolicyEvaluator]{
				{
					Policy: &v1alpha1.MutatingAdmissionPolicy{
						ObjectMeta: metav1.ObjectMeta{
							Name: "policy1",
						},
						Spec: v1alpha1.MutatingAdmissionPolicySpec{
							MatchConstraints: &v1alpha1.MatchResources{
								MatchPolicy:       ptr.To(v1alpha1.Equivalent),
								NamespaceSelector: &metav1.LabelSelector{},
								ObjectSelector:    &metav1.LabelSelector{},
								ResourceRules: []v1alpha1.NamedRuleWithOperations{
									{
										RuleWithOperations: v1alpha1.RuleWithOperations{
											Rule: v1alpha1.Rule{
												APIGroups:   []string{"apps"},
												APIVersions: []string{"v1"},
												Resources:   []string{"deployments"},
											},
											Operations: []admissionregistrationv1.OperationType{"*"},
										},
									},
								},
							},
							MatchConditions: []v1alpha1.MatchCondition{
								{
									Name:       "prodonly",
									Expression: `object.?metadata.labels["environment"].orValue("") == "production"`,
								},
							},
							Mutations: []v1alpha1.Mutation{{
								PatchType: v1alpha1.PatchTypeApplyConfiguration,
								ApplyConfiguration: &v1alpha1.ApplyConfiguration{
									Expression: `Object{
									metadata: Object.metadata{
										labels: {"policy1invoked": "true"}
									}
								}`}},
							},
						},
					},
					Bindings: []*PolicyBinding{{
						ObjectMeta: metav1.ObjectMeta{Name: "binding"},
						Spec: v1alpha1.MutatingAdmissionPolicyBindingSpec{
							PolicyName: "policy1",
						},
					}},
				},
				{
					Policy: &v1alpha1.MutatingAdmissionPolicy{
						ObjectMeta: metav1.ObjectMeta{
							Name: "policy2",
						},
						Spec: v1alpha1.MutatingAdmissionPolicySpec{
							MatchConstraints: &v1alpha1.MatchResources{
								MatchPolicy:       ptr.To(v1alpha1.Equivalent),
								NamespaceSelector: &metav1.LabelSelector{},
								ObjectSelector:    &metav1.LabelSelector{},
								ResourceRules: []v1alpha1.NamedRuleWithOperations{
									{
										RuleWithOperations: v1alpha1.RuleWithOperations{
											Rule: v1alpha1.Rule{
												APIGroups:   []string{"apps"},
												APIVersions: []string{"v1"},
												Resources:   []string{"deployments"},
											},
											Operations: []admissionregistrationv1.OperationType{"*"},
										},
									},
								},
							},
							Mutations: []v1alpha1.Mutation{{
								PatchType: v1alpha1.PatchTypeApplyConfiguration,
								ApplyConfiguration: &v1alpha1.ApplyConfiguration{
									Expression: `Object{
									metadata: Object.metadata{
										labels: {"environment": "production"}
									}
								}`}},
							},
						},
					},
					Bindings: []*PolicyBinding{{
						ObjectMeta: metav1.ObjectMeta{Name: "binding"},
						Spec: v1alpha1.MutatingAdmissionPolicyBindingSpec{
							PolicyName: "policy2",
						},
					}},
				},
			},
			expect: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "d1",
					Namespace: "default",
					Labels: map[string]string{
						"environment": "production",
					},
				},
				Spec: appsv1.DeploymentSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Volumes: []corev1.Volume{{Name: "x"}},
						},
					},
					Strategy: appsv1.DeploymentStrategy{
						Type: appsv1.RollingUpdateDeploymentStrategyType,
					},
				}},
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tcManager := patch.NewTypeConverterManager(nil, openapitest.NewEmbeddedFileClient())
	go tcManager.Run(ctx)

	err := wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, time.Second, true, func(context.Context) (done bool, err error) {
		converter := tcManager.GetTypeConverter(deploymentGVK)
		return converter != nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}

	scheme := runtime.NewScheme()
	err = appsv1.AddToScheme(scheme)
	if err != nil {
		t.Fatal(err)
	}
	err = corev1.AddToScheme(scheme)
	if err != nil {
		t.Fatal(err)
	}

	// Register a fake defaulter since registering the full defaulter adds noise
	// and creates dep cycles.
	scheme.AddTypeDefaultingFunc(&appsv1.Deployment{},
		func(obj interface{}) { fakeSetDefaultForDeployment(obj.(*appsv1.Deployment)) })

	objectInterfaces := admission.NewObjectInterfacesFromScheme(scheme)

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			client := fake.NewClientset(tc.params...)

			// always include default namespace
			err := client.Tracker().Add(&corev1.Namespace{
				TypeMeta: metav1.TypeMeta{
					APIVersion: "v1",
					Kind:       "Namespace",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name: "default",
				},
				Spec: corev1.NamespaceSpec{},
			})
			if err != nil {
				t.Fatal(err)
			}

			informerFactory := informers.NewSharedInformerFactory(client, 0)
			namespaceProvider := namespace.Provider{
				NamespaceLister: informerFactory.Core().V1().Namespaces().Lister(),
				Client:          client,
			}
			matcher := matching.NewMatcher(&namespaceProvider)
			paramInformer, err := informerFactory.ForResource(schema.GroupVersionResource{Version: "v1", Resource: "configmaps"})
			if err != nil {
				t.Fatal(err)
			}
			informerFactory.WaitForCacheSync(ctx.Done())
			informerFactory.Start(ctx.Done())
			for i, h := range tc.policyHooks {
				tc.policyHooks[i].ParamInformer = paramInformer
				tc.policyHooks[i].ParamScope = testParamScope{}
				tc.policyHooks[i].Evaluator = compilePolicy(h.Policy)
			}

			dispatcher := NewDispatcher(fakeAuthorizer{}, matcher, tcManager)
			err = dispatcher.Start(ctx)
			if err != nil {
				t.Fatalf("error starting dispatcher: %v", err)
			}

			metaAccessor, err := meta.Accessor(tc.object)
			if err != nil {
				t.Fatal(err)
			}

			attrs := admission.NewAttributesRecord(tc.object, tc.oldObject, tc.gvk,
				metaAccessor.GetNamespace(), metaAccessor.GetName(), tc.gvr,
				"", admission.Create, &metav1.CreateOptions{}, false, nil)
			vAttrs := &admission.VersionedAttributes{
				Attributes:         attrs,
				VersionedKind:      tc.gvk,
				VersionedObject:    tc.object,
				VersionedOldObject: tc.oldObject,
			}

			err = dispatcher.Dispatch(ctx, vAttrs, objectInterfaces, tc.policyHooks)
			if err != nil {
				t.Fatalf("error dispatching policy hooks: %v", err)
			}

			obj := vAttrs.VersionedObject
			if !equality.Semantic.DeepEqual(obj, tc.expect) {
				t.Errorf("unexpected result, got diff:\n%s\n", cmp.Diff(tc.expect, obj))
			}
		})
	}
}

type testParamScope struct{}

func (t testParamScope) Name() meta.RESTScopeName {
	return meta.RESTScopeNameNamespace
}

var _ meta.RESTScope = testParamScope{}

func fakeSetDefaultForDeployment(obj *appsv1.Deployment) {
	// Just default strategy type so the tests have a defaulted field to observe
	strategy := &obj.Spec.Strategy
	if strategy.Type == "" {
		strategy.Type = appsv1.RollingUpdateDeploymentStrategyType
	}
}
