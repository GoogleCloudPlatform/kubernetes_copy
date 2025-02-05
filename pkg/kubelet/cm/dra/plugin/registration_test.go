/*
Copyright 2023 The Kubernetes Authors.

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

package plugin

import (
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	v1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	cgotesting "k8s.io/client-go/testing"
	drapbv1alpha4 "k8s.io/kubelet/pkg/apis/dra/v1alpha4"
	drapb "k8s.io/kubelet/pkg/apis/dra/v1beta1"
	"k8s.io/kubernetes/test/utils/ktesting"
)

const (
	nodeName  = "worker"
	pluginA   = "pluginA"
	endpointA = "endpointA"
	pluginB   = "pluginB"
	endpointB = "endpointB"
)

func getFakeNode() (*v1.Node, error) {
	return &v1.Node{ObjectMeta: metav1.ObjectMeta{Name: nodeName}}, nil
}

func TestRegistrationHandler(t *testing.T) {
	slice := &resourceapi.ResourceSlice{
		ObjectMeta: metav1.ObjectMeta{Name: "test-slice"},
		Spec: resourceapi.ResourceSliceSpec{
			NodeName: nodeName,
		},
	}

	for _, test := range []struct {
		description       string
		pluginName        string
		endpoint          string
		withClient        bool
		supportedServices []string
		shouldError       bool
		chosenService     string
	}{
		{
			description: "no-services-provided",
			pluginName:  pluginB,
			endpoint:    endpointB,
			shouldError: true,
		},
		{
			description:       "current-service",
			pluginName:        pluginB,
			endpoint:          endpointB,
			supportedServices: []string{drapb.DRAPluginService},
			chosenService:     drapb.DRAPluginService,
		},
		{
			description:       "two-services",
			pluginName:        pluginB,
			endpoint:          endpointB,
			supportedServices: []string{drapbv1alpha4.NodeService, drapb.DRAPluginService},
			chosenService:     drapb.DRAPluginService,
		},
		{
			description:       "old-service",
			pluginName:        pluginB,
			endpoint:          endpointB,
			supportedServices: []string{drapbv1alpha4.NodeService},
			chosenService:     drapbv1alpha4.NodeService,
		},
		{
			// Legacy behavior.
			description:       "version",
			pluginName:        pluginB,
			endpoint:          endpointB,
			supportedServices: []string{"1.0.0"},
			chosenService:     drapbv1alpha4.NodeService,
		},
		{
			description:       "replace",
			pluginName:        pluginA,
			endpoint:          endpointB,
			supportedServices: []string{drapb.DRAPluginService},
			chosenService:     drapb.DRAPluginService,
		},
		{
			description:       "manage-resource-slices",
			withClient:        true,
			pluginName:        pluginB,
			endpoint:          endpointB,
			supportedServices: []string{drapb.DRAPluginService},
			chosenService:     drapb.DRAPluginService,
		},
	} {
		t.Run(test.description, func(t *testing.T) {
			tCtx := ktesting.Init(t)

			// Stand-alone kubelet has no connection to an
			// apiserver, so faking one is optional.
			var client kubernetes.Interface
			if test.withClient {
				expectedSliceFields := fields.Set{"spec.nodeName": nodeName}
				fakeClient := fake.NewClientset(slice)
				fakeClient.AddReactor("delete-collection", "resourceslices", func(action cgotesting.Action) (bool, runtime.Object, error) {
					deleteAction := action.(cgotesting.DeleteCollectionAction)
					restrictions := deleteAction.GetListRestrictions()
					fieldsSelector := fields.SelectorFromSet(expectedSliceFields)
					// The order of field requirements is random because it comes
					// from a map. We need to sort.
					normalize := func(selector string) string {
						requirements := strings.Split(selector, ",")
						sort.Strings(requirements)
						return strings.Join(requirements, ",")
					}
					assert.Equal(t, "", restrictions.Labels.String(), "label selector in DeleteCollection")
					assert.Equal(t, normalize(fieldsSelector.String()), normalize(restrictions.Fields.String()), "field selector in DeleteCollection")

					// There's only one object that could get matched, so delete it.
					// Delete doesn't return an error if already deleted, which is what
					// we need here (no error when nothing to delete).
					err := fakeClient.Tracker().Delete(resourceapi.SchemeGroupVersion.WithResource("resourceslices"), "", slice.Name)

					// Set expected slice fields for the next call of this reactor.
					// The reactor will be called next time when resourceslices object is deleted
					// by the kubelet after plugin deregistration.
					expectedSliceFields = fields.Set{"spec.nodeName": nodeName, "spec.driver": test.pluginName}

					return true, nil, err
				})
				client = fakeClient
			}

			// The handler wipes all slices at startup.
			handler := NewRegistrationHandler(client, getFakeNode)
			requireNoSlices := func() {
				t.Helper()
				if client == nil {
					return
				}
				require.EventuallyWithT(t, func(t *assert.CollectT) {
					slices, err := client.ResourceV1beta1().ResourceSlices().List(tCtx, metav1.ListOptions{})
					if !assert.NoError(t, err, "list slices") {
						return
					}
					assert.Empty(t, slices.Items, "slices")
				}, time.Minute, time.Second)
			}
			requireNoSlices()

			// Simulate one existing plugin A.
			err := handler.RegisterPlugin(pluginA, endpointA, []string{drapb.DRAPluginService}, nil)
			require.NoError(t, err)

			err = handler.ValidatePlugin(test.pluginName, test.endpoint, test.supportedServices)
			if test.shouldError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			if err != nil {
				return
			}
			if test.pluginName != pluginA {
				require.Nil(t, draPlugins.get(test.pluginName), "not registered yet")
			}

			// Add plugin for the first time.
			err = handler.RegisterPlugin(test.pluginName, test.endpoint, test.supportedServices, nil)
			if test.shouldError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			plugin := draPlugins.get(test.pluginName)
			assert.NotNil(t, plugin, "plugin should be registered")
			t.Cleanup(func() {
				if client != nil {
					// Create the slice as if the plugin had done that while it runs.
					_, err := client.ResourceV1beta1().ResourceSlices().Create(tCtx, slice, metav1.CreateOptions{})
					assert.NoError(t, err, "recreate slice")
				}

				handler.DeRegisterPlugin(test.pluginName)
				// Nop.
				handler.DeRegisterPlugin(test.pluginName)

				requireNoSlices()
			})
			assert.Equal(t, test.endpoint, plugin.endpoint, "plugin endpoint")
			assert.Equal(t, test.chosenService, plugin.chosenService, "chosen service")
		})
	}
}
