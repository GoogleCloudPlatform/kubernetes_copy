/*
Copyright 2023 The Kubernetes Authors.

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

package server

import (
	apiextensionsapiserver "k8s.io/apiextensions-apiserver/pkg/apiserver"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apiserver/pkg/util/webhook"
	aggregatorapiserver "k8s.io/kube-aggregator/pkg/apiserver"
	aggregatorscheme "k8s.io/kube-aggregator/pkg/apiserver/scheme"
	"k8s.io/kubernetes/pkg/controlplane"

	"k8s.io/kubernetes/pkg/api/legacyscheme"
	controlplaneapiserver "k8s.io/kubernetes/pkg/controlplane/apiserver"
	"k8s.io/kubernetes/pkg/controlplane/apiserver/options"
	generatedopenapi "k8s.io/kubernetes/pkg/generated/openapi"
)

type Config struct {
	Options options.CompletedOptions

	Aggregator    *aggregatorapiserver.Config
	ControlPlane  *controlplaneapiserver.Config
	APIExtensions *apiextensionsapiserver.Config

	ExtraConfig
}

type ExtraConfig struct {
}

type completedConfig struct {
	Options options.CompletedOptions

	Aggregator    aggregatorapiserver.CompletedConfig
	ControlPlane  controlplaneapiserver.CompletedConfig
	APIExtensions apiextensionsapiserver.CompletedConfig

	ExtraConfig
}

type CompletedConfig struct {
	// Embed a private pointer that cannot be instantiated outside of this package.
	*completedConfig
}

func (c *Config) Complete() (CompletedConfig, error) {
	return CompletedConfig{&completedConfig{
		Options: c.Options,

		Aggregator:    c.Aggregator.Complete(),
		ControlPlane:  c.ControlPlane.Complete(),
		APIExtensions: c.APIExtensions.Complete(),

		ExtraConfig: c.ExtraConfig,
	}}, nil
}

// NewConfig creates all the self-contained pieces making up an
// sample-generic-controlplane, but does not wire them yet into a server object.
func NewConfig(opts options.CompletedOptions) (*Config, error) {
	c := &Config{
		Options: opts,
	}

	genericConfig, versionedInformers, storageFactory, err := controlplaneapiserver.BuildGenericConfig(
		"sample-generic-controlplane",
		opts,
		[]*runtime.Scheme{legacyscheme.Scheme, apiextensionsapiserver.Scheme, aggregatorscheme.Scheme},
		controlplane.DefaultAPIResourceConfigSource(),
		generatedopenapi.GetOpenAPIDefinitions,
	)
	if err != nil {
		return nil, err
	}

	serviceResolver := webhook.NewDefaultServiceResolver()
	kubeAPIs, pluginInitializer, err := controlplaneapiserver.CreateConfig(opts, genericConfig, versionedInformers, storageFactory, serviceResolver, nil)
	if err != nil {
		return nil, err
	}
	c.ControlPlane = kubeAPIs

	authInfoResolver := webhook.NewDefaultAuthenticationInfoResolverWrapper(kubeAPIs.ProxyTransport, kubeAPIs.Generic.EgressSelector, kubeAPIs.Generic.LoopbackClientConfig, kubeAPIs.Generic.TracerProvider)
	apiExtensions, err := controlplaneapiserver.CreateAPIExtensionsConfig(*kubeAPIs.Generic, kubeAPIs.VersionedInformers, pluginInitializer, opts, 3, serviceResolver, authInfoResolver)
	if err != nil {
		return nil, err
	}
	c.APIExtensions = apiExtensions

	aggregator, err := controlplaneapiserver.CreateAggregatorConfig(*kubeAPIs.Generic, opts, kubeAPIs.VersionedInformers, serviceResolver, kubeAPIs.ProxyTransport, kubeAPIs.Extra.PeerProxy, pluginInitializer)
	if err != nil {
		return nil, err
	}
	c.Aggregator = aggregator
	c.Aggregator.ExtraConfig.DisableRemoteAvailableConditionController = true

	return c, nil
}
