/*
Copyright 2020 The Kubernetes Authors.

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

package app

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/spf13/pflag"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/version"
	"k8s.io/apiserver/pkg/util/feature"
	basecompatibility "k8s.io/component-base/compatibility"
	componentbaseconfig "k8s.io/component-base/config"
	"k8s.io/component-base/featuregate"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	configv1 "k8s.io/kube-scheduler/config/v1"
	"k8s.io/kubernetes/cmd/kube-scheduler/app/options"
	"k8s.io/kubernetes/pkg/features"
	"k8s.io/kubernetes/pkg/scheduler/apis/config"
	"k8s.io/kubernetes/pkg/scheduler/apis/config/testing/defaults"
	"k8s.io/kubernetes/pkg/scheduler/framework"
)

func TestSetup(t *testing.T) {
	// temp dir
	tmpDir, err := os.MkdirTemp("", "scheduler-options")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	// https server
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte(`ok`))
	}))
	defer server.Close()

	configKubeconfig := filepath.Join(tmpDir, "config.kubeconfig")
	if err := os.WriteFile(configKubeconfig, []byte(fmt.Sprintf(`
apiVersion: v1
kind: Config
clusters:
- cluster:
    insecure-skip-tls-verify: true
    server: %s
  name: default
contexts:
- context:
    cluster: default
    user: default
  name: default
current-context: default
users:
- name: default
  user:
    username: config
`, server.URL)), os.FileMode(0600)); err != nil {
		t.Fatal(err)
	}

	// plugin config
	pluginConfigFilev1 := filepath.Join(tmpDir, "pluginv1.yaml")
	if err := os.WriteFile(pluginConfigFilev1, []byte(fmt.Sprintf(`
apiVersion: kubescheduler.config.k8s.io/v1
kind: KubeSchedulerConfiguration
clientConnection:
  kubeconfig: '%s'
profiles:
- plugins:
    multiPoint:
      enabled:
      - name: SchedulingGates
      - name: DefaultBinder
      - name: PrioritySort
      - name: DefaultPreemption
      - name: VolumeBinding
      - name: NodeResourcesFit
      - name: NodePorts
      - name: InterPodAffinity
      - name: TaintToleration
      disabled:
      - name: "*"
    preFilter:
      disabled:
      - name: VolumeBinding
      - name: InterPodAffinity
    filter:
      disabled:
      - name: VolumeBinding
      - name: InterPodAffinity
      - name: TaintToleration
    score:
      disabled:
      - name: VolumeBinding
      - name: NodeResourcesFit
`, configKubeconfig)), os.FileMode(0600)); err != nil {
		t.Fatal(err)
	}

	// plugin config
	simplifiedPluginConfigFilev1 := filepath.Join(tmpDir, "simplifiedPluginv1.yaml")
	if err := os.WriteFile(simplifiedPluginConfigFilev1, []byte(fmt.Sprintf(`
apiVersion: kubescheduler.config.k8s.io/v1
kind: KubeSchedulerConfiguration
clientConnection:
  kubeconfig: '%s'
profiles:
- schedulerName: simplified-scheduler
`, configKubeconfig)), os.FileMode(0600)); err != nil {
		t.Fatal(err)
	}

	// out-of-tree plugin config v1
	outOfTreePluginConfigFilev1 := filepath.Join(tmpDir, "outOfTreePluginv1.yaml")
	if err := os.WriteFile(outOfTreePluginConfigFilev1, []byte(fmt.Sprintf(`
apiVersion: kubescheduler.config.k8s.io/v1
kind: KubeSchedulerConfiguration
clientConnection:
  kubeconfig: '%s'
profiles:
- plugins:
    preFilter:
      enabled:
      - name: Foo
    filter:
      enabled:
      - name: Foo
`, configKubeconfig)), os.FileMode(0600)); err != nil {
		t.Fatal(err)
	}

	// multiple profiles config
	multiProfilesConfig := filepath.Join(tmpDir, "multi-profiles.yaml")
	if err := os.WriteFile(multiProfilesConfig, []byte(fmt.Sprintf(`
apiVersion: kubescheduler.config.k8s.io/v1
kind: KubeSchedulerConfiguration
clientConnection:
  kubeconfig: '%s'
profiles:
- schedulerName: "profile-default-plugins"
- schedulerName: "profile-disable-all-filter-and-score-plugins"
  plugins:
    preFilter:
      disabled:
      - name: "*"
    filter:
      disabled:
      - name: "*"
    postFilter:
      disabled:
      - name: "*"
    preScore:
      disabled:
      - name: "*"
    score:
      disabled:
      - name: "*"
`, configKubeconfig)), os.FileMode(0600)); err != nil {
		t.Fatal(err)
	}

	// empty leader-election config
	emptyLeaderElectionConfig := filepath.Join(tmpDir, "empty-leader-election-config.yaml")
	if err := os.WriteFile(emptyLeaderElectionConfig, []byte(fmt.Sprintf(`
apiVersion: kubescheduler.config.k8s.io/v1
kind: KubeSchedulerConfiguration
clientConnection:
  kubeconfig: '%s'
`, configKubeconfig)), os.FileMode(0600)); err != nil {
		t.Fatal(err)
	}

	// leader-election config
	leaderElectionConfig := filepath.Join(tmpDir, "leader-election-config.yaml")
	if err := os.WriteFile(leaderElectionConfig, []byte(fmt.Sprintf(`
apiVersion: kubescheduler.config.k8s.io/v1
kind: KubeSchedulerConfiguration
clientConnection:
  kubeconfig: '%s'
leaderElection:
  leaseDuration: 1h
`, configKubeconfig)), os.FileMode(0600)); err != nil {
		t.Fatal(err)
	}

	testcases := []struct {
		name                 string
		flags                []string
		registryOptions      []Option
		restoreFeatures      map[featuregate.Feature]bool
		wantPlugins          map[string]*config.Plugins
		wantLeaderElection   *componentbaseconfig.LeaderElectionConfiguration
		wantClientConnection *componentbaseconfig.ClientConnectionConfiguration
		wantErr              bool
		wantFeaturesGates    map[string]bool
	}{
		{
			name: "default config with an alpha feature enabled",
			flags: []string{
				"--kubeconfig", configKubeconfig,
				"--feature-gates=VolumeCapacityPriority=true",
			},
			wantPlugins: map[string]*config.Plugins{
				"default-scheduler": defaults.ExpandedPluginsV1,
			},
			restoreFeatures: map[featuregate.Feature]bool{
				features.VolumeCapacityPriority: false,
			},
		},
		{
			name: "component configuration v1 with only scheduler name configured",
			flags: []string{
				"--config", simplifiedPluginConfigFilev1,
				"--kubeconfig", configKubeconfig,
			},
			wantPlugins: map[string]*config.Plugins{
				"simplified-scheduler": defaults.ExpandedPluginsV1,
			},
		},
		{
			name: "default config",
			flags: []string{
				"--kubeconfig", configKubeconfig,
			},
			wantPlugins: map[string]*config.Plugins{
				"default-scheduler": defaults.ExpandedPluginsV1,
			},
		},
		{
			name: "component configuration v1",
			flags: []string{
				"--config", pluginConfigFilev1,
				"--kubeconfig", configKubeconfig,
			},
			wantPlugins: map[string]*config.Plugins{
				"default-scheduler": func() *config.Plugins {
					plugins := defaults.ExpandedPluginsV1.DeepCopy()
					plugins.Filter.Enabled = []config.Plugin{
						{Name: "NodeResourcesFit"},
						{Name: "NodePorts"},
					}
					plugins.PreFilter.Enabled = []config.Plugin{
						{Name: "NodeResourcesFit"},
						{Name: "NodePorts"},
					}
					plugins.PreScore.Enabled = []config.Plugin{
						{Name: "VolumeBinding"},
						{Name: "NodeResourcesFit"},
						{Name: "InterPodAffinity"},
						{Name: "TaintToleration"},
					}
					plugins.Score.Enabled = []config.Plugin{
						{Name: "InterPodAffinity", Weight: 1},
						{Name: "TaintToleration", Weight: 1},
					}
					return plugins
				}(),
			},
		},
		{
			name: "out-of-tree component configuration v1",
			flags: []string{
				"--config", outOfTreePluginConfigFilev1,
				"--kubeconfig", configKubeconfig,
			},
			registryOptions: []Option{WithPlugin("Foo", newFoo)},
			wantPlugins: map[string]*config.Plugins{
				"default-scheduler": func() *config.Plugins {
					plugins := defaults.ExpandedPluginsV1.DeepCopy()
					plugins.PreFilter.Enabled = append(plugins.PreFilter.Enabled, config.Plugin{Name: "Foo"})
					plugins.Filter.Enabled = append(plugins.Filter.Enabled, config.Plugin{Name: "Foo"})
					return plugins
				}(),
			},
		},
		{
			name: "leader election CLI args, along with --config arg",
			flags: []string{
				"--leader-elect=false",
				"--leader-elect-lease-duration=2h", // CLI args are favored over the fields in ComponentConfig
				"--kubeconfig=foo",                 // deprecated CLI arg will be ignored if --config is specified
				"--config", emptyLeaderElectionConfig,
			},
			wantLeaderElection: &componentbaseconfig.LeaderElectionConfiguration{
				LeaderElect:       false,                                    // from CLI args
				LeaseDuration:     metav1.Duration{Duration: 2 * time.Hour}, // from CLI args
				RenewDeadline:     metav1.Duration{Duration: 10 * time.Second},
				RetryPeriod:       metav1.Duration{Duration: 2 * time.Second},
				ResourceLock:      "leases",
				ResourceName:      configv1.SchedulerDefaultLockObjectName,
				ResourceNamespace: configv1.SchedulerDefaultLockObjectNamespace,
			},
			wantClientConnection: &componentbaseconfig.ClientConnectionConfiguration{
				Kubeconfig:  configKubeconfig,
				ContentType: "application/vnd.kubernetes.protobuf",
				QPS:         50,
				Burst:       100,
			},
		},
		{
			name: "leader election CLI args, without --config arg",
			flags: []string{
				"--leader-elect=false",
				"--leader-elect-lease-duration=2h",
				"--leader-elect-resource-namespace=default",
				"--kubeconfig", configKubeconfig, // deprecated CLI arg is honored if --config is not specified
			},
			wantLeaderElection: &componentbaseconfig.LeaderElectionConfiguration{
				LeaderElect:       false,                                    // from CLI args
				LeaseDuration:     metav1.Duration{Duration: 2 * time.Hour}, // from CLI args
				RenewDeadline:     metav1.Duration{Duration: 10 * time.Second},
				RetryPeriod:       metav1.Duration{Duration: 2 * time.Second},
				ResourceLock:      "leases",
				ResourceName:      configv1.SchedulerDefaultLockObjectName,
				ResourceNamespace: "default", // from CLI args
			},
			wantClientConnection: &componentbaseconfig.ClientConnectionConfiguration{
				Kubeconfig:  configKubeconfig, // from deprecated CLI args
				ContentType: "application/vnd.kubernetes.protobuf",
				QPS:         50,
				Burst:       100,
			},
		},
		{
			name: "leader election settings specified by ComponentConfig only",
			flags: []string{
				"--config", leaderElectionConfig,
			},
			wantLeaderElection: &componentbaseconfig.LeaderElectionConfiguration{
				LeaderElect:       true,
				LeaseDuration:     metav1.Duration{Duration: 1 * time.Hour}, // from CC
				RenewDeadline:     metav1.Duration{Duration: 10 * time.Second},
				RetryPeriod:       metav1.Duration{Duration: 2 * time.Second},
				ResourceLock:      "leases",
				ResourceName:      configv1.SchedulerDefaultLockObjectName,
				ResourceNamespace: configv1.SchedulerDefaultLockObjectNamespace,
			},
		},
		{
			name: "leader election settings specified by CLI args and ComponentConfig",
			flags: []string{
				"--leader-elect=true",
				"--leader-elect-renew-deadline=5s",
				"--leader-elect-retry-period=1s",
				"--config", leaderElectionConfig,
			},
			wantLeaderElection: &componentbaseconfig.LeaderElectionConfiguration{
				LeaderElect:       true,
				LeaseDuration:     metav1.Duration{Duration: 1 * time.Hour},   // from CC
				RenewDeadline:     metav1.Duration{Duration: 5 * time.Second}, // from CLI args
				RetryPeriod:       metav1.Duration{Duration: 1 * time.Second}, // from CLI args
				ResourceLock:      "leases",
				ResourceName:      configv1.SchedulerDefaultLockObjectName,
				ResourceNamespace: configv1.SchedulerDefaultLockObjectNamespace,
			},
		},
		{
			name: "emulated version out of range",
			flags: []string{
				"--kubeconfig", configKubeconfig,
				"--emulated-version=1.28",
			},
			wantErr: true,
		},
		{
			name: "default feature gates at binary version",
			flags: []string{
				"--kubeconfig", configKubeconfig,
			},
			wantFeaturesGates: map[string]bool{"kubeA": true, "kubeB": false},
		},
		{
			name: "default feature gates at emulated version",
			flags: []string{
				"--kubeconfig", configKubeconfig,
				"--emulated-version=1.31",
			},
			wantFeaturesGates: map[string]bool{"kubeA": false, "kubeB": false},
		},
		{
			name: "set feature gates at emulated version",
			flags: []string{
				"--kubeconfig", configKubeconfig,
				"--emulated-version=1.31",
				"--feature-gates=kubeA=false,kubeB=true",
			},
			wantFeaturesGates: map[string]bool{"kubeA": false, "kubeB": true},
		},
		{
			name: "cannot set locked feature gate",
			flags: []string{
				"--kubeconfig", configKubeconfig,
				"--feature-gates=kubeA=false,kubeB=true",
			},
			wantErr: true,
		},
	}

	makeListener := func(t *testing.T) net.Listener {
		t.Helper()
		l, err := net.Listen("tcp", ":0")
		if err != nil {
			t.Fatal(err)
		}
		return l
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range tc.restoreFeatures {
				featuregatetesting.SetFeatureGateDuringTest(t, feature.DefaultFeatureGate, k, v)
			}
			componentGlobalsRegistry := basecompatibility.NewComponentGlobalsRegistry()
			verKube := basecompatibility.NewEffectiveVersionFromString("1.32", "1.31", "1.31")
			fg := feature.DefaultFeatureGate.DeepCopy()
			utilruntime.Must(fg.AddVersioned(map[featuregate.Feature]featuregate.VersionedSpecs{
				"kubeA": {
					{Version: version.MustParse("1.30"), Default: false, PreRelease: featuregate.Beta},
					{Version: version.MustParse("1.32"), Default: true, LockToDefault: true, PreRelease: featuregate.GA},
				},
				"kubeB": {
					{Version: version.MustParse("1.31"), Default: false, PreRelease: featuregate.Alpha},
				},
			}))
			utilruntime.Must(componentGlobalsRegistry.Register(basecompatibility.DefaultKubeComponent, verKube, fg))

			fs := pflag.NewFlagSet("test", pflag.PanicOnError)
			opts := options.NewOptionsWithComponentGlobalsRegistry(componentGlobalsRegistry)

			// use listeners instead of static ports so parallel test runs don't conflict
			opts.SecureServing.Listener = makeListener(t)
			defer opts.SecureServing.Listener.Close()

			nfs := opts.Flags
			for _, f := range nfs.FlagSets {
				fs.AddFlagSet(f)
			}
			if err := fs.Parse(tc.flags); err != nil {
				t.Fatal(err)
			}

			// use listeners instead of static ports so parallel test runs don't conflict
			opts.SecureServing.Listener = makeListener(t)
			defer opts.SecureServing.Listener.Close()

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			_, sched, err := Setup(ctx, opts, tc.registryOptions...)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected Setup error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}

			if tc.wantPlugins != nil {
				gotPlugins := make(map[string]*config.Plugins)
				for n, p := range sched.Profiles {
					gotPlugins[n] = p.ListPlugins()
				}

				if diff := cmp.Diff(tc.wantPlugins, gotPlugins); diff != "" {
					t.Errorf("Unexpected plugins diff (-want, +got): %s", diff)
				}
			}

			if tc.wantLeaderElection != nil {
				gotLeaderElection := opts.ComponentConfig.LeaderElection
				if diff := cmp.Diff(*tc.wantLeaderElection, gotLeaderElection); diff != "" {
					t.Errorf("Unexpected leaderElection diff (-want, +got): %s", diff)
				}
			}

			if tc.wantClientConnection != nil {
				gotClientConnection := opts.ComponentConfig.ClientConnection
				if diff := cmp.Diff(*tc.wantClientConnection, gotClientConnection); diff != "" {
					t.Errorf("Unexpected clientConnection diff (-want, +got): %s", diff)
				}
			}
			for f, v := range tc.wantFeaturesGates {
				enabled := fg.Enabled(featuregate.Feature(f))
				if enabled != v {
					t.Errorf("expected featuregate.Enabled(%s)=%v, got %v", f, v, enabled)
				}
			}
		})
	}
}

// Simulates an out-of-tree plugin.
type foo struct{}

var _ framework.PreFilterPlugin = &foo{}
var _ framework.FilterPlugin = &foo{}

func (*foo) Name() string {
	return "Foo"
}

func newFoo(_ context.Context, _ runtime.Object, _ framework.Handle) (framework.Plugin, error) {
	return &foo{}, nil
}

func (*foo) PreFilter(_ context.Context, _ *framework.CycleState, _ *v1.Pod) (*framework.PreFilterResult, *framework.Status) {
	return nil, nil
}

func (*foo) PreFilterExtensions() framework.PreFilterExtensions {
	return nil
}

func (*foo) Filter(_ context.Context, _ *framework.CycleState, _ *v1.Pod, nodeInfo *framework.NodeInfo) *framework.Status {
	return nil
}
