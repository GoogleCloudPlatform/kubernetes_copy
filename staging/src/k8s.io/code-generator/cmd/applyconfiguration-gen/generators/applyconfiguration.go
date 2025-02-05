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

package generators

import (
	"io"
	"path"
	"slices"
	"strings"

	"k8s.io/gengo/v2/generator"
	"k8s.io/gengo/v2/namer"
	"k8s.io/gengo/v2/types"
	"k8s.io/klog/v2"

	"k8s.io/code-generator/cmd/client-gen/generators/util"
	clientgentypes "k8s.io/code-generator/cmd/client-gen/types"
)

// applyConfigurationGenerator produces apply configurations for a given GroupVersion and type.
type applyConfigurationGenerator struct {
	generator.GoGenerator
	// outPkgBase is the base package, under which the "internal" and GV-specific subdirs live
	outPkgBase   string // must be a Go import-path
	localPkg     string
	groupVersion clientgentypes.GroupVersion
	applyConfig  applyConfig
	imports      namer.ImportTracker
	refGraph     refGraph
	openAPIType  *string // if absent, extraction function cannot be generated
}

var _ generator.Generator = &applyConfigurationGenerator{}

func (g *applyConfigurationGenerator) Filter(_ *generator.Context, t *types.Type) bool {
	return t == g.applyConfig.Type
}

func (g *applyConfigurationGenerator) Namers(*generator.Context) namer.NameSystems {
	return namer.NameSystems{
		"raw":          namer.NewRawNamer(g.localPkg, g.imports),
		"singularKind": namer.NewPublicNamer(0),
	}
}

func (g *applyConfigurationGenerator) Imports(*generator.Context) (imports []string) {
	return g.imports.ImportLines()
}

// TypeParams provides a struct that an apply configuration
// is generated for as well as the apply configuration details
// and types referenced by the struct.
type TypeParams struct {
	Struct      *types.Type
	ApplyConfig applyConfig
	Tags        util.Tags
	APIVersion  string
	ExtractInto *types.Type
	ParserFunc  *types.Type
	OpenAPIType *string
}

type memberParams struct {
	TypeParams
	Member     types.Member
	MemberType *types.Type
	JSONTags   JSONTags
	ArgType    *types.Type   // only set for maps and slices
	EmbeddedIn *memberParams // parent embedded member, if any
}

func (g *applyConfigurationGenerator) GenerateType(c *generator.Context, t *types.Type, w io.Writer) error {
	sw := generator.NewSnippetWriter(w, c, "$", "$")

	klog.V(5).Infof("processing type %v", t)
	typeParams := TypeParams{
		Struct:      t,
		ApplyConfig: g.applyConfig,
		Tags:        genclientTags(t),
		APIVersion:  g.groupVersion.ToAPIVersion(),
		ExtractInto: extractInto,
		ParserFunc:  types.Ref(path.Join(g.outPkgBase, "internal"), "Parser"),
		OpenAPIType: g.openAPIType,
	}

	g.generateStruct(sw, typeParams)

	if typeParams.Tags.GenerateClient {
		if typeParams.Tags.NonNamespaced {
			sw.Do(clientgenTypeConstructorNonNamespaced, typeParams)
		} else {
			sw.Do(clientgenTypeConstructorNamespaced, typeParams)
		}
		if typeParams.OpenAPIType != nil {
			g.generateClientgenExtract(sw, typeParams, !typeParams.Tags.NoStatus)
		}
		g.generateIsApplyConfiguration(typeParams.ApplyConfig.ApplyConfiguration, sw)
	} else {
		if hasTypeMetaField(t) {
			sw.Do(constructorWithTypeMeta, typeParams)
		} else {
			sw.Do(constructor, typeParams)
		}
	}
	g.generateWithFuncs(t, typeParams, sw, nil, &[]string{})
	g.generateGetters(t, typeParams, sw, nil)
	return sw.Error()
}

