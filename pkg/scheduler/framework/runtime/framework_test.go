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

package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/informers"
	clientsetfake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/component-base/metrics/testutil"
	"k8s.io/klog/v2/ktesting"
	"k8s.io/kubernetes/pkg/scheduler/apis/config"
	internalqueue "k8s.io/kubernetes/pkg/scheduler/backend/queue"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/metrics"
	"k8s.io/utils/ptr"
)

const (
	preEnqueuePlugin                  = "preEnqueue-plugin"
	queueSortPlugin                   = "no-op-queue-sort-plugin"
	scoreWithNormalizePlugin1         = "score-with-normalize-plugin-1"
	scoreWithNormalizePlugin2         = "score-with-normalize-plugin-2"
	scorePlugin1                      = "score-plugin-1"
	scorePlugin2                      = "score-plugin-2"
	pluginNotImplementingScore        = "plugin-not-implementing-score"
	preFilterPluginName               = "prefilter-plugin"
	preFilterWithExtensionsPluginName = "prefilter-with-extensions-plugin"
	duplicatePluginName               = "duplicate-plugin"
	testPlugin                        = "test-plugin"
	permitPlugin                      = "permit-plugin"
	bindPlugin                        = "bind-plugin"
	testCloseErrorPlugin              = "test-close-error-plugin"

	testProfileName              = "test-profile"
	testPercentageOfNodesToScore = 35
	nodeName                     = "testNode"

	injectReason       = "injected status"
	injectFilterReason = "injected filter status"
)

// TestScoreWithNormalizePlugin implements ScoreWithNormalizePlugin interface.
// TestScorePlugin only implements ScorePlugin interface.
var _ framework.ScorePlugin = &TestScoreWithNormalizePlugin{}
var _ framework.ScorePlugin = &TestScorePlugin{}

var statusCmpOpts = []cmp.Option{
	cmp.Comparer(func(s1 *framework.Status, s2 *framework.Status) bool {
		if s1 == nil || s2 == nil {
			return s1.IsSuccess() && s2.IsSuccess()
		}
		if s1.Code() == framework.Error {
			return s1.AsError().Error() == s2.AsError().Error()
		}
		return s1.Code() == s2.Code() && s1.Plugin() == s2.Plugin() && s1.Message() == s2.Message()
	}),
}

func newScoreWithNormalizePlugin1(_ context.Context, injArgs runtime.Object, f framework.Handle) (framework.Plugin, error) {
	var inj injectedResult
	if err := DecodeInto(injArgs, &inj); err != nil {
		return nil, err
	}
	return &TestScoreWithNormalizePlugin{scoreWithNormalizePlugin1, inj}, nil
}

func newScoreWithNormalizePlugin2(_ context.Context, injArgs runtime.Object, f framework.Handle) (framework.Plugin, error) {
	var inj injectedResult
	if err := DecodeInto(injArgs, &inj); err != nil {
		return nil, err
	}
	return &TestScoreWithNormalizePlugin{scoreWithNormalizePlugin2, inj}, nil
}

func newScorePlugin1(_ context.Context, injArgs runtime.Object, f framework.Handle) (framework.Plugin, error) {
	var inj injectedResult
	if err := DecodeInto(injArgs, &inj); err != nil {
		return nil, err
	}
	return &TestScorePlugin{scorePlugin1, inj}, nil
}

func newScorePlugin2(_ context.Context, injArgs runtime.Object, f framework.Handle) (framework.Plugin, error) {
	var inj injectedResult
	if err := DecodeInto(injArgs, &inj); err != nil {
		return nil, err
	}
	return &TestScorePlugin{scorePlugin2, inj}, nil
}

func newPluginNotImplementingScore(_ context.Context, _ runtime.Object, _ framework.Handle) (framework.Plugin, error) {
	return &PluginNotImplementingScore{}, nil
}

type TestScoreWithNormalizePlugin struct {
	name string
	inj  injectedResult
}

func (pl *TestScoreWithNormalizePlugin) Name() string {
	return pl.name
}

func (pl *TestScoreWithNormalizePlugin) NormalizeScore(ctx context.Context, state *framework.CycleState, pod *v1.Pod, scores framework.NodeScoreList) *framework.Status {
	return injectNormalizeRes(pl.inj, scores)
}

func (pl *TestScoreWithNormalizePlugin) Score(ctx context.Context, state *framework.CycleState, p *v1.Pod, nodeName string) (int64, *framework.Status) {
	return setScoreRes(pl.inj)
}

func (pl *TestScoreWithNormalizePlugin) ScoreExtensions() framework.ScoreExtensions {
	return pl
}

// TestScorePlugin only implements ScorePlugin interface.
type TestScorePlugin struct {
	name string
	inj  injectedResult
}

func (pl *TestScorePlugin) Name() string {
	return pl.name
}

func (pl *TestScorePlugin) PreScore(ctx context.Context, state *framework.CycleState, pod *v1.Pod, nodes []*framework.NodeInfo) *framework.Status {
	return framework.NewStatus(framework.Code(pl.inj.PreScoreStatus), injectReason)
}

func (pl *TestScorePlugin) Score(ctx context.Context, state *framework.CycleState, p *v1.Pod, nodeName string) (int64, *framework.Status) {
	return setScoreRes(pl.inj)
}

func (pl *TestScorePlugin) ScoreExtensions() framework.ScoreExtensions {
	return nil
}

// PluginNotImplementingScore doesn't implement the ScorePlugin interface.
type PluginNotImplementingScore struct{}

func (pl *PluginNotImplementingScore) Name() string {
	return pluginNotImplementingScore
}

func newTestPlugin(_ context.Context, injArgs runtime.Object, f framework.Handle) (framework.Plugin, error) {
	return &TestPlugin{name: testPlugin}, nil
}

// TestPlugin implements all Plugin interfaces.
type TestPlugin struct {
	name string
	inj  injectedResult
}

func (pl *TestPlugin) AddPod(ctx context.Context, state *framework.CycleState, podToSchedule *v1.Pod, podInfoToAdd *framework.PodInfo, nodeInfo *framework.NodeInfo) *framework.Status {
	return framework.NewStatus(framework.Code(pl.inj.PreFilterAddPodStatus), injectReason)
}
func (pl *TestPlugin) RemovePod(ctx context.Context, state *framework.CycleState, podToSchedule *v1.Pod, podInfoToRemove *framework.PodInfo, nodeInfo *framework.NodeInfo) *framework.Status {
	return framework.NewStatus(framework.Code(pl.inj.PreFilterRemovePodStatus), injectReason)
}

func (pl *TestPlugin) Name() string {
	return pl.name
}

func (pl *TestPlugin) Less(*framework.QueuedPodInfo, *framework.QueuedPodInfo) bool {
	return false
}

func (pl *TestPlugin) Score(ctx context.Context, state *framework.CycleState, p *v1.Pod, nodeName string) (int64, *framework.Status) {
	return 0, framework.NewStatus(framework.Code(pl.inj.ScoreStatus), injectReason)
}

func (pl *TestPlugin) ScoreExtensions() framework.ScoreExtensions {
	return nil
}

func (pl *TestPlugin) PreFilter(ctx context.Context, state *framework.CycleState, p *v1.Pod) (*framework.PreFilterResult, *framework.Status) {
	return pl.inj.PreFilterResult, framework.NewStatus(framework.Code(pl.inj.PreFilterStatus), injectReason)
}

func (pl *TestPlugin) PreFilterExtensions() framework.PreFilterExtensions {
	return pl
}

func (pl *TestPlugin) Filter(ctx context.Context, state *framework.CycleState, pod *v1.Pod, nodeInfo *framework.NodeInfo) *framework.Status {
	return framework.NewStatus(framework.Code(pl.inj.FilterStatus), injectFilterReason)
}

func (pl *TestPlugin) PostFilter(_ context.Context, _ *framework.CycleState, _ *v1.Pod, _ framework.NodeToStatusReader) (*framework.PostFilterResult, *framework.Status) {
	return nil, framework.NewStatus(framework.Code(pl.inj.PostFilterStatus), injectReason)
}

func (pl *TestPlugin) PreScore(ctx context.Context, state *framework.CycleState, pod *v1.Pod, nodes []*framework.NodeInfo) *framework.Status {
	return framework.NewStatus(framework.Code(pl.inj.PreScoreStatus), injectReason)
}

func (pl *TestPlugin) Reserve(ctx context.Context, state *framework.CycleState, p *v1.Pod, nodeName string) *framework.Status {
	return framework.NewStatus(framework.Code(pl.inj.ReserveStatus), injectReason)
}

func (pl *TestPlugin) Unreserve(ctx context.Context, state *framework.CycleState, p *v1.Pod, nodeName string) {
}

func (pl *TestPlugin) PreBind(ctx context.Context, state *framework.CycleState, p *v1.Pod, nodeName string) *framework.Status {
	return framework.NewStatus(framework.Code(pl.inj.PreBindStatus), injectReason)
}

func (pl *TestPlugin) PostBind(ctx context.Context, state *framework.CycleState, p *v1.Pod, nodeName string) {
}

func (pl *TestPlugin) Permit(ctx context.Context, state *framework.CycleState, p *v1.Pod, nodeName string) (*framework.Status, time.Duration) {
	return framework.NewStatus(framework.Code(pl.inj.PermitStatus), injectReason), time.Duration(0)
}

func (pl *TestPlugin) Bind(ctx context.Context, state *framework.CycleState, p *v1.Pod, nodeName string) *framework.Status {
	return framework.NewStatus(framework.Code(pl.inj.BindStatus), injectReason)
}

func newTestCloseErrorPlugin(_ context.Context, injArgs runtime.Object, f framework.Handle) (framework.Plugin, error) {
	return &TestCloseErrorPlugin{name: testCloseErrorPlugin}, nil
}

// TestCloseErrorPlugin implements for Close test.
type TestCloseErrorPlugin struct {
	name string
}

func (pl *TestCloseErrorPlugin) Name() string {
	return pl.name
}

var errClose = errors.New("close err")

func (pl *TestCloseErrorPlugin) Close() error {
	return errClose
}

// TestPreFilterPlugin only implements PreFilterPlugin interface.
type TestPreFilterPlugin struct {
	PreFilterCalled int
}

func (pl *TestPreFilterPlugin) Name() string {
	return preFilterPluginName
}

func (pl *TestPreFilterPlugin) PreFilter(ctx context.Context, state *framework.CycleState, p *v1.Pod) (*framework.PreFilterResult, *framework.Status) {
	pl.PreFilterCalled++
	return nil, nil
}

func (pl *TestPreFilterPlugin) PreFilterExtensions() framework.PreFilterExtensions {
	return nil
}

// TestPreFilterWithExtensionsPlugin implements Add/Remove interfaces.
type TestPreFilterWithExtensionsPlugin struct {
	PreFilterCalled int
	AddCalled       int
	RemoveCalled    int
}

func (pl *TestPreFilterWithExtensionsPlugin) Name() string {
	return preFilterWithExtensionsPluginName
}

func (pl *TestPreFilterWithExtensionsPlugin) PreFilter(ctx context.Context, state *framework.CycleState, p *v1.Pod) (*framework.PreFilterResult, *framework.Status) {
	pl.PreFilterCalled++
	return nil, nil
}

func (pl *TestPreFilterWithExtensionsPlugin) AddPod(ctx context.Context, state *framework.CycleState, podToSchedule *v1.Pod,
	podInfoToAdd *framework.PodInfo, nodeInfo *framework.NodeInfo) *framework.Status {
	pl.AddCalled++
	return nil
}

func (pl *TestPreFilterWithExtensionsPlugin) RemovePod(ctx context.Context, state *framework.CycleState, podToSchedule *v1.Pod,
	podInfoToRemove *framework.PodInfo, nodeInfo *framework.NodeInfo) *framework.Status {
	pl.RemoveCalled++
	return nil
}

func (pl *TestPreFilterWithExtensionsPlugin) PreFilterExtensions() framework.PreFilterExtensions {
	return pl
}

type TestDuplicatePlugin struct {
}

func (dp *TestDuplicatePlugin) Name() string {
	return duplicatePluginName
}

func (dp *TestDuplicatePlugin) PreFilter(ctx context.Context, state *framework.CycleState, p *v1.Pod) (*framework.PreFilterResult, *framework.Status) {
	return nil, nil
}

func (dp *TestDuplicatePlugin) PreFilterExtensions() framework.PreFilterExtensions {
	return nil
}

var _ framework.PreFilterPlugin = &TestDuplicatePlugin{}

func newDuplicatePlugin(_ context.Context, _ runtime.Object, _ framework.Handle) (framework.Plugin, error) {
	return &TestDuplicatePlugin{}, nil
}

// TestPermitPlugin only implements PermitPlugin interface.
type TestPermitPlugin struct {
	PreFilterCalled int
}

func (pp *TestPermitPlugin) Name() string {
	return permitPlugin
}
func (pp *TestPermitPlugin) Permit(ctx context.Context, state *framework.CycleState, p *v1.Pod, nodeName string) (*framework.Status, time.Duration) {
	return framework.NewStatus(framework.Wait), 10 * time.Second
}

var _ framework.PreEnqueuePlugin = &TestPreEnqueuePlugin{}

type TestPreEnqueuePlugin struct{}

func (pl *TestPreEnqueuePlugin) Name() string {
	return preEnqueuePlugin
}

func (pl *TestPreEnqueuePlugin) PreEnqueue(ctx context.Context, p *v1.Pod) *framework.Status {
	return nil
}

var _ framework.QueueSortPlugin = &TestQueueSortPlugin{}

func newQueueSortPlugin(_ context.Context, _ runtime.Object, _ framework.Handle) (framework.Plugin, error) {
	return &TestQueueSortPlugin{}, nil
}

// TestQueueSortPlugin is a no-op implementation for QueueSort extension point.
type TestQueueSortPlugin struct{}

func (pl *TestQueueSortPlugin) Name() string {
	return queueSortPlugin
}

func (pl *TestQueueSortPlugin) Less(_, _ *framework.QueuedPodInfo) bool {
	return false
}

var _ framework.BindPlugin = &TestBindPlugin{}

func newBindPlugin(_ context.Context, _ runtime.Object, _ framework.Handle) (framework.Plugin, error) {
	return &TestBindPlugin{}, nil
}

// TestBindPlugin is a no-op implementation for Bind extension point.
type TestBindPlugin struct{}

func (t TestBindPlugin) Name() string {
	return bindPlugin
}

func (t TestBindPlugin) Bind(ctx context.Context, state *framework.CycleState, p *v1.Pod, nodeName string) *framework.Status {
	return nil
}

// nolint:errcheck   // Ignore the error returned by Register as before
var registry = func() Registry {
	r := make(Registry)
	r.Register(scoreWithNormalizePlugin1, newScoreWithNormalizePlugin1)
	r.Register(scoreWithNormalizePlugin2, newScoreWithNormalizePlugin2)
	r.Register(scorePlugin1, newScorePlugin1)
	r.Register(scorePlugin2, newScorePlugin2)
	r.Register(pluginNotImplementingScore, newPluginNotImplementingScore)
	r.Register(duplicatePluginName, newDuplicatePlugin)
	r.Register(testPlugin, newTestPlugin)
	r.Register(queueSortPlugin, newQueueSortPlugin)
	r.Register(bindPlugin, newBindPlugin)
	r.Register(testCloseErrorPlugin, newTestCloseErrorPlugin)
	return r
}()

