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

package service

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	v1core "k8s.io/client-go/kubernetes/typed/core/v1"
	core "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	fakecloud "k8s.io/cloud-provider/fake"
	servicehelper "k8s.io/cloud-provider/service/helpers"

	utilpointer "k8s.io/utils/pointer"
)

const region = "us-central"

func newService(name string, uid types.UID, serviceType v1.ServiceType) *v1.Service {
	return &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
			UID:       uid,
		},
		Spec: v1.ServiceSpec{
			Type: serviceType,
		},
	}
}

func newETPLocalService(name string, serviceType v1.ServiceType) *v1.Service {
	return &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
			UID:       "777",
		},
		Spec: v1.ServiceSpec{
			Type:                  serviceType,
			ExternalTrafficPolicy: v1.ServiceExternalTrafficPolicyLocal,
		},
	}
}

// Wrap newService so that you don't have to call default arguments again and again.
func defaultExternalService() *v1.Service {
	return newService("external-balancer", types.UID("123"), v1.ServiceTypeLoadBalancer)
}

func alwaysReady() bool { return true }

type fakeController struct {
	*Controller
	nodeStore    cache.Store
	serviceStore cache.Store
}

func newController() (*fakeController, *fakecloud.Cloud, *fake.Clientset) {
	cloud := &fakecloud.Cloud{}
	cloud.Region = region

	kubeClient := fake.NewSimpleClientset()
	informerFactory := informers.NewSharedInformerFactory(kubeClient, 0)
	serviceInformer := informerFactory.Core().V1().Services()
	nodeInformer := informerFactory.Core().V1().Nodes()
	broadcaster := record.NewBroadcaster()
	broadcaster.StartStructuredLogging(0)
	broadcaster.StartRecordingToSink(&v1core.EventSinkImpl{Interface: kubeClient.CoreV1().Events("")})
	balancer, _ := cloud.LoadBalancer()

	controller := &Controller{
		cloud:               cloud,
		kubeClient:          kubeClient,
		clusterName:         "test-cluster",
		eventBroadcaster:    broadcaster,
		balancer:            balancer,
		eventRecorder:       record.NewFakeRecorder(100),
		serviceLister:       serviceInformer.Lister(),
		serviceListerSynced: alwaysReady,
		nodeLister:          nodeInformer.Lister(),
		nodeListerSynced:    alwaysReady,
		serviceQueue:        workqueue.NewNamedRateLimitingQueue(workqueue.NewItemExponentialFailureRateLimiter(minRetryDelay, maxRetryDelay), "service"),
		nodeQueue:           workqueue.NewNamedRateLimitingQueue(workqueue.NewItemExponentialFailureRateLimiter(minRetryDelay, maxRetryDelay), "node"),
		lastSyncedNodes:     []*v1.Node{},
	}

	cloud.Calls = nil         // ignore any cloud calls made in init()
	kubeClient.ClearActions() // ignore any client calls made in init()

	fakeController := &fakeController{
		Controller:   controller,
		nodeStore:    nodeInformer.Informer().GetStore(),
		serviceStore: serviceInformer.Informer().GetStore(),
	}
	return fakeController, cloud, kubeClient
}

// TODO(@MrHohn): Verify the end state when below issue is resolved:
// https://github.com/kubernetes/client-go/issues/607
func TestSyncLoadBalancerIfNeeded(t *testing.T) {
	testCases := []struct {
		desc                 string
		service              *v1.Service
		lbExists             bool
		expectCreateAttempt  bool
		expectDeleteAttempt  bool
		expectPatchStatus    bool
		expectPatchFinalizer bool
	}{
		{
			desc: "service doesn't want LB",
			service: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "no-external-balancer",
					Namespace: "default",
				},
				Spec: v1.ServiceSpec{
					Type: v1.ServiceTypeClusterIP,
				},
			},
			expectPatchStatus: false,
		},
		{
			desc: "service no longer wants LB",
			service: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "no-external-balancer",
					Namespace: "default",
				},
				Spec: v1.ServiceSpec{
					Type: v1.ServiceTypeClusterIP,
				},
				Status: v1.ServiceStatus{
					LoadBalancer: v1.LoadBalancerStatus{
						Ingress: []v1.LoadBalancerIngress{
							{IP: "8.8.8.8"},
						},
					},
				},
			},
			lbExists:            true,
			expectDeleteAttempt: true,
			expectPatchStatus:   true,
		},
		{
			desc: "udp service that wants LB",
			service: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "udp-service",
					Namespace: "default",
				},
				Spec: v1.ServiceSpec{
					Ports: []v1.ServicePort{{
						Port:     80,
						Protocol: v1.ProtocolUDP,
					}},
					Type: v1.ServiceTypeLoadBalancer,
				},
			},
			expectCreateAttempt:  true,
			expectPatchStatus:    true,
			expectPatchFinalizer: true,
		},
		{
			desc: "tcp service that wants LB",
			service: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "basic-service1",
					Namespace: "default",
				},
				Spec: v1.ServiceSpec{
					Ports: []v1.ServicePort{{
						Port:     80,
						Protocol: v1.ProtocolTCP,
					}},
					Type: v1.ServiceTypeLoadBalancer,
				},
			},
			expectCreateAttempt:  true,
			expectPatchStatus:    true,
			expectPatchFinalizer: true,
		},
		{
			desc: "sctp service that wants LB",
			service: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "sctp-service",
					Namespace: "default",
				},
				Spec: v1.ServiceSpec{
					Ports: []v1.ServicePort{{
						Port:     80,
						Protocol: v1.ProtocolSCTP,
					}},
					Type: v1.ServiceTypeLoadBalancer,
				},
			},
			expectCreateAttempt:  true,
			expectPatchStatus:    true,
			expectPatchFinalizer: true,
		},
		{
			desc: "service specifies loadBalancerClass",
			service: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "with-external-balancer",
					Namespace: "default",
				},
				Spec: v1.ServiceSpec{
					Type:              v1.ServiceTypeLoadBalancer,
					LoadBalancerClass: utilpointer.StringPtr("custom-loadbalancer"),
				},
			},
			expectCreateAttempt:  false,
			expectPatchStatus:    false,
			expectPatchFinalizer: false,
		},
		{
			desc: "service doesn't specify loadBalancerClass",
			service: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "with-external-balancer",
					Namespace: "default",
				},
				Spec: v1.ServiceSpec{
					Ports: []v1.ServicePort{{
						Port:     80,
						Protocol: v1.ProtocolSCTP,
					}},
					Type:              v1.ServiceTypeLoadBalancer,
					LoadBalancerClass: nil,
				},
			},
			expectCreateAttempt:  true,
			expectPatchStatus:    true,
			expectPatchFinalizer: true,
		},
		// Finalizer test cases below.
		{
			desc: "service with finalizer that no longer wants LB",
			service: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "no-external-balancer",
					Namespace:  "default",
					Finalizers: []string{servicehelper.LoadBalancerCleanupFinalizer},
				},
				Spec: v1.ServiceSpec{
					Type: v1.ServiceTypeClusterIP,
				},
				Status: v1.ServiceStatus{
					LoadBalancer: v1.LoadBalancerStatus{
						Ingress: []v1.LoadBalancerIngress{
							{IP: "8.8.8.8"},
						},
					},
				},
			},
			lbExists:             true,
			expectDeleteAttempt:  true,
			expectPatchStatus:    true,
			expectPatchFinalizer: true,
		},
		{
			desc: "service that needs cleanup",
			service: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "basic-service1",
					Namespace: "default",
					DeletionTimestamp: &metav1.Time{
						Time: time.Now(),
					},
					Finalizers: []string{servicehelper.LoadBalancerCleanupFinalizer},
				},
				Spec: v1.ServiceSpec{
					Ports: []v1.ServicePort{{
						Port:     80,
						Protocol: v1.ProtocolTCP,
					}},
					Type: v1.ServiceTypeLoadBalancer,
				},
				Status: v1.ServiceStatus{
					LoadBalancer: v1.LoadBalancerStatus{
						Ingress: []v1.LoadBalancerIngress{
							{IP: "8.8.8.8"},
						},
					},
				},
			},
			lbExists:             true,
			expectDeleteAttempt:  true,
			expectPatchStatus:    true,
			expectPatchFinalizer: true,
		},
		{
			desc: "service without finalizer that wants LB",
			service: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "basic-service1",
					Namespace: "default",
				},
				Spec: v1.ServiceSpec{
					Ports: []v1.ServicePort{{
						Port:     80,
						Protocol: v1.ProtocolTCP,
					}},
					Type: v1.ServiceTypeLoadBalancer,
				},
			},
			expectCreateAttempt:  true,
			expectPatchStatus:    true,
			expectPatchFinalizer: true,
		},
		{
			desc: "service with finalizer that wants LB",
			service: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "basic-service1",
					Namespace:  "default",
					Finalizers: []string{servicehelper.LoadBalancerCleanupFinalizer},
				},
				Spec: v1.ServiceSpec{
					Ports: []v1.ServicePort{{
						Port:     80,
						Protocol: v1.ProtocolTCP,
					}},
					Type: v1.ServiceTypeLoadBalancer,
				},
			},
			expectCreateAttempt:  true,
			expectPatchStatus:    true,
			expectPatchFinalizer: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.desc, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			controller, cloud, client := newController()
			cloud.Exists = tc.lbExists
			if _, err := client.CoreV1().Services(tc.service.Namespace).Create(ctx, tc.service, metav1.CreateOptions{}); err != nil {
				t.Fatalf("Failed to prepare service for testing: %v", err)
			}
			client.ClearActions()

			if err := controller.syncLoadBalancerIfNeeded(ctx, tc.service); err != nil {
				t.Errorf("Got error: %v, want nil", err)
			}

			// Capture actions from test so it won't be messed up.
			actions := client.Actions()

			if !tc.expectCreateAttempt && !tc.expectDeleteAttempt {
				if len(cloud.Calls) > 0 {
					t.Errorf("Unexpected cloud provider calls: %v", cloud.Calls)
				}
				if len(actions) > 0 {
					t.Errorf("Unexpected client actions: %v", actions)
				}
				return
			}

			if tc.expectCreateAttempt {
				createCallFound := false
				for _, call := range cloud.Calls {
					if call == "create" {
						createCallFound = true
					}
				}
				if !createCallFound {
					t.Errorf("Got no create call for load balancer, expected one")
				}

				if len(cloud.Balancers) == 0 {
					t.Errorf("Got no load balancer: %v, expected one to be created", cloud.Balancers)
				}

				for _, balancer := range cloud.Balancers {
					if balancer.Name != controller.balancer.GetLoadBalancerName(context.Background(), "", tc.service) ||
						balancer.Region != region ||
						balancer.Ports[0].Port != tc.service.Spec.Ports[0].Port {
						t.Errorf("Created load balancer has incorrect parameters: %v", balancer)
					}
				}
			}

			if tc.expectDeleteAttempt {
				deleteCallFound := false
				for _, call := range cloud.Calls {
					if call == "delete" {
						deleteCallFound = true
					}
				}
				if !deleteCallFound {
					t.Errorf("Got no delete call for load balancer, expected one")
				}
			}

			expectNumPatches := 0
			if tc.expectPatchStatus {
				expectNumPatches++
			}
			if tc.expectPatchFinalizer {
				expectNumPatches++
			}
			numPatches := 0
			for _, action := range actions {
				if action.Matches("patch", "services") {
					numPatches++
				}
			}
			if numPatches != expectNumPatches {
				t.Errorf("Got %d patches, expect %d instead. Actions: %v", numPatches, expectNumPatches, actions)
			}
		})
	}
}

