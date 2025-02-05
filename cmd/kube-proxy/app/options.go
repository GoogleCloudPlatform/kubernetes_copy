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

package app

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/spf13/pflag"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	cliflag "k8s.io/component-base/cli/flag"
	logsapi "k8s.io/component-base/logs/api/v1"
	zpagesfeatures "k8s.io/component-base/zpages/features"
	"k8s.io/component-base/zpages/flagz"
	"k8s.io/klog/v2"
	"k8s.io/kube-proxy/config/v1alpha1"
	"k8s.io/kubernetes/pkg/cluster/ports"
	"k8s.io/kubernetes/pkg/kubelet/qos"
	kubeproxyconfig "k8s.io/kubernetes/pkg/proxy/apis/config"
	proxyconfigscheme "k8s.io/kubernetes/pkg/proxy/apis/config/scheme"
	kubeproxyconfigv1alpha1 "k8s.io/kubernetes/pkg/proxy/apis/config/v1alpha1"
	"k8s.io/kubernetes/pkg/proxy/apis/config/validation"
	proxyutil "k8s.io/kubernetes/pkg/proxy/util"
	"k8s.io/kubernetes/pkg/util/filesystem"
	utilflag "k8s.io/kubernetes/pkg/util/flag"
	"k8s.io/utils/ptr"
)

// Options contains everything necessary to create and run a proxy server.
type Options struct {
	// ConfigFile is the location of the proxy server's configuration file.
	ConfigFile string
	// WriteConfigTo is the path where the default configuration will be written.
	WriteConfigTo string
	// CleanupAndExit, when true, makes the proxy server clean up iptables and ipvs rules, then exit.
	CleanupAndExit bool
	// InitAndExit, when true, makes the proxy server makes configurations that need privileged access, then exit.
	InitAndExit bool
	// config is the proxy server's configuration object.
	config *kubeproxyconfig.KubeProxyConfiguration
	// watcher is used to watch on the update change of ConfigFile
	watcher filesystem.FSWatcher
	// proxyServer is the interface to run the proxy server
	proxyServer proxyRun
	// errCh is the channel that errors will be sent
	errCh chan error
	// flagz is the Reader interface to get flags for the flagz page.
	flagz flagz.Reader

	// The fields below here are placeholders for flags that can't be directly mapped into
	// config.KubeProxyConfiguration.
	//
	// TODO remove these fields once the deprecated flags are removed.

	// master is used to override the kubeconfig's URL to the apiserver.
	master string
	// healthzPort is the port to be used by the healthz server.
	healthzPort int32
	// metricsPort is the port to be used by the metrics server.
	metricsPort int32

	// hostnameOverride, if set from the command line flag, takes precedence over the `HostnameOverride` value from the config file
	hostnameOverride string

	logger klog.Logger

	// The fields below here are placeholders for flags that can't be directly mapped into
	// config.KubeProxyConfiguration.
	iptablesSyncPeriod    time.Duration
	iptablesMinSyncPeriod time.Duration
	ipvsSyncPeriod        time.Duration
	ipvsMinSyncPeriod     time.Duration
	clusterCIDRs          string
}

