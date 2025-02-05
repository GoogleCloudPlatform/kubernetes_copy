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

package validating

import (
	"context"
	"io"
	"sync"

	v1 "k8s.io/api/admissionregistration/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apiserver/pkg/admission"
	"k8s.io/apiserver/pkg/admission/initializer"
	"k8s.io/apiserver/pkg/admission/plugin/cel"
	"k8s.io/apiserver/pkg/admission/plugin/policy/generic"
	"k8s.io/apiserver/pkg/admission/plugin/policy/matching"
	celmetrics "k8s.io/apiserver/pkg/admission/plugin/policy/validating/metrics"
	"k8s.io/apiserver/pkg/admission/plugin/webhook/matchconditions"
	"k8s.io/apiserver/pkg/authorization/authorizer"
	"k8s.io/apiserver/pkg/cel/environment"
	"k8s.io/apiserver/pkg/features"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
)

const (
	// PluginName indicates the name of admission plug-in
	PluginName = "ValidatingAdmissionPolicy"
)

var (
	lazyCompositionEnvTemplateWithStrictCostInit sync.Once
	lazyCompositionEnvTemplateWithStrictCost     *cel.CompositionEnv

	lazyCompositionEnvTemplateWithoutStrictCostInit sync.Once
	lazyCompositionEnvTemplateWithoutStrictCost     *cel.CompositionEnv
)

func getCompositionEnvTemplateWithStrictCost() *cel.CompositionEnv {
	lazyCompositionEnvTemplateWithStrictCostInit.Do(func() {
		env, err := cel.NewCompositionEnv(cel.VariablesTypeName, environment.MustBaseEnvSet(environment.DefaultCompatibilityVersion(), true))
		if err != nil {
			panic(err)
		}
		lazyCompositionEnvTemplateWithStrictCost = env
	})
	return lazyCompositionEnvTemplateWithStrictCost
}

func getCompositionEnvTemplateWithoutStrictCost() *cel.CompositionEnv {
	lazyCompositionEnvTemplateWithoutStrictCostInit.Do(func() {
		env, err := cel.NewCompositionEnv(cel.VariablesTypeName, environment.MustBaseEnvSet(environment.DefaultCompatibilityVersion(), false))
		if err != nil {
			panic(err)
		}
		lazyCompositionEnvTemplateWithoutStrictCost = env
	})
	return lazyCompositionEnvTemplateWithoutStrictCost
}

// Register registers a plugin
func Register(plugins *admission.Plugins) {
	celmetrics.Metrics.Register()
	plugins.Register(PluginName, func(configFile io.Reader) (admission.Interface, error) {
		return NewPlugin(configFile), nil
	})
}

// Plugin is an implementation of admission.Interface.
type Policy = v1.ValidatingAdmissionPolicy
type PolicyBinding = v1.ValidatingAdmissionPolicyBinding
type PolicyEvaluator = Validator
type PolicyHook = generic.PolicyHook[*Policy, *PolicyBinding, PolicyEvaluator]

type Plugin struct {
	*generic.Plugin[PolicyHook]
}

var _ admission.Interface = &Plugin{}
var _ admission.ValidationInterface = &Plugin{}
var _ initializer.WantsExcludedAdmissionResources = &Plugin{}

func getValidatingAction(validationActions []v1.ValidationAction) *v1.ValidationAction {
	for _, action := range validationActions {
		if action == v1.Deny || action == v1.Warn {
			return &action
		}
	}
	return nil
}

func NewPlugin(_ io.Reader) *Plugin {
	handler := admission.NewHandler(admission.Connect, admission.Create, admission.Delete, admission.Update)

	p := &Plugin{
		Plugin: generic.NewPlugin(
			handler,
			func(f informers.SharedInformerFactory, client kubernetes.Interface, dynamicClient dynamic.Interface, restMapper meta.RESTMapper) generic.Source[PolicyHook] {
				return generic.NewPolicySource(
					f.Admissionregistration().V1().ValidatingAdmissionPolicies().Informer(),
					f.Admissionregistration().V1().ValidatingAdmissionPolicyBindings().Informer(),
					NewValidatingAdmissionPolicyAccessor,
					NewValidatingAdmissionPolicyBindingAccessor,
					compilePolicy,
					f,
					func(hooks []generic.PolicyHook[*Policy, *PolicyBinding, PolicyEvaluator]) {
						var allowCount, warnCount, denyCount int
						policyCount := len(hooks)
						for _, hook := range hooks {
							for _, binding := range hook.Bindings {
								action := getValidatingAction(binding.Spec.ValidationActions)
								switch {
								case action == nil:
									allowCount++
								case *action == v1.Warn:
									warnCount++
								case *action == v1.Deny:
									denyCount++
								}

							}
						}
						celmetrics.Metrics.ObserveActiveBindings(policyCount, allowCount, warnCount, denyCount)
					},
					dynamicClient,
					restMapper,
				)
			},
			func(a authorizer.Authorizer, m *matching.Matcher, client kubernetes.Interface) generic.Dispatcher[PolicyHook] {
				return NewDispatcher(a, generic.NewPolicyMatcher(m))
			},
		),
	}
	p.SetEnabled(true)
	return p
}

