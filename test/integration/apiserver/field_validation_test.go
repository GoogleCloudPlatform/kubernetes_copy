/*
Copyright 2021 The Kubernetes Authors.

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

package apiserver

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"strings"
	"testing"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextensionsclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	"k8s.io/apiextensions-apiserver/test/integration/fixtures"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"
	kubeapiservertesting "k8s.io/kubernetes/cmd/kube-apiserver/app/testing"

	"k8s.io/kubernetes/test/integration/framework"
)

var (
	invalidBodyJSON = `
	{
		"apiVersion": "apps/v1",
		"kind": "Deployment",
		"metadata": {
			"name": "dupename",
			"name": "%s",
			"labels": {"app": "nginx"},
			"unknownMeta": "metaVal"
		},
		"spec": {
			"unknown1": "val1",
			"unknownDupe": "valDupe",
			"unknownDupe": "valDupe2",
			"paused": true,
			"paused": false,
			"selector": {
				"matchLabels": {
					"app": "nginx"
				}
			},
			"template": {
				"metadata": {
					"labels": {
						"app": "nginx"
					}
				},
				"spec": {
					"containers": [{
						"name":  "nginx",
						"image": "nginx:latest",
						"unknownNested": "val1",
						"imagePullPolicy": "Always",
						"imagePullPolicy": "Never"
					}]
				}
			}
		}
	}
		`
	validBodyJSON = `
{
	"apiVersion": "apps/v1",
	"kind": "Deployment",
	"metadata": {
		"name": "%s",
		"labels": {"app": "nginx"},
		"annotations": {"a1": "foo", "a2": "bar"}
	},
	"spec": {
		"selector": {
			"matchLabels": {
				"app": "nginx"
			}
		},
		"template": {
			"metadata": {
				"labels": {
					"app": "nginx"
				}
			},
			"spec": {
				"containers": [{
					"name":  "nginx",
					"image": "nginx:latest",
					"imagePullPolicy": "Always"
				}]
			}
		},
		"replicas": 2
	}
}`

	invalidBodyYAML = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: dupename
  name: %s
  unknownMeta: metaVal
  labels:
    app: nginx
spec:
  unknown1: val1
  unknownDupe: valDupe
  unknownDupe: valDupe2
  paused: true
  paused: false
  selector:
    matchLabels:
      app: nginx
  template:
    metadata:
      labels:
        app: nginx
    spec:
      containers:
      - name: nginx
        image: nginx:latest
        unknownNested: val1
        imagePullPolicy: Always
        imagePullPolicy: Never`

	validBodyYAML = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: %s
  labels:
    app: nginx
  annotations:
    a1: foo
    a2: bar
spec:
  replicas: 2
  paused: true
  selector:
    matchLabels:
      app: nginx
  template:
    metadata:
      labels:
        app: nginx
    spec:
      containers:
      - name: nginx
        image: nginx:latest
        imagePullPolicy: Always`

	applyInvalidBody = `{
		"apiVersion": "apps/v1",
		"kind": "Deployment",
		"metadata": {
			"name": "%s",
			"labels": {"app": "nginx"}
		},
		"spec": {
			"paused": false,
			"paused": true,
			"selector": {
				"matchLabels": {
					"app": "nginx"
				}
			},
			"template": {
				"metadata": {
					"labels": {
						"app": "nginx"
					}
				},
				"spec": {
					"containers": [{
						"name":  "nginx",
						"image": "nginx:latest",
						"imagePullPolicy": "Never",
						"imagePullPolicy": "Always"
					}]
				}
			}
		}
	}`
	applyValidBody = `
{
	"apiVersion": "apps/v1",
	"kind": "Deployment",
	"metadata": {
		"name": "%s",
		"labels": {"app": "nginx"},
		"annotations": {"a1": "foo", "a2": "bar"}
	},
	"spec": {
		"selector": {
			"matchLabels": {
				"app": "nginx"
			}
		},
		"template": {
			"metadata": {
				"labels": {
					"app": "nginx"
				}
			},
			"spec": {
				"containers": [{
					"name":  "nginx",
					"image": "nginx:latest",
					"imagePullPolicy": "Always"
				}]
			}
		},
		"replicas": 3
	}
}`
	crdInvalidBody = `
{
	"apiVersion": "%s",
	"kind": "%s",
	"metadata": {
		"name": "dupename",
		"name": "%s",
		"unknownMeta": "metaVal",
		"resourceVersion": "%s"
	},
	"spec": {
		"unknown1": "val1",
		"unknownDupe": "valDupe",
		"unknownDupe": "valDupe2",
		"knownField1": "val1",
		"knownField1": "val2",
			"ports": [{
				"name": "portName",
				"containerPort": 8080,
				"protocol": "TCP",
				"hostPort": 8081,
				"hostPort": 8082,
				"unknownNested": "val"
			}],
		"embeddedObj": {
			"apiVersion": "v1",
			"kind": "ConfigMap",
			"metadata": {
				"name": "my-cm",
				"namespace": "my-ns",
				"unknownEmbeddedMeta": "foo"
			}
		}
	}
}`

	crdValidBody = `
{
	"apiVersion": "%s",
	"kind": "%s",
	"metadata": {
		"name": "%s",
		"resourceVersion": "%s"
	},
	"spec": {
		"knownField1": "val1",
			"ports": [{
				"name": "portName",
				"containerPort": 8080,
				"protocol": "TCP",
				"hostPort": 8081
			}],
		"embeddedObj": {
			"apiVersion": "v1",
			"kind": "ConfigMap",
			"metadata": {
				"name": "my-cm"
			}
		}
	}
}
	`

	crdInvalidBodyYAML = `
apiVersion: "%s"
kind: "%s"
metadata:
  name: dupename
  name: "%s"
  resourceVersion: "%s"
  unknownMeta: metaVal
spec:
  unknown1: val1
  unknownDupe: valDupe
  unknownDupe: valDupe2
  knownField1: val1
  knownField1: val2
  ports:
  - name: portName
    containerPort: 8080
    protocol: TCP
    hostPort: 8081
    hostPort: 8082
    unknownNested: val
  embeddedObj:
    apiVersion: v1
    kind: ConfigMap
    metadata:
      name: my-cm
      namespace: my-ns
      unknownEmbeddedMeta: foo`

	crdValidBodyYAML = `
apiVersion: "%s"
kind: "%s"
metadata:
  name: "%s"
  resourceVersion: "%s"
spec:
  knownField1: val1
  ports:
  - name: portName
    containerPort: 8080
    protocol: TCP
    hostPort: 8081
  embeddedObj:
    apiVersion: v1
    kind: ConfigMap
    metadata:
      name: my-cm
      namespace: my-ns`

	crdApplyInvalidBody = `
{
	"apiVersion": "%s",
	"kind": "%s",
	"metadata": {
		"name": "%s"
	},
	"spec": {
		"knownField1": "val1",
		"knownField1": "val2",
		"ports": [{
			"name": "portName",
			"containerPort": 8080,
			"protocol": "TCP",
			"hostPort": 8081,
			"hostPort": 8082
		}],
		"embeddedObj": {
			"apiVersion": "v1",
			"kind": "ConfigMap",
			"metadata": {
				"name": "my-cm",
				"namespace": "my-ns"
			}
		}
	}
}`

	crdApplyValidBody = `
{
	"apiVersion": "%s",
	"kind": "%s",
	"metadata": {
		"name": "%s"
	},
	"spec": {
		"knownField1": "val1",
		"ports": [{
			"name": "portName",
			"containerPort": 8080,
			"protocol": "TCP",
			"hostPort": 8082
		}],
		"embeddedObj": {
			"apiVersion": "v1",
			"kind": "ConfigMap",
			"metadata": {
				"name": "my-cm",
				"namespace": "my-ns"
			}
		}
	}
}`

	crdApplyValidBody2 = `
{
	"apiVersion": "%s",
	"kind": "%s",
	"metadata": {
		"name": "%s"
	},
	"spec": {
		"knownField1": "val2",
		"ports": [{
			"name": "portName",
			"containerPort": 8080,
			"protocol": "TCP",
			"hostPort": 8083
		}],
		"embeddedObj": {
			"apiVersion": "v1",
			"kind": "ConfigMap",
			"metadata": {
				"name": "my-cm",
				"namespace": "my-ns"
			}
		}
	}
}`

	crdApplyFinalizerBody = `
{
	"apiVersion": "%s",
	"kind": "%s",
	"metadata": {
		"name": "%s",
		"finalizers": %s
	},
	"spec": {
		"knownField1": "val1",
		"ports": [{
			"name": "portName",
			"containerPort": 8080,
			"protocol": "TCP",
			"hostPort": 8082
		}],
		"embeddedObj": {
			"apiVersion": "v1",
			"kind": "ConfigMap",
			"metadata": {
				"name": "my-cm",
				"namespace": "my-ns"
			}
		}
	}
}`

	patchYAMLBody = `
apiVersion: %s
kind: %s
metadata:
  name: %s
  finalizers:
  - test/finalizer
spec:
  cronSpec: "* * * * */5"
  ports:
  - name: x
    containerPort: 80
    protocol: TCP
