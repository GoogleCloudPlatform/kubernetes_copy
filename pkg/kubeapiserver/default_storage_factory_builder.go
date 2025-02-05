/*
Copyright 2016 The Kubernetes Authors.

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

package kubeapiserver

import (
	"strings"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	serveroptions "k8s.io/apiserver/pkg/server/options"
	"k8s.io/apiserver/pkg/server/resourceconfig"
	serverstorage "k8s.io/apiserver/pkg/server/storage"
	"k8s.io/apiserver/pkg/storage/storagebackend"
	"k8s.io/apiserver/pkg/util/compatibility"
	basecompatibility "k8s.io/component-base/compatibility"
	"k8s.io/kubernetes/pkg/api/legacyscheme"
	"k8s.io/kubernetes/pkg/apis/admissionregistration"
	"k8s.io/kubernetes/pkg/apis/apps"
	"k8s.io/kubernetes/pkg/apis/certificates"
	"k8s.io/kubernetes/pkg/apis/coordination"
	api "k8s.io/kubernetes/pkg/apis/core"
	"k8s.io/kubernetes/pkg/apis/events"
	"k8s.io/kubernetes/pkg/apis/extensions"
	"k8s.io/kubernetes/pkg/apis/networking"
	"k8s.io/kubernetes/pkg/apis/policy"
	"k8s.io/kubernetes/pkg/apis/storage"
	"k8s.io/kubernetes/pkg/apis/storagemigration"
)

// SpecialDefaultResourcePrefixes are prefixes compiled into Kubernetes.
var SpecialDefaultResourcePrefixes = map[schema.GroupResource]string{
	{Group: "", Resource: "replicationcontrollers"}:     "controllers",
	{Group: "", Resource: "endpoints"}:                  "services/endpoints",
	{Group: "", Resource: "nodes"}:                      "minions",
	{Group: "", Resource: "services"}:                   "services/specs",
	{Group: "extensions", Resource: "ingresses"}:        "ingress",
	{Group: "networking.k8s.io", Resource: "ingresses"}: "ingress",
}

// DefaultWatchCacheSizes defines default resources for which watchcache
// should be disabled.
func DefaultWatchCacheSizes() map[schema.GroupResource]int {
	return map[schema.GroupResource]int{
		{Resource: "events"}:                         0,
		{Group: "events.k8s.io", Resource: "events"}: 0,
	}
}

// NewStorageFactoryConfig returns a new StorageFactoryConfig set up with necessary resource overrides.
func NewStorageFactoryConfig() *StorageFactoryConfig {
	return NewStorageFactoryConfigEffectiveVersion(compatibility.DefaultComponentGlobalsRegistry.EffectiveVersionFor(basecompatibility.DefaultKubeComponent))
}

// NewStorageFactoryConfigEffectiveVersion returns a new StorageFactoryConfig set up with necessary resource overrides for a given EffectiveVersion.
func NewStorageFactoryConfigEffectiveVersion(effectiveVersion basecompatibility.EffectiveVersion) *StorageFactoryConfig {
	resources := []schema.GroupVersionResource{
		// If a resource has to be stored in a version that is not the
		// latest, then it can be listed here. Usually this is the case
		// when a new version for a resource gets introduced and a
		// downgrade to an older apiserver that doesn't know the new
		// version still needs to be supported for one release.
		//
		// Example from Kubernetes 1.24 where csistoragecapacities had just
		// graduated to GA:
		//
		// TODO (https://github.com/kubernetes/kubernetes/issues/108451): remove the override in 1.25.
		// apisstorage.Resource("csistoragecapacities").WithVersion("v1beta1"),
		coordination.Resource("leasecandidates").WithVersion("v1alpha2"),
		// TODO(aojea) ipaddresses and servicecidrs are v1 in 1.33
		// remove them in 1.34 when all apiserver understand the v1 version.
		networking.Resource("ipaddresses").WithVersion("v1beta1"),
		networking.Resource("servicecidrs").WithVersion("v1beta1"),
		admissionregistration.Resource("mutatingadmissionpolicies").WithVersion("v1alpha1"),
		admissionregistration.Resource("mutatingadmissionpolicybindings").WithVersion("v1alpha1"),
		certificates.Resource("clustertrustbundles").WithVersion("v1alpha1"),
		storage.Resource("volumeattributesclasses").WithVersion("v1beta1"),
		storagemigration.Resource("storagemigrations").WithVersion("v1alpha1"),
	}

	return &StorageFactoryConfig{
		Serializer:                legacyscheme.Codecs,
		DefaultResourceEncoding:   serverstorage.NewDefaultResourceEncodingConfigForEffectiveVersion(legacyscheme.Scheme, effectiveVersion),
		ResourceEncodingOverrides: resources,
	}
}

// StorageFactoryConfig is a configuration for creating storage factory.
type StorageFactoryConfig struct {
	StorageConfig             storagebackend.Config
	APIResourceConfig         *serverstorage.ResourceConfig
	DefaultResourceEncoding   *serverstorage.DefaultResourceEncodingConfig
	DefaultStorageMediaType   string
	Serializer                runtime.StorageSerializer
	ResourceEncodingOverrides []schema.GroupVersionResource
	EtcdServersOverrides      []string
}

// Complete completes the StorageFactoryConfig with provided etcdOptions returning completedStorageFactoryConfig.
// This method mutates the receiver (StorageFactoryConfig).  It must never mutate the inputs.
func (c *StorageFactoryConfig) Complete(etcdOptions *serveroptions.EtcdOptions) *completedStorageFactoryConfig {
	c.StorageConfig = etcdOptions.StorageConfig
	c.DefaultStorageMediaType = etcdOptions.DefaultStorageMediaType
	c.EtcdServersOverrides = etcdOptions.EtcdServersOverrides
	return &completedStorageFactoryConfig{c}
}

// completedStorageFactoryConfig is a wrapper around StorageFactoryConfig completed with etcd options.
//
// Note: this struct is intentionally unexported so that it can only be constructed via a StorageFactoryConfig.Complete
// call. The implied consequence is that this does not comply with golint.
type completedStorageFactoryConfig struct {
	*StorageFactoryConfig
}

// New returns a new storage factory created from the completed storage factory configuration.
func (c *completedStorageFactoryConfig) New() (*serverstorage.DefaultStorageFactory, error) {
	resourceEncodingConfig := resourceconfig.MergeResourceEncodingConfigs(c.DefaultResourceEncoding, c.ResourceEncodingOverrides)
	storageFactory := serverstorage.NewDefaultStorageFactory(
		c.StorageConfig,
		c.DefaultStorageMediaType,
		c.Serializer,
		resourceEncodingConfig,
		c.APIResourceConfig,
		SpecialDefaultResourcePrefixes)

	storageFactory.AddCohabitatingResources(networking.Resource("networkpolicies"), extensions.Resource("networkpolicies"))
	storageFactory.AddCohabitatingResources(apps.Resource("deployments"), extensions.Resource("deployments"))
	storageFactory.AddCohabitatingResources(apps.Resource("daemonsets"), extensions.Resource("daemonsets"))
	storageFactory.AddCohabitatingResources(apps.Resource("replicasets"), extensions.Resource("replicasets"))
	storageFactory.AddCohabitatingResources(api.Resource("events"), events.Resource("events"))
	storageFactory.AddCohabitatingResources(api.Resource("replicationcontrollers"), extensions.Resource("replicationcontrollers")) // to make scale subresources equivalent
	storageFactory.AddCohabitatingResources(policy.Resource("podsecuritypolicies"), extensions.Resource("podsecuritypolicies"))
	storageFactory.AddCohabitatingResources(networking.Resource("ingresses"), extensions.Resource("ingresses"))

	for _, override := range c.EtcdServersOverrides {
		tokens := strings.Split(override, "#")
		apiresource := strings.Split(tokens[0], "/")

		group := apiresource[0]
		resource := apiresource[1]
		groupResource := schema.GroupResource{Group: group, Resource: resource}

		servers := strings.Split(tokens[1], ";")
		storageFactory.SetEtcdLocation(groupResource, servers)
	}
	return storageFactory, nil
}
