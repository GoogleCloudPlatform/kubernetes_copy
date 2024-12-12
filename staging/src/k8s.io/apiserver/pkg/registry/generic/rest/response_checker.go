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
	"fmt"
	"io"
	"net/http"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Check the http error status from a location URL.
// And convert an error into a structured API object.
// Finally ensure we close the body before returning the error
type HttpResponseChecker interface {
	Check(resp *http.Response) error
}

// Max length read from the response body of a location which returns error status
const (
	maxReadLength = 50000
)

// A generic http response checker to transform the error.
type GenericHttpResponseChecker struct {
	QualifiedResource schema.GroupResource
	Name              string
}

func (checker GenericHttpResponseChecker) Check(resp *http.Response) error {
	if resp.StatusCode < http.StatusOK || resp.StatusCode > http.StatusPartialContent {
		defer resp.Body.Close()
		bodyBytes, err := io.ReadAll(io.LimitReader(resp.Body, maxReadLength))
		if err != nil {
			return errors.NewInternalError(err)
		}
		bodyText := string(bodyBytes)

		switch {
		case resp.StatusCode == http.StatusInternalServerError:
			return errors.NewInternalError(fmt.Errorf("%s", bodyText))
		case resp.StatusCode == http.StatusBadRequest:
			return errors.NewBadRequest(bodyText)
		case resp.StatusCode == http.StatusNotFound:
			return errors.NewGenericServerResponse(resp.StatusCode, "", checker.QualifiedResource, checker.Name, bodyText, 0, false)
		}
		return errors.NewGenericServerResponse(resp.StatusCode, "", checker.QualifiedResource, checker.Name, bodyText, 0, false)
	}
	return nil
}

func NewGenericHttpResponseChecker(qualifiedResource schema.GroupResource, name string) GenericHttpResponseChecker {
	return GenericHttpResponseChecker{QualifiedResource: qualifiedResource, Name: name}
}
