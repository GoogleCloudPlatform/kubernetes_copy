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

package validating

import (
	"context"
	"errors"
	"fmt"
	"strings"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	v1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utiljson "k8s.io/apimachinery/pkg/util/json"
	"k8s.io/apiserver/pkg/admission"
	admissionauthorizer "k8s.io/apiserver/pkg/admission/plugin/authorizer"
	"k8s.io/apiserver/pkg/admission/plugin/policy/generic"
	celmetrics "k8s.io/apiserver/pkg/admission/plugin/policy/validating/metrics"
	celconfig "k8s.io/apiserver/pkg/apis/cel"
	"k8s.io/apiserver/pkg/authorization/authorizer"
	"k8s.io/apiserver/pkg/warning"
	"k8s.io/klog/v2"
)

type dispatcher struct {
	matcher generic.PolicyMatcher
	authz   authorizer.Authorizer
}

var _ generic.Dispatcher[PolicyHook] = &dispatcher{}

func NewDispatcher(
	authorizer authorizer.Authorizer,
	matcher generic.PolicyMatcher,
) generic.Dispatcher[PolicyHook] {
	return &dispatcher{
		matcher: matcher,
		authz:   authorizer,
	}
}

// contains the cel PolicyDecisions along with the ValidatingAdmissionPolicy and ValidatingAdmissionPolicyBinding
// that determined the decision
type policyDecisionWithMetadata struct {
	PolicyDecision
	Definition *admissionregistrationv1.ValidatingAdmissionPolicy
	Binding    *admissionregistrationv1.ValidatingAdmissionPolicyBinding
}

type validateResultWithParam struct {
	Result         ValidateResult
	ParamNamespace string
	ParamName      string
}

func (c *dispatcher) Start(ctx context.Context) error {
	return nil
}

