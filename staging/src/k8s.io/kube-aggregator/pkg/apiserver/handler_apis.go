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

package apiserver

import (
	"net/http"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apiserver/pkg/endpoints/discovery"
	"k8s.io/apiserver/pkg/endpoints/handlers/responsewriters"
	"k8s.io/klog/v2"

	apiregistrationv1api "k8s.io/kube-aggregator/pkg/apis/apiregistration/v1"
	apiregistrationv1apihelper "k8s.io/kube-aggregator/pkg/apis/apiregistration/v1/helper"
	apiregistrationv1beta1api "k8s.io/kube-aggregator/pkg/apis/apiregistration/v1beta1"
	listers "k8s.io/kube-aggregator/pkg/client/listers/apiregistration/v1"
)

// apisHandler serves the `/apis` endpoint.
// This is registered as a filter so that it never collides with any explicitly registered endpoints
type apisHandler struct {
	codecs         serializer.CodecFactory
	lister         listers.APIServiceLister
	discoveryGroup metav1.APIGroup
}

// discoveryGroup generates the APIGroup for the apiregistration group.
//
// enabledVersions is used to determine if the v1beta1 version of the apiregistration
// group is also to be included in the resulting APIGroup.
//
// handler is a GenericAPIServerHandler, used to resolve the hash of the API GroupVersion
func discoveryGroup(enabledVersions sets.String, handler http.Handler) metav1.APIGroup {
	hash, err := discovery.GetGroupVersionHash("/apis/"+apiregistrationv1api.SchemeGroupVersion.String(), handler)
	if err != nil {
		klog.Errorf("unable to retrieve hash for groupversion: %v", err)
	}
	retval := metav1.APIGroup{
		Name: apiregistrationv1api.GroupName,
		Versions: []metav1.GroupVersionForDiscovery{
			{
				GroupVersion: apiregistrationv1api.SchemeGroupVersion.String(),
				Version:      apiregistrationv1api.SchemeGroupVersion.Version,
				Hash:         hash,
			},
		},
		PreferredVersion: metav1.GroupVersionForDiscovery{
			GroupVersion: apiregistrationv1api.SchemeGroupVersion.String(),
			Version:      apiregistrationv1api.SchemeGroupVersion.Version,
			Hash:         hash,
		},
	}

	if enabledVersions.Has(apiregistrationv1beta1api.SchemeGroupVersion.Version) {
		retval.Versions = append(retval.Versions, metav1.GroupVersionForDiscovery{
			GroupVersion: apiregistrationv1beta1api.SchemeGroupVersion.String(),
			Version:      apiregistrationv1beta1api.SchemeGroupVersion.Version,
			Hash:         hash,
		})
	}

	return retval
}

func (r *apisHandler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	discoveryGroupList := &metav1.APIGroupList{
		// always add OUR api group to the list first.  Since we'll never have a registered APIService for it
		// and since this is the crux of the API, having this first will give our names priority.  It's good to be king.
		Groups: []metav1.APIGroup{r.discoveryGroup},
	}

	apiServices, err := r.lister.List(labels.Everything())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	apiServicesByGroup := apiregistrationv1apihelper.SortedByGroupAndVersion(apiServices)
	for _, apiGroupServers := range apiServicesByGroup {
		// skip the legacy group
		if len(apiGroupServers[0].Spec.Group) == 0 {
			continue
		}
		discoveryGroup := convertToDiscoveryAPIGroup(apiGroupServers)
		if discoveryGroup != nil {
			discoveryGroupList.Groups = append(discoveryGroupList.Groups, *discoveryGroup)
		}
	}

	hash, err := discovery.CalculateETag(discoveryGroupList)
	if err != nil {
		// Non-fatal
		klog.Error(
			"failed to calculate e-tag for group list of apisHandler.",
			"E-Tags will not be supported")

		// Empty hash to disable E-Tag
		hash = ""
	}
	discovery.ServeHTTPWithETag(discoveryGroupList, hash, r.codecs, w, req)
}

// convertToDiscoveryAPIGroup takes apiservices in a single group and returns a discovery compatible object.
// if none of the services are available, it will return nil.
func convertToDiscoveryAPIGroup(apiServices []*apiregistrationv1api.APIService) *metav1.APIGroup {
	apiServicesByGroup := apiregistrationv1apihelper.SortedByGroupAndVersion(apiServices)[0]

	var discoveryGroup *metav1.APIGroup

	for _, apiService := range apiServicesByGroup {
		// the first APIService which is valid becomes the default
		if discoveryGroup == nil {
			discoveryGroup = &metav1.APIGroup{
				Name: apiService.Spec.Group,
				PreferredVersion: metav1.GroupVersionForDiscovery{
					GroupVersion: apiService.Spec.Group + "/" + apiService.Spec.Version,
					Version:      apiService.Spec.Version,
					Hash:         apiService.Status.Hash,
				},
			}
		}

		discoveryGroup.Versions = append(discoveryGroup.Versions,
			metav1.GroupVersionForDiscovery{
				GroupVersion: apiService.Spec.Group + "/" + apiService.Spec.Version,
				Version:      apiService.Spec.Version,
				Hash:         apiService.Status.Hash,
			},
		)
	}

	return discoveryGroup
}

// apiGroupHandler serves the `/apis/<group>` endpoint.
type apiGroupHandler struct {
	codecs    serializer.CodecFactory
	groupName string

	lister listers.APIServiceLister

	delegate http.Handler
}

func (r *apiGroupHandler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	apiServices, err := r.lister.List(labels.Everything())
	if statusErr, ok := err.(*apierrors.StatusError); ok {
		responsewriters.WriteRawJSON(int(statusErr.Status().Code), statusErr.Status(), w)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	apiServicesForGroup := []*apiregistrationv1api.APIService{}
	for _, apiService := range apiServices {
		if apiService.Spec.Group == r.groupName {
			apiServicesForGroup = append(apiServicesForGroup, apiService)
		}
	}

	if len(apiServicesForGroup) == 0 {
		r.delegate.ServeHTTP(w, req)
		return
	}

	discoveryGroup := convertToDiscoveryAPIGroup(apiServicesForGroup)
	if discoveryGroup == nil {
		http.Error(w, "", http.StatusNotFound)
		return
	}

	hash, err := discovery.CalculateETag(discoveryGroup)
	if err != nil {
		// Non-fatal
		klog.Error(
			"failed to calculate e-tag for group list of apisHandler.",
			"E-Tags will not be supported")

		// Empty hash to disable E-Tag
		hash = ""
	}

	discovery.ServeHTTPWithETag(discoveryGroup, hash, r.codecs, w, req)
}
