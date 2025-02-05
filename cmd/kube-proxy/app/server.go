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

// Package app does all of the work necessary to configure and run a
// Kubernetes app process.
package app

import (
	"context"
	goflag "flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/spf13/cobra"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/apimachinery/pkg/types"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apiserver/pkg/server/healthz"
	"k8s.io/apiserver/pkg/server/mux"
	"k8s.io/apiserver/pkg/server/routes"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/client-go/informers"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"k8s.io/client-go/tools/events"
	cliflag "k8s.io/component-base/cli/flag"
	componentbaseconfig "k8s.io/component-base/config"
	"k8s.io/component-base/configz"
	"k8s.io/component-base/logs"
	logsapi "k8s.io/component-base/logs/api/v1"
	"k8s.io/component-base/metrics"
	metricsfeatures "k8s.io/component-base/metrics/features"
	"k8s.io/component-base/metrics/legacyregistry"
	"k8s.io/component-base/metrics/prometheus/slis"
	"k8s.io/component-base/version"
	"k8s.io/component-base/version/verflag"
	zpagesfeatures "k8s.io/component-base/zpages/features"
	"k8s.io/component-base/zpages/flagz"
	"k8s.io/component-base/zpages/statusz"
	nodeutil "k8s.io/component-helpers/node/util"
	"k8s.io/klog/v2"
	api "k8s.io/kubernetes/pkg/apis/core"
	"k8s.io/kubernetes/pkg/features"
	"k8s.io/kubernetes/pkg/proxy"
	"k8s.io/kubernetes/pkg/proxy/apis"
	kubeproxyconfig "k8s.io/kubernetes/pkg/proxy/apis/config"
	proxyconfigscheme "k8s.io/kubernetes/pkg/proxy/apis/config/scheme"
	"k8s.io/kubernetes/pkg/proxy/config"
	"k8s.io/kubernetes/pkg/proxy/healthcheck"
	proxymetrics "k8s.io/kubernetes/pkg/proxy/metrics"
	proxyutil "k8s.io/kubernetes/pkg/proxy/util"
	utilnode "k8s.io/kubernetes/pkg/util/node"
	"k8s.io/kubernetes/pkg/util/oom"
	netutils "k8s.io/utils/net"
)

const (
	// kubeProxy defines variable used internally when referring to kube-proxy component
	kubeProxy = "kube-proxy"
)

func init() {
	utilruntime.Must(metricsfeatures.AddFeatureGates(utilfeature.DefaultMutableFeatureGate))
	utilruntime.Must(logsapi.AddFeatureGates(utilfeature.DefaultMutableFeatureGate))
}

// proxyRun defines the interface to run a specified ProxyServer
type proxyRun interface {
	Run(ctx context.Context) error
}

