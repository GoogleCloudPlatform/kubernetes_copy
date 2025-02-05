//go:build testify_yaml_custom && !testify_yaml_fail && !testify_yaml_default
// +build testify_yaml_custom,!testify_yaml_fail,!testify_yaml_default

// Package yaml is an implementation of YAML functions that calls a pluggable implementation.
//
// This implementation is selected with the testify_yaml_custom build tag.
//
//	go test -tags testify_yaml_custom
//
// This implementation can be used at build time to replace the default implementation
// to avoid linking with [gopkg.in/yaml.v3].
//
// In your test package:
//
//		import assertYaml "github.com/stretchr/testify/assert/yaml"
//
//		func init() {
//			assertYaml.Unmarshal = func (in []byte, out interface{}) error {
//				// ...
//	     			return nil
//			}
//		}
package yaml

var Unmarshal func(in []byte, out interface{}) error