// TODO: Finish converting and update comments
func TestUpdateNodesInExternalLoadBalancer(t *testing.T) {
	nodes := []*v1.Node{
		{ObjectMeta: metav1.ObjectMeta{Name: "node0"}, Status: v1.NodeStatus{Conditions: []v1.NodeCondition{{Type: v1.NodeReady, Status: v1.ConditionTrue}}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "node1"}, Status: v1.NodeStatus{Conditions: []v1.NodeCondition{{Type: v1.NodeReady, Status: v1.ConditionTrue}}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "node73"}, Status: v1.NodeStatus{Conditions: []v1.NodeCondition{{Type: v1.NodeReady, Status: v1.ConditionTrue}}}},
	}
	table := []struct {
		desc                string
		services            []*v1.Service
		expectedUpdateCalls []fakecloud.UpdateBalancerCall
		workers             int
	}{
		{
			desc:                "No services present: no calls should be made.",
			services:            []*v1.Service{},
			expectedUpdateCalls: nil,
			workers:             1,
		},
		{
			desc: "Services do not have external load balancers: no calls should be made.",
			services: []*v1.Service{
				newService("s0", "111", v1.ServiceTypeClusterIP),
				newService("s1", "222", v1.ServiceTypeNodePort),
			},
			expectedUpdateCalls: nil,
			workers:             2,
		},
		{
			desc: "Services does have an external load balancer: one call should be made.",
			services: []*v1.Service{
				newService("s0", "333", v1.ServiceTypeLoadBalancer),
			},
			expectedUpdateCalls: []fakecloud.UpdateBalancerCall{
				{Service: newService("s0", "333", v1.ServiceTypeLoadBalancer), Hosts: nodes},
			},
			workers: 3,
		},
		{
			desc: "Three services have an external load balancer: three calls.",
			services: []*v1.Service{
				newService("s0", "444", v1.ServiceTypeLoadBalancer),
				newService("s1", "555", v1.ServiceTypeLoadBalancer),
				newService("s2", "666", v1.ServiceTypeLoadBalancer),
			},
			expectedUpdateCalls: []fakecloud.UpdateBalancerCall{
				{Service: newService("s0", "444", v1.ServiceTypeLoadBalancer), Hosts: nodes},
				{Service: newService("s1", "555", v1.ServiceTypeLoadBalancer), Hosts: nodes},
				{Service: newService("s2", "666", v1.ServiceTypeLoadBalancer), Hosts: nodes},
			},
			workers: 4,
		},
		{
			desc: "Two services have an external load balancer and two don't: two calls.",
			services: []*v1.Service{
				newService("s0", "777", v1.ServiceTypeNodePort),
				newService("s1", "888", v1.ServiceTypeLoadBalancer),
				newService("s3", "999", v1.ServiceTypeLoadBalancer),
				newService("s4", "123", v1.ServiceTypeClusterIP),
			},
			expectedUpdateCalls: []fakecloud.UpdateBalancerCall{
				{Service: newService("s1", "888", v1.ServiceTypeLoadBalancer), Hosts: nodes},
				{Service: newService("s3", "999", v1.ServiceTypeLoadBalancer), Hosts: nodes},
			},
			workers: 5,
		},
		{
			desc: "One service has an external load balancer and one is nil: one call.",
			services: []*v1.Service{
				newService("s0", "234", v1.ServiceTypeLoadBalancer),
				nil,
			},
			expectedUpdateCalls: []fakecloud.UpdateBalancerCall{
				{Service: newService("s0", "234", v1.ServiceTypeLoadBalancer), Hosts: nodes},
			},
			workers: 6,
		},
		{
			desc: "Four services have external load balancer with only 2 workers",
			services: []*v1.Service{
				newService("s0", "777", v1.ServiceTypeLoadBalancer),
				newService("s1", "888", v1.ServiceTypeLoadBalancer),
				newService("s3", "999", v1.ServiceTypeLoadBalancer),
				newService("s4", "123", v1.ServiceTypeLoadBalancer),
			},
			expectedUpdateCalls: []fakecloud.UpdateBalancerCall{
				{Service: newService("s0", "777", v1.ServiceTypeLoadBalancer), Hosts: nodes},
				{Service: newService("s1", "888", v1.ServiceTypeLoadBalancer), Hosts: nodes},
				{Service: newService("s3", "999", v1.ServiceTypeLoadBalancer), Hosts: nodes},
				{Service: newService("s4", "123", v1.ServiceTypeLoadBalancer), Hosts: nodes},
			},
			workers: 2,
		},
	}
	for _, item := range table {
		t.Run(item.desc, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			controller, cloud, _ := newController()
			for _, node := range nodes {
				controller.nodeStore.Add(node)
			}
			if servicesToRetry := controller.updateLoadBalancerHosts(ctx, item.services, item.workers); len(servicesToRetry) != 0 {
				t.Errorf("for case %q, unexpected servicesToRetry: %v", item.desc, servicesToRetry)
			}
			compareUpdateCalls(t, item.expectedUpdateCalls, cloud.UpdateCalls)
		})
	}
}

