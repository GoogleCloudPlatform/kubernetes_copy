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

package v1beta1_test

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"

	v1beta1 "k8s.io/api/resource/v1beta1"
	"k8s.io/apimachinery/pkg/runtime"

	// ensure types are installed
	"k8s.io/kubernetes/pkg/api/legacyscheme"
	_ "k8s.io/kubernetes/pkg/apis/resource/install"
)

func TestSetDefaultAllocationMode(t *testing.T) {
	claim := &v1beta1.ResourceClaim{
		Spec: v1beta1.ResourceClaimSpec{
			Devices: v1beta1.DeviceClaim{
				Requests: []v1beta1.DeviceRequest{{}},
			},
		},
	}

	// fields should be defaulted
	defaultMode := v1beta1.DeviceAllocationModeExactCount
	defaultCount := int64(1)
	output := roundTrip(t, runtime.Object(claim)).(*v1beta1.ResourceClaim)
	assert.Equal(t, defaultMode, output.Spec.Devices.Requests[0].AllocationMode)
	assert.Equal(t, defaultCount, output.Spec.Devices.Requests[0].Count)

	// field should not change
	nonDefaultMode := v1beta1.DeviceAllocationModeExactCount
	nonDefaultCount := int64(10)
	claim = &v1beta1.ResourceClaim{
		Spec: v1beta1.ResourceClaimSpec{
			Devices: v1beta1.DeviceClaim{
				Requests: []v1beta1.DeviceRequest{{
					AllocationMode: nonDefaultMode,
					Count:          nonDefaultCount,
				}},
			},
		},
	}
	output = roundTrip(t, runtime.Object(claim)).(*v1beta1.ResourceClaim)
	assert.Equal(t, nonDefaultMode, output.Spec.Devices.Requests[0].AllocationMode)
	assert.Equal(t, nonDefaultCount, output.Spec.Devices.Requests[0].Count)
}

func TestSetDefaultAllocationModeWithSubRequests(t *testing.T) {
	claim := &v1beta1.ResourceClaim{
		Spec: v1beta1.ResourceClaimSpec{
			Devices: v1beta1.DeviceClaim{
				Requests: []v1beta1.DeviceRequest{
					{
						Name: "req-1",
						FirstAvailable: []v1beta1.DeviceSubRequest{
							{
								Name: "subReq-1",
							},
							{
								Name: "subReq-2",
							},
						},
					},
				},
			},
		},
	}

	nilValueMode := v1beta1.DeviceAllocationMode("")
	nilValueCount := int64(0)
	defaultMode := v1beta1.DeviceAllocationModeExactCount
	defaultCount := int64(1)
	output := roundTrip(t, runtime.Object(claim)).(*v1beta1.ResourceClaim)
	// fields on the top-level DeviceRequest should not change
	assert.Equal(t, nilValueMode, output.Spec.Devices.Requests[0].AllocationMode)
	assert.Equal(t, nilValueCount, output.Spec.Devices.Requests[0].Count)
	// fields on the subRequests should be defaulted.
	assert.Equal(t, defaultMode, output.Spec.Devices.Requests[0].FirstAvailable[0].AllocationMode)
	assert.Equal(t, defaultCount, output.Spec.Devices.Requests[0].FirstAvailable[0].Count)
	assert.Equal(t, defaultMode, output.Spec.Devices.Requests[0].FirstAvailable[1].AllocationMode)
	assert.Equal(t, defaultCount, output.Spec.Devices.Requests[0].FirstAvailable[1].Count)

	// field should not change
	nonDefaultMode := v1beta1.DeviceAllocationModeExactCount
	nonDefaultCount := int64(10)
	claim = &v1beta1.ResourceClaim{
		Spec: v1beta1.ResourceClaimSpec{
			Devices: v1beta1.DeviceClaim{
				Requests: []v1beta1.DeviceRequest{{
					Name: "req-1",
					FirstAvailable: []v1beta1.DeviceSubRequest{
						{
							Name:           "subReq-1",
							AllocationMode: nonDefaultMode,
							Count:          nonDefaultCount,
						},
						{
							Name:           "subReq-2",
							AllocationMode: nonDefaultMode,
							Count:          nonDefaultCount,
						},
					},
				}},
			},
		},
	}
	output = roundTrip(t, runtime.Object(claim)).(*v1beta1.ResourceClaim)
	assert.Equal(t, nonDefaultMode, output.Spec.Devices.Requests[0].FirstAvailable[0].AllocationMode)
	assert.Equal(t, nonDefaultCount, output.Spec.Devices.Requests[0].FirstAvailable[0].Count)
	assert.Equal(t, nonDefaultMode, output.Spec.Devices.Requests[0].FirstAvailable[1].AllocationMode)
	assert.Equal(t, nonDefaultCount, output.Spec.Devices.Requests[0].FirstAvailable[1].Count)
}

func roundTrip(t *testing.T, obj runtime.Object) runtime.Object {
	codec := legacyscheme.Codecs.LegacyCodec(v1beta1.SchemeGroupVersion)
	data, err := runtime.Encode(codec, obj)
	if err != nil {
		t.Errorf("%v\n %#v", err, obj)
		return nil
	}
	obj2, err := runtime.Decode(codec, data)
	if err != nil {
		t.Errorf("%v\nData: %s\nSource: %#v", err, string(data), obj)
		return nil
	}
	obj3 := reflect.New(reflect.TypeOf(obj).Elem()).Interface().(runtime.Object)
	err = legacyscheme.Scheme.Convert(obj2, obj3, nil)
	if err != nil {
		t.Errorf("%v\nSource: %#v", err, obj2)
		return nil
	}
	return obj3
}