var defaultWeights = map[string]int32{
	scoreWithNormalizePlugin1: 1,
	scoreWithNormalizePlugin2: 2,
	scorePlugin1:              1,
}

var state = &framework.CycleState{}

// Pod is only used for logging errors.
var pod = &v1.Pod{}
var node = &v1.Node{
	ObjectMeta: metav1.ObjectMeta{
		Name: nodeName,
	},
}
var lowPriority, highPriority = int32(0), int32(1000)
var lowPriorityPod = &v1.Pod{
	ObjectMeta: metav1.ObjectMeta{UID: "low"},
	Spec:       v1.PodSpec{Priority: &lowPriority},
}
var highPriorityPod = &v1.Pod{
	ObjectMeta: metav1.ObjectMeta{UID: "high"},
	Spec:       v1.PodSpec{Priority: &highPriority},
}
var nodes = []*v1.Node{
	{ObjectMeta: metav1.ObjectMeta{Name: "node1"}},
	{ObjectMeta: metav1.ObjectMeta{Name: "node2"}},
}

var (
	errInjectedStatus       = errors.New(injectReason)
	errInjectedFilterStatus = errors.New(injectFilterReason)
)

func newFrameworkWithQueueSortAndBind(ctx context.Context, r Registry, profile config.KubeSchedulerProfile, opts ...Option) (framework.Framework, error) {
	if _, ok := r[queueSortPlugin]; !ok {
		r[queueSortPlugin] = newQueueSortPlugin
	}
	if _, ok := r[bindPlugin]; !ok {
		r[bindPlugin] = newBindPlugin
	}

	if len(profile.Plugins.QueueSort.Enabled) == 0 {
		profile.Plugins.QueueSort.Enabled = append(profile.Plugins.QueueSort.Enabled, config.Plugin{Name: queueSortPlugin})
	}
	if len(profile.Plugins.Bind.Enabled) == 0 {
		profile.Plugins.Bind.Enabled = append(profile.Plugins.Bind.Enabled, config.Plugin{Name: bindPlugin})
	}
	return NewFramework(ctx, r, &profile, opts...)
}

func TestInitFrameworkWithScorePlugins(t *testing.T) {
	metrics.Register()
	tests := []struct {
		name    string
		plugins *config.Plugins
		// If initErr is true, we expect framework initialization to fail.
		initErr bool
	}{
		{
			name:    "enabled Score plugin doesn't exist in registry",
			plugins: buildScoreConfigDefaultWeights("notExist"),
			initErr: true,
		},
		{
			name:    "enabled Score plugin doesn't extend the ScorePlugin interface",
			plugins: buildScoreConfigDefaultWeights(pluginNotImplementingScore),
			initErr: true,
		},
		{
			name:    "Score plugins are nil",
			plugins: &config.Plugins{},
		},
		{
			name:    "enabled Score plugin list is empty",
			plugins: buildScoreConfigDefaultWeights(),
		},
		{
			name:    "enabled plugin only implements ScorePlugin interface",
			plugins: buildScoreConfigDefaultWeights(scorePlugin1),
		},
		{
			name:    "enabled plugin implements ScoreWithNormalizePlugin interface",
			plugins: buildScoreConfigDefaultWeights(scoreWithNormalizePlugin1),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			profile := config.KubeSchedulerProfile{Plugins: tt.plugins}
			_, ctx := ktesting.NewTestContext(t)
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			_, err := newFrameworkWithQueueSortAndBind(ctx, registry, profile)
			if tt.initErr && err == nil {
				t.Fatal("Framework initialization should fail")
			}
			if !tt.initErr && err != nil {
				t.Fatalf("Failed to create framework for testing: %v", err)
			}
		})
	}
}

func TestNewFrameworkErrors(t *testing.T) {
	tests := []struct {
		name      string
		plugins   *config.Plugins
		pluginCfg []config.PluginConfig
		wantErr   string
	}{
		{
			name: "duplicate plugin name",
			plugins: &config.Plugins{
				PreFilter: config.PluginSet{
					Enabled: []config.Plugin{
						{Name: duplicatePluginName, Weight: 1},
						{Name: duplicatePluginName, Weight: 1},
					},
				},
			},
			pluginCfg: []config.PluginConfig{
				{Name: duplicatePluginName},
			},
			wantErr: "already registered",
		},
		{
			name: "duplicate plugin config",
			plugins: &config.Plugins{
				PreFilter: config.PluginSet{
					Enabled: []config.Plugin{
						{Name: duplicatePluginName, Weight: 1},
					},
				},
			},
			pluginCfg: []config.PluginConfig{
				{Name: duplicatePluginName},
				{Name: duplicatePluginName},
			},
			wantErr: "repeated config for plugin",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, ctx := ktesting.NewTestContext(t)
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			profile := &config.KubeSchedulerProfile{
				Plugins:      tc.plugins,
				PluginConfig: tc.pluginCfg,
			}
			_, err := NewFramework(ctx, registry, profile)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("Unexpected error, got %v, expect: %s", err, tc.wantErr)
			}
		})
	}
}

func TestNewFrameworkMultiPointExpansion(t *testing.T) {
	tests := []struct {
		name        string
		plugins     *config.Plugins
		wantPlugins *config.Plugins
		wantErr     string
	}{
		{
			name: "plugin expansion",
			plugins: &config.Plugins{
				MultiPoint: config.PluginSet{
					Enabled: []config.Plugin{
						{Name: testPlugin, Weight: 5},
					},
				},
			},
			wantPlugins: &config.Plugins{
				QueueSort:  config.PluginSet{Enabled: []config.Plugin{{Name: testPlugin}}},
				PreFilter:  config.PluginSet{Enabled: []config.Plugin{{Name: testPlugin}}},
				Filter:     config.PluginSet{Enabled: []config.Plugin{{Name: testPlugin}}},
				PostFilter: config.PluginSet{Enabled: []config.Plugin{{Name: testPlugin}}},
				PreScore:   config.PluginSet{Enabled: []config.Plugin{{Name: testPlugin}}},
				Score:      config.PluginSet{Enabled: []config.Plugin{{Name: testPlugin, Weight: 5}}},
				Reserve:    config.PluginSet{Enabled: []config.Plugin{{Name: testPlugin}}},
				Permit:     config.PluginSet{Enabled: []config.Plugin{{Name: testPlugin}}},
				PreBind:    config.PluginSet{Enabled: []config.Plugin{{Name: testPlugin}}},
				Bind:       config.PluginSet{Enabled: []config.Plugin{{Name: testPlugin}}},
				PostBind:   config.PluginSet{Enabled: []config.Plugin{{Name: testPlugin}}},
			},
		},
		{
			name: "disable MultiPoint plugin at some extension points",
			plugins: &config.Plugins{
				MultiPoint: config.PluginSet{
					Enabled: []config.Plugin{
						{Name: testPlugin},
					},
				},
				PreScore: config.PluginSet{
					Disabled: []config.Plugin{
						{Name: testPlugin},
					},
				},
				Score: config.PluginSet{
					Disabled: []config.Plugin{
						{Name: testPlugin},
					},
				},
			},
			wantPlugins: &config.Plugins{
				QueueSort:  config.PluginSet{Enabled: []config.Plugin{{Name: testPlugin}}},
				PreFilter:  config.PluginSet{Enabled: []config.Plugin{{Name: testPlugin}}},
				Filter:     config.PluginSet{Enabled: []config.Plugin{{Name: testPlugin}}},
				PostFilter: config.PluginSet{Enabled: []config.Plugin{{Name: testPlugin}}},
				Reserve:    config.PluginSet{Enabled: []config.Plugin{{Name: testPlugin}}},
				Permit:     config.PluginSet{Enabled: []config.Plugin{{Name: testPlugin}}},
				PreBind:    config.PluginSet{Enabled: []config.Plugin{{Name: testPlugin}}},
				Bind:       config.PluginSet{Enabled: []config.Plugin{{Name: testPlugin}}},
				PostBind:   config.PluginSet{Enabled: []config.Plugin{{Name: testPlugin}}},
			},
		},
		{
			name: "Multiple MultiPoint plugins",
			plugins: &config.Plugins{
				MultiPoint: config.PluginSet{
					Enabled: []config.Plugin{
						{Name: testPlugin},
						{Name: scorePlugin1},
					},
				},
			},
			wantPlugins: &config.Plugins{
				QueueSort:  config.PluginSet{Enabled: []config.Plugin{{Name: testPlugin}}},
				PreFilter:  config.PluginSet{Enabled: []config.Plugin{{Name: testPlugin}}},
				Filter:     config.PluginSet{Enabled: []config.Plugin{{Name: testPlugin}}},
				PostFilter: config.PluginSet{Enabled: []config.Plugin{{Name: testPlugin}}},
				PreScore: config.PluginSet{Enabled: []config.Plugin{
					{Name: testPlugin},
					{Name: scorePlugin1},
				}},
				Score: config.PluginSet{Enabled: []config.Plugin{
					{Name: testPlugin, Weight: 1},
					{Name: scorePlugin1, Weight: 1},
				}},
				Reserve:  config.PluginSet{Enabled: []config.Plugin{{Name: testPlugin}}},
				Permit:   config.PluginSet{Enabled: []config.Plugin{{Name: testPlugin}}},
				PreBind:  config.PluginSet{Enabled: []config.Plugin{{Name: testPlugin}}},
				Bind:     config.PluginSet{Enabled: []config.Plugin{{Name: testPlugin}}},
				PostBind: config.PluginSet{Enabled: []config.Plugin{{Name: testPlugin}}},
			},
		},
		{
			name: "disable MultiPoint extension",
			plugins: &config.Plugins{
				MultiPoint: config.PluginSet{
					Enabled: []config.Plugin{
						{Name: testPlugin},
						{Name: scorePlugin1},
					},
				},
				PreScore: config.PluginSet{
					Disabled: []config.Plugin{
						{Name: "*"},
					},
				},
			},
			wantPlugins: &config.Plugins{
				QueueSort:  config.PluginSet{Enabled: []config.Plugin{{Name: testPlugin}}},
				PreFilter:  config.PluginSet{Enabled: []config.Plugin{{Name: testPlugin}}},
				Filter:     config.PluginSet{Enabled: []config.Plugin{{Name: testPlugin}}},
				PostFilter: config.PluginSet{Enabled: []config.Plugin{{Name: testPlugin}}},
				Score: config.PluginSet{Enabled: []config.Plugin{
					{Name: testPlugin, Weight: 1},
					{Name: scorePlugin1, Weight: 1},
				}},
				Reserve:  config.PluginSet{Enabled: []config.Plugin{{Name: testPlugin}}},
				Permit:   config.PluginSet{Enabled: []config.Plugin{{Name: testPlugin}}},
				PreBind:  config.PluginSet{Enabled: []config.Plugin{{Name: testPlugin}}},
				Bind:     config.PluginSet{Enabled: []config.Plugin{{Name: testPlugin}}},
				PostBind: config.PluginSet{Enabled: []config.Plugin{{Name: testPlugin}}},
			},
		},
		{
			name: "Reorder MultiPoint plugins (specified extension takes precedence)",
			plugins: &config.Plugins{
				MultiPoint: config.PluginSet{
					Enabled: []config.Plugin{
						{Name: scoreWithNormalizePlugin1},
						{Name: testPlugin},
						{Name: scorePlugin1},
					},
				},
				Score: config.PluginSet{
					Enabled: []config.Plugin{
						{Name: scorePlugin1},
						{Name: testPlugin},
					},
				},
			},
			wantPlugins: &config.Plugins{
				QueueSort:  config.PluginSet{Enabled: []config.Plugin{{Name: testPlugin}}},
				PreFilter:  config.PluginSet{Enabled: []config.Plugin{{Name: testPlugin}}},
				Filter:     config.PluginSet{Enabled: []config.Plugin{{Name: testPlugin}}},
				PostFilter: config.PluginSet{Enabled: []config.Plugin{{Name: testPlugin}}},
				PreScore: config.PluginSet{Enabled: []config.Plugin{
					{Name: testPlugin},
					{Name: scorePlugin1},
				}},
				Score: config.PluginSet{Enabled: []config.Plugin{
					{Name: scorePlugin1, Weight: 1},
					{Name: testPlugin, Weight: 1},
					{Name: scoreWithNormalizePlugin1, Weight: 1},
				}},
				Reserve:  config.PluginSet{Enabled: []config.Plugin{{Name: testPlugin}}},
				Permit:   config.PluginSet{Enabled: []config.Plugin{{Name: testPlugin}}},
				PreBind:  config.PluginSet{Enabled: []config.Plugin{{Name: testPlugin}}},
				Bind:     config.PluginSet{Enabled: []config.Plugin{{Name: testPlugin}}},
				PostBind: config.PluginSet{Enabled: []config.Plugin{{Name: testPlugin}}},
			},
		},
		{
			name: "Reorder MultiPoint plugins (specified extension only takes precedence when it exists in MultiPoint)",
			plugins: &config.Plugins{
				MultiPoint: config.PluginSet{
					Enabled: []config.Plugin{
						{Name: testPlugin},
						{Name: scorePlugin1},
					},
				},
				Score: config.PluginSet{
					Enabled: []config.Plugin{
						{Name: scoreWithNormalizePlugin1},
						{Name: scorePlugin1},
						{Name: testPlugin},
					},
				},
			},
			wantPlugins: &config.Plugins{
				QueueSort:  config.PluginSet{Enabled: []config.Plugin{{Name: testPlugin}}},
				PreFilter:  config.PluginSet{Enabled: []config.Plugin{{Name: testPlugin}}},
				Filter:     config.PluginSet{Enabled: []config.Plugin{{Name: testPlugin}}},
				PostFilter: config.PluginSet{Enabled: []config.Plugin{{Name: testPlugin}}},
				PreScore: config.PluginSet{Enabled: []config.Plugin{
					{Name: testPlugin},
					{Name: scorePlugin1},
				}},
				Score: config.PluginSet{Enabled: []config.Plugin{
					{Name: scorePlugin1, Weight: 1},
					{Name: testPlugin, Weight: 1},
					{Name: scoreWithNormalizePlugin1, Weight: 1},
				}},
				Reserve:  config.PluginSet{Enabled: []config.Plugin{{Name: testPlugin}}},
				Permit:   config.PluginSet{Enabled: []config.Plugin{{Name: testPlugin}}},
				PreBind:  config.PluginSet{Enabled: []config.Plugin{{Name: testPlugin}}},
				Bind:     config.PluginSet{Enabled: []config.Plugin{{Name: testPlugin}}},
				PostBind: config.PluginSet{Enabled: []config.Plugin{{Name: testPlugin}}},
			},
		},
		{
			name: "Override MultiPoint plugins weights",
			plugins: &config.Plugins{
				MultiPoint: config.PluginSet{
					Enabled: []config.Plugin{
						{Name: testPlugin},
						{Name: scorePlugin1},
					},
				},
				Score: config.PluginSet{
					Enabled: []config.Plugin{
						{Name: scorePlugin1, Weight: 5},
						{Name: testPlugin, Weight: 3},
					},
				},
			},
			wantPlugins: &config.Plugins{
				QueueSort:  config.PluginSet{Enabled: []config.Plugin{{Name: testPlugin}}},
				PreFilter:  config.PluginSet{Enabled: []config.Plugin{{Name: testPlugin}}},
				Filter:     config.PluginSet{Enabled: []config.Plugin{{Name: testPlugin}}},
				PostFilter: config.PluginSet{Enabled: []config.Plugin{{Name: testPlugin}}},
				PreScore: config.PluginSet{Enabled: []config.Plugin{
					{Name: testPlugin},
					{Name: scorePlugin1},
				}},
				Score: config.PluginSet{Enabled: []config.Plugin{
					{Name: scorePlugin1, Weight: 5},
					{Name: testPlugin, Weight: 3},
				}},
				Reserve:  config.PluginSet{Enabled: []config.Plugin{{Name: testPlugin}}},
				Permit:   config.PluginSet{Enabled: []config.Plugin{{Name: testPlugin}}},
				PreBind:  config.PluginSet{Enabled: []config.Plugin{{Name: testPlugin}}},
				Bind:     config.PluginSet{Enabled: []config.Plugin{{Name: testPlugin}}},
				PostBind: config.PluginSet{Enabled: []config.Plugin{{Name: testPlugin}}},
			},
		},
		{
			name: "disable and enable MultiPoint plugins with '*'",
			plugins: &config.Plugins{
				MultiPoint: config.PluginSet{
					Enabled: []config.Plugin{
						{Name: queueSortPlugin},
						{Name: bindPlugin},
						{Name: scorePlugin1},
					},
					Disabled: []config.Plugin{
						{Name: "*"},
					},
				},
			},
			wantPlugins: &config.Plugins{
				QueueSort: config.PluginSet{Enabled: []config.Plugin{{Name: queueSortPlugin}}},
				PreScore: config.PluginSet{Enabled: []config.Plugin{
					{Name: scorePlugin1},
				}},
				Score: config.PluginSet{Enabled: []config.Plugin{
					{Name: scorePlugin1, Weight: 1},
				}},
				Bind: config.PluginSet{Enabled: []config.Plugin{{Name: bindPlugin}}},
			},
		},
		{
			name: "disable and enable MultiPoint plugin by name",
			plugins: &config.Plugins{
				MultiPoint: config.PluginSet{
					Enabled: []config.Plugin{
						{Name: bindPlugin},
						{Name: queueSortPlugin},
						{Name: scorePlugin1},
					},
					Disabled: []config.Plugin{
						{Name: scorePlugin1},
					},
				},
			},
			wantPlugins: &config.Plugins{
				QueueSort: config.PluginSet{Enabled: []config.Plugin{{Name: queueSortPlugin}}},
				PreScore: config.PluginSet{Enabled: []config.Plugin{
					{Name: scorePlugin1},
				}},
				Score: config.PluginSet{Enabled: []config.Plugin{
					{Name: scorePlugin1, Weight: 1},
				}},
				Bind: config.PluginSet{Enabled: []config.Plugin{{Name: bindPlugin}}},
			},
		},
		{
			name: "Expect 'already registered' error",
			plugins: &config.Plugins{
				MultiPoint: config.PluginSet{
					Enabled: []config.Plugin{
						{Name: testPlugin},
						{Name: testPlugin},
					},
				},
			},
			wantErr: "already registered",
		},
		{
			name: "Override MultiPoint plugins weights and avoid a plugin being loaded multiple times",
			plugins: &config.Plugins{
				MultiPoint: config.PluginSet{
					Enabled: []config.Plugin{
						{Name: testPlugin},
						{Name: scorePlugin1},
					},
				},
				Score: config.PluginSet{
					Enabled: []config.Plugin{
						{Name: scorePlugin1, Weight: 5},
						{Name: scorePlugin2, Weight: 5},
						{Name: testPlugin, Weight: 3},
					},
				},
			},
			wantPlugins: &config.Plugins{
				QueueSort:  config.PluginSet{Enabled: []config.Plugin{{Name: testPlugin}}},
				PreFilter:  config.PluginSet{Enabled: []config.Plugin{{Name: testPlugin}}},
				Filter:     config.PluginSet{Enabled: []config.Plugin{{Name: testPlugin}}},
				PostFilter: config.PluginSet{Enabled: []config.Plugin{{Name: testPlugin}}},
				PreScore: config.PluginSet{Enabled: []config.Plugin{
					{Name: testPlugin},
					{Name: scorePlugin1},
				}},
				Score: config.PluginSet{Enabled: []config.Plugin{
					{Name: scorePlugin1, Weight: 5},
					{Name: testPlugin, Weight: 3},
					{Name: scorePlugin2, Weight: 5},
				}},
				Reserve:  config.PluginSet{Enabled: []config.Plugin{{Name: testPlugin}}},
				Permit:   config.PluginSet{Enabled: []config.Plugin{{Name: testPlugin}}},
				PreBind:  config.PluginSet{Enabled: []config.Plugin{{Name: testPlugin}}},
				Bind:     config.PluginSet{Enabled: []config.Plugin{{Name: testPlugin}}},
				PostBind: config.PluginSet{Enabled: []config.Plugin{{Name: testPlugin}}},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, ctx := ktesting.NewTestContext(t)
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			fw, err := NewFramework(ctx, registry, &config.KubeSchedulerProfile{Plugins: tc.plugins})
			defer func() {
				if fw != nil {
					_ = fw.Close()
				}
			}()
			if err != nil {
				if tc.wantErr == "" || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("Unexpected error, got %v, expect: %s", err, tc.wantErr)
				}
			} else {
				if tc.wantErr != "" {
					t.Fatalf("Unexpected error, got %v, expect: %s", err, tc.wantErr)
				}
			}
			if tc.wantErr == "" {
				if diff := cmp.Diff(tc.wantPlugins, fw.ListPlugins()); diff != "" {
					t.Fatalf("Unexpected eventToPlugin map (-want,+got):\n%s", diff)
				}
			}
		})
	}
}