func TestNodeChangesForExternalTrafficPolicyLocalServices(t *testing.T) {
	node1 := &v1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node0"}, Status: v1.NodeStatus{Conditions: []v1.NodeCondition{{Type: v1.NodeReady, Status: v1.ConditionTrue}}}}
	node2 := &v1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node1"}, Status: v1.NodeStatus{Conditions: []v1.NodeCondition{{Type: v1.NodeReady, Status: v1.ConditionTrue}}}}
	node2NotReady := &v1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node1"}, Status: v1.NodeStatus{Conditions: []v1.NodeCondition{{Type: v1.NodeReady, Status: v1.ConditionFalse}}}}
	node2Tainted := &v1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node1"}, Spec: v1.NodeSpec{Taints: []v1.Taint{{Key: ToBeDeletedTaint}}}, Status: v1.NodeStatus{Conditions: []v1.NodeCondition{{Type: v1.NodeReady, Status: v1.ConditionFalse}}}}
	node2SpuriousChange := &v1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node1"}, Status: v1.NodeStatus{Phase: v1.NodeTerminated, Conditions: []v1.NodeCondition{{Type: v1.NodeReady, Status: v1.ConditionTrue}}}}
	node2Exclude := &v1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node1", Labels: map[string]string{v1.LabelNodeExcludeBalancers: ""}}, Status: v1.NodeStatus{Conditions: []v1.NodeCondition{{Type: v1.NodeReady, Status: v1.ConditionTrue}}}}
	node3 := &v1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node73"}, Status: v1.NodeStatus{Conditions: []v1.NodeCondition{{Type: v1.NodeReady, Status: v1.ConditionTrue}}}}

	type stateChanges struct {
		nodes       []*v1.Node
		syncCallErr bool
	}

	etpLocalservice1 := newETPLocalService("s0", v1.ServiceTypeLoadBalancer)
	etpLocalservice2 := newETPLocalService("s1", v1.ServiceTypeLoadBalancer)
	service3 := defaultExternalService()

	services := []*v1.Service{etpLocalservice1, etpLocalservice2, service3}

	for _, tc := range []struct {
		desc                string
		expectedUpdateCalls []fakecloud.UpdateBalancerCall
		stateChanges        []stateChanges
		initialState        []*v1.Node
	}{
		{
			desc:         "No node changes",
			initialState: []*v1.Node{node1, node2, node3},
			stateChanges: []stateChanges{
				{
					nodes: []*v1.Node{node1, node2, node3},
				},
			},
			expectedUpdateCalls: []fakecloud.UpdateBalancerCall{},
		},
		{
			desc:         "1 new node gets added",
			initialState: []*v1.Node{node1, node2},
			stateChanges: []stateChanges{
				{
					nodes: []*v1.Node{node1, node2, node3},
				},
			},
			expectedUpdateCalls: []fakecloud.UpdateBalancerCall{
				{Service: etpLocalservice1, Hosts: []*v1.Node{node1, node2, node3}},
				{Service: etpLocalservice2, Hosts: []*v1.Node{node1, node2, node3}},
				{Service: service3, Hosts: []*v1.Node{node1, node2, node3}},
			},
		},
		{
			desc:         "1 new node gets added - with retries",
			initialState: []*v1.Node{node1, node2},
			stateChanges: []stateChanges{
				{
					nodes:       []*v1.Node{node1, node2, node3},
					syncCallErr: true,
				},
				{
					nodes: []*v1.Node{node1, node2, node3},
				},
			},
			expectedUpdateCalls: []fakecloud.UpdateBalancerCall{
				{Service: etpLocalservice1, Hosts: []*v1.Node{node1, node2, node3}},
				{Service: etpLocalservice2, Hosts: []*v1.Node{node1, node2, node3}},
				{Service: service3, Hosts: []*v1.Node{node1, node2, node3}},
			},
		},
		{
			desc:         "1 node goes NotReady",
			initialState: []*v1.Node{node1, node2, node3},
			stateChanges: []stateChanges{
				{
					nodes: []*v1.Node{node1, node2NotReady, node3},
				},
			},
			expectedUpdateCalls: []fakecloud.UpdateBalancerCall{
				{Service: service3, Hosts: []*v1.Node{node1, node3}},
			},
		},
		{
			desc:         "1 node gets Tainted",
			initialState: []*v1.Node{node1, node2, node3},
			stateChanges: []stateChanges{
				{
					nodes: []*v1.Node{node1, node2Tainted, node3},
				},
			},
			expectedUpdateCalls: []fakecloud.UpdateBalancerCall{
				{Service: etpLocalservice1, Hosts: []*v1.Node{node1, node3}},
				{Service: etpLocalservice2, Hosts: []*v1.Node{node1, node3}},
				{Service: service3, Hosts: []*v1.Node{node1, node3}},
			},
		},
		{
			desc:         "1 node goes Ready",
			initialState: []*v1.Node{node1, node2NotReady, node3},
			stateChanges: []stateChanges{
				{
					nodes: []*v1.Node{node1, node2, node3},
				},
			},
			expectedUpdateCalls: []fakecloud.UpdateBalancerCall{
				{Service: service3, Hosts: []*v1.Node{node1, node2, node3}},
			},
		},
		{
			desc:         "1 node get excluded",
			initialState: []*v1.Node{node1, node2, node3},
			stateChanges: []stateChanges{
				{
					nodes: []*v1.Node{node1, node2Exclude, node3},
				},
			},
			expectedUpdateCalls: []fakecloud.UpdateBalancerCall{
				{Service: etpLocalservice1, Hosts: []*v1.Node{node1, node3}},
				{Service: etpLocalservice2, Hosts: []*v1.Node{node1, node3}},
				{Service: service3, Hosts: []*v1.Node{node1, node3}},
			},
		},
		{
			desc:         "1 old node gets deleted",
			initialState: []*v1.Node{node1, node2, node3},
			stateChanges: []stateChanges{
				{
					nodes: []*v1.Node{node1, node2},
				},
			},
			expectedUpdateCalls: []fakecloud.UpdateBalancerCall{
				{Service: etpLocalservice1, Hosts: []*v1.Node{node1, node2}},
				{Service: etpLocalservice2, Hosts: []*v1.Node{node1, node2}},
				{Service: service3, Hosts: []*v1.Node{node1, node2}},
			},
		},
		{
			desc:         "1 spurious node update",
			initialState: []*v1.Node{node1, node2, node3},
			stateChanges: []stateChanges{
				{
					nodes: []*v1.Node{node1, node2SpuriousChange, node3},
				},
			},
			expectedUpdateCalls: []fakecloud.UpdateBalancerCall{},
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			controller, cloud, _ := newController()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			controller.lastSyncedNodes = tc.initialState

			for _, state := range tc.stateChanges {
				setupState := func() {
					for _, node := range state.nodes {
						controller.nodeStore.Add(node)
					}
					if state.syncCallErr {
						cloud.Err = fmt.Errorf("error please")
					}
				}
				cleanupState := func() {
					cloud.Err = nil
				}
				setupState()
				controller.updateLoadBalancerHosts(ctx, services, 3)
				cleanupState()
			}

			compareUpdateCalls(t, tc.expectedUpdateCalls, cloud.UpdateCalls)
		})
	}
}

func TestNodeChangesInExternalLoadBalancer(t *testing.T) {
	node1 := &v1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node0"}, Status: v1.NodeStatus{Conditions: []v1.NodeCondition{{Type: v1.NodeReady, Status: v1.ConditionTrue}}}}
	node2 := &v1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node1"}, Status: v1.NodeStatus{Conditions: []v1.NodeCondition{{Type: v1.NodeReady, Status: v1.ConditionTrue}}}}
	node3 := &v1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node73"}, Status: v1.NodeStatus{Conditions: []v1.NodeCondition{{Type: v1.NodeReady, Status: v1.ConditionTrue}}}}
	node4 := &v1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node4"}, Status: v1.NodeStatus{Conditions: []v1.NodeCondition{{Type: v1.NodeReady, Status: v1.ConditionTrue}}}}

	services := []*v1.Service{
		newService("s0", "777", v1.ServiceTypeLoadBalancer),
		newService("s1", "888", v1.ServiceTypeLoadBalancer),
		newService("s3", "999", v1.ServiceTypeLoadBalancer),
		newService("s4", "123", v1.ServiceTypeLoadBalancer),
	}

	serviceNames := sets.NewString()
	for _, svc := range services {
		serviceNames.Insert(fmt.Sprintf("%s/%s", svc.GetObjectMeta().GetNamespace(), svc.GetObjectMeta().GetName()))
	}

	controller, cloud, _ := newController()
	for _, tc := range []struct {
		desc                  string
		nodes                 []*v1.Node
		expectedUpdateCalls   []fakecloud.UpdateBalancerCall
		worker                int
		nodeListerErr         error
		expectedRetryServices sets.String
	}{
		{
			desc:  "only 1 node",
			nodes: []*v1.Node{node1},
			expectedUpdateCalls: []fakecloud.UpdateBalancerCall{
				{Service: newService("s0", "777", v1.ServiceTypeLoadBalancer), Hosts: []*v1.Node{node1}},
				{Service: newService("s1", "888", v1.ServiceTypeLoadBalancer), Hosts: []*v1.Node{node1}},
				{Service: newService("s3", "999", v1.ServiceTypeLoadBalancer), Hosts: []*v1.Node{node1}},
				{Service: newService("s4", "123", v1.ServiceTypeLoadBalancer), Hosts: []*v1.Node{node1}},
			},
			worker:                3,
			nodeListerErr:         nil,
			expectedRetryServices: sets.NewString(),
		},
		{
			desc:  "2 nodes",
			nodes: []*v1.Node{node1, node2},
			expectedUpdateCalls: []fakecloud.UpdateBalancerCall{
				{Service: newService("s0", "777", v1.ServiceTypeLoadBalancer), Hosts: []*v1.Node{node1, node2}},
				{Service: newService("s1", "888", v1.ServiceTypeLoadBalancer), Hosts: []*v1.Node{node1, node2}},
				{Service: newService("s3", "999", v1.ServiceTypeLoadBalancer), Hosts: []*v1.Node{node1, node2}},
				{Service: newService("s4", "123", v1.ServiceTypeLoadBalancer), Hosts: []*v1.Node{node1, node2}},
			},
			worker:                1,
			nodeListerErr:         nil,
			expectedRetryServices: sets.NewString(),
		},
		{
			desc:  "4 nodes",
			nodes: []*v1.Node{node1, node2, node3, node4},
			expectedUpdateCalls: []fakecloud.UpdateBalancerCall{
				{Service: newService("s0", "777", v1.ServiceTypeLoadBalancer), Hosts: []*v1.Node{node1, node2, node3, node4}},
				{Service: newService("s1", "888", v1.ServiceTypeLoadBalancer), Hosts: []*v1.Node{node1, node2, node3, node4}},
				{Service: newService("s3", "999", v1.ServiceTypeLoadBalancer), Hosts: []*v1.Node{node1, node2, node3, node4}},
				{Service: newService("s4", "123", v1.ServiceTypeLoadBalancer), Hosts: []*v1.Node{node1, node2, node3, node4}},
			},
			worker:                3,
			nodeListerErr:         nil,
			expectedRetryServices: sets.NewString(),
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			for _, node := range tc.nodes {
				controller.nodeStore.Add(node)
			}
			servicesToRetry := controller.updateLoadBalancerHosts(ctx, services, tc.worker)
			assert.Truef(t, tc.expectedRetryServices.Equal(servicesToRetry), "Services to retry are not expected")
			compareUpdateCalls(t, tc.expectedUpdateCalls, cloud.UpdateCalls)
			cloud.UpdateCalls = []fakecloud.UpdateBalancerCall{}
		})
	}
}

// compareUpdateCalls compares if the same update calls were made in both left and right inputs despite the order.
func compareUpdateCalls(t *testing.T, left, right []fakecloud.UpdateBalancerCall) {
	if len(left) != len(right) {
		t.Errorf("expect len(left) == len(right), but got %v != %v", len(left), len(right))
	}

	mismatch := false
	for _, l := range left {
		found := false
		for _, r := range right {
			if updateCallEquals(l, r) {
				found = true
			}
		}
		if !found {
			mismatch = true
			break
		}
	}
	if mismatch {
		t.Errorf("expected update calls to match, expected %+v, got %+v", left, right)
	}
}

