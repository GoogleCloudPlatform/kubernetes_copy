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

package scheduler

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	v1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1beta1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	"k8s.io/klog/v2/ktesting"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic/dynamicinformer"
	dyfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"

	"k8s.io/kubernetes/pkg/features"
	"k8s.io/kubernetes/pkg/scheduler/backend/cache"
	"k8s.io/kubernetes/pkg/scheduler/backend/queue"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/nodeaffinity"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/nodename"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/nodeports"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/noderesources"
	st "k8s.io/kubernetes/pkg/scheduler/testing"
	"k8s.io/kubernetes/pkg/scheduler/util/assumecache"
)

func TestUpdatePodInCache(t *testing.T) {
	ttl := 10 * time.Second
	nodeName := "node"

	tests := []struct {
		name   string
		oldObj interface{}
		newObj interface{}
	}{
		{
			name:   "pod updated with the same UID",
			oldObj: withPodName(podWithPort("oldUID", nodeName, 80), "pod"),
			newObj: withPodName(podWithPort("oldUID", nodeName, 8080), "pod"),
		},
		{
			name:   "pod updated with different UIDs",
			oldObj: withPodName(podWithPort("oldUID", nodeName, 80), "pod"),
			newObj: withPodName(podWithPort("newUID", nodeName, 8080), "pod"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logger, ctx := ktesting.NewTestContext(t)
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			sched := &Scheduler{
				Cache:           cache.New(ctx, ttl),
				SchedulingQueue: queue.NewTestQueue(ctx, nil),
				logger:          logger,
			}
			sched.addPodToCache(tt.oldObj)
			sched.updatePodInCache(tt.oldObj, tt.newObj)

			if tt.oldObj.(*v1.Pod).UID != tt.newObj.(*v1.Pod).UID {
				if pod, err := sched.Cache.GetPod(tt.oldObj.(*v1.Pod)); err == nil {
					t.Errorf("Get pod UID %v from cache but it should not happen", pod.UID)
				}
			}
			pod, err := sched.Cache.GetPod(tt.newObj.(*v1.Pod))
			if err != nil {
				t.Errorf("Failed to get pod from scheduler: %v", err)
			}
			if pod.UID != tt.newObj.(*v1.Pod).UID {
				t.Errorf("Want pod UID %v, got %v", tt.newObj.(*v1.Pod).UID, pod.UID)
			}
		})
	}
}

func withPodName(pod *v1.Pod, name string) *v1.Pod {
	pod.Name = name
	return pod
}

