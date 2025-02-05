/*
Copyright 2017 The Kubernetes Authors.

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
	"context"
	"net/http"
	"strings"
	"sync"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apiserver/pkg/endpoints/discovery"
)

type versionDiscoveryHandler struct {
	// TODO, writing is infrequent, optimize this
	discoveryLock sync.RWMutex
	discovery     map[schema.GroupVersion]*discovery.APIVersionHandler

	delegate http.Handler
}

func (r *versionDiscoveryHandler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	pathParts := splitPath(req.URL.Path)
	// only match /apis/<group>/<version>
	if len(pathParts) != 3 || pathParts[0] != "apis" {
		r.delegate.ServeHTTP(w, req)
		return
	}
	discovery, ok := r.getDiscovery(schema.GroupVersion{Group: pathParts[1], Version: pathParts[2]})
	if !ok {
		r.delegate.ServeHTTP(w, req)
		return
	}

	discovery.ServeHTTP(w, req)
}

func (r *versionDiscoveryHandler) getDiscovery(gv schema.GroupVersion) (*discovery.APIVersionHandler, bool) {
	r.discoveryLock.RLock()
	defer r.discoveryLock.RUnlock()

	ret, ok := r.discovery[gv]
	return ret, ok
}

func (r *versionDiscoveryHandler) setDiscovery(gv schema.GroupVersion, discovery *discovery.APIVersionHandler) {
	r.discoveryLock.Lock()
	defer r.discoveryLock.Unlock()

	r.discovery[gv] = discovery
}

func (r *versionDiscoveryHandler) unsetDiscovery(gv schema.GroupVersion) {
	r.discoveryLock.Lock()
	defer r.discoveryLock.Unlock()

	delete(r.discovery, gv)
}

type groupDiscoveryHandler struct {
	// TODO, writing is infrequent, optimize this
	discoveryLock   sync.RWMutex
	discovery       map[string]*discovery.APIGroupHandler
	discoveryGroups map[string]metav1.APIGroup

	delegate http.Handler
}

func (r *groupDiscoveryHandler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	pathParts := splitPath(req.URL.Path)
	// only match /apis/<group>
	if len(pathParts) != 2 || pathParts[0] != "apis" {
		r.delegate.ServeHTTP(w, req)
		return
	}
	discovery, ok := r.getDiscovery(pathParts[1])
	if !ok {
		r.delegate.ServeHTTP(w, req)
		return
	}

	discovery.ServeHTTP(w, req)
}

func (r *groupDiscoveryHandler) getDiscovery(group string) (*discovery.APIGroupHandler, bool) {
	r.discoveryLock.RLock()
	defer r.discoveryLock.RUnlock()

	ret, ok := r.discovery[group]
	return ret, ok
}

func (r *groupDiscoveryHandler) getGroups() []metav1.APIGroup {
	r.discoveryLock.RLock()
	defer r.discoveryLock.RUnlock()

	groups := make([]metav1.APIGroup, 0, len(r.discoveryGroups))
	for _, apiGroup := range r.discoveryGroups {
		groups = append(groups, apiGroup)
	}

	return groups
}

func (r *groupDiscoveryHandler) setDiscovery(group string, discovery *discovery.APIGroupHandler, apiGroup metav1.APIGroup) {
	r.discoveryLock.Lock()
	defer r.discoveryLock.Unlock()

	r.discovery[group] = discovery
	r.discoveryGroups[group] = apiGroup
}

func (r *groupDiscoveryHandler) unsetDiscovery(group string) {
	r.discoveryLock.Lock()
	defer r.discoveryLock.Unlock()

	delete(r.discovery, group)
	delete(r.discoveryGroups, group)
}

// Groups returns the list of API groups that are served by the API extensions server.
// It uses the CustomResourceDefinitionLister to determine which groups are served.
func (r *groupDiscoveryHandler) Groups(ctx context.Context, _ *http.Request) ([]metav1.APIGroup, error) {
	return r.getGroups(), nil
}

// splitPath returns the segments for a URL path.
func splitPath(path string) []string {
	path = strings.Trim(path, "/")
	if path == "" {
		return []string{}
	}
	return strings.Split(path, "/")
}