`

	crdSchemaBase = `
{
		"openAPIV3Schema": {
			"type": "object",
			"properties": {
				"spec": {
					"type": "object",
					%s
					"properties": {
						"cronSpec": {
							"type": "string",
							"pattern": "^(\\d+|\\*)(/\\d+)?(\\s+(\\d+|\\*)(/\\d+)?){4}$"
						},
						"knownField1": {
							"type": "string"
						},
						"embeddedObj": {
							"x-kubernetes-embedded-resource": true,
							"type": "object",
							"properties": {
								"apiversion": {
									"type": "string"
								},
								"kind": {
									"type": "string"
								},
								"metadata": {
									"type": "object"
								}
							}
						},
						"ports": {
							"type": "array",
							"x-kubernetes-list-map-keys": [
								"containerPort",
								"protocol"
							],
							"x-kubernetes-list-type": "map",
							"items": {
								"properties": {
									"containerPort": {
										"format": "int32",
										"type": "integer"
									},
									"hostIP": {
										"type": "string"
									},
									"hostPort": {
										"format": "int32",
										"type": "integer"
									},
									"name": {
										"type": "string"
									},
									"protocol": {
										"type": "string"
									}
								},
								"required": [
									"containerPort",
									"protocol"
								],
								"type": "object"
							}
						}
					}
				}
			}
		}
	}
	`
)

func TestFieldValidation(t *testing.T) {
	server, err := kubeapiservertesting.StartTestServer(t, kubeapiservertesting.NewDefaultTestServerOptions(), nil, framework.SharedEtcd())
	if err != nil {
		t.Fatal(err)
	}
	config := server.ClientConfig
	defer server.TearDownFn()

	// don't log warnings, tests inspect them in the responses directly
	config.WarningHandler = rest.NoWarnings{}

	schemaCRD := setupCRD(t, config, "schema.example.com", false)
	schemaGVR := schema.GroupVersionResource{
		Group:    schemaCRD.Spec.Group,
		Version:  schemaCRD.Spec.Versions[0].Name,
		Resource: schemaCRD.Spec.Names.Plural,
	}
	schemaGVK := schema.GroupVersionKind{
		Group:   schemaCRD.Spec.Group,
		Version: schemaCRD.Spec.Versions[0].Name,
		Kind:    schemaCRD.Spec.Names.Kind,
	}

	schemalessCRD := setupCRD(t, config, "schemaless.example.com", true)
	schemalessGVR := schema.GroupVersionResource{
		Group:    schemalessCRD.Spec.Group,
		Version:  schemalessCRD.Spec.Versions[0].Name,
		Resource: schemalessCRD.Spec.Names.Plural,
	}
	schemalessGVK := schema.GroupVersionKind{
		Group:   schemalessCRD.Spec.Group,
		Version: schemalessCRD.Spec.Versions[0].Name,
		Kind:    schemalessCRD.Spec.Names.Kind,
	}

	client := clientset.NewForConfigOrDie(config)
	rest := client.Discovery().RESTClient()

	t.Run("Post", func(t *testing.T) { testFieldValidationPost(t, client) })
	t.Run("Put", func(t *testing.T) { testFieldValidationPut(t, client) })
	t.Run("PatchTyped", func(t *testing.T) { testFieldValidationPatchTyped(t, client) })
	t.Run("SMP", func(t *testing.T) { testFieldValidationSMP(t, client) })
	t.Run("ApplyCreate", func(t *testing.T) { testFieldValidationApplyCreate(t, client) })
	t.Run("ApplyUpdate", func(t *testing.T) { testFieldValidationApplyUpdate(t, client) })

	t.Run("PostCRD", func(t *testing.T) { testFieldValidationPostCRD(t, rest, schemaGVK, schemaGVR) })
	t.Run("PutCRD", func(t *testing.T) { testFieldValidationPutCRD(t, rest, schemaGVK, schemaGVR) })
	t.Run("PatchCRD", func(t *testing.T) { testFieldValidationPatchCRD(t, rest, schemaGVK, schemaGVR) })
	t.Run("ApplyCreateCRD", func(t *testing.T) { testFieldValidationApplyCreateCRD(t, rest, schemaGVK, schemaGVR) })
	t.Run("ApplyUpdateCRD", func(t *testing.T) { testFieldValidationApplyUpdateCRD(t, rest, schemaGVK, schemaGVR) })

	t.Run("PostCRDSchemaless", func(t *testing.T) { testFieldValidationPostCRDSchemaless(t, rest, schemalessGVK, schemalessGVR) })
	t.Run("PutCRDSchemaless", func(t *testing.T) { testFieldValidationPutCRDSchemaless(t, rest, schemalessGVK, schemalessGVR) })
	t.Run("PatchCRDSchemaless", func(t *testing.T) { testFieldValidationPatchCRDSchemaless(t, rest, schemalessGVK, schemalessGVR) })
	t.Run("ApplyCreateCRDSchemaless", func(t *testing.T) { testFieldValidationApplyCreateCRDSchemaless(t, rest, schemalessGVK, schemalessGVR) })
	t.Run("ApplyUpdateCRDSchemaless", func(t *testing.T) { testFieldValidationApplyUpdateCRDSchemaless(t, rest, schemalessGVK, schemalessGVR) })
	t.Run("testFinalizerValidationApplyCreateCRD", func(t *testing.T) {
		testFinalizerValidationApplyCreateAndUpdateCRD(t, rest, schemalessGVK, schemalessGVR)
	})
}

// testFieldValidationPost tests POST requests containing unknown fields with
// strict and non-strict field validation.
func testFieldValidationPost(t *testing.T, client clientset.Interface) {
	var testcases = []struct {
		name                   string
		bodyBase               string
		opts                   metav1.CreateOptions
		contentType            string
		strictDecodingError    string
		strictDecodingWarnings []string
	}{
		{
			name: "post-strict-validation",
			opts: metav1.CreateOptions{
				FieldValidation: "Strict",
			},
			bodyBase:            invalidBodyJSON,
			strictDecodingError: `strict decoding error: duplicate field "metadata.name", unknown field "metadata.unknownMeta", unknown field "spec.unknown1", unknown field "spec.unknownDupe", duplicate field "spec.paused", unknown field "spec.template.spec.containers[0].unknownNested", duplicate field "spec.template.spec.containers[0].imagePullPolicy"`,
		},
		{
			name: "post-warn-validation",
			opts: metav1.CreateOptions{
				FieldValidation: "Warn",
			},
			bodyBase: invalidBodyJSON,
			strictDecodingWarnings: []string{
				`duplicate field "metadata.name"`,
				`unknown field "metadata.unknownMeta"`,
				`unknown field "spec.unknown1"`,
				`unknown field "spec.unknownDupe"`,
				`duplicate field "spec.paused"`,
				// note: fields that are both unknown
				// and duplicated will only be detected
				// as unknown for typed resources.
				`unknown field "spec.template.spec.containers[0].unknownNested"`,
				`duplicate field "spec.template.spec.containers[0].imagePullPolicy"`,
			},
		},
		{
			name: "post-ignore-validation",
			opts: metav1.CreateOptions{
				FieldValidation: "Ignore",
			},
			bodyBase: invalidBodyJSON,
		},
		{
			name:     "post-no-validation",
			bodyBase: invalidBodyJSON,
			strictDecodingWarnings: []string{
				`duplicate field "metadata.name"`,
				`unknown field "metadata.unknownMeta"`,
				`unknown field "spec.unknown1"`,
				`unknown field "spec.unknownDupe"`,
				`duplicate field "spec.paused"`,
				// note: fields that are both unknown
				// and duplicated will only be detected
				// as unknown for typed resources.
				`unknown field "spec.template.spec.containers[0].unknownNested"`,
				`duplicate field "spec.template.spec.containers[0].imagePullPolicy"`,
			},
		},
		{
			name: "post-strict-validation-yaml",
			opts: metav1.CreateOptions{
				FieldValidation: "Strict",
			},
			bodyBase:    invalidBodyYAML,
			contentType: "application/yaml",
			strictDecodingError: `strict decoding error: yaml: unmarshal errors:
  line 5: key "name" already set in map
  line 12: key "unknownDupe" already set in map
  line 14: key "paused" already set in map
  line 28: key "imagePullPolicy" already set in map, unknown field "metadata.unknownMeta", unknown field "spec.template.spec.containers[0].unknownNested", unknown field "spec.unknown1", unknown field "spec.unknownDupe"`,
		},
		{
			name: "post-warn-validation-yaml",
			opts: metav1.CreateOptions{
				FieldValidation: "Warn",
			},
			bodyBase:    invalidBodyYAML,
			contentType: "application/yaml",
			strictDecodingWarnings: []string{
				`line 5: key "name" already set in map`,
				`line 12: key "unknownDupe" already set in map`,
				`line 14: key "paused" already set in map`,
				`line 28: key "imagePullPolicy" already set in map`,
				`unknown field "metadata.unknownMeta"`,
				`unknown field "spec.template.spec.containers[0].unknownNested"`,
				`unknown field "spec.unknown1"`,
				`unknown field "spec.unknownDupe"`,
			},
		},
		{
			name: "post-ignore-validation-yaml",
			opts: metav1.CreateOptions{
				FieldValidation: "Ignore",
			},
			bodyBase:    invalidBodyYAML,
			contentType: "application/yaml",
		},
		{
			name:        "post-no-validation-yaml",
			bodyBase:    invalidBodyYAML,
			contentType: "application/yaml",
			strictDecodingWarnings: []string{
				`line 5: key "name" already set in map`,
				`line 12: key "unknownDupe" already set in map`,
				`line 14: key "paused" already set in map`,
				`line 28: key "imagePullPolicy" already set in map`,
				`unknown field "metadata.unknownMeta"`,
				`unknown field "spec.template.spec.containers[0].unknownNested"`,
				`unknown field "spec.unknown1"`,
				`unknown field "spec.unknownDupe"`,
			},
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			klog.Warningf("running tc named: %s", tc.name)
			body := []byte(fmt.Sprintf(tc.bodyBase, fmt.Sprintf("test-deployment-%s", tc.name)))
			req := client.CoreV1().RESTClient().Post().
				AbsPath("/apis/apps/v1").
				Namespace("default").
				Resource("deployments").
				SetHeader("Content-Type", tc.contentType).
				VersionedParams(&tc.opts, metav1.ParameterCodec)
			result := req.Body(body).Do(context.TODO())
			if result.Error() == nil && tc.strictDecodingError != "" {
				t.Fatalf("received nil error when expecting: %q", tc.strictDecodingError)
			}
			if result.Error() != nil && (tc.strictDecodingError == "" || !strings.HasSuffix(result.Error().Error(), tc.strictDecodingError)) {
				t.Fatalf("expected error: %q, got: %v", tc.strictDecodingError, result.Error())
			}

			if len(result.Warnings()) != len(tc.strictDecodingWarnings) {
				t.Fatalf("unexpected number of warnings, expected: %d, got: %d", len(tc.strictDecodingWarnings), len(result.Warnings()))
			}
			for i, strictWarn := range tc.strictDecodingWarnings {
				if strictWarn != result.Warnings()[i].Text {
					t.Fatalf("expected warning: %s, got warning: %s", strictWarn, result.Warnings()[i].Text)
				}

			}
		})
	}
}

// testFieldValidationPut tests PUT requests
// that update existing objects with unknown fields
// for both strict and non-strict field validation.
func testFieldValidationPut(t *testing.T, client clientset.Interface) {
	deployName := "test-deployment-put"
	postBody := []byte(fmt.Sprintf(string(validBodyJSON), deployName))

	if _, err := client.CoreV1().RESTClient().Post().
		AbsPath("/apis/apps/v1").
		Namespace("default").
		Resource("deployments").
		Body(postBody).
		DoRaw(context.TODO()); err != nil {
		t.Fatalf("failed to create initial deployment: %v", err)
	}

	var testcases = []struct {
		name                   string
		opts                   metav1.UpdateOptions
		putBodyBase            string
		contentType            string
		strictDecodingError    string
		strictDecodingWarnings []string
	}{
		{
			name: "put-strict-validation",
			opts: metav1.UpdateOptions{
				FieldValidation: "Strict",
			},
			putBodyBase:         invalidBodyJSON,
			strictDecodingError: `strict decoding error: duplicate field "metadata.name", unknown field "metadata.unknownMeta", unknown field "spec.unknown1", unknown field "spec.unknownDupe", duplicate field "spec.paused", unknown field "spec.template.spec.containers[0].unknownNested", duplicate field "spec.template.spec.containers[0].imagePullPolicy"`,
		},
		{
			name: "put-warn-validation",
			opts: metav1.UpdateOptions{
				FieldValidation: "Warn",
			},
			putBodyBase: invalidBodyJSON,
			strictDecodingWarnings: []string{
				`duplicate field "metadata.name"`,
				`unknown field "metadata.unknownMeta"`,
				`unknown field "spec.unknown1"`,
				`unknown field "spec.unknownDupe"`,
				`duplicate field "spec.paused"`,
				// note: fields that are both unknown
				// and duplicated will only be detected
				// as unknown for typed resources.
				`unknown field "spec.template.spec.containers[0].unknownNested"`,
				`duplicate field "spec.template.spec.containers[0].imagePullPolicy"`,
			},
		},
		{
			name: "put-ignore-validation",
			opts: metav1.UpdateOptions{
				FieldValidation: "Ignore",
			},
			putBodyBase: invalidBodyJSON,
		},
		{
			name:        "put-no-validation",
			putBodyBase: invalidBodyJSON,
			strictDecodingWarnings: []string{
				`duplicate field "metadata.name"`,
				`unknown field "metadata.unknownMeta"`,
				`unknown field "spec.unknown1"`,
				`unknown field "spec.unknownDupe"`,
				`duplicate field "spec.paused"`,
				// note: fields that are both unknown
				// and duplicated will only be detected
				// as unknown for typed resources.
				`unknown field "spec.template.spec.containers[0].unknownNested"`,
				`duplicate field "spec.template.spec.containers[0].imagePullPolicy"`,
			},
		},
		{
			name: "put-strict-validation-yaml",
			opts: metav1.UpdateOptions{
				FieldValidation: "Strict",
			},
			putBodyBase: invalidBodyYAML,
			contentType: "application/yaml",
			strictDecodingError: `strict decoding error: yaml: unmarshal errors:
  line 5: key "name" already set in map
  line 12: key "unknownDupe" already set in map
  line 14: key "paused" already set in map
  line 28: key "imagePullPolicy" already set in map, unknown field "metadata.unknownMeta", unknown field "spec.template.spec.containers[0].unknownNested", unknown field "spec.unknown1", unknown field "spec.unknownDupe"`,
		},
		{
			name: "put-warn-validation-yaml",
			opts: metav1.UpdateOptions{
				FieldValidation: "Warn",
			},
			putBodyBase: invalidBodyYAML,
			contentType: "application/yaml",
			strictDecodingWarnings: []string{
				`line 5: key "name" already set in map`,
				`line 12: key "unknownDupe" already set in map`,
				`line 14: key "paused" already set in map`,
				`line 28: key "imagePullPolicy" already set in map`,
				`unknown field "metadata.unknownMeta"`,
				`unknown field "spec.template.spec.containers[0].unknownNested"`,
				`unknown field "spec.unknown1"`,
				`unknown field "spec.unknownDupe"`,
			},
		},
		{
			name: "put-ignore-validation-yaml",
			opts: metav1.UpdateOptions{
				FieldValidation: "Ignore",
			},
			putBodyBase: invalidBodyYAML,
			contentType: "application/yaml",
		},
		{
			name:        "put-no-validation-yaml",
			putBodyBase: invalidBodyYAML,
			contentType: "application/yaml",
			strictDecodingWarnings: []string{
				`line 5: key "name" already set in map`,
				`line 12: key "unknownDupe" already set in map`,
				`line 14: key "paused" already set in map`,
				`line 28: key "imagePullPolicy" already set in map`,
				`unknown field "metadata.unknownMeta"`,
				`unknown field "spec.template.spec.containers[0].unknownNested"`,
				`unknown field "spec.unknown1"`,
				`unknown field "spec.unknownDupe"`,
			},
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			putBody := []byte(fmt.Sprintf(string(tc.putBodyBase), deployName))
			req := client.CoreV1().RESTClient().Put().
				AbsPath("/apis/apps/v1").
				Namespace("default").
				Resource("deployments").
				SetHeader("Content-Type", tc.contentType).
				Name(deployName).
				VersionedParams(&tc.opts, metav1.ParameterCodec)
			result := req.Body([]byte(putBody)).Do(context.TODO())
			if result.Error() == nil && tc.strictDecodingError != "" {
				t.Fatalf("received nil error when expecting: %q", tc.strictDecodingError)
			}
			if result.Error() != nil && (tc.strictDecodingError == "" || !strings.HasSuffix(result.Error().Error(), tc.strictDecodingError)) {
				t.Fatalf("expected error: %q, got: %v", tc.strictDecodingError, result.Error())
			}

			if len(result.Warnings()) != len(tc.strictDecodingWarnings) {
				t.Fatalf("unexpected number of warnings, expected: %d, got: %d", len(tc.strictDecodingWarnings), len(result.Warnings()))
			}
			for i, strictWarn := range tc.strictDecodingWarnings {
				if strictWarn != result.Warnings()[i].Text {
					t.Fatalf("expected warning: %s, got warning: %s", strictWarn, result.Warnings()[i].Text)
				}

			}
		})
	}
}

// testFieldValidationPatchTyped tests merge-patch and json-patch requests containing unknown fields with
// strict and non-strict field validation for typed objects.
func testFieldValidationPatchTyped(t *testing.T, client clientset.Interface) {
	deployName := "test-deployment-patch-typed"
	postBody := []byte(fmt.Sprintf(string(validBodyJSON), deployName))

	if _, err := client.CoreV1().RESTClient().Post().
		AbsPath("/apis/apps/v1").
		Namespace("default").
		Resource("deployments").
		Body(postBody).
		DoRaw(context.TODO()); err != nil {
		t.Fatalf("failed to create initial deployment: %v", err)
	}

	mergePatchBody := `
{
	"spec": {
		"unknown1": "val1",
		"unknownDupe": "valDupe",
		"unknownDupe": "valDupe2",
		"paused": true,
		"paused": false,
		"template": {
			"spec": {
				"containers": [{
					"name": "nginx",
					"image": "nginx:latest",
					"unknownNested": "val1",
					"imagePullPolicy": "Always",
					"imagePullPolicy": "Never"
				}]
			}
		}
	}
}
	`
	jsonPatchBody := `
			[
				{"op": "add", "path": "/spec/unknown1", "value": "val1", "foo":"bar"},
				{"op": "add", "path": "/spec/unknown2", "path": "/spec/unknown3", "value": "val1"},
				{"op": "add", "path": "/spec/unknownDupe", "value": "valDupe"},
				{"op": "add", "path": "/spec/unknownDupe", "value": "valDupe2"},
				{"op": "add", "path": "/spec/paused", "value": true},
				{"op": "add", "path": "/spec/paused", "value": false},
				{"op": "add", "path": "/spec/template/spec/containers/0/unknownNested", "value": "val1"},
				{"op": "add", "path": "/spec/template/spec/containers/0/imagePullPolicy", "value": "Always"},
				{"op": "add", "path": "/spec/template/spec/containers/0/imagePullPolicy", "value": "Never"}
			]
			`
	// non-conflicting mergePatch has issues with the patch (duplicate fields),
	// but doesn't conflict with the existing object it's being patched to
	nonconflictingMergePatchBody := `
{
	"spec": {
		"paused": true,
		"paused": false,
		"template": {
			"spec": {
				"containers": [{
					"name": "nginx",
					"image": "nginx:latest",
					"imagePullPolicy": "Always",
					"imagePullPolicy": "Never"
				}]
			}
		}
	}
}
			`
	var testcases = []struct {
		name                   string
		opts                   metav1.PatchOptions
		patchType              types.PatchType
		body                   string
		strictDecodingError    string
		strictDecodingWarnings []string
	}{
		{
			name: "merge-patch-strict-validation",
			opts: metav1.PatchOptions{
				FieldValidation: "Strict",
			},
			patchType:           types.MergePatchType,
			body:                mergePatchBody,
			strictDecodingError: `strict decoding error: duplicate field "spec.unknownDupe", duplicate field "spec.paused", duplicate field "spec.template.spec.containers[0].imagePullPolicy", unknown field "spec.template.spec.containers[0].unknownNested", unknown field "spec.unknown1", unknown field "spec.unknownDupe"`,
		},
		{
			name: "merge-patch-warn-validation",
			opts: metav1.PatchOptions{
				FieldValidation: "Warn",
			},
			patchType: types.MergePatchType,
			body:      mergePatchBody,
			strictDecodingWarnings: []string{
				`duplicate field "spec.unknownDupe"`,
				`duplicate field "spec.paused"`,
				`duplicate field "spec.template.spec.containers[0].imagePullPolicy"`,
				`unknown field "spec.template.spec.containers[0].unknownNested"`,
				`unknown field "spec.unknown1"`,
				`unknown field "spec.unknownDupe"`,
			},
		},
		{
			name: "merge-patch-ignore-validation",
			opts: metav1.PatchOptions{
				FieldValidation: "Ignore",
			},
			patchType: types.MergePatchType,
			body:      mergePatchBody,
		},
		{
			name:      "merge-patch-no-validation",
			patchType: types.MergePatchType,
			body:      mergePatchBody,
			strictDecodingWarnings: []string{
				`duplicate field "spec.unknownDupe"`,
				`duplicate field "spec.paused"`,
				`duplicate field "spec.template.spec.containers[0].imagePullPolicy"`,
				`unknown field "spec.template.spec.containers[0].unknownNested"`,
				`unknown field "spec.unknown1"`,
				`unknown field "spec.unknownDupe"`,
			},
		},
		{
			name:      "json-patch-strict-validation",
			patchType: types.JSONPatchType,
			opts: metav1.PatchOptions{
				FieldValidation: "Strict",
			},
			body:                jsonPatchBody,
			strictDecodingError: `strict decoding error: json patch unknown field "[0].foo", json patch duplicate field "[1].path", unknown field "spec.template.spec.containers[0].unknownNested", unknown field "spec.unknown1", unknown field "spec.unknown3", unknown field "spec.unknownDupe"`,
		},
		{
			name:      "json-patch-warn-validation",
			patchType: types.JSONPatchType,
			opts: metav1.PatchOptions{
				FieldValidation: "Warn",
			},
			body: jsonPatchBody,
			strictDecodingWarnings: []string{
				// note: duplicate fields in the patch itself
				// are dropped by the
				// evanphx/json-patch library and is expected.
				// Duplicate fields in the json patch ops
				// themselves can be detected though
				`json patch unknown field "[0].foo"`,
				`json patch duplicate field "[1].path"`,
				`unknown field "spec.template.spec.containers[0].unknownNested"`,
				`unknown field "spec.unknown1"`,
				`unknown field "spec.unknown3"`,
				`unknown field "spec.unknownDupe"`,
			},
		},
		{
			name:      "json-patch-ignore-validation",
			patchType: types.JSONPatchType,
			opts: metav1.PatchOptions{
				FieldValidation: "Ignore",
			},
			body: jsonPatchBody,
		},
		{
			name:      "json-patch-no-validation",
			patchType: types.JSONPatchType,
			body:      jsonPatchBody,
			strictDecodingWarnings: []string{
				// note: duplicate fields in the patch itself
				// are dropped by the
				// evanphx/json-patch library and is expected.
				// Duplicate fields in the json patch ops
				// themselves can be detected though
				`json patch unknown field "[0].foo"`,
				`json patch duplicate field "[1].path"`,
				`unknown field "spec.template.spec.containers[0].unknownNested"`,
				`unknown field "spec.unknown1"`,
				`unknown field "spec.unknown3"`,
				`unknown field "spec.unknownDupe"`,
			},
		},
		{
			name: "nonconflicting-merge-patch-strict-validation",
			opts: metav1.PatchOptions{
				FieldValidation: "Strict",
			},
			patchType:           types.MergePatchType,
			body:                nonconflictingMergePatchBody,
			strictDecodingError: `strict decoding error: duplicate field "spec.paused", duplicate field "spec.template.spec.containers[0].imagePullPolicy"`,
		},
		{
			name: "nonconflicting-merge-patch-warn-validation",
			opts: metav1.PatchOptions{
				FieldValidation: "Warn",
			},
			patchType: types.MergePatchType,
			body:      nonconflictingMergePatchBody,
			strictDecodingWarnings: []string{
				`duplicate field "spec.paused"`,
				`duplicate field "spec.template.spec.containers[0].imagePullPolicy"`,
			},
		},
		{
			name: "nonconflicting-merge-patch-ignore-validation",
			opts: metav1.PatchOptions{
				FieldValidation: "Ignore",
			},
			patchType: types.MergePatchType,
			body:      nonconflictingMergePatchBody,
		},
		{
			name:      "nonconflicting-merge-patch-no-validation",
			patchType: types.MergePatchType,
			body:      nonconflictingMergePatchBody,
			strictDecodingWarnings: []string{
				`duplicate field "spec.paused"`,
				`duplicate field "spec.template.spec.containers[0].imagePullPolicy"`,
			},
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			req := client.CoreV1().RESTClient().Patch(tc.patchType).
				AbsPath("/apis/apps/v1").
				Namespace("default").
				Resource("deployments").
				Name(deployName).
				VersionedParams(&tc.opts, metav1.ParameterCodec)
			result := req.Body([]byte(tc.body)).Do(context.TODO())
			if result.Error() == nil && tc.strictDecodingError != "" {
				t.Fatalf("received nil error when expecting: %q", tc.strictDecodingError)
			}
			if result.Error() != nil && (tc.strictDecodingError == "" || !strings.HasSuffix(result.Error().Error(), tc.strictDecodingError)) {
				t.Fatalf("expected error: %q, got: %v", tc.strictDecodingError, result.Error())
			}

			if len(result.Warnings()) != len(tc.strictDecodingWarnings) {
				t.Fatalf("unexpected number of warnings, expected: %d, got: %d", len(tc.strictDecodingWarnings), len(result.Warnings()))
			}
			for i, strictWarn := range tc.strictDecodingWarnings {
				if strictWarn != result.Warnings()[i].Text {
					t.Fatalf("expected warning: %s, got warning: %s", strictWarn, result.Warnings()[i].Text)
				}

			}
		})
	}
}

// testFieldValidationSMP tests that attempting a strategic-merge-patch
// with unknown fields errors out when fieldValidation is strict,
// but succeeds when fieldValidation is ignored.
func testFieldValidationSMP(t *testing.T, client clientset.Interface) {
	// non-conflicting SMP has issues with the patch (duplicate fields),
	// but doesn't conflict with the existing object it's being patched to
	nonconflictingSMPBody := `
	{
		"spec": {
			"paused": true,
			"paused": false,
			"selector": {
				"matchLabels": {
					"app": "nginx"
				}
			},
			"template": {
				"metadata": {
					"labels": {
						"app": "nginx"
					}
				},
				"spec": {
					"containers": [{
						"name": "nginx",
						"imagePullPolicy": "Always",
						"imagePullPolicy": "Never"
					}]
				}
			}
		}
	}
	`

	smpBody := `
	{
		"spec": {
			"unknown1": "val1",
			"unknownDupe": "valDupe",
			"unknownDupe": "valDupe2",
			"paused": true,
			"paused": false,
			"selector": {
				"matchLabels": {
					"app": "nginx"
				}
			},
			"template": {
				"metadata": {
					"labels": {
						"app": "nginx"
					}
				},
				"spec": {
					"containers": [{
						"name": "nginx",
						"unknownNested": "val1",
						"imagePullPolicy": "Always",
						"imagePullPolicy": "Never"
					}]
				}
			}
		}
	}
	`
	var testcases = []struct {
		name                   string
		opts                   metav1.PatchOptions
		body                   string
		strictDecodingError    string
		strictDecodingWarnings []string
	}{
		{
			name: "smp-strict-validation",
			opts: metav1.PatchOptions{
				FieldValidation: "Strict",
			},
			body:                smpBody,
			strictDecodingError: `strict decoding error: duplicate field "spec.unknownDupe", duplicate field "spec.paused", duplicate field "spec.template.spec.containers[0].imagePullPolicy", unknown field "spec.template.spec.containers[0].unknownNested", unknown field "spec.unknown1", unknown field "spec.unknownDupe"`,
		},
		{
			name: "smp-warn-validation",
			opts: metav1.PatchOptions{
				FieldValidation: "Warn",
			},
			body: smpBody,
			strictDecodingWarnings: []string{
				`duplicate field "spec.unknownDupe"`,
				`duplicate field "spec.paused"`,
				`duplicate field "spec.template.spec.containers[0].imagePullPolicy"`,
				`unknown field "spec.template.spec.containers[0].unknownNested"`,
				`unknown field "spec.unknown1"`,
				`unknown field "spec.unknownDupe"`,
			},
		},
		{
			name: "smp-ignore-validation",
			opts: metav1.PatchOptions{
				FieldValidation: "Ignore",
			},
			body: smpBody,
		},
		{
			name: "smp-no-validation",
			body: smpBody,
			strictDecodingWarnings: []string{
				`duplicate field "spec.unknownDupe"`,
				`duplicate field "spec.paused"`,
				`duplicate field "spec.template.spec.containers[0].imagePullPolicy"`,
				`unknown field "spec.template.spec.containers[0].unknownNested"`,
				`unknown field "spec.unknown1"`,
				`unknown field "spec.unknownDupe"`,
			},
		},
		{
			name: "nonconflicting-smp-strict-validation",
			opts: metav1.PatchOptions{
				FieldValidation: "Strict",
			},
			body:                nonconflictingSMPBody,
			strictDecodingError: `strict decoding error: duplicate field "spec.paused", duplicate field "spec.template.spec.containers[0].imagePullPolicy"`,
		},
		{
			name: "nonconflicting-smp-warn-validation",
			opts: metav1.PatchOptions{
				FieldValidation: "Warn",
			},
			body: nonconflictingSMPBody,
			strictDecodingWarnings: []string{
				`duplicate field "spec.paused"`,
				`duplicate field "spec.template.spec.containers[0].imagePullPolicy"`,
			},
		},
		{
			name: "nonconflicting-smp-ignore-validation",
			opts: metav1.PatchOptions{
				FieldValidation: "Ignore",
			},
			body: nonconflictingSMPBody,
		},
		{
			name: "nonconflicting-smp-no-validation",
			body: nonconflictingSMPBody,
			strictDecodingWarnings: []string{
				`duplicate field "spec.paused"`,
				`duplicate field "spec.template.spec.containers[0].imagePullPolicy"`,
			},
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(fmt.Sprintf(validBodyJSON, tc.name))
			_, err := client.CoreV1().RESTClient().Patch(types.ApplyPatchType).
				AbsPath("/apis/apps/v1").
				Namespace("default").
				Resource("deployments").
				Name(tc.name).
				Param("fieldManager", "apply_test").
				Body(body).
				Do(context.TODO()).
				Get()
			if err != nil {
				t.Fatalf("Failed to create object using Apply patch: %v", err)
			}

			req := client.CoreV1().RESTClient().Patch(types.StrategicMergePatchType).
				AbsPath("/apis/apps/v1").
				Namespace("default").
				Resource("deployments").
				Name(tc.name).
				VersionedParams(&tc.opts, metav1.ParameterCodec)
			result := req.Body([]byte(tc.body)).Do(context.TODO())
			if result.Error() == nil && tc.strictDecodingError != "" {
				t.Fatalf("received nil error when expecting: %q", tc.strictDecodingError)
			}
			if result.Error() != nil && (tc.strictDecodingError == "" || !strings.HasSuffix(result.Error().Error(), tc.strictDecodingError)) {
				t.Fatalf("expected error: %q, got: %v", tc.strictDecodingError, result.Error())
			}

			if len(result.Warnings()) != len(tc.strictDecodingWarnings) {
				t.Fatalf("unexpected number of warnings, expected: %d, got: %d", len(tc.strictDecodingWarnings), len(result.Warnings()))
			}

			for i, strictWarn := range tc.strictDecodingWarnings {
				if strictWarn != result.Warnings()[i].Text {
					t.Fatalf("expected warning: %s, got warning: %s", strictWarn, result.Warnings()[i].Text)
				}

			}
		})
	}
}

// testFieldValidationApplyCreate tests apply patch requests containing unknown fields
// on newly created objects, with strict and non-strict field validation.
func testFieldValidationApplyCreate(t *testing.T, client clientset.Interface) {
	var testcases = []struct {
		name                   string
		opts                   metav1.PatchOptions
		strictDecodingError    string
		strictDecodingWarnings []string
	}{
		{
			name: "strict-validation",
			opts: metav1.PatchOptions{
				FieldValidation: "Strict",
				FieldManager:    "mgr",
			},
			strictDecodingError: `error strict decoding YAML: error converting YAML to JSON: yaml: unmarshal errors:
  line 10: key "paused" already set in map
  line 27: key "imagePullPolicy" already set in map`,
		},
		{
			name: "warn-validation",
			opts: metav1.PatchOptions{
				FieldValidation: "Warn",
				FieldManager:    "mgr",
			},
			strictDecodingWarnings: []string{
				`line 10: key "paused" already set in map`,
				`line 27: key "imagePullPolicy" already set in map`,
			},
		},
		{
			name: "ignore-validation",
			opts: metav1.PatchOptions{
				FieldValidation: "Ignore",
				FieldManager:    "mgr",
			},
		},
		{
			name: "no-validation",
			opts: metav1.PatchOptions{
				FieldManager: "mgr",
			},
			strictDecodingWarnings: []string{
				`line 10: key "paused" already set in map`,
				`line 27: key "imagePullPolicy" already set in map`,
			},
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			name := fmt.Sprintf("apply-create-deployment-%s", tc.name)
			body := []byte(fmt.Sprintf(applyInvalidBody, name))
			req := client.CoreV1().RESTClient().Patch(types.ApplyPatchType).
				AbsPath("/apis/apps/v1").
				Namespace("default").
				Resource("deployments").
				Name(name).
				VersionedParams(&tc.opts, metav1.ParameterCodec)
			result := req.Body(body).Do(context.TODO())
			if result.Error() == nil && tc.strictDecodingError != "" {
				t.Fatalf("received nil error when expecting: %q", tc.strictDecodingError)
			}
			if result.Error() != nil && (tc.strictDecodingError == "" || !strings.HasSuffix(result.Error().Error(), tc.strictDecodingError)) {
				t.Fatalf("expected error: %q, got: %v", tc.strictDecodingError, result.Error())
			}

			if len(result.Warnings()) != len(tc.strictDecodingWarnings) {
				t.Fatalf("unexpected number of warnings, expected: %d, got: %d", len(tc.strictDecodingWarnings), len(result.Warnings()))
			}
			for i, strictWarn := range tc.strictDecodingWarnings {
				if strictWarn != result.Warnings()[i].Text {
					t.Fatalf("expected warning: %s, got warning: %s", strictWarn, result.Warnings()[i].Text)
				}

			}
		})
	}
}

// testFieldValidationApplyUpdate tests apply patch requests containing unknown fields
// on apply requests to existing objects, with strict and non-strict field validation.
func testFieldValidationApplyUpdate(t *testing.T, client clientset.Interface) {
	var testcases = []struct {
		name                   string
		opts                   metav1.PatchOptions
		strictDecodingError    string
		strictDecodingWarnings []string
	}{
		{
			name: "strict-validation",
			opts: metav1.PatchOptions{
				FieldValidation: "Strict",
				FieldManager:    "mgr",
			},
			strictDecodingError: `error strict decoding YAML: error converting YAML to JSON: yaml: unmarshal errors:
  line 10: key "paused" already set in map
  line 27: key "imagePullPolicy" already set in map`,
		},
		{
			name: "warn-validation",
			opts: metav1.PatchOptions{
				FieldValidation: "Warn",
				FieldManager:    "mgr",
			},
			strictDecodingWarnings: []string{
				`line 10: key "paused" already set in map`,
				`line 27: key "imagePullPolicy" already set in map`,
			},
		},
		{
			name: "ignore-validation",
			opts: metav1.PatchOptions{
				FieldValidation: "Ignore",
				FieldManager:    "mgr",
			},
		},
		{
			name: "no-validation",
			opts: metav1.PatchOptions{
				FieldManager: "mgr",
			},
			strictDecodingWarnings: []string{
				`line 10: key "paused" already set in map`,
				`line 27: key "imagePullPolicy" already set in map`,
			},
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			name := fmt.Sprintf("apply-update-deployment-%s", tc.name)
			createBody := []byte(fmt.Sprintf(validBodyJSON, name))
			createReq := client.CoreV1().RESTClient().Patch(types.ApplyPatchType).
				AbsPath("/apis/apps/v1").
				Namespace("default").
				Resource("deployments").
				Name(name).
				VersionedParams(&tc.opts, metav1.ParameterCodec)
			createResult := createReq.Body(createBody).Do(context.TODO())
			if createResult.Error() != nil {
				t.Fatalf("unexpected apply create err: %v", createResult.Error())
			}

			updateBody := []byte(fmt.Sprintf(applyInvalidBody, name))
			updateReq := client.CoreV1().RESTClient().Patch(types.ApplyPatchType).
				AbsPath("/apis/apps/v1").
				Namespace("default").
				Resource("deployments").
				Name(name).
				VersionedParams(&tc.opts, metav1.ParameterCodec)
			result := updateReq.Body(updateBody).Do(context.TODO())
			if result.Error() == nil && tc.strictDecodingError != "" {
				t.Fatalf("received nil error when expecting: %q", tc.strictDecodingError)
			}
			if result.Error() != nil && (tc.strictDecodingError == "" || !strings.HasSuffix(result.Error().Error(), tc.strictDecodingError)) {
				t.Fatalf("expected error: %q, got: %v", tc.strictDecodingError, result.Error())
			}

			if len(result.Warnings()) != len(tc.strictDecodingWarnings) {
				t.Fatalf("unexpected number of warnings, expected: %d, got: %d", len(tc.strictDecodingWarnings), len(result.Warnings()))
			}
			for i, strictWarn := range tc.strictDecodingWarnings {
				if strictWarn != result.Warnings()[i].Text {
					t.Fatalf("expected warning: %s, got warning: %s", strictWarn, result.Warnings()[i].Text)
				}

			}
		})
	}
}

// testFieldValidationPostCRD tests that server-side schema validation
// works for CRD create requests for CRDs with schemas
func testFieldValidationPostCRD(t *testing.T, rest rest.Interface, gvk schema.GroupVersionKind, gvr schema.GroupVersionResource) {
	var testcases = []struct {
		name                   string
		opts                   metav1.PatchOptions
		body                   string
		contentType            string
		strictDecodingError    string
		strictDecodingWarnings []string
	}{
		{
			name: "crd-post-strict-validation",
			opts: metav1.PatchOptions{
				FieldValidation: "Strict",
			},
			body:                crdInvalidBody,
			strictDecodingError: `strict decoding error: duplicate field "metadata.name", duplicate field "spec.unknownDupe", duplicate field "spec.knownField1", duplicate field "spec.ports[0].hostPort", unknown field "metadata.unknownMeta", unknown field "spec.ports[0].unknownNested", unknown field "spec.unknown1", unknown field "spec.unknownDupe", unknown field "spec.embeddedObj.metadata.unknownEmbeddedMeta"`,
		},
		{
			name: "crd-post-warn-validation",
			opts: metav1.PatchOptions{
				FieldValidation: "Warn",
			},
			body: crdInvalidBody,
			strictDecodingWarnings: []string{
				`duplicate field "metadata.name"`,
				`duplicate field "spec.unknownDupe"`,
				`duplicate field "spec.knownField1"`,
				`duplicate field "spec.ports[0].hostPort"`,
				`unknown field "metadata.unknownMeta"`,
				`unknown field "spec.ports[0].unknownNested"`,
				`unknown field "spec.unknown1"`,
				`unknown field "spec.unknownDupe"`,
				`unknown field "spec.embeddedObj.metadata.unknownEmbeddedMeta"`,
			},
		},
		{
			name: "crd-post-ignore-validation",
			opts: metav1.PatchOptions{
				FieldValidation: "Ignore",
			},
			body: crdInvalidBody,
		},
		{
			name: "crd-post-no-validation",
			body: crdInvalidBody,
			strictDecodingWarnings: []string{
				`duplicate field "metadata.name"`,
				`duplicate field "spec.unknownDupe"`,
				`duplicate field "spec.knownField1"`,
				`duplicate field "spec.ports[0].hostPort"`,
				`unknown field "metadata.unknownMeta"`,
				`unknown field "spec.ports[0].unknownNested"`,
				`unknown field "spec.unknown1"`,
				`unknown field "spec.unknownDupe"`,
				`unknown field "spec.embeddedObj.metadata.unknownEmbeddedMeta"`,
			},
		},
		{
			name: "crd-post-strict-validation-yaml",
			opts: metav1.PatchOptions{
				FieldValidation: "Strict",
			},
			body:        crdInvalidBodyYAML,
			contentType: "application/yaml",
			strictDecodingError: `strict decoding error: yaml: unmarshal errors:
  line 6: key "name" already set in map
  line 12: key "unknownDupe" already set in map
  line 14: key "knownField1" already set in map
  line 20: key "hostPort" already set in map, unknown field "metadata.unknownMeta", unknown field "spec.ports[0].unknownNested", unknown field "spec.unknown1", unknown field "spec.unknownDupe", unknown field "spec.embeddedObj.metadata.unknownEmbeddedMeta"`,
		},
		{
			name: "crd-post-warn-validation-yaml",
			opts: metav1.PatchOptions{
				FieldValidation: "Warn",
			},
			body:        crdInvalidBodyYAML,
			contentType: "application/yaml",
			strictDecodingWarnings: []string{
				`line 6: key "name" already set in map`,
				`line 12: key "unknownDupe" already set in map`,
				`line 14: key "knownField1" already set in map`,
				`line 20: key "hostPort" already set in map`,
				`unknown field "metadata.unknownMeta"`,
				`unknown field "spec.ports[0].unknownNested"`,
				`unknown field "spec.unknown1"`,
				`unknown field "spec.unknownDupe"`,
				`unknown field "spec.embeddedObj.metadata.unknownEmbeddedMeta"`,
			},
		},
		{
			name: "crd-post-ignore-validation-yaml",
			opts: metav1.PatchOptions{
				FieldValidation: "Ignore",
			},
			body:        crdInvalidBodyYAML,
			contentType: "application/yaml",
		},
		{
			name:        "crd-post-no-validation-yaml",
			body:        crdInvalidBodyYAML,
			contentType: "application/yaml",
			strictDecodingWarnings: []string{
				`line 6: key "name" already set in map`,
				`line 12: key "unknownDupe" already set in map`,
				`line 14: key "knownField1" already set in map`,
				`line 20: key "hostPort" already set in map`,
				`unknown field "metadata.unknownMeta"`,
				`unknown field "spec.ports[0].unknownNested"`,
				`unknown field "spec.unknown1"`,
				`unknown field "spec.unknownDupe"`,
				`unknown field "spec.embeddedObj.metadata.unknownEmbeddedMeta"`,
			},
		},
	}
	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			klog.Warningf("running tc named: %s", tc.name)
			kind := gvk.Kind
			apiVersion := gvk.Group + "/" + gvk.Version

			// create the CR as specified by the test case
			jsonBody := []byte(fmt.Sprintf(tc.body, apiVersion, kind, tc.name))
			req := rest.Post().
				AbsPath("/apis", gvr.Group, gvr.Version, gvr.Resource).
				SetHeader("Content-Type", tc.contentType).
				VersionedParams(&tc.opts, metav1.ParameterCodec)
			result := req.Body([]byte(jsonBody)).Do(context.TODO())
			if result.Error() == nil && tc.strictDecodingError != "" {
				t.Fatalf("received nil error when expecting: %q", tc.strictDecodingError)
			}
			if result.Error() != nil && (tc.strictDecodingError == "" || !strings.HasSuffix(result.Error().Error(), tc.strictDecodingError)) {
				t.Fatalf("expected error: %q, got: %v", tc.strictDecodingError, result.Error())
			}

			if len(result.Warnings()) != len(tc.strictDecodingWarnings) {
				t.Fatalf("unexpected number of warnings, expected: %d, got: %d", len(tc.strictDecodingWarnings), len(result.Warnings()))
			}

			for i, strictWarn := range tc.strictDecodingWarnings {
				if strictWarn != result.Warnings()[i].Text {
					t.Fatalf("expected warning: %s, got warning: %s", strictWarn, result.Warnings()[i].Text)
				}

			}
		})
	}
}

// testFieldValidationPostCRDSchemaless tests that server-side schema validation
// works for CRD create requests for CRDs that have schemas
// with x-kubernetes-preserve-unknown-field set
func testFieldValidationPostCRDSchemaless(t *testing.T, rest rest.Interface, gvk schema.GroupVersionKind, gvr schema.GroupVersionResource) {
	var testcases = []struct {
		name                   string
		opts                   metav1.PatchOptions
		body                   string
		contentType            string
		strictDecodingError    string
		strictDecodingWarnings []string
	}{
		{
			name: "schemaless-crd-post-strict-validation",
			opts: metav1.PatchOptions{
				FieldValidation: "Strict",
			},
			body:                crdInvalidBody,
			strictDecodingError: `strict decoding error: duplicate field "metadata.name", duplicate field "spec.unknownDupe", duplicate field "spec.knownField1", duplicate field "spec.ports[0].hostPort", unknown field "metadata.unknownMeta", unknown field "spec.ports[0].unknownNested", unknown field "spec.embeddedObj.metadata.unknownEmbeddedMeta"`,
		},
		{
			name: "schemaless-crd-post-warn-validation",
			opts: metav1.PatchOptions{
				FieldValidation: "Warn",
			},
			body: crdInvalidBody,
			strictDecodingWarnings: []string{
				`duplicate field "metadata.name"`,
				`duplicate field "spec.unknownDupe"`,
				`duplicate field "spec.knownField1"`,
				`duplicate field "spec.ports[0].hostPort"`,
				`unknown field "metadata.unknownMeta"`,
				`unknown field "spec.ports[0].unknownNested"`,
				`unknown field "spec.embeddedObj.metadata.unknownEmbeddedMeta"`,
			},
		},
		{
			name: "schemaless-crd-post-ignore-validation",
			opts: metav1.PatchOptions{
				FieldValidation: "Ignore",
			},
			body: crdInvalidBody,
		},
		{
			name: "schemaless-crd-post-no-validation",
			body: crdInvalidBody,
			strictDecodingWarnings: []string{
				`duplicate field "metadata.name"`,
				`duplicate field "spec.unknownDupe"`,
				`duplicate field "spec.knownField1"`,
				`duplicate field "spec.ports[0].hostPort"`,
				`unknown field "metadata.unknownMeta"`,
				`unknown field "spec.ports[0].unknownNested"`,
				`unknown field "spec.embeddedObj.metadata.unknownEmbeddedMeta"`,
			},
		},
		{
			name: "schemaless-crd-post-strict-validation-yaml",
			opts: metav1.PatchOptions{
				FieldValidation: "Strict",
			},
			body:        crdInvalidBodyYAML,
			contentType: "application/yaml",
			strictDecodingError: `strict decoding error: yaml: unmarshal errors:
  line 6: key "name" already set in map
  line 12: key "unknownDupe" already set in map
  line 14: key "knownField1" already set in map
  line 20: key "hostPort" already set in map, unknown field "metadata.unknownMeta", unknown field "spec.ports[0].unknownNested", unknown field "spec.embeddedObj.metadata.unknownEmbeddedMeta"`,
		},
		{
			name: "schemaless-crd-post-warn-validation-yaml",
			opts: metav1.PatchOptions{
				FieldValidation: "Warn",
			},
			body:        crdInvalidBodyYAML,
			contentType: "application/yaml",
			strictDecodingWarnings: []string{
				`line 6: key "name" already set in map`,
				`line 12: key "unknownDupe" already set in map`,
				`line 14: key "knownField1" already set in map`,
				`line 20: key "hostPort" already set in map`,
				`unknown field "metadata.unknownMeta"`,
				`unknown field "spec.ports[0].unknownNested"`,
				`unknown field "spec.embeddedObj.metadata.unknownEmbeddedMeta"`,
			},
		},
		{
			name: "schemaless-crd-post-ignore-validation-yaml",
			opts: metav1.PatchOptions{
				FieldValidation: "Ignore",
			},
			body:        crdInvalidBodyYAML,
			contentType: "application/yaml",
		},
		{
			name:        "schemaless-crd-post-no-validation-yaml",
			body:        crdInvalidBodyYAML,
			contentType: "application/yaml",
			strictDecodingWarnings: []string{
				`line 6: key "name" already set in map`,
				`line 12: key "unknownDupe" already set in map`,
				`line 14: key "knownField1" already set in map`,
				`line 20: key "hostPort" already set in map`,
				`unknown field "metadata.unknownMeta"`,
				`unknown field "spec.ports[0].unknownNested"`,
				`unknown field "spec.embeddedObj.metadata.unknownEmbeddedMeta"`,
			},
		},
	}
	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {

			kind := gvk.Kind
			apiVersion := gvk.Group + "/" + gvk.Version

			// create the CR as specified by the test case
			jsonBody := []byte(fmt.Sprintf(tc.body, apiVersion, kind, tc.name))
			req := rest.Post().
				AbsPath("/apis", gvr.Group, gvr.Version, gvr.Resource).
				SetHeader("Content-Type", tc.contentType).
				VersionedParams(&tc.opts, metav1.ParameterCodec)
			result := req.Body([]byte(jsonBody)).Do(context.TODO())
			if result.Error() == nil && tc.strictDecodingError != "" {
				t.Fatalf("received nil error when expecting: %q", tc.strictDecodingError)
			}
			if result.Error() != nil && (tc.strictDecodingError == "" || !strings.HasSuffix(result.Error().Error(), tc.strictDecodingError)) {
				t.Fatalf("expected error: %q, got: %v", tc.strictDecodingError, result.Error())
			}

			if len(result.Warnings()) != len(tc.strictDecodingWarnings) {
				t.Logf("expected:")
				for _, w := range tc.strictDecodingWarnings {
					t.Logf("\t%v", w)
				}
				t.Logf("got:")
				for _, w := range result.Warnings() {
					t.Logf("\t%v", w.Text)
				}
				t.Fatalf("unexpected number of warnings, expected: %d, got: %d", len(tc.strictDecodingWarnings), len(result.Warnings()))
			}

			for i, strictWarn := range tc.strictDecodingWarnings {
				if strictWarn != result.Warnings()[i].Text {
					t.Fatalf("expected warning: %s, got warning: %s", strictWarn, result.Warnings()[i].Text)
				}

			}
		})
	}
}

// testFieldValidationPutCRD tests that server-side schema validation
// works for CRD update requests for CRDs with schemas.
func testFieldValidationPutCRD(t *testing.T, rest rest.Interface, gvk schema.GroupVersionKind, gvr schema.GroupVersionResource) {
	var testcases = []struct {
		name                   string
		opts                   metav1.PatchOptions
		putBody                string
		contentType            string
		strictDecodingError    string
		strictDecodingWarnings []string
	}{
		{
			name: "crd-put-strict-validation",
			opts: metav1.PatchOptions{
				FieldValidation: "Strict",
			},
			putBody:             crdInvalidBody,
			strictDecodingError: `strict decoding error: duplicate field "metadata.name", duplicate field "spec.unknownDupe", duplicate field "spec.knownField1", duplicate field "spec.ports[0].hostPort", unknown field "metadata.unknownMeta", unknown field "spec.ports[0].unknownNested", unknown field "spec.unknown1", unknown field "spec.unknownDupe", unknown field "spec.embeddedObj.metadata.unknownEmbeddedMeta"`,
		},
		{
			name: "crd-put-warn-validation",
			opts: metav1.PatchOptions{
				FieldValidation: "Warn",
			},
			putBody: crdInvalidBody,
			strictDecodingWarnings: []string{
				`duplicate field "metadata.name"`,
				`duplicate field "spec.unknownDupe"`,
				`duplicate field "spec.knownField1"`,
				`duplicate field "spec.ports[0].hostPort"`,
				`unknown field "metadata.unknownMeta"`,
				`unknown field "spec.ports[0].unknownNested"`,
				`unknown field "spec.unknown1"`,
				`unknown field "spec.unknownDupe"`,
				`unknown field "spec.embeddedObj.metadata.unknownEmbeddedMeta"`,
			},
		},
		{
			name: "crd-put-ignore-validation",
			opts: metav1.PatchOptions{
				FieldValidation: "Ignore",
			},
			putBody: crdInvalidBody,
		},
		{
			name:    "crd-put-no-validation",
			putBody: crdInvalidBody,
			strictDecodingWarnings: []string{
				`duplicate field "metadata.name"`,
				`duplicate field "spec.unknownDupe"`,
				`duplicate field "spec.knownField1"`,
				`duplicate field "spec.ports[0].hostPort"`,
				`unknown field "metadata.unknownMeta"`,
				`unknown field "spec.ports[0].unknownNested"`,
				`unknown field "spec.unknown1"`,
				`unknown field "spec.unknownDupe"`,
				`unknown field "spec.embeddedObj.metadata.unknownEmbeddedMeta"`,
			},
		},
		{
			name: "crd-put-strict-validation-yaml",
			opts: metav1.PatchOptions{
				FieldValidation: "Strict",
			},
			putBody:     crdInvalidBodyYAML,
			contentType: "application/yaml",
			strictDecodingError: `strict decoding error: yaml: unmarshal errors:
  line 6: key "name" already set in map
  line 12: key "unknownDupe" already set in map
  line 14: key "knownField1" already set in map
  line 20: key "hostPort" already set in map, unknown field "metadata.unknownMeta", unknown field "spec.ports[0].unknownNested", unknown field "spec.unknown1", unknown field "spec.unknownDupe", unknown field "spec.embeddedObj.metadata.unknownEmbeddedMeta"`,
		},
		{
			name: "crd-put-warn-validation-yaml",
			opts: metav1.PatchOptions{
				FieldValidation: "Warn",
			},
			putBody:     crdInvalidBodyYAML,
			contentType: "application/yaml",
			strictDecodingWarnings: []string{
				`line 6: key "name" already set in map`,
				`line 12: key "unknownDupe" already set in map`,
				`line 14: key "knownField1" already set in map`,
				`line 20: key "hostPort" already set in map`,
				`unknown field "metadata.unknownMeta"`,
				`unknown field "spec.ports[0].unknownNested"`,
				`unknown field "spec.unknown1"`,
				`unknown field "spec.unknownDupe"`,
				`unknown field "spec.embeddedObj.metadata.unknownEmbeddedMeta"`,
			},
		},
		{
			name: "crd-put-ignore-validation-yaml",
			opts: metav1.PatchOptions{
				FieldValidation: "Ignore",
			},
			putBody:     crdInvalidBodyYAML,
			contentType: "application/yaml",
		},
		{
			name:        "crd-put-no-validation-yaml",
			putBody:     crdInvalidBodyYAML,
			contentType: "application/yaml",
			strictDecodingWarnings: []string{
				`line 6: key "name" already set in map`,
				`line 12: key "unknownDupe" already set in map`,
				`line 14: key "knownField1" already set in map`,
				`line 20: key "hostPort" already set in map`,
				`unknown field "metadata.unknownMeta"`,
				`unknown field "spec.ports[0].unknownNested"`,
				`unknown field "spec.unknown1"`,
				`unknown field "spec.unknownDupe"`,
				`unknown field "spec.embeddedObj.metadata.unknownEmbeddedMeta"`,
			},
		},
	}
	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			kind := gvk.Kind
			apiVersion := gvk.Group + "/" + gvk.Version

			// create the CR as specified by the test case
			jsonPostBody := []byte(fmt.Sprintf(crdValidBody, apiVersion, kind, tc.name))
			postReq := rest.Post().
				AbsPath("/apis", gvr.Group, gvr.Version, gvr.Resource).
				VersionedParams(&tc.opts, metav1.ParameterCodec)
			postResult, err := postReq.Body([]byte(jsonPostBody)).Do(context.TODO()).Raw()
			if err != nil {
				t.Fatalf("unexpected error on CR creation: %v", err)
			}
			postUnstructured := &unstructured.Unstructured{}
			if err := postUnstructured.UnmarshalJSON(postResult); err != nil {
				t.Fatalf("unexpected error unmarshalling created CR: %v", err)
			}

			// update the CR as specified by the test case
			putBody := []byte(fmt.Sprintf(tc.putBody, apiVersion, kind, tc.name, postUnstructured.GetResourceVersion()))
			putReq := rest.Put().
				AbsPath("/apis", gvr.Group, gvr.Version, gvr.Resource).
				Name(tc.name).
				SetHeader("Content-Type", tc.contentType).
				VersionedParams(&tc.opts, metav1.ParameterCodec)
			result := putReq.Body([]byte(putBody)).Do(context.TODO())
			if result.Error() == nil && tc.strictDecodingError != "" {
				t.Fatalf("received nil error when expecting: %q", tc.strictDecodingError)
			}
			if result.Error() != nil && (tc.strictDecodingError == "" || !strings.HasSuffix(result.Error().Error(), tc.strictDecodingError)) {
				t.Fatalf("expected error: %q, got: %v", tc.strictDecodingError, result.Error())
			}

			if len(result.Warnings()) != len(tc.strictDecodingWarnings) {
				t.Fatalf("unexpected number of warnings, expected: %d, got: %d", len(tc.strictDecodingWarnings), len(result.Warnings()))
			}

			for i, strictWarn := range tc.strictDecodingWarnings {
				if strictWarn != result.Warnings()[i].Text {
					t.Fatalf("expected warning: %s, got warning: %s", strictWarn, result.Warnings()[i].Text)
				}

			}
		})
	}
}

// testFieldValidationPutCRDSchemaless tests that server-side schema validation
// works for CRD update requests for CRDs that have schemas
// with x-kubernetes-preserve-unknown-field set
func testFieldValidationPutCRDSchemaless(t *testing.T, rest rest.Interface, gvk schema.GroupVersionKind, gvr schema.GroupVersionResource) {
	var testcases = []struct {
		name                   string
		opts                   metav1.PatchOptions
		putBody                string
		contentType            string
		strictDecodingError    string
		strictDecodingWarnings []string
	}{
		{
			name: "schemaless-crd-put-strict-validation",
			opts: metav1.PatchOptions{
				FieldValidation: "Strict",
			},
			putBody:             crdInvalidBody,
			strictDecodingError: `strict decoding error: duplicate field "metadata.name", duplicate field "spec.unknownDupe", duplicate field "spec.knownField1", duplicate field "spec.ports[0].hostPort", unknown field "metadata.unknownMeta", unknown field "spec.ports[0].unknownNested", unknown field "spec.embeddedObj.metadata.unknownEmbeddedMeta"`,
		},
		{
			name: "schemaless-crd-put-warn-validation",
			opts: metav1.PatchOptions{
				FieldValidation: "Warn",
			},
			putBody: crdInvalidBody,
			strictDecodingWarnings: []string{
				`duplicate field "metadata.name"`,
				`duplicate field "spec.unknownDupe"`,
				`duplicate field "spec.knownField1"`,
				`duplicate field "spec.ports[0].hostPort"`,
				`unknown field "metadata.unknownMeta"`,
				`unknown field "spec.ports[0].unknownNested"`,
				`unknown field "spec.embeddedObj.metadata.unknownEmbeddedMeta"`,
			},
		},
		{
			name: "schemaless-crd-put-ignore-validation",
			opts: metav1.PatchOptions{
				FieldValidation: "Ignore",
			},
			putBody: crdInvalidBody,
		},
		{
			name:    "schemaless-crd-put-no-validation",
			putBody: crdInvalidBody,
			strictDecodingWarnings: []string{
				`duplicate field "metadata.name"`,
				`duplicate field "spec.unknownDupe"`,
				`duplicate field "spec.knownField1"`,
				`duplicate field "spec.ports[0].hostPort"`,
				`unknown field "metadata.unknownMeta"`,
				`unknown field "spec.ports[0].unknownNested"`,
				`unknown field "spec.embeddedObj.metadata.unknownEmbeddedMeta"`,
			},
		},
		{
			name: "schemaless-crd-put-strict-validation-yaml",
			opts: metav1.PatchOptions{
				FieldValidation: "Strict",
			},
			putBody:     crdInvalidBodyYAML,
			contentType: "application/yaml",
			strictDecodingError: `strict decoding error: yaml: unmarshal errors:
  line 6: key "name" already set in map
  line 12: key "unknownDupe" already set in map
  line 14: key "knownField1" already set in map
  line 20: key "hostPort" already set in map, unknown field "metadata.unknownMeta", unknown field "spec.ports[0].unknownNested", unknown field "spec.embeddedObj.metadata.unknownEmbeddedMeta"`,
		},
		{
			name: "schemaless-crd-put-warn-validation-yaml",
			opts: metav1.PatchOptions{
				FieldValidation: "Warn",
			},
			putBody:     crdInvalidBodyYAML,
			contentType: "application/yaml",
			strictDecodingWarnings: []string{
				`line 6: key "name" already set in map`,
				`line 12: key "unknownDupe" already set in map`,
				`line 14: key "knownField1" already set in map`,
				`line 20: key "hostPort" already set in map`,
				`unknown field "metadata.unknownMeta"`,
				`unknown field "spec.ports[0].unknownNested"`,
				`unknown field "spec.embeddedObj.metadata.unknownEmbeddedMeta"`,
			},
		},
		{
			name: "schemaless-crd-put-ignore-validation-yaml",
			opts: metav1.PatchOptions{
				FieldValidation: "Ignore",
			},
			putBody:     crdInvalidBodyYAML,
			contentType: "application/yaml",
		},
		{
			name:        "schemaless-crd-put-no-validation-yaml",
			putBody:     crdInvalidBodyYAML,
			contentType: "application/yaml",
			strictDecodingWarnings: []string{
				`line 6: key "name" already set in map`,
				`line 12: key "unknownDupe" already set in map`,
				`line 14: key "knownField1" already set in map`,
				`line 20: key "hostPort" already set in map`,
				`unknown field "metadata.unknownMeta"`,
				`unknown field "spec.ports[0].unknownNested"`,
				`unknown field "spec.embeddedObj.metadata.unknownEmbeddedMeta"`,
			},
		},
	}
	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			kind := gvk.Kind
			apiVersion := gvk.Group + "/" + gvk.Version

			// create the CR as specified by the test case
			jsonPostBody := []byte(fmt.Sprintf(crdValidBody, apiVersion, kind, tc.name))
			postReq := rest.Post().
				AbsPath("/apis", gvr.Group, gvr.Version, gvr.Resource).
				VersionedParams(&tc.opts, metav1.ParameterCodec)
			postResult, err := postReq.Body([]byte(jsonPostBody)).Do(context.TODO()).Raw()
			if err != nil {
				t.Fatalf("unexpected error on CR creation: %v", err)
			}
			postUnstructured := &unstructured.Unstructured{}
			if err := postUnstructured.UnmarshalJSON(postResult); err != nil {
				t.Fatalf("unexpected error unmarshalling created CR: %v", err)
			}

			// update the CR as specified by the test case
			putBody := []byte(fmt.Sprintf(tc.putBody, apiVersion, kind, tc.name, postUnstructured.GetResourceVersion()))
			putReq := rest.Put().
				AbsPath("/apis", gvr.Group, gvr.Version, gvr.Resource).
				Name(tc.name).
				SetHeader("Content-Type", tc.contentType).
				VersionedParams(&tc.opts, metav1.ParameterCodec)
			result := putReq.Body([]byte(putBody)).Do(context.TODO())
			if result.Error() == nil && tc.strictDecodingError != "" {
				t.Fatalf("received nil error when expecting: %q", tc.strictDecodingError)
			}
			if result.Error() != nil && (tc.strictDecodingError == "" || !strings.HasSuffix(result.Error().Error(), tc.strictDecodingError)) {
				t.Fatalf("expected error: %q, got: %v", tc.strictDecodingError, result.Error())
			}

			if len(result.Warnings()) != len(tc.strictDecodingWarnings) {
				t.Logf("expected:")
				for _, w := range tc.strictDecodingWarnings {
					t.Logf("\t%v", w)
				}
				t.Logf("got:")
				for _, w := range result.Warnings() {
					t.Logf("\t%v", w.Text)
				}
				t.Fatalf("unexpected number of warnings, expected: %d, got: %d", len(tc.strictDecodingWarnings), len(result.Warnings()))
			}

			for i, strictWarn := range tc.strictDecodingWarnings {
				if strictWarn != result.Warnings()[i].Text {
					t.Fatalf("expected warning: %s, got warning: %s", strictWarn, result.Warnings()[i].Text)
				}

			}
		})
	}
}

// testFieldValidationPatchCRD tests that server-side schema validation
// works for jsonpatch and mergepatch requests
// for custom resources that have schemas.
func testFieldValidationPatchCRD(t *testing.T, rest rest.Interface, gvk schema.GroupVersionKind, gvr schema.GroupVersionResource) {
	patchYAMLBody := `
