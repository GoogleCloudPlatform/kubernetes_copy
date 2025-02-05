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

package cel

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/interpreter"

	"k8s.io/apiserver/pkg/cel/environment"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	apiv1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	nodev1 "k8s.io/api/node/v1"
	storagev1 "k8s.io/api/storage/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	extclientset "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	apiextensionsscheme "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/scheme"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
	commoncel "k8s.io/apiserver/pkg/cel"
	celopenapi "k8s.io/apiserver/pkg/cel/openapi"
	"k8s.io/apiserver/pkg/cel/openapi/resolver"
	k8sscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/kube-openapi/pkg/validation/spec"
	"k8s.io/utils/pointer"

	apiservertesting "k8s.io/kubernetes/cmd/kube-apiserver/app/testing"
	corev1 "k8s.io/kubernetes/pkg/apis/core/v1"
	"k8s.io/kubernetes/pkg/generated/openapi"
	"k8s.io/kubernetes/test/integration/framework"
)

func TestTypeResolver(t *testing.T) {
	server, err := apiservertesting.StartTestServer(t, nil, nil, framework.SharedEtcd())
	if err != nil {
		t.Fatal(err)
	}
	defer server.TearDownFn()

	config := server.ClientConfig

	client, err := extclientset.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}

	crd, err := installCRD(client)
	if err != nil {
		t.Fatal(err)
	}
	defer func(crd *apiextensionsv1.CustomResourceDefinition) {
		err := client.ApiextensionsV1().CustomResourceDefinitions().Delete(context.Background(), crd.Name, metav1.DeleteOptions{})
		if err != nil {
			t.Fatal(err)
		}
	}(crd)
	discoveryResolver := &resolver.ClientDiscoveryResolver{Discovery: client.Discovery()}
	definitionsResolver := resolver.NewDefinitionsSchemaResolver(openapi.GetOpenAPIDefinitions, k8sscheme.Scheme, apiextensionsscheme.Scheme)
	// wait until the CRD schema is published at the OpenAPI v3 endpoint
	err = wait.PollImmediate(time.Second, time.Minute, func() (done bool, err error) {
		p, err := client.OpenAPIV3().Paths()
		if err != nil {
			return
		}
		if _, ok := p["apis/apis.example.com/v1beta1"]; ok {
			return true, nil
		}
		return false, nil
	})
	if err != nil {
		t.Fatalf("timeout wait for CRD schema publication: %v", err)
	}

	for _, tc := range []struct {
		name                string
		obj                 runtime.Object
		expression          string
		expectResolutionErr bool
		expectCompileErr    bool
		expectEvalErr       bool
		expectedResult      any
		resolvers           []resolver.SchemaResolver
	}{
		{
			name: "unknown type",
			obj: &unstructured.Unstructured{Object: map[string]any{
				"kind":       "Bad",
				"apiVersion": "bad.example.com/v1",
			}},
			expectResolutionErr: true,
			resolvers:           []resolver.SchemaResolver{definitionsResolver, discoveryResolver},
		},
		{
			name:                "deployment",
			obj:                 sampleReplicatedDeployment(),
			expression:          "self.spec.replicas > 1",
			expectResolutionErr: false,
			expectCompileErr:    false,
			expectEvalErr:       false,
			resolvers:           []resolver.SchemaResolver{definitionsResolver, discoveryResolver},

			// expect a boolean, which is `true`.
			expectedResult: true,
		},
		{
			name:                "missing field",
			obj:                 sampleReplicatedDeployment(),
			expression:          "self.spec.missing > 1",
			expectResolutionErr: false,
			expectCompileErr:    true,
			resolvers:           []resolver.SchemaResolver{definitionsResolver, discoveryResolver},
		},
		{
			name:                "mistyped expression",
			obj:                 sampleReplicatedDeployment(),
			expression:          "self.spec.replicas == '1'",
			expectResolutionErr: false,
			expectCompileErr:    true,
			resolvers:           []resolver.SchemaResolver{definitionsResolver, discoveryResolver},
		},
		{
			name: "crd valid",
			obj: &unstructured.Unstructured{Object: map[string]any{
				"kind":       "CronTab",
				"apiVersion": "apis.example.com/v1beta1",
				"spec": map[string]any{
					"cronSpec": "* * * * *",
					"image":    "foo-image",
					"replicas": 2,
				},
			}},
			expression:          "self.spec.replicas > 1",
			expectResolutionErr: false,
			expectCompileErr:    false,
			expectEvalErr:       false,
			resolvers:           []resolver.SchemaResolver{discoveryResolver},

			// expect a boolean, which is `true`.
			expectedResult: true,
		},
		{
			name: "crd missing field",
			obj: &unstructured.Unstructured{Object: map[string]any{
				"kind":       "CronTab",
				"apiVersion": "apis.example.com/v1beta1",
				"spec": map[string]any{
					"cronSpec": "* * * * *",
					"image":    "foo-image",
					"replicas": 2,
				},
			}},
			expression:          "self.spec.missing > 1",
			expectResolutionErr: false,
			expectCompileErr:    true,
			resolvers:           []resolver.SchemaResolver{discoveryResolver},
		},
		{
			name: "crd mistyped",
			obj: &unstructured.Unstructured{Object: map[string]any{
				"kind":       "CronTab",
				"apiVersion": "apis.example.com/v1beta1",
				"spec": map[string]any{
					"cronSpec": "* * * * *",
					"image":    "foo-image",
					"replicas": 2,
				},
			}},
			expression:          "self.spec.replica == '1'",
			expectResolutionErr: false,
			expectCompileErr:    true,
			resolvers:           []resolver.SchemaResolver{discoveryResolver},
		},
		{
			name: "items population",
			obj:  sampleReplicatedDeployment(),
			// `containers` is an array whose items are of `Container` type
			// `ports` is an array of `ContainerPort`
			expression: "size(self.spec.template.spec.containers) > 0 &&" +
				"self.spec.template.spec.containers.all(c, c.ports.all(p, p.containerPort < 1024))",
			expectResolutionErr: false,
			expectCompileErr:    false,
			expectEvalErr:       false,
			expectedResult:      true,
			resolvers:           []resolver.SchemaResolver{definitionsResolver, discoveryResolver},
		},
		{
			name: "int-or-string int",
			obj: &appsv1.Deployment{
				TypeMeta: metav1.TypeMeta{
					Kind:       "Deployment",
					APIVersion: "apps/v1",
				},
				Spec: appsv1.DeploymentSpec{
					Strategy: appsv1.DeploymentStrategy{
						Type: appsv1.RollingUpdateDeploymentStrategyType,
						RollingUpdate: &appsv1.RollingUpdateDeployment{
							MaxSurge: &intstr.IntOrString{Type: intstr.Int, IntVal: 5},
						},
					},
				},
			},
			expression: "has(self.spec.strategy.rollingUpdate) &&" +
				"type(self.spec.strategy.rollingUpdate.maxSurge) == int &&" +
				"self.spec.strategy.rollingUpdate.maxSurge > 1",
			expectResolutionErr: false,
			expectCompileErr:    false,
			expectEvalErr:       false,
			expectedResult:      true,
			resolvers:           []resolver.SchemaResolver{definitionsResolver, discoveryResolver},
		},
		{
			name: "int-or-string string",
			obj: &appsv1.Deployment{
				TypeMeta: metav1.TypeMeta{
					Kind:       "Deployment",
					APIVersion: "apps/v1",
				},
				Spec: appsv1.DeploymentSpec{
					Strategy: appsv1.DeploymentStrategy{
						Type: appsv1.RollingUpdateDeploymentStrategyType,
						RollingUpdate: &appsv1.RollingUpdateDeployment{
							MaxSurge: &intstr.IntOrString{Type: intstr.String, StrVal: "10%"},
						},
					},
				},
			},
			expression: "has(self.spec.strategy.rollingUpdate) &&" +
				"type(self.spec.strategy.rollingUpdate.maxSurge) == string &&" +
				"self.spec.strategy.rollingUpdate.maxSurge == '10%'",
			expectResolutionErr: false,
			expectCompileErr:    false,
			expectEvalErr:       false,
			expectedResult:      true,
			resolvers:           []resolver.SchemaResolver{definitionsResolver, discoveryResolver},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gvk := tc.obj.GetObjectKind().GroupVersionKind()
			var s *spec.Schema
			for _, r := range tc.resolvers {
				var err error
				s, err = r.ResolveSchema(gvk)
				if err != nil {
					if tc.expectResolutionErr {
						return
					}
					t.Fatalf("cannot resolve type: %v", err)
				}
				if tc.expectResolutionErr {
					t.Fatalf("expected resolution error but got none")
				}
			}
			program, err := simpleCompileCEL(s, tc.expression)
			if err != nil {
				if tc.expectCompileErr {
					return
				}
				t.Fatalf("cannot eval: %v", err)
			}
			if tc.expectCompileErr {
				t.Fatalf("expected compilation error but got none")
			}
			unstructured, err := runtime.DefaultUnstructuredConverter.ToUnstructured(tc.obj)
			if err != nil {
				t.Fatal(err)
			}
			ret, _, err := program.Eval(&simpleActivation{self: celopenapi.UnstructuredToVal(unstructured, s)})
			if err != nil {
				if tc.expectEvalErr {
					return
				}
				t.Fatalf("cannot eval: %v", err)
			}
			if tc.expectEvalErr {
				t.Fatalf("expected eval error but got none")
			}
			if !reflect.DeepEqual(ret.Value(), tc.expectedResult) {
				t.Errorf("wrong result, expected %q but got %q", tc.expectedResult, ret)
			}
		})
	}

}