// Validate makes an admission decision based on the request attributes.
func (a *Plugin) Validate(ctx context.Context, attr admission.Attributes, o admission.ObjectInterfaces) error {
	return a.Plugin.Dispatch(ctx, attr, o)
}

func compilePolicy(policy *Policy) Validator {
	hasParam := false
	if policy.Spec.ParamKind != nil {
		hasParam = true
	}
	strictCost := utilfeature.DefaultFeatureGate.Enabled(features.StrictCostEnforcementForVAP)
	optionalVars := cel.OptionalVariableDeclarations{HasParams: hasParam, HasAuthorizer: true, StrictCost: strictCost}
	expressionOptionalVars := cel.OptionalVariableDeclarations{HasParams: hasParam, HasAuthorizer: false, StrictCost: strictCost}
	failurePolicy := policy.Spec.FailurePolicy
	var matcher matchconditions.Matcher = nil
	matchConditions := policy.Spec.MatchConditions
	var compositionEnvTemplate *cel.CompositionEnv
	if strictCost {
		compositionEnvTemplate = getCompositionEnvTemplateWithStrictCost()
	} else {
		compositionEnvTemplate = getCompositionEnvTemplateWithoutStrictCost()
	}
	filterCompiler := cel.NewCompositedCompilerFromTemplate(compositionEnvTemplate)
	filterCompiler.CompileAndStoreVariables(convertv1beta1Variables(policy.Spec.Variables), optionalVars, environment.StoredExpressions)

	if len(matchConditions) > 0 {
		matchExpressionAccessors := make([]cel.ExpressionAccessor, len(matchConditions))
		for i := range matchConditions {
			matchExpressionAccessors[i] = (*matchconditions.MatchCondition)(&matchConditions[i])
		}
		matcher = matchconditions.NewMatcher(filterCompiler.CompileCondition(matchExpressionAccessors, optionalVars, environment.StoredExpressions), failurePolicy, "policy", "validate", policy.Name)
	}
	res := NewValidator(
		filterCompiler.CompileCondition(convertv1Validations(policy.Spec.Validations), optionalVars, environment.StoredExpressions),
		matcher,
		filterCompiler.CompileCondition(convertv1AuditAnnotations(policy.Spec.AuditAnnotations), optionalVars, environment.StoredExpressions),
		filterCompiler.CompileCondition(convertv1MessageExpressions(policy.Spec.Validations), expressionOptionalVars, environment.StoredExpressions),
		failurePolicy,
	)

	return res
}

func convertv1Validations(inputValidations []v1.Validation) []cel.ExpressionAccessor {
	celExpressionAccessor := make([]cel.ExpressionAccessor, len(inputValidations))
	for i, validation := range inputValidations {
		validation := ValidationCondition{
			Expression: validation.Expression,
			Message:    validation.Message,
			Reason:     validation.Reason,
		}
		celExpressionAccessor[i] = &validation
	}
	return celExpressionAccessor
}

func convertv1MessageExpressions(inputValidations []v1.Validation) []cel.ExpressionAccessor {
	celExpressionAccessor := make([]cel.ExpressionAccessor, len(inputValidations))
	for i, validation := range inputValidations {
		if validation.MessageExpression != "" {
			condition := MessageExpressionCondition{
				MessageExpression: validation.MessageExpression,
			}
			celExpressionAccessor[i] = &condition
		}
	}
	return celExpressionAccessor
}

func convertv1AuditAnnotations(inputValidations []v1.AuditAnnotation) []cel.ExpressionAccessor {
	celExpressionAccessor := make([]cel.ExpressionAccessor, len(inputValidations))
	for i, validation := range inputValidations {
		validation := AuditAnnotationCondition{
			Key:             validation.Key,
			ValueExpression: validation.ValueExpression,
		}
		celExpressionAccessor[i] = &validation
	}
	return celExpressionAccessor
}

func convertv1beta1Variables(variables []v1.Variable) []cel.NamedExpressionAccessor {
	namedExpressions := make([]cel.NamedExpressionAccessor, len(variables))
	for i, variable := range variables {
		namedExpressions[i] = &Variable{Name: variable.Name, Expression: variable.Expression}
	}
	return namedExpressions
}
