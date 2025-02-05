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

package egressselector

import (
	"fmt"
	"os"
	"strings"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/apiserver/pkg/apis/apiserver"
	"k8s.io/apiserver/pkg/apis/apiserver/install"
	"k8s.io/utils/path"
)

var cfgScheme = runtime.NewScheme()

// validEgressSelectorNames contains the set of valid egress selector names.
var validEgressSelectorNames = sets.NewString("controlplane", "cluster", "etcd")

func init() {
	install.Install(cfgScheme)
}

// ReadEgressSelectorConfiguration reads the egress selector configuration at the specified path.
// It returns the loaded egress selector configuration if the input file aligns with the required syntax.
// If it does not align with the provided syntax, it returns a default configuration which should function as a no-op.
// It does this by returning a nil configuration, which preserves backward compatibility.
// This works because prior to this there was no egress selector configuration.
// It returns an error if the file did not exist.
func ReadEgressSelectorConfiguration(configFilePath string) (*apiserver.EgressSelectorConfiguration, error) {
	if configFilePath == "" {
		return nil, nil
	}
	// a file was provided, so we just read it.
	data, err := os.ReadFile(configFilePath)
	if err != nil {
		return nil, fmt.Errorf("unable to read egress selector configuration from %q [%v]", configFilePath, err)
	}
	config, gvk, err := serializer.NewCodecFactory(cfgScheme, serializer.EnableStrict).UniversalDecoder().Decode(data, nil, nil)
	if err != nil {
		return nil, err
	}
	internalConfig, ok := config.(*apiserver.EgressSelectorConfiguration)
	if !ok {
		return nil, fmt.Errorf("unexpected config type: %v", gvk)
	}
	return internalConfig, nil
}

// ValidateEgressSelectorConfiguration checks the apiserver.EgressSelectorConfiguration for
// common configuration errors. It will return error for problems such as configuring mtls/cert
// settings for protocol which do not support security. It will also try to catch errors such as
// incorrect file paths. It will return nil if it does not find anything wrong.
func ValidateEgressSelectorConfiguration(config *apiserver.EgressSelectorConfiguration) field.ErrorList {
	allErrs := field.ErrorList{}
	if config == nil {
		return allErrs // Treating a nil configuration as valid
	}
	for _, service := range config.EgressSelections {
		fldPath := field.NewPath("service", "connection")
		switch service.Connection.ProxyProtocol {
		case apiserver.ProtocolDirect:
			allErrs = append(allErrs, validateDirectConnection(service.Connection, fldPath)...)
		case apiserver.ProtocolHTTPConnect:
			allErrs = append(allErrs, validateHTTPConnectTransport(service.Connection.Transport, fldPath)...)
		case apiserver.ProtocolGRPC:
			allErrs = append(allErrs, validateGRPCTransport(service.Connection.Transport, fldPath)...)
		default:
			allErrs = append(allErrs, field.NotSupported(
				fldPath.Child("protocol"),
				service.Connection.ProxyProtocol,
				[]string{
					string(apiserver.ProtocolDirect),
					string(apiserver.ProtocolHTTPConnect),
					string(apiserver.ProtocolGRPC),
				}))
		}
	}

	seen := sets.String{}
	for i, service := range config.EgressSelections {
		canonicalName := strings.ToLower(service.Name)
		fldPath := field.NewPath("service", "connection")
		// no duplicate check
		if seen.Has(canonicalName) {
			allErrs = append(allErrs, field.Duplicate(fldPath.Index(i), canonicalName))
			continue
		}
		seen.Insert(canonicalName)

		if !validEgressSelectorNames.Has(canonicalName) {
			allErrs = append(allErrs, field.NotSupported(fldPath, canonicalName, validEgressSelectorNames.List()))
			continue
		}
	}

	return allErrs
}

