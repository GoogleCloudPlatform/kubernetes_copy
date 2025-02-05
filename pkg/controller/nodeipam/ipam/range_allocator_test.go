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

package ipam

import (
	"net"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/kubernetes/pkg/controller/nodeipam/ipam/test"
	"k8s.io/kubernetes/pkg/controller/testutil"
	"k8s.io/kubernetes/test/utils/ktesting"
	netutils "k8s.io/utils/net"
)

type testCase struct {
	description     string
	fakeNodeHandler *testutil.FakeNodeHandler
	allocatorParams CIDRAllocatorParams
	// key is index of the cidr allocated
	expectedAllocatedCIDR map[int]string
	allocatedCIDRs        map[int][]string
	// should controller creation fail?
	ctrlCreateFail bool
}

func TestOccupyPreExistingCIDR(t *testing.T) {
	// all tests operate on a single node
	testCases := []testCase{
		{
			description: "success, single stack no node allocation",
			fakeNodeHandler: &testutil.FakeNodeHandler{
				Existing: []*v1.Node{
					{
						ObjectMeta: metav1.ObjectMeta{
							Name: "node0",
						},
					},
				},
				Clientset: fake.NewSimpleClientset(),
			},
			allocatorParams: CIDRAllocatorParams{
				ClusterCIDRs: func() []*net.IPNet {
					_, clusterCIDRv4, _ := netutils.ParseCIDRSloppy("10.10.0.0/16")
					return []*net.IPNet{clusterCIDRv4}
				}(),
				ServiceCIDR:          nil,
				SecondaryServiceCIDR: nil,
				NodeCIDRMaskSizes:    []int{24},
			},
			allocatedCIDRs:        nil,
			expectedAllocatedCIDR: nil,
			ctrlCreateFail:        false,
		},
		{
			description: "success, dual stack no node allocation",
			fakeNodeHandler: &testutil.FakeNodeHandler{
				Existing: []*v1.Node{
					{
						ObjectMeta: metav1.ObjectMeta{
							Name: "node0",
						},
					},
				},
				Clientset: fake.NewSimpleClientset(),
			},
			allocatorParams: CIDRAllocatorParams{
				ClusterCIDRs: func() []*net.IPNet {
					_, clusterCIDRv4, _ := netutils.ParseCIDRSloppy("10.10.0.0/16")
					_, clusterCIDRv6, _ := netutils.ParseCIDRSloppy("ace:cab:deca::/8")
					return []*net.IPNet{clusterCIDRv4, clusterCIDRv6}
				}(),
				ServiceCIDR:          nil,
				SecondaryServiceCIDR: nil,
				NodeCIDRMaskSizes:    []int{24, 24},
			},
			allocatedCIDRs:        nil,
			expectedAllocatedCIDR: nil,
			ctrlCreateFail:        false,
		},
		{
			description: "success, single stack correct node allocation",
			fakeNodeHandler: &testutil.FakeNodeHandler{
				Existing: []*v1.Node{
					{
						ObjectMeta: metav1.ObjectMeta{
							Name: "node0",
						},
						Spec: v1.NodeSpec{
							PodCIDRs: []string{"10.10.0.1/24"},
						},
					},
				},
				Clientset: fake.NewSimpleClientset(),
			},
			allocatorParams: CIDRAllocatorParams{
				ClusterCIDRs: func() []*net.IPNet {
					_, clusterCIDRv4, _ := netutils.ParseCIDRSloppy("10.10.0.0/16")
					return []*net.IPNet{clusterCIDRv4}
				}(),
				ServiceCIDR:          nil,
				SecondaryServiceCIDR: nil,
				NodeCIDRMaskSizes:    []int{24},
			},
			allocatedCIDRs:        nil,
			expectedAllocatedCIDR: nil,
			ctrlCreateFail:        false,
		},
		{
			description: "success, dual stack both allocated correctly",
			fakeNodeHandler: &testutil.FakeNodeHandler{
				Existing: []*v1.Node{
					{
						ObjectMeta: metav1.ObjectMeta{
							Name: "node0",
						},
						Spec: v1.NodeSpec{
							PodCIDRs: []string{"10.10.0.1/24", "a00::/86"},
						},
					},
				},
				Clientset: fake.NewSimpleClientset(),
			},
			allocatorParams: CIDRAllocatorParams{
				ClusterCIDRs: func() []*net.IPNet {
					_, clusterCIDRv4, _ := netutils.ParseCIDRSloppy("10.10.0.0/16")
					_, clusterCIDRv6, _ := netutils.ParseCIDRSloppy("ace:cab:deca::/8")
					return []*net.IPNet{clusterCIDRv4, clusterCIDRv6}
				}(),
				ServiceCIDR:          nil,
				SecondaryServiceCIDR: nil,
				NodeCIDRMaskSizes:    []int{24, 24},
			},
			allocatedCIDRs:        nil,
			expectedAllocatedCIDR: nil,
			ctrlCreateFail:        false,
		},
		// failure cases
		{
			description: "fail, single stack incorrect node allocation",
			fakeNodeHandler: &testutil.FakeNodeHandler{
				Existing: []*v1.Node{
					{
						ObjectMeta: metav1.ObjectMeta{
							Name: "node0",
						},
						Spec: v1.NodeSpec{
							PodCIDRs: []string{"172.10.0.1/24"},
						},
					},
				},
				Clientset: fake.NewSimpleClientset(),
			},
			allocatorParams: CIDRAllocatorParams{
				ClusterCIDRs: func() []*net.IPNet {
					_, clusterCIDRv4, _ := netutils.ParseCIDRSloppy("10.10.0.0/16")
					return []*net.IPNet{clusterCIDRv4}
				}(),
				ServiceCIDR:          nil,
				SecondaryServiceCIDR: nil,
				NodeCIDRMaskSizes:    []int{24},
			},
			allocatedCIDRs:        nil,
			expectedAllocatedCIDR: nil,
			ctrlCreateFail:        true,
		},
		{
			description: "fail, dualstack node allocating from non existing cidr",

			fakeNodeHandler: &testutil.FakeNodeHandler{
				Existing: []*v1.Node{
					{
						ObjectMeta: metav1.ObjectMeta{
							Name: "node0",
						},
						Spec: v1.NodeSpec{
							PodCIDRs: []string{"10.10.0.1/24", "a00::/86"},
						},
					},
				},
				Clientset: fake.NewSimpleClientset(),
			},
			allocatorParams: CIDRAllocatorParams{
				ClusterCIDRs: func() []*net.IPNet {
					_, clusterCIDRv4, _ := netutils.ParseCIDRSloppy("10.10.0.0/16")
					return []*net.IPNet{clusterCIDRv4}
				}(),
				ServiceCIDR:          nil,
				SecondaryServiceCIDR: nil,
				NodeCIDRMaskSizes:    []int{24},
			},
			allocatedCIDRs:        nil,
			expectedAllocatedCIDR: nil,
			ctrlCreateFail:        true,
		},
		{
			description: "fail, dualstack node allocating bad v4",

			fakeNodeHandler: &testutil.FakeNodeHandler{
				Existing: []*v1.Node{
					{
						ObjectMeta: metav1.ObjectMeta{
							Name: "node0",
						},
						Spec: v1.NodeSpec{
							PodCIDRs: []string{"172.10.0.1/24", "a00::/86"},
						},
					},
				},
				Clientset: fake.NewSimpleClientset(),
			},
			allocatorParams: CIDRAllocatorParams{
				ClusterCIDRs: func() []*net.IPNet {
					_, clusterCIDRv4, _ := netutils.ParseCIDRSloppy("10.10.0.0/16")
					_, clusterCIDRv6, _ := netutils.ParseCIDRSloppy("ace:cab:deca::/8")
					return []*net.IPNet{clusterCIDRv4, clusterCIDRv6}
				}(),
				ServiceCIDR:          nil,
				SecondaryServiceCIDR: nil,
				NodeCIDRMaskSizes:    []int{24, 24},
			},
			allocatedCIDRs:        nil,
			expectedAllocatedCIDR: nil,
			ctrlCreateFail:        true,
		},
		{
			description: "fail, dualstack node allocating bad v6",

			fakeNodeHandler: &testutil.FakeNodeHandler{
				Existing: []*v1.Node{
					{
						ObjectMeta: metav1.ObjectMeta{
							Name: "node0",
						},
						Spec: v1.NodeSpec{
							PodCIDRs: []string{"10.10.0.1/24", "cdd::/86"},
						},
					},
				},
				Clientset: fake.NewSimpleClientset(),
			},
			allocatorParams: CIDRAllocatorParams{
				ClusterCIDRs: func() []*net.IPNet {
					_, clusterCIDRv4, _ := netutils.ParseCIDRSloppy("10.10.0.0/16")
					_, clusterCIDRv6, _ := netutils.ParseCIDRSloppy("ace:cab:deca::/8")
					return []*net.IPNet{clusterCIDRv4, clusterCIDRv6}
				}(),
				ServiceCIDR:          nil,
				SecondaryServiceCIDR: nil,
				NodeCIDRMaskSizes:    []int{24, 24},
			},
			allocatedCIDRs:        nil,
			expectedAllocatedCIDR: nil,
			ctrlCreateFail:        true,
		},
	}

	// test function
	tCtx := ktesting.Init(t)
	for _, tc := range testCases {
		t.Run(tc.description, func(t *testing.T) {
			// Initialize the range allocator.
			fakeNodeInformer := test.FakeNodeInformer(tc.fakeNodeHandler)
			nodeList, _ := tc.fakeNodeHandler.List(tCtx, metav1.ListOptions{})
			_, err := NewCIDRRangeAllocator(tCtx, tc.fakeNodeHandler, fakeNodeInformer, tc.allocatorParams, nodeList)
			if err == nil && tc.ctrlCreateFail {
				t.Fatalf("creating range allocator was expected to fail, but it did not")
			}
			if err != nil && !tc.ctrlCreateFail {
				t.Fatalf("creating range allocator was expected to succeed, but it did not")
			}
		})
	}
}

