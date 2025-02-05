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

package resourceclaim

import (
	"testing"

	"github.com/stretchr/testify/assert"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	genericapirequest "k8s.io/apiserver/pkg/endpoints/request"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	"k8s.io/kubernetes/pkg/apis/resource"
	"k8s.io/kubernetes/pkg/features"
	"k8s.io/utils/ptr"
)

var obj = &resource.ResourceClaim{
	ObjectMeta: metav1.ObjectMeta{
		Name:      "valid-claim",
		Namespace: "default",
	},
	Spec: resource.ResourceClaimSpec{
		Devices: resource.DeviceClaim{
			Requests: []resource.DeviceRequest{
				{
					Name:            "req-0",
					DeviceClassName: "class",
					AllocationMode:  resource.DeviceAllocationModeAll,
				},
			},
		},
	},
}

var objWithStatus = &resource.ResourceClaim{
	ObjectMeta: metav1.ObjectMeta{
		Name:      "valid-claim",
		Namespace: "default",
	},
	Spec: resource.ResourceClaimSpec{
		Devices: resource.DeviceClaim{
			Requests: []resource.DeviceRequest{
				{
					Name:            "req-0",
					DeviceClassName: "class",
					AllocationMode:  resource.DeviceAllocationModeAll,
				},
			},
		},
	},
	Status: resource.ResourceClaimStatus{
		Allocation: &resource.AllocationResult{
			Devices: resource.DeviceAllocationResult{
				Results: []resource.DeviceRequestAllocationResult{
					{
						Request: "req-0",
						Driver:  "dra.example.com",
						Pool:    "pool-0",
						Device:  "device-0",
					},
				},
			},
		},
	},
}

var objWithAdminAccess = &resource.ResourceClaim{
	ObjectMeta: metav1.ObjectMeta{
		Name:      "valid-claim",
		Namespace: "default",
	},
	Spec: resource.ResourceClaimSpec{
		Devices: resource.DeviceClaim{
			Requests: []resource.DeviceRequest{
				{
					Name:            "req-0",
					DeviceClassName: "class",
					AllocationMode:  resource.DeviceAllocationModeAll,
					AdminAccess:     ptr.To(true),
				},
			},
		},
	},
}

var objWithAdminAccessStatus = &resource.ResourceClaim{
	ObjectMeta: metav1.ObjectMeta{
		Name:      "valid-claim",
		Namespace: "default",
	},
	Spec: resource.ResourceClaimSpec{
		Devices: resource.DeviceClaim{
			Requests: []resource.DeviceRequest{
				{
					Name:            "req-0",
					DeviceClassName: "class",
					AllocationMode:  resource.DeviceAllocationModeAll,
					AdminAccess:     ptr.To(true),
				},
			},
		},
	},
	Status: resource.ResourceClaimStatus{
		Allocation: &resource.AllocationResult{
			Devices: resource.DeviceAllocationResult{
				Results: []resource.DeviceRequestAllocationResult{
					{
						Request:     "req-0",
						Driver:      "dra.example.com",
						Pool:        "pool-0",
						Device:      "device-0",
						AdminAccess: ptr.To(true),
					},
				},
			},
		},
	},
}

var objWithPrioritizedList = &resource.ResourceClaim{
	ObjectMeta: metav1.ObjectMeta{
		Name:      "valid-claim",
		Namespace: "default",
	},
	Spec: resource.ResourceClaimSpec{
		Devices: resource.DeviceClaim{
			Requests: []resource.DeviceRequest{
				{
					Name: "req-0",
					FirstAvailable: []resource.DeviceSubRequest{
						{
							Name:            "subreq-0",
							DeviceClassName: "class",
							AllocationMode:  resource.DeviceAllocationModeExactCount,
							Count:           1,
						},
					},
				},
			},
		},
	},
}

const (
	testRequest = "test-request"
	testDriver  = "test-driver"
	testPool    = "test-pool"
	testDevice  = "test-device"
)

func TestStrategy(t *testing.T) {
	if !Strategy.NamespaceScoped() {
		t.Errorf("ResourceClaim must be namespace scoped")
	}
	if Strategy.AllowCreateOnUpdate() {
		t.Errorf("ResourceClaim should not allow create on update")
	}
}

