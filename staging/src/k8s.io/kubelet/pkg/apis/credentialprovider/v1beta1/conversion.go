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
	"k8s.io/kubelet/pkg/apis/credentialprovider"
)

func Convert_credentialprovider_CredentialProviderRequest_To_v1beta1_CredentialProviderRequest(in *credentialprovider.CredentialProviderRequest, out *CredentialProviderRequest, s conversion.Scope) error {
	// This conversion intentionally omits the serviceAccountToken and serviceAccountAnnotations fields which are only supported in v1 CredentialProviderRequest.
	return autoConvert_credentialprovider_CredentialProviderRequest_To_v1beta1_CredentialProviderRequest(in, out, s)
}
