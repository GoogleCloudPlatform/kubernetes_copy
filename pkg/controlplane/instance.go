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

package controlplane

import (
	"fmt"
	"net"
	"reflect"
	"strconv"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	admissionregistrationv1alpha1 "k8s.io/api/admissionregistration/v1alpha1"
	admissionregistrationv1beta1 "k8s.io/api/admissionregistration/v1beta1"
	apiserverinternalv1alpha1 "k8s.io/api/apiserverinternal/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	authenticationv1alpha1 "k8s.io/api/authentication/v1alpha1"
	authenticationv1beta1 "k8s.io/api/authentication/v1beta1"
	authorizationapiv1 "k8s.io/api/authorization/v1"
	autoscalingapiv1 "k8s.io/api/autoscaling/v1"
	autoscalingapiv2 "k8s.io/api/autoscaling/v2"
	batchapiv1 "k8s.io/api/batch/v1"
	certificatesapiv1 "k8s.io/api/certificates/v1"
	certificatesv1alpha1 "k8s.io/api/certificates/v1alpha1"
	certificatesv1beta1 "k8s.io/api/certificates/v1beta1"
	coordinationapiv1 "k8s.io/api/coordination/v1"
	coordinationv1alpha2 "k8s.io/api/coordination/v1alpha2"
	apiv1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	eventsv1 "k8s.io/api/events/v1"
	networkingapiv1 "k8s.io/api/networking/v1"
	networkingapiv1alpha1 "k8s.io/api/networking/v1alpha1"
	networkingapiv1beta1 "k8s.io/api/networking/v1beta1"
	nodev1 "k8s.io/api/node/v1"
	policyapiv1 "k8s.io/api/policy/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	resourcev1alpha3 "k8s.io/api/resource/v1alpha3"
	resourcev1beta1 "k8s.io/api/resource/v1beta1"
	schedulingapiv1 "k8s.io/api/scheduling/v1"
	storageapiv1 "k8s.io/api/storage/v1"
	storageapiv1alpha1 "k8s.io/api/storage/v1alpha1"
	storageapiv1beta1 "k8s.io/api/storage/v1beta1"
	svmv1alpha1 "k8s.io/api/storagemigration/v1alpha1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilnet "k8s.io/apimachinery/pkg/util/net"
	"k8s.io/apiserver/pkg/endpoints/discovery"
	genericapiserver "k8s.io/apiserver/pkg/server"
	serverstorage "k8s.io/apiserver/pkg/server/storage"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	clientdiscovery "k8s.io/client-go/discovery"
	"k8s.io/client-go/kubernetes"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	discoveryclient "k8s.io/client-go/kubernetes/typed/discovery/v1"
	"k8s.io/klog/v2"
	api "k8s.io/kubernetes/pkg/apis/core"
	flowcontrolv1 "k8s.io/kubernetes/pkg/apis/flowcontrol/v1"
	flowcontrolv1beta1 "k8s.io/kubernetes/pkg/apis/flowcontrol/v1beta1"
	flowcontrolv1beta2 "k8s.io/kubernetes/pkg/apis/flowcontrol/v1beta2"
	flowcontrolv1beta3 "k8s.io/kubernetes/pkg/apis/flowcontrol/v1beta3"
	controlplaneapiserver "k8s.io/kubernetes/pkg/controlplane/apiserver"
	"k8s.io/kubernetes/pkg/controlplane/apiserver/options"
	"k8s.io/kubernetes/pkg/controlplane/controller/defaultservicecidr"
	"k8s.io/kubernetes/pkg/controlplane/controller/kubernetesservice"
	"k8s.io/kubernetes/pkg/controlplane/reconcilers"
	"k8s.io/kubernetes/pkg/features"
	kubeoptions "k8s.io/kubernetes/pkg/kubeapiserver/options"
	kubeletclient "k8s.io/kubernetes/pkg/kubelet/client"

	// RESTStorage installers
	admissionregistrationrest "k8s.io/kubernetes/pkg/registry/admissionregistration/rest"
	apiserverinternalrest "k8s.io/kubernetes/pkg/registry/apiserverinternal/rest"
	appsrest "k8s.io/kubernetes/pkg/registry/apps/rest"
	authenticationrest "k8s.io/kubernetes/pkg/registry/authentication/rest"
	authorizationrest "k8s.io/kubernetes/pkg/registry/authorization/rest"
	autoscalingrest "k8s.io/kubernetes/pkg/registry/autoscaling/rest"
	batchrest "k8s.io/kubernetes/pkg/registry/batch/rest"
	certificatesrest "k8s.io/kubernetes/pkg/registry/certificates/rest"
	coordinationrest "k8s.io/kubernetes/pkg/registry/coordination/rest"
	corerest "k8s.io/kubernetes/pkg/registry/core/rest"
	discoveryrest "k8s.io/kubernetes/pkg/registry/discovery/rest"
	eventsrest "k8s.io/kubernetes/pkg/registry/events/rest"
	flowcontrolrest "k8s.io/kubernetes/pkg/registry/flowcontrol/rest"
	networkingrest "k8s.io/kubernetes/pkg/registry/networking/rest"
	noderest "k8s.io/kubernetes/pkg/registry/node/rest"
	policyrest "k8s.io/kubernetes/pkg/registry/policy/rest"
	rbacrest "k8s.io/kubernetes/pkg/registry/rbac/rest"
	resourcerest "k8s.io/kubernetes/pkg/registry/resource/rest"
	schedulingrest "k8s.io/kubernetes/pkg/registry/scheduling/rest"
	storagerest "k8s.io/kubernetes/pkg/registry/storage/rest"
	svmrest "k8s.io/kubernetes/pkg/registry/storagemigration/rest"
)