apiVersion: %s
kind: %s
metadata:
  name: %s
  finalizers:
  - test/finalizer
spec:
  cronSpec: "* * * * */5"
  ports:
  - name: x
    containerPort: 80
    protocol: TCP`

	mergePatchBody := `
{
	"spec": {
		"unknown1": "val1",
		"unknownDupe": "valDupe",
		"unknownDupe": "valDupe2",
		"knownField1": "val1",
		"knownField1": "val2",
			"ports": [{
				"name": "portName",
				"containerPort": 8080,
				"protocol": "TCP",
				"hostPort": 8081,
				"hostPort": 8082,
				"unknownNested": "val"
			}]
	}
}
	`
	jsonPatchBody := `
			[
				{"op": "add", "path": "/spec/unknown1", "value": "val1", "foo": "bar"},
				{"op": "add", "path": "/spec/unknown2", "path": "/spec/unknown3", "value": "val2"},
				{"op": "add", "path": "/spec/unknownDupe", "value": "valDupe"},
				{"op": "add", "path": "/spec/unknownDupe", "value": "valDupe2"},
				{"op": "add", "path": "/spec/knownField1", "value": "val1"},
				{"op": "add", "path": "/spec/knownField1", "value": "val2"},
				{"op": "add", "path": "/spec/ports/0/name", "value": "portName"},
				{"op": "add", "path": "/spec/ports/0/containerPort", "value": 8080},
				{"op": "add", "path": "/spec/ports/0/protocol", "value": "TCP"},
				{"op": "add", "path": "/spec/ports/0/hostPort", "value": 8081},
				{"op": "add", "path": "/spec/ports/0/hostPort", "value": 8082},
				{"op": "add", "path": "/spec/ports/0/unknownNested", "value": "val"}
			]
			`
	var testcases = []struct {
		name                   string
		patchType              types.PatchType
		opts                   metav1.PatchOptions
		body                   string
		strictDecodingError    string
		strictDecodingWarnings []string
	}{
		{
			name:      "crd-merge-patch-strict-validation",
			patchType: types.MergePatchType,
			opts: metav1.PatchOptions{
				FieldValidation: "Strict",
			},
			body:                mergePatchBody,
			strictDecodingError: `strict decoding error: duplicate field "spec.unknownDupe", duplicate field "spec.knownField1", duplicate field "spec.ports[0].hostPort", unknown field "spec.ports[0].unknownNested", unknown field "spec.unknown1", unknown field "spec.unknownDupe"`,
		},
		{
			name:      "crd-merge-patch-warn-validation",
			patchType: types.MergePatchType,
			opts: metav1.PatchOptions{
				FieldValidation: "Warn",
			},
			body: mergePatchBody,
			strictDecodingWarnings: []string{
				`duplicate field "spec.unknownDupe"`,
				`duplicate field "spec.knownField1"`,
				`duplicate field "spec.ports[0].hostPort"`,
				`unknown field "spec.ports[0].unknownNested"`,
				`unknown field "spec.unknown1"`,
				`unknown field "spec.unknownDupe"`,
			},
		},
		{
			name:      "crd-merge-patch-ignore-validation",
			patchType: types.MergePatchType,
			opts: metav1.PatchOptions{
				FieldValidation: "Ignore",
			},
			body: mergePatchBody,
		},
		{
			name:      "crd-merge-patch-no-validation",
			patchType: types.MergePatchType,
			body:      mergePatchBody,
			strictDecodingWarnings: []string{
				`duplicate field "spec.unknownDupe"`,
				`duplicate field "spec.knownField1"`,
				`duplicate field "spec.ports[0].hostPort"`,
				`unknown field "spec.ports[0].unknownNested"`,
				`unknown field "spec.unknown1"`,
				`unknown field "spec.unknownDupe"`,
			},
		},
		{
			name:      "crd-json-patch-strict-validation",
			patchType: types.JSONPatchType,
			opts: metav1.PatchOptions{
				FieldValidation: "Strict",
			},
			body: jsonPatchBody,
			// note: duplicate fields in the patch itself
			// are dropped by the
			// evanphx/json-patch library and is expected.
			// Duplicate fields in the json patch ops
			// themselves can be detected though
			strictDecodingError: `strict decoding error: json patch unknown field "[0].foo", json patch duplicate field "[1].path", unknown field "spec.ports[0].unknownNested", unknown field "spec.unknown1", unknown field "spec.unknown3", unknown field "spec.unknownDupe"`,
		},
		{
			name:      "crd-json-patch-warn-validation",
			patchType: types.JSONPatchType,
			opts: metav1.PatchOptions{
				FieldValidation: "Warn",
			},
			body: jsonPatchBody,
			strictDecodingWarnings: []string{
				// note: duplicate fields in the patch itself
				// are dropped by the
				// evanphx/json-patch library and is expected.
				// Duplicate fields in the json patch ops
				// themselves can be detected though
				`json patch unknown field "[0].foo"`,
				`json patch duplicate field "[1].path"`,
				`unknown field "spec.ports[0].unknownNested"`,
				`unknown field "spec.unknown1"`,
				`unknown field "spec.unknown3"`,
				`unknown field "spec.unknownDupe"`,
			},
		},
		{
			name:      "crd-json-patch-ignore-validation",
			patchType: types.JSONPatchType,
			opts: metav1.PatchOptions{
				FieldValidation: "Ignore",
			},
			body: jsonPatchBody,
		},
		{
			name:      "crd-json-patch-no-validation",
			patchType: types.JSONPatchType,
			body:      jsonPatchBody,
			strictDecodingWarnings: []string{
				// note: duplicate fields in the patch itself
				// are dropped by the
				// evanphx/json-patch library and is expected.
				// Duplicate fields in the json patch ops
				// themselves can be detected though
				`json patch unknown field "[0].foo"`,
				`json patch duplicate field "[1].path"`,
				`unknown field "spec.ports[0].unknownNested"`,
				`unknown field "spec.unknown1"`,
				`unknown field "spec.unknown3"`,
				`unknown field "spec.unknownDupe"`,
			},
		},
	}
	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			kind := gvk.Kind
			apiVersion := gvk.Group + "/" + gvk.Version
			// create a CR
			yamlBody := []byte(fmt.Sprintf(string(patchYAMLBody), apiVersion, kind, tc.name))
			createResult, err := rest.Patch(types.ApplyPatchType).
				AbsPath("/apis", gvr.Group, gvr.Version, gvr.Resource).
				Name(tc.name).
				Param("fieldManager", "apply_test").
				Body(yamlBody).
				DoRaw(context.TODO())
			if err != nil {
				t.Fatalf("failed to create custom resource with apply: %v:\n%v", err, string(createResult))
			}

			// patch the CR as specified by the test case
			req := rest.Patch(tc.patchType).
				AbsPath("/apis", gvr.Group, gvr.Version, gvr.Resource).
				Name(tc.name).
				VersionedParams(&tc.opts, metav1.ParameterCodec)
			result := req.Body([]byte(tc.body)).Do(context.TODO())
			if result.Error() == nil && tc.strictDecodingError != "" {
				t.Fatalf("received nil error when expecting: %q", tc.strictDecodingError)
			}
			if result.Error() != nil && (tc.strictDecodingError == "" || !strings.HasSuffix(result.Error().Error(), tc.strictDecodingError)) {
				t.Fatalf("expected error: %q, got: %v", tc.strictDecodingError, result.Error())
			}

			if len(result.Warnings()) != len(tc.strictDecodingWarnings) {
				t.Fatalf("unexpected number of warnings, expected: %d, got: %d", len(tc.strictDecodingWarnings), len(result.Warnings()))
			}

			for i, strictWarn := range tc.strictDecodingWarnings {
				if strictWarn != result.Warnings()[i].Text {
					t.Fatalf("expected warning: %s, got warning: %s", strictWarn, result.Warnings()[i].Text)
				}

			}
		})
	}
}

// testFieldValidationPatchCRDSchemaless tests that server-side schema validation
// works for jsonpatch and mergepatch requests
// for custom resources that have schemas
// with x-kubernetes-preserve-unknown-field set
func testFieldValidationPatchCRDSchemaless(t *testing.T, rest rest.Interface, gvk schema.GroupVersionKind, gvr schema.GroupVersionResource) {
	mergePatchBody := `
{
	"spec": {
		"unknown1": "val1",
		"unknownDupe": "valDupe",
		"unknownDupe": "valDupe2",
		"knownField1": "val1",
		"knownField1": "val2",
			"ports": [{
				"name": "portName",
				"containerPort": 8080,
				"protocol": "TCP",
				"hostPort": 8081,
				"hostPort": 8082,
				"unknownNested": "val"
			}]
	}
}
	`
	jsonPatchBody := `
			[
				{"op": "add", "path": "/spec/unknown1", "value": "val1", "foo": "bar"},
				{"op": "add", "path": "/spec/unknown2", "path": "/spec/unknown3", "value": "val2"},
				{"op": "add", "path": "/spec/unknownDupe", "value": "valDupe"},
				{"op": "add", "path": "/spec/unknownDupe", "value": "valDupe2"},
				{"op": "add", "path": "/spec/knownField1", "value": "val1"},
				{"op": "add", "path": "/spec/knownField1", "value": "val2"},
				{"op": "add", "path": "/spec/ports/0/name", "value": "portName"},
				{"op": "add", "path": "/spec/ports/0/containerPort", "value": 8080},
				{"op": "add", "path": "/spec/ports/0/protocol", "value": "TCP"},
				{"op": "add", "path": "/spec/ports/0/hostPort", "value": 8081},
				{"op": "add", "path": "/spec/ports/0/hostPort", "value": 8082},
				{"op": "add", "path": "/spec/ports/0/unknownNested", "value": "val"}
			]
			`
	var testcases = []struct {
		name                   string
		patchType              types.PatchType
		opts                   metav1.PatchOptions
		body                   string
		strictDecodingError    string
		strictDecodingWarnings []string
	}{
		{
			name:      "schemaless-crd-merge-patch-strict-validation",
			patchType: types.MergePatchType,
			opts: metav1.PatchOptions{
				FieldValidation: "Strict",
			},
			body:                mergePatchBody,
			strictDecodingError: `strict decoding error: duplicate field "spec.unknownDupe", duplicate field "spec.knownField1", duplicate field "spec.ports[0].hostPort", unknown field "spec.ports[0].unknownNested"`,
		},
		{
			name:      "schemaless-crd-merge-patch-warn-validation",
			patchType: types.MergePatchType,
			opts: metav1.PatchOptions{
				FieldValidation: "Warn",
			},
			body: mergePatchBody,
			strictDecodingWarnings: []string{
				`duplicate field "spec.unknownDupe"`,
				`duplicate field "spec.knownField1"`,
				`duplicate field "spec.ports[0].hostPort"`,
				`unknown field "spec.ports[0].unknownNested"`,
			},
		},
		{
			name:      "schemaless-crd-merge-patch-ignore-validation",
			patchType: types.MergePatchType,
			opts: metav1.PatchOptions{
				FieldValidation: "Ignore",
			},
			body: mergePatchBody,
		},
		{
			name:      "schemaless-crd-merge-patch-no-validation",
			patchType: types.MergePatchType,
			body:      mergePatchBody,
			strictDecodingWarnings: []string{
				`duplicate field "spec.unknownDupe"`,
				`duplicate field "spec.knownField1"`,
				`duplicate field "spec.ports[0].hostPort"`,
				`unknown field "spec.ports[0].unknownNested"`,
			},
		},
		{
			name:      "schemaless-crd-json-patch-strict-validation",
			patchType: types.JSONPatchType,
			opts: metav1.PatchOptions{
				FieldValidation: "Strict",
			},
			body: jsonPatchBody,
			// note: duplicate fields in the patch itself
			// are dropped by the
			// evanphx/json-patch library and is expected.
			// Duplicate fields in the json patch ops
			// themselves can be detected though
			strictDecodingError: `strict decoding error: json patch unknown field "[0].foo", json patch duplicate field "[1].path", unknown field "spec.ports[0].unknownNested"`,
		},
		{
			name:      "schemaless-crd-json-patch-warn-validation",
			patchType: types.JSONPatchType,
			opts: metav1.PatchOptions{
				FieldValidation: "Warn",
			},
			body: jsonPatchBody,
			strictDecodingWarnings: []string{
				// note: duplicate fields in the patch itself
				// are dropped by the
				// evanphx/json-patch library and is expected.
				// Duplicate fields in the json patch ops
				// themselves can be detected though
				`json patch unknown field "[0].foo"`,
				`json patch duplicate field "[1].path"`,
				`unknown field "spec.ports[0].unknownNested"`,
			},
		},
		{
			name:      "schemaless-crd-json-patch-ignore-validation",
			patchType: types.JSONPatchType,
			opts: metav1.PatchOptions{
				FieldValidation: "Ignore",
			},
			body: jsonPatchBody,
		},
		{
			name:      "schemaless-crd-json-patch-no-validation",
			patchType: types.JSONPatchType,
			body:      jsonPatchBody,
			strictDecodingWarnings: []string{
				// note: duplicate fields in the patch itself
				// are dropped by the
				// evanphx/json-patch library and is expected.
				// Duplicate fields in the json patch ops
				// themselves can be detected though
				`json patch unknown field "[0].foo"`,
				`json patch duplicate field "[1].path"`,
				`unknown field "spec.ports[0].unknownNested"`,
			},
		},
	}
	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			kind := gvk.Kind
			apiVersion := gvk.Group + "/" + gvk.Version
			// create a CR
			yamlBody := []byte(fmt.Sprintf(string(patchYAMLBody), apiVersion, kind, tc.name))
			createResult, err := rest.Patch(types.ApplyPatchType).
				AbsPath("/apis", gvr.Group, gvr.Version, gvr.Resource).
				Name(tc.name).
				Param("fieldManager", "apply_test").
				Body(yamlBody).
				DoRaw(context.TODO())
			if err != nil {
				t.Fatalf("failed to create custom resource with apply: %v:\n%v", err, string(createResult))
			}

			// patch the CR as specified by the test case
			req := rest.Patch(tc.patchType).
				AbsPath("/apis", gvr.Group, gvr.Version, gvr.Resource).
				Name(tc.name).
				VersionedParams(&tc.opts, metav1.ParameterCodec)
			result := req.Body([]byte(tc.body)).Do(context.TODO())
			if result.Error() == nil && tc.strictDecodingError != "" {
				t.Fatalf("received nil error when expecting: %q", tc.strictDecodingError)
			}
			if result.Error() != nil && (tc.strictDecodingError == "" || !strings.HasSuffix(result.Error().Error(), tc.strictDecodingError)) {
				t.Fatalf("expected error: %q, got: %v", tc.strictDecodingError, result.Error())
			}

			if len(result.Warnings()) != len(tc.strictDecodingWarnings) {
				t.Fatalf("unexpected number of warnings, expected: %d, got: %d", len(tc.strictDecodingWarnings), len(result.Warnings()))
			}

			for i, strictWarn := range tc.strictDecodingWarnings {
				if strictWarn != result.Warnings()[i].Text {
					t.Fatalf("expected warning: %s, got warning: %s", strictWarn, result.Warnings()[i].Text)
				}

			}
		})
	}
}

// testFieldValidationApplyCreateCRD tests apply patch requests containing duplicate fields
// on newly created objects, for CRDs that have schemas
// Note that even prior to server-side validation, unknown fields were treated as
// errors in apply-patch and are not tested here.
func testFieldValidationApplyCreateCRD(t *testing.T, rest rest.Interface, gvk schema.GroupVersionKind, gvr schema.GroupVersionResource) {
	var testcases = []struct {
		name                   string
		opts                   metav1.PatchOptions
		strictDecodingError    string
		strictDecodingWarnings []string
	}{
		{
			name: "strict-validation",
			opts: metav1.PatchOptions{
				FieldValidation: "Strict",
				FieldManager:    "mgr",
			},
			strictDecodingError: `error strict decoding YAML: error converting YAML to JSON: yaml: unmarshal errors:
  line 10: key "knownField1" already set in map
  line 16: key "hostPort" already set in map`,
		},
		{
			name: "warn-validation",
			opts: metav1.PatchOptions{
				FieldValidation: "Warn",
				FieldManager:    "mgr",
			},
			strictDecodingWarnings: []string{
				`line 10: key "knownField1" already set in map`,
				`line 16: key "hostPort" already set in map`,
			},
		},
		{
			name: "ignore-validation",
			opts: metav1.PatchOptions{
				FieldValidation: "Ignore",
				FieldManager:    "mgr",
			},
		},
		{
			name: "no-validation",
			opts: metav1.PatchOptions{
				FieldManager: "mgr",
			},
			strictDecodingWarnings: []string{
				`line 10: key "knownField1" already set in map`,
				`line 16: key "hostPort" already set in map`,
			},
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			kind := gvk.Kind
			apiVersion := gvk.Group + "/" + gvk.Version

			// create the CR as specified by the test case
			name := fmt.Sprintf("apply-create-crd-%s", tc.name)
			applyCreateBody := []byte(fmt.Sprintf(crdApplyInvalidBody, apiVersion, kind, name))

			req := rest.Patch(types.ApplyPatchType).
				AbsPath("/apis", gvr.Group, gvr.Version, gvr.Resource).
				Name(name).
				VersionedParams(&tc.opts, metav1.ParameterCodec)
			result := req.Body(applyCreateBody).Do(context.TODO())
			if result.Error() == nil && tc.strictDecodingError != "" {
				t.Fatalf("received nil error when expecting: %q", tc.strictDecodingError)
			}
			if result.Error() != nil && (tc.strictDecodingError == "" || !strings.HasSuffix(result.Error().Error(), tc.strictDecodingError)) {
				t.Fatalf("expected error: %q, got: %v", tc.strictDecodingError, result.Error())
			}

			if len(result.Warnings()) != len(tc.strictDecodingWarnings) {
				t.Fatalf("unexpected number of warnings, expected: %d, got: %d", len(tc.strictDecodingWarnings), len(result.Warnings()))
			}
			for i, strictWarn := range tc.strictDecodingWarnings {
				if strictWarn != result.Warnings()[i].Text {
					t.Fatalf("expected warning: %s, got warning: %s", strictWarn, result.Warnings()[i].Text)
				}

			}
		})
	}
}

// testFieldValidationApplyCreateCRDSchemaless tests apply patch requests containing duplicate fields
// on newly created objects, for CRDs that have schemas
// with x-kubernetes-preserve-unknown-field set
// Note that even prior to server-side validation, unknown fields were treated as
// errors in apply-patch and are not tested here.
func testFieldValidationApplyCreateCRDSchemaless(t *testing.T, rest rest.Interface, gvk schema.GroupVersionKind, gvr schema.GroupVersionResource) {
	var testcases = []struct {
		name                   string
		opts                   metav1.PatchOptions
		strictDecodingError    string
		strictDecodingWarnings []string
	}{
		{
			name: "schemaless-strict-validation",
			opts: metav1.PatchOptions{
				FieldValidation: "Strict",
				FieldManager:    "mgr",
			},
			strictDecodingError: `error strict decoding YAML: error converting YAML to JSON: yaml: unmarshal errors:
  line 10: key "knownField1" already set in map
  line 16: key "hostPort" already set in map`,
		},
		{
			name: "schemaless-warn-validation",
			opts: metav1.PatchOptions{
				FieldValidation: "Warn",
				FieldManager:    "mgr",
			},
			strictDecodingWarnings: []string{
				`line 10: key "knownField1" already set in map`,
				`line 16: key "hostPort" already set in map`,
			},
		},
		{
			name: "schemaless-ignore-validation",
			opts: metav1.PatchOptions{
				FieldValidation: "Ignore",
				FieldManager:    "mgr",
			},
		},
		{
			name: "schemaless-no-validation",
			opts: metav1.PatchOptions{
				FieldManager: "mgr",
			},
			strictDecodingWarnings: []string{
				`line 10: key "knownField1" already set in map`,
				`line 16: key "hostPort" already set in map`,
			},
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			kind := gvk.Kind
			apiVersion := gvk.Group + "/" + gvk.Version

			// create the CR as specified by the test case
			name := fmt.Sprintf("apply-create-crd-schemaless-%s", tc.name)
			applyCreateBody := []byte(fmt.Sprintf(crdApplyInvalidBody, apiVersion, kind, name))

			req := rest.Patch(types.ApplyPatchType).
				AbsPath("/apis", gvr.Group, gvr.Version, gvr.Resource).
				Name(name).
				VersionedParams(&tc.opts, metav1.ParameterCodec)
			result := req.Body(applyCreateBody).Do(context.TODO())
			if result.Error() == nil && tc.strictDecodingError != "" {
				t.Fatalf("received nil error when expecting: %q", tc.strictDecodingError)
			}
			if result.Error() != nil && (tc.strictDecodingError == "" || !strings.HasSuffix(result.Error().Error(), tc.strictDecodingError)) {
				t.Fatalf("expected error: %q, got: %v", tc.strictDecodingError, result.Error())
			}

			if len(result.Warnings()) != len(tc.strictDecodingWarnings) {
				t.Fatalf("unexpected number of warnings, expected: %d, got: %d", len(tc.strictDecodingWarnings), len(result.Warnings()))
			}
			for i, strictWarn := range tc.strictDecodingWarnings {
				if strictWarn != result.Warnings()[i].Text {
					t.Fatalf("expected warning: %s, got warning: %s", strictWarn, result.Warnings()[i].Text)
				}

			}
		})
	}
}

// testFieldValidationApplyUpdateCRD tests apply patch requests containing duplicate fields
// on existing objects, for CRDs with schemas
// Note that even prior to server-side validation, unknown fields were treated as
// errors in apply-patch and are not tested here.
func testFieldValidationApplyUpdateCRD(t *testing.T, rest rest.Interface, gvk schema.GroupVersionKind, gvr schema.GroupVersionResource) {
	var testcases = []struct {
		name                   string
		opts                   metav1.PatchOptions
		strictDecodingError    string
		strictDecodingWarnings []string
	}{
		{
			name: "strict-validation",
			opts: metav1.PatchOptions{
				FieldValidation: "Strict",
				FieldManager:    "mgr",
			},
			strictDecodingError: `error strict decoding YAML: error converting YAML to JSON: yaml: unmarshal errors:
  line 10: key "knownField1" already set in map
  line 16: key "hostPort" already set in map`,
		},
		{
			name: "warn-validation",
			opts: metav1.PatchOptions{
				FieldValidation: "Warn",
				FieldManager:    "mgr",
			},
			strictDecodingWarnings: []string{
				`line 10: key "knownField1" already set in map`,
				`line 16: key "hostPort" already set in map`,
			},
		},
		{
			name: "ignore-validation",
			opts: metav1.PatchOptions{
				FieldValidation: "Ignore",
				FieldManager:    "mgr",
			},
		},
		{
			name: "no-validation",
			opts: metav1.PatchOptions{
				FieldManager: "mgr",
			},
			strictDecodingWarnings: []string{
				`line 10: key "knownField1" already set in map`,
				`line 16: key "hostPort" already set in map`,
			},
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			kind := gvk.Kind
			apiVersion := gvk.Group + "/" + gvk.Version

			// create the CR as specified by the test case
			name := fmt.Sprintf("apply-update-crd-%s", tc.name)
			applyCreateBody := []byte(fmt.Sprintf(crdApplyValidBody, apiVersion, kind, name))
			createReq := rest.Patch(types.ApplyPatchType).
				AbsPath("/apis", gvr.Group, gvr.Version, gvr.Resource).
				Name(name).
				VersionedParams(&tc.opts, metav1.ParameterCodec)
			createResult := createReq.Body(applyCreateBody).Do(context.TODO())
			if createResult.Error() != nil {
				t.Fatalf("unexpected apply create err: %v", createResult.Error())
			}

			applyUpdateBody := []byte(fmt.Sprintf(crdApplyInvalidBody, apiVersion, kind, name))
			updateReq := rest.Patch(types.ApplyPatchType).
				AbsPath("/apis", gvr.Group, gvr.Version, gvr.Resource).
				Name(name).
				VersionedParams(&tc.opts, metav1.ParameterCodec)
			result := updateReq.Body(applyUpdateBody).Do(context.TODO())
			if result.Error() == nil && tc.strictDecodingError != "" {
				t.Fatalf("received nil error when expecting: %q", tc.strictDecodingError)
			}
			if result.Error() != nil && (tc.strictDecodingError == "" || !strings.HasSuffix(result.Error().Error(), tc.strictDecodingError)) {
				t.Fatalf("expected error: %q, got: %v", tc.strictDecodingError, result.Error())
			}

			if len(result.Warnings()) != len(tc.strictDecodingWarnings) {
				t.Fatalf("unexpected number of warnings, expected: %d, got: %d", len(tc.strictDecodingWarnings), len(result.Warnings()))
			}
			for i, strictWarn := range tc.strictDecodingWarnings {
				if strictWarn != result.Warnings()[i].Text {
					t.Fatalf("expected warning: %s, got warning: %s", strictWarn, result.Warnings()[i].Text)
				}

			}
		})
	}
}

// testFieldValidationApplyUpdateCRDSchemaless tests apply patch requests containing duplicate fields
// on existing objects, for CRDs with schemas
// with x-kubernetes-preserve-unknown-field set
// Note that even prior to server-side validation, unknown fields were treated as
// errors in apply-patch and are not tested here.
func testFieldValidationApplyUpdateCRDSchemaless(t *testing.T, rest rest.Interface, gvk schema.GroupVersionKind, gvr schema.GroupVersionResource) {
	var testcases = []struct {
		name                   string
		opts                   metav1.PatchOptions
		strictDecodingError    string
		strictDecodingWarnings []string
	}{
		{
			name: "schemaless-strict-validation",
			opts: metav1.PatchOptions{
				FieldValidation: "Strict",
				FieldManager:    "mgr",
			},
			strictDecodingError: `error strict decoding YAML: error converting YAML to JSON: yaml: unmarshal errors:
  line 10: key "knownField1" already set in map
  line 16: key "hostPort" already set in map`,
		},
		{
			name: "schemaless-warn-validation",
			opts: metav1.PatchOptions{
				FieldValidation: "Warn",
				FieldManager:    "mgr",
			},
			strictDecodingWarnings: []string{
				`line 10: key "knownField1" already set in map`,
				`line 16: key "hostPort" already set in map`,
			},
		},
		{
			name: "schemaless-ignore-validation",
			opts: metav1.PatchOptions{
				FieldValidation: "Ignore",
				FieldManager:    "mgr",
			},
		},
		{
			name: "schemaless-no-validation",
			opts: metav1.PatchOptions{
				FieldManager: "mgr",
			},
			strictDecodingWarnings: []string{
				`line 10: key "knownField1" already set in map`,
				`line 16: key "hostPort" already set in map`,
			},
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			kind := gvk.Kind
			apiVersion := gvk.Group + "/" + gvk.Version

			// create the CR as specified by the test case
			name := fmt.Sprintf("apply-update-crd-schemaless-%s", tc.name)
			applyCreateBody := []byte(fmt.Sprintf(crdApplyValidBody, apiVersion, kind, name))
			createReq := rest.Patch(types.ApplyPatchType).
				AbsPath("/apis", gvr.Group, gvr.Version, gvr.Resource).
				Name(name).
				VersionedParams(&tc.opts, metav1.ParameterCodec)
			createResult := createReq.Body(applyCreateBody).Do(context.TODO())
			if createResult.Error() != nil {
				t.Fatalf("unexpected apply create err: %v", createResult.Error())
			}

			applyUpdateBody := []byte(fmt.Sprintf(crdApplyInvalidBody, apiVersion, kind, name))
			updateReq := rest.Patch(types.ApplyPatchType).
				AbsPath("/apis", gvr.Group, gvr.Version, gvr.Resource).
				Name(name).
				VersionedParams(&tc.opts, metav1.ParameterCodec)
			result := updateReq.Body(applyUpdateBody).Do(context.TODO())

			if result.Error() == nil && tc.strictDecodingError != "" {
				t.Fatalf("received nil error when expecting: %q", tc.strictDecodingError)
			}
			if result.Error() != nil && (tc.strictDecodingError == "" || !strings.HasSuffix(result.Error().Error(), tc.strictDecodingError)) {
				t.Fatalf("expected error: %q, got: %v", tc.strictDecodingError, result.Error())
			}

			if len(result.Warnings()) != len(tc.strictDecodingWarnings) {
				t.Fatalf("unexpected number of warnings, expected: %d, got: %d", len(tc.strictDecodingWarnings), len(result.Warnings()))
			}
			for i, strictWarn := range tc.strictDecodingWarnings {
				if strictWarn != result.Warnings()[i].Text {
					t.Fatalf("expected warning: %s, got warning: %s", strictWarn, result.Warnings()[i].Text)
				}

			}
		})
	}
}

func testFinalizerValidationApplyCreateAndUpdateCRD(t *testing.T, rest rest.Interface, gvk schema.GroupVersionKind, gvr schema.GroupVersionResource) {
	var testcases = []struct {
		name                 string
		finalizer            []string
		updatedFinalizer     []string
		opts                 metav1.PatchOptions
		expectUpdateWarnings []string
		expectCreateWarnings []string
	}{
		{
			name:      "create-crd-with-invalid-finalizer",
			finalizer: []string{"invalid-finalizer"},
			expectCreateWarnings: []string{
				`metadata.finalizers: "invalid-finalizer": prefer a domain-qualified finalizer name to avoid accidental conflicts with other finalizer writers`,
			},
		},
		{
			name:      "create-crd-with-valid-finalizer",
			finalizer: []string{"kubernetes.io/valid-finalizer"},
		},
		{
			name:      "create-crd-with-fqdn-like-finalizer-without-path",
			finalizer: []string{"finalizer.without.valid-path.io"},
			expectCreateWarnings: []string{
				`metadata.finalizers: "finalizer.without.valid-path.io": prefer a domain-qualified finalizer name including a path (/) to avoid accidental conflicts with other finalizer writers`,
			},
		},
		{
			name:             "update-crd-with-invalid-finalizer",
			finalizer:        []string{"invalid-finalizer"},
			updatedFinalizer: []string{"another-invalid-finalizer"},
			expectCreateWarnings: []string{
				`metadata.finalizers: "invalid-finalizer": prefer a domain-qualified finalizer name to avoid accidental conflicts with other finalizer writers`,
			},
			expectUpdateWarnings: []string{
				`metadata.finalizers: "another-invalid-finalizer": prefer a domain-qualified finalizer name to avoid accidental conflicts with other finalizer writers`,
			},
		},
		{
			name:             "update-crd-with-valid-finalizer",
			finalizer:        []string{"kubernetes.io/valid-finalizer"},
			updatedFinalizer: []string{"kubernetes.io/another-valid-finalizer"},
		},
		{
			name:             "update-crd-with-valid-finalizer-leaving-an-existing-invalid-finalizer",
			finalizer:        []string{"invalid-finalizer"},
			updatedFinalizer: []string{"kubernetes.io/another-valid-finalizer"},
			expectCreateWarnings: []string{
				`metadata.finalizers: "invalid-finalizer": prefer a domain-qualified finalizer name to avoid accidental conflicts with other finalizer writers`,
			},
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			kind := gvk.Kind
			apiVersion := gvk.Group + "/" + gvk.Version

			// create the CR as specified by the test case
			name := fmt.Sprintf("apply-create-crd-%s", tc.name)
			finalizerVal, _ := json.Marshal(tc.finalizer)
			applyCreateBody := []byte(fmt.Sprintf(crdApplyFinalizerBody, apiVersion, kind, name, finalizerVal))

			req := rest.Patch(types.ApplyPatchType).
				AbsPath("/apis", gvr.Group, gvr.Version, gvr.Resource).
				Name(name).
				Param("fieldManager", "apply_test").
				VersionedParams(&tc.opts, metav1.ParameterCodec)
			result := req.Body(applyCreateBody).Do(context.TODO())
			if result.Error() != nil {
				t.Fatalf("unexpected error: %v", result.Error())
			}

			if len(result.Warnings()) != len(tc.expectCreateWarnings) {
				for _, r := range result.Warnings() {
					t.Logf("received warning: %v", r)
				}
				t.Fatalf("unexpected number of warnings, expected: %d, got: %d", len(tc.expectCreateWarnings), len(result.Warnings()))
			}
			for i, expectedWarning := range tc.expectCreateWarnings {
				if expectedWarning != result.Warnings()[i].Text {
					t.Fatalf("expected warning: %s, got warning: %s", expectedWarning, result.Warnings()[i].Text)
				}
			}

			if len(tc.updatedFinalizer) != 0 {
				finalizerVal, _ := json.Marshal(tc.updatedFinalizer)
				applyUpdateBody := []byte(fmt.Sprintf(crdApplyFinalizerBody, apiVersion, kind, name, finalizerVal))
				updateReq := rest.Patch(types.ApplyPatchType).
					AbsPath("/apis", gvr.Group, gvr.Version, gvr.Resource).
					Name(name).
					Param("fieldManager", "apply_test").
					VersionedParams(&tc.opts, metav1.ParameterCodec)
				result = updateReq.Body(applyUpdateBody).Do(context.TODO())

				if result.Error() != nil {
					t.Fatalf("unexpected error: %v", result.Error())
				}

				if len(result.Warnings()) != len(tc.expectUpdateWarnings) {
					t.Fatalf("unexpected number of warnings, expected: %d, got: %d", len(tc.expectUpdateWarnings), len(result.Warnings()))
				}
				for i, expectedWarning := range tc.expectUpdateWarnings {
					if expectedWarning != result.Warnings()[i].Text {
						t.Fatalf("expected warning: %s, got warning: %s", expectedWarning, result.Warnings()[i].Text)
					}
				}
			}
		})
	}
}

func setupCRD(t testing.TB, config *rest.Config, apiGroup string, schemaless bool) *apiextensionsv1.CustomResourceDefinition {
	apiExtensionClient, err := apiextensionsclient.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}

	preserveUnknownFields := ""
	if schemaless {
		preserveUnknownFields = `"x-kubernetes-preserve-unknown-fields": true,`
	}
	crdSchema := fmt.Sprintf(crdSchemaBase, preserveUnknownFields)

	// create the CRD
	crd := fixtures.NewNoxuV1CustomResourceDefinition(apiextensionsv1.ClusterScoped)

	// adjust the API group
	crd.Name = crd.Spec.Names.Plural + "." + apiGroup
	crd.Spec.Group = apiGroup

	var c apiextensionsv1.CustomResourceValidation
	err = json.Unmarshal([]byte(crdSchema), &c)
	if err != nil {
		t.Fatal(err)
	}
	//crd.Spec.PreserveUnknownFields = false
	for i := range crd.Spec.Versions {
		crd.Spec.Versions[i].Schema = &c
	}
	// install the CRD
	crd, err = fixtures.CreateNewV1CustomResourceDefinition(crd, apiExtensionClient, dynamicClient)
	if err != nil {
		t.Fatal(err)
	}

	return crd
}

func BenchmarkFieldValidation(b *testing.B) {
	flag.Lookup("v").Value.Set("0")
	server, err := kubeapiservertesting.StartTestServer(b, kubeapiservertesting.NewDefaultTestServerOptions(), nil, framework.SharedEtcd())
	if err != nil {
		b.Fatal(err)
	}
	config := server.ClientConfig
	defer server.TearDownFn()

	// don't log warnings, tests inspect them in the responses directly
	config.WarningHandler = rest.NoWarnings{}

	client := clientset.NewForConfigOrDie(config)

	schemaCRD := setupCRD(b, config, "schema.example.com", false)
	schemaGVR := schema.GroupVersionResource{
		Group:    schemaCRD.Spec.Group,
		Version:  schemaCRD.Spec.Versions[0].Name,
		Resource: schemaCRD.Spec.Names.Plural,
	}
	schemaGVK := schema.GroupVersionKind{
		Group:   schemaCRD.Spec.Group,
		Version: schemaCRD.Spec.Versions[0].Name,
		Kind:    schemaCRD.Spec.Names.Kind,
	}

	schemalessCRD := setupCRD(b, config, "schemaless.example.com", true)
	schemalessGVR := schema.GroupVersionResource{
		Group:    schemalessCRD.Spec.Group,
		Version:  schemalessCRD.Spec.Versions[0].Name,
		Resource: schemalessCRD.Spec.Names.Plural,
	}
	schemalessGVK := schema.GroupVersionKind{
		Group:   schemalessCRD.Spec.Group,
		Version: schemalessCRD.Spec.Versions[0].Name,
		Kind:    schemalessCRD.Spec.Names.Kind,
	}

	rest := client.Discovery().RESTClient()

	b.Run("Post", func(b *testing.B) { benchFieldValidationPost(b, client) })
	b.Run("Put", func(b *testing.B) { benchFieldValidationPut(b, client) })
	b.Run("PatchTyped", func(b *testing.B) { benchFieldValidationPatchTyped(b, client) })
	b.Run("SMP", func(b *testing.B) { benchFieldValidationSMP(b, client) })
	b.Run("ApplyCreate", func(b *testing.B) { benchFieldValidationApplyCreate(b, client) })
	b.Run("ApplyUpdate", func(b *testing.B) { benchFieldValidationApplyUpdate(b, client) })

	b.Run("PostCRD", func(b *testing.B) { benchFieldValidationPostCRD(b, rest, schemaGVK, schemaGVR) })
	b.Run("PutCRD", func(b *testing.B) { benchFieldValidationPutCRD(b, rest, schemaGVK, schemaGVR) })
	b.Run("PatchCRD", func(b *testing.B) { benchFieldValidationPatchCRD(b, rest, schemaGVK, schemaGVR) })
	b.Run("ApplyCreateCRD", func(b *testing.B) { benchFieldValidationApplyCreateCRD(b, rest, schemaGVK, schemaGVR) })
	b.Run("ApplyUpdateCRD", func(b *testing.B) { benchFieldValidationApplyUpdateCRD(b, rest, schemaGVK, schemaGVR) })

	b.Run("PostCRDSchemaless", func(b *testing.B) { benchFieldValidationPostCRD(b, rest, schemalessGVK, schemalessGVR) })
	b.Run("PutCRDSchemaless", func(b *testing.B) { benchFieldValidationPutCRD(b, rest, schemalessGVK, schemalessGVR) })
	b.Run("PatchCRDSchemaless", func(b *testing.B) { benchFieldValidationPatchCRD(b, rest, schemalessGVK, schemalessGVR) })
	b.Run("ApplyCreateCRDSchemaless", func(b *testing.B) { benchFieldValidationApplyCreateCRD(b, rest, schemalessGVK, schemalessGVR) })
	b.Run("ApplyUpdateCRDSchemaless", func(b *testing.B) { benchFieldValidationApplyUpdateCRD(b, rest, schemalessGVK, schemalessGVR) })

}

func benchFieldValidationPost(b *testing.B, client clientset.Interface) {
	var benchmarks = []struct {
		name        string
		bodyBase    string
		opts        metav1.CreateOptions
		contentType string
	}{
		{
			name: "post-strict-validation",
			opts: metav1.CreateOptions{
				FieldValidation: "Strict",
			},
			bodyBase: validBodyJSON,
		},
		{
			name: "post-warn-validation",
			opts: metav1.CreateOptions{
				FieldValidation: "Warn",
			},
			bodyBase: validBodyJSON,
		},
		{
			name: "post-ignore-validation",
			opts: metav1.CreateOptions{
				FieldValidation: "Ignore",
			},
			bodyBase: validBodyJSON,
		},
		{
			name: "post-strict-validation-yaml",
			opts: metav1.CreateOptions{
				FieldValidation: "Strict",
			},
			bodyBase:    validBodyYAML,
			contentType: "application/yaml",
		},
		{
			name: "post-warn-validation-yaml",
			opts: metav1.CreateOptions{
				FieldValidation: "Warn",
			},
			bodyBase:    validBodyYAML,
			contentType: "application/yaml",
		},
		{
			name: "post-ignore-validation-yaml",
			opts: metav1.CreateOptions{
				FieldValidation: "Ignore",
			},
			bodyBase:    validBodyYAML,
			contentType: "application/yaml",
		},
	}

	for _, bm := range benchmarks {
		b.Run(bm.name, func(b *testing.B) {
			b.ResetTimer()
			b.ReportAllocs()
			for n := 0; n < b.N; n++ {
				body := []byte(fmt.Sprintf(bm.bodyBase, fmt.Sprintf("test-deployment-%s-%d-%d-%d", bm.name, n, b.N, time.Now().UnixNano())))
				req := client.CoreV1().RESTClient().Post().
					AbsPath("/apis/apps/v1").
					Namespace("default").
					Resource("deployments").
					SetHeader("Content-Type", bm.contentType).
					VersionedParams(&bm.opts, metav1.ParameterCodec)
				result := req.Body(body).Do(context.TODO())
				if result.Error() != nil {
					b.Fatalf("unexpected request err: %v", result.Error())
				}
			}
		})
	}
}

func benchFieldValidationPut(b *testing.B, client clientset.Interface) {
	var testcases = []struct {
		name        string
		opts        metav1.UpdateOptions
		putBodyBase string
		contentType string
	}{
		{
			name: "put-strict-validation",
			opts: metav1.UpdateOptions{
				FieldValidation: "Strict",
			},
			putBodyBase: validBodyJSON,
		},
		{
			name: "put-warn-validation",
			opts: metav1.UpdateOptions{
				FieldValidation: "Warn",
			},
			putBodyBase: validBodyJSON,
		},
		{
			name: "put-ignore-validation",
			opts: metav1.UpdateOptions{
				FieldValidation: "Ignore",
			},
			putBodyBase: validBodyJSON,
		},
		{
			name: "put-strict-validation-yaml",
			opts: metav1.UpdateOptions{
				FieldValidation: "Strict",
			},
			putBodyBase: validBodyYAML,
			contentType: "application/yaml",
		},
		{
			name: "put-warn-validation-yaml",
			opts: metav1.UpdateOptions{
				FieldValidation: "Warn",
			},
			putBodyBase: validBodyYAML,
			contentType: "application/yaml",
		},
		{
			name: "put-ignore-validation-yaml",
			opts: metav1.UpdateOptions{
				FieldValidation: "Ignore",
			},
			putBodyBase: validBodyYAML,
			contentType: "application/yaml",
		},
	}

	for _, tc := range testcases {
		b.Run(tc.name, func(b *testing.B) {
			names := make([]string, b.N)
			for n := 0; n < b.N; n++ {
				deployName := fmt.Sprintf("%s-%d-%d-%d", tc.name, n, b.N, time.Now().UnixNano())
				names[n] = deployName
				postBody := []byte(fmt.Sprintf(string(validBodyJSON), deployName))

				if _, err := client.CoreV1().RESTClient().Post().
					AbsPath("/apis/apps/v1").
					Namespace("default").
					Resource("deployments").
					Body(postBody).
					DoRaw(context.TODO()); err != nil {
					b.Fatalf("failed to create initial deployment: %v", err)
				}

			}
			b.ResetTimer()
			b.ReportAllocs()
			for n := 0; n < b.N; n++ {
				deployName := names[n]
				putBody := []byte(fmt.Sprintf(string(tc.putBodyBase), deployName))
				req := client.CoreV1().RESTClient().Put().
					AbsPath("/apis/apps/v1").
					Namespace("default").
					Resource("deployments").
					SetHeader("Content-Type", tc.contentType).
					Name(deployName).
					VersionedParams(&tc.opts, metav1.ParameterCodec)
				result := req.Body([]byte(putBody)).Do(context.TODO())
				if result.Error() != nil {
					b.Fatalf("unexpected request err: %v", result.Error())
				}
			}
		})
	}
}

func benchFieldValidationPatchTyped(b *testing.B, client clientset.Interface) {
	mergePatchBodyValid := `
{
	"spec": {
		"paused": false,
		"template": {
			"spec": {
				"containers": [{
					"name": "nginx",
					"image": "nginx:latest",
					"imagePullPolicy": "Always"
				}]
			}
		},
		"replicas": 2
	}
}
	`

	jsonPatchBodyValid := `
			[
				{"op": "add", "path": "/spec/paused", "value": true},
				{"op": "add", "path": "/spec/template/spec/containers/0/imagePullPolicy", "value": "Never"},
				{"op": "add", "path": "/spec/replicas", "value": 2}
			]
			`

	var testcases = []struct {
		name      string
		opts      metav1.PatchOptions
		patchType types.PatchType
		body      string
	}{
		{
			name: "merge-patch-strict-validation",
			opts: metav1.PatchOptions{
				FieldValidation: "Strict",
			},
			patchType: types.MergePatchType,
			body:      mergePatchBodyValid,
		},
		{
			name: "merge-patch-warn-validation",
			opts: metav1.PatchOptions{
				FieldValidation: "Warn",
			},
			patchType: types.MergePatchType,
			body:      mergePatchBodyValid,
		},
		{
			name: "merge-patch-ignore-validation",
			opts: metav1.PatchOptions{
				FieldValidation: "Ignore",
			},
			patchType: types.MergePatchType,
			body:      mergePatchBodyValid,
		},
		{
			name:      "json-patch-strict-validation",
			patchType: types.JSONPatchType,
			opts: metav1.PatchOptions{
				FieldValidation: "Strict",
			},
			body: jsonPatchBodyValid,
		},
		{
			name:      "json-patch-warn-validation",
			patchType: types.JSONPatchType,
			opts: metav1.PatchOptions{
				FieldValidation: "Warn",
			},
			body: jsonPatchBodyValid,
		},
		{
			name:      "json-patch-ignore-validation",
			patchType: types.JSONPatchType,
			opts: metav1.PatchOptions{
				FieldValidation: "Ignore",
			},
			body: jsonPatchBodyValid,
		},
	}

	for _, tc := range testcases {
		b.Run(tc.name, func(b *testing.B) {
			names := make([]string, b.N)
			for n := 0; n < b.N; n++ {
				deployName := fmt.Sprintf("%s-%d-%d-%d", tc.name, n, b.N, time.Now().UnixNano())
				names[n] = deployName
				postBody := []byte(fmt.Sprintf(string(validBodyJSON), deployName))

				if _, err := client.CoreV1().RESTClient().Post().
					AbsPath("/apis/apps/v1").
					Namespace("default").
					Resource("deployments").
					Body(postBody).
					DoRaw(context.TODO()); err != nil {
					b.Fatalf("failed to create initial deployment: %v", err)
				}
			}
			b.ResetTimer()
			b.ReportAllocs()
			for n := 0; n < b.N; n++ {
				deployName := names[n]
				req := client.CoreV1().RESTClient().Patch(tc.patchType).
					AbsPath("/apis/apps/v1").
					Namespace("default").
					Resource("deployments").
					Name(deployName).
					VersionedParams(&tc.opts, metav1.ParameterCodec)
				result := req.Body([]byte(tc.body)).Do(context.TODO())
				if result.Error() != nil {
					b.Fatalf("unexpected request err: %v", result.Error())
				}
			}

		})
	}
}

func benchFieldValidationSMP(b *testing.B, client clientset.Interface) {
	smpBodyValid := `
	{
		"spec": {
			"replicas": 3,
			"paused": false,
			"selector": {
				"matchLabels": {
					"app": "nginx"
				}
			},
			"template": {
				"metadata": {
					"labels": {
						"app": "nginx"
					}
				},
				"spec": {
					"containers": [{
						"name": "nginx",
						"imagePullPolicy": "Never"
					}]
				}
			}
		}
	}
	`
	var testcases = []struct {
		name string
		opts metav1.PatchOptions
		body string
	}{
		{
			name: "smp-strict-validation",
			opts: metav1.PatchOptions{
				FieldValidation: "Strict",
			},
			body: smpBodyValid,
		},
		{
			name: "smp-warn-validation",
			opts: metav1.PatchOptions{
				FieldValidation: "Warn",
			},
			body: smpBodyValid,
		},
		{
			name: "smp-ignore-validation",
			opts: metav1.PatchOptions{
				FieldValidation: "Ignore",
			},
			body: smpBodyValid,
		},
	}

	for _, tc := range testcases {
		b.Run(tc.name, func(b *testing.B) {
			names := make([]string, b.N)
			for n := 0; n < b.N; n++ {
				name := fmt.Sprintf("%s-%d-%d-%d", tc.name, n, b.N, time.Now().UnixNano())
				names[n] = name
				body := []byte(fmt.Sprintf(validBodyJSON, name))
				_, err := client.CoreV1().RESTClient().Patch(types.ApplyPatchType).
					AbsPath("/apis/apps/v1").
					Namespace("default").
					Resource("deployments").
					Name(name).
					Param("fieldManager", "apply_test").
					Body(body).
					Do(context.TODO()).
					Get()
				if err != nil {
					b.Fatalf("Failed to create object using Apply patch: %v", err)
				}
			}
			b.ResetTimer()
			b.ReportAllocs()
			for n := 0; n < b.N; n++ {
				name := names[n]
				req := client.CoreV1().RESTClient().Patch(types.StrategicMergePatchType).
					AbsPath("/apis/apps/v1").
					Namespace("default").
					Resource("deployments").
					Name(name).
					VersionedParams(&tc.opts, metav1.ParameterCodec)
				result := req.Body([]byte(tc.body)).Do(context.TODO())
				if result.Error() != nil {
					b.Fatalf("unexpected request err: %v", result.Error())
				}
			}
		})
	}

}

func benchFieldValidationApplyCreate(b *testing.B, client clientset.Interface) {
	var testcases = []struct {
		name string
		opts metav1.PatchOptions
	}{
		{
			name: "strict-validation",
			opts: metav1.PatchOptions{
				FieldValidation: "Strict",
				FieldManager:    "mgr",
			},
		},
		{
			name: "warn-validation",
			opts: metav1.PatchOptions{
				FieldValidation: "Warn",
				FieldManager:    "mgr",
			},
		},
		{
			name: "ignore-validation",
			opts: metav1.PatchOptions{
				FieldValidation: "Ignore",
				FieldManager:    "mgr",
			},
		},
	}

	for _, tc := range testcases {
		b.Run(tc.name, func(b *testing.B) {
			b.ResetTimer()
			b.ReportAllocs()
			for n := 0; n < b.N; n++ {
				name := fmt.Sprintf("apply-create-deployment-%s-%d-%d-%d", tc.name, n, b.N, time.Now().UnixNano())
				body := []byte(fmt.Sprintf(validBodyJSON, name))
				req := client.CoreV1().RESTClient().Patch(types.ApplyPatchType).
					AbsPath("/apis/apps/v1").
					Namespace("default").
					Resource("deployments").
					Name(name).
					VersionedParams(&tc.opts, metav1.ParameterCodec)
				result := req.Body(body).Do(context.TODO())
				if result.Error() != nil {
					b.Fatalf("unexpected request err: %v", result.Error())
				}
			}
		})
	}
}

func benchFieldValidationApplyUpdate(b *testing.B, client clientset.Interface) {
	var testcases = []struct {
		name string
		opts metav1.PatchOptions
	}{
		{
			name: "strict-validation",
			opts: metav1.PatchOptions{
				FieldValidation: "Strict",
				FieldManager:    "mgr",
			},
		},
		{
			name: "warn-validation",
			opts: metav1.PatchOptions{
				FieldValidation: "Warn",
				FieldManager:    "mgr",
			},
		},
		{
			name: "ignore-validation",
			opts: metav1.PatchOptions{
				FieldValidation: "Ignore",
				FieldManager:    "mgr",
			},
		},
	}

	for _, tc := range testcases {
		b.Run(tc.name, func(b *testing.B) {
			names := make([]string, b.N)
			for n := 0; n < b.N; n++ {
				name := fmt.Sprintf("apply-update-deployment-%s-%d-%d-%d", tc.name, n, b.N, time.Now().UnixNano())
				names[n] = name
				createBody := []byte(fmt.Sprintf(validBodyJSON, name))
				createReq := client.CoreV1().RESTClient().Patch(types.ApplyPatchType).
					AbsPath("/apis/apps/v1").
					Namespace("default").
					Resource("deployments").
					Name(name).
					VersionedParams(&tc.opts, metav1.ParameterCodec)
				createResult := createReq.Body(createBody).Do(context.TODO())
				if createResult.Error() != nil {
					b.Fatalf("unexpected apply create err: %v", createResult.Error())
				}
			}
			b.ResetTimer()
			b.ReportAllocs()
			for n := 0; n < b.N; n++ {
				name := names[n]
				updateBody := []byte(fmt.Sprintf(applyValidBody, name))
				updateReq := client.CoreV1().RESTClient().Patch(types.ApplyPatchType).
					AbsPath("/apis/apps/v1").
					Namespace("default").
					Resource("deployments").
					Name(name).
					VersionedParams(&tc.opts, metav1.ParameterCodec)
				result := updateReq.Body(updateBody).Do(context.TODO())
				if result.Error() != nil {
					b.Fatalf("unexpected request err: %v", result.Error())
				}
			}
		})
	}
}

func benchFieldValidationPostCRD(b *testing.B, rest rest.Interface, gvk schema.GroupVersionKind, gvr schema.GroupVersionResource) {
	var testcases = []struct {
		name        string
		opts        metav1.PatchOptions
		body        string
		contentType string
	}{
		{
			name: "crd-post-strict-validation",
			opts: metav1.PatchOptions{
				FieldValidation: "Strict",
			},
			body: crdValidBody,
		},
		{
			name: "crd-post-warn-validation",
			opts: metav1.PatchOptions{
				FieldValidation: "Warn",
			},
			body: crdValidBody,
		},
		{
			name: "crd-post-ignore-validation",
			opts: metav1.PatchOptions{
				FieldValidation: "Ignore",
			},
			body: crdValidBody,
		},
		{
			name: "crd-post-no-validation",
			body: crdValidBody,
		},
		{
			name: "crd-post-strict-validation-yaml",
			opts: metav1.PatchOptions{
				FieldValidation: "Strict",
			},
			body:        crdValidBodyYAML,
			contentType: "application/yaml",
		},
		{
			name: "crd-post-warn-validation-yaml",
			opts: metav1.PatchOptions{
				FieldValidation: "Warn",
			},
			body:        crdValidBodyYAML,
			contentType: "application/yaml",
		},
		{
			name: "crd-post-ignore-validation-yaml",
			opts: metav1.PatchOptions{
				FieldValidation: "Ignore",
			},
			body:        crdValidBodyYAML,
			contentType: "application/yaml",
		},
		{
			name:        "crd-post-no-validation-yaml",
			body:        crdValidBodyYAML,
			contentType: "application/yaml",
		},
	}
	for _, tc := range testcases {
		b.Run(tc.name, func(b *testing.B) {
			b.ResetTimer()
			b.ReportAllocs()
			for n := 0; n < b.N; n++ {
				kind := gvk.Kind
				apiVersion := gvk.Group + "/" + gvk.Version

				// create the CR as specified by the test case
				jsonBody := []byte(fmt.Sprintf(tc.body, apiVersion, kind, fmt.Sprintf("test-dep-%s-%d-%d-%d", tc.name, n, b.N, time.Now().UnixNano())))
				req := rest.Post().
					AbsPath("/apis", gvr.Group, gvr.Version, gvr.Resource).
					SetHeader("Content-Type", tc.contentType).
					VersionedParams(&tc.opts, metav1.ParameterCodec)
				result := req.Body([]byte(jsonBody)).Do(context.TODO())

				if result.Error() != nil {
					b.Fatalf("unexpected post err: %v", result.Error())
				}
			}
		})
	}
}

func benchFieldValidationPutCRD(b *testing.B, rest rest.Interface, gvk schema.GroupVersionKind, gvr schema.GroupVersionResource) {
	var testcases = []struct {
		name        string
		opts        metav1.PatchOptions
		putBody     string
		contentType string
	}{
		{
			name: "crd-put-strict-validation",
			opts: metav1.PatchOptions{
				FieldValidation: "Strict",
			},
			putBody: crdValidBody,
		},
		{
			name: "crd-put-warn-validation",
			opts: metav1.PatchOptions{
				FieldValidation: "Warn",
			},
			putBody: crdValidBody,
		},
		{
			name: "crd-put-ignore-validation",
			opts: metav1.PatchOptions{
				FieldValidation: "Ignore",
			},
			putBody: crdValidBody,
		},
		{
			name:    "crd-put-no-validation",
			putBody: crdValidBody,
		},
		{
			name: "crd-put-strict-validation-yaml",
			opts: metav1.PatchOptions{
				FieldValidation: "Strict",
			},
			putBody:     crdValidBodyYAML,
			contentType: "application/yaml",
		},
		{
			name: "crd-put-warn-validation-yaml",
			opts: metav1.PatchOptions{
				FieldValidation: "Warn",
			},
			putBody:     crdValidBodyYAML,
			contentType: "application/yaml",
		},
		{
			name: "crd-put-ignore-validation-yaml",
			opts: metav1.PatchOptions{
				FieldValidation: "Ignore",
			},
			putBody:     crdValidBodyYAML,
			contentType: "application/yaml",
		},
		{
			name:        "crd-put-no-validation-yaml",
			putBody:     crdValidBodyYAML,
			contentType: "application/yaml",
		},
	}
	for _, tc := range testcases {
		b.Run(tc.name, func(b *testing.B) {
			kind := gvk.Kind
			apiVersion := gvk.Group + "/" + gvk.Version
			names := make([]string, b.N)
			resourceVersions := make([]string, b.N)
			for n := 0; n < b.N; n++ {
				deployName := fmt.Sprintf("test-dep-%s-%d-%d-%d", tc.name, n, b.N, time.Now().UnixNano())
				names[n] = deployName

				// create the CR as specified by the test case
				jsonPostBody := []byte(fmt.Sprintf(crdValidBody, apiVersion, kind, deployName))
				postReq := rest.Post().
					AbsPath("/apis", gvr.Group, gvr.Version, gvr.Resource).
					VersionedParams(&tc.opts, metav1.ParameterCodec)
				postResult, err := postReq.Body([]byte(jsonPostBody)).Do(context.TODO()).Raw()
				if err != nil {
					b.Fatalf("unexpected error on CR creation: %v", err)
				}
				postUnstructured := &unstructured.Unstructured{}
				if err := postUnstructured.UnmarshalJSON(postResult); err != nil {
					b.Fatalf("unexpected error unmarshalling created CR: %v", err)
				}
				resourceVersions[n] = postUnstructured.GetResourceVersion()
			}
			b.ResetTimer()
			b.ReportAllocs()
			for n := 0; n < b.N; n++ {
				// update the CR as specified by the test case
				putBody := []byte(fmt.Sprintf(tc.putBody, apiVersion, kind, names[n], resourceVersions[n]))
				putReq := rest.Put().
					AbsPath("/apis", gvr.Group, gvr.Version, gvr.Resource).
					Name(names[n]).
					SetHeader("Content-Type", tc.contentType).
					VersionedParams(&tc.opts, metav1.ParameterCodec)
				result := putReq.Body([]byte(putBody)).Do(context.TODO())
				if result.Error() != nil {
					b.Fatalf("unexpected put err: %v", result.Error())
				}
			}
		})
	}
}

func benchFieldValidationPatchCRD(b *testing.B, rest rest.Interface, gvk schema.GroupVersionKind, gvr schema.GroupVersionResource) {
	patchYAMLBody := `
