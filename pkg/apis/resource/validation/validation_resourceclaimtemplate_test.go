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
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/kubernetes/pkg/apis/resource"
	"k8s.io/utils/pointer"
)

func testClaimTemplate(name, namespace string, spec resource.ResourceClaimSpec) *resource.ResourceClaimTemplate {
	return &resource.ResourceClaimTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: resource.ResourceClaimTemplateSpec{
			Spec: *spec.DeepCopy(),
		},
	}
}

func TestValidateClaimTemplate(t *testing.T) {
	now := metav1.Now()
	badValue := "spaces not allowed"

	scenarios := map[string]struct {
		template     *resource.ResourceClaimTemplate
		wantFailures field.ErrorList
	}{
		"good-claim": {
			template: testClaimTemplate(goodName, goodNS, validClaimSpec),
		},
		"missing-name": {
			wantFailures: field.ErrorList{field.Required(field.NewPath("metadata", "name"), "name or generateName is required")},
			template:     testClaimTemplate("", goodNS, validClaimSpec),
		},
		"bad-name": {
			wantFailures: field.ErrorList{field.Invalid(field.NewPath("metadata", "name"), badName, "a lowercase RFC 1123 subdomain must consist of lower case alphanumeric characters, '-' or '.', and must start and end with an alphanumeric character (e.g. 'example.com', regex used for validation is '[a-z0-9]([-a-z0-9]*[a-z0-9])?(\\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*')")},
			template:     testClaimTemplate(badName, goodNS, validClaimSpec),
		},
		"missing-namespace": {
			wantFailures: field.ErrorList{field.Required(field.NewPath("metadata", "namespace"), "")},
			template:     testClaimTemplate(goodName, "", validClaimSpec),
		},
		"generate-name": {
			template: func() *resource.ResourceClaimTemplate {
				template := testClaimTemplate(goodName, goodNS, validClaimSpec)
				template.GenerateName = "pvc-"
				return template
			}(),
		},
		"uid": {
			template: func() *resource.ResourceClaimTemplate {
				template := testClaimTemplate(goodName, goodNS, validClaimSpec)
				template.UID = "ac051fac-2ead-46d9-b8b4-4e0fbeb7455d"
				return template
			}(),
		},
		"resource-version": {
			template: func() *resource.ResourceClaimTemplate {
				template := testClaimTemplate(goodName, goodNS, validClaimSpec)
				template.ResourceVersion = "1"
				return template
			}(),
		},
		"generation": {
			template: func() *resource.ResourceClaimTemplate {
				template := testClaimTemplate(goodName, goodNS, validClaimSpec)
				template.Generation = 100
				return template
			}(),
		},
		"creation-timestamp": {
			template: func() *resource.ResourceClaimTemplate {
				template := testClaimTemplate(goodName, goodNS, validClaimSpec)
				template.CreationTimestamp = now
				return template
			}(),
		},
		"deletion-grace-period-seconds": {
			template: func() *resource.ResourceClaimTemplate {
				template := testClaimTemplate(goodName, goodNS, validClaimSpec)
				template.DeletionGracePeriodSeconds = pointer.Int64(10)
				return template
			}(),
		},
		"owner-references": {
			template: func() *resource.ResourceClaimTemplate {
				template := testClaimTemplate(goodName, goodNS, validClaimSpec)
				template.OwnerReferences = []metav1.OwnerReference{
					{
						APIVersion: "v1",
						Kind:       "pod",
						Name:       "foo",
						UID:        "ac051fac-2ead-46d9-b8b4-4e0fbeb7455d",
					},
				}
				return template
			}(),
		},
		"finalizers": {
			template: func() *resource.ResourceClaimTemplate {
				template := testClaimTemplate(goodName, goodNS, validClaimSpec)
				template.Finalizers = []string{
					"example.com/foo",
				}
				return template
			}(),
		},
		"managed-fields": {
			template: func() *resource.ResourceClaimTemplate {
				template := testClaimTemplate(goodName, goodNS, validClaimSpec)
				template.ManagedFields = []metav1.ManagedFieldsEntry{
					{
						FieldsType: "FieldsV1",
						Operation:  "Apply",
						APIVersion: "apps/v1",
						Manager:    "foo",
					},
				}
				return template
			}(),
		},
		"good-labels": {
			template: func() *resource.ResourceClaimTemplate {
				template := testClaimTemplate(goodName, goodNS, validClaimSpec)
				template.Labels = map[string]string{
					"apps.kubernetes.io/name": "test",
				}
				return template
			}(),
		},
		"bad-labels": {
			wantFailures: field.ErrorList{field.Invalid(field.NewPath("metadata", "labels"), badValue, "a valid label must be an empty string or consist of alphanumeric characters, '-', '_' or '.', and must start and end with an alphanumeric character (e.g. 'MyValue',  or 'my_value',  or '12345', regex used for validation is '(([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9])?')")},
			template: func() *resource.ResourceClaimTemplate {
				template := testClaimTemplate(goodName, goodNS, validClaimSpec)
				template.Labels = map[string]string{
					"hello-world": badValue,
				}
				return template
			}(),
		},
		"good-annotations": {
			template: func() *resource.ResourceClaimTemplate {
				template := testClaimTemplate(goodName, goodNS, validClaimSpec)
				template.Annotations = map[string]string{
					"foo": "bar",
				}
				return template
			}(),
		},
		"bad-annotations": {
			wantFailures: field.ErrorList{field.Invalid(field.NewPath("metadata", "annotations"), badName, "name part must consist of alphanumeric characters, '-', '_' or '.', and must start and end with an alphanumeric character (e.g. 'MyName',  or 'my.name',  or '123-abc', regex used for validation is '([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9]')")},
			template: func() *resource.ResourceClaimTemplate {
				template := testClaimTemplate(goodName, goodNS, validClaimSpec)
				template.Annotations = map[string]string{
					badName: "hello world",
				}
				return template
			}(),
		},
		"bad-classname": {
			wantFailures: field.ErrorList{field.Invalid(field.NewPath("spec", "spec", "devices", "requests").Index(0).Child("deviceClassName"), badName, "a lowercase RFC 1123 subdomain must consist of lower case alphanumeric characters, '-' or '.', and must start and end with an alphanumeric character (e.g. 'example.com', regex used for validation is '[a-z0-9]([-a-z0-9]*[a-z0-9])?(\\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*')")},
			template: func() *resource.ResourceClaimTemplate {
				template := testClaimTemplate(goodName, goodNS, validClaimSpec)
				template.Spec.Spec.Devices.Requests[0].DeviceClassName = badName
				return template
			}(),
		},
		"prioritized-list": {
			wantFailures: nil,
			template:     testClaimTemplate(goodName, goodNS, validClaimSpecWithFirstAvailable),
		},
		"proritized-list-class-name-on-parent": {
			wantFailures: field.ErrorList{field.Invalid(field.NewPath("spec", "spec", "devices", "requests").Index(0).Child("deviceClassName"), goodName, "must not be specified when firstAvailable is set")},
			template: func() *resource.ResourceClaimTemplate {
				template := testClaimTemplate(goodName, goodNS, validClaimSpecWithFirstAvailable)
				template.Spec.Spec.Devices.Requests[0].DeviceClassName = goodName
				return template
			}(),
		},
		"prioritized-list-bad-class-name-on-subrequest": {
			wantFailures: field.ErrorList{field.Invalid(field.NewPath("spec", "spec", "devices", "requests").Index(0).Child("firstAvailable").Index(0).Child("deviceClassName"), badName, "a lowercase RFC 1123 subdomain must consist of lower case alphanumeric characters, '-' or '.', and must start and end with an alphanumeric character (e.g. 'example.com', regex used for validation is '[a-z0-9]([-a-z0-9]*[a-z0-9])?(\\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*')")},
			template: func() *resource.ResourceClaimTemplate {
				template := testClaimTemplate(goodName, goodNS, validClaimSpecWithFirstAvailable)
				template.Spec.Spec.Devices.Requests[0].FirstAvailable[0].DeviceClassName = badName
				return template
			}(),
		},
	}

	for name, scenario := range scenarios {
		t.Run(name, func(t *testing.T) {
			errs := ValidateResourceClaimTemplate(scenario.template)
			assertFailures(t, scenario.wantFailures, errs)
		})
	}
}

