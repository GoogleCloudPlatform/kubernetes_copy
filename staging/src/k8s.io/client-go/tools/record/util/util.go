/*
Copyright 2019 The Kubernetes Authors.

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

package util

import (
	"fmt"
	"net/http"
	"regexp"
	"strings"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	apimachineryvalidation "k8s.io/apimachinery/pkg/api/validation"
	utilvalidation "k8s.io/apimachinery/pkg/util/validation"
)

// ValidateEventType checks that eventtype is an expected type of event
func ValidateEventType(eventtype string) bool {
	switch eventtype {
	case v1.EventTypeNormal, v1.EventTypeWarning:
		return true
	}
	return false
}

// IsKeyNotFoundError is utility function that checks if an error is not found error
func IsKeyNotFoundError(err error) bool {
	statusErr, _ := err.(*errors.StatusError)

	return statusErr != nil && statusErr.Status().Code == http.StatusNotFound
}

var dns1123LabelNonValid = regexp.MustCompile("[^a-z0-9-.]")

// GenerateEventName generates a valid Event name from the referenced name and the passed UNIX timestamp.
// The referenced Object name may not be a valid name for Events and cause the Event to fail
// to be created, so we need to sanitize the referenced name to be a valid Event name.
// Ref: https://issues.k8s.io/127594
func GenerateEventName(refName string, unixNano int64) string {
	name := fmt.Sprintf("%s.%x", refName, unixNano)
	if errs := apimachineryvalidation.NameIsDNSSubdomain(name, false); len(errs) > 0 {
		suffix := fmt.Sprintf(".%x", unixNano) // it always ends in a digit
		// a lowercase RFC 1123 subdomain must consist of lower case alphanumeric characters, '-' or '.'
		prefix := strings.ToLower(refName)
		prefix = dns1123LabelNonValid.ReplaceAllString(prefix, "-")
		// and must start and end with an alphanumeric character
		prefix = strings.Trim(prefix, "-.")
		if len(prefix)+len(suffix) > utilvalidation.DNS1123SubdomainMaxLength {
			prefix = prefix[:utilvalidation.DNS1123SubdomainMaxLength-len(suffix)]
		}
		name = prefix + suffix
	}
	return name
}
