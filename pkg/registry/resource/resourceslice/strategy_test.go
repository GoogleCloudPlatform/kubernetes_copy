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

package resourceslice

import (
	"testing"

	"github.com/stretchr/testify/assert"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	genericapirequest "k8s.io/apiserver/pkg/endpoints/request"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	"k8s.io/kubernetes/pkg/apis/resource"
	"k8s.io/kubernetes/pkg/features"
)

var slice = &resource.ResourceSlice{
	ObjectMeta: metav1.ObjectMeta{
		Name: "valid-class",
	},
	Spec: resource.ResourceSliceSpec{
		NodeName: "valid-node-name",
		Driver:   "testdriver.example.com",
		Pool: resource.ResourcePool{
			Name:               "valid-pool-name",
			ResourceSliceCount: 1,
		},
	},
}

var sliceWithPartitionableDevices = &resource.ResourceSlice{
	ObjectMeta: metav1.ObjectMeta{
		Name: "valid-with-partitionable-devices",
	},
	Spec: resource.ResourceSliceSpec{
		NodeName: "valid-node-name",
		Driver:   "testdriver.example.com",
		Pool: resource.ResourcePool{
			Name:               "valid-pool-name",
			ResourceSliceCount: 1,
		},
		DeviceMixins: []resource.DeviceMixin{
			{
				Name:      "mixin",
				Composite: &resource.CompositeDeviceMixin{},
			},
		},
		Devices: []resource.Device{
			{
				Name: "device",
				Composite: &resource.CompositeDevice{
					Includes: []resource.DeviceMixinRef{
						{
							Name: "mixin",
						},
					},
				},
			},
		},
	},
}

func TestResourceSliceStrategy(t *testing.T) {
	if Strategy.NamespaceScoped() {
		t.Errorf("ResourceSlice must not be namespace scoped")
	}
	if Strategy.AllowCreateOnUpdate() {
		t.Errorf("ResourceSlice should not allow create on update")
	}
}

func TestResourceSliceStrategyCreate(t *testing.T) {
	ctx := genericapirequest.NewDefaultContext()
	testcases := map[string]struct {
		obj                     *resource.ResourceSlice
		partitionableDevices    bool
		expectedValidationError bool
		expectObj               *resource.ResourceSlice
	}{
		"simple": {
			obj: slice,
			expectObj: func() *resource.ResourceSlice {
				obj := slice.DeepCopy()
				obj.ObjectMeta.Generation = 1
				return obj
			}(),
		},
		"validation error": {
			obj: func() *resource.ResourceSlice {
				obj := slice.DeepCopy()
				obj.Name = "%#@$%$"
				return obj
			}(),
			expectedValidationError: true,
		},
		"drop-fields-partitionable-devices-with-mixin": {
			obj: func() *resource.ResourceSlice {
				obj := sliceWithPartitionableDevices.DeepCopy()
				obj.Spec.Devices = []resource.Device{
					{
						Name:  "basic-device",
						Basic: &resource.BasicDevice{},
					},
				}
				return obj
			}(),
			partitionableDevices: false,
			expectObj: func() *resource.ResourceSlice {
				obj := sliceWithPartitionableDevices.DeepCopy()
				obj.ObjectMeta.Generation = 1
				obj.Spec.DeviceMixins = nil
				obj.Spec.Devices = []resource.Device{
					{
						Name:  "basic-device",
						Basic: &resource.BasicDevice{},
					},
				}
				return obj
			}(),
		},
		"drop-fields-partitionable-devices-with-composite-device": {
			obj: func() *resource.ResourceSlice {
				obj := sliceWithPartitionableDevices.DeepCopy()
				obj.Spec.DeviceMixins = nil
				obj.Spec.Devices[0].Composite.Includes = []resource.DeviceMixinRef{}
				return obj
			}(),
			partitionableDevices:    false,
			expectedValidationError: true,
		},
		"keep-fields-partitionable-devices": {
			obj:                  sliceWithPartitionableDevices,
			partitionableDevices: true,
			expectObj: func() *resource.ResourceSlice {
				obj := sliceWithPartitionableDevices.DeepCopy()
				obj.ObjectMeta.Generation = 1
				return obj
			}(),
		},
	}

	for name, tc := range testcases {
		t.Run(name, func(t *testing.T) {
			featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.DRAPartitionableDevices, tc.partitionableDevices)

			obj := tc.obj.DeepCopy()

			Strategy.PrepareForCreate(ctx, obj)
			if errs := Strategy.Validate(ctx, obj); len(errs) != 0 {
				if !tc.expectedValidationError {
					t.Fatalf("unexpected validation errors: %q", errs)
				}
				return
			}
			if warnings := Strategy.WarningsOnCreate(ctx, obj); len(warnings) != 0 {
				t.Fatalf("unexpected warnings: %q", warnings)
			}
			Strategy.Canonicalize(obj)
			assert.Equal(t, tc.expectObj, obj)
		})
	}

}

