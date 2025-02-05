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
	"k8s.io/kube-openapi/pkg/util/proto"
)

// indentPerLevel is the level of indentation for each field recursion.
const indentPerLevel = 3

// recursiveFieldsPrinter recursively prints all the fields for a given
// schema.
type recursiveFieldsPrinter struct {
	Writer       *Formatter
	Error        error
	Depth        int
	CurrentDepth int
}

var _ proto.SchemaVisitor = &recursiveFieldsPrinter{}
var _ fieldsPrinter = &recursiveFieldsPrinter{}
var visitedReferences = map[string]struct{}{}

// VisitArray is just a passthrough.
func (f *recursiveFieldsPrinter) VisitArray(a *proto.Array) {
	a.SubType.Accept(f)
}

// VisitKind prints all its fields with their type, and then recurses
// inside each of these (pre-order).
func (f *recursiveFieldsPrinter) VisitKind(k *proto.Kind) {
	for _, key := range k.Keys() {
		if f.Depth > 0 && f.CurrentDepth >= f.Depth {
			continue
		}

		v := k.Fields[key]
		f.Writer.Write("%s\t<%s>", key, GetTypeName(v))
		subFields := &recursiveFieldsPrinter{
			Writer:       f.Writer.Indent(indentPerLevel),
			Depth:        f.Depth,
			CurrentDepth: f.CurrentDepth + 1, // Increment the current depth
		}
		if err := subFields.PrintFields(v); err != nil {
			f.Error = err
			return
		}
	}
}

// VisitMap is just a passthrough.
func (f *recursiveFieldsPrinter) VisitMap(m *proto.Map) {
	m.SubType.Accept(f)
}

// VisitPrimitive does nothing, since it doesn't have sub-fields.
func (f *recursiveFieldsPrinter) VisitPrimitive(p *proto.Primitive) {
	// Nothing to do.
}

// VisitReference is just a passthrough.
func (f *recursiveFieldsPrinter) VisitReference(r proto.Reference) {
	if _, ok := visitedReferences[r.Reference()]; ok {
		return
	}
	visitedReferences[r.Reference()] = struct{}{}
	r.SubSchema().Accept(f)
	delete(visitedReferences, r.Reference())
}

// PrintFields will recursively print all the fields for the given
// schema.
func (f *recursiveFieldsPrinter) PrintFields(schema proto.Schema) error {
	schema.Accept(f)
	return f.Error
}
