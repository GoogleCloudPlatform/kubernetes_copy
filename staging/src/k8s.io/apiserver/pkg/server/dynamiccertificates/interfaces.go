/*
Copyright 2021 The Kubernetes Authors.

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

package dynamiccertificates

import (
	"crypto/x509"
)

// Listener is an interface to use to notify interested parties of a change.
type Listener interface {
	// Enqueue should be called when an input may have changed
	Enqueue()
}

// Notifier is a way to add listeners
type Notifier interface {
	// AddListener is adds a listener to be notified of potential input changes.
	// This is a noop on static providers.
	AddListener(listener Listener)
}

// CAContentProvider provides ca bundle byte content
type CAContentProvider interface {
	Notifier

	// Name is just an identifier.
	Name() string
	// CurrentCABundleContent provides ca bundle byte content. Errors can be
	// contained to the controllers initializing the value. By the time you get
	// here, you should always be returning a value that won't fail.
	CurrentCABundleContent() []byte
	// Roots provides root certificates for certificate verification
	Roots() (*x509.CertPool, bool)
}

// CertKeyContentProvider provides a certificate and matching private key.
type CertKeyContentProvider interface {
	Notifier

	// Name is just an identifier.
	Name() string
	// CurrentCertKeyContent provides cert and key byte content.
	CurrentCertKeyContent() ([]byte, []byte)
}

// SNICertKeyContentProvider provides a certificate and matching private key as
// well as optional explicit names.
type SNICertKeyContentProvider interface {
	Notifier

	CertKeyContentProvider
	// SNINames provides names used for SNI. May return nil.
	SNINames() []string
}
