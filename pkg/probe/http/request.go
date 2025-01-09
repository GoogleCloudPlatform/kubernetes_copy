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
	"fmt"
	"k8s.io/kubernetes/third_party/forked/golang/expansion"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	v1 "k8s.io/api/core/v1"
	"k8s.io/component-base/version"
	"k8s.io/kubernetes/pkg/probe"
)

// NewProbeRequest returns an http.Request suitable for use as a request for a
// probe.
func NewProbeRequest(url *url.URL, headers http.Header) (*http.Request, error) {
	return newProbeRequest(url, headers, "probe")
}

// NewRequestForHTTPGetAction returns an http.Request derived from httpGet.
// When httpGet.Host is empty, podIP will be used instead.
func NewRequestForHTTPGetAction(httpGet *v1.HTTPGetAction, container *v1.Container, podIP string, userAgentFragment string, envs map[string]string) (*http.Request, error) {
	scheme := strings.ToLower(string(httpGet.Scheme))
	if scheme == "" {
		scheme = "http"
	}

	host := httpGet.Host
	if host == "" {
		host = podIP
	}

	port, err := probe.ResolveContainerPort(httpGet.Port, container)
	if err != nil {
		return nil, err
	}

	path := expandPath(httpGet.Path, envs)
	url := formatURL(scheme, host, port, path)
	headers := v1HeaderToHTTPHeader(httpGet.HTTPHeaders)

	return newProbeRequest(url, headers, userAgentFragment)
}

func newProbeRequest(url *url.URL, headers http.Header, userAgentFragment string) (*http.Request, error) {
	req, err := http.NewRequest("GET", url.String(), nil)
	if err != nil {
		return nil, err
	}

	if headers == nil {
		headers = http.Header{}
	}
	if _, ok := headers["User-Agent"]; !ok {
		// User-Agent header was not defined, set it
		headers.Set("User-Agent", userAgent(userAgentFragment))
	}
	if _, ok := headers["Accept"]; !ok {
		// Accept header was not defined. accept all
		headers.Set("Accept", "*/*")
	} else if headers.Get("Accept") == "" {
		// Accept header was overridden but is empty. removing
		headers.Del("Accept")
	}
	req.Header = headers
	req.Host = headers.Get("Host")

	return req, nil
}

func userAgent(purpose string) string {
	v := version.Get()
	return fmt.Sprintf("kube-%s/%s.%s", purpose, v.Major, v.Minor)
}

// formatURL formats a URL from args.  For testability.
func formatURL(scheme string, host string, port int, path string) *url.URL {
	u, err := url.Parse(path)
	// Something is busted with the path, but it's too late to reject it. Pass it along as is.
	//
	// This construction of a URL may be wrong in some cases, but it preserves
	// legacy prober behavior.
	if err != nil {
		u = &url.URL{
			Path: path,
		}
	}
	u.Scheme = scheme
	u.Host = net.JoinHostPort(host, strconv.Itoa(port))
	return u
}

// v1HeaderToHTTPHeader takes a list of HTTPHeader <name, value> string pairs
// and returns a populated string->[]string http.Header map.
func v1HeaderToHTTPHeader(headerList []v1.HTTPHeader) http.Header {
	headers := make(http.Header)
	for _, header := range headerList {
		headers.Add(header.Name, header.Value)
	}
	return headers
}

// expandPath expands path using environment variables
func expandPath(path string, envVars map[string]string) string {
	mappingFunc := expansion.MappingFuncFor(envVars)
	return expansion.Expand(path, mappingFunc)
}