func TestPreEnqueuePlugins(t *testing.T) {
	tests := []struct {
		name    string
		plugins []framework.Plugin
		want    []framework.PreEnqueuePlugin
	}{
		{
			name: "no PreEnqueuePlugin registered",
		},
		{
			name: "one PreEnqueuePlugin registered",
			plugins: []framework.Plugin{
				&TestPreEnqueuePlugin{},
			},
			want: []framework.PreEnqueuePlugin{
				&TestPreEnqueuePlugin{},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, ctx := ktesting.NewTestContext(t)
			registry := Registry{}
			cfgPls := &config.Plugins{}
			for _, pl := range tt.plugins {
				// register all plugins
				tmpPl := pl
				if err := registry.Register(pl.Name(),
					func(_ context.Context, _ runtime.Object, _ framework.Handle) (framework.Plugin, error) {
						return tmpPl, nil
					}); err != nil {
					t.Fatalf("fail to register preEnqueue plugin (%s)", pl.Name())
				}
				// append plugins to filter pluginset
				cfgPls.PreEnqueue.Enabled = append(
					cfgPls.PreEnqueue.Enabled,
					config.Plugin{Name: pl.Name()},
				)
			}
			profile := config.KubeSchedulerProfile{Plugins: cfgPls}
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			f, err := newFrameworkWithQueueSortAndBind(ctx, registry, profile)
			if err != nil {
				t.Fatalf("fail to create framework: %s", err)
			}
			defer func() {
				_ = f.Close()
			}()
			got := f.PreEnqueuePlugins()
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("Unexpected PreEnqueuePlugins(): (-want,+got):\n%s", diff)
			}
		})
	}
}

func TestRunPreScorePlugins(t *testing.T) {
	tests := []struct {
		name               string
		plugins            []*TestPlugin
		wantSkippedPlugins sets.Set[string]
		wantStatusCode     framework.Code
	}{
		{
			name: "all PreScorePlugins returned success",
			plugins: []*TestPlugin{
				{
					name: "success1",
				},
				{
					name: "success2",
				},
			},
			wantStatusCode: framework.Success,
		},
		{
			name: "one PreScore plugin returned success, but another PreScore plugin returned non-success",
			plugins: []*TestPlugin{
				{
					name: "success",
				},
				{
					name: "error",
					inj:  injectedResult{PreScoreStatus: int(framework.Error)},
				},
			},
			wantStatusCode: framework.Error,
		},
		{
			name: "one PreScore plugin returned skip, but another PreScore plugin returned non-success",
			plugins: []*TestPlugin{
				{
					name: "skip",
					inj:  injectedResult{PreScoreStatus: int(framework.Skip)},
				},
				{
					name: "error",
					inj:  injectedResult{PreScoreStatus: int(framework.Error)},
				},
			},
			wantSkippedPlugins: sets.New("skip"),
			wantStatusCode:     framework.Error,
		},
		{
			name: "all PreScore plugins returned skip",
			plugins: []*TestPlugin{
				{
					name: "skip1",
					inj:  injectedResult{PreScoreStatus: int(framework.Skip)},
				},
				{
					name: "skip2",
					inj:  injectedResult{PreScoreStatus: int(framework.Skip)},
				},
				{
					name: "skip3",
					inj:  injectedResult{PreScoreStatus: int(framework.Skip)},
				},
			},
			wantSkippedPlugins: sets.New("skip1", "skip2", "skip3"),
			wantStatusCode:     framework.Success,
		},
		{
			name: "some PreScore plugins returned skip",
			plugins: []*TestPlugin{
				{
					name: "skip1",
					inj:  injectedResult{PreScoreStatus: int(framework.Skip)},
				},
				{
					name: "success1",
				},
				{
					name: "skip2",
					inj:  injectedResult{PreScoreStatus: int(framework.Skip)},
				},
				{
					name: "success2",
				},
			},
			wantSkippedPlugins: sets.New("skip1", "skip2"),
			wantStatusCode:     framework.Success,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := make(Registry)
			enabled := make([]config.Plugin, len(tt.plugins))
			for i, p := range tt.plugins {
				p := p
				enabled[i].Name = p.name
				if err := r.Register(p.name, func(_ context.Context, _ runtime.Object, fh framework.Handle) (framework.Plugin, error) {
					return p, nil
				}); err != nil {
					t.Fatalf("fail to register PreScorePlugins plugin (%s)", p.Name())
				}
			}

			_, ctx := ktesting.NewTestContext(t)
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()

			f, err := newFrameworkWithQueueSortAndBind(
				ctx,
				r,
				config.KubeSchedulerProfile{Plugins: &config.Plugins{PreScore: config.PluginSet{Enabled: enabled}}},
			)
			if err != nil {
				t.Fatalf("Failed to create framework for testing: %v", err)
			}
			defer func() {
				_ = f.Close()
			}()
			state := framework.NewCycleState()
			status := f.RunPreScorePlugins(ctx, state, nil, nil)
			if status.Code() != tt.wantStatusCode {
				t.Errorf("wrong status code. got: %v, want: %v", status, tt.wantStatusCode)
			}
			skipped := state.SkipScorePlugins
			if diff := cmp.Diff(tt.wantSkippedPlugins, skipped); diff != "" {
				t.Errorf("wrong skip score plugins (-want, +got):\n%s", diff)
			}
		})
	}
}