// Dispatch implements generic.Dispatcher.
func (c *dispatcher) Dispatch(ctx context.Context, a admission.Attributes, o admission.ObjectInterfaces, hooks []PolicyHook) error {

	var deniedDecisions []policyDecisionWithMetadata

	addConfigError := func(err error, definition *admissionregistrationv1.ValidatingAdmissionPolicy, binding *admissionregistrationv1.ValidatingAdmissionPolicyBinding) {
		// we always default the FailurePolicy if it is unset and validate it in API level
		var policy admissionregistrationv1.FailurePolicyType
		if definition.Spec.FailurePolicy == nil {
			policy = admissionregistrationv1.Fail
		} else {
			policy = *definition.Spec.FailurePolicy
		}

		// apply FailurePolicy specified in ValidatingAdmissionPolicy, the default would be Fail
		switch policy {
		case admissionregistrationv1.Ignore:
			// TODO: add metrics for ignored error here
			return
		case admissionregistrationv1.Fail:
			var message string
			if binding == nil {
				message = fmt.Errorf("failed to configure policy: %w", err).Error()
			} else {
				message = fmt.Errorf("failed to configure binding: %w", err).Error()
			}
			deniedDecisions = append(deniedDecisions, policyDecisionWithMetadata{
				PolicyDecision: PolicyDecision{
					Action:  ActionDeny,
					Message: message,
				},
				Definition: definition,
				Binding:    binding,
			})
		default:
			deniedDecisions = append(deniedDecisions, policyDecisionWithMetadata{
				PolicyDecision: PolicyDecision{
					Action:  ActionDeny,
					Message: fmt.Errorf("unrecognized failure policy: '%v'", policy).Error(),
				},
				Definition: definition,
				Binding:    binding,
			})
		}
	}

	authz := admissionauthorizer.NewCachingAuthorizer(c.authz)

	for _, hook := range hooks {
		// versionedAttributes will be set to non-nil inside of the loop, but
		// is scoped outside of the param loop so we only convert once. We defer
		// conversion so that it is only performed when we know a policy matches,
		// saving the cost of converting non-matching requests.
		var versionedAttr *admission.VersionedAttributes

		definition := hook.Policy
		matches, matchResource, matchKind, err := c.matcher.DefinitionMatches(a, o, NewValidatingAdmissionPolicyAccessor(definition))
		if err != nil {
			// Configuration error.
			addConfigError(err, definition, nil)
			continue
		}
		if !matches {
			// Policy definition does not match request
			continue
		} else if hook.ConfigurationError != nil {
			// Configuration error.
			addConfigError(hook.ConfigurationError, definition, nil)
			continue
		}

		auditAnnotationCollector := newAuditAnnotationCollector()
		for _, binding := range hook.Bindings {
			// If the key is inside dependentBindings, there is guaranteed to
			// be a bindingInfo for it
			matches, err := c.matcher.BindingMatches(a, o, NewValidatingAdmissionPolicyBindingAccessor(binding))
			if err != nil {
				// Configuration error.
				addConfigError(err, definition, binding)
				continue
			}
			if !matches {
				continue
			}

			params, err := generic.CollectParams(
				hook.Policy.Spec.ParamKind,
				hook.ParamInformer,
				hook.ParamScope,
				binding.Spec.ParamRef,
				a.GetNamespace(),
			)

			if err != nil {
				addConfigError(err, definition, binding)
				continue
			} else if versionedAttr == nil && len(params) > 0 {
				// As optimization versionedAttr creation is deferred until
				// first use. Since > 0 params, we will validate
				va, err := admission.NewVersionedAttributes(a, matchKind, o)
				if err != nil {
					wrappedErr := fmt.Errorf("failed to convert object version: %w", err)
					addConfigError(wrappedErr, definition, binding)
					continue
				}
				versionedAttr = va
			}

			var validationResults []validateResultWithParam
			var namespace *v1.Namespace
			namespaceName := a.GetNamespace()

			// Special case, the namespace object has the namespace of itself (maybe a bug).
			// unset it if the incoming object is a namespace
			if gvk := a.GetKind(); gvk.Kind == "Namespace" && gvk.Version == "v1" && gvk.Group == "" {
				namespaceName = ""
			}

			// if it is cluster scoped, namespaceName will be empty
			// Otherwise, get the Namespace resource.
			if namespaceName != "" {
				namespace, err = c.matcher.GetNamespace(namespaceName)
				if err != nil {
					return err
				}
			}

			for _, param := range params {
				var p runtime.Object = param
				paramNamespace := ""
				paramName := ""
				if p != nil {
					accessor, err := meta.Accessor(p)
					if err != nil {
						return err
					}
					paramNamespace = accessor.GetNamespace()
					paramName = accessor.GetName()

					if p.GetObjectKind().GroupVersionKind().Empty() {
						// Make sure param has TypeMeta populated
						// This is a simple hack to make sure typeMeta is
						// available to CEL without making copies of objects, etc.
						p = &wrappedParam{
							TypeMeta: metav1.TypeMeta{
								APIVersion: definition.Spec.ParamKind.APIVersion,
								Kind:       definition.Spec.ParamKind.Kind,
							},
							nested: param,
						}
					}
				}

				validationResult := hook.Evaluator.Validate(
					ctx,
					matchResource,
					versionedAttr,
					p,
					namespace,
					celconfig.RuntimeCELCostBudget,
					authz,
				)

				validationResults = append(validationResults, validateResultWithParam{
					Result:         validationResult,
					ParamNamespace: paramNamespace,
					ParamName:      paramName,
				})
			}

			for _, result := range validationResults {
				validationResult := result.Result
				for i, decision := range validationResult.Decisions {
					switch decision.Action {
					case ActionAdmit:
						celmetrics.Metrics.ObserveAdmission(ctx, decision.Elapsed, definition.Name, binding.Name, ErrorType(&decision),
							"validation", result.ParamNamespace, result.ParamName, i)
					case ActionDeny:
						for _, action := range binding.Spec.ValidationActions {
							switch action {
							case admissionregistrationv1.Deny:
								deniedDecisions = append(deniedDecisions, policyDecisionWithMetadata{
									Definition:     definition,
									Binding:        binding,
									PolicyDecision: decision,
								})
								celmetrics.Metrics.ObserveRejection(ctx, decision.Elapsed, definition.Name, binding.Name, ErrorType(&decision),
									"validation", result.ParamNamespace, result.ParamName, i)
							case admissionregistrationv1.Audit:
								publishValidationFailureAnnotation(binding, i, decision, versionedAttr)
								celmetrics.Metrics.ObserveAudit(ctx, decision.Elapsed, definition.Name, binding.Name, ErrorType(&decision),
									"validation", result.ParamNamespace, result.ParamName, i)
							case admissionregistrationv1.Warn:
								warning.AddWarning(ctx, "", fmt.Sprintf("Validation failed for ValidatingAdmissionPolicy '%s' with binding '%s': %s", definition.Name, binding.Name, decision.Message))
								celmetrics.Metrics.ObserveWarn(ctx, decision.Elapsed, definition.Name, binding.Name, ErrorType(&decision),
									"validation", result.ParamNamespace, result.ParamName, i)
							}
						}
					default:
						return fmt.Errorf("unrecognized evaluation decision '%s' for ValidatingAdmissionPolicyBinding '%s' with ValidatingAdmissionPolicy '%s'",
							decision.Action, binding.Name, definition.Name)
					}
				}

				for i, auditAnnotation := range validationResult.AuditAnnotations {
					switch auditAnnotation.Action {
					case AuditAnnotationActionPublish:
						value := auditAnnotation.Value
						if len(auditAnnotation.Value) > maxAuditAnnotationValueLength {
							value = value[:maxAuditAnnotationValueLength]
						}
						auditAnnotationCollector.add(auditAnnotation.Key, value)
					case AuditAnnotationActionError:
						// When failurePolicy=fail, audit annotation errors result in deny
						d := policyDecisionWithMetadata{
							Definition: definition,
							Binding:    binding,
							PolicyDecision: PolicyDecision{
								Action:     ActionDeny,
								Evaluation: EvalError,
								Message:    auditAnnotation.Error,
								Elapsed:    auditAnnotation.Elapsed,
							},
						}
						deniedDecisions = append(deniedDecisions, d)
						celmetrics.Metrics.ObserveRejection(ctx, auditAnnotation.Elapsed, definition.Name, binding.Name, ErrorType(&d.PolicyDecision),
							"auditAnnotation", result.ParamNamespace, result.ParamName, i)
					case AuditAnnotationActionExclude: // skip it
					default:
						return fmt.Errorf("unsupported AuditAnnotation Action: %s", auditAnnotation.Action)
					}
				}
			}
		}
		auditAnnotationCollector.publish(definition.Name, a)
	}

	if len(deniedDecisions) > 0 {
		// TODO: refactor admission.NewForbidden so the name extraction is reusable but the code/reason is customizable
		var message string
		deniedDecision := deniedDecisions[0]
		if deniedDecision.Binding != nil {
			message = fmt.Sprintf("ValidatingAdmissionPolicy '%s' with binding '%s' denied request: %s", deniedDecision.Definition.Name, deniedDecision.Binding.Name, deniedDecision.Message)
		} else {
			message = fmt.Sprintf("ValidatingAdmissionPolicy '%s' denied request: %s", deniedDecision.Definition.Name, deniedDecision.Message)
		}
		err := admission.NewForbidden(a, errors.New(message)).(*k8serrors.StatusError)
		reason := deniedDecision.Reason
		if len(reason) == 0 {
			reason = metav1.StatusReasonInvalid
		}
		err.ErrStatus.Reason = reason
		err.ErrStatus.Code = reasonToCode(reason)
		err.ErrStatus.Details.Causes = append(err.ErrStatus.Details.Causes, metav1.StatusCause{Message: message})
		return err
	}
	return nil
}