func TestPreCheckForNode(t *testing.T) {
	cpu4 := map[v1.ResourceName]string{v1.ResourceCPU: "4"}
	cpu8 := map[v1.ResourceName]string{v1.ResourceCPU: "8"}
	cpu16 := map[v1.ResourceName]string{v1.ResourceCPU: "16"}
	tests := []struct {
		name               string
		nodeFn             func() *v1.Node
		existingPods, pods []*v1.Pod
		want               []bool
		qHintEnabled       bool
	}{
		{
			name: "regular node, pods with a single constraint",
			nodeFn: func() *v1.Node {
				return st.MakeNode().Name("fake-node").Label("hostname", "fake-node").Capacity(cpu8).Obj()
			},
			existingPods: []*v1.Pod{
				st.MakePod().Name("p").HostPort(80).Obj(),
			},
			pods: []*v1.Pod{
				st.MakePod().Name("p1").Req(cpu4).Obj(),
				st.MakePod().Name("p2").Req(cpu16).Obj(),
				st.MakePod().Name("p3").Req(cpu4).Req(cpu8).Obj(),
				st.MakePod().Name("p4").NodeAffinityIn("hostname", []string{"fake-node"}, st.NodeSelectorTypeMatchExpressions).Obj(),
				st.MakePod().Name("p5").NodeAffinityNotIn("hostname", []string{"fake-node"}).Obj(),
				st.MakePod().Name("p6").Obj(),
				st.MakePod().Name("p7").Node("invalid-node").Obj(),
				st.MakePod().Name("p8").HostPort(8080).Obj(),
				st.MakePod().Name("p9").HostPort(80).Obj(),
			},
			want: []bool{true, false, false, true, false, true, false, true, false},
		},
		{
			name: "no filtering when QHint is enabled",
			nodeFn: func() *v1.Node {
				return st.MakeNode().Name("fake-node").Label("hostname", "fake-node").Capacity(cpu8).Obj()
			},
			existingPods: []*v1.Pod{
				st.MakePod().Name("p").HostPort(80).Obj(),
			},
			pods: []*v1.Pod{
				st.MakePod().Name("p1").Req(cpu4).Obj(),
				st.MakePod().Name("p2").Req(cpu16).Obj(),
				st.MakePod().Name("p3").Req(cpu4).Req(cpu8).Obj(),
				st.MakePod().Name("p4").NodeAffinityIn("hostname", []string{"fake-node"}, st.NodeSelectorTypeMatchExpressions).Obj(),
				st.MakePod().Name("p5").NodeAffinityNotIn("hostname", []string{"fake-node"}).Obj(),
				st.MakePod().Name("p6").Obj(),
				st.MakePod().Name("p7").Node("invalid-node").Obj(),
				st.MakePod().Name("p8").HostPort(8080).Obj(),
				st.MakePod().Name("p9").HostPort(80).Obj(),
			},
			qHintEnabled: true,
			want:         []bool{true, true, true, true, true, true, true, true, true},
		},
		{
			name: "tainted node, pods with a single constraint",
			nodeFn: func() *v1.Node {
				node := st.MakeNode().Name("fake-node").Obj()
				node.Spec.Taints = []v1.Taint{
					{Key: "foo", Effect: v1.TaintEffectNoSchedule},
					{Key: "bar", Effect: v1.TaintEffectPreferNoSchedule},
				}
				return node
			},
			pods: []*v1.Pod{
				st.MakePod().Name("p1").Obj(),
				st.MakePod().Name("p2").Toleration("foo").Obj(),
				st.MakePod().Name("p3").Toleration("bar").Obj(),
				st.MakePod().Name("p4").Toleration("bar").Toleration("foo").Obj(),
			},
			want: []bool{false, true, false, true},
		},
		{
			name: "regular node, pods with multiple constraints",
			nodeFn: func() *v1.Node {
				return st.MakeNode().Name("fake-node").Label("hostname", "fake-node").Capacity(cpu8).Obj()
			},
			existingPods: []*v1.Pod{
				st.MakePod().Name("p").HostPort(80).Obj(),
			},
			pods: []*v1.Pod{
				st.MakePod().Name("p1").Req(cpu4).NodeAffinityNotIn("hostname", []string{"fake-node"}).Obj(),
				st.MakePod().Name("p2").Req(cpu16).NodeAffinityIn("hostname", []string{"fake-node"}, st.NodeSelectorTypeMatchExpressions).Obj(),
				st.MakePod().Name("p3").Req(cpu8).NodeAffinityIn("hostname", []string{"fake-node"}, st.NodeSelectorTypeMatchExpressions).Obj(),
				st.MakePod().Name("p4").HostPort(8080).Node("invalid-node").Obj(),
				st.MakePod().Name("p5").Req(cpu4).NodeAffinityIn("hostname", []string{"fake-node"}, st.NodeSelectorTypeMatchExpressions).HostPort(80).Obj(),
			},
			want: []bool{false, false, true, false, false},
		},
		{
			name: "tainted node, pods with multiple constraints",
			nodeFn: func() *v1.Node {
				node := st.MakeNode().Name("fake-node").Label("hostname", "fake-node").Capacity(cpu8).Obj()
				node.Spec.Taints = []v1.Taint{
					{Key: "foo", Effect: v1.TaintEffectNoSchedule},
					{Key: "bar", Effect: v1.TaintEffectPreferNoSchedule},
				}
				return node
			},
			pods: []*v1.Pod{
				st.MakePod().Name("p1").Req(cpu4).Toleration("bar").Obj(),
				st.MakePod().Name("p2").Req(cpu4).Toleration("bar").Toleration("foo").Obj(),
				st.MakePod().Name("p3").Req(cpu16).Toleration("foo").Obj(),
				st.MakePod().Name("p3").Req(cpu16).Toleration("bar").Obj(),
			},
			want: []bool{false, true, false, false},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.SchedulerQueueingHints, tt.qHintEnabled)

			nodeInfo := framework.NewNodeInfo(tt.existingPods...)
			nodeInfo.SetNode(tt.nodeFn())
			preCheckFn := preCheckForNode(nodeInfo)

			got := make([]bool, 0, len(tt.pods))
			for _, pod := range tt.pods {
				got = append(got, preCheckFn == nil || preCheckFn(pod))
			}

			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("Unexpected diff (-want, +got):\n%s", diff)
			}
		})
	}
}

