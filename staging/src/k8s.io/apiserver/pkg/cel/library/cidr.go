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

package library

import (
	"fmt"
	"net/netip"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"

	apiservercel "k8s.io/apiserver/pkg/cel"
)

// CIDR provides a CEL function library extension of CIDR notation parsing functions.
//
// cidr
//
// Converts a string in CIDR notation to a network address representation or results in an error if the string is not a valid CIDR notation.
// The CIDR must be an IPv4 or IPv6 subnet address with a mask.
// Leading zeros in IPv4 address octets are not allowed.
// IPv4-mapped IPv6 addresses (e.g. ::ffff:1.2.3.4/24) are not allowed.
//
//	cidr(<string>) <CIDR>
//
// Examples:
//
//	cidr('192.168.0.0/16') // returns an IPv4 address with a CIDR mask
//	cidr('::1/128') // returns an IPv6 address with a CIDR mask
//	cidr('192.168.0.0/33') // error
//	cidr('::1/129') // error
//	cidr('192.168.0.1/16') // error, because there are non-0 bits after the prefix
//
// isCIDR
//
// Returns true if a string is a valid CIDR notation representation of a subnet with mask.
// The CIDR must be an IPv4 or IPv6 subnet address with a mask.
// Leading zeros in IPv4 address octets are not allowed.
// IPv4-mapped IPv6 addresses (e.g. ::ffff:1.2.3.4/24) are not allowed.
//
//	isCIDR(<string>) <bool>
//
// Examples:
//
//	isCIDR('192.168.0.0/16') // returns true
//	isCIDR('::1/128') // returns true
//	isCIDR('192.168.0.0/33') // returns false
//	isCIDR('::1/129') // returns false
//
// containsIP / containerCIDR / ip / masked / prefixLength
//
// - containsIP: Returns true if a the CIDR contains the given IP address.
// The IP address must be an IPv4 or IPv6 address.
// May take either a string or IP address as an argument.
//
// - containsCIDR: Returns true if a the CIDR contains the given CIDR.
// The CIDR must be an IPv4 or IPv6 subnet address with a mask.
// May take either a string or CIDR as an argument.
//
// - ip: Returns the IP address representation of the CIDR.
//
// - masked: Returns the CIDR representation of the network address with a masked prefix.
// This can be used to return the canonical form of the CIDR network.
//
// - prefixLength: Returns the prefix length of the CIDR in bits.
// This is the number of bits in the mask.
//
// Examples:
//
// cidr('192.168.0.0/24').containsIP(ip('192.168.0.1')) // returns true
// cidr('192.168.0.0/24').containsIP(ip('192.168.1.1')) // returns false
// cidr('192.168.0.0/24').containsIP('192.168.0.1') // returns true
// cidr('192.168.0.0/24').containsIP('192.168.1.1') // returns false
// cidr('192.168.0.0/16').containsCIDR(cidr('192.168.10.0/24')) // returns true
// cidr('192.168.1.0/24').containsCIDR(cidr('192.168.2.0/24')) // returns false
// cidr('192.168.0.0/16').containsCIDR('192.168.10.0/24') // returns true
// cidr('192.168.1.0/24').containsCIDR('192.168.2.0/24') // returns false
// cidr('192.168.0.1/24').ip() // returns ipAddr('192.168.0.1')
// cidr('192.168.0.1/24').ip().family() // returns '4'
// cidr('::1/128').ip() // returns ipAddr('::1')
// cidr('::1/128').ip().family() // returns '6'
// cidr('192.168.0.0/24').masked() // returns cidr('192.168.0.0/24')
// cidr('192.168.0.1/24').masked() // returns cidr('192.168.0.0/24')
// cidr('192.168.0.0/24') == cidr('192.168.0.0/24').masked() // returns true, CIDR was already in canonical format
// cidr('192.168.0.1/24') == cidr('192.168.0.1/24').masked() // returns false, CIDR was not in canonical format
// cidr('192.168.0.0/16').prefixLength() // returns 16
// cidr('::1/128').prefixLength() // returns 128
func CIDR() cel.EnvOption {
	return cel.Lib(cidrsLib)
}

var cidrsLib = &cidrs{}

type cidrs struct{}

func (*cidrs) LibraryName() string {
	return "kubernetes.net.cidr"
}

func (*cidrs) declarations() map[string][]cel.FunctionOpt {
	return cidrLibraryDecls
}

func (*cidrs) Types() []*cel.Type {
	return []*cel.Type{apiservercel.CIDRType, apiservercel.IPType}
}

