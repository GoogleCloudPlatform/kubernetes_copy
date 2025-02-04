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

// Package app does all of the work necessary to create a Kubernetes
// APIServer by binding together the API, master and APIServer infrastructure.
// It can be configured and called directly or via the hyperkube framework.
package app

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	controlplaneapiserver "k8s.io/kubernetes/pkg/controlplane/apiserver"
)

// The proper way to resolve this letting the aggregator know the desired group and version-within-group order of the underlying servers
// is to refactor the genericapiserver.DelegationTarget to include a list of priorities based on which APIs were installed.
// This requires the APIGroupInfo struct to evolve and include the concept of priorities and to avoid mistakes, the core storage map there needs to be updated.
// That ripples out every bit as far as you'd expect, so for 1.7 we'll include the list here instead of being built up during storage.
var apiVersionPriorities = merge(controlplaneapiserver.DefaultGenericAPIServicePriorities(), map[schema.GroupVersion]controlplaneapiserver.APIServicePriority{
	{Group: "", Version: "v1"}: {Group: 18000, Version: 1},
	// to my knowledge, nothing below here collides
	{Group: "apps", Version: "v1"}:                    {Group: 17800, Version: 15},
	{Group: "autoscaling", Version: "v1"}:             {Group: 17500, Version: 15},
	{Group: "autoscaling", Version: "v2"}:             {Group: 17500, Version: 30},
	{Group: "autoscaling", Version: "v2beta1"}:        {Group: 17500, Version: 9},
	{Group: "autoscaling", Version: "v2beta2"}:        {Group: 17500, Version: 1},
	{Group: "batch", Version: "v1"}:                   {Group: 17400, Version: 15},
	{Group: "batch", Version: "v1beta1"}:              {Group: 17400, Version: 9},
	{Group: "batch", Version: "v2alpha1"}:             {Group: 17400, Version: 9},
	{Group: "networking.k8s.io", Version: "v1"}:       {Group: 17200, Version: 15},
	{Group: "networking.k8s.io", Version: "v1beta1"}:  {Group: 17200, Version: 9},
	{Group: "networking.k8s.io", Version: "v1alpha1"}: {Group: 17200, Version: 1},
	{Group: "policy", Version: "v1"}:                  {Group: 17100, Version: 15},
	{Group: "policy", Version: "v1beta1"}:             {Group: 17100, Version: 9},
	{Group: "storage.k8s.io", Version: "v1"}:          {Group: 16800, Version: 15},
	{Group: "storage.k8s.io", Version: "v1beta1"}:     {Group: 16800, Version: 9},
	{Group: "storage.k8s.io", Version: "v1alpha1"}:    {Group: 16800, Version: 1},
	{Group: "scheduling.k8s.io", Version: "v1"}:       {Group: 16600, Version: 15},
	{Group: "node.k8s.io", Version: "v1"}:             {Group: 16300, Version: 15},
	{Group: "node.k8s.io", Version: "v1alpha1"}:       {Group: 16300, Version: 1},
	{Group: "node.k8s.io", Version: "v1beta1"}:        {Group: 16300, Version: 9},
	{Group: "resource.k8s.io", Version: "v1beta2"}:    {Group: 16200, Version: 15},
	{Group: "resource.k8s.io", Version: "v1beta1"}:    {Group: 16200, Version: 9},
	{Group: "resource.k8s.io", Version: "v1alpha3"}:   {Group: 16200, Version: 1},
	// Append a new group to the end of the list if unsure.
	// You can use min(existing group)-100 as the initial value for a group.
	// Version can be set to 9 (to have space around) for a new group.
})

func merge(a, b map[schema.GroupVersion]controlplaneapiserver.APIServicePriority) map[schema.GroupVersion]controlplaneapiserver.APIServicePriority {
	for k, v := range b {
		a[k] = v
	}
	return a
}
