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

package headerrequest

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"k8s.io/apiserver/pkg/authentication/authenticator"
	x509request "k8s.io/apiserver/pkg/authentication/request/x509"
	"k8s.io/apiserver/pkg/authentication/user"
)

// StringSliceProvider is a way to get a string slice value.  It is heavily used for authentication headers among other places.
type StringSliceProvider interface {
	// Value returns the current string slice.  Callers should never mutate the returned value.
	Value() []string
}

// StringSliceProviderFunc is a function that matches the StringSliceProvider interface
type StringSliceProviderFunc func() []string

// Value returns the current string slice.  Callers should never mutate the returned value.
func (d StringSliceProviderFunc) Value() []string {
	return d()
}

// StaticStringSlice a StringSliceProvider that returns a fixed value
type StaticStringSlice []string

// Value returns the current string slice.  Callers should never mutate the returned value.
func (s StaticStringSlice) Value() []string {
	return s
}

type requestHeaderAuthRequestHandler struct {
	// nameHeaders are the headers to check (in order, case-insensitively) for an identity. The first header with a value wins.
	nameHeaders StringSliceProvider

	// nameHeaders are the headers to check (in order, case-insensitively) for an identity UID. The first header with a value wins.
	uidHeaders StringSliceProvider

	// groupHeaders are the headers to check (case-insensitively) for group membership.  All values of all headers will be added.
	groupHeaders StringSliceProvider

	// extraHeaderPrefixes are the head prefixes to check (case-insensitively) for filling in
	// the user.Info.Extra.  All values of all matching headers will be added.
	extraHeaderPrefixes StringSliceProvider
}

func New(nameHeaders, uidHeaders, groupHeaders, extraHeaderPrefixes []string) (authenticator.Request, error) {
	trimmedNameHeaders, err := trimHeaders(nameHeaders...)
	if err != nil {
		return nil, err
	}
	trimmedUIDHeaders, err := trimHeaders(uidHeaders...)
	if err != nil {
		return nil, err
	}
	trimmedGroupHeaders, err := trimHeaders(groupHeaders...)
	if err != nil {
		return nil, err
	}
	trimmedExtraHeaderPrefixes, err := trimHeaders(extraHeaderPrefixes...)
	if err != nil {
		return nil, err
	}

	return NewDynamic(
		StaticStringSlice(trimmedNameHeaders),
		StaticStringSlice(trimmedUIDHeaders),
		StaticStringSlice(trimmedGroupHeaders),
		StaticStringSlice(trimmedExtraHeaderPrefixes),
	), nil
}

func NewDynamic(nameHeaders, uidHeaders, groupHeaders, extraHeaderPrefixes StringSliceProvider) authenticator.Request {
	return &requestHeaderAuthRequestHandler{
		nameHeaders:         nameHeaders,
		uidHeaders:          uidHeaders,
		groupHeaders:        groupHeaders,
		extraHeaderPrefixes: extraHeaderPrefixes,
	}
}

func trimHeaders(headerNames ...string) ([]string, error) {
	ret := []string{}
	for _, headerName := range headerNames {
		trimmedHeader := strings.TrimSpace(headerName)
		if len(trimmedHeader) == 0 {
			return nil, fmt.Errorf("empty header %q", headerName)
		}
		ret = append(ret, trimmedHeader)
	}

	return ret, nil
}

func NewDynamicVerifyOptionsSecure(verifyOptionFn x509request.VerifyOptionFunc, proxyClientNames, nameHeaders, uidHeaders, groupHeaders, extraHeaderPrefixes StringSliceProvider) authenticator.Request {
	headerAuthenticator := NewDynamic(nameHeaders, uidHeaders, groupHeaders, extraHeaderPrefixes)

	return x509request.NewDynamicCAVerifier(verifyOptionFn, headerAuthenticator, proxyClientNames)
}

func (a *requestHeaderAuthRequestHandler) AuthenticateRequest(req *http.Request) (*authenticator.Response, bool, error) {
	name := headerValue(req.Header, a.nameHeaders.Value())
	if len(name) == 0 {
		return nil, false, nil
	}
	uid := headerValue(req.Header, a.uidHeaders.Value())
	groups := allHeaderValues(req.Header, a.groupHeaders.Value())
	extra := newExtra(req.Header, a.extraHeaderPrefixes.Value())

	// clear headers used for authentication
	ClearAuthenticationHeaders(req.Header, a.nameHeaders, a.uidHeaders, a.groupHeaders, a.extraHeaderPrefixes)

	return &authenticator.Response{
		User: &user.DefaultInfo{
			Name:   name,
			UID:    uid,
			Groups: groups,
			Extra:  extra,
		},
		IsProxiedAuthentication: true,
	}, true, nil
}

func ClearAuthenticationHeaders(h http.Header, nameHeaders, uidHeaders, groupHeaders, extraHeaderPrefixes StringSliceProvider) {
	for _, headerName := range nameHeaders.Value() {
		h.Del(headerName)
	}
	for _, headerName := range uidHeaders.Value() {
		h.Del(headerName)
	}
	for _, headerName := range groupHeaders.Value() {
		h.Del(headerName)
	}
	for _, prefix := range extraHeaderPrefixes.Value() {
		for k := range h {
			if hasPrefixIgnoreCase(k, prefix) {
				delete(h, k) // we have the raw key so avoid relying on canonicalization
			}
		}
	}
}

func hasPrefixIgnoreCase(s, prefix string) bool {
	return len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix)
}

func headerValue(h http.Header, headerNames []string) string {
	for _, headerName := range headerNames {
		headerValue := h.Get(headerName)
		if len(headerValue) > 0 {
			return headerValue
		}
	}
	return ""
}

func allHeaderValues(h http.Header, headerNames []string) []string {
	ret := []string{}
	for _, headerName := range headerNames {
		headerKey := http.CanonicalHeaderKey(headerName)
		values, ok := h[headerKey]
		if !ok {
			continue
		}

		for _, headerValue := range values {
			if len(headerValue) > 0 {
				ret = append(ret, headerValue)
			}
		}
	}
	return ret
}

func unescapeExtraKey(encodedKey string) string {
	key, err := url.PathUnescape(encodedKey) // Decode %-encoded bytes.
	if err != nil {
		return encodedKey // Always record extra strings, even if malformed/unencoded.
	}
	return key
}

func newExtra(h http.Header, headerPrefixes []string) map[string][]string {
	ret := map[string][]string{}

	// we have to iterate over prefixes first in order to have proper ordering inside the value slices
	for _, prefix := range headerPrefixes {
		for headerName, vv := range h {
			if !hasPrefixIgnoreCase(headerName, prefix) {
				continue
			}

			extraKey := unescapeExtraKey(strings.ToLower(headerName[len(prefix):]))
			ret[extraKey] = append(ret[extraKey], vv...)
		}
	}

	return ret
}
