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
	"fmt"
	"net/netip"

	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/klog/v2"
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

// GetWarningsForIP returns warnings for IP address values in non-standard forms. This
// should only be used with fields that are validated with IsValidIP().
func GetWarningsForIP(fldPath *field.Path, value string) []string {
	ip := netutils.ParseIPSloppy(value)
	if ip == nil {
		klog.ErrorS(nil, "GetWarningsForIP called on value that was not validated with IsValidIP", "field", fldPath, "value", value)
		return nil
	}

	addr, _ := netip.ParseAddr(value)
	if !addr.IsValid() || addr.Is4In6() {
		// This catches 2 cases: leading 0s (if ParseIPSloppy() accepted it but
		// ParseAddr() doesn't) or IPv4-mapped IPv6 (.Is4In6()). Either way,
		// re-stringifying the net.IP value will give the preferred form.
		return []string{
			fmt.Sprintf("%s: non-standard IP address %q will be considered invalid in a future Kubernetes release: use %q", fldPath, value, ip.String()),
		}
	}

	// If ParseIPSloppy() and ParseAddr() both accept it then it's fully valid, though
	// it may be non-canonical.
	if addr.Is6() && addr.String() != value {
		return []string{
			fmt.Sprintf("%s: IPv6 address %q should be in RFC 5952 canonical format (%q)", fldPath, value, addr.String()),
		}
	}

	return nil
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

// GetWarningsForCIDR returns warnings for CIDR values in non-standard forms. This should
// only be used with fields that are validated with IsValidCIDR().
func GetWarningsForCIDR(fldPath *field.Path, value string) []string {
	ip, ipnet, err := netutils.ParseCIDRSloppy(value)
	if err != nil {
		klog.ErrorS(err, "GetWarningsForCIDR called on value that was not validated with IsValidCIDR", "field", fldPath, "value", value)
		return nil
	}

	var warnings []string

	// Check for bits set after prefix length
	if !ip.Equal(ipnet.IP) {
		_, addrlen := ipnet.Mask.Size()
		singleIPCIDR := fmt.Sprintf("%s/%d", ip.String(), addrlen)
		warnings = append(warnings,
			fmt.Sprintf("%s: CIDR value %q is ambiguous in this context (should be %q or %q?)", fldPath, value, ipnet.String(), singleIPCIDR),
		)
	}

	prefix, _ := netip.ParsePrefix(value)
	addr := prefix.Addr()
	if !prefix.IsValid() || addr.Is4In6() {
		// This catches 2 cases: leading 0s (if ParseCIDRSloppy() accepted it but
		// ParsePrefix() doesn't) or IPv4-mapped IPv6 (.Is4In6()). Either way,
		// re-stringifying the net.IPNet value will give the preferred form.
		warnings = append(warnings,
			fmt.Sprintf("%s: non-standard CIDR value %q will be considered invalid in a future Kubernetes release: use %q", fldPath, value, ipnet.String()),
		)
	}

	// If ParseCIDRSloppy() and ParsePrefix() both accept it then it's fully valid,
	// though it may be non-canonical. But only check this if there are no other
	// warnings, since either of the other warnings would also cause a round-trip
	// failure.
	if len(warnings) == 0 && addr.Is6() && prefix.String() != value {
		warnings = append(warnings,
			fmt.Sprintf("%s: IPv6 CIDR value %q should be in RFC 5952 canonical format (%q)", fldPath, value, prefix.String()),
		)
	}

	return warnings
}
