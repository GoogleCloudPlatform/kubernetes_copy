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
	"fmt"
	"io"
	"text/template"

	"k8s.io/apimachinery/pkg/runtime/schema"
)

type Generator interface {
	AddTemplate(name string, contents string) error

	Render(
		// Template to use for rendering
		templateName string,
		// Self-Contained OpenAPI Document Containing all schemas used by $ref
		// Only OpenAPI V3 documents are supported
		document map[string]interface{},
		// Resource within OpenAPI document for which to render explain schema
		gvr schema.GroupVersionResource,
		// Field path of child of resource to focus output onto
		fieldSelector []string,
		// Boolean indicating whether the fields should be rendered recursively/deeply
		recursive bool,
		// Output writer
		writer io.Writer,
		depth int,
	) error
}

type TemplateContext struct {
	GVR       schema.GroupVersionResource
	Document  map[string]interface{}
	Recursive bool
	FieldPath []string
	Depth     int
}

type generator struct {
	templates map[string]*template.Template
}

func NewGenerator() Generator {
	return &generator{
		templates: make(map[string]*template.Template),
	}
}

func (g *generator) AddTemplate(name string, contents string) error {
	compiled, err := WithBuiltinTemplateFuncs(template.New(name)).Parse(contents)

	if err != nil {
		return err
	}

	g.templates[name] = compiled
	return nil
}

func (g *generator) Render(
	// Template to use for rendering
	templateName string,
	// Self-Contained OpenAPI Document Containing all schemas used by $ref
	// Only OpenAPI V3 documents are supported
	document map[string]interface{},
	// Resource within OpenAPI document for which to render explain schema
	gvr schema.GroupVersionResource,
	// Field path of child of resource to focus output onto
	fieldSelector []string,
	// Boolean indicating whether the fields should be rendered recursively/deeply
	recursive bool,
	// Output writer
	writer io.Writer,
	depth int,
) error {
	compiledTemplate, ok := g.templates[templateName]
	if !ok {
		return fmt.Errorf("unrecognized format: %s", templateName)
	}

	err := compiledTemplate.Execute(writer, TemplateContext{
		Document:  document,
		Recursive: recursive,
		FieldPath: fieldSelector,
		GVR:       gvr,
		Depth:     depth,
	})
	return err
}
