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

package discovery

import (
	"context"
	"encoding/json"
	goerrors "errors"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	openapi_v2 "github.com/google/gnostic-models/openapiv2"
	"google.golang.org/protobuf/proto"

	apidiscoveryv2 "k8s.io/api/apidiscovery/v2"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/version"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/openapi"
	restclient "k8s.io/client-go/rest"
)

const (
	// defaultRetries is the number of times a resource discovery is repeated if an api group disappears on the fly (e.g. CustomResourceDefinitions).
	defaultRetries = 2
	// protobuf mime type
	openAPIV2mimePb = "application/com.github.proto-openapi.spec.v2@v1.0+protobuf"

	// defaultTimeout is the maximum amount of time per request when no timeout has been set on a RESTClient.
	// Defaults to 32s in order to have a distinguishable length of time, relative to other timeouts that exist.
	defaultTimeout = 32 * time.Second

	// defaultBurst is the default burst to be used with the discovery client's token bucket rate limiter
	defaultBurst = 300

	AcceptV1 = runtime.ContentTypeJSON
	// Aggregated discovery content-type (v2beta1). NOTE: content-type parameters
	// MUST be ordered (g, v, as) for server in "Accept" header (BUT we are resilient
	// to ordering when comparing returned values in "Content-Type" header).
	AcceptV2 = runtime.ContentTypeJSON + ";" + "g=apidiscovery.k8s.io;v=v2;as=APIGroupDiscoveryList"
	// Prioritize aggregated discovery by placing first in the order of discovery accept types.
	acceptDiscoveryFormats = AcceptV2 + "," + AcceptV1
)

// Aggregated discovery content-type GVK.
var v2GVK = schema.GroupVersionKind{Group: "apidiscovery.k8s.io", Version: "v2", Kind: "APIGroupDiscoveryList"}

// DiscoveryInterface holds the methods that discover server-supported API groups,
// versions and resources.
type DiscoveryInterface interface {
	RESTClient() restclient.Interface
	ServerGroupsInterface
	ServerResourcesInterface
	ServerVersionInterface
	OpenAPISchemaInterface
	OpenAPIV3SchemaInterface
	// Returns copy of current discovery client that will only
	// receive the legacy discovery format, or pointer to current
	// discovery client if it does not support legacy-only discovery.
	WithLegacy() DiscoveryInterface
}

// AggregatedDiscoveryInterface extends DiscoveryInterface to include a method to possibly
// return discovery resources along with the discovery groups, which is what the newer
// aggregated discovery format does (APIGroupDiscoveryList).
type AggregatedDiscoveryInterface interface {
	DiscoveryInterface

	GroupsAndMaybeResources() (*metav1.APIGroupList, map[schema.GroupVersion]*metav1.APIResourceList, map[schema.GroupVersion]error, error)
}

// CachedDiscoveryInterface is a DiscoveryInterface with cache invalidation and freshness.
// Note that If the ServerResourcesForGroupVersion method returns a cache miss
// error, the user needs to explicitly call Invalidate to clear the cache,
// otherwise the same cache miss error will be returned next time.
type CachedDiscoveryInterface interface {
	DiscoveryInterface
	// Fresh is supposed to tell the caller whether or not to retry if the cache
	// fails to find something (false = retry, true = no need to retry).
	//
	// TODO: this needs to be revisited, this interface can't be locked properly
	// and doesn't make a lot of sense.
	Fresh() bool
	// Invalidate enforces that no cached data that is older than the current time
	// is used.
	Invalidate()
}

// ServerGroupsInterface has methods for obtaining supported groups on the API server
type ServerGroupsInterface interface {
	// ServerGroups returns the supported groups, with information like supported versions and the
	// preferred version.
	ServerGroups() (*metav1.APIGroupList, error)
}

