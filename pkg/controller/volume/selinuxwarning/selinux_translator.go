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

package selinuxwarning

import (
	"strings"

	v1 "k8s.io/api/core/v1"
	"k8s.io/kubernetes/pkg/volume/util"
)

// controllerSELinuxTranslator is implementation of SELinuxLabelTranslator that can be used in kube-controller-manager (KCM).
// A real SELinuxLabelTranslator would be able to file empty parts of SELinuxOptions from the operating system defaults (/etc/selinux/*).
// KCM often runs as a container and cannot access /etc/selinux on the host. Even if it could, KCM can run on a different distro
// than the actual worker nodes.
// Therefore do not even try to file the defaults, use only fields filed in the provided SELinuxOptions.
// This may lead to false conflicts - two SELinuxOptions that are the same after proper defaulting are not equal in KCM.
// E.g, {"type": "container_t", "level": "c0,c1"} is equal to {"type": "", "level": "c0,c1"} on Fedora Linux, because "container_t"
// is the default there. But the controller does not know the default, e.g. Debian uses "svirt_lxc_net_t" instead.
type controllerSELinuxTranslator struct{}

func (c *controllerSELinuxTranslator) SELinuxEnabled() bool {
	// The controller must have been explicitly enabled, so expect that all nodes have SELinux enabled.
	return true
}

func (c *controllerSELinuxTranslator) SELinuxOptionsToFileLabel(opts *v1.SELinuxOptions) (string, error) {
	if opts == nil {
		return "", nil
	}
	// kube-controller-manager cannot access SELinux defaults in /etc/selinux on nodes.
	// Just concatenate the existing fields and do not try to default the missing ones.
	parts := make([]string, 0, 4)
	if opts.User != "" {
		parts = append(parts, opts.User)
	}
	if opts.Role != "" {
		parts = append(parts, opts.Role)
	}
	if opts.Type != "" {
		parts = append(parts, opts.Type)
	}
	if opts.Level != "" {
		parts = append(parts, opts.Level)
	}
	return strings.Join(parts, ":"), nil
}

var _ util.SELinuxLabelTranslator = &controllerSELinuxTranslator{}