var cidrLibraryDecls = map[string][]cel.FunctionOpt{
	"cidr": {
		cel.Overload("string_to_cidr", []*cel.Type{cel.StringType}, apiservercel.CIDRType,
			cel.UnaryBinding(stringToCIDR)),
	},
	"containsIP": {
		cel.MemberOverload("cidr_contains_ip_string", []*cel.Type{apiservercel.CIDRType, cel.StringType}, cel.BoolType,
			cel.BinaryBinding(cidrContainsIPString)),
		cel.MemberOverload("cidr_contains_ip_ip", []*cel.Type{apiservercel.CIDRType, apiservercel.IPType}, cel.BoolType,
			cel.BinaryBinding(cidrContainsIP)),
	},
	"containsCIDR": {
		cel.MemberOverload("cidr_contains_cidr_string", []*cel.Type{apiservercel.CIDRType, cel.StringType}, cel.BoolType,
			cel.BinaryBinding(cidrContainsCIDRString)),
		cel.MemberOverload("cidr_contains_cidr", []*cel.Type{apiservercel.CIDRType, apiservercel.CIDRType}, cel.BoolType,
			cel.BinaryBinding(cidrContainsCIDR)),
	},
	"ip": {
		cel.MemberOverload("cidr_ip", []*cel.Type{apiservercel.CIDRType}, apiservercel.IPType,
			cel.UnaryBinding(cidrToIP)),
	},
	"prefixLength": {
		cel.MemberOverload("cidr_prefix_length", []*cel.Type{apiservercel.CIDRType}, cel.IntType,
			cel.UnaryBinding(prefixLength)),
	},
	"masked": {
		cel.MemberOverload("cidr_masked", []*cel.Type{apiservercel.CIDRType}, apiservercel.CIDRType,
			cel.UnaryBinding(masked)),
	},
	"isCIDR": {
		cel.Overload("is_cidr", []*cel.Type{cel.StringType}, cel.BoolType,
			cel.UnaryBinding(isCIDR)),
	},
	"string": {
		cel.Overload("cidr_to_string", []*cel.Type{apiservercel.CIDRType}, cel.StringType,
			cel.UnaryBinding(cidrToString)),
	},
}

func (*cidrs) CompileOptions() []cel.EnvOption {
	options := []cel.EnvOption{cel.Types(apiservercel.CIDRType)}
	for name, overloads := range cidrLibraryDecls {
		options = append(options, cel.Function(name, overloads...))
	}
	return options
}

func (*cidrs) ProgramOptions() []cel.ProgramOption {
	return []cel.ProgramOption{}
}

func stringToCIDR(arg ref.Val) ref.Val {
	s, ok := arg.Value().(string)
	if !ok {
		return types.MaybeNoSuchOverloadErr(arg)
	}

	net, err := parseCIDR(s)
	if err != nil {
		return types.NewErr("network address parse error during conversion from string: %v", err)
	}

	return apiservercel.CIDR{
		Prefix: net,
	}
}

func cidrToString(arg ref.Val) ref.Val {
	cidr, ok := arg.(apiservercel.CIDR)
	if !ok {
		return types.MaybeNoSuchOverloadErr(arg)
	}

	return types.String(cidr.Prefix.String())
}

func cidrContainsIPString(arg ref.Val, other ref.Val) ref.Val {
	return cidrContainsIP(arg, stringToIP(other))
}

func cidrContainsCIDRString(arg ref.Val, other ref.Val) ref.Val {
	return cidrContainsCIDR(arg, stringToCIDR(other))
}

func cidrContainsIP(arg ref.Val, other ref.Val) ref.Val {
	cidr, ok := arg.(apiservercel.CIDR)
	if !ok {
		return types.MaybeNoSuchOverloadErr(other)
	}

	ip, ok := other.(apiservercel.IP)
	if !ok {
		return types.MaybeNoSuchOverloadErr(arg)
	}

	return types.Bool(cidr.Contains(ip.Addr))
}

func cidrContainsCIDR(arg ref.Val, other ref.Val) ref.Val {
	cidr, ok := arg.(apiservercel.CIDR)
	if !ok {
		return types.MaybeNoSuchOverloadErr(arg)
	}

	containsCIDR, ok := other.(apiservercel.CIDR)
	if !ok {
		return types.MaybeNoSuchOverloadErr(other)
	}

	equalMasked := cidr.Prefix.Masked() == netip.PrefixFrom(containsCIDR.Prefix.Addr(), cidr.Prefix.Bits())
	return types.Bool(equalMasked && cidr.Prefix.Bits() <= containsCIDR.Prefix.Bits())
}

func prefixLength(arg ref.Val) ref.Val {
	cidr, ok := arg.(apiservercel.CIDR)
	if !ok {
		return types.MaybeNoSuchOverloadErr(arg)
	}

	return types.Int(cidr.Prefix.Bits())
}

func isCIDR(arg ref.Val) ref.Val {
	s, ok := arg.Value().(string)
	if !ok {
		return types.MaybeNoSuchOverloadErr(arg)
	}

	_, err := parseCIDR(s)
	return types.Bool(err == nil)
}

func cidrToIP(arg ref.Val) ref.Val {
	cidr, ok := arg.(apiservercel.CIDR)
	if !ok {
		return types.MaybeNoSuchOverloadErr(arg)
	}

	return apiservercel.IP{
		Addr: cidr.Prefix.Addr(),
	}
}

func masked(arg ref.Val) ref.Val {
	cidr, ok := arg.(apiservercel.CIDR)
	if !ok {
		return types.MaybeNoSuchOverloadErr(arg)
	}

	maskedCIDR := cidr.Prefix.Masked()
	return apiservercel.CIDR{
		Prefix: maskedCIDR,
	}
}

// parseCIDR parses a string into an CIDR.
// We use this function to parse CIDR notation in the CEL library
// so that we can share the common logic of rejecting strings
// that IPv4-mapped IPv6 addresses or contain non-zero bits after the mask.
func parseCIDR(raw string) (netip.Prefix, error) {
	net, err := netip.ParsePrefix(raw)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("network address parse error during conversion from string: %v", err)
	}

	if net.Addr().Is4In6() {
		return netip.Prefix{}, fmt.Errorf("IPv4-mapped IPv6 address %q is not allowed", raw)
	}

	return net, nil
}