// AddFlags adds flags to fs and binds them to options.
func (o *Options) AddFlags(fs *pflag.FlagSet) {
	o.addOSFlags(fs)

	fs.StringVar(&o.ConfigFile, "config", o.ConfigFile, "The path to the configuration file.")
	fs.StringVar(&o.WriteConfigTo, "write-config-to", o.WriteConfigTo, "If set, write the default configuration values to this file and exit.")

	fs.BoolVar(&o.CleanupAndExit, "cleanup", o.CleanupAndExit, "If true cleanup iptables and ipvs rules and exit.")

	fs.Var(cliflag.NewMapStringBool(&o.config.FeatureGates), "feature-gates", "A set of key=value pairs that describe feature gates for alpha/experimental features. "+
		"Options are:\n"+strings.Join(utilfeature.DefaultFeatureGate.KnownFeatures(), "\n")+"\n"+
		"This parameter is ignored if a config file is specified by --config.")

	fs.StringVar(&o.config.ClientConnection.Kubeconfig, "kubeconfig", o.config.ClientConnection.Kubeconfig, "Path to kubeconfig file with authorization information (the master location can be overridden by the master flag).")
	fs.StringVar(&o.master, "master", o.master, "The address of the Kubernetes API server (overrides any value in kubeconfig)")
	fs.StringVar(&o.config.ClientConnection.ContentType, "kube-api-content-type", o.config.ClientConnection.ContentType, "Content type of requests sent to apiserver.")
	fs.Int32Var(&o.config.ClientConnection.Burst, "kube-api-burst", o.config.ClientConnection.Burst, "Burst to use while talking with kubernetes apiserver")
	fs.Float32Var(&o.config.ClientConnection.QPS, "kube-api-qps", o.config.ClientConnection.QPS, "QPS to use while talking with kubernetes apiserver")

	fs.StringVar(&o.hostnameOverride, "hostname-override", o.hostnameOverride, "If non-empty, will be used as the name of the Node that kube-proxy is running on. If unset, the node name is assumed to be the same as the node's hostname.")
	fs.Var(&utilflag.IPVar{Val: &o.config.BindAddress}, "bind-address", "Overrides kube-proxy's idea of what its node's primary IP is. Note that the name is a historical artifact, and kube-proxy does not actually bind any sockets to this IP. This parameter is ignored if a config file is specified by --config.")
	fs.Var(&utilflag.IPPortVar{Val: &o.config.HealthzBindAddress}, "healthz-bind-address", "The IP address and port for the health check server to serve on, defaulting to \"0.0.0.0:10256\". This parameter is ignored if a config file is specified by --config.")
	fs.Var(&utilflag.IPPortVar{Val: &o.config.MetricsBindAddress}, "metrics-bind-address", "The IP address and port for the metrics server to serve on, defaulting to \"127.0.0.1:10249\". (Set to \"0.0.0.0:10249\" / \"[::]:10249\" to bind on all interfaces.) Set empty to disable. This parameter is ignored if a config file is specified by --config.")
	fs.BoolVar(&o.config.BindAddressHardFail, "bind-address-hard-fail", o.config.BindAddressHardFail, "If true kube-proxy will treat failure to bind to a port as fatal and exit")
	fs.BoolVar(&o.config.EnableProfiling, "profiling", o.config.EnableProfiling, "If true enables profiling via web interface on /debug/pprof handler. This parameter is ignored if a config file is specified by --config.")
	fs.StringVar(&o.config.ShowHiddenMetricsForVersion, "show-hidden-metrics-for-version", o.config.ShowHiddenMetricsForVersion,
		"The previous version for which you want to show hidden metrics. "+
			"Only the previous minor version is meaningful, other values will not be allowed. "+
			"The format is <major>.<minor>, e.g.: '1.16'. "+
			"The purpose of this format is make sure you have the opportunity to notice if the next release hides additional metrics, "+
			"rather than being surprised when they are permanently removed in the release after that. "+
			"This parameter is ignored if a config file is specified by --config.")
	fs.BoolVar(&o.InitAndExit, "init-only", o.InitAndExit, "If true, perform any initialization steps that must be done with full root privileges, and then exit. After doing this, you can run kube-proxy again with only the CAP_NET_ADMIN capability.")
	fs.Var(&o.config.Mode, "proxy-mode", "Which proxy mode to use: on Linux this can be 'iptables' (default), 'ipvs', or 'nftables'. On Windows the only supported value is 'kernelspace'."+
		"This parameter is ignored if a config file is specified by --config.")

	fs.Int32Var(o.config.IPTables.MasqueradeBit, "iptables-masquerade-bit", ptr.Deref(o.config.IPTables.MasqueradeBit, 14), "If using the iptables or ipvs proxy mode, the bit of the fwmark space to mark packets requiring SNAT with.  Must be within the range [0, 31].")
	fs.BoolVar(&o.config.Linux.MasqueradeAll, "masquerade-all", o.config.Linux.MasqueradeAll, "SNAT all traffic sent via Service cluster IPs. This may be required with some CNI plugins. Only supported on Linux.")
	fs.BoolVar(o.config.IPTables.LocalhostNodePorts, "iptables-localhost-nodeports", ptr.Deref(o.config.IPTables.LocalhostNodePorts, true), "If false, kube-proxy will disable the legacy behavior of allowing NodePort services to be accessed via localhost. (Applies only to iptables mode and IPv4; localhost NodePorts are never allowed with other proxy modes or with IPv6.)")
	fs.DurationVar(&o.iptablesSyncPeriod, "iptables-sync-period", o.config.SyncPeriod.Duration, "An interval (e.g. '5s', '1m', '2h22m') indicating how frequently various re-synchronizing and cleanup operations are performed. Must be greater than 0.")
	fs.DurationVar(&o.iptablesMinSyncPeriod, "iptables-min-sync-period", o.config.MinSyncPeriod.Duration, "The minimum period between iptables rule resyncs (e.g. '5s', '1m', '2h22m'). A value of 0 means every Service or EndpointSlice change will result in an immediate iptables resync.")

	fs.DurationVar(&o.ipvsSyncPeriod, "ipvs-sync-period", o.config.SyncPeriod.Duration, "An interval (e.g. '5s', '1m', '2h22m') indicating how frequently various re-synchronizing and cleanup operations are performed. Must be greater than 0.")
	fs.DurationVar(&o.ipvsMinSyncPeriod, "ipvs-min-sync-period", o.config.MinSyncPeriod.Duration, "The minimum period between IPVS rule resyncs (e.g. '5s', '1m', '2h22m'). A value of 0 means every Service or EndpointSlice change will result in an immediate IPVS resync.")
	fs.StringVar(&o.config.IPVS.Scheduler, "ipvs-scheduler", o.config.IPVS.Scheduler, "The ipvs scheduler type when proxy mode is ipvs")
	fs.StringSliceVar(&o.config.IPVS.ExcludeCIDRs, "ipvs-exclude-cidrs", o.config.IPVS.ExcludeCIDRs, "A comma-separated list of CIDRs which the ipvs proxier should not touch when cleaning up IPVS rules.")
	fs.BoolVar(&o.config.IPVS.StrictARP, "ipvs-strict-arp", o.config.IPVS.StrictARP, "Enable strict ARP by setting arp_ignore to 1 and arp_announce to 2")
	fs.DurationVar(&o.config.IPVS.TCPTimeout.Duration, "ipvs-tcp-timeout", o.config.IPVS.TCPTimeout.Duration, "The timeout for idle IPVS TCP connections, 0 to leave as-is. (e.g. '5s', '1m', '2h22m').")
	fs.DurationVar(&o.config.IPVS.TCPFinTimeout.Duration, "ipvs-tcpfin-timeout", o.config.IPVS.TCPFinTimeout.Duration, "The timeout for IPVS TCP connections after receiving a FIN packet, 0 to leave as-is. (e.g. '5s', '1m', '2h22m').")
	fs.DurationVar(&o.config.IPVS.UDPTimeout.Duration, "ipvs-udp-timeout", o.config.IPVS.UDPTimeout.Duration, "The timeout for IPVS UDP packets, 0 to leave as-is. (e.g. '5s', '1m', '2h22m').")

	fs.Var(&o.config.DetectLocalMode, "detect-local-mode", "Mode to use to detect local traffic. This parameter is ignored if a config file is specified by --config.")
	fs.StringVar(&o.config.DetectLocal.BridgeInterface, "pod-bridge-interface", o.config.DetectLocal.BridgeInterface, "A bridge interface name. When --detect-local-mode is set to BridgeInterface, kube-proxy will consider traffic to be local if it originates from this bridge.")
	fs.StringVar(&o.config.DetectLocal.InterfaceNamePrefix, "pod-interface-name-prefix", o.config.DetectLocal.InterfaceNamePrefix, "An interface name prefix. When --detect-local-mode is set to InterfaceNamePrefix, kube-proxy will consider traffic to be local if it originates from any interface whose name begins with this prefix.")
	fs.StringVar(&o.clusterCIDRs, "cluster-cidr", strings.Join(o.config.DetectLocal.ClusterCIDRs, ","), "The CIDR range of the pods in the cluster. (For dual-stack clusters, this can be a comma-separated dual-stack pair of CIDR ranges.). When --detect-local-mode is set to ClusterCIDR, kube-proxy will consider traffic to be local if its source IP is in this range. (Otherwise it is not used.) "+
		"This parameter is ignored if a config file is specified by --config.")

	fs.StringSliceVar(&o.config.NodePortAddresses, "nodeport-addresses", o.config.NodePortAddresses,
		"A list of CIDR ranges that contain valid node IPs, or alternatively, the single string 'primary'. If set to a list of CIDRs, connections to NodePort services will only be accepted on node IPs in one of the indicated ranges. If set to 'primary', NodePort services will only be accepted on the node's primary IP(s) according to the Node object. If unset, NodePort connections will be accepted on all local IPs. This parameter is ignored if a config file is specified by --config.")

	fs.Int32Var(o.config.Linux.OOMScoreAdj, "oom-score-adj", ptr.Deref(o.config.Linux.OOMScoreAdj, int32(qos.KubeProxyOOMScoreAdj)), "The oom-score-adj value for kube-proxy process. Values must be within the range [-1000, 1000]. This parameter is ignored if a config file is specified by --config.")
	fs.Int32Var(o.config.Linux.Conntrack.MaxPerCore, "conntrack-max-per-core", *o.config.Linux.Conntrack.MaxPerCore,
		"Maximum number of NAT connections to track per CPU core (0 to leave the limit as-is and ignore conntrack-min).")
	fs.Int32Var(o.config.Linux.Conntrack.Min, "conntrack-min", *o.config.Linux.Conntrack.Min,
		"Minimum number of conntrack entries to allocate, regardless of conntrack-max-per-core (set conntrack-max-per-core=0 to leave the limit as-is).")

	fs.DurationVar(&o.config.Linux.Conntrack.TCPEstablishedTimeout.Duration, "conntrack-tcp-timeout-established", o.config.Linux.Conntrack.TCPEstablishedTimeout.Duration, "Idle timeout for established TCP connections (0 to leave as-is)")
	fs.DurationVar(
		&o.config.Linux.Conntrack.TCPCloseWaitTimeout.Duration, "conntrack-tcp-timeout-close-wait",
		o.config.Linux.Conntrack.TCPCloseWaitTimeout.Duration,
		"NAT timeout for TCP connections in the CLOSE_WAIT state")
	fs.BoolVar(&o.config.Linux.Conntrack.TCPBeLiberal, "conntrack-tcp-be-liberal", o.config.Linux.Conntrack.TCPBeLiberal, "Enable liberal mode for tracking TCP packets by setting nf_conntrack_tcp_be_liberal to 1")
	fs.DurationVar(&o.config.Linux.Conntrack.UDPTimeout.Duration, "conntrack-udp-timeout", o.config.Linux.Conntrack.UDPTimeout.Duration, "Idle timeout for UNREPLIED UDP connections (0 to leave as-is)")
	fs.DurationVar(&o.config.Linux.Conntrack.UDPStreamTimeout.Duration, "conntrack-udp-timeout-stream", o.config.Linux.Conntrack.UDPStreamTimeout.Duration, "Idle timeout for ASSURED UDP connections (0 to leave as-is)")
	fs.DurationVar(&o.config.ConfigSyncPeriod.Duration, "config-sync-period", o.config.ConfigSyncPeriod.Duration, "How often configuration from the apiserver is refreshed.  Must be greater than 0.")

	fs.Int32Var(&o.healthzPort, "healthz-port", o.healthzPort, "The port to bind the health check server. Use 0 to disable.")
	_ = fs.MarkDeprecated("healthz-port", "This flag is deprecated and will be removed in a future release. Please use --healthz-bind-address instead.")
	fs.Int32Var(&o.metricsPort, "metrics-port", o.metricsPort, "The port to bind the metrics server. Use 0 to disable.")
	_ = fs.MarkDeprecated("metrics-port", "This flag is deprecated and will be removed in a future release. Please use --metrics-bind-address instead.")

	fs.Int32Var(o.config.IPVS.IPSet.HashSize, "ipvs-ipset-hashsize", *o.config.IPVS.IPSet.HashSize, "The hash size for ipset used by IPVS proxier. Must be greater than 0.")
	fs.Int32Var(o.config.IPVS.IPSet.MaxElements, "ipvs-ipset-maxelements", *o.config.IPVS.IPSet.MaxElements, "The maximal number of elements that can be stored in an ipset used by IPVS proxier. Must be greater than 0.")
	logsapi.AddFlags(&o.config.Logging, fs)
}