// ServerResourcesInterface has methods for obtaining supported resources on the API server
type ServerResourcesInterface interface {
	// ServerResourcesForGroupVersion returns the supported resources for a group and version.
	ServerResourcesForGroupVersion(groupVersion string) (*metav1.APIResourceList, error)
	// ServerGroupsAndResources returns the supported groups and resources for all groups and versions.
	//
	// The returned group and resource lists might be non-nil with partial results even in the
	// case of non-nil error.
	ServerGroupsAndResources() ([]*metav1.APIGroup, []*metav1.APIResourceList, error)
	// ServerPreferredResources returns the supported resources with the version preferred by the
	// server.
	//
	// The returned group and resource lists might be non-nil with partial results even in the
	// case of non-nil error.
	ServerPreferredResources() ([]*metav1.APIResourceList, error)
	// ServerPreferredNamespacedResources returns the supported namespaced resources with the
	// version preferred by the server.
	//
	// The returned resource list might be non-nil with partial results even in the case of
	// non-nil error.
	ServerPreferredNamespacedResources() ([]*metav1.APIResourceList, error)
}

// ServerVersionInterface has a method for retrieving the server's version.
type ServerVersionInterface interface {
	// ServerVersion retrieves and parses the server's version (git version).
	ServerVersion() (*version.Info, error)
}

// OpenAPISchemaInterface has a method to retrieve the open API schema.
type OpenAPISchemaInterface interface {
	// OpenAPISchema retrieves and parses the swagger API schema the server supports.
	OpenAPISchema() (*openapi_v2.Document, error)
}

type OpenAPIV3SchemaInterface interface {
	OpenAPIV3() openapi.Client
}

// DiscoveryClient implements the functions that discover server-supported API groups,
// versions and resources.
type DiscoveryClient struct {
	restClient restclient.Interface

	LegacyPrefix string
	// Forces the client to request only "unaggregated" (legacy) discovery.
	UseLegacyDiscovery bool
}

var _ AggregatedDiscoveryInterface = &DiscoveryClient{}

// Convert metav1.APIVersions to metav1.APIGroup. APIVersions is used by legacy v1, so
// group would be "".
func apiVersionsToAPIGroup(apiVersions *metav1.APIVersions) (apiGroup metav1.APIGroup) {
	groupVersions := []metav1.GroupVersionForDiscovery{}
	for _, version := range apiVersions.Versions {
		groupVersion := metav1.GroupVersionForDiscovery{
			GroupVersion: version,
			Version:      version,
		}
		groupVersions = append(groupVersions, groupVersion)
	}
	apiGroup.Versions = groupVersions
	// There should be only one groupVersion returned at /api
	apiGroup.PreferredVersion = groupVersions[0]
	return
}

// GroupsAndMaybeResources returns the discovery groups, and (if new aggregated
// discovery format) the resources keyed by group/version. Merges discovery groups
// and resources from /api and /apis (either aggregated or not). Legacy groups
// must be ordered first. The server will either return both endpoints (/api, /apis)
// as aggregated discovery format or legacy format. For safety, resources will only
// be returned if both endpoints returned resources. Returned "failedGVs" can be
// empty, but will only be nil in the case an error is returned.
func (d *DiscoveryClient) GroupsAndMaybeResources() (
	*metav1.APIGroupList,
	map[schema.GroupVersion]*metav1.APIResourceList,
	map[schema.GroupVersion]error,
	error) {
	// Legacy group ordered first (there is only one -- core/v1 group). Returned groups must
	// be non-nil, but it could be empty. Returned resources, apiResources map could be nil.
	groups, resources, failedGVs, err := d.downloadLegacy()
	if err != nil {
		return nil, nil, nil, err
	}
	// Discovery groups and (possibly) resources downloaded from /apis.
	apiGroups, apiResources, failedApisGVs, aerr := d.downloadAPIs()
	if aerr != nil {
		return nil, nil, nil, aerr
	}
	// Merge apis groups into the legacy groups.
	for _, group := range apiGroups.Groups {
		groups.Groups = append(groups.Groups, group)
	}
	// For safety, only return resources if both endpoints returned resources.
	if resources != nil && apiResources != nil {
		for gv, resourceList := range apiResources {
			resources[gv] = resourceList
		}
	} else if resources != nil {
		resources = nil
	}
	// Merge failed GroupVersions from /api and /apis
	for gv, err := range failedApisGVs {
		failedGVs[gv] = err
	}
	return groups, resources, failedGVs, err
}

