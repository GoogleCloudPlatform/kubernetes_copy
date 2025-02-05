//go:build testify_yaml_fail && !testify_yaml_custom && !testify_yaml_default
// +build testify_yaml_fail,!testify_yaml_custom,!testify_yaml_default

// Package yaml is an implementation of YAML functions that always fail.
//
// This implementation can be used at build time to replace the default implementation
// to avoid linking with [gopkg.in/yaml.v3]:
//
//	go test -tags testify_yaml_fail
package yaml

import "errors"

var errNotImplemented = errors.New("YAML functions are not available (see https://pkg.go.dev/github.com/stretchr/testify/assert/yaml)")

func Unmarshal([]byte, interface{}) error {
	return errNotImplemented
}
