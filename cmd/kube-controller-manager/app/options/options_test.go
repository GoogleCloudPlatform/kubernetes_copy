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
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/spf13/pflag"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/version"

	"k8s.io/apiserver/pkg/apis/apiserver"
	apiserveroptions "k8s.io/apiserver/pkg/server/options"
	basecompatibility "k8s.io/component-base/compatibility"

	componentbaseconfig "k8s.io/component-base/config"
	"k8s.io/component-base/featuregate"
	"k8s.io/component-base/logs"
	"k8s.io/component-base/metrics"

	cpconfig "k8s.io/cloud-provider/config"
	serviceconfig "k8s.io/cloud-provider/controllers/service/config"
	cpoptions "k8s.io/cloud-provider/options"

	eventv1 "k8s.io/api/events/v1"
	clientgofeaturegate "k8s.io/client-go/features"
	cmconfig "k8s.io/controller-manager/config"
	cmoptions "k8s.io/controller-manager/options"
	migration "k8s.io/controller-manager/pkg/leadermigration/options"
	kubecontrollerconfig "k8s.io/kubernetes/cmd/kube-controller-manager/app/config"
	kubectrlmgrconfig "k8s.io/kubernetes/pkg/controller/apis/config"
	csrsigningconfig "k8s.io/kubernetes/pkg/controller/certificates/signer/config"
	cronjobconfig "k8s.io/kubernetes/pkg/controller/cronjob/config"
	daemonconfig "k8s.io/kubernetes/pkg/controller/daemon/config"
	deploymentconfig "k8s.io/kubernetes/pkg/controller/deployment/config"
	endpointconfig "k8s.io/kubernetes/pkg/controller/endpoint/config"
	endpointsliceconfig "k8s.io/kubernetes/pkg/controller/endpointslice/config"
	endpointslicemirroringconfig "k8s.io/kubernetes/pkg/controller/endpointslicemirroring/config"
	garbagecollectorconfig "k8s.io/kubernetes/pkg/controller/garbagecollector/config"
	jobconfig "k8s.io/kubernetes/pkg/controller/job/config"
	namespaceconfig "k8s.io/kubernetes/pkg/controller/namespace/config"
	nodeipamconfig "k8s.io/kubernetes/pkg/controller/nodeipam/config"
	nodelifecycleconfig "k8s.io/kubernetes/pkg/controller/nodelifecycle/config"
	poautosclerconfig "k8s.io/kubernetes/pkg/controller/podautoscaler/config"
	podgcconfig "k8s.io/kubernetes/pkg/controller/podgc/config"
	replicasetconfig "k8s.io/kubernetes/pkg/controller/replicaset/config"
	replicationconfig "k8s.io/kubernetes/pkg/controller/replication/config"
	resourcequotaconfig "k8s.io/kubernetes/pkg/controller/resourcequota/config"
	serviceaccountconfig "k8s.io/kubernetes/pkg/controller/serviceaccount/config"
	statefulsetconfig "k8s.io/kubernetes/pkg/controller/statefulset/config"
	ttlafterfinishedconfig "k8s.io/kubernetes/pkg/controller/ttlafterfinished/config"
	validatingadmissionpolicystatusconfig "k8s.io/kubernetes/pkg/controller/validatingadmissionpolicystatus/config"
	attachdetachconfig "k8s.io/kubernetes/pkg/controller/volume/attachdetach/config"
	ephemeralvolumeconfig "k8s.io/kubernetes/pkg/controller/volume/ephemeral/config"
	persistentvolumeconfig "k8s.io/kubernetes/pkg/controller/volume/persistentvolume/config"
	netutils "k8s.io/utils/net"
)

var args = []string{
	"--allocate-node-cidrs=true",
	"--attach-detach-reconcile-sync-period=30s",
	"--cidr-allocator-type=CloudAllocator",
	"--cloud-config=/cloud-config",
	"--cloud-provider=gce",
	"--cluster-cidr=1.2.3.4/24",
	"--cluster-name=k8s",
	"--cluster-signing-cert-file=/cluster-signing-cert",
	"--cluster-signing-key-file=/cluster-signing-key",
	"--cluster-signing-kubelet-serving-cert-file=/cluster-signing-kubelet-serving/cert-file",
	"--cluster-signing-kubelet-serving-key-file=/cluster-signing-kubelet-serving/key-file",
	"--cluster-signing-kubelet-client-cert-file=/cluster-signing-kubelet-client/cert-file",
	"--cluster-signing-kubelet-client-key-file=/cluster-signing-kubelet-client/key-file",
	"--cluster-signing-kube-apiserver-client-cert-file=/cluster-signing-kube-apiserver-client/cert-file",
	"--cluster-signing-kube-apiserver-client-key-file=/cluster-signing-kube-apiserver-client/key-file",
	"--cluster-signing-legacy-unknown-cert-file=/cluster-signing-legacy-unknown/cert-file",
	"--cluster-signing-legacy-unknown-key-file=/cluster-signing-legacy-unknown/key-file",
	"--concurrent-deployment-syncs=10",
	"--concurrent-daemonset-syncs=10",
	"--concurrent-horizontal-pod-autoscaler-syncs=10",
	"--concurrent-statefulset-syncs=15",
	"--concurrent-endpoint-syncs=10",
	"--concurrent-ephemeralvolume-syncs=10",
	"--concurrent-service-endpoint-syncs=10",
	"--concurrent-gc-syncs=30",
	"--concurrent-namespace-syncs=20",
	"--concurrent-job-syncs=10",
	"--concurrent-cron-job-syncs=10",
	"--concurrent-replicaset-syncs=10",
	"--concurrent-resource-quota-syncs=10",
	"--concurrent-service-syncs=2",
	"--concurrent-serviceaccount-token-syncs=10",
	"--concurrent_rc_syncs=10",
	"--concurrent-validating-admission-policy-status-syncs=9",
	"--configure-cloud-routes=false",
	"--contention-profiling=true",
	"--controller-start-interval=2m",
	"--controllers=foo,bar",
	"--disable-attach-detach-reconcile-sync=true",
	"--enable-dynamic-provisioning=false",
	"--enable-garbage-collector=false",
	"--enable-hostpath-provisioner=true",
	"--cluster-signing-duration=10h",
	"--flex-volume-plugin-dir=/flex-volume-plugin",
	"--horizontal-pod-autoscaler-sync-period=45s",
	"--horizontal-pod-autoscaler-downscale-stabilization=3m",
	"--horizontal-pod-autoscaler-cpu-initialization-period=90s",
	"--horizontal-pod-autoscaler-initial-readiness-delay=50s",
	"--http2-max-streams-per-connection=47",
	"--kube-api-burst=100",
	"--kube-api-content-type=application/json",
	"--kube-api-qps=50.0",
	"--kubeconfig=/kubeconfig",
	"--large-cluster-size-threshold=100",
	"--leader-elect=false",
	"--leader-elect-lease-duration=30s",
	"--leader-elect-renew-deadline=15s",
	"--leader-elect-resource-lock=configmap",
	"--leader-elect-retry-period=5s",
	"--legacy-service-account-token-clean-up-period=8760h",
	"--master=192.168.4.20",
	"--max-endpoints-per-slice=200",
	"--min-resync-period=8h",
	"--mirroring-concurrent-service-endpoint-syncs=2",
	"--mirroring-max-endpoints-per-subset=1000",
	"--namespace-sync-period=10m",
	"--node-cidr-mask-size=48",
	"--node-cidr-mask-size-ipv4=48",
	"--node-cidr-mask-size-ipv6=108",
	"--node-eviction-rate=0.2",
	"--node-monitor-grace-period=30s",
	"--node-monitor-period=10s",
	"--node-startup-grace-period=30s",
	"--profiling=false",
	"--pv-recycler-increment-timeout-nfs=45",
	"--pv-recycler-minimum-timeout-hostpath=45",
	"--pv-recycler-minimum-timeout-nfs=200",
	"--pv-recycler-timeout-increment-hostpath=45",
	"--pvclaimbinder-sync-period=30s",
	"--resource-quota-sync-period=10m",
	"--route-reconciliation-period=30s",
	"--secondary-node-eviction-rate=0.05",
	"--service-account-private-key-file=/service-account-private-key",
	"--terminated-pod-gc-threshold=12000",
	"--unhealthy-zone-threshold=0.6",
	"--use-service-account-credentials=true",
	"--cert-dir=/a/b/c",
	"--bind-address=192.168.4.21",
	"--secure-port=10001",
	"--concurrent-ttl-after-finished-syncs=8",
}

