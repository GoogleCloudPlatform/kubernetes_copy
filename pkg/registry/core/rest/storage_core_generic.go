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

package rest

import (
	"context"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apiserver/pkg/authentication/authenticator"
	"k8s.io/apiserver/pkg/registry/generic"
	"k8s.io/apiserver/pkg/registry/rest"
	genericapiserver "k8s.io/apiserver/pkg/server"
	serverstorage "k8s.io/apiserver/pkg/server/storage"
	"k8s.io/client-go/informers"
	restclient "k8s.io/client-go/rest"

	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/runtime/serializer/cbor"
	"k8s.io/apiserver/pkg/features"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/kubernetes/pkg/api/legacyscheme"
	api "k8s.io/kubernetes/pkg/apis/core"
	configmapstore "k8s.io/kubernetes/pkg/registry/core/configmap/storage"
	eventstore "k8s.io/kubernetes/pkg/registry/core/event/storage"
	namespacestore "k8s.io/kubernetes/pkg/registry/core/namespace/storage"
	resourcequotastore "k8s.io/kubernetes/pkg/registry/core/resourcequota/storage"
	secretstore "k8s.io/kubernetes/pkg/registry/core/secret/storage"
	serviceaccountstore "k8s.io/kubernetes/pkg/registry/core/serviceaccount/storage"
	"k8s.io/kubernetes/pkg/serviceaccount"
)

// GenericConfig provides information needed to build RESTStorage
// for generic resources in core. It implements the "normal" RESTStorageProvider interface.
type GenericConfig struct {
	StorageFactory serverstorage.StorageFactory
	EventTTL       time.Duration

	ServiceAccountIssuer        serviceaccount.TokenGenerator
	ServiceAccountMaxExpiration time.Duration
	ExtendExpiration            bool
	MaxExtendedExpiration       time.Duration

	APIAudiences authenticator.Audiences

	LoopbackClientConfig *restclient.Config
	Informers            informers.SharedInformerFactory
}

func (c *GenericConfig) NewRESTStorage(apiResourceConfigSource serverstorage.APIResourceConfigSource, restOptionsGetter generic.RESTOptionsGetter) (genericapiserver.APIGroupInfo, error) {
	apiGroupInfo := genericapiserver.APIGroupInfo{
		PrioritizedVersions:          legacyscheme.Scheme.PrioritizedVersionsForGroup(""),
		VersionedResourcesStorageMap: map[string]map[string]rest.Storage{},
		Scheme:                       legacyscheme.Scheme,
		ParameterCodec:               legacyscheme.ParameterCodec,
		NegotiatedSerializer:         legacyscheme.Codecs,
	}
	opts := []serializer.CodecFactoryOptionsMutator{}
	if utilfeature.DefaultFeatureGate.Enabled(features.CBORServingAndStorage) {
		opts = append(opts, serializer.WithSerializer(cbor.NewSerializerInfo))
	}
	if utilfeature.DefaultFeatureGate.Enabled(features.StreamingCollectionEncodingToJSON) {
		opts = append(opts, serializer.WithStreamingCollectionEncodingToJSON())
	}
	if len(opts) != 0 {
		apiGroupInfo.NegotiatedSerializer = serializer.NewCodecFactory(legacyscheme.Scheme, opts...)
	}

	eventStorage, err := eventstore.NewREST(restOptionsGetter, uint64(c.EventTTL.Seconds()))
	if err != nil {
		return genericapiserver.APIGroupInfo{}, err
	}

	resourceQuotaStorage, resourceQuotaStatusStorage, err := resourcequotastore.NewREST(restOptionsGetter)
	if err != nil {
		return genericapiserver.APIGroupInfo{}, err
	}
	secretStorage, err := secretstore.NewREST(restOptionsGetter)
	if err != nil {
		return genericapiserver.APIGroupInfo{}, err
	}

	configMapStorage, err := configmapstore.NewREST(restOptionsGetter)
	if err != nil {
		return genericapiserver.APIGroupInfo{}, err
	}

	namespaceStorage, namespaceStatusStorage, namespaceFinalizeStorage, err := namespacestore.NewREST(restOptionsGetter)
	if err != nil {
		return genericapiserver.APIGroupInfo{}, err
	}

	var serviceAccountStorage *serviceaccountstore.REST
	if c.ServiceAccountIssuer != nil {
		serviceAccountStorage, err = serviceaccountstore.NewREST(restOptionsGetter, c.ServiceAccountIssuer, c.APIAudiences, c.ServiceAccountMaxExpiration, newNotFoundGetter(schema.GroupResource{Resource: "pods"}), secretStorage.Store, newNotFoundGetter(schema.GroupResource{Resource: "nodes"}), c.ExtendExpiration, c.MaxExtendedExpiration)
	} else {
		serviceAccountStorage, err = serviceaccountstore.NewREST(restOptionsGetter, nil, nil, 0, newNotFoundGetter(schema.GroupResource{Resource: "pods"}), newNotFoundGetter(schema.GroupResource{Resource: "secrets"}), newNotFoundGetter(schema.GroupResource{Resource: "nodes"}), false, c.MaxExtendedExpiration)
	}
	if err != nil {
		return genericapiserver.APIGroupInfo{}, err
	}

	storage := map[string]rest.Storage{}
	if resource := "events"; apiResourceConfigSource.ResourceEnabled(corev1.SchemeGroupVersion.WithResource(resource)) {
		storage[resource] = eventStorage
	}

	if resource := "resourcequotas"; apiResourceConfigSource.ResourceEnabled(corev1.SchemeGroupVersion.WithResource(resource)) {
		storage[resource] = resourceQuotaStorage
		storage[resource+"/status"] = resourceQuotaStatusStorage
	}

	if resource := "namespaces"; apiResourceConfigSource.ResourceEnabled(corev1.SchemeGroupVersion.WithResource(resource)) {
		storage[resource] = namespaceStorage
		storage[resource+"/status"] = namespaceStatusStorage
		storage[resource+"/finalize"] = namespaceFinalizeStorage
	}

	if resource := "secrets"; apiResourceConfigSource.ResourceEnabled(corev1.SchemeGroupVersion.WithResource(resource)) {
		storage[resource] = secretStorage
	}

	if resource := "serviceaccounts"; apiResourceConfigSource.ResourceEnabled(corev1.SchemeGroupVersion.WithResource(resource)) {
		storage[resource] = serviceAccountStorage
		if serviceAccountStorage.Token != nil {
			storage[resource+"/token"] = serviceAccountStorage.Token
		}
	}

	if resource := "configmaps"; apiResourceConfigSource.ResourceEnabled(corev1.SchemeGroupVersion.WithResource(resource)) {
		storage[resource] = configMapStorage
	}

	if len(storage) > 0 {
		apiGroupInfo.VersionedResourcesStorageMap["v1"] = storage
	}

	return apiGroupInfo, nil
}

func (c *GenericConfig) GroupName() string {
	return api.GroupName
}

func newNotFoundGetter(gr schema.GroupResource) rest.Getter {
	return notFoundGetter{gr: gr}
}

type notFoundGetter struct {
	gr schema.GroupResource
}

func (g notFoundGetter) Get(ctx context.Context, name string, options *metav1.GetOptions) (runtime.Object, error) {
	return nil, errors.NewNotFound(g.gr, name)
}
