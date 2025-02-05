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

package rest

import (
	certificatesapiv1 "k8s.io/api/certificates/v1"
	certificatesapiv1alpha1 "k8s.io/api/certificates/v1alpha1"
	certificatesapiv1beta1 "k8s.io/api/certificates/v1beta1"
	"k8s.io/apiserver/pkg/registry/generic"
	"k8s.io/apiserver/pkg/registry/rest"
	genericapiserver "k8s.io/apiserver/pkg/server"
	serverstorage "k8s.io/apiserver/pkg/server/storage"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/api/legacyscheme"
	"k8s.io/kubernetes/pkg/apis/certificates"
	"k8s.io/kubernetes/pkg/features"
	certificatestore "k8s.io/kubernetes/pkg/registry/certificates/certificates/storage"
	clustertrustbundlestore "k8s.io/kubernetes/pkg/registry/certificates/clustertrustbundle/storage"
)

type RESTStorageProvider struct{}

func (p RESTStorageProvider) NewRESTStorage(apiResourceConfigSource serverstorage.APIResourceConfigSource, restOptionsGetter generic.RESTOptionsGetter) (genericapiserver.APIGroupInfo, error) {
	apiGroupInfo := genericapiserver.NewDefaultAPIGroupInfo(certificates.GroupName, legacyscheme.Scheme, legacyscheme.ParameterCodec, legacyscheme.Codecs)
	// If you add a version here, be sure to add an entry in `k8s.io/kubernetes/cmd/kube-apiserver/app/aggregator.go with specific priorities.
	// TODO refactor the plumbing to provide the information in the APIGroupInfo

	if storageMap, err := p.v1Storage(apiResourceConfigSource, restOptionsGetter); err != nil {
		return genericapiserver.APIGroupInfo{}, err
	} else if len(storageMap) > 0 {
		apiGroupInfo.VersionedResourcesStorageMap[certificatesapiv1.SchemeGroupVersion.Version] = storageMap
	}

	if storageMap, err := p.v1beta1Storage(apiResourceConfigSource, restOptionsGetter); err != nil {
		return genericapiserver.APIGroupInfo{}, err
	} else if len(storageMap) > 0 {
		apiGroupInfo.VersionedResourcesStorageMap[certificatesapiv1beta1.SchemeGroupVersion.Version] = storageMap
	}

	if storageMap, err := p.v1alpha1Storage(apiResourceConfigSource, restOptionsGetter); err != nil {
		return genericapiserver.APIGroupInfo{}, err
	} else if len(storageMap) > 0 {
		apiGroupInfo.VersionedResourcesStorageMap[certificatesapiv1alpha1.SchemeGroupVersion.Version] = storageMap
	}

	return apiGroupInfo, nil
}

func (p RESTStorageProvider) v1Storage(apiResourceConfigSource serverstorage.APIResourceConfigSource, restOptionsGetter generic.RESTOptionsGetter) (map[string]rest.Storage, error) {
	storage := map[string]rest.Storage{}

	if resource := "certificatesigningrequests"; apiResourceConfigSource.ResourceEnabled(certificatesapiv1.SchemeGroupVersion.WithResource(resource)) {
		csrStorage, csrStatusStorage, csrApprovalStorage, err := certificatestore.NewREST(restOptionsGetter)
		if err != nil {
			return nil, err
		}
		storage[resource] = csrStorage
		storage[resource+"/status"] = csrStatusStorage
		storage[resource+"/approval"] = csrApprovalStorage
	}

	return storage, nil
}

func (p RESTStorageProvider) v1beta1Storage(apiResourceConfigSource serverstorage.APIResourceConfigSource, restOptionsGetter generic.RESTOptionsGetter) (map[string]rest.Storage, error) {
	storage := map[string]rest.Storage{}

	if resource := "clustertrustbundles"; apiResourceConfigSource.ResourceEnabled(certificatesapiv1beta1.SchemeGroupVersion.WithResource(resource)) {
		if utilfeature.DefaultFeatureGate.Enabled(features.ClusterTrustBundle) {
			bundleStorage, err := clustertrustbundlestore.NewREST(restOptionsGetter)
			if err != nil {
				return nil, err
			}
			storage[resource] = bundleStorage
		} else {
			klog.Warning("ClusterTrustBundle storage is disabled because the ClusterTrustBundle feature gate is disabled")
		}
	}

	return storage, nil
}

func (p RESTStorageProvider) v1alpha1Storage(apiResourceConfigSource serverstorage.APIResourceConfigSource, restOptionsGetter generic.RESTOptionsGetter) (map[string]rest.Storage, error) {
	storage := map[string]rest.Storage{}

	if resource := "clustertrustbundles"; apiResourceConfigSource.ResourceEnabled(certificatesapiv1alpha1.SchemeGroupVersion.WithResource(resource)) {
		if utilfeature.DefaultFeatureGate.Enabled(features.ClusterTrustBundle) {
			bundleStorage, err := clustertrustbundlestore.NewREST(restOptionsGetter)
			if err != nil {
				return nil, err
			}
			storage[resource] = bundleStorage
		} else {
			klog.Warning("ClusterTrustBundle storage is disabled because the ClusterTrustBundle feature gate is disabled")
		}
	}

	return storage, nil
}

func (p RESTStorageProvider) GroupName() string {
	return certificates.GroupName
}