// TestBuiltinResolution asserts that all resolver implementations should
// resolve Kubernetes built-in types without error.
func TestBuiltinResolution(t *testing.T) {
	// before all, setup server and client
	server, err := apiservertesting.StartTestServer(t, nil, nil, framework.SharedEtcd())
	if err != nil {
		t.Fatal(err)
	}
	defer server.TearDownFn()

	config := server.ClientConfig

	client, err := extclientset.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name     string
		resolver resolver.SchemaResolver
		scheme   *runtime.Scheme
	}{
		{
			name:     "definitions",
			resolver: resolver.NewDefinitionsSchemaResolver(openapi.GetOpenAPIDefinitions, k8sscheme.Scheme, apiextensionsscheme.Scheme),
			scheme:   buildTestScheme(),
		},
		{
			name:     "discovery",
			resolver: &resolver.ClientDiscoveryResolver{Discovery: client.Discovery()},
			scheme:   buildTestScheme(),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for gvk := range tc.scheme.AllKnownTypes() {
				// skip aliases to metav1
				if gvk.Kind == "APIGroup" || gvk.Kind == "APIGroupList" || gvk.Kind == "APIVersions" ||
					strings.HasSuffix(gvk.Kind, "Options") || strings.HasSuffix(gvk.Kind, "Event") {
					continue
				}
				// skip private, reference, and alias types that cannot appear in the wild
				if gvk.Kind == "SerializedReference" || gvk.Kind == "List" || gvk.Kind == "RangeAllocation" || gvk.Kind == "PodStatusResult" {
					continue
				}
				// skip internal types
				if gvk.Version == "__internal" {
					continue
				}
				// apiextensions.k8s.io/v1beta1 not published
				if tc.name == "discovery" && gvk.Group == "apiextensions.k8s.io" && gvk.Version == "v1beta1" {
					continue
				}
				// apiextensions.k8s.io ConversionReview not published
				if tc.name == "discovery" && gvk.Group == "apiextensions.k8s.io" && gvk.Kind == "ConversionReview" {
					continue
				}
				_, err = tc.resolver.ResolveSchema(gvk)
				if err != nil {
					t.Errorf("resolver %q cannot resolve %v", tc.name, gvk)
				}
			}
		})
	}
}

