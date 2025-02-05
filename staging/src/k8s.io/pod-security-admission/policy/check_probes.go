/*
Copyright 2024 The Kubernetes Authors.

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
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/pod-security-admission/api"
)

/*
Host must be forbidden in the probes.

**Restricted Fields:**

spec.containers[*].livenessProbe.httpGet.host
spec.containers[*].readinessProbe.httpGet.host
spec.containers[*].startupProbe.httpGet.host
spec.containers[*].livenessProbe.tcpSocket.host
spec.containers[*].readinessProbe.tcpSocket.host
spec.containers[*].startupProbe.tcpSocket.host


**Allowed Values:** undefined/empty
*/

func init() {
	addCheck(CheckProbes)
}

// CheckProbes returns a baseline level check
// that forbids any host ports in 1.0+
func CheckProbes() Check {
	return Check{
		ID:    "probeHost",
		Level: api.LevelBaseline,
		Versions: []VersionedCheck{
			{
				MinimumVersion: api.MajorMinorVersion(1, 0),
				CheckPod:       probeHost_1_0,
			},
		},
	}
}

func probeHost_1_0(podMetadata *metav1.ObjectMeta, podSpec *corev1.PodSpec) CheckResult {
	var badContainers []string
	forbiddenProbeHost := sets.NewString()
	visitContainers(podSpec, func(container *corev1.Container) {
		valid := true
		lp := container.LivenessProbe
		if lp != nil && lp.HTTPGet != nil && lp.HTTPGet.Host != "" {
			valid = false
			forbiddenProbeHost.Insert(lp.HTTPGet.Host)
		}
		rp := container.ReadinessProbe
		if rp != nil && rp.HTTPGet != nil && rp.HTTPGet.Host != "" {
			valid = false
			forbiddenProbeHost.Insert(rp.HTTPGet.Host)
		}
		sp := container.StartupProbe
		if sp != nil && sp.HTTPGet != nil && sp.HTTPGet.Host != "" {
			valid = false
			forbiddenProbeHost.Insert(lp.HTTPGet.Host)
		}
		if !valid {
			badContainers = append(badContainers, container.Name)
		}
	})

	if len(badContainers) > 0 {
		return CheckResult{
			Allowed:         false,
			ForbiddenReason: "probeHost",
			ForbiddenDetail: fmt.Sprintf(
				"%s %s %s %s %s",
				pluralize("container", "containers", len(badContainers)),
				joinQuote(badContainers),
				pluralize("uses", "use", len(badContainers)),
				pluralize("probeHost", "probeHosts", len(forbiddenProbeHost)),
				strings.Join(forbiddenProbeHost.List(), ", "),
			),
		}
	}
	return CheckResult{Allowed: true}
}
