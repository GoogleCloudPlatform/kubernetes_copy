/*
Copyright The Kubernetes Authors.

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

// Code generated by client-gen. DO NOT EDIT.

package fake

import (
	gentype "k8s.io/client-go/gentype"
	v1alpha1 "k8s.io/sample-apiserver/pkg/apis/wardle/v1alpha1"
	wardlev1alpha1 "k8s.io/sample-apiserver/pkg/generated/applyconfiguration/wardle/v1alpha1"
	typedwardlev1alpha1 "k8s.io/sample-apiserver/pkg/generated/clientset/versioned/typed/wardle/v1alpha1"
)

// fakeFischers implements FischerInterface
type fakeFischers struct {
	*gentype.FakeClientWithListAndApply[*v1alpha1.Fischer, *v1alpha1.FischerList, *wardlev1alpha1.FischerApplyConfiguration]
	Fake *FakeWardleV1alpha1
}

func newFakeFischers(fake *FakeWardleV1alpha1) typedwardlev1alpha1.FischerInterface {
	return &fakeFischers{
		gentype.NewFakeClientWithListAndApply[*v1alpha1.Fischer, *v1alpha1.FischerList, *wardlev1alpha1.FischerApplyConfiguration](
			fake.Fake,
			"",
			v1alpha1.SchemeGroupVersion.WithResource("fischers"),
			v1alpha1.SchemeGroupVersion.WithKind("Fischer"),
			func() *v1alpha1.Fischer { return &v1alpha1.Fischer{} },
			func() *v1alpha1.FischerList { return &v1alpha1.FischerList{} },
			func(dst, src *v1alpha1.FischerList) { dst.ListMeta = src.ListMeta },
			func(list *v1alpha1.FischerList) []*v1alpha1.Fischer { return gentype.ToPointerSlice(list.Items) },
			func(list *v1alpha1.FischerList, items []*v1alpha1.Fischer) {
				list.Items = gentype.FromPointerSlice(items)
			},
		),
		fake,
	}
}
