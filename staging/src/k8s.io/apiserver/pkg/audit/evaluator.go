/*
Copyright 2021 The Kubernetes Authors.

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

package audit

import (
	"k8s.io/apiserver/pkg/apis/audit"
	"k8s.io/apiserver/pkg/authorization/authorizer"
)

// RequestAuditConfig is the evaluated audit configuration that is applicable to
// a given request. PolicyRuleEvaluator evaluates the audit policy against the
// authorizer attributes and returns a RequestAuditConfig that applies to the request.
type RequestAuditConfig struct {
	// Level at which the request is being audited at
	Level audit.Level

	// OmitStages is the stages that need to be omitted from being audited.
	OmitStages []audit.Stage

	// OmitManagedFields indicates whether to omit the managed fields of the request
	// and response bodies from being written to the API audit log.
	OmitManagedFields bool
}

// PolicyRuleEvaluator exposes methods for evaluating the policy rules.
type PolicyRuleEvaluator interface {
	// EvaluatePolicyRule evaluates the audit policy of the apiserver against
	// the given authorizer attributes and returns the audit configuration that
	// is applicable to the given request.
	EvaluatePolicyRule(authorizer.Attributes) RequestAuditConfig
}
