/*
Copyright 2025 The Kubernetes Authors.

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

package v1beta1

import (
	"k8s.io/apimachinery/pkg/conversion"
	configv1beta1 "k8s.io/kubelet/config/v1beta1"
	"k8s.io/kubernetes/pkg/kubelet/apis/config"
)

func Convert_config_CredentialProvider_To_v1beta1_CredentialProvider(in *config.CredentialProvider, out *configv1beta1.CredentialProvider, s conversion.Scope) error {
	// This conversion intentionally omits the tokenAttributes field which is only supported in v1 CredentialProvider.
	return autoConvert_config_CredentialProvider_To_v1beta1_CredentialProvider(in, out, s)
}
