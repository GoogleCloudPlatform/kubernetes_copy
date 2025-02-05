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
	"net/url"

	"k8s.io/apiserver/pkg/util/proxy"
	listersv1 "k8s.io/client-go/listers/core/v1"
	discoveryv1listers "k8s.io/client-go/listers/discovery/v1"
)

// A ServiceResolver knows how to get a URL given a service.
type ServiceResolver interface {
	ResolveEndpoint(namespace, name string, port int32) (*url.URL, error)
}

// NewEndpointServiceResolver returns a ServiceResolver that chooses one of the
// service's endpoints.
func NewEndpointServiceResolver(services listersv1.ServiceLister, endpointSlices discoveryv1listers.EndpointSliceLister) ServiceResolver {
	return &aggregatorEndpointRouting{
		services:       services,
		endpointSlices: endpointSlices,
	}
}

type aggregatorEndpointRouting struct {
	services       listersv1.ServiceLister
	endpointSlices discoveryv1listers.EndpointSliceLister
}

func (r *aggregatorEndpointRouting) ResolveEndpoint(namespace, name string, port int32) (*url.URL, error) {
	return proxy.ResolveEndpoint(r.services, r.endpointSlices, namespace, name, port)
}

// NewClusterIPServiceResolver returns a ServiceResolver that directly calls the
// service's cluster IP.
func NewClusterIPServiceResolver(services listersv1.ServiceLister) ServiceResolver {
	return &aggregatorClusterRouting{
		services: services,
	}
}

type aggregatorClusterRouting struct {
	services listersv1.ServiceLister
}

func (r *aggregatorClusterRouting) ResolveEndpoint(namespace, name string, port int32) (*url.URL, error) {
	return proxy.ResolveCluster(r.services, namespace, name, port)
}

// NewLoopbackServiceResolver returns a ServiceResolver that routes
// the kubernetes/default service with port 443 to loopback.
func NewLoopbackServiceResolver(delegate ServiceResolver, host *url.URL) ServiceResolver {
	return &loopbackResolver{
		delegate: delegate,
		host:     host,
	}
}

type loopbackResolver struct {
	delegate ServiceResolver
	host     *url.URL
}

func (r *loopbackResolver) ResolveEndpoint(namespace, name string, port int32) (*url.URL, error) {
	if namespace == "default" && name == "kubernetes" && port == 443 {
		return r.host, nil
	}
	return r.delegate.ResolveEndpoint(namespace, name, port)
}
