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

package plugin

import (
	"sync"

	"k8s.io/apimachinery/pkg/types"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/klog/v2"
	"k8s.io/kube-openapi/pkg/util/sets"
	"k8s.io/kubernetes/pkg/credentialprovider"
	"k8s.io/kubernetes/pkg/features"
)

type provider struct {
	name string
	impl *pluginProvider
}

var providersMutex sync.RWMutex
var providers = make([]provider, 0)
var seenProviderNames = sets.NewString()

func registerCredentialProviderPlugin(name string, p *pluginProvider) {
	providersMutex.Lock()
	defer providersMutex.Unlock()

	if seenProviderNames.Has(name) {
		klog.Fatalf("Credential provider %q was registered twice", name)
	}
	seenProviderNames.Insert(name)

	providers = append(providers, provider{name, p})
	klog.V(4).Infof("Registered credential provider %q", name)
}

type externalCredentialProviderKeyring struct {
	providers []credentialprovider.DockerConfigProvider
}

func NewExternalCredentialProviderDockerKeyring(podName, podNamespace, podUID, serviceAccountName string) credentialprovider.DockerKeyring {
	providersMutex.RLock()
	defer providersMutex.RUnlock()

	keyring := &externalCredentialProviderKeyring{
		providers: make([]credentialprovider.DockerConfigProvider, 0, len(providers)),
	}

	for _, p := range providers {
		if !p.impl.Enabled() {
			continue
		}

		var pp *perPodPluginProvider
		if utilfeature.DefaultFeatureGate.Enabled(features.KubeletServiceAccountTokenForCredentialProviders) {
			klog.V(4).InfoS("Generating per pod credential provider", "provider", p.name, "podName", podName, "podNamespace", podNamespace, "podUID", podUID, "serviceAccountName", serviceAccountName)
			pp = &perPodPluginProvider{provider: p.impl, podName: podName, podNamespace: podNamespace, podUID: types.UID(podUID), serviceAccountName: serviceAccountName}
		} else {
			klog.V(4).InfoS("Generating credential provider", "provider", p.name)
			pp = &perPodPluginProvider{provider: p.impl}
		}

		keyring.providers = append(keyring.providers, pp)
	}

	return keyring
}

func (k *externalCredentialProviderKeyring) Lookup(image string) ([]credentialprovider.AuthConfig, bool) {
	keyring := &credentialprovider.BasicDockerKeyring{}

	for _, p := range k.providers {
		keyring.Add(p.Provide(image))
	}

	return keyring.Lookup(image)
}