// newKubeProxyConfiguration returns a KubeProxyConfiguration with default values
func newKubeProxyConfiguration() *kubeproxyconfig.KubeProxyConfiguration {
	versionedConfig := &v1alpha1.KubeProxyConfiguration{}
	proxyconfigscheme.Scheme.Default(versionedConfig)
	internalConfig, err := proxyconfigscheme.Scheme.ConvertToVersion(versionedConfig, kubeproxyconfig.SchemeGroupVersion)
	if err != nil {
		panic(fmt.Sprintf("Unable to create default config: %v", err))
	}

	return internalConfig.(*kubeproxyconfig.KubeProxyConfiguration)
}

// NewOptions returns initialized Options
func NewOptions() *Options {
	return &Options{
		config:      newKubeProxyConfiguration(),
		healthzPort: ports.ProxyHealthzPort,
		metricsPort: ports.ProxyStatusPort,
		errCh:       make(chan error),
		logger:      klog.FromContext(context.Background()),
	}
}

// Complete completes all the required options.
func (o *Options) Complete(fs *pflag.FlagSet) error {
	if len(o.ConfigFile) == 0 && len(o.WriteConfigTo) == 0 {
		o.config.HealthzBindAddress = addressFromDeprecatedFlags(o.config.HealthzBindAddress, o.healthzPort)
		o.config.MetricsBindAddress = addressFromDeprecatedFlags(o.config.MetricsBindAddress, o.metricsPort)
	}

	// Load the config file here in Complete, so that Validate validates the fully-resolved config.
	if len(o.ConfigFile) > 0 {
		c, err := o.loadConfigFromFile(o.ConfigFile)
		if err != nil {
			return err
		}

		// Before we overwrite the config which holds the parsed
		// command line parameters, we need to copy all modified
		// logging settings over to the loaded config (i.e.  logging
		// command line flags have priority). Otherwise `--config
		// ... -v=5` doesn't work (config resets verbosity even
		// when it contains no logging settings).
		_ = copyLogsFromFlags(fs, &c.Logging)
		o.config = c

		if err := o.initWatcher(); err != nil {
			return err
		}
	} else {
		o.processV1Alpha1Flags(fs)
	}

	o.platformApplyDefaults(o.config)

	if err := o.processHostnameOverrideFlag(); err != nil {
		return err
	}

	if err := utilfeature.DefaultMutableFeatureGate.SetFromMap(o.config.FeatureGates); err != nil {
		return err
	}

	if utilfeature.DefaultFeatureGate.Enabled(zpagesfeatures.ComponentFlagz) {
		nfs := cliflag.NamedFlagSets{
			FlagSets: make(map[string]*pflag.FlagSet),
		}
		nfs.FlagSets["generic"] = fs
		o.flagz = flagz.NamedFlagSetsReader{
			FlagSets: nfs,
		}
	}

	return nil
}