func publishValidationFailureAnnotation(binding *admissionregistrationv1.ValidatingAdmissionPolicyBinding, expressionIndex int, decision PolicyDecision, attributes admission.Attributes) {
	key := "validation.policy.admission.k8s.io/validation_failure"
	// Marshal to a list of failures since, in the future, we may need to support multiple failures
	valueJSON, err := utiljson.Marshal([]ValidationFailureValue{{
		ExpressionIndex:   expressionIndex,
		Message:           decision.Message,
		ValidationActions: binding.Spec.ValidationActions,
		Binding:           binding.Name,
		Policy:            binding.Spec.PolicyName,
	}})
	if err != nil {
		klog.Warningf("Failed to set admission audit annotation %s for ValidatingAdmissionPolicy %s and ValidatingAdmissionPolicyBinding %s: %v", key, binding.Spec.PolicyName, binding.Name, err)
	}
	value := string(valueJSON)
	if err := attributes.AddAnnotation(key, value); err != nil {
		klog.Warningf("Failed to set admission audit annotation %s to %s for ValidatingAdmissionPolicy %s and ValidatingAdmissionPolicyBinding %s: %v", key, value, binding.Spec.PolicyName, binding.Name, err)
	}
}

const maxAuditAnnotationValueLength = 10 * 1024