const (
	// DefaultEndpointReconcilerInterval is the default amount of time for how often the endpoints for
	// the kubernetes Service are reconciled.
	DefaultEndpointReconcilerInterval = 10 * time.Second
	// DefaultEndpointReconcilerTTL is the default TTL timeout for the storage layer
	DefaultEndpointReconcilerTTL = 15 * time.Second
	// IdentityLeaseComponentLabelKey is used to apply a component label to identity lease objects, indicating:
	//   1. the lease is an identity lease (different from leader election leases)
	//   2. which component owns this lease
	// TODO(sttts): remove this indirection
	IdentityLeaseComponentLabelKey = controlplaneapiserver.IdentityLeaseComponentLabelKey
	// KubeAPIServer defines variable used internally when referring to kube-apiserver component
	KubeAPIServer = "kube-apiserver"
	// repairLoopInterval defines the interval used to run the Services ClusterIP and NodePort repair loops
	repairLoopInterval = 3 * time.Minute
)

// Extra defines extra configuration for kube-apiserver
type Extra struct {
	EndpointReconcilerConfig EndpointReconcilerConfig
	KubeletClientConfig      kubeletclient.KubeletClientConfig

	// Values to build the IP addresses used by discovery
	// The range of IPs to be assigned to services with type=ClusterIP or greater
	ServiceIPRange net.IPNet
	// The IP address for the GenericAPIServer service (must be inside ServiceIPRange)
	APIServerServiceIP net.IP

	// dual stack services, the range represents an alternative IP range for service IP
	// must be of different family than primary (ServiceIPRange)
	SecondaryServiceIPRange net.IPNet
	// the secondary IP address the GenericAPIServer service (must be inside SecondaryServiceIPRange)
	SecondaryAPIServerServiceIP net.IP

	// Port for the apiserver service.
	APIServerServicePort int

	// TODO, we can probably group service related items into a substruct to make it easier to configure
	// the API server items and `Extra*` fields likely fit nicely together.

	// The range of ports to be assigned to services with type=NodePort or greater
	ServiceNodePortRange utilnet.PortRange
	// If non-zero, the "kubernetes" services uses this port as NodePort.
	KubernetesServiceNodePort int

	// Number of masters running; all masters must be started with the
	// same value for this field. (Numbers > 1 currently untested.)
	MasterCount int

	// MasterEndpointReconcileTTL sets the time to live in seconds of an
	// endpoint record recorded by each master. The endpoints are checked at an
	// interval that is 2/3 of this value and this value defaults to 15s if
	// unset. In very large clusters, this value may be increased to reduce the
	// possibility that the master endpoint record expires (due to other load
	// on the etcd server) and causes masters to drop in and out of the
	// kubernetes service record. It is not recommended to set this value below
	// 15s.
	MasterEndpointReconcileTTL time.Duration

	// Selects which reconciler to use
	EndpointReconcilerType reconcilers.Type

	// RepairServicesInterval interval used by the repair loops for
	// the Services NodePort and ClusterIP resources
	RepairServicesInterval time.Duration
}