func TestResourceSliceStrategyUpdate(t *testing.T) {
	ctx := genericapirequest.NewDefaultContext()

	testcases := map[string]struct {
		oldObj                *resource.ResourceSlice
		newObj                *resource.ResourceSlice
		partitionableDevices  bool
		expectValidationError bool
		expectObj             *resource.ResourceSlice
	}{
		"no-changes-okay": {
			oldObj: slice,
			newObj: func() *resource.ResourceSlice {
				obj := slice.DeepCopy()
				obj.ResourceVersion = "4"
				return obj
			}(),
			expectObj: func() *resource.ResourceSlice {
				obj := slice.DeepCopy()
				obj.ResourceVersion = "4"
				return obj
			}(),
		},
		"name-change-not-allowed": {
			oldObj: slice,
			newObj: func() *resource.ResourceSlice {
				obj := slice.DeepCopy()
				obj.Name = "valid-slice-2"
				obj.ResourceVersion = "4"
				return obj
			}(),
			expectValidationError: true,
		},
		"drop-fields-partitionable-devices": {
			oldObj: slice,
			newObj: func() *resource.ResourceSlice {
				obj := sliceWithPartitionableDevices.DeepCopy()
				obj.ResourceVersion = "4"
				return obj
			}(),
			partitionableDevices:  false,
			expectValidationError: true,
		},
		"keep-fields-partitionable-devices": {
			oldObj: slice,
			newObj: func() *resource.ResourceSlice {
				obj := sliceWithPartitionableDevices.DeepCopy()
				obj.ResourceVersion = "4"
				return obj
			}(),
			partitionableDevices:  true,
			expectValidationError: true,
		},
		"keep-existing-fields-partitionable-devices": {
			oldObj: sliceWithPartitionableDevices,
			newObj: func() *resource.ResourceSlice {
				obj := sliceWithPartitionableDevices.DeepCopy()
				obj.ResourceVersion = "4"
				return obj
			}(),
			partitionableDevices: true,
			expectObj: func() *resource.ResourceSlice {
				obj := sliceWithPartitionableDevices.DeepCopy()
				obj.ResourceVersion = "4"
				return obj
			}(),
		},
		"keep-existing-fields-partitionable-devices-disabled-feature": {
			oldObj: sliceWithPartitionableDevices,
			newObj: func() *resource.ResourceSlice {
				obj := sliceWithPartitionableDevices.DeepCopy()
				obj.ResourceVersion = "4"
				return obj
			}(),
			partitionableDevices: false,
			expectObj: func() *resource.ResourceSlice {
				obj := sliceWithPartitionableDevices.DeepCopy()
				obj.ResourceVersion = "4"
				return obj
			}(),
		},
	}

	for name, tc := range testcases {
		t.Run(name, func(t *testing.T) {
			featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.DRAPartitionableDevices, tc.partitionableDevices)

			oldObj := tc.oldObj.DeepCopy()
			newObj := tc.newObj.DeepCopy()

			Strategy.PrepareForUpdate(ctx, newObj, oldObj)
			if errs := Strategy.ValidateUpdate(ctx, newObj, oldObj); len(errs) != 0 {
				if !tc.expectValidationError {
					t.Fatalf("unexpected validation errors: %q", errs)
				}
				return
			} else if tc.expectValidationError {
				t.Fatal("expected validation error(s), got none")
			}
			if warnings := Strategy.WarningsOnUpdate(ctx, newObj, oldObj); len(warnings) != 0 {
				t.Fatalf("unexpected warnings: %q", warnings)
			}
			Strategy.Canonicalize(newObj)

			expectObj := tc.expectObj.DeepCopy()
			assert.Equal(t, expectObj, newObj)

		})
	}
}