func TestRunScorePlugins(t *testing.T) {
	tests := []struct {
		name           string
		registry       Registry
		plugins        *config.Plugins
		pluginConfigs  []config.PluginConfig
		want           []framework.NodePluginScores
		skippedPlugins sets.Set[string]
		// If err is true, we expect RunScorePlugin to fail.
		err bool
	}{
		{
			name:    "no Score plugins",
			plugins: buildScoreConfigDefaultWeights(),
			want: []framework.NodePluginScores{
				{
					Name:   "node1",
					Scores: []framework.PluginScore{},
				},
				{
					Name:   "node2",
					Scores: []framework.PluginScore{},
				},
			},
		},
		{
			name:    "single Score plugin",
			plugins: buildScoreConfigDefaultWeights(scorePlugin1),
			pluginConfigs: []config.PluginConfig{
				{
					Name: scorePlugin1,
					Args: &runtime.Unknown{
						Raw: []byte(`{ "scoreRes": 1 }`),
					},
				},
			},
			// scorePlugin1 Score returns 1, weight=1, so want=1.
			want: []framework.NodePluginScores{
				{
					Name: "node1",
					Scores: []framework.PluginScore{
						{
							Name:  scorePlugin1,
							Score: 1,
						},
					},
					TotalScore: 1,
				},
				{
					Name: "node2",
					Scores: []framework.PluginScore{
						{
							Name:  scorePlugin1,
							Score: 1,
						},
					},
					TotalScore: 1,
				},
			},
		},
		{
			name: "single ScoreWithNormalize plugin",
			// registry: registry,
			plugins: buildScoreConfigDefaultWeights(scoreWithNormalizePlugin1),
			pluginConfigs: []config.PluginConfig{
				{
					Name: scoreWithNormalizePlugin1,
					Args: &runtime.Unknown{
						Raw: []byte(`{ "scoreRes": 10, "normalizeRes": 5 }`),
					},
				},
			},
			// scoreWithNormalizePlugin1 Score returns 10, but NormalizeScore overrides to 5, weight=1, so want=5
			want: []framework.NodePluginScores{
				{
					Name: "node1",
					Scores: []framework.PluginScore{
						{
							Name:  scoreWithNormalizePlugin1,
							Score: 5,
						},
					},
					TotalScore: 5,
				},
				{
					Name: "node2",
					Scores: []framework.PluginScore{
						{
							Name:  scoreWithNormalizePlugin1,
							Score: 5,
						},
					},
					TotalScore: 5,
				},
			},
		},
		{
			name:    "3 Score plugins, 2 NormalizeScore plugins",
			plugins: buildScoreConfigDefaultWeights(scorePlugin1, scoreWithNormalizePlugin1, scoreWithNormalizePlugin2),
			pluginConfigs: []config.PluginConfig{
				{
					Name: scorePlugin1,
					Args: &runtime.Unknown{
						Raw: []byte(`{ "scoreRes": 1 }`),
					},
				},
				{
					Name: scoreWithNormalizePlugin1,
					Args: &runtime.Unknown{
						Raw: []byte(`{ "scoreRes": 3, "normalizeRes": 4}`),
					},
				},
				{
					Name: scoreWithNormalizePlugin2,
					Args: &runtime.Unknown{
						Raw: []byte(`{ "scoreRes": 4, "normalizeRes": 5}`),
					},
				},
			},
			// scorePlugin1 Score returns 1, weight =1, so want=1.
			// scoreWithNormalizePlugin1 Score returns 3, but NormalizeScore overrides to 4, weight=1, so want=4.
			// scoreWithNormalizePlugin2 Score returns 4, but NormalizeScore overrides to 5, weight=2, so want=10.
			want: []framework.NodePluginScores{
				{
					Name: "node1",
					Scores: []framework.PluginScore{
						{
							Name:  scorePlugin1,
							Score: 1,
						},
						{
							Name:  scoreWithNormalizePlugin1,
							Score: 4,
						},
						{
							Name:  scoreWithNormalizePlugin2,
							Score: 10,
						},
					},
					TotalScore: 15,
				},
				{
					Name: "node2",
					Scores: []framework.PluginScore{
						{
							Name:  scorePlugin1,
							Score: 1,
						},
						{
							Name:  scoreWithNormalizePlugin1,
							Score: 4,
						},
						{
							Name:  scoreWithNormalizePlugin2,
							Score: 10,
						},
					},
					TotalScore: 15,
				},
			},
		},
		{
			name: "score fails",
			pluginConfigs: []config.PluginConfig{
				{
					Name: scoreWithNormalizePlugin1,
					Args: &runtime.Unknown{
						Raw: []byte(`{ "scoreStatus": 1 }`),
					},
				},
			},
			plugins: buildScoreConfigDefaultWeights(scorePlugin1, scoreWithNormalizePlugin1),
			err:     true,
		},
		{
			name: "normalize fails",
			pluginConfigs: []config.PluginConfig{
				{
					Name: scoreWithNormalizePlugin1,
					Args: &runtime.Unknown{
						Raw: []byte(`{ "normalizeStatus": 1 }`),
					},
				},
			},
			plugins: buildScoreConfigDefaultWeights(scorePlugin1, scoreWithNormalizePlugin1),
			err:     true,
		},
		{
			name:    "Score plugin return score greater than MaxNodeScore",
			plugins: buildScoreConfigDefaultWeights(scorePlugin1),
			pluginConfigs: []config.PluginConfig{
				{
					Name: scorePlugin1,
					Args: &runtime.Unknown{
						Raw: []byte(fmt.Sprintf(`{ "scoreRes": %d }`, framework.MaxNodeScore+1)),
					},
				},
			},
			err: true,
		},
		{
			name:    "Score plugin return score less than MinNodeScore",
			plugins: buildScoreConfigDefaultWeights(scorePlugin1),
			pluginConfigs: []config.PluginConfig{
				{
					Name: scorePlugin1,
					Args: &runtime.Unknown{
						Raw: []byte(fmt.Sprintf(`{ "scoreRes": %d }`, framework.MinNodeScore-1)),
					},
				},
			},
			err: true,
		},
		{
			name:    "ScoreWithNormalize plugin return score greater than MaxNodeScore",
			plugins: buildScoreConfigDefaultWeights(scoreWithNormalizePlugin1),
			pluginConfigs: []config.PluginConfig{
				{
					Name: scoreWithNormalizePlugin1,
					Args: &runtime.Unknown{
						Raw: []byte(fmt.Sprintf(`{ "normalizeRes": %d }`, framework.MaxNodeScore+1)),
					},
				},
			},
			err: true,
		},
		{
			name:    "ScoreWithNormalize plugin return score less than MinNodeScore",
			plugins: buildScoreConfigDefaultWeights(scoreWithNormalizePlugin1),
			pluginConfigs: []config.PluginConfig{
				{
					Name: scoreWithNormalizePlugin1,
					Args: &runtime.Unknown{
						Raw: []byte(fmt.Sprintf(`{ "normalizeRes": %d }`, framework.MinNodeScore-1)),
					},
				},
			},
			err: true,
		},
		{
			name: "single Score plugin with MultiPointExpansion",
			plugins: &config.Plugins{
				MultiPoint: config.PluginSet{
					Enabled: []config.Plugin{
						{Name: scorePlugin1},
					},
				},
				Score: config.PluginSet{
					Enabled: []config.Plugin{
						{Name: scorePlugin1, Weight: 3},
					},
				},
			},
			pluginConfigs: []config.PluginConfig{
				{
					Name: scorePlugin1,
					Args: &runtime.Unknown{
						Raw: []byte(`{ "scoreRes": 1 }`),
					},
				},
			},
			// scorePlugin1 Score returns 1, weight=3, so want=3.
			want: []framework.NodePluginScores{
				{
					Name: "node1",
					Scores: []framework.PluginScore{
						{
							Name:  scorePlugin1,
							Score: 3,
						},
					},
					TotalScore: 3,
				},
				{
					Name: "node2",
					Scores: []framework.PluginScore{
						{
							Name:  scorePlugin1,
							Score: 3,
						},
					},
					TotalScore: 3,
				},
			},
		},
		{
			name:    "one success plugin, one skip plugin",
			plugins: buildScoreConfigDefaultWeights(scorePlugin1, scoreWithNormalizePlugin1),
			pluginConfigs: []config.PluginConfig{
				{
					Name: scorePlugin1,
					Args: &runtime.Unknown{
						Raw: []byte(`{ "scoreRes": 1 }`),
					},
				},
				{
					Name: scoreWithNormalizePlugin1,
					Args: &runtime.Unknown{
						Raw: []byte(`{ "scoreStatus": 1 }`), // To make sure this plugin isn't called, set error as an injected result.
					},
				},
			},
			skippedPlugins: sets.New(scoreWithNormalizePlugin1),
			want: []framework.NodePluginScores{
				{
					Name: "node1",
					Scores: []framework.PluginScore{
						{
							Name:  scorePlugin1,
							Score: 1,
						},
					},
					TotalScore: 1,
				},
				{
					Name: "node2",
					Scores: []framework.PluginScore{
						{
							Name:  scorePlugin1,
							Score: 1,
						},
					},
					TotalScore: 1,
				},
			},
		},
		{
			name:    "all plugins are skipped in prescore",
			plugins: buildScoreConfigDefaultWeights(scorePlugin1),
			pluginConfigs: []config.PluginConfig{
				{
					Name: scorePlugin1,
					Args: &runtime.Unknown{
						Raw: []byte(`{ "scoreStatus": 1 }`), // To make sure this plugin isn't called, set error as an injected result.
					},
				},
			},
			skippedPlugins: sets.New(scorePlugin1),
			want: []framework.NodePluginScores{
				{
					Name:   "node1",
					Scores: []framework.PluginScore{},
				},
				{
					Name:   "node2",
					Scores: []framework.PluginScore{},
				},
			},
		},
		{
			name:           "skipped prescore plugin number greater than the number of score plugins",
			plugins:        buildScoreConfigDefaultWeights(scorePlugin1),
			pluginConfigs:  nil,
			skippedPlugins: sets.New(scorePlugin1, "score-plugin-unknown"),
			want: []framework.NodePluginScores{
				{
					Name:   "node1",
					Scores: []framework.PluginScore{},
				},
				{
					Name:   "node2",
					Scores: []framework.PluginScore{},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Inject the results via Args in PluginConfig.
			profile := config.KubeSchedulerProfile{
				Plugins:      tt.plugins,
				PluginConfig: tt.pluginConfigs,
			}
			_, ctx := ktesting.NewTestContext(t)
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			f, err := newFrameworkWithQueueSortAndBind(ctx, registry, profile)
			if err != nil {
				t.Fatalf("Failed to create framework for testing: %v", err)
			}
			defer func() {
				_ = f.Close()
			}()

			state := framework.NewCycleState()
			state.SkipScorePlugins = tt.skippedPlugins
			res, status := f.RunScorePlugins(ctx, state, pod, BuildNodeInfos(nodes))

			if tt.err {
				if status.IsSuccess() {
					t.Errorf("Expected status to be non-success. got: %v", status.Code().String())
				}
				return
			}

			if !status.IsSuccess() {
				t.Errorf("Expected status to be success.")
			}
			if diff := cmp.Diff(tt.want, res); diff != "" {
				t.Errorf("Score map after RunScorePlugin (-want,+got):\n%s", diff)
			}
		})
	}
}

func TestPreFilterPlugins(t *testing.T) {
	preFilter1 := &TestPreFilterPlugin{}
	preFilter2 := &TestPreFilterWithExtensionsPlugin{}
	r := make(Registry)
	r.Register(preFilterPluginName,
		func(_ context.Context, _ runtime.Object, fh framework.Handle) (framework.Plugin, error) {
			return preFilter1, nil
		})
	r.Register(preFilterWithExtensionsPluginName,
		func(_ context.Context, _ runtime.Object, fh framework.Handle) (framework.Plugin, error) {
			return preFilter2, nil
		})
	plugins := &config.Plugins{PreFilter: config.PluginSet{Enabled: []config.Plugin{{Name: preFilterWithExtensionsPluginName}, {Name: preFilterPluginName}}}}
	t.Run("TestPreFilterPlugin", func(t *testing.T) {
		profile := config.KubeSchedulerProfile{Plugins: plugins}
		_, ctx := ktesting.NewTestContext(t)
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()

		f, err := newFrameworkWithQueueSortAndBind(ctx, r, profile)
		if err != nil {
			t.Fatalf("Failed to create framework for testing: %v", err)
		}
		defer func() {
			_ = f.Close()
		}()
		state := framework.NewCycleState()
		f.RunPreFilterPlugins(ctx, state, nil)
		f.RunPreFilterExtensionAddPod(ctx, state, nil, nil, nil)
		f.RunPreFilterExtensionRemovePod(ctx, state, nil, nil, nil)

		if preFilter1.PreFilterCalled != 1 {
			t.Errorf("preFilter1 called %v, expected: 1", preFilter1.PreFilterCalled)
		}
		if preFilter2.PreFilterCalled != 1 {
			t.Errorf("preFilter2 called %v, expected: 1", preFilter2.PreFilterCalled)
		}
		if preFilter2.AddCalled != 1 {
			t.Errorf("AddPod called %v, expected: 1", preFilter2.AddCalled)
		}
		if preFilter2.RemoveCalled != 1 {
			t.Errorf("AddPod called %v, expected: 1", preFilter2.RemoveCalled)
		}
	})
}

func TestRunPreFilterPlugins(t *testing.T) {
	tests := []struct {
		name                string
		plugins             []*TestPlugin
		wantPreFilterResult *framework.PreFilterResult
		wantSkippedPlugins  sets.Set[string]
		wantStatusCode      framework.Code
	}{
		{
			name: "all PreFilter returned success",
			plugins: []*TestPlugin{
				{
					name: "success1",
				},
				{
					name: "success2",
				},
			},
			wantPreFilterResult: nil,
			wantStatusCode:      framework.Success,
		},
		{
			name: "one PreFilter plugin returned success, but another PreFilter plugin returned non-success",
			plugins: []*TestPlugin{
				{
					name: "success",
				},
				{
					name: "error",
					inj:  injectedResult{PreFilterStatus: int(framework.Error)},
				},
			},
			wantPreFilterResult: nil,
			wantStatusCode:      framework.Error,
		},
		{
			name: "one PreFilter plugin returned skip, but another PreFilter plugin returned non-success",
			plugins: []*TestPlugin{
				{
					name: "skip",
					inj:  injectedResult{PreFilterStatus: int(framework.Skip)},
				},
				{
					name: "error",
					inj:  injectedResult{PreFilterStatus: int(framework.Error)},
				},
			},
			wantSkippedPlugins: sets.New("skip"),
			wantStatusCode:     framework.Error,
		},
		{
			name: "all PreFilter plugins returned skip",
			plugins: []*TestPlugin{
				{
					name: "skip1",
					inj:  injectedResult{PreFilterStatus: int(framework.Skip)},
				},
				{
					name: "skip2",
					inj:  injectedResult{PreFilterStatus: int(framework.Skip)},
				},
				{
					name: "skip3",
					inj:  injectedResult{PreFilterStatus: int(framework.Skip)},
				},
			},
			wantPreFilterResult: nil,
			wantSkippedPlugins:  sets.New("skip1", "skip2", "skip3"),
			wantStatusCode:      framework.Success,
		},
		{
			name: "some PreFilter plugins returned skip",
			plugins: []*TestPlugin{
				{
					name: "skip1",
					inj:  injectedResult{PreFilterStatus: int(framework.Skip)},
				},
				{
					name: "success1",
				},
				{
					name: "skip2",
					inj:  injectedResult{PreFilterStatus: int(framework.Skip)},
				},
				{
					name: "success2",
				},
			},
			wantPreFilterResult: nil,
			wantSkippedPlugins:  sets.New("skip1", "skip2"),
			wantStatusCode:      framework.Success,
		},
		{
			name: "one PreFilter plugin returned Unschedulable, but another PreFilter plugin should be executed",
			plugins: []*TestPlugin{
				{
					name: "unschedulable",
					inj:  injectedResult{PreFilterStatus: int(framework.Unschedulable)},
				},
				{
					// to make sure this plugin is executed, this plugin return Skip and we confirm it via wantSkippedPlugins.
					name: "skip",
					inj:  injectedResult{PreFilterStatus: int(framework.Skip)},
				},
			},
			wantPreFilterResult: nil,
			wantSkippedPlugins:  sets.New("skip"),
			wantStatusCode:      framework.Unschedulable,
		},
		{
			name: "one PreFilter plugin returned UnschedulableAndUnresolvable, and all other plugins aren't executed",
			plugins: []*TestPlugin{
				{
					name: "unresolvable",
					inj:  injectedResult{PreFilterStatus: int(framework.UnschedulableAndUnresolvable)},
				},
				{
					// to make sure this plugin is not executed, this plugin return Skip and we confirm it via wantSkippedPlugins.
					name: "skip",
					inj:  injectedResult{PreFilterStatus: int(framework.Skip)},
				},
			},
			wantPreFilterResult: nil,
			wantStatusCode:      framework.UnschedulableAndUnresolvable,
		},
		{
			name: "all nodes are filtered out by prefilter result, but other plugins aren't executed because we consider all nodes are filtered out by UnschedulableAndUnresolvable",
			plugins: []*TestPlugin{
				{
					name: "reject-all-nodes",
					inj:  injectedResult{PreFilterResult: &framework.PreFilterResult{NodeNames: sets.New[string]()}},
				},
				{
					// to make sure this plugin is not executed, this plugin return Skip and we confirm it via wantSkippedPlugins.
					name: "skip",
					inj:  injectedResult{PreFilterStatus: int(framework.Skip)},
				},
			},
			wantPreFilterResult: &framework.PreFilterResult{NodeNames: sets.New[string]()},
			wantSkippedPlugins:  sets.New[string](), // "skip" plugin isn't executed.
			wantStatusCode:      framework.UnschedulableAndUnresolvable,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := make(Registry)
			enabled := make([]config.Plugin, len(tt.plugins))
			for i, p := range tt.plugins {
				p := p
				enabled[i].Name = p.name
				if err := r.Register(p.name, func(_ context.Context, _ runtime.Object, fh framework.Handle) (framework.Plugin, error) {
					return p, nil
				}); err != nil {
					t.Fatalf("fail to register PreFilter plugin (%s)", p.Name())
				}
			}

			_, ctx := ktesting.NewTestContext(t)
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()

			f, err := newFrameworkWithQueueSortAndBind(
				ctx,
				r,
				config.KubeSchedulerProfile{Plugins: &config.Plugins{PreFilter: config.PluginSet{Enabled: enabled}}},
			)
			if err != nil {
				t.Fatalf("Failed to create framework for testing: %v", err)
			}
			defer func() {
				_ = f.Close()
			}()

			state := framework.NewCycleState()
			result, status, _ := f.RunPreFilterPlugins(ctx, state, nil)
			if diff := cmp.Diff(tt.wantPreFilterResult, result); diff != "" {
				t.Errorf("wrong status (-want,+got):\n%s", diff)
			}
			if status.Code() != tt.wantStatusCode {
				t.Errorf("wrong status code. got: %v, want: %v", status, tt.wantStatusCode)
			}
			skipped := state.SkipFilterPlugins
			if diff := cmp.Diff(tt.wantSkippedPlugins, skipped); diff != "" {
				t.Errorf("wrong skip filter plugins (-want,+got):\n%s", diff)
			}
		})
	}
}

