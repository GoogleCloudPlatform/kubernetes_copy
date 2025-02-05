/*
Copyright 2023 The Kubernetes Authors.

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

package matchconditions

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/cel-go/cel"
	celtypes "github.com/google/cel-go/common/types"

	v1 "k8s.io/api/admissionregistration/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apiserver/pkg/admission"
	admissionmetrics "k8s.io/apiserver/pkg/admission/metrics"
	celplugin "k8s.io/apiserver/pkg/admission/plugin/cel"
	celconfig "k8s.io/apiserver/pkg/apis/cel"
	"k8s.io/apiserver/pkg/authorization/authorizer"
	"k8s.io/klog/v2"
)

var _ celplugin.ExpressionAccessor = &MatchCondition{}

// MatchCondition contains the inputs needed to compile, evaluate and match a cel expression
type MatchCondition v1.MatchCondition

func (v *MatchCondition) GetExpression() string {
	return v.Expression
}

func (v *MatchCondition) ReturnTypes() []*cel.Type {
	return []*cel.Type{cel.BoolType}
}

var _ Matcher = &matcher{}

// matcher evaluates compiled cel expressions and determines if they match the given request or not
type matcher struct {
	filter      celplugin.ConditionEvaluator
	failPolicy  v1.FailurePolicyType
	matcherType string
	matcherKind string
	objectName  string
}

func NewMatcher(filter celplugin.ConditionEvaluator, failPolicy *v1.FailurePolicyType, matcherKind, matcherType, objectName string) Matcher {
	var f v1.FailurePolicyType
	if failPolicy == nil {
		f = v1.Fail
	} else {
		f = *failPolicy
	}
	return &matcher{
		filter:      filter,
		failPolicy:  f,
		matcherKind: matcherKind,
		matcherType: matcherType,
		objectName:  objectName,
	}
}

func (m *matcher) Match(ctx context.Context, versionedAttr *admission.VersionedAttributes, versionedParams runtime.Object, authz authorizer.Authorizer) MatchResult {
	t := time.Now()
	evalResults, _, err := m.filter.ForInput(ctx, versionedAttr, celplugin.CreateAdmissionRequest(versionedAttr.Attributes, metav1.GroupVersionResource(versionedAttr.GetResource()), metav1.GroupVersionKind(versionedAttr.VersionedKind)), celplugin.OptionalVariableBindings{
		VersionedParams: versionedParams,
		Authorizer:      authz,
	}, nil, celconfig.RuntimeCELCostBudgetMatchConditions)

	if err != nil {
		admissionmetrics.Metrics.ObserveMatchConditionEvaluationTime(ctx, time.Since(t), m.objectName, m.matcherKind, m.matcherType, string(versionedAttr.GetOperation()))
		// filter returning error is unexpected and not an evaluation error so not incrementing metric here
		if m.failPolicy == v1.Fail {
			return MatchResult{
				Error: err,
			}
		} else if m.failPolicy == v1.Ignore {
			return MatchResult{
				Matches: false,
			}
		}
		//TODO: add default so that if in future we add different failure types it doesn't fall through
	}

	errorList := []error{}
	for _, evalResult := range evalResults {
		matchCondition, ok := evalResult.ExpressionAccessor.(*MatchCondition)
		if !ok {
			// This shouldn't happen, but if it does treat same as eval error
			klog.Error("Invalid type conversion to MatchCondition")
			errorList = append(errorList, errors.New(fmt.Sprintf("internal error converting ExpressionAccessor to MatchCondition")))
			continue
		}
		if evalResult.Error != nil {
			errorList = append(errorList, evalResult.Error)
			admissionmetrics.Metrics.ObserveMatchConditionEvalError(ctx, m.objectName, m.matcherKind, m.matcherType, string(versionedAttr.GetOperation()))
		}
		if evalResult.EvalResult == celtypes.False {
			admissionmetrics.Metrics.ObserveMatchConditionEvaluationTime(ctx, time.Since(t), m.objectName, m.matcherKind, m.matcherType, string(versionedAttr.GetOperation()))
			// If any condition false, skip calling webhook always
			return MatchResult{
				Matches:             false,
				FailedConditionName: matchCondition.Name,
			}
		}
	}
	if len(errorList) > 0 {
		admissionmetrics.Metrics.ObserveMatchConditionEvaluationTime(ctx, time.Since(t), m.objectName, m.matcherKind, m.matcherType, string(versionedAttr.GetOperation()))
		// If mix of true and eval errors then resort to fail policy
		if m.failPolicy == v1.Fail {
			// mix of true and errors with fail policy fail should fail request without calling webhook
			err = utilerrors.NewAggregate(errorList)
			return MatchResult{
				Error: err,
			}
		} else if m.failPolicy == v1.Ignore {
			// if fail policy ignore then skip call to webhook
			return MatchResult{
				Matches: false,
			}
		}
	}
	// if no results eval to false, return matches true with list of any errors encountered
	return MatchResult{
		Matches: true,
	}
}
