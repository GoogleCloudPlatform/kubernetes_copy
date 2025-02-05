/*
Copyright 2022 The Kubernetes Authors.

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

package aggregated

import (
	"fmt"
	"net/http"
	"reflect"
	"sort"
	"sync"

	apidiscoveryv2 "k8s.io/api/apidiscovery/v2"
	apidiscoveryv2beta1 "k8s.io/api/apidiscovery/v2beta1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	utilversion "k8s.io/apimachinery/pkg/util/version"
	"k8s.io/apimachinery/pkg/version"
	apidiscoveryv2conversion "k8s.io/apiserver/pkg/apis/apidiscovery/v2"
	componentbaseversion "k8s.io/component-base/version"

	"k8s.io/apiserver/pkg/endpoints/handlers/responsewriters"

	"k8s.io/apiserver/pkg/endpoints/handlers/negotiation"
	"k8s.io/apiserver/pkg/endpoints/metrics"

	"sync/atomic"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/klog/v2"
)

type Source uint

// The GroupVersion from the lowest Source takes precedence
const (
	AggregatorSource Source = 0
	BuiltinSource    Source = 100
	CRDSource        Source = 200
)

var V2Beta1RemovedVersion = utilversion.MustParseMajorMinor("1.33")

// This handler serves the /apis endpoint for an aggregated list of
// api resources indexed by their group version.
type ResourceManager interface {
	// Adds knowledge of the given groupversion to the discovery document
	// If it was already being tracked, updates the stored APIVersionDiscovery
	// Thread-safe
	AddGroupVersion(groupName string, value apidiscoveryv2.APIVersionDiscovery)

	// Sets a priority to be used while sorting a specific group and
	// group-version. If two versions report different priorities for
	// the group, the higher one will be used. If the group is not
	// known, the priority is ignored. The priority for this version
	// is forgotten once the group-version is forgotten
	SetGroupVersionPriority(gv metav1.GroupVersion, grouppriority, versionpriority int)

	// Removes all group versions for a given group
	// Thread-safe
	RemoveGroup(groupName string)

	// Removes a specific groupversion. If all versions of a group have been
	// removed, then the entire group is unlisted.
	// Thread-safe
	RemoveGroupVersion(gv metav1.GroupVersion)

	// Resets the manager's known list of group-versions and replaces them
	// with the given groups
	// Thread-Safe
	SetGroups([]apidiscoveryv2.APIGroupDiscovery)

	// Returns the same resource manager using a different source
	// The source is used to decide how to de-duplicate groups.
	// The group from the least-numbered source is used
	WithSource(source Source) ResourceManager

	http.Handler
}

type resourceManager struct {
	source Source
	*resourceDiscoveryManager
}

func (rm resourceManager) AddGroupVersion(groupName string, value apidiscoveryv2.APIVersionDiscovery) {
	rm.resourceDiscoveryManager.AddGroupVersion(rm.source, groupName, value)
}
func (rm resourceManager) SetGroupVersionPriority(gv metav1.GroupVersion, grouppriority, versionpriority int) {
	rm.resourceDiscoveryManager.SetGroupVersionPriority(rm.source, gv, grouppriority, versionpriority)
}
func (rm resourceManager) RemoveGroup(groupName string) {
	rm.resourceDiscoveryManager.RemoveGroup(rm.source, groupName)
}
func (rm resourceManager) RemoveGroupVersion(gv metav1.GroupVersion) {
	rm.resourceDiscoveryManager.RemoveGroupVersion(rm.source, gv)
}
func (rm resourceManager) SetGroups(groups []apidiscoveryv2.APIGroupDiscovery) {
	rm.resourceDiscoveryManager.SetGroups(rm.source, groups)
}

func (rm resourceManager) WithSource(source Source) ResourceManager {
	return resourceManager{
		source:                   source,
		resourceDiscoveryManager: rm.resourceDiscoveryManager,
	}
}

type groupKey struct {
	name string

	// Source identifies where this group came from and dictates which group
	// among duplicates is chosen to be used for discovery.
	source Source
}

type groupVersionKey struct {
	metav1.GroupVersion
	source Source
}

type resourceDiscoveryManager struct {
	serializer runtime.NegotiatedSerializer
	// cache is an atomic pointer to avoid the use of locks
	cache atomic.Pointer[cachedGroupList]

	serveHTTPFunc http.HandlerFunc

	// Writes protected by the lock.
	// List of all apigroups & resources indexed by the resource manager
	lock              sync.RWMutex
	apiGroups         map[groupKey]*apidiscoveryv2.APIGroupDiscovery
	versionPriorities map[groupVersionKey]priorityInfo
}

type priorityInfo struct {
	GroupPriorityMinimum int
	VersionPriority      int
}

func NewResourceManager(path string) ResourceManager {
	return NewResourceManagerWithEmulatedVersion(path, componentbaseversion.DefaultKubeEffectiveVersion().EmulationVersion())
}

func NewResourceManagerWithEmulatedVersion(path string, emulatedVersion *utilversion.Version) ResourceManager {
	scheme := runtime.NewScheme()
	utilruntime.Must(apidiscoveryv2.AddToScheme(scheme))
	// Only register v2beta1 and corresponding conversions
	// if the emulated version is less than 1.33 as the type
	// is removed in the corresponding release
	if emulatedVersion.LessThan(V2Beta1RemovedVersion) {
		utilruntime.Must(apidiscoveryv2beta1.AddToScheme(scheme))
		// Register conversion for apidiscovery
		utilruntime.Must(apidiscoveryv2conversion.RegisterConversions(scheme))
	}

	codecs := serializer.NewCodecFactory(scheme)
	rdm := &resourceDiscoveryManager{
		serializer:        codecs,
		versionPriorities: make(map[groupVersionKey]priorityInfo),
	}
	rdm.serveHTTPFunc = metrics.InstrumentHandlerFunc("GET",
		/* group = */ "",
		/* version = */ "",
		/* resource = */ "",
		/* subresource = */ path,
		/* scope = */ "",
		/* component = */ metrics.APIServerComponent,
		/* deprecated */ false,
		/* removedRelease */ "",
		rdm.serveHTTP)
	return resourceManager{
		source:                   BuiltinSource,
		resourceDiscoveryManager: rdm,
	}
}