func hasTypeMetaField(t *types.Type) bool {
	for _, member := range t.Members {
		if typeMeta.Name == member.Type.Name && member.Embedded {
			return true
		}
	}
	return false
}

func blocklisted(t *types.Type, member types.Member) bool {
	if objectMeta.Name == t.Name && member.Name == "ManagedFields" {
		return true
	}
	if objectMeta.Name == t.Name && member.Name == "SelfLink" {
		return true
	}
	// Hide any fields which are en route to deletion.
	if strings.HasPrefix(member.Name, "ZZZ_") {
		return true
	}
	return false
}

func needsGetter(t *types.Type, member types.Member) bool {
	// Needed when applying an ApplyConfiguration
	return objectMeta.Name == t.Name && member.Name == "Name"
}

func (g *applyConfigurationGenerator) generateGetters(t *types.Type, typeParams TypeParams, sw *generator.SnippetWriter, embed *memberParams) {
	for _, member := range t.Members {
		if blocklisted(t, member) {
			continue
		}
		memberType := g.refGraph.applyConfigForType(member.Type)
		if g.refGraph.isApplyConfig(member.Type) {
			memberType = &types.Type{Kind: types.Pointer, Elem: memberType}
		}
		if jsonTags, ok := lookupJSONTags(member); ok {
			memberParams := memberParams{
				TypeParams: typeParams,
				Member:     member,
				MemberType: memberType,
				JSONTags:   jsonTags,
				EmbeddedIn: embed,
			}
			if memberParams.Member.Embedded {
				g.generateGetters(member.Type, typeParams, sw, &memberParams)
				continue
			}

			if needsGetter(t, member) {
				g.generateMemberGetter(sw, memberParams)
			}
		}
	}
}

func (g *applyConfigurationGenerator) generateWithFuncs(t *types.Type, typeParams TypeParams, sw *generator.SnippetWriter, embed *memberParams,
	generated *[]string) {
	for _, member := range t.Members {
		if blocklisted(t, member) {
			continue
		}
		memberType := g.refGraph.applyConfigForType(member.Type)
		if g.refGraph.isApplyConfig(member.Type) {
			memberType = &types.Type{Kind: types.Pointer, Elem: memberType}
		}
		if jsonTags, ok := lookupJSONTags(member); ok {
			if slices.Contains(*generated, member.Name) {
				klog.V(5).Infof("With%s already generated on %s, skipping\n", member.Name, t.Name)
				continue
			}
			*generated = append(*generated, member.Name)
			memberParams := memberParams{
				TypeParams: typeParams,
				Member:     member,
				MemberType: memberType,
				JSONTags:   jsonTags,
				EmbeddedIn: embed,
			}
			if memberParams.Member.Embedded {
				g.generateWithFuncs(member.Type, typeParams, sw, &memberParams, generated)
				if !jsonTags.inline {
					// non-inlined embeds are nillable and need a "ensure exists" utility function
					sw.Do(ensureEmbedExists, memberParams)
				}
				continue
			}

			// For slices where the items are generated apply configuration types, accept varargs of
			// pointers of the type as "with" function arguments so the "with" function can be used like so:
			// WithFoos(Foo().WithName("x"), Foo().WithName("y"))
			if t := deref(member.Type); t.Kind == types.Slice && g.refGraph.isApplyConfig(t.Elem) {
				memberParams.ArgType = &types.Type{Kind: types.Pointer, Elem: memberType.Elem}
				g.generateMemberWithForSlice(sw, member, memberParams)
				continue
			}
			// Note: There are no maps where the values are generated apply configurations (because
			// associative lists are used instead). So if a type like this is ever introduced, the
			// default "with" function generator will produce a working (but not entirely convenient "with" function)
			// that would be used like so:
			// WithMap(map[string]FooApplyConfiguration{*Foo().WithName("x")})

			switch memberParams.Member.Type.Kind {
			case types.Slice:
				memberParams.ArgType = memberType.Elem
				g.generateMemberWithForSlice(sw, member, memberParams)
			case types.Map:
				g.generateMemberWithForMap(sw, memberParams)
			default:
				g.generateMemberWith(sw, memberParams)
			}
		}
	}
}

