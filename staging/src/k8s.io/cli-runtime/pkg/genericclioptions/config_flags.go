/*
Copyright 2018 The Kubernetes Authors.

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

package genericclioptions

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/spf13/pflag"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/cli-runtime/pkg/genericiooptions"
	"k8s.io/cli-runtime/pkg/printers"
	"k8s.io/client-go/discovery"
	diskcached "k8s.io/client-go/discovery/cached/disk"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
	utilpointer "k8s.io/utils/pointer"
)

const (
	flagClusterName        = "cluster"
	flagAuthInfoName       = "user"
	flagContext            = "context"
	flagNamespace          = "namespace"
	flagAPIServer          = "server"
	flagTLSServerName      = "tls-server-name"
	flagInsecure           = "insecure-skip-tls-verify"
	flagCertFile           = "client-certificate"
	flagKeyFile            = "client-key"
	flagCAFile             = "certificate-authority"
	flagBearerToken        = "token"
	flagImpersonate        = "as"
	flagImpersonateUID     = "as-uid"
	flagImpersonateGroup   = "as-group"
	flagUsername           = "username"
	flagPassword           = "password"
	flagTimeout            = "request-timeout"
	flagCacheDir           = "cache-dir"
	flagDisableCompression = "disable-compression"
	flagProxyURL           = "proxy-url"
)

// RESTClientGetter is an interface that the ConfigFlags describe to provide an easier way to mock for commands
// and eliminate the direct coupling to a struct type.  Users may wish to duplicate this type in their own packages
// as per the golang type overlapping.
type RESTClientGetter interface {
	// ToRESTConfig returns restconfig
	ToRESTConfig() (*rest.Config, error)
	// ToDiscoveryClient returns discovery client
	ToDiscoveryClient() (discovery.CachedDiscoveryInterface, error)
	// ToRESTMapper returns a restmapper
	ToRESTMapper() (meta.RESTMapper, error)
	// ToRawKubeConfigLoader return kubeconfig loader as-is
	ToRawKubeConfigLoader() clientcmd.ClientConfig
}

var _ RESTClientGetter = &ConfigFlags{}

// ConfigFlags composes the set of values necessary
// for obtaining a REST client config
type ConfigFlags struct {
	CacheDir   *string
	KubeConfig *string

	// config flags
	ClusterName        *string
	AuthInfoName       *string
	Context            *string
	Namespace          *string
	APIServer          *string
	TLSServerName      *string
	Insecure           *bool
	CertFile           *string
	KeyFile            *string
	CAFile             *string
	BearerToken        *string
	Impersonate        *string
	ImpersonateUID     *string
	ImpersonateGroup   *[]string
	Username           *string
	Password           *string
	Timeout            *string
	DisableCompression *bool
	ProxyURL           *string
	// If non-nil, wrap config function can transform the Config
	// before it is returned in ToRESTConfig function.
	WrapConfigFn func(*rest.Config) *rest.Config

	clientConfig     clientcmd.ClientConfig
	clientConfigLock sync.Mutex

	restMapper     meta.RESTMapper
	restMapperLock sync.Mutex

	discoveryClient     discovery.CachedDiscoveryInterface
	discoveryClientLock sync.Mutex

	// If set to true, will use persistent client config, rest mapper, discovery client, and
	// propagate them to the places that need them, rather than
	// instantiating them multiple times.
	usePersistentConfig bool
	// Allows increasing burst used for discovery, this is useful
	// in clusters with many registered resources
	discoveryBurst int
	// Allows increasing qps used for discovery, this is useful
	// in clusters with many registered resources
	discoveryQPS float32
	// Allows all possible warnings are printed in a standardized
	// format.
	warningPrinter *printers.WarningPrinter
}

// ToRESTConfig implements RESTClientGetter.
// Returns a REST client configuration based on a provided path
// to a .kubeconfig file, loading rules, and config flag overrides.
// Expects the AddFlags method to have been called. If WrapConfigFn
// is non-nil this function can transform config before return.
func (f *ConfigFlags) ToRESTConfig() (*rest.Config, error) {
	c, err := f.ToRawKubeConfigLoader().ClientConfig()
	if err != nil {
		return nil, err
	}
	if f.WrapConfigFn != nil {
		return f.WrapConfigFn(c), nil
	}
	return c, nil
}

// ToRawKubeConfigLoader binds config flag values to config overrides
// Returns an interactive clientConfig if the password flag is enabled,
// or a non-interactive clientConfig otherwise.
func (f *ConfigFlags) ToRawKubeConfigLoader() clientcmd.ClientConfig {
	if f.usePersistentConfig {
		return f.toRawKubePersistentConfigLoader()
	}
	return f.toRawKubeConfigLoader()
}

func (f *ConfigFlags) toRawKubeConfigLoader() clientcmd.ClientConfig {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	// use the standard defaults for this client command
	// DEPRECATED: remove and replace with something more accurate
	loadingRules.DefaultClientConfig = &clientcmd.DefaultClientConfig

	if f.KubeConfig != nil {
		loadingRules.ExplicitPath = *f.KubeConfig
	}

	overrides := &clientcmd.ConfigOverrides{ClusterDefaults: clientcmd.ClusterDefaults}

	// bind auth info flag values to overrides
	if f.CertFile != nil {
		overrides.AuthInfo.ClientCertificate = *f.CertFile
	}
	if f.KeyFile != nil {
		overrides.AuthInfo.ClientKey = *f.KeyFile
	}
	if f.BearerToken != nil {
		overrides.AuthInfo.Token = *f.BearerToken
	}
	if f.Impersonate != nil {
		overrides.AuthInfo.Impersonate = *f.Impersonate
	}
	if f.ImpersonateUID != nil {
		overrides.AuthInfo.ImpersonateUID = *f.ImpersonateUID
	}
	if f.ImpersonateGroup != nil {
		overrides.AuthInfo.ImpersonateGroups = *f.ImpersonateGroup
	}
	if f.Username != nil {
		overrides.AuthInfo.Username = *f.Username
	}
	if f.Password != nil {
		overrides.AuthInfo.Password = *f.Password
	}

	// bind cluster flags
	if f.APIServer != nil {
		overrides.ClusterInfo.Server = *f.APIServer
	}
	if f.TLSServerName != nil {
		overrides.ClusterInfo.TLSServerName = *f.TLSServerName
	}
	if f.CAFile != nil {
		overrides.ClusterInfo.CertificateAuthority = *f.CAFile
	}
	if f.Insecure != nil {
		overrides.ClusterInfo.InsecureSkipTLSVerify = *f.Insecure
	}
	if f.DisableCompression != nil {
		overrides.ClusterInfo.DisableCompression = *f.DisableCompression
	}

	// bind context flags
	if f.Context != nil {
		overrides.CurrentContext = *f.Context
	}
	if f.ClusterName != nil {
		overrides.Context.Cluster = *f.ClusterName
	}
	if f.AuthInfoName != nil {
		overrides.Context.AuthInfo = *f.AuthInfoName
	}
	if f.Namespace != nil {
		overrides.Context.Namespace = *f.Namespace
	}

	if f.Timeout != nil {
		overrides.Timeout = *f.Timeout
	}

	// we only have an interactive prompt when a password is allowed
	if f.Password == nil {
		return &clientConfig{clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, overrides)}
	}

	if f.ProxyURL != nil {
		overrides.ClusterInfo.ProxyURL = *f.ProxyURL
	}

	return &clientConfig{clientcmd.NewInteractiveDeferredLoadingClientConfig(loadingRules, overrides, os.Stdin)}
}

// toRawKubePersistentConfigLoader binds config flag values to config overrides
// Returns a persistent clientConfig for propagation.
func (f *ConfigFlags) toRawKubePersistentConfigLoader() clientcmd.ClientConfig {
	f.clientConfigLock.Lock()
	defer f.clientConfigLock.Unlock()

	if f.clientConfig == nil {
		f.clientConfig = f.toRawKubeConfigLoader()
	}

	return f.clientConfig
}

// ToDiscoveryClient implements RESTClientGetter.
// Expects the AddFlags method to have been called.
// Returns a CachedDiscoveryInterface using a computed RESTConfig.
func (f *ConfigFlags) ToDiscoveryClient() (discovery.CachedDiscoveryInterface, error) {
	if f.usePersistentConfig {
		return f.toPersistentDiscoveryClient()
	}
	return f.toDiscoveryClient()
}

func (f *ConfigFlags) toPersistentDiscoveryClient() (discovery.CachedDiscoveryInterface, error) {
	f.discoveryClientLock.Lock()
	defer f.discoveryClientLock.Unlock()

	if f.discoveryClient == nil {
		discoveryClient, err := f.toDiscoveryClient()
		if err != nil {
			return nil, err
		}
		f.discoveryClient = discoveryClient
	}
	return f.discoveryClient, nil
}

func (f *ConfigFlags) toDiscoveryClient() (discovery.CachedDiscoveryInterface, error) {
	config, err := f.ToRESTConfig()
	if err != nil {
		return nil, err
	}

	config.Burst = f.discoveryBurst
	config.QPS = f.discoveryQPS

	cacheDir := getDefaultCacheDir()

	// retrieve a user-provided value for the "cache-dir"
	// override httpCacheDir and discoveryCacheDir if user-value is given.
	// user-provided value has higher precedence than default
	// and KUBECACHEDIR environment variable.
	if f.CacheDir != nil && *f.CacheDir != "" && *f.CacheDir != getDefaultCacheDir() {
		cacheDir = *f.CacheDir
	}

	httpCacheDir := filepath.Join(cacheDir, "http")
	discoveryCacheDir := computeDiscoverCacheDir(filepath.Join(cacheDir, "discovery"), config.Host)

	return diskcached.NewCachedDiscoveryClientForConfig(config, discoveryCacheDir, httpCacheDir, time.Duration(6*time.Hour))
}

// getDefaultCacheDir returns default caching directory path.
// it first looks at KUBECACHEDIR env var if it is set, otherwise
// it returns standard kube cache dir.
func getDefaultCacheDir() string {
	if kcd := os.Getenv("KUBECACHEDIR"); kcd != "" {
		return kcd
	}

	return filepath.Join(homedir.HomeDir(), ".kube", "cache")
}

// ToRESTMapper returns a mapper.
func (f *ConfigFlags) ToRESTMapper() (meta.RESTMapper, error) {
	if f.usePersistentConfig {
		return f.toPersistentRESTMapper()
	}
	return f.toRESTMapper()
}

func (f *ConfigFlags) toPersistentRESTMapper() (meta.RESTMapper, error) {
	f.restMapperLock.Lock()
	defer f.restMapperLock.Unlock()

	if f.restMapper == nil {
		restMapper, err := f.toRESTMapper()
		if err != nil {
			return nil, err
		}
		f.restMapper = restMapper
	}
	return f.restMapper, nil
}

func (f *ConfigFlags) toRESTMapper() (meta.RESTMapper, error) {
	discoveryClient, err := f.ToDiscoveryClient()
	if err != nil {
		return nil, err
	}

	mapper := restmapper.NewDeferredDiscoveryRESTMapper(discoveryClient)
	expander := restmapper.NewShortcutExpander(mapper, discoveryClient, func(a string) {
		if f.warningPrinter != nil {
			f.warningPrinter.Print(a)
		}
	})
	return expander, nil
}

// AddFlags binds client configuration flags to a given flagset
func (f *ConfigFlags) AddFlags(flags *pflag.FlagSet) {
	if f.KubeConfig != nil {
		flags.StringVar(f.KubeConfig, "kubeconfig", *f.KubeConfig, "Path to the kubeconfig file to use for CLI requests.")
	}
	if f.CacheDir != nil {
		flags.StringVar(f.CacheDir, flagCacheDir, *f.CacheDir, "Default cache directory")
	}

	// add config options
	if f.CertFile != nil {
		flags.StringVar(f.CertFile, flagCertFile, *f.CertFile, "Path to a client certificate file for TLS")
	}
	if f.KeyFile != nil {
		flags.StringVar(f.KeyFile, flagKeyFile, *f.KeyFile, "Path to a client key file for TLS")
	}
	if f.BearerToken != nil {
		flags.StringVar(f.BearerToken, flagBearerToken, *f.BearerToken, "Bearer token for authentication to the API server")
	}
	if f.Impersonate != nil {
		flags.StringVar(f.Impersonate, flagImpersonate, *f.Impersonate, "Username to impersonate for the operation. User could be a regular user or a service account in a namespace.")
	}
	if f.ImpersonateUID != nil {
		flags.StringVar(f.ImpersonateUID, flagImpersonateUID, *f.ImpersonateUID, "UID to impersonate for the operation.")
	}
	if f.ImpersonateGroup != nil {
		flags.StringArrayVar(f.ImpersonateGroup, flagImpersonateGroup, *f.ImpersonateGroup, "Group to impersonate for the operation, this flag can be repeated to specify multiple groups.")
	}
	if f.Username != nil {
		flags.StringVar(f.Username, flagUsername, *f.Username, "Username for basic authentication to the API server")
	}
	if f.Password != nil {
		flags.StringVar(f.Password, flagPassword, *f.Password, "Password for basic authentication to the API server")
	}
	if f.ClusterName != nil {
		flags.StringVar(f.ClusterName, flagClusterName, *f.ClusterName, "The name of the kubeconfig cluster to use")
	}
	if f.AuthInfoName != nil {
		flags.StringVar(f.AuthInfoName, flagAuthInfoName, *f.AuthInfoName, "The name of the kubeconfig user to use")
	}
	if f.Namespace != nil {
		flags.StringVarP(f.Namespace, flagNamespace, "n", *f.Namespace, "If present, the namespace scope for this CLI request")
	}
	if f.Context != nil {
		flags.StringVar(f.Context, flagContext, *f.Context, "The name of the kubeconfig context to use")
	}

	if f.APIServer != nil {
		flags.StringVarP(f.APIServer, flagAPIServer, "s", *f.APIServer, "The address and port of the Kubernetes API server")
	}
	if f.TLSServerName != nil {
		flags.StringVar(f.TLSServerName, flagTLSServerName, *f.TLSServerName, "Server name to use for server certificate validation. If it is not provided, the hostname used to contact the server is used")
	}
	if f.Insecure != nil {
		flags.BoolVar(f.Insecure, flagInsecure, *f.Insecure, "If true, the server's certificate will not be checked for validity. This will make your HTTPS connections insecure")
	}
	if f.CAFile != nil {
		flags.StringVar(f.CAFile, flagCAFile, *f.CAFile, "Path to a cert file for the certificate authority")
	}
	if f.Timeout != nil {
		flags.StringVar(f.Timeout, flagTimeout, *f.Timeout, "The length of time to wait before giving up on a single server request. Non-zero values should contain a corresponding time unit (e.g. 1s, 2m, 3h). A value of zero means don't timeout requests.")
	}
	if f.DisableCompression != nil {
		flags.BoolVar(f.DisableCompression, flagDisableCompression, *f.DisableCompression, "If true, opt-out of response compression for all requests to the server")
	}
	if f.ProxyURL != nil {
		flags.StringVar(f.ProxyURL, flagProxyURL, *f.ProxyURL, "If provided, this URL will be used to connect via proxy")
	}
}

// WithDeprecatedPasswordFlag enables the username and password config flags
func (f *ConfigFlags) WithDeprecatedPasswordFlag() *ConfigFlags {
	f.Username = utilpointer.String("")
	f.Password = utilpointer.String("")
	return f
}

// WithDiscoveryBurst sets the RESTClient burst for discovery.
func (f *ConfigFlags) WithDiscoveryBurst(discoveryBurst int) *ConfigFlags {
	f.discoveryBurst = discoveryBurst
	return f
}

// WithDiscoveryQPS sets the RESTClient QPS for discovery.
func (f *ConfigFlags) WithDiscoveryQPS(discoveryQPS float32) *ConfigFlags {
	f.discoveryQPS = discoveryQPS
	return f
}

// WithWrapConfigFn allows providing a wrapper function for the client Config.
func (f *ConfigFlags) WithWrapConfigFn(wrapConfigFn func(*rest.Config) *rest.Config) *ConfigFlags {
	f.WrapConfigFn = wrapConfigFn
	return f
}

// WithWarningPrinter initializes WarningPrinter with the given IOStreams
func (f *ConfigFlags) WithWarningPrinter(ioStreams genericiooptions.IOStreams) *ConfigFlags {
	f.warningPrinter = printers.NewWarningPrinter(ioStreams.ErrOut, printers.WarningPrinterOptions{Color: printers.AllowsColorOutput(ioStreams.ErrOut)})
	return f
}

// NewConfigFlags returns ConfigFlags with default values set
func NewConfigFlags(usePersistentConfig bool) *ConfigFlags {
	impersonateGroup := []string{}
	insecure := false
	disableCompression := false

	return &ConfigFlags{
		Insecure:   &insecure,
		Timeout:    utilpointer.String("0"),
		KubeConfig: utilpointer.String(""),

		CacheDir:           utilpointer.String(getDefaultCacheDir()),
		ClusterName:        utilpointer.String(""),
		AuthInfoName:       utilpointer.String(""),
		Context:            utilpointer.String(""),
		Namespace:          utilpointer.String(""),
		APIServer:          utilpointer.String(""),
		TLSServerName:      utilpointer.String(""),
		CertFile:           utilpointer.String(""),
		KeyFile:            utilpointer.String(""),
		CAFile:             utilpointer.String(""),
		BearerToken:        utilpointer.String(""),
		Impersonate:        utilpointer.String(""),
		ImpersonateUID:     utilpointer.String(""),
		ProxyURL:           utilpointer.String(""),
		ImpersonateGroup:   &impersonateGroup,
		DisableCompression: &disableCompression,

		usePersistentConfig: usePersistentConfig,
		// The more groups you have, the more discovery requests you need to make.
		// with a burst of 300, we will not be rate-limiting for most clusters but
		// the safeguard will still be here. This config is only used for discovery.
		discoveryBurst: 300,
	}
}

// overlyCautiousIllegalFileCharacters matches characters that *might* not be supported.  Windows is really restrictive, so this is really restrictive
var overlyCautiousIllegalFileCharacters = regexp.MustCompile(`[^(\w/.)]`)

// computeDiscoverCacheDir takes the parentDir and the host and comes up with a "usually non-colliding" name.
func computeDiscoverCacheDir(parentDir, host string) string {
	// strip the optional scheme from host if its there:
	schemelessHost := strings.Replace(strings.Replace(host, "https://", "", 1), "http://", "", 1)
	// now do a simple collapse of non-AZ09 characters.  Collisions are possible but unlikely.  Even if we do collide the problem is short lived
	safeHost := overlyCautiousIllegalFileCharacters.ReplaceAllString(schemelessHost, "_")
	return filepath.Join(parentDir, safeHost)
}
