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

package cel

import (
	"fmt"
	"reflect"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/checker/decls"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
	"k8s.io/apimachinery/pkg/api/resource"
)

var (
	QuantityObject    = decls.NewObjectType("kubernetes.Quantity")
	quantityTypeValue = types.NewTypeValue("kubernetes.Quantity")
	QuantityType      = cel.ObjectType("kubernetes.Quantity")
)

// Quantity provides a CEL representation of a resource.Quantity
type Quantity struct {
	*resource.Quantity
}

func (d Quantity) ConvertToNative(typeDesc reflect.Type) (interface{}, error) {
	if reflect.TypeOf(d.Quantity).AssignableTo(typeDesc) {
		return d.Quantity, nil
	}
	if reflect.TypeOf("").AssignableTo(typeDesc) {
		return d.Quantity.String(), nil
	}
	return nil, fmt.Errorf("type conversion error from 'Quantity' to '%v'", typeDesc)
}

func (d Quantity) ConvertToType(typeVal ref.Type) ref.Val {
	switch typeVal {
	case quantityTypeValue:
		return d
	case types.TypeType:
		return quantityTypeValue
	default:
		return types.NewErr("type conversion error from '%s' to '%s'", quantityTypeValue, typeVal)
	}
}

func (d Quantity) Equal(other ref.Val) ref.Val {
	otherDur, ok := other.(Quantity)
	if !ok {
		return types.MaybeNoSuchOverloadErr(other)
	}
	return types.Bool(d.Quantity.Equal(*otherDur.Quantity))
}

func (d Quantity) Type() ref.Type {
	return quantityTypeValue
}

func (d Quantity) Value() interface{} {
	return d.Quantity
}
