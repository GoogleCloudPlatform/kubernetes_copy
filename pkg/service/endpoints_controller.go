/*
Copyright 2014 Google Inc. All rights reserved.

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

package service

import (
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/GoogleCloudPlatform/kubernetes/pkg/api"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/client"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/labels"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/registry/service"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/util"

	"github.com/golang/glog"
)

// EndpointController manages service endpoints.
type EndpointController struct {
	client          *client.Client
	serviceRegistry service.Registry
}

// NewEndpointController returns a new *EndpointController.
func NewEndpointController(serviceRegistry service.Registry, client *client.Client) *EndpointController {
	return &EndpointController{
		serviceRegistry: serviceRegistry,
		client:          client,
	}
}

// SyncServiceEndpoints syncs service endpoints.
func (e *EndpointController) SyncServiceEndpoints() error {
	ctx := api.NewContext()
	services, err := e.client.ListServices(ctx, labels.Everything())
	if err != nil {
		glog.Errorf("Failed to list services: %v", err)
		return err
	}
	var resultErr error
	for _, service := range services.Items {
		nsCtx := api.WithNamespace(ctx, service.Metadata.Namespace)
		pods, err := e.client.ListPods(nsCtx, labels.Set(service.Spec.Selector).AsSelector())
		if err != nil {
			glog.Errorf("Error syncing service: %#v, skipping.", service)
			resultErr = err
			continue
		}
		endpoints := make([]string, len(pods.Items))
		for ix, pod := range pods.Items {
			port, err := findPort(&pod.Spec, service.ContainerPort)
			if err != nil {
				glog.Errorf("Failed to find port for service: %v, %v", service, err)
				continue
			}
			if len(pod.Status.PodIP) == 0 {
				glog.Errorf("Failed to find an IP for pod: %v", pod)
				continue
			}
			endpoints[ix] = net.JoinHostPort(pod.Status.PodIP, strconv.Itoa(port))
		}
		currentEndpoints, err := e.client.GetEndpoints(nsCtx, service.Metadata.Name)
		if err != nil {
			// TODO this is brittle as all get out, refactor the client libraries to return a structured error.
			if strings.Contains(err.Error(), "not found") {
				currentEndpoints = &api.Endpoints{
					JSONBase: api.JSONBase{
						ID: service.Metadata.Name,
					},
				}
			} else {
				glog.Errorf("Error getting endpoints: %#v", err)
				continue
			}
		}
		newEndpoints := &api.Endpoints{}
		*newEndpoints = *currentEndpoints
		newEndpoints.Endpoints = endpoints

		if currentEndpoints.ResourceVersion == 0 {
			// No previous endpoints, create them
			_, err = e.client.CreateEndpoints(nsCtx, newEndpoints)
		} else {
			// Pre-existing
			if endpointsEqual(currentEndpoints, endpoints) {
				glog.V(2).Infof("endpoints are equal for %s, skipping update", service.Metadata.Name)
				continue
			}
			_, err = e.client.UpdateEndpoints(nsCtx, newEndpoints)
		}
		if err != nil {
			glog.Errorf("Error updating endpoints: %#v", err)
			continue
		}
	}
	return resultErr
}

func containsEndpoint(endpoints *api.Endpoints, endpoint string) bool {
	if endpoints == nil {
		return false
	}
	for ix := range endpoints.Endpoints {
		if endpoints.Endpoints[ix] == endpoint {
			return true
		}
	}
	return false
}

func endpointsEqual(e *api.Endpoints, endpoints []string) bool {
	if len(e.Endpoints) != len(endpoints) {
		return false
	}
	for _, endpoint := range endpoints {
		if !containsEndpoint(e, endpoint) {
			return false
		}
	}
	return true
}

// findPort locates the container port for the given manifest and portName.
func findPort(manifest *api.ContainerManifest, portName util.IntOrString) (int, error) {
	if ((portName.Kind == util.IntstrString && len(portName.StrVal) == 0) ||
		(portName.Kind == util.IntstrInt && portName.IntVal == 0)) &&
		len(manifest.Containers[0].Ports) > 0 {
		return manifest.Containers[0].Ports[0].ContainerPort, nil
	}
	if portName.Kind == util.IntstrInt {
		return portName.IntVal, nil
	}
	name := portName.StrVal
	for _, container := range manifest.Containers {
		for _, port := range container.Ports {
			if port.Name == name {
				return port.ContainerPort, nil
			}
		}
	}
	return -1, fmt.Errorf("no suitable port for manifest: %s", manifest.ID)
}