func TestRunPreFilterExtensionRemovePod(t *testing.T) {
	tests := []struct {
		name               string
		plugins            []*TestPlugin
		skippedPluginNames sets.Set[string]
		wantStatusCode     framework.Code
	}{
		{
			name: "no plugins are skipped and all RemovePod() returned success",
			plugins: []*TestPlugin{
				{
					name: "success1",
				},
				{
					name: "success2",
				},
			},
			wantStatusCode: framework.Success,
		},
		{
			name: "one RemovePod() returned error",
			plugins: []*TestPlugin{
				{
					name: "success1",
				},
				{
					name: "error1",
					inj:  injectedResult{PreFilterRemovePodStatus: int(framework.Error)},
				},
			},
			wantStatusCode: framework.Error,
		},
		{
			name: "one RemovePod() is skipped",
			plugins: []*TestPlugin{
				{
					name: "success1",
				},
				{
					name: "skipped",
					// To confirm it's skipped, return error so that this test case will fail when it isn't skipped.
					inj: injectedResult{PreFilterRemovePodStatus: int(framework.Error)},
				},
			},
			skippedPluginNames: sets.New("skipped"),
			wantStatusCode:     framework.Success,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := make(Registry)
			enabled := make([]config.Plugin, len(tt.plugins))
			for i, p := range tt.plugins {
				p := p
				enabled[i].Name = p.name
				if err := r.Register(p.name, func(_ context.Context, _ runtime.Object, fh framework.Handle) (framework.Plugin, error) {
					return p, nil
				}); err != nil {
					t.Fatalf("fail to register PreFilterExtension plugin (%s)", p.Name())
				}
			}

			_, ctx := ktesting.NewTestContext(t)
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()

			f, err := newFrameworkWithQueueSortAndBind(
				ctx,
				r,
				config.KubeSchedulerProfile{Plugins: &config.Plugins{PreFilter: config.PluginSet{Enabled: enabled}}},
			)
			if err != nil {
				t.Fatalf("Failed to create framework for testing: %v", err)
			}
			defer func() {
				_ = f.Close()
			}()

			state := framework.NewCycleState()
			state.SkipFilterPlugins = tt.skippedPluginNames
			status := f.RunPreFilterExtensionRemovePod(ctx, state, nil, nil, nil)
			if status.Code() != tt.wantStatusCode {
				t.Errorf("wrong status code. got: %v, want: %v", status, tt.wantStatusCode)
			}
		})
	}
}

func TestRunPreFilterExtensionAddPod(t *testing.T) {
	tests := []struct {
		name               string
		plugins            []*TestPlugin
		skippedPluginNames sets.Set[string]
		wantStatusCode     framework.Code
	}{
		{
			name: "no plugins are skipped and all AddPod() returned success",
			plugins: []*TestPlugin{
				{
					name: "success1",
				},
				{
					name: "success2",
				},
			},
			wantStatusCode: framework.Success,
		},
		{
			name: "one AddPod() returned error",
			plugins: []*TestPlugin{
				{
					name: "success1",
				},
				{
					name: "error1",
					inj:  injectedResult{PreFilterAddPodStatus: int(framework.Error)},
				},
			},
			wantStatusCode: framework.Error,
		},
		{
			name: "one AddPod() is skipped",
			plugins: []*TestPlugin{
				{
					name: "success1",
				},
				{
					name: "skipped",
					// To confirm it's skipped, return error so that this test case will fail when it isn't skipped.
					inj: injectedResult{PreFilterAddPodStatus: int(framework.Error)},
				},
			},
			skippedPluginNames: sets.New("skipped"),
			wantStatusCode:     framework.Success,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := make(Registry)
			enabled := make([]config.Plugin, len(tt.plugins))
			for i, p := range tt.plugins {
				p := p
				enabled[i].Name = p.name
				if err := r.Register(p.name, func(_ context.Context, _ runtime.Object, fh framework.Handle) (framework.Plugin, error) {
					return p, nil
				}); err != nil {
					t.Fatalf("fail to register PreFilterExtension plugin (%s)", p.Name())
				}
			}

			_, ctx := ktesting.NewTestContext(t)
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()

			f, err := newFrameworkWithQueueSortAndBind(
				ctx,
				r,
				config.KubeSchedulerProfile{Plugins: &config.Plugins{PreFilter: config.PluginSet{Enabled: enabled}}},
			)
			if err != nil {
				t.Fatalf("Failed to create framework for testing: %v", err)
			}
			defer func() {
				_ = f.Close()
			}()

			state := framework.NewCycleState()
			state.SkipFilterPlugins = tt.skippedPluginNames
			status := f.RunPreFilterExtensionAddPod(ctx, state, nil, nil, nil)
			if status.Code() != tt.wantStatusCode {
				t.Errorf("wrong status code. got: %v, want: %v", status, tt.wantStatusCode)
			}
		})
	}
}

func TestFilterPlugins(t *testing.T) {
	tests := []struct {
		name           string
		plugins        []*TestPlugin
		skippedPlugins sets.Set[string]
		wantStatus     *framework.Status
	}{
		{
			name: "SuccessFilter",
			plugins: []*TestPlugin{
				{
					name: "TestPlugin",
					inj:  injectedResult{FilterStatus: int(framework.Success)},
				},
			},
			wantStatus: nil,
		},
		{
			name: "ErrorFilter",
			plugins: []*TestPlugin{
				{
					name: "TestPlugin",
					inj:  injectedResult{FilterStatus: int(framework.Error)},
				},
			},
			wantStatus: framework.AsStatus(fmt.Errorf(`running "TestPlugin" filter plugin: %w`, errInjectedFilterStatus)).WithPlugin("TestPlugin"),
		},
		{
			name: "UnschedulableFilter",
			plugins: []*TestPlugin{
				{
					name: "TestPlugin",
					inj:  injectedResult{FilterStatus: int(framework.Unschedulable)},
				},
			},
			wantStatus: framework.NewStatus(framework.Unschedulable, injectFilterReason).WithPlugin("TestPlugin"),
		},
		{
			name: "UnschedulableAndUnresolvableFilter",
			plugins: []*TestPlugin{
				{
					name: "TestPlugin",
					inj: injectedResult{
						FilterStatus: int(framework.UnschedulableAndUnresolvable)},
				},
			},
			wantStatus: framework.NewStatus(framework.UnschedulableAndUnresolvable, injectFilterReason).WithPlugin("TestPlugin"),
		},
		// following tests cover multiple-plugins scenarios
		{
			name: "ErrorAndErrorFilters",
			plugins: []*TestPlugin{
				{
					name: "TestPlugin1",
					inj:  injectedResult{FilterStatus: int(framework.Error)},
				},
				{
					name: "TestPlugin2",
					inj:  injectedResult{FilterStatus: int(framework.Error)},
				},
			},
			wantStatus: framework.AsStatus(fmt.Errorf(`running "TestPlugin1" filter plugin: %w`, errInjectedFilterStatus)).WithPlugin("TestPlugin1"),
		},
		{
			name: "UnschedulableAndUnschedulableFilters",
			plugins: []*TestPlugin{
				{
					name: "TestPlugin1",
					inj:  injectedResult{FilterStatus: int(framework.Unschedulable)},
				},
				{
					name: "TestPlugin2",
					inj:  injectedResult{FilterStatus: int(framework.Unschedulable)},
				},
			},
			wantStatus: framework.NewStatus(framework.Unschedulable, injectFilterReason).WithPlugin("TestPlugin1"),
		},
		{
			name: "UnschedulableAndUnschedulableAndUnresolvableFilters",
			plugins: []*TestPlugin{
				{
					name: "TestPlugin1",
					inj:  injectedResult{FilterStatus: int(framework.UnschedulableAndUnresolvable)},
				},
				{
					name: "TestPlugin2",
					inj:  injectedResult{FilterStatus: int(framework.Unschedulable)},
				},
			},
			wantStatus: framework.NewStatus(framework.UnschedulableAndUnresolvable, injectFilterReason).WithPlugin("TestPlugin1"),
		},
		{
			name: "SuccessAndSuccessFilters",
			plugins: []*TestPlugin{
				{
					name: "TestPlugin1",
					inj:  injectedResult{FilterStatus: int(framework.Success)},
				},
				{
					name: "TestPlugin2",
					inj:  injectedResult{FilterStatus: int(framework.Success)},
				},
			},
			wantStatus: nil,
		},
		{
			name: "SuccessAndSkipFilters",
			plugins: []*TestPlugin{
				{
					name: "TestPlugin1",
					inj:  injectedResult{FilterStatus: int(framework.Success)},
				},

				{
					name: "TestPlugin2",
					inj:  injectedResult{FilterStatus: int(framework.Error)}, // To make sure this plugins isn't called, set error as an injected result.
				},
			},
			wantStatus:     nil,
			skippedPlugins: sets.New("TestPlugin2"),
		},
		{
			name: "ErrorAndSuccessFilters",
			plugins: []*TestPlugin{
				{
					name: "TestPlugin1",
					inj:  injectedResult{FilterStatus: int(framework.Error)},
				},
				{
					name: "TestPlugin2",
					inj:  injectedResult{FilterStatus: int(framework.Success)},
				},
			},
			wantStatus: framework.AsStatus(fmt.Errorf(`running "TestPlugin1" filter plugin: %w`, errInjectedFilterStatus)).WithPlugin("TestPlugin1"),
		},
		{
			name: "SuccessAndErrorFilters",
			plugins: []*TestPlugin{
				{

					name: "TestPlugin1",
					inj:  injectedResult{FilterStatus: int(framework.Success)},
				},
				{
					name: "TestPlugin2",
					inj:  injectedResult{FilterStatus: int(framework.Error)},
				},
			},
			wantStatus: framework.AsStatus(fmt.Errorf(`running "TestPlugin2" filter plugin: %w`, errInjectedFilterStatus)).WithPlugin("TestPlugin2"),
		},
		{
			name: "SuccessAndUnschedulableFilters",
			plugins: []*TestPlugin{
				{
					name: "TestPlugin1",
					inj:  injectedResult{FilterStatus: int(framework.Success)},
				},
				{
					name: "TestPlugin2",
					inj:  injectedResult{FilterStatus: int(framework.Unschedulable)},
				},
			},
			wantStatus: framework.NewStatus(framework.Unschedulable, injectFilterReason).WithPlugin("TestPlugin2"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, ctx := ktesting.NewTestContext(t)
			registry := Registry{}
			cfgPls := &config.Plugins{}
			for _, pl := range tt.plugins {
				// register all plugins
				tmpPl := pl
				if err := registry.Register(pl.name,
					func(_ context.Context, _ runtime.Object, _ framework.Handle) (framework.Plugin, error) {
						return tmpPl, nil
					}); err != nil {
					t.Fatalf("fail to register filter plugin (%s)", pl.name)
				}
				// append plugins to filter pluginset
				cfgPls.Filter.Enabled = append(
					cfgPls.Filter.Enabled,
					config.Plugin{Name: pl.name})
			}
			profile := config.KubeSchedulerProfile{Plugins: cfgPls}
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()

			f, err := newFrameworkWithQueueSortAndBind(ctx, registry, profile)
			if err != nil {
				t.Fatalf("fail to create framework: %s", err)
			}
			defer func() {
				_ = f.Close()
			}()
			state := framework.NewCycleState()
			state.SkipFilterPlugins = tt.skippedPlugins
			gotStatus := f.RunFilterPlugins(ctx, state, pod, nil)
			if diff := cmp.Diff(tt.wantStatus, gotStatus, statusCmpOpts...); diff != "" {
				t.Errorf("Unexpected status: (-want,+got):\n%s", diff)
			}
		})
	}
}

func TestPostFilterPlugins(t *testing.T) {
	tests := []struct {
		name       string
		plugins    []*TestPlugin
		wantStatus *framework.Status
	}{
		{
			name: "a single plugin makes a Pod schedulable",
			plugins: []*TestPlugin{
				{
					name: "TestPlugin",
					inj:  injectedResult{PostFilterStatus: int(framework.Success)},
				},
			},
			wantStatus: framework.NewStatus(framework.Success, injectReason),
		},
		{
			name: "plugin1 failed to make a Pod schedulable, followed by plugin2 which makes the Pod schedulable",
			plugins: []*TestPlugin{
				{
					name: "TestPlugin1",
					inj:  injectedResult{PostFilterStatus: int(framework.Unschedulable)},
				},
				{
					name: "TestPlugin2",
					inj:  injectedResult{PostFilterStatus: int(framework.Success)},
				},
			},
			wantStatus: framework.NewStatus(framework.Success, injectReason),
		},
		{
			name: "plugin1 makes a Pod schedulable, followed by plugin2 which cannot make the Pod schedulable",
			plugins: []*TestPlugin{
				{
					name: "TestPlugin1",
					inj:  injectedResult{PostFilterStatus: int(framework.Success)},
				},
				{
					name: "TestPlugin2",
					inj:  injectedResult{PostFilterStatus: int(framework.Unschedulable)},
				},
			},
			wantStatus: framework.NewStatus(framework.Success, injectReason),
		},
		{
			name: "plugin1 failed to make a Pod schedulable, followed by plugin2 which makes the Pod schedulable",
			plugins: []*TestPlugin{
				{
					name: "TestPlugin1",
					inj:  injectedResult{PostFilterStatus: int(framework.Error)},
				},
				{
					name: "TestPlugin2",
					inj:  injectedResult{PostFilterStatus: int(framework.Success)},
				},
			},
			wantStatus: framework.AsStatus(fmt.Errorf(injectReason)).WithPlugin("TestPlugin1"),
		},
		{
			name: "plugin1 failed to make a Pod schedulable, followed by plugin2 which makes the Pod unresolvable",
			plugins: []*TestPlugin{
				{
					name: "TestPlugin1",
					inj:  injectedResult{PostFilterStatus: int(framework.Unschedulable)},
				},
				{
					name: "TestPlugin2",
					inj:  injectedResult{PostFilterStatus: int(framework.UnschedulableAndUnresolvable)},
				},
			},
			wantStatus: framework.NewStatus(framework.UnschedulableAndUnresolvable, injectReason).WithPlugin("TestPlugin2"),
		},
		{
			name: "both plugins failed to make a Pod schedulable",
			plugins: []*TestPlugin{
				{
					name: "TestPlugin1",
					inj:  injectedResult{PostFilterStatus: int(framework.Unschedulable)},
				},
				{
					name: "TestPlugin2",
					inj:  injectedResult{PostFilterStatus: int(framework.Unschedulable)},
				},
			},
			wantStatus: framework.NewStatus(framework.Unschedulable, []string{injectReason, injectReason}...).WithPlugin("TestPlugin1"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, ctx := ktesting.NewTestContext(t)
			registry := Registry{}
			cfgPls := &config.Plugins{}
			for _, pl := range tt.plugins {
				// register all plugins
				tmpPl := pl
				if err := registry.Register(pl.name,
					func(_ context.Context, _ runtime.Object, _ framework.Handle) (framework.Plugin, error) {
						return tmpPl, nil
					}); err != nil {
					t.Fatalf("fail to register postFilter plugin (%s)", pl.name)
				}
				// append plugins to filter pluginset
				cfgPls.PostFilter.Enabled = append(
					cfgPls.PostFilter.Enabled,
					config.Plugin{Name: pl.name},
				)
			}
			profile := config.KubeSchedulerProfile{Plugins: cfgPls}
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			f, err := newFrameworkWithQueueSortAndBind(ctx, registry, profile)
			if err != nil {
				t.Fatalf("fail to create framework: %s", err)
			}
			defer func() {
				_ = f.Close()
			}()
			_, gotStatus := f.RunPostFilterPlugins(ctx, nil, pod, nil)

			if diff := cmp.Diff(tt.wantStatus, gotStatus, statusCmpOpts...); diff != "" {
				t.Errorf("Unexpected status (-want,+got):\n%s", diff)
			}
		})
	}
}

