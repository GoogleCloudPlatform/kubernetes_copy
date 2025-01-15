/*
Copyright 2022 The Kubernetes Authors.

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

package validation

import (
	"fmt"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/kubernetes/pkg/apis/core"
	resourceapi "k8s.io/kubernetes/pkg/apis/resource"
	"k8s.io/utils/ptr"
)

func testAttributes() map[resourceapi.QualifiedName]resourceapi.DeviceAttribute {
	return map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
		"int":     {IntValue: ptr.To(int64(42))},
		"string":  {StringValue: ptr.To("hello world")},
		"version": {VersionValue: ptr.To("1.2.3")},
		"bool":    {BoolValue: ptr.To(true)},
	}
}

func testCapacity() map[resourceapi.QualifiedName]resourceapi.DeviceCapacity {
	return map[resourceapi.QualifiedName]resourceapi.DeviceCapacity{
		"memory": {Value: resource.MustParse("1Gi")},
	}
}

func testResourceSlice(name, nodeName, driverName string, numDevices int) *resourceapi.ResourceSlice {
	slice := &resourceapi.ResourceSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: resourceapi.ResourceSliceSpec{
			NodeName: nodeName,
			Driver:   driverName,
			Pool: resourceapi.ResourcePool{
				Name:               nodeName,
				ResourceSliceCount: 1,
			},
		},
	}
	for i := 0; i < numDevices; i++ {
		device := resourceapi.Device{
			Name: fmt.Sprintf("device-%d", i),
			Basic: &resourceapi.BasicDevice{
				Attributes: testAttributes(),
				Capacity:   testCapacity(),
			},
		}
		slice.Spec.Devices = append(slice.Spec.Devices, device)
	}
	return slice
}

func testResourceSliceWithCompositeMixin(name, nodeName, driverName string, numMixin, numDevicePerMixin int) *resourceapi.ResourceSlice {
	slice := &resourceapi.ResourceSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: resourceapi.ResourceSliceSpec{
			NodeName: nodeName,
			Driver:   driverName,
			Pool: resourceapi.ResourcePool{
				Name:               nodeName,
				ResourceSliceCount: 1,
			},
		},
	}

	for i := 0; i < numMixin; i++ {
		deviceMixinName := fmt.Sprintf("devicemixin-%d", i)
		slice.Spec.DeviceMixins = append(slice.Spec.DeviceMixins, resourceapi.DeviceMixin{
			Name: deviceMixinName,
			Composite: &resourceapi.CompositeDeviceMixin{
				Attributes: testAttributes(),
				Capacity:   testCapacity(),
			},
		})

		for j := 0; j < numDevicePerMixin; j++ {
			slice.Spec.Devices = append(slice.Spec.Devices, resourceapi.Device{
				Name: fmt.Sprintf("device-%d-%d", i, j),
				Composite: &resourceapi.CompositeDevice{
					Includes: []resourceapi.DeviceMixinRef{
						{
							Name: deviceMixinName,
						},
					},
					Attributes: testAttributes(),
					Capacity:   testCapacity(),
				},
			})
		}
	}
	return slice
}

func updateMixinNameAndRefs(slice *resourceapi.ResourceSlice, oldName, newName string) {
	for i := range slice.Spec.DeviceMixins {
		if slice.Spec.DeviceMixins[i].Name == oldName {
			slice.Spec.DeviceMixins[i].Name = newName
		}
	}
	for i := range slice.Spec.Devices {
		if slice.Spec.Devices[i].Composite == nil {
			continue
		}
		for j := range slice.Spec.Devices[i].Composite.Includes {
			if slice.Spec.Devices[i].Composite.Includes[j].Name == oldName {
				slice.Spec.Devices[i].Composite.Includes[j].Name = newName
			}
		}
	}
}

func TestValidateResourceSlice(t *testing.T) {
	goodName := "foo"
	badName := "!@#$%^"
	driverName := "test.example.com"
	now := metav1.Now()
	badValue := "spaces not allowed"

	scenarios := map[string]struct {
		slice        *resourceapi.ResourceSlice
		wantFailures field.ErrorList
	}{
		"good": {
			slice: testResourceSlice(goodName, goodName, driverName, resourceapi.ResourceSliceMaxDevicesAndMixins),
		},
		"too-large": {
			wantFailures: field.ErrorList{field.Invalid(field.NewPath("spec"), resourceapi.ResourceSliceMaxDevicesAndMixins+1, "the total number of devices and mixins must not exceed 128")},
			slice:        testResourceSlice(goodName, goodName, goodName, resourceapi.ResourceSliceMaxDevicesAndMixins+1),
		},
		"missing-name": {
			wantFailures: field.ErrorList{field.Required(field.NewPath("metadata", "name"), "name or generateName is required")},
			slice:        testResourceSlice("", goodName, driverName, 1),
		},
		"bad-name": {
			wantFailures: field.ErrorList{field.Invalid(field.NewPath("metadata", "name"), badName, "a lowercase RFC 1123 subdomain must consist of lower case alphanumeric characters, '-' or '.', and must start and end with an alphanumeric character (e.g. 'example.com', regex used for validation is '[a-z0-9]([-a-z0-9]*[a-z0-9])?(\\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*')")},
			slice:        testResourceSlice(badName, goodName, driverName, 1),
		},
		"generate-name": {
			slice: func() *resourceapi.ResourceSlice {
				slice := testResourceSlice(goodName, goodName, driverName, 1)
				slice.GenerateName = "prefix-"
				return slice
			}(),
		},
		"uid": {
			slice: func() *resourceapi.ResourceSlice {
				slice := testResourceSlice(goodName, goodName, driverName, 1)
				slice.UID = "ac051fac-2ead-46d9-b8b4-4e0fbeb7455d"
				return slice
			}(),
		},
		"resource-version": {
			slice: func() *resourceapi.ResourceSlice {
				slice := testResourceSlice(goodName, goodName, driverName, 1)
				slice.ResourceVersion = "1"
				return slice
			}(),
		},
		"generation": {
			slice: func() *resourceapi.ResourceSlice {
				slice := testResourceSlice(goodName, goodName, driverName, 1)
				slice.Generation = 100
				return slice
			}(),
		},
		"creation-timestamp": {
			slice: func() *resourceapi.ResourceSlice {
				slice := testResourceSlice(goodName, goodName, driverName, 1)
				slice.CreationTimestamp = now
				return slice
			}(),
		},
		"deletion-grace-period-seconds": {
			slice: func() *resourceapi.ResourceSlice {
				slice := testResourceSlice(goodName, goodName, driverName, 1)
				slice.DeletionGracePeriodSeconds = ptr.To[int64](10)
				return slice
			}(),
		},
		"owner-references": {
			slice: func() *resourceapi.ResourceSlice {
				slice := testResourceSlice(goodName, goodName, driverName, 1)
				slice.OwnerReferences = []metav1.OwnerReference{
					{
						APIVersion: "v1",
						Kind:       "pod",
						Name:       "foo",
						UID:        "ac051fac-2ead-46d9-b8b4-4e0fbeb7455d",
					},
				}
				return slice
			}(),
		},
		"finalizers": {
			slice: func() *resourceapi.ResourceSlice {
				slice := testResourceSlice(goodName, goodName, driverName, 1)
				slice.Finalizers = []string{
					"example.com/foo",
				}
				return slice
			}(),
		},
		"managed-fields": {
			slice: func() *resourceapi.ResourceSlice {
				slice := testResourceSlice(goodName, goodName, driverName, 1)
				slice.ManagedFields = []metav1.ManagedFieldsEntry{
					{
						FieldsType: "FieldsV1",
						Operation:  "Apply",
						APIVersion: "apps/v1",
						Manager:    "foo",
					},
				}
				return slice
			}(),
		},
		"good-labels": {
			slice: func() *resourceapi.ResourceSlice {
				slice := testResourceSlice(goodName, goodName, driverName, 1)
				slice.Labels = map[string]string{
					"apps.kubernetes.io/name": "test",
				}
				return slice
			}(),
		},
		"bad-labels": {
			wantFailures: field.ErrorList{field.Invalid(field.NewPath("metadata", "labels"), badValue, "a valid label must be an empty string or consist of alphanumeric characters, '-', '_' or '.', and must start and end with an alphanumeric character (e.g. 'MyValue',  or 'my_value',  or '12345', regex used for validation is '(([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9])?')")},
			slice: func() *resourceapi.ResourceSlice {
				slice := testResourceSlice(goodName, goodName, driverName, 1)
				slice.Labels = map[string]string{
					"hello-world": badValue,
				}
				return slice
			}(),
		},
		"good-annotations": {
			slice: func() *resourceapi.ResourceSlice {
				slice := testResourceSlice(goodName, goodName, driverName, 1)
				slice.Annotations = map[string]string{
					"foo": "bar",
				}
				return slice
			}(),
		},
		"bad-annotations": {
			wantFailures: field.ErrorList{field.Invalid(field.NewPath("metadata", "annotations"), badName, "name part must consist of alphanumeric characters, '-', '_' or '.', and must start and end with an alphanumeric character (e.g. 'MyName',  or 'my.name',  or '123-abc', regex used for validation is '([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9]')")},
			slice: func() *resourceapi.ResourceSlice {
				slice := testResourceSlice(goodName, goodName, driverName, 1)
				slice.Annotations = map[string]string{
					badName: "hello world",
				}
				return slice
			}(),
		},
		"bad-nodename": {
			wantFailures: field.ErrorList{
				field.Invalid(field.NewPath("spec", "pool", "name"), badName, "a lowercase RFC 1123 subdomain must consist of lower case alphanumeric characters, '-' or '.', and must start and end with an alphanumeric character (e.g. 'example.com', regex used for validation is '[a-z0-9]([-a-z0-9]*[a-z0-9])?(\\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*')"),
				field.Invalid(field.NewPath("spec", "nodeName"), badName, "a lowercase RFC 1123 subdomain must consist of lower case alphanumeric characters, '-' or '.', and must start and end with an alphanumeric character (e.g. 'example.com', regex used for validation is '[a-z0-9]([-a-z0-9]*[a-z0-9])?(\\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*')"),
			},
			slice: testResourceSlice(goodName, badName, driverName, 1),
		},
		"bad-multi-pool-name": {
			wantFailures: field.ErrorList{
				field.Invalid(field.NewPath("spec", "pool", "name"), badName, "a lowercase RFC 1123 subdomain must consist of lower case alphanumeric characters, '-' or '.', and must start and end with an alphanumeric character (e.g. 'example.com', regex used for validation is '[a-z0-9]([-a-z0-9]*[a-z0-9])?(\\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*')"),
				field.Invalid(field.NewPath("spec", "pool", "name"), badName, "a lowercase RFC 1123 subdomain must consist of lower case alphanumeric characters, '-' or '.', and must start and end with an alphanumeric character (e.g. 'example.com', regex used for validation is '[a-z0-9]([-a-z0-9]*[a-z0-9])?(\\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*')"),
				field.Invalid(field.NewPath("spec", "nodeName"), badName+"/"+badName, "a lowercase RFC 1123 subdomain must consist of lower case alphanumeric characters, '-' or '.', and must start and end with an alphanumeric character (e.g. 'example.com', regex used for validation is '[a-z0-9]([-a-z0-9]*[a-z0-9])?(\\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*')"),
			},
			slice: testResourceSlice(goodName, badName+"/"+badName, driverName, 1),
		},
		"good-pool-name": {
			slice: func() *resourceapi.ResourceSlice {
				slice := testResourceSlice(goodName, goodName, driverName, 1)
				slice.Spec.Pool.Name = strings.Repeat("x", resourceapi.PoolNameMaxLength)
				return slice
			}(),
		},
		"bad-pool": {
			wantFailures: field.ErrorList{
				field.TooLongMaxLength(field.NewPath("spec", "pool", "name"), strings.Repeat("x/", resourceapi.PoolNameMaxLength/2)+"xy", resourceapi.PoolNameMaxLength),
				field.Invalid(field.NewPath("spec", "pool", "resourceSliceCount"), int64(0), "must be greater than zero"),
				field.Invalid(field.NewPath("spec", "pool", "generation"), int64(-1), "must be greater than or equal to zero"),
			},
			slice: func() *resourceapi.ResourceSlice {
				slice := testResourceSlice(goodName, goodName, driverName, 1)
				slice.Spec.Pool.Name = strings.Repeat("x/", resourceapi.PoolNameMaxLength/2) + "xy"
				slice.Spec.Pool.ResourceSliceCount = 0
				slice.Spec.Pool.Generation = -1
				return slice
			}(),
		},
		"missing-pool-name": {
			wantFailures: field.ErrorList{
				field.Required(field.NewPath("spec", "pool", "name"), ""),
			},
			slice: func() *resourceapi.ResourceSlice {
				slice := testResourceSlice(goodName, goodName, driverName, 1)
				slice.Spec.Pool.Name = ""
				return slice
			}(),
		},
		"bad-empty-node-selector": {
			wantFailures: field.ErrorList{
				field.Required(field.NewPath("spec", "nodeSelector", "nodeSelectorTerms"), "must have at least one node selector term"),                             // From core validation.
				field.Invalid(field.NewPath("spec", "nodeSelector", "nodeSelectorTerms"), []core.NodeSelectorTerm(nil), "must have exactly one node selector term"), // From DRA validation.
			},
			slice: func() *resourceapi.ResourceSlice {
				slice := testResourceSlice(goodName, goodName, driverName, 1)
				slice.Spec.NodeName = ""
				slice.Spec.NodeSelector = &core.NodeSelector{}
				return slice
			}(),
		},
		"bad-node-selection": {
			wantFailures: field.ErrorList{field.Invalid(field.NewPath("spec"), nil, "exactly one of `nodeName`, `nodeSelector`, or `allNodes` is required")},
			slice: func() *resourceapi.ResourceSlice {
				slice := testResourceSlice(goodName, goodName, driverName, 1)
				slice.Spec.NodeName = "worker"
				slice.Spec.NodeSelector = &core.NodeSelector{
					NodeSelectorTerms: []core.NodeSelectorTerm{{MatchFields: []core.NodeSelectorRequirement{{Key: "metadata.name", Operator: core.NodeSelectorOpIn, Values: []string{"worker"}}}}},
				}
				return slice
			}(),
		},
		"bad-node-selection-all-nodes": {
			wantFailures: field.ErrorList{field.Invalid(field.NewPath("spec"), nil, "exactly one of `nodeName`, `nodeSelector`, or `allNodes` is required")},
			slice: func() *resourceapi.ResourceSlice {
				slice := testResourceSlice(goodName, goodName, driverName, 1)
				slice.Spec.NodeName = "worker"
				slice.Spec.AllNodes = true
				return slice
			}(),
		},
		"empty-node-selection": {
			wantFailures: field.ErrorList{field.Required(field.NewPath("spec"), "exactly one of `nodeName`, `nodeSelector`, or `allNodes` is required")},
			slice: func() *resourceapi.ResourceSlice {
				slice := testResourceSlice(goodName, goodName, driverName, 1)
				slice.Spec.NodeName = ""
				return slice
			}(),
		},
		"bad-drivername": {
			wantFailures: field.ErrorList{field.Invalid(field.NewPath("spec", "driver"), badName, "a lowercase RFC 1123 subdomain must consist of lower case alphanumeric characters, '-' or '.', and must start and end with an alphanumeric character (e.g. 'example.com', regex used for validation is '[a-z0-9]([-a-z0-9]*[a-z0-9])?(\\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*')")},
			slice:        testResourceSlice(goodName, goodName, badName, 1),
		},
		"bad-devices": {
			wantFailures: field.ErrorList{
				field.Invalid(field.NewPath("spec", "devices").Index(1).Child("name"), badName, "a lowercase RFC 1123 label must consist of lower case alphanumeric characters or '-', and must start and end with an alphanumeric character (e.g. 'my-name',  or '123-abc', regex used for validation is '[a-z0-9]([-a-z0-9]*[a-z0-9])?')"),
				field.Required(field.NewPath("spec", "devices").Index(2), "exactly one of `basic`, or `composite` is required"),
			},
			slice: func() *resourceapi.ResourceSlice {
				slice := testResourceSlice(goodName, goodName, goodName, 3)
				slice.Spec.Devices[1].Name = badName
				slice.Spec.Devices[2].Basic = nil
				return slice
			}(),
		},
		"invalid-node-selecor-label-value": {
			wantFailures: field.ErrorList{field.Invalid(field.NewPath("spec", "nodeSelector", "nodeSelectorTerms").Index(0).Child("matchExpressions").Index(0).Child("values").Index(0), "-1", "a valid label must be an empty string or consist of alphanumeric characters, '-', '_' or '.', and must start and end with an alphanumeric character (e.g. 'MyValue',  or 'my_value',  or '12345', regex used for validation is '(([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9])?')")},
			slice: func() *resourceapi.ResourceSlice {
				slice := testResourceSlice(goodName, goodName, goodName, 3)
				slice.Spec.NodeName = ""
				slice.Spec.NodeSelector = &core.NodeSelector{
					NodeSelectorTerms: []core.NodeSelectorTerm{{
						MatchExpressions: []core.NodeSelectorRequirement{{
							Key:      "foo",
							Operator: core.NodeSelectorOpIn,
							Values:   []string{"-1"},
						}},
					}},
				}
				return slice
			}(),
		},
		"valid-mixin-and-composite-device": {
			slice: func() *resourceapi.ResourceSlice {
				return testResourceSliceWithCompositeMixin(goodName, goodName, goodName, 1, 1)
			}(),
		},
		"invalid-mixin-name": {
			wantFailures: field.ErrorList{field.Invalid(field.NewPath("spec", "deviceMixins").Index(1).Child("name"), badName, "a lowercase RFC 1123 label must consist of lower case alphanumeric characters or '-', and must start and end with an alphanumeric character (e.g. 'my-name',  or '123-abc', regex used for validation is '[a-z0-9]([-a-z0-9]*[a-z0-9])?')")},
			slice: func() *resourceapi.ResourceSlice {
				slice := testResourceSliceWithCompositeMixin(goodName, goodName, goodName, 1, 1)
				slice.Spec.DeviceMixins = append(slice.Spec.DeviceMixins, resourceapi.DeviceMixin{
					Name:      badName,
					Composite: &resourceapi.CompositeDeviceMixin{},
				})
				return slice
			}(),
		},
		"invalid-mixin-without-composite": {
			wantFailures: field.ErrorList{field.Required(field.NewPath("spec", "deviceMixins").Index(1), "`composite` is required")},
			slice: func() *resourceapi.ResourceSlice {
				slice := testResourceSliceWithCompositeMixin(goodName, goodName, goodName, 1, 1)
				slice.Spec.DeviceMixins = append(slice.Spec.DeviceMixins, resourceapi.DeviceMixin{
					Name: goodName,
				})
				return slice
			}(),
		},
		"reused-mixin-name": {
			wantFailures: field.ErrorList{field.Duplicate(field.NewPath("spec", "deviceMixins").Index(1).Child("name"), "foo")},
			slice: func() *resourceapi.ResourceSlice {
				slice := testResourceSliceWithCompositeMixin(goodName, goodName, goodName, 2, 1)
				updateMixinNameAndRefs(slice, "devicemixin-0", "foo")
				updateMixinNameAndRefs(slice, "devicemixin-1", "foo")
				return slice
			}(),
		},
		"device-with-both-basic-and-composite-type": {
			wantFailures: field.ErrorList{field.Invalid(field.NewPath("spec", "devices").Index(0), nil, "exactly one of `basic`, or `composite` is required")},
			slice: func() *resourceapi.ResourceSlice {
				slice := testResourceSliceWithCompositeMixin(goodName, goodName, goodName, 1, 1)
				slice.Spec.Devices[0].Basic = &resourceapi.BasicDevice{
					Attributes: testAttributes(),
				}
				return slice
			}(),
		},
		"composite-device-with-too-many-mixin-refs": {
			wantFailures: field.ErrorList{field.TooMany(field.NewPath("spec", "devices").Index(0).Child("composite", "includes"), 9, 8)},
			slice: func() *resourceapi.ResourceSlice {
				slice := testResourceSliceWithCompositeMixin(goodName, goodName, goodName, 9, 1)
				var deviceMixinRefs []resourceapi.DeviceMixinRef
				for i := 0; i < 9; i++ {
					deviceMixinRefs = append(deviceMixinRefs, resourceapi.DeviceMixinRef{
						Name: fmt.Sprintf("devicemixin-%d", i),
					})
				}
				slice.Spec.Devices[0].Composite.Includes = deviceMixinRefs
				return slice
			}(),
		},
		"composite-device-reference-with-bad-name": {
			wantFailures: field.ErrorList{
				field.Invalid(field.NewPath("spec", "devices").Index(0).Child("composite", "includes").Index(0).Child("name"), badName, "a lowercase RFC 1123 label must consist of lower case alphanumeric characters or '-', and must start and end with an alphanumeric character (e.g. 'my-name',  or '123-abc', regex used for validation is '[a-z0-9]([-a-z0-9]*[a-z0-9])?')"),
				field.Invalid(field.NewPath("spec", "devices").Index(0).Child("composite", "includes").Index(0), badName, "must be the name of a mixin in the resource slice"),
			},
			slice: func() *resourceapi.ResourceSlice {
				slice := testResourceSliceWithCompositeMixin(goodName, goodName, goodName, 1, 1)
				slice.Spec.Devices[0].Composite.Includes = []resourceapi.DeviceMixinRef{
					{
						Name: badName,
					},
				}
				return slice
			}(),
		},
		"composite-device-references-unknown-mixin": {
			wantFailures: field.ErrorList{field.Invalid(field.NewPath("spec", "devices").Index(0).Child("composite", "includes").Index(0), "unknown-mixin", "must be the name of a mixin in the resource slice")},
			slice: func() *resourceapi.ResourceSlice {
				slice := testResourceSliceWithCompositeMixin(goodName, goodName, goodName, 1, 1)
				slice.Spec.Devices[0].Composite.Includes = []resourceapi.DeviceMixinRef{
					{
						Name: "unknown-mixin",
					},
				}
				return slice
			}(),
		},
		"composite-device-with-partition": {
			slice: func() *resourceapi.ResourceSlice {
				slice := testResourceSliceWithCompositeMixin(goodName, goodName, goodName, 2, 1)
				slice.Spec.Devices[1].Composite.ConsumesCapacityFrom = []resourceapi.DeviceRef{
					{
						Name: "device-0-0",
					},
				}
				return slice
			}(),
		},
		"composite-device-with-too-many-device-refs": {
			wantFailures: field.ErrorList{field.TooMany(field.NewPath("spec", "devices").Index(0).Child("composite", "consumesCapacityFrom"), 9, 8)},
			slice: func() *resourceapi.ResourceSlice {
				slice := testResourceSliceWithCompositeMixin(goodName, goodName, goodName, 1, 10)
				var deviceRefs []resourceapi.DeviceRef
				for i := 1; i < 10; i++ {
					deviceRefs = append(deviceRefs, resourceapi.DeviceRef{
						Name: fmt.Sprintf("device-0-%d", i),
					})
				}
				slice.Spec.Devices[0].Composite.ConsumesCapacityFrom = deviceRefs
				return slice
			}(),
		},
		"composite-device-with-device-ref-with-bad-name": {
			wantFailures: field.ErrorList{
				field.Invalid(field.NewPath("spec", "devices").Index(0).Child("composite", "consumesCapacityFrom").Index(0).Child("name"), badName, "a lowercase RFC 1123 label must consist of lower case alphanumeric characters or '-', and must start and end with an alphanumeric character (e.g. 'my-name',  or '123-abc', regex used for validation is '[a-z0-9]([-a-z0-9]*[a-z0-9])?')"),
				field.Invalid(field.NewPath("spec", "devices").Index(0).Child("composite", "consumesCapacityFrom").Index(0), badName, "must be the name of a device in the resource slice"),
			},
			slice: func() *resourceapi.ResourceSlice {
				slice := testResourceSliceWithCompositeMixin(goodName, goodName, goodName, 1, 1)
				slice.Spec.Devices[0].Composite.ConsumesCapacityFrom = []resourceapi.DeviceRef{
					{
						Name: badName,
					},
				}
				return slice
			}(),
		},
		"composite-device-with-unknown-device-ref": {
			wantFailures: field.ErrorList{
				field.Invalid(field.NewPath("spec", "devices").Index(0).Child("composite", "consumesCapacityFrom").Index(0), "not-a-device", "must be the name of a device in the resource slice"),
			},
			slice: func() *resourceapi.ResourceSlice {
				slice := testResourceSliceWithCompositeMixin(goodName, goodName, goodName, 1, 1)
				slice.Spec.Devices[0].Composite.ConsumesCapacityFrom = []resourceapi.DeviceRef{
					{
						Name: "not-a-device",
					},
				}
				return slice
			}(),
		},
	}

	for name, scenario := range scenarios {
		t.Run(name, func(t *testing.T) {
			foo := name
			errs := ValidateResourceSlice(scenario.slice)
			assertFailures(t, scenario.wantFailures, errs)
			_ = foo
		})
	}
}

func TestAttributesAndCapacities(t *testing.T) {
	scenarios := map[string]struct {
		deviceCount      int
		attributes       []map[resourceapi.QualifiedName]resourceapi.DeviceAttribute
		capacities       []map[resourceapi.QualifiedName]resourceapi.DeviceCapacity
		wantFailuresFunc func(*field.Path, string) field.ErrorList
	}{
		"bad-attribute": {
			deviceCount: 4,
			wantFailuresFunc: func(basePath *field.Path, deviceType string) field.ErrorList {
				return field.ErrorList{
					field.TypeInvalid(basePath.Index(0).Child(deviceType, "attributes").Key(badName), badName, "a valid C identifier must start with alphabetic character or '_', followed by a string of alphanumeric characters or '_' (e.g. 'my_name',  or 'MY_NAME',  or 'MyName', regex used for validation is '[A-Za-z_][A-Za-z0-9_]*')"),
					field.Required(basePath.Index(0).Child(deviceType, "attributes").Key(badName), "exactly one value must be specified"),
					field.Invalid(basePath.Index(1).Child(deviceType, "attributes").Key(goodName), resourceapi.DeviceAttribute{StringValue: ptr.To("x"), VersionValue: ptr.To("1.2.3")}, "exactly one value must be specified"),
					field.Invalid(basePath.Index(2).Child(deviceType, "attributes").Key(goodName).Child("version"), strings.Repeat("x", resourceapi.DeviceAttributeMaxValueLength+1), "must be a string compatible with semver.org spec 2.0.0"),
					field.TooLongMaxLength(basePath.Index(2).Child(deviceType, "attributes").Key(goodName).Child("version"), strings.Repeat("x", resourceapi.DeviceAttributeMaxValueLength+1), resourceapi.DeviceAttributeMaxValueLength),
					field.TooLongMaxLength(basePath.Index(3).Child(deviceType, "attributes").Key(goodName).Child("string"), strings.Repeat("x", resourceapi.DeviceAttributeMaxValueLength+1), resourceapi.DeviceAttributeMaxValueLength),
				}
			},
			attributes: []map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
				{
					resourceapi.QualifiedName(badName): {},
				},
				{
					resourceapi.QualifiedName(goodName): {StringValue: ptr.To("x"), VersionValue: ptr.To("1.2.3")},
				},
				{
					resourceapi.QualifiedName(goodName): {VersionValue: ptr.To(strings.Repeat("x", resourceapi.DeviceAttributeMaxValueLength+1))},
				},
				{
					resourceapi.QualifiedName(goodName): {StringValue: ptr.To(strings.Repeat("x", resourceapi.DeviceAttributeMaxValueLength+1))},
				},
			},
		},
		"good-attribute-names": {
			deviceCount: 1,
			attributes: []map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
				{
					resourceapi.QualifiedName(strings.Repeat("x", resourceapi.DeviceMaxIDLength)):                                                                {StringValue: ptr.To("y")},
					resourceapi.QualifiedName(strings.Repeat("x", resourceapi.DeviceMaxDomainLength) + "/" + strings.Repeat("y", resourceapi.DeviceMaxIDLength)): {StringValue: ptr.To("z")},
				},
			},
		},
		"bad-attribute-c-identifier": {
			deviceCount: 1,
			attributes: []map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
				{
					resourceapi.QualifiedName(strings.Repeat(".", resourceapi.DeviceMaxIDLength+1)): {StringValue: ptr.To("y")},
				},
			},
			wantFailuresFunc: func(basePath *field.Path, deviceType string) field.ErrorList {
				return field.ErrorList{
					field.TooLongMaxLength(basePath.Index(0).Child(deviceType, "attributes").Key(strings.Repeat(".", resourceapi.DeviceMaxIDLength+1)), strings.Repeat(".", resourceapi.DeviceMaxIDLength+1), resourceapi.DeviceMaxIDLength),
					field.TypeInvalid(basePath.Index(0).Child(deviceType, "attributes").Key(strings.Repeat(".", resourceapi.DeviceMaxIDLength+1)), strings.Repeat(".", resourceapi.DeviceMaxIDLength+1), "a valid C identifier must start with alphabetic character or '_', followed by a string of alphanumeric characters or '_' (e.g. 'my_name',  or 'MY_NAME',  or 'MyName', regex used for validation is '[A-Za-z_][A-Za-z0-9_]*')"),
				}
			},
		},
		"bad-attribute-domain": {
			deviceCount: 1,
			attributes: []map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
				{
					resourceapi.QualifiedName(strings.Repeat("_", resourceapi.DeviceMaxDomainLength+1) + "/y"): {StringValue: ptr.To("z")},
				},
			},
			wantFailuresFunc: func(basePath *field.Path, deviceType string) field.ErrorList {
				return field.ErrorList{
					field.TooLong(basePath.Index(0).Child(deviceType, "attributes").Key(strings.Repeat("_", resourceapi.DeviceMaxDomainLength+1)+"/y"), strings.Repeat("_", resourceapi.DeviceMaxDomainLength+1), resourceapi.DeviceMaxDomainLength),
					field.Invalid(basePath.Index(0).Child(deviceType, "attributes").Key(strings.Repeat("_", resourceapi.DeviceMaxDomainLength+1)+"/y"), strings.Repeat("_", resourceapi.DeviceMaxDomainLength+1), "a lowercase RFC 1123 subdomain must consist of lower case alphanumeric characters, '-' or '.', and must start and end with an alphanumeric character (e.g. 'example.com', regex used for validation is '[a-z0-9]([-a-z0-9]*[a-z0-9])?(\\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*')"),
				}
			},
		},
		"bad-key-too-long": {
			deviceCount: 1,
			attributes: []map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
				{
					resourceapi.QualifiedName(strings.Repeat("x", resourceapi.DeviceMaxDomainLength+1) + "/" + strings.Repeat("y", resourceapi.DeviceMaxIDLength+1)): {StringValue: ptr.To("z")},
				},
			},
			wantFailuresFunc: func(basePath *field.Path, deviceType string) field.ErrorList {
				return field.ErrorList{
					field.TooLong(basePath.Index(0).Child(deviceType, "attributes").Key("xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx...xxxxxxxxxxxx/yyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyy"), strings.Repeat("x", resourceapi.DeviceMaxDomainLength+1), resourceapi.DeviceMaxDomainLength),
					field.TooLongMaxLength(basePath.Index(0).Child(deviceType, "attributes").Key("xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx...xxxxxxxxxxxx/yyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyy"), strings.Repeat("y", resourceapi.DeviceMaxIDLength+1), resourceapi.DeviceMaxIDLength),
				}
			},
		},
		"bad-attribute-empty-domain-and-c-identifier": {
			deviceCount: 1,
			attributes: []map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
				{
					resourceapi.QualifiedName("/"): {StringValue: ptr.To("z")},
				},
			},
			wantFailuresFunc: func(basePath *field.Path, deviceType string) field.ErrorList {
				return field.ErrorList{
					field.Required(basePath.Index(0).Child(deviceType, "attributes").Key("/"), "the domain must not be empty"),
					field.Required(basePath.Index(0).Child(deviceType, "attributes").Key("/"), "the name must not be empty"),
				}
			},
		},
		"combined-attributes-and-capacity-length": {
			deviceCount: 3,
			attributes: []map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
				func() map[resourceapi.QualifiedName]resourceapi.DeviceAttribute {
					attributes := make(map[resourceapi.QualifiedName]resourceapi.DeviceAttribute)
					for i := 0; i < resourceapi.ResourceSliceMaxAttributesAndCapacitiesPerDevice; i++ {
						attributes[resourceapi.QualifiedName(fmt.Sprintf("attr_%d", i))] = resourceapi.DeviceAttribute{StringValue: ptr.To("x")}
					}
					return attributes
				}(),
				func() map[resourceapi.QualifiedName]resourceapi.DeviceAttribute {
					attributes := make(map[resourceapi.QualifiedName]resourceapi.DeviceAttribute)
					return attributes
				}(),
				func() map[resourceapi.QualifiedName]resourceapi.DeviceAttribute {
					attributes := make(map[resourceapi.QualifiedName]resourceapi.DeviceAttribute)
					for i := 0; i < resourceapi.ResourceSliceMaxAttributesAndCapacitiesPerDevice; i++ {
						attributes[resourceapi.QualifiedName(fmt.Sprintf("attr_%d", i))] = resourceapi.DeviceAttribute{StringValue: ptr.To("x")}
					}
					return attributes
				}(),
			},
			capacities: []map[resourceapi.QualifiedName]resourceapi.DeviceCapacity{
				func() map[resourceapi.QualifiedName]resourceapi.DeviceCapacity {
					capacities := make(map[resourceapi.QualifiedName]resourceapi.DeviceCapacity)
					return capacities
				}(),
				func() map[resourceapi.QualifiedName]resourceapi.DeviceCapacity {
					capacities := make(map[resourceapi.QualifiedName]resourceapi.DeviceCapacity)
					quantity := resource.MustParse("1Gi")
					capacity := resourceapi.DeviceCapacity{Value: quantity}
					for i := 0; i < resourceapi.ResourceSliceMaxAttributesAndCapacitiesPerDevice; i++ {
						capacities[resourceapi.QualifiedName(fmt.Sprintf("cap_%d", i))] = capacity
					}
					return capacities
				}(),
				func() map[resourceapi.QualifiedName]resourceapi.DeviceCapacity {
					capacities := make(map[resourceapi.QualifiedName]resourceapi.DeviceCapacity)
					quantity := resource.MustParse("1Gi")
					capacity := resourceapi.DeviceCapacity{Value: quantity}
					capacities[resourceapi.QualifiedName("cap")] = capacity
					return capacities
				}(),
			},
			wantFailuresFunc: func(basePath *field.Path, deviceType string) field.ErrorList {
				return field.ErrorList{
					field.Invalid(basePath.Index(2).Child(deviceType), resourceapi.ResourceSliceMaxAttributesAndCapacitiesPerDevice+1, fmt.Sprintf("the total number of attributes and capacities must not exceed %d", resourceapi.ResourceSliceMaxAttributesAndCapacitiesPerDevice)),
				}
			},
		},
	}

	fieldConfigs := map[string]struct {
		sliceCreateFunc          func(count int) *resourceapi.ResourceSlice
		updateSliceAttributeFunc func(slice *resourceapi.ResourceSlice, index int, attribute map[resourceapi.QualifiedName]resourceapi.DeviceAttribute)
		updateSliceCapacityFunc  func(slice *resourceapi.ResourceSlice, index int, capacity map[resourceapi.QualifiedName]resourceapi.DeviceCapacity)
		baseFieldPath            *field.Path
		deviceType               string
	}{
		"device-basic": {
			sliceCreateFunc: func(count int) *resourceapi.ResourceSlice {
				return testResourceSlice(goodName, goodName, goodName, count)
			},
			updateSliceAttributeFunc: func(slice *resourceapi.ResourceSlice, index int, attribute map[resourceapi.QualifiedName]resourceapi.DeviceAttribute) {
				slice.Spec.Devices[index].Basic.Attributes = attribute
			},
			updateSliceCapacityFunc: func(slice *resourceapi.ResourceSlice, index int, capacity map[resourceapi.QualifiedName]resourceapi.DeviceCapacity) {
				slice.Spec.Devices[index].Basic.Capacity = capacity
			},
			baseFieldPath: field.NewPath("spec", "devices"),
			deviceType:    "basic",
		},
		"device-composite": {
			sliceCreateFunc: func(count int) *resourceapi.ResourceSlice {
				return testResourceSliceWithCompositeMixin(goodName, goodName, goodName, 1, count)
			},
			updateSliceAttributeFunc: func(slice *resourceapi.ResourceSlice, index int, attribute map[resourceapi.QualifiedName]resourceapi.DeviceAttribute) {
				slice.Spec.Devices[index].Composite.Attributes = attribute
			},
			updateSliceCapacityFunc: func(slice *resourceapi.ResourceSlice, index int, capacity map[resourceapi.QualifiedName]resourceapi.DeviceCapacity) {
				slice.Spec.Devices[index].Composite.Capacity = capacity
			},
			baseFieldPath: field.NewPath("spec", "devices"),
			deviceType:    "composite",
		},
		"device-mixin-composite": {
			sliceCreateFunc: func(count int) *resourceapi.ResourceSlice {
				return testResourceSliceWithCompositeMixin(goodName, goodName, goodName, count, 1)
			},
			updateSliceAttributeFunc: func(slice *resourceapi.ResourceSlice, index int, attribute map[resourceapi.QualifiedName]resourceapi.DeviceAttribute) {
				slice.Spec.DeviceMixins[index].Composite.Attributes = attribute
			},
			updateSliceCapacityFunc: func(slice *resourceapi.ResourceSlice, index int, capacity map[resourceapi.QualifiedName]resourceapi.DeviceCapacity) {
				slice.Spec.DeviceMixins[index].Composite.Capacity = capacity
			},
			baseFieldPath: field.NewPath("spec", "deviceMixins"),
			deviceType:    "composite",
		},
	}

	for fieldName, fieldConfig := range fieldConfigs {
		for scenario, scenarioConfig := range scenarios {
			t.Run(fmt.Sprintf("%s/%s", fieldName, scenario), func(t *testing.T) {
				slice := fieldConfig.sliceCreateFunc(scenarioConfig.deviceCount)
				for i, attribute := range scenarioConfig.attributes {
					fieldConfig.updateSliceAttributeFunc(slice, i, attribute)
				}
				for i, capacity := range scenarioConfig.capacities {
					fieldConfig.updateSliceCapacityFunc(slice, i, capacity)
				}
				errs := ValidateResourceSlice(slice)
				var expectedFailures field.ErrorList
				if scenarioConfig.wantFailuresFunc != nil {
					expectedFailures = scenarioConfig.wantFailuresFunc(fieldConfig.baseFieldPath, fieldConfig.deviceType)
				}
				assertFailures(t, expectedFailures, errs)
			})
		}
	}
}

func TestValidateResourceSliceUpdate(t *testing.T) {
	name := "valid"
	validResourceSlice := testResourceSlice(name, name, name, 1)

	scenarios := map[string]struct {
		oldResourceSlice *resourceapi.ResourceSlice
		update           func(slice *resourceapi.ResourceSlice) *resourceapi.ResourceSlice
		wantFailures     field.ErrorList
	}{
		"valid-no-op-update": {
			oldResourceSlice: validResourceSlice,
			update:           func(slice *resourceapi.ResourceSlice) *resourceapi.ResourceSlice { return slice },
		},
		"invalid-name-update": {
			oldResourceSlice: validResourceSlice,
			update: func(slice *resourceapi.ResourceSlice) *resourceapi.ResourceSlice {
				slice.Name += "-update"
				return slice
			},
			wantFailures: field.ErrorList{field.Invalid(field.NewPath("metadata", "name"), name+"-update", "field is immutable")},
		},
		"invalid-update-nodename": {
			wantFailures:     field.ErrorList{field.Invalid(field.NewPath("spec", "nodeName"), name+"-updated", "field is immutable")},
			oldResourceSlice: validResourceSlice,
			update: func(slice *resourceapi.ResourceSlice) *resourceapi.ResourceSlice {
				slice.Spec.NodeName += "-updated"
				return slice
			},
		},
		"invalid-update-drivername": {
			wantFailures:     field.ErrorList{field.Invalid(field.NewPath("spec", "driver"), name+"-updated", "field is immutable")},
			oldResourceSlice: validResourceSlice,
			update: func(slice *resourceapi.ResourceSlice) *resourceapi.ResourceSlice {
				slice.Spec.Driver += "-updated"
				return slice
			},
		},
		"invalid-update-pool": {
			wantFailures:     field.ErrorList{field.Invalid(field.NewPath("spec", "pool", "name"), validResourceSlice.Spec.Pool.Name+"-updated", "field is immutable")},
			oldResourceSlice: validResourceSlice,
			update: func(slice *resourceapi.ResourceSlice) *resourceapi.ResourceSlice {
				slice.Spec.Pool.Name += "-updated"
				return slice
			},
		},
		"invalid-update-to-invalid-nodeselector-label-value": {
			wantFailures: field.ErrorList{field.Invalid(field.NewPath("spec", "nodeSelector", "nodeSelectorTerms").Index(0).Child("matchExpressions").Index(0).Child("values").Index(0), "-1", "a valid label must be an empty string or consist of alphanumeric characters, '-', '_' or '.', and must start and end with an alphanumeric character (e.g. 'MyValue',  or 'my_value',  or '12345', regex used for validation is '(([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9])?')")},
			oldResourceSlice: func() *resourceapi.ResourceSlice {
				slice := validResourceSlice.DeepCopy()
				slice.Spec.NodeName = ""
				slice.Spec.NodeSelector = &core.NodeSelector{
					NodeSelectorTerms: []core.NodeSelectorTerm{{
						MatchExpressions: []core.NodeSelectorRequirement{{
							Key:      "foo",
							Operator: core.NodeSelectorOpIn,
							Values:   []string{"bar"},
						}},
					}},
				}
				return slice
			}(),
			update: func(slice *resourceapi.ResourceSlice) *resourceapi.ResourceSlice {
				slice.Spec.NodeSelector = &core.NodeSelector{
					NodeSelectorTerms: []core.NodeSelectorTerm{{
						MatchExpressions: []core.NodeSelectorRequirement{{
							Key:      "foo",
							Operator: core.NodeSelectorOpIn,
							Values:   []string{"-1"},
						}},
					}},
				}
				return slice
			},
		},
	}

	for name, scenario := range scenarios {
		t.Run(name, func(t *testing.T) {
			scenario.oldResourceSlice.ResourceVersion = "1"
			errs := ValidateResourceSliceUpdate(scenario.update(scenario.oldResourceSlice.DeepCopy()), scenario.oldResourceSlice)
			assertFailures(t, scenario.wantFailures, errs)
		})
	}
}