func validateHTTPConnectTransport(transport *apiserver.Transport, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	if transport == nil {
		allErrs = append(allErrs, field.Required(
			fldPath.Child("transport"),
			"transport must be set for HTTPConnect"))
		return allErrs
	}

	if transport.TCP != nil && transport.UDS != nil {
		allErrs = append(allErrs, field.Invalid(
			fldPath.Child("tcp"),
			transport.TCP,
			"TCP and UDS cannot both be set"))
	} else if transport.TCP == nil && transport.UDS == nil {
		allErrs = append(allErrs, field.Required(
			fldPath.Child("tcp"),
			"One of TCP or UDS must be set"))
	} else if transport.TCP != nil {
		allErrs = append(allErrs, validateTCPConnection(transport.TCP, fldPath)...)
	} else if transport.UDS != nil {
		allErrs = append(allErrs, validateUDSConnection(transport.UDS, fldPath)...)
	}
	return allErrs
}

func validateGRPCTransport(transport *apiserver.Transport, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	if transport == nil {
		allErrs = append(allErrs, field.Required(
			fldPath.Child("transport"),
			"transport must be set for GRPC"))
		return allErrs
	}

	if transport.UDS != nil {
		allErrs = append(allErrs, validateUDSConnection(transport.UDS, fldPath)...)
	} else {
		allErrs = append(allErrs, field.Required(
			fldPath.Child("uds"),
			"UDS must be set with GRPC"))
	}
	return allErrs
}

func validateDirectConnection(connection apiserver.Connection, fldPath *field.Path) field.ErrorList {
	if connection.Transport != nil {
		return field.ErrorList{field.Invalid(
			fldPath.Child("transport"),
			"direct",
			"Transport config should be absent for direct connect"),
		}
	}

	return nil
}

func validateUDSConnection(udsConfig *apiserver.UDSTransport, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	if udsConfig.UDSName == "" {
		allErrs = append(allErrs, field.Invalid(
			fldPath.Child("udsName"),
			"nil",
			"UDSName should be present for UDS connections"))
	}
	return allErrs
}

func validateTCPConnection(tcpConfig *apiserver.TCPTransport, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}

	if strings.HasPrefix(tcpConfig.URL, "http://") {
		if tcpConfig.TLSConfig != nil {
			allErrs = append(allErrs, field.Invalid(
				fldPath.Child("tlsConfig"),
				"nil",
				"TLSConfig config should not be present when using HTTP"))
		}
	} else if strings.HasPrefix(tcpConfig.URL, "https://") {
		return validateTLSConfig(tcpConfig.TLSConfig, fldPath)
	} else {
		allErrs = append(allErrs, field.Invalid(
			fldPath.Child("url"),
			tcpConfig.URL,
			"supported connection protocols are http:// and https://"))
	}
	return allErrs
}

func validateTLSConfig(tlsConfig *apiserver.TLSConfig, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}

	if tlsConfig == nil {
		allErrs = append(allErrs, field.Required(
			fldPath.Child("tlsConfig"),
			"TLSConfig must be present when using HTTPS"))
		return allErrs
	}
	if tlsConfig.CABundle != "" {
		if exists, err := path.Exists(path.CheckFollowSymlink, tlsConfig.CABundle); !exists || err != nil {
			allErrs = append(allErrs, field.Invalid(
				fldPath.Child("tlsConfig", "caBundle"),
				tlsConfig.CABundle,
				"TLS config ca bundle does not exist"))
		}
	}
	if tlsConfig.ClientCert == "" {
		allErrs = append(allErrs, field.Invalid(
			fldPath.Child("tlsConfig", "clientCert"),
			"nil",
			"Using TLS requires clientCert"))
	} else if exists, err := path.Exists(path.CheckFollowSymlink, tlsConfig.ClientCert); !exists || err != nil {
		allErrs = append(allErrs, field.Invalid(
			fldPath.Child("tlsConfig", "clientCert"),
			tlsConfig.ClientCert,
			"TLS client cert does not exist"))
	}
	if tlsConfig.ClientKey == "" {
		allErrs = append(allErrs, field.Invalid(
			fldPath.Child("tlsConfig", "clientKey"),
			"nil",
			"Using TLS requires requires clientKey"))
	} else if exists, err := path.Exists(path.CheckFollowSymlink, tlsConfig.ClientKey); !exists || err != nil {
		allErrs = append(allErrs, field.Invalid(
			fldPath.Child("tlsConfig", "clientKey"),
			tlsConfig.ClientKey,
			"TLS client key does not exist"))
	}
	return allErrs
}
