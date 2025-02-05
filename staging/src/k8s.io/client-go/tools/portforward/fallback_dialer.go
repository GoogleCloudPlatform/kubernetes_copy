/*
Copyright 2024 The Kubernetes Authors.

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

package portforward

import (
	"k8s.io/apimachinery/pkg/util/httpstream"
	"k8s.io/klog/v2"
)

var _ httpstream.Dialer = &FallbackDialer{}

// FallbackDialer encapsulates a primary and secondary dialer, including
// the boolean function to determine if the primary dialer failed. Implements
// the httpstream.Dialer interface.
type FallbackDialer struct {
	primary        httpstream.Dialer
	secondary      httpstream.Dialer
	shouldFallback func(error) bool
}

// NewFallbackDialer creates the FallbackDialer with the primary and secondary dialers,
// as well as the boolean function to determine if the primary dialer failed.
func NewFallbackDialer(primary, secondary httpstream.Dialer, shouldFallback func(error) bool) httpstream.Dialer {
	return &FallbackDialer{
		primary:        primary,
		secondary:      secondary,
		shouldFallback: shouldFallback,
	}
}

// Dial is the single function necessary to implement the "httpstream.Dialer" interface.
// It takes the protocol version strings to request, returning an the upgraded
// httstream.Connection and the negotiated protocol version accepted. If the initial
// primary dialer fails, this function attempts the secondary dialer. Returns an error
// if one occurs.
func (f *FallbackDialer) Dial(protocols ...string) (httpstream.Connection, string, error) {
	conn, version, err := f.primary.Dial(protocols...)
	if err != nil && f.shouldFallback(err) {
		//nolint:logcheck // This code is only used by kubectl where contextual logging is not that useful.
		klog.V(4).Infof("fallback to secondary dialer from primary dialer err: %v", err)
		return f.secondary.Dial(protocols...)
	}
	return conn, version, err
}
