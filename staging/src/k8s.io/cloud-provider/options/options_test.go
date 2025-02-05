/*
Copyright 2017 The Kubernetes Authors.

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
	"fmt"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/spf13/pflag"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	apiserverapis "k8s.io/apiserver/pkg/apis/apiserver"
	apiserver "k8s.io/apiserver/pkg/server"
	apiserveroptions "k8s.io/apiserver/pkg/server/options"
	appconfig "k8s.io/cloud-provider/app/config"
	cpconfig "k8s.io/cloud-provider/config"
	nodeconfig "k8s.io/cloud-provider/controllers/node/config"
	serviceconfig "k8s.io/cloud-provider/controllers/service/config"
	componentbaseconfig "k8s.io/component-base/config"
	cmconfig "k8s.io/controller-manager/config"
	cmoptions "k8s.io/controller-manager/options"
	migration "k8s.io/controller-manager/pkg/leadermigration/options"
	netutils "k8s.io/utils/net"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDefaultFlags(t *testing.T) {
	s, err := NewCloudControllerManagerOptions()
	if err != nil {
		t.Errorf("unexpected err: %v", err)
	}

	expected := &CloudControllerManagerOptions{
		Generic: &cmoptions.GenericControllerManagerConfigurationOptions{
			GenericControllerManagerConfiguration: &cmconfig.GenericControllerManagerConfiguration{
				Address:         "0.0.0.0",
				MinResyncPeriod: metav1.Duration{Duration: 12 * time.Hour},
				ClientConnection: componentbaseconfig.ClientConnectionConfiguration{
					Kubeconfig:  "",
					ContentType: "application/vnd.kubernetes.protobuf",
					QPS:         20.0,
					Burst:       30,
				},
				ControllerStartInterval: metav1.Duration{Duration: 0},
				LeaderElection: componentbaseconfig.LeaderElectionConfiguration{
					ResourceLock:      "leases",
					LeaderElect:       true,
					LeaseDuration:     metav1.Duration{Duration: 15 * time.Second},
					RenewDeadline:     metav1.Duration{Duration: 10 * time.Second},
					RetryPeriod:       metav1.Duration{Duration: 2 * time.Second},
					ResourceName:      "cloud-controller-manager",
					ResourceNamespace: "kube-system",
				},
				Controllers: []string{"*"},
			},
			Debugging: &cmoptions.DebuggingOptions{
				DebuggingConfiguration: &componentbaseconfig.DebuggingConfiguration{
					EnableProfiling:           true,
					EnableContentionProfiling: false,
				},
			},
			LeaderMigration: &migration.LeaderMigrationOptions{},
		},
		KubeCloudShared: &KubeCloudSharedOptions{
			KubeCloudSharedConfiguration: &cpconfig.KubeCloudSharedConfiguration{
				RouteReconciliationPeriod: metav1.Duration{Duration: 10 * time.Second},
				NodeMonitorPeriod:         metav1.Duration{Duration: 5 * time.Second},
				ClusterName:               "kubernetes",
				ClusterCIDR:               "",
				AllocateNodeCIDRs:         false,
				CIDRAllocatorType:         "",
				ConfigureCloudRoutes:      true,
			},
			CloudProvider: &CloudProviderOptions{
				CloudProviderConfiguration: &cpconfig.CloudProviderConfiguration{
					Name:            "",
					CloudConfigFile: "",
				},
			},
		},
		NodeController: &NodeControllerOptions{
			NodeControllerConfiguration: &nodeconfig.NodeControllerConfiguration{
				ConcurrentNodeSyncs: 1,
			},
		},
		ServiceController: &ServiceControllerOptions{
			ServiceControllerConfiguration: &serviceconfig.ServiceControllerConfiguration{
				ConcurrentServiceSyncs: 1,
			},
		},
		Webhook: &WebhookOptions{},
		WebhookServing: &WebhookServingOptions{
			SecureServingOptions: &apiserveroptions.SecureServingOptions{
				ServerCert: apiserveroptions.GeneratableKeyCert{
					CertDirectory: "",
					PairName:      "cloud-controller-manager-webhook",
				},
				BindPort:    10260,
				BindAddress: netutils.ParseIPSloppy("0.0.0.0"),
			},
		},
		SecureServing: (&apiserveroptions.SecureServingOptions{
			BindPort:    10258,
			BindAddress: netutils.ParseIPSloppy("0.0.0.0"),
			ServerCert: apiserveroptions.GeneratableKeyCert{
				CertDirectory: "",
				PairName:      "cloud-controller-manager",
			},
			HTTP2MaxStreamsPerConnection: 0,
		}).WithLoopback(),
		Authentication: &apiserveroptions.DelegatingAuthenticationOptions{
			CacheTTL:            10 * time.Second,
			TokenRequestTimeout: 10 * time.Second,
			WebhookRetryBackoff: apiserveroptions.DefaultAuthWebhookRetryBackoff(),
			ClientCert:          apiserveroptions.ClientCertAuthenticationOptions{},
			RequestHeader: apiserveroptions.RequestHeaderAuthenticationOptions{
				UsernameHeaders:     []string{"x-remote-user"},
				UIDHeaders:          nil,
				GroupHeaders:        []string{"x-remote-group"},
				ExtraHeaderPrefixes: []string{"x-remote-extra-"},
			},
			RemoteKubeConfigFileOptional: true,
			Anonymous:                    &apiserverapis.AnonymousAuthConfig{Enabled: true},
		},
		Authorization: &apiserveroptions.DelegatingAuthorizationOptions{
			AllowCacheTTL:                10 * time.Second,
			DenyCacheTTL:                 10 * time.Second,
			ClientTimeout:                10 * time.Second,
			WebhookRetryBackoff:          apiserveroptions.DefaultAuthWebhookRetryBackoff(),
			RemoteKubeConfigFileOptional: true,
			AlwaysAllowPaths:             []string{"/healthz", "/readyz", "/livez"}, // note: this does not match /healthz/ or /healthz/*
			AlwaysAllowGroups:            []string{"system:masters"},
		},
		Master:                    "",
		NodeStatusUpdateFrequency: metav1.Duration{Duration: 5 * time.Minute},
	}
	if !reflect.DeepEqual(expected, s) {
		t.Errorf("Got different run options than expected.\nDifference detected on:\n%s", cmp.Diff(expected, s))
	}
}

func TestAddFlags(t *testing.T) {
	fs := pflag.NewFlagSet("addflagstest", pflag.ContinueOnError)

	s, err := NewCloudControllerManagerOptions()
	if err != nil {
		t.Errorf("unexpected err: %v", err)
	}

	for _, f := range s.Flags([]string{""}, []string{""}, nil, []string{""}, []string{""}).FlagSets {
		fs.AddFlagSet(f)
	}

	args := []string{
		"--allocate-node-cidrs=true",
		"--authorization-always-allow-paths=", // this proves that we can clear the default
		"--bind-address=192.168.4.21",
		"--cert-dir=/a/b/c",
		"--cloud-config=/cloud-config",
		"--cloud-provider=gce",
		"--cluster-cidr=1.2.3.4/24",
		"--cluster-name=k8s",
		"--configure-cloud-routes=false",
		"--contention-profiling=true",
		"--controller-start-interval=2m",
		"--controllers=foo,bar",
		"--http2-max-streams-per-connection=47",
		"--kube-api-burst=100",
		"--kube-api-content-type=application/vnd.kubernetes.protobuf",
		"--kube-api-qps=50.0",
		"--kubeconfig=/kubeconfig",
		"--leader-elect=false",
		"--leader-elect-lease-duration=30s",
		"--leader-elect-renew-deadline=15s",
		"--leader-elect-resource-lock=leases",
		"--leader-elect-retry-period=5s",
		"--master=192.168.4.20",
		"--min-resync-period=100m",
		"--node-status-update-frequency=10m",
		"--profiling=false",
		"--route-reconciliation-period=30s",
		"--secure-port=10001",
		"--use-service-account-credentials=false",
		"--concurrent-node-syncs=5",
		"--webhooks=foo,bar,-baz",
	}
	err = fs.Parse(args)
	if err != nil {
		t.Errorf("unexpected err: %v", err)
	}

	expected := &CloudControllerManagerOptions{
		Generic: &cmoptions.GenericControllerManagerConfigurationOptions{
			GenericControllerManagerConfiguration: &cmconfig.GenericControllerManagerConfiguration{
				Address:         "0.0.0.0",
				MinResyncPeriod: metav1.Duration{Duration: 100 * time.Minute},
				ClientConnection: componentbaseconfig.ClientConnectionConfiguration{
					Kubeconfig:  "/kubeconfig",
					ContentType: "application/vnd.kubernetes.protobuf",
					QPS:         50.0,
					Burst:       100,
				},
				ControllerStartInterval: metav1.Duration{Duration: 2 * time.Minute},
				LeaderElection: componentbaseconfig.LeaderElectionConfiguration{
					ResourceLock:      "leases",
					LeaderElect:       false,
					LeaseDuration:     metav1.Duration{Duration: 30 * time.Second},
					RenewDeadline:     metav1.Duration{Duration: 15 * time.Second},
					RetryPeriod:       metav1.Duration{Duration: 5 * time.Second},
					ResourceName:      "cloud-controller-manager",
					ResourceNamespace: "kube-system",
				},
				Controllers: []string{"foo", "bar"},
			},
			Debugging: &cmoptions.DebuggingOptions{
				DebuggingConfiguration: &componentbaseconfig.DebuggingConfiguration{
					EnableProfiling:           false,
					EnableContentionProfiling: true,
				},
			},
			LeaderMigration: &migration.LeaderMigrationOptions{},
		},
		KubeCloudShared: &KubeCloudSharedOptions{
			KubeCloudSharedConfiguration: &cpconfig.KubeCloudSharedConfiguration{
				RouteReconciliationPeriod: metav1.Duration{Duration: 30 * time.Second},
				NodeMonitorPeriod:         metav1.Duration{Duration: 5 * time.Second},
				ClusterName:               "k8s",
				ClusterCIDR:               "1.2.3.4/24",
				AllocateNodeCIDRs:         true,
				CIDRAllocatorType:         "RangeAllocator",
				ConfigureCloudRoutes:      false,
			},
			CloudProvider: &CloudProviderOptions{
				CloudProviderConfiguration: &cpconfig.CloudProviderConfiguration{
					Name:            "gce",
					CloudConfigFile: "/cloud-config",
				},
			},
		},
		NodeController: &NodeControllerOptions{
			NodeControllerConfiguration: &nodeconfig.NodeControllerConfiguration{
				ConcurrentNodeSyncs: 5,
			},
		},
		ServiceController: &ServiceControllerOptions{
			ServiceControllerConfiguration: &serviceconfig.ServiceControllerConfiguration{
				ConcurrentServiceSyncs: 1,
			},
		},
		Webhook: &WebhookOptions{
			Webhooks: []string{"foo", "bar", "-baz"},
		},
		WebhookServing: &WebhookServingOptions{
			SecureServingOptions: &apiserveroptions.SecureServingOptions{
				ServerCert: apiserveroptions.GeneratableKeyCert{
					CertDirectory: "",
					PairName:      "cloud-controller-manager-webhook",
				},
				BindPort:    10260,
				BindAddress: netutils.ParseIPSloppy("0.0.0.0"),
			},
		},
		SecureServing: (&apiserveroptions.SecureServingOptions{
			BindPort:    10001,
			BindAddress: netutils.ParseIPSloppy("192.168.4.21"),
			ServerCert: apiserveroptions.GeneratableKeyCert{
				CertDirectory: "/a/b/c",
				PairName:      "cloud-controller-manager",
			},
			HTTP2MaxStreamsPerConnection: 47,
		}).WithLoopback(),
		Authentication: &apiserveroptions.DelegatingAuthenticationOptions{
			CacheTTL:            10 * time.Second,
			TokenRequestTimeout: 10 * time.Second,
			WebhookRetryBackoff: apiserveroptions.DefaultAuthWebhookRetryBackoff(),
			ClientCert:          apiserveroptions.ClientCertAuthenticationOptions{},
			RequestHeader: apiserveroptions.RequestHeaderAuthenticationOptions{
				UsernameHeaders:     []string{"x-remote-user"},
				UIDHeaders:          nil,
				GroupHeaders:        []string{"x-remote-group"},
				ExtraHeaderPrefixes: []string{"x-remote-extra-"},
			},
			RemoteKubeConfigFileOptional: true,
			Anonymous:                    &apiserverapis.AnonymousAuthConfig{Enabled: true},
		},
		Authorization: &apiserveroptions.DelegatingAuthorizationOptions{
			AllowCacheTTL:                10 * time.Second,
			DenyCacheTTL:                 10 * time.Second,
			ClientTimeout:                10 * time.Second,
			WebhookRetryBackoff:          apiserveroptions.DefaultAuthWebhookRetryBackoff(),
			RemoteKubeConfigFileOptional: true,
			AlwaysAllowPaths:             []string{},
			AlwaysAllowGroups:            []string{"system:masters"},
		},
		Master:                    "192.168.4.20",
		NodeStatusUpdateFrequency: metav1.Duration{Duration: 10 * time.Minute},
	}
	if !reflect.DeepEqual(expected, s) {
		t.Errorf("Got different run options than expected.\nDifference detected on:\n%s", cmp.Diff(expected, s))
	}
}

func TestCreateConfig(t *testing.T) {
	fs := pflag.NewFlagSet("addflagstest", pflag.ContinueOnError)

	s, err := NewCloudControllerManagerOptions()
	if err != nil {
		t.Errorf("unexpected err: %v", err)
	}

	for _, f := range s.Flags([]string{""}, []string{""}, nil, []string{""}, []string{""}).FlagSets {
		fs.AddFlagSet(f)
	}

	tmpdir, err := os.MkdirTemp("", "options_test")
	if err != nil {
		t.Fatalf("%s", err)
	}
	defer os.RemoveAll(tmpdir)

	args := []string{
		"--webhooks=foo,bar,-baz",
		"--allocate-node-cidrs=true",
		"--authorization-always-allow-paths=",
		"--bind-address=0.0.0.0",
		"--secure-port=10200",
		fmt.Sprintf("--cert-dir=%s/certs", tmpdir),
		"--cloud-provider=aws",
		"--cluster-cidr=1.2.3.4/24",
		"--cluster-name=k8s",
		"--configure-cloud-routes=false",
		"--contention-profiling=true",
		"--controller-start-interval=2m",
		"--controllers=foo,bar",
		"--concurrent-node-syncs=1",
		"--http2-max-streams-per-connection=47",
		"--kube-api-burst=101",
		"--kube-api-content-type=application/vnd.kubernetes.protobuf",
		"--kube-api-qps=50.0",
		"--leader-elect=false",
		"--leader-elect-lease-duration=30s",
		"--leader-elect-renew-deadline=15s",
		"--leader-elect-resource-lock=leases",
		"--leader-elect-retry-period=5s",
		"--master=192.168.4.20",
		"--min-resync-period=100m",
		"--node-status-update-frequency=10m",
		"--profiling=false",
		"--route-reconciliation-period=30s",
		"--use-service-account-credentials=false",
		"--webhook-bind-address=0.0.0.0",
		"--webhook-secure-port=10300",
	}
	err = fs.Parse(args)
	require.NoError(t, err, "unexpected error: %s", err)

	fs.VisitAll(func(f *pflag.Flag) {
		fmt.Printf("%s: %s\n", f.Name, f.Value)
	})

	c, err := s.Config([]string{"foo", "bar"}, []string{}, nil, []string{"foo", "bar", "baz"}, []string{})
	require.NoError(t, err, "unexpected error: %s", err)

	expected := &appconfig.Config{
		ComponentConfig: cpconfig.CloudControllerManagerConfiguration{
			Generic: cmconfig.GenericControllerManagerConfiguration{
				Address:         "0.0.0.0",
				MinResyncPeriod: metav1.Duration{Duration: 100 * time.Minute},
				ClientConnection: componentbaseconfig.ClientConnectionConfiguration{
					ContentType: "application/vnd.kubernetes.protobuf",
					QPS:         50.0,
					Burst:       101,
				},
				ControllerStartInterval: metav1.Duration{Duration: 2 * time.Minute},
				LeaderElection: componentbaseconfig.LeaderElectionConfiguration{
					ResourceLock:      "leases",
					LeaderElect:       false,
					LeaseDuration:     metav1.Duration{Duration: 30 * time.Second},
					RenewDeadline:     metav1.Duration{Duration: 15 * time.Second},
					RetryPeriod:       metav1.Duration{Duration: 5 * time.Second},
					ResourceName:      "cloud-controller-manager",
					ResourceNamespace: "kube-system",
				},
				Controllers: []string{"foo", "bar"},
				Debugging: componentbaseconfig.DebuggingConfiguration{
					EnableProfiling:           false,
					EnableContentionProfiling: true,
				},
				LeaderMigration: cmconfig.LeaderMigrationConfiguration{},
			},
			KubeCloudShared: cpconfig.KubeCloudSharedConfiguration{
				RouteReconciliationPeriod: metav1.Duration{Duration: 30 * time.Second},
				NodeMonitorPeriod:         metav1.Duration{Duration: 5 * time.Second},
				ClusterName:               "k8s",
				ClusterCIDR:               "1.2.3.4/24",
				AllocateNodeCIDRs:         true,
				CIDRAllocatorType:         "RangeAllocator",
				ConfigureCloudRoutes:      false,
				CloudProvider: cpconfig.CloudProviderConfiguration{
					Name:            "aws",
					CloudConfigFile: "",
				},
			},
			ServiceController: serviceconfig.ServiceControllerConfiguration{
				ConcurrentServiceSyncs: 1,
			},
			NodeController:            nodeconfig.NodeControllerConfiguration{ConcurrentNodeSyncs: 1},
			NodeStatusUpdateFrequency: metav1.Duration{Duration: 10 * time.Minute},
			Webhook: cpconfig.WebhookConfiguration{
				Webhooks: []string{"foo", "bar", "-baz"},
			},
		},
		SecureServing:        nil,
		WebhookSecureServing: nil,
		Authentication:       apiserver.AuthenticationInfo{},
		Authorization:        apiserver.AuthorizationInfo{},
	}

	// Don't check
	c.SecureServing = nil
	assert.NotNil(t, c.WebhookSecureServing, "webhook secureserving shouldn't be nil")
	c.WebhookSecureServing = nil
	c.Authentication = apiserver.AuthenticationInfo{}
	c.Authorization = apiserver.AuthorizationInfo{}
	c.SharedInformers = nil
	c.VersionedClient = nil
	c.ClientBuilder = nil
	c.EventRecorder = nil
	c.EventBroadcaster = nil
	c.Kubeconfig = nil
	c.Client = nil
	c.LoopbackClientConfig = nil

	if !reflect.DeepEqual(expected, c) {
		t.Errorf("Got different config than expected.\nDifference detected on:\n%s", cmp.Diff(expected, c))
	}
}

func TestCreateConfigWithoutWebHooks(t *testing.T) {
	fs := pflag.NewFlagSet("addflagstest", pflag.ContinueOnError)

	s, err := NewCloudControllerManagerOptions()
	if err != nil {
		t.Errorf("unexpected err: %v", err)
	}

	for _, f := range s.Flags([]string{""}, []string{""}, nil, []string{""}, []string{""}).FlagSets {
		fs.AddFlagSet(f)
	}

	tmpdir, err := os.MkdirTemp("", "options_test")
	if err != nil {
		t.Fatalf("%s", err)
	}
	defer func() {
		if err := os.RemoveAll(tmpdir); err != nil {
			t.Error(err)
		}
	}()

	args := []string{
		"--allocate-node-cidrs=true",
		"--authorization-always-allow-paths=",
		"--bind-address=0.0.0.0",
		"--secure-port=10200",
		fmt.Sprintf("--cert-dir=%s/certs", tmpdir),
		"--cloud-provider=aws",
		"--cluster-cidr=1.2.3.4/24",
		"--cluster-name=k8s",
		"--configure-cloud-routes=false",
		"--contention-profiling=true",
		"--controller-start-interval=2m",
		"--controllers=foo,bar",
		"--concurrent-node-syncs=1",
		"--http2-max-streams-per-connection=47",
		"--kube-api-burst=101",
		"--kube-api-content-type=application/vnd.kubernetes.protobuf",
		"--kube-api-qps=50.0",
		"--leader-elect=false",
		"--leader-elect-lease-duration=30s",
		"--leader-elect-renew-deadline=15s",
		"--leader-elect-resource-lock=leases",
		"--leader-elect-retry-period=5s",
		"--master=192.168.4.20",
		"--min-resync-period=100m",
		"--node-status-update-frequency=10m",
		"--profiling=false",
		"--route-reconciliation-period=30s",
		"--use-service-account-credentials=false",
	}
	err = fs.Parse(args)
	if err != nil {
		t.Errorf("error parsing the arguments, error : %v", err)
	}

	fs.VisitAll(func(f *pflag.Flag) {
		fmt.Printf("%s: %s\n", f.Name, f.Value)
	})

	c, err := s.Config([]string{"foo", "bar"}, []string{}, nil, []string{"foo", "bar", "baz"}, []string{})
	if err != nil {
		t.Errorf("error generating config, error : %v", err)
	}

	expected := &appconfig.Config{
		ComponentConfig: cpconfig.CloudControllerManagerConfiguration{
			Generic: cmconfig.GenericControllerManagerConfiguration{
				Address:         "0.0.0.0",
				MinResyncPeriod: metav1.Duration{Duration: 100 * time.Minute},
				ClientConnection: componentbaseconfig.ClientConnectionConfiguration{
					ContentType: "application/vnd.kubernetes.protobuf",
					QPS:         50.0,
					Burst:       101,
				},
				ControllerStartInterval: metav1.Duration{Duration: 2 * time.Minute},
				LeaderElection: componentbaseconfig.LeaderElectionConfiguration{
					ResourceLock:      "leases",
					LeaderElect:       false,
					LeaseDuration:     metav1.Duration{Duration: 30 * time.Second},
					RenewDeadline:     metav1.Duration{Duration: 15 * time.Second},
					RetryPeriod:       metav1.Duration{Duration: 5 * time.Second},
					ResourceName:      "cloud-controller-manager",
					ResourceNamespace: "kube-system",
				},
				Controllers: []string{"foo", "bar"},
				Debugging: componentbaseconfig.DebuggingConfiguration{
					EnableProfiling:           false,
					EnableContentionProfiling: true,
				},
				LeaderMigration: cmconfig.LeaderMigrationConfiguration{},
			},
			KubeCloudShared: cpconfig.KubeCloudSharedConfiguration{
				RouteReconciliationPeriod: metav1.Duration{Duration: 30 * time.Second},
				NodeMonitorPeriod:         metav1.Duration{Duration: 5 * time.Second},
				ClusterName:               "k8s",
				ClusterCIDR:               "1.2.3.4/24",
				AllocateNodeCIDRs:         true,
				CIDRAllocatorType:         "RangeAllocator",
				ConfigureCloudRoutes:      false,
				CloudProvider: cpconfig.CloudProviderConfiguration{
					Name:            "aws",
					CloudConfigFile: "",
				},
			},
			ServiceController: serviceconfig.ServiceControllerConfiguration{
				ConcurrentServiceSyncs: 1,
			},
			NodeController:            nodeconfig.NodeControllerConfiguration{ConcurrentNodeSyncs: 1},
			NodeStatusUpdateFrequency: metav1.Duration{Duration: 10 * time.Minute},
			Webhook:                   cpconfig.WebhookConfiguration{},
		},
		SecureServing:        nil,
		WebhookSecureServing: nil,
		Authentication:       apiserver.AuthenticationInfo{},
		Authorization:        apiserver.AuthorizationInfo{},
	}

	// Don't check
	c.SecureServing = nil
	c.Authentication = apiserver.AuthenticationInfo{}
	c.Authorization = apiserver.AuthorizationInfo{}
	c.SharedInformers = nil
	c.VersionedClient = nil
	c.ClientBuilder = nil
	c.EventRecorder = nil
	c.EventBroadcaster = nil
	c.Kubeconfig = nil
	c.Client = nil
	c.LoopbackClientConfig = nil

	if !reflect.DeepEqual(expected, c) {
		t.Errorf("Got different config than expected.\nDifference detected on:\n%s", cmp.Diff(expected, c))
	}
}

func TestCloudControllerManagerAliases(t *testing.T) {
	opts, err := NewCloudControllerManagerOptions()
	if err != nil {
		t.Errorf("expected no error, error found %+v", err)
	}
	opts.KubeCloudShared.CloudProvider.Name = "gce"
	opts.Generic.Controllers = []string{"service-controller", "-service", "route", "-cloud-node-lifecycle-controller"}
	expectedControllers := []string{"service-controller", "-service-controller", "route-controller", "-cloud-node-lifecycle-controller"}

	allControllers := []string{
		"cloud-node-controller",
		"service-controller",
		"route-controller",
		"cloud-node-lifecycle-controller",
	}
	disabledByDefaultControllers := []string{}
	controllerAliases := map[string]string{
		"cloud-node":           "cloud-node-controller",
		"service":              "service-controller",
		"route":                "route-controller",
		"cloud-node-lifecycle": "cloud-node-lifecycle-controller",
	}

	if err := opts.Validate(allControllers, disabledByDefaultControllers, controllerAliases, nil, nil); err != nil {
		t.Errorf("expected no error, error found %v", err)
	}

	cfg := &cmconfig.GenericControllerManagerConfiguration{}
	if err := opts.Generic.ApplyTo(cfg, allControllers, disabledByDefaultControllers, controllerAliases); err != nil {
		t.Errorf("expected no error, error found %v", err)
	}
	if !reflect.DeepEqual(cfg.Controllers, expectedControllers) {
		t.Errorf("controller aliases not resolved correctly, expected %+v, got %+v", expectedControllers, cfg.Controllers)
	}
}