func TestAllocateOrOccupyCIDRSuccess(t *testing.T) {
	// Non-parallel test (overrides global var)
	oldNodePollInterval := nodePollInterval
	nodePollInterval = test.NodePollInterval
	defer func() {
		nodePollInterval = oldNodePollInterval
	}()

	// all tests operate on a single node
	testCases := []testCase{
		{
			description: "When there's no ServiceCIDR return first CIDR in range",
			fakeNodeHandler: &testutil.FakeNodeHandler{
				Existing: []*v1.Node{
					{
						ObjectMeta: metav1.ObjectMeta{
							Name: "node0",
						},
					},
				},
				Clientset: fake.NewSimpleClientset(),
			},
			allocatorParams: CIDRAllocatorParams{
				ClusterCIDRs: func() []*net.IPNet {
					_, clusterCIDR, _ := netutils.ParseCIDRSloppy("127.123.234.0/24")
					return []*net.IPNet{clusterCIDR}
				}(),
				ServiceCIDR:          nil,
				SecondaryServiceCIDR: nil,
				NodeCIDRMaskSizes:    []int{30},
			},
			expectedAllocatedCIDR: map[int]string{
				0: "127.123.234.0/30",
			},
		},
		{
			description: "Correctly filter out ServiceCIDR",
			fakeNodeHandler: &testutil.FakeNodeHandler{
				Existing: []*v1.Node{
					{
						ObjectMeta: metav1.ObjectMeta{
							Name: "node0",
						},
					},
				},
				Clientset: fake.NewSimpleClientset(),
			},
			allocatorParams: CIDRAllocatorParams{
				ClusterCIDRs: func() []*net.IPNet {
					_, clusterCIDR, _ := netutils.ParseCIDRSloppy("127.123.234.0/24")
					return []*net.IPNet{clusterCIDR}
				}(),
				ServiceCIDR: func() *net.IPNet {
					_, serviceCIDR, _ := netutils.ParseCIDRSloppy("127.123.234.0/26")
					return serviceCIDR
				}(),
				SecondaryServiceCIDR: nil,
				NodeCIDRMaskSizes:    []int{30},
			},
			// it should return first /30 CIDR after service range
			expectedAllocatedCIDR: map[int]string{
				0: "127.123.234.64/30",
			},
		},
		{
			description: "Correctly ignore already allocated CIDRs",
			fakeNodeHandler: &testutil.FakeNodeHandler{
				Existing: []*v1.Node{
					{
						ObjectMeta: metav1.ObjectMeta{
							Name: "node0",
						},
					},
				},
				Clientset: fake.NewSimpleClientset(),
			},
			allocatorParams: CIDRAllocatorParams{
				ClusterCIDRs: func() []*net.IPNet {
					_, clusterCIDR, _ := netutils.ParseCIDRSloppy("127.123.234.0/24")
					return []*net.IPNet{clusterCIDR}
				}(),
				ServiceCIDR: func() *net.IPNet {
					_, serviceCIDR, _ := netutils.ParseCIDRSloppy("127.123.234.0/26")
					return serviceCIDR
				}(),
				SecondaryServiceCIDR: nil,
				NodeCIDRMaskSizes:    []int{30},
			},
			allocatedCIDRs: map[int][]string{
				0: {"127.123.234.64/30", "127.123.234.68/30", "127.123.234.72/30", "127.123.234.80/30"},
			},
			expectedAllocatedCIDR: map[int]string{
				0: "127.123.234.76/30",
			},
		},
		{
			description: "Dualstack CIDRs v4,v6",
			fakeNodeHandler: &testutil.FakeNodeHandler{
				Existing: []*v1.Node{
					{
						ObjectMeta: metav1.ObjectMeta{
							Name: "node0",
						},
					},
				},
				Clientset: fake.NewSimpleClientset(),
			},
			allocatorParams: CIDRAllocatorParams{
				ClusterCIDRs: func() []*net.IPNet {
					_, clusterCIDRv4, _ := netutils.ParseCIDRSloppy("127.123.234.0/8")
					_, clusterCIDRv6, _ := netutils.ParseCIDRSloppy("ace:cab:deca::/84")
					return []*net.IPNet{clusterCIDRv4, clusterCIDRv6}
				}(),
				ServiceCIDR: func() *net.IPNet {
					_, serviceCIDR, _ := netutils.ParseCIDRSloppy("127.123.234.0/26")
					return serviceCIDR
				}(),
				SecondaryServiceCIDR: nil,
				NodeCIDRMaskSizes:    []int{24, 98},
			},
		},
		{
			description: "Dualstack CIDRs v6,v4",
			fakeNodeHandler: &testutil.FakeNodeHandler{
				Existing: []*v1.Node{
					{
						ObjectMeta: metav1.ObjectMeta{
							Name: "node0",
						},
					},
				},
				Clientset: fake.NewSimpleClientset(),
			},
			allocatorParams: CIDRAllocatorParams{
				ClusterCIDRs: func() []*net.IPNet {
					_, clusterCIDRv4, _ := netutils.ParseCIDRSloppy("127.123.234.0/8")
					_, clusterCIDRv6, _ := netutils.ParseCIDRSloppy("ace:cab:deca::/84")
					return []*net.IPNet{clusterCIDRv6, clusterCIDRv4}
				}(),
				ServiceCIDR: func() *net.IPNet {
					_, serviceCIDR, _ := netutils.ParseCIDRSloppy("127.123.234.0/26")
					return serviceCIDR
				}(),
				SecondaryServiceCIDR: nil,
				NodeCIDRMaskSizes:    []int{98, 24},
			},
		},
		{
			description: "Dualstack CIDRs, more than two",
			fakeNodeHandler: &testutil.FakeNodeHandler{
				Existing: []*v1.Node{
					{
						ObjectMeta: metav1.ObjectMeta{
							Name: "node0",
						},
					},
				},
				Clientset: fake.NewSimpleClientset(),
			},
			allocatorParams: CIDRAllocatorParams{
				ClusterCIDRs: func() []*net.IPNet {
					_, clusterCIDRv4, _ := netutils.ParseCIDRSloppy("127.123.234.0/8")
					_, clusterCIDRv6, _ := netutils.ParseCIDRSloppy("ace:cab:deca::/84")
					_, clusterCIDRv4_2, _ := netutils.ParseCIDRSloppy("10.0.0.0/8")
					return []*net.IPNet{clusterCIDRv4, clusterCIDRv6, clusterCIDRv4_2}
				}(),
				ServiceCIDR: func() *net.IPNet {
					_, serviceCIDR, _ := netutils.ParseCIDRSloppy("127.123.234.0/26")
					return serviceCIDR
				}(),
				SecondaryServiceCIDR: nil,
				NodeCIDRMaskSizes:    []int{24, 98, 24},
			},
		},
		{
			description: "no double counting",
			fakeNodeHandler: &testutil.FakeNodeHandler{
				Existing: []*v1.Node{
					{
						ObjectMeta: metav1.ObjectMeta{
							Name: "node0",
						},
						Spec: v1.NodeSpec{
							PodCIDRs: []string{"10.10.0.0/24"},
						},
					},
					{
						ObjectMeta: metav1.ObjectMeta{
							Name: "node1",
						},
						Spec: v1.NodeSpec{
							PodCIDRs: []string{"10.10.2.0/24"},
						},
					},
					{
						ObjectMeta: metav1.ObjectMeta{
							Name: "node2",
						},
					},
				},
				Clientset: fake.NewSimpleClientset(),
			},
			allocatorParams: CIDRAllocatorParams{
				ClusterCIDRs: func() []*net.IPNet {
					_, clusterCIDR, _ := netutils.ParseCIDRSloppy("10.10.0.0/22")
					return []*net.IPNet{clusterCIDR}
				}(),
				ServiceCIDR:          nil,
				SecondaryServiceCIDR: nil,
				NodeCIDRMaskSizes:    []int{24},
			},
			expectedAllocatedCIDR: map[int]string{
				0: "10.10.1.0/24",
			},
		},
	}

	// test function
	_, tCtx := ktesting.NewTestContext(t)
	testFunc := func(tc testCase) {
		fakeNodeInformer := test.FakeNodeInformer(tc.fakeNodeHandler)
		nodeList, _ := tc.fakeNodeHandler.List(tCtx, metav1.ListOptions{})
		// Initialize the range allocator.
		allocator, err := NewCIDRRangeAllocator(tCtx, tc.fakeNodeHandler, fakeNodeInformer, tc.allocatorParams, nodeList)
		if err != nil {
			t.Errorf("%v: failed to create CIDRRangeAllocator with error %v", tc.description, err)
			return
		}
		rangeAllocator, ok := allocator.(*rangeAllocator)
		if !ok {
			t.Logf("%v: found non-default implementation of CIDRAllocator, skipping white-box test...", tc.description)
			return
		}
		rangeAllocator.nodesSynced = test.AlwaysReady
		rangeAllocator.recorder = testutil.NewFakeRecorder()
		go allocator.Run(tCtx)

		// this is a bit of white box testing
		// pre allocate the cidrs as per the test
		for idx, allocatedList := range tc.allocatedCIDRs {
			for _, allocated := range allocatedList {
				_, cidr, err := netutils.ParseCIDRSloppy(allocated)
				if err != nil {
					t.Fatalf("%v: unexpected error when parsing CIDR %v: %v", tc.description, allocated, err)
				}
				if err = rangeAllocator.cidrSets[idx].Occupy(cidr); err != nil {
					t.Fatalf("%v: unexpected error when occupying CIDR %v: %v", tc.description, allocated, err)
				}
			}
		}

		updateCount := 0
		for _, node := range tc.fakeNodeHandler.Existing {
			if node.Spec.PodCIDRs == nil {
				updateCount++
			}
			if err := allocator.AllocateOrOccupyCIDR(tCtx, node); err != nil {
				t.Errorf("%v: unexpected error in AllocateOrOccupyCIDR: %v", tc.description, err)
			}
		}
		if updateCount != 1 {
			t.Fatalf("test error: all tests must update exactly one node")
		}
		if err := test.WaitForUpdatedNodeWithTimeout(tc.fakeNodeHandler, updateCount, wait.ForeverTestTimeout); err != nil {
			t.Fatalf("%v: timeout while waiting for Node update: %v", tc.description, err)
		}

		if len(tc.expectedAllocatedCIDR) == 0 {
			// nothing further expected
			return
		}
		for _, updatedNode := range tc.fakeNodeHandler.GetUpdatedNodesCopy() {
			if len(updatedNode.Spec.PodCIDRs) == 0 {
				continue // not assigned yet
			}
			// match
			for podCIDRIdx, expectedPodCIDR := range tc.expectedAllocatedCIDR {
				if updatedNode.Spec.PodCIDRs[podCIDRIdx] != expectedPodCIDR {
					t.Errorf("%v: Unable to find allocated CIDR %v, found updated Nodes with CIDRs: %v", tc.description, expectedPodCIDR, updatedNode.Spec.PodCIDRs)
					break
				}
			}
		}
	}

	// run the test cases
	for _, tc := range testCases {
		testFunc(tc)
	}
}

