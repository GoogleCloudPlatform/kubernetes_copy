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

package server

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	_ "k8s.io/apiserver/pkg/admission"
	genericapifilters "k8s.io/apiserver/pkg/endpoints/filters"
	genericapiserver "k8s.io/apiserver/pkg/server"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/apiserver/pkg/util/notfoundhandler"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	cliflag "k8s.io/component-base/cli/flag"
	"k8s.io/component-base/cli/globalflag"
	"k8s.io/component-base/logs"
	logsapi "k8s.io/component-base/logs/api/v1"
	_ "k8s.io/component-base/metrics/prometheus/workqueue"
	"k8s.io/component-base/term"
	"k8s.io/component-base/version"
	"k8s.io/component-base/version/verflag"
	"k8s.io/klog/v2"
	aggregatorapiserver "k8s.io/kube-aggregator/pkg/apiserver"

	controlplaneapiserver "k8s.io/kubernetes/pkg/controlplane/apiserver"
	"k8s.io/kubernetes/pkg/controlplane/apiserver/options"
	_ "k8s.io/kubernetes/pkg/features"
	// add the kubernetes feature gates
)

func init() {
	utilruntime.Must(logsapi.AddFeatureGates(utilfeature.DefaultMutableFeatureGate))
}

// NewCommand creates a *cobra.Command object with default parameters
func NewCommand() *cobra.Command {
	s := NewOptions()
	cmd := &cobra.Command{
		Use: "sample-generic-apiserver",
		Long: `The sample generic apiserver is part of a generic controlplane,
a system serving APIs like Kubernetes, but without the container domain specific
APIs.`,

		// stop printing usage when the command errors
		SilenceUsage: true,
		PersistentPreRunE: func(*cobra.Command, []string) error {
			// silence client-go warnings.
			// kube-apiserver loopback clients should not log self-issued warnings.
			rest.SetDefaultWarningHandler(rest.NoWarnings{})
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			verflag.PrintAndExitIfRequested()
			fs := cmd.Flags()

			// Activate logging as soon as possible, after that
			// show flags with the final logging configuration.
			if err := logsapi.ValidateAndApply(s.Logs, utilfeature.DefaultFeatureGate); err != nil {
				return err
			}
			cliflag.PrintFlags(fs)

			ctx := genericapiserver.SetupSignalContext()

			completedOptions, err := s.Complete(ctx, cliflag.NamedFlagSets{FlagSets: map[string]*pflag.FlagSet{"sample_generic_controlplane": fs}}, []string{}, []net.IP{})
			if err != nil {
				return err
			}

			if errs := completedOptions.Validate(); len(errs) != 0 {
				return utilerrors.NewAggregate(errs)
			}

			// add feature enablement metrics
			utilfeature.DefaultMutableFeatureGate.AddMetrics()

			return Run(ctx, completedOptions)
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

	var namedFlagSets cliflag.NamedFlagSets
	s.AddFlags(&namedFlagSets)
	verflag.AddFlags(namedFlagSets.FlagSet("global"))
	globalflag.AddGlobalFlags(namedFlagSets.FlagSet("global"), cmd.Name(), logs.SkipLoggingConfigurationFlags())

	fs := cmd.Flags()
	for _, f := range namedFlagSets.FlagSets {
		fs.AddFlagSet(f)
	}

	cols, _, _ := term.TerminalSize(cmd.OutOrStdout())
	cliflag.SetUsageAndHelpFunc(cmd, namedFlagSets, cols)

	return cmd
}

func NewOptions() *options.Options {
	s := options.NewOptions()
	s.Admission.GenericAdmission.DefaultOffPlugins = DefaultOffAdmissionPlugins()

	wd, _ := os.Getwd()
	s.SecureServing.ServerCert.CertDirectory = filepath.Join(wd, ".sample-minimal-controlplane")

	// Wire ServiceAccount authentication without relying on pods and nodes.
	s.Authentication.ServiceAccounts.OptionalTokenGetter = genericTokenGetter

	return s
}

// Run runs the specified APIServer. This should never exit.
func Run(ctx context.Context, opts options.CompletedOptions) error {
	// To help debugging, immediately log version
	klog.Infof("Version: %+v", version.Get())

	klog.InfoS("Golang settings", "GOGC", os.Getenv("GOGC"), "GOMAXPROCS", os.Getenv("GOMAXPROCS"), "GOTRACEBACK", os.Getenv("GOTRACEBACK"))

	config, err := NewConfig(opts)
	if err != nil {
		return err
	}
	completed, err := config.Complete()
	if err != nil {
		return err
	}
	server, err := CreateServerChain(ctx, completed)
	if err != nil {
		return err
	}

	prepared, err := server.PrepareRun()
	if err != nil {
		return err
	}

	return prepared.Run(ctx)
}

// CreateServerChain creates the apiservers connected via delegation.
func CreateServerChain(ctx context.Context, config CompletedConfig) (*aggregatorapiserver.APIAggregator, error) {
	// 1. CRDs
	notFoundHandler := notfoundhandler.New(config.ControlPlane.Generic.Serializer, genericapifilters.NoMuxAndDiscoveryIncompleteKey)
	apiExtensionsServer, err := config.APIExtensions.New(genericapiserver.NewEmptyDelegateWithCustomHandler(notFoundHandler))
	if err != nil {
		return nil, fmt.Errorf("failed to create apiextensions-apiserver: %w", err)
	}
	crdAPIEnabled := config.APIExtensions.GenericConfig.MergedResourceConfig.ResourceEnabled(apiextensionsv1.SchemeGroupVersion.WithResource("customresourcedefinitions"))

	// 2. Natively implemented resources
	nativeAPIs, err := config.ControlPlane.New("sample-generic-controlplane", apiExtensionsServer.GenericAPIServer)
	if err != nil {
		return nil, fmt.Errorf("failed to create generic controlplane apiserver: %w", err)
	}
	client, err := kubernetes.NewForConfig(config.ControlPlane.Generic.LoopbackClientConfig)
	if err != nil {
		return nil, err
	}
	storageProviders, err := config.ControlPlane.GenericStorageProviders(client.Discovery())
	if err != nil {
		return nil, fmt.Errorf("failed to create storage providers: %w", err)
	}
	if err := nativeAPIs.InstallAPIs(storageProviders...); err != nil {
		return nil, fmt.Errorf("failed to install APIs: %w", err)
	}

	// 3. Aggregator for APIServices, discovery and OpenAPI
	aggregatorServer, err := controlplaneapiserver.CreateAggregatorServer(ctx, config.Aggregator, nativeAPIs.GenericAPIServer, apiExtensionsServer.Informers.Apiextensions().V1().CustomResourceDefinitions(), crdAPIEnabled, controlplaneapiserver.DefaultGenericAPIServicePriorities())
	if err != nil {
		// we don't need special handling for innerStopCh because the aggregator server doesn't create any go routines
		return nil, fmt.Errorf("failed to create kube-aggregator: %w", err)
	}

	return aggregatorServer, nil
}