// NewProxyCommand creates a *cobra.Command object with default parameters
func NewProxyCommand() *cobra.Command {
	opts := NewOptions()

	cmd := &cobra.Command{
		Use: kubeProxy,
		Long: `The Kubernetes network proxy runs on each node. This
reflects services as defined in the Kubernetes API on each node and can do simple
TCP, UDP, and SCTP stream forwarding or round robin TCP, UDP, and SCTP forwarding across a set of backends.
Service cluster IPs and ports are currently found through Docker-links-compatible
environment variables specifying ports opened by the service proxy. There is an optional
addon that provides cluster DNS for these cluster IPs. The user must create a service
with the apiserver API to configure the proxy.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			verflag.PrintAndExitIfRequested()

			if err := initForOS(opts.config.Windows.RunAsService); err != nil {
				return fmt.Errorf("failed os init: %w", err)
			}

			if err := opts.Complete(cmd.Flags()); err != nil {
				return fmt.Errorf("failed complete: %w", err)
			}

			logs.InitLogs()
			if err := logsapi.ValidateAndApplyAsField(&opts.config.Logging, utilfeature.DefaultFeatureGate, field.NewPath("logging")); err != nil {
				return fmt.Errorf("initialize logging: %w", err)
			}

			cliflag.PrintFlags(cmd.Flags())

			if err := opts.Validate(); err != nil {
				return fmt.Errorf("failed validate: %w", err)
			}
			// add feature enablement metrics
			utilfeature.DefaultMutableFeatureGate.AddMetrics()
			if err := opts.Run(context.Background()); err != nil {
				opts.logger.Error(err, "Error running ProxyServer")
				return err
			}

			return nil
		},
		Args: func(cmd *cobra.Command, args []string) error {
			for _, arg := range args {
				if len(arg) > 0 {
					return fmt.Errorf("%q does not take any arguments, got %q", cmd.CommandPath(), args)
				}
			}
			return nil
		},
	}

	fs := cmd.Flags()
	opts.AddFlags(fs)
	fs.AddGoFlagSet(goflag.CommandLine) // for --boot-id-file and --machine-id-file

	_ = cmd.MarkFlagFilename("config", "yaml", "yml", "json")

	return cmd
}

// ProxyServer represents all the parameters required to start the Kubernetes proxy server. All
// fields are required.
type ProxyServer struct {
	Config *kubeproxyconfig.KubeProxyConfiguration

	Client          clientset.Interface
	Broadcaster     events.EventBroadcaster
	Recorder        events.EventRecorder
	NodeRef         *v1.ObjectReference
	HealthzServer   *healthcheck.ProxyHealthServer
	Hostname        string
	PrimaryIPFamily v1.IPFamily
	NodeIPs         map[v1.IPFamily]net.IP
	flagz           flagz.Reader

	podCIDRs []string // only used for LocalModeNodeCIDR

	Proxier proxy.Provider
}

// newProxyServer creates a ProxyServer based on the given config
func newProxyServer(ctx context.Context, config *kubeproxyconfig.KubeProxyConfiguration, master string, initOnly bool, flagzReader flagz.Reader) (*ProxyServer, error) {
	logger := klog.FromContext(ctx)

	s := &ProxyServer{
		Config: config,
		flagz:  flagzReader,
	}

	cz, err := configz.New(kubeproxyconfig.GroupName)
	if err != nil {
		return nil, fmt.Errorf("unable to register configz: %s", err)
	}
	cz.Set(config)

	if len(config.ShowHiddenMetricsForVersion) > 0 {
		metrics.SetShowHidden()
	}

	s.Hostname, err = nodeutil.GetHostname(config.HostnameOverride)
	if err != nil {
		return nil, err
	}

	s.Client, err = createClient(ctx, config.ClientConnection, master)
	if err != nil {
		return nil, err
	}

	rawNodeIPs := getNodeIPs(ctx, s.Client, s.Hostname)
	s.PrimaryIPFamily, s.NodeIPs = detectNodeIPs(ctx, rawNodeIPs, config.NodeIPOverride, config.IPFamilyPolicy)

	if len(config.NodePortAddresses) == 1 && config.NodePortAddresses[0] == kubeproxyconfig.NodePortAddressesPrimary {
		var nodePortAddresses []string
		if nodeIP := s.NodeIPs[v1.IPv4Protocol]; nodeIP != nil && !nodeIP.IsLoopback() {
			nodePortAddresses = append(nodePortAddresses, fmt.Sprintf("%s/32", nodeIP.String()))
		}
		if nodeIP := s.NodeIPs[v1.IPv6Protocol]; nodeIP != nil && !nodeIP.IsLoopback() {
			nodePortAddresses = append(nodePortAddresses, fmt.Sprintf("%s/128", nodeIP.String()))
		}
		config.NodePortAddresses = nodePortAddresses
	}

	s.Broadcaster = events.NewBroadcaster(&events.EventSinkImpl{Interface: s.Client.EventsV1()})
	s.Recorder = s.Broadcaster.NewRecorder(proxyconfigscheme.Scheme, kubeProxy)

	s.NodeRef = &v1.ObjectReference{
		Kind:      "Node",
		Name:      s.Hostname,
		UID:       types.UID(s.Hostname),
		Namespace: "",
	}

	if len(config.HealthzBindAddress) > 0 {
		s.HealthzServer = healthcheck.NewProxyHealthServer(config.HealthzBindAddress, 2*config.SyncPeriod.Duration)
	}

	err = s.platformSetup(ctx)
	if err != nil {
		return nil, err
	}

	err = checkBadConfig(s)
	if err != nil {
		logger.Error(err, "Kube-proxy configuration may be incomplete or incorrect")
	}

	ipv4Supported, ipv6Supported, dualStackSupported, err := s.platformCheckSupported(ctx)
	if err != nil {
		return nil, err
	} else if (s.PrimaryIPFamily == v1.IPv4Protocol && !ipv4Supported) || (s.PrimaryIPFamily == v1.IPv6Protocol && !ipv6Supported) {
		return nil, fmt.Errorf("no support for primary IP family %q", s.PrimaryIPFamily)
	} else if dualStackSupported {
		logger.Info("kube-proxy running in dual-stack mode", "primary ipFamily", s.PrimaryIPFamily)
	} else {
		logger.Info("kube-proxy running in single-stack mode", "ipFamily", s.PrimaryIPFamily)
	}

	err, fatal := checkBadIPConfig(s, dualStackSupported)
	if err != nil {
		if fatal {
			return nil, fmt.Errorf("kube-proxy configuration is incorrect: %w", err)
		}
		logger.Error(err, "Kube-proxy configuration may be incomplete or incorrect")
	}

	s.Proxier, err = s.createProxier(ctx, config, dualStackSupported, initOnly)
	if err != nil {
		return nil, err
	}

	return s, nil
}

// checkBadConfig checks for bad/deprecated configuation
func checkBadConfig(s *ProxyServer) error {
	var errors []error

	// At this point we haven't seen any actual Services or EndpointSlices, so we
	// don't really know if the cluster is expected to be single- or dual-stack. But
	// we can at least take note of whether there is any explicitly-dual-stack
	// configuration.
	anyDualStackConfig := false
	for _, config := range [][]string{s.Config.DetectLocal.ClusterCIDRs, s.Config.NodePortAddresses, s.Config.IPVS.ExcludeCIDRs, s.podCIDRs} {
		if dual, _ := netutils.IsDualStackCIDRStrings(config); dual {
			anyDualStackConfig = true
			break
		}
	}

	// Warn if NodePortAddresses does not limit connections on all IP families that
	// seem to be in use.
	cidrsByFamily := proxyutil.MapCIDRsByIPFamily(s.Config.NodePortAddresses)
	if len(s.Config.NodePortAddresses) == 0 {
		errors = append(errors, fmt.Errorf("nodePortAddresses is unset; NodePort connections will be accepted on all local IPs. Consider using `--nodeport-addresses primary`"))
	} else if anyDualStackConfig && len(cidrsByFamily[s.PrimaryIPFamily]) == len(s.Config.NodePortAddresses) {
		errors = append(errors, fmt.Errorf("cluster appears to be dual-stack but nodePortAddresses contains only %s addresses; NodePort connections will be accepted on all local %s IPs", s.PrimaryIPFamily, proxyutil.OtherIPFamily(s.PrimaryIPFamily)))
	} else if len(cidrsByFamily[s.PrimaryIPFamily]) == 0 {
		errors = append(errors, fmt.Errorf("cluster appears to be %s-primary but nodePortAddresses contains only %s addresses; NodePort connections will be accepted on all local %s IPs", s.PrimaryIPFamily, proxyutil.OtherIPFamily(s.PrimaryIPFamily), s.PrimaryIPFamily))
	}

	return utilerrors.NewAggregate(errors)
}

// checkBadIPConfig checks for bad configuration relative to s.PrimaryIPFamily.
// Historically, we did not check most of the config options, so we cannot retroactively
// make IP family mismatches in those options be fatal. When we add new options to check
// here, we should make problems with those options be fatal.
func checkBadIPConfig(s *ProxyServer, dualStackSupported bool) (err error, fatal bool) {
	var errors []error
	var badFamily netutils.IPFamily

	if s.PrimaryIPFamily == v1.IPv4Protocol {
		badFamily = netutils.IPv6
	} else {
		badFamily = netutils.IPv4
	}

	var clusterType string
	if dualStackSupported {
		clusterType = fmt.Sprintf("%s-primary", s.PrimaryIPFamily)
	} else {
		clusterType = fmt.Sprintf("%s-only", s.PrimaryIPFamily)
	}

	if badCIDRs(s.Config.DetectLocal.ClusterCIDRs, badFamily) {
		errors = append(errors, fmt.Errorf("cluster is %s but clusterCIDRs contains only IPv%s addresses", clusterType, badFamily))
		if s.Config.DetectLocalMode == kubeproxyconfig.LocalModeClusterCIDR && !dualStackSupported {
			// This has always been a fatal error
			fatal = true
		}
	}

	if badCIDRs(s.podCIDRs, badFamily) {
		errors = append(errors, fmt.Errorf("cluster is %s but node.spec.podCIDRs contains only IPv%s addresses", clusterType, badFamily))
		if s.Config.DetectLocalMode == kubeproxyconfig.LocalModeNodeCIDR {
			// This has always been a fatal error
			fatal = true
		}
	}

	if netutils.IPFamilyOfString(s.Config.Winkernel.SourceVip) == badFamily {
		errors = append(errors, fmt.Errorf("cluster is %s but winkernel.sourceVip is IPv%s", clusterType, badFamily))
	}

	// In some cases, wrong-IP-family is only a problem when the secondary IP family
	// isn't present at all.
	if !dualStackSupported {
		if badCIDRs(s.Config.IPVS.ExcludeCIDRs, badFamily) {
			errors = append(errors, fmt.Errorf("cluster is %s but ipvs.excludeCIDRs contains only IPv%s addresses", clusterType, badFamily))
		}

		if badBindAddress(s.Config.HealthzBindAddress, badFamily) {
			errors = append(errors, fmt.Errorf("cluster is %s but healthzBindAddress is IPv%s", clusterType, badFamily))
		}
		if badBindAddress(s.Config.MetricsBindAddress, badFamily) {
			errors = append(errors, fmt.Errorf("cluster is %s but metricsBindAddress is IPv%s", clusterType, badFamily))
		}
	}

	// Note that s.Config.NodePortAddresses gets checked as part of checkBadConfig()
	// so it doesn't need to be checked here.

	return utilerrors.NewAggregate(errors), fatal
}

// badCIDRs returns true if cidrs is a non-empty list of CIDRs, all of wrongFamily.
func badCIDRs(cidrs []string, wrongFamily netutils.IPFamily) bool {
	if len(cidrs) == 0 {
		return false
	}
	for _, cidr := range cidrs {
		if netutils.IPFamilyOfCIDRString(cidr) != wrongFamily {
			return false
		}
	}
	return true
}

// badBindAddress returns true if bindAddress is an "IP:port" string where IP is a
// non-zero IP of wrongFamily.
func badBindAddress(bindAddress string, wrongFamily netutils.IPFamily) bool {
	if host, _, _ := net.SplitHostPort(bindAddress); host != "" {
		ip := netutils.ParseIPSloppy(host)
		if ip != nil && netutils.IPFamilyOf(ip) == wrongFamily && !ip.IsUnspecified() {
			return true
		}
	}
	return false
}

// createClient creates a kube client from the given config and masterOverride.
// TODO remove masterOverride when CLI flags are removed.
func createClient(ctx context.Context, config componentbaseconfig.ClientConnectionConfiguration, masterOverride string) (clientset.Interface, error) {
	logger := klog.FromContext(ctx)
	var kubeConfig *rest.Config
	var err error

	if len(config.Kubeconfig) == 0 && len(masterOverride) == 0 {
		logger.Info("Neither kubeconfig file nor master URL was specified, falling back to in-cluster config")
		kubeConfig, err = rest.InClusterConfig()
	} else {
		// This creates a client, first loading any specified kubeconfig
		// file, and then overriding the Master flag, if non-empty.
		kubeConfig, err = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
			&clientcmd.ClientConfigLoadingRules{ExplicitPath: config.Kubeconfig},
			&clientcmd.ConfigOverrides{ClusterInfo: clientcmdapi.Cluster{Server: masterOverride}}).ClientConfig()
	}
	if err != nil {
		return nil, err
	}

	kubeConfig.AcceptContentTypes = config.AcceptContentTypes
	kubeConfig.ContentType = config.ContentType
	kubeConfig.QPS = config.QPS
	kubeConfig.Burst = int(config.Burst)

	client, err := clientset.NewForConfig(kubeConfig)
	if err != nil {
		return nil, err
	}

	return client, nil
}

func serveHealthz(ctx context.Context, hz *healthcheck.ProxyHealthServer, errCh chan error) {
	logger := klog.FromContext(ctx)
	if hz == nil {
		return
	}

	fn := func() {
		err := hz.Run(ctx)
		if err != nil {
			logger.Error(err, "Healthz server failed")
			if errCh != nil {
				errCh <- fmt.Errorf("healthz server failed: %w", err)
				// if in hardfail mode, never retry again
				blockCh := make(chan error)
				<-blockCh
			}
		} else {
			logger.Error(nil, "Healthz server returned without error")
		}
	}
	go wait.Until(fn, 5*time.Second, ctx.Done())
}

func serveMetrics(ctx context.Context, bindAddress string, proxyMode kubeproxyconfig.ProxyMode, enableProfiling bool, flagzReader flagz.Reader, errCh chan error) {
	if len(bindAddress) == 0 {
		return
	}

	proxyMux := mux.NewPathRecorderMux(kubeProxy)
	healthz.InstallHandler(proxyMux)
	slis.SLIMetricsWithReset{}.Install(proxyMux)

	proxyMux.HandleFunc("/proxyMode", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		fmt.Fprintf(w, "%s", proxyMode)
	})

	proxyMux.Handle("/metrics", legacyregistry.Handler())

	if enableProfiling {
		routes.Profiling{}.Install(proxyMux)
		routes.DebugFlags{}.Install(proxyMux, "v", routes.StringFlagPutHandler(logs.GlogSetter))
	}

	configz.InstallHandler(proxyMux)

	if flagzReader != nil {
		flagz.Install(proxyMux, "kube-proxy", flagzReader)
	}

	if utilfeature.DefaultFeatureGate.Enabled(zpagesfeatures.ComponentStatusz) {
		statusz.Install(proxyMux, kubeProxy, statusz.NewRegistry())
	}

	fn := func() {
		var err error
		defer func() {
			if err != nil {
				err = fmt.Errorf("starting metrics server failed: %w", err)
				utilruntime.HandleError(err)
				if errCh != nil {
					errCh <- err
					// if in hardfail mode, never retry again
					blockCh := make(chan error)
					<-blockCh
				}
			}
		}()

		listener, err := netutils.MultiListen(ctx, "tcp", bindAddress)
		if err != nil {
			return
		}

		server := &http.Server{Handler: proxyMux}
		err = server.Serve(listener)
		if err != nil {
			return
		}

	}
	go wait.Until(fn, 5*time.Second, wait.NeverStop)
}

// Run runs the specified ProxyServer.  This should never exit (unless CleanupAndExit is set).
// TODO: At the moment, Run() cannot return a nil error, otherwise it's caller will never exit. Update callers of Run to handle nil errors.
func (s *ProxyServer) Run(ctx context.Context) error {
	logger := klog.FromContext(ctx)
	// To help debugging, immediately log version
	logger.Info("Version info", "version", version.Get())

	logger.Info("Golang settings", "GOGC", os.Getenv("GOGC"), "GOMAXPROCS", os.Getenv("GOMAXPROCS"), "GOTRACEBACK", os.Getenv("GOTRACEBACK"))

	proxymetrics.RegisterMetrics(s.Config.Mode)

	// TODO(vmarmol): Use container config for this.
	var oomAdjuster *oom.OOMAdjuster
	if s.Config.Linux.OOMScoreAdj != nil {
		oomAdjuster = oom.NewOOMAdjuster()
		if err := oomAdjuster.ApplyOOMScoreAdj(0, int(*s.Config.Linux.OOMScoreAdj)); err != nil {
			logger.V(2).Info("Failed to apply OOMScore", "err", err)
		}
	}

	if s.Broadcaster != nil {
		stopCh := make(chan struct{})
		s.Broadcaster.StartRecordingToSink(stopCh)
	}

	// TODO(thockin): make it possible for healthz and metrics to be on the same port.

	var healthzErrCh, metricsErrCh chan error
	if s.Config.BindAddressHardFail {
		healthzErrCh = make(chan error)
		metricsErrCh = make(chan error)
	}

	// Start up a healthz server if requested
	serveHealthz(ctx, s.HealthzServer, healthzErrCh)

	// Start up a metrics server if requested
	serveMetrics(ctx, s.Config.MetricsBindAddress, s.Config.Mode, s.Config.EnableProfiling, s.flagz, metricsErrCh)

	noProxyName, err := labels.NewRequirement(apis.LabelServiceProxyName, selection.DoesNotExist, nil)
	if err != nil {
		return err
	}

	noHeadlessEndpoints, err := labels.NewRequirement(v1.IsHeadlessService, selection.DoesNotExist, nil)
	if err != nil {
		return err
	}

	labelSelector := labels.NewSelector()
	labelSelector = labelSelector.Add(*noProxyName, *noHeadlessEndpoints)

	// Make informers that filter out objects that want a non-default service proxy.
	informerFactory := informers.NewSharedInformerFactoryWithOptions(s.Client, s.Config.ConfigSyncPeriod.Duration,
		informers.WithTweakListOptions(func(options *metav1.ListOptions) {
			options.LabelSelector = labelSelector.String()
		}))

	// Create configs (i.e. Watches for Services, EndpointSlices and ServiceCIDRs)
	// Note: RegisterHandler() calls need to happen before creation of Sources because sources
	// only notify on changes, and the initial update (on process start) may be lost if no handlers
	// are registered yet.
	// don't watch headless services for kube-proxy, they are proxied by DNS.
	serviceInformerFactory := informers.NewSharedInformerFactoryWithOptions(s.Client, s.Config.ConfigSyncPeriod.Duration,
		informers.WithTweakListOptions(func(options *metav1.ListOptions) {
			options.LabelSelector = labelSelector.String()
			options.FieldSelector = fields.OneTermNotEqualSelector("spec.clusterIP", v1.ClusterIPNone).String()
		}))
	serviceConfig := config.NewServiceConfig(ctx, serviceInformerFactory.Core().V1().Services(), s.Config.ConfigSyncPeriod.Duration)
	serviceConfig.RegisterEventHandler(s.Proxier)
	go serviceConfig.Run(ctx.Done())

	endpointSliceConfig := config.NewEndpointSliceConfig(ctx, informerFactory.Discovery().V1().EndpointSlices(), s.Config.ConfigSyncPeriod.Duration)
	endpointSliceConfig.RegisterEventHandler(s.Proxier)
	go endpointSliceConfig.Run(ctx.Done())

	if utilfeature.DefaultFeatureGate.Enabled(features.MultiCIDRServiceAllocator) {
		serviceCIDRConfig := config.NewServiceCIDRConfig(ctx, informerFactory.Networking().V1().ServiceCIDRs(), s.Config.ConfigSyncPeriod.Duration)
		serviceCIDRConfig.RegisterEventHandler(s.Proxier)
		go serviceCIDRConfig.Run(wait.NeverStop)
	}
	// This has to start after the calls to NewServiceConfig because that
	// function must configure its shared informer event handlers first.
	informerFactory.Start(wait.NeverStop)
	serviceInformerFactory.Start(wait.NeverStop)

	// Make an informer that selects for our nodename.
	currentNodeInformerFactory := informers.NewSharedInformerFactoryWithOptions(s.Client, s.Config.ConfigSyncPeriod.Duration,
		informers.WithTweakListOptions(func(options *metav1.ListOptions) {
			options.FieldSelector = fields.OneTermEqualSelector("metadata.name", s.NodeRef.Name).String()
		}))
	nodeConfig := config.NewNodeConfig(ctx, currentNodeInformerFactory.Core().V1().Nodes(), s.Config.ConfigSyncPeriod.Duration)
	// https://issues.k8s.io/111321
	if s.Config.DetectLocalMode == kubeproxyconfig.LocalModeNodeCIDR {
		nodeConfig.RegisterEventHandler(proxy.NewNodePodCIDRHandler(ctx, s.podCIDRs))
	}
	nodeConfig.RegisterEventHandler(&proxy.NodeEligibleHandler{
		HealthServer: s.HealthzServer,
	})
	nodeConfig.RegisterEventHandler(s.Proxier)

	go nodeConfig.Run(wait.NeverStop)

	// This has to start after the calls to NewNodeConfig because that must
	// configure the shared informer event handler first.
	currentNodeInformerFactory.Start(wait.NeverStop)

	// Birth Cry after the birth is successful
	s.birthCry()

	go s.Proxier.SyncLoop()

	select {
	case err = <-healthzErrCh:
		s.Recorder.Eventf(s.NodeRef, nil, api.EventTypeWarning, "FailedToStartProxierHealthcheck", "StartKubeProxy", err.Error())
	case err = <-metricsErrCh:
		s.Recorder.Eventf(s.NodeRef, nil, api.EventTypeWarning, "FailedToStartMetricServer", "StartKubeProxy", err.Error())
	}
	return err
}

func (s *ProxyServer) birthCry() {
	s.Recorder.Eventf(s.NodeRef, nil, api.EventTypeNormal, "Starting", "StartKubeProxy", "")
}

// detectNodeIPs returns the proxier's "node IP" or IPs, and the IP family to use if the
// node turns out to be incapable of dual-stack. (Note that kube-proxy normally runs as
// dual-stack if the backend is capable of supporting both IP families, regardless of
// whether the node is *actually* configured as dual-stack or not.)

// (Note that on Linux, the node IPs are used only to determine whether a given
// LoadBalancerSourceRanges value matches the node or not. In particular, they are *not*
// used for NodePort handling unless NodePortAddresses is not set to 'primary'.)
//
// The order of precedence is:
//  1. if nodeIPOverride[0] is not 0.0.0.0 or ::, then ip family of nodeIPOverride[0] is considered as primary IP family.
//  2. if rawNodeIPs is not empty, then its address(es) is/are used
//  3. otherwise the node IPs are 127.0.0.1 and ::1
func detectNodeIPs(ctx context.Context, rawNodeIPs []net.IP, nodeIPOverride []string, ipFamilyPolicy kubeproxyconfig.IPFamilyPolicy) (v1.IPFamily, map[v1.IPFamily]net.IP) {
	logger := klog.FromContext(ctx)
	primaryFamily := v1.IPv4Protocol
	nodeIPs := map[v1.IPFamily]net.IP{
		v1.IPv4Protocol: net.IPv4(127, 0, 0, 1),
		v1.IPv6Protocol: net.IPv6loopback,
	}

	if len(rawNodeIPs) > 0 {
		if !netutils.IsIPv4(rawNodeIPs[0]) {
			primaryFamily = v1.IPv6Protocol
		}
		nodeIPs[primaryFamily] = rawNodeIPs[0]
		if len(rawNodeIPs) > 1 {
			// If more than one address is returned, they are guaranteed to be of different families
			family := v1.IPv4Protocol
			if !netutils.IsIPv4(rawNodeIPs[1]) {
				family = v1.IPv6Protocol
			}
			nodeIPs[family] = rawNodeIPs[1]
		}
	}

	// if nodeIPOverride is passed, override the IPs.
	for i := range nodeIPOverride {
		family := v1.IPv4Protocol
		ip := netutils.ParseIPSloppy(nodeIPOverride[i])
		if ip != nil && !ip.IsUnspecified() {
			if !netutils.IsIPv4(ip) {
				family = v1.IPv6Protocol
			}
			nodeIPs[family] = ip
			// only override the primaryFamily for nodeIPOverride[0]
			if i == 0 {
				primaryFamily = family
			}
		}
	}

	// we should only return NodeIP for single IPFamily if IPFamilyPolicy is SingleStack.
	if ipFamilyPolicy == kubeproxyconfig.IPFamilyPolicySingleStack {
		var secondaryFamily v1.IPFamily
		for family := range nodeIPs {
			if family == primaryFamily {
				continue
			}
			secondaryFamily = family
			break
		}
		delete(nodeIPs, secondaryFamily)
	}

	if nodeIPs[primaryFamily].IsLoopback() {
		logger.Info("Can't determine this node's IP, assuming loopback; if this is incorrect, please set the --bind-address flag")
	}
	return primaryFamily, nodeIPs
}

// getNodeIP returns IPs for the node with the provided name.  If
// required, it will wait for the node to be created.
func getNodeIPs(ctx context.Context, client clientset.Interface, name string) []net.IP {
	logger := klog.FromContext(ctx)
	var nodeIPs []net.IP
	backoff := wait.Backoff{
		Steps:    6,
		Duration: 1 * time.Second,
		Factor:   2.0,
		Jitter:   0.2,
	}

	err := wait.ExponentialBackoff(backoff, func() (bool, error) {
		node, err := client.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			logger.Error(err, "Failed to retrieve node info")
			return false, nil
		}
		nodeIPs, err = utilnode.GetNodeHostIPs(node)
		if err != nil {
			logger.Error(err, "Failed to retrieve node IPs")
			return false, nil
		}
		return true, nil
	})
	if err == nil {
		logger.Info("Successfully retrieved node IP(s)", "IPs", nodeIPs)
	}
	return nodeIPs
}