func (rdm *resourceDiscoveryManager) SetGroupVersionPriority(source Source, gv metav1.GroupVersion, groupPriorityMinimum, versionPriority int) {
	rdm.lock.Lock()
	defer rdm.lock.Unlock()

	key := groupVersionKey{
		GroupVersion: gv,
		source:       source,
	}
	rdm.versionPriorities[key] = priorityInfo{
		GroupPriorityMinimum: groupPriorityMinimum,
		VersionPriority:      versionPriority,
	}
	rdm.cache.Store(nil)
}

func (rdm *resourceDiscoveryManager) SetGroups(source Source, groups []apidiscoveryv2.APIGroupDiscovery) {
	rdm.lock.Lock()
	defer rdm.lock.Unlock()

	rdm.apiGroups = nil
	rdm.cache.Store(nil)

	for _, group := range groups {
		for _, version := range group.Versions {
			rdm.addGroupVersionLocked(source, group.Name, version)
		}
	}

	// Filter unused out priority entries
	for gv := range rdm.versionPriorities {
		key := groupKey{
			source: source,
			name:   gv.Group,
		}
		entry, exists := rdm.apiGroups[key]
		if !exists {
			delete(rdm.versionPriorities, gv)
			continue
		}

		containsVersion := false

		for _, v := range entry.Versions {
			if v.Version == gv.Version {
				containsVersion = true
				break
			}
		}

		if !containsVersion {
			delete(rdm.versionPriorities, gv)
		}
	}
}

func (rdm *resourceDiscoveryManager) AddGroupVersion(source Source, groupName string, value apidiscoveryv2.APIVersionDiscovery) {
	rdm.lock.Lock()
	defer rdm.lock.Unlock()

	rdm.addGroupVersionLocked(source, groupName, value)
}

func (rdm *resourceDiscoveryManager) addGroupVersionLocked(source Source, groupName string, value apidiscoveryv2.APIVersionDiscovery) {

	if rdm.apiGroups == nil {
		rdm.apiGroups = make(map[groupKey]*apidiscoveryv2.APIGroupDiscovery)
	}

	key := groupKey{
		source: source,
		name:   groupName,
	}

	if existing, groupExists := rdm.apiGroups[key]; groupExists {
		// If this version already exists, replace it
		versionExists := false

		// Not very efficient, but in practice there are generally not many versions
		for i := range existing.Versions {
			if existing.Versions[i].Version == value.Version {
				// The new gv is the exact same as what is already in
				// the map. This is a noop and cache should not be
				// invalidated.
				if reflect.DeepEqual(existing.Versions[i], value) {
					return
				}

				existing.Versions[i] = value
				versionExists = true
				break
			}
		}

		if !versionExists {
			existing.Versions = append(existing.Versions, value)
		}

	} else {
		group := &apidiscoveryv2.APIGroupDiscovery{
			ObjectMeta: metav1.ObjectMeta{
				Name: groupName,
			},
			Versions: []apidiscoveryv2.APIVersionDiscovery{value},
		}
		rdm.apiGroups[key] = group
	}
	klog.Infof("Adding GroupVersion %s %s to ResourceManager", groupName, value.Version)

	gv := metav1.GroupVersion{Group: groupName, Version: value.Version}
	gvKey := groupVersionKey{
		GroupVersion: gv,
		source:       source,
	}
	if _, ok := rdm.versionPriorities[gvKey]; !ok {
		rdm.versionPriorities[gvKey] = priorityInfo{
			GroupPriorityMinimum: 1000,
			VersionPriority:      15,
		}
	}

	// Reset response document so it is recreated lazily
	rdm.cache.Store(nil)
}

