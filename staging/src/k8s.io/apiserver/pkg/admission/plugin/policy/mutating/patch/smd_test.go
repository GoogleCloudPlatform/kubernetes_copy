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

package patch

import (
	"context"
	"github.com/google/go-cmp/cmp"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apiserver/pkg/admission"
	"k8s.io/apiserver/pkg/admission/plugin/cel"
	celconfig "k8s.io/apiserver/pkg/apis/cel"
	"k8s.io/apiserver/pkg/cel/environment"
	"k8s.io/client-go/openapi/openapitest"
	"k8s.io/utils/ptr"
)

func TestApplyConfiguration(t *testing.T) {
	deploymentGVR := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
	deploymentGVK := schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"}
	tests := []struct {
		name              string
		expression        string
		gvr               schema.GroupVersionResource
		object, oldObject runtime.Object
		expectedResult    runtime.Object
		expectedErr       string
	}{
		{
			name: "apply configuration add to listType=map",
			expression: `Object{
					spec: Object.spec{
						template: Object.spec.template{
							spec: Object.spec.template.spec{
								volumes: [Object.spec.template.spec.volumes{
									name: "y"
								}]
							}
						}
					}
				}`,
			gvr: deploymentGVR,
			object: &appsv1.Deployment{Spec: appsv1.DeploymentSpec{
				Template: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Volumes: []corev1.Volume{{Name: "x"}},
					},
				},
			}},
			expectedResult: &appsv1.Deployment{Spec: appsv1.DeploymentSpec{
				Template: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Volumes: []corev1.Volume{{Name: "x"}, {Name: "y"}},
					},
				},
			}},
		},
		{
			name: "apply configuration update listType=map entry",
			expression: `Object{
					spec: Object.spec{
						template: Object.spec.template{
							spec: Object.spec.template.spec{
								volumes: [Object.spec.template.spec.volumes{
									name: "y",
									hostPath: Object.spec.template.spec.volumes.hostPath{
										path: "a"
									}
								}]
							}
						}
					}
				}`,
			gvr: deploymentGVR,
			object: &appsv1.Deployment{Spec: appsv1.DeploymentSpec{
				Template: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Volumes: []corev1.Volume{{Name: "x"}, {Name: "y"}},
					},
				},
			}},
			expectedResult: &appsv1.Deployment{Spec: appsv1.DeploymentSpec{
				Template: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Volumes: []corev1.Volume{{Name: "x"}, {Name: "y", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "a"}}}},
					},
				},
			}},
		},
		{
			name: "apply configuration delete from listType=map via filter",
			expression: `Object{
					spec: Object.spec{
						template: Object.spec.template{
							spec: Object.spec.template.spec{
								volumes: object.spec.template.spec.volumes.filter(c, c.name != "y") 
							}
						}
					}
				}`,
			gvr: deploymentGVR,
			object: &appsv1.Deployment{Spec: appsv1.DeploymentSpec{
				Template: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Volumes: []corev1.Volume{{Name: "x"}, {Name: "y"}},
					},
				},
			}},
			expectedResult: &appsv1.Deployment{Spec: appsv1.DeploymentSpec{
				Template: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Volumes: []corev1.Volume{{Name: "x"}},
					},
				},
			}},
		},
		{
			name: "apply configuration delete from listType=map via optional.none()",
			expression: `Object{
					spec: Object.spec{
						template: Object.spec.template{
							spec: Object.spec.template.spec{
								volumes: [Object.spec.template.spec.volumes{
									name: "y",
									_: optional.none()
								}]
							}
						}
					}
				}`,
			// gives:     smd_test.go:379: unexpected error: error applying patch: failed to convert patch object to typed object: .spec.template.spec.volumes[name="y"]._: field not declared in schema
			gvr: deploymentGVR,
			object: &appsv1.Deployment{Spec: appsv1.DeploymentSpec{
				Template: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Volumes: []corev1.Volume{{Name: "x"}, {Name: "y"}},
					},
				},
			}},
			expectedResult: &appsv1.Deployment{Spec: appsv1.DeploymentSpec{
				Template: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Volumes: []corev1.Volume{{Name: "x"}},
					},
				},
			}},
		},
		{
			name: "apply configuration with conditionals",
			expression: `Object{
					spec: Object.spec{
						replicas: object.spec.replicas % 2 == 0?object.spec.replicas + 1:object.spec.replicas
					}
				}`,
			gvr:            deploymentGVR,
			object:         &appsv1.Deployment{Spec: appsv1.DeploymentSpec{Replicas: ptr.To[int32](2)}},
			expectedResult: &appsv1.Deployment{Spec: appsv1.DeploymentSpec{Replicas: ptr.To[int32](3)}},
		},
		{
			name: "apply configuration with old object",
			expression: `Object{
					spec: Object.spec{
						replicas: oldObject.spec.replicas % 2 == 0?oldObject.spec.replicas + 1:oldObject.spec.replicas
					}
				}`,
			gvr:            deploymentGVR,
			object:         &appsv1.Deployment{Spec: appsv1.DeploymentSpec{Replicas: ptr.To[int32](1)}},
			oldObject:      &appsv1.Deployment{Spec: appsv1.DeploymentSpec{Replicas: ptr.To[int32](2)}},
			expectedResult: &appsv1.Deployment{Spec: appsv1.DeploymentSpec{Replicas: ptr.To[int32](3)}},
		},
		{
			name: "complex apply configuration initialization",
			expression: `Object{
					spec: Object.spec{
						replicas: 1,
						template: Object.spec.template{
							metadata: Object.spec.template.metadata{
								labels: {"app": "nginx"}
							},
							spec: Object.spec.template.spec{
								containers: [Object.spec.template.spec.containers{
									name: "nginx",
									image: "nginx:1.14.2",
									ports: [Object.spec.template.spec.containers.ports{
										containerPort: 80
									}],
									resources: Object.spec.template.spec.containers.resources{
										limits: {"cpu": "128M"},
									}
								}]
							}
						}
					}
				}`,

			gvr:    deploymentGVR,
			object: &appsv1.Deployment{Spec: appsv1.DeploymentSpec{}},
			expectedResult: &appsv1.Deployment{Spec: appsv1.DeploymentSpec{
				Replicas: ptr.To[int32](1),
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{"app": "nginx"},
					},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{
							Name:  "nginx",
							Image: "nginx:1.14.2",
							Ports: []corev1.ContainerPort{
								{ContainerPort: 80},
							},
							Resources: corev1.ResourceRequirements{
								Limits: corev1.ResourceList{corev1.ResourceName("cpu"): resource.MustParse("128M")},
							},
						}},
					},
				},
			}},
		},
		{
			name: "apply configuration with change to atomic",
			expression: `Object{
					spec: Object.spec{
						selector: Object.spec.selector{
							matchLabels: {"l": "v"}
						}
					}
				}`,
			gvr:         deploymentGVR,
			object:      &appsv1.Deployment{Spec: appsv1.DeploymentSpec{Replicas: ptr.To[int32](1)}},
			expectedErr: "error applying patch: invalid ApplyConfiguration: may not mutate atomic arrays, maps or structs: .spec.selector",
		},
		{
			name: "apply configuration with invalid type name",
			expression: `Object{
					spec: Object.specx{
						replicas: 1
					}
				}`,
			gvr:         deploymentGVR,
			object:      &appsv1.Deployment{Spec: appsv1.DeploymentSpec{Replicas: ptr.To[int32](1)}},
			expectedErr: "type mismatch: unexpected type name \"Object.specx\", expected \"Object.spec\", which matches field name path from root Object type",
		},
		{
			name: "apply configuration with invalid field name",
			expression: `Object{
					spec: Object.spec{
						replicasx: 1
					}
				}`,
			gvr:         deploymentGVR,
			object:      &appsv1.Deployment{Spec: appsv1.DeploymentSpec{Replicas: ptr.To[int32](1)}},
			expectedErr: "error applying patch: failed to convert patch object to typed object: .spec.replicasx: field not declared in schema",
		},
		{
			name:        "apply configuration with invalid return type",
			expression:  `"I'm a teapot!"`,
			gvr:         deploymentGVR,
			object:      &appsv1.Deployment{Spec: appsv1.DeploymentSpec{Replicas: ptr.To[int32](1)}},
			expectedErr: "must evaluate to Object but got string",
		},
		{
			name:        "apply configuration with invalid initializer return type",
			expression:  `Object.spec.metadata{}`,
			gvr:         deploymentGVR,
			object:      &appsv1.Deployment{Spec: appsv1.DeploymentSpec{Replicas: ptr.To[int32](1)}},
			expectedErr: "must evaluate to Object but got Object.spec.metadata",
		},
	}

	compiler, err := cel.NewCompositedCompiler(environment.MustBaseEnvSet(environment.DefaultCompatibilityVersion(), true))
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tcManager := NewTypeConverterManager(nil, openapitest.NewEmbeddedFileClient())
	go tcManager.Run(ctx)

	err = wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, time.Second, true, func(context.Context) (done bool, err error) {
		converter := tcManager.GetTypeConverter(deploymentGVK)
		return converter != nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			accessor := &ApplyConfigurationCondition{Expression: tc.expression}
			compileResult := compiler.CompileMutatingEvaluator(accessor, cel.OptionalVariableDeclarations{StrictCost: true, HasPatchTypes: true}, environment.StoredExpressions)

			patcher := applyConfigPatcher{expressionEvaluator: compileResult}

			scheme := runtime.NewScheme()
			err := appsv1.AddToScheme(scheme)
			if err != nil {
				t.Fatal(err)
			}

			var gvk schema.GroupVersionKind
			gvks, _, err := scheme.ObjectKinds(tc.object)
			if err != nil {
				t.Fatal(err)
			}
			if len(gvks) == 1 {
				gvk = gvks[0]
			} else {
				t.Fatalf("Failed to find gvk for type: %T", tc.object)
			}

			metaAccessor, err := meta.Accessor(tc.object)
			if err != nil {
				t.Fatal(err)
			}

			typeAccessor, err := meta.TypeAccessor(tc.object)
			if err != nil {
				t.Fatal(err)
			}
			typeAccessor.SetKind(gvk.Kind)
			typeAccessor.SetAPIVersion(gvk.GroupVersion().String())

			attrs := admission.NewAttributesRecord(tc.object, tc.oldObject, gvk,
				metaAccessor.GetNamespace(), metaAccessor.GetName(), tc.gvr,
				"", admission.Create, &metav1.CreateOptions{}, false, nil)
			vAttrs := &admission.VersionedAttributes{
				Attributes:         attrs,
				VersionedKind:      gvk,
				VersionedObject:    tc.object,
				VersionedOldObject: tc.oldObject,
			}

			r := Request{
				MatchedResource:     tc.gvr,
				VersionedAttributes: vAttrs,
				ObjectInterfaces:    admission.NewObjectInterfacesFromScheme(scheme),
				OptionalVariables:   cel.OptionalVariableBindings{},
				TypeConverter:       tcManager.GetTypeConverter(gvk),
			}

			patched, err := patcher.Patch(ctx, r, celconfig.RuntimeCELCostBudget)
			if len(tc.expectedErr) > 0 {
				if err == nil {
					t.Fatalf("expected error: %s", tc.expectedErr)
				} else {
					if !strings.Contains(err.Error(), tc.expectedErr) {
						t.Fatalf("expected error: %s, got: %s", tc.expectedErr, err.Error())
					}
					return
				}
			}
			if err != nil && len(tc.expectedErr) == 0 {
				t.Fatalf("unexpected error: %v", err)
			}

			got, err := runtime.DefaultUnstructuredConverter.ToUnstructured(patched)
			if err != nil {
				t.Fatal(err)
			}

			wantTypeAccessor, err := meta.TypeAccessor(tc.expectedResult)
			if err != nil {
				t.Fatal(err)
			}
			wantTypeAccessor.SetKind(gvk.Kind)
			wantTypeAccessor.SetAPIVersion(gvk.GroupVersion().String())

			want, err := runtime.DefaultUnstructuredConverter.ToUnstructured(tc.expectedResult)

			if err != nil {
				t.Fatal(err)
			}
			if !equality.Semantic.DeepEqual(want, got) {
				t.Errorf("unexpected result, got diff:\n%s\n", cmp.Diff(want, got))
			}
		})
	}
}