func (g *applyConfigurationGenerator) generateStruct(sw *generator.SnippetWriter, typeParams TypeParams) {
	sw.Do("// $.ApplyConfig.ApplyConfiguration|public$ represents a declarative configuration of the $.ApplyConfig.Type|public$ type for use\n", typeParams)
	sw.Do("// with apply.\n", typeParams)
	sw.Do("type $.ApplyConfig.ApplyConfiguration|public$ struct {\n", typeParams)
	for _, structMember := range typeParams.Struct.Members {
		if blocklisted(typeParams.Struct, structMember) {
			continue
		}
		if structMemberTags, ok := lookupJSONTags(structMember); ok {
			if !structMemberTags.inline {
				structMemberTags.omitempty = true
			}
			params := memberParams{
				TypeParams: typeParams,
				Member:     structMember,
				MemberType: g.refGraph.applyConfigForType(structMember.Type),
				JSONTags:   structMemberTags,
			}
			if structMember.Embedded {
				if structMemberTags.inline {
					sw.Do("$.MemberType|raw$ `json:\"$.JSONTags$\"`\n", params)
				} else {
					sw.Do("*$.MemberType|raw$ `json:\"$.JSONTags$\"`\n", params)
				}
			} else if isNillable(structMember.Type) {
				sw.Do("$.Member.Name$ $.MemberType|raw$ `json:\"$.JSONTags$\"`\n", params)
			} else {
				sw.Do("$.Member.Name$ *$.MemberType|raw$ `json:\"$.JSONTags$\"`\n", params)
			}
		}
	}
	sw.Do("}\n", typeParams)
}

func (g *applyConfigurationGenerator) generateIsApplyConfiguration(t *types.Type, sw *generator.SnippetWriter) {
	sw.Do("func (b $.|public$) IsApplyConfiguration() bool {\n", t)
	sw.Do("  return true\n", nil)
	sw.Do("}\n", nil)
}

func deref(t *types.Type) *types.Type {
	for t.Kind == types.Pointer {
		t = t.Elem
	}
	return t
}

func isNillable(t *types.Type) bool {
	return t.Kind == types.Slice || t.Kind == types.Map
}

func (g *applyConfigurationGenerator) generateMemberWith(sw *generator.SnippetWriter, memberParams memberParams) {
	sw.Do("// With$.Member.Name$ sets the $.Member.Name$ field in the declarative configuration to the given value\n", memberParams)
	sw.Do("// and returns the receiver, so that objects can be built by chaining \"With\" function invocations.\n", memberParams)
	sw.Do("// If called multiple times, the $.Member.Name$ field is set to the value of the last call.\n", memberParams)
	sw.Do("func (b *$.ApplyConfig.ApplyConfiguration|public$) With$.Member.Name$(value $.MemberType|raw$) *$.ApplyConfig.ApplyConfiguration|public$ {\n", memberParams)
	g.ensureEmbedExistsIfApplicable(sw, memberParams)
	if g.refGraph.isApplyConfig(memberParams.Member.Type) || isNillable(memberParams.Member.Type) {
		sw.Do("b$if ne .EmbeddedIn nil$.$.EmbeddedIn.MemberType.Elem.Name.Name$$end$.$.Member.Name$ = value\n", memberParams)
	} else {
		sw.Do("b$if ne .EmbeddedIn nil$.$.EmbeddedIn.MemberType.Elem.Name.Name$$end$.$.Member.Name$ = &value\n", memberParams)
	}
	sw.Do("  return b\n", memberParams)
	sw.Do("}\n", memberParams)
}

