/*
Copyright 2014 The Kubernetes Authors.

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

package intstr

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"runtime/debug"
	"strconv"
	"strings"

	cbor "k8s.io/apimachinery/pkg/runtime/serializer/cbor/direct"
	"k8s.io/klog/v2"
)

// IntOrString is a type that can hold an int32 or a string.  When used in
// JSON or YAML marshalling and unmarshalling, it produces or consumes the
// inner type.  This allows you to have, for example, a JSON field that can
// accept a name or number.
// TODO: Rename to Int32OrString
//
// +protobuf=true
// +protobuf.options.(gogoproto.goproto_stringer)=false
// +k8s:openapi-gen=true
type IntOrString struct {
	Type   Type   `protobuf:"varint,1,opt,name=type,casttype=Type"`
	IntVal int32  `protobuf:"varint,2,opt,name=intVal"`
	StrVal string `protobuf:"bytes,3,opt,name=strVal"`
}

// Type represents the stored type of IntOrString.
type Type int64

const (
	Int    Type = iota // The IntOrString holds an int.
	String             // The IntOrString holds a string.
)

// FromInt creates an IntOrString object with an int32 value. It is
// your responsibility not to call this method with a value greater
// than int32.
// Deprecated: use FromInt32 instead.
func FromInt(val int) IntOrString {
	if val > math.MaxInt32 || val < math.MinInt32 {
		//nolint:logcheck // FromInt is deprecated and only used in tests in-tree.
		klog.Errorf("value: %d overflows int32\n%s\n", val, debug.Stack())
	}
	return IntOrString{Type: Int, IntVal: int32(val)}
}

// FromInt32 creates an IntOrString object with an int32 value.
func FromInt32(val int32) IntOrString {
	return IntOrString{Type: Int, IntVal: val}
}

// FromString creates an IntOrString object with a string value.
func FromString(val string) IntOrString {
	return IntOrString{Type: String, StrVal: val}
}

// Parse the given string and try to convert it to an int32 integer before
// setting it as a string value.
func Parse(val string) IntOrString {
	i, err := strconv.ParseInt(val, 10, 32)
	if err != nil {
		return FromString(val)
	}
	return FromInt32(int32(i))
}

// UnmarshalJSON implements the json.Unmarshaller interface.
func (intstr *IntOrString) UnmarshalJSON(value []byte) error {
	if value[0] == '"' {
		intstr.Type = String
		return json.Unmarshal(value, &intstr.StrVal)
	}
	intstr.Type = Int
	return json.Unmarshal(value, &intstr.IntVal)
}

func (intstr *IntOrString) UnmarshalCBOR(value []byte) error {
	if err := cbor.Unmarshal(value, &intstr.StrVal); err == nil {
		intstr.Type = String
		return nil
	}

	if err := cbor.Unmarshal(value, &intstr.IntVal); err != nil {
		return err
	}

	intstr.Type = Int
	return nil
}

// String returns the string value, or the Itoa of the int value.
func (intstr *IntOrString) String() string {
	if intstr == nil {
		return "<nil>"
	}
	if intstr.Type == String {
		return intstr.StrVal
	}
	return strconv.Itoa(intstr.IntValue())
}

// IntValue returns the IntVal if type Int, or if
// it is a String, will attempt a conversion to int,
// returning 0 if a parsing error occurs.
func (intstr *IntOrString) IntValue() int {
	if intstr.Type == String {
		i, _ := strconv.Atoi(intstr.StrVal)
		return i
	}
	return int(intstr.IntVal)
}

// MarshalJSON implements the json.Marshaller interface.
func (intstr IntOrString) MarshalJSON() ([]byte, error) {
	switch intstr.Type {
	case Int:
		return json.Marshal(intstr.IntVal)
	case String:
		return json.Marshal(intstr.StrVal)
	default:
		return []byte{}, fmt.Errorf("impossible IntOrString.Type")
	}
}

func (intstr IntOrString) MarshalCBOR() ([]byte, error) {
	switch intstr.Type {
	case Int:
		return cbor.Marshal(intstr.IntVal)
	case String:
		return cbor.Marshal(intstr.StrVal)
	default:
		return nil, fmt.Errorf("impossible IntOrString.Type")
	}
}

// OpenAPISchemaType is used by the kube-openapi generator when constructing
// the OpenAPI spec of this type.
//
// See: https://github.com/kubernetes/kube-openapi/tree/master/pkg/generators
func (IntOrString) OpenAPISchemaType() []string { return []string{"string"} }

// OpenAPISchemaFormat is used by the kube-openapi generator when constructing
// the OpenAPI spec of this type.
func (IntOrString) OpenAPISchemaFormat() string { return "int-or-string" }

// OpenAPIV3OneOfTypes is used by the kube-openapi generator when constructing
// the OpenAPI v3 spec of this type.
func (IntOrString) OpenAPIV3OneOfTypes() []string { return []string{"integer", "string"} }

func ValueOrDefault(intOrPercent *IntOrString, defaultValue IntOrString) *IntOrString {
	if intOrPercent == nil {
		return &defaultValue
	}
	return intOrPercent
}

// GetScaledValueFromIntOrPercent is meant to replace GetValueFromIntOrPercent.
// This method returns a scaled value from an IntOrString type. If the IntOrString
// is a percentage string value it's treated as a percentage and scaled appropriately
// in accordance to the total, if it's an int value it's treated as a simple value and
// if it is a string value which is either non-numeric or numeric but lacking a trailing '%' it returns an error.
func GetScaledValueFromIntOrPercent(intOrPercent *IntOrString, total int, roundUp bool) (int, error) {
	if intOrPercent == nil {
		return 0, errors.New("nil value for IntOrString")
	}
	value, isPercent, err := getIntOrPercentValueSafely(intOrPercent)
	if err != nil {
		return 0, fmt.Errorf("invalid value for IntOrString: %v", err)
	}
	if isPercent {
		if roundUp {
			value = int(math.Ceil(float64(value) * (float64(total)) / 100))
		} else {
			value = int(math.Floor(float64(value) * (float64(total)) / 100))
		}
	}
	return value, nil
}

// GetValueFromIntOrPercent was deprecated in favor of
// GetScaledValueFromIntOrPercent. This method was treating all int as a numeric value and all
// strings with or without a percent symbol as a percentage value.
// Deprecated
func GetValueFromIntOrPercent(intOrPercent *IntOrString, total int, roundUp bool) (int, error) {
	if intOrPercent == nil {
		return 0, errors.New("nil value for IntOrString")
	}
	value, isPercent, err := getIntOrPercentValue(intOrPercent)
	if err != nil {
		return 0, fmt.Errorf("invalid value for IntOrString: %v", err)
	}
	if isPercent {
		if roundUp {
			value = int(math.Ceil(float64(value) * (float64(total)) / 100))
		} else {
			value = int(math.Floor(float64(value) * (float64(total)) / 100))
		}
	}
	return value, nil
}

// getIntOrPercentValue is a legacy function and only meant to be called by GetValueFromIntOrPercent
// For a more correct implementation call getIntOrPercentSafely
func getIntOrPercentValue(intOrStr *IntOrString) (int, bool, error) {
	switch intOrStr.Type {
	case Int:
		return intOrStr.IntValue(), false, nil
	case String:
		s := strings.Replace(intOrStr.StrVal, "%", "", -1)
		v, err := strconv.Atoi(s)
		if err != nil {
			return 0, false, fmt.Errorf("invalid value %q: %v", intOrStr.StrVal, err)
		}
		return int(v), true, nil
	}
	return 0, false, fmt.Errorf("invalid type: neither int nor percentage")
}

func getIntOrPercentValueSafely(intOrStr *IntOrString) (int, bool, error) {
	switch intOrStr.Type {
	case Int:
		return intOrStr.IntValue(), false, nil
	case String:
		isPercent := false
		s := intOrStr.StrVal
		if strings.HasSuffix(s, "%") {
			isPercent = true
			s = strings.TrimSuffix(intOrStr.StrVal, "%")
		} else {
			return 0, false, fmt.Errorf("invalid type: string is not a percentage")
		}
		v, err := strconv.Atoi(s)
		if err != nil {
			return 0, false, fmt.Errorf("invalid value %q: %v", intOrStr.StrVal, err)
		}
		return int(v), isPercent, nil
	}
	return 0, false, fmt.Errorf("invalid type: neither int nor percentage")
}