// Config defines configuration for the master
type Config struct {
	ControlPlane controlplaneapiserver.Config
	Extra
}

type completedConfig struct {
	ControlPlane controlplaneapiserver.CompletedConfig
	*Extra
}

// CompletedConfig embeds a private pointer that cannot be instantiated outside of this package
type CompletedConfig struct {
	*completedConfig
}

// EndpointReconcilerConfig holds the endpoint reconciler and endpoint reconciliation interval to be
// used by the master.
type EndpointReconcilerConfig struct {
	Reconciler reconcilers.EndpointReconciler
	Interval   time.Duration
}

// Instance contains state for a Kubernetes cluster api server instance.
type Instance struct {
	ControlPlane *controlplaneapiserver.Server
}

func (c *Config) createMasterCountReconciler() reconcilers.EndpointReconciler {
	endpointClient := corev1client.NewForConfigOrDie(c.ControlPlane.Generic.LoopbackClientConfig)
	endpointSliceClient := discoveryclient.NewForConfigOrDie(c.ControlPlane.Generic.LoopbackClientConfig)
	endpointsAdapter := reconcilers.NewEndpointsAdapter(endpointClient, endpointSliceClient)

	return reconcilers.NewMasterCountEndpointReconciler(c.Extra.MasterCount, endpointsAdapter)
}

func (c *Config) createNoneReconciler() reconcilers.EndpointReconciler {
	return reconcilers.NewNoneEndpointReconciler()
}

func (c *Config) createLeaseReconciler() reconcilers.EndpointReconciler {
	endpointClient := corev1client.NewForConfigOrDie(c.ControlPlane.Generic.LoopbackClientConfig)
	endpointSliceClient := discoveryclient.NewForConfigOrDie(c.ControlPlane.Generic.LoopbackClientConfig)
	endpointsAdapter := reconcilers.NewEndpointsAdapter(endpointClient, endpointSliceClient)

	ttl := c.Extra.MasterEndpointReconcileTTL
	config, err := c.ControlPlane.StorageFactory.NewConfig(api.Resource("apiServerIPInfo"), &api.Endpoints{})
	if err != nil {
		klog.Fatalf("Error creating storage factory config: %v", err)
	}
	masterLeases, err := reconcilers.NewLeases(config, "/masterleases/", ttl)
	if err != nil {
		klog.Fatalf("Error creating leases: %v", err)
	}

	return reconcilers.NewLeaseEndpointReconciler(endpointsAdapter, masterLeases)
}

func (c *Config) createEndpointReconciler() reconcilers.EndpointReconciler {
	klog.Infof("Using reconciler: %v", c.Extra.EndpointReconcilerType)
	switch c.Extra.EndpointReconcilerType {
	// there are numerous test dependencies that depend on a default controller
	case reconcilers.MasterCountReconcilerType:
		return c.createMasterCountReconciler()
	case "", reconcilers.LeaseEndpointReconcilerType:
		return c.createLeaseReconciler()
	case reconcilers.NoneEndpointReconcilerType:
		return c.createNoneReconciler()
	default:
		klog.Fatalf("Reconciler not implemented: %v", c.Extra.EndpointReconcilerType)
	}
	return nil
}