func (g *applyConfigurationGenerator) generateMemberGetter(sw *generator.SnippetWriter, memberParams memberParams) {
	sw.Do("// Get$.Member.Name$ retrieves the value of the $.Member.Name$ field in the declarative configuration.\n", memberParams)
	if g.refGraph.isApplyConfig(memberParams.Member.Type) || isNillable(memberParams.Member.Type) {
		sw.Do("func (b *$.ApplyConfig.ApplyConfiguration|public$) Get$.Member.Name$() $.MemberType|raw$ {\n", memberParams)
	} else {
		sw.Do("func (b *$.ApplyConfig.ApplyConfiguration|public$) Get$.Member.Name$() *$.MemberType|raw$ {\n", memberParams)
	}
	g.ensureEmbedExistsIfApplicable(sw, memberParams)
	sw.Do("  return b$if ne .EmbeddedIn nil$.$.EmbeddedIn.MemberType.Elem.Name.Name$$end$.$.Member.Name$\n", memberParams)
	sw.Do("}\n", memberParams)
}

func (g *applyConfigurationGenerator) generateMemberWithForSlice(sw *generator.SnippetWriter, member types.Member, memberParams memberParams) {
	memberIsPointerToSlice := member.Type.Kind == types.Pointer
	if memberIsPointerToSlice {
		sw.Do(ensureNonEmbedSliceExists, memberParams)
	}

	sw.Do("// With$.Member.Name$ adds the given value to the $.Member.Name$ field in the declarative configuration\n", memberParams)
	sw.Do("// and returns the receiver, so that objects can be build by chaining \"With\" function invocations.\n", memberParams)
	sw.Do("// If called multiple times, values provided by each call will be appended to the $.Member.Name$ field.\n", memberParams)
	sw.Do("func (b *$.ApplyConfig.ApplyConfiguration|public$) With$.Member.Name$(values ...$.ArgType|raw$) *$.ApplyConfig.ApplyConfiguration|public$ {\n", memberParams)
	g.ensureEmbedExistsIfApplicable(sw, memberParams)

	if memberIsPointerToSlice {
		sw.Do("b.ensure$.MemberType.Elem|public$Exists()\n", memberParams)
	}

	sw.Do("  for i := range values {\n", memberParams)
	if memberParams.ArgType.Kind == types.Pointer {
		sw.Do("if values[i] == nil {\n", memberParams)
		sw.Do("  panic(\"nil value passed to With$.Member.Name$\")\n", memberParams)
		sw.Do("}\n", memberParams)

		if memberIsPointerToSlice {
			sw.Do("*b$if ne .EmbeddedIn nil$.$.EmbeddedIn.MemberType.Elem.Name.Name$$end$.$.Member.Name$ = append(*b$if ne .EmbeddedIn nil$.$.EmbeddedIn.MemberType.Elem.Name.Name$$end$.$.Member.Name$, *values[i])\n", memberParams)
		} else {
			sw.Do("b$if ne .EmbeddedIn nil$.$.EmbeddedIn.MemberType.Elem.Name.Name$$end$.$.Member.Name$ = append(b$if ne .EmbeddedIn nil$.$.EmbeddedIn.MemberType.Elem.Name.Name$$end$.$.Member.Name$, *values[i])\n", memberParams)
		}
	} else {
		if memberIsPointerToSlice {
			sw.Do("*b$if ne .EmbeddedIn nil$.$.EmbeddedIn.MemberType.Elem.Name.Name$$end$.$.Member.Name$ = append(*b$if ne .EmbeddedIn nil$.$.EmbeddedIn.MemberType.Elem.Name.Name$$end$.$.Member.Name$, values[i])\n", memberParams)
		} else {
			sw.Do("b$if ne .EmbeddedIn nil$.$.EmbeddedIn.MemberType.Elem.Name.Name$$end$.$.Member.Name$ = append(b$if ne .EmbeddedIn nil$.$.EmbeddedIn.MemberType.Elem.Name.Name$$end$.$.Member.Name$, values[i])\n", memberParams)
		}
	}
	sw.Do("  }\n", memberParams)
	sw.Do("  return b\n", memberParams)
	sw.Do("}\n", memberParams)
}

