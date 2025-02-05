/*
Copyright 2017 The Kubernetes Authors.

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

package flag

import (
	"crypto/tls"
	"fmt"

	"k8s.io/apimachinery/pkg/util/sets"
)

var (
	// ciphers maps strings into tls package cipher constants in
	// https://golang.org/pkg/crypto/tls/#pkg-constants
	ciphers         = map[string]uint16{}
	insecureCiphers = map[string]uint16{}
)

func init() {
	for _, suite := range tls.CipherSuites() {
		ciphers[suite.Name] = suite.ID
	}
	// keep legacy names for backward compatibility
	ciphers["TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305"] = tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256
	ciphers["TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305"] = tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256

	for _, suite := range tls.InsecureCipherSuites() {
		insecureCiphers[suite.Name] = suite.ID
	}
}

// InsecureTLSCiphers returns the cipher suites implemented by crypto/tls which have
// security issues.
func InsecureTLSCiphers() map[string]uint16 {
	cipherKeys := make(map[string]uint16, len(insecureCiphers))
	for k, v := range insecureCiphers {
		cipherKeys[k] = v
	}
	return cipherKeys
}

// InsecureTLSCipherNames returns a list of cipher suite names implemented by crypto/tls
// which have security issues.
func InsecureTLSCipherNames() []string {
	cipherKeys := sets.NewString()
	for key := range insecureCiphers {
		cipherKeys.Insert(key)
	}
	return cipherKeys.List()
}

// PreferredTLSCipherNames returns a list of cipher suite names implemented by crypto/tls.
func PreferredTLSCipherNames() []string {
	cipherKeys := sets.NewString()
	for key := range ciphers {
		cipherKeys.Insert(key)
	}
	return cipherKeys.List()
}

func allCiphers() map[string]uint16 {
	acceptedCiphers := make(map[string]uint16, len(ciphers)+len(insecureCiphers))
	for k, v := range ciphers {
		acceptedCiphers[k] = v
	}
	for k, v := range insecureCiphers {
		acceptedCiphers[k] = v
	}
	return acceptedCiphers
}

// TLSCipherPossibleValues returns all acceptable cipher suite names.
// This is a combination of both InsecureTLSCipherNames() and PreferredTLSCipherNames().
func TLSCipherPossibleValues() []string {
	cipherKeys := sets.NewString()
	acceptedCiphers := allCiphers()
	for key := range acceptedCiphers {
		cipherKeys.Insert(key)
	}
	return cipherKeys.List()
}

// TLSCipherSuites returns a list of cipher suite IDs from the cipher suite names passed.
func TLSCipherSuites(cipherNames []string) ([]uint16, error) {
	if len(cipherNames) == 0 {
		return nil, nil
	}
	ciphersIntSlice := make([]uint16, 0)
	possibleCiphers := allCiphers()
	for _, cipher := range cipherNames {
		intValue, ok := possibleCiphers[cipher]
		if !ok {
			return nil, fmt.Errorf("cipher suite %q not supported or doesn't exist", cipher)
		}
		ciphersIntSlice = append(ciphersIntSlice, intValue)
	}
	return ciphersIntSlice, nil
}

var versions = map[string]uint16{
	"VersionTLS10": tls.VersionTLS10,
	"VersionTLS11": tls.VersionTLS11,
	"VersionTLS12": tls.VersionTLS12,
	"VersionTLS13": tls.VersionTLS13,
}

// TLSPossibleVersions returns all acceptable values for TLS Version.
func TLSPossibleVersions() []string {
	versionsKeys := sets.NewString()
	for key := range versions {
		versionsKeys.Insert(key)
	}
	return versionsKeys.List()
}

// TLSVersion returns the TLS Version ID for the version name passed.
func TLSVersion(versionName string) (uint16, error) {
	if len(versionName) == 0 {
		return DefaultTLSVersion(), nil
	}
	if version, ok := versions[versionName]; ok {
		return version, nil
	}
	return 0, fmt.Errorf("unknown tls version %q", versionName)
}

// DefaultTLSVersion defines the default TLS Version.
func DefaultTLSVersion() uint16 {
	// Can't use SSLv3 because of POODLE and BEAST
	// Can't use TLSv1.0 because of POODLE and BEAST using CBC cipher
	// Can't use TLSv1.1 because of RC4 cipher usage
	return tls.VersionTLS12
}
