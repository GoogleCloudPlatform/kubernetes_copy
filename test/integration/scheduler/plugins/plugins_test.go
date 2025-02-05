/*
Copyright 2018 The Kubernetes Authors.

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

// Package plugins contains functional tests for scheduler plugin support.
// Beware that the plugins in this directory are not meant to be used in
// performance tests because they don't behave like real plugins.
package plugins

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	clientset "k8s.io/client-go/kubernetes"
	listersv1 "k8s.io/client-go/listers/core/v1"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	corev1helpers "k8s.io/component-helpers/scheduling/corev1"
	configv1 "k8s.io/kube-scheduler/config/v1"
	"k8s.io/kubernetes/pkg/features"
	"k8s.io/kubernetes/pkg/scheduler"
	schedulerconfig "k8s.io/kubernetes/pkg/scheduler/apis/config"
	configtesting "k8s.io/kubernetes/pkg/scheduler/apis/config/testing"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/defaultbinder"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/noderesources"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/schedulinggates"
	frameworkruntime "k8s.io/kubernetes/pkg/scheduler/framework/runtime"
	st "k8s.io/kubernetes/pkg/scheduler/testing"
	schedulerutils "k8s.io/kubernetes/test/integration/scheduler"
	testutils "k8s.io/kubernetes/test/integration/util"
	imageutils "k8s.io/kubernetes/test/utils/image"
	"k8s.io/utils/pointer"
)

// imported from testutils
var (
	initRegistryAndConfig = func(t *testing.T, plugins ...framework.Plugin) (frameworkruntime.Registry, schedulerconfig.KubeSchedulerProfile) {
		return schedulerutils.InitRegistryAndConfig(t, newPlugin, plugins...)
	}
)

// newPlugin returns a plugin factory with specified Plugin.
func newPlugin(plugin framework.Plugin) frameworkruntime.PluginFactory {
	return func(_ context.Context, _ runtime.Object, fh framework.Handle) (framework.Plugin, error) {
		switch pl := plugin.(type) {
		case *PermitPlugin:
			pl.fh = fh
		case *PostFilterPlugin:
			pl.fh = fh
		}
		return plugin, nil
	}
}

type QueueSortPlugin struct {
	// lessFunc is used to compare two queued pod infos.
	lessFunc func(info1, info2 *framework.QueuedPodInfo) bool
}

type PreEnqueuePlugin struct {
	called int
	admit  bool
}

type PreFilterPlugin struct {
	numPreFilterCalled   int
	failPreFilter        bool
	rejectPreFilter      bool
	preFilterResultNodes sets.Set[string]
}

type ScorePlugin struct {
	mutex          sync.Mutex
	failScore      bool
	numScoreCalled int
	highScoreNode  string
}

func (sp *ScorePlugin) deepCopy() *ScorePlugin {
	sp.mutex.Lock()
	defer sp.mutex.Unlock()

	return &ScorePlugin{
		failScore:      sp.failScore,
		numScoreCalled: sp.numScoreCalled,
		highScoreNode:  sp.highScoreNode,
	}
}

type ScoreWithNormalizePlugin struct {
	mutex                   sync.Mutex
	numScoreCalled          int
	numNormalizeScoreCalled int
}

func (sp *ScoreWithNormalizePlugin) deepCopy() *ScoreWithNormalizePlugin {
	sp.mutex.Lock()
	defer sp.mutex.Unlock()

	return &ScoreWithNormalizePlugin{
		numScoreCalled:          sp.numScoreCalled,
		numNormalizeScoreCalled: sp.numNormalizeScoreCalled,
	}
}

type FilterPlugin struct {
	mutex           sync.Mutex
	numFilterCalled int
	failFilter      bool
	rejectFilter    bool

	numCalledPerPod map[string]int
}

func (fp *FilterPlugin) deepCopy() *FilterPlugin {
	fp.mutex.Lock()
	defer fp.mutex.Unlock()

	clone := &FilterPlugin{
		numFilterCalled: fp.numFilterCalled,
		failFilter:      fp.failFilter,
		rejectFilter:    fp.rejectFilter,
		numCalledPerPod: make(map[string]int),
	}
	for pod, counter := range fp.numCalledPerPod {
		clone.numCalledPerPod[pod] = counter
	}
	return clone
}

type PostFilterPlugin struct {
	name                string
	fh                  framework.Handle
	numPostFilterCalled int
	failPostFilter      bool
	rejectPostFilter    bool
	breakPostFilter     bool
}

type ReservePlugin struct {
	name                  string
	numReserveCalled      int
	failReserve           bool
	numUnreserveCalled    int
	pluginInvokeEventChan chan pluginInvokeEvent
}

type PreScorePlugin struct {
	numPreScoreCalled int
	failPreScore      bool
}

type PreBindPlugin struct {
	mutex            sync.Mutex
	numPreBindCalled int
	failPreBind      bool
	rejectPreBind    bool
	// If set to true, always succeed on non-first scheduling attempt.
	succeedOnRetry bool
	// Record the pod UIDs that have been tried scheduling.
	podUIDs map[types.UID]struct{}
}

func (pp *PreBindPlugin) set(fail, reject, succeed bool) {
	pp.mutex.Lock()
	defer pp.mutex.Unlock()

	pp.failPreBind = fail
	pp.rejectPreBind = reject
	pp.succeedOnRetry = succeed
}

func (pp *PreBindPlugin) deepCopy() *PreBindPlugin {
	pp.mutex.Lock()
	defer pp.mutex.Unlock()

	clone := &PreBindPlugin{
		numPreBindCalled: pp.numPreBindCalled,
		failPreBind:      pp.failPreBind,
		rejectPreBind:    pp.rejectPreBind,
		succeedOnRetry:   pp.succeedOnRetry,
		podUIDs:          make(map[types.UID]struct{}),
	}
	for uid := range pp.podUIDs {
		clone.podUIDs[uid] = struct{}{}
	}
	return clone
}

type BindPlugin struct {
	mutex                 sync.Mutex
	name                  string
	numBindCalled         int
	bindStatus            *framework.Status
	client                clientset.Interface
	pluginInvokeEventChan chan pluginInvokeEvent
}

func (bp *BindPlugin) deepCopy() *BindPlugin {
	bp.mutex.Lock()
	defer bp.mutex.Unlock()

	return &BindPlugin{
		name:                  bp.name,
		numBindCalled:         bp.numBindCalled,
		bindStatus:            bp.bindStatus,
		client:                bp.client,
		pluginInvokeEventChan: bp.pluginInvokeEventChan,
	}
}

type PostBindPlugin struct {
	mutex                 sync.Mutex
	name                  string
	numPostBindCalled     int
	pluginInvokeEventChan chan pluginInvokeEvent
}

func (pp *PostBindPlugin) deepCopy() *PostBindPlugin {
	pp.mutex.Lock()
	defer pp.mutex.Unlock()

	return &PostBindPlugin{
		name:                  pp.name,
		numPostBindCalled:     pp.numPostBindCalled,
		pluginInvokeEventChan: pp.pluginInvokeEventChan,
	}
}

type PermitPlugin struct {
	mutex               sync.Mutex
	name                string
	numPermitCalled     int
	failPermit          bool
	rejectPermit        bool
	timeoutPermit       bool
	waitAndRejectPermit bool
	waitAndAllowPermit  bool
	cancelled           bool
	waitingPod          string
	rejectingPod        string
	allowingPod         string
	fh                  framework.Handle
}

func (pp *PermitPlugin) deepCopy() *PermitPlugin {
	pp.mutex.Lock()
	defer pp.mutex.Unlock()

	return &PermitPlugin{
		name:                pp.name,
		numPermitCalled:     pp.numPermitCalled,
		failPermit:          pp.failPermit,
		rejectPermit:        pp.rejectPermit,
		timeoutPermit:       pp.timeoutPermit,
		waitAndRejectPermit: pp.waitAndRejectPermit,
		waitAndAllowPermit:  pp.waitAndAllowPermit,
		cancelled:           pp.cancelled,
		waitingPod:          pp.waitingPod,
		rejectingPod:        pp.rejectingPod,
		allowingPod:         pp.allowingPod,
		fh:                  pp.fh,
	}
}

const (
	queuesortPluginName          = "queuesort-plugin"
	enqueuePluginName            = "enqueue-plugin"
	prefilterPluginName          = "prefilter-plugin"
	postfilterPluginName         = "postfilter-plugin"
	scorePluginName              = "score-plugin"
	scoreWithNormalizePluginName = "score-with-normalize-plugin"
	filterPluginName             = "filter-plugin"
	preScorePluginName           = "prescore-plugin"
	reservePluginName            = "reserve-plugin"
	preBindPluginName            = "prebind-plugin"
	postBindPluginName           = "postbind-plugin"
	permitPluginName             = "permit-plugin"
)

var _ framework.PreEnqueuePlugin = &PreEnqueuePlugin{}
var _ framework.PreFilterPlugin = &PreFilterPlugin{}
var _ framework.PostFilterPlugin = &PostFilterPlugin{}
var _ framework.ScorePlugin = &ScorePlugin{}
var _ framework.FilterPlugin = &FilterPlugin{}
var _ framework.ScorePlugin = &ScorePlugin{}
var _ framework.ScorePlugin = &ScoreWithNormalizePlugin{}
var _ framework.ReservePlugin = &ReservePlugin{}
var _ framework.PreScorePlugin = &PreScorePlugin{}
var _ framework.PreBindPlugin = &PreBindPlugin{}
var _ framework.BindPlugin = &BindPlugin{}
var _ framework.PostBindPlugin = &PostBindPlugin{}
var _ framework.PermitPlugin = &PermitPlugin{}
var _ framework.QueueSortPlugin = &QueueSortPlugin{}

func (ep *QueueSortPlugin) Name() string {
	return queuesortPluginName
}

func (ep *QueueSortPlugin) Less(info1, info2 *framework.QueuedPodInfo) bool {
	if ep.lessFunc != nil {
		return ep.lessFunc(info1, info2)
	}
	// If no custom less function is provided, default to return true.
	return true
}

func NewQueueSortPlugin(lessFunc func(info1, info2 *framework.QueuedPodInfo) bool) *QueueSortPlugin {
	return &QueueSortPlugin{
		lessFunc: lessFunc,
	}
}

func (ep *PreEnqueuePlugin) Name() string {
	return enqueuePluginName
}

func (ep *PreEnqueuePlugin) PreEnqueue(ctx context.Context, p *v1.Pod) *framework.Status {
	ep.called++
	if ep.admit {
		return nil
	}
	return framework.NewStatus(framework.UnschedulableAndUnresolvable, "not ready for scheduling")
}

// Name returns name of the score plugin.
func (sp *ScorePlugin) Name() string {
	return scorePluginName
}

// Score returns the score of scheduling a pod on a specific node.
func (sp *ScorePlugin) Score(ctx context.Context, state *framework.CycleState, p *v1.Pod, nodeName string) (int64, *framework.Status) {
	sp.mutex.Lock()
	defer sp.mutex.Unlock()

	sp.numScoreCalled++
	if sp.failScore {
		return 0, framework.NewStatus(framework.Error, fmt.Sprintf("injecting failure for pod %v", p.Name))
	}

	score := int64(1)
	if sp.numScoreCalled == 1 {
		// The first node is scored the highest, the rest is scored lower.
		sp.highScoreNode = nodeName
		score = framework.MaxNodeScore
	}
	return score, nil
}

func (sp *ScorePlugin) ScoreExtensions() framework.ScoreExtensions {
	return nil
}

// Name returns name of the score plugin.
func (sp *ScoreWithNormalizePlugin) Name() string {
	return scoreWithNormalizePluginName
}

// Score returns the score of scheduling a pod on a specific node.
func (sp *ScoreWithNormalizePlugin) Score(ctx context.Context, state *framework.CycleState, p *v1.Pod, nodeName string) (int64, *framework.Status) {
	sp.mutex.Lock()
	defer sp.mutex.Unlock()

	sp.numScoreCalled++
	score := int64(10)
	return score, nil
}

func (sp *ScoreWithNormalizePlugin) NormalizeScore(ctx context.Context, state *framework.CycleState, pod *v1.Pod, scores framework.NodeScoreList) *framework.Status {
	sp.numNormalizeScoreCalled++
	return nil
}

func (sp *ScoreWithNormalizePlugin) ScoreExtensions() framework.ScoreExtensions {
	return sp
}

// Name returns name of the plugin.
func (fp *FilterPlugin) Name() string {
	return filterPluginName
}

// Filter is a test function that returns an error or nil, depending on the
// value of "failFilter".
func (fp *FilterPlugin) Filter(ctx context.Context, state *framework.CycleState, pod *v1.Pod, nodeInfo *framework.NodeInfo) *framework.Status {
	fp.mutex.Lock()
	defer fp.mutex.Unlock()

	fp.numFilterCalled++
	if fp.numCalledPerPod != nil {
		fp.numCalledPerPod[fmt.Sprintf("%v/%v", pod.Namespace, pod.Name)]++
	}

	if fp.failFilter {
		return framework.NewStatus(framework.Error, fmt.Sprintf("injecting failure for pod %v", pod.Name))
	}
	if fp.rejectFilter {
		return framework.NewStatus(framework.Unschedulable, fmt.Sprintf("reject pod %v", pod.Name))
	}

	return nil
}

// Name returns name of the plugin.
func (rp *ReservePlugin) Name() string {
	return rp.name
}

// Reserve is a test function that increments an intenral counter and returns
// an error or nil, depending on the value of "failReserve".
func (rp *ReservePlugin) Reserve(ctx context.Context, state *framework.CycleState, pod *v1.Pod, nodeName string) *framework.Status {
	rp.numReserveCalled++
	if rp.failReserve {
		return framework.NewStatus(framework.Unschedulable, fmt.Sprintf("injecting failure for pod %v", pod.Name))
	}
	return nil
}

// Unreserve is a test function that increments an internal counter and emits
// an event to a channel. While Unreserve implementations should normally be
// idempotent, we relax that requirement here for testing purposes.
func (rp *ReservePlugin) Unreserve(ctx context.Context, state *framework.CycleState, pod *v1.Pod, nodeName string) {
	rp.numUnreserveCalled++
	if rp.pluginInvokeEventChan != nil {
		select {
		case <-ctx.Done():
		case rp.pluginInvokeEventChan <- pluginInvokeEvent{pluginName: rp.Name(), val: rp.numUnreserveCalled}:
		}
	}
}

// Name returns name of the plugin.
func (*PreScorePlugin) Name() string {
	return preScorePluginName
}

// PreScore is a test function.
func (pfp *PreScorePlugin) PreScore(ctx context.Context, _ *framework.CycleState, pod *v1.Pod, _ []*framework.NodeInfo) *framework.Status {
	pfp.numPreScoreCalled++
	if pfp.failPreScore {
		return framework.NewStatus(framework.Error, fmt.Sprintf("injecting failure for pod %v", pod.Name))
	}

	return nil
}

// Name returns name of the plugin.
func (pp *PreBindPlugin) Name() string {
	return preBindPluginName
}

// PreBind is a test function that returns (true, nil) or errors for testing.
func (pp *PreBindPlugin) PreBind(ctx context.Context, state *framework.CycleState, pod *v1.Pod, nodeName string) *framework.Status {
	pp.mutex.Lock()
	defer pp.mutex.Unlock()

	pp.numPreBindCalled++
	if _, tried := pp.podUIDs[pod.UID]; tried && pp.succeedOnRetry {
		return nil
	}
	pp.podUIDs[pod.UID] = struct{}{}
	if pp.failPreBind {
		return framework.NewStatus(framework.Error, fmt.Sprintf("injecting failure for pod %v", pod.Name))
	}
	if pp.rejectPreBind {
		return framework.NewStatus(framework.Unschedulable, fmt.Sprintf("reject pod %v", pod.Name))
	}
	return nil
}

const bindPluginAnnotation = "bindPluginName"

func (bp *BindPlugin) Name() string {
	return bp.name
}

func (bp *BindPlugin) Bind(ctx context.Context, state *framework.CycleState, p *v1.Pod, nodeName string) *framework.Status {
	bp.mutex.Lock()
	defer bp.mutex.Unlock()

	bp.numBindCalled++
	if bp.pluginInvokeEventChan != nil {
		select {
		case <-ctx.Done():
		case bp.pluginInvokeEventChan <- pluginInvokeEvent{pluginName: bp.Name(), val: bp.numBindCalled}:
		}
	}
	if bp.bindStatus.IsSuccess() {
		if err := bp.client.CoreV1().Pods(p.Namespace).Bind(ctx, &v1.Binding{
			ObjectMeta: metav1.ObjectMeta{Namespace: p.Namespace, Name: p.Name, UID: p.UID, Annotations: map[string]string{bindPluginAnnotation: bp.Name()}},
			Target: v1.ObjectReference{
				Kind: "Node",
				Name: nodeName,
			},
		}, metav1.CreateOptions{}); err != nil {
			return framework.NewStatus(framework.Error, fmt.Sprintf("bind failed: %v", err))
		}
	}
	return bp.bindStatus
}

// Name returns name of the plugin.
func (pp *PostBindPlugin) Name() string {
	return pp.name
}

// PostBind is a test function, which counts the number of times called.
func (pp *PostBindPlugin) PostBind(ctx context.Context, state *framework.CycleState, pod *v1.Pod, nodeName string) {
	pp.mutex.Lock()
	defer pp.mutex.Unlock()

	pp.numPostBindCalled++
	if pp.pluginInvokeEventChan != nil {
		select {
		case <-ctx.Done():
		case pp.pluginInvokeEventChan <- pluginInvokeEvent{pluginName: pp.Name(), val: pp.numPostBindCalled}:
		}
	}
}

// Name returns name of the plugin.
func (pp *PreFilterPlugin) Name() string {
	return prefilterPluginName
}

// Extensions returns the PreFilterExtensions interface.
func (pp *PreFilterPlugin) PreFilterExtensions() framework.PreFilterExtensions {
	return nil
}

// PreFilter is a test function that returns (true, nil) or errors for testing.
func (pp *PreFilterPlugin) PreFilter(ctx context.Context, state *framework.CycleState, pod *v1.Pod) (*framework.PreFilterResult, *framework.Status) {
	pp.numPreFilterCalled++
	if pp.failPreFilter {
		return nil, framework.NewStatus(framework.Error, fmt.Sprintf("injecting failure for pod %v", pod.Name))
	}
	if pp.rejectPreFilter {
		return nil, framework.NewStatus(framework.Unschedulable, fmt.Sprintf("reject pod %v", pod.Name))
	}
	if len(pp.preFilterResultNodes) != 0 {
		return &framework.PreFilterResult{NodeNames: pp.preFilterResultNodes}, nil
	}
	return nil, nil
}

// Name returns name of the plugin.
func (pp *PostFilterPlugin) Name() string {
	return pp.name
}

func (pp *PostFilterPlugin) PostFilter(ctx context.Context, state *framework.CycleState, pod *v1.Pod, _ framework.NodeToStatusReader) (*framework.PostFilterResult, *framework.Status) {
	pp.numPostFilterCalled++
	nodeInfos, err := pp.fh.SnapshotSharedLister().NodeInfos().List()
	if err != nil {
		return nil, framework.NewStatus(framework.Error, err.Error())
	}

	for _, nodeInfo := range nodeInfos {
		pp.fh.RunFilterPlugins(ctx, state, pod, nodeInfo)
	}
	pp.fh.RunScorePlugins(ctx, state, pod, nodeInfos)

	if pp.failPostFilter {
		return nil, framework.NewStatus(framework.Error, fmt.Sprintf("injecting failure for pod %v", pod.Name))
	}
	if pp.rejectPostFilter {
		return nil, framework.NewStatus(framework.Unschedulable, fmt.Sprintf("injecting unschedulable for pod %v", pod.Name))
	}
	if pp.breakPostFilter {
		return nil, framework.NewStatus(framework.UnschedulableAndUnresolvable, fmt.Sprintf("injecting unresolvable for pod %v", pod.Name))
	}

	return nil, framework.NewStatus(framework.Success, fmt.Sprintf("make room for pod %v to be schedulable", pod.Name))
}

// Name returns name of the plugin.
func (pp *PermitPlugin) Name() string {
	return pp.name
}

// Permit implements the permit test plugin.
func (pp *PermitPlugin) Permit(ctx context.Context, state *framework.CycleState, pod *v1.Pod, nodeName string) (*framework.Status, time.Duration) {
	pp.mutex.Lock()
	defer pp.mutex.Unlock()

	pp.numPermitCalled++
	if pp.failPermit {
		return framework.NewStatus(framework.Error, fmt.Sprintf("injecting failure for pod %v", pod.Name)), 0
	}
	if pp.rejectPermit {
		return framework.NewStatus(framework.Unschedulable, fmt.Sprintf("reject pod %v", pod.Name)), 0
	}
	if pp.timeoutPermit {
		go func() {
			select {
			case <-ctx.Done():
				pp.mutex.Lock()
				defer pp.mutex.Unlock()
				pp.cancelled = true
			}
		}()
		return framework.NewStatus(framework.Wait, ""), 3 * time.Second
	}
	if pp.waitAndRejectPermit || pp.waitAndAllowPermit {
		if pp.waitingPod == "" || pp.waitingPod == pod.Name {
			pp.waitingPod = pod.Name
			return framework.NewStatus(framework.Wait, ""), 30 * time.Second
		}
		if pp.waitAndRejectPermit {
			pp.rejectingPod = pod.Name
			pp.fh.IterateOverWaitingPods(func(wp framework.WaitingPod) {
				wp.Reject(pp.name, fmt.Sprintf("reject pod %v", wp.GetPod().Name))
			})
			return framework.NewStatus(framework.Unschedulable, fmt.Sprintf("reject pod %v", pod.Name)), 0
		}
		if pp.waitAndAllowPermit {
			pp.allowingPod = pod.Name
			pp.allowAllPods()
			return nil, 0
		}
	}
	return nil, 0
}

// allowAllPods allows all waiting pods.
func (pp *PermitPlugin) allowAllPods() {
	pp.fh.IterateOverWaitingPods(func(wp framework.WaitingPod) { wp.Allow(pp.name) })
}

// rejectAllPods rejects all waiting pods.
func (pp *PermitPlugin) rejectAllPods() {
	pp.mutex.Lock()
	defer pp.mutex.Unlock()
	pp.fh.IterateOverWaitingPods(func(wp framework.WaitingPod) { wp.Reject(pp.name, "rejectAllPods") })
}

// TestPreFilterPlugin tests invocation of prefilter plugins.
func TestPreFilterPlugin(t *testing.T) {
	testContext := testutils.InitTestAPIServer(t, "prefilter-plugin", nil)

	tests := []struct {
		name                 string
		fail                 bool
		reject               bool
		preFilterResultNodes sets.Set[string]
	}{
		{
			name:   "disable fail and reject flags",
			fail:   false,
			reject: false,
		},
		{
			name:   "enable fail and disable reject flags",
			fail:   true,
			reject: false,
		},
		{
			name:   "disable fail and enable reject flags",
			fail:   false,
			reject: true,
		},
		{
			name:                 "inject legal node names in PreFilterResult",
			fail:                 false,
			reject:               false,
			preFilterResultNodes: sets.New[string]("test-node-0", "test-node-1"),
		},
		{
			name:                 "inject legal and illegal node names in PreFilterResult",
			fail:                 false,
			reject:               false,
			preFilterResultNodes: sets.New[string]("test-node-0", "non-existent-node"),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// Create a plugin registry for testing. Register only a pre-filter plugin.
			preFilterPlugin := &PreFilterPlugin{}
			registry, prof := initRegistryAndConfig(t, preFilterPlugin)

			testCtx, teardown := schedulerutils.InitTestSchedulerForFrameworkTest(t, testContext, 2, true,
				scheduler.WithProfiles(prof),
				scheduler.WithFrameworkOutOfTreeRegistry(registry))
			defer teardown()

			preFilterPlugin.failPreFilter = test.fail
			preFilterPlugin.rejectPreFilter = test.reject
			preFilterPlugin.preFilterResultNodes = test.preFilterResultNodes
			// Create a best effort pod.
			pod, err := testutils.CreatePausePod(testCtx.ClientSet,
				testutils.InitPausePod(&testutils.PausePodConfig{Name: "test-pod", Namespace: testCtx.NS.Name}))
			if err != nil {
				t.Errorf("Error while creating a test pod: %v", err)
			}

			if test.reject {
				if err = testutils.WaitForPodUnschedulable(testCtx.Ctx, testCtx.ClientSet, pod); err != nil {
					t.Errorf("Didn't expect the pod to be scheduled. error: %v", err)
				}
			} else if test.fail {
				if err = wait.PollUntilContextTimeout(testCtx.Ctx, 10*time.Millisecond, 30*time.Second, false,
					testutils.PodSchedulingError(testCtx.ClientSet, pod.Namespace, pod.Name)); err != nil {
					t.Errorf("Expected a scheduling error, but got: %v", err)
				}
			} else {
				if err = testutils.WaitForPodToSchedule(testCtx.Ctx, testCtx.ClientSet, pod); err != nil {
					t.Errorf("Expected the pod to be scheduled. error: %v", err)
				}
			}

			if preFilterPlugin.numPreFilterCalled == 0 {
				t.Errorf("Expected the prefilter plugin to be called.")
			}
		})
	}
}

// TestQueueSortPlugin tests invocation of queueSort plugins.
func TestQueueSortPlugin(t *testing.T) {
	tests := []struct {
		name           string
		podNames       []string
		expectedOrder  []string
		customLessFunc func(info1, info2 *framework.QueuedPodInfo) bool
	}{
		{
			name:          "timestamp_sort_order",
			podNames:      []string{"pod-1", "pod-2", "pod-3"},
			expectedOrder: []string{"pod-1", "pod-2", "pod-3"},
			customLessFunc: func(info1, info2 *framework.QueuedPodInfo) bool {
				return info1.Timestamp.Before(info2.Timestamp)
			},
		},
		{
			name:          "priority_sort_order",
			podNames:      []string{"pod-1", "pod-2", "pod-3"},
			expectedOrder: []string{"pod-3", "pod-2", "pod-1"}, // depends on pod priority
			customLessFunc: func(info1, info2 *framework.QueuedPodInfo) bool {
				p1 := corev1helpers.PodPriority(info1.Pod)
				p2 := corev1helpers.PodPriority(info2.Pod)
				return (p1 > p2) || (p1 == p2 && info1.Timestamp.Before(info2.Timestamp))
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			queueSortPlugin := NewQueueSortPlugin(tt.customLessFunc)
			registry, prof := initRegistryAndConfig(t, queueSortPlugin)

			testCtx, teardown := schedulerutils.InitTestSchedulerForFrameworkTest(t, testutils.InitTestAPIServer(t, "queuesort-plugin", nil), 2, false,
				scheduler.WithProfiles(prof),
				scheduler.WithFrameworkOutOfTreeRegistry(registry))
			defer teardown()

			pods := make([]*v1.Pod, 0, len(tt.podNames))
			for i, name := range tt.podNames {
				// Create a pod with different priority.
				priority := int32(i + 1)
				pod, err := testutils.CreatePausePod(testCtx.ClientSet,
					testutils.InitPausePod(&testutils.PausePodConfig{Name: name, Namespace: testCtx.NS.Name, Priority: &priority}))
				if err != nil {
					t.Fatalf("Error while creating %v: %v", name, err)
				}
				pods = append(pods, pod)
			}

			// Wait for all Pods to be in the scheduling queue.
			err := wait.PollUntilContextTimeout(testCtx.Ctx, time.Millisecond*200, wait.ForeverTestTimeout, false, func(ctx context.Context) (bool, error) {
				pendingPods, _ := testCtx.Scheduler.SchedulingQueue.PendingPods()
				if len(pendingPods) == len(pods) {
					t.Logf("All Pods are in the pending queue.")
					return true, nil
				}
				t.Logf("Waiting for all Pods to be in the scheduling queue. %d/%d", len(pendingPods), len(pods))
				return false, nil
			})
			if err != nil {
				t.Fatalf("Failed to observe all Pods in the scheduling queue: %v", err)
			}

			actualOrder := make([]string, len(tt.expectedOrder))
			for i := 0; i < len(tt.expectedOrder); i++ {
				queueInfo := testutils.NextPodOrDie(t, testCtx)
				actualOrder[i] = queueInfo.Pod.Name
				t.Logf("Popped Pod %q", queueInfo.Pod.Name)
			}
			if diff := cmp.Diff(tt.expectedOrder, actualOrder); diff != "" {
				t.Errorf("Expected Pod order (-want,+got):\n%s", diff)
			} else {
				t.Logf("Pods were popped out in the expected order based on custom sorting logic.")
			}
		})
	}
}

// TestPostFilterPlugin tests invocation of postFilter plugins.
func TestPostFilterPlugin(t *testing.T) {
	numNodes := 1
	tests := []struct {
		name                      string
		numNodes                  int
		rejectFilter              bool
		failScore                 bool
		rejectPostFilter          bool
		rejectPostFilter2         bool
		breakPostFilter           bool
		breakPostFilter2          bool
		expectFilterNumCalled     int
		expectScoreNumCalled      int
		expectPostFilterNumCalled int
	}{
		{
			name:                      "Filter passed and Score success",
			numNodes:                  30,
			rejectFilter:              false,
			failScore:                 false,
			rejectPostFilter:          false,
			expectFilterNumCalled:     30,
			expectScoreNumCalled:      30,
			expectPostFilterNumCalled: 0,
		},
		{
			name:                      "Filter failed and PostFilter passed",
			numNodes:                  numNodes,
			rejectFilter:              true,
			failScore:                 false,
			rejectPostFilter:          false,
			expectFilterNumCalled:     numNodes * 2,
			expectScoreNumCalled:      1,
			expectPostFilterNumCalled: 1,
		},
		{
			name:                      "Filter failed and PostFilter failed",
			numNodes:                  numNodes,
			rejectFilter:              true,
			failScore:                 false,
			rejectPostFilter:          true,
			expectFilterNumCalled:     numNodes * 2,
			expectScoreNumCalled:      1,
			expectPostFilterNumCalled: 2,
		},
		{
			name:                      "Score failed and PostFilter failed",
			numNodes:                  numNodes,
			rejectFilter:              true,
			failScore:                 true,
			rejectPostFilter:          true,
			expectFilterNumCalled:     numNodes * 2,
			expectScoreNumCalled:      1,
			expectPostFilterNumCalled: 2,
		},
		{
			name:                      "Filter failed and first PostFilter broken",
			numNodes:                  numNodes,
			rejectFilter:              true,
			breakPostFilter:           true,
			expectFilterNumCalled:     numNodes * 2,
			expectScoreNumCalled:      0,
			expectPostFilterNumCalled: 1,
		},
		{
			name:                      "Filter failed and second PostFilter broken",
			numNodes:                  numNodes,
			rejectFilter:              true,
			rejectPostFilter:          true,
			rejectPostFilter2:         true,
			breakPostFilter2:          true,
			expectFilterNumCalled:     numNodes * 2,
			expectScoreNumCalled:      0,
			expectPostFilterNumCalled: 2,
		},
	}

	var postFilterPluginName2 = postfilterPluginName + "2"
	testContext := testutils.InitTestAPIServer(t, "post-filter", nil)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create a plugin registry for testing. Register a combination of filter and postFilter plugin.
			var (
				filterPlugin      = &FilterPlugin{}
				scorePlugin       = &ScorePlugin{}
				postFilterPlugin  = &PostFilterPlugin{name: postfilterPluginName}
				postFilterPlugin2 = &PostFilterPlugin{name: postFilterPluginName2}
			)
			filterPlugin.rejectFilter = tt.rejectFilter
			scorePlugin.failScore = tt.failScore
			postFilterPlugin.rejectPostFilter = tt.rejectPostFilter
			postFilterPlugin2.rejectPostFilter = tt.rejectPostFilter2
			postFilterPlugin.breakPostFilter = tt.breakPostFilter
			postFilterPlugin2.breakPostFilter = tt.breakPostFilter2

			registry := frameworkruntime.Registry{
				filterPluginName:      newPlugin(filterPlugin),
				scorePluginName:       newPlugin(scorePlugin),
				postfilterPluginName:  newPlugin(postFilterPlugin),
				postFilterPluginName2: newPlugin(postFilterPlugin2),
			}

			// Setup plugins for testing.
			cfg := configtesting.V1ToInternalWithDefaults(t, configv1.KubeSchedulerConfiguration{
				Profiles: []configv1.KubeSchedulerProfile{{
					SchedulerName: pointer.String(v1.DefaultSchedulerName),
					Plugins: &configv1.Plugins{
						Filter: configv1.PluginSet{
							Enabled: []configv1.Plugin{
								{Name: filterPluginName},
							},
						},
						PreScore: configv1.PluginSet{
							Disabled: []configv1.Plugin{
								{Name: "*"},
							},
						},
						Score: configv1.PluginSet{
							Enabled: []configv1.Plugin{
								{Name: scorePluginName},
							},
							// disable default in-tree Score plugins
							// to make it easy to control configured ScorePlugins failure
							Disabled: []configv1.Plugin{
								{Name: "*"},
							},
						},
						PostFilter: configv1.PluginSet{
							Enabled: []configv1.Plugin{
								{Name: postfilterPluginName},
								{Name: postFilterPluginName2},
							},
							// Need to disable default in-tree PostFilter plugins, as they will
							// call RunPostFilterPlugins and hence impact the "numPostFilterCalled".
							Disabled: []configv1.Plugin{
								{Name: "*"},
							},
						},
					},
				}}})

			testCtx, teardown := schedulerutils.InitTestSchedulerForFrameworkTest(t, testContext, int(tt.numNodes), true,
				scheduler.WithProfiles(cfg.Profiles...),
				scheduler.WithFrameworkOutOfTreeRegistry(registry),
			)
			defer teardown()

			// Create a best effort pod.
			pod, err := testutils.CreatePausePod(testCtx.ClientSet, testutils.InitPausePod(&testutils.PausePodConfig{Name: "test-pod", Namespace: testCtx.NS.Name}))
			if err != nil {
				t.Errorf("Error while creating a test pod: %v", err)
			}

			if tt.rejectFilter {
				if err = wait.PollUntilContextTimeout(testCtx.Ctx, 10*time.Millisecond, 10*time.Second, false,
					testutils.PodUnschedulable(testCtx.ClientSet, pod.Namespace, pod.Name)); err != nil {
					t.Errorf("Didn't expect the pod to be scheduled.")
				}

				if numFilterCalled := filterPlugin.deepCopy().numFilterCalled; numFilterCalled < tt.expectFilterNumCalled {
					t.Errorf("Expected the filter plugin to be called at least %v times, but got %v.", tt.expectFilterNumCalled, numFilterCalled)
				}
				if numScoreCalled := scorePlugin.deepCopy().numScoreCalled; numScoreCalled < tt.expectScoreNumCalled {
					t.Errorf("Expected the score plugin to be called at least %v times, but got %v.", tt.expectScoreNumCalled, numScoreCalled)
				}
			} else {
				if err = testutils.WaitForPodToSchedule(testCtx.Ctx, testCtx.ClientSet, pod); err != nil {
					t.Errorf("Expected the pod to be scheduled. error: %v", err)
				}
				if numFilterCalled := filterPlugin.deepCopy().numFilterCalled; numFilterCalled != tt.expectFilterNumCalled {
					t.Errorf("Expected the filter plugin to be called %v times, but got %v.", tt.expectFilterNumCalled, numFilterCalled)
				}
				if numScoreCalled := scorePlugin.deepCopy().numScoreCalled; numScoreCalled != tt.expectScoreNumCalled {
					t.Errorf("Expected the score plugin to be called %v times, but got %v.", tt.expectScoreNumCalled, numScoreCalled)
				}
			}

			numPostFilterCalled := postFilterPlugin.numPostFilterCalled + postFilterPlugin2.numPostFilterCalled
			if numPostFilterCalled != tt.expectPostFilterNumCalled {
				t.Errorf("Expected the postfilter plugin to be called %v times, but got %v.", tt.expectPostFilterNumCalled, numPostFilterCalled)
			}
		})
	}
}

// TestScorePlugin tests invocation of score plugins.
func TestScorePlugin(t *testing.T) {
	testContext := testutils.InitTestAPIServer(t, "score-plugin", nil)

	tests := []struct {
		name string
		fail bool
	}{
		{
			name: "fail score plugin",
			fail: true,
		},
		{
			name: "do not fail score plugin",
			fail: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// Create a plugin registry for testing. Register only a score plugin.
			scorePlugin := &ScorePlugin{}
			registry, prof := initRegistryAndConfig(t, scorePlugin)

			testCtx, teardown := schedulerutils.InitTestSchedulerForFrameworkTest(t, testContext, 10, true,
				scheduler.WithProfiles(prof),
				scheduler.WithFrameworkOutOfTreeRegistry(registry))
			defer teardown()

			scorePlugin.failScore = test.fail
			// Create a best effort pod.
			pod, err := testutils.CreatePausePod(testCtx.ClientSet,
				testutils.InitPausePod(&testutils.PausePodConfig{Name: "test-pod", Namespace: testCtx.NS.Name}))
			if err != nil {
				t.Fatalf("Error while creating a test pod: %v", err)
			}

			if test.fail {
				if err = wait.PollUntilContextTimeout(testCtx.Ctx, 10*time.Millisecond, 30*time.Second, false,
					testutils.PodSchedulingError(testCtx.ClientSet, pod.Namespace, pod.Name)); err != nil {
					t.Errorf("Expected a scheduling error, but got: %v", err)
				}
			} else {
				if err = testutils.WaitForPodToSchedule(testCtx.Ctx, testCtx.ClientSet, pod); err != nil {
					t.Errorf("Expected the pod to be scheduled. error: %v", err)
				} else {
					p, err := testutils.GetPod(testCtx.ClientSet, pod.Name, pod.Namespace)
					if err != nil {
						t.Errorf("Failed to retrieve the pod. error: %v", err)
					} else if p.Spec.NodeName != scorePlugin.highScoreNode {
						t.Errorf("Expected the pod to be scheduled on node %q, got %q", scorePlugin.highScoreNode, p.Spec.NodeName)
					}
				}
			}

			if numScoreCalled := scorePlugin.deepCopy().numScoreCalled; numScoreCalled == 0 {
				t.Errorf("Expected the score plugin to be called.")
			}
		})
	}
}

// TestNormalizeScorePlugin tests invocation of normalize score plugins.
func TestNormalizeScorePlugin(t *testing.T) {
	// Create a plugin registry for testing. Register only a normalize score plugin.
	scoreWithNormalizePlugin := &ScoreWithNormalizePlugin{}
	registry, prof := initRegistryAndConfig(t, scoreWithNormalizePlugin)

	testCtx, _ := schedulerutils.InitTestSchedulerForFrameworkTest(t, testutils.InitTestAPIServer(t, "score-plugin", nil), 10, true,
		scheduler.WithProfiles(prof),
		scheduler.WithFrameworkOutOfTreeRegistry(registry))

	// Create a best effort pod.
	pod, err := testutils.CreatePausePod(testCtx.ClientSet,
		testutils.InitPausePod(&testutils.PausePodConfig{Name: "test-pod", Namespace: testCtx.NS.Name}))
	if err != nil {
		t.Fatalf("Error while creating a test pod: %v", err)
	}

	if err = testutils.WaitForPodToSchedule(testCtx.Ctx, testCtx.ClientSet, pod); err != nil {
		t.Errorf("Expected the pod to be scheduled. error: %v", err)
	}

	p := scoreWithNormalizePlugin.deepCopy()
	if p.numScoreCalled == 0 {
		t.Errorf("Expected the score plugin to be called.")
	}
	if p.numNormalizeScoreCalled == 0 {
		t.Error("Expected the normalize score plugin to be called")
	}
}

// TestReservePlugin tests invocation of reserve plugins.
func TestReservePluginReserve(t *testing.T) {
	testContext := testutils.InitTestAPIServer(t, "reserve-plugin-reserve", nil)

	tests := []struct {
		name string
		fail bool
	}{
		{
			name: "fail reserve plugin",
			fail: true,
		},
		{
			name: "do not fail reserve plugin",
			fail: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// Create a plugin registry for testing. Register only a reserve plugin.
			reservePlugin := &ReservePlugin{}
			registry, prof := initRegistryAndConfig(t, reservePlugin)

			testCtx, teardown := schedulerutils.InitTestSchedulerForFrameworkTest(t, testContext, 2, true,
				scheduler.WithProfiles(prof),
				scheduler.WithFrameworkOutOfTreeRegistry(registry))
			defer teardown()

			reservePlugin.failReserve = test.fail
			// Create a best effort pod.
			pod, err := testutils.CreatePausePod(testCtx.ClientSet,
				testutils.InitPausePod(&testutils.PausePodConfig{Name: "test-pod", Namespace: testCtx.NS.Name}))
			if err != nil {
				t.Errorf("Error while creating a test pod: %v", err)
			}

			if test.fail {
				if err = wait.PollUntilContextTimeout(testCtx.Ctx, 10*time.Millisecond, 30*time.Second, false,
					testutils.PodUnschedulable(testCtx.ClientSet, pod.Namespace, pod.Name)); err != nil {
					t.Errorf("Didn't expect the pod to be scheduled. error: %v", err)
				}
			} else {
				if err = testutils.WaitForPodToSchedule(testCtx.Ctx, testCtx.ClientSet, pod); err != nil {
					t.Errorf("Expected the pod to be scheduled. error: %v", err)
				}
			}

			if reservePlugin.numReserveCalled == 0 {
				t.Errorf("Expected the reserve plugin to be called.")
			}
		})
	}
}

// TestPrebindPlugin tests invocation of prebind plugins.
func TestPrebindPlugin(t *testing.T) {
	testContext := testutils.InitTestAPIServer(t, "prebind-plugin", nil)

	nodesNum := 2

	tests := []struct {
		name             string
		fail             bool
		reject           bool
		succeedOnRetry   bool
		unschedulablePod *v1.Pod
	}{
		{
			name:   "disable fail and reject flags",
			fail:   false,
			reject: false,
		},
		{
			name:   "enable fail and disable reject flags",
			fail:   true,
			reject: false,
		},
		{
			name:   "disable fail and enable reject flags",
			fail:   false,
			reject: true,
		},
		{
			name:   "enable fail and reject flags",
			fail:   true,
			reject: true,
		},
		{
			name:           "fail on 1st try but succeed on retry",
			fail:           true,
			reject:         false,
			succeedOnRetry: true,
		},
		{
			name:             "failure on preBind moves unschedulable pods",
			fail:             true,
			unschedulablePod: st.MakePod().Name("unschedulable-pod").Namespace(testContext.NS.Name).Container(imageutils.GetPauseImageName()).Obj(),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// Create a plugin registry for testing. Register a prebind and a filter plugin.
			preBindPlugin := &PreBindPlugin{podUIDs: make(map[types.UID]struct{})}
			filterPlugin := &FilterPlugin{}
			registry := frameworkruntime.Registry{
				preBindPluginName: newPlugin(preBindPlugin),
				filterPluginName:  newPlugin(filterPlugin),
			}

			// Setup initial prebind and filter plugin in different profiles.
			// The second profile ensures the embedded filter plugin is exclusively called, and hence
			// we can use its internal `numFilterCalled` to perform some precise checking logic.
			cfg := configtesting.V1ToInternalWithDefaults(t, configv1.KubeSchedulerConfiguration{
				Profiles: []configv1.KubeSchedulerProfile{
					{
						SchedulerName: pointer.String(v1.DefaultSchedulerName),
						Plugins: &configv1.Plugins{
							PreBind: configv1.PluginSet{
								Enabled: []configv1.Plugin{
									{Name: preBindPluginName},
								},
							},
						},
					},
					{
						SchedulerName: pointer.String("2nd-scheduler"),
						Plugins: &configv1.Plugins{
							Filter: configv1.PluginSet{
								Enabled: []configv1.Plugin{
									{Name: filterPluginName},
								},
							},
						},
					},
				},
			})

			testCtx, teardown := schedulerutils.InitTestSchedulerForFrameworkTest(t, testContext, nodesNum, true,
				scheduler.WithProfiles(cfg.Profiles...),
				scheduler.WithFrameworkOutOfTreeRegistry(registry))
			defer teardown()

			if p := test.unschedulablePod; p != nil {
				p.Spec.SchedulerName = "2nd-scheduler"
				filterPlugin.rejectFilter = true
				if _, err := testutils.CreatePausePod(testCtx.ClientSet, p); err != nil {
					t.Fatalf("Error while creating an unschedulable pod: %v", err)
				}
			}

			preBindPlugin.set(test.fail, test.reject, test.succeedOnRetry)

			// Create a best effort pod.
			pod, err := testutils.CreatePausePod(testCtx.ClientSet,
				testutils.InitPausePod(&testutils.PausePodConfig{Name: "test-pod", Namespace: testCtx.NS.Name}))
			if err != nil {
				t.Errorf("Error while creating a test pod: %v", err)
			}

			if test.fail {
				if test.succeedOnRetry {
					if err = testutils.WaitForPodToScheduleWithTimeout(testCtx.Ctx, testCtx.ClientSet, pod, 10*time.Second); err != nil {
						t.Errorf("Expected the pod to be schedulable on retry, but got an error: %v", err)
					}
				} else if err = wait.PollUntilContextTimeout(testCtx.Ctx, 10*time.Millisecond, 30*time.Second, false,
					testutils.PodSchedulingError(testCtx.ClientSet, pod.Namespace, pod.Name)); err != nil {
					t.Errorf("Expected a scheduling error, but didn't get it. error: %v", err)
				}
			} else if test.reject {
				if err = testutils.WaitForPodUnschedulable(testCtx.Ctx, testCtx.ClientSet, pod); err != nil {
					t.Errorf("Expected the pod to be unschedulable")
				}
			} else if err = testutils.WaitForPodToSchedule(testCtx.Ctx, testCtx.ClientSet, pod); err != nil {
				t.Errorf("Expected the pod to be scheduled. error: %v", err)
			}

			p := preBindPlugin.deepCopy()
			if p.numPreBindCalled == 0 {
				t.Errorf("Expected the prebind plugin to be called.")
			}

			if test.unschedulablePod != nil {
				if err := wait.PollUntilContextTimeout(testCtx.Ctx, 10*time.Millisecond, 15*time.Second, false, func(ctx context.Context) (bool, error) {
					// 2 means the unschedulable pod is expected to be retried at least twice.
					// (one initial attempt plus the one moved by the preBind pod)
					return filterPlugin.deepCopy().numFilterCalled >= 2*nodesNum, nil
				}); err != nil {
					t.Errorf("Timed out waiting for the unschedulable Pod to be retried at least twice.")
				}
			}
		})
	}
}

// TestUnReserveReservePlugins tests invocation of the Unreserve operation in
// reserve plugins through failures in execution points such as pre-bind. Also
// tests that the order of invocation of Unreserve operation is executed in the
// reverse order of invocation of the Reserve operation.
func TestUnReserveReservePlugins(t *testing.T) {
	tests := []struct {
		name          string
		plugins       []*ReservePlugin
		fail          bool
		failPluginIdx int
	}{
		{
			name:          "The first Reserve plugin fails",
			failPluginIdx: 0,
			plugins: []*ReservePlugin{
				{
					name:        "failedReservePlugin1",
					failReserve: true,
				},
				{
					name:        "succeedReservePlugin",
					failReserve: false,
				},
				{
					name:        "failedReservePlugin2",
					failReserve: true,
				},
			},
			fail: true,
		},
		{
			name:          "The middle Reserve plugin fails",
			failPluginIdx: 1,
			plugins: []*ReservePlugin{
				{
					name:        "succeedReservePlugin1",
					failReserve: false,
				},
				{
					name:        "failedReservePlugin1",
					failReserve: true,
				},
				{
					name:        "succeedReservePlugin2",
					failReserve: false,
				},
			},
			fail: true,
		},
		{
			name:          "The last Reserve plugin fails",
			failPluginIdx: 2,
			plugins: []*ReservePlugin{
				{
					name:        "succeedReservePlugin1",
					failReserve: false,
				},
				{
					name:        "succeedReservePlugin2",
					failReserve: false,
				},
				{
					name:        "failedReservePlugin1",
					failReserve: true,
				},
			},
			fail: true,
		},
		{
			name:          "The Reserve plugins succeed",
			failPluginIdx: -1,
			plugins: []*ReservePlugin{
				{
					name:        "succeedReservePlugin1",
					failReserve: false,
				},
				{
					name:        "succeedReservePlugin2",
					failReserve: false,
				},
				{
					name:        "succeedReservePlugin3",
					failReserve: false,
				},
			},
			fail: false,
		},
	}

	testContext := testutils.InitTestAPIServer(t, "unreserve-reserve-plugin", nil)

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var pls []framework.Plugin
			for _, pl := range test.plugins {
				pls = append(pls, pl)
			}
			registry, prof := initRegistryAndConfig(t, pls...)

			testCtx, teardown := schedulerutils.InitTestSchedulerForFrameworkTest(t, testContext, 2, true,
				scheduler.WithProfiles(prof),
				scheduler.WithFrameworkOutOfTreeRegistry(registry))
			defer teardown()

			// Create a best effort pod.
			podName := "test-pod"
			pod, err := testutils.CreatePausePod(testCtx.ClientSet,
				testutils.InitPausePod(&testutils.PausePodConfig{Name: podName, Namespace: testCtx.NS.Name}))
			if err != nil {
				t.Errorf("Error while creating a test pod: %v", err)
			}

			if test.fail {
				if err = wait.PollUntilContextTimeout(testCtx.Ctx, 10*time.Millisecond, 30*time.Second, false,
					testutils.PodUnschedulable(testCtx.ClientSet, pod.Namespace, pod.Name)); err != nil {
					t.Errorf("Expected a Pod to get Unschedulable, but got: %v", err)
				}

				for i, pl := range test.plugins {
					if i <= test.failPluginIdx {
						if pl.numReserveCalled != 1 {
							t.Errorf("Reserve Plugins %s numReserveCalled = %d, want 1.", pl.name, pl.numReserveCalled)
						}
					} else {
						if pl.numReserveCalled != 0 {
							t.Errorf("Reserve Plugins %s numReserveCalled = %d, want 0.", pl.name, pl.numReserveCalled)
						}
					}

					if pl.numUnreserveCalled != 1 {
						t.Errorf("Reserve Plugin %s numUnreserveCalled = %d, want 1.", pl.name, pl.numUnreserveCalled)
					}
				}
			} else {
				if err = testutils.WaitForPodToSchedule(testCtx.Ctx, testCtx.ClientSet, pod); err != nil {
					t.Errorf("Expected the pod to be scheduled. error: %v", err)
				}

				for _, pl := range test.plugins {
					if pl.numReserveCalled != 1 {
						t.Errorf("Reserve Plugin %s numReserveCalled = %d, want 1.", pl.name, pl.numReserveCalled)
					}
					if pl.numUnreserveCalled != 0 {
						t.Errorf("Reserve Plugin %s numUnreserveCalled = %d, want 0.", pl.name, pl.numUnreserveCalled)
					}
				}
			}
		})
	}
}

// TestUnReservePermitPlugins tests unreserve of Permit plugins.
func TestUnReservePermitPlugins(t *testing.T) {
	testContext := testutils.InitTestAPIServer(t, "unreserve-reserve-plugin", nil)

	tests := []struct {
		name   string
		plugin *PermitPlugin
		reject bool
	}{
		{
			name:   "All Reserve plugins passed, but a Permit plugin was rejected",
			reject: true,
			plugin: &PermitPlugin{
				name:         "rejectedPermitPlugin",
				rejectPermit: true,
			},
		},
		{
			name:   "All Reserve plugins passed, but a Permit plugin timeout in waiting",
			reject: true,
			plugin: &PermitPlugin{
				name:          "timeoutPermitPlugin",
				timeoutPermit: true,
			},
		},
		{
			name:   "The Permit plugin succeed",
			reject: false,
			plugin: &PermitPlugin{
				name: "succeedPermitPlugin",
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			reservePlugin := &ReservePlugin{
				name:        "reservePlugin",
				failReserve: false,
			}
			registry, profile := initRegistryAndConfig(t, []framework.Plugin{test.plugin, reservePlugin}...)

			testCtx, teardown := schedulerutils.InitTestSchedulerForFrameworkTest(t, testContext, 2, true,
				scheduler.WithProfiles(profile),
				scheduler.WithFrameworkOutOfTreeRegistry(registry))
			defer teardown()

			// Create a best effort pod.
			podName := "test-pod"
			pod, err := testutils.CreatePausePod(testCtx.ClientSet,
				testutils.InitPausePod(&testutils.PausePodConfig{Name: podName, Namespace: testCtx.NS.Name}))
			if err != nil {
				t.Errorf("Error while creating a test pod: %v", err)
			}

			if test.reject {
				if err = testutils.WaitForPodUnschedulable(testCtx.Ctx, testCtx.ClientSet, pod); err != nil {
					t.Errorf("Didn't expect the pod to be scheduled. error: %v", err)
				}

				// Verify the Reserve Plugins
				if reservePlugin.numUnreserveCalled != 1 {
					t.Errorf("Reserve Plugin %s numUnreserveCalled = %d, want 1.", reservePlugin.name, reservePlugin.numUnreserveCalled)
				}
			} else {
				if err = testutils.WaitForPodToSchedule(testCtx.Ctx, testCtx.ClientSet, pod); err != nil {
					t.Errorf("Expected the pod to be scheduled. error: %v", err)
				}

				// Verify the Reserve Plugins
				if reservePlugin.numUnreserveCalled != 0 {
					t.Errorf("Reserve Plugin %s numUnreserveCalled = %d, want 0.", reservePlugin.name, reservePlugin.numUnreserveCalled)
				}
			}

			if test.plugin.numPermitCalled != 1 {
				t.Errorf("Expected the Permit plugin to be called.")
			}
		})
	}
}

// TestUnReservePreBindPlugins tests unreserve of Prebind plugins.
func TestUnReservePreBindPlugins(t *testing.T) {
	testContext := testutils.InitTestAPIServer(t, "unreserve-prebind-plugin", nil)

	tests := []struct {
		name       string
		plugin     *PreBindPlugin
		wantReject bool
	}{
		{
			name:       "All Reserve plugins passed, but a PreBind plugin failed",
			wantReject: true,
			plugin: &PreBindPlugin{
				podUIDs:       make(map[types.UID]struct{}),
				rejectPreBind: true,
			},
		},
		{
			name:       "All Reserve plugins passed, and PreBind plugin succeed",
			wantReject: false,
			plugin:     &PreBindPlugin{podUIDs: make(map[types.UID]struct{})},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			reservePlugin := &ReservePlugin{
				name:        "reservePlugin",
				failReserve: false,
			}
			registry, profile := initRegistryAndConfig(t, []framework.Plugin{test.plugin, reservePlugin}...)

			testCtx, teardown := schedulerutils.InitTestSchedulerForFrameworkTest(t, testContext, 2, true,
				scheduler.WithProfiles(profile),
				scheduler.WithFrameworkOutOfTreeRegistry(registry))
			defer teardown()

			// Create a pause pod.
			podName := "test-pod"
			pod, err := testutils.CreatePausePod(testCtx.ClientSet,
				testutils.InitPausePod(&testutils.PausePodConfig{Name: podName, Namespace: testCtx.NS.Name}))
			if err != nil {
				t.Errorf("Error while creating a test pod: %v", err)
			}

			if test.wantReject {
				if err = testutils.WaitForPodUnschedulable(testCtx.Ctx, testCtx.ClientSet, pod); err != nil {
					t.Errorf("Expected a reasons other than Unschedulable, but got: %v", err)
				}

				// Verify the Reserve Plugins
				if reservePlugin.numUnreserveCalled != 1 {
					t.Errorf("Reserve Plugin %s numUnreserveCalled = %d, want 1.", reservePlugin.name, reservePlugin.numUnreserveCalled)
				}
			} else {
				if err = testutils.WaitForPodToSchedule(testCtx.Ctx, testCtx.ClientSet, pod); err != nil {
					t.Errorf("Expected the pod to be scheduled. error: %v", err)
				}

				// Verify the Reserve Plugins
				if reservePlugin.numUnreserveCalled != 0 {
					t.Errorf("Reserve Plugin %s numUnreserveCalled = %d, want 0.", reservePlugin.name, reservePlugin.numUnreserveCalled)
				}
			}

			if test.plugin.numPreBindCalled != 1 {
				t.Errorf("Expected the Prebind plugin to be called.")
			}
		})
	}
}

// TestUnReserveBindPlugins tests unreserve of Bind plugins.
func TestUnReserveBindPlugins(t *testing.T) {
	testContext := testutils.InitTestAPIServer(t, "unreserve-bind-plugin", nil)

	tests := []struct {
		name   string
		plugin *BindPlugin
		fail   bool
	}{
		{
			name:   "All Reserve plugins passed, and Bind plugin succeed",
			fail:   false,
			plugin: &BindPlugin{name: "SucceedBindPlugin"},
		},
		{
			name:   "All Reserve plugins passed, but a Bind plugin failed",
			fail:   false,
			plugin: &BindPlugin{name: "FailedBindPlugin"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			reservePlugin := &ReservePlugin{
				name:        "reservePlugin",
				failReserve: false,
			}
			registry, profile := initRegistryAndConfig(t, []framework.Plugin{test.plugin, reservePlugin}...)

			test.plugin.client = testContext.ClientSet

			testCtx, teardown := schedulerutils.InitTestSchedulerForFrameworkTest(t, testContext, 2, true,
				scheduler.WithProfiles(profile),
				scheduler.WithFrameworkOutOfTreeRegistry(registry))
			defer teardown()

			// Create a pause pod.
			podName := "test-pod"
			pod, err := testutils.CreatePausePod(testCtx.ClientSet,
				testutils.InitPausePod(&testutils.PausePodConfig{Name: podName, Namespace: testCtx.NS.Name}))
			if err != nil {
				t.Errorf("Error while creating a test pod: %v", err)
			}

			if test.fail {
				if err = wait.PollUntilContextTimeout(testCtx.Ctx, 10*time.Millisecond, 30*time.Second, false,
					testutils.PodSchedulingError(testCtx.ClientSet, pod.Namespace, pod.Name)); err != nil {
					t.Errorf("Expected a reasons other than Unschedulable, but got: %v", err)
				}

				// Verify the Reserve Plugins
				if reservePlugin.numUnreserveCalled != 1 {
					t.Errorf("Reserve Plugin %s numUnreserveCalled = %d, want 1.", reservePlugin.name, reservePlugin.numUnreserveCalled)
				}
			} else {
				if err = testutils.WaitForPodToSchedule(testCtx.Ctx, testCtx.ClientSet, pod); err != nil {
					t.Errorf("Expected the pod to be scheduled. error: %v", err)
				}

				// Verify the Reserve Plugins
				if reservePlugin.numUnreserveCalled != 0 {
					t.Errorf("Reserve Plugin %s numUnreserveCalled = %d, want 0.", reservePlugin.name, reservePlugin.numUnreserveCalled)
				}
			}

			if test.plugin.numBindCalled != 1 {
				t.Errorf("Expected the Bind plugin to be called.")
			}
		})
	}
}

type pluginInvokeEvent struct {
	pluginName string
	val        int
}

func TestBindPlugin(t *testing.T) {

	var (
		bindPlugin1Name    = "bind-plugin-1"
		bindPlugin2Name    = "bind-plugin-2"
		reservePluginName  = "mock-reserve-plugin"
		postBindPluginName = "mock-post-bind-plugin"
	)

	testContext := testutils.InitTestAPIServer(t, "bind-plugin", nil)

	tests := []struct {
		name                   string
		enabledBindPlugins     []configv1.Plugin
		bindPluginStatuses     []*framework.Status
		expectBoundByScheduler bool   // true means this test case expecting scheduler would bind pods
		expectBoundByPlugin    bool   // true means this test case expecting a plugin would bind pods
		expectBindFailed       bool   // true means this test case expecting a plugin binding pods with error
		expectBindPluginName   string // expecting plugin name to bind pods
		expectInvokeEvents     []pluginInvokeEvent
	}{
		{
			name:                   "bind plugins skipped to bind the pod and scheduler bond the pod",
			enabledBindPlugins:     []configv1.Plugin{{Name: bindPlugin1Name}, {Name: bindPlugin2Name}, {Name: defaultbinder.Name}},
			bindPluginStatuses:     []*framework.Status{framework.NewStatus(framework.Skip, ""), framework.NewStatus(framework.Skip, "")},
			expectBoundByScheduler: true,
			expectInvokeEvents:     []pluginInvokeEvent{{pluginName: bindPlugin1Name, val: 1}, {pluginName: bindPlugin2Name, val: 1}, {pluginName: postBindPluginName, val: 1}},
		},
		{
			name:                 "bindplugin2 succeeded to bind the pod",
			enabledBindPlugins:   []configv1.Plugin{{Name: bindPlugin1Name}, {Name: bindPlugin2Name}, {Name: defaultbinder.Name}},
			bindPluginStatuses:   []*framework.Status{framework.NewStatus(framework.Skip, ""), framework.NewStatus(framework.Success, "")},
			expectBoundByPlugin:  true,
			expectBindPluginName: bindPlugin2Name,
			expectInvokeEvents:   []pluginInvokeEvent{{pluginName: bindPlugin1Name, val: 1}, {pluginName: bindPlugin2Name, val: 1}, {pluginName: postBindPluginName, val: 1}},
		},
		{
			name:                 "bindplugin1 succeeded to bind the pod",
			enabledBindPlugins:   []configv1.Plugin{{Name: bindPlugin1Name}, {Name: bindPlugin2Name}, {Name: defaultbinder.Name}},
			bindPluginStatuses:   []*framework.Status{framework.NewStatus(framework.Success, ""), framework.NewStatus(framework.Success, "")},
			expectBoundByPlugin:  true,
			expectBindPluginName: bindPlugin1Name,
			expectInvokeEvents:   []pluginInvokeEvent{{pluginName: bindPlugin1Name, val: 1}, {pluginName: postBindPluginName, val: 1}},
		},
		{
			name:               "bind plugin fails to bind the pod",
			enabledBindPlugins: []configv1.Plugin{{Name: bindPlugin1Name}, {Name: bindPlugin2Name}, {Name: defaultbinder.Name}},
			expectBindFailed:   true,
			bindPluginStatuses: []*framework.Status{framework.NewStatus(framework.Error, "failed to bind"), framework.NewStatus(framework.Success, "")},
			expectInvokeEvents: []pluginInvokeEvent{{pluginName: bindPlugin1Name, val: 1}, {pluginName: reservePluginName, val: 1}},
		},
		{
			name:               "all bind plugins will be skipped(this should not happen for most of the cases)",
			enabledBindPlugins: []configv1.Plugin{{Name: bindPlugin1Name}, {Name: bindPlugin2Name}},
			bindPluginStatuses: []*framework.Status{framework.NewStatus(framework.Skip, ""), framework.NewStatus(framework.Skip, "")},
			expectInvokeEvents: []pluginInvokeEvent{{pluginName: bindPlugin1Name, val: 1}, {pluginName: bindPlugin2Name, val: 1}},
		},
	}

	var pluginInvokeEventChan chan pluginInvokeEvent
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			bindPlugin1 := &BindPlugin{name: bindPlugin1Name, client: testContext.ClientSet}
			bindPlugin2 := &BindPlugin{name: bindPlugin2Name, client: testContext.ClientSet}
			reservePlugin := &ReservePlugin{name: reservePluginName}
			postBindPlugin := &PostBindPlugin{name: postBindPluginName}

			// Create a plugin registry for testing. Register reserve, bind, and
			// postBind plugins.
			registry := frameworkruntime.Registry{
				reservePlugin.Name():  newPlugin(reservePlugin),
				bindPlugin1.Name():    newPlugin(bindPlugin1),
				bindPlugin2.Name():    newPlugin(bindPlugin2),
				postBindPlugin.Name(): newPlugin(postBindPlugin),
			}

			// Setup initial unreserve and bind plugins for testing.
			cfg := configtesting.V1ToInternalWithDefaults(t, configv1.KubeSchedulerConfiguration{
				Profiles: []configv1.KubeSchedulerProfile{{
					SchedulerName: pointer.String(v1.DefaultSchedulerName),
					Plugins: &configv1.Plugins{
						MultiPoint: configv1.PluginSet{
							Disabled: []configv1.Plugin{
								{Name: defaultbinder.Name},
							},
						},
						Reserve: configv1.PluginSet{
							Enabled: []configv1.Plugin{{Name: reservePlugin.Name()}},
						},
						Bind: configv1.PluginSet{
							// Put DefaultBinder last.
							Enabled:  test.enabledBindPlugins,
							Disabled: []configv1.Plugin{{Name: defaultbinder.Name}},
						},
						PostBind: configv1.PluginSet{
							Enabled: []configv1.Plugin{{Name: postBindPlugin.Name()}},
						},
					},
				}},
			})

			testCtx, teardown := schedulerutils.InitTestSchedulerForFrameworkTest(t, testContext, 2, true,
				scheduler.WithProfiles(cfg.Profiles...),
				scheduler.WithFrameworkOutOfTreeRegistry(registry),
			)
			defer teardown()

			pluginInvokeEventChan = make(chan pluginInvokeEvent, 10)

			bindPlugin1.bindStatus = test.bindPluginStatuses[0]
			bindPlugin2.bindStatus = test.bindPluginStatuses[1]

			bindPlugin1.pluginInvokeEventChan = pluginInvokeEventChan
			bindPlugin2.pluginInvokeEventChan = pluginInvokeEventChan
			reservePlugin.pluginInvokeEventChan = pluginInvokeEventChan
			postBindPlugin.pluginInvokeEventChan = pluginInvokeEventChan

			// Create a best effort pod.
			pod, err := testutils.CreatePausePod(testCtx.ClientSet,
				testutils.InitPausePod(&testutils.PausePodConfig{Name: "test-pod", Namespace: testCtx.NS.Name}))
			if err != nil {
				t.Errorf("Error while creating a test pod: %v", err)
			}

			if test.expectBoundByScheduler || test.expectBoundByPlugin {
				// bind plugins skipped to bind the pod
				if err = testutils.WaitForPodToSchedule(testCtx.Ctx, testCtx.ClientSet, pod); err != nil {
					t.Fatalf("Expected the pod to be scheduled. error: %v", err)
				}
				pod, err = testCtx.ClientSet.CoreV1().Pods(pod.Namespace).Get(testCtx.Ctx, pod.Name, metav1.GetOptions{})
				if err != nil {
					t.Errorf("can't get pod: %v", err)
				}
				p1 := bindPlugin1.deepCopy()
				p2 := bindPlugin2.deepCopy()
				if test.expectBoundByScheduler {
					if pod.Annotations[bindPluginAnnotation] != "" {
						t.Errorf("Expected the pod to be bound by scheduler instead of by bindplugin %s", pod.Annotations[bindPluginAnnotation])
					}
					if p1.numBindCalled != 1 || p2.numBindCalled != 1 {
						t.Errorf("Expected each bind plugin to be called once, was called %d and %d times.", p1.numBindCalled, p2.numBindCalled)
					}
				} else {
					if pod.Annotations[bindPluginAnnotation] != test.expectBindPluginName {
						t.Errorf("Expected the pod to be bound by bindplugin %s instead of by bindplugin %s", test.expectBindPluginName, pod.Annotations[bindPluginAnnotation])
					}
					if p1.numBindCalled != 1 {
						t.Errorf("Expected %s to be called once, was called %d times.", p1.Name(), p1.numBindCalled)
					}
					if test.expectBindPluginName == p1.Name() && p2.numBindCalled > 0 {
						// expect bindplugin1 succeeded to bind the pod and bindplugin2 should not be called.
						t.Errorf("Expected %s not to be called, was called %d times.", p2.Name(), p2.numBindCalled)
					}
				}
				if err = wait.PollUntilContextTimeout(testCtx.Ctx, 10*time.Millisecond, 30*time.Second, false, func(ctx context.Context) (done bool, err error) {
					p := postBindPlugin.deepCopy()
					return p.numPostBindCalled == 1, nil
				}); err != nil {
					t.Errorf("Expected the postbind plugin to be called once, was called %d times.", postBindPlugin.numPostBindCalled)
				}
				if reservePlugin.numUnreserveCalled != 0 {
					t.Errorf("Expected unreserve to not be called, was called %d times.", reservePlugin.numUnreserveCalled)
				}
			} else if test.expectBindFailed {
				// bind plugin fails to bind the pod
				if err = wait.PollUntilContextTimeout(testCtx.Ctx, 10*time.Millisecond, 30*time.Second, false,
					testutils.PodSchedulingError(testCtx.ClientSet, pod.Namespace, pod.Name)); err != nil {
					t.Errorf("Expected a scheduling error, but didn't get it. error: %v", err)
				}
				p := postBindPlugin.deepCopy()
				if p.numPostBindCalled > 0 {
					t.Errorf("Didn't expect the postbind plugin to be called %d times.", p.numPostBindCalled)
				}
			} else if postBindPlugin.numPostBindCalled > 0 {
				// all bind plugins are skipped
				t.Errorf("Didn't expect the postbind plugin to be called %d times.", postBindPlugin.numPostBindCalled)
			}

			for j := range test.expectInvokeEvents {
				expectEvent := test.expectInvokeEvents[j]
				select {
				case event := <-pluginInvokeEventChan:
					if event.pluginName != expectEvent.pluginName {
						t.Errorf("Expect invoke event %d from plugin %s instead of %s", j, expectEvent.pluginName, event.pluginName)
					}
					if event.val != expectEvent.val {
						t.Errorf("Expect val of invoke event %d to be %d instead of %d", j, expectEvent.val, event.val)
					}
				case <-time.After(time.Second * 30):
					t.Errorf("Waiting for invoke event %d timeout.", j)
				}
			}
		})
	}
}

// TestPostBindPlugin tests invocation of postbind plugins.
func TestPostBindPlugin(t *testing.T) {
	testContext := testutils.InitTestAPIServer(t, "postbind-plugin", nil)

	tests := []struct {
		name        string
		preBindFail bool
	}{
		{
			name:        "plugin preBind fail",
			preBindFail: true,
		},
		{
			name:        "plugin preBind do not fail",
			preBindFail: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// Create a plugin registry for testing. Register a prebind and a postbind plugin.
			preBindPlugin := &PreBindPlugin{
				failPreBind: test.preBindFail,
				podUIDs:     make(map[types.UID]struct{}),
			}
			postBindPlugin := &PostBindPlugin{
				name:                  postBindPluginName,
				pluginInvokeEventChan: make(chan pluginInvokeEvent, 1),
			}

			registry, prof := initRegistryAndConfig(t, preBindPlugin, postBindPlugin)
			testCtx, teardown := schedulerutils.InitTestSchedulerForFrameworkTest(t, testContext, 2, true,
				scheduler.WithProfiles(prof),
				scheduler.WithFrameworkOutOfTreeRegistry(registry))
			defer teardown()

			// Create a best effort pod.
			pod, err := testutils.CreatePausePod(testCtx.ClientSet,
				testutils.InitPausePod(&testutils.PausePodConfig{Name: "test-pod", Namespace: testCtx.NS.Name}))
			if err != nil {
				t.Errorf("Error while creating a test pod: %v", err)
			}

			if test.preBindFail {
				if err = wait.PollUntilContextTimeout(testCtx.Ctx, 10*time.Millisecond, 30*time.Second, false,
					testutils.PodSchedulingError(testCtx.ClientSet, pod.Namespace, pod.Name)); err != nil {
					t.Errorf("Expected a scheduling error, but didn't get it. error: %v", err)
				}
				if postBindPlugin.numPostBindCalled > 0 {
					t.Errorf("Didn't expect the postbind plugin to be called %d times.", postBindPlugin.numPostBindCalled)
				}
			} else {
				if err = testutils.WaitForPodToSchedule(testCtx.Ctx, testCtx.ClientSet, pod); err != nil {
					t.Errorf("Expected the pod to be scheduled. error: %v", err)
				}
				select {
				case <-postBindPlugin.pluginInvokeEventChan:
				case <-time.After(time.Second * 15):
					t.Errorf("pluginInvokeEventChan timed out")
				}
				if postBindPlugin.numPostBindCalled == 0 {
					t.Errorf("Expected the postbind plugin to be called, was called %d times.", postBindPlugin.numPostBindCalled)
				}
			}
		})
	}
}

// TestPermitPlugin tests invocation of permit plugins.
func TestPermitPlugin(t *testing.T) {
	testContext := testutils.InitTestAPIServer(t, "permit-plugin", nil)

	tests := []struct {
		name    string
		fail    bool
		reject  bool
		timeout bool
	}{
		{
			name:    "disable fail, reject and timeout flags",
			fail:    false,
			reject:  false,
			timeout: false,
		},
		{
			name:    "enable fail, disable reject and timeout flags",
			fail:    true,
			reject:  false,
			timeout: false,
		},
		{
			name:    "disable fail and timeout, enable reject flags",
			fail:    false,
			reject:  true,
			timeout: false,
		},
		{
			name:    "enable fail and reject, disable timeout flags",
			fail:    true,
			reject:  true,
			timeout: false,
		},
		{
			name:    "disable fail and reject, disable timeout flags",
			fail:    false,
			reject:  false,
			timeout: true,
		},
		{
			name:    "disable fail and reject, enable timeout flags",
			fail:    false,
			reject:  false,
			timeout: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {

			// Create a plugin registry for testing. Register only a permit plugin.
			perPlugin := &PermitPlugin{name: permitPluginName}
			registry, prof := initRegistryAndConfig(t, perPlugin)

			testCtx, teardown := schedulerutils.InitTestSchedulerForFrameworkTest(t, testContext, 2, true,
				scheduler.WithProfiles(prof),
				scheduler.WithFrameworkOutOfTreeRegistry(registry))
			defer teardown()

			perPlugin.failPermit = test.fail
			perPlugin.rejectPermit = test.reject
			perPlugin.timeoutPermit = test.timeout
			perPlugin.waitAndRejectPermit = false
			perPlugin.waitAndAllowPermit = false

			// Create a best effort pod.
			pod, err := testutils.CreatePausePod(testCtx.ClientSet,
				testutils.InitPausePod(&testutils.PausePodConfig{Name: "test-pod", Namespace: testCtx.NS.Name}))
			if err != nil {
				t.Errorf("Error while creating a test pod: %v", err)
			}
			if test.fail {
				if err = wait.PollUntilContextTimeout(testCtx.Ctx, 10*time.Millisecond, 30*time.Second, false,
					testutils.PodSchedulingError(testCtx.ClientSet, pod.Namespace, pod.Name)); err != nil {
					t.Errorf("Expected a scheduling error, but didn't get it. error: %v", err)
				}
			} else {
				if test.reject || test.timeout {
					if err = testutils.WaitForPodUnschedulable(testCtx.Ctx, testCtx.ClientSet, pod); err != nil {
						t.Errorf("Didn't expect the pod to be scheduled. error: %v", err)
					}
				} else {
					if err = testutils.WaitForPodToSchedule(testCtx.Ctx, testCtx.ClientSet, pod); err != nil {
						t.Errorf("Expected the pod to be scheduled. error: %v", err)
					}
				}
			}

			p := perPlugin.deepCopy()
			if p.numPermitCalled == 0 {
				t.Errorf("Expected the permit plugin to be called.")
			}
		})
	}
}

// TestMultiplePermitPlugins tests multiple permit plugins returning wait for a same pod.
func TestMultiplePermitPlugins(t *testing.T) {
	// Create a plugin registry for testing.
	perPlugin1 := &PermitPlugin{name: "permit-plugin-1"}
	perPlugin2 := &PermitPlugin{name: "permit-plugin-2"}
	registry, prof := initRegistryAndConfig(t, perPlugin1, perPlugin2)

	// Create the API server and the scheduler with the test plugin set.
	testCtx, _ := schedulerutils.InitTestSchedulerForFrameworkTest(t, testutils.InitTestAPIServer(t, "multi-permit-plugin", nil), 2, true,
		scheduler.WithProfiles(prof),
		scheduler.WithFrameworkOutOfTreeRegistry(registry))

	// Both permit plugins will return Wait for permitting
	perPlugin1.timeoutPermit = true
	perPlugin2.timeoutPermit = true

	// Create a test pod.
	podName := "test-pod"
	pod, err := testutils.CreatePausePod(testCtx.ClientSet,
		testutils.InitPausePod(&testutils.PausePodConfig{Name: podName, Namespace: testCtx.NS.Name}))
	if err != nil {
		t.Errorf("Error while creating a test pod: %v", err)
	}

	var waitingPod framework.WaitingPod
	// Wait until the test pod is actually waiting.
	wait.PollUntilContextTimeout(testCtx.Ctx, 10*time.Millisecond, 30*time.Second, false, func(ctx context.Context) (bool, error) {
		waitingPod = perPlugin1.fh.GetWaitingPod(pod.UID)
		return waitingPod != nil, nil
	})

	// Check the number of pending permits
	if l := len(waitingPod.GetPendingPlugins()); l != 2 {
		t.Errorf("Expected the number of pending plugins is 2, but got %d", l)
	}

	perPlugin1.allowAllPods()
	// Check the number of pending permits
	if l := len(waitingPod.GetPendingPlugins()); l != 1 {
		t.Errorf("Expected the number of pending plugins is 1, but got %d", l)
	}

	perPlugin2.allowAllPods()
	if err = testutils.WaitForPodToSchedule(testCtx.Ctx, testCtx.ClientSet, pod); err != nil {
		t.Errorf("Expected the pod to be scheduled. error: %v", err)
	}

	if perPlugin1.numPermitCalled == 0 || perPlugin2.numPermitCalled == 0 {
		t.Errorf("Expected the permit plugin to be called.")
	}
}

// TestPermitPluginsCancelled tests whether all permit plugins are cancelled when pod is rejected.
func TestPermitPluginsCancelled(t *testing.T) {
	// Create a plugin registry for testing.
	perPlugin1 := &PermitPlugin{name: "permit-plugin-1"}
	perPlugin2 := &PermitPlugin{name: "permit-plugin-2"}
	registry, prof := initRegistryAndConfig(t, perPlugin1, perPlugin2)

	// Create the API server and the scheduler with the test plugin set.
	testCtx, _ := schedulerutils.InitTestSchedulerForFrameworkTest(t, testutils.InitTestAPIServer(t, "permit-plugins", nil), 2, true,
		scheduler.WithProfiles(prof),
		scheduler.WithFrameworkOutOfTreeRegistry(registry))

	// Both permit plugins will return Wait for permitting
	perPlugin1.timeoutPermit = true
	perPlugin2.timeoutPermit = true

	// Create a test pod.
	podName := "test-pod"
	pod, err := testutils.CreatePausePod(testCtx.ClientSet,
		testutils.InitPausePod(&testutils.PausePodConfig{Name: podName, Namespace: testCtx.NS.Name}))
	if err != nil {
		t.Errorf("Error while creating a test pod: %v", err)
	}

	var waitingPod framework.WaitingPod
	// Wait until the test pod is actually waiting.
	wait.PollUntilContextTimeout(testCtx.Ctx, 10*time.Millisecond, 30*time.Second, false, func(ctx context.Context) (bool, error) {
		waitingPod = perPlugin1.fh.GetWaitingPod(pod.UID)
		return waitingPod != nil, nil
	})

	perPlugin1.rejectAllPods()
	// Wait some time for the permit plugins to be cancelled
	err = wait.PollUntilContextTimeout(testCtx.Ctx, 10*time.Millisecond, 30*time.Second, false, func(ctx context.Context) (bool, error) {
		p1 := perPlugin1.deepCopy()
		p2 := perPlugin2.deepCopy()
		return p1.cancelled && p2.cancelled, nil
	})
	if err != nil {
		t.Errorf("Expected all permit plugins to be cancelled")
	}
}

// TestCoSchedulingWithPermitPlugin tests invocation of permit plugins.
func TestCoSchedulingWithPermitPlugin(t *testing.T) {
	testContext := testutils.InitTestAPIServer(t, "permit-plugin", nil)

	tests := []struct {
		name       string
		waitReject bool
		waitAllow  bool
	}{
		{
			name:       "having wait reject true and wait allow false",
			waitReject: true,
			waitAllow:  false,
		},
		{
			name:       "having wait reject false and wait allow true",
			waitReject: false,
			waitAllow:  true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {

			// Create a plugin registry for testing. Register only a permit plugin.
			permitPlugin := &PermitPlugin{name: permitPluginName}
			registry, prof := initRegistryAndConfig(t, permitPlugin)

			testCtx, teardown := schedulerutils.InitTestSchedulerForFrameworkTest(t, testContext, 2, true,
				scheduler.WithProfiles(prof),
				scheduler.WithFrameworkOutOfTreeRegistry(registry))
			defer teardown()

			permitPlugin.failPermit = false
			permitPlugin.rejectPermit = false
			permitPlugin.timeoutPermit = false
			permitPlugin.waitAndRejectPermit = test.waitReject
			permitPlugin.waitAndAllowPermit = test.waitAllow

			// Create two pods. First pod to enter Permit() will wait and a second one will either
			// reject or allow first one.
			podA, err := testutils.CreatePausePod(testCtx.ClientSet,
				testutils.InitPausePod(&testutils.PausePodConfig{Name: "pod-a", Namespace: testCtx.NS.Name}))
			if err != nil {
				t.Errorf("Error while creating the first pod: %v", err)
			}
			podB, err := testutils.CreatePausePod(testCtx.ClientSet,
				testutils.InitPausePod(&testutils.PausePodConfig{Name: "pod-b", Namespace: testCtx.NS.Name}))
			if err != nil {
				t.Errorf("Error while creating the second pod: %v", err)
			}

			if test.waitReject {
				if err = testutils.WaitForPodUnschedulable(testCtx.Ctx, testCtx.ClientSet, podA); err != nil {
					t.Errorf("Didn't expect the first pod to be scheduled. error: %v", err)
				}
				if err = testutils.WaitForPodUnschedulable(testCtx.Ctx, testCtx.ClientSet, podB); err != nil {
					t.Errorf("Didn't expect the second pod to be scheduled. error: %v", err)
				}
				if !((permitPlugin.waitingPod == podA.Name && permitPlugin.rejectingPod == podB.Name) ||
					(permitPlugin.waitingPod == podB.Name && permitPlugin.rejectingPod == podA.Name)) {
					t.Errorf("Expect one pod to wait and another pod to reject instead %s waited and %s rejected.",
						permitPlugin.waitingPod, permitPlugin.rejectingPod)
				}
			} else {
				if err = testutils.WaitForPodToSchedule(testCtx.Ctx, testCtx.ClientSet, podA); err != nil {
					t.Errorf("Expected the first pod to be scheduled. error: %v", err)
				}
				if err = testutils.WaitForPodToSchedule(testCtx.Ctx, testCtx.ClientSet, podB); err != nil {
					t.Errorf("Expected the second pod to be scheduled. error: %v", err)
				}
				if !((permitPlugin.waitingPod == podA.Name && permitPlugin.allowingPod == podB.Name) ||
					(permitPlugin.waitingPod == podB.Name && permitPlugin.allowingPod == podA.Name)) {
					t.Errorf("Expect one pod to wait and another pod to allow instead %s waited and %s allowed.",
						permitPlugin.waitingPod, permitPlugin.allowingPod)
				}
			}

			p := permitPlugin.deepCopy()
			if p.numPermitCalled == 0 {
				t.Errorf("Expected the permit plugin to be called.")
			}
		})
	}
}

// TestFilterPlugin tests invocation of filter plugins.
func TestFilterPlugin(t *testing.T) {
	testContext := testutils.InitTestAPIServer(t, "filter-plugin", nil)

	tests := []struct {
		name string
		fail bool
	}{
		{
			name: "fail filter plugin",
			fail: true,
		},
		{
			name: "do not fail filter plugin",
			fail: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// Create a plugin registry for testing. Register only a filter plugin.
			filterPlugin := &FilterPlugin{}
			registry, prof := initRegistryAndConfig(t, filterPlugin)

			testCtx, teardown := schedulerutils.InitTestSchedulerForFrameworkTest(t, testContext, 1, true,
				scheduler.WithProfiles(prof),
				scheduler.WithFrameworkOutOfTreeRegistry(registry))
			defer teardown()

			filterPlugin.failFilter = test.fail
			// Create a best effort pod.
			pod, err := testutils.CreatePausePod(testCtx.ClientSet,
				testutils.InitPausePod(&testutils.PausePodConfig{Name: "test-pod", Namespace: testCtx.NS.Name}))
			if err != nil {
				t.Errorf("Error while creating a test pod: %v", err)
			}

			if test.fail {
				if err = wait.PollUntilContextTimeout(testCtx.Ctx, 10*time.Millisecond, 30*time.Second, false,
					testutils.PodSchedulingError(testCtx.ClientSet, pod.Namespace, pod.Name)); err != nil {
					t.Errorf("Expected a scheduling error, but got: %v", err)
				}
				if filterPlugin.numFilterCalled < 1 {
					t.Errorf("Expected the filter plugin to be called at least 1 time, but got %v.", filterPlugin.numFilterCalled)
				}
			} else {
				if err = testutils.WaitForPodToSchedule(testCtx.Ctx, testCtx.ClientSet, pod); err != nil {
					t.Errorf("Expected the pod to be scheduled. error: %v", err)
				}
				if filterPlugin.numFilterCalled != 1 {
					t.Errorf("Expected the filter plugin to be called 1 time, but got %v.", filterPlugin.numFilterCalled)
				}
			}
		})
	}
}

// TestPreScorePlugin tests invocation of pre-score plugins.
func TestPreScorePlugin(t *testing.T) {
	testContext := testutils.InitTestAPIServer(t, "pre-score-plugin", nil)

	tests := []struct {
		name string
		fail bool
	}{
		{
			name: "fail preScore plugin",
			fail: true,
		},
		{
			name: "do not fail preScore plugin",
			fail: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// Create a plugin registry for testing. Register only a pre-score plugin.
			preScorePlugin := &PreScorePlugin{}
			registry, prof := initRegistryAndConfig(t, preScorePlugin)

			testCtx, teardown := schedulerutils.InitTestSchedulerForFrameworkTest(t, testContext, 2, true,
				scheduler.WithProfiles(prof),
				scheduler.WithFrameworkOutOfTreeRegistry(registry))
			defer teardown()

			preScorePlugin.failPreScore = test.fail
			// Create a best effort pod.
			pod, err := testutils.CreatePausePod(testCtx.ClientSet,
				testutils.InitPausePod(&testutils.PausePodConfig{Name: "test-pod", Namespace: testCtx.NS.Name}))
			if err != nil {
				t.Errorf("Error while creating a test pod: %v", err)
			}

			if test.fail {
				if err = wait.PollUntilContextTimeout(testCtx.Ctx, 10*time.Millisecond, 30*time.Second, false,
					testutils.PodSchedulingError(testCtx.ClientSet, pod.Namespace, pod.Name)); err != nil {
					t.Errorf("Expected a scheduling error, but got: %v", err)
				}
			} else {
				if err = testutils.WaitForPodToSchedule(testCtx.Ctx, testCtx.ClientSet, pod); err != nil {
					t.Errorf("Expected the pod to be scheduled. error: %v", err)
				}
			}

			if preScorePlugin.numPreScoreCalled == 0 {
				t.Errorf("Expected the pre-score plugin to be called.")
			}
		})
	}
}

// TestPreEnqueuePlugin tests invocation of enqueue plugins.
func TestPreEnqueuePlugin(t *testing.T) {
	testContext := testutils.InitTestAPIServer(t, "enqueue-plugin", nil)

	tests := []struct {
		name         string
		pod          *v1.Pod
		admitEnqueue bool
	}{
		{
			name:         "pod is admitted to enqueue",
			pod:          st.MakePod().Name("p").Namespace(testContext.NS.Name).Container("pause").Obj(),
			admitEnqueue: true,
		},
		{
			name:         "pod is not admitted to enqueue",
			pod:          st.MakePod().Name("p").Namespace(testContext.NS.Name).SchedulingGates([]string{"foo"}).Container("pause").Obj(),
			admitEnqueue: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create a plugin registry for testing. Register only a filter plugin.
			enqueuePlugin := &PreEnqueuePlugin{}
			// Plumb a preFilterPlugin to verify if it's called or not.
			preFilterPlugin := &PreFilterPlugin{}
			registry, prof := initRegistryAndConfig(t, enqueuePlugin, preFilterPlugin)

			testCtx, teardown := schedulerutils.InitTestSchedulerForFrameworkTest(t, testContext, 1, true,
				scheduler.WithProfiles(prof),
				scheduler.WithFrameworkOutOfTreeRegistry(registry))
			defer teardown()

			enqueuePlugin.admit = tt.admitEnqueue
			// Create a best effort pod.
			pod, err := testutils.CreatePausePod(testCtx.ClientSet, tt.pod)
			if err != nil {
				t.Errorf("Error while creating a test pod: %v", err)
			}

			if tt.admitEnqueue {
				if err := testutils.WaitForPodToScheduleWithTimeout(testCtx.Ctx, testCtx.ClientSet, pod, 10*time.Second); err != nil {
					t.Errorf("Expected the pod to be schedulable, but got: %v", err)
				}
				// Also verify enqueuePlugin is called.
				if enqueuePlugin.called == 0 {
					t.Errorf("Expected the enqueuePlugin plugin to be called at least once, but got 0")
				}
			} else {
				if err := testutils.WaitForPodSchedulingGated(testCtx.Ctx, testCtx.ClientSet, pod, 10*time.Second); err != nil {
					t.Errorf("Expected the pod to be scheduling waiting, but got: %v", err)
				}
				// Also verify preFilterPlugin is not called.
				if preFilterPlugin.numPreFilterCalled != 0 {
					t.Errorf("Expected the preFilter plugin not to be called, but got %v", preFilterPlugin.numPreFilterCalled)
				}
			}
		})
	}
}

// TestPreemptWithPermitPlugin tests preempt with permit plugins.
// It verifies how waitingPods behave in different scenarios:
// - when waitingPods get preempted
//   - they should be removed from internal waitingPods map, but not physically deleted
//   - it'd trigger moving unschedulable Pods, but not the waitingPods themselves
//
// - when waitingPods get deleted externally, it'd trigger moving unschedulable Pods
func TestPreemptWithPermitPlugin(t *testing.T) {
	testContext := testutils.InitTestAPIServer(t, "preempt-with-permit-plugin", nil)

	ns := testContext.NS.Name
	lowPriority, highPriority := int32(100), int32(300)
	resReq := map[v1.ResourceName]string{
		v1.ResourceCPU:    "200m",
		v1.ResourceMemory: "200",
	}
	preemptorReq := map[v1.ResourceName]string{
		v1.ResourceCPU:    "400m",
		v1.ResourceMemory: "400",
	}

	nodeRes := map[v1.ResourceName]string{
		v1.ResourcePods:   "32",
		v1.ResourceCPU:    "500m",
		v1.ResourceMemory: "500",
	}

	tests := []struct {
		name                   string
		deleteWaitingPod       bool
		maxNumWaitingPodCalled int
		runningPod             *v1.Pod
		waitingPod             *v1.Pod
		preemptor              *v1.Pod
	}{
		{
			name:                   "waiting pod is not physically deleted upon preemption",
			maxNumWaitingPodCalled: 2,
			runningPod:             st.MakePod().Name("running-pod").Namespace(ns).Priority(lowPriority).Req(resReq).ZeroTerminationGracePeriod().Obj(),
			waitingPod:             st.MakePod().Name("waiting-pod").Namespace(ns).Priority(lowPriority).Req(resReq).ZeroTerminationGracePeriod().Obj(),
			preemptor:              st.MakePod().Name("preemptor-pod").Namespace(ns).Priority(highPriority).Req(preemptorReq).ZeroTerminationGracePeriod().Obj(),
		},
		{
			// The waiting Pod has once gone through the scheduling cycle,
			// and we don't know if it's schedulable or not after it's preempted.
			// So, we should retry the scheduling of it so that it won't stuck in the unschedulable Pod pool.
			name:                   "rejecting a waiting pod to trigger retrying unschedulable pods immediately, and the waiting pod itself will be retried",
			maxNumWaitingPodCalled: 2,
			waitingPod:             st.MakePod().Name("waiting-pod").Namespace(ns).Priority(lowPriority).Req(resReq).ZeroTerminationGracePeriod().Obj(),
			preemptor:              st.MakePod().Name("preemptor-pod").Namespace(ns).Priority(highPriority).Req(preemptorReq).ZeroTerminationGracePeriod().Obj(),
		},
		{
			name:                   "deleting a waiting pod to trigger retrying unschedulable pods immediately",
			deleteWaitingPod:       true,
			maxNumWaitingPodCalled: 1,
			waitingPod:             st.MakePod().Name("waiting-pod").Namespace(ns).Priority(lowPriority).Req(resReq).ZeroTerminationGracePeriod().Obj(),
			preemptor:              st.MakePod().Name("preemptor-pod").Namespace(ns).Priority(lowPriority).Req(preemptorReq).ZeroTerminationGracePeriod().Obj(),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create a plugin registry for testing. Register a permit and a filter plugin.
			permitPlugin := &PermitPlugin{}
			// Inject a fake filter plugin to use its internal `numFilterCalled` to verify
			// how many times a Pod gets tried scheduling.
			filterPlugin := &FilterPlugin{numCalledPerPod: make(map[string]int)}
			registry := frameworkruntime.Registry{
				permitPluginName: newPlugin(permitPlugin),
				filterPluginName: newPlugin(filterPlugin),
			}

			// Setup initial permit and filter plugins in the profile.
			cfg := configtesting.V1ToInternalWithDefaults(t, configv1.KubeSchedulerConfiguration{
				Profiles: []configv1.KubeSchedulerProfile{
					{
						SchedulerName: pointer.String(v1.DefaultSchedulerName),
						Plugins: &configv1.Plugins{
							Permit: configv1.PluginSet{
								Enabled: []configv1.Plugin{
									{Name: permitPluginName},
								},
							},
							Filter: configv1.PluginSet{
								// Ensure the fake filter plugin is always called; otherwise noderesources
								// would fail first and exit the Filter phase.
								Enabled: []configv1.Plugin{
									{Name: filterPluginName},
									{Name: noderesources.Name},
								},
								Disabled: []configv1.Plugin{
									{Name: noderesources.Name},
								},
							},
						},
					},
				},
			})

			testCtx, teardown := schedulerutils.InitTestSchedulerForFrameworkTest(t, testContext, 0, true,
				scheduler.WithProfiles(cfg.Profiles...),
				scheduler.WithFrameworkOutOfTreeRegistry(registry),
			)
			defer teardown()

			_, err := testutils.CreateAndWaitForNodesInCache(testCtx, "test-node", st.MakeNode().Capacity(nodeRes), 1)
			if err != nil {
				t.Fatal(err)
			}

			permitPlugin.waitAndAllowPermit = true
			permitPlugin.waitingPod = "waiting-pod"

			if r := tt.runningPod; r != nil {
				if _, err := testutils.CreatePausePod(testCtx.ClientSet, r); err != nil {
					t.Fatalf("Error while creating the running pod: %v", err)
				}
				// Wait until the pod to be scheduled.
				if err = testutils.WaitForPodToSchedule(testCtx.Ctx, testCtx.ClientSet, r); err != nil {
					t.Fatalf("The running pod is expected to be scheduled: %v", err)
				}
			}

			if w := tt.waitingPod; w != nil {
				if _, err := testutils.CreatePausePod(testCtx.ClientSet, w); err != nil {
					t.Fatalf("Error while creating the waiting pod: %v", err)
				}
				// Wait until the waiting-pod is actually waiting.
				if err := wait.PollUntilContextTimeout(testCtx.Ctx, 10*time.Millisecond, 30*time.Second, false, func(ctx context.Context) (bool, error) {
					w := false
					permitPlugin.fh.IterateOverWaitingPods(func(wp framework.WaitingPod) { w = true })
					return w, nil
				}); err != nil {
					t.Fatalf("The waiting pod is expected to be waiting: %v", err)
				}
			}

			if p := tt.preemptor; p != nil {
				if _, err := testutils.CreatePausePod(testCtx.ClientSet, p); err != nil {
					t.Fatalf("Error while creating the preemptor pod: %v", err)
				}
				// Delete the waiting pod if specified.
				if w := tt.waitingPod; w != nil && tt.deleteWaitingPod {
					if err := testutils.DeletePod(testCtx.ClientSet, w.Name, w.Namespace); err != nil {
						t.Fatalf("Error while deleting the waiting pod: %v", err)
					}
				}
				if err = testutils.WaitForPodToSchedule(testCtx.Ctx, testCtx.ClientSet, p); err != nil {
					t.Fatalf("Expected the preemptor pod to be scheduled. error: %v", err)
				}
			}

			if w := tt.waitingPod; w != nil {
				if err := wait.PollUntilContextTimeout(testCtx.Ctx, 200*time.Millisecond, wait.ForeverTestTimeout, false, func(ctx context.Context) (bool, error) {
					w := false
					permitPlugin.fh.IterateOverWaitingPods(func(wp framework.WaitingPod) { w = true })
					return !w, nil
				}); err != nil {
					t.Fatalf("Expected the waiting pod to get preempted.")
				}

				p := filterPlugin.deepCopy()
				waitingPodCalled := p.numCalledPerPod[fmt.Sprintf("%v/%v", w.Namespace, w.Name)]
				if waitingPodCalled > tt.maxNumWaitingPodCalled {
					t.Fatalf("Expected the waiting pod to be called %v times at most, but got %v", tt.maxNumWaitingPodCalled, waitingPodCalled)
				}

				if !tt.deleteWaitingPod {
					// Expect the waitingPod to be still present.
					if _, err := testutils.GetPod(testCtx.ClientSet, w.Name, w.Namespace); err != nil {
						t.Error("Get waiting pod in waiting pod failed.")
					}
				}

				if permitPlugin.numPermitCalled == 0 {
					t.Errorf("Expected the permit plugin to be called.")
				}
			}

			if r := tt.runningPod; r != nil {
				// Expect the runningPod to be deleted physically.
				if _, err = testutils.GetPod(testCtx.ClientSet, r.Name, r.Namespace); err == nil {
					t.Error("The running pod still exists.")
				} else if !errors.IsNotFound(err) {
					t.Errorf("Get running pod failed: %v", err)
				}
			}
		})
	}
}

const (
	jobPluginName = "job plugin"
)

var _ framework.PreFilterPlugin = &JobPlugin{}
var _ framework.PostBindPlugin = &PostBindPlugin{}

type JobPlugin struct {
	podLister     listersv1.PodLister
	podsActivated bool
}

func (j *JobPlugin) Name() string {
	return jobPluginName
}

func (j *JobPlugin) PreFilter(_ context.Context, _ *framework.CycleState, p *v1.Pod) (*framework.PreFilterResult, *framework.Status) {
	labelSelector := labels.SelectorFromSet(labels.Set{"driver": ""})
	driverPods, err := j.podLister.Pods(p.Namespace).List(labelSelector)
	if err != nil {
		return nil, framework.AsStatus(err)
	}
	if len(driverPods) == 0 {
		return nil, framework.NewStatus(framework.UnschedulableAndUnresolvable, "unable to find driver pod")
	}
	return nil, nil
}

func (j *JobPlugin) PreFilterExtensions() framework.PreFilterExtensions {
	return nil
}

func (j *JobPlugin) PostBind(_ context.Context, state *framework.CycleState, p *v1.Pod, nodeName string) {
	if _, ok := p.Labels["driver"]; !ok {
		return
	}

	// If it's a driver pod, move other executor pods proactively to accelerating the scheduling.
	labelSelector := labels.SelectorFromSet(labels.Set{"executor": ""})
	podsToActivate, err := j.podLister.Pods(p.Namespace).List(labelSelector)
	if err == nil && len(podsToActivate) != 0 {
		c, err := state.Read(framework.PodsToActivateKey)
		if err == nil {
			if s, ok := c.(*framework.PodsToActivate); ok {
				s.Lock()
				for _, pod := range podsToActivate {
					namespacedName := fmt.Sprintf("%v/%v", pod.Namespace, pod.Name)
					s.Map[namespacedName] = pod
				}
				s.Unlock()
				j.podsActivated = true
			}
		}
	}
}

// This test simulates a typical spark job workflow.
//   - N executor pods are created, but kept pending due to missing the driver pod
//   - when the driver pod gets created and scheduled, proactively move the executors to activeQ
//     and thus accelerate the entire job workflow.
func TestActivatePods(t *testing.T) {
	var jobPlugin *JobPlugin
	// Create a plugin registry for testing. Register a Job plugin.
	registry := frameworkruntime.Registry{jobPluginName: func(_ context.Context, _ runtime.Object, fh framework.Handle) (framework.Plugin, error) {
		jobPlugin = &JobPlugin{podLister: fh.SharedInformerFactory().Core().V1().Pods().Lister()}
		return jobPlugin, nil
	}}

	// Setup initial filter plugin for testing.
	cfg := configtesting.V1ToInternalWithDefaults(t, configv1.KubeSchedulerConfiguration{
		Profiles: []configv1.KubeSchedulerProfile{{
			SchedulerName: pointer.String(v1.DefaultSchedulerName),
			Plugins: &configv1.Plugins{
				PreFilter: configv1.PluginSet{
					Enabled: []configv1.Plugin{
						{Name: jobPluginName},
					},
				},
				PostBind: configv1.PluginSet{
					Enabled: []configv1.Plugin{
						{Name: jobPluginName},
					},
				},
			},
		}},
	})

	// Create the API server and the scheduler with the test plugin set.
	testCtx, _ := schedulerutils.InitTestSchedulerForFrameworkTest(t, testutils.InitTestAPIServer(t, "job-plugin", nil), 1, true,
		scheduler.WithProfiles(cfg.Profiles...),
		scheduler.WithFrameworkOutOfTreeRegistry(registry))

	cs := testCtx.ClientSet
	ns := testCtx.NS.Name
	pause := imageutils.GetPauseImageName()

	// Firstly create 2 executor pods.
	var pods []*v1.Pod
	for i := 1; i <= 2; i++ {
		name := fmt.Sprintf("executor-%v", i)
		executor := st.MakePod().Name(name).Namespace(ns).Label("executor", "").Container(pause).Obj()
		pods = append(pods, executor)
		if _, err := cs.CoreV1().Pods(executor.Namespace).Create(testCtx.Ctx, executor, metav1.CreateOptions{}); err != nil {
			t.Fatalf("Failed to create pod %v: %v", executor.Name, err)
		}
	}

	// Wait for the 2 executor pods to be unschedulable.
	for _, pod := range pods {
		if err := testutils.WaitForPodUnschedulable(testCtx.Ctx, cs, pod); err != nil {
			t.Errorf("Failed to wait for Pod %v to be unschedulable: %v", pod.Name, err)
		}
	}

	// Create a driver pod.
	driver := st.MakePod().Name("driver").Namespace(ns).Label("driver", "").Container(pause).Obj()
	pods = append(pods, driver)
	if _, err := cs.CoreV1().Pods(driver.Namespace).Create(testCtx.Ctx, driver, metav1.CreateOptions{}); err != nil {
		t.Fatalf("Failed to create pod %v: %v", driver.Name, err)
	}

	// Verify all pods to be scheduled.
	for _, pod := range pods {
		if err := testutils.WaitForPodToScheduleWithTimeout(testCtx.Ctx, cs, pod, wait.ForeverTestTimeout); err != nil {
			t.Fatalf("Failed to wait for Pod %v to be schedulable: %v", pod.Name, err)
		}
	}

	// Lastly verify the pods activation logic is really called.
	if jobPlugin.podsActivated == false {
		t.Errorf("JobPlugin's pods activation logic is not called")
	}
}

var _ framework.PreEnqueuePlugin = &SchedulingGatesPluginWithEvents{}
var _ framework.EnqueueExtensions = &SchedulingGatesPluginWithEvents{}
var _ framework.PreEnqueuePlugin = &SchedulingGatesPluginWOEvents{}
var _ framework.EnqueueExtensions = &SchedulingGatesPluginWOEvents{}

const (
	schedulingGatesPluginWithEvents = "scheduling-gates-with-events"
	schedulingGatesPluginWOEvents   = "scheduling-gates-without-events"
)

type SchedulingGatesPluginWithEvents struct {
	called int
	schedulinggates.SchedulingGates
}

func (pl *SchedulingGatesPluginWithEvents) Name() string {
	return schedulingGatesPluginWithEvents
}

func (pl *SchedulingGatesPluginWithEvents) PreEnqueue(ctx context.Context, p *v1.Pod) *framework.Status {
	pl.called++
	return pl.SchedulingGates.PreEnqueue(ctx, p)
}

func (pl *SchedulingGatesPluginWithEvents) EventsToRegister(_ context.Context) ([]framework.ClusterEventWithHint, error) {
	return []framework.ClusterEventWithHint{
		{Event: framework.ClusterEvent{Resource: framework.Pod, ActionType: framework.Update}},
	}, nil
}

type SchedulingGatesPluginWOEvents struct {
	called int
	schedulinggates.SchedulingGates
}

func (pl *SchedulingGatesPluginWOEvents) Name() string {
	return schedulingGatesPluginWOEvents
}

func (pl *SchedulingGatesPluginWOEvents) PreEnqueue(ctx context.Context, p *v1.Pod) *framework.Status {
	pl.called++
	return pl.SchedulingGates.PreEnqueue(ctx, p)
}

func (pl *SchedulingGatesPluginWOEvents) EventsToRegister(_ context.Context) ([]framework.ClusterEventWithHint, error) {
	return nil, nil
}

// This test helps to verify registering nil events for PreEnqueue plugin works as expected.
func TestPreEnqueuePluginEventsToRegister(t *testing.T) {
	testContext := testutils.InitTestAPIServer(t, "preenqueue-plugin", nil)

	num := func(pl framework.Plugin) int {
		switch item := pl.(type) {
		case *SchedulingGatesPluginWithEvents:
			return item.called
		case *SchedulingGatesPluginWOEvents:
			return item.called
		default:
			t.Error("unsupported plugin")
		}
		return 0
	}

	tests := []struct {
		name       string
		withEvents bool
		// count is the expected number of calls to PreEnqueue().
		count             int
		queueHintEnabled  []bool
		expectedScheduled []bool
	}{
		{
			name:       "preEnqueue plugin without event registered",
			withEvents: false,
			count:      2,
			// This test case doesn't expect that the pod is scheduled again after the pod is updated
			// when queuehint is enabled, because it doesn't register any events in EventsToRegister.
			queueHintEnabled:  []bool{false, true},
			expectedScheduled: []bool{true, false},
		},
		{
			name:              "preEnqueue plugin with event registered",
			withEvents:        true,
			count:             3,
			queueHintEnabled:  []bool{false, true},
			expectedScheduled: []bool{true, true},
		},
	}

	for _, tt := range tests {
		for i := 0; i < len(tt.queueHintEnabled); i++ {
			queueHintEnabled := tt.queueHintEnabled[i]
			expectedScheduled := tt.expectedScheduled[i]

			t.Run(tt.name+fmt.Sprintf(" queueHint(%v)", queueHintEnabled), func(t *testing.T) {
				featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.SchedulerQueueingHints, queueHintEnabled)

				// use new plugin every time to clear counts
				var plugin framework.PreEnqueuePlugin
				if tt.withEvents {
					plugin = &SchedulingGatesPluginWithEvents{SchedulingGates: schedulinggates.SchedulingGates{}}
				} else {
					plugin = &SchedulingGatesPluginWOEvents{SchedulingGates: schedulinggates.SchedulingGates{}}
				}

				registry := frameworkruntime.Registry{
					plugin.Name(): newPlugin(plugin),
				}

				// Setup plugins for testing.
				cfg := configtesting.V1ToInternalWithDefaults(t, configv1.KubeSchedulerConfiguration{
					Profiles: []configv1.KubeSchedulerProfile{{
						SchedulerName: pointer.String(v1.DefaultSchedulerName),
						Plugins: &configv1.Plugins{
							PreEnqueue: configv1.PluginSet{
								Enabled: []configv1.Plugin{
									{Name: plugin.Name()},
								},
								Disabled: []configv1.Plugin{
									{Name: "*"},
								},
							},
						},
					}},
				})

				testCtx, teardown := schedulerutils.InitTestSchedulerForFrameworkTest(t, testContext, 2, true,
					scheduler.WithProfiles(cfg.Profiles...),
					scheduler.WithFrameworkOutOfTreeRegistry(registry),
				)
				defer teardown()

				// Create a pod with schedulingGates.
				gatedPod := st.MakePod().Name("p").Namespace(testContext.NS.Name).
					SchedulingGates([]string{"foo"}).
					PodAffinity("kubernetes.io/hostname", &metav1.LabelSelector{MatchLabels: map[string]string{"foo": "bar"}}, st.PodAffinityWithRequiredReq).
					Container("pause").Obj()
				gatedPod, err := testutils.CreatePausePod(testCtx.ClientSet, gatedPod)
				if err != nil {
					t.Errorf("Error while creating a gated pod: %v", err)
					return
				}

				if err := testutils.WaitForPodSchedulingGated(testCtx.Ctx, testCtx.ClientSet, gatedPod, 10*time.Second); err != nil {
					t.Errorf("Expected the pod to be gated, but got: %v", err)
					return
				}
				if num(plugin) != 1 {
					t.Errorf("Expected the preEnqueue plugin to be called once, but got %v", num(plugin))
					return
				}

				// Create a best effort pod.
				pausePod, err := testutils.CreatePausePod(testCtx.ClientSet, testutils.InitPausePod(&testutils.PausePodConfig{
					Name:      "pause-pod",
					Namespace: testCtx.NS.Name,
					Labels:    map[string]string{"foo": "bar"},
				}))
				if err != nil {
					t.Errorf("Error while creating a pod: %v", err)
					return
				}

				// Wait for the pod schedulabled.
				if err := testutils.WaitForPodToScheduleWithTimeout(testCtx.Ctx, testCtx.ClientSet, pausePod, 10*time.Second); err != nil {
					t.Errorf("Expected the pod to be schedulable, but got: %v", err)
					return
				}

				// Update the pod which will trigger the requeue logic if plugin registers the events.
				pausePod, err = testCtx.ClientSet.CoreV1().Pods(pausePod.Namespace).Get(testCtx.Ctx, pausePod.Name, metav1.GetOptions{})
				if err != nil {
					t.Errorf("Error while getting a pod: %v", err)
					return
				}
				pausePod.Annotations = map[string]string{"foo": "bar"}
				_, err = testCtx.ClientSet.CoreV1().Pods(pausePod.Namespace).Update(testCtx.Ctx, pausePod, metav1.UpdateOptions{})
				if err != nil {
					t.Errorf("Error while updating a pod: %v", err)
					return
				}

				// Pod should still be unschedulable because scheduling gates still exist, theoretically, it's a waste rescheduling.
				if err := testutils.WaitForPodSchedulingGated(testCtx.Ctx, testCtx.ClientSet, gatedPod, 10*time.Second); err != nil {
					t.Errorf("Expected the pod to be gated, but got: %v", err)
					return
				}
				if num(plugin) != tt.count {
					t.Errorf("Expected the preEnqueue plugin to be called %v, but got %v", tt.count, num(plugin))
					return
				}

				// Remove gated pod's scheduling gates.
				gatedPod, err = testCtx.ClientSet.CoreV1().Pods(gatedPod.Namespace).Get(testCtx.Ctx, gatedPod.Name, metav1.GetOptions{})
				if err != nil {
					t.Errorf("Error while getting a pod: %v", err)
					return
				}
				gatedPod.Spec.SchedulingGates = nil
				_, err = testCtx.ClientSet.CoreV1().Pods(gatedPod.Namespace).Update(testCtx.Ctx, gatedPod, metav1.UpdateOptions{})
				if err != nil {
					t.Errorf("Error while updating a pod: %v", err)
					return
				}

				if expectedScheduled {
					if err := testutils.WaitForPodToScheduleWithTimeout(testCtx.Ctx, testCtx.ClientSet, gatedPod, 10*time.Second); err != nil {
						t.Errorf("Expected the pod to be schedulable, but got: %v", err)
					}
					return
				}
				// wait for some time to ensure that the schedulerQueue has completed processing the podUpdate event.
				time.Sleep(time.Second)
				// pod shouldn't be scheduled if we didn't register podUpdate event for schedulingGates plugin
				if err := testutils.WaitForPodSchedulingGated(testCtx.Ctx, testCtx.ClientSet, gatedPod, 10*time.Second); err != nil {
					t.Errorf("Expected the pod to be gated, but got: %v", err)
					return
				}
			})
		}
	}
}
