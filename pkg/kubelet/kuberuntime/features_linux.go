//go:build linux
// +build linux

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

package kuberuntime

import (
	v1 "k8s.io/api/core/v1"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/kubernetes/pkg/features"
	kubetypes "k8s.io/kubernetes/pkg/kubelet/types"
)

func IsInPlacePodVerticalScalingAllowed(pod *v1.Pod) (allowed bool, msg string) {
	if !utilfeature.DefaultFeatureGate.Enabled(features.InPlacePodVerticalScaling) {
		return false, "InPlacePodVerticalScaling is disabled"
	}
	if kubetypes.IsStaticPod(pod) {
		return false, "In-place resize of static-pods is not supported"
	}
	return true, ""
}
