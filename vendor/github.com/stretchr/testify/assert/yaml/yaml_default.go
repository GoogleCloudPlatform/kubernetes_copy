//go:build !testify_yaml_fail && !testify_yaml_custom
// +build !testify_yaml_fail,!testify_yaml_custom

// Package yaml is just an indirection to handle YAML deserialization.
//
// This package is just an indirection that allows the builder to override the
// indirection with an alternative implementation of this package that uses
// another implementation of YAML deserialization. This allows to not either not
// use YAML deserialization at all, or to use another implementation than
// [gopkg.in/yaml.v3] (for example for license compatibility reasons, see [PR #1120]).
//
// Alternative implementations are selected using build tags:
//
//   - testify_yaml_fail: [Unmarshal] always fails with an error
//   - testify_yaml_custom: [Unmarshal] is a variable. Caller must initialize it
//     before calling any of [github.com/stretchr/testify/assert.YAMLEq] or
//     [github.com/stretchr/testify/assert.YAMLEqf].
//
// Usage:
//
//	go test -tags testify_yaml_fail
//
// You can check with "go list" which implementation is linked:
//
//	go list -f '{{.Imports}}' github.com/stretchr/testify/assert/yaml
//	go list -tags testify_yaml_fail -f '{{.Imports}}' github.com/stretchr/testify/assert/yaml
//	go list -tags testify_yaml_custom -f '{{.Imports}}' github.com/stretchr/testify/assert/yaml
//
// [PR #1120]: https://github.com/stretchr/testify/pull/1120
package yaml

import goyaml "gopkg.in/yaml.v3"

// Unmarshal is just a wrapper of [gopkg.in/yaml.v3.Unmarshal].
func Unmarshal(in []byte, out interface{}) error {
	return goyaml.Unmarshal(in, out)
}
