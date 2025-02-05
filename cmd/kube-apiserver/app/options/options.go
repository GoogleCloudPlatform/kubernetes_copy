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

// Package options contains flags and options for initializing an apiserver
package options

import (
	"net"
	"strings"
	"time"

	v1 "k8s.io/api/core/v1"
	utilnet "k8s.io/apimachinery/pkg/util/net"
	cliflag "k8s.io/component-base/cli/flag"

	api "k8s.io/kubernetes/pkg/apis/core"
	"k8s.io/kubernetes/pkg/cluster/ports"
	controlplaneapiserver "k8s.io/kubernetes/pkg/controlplane/apiserver/options"
	"k8s.io/kubernetes/pkg/controlplane/reconcilers"
	_ "k8s.io/kubernetes/pkg/features" // add the kubernetes feature gates
	kubeoptions "k8s.io/kubernetes/pkg/kubeapiserver/options"
	kubeletclient "k8s.io/kubernetes/pkg/kubelet/client"
)

// ServerRunOptions runs a kubernetes api server.
type ServerRunOptions struct {
	*controlplaneapiserver.Options // embedded to avoid noise in existing consumers
	CloudProvider                  *kubeoptions.CloudProviderOptions

	Extra
}

type Extra struct {
	AllowPrivileged           bool
	KubeletConfig             kubeletclient.KubeletClientConfig
	KubernetesServiceNodePort int
	// ServiceClusterIPRange is mapped to input provided by user
	ServiceClusterIPRanges string
	// PrimaryServiceClusterIPRange and SecondaryServiceClusterIPRange are the results
	// of parsing ServiceClusterIPRange into actual values
	PrimaryServiceClusterIPRange   net.IPNet
	SecondaryServiceClusterIPRange net.IPNet
	// APIServerServiceIP is the first valid IP from PrimaryServiceClusterIPRange
	APIServerServiceIP net.IP

	ServiceNodePortRange utilnet.PortRange

	EndpointReconcilerType string

	MasterCount int
}

// NewServerRunOptions creates and returns ServerRunOptions according to the given featureGate and effectiveVersion of the server binary to run.
func NewServerRunOptions() *ServerRunOptions {
	s := ServerRunOptions{
		Options:       controlplaneapiserver.NewOptions(),
		CloudProvider: kubeoptions.NewCloudProviderOptions(),

		Extra: Extra{
			EndpointReconcilerType: string(reconcilers.LeaseEndpointReconcilerType),
			KubeletConfig: kubeletclient.KubeletClientConfig{
				Port: ports.KubeletPort,
				PreferredAddressTypes: []string{
					// --override-hostname
					string(api.NodeHostName),

					// internal, preferring DNS if reported
					string(api.NodeInternalDNS),
					string(api.NodeInternalIP),

					// external, preferring DNS if reported
					string(api.NodeExternalDNS),
					string(api.NodeExternalIP),
				},
				HTTPTimeout: time.Duration(5) * time.Second,
			},
			ServiceNodePortRange: kubeoptions.DefaultServiceNodePortRange,
			MasterCount:          1,
		},
	}

	s.Options.SystemNamespaces = append(s.Options.SystemNamespaces, v1.NamespaceNodeLease)

	return &s
}

// Flags returns flags for a specific APIServer by section name
func (s *ServerRunOptions) Flags() (fss cliflag.NamedFlagSets) {
	s.Options.AddFlags(&fss)
	s.CloudProvider.AddFlags(fss.FlagSet("cloud provider"))

	// Note: the weird ""+ in below lines seems to be the only way to get gofmt to
	// arrange these text blocks sensibly. Grrr.
	fs := fss.FlagSet("misc")

	fs.BoolVar(&s.AllowPrivileged, "allow-privileged", s.AllowPrivileged,
		"If true, allow privileged containers. [default=false]")

	fs.StringVar(&s.EndpointReconcilerType, "endpoint-reconciler-type", s.EndpointReconcilerType,
		"Use an endpoint reconciler ("+strings.Join(reconcilers.AllTypes.Names(), ", ")+") master-count is deprecated, and will be removed in a future version.")

	// See #14282 for details on how to test/try this option out.
	// TODO: remove this comment once this option is tested in CI.
	fs.IntVar(&s.KubernetesServiceNodePort, "kubernetes-service-node-port", s.KubernetesServiceNodePort, ""+
		"If non-zero, the Kubernetes master service (which apiserver creates/maintains) will be "+
		"of type NodePort, using this as the value of the port. If zero, the Kubernetes master "+
		"service will be of type ClusterIP.")

	fs.StringVar(&s.ServiceClusterIPRanges, "service-cluster-ip-range", s.ServiceClusterIPRanges, ""+
		"A CIDR notation IP range from which to assign service cluster IPs. This must not "+
		"overlap with any IP ranges assigned to nodes or pods. Max of two dual-stack CIDRs is allowed.")

	fs.Var(&s.ServiceNodePortRange, "service-node-port-range", ""+
		"A port range to reserve for services with NodePort visibility.  This must not overlap with the ephemeral port range on nodes.  "+
		"Example: '30000-32767'. Inclusive at both ends of the range.")

	// Kubelet related flags:
	fs.StringSliceVar(&s.KubeletConfig.PreferredAddressTypes, "kubelet-preferred-address-types", s.KubeletConfig.PreferredAddressTypes,
		"List of the preferred NodeAddressTypes to use for kubelet connections.")

	fs.UintVar(&s.KubeletConfig.Port, "kubelet-port", s.KubeletConfig.Port,
		"DEPRECATED: kubelet port.")
	fs.MarkDeprecated("kubelet-port", "kubelet-port is deprecated and will be removed.")

	fs.UintVar(new(uint), "kubelet-read-only-port", 0,
		"DEPRECATED: kubelet read only port. Value ignored even if set.")
	fs.MarkDeprecated("kubelet-read-only-port", "kubelet-read-only-port is deprecated, ignored and will be removed.") //nolint:errcheck

	fs.DurationVar(&s.KubeletConfig.HTTPTimeout, "kubelet-timeout", s.KubeletConfig.HTTPTimeout,
		"Timeout for kubelet operations.")

	fs.StringVar(&s.KubeletConfig.TLSClientConfig.CertFile, "kubelet-client-certificate", s.KubeletConfig.TLSClientConfig.CertFile,
		"Path to a client cert file for TLS.")

	fs.StringVar(&s.KubeletConfig.TLSClientConfig.KeyFile, "kubelet-client-key", s.KubeletConfig.TLSClientConfig.KeyFile,
		"Path to a client key file for TLS.")

	fs.StringVar(&s.KubeletConfig.TLSClientConfig.CAFile, "kubelet-certificate-authority", s.KubeletConfig.TLSClientConfig.CAFile,
		"Path to a cert file for the certificate authority.")

	fs.IntVar(&s.MasterCount, "apiserver-count", s.MasterCount,
		"The number of apiservers running in the cluster, must be a positive number. (In use when --endpoint-reconciler-type=master-count is enabled.)")
	fs.MarkDeprecated("apiserver-count", "apiserver-count is deprecated and will be removed in a future version.")

	return fss
}
