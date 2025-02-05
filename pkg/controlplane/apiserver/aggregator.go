/*
Copyright 2024 The Kubernetes Authors.

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

package apiserver

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"

	apiextensionsinformers "k8s.io/apiextensions-apiserver/pkg/client/informers/externalversions/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apiserver/pkg/admission"
	genericfeatures "k8s.io/apiserver/pkg/features"
	genericapiserver "k8s.io/apiserver/pkg/server"
	"k8s.io/apiserver/pkg/server/healthz"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	utilpeerproxy "k8s.io/apiserver/pkg/util/peerproxy"
	kubeexternalinformers "k8s.io/client-go/informers"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
	v1 "k8s.io/kube-aggregator/pkg/apis/apiregistration/v1"
	v1helper "k8s.io/kube-aggregator/pkg/apis/apiregistration/v1/helper"
	"k8s.io/kube-aggregator/pkg/apis/apiregistration/v1beta1"
	aggregatorapiserver "k8s.io/kube-aggregator/pkg/apiserver"
	aggregatorscheme "k8s.io/kube-aggregator/pkg/apiserver/scheme"
	apiregistrationclient "k8s.io/kube-aggregator/pkg/client/clientset_generated/clientset/typed/apiregistration/v1"
	informers "k8s.io/kube-aggregator/pkg/client/informers/externalversions/apiregistration/v1"
	"k8s.io/kube-aggregator/pkg/controllers/autoregister"

	"k8s.io/kubernetes/pkg/controlplane/apiserver/options"
	"k8s.io/kubernetes/pkg/controlplane/controller/crdregistration"
)

func CreateAggregatorConfig(
	kubeAPIServerConfig genericapiserver.Config,
	commandOptions options.CompletedOptions,
	externalInformers kubeexternalinformers.SharedInformerFactory,
	serviceResolver aggregatorapiserver.ServiceResolver,
	proxyTransport *http.Transport,
	peerProxy utilpeerproxy.Interface,
	pluginInitializers []admission.PluginInitializer,
) (*aggregatorapiserver.Config, error) {
	// make a shallow copy to let us twiddle a few things
	// most of the config actually remains the same.  We only need to mess with a couple items related to the particulars of the aggregator
	genericConfig := kubeAPIServerConfig
	genericConfig.PostStartHooks = map[string]genericapiserver.PostStartHookConfigEntry{}
	genericConfig.RESTOptionsGetter = nil
	// prevent generic API server from installing the OpenAPI handler. Aggregator server
	// has its own customized OpenAPI handler.
	genericConfig.SkipOpenAPIInstallation = true

	if utilfeature.DefaultFeatureGate.Enabled(genericfeatures.StorageVersionAPI) &&
		utilfeature.DefaultFeatureGate.Enabled(genericfeatures.APIServerIdentity) {
		// Add StorageVersionPrecondition handler to aggregator-apiserver.
		// The handler will block write requests to built-in resources until the
		// target resources' storage versions are up-to-date.
		genericConfig.BuildHandlerChainFunc = genericapiserver.BuildHandlerChainWithStorageVersionPrecondition
	}

	if peerProxy != nil {
		originalHandlerChainBuilder := genericConfig.BuildHandlerChainFunc
		genericConfig.BuildHandlerChainFunc = func(apiHandler http.Handler, c *genericapiserver.Config) http.Handler {
			// Add peer proxy handler to aggregator-apiserver.
			// wrap the peer proxy handler first.
			apiHandler = peerProxy.WrapHandler(apiHandler)
			return originalHandlerChainBuilder(apiHandler, c)
		}
	}

	// copy the etcd options so we don't mutate originals.
	// we assume that the etcd options have been completed already.  avoid messing with anything outside
	// of changes to StorageConfig as that may lead to unexpected behavior when the options are applied.
	etcdOptions := *commandOptions.Etcd
	etcdOptions.StorageConfig.Codec = aggregatorscheme.Codecs.LegacyCodec(v1.SchemeGroupVersion, v1beta1.SchemeGroupVersion)
	etcdOptions.StorageConfig.EncodeVersioner = runtime.NewMultiGroupVersioner(v1.SchemeGroupVersion, schema.GroupKind{Group: v1beta1.GroupName})
	etcdOptions.SkipHealthEndpoints = true // avoid double wiring of health checks
	if err := etcdOptions.ApplyTo(&genericConfig); err != nil {
		return nil, err
	}

	// override MergedResourceConfig with aggregator defaults and registry
	if err := commandOptions.APIEnablement.ApplyTo(
		&genericConfig,
		aggregatorapiserver.DefaultAPIResourceConfigSource(),
		aggregatorscheme.Scheme); err != nil {
		return nil, err
	}

	aggregatorConfig := &aggregatorapiserver.Config{
		GenericConfig: &genericapiserver.RecommendedConfig{
			Config:                genericConfig,
			SharedInformerFactory: externalInformers,
		},
		ExtraConfig: aggregatorapiserver.ExtraConfig{
			ProxyClientCertFile:       commandOptions.ProxyClientCertFile,
			ProxyClientKeyFile:        commandOptions.ProxyClientKeyFile,
			PeerAdvertiseAddress:      commandOptions.PeerAdvertiseAddress,
			ServiceResolver:           serviceResolver,
			ProxyTransport:            proxyTransport,
			RejectForwardingRedirects: commandOptions.AggregatorRejectForwardingRedirects,
		},
	}

	// we need to clear the poststarthooks so we don't add them multiple times to all the servers (that fails)
	aggregatorConfig.GenericConfig.PostStartHooks = map[string]genericapiserver.PostStartHookConfigEntry{}

	return aggregatorConfig, nil
}

func CreateAggregatorServer(ctx context.Context, aggregatorConfig aggregatorapiserver.CompletedConfig, delegateAPIServer genericapiserver.DelegationTarget, crds apiextensionsinformers.CustomResourceDefinitionInformer, crdAPIEnabled bool, apiVersionPriorities map[schema.GroupVersion]APIServicePriority) (*aggregatorapiserver.APIAggregator, error) {
	aggregatorServer, err := aggregatorConfig.NewWithDelegate(ctx, delegateAPIServer)
	if err != nil {
		return nil, err
	}

	// create controllers for auto-registration
	apiRegistrationClient, err := apiregistrationclient.NewForConfig(aggregatorConfig.GenericConfig.LoopbackClientConfig)
	if err != nil {
		return nil, err
	}
	autoRegistrationController := autoregister.NewAutoRegisterController(aggregatorServer.APIRegistrationInformers.Apiregistration().V1().APIServices(), apiRegistrationClient)
	apiServices := apiServicesToRegister(delegateAPIServer, autoRegistrationController, apiVersionPriorities)

	type controller interface {
		Run(workers int, stopCh <-chan struct{})
		WaitForInitialSync()
	}
	var crdRegistrationController controller
	if crdAPIEnabled {
		crdRegistrationController = crdregistration.NewCRDRegistrationController(
			crds,
			autoRegistrationController)
	}

	// Imbue all builtin group-priorities onto the aggregated discovery
	if aggregatorConfig.GenericConfig.AggregatedDiscoveryGroupManager != nil {
		for gv, entry := range apiVersionPriorities {
			aggregatorConfig.GenericConfig.AggregatedDiscoveryGroupManager.SetGroupVersionPriority(metav1.GroupVersion(gv), int(entry.Group), int(entry.Version))
		}
	}

	err = aggregatorServer.GenericAPIServer.AddPostStartHook("kube-apiserver-autoregistration", func(context genericapiserver.PostStartHookContext) error {
		if crdAPIEnabled {
			go crdRegistrationController.Run(5, context.Done())
		}
		go func() {
			// let the CRD controller process the initial set of CRDs before starting the autoregistration controller.
			// this prevents the autoregistration controller's initial sync from deleting APIServices for CRDs that still exist.
			// we only need to do this if CRDs are enabled on this server.  We can't use discovery because we are the source for discovery.
			if crdAPIEnabled {
				klog.Infof("waiting for initial CRD sync...")
				crdRegistrationController.WaitForInitialSync()
				klog.Infof("initial CRD sync complete...")
			} else {
				klog.Infof("CRD API not enabled, starting APIService registration without waiting for initial CRD sync")
			}
			autoRegistrationController.Run(5, context.Done())
		}()
		return nil
	})
	if err != nil {
		return nil, err
	}

	err = aggregatorServer.GenericAPIServer.AddBootSequenceHealthChecks(
		makeAPIServiceAvailableHealthCheck(
			"autoregister-completion",
			apiServices,
			aggregatorServer.APIRegistrationInformers.Apiregistration().V1().APIServices(),
		),
	)
	if err != nil {
		return nil, err
	}

	return aggregatorServer, nil
}

func makeAPIService(gv schema.GroupVersion, apiVersionPriorities map[schema.GroupVersion]APIServicePriority) *v1.APIService {
	apiServicePriority, ok := apiVersionPriorities[gv]
	if !ok {
		// if we aren't found, then we shouldn't register ourselves because it could result in a CRD group version
		// being permanently stuck in the APIServices list.
		klog.Infof("Skipping APIService creation for %v", gv)
		return nil
	}
	return &v1.APIService{
		ObjectMeta: metav1.ObjectMeta{Name: gv.Version + "." + gv.Group},
		Spec: v1.APIServiceSpec{
			Group:                gv.Group,
			Version:              gv.Version,
			GroupPriorityMinimum: apiServicePriority.Group,
			VersionPriority:      apiServicePriority.Version,
		},
	}
}

// makeAPIServiceAvailableHealthCheck returns a healthz check that returns healthy
// once all of the specified services have been observed to be available at least once.
func makeAPIServiceAvailableHealthCheck(name string, apiServices []*v1.APIService, apiServiceInformer informers.APIServiceInformer) healthz.HealthChecker {
	// Track the auto-registered API services that have not been observed to be available yet
	pendingServiceNamesLock := &sync.RWMutex{}
	pendingServiceNames := sets.NewString()
	for _, service := range apiServices {
		pendingServiceNames.Insert(service.Name)
	}

	// When an APIService in the list is seen as available, remove it from the pending list
	handleAPIServiceChange := func(service *v1.APIService) {
		pendingServiceNamesLock.Lock()
		defer pendingServiceNamesLock.Unlock()
		if !pendingServiceNames.Has(service.Name) {
			return
		}
		if v1helper.IsAPIServiceConditionTrue(service, v1.Available) {
			pendingServiceNames.Delete(service.Name)
		}
	}

	// Watch add/update events for APIServices
	apiServiceInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{ //nolint:errcheck // no way to return error
		AddFunc:    func(obj interface{}) { handleAPIServiceChange(obj.(*v1.APIService)) },
		UpdateFunc: func(old, new interface{}) { handleAPIServiceChange(new.(*v1.APIService)) },
	})

	// Don't return healthy until the pending list is empty
	return healthz.NamedCheck(name, func(r *http.Request) error {
		pendingServiceNamesLock.RLock()
		defer pendingServiceNamesLock.RUnlock()
		if pendingServiceNames.Len() > 0 {
			return fmt.Errorf("missing APIService: %v", pendingServiceNames.List())
		}
		return nil
	})
}

// APIServicePriority defines group priority that is used in discovery. This controls
// group position in the kubectl output.
type APIServicePriority struct {
	// Group indicates the order of the group relative to other groups.
	Group int32
	// Version indicates the relative order of the Version inside of its group.
	Version int32
}

// DefaultGenericAPIServicePriorities returns the APIService priorities for generic APIs
func DefaultGenericAPIServicePriorities() map[schema.GroupVersion]APIServicePriority {
	// The proper way to resolve this letting the aggregator know the desired group and version-within-group order of the underlying servers
	// is to refactor the genericapiserver.DelegationTarget to include a list of priorities based on which APIs were installed.
	// This requires the APIGroupInfo struct to evolve and include the concept of priorities and to avoid mistakes, the core storage map there needs to be updated.
	// That ripples out every bit as far as you'd expect, so for 1.7 we'll include the list here instead of being built up during storage.
	return map[schema.GroupVersion]APIServicePriority{
		{Group: "", Version: "v1"}: {Group: 18000, Version: 1},
		// to my knowledge, nothing below here collides
		{Group: "events.k8s.io", Version: "v1"}:                      {Group: 17750, Version: 15},
		{Group: "events.k8s.io", Version: "v1beta1"}:                 {Group: 17750, Version: 5},
		{Group: "authentication.k8s.io", Version: "v1"}:              {Group: 17700, Version: 15},
		{Group: "authentication.k8s.io", Version: "v1beta1"}:         {Group: 17700, Version: 9},
		{Group: "authentication.k8s.io", Version: "v1alpha1"}:        {Group: 17700, Version: 1},
		{Group: "authorization.k8s.io", Version: "v1"}:               {Group: 17600, Version: 15},
		{Group: "certificates.k8s.io", Version: "v1"}:                {Group: 17300, Version: 15},
		{Group: "certificates.k8s.io", Version: "v1alpha1"}:          {Group: 17300, Version: 1},
		{Group: "rbac.authorization.k8s.io", Version: "v1"}:          {Group: 17000, Version: 15},
		{Group: "apiextensions.k8s.io", Version: "v1"}:               {Group: 16700, Version: 15},
		{Group: "admissionregistration.k8s.io", Version: "v1"}:       {Group: 16700, Version: 15},
		{Group: "admissionregistration.k8s.io", Version: "v1beta1"}:  {Group: 16700, Version: 12},
		{Group: "admissionregistration.k8s.io", Version: "v1alpha1"}: {Group: 16700, Version: 9},
		{Group: "coordination.k8s.io", Version: "v1"}:                {Group: 16500, Version: 15},
		{Group: "coordination.k8s.io", Version: "v1alpha2"}:          {Group: 16500, Version: 12},
		{Group: "discovery.k8s.io", Version: "v1"}:                   {Group: 16200, Version: 15},
		{Group: "discovery.k8s.io", Version: "v1beta1"}:              {Group: 16200, Version: 12},
		{Group: "flowcontrol.apiserver.k8s.io", Version: "v1"}:       {Group: 16100, Version: 21},
		{Group: "flowcontrol.apiserver.k8s.io", Version: "v1beta3"}:  {Group: 16100, Version: 18},
		{Group: "flowcontrol.apiserver.k8s.io", Version: "v1beta2"}:  {Group: 16100, Version: 15},
		{Group: "flowcontrol.apiserver.k8s.io", Version: "v1beta1"}:  {Group: 16100, Version: 12},
		{Group: "flowcontrol.apiserver.k8s.io", Version: "v1alpha1"}: {Group: 16100, Version: 9},
		{Group: "internal.apiserver.k8s.io", Version: "v1alpha1"}:    {Group: 16000, Version: 9},
		{Group: "resource.k8s.io", Version: "v1alpha3"}:              {Group: 15900, Version: 9},
		{Group: "storagemigration.k8s.io", Version: "v1alpha1"}:      {Group: 15800, Version: 9},
		// Append a new group to the end of the list if unsure.
		// You can use min(existing group)-100 as the initial value for a group.
		// Version can be set to 9 (to have space around) for a new group.
	}
}

func apiServicesToRegister(delegateAPIServer genericapiserver.DelegationTarget, registration autoregister.AutoAPIServiceRegistration, apiVersionPriorities map[schema.GroupVersion]APIServicePriority) []*v1.APIService {
	apiServices := []*v1.APIService{}

	for _, curr := range delegateAPIServer.ListedPaths() {
		if curr == "/api/v1" {
			apiService := makeAPIService(schema.GroupVersion{Group: "", Version: "v1"}, apiVersionPriorities)
			registration.AddAPIServiceToSyncOnStart(apiService)
			apiServices = append(apiServices, apiService)
			continue
		}

		if !strings.HasPrefix(curr, "/apis/") {
			continue
		}
		// this comes back in a list that looks like /apis/rbac.authorization.k8s.io/v1alpha1
		tokens := strings.Split(curr, "/")
		if len(tokens) != 4 {
			continue
		}

		apiService := makeAPIService(schema.GroupVersion{Group: tokens[2], Version: tokens[3]}, apiVersionPriorities)
		if apiService == nil {
			continue
		}
		registration.AddAPIServiceToSyncOnStart(apiService)
		apiServices = append(apiServices, apiService)
	}

	return apiServices
}
