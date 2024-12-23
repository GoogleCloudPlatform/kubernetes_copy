/*
Copyright 2016 The Kubernetes Authors.

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

package server

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apiserver/pkg/authentication/user"
	"k8s.io/apiserver/pkg/authorization/authorizer"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	"k8s.io/kubernetes/pkg/features"
)

func TestIsSubPath(t *testing.T) {
	testcases := map[string]struct {
		subpath  string
		path     string
		expected bool
	}{
		"empty": {subpath: "", path: "", expected: true},

		"match 1": {subpath: "foo", path: "foo", expected: true},
		"match 2": {subpath: "/foo", path: "/foo", expected: true},
		"match 3": {subpath: "/foo/", path: "/foo/", expected: true},
		"match 4": {subpath: "/foo/bar", path: "/foo/bar", expected: true},

		"subpath of root 1": {subpath: "/foo", path: "/", expected: true},
		"subpath of root 2": {subpath: "/foo/", path: "/", expected: true},
		"subpath of root 3": {subpath: "/foo/bar", path: "/", expected: true},

		"subpath of path 1": {subpath: "/foo", path: "/foo", expected: true},
		"subpath of path 2": {subpath: "/foo/", path: "/foo", expected: true},
		"subpath of path 3": {subpath: "/foo/bar", path: "/foo", expected: true},

		"mismatch 1": {subpath: "/foo", path: "/bar", expected: false},
		"mismatch 2": {subpath: "/foo", path: "/foobar", expected: false},
		"mismatch 3": {subpath: "/foobar", path: "/foo", expected: false},
	}

	for k, tc := range testcases {
		result := isSubpath(tc.subpath, tc.path)
		if result != tc.expected {
			t.Errorf("%s: expected %v, got %v", k, tc.expected, result)
		}
	}
}

func TestGetRequestAttributes(t *testing.T) {
	for _, fineGrained := range []bool{false, true} {
		featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.KubeletFineGrainedAuthz, fineGrained)
		for _, test := range AuthzTestCases(fineGrained) {
			t.Run(test.Method+":"+test.Path, func(t *testing.T) {
				getter := NewNodeAuthorizerAttributesGetter(authzTestNodeName)

				req, err := http.NewRequest(test.Method, "https://localhost:1234"+test.Path, nil)
				require.NoError(t, err)
				attrs := getter.GetRequestAttributes(AuthzTestUser(), req)

				test.AssertAttributes(t, attrs)
			})
		}
	}
}

const (
	authzTestNodeName = "test"
	authzTestUserName = "phibby"
)

type AuthzTestCase struct {
	Method, Path string

	ExpectedVerb         string
	ExpectedSubresources []string
}

func (a *AuthzTestCase) AssertAttributes(t *testing.T, attrs []authorizer.Attributes) {
	var expectedAttributes []authorizer.AttributesRecord
	for _, subresource := range a.ExpectedSubresources {
		expectedAttributes = append(expectedAttributes, authorizer.AttributesRecord{
			User:            AuthzTestUser(),
			APIGroup:        "",
			APIVersion:      "v1",
			Verb:            a.ExpectedVerb,
			Resource:        "nodes",
			Name:            authzTestNodeName,
			Subresource:     subresource,
			ResourceRequest: true,
			Path:            a.Path,
		})
	}

	assert.Equal(t, len(attrs), len(expectedAttributes))
	for i := range attrs {
		assert.Equal(t, attrs[i], expectedAttributes[i])
	}
}

func AuthzTestUser() user.Info {
	return &user.DefaultInfo{Name: authzTestUserName}
}

func AuthzTestCases(fineGrained bool) []AuthzTestCase {
	// Path -> ExpectedSubresource
	testPaths := map[string][]string{
		"/attach/{podNamespace}/{podID}/{containerName}":       {"proxy"},
		"/attach/{podNamespace}/{podID}/{uid}/{containerName}": {"proxy"},
		"/checkpoint/{podNamespace}/{podID}/{containerName}":   {"checkpoint"},
		"/configz": {"proxy"},
		"/flagz":   {"proxy"},
		"/statusz": {"statusz"},
		"/containerLogs/{podNamespace}/{podID}/{containerName}": {"proxy"},
		"/debug/flags/v":                                     {"proxy"},
		"/debug/pprof/{subpath:*}":                           {"proxy"},
		"/exec/{podNamespace}/{podID}/{containerName}":       {"proxy"},
		"/exec/{podNamespace}/{podID}/{uid}/{containerName}": {"proxy"},
		"/healthz":                            {"proxy"},
		"/healthz/log":                        {"proxy"},
		"/healthz/ping":                       {"proxy"},
		"/healthz/syncloop":                   {"proxy"},
		"/logs/":                              {"log"},
		"/logs/{logpath:*}":                   {"log"},
		"/metrics":                            {"metrics"},
		"/metrics/slis":                       {"metrics"},
		"/metrics/cadvisor":                   {"metrics"},
		"/metrics/probes":                     {"metrics"},
		"/metrics/resource":                   {"metrics"},
		"/pods/":                              {"proxy"},
		"/portForward/{podNamespace}/{podID}": {"proxy"},
		"/portForward/{podNamespace}/{podID}/{uid}":         {"proxy"},
		"/run/{podNamespace}/{podID}/{containerName}":       {"proxy"},
		"/run/{podNamespace}/{podID}/{uid}/{containerName}": {"proxy"},
		"/runningpods/":  {"proxy"},
		"/stats/":        {"stats"},
		"/stats/summary": {"stats"},
	}

	if fineGrained {
		testPaths["/healthz"] = append([]string{"healthz"}, testPaths["/healthz"]...)
		testPaths["/healthz/log"] = append([]string{"healthz"}, testPaths["/healthz/log"]...)
		testPaths["/healthz/ping"] = append([]string{"healthz"}, testPaths["/healthz/ping"]...)
		testPaths["/healthz/syncloop"] = append([]string{"healthz"}, testPaths["/healthz/syncloop"]...)
		testPaths["/pods/"] = append([]string{"pods"}, testPaths["/pods/"]...)
		testPaths["/runningpods/"] = append([]string{"pods"}, testPaths["/runningpods/"]...)
		testPaths["/configz"] = append([]string{"configz"}, testPaths["/configz"]...)
		testPaths["/flagz"] = append([]string{"configz"}, testPaths["/flagz"]...)
	}

	testCases := []AuthzTestCase{}
	for path, subresource := range testPaths {
		testCases = append(testCases,
			AuthzTestCase{"POST", path, "create", subresource},
			AuthzTestCase{"GET", path, "get", subresource},
			AuthzTestCase{"PUT", path, "update", subresource},
			AuthzTestCase{"PATCH", path, "patch", subresource},
			AuthzTestCase{"DELETE", path, "delete", subresource})
	}
	return testCases
}
