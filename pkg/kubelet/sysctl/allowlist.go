/*
Copyright 2016 The Kubernetes Authors.

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

package sysctl

import (
	"fmt"
	"strings"

	v1 "k8s.io/api/core/v1"
	utilsysctl "k8s.io/component-helpers/node/util/sysctl"
	"k8s.io/kubernetes/pkg/apis/core/validation"
	policyvalidation "k8s.io/kubernetes/pkg/apis/policy/validation"
	"k8s.io/kubernetes/pkg/kubelet/lifecycle"
)

const (
	ForbiddenReason = "SysctlForbidden"
)

// patternAllowlist takes a list of sysctls or sysctl patterns (ending in *) and
// checks validity via a sysctl and prefix map, rejecting those which are not known
// to be namespaced.
type patternAllowlist struct {
	sysctls  map[string]utilsysctl.Namespace
	prefixes map[string]utilsysctl.Namespace
}

var _ lifecycle.PodAdmitHandler = &patternAllowlist{}

// NewAllowlist creates a new Allowlist from a list of sysctls and sysctl pattern (ending in *).
func NewAllowlist(patterns []string) (*patternAllowlist, error) {
	w := &patternAllowlist{
		sysctls:  map[string]utilsysctl.Namespace{},
		prefixes: map[string]utilsysctl.Namespace{},
	}
	if err := w.setPrefixAndSysctls(patterns); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *patternAllowlist) setPrefixAndSysctls(patterns []string) error {
	for _, s := range patterns {
		if !policyvalidation.IsValidSysctlPattern(s) {
			return fmt.Errorf("sysctl %q must have at most %d characters and match regex %s",
				s,
				validation.SysctlMaxLength,
				policyvalidation.SysctlContainSlashPatternFmt,
			)
		}
		ns, sysctlOrPrefix, prefixed := utilsysctl.GetNamespace(s)
		if ns == utilsysctl.UnknownNamespace {
			return fmt.Errorf("the sysctls %q are not known to be namespaced", sysctlOrPrefix)
		}
		if prefixed {
			w.prefixes[sysctlOrPrefix] = ns
		} else {
			w.sysctls[sysctlOrPrefix] = ns
		}
	}
	return nil
}

// validateSysctl checks that a sysctl is allowlisted because it is known
// to be namespaced by the Linux kernel. Note that being allowlisted is required, but not
// sufficient: the container runtime might have a stricter check and refuse to launch a pod.
//
// The parameters hostNet and hostIPC are used to forbid sysctls for pod sharing the
// respective namespaces with the host. This check is only possible for sysctls on
// the static default allowlist, not those on the custom allowlist provided by the admin.
func (w *patternAllowlist) validateSysctl(sysctl string, hostNet, hostIPC bool) error {
	sysctl = utilsysctl.NormalizeName(sysctl)
	nsErrorFmt := "%q not allowed with host %s enabled"
	if ns, found := w.sysctls[sysctl]; found {
		if ns == utilsysctl.IPCNamespace && hostIPC {
			return fmt.Errorf(nsErrorFmt, sysctl, ns)
		}
		if ns == utilsysctl.NetNamespace && hostNet {
			return fmt.Errorf(nsErrorFmt, sysctl, ns)
		}
		return nil
	}
	for p, ns := range w.prefixes {
		if strings.HasPrefix(sysctl, p) {
			if ns == utilsysctl.IPCNamespace && hostIPC {
				return fmt.Errorf(nsErrorFmt, sysctl, ns)
			}
			if ns == utilsysctl.NetNamespace && hostNet {
				return fmt.Errorf(nsErrorFmt, sysctl, ns)
			}
			return nil
		}
	}
	return fmt.Errorf("%q not allowlisted", sysctl)
}

// Admit checks that all sysctls given in pod's security context
// are valid according to the allowlist.
func (w *patternAllowlist) Admit(attrs *lifecycle.PodAdmitAttributes) lifecycle.PodAdmitResult {
	pod := attrs.Pod
	if pod.Spec.SecurityContext == nil || len(pod.Spec.SecurityContext.Sysctls) == 0 {
		return lifecycle.PodAdmitResult{
			Admit: true,
		}
	}
	if pod.Spec.OS != nil && pod.Spec.OS.Name == v1.Linux {
		// Unsafe sysctl list would have been passed by kubelet
		if err := w.setPrefixAndSysctls(safeAllowedList()); err != nil {
			return lifecycle.PodAdmitResult{
				Admit:   false,
				Reason:  ForbiddenReason,
				Message: fmt.Sprintf("setting prefixes and sysctls failed: %v", err),
			}
		}
	}
	for _, s := range pod.Spec.SecurityContext.Sysctls {
		if err := w.validateSysctl(s.Name, pod.Spec.HostNetwork, pod.Spec.HostIPC); err != nil {
			return lifecycle.PodAdmitResult{
				Admit:   false,
				Reason:  ForbiddenReason,
				Message: fmt.Sprintf("forbidden sysctl: %v", err),
			}
		}
	}

	return lifecycle.PodAdmitResult{
		Admit: true,
	}
}