apiVersion: %s
kind: %s
metadata:
  name: %s
  finalizers:
  - test/finalizer
spec:
  cronSpec: "* * * * */5"
  ports:
  - name: x
    containerPort: 80
    protocol: TCP`

	mergePatchBody := `
{
	"spec": {
		"knownField1": "val1",
			"ports": [{
				"name": "portName",
				"containerPort": 8080,
				"protocol": "TCP",
				"hostPort": 8081
			}]
	}
}
	`
	jsonPatchBody := `
			[
				{"op": "add", "path": "/spec/knownField1", "value": "val1"},
				{"op": "add", "path": "/spec/ports/0/name", "value": "portName"},
				{"op": "add", "path": "/spec/ports/0/containerPort", "value": 8080},
				{"op": "add", "path": "/spec/ports/0/protocol", "value": "TCP"},
				{"op": "add", "path": "/spec/ports/0/hostPort", "value": 8081}
			]
			`
	var testcases = []struct {
		name      string
		patchType types.PatchType
		opts      metav1.PatchOptions
		body      string
	}{
		{
			name:      "crd-merge-patch-strict-validation",
			patchType: types.MergePatchType,
			opts: metav1.PatchOptions{
				FieldValidation: "Strict",
			},
			body: mergePatchBody,
		},
		{
			name:      "crd-merge-patch-warn-validation",
			patchType: types.MergePatchType,
			opts: metav1.PatchOptions{
				FieldValidation: "Warn",
			},
			body: mergePatchBody,
		},
		{
			name:      "crd-merge-patch-ignore-validation",
			patchType: types.MergePatchType,
			opts: metav1.PatchOptions{
				FieldValidation: "Ignore",
			},
			body: mergePatchBody,
		},
		{
			name:      "crd-merge-patch-no-validation",
			patchType: types.MergePatchType,
			body:      mergePatchBody,
		},
		{
			name:      "crd-json-patch-strict-validation",
			patchType: types.JSONPatchType,
			opts: metav1.PatchOptions{
				FieldValidation: "Strict",
			},
			body: jsonPatchBody,
		},
		{
			name:      "crd-json-patch-warn-validation",
			patchType: types.JSONPatchType,
			opts: metav1.PatchOptions{
				FieldValidation: "Warn",
			},
			body: jsonPatchBody,
		},
		{
			name:      "crd-json-patch-ignore-validation",
			patchType: types.JSONPatchType,
			opts: metav1.PatchOptions{
				FieldValidation: "Ignore",
			},
			body: jsonPatchBody,
		},
		{
			name:      "crd-json-patch-no-validation",
			patchType: types.JSONPatchType,
			body:      jsonPatchBody,
		},
	}
	for _, tc := range testcases {
		b.Run(tc.name, func(b *testing.B) {
			kind := gvk.Kind
			apiVersion := gvk.Group + "/" + gvk.Version
			names := make([]string, b.N)
			for n := 0; n < b.N; n++ {
				deployName := fmt.Sprintf("test-dep-%s-%d-%d-%d", tc.name, n, b.N, time.Now().UnixNano())
				names[n] = deployName

				// create a CR
				yamlBody := []byte(fmt.Sprintf(string(patchYAMLBody), apiVersion, kind, deployName))
				createResult, err := rest.Patch(types.ApplyPatchType).
					AbsPath("/apis", gvr.Group, gvr.Version, gvr.Resource).
					Name(deployName).
					Param("fieldManager", "apply_test").
					Body(yamlBody).
					DoRaw(context.TODO())
				if err != nil {
					b.Fatalf("failed to create custom resource with apply: %v:\n%v", err, string(createResult))
				}
			}
			b.ResetTimer()
			b.ReportAllocs()
			for n := 0; n < b.N; n++ {
				// patch the CR as specified by the test case
				req := rest.Patch(tc.patchType).
					AbsPath("/apis", gvr.Group, gvr.Version, gvr.Resource).
					Name(names[n]).
					VersionedParams(&tc.opts, metav1.ParameterCodec)
				result := req.Body([]byte(tc.body)).Do(context.TODO())
				if result.Error() != nil {
					b.Fatalf("unexpected patch err: %v", result.Error())
				}
			}

		})
	}
}

func benchFieldValidationApplyCreateCRD(b *testing.B, rest rest.Interface, gvk schema.GroupVersionKind, gvr schema.GroupVersionResource) {
	var testcases = []struct {
		name string
		opts metav1.PatchOptions
	}{
		{
			name: "strict-validation",
			opts: metav1.PatchOptions{
				FieldValidation: "Strict",
				FieldManager:    "mgr",
			},
		},
		{
			name: "warn-validation",
			opts: metav1.PatchOptions{
				FieldValidation: "Warn",
				FieldManager:    "mgr",
			},
		},
		{
			name: "ignore-validation",
			opts: metav1.PatchOptions{
				FieldValidation: "Ignore",
				FieldManager:    "mgr",
			},
		},
		{
			name: "no-validation",
			opts: metav1.PatchOptions{
				FieldManager: "mgr",
			},
		},
	}

	for _, tc := range testcases {
		b.Run(tc.name, func(b *testing.B) {
			b.ResetTimer()
			b.ReportAllocs()
			for n := 0; n < b.N; n++ {
				kind := gvk.Kind
				apiVersion := gvk.Group + "/" + gvk.Version
				name := fmt.Sprintf("test-dep-%s-%d-%d-%d", tc.name, n, b.N, time.Now().UnixNano())

				// create the CR as specified by the test case
				applyCreateBody := []byte(fmt.Sprintf(crdApplyValidBody, apiVersion, kind, name))

				req := rest.Patch(types.ApplyPatchType).
					AbsPath("/apis", gvr.Group, gvr.Version, gvr.Resource).
					Name(name).
					VersionedParams(&tc.opts, metav1.ParameterCodec)
				result := req.Body(applyCreateBody).Do(context.TODO())
				if result.Error() != nil {
					b.Fatalf("unexpected apply err: %v", result.Error())
				}

			}
		})
	}
}

func benchFieldValidationApplyUpdateCRD(b *testing.B, rest rest.Interface, gvk schema.GroupVersionKind, gvr schema.GroupVersionResource) {
	var testcases = []struct {
		name string
		opts metav1.PatchOptions
	}{
		{
			name: "strict-validation",
			opts: metav1.PatchOptions{
				FieldValidation: "Strict",
				FieldManager:    "mgr",
			},
		},
		{
			name: "warn-validation",
			opts: metav1.PatchOptions{
				FieldValidation: "Warn",
				FieldManager:    "mgr",
			},
		},
		{
			name: "ignore-validation",
			opts: metav1.PatchOptions{
				FieldValidation: "Ignore",
				FieldManager:    "mgr",
			},
		},
		{
			name: "no-validation",
			opts: metav1.PatchOptions{
				FieldManager: "mgr",
			},
		},
	}

	for _, tc := range testcases {
		b.Run(tc.name, func(b *testing.B) {
			kind := gvk.Kind
			apiVersion := gvk.Group + "/" + gvk.Version
			names := make([]string, b.N)

			for n := 0; n < b.N; n++ {
				names[n] = fmt.Sprintf("apply-update-crd-%s-%d-%d-%d", tc.name, n, b.N, time.Now().UnixNano())
				applyCreateBody := []byte(fmt.Sprintf(crdApplyValidBody, apiVersion, kind, names[n]))
				createReq := rest.Patch(types.ApplyPatchType).
					AbsPath("/apis", gvr.Group, gvr.Version, gvr.Resource).
					Name(names[n]).
					VersionedParams(&tc.opts, metav1.ParameterCodec)
				createResult := createReq.Body(applyCreateBody).Do(context.TODO())
				if createResult.Error() != nil {
					b.Fatalf("unexpected apply create err: %v", createResult.Error())
				}
			}
			b.ResetTimer()
			b.ReportAllocs()
			for n := 0; n < b.N; n++ {
				applyUpdateBody := []byte(fmt.Sprintf(crdApplyValidBody2, apiVersion, kind, names[n]))
				updateReq := rest.Patch(types.ApplyPatchType).
					AbsPath("/apis", gvr.Group, gvr.Version, gvr.Resource).
					Name(names[n]).
					VersionedParams(&tc.opts, metav1.ParameterCodec)
				result := updateReq.Body(applyUpdateBody).Do(context.TODO())

				if result.Error() != nil {
					b.Fatalf("unexpected apply err: %v", result.Error())
				}
			}

		})
	}
}
