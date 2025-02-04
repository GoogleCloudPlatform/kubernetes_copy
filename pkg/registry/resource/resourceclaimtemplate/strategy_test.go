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

package resourceclaimtemplate

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

var obj = &resource.ResourceClaimTemplate{
	ObjectMeta: metav1.ObjectMeta{
		Name:      "valid-claim-template",
		Namespace: "default",
	},
	Spec: resource.ResourceClaimTemplateSpec{
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
	},
}

var objWithAdminAccess = &resource.ResourceClaimTemplate{
	ObjectMeta: metav1.ObjectMeta{
		Name:      "valid-claim-template",
		Namespace: "default",
	},
	Spec: resource.ResourceClaimTemplateSpec{
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
	},
}

var objWithPrioritizedList = &resource.ResourceClaimTemplate{
	ObjectMeta: metav1.ObjectMeta{
		Name:      "valid-claim-template",
		Namespace: "default",
	},
	Spec: resource.ResourceClaimTemplateSpec{
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
	},
}

func TestClaimTemplateStrategy(t *testing.T) {
	if !Strategy.NamespaceScoped() {
		t.Errorf("ResourceClaimTemplate must be namespace scoped")
	}
	if Strategy.AllowCreateOnUpdate() {
		t.Errorf("ResourceClaimTemplate should not allow create on update")
	}
}

func TestClaimTemplateStrategyCreate(t *testing.T) {
	ctx := genericapirequest.NewDefaultContext()

	testcases := map[string]struct {
		obj                   *resource.ResourceClaimTemplate
		adminAccess           bool
		prioritizedList       bool
		expectValidationError bool
		expectObj             *resource.ResourceClaimTemplate
	}{
		"simple": {
			obj:       obj,
			expectObj: obj,
		},
		"validation-error": {
			obj: func() *resource.ResourceClaimTemplate {
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

func TestClaimTemplateStrategyUpdate(t *testing.T) {
	t.Run("no-changes-okay", func(t *testing.T) {
		ctx := genericapirequest.NewDefaultContext()
		resourceClaimTemplate := obj.DeepCopy()
		newClaimTemplate := resourceClaimTemplate.DeepCopy()
		newClaimTemplate.ResourceVersion = "4"

		Strategy.PrepareForUpdate(ctx, newClaimTemplate, resourceClaimTemplate)
		errs := Strategy.ValidateUpdate(ctx, newClaimTemplate, resourceClaimTemplate)
		if len(errs) != 0 {
			t.Errorf("unexpected validation errors: %v", errs)
		}
	})

	t.Run("name-change-not-allowed", func(t *testing.T) {
		ctx := genericapirequest.NewDefaultContext()
		resourceClaimTemplate := obj.DeepCopy()
		newClaimTemplate := resourceClaimTemplate.DeepCopy()
		newClaimTemplate.Name = "valid-class-2"
		newClaimTemplate.ResourceVersion = "4"

		Strategy.PrepareForUpdate(ctx, newClaimTemplate, resourceClaimTemplate)
		errs := Strategy.ValidateUpdate(ctx, newClaimTemplate, resourceClaimTemplate)
		if len(errs) == 0 {
			t.Errorf("expected a validation error")
		}
	})
}