// downloadLegacy returns the discovery groups and possibly resources
// for the legacy v1 GVR at /api, or an error if one occurred. It is
// possible for the resource map to be nil if the server returned
// the unaggregated discovery. Returned "failedGVs" can be empty, but
// will only be nil in the case of a returned error.
func (d *DiscoveryClient) downloadLegacy() (
	*metav1.APIGroupList,
	map[schema.GroupVersion]*metav1.APIResourceList,
	map[schema.GroupVersion]error,
	error) {
	accept := acceptDiscoveryFormats
	if d.UseLegacyDiscovery {
		accept = AcceptV1
	}
	var responseContentType string
	body, err := d.restClient.Get().
		AbsPath("/api").
		SetHeader("Accept", accept).
		Do(context.TODO()).
		ContentType(&responseContentType).
		Raw()
	apiGroupList := &metav1.APIGroupList{}
	failedGVs := map[schema.GroupVersion]error{}
	if err != nil {
		// Tolerate 404, since aggregated api servers can return it.
		if errors.IsNotFound(err) {
			// Return empty structures and no error.
			emptyGVMap := map[schema.GroupVersion]*metav1.APIResourceList{}
			return apiGroupList, emptyGVMap, failedGVs, nil
		} else {
			return nil, nil, nil, err
		}
	}

	var resourcesByGV map[schema.GroupVersion]*metav1.APIResourceList
	// Based on the content-type server responded with: aggregated or unaggregated.
	if isGVK, _ := ContentTypeIsGVK(responseContentType, v2GVK); isGVK {
		var aggregatedDiscovery apidiscoveryv2.APIGroupDiscoveryList
		err = json.Unmarshal(body, &aggregatedDiscovery)
		if err != nil {
			return nil, nil, nil, err
		}
		apiGroupList, resourcesByGV, failedGVs = SplitGroupsAndResources(aggregatedDiscovery)
	} else {
		// Default is unaggregated discovery v1.
		var v metav1.APIVersions
		err = json.Unmarshal(body, &v)
		if err != nil {
			return nil, nil, nil, err
		}
		apiGroup := metav1.APIGroup{}
		if len(v.Versions) != 0 {
			apiGroup = apiVersionsToAPIGroup(&v)
		}
		apiGroupList.Groups = []metav1.APIGroup{apiGroup}
	}

	return apiGroupList, resourcesByGV, failedGVs, nil
}

// downloadAPIs returns the discovery groups and (if aggregated format) the
// discovery resources. The returned groups will always exist, but the
// resources map may be nil. Returned "failedGVs" can be empty, but will
// only be nil in the case of a returned error.
func (d *DiscoveryClient) downloadAPIs() (
	*metav1.APIGroupList,
	map[schema.GroupVersion]*metav1.APIResourceList,
	map[schema.GroupVersion]error,
	error) {
	accept := acceptDiscoveryFormats
	if d.UseLegacyDiscovery {
		accept = AcceptV1
	}
	var responseContentType string
	body, err := d.restClient.Get().
		AbsPath("/apis").
		SetHeader("Accept", accept).
		Do(context.TODO()).
		ContentType(&responseContentType).
		Raw()
	if err != nil {
		return nil, nil, nil, err
	}

	apiGroupList := &metav1.APIGroupList{}
	failedGVs := map[schema.GroupVersion]error{}
	var resourcesByGV map[schema.GroupVersion]*metav1.APIResourceList
	// Based on the content-type server responded with: aggregated or unaggregated.
	if isGVK, _ := ContentTypeIsGVK(responseContentType, v2GVK); isGVK {
		var aggregatedDiscovery apidiscoveryv2.APIGroupDiscoveryList
		err = json.Unmarshal(body, &aggregatedDiscovery)
		if err != nil {
			return nil, nil, nil, err
		}
		apiGroupList, resourcesByGV, failedGVs = SplitGroupsAndResources(aggregatedDiscovery)
	} else {
		// Default is unaggregated discovery v1.
		err = json.Unmarshal(body, apiGroupList)
		if err != nil {
			return nil, nil, nil, err
		}
	}

	return apiGroupList, resourcesByGV, failedGVs, nil
}