func TestStrategyCreate(t *testing.T) {
	ctx := genericapirequest.NewDefaultContext()

	testcases := map[string]struct {
		obj                   *resource.ResourceClaim
		adminAccess           bool
		prioritizedList       bool
		expectValidationError bool
		expectObj             *resource.ResourceClaim
	}{
		"simple": {
			obj:       obj,
			expectObj: obj,
		},
		"validation-error": {
			obj: func() *resource.ResourceClaim {
				obj := obj.DeepCopy()
				obj.Name = "%#@$%$"
				return obj
			}(),
			expectValidationError: true,
		},
		"drop-fields-admin-access": {
			obj:         objWithAdminAccess,
			adminAccess: false,
			expectObj:   obj,
		},
		"keep-fields-admin-access": {
			obj:         objWithAdminAccess,
			adminAccess: true,
			expectObj:   objWithAdminAccess,
		},
		"drop-fields-prioritized-list": {
			obj:                   objWithPrioritizedList,
			prioritizedList:       false,
			expectValidationError: true,
		},
		"keep-fields-prioritized-list": {
			obj:             objWithPrioritizedList,
			prioritizedList: true,
			expectObj:       objWithPrioritizedList,
		},
	}

	for name, tc := range testcases {
		t.Run(name, func(t *testing.T) {
			featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.DRAAdminAccess, tc.adminAccess)
			featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.DRAPrioritizedList, tc.prioritizedList)

			obj := tc.obj.DeepCopy()
			Strategy.PrepareForCreate(ctx, obj)
			if errs := Strategy.Validate(ctx, obj); len(errs) != 0 {
				if !tc.expectValidationError {
					t.Fatalf("unexpected validation errors: %q", errs)
				}
				return
			} else if tc.expectValidationError {
				t.Fatal("expected validation error(s), got none")
			}
			if warnings := Strategy.WarningsOnCreate(ctx, obj); len(warnings) != 0 {
				t.Fatalf("unexpected warnings: %q", warnings)
			}
			Strategy.Canonicalize(obj)
			assert.Equal(t, tc.expectObj, obj)
		})
	}
}

func TestStrategyUpdate(t *testing.T) {
	ctx := genericapirequest.NewDefaultContext()

	testcases := map[string]struct {
		oldObj                *resource.ResourceClaim
		newObj                *resource.ResourceClaim
		adminAccess           bool
		prioritizedList       bool
		expectValidationError bool
		expectObj             *resource.ResourceClaim
	}{
		"no-changes-okay": {
			oldObj:    obj,
			newObj:    obj,
			expectObj: obj,
		},
		"name-change-not-allowed": {
			oldObj: obj,
			newObj: func() *resource.ResourceClaim {
				obj := obj.DeepCopy()
				obj.Name += "-2"
				return obj
			}(),
			expectValidationError: true,
		},
		"drop-fields-admin-access": {
			oldObj:      obj,
			newObj:      objWithAdminAccess,
			adminAccess: false,
			expectObj:   obj,
		},
		"keep-fields-admin-access": {
			oldObj:                obj,
			newObj:                objWithAdminAccess,
			adminAccess:           true,
			expectValidationError: true, // Spec is immutable.
		},
		"keep-existing-fields-admin-access": {
			oldObj:      objWithAdminAccess,
			newObj:      objWithAdminAccess,
			adminAccess: true,
			expectObj:   objWithAdminAccess,
		},
		"drop-fields-prioritized-list": {
			oldObj:                obj,
			newObj:                objWithPrioritizedList,
			prioritizedList:       false,
			expectValidationError: true,
		},
		"keep-fields-prioritized-list": {
			oldObj:                obj,
			newObj:                objWithPrioritizedList,
			prioritizedList:       true,
			expectValidationError: true, // Spec is immutable.
		},
		"keep-existing-fields-prioritized-list": {
			oldObj:          objWithPrioritizedList,
			newObj:          objWithPrioritizedList,
			prioritizedList: true,
			expectObj:       objWithPrioritizedList,
		},
		"keep-existing-fields-prioritized-list-disabled-feature": {
			oldObj:          objWithPrioritizedList,
			newObj:          objWithPrioritizedList,
			prioritizedList: false,
			expectObj:       objWithPrioritizedList,
		},
	}

	for name, tc := range testcases {
		t.Run(name, func(t *testing.T) {
			featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.DRAAdminAccess, tc.adminAccess)
			featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.DRAPrioritizedList, tc.prioritizedList)

			oldObj := tc.oldObj.DeepCopy()
			newObj := tc.newObj.DeepCopy()
			newObj.ResourceVersion = "4"

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
			expectObj.ResourceVersion = "4"
			assert.Equal(t, expectObj, newObj)
		})
	}
}