// copyLogsFromFlags applies the logging flags from the given flag set to the given
// configuration. Fields for which the corresponding flag was not used are left
// unmodified. For fields that have multiple values (like vmodule), the values from
// the flags get joined so that the command line flags have priority.
//
// TODO (pohly): move this to logsapi
func copyLogsFromFlags(from *pflag.FlagSet, to *logsapi.LoggingConfiguration) error {
	var cloneFS pflag.FlagSet
	logsapi.AddFlags(to, &cloneFS)
	vmodule := to.VModule
	to.VModule = nil
	var err error
	cloneFS.VisitAll(func(f *pflag.Flag) {
		if err != nil {
			return
		}
		fsFlag := from.Lookup(f.Name)
		if fsFlag == nil {
			err = fmt.Errorf("logging flag %s not found in flag set", f.Name)
			return
		}
		if !fsFlag.Changed {
			return
		}
		if setErr := f.Value.Set(fsFlag.Value.String()); setErr != nil {
			err = fmt.Errorf("copying flag %s value: %w", f.Name, setErr)
			return
		}
	})
	to.VModule = append(to.VModule, vmodule...)
	return err
}

// Creates a new filesystem watcher and adds watches for the config file.
func (o *Options) initWatcher() error {
	fswatcher := filesystem.NewFsnotifyWatcher()
	err := fswatcher.Init(o.eventHandler, o.errorHandler)
	if err != nil {
		return err
	}
	err = fswatcher.AddWatch(o.ConfigFile)
	if err != nil {
		return err
	}
	o.watcher = fswatcher
	return nil
}