// ContentTypeIsGVK checks of the content-type string is both
// "application/json" and matches the provided GVK. An error
// is returned if the content type string is malformed.
// NOTE: This function is resilient to the ordering of the
// content-type parameters, as well as parameters added by
// intermediaries such as proxies or gateways. Examples:
//
//	("application/json; g=apidiscovery.k8s.io;v=v2beta1;as=APIGroupDiscoveryList", {apidiscovery.k8s.io, v2beta1, APIGroupDiscoveryList}) = (true, nil)
//	("application/json; as=APIGroupDiscoveryList;v=v2beta1;g=apidiscovery.k8s.io", {apidiscovery.k8s.io, v2beta1, APIGroupDiscoveryList}) = (true, nil)
//	("application/json; as=APIGroupDiscoveryList;v=v2beta1;g=apidiscovery.k8s.io;charset=utf-8", {apidiscovery.k8s.io, v2beta1, APIGroupDiscoveryList}) = (true, nil)
//	("application/json", any GVK) = (false, nil)
//	("application/json; charset=UTF-8", any GVK) = (false, nil)
//	("malformed content type string", any GVK) = (false, error)
func ContentTypeIsGVK(contentType string, gvk schema.GroupVersionKind) (bool, error) {
	base, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		return false, err
	}
	gvkMatch := runtime.ContentTypeJSON == base &&
		params["g"] == gvk.Group &&
		params["v"] == gvk.Version &&
		params["as"] == gvk.Kind
	return gvkMatch, nil
}

// ServerGroups returns the supported groups, with information like supported versions and the
// preferred version.
func (d *DiscoveryClient) ServerGroups() (*metav1.APIGroupList, error) {
	groups, _, _, err := d.GroupsAndMaybeResources()
	if err != nil {
		return nil, err
	}
	return groups, nil
}

// ServerResourcesForGroupVersion returns the supported resources for a group and version.
func (d *DiscoveryClient) ServerResourcesForGroupVersion(groupVersion string) (resources *metav1.APIResourceList, err error) {
	url := url.URL{}
	if len(groupVersion) == 0 {
		return nil, fmt.Errorf("groupVersion shouldn't be empty")
	}
	if len(d.LegacyPrefix) > 0 && groupVersion == "v1" {
		url.Path = d.LegacyPrefix + "/" + groupVersion
	} else {
		url.Path = "/apis/" + groupVersion
	}
	resources = &metav1.APIResourceList{
		GroupVersion: groupVersion,
	}
	err = d.restClient.Get().AbsPath(url.String()).Do(context.TODO()).Into(resources)
	if err != nil {
		// Tolerate core/v1 not found response by returning empty resource list;
		// this probably should not happen. But we should verify all callers are
		// not depending on this toleration before removal.
		if groupVersion == "v1" && errors.IsNotFound(err) {
			return resources, nil
		}
		return nil, err
	}
	return resources, nil
}

// ServerGroupsAndResources returns the supported resources for all groups and versions.
func (d *DiscoveryClient) ServerGroupsAndResources() ([]*metav1.APIGroup, []*metav1.APIResourceList, error) {
	return withRetries(defaultRetries, func() ([]*metav1.APIGroup, []*metav1.APIResourceList, error) {
		return ServerGroupsAndResources(d)
	})
}

// ErrGroupDiscoveryFailed is returned if one or more API groups fail to load.
type ErrGroupDiscoveryFailed struct {
	// Groups is a list of the groups that failed to load and the error cause
	Groups map[schema.GroupVersion]error
}

// Error implements the error interface
func (e *ErrGroupDiscoveryFailed) Error() string {
	var groups []string
	for k, v := range e.Groups {
		groups = append(groups, fmt.Sprintf("%s: %v", k, v))
	}
	sort.Strings(groups)
	return fmt.Sprintf("unable to retrieve the complete list of server APIs: %s", strings.Join(groups, ", "))
}

// Is makes it possible for the callers to use `errors.Is(` helper on errors wrapped with ErrGroupDiscoveryFailed error.
func (e *ErrGroupDiscoveryFailed) Is(target error) bool {
	_, ok := target.(*ErrGroupDiscoveryFailed)
	return ok
}

// IsGroupDiscoveryFailedError returns true if the provided error indicates the server was unable to discover
// a complete list of APIs for the client to use.
func IsGroupDiscoveryFailedError(err error) bool {
	_, ok := err.(*ErrGroupDiscoveryFailed)
	return err != nil && ok
}

// GroupDiscoveryFailedErrorGroups returns true if the error is an ErrGroupDiscoveryFailed error,
// along with the map of group versions that failed discovery.
func GroupDiscoveryFailedErrorGroups(err error) (map[schema.GroupVersion]error, bool) {
	var groupDiscoveryError *ErrGroupDiscoveryFailed
	if err != nil && goerrors.As(err, &groupDiscoveryError) {
		return groupDiscoveryError.Groups, true
	}
	return nil, false
}

