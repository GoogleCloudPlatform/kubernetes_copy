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

package extendedresourcetoleration

import (
	"context"
	"fmt"
	"io"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apiserver/pkg/admission"
	"k8s.io/kubernetes/pkg/apis/core"
	"k8s.io/kubernetes/pkg/apis/core/helper"
)

// PluginName indicates name of admission plugin.
const PluginName = "ExtendedResourceToleration"

// Register is called by the apiserver to register the plugin factory.
func Register(plugins *admission.Plugins) {
	plugins.Register(PluginName, func(config io.Reader) (admission.Interface, error) {
		return newExtendedResourceToleration(), nil
	})
}

// newExtendedResourceToleration creates a new instance of the ExtendedResourceToleration admission controller.
func newExtendedResourceToleration() *plugin {
	return &plugin{
		Handler: admission.NewHandler(admission.Create, admission.Update),
	}
}

// Make sure we are implementing the interface.
var _ admission.MutationInterface = &plugin{}

type plugin struct {
	*admission.Handler
}

// Admit updates the toleration of a pod based on the resources requested by it.
// If an extended resource of name "example.com/device" is requested, it adds
// a toleration with key "example.com/device", operator "Exists" and effect "NoSchedule".
// The rationale for this is described in:
// https://github.com/kubernetes/kubernetes/issues/55080
func (p *plugin) Admit(ctx context.Context, attributes admission.Attributes, o admission.ObjectInterfaces) error {
	// Ignore all calls to subresources or resources other than pods.
	if len(attributes.GetSubresource()) != 0 || attributes.GetResource().GroupResource() != core.Resource("pods") {
		return nil
	}

	pod, ok := attributes.GetObject().(*core.Pod)
	if !ok {
		return errors.NewBadRequest(fmt.Sprintf("expected *core.Pod but got %T", attributes.GetObject()))
	}

	resources := sets.New[string]()
	for _, container := range pod.Spec.Containers {
		for resourceName := range container.Resources.Requests {
			if helper.IsExtendedResourceName(resourceName) {
				resources.Insert(string(resourceName))
			}
		}
	}
	for _, container := range pod.Spec.InitContainers {
		for resourceName := range container.Resources.Requests {
			if helper.IsExtendedResourceName(resourceName) {
				resources.Insert(string(resourceName))
			}
		}
	}

	// Doing .List() so that we get a stable sorted list.
	// This allows us to test adding tolerations for multiple extended resources.
	for _, resource := range sets.List(resources) {
		helper.AddOrUpdateTolerationInPod(pod, &core.Toleration{
			Key:      resource,
			Operator: core.TolerationOpExists,
			Effect:   core.TaintEffectNoSchedule,
		})
	}

	return nil
}
