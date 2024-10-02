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
	"fmt"
	"io"
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/util/jsonpath"
	"k8s.io/kube-openapi/pkg/util/proto"
)

type fieldsPrinter interface {
	PrintFields(proto.Schema) error
}

// jsonPathParse gets back the inner list of nodes we want to work with
func jsonPathParse(in string) ([]jsonpath.Node, error) {
	// Remove trailing period just in case
	in = strings.TrimSuffix(in, ".")

	// Define initial jsonpath Parser
	jpp, err := jsonpath.Parse("user", "{."+in+"}")
	if err != nil {
		return nil, err
	}

	// Because of the way the jsonpath library works, the schema of the parser is [][]NodeList
	// meaning we need to get the outer node list, make sure it's only length 1, then get the inner node
	// list, and only then can we look at the individual nodes themselves.
	outerNodeList := jpp.Root.Nodes
	if len(outerNodeList) != 1 {
		return nil, fmt.Errorf("must pass in 1 jsonpath string")
	}

	// The root node is always a list node so this type assertion is safe
	return outerNodeList[0].(*jsonpath.ListNode).Nodes, nil
}

// SplitAndParseResourceRequest separates the users input into a model and fields
func SplitAndParseResourceRequest(inResource string, mapper meta.RESTMapper) (schema.GroupVersionResource, []string, error) {
	inResourceNodeList, err := jsonPathParse(inResource)
	if err != nil {
		return schema.GroupVersionResource{}, nil, err
	}

	if inResourceNodeList[0].Type() != jsonpath.NodeField {
		return schema.GroupVersionResource{}, nil, fmt.Errorf("invalid jsonpath syntax, first node must be field node")
	}
	resource := inResourceNodeList[0].(*jsonpath.FieldNode).Value
	gvr, err := mapper.ResourceFor(schema.GroupVersionResource{Resource: resource})
	if err != nil {
		return schema.GroupVersionResource{}, nil, err
	}

	var fieldsPath []string
	for _, node := range inResourceNodeList[1:] {
		if node.Type() != jsonpath.NodeField {
			return schema.GroupVersionResource{}, nil, fmt.Errorf("invalid jsonpath syntax, all nodes must be field nodes")
		}
		fieldsPath = append(fieldsPath, node.(*jsonpath.FieldNode).Value)
	}

	return gvr, fieldsPath, nil
}

// SplitAndParseResourceRequestWithMatchingPrefix separates the users input into a model and fields
// while selecting gvr whose (resource, group) prefix matches the resource
func SplitAndParseResourceRequestWithMatchingPrefix(inResource string, mapper meta.RESTMapper) (gvr schema.GroupVersionResource, fieldsPath []string, err error) {
	inResourceNodeList, err := jsonPathParse(inResource)
	if err != nil {
		return schema.GroupVersionResource{}, nil, err
	}

	// Get resource from first node of jsonpath
	if inResourceNodeList[0].Type() != jsonpath.NodeField {
		return schema.GroupVersionResource{}, nil, fmt.Errorf("invalid jsonpath syntax, first node must be field node")
	}
	resource := inResourceNodeList[0].(*jsonpath.FieldNode).Value

	gvrs, err := mapper.ResourcesFor(schema.GroupVersionResource{Resource: resource})
	if err != nil {
		return schema.GroupVersionResource{}, nil, err
	}

	for _, gvrItem := range gvrs {
		// Find first gvr whose gr prefixes requested resource
		groupResource := gvrItem.GroupResource().String()
		if strings.HasPrefix(inResource, groupResource) {
			resourceSuffix := inResource[len(groupResource):]
			var fieldsPath []string
			if len(resourceSuffix) > 0 {
				// Define another jsonpath Parser for the resource suffix
				resourceSuffixNodeList, err := jsonPathParse(resourceSuffix)
				if err != nil {
					return schema.GroupVersionResource{}, nil, err
				}

				if len(resourceSuffixNodeList) > 0 {
					nodeList := resourceSuffixNodeList[1:]
					for _, node := range nodeList {
						if node.Type() != jsonpath.NodeField {
							return schema.GroupVersionResource{}, nil, fmt.Errorf("invalid jsonpath syntax, first node must be field node")
						}
						fieldsPath = append(fieldsPath, node.(*jsonpath.FieldNode).Value)
					}
				}
			}
			return gvrItem, fieldsPath, nil
		}
	}

	// If no match, take the first (the highest priority) gvr
	fieldsPath = []string{}
	if len(gvrs) > 0 {
		gvr = gvrs[0]

		fieldsPathNodeList, err := jsonPathParse(inResource)
		if err != nil {
			return schema.GroupVersionResource{}, nil, err
		}

		for _, node := range fieldsPathNodeList[1:] {
			if node.Type() != jsonpath.NodeField {
				return schema.GroupVersionResource{}, nil, fmt.Errorf("invalid jsonpath syntax, first node must be field node")
			}
			fieldsPath = append(fieldsPath, node.(*jsonpath.FieldNode).Value)
		}
	}

	return gvr, fieldsPath, nil
}

// PrintModelDescription prints the description of a specific model or dot path.
// If recursive, all components nested within the fields of the schema will be
// printed.
func PrintModelDescription(fieldsPath []string, w io.Writer, schema proto.Schema, gvk schema.GroupVersionKind, recursive bool, depth int) error {
	fieldName := ""
	if len(fieldsPath) != 0 {
		fieldName = fieldsPath[len(fieldsPath)-1]
	}

	// Go down the fieldsPath to find what we're trying to explain
	schema, err := LookupSchemaForField(schema, fieldsPath)
	if err != nil {
		return err
	}
	b := fieldsPrinterBuilder{Recursive: recursive, Depth: depth}
	f := &Formatter{Writer: w, Wrap: 80}
	return PrintModel(fieldName, f, b, schema, gvk, depth)
}