func ServerGroupsAndResources(d DiscoveryInterface) ([]*metav1.APIGroup, []*metav1.APIResourceList, error) {
	var sgs *metav1.APIGroupList
	var resources []*metav1.APIResourceList
	var failedGVs map[schema.GroupVersion]error
	var err error

	// If the passed discovery object implements the wider AggregatedDiscoveryInterface,
	// then attempt to retrieve aggregated discovery with both groups and the resources.
	if ad, ok := d.(AggregatedDiscoveryInterface); ok {
		var resourcesByGV map[schema.GroupVersion]*metav1.APIResourceList
		sgs, resourcesByGV, failedGVs, err = ad.GroupsAndMaybeResources()
		for _, resourceList := range resourcesByGV {
			resources = append(resources, resourceList)
		}
	} else {
		sgs, err = d.ServerGroups()
	}

	if sgs == nil {
		return nil, nil, err
	}
	resultGroups := []*metav1.APIGroup{}
	for i := range sgs.Groups {
		resultGroups = append(resultGroups, &sgs.Groups[i])
	}
	// resources is non-nil if aggregated discovery succeeded.
	if resources != nil {
		// Any stale Group/Versions returned by aggregated discovery
		// must be surfaced to the caller as failed Group/Versions.
		var ferr error
		if len(failedGVs) > 0 {
			ferr = &ErrGroupDiscoveryFailed{Groups: failedGVs}
		}
		return resultGroups, resources, ferr
	}

	groupVersionResources, failedGroups := fetchGroupVersionResources(d, sgs)

	// order results by group/version discovery order
	result := []*metav1.APIResourceList{}
	for _, apiGroup := range sgs.Groups {
		for _, version := range apiGroup.Versions {
			gv := schema.GroupVersion{Group: apiGroup.Name, Version: version.Version}
			if resources, ok := groupVersionResources[gv]; ok {
				result = append(result, resources)
			}
		}
	}

	if len(failedGroups) == 0 {
		return resultGroups, result, nil
	}

	return resultGroups, result, &ErrGroupDiscoveryFailed{Groups: failedGroups}
}

// ServerPreferredResources uses the provided discovery interface to look up preferred resources
func ServerPreferredResources(d DiscoveryInterface) ([]*metav1.APIResourceList, error) {
	var serverGroupList *metav1.APIGroupList
	var failedGroups map[schema.GroupVersion]error
	var groupVersionResources map[schema.GroupVersion]*metav1.APIResourceList
	var err error

	// If the passed discovery object implements the wider AggregatedDiscoveryInterface,
	// then it is attempt to retrieve both the groups and the resources. "failedGroups"
	// are Group/Versions returned as stale in AggregatedDiscovery format.
	ad, ok := d.(AggregatedDiscoveryInterface)
	if ok {
		serverGroupList, groupVersionResources, failedGroups, err = ad.GroupsAndMaybeResources()
	} else {
		serverGroupList, err = d.ServerGroups()
	}
	if err != nil {
		return nil, err
	}
	// Non-aggregated discovery must fetch resources from Groups.
	if groupVersionResources == nil {
		groupVersionResources, failedGroups = fetchGroupVersionResources(d, serverGroupList)
	}

	result := []*metav1.APIResourceList{}
	grVersions := map[schema.GroupResource]string{}                         // selected version of a GroupResource
	grAPIResources := map[schema.GroupResource]*metav1.APIResource{}        // selected APIResource for a GroupResource
	gvAPIResourceLists := map[schema.GroupVersion]*metav1.APIResourceList{} // blueprint for a APIResourceList for later grouping

	for _, apiGroup := range serverGroupList.Groups {
		for _, version := range apiGroup.Versions {
			groupVersion := schema.GroupVersion{Group: apiGroup.Name, Version: version.Version}

			apiResourceList, ok := groupVersionResources[groupVersion]
			if !ok {
				continue
			}

			// create empty list which is filled later in another loop
			emptyAPIResourceList := metav1.APIResourceList{
				GroupVersion: version.GroupVersion,
			}
			gvAPIResourceLists[groupVersion] = &emptyAPIResourceList
			result = append(result, &emptyAPIResourceList)

			for i := range apiResourceList.APIResources {
				apiResource := &apiResourceList.APIResources[i]
				if strings.Contains(apiResource.Name, "/") {
					continue
				}
				gv := schema.GroupResource{Group: apiGroup.Name, Resource: apiResource.Name}
				if _, ok := grAPIResources[gv]; ok && version.Version != apiGroup.PreferredVersion.Version {
					// only override with preferred version
					continue
				}
				grVersions[gv] = version.Version
				grAPIResources[gv] = apiResource
			}
		}
	}

	// group selected APIResources according to GroupVersion into APIResourceLists
	for groupResource, apiResource := range grAPIResources {
		version := grVersions[groupResource]
		groupVersion := schema.GroupVersion{Group: groupResource.Group, Version: version}
		apiResourceList := gvAPIResourceLists[groupVersion]
		apiResourceList.APIResources = append(apiResourceList.APIResources, *apiResource)
	}

	if len(failedGroups) == 0 {
		return result, nil
	}

	return result, &ErrGroupDiscoveryFailed{Groups: failedGroups}
}

