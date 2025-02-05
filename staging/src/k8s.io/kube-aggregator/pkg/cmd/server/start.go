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

package server

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	openapinamer "k8s.io/apiserver/pkg/endpoints/openapi"

	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/sets"
	genericapiserver "k8s.io/apiserver/pkg/server"
	"k8s.io/apiserver/pkg/server/filters"
	genericoptions "k8s.io/apiserver/pkg/server/options"
	"k8s.io/kube-aggregator/pkg/apis/apiregistration/v1beta1"
	"k8s.io/kube-aggregator/pkg/apiserver"
	aggregatorscheme "k8s.io/kube-aggregator/pkg/apiserver/scheme"
	"k8s.io/kube-aggregator/pkg/generated/openapi"
)

const defaultEtcdPathPrefix = "/registry/kube-aggregator.kubernetes.io/"

// AggregatorOptions contains everything necessary to create and run an API Aggregator.
type AggregatorOptions struct {
	ServerRunOptions   *genericoptions.ServerRunOptions
	RecommendedOptions *genericoptions.RecommendedOptions
	APIEnablement      *genericoptions.APIEnablementOptions

	// ProxyClientCert/Key are the client cert used to identify this proxy. Backing APIServices use
	// this to confirm the proxy's identity
	ProxyClientCertFile string
	ProxyClientKeyFile  string

	StdOut io.Writer
	StdErr io.Writer
}

// NewCommandStartAggregator provides a CLI handler for 'start master' command
// with a default AggregatorOptions.
func NewCommandStartAggregator(ctx context.Context, defaults *AggregatorOptions) *cobra.Command {
	o := *defaults
	cmd := &cobra.Command{
		Short: "Launch a API aggregator and proxy server",
		Long:  "Launch a API aggregator and proxy server",
		PersistentPreRunE: func(*cobra.Command, []string) error {
			return o.ServerRunOptions.ComponentGlobalsRegistry.Set()
		},
		RunE: func(c *cobra.Command, args []string) error {
			if err := o.Complete(); err != nil {
				return err
			}
			if err := o.Validate(args); err != nil {
				return err
			}
			if err := o.RunAggregator(c.Context()); err != nil {
				return err
			}
			return nil
		},
	}
	cmd.SetContext(ctx)

	o.AddFlags(cmd.Flags())
	return cmd
}

// AddFlags is necessary because hyperkube doesn't work using cobra, so we have to have different registration and execution paths
func (o *AggregatorOptions) AddFlags(fs *pflag.FlagSet) {
	o.ServerRunOptions.AddUniversalFlags(fs)
	o.RecommendedOptions.AddFlags(fs)
	o.APIEnablement.AddFlags(fs)
	fs.StringVar(&o.ProxyClientCertFile, "proxy-client-cert-file", o.ProxyClientCertFile, "client certificate used identify the proxy to the API server")
	fs.StringVar(&o.ProxyClientKeyFile, "proxy-client-key-file", o.ProxyClientKeyFile, "client certificate key used identify the proxy to the API server")
}

// NewDefaultOptions builds a "normal" set of options.  You wouldn't normally expose this, but hyperkube isn't cobra compatible
func NewDefaultOptions(out, err io.Writer) *AggregatorOptions {
	o := &AggregatorOptions{
		ServerRunOptions: genericoptions.NewServerRunOptions(),
		RecommendedOptions: genericoptions.NewRecommendedOptions(
			defaultEtcdPathPrefix,
			aggregatorscheme.Codecs.LegacyCodec(v1beta1.SchemeGroupVersion),
		),
		APIEnablement: genericoptions.NewAPIEnablementOptions(),

		StdOut: out,
		StdErr: err,
	}

	return o
}

// Validate validates all the required options.
func (o AggregatorOptions) Validate(args []string) error {
	errors := []error{}
	errors = append(errors, o.ServerRunOptions.Validate()...)
	errors = append(errors, o.RecommendedOptions.Validate()...)
	errors = append(errors, o.APIEnablement.Validate(aggregatorscheme.Scheme)...)
	return utilerrors.NewAggregate(errors)
}

// Complete fills in missing Options.
func (o *AggregatorOptions) Complete() error {
	return o.ServerRunOptions.Complete()
}

// RunAggregator runs the API Aggregator.
func (o AggregatorOptions) RunAggregator(ctx context.Context) error {
	// TODO have a "real" external address
	if err := o.RecommendedOptions.SecureServing.MaybeDefaultWithSelfSignedCerts("localhost", nil, nil); err != nil {
		return fmt.Errorf("error creating self-signed certificates: %v", err)
	}

	serverConfig := genericapiserver.NewRecommendedConfig(aggregatorscheme.Codecs)

	if err := o.ServerRunOptions.ApplyTo(&serverConfig.Config); err != nil {
		return err
	}
	if err := o.RecommendedOptions.ApplyTo(serverConfig); err != nil {
		return err
	}
	if err := o.APIEnablement.ApplyTo(&serverConfig.Config, apiserver.DefaultAPIResourceConfigSource(), aggregatorscheme.Scheme); err != nil {
		return err
	}
	serverConfig.LongRunningFunc = filters.BasicLongRunningRequestCheck(
		sets.NewString("watch", "proxy"),
		sets.NewString("attach", "exec", "proxy", "log", "portforward"),
	)
	serverConfig.OpenAPIConfig = genericapiserver.DefaultOpenAPIConfig(openapi.GetOpenAPIDefinitions, openapinamer.NewDefinitionNamer(aggregatorscheme.Scheme))
	serverConfig.OpenAPIConfig.Info.Title = "kube-aggregator"
	// prevent generic API server from installing the OpenAPI handler. Aggregator server
	// has its own customized OpenAPI handler.
	serverConfig.SkipOpenAPIInstallation = true

	serviceResolver := apiserver.NewClusterIPServiceResolver(serverConfig.SharedInformerFactory.Core().V1().Services().Lister())

	config := apiserver.Config{
		GenericConfig: serverConfig,
		ExtraConfig: apiserver.ExtraConfig{
			ServiceResolver: serviceResolver,
		},
	}

	if len(o.ProxyClientCertFile) == 0 || len(o.ProxyClientKeyFile) == 0 {
		return errors.New("missing a client certificate along with a key to identify the proxy to the API server")
	}

	config.ExtraConfig.ProxyClientCertFile = o.ProxyClientCertFile
	config.ExtraConfig.ProxyClientKeyFile = o.ProxyClientKeyFile

	server, err := config.Complete().NewWithDelegate(genericapiserver.NewEmptyDelegate())
	if err != nil {
		return err
	}

	prepared, err := server.PrepareRun()
	if err != nil {
		return err
	}
	return prepared.Run(ctx)
}