func TestAddFlags(t *testing.T) {
	fs, s := setupControllerManagerFlagSet(t)

	fs.Parse(args)
	// Sort GCIgnoredResources because it's built from a map, which means the
	// insertion order is random.
	sort.Sort(sortedGCIgnoredResources(s.GarbageCollectorController.GCIgnoredResources))

	expected := &KubeControllerManagerOptions{
		Generic: &cmoptions.GenericControllerManagerConfigurationOptions{
			GenericControllerManagerConfiguration: &cmconfig.GenericControllerManagerConfiguration{
				Address:         "0.0.0.0", // Note: This field should have no effect in CM now, and "0.0.0.0" is the default value.
				MinResyncPeriod: metav1.Duration{Duration: 8 * time.Hour},
				ClientConnection: componentbaseconfig.ClientConnectionConfiguration{
					Kubeconfig:  "/kubeconfig",
					ContentType: "application/json",
					QPS:         50.0,
					Burst:       100,
				},
				ControllerStartInterval: metav1.Duration{Duration: 2 * time.Minute},
				LeaderElection: componentbaseconfig.LeaderElectionConfiguration{
					ResourceLock:      "configmap",
					LeaderElect:       false,
					LeaseDuration:     metav1.Duration{Duration: 30 * time.Second},
					RenewDeadline:     metav1.Duration{Duration: 15 * time.Second},
					RetryPeriod:       metav1.Duration{Duration: 5 * time.Second},
					ResourceName:      "kube-controller-manager",
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
		KubeCloudShared: &cpoptions.KubeCloudSharedOptions{
			KubeCloudSharedConfiguration: &cpconfig.KubeCloudSharedConfiguration{
				UseServiceAccountCredentials: true,
				RouteReconciliationPeriod:    metav1.Duration{Duration: 30 * time.Second},
				NodeMonitorPeriod:            metav1.Duration{Duration: 10 * time.Second},
				ClusterName:                  "k8s",
				ClusterCIDR:                  "1.2.3.4/24",
				AllocateNodeCIDRs:            true,
				CIDRAllocatorType:            "CloudAllocator",
				ConfigureCloudRoutes:         false,
			},
			CloudProvider: &cpoptions.CloudProviderOptions{
				CloudProviderConfiguration: &cpconfig.CloudProviderConfiguration{
					Name:            "gce",
					CloudConfigFile: "/cloud-config",
				},
			},
		},
		ServiceController: &cpoptions.ServiceControllerOptions{
			ServiceControllerConfiguration: &serviceconfig.ServiceControllerConfiguration{
				ConcurrentServiceSyncs: 2,
			},
		},
		AttachDetachController: &AttachDetachControllerOptions{
			&attachdetachconfig.AttachDetachControllerConfiguration{
				ReconcilerSyncLoopPeriod:          metav1.Duration{Duration: 30 * time.Second},
				DisableAttachDetachReconcilerSync: true,
			},
		},
		CSRSigningController: &CSRSigningControllerOptions{
			&csrsigningconfig.CSRSigningControllerConfiguration{
				ClusterSigningCertFile: "/cluster-signing-cert",
				ClusterSigningKeyFile:  "/cluster-signing-key",
				ClusterSigningDuration: metav1.Duration{Duration: 10 * time.Hour},
				KubeletServingSignerConfiguration: csrsigningconfig.CSRSigningConfiguration{
					CertFile: "/cluster-signing-kubelet-serving/cert-file",
					KeyFile:  "/cluster-signing-kubelet-serving/key-file",
				},
				KubeletClientSignerConfiguration: csrsigningconfig.CSRSigningConfiguration{
					CertFile: "/cluster-signing-kubelet-client/cert-file",
					KeyFile:  "/cluster-signing-kubelet-client/key-file",
				},
				KubeAPIServerClientSignerConfiguration: csrsigningconfig.CSRSigningConfiguration{
					CertFile: "/cluster-signing-kube-apiserver-client/cert-file",
					KeyFile:  "/cluster-signing-kube-apiserver-client/key-file",
				},
				LegacyUnknownSignerConfiguration: csrsigningconfig.CSRSigningConfiguration{
					CertFile: "/cluster-signing-legacy-unknown/cert-file",
					KeyFile:  "/cluster-signing-legacy-unknown/key-file",
				},
			},
		},
		DaemonSetController: &DaemonSetControllerOptions{
			&daemonconfig.DaemonSetControllerConfiguration{
				ConcurrentDaemonSetSyncs: 10,
			},
		},
		DeploymentController: &DeploymentControllerOptions{
			&deploymentconfig.DeploymentControllerConfiguration{
				ConcurrentDeploymentSyncs: 10,
			},
		},
		StatefulSetController: &StatefulSetControllerOptions{
			&statefulsetconfig.StatefulSetControllerConfiguration{
				ConcurrentStatefulSetSyncs: 15,
			},
		},
		DeprecatedFlags: &DeprecatedControllerOptions{
			&kubectrlmgrconfig.DeprecatedControllerConfiguration{},
		},
		EndpointController: &EndpointControllerOptions{
			&endpointconfig.EndpointControllerConfiguration{
				ConcurrentEndpointSyncs: 10,
			},
		},
		EndpointSliceController: &EndpointSliceControllerOptions{
			&endpointsliceconfig.EndpointSliceControllerConfiguration{
				ConcurrentServiceEndpointSyncs: 10,
				MaxEndpointsPerSlice:           200,
			},
		},
		EndpointSliceMirroringController: &EndpointSliceMirroringControllerOptions{
			&endpointslicemirroringconfig.EndpointSliceMirroringControllerConfiguration{
				MirroringConcurrentServiceEndpointSyncs: 2,
				MirroringMaxEndpointsPerSubset:          1000,
			},
		},
		EphemeralVolumeController: &EphemeralVolumeControllerOptions{
			&ephemeralvolumeconfig.EphemeralVolumeControllerConfiguration{
				ConcurrentEphemeralVolumeSyncs: 10,
			},
		},
		GarbageCollectorController: &GarbageCollectorControllerOptions{
			&garbagecollectorconfig.GarbageCollectorControllerConfiguration{
				ConcurrentGCSyncs: 30,
				GCIgnoredResources: []garbagecollectorconfig.GroupResource{
					{Group: "", Resource: "events"},
					{Group: eventv1.GroupName, Resource: "events"},
				},
				EnableGarbageCollector: false,
			},
		},
		HPAController: &HPAControllerOptions{
			&poautosclerconfig.HPAControllerConfiguration{
				ConcurrentHorizontalPodAutoscalerSyncs:              10,
				HorizontalPodAutoscalerSyncPeriod:                   metav1.Duration{Duration: 45 * time.Second},
				HorizontalPodAutoscalerDownscaleStabilizationWindow: metav1.Duration{Duration: 3 * time.Minute},
				HorizontalPodAutoscalerCPUInitializationPeriod:      metav1.Duration{Duration: 90 * time.Second},
				HorizontalPodAutoscalerInitialReadinessDelay:        metav1.Duration{Duration: 50 * time.Second},
				HorizontalPodAutoscalerTolerance:                    0.1,
			},
		},
		JobController: &JobControllerOptions{
			&jobconfig.JobControllerConfiguration{
				ConcurrentJobSyncs: 10,
			},
		},
		CronJobController: &CronJobControllerOptions{
			&cronjobconfig.CronJobControllerConfiguration{
				ConcurrentCronJobSyncs: 10,
			},
		},
		NamespaceController: &NamespaceControllerOptions{
			&namespaceconfig.NamespaceControllerConfiguration{
				NamespaceSyncPeriod:      metav1.Duration{Duration: 10 * time.Minute},
				ConcurrentNamespaceSyncs: 20,
			},
		},
		NodeIPAMController: &NodeIPAMControllerOptions{
			&nodeipamconfig.NodeIPAMControllerConfiguration{
				NodeCIDRMaskSize:     48,
				NodeCIDRMaskSizeIPv4: 48,
				NodeCIDRMaskSizeIPv6: 108,
			},
		},
		NodeLifecycleController: &NodeLifecycleControllerOptions{
			&nodelifecycleconfig.NodeLifecycleControllerConfiguration{
				NodeEvictionRate:          0.2,
				SecondaryNodeEvictionRate: 0.05,
				NodeMonitorGracePeriod:    metav1.Duration{Duration: 30 * time.Second},
				NodeStartupGracePeriod:    metav1.Duration{Duration: 30 * time.Second},
				LargeClusterSizeThreshold: 100,
				UnhealthyZoneThreshold:    0.6,
			},
		},
		PersistentVolumeBinderController: &PersistentVolumeBinderControllerOptions{
			&persistentvolumeconfig.PersistentVolumeBinderControllerConfiguration{
				PVClaimBinderSyncPeriod: metav1.Duration{Duration: 30 * time.Second},
				VolumeConfiguration: persistentvolumeconfig.VolumeConfiguration{
					EnableDynamicProvisioning:  false,
					EnableHostPathProvisioning: true,
					FlexVolumePluginDir:        "/flex-volume-plugin",
					PersistentVolumeRecyclerConfiguration: persistentvolumeconfig.PersistentVolumeRecyclerConfiguration{
						MaximumRetry:             3,
						MinimumTimeoutNFS:        200,
						IncrementTimeoutNFS:      45,
						MinimumTimeoutHostPath:   45,
						IncrementTimeoutHostPath: 45,
					},
				},
			},
		},
		PodGCController: &PodGCControllerOptions{
			&podgcconfig.PodGCControllerConfiguration{
				TerminatedPodGCThreshold: 12000,
			},
		},
		ReplicaSetController: &ReplicaSetControllerOptions{
			&replicasetconfig.ReplicaSetControllerConfiguration{
				ConcurrentRSSyncs: 10,
			},
		},
		ReplicationController: &ReplicationControllerOptions{
			&replicationconfig.ReplicationControllerConfiguration{
				ConcurrentRCSyncs: 10,
			},
		},
		ResourceQuotaController: &ResourceQuotaControllerOptions{
			&resourcequotaconfig.ResourceQuotaControllerConfiguration{
				ResourceQuotaSyncPeriod:      metav1.Duration{Duration: 10 * time.Minute},
				ConcurrentResourceQuotaSyncs: 10,
			},
		},
		SAController: &SAControllerOptions{
			&serviceaccountconfig.SAControllerConfiguration{
				ServiceAccountKeyFile:  "/service-account-private-key",
				ConcurrentSATokenSyncs: 10,
			},
		},
		LegacySATokenCleaner: &LegacySATokenCleanerOptions{
			&serviceaccountconfig.LegacySATokenCleanerConfiguration{
				CleanUpPeriod: metav1.Duration{Duration: 365 * 24 * time.Hour},
			},
		},
		TTLAfterFinishedController: &TTLAfterFinishedControllerOptions{
			&ttlafterfinishedconfig.TTLAfterFinishedControllerConfiguration{
				ConcurrentTTLSyncs: 8,
			},
		},
		ValidatingAdmissionPolicyStatusController: &ValidatingAdmissionPolicyStatusControllerOptions{
			&validatingadmissionpolicystatusconfig.ValidatingAdmissionPolicyStatusControllerConfiguration{
				ConcurrentPolicySyncs: 9,
			},
		},
		SecureServing: (&apiserveroptions.SecureServingOptions{
			BindPort:    10001,
			BindAddress: netutils.ParseIPSloppy("192.168.4.21"),
			ServerCert: apiserveroptions.GeneratableKeyCert{
				CertDirectory: "/a/b/c",
				PairName:      "kube-controller-manager",
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
			Anonymous:                    &apiserver.AnonymousAuthConfig{Enabled: true},
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
		Master:  "192.168.4.20",
		Metrics: &metrics.Options{},
		Logs:    logs.NewOptions(),
		// ignores comparing ComponentGlobalsRegistry in this test.
		ComponentGlobalsRegistry: s.ComponentGlobalsRegistry,
	}

	// Sort GCIgnoredResources because it's built from a map, which means the
	// insertion order is random.
	sort.Sort(sortedGCIgnoredResources(expected.GarbageCollectorController.GCIgnoredResources))

	if !reflect.DeepEqual(expected, s) {
		t.Errorf("Got different run options than expected.\nDifference detected on:\n%s", cmp.Diff(expected, s))
	}
}

func TestValidateFlags(t *testing.T) {
	testcases := []struct {
		name    string
		flags   []string // not a good place to test flagParse error
		wantErr bool     // this won't apply to flagParse, it only apply to KubeControllerManagerOptions.Validate
	}{
		{
			name:    "empty flags",
			flags:   []string{},
			wantErr: false,
		},
		{
			name: "cloud provider empty flag",
			flags: []string{
				"--cloud-provider", "",
			},
			wantErr: false,
		},
		{
			name: "cloud provider set but not external",
			flags: []string{
				"--cloud-provider=gce",
			},
			wantErr: true,
		},
		{
			name: "cloud provider set to external",
			flags: []string{
				"--cloud-provider=external",
			},
			wantErr: false,
		},
		{
			name: "nodeipam to cloudAllocator",
			flags: []string{
				"--cidr-allocator-type=CloudAllocator",
			},
			wantErr: true,
		},
		{
			name: "nodeipam to rangeAllocator",
			flags: []string{
				"--cidr-allocator-type=RangeAllocator",
			},
			wantErr: false,
		},
		{
			name: "nodeipam to IPAMFromCloud",
			flags: []string{
				"--cidr-allocator-type=IPAMFromCloud",
			},
			wantErr: true,
		},
		{
			name: "concurrent daemonset syncs set to 0",
			flags: []string{
				"--concurrent-daemonset-syncs=0",
			},
			wantErr: true,
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			fs, s := setupControllerManagerFlagSet(t)
			err := fs.Parse(tc.flags)
			checkTestError(t, err, false, "")
			err = s.Validate([]string{""}, []string{""}, nil)
			if !tc.wantErr && err != nil {
				t.Fatal(fmt.Errorf("expected no error, got %w", err))
			} else if tc.wantErr && err == nil {
				t.Fatal("expected error, got nil")
			}
		})
	}
}

func TestApplyTo(t *testing.T) {
	fs, s := setupControllerManagerFlagSet(t)

	fs.Parse(args)
	// Sort GCIgnoredResources because it's built from a map, which means the
	// insertion order is random.
	sort.Sort(sortedGCIgnoredResources(s.GarbageCollectorController.GCIgnoredResources))

	expected := &kubecontrollerconfig.Config{
		ComponentConfig: kubectrlmgrconfig.KubeControllerManagerConfiguration{
			Generic: cmconfig.GenericControllerManagerConfiguration{
				Address:         "0.0.0.0", // Note: This field should have no effect in CM now, and "0.0.0.0" is the default value.
				MinResyncPeriod: metav1.Duration{Duration: 8 * time.Hour},
				ClientConnection: componentbaseconfig.ClientConnectionConfiguration{
					Kubeconfig:  "/kubeconfig",
					ContentType: "application/json",
					QPS:         50.0,
					Burst:       100,
				},
				ControllerStartInterval: metav1.Duration{Duration: 2 * time.Minute},
				LeaderElection: componentbaseconfig.LeaderElectionConfiguration{
					ResourceLock:      "configmap",
					LeaderElect:       false,
					LeaseDuration:     metav1.Duration{Duration: 30 * time.Second},
					RenewDeadline:     metav1.Duration{Duration: 15 * time.Second},
					RetryPeriod:       metav1.Duration{Duration: 5 * time.Second},
					ResourceName:      "kube-controller-manager",
					ResourceNamespace: "kube-system",
				},
				Controllers: []string{"foo", "bar"},
				Debugging: componentbaseconfig.DebuggingConfiguration{
					EnableProfiling:           false,
					EnableContentionProfiling: true,
				},
			},
			KubeCloudShared: cpconfig.KubeCloudSharedConfiguration{
				UseServiceAccountCredentials: true,
				RouteReconciliationPeriod:    metav1.Duration{Duration: 30 * time.Second},
				NodeMonitorPeriod:            metav1.Duration{Duration: 10 * time.Second},
				ClusterName:                  "k8s",
				ClusterCIDR:                  "1.2.3.4/24",
				AllocateNodeCIDRs:            true,
				CIDRAllocatorType:            "CloudAllocator",
				ConfigureCloudRoutes:         false,
				CloudProvider: cpconfig.CloudProviderConfiguration{
					Name:            "gce",
					CloudConfigFile: "/cloud-config",
				},
			},
			ServiceController: serviceconfig.ServiceControllerConfiguration{
				ConcurrentServiceSyncs: 2,
			},
			AttachDetachController: attachdetachconfig.AttachDetachControllerConfiguration{
				ReconcilerSyncLoopPeriod:          metav1.Duration{Duration: 30 * time.Second},
				DisableAttachDetachReconcilerSync: true,
			},
			CSRSigningController: csrsigningconfig.CSRSigningControllerConfiguration{
				ClusterSigningCertFile: "/cluster-signing-cert",
				ClusterSigningKeyFile:  "/cluster-signing-key",
				ClusterSigningDuration: metav1.Duration{Duration: 10 * time.Hour},
				KubeletServingSignerConfiguration: csrsigningconfig.CSRSigningConfiguration{
					CertFile: "/cluster-signing-kubelet-serving/cert-file",
					KeyFile:  "/cluster-signing-kubelet-serving/key-file",
				},
				KubeletClientSignerConfiguration: csrsigningconfig.CSRSigningConfiguration{
					CertFile: "/cluster-signing-kubelet-client/cert-file",
					KeyFile:  "/cluster-signing-kubelet-client/key-file",
				},
				KubeAPIServerClientSignerConfiguration: csrsigningconfig.CSRSigningConfiguration{
					CertFile: "/cluster-signing-kube-apiserver-client/cert-file",
					KeyFile:  "/cluster-signing-kube-apiserver-client/key-file",
				},
				LegacyUnknownSignerConfiguration: csrsigningconfig.CSRSigningConfiguration{
					CertFile: "/cluster-signing-legacy-unknown/cert-file",
					KeyFile:  "/cluster-signing-legacy-unknown/key-file",
				},
			},
			DaemonSetController: daemonconfig.DaemonSetControllerConfiguration{
				ConcurrentDaemonSetSyncs: 10,
			},
			DeploymentController: deploymentconfig.DeploymentControllerConfiguration{
				ConcurrentDeploymentSyncs: 10,
			},
			StatefulSetController: statefulsetconfig.StatefulSetControllerConfiguration{
				ConcurrentStatefulSetSyncs: 15,
			},
			DeprecatedController: kubectrlmgrconfig.DeprecatedControllerConfiguration{},
			EndpointController: endpointconfig.EndpointControllerConfiguration{
				ConcurrentEndpointSyncs: 10,
			},
			EndpointSliceController: endpointsliceconfig.EndpointSliceControllerConfiguration{
				ConcurrentServiceEndpointSyncs: 10,
				MaxEndpointsPerSlice:           200,
			},
			EndpointSliceMirroringController: endpointslicemirroringconfig.EndpointSliceMirroringControllerConfiguration{
				MirroringConcurrentServiceEndpointSyncs: 2,
				MirroringMaxEndpointsPerSubset:          1000,
			},
			EphemeralVolumeController: ephemeralvolumeconfig.EphemeralVolumeControllerConfiguration{
				ConcurrentEphemeralVolumeSyncs: 10,
			},
			GarbageCollectorController: garbagecollectorconfig.GarbageCollectorControllerConfiguration{
				ConcurrentGCSyncs: 30,
				GCIgnoredResources: []garbagecollectorconfig.GroupResource{
					{Group: "", Resource: "events"},
					{Group: eventv1.GroupName, Resource: "events"},
				},
				EnableGarbageCollector: false,
			},
			HPAController: poautosclerconfig.HPAControllerConfiguration{
				ConcurrentHorizontalPodAutoscalerSyncs:              10,
				HorizontalPodAutoscalerSyncPeriod:                   metav1.Duration{Duration: 45 * time.Second},
				HorizontalPodAutoscalerDownscaleStabilizationWindow: metav1.Duration{Duration: 3 * time.Minute},
				HorizontalPodAutoscalerCPUInitializationPeriod:      metav1.Duration{Duration: 90 * time.Second},
				HorizontalPodAutoscalerInitialReadinessDelay:        metav1.Duration{Duration: 50 * time.Second},
				HorizontalPodAutoscalerTolerance:                    0.1,
			},
			JobController: jobconfig.JobControllerConfiguration{
				ConcurrentJobSyncs: 10,
			},
			CronJobController: cronjobconfig.CronJobControllerConfiguration{
				ConcurrentCronJobSyncs: 10,
			},
			NamespaceController: namespaceconfig.NamespaceControllerConfiguration{
				NamespaceSyncPeriod:      metav1.Duration{Duration: 10 * time.Minute},
				ConcurrentNamespaceSyncs: 20,
			},
			NodeIPAMController: nodeipamconfig.NodeIPAMControllerConfiguration{
				NodeCIDRMaskSize:     48,
				NodeCIDRMaskSizeIPv4: 48,
				NodeCIDRMaskSizeIPv6: 108,
			},
			NodeLifecycleController: nodelifecycleconfig.NodeLifecycleControllerConfiguration{
				NodeEvictionRate:          0.2,
				SecondaryNodeEvictionRate: 0.05,
				NodeMonitorGracePeriod:    metav1.Duration{Duration: 30 * time.Second},
				NodeStartupGracePeriod:    metav1.Duration{Duration: 30 * time.Second},
				LargeClusterSizeThreshold: 100,
				UnhealthyZoneThreshold:    0.6,
			},
			PersistentVolumeBinderController: persistentvolumeconfig.PersistentVolumeBinderControllerConfiguration{
				PVClaimBinderSyncPeriod: metav1.Duration{Duration: 30 * time.Second},
				VolumeConfiguration: persistentvolumeconfig.VolumeConfiguration{
					EnableDynamicProvisioning:  false,
					EnableHostPathProvisioning: true,
					FlexVolumePluginDir:        "/flex-volume-plugin",
					PersistentVolumeRecyclerConfiguration: persistentvolumeconfig.PersistentVolumeRecyclerConfiguration{
						MaximumRetry:             3,
						MinimumTimeoutNFS:        200,
						IncrementTimeoutNFS:      45,
						MinimumTimeoutHostPath:   45,
						IncrementTimeoutHostPath: 45,
					},
				},
			},
			PodGCController: podgcconfig.PodGCControllerConfiguration{
				TerminatedPodGCThreshold: 12000,
			},
			ReplicaSetController: replicasetconfig.ReplicaSetControllerConfiguration{
				ConcurrentRSSyncs: 10,
			},
			ReplicationController: replicationconfig.ReplicationControllerConfiguration{
				ConcurrentRCSyncs: 10,
			},
			ResourceQuotaController: resourcequotaconfig.ResourceQuotaControllerConfiguration{
				ResourceQuotaSyncPeriod:      metav1.Duration{Duration: 10 * time.Minute},
				ConcurrentResourceQuotaSyncs: 10,
			},
			SAController: serviceaccountconfig.SAControllerConfiguration{
				ServiceAccountKeyFile:  "/service-account-private-key",
				ConcurrentSATokenSyncs: 10,
			},
			LegacySATokenCleaner: serviceaccountconfig.LegacySATokenCleanerConfiguration{
				CleanUpPeriod: metav1.Duration{Duration: 365 * 24 * time.Hour},
			},
			TTLAfterFinishedController: ttlafterfinishedconfig.TTLAfterFinishedControllerConfiguration{
				ConcurrentTTLSyncs: 8,
			},
			ValidatingAdmissionPolicyStatusController: validatingadmissionpolicystatusconfig.ValidatingAdmissionPolicyStatusControllerConfiguration{
				ConcurrentPolicySyncs: 9,
			},
		},
	}

	// Sort GCIgnoredResources because it's built from a map, which means the
	// insertion order is random.
	sort.Sort(sortedGCIgnoredResources(expected.ComponentConfig.GarbageCollectorController.GCIgnoredResources))

	c := &kubecontrollerconfig.Config{}
	s.ApplyTo(c, []string{""}, []string{""}, nil)

	if !reflect.DeepEqual(expected.ComponentConfig, c.ComponentConfig) {
		t.Errorf("Got different configuration than expected.\nDifference detected on:\n%s", cmp.Diff(expected.ComponentConfig, c.ComponentConfig))
	}
}

func TestEmulatedVersion(t *testing.T) {

	testcases := []struct {
		name              string
		flags             []string // not a good place to test flagParse error
		wantErr           bool     // this won't apply to flagParse, it only apply to KubeControllerManagerOptions.Validate
		errorSubString    string
		wantFeaturesGates map[string]bool
	}{
		{
			name:              "default feature gates at binary version",
			flags:             []string{},
			wantErr:           false,
			wantFeaturesGates: map[string]bool{"kubeA": true, "kubeB": false},
		},
		{
			name: "emulating version out of range",
			flags: []string{
				"--emulated-version=1.28",
			},
			wantErr:           true,
			errorSubString:    "emulation version 1.28 is not between",
			wantFeaturesGates: nil,
		},
		{
			name: "default feature gates at emulated version",
			flags: []string{
				"--emulated-version=1.31",
			},
			wantFeaturesGates: map[string]bool{"kubeA": false, "kubeB": false},
		},
		{
			name: "set feature gates at emulated version",
			flags: []string{
				"--emulated-version=1.31",
				"--feature-gates=kubeA=false,kubeB=true",
			},
			wantFeaturesGates: map[string]bool{"kubeA": false, "kubeB": true},
		},
		{
			name: "cannot set locked feature gate",
			flags: []string{
				"--emulated-version=1.32",
				"--feature-gates=kubeA=false,kubeB=true",
			},
			errorSubString: "cannot set feature gate kubeA to false, feature is locked to true",
			wantErr:        true,
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			fs, s := setupControllerManagerFlagSet(t)
			fg := s.ComponentGlobalsRegistry.FeatureGateFor(basecompatibility.DefaultKubeComponent)
			err := fs.Parse(tc.flags)
			checkTestError(t, err, false, "")
			err = s.Validate([]string{""}, []string{""}, nil)
			checkTestError(t, err, tc.wantErr, tc.errorSubString)

			for feature, expected := range tc.wantFeaturesGates {
				if fg.Enabled(featuregate.Feature(feature)) != expected {
					t.Errorf("expected %s to be %v", feature, expected)
				}
			}
		})
	}
}

func TestValidateControllersOptions(t *testing.T) {
	testCases := []struct {
		name                   string
		expectErrors           bool
		expectedErrorSubString string
		options                interface {
			Validate() []error
		}
	}{
		{
			name:                   "AttachDetachControllerOptions reconciler sync loop period less than one second",
			expectErrors:           true,
			expectedErrorSubString: "duration time must be greater than one second",
			options: &AttachDetachControllerOptions{
				&attachdetachconfig.AttachDetachControllerConfiguration{
					ReconcilerSyncLoopPeriod:          metav1.Duration{Duration: time.Second / 2},
					DisableAttachDetachReconcilerSync: true,
				},
			},
		},
		{
			name:                   "CSRSigningControllerOptions KubeletServingSignerConfiguration no cert file",
			expectErrors:           true,
			expectedErrorSubString: "cannot specify key without cert",
			options: &CSRSigningControllerOptions{
				&csrsigningconfig.CSRSigningControllerConfiguration{
					ClusterSigningCertFile: "",
					ClusterSigningKeyFile:  "",
					ClusterSigningDuration: metav1.Duration{Duration: 10 * time.Hour},
					KubeletServingSignerConfiguration: csrsigningconfig.CSRSigningConfiguration{
						CertFile: "",
						KeyFile:  "/cluster-signing-kubelet-serving/key-file",
					},
					KubeletClientSignerConfiguration: csrsigningconfig.CSRSigningConfiguration{
						CertFile: "/cluster-signing-kubelet-client/cert-file",
						KeyFile:  "/cluster-signing-kubelet-client/key-file",
					},
					KubeAPIServerClientSignerConfiguration: csrsigningconfig.CSRSigningConfiguration{
						CertFile: "/cluster-signing-kube-apiserver-client/cert-file",
						KeyFile:  "/cluster-signing-kube-apiserver-client/key-file",
					},
					LegacyUnknownSignerConfiguration: csrsigningconfig.CSRSigningConfiguration{
						CertFile: "/cluster-signing-legacy-unknown/cert-file",
						KeyFile:  "/cluster-signing-legacy-unknown/key-file",
					},
				},
			},
		},
		{
			name:                   "CSRSigningControllerOptions KubeletServingSignerConfiguration no key file",
			expectErrors:           true,
			expectedErrorSubString: "cannot specify cert without key",
			options: &CSRSigningControllerOptions{
				&csrsigningconfig.CSRSigningControllerConfiguration{
					ClusterSigningCertFile: "",
					ClusterSigningKeyFile:  "",
					ClusterSigningDuration: metav1.Duration{Duration: 10 * time.Hour},
					KubeletServingSignerConfiguration: csrsigningconfig.CSRSigningConfiguration{
						CertFile: "/cluster-signing-kubelet-serving/cert-file",
						KeyFile:  "",
					},
					KubeletClientSignerConfiguration: csrsigningconfig.CSRSigningConfiguration{
						CertFile: "/cluster-signing-kubelet-client/cert-file",
						KeyFile:  "/cluster-signing-kubelet-client/key-file",
					},
					KubeAPIServerClientSignerConfiguration: csrsigningconfig.CSRSigningConfiguration{
						CertFile: "/cluster-signing-kube-apiserver-client/cert-file",
						KeyFile:  "/cluster-signing-kube-apiserver-client/key-file",
					},
					LegacyUnknownSignerConfiguration: csrsigningconfig.CSRSigningConfiguration{
						CertFile: "/cluster-signing-legacy-unknown/cert-file",
						KeyFile:  "/cluster-signing-legacy-unknown/key-file",
					},
				},
			},
		},
		{
			name:                   "CSRSigningControllerOptions KubeletClientSignerConfiguration no cert file",
			expectErrors:           true,
			expectedErrorSubString: "cannot specify key without cert",
			options: &CSRSigningControllerOptions{
				&csrsigningconfig.CSRSigningControllerConfiguration{
					ClusterSigningCertFile: "",
					ClusterSigningKeyFile:  "",
					ClusterSigningDuration: metav1.Duration{Duration: 10 * time.Hour},
					KubeletServingSignerConfiguration: csrsigningconfig.CSRSigningConfiguration{
						CertFile: "/cluster-signing-kubelet-serving/cert-file",
						KeyFile:  "/cluster-signing-kubelet-serving/key-file",
					},
					KubeletClientSignerConfiguration: csrsigningconfig.CSRSigningConfiguration{
						CertFile: "",
						KeyFile:  "/cluster-signing-kubelet-client/key-file",
					},
					KubeAPIServerClientSignerConfiguration: csrsigningconfig.CSRSigningConfiguration{
						CertFile: "/cluster-signing-kube-apiserver-client/cert-file",
						KeyFile:  "/cluster-signing-kube-apiserver-client/key-file",
					},
					LegacyUnknownSignerConfiguration: csrsigningconfig.CSRSigningConfiguration{
						CertFile: "/cluster-signing-legacy-unknown/cert-file",
						KeyFile:  "/cluster-signing-legacy-unknown/key-file",
					},
				},
			},
		},
		{
			name:                   "CSRSigningControllerOptions KubeletClientSignerConfiguration no key file",
			expectErrors:           true,
			expectedErrorSubString: "cannot specify cert without key",
			options: &CSRSigningControllerOptions{
				&csrsigningconfig.CSRSigningControllerConfiguration{
					ClusterSigningCertFile: "",
					ClusterSigningKeyFile:  "",
					ClusterSigningDuration: metav1.Duration{Duration: 10 * time.Hour},
					KubeletServingSignerConfiguration: csrsigningconfig.CSRSigningConfiguration{
						CertFile: "/cluster-signing-kubelet-serving/cert-file",
						KeyFile:  "/cluster-signing-kubelet-serving/key-file",
					},
					KubeletClientSignerConfiguration: csrsigningconfig.CSRSigningConfiguration{
						CertFile: "/cluster-signing-kubelet-client/cert-file",
						KeyFile:  "",
					},
					KubeAPIServerClientSignerConfiguration: csrsigningconfig.CSRSigningConfiguration{
						CertFile: "/cluster-signing-kube-apiserver-client/cert-file",
						KeyFile:  "/cluster-signing-kube-apiserver-client/key-file",
					},
					LegacyUnknownSignerConfiguration: csrsigningconfig.CSRSigningConfiguration{
						CertFile: "/cluster-signing-legacy-unknown/cert-file",
						KeyFile:  "/cluster-signing-legacy-unknown/key-file",
					},
				},
			},
		},
		{
			name:                   "CSRSigningControllerOptions KubeAPIServerClientSignerConfiguration no cert file",
			expectErrors:           true,
			expectedErrorSubString: "cannot specify key without cert",
			options: &CSRSigningControllerOptions{
				&csrsigningconfig.CSRSigningControllerConfiguration{
					ClusterSigningCertFile: "",
					ClusterSigningKeyFile:  "",
					ClusterSigningDuration: metav1.Duration{Duration: 10 * time.Hour},
					KubeletServingSignerConfiguration: csrsigningconfig.CSRSigningConfiguration{
						CertFile: "/cluster-signing-kubelet-serving/cert-file",
						KeyFile:  "/cluster-signing-kubelet-serving/key-file",
					},
					KubeletClientSignerConfiguration: csrsigningconfig.CSRSigningConfiguration{
						CertFile: "/cluster-signing-kubelet-client/cert-file",
						KeyFile:  "/cluster-signing-kubelet-client/key-file",
					},
					KubeAPIServerClientSignerConfiguration: csrsigningconfig.CSRSigningConfiguration{
						CertFile: "",
						KeyFile:  "/cluster-signing-kube-apiserver-client/key-file",
					},
					LegacyUnknownSignerConfiguration: csrsigningconfig.CSRSigningConfiguration{
						CertFile: "/cluster-signing-legacy-unknown/cert-file",
						KeyFile:  "/cluster-signing-legacy-unknown/key-file",
					},
				},
			},
		},
		{
			name:                   "CSRSigningControllerOptions KubeAPIServerClientSignerConfiguration no key file",
			expectErrors:           true,
			expectedErrorSubString: "cannot specify cert without key",
			options: &CSRSigningControllerOptions{
				&csrsigningconfig.CSRSigningControllerConfiguration{
					ClusterSigningCertFile: "",
					ClusterSigningKeyFile:  "",
					ClusterSigningDuration: metav1.Duration{Duration: 10 * time.Hour},
					KubeletServingSignerConfiguration: csrsigningconfig.CSRSigningConfiguration{
						CertFile: "/cluster-signing-kubelet-serving/cert-file",
						KeyFile:  "/cluster-signing-kubelet-serving/key-file",
					},
					KubeletClientSignerConfiguration: csrsigningconfig.CSRSigningConfiguration{
						CertFile: "/cluster-signing-kubelet-client/cert-file",
						KeyFile:  "/cluster-signing-kubelet-client/key-file",
					},
					KubeAPIServerClientSignerConfiguration: csrsigningconfig.CSRSigningConfiguration{
						CertFile: "/cluster-signing-kube-apiserver-client/cert-file",
						KeyFile:  "",
					},
					LegacyUnknownSignerConfiguration: csrsigningconfig.CSRSigningConfiguration{
						CertFile: "/cluster-signing-legacy-unknown/cert-file",
						KeyFile:  "/cluster-signing-legacy-unknown/key-file",
					},
				},
			},
		},
		{
			name:                   "CSRSigningControllerOptions LegacyUnknownSignerConfiguration no cert file",
			expectErrors:           true,
			expectedErrorSubString: "cannot specify key without cert",
			options: &CSRSigningControllerOptions{
				&csrsigningconfig.CSRSigningControllerConfiguration{
					ClusterSigningCertFile: "",
					ClusterSigningKeyFile:  "",
					ClusterSigningDuration: metav1.Duration{Duration: 10 * time.Hour},
					KubeletServingSignerConfiguration: csrsigningconfig.CSRSigningConfiguration{
						CertFile: "/cluster-signing-kubelet-serving/cert-file",
						KeyFile:  "/cluster-signing-kubelet-serving/key-file",
					},
					KubeletClientSignerConfiguration: csrsigningconfig.CSRSigningConfiguration{
						CertFile: "/cluster-signing-kubelet-client/cert-file",
						KeyFile:  "/cluster-signing-kubelet-client/key-file",
					},
					KubeAPIServerClientSignerConfiguration: csrsigningconfig.CSRSigningConfiguration{
						CertFile: "/cluster-signing-kube-apiserver-client/cert-file",
						KeyFile:  "/cluster-signing-kube-apiserver-client/key-file",
					},
					LegacyUnknownSignerConfiguration: csrsigningconfig.CSRSigningConfiguration{
						CertFile: "",
						KeyFile:  "/cluster-signing-legacy-unknown/key-file",
					},
				},
			},
		},
		{
			name:                   "CSRSigningControllerOptions LegacyUnknownSignerConfiguration no key file",
			expectErrors:           true,
			expectedErrorSubString: "cannot specify cert without key",
			options: &CSRSigningControllerOptions{
				&csrsigningconfig.CSRSigningControllerConfiguration{
					ClusterSigningCertFile: "",
					ClusterSigningKeyFile:  "",
					ClusterSigningDuration: metav1.Duration{Duration: 10 * time.Hour},
					KubeletServingSignerConfiguration: csrsigningconfig.CSRSigningConfiguration{
						CertFile: "/cluster-signing-kubelet-serving/cert-file",
						KeyFile:  "/cluster-signing-kubelet-serving/key-file",
					},
					KubeletClientSignerConfiguration: csrsigningconfig.CSRSigningConfiguration{
						CertFile: "/cluster-signing-kubelet-client/cert-file",
						KeyFile:  "/cluster-signing-kubelet-client/key-file",
					},
					KubeAPIServerClientSignerConfiguration: csrsigningconfig.CSRSigningConfiguration{
						CertFile: "/cluster-signing-kube-apiserver-client/cert-file",
						KeyFile:  "/cluster-signing-kube-apiserver-client/key-file",
					},
					LegacyUnknownSignerConfiguration: csrsigningconfig.CSRSigningConfiguration{
						CertFile: "/cluster-signing-legacy-unknown/cert-file",
						KeyFile:  "",
					},
				},
			},
		},
		{
			name:                   "CSRSigningControllerOptions specific file set along with cluster single signing file",
			expectErrors:           true,
			expectedErrorSubString: "cannot specify --cluster-signing-{cert,key}-file and other --cluster-signing-*-file flags at the same time",
			options: &CSRSigningControllerOptions{
				&csrsigningconfig.CSRSigningControllerConfiguration{
					ClusterSigningCertFile: "/cluster-signing-cert-file",
					ClusterSigningKeyFile:  "/cluster-signing-key-file",
					ClusterSigningDuration: metav1.Duration{Duration: 10 * time.Hour},
					KubeletServingSignerConfiguration: csrsigningconfig.CSRSigningConfiguration{
						CertFile: "/cluster-signing-kubelet-serving/cert-file",
						KeyFile:  "",
					},
					KubeletClientSignerConfiguration: csrsigningconfig.CSRSigningConfiguration{
						CertFile: "",
						KeyFile:  "",
					},
					KubeAPIServerClientSignerConfiguration: csrsigningconfig.CSRSigningConfiguration{
						CertFile: "",
						KeyFile:  "",
					},
					LegacyUnknownSignerConfiguration: csrsigningconfig.CSRSigningConfiguration{
						CertFile: "",
						KeyFile:  "",
					},
				},
			},
		},
		{
			name:                   "EndpointSliceControllerOptions ConcurrentServiceEndpointSyncs lower than minConcurrentServiceEndpointSyncs (1)",
			expectErrors:           true,
			expectedErrorSubString: "concurrent-service-endpoint-syncs must not be less than 1",
			options: &EndpointSliceControllerOptions{
				&endpointsliceconfig.EndpointSliceControllerConfiguration{
					ConcurrentServiceEndpointSyncs: 0,
					MaxEndpointsPerSlice:           200,
				},
			},
		},
		{
			name:                   "EndpointSliceControllerOptions ConcurrentServiceEndpointSyncs greater than maxConcurrentServiceEndpointSyncs (50)",
			expectErrors:           true,
			expectedErrorSubString: "concurrent-service-endpoint-syncs must not be more than 50",
			options: &EndpointSliceControllerOptions{
				&endpointsliceconfig.EndpointSliceControllerConfiguration{
					ConcurrentServiceEndpointSyncs: 51,
					MaxEndpointsPerSlice:           200,
				},
			},
		},
		{
			name:                   "EndpointSliceControllerOptions MaxEndpointsPerSlice lower than minMaxEndpointsPerSlice (1)",
			expectErrors:           true,
			expectedErrorSubString: "max-endpoints-per-slice must not be less than 1",
			options: &EndpointSliceControllerOptions{
				&endpointsliceconfig.EndpointSliceControllerConfiguration{
					ConcurrentServiceEndpointSyncs: 10,
					MaxEndpointsPerSlice:           0,
				},
			},
		},
		{
			name:                   "EndpointSliceControllerOptions MaxEndpointsPerSlice greater than maxMaxEndpointsPerSlice (1000)",
			expectErrors:           true,
			expectedErrorSubString: "max-endpoints-per-slice must not be more than 1000",
			options: &EndpointSliceControllerOptions{
				&endpointsliceconfig.EndpointSliceControllerConfiguration{
					ConcurrentServiceEndpointSyncs: 10,
					MaxEndpointsPerSlice:           1001,
				},
			},
		},
		{
			name:                   "EndpointSliceMirroringControllerOptions MirroringConcurrentServiceEndpointSyncs lower than mirroringMinConcurrentServiceEndpointSyncs (1)",
			expectErrors:           true,
			expectedErrorSubString: "mirroring-concurrent-service-endpoint-syncs must not be less than 1",
			options: &EndpointSliceMirroringControllerOptions{
				&endpointslicemirroringconfig.EndpointSliceMirroringControllerConfiguration{
					MirroringConcurrentServiceEndpointSyncs: 0,
					MirroringMaxEndpointsPerSubset:          100,
				},
			},
		},
		{
			name:                   "EndpointSliceMirroringControllerOptions MirroringConcurrentServiceEndpointSyncs greater than mirroringMaxConcurrentServiceEndpointSyncs (50)",
			expectErrors:           true,
			expectedErrorSubString: "mirroring-concurrent-service-endpoint-syncs must not be more than 50",
			options: &EndpointSliceMirroringControllerOptions{
				&endpointslicemirroringconfig.EndpointSliceMirroringControllerConfiguration{
					MirroringConcurrentServiceEndpointSyncs: 51,
					MirroringMaxEndpointsPerSubset:          100,
				},
			},
		},
		{
			name:                   "EndpointSliceMirroringControllerOptions MirroringMaxEndpointsPerSubset lower than mirroringMinMaxEndpointsPerSubset (1)",
			expectErrors:           true,
			expectedErrorSubString: "mirroring-max-endpoints-per-subset must not be less than 1",
			options: &EndpointSliceMirroringControllerOptions{
				&endpointslicemirroringconfig.EndpointSliceMirroringControllerConfiguration{
					MirroringConcurrentServiceEndpointSyncs: 10,
					MirroringMaxEndpointsPerSubset:          0,
				},
			},
		},
		{
			name:                   "EndpointSliceMirroringControllerOptions MirroringMaxEndpointsPerSubset greater than mirroringMaxMaxEndpointsPerSubset (1000)",
			expectErrors:           true,
			expectedErrorSubString: "mirroring-max-endpoints-per-subset must not be more than 1000",
			options: &EndpointSliceMirroringControllerOptions{
				&endpointslicemirroringconfig.EndpointSliceMirroringControllerConfiguration{
					MirroringConcurrentServiceEndpointSyncs: 10,
					MirroringMaxEndpointsPerSubset:          1001,
				},
			},
		},
		{
			name:                   "EphemeralVolumeControllerOptions ConcurrentEphemeralVolumeSyncs equal 0",
			expectErrors:           true,
			expectedErrorSubString: "concurrent-ephemeralvolume-syncs must be greater than 0",
			options: &EphemeralVolumeControllerOptions{
				&ephemeralvolumeconfig.EphemeralVolumeControllerConfiguration{
					ConcurrentEphemeralVolumeSyncs: 0,
				},
			},
		},
		{
			name:                   "HPAControllerOptions ConcurrentHorizontalPodAutoscalerSyncs equal 0",
			expectErrors:           true,
			expectedErrorSubString: "concurrent-horizontal-pod-autoscaler-syncs must be greater than 0",
			options: &HPAControllerOptions{
				&poautosclerconfig.HPAControllerConfiguration{
					ConcurrentHorizontalPodAutoscalerSyncs:              0,
					HorizontalPodAutoscalerSyncPeriod:                   metav1.Duration{Duration: 45 * time.Second},
					HorizontalPodAutoscalerDownscaleStabilizationWindow: metav1.Duration{Duration: 3 * time.Minute},
					HorizontalPodAutoscalerCPUInitializationPeriod:      metav1.Duration{Duration: 90 * time.Second},
					HorizontalPodAutoscalerInitialReadinessDelay:        metav1.Duration{Duration: 50 * time.Second},
					HorizontalPodAutoscalerTolerance:                    0.1,
				},
			},
		},
		{
			name:                   "NodeIPAMControllerOptions service cluster ip range more than two entries",
			expectErrors:           true,
			expectedErrorSubString: "--service-cluster-ip-range can not contain more than two entries",
			options: &NodeIPAMControllerOptions{
				&nodeipamconfig.NodeIPAMControllerConfiguration{
					ServiceCIDR:          "10.0.0.0/16,244.0.0.0/16,3000::/108",
					NodeCIDRMaskSize:     48,
					NodeCIDRMaskSizeIPv4: 48,
					NodeCIDRMaskSizeIPv6: 108,
				},
			},
		},
		{
			name:                   "StatefulSetControllerOptions ConcurrentStatefulSetSyncs equal 0",
			expectErrors:           true,
			expectedErrorSubString: "concurrent-statefulset-syncs must be greater than 0",
			options: &StatefulSetControllerOptions{
				&statefulsetconfig.StatefulSetControllerConfiguration{
					ConcurrentStatefulSetSyncs: 0,
				},
			},
		},
		{
			name:                   "JobControllerOptions ConcurrentJobSyncs equal 0",
			expectErrors:           true,
			expectedErrorSubString: "concurrent-job-syncs must be greater than 0",
			options: &JobControllerOptions{
				&jobconfig.JobControllerConfiguration{
					ConcurrentJobSyncs: 0,
				},
			},
		},
		{
			name:                   "CronJobControllerOptions ConcurrentCronJobSyncs equal 0",
			expectErrors:           true,
			expectedErrorSubString: "concurrent-cron-job-syncs must be greater than 0",
			options: &CronJobControllerOptions{
				&cronjobconfig.CronJobControllerConfiguration{
					ConcurrentCronJobSyncs: 0,
				},
			},
		},
		/* empty errs */
		{
			name:         "CronJobControllerOptions",
			expectErrors: false,
			options: &CronJobControllerOptions{
				&cronjobconfig.CronJobControllerConfiguration{
					ConcurrentCronJobSyncs: 10,
				},
			},
		},
		{
			name:         "DaemonSetControllerOptions",
			expectErrors: false,
			options: &DaemonSetControllerOptions{
				&daemonconfig.DaemonSetControllerConfiguration{
					ConcurrentDaemonSetSyncs: 2,
				},
			},
		},
		{
			name:         "DeploymentControllerOptions",
			expectErrors: false,
			options: &DeploymentControllerOptions{
				&deploymentconfig.DeploymentControllerConfiguration{
					ConcurrentDeploymentSyncs: 10,
				},
			},
		},
		{
			name:         "DeprecatedControllerOptions",
			expectErrors: false,
			options: &DeprecatedControllerOptions{
				&kubectrlmgrconfig.DeprecatedControllerConfiguration{},
			},
		},
		{
			name:         "EndpointControllerOptions",
			expectErrors: false,
			options: &EndpointControllerOptions{
				&endpointconfig.EndpointControllerConfiguration{
					ConcurrentEndpointSyncs: 10,
				},
			},
		},
		{
			name:         "GarbageCollectorControllerOptions",
			expectErrors: false,
			options: &GarbageCollectorControllerOptions{
				&garbagecollectorconfig.GarbageCollectorControllerConfiguration{
					ConcurrentGCSyncs: 30,
					GCIgnoredResources: []garbagecollectorconfig.GroupResource{
						{Group: "", Resource: "events"},
						{Group: eventv1.GroupName, Resource: "events"},
					},
					EnableGarbageCollector: false,
				},
			},
		},
		{
			name:         "JobControllerOptions",
			expectErrors: false,
			options: &JobControllerOptions{
				&jobconfig.JobControllerConfiguration{
					ConcurrentJobSyncs: 10,
				},
			},
		},
		{
			name:         "NamespaceControllerOptions",
			expectErrors: false,
			options: &NamespaceControllerOptions{
				&namespaceconfig.NamespaceControllerConfiguration{
					NamespaceSyncPeriod:      metav1.Duration{Duration: 10 * time.Minute},
					ConcurrentNamespaceSyncs: 20,
				},
			},
		},
		{
			name:         "NodeLifecycleControllerOptions",
			expectErrors: false,
			options: &NodeLifecycleControllerOptions{
				&nodelifecycleconfig.NodeLifecycleControllerConfiguration{
					NodeEvictionRate:          0.2,
					SecondaryNodeEvictionRate: 0.05,
					NodeMonitorGracePeriod:    metav1.Duration{Duration: 30 * time.Second},
					NodeStartupGracePeriod:    metav1.Duration{Duration: 30 * time.Second},
					LargeClusterSizeThreshold: 100,
					UnhealthyZoneThreshold:    0.6,
				},
			},
		},
		{
			name:         "PodGCControllerOptions",
			expectErrors: false,
			options: &PodGCControllerOptions{
				&podgcconfig.PodGCControllerConfiguration{
					TerminatedPodGCThreshold: 12000,
				},
			},
		},
		{
			name:         "ReplicaSetControllerOptions",
			expectErrors: false,
			options: &ReplicaSetControllerOptions{
				&replicasetconfig.ReplicaSetControllerConfiguration{
					ConcurrentRSSyncs: 10,
				},
			},
		},
		{
			name:         "ReplicationControllerOptions",
			expectErrors: false,
			options: &ReplicationControllerOptions{
				&replicationconfig.ReplicationControllerConfiguration{
					ConcurrentRCSyncs: 10,
				},
			},
		},
		{
			name:         "ResourceQuotaControllerOptions",
			expectErrors: false,
			options: &ResourceQuotaControllerOptions{
				&resourcequotaconfig.ResourceQuotaControllerConfiguration{
					ResourceQuotaSyncPeriod:      metav1.Duration{Duration: 10 * time.Minute},
					ConcurrentResourceQuotaSyncs: 10,
				},
			},
		},
		{
			name:         "SAControllerOptions",
			expectErrors: false,
			options: &SAControllerOptions{
				&serviceaccountconfig.SAControllerConfiguration{
					ServiceAccountKeyFile:  "/service-account-private-key",
					ConcurrentSATokenSyncs: 10,
				},
			},
		},
		{
			name:         "LegacySATokenCleanerOptions",
			expectErrors: false,
			options: &LegacySATokenCleanerOptions{
				&serviceaccountconfig.LegacySATokenCleanerConfiguration{
					CleanUpPeriod: metav1.Duration{Duration: 24 * 365 * time.Hour},
				},
			},
		},
		{
			name:         "TTLAfterFinishedControllerOptions",
			expectErrors: false,
			options: &TTLAfterFinishedControllerOptions{
				&ttlafterfinishedconfig.TTLAfterFinishedControllerConfiguration{
					ConcurrentTTLSyncs: 8,
				},
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			errs := tc.options.Validate()
			if len(errs) > 0 && !tc.expectErrors {
				t.Errorf("expected no errors, errors found %+v", errs)
			}

			if len(errs) == 0 && tc.expectErrors {
				t.Errorf("expected errors, no errors found")
			}

			if len(errs) > 0 && tc.expectErrors {
				gotErr := utilerrors.NewAggregate(errs).Error()
				if !strings.Contains(gotErr, tc.expectedErrorSubString) {
					t.Errorf("expected error: %s, got err: %v", tc.expectedErrorSubString, gotErr)
				}
			}
		})
	}
}

func TestValidateControllerManagerOptions(t *testing.T) {
	opts, err := NewKubeControllerManagerOptions()
	if err != nil {
		t.Errorf("expected no error, error found %+v", err)
	}

	opts.EndpointSliceController.MaxEndpointsPerSlice = 1001 // max endpoints per slice should be a positive integer <= 1000

	if err := opts.Validate([]string{"*"}, []string{""}, nil); err == nil {
		t.Error("expected error, no error found")
	}
}

func TestControllerManagerAliases(t *testing.T) {
	opts, err := NewKubeControllerManagerOptions()
	if err != nil {
		t.Errorf("expected no error, error found %+v", err)
	}
	opts.Generic.Controllers = []string{"deployment", "-job", "-cronjob-controller", "podgc", "token-cleaner-controller"}
	expectedControllers := []string{"deployment-controller", "-job-controller", "-cronjob-controller", "pod-garbage-collector-controller", "token-cleaner-controller"}

	allControllers := []string{
		"bootstrap-signer-controller",
		"job-controller",
		"deployment-controller",
		"cronjob-controller",
		"namespace-controller",
		"pod-garbage-collector-controller",
		"token-cleaner-controller",
	}
	disabledByDefaultControllers := []string{
		"bootstrap-signer-controller",
		"token-cleaner-controller",
	}
	controllerAliases := map[string]string{
		"bootstrapsigner": "bootstrap-signer-controller",
		"job":             "job-controller",
		"deployment":      "deployment-controller",
		"cronjob":         "cronjob-controller",
		"namespace":       "namespace-controller",
		"podgc":           "pod-garbage-collector-controller",
		"tokencleaner":    "token-cleaner-controller",
	}

	if err := opts.Validate(allControllers, disabledByDefaultControllers, controllerAliases); err != nil {
		t.Errorf("expected no error, error found %v", err)
	}

	cfg := &kubecontrollerconfig.Config{}
	if err := opts.ApplyTo(cfg, allControllers, disabledByDefaultControllers, controllerAliases); err != nil {
		t.Errorf("expected no error, error found %v", err)
	}
	if !reflect.DeepEqual(cfg.ComponentConfig.Generic.Controllers, expectedControllers) {
		t.Errorf("controller aliases not resolved correctly, expected %+v, got %+v", expectedControllers, cfg.ComponentConfig.Generic.Controllers)
	}
}

func TestWatchListClientFlagUsage(t *testing.T) {
	fs := pflag.NewFlagSet("addflagstest", pflag.ContinueOnError)
	s, _ := NewKubeControllerManagerOptions()
	for _, f := range s.Flags([]string{""}, []string{""}, nil).FlagSets {
		fs.AddFlagSet(f)
	}

	assertWatchListClientFeatureDefaultValue(t)
	assertWatchListCommandLineDefaultValue(t, fs)
}

func TestWatchListClientFlagChange(t *testing.T) {
	fs := pflag.NewFlagSet("addflagstest", pflag.ContinueOnError)
	s, err := NewKubeControllerManagerOptions()
	if err != nil {
		t.Fatal(fmt.Errorf("NewKubeControllerManagerOptions failed with %w", err))
	}

	for _, f := range s.Flags([]string{""}, []string{""}, nil).FlagSets {
		fs.AddFlagSet(f)
	}

	assertWatchListClientFeatureDefaultValue(t)
	assertWatchListCommandLineDefaultValue(t, fs)

	args := []string{fmt.Sprintf("--feature-gates=%v=true", clientgofeaturegate.WatchListClient)}
	if err := fs.Parse(args); err != nil {
		t.Fatal(fmt.Errorf("FlatSet.Parse failed with %w", err))
	}

	// this is needed to Apply parsed flags to GlobalRegistry, so the DefaultFeatureGate values can be set from the flag
	err = s.ComponentGlobalsRegistry.Set()
	if err != nil {
		t.Fatal(fmt.Errorf("ComponentGlobalsRegistry.Set failed with %w", err))
	}

	watchListClientValue := clientgofeaturegate.FeatureGates().Enabled(clientgofeaturegate.WatchListClient)
	if !watchListClientValue {
		t.Fatalf("expected %q feature gate to be enabled after setting the command line flag", clientgofeaturegate.WatchListClient)
	}
}

func assertWatchListClientFeatureDefaultValue(t *testing.T) {
	watchListClientDefaultValue := clientgofeaturegate.FeatureGates().Enabled(clientgofeaturegate.WatchListClient)
	if !watchListClientDefaultValue {
		t.Fatalf("expected %q feature gate to be enabled for KCM", clientgofeaturegate.WatchListClient)
	}
}

func assertWatchListCommandLineDefaultValue(t *testing.T, fs *pflag.FlagSet) {
	fgFlagName := "feature-gates"
	fg := fs.Lookup(fgFlagName)
	if fg == nil {
		t.Fatalf("didn't find %q flag", fgFlagName)
	}

	expectedWatchListClientString := "WatchListClient=true|false (BETA - default=true)"
	if !strings.Contains(fg.Usage, expectedWatchListClientString) {
		t.Fatalf("%q flag doesn't contain the expected usage for %v feature gate.\nExpected = %v\nUsage = %v", fgFlagName, clientgofeaturegate.WatchListClient, expectedWatchListClientString, fg.Usage)
	}
}

func setupControllerManagerFlagSet(t *testing.T) (*pflag.FlagSet, *KubeControllerManagerOptions) {
	fs := pflag.NewFlagSet("addflagstest", pflag.ContinueOnError)
	s, err := NewKubeControllerManagerOptions()
	if err != nil {
		t.Fatal(fmt.Errorf("NewKubeControllerManagerOptions failed with %w", err))
	}

	componentGlobalsRegistry := basecompatibility.NewComponentGlobalsRegistry()

	verKube := basecompatibility.NewEffectiveVersionFromString("1.32", "1.31", "1.31")
	fg := featuregate.NewVersionedFeatureGate(version.MustParse("1.32"))
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
	s.ComponentGlobalsRegistry = componentGlobalsRegistry

	for _, f := range s.Flags([]string{""}, []string{""}, nil).FlagSets {
		fs.AddFlagSet(f)
	}
	return fs, s
}

// caution: checkTestError use t.Fatal, to simplify caller handling.
// it also means it may break test code execution flow.
func checkTestError(t *testing.T, err error, expectingErr bool, expectedErrorSubString string) {
	if !expectingErr {
		if err != nil { // not expecting, but got error
			t.Fatal(fmt.Errorf("expected no error, got %w", err))
		}
		return // not expecting, and no error
	}

	// from this point we do expecting error
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	if expectedErrorSubString != "" && !strings.Contains(err.Error(), expectedErrorSubString) {
		t.Fatalf("expected error to contain %q, but got %q", expectedErrorSubString, err.Error())
	}
}

type sortedGCIgnoredResources []garbagecollectorconfig.GroupResource

func (r sortedGCIgnoredResources) Len() int {
	return len(r)
}

func (r sortedGCIgnoredResources) Less(i, j int) bool {
	if r[i].Group < r[j].Group {
		return true
	} else if r[i].Group > r[j].Group {
		return false
	}
	return r[i].Resource < r[j].Resource
}

func (r sortedGCIgnoredResources) Swap(i, j int) {
	r[i], r[j] = r[j], r[i]
}