// simpleCompileCEL compiles the CEL expression against the schema
// with the practical defaults.
// `self` is defined as the object being evaluated against.
func simpleCompileCEL(schema *spec.Schema, expression string) (cel.Program, error) {
	env, err := environment.MustBaseEnvSet(environment.DefaultCompatibilityVersion(), true).Env(environment.NewExpressions)
	if err != nil {
		return nil, err
	}
	declType := celopenapi.SchemaDeclType(schema, true).MaybeAssignTypeName("selfType")
	rt := commoncel.NewDeclTypeProvider(declType)
	opts, err := rt.EnvOptions(env.CELTypeProvider())
	if err != nil {
		return nil, err
	}
	rootType, _ := rt.FindDeclType("selfType")
	opts = append(opts, cel.Variable("self", rootType.CelType()))
	env, err = env.Extend(opts...)
	if err != nil {
		return nil, err
	}
	ast, issues := env.Compile(expression)
	if issues != nil {
		return nil, issues.Err()
	}
	return env.Program(ast)
}

// sampleReplicatedDeployment returns a sample Deployment with 2 replicas.
// The object is not inlined because the schema of Deployment is well-known
// and thus requires no reference when reading the test cases.
func sampleReplicatedDeployment() *appsv1.Deployment {
	return &appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Deployment",
			APIVersion: "apps/v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: "demo-deployment",
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: pointer.Int32(2),
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app": "demo",
				},
			},
			Template: apiv1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app": "demo",
					},
				},
				Spec: apiv1.PodSpec{
					Containers: []apiv1.Container{
						{
							Name:  "web",
							Image: "nginx",
							Ports: []apiv1.ContainerPort{
								{
									Name:          "http",
									Protocol:      apiv1.ProtocolTCP,
									ContainerPort: 80,
								},
							},
						},
					},
				},
			},
		},
	}
}

