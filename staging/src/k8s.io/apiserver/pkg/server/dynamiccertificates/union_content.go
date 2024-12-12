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

package dynamiccertificates

import (
	"bytes"
	"context"
	"crypto/x509"
	"strings"

	utilerrors "k8s.io/apimachinery/pkg/util/errors"
)

type unionCAContent []CAContentProvider

var _ CAContentProvider = &unionCAContent{}
var _ ControllerRunner = &unionCAContent{}

// NewUnionCAContentProvider returns a CAContentProvider that is a union of other CAContentProviders
func NewUnionCAContentProvider(caContentProviders ...CAContentProvider) CAContentProvider {
	return unionCAContent(caContentProviders)
}

// Name is just an identifier
func (c unionCAContent) Name() string {
	names := []string{}
	for _, curr := range c {
		names = append(names, curr.Name())
	}
	return strings.Join(names, ",")
}

// CurrentCABundleContent provides ca bundle byte content
func (c unionCAContent) CurrentCABundleContent() []byte {
	caBundles := [][]byte{}
	for _, curr := range c {
		if currCABytes := curr.CurrentCABundleContent(); len(currCABytes) > 0 {
			caBundles = append(caBundles, []byte(strings.TrimSpace(string(currCABytes))))
		}
	}

	return bytes.Join(caBundles, []byte("\n"))
}

// Roots provides root certificates for certificate verification.
func (c unionCAContent) Roots() (*x509.CertPool, bool) {
	currCABundle := c.CurrentCABundleContent()
	if len(currCABundle) == 0 {
		return nil, false
	}

	// TODO make more efficient.  This isn't actually used in any of our mainline paths.  It's called to build the TLSConfig
	// TODO on file changes, but the actual authentication runs against the individual items, not the union.
	ret, err := newCABundleAndVerifier(c.Name(), c.CurrentCABundleContent())
	if err != nil {
		// because we're made up of already vetted values, this indicates some kind of coding error
		panic(err)
	}

	return ret.roots, true
}

// AddListener adds a listener to be notified when the CA content changes.
func (c unionCAContent) AddListener(listener Listener) {
	for _, curr := range c {
		curr.AddListener(listener)
	}
}

// AddListener adds a listener to be notified when the CA content changes.
func (c unionCAContent) RunOnce(ctx context.Context) error {
	errors := []error{}
	for _, curr := range c {
		if controller, ok := curr.(ControllerRunner); ok {
			if err := controller.RunOnce(ctx); err != nil {
				errors = append(errors, err)
			}
		}
	}

	return utilerrors.NewAggregate(errors)
}

// Run runs the controller
func (c unionCAContent) Run(ctx context.Context, workers int) {
	for _, curr := range c {
		if controller, ok := curr.(ControllerRunner); ok {
			go controller.Run(ctx, workers)
		}
	}
}