// test for informers of resources we care about is registered
func TestAddAllEventHandlers(t *testing.T) {
	tests := []struct {
		name                   string
		gvkMap                 map[framework.EventResource]framework.ActionType
		enableDRA              bool
		expectStaticInformers  map[reflect.Type]bool
		expectDynamicInformers map[schema.GroupVersionResource]bool
	}{
		{
			name:   "default handlers in framework",
			gvkMap: map[framework.EventResource]framework.ActionType{},
			expectStaticInformers: map[reflect.Type]bool{
				reflect.TypeOf(&v1.Pod{}):       true,
				reflect.TypeOf(&v1.Node{}):      true,
				reflect.TypeOf(&v1.Namespace{}): true,
			},
			expectDynamicInformers: map[schema.GroupVersionResource]bool{},
		},
		{
			name: "DRA events disabled",
			gvkMap: map[framework.EventResource]framework.ActionType{
				framework.ResourceClaim: framework.Add,
				framework.ResourceSlice: framework.Add,
				framework.DeviceClass:   framework.Add,
			},
			expectStaticInformers: map[reflect.Type]bool{
				reflect.TypeOf(&v1.Pod{}):       true,
				reflect.TypeOf(&v1.Node{}):      true,
				reflect.TypeOf(&v1.Namespace{}): true,
			},
			expectDynamicInformers: map[schema.GroupVersionResource]bool{},
		},
		{
			name: "all DRA events enabled",
			gvkMap: map[framework.EventResource]framework.ActionType{
				framework.ResourceClaim: framework.Add,
				framework.ResourceSlice: framework.Add,
				framework.DeviceClass:   framework.Add,
			},
			enableDRA: true,
			expectStaticInformers: map[reflect.Type]bool{
				reflect.TypeOf(&v1.Pod{}):                    true,
				reflect.TypeOf(&v1.Node{}):                   true,
				reflect.TypeOf(&v1.Namespace{}):              true,
				reflect.TypeOf(&resourceapi.ResourceClaim{}): true,
				reflect.TypeOf(&resourceapi.ResourceSlice{}): true,
				reflect.TypeOf(&resourceapi.DeviceClass{}):   true,
			},
			expectDynamicInformers: map[schema.GroupVersionResource]bool{},
		},
		{
			name: "add GVKs handlers defined in framework dynamically",
			gvkMap: map[framework.EventResource]framework.ActionType{
				"Pod":                               framework.Add | framework.Delete,
				"PersistentVolume":                  framework.Delete,
				"storage.k8s.io/CSIStorageCapacity": framework.Update,
			},
			expectStaticInformers: map[reflect.Type]bool{
				reflect.TypeOf(&v1.Pod{}):                       true,
				reflect.TypeOf(&v1.Node{}):                      true,
				reflect.TypeOf(&v1.Namespace{}):                 true,
				reflect.TypeOf(&v1.PersistentVolume{}):          true,
				reflect.TypeOf(&storagev1.CSIStorageCapacity{}): true,
			},
			expectDynamicInformers: map[schema.GroupVersionResource]bool{},
		},
		{
			name: "add GVKs handlers defined in plugins dynamically",
			gvkMap: map[framework.EventResource]framework.ActionType{
				"daemonsets.v1.apps": framework.Add | framework.Delete,
				"cronjobs.v1.batch":  framework.Delete,
			},
			expectStaticInformers: map[reflect.Type]bool{
				reflect.TypeOf(&v1.Pod{}):       true,
				reflect.TypeOf(&v1.Node{}):      true,
				reflect.TypeOf(&v1.Namespace{}): true,
			},
			expectDynamicInformers: map[schema.GroupVersionResource]bool{
				{Group: "apps", Version: "v1", Resource: "daemonsets"}: true,
				{Group: "batch", Version: "v1", Resource: "cronjobs"}:  true,
			},
		},
		{
			name: "add GVKs handlers defined in plugins dynamically, with one illegal GVK form",
			gvkMap: map[framework.EventResource]framework.ActionType{
				"daemonsets.v1.apps":    framework.Add | framework.Delete,
				"custommetrics.v1beta1": framework.Update,
			},
			expectStaticInformers: map[reflect.Type]bool{
				reflect.TypeOf(&v1.Pod{}):       true,
				reflect.TypeOf(&v1.Node{}):      true,
				reflect.TypeOf(&v1.Namespace{}): true,
			},
			expectDynamicInformers: map[schema.GroupVersionResource]bool{
				{Group: "apps", Version: "v1", Resource: "daemonsets"}: true,
			},
		},
	}

	scheme := runtime.NewScheme()
	var localSchemeBuilder = runtime.SchemeBuilder{
		appsv1.AddToScheme,
		batchv1.AddToScheme,
	}
	localSchemeBuilder.AddToScheme(scheme)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.DynamicResourceAllocation, tt.enableDRA)

			logger, ctx := ktesting.NewTestContext(t)
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()

			informerFactory := informers.NewSharedInformerFactory(fake.NewClientset(), 0)
			schedulingQueue := queue.NewTestQueueWithInformerFactory(ctx, nil, informerFactory)
			testSched := Scheduler{
				StopEverything:  ctx.Done(),
				SchedulingQueue: schedulingQueue,
				logger:          logger,
			}

			dynclient := dyfake.NewSimpleDynamicClient(scheme)
			dynInformerFactory := dynamicinformer.NewDynamicSharedInformerFactory(dynclient, 0)
			var resourceClaimCache *assumecache.AssumeCache
			if utilfeature.DefaultFeatureGate.Enabled(features.DynamicResourceAllocation) {
				resourceClaimInformer := informerFactory.Resource().V1beta1().ResourceClaims().Informer()
				resourceClaimCache = assumecache.NewAssumeCache(logger, resourceClaimInformer, "ResourceClaim", "", nil)
			}

			if err := addAllEventHandlers(&testSched, informerFactory, dynInformerFactory, resourceClaimCache, tt.gvkMap); err != nil {
				t.Fatalf("Add event handlers failed, error = %v", err)
			}

			informerFactory.Start(testSched.StopEverything)
			dynInformerFactory.Start(testSched.StopEverything)
			staticInformers := informerFactory.WaitForCacheSync(testSched.StopEverything)
			dynamicInformers := dynInformerFactory.WaitForCacheSync(testSched.StopEverything)

			if diff := cmp.Diff(tt.expectStaticInformers, staticInformers); diff != "" {
				t.Errorf("Unexpected diff (-want, +got):\n%s", diff)
			}
			if diff := cmp.Diff(tt.expectDynamicInformers, dynamicInformers); diff != "" {
				t.Errorf("Unexpected diff (-want, +got):\n%s", diff)
			}
		})
	}
}

