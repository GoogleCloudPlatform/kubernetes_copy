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

package fieldmanager_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/util/managedfields"
	"k8s.io/apimachinery/pkg/util/managedfields/managedfieldstest"
	"k8s.io/kube-openapi/pkg/validation/spec"
	"sigs.k8s.io/yaml"
)

var fakeTypeConverter = func() managedfields.TypeConverter {
	data, err := os.ReadFile(filepath.Join(strings.Repeat(".."+string(filepath.Separator), 8),
		"api", "openapi-spec", "swagger.json"))
	if err != nil {
		panic(err)
	}
	swagger := spec.Swagger{}
	if err := json.Unmarshal(data, &swagger); err != nil {
		panic(err)
	}
	definitions := map[string]*spec.Schema{}
	for k, v := range swagger.Definitions {
		p := v
		definitions[k] = &p
	}
	typeConverter, err := managedfields.NewTypeConverter(definitions, false)
	if err != nil {
		panic(err)
	}
	return typeConverter
}()

func getObjectBytes(file string) []byte {
	s, err := os.ReadFile(file)
	if err != nil {
		panic(err)
	}
	return s
}

func BenchmarkNewObject(b *testing.B) {
	tests := []struct {
		gvk schema.GroupVersionKind
		obj []byte
	}{
		{
			gvk: schema.FromAPIVersionAndKind("v1", "Pod"),
			obj: getObjectBytes("pod.yaml"),
		},
		{
			gvk: schema.FromAPIVersionAndKind("v1", "Node"),
			obj: getObjectBytes("node.yaml"),
		},
		{
			gvk: schema.FromAPIVersionAndKind("v1", "Endpoints"),
			obj: getObjectBytes("endpoints.yaml"),
		},
	}
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		b.Fatalf("Failed to add to scheme: %v", err)
	}
	for _, test := range tests {
		b.Run(test.gvk.Kind, func(b *testing.B) {
			f := managedfieldstest.NewTestFieldManager(fakeTypeConverter, test.gvk)

			decoder := serializer.NewCodecFactory(scheme).UniversalDecoder(test.gvk.GroupVersion())
			newObj, err := runtime.Decode(decoder, test.obj)
			if err != nil {
				b.Fatalf("Failed to parse yaml object: %v", err)
			}
			objMeta, err := meta.Accessor(newObj)
			if err != nil {
				b.Fatalf("Failed to get object meta: %v", err)
			}
			objMeta.SetManagedFields([]metav1.ManagedFieldsEntry{
				{
					Manager:    "default",
					Operation:  "Update",
					APIVersion: "v1",
				},
			})
			appliedObj := &unstructured.Unstructured{Object: map[string]interface{}{}}
			if err := yaml.Unmarshal(test.obj, &appliedObj.Object); err != nil {
				b.Fatalf("Failed to parse yaml object: %v", err)
			}
			b.Run("Update", func(b *testing.B) {
				b.ReportAllocs()
				b.ResetTimer()
				for n := 0; n < b.N; n++ {
					if err := f.Update(newObj, "fieldmanager_test"); err != nil {
						b.Fatal(err)
					}
					f.Reset()
				}
			})
			b.Run("UpdateTwice", func(b *testing.B) {
				b.ReportAllocs()
				b.ResetTimer()
				for n := 0; n < b.N; n++ {
					if err := f.Update(newObj, "fieldmanager_test"); err != nil {
						b.Fatal(err)
					}
					if err := f.Update(newObj, "fieldmanager_test_2"); err != nil {
						b.Fatal(err)
					}
					f.Reset()
				}
			})
			b.Run("Apply", func(b *testing.B) {
				b.ReportAllocs()
				b.ResetTimer()
				for n := 0; n < b.N; n++ {
					if err := f.Apply(appliedObj, "fieldmanager_test", false); err != nil {
						b.Fatal(err)
					}
					f.Reset()
				}
			})
			b.Run("UpdateApply", func(b *testing.B) {
				b.ReportAllocs()
				b.ResetTimer()
				for n := 0; n < b.N; n++ {
					if err := f.Update(newObj, "fieldmanager_test"); err != nil {
						b.Fatal(err)
					}
					if err := f.Apply(appliedObj, "fieldmanager_test", false); err != nil {
						b.Fatal(err)
					}
					f.Reset()
				}
			})
		})
	}
}

func toUnstructured(b *testing.B, o runtime.Object) *unstructured.Unstructured {
	u, err := runtime.DefaultUnstructuredConverter.ToUnstructured(o)
	if err != nil {
		b.Fatalf("Failed to unmarshal to json: %v", err)
	}
	return &unstructured.Unstructured{Object: u}
}

