/*
Copyright 2017 The Kubernetes Authors.

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

package policy

import (
	"fmt"
	"os"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	auditinternal "k8s.io/apiserver/pkg/apis/audit"
	auditv1 "k8s.io/apiserver/pkg/apis/audit/v1"
	"k8s.io/apiserver/pkg/apis/audit/validation"
	"k8s.io/apiserver/pkg/audit"
	"k8s.io/klog/v2"
)

var (
	apiGroupVersions = []schema.GroupVersion{
		auditv1.SchemeGroupVersion,
	}
	apiGroupVersionSet = map[schema.GroupVersion]bool{}
)

func init() {
	for _, gv := range apiGroupVersions {
		apiGroupVersionSet[gv] = true
	}
}

func LoadPolicyFromFile(filePath string) (*auditinternal.Policy, error) {
	if filePath == "" {
		return nil, fmt.Errorf("file path not specified")
	}
	policyDef, err := os.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to read file path %q: %+v", filePath, err)
	}

	ret, err := LoadPolicyFromBytes(policyDef)
	if err != nil {
		return nil, fmt.Errorf("%v: from file %v", err.Error(), filePath)
	}

	return ret, nil
}

func LoadPolicyFromBytes(policyDef []byte) (*auditinternal.Policy, error) {
	policy := &auditinternal.Policy{}
	strictDecoder := serializer.NewCodecFactory(audit.Scheme, serializer.EnableStrict).UniversalDecoder()

	// Try strict decoding first.
	_, gvk, err := strictDecoder.Decode(policyDef, nil, policy)
	if err != nil {
		if !runtime.IsStrictDecodingError(err) {
			return nil, fmt.Errorf("failed decoding: %w", err)
		}
		var (
			lenientDecoder = audit.Codecs.UniversalDecoder(apiGroupVersions...)
			lenientErr     error
		)
		_, gvk, lenientErr = lenientDecoder.Decode(policyDef, nil, policy)
		if lenientErr != nil {
			return nil, fmt.Errorf("failed lenient decoding: %w", lenientErr)
		}
		klog.Warningf("Audit policy contains errors, falling back to lenient decoding: %v", err)
	}

	// Ensure the policy file contained an apiVersion and kind.
	gv := schema.GroupVersion{Group: gvk.Group, Version: gvk.Version}
	if !apiGroupVersionSet[gv] {
		return nil, fmt.Errorf("unknown group version field %v in policy", gvk)
	}

	if err := validation.ValidatePolicy(policy); err != nil {
		return nil, err.ToAggregate()
	}

	policyCnt := len(policy.Rules)
	if policyCnt == 0 {
		return nil, fmt.Errorf("loaded illegal policy with 0 rules")
	}

	klog.V(4).InfoS("Load audit policy rules success", "policyCnt", policyCnt)
	return policy, nil
}