func TestStatusStrategyUpdate(t *testing.T) {
	ctx := genericapirequest.NewDefaultContext()

	testcases := map[string]struct {
		oldObj                  *resource.ResourceClaim
		newObj                  *resource.ResourceClaim
		adminAccess             bool
		deviceStatusFeatureGate bool
		expectValidationError   bool
		expectObj               *resource.ResourceClaim
	}{
		"no-changes-okay": {
			oldObj:    obj,
			newObj:    obj,
			expectObj: obj,
		},
		"name-change-not-allowed": {
			oldObj: obj,
			newObj: func() *resource.ResourceClaim {
				obj := obj.DeepCopy()
				obj.Name += "-2"
				return obj
			}(),
			expectValidationError: true,
		},
		// Cannot add finalizers, annotations and labels during status update.
		"drop-meta-changes": {
			oldObj: obj,
			newObj: func() *resource.ResourceClaim {
				obj := obj.DeepCopy()
				obj.Finalizers = []string{"foo"}
				obj.Annotations = map[string]string{"foo": "bar"}
				obj.Labels = map[string]string{"foo": "bar"}
				return obj
			}(),
			expectObj: obj,
		},
		"drop-fields-admin-access": {
			oldObj:      obj,
			newObj:      objWithAdminAccessStatus,
			adminAccess: false,
			expectObj:   objWithStatus,
		},
		"keep-fields-admin-access": {
			oldObj:      obj,
			newObj:      objWithAdminAccessStatus,
			adminAccess: true,
			expectObj: func() *resource.ResourceClaim {
				expectObj := objWithAdminAccessStatus.DeepCopy()
				// Spec remains unchanged.
				expectObj.Spec = obj.Spec
				return expectObj
			}(),
		},
		"keep-fields-admin-access-because-of-spec": {
			oldObj:      objWithAdminAccess,
			newObj:      objWithAdminAccessStatus,
			adminAccess: false,
			expectObj:   objWithAdminAccessStatus,
		},
		// Normally a claim without admin access in the spec shouldn't
		// have one in the status either, but it's not invalid and thus
		// let's test this.
		"keep-fields-admin-access-because-of-status": {
			oldObj: func() *resource.ResourceClaim {
				oldObj := objWithAdminAccessStatus.DeepCopy()
				oldObj.Spec.Devices.Requests[0].AdminAccess = ptr.To(false)
				return oldObj
			}(),
			newObj:      objWithAdminAccessStatus,
			adminAccess: false,
			expectObj: func() *resource.ResourceClaim {
				oldObj := objWithAdminAccessStatus.DeepCopy()
				oldObj.Spec.Devices.Requests[0].AdminAccess = ptr.To(false)
				return oldObj
			}(),
		},
		"drop-fields-devices-status": {
			oldObj: func() *resource.ResourceClaim {
				obj := obj.DeepCopy()
				addSpecDevicesRequest(obj, testRequest)
				addStatusAllocationDevicesResults(obj, testDriver, testPool, testDevice, testRequest)
				return obj
			}(),
			newObj: func() *resource.ResourceClaim { // Status is added
				obj := obj.DeepCopy()
				addSpecDevicesRequest(obj, testRequest)
				addStatusAllocationDevicesResults(obj, testDriver, testPool, testDevice, testRequest)
				addStatusDevices(obj, testDriver, testPool, testDevice)
				return obj
			}(),
			deviceStatusFeatureGate: false,
			expectObj: func() *resource.ResourceClaim { // Status is no longer there
				obj := obj.DeepCopy()
				addSpecDevicesRequest(obj, testRequest)
				addStatusAllocationDevicesResults(obj, testDriver, testPool, testDevice, testRequest)
				return obj
			}(),
		},
		"keep-fields-devices-status-disable-feature-gate": {
			oldObj: func() *resource.ResourceClaim {
				obj := obj.DeepCopy()
				addSpecDevicesRequest(obj, testRequest)
				addStatusAllocationDevicesResults(obj, testDriver, testPool, testDevice, testRequest)
				addStatusDevices(obj, testDriver, testPool, testDevice)
				return obj
			}(),
			newObj: func() *resource.ResourceClaim { // Status is added
				obj := obj.DeepCopy()
				addSpecDevicesRequest(obj, testRequest)
				addStatusAllocationDevicesResults(obj, testDriver, testPool, testDevice, testRequest)
				addStatusDevices(obj, testDriver, testPool, testDevice)
				return obj
			}(),
			deviceStatusFeatureGate: false,
			expectObj: func() *resource.ResourceClaim { // Status is still there (as the status was set in the old object)
				obj := obj.DeepCopy()
				addSpecDevicesRequest(obj, testRequest)
				addStatusAllocationDevicesResults(obj, testDriver, testPool, testDevice, testRequest)
				addStatusDevices(obj, testDriver, testPool, testDevice)
				return obj
			}(),
		},
		"keep-fields-devices-status": {
			oldObj: func() *resource.ResourceClaim {
				obj := obj.DeepCopy()
				addSpecDevicesRequest(obj, testRequest)
				addStatusAllocationDevicesResults(obj, testDriver, testPool, testDevice, testRequest)
				return obj
			}(),
			newObj: func() *resource.ResourceClaim { // Status is added
				obj := obj.DeepCopy()
				addSpecDevicesRequest(obj, testRequest)
				addStatusAllocationDevicesResults(obj, testDriver, testPool, testDevice, testRequest)
				addStatusDevices(obj, testDriver, testPool, testDevice)
				return obj
			}(),
			deviceStatusFeatureGate: true,
			expectObj: func() *resource.ResourceClaim { // Status is still there
				obj := obj.DeepCopy()
				addSpecDevicesRequest(obj, testRequest)
				addStatusAllocationDevicesResults(obj, testDriver, testPool, testDevice, testRequest)
				addStatusDevices(obj, testDriver, testPool, testDevice)
				return obj
			}(),
		},
		"drop-status-deallocated-device": {
			oldObj: func() *resource.ResourceClaim {
				obj := obj.DeepCopy()
				addSpecDevicesRequest(obj, testRequest)
				addStatusAllocationDevicesResults(obj, testDriver, testPool, testDevice, testRequest)
				addStatusDevices(obj, testDriver, testPool, testDevice)
				return obj
			}(),
			newObj: func() *resource.ResourceClaim { // device is deallocated
				obj := obj.DeepCopy()
				addSpecDevicesRequest(obj, testRequest)
				addStatusDevices(obj, testDriver, testPool, testDevice)
				return obj
			}(),
			deviceStatusFeatureGate: true,
			expectObj: func() *resource.ResourceClaim { // Status is no longer there
				obj := obj.DeepCopy()
				addSpecDevicesRequest(obj, testRequest)
				return obj
			}(),
		},
		"drop-status-deallocated-device-disable-feature-gate": {
			oldObj: func() *resource.ResourceClaim {
				obj := obj.DeepCopy()
				addSpecDevicesRequest(obj, testRequest)
				addStatusAllocationDevicesResults(obj, testDriver, testPool, testDevice, testRequest)
				addStatusDevices(obj, testDriver, testPool, testDevice)
				return obj
			}(),
			newObj: func() *resource.ResourceClaim { // device is deallocated
				obj := obj.DeepCopy()
				addSpecDevicesRequest(obj, testRequest)
				addStatusDevices(obj, testDriver, testPool, testDevice)
				return obj
			}(),
			deviceStatusFeatureGate: false,
			expectObj: func() *resource.ResourceClaim { // Status is no longer there
				obj := obj.DeepCopy()
				addSpecDevicesRequest(obj, testRequest)
				return obj
			}(),
		},
	}

	for name, tc := range testcases {
		t.Run(name, func(t *testing.T) {
			featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.DRAAdminAccess, tc.adminAccess)
			featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.DRAResourceClaimDeviceStatus, tc.deviceStatusFeatureGate)

			oldObj := tc.oldObj.DeepCopy()
			newObj := tc.newObj.DeepCopy()
			newObj.ResourceVersion = "4"

			StatusStrategy.PrepareForUpdate(ctx, newObj, oldObj)
			if errs := StatusStrategy.ValidateUpdate(ctx, newObj, oldObj); len(errs) != 0 {
				if !tc.expectValidationError {
					t.Fatalf("unexpected validation errors: %q", errs)
				}
				return
			} else if tc.expectValidationError {
				t.Fatal("expected validation error(s), got none")
			}
			if warnings := StatusStrategy.WarningsOnUpdate(ctx, newObj, oldObj); len(warnings) != 0 {
				t.Fatalf("unexpected warnings: %q", warnings)
			}
			StatusStrategy.Canonicalize(newObj)

			expectObj := tc.expectObj.DeepCopy()
			expectObj.ResourceVersion = "4"
			assert.Equal(t, expectObj, newObj)
		})
	}
}

func addSpecDevicesRequest(resourceClaim *resource.ResourceClaim, request string) {
	resourceClaim.Spec.Devices.Requests = append(resourceClaim.Spec.Devices.Requests, resource.DeviceRequest{
		Name: request,
	})
}

func addStatusAllocationDevicesResults(resourceClaim *resource.ResourceClaim, driver string, pool string, device string, request string) {
	if resourceClaim.Status.Allocation == nil {
		resourceClaim.Status.Allocation = &resource.AllocationResult{}
	}
	resourceClaim.Status.Allocation.Devices.Results = append(resourceClaim.Status.Allocation.Devices.Results, resource.DeviceRequestAllocationResult{
		Request: request,
		Driver:  driver,
		Pool:    pool,
		Device:  device,
	})
}

func addStatusDevices(resourceClaim *resource.ResourceClaim, driver string, pool string, device string) {
	resourceClaim.Status.Devices = append(resourceClaim.Status.Devices, resource.AllocatedDeviceStatus{
		Driver: driver,
		Pool:   pool,
		Device: device,
	})
}
