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

package featuregate

import (
	runtime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func (in *IntTest) DeepCopy() *IntTest {
	if in == nil {
		return nil
	}
	out := new(IntTest)
	in.DeepCopyInto(out)
	return out
}

func (in *IntTest) DeepCopyInto(out *IntTest) {
	*out = *in
}

func (in *IntTest) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *IntTest) GetObjectKind() schema.ObjectKind { return schema.EmptyObjectKind }

func (in *StringTest) DeepCopy() *StringTest {
	if in == nil {
		return nil
	}
	out := new(StringTest)
	in.DeepCopyInto(out)
	return out
}

func (in *StringTest) DeepCopyInto(out *StringTest) {
	*out = *in
}

func (in *StringTest) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *StringTest) GetObjectKind() schema.ObjectKind { return schema.EmptyObjectKind }

func (in *StringPtrTest) DeepCopy() *StringPtrTest {
	if in == nil {
		return nil
	}
	out := new(StringPtrTest)
	in.DeepCopyInto(out)
	return out
}

func (in *StringPtrTest) DeepCopyInto(out *StringPtrTest) {
	*out = *in
}

func (in *StringPtrTest) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *StringPtrTest) GetObjectKind() schema.ObjectKind { return schema.EmptyObjectKind }

func (in *NestedStructTest) DeepCopy() *NestedStructTest {
	if in == nil {
		return nil
	}
	out := new(NestedStructTest)
	in.DeepCopyInto(out)
	return out
}

func (in *NestedStructTest) DeepCopyInto(out *NestedStructTest) {
	*out = *in
}

func (in *NestedStructTest) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *NestedStructTest) GetObjectKind() schema.ObjectKind { return schema.EmptyObjectKind }

func (in *MultipleGatesIntTest) DeepCopy() *MultipleGatesIntTest {
	if in == nil {
		return nil
	}
	out := new(MultipleGatesIntTest)
	in.DeepCopyInto(out)
	return out
}

func (in *MultipleGatesIntTest) DeepCopyInto(out *MultipleGatesIntTest) {
	*out = *in
}

func (in *MultipleGatesIntTest) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *MultipleGatesIntTest) GetObjectKind() schema.ObjectKind { return schema.EmptyObjectKind }