// fetchServerResourcesForGroupVersions uses the discovery client to fetch the resources for the specified groups in parallel.
func fetchGroupVersionResources(d DiscoveryInterface, apiGroups *metav1.APIGroupList) (map[schema.GroupVersion]*metav1.APIResourceList, map[schema.GroupVersion]error) {
	groupVersionResources := make(map[schema.GroupVersion]*metav1.APIResourceList)
	failedGroups := make(map[schema.GroupVersion]error)

	wg := &sync.WaitGroup{}
	resultLock := &sync.Mutex{}
	for _, apiGroup := range apiGroups.Groups {
		for _, version := range apiGroup.Versions {
			groupVersion := schema.GroupVersion{Group: apiGroup.Name, Version: version.Version}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer utilruntime.HandleCrash()

				apiResourceList, err := d.ServerResourcesForGroupVersion(groupVersion.String())

				// lock to record results
				resultLock.Lock()
				defer resultLock.Unlock()

				if err != nil {
					// TODO: maybe restrict this to NotFound errors
					failedGroups[groupVersion] = err
				}
				if apiResourceList != nil {
					// even in case of error, some fallback might have been returned
					groupVersionResources[groupVersion] = apiResourceList
				}
			}()
		}
	}
	wg.Wait()

	return groupVersionResources, failedGroups
}

// ServerPreferredResources returns the supported resources with the version preferred by the
// server.
func (d *DiscoveryClient) ServerPreferredResources() ([]*metav1.APIResourceList, error) {
	_, rs, err := withRetries(defaultRetries, func() ([]*metav1.APIGroup, []*metav1.APIResourceList, error) {
		rs, err := ServerPreferredResources(d)
		return nil, rs, err
	})
	return rs, err
}

// ServerPreferredNamespacedResources returns the supported namespaced resources with the
// version preferred by the server.
func (d *DiscoveryClient) ServerPreferredNamespacedResources() ([]*metav1.APIResourceList, error) {
	return ServerPreferredNamespacedResources(d)
}

// ServerPreferredNamespacedResources uses the provided discovery interface to look up preferred namespaced resources
func ServerPreferredNamespacedResources(d DiscoveryInterface) ([]*metav1.APIResourceList, error) {
	all, err := ServerPreferredResources(d)
	return FilteredBy(ResourcePredicateFunc(func(groupVersion string, r *metav1.APIResource) bool {
		return r.Namespaced
	}), all), err
}

// ServerVersion retrieves and parses the server's version (git version).
func (d *DiscoveryClient) ServerVersion() (*version.Info, error) {
	body, err := d.restClient.Get().AbsPath("/version").Do(context.TODO()).Raw()
	if err != nil {
		return nil, err
	}
	var info version.Info
	err = json.Unmarshal(body, &info)
	if err != nil {
		return nil, fmt.Errorf("unable to parse the server version: %v", err)
	}
	return &info, nil
}

// OpenAPISchema fetches the open api v2 schema using a rest client and parses the proto.
func (d *DiscoveryClient) OpenAPISchema() (*openapi_v2.Document, error) {
	data, err := d.restClient.Get().AbsPath("/openapi/v2").SetHeader("Accept", openAPIV2mimePb).Do(context.TODO()).Raw()
	if err != nil {
		return nil, err
	}
	document := &openapi_v2.Document{}
	err = proto.Unmarshal(data, document)
	if err != nil {
		return nil, err
	}
	return document, nil
}

