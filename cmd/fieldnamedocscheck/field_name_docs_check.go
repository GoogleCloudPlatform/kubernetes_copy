/*
Copyright 2023 The Kubernetes Authors.

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

package main

import (
	"fmt"
	"os"
	"regexp"
	"strings"

	flag "github.com/spf13/pflag"
	kruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
)

var (
	typeSrc = flag.StringP("type-src", "s", "", "From where we are going to read the types")
	re      = regexp.MustCompile("`(\\b\\w+\\b)`")
)

// kubeTypesMap is a map from field name to its tag name and doc.
type kubeTypesMap map[string]kruntime.Pair

func main() {
	logger := klog.Background()
	flag.Parse()

	if *typeSrc == "" {
		logger.Error(nil, "Please define -s flag as it is the api type file")
	}

	docsForTypes := kruntime.ParseDocumentationFrom(*typeSrc)
	rc := false

	for _, ks := range docsForTypes {
		typesMap := make(kubeTypesMap)

		for _, p := range ks[1:] {
			// skip the field with no tag name
			if p.Name != "" {
				typesMap[strings.ToLower(p.Name)] = p
			}
		}

		structName := ks[0].Name

		rc = checkFieldNameAndDoc(structName, "", ks[0].Doc, typesMap) || rc
		for _, p := range ks[1:] {
			rc = checkFieldNameAndDoc(structName, p.Name, p.Doc, typesMap) || rc
		}
	}

	if rc {
		os.Exit(1)
	}
}

func checkFieldNameAndDoc(structName, fieldName, doc string, typesMap kubeTypesMap) bool {
	rc := false
	visited := sets.Set[string]{}

	// The rule is:
	// 1. Get all back-tick quoted names in the doc
	// 2. Skip the name which is already found mismatched.
	// 3. Skip the name whose lowercase is different from the lowercase of tag names,
	//    because some docs use back-tick to quote field value or nil
	// 4. Check if the name is different from its tag name

	// TODO: a manual pass adding back-ticks to the doc strings, then update the linter to
	// check the existence of back-ticks
	nameGroups := re.FindAllStringSubmatch(doc, -1)
	for _, nameGroup := range nameGroups {
		name := nameGroup[1]
		if visited.Has(name) {
			continue
		}
		if p, ok := typesMap[strings.ToLower(name)]; ok && p.Name != name {
			rc = true
			visited.Insert(name)

			fmt.Fprintf(os.Stderr, "Error: doc for %s", structName)
			if fieldName != "" {
				fmt.Fprintf(os.Stderr, ".%s", fieldName)
			}

			fmt.Fprintf(os.Stderr, " contains: %s, which should be: %s\n", name, p.Name)
		}
	}

	return rc
}
