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

package api

import (
	"errors"
	"slices"
	"unique"

	v1beta1 "k8s.io/api/resource/v1beta1"
	conversion "k8s.io/apimachinery/pkg/conversion"
	"k8s.io/apimachinery/pkg/runtime"
)

var (
	localSchemeBuilder runtime.SchemeBuilder
	AddToScheme        = localSchemeBuilder.AddToScheme
)

func Convert_api_UniqueString_To_string(in *UniqueString, out *string, s conversion.Scope) error {
	if *in == NullUniqueString {
		*out = ""
		return nil
	}
	*out = in.String()
	return nil
}

func Convert_string_To_api_UniqueString(in *string, out *UniqueString, s conversion.Scope) error {
	if *in == "" {
		*out = NullUniqueString
		return nil
	}
	*out = UniqueString(unique.Make(*in))
	return nil
}

func Convert_v1beta1_ResourceSlice_To_api_ResourceSlice(in *v1beta1.ResourceSlice, out *ResourceSlice, s conversion.Scope) error {
	// Should be nil, we only have the parameter because generated code expects it.
	if s == nil {
		s = sliceScope{
			slice: in,
		}
	}
	return autoConvert_v1beta1_ResourceSlice_To_api_ResourceSlice(in, out, s)
}

type sliceScope struct {
	slice *v1beta1.ResourceSlice
}

func (s sliceScope) Convert(src, dest interface{}) error {
	return errors.New("conversion.Scope.Convert not implemented")
}

func (s sliceScope) Meta() *conversion.Meta {
	return &conversion.Meta{Context: s.slice}
}

func Convert_v1beta1_Device_To_api_Device(in *v1beta1.Device, out *Device, s conversion.Scope) error {
	out.Name = UniqueString(unique.Make(in.Name))
	switch {
	case in.Basic != nil:
		var outBasic BasicDevice
		if err := Convert_v1beta1_BasicDevice_To_api_BasicDevice(in.Basic, &outBasic, s); err != nil {
			return err
		}
		out.Composite = &CompositeDevice{
			Attributes: outBasic.Attributes,
			Capacity:   outBasic.Capacity,
		}
	case in.Composite != nil:
		var outComposite CompositeDevice
		if err := Convert_v1beta1_CompositeDevice_To_api_CompositeDevice(in.Composite, &outComposite, s); err != nil {
			return err
		}
		out.Composite = &outComposite

		// Resolve mixin references. This depends on being passed the original
		// slice because we need the mixin definitions from it. For a valid slice
		// and correct conversion invocation this will not fail.
		slice, ok := s.Meta().Context.(*v1beta1.ResourceSlice)
		if ok {
			for _, mixinRef := range outComposite.Includes {
				i := slices.IndexFunc(slice.Spec.DeviceMixins, func(mixin v1beta1.DeviceMixin) bool {
					return mixin.Name == mixinRef.Name
				})
				if i >= 0 {
					mixin := slice.Spec.DeviceMixins[i]
					if out.Composite.Attributes == nil && len(mixin.Composite.Attributes) > 0 {
						out.Composite.Attributes = make(map[QualifiedName]DeviceAttribute, len(mixin.Composite.Attributes))
					}
					for name, inAttr := range mixin.Composite.Attributes {
						var outAttr DeviceAttribute
						if err := Convert_v1beta1_DeviceAttribute_To_api_DeviceAttribute(&inAttr, &outAttr, s); err != nil {
							continue
						}
						out.Composite.Attributes[QualifiedName(name)] = outAttr
					}
					if out.Composite.Capacity == nil && len(mixin.Composite.Capacity) > 0 {
						out.Composite.Capacity = make(map[QualifiedName]DeviceCapacity, len(mixin.Composite.Capacity))
					}
					for name, inCap := range mixin.Composite.Capacity {
						var outCap DeviceCapacity
						if err := Convert_v1beta1_DeviceCapacity_To_api_DeviceCapacity(&inCap, &outCap, s); err != nil {
							continue
						}
						out.Composite.Capacity[QualifiedName(name)] = outCap
					}
				}
			}
		}
	default:
		// Don't fail during conversion, it would break the informer cache.
		// Unknown devices get ignored because they are not supported.
	}
	return nil
}

func Convert_api_Device_To_v1beta1_Device(in *Device, out *v1beta1.Device, s conversion.Scope) error {
	return errors.New("conversion to v1beta1.Device not supported")
}