func TestFilterPluginsWithNominatedPods(t *testing.T) {
	tests := []struct {
		name            string
		preFilterPlugin *TestPlugin
		filterPlugin    *TestPlugin
		pod             *v1.Pod
		nominatedPod    *v1.Pod
		node            *v1.Node
		nodeInfo        *framework.NodeInfo
		wantStatus      *framework.Status
	}{
		{
			name:            "node has no nominated pod",
			preFilterPlugin: nil,
			filterPlugin:    nil,
			pod:             lowPriorityPod,
			nominatedPod:    nil,
			node:            node,
			nodeInfo:        framework.NewNodeInfo(pod),
			wantStatus:      nil,
		},
		{
			name: "node has a high-priority nominated pod and all filters succeed",
			preFilterPlugin: &TestPlugin{
				name: "TestPlugin1",
				inj: injectedResult{
					PreFilterAddPodStatus: int(framework.Success),
				},
			},
			filterPlugin: &TestPlugin{
				name: "TestPlugin2",
				inj: injectedResult{
					FilterStatus: int(framework.Success),
				},
			},
			pod:          lowPriorityPod,
			nominatedPod: highPriorityPod,
			node:         node,
			nodeInfo:     framework.NewNodeInfo(pod),
			wantStatus:   nil,
		},
		{
			name: "node has a high-priority nominated pod and pre filters fail",
			preFilterPlugin: &TestPlugin{
				name: "TestPlugin1",
				inj: injectedResult{
					PreFilterAddPodStatus: int(framework.Error),
				},
			},
			filterPlugin: nil,
			pod:          lowPriorityPod,
			nominatedPod: highPriorityPod,
			node:         node,
			nodeInfo:     framework.NewNodeInfo(pod),
			wantStatus:   framework.AsStatus(fmt.Errorf(`running AddPod on PreFilter plugin "TestPlugin1": %w`, errInjectedStatus)),
		},
		{
			name: "node has a high-priority nominated pod and filters fail",
			preFilterPlugin: &TestPlugin{
				name: "TestPlugin1",
				inj: injectedResult{
					PreFilterAddPodStatus: int(framework.Success),
				},
			},
			filterPlugin: &TestPlugin{
				name: "TestPlugin2",
				inj: injectedResult{
					FilterStatus: int(framework.Error),
				},
			},
			pod:          lowPriorityPod,
			nominatedPod: highPriorityPod,
			node:         node,
			nodeInfo:     framework.NewNodeInfo(pod),
			wantStatus:   framework.AsStatus(fmt.Errorf(`running "TestPlugin2" filter plugin: %w`, errInjectedFilterStatus)).WithPlugin("TestPlugin2"),
		},
		{
			name: "node has a low-priority nominated pod and pre filters return unschedulable",
			preFilterPlugin: &TestPlugin{
				name: "TestPlugin1",
				inj: injectedResult{
					PreFilterAddPodStatus: int(framework.Unschedulable),
				},
			},
			filterPlugin: &TestPlugin{
				name: "TestPlugin2",
				inj: injectedResult{
					FilterStatus: int(framework.Success),
				},
			},
			pod:          highPriorityPod,
			nominatedPod: lowPriorityPod,
			node:         node,
			nodeInfo:     framework.NewNodeInfo(pod),
			wantStatus:   nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logger, ctx := ktesting.NewTestContext(t)
			registry := Registry{}
			cfgPls := &config.Plugins{}

			if tt.preFilterPlugin != nil {
				if err := registry.Register(tt.preFilterPlugin.name,
					func(_ context.Context, _ runtime.Object, _ framework.Handle) (framework.Plugin, error) {
						return tt.preFilterPlugin, nil
					}); err != nil {
					t.Fatalf("fail to register preFilter plugin (%s)", tt.preFilterPlugin.name)
				}
				cfgPls.PreFilter.Enabled = append(
					cfgPls.PreFilter.Enabled,
					config.Plugin{Name: tt.preFilterPlugin.name},
				)
			}
			if tt.filterPlugin != nil {
				if err := registry.Register(tt.filterPlugin.name,
					func(_ context.Context, _ runtime.Object, _ framework.Handle) (framework.Plugin, error) {
						return tt.filterPlugin, nil
					}); err != nil {
					t.Fatalf("fail to register filter plugin (%s)", tt.filterPlugin.name)
				}
				cfgPls.Filter.Enabled = append(
					cfgPls.Filter.Enabled,
					config.Plugin{Name: tt.filterPlugin.name},
				)
			}

			informerFactory := informers.NewSharedInformerFactory(clientsetfake.NewClientset(), 0)
			podInformer := informerFactory.Core().V1().Pods().Informer()
			err := podInformer.GetStore().Add(tt.pod)
			if err != nil {
				t.Fatalf("Error adding pod to podInformer: %s", err)
			}
			if tt.nominatedPod != nil {
				err = podInformer.GetStore().Add(tt.nominatedPod)
				if err != nil {
					t.Fatalf("Error adding nominated pod to podInformer: %s", err)
				}
			}

			podNominator := internalqueue.NewSchedulingQueue(nil, informerFactory)
			if tt.nominatedPod != nil {
				podNominator.AddNominatedPod(
					logger,
					mustNewPodInfo(t, tt.nominatedPod),
					&framework.NominatingInfo{NominatingMode: framework.ModeOverride, NominatedNodeName: nodeName})
			}
			profile := config.KubeSchedulerProfile{Plugins: cfgPls}
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			f, err := newFrameworkWithQueueSortAndBind(ctx, registry, profile, WithPodNominator(podNominator))
			if err != nil {
				t.Fatalf("fail to create framework: %s", err)
			}
			defer func() {
				_ = f.Close()
			}()
			tt.nodeInfo.SetNode(tt.node)
			gotStatus := f.RunFilterPluginsWithNominatedPods(ctx, framework.NewCycleState(), tt.pod, tt.nodeInfo)
			if diff := cmp.Diff(tt.wantStatus, gotStatus, statusCmpOpts...); diff != "" {
				t.Errorf("Unexpected status: (-want,+got):\n%s", diff)
			}
		})
	}
}

func TestPreBindPlugins(t *testing.T) {
	tests := []struct {
		name       string
		plugins    []*TestPlugin
		wantStatus *framework.Status
	}{
		{
			name:       "NoPreBindPlugin",
			plugins:    []*TestPlugin{},
			wantStatus: nil,
		},
		{
			name: "SuccessPreBindPlugins",
			plugins: []*TestPlugin{
				{
					name: "TestPlugin",
					inj:  injectedResult{PreBindStatus: int(framework.Success)},
				},
			},
			wantStatus: nil,
		},
		{
			name: "UnschedulablePreBindPlugin",
			plugins: []*TestPlugin{
				{
					name: "TestPlugin",
					inj:  injectedResult{PreBindStatus: int(framework.Unschedulable)},
				},
			},
			wantStatus: framework.NewStatus(framework.Unschedulable, injectReason).WithPlugin("TestPlugin"),
		},
		{
			name: "ErrorPreBindPlugin",
			plugins: []*TestPlugin{
				{
					name: "TestPlugin",
					inj:  injectedResult{PreBindStatus: int(framework.Error)},
				},
			},
			wantStatus: framework.AsStatus(fmt.Errorf(`running PreBind plugin "TestPlugin": %w`, errInjectedStatus)),
		},
		{
			name: "UnschedulablePreBindPlugin",
			plugins: []*TestPlugin{
				{
					name: "TestPlugin",
					inj:  injectedResult{PreBindStatus: int(framework.UnschedulableAndUnresolvable)},
				},
			},
			wantStatus: framework.NewStatus(framework.UnschedulableAndUnresolvable, injectReason).WithPlugin("TestPlugin"),
		},
		{
			name: "SuccessErrorPreBindPlugins",
			plugins: []*TestPlugin{
				{
					name: "TestPlugin",
					inj:  injectedResult{PreBindStatus: int(framework.Success)},
				},
				{
					name: "TestPlugin 1",
					inj:  injectedResult{PreBindStatus: int(framework.Error)},
				},
			},
			wantStatus: framework.AsStatus(fmt.Errorf(`running PreBind plugin "TestPlugin 1": %w`, errInjectedStatus)),
		},
		{
			name: "ErrorSuccessPreBindPlugin",
			plugins: []*TestPlugin{
				{
					name: "TestPlugin",
					inj:  injectedResult{PreBindStatus: int(framework.Error)},
				},
				{
					name: "TestPlugin 1",
					inj:  injectedResult{PreBindStatus: int(framework.Success)},
				},
			},
			wantStatus: framework.AsStatus(fmt.Errorf(`running PreBind plugin "TestPlugin": %w`, errInjectedStatus)),
		},
		{
			name: "SuccessSuccessPreBindPlugin",
			plugins: []*TestPlugin{
				{
					name: "TestPlugin",
					inj:  injectedResult{PreBindStatus: int(framework.Success)},
				},
				{
					name: "TestPlugin 1",
					inj:  injectedResult{PreBindStatus: int(framework.Success)},
				},
			},
			wantStatus: nil,
		},
		{
			name: "ErrorAndErrorPlugins",
			plugins: []*TestPlugin{
				{
					name: "TestPlugin",
					inj:  injectedResult{PreBindStatus: int(framework.Error)},
				},
				{
					name: "TestPlugin 1",
					inj:  injectedResult{PreBindStatus: int(framework.Error)},
				},
			},
			wantStatus: framework.AsStatus(fmt.Errorf(`running PreBind plugin "TestPlugin": %w`, errInjectedStatus)),
		},
		{
			name: "UnschedulableAndSuccessPreBindPlugin",
			plugins: []*TestPlugin{
				{
					name: "TestPlugin",
					inj:  injectedResult{PreBindStatus: int(framework.Unschedulable)},
				},
				{
					name: "TestPlugin 1",
					inj:  injectedResult{PreBindStatus: int(framework.Success)},
				},
			},
			wantStatus: framework.NewStatus(framework.Unschedulable, injectReason).WithPlugin("TestPlugin"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, ctx := ktesting.NewTestContext(t)
			registry := Registry{}
			configPlugins := &config.Plugins{}

			for _, pl := range tt.plugins {
				tmpPl := pl
				if err := registry.Register(pl.name, func(_ context.Context, _ runtime.Object, _ framework.Handle) (framework.Plugin, error) {
					return tmpPl, nil
				}); err != nil {
					t.Fatalf("Unable to register pre bind plugins: %s", pl.name)
				}

				configPlugins.PreBind.Enabled = append(
					configPlugins.PreBind.Enabled,
					config.Plugin{Name: pl.name},
				)
			}
			profile := config.KubeSchedulerProfile{Plugins: configPlugins}
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			f, err := newFrameworkWithQueueSortAndBind(ctx, registry, profile)
			if err != nil {
				t.Fatalf("fail to create framework: %s", err)
			}
			defer func() {
				_ = f.Close()
			}()

			status := f.RunPreBindPlugins(ctx, nil, pod, "")

			if diff := cmp.Diff(tt.wantStatus, status, statusCmpOpts...); diff != "" {
				t.Errorf("Wrong status code (-want,+got):\n%s", diff)
			}
		})
	}
}

