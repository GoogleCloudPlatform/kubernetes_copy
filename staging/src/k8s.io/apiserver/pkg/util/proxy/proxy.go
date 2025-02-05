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

package proxy

import (
	"context"
	"fmt"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	utilnet "k8s.io/apimachinery/pkg/util/net"
	auditinternal "k8s.io/apiserver/pkg/apis/audit"
	"k8s.io/apiserver/pkg/audit"
	genericapirequest "k8s.io/apiserver/pkg/endpoints/request"
	listersv1 "k8s.io/client-go/listers/core/v1"
	discoveryv1listers "k8s.io/client-go/listers/discovery/v1"
)

const (
	// taken from https://github.com/kubernetes/kubernetes/blob/release-1.27/staging/src/k8s.io/kube-aggregator/pkg/apiserver/handler_proxy.go#L47
	aggregatedDiscoveryTimeout = 5 * time.Second
)

// findServicePort finds the service port by name or numerically.
func findServicePort(svc *v1.Service, port int32) (*v1.ServicePort, error) {
	for _, svcPort := range svc.Spec.Ports {
		if svcPort.Port == port {
			return &svcPort, nil
		}
	}
	return nil, errors.NewServiceUnavailable(fmt.Sprintf("no service port %d found for service %q", port, svc.Name))
}

// ResolveEndpoint returns a URL to which one can send traffic for the specified service.
// If the service is dual-stack, the URL will preferentially point to an endpoint of the
// service's primary IP family.
func ResolveEndpoint(services listersv1.ServiceLister, endpointSlices discoveryv1listers.EndpointSliceLister, namespace, id string, port int32) (*url.URL, error) {
	svc, err := services.Services(namespace).Get(id)
	if err != nil {
		return nil, err
	}

	switch {
	case svc.Spec.Type == v1.ServiceTypeClusterIP, svc.Spec.Type == v1.ServiceTypeLoadBalancer, svc.Spec.Type == v1.ServiceTypeNodePort:
		// these are fine
	default:
		return nil, fmt.Errorf("unsupported service type %q", svc.Spec.Type)
	}

	svcPort, err := findServicePort(svc, port)
	if err != nil {
		return nil, err
	}

	endpointSliceSelector := labels.SelectorFromSet(labels.Set{discoveryv1.LabelServiceName: svc.Name})
	slices, err := endpointSlices.EndpointSlices(namespace).List(endpointSliceSelector)
	if err != nil {
		return nil, err
	}
	if len(slices) == 0 {
		return nil, errors.NewServiceUnavailable(fmt.Sprintf("no endpoints available for service %q", svc.Name))
	} else if len(slices) > 1 {
		// If there are multiple slices, we want to look at them in a random
		// order, but we need to look at all of the slices of the primary IP
		// family first.
		preferredAddressType := discoveryv1.AddressType(svc.Spec.IPFamilies[0])
		randomOrder := rand.Perm(len(slices))
		sort.Slice(slices, func(i, j int) bool {
			if slices[i].AddressType != slices[j].AddressType {
				return slices[i].AddressType == preferredAddressType
			}
			return randomOrder[i] < randomOrder[j]
		})
	}

	// Find a slice that has the port.
	for _, slice := range slices {
		for i := range slice.Ports {
			if slice.Ports[i].Name == nil || *slice.Ports[i].Name != svcPort.Name {
				continue
			}
			if len(slice.Endpoints) == 0 {
				continue
			}

			// Starting from a random index, find a Ready endpoint
			offset := rand.Intn(len(slice.Endpoints))
			for epi := range slice.Endpoints {
				ep := &slice.Endpoints[(epi+offset)%len(slice.Endpoints)]
				if ep.Conditions.Ready == nil || *ep.Conditions.Ready {
					// (Addresses is an array but only Addresses[0] is used.)
					ip := ep.Addresses[0]
					port := int(*slice.Ports[i].Port)
					return &url.URL{
						Scheme: "https",
						Host:   net.JoinHostPort(ip, strconv.Itoa(port)),
					}, nil
				}
			}
		}
	}
	return nil, errors.NewServiceUnavailable(fmt.Sprintf("no endpoints available for service %q", id))
}

func ResolveCluster(services listersv1.ServiceLister, namespace, id string, port int32) (*url.URL, error) {
	svc, err := services.Services(namespace).Get(id)
	if err != nil {
		return nil, err
	}

	switch {
	case svc.Spec.Type == v1.ServiceTypeClusterIP && svc.Spec.ClusterIP == v1.ClusterIPNone:
		return nil, fmt.Errorf(`cannot route to service with ClusterIP "None"`)
	// use IP from a clusterIP for these service types
	case svc.Spec.Type == v1.ServiceTypeClusterIP, svc.Spec.Type == v1.ServiceTypeLoadBalancer, svc.Spec.Type == v1.ServiceTypeNodePort:
		svcPort, err := findServicePort(svc, port)
		if err != nil {
			return nil, err
		}
		return &url.URL{
			Scheme: "https",
			Host:   net.JoinHostPort(svc.Spec.ClusterIP, fmt.Sprintf("%d", svcPort.Port)),
		}, nil
	case svc.Spec.Type == v1.ServiceTypeExternalName:
		return &url.URL{
			Scheme: "https",
			Host:   net.JoinHostPort(svc.Spec.ExternalName, fmt.Sprintf("%d", port)),
		}, nil
	default:
		return nil, fmt.Errorf("unsupported service type %q", svc.Spec.Type)
	}
}

// NewRequestForProxy returns a shallow copy of the original request with a context that may include a timeout for discovery requests
func NewRequestForProxy(location *url.URL, req *http.Request) (*http.Request, context.CancelFunc) {
	newCtx := req.Context()
	cancelFn := func() {}

	if requestInfo, ok := genericapirequest.RequestInfoFrom(req.Context()); ok {
		// trim leading and trailing slashes. Then "/apis/group/version" requests are for discovery, so if we have exactly three
		// segments that we are going to proxy, we have a discovery request.
		if !requestInfo.IsResourceRequest && len(strings.Split(strings.Trim(requestInfo.Path, "/"), "/")) == 3 {
			// discovery requests are used by kubectl and others to determine which resources a server has.  This is a cheap call that
			// should be fast for every aggregated apiserver.  Latency for aggregation is expected to be low (as for all extensions)
			// so forcing a short timeout here helps responsiveness of all clients.
			newCtx, cancelFn = context.WithTimeout(newCtx, aggregatedDiscoveryTimeout)
		}
	}

	// WithContext creates a shallow clone of the request with the same context.
	newReq := req.WithContext(newCtx)
	newReq.Header = utilnet.CloneHeader(req.Header)
	newReq.URL = location
	newReq.Host = location.Host

	// If the original request has an audit ID, let's make sure we propagate this
	// to the aggregated server.
	if auditID, found := audit.AuditIDFrom(req.Context()); found {
		newReq.Header.Set(auditinternal.HeaderAuditID, string(auditID))
	}

	return newReq, cancelFn
}
