/*
Copyright 2014 The Kubernetes Authors.

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

// Package options provides the flags used for the controller manager.
package options

import (
	"fmt"
	"net"

	v1 "k8s.io/api/core/v1"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	apiserveroptions "k8s.io/apiserver/pkg/server/options"
	"k8s.io/apiserver/pkg/util/compatibility"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	clientgofeaturegate "k8s.io/client-go/features"
	clientset "k8s.io/client-go/kubernetes"
	clientgokubescheme "k8s.io/client-go/kubernetes/scheme"
	restclient "k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/record"
	cloudprovider "k8s.io/cloud-provider"
	cpnames "k8s.io/cloud-provider/names"
	cpoptions "k8s.io/cloud-provider/options"
	cliflag "k8s.io/component-base/cli/flag"
	basecompatibility "k8s.io/component-base/compatibility"
	"k8s.io/component-base/featuregate"
	"k8s.io/component-base/logs"
	logsapi "k8s.io/component-base/logs/api/v1"
	"k8s.io/component-base/metrics"
	cmoptions "k8s.io/controller-manager/options"
	"k8s.io/klog/v2"
	kubectrlmgrconfigv1alpha1 "k8s.io/kube-controller-manager/config/v1alpha1"
	kubecontrollerconfig "k8s.io/kubernetes/cmd/kube-controller-manager/app/config"
	"k8s.io/kubernetes/cmd/kube-controller-manager/names"
	"k8s.io/kubernetes/pkg/cluster/ports"
	kubectrlmgrconfig "k8s.io/kubernetes/pkg/controller/apis/config"
	kubectrlmgrconfigscheme "k8s.io/kubernetes/pkg/controller/apis/config/scheme"
	"k8s.io/kubernetes/pkg/controller/garbagecollector"
	garbagecollectorconfig "k8s.io/kubernetes/pkg/controller/garbagecollector/config"
	"k8s.io/kubernetes/pkg/controller/nodeipam/ipam"
	netutils "k8s.io/utils/net"

	// add the kubernetes feature gates
	_ "k8s.io/kubernetes/pkg/features"
)

const (
	// KubeControllerManagerUserAgent is the userAgent name when starting kube-controller managers.
	KubeControllerManagerUserAgent = "kube-controller-manager"
)

// KubeControllerManagerOptions is the main context object for the kube-controller manager.
type KubeControllerManagerOptions struct {
	Generic           *cmoptions.GenericControllerManagerConfigurationOptions
	KubeCloudShared   *cpoptions.KubeCloudSharedOptions
	ServiceController *cpoptions.ServiceControllerOptions

	AttachDetachController                    *AttachDetachControllerOptions
	CSRSigningController                      *CSRSigningControllerOptions
	DaemonSetController                       *DaemonSetControllerOptions
	DeploymentController                      *DeploymentControllerOptions
	StatefulSetController                     *StatefulSetControllerOptions
	DeprecatedFlags                           *DeprecatedControllerOptions
	EndpointController                        *EndpointControllerOptions
	EndpointSliceController                   *EndpointSliceControllerOptions
	EndpointSliceMirroringController          *EndpointSliceMirroringControllerOptions
	EphemeralVolumeController                 *EphemeralVolumeControllerOptions
	GarbageCollectorController                *GarbageCollectorControllerOptions
	HPAController                             *HPAControllerOptions
	JobController                             *JobControllerOptions
	CronJobController                         *CronJobControllerOptions
	LegacySATokenCleaner                      *LegacySATokenCleanerOptions
	NamespaceController                       *NamespaceControllerOptions
	NodeIPAMController                        *NodeIPAMControllerOptions
	NodeLifecycleController                   *NodeLifecycleControllerOptions
	PersistentVolumeBinderController          *PersistentVolumeBinderControllerOptions
	PodGCController                           *PodGCControllerOptions
	ReplicaSetController                      *ReplicaSetControllerOptions
	ReplicationController                     *ReplicationControllerOptions
	ResourceQuotaController                   *ResourceQuotaControllerOptions
	SAController                              *SAControllerOptions
	TTLAfterFinishedController                *TTLAfterFinishedControllerOptions
	ValidatingAdmissionPolicyStatusController *ValidatingAdmissionPolicyStatusControllerOptions

	SecureServing  *apiserveroptions.SecureServingOptionsWithLoopback
	Authentication *apiserveroptions.DelegatingAuthenticationOptions
	Authorization  *apiserveroptions.DelegatingAuthorizationOptions
	Metrics        *metrics.Options
	Logs           *logs.Options

	Master                      string
	ShowHiddenMetricsForVersion string

	// ComponentGlobalsRegistry is the registry where the effective versions and feature gates for all components are stored.
	ComponentGlobalsRegistry basecompatibility.ComponentGlobalsRegistry
}

// NewKubeControllerManagerOptions creates a new KubeControllerManagerOptions with a default config.
func NewKubeControllerManagerOptions() (*KubeControllerManagerOptions, error) {
	componentConfig, err := NewDefaultComponentConfig()
	if err != nil {
		return nil, err
	}

	componentGlobalsRegistry := compatibility.DefaultComponentGlobalsRegistry

	if componentGlobalsRegistry.EffectiveVersionFor(basecompatibility.DefaultKubeComponent) == nil {
		featureGate := utilfeature.DefaultMutableFeatureGate
		effectiveVersion := compatibility.DefaultBuildEffectiveVersion()
		utilruntime.Must(componentGlobalsRegistry.Register(basecompatibility.DefaultKubeComponent, effectiveVersion, featureGate))
	}

	s := KubeControllerManagerOptions{
		Generic:         cmoptions.NewGenericControllerManagerConfigurationOptions(&componentConfig.Generic),
		KubeCloudShared: cpoptions.NewKubeCloudSharedOptions(&componentConfig.KubeCloudShared),
		ServiceController: &cpoptions.ServiceControllerOptions{
			ServiceControllerConfiguration: &componentConfig.ServiceController,
		},
		AttachDetachController: &AttachDetachControllerOptions{
			&componentConfig.AttachDetachController,
		},
		CSRSigningController: &CSRSigningControllerOptions{
			&componentConfig.CSRSigningController,
		},
		DaemonSetController: &DaemonSetControllerOptions{
			&componentConfig.DaemonSetController,
		},
		DeploymentController: &DeploymentControllerOptions{
			&componentConfig.DeploymentController,
		},
		StatefulSetController: &StatefulSetControllerOptions{
			&componentConfig.StatefulSetController,
		},
		DeprecatedFlags: &DeprecatedControllerOptions{
			&componentConfig.DeprecatedController,
		},
		EndpointController: &EndpointControllerOptions{
			&componentConfig.EndpointController,
		},
		EndpointSliceController: &EndpointSliceControllerOptions{
			&componentConfig.EndpointSliceController,
		},
		EndpointSliceMirroringController: &EndpointSliceMirroringControllerOptions{
			&componentConfig.EndpointSliceMirroringController,
		},
		EphemeralVolumeController: &EphemeralVolumeControllerOptions{
			&componentConfig.EphemeralVolumeController,
		},
		GarbageCollectorController: &GarbageCollectorControllerOptions{
			&componentConfig.GarbageCollectorController,
		},
		HPAController: &HPAControllerOptions{
			&componentConfig.HPAController,
		},
		JobController: &JobControllerOptions{
			&componentConfig.JobController,
		},
		CronJobController: &CronJobControllerOptions{
			&componentConfig.CronJobController,
		},
		LegacySATokenCleaner: &LegacySATokenCleanerOptions{
			&componentConfig.LegacySATokenCleaner,
		},
		NamespaceController: &NamespaceControllerOptions{
			&componentConfig.NamespaceController,
		},
		NodeIPAMController: &NodeIPAMControllerOptions{
			&componentConfig.NodeIPAMController,
		},
		NodeLifecycleController: &NodeLifecycleControllerOptions{
			&componentConfig.NodeLifecycleController,
		},
		PersistentVolumeBinderController: &PersistentVolumeBinderControllerOptions{
			&componentConfig.PersistentVolumeBinderController,
		},
		PodGCController: &PodGCControllerOptions{
			&componentConfig.PodGCController,
		},
		ReplicaSetController: &ReplicaSetControllerOptions{
			&componentConfig.ReplicaSetController,
		},
		ReplicationController: &ReplicationControllerOptions{
			&componentConfig.ReplicationController,
		},
		ResourceQuotaController: &ResourceQuotaControllerOptions{
			&componentConfig.ResourceQuotaController,
		},
		SAController: &SAControllerOptions{
			&componentConfig.SAController,
		},
		TTLAfterFinishedController: &TTLAfterFinishedControllerOptions{
			&componentConfig.TTLAfterFinishedController,
		},
		ValidatingAdmissionPolicyStatusController: &ValidatingAdmissionPolicyStatusControllerOptions{
			&componentConfig.ValidatingAdmissionPolicyStatusController,
		},
		SecureServing:            apiserveroptions.NewSecureServingOptions().WithLoopback(),
		Authentication:           apiserveroptions.NewDelegatingAuthenticationOptions(),
		Authorization:            apiserveroptions.NewDelegatingAuthorizationOptions(),
		Metrics:                  metrics.NewOptions(),
		Logs:                     logs.NewOptions(),
		ComponentGlobalsRegistry: componentGlobalsRegistry,
	}

	s.Authentication.RemoteKubeConfigFileOptional = true
	s.Authorization.RemoteKubeConfigFileOptional = true

	// Set the PairName but leave certificate directory blank to generate in-memory by default
	s.SecureServing.ServerCert.CertDirectory = ""
	s.SecureServing.ServerCert.PairName = "kube-controller-manager"
	s.SecureServing.BindPort = ports.KubeControllerManagerPort

	gcIgnoredResources := make([]garbagecollectorconfig.GroupResource, 0, len(garbagecollector.DefaultIgnoredResources()))
	for r := range garbagecollector.DefaultIgnoredResources() {
		gcIgnoredResources = append(gcIgnoredResources, garbagecollectorconfig.GroupResource{Group: r.Group, Resource: r.Resource})
	}

	s.GarbageCollectorController.GCIgnoredResources = gcIgnoredResources
	s.Generic.LeaderElection.ResourceName = "kube-controller-manager"
	s.Generic.LeaderElection.ResourceNamespace = "kube-system"
	return &s, nil
}

// NewDefaultComponentConfig returns kube-controller manager configuration object.
func NewDefaultComponentConfig() (kubectrlmgrconfig.KubeControllerManagerConfiguration, error) {
	versioned := kubectrlmgrconfigv1alpha1.KubeControllerManagerConfiguration{}
	kubectrlmgrconfigscheme.Scheme.Default(&versioned)

	internal := kubectrlmgrconfig.KubeControllerManagerConfiguration{}
	if err := kubectrlmgrconfigscheme.Scheme.Convert(&versioned, &internal, nil); err != nil {
		return internal, err
	}
	return internal, nil
}

// Flags returns flags for a specific KubeController by section name
func (s *KubeControllerManagerOptions) Flags(allControllers []string, disabledByDefaultControllers []string, controllerAliases map[string]string) cliflag.NamedFlagSets {
	fss := cliflag.NamedFlagSets{}
	s.Generic.AddFlags(&fss, allControllers, disabledByDefaultControllers, controllerAliases)
	s.KubeCloudShared.AddFlags(fss.FlagSet("generic"))
	s.ServiceController.AddFlags(fss.FlagSet(cpnames.ServiceLBController))

	s.SecureServing.AddFlags(fss.FlagSet("secure serving"))
	s.Authentication.AddFlags(fss.FlagSet("authentication"))
	s.Authorization.AddFlags(fss.FlagSet("authorization"))

	s.AttachDetachController.AddFlags(fss.FlagSet(names.PersistentVolumeAttachDetachController))
	s.CSRSigningController.AddFlags(fss.FlagSet(names.CertificateSigningRequestSigningController))
	s.DeploymentController.AddFlags(fss.FlagSet(names.DeploymentController))
	s.StatefulSetController.AddFlags(fss.FlagSet(names.StatefulSetController))
	s.DaemonSetController.AddFlags(fss.FlagSet(names.DaemonSetController))
	s.DeprecatedFlags.AddFlags(fss.FlagSet("deprecated"))
	s.EndpointController.AddFlags(fss.FlagSet(names.EndpointsController))
	s.EndpointSliceController.AddFlags(fss.FlagSet(names.EndpointSliceController))
	s.EndpointSliceMirroringController.AddFlags(fss.FlagSet(names.EndpointSliceMirroringController))
	s.EphemeralVolumeController.AddFlags(fss.FlagSet(names.EphemeralVolumeController))
	s.GarbageCollectorController.AddFlags(fss.FlagSet(names.GarbageCollectorController))
	s.HPAController.AddFlags(fss.FlagSet(names.HorizontalPodAutoscalerController))
	s.JobController.AddFlags(fss.FlagSet(names.JobController))
	s.CronJobController.AddFlags(fss.FlagSet(names.CronJobController))
	s.LegacySATokenCleaner.AddFlags(fss.FlagSet(names.LegacyServiceAccountTokenCleanerController))
	s.NamespaceController.AddFlags(fss.FlagSet(names.NamespaceController))
	s.NodeIPAMController.AddFlags(fss.FlagSet(names.NodeIpamController))
	s.NodeLifecycleController.AddFlags(fss.FlagSet(names.NodeLifecycleController))
	s.PersistentVolumeBinderController.AddFlags(fss.FlagSet(names.PersistentVolumeBinderController))
	s.PodGCController.AddFlags(fss.FlagSet(names.PodGarbageCollectorController))
	s.ReplicaSetController.AddFlags(fss.FlagSet(names.ReplicaSetController))
	s.ReplicationController.AddFlags(fss.FlagSet(names.ReplicationControllerController))
	s.ResourceQuotaController.AddFlags(fss.FlagSet(names.ResourceQuotaController))
	s.SAController.AddFlags(fss.FlagSet(names.ServiceAccountController))
	s.TTLAfterFinishedController.AddFlags(fss.FlagSet(names.TTLAfterFinishedController))
	s.ValidatingAdmissionPolicyStatusController.AddFlags(fss.FlagSet(names.ValidatingAdmissionPolicyStatusController))

	s.Metrics.AddFlags(fss.FlagSet("metrics"))
	logsapi.AddFlags(s.Logs, fss.FlagSet("logs"))

	fs := fss.FlagSet("misc")
	fs.StringVar(&s.Master, "master", s.Master, "The address of the Kubernetes API server (overrides any value in kubeconfig).")
	fs.StringVar(&s.Generic.ClientConnection.Kubeconfig, "kubeconfig", s.Generic.ClientConnection.Kubeconfig, "Path to kubeconfig file with authorization and master location information (the master location can be overridden by the master flag).")

	if !utilfeature.DefaultFeatureGate.Enabled(featuregate.Feature(clientgofeaturegate.WatchListClient)) {
		if err := utilfeature.DefaultMutableFeatureGate.OverrideDefault(featuregate.Feature(clientgofeaturegate.WatchListClient), true); err != nil {
			// it turns out that there are some integration tests that start multiple control plane components which
			// share global DefaultFeatureGate/DefaultMutableFeatureGate variables.
			// in those cases, the above call will fail (FG already registered and cannot be overridden), and the error will be logged.
			klog.Errorf("unable to set %s feature gate, err: %v", clientgofeaturegate.WatchListClient, err)
		}
	}

	s.ComponentGlobalsRegistry.AddFlags(fss.FlagSet("generic"))

	return fss
}

// ApplyTo fills up controller manager config with options.
func (s *KubeControllerManagerOptions) ApplyTo(c *kubecontrollerconfig.Config, allControllers []string, disabledByDefaultControllers []string, controllerAliases map[string]string) error {
	if err := s.ComponentGlobalsRegistry.SetFallback(); err != nil {
		return err
	}
	if err := s.Generic.ApplyTo(&c.ComponentConfig.Generic, allControllers, disabledByDefaultControllers, controllerAliases); err != nil {
		return err
	}
	if err := s.KubeCloudShared.ApplyTo(&c.ComponentConfig.KubeCloudShared); err != nil {
		return err
	}
	if err := s.AttachDetachController.ApplyTo(&c.ComponentConfig.AttachDetachController); err != nil {
		return err
	}
	if err := s.CSRSigningController.ApplyTo(&c.ComponentConfig.CSRSigningController); err != nil {
		return err
	}
	if err := s.DaemonSetController.ApplyTo(&c.ComponentConfig.DaemonSetController); err != nil {
		return err
	}
	if err := s.DeploymentController.ApplyTo(&c.ComponentConfig.DeploymentController); err != nil {
		return err
	}
	if err := s.StatefulSetController.ApplyTo(&c.ComponentConfig.StatefulSetController); err != nil {
		return err
	}
	if err := s.DeprecatedFlags.ApplyTo(&c.ComponentConfig.DeprecatedController); err != nil {
		return err
	}
	if err := s.EndpointController.ApplyTo(&c.ComponentConfig.EndpointController); err != nil {
		return err
	}
	if err := s.EndpointSliceController.ApplyTo(&c.ComponentConfig.EndpointSliceController); err != nil {
		return err
	}
	if err := s.EndpointSliceMirroringController.ApplyTo(&c.ComponentConfig.EndpointSliceMirroringController); err != nil {
		return err
	}
	if err := s.EphemeralVolumeController.ApplyTo(&c.ComponentConfig.EphemeralVolumeController); err != nil {
		return err
	}
	if err := s.GarbageCollectorController.ApplyTo(&c.ComponentConfig.GarbageCollectorController); err != nil {
		return err
	}
	if err := s.HPAController.ApplyTo(&c.ComponentConfig.HPAController); err != nil {
		return err
	}
	if err := s.JobController.ApplyTo(&c.ComponentConfig.JobController); err != nil {
		return err
	}
	if err := s.CronJobController.ApplyTo(&c.ComponentConfig.CronJobController); err != nil {
		return err
	}
	if err := s.LegacySATokenCleaner.ApplyTo(&c.ComponentConfig.LegacySATokenCleaner); err != nil {
		return err
	}
	if err := s.NamespaceController.ApplyTo(&c.ComponentConfig.NamespaceController); err != nil {
		return err
	}
	if err := s.NodeIPAMController.ApplyTo(&c.ComponentConfig.NodeIPAMController); err != nil {
		return err
	}
	if err := s.NodeLifecycleController.ApplyTo(&c.ComponentConfig.NodeLifecycleController); err != nil {
		return err
	}
	if err := s.PersistentVolumeBinderController.ApplyTo(&c.ComponentConfig.PersistentVolumeBinderController); err != nil {
		return err
	}
	if err := s.PodGCController.ApplyTo(&c.ComponentConfig.PodGCController); err != nil {
		return err
	}
	if err := s.ReplicaSetController.ApplyTo(&c.ComponentConfig.ReplicaSetController); err != nil {
		return err
	}
	if err := s.ReplicationController.ApplyTo(&c.ComponentConfig.ReplicationController); err != nil {
		return err
	}
	if err := s.ResourceQuotaController.ApplyTo(&c.ComponentConfig.ResourceQuotaController); err != nil {
		return err
	}
	if err := s.SAController.ApplyTo(&c.ComponentConfig.SAController); err != nil {
		return err
	}
	if err := s.ServiceController.ApplyTo(&c.ComponentConfig.ServiceController); err != nil {
		return err
	}
	if err := s.TTLAfterFinishedController.ApplyTo(&c.ComponentConfig.TTLAfterFinishedController); err != nil {
		return err
	}
	if err := s.ValidatingAdmissionPolicyStatusController.ApplyTo(&c.ComponentConfig.ValidatingAdmissionPolicyStatusController); err != nil {
		return err
	}
	if err := s.SecureServing.ApplyTo(&c.SecureServing, &c.LoopbackClientConfig); err != nil {
		return err
	}
	if s.SecureServing.BindPort != 0 || s.SecureServing.Listener != nil {
		if err := s.Authentication.ApplyTo(&c.Authentication, c.SecureServing, nil); err != nil {
			return err
		}
		if err := s.Authorization.ApplyTo(&c.Authorization); err != nil {
			return err
		}
	}
	return nil
}

// Validate is used to validate the options and config before launching the controller manager
func (s *KubeControllerManagerOptions) Validate(allControllers []string, disabledByDefaultControllers []string, controllerAliases map[string]string) error {
	var errs []error

	if err := s.ComponentGlobalsRegistry.SetFallback(); err != nil {
		errs = append(errs, err)
	}

	errs = append(errs, s.ComponentGlobalsRegistry.Validate()...)
	errs = append(errs, s.Generic.Validate(allControllers, disabledByDefaultControllers, controllerAliases)...)
	errs = append(errs, s.KubeCloudShared.Validate()...)
	errs = append(errs, s.AttachDetachController.Validate()...)
	errs = append(errs, s.CSRSigningController.Validate()...)
	errs = append(errs, s.DaemonSetController.Validate()...)
	errs = append(errs, s.DeploymentController.Validate()...)
	errs = append(errs, s.StatefulSetController.Validate()...)
	errs = append(errs, s.DeprecatedFlags.Validate()...)
	errs = append(errs, s.EndpointController.Validate()...)
	errs = append(errs, s.EndpointSliceController.Validate()...)
	errs = append(errs, s.EndpointSliceMirroringController.Validate()...)
	errs = append(errs, s.EphemeralVolumeController.Validate()...)
	errs = append(errs, s.GarbageCollectorController.Validate()...)
	errs = append(errs, s.HPAController.Validate()...)
	errs = append(errs, s.JobController.Validate()...)
	errs = append(errs, s.CronJobController.Validate()...)
	errs = append(errs, s.LegacySATokenCleaner.Validate()...)
	errs = append(errs, s.NamespaceController.Validate()...)
	errs = append(errs, s.NodeIPAMController.Validate()...)
	errs = append(errs, s.NodeLifecycleController.Validate()...)
	errs = append(errs, s.PersistentVolumeBinderController.Validate()...)
	errs = append(errs, s.PodGCController.Validate()...)
	errs = append(errs, s.ReplicaSetController.Validate()...)
	errs = append(errs, s.ReplicationController.Validate()...)
	errs = append(errs, s.ResourceQuotaController.Validate()...)
	errs = append(errs, s.SAController.Validate()...)
	errs = append(errs, s.ServiceController.Validate()...)
	errs = append(errs, s.TTLAfterFinishedController.Validate()...)
	errs = append(errs, s.SecureServing.Validate()...)
	errs = append(errs, s.Authentication.Validate()...)
	errs = append(errs, s.Authorization.Validate()...)
	errs = append(errs, s.Metrics.Validate()...)

	// in-tree cloud providers are disabled since v1.31 (KEP-2395)
	if len(s.KubeCloudShared.CloudProvider.Name) > 0 && !cloudprovider.IsExternal(s.KubeCloudShared.CloudProvider.Name) {
		cloudprovider.DisableWarningForProvider(s.KubeCloudShared.CloudProvider.Name)
		errs = append(errs, cloudprovider.ErrorForDisabledProvider(s.KubeCloudShared.CloudProvider.Name))
	}

	if len(s.KubeCloudShared.CIDRAllocatorType) > 0 && s.KubeCloudShared.CIDRAllocatorType != string(ipam.RangeAllocatorType) {
		errs = append(errs, fmt.Errorf("built-in cloud providers are disabled. The ipam %s is not available", s.KubeCloudShared.CIDRAllocatorType))
	}

	// TODO: validate component config, master and kubeconfig

	return utilerrors.NewAggregate(errs)
}

// Config return a controller manager config objective
func (s KubeControllerManagerOptions) Config(allControllers []string, disabledByDefaultControllers []string, controllerAliases map[string]string) (*kubecontrollerconfig.Config, error) {
	if err := s.Validate(allControllers, disabledByDefaultControllers, controllerAliases); err != nil {
		return nil, err
	}

	if err := s.SecureServing.MaybeDefaultWithSelfSignedCerts("localhost", nil, []net.IP{netutils.ParseIPSloppy("127.0.0.1")}); err != nil {
		return nil, fmt.Errorf("error creating self-signed certificates: %v", err)
	}

	kubeconfig, err := clientcmd.BuildConfigFromFlags(s.Master, s.Generic.ClientConnection.Kubeconfig)
	if err != nil {
		return nil, err
	}
	kubeconfig.DisableCompression = true
	kubeconfig.ContentConfig.AcceptContentTypes = s.Generic.ClientConnection.AcceptContentTypes
	kubeconfig.ContentConfig.ContentType = s.Generic.ClientConnection.ContentType
	kubeconfig.QPS = s.Generic.ClientConnection.QPS
	kubeconfig.Burst = int(s.Generic.ClientConnection.Burst)

	client, err := clientset.NewForConfig(restclient.AddUserAgent(kubeconfig, KubeControllerManagerUserAgent))
	if err != nil {
		return nil, err
	}

	eventBroadcaster := record.NewBroadcaster()
	eventRecorder := eventBroadcaster.NewRecorder(clientgokubescheme.Scheme, v1.EventSource{Component: KubeControllerManagerUserAgent})

	c := &kubecontrollerconfig.Config{
		Client:                   client,
		Kubeconfig:               kubeconfig,
		EventBroadcaster:         eventBroadcaster,
		EventRecorder:            eventRecorder,
		ComponentGlobalsRegistry: s.ComponentGlobalsRegistry,
	}
	if err := s.ApplyTo(c, allControllers, disabledByDefaultControllers, controllerAliases); err != nil {
		return nil, err
	}
	s.Metrics.Apply()

	return c, nil
}
