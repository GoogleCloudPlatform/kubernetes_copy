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

package patch

import (
	"context"
	gojson "encoding/json"
	"errors"
	"fmt"
	celgo "github.com/google/cel-go/cel"
	"reflect"
	"strconv"

	jsonpatch "github.com/evanphx/json-patch/v5"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/traits"
	"google.golang.org/protobuf/types/known/structpb"

	admissionv1 "k8s.io/api/admission/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer/json"
	plugincel "k8s.io/apiserver/pkg/admission/plugin/cel"
	"k8s.io/apiserver/pkg/cel/mutation"
	"k8s.io/apiserver/pkg/cel/mutation/dynamic"
	pointer "k8s.io/utils/ptr"
)

// JSONPatchCondition contains the inputs needed to compile and evaluate a cel expression
// that returns a JSON patch value.
type JSONPatchCondition struct {
	Expression string
}

var _ plugincel.ExpressionAccessor = &JSONPatchCondition{}

func (v *JSONPatchCondition) GetExpression() string {
	return v.Expression
}

func (v *JSONPatchCondition) ReturnTypes() []*celgo.Type {
	return []*celgo.Type{celgo.ListType(jsonPatchType)}
}

var jsonPatchType = types.NewObjectType("JSONPatch")

// NewJSONPatcher creates a patcher that performs a JSON Patch mutation.
func NewJSONPatcher(patchEvaluator plugincel.MutatingEvaluator) Patcher {
	return &jsonPatcher{patchEvaluator}
}

type jsonPatcher struct {
	PatchEvaluator plugincel.MutatingEvaluator
}

func (e *jsonPatcher) Patch(ctx context.Context, r Request, runtimeCELCostBudget int64) (runtime.Object, error) {
	admissionRequest := plugincel.CreateAdmissionRequest(
		r.VersionedAttributes.Attributes,
		metav1.GroupVersionResource(r.MatchedResource),
		metav1.GroupVersionKind(r.VersionedAttributes.VersionedKind))

	compileErrors := e.PatchEvaluator.CompilationErrors()
	if len(compileErrors) > 0 {
		return nil, errors.Join(compileErrors...)
	}
	patchObj, _, err := e.evaluatePatchExpression(ctx, e.PatchEvaluator, runtimeCELCostBudget, r, admissionRequest)
	if err != nil {
		return nil, err
	}
	o := r.ObjectInterfaces
	jsonSerializer := json.NewSerializerWithOptions(json.DefaultMetaFactory, o.GetObjectCreater(), o.GetObjectTyper(), json.SerializerOptions{Pretty: false, Strict: true})
	objJS, err := runtime.Encode(jsonSerializer, r.VersionedAttributes.VersionedObject)
	if err != nil {
		return nil, fmt.Errorf("failed to create JSON patch: %w", err)
	}
	patchedJS, err := patchObj.Apply(objJS)
	if err != nil {
		if errors.Is(err, jsonpatch.ErrTestFailed) {
			// If a json patch fails a test operation, the patch must not be applied
			return r.VersionedAttributes.VersionedObject, nil
		}
		return nil, fmt.Errorf("JSON Patch: %w", err)
	}

	var newVersionedObject runtime.Object
	if _, ok := r.VersionedAttributes.VersionedObject.(*unstructured.Unstructured); ok {
		newVersionedObject = &unstructured.Unstructured{}
	} else {
		newVersionedObject, err = o.GetObjectCreater().New(r.VersionedAttributes.VersionedKind)
		if err != nil {
			return nil, apierrors.NewInternalError(err)
		}
	}

	if newVersionedObject, _, err = jsonSerializer.Decode(patchedJS, nil, newVersionedObject); err != nil {
		return nil, apierrors.NewInternalError(err)
	}

	return newVersionedObject, nil
}

func (e *jsonPatcher) evaluatePatchExpression(ctx context.Context, patchEvaluator plugincel.MutatingEvaluator, remainingBudget int64, r Request, admissionRequest *admissionv1.AdmissionRequest) (jsonpatch.Patch, int64, error) {
	var err error
	var eval plugincel.EvaluationResult
	eval, remainingBudget, err = patchEvaluator.ForInput(ctx, r.VersionedAttributes, admissionRequest, r.OptionalVariables, r.Namespace, remainingBudget)
	if err != nil {
		return nil, -1, err
	}
	if eval.Error != nil {
		return nil, -1, eval.Error
	}
	refVal := eval.EvalResult

	// the return type can be any valid CEL value.
	// Scalars, maps and lists are used to set the value when the path points to a field of that type.
	// ObjectVal is used when the path points to a struct. A map like "{"field1": 1, "fieldX": bool}" is not
	// possible in Kubernetes CEL because maps and lists may not have mixed types.

	iter, ok := refVal.(traits.Lister)
	if !ok {
		// Should never happen since compiler checks return type.
		return nil, -1, fmt.Errorf("type mismatch: JSONPatchType.expression should evaluate to array")
	}
	result := jsonpatch.Patch{}
	for it := iter.Iterator(); it.HasNext() == types.True; {
		v := it.Next()
		patchObj, err := v.ConvertToNative(reflect.TypeOf(&mutation.JSONPatchVal{}))
		if err != nil {
			// Should never happen since return type is checked by compiler.
			return nil, -1, fmt.Errorf("type mismatch: JSONPatchType.expression should evaluate to array of JSONPatch: %w", err)
		}
		op, ok := patchObj.(*mutation.JSONPatchVal)
		if !ok {
			// Should never happen since return type is checked by compiler.
			return nil, -1, fmt.Errorf("type mismatch: JSONPatchType.expression should evaluate to array of JSONPatch, got element of %T", patchObj)
		}

		// Construct a JSON Patch from the evaluated CEL expression
		resultOp := jsonpatch.Operation{}
		resultOp["op"] = pointer.To(gojson.RawMessage(strconv.Quote(op.Op)))
		resultOp["path"] = pointer.To(gojson.RawMessage(strconv.Quote(op.Path)))
		if len(op.From) > 0 {
			resultOp["from"] = pointer.To(gojson.RawMessage(strconv.Quote(op.From)))
		}
		if op.Val != nil {
			if objVal, ok := op.Val.(*dynamic.ObjectVal); ok {
				// TODO: Object initializers are insufficiently type checked.
				// In the interim, we use this sanity check to detect type mismatches
				// between field names and Object initializers. For example,
				// "Object.spec{ selector: Object.spec.wrong{}}" is detected as a mismatch.
				// Before beta, attaching full type information both to Object initializers and
				// the "object" and "oldObject" variables is needed. This will allow CEL to
				// perform comprehensive runtime type checking.
				err := objVal.CheckTypeNamesMatchFieldPathNames()
				if err != nil {
					return nil, -1, fmt.Errorf("type mismatch: %w", err)
				}
			}
			// CEL data literals representing arbitrary JSON values can be serialized to JSON for use in
			// JSON Patch if first converted to pb.Value.
			v, err := op.Val.ConvertToNative(reflect.TypeOf(&structpb.Value{}))
			if err != nil {
				return nil, -1, fmt.Errorf("JSONPath valueExpression evaluated to a type that could not marshal to JSON: %w", err)
			}
			b, err := gojson.Marshal(v)
			if err != nil {
				return nil, -1, fmt.Errorf("JSONPath valueExpression evaluated to a type that could not marshal to JSON: %w", err)
			}
			resultOp["value"] = pointer.To[gojson.RawMessage](b)
		}

		result = append(result, resultOp)
	}

	return result, remainingBudget, nil
}
