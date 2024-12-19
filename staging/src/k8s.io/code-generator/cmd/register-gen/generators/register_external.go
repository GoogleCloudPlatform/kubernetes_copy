/*
Copyright 2018 The Kubernetes Authors.

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

package generators

import (
	"io"
	"sort"

	clientgentypes "k8s.io/code-generator/cmd/client-gen/types"
	"k8s.io/gengo/v2/generator"
	"k8s.io/gengo/v2/namer"
	"k8s.io/gengo/v2/types"
)

type registerExternalGenerator struct {
	generator.GoGenerator
	outputPackage   string
	gv              clientgentypes.GroupVersion
	typesToGenerate []*types.Type
	imports         namer.ImportTracker
}

var _ generator.Generator = &registerExternalGenerator{}

func (g *registerExternalGenerator) Filter(_ *generator.Context, _ *types.Type) bool {
	return false
}

func (g *registerExternalGenerator) Imports(c *generator.Context) (imports []string) {
	return g.imports.ImportLines()
}

func (g *registerExternalGenerator) Namers(_ *generator.Context) namer.NameSystems {
	return namer.NameSystems{
		"raw": namer.NewRawNamer(g.outputPackage, g.imports),
	}
}

func (g *registerExternalGenerator) Finalize(context *generator.Context, w io.Writer) error {
	typesToGenerateOnlyNames := make([]string, len(g.typesToGenerate))
	for index, typeToGenerate := range g.typesToGenerate {
		typesToGenerateOnlyNames[index] = typeToGenerate.Name.Name
	}

	// sort the list of types to register, so that the generator produces stable output
	sort.Strings(typesToGenerateOnlyNames)

	sw := generator.NewSnippetWriter(w, context, "$", "$")
	m := map[string]interface{}{
		"groupName":           g.gv.Group,
		"version":             g.gv.Version,
		"types":               typesToGenerateOnlyNames,
		"addToGroupVersion":   context.Universe.Function(types.Name{Package: "k8s.io/apimachinery/pkg/apis/meta/v1", Name: "AddToGroupVersion"}),
		"groupVersion":        context.Universe.Type(types.Name{Package: "k8s.io/apimachinery/pkg/apis/meta/v1", Name: "GroupVersion"}),
		"schemaGroupVersion":  context.Universe.Type(types.Name{Package: "k8s.io/apimachinery/pkg/runtime/schema", Name: "GroupVersion"}),
		"schemaGroupResource": context.Universe.Type(types.Name{Package: "k8s.io/apimachinery/pkg/runtime/schema", Name: "GroupResource"}),
		"scheme":              context.Universe.Type(types.Name{Package: "k8s.io/apimachinery/pkg/runtime", Name: "Scheme"}),
		"schemeBuilder":       context.Universe.Type(types.Name{Package: "k8s.io/apimachinery/pkg/runtime", Name: "SchemeBuilder"}),
	}
	sw.Do(registerExternalTypesTemplate, m)
	return sw.Error()
}

var registerExternalTypesTemplate = `
// GroupName specifies the group name used to register the objects.
const GroupName = "$.groupName$"

// GroupVersion specifies the group and the version used to register the objects.
var GroupVersion = $.groupVersion|raw${Group: GroupName, Version: "$.version$"}

// SchemeGroupVersion is group version used to register these objects
// Deprecated: use GroupVersion instead.
var SchemeGroupVersion = $.schemaGroupVersion|raw${Group: GroupName, Version: "$.version$"}

// Resource takes an unqualified resource and returns a Group qualified GroupResource
func Resource(resource string) $.schemaGroupResource|raw$ {
	return SchemeGroupVersion.WithResource(resource).GroupResource()
}

var (
	// localSchemeBuilder and AddToScheme will stay in k8s.io/kubernetes.
	SchemeBuilder      $.schemeBuilder|raw$
	localSchemeBuilder = &SchemeBuilder
    // Deprecated: use Install instead
	AddToScheme        = localSchemeBuilder.AddToScheme
	Install            = localSchemeBuilder.AddToScheme
)

func init() {
	// We only register manually written functions here. The registration of the
	// generated functions takes place in the generated files. The separation
	// makes the code compile even when the generated files are missing.
	localSchemeBuilder.Register(addKnownTypes)
}

// Adds the list of known types to Scheme.
func addKnownTypes(scheme *$.scheme|raw$) error {
	scheme.AddKnownTypes(SchemeGroupVersion,
    $range .types -$
        &$.${},
    $end$
	)
    // AddToGroupVersion allows the serialization of client types like ListOptions.
	$.addToGroupVersion|raw$(scheme, SchemeGroupVersion)
	return nil
}
`