// Complete fills in any fields not set that are required to have valid data. It's mutating the receiver.
func (c *Config) Complete() CompletedConfig {
	if c.ControlPlane.PeerEndpointReconcileInterval == 0 && c.EndpointReconcilerConfig.Interval != 0 {
		// default this to the endpoint reconciler value before the generic
		// controlplane completion can kick in
		c.ControlPlane.PeerEndpointReconcileInterval = c.EndpointReconcilerConfig.Interval
	}

	cfg := completedConfig{
		c.ControlPlane.Complete(),
		&c.Extra,
	}

	serviceIPRange, apiServerServiceIP, err := options.ServiceIPRange(cfg.Extra.ServiceIPRange)
	if err != nil {
		klog.Fatalf("Error determining service IP ranges: %v", err)
	}
	if cfg.Extra.ServiceIPRange.IP == nil {
		cfg.Extra.ServiceIPRange = serviceIPRange
	}
	if cfg.Extra.APIServerServiceIP == nil {
		cfg.Extra.APIServerServiceIP = apiServerServiceIP
	}

	// override the default discovery addresses in the generic controlplane adding service IP support
	discoveryAddresses := discovery.DefaultAddresses{DefaultAddress: cfg.ControlPlane.Generic.ExternalAddress}
	discoveryAddresses.CIDRRules = append(discoveryAddresses.CIDRRules,
		discovery.CIDRRule{IPRange: cfg.Extra.ServiceIPRange, Address: net.JoinHostPort(cfg.Extra.APIServerServiceIP.String(), strconv.Itoa(cfg.Extra.APIServerServicePort))})
	cfg.ControlPlane.Generic.DiscoveryAddresses = discoveryAddresses

	if cfg.Extra.ServiceNodePortRange.Size == 0 {
		// TODO: Currently no way to specify an empty range (do we need to allow this?)
		// We should probably allow this for clouds that don't require NodePort to do load-balancing (GCE)
		// but then that breaks the strict nestedness of ServiceType.
		// Review post-v1
		cfg.Extra.ServiceNodePortRange = kubeoptions.DefaultServiceNodePortRange
		klog.Infof("Node port range unspecified. Defaulting to %v.", cfg.Extra.ServiceNodePortRange)
	}

	if cfg.Extra.EndpointReconcilerConfig.Interval == 0 {
		cfg.Extra.EndpointReconcilerConfig.Interval = DefaultEndpointReconcilerInterval
	}

	if cfg.Extra.MasterEndpointReconcileTTL == 0 {
		cfg.Extra.MasterEndpointReconcileTTL = DefaultEndpointReconcilerTTL
	}

	if cfg.Extra.EndpointReconcilerConfig.Reconciler == nil {
		cfg.Extra.EndpointReconcilerConfig.Reconciler = c.createEndpointReconciler()
	}

	if cfg.Extra.RepairServicesInterval == 0 {
		cfg.Extra.RepairServicesInterval = repairLoopInterval
	}

	return CompletedConfig{&cfg}
}

