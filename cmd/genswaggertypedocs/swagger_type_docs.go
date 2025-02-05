/*
Copyright 2015 The Kubernetes Authors.

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
	"io"
	"os"

	kruntime "k8s.io/apimachinery/pkg/runtime"

	flag "github.com/spf13/pflag"
	"k8s.io/klog/v2"
)

var (
	functionDest = flag.StringP("func-dest", "f", "-", "Output for swagger functions; '-' means stdout (default)")
	typeSrc      = flag.StringP("type-src", "s", "", "From where we are going to read the types")
	verify       = flag.BoolP("verify", "v", false, "Verifies if the given type-src file has documentation for every type")
)

func main() {
	logger := klog.Background()
	flag.Parse()

	if *typeSrc == "" {
		logger.Error(nil, "Please define -s flag as it is the source file")
	}

	var funcOut io.Writer
	if *functionDest == "-" {
		funcOut = os.Stdout
	} else {
		file, err := os.Create(*functionDest)
		if err != nil {
			logger.Error(err, "Couldn't open.", "destination", *functionDest)
		}
		defer file.Close()
		funcOut = file
	}

	docsForTypes := kruntime.ParseDocumentationFrom(*typeSrc)

	if *verify {
		rc, err := kruntime.VerifySwaggerDocsExist(docsForTypes, funcOut)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error in verification process: %s\n", err)
		}
		os.Exit(rc)
	}

	if len(docsForTypes) > 0 {
		if err := kruntime.WriteSwaggerDocFunc(docsForTypes, funcOut); err != nil {
			fmt.Fprintf(os.Stderr, "Error when writing swagger documentation functions: %s\n", err)
			os.Exit(-1)
		}
	}
}
