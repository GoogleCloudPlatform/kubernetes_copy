/*
Copyright 2019 The Kubernetes Authors.

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

package config

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// HPAControllerConfiguration contains elements describing HPAController.
type HPAControllerConfiguration struct {
	// ConcurrentHorizontalPodAutoscalerSyncs is the number of HPA objects that are allowed to sync concurrently.
	// Larger number = more responsive HPA processing, but more CPU (and network) load.
	ConcurrentHorizontalPodAutoscalerSyncs int32
	// horizontalPodAutoscalerSyncPeriod is the period for syncing the number of
	// pods in horizontal pod autoscaler.
	HorizontalPodAutoscalerSyncPeriod metav1.Duration
	// HorizontalPodAutoscalerDownscaleStabilizationWindow is a period for which autoscaler will look
	// backwards and not scale down below any recommendation it made during that period.
	HorizontalPodAutoscalerDownscaleStabilizationWindow metav1.Duration
	// horizontalPodAutoscalerTolerance is the tolerance for when
	// resource usage suggests upscaling/downscaling
	HorizontalPodAutoscalerTolerance float64
	// HorizontalPodAutoscalerCPUInitializationPeriod is the period after pod start when CPU samples
	// might be skipped.
	HorizontalPodAutoscalerCPUInitializationPeriod metav1.Duration
	// HorizontalPodAutoscalerInitialReadinessDelay is period after pod start during which readiness
	// changes are treated as readiness being set for the first time. The only effect of this is that
	// HPA will disregard CPU samples from unready pods that had last readiness change during that
	// period.
	HorizontalPodAutoscalerInitialReadinessDelay metav1.Duration
}