func (o *Options) eventHandler(ent fsnotify.Event) {
	if ent.Has(fsnotify.Write) || ent.Has(fsnotify.Rename) {
		// error out when ConfigFile is updated
		o.errCh <- fmt.Errorf("content of the proxy server's configuration file was updated")
		return
	}
	o.errCh <- nil
}

func (o *Options) errorHandler(err error) {
	o.errCh <- err
}

// processHostnameOverrideFlag processes hostname-override flag
func (o *Options) processHostnameOverrideFlag() error {
	// Check if hostname-override flag is set and use value since configFile always overrides
	if len(o.hostnameOverride) > 0 {
		hostName := strings.TrimSpace(o.hostnameOverride)
		if len(hostName) == 0 {
			return fmt.Errorf("empty hostname-override is invalid")
		}
		o.config.HostnameOverride = strings.ToLower(hostName)
	}

	return nil
}

// processV1Alpha1Flags processes v1alpha1 flags which can't be directly mapped to internal config.
func (o *Options) processV1Alpha1Flags(fs *pflag.FlagSet) {
	if fs.Changed("iptables-sync-period") && o.config.Mode != kubeproxyconfig.ProxyModeIPVS {
		o.config.SyncPeriod.Duration = o.iptablesSyncPeriod
	}
	if fs.Changed("iptables-min-sync-period") && o.config.Mode != kubeproxyconfig.ProxyModeIPVS {
		o.config.MinSyncPeriod.Duration = o.iptablesMinSyncPeriod
	}
	if fs.Changed("ipvs-sync-period") && o.config.Mode == kubeproxyconfig.ProxyModeIPVS {
		o.config.SyncPeriod.Duration = o.ipvsSyncPeriod
	}
	if fs.Changed("ipvs-min-sync-period") && o.config.Mode == kubeproxyconfig.ProxyModeIPVS {
		o.config.MinSyncPeriod.Duration = o.ipvsMinSyncPeriod
	}
	if fs.Changed("cluster-cidr") {
		o.config.DetectLocal.ClusterCIDRs = strings.Split(o.clusterCIDRs, ",")
	}
}