func TestValidateClaimTemplateUpdate(t *testing.T) {
	validClaimTemplate := testClaimTemplate(goodName, goodNS, validClaimSpec)

	scenarios := map[string]struct {
		oldClaimTemplate *resource.ResourceClaimTemplate
		update           func(claim *resource.ResourceClaimTemplate) *resource.ResourceClaimTemplate
		wantFailures     field.ErrorList
	}{
		"valid-no-op-update": {
			oldClaimTemplate: validClaimTemplate,
			update:           func(claim *resource.ResourceClaimTemplate) *resource.ResourceClaimTemplate { return claim },
		},
		"invalid-update-class": {
			wantFailures: field.ErrorList{field.Invalid(field.NewPath("spec"), func() resource.ResourceClaimTemplateSpec {
				spec := validClaimTemplate.Spec.DeepCopy()
				spec.Spec.Devices.Requests[0].DeviceClassName += "2"
				return *spec
			}(), "field is immutable")},
			oldClaimTemplate: validClaimTemplate,
			update: func(template *resource.ResourceClaimTemplate) *resource.ResourceClaimTemplate {
				template.Spec.Spec.Devices.Requests[0].DeviceClassName += "2"
				return template
			},
		},
		"prioritized-listinvalid-update-class": {
			wantFailures: field.ErrorList{field.Invalid(field.NewPath("spec"), func() resource.ResourceClaimTemplateSpec {
				template := testClaimTemplate(goodName, goodNS, validClaimSpecWithFirstAvailable)
				template.Spec.Spec.Devices.Requests[0].FirstAvailable[0].DeviceClassName += "2"
				return template.Spec
			}(), "field is immutable")},
			oldClaimTemplate: testClaimTemplate(goodName, goodNS, validClaimSpecWithFirstAvailable),
			update: func(template *resource.ResourceClaimTemplate) *resource.ResourceClaimTemplate {
				template.Spec.Spec.Devices.Requests[0].FirstAvailable[0].DeviceClassName += "2"
				return template
			},
		},
	}

	for name, scenario := range scenarios {
		t.Run(name, func(t *testing.T) {
			scenario.oldClaimTemplate.ResourceVersion = "1"
			errs := ValidateResourceClaimTemplateUpdate(scenario.update(scenario.oldClaimTemplate.DeepCopy()), scenario.oldClaimTemplate)
			assertFailures(t, scenario.wantFailures, errs)
		})
	}
}
