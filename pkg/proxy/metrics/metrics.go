/*
Copyright 2017 The Kubernetes Authors.

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

package metrics

import (
	"context"
	"fmt"
	"sync"
	"time"

	"k8s.io/component-base/metrics"
	"k8s.io/component-base/metrics/legacyregistry"
	"k8s.io/klog/v2"
	kubeproxyconfig "k8s.io/kubernetes/pkg/proxy/apis/config"
	"k8s.io/kubernetes/pkg/proxy/util/nfacct"
	"sigs.k8s.io/knftables"
)

const kubeProxySubsystem = "kubeproxy"

var (
	// SyncProxyRulesLatency is the latency of one round of kube-proxy syncing proxy
	// rules. (With the iptables proxy, this includes both full and partial syncs.)
	SyncProxyRulesLatency = metrics.NewHistogramVec(
		&metrics.HistogramOpts{
			Subsystem:      kubeProxySubsystem,
			Name:           "sync_proxy_rules_duration_seconds",
			Help:           "SyncProxyRules latency in seconds",
			Buckets:        metrics.ExponentialBuckets(0.001, 2, 15),
			StabilityLevel: metrics.ALPHA,
		},
		[]string{"ip_family"},
	)

	// SyncFullProxyRulesLatency is the latency of one round of full rule syncing.
	SyncFullProxyRulesLatency = metrics.NewHistogramVec(
		&metrics.HistogramOpts{
			Subsystem:      kubeProxySubsystem,
			Name:           "sync_full_proxy_rules_duration_seconds",
			Help:           "SyncProxyRules latency in seconds for full resyncs",
			Buckets:        metrics.ExponentialBuckets(0.001, 2, 15),
			StabilityLevel: metrics.ALPHA,
		},
		[]string{"ip_family"},
	)

	// SyncPartialProxyRulesLatency is the latency of one round of partial rule syncing.
	SyncPartialProxyRulesLatency = metrics.NewHistogramVec(
		&metrics.HistogramOpts{
			Subsystem:      kubeProxySubsystem,
			Name:           "sync_partial_proxy_rules_duration_seconds",
			Help:           "SyncProxyRules latency in seconds for partial resyncs",
			Buckets:        metrics.ExponentialBuckets(0.001, 2, 15),
			StabilityLevel: metrics.ALPHA,
		},
		[]string{"ip_family"},
	)

	// SyncProxyRulesLastTimestamp is the timestamp proxy rules were last
	// successfully synced.
	SyncProxyRulesLastTimestamp = metrics.NewGaugeVec(
		&metrics.GaugeOpts{
			Subsystem:      kubeProxySubsystem,
			Name:           "sync_proxy_rules_last_timestamp_seconds",
			Help:           "The last time proxy rules were successfully synced",
			StabilityLevel: metrics.ALPHA,
		},
		[]string{"ip_family"},
	)

	// NetworkProgrammingLatency is defined as the time it took to program the network - from the time
	// the service or pod has changed to the time the change was propagated and the proper kube-proxy
	// rules were synced. Exported for each endpoints object that were part of the rules sync.
	// See https://github.com/kubernetes/community/blob/master/sig-scalability/slos/network_programming_latency.md
	// Note that the metrics is partially based on the time exported by the endpoints controller on
	// the master machine. The measurement may be inaccurate if there is a clock drift between the
	// node and master machine.
	NetworkProgrammingLatency = metrics.NewHistogramVec(
		&metrics.HistogramOpts{
			Subsystem: kubeProxySubsystem,
			Name:      "network_programming_duration_seconds",
			Help:      "In Cluster Network Programming Latency in seconds",
			Buckets: metrics.MergeBuckets(
				metrics.LinearBuckets(0.25, 0.25, 2), // 0.25s, 0.50s
				metrics.LinearBuckets(1, 1, 59),      // 1s, 2s, 3s, ... 59s
				metrics.LinearBuckets(60, 5, 12),     // 60s, 65s, 70s, ... 115s
				metrics.LinearBuckets(120, 30, 7),    // 2min, 2.5min, 3min, ..., 5min
			),
			StabilityLevel: metrics.ALPHA,
		},
		[]string{"ip_family"},
	)

	// EndpointChangesPending is the number of pending endpoint changes that
	// have not yet been synced to the proxy.
	EndpointChangesPending = metrics.NewGauge(
		&metrics.GaugeOpts{
			Subsystem:      kubeProxySubsystem,
			Name:           "sync_proxy_rules_endpoint_changes_pending",
			Help:           "Pending proxy rules Endpoint changes",
			StabilityLevel: metrics.ALPHA,
		},
	)

	// EndpointChangesTotal is the number of endpoint changes that the proxy
	// has seen.
	EndpointChangesTotal = metrics.NewCounter(
		&metrics.CounterOpts{
			Subsystem:      kubeProxySubsystem,
			Name:           "sync_proxy_rules_endpoint_changes_total",
			Help:           "Cumulative proxy rules Endpoint changes",
			StabilityLevel: metrics.ALPHA,
		},
	)

	// ServiceChangesPending is the number of pending service changes that
	// have not yet been synced to the proxy.
	ServiceChangesPending = metrics.NewGauge(
		&metrics.GaugeOpts{
			Subsystem:      kubeProxySubsystem,
			Name:           "sync_proxy_rules_service_changes_pending",
			Help:           "Pending proxy rules Service changes",
			StabilityLevel: metrics.ALPHA,
		},
	)

	// ServiceChangesTotal is the number of service changes that the proxy has
	// seen.
	ServiceChangesTotal = metrics.NewCounter(
		&metrics.CounterOpts{
			Subsystem:      kubeProxySubsystem,
			Name:           "sync_proxy_rules_service_changes_total",
			Help:           "Cumulative proxy rules Service changes",
			StabilityLevel: metrics.ALPHA,
		},
	)

	// iptablesCTStateInvalidDroppedPacketsDescription describe the metrics for the number of packets dropped
	// by iptables which were marked INVALID by conntrack.
	iptablesCTStateInvalidDroppedPacketsDescription = metrics.NewDesc(
		"kubeproxy_iptables_ct_state_invalid_dropped_packets_total",
		"packets dropped by iptables to work around conntrack problems",
		nil, nil, metrics.ALPHA, "")
	IPTablesCTStateInvalidDroppedNFAcctCounter = "ct_state_invalid_dropped_pkts"

	// IPTablesRestoreFailuresTotal is the number of iptables restore failures that the proxy has
	// seen.
	IPTablesRestoreFailuresTotal = metrics.NewCounterVec(
		&metrics.CounterOpts{
			Subsystem:      kubeProxySubsystem,
			Name:           "sync_proxy_rules_iptables_restore_failures_total",
			Help:           "Cumulative proxy iptables restore failures",
			StabilityLevel: metrics.ALPHA,
		},
		[]string{"ip_family"},
	)

	// IPTablesPartialRestoreFailuresTotal is the number of iptables *partial* restore
	// failures (resulting in a fall back to a full restore) that the proxy has seen.
	IPTablesPartialRestoreFailuresTotal = metrics.NewCounterVec(
		&metrics.CounterOpts{
			Subsystem:      kubeProxySubsystem,
			Name:           "sync_proxy_rules_iptables_partial_restore_failures_total",
			Help:           "Cumulative proxy iptables partial restore failures",
			StabilityLevel: metrics.ALPHA,
		},
		[]string{"ip_family"},
	)

	// IPTablesRulesTotal is the total number of iptables rules that the iptables
	// proxy has installed.
	IPTablesRulesTotal = metrics.NewGaugeVec(
		&metrics.GaugeOpts{
			Subsystem:      kubeProxySubsystem,
			Name:           "sync_proxy_rules_iptables_total",
			Help:           "Total number of iptables rules owned by kube-proxy",
			StabilityLevel: metrics.ALPHA,
		},
		[]string{"table", "ip_family"},
	)

	// IPTablesRulesLastSync is the number of iptables rules that the iptables proxy
	// updated in the last sync.
	IPTablesRulesLastSync = metrics.NewGaugeVec(
		&metrics.GaugeOpts{
			Subsystem:      kubeProxySubsystem,
			Name:           "sync_proxy_rules_iptables_last",
			Help:           "Number of iptables rules written by kube-proxy in last sync",
			StabilityLevel: metrics.ALPHA,
		},
		[]string{"table", "ip_family"},
	)

	// NFTablesSyncFailuresTotal is the number of nftables sync failures that the
	// proxy has seen.
	NFTablesSyncFailuresTotal = metrics.NewCounterVec(
		&metrics.CounterOpts{
			Subsystem:      kubeProxySubsystem,
			Name:           "sync_proxy_rules_nftables_sync_failures_total",
			Help:           "Cumulative proxy nftables sync failures",
			StabilityLevel: metrics.ALPHA,
		},
		[]string{"ip_family"},
	)

	// NFTablesCleanupFailuresTotal is the number of nftables stale chain cleanup
	// failures that the proxy has seen.
	NFTablesCleanupFailuresTotal = metrics.NewCounterVec(
		&metrics.CounterOpts{
			Subsystem:      kubeProxySubsystem,
			Name:           "sync_proxy_rules_nftables_cleanup_failures_total",
			Help:           "Cumulative proxy nftables cleanup failures",
			StabilityLevel: metrics.ALPHA,
		},
		[]string{"ip_family"},
	)

	// ProxyHealthzTotal is the number of returned HTTP Status for each
	// healthz probe.
	ProxyHealthzTotal = metrics.NewCounterVec(
		&metrics.CounterOpts{
			Subsystem:      kubeProxySubsystem,
			Name:           "proxy_healthz_total",
			Help:           "Cumulative proxy healthz HTTP status",
			StabilityLevel: metrics.ALPHA,
		},
		[]string{"code"},
	)

	// ProxyLivezTotal is the number of returned HTTP Status for each
	// livez probe.
	ProxyLivezTotal = metrics.NewCounterVec(
		&metrics.CounterOpts{
			Subsystem:      kubeProxySubsystem,
			Name:           "proxy_livez_total",
			Help:           "Cumulative proxy livez HTTP status",
			StabilityLevel: metrics.ALPHA,
		},
		[]string{"code"},
	)

	// SyncProxyRulesLastQueuedTimestamp is the last time a proxy sync was
	// requested. If this is much larger than
	// kubeproxy_sync_proxy_rules_last_timestamp_seconds, then something is hung.
	SyncProxyRulesLastQueuedTimestamp = metrics.NewGaugeVec(
		&metrics.GaugeOpts{
			Subsystem:      kubeProxySubsystem,
			Name:           "sync_proxy_rules_last_queued_timestamp_seconds",
			Help:           "The last time a sync of proxy rules was queued",
			StabilityLevel: metrics.ALPHA,
		},
		[]string{"ip_family"},
	)

	// SyncProxyRulesNoLocalEndpointsTotal is the total number of rules that do
	// not have an available endpoint. This can be caused by an internal
	// traffic policy with no available local workload.
	SyncProxyRulesNoLocalEndpointsTotal = metrics.NewGaugeVec(
		&metrics.GaugeOpts{
			Subsystem:      kubeProxySubsystem,
			Name:           "sync_proxy_rules_no_local_endpoints_total",
			Help:           "Number of services with a Local traffic policy and no endpoints",
			StabilityLevel: metrics.ALPHA,
		},
		[]string{"traffic_policy", "ip_family"},
	)

	// localhostNodePortsAcceptedPacketsDescription describe the metrics for the number of packets accepted
	// by iptables which were destined for nodeports on loopback interface.
	localhostNodePortsAcceptedPacketsDescription = metrics.NewDesc(
		"kubeproxy_iptables_localhost_nodeports_accepted_packets_total",
		"Number of packets accepted on nodeports of loopback interface",
		nil, nil, metrics.ALPHA, "")
	LocalhostNodePortAcceptedNFAcctCounter = "localhost_nps_accepted_pkts"

	// trafficToInvalidServicePortsDescription describe the metrics for the number of packets rejected
	// by nftables which were destined for invalid ports of the ClustersIPs.
	trafficToInvalidServicePortsDescription = metrics.NewDesc(
		"kubeproxy_nftables_packets_to_invalid_ports_rejected",
		"Number of packets rejected on invalid ports of ClusterIPs",
		nil, nil, metrics.ALPHA, "")
	TrafficToInvalidServicePortsCounter = "traffic-to-invalid-ports"
)

var registerMetricsOnce sync.Once

// RegisterMetrics registers kube-proxy metrics.
func RegisterMetrics(mode kubeproxyconfig.ProxyMode) {
	registerMetricsOnce.Do(func() {
		// Core kube-proxy metrics for all backends
		legacyregistry.MustRegister(SyncProxyRulesLatency)
		legacyregistry.MustRegister(SyncProxyRulesLastQueuedTimestamp)
		legacyregistry.MustRegister(SyncProxyRulesLastTimestamp)
		legacyregistry.MustRegister(EndpointChangesPending)
		legacyregistry.MustRegister(EndpointChangesTotal)
		legacyregistry.MustRegister(ServiceChangesPending)
		legacyregistry.MustRegister(ServiceChangesTotal)
		legacyregistry.MustRegister(ProxyHealthzTotal)
		legacyregistry.MustRegister(ProxyLivezTotal)

		// FIXME: winkernel does not implement these
		legacyregistry.MustRegister(NetworkProgrammingLatency)
		legacyregistry.MustRegister(SyncProxyRulesNoLocalEndpointsTotal)

		switch mode {
		case kubeproxyconfig.ProxyModeIPTables:
			iptablesCTStateInvalidDroppedMetricCollector := newNFAcctMetricCollector(IPTablesCTStateInvalidDroppedNFAcctCounter, iptablesCTStateInvalidDroppedPacketsDescription)
			if iptablesCTStateInvalidDroppedMetricCollector != nil {
				legacyregistry.CustomMustRegister(iptablesCTStateInvalidDroppedMetricCollector)
			}
			localhostNodePortsAcceptedMetricsCollector := newNFAcctMetricCollector(LocalhostNodePortAcceptedNFAcctCounter, localhostNodePortsAcceptedPacketsDescription)
			if localhostNodePortsAcceptedMetricsCollector != nil {
				legacyregistry.CustomMustRegister(localhostNodePortsAcceptedMetricsCollector)
			}
			legacyregistry.MustRegister(SyncFullProxyRulesLatency)
			legacyregistry.MustRegister(SyncPartialProxyRulesLatency)
			legacyregistry.MustRegister(IPTablesRestoreFailuresTotal)
			legacyregistry.MustRegister(IPTablesPartialRestoreFailuresTotal)
			legacyregistry.MustRegister(IPTablesRulesTotal)
			legacyregistry.MustRegister(IPTablesRulesLastSync)

		case kubeproxyconfig.ProxyModeIPVS:
			legacyregistry.MustRegister(IPTablesRestoreFailuresTotal)

		case kubeproxyconfig.ProxyModeNFTables:
			legacyregistry.MustRegister(SyncFullProxyRulesLatency)
			legacyregistry.MustRegister(SyncPartialProxyRulesLatency)
			legacyregistry.MustRegister(NFTablesSyncFailuresTotal)
			legacyregistry.MustRegister(NFTablesCleanupFailuresTotal)
			trafficToInvalidServicePortsDescriptionMetricsCollector := newNFTablesMetricCollector(TrafficToInvalidServicePortsCounter, trafficToInvalidServicePortsDescription)
			if trafficToInvalidServicePortsDescriptionMetricsCollector != nil {
				legacyregistry.CustomMustRegister(trafficToInvalidServicePortsDescriptionMetricsCollector)
			}
		case kubeproxyconfig.ProxyModeKernelspace:
			// currently no winkernel-specific metrics
		}
	})
}

// SinceInSeconds gets the time since the specified start in seconds.
func SinceInSeconds(start time.Time) float64 {
	return time.Since(start).Seconds()
}

var _ metrics.StableCollector = &nfacctMetricCollector{}

func newNFAcctMetricCollector(counter string, description *metrics.Desc) *nfacctMetricCollector {
	client, err := nfacct.New()
	if err != nil {
		klog.ErrorS(err, "failed to initialize nfacct client")
		return nil
	}
	return &nfacctMetricCollector{
		client:      client,
		counter:     counter,
		description: description,
	}
}

type nfacctMetricCollector struct {
	metrics.BaseStableCollector
	client      nfacct.Interface
	counter     string
	description *metrics.Desc
}

// DescribeWithStability implements the metrics.StableCollector interface.
func (n *nfacctMetricCollector) DescribeWithStability(ch chan<- *metrics.Desc) {
	ch <- n.description
}

// CollectWithStability implements the metrics.StableCollector interface.
func (n *nfacctMetricCollector) CollectWithStability(ch chan<- metrics.Metric) {
	if n.client != nil {
		counter, err := n.client.Get(n.counter)
		if err != nil {
			klog.ErrorS(err, "failed to collect nfacct counter", "counter", n.counter)
		} else {
			metric, err := metrics.NewConstMetric(n.description, metrics.CounterValue, float64(counter.Packets))
			if err != nil {
				klog.ErrorS(err, "failed to create constant metric")
			} else {
				ch <- metric
			}
		}
	}
}

type nftablesMetricCollector struct {
	metrics.BaseStableCollector
	clients     [2]knftables.Interface
	counter     string
	description *metrics.Desc
}

func newNFTablesMetricCollector(counter string, description *metrics.Desc) *nftablesMetricCollector {
	var clients [2]knftables.Interface
	for i, ipFamily := range []knftables.Family{knftables.IPv4Family, knftables.IPv6Family} {
		client, err := knftables.New(ipFamily, "kube-proxy")
		if err != nil {
			klog.ErrorS(err, "failed to initialize nftables client")
			return nil
		}
		clients[i] = client
	}

	return &nftablesMetricCollector{
		clients:     clients,
		counter:     counter,
		description: description,
	}
}

// DescribeWithStability implements the metrics.StableCollector interface.
func (n *nftablesMetricCollector) DescribeWithStability(ch chan<- *metrics.Desc) {
	ch <- n.description
}

// CollectWithStability implements the metrics.StableCollector interface.
func (n *nftablesMetricCollector) CollectWithStability(ch chan<- metrics.Metric) {
	getCounterPackets := func(client knftables.Interface, counterName string) (uint64, error) {
		counters, err := client.ListCounters(context.TODO())
		if err != nil {
			return 0, err
		}
		for _, counter := range counters {
			if counter.Name == counterName {
				return *counter.Packets, nil
			}
		}
		return 0, fmt.Errorf("counter '%s' not found", counterName)
	}

	if n.clients[0] != nil && n.clients[1] != nil {
		var packetsTotal uint64
		for _, client := range n.clients {
			packet, err := getCounterPackets(client, n.counter)
			if err != nil {
				klog.ErrorS(err, "failed to collect nftables counter", "counter", n.counter)
				return
			}
			packetsTotal += packet
		}

		metric, err := metrics.NewConstMetric(n.description, metrics.CounterValue, float64(packetsTotal))
		if err != nil {
			klog.ErrorS(err, "failed to create constant metric")
		} else {
			ch <- metric
		}
	}
}
