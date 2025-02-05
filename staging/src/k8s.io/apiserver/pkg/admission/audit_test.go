/*
Copyright 2018 The Kubernetes Authors.

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

package admission

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"k8s.io/apimachinery/pkg/runtime/schema"
	auditinternal "k8s.io/apiserver/pkg/apis/audit"
	"k8s.io/apiserver/pkg/audit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeHandler implements Interface
type fakeHandler struct {
	// return value of Admit()
	admit error
	// annotations add to attributesRecord during Admit() phase
	admitAnnotations map[string]string
	// return value of Validate()
	validate error
	// annotations add to attributesRecord during Validate() phase
	validateAnnotations map[string]string
	// return value of Handles()
	handles bool
}

var _ Interface = &fakeHandler{}
var _ MutationInterface = &fakeHandler{}
var _ ValidationInterface = &fakeHandler{}

func (h fakeHandler) Admit(ctx context.Context, a Attributes, o ObjectInterfaces) error {
	for k, v := range h.admitAnnotations {
		a.AddAnnotation(k, v)
	}
	return h.admit
}

func (h fakeHandler) Validate(ctx context.Context, a Attributes, o ObjectInterfaces) error {
	for k, v := range h.validateAnnotations {
		a.AddAnnotation(k, v)
	}
	return h.validate
}

func (h fakeHandler) Handles(o Operation) bool {
	return h.handles
}

func attributes() Attributes {
	return NewAttributesRecord(nil, nil, schema.GroupVersionKind{}, "", "", schema.GroupVersionResource{}, "", "", nil, false, nil)
}

func TestWithAudit(t *testing.T) {
	var testCases = map[string]struct {
		admit               error
		admitAnnotations    map[string]string
		validate            error
		validateAnnotations map[string]string
		handles             bool
	}{
		"not handle": {
			nil,
			nil,
			nil,
			nil,
			false,
		},
		"allow": {
			nil,
			nil,
			nil,
			nil,
			true,
		},
		"allow with annotations": {
			nil,
			map[string]string{
				"plugin.example.com/foo": "bar",
			},
			nil,
			nil,
			true,
		},
		"allow with annotations overwrite": {
			nil,
			map[string]string{
				"plugin.example.com/foo": "bar",
			},
			nil,
			map[string]string{
				"plugin.example.com/foo": "bar",
			},
			true,
		},
		"forbidden error": {
			NewForbidden(attributes(), fmt.Errorf("quota exceeded")),
			nil,
			NewForbidden(attributes(), fmt.Errorf("quota exceeded")),
			nil,
			true,
		},
		"forbidden error with annotations": {
			NewForbidden(attributes(), fmt.Errorf("quota exceeded")),
			nil,
			NewForbidden(attributes(), fmt.Errorf("quota exceeded")),
			map[string]string{
				"plugin.example.com/foo": "bar",
			},
			true,
		},
		"forbidden error with annotations overwrite": {
			NewForbidden(attributes(), fmt.Errorf("quota exceeded")),
			map[string]string{
				"plugin.example.com/foo": "bar",
			},
			NewForbidden(attributes(), fmt.Errorf("quota exceeded")),
			map[string]string{
				"plugin.example.com/foo": "bar",
			},
			true,
		},
	}
	for tcName, tc := range testCases {
		var handler Interface = fakeHandler{tc.admit, tc.admitAnnotations, tc.validate, tc.validateAnnotations, tc.handles}
		ctx := audit.WithAuditContext(context.Background())
		ac := audit.AuditContextFrom(ctx)
		ac.SetEventLevel(auditinternal.LevelMetadata)
		auditHandler := WithAudit(handler)
		a := attributes()

		assert.Equal(t, handler.Handles(Create), auditHandler.Handles(Create), tcName+": WithAudit decorator should not effect the return value")

		mutator, ok := handler.(MutationInterface)
		require.True(t, ok)
		auditMutator, ok := auditHandler.(MutationInterface)
		require.True(t, ok)
		assert.Equal(t, mutator.Admit(ctx, a, nil), auditMutator.Admit(ctx, a, nil), tcName+": WithAudit decorator should not effect the return value")

		validator, ok := handler.(ValidationInterface)
		require.True(t, ok)
		auditValidator, ok := auditHandler.(ValidationInterface)
		require.True(t, ok)
		assert.Equal(t, validator.Validate(ctx, a, nil), auditValidator.Validate(ctx, a, nil), tcName+": WithAudit decorator should not effect the return value")

		annotations := make(map[string]string, len(tc.admitAnnotations)+len(tc.validateAnnotations))
		for k, v := range tc.admitAnnotations {
			annotations[k] = v
		}
		for k, v := range tc.validateAnnotations {
			annotations[k] = v
		}
		if len(annotations) == 0 {
			assert.Nil(t, ac.GetEventAnnotations(), tcName+": unexptected annotations set in audit event")
		} else {
			assert.Equal(t, annotations, ac.GetEventAnnotations(), tcName+": unexptected annotations set in audit event")
		}
	}
}

func TestWithAuditConcurrency(t *testing.T) {
	admitAnnotations := map[string]string{
		"plugin.example.com/foo": "foo",
		"plugin.example.com/bar": "bar",
		"plugin.example.com/baz": "baz",
		"plugin.example.com/qux": "qux",
	}
	var handler Interface = fakeHandler{admitAnnotations: admitAnnotations, handles: true}
	ctx := audit.WithAuditContext(context.Background())
	ac := audit.AuditContextFrom(ctx)
	ac.SetEventLevel(auditinternal.LevelMetadata)
	auditHandler := WithAudit(handler)
	a := attributes()

	// Simulate the scenario store.DeleteCollection
	workers := 2
	wg := &sync.WaitGroup{}
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			mutator, ok := handler.(MutationInterface)
			require.True(t, ok)
			auditMutator, ok := auditHandler.(MutationInterface)
			require.True(t, ok)
			assert.Equal(t, mutator.Admit(ctx, a, nil), auditMutator.Admit(ctx, a, nil), "WithAudit decorator should not effect the return value")
		}()
	}
	wg.Wait()
}
