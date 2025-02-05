/*
Copyright 2020 The Kubernetes Authors.

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
	"fmt"
	"os"
	"strings"

	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	credentialproviderv1 "k8s.io/kubelet/pkg/apis/credentialprovider/v1"
	"k8s.io/kubernetes/pkg/credentialprovider"
	kubeletconfig "k8s.io/kubernetes/pkg/kubelet/apis/config"
)

// readCredentialProviderConfigFile receives a path to a config file and decodes it
// into the internal CredentialProviderConfig type.
func readCredentialProviderConfigFile(configPath string) (*kubeletconfig.CredentialProviderConfig, error) {
	if configPath == "" {
		return nil, fmt.Errorf("credential provider config path is empty")
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("unable to read external registry credential provider configuration from %q: %w", configPath, err)
	}

	config, err := decode(data)
	if err != nil {
		return nil, fmt.Errorf("error decoding config %s: %w", configPath, err)
	}

	return config, nil
}

// decode decodes data into the internal CredentialProviderConfig type.
func decode(data []byte) (*kubeletconfig.CredentialProviderConfig, error) {
	obj, gvk, err := codecs.UniversalDecoder().Decode(data, nil, nil)
	if err != nil {
		return nil, err
	}

	if gvk.Kind != "CredentialProviderConfig" {
		return nil, fmt.Errorf("failed to decode %q (wrong Kind)", gvk.Kind)
	}

	if gvk.Group != kubeletconfig.GroupName {
		return nil, fmt.Errorf("failed to decode CredentialProviderConfig, unexpected Group: %s", gvk.Group)
	}

	if internalConfig, ok := obj.(*kubeletconfig.CredentialProviderConfig); ok {
		return internalConfig, nil
	}

	return nil, fmt.Errorf("unable to convert %T to *CredentialProviderConfig", obj)
}

// validateCredentialProviderConfig validates CredentialProviderConfig.
func validateCredentialProviderConfig(config *kubeletconfig.CredentialProviderConfig, saTokenForCredentialProviders bool) field.ErrorList {
	allErrs := field.ErrorList{}

	if len(config.Providers) == 0 {
		allErrs = append(allErrs, field.Required(field.NewPath("providers"), "at least 1 item in plugins is required"))
	}

	fieldPath := field.NewPath("providers")
	seenProviderNames := sets.NewString()
	for _, provider := range config.Providers {
		if strings.Contains(provider.Name, "/") {
			allErrs = append(allErrs, field.Invalid(fieldPath.Child("name"), provider.Name, "provider name cannot contain '/'"))
		}

		if strings.Contains(provider.Name, " ") {
			allErrs = append(allErrs, field.Invalid(fieldPath.Child("name"), provider.Name, "provider name cannot contain spaces"))
		}

		if provider.Name == "." {
			allErrs = append(allErrs, field.Invalid(fieldPath.Child("name"), provider.Name, "provider name cannot be '.'"))
		}

		if provider.Name == ".." {
			allErrs = append(allErrs, field.Invalid(fieldPath.Child("name"), provider.Name, "provider name cannot be '..'"))
		}

		if seenProviderNames.Has(provider.Name) {
			allErrs = append(allErrs, field.Duplicate(fieldPath.Child("name"), provider.Name))
		}
		seenProviderNames.Insert(provider.Name)

		if provider.APIVersion == "" {
			allErrs = append(allErrs, field.Required(fieldPath.Child("apiVersion"), "apiVersion is required"))
		} else if _, ok := apiVersions[provider.APIVersion]; !ok {
			validAPIVersions := sets.StringKeySet(apiVersions).List()
			allErrs = append(allErrs, field.NotSupported(fieldPath.Child("apiVersion"), provider.APIVersion, validAPIVersions))
		}

		if len(provider.MatchImages) == 0 {
			allErrs = append(allErrs, field.Required(fieldPath.Child("matchImages"), "at least 1 item in matchImages is required"))
		}

		for _, matchImage := range provider.MatchImages {
			if _, err := credentialprovider.ParseSchemelessURL(matchImage); err != nil {
				allErrs = append(allErrs, field.Invalid(fieldPath.Child("matchImages"), matchImage, fmt.Sprintf("match image is invalid: %s", err.Error())))
			}
		}

		if provider.DefaultCacheDuration == nil {
			allErrs = append(allErrs, field.Required(fieldPath.Child("defaultCacheDuration"), "defaultCacheDuration is required"))
		}

		if provider.DefaultCacheDuration != nil && provider.DefaultCacheDuration.Duration < 0 {
			allErrs = append(allErrs, field.Invalid(fieldPath.Child("defaultCacheDuration"), provider.DefaultCacheDuration.Duration, "defaultCacheDuration must be greater than or equal to 0"))
		}

		if provider.TokenAttributes != nil {
			fldPath := fieldPath.Child("tokenAttributes")
			if !saTokenForCredentialProviders {
				allErrs = append(allErrs, field.Forbidden(fldPath, "tokenAttributes is not supported when KubeletServiceAccountTokenForCredentialProviders feature gate is disabled"))
			}
			if len(provider.TokenAttributes.ServiceAccountTokenAudience) == 0 {
				allErrs = append(allErrs, field.Required(fldPath.Child("serviceAccountTokenAudience"), "serviceAccountTokenAudience is required"))
			}
			if provider.TokenAttributes.RequireServiceAccount == nil {
				allErrs = append(allErrs, field.Required(fldPath.Child("requireServiceAccount"), "requireServiceAccount is required"))
			}
			if provider.APIVersion != credentialproviderv1.SchemeGroupVersion.String() {
				allErrs = append(allErrs, field.Forbidden(fldPath, fmt.Sprintf("tokenAttributes is only supported for %s API version", credentialproviderv1.SchemeGroupVersion.String())))
			}

			seenAnnotationKeys := sets.NewString()
			// Using the validation logic for keys from https://github.com/kubernetes/kubernetes/blob/69dbc74417304328a9fd3c161643dc4f0a057f41/staging/src/k8s.io/apimachinery/pkg/api/validation/objectmeta.go#L46-L51
			for _, k := range provider.TokenAttributes.ServiceAccountAnnotationKeys {
				// The rule is QualifiedName except that case doesn't matter, so convert to lowercase before checking.
				for _, msg := range validation.IsQualifiedName(strings.ToLower(k)) {
					allErrs = append(allErrs, field.Invalid(fldPath.Child("serviceAccountAnnotationKeys"), k, msg))
				}
				if seenAnnotationKeys.Has(k) {
					allErrs = append(allErrs, field.Duplicate(fldPath.Child("serviceAccountAnnotationKeys"), k))
				}
				seenAnnotationKeys.Insert(k)
			}
		}
	}

	return allErrs
}