func (g *applyConfigurationGenerator) generateMemberWithForMap(sw *generator.SnippetWriter, memberParams memberParams) {
	sw.Do("// With$.Member.Name$ puts the entries into the $.Member.Name$ field in the declarative configuration\n", memberParams)
	sw.Do("// and returns the receiver, so that objects can be build by chaining \"With\" function invocations.\n", memberParams)
	sw.Do("// If called multiple times, the entries provided by each call will be put on the $.Member.Name$ field,\n", memberParams)
	sw.Do("// overwriting an existing map entries in $.Member.Name$ field with the same key.\n", memberParams)
	sw.Do("func (b *$.ApplyConfig.ApplyConfiguration|public$) With$.Member.Name$(entries $.MemberType|raw$) *$.ApplyConfig.ApplyConfiguration|public$ {\n", memberParams)
	g.ensureEmbedExistsIfApplicable(sw, memberParams)
	sw.Do("  if b$if ne .EmbeddedIn nil$.$.EmbeddedIn.MemberType.Elem.Name.Name$$end$.$.Member.Name$ == nil && len(entries) > 0 {\n", memberParams)
	sw.Do("    b$if ne .EmbeddedIn nil$.$.EmbeddedIn.MemberType.Elem.Name.Name$$end$.$.Member.Name$ = make($.MemberType|raw$, len(entries))\n", memberParams)
	sw.Do("  }\n", memberParams)
	sw.Do("  for k, v := range entries {\n", memberParams)
	sw.Do("    b$if ne .EmbeddedIn nil$.$.EmbeddedIn.MemberType.Elem.Name.Name$$end$.$.Member.Name$[k] = v\n", memberParams)
	sw.Do("  }\n", memberParams)
	sw.Do("  return b\n", memberParams)
	sw.Do("}\n", memberParams)
}

func (g *applyConfigurationGenerator) ensureEmbedExistsIfApplicable(sw *generator.SnippetWriter, memberParams memberParams) {
	// Embedded types that are not inlined must be nillable so they are not included in the apply configuration
	// when all their fields are omitted.
	if memberParams.EmbeddedIn != nil && !memberParams.EmbeddedIn.JSONTags.inline {
		sw.Do("b.ensure$.MemberType.Elem|public$Exists()\n", memberParams.EmbeddedIn)
	}
}

var ensureEmbedExists = `
func (b *$.ApplyConfig.ApplyConfiguration|public$) ensure$.MemberType.Elem|public$Exists() {
  if b.$.MemberType.Elem|public$ == nil {
    b.$.MemberType.Elem|public$ = &$.MemberType.Elem|raw${}
  }
}
`

var ensureNonEmbedSliceExists = `
func (b *$.ApplyConfig.ApplyConfiguration|public$) ensure$.MemberType.Elem|public$Exists() {
  if b.$.Member.Name$ == nil {
    b.$.Member.Name$ = &[]$.MemberType.Elem|raw${}
  }
}
`

var clientgenTypeConstructorNamespaced = `
// $.ApplyConfig.Type|public$ constructs a declarative configuration of the $.ApplyConfig.Type|public$ type for use with
// apply.
func $.ApplyConfig.Type|public$(name, namespace string) *$.ApplyConfig.ApplyConfiguration|public$ {
  b := &$.ApplyConfig.ApplyConfiguration|public${}
  b.WithName(name)
  b.WithNamespace(namespace)
  b.WithKind("$.ApplyConfig.Type|singularKind$")
  b.WithAPIVersion("$.APIVersion$")
  return b
}
`

var clientgenTypeConstructorNonNamespaced = `
// $.ApplyConfig.Type|public$ constructs a declarative configuration of the $.ApplyConfig.Type|public$ type for use with
// apply.
func $.ApplyConfig.Type|public$(name string) *$.ApplyConfig.ApplyConfiguration|public$ {
  b := &$.ApplyConfig.ApplyConfiguration|public${}
  b.WithName(name)
  b.WithKind("$.ApplyConfig.Type|singularKind$")
  b.WithAPIVersion("$.APIVersion$")
  return b
}
`

