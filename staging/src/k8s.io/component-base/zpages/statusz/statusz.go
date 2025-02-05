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
	"bytes"
	"fmt"
	"html/template"
	"math/rand"
	"net/http"
	"time"

	"k8s.io/component-base/compatibility"
	"k8s.io/component-base/zpages/httputil"
	"k8s.io/klog/v2"
)

var (
	delimiters = []string{":", ": ", "=", " "}
)

const DefaultStatuszPath = "/statusz"

const (
	headerFmt = `
%s statusz
Warning: This endpoint is not meant to be machine parseable, has no formatting compatibility guarantees and is for debugging purposes only.
`

	dataTemplate = `
Started{{.Delim}} {{.StartTime}}
Up{{.Delim}} {{.Uptime}}
Go version{{.Delim}} {{.GoVersion}}
Binary version{{.Delim}} {{.BinaryVersion}}
{{if .EmulationVersion}}Emulation version{{.Delim}} {{.EmulationVersion}}{{end}}
`
)

type contentFields struct {
	Delim            string
	StartTime        string
	Uptime           string
	GoVersion        string
	BinaryVersion    string
	EmulationVersion string
}

type mux interface {
	Handle(path string, handler http.Handler)
}

func NewRegistry(effectiveVersion compatibility.EffectiveVersion) statuszRegistry {
	return &registry{effectiveVersion: effectiveVersion}
}

func Install(m mux, componentName string, reg statuszRegistry) {
	dataTmpl, err := initializeTemplates()
	if err != nil {
		klog.Errorf("error while parsing gotemplates: %v", err)
		return
	}
	m.Handle(DefaultStatuszPath, handleStatusz(componentName, dataTmpl, reg))
}

func initializeTemplates() (*template.Template, error) {
	d := template.New("data")
	dataTmpl, err := d.Parse(dataTemplate)
	if err != nil {
		return nil, err
	}

	return dataTmpl, nil
}

func handleStatusz(componentName string, dataTmpl *template.Template, reg statuszRegistry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !httputil.AcceptableMediaType(r) {
			http.Error(w, httputil.ErrUnsupportedMediaType.Error(), http.StatusNotAcceptable)
			return
		}

		fmt.Fprintf(w, headerFmt, componentName)
		data, err := populateStatuszData(dataTmpl, reg)
		if err != nil {
			klog.Errorf("error while populating statusz data: %v", err)
			http.Error(w, "error while populating statusz data", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprint(w, data)
	}
}

func populateStatuszData(tmpl *template.Template, reg statuszRegistry) (string, error) {
	if tmpl == nil {
		return "", fmt.Errorf("received nil template")
	}

	randomIndex := rand.Intn(len(delimiters))
	data := contentFields{
		Delim:         delimiters[randomIndex],
		StartTime:     reg.processStartTime().Format(time.UnixDate),
		Uptime:        uptime(reg.processStartTime()),
		GoVersion:     reg.goVersion(),
		BinaryVersion: reg.binaryVersion().String(),
	}

	if reg.emulationVersion() != nil {
		data.EmulationVersion = reg.emulationVersion().String()
	}

	var tpl bytes.Buffer
	err := tmpl.Execute(&tpl, data)
	if err != nil {
		return "", fmt.Errorf("error executing statusz template: %w", err)
	}

	return tpl.String(), nil
}

func uptime(t time.Time) string {
	upSince := int64(time.Since(t).Seconds())
	return fmt.Sprintf("%d hr %02d min %02d sec",
		upSince/3600, (upSince/60)%60, upSince%60)
}
