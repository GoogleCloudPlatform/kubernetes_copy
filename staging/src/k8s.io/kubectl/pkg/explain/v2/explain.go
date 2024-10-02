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
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/openapi"
)

// PrintModelDescription prints the description of a specific model or dot path.
// If recursive, all components nested within the fields of the schema will be
// printed.
func PrintModelDescription(
	fieldsPath []string,
	w io.Writer,
	client openapi.Client,
	gvr schema.GroupVersionResource,
	recursive bool,
	outputFormat string,
	depth int,
) error {
	generator := NewGenerator()
	if err := registerBuiltinTemplates(generator); err != nil {
		return fmt.Errorf("error parsing builtin templates. Please file a bug on GitHub: %w", err)
	}

	return printModelDescriptionWithGenerator(
		generator, fieldsPath, w, client, gvr, recursive, outputFormat, depth)
}

// Factored out for testability
func printModelDescriptionWithGenerator(
	generator Generator,
	fieldsPath []string,
	w io.Writer,
	client openapi.Client,
	gvr schema.GroupVersionResource,
	recursive bool,
	outputFormat string,
	depth int,
) error {
	paths, err := client.Paths()

	if err != nil {
		return fmt.Errorf("failed to fetch list of groupVersions: %w", err)
	}

	var resourcePath string
	if len(gvr.Group) == 0 {
		resourcePath = fmt.Sprintf("api/%s", gvr.Version)
	} else {
		resourcePath = fmt.Sprintf("apis/%s/%s", gvr.Group, gvr.Version)
	}

	gv, exists := paths[resourcePath]

	if !exists {
		return fmt.Errorf("couldn't find resource for \"%v\"", gvr)
	}

	openAPISchemaBytes, err := gv.Schema(runtime.ContentTypeJSON)
	if err != nil {
		return fmt.Errorf("failed to fetch openapi schema for %s: %w", resourcePath, err)
	}

	var parsedV3Schema map[string]interface{}
	if err := json.Unmarshal(openAPISchemaBytes, &parsedV3Schema); err != nil {
		return fmt.Errorf("failed to parse openapi schema for %s: %w", resourcePath, err)
	}

	err = generator.Render(outputFormat, parsedV3Schema, gvr, fieldsPath, recursive, w, depth)

	explainErr := explainError("")
	if errors.As(err, &explainErr) {
		return explainErr
	}

	return err
}