func BenchmarkConvertObjectToTyped(b *testing.B) {
	tests := []struct {
		gvk schema.GroupVersionKind
		obj []byte
	}{
		{
			gvk: schema.FromAPIVersionAndKind("v1", "Pod"),
			obj: getObjectBytes("pod.yaml"),
		},
		{
			gvk: schema.FromAPIVersionAndKind("v1", "Node"),
			obj: getObjectBytes("node.yaml"),
		},
		{
			gvk: schema.FromAPIVersionAndKind("v1", "Endpoints"),
			obj: getObjectBytes("endpoints.yaml"),
		},
	}
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		b.Fatalf("Failed to add to scheme: %v", err)
	}

	for _, test := range tests {
		b.Run(test.gvk.Kind, func(b *testing.B) {
			decoder := serializer.NewCodecFactory(scheme).UniversalDecoder(test.gvk.GroupVersion())
			structured, err := runtime.Decode(decoder, test.obj)
			if err != nil {
				b.Fatalf("Failed to parse yaml object: %v", err)
			}
			b.Run("structured", func(b *testing.B) {
				b.ReportAllocs()
				b.RunParallel(func(pb *testing.PB) {
					for pb.Next() {
						_, err := fakeTypeConverter.ObjectToTyped(structured)
						if err != nil {
							b.Errorf("Error in ObjectToTyped: %v", err)
						}
					}
				})
			})

			unstructured := toUnstructured(b, structured)
			b.Run("unstructured", func(b *testing.B) {
				b.ReportAllocs()
				b.RunParallel(func(pb *testing.PB) {
					for pb.Next() {
						_, err := fakeTypeConverter.ObjectToTyped(unstructured)
						if err != nil {
							b.Errorf("Error in ObjectToTyped: %v", err)
						}
					}
				})
			})
		})
	}
}

func BenchmarkCompare(b *testing.B) {
	tests := []struct {
		gvk schema.GroupVersionKind
		obj []byte
	}{
		{
			gvk: schema.FromAPIVersionAndKind("v1", "Pod"),
			obj: getObjectBytes("pod.yaml"),
		},
		{
			gvk: schema.FromAPIVersionAndKind("v1", "Node"),
			obj: getObjectBytes("node.yaml"),
		},
		{
			gvk: schema.FromAPIVersionAndKind("v1", "Endpoints"),
			obj: getObjectBytes("endpoints.yaml"),
		},
	}

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		b.Fatalf("Failed to add to scheme: %v", err)
	}

	for _, test := range tests {
		b.Run(test.gvk.Kind, func(b *testing.B) {
			decoder := serializer.NewCodecFactory(scheme).UniversalDecoder(test.gvk.GroupVersion())
			structured, err := runtime.Decode(decoder, test.obj)
			if err != nil {
				b.Fatal(err)
			}
			tv1, err := fakeTypeConverter.ObjectToTyped(structured)
			if err != nil {
				b.Errorf("Error in ObjectToTyped: %v", err)
			}
			tv2, err := fakeTypeConverter.ObjectToTyped(structured)
			if err != nil {
				b.Errorf("Error in ObjectToTyped: %v", err)
			}

			b.Run("structured", func(b *testing.B) {
				b.ReportAllocs()
				for n := 0; n < b.N; n++ {
					_, err = tv1.Compare(tv2)
					if err != nil {
						b.Errorf("Error in ObjectToTyped: %v", err)
					}
				}
			})

			unstructured := toUnstructured(b, structured)
			utv1, err := fakeTypeConverter.ObjectToTyped(unstructured)
			if err != nil {
				b.Errorf("Error in ObjectToTyped: %v", err)
			}
			utv2, err := fakeTypeConverter.ObjectToTyped(unstructured)
			if err != nil {
				b.Errorf("Error in ObjectToTyped: %v", err)
			}
			b.Run("unstructured", func(b *testing.B) {
				b.ReportAllocs()
				b.RunParallel(func(pb *testing.PB) {
					for pb.Next() {
						_, err = utv1.Compare(utv2)
						if err != nil {
							b.Errorf("Error in ObjectToTyped: %v", err)
						}
					}
				})
			})
		})
	}
}

func BenchmarkRepeatedUpdate(b *testing.B) {
	f := managedfieldstest.NewTestFieldManager(fakeTypeConverter, schema.FromAPIVersionAndKind("v1", "Pod"))
	podBytes := getObjectBytes("pod.yaml")

	var obj *corev1.Pod
	if err := yaml.Unmarshal(podBytes, &obj); err != nil {
		b.Fatalf("Failed to parse yaml object: %v", err)
	}
	obj.Spec.Containers[0].Image = "nginx:latest"
	objs := []*corev1.Pod{obj}
	obj = obj.DeepCopy()
	obj.Spec.Containers[0].Image = "nginx:4.3"
	objs = append(objs, obj)

	appliedObj := &unstructured.Unstructured{Object: map[string]interface{}{}}
	if err := yaml.Unmarshal(podBytes, &appliedObj.Object); err != nil {
		b.Fatalf("error decoding YAML: %v", err)
	}

	err := f.Apply(appliedObj, "fieldmanager_apply", false)
	if err != nil {
		b.Fatal(err)
	}

	if err := f.Update(objs[1], "fieldmanager_1"); err != nil {
		b.Fatal(err)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		err := f.Update(objs[n%len(objs)], fmt.Sprintf("fieldmanager_%d", n%len(objs)))
		if err != nil {
			b.Fatal(err)
		}
		f.Reset()
	}
}