func (rdm *resourceDiscoveryManager) RemoveGroupVersion(source Source, apiGroup metav1.GroupVersion) {
	rdm.lock.Lock()
	defer rdm.lock.Unlock()

	key := groupKey{
		source: source,
		name:   apiGroup.Group,
	}

	group, exists := rdm.apiGroups[key]
	if !exists {
		return
	}

	modified := false
	for i := range group.Versions {
		if group.Versions[i].Version == apiGroup.Version {
			group.Versions = append(group.Versions[:i], group.Versions[i+1:]...)
			modified = true
			break
		}
	}
	// If no modification was done, cache does not need to be cleared
	if !modified {
		return
	}

	gvKey := groupVersionKey{
		GroupVersion: apiGroup,
		source:       source,
	}

	delete(rdm.versionPriorities, gvKey)
	if len(group.Versions) == 0 {
		delete(rdm.apiGroups, key)
	}

	// Reset response document so it is recreated lazily
	rdm.cache.Store(nil)
}

func (rdm *resourceDiscoveryManager) RemoveGroup(source Source, groupName string) {
	rdm.lock.Lock()
	defer rdm.lock.Unlock()

	key := groupKey{
		source: source,
		name:   groupName,
	}

	delete(rdm.apiGroups, key)

	for k := range rdm.versionPriorities {
		if k.Group == groupName && k.source == source {
			delete(rdm.versionPriorities, k)
		}
	}

	// Reset response document so it is recreated lazily
	rdm.cache.Store(nil)
}

// Prepares the api group list for serving by converting them from map into
// list and sorting them according to insertion order
func (rdm *resourceDiscoveryManager) calculateAPIGroupsLocked() []apidiscoveryv2.APIGroupDiscovery {
	regenerationCounter.Inc()
	// Re-order the apiGroups by their priority.
	groups := []apidiscoveryv2.APIGroupDiscovery{}

	groupsToUse := map[string]apidiscoveryv2.APIGroupDiscovery{}
	sourcesUsed := map[metav1.GroupVersion]Source{}

	for key, group := range rdm.apiGroups {
		if existing, ok := groupsToUse[key.name]; ok {
			for _, v := range group.Versions {
				gv := metav1.GroupVersion{Group: key.name, Version: v.Version}

				// Skip groupversions we've already seen before. Only DefaultSource
				// takes precedence
				if usedSource, seen := sourcesUsed[gv]; seen && key.source >= usedSource {
					continue
				} else if seen {
					// Find the index of the duplicate version and replace
					for i := 0; i < len(existing.Versions); i++ {
						if existing.Versions[i].Version == v.Version {
							existing.Versions[i] = v
							break
						}
					}

				} else {
					// New group-version, just append
					existing.Versions = append(existing.Versions, v)
				}

				sourcesUsed[gv] = key.source
				groupsToUse[key.name] = existing
			}
			// Check to see if we have overlapping versions. If we do, take the one
			// with highest source precedence
		} else {
			groupsToUse[key.name] = *group.DeepCopy()
			for _, v := range group.Versions {
				gv := metav1.GroupVersion{Group: key.name, Version: v.Version}
				sourcesUsed[gv] = key.source
			}
		}
	}

	for _, group := range groupsToUse {

		// Re-order versions based on their priority. Use kube-aware string
		// comparison as a tie breaker
		sort.SliceStable(group.Versions, func(i, j int) bool {
			iVersion := group.Versions[i].Version
			jVersion := group.Versions[j].Version

			iGV := metav1.GroupVersion{Group: group.Name, Version: iVersion}
			jGV := metav1.GroupVersion{Group: group.Name, Version: jVersion}

			iSource := sourcesUsed[iGV]
			jSource := sourcesUsed[jGV]

			iPriority := rdm.versionPriorities[groupVersionKey{iGV, iSource}].VersionPriority
			jPriority := rdm.versionPriorities[groupVersionKey{jGV, jSource}].VersionPriority

			// Sort by version string comparator if priority is equal
			if iPriority == jPriority {
				return version.CompareKubeAwareVersionStrings(iVersion, jVersion) > 0
			}

			// i sorts before j if it has a higher priority
			return iPriority > jPriority
		})

		groups = append(groups, group)
	}

	// For each group, determine the highest minimum group priority and use that
	priorities := map[string]int{}
	for gv, info := range rdm.versionPriorities {
		if source := sourcesUsed[gv.GroupVersion]; source != gv.source {
			continue
		}

		if existing, exists := priorities[gv.Group]; exists {
			if existing < info.GroupPriorityMinimum {
				priorities[gv.Group] = info.GroupPriorityMinimum
			}
		} else {
			priorities[gv.Group] = info.GroupPriorityMinimum
		}
	}

	sort.SliceStable(groups, func(i, j int) bool {
		iName := groups[i].Name
		jName := groups[j].Name

		// Default to 0 priority by default
		iPriority := priorities[iName]
		jPriority := priorities[jName]

		// Sort discovery based on apiservice priority.
		// Duplicated from staging/src/k8s.io/kube-aggregator/pkg/apis/apiregistration/v1/helpers.go
		if iPriority == jPriority {
			// Equal priority uses name to break ties
			return iName < jName
		}

		// i sorts before j if it has a higher priority
		return iPriority > jPriority
	})

	return groups
}

