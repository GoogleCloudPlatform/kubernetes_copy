//go:build !ignore_autogenerated
// +build !ignore_autogenerated

/*
Copyright The Kubernetes Authors.

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

// Code generated by conversion-gen. DO NOT EDIT.

package v1

import (
	unsafe "unsafe"

	conversion "k8s.io/apimachinery/pkg/conversion"
	runtime "k8s.io/apimachinery/pkg/runtime"
	webhookadmission "k8s.io/apiserver/pkg/admission/plugin/webhook/config/apis/webhookadmission"
)

func init() {
	localSchemeBuilder.Register(RegisterConversions)
}

// RegisterConversions adds conversion functions to the given scheme.
// Public to allow building arbitrary schemes.
func RegisterConversions(s *runtime.Scheme) error {
	if err := s.AddGeneratedConversionFunc((*ExclusionRule)(nil), (*webhookadmission.ExclusionRule)(nil), func(a, b interface{}, scope conversion.Scope) error {
		return Convert_v1_ExclusionRule_To_webhookadmission_ExclusionRule(a.(*ExclusionRule), b.(*webhookadmission.ExclusionRule), scope)
	}); err != nil {
		return err
	}
	if err := s.AddGeneratedConversionFunc((*webhookadmission.ExclusionRule)(nil), (*ExclusionRule)(nil), func(a, b interface{}, scope conversion.Scope) error {
		return Convert_webhookadmission_ExclusionRule_To_v1_ExclusionRule(a.(*webhookadmission.ExclusionRule), b.(*ExclusionRule), scope)
	}); err != nil {
		return err
	}
	if err := s.AddGeneratedConversionFunc((*WebhookAdmission)(nil), (*webhookadmission.WebhookAdmission)(nil), func(a, b interface{}, scope conversion.Scope) error {
		return Convert_v1_WebhookAdmission_To_webhookadmission_WebhookAdmission(a.(*WebhookAdmission), b.(*webhookadmission.WebhookAdmission), scope)
	}); err != nil {
		return err
	}
	if err := s.AddGeneratedConversionFunc((*webhookadmission.WebhookAdmission)(nil), (*WebhookAdmission)(nil), func(a, b interface{}, scope conversion.Scope) error {
		return Convert_webhookadmission_WebhookAdmission_To_v1_WebhookAdmission(a.(*webhookadmission.WebhookAdmission), b.(*WebhookAdmission), scope)
	}); err != nil {
		return err
	}
	return nil
}

func autoConvert_v1_ExclusionRule_To_webhookadmission_ExclusionRule(in *ExclusionRule, out *webhookadmission.ExclusionRule, s conversion.Scope) error {
	out.APIGroups = *(*[]string)(unsafe.Pointer(&in.APIGroups))
	out.APIVersions = *(*[]string)(unsafe.Pointer(&in.APIVersions))
	out.Kind = in.Kind
	out.Name = in.Name
	out.Namespace = in.Namespace
	return nil
}

// Convert_v1_ExclusionRule_To_webhookadmission_ExclusionRule is an autogenerated conversion function.
func Convert_v1_ExclusionRule_To_webhookadmission_ExclusionRule(in *ExclusionRule, out *webhookadmission.ExclusionRule, s conversion.Scope) error {
	return autoConvert_v1_ExclusionRule_To_webhookadmission_ExclusionRule(in, out, s)
}

func autoConvert_webhookadmission_ExclusionRule_To_v1_ExclusionRule(in *webhookadmission.ExclusionRule, out *ExclusionRule, s conversion.Scope) error {
	out.APIGroups = *(*[]string)(unsafe.Pointer(&in.APIGroups))
	out.APIVersions = *(*[]string)(unsafe.Pointer(&in.APIVersions))
	out.Kind = in.Kind
	out.Name = in.Name
	out.Namespace = in.Namespace
	return nil
}

// Convert_webhookadmission_ExclusionRule_To_v1_ExclusionRule is an autogenerated conversion function.
func Convert_webhookadmission_ExclusionRule_To_v1_ExclusionRule(in *webhookadmission.ExclusionRule, out *ExclusionRule, s conversion.Scope) error {
	return autoConvert_webhookadmission_ExclusionRule_To_v1_ExclusionRule(in, out, s)
}

func autoConvert_v1_WebhookAdmission_To_webhookadmission_WebhookAdmission(in *WebhookAdmission, out *webhookadmission.WebhookAdmission, s conversion.Scope) error {
	out.KubeConfigFile = in.KubeConfigFile
	out.ExclusionRules = *(*[]webhookadmission.ExclusionRule)(unsafe.Pointer(&in.ExclusionRules))
	return nil
}

// Convert_v1_WebhookAdmission_To_webhookadmission_WebhookAdmission is an autogenerated conversion function.
func Convert_v1_WebhookAdmission_To_webhookadmission_WebhookAdmission(in *WebhookAdmission, out *webhookadmission.WebhookAdmission, s conversion.Scope) error {
	return autoConvert_v1_WebhookAdmission_To_webhookadmission_WebhookAdmission(in, out, s)
}

func autoConvert_webhookadmission_WebhookAdmission_To_v1_WebhookAdmission(in *webhookadmission.WebhookAdmission, out *WebhookAdmission, s conversion.Scope) error {
	out.KubeConfigFile = in.KubeConfigFile
	out.ExclusionRules = *(*[]ExclusionRule)(unsafe.Pointer(&in.ExclusionRules))
	return nil
}

// Convert_webhookadmission_WebhookAdmission_To_v1_WebhookAdmission is an autogenerated conversion function.
func Convert_webhookadmission_WebhookAdmission_To_v1_WebhookAdmission(in *webhookadmission.WebhookAdmission, out *WebhookAdmission, s conversion.Scope) error {
	return autoConvert_webhookadmission_WebhookAdmission_To_v1_WebhookAdmission(in, out, s)
}
