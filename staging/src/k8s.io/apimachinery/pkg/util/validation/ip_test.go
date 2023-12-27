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
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/util/validation/field"
)

func TestIsValidIP(t *testing.T) {
	for _, tc := range []struct {
		name   string
		in     string
		family int
		err    string
	}{
		// GOOD VALUES
		{
			name:   "ipv4",
			in:     "1.2.3.4",
			family: 4,
		},
		{
			name:   "ipv4, all zeros",
			in:     "0.0.0.0",
			family: 4,
		},
		{
			name:   "ipv4, max",
			in:     "255.255.255.255",
			family: 4,
		},
		{
			name:   "ipv6",
			in:     "1234::abcd",
			family: 6,
		},
		{
			name:   "ipv6, all zeros, collapsed",
			in:     "::",
			family: 6,
		},
		{
			name:   "ipv6, max",
			in:     "ffff:ffff:ffff:ffff:ffff:ffff:ffff:ffff",
			family: 6,
		},

		// GOOD, THOUGH NON-CANONICAL, VALUES
		{
			name:   "ipv6, all zeros, expanded (non-canonical)",
			in:     "0:0:0:0:0:0:0:0",
			family: 6,
		},
		{
			name:   "ipv6, leading 0s (non-canonical)",
			in:     "0001:002:03:4::",
			family: 6,
		},
		{
			name:   "ipv6, capital letters (non-canonical)",
			in:     "1234::ABCD",
			family: 6,
		},

		// BAD VALUES WE CURRENTLY CONSIDER GOOD
		{
			name:   "ipv4 with leading 0s",
			in:     "1.1.1.01",
			family: 4,
		},
		{
			name:   "ipv4-in-ipv6 value",
			in:     "::ffff:1.1.1.1",
			family: 4,
		},

		// BAD VALUES
		{
			name: "empty string",
			in:   "",
			err:  "must be a valid IP address",
		},
		{
			name: "junk",
			in:   "aaaaaaa",
			err:  "must be a valid IP address",
		},
		{
			name: "domain name",
			in:   "myhost.mydomain",
			err:  "must be a valid IP address",
		},
		{
			name: "cidr",
			in:   "1.2.3.0/24",
			err:  "must be a valid IP address",
		},
		{
			name: "ipv4 with out-of-range octets",
			in:   "1.2.3.400",
			err:  "must be a valid IP address",
		},
		{
			name: "ipv4 with negative octets",
			in:   "-1.0.0.0",
			err:  "must be a valid IP address",
		},
		{
			name: "ipv6 with out-of-range segment",
			in:   "2001:db8::10005",
			err:  "must be a valid IP address",
		},
		{
			name: "ipv4:port",
			in:   "1.2.3.4:80",
			err:  "must be a valid IP address",
		},
		{
			name: "ipv6 with brackets",
			in:   "[2001:db8::1]",
			err:  "must be a valid IP address",
		},
		{
			name: "[ipv6]:port",
			in:   "[2001:db8::1]:80",
			err:  "must be a valid IP address",
		},
		{
			name: "host:port",
			in:   "example.com:80",
			err:  "must be a valid IP address",
		},
		{
			name: "ipv6 with zone",
			in:   "1234::abcd%eth0",
			err:  "must be a valid IP address",
		},
		{
			name: "ipv4 with zone",
			in:   "169.254.0.0%eth0",
			err:  "must be a valid IP address",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			errs := IsValidIP(field.NewPath(""), tc.in)
			if tc.err == "" {
				if len(errs) != 0 {
					t.Errorf("expected %q to be valid but got: %v", tc.in, errs)
				}
			} else {
				if len(errs) != 1 {
					t.Errorf("expected %q to have 1 error but got: %v", tc.in, errs)
				} else if !strings.Contains(errs[0].Detail, tc.err) {
					t.Errorf("expected error for %q to contain %q but got: %q", tc.in, tc.err, errs[0].Detail)
				}
			}

			errs = IsValidIPv4Address(field.NewPath(""), tc.in)
			if tc.family == 4 {
				if len(errs) != 0 {
					t.Errorf("expected %q to pass IsValidIPv4Address but got: %v", tc.in, errs)
				}
			} else {
				if len(errs) == 0 {
					t.Errorf("expected %q to fail IsValidIPv4Address", tc.in)
				}
			}

			errs = IsValidIPv6Address(field.NewPath(""), tc.in)
			if tc.family == 6 {
				if len(errs) != 0 {
					t.Errorf("expected %q to pass IsValidIPv6Address but got: %v", tc.in, errs)
				}
			} else {
				if len(errs) == 0 {
					t.Errorf("expected %q to fail IsValidIPv6Address", tc.in)
				}
			}
		})
	}
}

