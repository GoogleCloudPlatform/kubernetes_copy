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

package secrets

import (
	"encoding/json"

	v1 "k8s.io/api/core/v1"
	"k8s.io/kubernetes/pkg/credentialprovider"
)

// MakeDockerKeyring inspects the passedSecrets to see if they contain any DockerConfig secrets.  If they do,
// then a DockerKeyring is built based on every hit and unioned with the defaultKeyring.
// If they do not, then the default keyring is returned
func MakeDockerKeyring(passedSecrets []v1.Secret, defaultKeyring credentialprovider.DockerKeyring) (credentialprovider.DockerKeyring, error) {
	providerFromSecrets, err := secretsToTrackedDockerConfigs(passedSecrets)
	if err != nil {
		return nil, err
	}

	if providerFromSecrets == nil {
		return defaultKeyring, nil
	}

	return credentialprovider.UnionDockerKeyring{providerFromSecrets, defaultKeyring}, nil
}

func secretsToTrackedDockerConfigs(secrets []v1.Secret) (credentialprovider.DockerKeyring, error) {
	provider := &credentialprovider.BasicDockerKeyring{}
	validSecretsFound := 0
	for _, passedSecret := range secrets {
		if dockerConfigJSONBytes, dockerConfigJSONExists := passedSecret.Data[v1.DockerConfigJsonKey]; (passedSecret.Type == v1.SecretTypeDockerConfigJson) && dockerConfigJSONExists && (len(dockerConfigJSONBytes) > 0) {
			dockerConfigJSON := credentialprovider.DockerConfigJSON{}
			if err := json.Unmarshal(dockerConfigJSONBytes, &dockerConfigJSON); err != nil {
				return nil, err
			}

			coords := credentialprovider.SecretCoordinates{
				UID:       string(passedSecret.UID),
				Namespace: passedSecret.Namespace,
				Name:      passedSecret.Name}

			provider.Add(&credentialprovider.CredentialSource{Secret: coords}, dockerConfigJSON.Auths)
			validSecretsFound++
		} else if dockercfgBytes, dockercfgExists := passedSecret.Data[v1.DockerConfigKey]; (passedSecret.Type == v1.SecretTypeDockercfg) && dockercfgExists && (len(dockercfgBytes) > 0) {
			dockercfg := credentialprovider.DockerConfig{}
			if err := json.Unmarshal(dockercfgBytes, &dockercfg); err != nil {
				return nil, err
			}

			coords := credentialprovider.SecretCoordinates{
				UID:       string(passedSecret.UID),
				Namespace: passedSecret.Namespace,
				Name:      passedSecret.Name}
			provider.Add(&credentialprovider.CredentialSource{Secret: coords}, dockercfg)
			validSecretsFound++
		}
	}

	if validSecretsFound == 0 {
		return nil, nil
	}
	return provider, nil
}
