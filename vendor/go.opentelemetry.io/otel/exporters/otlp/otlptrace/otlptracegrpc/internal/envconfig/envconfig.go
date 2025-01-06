// Code created by gotmpl. DO NOT MODIFY.
// source: internal/shared/otlp/envconfig/envconfig.go.tmpl

// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package envconfig // import "go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc/internal/envconfig"

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"

	"go.opentelemetry.io/otel/internal/global"
)

// ConfigFn is the generic function used to set a config.
type ConfigFn func(*EnvOptionsReader)

// EnvOptionsReader reads the required environment variables.
type EnvOptionsReader struct {
	GetEnv    func(string) string
	ReadFile  func(string) ([]byte, error)
	Namespace string
}

// Apply runs every ConfigFn.
func (e *EnvOptionsReader) Apply(opts ...ConfigFn) {
	for _, o := range opts {
		o(e)
	}
}

// GetEnvValue gets an OTLP environment variable value of the specified key
// using the GetEnv function.
// This function prepends the OTLP specified namespace to all key lookups.
func (e *EnvOptionsReader) GetEnvValue(key string) (string, bool) {
	v := strings.TrimSpace(e.GetEnv(keyWithNamespace(e.Namespace, key)))
	return v, v != ""
}

// WithString retrieves the specified config and passes it to ConfigFn as a string.
func WithString(n string, fn func(string)) func(e *EnvOptionsReader) {
	return func(e *EnvOptionsReader) {
		if v, ok := e.GetEnvValue(n); ok {
			fn(v)
		}
	}
}

// WithBool returns a ConfigFn that reads the environment variable n and if it exists passes its parsed bool value to fn.
func WithBool(n string, fn func(bool)) ConfigFn {
	return func(e *EnvOptionsReader) {
		if v, ok := e.GetEnvValue(n); ok {
			b := strings.ToLower(v) == "true"
			fn(b)
		}
	}
}

// WithDuration retrieves the specified config and passes it to ConfigFn as a duration.
func WithDuration(n string, fn func(time.Duration)) func(e *EnvOptionsReader) {
	return func(e *EnvOptionsReader) {
		if v, ok := e.GetEnvValue(n); ok {
			d, err := strconv.Atoi(v)
			if err != nil {
				global.Error(err, "parse duration", "input", v)
				return
			}
			fn(time.Duration(d) * time.Millisecond)
		}
	}
}

// WithHeaders retrieves the specified config and passes it to ConfigFn as a map of HTTP headers.
func WithHeaders(n string, fn func(map[string]string)) func(e *EnvOptionsReader) {
	return func(e *EnvOptionsReader) {
		if v, ok := e.GetEnvValue(n); ok {
			fn(stringToHeader(v))
		}
	}
}

// WithURL retrieves the specified config and passes it to ConfigFn as a net/url.URL.
func WithURL(n string, fn func(*url.URL)) func(e *EnvOptionsReader) {
	return func(e *EnvOptionsReader) {
		if v, ok := e.GetEnvValue(n); ok {
			u, err := url.Parse(v)
			if err != nil {
				global.Error(err, "parse url", "input", v)
				return
			}
			fn(u)
		}
	}
}

// WithCertPool returns a ConfigFn that reads the environment variable n as a filepath to a TLS certificate pool. If it exists, it is parsed as a crypto/x509.CertPool and it is passed to fn.
func WithCertPool(n string, fn func(*x509.CertPool)) ConfigFn {
	return func(e *EnvOptionsReader) {
		if v, ok := e.GetEnvValue(n); ok {
			b, err := e.ReadFile(v)
			if err != nil {
				global.Error(err, "read tls ca cert file", "file", v)
				return
			}
			c, err := createCertPool(b)
			if err != nil {
				global.Error(err, "create tls cert pool")
				return
			}
			fn(c)
		}
	}
}

// WithClientCert returns a ConfigFn that reads the environment variable nc and nk as filepaths to a client certificate and key pair. If they exists, they are parsed as a crypto/tls.Certificate and it is passed to fn.
func WithClientCert(nc, nk string, fn func(tls.Certificate)) ConfigFn {
	return func(e *EnvOptionsReader) {
		vc, okc := e.GetEnvValue(nc)
		vk, okk := e.GetEnvValue(nk)
		if !okc || !okk {
			return
		}
		cert, err := e.ReadFile(vc)
		if err != nil {
			global.Error(err, "read tls client cert", "file", vc)
			return
		}
		key, err := e.ReadFile(vk)
		if err != nil {
			global.Error(err, "read tls client key", "file", vk)
			return
		}
		crt, err := tls.X509KeyPair(cert, key)
		if err != nil {
			global.Error(err, "create tls client key pair")
			return
		}
		fn(crt)
	}
}

func keyWithNamespace(ns, key string) string {
	if ns == "" {
		return key
	}
	return fmt.Sprintf("%s_%s", ns, key)
}

func stringToHeader(value string) map[string]string {
	headersPairs := strings.Split(value, ",")
	headers := make(map[string]string)

	for _, header := range headersPairs {
		n, v, found := strings.Cut(header, "=")
		if !found {
			global.Error(errors.New("missing '="), "parse headers", "input", header)
			continue
		}

		trimmedName := strings.TrimSpace(n)

		// Validate the key.
		if !isValidHeaderKey(trimmedName) {
			global.Error(errors.New("invalid header key"), "parse headers", "key", trimmedName)
			continue
		}

		// Only decode the value.
		value, err := url.PathUnescape(v)
		if err != nil {
			global.Error(err, "escape header value", "value", v)
			continue
		}
		trimmedValue := strings.TrimSpace(value)

		headers[trimmedName] = trimmedValue
	}

	return headers
}

func createCertPool(certBytes []byte) (*x509.CertPool, error) {
	cp := x509.NewCertPool()
	if ok := cp.AppendCertsFromPEM(certBytes); !ok {
		return nil, errors.New("failed to append certificate to the cert pool")
	}
	return cp, nil
}

func isValidHeaderKey(key string) bool {
	if key == "" {
		return false
	}
	for _, c := range key {
		if !isTokenChar(c) {
			return false
		}
	}
	return true
}

func isTokenChar(c rune) bool {
	return c <= unicode.MaxASCII && (unicode.IsLetter(c) ||
		unicode.IsDigit(c) ||
		c == '!' || c == '#' || c == '$' || c == '%' || c == '&' || c == '\'' || c == '*' ||
		c == '+' || c == '-' || c == '.' || c == '^' || c == '_' || c == '`' || c == '|' || c == '~')
}