var constructorWithTypeMeta = `
// $.ApplyConfig.ApplyConfiguration|public$ constructs a declarative configuration of the $.ApplyConfig.Type|public$ type for use with
// apply.
func $.ApplyConfig.Type|public$() *$.ApplyConfig.ApplyConfiguration|public$ {
  b := &$.ApplyConfig.ApplyConfiguration|public${}
  b.WithKind("$.ApplyConfig.Type|singularKind$")
  b.WithAPIVersion("$.APIVersion$")
  return b
}
`

var constructor = `
// $.ApplyConfig.ApplyConfiguration|public$ constructs a declarative configuration of the $.ApplyConfig.Type|public$ type for use with
// apply.
func $.ApplyConfig.Type|public$() *$.ApplyConfig.ApplyConfiguration|public$ {
  return &$.ApplyConfig.ApplyConfiguration|public${}
}
`

func (g *applyConfigurationGenerator) generateClientgenExtract(sw *generator.SnippetWriter, typeParams TypeParams, includeStatus bool) {
	sw.Do(`
// Extract$.ApplyConfig.Type|public$ extracts the applied configuration owned by fieldManager from
// $.Struct|private$. If no managedFields are found in $.Struct|private$ for fieldManager, a
// $.ApplyConfig.ApplyConfiguration|public$ is returned with only the Name, Namespace (if applicable),
// APIVersion and Kind populated. It is possible that no managed fields were found for because other
// field managers have taken ownership of all the fields previously owned by fieldManager, or because
// the fieldManager never owned fields any fields.
// $.Struct|private$ must be a unmodified $.Struct|public$ API object that was retrieved from the Kubernetes API.
// Extract$.ApplyConfig.Type|public$ provides a way to perform a extract/modify-in-place/apply workflow.
// Note that an extracted apply configuration will contain fewer fields than what the fieldManager previously
// applied if another fieldManager has updated or force applied any of the previously applied fields.
// Experimental!
func Extract$.ApplyConfig.Type|public$($.Struct|private$ *$.Struct|raw$, fieldManager string) (*$.ApplyConfig.ApplyConfiguration|public$, error) {
	return extract$.ApplyConfig.Type|public$($.Struct|private$, fieldManager, "")
}`, typeParams)
	if includeStatus {
		sw.Do(`
// Extract$.ApplyConfig.Type|public$Status is the same as Extract$.ApplyConfig.Type|public$ except
// that it extracts the status subresource applied configuration.
// Experimental!
func Extract$.ApplyConfig.Type|public$Status($.Struct|private$ *$.Struct|raw$, fieldManager string) (*$.ApplyConfig.ApplyConfiguration|public$, error) {
	return extract$.ApplyConfig.Type|public$($.Struct|private$, fieldManager, "status")
}
`, typeParams)
	}
	sw.Do(`
func extract$.ApplyConfig.Type|public$($.Struct|private$ *$.Struct|raw$, fieldManager string, subresource string) (*$.ApplyConfig.ApplyConfiguration|public$, error) {
	b := &$.ApplyConfig.ApplyConfiguration|public${}
	err := $.ExtractInto|raw$($.Struct|private$, $.ParserFunc|raw$().Type("$.OpenAPIType$"), fieldManager, b, subresource)
	if err != nil {
		return nil, err
	}
	b.WithName($.Struct|private$.Name)
`, typeParams)
	if !typeParams.Tags.NonNamespaced {
		sw.Do("b.WithNamespace($.Struct|private$.Namespace)\n", typeParams)
	}
	sw.Do(`
	b.WithKind("$.ApplyConfig.Type|singularKind$")
	b.WithAPIVersion("$.APIVersion$")
	return b, nil
}
`, typeParams)
}