func TestAdmissionCheck(t *testing.T) {
	nodeaffinityError := AdmissionResult{Name: nodeaffinity.Name, Reason: nodeaffinity.ErrReasonPod}
	nodenameError := AdmissionResult{Name: nodename.Name, Reason: nodename.ErrReason}
	nodeportsError := AdmissionResult{Name: nodeports.Name, Reason: nodeports.ErrReason}
	podOverheadError := AdmissionResult{InsufficientResource: &noderesources.InsufficientResource{ResourceName: v1.ResourceCPU, Reason: "Insufficient cpu", Requested: 2000, Used: 7000, Capacity: 8000}}
	cpu := map[v1.ResourceName]string{v1.ResourceCPU: "8"}
	tests := []struct {
		name                 string
		node                 *v1.Node
		existingPods         []*v1.Pod
		pod                  *v1.Pod
		wantAdmissionResults [][]AdmissionResult
	}{
		{
			name: "check nodeAffinity and nodeports, nodeAffinity need fail quickly if includeAllFailures is false",
			node: st.MakeNode().Name("fake-node").Label("foo", "bar").Obj(),
			pod:  st.MakePod().Name("pod2").HostPort(80).NodeSelector(map[string]string{"foo": "bar1"}).Obj(),
			existingPods: []*v1.Pod{
				st.MakePod().Name("pod1").HostPort(80).Obj(),
			},
			wantAdmissionResults: [][]AdmissionResult{{nodeaffinityError, nodeportsError}, {nodeaffinityError}},
		},
		{
			name: "check PodOverhead and nodeAffinity, PodOverhead need fail quickly if includeAllFailures is false",
			node: st.MakeNode().Name("fake-node").Label("foo", "bar").Capacity(cpu).Obj(),
			pod:  st.MakePod().Name("pod2").Container("c").Overhead(v1.ResourceList{v1.ResourceCPU: resource.MustParse("1")}).Req(map[v1.ResourceName]string{v1.ResourceCPU: "1"}).NodeSelector(map[string]string{"foo": "bar1"}).Obj(),
			existingPods: []*v1.Pod{
				st.MakePod().Name("pod1").Req(map[v1.ResourceName]string{v1.ResourceCPU: "7"}).Node("fake-node").Obj(),
			},
			wantAdmissionResults: [][]AdmissionResult{{podOverheadError, nodeaffinityError}, {podOverheadError}},
		},
		{
			name: "check nodename and nodeports, nodename need fail quickly if includeAllFailures is false",
			node: st.MakeNode().Name("fake-node").Obj(),
			pod:  st.MakePod().Name("pod2").HostPort(80).Node("fake-node1").Obj(),
			existingPods: []*v1.Pod{
				st.MakePod().Name("pod1").HostPort(80).Node("fake-node").Obj(),
			},
			wantAdmissionResults: [][]AdmissionResult{{nodenameError, nodeportsError}, {nodenameError}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nodeInfo := framework.NewNodeInfo(tt.existingPods...)
			nodeInfo.SetNode(tt.node)

			flags := []bool{true, false}
			for i := range flags {
				admissionResults := AdmissionCheck(tt.pod, nodeInfo, flags[i])

				if diff := cmp.Diff(tt.wantAdmissionResults[i], admissionResults); diff != "" {
					t.Errorf("Unexpected admissionResults (-want, +got):\n%s", diff)
				}
			}
		})
	}
}