// validationFailureValue defines the JSON format of a "validation.policy.admission.k8s.io/validation_failure" audit
// annotation value.
type ValidationFailureValue struct {
	Message           string                                     `json:"message"`
	Policy            string                                     `json:"policy"`
	Binding           string                                     `json:"binding"`
	ExpressionIndex   int                                        `json:"expressionIndex"`
	ValidationActions []admissionregistrationv1.ValidationAction `json:"validationActions"`
}

type auditAnnotationCollector struct {
	annotations map[string][]string
}

func newAuditAnnotationCollector() auditAnnotationCollector {
	return auditAnnotationCollector{annotations: map[string][]string{}}
}

func (a auditAnnotationCollector) add(key, value string) {
	// If multiple bindings produces the exact same key and value for an audit annotation,
	// ignore the duplicates.
	for _, v := range a.annotations[key] {
		if v == value {
			return
		}
	}
	a.annotations[key] = append(a.annotations[key], value)
}

func (a auditAnnotationCollector) publish(policyName string, attributes admission.Attributes) {
	for key, bindingAnnotations := range a.annotations {
		var value string
		if len(bindingAnnotations) == 1 {
			value = bindingAnnotations[0]
		} else {
			// Multiple distinct values can exist when binding params are used in the valueExpression of an auditAnnotation.
			// When this happens, the values are concatenated into a comma-separated list.
			value = strings.Join(bindingAnnotations, ", ")
		}
		if err := attributes.AddAnnotation(policyName+"/"+key, value); err != nil {
			klog.Warningf("Failed to set admission audit annotation %s to %s for ValidatingAdmissionPolicy %s: %v", key, value, policyName, err)
		}
	}
}

// A workaround to fact that native types do not have TypeMeta populated, which
// is needed for CEL expressions to be able to access the value.
type wrappedParam struct {
	metav1.TypeMeta
	nested runtime.Object
}

func (w *wrappedParam) MarshalJSON() ([]byte, error) {
	return nil, errors.New("MarshalJSON unimplemented for wrappedParam")
}

func (w *wrappedParam) UnmarshalJSON(data []byte) error {
	return errors.New("UnmarshalJSON unimplemented for wrappedParam")
}

func (w *wrappedParam) ToUnstructured() interface{} {
	res, err := runtime.DefaultUnstructuredConverter.ToUnstructured(w.nested)

	if err != nil {
		return nil
	}

	metaRes, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&w.TypeMeta)
	if err != nil {
		return nil
	}

	for k, v := range metaRes {
		res[k] = v
	}

	return res
}

func (w *wrappedParam) DeepCopyObject() runtime.Object {
	return &wrappedParam{
		TypeMeta: w.TypeMeta,
		nested:   w.nested.DeepCopyObject(),
	}
}

func (w *wrappedParam) GetObjectKind() schema.ObjectKind {
	return w
}