func updateCallEquals(l, r fakecloud.UpdateBalancerCall) bool {
	if !reflect.DeepEqual(l.Service, r.Service) {
		return false
	}
	if len(l.Hosts) != len(r.Hosts) {
		return false
	}
	comp := make(map[string]*v1.Node, len(l.Hosts))
	for _, n := range l.Hosts {
		key, _ := cache.MetaNamespaceKeyFunc(n)
		comp[key] = n
	}
	for _, n := range r.Hosts {
		key, _ := cache.MetaNamespaceKeyFunc(n)
		l, exists := comp[key]
		if !exists {
			return false
		}
		if !reflect.DeepEqual(l, n) {
			return false
		}
	}
	return true
}

func TestProcessServiceCreateOrUpdate(t *testing.T) {
	controller, _, client := newController()

	//A pair of old and new loadbalancer IP address
	oldLBIP := "192.168.1.1"
	newLBIP := "192.168.1.11"

	testCases := []struct {
		testName   string
		updateFn   func(*v1.Service) *v1.Service //Manipulate the structure
		svc        *v1.Service
		expectedFn func(*v1.Service, error) error //Error comparison function
	}{
		{
			testName: "If updating a valid service",
			svc:      defaultExternalService(),
			updateFn: func(svc *v1.Service) *v1.Service {
				return svc
			},
			expectedFn: func(svc *v1.Service, err error) error {
				return err
			},
		},
		{
			testName: "If Updating Loadbalancer IP",
			svc:      newService("sync-test-name", types.UID("sync-test-uid"), v1.ServiceTypeLoadBalancer),
			updateFn: func(svc *v1.Service) *v1.Service {

				svc.Spec.LoadBalancerIP = oldLBIP

				keyExpected := svc.GetObjectMeta().GetNamespace() + "/" + svc.GetObjectMeta().GetName()
				controller.enqueueService(svc)

				keyGot, quit := controller.serviceQueue.Get()
				if quit {
					t.Fatalf("get no queue element")
				}
				if keyExpected != keyGot.(string) {
					t.Fatalf("get service key error, expected: %s, got: %s", keyExpected, keyGot.(string))
				}

				newService := svc.DeepCopy()

				newService.Spec.LoadBalancerIP = newLBIP
				return newService

			},
			expectedFn: func(svc *v1.Service, err error) error {

				if err != nil {
					return err
				}

				if svc.Spec.LoadBalancerIP != newLBIP {
					return fmt.Errorf("update LoadBalancerIP error, expected: %s, got: %s", newLBIP, svc.Spec.LoadBalancerIP)
				}
				return nil
			},
		},
	}

	for _, tc := range testCases {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		newSvc := tc.updateFn(tc.svc)
		if _, err := client.CoreV1().Services(tc.svc.Namespace).Create(ctx, tc.svc, metav1.CreateOptions{}); err != nil {
			t.Fatalf("Failed to prepare service for testing: %v", err)
		}
		obtErr := controller.syncLoadBalancerIfNeeded(ctx, newSvc)
		if err := tc.expectedFn(newSvc, obtErr); err != nil {
			t.Errorf("%v processServiceCreateOrUpdate() %v", tc.testName, err)
		}
	}

}

// TestProcessServiceCreateOrUpdateK8sError tests processServiceCreateOrUpdate
// with various kubernetes errors when patching status.
func TestProcessServiceCreateOrUpdateK8sError(t *testing.T) {
	svcName := "svc-k8s-err"
	conflictErr := apierrors.NewConflict(schema.GroupResource{}, svcName, errors.New("object conflict"))
	notFoundErr := apierrors.NewNotFound(schema.GroupResource{}, svcName)

	testCases := []struct {
		desc      string
		k8sErr    error
		expectErr error
	}{
		{
			desc:      "conflict error",
			k8sErr:    conflictErr,
			expectErr: fmt.Errorf("failed to update load balancer status: %v", conflictErr),
		},
		{
			desc:      "not found error",
			k8sErr:    notFoundErr,
			expectErr: nil,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.desc, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			svc := newService(svcName, types.UID("123"), v1.ServiceTypeLoadBalancer)
			// Preset finalizer so k8s error only happens when patching status.
			svc.Finalizers = []string{servicehelper.LoadBalancerCleanupFinalizer}
			controller, _, client := newController()
			controller.serviceStore.Add(svc)

			client.PrependReactor("patch", "services", func(action core.Action) (bool, runtime.Object, error) {
				return true, nil, tc.k8sErr
			})

			key, _ := cache.MetaNamespaceKeyFunc(svc)
			if err := controller.syncService(ctx, key); !reflect.DeepEqual(err, tc.expectErr) {
				t.Fatalf("syncLoadBalancerIfNeeded() = %v, want %v", err, tc.expectErr)
			}
			if tc.expectErr == nil {
				return
			}

			errMsg := "Error syncing load balancer"
			if gotEvent := func() bool {
				events := controller.eventRecorder.(*record.FakeRecorder).Events
				for len(events) > 0 {
					e := <-events
					if strings.Contains(e, errMsg) {
						return true
					}
				}
				return false
			}(); !gotEvent {
				t.Errorf("processServiceCreateOrUpdate() = can't find sync error event, want event contains %q", errMsg)
			}
		})
	}

}

// Test cases:
// index    finalizer    timestamp    wantLB  |  clean-up
//
//	0         0           0            0     |   false    (No finalizer, no clean up)
//	1         0           0            1     |   false    (Ignored as same with case 0)
//	2         0           1            0     |   false    (Ignored as same with case 0)
//	3         0           1            1     |   false    (Ignored as same with case 0)
//	4         1           0            0     |   true
//	5         1           0            1     |   false
//	6         1           1            0     |   true    (Service is deleted, needs clean up)
//	7         1           1            1     |   true    (Ignored as same with case 6)
func TestNeedsCleanup(t *testing.T) {
	testCases := []struct {
		desc               string
		svc                *v1.Service
		expectNeedsCleanup bool
	}{
		{
			desc:               "service without finalizer",
			svc:                &v1.Service{},
			expectNeedsCleanup: false,
		},
		{
			desc: "service with finalizer without timestamp without LB",
			svc: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Finalizers: []string{servicehelper.LoadBalancerCleanupFinalizer},
				},
				Spec: v1.ServiceSpec{
					Type: v1.ServiceTypeNodePort,
				},
			},
			expectNeedsCleanup: true,
		},
		{
			desc: "service with finalizer without timestamp with LB",
			svc: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Finalizers: []string{servicehelper.LoadBalancerCleanupFinalizer},
				},
				Spec: v1.ServiceSpec{
					Type: v1.ServiceTypeLoadBalancer,
				},
			},
			expectNeedsCleanup: false,
		},
		{
			desc: "service with finalizer with timestamp",
			svc: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Finalizers: []string{servicehelper.LoadBalancerCleanupFinalizer},
					DeletionTimestamp: &metav1.Time{
						Time: time.Now(),
					},
				},
			},
			expectNeedsCleanup: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.desc, func(t *testing.T) {
			if gotNeedsCleanup := needsCleanup(tc.svc); gotNeedsCleanup != tc.expectNeedsCleanup {
				t.Errorf("needsCleanup() = %t, want %t", gotNeedsCleanup, tc.expectNeedsCleanup)
			}
		})
	}

}

