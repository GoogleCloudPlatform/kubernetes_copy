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

package statusz

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"k8s.io/apimachinery/pkg/util/version"
)

const wantTmpl = `
%s statusz
Warning: This endpoint is not meant to be machine parseable, has no formatting compatibility guarantees and is for debugging purposes only.

Started: %v
Up: %s
Go version: %s
Binary version: %v
Emulation version: %v
`

const wantTmplWithoutEmulation = `
%s statusz
Warning: This endpoint is not meant to be machine parseable, has no formatting compatibility guarantees and is for debugging purposes only.

Started: %v
Up: %s
Go version: %s
Binary version: %v

`

func TestStatusz(t *testing.T) {
	delimiters = []string{":"}
	fakeStartTime := time.Now()
	fakeUptime := uptime(fakeStartTime)
	fakeGoVersion := "1.21"
	fakeBvStr := "1.31"
	fakeEvStr := "1.30"
	fakeBinaryVersion := parseVersion(t, fakeBvStr)
	fakeEmulationVersion := parseVersion(t, fakeEvStr)
	tests := []struct {
		name           string
		componentName  string
		reqHeader      string
		registry       fakeRegistry
		wantStatusCode int
		wantBody       string
	}{
		{
			name:           "invalid header",
			reqHeader:      "some header",
			wantStatusCode: http.StatusNotAcceptable,
		},
		{
			name:          "valid request",
			componentName: "test-server",
			reqHeader:     "text/plain; charset=utf-8",
			registry: fakeRegistry{
				startTime:    fakeStartTime,
				goVer:        fakeGoVersion,
				binaryVer:    fakeBinaryVersion,
				emulationVer: fakeEmulationVersion,
			},
			wantStatusCode: http.StatusOK,
			wantBody: fmt.Sprintf(
				wantTmpl,
				"test-server",
				fakeStartTime.Format(time.UnixDate),
				fakeUptime,
				fakeGoVersion,
				fakeBinaryVersion,
				fakeEmulationVersion,
			),
		},
		{
			name:          "missing emulation version",
			componentName: "test-server",
			reqHeader:     "text/plain; charset=utf-8",
			registry: fakeRegistry{
				startTime:    fakeStartTime,
				goVer:        fakeGoVersion,
				binaryVer:    fakeBinaryVersion,
				emulationVer: nil,
			},
			wantStatusCode: http.StatusOK,
			wantBody: fmt.Sprintf(
				wantTmplWithoutEmulation,
				"test-server",
				fakeStartTime.Format(time.UnixDate),
				fakeUptime,
				fakeGoVersion,
				fakeBinaryVersion,
			),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mux := http.NewServeMux()

			Install(mux, tt.componentName, tt.registry)

			req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("http://example.com%s", DefaultStatuszPath), nil)
			if err != nil {
				t.Fatalf("unexpected error while creating request: %v", err)
			}

			req.Header.Set("Accept", "text/plain; charset=utf-8")
			if tt.reqHeader != "" {
				req.Header.Set("Accept", tt.reqHeader)
			}

			w := httptest.NewRecorder()
			mux.ServeHTTP(w, req)

			if w.Code != tt.wantStatusCode {
				t.Fatalf("want status code: %v, got: %v", tt.wantStatusCode, w.Code)
			}

			if tt.wantStatusCode == http.StatusOK {
				c := w.Header().Get("Content-Type")
				if c != "text/plain; charset=utf-8" {
					t.Fatalf("want header: %v, got: %v", "text/plain", c)
				}

				if diff := cmp.Diff(tt.wantBody, string(w.Body.String())); diff != "" {
					t.Errorf("Unexpected diff on response (-want,+got):\n%s", diff)
				}
			}
		})
	}
}

func parseVersion(t *testing.T, v string) *version.Version {
	parsed, err := version.ParseMajorMinor(v)
	if err != nil {
		t.Fatalf("error parsing binary version: %s", v)
	}

	return parsed
}

type fakeRegistry struct {
	startTime    time.Time
	goVer        string
	binaryVer    *version.Version
	emulationVer *version.Version
}

func (f fakeRegistry) processStartTime() time.Time {
	return f.startTime
}

func (f fakeRegistry) goVersion() string {
	return f.goVer
}

func (f fakeRegistry) binaryVersion() *version.Version {
	return f.binaryVer
}

func (f fakeRegistry) emulationVersion() *version.Version {
	return f.emulationVer
}