func TestReservePlugins(t *testing.T) {
	tests := []struct {
		name       string
		plugins    []*TestPlugin
		wantStatus *framework.Status
	}{
		{
			name:       "NoReservePlugin",
			plugins:    []*TestPlugin{},
			wantStatus: nil,
		},
		{
			name: "SuccessReservePlugins",
			plugins: []*TestPlugin{
				{
					name: "TestPlugin",
					inj:  injectedResult{ReserveStatus: int(framework.Success)},
				},
			},
			wantStatus: nil,
		},
		{
			name: "UnschedulableReservePlugin",
			plugins: []*TestPlugin{
				{
					name: "TestPlugin",
					inj:  injectedResult{ReserveStatus: int(framework.Unschedulable)},
				},
			},
			wantStatus: framework.NewStatus(framework.Unschedulable, injectReason).WithPlugin("TestPlugin"),
		},
		{
			name: "ErrorReservePlugin",
			plugins: []*TestPlugin{
				{
					name: "TestPlugin",
					inj:  injectedResult{ReserveStatus: int(framework.Error)},
				},
			},
			wantStatus: framework.AsStatus(fmt.Errorf(`running Reserve plugin "TestPlugin": %w`, errInjectedStatus)),
		},
		{
			name: "UnschedulableReservePlugin",
			plugins: []*TestPlugin{
				{
					name: "TestPlugin",
					inj:  injectedResult{ReserveStatus: int(framework.UnschedulableAndUnresolvable)},
				},
			},
			wantStatus: framework.NewStatus(framework.UnschedulableAndUnresolvable, injectReason).WithPlugin("TestPlugin"),
		},
		{
			name: "SuccessSuccessReservePlugins",
			plugins: []*TestPlugin{
				{
					name: "TestPlugin",
					inj:  injectedResult{ReserveStatus: int(framework.Success)},
				},
				{
					name: "TestPlugin 1",
					inj:  injectedResult{ReserveStatus: int(framework.Success)},
				},
			},
			wantStatus: nil,
		},
		{
			name: "ErrorErrorReservePlugins",
			plugins: []*TestPlugin{
				{
					name: "TestPlugin",
					inj:  injectedResult{ReserveStatus: int(framework.Error)},
				},
				{
					name: "TestPlugin 1",
					inj:  injectedResult{ReserveStatus: int(framework.Error)},
				},
			},
			wantStatus: framework.AsStatus(fmt.Errorf(`running Reserve plugin "TestPlugin": %w`, errInjectedStatus)),
		},
		{
			name: "SuccessErrorReservePlugins",
			plugins: []*TestPlugin{
				{
					name: "TestPlugin",
					inj:  injectedResult{ReserveStatus: int(framework.Success)},
				},
				{
					name: "TestPlugin 1",
					inj:  injectedResult{ReserveStatus: int(framework.Error)},
				},
			},
			wantStatus: framework.AsStatus(fmt.Errorf(`running Reserve plugin "TestPlugin 1": %w`, errInjectedStatus)),
		},
		{
			name: "ErrorSuccessReservePlugin",
			plugins: []*TestPlugin{
				{
					name: "TestPlugin",
					inj:  injectedResult{ReserveStatus: int(framework.Error)},
				},
				{
					name: "TestPlugin 1",
					inj:  injectedResult{ReserveStatus: int(framework.Success)},
				},
			},
			wantStatus: framework.AsStatus(fmt.Errorf(`running Reserve plugin "TestPlugin": %w`, errInjectedStatus)),
		},
		{
			name: "UnschedulableAndSuccessReservePlugin",
			plugins: []*TestPlugin{
				{
					name: "TestPlugin",
					inj:  injectedResult{ReserveStatus: int(framework.Unschedulable)},
				},
				{
					name: "TestPlugin 1",
					inj:  injectedResult{ReserveStatus: int(framework.Success)},
				},
			},
			wantStatus: framework.NewStatus(framework.Unschedulable, injectReason).WithPlugin("TestPlugin"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, ctx := ktesting.NewTestContext(t)
			registry := Registry{}
			configPlugins := &config.Plugins{}

			for _, pl := range tt.plugins {
				tmpPl := pl
				if err := registry.Register(pl.name, func(_ context.Context, _ runtime.Object, _ framework.Handle) (framework.Plugin, error) {
					return tmpPl, nil
				}); err != nil {
					t.Fatalf("Unable to register pre bind plugins: %s", pl.name)
				}

				configPlugins.Reserve.Enabled = append(
					configPlugins.Reserve.Enabled,
					config.Plugin{Name: pl.name},
				)
			}
			profile := config.KubeSchedulerProfile{Plugins: configPlugins}
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			f, err := newFrameworkWithQueueSortAndBind(ctx, registry, profile)
			defer func() {
				_ = f.Close()
			}()
			if err != nil {
				t.Fatalf("fail to create framework: %s", err)
			}

			status := f.RunReservePluginsReserve(ctx, nil, pod, "")

			if diff := cmp.Diff(tt.wantStatus, status, statusCmpOpts...); diff != "" {
				t.Errorf("Wrong status code (-want,+got):\n%s", diff)
			}
		})
	}
}

func TestPermitPlugins(t *testing.T) {
	tests := []struct {
		name    string
		plugins []*TestPlugin
		want    *framework.Status
	}{
		{
			name:    "NilPermitPlugin",
			plugins: []*TestPlugin{},
			want:    nil,
		},
		{
			name: "SuccessPermitPlugin",
			plugins: []*TestPlugin{
				{
					name: "TestPlugin",
					inj:  injectedResult{PermitStatus: int(framework.Success)},
				},
			},
			want: nil,
		},
		{
			name: "UnschedulablePermitPlugin",
			plugins: []*TestPlugin{
				{
					name: "TestPlugin",
					inj:  injectedResult{PermitStatus: int(framework.Unschedulable)},
				},
			},
			want: framework.NewStatus(framework.Unschedulable, injectReason).WithPlugin("TestPlugin"),
		},
		{
			name: "ErrorPermitPlugin",
			plugins: []*TestPlugin{
				{
					name: "TestPlugin",
					inj:  injectedResult{PermitStatus: int(framework.Error)},
				},
			},
			want: framework.AsStatus(fmt.Errorf(`running Permit plugin "TestPlugin": %w`, errInjectedStatus)).WithPlugin("TestPlugin"),
		},
		{
			name: "UnschedulableAndUnresolvablePermitPlugin",
			plugins: []*TestPlugin{
				{
					name: "TestPlugin",
					inj:  injectedResult{PermitStatus: int(framework.UnschedulableAndUnresolvable)},
				},
			},
			want: framework.NewStatus(framework.UnschedulableAndUnresolvable, injectReason).WithPlugin("TestPlugin"),
		},
		{
			name: "WaitPermitPlugin",
			plugins: []*TestPlugin{
				{
					name: "TestPlugin",
					inj:  injectedResult{PermitStatus: int(framework.Wait)},
				},
			},
			want: framework.NewStatus(framework.Wait, `one or more plugins asked to wait and no plugin rejected pod ""`),
		},
		{
			name: "SuccessSuccessPermitPlugin",
			plugins: []*TestPlugin{
				{
					name: "TestPlugin",
					inj:  injectedResult{PermitStatus: int(framework.Success)},
				},
				{
					name: "TestPlugin 1",
					inj:  injectedResult{PermitStatus: int(framework.Success)},
				},
			},
			want: nil,
		},
		{
			name: "ErrorAndErrorPlugins",
			plugins: []*TestPlugin{
				{
					name: "TestPlugin",
					inj:  injectedResult{PermitStatus: int(framework.Error)},
				},
				{
					name: "TestPlugin 1",
					inj:  injectedResult{PermitStatus: int(framework.Error)},
				},
			},
			want: framework.AsStatus(fmt.Errorf(`running Permit plugin "TestPlugin": %w`, errInjectedStatus)).WithPlugin("TestPlugin"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, ctx := ktesting.NewTestContext(t)
			registry := Registry{}
			configPlugins := &config.Plugins{}

			for _, pl := range tt.plugins {
				tmpPl := pl
				if err := registry.Register(pl.name, func(_ context.Context, _ runtime.Object, _ framework.Handle) (framework.Plugin, error) {
					return tmpPl, nil
				}); err != nil {
					t.Fatalf("Unable to register Permit plugin: %s", pl.name)
				}

				configPlugins.Permit.Enabled = append(
					configPlugins.Permit.Enabled,
					config.Plugin{Name: pl.name},
				)
			}
			profile := config.KubeSchedulerProfile{Plugins: configPlugins}
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			f, err := newFrameworkWithQueueSortAndBind(ctx, registry, profile,
				WithWaitingPods(NewWaitingPodsMap()),
			)
			defer func() {
				_ = f.Close()
			}()
			if err != nil {
				t.Fatalf("fail to create framework: %s", err)
			}

			status := f.RunPermitPlugins(ctx, nil, pod, "")
			if diff := cmp.Diff(tt.want, status, statusCmpOpts...); diff != "" {
				t.Errorf("Wrong status code (-want,+got):\n%s", diff)
			}
		})
	}
}

// withMetricsRecorder set metricsRecorder for the scheduling frameworkImpl.
func withMetricsRecorder(recorder *metrics.MetricAsyncRecorder) Option {
	return func(o *frameworkOptions) {
		o.metricsRecorder = recorder
	}
}

func TestRecordingMetrics(t *testing.T) {
	state := &framework.CycleState{}
	state.SetRecordPluginMetrics(true)
	metrics.Register()
	tests := []struct {
		name               string
		action             func(ctx context.Context, f framework.Framework)
		inject             injectedResult
		wantExtensionPoint string
		wantStatus         framework.Code
	}{
		{
			name:               "PreFilter - Success",
			action:             func(ctx context.Context, f framework.Framework) { f.RunPreFilterPlugins(ctx, state, pod) },
			wantExtensionPoint: "PreFilter",
			wantStatus:         framework.Success,
		},
		{
			name:               "PreScore - Success",
			action:             func(ctx context.Context, f framework.Framework) { f.RunPreScorePlugins(ctx, state, pod, nil) },
			wantExtensionPoint: "PreScore",
			wantStatus:         framework.Success,
		},
		{
			name: "Score - Success",
			action: func(ctx context.Context, f framework.Framework) {
				f.RunScorePlugins(ctx, state, pod, BuildNodeInfos(nodes))
			},
			wantExtensionPoint: "Score",
			wantStatus:         framework.Success,
		},
		{
			name:               "Reserve - Success",
			action:             func(ctx context.Context, f framework.Framework) { f.RunReservePluginsReserve(ctx, state, pod, "") },
			wantExtensionPoint: "Reserve",
			wantStatus:         framework.Success,
		},
		{
			name:               "Unreserve - Success",
			action:             func(ctx context.Context, f framework.Framework) { f.RunReservePluginsUnreserve(ctx, state, pod, "") },
			wantExtensionPoint: "Unreserve",
			wantStatus:         framework.Success,
		},
		{
			name:               "PreBind - Success",
			action:             func(ctx context.Context, f framework.Framework) { f.RunPreBindPlugins(ctx, state, pod, "") },
			wantExtensionPoint: "PreBind",
			wantStatus:         framework.Success,
		},
		{
			name:               "Bind - Success",
			action:             func(ctx context.Context, f framework.Framework) { f.RunBindPlugins(ctx, state, pod, "") },
			wantExtensionPoint: "Bind",
			wantStatus:         framework.Success,
		},
		{
			name:               "PostBind - Success",
			action:             func(ctx context.Context, f framework.Framework) { f.RunPostBindPlugins(ctx, state, pod, "") },
			wantExtensionPoint: "PostBind",
			wantStatus:         framework.Success,
		},
		{
			name:               "Permit - Success",
			action:             func(ctx context.Context, f framework.Framework) { f.RunPermitPlugins(ctx, state, pod, "") },
			wantExtensionPoint: "Permit",
			wantStatus:         framework.Success,
		},

		{
			name:               "PreFilter - Error",
			action:             func(ctx context.Context, f framework.Framework) { f.RunPreFilterPlugins(ctx, state, pod) },
			inject:             injectedResult{PreFilterStatus: int(framework.Error)},
			wantExtensionPoint: "PreFilter",
			wantStatus:         framework.Error,
		},
		{
			name:               "PreScore - Error",
			action:             func(ctx context.Context, f framework.Framework) { f.RunPreScorePlugins(ctx, state, pod, nil) },
			inject:             injectedResult{PreScoreStatus: int(framework.Error)},
			wantExtensionPoint: "PreScore",
			wantStatus:         framework.Error,
		},
		{
			name: "Score - Error",
			action: func(ctx context.Context, f framework.Framework) {
				f.RunScorePlugins(ctx, state, pod, BuildNodeInfos(nodes))
			},
			inject:             injectedResult{ScoreStatus: int(framework.Error)},
			wantExtensionPoint: "Score",
			wantStatus:         framework.Error,
		},
		{
			name:               "Reserve - Error",
			action:             func(ctx context.Context, f framework.Framework) { f.RunReservePluginsReserve(ctx, state, pod, "") },
			inject:             injectedResult{ReserveStatus: int(framework.Error)},
			wantExtensionPoint: "Reserve",
			wantStatus:         framework.Error,
		},
		{
			name:               "PreBind - Error",
			action:             func(ctx context.Context, f framework.Framework) { f.RunPreBindPlugins(ctx, state, pod, "") },
			inject:             injectedResult{PreBindStatus: int(framework.Error)},
			wantExtensionPoint: "PreBind",
			wantStatus:         framework.Error,
		},
		{
			name:               "Bind - Error",
			action:             func(ctx context.Context, f framework.Framework) { f.RunBindPlugins(ctx, state, pod, "") },
			inject:             injectedResult{BindStatus: int(framework.Error)},
			wantExtensionPoint: "Bind",
			wantStatus:         framework.Error,
		},
		{
			name:               "Permit - Error",
			action:             func(ctx context.Context, f framework.Framework) { f.RunPermitPlugins(ctx, state, pod, "") },
			inject:             injectedResult{PermitStatus: int(framework.Error)},
			wantExtensionPoint: "Permit",
			wantStatus:         framework.Error,
		},
		{
			name:               "Permit - Wait",
			action:             func(ctx context.Context, f framework.Framework) { f.RunPermitPlugins(ctx, state, pod, "") },
			inject:             injectedResult{PermitStatus: int(framework.Wait)},
			wantExtensionPoint: "Permit",
			wantStatus:         framework.Wait,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, ctx := ktesting.NewTestContext(t)
			ctx, cancel := context.WithCancel(ctx)
			metrics.FrameworkExtensionPointDuration.Reset()
			metrics.PluginExecutionDuration.Reset()

			plugin := &TestPlugin{name: testPlugin, inj: tt.inject}
			r := make(Registry)
			r.Register(testPlugin,
				func(_ context.Context, _ runtime.Object, fh framework.Handle) (framework.Plugin, error) {
					return plugin, nil
				})
			pluginSet := config.PluginSet{Enabled: []config.Plugin{{Name: testPlugin, Weight: 1}}}
			plugins := &config.Plugins{
				Score:     pluginSet,
				PreFilter: pluginSet,
				Filter:    pluginSet,
				PreScore:  pluginSet,
				Reserve:   pluginSet,
				Permit:    pluginSet,
				PreBind:   pluginSet,
				Bind:      pluginSet,
				PostBind:  pluginSet,
			}

			recorder := metrics.NewMetricsAsyncRecorder(100, time.Nanosecond, ctx.Done())
			profile := config.KubeSchedulerProfile{
				PercentageOfNodesToScore: ptr.To[int32](testPercentageOfNodesToScore),
				SchedulerName:            testProfileName,
				Plugins:                  plugins,
			}
			f, err := newFrameworkWithQueueSortAndBind(ctx, r, profile,
				withMetricsRecorder(recorder),
				WithWaitingPods(NewWaitingPodsMap()),
			)
			if err != nil {
				cancel()
				t.Fatalf("Failed to create framework for testing: %v", err)
			}
			defer func() {
				_ = f.Close()
			}()

			tt.action(ctx, f)

			// Stop the goroutine which records metrics and ensure it's stopped.
			cancel()
			<-recorder.IsStoppedCh
			// Try to clean up the metrics buffer again in case it's not empty.
			recorder.FlushMetrics()

			collectAndCompareFrameworkMetrics(t, tt.wantExtensionPoint, tt.wantStatus)
			collectAndComparePluginMetrics(t, tt.wantExtensionPoint, testPlugin, tt.wantStatus)
		})
	}
}