func TestNeedsUpdate(t *testing.T) {

	var oldSvc, newSvc *v1.Service

	testCases := []struct {
		testName            string //Name of the test case
		updateFn            func() //Function to update the service object
		expectedNeedsUpdate bool   //needsupdate always returns bool

	}{
		{
			testName: "If the service type is changed from LoadBalancer to ClusterIP",
			updateFn: func() {
				oldSvc = defaultExternalService()
				newSvc = defaultExternalService()
				newSvc.Spec.Type = v1.ServiceTypeClusterIP
			},
			expectedNeedsUpdate: true,
		},
		{
			testName: "If the Ports are different",
			updateFn: func() {
				oldSvc = defaultExternalService()
				newSvc = defaultExternalService()
				oldSvc.Spec.Ports = []v1.ServicePort{
					{
						Port: 8000,
					},
					{
						Port: 9000,
					},
					{
						Port: 10000,
					},
				}
				newSvc.Spec.Ports = []v1.ServicePort{
					{
						Port: 8001,
					},
					{
						Port: 9001,
					},
					{
						Port: 10001,
					},
				}

			},
			expectedNeedsUpdate: true,
		},
		{
			testName: "If external ip counts are different",
			updateFn: func() {
				oldSvc = defaultExternalService()
				newSvc = defaultExternalService()
				oldSvc.Spec.ExternalIPs = []string{"old.IP.1"}
				newSvc.Spec.ExternalIPs = []string{"new.IP.1", "new.IP.2"}
			},
			expectedNeedsUpdate: true,
		},
		{
			testName: "If external ips are different",
			updateFn: func() {
				oldSvc = defaultExternalService()
				newSvc = defaultExternalService()
				oldSvc.Spec.ExternalIPs = []string{"old.IP.1", "old.IP.2"}
				newSvc.Spec.ExternalIPs = []string{"new.IP.1", "new.IP.2"}
			},
			expectedNeedsUpdate: true,
		},
		{
			testName: "If UID is different",
			updateFn: func() {
				oldSvc = defaultExternalService()
				newSvc = defaultExternalService()
				oldSvc.UID = types.UID("UID old")
				newSvc.UID = types.UID("UID new")
			},
			expectedNeedsUpdate: true,
		},
		{
			testName: "If ExternalTrafficPolicy is different",
			updateFn: func() {
				oldSvc = defaultExternalService()
				newSvc = defaultExternalService()
				newSvc.Spec.ExternalTrafficPolicy = v1.ServiceExternalTrafficPolicyLocal
			},
			expectedNeedsUpdate: true,
		},
		{
			testName: "If HealthCheckNodePort is different",
			updateFn: func() {
				oldSvc = defaultExternalService()
				newSvc = defaultExternalService()
				newSvc.Spec.HealthCheckNodePort = 30123
			},
			expectedNeedsUpdate: true,
		},
		{
			testName: "If TargetGroup is different 1",
			updateFn: func() {
				oldSvc = &v1.Service{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "tcp-service",
						Namespace: "default",
					},
					Spec: v1.ServiceSpec{
						Ports: []v1.ServicePort{{
							Port:       80,
							Protocol:   v1.ProtocolTCP,
							TargetPort: intstr.Parse("20"),
						}},
						Type: v1.ServiceTypeLoadBalancer,
					},
				}
				newSvc = oldSvc.DeepCopy()
				newSvc.Spec.Ports[0].TargetPort = intstr.Parse("21")
			},
			expectedNeedsUpdate: true,
		},
		{
			testName: "If TargetGroup is different 2",
			updateFn: func() {
				oldSvc = &v1.Service{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "tcp-service",
						Namespace: "default",
					},
					Spec: v1.ServiceSpec{
						Ports: []v1.ServicePort{{
							Port:       80,
							Protocol:   v1.ProtocolTCP,
							TargetPort: intstr.Parse("22"),
						}},
						Type: v1.ServiceTypeLoadBalancer,
					},
				}
				newSvc = oldSvc.DeepCopy()
				newSvc.Spec.Ports[0].TargetPort = intstr.Parse("dns")
			},
			expectedNeedsUpdate: true,
		},
		{
			testName: "If appProtocol is the same",
			updateFn: func() {
				oldSvc = &v1.Service{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "tcp-service",
						Namespace: "default",
					},
					Spec: v1.ServiceSpec{
						Ports: []v1.ServicePort{{
							Port:       80,
							Protocol:   v1.ProtocolTCP,
							TargetPort: intstr.Parse("22"),
						}},
						Type: v1.ServiceTypeLoadBalancer,
					},
				}
				newSvc = oldSvc.DeepCopy()
			},
			expectedNeedsUpdate: false,
		},
		{
			testName: "If appProtocol is set when previously unset",
			updateFn: func() {
				oldSvc = &v1.Service{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "tcp-service",
						Namespace: "default",
					},
					Spec: v1.ServiceSpec{
						Ports: []v1.ServicePort{{
							Port:       80,
							Protocol:   v1.ProtocolTCP,
							TargetPort: intstr.Parse("22"),
						}},
						Type: v1.ServiceTypeLoadBalancer,
					},
				}
				newSvc = oldSvc.DeepCopy()
				protocol := "http"
				newSvc.Spec.Ports[0].AppProtocol = &protocol
			},
			expectedNeedsUpdate: true,
		},
		{
			testName: "If appProtocol is set to a different value",
			updateFn: func() {
				protocol := "http"
				oldSvc = &v1.Service{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "tcp-service",
						Namespace: "default",
					},
					Spec: v1.ServiceSpec{
						Ports: []v1.ServicePort{{
							Port:        80,
							Protocol:    v1.ProtocolTCP,
							TargetPort:  intstr.Parse("22"),
							AppProtocol: &protocol,
						}},
						Type: v1.ServiceTypeLoadBalancer,
					},
				}
				newSvc = oldSvc.DeepCopy()
				newProtocol := "tcp"
				newSvc.Spec.Ports[0].AppProtocol = &newProtocol
			},
			expectedNeedsUpdate: true,
		},
	}

	controller, _, _ := newController()
	for _, tc := range testCases {
		tc.updateFn()
		obtainedResult := controller.needsUpdate(oldSvc, newSvc)
		if obtainedResult != tc.expectedNeedsUpdate {
			t.Errorf("%v needsUpdate() should have returned %v but returned %v", tc.testName, tc.expectedNeedsUpdate, obtainedResult)
		}
	}
}

// TODO(@MrHohn): Verify the end state when below issue is resolved:
// https://github.com/kubernetes/client-go/issues/607
func TestAddFinalizer(t *testing.T) {
	testCases := []struct {
		desc        string
		svc         *v1.Service
		expectPatch bool
	}{
		{
			desc: "no-op add finalizer",
			svc: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "test-patch-finalizer",
					Finalizers: []string{servicehelper.LoadBalancerCleanupFinalizer},
				},
			},
			expectPatch: false,
		},
		{
			desc: "add finalizer",
			svc: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-patch-finalizer",
				},
			},
			expectPatch: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.desc, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			c := fake.NewSimpleClientset()
			s := &Controller{
				kubeClient: c,
			}
			if _, err := s.kubeClient.CoreV1().Services(tc.svc.Namespace).Create(ctx, tc.svc, metav1.CreateOptions{}); err != nil {
				t.Fatalf("Failed to prepare service for testing: %v", err)
			}
			if err := s.addFinalizer(tc.svc); err != nil {
				t.Fatalf("addFinalizer() = %v, want nil", err)
			}
			patchActionFound := false
			for _, action := range c.Actions() {
				if action.Matches("patch", "services") {
					patchActionFound = true
				}
			}
			if patchActionFound != tc.expectPatch {
				t.Errorf("Got patchActionFound = %t, want %t", patchActionFound, tc.expectPatch)
			}
		})
	}
}

// TODO(@MrHohn): Verify the end state when below issue is resolved:
// https://github.com/kubernetes/client-go/issues/607
func TestRemoveFinalizer(t *testing.T) {
	testCases := []struct {
		desc        string
		svc         *v1.Service
		expectPatch bool
	}{
		{
			desc: "no-op remove finalizer",
			svc: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-patch-finalizer",
				},
			},
			expectPatch: false,
		},
		{
			desc: "remove finalizer",
			svc: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "test-patch-finalizer",
					Finalizers: []string{servicehelper.LoadBalancerCleanupFinalizer},
				},
			},
			expectPatch: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.desc, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			c := fake.NewSimpleClientset()
			s := &Controller{
				kubeClient: c,
			}
			if _, err := s.kubeClient.CoreV1().Services(tc.svc.Namespace).Create(ctx, tc.svc, metav1.CreateOptions{}); err != nil {
				t.Fatalf("Failed to prepare service for testing: %v", err)
			}
			if err := s.removeFinalizer(tc.svc); err != nil {
				t.Fatalf("removeFinalizer() = %v, want nil", err)
			}
			patchActionFound := false
			for _, action := range c.Actions() {
				if action.Matches("patch", "services") {
					patchActionFound = true
				}
			}
			if patchActionFound != tc.expectPatch {
				t.Errorf("Got patchActionFound = %t, want %t", patchActionFound, tc.expectPatch)
			}
		})
	}
}

// TODO(@MrHohn): Verify the end state when below issue is resolved:
// https://github.com/kubernetes/client-go/issues/607
func TestPatchStatus(t *testing.T) {
	testCases := []struct {
		desc        string
		svc         *v1.Service
		newStatus   *v1.LoadBalancerStatus
		expectPatch bool
	}{
		{
			desc: "no-op add status",
			svc: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-patch-status",
				},
				Status: v1.ServiceStatus{
					LoadBalancer: v1.LoadBalancerStatus{
						Ingress: []v1.LoadBalancerIngress{
							{IP: "8.8.8.8"},
						},
					},
				},
			},
			newStatus: &v1.LoadBalancerStatus{
				Ingress: []v1.LoadBalancerIngress{
					{IP: "8.8.8.8"},
				},
			},
			expectPatch: false,
		},
		{
			desc: "add status",
			svc: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-patch-status",
				},
				Status: v1.ServiceStatus{},
			},
			newStatus: &v1.LoadBalancerStatus{
				Ingress: []v1.LoadBalancerIngress{
					{IP: "8.8.8.8"},
				},
			},
			expectPatch: true,
		},
		{
			desc: "no-op clear status",
			svc: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-patch-status",
				},
				Status: v1.ServiceStatus{},
			},
			newStatus:   &v1.LoadBalancerStatus{},
			expectPatch: false,
		},
		{
			desc: "clear status",
			svc: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-patch-status",
				},
				Status: v1.ServiceStatus{
					LoadBalancer: v1.LoadBalancerStatus{
						Ingress: []v1.LoadBalancerIngress{
							{IP: "8.8.8.8"},
						},
					},
				},
			},
			newStatus:   &v1.LoadBalancerStatus{},
			expectPatch: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.desc, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			c := fake.NewSimpleClientset()
			s := &Controller{
				kubeClient: c,
			}
			if _, err := s.kubeClient.CoreV1().Services(tc.svc.Namespace).Create(ctx, tc.svc, metav1.CreateOptions{}); err != nil {
				t.Fatalf("Failed to prepare service for testing: %v", err)
			}
			if err := s.patchStatus(tc.svc, &tc.svc.Status.LoadBalancer, tc.newStatus); err != nil {
				t.Fatalf("patchStatus() = %v, want nil", err)
			}
			patchActionFound := false
			for _, action := range c.Actions() {
				if action.Matches("patch", "services") {
					patchActionFound = true
				}
			}
			if patchActionFound != tc.expectPatch {
				t.Errorf("Got patchActionFound = %t, want %t", patchActionFound, tc.expectPatch)
			}
		})
	}
}

