/*
Copyright 2016 The Kubernetes Authors.

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

package cidrset

import (
	"math/big"
	"net"
	"reflect"
	"testing"

	"k8s.io/component-base/metrics/testutil"
	netutils "k8s.io/utils/net"
)

func TestCIDRSetFullyAllocated(t *testing.T) {
	cases := []struct {
		clusterCIDRStr string
		subNetMaskSize int
		expectedCIDR   string
		description    string
	}{
		{
			clusterCIDRStr: "127.123.234.0/30",
			subNetMaskSize: 30,
			expectedCIDR:   "127.123.234.0/30",
			description:    "Fully allocated CIDR with IPv4",
		},
		{
			clusterCIDRStr: "beef:1234::/30",
			subNetMaskSize: 30,
			expectedCIDR:   "beef:1234::/30",
			description:    "Fully allocated CIDR with IPv6",
		},
	}
	for _, tc := range cases {
		_, clusterCIDR, _ := netutils.ParseCIDRSloppy(tc.clusterCIDRStr)
		a, err := NewCIDRSet(clusterCIDR, tc.subNetMaskSize)
		if err != nil {
			t.Fatalf("unexpected error: %v for %v", err, tc.description)
		}
		p, err := a.AllocateNext()
		if err != nil {
			t.Fatalf("unexpected error: %v for %v", err, tc.description)
		}
		if p.String() != tc.expectedCIDR {
			t.Fatalf("unexpected allocated cidr: %v, expecting %v for %v",
				p.String(), tc.expectedCIDR, tc.description)
		}

		_, err = a.AllocateNext()
		if err == nil {
			t.Fatalf("expected error because of fully-allocated range for %v", tc.description)
		}

		a.Release(p)

		p, err = a.AllocateNext()
		if err != nil {
			t.Fatalf("unexpected error: %v for %v", err, tc.description)
		}
		if p.String() != tc.expectedCIDR {
			t.Fatalf("unexpected allocated cidr: %v, expecting %v for %v",
				p.String(), tc.expectedCIDR, tc.description)
		}
		_, err = a.AllocateNext()
		if err == nil {
			t.Fatalf("expected error because of fully-allocated range for %v", tc.description)
		}
	}
}

func TestIndexToCIDRBlock(t *testing.T) {
	cases := []struct {
		clusterCIDRStr string
		subnetMaskSize int
		index          int
		CIDRBlock      string
		description    string
	}{
		{
			clusterCIDRStr: "127.123.3.0/16",
			subnetMaskSize: 24,
			index:          0,
			CIDRBlock:      "127.123.0.0/24",
			description:    "1st IP address indexed with IPv4",
		},
		{
			clusterCIDRStr: "127.123.0.0/16",
			subnetMaskSize: 24,
			index:          15,
			CIDRBlock:      "127.123.15.0/24",
			description:    "16th IP address indexed with IPv4",
		},
		{
			clusterCIDRStr: "192.168.5.219/28",
			subnetMaskSize: 32,
			index:          5,
			CIDRBlock:      "192.168.5.213/32",
			description:    "5th IP address indexed with IPv4",
		},
		{
			clusterCIDRStr: "2001:0db8:1234:3::/48",
			subnetMaskSize: 64,
			index:          0,
			CIDRBlock:      "2001:db8:1234::/64",
			description:    "1st IP address indexed with IPv6 /64",
		},
		{
			clusterCIDRStr: "2001:0db8:1234::/48",
			subnetMaskSize: 64,
			index:          15,
			CIDRBlock:      "2001:db8:1234:f::/64",
			description:    "16th IP address indexed with IPv6 /64",
		},
		{
			clusterCIDRStr: "2001:0db8:85a3::8a2e:0370:7334/50",
			subnetMaskSize: 63,
			index:          6425,
			CIDRBlock:      "2001:db8:85a3:3232::/63",
			description:    "6426th IP address indexed with IPv6 /63",
		},
		{
			clusterCIDRStr: "2001:0db8::/32",
			subnetMaskSize: 48,
			index:          0,
			CIDRBlock:      "2001:db8::/48",
			description:    "1st IP address indexed with IPv6 /48",
		},
		{
			clusterCIDRStr: "2001:0db8::/32",
			subnetMaskSize: 48,
			index:          15,
			CIDRBlock:      "2001:db8:f::/48",
			description:    "16th IP address indexed with IPv6 /48",
		},
		{
			clusterCIDRStr: "2001:0db8:85a3::8a2e:0370:7334/32",
			subnetMaskSize: 48,
			index:          6425,
			CIDRBlock:      "2001:db8:1919::/48",
			description:    "6426th IP address indexed with IPv6 /48",
		},
		{
			clusterCIDRStr: "2001:0db8:1234:ff00::/56",
			subnetMaskSize: 72,
			index:          0,
			CIDRBlock:      "2001:db8:1234:ff00::/72",
			description:    "1st IP address indexed with IPv6 /72",
		},
		{
			clusterCIDRStr: "2001:0db8:1234:ff00::/56",
			subnetMaskSize: 72,
			index:          15,
			CIDRBlock:      "2001:db8:1234:ff00:f00::/72",
			description:    "16th IP address indexed with IPv6 /72",
		},
		{
			clusterCIDRStr: "2001:0db8:1234:ff00::0370:7334/56",
			subnetMaskSize: 72,
			index:          6425,
			CIDRBlock:      "2001:db8:1234:ff19:1900::/72",
			description:    "6426th IP address indexed with IPv6 /72",
		},
		{
			clusterCIDRStr: "2001:0db8:1234:0:1234::/80",
			subnetMaskSize: 96,
			index:          0,
			CIDRBlock:      "2001:db8:1234:0:1234::/96",
			description:    "1st IP address indexed with IPv6 /96",
		},
		{
			clusterCIDRStr: "2001:0db8:1234:0:1234::/80",
			subnetMaskSize: 96,
			index:          15,
			CIDRBlock:      "2001:db8:1234:0:1234:f::/96",
			description:    "16th IP address indexed with IPv6 /96",
		},
		{
			clusterCIDRStr: "2001:0db8:1234:ff00::0370:7334/80",
			subnetMaskSize: 96,
			index:          6425,
			CIDRBlock:      "2001:db8:1234:ff00:0:1919::/96",
			description:    "6426th IP address indexed with IPv6 /96",
		},
	}
	for _, tc := range cases {
		_, clusterCIDR, _ := netutils.ParseCIDRSloppy(tc.clusterCIDRStr)
		a, err := NewCIDRSet(clusterCIDR, tc.subnetMaskSize)
		if err != nil {
			t.Fatalf("error for %v ", tc.description)
		}
		cidr := a.indexToCIDRBlock(tc.index)
		if cidr.String() != tc.CIDRBlock {
			t.Fatalf("error for %v index %d %s", tc.description, tc.index, cidr.String())
		}
	}
}

func TestCIDRSet_RandomishAllocation(t *testing.T) {
	cases := []struct {
		clusterCIDRStr string
		description    string
	}{
		{
			clusterCIDRStr: "127.123.234.0/16",
			description:    "RandomishAllocation with IPv4",
		},
		{
			clusterCIDRStr: "beef:1234::/16",
			description:    "RandomishAllocation with IPv6",
		},
	}
	for _, tc := range cases {
		_, clusterCIDR, _ := netutils.ParseCIDRSloppy(tc.clusterCIDRStr)
		a, err := NewCIDRSet(clusterCIDR, 24)
		if err != nil {
			t.Fatalf("Error allocating CIDRSet for %v", tc.description)
		}
		// allocate all the CIDRs
		var cidrs []*net.IPNet

		for i := 0; i < 256; i++ {
			if c, err := a.AllocateNext(); err == nil {
				cidrs = append(cidrs, c)
			} else {
				t.Fatalf("unexpected error: %v for %v", err, tc.description)
			}
		}

		//var err error
		_, err = a.AllocateNext()
		if err == nil {
			t.Fatalf("expected error because of fully-allocated range for %v", tc.description)
		}
		// release them all
		for i := 0; i < len(cidrs); i++ {
			a.Release(cidrs[i])
		}

		// allocate the CIDRs again
		var rcidrs []*net.IPNet
		for i := 0; i < 256; i++ {
			if c, err := a.AllocateNext(); err == nil {
				rcidrs = append(rcidrs, c)
			} else {
				t.Fatalf("unexpected error: %d, %v for %v", i, err, tc.description)
			}
		}
		_, err = a.AllocateNext()
		if err == nil {
			t.Fatalf("expected error because of fully-allocated range for %v", tc.description)
		}

		if !reflect.DeepEqual(cidrs, rcidrs) {
			t.Fatalf("expected re-allocated cidrs are the same collection for %v", tc.description)
		}
	}
}

func TestCIDRSet_AllocationOccupied(t *testing.T) {
	cases := []struct {
		clusterCIDRStr string
		description    string
	}{
		{
			clusterCIDRStr: "127.123.234.0/16",
			description:    "AllocationOccupied with IPv4",
		},
		{
			clusterCIDRStr: "beef:1234::/16",
			description:    "AllocationOccupied with IPv6",
		},
	}
	for _, tc := range cases {
		_, clusterCIDR, _ := netutils.ParseCIDRSloppy(tc.clusterCIDRStr)
		a, err := NewCIDRSet(clusterCIDR, 24)
		if err != nil {
			t.Fatalf("Error allocating CIDRSet for %v", tc.description)
		}
		// allocate all the CIDRs
		var cidrs []*net.IPNet
		var numCIDRs = 256

		for i := 0; i < numCIDRs; i++ {
			if c, err := a.AllocateNext(); err == nil {
				cidrs = append(cidrs, c)
			} else {
				t.Fatalf("unexpected error: %v for %v", err, tc.description)
			}
		}

		//var err error
		_, err = a.AllocateNext()
		if err == nil {
			t.Fatalf("expected error because of fully-allocated range for %v", tc.description)
		}
		// release them all
		for i := 0; i < len(cidrs); i++ {
			a.Release(cidrs[i])
		}
		// occupy the last 128 CIDRs
		for i := numCIDRs / 2; i < numCIDRs; i++ {
			a.Occupy(cidrs[i])
		}
		// occupy the first of the last 128 again
		a.Occupy(cidrs[numCIDRs/2])

		// allocate the first 128 CIDRs again
		var rcidrs []*net.IPNet
		for i := 0; i < numCIDRs/2; i++ {
			if c, err := a.AllocateNext(); err == nil {
				rcidrs = append(rcidrs, c)
			} else {
				t.Fatalf("unexpected error: %d, %v for %v", i, err, tc.description)
			}
		}
		_, err = a.AllocateNext()
		if err == nil {
			t.Fatalf("expected error because of fully-allocated range for %v", tc.description)
		}

		// check Occupy() work properly
		for i := numCIDRs / 2; i < numCIDRs; i++ {
			rcidrs = append(rcidrs, cidrs[i])
		}
		if !reflect.DeepEqual(cidrs, rcidrs) {
			t.Fatalf("expected re-allocated cidrs are the same collection for %v", tc.description)
		}
	}
}

func TestDoubleOccupyRelease(t *testing.T) {
	// Run a sequence of operations and check the number of occupied CIDRs
	// after each one.
	clusterCIDRStr := "10.42.0.0/16"
	operations := []struct {
		cidrStr     string
		operation   string
		numOccupied int
	}{
		// Occupy 1 element: +1
		{
			cidrStr:     "10.42.5.0/24",
			operation:   "occupy",
			numOccupied: 1,
		},
		// Occupy 1 more element: +1
		{
			cidrStr:     "10.42.9.0/24",
			operation:   "occupy",
			numOccupied: 2,
		},
		// Occupy 4 elements overlapping with one from the above: +3
		{
			cidrStr:     "10.42.8.0/22",
			operation:   "occupy",
			numOccupied: 5,
		},
		// Occupy an already-occupied element: no change
		{
			cidrStr:     "10.42.9.0/24",
			operation:   "occupy",
			numOccupied: 5,
		},
		// Release an occupied element: -1
		{
			cidrStr:     "10.42.9.0/24",
			operation:   "release",
			numOccupied: 4,
		},
		// Release an unoccupied element: no change
		{
			cidrStr:     "10.42.9.0/24",
			operation:   "release",
			numOccupied: 4,
		},
		// Release 4 elements, only one of which is occupied: -1
		{
			cidrStr:     "10.42.4.0/22",
			operation:   "release",
			numOccupied: 3,
		},
	}
	// Check that there are exactly that many allocatable CIDRs after all
	// operations have been executed.
	numAllocatable24s := (1 << 8) - 3

	_, clusterCIDR, _ := netutils.ParseCIDRSloppy(clusterCIDRStr)
	a, err := NewCIDRSet(clusterCIDR, 24)
	if err != nil {
		t.Fatalf("Error allocating CIDRSet")
	}

	// Execute the operations
	for _, op := range operations {
		_, cidr, _ := netutils.ParseCIDRSloppy(op.cidrStr)
		switch op.operation {
		case "occupy":
			a.Occupy(cidr)
		case "release":
			a.Release(cidr)
		default:
			t.Fatalf("test error: unknown operation %v", op.operation)
		}
		if a.allocatedCIDRs != op.numOccupied {
			t.Fatalf("Expected %d occupied CIDRS, got %d", op.numOccupied, a.allocatedCIDRs)
		}
	}

	// Make sure that we can allocate exactly `numAllocatable24s` elements.
	for i := 0; i < numAllocatable24s; i++ {
		_, err := a.AllocateNext()
		if err != nil {
			t.Fatalf("Expected to be able to allocate %d CIDRS, failed after %d", numAllocatable24s, i)
		}
	}

	_, err = a.AllocateNext()
	if err == nil {
		t.Fatalf("Expected to be able to allocate exactly %d CIDRS, got one more", numAllocatable24s)
	}
}

func TestGetBitforCIDR(t *testing.T) {
	cases := []struct {
		clusterCIDRStr string
		subNetMaskSize int
		subNetCIDRStr  string
		expectedBit    int
		expectErr      bool
		description    string
	}{
		{
			clusterCIDRStr: "127.0.0.0/8",
			subNetMaskSize: 16,
			subNetCIDRStr:  "127.0.0.0/16",
			expectedBit:    0,
			expectErr:      false,
			description:    "Get 0 Bit with IPv4",
		},
		{
			clusterCIDRStr: "be00::/8",
			subNetMaskSize: 16,
			subNetCIDRStr:  "be00::/16",
			expectedBit:    0,
			expectErr:      false,
			description:    "Get 0 Bit with IPv6",
		},
		{
			clusterCIDRStr: "127.0.0.0/8",
			subNetMaskSize: 16,
			subNetCIDRStr:  "127.123.0.0/16",
			expectedBit:    123,
			expectErr:      false,
			description:    "Get 123rd Bit with IPv4",
		},
		{
			clusterCIDRStr: "be00::/8",
			subNetMaskSize: 16,
			subNetCIDRStr:  "beef::/16",
			expectedBit:    0xef,
			expectErr:      false,
			description:    "Get xef Bit with IPv6",
		},
		{
			clusterCIDRStr: "127.0.0.0/8",
			subNetMaskSize: 16,
			subNetCIDRStr:  "127.168.0.0/16",
			expectedBit:    168,
			expectErr:      false,
			description:    "Get 168th Bit with IPv4",
		},
		{
			clusterCIDRStr: "be00::/8",
			subNetMaskSize: 16,
			subNetCIDRStr:  "be68::/16",
			expectedBit:    0x68,
			expectErr:      false,
			description:    "Get x68th Bit with IPv6",
		},
		{
			clusterCIDRStr: "127.0.0.0/8",
			subNetMaskSize: 16,
			subNetCIDRStr:  "127.224.0.0/16",
			expectedBit:    224,
			expectErr:      false,
			description:    "Get 224th Bit with IPv4",
		},
		{
			clusterCIDRStr: "be00::/8",
			subNetMaskSize: 16,
			subNetCIDRStr:  "be24::/16",
			expectedBit:    0x24,
			expectErr:      false,
			description:    "Get x24th Bit with IPv6",
		},
		{
			clusterCIDRStr: "192.168.0.0/16",
			subNetMaskSize: 24,
			subNetCIDRStr:  "192.168.12.0/24",
			expectedBit:    12,
			expectErr:      false,
			description:    "Get 12th Bit with IPv4",
		},
		{
			clusterCIDRStr: "beef::/16",
			subNetMaskSize: 24,
			subNetCIDRStr:  "beef:1200::/24",
			expectedBit:    0x12,
			expectErr:      false,
			description:    "Get x12th Bit with IPv6",
		},
		{
			clusterCIDRStr: "192.168.0.0/16",
			subNetMaskSize: 24,
			subNetCIDRStr:  "192.168.151.0/24",
			expectedBit:    151,
			expectErr:      false,
			description:    "Get 151st Bit with IPv4",
		},
		{
			clusterCIDRStr: "beef::/16",
			subNetMaskSize: 24,
			subNetCIDRStr:  "beef:9700::/24",
			expectedBit:    0x97,
			expectErr:      false,
			description:    "Get x97st Bit with IPv6",
		},
		{
			clusterCIDRStr: "192.168.0.0/16",
			subNetMaskSize: 24,
			subNetCIDRStr:  "127.168.224.0/24",
			expectErr:      true,
			description:    "Get error with IPv4",
		},
		{
			clusterCIDRStr: "beef::/16",
			subNetMaskSize: 24,
			subNetCIDRStr:  "2001:db00::/24",
			expectErr:      true,
			description:    "Get error with IPv6",
		},
	}

	for _, tc := range cases {
		t.Run(tc.description, func(t *testing.T) {
			_, clusterCIDR, err := netutils.ParseCIDRSloppy(tc.clusterCIDRStr)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			cs, err := NewCIDRSet(clusterCIDR, tc.subNetMaskSize)
			if err != nil {
				t.Fatalf("Error allocating CIDRSet")
			}
			_, subnetCIDR, err := netutils.ParseCIDRSloppy(tc.subNetCIDRStr)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			got, err := cs.getIndexForIP(subnetCIDR.IP)
			if err == nil && tc.expectErr {
				t.Errorf("expected error but got null")
				return
			}

			if err != nil && !tc.expectErr {
				t.Errorf("unexpected error: %v", err)
				return
			}

			if got != tc.expectedBit {
				t.Errorf("expected %v, but got %v", tc.expectedBit, got)
			}
		})
	}
}

func TestOccupy(t *testing.T) {
	cases := []struct {
		clusterCIDRStr    string
		subNetMaskSize    int
		subNetCIDRStr     string
		expectedUsedBegin int
		expectedUsedEnd   int
		expectErr         bool
		description       string
	}{
		{
			clusterCIDRStr:    "127.0.0.0/8",
			subNetMaskSize:    16,
			subNetCIDRStr:     "127.0.0.0/8",
			expectedUsedBegin: 0,
			expectedUsedEnd:   255,
			expectErr:         false,
			description:       "Occupy all Bits with IPv4",
		},
		{
			clusterCIDRStr:    "2001:beef:1200::/40",
			subNetMaskSize:    48,
			subNetCIDRStr:     "2001:beef:1200::/40",
			expectedUsedBegin: 0,
			expectedUsedEnd:   255,
			expectErr:         false,
			description:       "Occupy all Bits with IPv6",
		},
		{
			clusterCIDRStr:    "127.0.0.0/8",
			subNetMaskSize:    16,
			subNetCIDRStr:     "127.0.0.0/2",
			expectedUsedBegin: 0,
			expectedUsedEnd:   255,
			expectErr:         false,
			description:       "Occupy every Bit with IPv4",
		},
		{
			clusterCIDRStr:    "2001:beef:1200::/40",
			subNetMaskSize:    48,
			subNetCIDRStr:     "2001:beef:1234::/34",
			expectedUsedBegin: 0,
			expectedUsedEnd:   255,
			expectErr:         false,
			description:       "Occupy every Bit with IPv6",
		},
		{
			clusterCIDRStr:    "127.0.0.0/8",
			subNetMaskSize:    16,
			subNetCIDRStr:     "127.0.0.0/16",
			expectedUsedBegin: 0,
			expectedUsedEnd:   0,
			expectErr:         false,
			description:       "Occupy 1st Bit with IPv4",
		},
		{
			clusterCIDRStr:    "2001:beef:1200::/40",
			subNetMaskSize:    48,
			subNetCIDRStr:     "2001:beef:1200::/48",
			expectedUsedBegin: 0,
			expectedUsedEnd:   0,
			expectErr:         false,
			description:       "Occupy 1st Bit with IPv6",
		},
		{
			clusterCIDRStr:    "127.0.0.0/8",
			subNetMaskSize:    32,
			subNetCIDRStr:     "127.0.0.0/16",
			expectedUsedBegin: 0,
			expectedUsedEnd:   65535,
			expectErr:         false,
			description:       "Occupy 65535 Bits with IPv4",
		},
		{
			clusterCIDRStr:    "2001:beef:1200::/48",
			subNetMaskSize:    64,
			subNetCIDRStr:     "2001:beef:1200::/48",
			expectedUsedBegin: 0,
			expectedUsedEnd:   65535,
			expectErr:         false,
			description:       "Occupy 65535 Bits with IPv6",
		},
		{
			clusterCIDRStr:    "127.0.0.0/7",
			subNetMaskSize:    16,
			subNetCIDRStr:     "127.0.0.0/15",
			expectedUsedBegin: 256,
			expectedUsedEnd:   257,
			expectErr:         false,
			description:       "Occupy 257th Bit with IPv4",
		},
		{
			clusterCIDRStr:    "2001:beef:7f00::/39",
			subNetMaskSize:    48,
			subNetCIDRStr:     "2001:beef:7f00::/47",
			expectedUsedBegin: 256,
			expectedUsedEnd:   257,
			expectErr:         false,
			description:       "Occupy 257th Bit with IPv6",
		},
		{
			clusterCIDRStr:    "127.0.0.0/7",
			subNetMaskSize:    15,
			subNetCIDRStr:     "127.0.0.0/15",
			expectedUsedBegin: 128,
			expectedUsedEnd:   128,
			expectErr:         false,
			description:       "Occupy 128th Bit with IPv4",
		},
		{
			clusterCIDRStr:    "2001:beef:7f00::/39",
			subNetMaskSize:    47,
			subNetCIDRStr:     "2001:beef:7f00::/47",
			expectedUsedBegin: 128,
			expectedUsedEnd:   128,
			expectErr:         false,
			description:       "Occupy 128th Bit with IPv6",
		},
		{
			clusterCIDRStr:    "127.0.0.0/7",
			subNetMaskSize:    18,
			subNetCIDRStr:     "127.0.0.0/15",
			expectedUsedBegin: 1024,
			expectedUsedEnd:   1031,
			expectErr:         false,
			description:       "Occupy 1031st Bit with IPv4",
		},
		{
			clusterCIDRStr:    "2001:beef:7f00::/39",
			subNetMaskSize:    50,
			subNetCIDRStr:     "2001:beef:7f00::/47",
			expectedUsedBegin: 1024,
			expectedUsedEnd:   1031,
			expectErr:         false,
			description:       "Occupy 1031st Bit with IPv6",
		},
	}

	for _, tc := range cases {
		_, clusterCIDR, err := netutils.ParseCIDRSloppy(tc.clusterCIDRStr)
		if err != nil {
			t.Fatalf("unexpected error: %v for %v", err, tc.description)
		}

		cs, err := NewCIDRSet(clusterCIDR, tc.subNetMaskSize)
		if err != nil {
			t.Fatalf("Error allocating CIDRSet for %v", tc.description)
		}

		_, subnetCIDR, err := netutils.ParseCIDRSloppy(tc.subNetCIDRStr)
		if err != nil {
			t.Fatalf("unexpected error: %v for %v", err, tc.description)
		}

		err = cs.Occupy(subnetCIDR)
		if err == nil && tc.expectErr {
			t.Errorf("expected error but got none for %v", tc.description)
			continue
		}
		if err != nil && !tc.expectErr {
			t.Errorf("unexpected error: %v for %v", err, tc.description)
			continue
		}

		expectedUsed := big.Int{}
		for i := tc.expectedUsedBegin; i <= tc.expectedUsedEnd; i++ {
			expectedUsed.SetBit(&expectedUsed, i, 1)
		}
		if expectedUsed.Cmp(&cs.used) != 0 {
			t.Errorf("error for %v", tc.description)
		}
	}
}

func TestCIDRSetv6(t *testing.T) {
	cases := []struct {
		clusterCIDRStr string
		subNetMaskSize int
		expectedCIDR   string
		expectedCIDR2  string
		expectErr      bool
		description    string
	}{
		{
			clusterCIDRStr: "127.0.0.0/8",
			subNetMaskSize: 32,
			expectErr:      false,
			expectedCIDR:   "127.0.0.0/32",
			expectedCIDR2:  "127.0.0.1/32",
			description:    "Max cluster subnet size with IPv4",
		},
		{
			clusterCIDRStr: "beef:1234::/32",
			subNetMaskSize: 49,
			expectErr:      true,
			description:    "Max cluster subnet size with IPv6",
		},
		{
			clusterCIDRStr: "2001:beef:1234:369b::/60",
			subNetMaskSize: 64,
			expectedCIDR:   "2001:beef:1234:3690::/64",
			expectedCIDR2:  "2001:beef:1234:3691::/64",
			expectErr:      false,
			description:    "Allocate a few IPv6",
		},
	}
	for _, tc := range cases {
		t.Run(tc.description, func(t *testing.T) {
			_, clusterCIDR, _ := netutils.ParseCIDRSloppy(tc.clusterCIDRStr)
			a, err := NewCIDRSet(clusterCIDR, tc.subNetMaskSize)
			if gotErr := err != nil; gotErr != tc.expectErr {
				t.Fatalf("NewCIDRSet(%v, %v) = %v, %v; gotErr = %t, want %t", clusterCIDR, tc.subNetMaskSize, a, err, gotErr, tc.expectErr)
			}
			if a == nil {
				return
			}
			p, err := a.AllocateNext()
			if err == nil && tc.expectErr {
				t.Errorf("a.AllocateNext() = nil, want error")
			}
			if err != nil && !tc.expectErr {
				t.Errorf("a.AllocateNext() = %+v, want no error", err)
			}
			if !tc.expectErr {
				if p != nil && p.String() != tc.expectedCIDR {
					t.Fatalf("a.AllocateNext() got %+v, want %+v", p.String(), tc.expectedCIDR)
				}
			}
			p2, err := a.AllocateNext()
			if err == nil && tc.expectErr {
				t.Errorf("a.AllocateNext() = nil, want error")
			}
			if err != nil && !tc.expectErr {
				t.Errorf("a.AllocateNext() = %+v, want no error", err)
			}
			if !tc.expectErr {
				if p2 != nil && p2.String() != tc.expectedCIDR2 {
					t.Fatalf("a.AllocateNext() got %+v, want %+v", p2.String(), tc.expectedCIDR)
				}
			}
		})
	}
}

func TestCidrSetMetrics(t *testing.T) {
	cidr := "10.0.0.0/16"
	_, clusterCIDR, _ := netutils.ParseCIDRSloppy(cidr)
	clearMetrics(map[string]string{"clusterCIDR": cidr})

	// We have 256 free cidrs
	a, err := NewCIDRSet(clusterCIDR, 24)
	if err != nil {
		t.Fatalf("unexpected error creating CidrSet: %v", err)
	}

	clusterMaskSize, _ := clusterCIDR.Mask.Size()
	max := getMaxCIDRs(24, clusterMaskSize)
	em := testMetrics{
		usage:      0,
		allocs:     0,
		releases:   0,
		allocTries: 0,
		max:        float64(max),
	}
	expectMetrics(t, cidr, em)

	// Allocate next all
	for i := 1; i <= 256; i++ {
		_, err := a.AllocateNext()
		if err != nil {
			t.Fatalf("unexpected error allocating a new CIDR: %v", err)
		}
		em := testMetrics{
			usage:      float64(i) / float64(256),
			allocs:     float64(i),
			releases:   0,
			allocTries: 0,
			max:        float64(max),
		}
		expectMetrics(t, cidr, em)
	}
	// Release all
	a.Release(clusterCIDR)
	em = testMetrics{
		usage:      0,
		allocs:     256,
		releases:   256,
		allocTries: 0,
		max:        float64(max),
	}
	expectMetrics(t, cidr, em)

	// Allocate all
	a.Occupy(clusterCIDR)
	em = testMetrics{
		usage:      1,
		allocs:     512,
		releases:   256,
		allocTries: 0,
		max:        float64(max),
	}
	expectMetrics(t, cidr, em)
}

func TestCidrSetMetricsHistogram(t *testing.T) {
	cidr := "10.0.0.0/16"
	_, clusterCIDR, _ := netutils.ParseCIDRSloppy(cidr)
	clearMetrics(map[string]string{"clusterCIDR": cidr})

	// We have 256 free cidrs
	a, err := NewCIDRSet(clusterCIDR, 24)
	if err != nil {
		t.Fatalf("unexpected error creating CidrSet: %v", err)
	}

	clusterMaskSize, _ := clusterCIDR.Mask.Size()
	max := getMaxCIDRs(24, clusterMaskSize)
	em := testMetrics{
		usage:      0,
		allocs:     0,
		releases:   0,
		allocTries: 0,
		max:        float64(max),
	}
	expectMetrics(t, cidr, em)

	// Allocate half of the range
	// Occupy does not update the nextCandidate
	_, halfClusterCIDR, _ := netutils.ParseCIDRSloppy("10.0.0.0/17")
	a.Occupy(halfClusterCIDR)
	em = testMetrics{
		usage:      0.5,
		allocs:     128,
		releases:   0,
		allocTries: 0,
		max:        float64(max),
	}
	expectMetrics(t, cidr, em)
	// Allocate next should iterate until the next free cidr
	// that is exactly the same number we allocated previously
	_, err = a.AllocateNext()
	if err != nil {
		t.Fatalf("unexpected error allocating a new CIDR: %v", err)
	}
	em = testMetrics{
		usage:      float64(129) / float64(256),
		allocs:     129,
		releases:   0,
		allocTries: 128,
		max:        float64(max),
	}
	expectMetrics(t, cidr, em)
}

func TestCidrSetMetricsDual(t *testing.T) {
	// create IPv4 cidrSet
	cidrIPv4 := "10.0.0.0/16"
	_, clusterCIDRv4, _ := netutils.ParseCIDRSloppy(cidrIPv4)
	clearMetrics(map[string]string{"clusterCIDR": cidrIPv4})

	a, err := NewCIDRSet(clusterCIDRv4, 24)
	if err != nil {
		t.Fatalf("unexpected error creating CidrSet: %v", err)
	}

	clusterMaskSize, _ := clusterCIDRv4.Mask.Size()
	maxIPv4 := getMaxCIDRs(24, clusterMaskSize)
	em := testMetrics{
		usage:      0,
		allocs:     0,
		releases:   0,
		allocTries: 0,
		max:        float64(maxIPv4),
	}
	expectMetrics(t, cidrIPv4, em)

	// create IPv6 cidrSet
	cidrIPv6 := "2001:db8::/48"
	_, clusterCIDRv6, _ := netutils.ParseCIDRSloppy(cidrIPv6)
	clearMetrics(map[string]string{"clusterCIDR": cidrIPv6})

	b, err := NewCIDRSet(clusterCIDRv6, 64)
	if err != nil {
		t.Fatalf("unexpected error creating CidrSet: %v", err)
	}

	clusterMaskSize, _ = clusterCIDRv6.Mask.Size()
	maxIPv6 := getMaxCIDRs(64, clusterMaskSize)
	em = testMetrics{
		usage:      0,
		allocs:     0,
		releases:   0,
		allocTries: 0,
		max:        float64(maxIPv6),
	}
	expectMetrics(t, cidrIPv6, em)

	// Allocate all
	a.Occupy(clusterCIDRv4)
	em = testMetrics{
		usage:      1,
		allocs:     256,
		releases:   0,
		allocTries: 0,
		max:        float64(maxIPv4),
	}
	expectMetrics(t, cidrIPv4, em)

	b.Occupy(clusterCIDRv6)
	em = testMetrics{
		usage:      1,
		allocs:     65536,
		releases:   0,
		allocTries: 0,
		max:        float64(maxIPv6),
	}
	expectMetrics(t, cidrIPv6, em)

	// Release all
	a.Release(clusterCIDRv4)
	em = testMetrics{
		usage:      0,
		allocs:     256,
		releases:   256,
		allocTries: 0,
		max:        float64(maxIPv4),
	}
	expectMetrics(t, cidrIPv4, em)
	b.Release(clusterCIDRv6)
	em = testMetrics{
		usage:      0,
		allocs:     65536,
		releases:   65536,
		allocTries: 0,
		max:        float64(maxIPv6),
	}
	expectMetrics(t, cidrIPv6, em)
}

func Test_getMaxCIDRs(t *testing.T) {
	cidrIPv4 := "10.0.0.0/16"
	_, clusterCIDRv4, _ := netutils.ParseCIDRSloppy(cidrIPv4)

	cidrIPv6 := "2001:db8::/48"
	_, clusterCIDRv6, _ := netutils.ParseCIDRSloppy(cidrIPv6)

	tests := []struct {
		name             string
		subNetMaskSize   int
		clusterCIDR      *net.IPNet
		expectedMaxCIDRs int
	}{
		{
			name:             "IPv4",
			subNetMaskSize:   24,
			clusterCIDR:      clusterCIDRv4,
			expectedMaxCIDRs: 256,
		},
		{
			name:             "IPv6",
			subNetMaskSize:   64,
			clusterCIDR:      clusterCIDRv6,
			expectedMaxCIDRs: 65536,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			clusterMaskSize, _ := test.clusterCIDR.Mask.Size()
			maxCIDRs := getMaxCIDRs(test.subNetMaskSize, clusterMaskSize)
			if test.expectedMaxCIDRs != maxCIDRs {
				t.Errorf("incorrect maxCIDRs, expected: %d, got: %d", test.expectedMaxCIDRs, maxCIDRs)
			}
		})
	}
}

// Metrics helpers
func clearMetrics(labels map[string]string) {
	cidrSetAllocations.Delete(labels)
	cidrSetReleases.Delete(labels)
	cidrSetUsage.Delete(labels)
	cidrSetAllocationTriesPerRequest.Delete(labels)
	cidrSetMaxCidrs.Delete(labels)
}

type testMetrics struct {
	usage      float64
	allocs     float64
	releases   float64
	allocTries float64
	max        float64
}

func expectMetrics(t *testing.T, label string, em testMetrics) {
	var m testMetrics
	var err error
	m.usage, err = testutil.GetGaugeMetricValue(cidrSetUsage.WithLabelValues(label))
	if err != nil {
		t.Errorf("failed to get %s value, err: %v", cidrSetUsage.Name, err)
	}
	m.allocs, err = testutil.GetCounterMetricValue(cidrSetAllocations.WithLabelValues(label))
	if err != nil {
		t.Errorf("failed to get %s value, err: %v", cidrSetAllocations.Name, err)
	}
	m.releases, err = testutil.GetCounterMetricValue(cidrSetReleases.WithLabelValues(label))
	if err != nil {
		t.Errorf("failed to get %s value, err: %v", cidrSetReleases.Name, err)
	}
	m.allocTries, err = testutil.GetHistogramMetricValue(cidrSetAllocationTriesPerRequest.WithLabelValues(label))
	if err != nil {
		t.Errorf("failed to get %s value, err: %v", cidrSetAllocationTriesPerRequest.Name, err)
	}
	m.max, err = testutil.GetGaugeMetricValue(cidrSetMaxCidrs.WithLabelValues(label))
	if err != nil {
		t.Errorf("failed to get %s value, err: %v", cidrSetMaxCidrs.Name, err)
	}

	if m != em {
		t.Fatalf("metrics error: expected %v, received %v", em, m)
	}
}

// Benchmarks
func benchmarkAllocateAllIPv6(cidr string, subnetMaskSize int, b *testing.B) {
	_, clusterCIDR, _ := netutils.ParseCIDRSloppy(cidr)
	a, _ := NewCIDRSet(clusterCIDR, subnetMaskSize)
	for n := 0; n < b.N; n++ {
		// Allocate the whole range + 1
		for i := 0; i <= a.maxCIDRs; i++ {
			a.AllocateNext()
		}
		// Release all
		a.Release(clusterCIDR)
	}
}

func BenchmarkAllocateAll_48_52(b *testing.B) { benchmarkAllocateAllIPv6("2001:db8::/48", 52, b) }
func BenchmarkAllocateAll_48_56(b *testing.B) { benchmarkAllocateAllIPv6("2001:db8::/48", 56, b) }

func BenchmarkAllocateAll_48_60(b *testing.B) { benchmarkAllocateAllIPv6("2001:db8::/48", 60, b) }
func BenchmarkAllocateAll_48_64(b *testing.B) { benchmarkAllocateAllIPv6("2001:db8::/48", 64, b) }

func BenchmarkAllocateAll_64_68(b *testing.B) { benchmarkAllocateAllIPv6("2001:db8::/64", 68, b) }

func BenchmarkAllocateAll_64_72(b *testing.B) { benchmarkAllocateAllIPv6("2001:db8::/64", 72, b) }
func BenchmarkAllocateAll_64_76(b *testing.B) { benchmarkAllocateAllIPv6("2001:db8::/64", 76, b) }

func BenchmarkAllocateAll_64_80(b *testing.B) { benchmarkAllocateAllIPv6("2001:db8::/64", 80, b) }
