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

	"github.com/blang/semver/v4"
	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
)

var (
	SemverType = cel.ObjectType("kubernetes.Semver")
)

// Semver provides a CEL representation of a [semver.Version].
type Semver struct {
	semver.Version
}

func (v Semver) ConvertToNative(typeDesc reflect.Type) (interface{}, error) {
	if reflect.TypeOf(v.Version).AssignableTo(typeDesc) {
		return v.Version, nil
	}
	if reflect.TypeOf("").AssignableTo(typeDesc) {
		return v.Version.String(), nil
	}
	return nil, fmt.Errorf("type conversion error from 'Semver' to '%v'", typeDesc)
}

func (v Semver) ConvertToType(typeVal ref.Type) ref.Val {
	switch typeVal {
	case SemverType:
		return v
	case types.TypeType:
		return SemverType
	default:
		return types.NewErr("type conversion error from '%s' to '%s'", SemverType, typeVal)
	}
}

func (v Semver) Equal(other ref.Val) ref.Val {
	otherDur, ok := other.(Semver)
	if !ok {
		return types.MaybeNoSuchOverloadErr(other)
	}
	return types.Bool(v.Version.EQ(otherDur.Version))
}

func (v Semver) Type() ref.Type {
	return SemverType
}

func (v Semver) Value() interface{} {
	return v.Version
}
