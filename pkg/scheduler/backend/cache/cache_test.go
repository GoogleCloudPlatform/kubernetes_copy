/*
Copyright 2015 The Kubernetes Authors.

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

package cache

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
	"k8s.io/klog/v2/ktesting"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/metrics"
	st "k8s.io/kubernetes/pkg/scheduler/testing"
	schedutil "k8s.io/kubernetes/pkg/scheduler/util"
)

var nodeInfoCmpOpts = []cmp.Option{
	cmp.AllowUnexported(framework.NodeInfo{}),
	cmpopts.IgnoreUnexported(framework.PodInfo{}),
}

func deepEqualWithoutGeneration(actual *nodeInfoListItem, expected *framework.NodeInfo) error {
	if (actual == nil) != (expected == nil) {
		return errors.New("one of the actual or expected is nil and the other is not")
	}
	// Ignore generation field.
	if actual != nil {
		actual.info.Generation = 0
	}
	if expected != nil {
		expected.Generation = 0
	}
	if actual != nil {
		if diff := cmp.Diff(expected, actual.info, nodeInfoCmpOpts...); diff != "" {
			return fmt.Errorf("Unexpected node info (-want,+got):\n%s", diff)
		}
	}
	return nil
}

type hostPortInfoParam struct {
	protocol, ip string
	port         int32
}

type hostPortInfoBuilder struct {
	inputs []hostPortInfoParam
}

func newHostPortInfoBuilder() *hostPortInfoBuilder {
	return &hostPortInfoBuilder{}
}

func (b *hostPortInfoBuilder) add(protocol, ip string, port int32) *hostPortInfoBuilder {
	b.inputs = append(b.inputs, hostPortInfoParam{protocol, ip, port})
	return b
}

func (b *hostPortInfoBuilder) build() framework.HostPortInfo {
	res := make(framework.HostPortInfo)
	for _, param := range b.inputs {
		res.Add(param.ip, param.protocol, param.port)
	}
	return res
}

func newNodeInfo(requestedResource *framework.Resource,
	nonzeroRequest *framework.Resource,
	pods []*v1.Pod,
	usedPorts framework.HostPortInfo,
	imageStates map[string]*framework.ImageStateSummary,
) *framework.NodeInfo {
	nodeInfo := framework.NewNodeInfo(pods...)
	nodeInfo.Requested = requestedResource
	nodeInfo.NonZeroRequested = nonzeroRequest
	nodeInfo.UsedPorts = usedPorts
	nodeInfo.ImageStates = imageStates
	return nodeInfo
}

// TestAssumePodScheduled tests that after a pod is assumed, its information is aggregated
// on node level.
func TestAssumePodScheduled(t *testing.T) {
	nodeName := "node"
	testPods := []*v1.Pod{
		makeBasePod(t, nodeName, "test-resource-request-and-port-0", "100m", "500", "", []v1.ContainerPort{{HostIP: "127.0.0.1", HostPort: 80, Protocol: "TCP"}}),
		makeBasePod(t, nodeName, "test-resource-request-and-port-1", "100m", "500", "", []v1.ContainerPort{{HostIP: "127.0.0.1", HostPort: 80, Protocol: "TCP"}}),
		makeBasePod(t, nodeName, "test-resource-request-and-port-2", "200m", "1Ki", "", []v1.ContainerPort{{HostIP: "127.0.0.1", HostPort: 8080, Protocol: "TCP"}}),
		makeBasePod(t, nodeName, "test-nonzero-request", "", "", "", []v1.ContainerPort{{HostIP: "127.0.0.1", HostPort: 80, Protocol: "TCP"}}),
		makeBasePod(t, nodeName, "test-extended-resource-1", "100m", "500", "example.com/foo:3", []v1.ContainerPort{{HostIP: "127.0.0.1", HostPort: 80, Protocol: "TCP"}}),
		makeBasePod(t, nodeName, "test-extended-resource-2", "200m", "1Ki", "example.com/foo:5", []v1.ContainerPort{{HostIP: "127.0.0.1", HostPort: 8080, Protocol: "TCP"}}),
		makeBasePod(t, nodeName, "test-extended-key", "100m", "500", "random-invalid-extended-key:100", []v1.ContainerPort{{}}),
	}

	tests := []struct {
		name string
		pods []*v1.Pod

		wNodeInfo *framework.NodeInfo
	}{{
		name: "assumed one pod with resource request and used ports",
		pods: []*v1.Pod{testPods[0]},
		wNodeInfo: newNodeInfo(
			&framework.Resource{
				MilliCPU: 100,
				Memory:   500,
			},
			&framework.Resource{
				MilliCPU: 100,
				Memory:   500,
			},
			[]*v1.Pod{testPods[0]},
			newHostPortInfoBuilder().add("TCP", "127.0.0.1", 80).build(),
			make(map[string]*framework.ImageStateSummary),
		),
	}, {
		name: "node requested resource are equal to the sum of the assumed pods requested resource, node contains host ports defined by pods",
		pods: []*v1.Pod{testPods[1], testPods[2]},
		wNodeInfo: newNodeInfo(
			&framework.Resource{
				MilliCPU: 300,
				Memory:   1524,
			},
			&framework.Resource{
				MilliCPU: 300,
				Memory:   1524,
			},
			[]*v1.Pod{testPods[1], testPods[2]},
			newHostPortInfoBuilder().add("TCP", "127.0.0.1", 80).add("TCP", "127.0.0.1", 8080).build(),
			make(map[string]*framework.ImageStateSummary),
		),
	}, { // test non-zero request
		name: "assumed pod without resource request",
		pods: []*v1.Pod{testPods[3]},
		wNodeInfo: newNodeInfo(
			&framework.Resource{
				MilliCPU: 0,
				Memory:   0,
			},
			&framework.Resource{
				MilliCPU: schedutil.DefaultMilliCPURequest,
				Memory:   schedutil.DefaultMemoryRequest,
			},
			[]*v1.Pod{testPods[3]},
			newHostPortInfoBuilder().add("TCP", "127.0.0.1", 80).build(),
			make(map[string]*framework.ImageStateSummary),
		),
	}, {
		name: "assumed one pod with extended resource",
		pods: []*v1.Pod{testPods[4]},
		wNodeInfo: newNodeInfo(
			&framework.Resource{
				MilliCPU:        100,
				Memory:          500,
				ScalarResources: map[v1.ResourceName]int64{"example.com/foo": 3},
			},
			&framework.Resource{
				MilliCPU: 100,
				Memory:   500,
			},
			[]*v1.Pod{testPods[4]},
			newHostPortInfoBuilder().add("TCP", "127.0.0.1", 80).build(),
			make(map[string]*framework.ImageStateSummary),
		),
	}, {
		name: "assumed two pods with extended resources",
		pods: []*v1.Pod{testPods[4], testPods[5]},
		wNodeInfo: newNodeInfo(
			&framework.Resource{
				MilliCPU:        300,
				Memory:          1524,
				ScalarResources: map[v1.ResourceName]int64{"example.com/foo": 8},
			},
			&framework.Resource{
				MilliCPU: 300,
				Memory:   1524,
			},
			[]*v1.Pod{testPods[4], testPods[5]},
			newHostPortInfoBuilder().add("TCP", "127.0.0.1", 80).add("TCP", "127.0.0.1", 8080).build(),
			make(map[string]*framework.ImageStateSummary),
		),
	}, {
		name: "assumed pod with random invalid extended resource key",
		pods: []*v1.Pod{testPods[6]},
		wNodeInfo: newNodeInfo(
			&framework.Resource{
				MilliCPU: 100,
				Memory:   500,
			},
			&framework.Resource{
				MilliCPU: 100,
				Memory:   500,
			},
			[]*v1.Pod{testPods[6]},
			newHostPortInfoBuilder().build(),
			make(map[string]*framework.ImageStateSummary),
		),
	},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			logger, ctx := ktesting.NewTestContext(t)
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			cache := newCache(ctx, time.Second, time.Second)
			for _, pod := range tc.pods {
				if err := cache.AssumePod(logger, pod); err != nil {
					t.Fatalf("AssumePod failed: %v", err)
				}
				// pod already in cache so can't be assumed
				if err := cache.AssumePod(logger, pod); err == nil {
					t.Error("expected error, no error found")
				}
			}
			n := cache.nodes[nodeName]
			if err := deepEqualWithoutGeneration(n, tc.wNodeInfo); err != nil {
				t.Error(err)
			}

			for _, pod := range tc.pods {
				if err := cache.ForgetPod(logger, pod); err != nil {
					t.Fatalf("ForgetPod failed: %v", err)
				}
				if err := isForgottenFromCache(pod, cache); err != nil {
					t.Errorf("pod %s: %v", pod.Name, err)
				}
			}
		})
	}
}

type testExpirePodStruct struct {
	pod         *v1.Pod
	finishBind  bool
	assumedTime time.Time
}

func assumeAndFinishBinding(logger klog.Logger, cache *cacheImpl, pod *v1.Pod, assumedTime time.Time) error {
	if err := cache.AssumePod(logger, pod); err != nil {
		return err
	}
	return cache.finishBinding(logger, pod, assumedTime)
}

// TestExpirePod tests that assumed pods will be removed if expired.
// The removal will be reflected in node info.
func TestExpirePod(t *testing.T) {
	metrics.Register()
	nodeName := "node"
	testPods := []*v1.Pod{
		makeBasePod(t, nodeName, "test-1", "100m", "500", "", []v1.ContainerPort{{HostIP: "127.0.0.1", HostPort: 80, Protocol: "TCP"}}),
		makeBasePod(t, nodeName, "test-2", "200m", "1Ki", "", []v1.ContainerPort{{HostIP: "127.0.0.1", HostPort: 8080, Protocol: "TCP"}}),
		makeBasePod(t, nodeName, "test-3", "200m", "1Ki", "", []v1.ContainerPort{{HostIP: "127.0.0.1", HostPort: 8080, Protocol: "TCP"}}),
	}
	now := time.Now()
	defaultTTL := 10 * time.Second
	tests := []struct {
		name        string
		pods        []*testExpirePodStruct
		cleanupTime time.Time
		ttl         time.Duration
		wNodeInfo   *framework.NodeInfo
	}{
		{
			name: "assumed pod would expire",
			pods: []*testExpirePodStruct{
				{pod: testPods[0], finishBind: true, assumedTime: now},
			},
			cleanupTime: now.Add(2 * defaultTTL),
			wNodeInfo:   nil,
			ttl:         defaultTTL,
		},
		{
			name: "first one would expire, second and third would not",
			pods: []*testExpirePodStruct{
				{pod: testPods[0], finishBind: true, assumedTime: now},
				{pod: testPods[1], finishBind: true, assumedTime: now.Add(3 * defaultTTL / 2)},
				{pod: testPods[2]},
			},
			cleanupTime: now.Add(2 * defaultTTL),
			wNodeInfo: newNodeInfo(
				&framework.Resource{
					MilliCPU: 400,
					Memory:   2048,
				},
				&framework.Resource{
					MilliCPU: 400,
					Memory:   2048,
				},
				// Order gets altered when removing pods.
				[]*v1.Pod{testPods[2], testPods[1]},
				newHostPortInfoBuilder().add("TCP", "127.0.0.1", 8080).build(),
				make(map[string]*framework.ImageStateSummary),
			),
			ttl: defaultTTL,
		},
		{
			name: "assumed pod would never expire",
			pods: []*testExpirePodStruct{
				{pod: testPods[0], finishBind: true, assumedTime: now},
			},
			cleanupTime: now.Add(3 * defaultTTL),
			wNodeInfo: newNodeInfo(
				&framework.Resource{
					MilliCPU: 100,
					Memory:   500,
				},
				&framework.Resource{
					MilliCPU: 100,
					Memory:   500,
				},
				[]*v1.Pod{testPods[0]},
				newHostPortInfoBuilder().add("TCP", "127.0.0.1", 80).build(),
				make(map[string]*framework.ImageStateSummary),
			),
			ttl: time.Duration(0),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			logger, ctx := ktesting.NewTestContext(t)
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			cache := newCache(ctx, tc.ttl, time.Second)

			for _, pod := range tc.pods {
				if err := cache.AssumePod(logger, pod.pod); err != nil {
					t.Fatal(err)
				}
				if !pod.finishBind {
					continue
				}
				if err := cache.finishBinding(logger, pod.pod, pod.assumedTime); err != nil {
					t.Fatal(err)
				}
			}
			// pods that got bound and have assumedTime + ttl < cleanupTime will get
			// expired and removed
			cache.cleanupAssumedPods(logger, tc.cleanupTime)
			n := cache.nodes[nodeName]
			if err := deepEqualWithoutGeneration(n, tc.wNodeInfo); err != nil {
				t.Error(err)
			}
		})
	}
}

// TestAddPodWillConfirm tests that a pod being Add()ed will be confirmed if assumed.
// The pod info should still exist after manually expiring unconfirmed pods.
func TestAddPodWillConfirm(t *testing.T) {
	nodeName := "node"
	now := time.Now()
	ttl := 10 * time.Second

	testPods := []*v1.Pod{
		makeBasePod(t, nodeName, "test-1", "100m", "500", "", []v1.ContainerPort{{HostIP: "127.0.0.1", HostPort: 80, Protocol: "TCP"}}),
		makeBasePod(t, nodeName, "test-2", "200m", "1Ki", "", []v1.ContainerPort{{HostIP: "127.0.0.1", HostPort: 8080, Protocol: "TCP"}}),
	}
	test := struct {
		podsToAssume []*v1.Pod
		podsToAdd    []*v1.Pod

		wNodeInfo *framework.NodeInfo
	}{ // two pod were assumed at same time. But first one is called Add() and gets confirmed.
		podsToAssume: []*v1.Pod{testPods[0], testPods[1]},
		podsToAdd:    []*v1.Pod{testPods[0]},
		wNodeInfo: newNodeInfo(
			&framework.Resource{
				MilliCPU: 100,
				Memory:   500,
			},
			&framework.Resource{
				MilliCPU: 100,
				Memory:   500,
			},
			[]*v1.Pod{testPods[0]},
			newHostPortInfoBuilder().add("TCP", "127.0.0.1", 80).build(),
			make(map[string]*framework.ImageStateSummary),
		),
	}

	logger, ctx := ktesting.NewTestContext(t)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cache := newCache(ctx, ttl, time.Second)
	for _, podToAssume := range test.podsToAssume {
		if err := assumeAndFinishBinding(logger, cache, podToAssume, now); err != nil {
			t.Fatalf("assumePod failed: %v", err)
		}
	}
	for _, podToAdd := range test.podsToAdd {
		if err := cache.AddPod(logger, podToAdd); err != nil {
			t.Fatalf("AddPod failed: %v", err)
		}
		// pod already in added state
		if err := cache.AddPod(logger, podToAdd); err == nil {
			t.Error("expected error, no error found")
		}
	}
	cache.cleanupAssumedPods(logger, now.Add(2*ttl))
	// check after expiration. confirmed pods shouldn't be expired.
	n := cache.nodes[nodeName]
	if err := deepEqualWithoutGeneration(n, test.wNodeInfo); err != nil {
		t.Error(err)
	}
}

func TestDump(t *testing.T) {
	nodeName := "node"
	now := time.Now()
	ttl := 10 * time.Second

	testPods := []*v1.Pod{
		makeBasePod(t, nodeName, "test-1", "100m", "500", "", []v1.ContainerPort{{HostIP: "127.0.0.1", HostPort: 80, Protocol: "TCP"}}),
		makeBasePod(t, nodeName, "test-2", "200m", "1Ki", "", []v1.ContainerPort{{HostIP: "127.0.0.1", HostPort: 80, Protocol: "TCP"}}),
	}
	test := struct {
		podsToAssume []*v1.Pod
		podsToAdd    []*v1.Pod
	}{ // two pod were assumed at same time. But first one is called Add() and gets confirmed.
		podsToAssume: []*v1.Pod{testPods[0], testPods[1]},
		podsToAdd:    []*v1.Pod{testPods[0]},
	}

	logger, ctx := ktesting.NewTestContext(t)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cache := newCache(ctx, ttl, time.Second)
	for _, podToAssume := range test.podsToAssume {
		if err := assumeAndFinishBinding(logger, cache, podToAssume, now); err != nil {
			t.Errorf("assumePod failed: %v", err)
		}
	}
	for _, podToAdd := range test.podsToAdd {
		if err := cache.AddPod(logger, podToAdd); err != nil {
			t.Errorf("AddPod failed: %v", err)
		}
	}

	snapshot := cache.Dump()
	if len(snapshot.Nodes) != len(cache.nodes) {
		t.Errorf("Unequal number of nodes in the cache and its snapshot. expected: %v, got: %v", len(cache.nodes), len(snapshot.Nodes))
	}
	for name, ni := range snapshot.Nodes {
		nItem := cache.nodes[name]
		if diff := cmp.Diff(nItem.info, ni, nodeInfoCmpOpts...); diff != "" {
			t.Errorf("Unexpected node info (-want,+got):\n%s", diff)
		}
	}
	if diff := cmp.Diff(cache.assumedPods, snapshot.AssumedPods); diff != "" {
		t.Errorf("Unexpected assumedPods (-want,+got):\n%s", diff)
	}

}

// TestAddPodAlwaysUpdatePodInfoInNodeInfo tests that AddPod method always updates PodInfo in NodeInfo,
// even when the Pod is assumed one.
func TestAddPodAlwaysUpdatesPodInfoInNodeInfo(t *testing.T) {
	ttl := 10 * time.Second
	logger, ctx := ktesting.NewTestContext(t)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	now := time.Now()
	p1 := makeBasePod(t, "node1", "test-1", "100m", "500", "", []v1.ContainerPort{{HostPort: 80}})

	p2 := p1.DeepCopy()
	p2.Status.Conditions = append(p1.Status.Conditions, v1.PodCondition{
		Type:   v1.PodScheduled,
		Status: v1.ConditionTrue,
	})

	test := struct {
		podsToAssume         []*v1.Pod
		podsToAddAfterAssume []*v1.Pod
		nodeInfo             map[string]*framework.NodeInfo
	}{
		podsToAssume:         []*v1.Pod{p1},
		podsToAddAfterAssume: []*v1.Pod{p2},
		nodeInfo: map[string]*framework.NodeInfo{
			"node1": newNodeInfo(
				&framework.Resource{
					MilliCPU: 100,
					Memory:   500,
				},
				&framework.Resource{
					MilliCPU: 100,
					Memory:   500,
				},
				[]*v1.Pod{p2},
				newHostPortInfoBuilder().add("TCP", "0.0.0.0", 80).build(),
				make(map[string]*framework.ImageStateSummary),
			),
		},
	}

	cache := newCache(ctx, ttl, time.Second)
	for _, podToAssume := range test.podsToAssume {
		if err := assumeAndFinishBinding(logger, cache, podToAssume, now); err != nil {
			t.Fatalf("assumePod failed: %v", err)
		}
	}
	for _, podToAdd := range test.podsToAddAfterAssume {
		if err := cache.AddPod(logger, podToAdd); err != nil {
			t.Fatalf("AddPod failed: %v", err)
		}
	}
	for nodeName, expected := range test.nodeInfo {
		n := cache.nodes[nodeName]
		if err := deepEqualWithoutGeneration(n, expected); err != nil {
			t.Errorf("node %q: %v", nodeName, err)
		}
	}
}

// TestAddPodWillReplaceAssumed tests that a pod being Add()ed will replace any assumed pod.
func TestAddPodWillReplaceAssumed(t *testing.T) {
	now := time.Now()
	ttl := 10 * time.Second

	assumedPod := makeBasePod(t, "assumed-node-1", "test-1", "100m", "500", "", []v1.ContainerPort{{HostPort: 80}})
	addedPod := makeBasePod(t, "actual-node", "test-1", "100m", "500", "", []v1.ContainerPort{{HostPort: 80}})
	updatedPod := makeBasePod(t, "actual-node", "test-1", "200m", "500", "", []v1.ContainerPort{{HostPort: 90}})

	test := struct {
		podsToAssume []*v1.Pod
		podsToAdd    []*v1.Pod
		podsToUpdate [][]*v1.Pod

		wNodeInfo map[string]*framework.NodeInfo
	}{
		podsToAssume: []*v1.Pod{assumedPod.DeepCopy()},
		podsToAdd:    []*v1.Pod{addedPod.DeepCopy()},
		podsToUpdate: [][]*v1.Pod{{addedPod.DeepCopy(), updatedPod.DeepCopy()}},
		wNodeInfo: map[string]*framework.NodeInfo{
			"assumed-node": nil,
			"actual-node": newNodeInfo(
				&framework.Resource{
					MilliCPU: 200,
					Memory:   500,
				},
				&framework.Resource{
					MilliCPU: 200,
					Memory:   500,
				},
				[]*v1.Pod{updatedPod.DeepCopy()},
				newHostPortInfoBuilder().add("TCP", "0.0.0.0", 90).build(),
				make(map[string]*framework.ImageStateSummary),
			),
		},
	}

	logger, ctx := ktesting.NewTestContext(t)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cache := newCache(ctx, ttl, time.Second)
	for _, podToAssume := range test.podsToAssume {
		if err := assumeAndFinishBinding(logger, cache, podToAssume, now); err != nil {
			t.Fatalf("assumePod failed: %v", err)
		}
	}
	for _, podToAdd := range test.podsToAdd {
		if err := cache.AddPod(logger, podToAdd); err != nil {
			t.Fatalf("AddPod failed: %v", err)
		}
	}
	for _, podToUpdate := range test.podsToUpdate {
		if err := cache.UpdatePod(logger, podToUpdate[0], podToUpdate[1]); err != nil {
			t.Fatalf("UpdatePod failed: %v", err)
		}
	}
	for nodeName, expected := range test.wNodeInfo {
		n := cache.nodes[nodeName]
		if err := deepEqualWithoutGeneration(n, expected); err != nil {
			t.Errorf("node %q: %v", nodeName, err)
		}
	}
}

// TestAddPodAfterExpiration tests that a pod being Add()ed will be added back if expired.
func TestAddPodAfterExpiration(t *testing.T) {
	nodeName := "node"
	ttl := 10 * time.Second
	basePod := makeBasePod(t, nodeName, "test", "100m", "500", "", []v1.ContainerPort{{HostIP: "127.0.0.1", HostPort: 80, Protocol: "TCP"}})
	test := struct {
		pod       *v1.Pod
		wNodeInfo *framework.NodeInfo
	}{
		pod: basePod,
		wNodeInfo: newNodeInfo(
			&framework.Resource{
				MilliCPU: 100,
				Memory:   500,
			},
			&framework.Resource{
				MilliCPU: 100,
				Memory:   500,
			},
			[]*v1.Pod{basePod},
			newHostPortInfoBuilder().add("TCP", "127.0.0.1", 80).build(),
			make(map[string]*framework.ImageStateSummary),
		),
	}

	logger, ctx := ktesting.NewTestContext(t)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	now := time.Now()
	cache := newCache(ctx, ttl, time.Second)
	if err := assumeAndFinishBinding(logger, cache, test.pod, now); err != nil {
		t.Fatalf("assumePod failed: %v", err)
	}
	cache.cleanupAssumedPods(logger, now.Add(2*ttl))
	// It should be expired and removed.
	if err := isForgottenFromCache(test.pod, cache); err != nil {
		t.Error(err)
	}
	if err := cache.AddPod(logger, test.pod); err != nil {
		t.Fatalf("AddPod failed: %v", err)
	}
	// check after expiration. confirmed pods shouldn't be expired.
	n := cache.nodes[nodeName]
	if err := deepEqualWithoutGeneration(n, test.wNodeInfo); err != nil {
		t.Error(err)
	}
}

// TestUpdatePod tests that a pod will be updated if added before.
func TestUpdatePod(t *testing.T) {
	nodeName := "node"
	ttl := 10 * time.Second
	testPods := []*v1.Pod{
		makeBasePod(t, nodeName, "test", "100m", "500", "", []v1.ContainerPort{{HostIP: "127.0.0.1", HostPort: 80, Protocol: "TCP"}}),
		makeBasePod(t, nodeName, "test", "200m", "1Ki", "", []v1.ContainerPort{{HostIP: "127.0.0.1", HostPort: 8080, Protocol: "TCP"}}),
	}
	test := struct {
		podsToAdd    []*v1.Pod
		podsToUpdate []*v1.Pod

		wNodeInfo []*framework.NodeInfo
	}{ // add a pod and then update it twice
		podsToAdd:    []*v1.Pod{testPods[0]},
		podsToUpdate: []*v1.Pod{testPods[0], testPods[1], testPods[0]},
		wNodeInfo: []*framework.NodeInfo{newNodeInfo(
			&framework.Resource{
				MilliCPU: 200,
				Memory:   1024,
			},
			&framework.Resource{
				MilliCPU: 200,
				Memory:   1024,
			},
			[]*v1.Pod{testPods[1]},
			newHostPortInfoBuilder().add("TCP", "127.0.0.1", 8080).build(),
			make(map[string]*framework.ImageStateSummary),
		), newNodeInfo(
			&framework.Resource{
				MilliCPU: 100,
				Memory:   500,
			},
			&framework.Resource{
				MilliCPU: 100,
				Memory:   500,
			},
			[]*v1.Pod{testPods[0]},
			newHostPortInfoBuilder().add("TCP", "127.0.0.1", 80).build(),
			make(map[string]*framework.ImageStateSummary),
		)},
	}

	logger, ctx := ktesting.NewTestContext(t)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cache := newCache(ctx, ttl, time.Second)
	for _, podToAdd := range test.podsToAdd {
		if err := cache.AddPod(logger, podToAdd); err != nil {
			t.Fatalf("AddPod failed: %v", err)
		}
	}

	for j := range test.podsToUpdate {
		if j == 0 {
			continue
		}
		if err := cache.UpdatePod(logger, test.podsToUpdate[j-1], test.podsToUpdate[j]); err != nil {
			t.Fatalf("UpdatePod failed: %v", err)
		}
		// check after expiration. confirmed pods shouldn't be expired.
		n := cache.nodes[nodeName]
		if err := deepEqualWithoutGeneration(n, test.wNodeInfo[j-1]); err != nil {
			t.Errorf("update %d: %v", j, err)
		}
	}
}

// TestUpdatePodAndGet tests get always return latest pod state
func TestUpdatePodAndGet(t *testing.T) {
	nodeName := "node"
	ttl := 10 * time.Second
	testPods := []*v1.Pod{
		makeBasePod(t, nodeName, "test", "100m", "500", "", []v1.ContainerPort{{HostIP: "127.0.0.1", HostPort: 80, Protocol: "TCP"}}),
		makeBasePod(t, nodeName, "test", "200m", "1Ki", "", []v1.ContainerPort{{HostIP: "127.0.0.1", HostPort: 8080, Protocol: "TCP"}}),
	}
	tests := []struct {
		name        string
		pod         *v1.Pod
		podToUpdate *v1.Pod
		handler     func(logger klog.Logger, cache Cache, pod *v1.Pod) error
		assumePod   bool
	}{
		{
			name:        "do not update pod when pod information has not changed",
			pod:         testPods[0],
			podToUpdate: testPods[0],
			handler: func(logger klog.Logger, cache Cache, pod *v1.Pod) error {
				return cache.AssumePod(logger, pod)
			},
			assumePod: true,
		},
		{
			name:        "update  pod when pod information changed",
			pod:         testPods[0],
			podToUpdate: testPods[1],
			handler: func(logger klog.Logger, cache Cache, pod *v1.Pod) error {
				return cache.AddPod(logger, pod)
			},
			assumePod: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			logger, ctx := ktesting.NewTestContext(t)
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			cache := newCache(ctx, ttl, time.Second)
			// trying to get an unknown pod should return an error
			// podToUpdate has not been added yet
			if _, err := cache.GetPod(tc.podToUpdate); err == nil {
				t.Error("expected error, no error found")
			}

			// trying to update an unknown pod should return an error
			// pod has not been added yet
			if err := cache.UpdatePod(logger, tc.pod, tc.podToUpdate); err == nil {
				t.Error("expected error, no error found")
			}

			if err := tc.handler(logger, cache, tc.pod); err != nil {
				t.Fatalf("unexpected err: %v", err)
			}

			if !tc.assumePod {
				if err := cache.UpdatePod(logger, tc.pod, tc.podToUpdate); err != nil {
					t.Fatalf("UpdatePod failed: %v", err)
				}
			}

			cachedPod, err := cache.GetPod(tc.pod)
			if err != nil {
				t.Fatalf("GetPod failed: %v", err)
			}
			if diff := cmp.Diff(tc.podToUpdate, cachedPod); diff != "" {
				t.Fatalf("Unexpected pod (-want, +got):\n%s", diff)
			}
		})
	}
}

// TestExpireAddUpdatePod test the sequence that a pod is expired, added, then updated
func TestExpireAddUpdatePod(t *testing.T) {
	nodeName := "node"
	ttl := 10 * time.Second
	testPods := []*v1.Pod{
		makeBasePod(t, nodeName, "test", "100m", "500", "", []v1.ContainerPort{{HostIP: "127.0.0.1", HostPort: 80, Protocol: "TCP"}}),
		makeBasePod(t, nodeName, "test", "200m", "1Ki", "", []v1.ContainerPort{{HostIP: "127.0.0.1", HostPort: 8080, Protocol: "TCP"}}),
	}
	test := struct {
		podsToAssume []*v1.Pod
		podsToAdd    []*v1.Pod
		podsToUpdate []*v1.Pod

		wNodeInfo []*framework.NodeInfo
	}{ // Pod is assumed, expired, and added. Then it would be updated twice.
		podsToAssume: []*v1.Pod{testPods[0]},
		podsToAdd:    []*v1.Pod{testPods[0]},
		podsToUpdate: []*v1.Pod{testPods[0], testPods[1], testPods[0]},
		wNodeInfo: []*framework.NodeInfo{newNodeInfo(
			&framework.Resource{
				MilliCPU: 200,
				Memory:   1024,
			},
			&framework.Resource{
				MilliCPU: 200,
				Memory:   1024,
			},
			[]*v1.Pod{testPods[1]},
			newHostPortInfoBuilder().add("TCP", "127.0.0.1", 8080).build(),
			make(map[string]*framework.ImageStateSummary),
		), newNodeInfo(
			&framework.Resource{
				MilliCPU: 100,
				Memory:   500,
			},
			&framework.Resource{
				MilliCPU: 100,
				Memory:   500,
			},
			[]*v1.Pod{testPods[0]},
			newHostPortInfoBuilder().add("TCP", "127.0.0.1", 80).build(),
			make(map[string]*framework.ImageStateSummary),
		)},
	}

	logger, ctx := ktesting.NewTestContext(t)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	now := time.Now()
	cache := newCache(ctx, ttl, time.Second)
	for _, podToAssume := range test.podsToAssume {
		if err := assumeAndFinishBinding(logger, cache, podToAssume, now); err != nil {
			t.Fatalf("assumePod failed: %v", err)
		}
	}
	cache.cleanupAssumedPods(logger, now.Add(2*ttl))

	for _, podToAdd := range test.podsToAdd {
		if err := cache.AddPod(logger, podToAdd); err != nil {
			t.Fatalf("AddPod failed: %v", err)
		}
	}

	for j := range test.podsToUpdate {
		if j == 0 {
			continue
		}
		if err := cache.UpdatePod(logger, test.podsToUpdate[j-1], test.podsToUpdate[j]); err != nil {
			t.Fatalf("UpdatePod failed: %v", err)
		}
		// check after expiration. confirmed pods shouldn't be expired.
		n := cache.nodes[nodeName]
		if err := deepEqualWithoutGeneration(n, test.wNodeInfo[j-1]); err != nil {
			t.Errorf("update %d: %v", j, err)
		}
	}
}

func makePodWithEphemeralStorage(nodeName, ephemeralStorage string) *v1.Pod {
	return st.MakePod().Name("pod-with-ephemeral-storage").Namespace("default-namespace").UID("pod-with-ephemeral-storage").Req(
		map[v1.ResourceName]string{
			v1.ResourceEphemeralStorage: ephemeralStorage,
		},
	).Node(nodeName).Obj()
}

func TestEphemeralStorageResource(t *testing.T) {
	nodeName := "node"
	podE := makePodWithEphemeralStorage(nodeName, "500")
	test := struct {
		pod       *v1.Pod
		wNodeInfo *framework.NodeInfo
	}{
		pod: podE,
		wNodeInfo: newNodeInfo(
			&framework.Resource{
				EphemeralStorage: 500,
			},
			&framework.Resource{
				MilliCPU: schedutil.DefaultMilliCPURequest,
				Memory:   schedutil.DefaultMemoryRequest,
			},
			[]*v1.Pod{podE},
			framework.HostPortInfo{},
			make(map[string]*framework.ImageStateSummary),
		),
	}
	logger, ctx := ktesting.NewTestContext(t)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cache := newCache(ctx, time.Second, time.Second)
	if err := cache.AddPod(logger, test.pod); err != nil {
		t.Fatalf("AddPod failed: %v", err)
	}
	n := cache.nodes[nodeName]
	if err := deepEqualWithoutGeneration(n, test.wNodeInfo); err != nil {
		t.Error(err)
	}

	if err := cache.RemovePod(logger, test.pod); err != nil {
		t.Fatalf("RemovePod failed: %v", err)
	}
	if _, err := cache.GetPod(test.pod); err == nil {
		t.Errorf("pod was not deleted")
	}
}

// TestRemovePod tests after added pod is removed, its information should also be subtracted.
func TestRemovePod(t *testing.T) {
	pod := makeBasePod(t, "node-1", "test", "100m", "500", "", []v1.ContainerPort{{HostIP: "127.0.0.1", HostPort: 80, Protocol: "TCP"}})
	nodes := []*v1.Node{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "node-1"},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "node-2"},
		},
	}
	wNodeInfo := newNodeInfo(
		&framework.Resource{
			MilliCPU: 100,
			Memory:   500,
		},
		&framework.Resource{
			MilliCPU: 100,
			Memory:   500,
		},
		[]*v1.Pod{pod},
		newHostPortInfoBuilder().add("TCP", "127.0.0.1", 80).build(),
		make(map[string]*framework.ImageStateSummary),
	)
	tests := map[string]struct {
		assume bool
	}{
		"bound":   {},
		"assumed": {assume: true},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			logger, ctx := ktesting.NewTestContext(t)
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			nodeName := pod.Spec.NodeName
			cache := newCache(ctx, time.Second, time.Second)
			// Add/Assume pod succeeds even before adding the nodes.
			if tt.assume {
				if err := cache.AddPod(logger, pod); err != nil {
					t.Fatalf("AddPod failed: %v", err)
				}
			} else {
				if err := cache.AssumePod(logger, pod); err != nil {
					t.Fatalf("AssumePod failed: %v", err)
				}
			}
			n := cache.nodes[nodeName]
			if err := deepEqualWithoutGeneration(n, wNodeInfo); err != nil {
				t.Error(err)
			}
			for _, n := range nodes {
				cache.AddNode(logger, n)
			}

			if err := cache.RemovePod(logger, pod); err != nil {
				t.Fatalf("RemovePod failed: %v", err)
			}

			if _, err := cache.GetPod(pod); err == nil {
				t.Errorf("pod was not deleted")
			}

			// trying to remove a pod already removed should return an error
			if err := cache.RemovePod(logger, pod); err == nil {
				t.Error("expected error, no error found")
			}

			// Node that owned the Pod should be at the head of the list.
			if cache.headNode.info.Node().Name != nodeName {
				t.Errorf("node %q is not at the head of the list", nodeName)
			}
		})
	}
}

func TestForgetPod(t *testing.T) {
	nodeName := "node"
	basePod := makeBasePod(t, nodeName, "test", "100m", "500", "", []v1.ContainerPort{{HostIP: "127.0.0.1", HostPort: 80, Protocol: "TCP"}})
	pods := []*v1.Pod{basePod}
	now := time.Now()
	ttl := 10 * time.Second
	logger, ctx := ktesting.NewTestContext(t)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	cache := newCache(ctx, ttl, time.Second)
	for _, pod := range pods {
		if err := assumeAndFinishBinding(logger, cache, pod, now); err != nil {
			t.Fatalf("assumePod failed: %v", err)
		}
		isAssumed, err := cache.IsAssumedPod(pod)
		if err != nil {
			t.Fatalf("IsAssumedPod failed: %v.", err)
		}
		if !isAssumed {
			t.Fatalf("Pod is expected to be assumed.")
		}
		assumedPod, err := cache.GetPod(pod)
		if err != nil {
			t.Fatalf("GetPod failed: %v.", err)
		}
		if assumedPod.Namespace != pod.Namespace {
			t.Errorf("assumedPod.Namespace != pod.Namespace (%s != %s)", assumedPod.Namespace, pod.Namespace)
		}
		if assumedPod.Name != pod.Name {
			t.Errorf("assumedPod.Name != pod.Name (%s != %s)", assumedPod.Name, pod.Name)
		}
	}
	for _, pod := range pods {
		if err := cache.ForgetPod(logger, pod); err != nil {
			t.Fatalf("ForgetPod failed: %v", err)
		}
		if err := isForgottenFromCache(pod, cache); err != nil {
			t.Errorf("pod %q: %v", pod.Name, err)
		}
		// trying to forget a pod already forgotten should return an error
		if err := cache.ForgetPod(logger, pod); err == nil {
			t.Error("expected error, no error found")
		}
	}
}

// buildNodeInfo creates a NodeInfo by simulating node operations in cache.
func buildNodeInfo(node *v1.Node, pods []*v1.Pod, imageStates map[string]*framework.ImageStateSummary) *framework.NodeInfo {
	expected := framework.NewNodeInfo()
	expected.SetNode(node)
	expected.Allocatable = framework.NewResource(node.Status.Allocatable)
	expected.Generation++
	for _, pod := range pods {
		expected.AddPod(pod)
	}
	for _, image := range node.Status.Images {
		for _, name := range image.Names {
			if state, ok := imageStates[name]; ok {
				expected.ImageStates[name] = state
			}
		}
	}
	return expected
}

// buildImageStates creates ImageStateSummary of image from nodes that will be added in cache.
func buildImageStates(nodes []*v1.Node) map[string]*framework.ImageStateSummary {
	imageStates := make(map[string]*framework.ImageStateSummary)
	for _, item := range nodes {
		for _, image := range item.Status.Images {
			for _, name := range image.Names {
				if state, ok := imageStates[name]; !ok {
					state = &framework.ImageStateSummary{
						Size:  image.SizeBytes,
						Nodes: sets.New[string](item.Name),
					}
					imageStates[name] = state
				} else {
					state.Nodes.Insert(item.Name)
				}
			}
		}
	}
	return imageStates
}

// TestNodeOperators tests node operations of cache, including add, update
// and remove.
func TestNodeOperators(t *testing.T) {
	// Test data
	cpuHalf := resource.MustParse("500m")
	mem50m := resource.MustParse("50m")
	resourceList1 := map[v1.ResourceName]string{
		v1.ResourceCPU:                     "1000m",
		v1.ResourceMemory:                  "100m",
		v1.ResourceName("example.com/foo"): "1",
	}
	resourceList2 := map[v1.ResourceName]string{
		v1.ResourceCPU:                     "500m",
		v1.ResourceMemory:                  "50m",
		v1.ResourceName("example.com/foo"): "2",
	}
	taints := []v1.Taint{
		{
			Key:    "test-key",
			Value:  "test-value",
			Effect: v1.TaintEffectPreferNoSchedule,
		},
	}
	imageStatus1 := map[string]int64{
		"gcr.io/80:latest":  80 * mb,
		"gcr.io/80:v1":      80 * mb,
		"gcr.io/300:latest": 300 * mb,
		"gcr.io/300:v1":     300 * mb,
	}
	imageStatus2 := map[string]int64{
		"gcr.io/600:latest": 600 * mb,
		"gcr.io/80:latest":  80 * mb,
		"gcr.io/900:latest": 900 * mb,
	}
	tests := []struct {
		name  string
		nodes []*v1.Node
		pods  []*v1.Pod
	}{
		{
			name: "operate the node with one pod",
			nodes: []*v1.Node{
				&st.MakeNode().Name("test-node-1").Capacity(resourceList1).Taints(taints).Images(imageStatus1).Node,
				&st.MakeNode().Name("test-node-2").Capacity(resourceList2).Taints(taints).Images(imageStatus2).Node,
				&st.MakeNode().Name("test-node-3").Capacity(resourceList1).Taints(taints).Images(imageStatus1).Node,
				&st.MakeNode().Name("test-node-4").Capacity(resourceList2).Taints(taints).Images(imageStatus2).Node,
			},
			pods: []*v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "pod1",
						UID:  types.UID("pod1"),
					},
					Spec: v1.PodSpec{
						NodeName: "test-node-1",
						Containers: []v1.Container{
							{
								Resources: v1.ResourceRequirements{
									Requests: v1.ResourceList{
										v1.ResourceCPU:    cpuHalf,
										v1.ResourceMemory: mem50m,
									},
								},
								Ports: []v1.ContainerPort{
									{
										Name:          "http",
										HostPort:      80,
										ContainerPort: 80,
									},
								},
							},
						},
					},
				},
			},
		},
		{
			name: "operate the node with two pods",
			nodes: []*v1.Node{
				&st.MakeNode().Name("test-node-1").Capacity(resourceList1).Taints(taints).Images(imageStatus1).Node,
				&st.MakeNode().Name("test-node-2").Capacity(resourceList2).Taints(taints).Images(imageStatus2).Node,
				&st.MakeNode().Name("test-node-3").Capacity(resourceList1).Taints(taints).Images(imageStatus1).Node,
			},
			pods: []*v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "pod1",
						UID:  types.UID("pod1"),
					},
					Spec: v1.PodSpec{
						NodeName: "test-node-1",
						Containers: []v1.Container{
							{
								Resources: v1.ResourceRequirements{
									Requests: v1.ResourceList{
										v1.ResourceCPU:    cpuHalf,
										v1.ResourceMemory: mem50m,
									},
								},
							},
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "pod2",
						UID:  types.UID("pod2"),
					},
					Spec: v1.PodSpec{
						NodeName: "test-node-1",
						Containers: []v1.Container{
							{
								Resources: v1.ResourceRequirements{
									Requests: v1.ResourceList{
										v1.ResourceCPU:    cpuHalf,
										v1.ResourceMemory: mem50m,
									},
								},
							},
						},
					},
				},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			logger, ctx := ktesting.NewTestContext(t)
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			node := tc.nodes[0]

			imageStates := buildImageStates(tc.nodes)
			expected := buildNodeInfo(node, tc.pods, imageStates)

			cache := newCache(ctx, time.Second, time.Second)
			for _, nodeItem := range tc.nodes {
				cache.AddNode(logger, nodeItem)
			}
			for _, pod := range tc.pods {
				if err := cache.AddPod(logger, pod); err != nil {
					t.Fatal(err)
				}
			}
			nodes := map[string]*framework.NodeInfo{}
			for nodeItem := cache.headNode; nodeItem != nil; nodeItem = nodeItem.next {
				nodes[nodeItem.info.Node().Name] = nodeItem.info
			}

			// Step 1: the node was added into cache successfully.
			got, found := cache.nodes[node.Name]
			if !found {
				t.Errorf("Failed to find node %v in internalcache.", node.Name)
			}
			nodesList, err := cache.nodeTree.list()
			if err != nil {
				t.Fatal(err)
			}
			if cache.nodeTree.numNodes != len(tc.nodes) || len(nodesList) != len(tc.nodes) {
				t.Errorf("cache.nodeTree is not updated correctly after adding node got: %d, expected: %d",
					cache.nodeTree.numNodes, len(tc.nodes))
			}

			// Generations are globally unique. We check in our unit tests that they are incremented correctly.
			expected.Generation = got.info.Generation
			if diff := cmp.Diff(expected, got.info, nodeInfoCmpOpts...); diff != "" {
				t.Errorf("Failed to add node into scheduler cache (-want,+got):\n%s", diff)
			}

			// check imageState of NodeInfo with specific image when node added
			if !checkImageStateSummary(nodes, "gcr.io/80:latest", "gcr.io/300:latest") {
				t.Error("image have different ImageStateSummary")
			}

			// Step 2: dump cached nodes successfully.
			cachedNodes := NewEmptySnapshot()
			if err := cache.UpdateSnapshot(logger, cachedNodes); err != nil {
				t.Error(err)
			}
			newNode, found := cachedNodes.nodeInfoMap[node.Name]
			if !found || len(cachedNodes.nodeInfoMap) != len(tc.nodes) {
				t.Errorf("failed to dump cached nodes:\n got: %v \nexpected: %v", cachedNodes.nodeInfoMap, tc.nodes)
			}
			expected.Generation = newNode.Generation
			if diff := cmp.Diff(expected.Snapshot(), newNode, nodeInfoCmpOpts...); diff != "" {
				t.Errorf("Failed to clone node (-want,+got):\n%s", diff)
			}
			// check imageState of NodeInfo with specific image when update snapshot
			if !checkImageStateSummary(cachedNodes.nodeInfoMap, "gcr.io/80:latest", "gcr.io/300:latest") {
				t.Error("image have different ImageStateSummary")
			}

			// Step 3: update node attribute successfully.
			node.Status.Allocatable[v1.ResourceMemory] = mem50m
			expected.Allocatable.Memory = mem50m.Value()

			cache.UpdateNode(logger, nil, node)
			got, found = cache.nodes[node.Name]
			if !found {
				t.Errorf("Failed to find node %v in schedulertypes after UpdateNode.", node.Name)
			}
			if got.info.Generation <= expected.Generation {
				t.Errorf("Generation is not incremented. got: %v, expected: %v", got.info.Generation, expected.Generation)
			}
			expected.Generation = got.info.Generation

			if diff := cmp.Diff(expected, got.info, nodeInfoCmpOpts...); diff != "" {
				t.Errorf("Unexpected schedulertypes after updating node (-want, +got):\n%s", diff)
			}
			// check imageState of NodeInfo with specific image when update node
			if !checkImageStateSummary(nodes, "gcr.io/80:latest", "gcr.io/300:latest") {
				t.Error("image have different ImageStateSummary")
			}
			// Check nodeTree after update
			nodesList, err = cache.nodeTree.list()
			if err != nil {
				t.Fatal(err)
			}
			if cache.nodeTree.numNodes != len(tc.nodes) || len(nodesList) != len(tc.nodes) {
				t.Errorf("unexpected cache.nodeTree after updating node")
			}

			// Step 4: the node can be removed even if it still has pods.
			if err := cache.RemoveNode(logger, node); err != nil {
				t.Error(err)
			}
			if n, err := cache.getNodeInfo(node.Name); err != nil {
				t.Errorf("The node %v should still have a ghost entry: %v", node.Name, err)
			} else if n != nil {
				t.Errorf("The node object for %v should be nil", node.Name)
			}

			// trying to remove a node already removed should return an error
			if err := cache.RemoveNode(logger, node); err == nil {
				t.Error("expected error, no error found")
			}

			// Check node is removed from nodeTree as well.
			nodesList, err = cache.nodeTree.list()
			if err != nil {
				t.Fatal(err)
			}
			if cache.nodeTree.numNodes != len(tc.nodes)-1 || len(nodesList) != len(tc.nodes)-1 {
				t.Errorf("unexpected cache.nodeTree after removing node: %v", node.Name)
			}
			// check imageState of NodeInfo with specific image when delete node
			if !checkImageStateSummary(nodes, "gcr.io/80:latest", "gcr.io/300:latest") {
				t.Error("image have different ImageStateSummary after removing node")
			}
			// Pods are still in the pods cache.
			for _, p := range tc.pods {
				if _, err := cache.GetPod(p); err != nil {
					t.Error(err)
				}
			}

			// Step 5: removing pods for the removed node still succeeds.
			for _, p := range tc.pods {
				if err := cache.RemovePod(logger, p); err != nil {
					t.Error(err)
				}
				if _, err := cache.GetPod(p); err == nil {
					t.Errorf("pod %q still in cache", p.Name)
				}
			}
		})
	}
}

func TestSchedulerCache_UpdateSnapshot(t *testing.T) {
	logger, _ := ktesting.NewTestContext(t)

	// Create a few nodes to be used in tests.
	var nodes []*v1.Node
	for i := 0; i < 10; i++ {
		node := &v1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: fmt.Sprintf("test-node%v", i),
			},
			Status: v1.NodeStatus{
				Allocatable: v1.ResourceList{
					v1.ResourceCPU:    resource.MustParse("1000m"),
					v1.ResourceMemory: resource.MustParse("100m"),
				},
			},
		}
		nodes = append(nodes, node)
	}
	// Create a few nodes as updated versions of the above nodes
	var updatedNodes []*v1.Node
	for _, n := range nodes {
		updatedNode := n.DeepCopy()
		updatedNode.Status.Allocatable = v1.ResourceList{
			v1.ResourceCPU:    resource.MustParse("2000m"),
			v1.ResourceMemory: resource.MustParse("500m"),
		}
		updatedNodes = append(updatedNodes, updatedNode)
	}

	// Create a few pods for tests.
	var pods []*v1.Pod
	for i := 0; i < 20; i++ {
		pod := st.MakePod().Name(fmt.Sprintf("test-pod%v", i)).Namespace("test-ns").UID(fmt.Sprintf("test-puid%v", i)).
			Node(fmt.Sprintf("test-node%v", i%10)).Obj()
		pods = append(pods, pod)
	}

	// Create a few pods as updated versions of the above pods.
	var updatedPods []*v1.Pod
	for _, p := range pods {
		updatedPod := p.DeepCopy()
		priority := int32(1000)
		updatedPod.Spec.Priority = &priority
		updatedPods = append(updatedPods, updatedPod)
	}

	// Add a couple of pods with affinity, on the first and seconds nodes.
	var podsWithAffinity []*v1.Pod
	for i := 0; i < 2; i++ {
		pod := st.MakePod().Name(fmt.Sprintf("p-affinity-%v", i)).Namespace("test-ns").UID(fmt.Sprintf("puid-affinity-%v", i)).
			PodAffinityExists("foo", "", st.PodAffinityWithRequiredReq).Node(fmt.Sprintf("test-node%v", i)).Obj()
		podsWithAffinity = append(podsWithAffinity, pod)
	}

	// Add a few of pods with PVC
	var podsWithPVC []*v1.Pod
	for i := 0; i < 8; i++ {
		pod := st.MakePod().Name(fmt.Sprintf("p-pvc-%v", i)).Namespace("test-ns").UID(fmt.Sprintf("puid-pvc-%v", i)).
			PVC(fmt.Sprintf("test-pvc%v", i%4)).Node(fmt.Sprintf("test-node%v", i%2)).Obj()
		podsWithPVC = append(podsWithPVC, pod)
	}

	var cache *cacheImpl
	var snapshot *Snapshot
	type operation = func(t *testing.T)

	addNode := func(i int) operation {
		return func(t *testing.T) {
			cache.AddNode(logger, nodes[i])
		}
	}
	removeNode := func(i int) operation {
		return func(t *testing.T) {
			if err := cache.RemoveNode(logger, nodes[i]); err != nil {
				t.Error(err)
			}
		}
	}
	updateNode := func(i int) operation {
		return func(t *testing.T) {
			cache.UpdateNode(logger, nodes[i], updatedNodes[i])
		}
	}
	addPod := func(i int) operation {
		return func(t *testing.T) {
			if err := cache.AddPod(logger, pods[i]); err != nil {
				t.Error(err)
			}
		}
	}
	addPodWithAffinity := func(i int) operation {
		return func(t *testing.T) {
			if err := cache.AddPod(logger, podsWithAffinity[i]); err != nil {
				t.Error(err)
			}
		}
	}
	addPodWithPVC := func(i int) operation {
		return func(t *testing.T) {
			if err := cache.AddPod(logger, podsWithPVC[i]); err != nil {
				t.Error(err)
			}
		}
	}
	removePod := func(i int) operation {
		return func(t *testing.T) {
			if err := cache.RemovePod(logger, pods[i]); err != nil {
				t.Error(err)
			}
		}
	}
	removePodWithAffinity := func(i int) operation {
		return func(t *testing.T) {
			if err := cache.RemovePod(logger, podsWithAffinity[i]); err != nil {
				t.Error(err)
			}
		}
	}
	removePodWithPVC := func(i int) operation {
		return func(t *testing.T) {
			if err := cache.RemovePod(logger, podsWithPVC[i]); err != nil {
				t.Error(err)
			}
		}
	}
	updatePod := func(i int) operation {
		return func(t *testing.T) {
			if err := cache.UpdatePod(logger, pods[i], updatedPods[i]); err != nil {
				t.Error(err)
			}
		}
	}
	updateSnapshot := func() operation {
		return func(t *testing.T) {
			cache.UpdateSnapshot(logger, snapshot)
			if err := compareCacheWithNodeInfoSnapshot(t, cache, snapshot); err != nil {
				t.Error(err)
			}
		}
	}

	tests := []struct {
		name                         string
		operations                   []operation
		expected                     []*v1.Node
		expectedHavePodsWithAffinity int
		expectedUsedPVCSet           sets.Set[string]
	}{
		{
			name:               "Empty cache",
			operations:         []operation{},
			expected:           []*v1.Node{},
			expectedUsedPVCSet: sets.New[string](),
		},
		{
			name:               "Single node",
			operations:         []operation{addNode(1)},
			expected:           []*v1.Node{nodes[1]},
			expectedUsedPVCSet: sets.New[string](),
		},
		{
			name: "Add node, remove it, add it again",
			operations: []operation{
				addNode(1), updateSnapshot(), removeNode(1), addNode(1),
			},
			expected:           []*v1.Node{nodes[1]},
			expectedUsedPVCSet: sets.New[string](),
		},
		{
			name: "Add node and remove it in the same cycle, add it again",
			operations: []operation{
				addNode(1), updateSnapshot(), addNode(2), removeNode(1),
			},
			expected:           []*v1.Node{nodes[2]},
			expectedUsedPVCSet: sets.New[string](),
		},
		{
			name: "Add a few nodes, and snapshot in the middle",
			operations: []operation{
				addNode(0), updateSnapshot(), addNode(1), updateSnapshot(), addNode(2),
				updateSnapshot(), addNode(3),
			},
			expected:           []*v1.Node{nodes[3], nodes[2], nodes[1], nodes[0]},
			expectedUsedPVCSet: sets.New[string](),
		},
		{
			name: "Add a few nodes, and snapshot in the end",
			operations: []operation{
				addNode(0), addNode(2), addNode(5), addNode(6),
			},
			expected:           []*v1.Node{nodes[6], nodes[5], nodes[2], nodes[0]},
			expectedUsedPVCSet: sets.New[string](),
		},
		{
			name: "Update some nodes",
			operations: []operation{
				addNode(0), addNode(1), addNode(5), updateSnapshot(), updateNode(1),
			},
			expected:           []*v1.Node{nodes[1], nodes[5], nodes[0]},
			expectedUsedPVCSet: sets.New[string](),
		},
		{
			name: "Add a few nodes, and remove all of them",
			operations: []operation{
				addNode(0), addNode(2), addNode(5), addNode(6), updateSnapshot(),
				removeNode(0), removeNode(2), removeNode(5), removeNode(6),
			},
			expected:           []*v1.Node{},
			expectedUsedPVCSet: sets.New[string](),
		},
		{
			name: "Add a few nodes, and remove some of them",
			operations: []operation{
				addNode(0), addNode(2), addNode(5), addNode(6), updateSnapshot(),
				removeNode(0), removeNode(6),
			},
			expected:           []*v1.Node{nodes[5], nodes[2]},
			expectedUsedPVCSet: sets.New[string](),
		},
		{
			name: "Add a few nodes, remove all of them, and add more",
			operations: []operation{
				addNode(2), addNode(5), addNode(6), updateSnapshot(),
				removeNode(2), removeNode(5), removeNode(6), updateSnapshot(),
				addNode(7), addNode(9),
			},
			expected:           []*v1.Node{nodes[9], nodes[7]},
			expectedUsedPVCSet: sets.New[string](),
		},
		{
			name: "Update nodes in particular order",
			operations: []operation{
				addNode(8), updateNode(2), updateNode(8), updateSnapshot(),
				addNode(1),
			},
			expected:           []*v1.Node{nodes[1], nodes[8], nodes[2]},
			expectedUsedPVCSet: sets.New[string](),
		},
		{
			name: "Add some nodes and some pods",
			operations: []operation{
				addNode(0), addNode(2), addNode(8), updateSnapshot(),
				addPod(8), addPod(2),
			},
			expected:           []*v1.Node{nodes[2], nodes[8], nodes[0]},
			expectedUsedPVCSet: sets.New[string](),
		},
		{
			name: "Updating a pod moves its node to the head",
			operations: []operation{
				addNode(0), addPod(0), addNode(2), addNode(4), updatePod(0),
			},
			expected:           []*v1.Node{nodes[0], nodes[4], nodes[2]},
			expectedUsedPVCSet: sets.New[string](),
		},
		{
			name: "Add pod before its node",
			operations: []operation{
				addNode(0), addPod(1), updatePod(1), addNode(1),
			},
			expected:           []*v1.Node{nodes[1], nodes[0]},
			expectedUsedPVCSet: sets.New[string](),
		},
		{
			name: "Remove node before its pods",
			operations: []operation{
				addNode(0), addNode(1), addPod(1), addPod(11), updateSnapshot(),
				removeNode(1), updateSnapshot(),
				updatePod(1), updatePod(11), removePod(1), removePod(11),
			},
			expected:           []*v1.Node{nodes[0]},
			expectedUsedPVCSet: sets.New[string](),
		},
		{
			name: "Add Pods with affinity",
			operations: []operation{
				addNode(0), addPodWithAffinity(0), updateSnapshot(), addNode(1),
			},
			expected:                     []*v1.Node{nodes[1], nodes[0]},
			expectedHavePodsWithAffinity: 1,
			expectedUsedPVCSet:           sets.New[string](),
		},
		{
			name: "Add Pods with PVC",
			operations: []operation{
				addNode(0), addPodWithPVC(0), updateSnapshot(), addNode(1),
			},
			expected:           []*v1.Node{nodes[1], nodes[0]},
			expectedUsedPVCSet: sets.New("test-ns/test-pvc0"),
		},
		{
			name: "Add multiple nodes with pods with affinity",
			operations: []operation{
				addNode(0), addPodWithAffinity(0), updateSnapshot(), addNode(1), addPodWithAffinity(1), updateSnapshot(),
			},
			expected:                     []*v1.Node{nodes[1], nodes[0]},
			expectedHavePodsWithAffinity: 2,
			expectedUsedPVCSet:           sets.New[string](),
		},
		{
			name: "Add multiple nodes with pods with PVC",
			operations: []operation{
				addNode(0), addPodWithPVC(0), updateSnapshot(), addNode(1), addPodWithPVC(1), updateSnapshot(),
			},
			expected:           []*v1.Node{nodes[1], nodes[0]},
			expectedUsedPVCSet: sets.New("test-ns/test-pvc0", "test-ns/test-pvc1"),
		},
		{
			name: "Add then Remove pods with affinity",
			operations: []operation{
				addNode(0), addNode(1), addPodWithAffinity(0), updateSnapshot(), removePodWithAffinity(0), updateSnapshot(),
			},
			expected:                     []*v1.Node{nodes[0], nodes[1]},
			expectedHavePodsWithAffinity: 0,
			expectedUsedPVCSet:           sets.New[string](),
		},
		{
			name: "Add then Remove pod with PVC",
			operations: []operation{
				addNode(0), addPodWithPVC(0), updateSnapshot(), removePodWithPVC(0), addPodWithPVC(2), updateSnapshot(),
			},
			expected:           []*v1.Node{nodes[0]},
			expectedUsedPVCSet: sets.New("test-ns/test-pvc2"),
		},
		{
			name: "Add then Remove pod with PVC and add same pod again",
			operations: []operation{
				addNode(0), addPodWithPVC(0), updateSnapshot(), removePodWithPVC(0), addPodWithPVC(0), updateSnapshot(),
			},
			expected:           []*v1.Node{nodes[0]},
			expectedUsedPVCSet: sets.New("test-ns/test-pvc0"),
		},
		{
			name: "Add and Remove multiple pods with PVC with same ref count length different content",
			operations: []operation{
				addNode(0), addNode(1), addPodWithPVC(0), addPodWithPVC(1), updateSnapshot(),
				removePodWithPVC(0), removePodWithPVC(1), addPodWithPVC(2), addPodWithPVC(3), updateSnapshot(),
			},
			expected:           []*v1.Node{nodes[1], nodes[0]},
			expectedUsedPVCSet: sets.New("test-ns/test-pvc2", "test-ns/test-pvc3"),
		},
		{
			name: "Add and Remove multiple pods with PVC",
			operations: []operation{
				addNode(0), addNode(1), addPodWithPVC(0), addPodWithPVC(1), addPodWithPVC(2), updateSnapshot(),
				removePodWithPVC(0), removePodWithPVC(1), updateSnapshot(), addPodWithPVC(0), updateSnapshot(),
				addPodWithPVC(3), addPodWithPVC(4), addPodWithPVC(5), updateSnapshot(),
				removePodWithPVC(0), removePodWithPVC(3), removePodWithPVC(4), updateSnapshot(),
			},
			expected:           []*v1.Node{nodes[0], nodes[1]},
			expectedUsedPVCSet: sets.New("test-ns/test-pvc1", "test-ns/test-pvc2"),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, ctx := ktesting.NewTestContext(t)
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			cache = newCache(ctx, time.Second, time.Second)
			snapshot = NewEmptySnapshot()

			for _, op := range test.operations {
				op(t)
			}

			if len(test.expected) != len(cache.nodes) {
				t.Errorf("unexpected number of nodes. Expected: %v, got: %v", len(test.expected), len(cache.nodes))
			}
			var i int
			// Check that cache is in the expected state.
			for node := cache.headNode; node != nil; node = node.next {
				if node.info.Node() != nil && node.info.Node().Name != test.expected[i].Name {
					t.Errorf("unexpected node. Expected: %v, got: %v, index: %v", test.expected[i].Name, node.info.Node().Name, i)
				}
				i++
			}
			// Make sure we visited all the cached nodes in the above for loop.
			if i != len(cache.nodes) {
				t.Errorf("Not all the nodes were visited by following the NodeInfo linked list. Expected to see %v nodes, saw %v.", len(cache.nodes), i)
			}

			// Check number of nodes with pods with affinity
			if len(snapshot.havePodsWithAffinityNodeInfoList) != test.expectedHavePodsWithAffinity {
				t.Errorf("unexpected number of HavePodsWithAffinity nodes. Expected: %v, got: %v", test.expectedHavePodsWithAffinity, len(snapshot.havePodsWithAffinityNodeInfoList))
			}

			// Compare content of the used PVC set
			if diff := cmp.Diff(test.expectedUsedPVCSet, snapshot.usedPVCSet); diff != "" {
				t.Errorf("Unexpected usedPVCSet (-want +got):\n%s", diff)
			}

			// Always update the snapshot at the end of operations and compare it.
			if err := cache.UpdateSnapshot(logger, snapshot); err != nil {
				t.Error(err)
			}
			if err := compareCacheWithNodeInfoSnapshot(t, cache, snapshot); err != nil {
				t.Error(err)
			}
		})
	}
}

func compareCacheWithNodeInfoSnapshot(t *testing.T, cache *cacheImpl, snapshot *Snapshot) error {
	// Compare the map.
	if len(snapshot.nodeInfoMap) != cache.nodeTree.numNodes {
		return fmt.Errorf("unexpected number of nodes in the snapshot. Expected: %v, got: %v", cache.nodeTree.numNodes, len(snapshot.nodeInfoMap))
	}
	for name, ni := range cache.nodes {
		want := ni.info
		if want.Node() == nil {
			want = nil
		}
		if diff := cmp.Diff(want, snapshot.nodeInfoMap[name], nodeInfoCmpOpts...); diff != "" {
			return fmt.Errorf("Unexpected node info for node (-want, +got):\n%s", diff)
		}
	}

	// Compare the lists.
	if len(snapshot.nodeInfoList) != cache.nodeTree.numNodes {
		return fmt.Errorf("unexpected number of nodes in NodeInfoList. Expected: %v, got: %v", cache.nodeTree.numNodes, len(snapshot.nodeInfoList))
	}

	expectedNodeInfoList := make([]*framework.NodeInfo, 0, cache.nodeTree.numNodes)
	expectedHavePodsWithAffinityNodeInfoList := make([]*framework.NodeInfo, 0, cache.nodeTree.numNodes)
	expectedUsedPVCSet := sets.New[string]()
	nodesList, err := cache.nodeTree.list()
	if err != nil {
		t.Fatal(err)
	}
	for _, nodeName := range nodesList {
		if n := snapshot.nodeInfoMap[nodeName]; n != nil {
			expectedNodeInfoList = append(expectedNodeInfoList, n)
			if len(n.PodsWithAffinity) > 0 {
				expectedHavePodsWithAffinityNodeInfoList = append(expectedHavePodsWithAffinityNodeInfoList, n)
			}
			for key := range n.PVCRefCounts {
				expectedUsedPVCSet.Insert(key)
			}
		} else {
			return fmt.Errorf("node %q exist in nodeTree but not in NodeInfoMap, this should not happen", nodeName)
		}
	}

	for i, expected := range expectedNodeInfoList {
		got := snapshot.nodeInfoList[i]
		if expected != got {
			return fmt.Errorf("unexpected NodeInfo pointer in NodeInfoList. Expected: %p, got: %p", expected, got)
		}
	}

	for i, expected := range expectedHavePodsWithAffinityNodeInfoList {
		got := snapshot.havePodsWithAffinityNodeInfoList[i]
		if expected != got {
			return fmt.Errorf("unexpected NodeInfo pointer in HavePodsWithAffinityNodeInfoList. Expected: %p, got: %p", expected, got)
		}
	}

	for key := range expectedUsedPVCSet {
		if !snapshot.usedPVCSet.Has(key) {
			return fmt.Errorf("expected PVC %s to exist in UsedPVCSet but it is not found", key)
		}
	}

	return nil
}

func TestSchedulerCache_updateNodeInfoSnapshotList(t *testing.T) {
	logger, _ := ktesting.NewTestContext(t)

	// Create a few nodes to be used in tests.
	var nodes []*v1.Node
	i := 0
	// List of number of nodes per zone, zone 0 -> 2, zone 1 -> 6
	for zone, nb := range []int{2, 6} {
		for j := 0; j < nb; j++ {
			nodes = append(nodes, &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: fmt.Sprintf("node-%d", i),
					Labels: map[string]string{
						v1.LabelTopologyRegion: fmt.Sprintf("region-%d", zone),
						v1.LabelTopologyZone:   fmt.Sprintf("zone-%d", zone),
					},
				},
			})
			i++
		}
	}

	var cache *cacheImpl
	var snapshot *Snapshot

	addNode := func(t *testing.T, i int) {
		cache.AddNode(logger, nodes[i])
		_, ok := snapshot.nodeInfoMap[nodes[i].Name]
		if !ok {
			snapshot.nodeInfoMap[nodes[i].Name] = cache.nodes[nodes[i].Name].info
		}
	}

	updateSnapshot := func(t *testing.T) {
		cache.updateNodeInfoSnapshotList(logger, snapshot, true)
		if err := compareCacheWithNodeInfoSnapshot(t, cache, snapshot); err != nil {
			t.Error(err)
		}
	}

	tests := []struct {
		name       string
		operations func(t *testing.T)
		expected   []string
	}{
		{
			name:       "Empty cache",
			operations: func(t *testing.T) {},
			expected:   []string{},
		},
		{
			name: "Single node",
			operations: func(t *testing.T) {
				addNode(t, 0)
			},
			expected: []string{"node-0"},
		},
		{
			name: "Two nodes",
			operations: func(t *testing.T) {
				addNode(t, 0)
				updateSnapshot(t)
				addNode(t, 1)
			},
			expected: []string{"node-0", "node-1"},
		},
		{
			name: "bug 91601, two nodes, update the snapshot and add two nodes in different zones",
			operations: func(t *testing.T) {
				addNode(t, 2)
				addNode(t, 3)
				updateSnapshot(t)
				addNode(t, 4)
				addNode(t, 0)
			},
			expected: []string{"node-2", "node-0", "node-3", "node-4"},
		},
		{
			name: "bug 91601, 6 nodes, one in a different zone",
			operations: func(t *testing.T) {
				addNode(t, 2)
				addNode(t, 3)
				addNode(t, 4)
				addNode(t, 5)
				updateSnapshot(t)
				addNode(t, 6)
				addNode(t, 0)
			},
			expected: []string{"node-2", "node-0", "node-3", "node-4", "node-5", "node-6"},
		},
		{
			name: "bug 91601, 7 nodes, two in a different zone",
			operations: func(t *testing.T) {
				addNode(t, 2)
				updateSnapshot(t)
				addNode(t, 3)
				addNode(t, 4)
				updateSnapshot(t)
				addNode(t, 5)
				addNode(t, 6)
				addNode(t, 0)
				addNode(t, 1)
			},
			expected: []string{"node-2", "node-0", "node-3", "node-1", "node-4", "node-5", "node-6"},
		},
		{
			name: "bug 91601, 7 nodes, two in a different zone, different zone order",
			operations: func(t *testing.T) {
				addNode(t, 2)
				addNode(t, 1)
				updateSnapshot(t)
				addNode(t, 3)
				addNode(t, 4)
				updateSnapshot(t)
				addNode(t, 5)
				addNode(t, 6)
				addNode(t, 0)
			},
			expected: []string{"node-2", "node-1", "node-3", "node-0", "node-4", "node-5", "node-6"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, ctx := ktesting.NewTestContext(t)
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			cache = newCache(ctx, time.Second, time.Second)
			snapshot = NewEmptySnapshot()

			test.operations(t)

			// Always update the snapshot at the end of operations and compare it.
			cache.updateNodeInfoSnapshotList(logger, snapshot, true)
			if err := compareCacheWithNodeInfoSnapshot(t, cache, snapshot); err != nil {
				t.Error(err)
			}
			nodeNames := make([]string, len(snapshot.nodeInfoList))
			for i, nodeInfo := range snapshot.nodeInfoList {
				nodeNames[i] = nodeInfo.Node().Name
			}
			if diff := cmp.Diff(test.expected, nodeNames); diff != "" {
				t.Errorf("Unexpected nodeInfoList (-want, +got):\n%s", diff)
			}
		})
	}
}

func BenchmarkUpdate1kNodes30kPods(b *testing.B) {
	logger, _ := ktesting.NewTestContext(b)
	cache := setupCacheOf1kNodes30kPods(b)
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		cachedNodes := NewEmptySnapshot()
		cache.UpdateSnapshot(logger, cachedNodes)
	}
}

func BenchmarkExpirePods(b *testing.B) {
	podNums := []int{
		100,
		1000,
		10000,
	}
	for _, podNum := range podNums {
		name := fmt.Sprintf("%dPods", podNum)
		b.Run(name, func(b *testing.B) {
			benchmarkExpire(b, podNum)
		})
	}
}

func benchmarkExpire(b *testing.B, podNum int) {
	logger, _ := ktesting.NewTestContext(b)
	now := time.Now()
	for n := 0; n < b.N; n++ {
		b.StopTimer()
		cache := setupCacheWithAssumedPods(b, podNum, now)
		b.StartTimer()
		cache.cleanupAssumedPods(logger, now.Add(2*time.Second))
	}
}

type testingMode interface {
	Fatalf(format string, args ...interface{})
}

func makeBasePod(t testingMode, nodeName, objName, cpu, mem, extended string, ports []v1.ContainerPort) *v1.Pod {
	req := make(map[v1.ResourceName]string)
	if cpu != "" {
		req[v1.ResourceCPU] = cpu
		req[v1.ResourceMemory] = mem

		if extended != "" {
			parts := strings.Split(extended, ":")
			if len(parts) != 2 {
				t.Fatalf("Invalid extended resource string: \"%s\"", extended)
			}
			req[v1.ResourceName(parts[0])] = parts[1]
		}
	}
	podWrapper := st.MakePod().Name(objName).Namespace("node_info_cache_test").UID(objName).Node(nodeName).Containers([]v1.Container{
		st.MakeContainer().Name("container").Image("pause").Resources(req).ContainerPort(ports).Obj(),
	})
	return podWrapper.Obj()
}

// checkImageStateSummary collect ImageStateSummary of image traverse nodes,
// the collected ImageStateSummary should be equal
func checkImageStateSummary(nodes map[string]*framework.NodeInfo, imageNames ...string) bool {
	for _, imageName := range imageNames {
		var imageState *framework.ImageStateSummary
		for _, node := range nodes {
			state, ok := node.ImageStates[imageName]
			if !ok {
				continue
			}
			if imageState == nil {
				imageState = state
				continue
			}
			if diff := cmp.Diff(imageState, state); diff != "" {
				return false
			}
		}
	}
	return true
}

func setupCacheOf1kNodes30kPods(b *testing.B) Cache {
	logger, ctx := ktesting.NewTestContext(b)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cache := newCache(ctx, time.Second, time.Second)
	for i := 0; i < 1000; i++ {
		nodeName := fmt.Sprintf("node-%d", i)
		cache.AddNode(logger, st.MakeNode().Name(nodeName).Obj())
		for j := 0; j < 30; j++ {
			objName := fmt.Sprintf("%s-pod-%d", nodeName, j)
			pod := makeBasePod(b, nodeName, objName, "0", "0", "", nil)

			if err := cache.AddPod(logger, pod); err != nil {
				b.Fatalf("AddPod failed: %v", err)
			}
		}
	}
	return cache
}

func setupCacheWithAssumedPods(b *testing.B, podNum int, assumedTime time.Time) *cacheImpl {
	logger, ctx := ktesting.NewTestContext(b)
	ctx, cancel := context.WithCancel(ctx)
	addedNodes := make(map[string]struct{})
	defer cancel()
	cache := newCache(ctx, time.Second, time.Second)
	for i := 0; i < podNum; i++ {
		nodeName := fmt.Sprintf("node-%d", i/10)
		if _, ok := addedNodes[nodeName]; !ok {
			cache.AddNode(logger, st.MakeNode().Name(nodeName).Obj())
			addedNodes[nodeName] = struct{}{}
		}
		objName := fmt.Sprintf("%s-pod-%d", nodeName, i%10)
		pod := makeBasePod(b, nodeName, objName, "0", "0", "", nil)

		err := assumeAndFinishBinding(logger, cache, pod, assumedTime)
		if err != nil {
			b.Fatalf("assumePod failed: %v", err)
		}
	}
	return cache
}

func isForgottenFromCache(p *v1.Pod, c *cacheImpl) error {
	if assumed, err := c.IsAssumedPod(p); err != nil {
		return err
	} else if assumed {
		return errors.New("still assumed")
	}
	if _, err := c.GetPod(p); err == nil {
		return errors.New("still in cache")
	}
	return nil
}

// getNodeInfo returns cached data for the node name.
func (cache *cacheImpl) getNodeInfo(nodeName string) (*v1.Node, error) {
	cache.mu.RLock()
	defer cache.mu.RUnlock()

	n, ok := cache.nodes[nodeName]
	if !ok {
		return nil, fmt.Errorf("node %q not found in cache", nodeName)
	}

	return n.info.Node(), nil
}
