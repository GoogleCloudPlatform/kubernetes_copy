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

package options

import (
	"fmt"
	"io"
	"net"
	"net/url"

	"github.com/spf13/pflag"
	noopoteltrace "go.opentelemetry.io/otel/trace/noop"

	v1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1beta1"
	"k8s.io/apiextensions-apiserver/pkg/apiserver"
	generatedopenapi "k8s.io/apiextensions-apiserver/pkg/generated/openapi"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured/unstructuredscheme"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer/cbor"
	"k8s.io/apimachinery/pkg/runtime/serializer/recognizer"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	openapinamer "k8s.io/apiserver/pkg/endpoints/openapi"
	"k8s.io/apiserver/pkg/features"
	genericregistry "k8s.io/apiserver/pkg/registry/generic"
	genericapiserver "k8s.io/apiserver/pkg/server"
	server "k8s.io/apiserver/pkg/server"
	genericoptions "k8s.io/apiserver/pkg/server/options"
	storagevalue "k8s.io/apiserver/pkg/storage/value"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	flowcontrolrequest "k8s.io/apiserver/pkg/util/flowcontrol/request"
	"k8s.io/apiserver/pkg/util/openapi"
	"k8s.io/apiserver/pkg/util/proxy"
	"k8s.io/apiserver/pkg/util/webhook"
	scheme "k8s.io/client-go/kubernetes/scheme"
	corev1 "k8s.io/client-go/listers/core/v1"
	netutils "k8s.io/utils/net"
)

const defaultEtcdPathPrefix = "/registry/apiextensions.kubernetes.io"

// CustomResourceDefinitionsServerOptions describes the runtime options of an apiextensions-apiserver.
type CustomResourceDefinitionsServerOptions struct {
	ServerRunOptions   *genericoptions.ServerRunOptions
	RecommendedOptions *genericoptions.RecommendedOptions
	APIEnablement      *genericoptions.APIEnablementOptions

	StdOut io.Writer
	StdErr io.Writer
}

// NewCustomResourceDefinitionsServerOptions creates default options of an apiextensions-apiserver.
func NewCustomResourceDefinitionsServerOptions(out, errOut io.Writer) *CustomResourceDefinitionsServerOptions {
	o := &CustomResourceDefinitionsServerOptions{
		ServerRunOptions: genericoptions.NewServerRunOptions(),
		RecommendedOptions: genericoptions.NewRecommendedOptions(
			defaultEtcdPathPrefix,
			apiserver.Codecs.LegacyCodec(v1beta1.SchemeGroupVersion, v1.SchemeGroupVersion),
		),
		APIEnablement: genericoptions.NewAPIEnablementOptions(),

		StdOut: out,
		StdErr: errOut,
	}

	return o
}

// AddFlags adds the apiextensions-apiserver flags to the flagset.
func (o CustomResourceDefinitionsServerOptions) AddFlags(fs *pflag.FlagSet) {
	o.ServerRunOptions.AddUniversalFlags(fs)
	o.RecommendedOptions.AddFlags(fs)
	o.APIEnablement.AddFlags(fs)
}

// Validate validates the apiextensions-apiserver options.
func (o CustomResourceDefinitionsServerOptions) Validate() error {
	errors := []error{}
	errors = append(errors, o.ServerRunOptions.Validate()...)
	errors = append(errors, o.RecommendedOptions.Validate()...)
	errors = append(errors, o.APIEnablement.Validate(apiserver.Scheme)...)
	return utilerrors.NewAggregate(errors)
}

// Complete fills in missing options.
func (o *CustomResourceDefinitionsServerOptions) Complete() error {
	return o.ServerRunOptions.Complete()
}

// Config returns an apiextensions-apiserver configuration.
func (o CustomResourceDefinitionsServerOptions) Config() (*apiserver.Config, error) {
	// TODO have a "real" external address
	if err := o.RecommendedOptions.SecureServing.MaybeDefaultWithSelfSignedCerts("localhost", nil, []net.IP{netutils.ParseIPSloppy("127.0.0.1")}); err != nil {
		return nil, fmt.Errorf("error creating self-signed certificates: %v", err)
	}

	serverConfig := genericapiserver.NewRecommendedConfig(apiserver.Codecs, server.KubeAPIExtensionsAPIServer)
	if err := o.ServerRunOptions.ApplyTo(&serverConfig.Config); err != nil {
		return nil, err
	}
	if err := o.RecommendedOptions.ApplyTo(serverConfig); err != nil {
		return nil, err
	}
	if err := o.APIEnablement.ApplyTo(&serverConfig.Config, apiserver.DefaultAPIResourceConfigSource(), apiserver.Scheme); err != nil {
		return nil, err
	}

	serverConfig.OpenAPIV3Config = genericapiserver.DefaultOpenAPIV3Config(openapi.GetOpenAPIDefinitionsWithoutDisabledFeatures(generatedopenapi.GetOpenAPIDefinitions), openapinamer.NewDefinitionNamer(apiserver.Scheme, scheme.Scheme))
	config := &apiserver.Config{
		GenericConfig: serverConfig,
		ExtraConfig: apiserver.ExtraConfig{
			CRDRESTOptionsGetter: NewCRDRESTOptionsGetter(*o.RecommendedOptions.Etcd, serverConfig.ResourceTransformers, serverConfig.StorageObjectCountTracker),
			ServiceResolver:      &serviceResolver{serverConfig.SharedInformerFactory.Core().V1().Services().Lister()},
			AuthResolverWrapper:  webhook.NewDefaultAuthenticationInfoResolverWrapper(nil, nil, serverConfig.LoopbackClientConfig, noopoteltrace.NewTracerProvider()),
		},
	}
	return config, nil
}

// NewCRDRESTOptionsGetter create a RESTOptionsGetter for CustomResources.
//
// Avoid messing with anything outside of changes to StorageConfig as that
// may lead to unexpected behavior when the options are applied.
func NewCRDRESTOptionsGetter(etcdOptions genericoptions.EtcdOptions, resourceTransformers storagevalue.ResourceTransformers, tracker flowcontrolrequest.StorageObjectCountTracker) genericregistry.RESTOptionsGetter {
	ucbor := cbor.NewSerializer(unstructuredscheme.NewUnstructuredCreator(), unstructuredscheme.NewUnstructuredObjectTyper())

	encoder := unstructured.UnstructuredJSONScheme
	if utilfeature.DefaultFeatureGate.Enabled(features.CBORServingAndStorage) {
		encoder = ucbor
	}

	etcdOptionsCopy := etcdOptions
	etcdOptionsCopy.StorageConfig.Codec = runtime.NewCodec(
		encoder,
		// Whether the feature gate is enabled or disabled, the decoder must be able to
		// recognize any resources stored using the CBOR encoder.
		recognizer.NewDecoder(
			ucbor,
			unstructured.UnstructuredJSONScheme,
		),
	)
	etcdOptionsCopy.StorageConfig.StorageObjectCountTracker = tracker
	etcdOptionsCopy.WatchCacheSizes = nil // this control is not provided for custom resources

	return etcdOptions.CreateRESTOptionsGetter(&genericoptions.SimpleStorageFactory{StorageConfig: etcdOptionsCopy.StorageConfig}, resourceTransformers)
}

type serviceResolver struct {
	services corev1.ServiceLister
}

func (r *serviceResolver) ResolveEndpoint(namespace, name string, port int32) (*url.URL, error) {
	return proxy.ResolveCluster(r.services, namespace, name, port)
}
