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

package options

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/stretchr/testify/assert"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	apiserveroptions "k8s.io/apiserver/pkg/server/options"
	basecompatibility "k8s.io/component-base/compatibility"
	componentbaseconfig "k8s.io/component-base/config"
	"k8s.io/component-base/logs"
	"k8s.io/klog/v2/ktesting"
	v1 "k8s.io/kube-scheduler/config/v1"
	kubeschedulerconfig "k8s.io/kubernetes/pkg/scheduler/apis/config"
	"k8s.io/kubernetes/pkg/scheduler/apis/config/latest"
	configtesting "k8s.io/kubernetes/pkg/scheduler/apis/config/testing"
	"k8s.io/kubernetes/pkg/scheduler/apis/config/testing/defaults"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/names"
	"k8s.io/utils/ptr"
)

func TestSchedulerOptions(t *testing.T) {
	// temp dir
	tmpDir, err := os.MkdirTemp("", "scheduler-options")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	// record the username requests were made with
	username := ""
	// https server
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		username, _, _ = req.BasicAuth()
		if username == "" {
			username = "none, tls"
		}
		w.WriteHeader(200)
		w.Write([]byte(`ok`))
	}))
	defer server.Close()
	// http server
	insecureserver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		username, _, _ = req.BasicAuth()
		if username == "" {
			username = "none, http"
		}
		w.WriteHeader(200)
		w.Write([]byte(`ok`))
	}))
	defer insecureserver.Close()

	// config file and kubeconfig
	configFile := filepath.Join(tmpDir, "scheduler.yaml")
	configKubeconfig := filepath.Join(tmpDir, "config.kubeconfig")
	if err := os.WriteFile(configFile, []byte(fmt.Sprintf(`
apiVersion: kubescheduler.config.k8s.io/v1
kind: KubeSchedulerConfiguration
clientConnection:
  kubeconfig: '%s'
leaderElection:
  leaderElect: true`, configKubeconfig)), os.FileMode(0600)); err != nil {
		t.Fatal(err)
	}
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

	oldConfigFile := filepath.Join(tmpDir, "scheduler_old.yaml")
	if err := os.WriteFile(oldConfigFile, []byte(fmt.Sprintf(`
apiVersion: componentconfig/v1alpha1
kind: KubeSchedulerConfiguration
clientConnection:
  kubeconfig: '%s'
leaderElection:
  leaderElect: true`, configKubeconfig)), os.FileMode(0600)); err != nil {
		t.Fatal(err)
	}

	unknownVersionConfig := filepath.Join(tmpDir, "scheduler_invalid_wrong_api_version.yaml")
	if err := os.WriteFile(unknownVersionConfig, []byte(fmt.Sprintf(`
apiVersion: kubescheduler.config.k8s.io/unknown
kind: KubeSchedulerConfiguration
clientConnection:
  kubeconfig: '%s'
leaderElection:
  leaderElect: true`, configKubeconfig)), os.FileMode(0600)); err != nil {
		t.Fatal(err)
	}

	noVersionConfig := filepath.Join(tmpDir, "scheduler_invalid_no_version.yaml")
	if err := os.WriteFile(noVersionConfig, []byte(fmt.Sprintf(`
kind: KubeSchedulerConfiguration
clientConnection:
  kubeconfig: '%s'
leaderElection:
  leaderElect: true`, configKubeconfig)), os.FileMode(0600)); err != nil {
		t.Fatal(err)
	}

	unknownFieldConfig := filepath.Join(tmpDir, "scheduler_invalid_unknown_field.yaml")
	if err := os.WriteFile(unknownFieldConfig, []byte(fmt.Sprintf(`
apiVersion: kubescheduler.config.k8s.io/v1
kind: KubeSchedulerConfiguration
clientConnection:
  kubeconfig: '%s'
leaderElection:
  leaderElect: true
foo: bar`, configKubeconfig)), os.FileMode(0600)); err != nil {
		t.Fatal(err)
	}

	duplicateFieldConfig := filepath.Join(tmpDir, "scheduler_invalid_duplicate_fields.yaml")
	if err := os.WriteFile(duplicateFieldConfig, []byte(fmt.Sprintf(`
apiVersion: kubescheduler.config.k8s.io/v1
kind: KubeSchedulerConfiguration
clientConnection:
  kubeconfig: '%s'
leaderElection:
  leaderElect: true
  leaderElect: false`, configKubeconfig)), os.FileMode(0600)); err != nil {
		t.Fatal(err)
	}

	// flag-specified kubeconfig
	flagKubeconfig := filepath.Join(tmpDir, "flag.kubeconfig")
	if err := os.WriteFile(flagKubeconfig, []byte(fmt.Sprintf(`
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
    username: flag
`, server.URL)), os.FileMode(0600)); err != nil {
		t.Fatal(err)
	}

	// plugin config
	pluginConfigFile := filepath.Join(tmpDir, "plugin.yaml")
	if err := os.WriteFile(pluginConfigFile, []byte(fmt.Sprintf(`
apiVersion: kubescheduler.config.k8s.io/v1
kind: KubeSchedulerConfiguration
clientConnection:
  kubeconfig: '%s'
profiles:
- plugins:
    preEnqueue:
      enabled:
      - name: foo
    reserve:
      enabled:
      - name: foo
      - name: bar
      disabled:
      - name: VolumeBinding
    preBind:
      enabled:
      - name: foo
      disabled:
      - name: VolumeBinding
  pluginConfig:
  - name: InterPodAffinity
    args:
      hardPodAffinityWeight: 2
  - name: foo
    args:
      bar: baz
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
- schedulerName: "foo-profile"
  plugins:
    reserve:
      enabled:
      - name: foo
      - name: VolumeBinding
      disabled:
      - name: VolumeBinding
- schedulerName: "bar-profile"
  plugins:
    preBind:
      disabled:
      - name: VolumeBinding
  pluginConfig:
  - name: foo
`, configKubeconfig)), os.FileMode(0600)); err != nil {
		t.Fatal(err)
	}

	// high throughput profile config
	highThroughputProfileConfig := filepath.Join(tmpDir, "high-throughput.yaml")
	if err := os.WriteFile(highThroughputProfileConfig, []byte(fmt.Sprintf(`
apiVersion: kubescheduler.config.k8s.io/v1
kind: KubeSchedulerConfiguration
clientConnection:
  kubeconfig: '%s'
profiles:
- schedulerName: "high-throughput-profile"
  plugins:
    preScore:
      enabled:
      - name: InterPodAffinity
  pluginConfig:
  - name: InterPodAffinity
    args:
      ignorePreferredTermsOfExistingPods: true
`, configKubeconfig)), os.FileMode(0600)); err != nil {
		t.Fatal(err)
	}

	// Insulate this test from picking up in-cluster config when run inside a pod
	// We can't assume we have permissions to write to /var/run/secrets/... from a unit test to mock in-cluster config for testing
	if len(os.Getenv("KUBERNETES_SERVICE_HOST")) > 0 {
		t.Setenv("KUBERNETES_SERVICE_HOST", "")
	}

	defaultPodInitialBackoffSeconds := int64(1)
	defaultPodMaxBackoffSeconds := int64(10)
	defaultPercentageOfNodesToScore := ptr.To[int32](0)

	componentGlobalsRegistry := basecompatibility.NewComponentGlobalsRegistry()

	testcases := []struct {
		name             string
		options          *Options
		expectedUsername string
		expectedError    string
		expectedConfig   kubeschedulerconfig.KubeSchedulerConfiguration
		checkErrFn       func(err error) bool
	}{
		{
			name: "v1 config file",
			options: &Options{
				ConfigFile: configFile,
				ComponentConfig: func() *kubeschedulerconfig.KubeSchedulerConfiguration {
					cfg := configtesting.V1ToInternalWithDefaults(t, v1.KubeSchedulerConfiguration{})
					return cfg
				}(),
				SecureServing: (&apiserveroptions.SecureServingOptions{
					ServerCert: apiserveroptions.GeneratableKeyCert{
						CertDirectory: "/a/b/c",
						PairName:      "kube-scheduler",
					},
					HTTP2MaxStreamsPerConnection: 47,
				}).WithLoopback(),
				Authentication: &apiserveroptions.DelegatingAuthenticationOptions{
					CacheTTL:   10 * time.Second,
					ClientCert: apiserveroptions.ClientCertAuthenticationOptions{},
					RequestHeader: apiserveroptions.RequestHeaderAuthenticationOptions{
						UsernameHeaders:     []string{"x-remote-user"},
						GroupHeaders:        []string{"x-remote-group"},
						ExtraHeaderPrefixes: []string{"x-remote-extra-"},
					},
					RemoteKubeConfigFileOptional: true,
				},
				Authorization: &apiserveroptions.DelegatingAuthorizationOptions{
					AllowCacheTTL:                10 * time.Second,
					DenyCacheTTL:                 10 * time.Second,
					RemoteKubeConfigFileOptional: true,
					AlwaysAllowPaths:             []string{"/healthz", "/readyz", "/livez"}, // note: this does not match /healthz/ or /healthz/*
					AlwaysAllowGroups:            []string{"system:masters"},
				},
				Logs:                     logs.NewOptions(),
				ComponentGlobalsRegistry: componentGlobalsRegistry,
			},
			expectedUsername: "config",
			expectedConfig: kubeschedulerconfig.KubeSchedulerConfiguration{
				TypeMeta: metav1.TypeMeta{
					APIVersion: v1.SchemeGroupVersion.String(),
				},
				Parallelism:           16,
				DelayCacheUntilActive: false,
				DebuggingConfiguration: componentbaseconfig.DebuggingConfiguration{
					EnableProfiling:           true,
					EnableContentionProfiling: true,
				},
				LeaderElection: componentbaseconfig.LeaderElectionConfiguration{
					LeaderElect:       true,
					LeaseDuration:     metav1.Duration{Duration: 15 * time.Second},
					RenewDeadline:     metav1.Duration{Duration: 10 * time.Second},
					RetryPeriod:       metav1.Duration{Duration: 2 * time.Second},
					ResourceLock:      "leases",
					ResourceNamespace: "kube-system",
					ResourceName:      "kube-scheduler",
				},
				ClientConnection: componentbaseconfig.ClientConnectionConfiguration{
					Kubeconfig:  configKubeconfig,
					QPS:         50,
					Burst:       100,
					ContentType: "application/vnd.kubernetes.protobuf",
				},
				PercentageOfNodesToScore: defaultPercentageOfNodesToScore,
				PodInitialBackoffSeconds: defaultPodInitialBackoffSeconds,
				PodMaxBackoffSeconds:     defaultPodMaxBackoffSeconds,
				Profiles: []kubeschedulerconfig.KubeSchedulerProfile{
					{
						SchedulerName: "default-scheduler",
						Plugins:       defaults.PluginsV1,
						PluginConfig:  defaults.PluginConfigsV1,
					},
				},
			},
		},
		{
			name: "config file in componentconfig/v1alpha1",
			options: &Options{
				ConfigFile: oldConfigFile,
				ComponentConfig: func() *kubeschedulerconfig.KubeSchedulerConfiguration {
					cfg, err := latest.Default()
					if err != nil {
						t.Fatal(err)
					}
					return cfg
				}(),
				Logs:                     logs.NewOptions(),
				ComponentGlobalsRegistry: componentGlobalsRegistry,
			},
			expectedError: "no kind \"KubeSchedulerConfiguration\" is registered for version \"componentconfig/v1alpha1\"",
		},
		{
			name: "unknown version kubescheduler.config.k8s.io/unknown",
			options: &Options{
				ConfigFile:               unknownVersionConfig,
				Logs:                     logs.NewOptions(),
				ComponentGlobalsRegistry: componentGlobalsRegistry,
			},
			expectedError: "no kind \"KubeSchedulerConfiguration\" is registered for version \"kubescheduler.config.k8s.io/unknown\"",
		},
		{
			name: "config file with no version",
			options: &Options{
				ConfigFile:               noVersionConfig,
				Logs:                     logs.NewOptions(),
				ComponentGlobalsRegistry: componentGlobalsRegistry,
			},
			expectedError: "Object 'apiVersion' is missing",
		},
		{
			name: "kubeconfig flag",
			options: &Options{
				ComponentConfig: func() *kubeschedulerconfig.KubeSchedulerConfiguration {
					cfg, _ := latest.Default()
					cfg.ClientConnection.Kubeconfig = flagKubeconfig
					return cfg
				}(),
				SecureServing: (&apiserveroptions.SecureServingOptions{
					ServerCert: apiserveroptions.GeneratableKeyCert{
						CertDirectory: "/a/b/c",
						PairName:      "kube-scheduler",
					},
					HTTP2MaxStreamsPerConnection: 47,
				}).WithLoopback(),
				Authentication: &apiserveroptions.DelegatingAuthenticationOptions{
					CacheTTL:   10 * time.Second,
					ClientCert: apiserveroptions.ClientCertAuthenticationOptions{},
					RequestHeader: apiserveroptions.RequestHeaderAuthenticationOptions{
						UsernameHeaders:     []string{"x-remote-user"},
						GroupHeaders:        []string{"x-remote-group"},
						ExtraHeaderPrefixes: []string{"x-remote-extra-"},
					},
					RemoteKubeConfigFileOptional: true,
				},
				Authorization: &apiserveroptions.DelegatingAuthorizationOptions{
					AllowCacheTTL:                10 * time.Second,
					DenyCacheTTL:                 10 * time.Second,
					RemoteKubeConfigFileOptional: true,
					AlwaysAllowPaths:             []string{"/healthz", "/readyz", "/livez"}, // note: this does not match /healthz/ or /healthz/*
					AlwaysAllowGroups:            []string{"system:masters"},
				},
				Logs:                     logs.NewOptions(),
				ComponentGlobalsRegistry: componentGlobalsRegistry,
			},
			expectedUsername: "flag",
			expectedConfig: kubeschedulerconfig.KubeSchedulerConfiguration{
				TypeMeta: metav1.TypeMeta{
					APIVersion: v1.SchemeGroupVersion.String(),
				},
				Parallelism:           16,
				DelayCacheUntilActive: false,
				DebuggingConfiguration: componentbaseconfig.DebuggingConfiguration{
					EnableProfiling:           true,
					EnableContentionProfiling: true,
				},
				LeaderElection: componentbaseconfig.LeaderElectionConfiguration{
					LeaderElect:       true,
					LeaseDuration:     metav1.Duration{Duration: 15 * time.Second},
					RenewDeadline:     metav1.Duration{Duration: 10 * time.Second},
					RetryPeriod:       metav1.Duration{Duration: 2 * time.Second},
					ResourceLock:      "leases",
					ResourceNamespace: "kube-system",
					ResourceName:      "kube-scheduler",
				},
				ClientConnection: componentbaseconfig.ClientConnectionConfiguration{
					Kubeconfig:  flagKubeconfig,
					QPS:         50,
					Burst:       100,
					ContentType: "application/vnd.kubernetes.protobuf",
				},
				PercentageOfNodesToScore: defaultPercentageOfNodesToScore,
				PodInitialBackoffSeconds: defaultPodInitialBackoffSeconds,
				PodMaxBackoffSeconds:     defaultPodMaxBackoffSeconds,
				Profiles: []kubeschedulerconfig.KubeSchedulerProfile{
					{
						SchedulerName: "default-scheduler",
						Plugins:       defaults.PluginsV1,
						PluginConfig:  defaults.PluginConfigsV1,
					},
				},
			},
		},
		{
			name: "overridden master",
			options: &Options{
				ComponentConfig: func() *kubeschedulerconfig.KubeSchedulerConfiguration {
					cfg, _ := latest.Default()
					cfg.ClientConnection.Kubeconfig = flagKubeconfig
					return cfg
				}(),
				Master: insecureserver.URL,
				SecureServing: (&apiserveroptions.SecureServingOptions{
					ServerCert: apiserveroptions.GeneratableKeyCert{
						CertDirectory: "/a/b/c",
						PairName:      "kube-scheduler",
					},
					HTTP2MaxStreamsPerConnection: 47,
				}).WithLoopback(),
				Authentication: &apiserveroptions.DelegatingAuthenticationOptions{
					CacheTTL: 10 * time.Second,
					RequestHeader: apiserveroptions.RequestHeaderAuthenticationOptions{
						UsernameHeaders:     []string{"x-remote-user"},
						GroupHeaders:        []string{"x-remote-group"},
						ExtraHeaderPrefixes: []string{"x-remote-extra-"},
					},
					RemoteKubeConfigFileOptional: true,
				},
				Authorization: &apiserveroptions.DelegatingAuthorizationOptions{
					AllowCacheTTL:                10 * time.Second,
					DenyCacheTTL:                 10 * time.Second,
					RemoteKubeConfigFileOptional: true,
					AlwaysAllowPaths:             []string{"/healthz", "/readyz", "/livez"}, // note: this does not match /healthz/ or /healthz/*
					AlwaysAllowGroups:            []string{"system:masters"},
				},
				Logs:                     logs.NewOptions(),
				ComponentGlobalsRegistry: componentGlobalsRegistry,
			},
			expectedConfig: kubeschedulerconfig.KubeSchedulerConfiguration{
				TypeMeta: metav1.TypeMeta{
					APIVersion: v1.SchemeGroupVersion.String(),
				},
				Parallelism:           16,
				DelayCacheUntilActive: false,
				DebuggingConfiguration: componentbaseconfig.DebuggingConfiguration{
					EnableProfiling:           true,
					EnableContentionProfiling: true,
				},
				LeaderElection: componentbaseconfig.LeaderElectionConfiguration{
					LeaderElect:       true,
					LeaseDuration:     metav1.Duration{Duration: 15 * time.Second},
					RenewDeadline:     metav1.Duration{Duration: 10 * time.Second},
					RetryPeriod:       metav1.Duration{Duration: 2 * time.Second},
					ResourceLock:      "leases",
					ResourceNamespace: "kube-system",
					ResourceName:      "kube-scheduler",
				},
				ClientConnection: componentbaseconfig.ClientConnectionConfiguration{
					Kubeconfig:  flagKubeconfig,
					QPS:         50,
					Burst:       100,
					ContentType: "application/vnd.kubernetes.protobuf",
				},
				PercentageOfNodesToScore: defaultPercentageOfNodesToScore,
				PodInitialBackoffSeconds: defaultPodInitialBackoffSeconds,
				PodMaxBackoffSeconds:     defaultPodMaxBackoffSeconds,
				Profiles: []kubeschedulerconfig.KubeSchedulerProfile{
					{
						SchedulerName: "default-scheduler",
						Plugins:       defaults.PluginsV1,
						PluginConfig:  defaults.PluginConfigsV1,
					},
				},
			},
			expectedUsername: "none, http",
		},
		{
			name: "plugin config",
			options: &Options{
				ConfigFile:               pluginConfigFile,
				Logs:                     logs.NewOptions(),
				ComponentGlobalsRegistry: componentGlobalsRegistry,
			},
			expectedUsername: "config",
			expectedConfig: kubeschedulerconfig.KubeSchedulerConfiguration{
				TypeMeta: metav1.TypeMeta{
					APIVersion: v1.SchemeGroupVersion.String(),
				},
				Parallelism:           16,
				DelayCacheUntilActive: false,
				DebuggingConfiguration: componentbaseconfig.DebuggingConfiguration{
					EnableProfiling:           true,
					EnableContentionProfiling: true,
				},
				LeaderElection: componentbaseconfig.LeaderElectionConfiguration{
					LeaderElect:       true,
					LeaseDuration:     metav1.Duration{Duration: 15 * time.Second},
					RenewDeadline:     metav1.Duration{Duration: 10 * time.Second},
					RetryPeriod:       metav1.Duration{Duration: 2 * time.Second},
					ResourceLock:      "leases",
					ResourceNamespace: "kube-system",
					ResourceName:      "kube-scheduler",
				},
				ClientConnection: componentbaseconfig.ClientConnectionConfiguration{
					Kubeconfig:  configKubeconfig,
					QPS:         50,
					Burst:       100,
					ContentType: "application/vnd.kubernetes.protobuf",
				},
				PercentageOfNodesToScore: defaultPercentageOfNodesToScore,
				PodInitialBackoffSeconds: defaultPodInitialBackoffSeconds,
				PodMaxBackoffSeconds:     defaultPodMaxBackoffSeconds,
				Profiles: []kubeschedulerconfig.KubeSchedulerProfile{
					{
						SchedulerName: "default-scheduler",
						Plugins: &kubeschedulerconfig.Plugins{
							PreEnqueue: kubeschedulerconfig.PluginSet{
								Enabled: []kubeschedulerconfig.Plugin{
									{Name: "foo"},
								},
							},
							Reserve: kubeschedulerconfig.PluginSet{
								Enabled: []kubeschedulerconfig.Plugin{
									{Name: "foo"},
									{Name: "bar"},
								},
								Disabled: []kubeschedulerconfig.Plugin{
									{Name: names.VolumeBinding},
								},
							},
							PreBind: kubeschedulerconfig.PluginSet{
								Enabled: []kubeschedulerconfig.Plugin{
									{Name: "foo"},
								},
								Disabled: []kubeschedulerconfig.Plugin{
									{Name: names.VolumeBinding},
								},
							},
							MultiPoint: defaults.PluginsV1.MultiPoint,
						},
						PluginConfig: []kubeschedulerconfig.PluginConfig{
							{
								Name: "InterPodAffinity",
								Args: &kubeschedulerconfig.InterPodAffinityArgs{
									HardPodAffinityWeight: 2,
								},
							},
							{
								Name: "foo",
								Args: &runtime.Unknown{
									Raw:         []byte(`{"bar":"baz"}`),
									ContentType: "application/json",
								},
							},
							{
								Name: "DefaultPreemption",
								Args: &kubeschedulerconfig.DefaultPreemptionArgs{
									MinCandidateNodesPercentage: 10,
									MinCandidateNodesAbsolute:   100,
								},
							},
							{
								Name: "NodeAffinity",
								Args: &kubeschedulerconfig.NodeAffinityArgs{},
							},
							{
								Name: "NodeResourcesBalancedAllocation",
								Args: &kubeschedulerconfig.NodeResourcesBalancedAllocationArgs{
									Resources: []kubeschedulerconfig.ResourceSpec{{Name: "cpu", Weight: 1}, {Name: "memory", Weight: 1}},
								},
							},
							{
								Name: "NodeResourcesFit",
								Args: &kubeschedulerconfig.NodeResourcesFitArgs{
									ScoringStrategy: &kubeschedulerconfig.ScoringStrategy{
										Type:      kubeschedulerconfig.LeastAllocated,
										Resources: []kubeschedulerconfig.ResourceSpec{{Name: "cpu", Weight: 1}, {Name: "memory", Weight: 1}},
									},
								},
							},
							{
								Name: "PodTopologySpread",
								Args: &kubeschedulerconfig.PodTopologySpreadArgs{
									DefaultingType: kubeschedulerconfig.SystemDefaulting,
								},
							},
							{
								Name: "VolumeBinding",
								Args: &kubeschedulerconfig.VolumeBindingArgs{
									BindTimeoutSeconds: 600,
								},
							},
						},
					},
				},
			},
		},
		{
			name: "multiple profiles",
			options: &Options{
				ConfigFile:               multiProfilesConfig,
				Logs:                     logs.NewOptions(),
				ComponentGlobalsRegistry: componentGlobalsRegistry,
			},
			expectedUsername: "config",
			expectedConfig: kubeschedulerconfig.KubeSchedulerConfiguration{
				TypeMeta: metav1.TypeMeta{
					APIVersion: v1.SchemeGroupVersion.String(),
				},
				Parallelism:           16,
				DelayCacheUntilActive: false,
				DebuggingConfiguration: componentbaseconfig.DebuggingConfiguration{
					EnableProfiling:           true,
					EnableContentionProfiling: true,
				},
				LeaderElection: componentbaseconfig.LeaderElectionConfiguration{
					LeaderElect:       true,
					LeaseDuration:     metav1.Duration{Duration: 15 * time.Second},
					RenewDeadline:     metav1.Duration{Duration: 10 * time.Second},
					RetryPeriod:       metav1.Duration{Duration: 2 * time.Second},
					ResourceLock:      "leases",
					ResourceNamespace: "kube-system",
					ResourceName:      "kube-scheduler",
				},
				ClientConnection: componentbaseconfig.ClientConnectionConfiguration{
					Kubeconfig:  configKubeconfig,
					QPS:         50,
					Burst:       100,
					ContentType: "application/vnd.kubernetes.protobuf",
				},
				PercentageOfNodesToScore: defaultPercentageOfNodesToScore,
				PodInitialBackoffSeconds: defaultPodInitialBackoffSeconds,
				PodMaxBackoffSeconds:     defaultPodMaxBackoffSeconds,
				Profiles: []kubeschedulerconfig.KubeSchedulerProfile{
					{
						SchedulerName: "foo-profile",
						Plugins: &kubeschedulerconfig.Plugins{
							MultiPoint: defaults.PluginsV1.MultiPoint,
							Reserve: kubeschedulerconfig.PluginSet{
								Enabled: []kubeschedulerconfig.Plugin{
									{Name: "foo"},
									{Name: names.VolumeBinding},
								},
								Disabled: []kubeschedulerconfig.Plugin{
									{Name: names.VolumeBinding},
								},
							},
						},
						PluginConfig: defaults.PluginConfigsV1,
					},
					{
						SchedulerName: "bar-profile",
						Plugins: &kubeschedulerconfig.Plugins{
							MultiPoint: defaults.PluginsV1.MultiPoint,
							PreBind: kubeschedulerconfig.PluginSet{
								Disabled: []kubeschedulerconfig.Plugin{
									{Name: names.VolumeBinding},
								},
							},
						},
						PluginConfig: []kubeschedulerconfig.PluginConfig{
							{
								Name: "foo",
							},
							{
								Name: "DefaultPreemption",
								Args: &kubeschedulerconfig.DefaultPreemptionArgs{
									MinCandidateNodesPercentage: 10,
									MinCandidateNodesAbsolute:   100,
								},
							},
							{
								Name: "InterPodAffinity",
								Args: &kubeschedulerconfig.InterPodAffinityArgs{
									HardPodAffinityWeight: 1,
								},
							},
							{
								Name: "NodeAffinity",
								Args: &kubeschedulerconfig.NodeAffinityArgs{},
							},
							{
								Name: "NodeResourcesBalancedAllocation",
								Args: &kubeschedulerconfig.NodeResourcesBalancedAllocationArgs{
									Resources: []kubeschedulerconfig.ResourceSpec{{Name: "cpu", Weight: 1}, {Name: "memory", Weight: 1}},
								},
							},
							{
								Name: "NodeResourcesFit",
								Args: &kubeschedulerconfig.NodeResourcesFitArgs{
									ScoringStrategy: &kubeschedulerconfig.ScoringStrategy{
										Type:      kubeschedulerconfig.LeastAllocated,
										Resources: []kubeschedulerconfig.ResourceSpec{{Name: "cpu", Weight: 1}, {Name: "memory", Weight: 1}},
									},
								},
							},
							{
								Name: "PodTopologySpread",
								Args: &kubeschedulerconfig.PodTopologySpreadArgs{
									DefaultingType: kubeschedulerconfig.SystemDefaulting,
								},
							},
							{
								Name: "VolumeBinding",
								Args: &kubeschedulerconfig.VolumeBindingArgs{
									BindTimeoutSeconds: 600,
								},
							},
						},
					},
				},
			},
		},
		{
			name: "no config",
			options: &Options{
				Logs:                     logs.NewOptions(),
				ComponentGlobalsRegistry: componentGlobalsRegistry,
			},
			expectedError: "no configuration has been provided",
		},
		{
			name: "unknown field",
			options: &Options{
				ConfigFile:               unknownFieldConfig,
				Logs:                     logs.NewOptions(),
				ComponentGlobalsRegistry: componentGlobalsRegistry,
			},
			expectedError: `unknown field "foo"`,
			checkErrFn:    runtime.IsStrictDecodingError,
		},
		{
			name: "duplicate fields",
			options: &Options{
				ConfigFile:               duplicateFieldConfig,
				Logs:                     logs.NewOptions(),
				ComponentGlobalsRegistry: componentGlobalsRegistry,
			},
			expectedError: `key "leaderElect" already set`,
			checkErrFn:    runtime.IsStrictDecodingError,
		},
		{
			name: "high throughput profile",
			options: &Options{
				ConfigFile:               highThroughputProfileConfig,
				Logs:                     logs.NewOptions(),
				ComponentGlobalsRegistry: componentGlobalsRegistry,
			},
			expectedUsername: "config",
			expectedConfig: kubeschedulerconfig.KubeSchedulerConfiguration{
				TypeMeta: metav1.TypeMeta{
					APIVersion: v1.SchemeGroupVersion.String(),
				},
				Parallelism:           16,
				DelayCacheUntilActive: false,
				DebuggingConfiguration: componentbaseconfig.DebuggingConfiguration{
					EnableProfiling:           true,
					EnableContentionProfiling: true,
				},
				LeaderElection: componentbaseconfig.LeaderElectionConfiguration{
					LeaderElect:       true,
					LeaseDuration:     metav1.Duration{Duration: 15 * time.Second},
					RenewDeadline:     metav1.Duration{Duration: 10 * time.Second},
					RetryPeriod:       metav1.Duration{Duration: 2 * time.Second},
					ResourceLock:      "leases",
					ResourceNamespace: "kube-system",
					ResourceName:      "kube-scheduler",
				},
				ClientConnection: componentbaseconfig.ClientConnectionConfiguration{
					Kubeconfig:  configKubeconfig,
					QPS:         50,
					Burst:       100,
					ContentType: "application/vnd.kubernetes.protobuf",
				},
				PercentageOfNodesToScore: defaultPercentageOfNodesToScore,
				PodInitialBackoffSeconds: defaultPodInitialBackoffSeconds,
				PodMaxBackoffSeconds:     defaultPodMaxBackoffSeconds,
				Profiles: []kubeschedulerconfig.KubeSchedulerProfile{
					{
						SchedulerName: "high-throughput-profile",
						Plugins: &kubeschedulerconfig.Plugins{
							QueueSort:  defaults.PluginsV1.QueueSort,
							PreFilter:  defaults.PluginsV1.PreFilter,
							Filter:     defaults.PluginsV1.Filter,
							PostFilter: defaults.PluginsV1.PostFilter,
							PreScore: kubeschedulerconfig.PluginSet{
								Enabled: []kubeschedulerconfig.Plugin{
									{Name: "InterPodAffinity"},
								},
							},
							Score:      defaults.PluginsV1.Score,
							Bind:       defaults.PluginsV1.Bind,
							PreBind:    defaults.PluginsV1.PreBind,
							Reserve:    defaults.PluginsV1.Reserve,
							MultiPoint: defaults.PluginsV1.MultiPoint,
						},
						PluginConfig: []kubeschedulerconfig.PluginConfig{
							{
								Name: "InterPodAffinity",
								Args: &kubeschedulerconfig.InterPodAffinityArgs{
									HardPodAffinityWeight:              1,
									IgnorePreferredTermsOfExistingPods: true,
								},
							},
							{
								Name: "DefaultPreemption",
								Args: &kubeschedulerconfig.DefaultPreemptionArgs{
									MinCandidateNodesPercentage: 10,
									MinCandidateNodesAbsolute:   100,
								},
							},
							{
								Name: "NodeAffinity",
								Args: &kubeschedulerconfig.NodeAffinityArgs{},
							},
							{
								Name: "NodeResourcesBalancedAllocation",
								Args: &kubeschedulerconfig.NodeResourcesBalancedAllocationArgs{
									Resources: []kubeschedulerconfig.ResourceSpec{{Name: "cpu", Weight: 1}, {Name: "memory", Weight: 1}},
								},
							},
							{
								Name: "NodeResourcesFit",
								Args: &kubeschedulerconfig.NodeResourcesFitArgs{
									ScoringStrategy: &kubeschedulerconfig.ScoringStrategy{
										Type:      kubeschedulerconfig.LeastAllocated,
										Resources: []kubeschedulerconfig.ResourceSpec{{Name: "cpu", Weight: 1}, {Name: "memory", Weight: 1}},
									},
								},
							},
							{
								Name: "PodTopologySpread",
								Args: &kubeschedulerconfig.PodTopologySpreadArgs{
									DefaultingType: kubeschedulerconfig.SystemDefaulting,
								},
							},
							{
								Name: "VolumeBinding",
								Args: &kubeschedulerconfig.VolumeBindingArgs{
									BindTimeoutSeconds: 600,
								},
							},
						},
					},
				},
			},
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.options.ComponentConfig == nil {
				if cfg, err := latest.Default(); err != nil {
					t.Fatal(err)
				} else {
					tc.options.ComponentConfig = cfg
				}
			}
			// create the config
			_, ctx := ktesting.NewTestContext(t)
			config, err := tc.options.Config(ctx)

			// handle errors
			if err != nil {
				if tc.expectedError != "" || tc.checkErrFn != nil {
					if tc.expectedError != "" {
						assert.ErrorContains(t, err, tc.expectedError)
					}
					if tc.checkErrFn != nil {
						assert.True(t, tc.checkErrFn(err), "got error: %v", err)
					}
					return
				}
				t.Errorf("unexpected error creating config: %v", err)
				return
			}

			if _, err := encodeConfig(&config.ComponentConfig); err != nil {
				t.Errorf("unexpected error in encodeConfig: %v", err)
			}

			if diff := cmp.Diff(tc.expectedConfig, config.ComponentConfig); diff != "" {
				t.Errorf("incorrect config (-want,+got):\n%s", diff)
			}

			// ensure we have a client
			if config.Client == nil {
				t.Error("unexpected nil client")
				return
			}

			// test the client talks to the endpoint we expect with the credentials we expect
			username = ""
			_, err = config.Client.Discovery().RESTClient().Get().AbsPath("/").DoRaw(context.TODO())
			if err != nil {
				t.Error(err)
				return
			}
			if username != tc.expectedUsername {
				t.Errorf("expected server call with user %q, got %q", tc.expectedUsername, username)
			}
		})
	}
}