// Fetches from cache if it exists. If cache is empty, create it.
func (rdm *resourceDiscoveryManager) fetchFromCache() *cachedGroupList {
	rdm.lock.RLock()
	defer rdm.lock.RUnlock()

	cacheLoad := rdm.cache.Load()
	if cacheLoad != nil {
		return cacheLoad
	}
	response := apidiscoveryv2.APIGroupDiscoveryList{
		Items: rdm.calculateAPIGroupsLocked(),
	}
	etag, err := calculateETag(response)
	if err != nil {
		klog.Errorf("failed to calculate etag for discovery document: %s", etag)
		etag = ""
	}
	cached := &cachedGroupList{
		cachedResponse:     response,
		cachedResponseETag: etag,
	}
	rdm.cache.Store(cached)
	return cached
}

type cachedGroupList struct {
	cachedResponse apidiscoveryv2.APIGroupDiscoveryList
	// etag is calculated based on a SHA hash of only the JSON object.
	// A response via different Accept encodings (eg: protobuf, json) will
	// yield the same etag. This is okay because Accept is part of the Vary header.
	// Per RFC7231 a client must only cache a response etag pair if the header field
	// matches as indicated by the Vary field. Thus, protobuf and json and other Accept
	// encodings will not be cached as the same response despite having the same etag.
	cachedResponseETag string
}

func (rdm *resourceDiscoveryManager) ServeHTTP(resp http.ResponseWriter, req *http.Request) {
	rdm.serveHTTPFunc(resp, req)
}

func (rdm *resourceDiscoveryManager) serveHTTP(resp http.ResponseWriter, req *http.Request) {
	cache := rdm.fetchFromCache()
	response := cache.cachedResponse
	etag := cache.cachedResponseETag

	mediaType, _, err := negotiation.NegotiateOutputMediaType(req, rdm.serializer, DiscoveryEndpointRestrictions)
	if err != nil {
		// Should never happen. wrapper.go will only proxy requests to this
		// handler if the media type passes DiscoveryEndpointRestrictions
		utilruntime.HandleError(err)
		resp.WriteHeader(http.StatusInternalServerError)
		return
	}
	var targetGV schema.GroupVersion
	if mediaType.Convert == nil ||
		(mediaType.Convert.GroupVersion() != apidiscoveryv2.SchemeGroupVersion &&
			mediaType.Convert.GroupVersion() != apidiscoveryv2beta1.SchemeGroupVersion) {
		utilruntime.HandleError(fmt.Errorf("expected aggregated discovery group version, got group: %s, version %s", mediaType.Convert.Group, mediaType.Convert.Version))
		resp.WriteHeader(http.StatusInternalServerError)
		return
	}
	targetGV = mediaType.Convert.GroupVersion()

	if len(etag) > 0 {
		// Use proper e-tag headers if one is available
		ServeHTTPWithETag(
			&response,
			etag,
			targetGV,
			rdm.serializer,
			resp,
			req,
		)
	} else {
		// Default to normal response in rare case etag is
		// not cached with the object for some reason.
		responsewriters.WriteObjectNegotiated(
			rdm.serializer,
			DiscoveryEndpointRestrictions,
			targetGV,
			resp,
			req,
			http.StatusOK,
			&response,
			true,
		)
	}
}