func Test_respectsPredicates(t *testing.T) {
	tests := []struct {
		name string

		input *v1.Node
		want  bool
	}{
		{want: false, input: &v1.Node{}},
		{want: true, input: &v1.Node{Status: v1.NodeStatus{Conditions: []v1.NodeCondition{{Type: v1.NodeReady, Status: v1.ConditionTrue}}}}},
		{want: false, input: &v1.Node{Status: v1.NodeStatus{Conditions: []v1.NodeCondition{{Type: v1.NodeReady, Status: v1.ConditionFalse}}}}},
		{want: true, input: &v1.Node{Status: v1.NodeStatus{Conditions: []v1.NodeCondition{{Type: v1.NodeReady, Status: v1.ConditionTrue}}}, ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{}}}},
		{want: false, input: &v1.Node{Status: v1.NodeStatus{Conditions: []v1.NodeCondition{{Type: v1.NodeReady, Status: v1.ConditionTrue}}}, ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{v1.LabelNodeExcludeBalancers: ""}}}},

		{want: false, input: &v1.Node{Status: v1.NodeStatus{Conditions: []v1.NodeCondition{{Type: v1.NodeReady, Status: v1.ConditionTrue}}},
			Spec: v1.NodeSpec{Taints: []v1.Taint{{Key: ToBeDeletedTaint, Value: fmt.Sprint(time.Now().Unix()), Effect: v1.TaintEffectNoSchedule}}}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if result := respectsPredicates(tt.input, allNodePredicates...); result != tt.want {
				t.Errorf("matchesPredicates() = %v, want %v", result, tt.want)
			}
		})
	}
}

func TestListWithPredicate(t *testing.T) {
	fakeInformerFactory := informers.NewSharedInformerFactory(&fake.Clientset{}, 0*time.Second)
	var nodes []*v1.Node
	for i := 0; i < 5; i++ {
		var phase v1.NodePhase
		if i%2 == 0 {
			phase = v1.NodePending
		} else {
			phase = v1.NodeRunning
		}
		node := &v1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: fmt.Sprintf("node-%d", i),
			},
			Status: v1.NodeStatus{
				Phase: phase,
			},
		}
		nodes = append(nodes, node)
		fakeInformerFactory.Core().V1().Nodes().Informer().GetStore().Add(node)
	}

	tests := []struct {
		name      string
		predicate NodeConditionPredicate
		expect    []*v1.Node
	}{
		{
			name: "ListWithPredicate filter Running node",
			predicate: func(node *v1.Node) bool {
				return node.Status.Phase == v1.NodeRunning
			},
			expect: []*v1.Node{nodes[1], nodes[3]},
		},
		{
			name: "ListWithPredicate filter Pending node",
			predicate: func(node *v1.Node) bool {
				return node.Status.Phase == v1.NodePending
			},
			expect: []*v1.Node{nodes[0], nodes[2], nodes[4]},
		},
		{
			name: "ListWithPredicate filter Terminated node",
			predicate: func(node *v1.Node) bool {
				return node.Status.Phase == v1.NodeTerminated
			},
			expect: nil,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			get, err := listWithPredicates(fakeInformerFactory.Core().V1().Nodes().Lister(), test.predicate)
			sort.Slice(get, func(i, j int) bool {
				return get[i].Name < get[j].Name
			})
			if err != nil {
				t.Errorf("Error from ListWithPredicate: %v", err)
			} else if !reflect.DeepEqual(get, test.expect) {
				t.Errorf("Expect nodes %v, but got %v", test.expect, get)
			}
		})
	}
}