// Validate validates all the required options.
func (o *Options) Validate() error {
	if errs := validation.Validate(o.config); len(errs) != 0 {
		return errs.ToAggregate()
	}

	return nil
}

// Run runs the specified ProxyServer.
func (o *Options) Run(ctx context.Context) error {
	defer close(o.errCh)
	if len(o.WriteConfigTo) > 0 {
		return o.writeConfigFile()
	}

	err := platformCleanup(ctx, o.config.Mode, o.CleanupAndExit)
	if o.CleanupAndExit {
		return err
	}
	// We ignore err otherwise; the cleanup is best-effort, and the backends will have
	// logged messages if they failed in interesting ways.

	proxyServer, err := newProxyServer(ctx, o.config, o.master, o.InitAndExit, o.flagz)
	if err != nil {
		return err
	}
	if o.InitAndExit {
		return nil
	}

	o.proxyServer = proxyServer
	return o.runLoop(ctx)
}

// runLoop will watch on the update change of the proxy server's configuration file.
// Return an error when updated
func (o *Options) runLoop(ctx context.Context) error {
	if o.watcher != nil {
		o.watcher.Run()
	}

	// run the proxy in goroutine
	go func() {
		err := o.proxyServer.Run(ctx)
		o.errCh <- err
	}()

	for {
		err := <-o.errCh
		if err != nil {
			return err
		}
	}
}