func TestRunBindPlugins(t *testing.T) {
	metrics.Register()
	tests := []struct {
		name       string
		injects    []framework.Code
		wantStatus framework.Code
	}{
		{
			name:       "simple success",
			injects:    []framework.Code{framework.Success},
			wantStatus: framework.Success,
		},
		{
			name:       "error on second",
			injects:    []framework.Code{framework.Skip, framework.Error, framework.Success},
			wantStatus: framework.Error,
		},
		{
			name:       "all skip",
			injects:    []framework.Code{framework.Skip, framework.Skip, framework.Skip},
			wantStatus: framework.Skip,
		},
		{
			name:       "error on third, but not reached",
			injects:    []framework.Code{framework.Skip, framework.Success, framework.Error},
			wantStatus: framework.Success,
		},
		{
			name:       "no bind plugin, returns default binder",
			injects:    []framework.Code{},
			wantStatus: framework.Success,
		},
		{
			name:       "invalid status",
			injects:    []framework.Code{framework.Unschedulable},
			wantStatus: framework.Unschedulable,
		},
		{
			name:       "simple error",
			injects:    []framework.Code{framework.Error},
			wantStatus: framework.Error,
		},
		{
			name:       "success on second, returns success",
			injects:    []framework.Code{framework.Skip, framework.Success},
			wantStatus: framework.Success,
		},
		{
			name:       "invalid status, returns error",
			injects:    []framework.Code{framework.Skip, framework.UnschedulableAndUnresolvable},
			wantStatus: framework.UnschedulableAndUnresolvable,
		},
		{
			name:       "error after success status, returns success",
			injects:    []framework.Code{framework.Success, framework.Error},
			wantStatus: framework.Success,
		},
		{
			name:       "success before invalid status, returns success",
			injects:    []framework.Code{framework.Success, framework.Error},
			wantStatus: framework.Success,
		},
		{
			name:       "success after error status, returns error",
			injects:    []framework.Code{framework.Error, framework.Success},
			wantStatus: framework.Error,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			metrics.FrameworkExtensionPointDuration.Reset()
			metrics.PluginExecutionDuration.Reset()

			pluginSet := config.PluginSet{}
			r := make(Registry)
			for i, inj := range tt.injects {
				name := fmt.Sprintf("bind-%d", i)
				plugin := &TestPlugin{name: name, inj: injectedResult{BindStatus: int(inj)}}
				r.Register(name,
					func(_ context.Context, _ runtime.Object, fh framework.Handle) (framework.Plugin, error) {
						return plugin, nil
					})
				pluginSet.Enabled = append(pluginSet.Enabled, config.Plugin{Name: name})
			}
			plugins := &config.Plugins{Bind: pluginSet}
			_, ctx := ktesting.NewTestContext(t)
			ctx, cancel := context.WithCancel(ctx)
			recorder := metrics.NewMetricsAsyncRecorder(100, time.Nanosecond, ctx.Done())
			profile := config.KubeSchedulerProfile{
				SchedulerName:            testProfileName,
				PercentageOfNodesToScore: ptr.To[int32](testPercentageOfNodesToScore),
				Plugins:                  plugins,
			}
			fwk, err := newFrameworkWithQueueSortAndBind(ctx, r, profile, withMetricsRecorder(recorder))
			if err != nil {
				cancel()
				t.Fatal(err)
			}
			defer func() {
				_ = fwk.Close()
			}()

			st := fwk.RunBindPlugins(ctx, state, pod, "")
			if st.Code() != tt.wantStatus {
				t.Errorf("got status code %s, want %s", st.Code(), tt.wantStatus)
			}

			// Stop the goroutine which records metrics and ensure it's stopped.
			cancel()
			<-recorder.IsStoppedCh
			// Try to clean up the metrics buffer again in case it's not empty.
			recorder.FlushMetrics()
			collectAndCompareFrameworkMetrics(t, "Bind", tt.wantStatus)
		})
	}
}

func TestPermitWaitDurationMetric(t *testing.T) {
	metrics.Register()
	tests := []struct {
		name    string
		inject  injectedResult
		wantRes string
	}{
		{
			name: "WaitOnPermit - No Wait",
		},
		{
			name:    "WaitOnPermit - Wait Timeout",
			inject:  injectedResult{PermitStatus: int(framework.Wait)},
			wantRes: "Unschedulable",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, ctx := ktesting.NewTestContext(t)
			metrics.PermitWaitDuration.Reset()

			plugin := &TestPlugin{name: testPlugin, inj: tt.inject}
			r := make(Registry)
			err := r.Register(testPlugin,
				func(_ context.Context, _ runtime.Object, fh framework.Handle) (framework.Plugin, error) {
					return plugin, nil
				})
			if err != nil {
				t.Fatal(err)
			}
			plugins := &config.Plugins{
				Permit: config.PluginSet{Enabled: []config.Plugin{{Name: testPlugin, Weight: 1}}},
			}
			profile := config.KubeSchedulerProfile{Plugins: plugins}
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			f, err := newFrameworkWithQueueSortAndBind(ctx, r, profile,
				WithWaitingPods(NewWaitingPodsMap()),
			)
			if err != nil {
				t.Fatalf("Failed to create framework for testing: %v", err)
			}
			defer func() {
				_ = f.Close()
			}()

			f.RunPermitPlugins(ctx, nil, pod, "")
			f.WaitOnPermit(ctx, pod)

			collectAndComparePermitWaitDuration(t, tt.wantRes)
		})
	}
}

func TestWaitOnPermit(t *testing.T) {
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "pod",
			UID:  types.UID("pod"),
		},
	}

	tests := []struct {
		name   string
		action func(f framework.Framework)
		want   *framework.Status
	}{
		{
			name: "Reject Waiting Pod",
			action: func(f framework.Framework) {
				f.GetWaitingPod(pod.UID).Reject(permitPlugin, "reject message")
			},
			want: framework.NewStatus(framework.Unschedulable, "reject message").WithPlugin(permitPlugin),
		},
		{
			name: "Allow Waiting Pod",
			action: func(f framework.Framework) {
				f.GetWaitingPod(pod.UID).Allow(permitPlugin)
			},
			want: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, ctx := ktesting.NewTestContext(t)
			testPermitPlugin := &TestPermitPlugin{}
			r := make(Registry)
			r.Register(permitPlugin,
				func(_ context.Context, _ runtime.Object, fh framework.Handle) (framework.Plugin, error) {
					return testPermitPlugin, nil
				})
			plugins := &config.Plugins{
				Permit: config.PluginSet{Enabled: []config.Plugin{{Name: permitPlugin, Weight: 1}}},
			}
			profile := config.KubeSchedulerProfile{Plugins: plugins}
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			f, err := newFrameworkWithQueueSortAndBind(ctx, r, profile,
				WithWaitingPods(NewWaitingPodsMap()),
			)
			if err != nil {
				t.Fatalf("Failed to create framework for testing: %v", err)
			}
			defer func() {
				_ = f.Close()
			}()

			runPermitPluginsStatus := f.RunPermitPlugins(ctx, nil, pod, "")
			if runPermitPluginsStatus.Code() != framework.Wait {
				t.Fatalf("Expected RunPermitPlugins to return status %v, but got %v",
					framework.Wait, runPermitPluginsStatus.Code())
			}

			go tt.action(f)

			got := f.WaitOnPermit(ctx, pod)
			if diff := cmp.Diff(tt.want, got, statusCmpOpts...); diff != "" {
				t.Errorf("Unexpected status (-want,+got):\n%s", diff)
			}
		})
	}
}

func TestListPlugins(t *testing.T) {
	tests := []struct {
		name    string
		plugins *config.Plugins
		want    *config.Plugins
	}{
		{
			name:    "Add empty plugin",
			plugins: &config.Plugins{},
			want: &config.Plugins{
				QueueSort: config.PluginSet{Enabled: []config.Plugin{{Name: queueSortPlugin}}},
				Bind:      config.PluginSet{Enabled: []config.Plugin{{Name: bindPlugin}}},
			},
		},
		{
			name: "Add multiple plugins",
			plugins: &config.Plugins{
				Score: config.PluginSet{Enabled: []config.Plugin{{Name: scorePlugin1, Weight: 3}, {Name: scoreWithNormalizePlugin1}}},
			},
			want: &config.Plugins{
				QueueSort: config.PluginSet{Enabled: []config.Plugin{{Name: queueSortPlugin}}},
				Bind:      config.PluginSet{Enabled: []config.Plugin{{Name: bindPlugin}}},
				Score:     config.PluginSet{Enabled: []config.Plugin{{Name: scorePlugin1, Weight: 3}, {Name: scoreWithNormalizePlugin1, Weight: 1}}},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			profile := config.KubeSchedulerProfile{Plugins: tt.plugins}
			_, ctx := ktesting.NewTestContext(t)
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			f, err := newFrameworkWithQueueSortAndBind(ctx, registry, profile)
			if err != nil {
				t.Fatalf("Failed to create framework for testing: %v", err)
			}
			defer func() {
				_ = f.Close()
			}()
			got := f.ListPlugins()
			if diff := cmp.Diff(tt.want, got, statusCmpOpts...); diff != "" {
				t.Errorf("Unexpected plugins (-want,+got):\n%s", diff)
			}
		})
	}
}

func TestClose(t *testing.T) {
	tests := []struct {
		name    string
		plugins *config.Plugins
		wantErr error
	}{
		{
			name: "close doesn't return error",
			plugins: &config.Plugins{
				MultiPoint: config.PluginSet{
					Enabled: []config.Plugin{
						{Name: testPlugin, Weight: 5},
					},
				},
			},
		},
		{
			name: "close returns error",
			plugins: &config.Plugins{
				MultiPoint: config.PluginSet{
					Enabled: []config.Plugin{
						{Name: testPlugin, Weight: 5},
						{Name: testCloseErrorPlugin},
					},
				},
			},
			wantErr: errClose,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, ctx := ktesting.NewTestContext(t)
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			fw, err := NewFramework(ctx, registry, &config.KubeSchedulerProfile{Plugins: tc.plugins})
			if err != nil {
				t.Fatalf("Unexpected error during calling NewFramework, got %v", err)
			}
			err = fw.Close()
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Unexpected error from Close(), got: %v, want: %v", err, tc.wantErr)
			}
		})
	}
}

func buildScoreConfigDefaultWeights(ps ...string) *config.Plugins {
	return buildScoreConfigWithWeights(defaultWeights, ps...)
}

func buildScoreConfigWithWeights(weights map[string]int32, ps ...string) *config.Plugins {
	var plugins []config.Plugin
	for _, p := range ps {
		plugins = append(plugins, config.Plugin{Name: p, Weight: weights[p]})
	}
	return &config.Plugins{Score: config.PluginSet{Enabled: plugins}}
}

type injectedResult struct {
	ScoreRes                 int64                      `json:"scoreRes,omitempty"`
	NormalizeRes             int64                      `json:"normalizeRes,omitempty"`
	ScoreStatus              int                        `json:"scoreStatus,omitempty"`
	NormalizeStatus          int                        `json:"normalizeStatus,omitempty"`
	PreFilterResult          *framework.PreFilterResult `json:"preFilterResult,omitempty"`
	PreFilterStatus          int                        `json:"preFilterStatus,omitempty"`
	PreFilterAddPodStatus    int                        `json:"preFilterAddPodStatus,omitempty"`
	PreFilterRemovePodStatus int                        `json:"preFilterRemovePodStatus,omitempty"`
	FilterStatus             int                        `json:"filterStatus,omitempty"`
	PostFilterStatus         int                        `json:"postFilterStatus,omitempty"`
	PreScoreStatus           int                        `json:"preScoreStatus,omitempty"`
	ReserveStatus            int                        `json:"reserveStatus,omitempty"`
	PreBindStatus            int                        `json:"preBindStatus,omitempty"`
	BindStatus               int                        `json:"bindStatus,omitempty"`
	PermitStatus             int                        `json:"permitStatus,omitempty"`
}

func setScoreRes(inj injectedResult) (int64, *framework.Status) {
	if framework.Code(inj.ScoreStatus) != framework.Success {
		return 0, framework.NewStatus(framework.Code(inj.ScoreStatus), "injecting failure.")
	}
	return inj.ScoreRes, nil
}

func injectNormalizeRes(inj injectedResult, scores framework.NodeScoreList) *framework.Status {
	if framework.Code(inj.NormalizeStatus) != framework.Success {
		return framework.NewStatus(framework.Code(inj.NormalizeStatus), "injecting failure.")
	}
	for i := range scores {
		scores[i].Score = inj.NormalizeRes
	}
	return nil
}

func collectAndComparePluginMetrics(t *testing.T, wantExtensionPoint, wantPlugin string, wantStatus framework.Code) {
	t.Helper()
	m := metrics.PluginExecutionDuration.WithLabelValues(wantPlugin, wantExtensionPoint, wantStatus.String())

	count, err := testutil.GetHistogramMetricCount(m)
	if err != nil {
		t.Errorf("Failed to get %s sampleCount, err: %v", metrics.PluginExecutionDuration.Name, err)
	}
	if count == 0 {
		t.Error("Expect at least 1 sample")
	}
	value, err := testutil.GetHistogramMetricValue(m)
	if err != nil {
		t.Errorf("Failed to get %s value, err: %v", metrics.PluginExecutionDuration.Name, err)
	}
	checkLatency(t, value)
}

func collectAndCompareFrameworkMetrics(t *testing.T, wantExtensionPoint string, wantStatus framework.Code) {
	t.Helper()
	m := metrics.FrameworkExtensionPointDuration.WithLabelValues(wantExtensionPoint, wantStatus.String(), testProfileName)

	count, err := testutil.GetHistogramMetricCount(m)
	if err != nil {
		t.Errorf("Failed to get %s sampleCount, err: %v", metrics.FrameworkExtensionPointDuration.Name, err)
	}
	if count != 1 {
		t.Errorf("Expect 1 sample, got: %v", count)
	}
	value, err := testutil.GetHistogramMetricValue(m)
	if err != nil {
		t.Errorf("Failed to get %s value, err: %v", metrics.FrameworkExtensionPointDuration.Name, err)
	}
	checkLatency(t, value)
}

func collectAndComparePermitWaitDuration(t *testing.T, wantRes string) {
	m := metrics.PermitWaitDuration.WithLabelValues(wantRes)
	count, err := testutil.GetHistogramMetricCount(m)
	if err != nil {
		t.Errorf("Failed to get %s sampleCount, err: %v", metrics.PermitWaitDuration.Name, err)
	}
	if wantRes == "" {
		if count != 0 {
			t.Errorf("Expect 0 sample, got: %v", count)
		}
	} else {
		if count != 1 {
			t.Errorf("Expect 1 sample, got: %v", count)
		}
		value, err := testutil.GetHistogramMetricValue(m)
		if err != nil {
			t.Errorf("Failed to get %s value, err: %v", metrics.PermitWaitDuration.Name, err)
		}
		checkLatency(t, value)
	}
}

func mustNewPodInfo(t *testing.T, pod *v1.Pod) *framework.PodInfo {
	podInfo, err := framework.NewPodInfo(pod)
	if err != nil {
		t.Fatal(err)
	}
	return podInfo
}

// BuildNodeInfos build NodeInfo slice from a v1.Node slice
func BuildNodeInfos(nodes []*v1.Node) []*framework.NodeInfo {
	res := make([]*framework.NodeInfo, len(nodes))
	for i := 0; i < len(nodes); i++ {
		res[i] = framework.NewNodeInfo()
		res[i].SetNode(nodes[i])
	}
	return res
}