func TestAllocateOrOccupyCIDRFailure(t *testing.T) {
	testCases := []testCase{
		{
			description: "When there's no ServiceCIDR return first CIDR in range",
			fakeNodeHandler: &testutil.FakeNodeHandler{
				Existing: []*v1.Node{
					{
						ObjectMeta: metav1.ObjectMeta{
							Name: "node0",
						},
					},
				},
				Clientset: fake.NewSimpleClientset(),
			},
			allocatorParams: CIDRAllocatorParams{
				ClusterCIDRs: func() []*net.IPNet {
					_, clusterCIDR, _ := netutils.ParseCIDRSloppy("127.123.234.0/28")
					return []*net.IPNet{clusterCIDR}
				}(),
				ServiceCIDR:          nil,
				SecondaryServiceCIDR: nil,
				NodeCIDRMaskSizes:    []int{30},
			},
			allocatedCIDRs: map[int][]string{
				0: {"127.123.234.0/30", "127.123.234.4/30", "127.123.234.8/30", "127.123.234.12/30"},
			},
		},
	}
	_, tCtx := ktesting.NewTestContext(t)
	testFunc := func(tc testCase) {
		// Initialize the range allocator.
		allocator, err := NewCIDRRangeAllocator(tCtx, tc.fakeNodeHandler, test.FakeNodeInformer(tc.fakeNodeHandler), tc.allocatorParams, nil)
		if err != nil {
			t.Logf("%v: failed to create CIDRRangeAllocator with error %v", tc.description, err)
		}
		rangeAllocator, ok := allocator.(*rangeAllocator)
		if !ok {
			t.Logf("%v: found non-default implementation of CIDRAllocator, skipping white-box test...", tc.description)
			return
		}
		rangeAllocator.nodesSynced = test.AlwaysReady
		rangeAllocator.recorder = testutil.NewFakeRecorder()
		go allocator.Run(tCtx)

		// this is a bit of white box testing
		for setIdx, allocatedList := range tc.allocatedCIDRs {
			for _, allocated := range allocatedList {
				_, cidr, err := netutils.ParseCIDRSloppy(allocated)
				if err != nil {
					t.Fatalf("%v: unexpected error when parsing CIDR %v: %v", tc.description, cidr, err)
				}
				err = rangeAllocator.cidrSets[setIdx].Occupy(cidr)
				if err != nil {
					t.Fatalf("%v: unexpected error when occupying CIDR %v: %v", tc.description, cidr, err)
				}
			}
		}
		if err := allocator.AllocateOrOccupyCIDR(tCtx, tc.fakeNodeHandler.Existing[0]); err == nil {
			t.Errorf("%v: unexpected success in AllocateOrOccupyCIDR: %v", tc.description, err)
		}
		// We don't expect any updates, so just sleep for some time
		time.Sleep(time.Second)
		if len(tc.fakeNodeHandler.GetUpdatedNodesCopy()) != 0 {
			t.Fatalf("%v: unexpected update of nodes: %v", tc.description, tc.fakeNodeHandler.GetUpdatedNodesCopy())
		}
		if len(tc.expectedAllocatedCIDR) == 0 {
			// nothing further expected
			return
		}
		for _, updatedNode := range tc.fakeNodeHandler.GetUpdatedNodesCopy() {
			if len(updatedNode.Spec.PodCIDRs) == 0 {
				continue // not assigned yet
			}
			// match
			for podCIDRIdx, expectedPodCIDR := range tc.expectedAllocatedCIDR {
				if updatedNode.Spec.PodCIDRs[podCIDRIdx] == expectedPodCIDR {
					t.Errorf("%v: found cidr %v that should not be allocated on node with CIDRs:%v", tc.description, expectedPodCIDR, updatedNode.Spec.PodCIDRs)
					break
				}
			}
		}
	}
	for _, tc := range testCases {
		testFunc(tc)
	}
}