// New returns a new instance of Master from the given config.
// Certain config fields will be set to a default value if unset.
// Certain config fields must be specified, including:
// KubeletClientConfig
func (c CompletedConfig) New(delegationTarget genericapiserver.DelegationTarget) (*Instance, error) {
	if reflect.DeepEqual(c.Extra.KubeletClientConfig, kubeletclient.KubeletClientConfig{}) {
		return nil, fmt.Errorf("Master.New() called with empty config.KubeletClientConfig")
	}

	cp, err := c.ControlPlane.New(KubeAPIServer, delegationTarget)
	if err != nil {
		return nil, err
	}

	s := &Instance{
		ControlPlane: cp,
	}

	client, err := kubernetes.NewForConfig(c.ControlPlane.Generic.LoopbackClientConfig)
	if err != nil {
		return nil, err
	}

	restStorageProviders, err := c.StorageProviders(client.Discovery())
	if err != nil {
		return nil, err
	}

	if err := s.ControlPlane.InstallAPIs(restStorageProviders...); err != nil {
		return nil, err
	}

	_, publicServicePort, err := c.ControlPlane.Generic.SecureServing.HostPort()
	if err != nil {
		return nil, fmt.Errorf("failed to get listener address: %w", err)
	}
	kubernetesServiceCtrl := kubernetesservice.New(kubernetesservice.Config{
		PublicIP: c.ControlPlane.Generic.PublicAddress,

		EndpointReconciler: c.Extra.EndpointReconcilerConfig.Reconciler,
		EndpointInterval:   c.Extra.EndpointReconcilerConfig.Interval,

		ServiceIP:                 c.Extra.APIServerServiceIP,
		ServicePort:               c.Extra.APIServerServicePort,
		PublicServicePort:         publicServicePort,
		KubernetesServiceNodePort: c.Extra.KubernetesServiceNodePort,
	}, client, c.ControlPlane.Extra.VersionedInformers.Core().V1().Services())
	s.ControlPlane.GenericAPIServer.AddPostStartHookOrDie("bootstrap-controller", func(hookContext genericapiserver.PostStartHookContext) error {
		kubernetesServiceCtrl.Start(hookContext.Done())
		return nil
	})
	s.ControlPlane.GenericAPIServer.AddPreShutdownHookOrDie("stop-kubernetes-service-controller", func() error {
		kubernetesServiceCtrl.Stop()
		return nil
	})

	if utilfeature.DefaultFeatureGate.Enabled(features.MultiCIDRServiceAllocator) {
		s.ControlPlane.GenericAPIServer.AddPostStartHookOrDie("start-kubernetes-service-cidr-controller", func(hookContext genericapiserver.PostStartHookContext) error {
			controller := defaultservicecidr.NewController(
				c.Extra.ServiceIPRange,
				c.Extra.SecondaryServiceIPRange,
				client,
			)
			// The default serviceCIDR must exist before the apiserver is healthy
			// otherwise the allocators for Services will not work.
			controller.Start(hookContext)
			return nil
		})
	}

	return s, nil

}

func (c CompletedConfig) StorageProviders(discovery clientdiscovery.DiscoveryInterface) ([]controlplaneapiserver.RESTStorageProvider, error) {
	legacyRESTStorageProvider, err := corerest.New(corerest.Config{
		GenericConfig: *c.ControlPlane.NewCoreGenericConfig(),
		Proxy: corerest.ProxyConfig{
			Transport:           c.ControlPlane.Extra.ProxyTransport,
			KubeletClientConfig: c.Extra.KubeletClientConfig,
		},
		Services: corerest.ServicesConfig{
			ClusterIPRange:          c.Extra.ServiceIPRange,
			SecondaryClusterIPRange: c.Extra.SecondaryServiceIPRange,
			NodePortRange:           c.Extra.ServiceNodePortRange,
			IPRepairInterval:        c.Extra.RepairServicesInterval,
		},
	})
	if err != nil {
		return nil, err
	}

	// The order here is preserved in discovery.
	// If resources with identical names exist in more than one of these groups (e.g. "deployments.apps"" and "deployments.extensions"),
	// the order of this list determines which group an unqualified resource name (e.g. "deployments") should prefer.
	// This priority order is used for local discovery, but it ends up aggregated in `k8s.io/kubernetes/cmd/kube-apiserver/app/aggregator.go
	// with specific priorities.
	// TODO: describe the priority all the way down in the RESTStorageProviders and plumb it back through the various discovery
	// handlers that we have.
	return []controlplaneapiserver.RESTStorageProvider{
		legacyRESTStorageProvider,
		apiserverinternalrest.StorageProvider{},
		authenticationrest.RESTStorageProvider{Authenticator: c.ControlPlane.Generic.Authentication.Authenticator, APIAudiences: c.ControlPlane.Generic.Authentication.APIAudiences},
		authorizationrest.RESTStorageProvider{Authorizer: c.ControlPlane.Generic.Authorization.Authorizer, RuleResolver: c.ControlPlane.Generic.RuleResolver},
		autoscalingrest.RESTStorageProvider{},
		batchrest.RESTStorageProvider{},
		certificatesrest.RESTStorageProvider{},
		coordinationrest.RESTStorageProvider{},
		discoveryrest.StorageProvider{},
		networkingrest.RESTStorageProvider{},
		noderest.RESTStorageProvider{},
		policyrest.RESTStorageProvider{},
		rbacrest.RESTStorageProvider{Authorizer: c.ControlPlane.Generic.Authorization.Authorizer},
		schedulingrest.RESTStorageProvider{},
		storagerest.RESTStorageProvider{},
		svmrest.RESTStorageProvider{},
		flowcontrolrest.RESTStorageProvider{InformerFactory: c.ControlPlane.Generic.SharedInformerFactory},
		// keep apps after extensions so legacy clients resolve the extensions versions of shared resource names.
		// See https://github.com/kubernetes/kubernetes/issues/42392
		appsrest.StorageProvider{},
		admissionregistrationrest.RESTStorageProvider{Authorizer: c.ControlPlane.Generic.Authorization.Authorizer, DiscoveryClient: discovery},
		eventsrest.RESTStorageProvider{TTL: c.ControlPlane.EventTTL},
		resourcerest.RESTStorageProvider{},
	}, nil
}

