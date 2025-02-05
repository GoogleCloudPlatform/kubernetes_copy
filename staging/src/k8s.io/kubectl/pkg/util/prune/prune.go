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

package prune

import (
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// default allowlist of namespaced resources
var defaultNamespacedPruneResources = []Resource{
	{"", "v1", "ConfigMap", true},
	{"", "v1", "Endpoints", true},
	{"", "v1", "PersistentVolumeClaim", true},
	{"", "v1", "Pod", true},
	{"", "v1", "ReplicationController", true},
	{"", "v1", "Secret", true},
	{"", "v1", "Service", true},
	{"batch", "v1", "Job", true},
	{"batch", "v1", "CronJob", true},
	{"networking.k8s.io", "v1", "Ingress", true},
	{"apps", "v1", "DaemonSet", true},
	{"apps", "v1", "Deployment", true},
	{"apps", "v1", "ReplicaSet", true},
	{"apps", "v1", "StatefulSet", true},
}

// default allowlist of non-namespaced resources
var defaultNonNamespacedPruneResources = []Resource{
	{"", "v1", "Namespace", false},
	{"", "v1", "PersistentVolume", false},
}

type Resource struct {
	group      string
	version    string
	kind       string
	namespaced bool
}

func (pr Resource) String() string {
	return fmt.Sprintf("%v/%v, Kind=%v, Namespaced=%v", pr.group, pr.version, pr.kind, pr.namespaced)
}

// if namespace is explicitly specified, the default allow list should not include non-namespaced resources.
// if pruneResources is specified by user, respect the user setting.
func GetRESTMappings(mapper meta.RESTMapper, pruneResources []Resource, namespaceSpecified bool) (namespaced, nonNamespaced []*meta.RESTMapping, err error) {
	if len(pruneResources) == 0 {
		pruneResources = defaultNamespacedPruneResources
		if !namespaceSpecified {
			pruneResources = append(pruneResources, defaultNonNamespacedPruneResources...)
		}
	}

	for _, resource := range pruneResources {
		addedMapping, err := mapper.RESTMapping(schema.GroupKind{Group: resource.group, Kind: resource.kind}, resource.version)
		if err != nil {
			return nil, nil, fmt.Errorf("invalid resource %v: %v", resource, err)
		}
		if resource.namespaced {
			namespaced = append(namespaced, addedMapping)
		} else {
			nonNamespaced = append(nonNamespaced, addedMapping)
		}
	}

	return namespaced, nonNamespaced, nil
}

func ParseResources(mapper meta.RESTMapper, gvks []string) ([]Resource, error) {
	pruneResources := []Resource{}
	for _, groupVersionKind := range gvks {
		gvk := strings.Split(groupVersionKind, "/")
		if len(gvk) != 3 {
			return nil, fmt.Errorf("invalid GroupVersionKind format: %v, please follow <group/version/kind>", groupVersionKind)
		}

		if gvk[0] == "core" {
			gvk[0] = ""
		}
		mapping, err := mapper.RESTMapping(schema.GroupKind{Group: gvk[0], Kind: gvk[2]}, gvk[1])
		if err != nil {
			return pruneResources, err
		}
		var namespaced bool
		namespaceScope := mapping.Scope.Name()
		switch namespaceScope {
		case meta.RESTScopeNameNamespace:
			namespaced = true
		case meta.RESTScopeNameRoot:
			namespaced = false
		default:
			return pruneResources, fmt.Errorf("Unknown namespace scope: %q", namespaceScope)
		}

		pruneResources = append(pruneResources, Resource{gvk[0], gvk[1], gvk[2], namespaced})
	}
	return pruneResources, nil
}
