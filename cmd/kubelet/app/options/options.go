/*
Copyright 2015 The Kubernetes Authors.

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

// Package options contains all of the primary arguments for a kubelet.
package options

import (
	"fmt"
	_ "net/http/pprof" // Enable pprof HTTP handlers.
	"path/filepath"
	"strings"

	"github.com/spf13/pflag"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/validation"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	cliflag "k8s.io/component-base/cli/flag"
	logsapi "k8s.io/component-base/logs/api/v1"
	"k8s.io/kubelet/config/v1beta1"
	kubeletapis "k8s.io/kubelet/pkg/apis"
	"k8s.io/kubernetes/pkg/cluster/ports"
	kubeletconfig "k8s.io/kubernetes/pkg/kubelet/apis/config"
	kubeletscheme "k8s.io/kubernetes/pkg/kubelet/apis/config/scheme"
	kubeletconfigvalidation "k8s.io/kubernetes/pkg/kubelet/apis/config/validation"
	"k8s.io/kubernetes/pkg/kubelet/config"
	utilflag "k8s.io/kubernetes/pkg/util/flag"
)

const defaultRootDir = "/var/lib/kubelet"

// KubeletFlags contains configuration flags for the Kubelet.
// A configuration field should go in KubeletFlags instead of KubeletConfiguration if any of these are true:
//   - its value will never, or cannot safely be changed during the lifetime of a node, or
//   - its value cannot be safely shared between nodes at the same time (e.g. a hostname);
//     KubeletConfiguration is intended to be shared between nodes.
//
// In general, please try to avoid adding flags or configuration fields,
// we already have a confusingly large amount of them.
type KubeletFlags struct {
	KubeConfig          string
	BootstrapKubeconfig string

	// HostnameOverride is the hostname used to identify the kubelet instead
	// of the actual hostname.
	HostnameOverride string
	// NodeIP is IP address of the node.
	// If set, kubelet will use this IP address for the node.
	NodeIP string

	// Container-runtime-specific options.
	config.ContainerRuntimeOptions

	// certDirectory is the directory where the TLS certs are located.
	// If tlsCertFile and tlsPrivateKeyFile are provided, this flag will be ignored.
	CertDirectory string

	// cloudProvider is the provider for cloud services.
	// +optional
	CloudProvider string

	// cloudConfigFile is the path to the cloud provider configuration file.
	// +optional
	CloudConfigFile string

	// rootDirectory is the directory path to place kubelet files (volume
	// mounts,etc).
	RootDirectory string

	// The Kubelet will load its initial configuration from this file.
	// The path may be absolute or relative; relative paths are under the Kubelet's current working directory.
	// Omit this flag to use the combination of built-in default configuration values and flags.
	KubeletConfigFile string

	// kubeletDropinConfigDirectory is a path to a directory to specify dropins allows the user to optionally specify
	// additional configs to overwrite what is provided by default and in the KubeletConfigFile flag
	KubeletDropinConfigDirectory string

	// WindowsService should be set to true if kubelet is running as a service on Windows.
	// Its corresponding flag only gets registered in Windows builds.
	WindowsService bool

	// WindowsPriorityClass sets the priority class associated with the Kubelet process
	// Its corresponding flag only gets registered in Windows builds
	// The default priority class associated with any process in Windows is NORMAL_PRIORITY_CLASS. Keeping it as is
	// to maintain backwards compatibility.
	// Source: https://docs.microsoft.com/en-us/windows/win32/procthread/scheduling-priorities
	WindowsPriorityClass string

	// experimentalMounterPath is the path of mounter binary. Leave empty to use the default mount path
	ExperimentalMounterPath string
	// This flag, if set, will avoid including `EvictionHard` limits while computing Node Allocatable.
	// Refer to [Node Allocatable](https://kubernetes.io/docs/tasks/administer-cluster/reserve-compute-resources/#node-allocatable) doc for more information.
	ExperimentalNodeAllocatableIgnoreEvictionThreshold bool
	// Node Labels are the node labels to add when registering the node in the cluster
	NodeLabels map[string]string
	// lockFilePath is the path that kubelet will use to as a lock file.
	// It uses this file as a lock to synchronize with other kubelet processes
	// that may be running.
	LockFilePath string
	// ExitOnLockContention is a flag that signifies to the kubelet that it is running
	// in "bootstrap" mode. This requires that 'LockFilePath' has been set.
	// This will cause the kubelet to listen to inotify events on the lock file,
	// releasing it and exiting when another process tries to open that file.
	ExitOnLockContention bool
	// DEPRECATED FLAGS
	// minimumGCAge is the minimum age for a finished container before it is
	// garbage collected.
	MinimumGCAge metav1.Duration
	// maxPerPodContainerCount is the maximum number of old instances to
	// retain per container. Each container takes up some disk space.
	MaxPerPodContainerCount int32
	// maxContainerCount is the maximum number of old instances of containers
	// to retain globally. Each container takes up some disk space.
	MaxContainerCount int32
	// registerSchedulable tells the kubelet to register the node as
	// schedulable. Won't have any effect if register-node is false.
	// DEPRECATED: use registerWithTaints instead
	RegisterSchedulable bool
	// SeccompDefault enables the use of `RuntimeDefault` as the default seccomp profile for all workloads on the node.
	SeccompDefault bool
}

// NewKubeletFlags will create a new KubeletFlags with default values
func NewKubeletFlags() *KubeletFlags {
	return &KubeletFlags{
		ContainerRuntimeOptions: *NewContainerRuntimeOptions(),
		CertDirectory:           "/var/lib/kubelet/pki",
		RootDirectory:           filepath.Clean(defaultRootDir),
		MaxContainerCount:       -1,
		MaxPerPodContainerCount: 1,
		MinimumGCAge:            metav1.Duration{Duration: 0},
		RegisterSchedulable:     true,
		NodeLabels:              make(map[string]string),
	}
}

// ValidateKubeletFlags validates Kubelet's configuration flags and returns an error if they are invalid.
func ValidateKubeletFlags(f *KubeletFlags) error {
	unknownLabels := sets.NewString()
	invalidLabelErrs := make(map[string][]string)
	for k, v := range f.NodeLabels {
		if isKubernetesLabel(k) && !kubeletapis.IsKubeletLabel(k) {
			unknownLabels.Insert(k)
		}

		if errs := validation.IsQualifiedName(k); len(errs) > 0 {
			invalidLabelErrs[k] = append(invalidLabelErrs[k], errs...)
		}
		if errs := validation.IsValidLabelValue(v); len(errs) > 0 {
			invalidLabelErrs[v] = append(invalidLabelErrs[v], errs...)
		}
	}
	if len(unknownLabels) > 0 {
		return fmt.Errorf("unknown 'kubernetes.io' or 'k8s.io' labels specified with --node-labels: %v\n--node-labels in the 'kubernetes.io' namespace must begin with an allowed prefix (%s) or be in the specifically allowed set (%s)", unknownLabels.List(), strings.Join(kubeletapis.KubeletLabelNamespaces(), ", "), strings.Join(kubeletapis.KubeletLabels(), ", "))
	}
	if len(invalidLabelErrs) > 0 {
		labelErrs := []string{}
		for k, v := range invalidLabelErrs {
			labelErrs = append(labelErrs, fmt.Sprintf("'%s' - %s", k, strings.Join(v, ", ")))
		}
		return fmt.Errorf("invalid node labels: %s", strings.Join(labelErrs, "; "))
	}
	return nil
}

func isKubernetesLabel(key string) bool {
	namespace := getLabelNamespace(key)
	if namespace == "kubernetes.io" || strings.HasSuffix(namespace, ".kubernetes.io") {
		return true
	}
	if namespace == "k8s.io" || strings.HasSuffix(namespace, ".k8s.io") {
		return true
	}
	return false
}

func getLabelNamespace(key string) string {
	if parts := strings.SplitN(key, "/", 2); len(parts) == 2 {
		return parts[0]
	}
	return ""
}

// NewKubeletConfiguration will create a new KubeletConfiguration with default values
func NewKubeletConfiguration() (*kubeletconfig.KubeletConfiguration, error) {
	scheme, _, err := kubeletscheme.NewSchemeAndCodecs()
	if err != nil {
		return nil, err
	}
	versioned := &v1beta1.KubeletConfiguration{}
	scheme.Default(versioned)
	config := &kubeletconfig.KubeletConfiguration{}
	if err := scheme.Convert(versioned, config, nil); err != nil {
		return nil, err
	}
	applyLegacyDefaults(config)
	return config, nil
}

// applyLegacyDefaults applies legacy default values to the KubeletConfiguration in order to
// preserve the command line API. This is used to construct the baseline default KubeletConfiguration
// before the first round of flag parsing.
func applyLegacyDefaults(kc *kubeletconfig.KubeletConfiguration) {
	// --anonymous-auth
	kc.Authentication.Anonymous.Enabled = true
	// --authentication-token-webhook
	kc.Authentication.Webhook.Enabled = false
	// --authorization-mode
	kc.Authorization.Mode = kubeletconfig.KubeletAuthorizationModeAlwaysAllow
	// --read-only-port
	kc.ReadOnlyPort = ports.KubeletReadOnlyPort
}

// KubeletServer encapsulates all of the parameters necessary for starting up
// a kubelet. These can either be set via command line or directly.
type KubeletServer struct {
	KubeletFlags
	kubeletconfig.KubeletConfiguration
}

// NewKubeletServer will create a new KubeletServer with default values.
func NewKubeletServer() (*KubeletServer, error) {
	config, err := NewKubeletConfiguration()
	if err != nil {
		return nil, err
	}
	return &KubeletServer{
		KubeletFlags:         *NewKubeletFlags(),
		KubeletConfiguration: *config,
	}, nil
}

// ValidateKubeletServer validates configuration of KubeletServer and returns an error if the input configuration is invalid.
func ValidateKubeletServer(s *KubeletServer) error {
	// please add any KubeletConfiguration validation to the kubeletconfigvalidation.ValidateKubeletConfiguration function
	if err := kubeletconfigvalidation.ValidateKubeletConfiguration(&s.KubeletConfiguration, utilfeature.DefaultFeatureGate); err != nil {
		return err
	}
	if err := ValidateKubeletFlags(&s.KubeletFlags); err != nil {
		return err
	}
	return nil
}

// AddFlags adds flags for a specific KubeletServer to the specified FlagSet
func (s *KubeletServer) AddFlags(fs *pflag.FlagSet) {
	s.KubeletFlags.AddFlags(fs)
	AddKubeletConfigFlags(fs, &s.KubeletConfiguration)
}

// AddFlags adds flags for a specific KubeletFlags to the specified FlagSet
func (f *KubeletFlags) AddFlags(mainfs *pflag.FlagSet) {
	fs := pflag.NewFlagSet("", pflag.ExitOnError)
	defer func() {
		// Unhide deprecated flags. We want deprecated flags to show in Kubelet help.
		// We have some hidden flags, but we might as well unhide these when they are deprecated,
		// as silently deprecating and removing (even hidden) things is unkind to people who use them.
		fs.VisitAll(func(f *pflag.Flag) {
			if len(f.Deprecated) > 0 {
				f.Hidden = false
			}
		})
		mainfs.AddFlagSet(fs)
	}()

	f.ContainerRuntimeOptions.AddFlags(fs)
	f.addOSFlags(fs)

	fs.StringVar(&f.KubeletConfigFile, "config", f.KubeletConfigFile, "The Kubelet will load its initial configuration from this file. The path may be absolute or relative; relative paths start at the Kubelet's current working directory. Omit this flag to use the built-in default configuration values. Command-line flags override configuration from this file.")
	fs.StringVar(&f.KubeletDropinConfigDirectory, "config-dir", "", "Path to a directory to specify drop-ins, allows the user to optionally specify additional configs to overwrite what is provided by default and in the KubeletConfigFile flag. [default='']")
	fs.StringVar(&f.KubeConfig, "kubeconfig", f.KubeConfig, "Path to a kubeconfig file, specifying how to connect to the API server. Providing --kubeconfig enables API server mode, omitting --kubeconfig enables standalone mode.")

	fs.StringVar(&f.BootstrapKubeconfig, "bootstrap-kubeconfig", f.BootstrapKubeconfig, "Path to a kubeconfig file that will be used to get client certificate for kubelet. "+
		"If the file specified by --kubeconfig does not exist, the bootstrap kubeconfig is used to request a client certificate from the API server. "+
		"On success, a kubeconfig file referencing the generated client certificate and key is written to the path specified by --kubeconfig. "+
		"The client certificate and key file will be stored in the directory pointed by --cert-dir.")

	fs.StringVar(&f.HostnameOverride, "hostname-override", f.HostnameOverride, "If non-empty, will use this string as identification instead of the actual hostname. If --cloud-provider is set, the cloud provider determines the name of the node (consult cloud provider documentation to determine if and how the hostname is used).")

	fs.StringVar(&f.NodeIP, "node-ip", f.NodeIP, "IP address (or comma-separated dual-stack IP addresses) of the node. If unset, kubelet will use the node's default IPv4 address, if any, or its default IPv6 address if it has no IPv4 addresses. You can pass '::' to make it prefer the default IPv6 address rather than the default IPv4 address. If cloud-provider is set to external, this flag will help to bootstrap the node with the corresponding IP.")

	fs.StringVar(&f.CertDirectory, "cert-dir", f.CertDirectory, "The directory where the TLS certs are located. "+
		"If --tls-cert-file and --tls-private-key-file are provided, this flag will be ignored.")

	fs.StringVar(&f.CloudProvider, "cloud-provider", f.CloudProvider, "The provider for cloud services. Set to empty string for running with no cloud provider. Set to 'external' for running with an external cloud provider. If set, the cloud provider determines the name of the node (consult cloud provider documentation to determine if and how the hostname is used).")

	fs.StringVar(&f.RootDirectory, "root-dir", f.RootDirectory, "Directory path for managing kubelet files (volume mounts,etc).")
	fs.BoolVar(&f.SeccompDefault, "seccomp-default", f.SeccompDefault, "Enable the use of `RuntimeDefault` as the default seccomp profile for all workloads.")

	bindableNodeLabels := cliflag.ConfigurationMap(f.NodeLabels)
	fs.Var(&bindableNodeLabels, "node-labels", fmt.Sprintf("Labels to add when registering the node in the cluster.  Labels must be key=value pairs separated by ','. Labels in the 'kubernetes.io' namespace must begin with an allowed prefix (%s) or be in the specifically allowed set (%s)", strings.Join(kubeletapis.KubeletLabelNamespaces(), ", "), strings.Join(kubeletapis.KubeletLabels(), ", ")))

	// EXPERIMENTAL FLAGS
	fs.StringVar(&f.LockFilePath, "lock-file", f.LockFilePath, "<Warning: Alpha feature> The path to file for kubelet to use as a lock file.")
	fs.BoolVar(&f.ExitOnLockContention, "exit-on-lock-contention", f.ExitOnLockContention, "Whether kubelet should exit upon lock-file contention.")

	// DEPRECATED FLAGS
	fs.DurationVar(&f.MinimumGCAge.Duration, "minimum-container-ttl-duration", f.MinimumGCAge.Duration, "Minimum age for a finished container before it is garbage collected.  Examples: '300ms', '10s' or '2h45m'")
	fs.MarkDeprecated("minimum-container-ttl-duration", "Use --eviction-hard or --eviction-soft instead. Will be removed in a future version.")
	fs.Int32Var(&f.MaxPerPodContainerCount, "maximum-dead-containers-per-container", f.MaxPerPodContainerCount, "Maximum number of old instances to retain per container.  Each container takes up some disk space.")
	fs.MarkDeprecated("maximum-dead-containers-per-container", "Use --eviction-hard or --eviction-soft instead. Will be removed in a future version.")
	fs.Int32Var(&f.MaxContainerCount, "maximum-dead-containers", f.MaxContainerCount, "Maximum number of old instances of containers to retain globally.  Each container takes up some disk space. To disable, set to a negative number.")
	fs.MarkDeprecated("maximum-dead-containers", "Use --eviction-hard or --eviction-soft instead. Will be removed in a future version.")
	fs.BoolVar(&f.RegisterSchedulable, "register-schedulable", f.RegisterSchedulable, "Register the node as schedulable. Won't have any effect if register-node is false.")
	fs.MarkDeprecated("register-schedulable", "will be removed in a future version")
	fs.StringVar(&f.ExperimentalMounterPath, "experimental-mounter-path", f.ExperimentalMounterPath, "[Experimental] Path of mounter binary. Leave empty to use the default mount.")
	fs.MarkDeprecated("experimental-mounter-path", "will be removed in 1.25 or later. in favor of using CSI.")
	fs.StringVar(&f.CloudConfigFile, "cloud-config", f.CloudConfigFile, "The path to the cloud provider configuration file. Empty string for no configuration file.")
	fs.MarkDeprecated("cloud-config", "will be removed in 1.25 or later, in favor of removing cloud provider code from Kubelet.")
	fs.BoolVar(&f.ExperimentalNodeAllocatableIgnoreEvictionThreshold, "experimental-allocatable-ignore-eviction", f.ExperimentalNodeAllocatableIgnoreEvictionThreshold, "When set to 'true', Hard Eviction Thresholds will be ignored while calculating Node Allocatable. See https://kubernetes.io/docs/tasks/administer-cluster/reserve-compute-resources/ for more details. [default=false]")
	fs.MarkDeprecated("experimental-allocatable-ignore-eviction", "will be removed in 1.25 or later.")
}

// AddKubeletConfigFlags adds flags for a specific kubeletconfig.KubeletConfiguration to the specified FlagSet
func AddKubeletConfigFlags(mainfs *pflag.FlagSet, c *kubeletconfig.KubeletConfiguration) {
	fs := pflag.NewFlagSet("", pflag.ExitOnError)
	defer func() {
		// All KubeletConfiguration flags are now deprecated, and any new flags that point to
		// KubeletConfiguration fields are deprecated-on-creation. When removing flags at the end
		// of their deprecation period, be careful to check that they have *actually* been deprecated
		// members of the KubeletConfiguration for the entire deprecation period:
		// e.g. if a flag was added after this deprecation function, it may not be at the end
		// of its lifetime yet, even if the rest are.
		deprecated := "This parameter should be set via the config file specified by the Kubelet's --config flag. See https://kubernetes.io/docs/tasks/administer-cluster/kubelet-config-file/ for more information."
		// Some flags are permanently (?) meant to be available. In
		// Kubernetes 1.23, the following options were added to
		// LoggingConfiguration (long-term goal, more complete
		// configuration file) but deprecating the flags seemed
		// premature.
		notDeprecated := map[string]bool{
			"v":                   true,
			"vmodule":             true,
			"log-flush-frequency": true,
			"provider-id":         true,
			"fail-cgroupv1":       true,
		}
		fs.VisitAll(func(f *pflag.Flag) {
			if notDeprecated[f.Name] {
				return
			}
			f.Deprecated = deprecated
		})
		mainfs.AddFlagSet(fs)
	}()

	fs.BoolVar(&c.EnableServer, "enable-server", c.EnableServer, "Enable the Kubelet's server")

	fs.BoolVar(&c.FailSwapOn, "fail-swap-on", c.FailSwapOn, "Makes the Kubelet fail to start if swap is enabled on the node. ")
	fs.StringVar(&c.StaticPodPath, "pod-manifest-path", c.StaticPodPath, "Path to the directory containing static pod files to run, or the path to a single static pod file. Files starting with dots will be ignored.")
	fs.DurationVar(&c.SyncFrequency.Duration, "sync-frequency", c.SyncFrequency.Duration, "Max period between synchronizing running containers and config")
	fs.DurationVar(&c.FileCheckFrequency.Duration, "file-check-frequency", c.FileCheckFrequency.Duration, "Duration between checking config files for new data")
	fs.DurationVar(&c.HTTPCheckFrequency.Duration, "http-check-frequency", c.HTTPCheckFrequency.Duration, "Duration between checking http for new data")
	fs.StringVar(&c.StaticPodURL, "manifest-url", c.StaticPodURL, "URL for accessing additional Pod specifications to run")
	fs.Var(cliflag.NewColonSeparatedMultimapStringString(&c.StaticPodURLHeader), "manifest-url-header", "Comma-separated list of HTTP headers to use when accessing the url provided to --manifest-url. Multiple headers with the same name will be added in the same order provided. This flag can be repeatedly invoked. For example: --manifest-url-header 'a:hello,b:again,c:world' --manifest-url-header 'b:beautiful'")
	fs.Var(&utilflag.IPVar{Val: &c.Address}, "address", "The IP address for the Kubelet to serve on (set to '0.0.0.0' or '::' for listening on all interfaces and IP address families)")
	fs.Int32Var(&c.Port, "port", c.Port, "The port for the Kubelet to serve on.")
	fs.Int32Var(&c.ReadOnlyPort, "read-only-port", c.ReadOnlyPort, "The read-only port for the Kubelet to serve on with no authentication/authorization (set to 0 to disable)")
	fs.BoolVar(&c.FailCgroupV1, "fail-cgroupv1", c.FailCgroupV1, "Prevent the kubelet from starting on the host using cgroup v1.")

	// runtime flags
	fs.StringVar(&c.ContainerRuntimeEndpoint, "container-runtime-endpoint", c.ContainerRuntimeEndpoint, "The endpoint of container runtime service. Unix Domain Sockets are supported on Linux, while npipe and tcp endpoints are supported on Windows. Examples:'unix:///path/to/runtime.sock', 'npipe:////./pipe/runtime'")
	fs.StringVar(&c.ImageServiceEndpoint, "image-service-endpoint", c.ImageServiceEndpoint, "The endpoint of container image service. If not specified, it will be the same with --container-runtime-endpoint by default. Unix Domain Socket are supported on Linux, while npipe and tcp endpoints are supported on Windows. Examples:'unix:///path/to/runtime.sock', 'npipe:////./pipe/runtime'")

	// Authentication
	fs.BoolVar(&c.Authentication.Anonymous.Enabled, "anonymous-auth", c.Authentication.Anonymous.Enabled, ""+
		"Enables anonymous requests to the Kubelet server. Requests that are not rejected by another "+
		"authentication method are treated as anonymous requests. Anonymous requests have a username "+
		"of system:anonymous, and a group name of system:unauthenticated.")
	fs.BoolVar(&c.Authentication.Webhook.Enabled, "authentication-token-webhook", c.Authentication.Webhook.Enabled, ""+
		"Use the TokenReview API to determine authentication for bearer tokens.")
	fs.DurationVar(&c.Authentication.Webhook.CacheTTL.Duration, "authentication-token-webhook-cache-ttl", c.Authentication.Webhook.CacheTTL.Duration, ""+
		"The duration to cache responses from the webhook token authenticator.")
	fs.StringVar(&c.Authentication.X509.ClientCAFile, "client-ca-file", c.Authentication.X509.ClientCAFile, ""+
		"If set, any request presenting a client certificate signed by one of the authorities in the client-ca-file "+
		"is authenticated with an identity corresponding to the CommonName of the client certificate.")

	// Authorization
	fs.StringVar((*string)(&c.Authorization.Mode), "authorization-mode", string(c.Authorization.Mode), ""+
		"Authorization mode for Kubelet server. Valid options are AlwaysAllow or Webhook. "+
		"Webhook mode uses the SubjectAccessReview API to determine authorization.")
	fs.DurationVar(&c.Authorization.Webhook.CacheAuthorizedTTL.Duration, "authorization-webhook-cache-authorized-ttl", c.Authorization.Webhook.CacheAuthorizedTTL.Duration, ""+
		"The duration to cache 'authorized' responses from the webhook authorizer.")
	fs.DurationVar(&c.Authorization.Webhook.CacheUnauthorizedTTL.Duration, "authorization-webhook-cache-unauthorized-ttl", c.Authorization.Webhook.CacheUnauthorizedTTL.Duration, ""+
		"The duration to cache 'unauthorized' responses from the webhook authorizer.")

	fs.StringVar(&c.TLSCertFile, "tls-cert-file", c.TLSCertFile, ""+
		"File containing x509 Certificate used for serving HTTPS (with intermediate certs, if any, concatenated after server cert). "+
		"If --tls-cert-file and --tls-private-key-file are not provided, a self-signed certificate and key "+
		"are generated for the public address and saved to the directory passed to --cert-dir.")
	fs.StringVar(&c.TLSPrivateKeyFile, "tls-private-key-file", c.TLSPrivateKeyFile, "File containing x509 private key matching --tls-cert-file.")
	fs.BoolVar(&c.ServerTLSBootstrap, "rotate-server-certificates", c.ServerTLSBootstrap, "Auto-request and rotate the kubelet serving certificates by requesting new certificates from the kube-apiserver when the certificate expiration approaches. Requires the RotateKubeletServerCertificate feature gate to be enabled, and approval of the submitted CertificateSigningRequest objects.")

	tlsCipherPreferredValues := cliflag.PreferredTLSCipherNames()
	tlsCipherInsecureValues := cliflag.InsecureTLSCipherNames()
	fs.StringSliceVar(&c.TLSCipherSuites, "tls-cipher-suites", c.TLSCipherSuites,
		"Comma-separated list of cipher suites for the server. "+
			"If omitted, the default Go cipher suites will be used. \n"+
			"Preferred values: "+strings.Join(tlsCipherPreferredValues, ", ")+". \n"+
			"Insecure values: "+strings.Join(tlsCipherInsecureValues, ", ")+".")
	tlsPossibleVersions := cliflag.TLSPossibleVersions()
	fs.StringVar(&c.TLSMinVersion, "tls-min-version", c.TLSMinVersion,
		"Minimum TLS version supported. "+
			"Possible values: "+strings.Join(tlsPossibleVersions, ", "))
	fs.BoolVar(&c.RotateCertificates, "rotate-certificates", c.RotateCertificates, "Auto rotate the kubelet client certificates by requesting new certificates from the kube-apiserver when the certificate expiration approaches.")

	fs.Int32Var(&c.RegistryPullQPS, "registry-qps", c.RegistryPullQPS, "If > 0, limit registry pull QPS to this value.  If 0, unlimited.")
	fs.Int32Var(&c.RegistryBurst, "registry-burst", c.RegistryBurst, "Maximum size of a bursty pulls, temporarily allows pulls to burst to this number, while still not exceeding registry-qps. Only used if --registry-qps > 0")
	fs.Int32Var(&c.EventRecordQPS, "event-qps", c.EventRecordQPS, "QPS to limit event creations. The number must be >= 0. If 0 will use DefaultQPS: 5.")
	fs.Int32Var(&c.EventBurst, "event-burst", c.EventBurst, "Maximum size of a bursty event records, temporarily allows event records to burst to this number, while still not exceeding event-qps. The number must be >= 0. If 0 will use DefaultBurst: 10.")

	fs.BoolVar(&c.EnableDebuggingHandlers, "enable-debugging-handlers", c.EnableDebuggingHandlers, "Enables server endpoints for log collection and local running of containers and commands")
	fs.BoolVar(&c.EnableContentionProfiling, "contention-profiling", c.EnableContentionProfiling, "Enable block profiling, if profiling is enabled")
	fs.Int32Var(&c.HealthzPort, "healthz-port", c.HealthzPort, "The port of the localhost healthz endpoint (set to 0 to disable)")
	fs.Var(&utilflag.IPVar{Val: &c.HealthzBindAddress}, "healthz-bind-address", "The IP address for the healthz server to serve on (set to '0.0.0.0' or '::' for listening on all interfaces and IP address families)")
	fs.Int32Var(&c.OOMScoreAdj, "oom-score-adj", c.OOMScoreAdj, "The oom-score-adj value for kubelet process. Values must be within the range [-1000, 1000]")
	fs.StringVar(&c.ClusterDomain, "cluster-domain", c.ClusterDomain, "Domain for this cluster.  If set, kubelet will configure all containers to search this domain in addition to the host's search domains")

	fs.StringVar(&c.VolumePluginDir, "volume-plugin-dir", c.VolumePluginDir, "The full path of the directory in which to search for additional third party volume plugins")
	fs.StringSliceVar(&c.ClusterDNS, "cluster-dns", c.ClusterDNS, "Comma-separated list of DNS server IP address.  This value is used for containers DNS server in case of Pods with \"dnsPolicy=ClusterFirst\". Note: all DNS servers appearing in the list MUST serve the same set of records otherwise name resolution within the cluster may not work correctly. There is no guarantee as to which DNS server may be contacted for name resolution.")
	fs.DurationVar(&c.StreamingConnectionIdleTimeout.Duration, "streaming-connection-idle-timeout", c.StreamingConnectionIdleTimeout.Duration, "Maximum time a streaming connection can be idle before the connection is automatically closed. 0 indicates no timeout. Example: '5m'. Note: All connections to the kubelet server have a maximum duration of 4 hours.")
	fs.DurationVar(&c.NodeStatusUpdateFrequency.Duration, "node-status-update-frequency", c.NodeStatusUpdateFrequency.Duration, "Specifies how often kubelet posts node status to master. Note: be cautious when changing the constant, it must work with nodeMonitorGracePeriod in nodecontroller.")
	fs.DurationVar(&c.ImageMinimumGCAge.Duration, "minimum-image-ttl-duration", c.ImageMinimumGCAge.Duration, "Minimum age for an unused image before it is garbage collected.  Examples: '300ms', '10s' or '2h45m'.")
	fs.Int32Var(&c.ImageGCHighThresholdPercent, "image-gc-high-threshold", c.ImageGCHighThresholdPercent, "The percent of disk usage after which image garbage collection is always run. Values must be within the range [0, 100], To disable image garbage collection, set to 100. ")
	fs.Int32Var(&c.ImageGCLowThresholdPercent, "image-gc-low-threshold", c.ImageGCLowThresholdPercent, "The percent of disk usage before which image garbage collection is never run. Lowest disk usage to garbage collect to. Values must be within the range [0, 100] and must be less than that of --image-gc-high-threshold.")
	fs.DurationVar(&c.VolumeStatsAggPeriod.Duration, "volume-stats-agg-period", c.VolumeStatsAggPeriod.Duration, "Specifies interval for kubelet to calculate and cache the volume disk usage for all pods and volumes.  To disable volume calculations, set to a negative number.")
	fs.Var(cliflag.NewMapStringBool(&c.FeatureGates), "feature-gates", "A set of key=value pairs that describe feature gates for alpha/experimental features. "+
		"Options are:\n"+strings.Join(utilfeature.DefaultFeatureGate.KnownFeatures(), "\n"))
	fs.StringVar(&c.KubeletCgroups, "kubelet-cgroups", c.KubeletCgroups, "Optional absolute name of cgroups to create and run the Kubelet in.")
	fs.StringVar(&c.SystemCgroups, "system-cgroups", c.SystemCgroups, "Optional absolute name of cgroups in which to place all non-kernel processes that are not already inside a cgroup under '/'. Empty for no container. Rolling back the flag requires a reboot.")

	fs.StringVar(&c.ProviderID, "provider-id", c.ProviderID, "Unique identifier for identifying the node in a machine database, i.e cloudprovider")

	fs.BoolVar(&c.CgroupsPerQOS, "cgroups-per-qos", c.CgroupsPerQOS, "Enable creation of QoS cgroup hierarchy, if true top level QoS and pod cgroups are created.")
	fs.StringVar(&c.CgroupDriver, "cgroup-driver", c.CgroupDriver, "Driver that the kubelet uses to manipulate cgroups on the host.  Possible values: 'cgroupfs', 'systemd'")
	fs.StringVar(&c.CgroupRoot, "cgroup-root", c.CgroupRoot, "Optional root cgroup to use for pods. This is handled by the container runtime on a best effort basis. Default: '', which means use the container runtime default.")
	fs.StringVar(&c.CPUManagerPolicy, "cpu-manager-policy", c.CPUManagerPolicy, "CPU Manager policy to use. Possible values: 'none', 'static'.")
	fs.Var(cliflag.NewMapStringStringNoSplit(&c.CPUManagerPolicyOptions), "cpu-manager-policy-options", "A set of key=value CPU Manager policy options to use, to fine tune their behaviour. If not supplied, keep the default behaviour.")
	fs.DurationVar(&c.CPUManagerReconcilePeriod.Duration, "cpu-manager-reconcile-period", c.CPUManagerReconcilePeriod.Duration, "<Warning: Alpha feature> CPU Manager reconciliation period. Examples: '10s', or '1m'. If not supplied, defaults to 'NodeStatusUpdateFrequency'")
	fs.Var(cliflag.NewMapStringString(&c.QOSReserved), "qos-reserved", "<Warning: Alpha feature> A set of ResourceName=Percentage (e.g. memory=50%) pairs that describe how pod resource requests are reserved at the QoS level. Currently only memory is supported. Requires the QOSReserved feature gate to be enabled.")
	fs.StringVar(&c.TopologyManagerPolicy, "topology-manager-policy", c.TopologyManagerPolicy, "Topology Manager policy to use. Possible values: 'none', 'best-effort', 'restricted', 'single-numa-node'.")
	fs.DurationVar(&c.RuntimeRequestTimeout.Duration, "runtime-request-timeout", c.RuntimeRequestTimeout.Duration, "Timeout of all runtime requests except long running request - pull, logs, exec and attach. When timeout exceeded, kubelet will cancel the request, throw out an error and retry later.")
	fs.StringVar(&c.HairpinMode, "hairpin-mode", c.HairpinMode, "How should the kubelet setup hairpin NAT. This allows endpoints of a Service to loadbalance back to themselves if they should try to access their own Service. Valid values are \"promiscuous-bridge\", \"hairpin-veth\" and \"none\".")
	fs.Int32Var(&c.MaxPods, "max-pods", c.MaxPods, "Number of Pods that can run on this Kubelet.")

	fs.StringVar(&c.PodCIDR, "pod-cidr", c.PodCIDR, "The CIDR to use for pod IP addresses, only used in standalone mode.  In cluster mode, this is obtained from the master. For IPv6, the maximum number of IP's allocated is 65536")
	fs.Int64Var(&c.PodPidsLimit, "pod-max-pids", c.PodPidsLimit, "Set the maximum number of processes per pod.  If -1, the kubelet defaults to the node allocatable pid capacity.")

	fs.StringVar(&c.ResolverConfig, "resolv-conf", c.ResolverConfig, "Resolver configuration file used as the basis for the container DNS resolution configuration.")

	fs.BoolVar(&c.RunOnce, "runonce", c.RunOnce, "If true, exit after spawning pods from static pod files or remote urls. Exclusive with --enable-server")

	fs.BoolVar(&c.CPUCFSQuota, "cpu-cfs-quota", c.CPUCFSQuota, "Enable CPU CFS quota enforcement for containers that specify CPU limits")
	fs.DurationVar(&c.CPUCFSQuotaPeriod.Duration, "cpu-cfs-quota-period", c.CPUCFSQuotaPeriod.Duration, "Sets CPU CFS quota period value, cpu.cfs_period_us, defaults to Linux Kernel default")
	fs.BoolVar(&c.EnableControllerAttachDetach, "enable-controller-attach-detach", c.EnableControllerAttachDetach, "Enables the Attach/Detach controller to manage attachment/detachment of volumes scheduled to this node, and disables kubelet from executing any attach/detach operations")
	fs.BoolVar(&c.MakeIPTablesUtilChains, "make-iptables-util-chains", c.MakeIPTablesUtilChains, "If true, kubelet will ensure iptables utility rules are present on host.")
	fs.StringVar(&c.ContainerLogMaxSize, "container-log-max-size", c.ContainerLogMaxSize, "<Warning: Beta feature> Set the maximum size (e.g. 10Mi) of container log file before it is rotated.")
	fs.Int32Var(&c.ContainerLogMaxFiles, "container-log-max-files", c.ContainerLogMaxFiles, "<Warning: Beta feature> Set the maximum number of container log files that can be present for a container. The number must be >= 2.")
	fs.StringSliceVar(&c.AllowedUnsafeSysctls, "allowed-unsafe-sysctls", c.AllowedUnsafeSysctls, "Comma-separated whitelist of unsafe sysctls or unsafe sysctl patterns (ending in *). Use these at your own risk.")

	fs.Int32Var(&c.NodeStatusMaxImages, "node-status-max-images", c.NodeStatusMaxImages, "The maximum number of images to report in Node.Status.Images. If -1 is specified, no cap will be applied.")
	fs.BoolVar(&c.KernelMemcgNotification, "kernel-memcg-notification", c.KernelMemcgNotification, "If enabled, the kubelet will integrate with the kernel memcg notification to determine if memory eviction thresholds are crossed rather than polling.")
	fs.BoolVar(&c.LocalStorageCapacityIsolation, "local-storage-capacity-isolation", c.LocalStorageCapacityIsolation, "If true, local ephemeral storage isolation is enabled. Otherwise, local storage isolation feature will be disabled")

	// Flags intended for testing, not recommended used in production environments.
	fs.Int64Var(&c.MaxOpenFiles, "max-open-files", c.MaxOpenFiles, "Number of files that can be opened by Kubelet process.")

	fs.StringVar(&c.ContentType, "kube-api-content-type", c.ContentType, "Content type of requests sent to apiserver.")
	fs.Int32Var(&c.KubeAPIQPS, "kube-api-qps", c.KubeAPIQPS, "QPS to use while talking with kubernetes apiserver. The number must be >= 0. If 0 will use DefaultQPS: 50. Doesn't cover events and node heartbeat apis which rate limiting is controlled by a different set of flags")
	fs.Int32Var(&c.KubeAPIBurst, "kube-api-burst", c.KubeAPIBurst, "Burst to use while talking with kubernetes apiserver. The number must be >= 0. If 0 will use DefaultBurst: 100. Doesn't cover events and node heartbeat apis which rate limiting is controlled by a different set of flags")
	fs.BoolVar(&c.SerializeImagePulls, "serialize-image-pulls", c.SerializeImagePulls, "Pull images one at a time. We recommend *not* changing the default value on nodes that run docker daemon with version < 1.9 or an Aufs storage backend. Issue #10959 has more details.")

	fs.Var(cliflag.NewLangleSeparatedMapStringString(&c.EvictionHard), "eviction-hard", "A set of eviction thresholds (e.g. memory.available<1Gi) that if met would trigger a pod eviction.")
	fs.Var(cliflag.NewLangleSeparatedMapStringString(&c.EvictionSoft), "eviction-soft", "A set of eviction thresholds (e.g. memory.available<1.5Gi) that if met over a corresponding grace period would trigger a pod eviction.")
	fs.Var(cliflag.NewMapStringString(&c.EvictionSoftGracePeriod), "eviction-soft-grace-period", "A set of eviction grace periods (e.g. memory.available=1m30s) that correspond to how long a soft eviction threshold must hold before triggering a pod eviction.")
	fs.DurationVar(&c.EvictionPressureTransitionPeriod.Duration, "eviction-pressure-transition-period", c.EvictionPressureTransitionPeriod.Duration, "Duration for which the kubelet has to wait before transitioning out of an eviction pressure condition.")
	fs.Int32Var(&c.EvictionMaxPodGracePeriod, "eviction-max-pod-grace-period", c.EvictionMaxPodGracePeriod, "Maximum allowed grace period (in seconds) to use when terminating pods in response to a soft eviction threshold being met.  If negative, defer to pod specified value.")
	fs.Var(cliflag.NewMapStringString(&c.EvictionMinimumReclaim), "eviction-minimum-reclaim", "A set of minimum reclaims (e.g. imagefs.available=2Gi) that describes the minimum amount of resource the kubelet will reclaim when performing a pod eviction if that resource is under pressure.")
	fs.Int32Var(&c.PodsPerCore, "pods-per-core", c.PodsPerCore, "Number of Pods per core that can run on this Kubelet. The total number of Pods on this Kubelet cannot exceed max-pods, so max-pods will be used if this calculation results in a larger number of Pods allowed on the Kubelet. A value of 0 disables this limit.")
	fs.BoolVar(&c.ProtectKernelDefaults, "protect-kernel-defaults", c.ProtectKernelDefaults, "Default kubelet behaviour for kernel tuning. If set, kubelet errors if any of kernel tunables is different than kubelet defaults.")
	fs.StringVar(&c.ReservedSystemCPUs, "reserved-cpus", c.ReservedSystemCPUs, "A comma-separated list of CPUs or CPU ranges that are reserved for system and kubernetes usage. This specific list will supersede cpu counts in --system-reserved and --kube-reserved.")
	fs.StringVar(&c.TopologyManagerScope, "topology-manager-scope", c.TopologyManagerScope, "Scope to which topology hints applied. Topology Manager collects hints from Hint Providers and applies them to defined scope to ensure the pod admission. Possible values: 'container', 'pod'.")
	fs.Var(cliflag.NewMapStringString(&c.TopologyManagerPolicyOptions), "topology-manager-policy-options", "A set of key=value Topology Manager policy options to use, to fine tune their behaviour. If not supplied, keep the default behaviour.")
	// Node Allocatable Flags
	fs.Var(cliflag.NewMapStringString(&c.SystemReserved), "system-reserved", "A set of ResourceName=ResourceQuantity (e.g. cpu=200m,memory=500Mi,ephemeral-storage=1Gi,pid=1000) pairs that describe resources reserved for non-kubernetes components. Currently only cpu, memory, pid and local ephemeral storage for root file system are supported. See https://kubernetes.io/docs/concepts/configuration/manage-resources-containers/ for more detail. [default=none]")
	fs.Var(cliflag.NewMapStringString(&c.KubeReserved), "kube-reserved", "A set of ResourceName=ResourceQuantity (e.g. cpu=200m,memory=500Mi,ephemeral-storage=1Gi,pid=1000) pairs that describe resources reserved for kubernetes system components. Currently only cpu, memory, pid and local ephemeral storage for root file system are supported. See https://kubernetes.io/docs/concepts/configuration/manage-resources-containers/ for more detail. [default=none]")
	fs.StringSliceVar(&c.EnforceNodeAllocatable, "enforce-node-allocatable", c.EnforceNodeAllocatable, "A comma separated list of levels of node allocatable enforcement to be enforced by kubelet. Acceptable options are 'none', 'pods', 'system-reserved', 'system-reserved-compressible', 'kube-reserved' and 'kube-reserved-compressible'. If any of the latter four options are specified, '--system-reserved-cgroup' and '--kube-reserved-cgroup' must also be set, respectively. If 'none' is specified, no additional options should be set. See https://kubernetes.io/docs/tasks/administer-cluster/reserve-compute-resources/ for more details.")
	fs.StringVar(&c.SystemReservedCgroup, "system-reserved-cgroup", c.SystemReservedCgroup, "Absolute name of the top level cgroup that is used to manage non-kubernetes components for which compute resources were reserved via '--system-reserved' flag. Ex. '/system-reserved'. [default='']")
	fs.StringVar(&c.KubeReservedCgroup, "kube-reserved-cgroup", c.KubeReservedCgroup, "Absolute name of the top level cgroup that is used to manage kubernetes components for which compute resources were reserved via '--kube-reserved' flag. Ex. '/kube-reserved'. [default='']")
	logsapi.AddFlags(&c.Logging, fs)

	// Memory Manager Flags
	fs.StringVar(&c.MemoryManagerPolicy, "memory-manager-policy", c.MemoryManagerPolicy, "Memory Manager policy to use. Possible values: 'None', 'Static'.")
	fs.Var(&utilflag.ReservedMemoryVar{Value: &c.ReservedMemory}, "reserved-memory", "A comma separated list of memory reservations for NUMA nodes. (e.g. --reserved-memory 0:memory=1Gi,hugepages-1M=2Gi --reserved-memory 1:memory=2Gi). The total sum for each memory type should be equal to the sum of kube-reserved, system-reserved and eviction-threshold. See https://kubernetes.io/docs/tasks/administer-cluster/memory-manager/#reserved-memory-flag for more details.")

	fs.BoolVar(&c.RegisterNode, "register-node", c.RegisterNode, "Register the node with the apiserver. If --kubeconfig is not provided, this flag is irrelevant, as the Kubelet won't have an apiserver to register with.")

	fs.Var(&utilflag.RegisterWithTaintsVar{Value: &c.RegisterWithTaints}, "register-with-taints", "Register the node with the given list of taints (comma separated \"<key>=<value>:<effect>\"). No-op if register-node is false.")
}
