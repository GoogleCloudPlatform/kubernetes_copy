/*
Copyright 2023 The Kubernetes Authors.

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

package validation

import (
	"k8s.io/apimachinery/pkg/util/validation/field"
	netutils "k8s.io/utils/net"
)

// IsValidIP tests that the argument is a valid IP address.
func IsValidIP(fldPath *field.Path, value string) field.ErrorList {
	var allErrors field.ErrorList
	if netutils.ParseIPSloppy(value) == nil {
		allErrors = append(allErrors, field.Invalid(fldPath, value, "must be a valid IP address, (e.g. 10.9.8.7 or 2001:db8::ffff)"))
	}
	return allErrors
}

// IsValidIPv4Address tests that the argument is a valid IPv4 address.
func IsValidIPv4Address(fldPath *field.Path, value string) field.ErrorList {
	var allErrors field.ErrorList
	ip := netutils.ParseIPSloppy(value)
	if ip == nil || ip.To4() == nil {
		allErrors = append(allErrors, field.Invalid(fldPath, value, "must be a valid IPv4 address"))
	}
	return allErrors
}

// IsValidIPv6Address tests that the argument is a valid IPv6 address.
func IsValidIPv6Address(fldPath *field.Path, value string) field.ErrorList {
	var allErrors field.ErrorList
	ip := netutils.ParseIPSloppy(value)
	if ip == nil || ip.To4() != nil {
		allErrors = append(allErrors, field.Invalid(fldPath, value, "must be a valid IPv6 address"))
	}
	return allErrors
}

// IsValidCIDR tests that the argument is a valid CIDR value.
func IsValidCIDR(fldPath *field.Path, value string) field.ErrorList {
	var allErrors field.ErrorList
	_, _, err := netutils.ParseCIDRSloppy(value)
	if err != nil {
		allErrors = append(allErrors, field.Invalid(fldPath, value, "must be a valid CIDR value, (e.g. 10.9.8.0/24 or 2001:db8::/64)"))
	}
	return allErrors
}