type releaseTestCase struct {
	description                      string
	fakeNodeHandler                  *testutil.FakeNodeHandler
	allocatorParams                  CIDRAllocatorParams
	expectedAllocatedCIDRFirstRound  map[int]string
	expectedAllocatedCIDRSecondRound map[int]string
	allocatedCIDRs                   map[int][]string
	cidrsToRelease                   [][]string
}

func TestReleaseCIDRSuccess(t *testing.T) {
	// Non-parallel test (overrides global var)
	oldNodePollInterval := nodePollInterval
	nodePollInterval = test.NodePollInterval
	defer func() {
		nodePollInterval = oldNodePollInterval
	}()

	testCases := []releaseTestCase{
		{
			description: "Correctly release preallocated CIDR",
			fakeNodeHandler: &testutil.FakeNodeHandler{
				Existing: []*v1.Node{
					{
						ObjectMeta: metav1.ObjectMeta{
							Name: "node0",
						},
					},
				},
				Clientset: fake.NewSimpleClientset(),
			},
			allocatorParams: CIDRAllocatorParams{
				ClusterCIDRs: func() []*net.IPNet {
					_, clusterCIDR, _ := netutils.ParseCIDRSloppy("127.123.234.0/28")
					return []*net.IPNet{clusterCIDR}
				}(),
				ServiceCIDR:          nil,
				SecondaryServiceCIDR: nil,
				NodeCIDRMaskSizes:    []int{30},
			},
			allocatedCIDRs: map[int][]string{
				0: {"127.123.234.0/30", "127.123.234.4/30", "127.123.234.8/30", "127.123.234.12/30"},
			},
			expectedAllocatedCIDRFirstRound: nil,
			cidrsToRelease: [][]string{
				{"127.123.234.4/30"},
			},
			expectedAllocatedCIDRSecondRound: map[int]string{
				0: "127.123.234.4/30",
			},
		},
		{
			description: "Correctly recycle CIDR",
			fakeNodeHandler: &testutil.FakeNodeHandler{
				Existing: []*v1.Node{
					{
						ObjectMeta: metav1.ObjectMeta{
							Name: "node0",
						},
					},
				},
				Clientset: fake.NewSimpleClientset(),
			},
			allocatorParams: CIDRAllocatorParams{
				ClusterCIDRs: func() []*net.IPNet {
					_, clusterCIDR, _ := netutils.ParseCIDRSloppy("127.123.234.0/28")
					return []*net.IPNet{clusterCIDR}
				}(),
				ServiceCIDR:          nil,
				SecondaryServiceCIDR: nil,
				NodeCIDRMaskSizes:    []int{30},
			},
			allocatedCIDRs: map[int][]string{
				0: {"127.123.234.4/30", "127.123.234.8/30", "127.123.234.12/30"},
			},
			expectedAllocatedCIDRFirstRound: map[int]string{
				0: "127.123.234.0/30",
			},
			cidrsToRelease: [][]string{
				{"127.123.234.0/30"},
			},
			expectedAllocatedCIDRSecondRound: map[int]string{
				0: "127.123.234.0/30",
			},
		},
	}
	logger, tCtx := ktesting.NewTestContext(t)
	testFunc := func(tc releaseTestCase) {
		// Initialize the range allocator.
		allocator, _ := NewCIDRRangeAllocator(tCtx, tc.fakeNodeHandler, test.FakeNodeInformer(tc.fakeNodeHandler), tc.allocatorParams, nil)
		rangeAllocator, ok := allocator.(*rangeAllocator)
		if !ok {
			t.Logf("%v: found non-default implementation of CIDRAllocator, skipping white-box test...", tc.description)
			return
		}
		rangeAllocator.nodesSynced = test.AlwaysReady
		rangeAllocator.recorder = testutil.NewFakeRecorder()
		go allocator.Run(tCtx)

		// this is a bit of white box testing
		for setIdx, allocatedList := range tc.allocatedCIDRs {
			for _, allocated := range allocatedList {
				_, cidr, err := netutils.ParseCIDRSloppy(allocated)
				if err != nil {
					t.Fatalf("%v: unexpected error when parsing CIDR %v: %v", tc.description, allocated, err)
				}
				err = rangeAllocator.cidrSets[setIdx].Occupy(cidr)
				if err != nil {
					t.Fatalf("%v: unexpected error when occupying CIDR %v: %v", tc.description, allocated, err)
				}
			}
		}

		err := allocator.AllocateOrOccupyCIDR(tCtx, tc.fakeNodeHandler.Existing[0])
		if len(tc.expectedAllocatedCIDRFirstRound) != 0 {
			if err != nil {
				t.Fatalf("%v: unexpected error in AllocateOrOccupyCIDR: %v", tc.description, err)
			}
			if err := test.WaitForUpdatedNodeWithTimeout(tc.fakeNodeHandler, 1, wait.ForeverTestTimeout); err != nil {
				t.Fatalf("%v: timeout while waiting for Node update: %v", tc.description, err)
			}
		} else {
			if err == nil {
				t.Fatalf("%v: unexpected success in AllocateOrOccupyCIDR: %v", tc.description, err)
			}
			// We don't expect any updates here
			time.Sleep(time.Second)
			if len(tc.fakeNodeHandler.GetUpdatedNodesCopy()) != 0 {
				t.Fatalf("%v: unexpected update of nodes: %v", tc.description, tc.fakeNodeHandler.GetUpdatedNodesCopy())
			}
		}
		for _, cidrToRelease := range tc.cidrsToRelease {
			nodeToRelease := v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "node0",
				},
			}
			nodeToRelease.Spec.PodCIDRs = cidrToRelease
			err = allocator.ReleaseCIDR(logger, &nodeToRelease)
			if err != nil {
				t.Fatalf("%v: unexpected error in ReleaseCIDR: %v", tc.description, err)
			}
		}
		if err = allocator.AllocateOrOccupyCIDR(tCtx, tc.fakeNodeHandler.Existing[0]); err != nil {
			t.Fatalf("%v: unexpected error in AllocateOrOccupyCIDR: %v", tc.description, err)
		}
		if err := test.WaitForUpdatedNodeWithTimeout(tc.fakeNodeHandler, 1, wait.ForeverTestTimeout); err != nil {
			t.Fatalf("%v: timeout while waiting for Node update: %v", tc.description, err)
		}

		if len(tc.expectedAllocatedCIDRSecondRound) == 0 {
			// nothing further expected
			return
		}
		for _, updatedNode := range tc.fakeNodeHandler.GetUpdatedNodesCopy() {
			if len(updatedNode.Spec.PodCIDRs) == 0 {
				continue // not assigned yet
			}
			// match
			for podCIDRIdx, expectedPodCIDR := range tc.expectedAllocatedCIDRSecondRound {
				if updatedNode.Spec.PodCIDRs[podCIDRIdx] != expectedPodCIDR {
					t.Errorf("%v: found cidr %v that should not be allocated on node with CIDRs:%v", tc.description, expectedPodCIDR, updatedNode.Spec.PodCIDRs)
					break
				}
			}
		}
	}

	for _, tc := range testCases {
		testFunc(tc)
	}
}
func TestNodeDeletionReleaseCIDR(t *testing.T) {
	_, clusterCIDRv4, err := netutils.ParseCIDRSloppy("10.10.0.0/16")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	_, allocatedCIDR, err := netutils.ParseCIDRSloppy("10.10.0.0/24")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	testCases := []struct {
		description       string
		nodeKey           string
		existingNodes     []*v1.Node
		shouldReleaseCIDR bool
	}{
		{
			description: "Regular node not under deletion",
			nodeKey:     "node0",
			existingNodes: []*v1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "node0",
					},
					Spec: v1.NodeSpec{
						PodCIDR:  allocatedCIDR.String(),
						PodCIDRs: []string{allocatedCIDR.String()}},
				},
			},
			shouldReleaseCIDR: false,
		},
		{
			description: "Node under deletion",
			nodeKey:     "node0",
			existingNodes: []*v1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:              "node0",
						DeletionTimestamp: &metav1.Time{Time: time.Now()},
					},
					Spec: v1.NodeSpec{
						PodCIDR:  allocatedCIDR.String(),
						PodCIDRs: []string{allocatedCIDR.String()}},
				},
			},
			shouldReleaseCIDR: false,
		},
		{
			description:       "Node deleted",
			nodeKey:           "node0",
			existingNodes:     []*v1.Node{},
			shouldReleaseCIDR: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.description, func(t *testing.T) {
			allocatorParams := CIDRAllocatorParams{
				ClusterCIDRs: []*net.IPNet{clusterCIDRv4}, ServiceCIDR: nil,
				SecondaryServiceCIDR: nil,
				NodeCIDRMaskSizes:    []int{24},
			}

			fakeNodeHandler := &testutil.FakeNodeHandler{
				Existing:  tc.existingNodes,
				Clientset: fake.NewSimpleClientset(),
			}
			_, tCtx := ktesting.NewTestContext(t)

			fakeNodeInformer := test.FakeNodeInformer(fakeNodeHandler)
			nodeList, err := fakeNodeHandler.List(tCtx, metav1.ListOptions{})
			if err != nil {
				t.Fatalf("Failed to get list of nodes %v", err)
			}
			allocator, err := NewCIDRRangeAllocator(tCtx, fakeNodeHandler, fakeNodeInformer, allocatorParams, nodeList)
			if err != nil {
				t.Fatalf("failed to create CIDRRangeAllocator: %v", err)
			}
			rangeAllocator, ok := allocator.(*rangeAllocator)
			if !ok {
				t.Fatalf("found non-default implementation of CIDRAllocator")
			}
			rangeAllocator.nodesSynced = test.AlwaysReady
			rangeAllocator.recorder = testutil.NewFakeRecorder()

			if err := rangeAllocator.syncNode(tCtx, tc.nodeKey); err != nil {
				t.Fatalf("failed to run rangeAllocator.syncNode")
			}

			if len(rangeAllocator.cidrSets) != 1 {
				t.Fatalf("Expected a single cidrSet, but got %d", len(rangeAllocator.cidrSets))
			}

			// if the allocated CIDR was released we expect the nextAllocated CIDR to be the same
			nextCIDR, err := rangeAllocator.cidrSets[0].AllocateNext()
			if err != nil {
				t.Fatalf("unexpected error trying to allocate next CIDR: %v", err)
			}
			expectedCIDR := "10.10.1.0/24" // existing allocated is 10.0.0.0/24
			if tc.shouldReleaseCIDR {
				expectedCIDR = allocatedCIDR.String() // if cidr was released we expect to reuse it. ie: 10.0.0.0/24
			}
			if nextCIDR.String() != expectedCIDR {
				t.Fatalf("Expected CIDR %s to be allocated next, but got: %v", expectedCIDR, nextCIDR.String())
			}
		})
	}
}
