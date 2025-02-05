/*
Copyright 2023 The Kubernetes Authors.

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
	goflag "flag"
	"fmt"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"go.opentelemetry.io/otel/trace/noop"
	internalapi "k8s.io/cri-api/pkg/apis"
	"k8s.io/klog/v2"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	clientset "k8s.io/client-go/kubernetes"
	restclient "k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/events"
	cliflag "k8s.io/component-base/cli/flag"
	_ "k8s.io/component-base/metrics/prometheus/restclient" // for client metric registration
	_ "k8s.io/component-base/metrics/prometheus/version"    // for version metric registration
	"k8s.io/component-base/version"
	"k8s.io/component-base/version/verflag"
	remote "k8s.io/cri-client/pkg"
	fakeremote "k8s.io/cri-client/pkg/fake"
	"k8s.io/kubernetes/pkg/api/legacyscheme"
	"k8s.io/kubernetes/pkg/cluster/ports"
	cadvisortest "k8s.io/kubernetes/pkg/kubelet/cadvisor/testing"
	"k8s.io/kubernetes/pkg/kubelet/certificate/bootstrap"
	"k8s.io/kubernetes/pkg/kubelet/cm"
	"k8s.io/kubernetes/pkg/kubemark"
	kubemarkproxy "k8s.io/kubernetes/pkg/proxy/kubemark"
	utilflag "k8s.io/kubernetes/pkg/util/flag"
)

type hollowNodeConfig struct {
	KubeconfigPath          string
	BootstrapKubeconfigPath string
	CertDirectory           string
	KubeletPort             int
	KubeletReadOnlyPort     int
	Morph                   string
	NodeName                string
	ServerPort              int
	ContentType             string
	QPS                     float32
	Burst                   int
	NodeLabels              map[string]string
	RegisterWithTaints      []v1.Taint
	MaxPods                 int
	ExtendedResources       map[string]string
	UseHostImageService     bool

	// Deprecated config; remove these with the corresponding flags
	UseRealProxier       bool
	ProxierSyncPeriod    time.Duration
	ProxierMinSyncPeriod time.Duration
}

const (
	maxPods     = 110
	podsPerCore = 0
)

// TODO(#45650): Refactor hollow-node into hollow-kubelet and hollow-proxy
// and make the config driven.
var knownMorphs = sets.NewString("kubelet", "proxy")

func (c *hollowNodeConfig) addFlags(fs *pflag.FlagSet) {
	fs.StringVar(&c.KubeconfigPath, "kubeconfig", "/kubeconfig/kubeconfig", "Path to kubeconfig file.")
	fs.StringVar(&c.BootstrapKubeconfigPath, "bootstrap-kubeconfig", "", "Path to bootstrap kubeconfig file.")
	fs.StringVar(&c.CertDirectory, "cert-dir", "/etc/srv/", "Path to cert directory for bootstraping.")
	fs.IntVar(&c.KubeletPort, "kubelet-port", ports.KubeletPort, "Port on which HollowKubelet should be listening.")
	fs.IntVar(&c.KubeletReadOnlyPort, "kubelet-read-only-port", ports.KubeletReadOnlyPort, "Read-only port on which Kubelet is listening.")
	fs.StringVar(&c.NodeName, "name", "fake-node", "Name of this Hollow Node.")
	fs.IntVar(&c.ServerPort, "api-server-port", 443, "Port on which API server is listening.")
	fs.StringVar(&c.Morph, "morph", "", fmt.Sprintf("Specifies into which Hollow component this binary should morph. Allowed values: %v", knownMorphs.List()))
	fs.StringVar(&c.ContentType, "kube-api-content-type", "application/vnd.kubernetes.protobuf", "ContentType of requests sent to apiserver.")
	fs.Float32Var(&c.QPS, "kube-api-qps", 10, "QPS indicates the maximum QPS to the apiserver.")
	fs.IntVar(&c.Burst, "kube-api-burst", 20, "Burst indicates maximum burst for throttle to the apiserver.")

	bindableNodeLabels := cliflag.ConfigurationMap(c.NodeLabels)
	fs.Var(&bindableNodeLabels, "node-labels", "Additional node labels")
	fs.Var(utilflag.RegisterWithTaintsVar{Value: &c.RegisterWithTaints}, "register-with-taints", "Register the node with the given list of taints (comma separated \"<key>=<value>:<effect>\"). No-op if register-node is false.")
	fs.IntVar(&c.MaxPods, "max-pods", maxPods, "Number of pods that can run on this Kubelet.")
	bindableExtendedResources := cliflag.ConfigurationMap(c.ExtendedResources)
	fs.Var(&bindableExtendedResources, "extended-resources", "Register the node with extended resources (comma separated \"<name>=<quantity>\")")
	fs.BoolVar(&c.UseHostImageService, "use-host-image-service", true, "Set to true if the hollow-kubelet should use the host image service. If set to false the fake image service will be used")

	fs.BoolVar(&c.UseRealProxier, "use-real-proxier", true, "Has no effect.")
	_ = fs.MarkDeprecated("use-real-proxier", "This flag is deprecated and will be removed in a future release.")
	fs.DurationVar(&c.ProxierSyncPeriod, "proxier-sync-period", 30*time.Second, "Has no effect.")
	_ = fs.MarkDeprecated("proxier-sync-period", "This flag is deprecated and will be removed in a future release.")
	fs.DurationVar(&c.ProxierMinSyncPeriod, "proxier-min-sync-period", 0, "Has no effect.")
	_ = fs.MarkDeprecated("proxier-min-sync-period", "This flag is deprecated and will be removed in a future release.")
}

func (c *hollowNodeConfig) createClientConfigFromFile() (*restclient.Config, error) {
	clientConfig, err := clientcmd.LoadFromFile(c.KubeconfigPath)
	if err != nil {
		return nil, fmt.Errorf("error while loading kubeconfig from file %v: %v", c.KubeconfigPath, err)
	}
	config, err := clientcmd.NewDefaultClientConfig(*clientConfig, &clientcmd.ConfigOverrides{}).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("error while creating kubeconfig: %v", err)
	}
	config.ContentType = c.ContentType
	config.QPS = c.QPS
	config.Burst = c.Burst
	return config, nil
}

func (c *hollowNodeConfig) bootstrapClientConfig() error {
	if c.BootstrapKubeconfigPath != "" {
		return bootstrap.LoadClientCert(context.TODO(), c.KubeconfigPath, c.BootstrapKubeconfigPath, c.CertDirectory, types.NodeName(c.NodeName))
	}
	return nil
}

func (c *hollowNodeConfig) createHollowKubeletOptions() *kubemark.HollowKubeletOptions {
	return &kubemark.HollowKubeletOptions{
		NodeName:            c.NodeName,
		KubeletPort:         c.KubeletPort,
		KubeletReadOnlyPort: c.KubeletReadOnlyPort,
		MaxPods:             c.MaxPods,
		PodsPerCore:         podsPerCore,
		NodeLabels:          c.NodeLabels,
		RegisterWithTaints:  c.RegisterWithTaints,
	}
}

// NewHollowNodeCommand creates a *cobra.Command object with default parameters
func NewHollowNodeCommand() *cobra.Command {
	s := &hollowNodeConfig{
		NodeLabels:        make(map[string]string),
		ExtendedResources: make(map[string]string),
	}

	cmd := &cobra.Command{
		Use:  "kubemark",
		Long: "kubemark",
		RunE: func(cmd *cobra.Command, args []string) error {
			verflag.PrintAndExitIfRequested()
			cliflag.PrintFlags(cmd.Flags())
			return run(cmd.Context(), s)
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
	fs.AddGoFlagSet(goflag.CommandLine) // for flags like --docker-only
	s.addFlags(fs)

	return cmd
}

func run(ctx context.Context, config *hollowNodeConfig) error {
	// To help debugging, immediately log version and print flags.
	logger := klog.FromContext(ctx)
	logger.Info("Version information", "version", version.Get())

	if !knownMorphs.Has(config.Morph) {
		return fmt.Errorf("Unknown morph: %v. allowed values: %v", config.Morph, knownMorphs.List())
	}

	// create a client to communicate with API server.
	err := config.bootstrapClientConfig()
	if err != nil {
		return fmt.Errorf("Failed to bootstrap, error: %w. Exiting", err)
	}
	clientConfig, err := config.createClientConfigFromFile()
	if err != nil {
		return fmt.Errorf("Failed to create a ClientConfig, error: %w. Exiting", err)
	}

	if config.Morph == "kubelet" {
		clientConfig.UserAgent = "hollow-kubelet"
		client, err := clientset.NewForConfig(clientConfig)
		if err != nil {
			return fmt.Errorf("Failed to create a ClientSet, error: %w. Exiting", err)
		}

		f, c := kubemark.GetHollowKubeletConfig(config.createHollowKubeletOptions())

		heartbeatClientConfig := *clientConfig
		heartbeatClientConfig.Timeout = c.NodeStatusUpdateFrequency.Duration
		// The timeout is the minimum of the lease duration and status update frequency
		leaseTimeout := time.Duration(c.NodeLeaseDurationSeconds) * time.Second
		if heartbeatClientConfig.Timeout > leaseTimeout {
			heartbeatClientConfig.Timeout = leaseTimeout
		}

		heartbeatClientConfig.QPS = float32(-1)
		heartbeatClient, err := clientset.NewForConfig(&heartbeatClientConfig)
		if err != nil {
			return fmt.Errorf("Failed to create a ClientSet, error: %w. Exiting", err)
		}

		cadvisorInterface := &cadvisortest.Fake{
			NodeName: config.NodeName,
		}

		var containerManager cm.ContainerManager
		if config.ExtendedResources != nil {
			extendedResources := v1.ResourceList{}
			for k, v := range config.ExtendedResources {
				extendedResources[v1.ResourceName(k)] = resource.MustParse(v)
			}

			containerManager = cm.NewStubContainerManagerWithDevicePluginResource(extendedResources)
		} else {
			containerManager = cm.NewStubContainerManager()
		}

		endpoint, err := fakeremote.GenerateEndpoint()
		if err != nil {
			return fmt.Errorf("Failed to generate fake endpoint, error: %w", err)
		}
		fakeRemoteRuntime := fakeremote.NewFakeRemoteRuntime()
		if err = fakeRemoteRuntime.Start(endpoint); err != nil {
			return fmt.Errorf("Failed to start fake runtime, error: %w", err)
		}
		defer fakeRemoteRuntime.Stop()
		logger := klog.Background()
		runtimeService, err := remote.NewRemoteRuntimeService(endpoint, 15*time.Second, noop.NewTracerProvider(), &logger)
		if err != nil {
			return fmt.Errorf("Failed to init runtime service, error: %w", err)
		}

		var imageService internalapi.ImageManagerService = fakeRemoteRuntime.ImageService
		if config.UseHostImageService {
			imageService, err = remote.NewRemoteImageService(c.ImageServiceEndpoint, 15*time.Second, noop.NewTracerProvider(), &logger)
			if err != nil {
				return fmt.Errorf("Failed to init image service, error: %w", err)
			}
		}

		hollowKubelet := kubemark.NewHollowKubelet(
			f, c,
			client,
			heartbeatClient,
			cadvisorInterface,
			imageService,
			runtimeService,
			containerManager,
		)
		hollowKubelet.Run(ctx)
	}

	if config.Morph == "proxy" {
		clientConfig.UserAgent = "hollow-proxy"

		client, err := clientset.NewForConfig(clientConfig)
		if err != nil {
			return fmt.Errorf("Failed to create API Server client, error: %w", err)
		}
		eventBroadcaster := events.NewBroadcaster(&events.EventSinkImpl{Interface: client.EventsV1()})
		recorder := eventBroadcaster.NewRecorder(legacyscheme.Scheme, "kube-proxy")

		hollowProxy := kubemarkproxy.NewHollowProxy(
			config.NodeName,
			client,
			client.CoreV1(),
			eventBroadcaster,
			recorder,
		)
		return hollowProxy.Run()
	}

	return nil
}