func installCRD(apiExtensionClient extclientset.Interface) (*apiextensionsv1.CustomResourceDefinition, error) {
	// CRD borrowed from https://kubernetes.io/docs/tasks/extend-kubernetes/custom-resources/custom-resource-definitions/
	crd := &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Name: "crontabs.apis.example.com",
		},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: "apis.example.com",
			Scope: apiextensionsv1.NamespaceScoped,
			Names: apiextensionsv1.CustomResourceDefinitionNames{
				Plural:   "crontabs",
				Singular: "crontab",
				Kind:     "CronTab",
				ListKind: "CronTabList",
			},
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{
				{
					Name:    "v1beta1",
					Served:  true,
					Storage: true,
					Schema: &apiextensionsv1.CustomResourceValidation{
						OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
							XPreserveUnknownFields: pointer.Bool(true),
							Type:                   "object",
							Properties: map[string]apiextensionsv1.JSONSchemaProps{
								"spec": {
									Type: "object",
									Properties: map[string]apiextensionsv1.JSONSchemaProps{
										"cronSpec": {Type: "string"},
										"image":    {Type: "string"},
										"replicas": {Type: "integer"},
									},
								},
							},
						},
					},
				},
			},
		},
	}

	return apiExtensionClient.ApiextensionsV1().
		CustomResourceDefinitions().Create(context.Background(), crd, metav1.CreateOptions{})
}

type simpleActivation struct {
	self any
}

func (a *simpleActivation) ResolveName(name string) (interface{}, bool) {
	switch name {
	case "self":
		return a.self, true
	default:
		return nil, false
	}
}

func (a *simpleActivation) Parent() interpreter.Activation {
	return nil
}

func buildTestScheme() *runtime.Scheme {
	// hand-picked schemes that the test API server serves
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = appsv1.AddToScheme(scheme)
	_ = admissionregistrationv1.AddToScheme(scheme)
	_ = networkingv1.AddToScheme(scheme)
	_ = nodev1.AddToScheme(scheme)
	_ = storagev1.AddToScheme(scheme)
	_ = apiextensionsscheme.AddToScheme(scheme)
	return scheme
}
