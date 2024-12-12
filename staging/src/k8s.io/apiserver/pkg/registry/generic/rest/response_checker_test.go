/*
Copyright 2014 The Kubernetes Authors.

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

package rest

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
)

func TestGenericHttpResponseChecker(t *testing.T) {
	responseChecker := NewGenericHttpResponseChecker(v1.Resource("pods"), "foo")
	tests := []struct {
		resp        *http.Response
		expectError bool
		expected    error
		name        string
	}{
		{
			resp: &http.Response{
				Body:       io.NopCloser(bytes.NewBufferString("Success")),
				StatusCode: http.StatusOK,
			},
			expectError: false,
			name:        "ok",
		},
		{
			resp: &http.Response{
				Body:       io.NopCloser(bytes.NewBufferString("Invalid request.")),
				StatusCode: http.StatusBadRequest,
			},
			expectError: true,
			expected:    errors.NewBadRequest("Invalid request."),
			name:        "bad request",
		},
		{
			resp: &http.Response{
				Body:       io.NopCloser(bytes.NewBufferString("Pod does not exist.")),
				StatusCode: http.StatusInternalServerError,
			},
			expectError: true,
			expected:    errors.NewInternalError(fmt.Errorf("%s", "Pod does not exist.")),
			name:        "internal server error",
		},
	}
	for _, test := range tests {
		err := responseChecker.Check(test.resp)
		if test.expectError && err == nil {
			t.Error("unexpected non-error")
		}
		if !test.expectError && err != nil {
			t.Errorf("unexpected error: %v", err)
		}
		if test.expectError && !reflect.DeepEqual(err, test.expected) {
			t.Errorf("expected: %s, saw: %s", test.expected, err)
		}
	}
}

func TestGenericHttpResponseCheckerLimitReader(t *testing.T) {
	responseChecker := NewGenericHttpResponseChecker(v1.Resource("pods"), "foo")
	excessedString := strings.Repeat("a", (maxReadLength + 10000))
	resp := &http.Response{
		Body:       io.NopCloser(bytes.NewBufferString(excessedString)),
		StatusCode: http.StatusBadRequest,
	}
	err := responseChecker.Check(resp)
	if err == nil {
		t.Error("unexpected non-error")
	}
	if len(err.Error()) != maxReadLength {
		t.Errorf("expected length of error message: %d, saw: %d", maxReadLength, len(err.Error()))
	}
}