func Test_shouldSyncUpdatedNode_individualPredicates(t *testing.T) {
	testcases := []struct {
		name       string
		oldNode    *v1.Node
		newNode    *v1.Node
		shouldSync bool
	}{
		{
			name: "taint F->T",
			oldNode: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "node",
				},
				Spec: v1.NodeSpec{
					Taints: []v1.Taint{},
				},
				Status: v1.NodeStatus{
					Conditions: []v1.NodeCondition{
						{
							Type:   v1.NodeReady,
							Status: v1.ConditionTrue,
						},
					},
				},
			},
			newNode: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "node",
				},
				Spec: v1.NodeSpec{
					Taints: []v1.Taint{
						{
							Key: ToBeDeletedTaint,
						},
					},
				},
				Status: v1.NodeStatus{
					Conditions: []v1.NodeCondition{
						{
							Type:   v1.NodeReady,
							Status: v1.ConditionTrue,
						},
					},
				},
			},
			shouldSync: true,
		},
		{
			name: "taint T->F",
			oldNode: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "node",
				},
				Spec: v1.NodeSpec{
					Taints: []v1.Taint{
						{
							Key: ToBeDeletedTaint,
						},
					},
				},
				Status: v1.NodeStatus{
					Conditions: []v1.NodeCondition{
						{
							Type:   v1.NodeReady,
							Status: v1.ConditionTrue,
						},
					},
				},
			},
			newNode: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "node",
				},
				Spec: v1.NodeSpec{
					Taints: []v1.Taint{},
				},
				Status: v1.NodeStatus{
					Conditions: []v1.NodeCondition{
						{
							Type:   v1.NodeReady,
							Status: v1.ConditionTrue,
						},
					},
				},
			},
			shouldSync: true,
		},
		{
			name: "taint F->F",
			oldNode: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "node",
				},
				Spec: v1.NodeSpec{
					Taints: []v1.Taint{},
				},
				Status: v1.NodeStatus{
					Conditions: []v1.NodeCondition{
						{
							Type:   v1.NodeReady,
							Status: v1.ConditionTrue,
						},
					},
				},
			},
			newNode: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "node",
				},
				Spec: v1.NodeSpec{
					Taints: []v1.Taint{},
				},
				Status: v1.NodeStatus{
					Conditions: []v1.NodeCondition{
						{
							Type:   v1.NodeReady,
							Status: v1.ConditionTrue,
						},
					},
				},
			},
			shouldSync: false,
		},
		{
			name: "taint T->T",
			oldNode: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "node",
				},
				Spec: v1.NodeSpec{
					Taints: []v1.Taint{
						{
							Key: ToBeDeletedTaint,
						},
					},
				},
				Status: v1.NodeStatus{
					Conditions: []v1.NodeCondition{
						{
							Type:   v1.NodeReady,
							Status: v1.ConditionTrue,
						},
					},
				},
			},
			newNode: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "node",
				},
				Spec: v1.NodeSpec{
					Taints: []v1.Taint{
						{
							Key: ToBeDeletedTaint,
						},
					},
				},
				Status: v1.NodeStatus{
					Conditions: []v1.NodeCondition{
						{
							Type:   v1.NodeReady,
							Status: v1.ConditionTrue,
						},
					},
				},
			},
			shouldSync: false,
		},
		{
			name: "other taint F->T",
			oldNode: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "node",
				},
				Spec: v1.NodeSpec{
					Taints: []v1.Taint{},
				},
				Status: v1.NodeStatus{
					Conditions: []v1.NodeCondition{
						{
							Type:   v1.NodeReady,
							Status: v1.ConditionTrue,
						},
					},
				},
			},
			newNode: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "node",
				},
				Spec: v1.NodeSpec{
					Taints: []v1.Taint{
						{
							Key: "other",
						},
					},
				},
				Status: v1.NodeStatus{
					Conditions: []v1.NodeCondition{
						{
							Type:   v1.NodeReady,
							Status: v1.ConditionTrue,
						},
					},
				},
			},
			shouldSync: false,
		},
		{
			name: "other taint T->F",
			oldNode: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "node",
				},
				Spec: v1.NodeSpec{
					Taints: []v1.Taint{
						{
							Key: "other",
						},
					},
				},
				Status: v1.NodeStatus{
					Conditions: []v1.NodeCondition{
						{
							Type:   v1.NodeReady,
							Status: v1.ConditionTrue,
						},
					},
				},
			},
			newNode: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "node",
				},
				Spec: v1.NodeSpec{
					Taints: []v1.Taint{},
				},
				Status: v1.NodeStatus{
					Conditions: []v1.NodeCondition{
						{
							Type:   v1.NodeReady,
							Status: v1.ConditionTrue,
						},
					},
				},
			},
			shouldSync: false,
		},
		{
			name: "excluded F->T",
			oldNode: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "node",
					Labels: map[string]string{},
				},
				Status: v1.NodeStatus{
					Conditions: []v1.NodeCondition{
						{
							Type:   v1.NodeReady,
							Status: v1.ConditionTrue,
						},
					},
				},
			},
			newNode: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "node",
					Labels: map[string]string{
						v1.LabelNodeExcludeBalancers: "",
					},
				},
				Status: v1.NodeStatus{
					Conditions: []v1.NodeCondition{
						{
							Type:   v1.NodeReady,
							Status: v1.ConditionTrue,
						},
					},
				},
			},
			shouldSync: true,
		},
		{
			name: "excluded changed T->F",
			oldNode: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "node",
					Labels: map[string]string{
						v1.LabelNodeExcludeBalancers: "",
					},
				},
				Status: v1.NodeStatus{
					Conditions: []v1.NodeCondition{
						{
							Type:   v1.NodeReady,
							Status: v1.ConditionTrue,
						},
					},
				},
			},
			newNode: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "node",
					Labels: map[string]string{},
				},
				Status: v1.NodeStatus{
					Conditions: []v1.NodeCondition{
						{
							Type:   v1.NodeReady,
							Status: v1.ConditionTrue,
						},
					},
				},
			},
			shouldSync: true,
		},
		{
			name: "excluded changed T->T",
			oldNode: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "node",
					Labels: map[string]string{
						v1.LabelNodeExcludeBalancers: "",
					},
				},
				Status: v1.NodeStatus{
					Conditions: []v1.NodeCondition{
						{
							Type:   v1.NodeReady,
							Status: v1.ConditionTrue,
						},
					},
				},
			},
			newNode: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "node",
					Labels: map[string]string{
						v1.LabelNodeExcludeBalancers: "",
					},
				},
				Status: v1.NodeStatus{
					Conditions: []v1.NodeCondition{
						{
							Type:   v1.NodeReady,
							Status: v1.ConditionTrue,
						},
					},
				},
			},
			shouldSync: false,
		},
		{
			name: "excluded changed F->F",
			oldNode: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "node",
					Labels: map[string]string{},
				},
				Status: v1.NodeStatus{
					Conditions: []v1.NodeCondition{
						{
							Type:   v1.NodeReady,
							Status: v1.ConditionTrue,
						},
					},
				},
			},
			newNode: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "node",
					Labels: map[string]string{},
				},
				Status: v1.NodeStatus{
					Conditions: []v1.NodeCondition{
						{
							Type:   v1.NodeReady,
							Status: v1.ConditionTrue,
						},
					},
				},
			},
			shouldSync: false,
		},
		{
			name: "other label changed F->T",
			oldNode: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "node",
					Labels: map[string]string{},
				},
				Status: v1.NodeStatus{
					Conditions: []v1.NodeCondition{
						{
							Type:   v1.NodeReady,
							Status: v1.ConditionTrue,
						},
					},
				},
			},
			newNode: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "node",
					Labels: map[string]string{
						"other": "",
					},
				},
				Status: v1.NodeStatus{
					Conditions: []v1.NodeCondition{
						{
							Type:   v1.NodeReady,
							Status: v1.ConditionTrue,
						},
					},
				},
			},
			shouldSync: false,
		},
		{
			name: "other label changed T->F",
			oldNode: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "node",
					Labels: map[string]string{
						"other": "",
					},
				},
				Status: v1.NodeStatus{
					Conditions: []v1.NodeCondition{
						{
							Type:   v1.NodeReady,
							Status: v1.ConditionTrue,
						},
					},
				},
			},
			newNode: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "node",
					Labels: map[string]string{},
				},
				Status: v1.NodeStatus{
					Conditions: []v1.NodeCondition{
						{
							Type:   v1.NodeReady,
							Status: v1.ConditionTrue,
						},
					},
				},
			},
			shouldSync: false,
		},
		{
			name: "readiness changed F->T",
			oldNode: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "node",
				},
				Status: v1.NodeStatus{
					Conditions: []v1.NodeCondition{
						{
							Type:   v1.NodeReady,
							Status: v1.ConditionFalse,
						},
					},
				},
			},
			newNode: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "node",
				},
				Status: v1.NodeStatus{
					Conditions: []v1.NodeCondition{
						{
							Type:   v1.NodeReady,
							Status: v1.ConditionTrue,
						},
					},
				},
			},
			shouldSync: true,
		},
		{
			name: "readiness changed T->F",
			oldNode: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "node",
				},
				Status: v1.NodeStatus{
					Conditions: []v1.NodeCondition{
						{
							Type:   v1.NodeReady,
							Status: v1.ConditionTrue,
						},
					},
				},
			},
			newNode: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "node",
				},
				Status: v1.NodeStatus{
					Conditions: []v1.NodeCondition{
						{
							Type:   v1.NodeReady,
							Status: v1.ConditionFalse,
						},
					},
				},
			},
			shouldSync: true,
		},
		{
			name: "readiness changed T->T",
			oldNode: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "node",
				},
				Status: v1.NodeStatus{
					Conditions: []v1.NodeCondition{
						{
							Type:   v1.NodeReady,
							Status: v1.ConditionTrue,
						},
					},
				},
			},
			newNode: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "node",
				},
				Status: v1.NodeStatus{
					Conditions: []v1.NodeCondition{
						{
							Type:   v1.NodeReady,
							Status: v1.ConditionTrue,
						},
					},
				},
			},
			shouldSync: false,
		},
		{
			name: "readiness changed F->F",
			oldNode: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "node",
				},
				Status: v1.NodeStatus{
					Conditions: []v1.NodeCondition{
						{
							Type:   v1.NodeReady,
							Status: v1.ConditionFalse,
						},
					},
				},
			},
			newNode: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "node",
				},
				Status: v1.NodeStatus{
					Conditions: []v1.NodeCondition{
						{
							Type:   v1.NodeReady,
							Status: v1.ConditionFalse,
						},
					},
				},
			},
			shouldSync: false,
		},
		{
			name: "readiness changed F->unset",
			oldNode: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "node",
				},
				Status: v1.NodeStatus{
					Conditions: []v1.NodeCondition{
						{
							Type:   v1.NodeReady,
							Status: v1.ConditionFalse,
						},
					},
				},
			},
			newNode: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "node",
				},
				Status: v1.NodeStatus{
					Conditions: []v1.NodeCondition{},
				},
			},
			shouldSync: false,
		},
		{
			name: "readiness changed T->unset",
			oldNode: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "node",
				},
				Status: v1.NodeStatus{
					Conditions: []v1.NodeCondition{
						{
							Type:   v1.NodeReady,
							Status: v1.ConditionTrue,
						},
					},
				},
			},
			newNode: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "node",
				},
				Status: v1.NodeStatus{
					Conditions: []v1.NodeCondition{},
				},
			},
			shouldSync: true,
		},
		{
			name: "readiness changed unset->F",
			oldNode: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "node",
				},
				Status: v1.NodeStatus{
					Conditions: []v1.NodeCondition{},
				},
			},
			newNode: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "node",
				},
				Status: v1.NodeStatus{
					Conditions: []v1.NodeCondition{
						{
							Type:   v1.NodeReady,
							Status: v1.ConditionFalse,
						},
					},
				},
			},
			shouldSync: false,
		},
		{
			name: "readiness changed unset->T",
			oldNode: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "node",
				},
				Status: v1.NodeStatus{
					Conditions: []v1.NodeCondition{},
				},
			},
			newNode: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "node",
				},
				Status: v1.NodeStatus{
					Conditions: []v1.NodeCondition{
						{
							Type:   v1.NodeReady,
							Status: v1.ConditionTrue,
						},
					},
				},
			},
			shouldSync: true,
		},
		{
			name: "readiness changed unset->unset",
			oldNode: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "node",
				},
				Status: v1.NodeStatus{
					Conditions: []v1.NodeCondition{},
				},
			},
			newNode: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "node",
				},
				Status: v1.NodeStatus{
					Conditions: []v1.NodeCondition{},
				},
			},
			shouldSync: false,
		},
		{
			name: "ready F, other condition changed F->T",
			oldNode: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "node",
				},
				Status: v1.NodeStatus{
					Conditions: []v1.NodeCondition{
						{
							Type:   v1.NodeDiskPressure,
							Status: v1.ConditionFalse,
						},
						{
							Type:   v1.NodeReady,
							Status: v1.ConditionFalse,
						},
					},
				},
			},
			newNode: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "node",
				},
				Status: v1.NodeStatus{
					Conditions: []v1.NodeCondition{
						{
							Type:   v1.NodeDiskPressure,
							Status: v1.ConditionTrue,
						},
						{
							Type:   v1.NodeReady,
							Status: v1.ConditionFalse,
						},
					},
				},
			},
			shouldSync: false,
		},
		{
			name: "ready F, other condition changed T->F",
			oldNode: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "node",
				},
				Status: v1.NodeStatus{
					Conditions: []v1.NodeCondition{
						{
							Type:   v1.NodeDiskPressure,
							Status: v1.ConditionTrue,
						},
						{
							Type:   v1.NodeReady,
							Status: v1.ConditionFalse,
						},
					},
				},
			},
			newNode: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "node",
				},
				Status: v1.NodeStatus{
					Conditions: []v1.NodeCondition{
						{
							Type:   v1.NodeDiskPressure,
							Status: v1.ConditionFalse,
						},
						{
							Type:   v1.NodeReady,
							Status: v1.ConditionFalse,
						},
					},
				},
			},
			shouldSync: false,
		},
		{
			name: "ready T, other condition changed F->T",
			oldNode: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "node",
				},
				Status: v1.NodeStatus{
					Conditions: []v1.NodeCondition{
						{
							Type:   v1.NodeDiskPressure,
							Status: v1.ConditionFalse,
						},
						{
							Type:   v1.NodeReady,
							Status: v1.ConditionTrue,
						},
					},
				},
			},
			newNode: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "node",
				},
				Status: v1.NodeStatus{
					Conditions: []v1.NodeCondition{
						{
							Type:   v1.NodeDiskPressure,
							Status: v1.ConditionTrue,
						},
						{
							Type:   v1.NodeReady,
							Status: v1.ConditionTrue,
						},
					},
				},
			},
			shouldSync: false,
		},
		{
			name: "ready T, other condition changed T->F",
			oldNode: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "node",
				},
				Status: v1.NodeStatus{
					Conditions: []v1.NodeCondition{
						{
							Type:   v1.NodeDiskPressure,
							Status: v1.ConditionTrue,
						},
						{
							Type:   v1.NodeReady,
							Status: v1.ConditionTrue,
						},
					},
				},
			},
			newNode: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "node",
				},
				Status: v1.NodeStatus{
					Conditions: []v1.NodeCondition{
						{
							Type:   v1.NodeDiskPressure,
							Status: v1.ConditionFalse,
						},
						{
							Type:   v1.NodeReady,
							Status: v1.ConditionTrue,
						},
					},
				},
			},
			shouldSync: false,
		},
	}
	for _, testcase := range testcases {
		t.Run(testcase.name, func(t *testing.T) {
			shouldSync := shouldSyncUpdatedNode(testcase.oldNode, testcase.newNode)
			if shouldSync != testcase.shouldSync {
				t.Errorf("unexpected result from shouldSyncNode, expected: %v, actual: %v", testcase.shouldSync, shouldSync)
			}
		})
	}
}

