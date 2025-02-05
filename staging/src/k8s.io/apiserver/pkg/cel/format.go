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

package cel

import (
	"fmt"
	"reflect"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/checker/decls"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
)

var (
	FormatObject = decls.NewObjectType("kubernetes.NamedFormat")
	FormatType   = cel.ObjectType("kubernetes.NamedFormat")
)

// Format provides a CEL representation of kubernetes format
type Format struct {
	Name         string
	ValidateFunc func(string) []string

	// Size of the regex string or estimated equivalent regex string used
	// for cost estimation
	MaxRegexSize int
}

func (d Format) ConvertToNative(typeDesc reflect.Type) (interface{}, error) {
	return nil, fmt.Errorf("type conversion error from 'Format' to '%v'", typeDesc)
}

func (d Format) ConvertToType(typeVal ref.Type) ref.Val {
	switch typeVal {
	case FormatType:
		return d
	case types.TypeType:
		return FormatType
	default:
		return types.NewErr("type conversion error from '%s' to '%s'", FormatType, typeVal)
	}
}

func (d Format) Equal(other ref.Val) ref.Val {
	otherDur, ok := other.(Format)
	if !ok {
		return types.MaybeNoSuchOverloadErr(other)
	}
	return types.Bool(d.Name == otherDur.Name)
}

func (d Format) Type() ref.Type {
	return FormatType
}

func (d Format) Value() interface{} {
	return d
}
