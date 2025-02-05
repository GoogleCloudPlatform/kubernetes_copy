/*
Copyright 2022 The Kubernetes Authors.

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

package service

import (
	"fmt"

	utilvalidation "k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	api "k8s.io/kubernetes/pkg/apis/core"
	"k8s.io/kubernetes/pkg/apis/core/helper"
)

func GetWarningsForService(service, oldService *api.Service) []string {
	if service == nil {
		return nil
	}
	var warnings []string

	if _, ok := service.Annotations[api.DeprecatedAnnotationTopologyAwareHints]; ok {
		warnings = append(warnings, fmt.Sprintf("annotation %s is deprecated, please use %s instead", api.DeprecatedAnnotationTopologyAwareHints, api.AnnotationTopologyMode))
	}

	if helper.IsServiceIPSet(service) {
		for i, clusterIP := range service.Spec.ClusterIPs {
			warnings = append(warnings, utilvalidation.GetWarningsForIP(field.NewPath("spec").Child("clusterIPs").Index(i), clusterIP)...)
		}
	}

	for i, externalIP := range service.Spec.ExternalIPs {
		warnings = append(warnings, utilvalidation.GetWarningsForIP(field.NewPath("spec").Child("externalIPs").Index(i), externalIP)...)
	}

	if len(service.Spec.LoadBalancerIP) > 0 {
		warnings = append(warnings, utilvalidation.GetWarningsForIP(field.NewPath("spec").Child("loadBalancerIP"), service.Spec.LoadBalancerIP)...)
	}

	for i, cidr := range service.Spec.LoadBalancerSourceRanges {
		warnings = append(warnings, utilvalidation.GetWarningsForCIDR(field.NewPath("spec").Child("loadBalancerSourceRanges").Index(i), cidr)...)
	}

	if service.Spec.Type == api.ServiceTypeExternalName && len(service.Spec.ExternalIPs) > 0 {
		warnings = append(warnings, fmt.Sprintf("spec.externalIPs is ignored when spec.type is %q", api.ServiceTypeExternalName))
	}
	if service.Spec.Type != api.ServiceTypeExternalName && service.Spec.ExternalName != "" {
		warnings = append(warnings, fmt.Sprintf("spec.externalName is ignored when spec.type is not %q", api.ServiceTypeExternalName))
	}

	return warnings
}
