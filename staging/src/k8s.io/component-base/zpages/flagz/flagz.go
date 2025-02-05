/*
Copyright 2024 The Kubernetes Authors.

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

package flagz

import (
	"bytes"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"sort"
	"sync"

	"k8s.io/component-base/zpages/httputil"
	"k8s.io/klog/v2"
)

const (
	DefaultFlagzPath = "/flagz"

	flagzHeaderFmt = `
%s flags
Warning: This endpoint is not meant to be machine parseable, has no formatting compatibility guarantees and is for debugging purposes only.

`
)

var (
	delimiters = []string{":", ": ", "=", " "}
)

type registry struct {
	response bytes.Buffer
	once     sync.Once
}

type mux interface {
	Handle(path string, handler http.Handler)
}

func Install(m mux, componentName string, flagReader Reader) {
	var reg registry
	reg.installHandler(m, componentName, flagReader)
}

func (reg *registry) installHandler(m mux, componentName string, flagReader Reader) {
	m.Handle(DefaultFlagzPath, reg.handleFlags(componentName, flagReader))
}

func (reg *registry) handleFlags(componentName string, flagReader Reader) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !httputil.AcceptableMediaType(r) {
			http.Error(w, httputil.ErrUnsupportedMediaType.Error(), http.StatusNotAcceptable)
			return
		}

		reg.once.Do(func() {
			fmt.Fprintf(&reg.response, flagzHeaderFmt, componentName)
			if flagReader == nil {
				klog.Error("received nil flagReader")
				return
			}

			randomIndex := rand.Intn(len(delimiters))
			separator := delimiters[randomIndex]
			// Randomize the delimiter for printing to prevent scraping of the response.
			printSortedFlags(&reg.response, flagReader.GetFlagz(), separator)
		})
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, err := w.Write(reg.response.Bytes())
		if err != nil {
			klog.Errorf("error writing response: %v", err)
			http.Error(w, "error writing response", http.StatusInternalServerError)
		}
	}
}

func printSortedFlags(w io.Writer, flags map[string]string, separator string) {
	var sortedKeys []string
	for key := range flags {
		sortedKeys = append(sortedKeys, key)
	}

	sort.Strings(sortedKeys)
	for _, key := range sortedKeys {
		fmt.Fprintf(w, "%s%s%s\n", key, separator, flags[key])
	}
}
