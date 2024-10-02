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

package v2

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/openapi/openapitest"
)

var appsv1Path = "apis/apps/v1"

var appsDeploymentGVR = schema.GroupVersionResource{
	Group:    "apps",
	Version:  "v1",
	Resource: "deployments",
}

// Shows generic throws error when attempting to `Render“ an invalid output name
// And if it is then added as a template, no error is thrown upon `Render`
func TestGeneratorMissingOutput(t *testing.T) {
	var buf bytes.Buffer
	var doc map[string]interface{}

	appsv1Bytes := bytesForGV(t, appsv1Path)
	err := json.Unmarshal(appsv1Bytes, &doc)
	require.NoError(t, err)

	gen := NewGenerator()
	badTemplateName := "bad-template"
	err = gen.Render(badTemplateName, doc, appsDeploymentGVR, nil, false, &buf, 0)

	require.ErrorContains(t, err, "unrecognized format: "+badTemplateName)
	require.Zero(t, buf.Len())

	err = gen.AddTemplate(badTemplateName, "ok")
	require.NoError(t, err)

	err = gen.Render(badTemplateName, doc, appsDeploymentGVR, nil, false, &buf, 0)
	require.NoError(t, err)
	require.Equal(t, "ok", buf.String())
}

// Shows that correct context with the passed object is passed to the template
func TestGeneratorContext(t *testing.T) {
	var buf bytes.Buffer
	var doc map[string]interface{}

	appsv1Bytes := bytesForGV(t, appsv1Path)
	err := json.Unmarshal(appsv1Bytes, &doc)
	require.NoError(t, err)

	gen := NewGenerator()
	err = gen.AddTemplate("Context", "{{ toJson . }}")
	require.NoError(t, err)

	expectedContext := TemplateContext{
		Document:  doc,
		GVR:       appsDeploymentGVR,
		Recursive: false,
		FieldPath: nil,
		Depth:     1,
	}

	err = gen.Render("Context",
		expectedContext.Document,
		expectedContext.GVR,
		expectedContext.FieldPath,
		expectedContext.Recursive,
		&buf,
		expectedContext.Depth)
	require.NoError(t, err)

	var actualContext TemplateContext
	err = json.Unmarshal(buf.Bytes(), &actualContext)
	require.NoError(t, err)
	require.Equal(t, expectedContext, actualContext)
}

// bytesForGV returns the OpenAPI V3 spec for the passed
// group/version as a byte slice. Assumes bytes are in json
// format. The passed path string looks like:
//
//	apis/apps/v1
func bytesForGV(t *testing.T, gvPath string) []byte {
	fakeClient := openapitest.NewEmbeddedFileClient()
	paths, err := fakeClient.Paths()
	require.NoError(t, err)
	gv, found := paths[gvPath]
	require.True(t, found)
	gvBytes, err := gv.Schema("application/json")
	require.NoError(t, err)
	return gvBytes
}