var (
	// stableAPIGroupVersionsEnabledByDefault is a list of our stable versions.
	stableAPIGroupVersionsEnabledByDefault = []schema.GroupVersion{
		admissionregistrationv1.SchemeGroupVersion,
		apiv1.SchemeGroupVersion,
		appsv1.SchemeGroupVersion,
		authenticationv1.SchemeGroupVersion,
		authorizationapiv1.SchemeGroupVersion,
		autoscalingapiv1.SchemeGroupVersion,
		autoscalingapiv2.SchemeGroupVersion,
		batchapiv1.SchemeGroupVersion,
		certificatesapiv1.SchemeGroupVersion,
		coordinationapiv1.SchemeGroupVersion,
		discoveryv1.SchemeGroupVersion,
		eventsv1.SchemeGroupVersion,
		networkingapiv1.SchemeGroupVersion,
		nodev1.SchemeGroupVersion,
		policyapiv1.SchemeGroupVersion,
		rbacv1.SchemeGroupVersion,
		storageapiv1.SchemeGroupVersion,
		schedulingapiv1.SchemeGroupVersion,
		flowcontrolv1.SchemeGroupVersion,
	}

	// betaAPIGroupVersionsDisabledByDefault is for all future beta groupVersions.
	betaAPIGroupVersionsDisabledByDefault = []schema.GroupVersion{
		admissionregistrationv1beta1.SchemeGroupVersion,
		authenticationv1beta1.SchemeGroupVersion,
		certificatesv1beta1.SchemeGroupVersion,
		storageapiv1beta1.SchemeGroupVersion,
		flowcontrolv1beta1.SchemeGroupVersion,
		flowcontrolv1beta2.SchemeGroupVersion,
		flowcontrolv1beta3.SchemeGroupVersion,
		networkingapiv1beta1.SchemeGroupVersion,
		resourcev1beta1.SchemeGroupVersion,
	}

	// alphaAPIGroupVersionsDisabledByDefault holds the alpha APIs we have.  They are always disabled by default.
	alphaAPIGroupVersionsDisabledByDefault = []schema.GroupVersion{
		admissionregistrationv1alpha1.SchemeGroupVersion,
		apiserverinternalv1alpha1.SchemeGroupVersion,
		authenticationv1alpha1.SchemeGroupVersion,
		apiserverinternalv1alpha1.SchemeGroupVersion,
		coordinationv1alpha2.SchemeGroupVersion,
		resourcev1alpha3.SchemeGroupVersion,
		certificatesv1alpha1.SchemeGroupVersion,
		networkingapiv1alpha1.SchemeGroupVersion,
		storageapiv1alpha1.SchemeGroupVersion,
		svmv1alpha1.SchemeGroupVersion,
	}
)

// DefaultAPIResourceConfigSource returns default configuration for an APIResource.
func DefaultAPIResourceConfigSource() *serverstorage.ResourceConfig {
	ret := serverstorage.NewResourceConfig()
	// NOTE: GroupVersions listed here will be enabled by default. Don't put alpha or beta versions in the list.
	ret.EnableVersions(stableAPIGroupVersionsEnabledByDefault...)

	// disable alpha and beta versions explicitly so we have a full list of what's possible to serve
	ret.DisableVersions(betaAPIGroupVersionsDisabledByDefault...)
	ret.DisableVersions(alphaAPIGroupVersionsDisabledByDefault...)

	return ret
}
