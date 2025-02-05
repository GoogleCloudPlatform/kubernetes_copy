/*
Copyright 2017 The Kubernetes Authors.

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

package explain

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/kube-openapi/pkg/util/proto"
)

const (
	// fieldIndentLevel is the level of indentation for fields.
	fieldIndentLevel = 3
	// descriptionIndentLevel is the level of indentation for the
	// description.
	descriptionIndentLevel = 5
)

// modelPrinter prints a schema in Writer. Its "Builder" will decide if
// it's recursive or not.
type modelPrinter struct {
	Name         string
	Type         string
	Descriptions []string
	Writer       *Formatter
	Builder      fieldsPrinterBuilder
	GVK          schema.GroupVersionKind
	Depth        int
	Error        error
}

var _ proto.SchemaVisitor = &modelPrinter{}

func (m *modelPrinter) PrintKindAndVersion() error {
	if err := m.Writer.Write("KIND:     %s", m.GVK.Kind); err != nil {
		return err
	}
	return m.Writer.Write("VERSION:  %s\n", m.GVK.GroupVersion())
}

// PrintDescription prints the description for a given schema. There
// might be multiple description, since we collect descriptions when we
// go through references, arrays and maps.
func (m *modelPrinter) PrintDescription(schema proto.Schema) error {
	if err := m.Writer.Write("DESCRIPTION:"); err != nil {
		return err
	}
	empty := true
	for i, desc := range append(m.Descriptions, schema.GetDescription()) {
		if desc == "" {
			continue
		}
		empty = false
		if i != 0 {
			if err := m.Writer.Write(""); err != nil {
				return err
			}
		}
		if err := m.Writer.Indent(descriptionIndentLevel).WriteWrapped("%s", desc); err != nil {
			return err
		}
	}
	if empty {
		return m.Writer.Indent(descriptionIndentLevel).WriteWrapped("<empty>")
	}
	return nil
}

// VisitArray recurses inside the subtype, while collecting the type if
// not done yet, and the description.
func (m *modelPrinter) VisitArray(a *proto.Array) {
	m.Descriptions = append(m.Descriptions, a.GetDescription())
	if m.Type == "" {
		m.Type = GetTypeName(a)
	}
	a.SubType.Accept(m)
}

// VisitKind prints a full resource with its fields.
func (m *modelPrinter) VisitKind(k *proto.Kind) {
	if err := m.PrintKindAndVersion(); err != nil {
		m.Error = err
		return
	}

	if m.Type == "" {
		m.Type = GetTypeName(k)
	}
	if m.Name != "" {
		m.Writer.Write("RESOURCE: %s <%s>\n", m.Name, m.Type)
	}

	if err := m.PrintDescription(k); err != nil {
		m.Error = err
		return
	}
	if err := m.Writer.Write("\nFIELDS:"); err != nil {
		m.Error = err
		return
	}
	m.Error = m.Builder.BuildFieldsPrinter(m.Writer.Indent(fieldIndentLevel)).PrintFields(k)
}

// VisitMap recurses inside the subtype, while collecting the type if
// not done yet, and the description.
func (m *modelPrinter) VisitMap(om *proto.Map) {
	m.Descriptions = append(m.Descriptions, om.GetDescription())
	if m.Type == "" {
		m.Type = GetTypeName(om)
	}
	om.SubType.Accept(m)
}

// VisitPrimitive prints a field type and its description.
func (m *modelPrinter) VisitPrimitive(p *proto.Primitive) {
	if err := m.PrintKindAndVersion(); err != nil {
		m.Error = err
		return
	}

	if m.Type == "" {
		m.Type = GetTypeName(p)
	}
	if err := m.Writer.Write("FIELD:    %s <%s>\n", m.Name, m.Type); err != nil {
		m.Error = err
		return
	}
	m.Error = m.PrintDescription(p)
}

func (m *modelPrinter) VisitArbitrary(a *proto.Arbitrary) {
	if err := m.PrintKindAndVersion(); err != nil {
		m.Error = err
		return
	}

	m.Error = m.PrintDescription(a)
}

// VisitReference recurses inside the subtype, while collecting the description.
func (m *modelPrinter) VisitReference(r proto.Reference) {
	m.Descriptions = append(m.Descriptions, r.GetDescription())
	r.SubSchema().Accept(m)
}

// PrintModel prints the description of a schema in writer.
func PrintModel(name string, writer *Formatter, builder fieldsPrinterBuilder, schema proto.Schema, gvk schema.GroupVersionKind, depth int) error {
	m := &modelPrinter{Name: name, Writer: writer, Builder: builder, GVK: gvk, Depth: depth}
	schema.Accept(m)
	return m.Error
}
