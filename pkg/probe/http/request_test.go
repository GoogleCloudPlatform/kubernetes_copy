/*
Copyright 2022 The Kubernetes Authors.

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

package http

import (
	"net/http"
	"reflect"
	"testing"

	v1 "k8s.io/api/core/v1"
)

func TestFormatURL(t *testing.T) {
	testCases := []struct {
		scheme string
		host   string
		port   int
		path   string
		result string
	}{
		{"http", "localhost", 93, "", "http://localhost:93"},
		{"https", "localhost", 93, "/path", "https://localhost:93/path"},
		{"http", "localhost", 93, "?foo", "http://localhost:93?foo"},
		{"https", "localhost", 93, "/path?bar", "https://localhost:93/path?bar"},
	}
	for _, test := range testCases {
		url := formatURL(test.scheme, test.host, test.port, test.path)
		if url.String() != test.result {
			t.Errorf("Expected %s, got %s", test.result, url.String())
		}
	}
}

func Test_v1HeaderToHTTPHeader(t *testing.T) {
	tests := []struct {
		name       string
		headerList []v1.HTTPHeader
		want       http.Header
	}{
		{
			name: "not empty input",
			headerList: []v1.HTTPHeader{
				{Name: "Connection", Value: "Keep-Alive"},
				{Name: "Content-Type", Value: "text/html"},
				{Name: "Accept-Ranges", Value: "bytes"},
			},
			want: http.Header{
				"Connection":    {"Keep-Alive"},
				"Content-Type":  {"text/html"},
				"Accept-Ranges": {"bytes"},
			},
		},
		{
			name: "case insensitive",
			headerList: []v1.HTTPHeader{
				{Name: "HOST", Value: "example.com"},
				{Name: "FOO-bAR", Value: "value"},
			},
			want: http.Header{
				"Host":    {"example.com"},
				"Foo-Bar": {"value"},
			},
		},
		{
			name:       "empty input",
			headerList: []v1.HTTPHeader{},
			want:       http.Header{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := v1HeaderToHTTPHeader(tt.headerList); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("v1HeaderToHTTPHeader() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestHeaderConversion(t *testing.T) {
	testCases := []struct {
		headers  []v1.HTTPHeader
		expected http.Header
	}{
		{
			[]v1.HTTPHeader{
				{
					Name:  "Accept",
					Value: "application/json",
				},
			},
			http.Header{
				"Accept": []string{"application/json"},
			},
		},
		{
			[]v1.HTTPHeader{
				{Name: "accept", Value: "application/json"},
			},
			http.Header{
				"Accept": []string{"application/json"},
			},
		},
		{
			[]v1.HTTPHeader{
				{Name: "accept", Value: "application/json"},
				{Name: "Accept", Value: "*/*"},
				{Name: "pragma", Value: "no-cache"},
				{Name: "X-forwarded-for", Value: "username"},
			},
			http.Header{
				"Accept":          []string{"application/json", "*/*"},
				"Pragma":          []string{"no-cache"},
				"X-Forwarded-For": []string{"username"},
			},
		},
	}
	for _, test := range testCases {
		headers := v1HeaderToHTTPHeader(test.headers)
		if !reflect.DeepEqual(headers, test.expected) {
			t.Errorf("Expected %v, got %v", test.expected, headers)
		}
	}
}

func TestExpandPath(t *testing.T) {
	envVars := map[string]string{
		"VAR_A": "var-a",
		"VAR_B": "var-b",
		"VAR_C": "var-c",
		"VAR_D": "var-d",
	}
	testCases := []struct {
		path         string
		expectedPath string
	}{
		{"", ""},
		{"/path/to", "/path/to"},
		{"/bad/path/$(VAR_A", "/bad/path/$(VAR_A"},
		{"/path/to/$(VAR_A)", "/path/to/var-a"},
		{"$(VAR_A)/path/to/$(VAR_B)", "var-a/path/to/var-b"},
		{"$(VAR_A)/path/to/$(VAR_B)?c=$(VAR_C)", "var-a/path/to/var-b?c=var-c"},
		{"$(VAR_A)/path/to/$(VAR_B)?c=$(VAR_C)&d=$(VAR_D)", "var-a/path/to/var-b?c=var-c&d=var-d"},
	}
	for _, test := range testCases {
		expandedPath := expandPath(test.path, envVars)
		if expandedPath != test.expectedPath {
			t.Errorf("Expected %s, got %s", test.expectedPath, expandedPath)
		}
	}
}
