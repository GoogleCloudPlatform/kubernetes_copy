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

package authenticatorfactory

import (
	"k8s.io/apiserver/pkg/authentication/request/headerrequest"
	"k8s.io/apiserver/pkg/server/dynamiccertificates"
)

type RequestHeaderConfig struct {
	// UsernameHeaders are the headers to check (in order, case-insensitively) for an identity. The first header with a value wins.
	UsernameHeaders headerrequest.StringSliceProvider
	// UsernameHeaders are the headers to check (in order, case-insensitively) for an identity UID. The first header with a value wins.
	UIDHeaders headerrequest.StringSliceProvider
	// GroupHeaders are the headers to check (case-insensitively) for a group names.  All values will be used.
	GroupHeaders headerrequest.StringSliceProvider
	// ExtraHeaderPrefixes are the head prefixes to check (case-insensitively) for filling in
	// the user.Info.Extra.  All values of all matching headers will be added.
	ExtraHeaderPrefixes headerrequest.StringSliceProvider
	// CAContentProvider the options for verifying incoming connections using mTLS.  Generally this points to CA bundle file which is used verify the identity of the front proxy.
	//	It may produce different options at will.
	CAContentProvider dynamiccertificates.CAContentProvider
	// AllowedClientNames is a list of common names that may be presented by the authenticating front proxy.  Empty means: accept any.
	AllowedClientNames headerrequest.StringSliceProvider
}
