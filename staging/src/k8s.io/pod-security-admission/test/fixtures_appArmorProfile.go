/*
Copyright 2021 The Kubernetes Authors.

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

package test

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/pod-security-admission/api"
)

func init() {
	appArmorFixture_1_0 := fixtureGenerator{
		expectErrorSubstring: "forbidden AppArmor profile",
		generatePass: func(pod *corev1.Pod) []*corev1.Pod {
			pod = ensureAnnotation(pod)
			return []*corev1.Pod{
				// container with runtime/default annotation
				// container with localhost/foo annotation
				tweak(pod, func(copy *corev1.Pod) {
					containerName := copy.Spec.Containers[0].Name
					copy.Annotations[corev1.DeprecatedAppArmorBetaContainerAnnotationKeyPrefix+containerName] = "runtime/default"

					initContainerName := copy.Spec.Containers[0].Name
					copy.Annotations[corev1.DeprecatedAppArmorBetaContainerAnnotationKeyPrefix+initContainerName] = "localhost/foo"
				}),
			}
		},
		generateFail: func(pod *corev1.Pod) []*corev1.Pod {
			pod = ensureAnnotation(pod)
			return []*corev1.Pod{
				// container with unconfined annotation
				tweak(pod, func(copy *corev1.Pod) {
					name := copy.Spec.Containers[0].Name
					copy.Annotations[corev1.DeprecatedAppArmorBetaContainerAnnotationKeyPrefix+name] = "unconfined"
				}),

				// initContainer with unconfined annotation
				tweak(pod, func(copy *corev1.Pod) {
					name := copy.Spec.InitContainers[0].Name
					copy.Annotations[corev1.DeprecatedAppArmorBetaContainerAnnotationKeyPrefix+name] = "unconfined"
				}),
			}
		},
	}

	registerFixtureGenerator(
		fixtureKey{level: api.LevelBaseline, version: api.MajorMinorVersion(1, 0), check: "appArmorProfile"},
		appArmorFixture_1_0,
	)
}
