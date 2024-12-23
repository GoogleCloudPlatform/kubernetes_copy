/*
Copyright 2014 The Kubernetes Authors.

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

package options

import (
	"errors"
	"fmt"
	"net"
	"strings"

	genericoptions "k8s.io/apiserver/pkg/server/options"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	netutils "k8s.io/utils/net"

	controlplaneapiserver "k8s.io/kubernetes/pkg/controlplane/apiserver/options"
	"k8s.io/kubernetes/pkg/controlplane/reconcilers"
	"k8s.io/kubernetes/pkg/features"
)

// TODO: Longer term we should read this from some config store, rather than a flag.
// validateClusterIPFlags is expected to be called after Complete()
func validateClusterIPFlags(options Extra) []error {
	var errs []error
	// maxCIDRBits is used to define the maximum CIDR size for the cluster ip(s)
	maxCIDRBits := 20

	// validate that primary has been processed by user provided values or it has been defaulted
	if options.PrimaryServiceClusterIPRange.IP == nil {
		errs = append(errs, errors.New("--service-cluster-ip-range must contain at least one valid cidr"))
	}

	serviceClusterIPRangeList := strings.Split(options.ServiceClusterIPRanges, ",")
	if len(serviceClusterIPRangeList) > 2 {
		errs = append(errs, errors.New("--service-cluster-ip-range must not contain more than two entries"))
	}

	// Complete() expected to have set Primary* and Secondary
	if !utilfeature.DefaultFeatureGate.Enabled(features.MultiCIDRServiceAllocator) ||
		!utilfeature.DefaultFeatureGate.Enabled(features.DisableAllocatorDualWrite) {
		// primary CIDR validation
		if err := validateMaxCIDRRange(options.PrimaryServiceClusterIPRange, maxCIDRBits, "--service-cluster-ip-range"); err != nil {
			errs = append(errs, err)
		}
	}

	secondaryServiceClusterIPRangeUsed := (options.SecondaryServiceClusterIPRange.IP != nil)
	// note: While the cluster might be dualstack (i.e. pods with multiple IPs), the user may choose
	// to only ingress traffic within and into the cluster on one IP family only. this family is decided
	// by the range set on --service-cluster-ip-range. If/when the user decides to use dual stack services
	// the Secondary* must be of different IPFamily than --service-cluster-ip-range
	if secondaryServiceClusterIPRangeUsed {
		// Should be dualstack IPFamily(PrimaryServiceClusterIPRange) != IPFamily(SecondaryServiceClusterIPRange)
		dualstack, err := netutils.IsDualStackCIDRs([]*net.IPNet{&options.PrimaryServiceClusterIPRange, &options.SecondaryServiceClusterIPRange})
		if err != nil {
			errs = append(errs, fmt.Errorf("error attempting to validate dualstack for --service-cluster-ip-range value error:%v", err))
		}

		if !dualstack {
			errs = append(errs, errors.New("--service-cluster-ip-range[0] and --service-cluster-ip-range[1] must be of different IP family"))
		}
		if !utilfeature.DefaultFeatureGate.Enabled(features.MultiCIDRServiceAllocator) ||
			!utilfeature.DefaultFeatureGate.Enabled(features.DisableAllocatorDualWrite) {
			if err := validateMaxCIDRRange(options.SecondaryServiceClusterIPRange, maxCIDRBits, "--service-cluster-ip-range[1]"); err != nil {
				errs = append(errs, err)
			}
		}
	}

	return errs
}

func validateMaxCIDRRange(cidr net.IPNet, maxCIDRBits int, cidrFlag string) error {
	// Should be smallish sized cidr, this thing is kept in etcd
	// bigger cidr (specially those offered by IPv6) will add no value
	// significantly increase snapshotting time.
	var ones, bits = cidr.Mask.Size()
	if bits-ones > maxCIDRBits {
		return fmt.Errorf("specified %s is too large; for %d-bit addresses, the mask must be >= %d", cidrFlag, bits, bits-maxCIDRBits)
	}

	return nil
}

func validateServiceNodePort(options Extra) []error {
	var errs []error

	if options.KubernetesServiceNodePort < 0 || options.KubernetesServiceNodePort > 65535 {
		errs = append(errs, fmt.Errorf("--kubernetes-service-node-port %v must be between 0 and 65535, inclusive. If 0, the Kubernetes master service will be of type ClusterIP", options.KubernetesServiceNodePort))
	}

	if options.KubernetesServiceNodePort > 0 && !options.ServiceNodePortRange.Contains(options.KubernetesServiceNodePort) {
		errs = append(errs, fmt.Errorf("kubernetes service node port range %v doesn't contain %v", options.ServiceNodePortRange, options.KubernetesServiceNodePort))
	}
	return errs
}

func validatePublicIPServiceClusterIPRangeIPFamilies(extra Extra, generic genericoptions.ServerRunOptions) []error {
	// The "kubernetes.default" Service is SingleStack based on the configured ServiceIPRange.
	// If the bootstrap controller reconcile the kubernetes.default Service and Endpoints, it must
	// guarantee that the Service ClusterIPRepairConfig and the associated Endpoints have the same IP family, or
	// it will not work for clients because of the IP family mismatch.
	// TODO: revisit for dual-stack https://github.com/kubernetes/enhancements/issues/2438
	if reconcilers.Type(extra.EndpointReconcilerType) != reconcilers.NoneEndpointReconcilerType {
		serviceIPRange, _, err := controlplaneapiserver.ServiceIPRange(extra.PrimaryServiceClusterIPRange)
		if err != nil {
			return []error{fmt.Errorf("error determining service IP ranges: %w", err)}
		}
		if netutils.IsIPv4CIDR(&serviceIPRange) != netutils.IsIPv4(generic.AdvertiseAddress) {
			return []error{fmt.Errorf("service IP family %q must match public address family %q", extra.PrimaryServiceClusterIPRange.String(), generic.AdvertiseAddress.String())}
		}
	}

	return nil
}

// Validate checks ServerRunOptions and return a slice of found errs.
func (s CompletedOptions) Validate() []error {
	var errs []error

	errs = append(errs, s.CompletedOptions.Validate()...)
	errs = append(errs, s.CloudProvider.Validate()...)
	errs = append(errs, validateClusterIPFlags(s.Extra)...)
	errs = append(errs, validateServiceNodePort(s.Extra)...)
	errs = append(errs, validatePublicIPServiceClusterIPRangeIPFamilies(s.Extra, *s.GenericServerRunOptions)...)

	if s.MasterCount <= 0 {
		errs = append(errs, fmt.Errorf("--apiserver-count should be a positive number, but value '%d' provided", s.MasterCount))
	}

	return errs
}