func TestIsValidCIDR(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		err  string
	}{
		// GOOD VALUES
		{
			name: "ipv4",
			in:   "1.0.0.0/8",
		},
		{
			name: "ipv4, all IPs",
			in:   "0.0.0.0/0",
		},
		{
			name: "ipv4, single IP",
			in:   "1.1.1.1/32",
		},
		{
			name: "ipv6",
			in:   "2001:4860:4860::/48",
		},
		{
			name: "ipv6, all IPs",
			in:   "::/0",
		},
		{
			name: "ipv6, single IP",
			in:   "::1/128",
		},

		// GOOD, THOUGH NON-CANONICAL, VALUES
		{
			name: "ipv6, extra 0s (non-canonical)",
			in:   "2a00:79e0:2:0::/64",
		},
		{
			name: "ipv6, capital letters (non-canonical)",
			in:   "2001:DB8::/64",
		},

		// BAD VALUES WE CURRENTLY CONSIDER GOOD
		{
			name: "ipv4 with leading 0s",
			in:   "1.1.01.0/24",
		},
		{
			name: "ipv4-in-ipv6 with ipv4-sized prefix",
			in:   "::ffff:1.1.1.0/24",
		},
		{
			name: "ipv4-in-ipv6 with ipv6-sized prefix",
			in:   "::ffff:1.1.1.0/120",
		},
		{
			name: "ipv4 with bits past prefix",
			in:   "1.2.3.4/24",
		},
		{
			name: "ipv6 with bits past prefix",
			in:   "2001:db8::1/64",
		},
		{
			name: "prefix length with leading 0s",
			in:   "192.168.0.0/016",
		},

		// BAD VALUES
		{
			name: "empty string",
			in:   "",
			err:  "must be a valid CIDR value",
		},
		{
			name: "junk",
			in:   "aaaaaaa",
			err:  "must be a valid CIDR value",
		},
		{
			name: "IP address",
			in:   "1.2.3.4",
			err:  "must be a valid CIDR value",
		},
		{
			name: "partial URL",
			in:   "192.168.0.1/healthz",
			err:  "must be a valid CIDR value",
		},
		{
			name: "partial URL 2",
			in:   "192.168.0.1/0/99",
			err:  "must be a valid CIDR value",
		},
		{
			name: "negative prefix length",
			in:   "192.168.0.0/-16",
			err:  "must be a valid CIDR value",
		},
		{
			name: "prefix length with sign",
			in:   "192.168.0.0/+16",
			err:  "must be a valid CIDR value",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			errs := IsValidCIDR(field.NewPath(""), tc.in)
			if tc.err == "" {
				if len(errs) != 0 {
					t.Errorf("expected %q to be valid but got: %v", tc.in, errs)
				}
			} else {
				if len(errs) != 1 {
					t.Errorf("expected %q to have 1 error but got: %v", tc.in, errs)
				} else if !strings.Contains(errs[0].Detail, tc.err) {
					t.Errorf("expected error for %q to contain %q but got: %q", tc.in, tc.err, errs[0].Detail)
				}
			}
		})
	}
}