func (o *Options) writeConfigFile() (err error) {
	const mediaType = runtime.ContentTypeYAML
	info, ok := runtime.SerializerInfoForMediaType(proxyconfigscheme.Codecs.SupportedMediaTypes(), mediaType)
	if !ok {
		return fmt.Errorf("unable to locate encoder -- %q is not a supported media type", mediaType)
	}

	encoder := proxyconfigscheme.Codecs.EncoderForVersion(info.Serializer, v1alpha1.SchemeGroupVersion)

	configFile, err := os.Create(o.WriteConfigTo)
	if err != nil {
		return err
	}

	defer func() {
		ferr := configFile.Close()
		if ferr != nil && err == nil {
			err = ferr
		}
	}()

	if err = encoder.Encode(o.config, configFile); err != nil {
		return err
	}

	o.logger.Info("Wrote configuration", "file", o.WriteConfigTo)

	return nil
}

// addressFromDeprecatedFlags returns server address from flags
// passed on the command line based on the following rules:
// 1. If port is 0, disable the server (e.g. set address to empty).
// 2. Otherwise, set the port portion of the config accordingly.
func addressFromDeprecatedFlags(addr string, port int32) string {
	if port == 0 {
		return ""
	}
	return proxyutil.AppendPortIfNeeded(addr, port)
}

// newLenientSchemeAndCodecs returns a scheme that has only v1alpha1 registered into
// it and a CodecFactory with strict decoding disabled.
func newLenientSchemeAndCodecs() (*runtime.Scheme, *serializer.CodecFactory, error) {
	lenientScheme := runtime.NewScheme()
	if err := kubeproxyconfig.AddToScheme(lenientScheme); err != nil {
		return nil, nil, fmt.Errorf("failed to add kube-proxy config API to lenient scheme: %w", err)
	}
	if err := kubeproxyconfigv1alpha1.AddToScheme(lenientScheme); err != nil {
		return nil, nil, fmt.Errorf("failed to add kube-proxy config v1alpha1 API to lenient scheme: %w", err)
	}
	lenientCodecs := serializer.NewCodecFactory(lenientScheme, serializer.DisableStrict)
	return lenientScheme, &lenientCodecs, nil
}

// loadConfigFromFile loads the contents of file and decodes it as a
// KubeProxyConfiguration object.
func (o *Options) loadConfigFromFile(file string) (*kubeproxyconfig.KubeProxyConfiguration, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}

	return o.loadConfig(data)
}

// loadConfig decodes a serialized KubeProxyConfiguration to the internal type.
func (o *Options) loadConfig(data []byte) (*kubeproxyconfig.KubeProxyConfiguration, error) {

	configObj, gvk, err := proxyconfigscheme.Codecs.UniversalDecoder().Decode(data, nil, nil)
	if err != nil {
		// Try strict decoding first. If that fails decode with a lenient
		// decoder, which has only v1alpha1 registered, and log a warning.
		// The lenient path is to be dropped when support for v1alpha1 is dropped.
		if !runtime.IsStrictDecodingError(err) {
			return nil, fmt.Errorf("failed to decode: %w", err)
		}

		_, lenientCodecs, lenientErr := newLenientSchemeAndCodecs()
		if lenientErr != nil {
			return nil, lenientErr
		}

		configObj, gvk, lenientErr = lenientCodecs.UniversalDecoder().Decode(data, nil, nil)
		if lenientErr != nil {
			// Lenient decoding failed with the current version, return the
			// original strict error.
			return nil, fmt.Errorf("failed lenient decoding: %w", err)
		}

		// Continue with the v1alpha1 object that was decoded leniently, but emit a warning.
		o.logger.Info("Using lenient decoding as strict decoding failed", "err", err)
	}

	proxyConfig, ok := configObj.(*kubeproxyconfig.KubeProxyConfiguration)
	if !ok {
		return nil, fmt.Errorf("got unexpected config type: %v", gvk)
	}
	return proxyConfig, nil
}
