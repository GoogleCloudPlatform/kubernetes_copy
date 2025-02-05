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

package server

import (
	"net/http"
	"strings"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apiserver/pkg/authentication/authenticator"
	"k8s.io/apiserver/pkg/authentication/user"
	"k8s.io/apiserver/pkg/authorization/authorizer"
	"k8s.io/apiserver/pkg/server/healthz"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/component-base/configz"
	"k8s.io/component-base/zpages/flagz"
	"k8s.io/component-base/zpages/statusz"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/features"
)

// KubeletAuth implements AuthInterface
type KubeletAuth struct {
	// authenticator identifies the user for requests to the Kubelet API
	authenticator.Request
	// KubeletRequestAttributesGetter builds authorization.Attributes for a request to the Kubelet API
	NodeRequestAttributesGetter
	// authorizer determines whether a given authorization.Attributes is allowed
	authorizer.Authorizer
}

// NewKubeletAuth returns a kubelet.AuthInterface composed of the given authenticator, attribute getter, and authorizer
func NewKubeletAuth(authenticator authenticator.Request, authorizerAttributeGetter NodeRequestAttributesGetter, authorizer authorizer.Authorizer) AuthInterface {
	return &KubeletAuth{authenticator, authorizerAttributeGetter, authorizer}
}

// NewNodeAuthorizerAttributesGetter creates a new authorizer.RequestAttributesGetter for the node.
func NewNodeAuthorizerAttributesGetter(nodeName types.NodeName) NodeRequestAttributesGetter {
	return nodeAuthorizerAttributesGetter{nodeName: nodeName}
}

type nodeAuthorizerAttributesGetter struct {
	nodeName types.NodeName
}

func isSubpath(subpath, path string) bool {
	path = strings.TrimSuffix(path, "/")
	return subpath == path || (strings.HasPrefix(subpath, path) && subpath[len(path)] == '/')
}

// GetRequestAttributes populates authorizer attributes for the requests to the kubelet API.
// Default attributes are: {apiVersion=v1,verb=<http verb from request>,resource=nodes,name=<node name>,subresource=proxy}
// More specific verb/resource is set for the following request patterns:
//
//	/stats/*		=> verb=<api verb from request>, resource=nodes, name=<node name>, subresource(s)=stats
//	/metrics/*		=> verb=<api verb from request>, resource=nodes, name=<node name>, subresource(s)=metrics
//	/logs/*			=> verb=<api verb from request>, resource=nodes, name=<node name>, subresource(s)=log
//	/checkpoint/*	=> verb=<api verb from request>, resource=nodes, name=<node name>, subresource(s)=checkpoint
//	/statusz 		=> verb=<api verb from request>, resource=nodes, name=<node name>, subresource(s)=statusz
//	/pods/*			=> verb=<api verb from request>, resource=nodes, name=<node name>, subresource(s)=pods,proxy
//	/runningPods/*	=> verb=<api verb from request>, resource=nodes, name=<node name>, subresource(s)=pods,proxy
//	/healthz/* 		=> verb=<api verb from request>, resource=nodes, name=<node name>, subresource(s)=healthz,proxy
//	/configz 		=> verb=<api verb from request>, resource=nodes, name=<node name>, subresource(s)=configz,proxy
//	/flagz 		    => verb=<api verb from request>, resource=nodes, name=<node name>, subresource(s)=configz,proxy
func (n nodeAuthorizerAttributesGetter) GetRequestAttributes(u user.Info, r *http.Request) []authorizer.Attributes {

	apiVerb := ""
	switch r.Method {
	case "POST":
		apiVerb = "create"
	case "GET":
		apiVerb = "get"
	case "PUT":
		apiVerb = "update"
	case "PATCH":
		apiVerb = "patch"
	case "DELETE":
		apiVerb = "delete"
	}

	requestPath := r.URL.Path

	var subresources []string
	if utilfeature.DefaultFeatureGate.Enabled(features.KubeletFineGrainedAuthz) {
		switch {
		case isSubpath(requestPath, podsPath):
			subresources = append(subresources, "pods")
		case isSubpath(requestPath, healthz.DefaultHealthzPath):
			subresources = append(subresources, "healthz")
		case isSubpath(requestPath, configz.DefaultConfigzPath),
			isSubpath(requestPath, flagz.DefaultFlagzPath):
			subresources = append(subresources, "configz")
		// We put runningpods last since it will allocate a new string on every
		// check since the handler path has a trailing slash.
		case isSubpath(requestPath, runningPodsPath):
			subresources = append(subresources, "pods")
		}
	}

	switch {
	case isSubpath(requestPath, statsPath):
		subresources = append(subresources, "stats")
	case isSubpath(requestPath, metricsPath):
		subresources = append(subresources, "metrics")
	case isSubpath(requestPath, logsPath):
		// "log" to match other log subresources (pods/log, etc)
		subresources = append(subresources, "log")
	case isSubpath(requestPath, checkpointPath):
		subresources = append(subresources, "checkpoint")
	case isSubpath(requestPath, statusz.DefaultStatuszPath):
		subresources = append(subresources, "statusz")
	default:
		subresources = append(subresources, "proxy")
	}

	var attrs []authorizer.Attributes
	for _, subresource := range subresources {
		attr := authorizer.AttributesRecord{
			User:            u,
			Verb:            apiVerb,
			Namespace:       "",
			APIGroup:        "",
			APIVersion:      "v1",
			Resource:        "nodes",
			Subresource:     subresource,
			Name:            string(n.nodeName),
			ResourceRequest: true,
			Path:            requestPath,
		}
		attrs = append(attrs, attr)
	}

	klog.V(5).InfoS("Node request attributes", "user", attrs[0].GetUser().GetName(), "verb", attrs[0].GetVerb(), "resource", attrs[0].GetResource(), "subresource(s)", subresources)

	return attrs
}
