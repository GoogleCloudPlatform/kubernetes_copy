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

package configuration

import (
	"fmt"
	"sort"
	"sync"

	v1 "k8s.io/api/admissionregistration/v1"
	"k8s.io/apimachinery/pkg/labels"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apiserver/pkg/admission/plugin/webhook"
	"k8s.io/apiserver/pkg/admission/plugin/webhook/generic"
	"k8s.io/client-go/informers"
	admissionregistrationlisters "k8s.io/client-go/listers/admissionregistration/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/cache/synctrack"
	"k8s.io/klog/v2"
)

// Type for test injection.
type mutatingWebhookAccessorCreator func(uid string, configurationName string, h *v1.MutatingWebhook) webhook.WebhookAccessor

// mutatingWebhookConfigurationManager collects the mutating webhook objects so that they can be called.
type mutatingWebhookConfigurationManager struct {
	lister              admissionregistrationlisters.MutatingWebhookConfigurationLister
	hasSynced           func() bool
	lazy                synctrack.Lazy[[]webhook.WebhookAccessor]
	configurationsCache sync.Map
	// createMutatingWebhookAccessor is used to instantiate webhook accessors.
	// This function is defined as field instead of a struct method to allow injection
	// during tests
	createMutatingWebhookAccessor mutatingWebhookAccessorCreator
}

var _ generic.Source = &mutatingWebhookConfigurationManager{}

func NewMutatingWebhookConfigurationManager(f informers.SharedInformerFactory) generic.Source {
	informer := f.Admissionregistration().V1().MutatingWebhookConfigurations()
	manager := &mutatingWebhookConfigurationManager{
		lister:                        informer.Lister(),
		createMutatingWebhookAccessor: webhook.NewMutatingWebhookAccessor,
	}
	manager.lazy.Evaluate = manager.getConfiguration

	handle, _ := informer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(_ interface{}) { manager.lazy.Notify() },
		UpdateFunc: func(old, new interface{}) {
			obj := new.(*v1.MutatingWebhookConfiguration)
			manager.configurationsCache.Delete(obj.GetName())
			manager.lazy.Notify()
		},
		DeleteFunc: func(obj interface{}) {
			vwc, ok := obj.(*v1.MutatingWebhookConfiguration)
			if !ok {
				tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
				if !ok {
					klog.V(2).Infof("Couldn't get object from tombstone %#v", obj)
					return
				}
				vwc, ok = tombstone.Obj.(*v1.MutatingWebhookConfiguration)
				if !ok {
					klog.V(2).Infof("Tombstone contained object that is not expected %#v", obj)
					return
				}
			}
			manager.configurationsCache.Delete(vwc.Name)
			manager.lazy.Notify()
		},
	})
	manager.hasSynced = handle.HasSynced

	return manager
}

// Webhooks returns the merged MutatingWebhookConfiguration.
func (m *mutatingWebhookConfigurationManager) Webhooks() []webhook.WebhookAccessor {
	out, err := m.lazy.Get()
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("error getting webhook configuration: %v", err))
	}
	return out
}

// HasSynced returns true if the initial set of mutating webhook configurations
// has been loaded.
func (m *mutatingWebhookConfigurationManager) HasSynced() bool { return m.hasSynced() }

func (m *mutatingWebhookConfigurationManager) getConfiguration() ([]webhook.WebhookAccessor, error) {
	configurations, err := m.lister.List(labels.Everything())
	if err != nil {
		return []webhook.WebhookAccessor{}, err
	}
	return m.getMutatingWebhookConfigurations(configurations), nil
}

// getMutatingWebhookConfigurations returns the webhook accessors for a given list of
// mutating webhook configurations.
//
// This function will, first, try to load the webhook accessors from the cache and avoid
// recreating them, which can be expensive (requiring CEL expression recompilation).
func (m *mutatingWebhookConfigurationManager) getMutatingWebhookConfigurations(configurations []*v1.MutatingWebhookConfiguration) []webhook.WebhookAccessor {
	// The internal order of webhooks for each configuration is provided by the user
	// but configurations themselves can be in any order. As we are going to run these
	// webhooks in serial, they are sorted here to have a deterministic order.
	sort.SliceStable(configurations, MutatingWebhookConfigurationSorter(configurations).ByName)
	size := 0
	for _, cfg := range configurations {
		size += len(cfg.Webhooks)
	}
	accessors := make([]webhook.WebhookAccessor, 0, size)

	for _, c := range configurations {
		cachedConfigurationAccessors, ok := m.configurationsCache.Load(c.Name)
		if ok {
			// Pick an already cached webhookAccessor
			accessors = append(accessors, cachedConfigurationAccessors.([]webhook.WebhookAccessor)...)
			continue
		}

		// webhook names are not validated for uniqueness, so we check for duplicates and
		// add a int suffix to distinguish between them
		names := map[string]int{}
		configurationAccessors := make([]webhook.WebhookAccessor, 0, len(c.Webhooks))
		for i := range c.Webhooks {
			n := c.Webhooks[i].Name
			uid := fmt.Sprintf("%s/%s/%d", c.Name, n, names[n])
			names[n]++
			configurationAccessor := m.createMutatingWebhookAccessor(uid, c.Name, &c.Webhooks[i])
			configurationAccessors = append(configurationAccessors, configurationAccessor)
		}
		accessors = append(accessors, configurationAccessors...)
		m.configurationsCache.Store(c.Name, configurationAccessors)
	}
	return accessors
}

type MutatingWebhookConfigurationSorter []*v1.MutatingWebhookConfiguration

func (a MutatingWebhookConfigurationSorter) ByName(i, j int) bool {
	return a[i].Name < a[j].Name
}