func Test_shouldSyncUpdatedNode_compoundedPredicates(t *testing.T) {
	testcases := []struct {
		name       string
		oldNode    *v1.Node
		newNode    *v1.Node
		shouldSync bool
	}{
		{
			name: "tainted T, excluded F->T",
			oldNode: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "node",
					Labels: map[string]string{},
				},
				Spec: v1.NodeSpec{
					Taints: []v1.Taint{
						{
							Key: ToBeDeletedTaint,
						},
					},
				},
				Status: v1.NodeStatus{
					Conditions: []v1.NodeCondition{
						{
							Type:   v1.NodeReady,
							Status: v1.ConditionTrue,
						},
					},
				},
			},
			newNode: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "node",
					Labels: map[string]string{
						v1.LabelNodeExcludeBalancers: "",
					},
				},
				Spec: v1.NodeSpec{
					Taints: []v1.Taint{
						{
							Key: ToBeDeletedTaint,
						},
					},
				},
				Status: v1.NodeStatus{
					Conditions: []v1.NodeCondition{
						{
							Type:   v1.NodeReady,
							Status: v1.ConditionTrue,
						},
					},
				},
			},
			shouldSync: true,
		},
		{
			name: "tainted T, excluded T->F",
			oldNode: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "node",
					Labels: map[string]string{
						v1.LabelNodeExcludeBalancers: "",
					},
				},
				Spec: v1.NodeSpec{
					Taints: []v1.Taint{
						{
							Key: ToBeDeletedTaint,
						},
					},
				},
				Status: v1.NodeStatus{
					Conditions: []v1.NodeCondition{
						{
							Type:   v1.NodeReady,
							Status: v1.ConditionTrue,
						},
					},
				},
			},
			newNode: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "node",
					Labels: map[string]string{},
				},
				Spec: v1.NodeSpec{
					Taints: []v1.Taint{
						{
							Key: ToBeDeletedTaint,
						},
					},
				},
				Status: v1.NodeStatus{
					Conditions: []v1.NodeCondition{
						{
							Type:   v1.NodeReady,
							Status: v1.ConditionTrue,
						},
					},
				},
			},
			shouldSync: true,
		},
		{
			name: "tainted T, ready F->T",
			oldNode: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "node",
				},
				Spec: v1.NodeSpec{
					Taints: []v1.Taint{
						{
							Key: ToBeDeletedTaint,
						},
					},
				},
				Status: v1.NodeStatus{
					Conditions: []v1.NodeCondition{
						{
							Type:   v1.NodeReady,
							Status: v1.ConditionFalse,
						},
					},
				},
			},
			newNode: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "node",
				},
				Spec: v1.NodeSpec{
					Taints: []v1.Taint{
						{
							Key: ToBeDeletedTaint,
						},
					},
				},
				Status: v1.NodeStatus{
					Conditions: []v1.NodeCondition{
						{
							Type:   v1.NodeReady,
							Status: v1.ConditionTrue,
						},
					},
				},
			},
			shouldSync: false,
		},
		{
			name: "tainted T, ready T->F",
			oldNode: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "node",
				},
				Spec: v1.NodeSpec{
					Taints: []v1.Taint{
						{
							Key: ToBeDeletedTaint,
						},
					},
				},
				Status: v1.NodeStatus{
					Conditions: []v1.NodeCondition{
						{
							Type:   v1.NodeReady,
							Status: v1.ConditionTrue,
						},
					},
				},
			},
			newNode: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "node",
				},
				Spec: v1.NodeSpec{
					Taints: []v1.Taint{
						{
							Key: ToBeDeletedTaint,
						},
					},
				},
				Status: v1.NodeStatus{
					Conditions: []v1.NodeCondition{
						{
							Type:   v1.NodeReady,
							Status: v1.ConditionFalse,
						},
					},
				},
			},
			shouldSync: false,
		},
		{
			name: "excluded T, tainted F->T",
			oldNode: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "node",
					Labels: map[string]string{
						v1.LabelNodeExcludeBalancers: "",
					},
				},
				Spec: v1.NodeSpec{
					Taints: []v1.Taint{},
				},
				Status: v1.NodeStatus{
					Conditions: []v1.NodeCondition{
						{
							Type:   v1.NodeReady,
							Status: v1.ConditionTrue,
						},
					},
				},
			},
			newNode: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "node",
					Labels: map[string]string{
						v1.LabelNodeExcludeBalancers: "",
					},
				},
				Spec: v1.NodeSpec{
					Taints: []v1.Taint{
						{
							Key: ToBeDeletedTaint,
						},
					},
				},
				Status: v1.NodeStatus{
					Conditions: []v1.NodeCondition{
						{
							Type:   v1.NodeReady,
							Status: v1.ConditionTrue,
						},
					},
				},
			},
			shouldSync: false,
		},
		{
			name: "excluded T, tainted T->F",
			oldNode: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "node",
					Labels: map[string]string{
						v1.LabelNodeExcludeBalancers: "",
					},
				},
				Spec: v1.NodeSpec{
					Taints: []v1.Taint{
						{
							Key: ToBeDeletedTaint,
						},
					},
				},
				Status: v1.NodeStatus{
					Conditions: []v1.NodeCondition{
						{
							Type:   v1.NodeReady,
							Status: v1.ConditionTrue,
						},
					},
				},
			},
			newNode: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "node",
					Labels: map[string]string{
						v1.LabelNodeExcludeBalancers: "",
					},
				},
				Spec: v1.NodeSpec{
					Taints: []v1.Taint{},
				},
				Status: v1.NodeStatus{
					Conditions: []v1.NodeCondition{
						{
							Type:   v1.NodeReady,
							Status: v1.ConditionTrue,
						},
					},
				},
			},
			shouldSync: false,
		},
		{
			name: "excluded T, ready F->T",
			oldNode: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "node",
					Labels: map[string]string{
						v1.LabelNodeExcludeBalancers: "",
					},
				},
				Status: v1.NodeStatus{
					Conditions: []v1.NodeCondition{
						{
							Type:   v1.NodeReady,
							Status: v1.ConditionFalse,
						},
					},
				},
			},
			newNode: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "node",
					Labels: map[string]string{
						v1.LabelNodeExcludeBalancers: "",
					},
				},
				Status: v1.NodeStatus{
					Conditions: []v1.NodeCondition{
						{
							Type:   v1.NodeReady,
							Status: v1.ConditionTrue,
						},
					},
				},
			},
			shouldSync: false,
		},
		{
			name: "excluded T, ready T->F",
			oldNode: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "node",
					Labels: map[string]string{
						v1.LabelNodeExcludeBalancers: "",
					},
				},
				Status: v1.NodeStatus{
					Conditions: []v1.NodeCondition{
						{
							Type:   v1.NodeReady,
							Status: v1.ConditionTrue,
						},
					},
				},
			},
			newNode: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "node",
					Labels: map[string]string{
						v1.LabelNodeExcludeBalancers: "",
					},
				},
				Status: v1.NodeStatus{
					Conditions: []v1.NodeCondition{
						{
							Type:   v1.NodeReady,
							Status: v1.ConditionFalse,
						},
					},
				},
			},
			shouldSync: false,
		},
		{
			name: "ready F, tainted F->T",
			oldNode: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "node",
				},
				Spec: v1.NodeSpec{
					Taints: []v1.Taint{},
				},
				Status: v1.NodeStatus{
					Conditions: []v1.NodeCondition{
						{
							Type:   v1.NodeReady,
							Status: v1.ConditionFalse,
						},
					},
				},
			},
			newNode: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "node",
				},
				Spec: v1.NodeSpec{
					Taints: []v1.Taint{
						{
							Key: ToBeDeletedTaint,
						},
					},
				},
				Status: v1.NodeStatus{
					Conditions: []v1.NodeCondition{
						{
							Type:   v1.NodeReady,
							Status: v1.ConditionFalse,
						},
					},
				},
			},
			shouldSync: false,
		},
		{
			name: "ready F, tainted T->F",
			oldNode: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "node",
				},
				Spec: v1.NodeSpec{
					Taints: []v1.Taint{
						{
							Key: ToBeDeletedTaint,
						},
					},
				},
				Status: v1.NodeStatus{
					Conditions: []v1.NodeCondition{
						{
							Type:   v1.NodeReady,
							Status: v1.ConditionFalse,
						},
					},
				},
			},
			newNode: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "node",
				},
				Spec: v1.NodeSpec{
					Taints: []v1.Taint{},
				},
				Status: v1.NodeStatus{
					Conditions: []v1.NodeCondition{
						{
							Type:   v1.NodeReady,
							Status: v1.ConditionFalse,
						},
					},
				},
			},
			shouldSync: false,
		},
		{
			name: "ready F, excluded F->T",
			oldNode: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "node",
					Labels: map[string]string{},
				},
				Status: v1.NodeStatus{
					Conditions: []v1.NodeCondition{
						{
							Type:   v1.NodeReady,
							Status: v1.ConditionFalse,
						},
					},
				},
			},
			newNode: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "node",
					Labels: map[string]string{
						v1.LabelNodeExcludeBalancers: "",
					},
				},
				Status: v1.NodeStatus{
					Conditions: []v1.NodeCondition{
						{
							Type:   v1.NodeReady,
							Status: v1.ConditionFalse,
						},
					},
				},
			},
			shouldSync: true,
		},
		{
			name: "ready F, excluded T->F",
			oldNode: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "node",
					Labels: map[string]string{
						v1.LabelNodeExcludeBalancers: "",
					},
				},
				Status: v1.NodeStatus{
					Conditions: []v1.NodeCondition{
						{
							Type:   v1.NodeReady,
							Status: v1.ConditionFalse,
						},
					},
				},
			},
			newNode: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "node",
					Labels: map[string]string{},
				},
				Status: v1.NodeStatus{
					Conditions: []v1.NodeCondition{
						{
							Type:   v1.NodeReady,
							Status: v1.ConditionFalse,
						},
					},
				},
			},
			shouldSync: true,
		},
	}
	for _, testcase := range testcases {
		t.Run(testcase.name, func(t *testing.T) {
			shouldSync := shouldSyncUpdatedNode(testcase.oldNode, testcase.newNode)
			if shouldSync != testcase.shouldSync {
				t.Errorf("unexpected result from shouldSyncNode, expected: %v, actual: %v", testcase.shouldSync, shouldSync)
			}
		})
	}
}
