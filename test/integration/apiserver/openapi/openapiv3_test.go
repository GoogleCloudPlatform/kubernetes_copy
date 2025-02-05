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

package openapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	openapi_v3 "github.com/google/gnostic-models/openapiv3"
	"google.golang.org/protobuf/proto"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	"k8s.io/apiextensions-apiserver/test/integration/fixtures"
	"k8s.io/client-go/dynamic"
	kubernetes "k8s.io/client-go/kubernetes"
	restclient "k8s.io/client-go/rest"
	"k8s.io/kube-openapi/pkg/handler3"
	"k8s.io/kube-openapi/pkg/spec3"
	apiservertesting "k8s.io/kubernetes/cmd/kube-apiserver/app/testing"
	"k8s.io/kubernetes/test/integration/framework"
	"k8s.io/kubernetes/test/utils/ktesting"
	"sigs.k8s.io/yaml"
)

func TestOpenAPIV3SpecRoundTrip(t *testing.T) {
	tCtx := ktesting.Init(t)
	_, kubeConfig, tearDownFn := framework.StartTestServer(tCtx, t, framework.TestServerSetup{})
	defer tearDownFn()

	paths := []string{
		"/apis/apps/v1",
		"/apis/authentication.k8s.io/v1",
		"/apis/policy/v1",
		"/apis/batch/v1",
		"/version",
	}
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			rt, err := restclient.TransportFor(kubeConfig)
			if err != nil {
				t.Fatal(err)
			}
			// attempt to fetch and unmarshal
			url := kubeConfig.Host + "/openapi/v3" + path
			req, err := http.NewRequest("GET", url, nil)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := rt.RoundTrip(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			bs, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			var firstSpec spec3.OpenAPI
			err = json.Unmarshal(bs, &firstSpec)
			if err != nil {
				t.Fatal(err)
			}
			specBytes, err := json.Marshal(&firstSpec)
			if err != nil {
				t.Fatal(err)
			}
			var secondSpec spec3.OpenAPI
			err = json.Unmarshal(specBytes, &secondSpec)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(firstSpec, secondSpec) {
				t.Fatal("spec mismatch")
			}
		})
	}
}

func TestAddRemoveGroupVersion(t *testing.T) {
	server, err := apiservertesting.StartTestServer(t, apiservertesting.NewDefaultTestServerOptions(), nil, framework.SharedEtcd())
	if err != nil {
		t.Fatal(err)
	}
	defer server.TearDownFn()
	config := server.ClientConfig

	apiExtensionClient, err := clientset.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}

	// Create a new CRD with group mygroup.example.com
	noxuDefinition := fixtures.NewNoxuV1CustomResourceDefinition(apiextensionsv1.NamespaceScoped)
	noxuDefinition, err = fixtures.CreateNewV1CustomResourceDefinition(noxuDefinition, apiExtensionClient, dynamicClient)
	if err != nil {
		t.Fatal(err)
	}

	// It takes a second for CRD specs to propagate to the aggregator
	time.Sleep(4 * time.Second)

	// Verify that the new group version is populated in the discovery for OpenAPI v3
	jsonData, err := clientset.RESTClient().Get().AbsPath("/openapi/v3").Do(context.TODO()).Raw()
	if err != nil {
		t.Fatal(err)
	}
	openAPIv3GV := handler3.OpenAPIV3Discovery{}
	err = json.Unmarshal(jsonData, &openAPIv3GV)
	if err != nil {
		t.Fatal(err)
	}
	foundPath := false
	for path := range openAPIv3GV.Paths {
		if strings.Contains(path, "mygroup.example.com/v1beta1") {
			foundPath = true
		}
	}
	if foundPath == false {
		t.Fatal("Expected group version mygroup.example.com to be present after CRD applied")
	}

	// Check the spec for the newly published group version
	jsonData, err = clientset.RESTClient().Get().AbsPath("/openapi/v3/apis/mygroup.example.com/v1beta1").Do(context.TODO()).Raw()
	if err != nil {
		t.Fatal(err)
	}
	var firstSpec spec3.OpenAPI
	err = json.Unmarshal(jsonData, &firstSpec)
	if err != nil {
		t.Fatal(err)
	}

	// Delete the CRD and ensure that the group/version is also deleted in discovery
	if err := fixtures.DeleteV1CustomResourceDefinition(noxuDefinition, apiExtensionClient); err != nil {
		t.Fatal(err)
	}
	time.Sleep(4 * time.Second)

	jsonData, err = clientset.RESTClient().Get().AbsPath("/openapi/v3").Do(context.TODO()).Raw()
	if err != nil {
		t.Fatal(err)
	}
	openAPIv3GV = handler3.OpenAPIV3Discovery{}
	err = json.Unmarshal(jsonData, &openAPIv3GV)
	if err != nil {
		t.Fatal(err)
	}

	for path := range openAPIv3GV.Paths {
		if strings.Contains(path, "mygroup.example.com") {
			t.Fatal("Unexpected group version mygroup.example.com in OpenAPI v3 discovery")
		}
	}
}

func TestOpenAPIV3ProtoRoundtrip(t *testing.T) {
	// The OpenAPI V3 proto library strips fields that are sibling elements to $ref
	// See https://github.com/kubernetes/kubernetes/issues/106387 for more details
	t.Skip("Skipping OpenAPI V3 Proto roundtrip test")

	tCtx := ktesting.Init(t)
	_, kubeConfig, tearDownFn := framework.StartTestServer(tCtx, t, framework.TestServerSetup{})
	defer tearDownFn()

	rt, err := restclient.TransportFor(kubeConfig)
	if err != nil {
		t.Fatal(err)
	}
	// attempt to fetch and unmarshal
	req, err := http.NewRequest("GET", kubeConfig.Host+"/openapi/v3/apis/apps/v1", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	bs, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var firstSpec spec3.OpenAPI
	err = json.Unmarshal(bs, &firstSpec)
	if err != nil {
		t.Fatal(err)
	}

	protoReq, err := http.NewRequest("GET", kubeConfig.Host+"/openapi/v3/apis/apps/v1", nil)
	if err != nil {
		t.Fatal(err)
	}
	protoReq.Header.Set("Accept", "application/com.github.proto-openapi.spec.v3@v1.0+protobuf")
	protoResp, err := rt.RoundTrip(protoReq)
	if err != nil {
		t.Fatal(err)
	}
	defer protoResp.Body.Close()
	bs, err = io.ReadAll(protoResp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var protoDoc openapi_v3.Document
	err = proto.Unmarshal(bs, &protoDoc)
	if err != nil {
		t.Fatal(err)
	}

	yamlBytes, err := protoDoc.YAMLValue("")
	if err != nil {
		t.Fatal(err)
	}
	jsonBytes, err := yaml.YAMLToJSON(yamlBytes)
	if err != nil {
		t.Fatal(err)
	}
	var specFromProto spec3.OpenAPI
	err = json.Unmarshal(jsonBytes, &specFromProto)
	if err != nil {
		t.Fatal(err)
	}

	if !reflect.DeepEqual(specFromProto, firstSpec) {
		t.Fatalf("spec mismatch - specFromProto: %s\n", jsonBytes)
	}
}