func (d *DiscoveryClient) OpenAPIV3() openapi.Client {
	return openapi.NewClient(d.restClient)
}

// WithLegacy returns copy of current discovery client that will only
// receive the legacy discovery format.
func (d *DiscoveryClient) WithLegacy() DiscoveryInterface {
	client := *d
	client.UseLegacyDiscovery = true
	return &client
}

// withRetries retries the given recovery function in case the groups supported by the server change after ServerGroup() returns.
func withRetries(maxRetries int, f func() ([]*metav1.APIGroup, []*metav1.APIResourceList, error)) ([]*metav1.APIGroup, []*metav1.APIResourceList, error) {
	var result []*metav1.APIResourceList
	var resultGroups []*metav1.APIGroup
	var err error
	for i := 0; i < maxRetries; i++ {
		resultGroups, result, err = f()
		if err == nil {
			return resultGroups, result, nil
		}
		if _, ok := err.(*ErrGroupDiscoveryFailed); !ok {
			return nil, nil, err
		}
	}
	return resultGroups, result, err
}

func setDiscoveryDefaults(config *restclient.Config) error {
	config.APIPath = ""
	config.GroupVersion = nil
	if config.Timeout == 0 {
		config.Timeout = defaultTimeout
	}
	// if a burst limit is not already configured
	if config.Burst == 0 {
		// discovery is expected to be bursty, increase the default burst
		// to accommodate looking up resource info for many API groups.
		// matches burst set by ConfigFlags#ToDiscoveryClient().
		// see https://issue.k8s.io/86149
		config.Burst = defaultBurst
	}
	codec := runtime.NoopEncoder{Decoder: scheme.Codecs.UniversalDecoder()}
	config.NegotiatedSerializer = serializer.NegotiatedSerializerWrapper(runtime.SerializerInfo{Serializer: codec})
	if len(config.UserAgent) == 0 {
		config.UserAgent = restclient.DefaultKubernetesUserAgent()
	}
	return nil
}

// NewDiscoveryClientForConfig creates a new DiscoveryClient for the given config. This client
// can be used to discover supported resources in the API server.
// NewDiscoveryClientForConfig is equivalent to NewDiscoveryClientForConfigAndClient(c, httpClient),
// where httpClient was generated with rest.HTTPClientFor(c).
func NewDiscoveryClientForConfig(c *restclient.Config) (*DiscoveryClient, error) {
	config := *c
	if err := setDiscoveryDefaults(&config); err != nil {
		return nil, err
	}
	httpClient, err := restclient.HTTPClientFor(&config)
	if err != nil {
		return nil, err
	}
	return NewDiscoveryClientForConfigAndClient(&config, httpClient)
}

// NewDiscoveryClientForConfigAndClient creates a new DiscoveryClient for the given config. This client
// can be used to discover supported resources in the API server.
// Note the http client provided takes precedence over the configured transport values.
func NewDiscoveryClientForConfigAndClient(c *restclient.Config, httpClient *http.Client) (*DiscoveryClient, error) {
	config := *c
	if err := setDiscoveryDefaults(&config); err != nil {
		return nil, err
	}
	client, err := restclient.UnversionedRESTClientForConfigAndClient(&config, httpClient)
	return &DiscoveryClient{restClient: client, LegacyPrefix: "/api", UseLegacyDiscovery: false}, err
}

// NewDiscoveryClientForConfigOrDie creates a new DiscoveryClient for the given config. If
// there is an error, it panics.
func NewDiscoveryClientForConfigOrDie(c *restclient.Config) *DiscoveryClient {
	client, err := NewDiscoveryClientForConfig(c)
	if err != nil {
		panic(err)
	}
	return client

}

// NewDiscoveryClient returns a new DiscoveryClient for the given RESTClient.
func NewDiscoveryClient(c restclient.Interface) *DiscoveryClient {
	return &DiscoveryClient{restClient: c, LegacyPrefix: "/api", UseLegacyDiscovery: false}
}

// RESTClient returns a RESTClient that is used to communicate
// with API server by this client implementation.
func (d *DiscoveryClient) RESTClient() restclient.Interface {
	if d == nil {
		return nil
	}
	return d.restClient
}
