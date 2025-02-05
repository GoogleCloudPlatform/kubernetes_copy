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

package apiserver

import (
	"k8s.io/apiserver/pkg/endpoints/discovery"
	genericapiserver "k8s.io/apiserver/pkg/server"
)

type completedConfig struct {
	Generic genericapiserver.CompletedConfig
	*Extra
}

// CompletedConfig embeds a private pointer that cannot be instantiated outside of this package
type CompletedConfig struct {
	*completedConfig
}

func (c *Config) Complete() CompletedConfig {
	cfg := completedConfig{
		c.Generic.Complete(c.VersionedInformers, nil),
		&c.Extra,
	}

	discoveryAddresses := discovery.DefaultAddresses{DefaultAddress: cfg.Generic.ExternalAddress}
	cfg.Generic.DiscoveryAddresses = discoveryAddresses

	if cfg.Extra.PeerEndpointReconcileInterval == 0 {
		cfg.Extra.PeerEndpointReconcileInterval = DefaultPeerEndpointReconcileInterval
	}

	return CompletedConfig{&cfg}
}
