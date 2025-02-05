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
			slice: testResourceSlice(goodName, goodName, driverName, resourceapi.ResourceSliceMaxDevices),
		},
		"too-large": {
			wantFailures: field.ErrorList{field.TooMany(field.NewPath("spec", "devices"), resourceapi.ResourceSliceMaxDevices+1, resourceapi.ResourceSliceMaxDevices)},
			slice:        testResourceSlice(goodName, goodName, goodName, resourceapi.ResourceSliceMaxDevices+1),
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
			wantFailures: field.ErrorList{field.Invalid(field.NewPath("spec"), nil, "exactly one of `nodeName`, `nodeSelector`, or `allNodes` is required, but multiple fields are set: `nodeName`, `nodeSelector`")},
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
			wantFailures: field.ErrorList{field.Invalid(field.NewPath("spec"), nil, "exactly one of `nodeName`, `nodeSelector`, or `allNodes` is required, but multiple fields are set: `nodeName`, `allNodes`")},
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
				field.Required(field.NewPath("spec", "devices").Index(2).Child("basic"), ""),
			},
			slice: func() *resourceapi.ResourceSlice {
				slice := testResourceSlice(goodName, goodName, goodName, 3)
				slice.Spec.Devices[1].Name = badName
				slice.Spec.Devices[2].Basic = nil
				return slice
			}(),
		},
		"bad-attribute": {
			wantFailures: field.ErrorList{
				field.TypeInvalid(field.NewPath("spec", "devices").Index(1).Child("basic", "attributes").Key(badName), badName, "a valid C identifier must start with alphabetic character or '_', followed by a string of alphanumeric characters or '_' (e.g. 'my_name',  or 'MY_NAME',  or 'MyName', regex used for validation is '[A-Za-z_][A-Za-z0-9_]*')"),
				field.Required(field.NewPath("spec", "devices").Index(1).Child("basic", "attributes").Key(badName), "exactly one value must be specified"),
				field.Invalid(field.NewPath("spec", "devices").Index(2).Child("basic", "attributes").Key(goodName), resourceapi.DeviceAttribute{StringValue: ptr.To("x"), VersionValue: ptr.To("1.2.3")}, "exactly one value must be specified"),
				field.Invalid(field.NewPath("spec", "devices").Index(3).Child("basic", "attributes").Key(goodName).Child("version"), strings.Repeat("x", resourceapi.DeviceAttributeMaxValueLength+1), "must be a string compatible with semver.org spec 2.0.0"),
				field.TooLongMaxLength(field.NewPath("spec", "devices").Index(3).Child("basic", "attributes").Key(goodName).Child("version"), strings.Repeat("x", resourceapi.DeviceAttributeMaxValueLength+1), resourceapi.DeviceAttributeMaxValueLength),
				field.TooLongMaxLength(field.NewPath("spec", "devices").Index(4).Child("basic", "attributes").Key(goodName).Child("string"), strings.Repeat("x", resourceapi.DeviceAttributeMaxValueLength+1), resourceapi.DeviceAttributeMaxValueLength),
			},
			slice: func() *resourceapi.ResourceSlice {
				slice := testResourceSlice(goodName, goodName, goodName, 5)
				slice.Spec.Devices[1].Basic.Attributes = map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
					resourceapi.QualifiedName(badName): {},
				}
				slice.Spec.Devices[2].Basic.Attributes = map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
					resourceapi.QualifiedName(goodName): {StringValue: ptr.To("x"), VersionValue: ptr.To("1.2.3")},
				}
				slice.Spec.Devices[3].Basic.Attributes = map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
					resourceapi.QualifiedName(goodName): {VersionValue: ptr.To(strings.Repeat("x", resourceapi.DeviceAttributeMaxValueLength+1))},
				}
				slice.Spec.Devices[4].Basic.Attributes = map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
					resourceapi.QualifiedName(goodName): {StringValue: ptr.To(strings.Repeat("x", resourceapi.DeviceAttributeMaxValueLength+1))},
				}
				return slice
			}(),
		},
		"good-attribute-names": {
			slice: func() *resourceapi.ResourceSlice {
				slice := testResourceSlice(goodName, goodName, goodName, 2)
				slice.Spec.Devices[1].Basic.Attributes = map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
					resourceapi.QualifiedName(strings.Repeat("x", resourceapi.DeviceMaxIDLength)):                                                                {StringValue: ptr.To("y")},
					resourceapi.QualifiedName(strings.Repeat("x", resourceapi.DeviceMaxDomainLength) + "/" + strings.Repeat("y", resourceapi.DeviceMaxIDLength)): {StringValue: ptr.To("z")},
				}
				return slice
			}(),
		},
		"bad-attribute-c-identifier": {
			wantFailures: field.ErrorList{
				field.TooLongMaxLength(field.NewPath("spec", "devices").Index(1).Child("basic", "attributes").Key(strings.Repeat(".", resourceapi.DeviceMaxIDLength+1)), strings.Repeat(".", resourceapi.DeviceMaxIDLength+1), resourceapi.DeviceMaxIDLength),
				field.TypeInvalid(field.NewPath("spec", "devices").Index(1).Child("basic", "attributes").Key(strings.Repeat(".", resourceapi.DeviceMaxIDLength+1)), strings.Repeat(".", resourceapi.DeviceMaxIDLength+1), "a valid C identifier must start with alphabetic character or '_', followed by a string of alphanumeric characters or '_' (e.g. 'my_name',  or 'MY_NAME',  or 'MyName', regex used for validation is '[A-Za-z_][A-Za-z0-9_]*')"),
			},
			slice: func() *resourceapi.ResourceSlice {
				slice := testResourceSlice(goodName, goodName, goodName, 2)
				slice.Spec.Devices[1].Basic.Attributes = map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
					resourceapi.QualifiedName(strings.Repeat(".", resourceapi.DeviceMaxIDLength+1)): {StringValue: ptr.To("y")},
				}
				return slice
			}(),
		},
		"bad-attribute-domain": {
			wantFailures: field.ErrorList{
				field.TooLong(field.NewPath("spec", "devices").Index(1).Child("basic", "attributes").Key(strings.Repeat("_", resourceapi.DeviceMaxDomainLength+1)+"/y"), strings.Repeat("_", resourceapi.DeviceMaxDomainLength+1), resourceapi.DeviceMaxDomainLength),
				field.Invalid(field.NewPath("spec", "devices").Index(1).Child("basic", "attributes").Key(strings.Repeat("_", resourceapi.DeviceMaxDomainLength+1)+"/y"), strings.Repeat("_", resourceapi.DeviceMaxDomainLength+1), "a lowercase RFC 1123 subdomain must consist of lower case alphanumeric characters, '-' or '.', and must start and end with an alphanumeric character (e.g. 'example.com', regex used for validation is '[a-z0-9]([-a-z0-9]*[a-z0-9])?(\\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*')"),
			},
			slice: func() *resourceapi.ResourceSlice {
				slice := testResourceSlice(goodName, goodName, goodName, 2)
				slice.Spec.Devices[1].Basic.Attributes = map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
					resourceapi.QualifiedName(strings.Repeat("_", resourceapi.DeviceMaxDomainLength+1) + "/y"): {StringValue: ptr.To("z")},
				}
				return slice
			}(),
		},
		"bad-key-too-long": {
			wantFailures: field.ErrorList{
				field.TooLong(field.NewPath("spec", "devices").Index(1).Child("basic", "attributes").Key("xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx...xxxxxxxxxxxx/yyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyy"), strings.Repeat("x", resourceapi.DeviceMaxDomainLength+1), resourceapi.DeviceMaxDomainLength),
				field.TooLongMaxLength(field.NewPath("spec", "devices").Index(1).Child("basic", "attributes").Key("xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx...xxxxxxxxxxxx/yyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyy"), strings.Repeat("y", resourceapi.DeviceMaxIDLength+1), resourceapi.DeviceMaxIDLength),
			},
			slice: func() *resourceapi.ResourceSlice {
				slice := testResourceSlice(goodName, goodName, goodName, 2)
				slice.Spec.Devices[1].Basic.Attributes = map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
					resourceapi.QualifiedName(strings.Repeat("x", resourceapi.DeviceMaxDomainLength+1) + "/" + strings.Repeat("y", resourceapi.DeviceMaxIDLength+1)): {StringValue: ptr.To("z")},
				}
				return slice
			}(),
		},
		"bad-attribute-empty-domain-and-c-identifier": {
			wantFailures: field.ErrorList{
				field.Required(field.NewPath("spec", "devices").Index(1).Child("basic", "attributes").Key("/"), "the domain must not be empty"),
				field.Required(field.NewPath("spec", "devices").Index(1).Child("basic", "attributes").Key("/"), "the name must not be empty"),
			},
			slice: func() *resourceapi.ResourceSlice {
				slice := testResourceSlice(goodName, goodName, goodName, 2)
				slice.Spec.Devices[1].Basic.Attributes = map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
					resourceapi.QualifiedName("/"): {StringValue: ptr.To("z")},
				}
				return slice
			}(),
		},
		"combined-attributes-and-capacity-length": {
			wantFailures: field.ErrorList{
				field.Invalid(field.NewPath("spec", "devices").Index(2).Child("basic"), resourceapi.ResourceSliceMaxAttributesAndCapacitiesPerDevice+1, fmt.Sprintf("the total number of attributes and capacities must not exceed %d", resourceapi.ResourceSliceMaxAttributesAndCapacitiesPerDevice)),
			},
			slice: func() *resourceapi.ResourceSlice {
				slice := testResourceSlice(goodName, goodName, goodName, 3)
				slice.Spec.Devices[0].Basic.Attributes = map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{}
				slice.Spec.Devices[0].Basic.Capacity = map[resourceapi.QualifiedName]resourceapi.DeviceCapacity{}
				for i := 0; i < resourceapi.ResourceSliceMaxAttributesAndCapacitiesPerDevice; i++ {
					slice.Spec.Devices[0].Basic.Attributes[resourceapi.QualifiedName(fmt.Sprintf("attr_%d", i))] = resourceapi.DeviceAttribute{StringValue: ptr.To("x")}
				}
				slice.Spec.Devices[1].Basic.Attributes = map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{}
				slice.Spec.Devices[1].Basic.Capacity = map[resourceapi.QualifiedName]resourceapi.DeviceCapacity{}
				quantity := resource.MustParse("1Gi")
				capacity := resourceapi.DeviceCapacity{Value: quantity}
				for i := 0; i < resourceapi.ResourceSliceMaxAttributesAndCapacitiesPerDevice; i++ {
					slice.Spec.Devices[1].Basic.Capacity[resourceapi.QualifiedName(fmt.Sprintf("cap_%d", i))] = capacity
				}
				// Too large together by one.
				slice.Spec.Devices[2].Basic.Attributes = slice.Spec.Devices[0].Basic.Attributes
				slice.Spec.Devices[2].Basic.Capacity = map[resourceapi.QualifiedName]resourceapi.DeviceCapacity{
					"cap": capacity,
				}
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
	}

	for name, scenario := range scenarios {
		t.Run(name, func(t *testing.T) {
			errs := ValidateResourceSlice(scenario.slice)
			assertFailures(t, scenario.wantFailures, errs)
		})
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
