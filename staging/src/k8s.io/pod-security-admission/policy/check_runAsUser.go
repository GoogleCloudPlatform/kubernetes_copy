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

package policy

import (
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/pod-security-admission/api"
)

/*
Containers must not set runAsUser: 0

**Restricted Fields:**

spec.securityContext.runAsUser
spec.containers[*].securityContext.runAsUser
spec.initContainers[*].securityContext.runAsUser

**Allowed Values:**
non-zero values
undefined/null

*/

func init() {
	addCheck(CheckRunAsUser)
}

// CheckRunAsUser returns a restricted level check
// that forbids runAsUser=0 in 1.23+
func CheckRunAsUser() Check {
	return Check{
		ID:    "runAsUser",
		Level: api.LevelRestricted,
		Versions: []VersionedCheck{
			{
				MinimumVersion: api.MajorMinorVersion(1, 23),
				CheckPod:       runAsUser_1_23,
			},
		},
	}
}

func runAsUser_1_23(podMetadata *metav1.ObjectMeta, podSpec *corev1.PodSpec) CheckResult {
	// See KEP-127: https://github.com/kubernetes/enhancements/blob/308ba8d/keps/sig-node/127-user-namespaces/README.md?plain=1#L411-L447
	if relaxPolicyForUserNamespacePod(podSpec) {
		return CheckResult{Allowed: true}
	}

	// things that explicitly set runAsUser=0
	var badSetters []string

	if podSpec.SecurityContext != nil && podSpec.SecurityContext.RunAsUser != nil && *podSpec.SecurityContext.RunAsUser == 0 {
		badSetters = append(badSetters, "pod")
	}

	// containers that explicitly set runAsUser=0
	var explicitlyBadContainers []string

	visitContainers(podSpec, func(container *corev1.Container) {
		if container.SecurityContext != nil && container.SecurityContext.RunAsUser != nil && *container.SecurityContext.RunAsUser == 0 {
			explicitlyBadContainers = append(explicitlyBadContainers, container.Name)
		}
	})

	if len(explicitlyBadContainers) > 0 {
		badSetters = append(
			badSetters,
			fmt.Sprintf(
				"%s %s",
				pluralize("container", "containers", len(explicitlyBadContainers)),
				joinQuote(explicitlyBadContainers),
			),
		)
	}
	// pod or containers explicitly set runAsUser=0
	if len(badSetters) > 0 {
		return CheckResult{
			Allowed:         false,
			ForbiddenReason: "runAsUser=0",
			ForbiddenDetail: fmt.Sprintf("%s must not set runAsUser=0", strings.Join(badSetters, " and ")),
		}
	}

	return CheckResult{Allowed: true}
}
