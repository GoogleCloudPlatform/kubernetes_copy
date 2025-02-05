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
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
	cliflag "k8s.io/component-base/cli/flag"
)

const wantTmpl = `%s flags
Warning: This endpoint is not meant to be machine parseable, has no formatting compatibility guarantees and is for debugging purposes only.
`

func TestFlagz(t *testing.T) {
	componentName := "test-server"
	delimiters = []string{"="}
	wantHeaderLines := strings.Split(fmt.Sprintf(wantTmpl, componentName), "\n")
	tests := []struct {
		name        string
		header      string
		flagzReader Reader
		wantStatus  int
		wantResp    []string
	}{
		{
			name:       "nil flags",
			wantStatus: http.StatusOK,
			wantResp:   wantHeaderLines,
		},
		{
			name:       "unaccepted header",
			header:     "some header",
			wantStatus: http.StatusNotAcceptable,
		},
		{
			name: "test flags",
			flagzReader: NamedFlagSetsReader{
				FlagSets: cliflag.NamedFlagSets{
					FlagSets: map[string]*pflag.FlagSet{
						"test": flagSet(t, map[string]flagValue{
							"test-flag-bar": {
								value:     "test-value-bar",
								sensitive: false,
							},
							"test-flag-foo": {
								value:     "test-value-foo",
								sensitive: false,
							},
						}),
					},
				},
			},
			wantStatus: http.StatusOK,
			wantResp: append(wantHeaderLines,
				"test-flag-bar=test-value-bar",
				"test-flag-foo=test-value-foo",
			),
		},
	}

	for i, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mux := http.NewServeMux()
			Install(mux, componentName, test.flagzReader)

			req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("http://example.com%s", DefaultFlagzPath), nil)
			if err != nil {
				t.Fatalf("case[%d] Unexpected error: %v", i, err)
			}

			req.Header.Set("Accept", "text/plain; charset=utf-8")
			if test.header != "" {
				req.Header.Set("Accept", test.header)
			}

			w := httptest.NewRecorder()
			mux.ServeHTTP(w, req)
			assert.Equal(t, test.wantStatus, w.Code, "case[%s] Expected status code %d, got %d", test.name, test.wantStatus, w.Code)

			if test.wantStatus == http.StatusOK {
				assert.Equal(t, "text/plain; charset=utf-8", w.Header().Get("Content-Type"), "case[%s] Incorrect Content-Type header", test.name)

				gotLines := strings.Split(w.Body.String(), "\n")
				gotLines = trimEmptyLines(gotLines)
				sort.Strings(gotLines)

				sort.Strings(test.wantResp)
				wantLines := trimEmptyLines(test.wantResp)

				assert.Equal(t, wantLines, gotLines, "case[%s] Response body mismatch", test.name)
			}
		})
	}
}

func trimEmptyLines(lines []string) []string {
	var result []string
	for _, line := range lines {
		if line != "" {
			result = append(result, line)
		}
	}
	return result
}
