/*
Copyright 2019 The Kubernetes Authors.

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

package testing

import (
	"context"
	"reflect"
	"testing"

	"github.com/google/go-cmp/cmp" //nolint:depguard
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apiserver/pkg/admission"
)

// WithReinvocationTesting wraps a mutating admission handler and reinvokes it each time Admit is
// called. It checks the admission output object and reports a test error if the admission handler
// performs non-idempotent mutations to the object.
func WithReinvocationTesting(t *testing.T, admission admission.MutationInterface) admission.MutationInterface {
	return &reinvoker{t, admission}
}

type reinvoker struct {
	t         *testing.T
	admission admission.MutationInterface
}

// Admit reinvokes the admission handler and reports a test error if the admission handler performs
// non-idempotent mutations to the admission object.
func (r *reinvoker) Admit(ctx context.Context, a admission.Attributes, o admission.ObjectInterfaces) error {
	r.t.Helper()
	outputs := []runtime.Object{}
	for i := 0; i < 2; i++ {
		err := r.admission.Admit(ctx, a, o)
		if err != nil {
			return err
		}
		if a.GetObject() != nil {
			// keep a copy of the output for subsequent idempotency checking
			outputs = append(outputs, a.GetObject().DeepCopyObject())
			// replace a.GetObject() with a copy of itself to make sure admission is safe to reinvoke with a round-tripped copy (no pointer comparisons are done)
			if deepCopyInto, ok := reflect.TypeOf(a.GetObject()).MethodByName("DeepCopyInto"); ok {
				deepCopyInto.Func.Call([]reflect.Value{
					reflect.ValueOf(a.GetObject().DeepCopyObject()),
					reflect.ValueOf(a.GetObject()),
				})
			}
		}
	}
	for i := 1; i < len(outputs); i++ {
		if !apiequality.Semantic.DeepEqual(outputs[0], outputs[i]) {
			r.t.Errorf("expected mutating admission plugin to be idempontent, but got different results on reinvocation, diff:\n%s", cmp.Diff(outputs[0], outputs[i]))
		}
	}
	return nil
}

// Handles will return true if any of the admission andler handlers handle the given operation.
func (r *reinvoker) Handles(operation admission.Operation) bool {
	r.t.Helper()
	return r.admission.Handles(operation)
}
