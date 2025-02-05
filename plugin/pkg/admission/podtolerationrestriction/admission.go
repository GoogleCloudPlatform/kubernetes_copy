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

package podtolerationrestriction

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"k8s.io/klog/v2"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apiserver/pkg/admission"
	genericadmissioninitializer "k8s.io/apiserver/pkg/admission/initializer"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	corev1listers "k8s.io/client-go/listers/core/v1"
	api "k8s.io/kubernetes/pkg/apis/core"
	qoshelper "k8s.io/kubernetes/pkg/apis/core/helper/qos"
	k8s_api_v1 "k8s.io/kubernetes/pkg/apis/core/v1"
	"k8s.io/kubernetes/pkg/util/tolerations"
	pluginapi "k8s.io/kubernetes/plugin/pkg/admission/podtolerationrestriction/apis/podtolerationrestriction"
)

// PluginName is a string with the name of the plugin
const PluginName = "PodTolerationRestriction"

// Register registers a plugin
func Register(plugins *admission.Plugins) {
	plugins.Register(PluginName, func(config io.Reader) (admission.Interface, error) {
		pluginConfig, err := loadConfiguration(config)
		if err != nil {
			return nil, err
		}
		return NewPodTolerationsPlugin(pluginConfig), nil
	})
}

// The annotation keys for default and whitelist of tolerations
const (
	NSDefaultTolerations string = "scheduler.alpha.kubernetes.io/defaultTolerations"
	NSWLTolerations      string = "scheduler.alpha.kubernetes.io/tolerationsWhitelist"
)

var _ admission.MutationInterface = &Plugin{}
var _ admission.ValidationInterface = &Plugin{}
var _ = genericadmissioninitializer.WantsExternalKubeInformerFactory(&Plugin{})
var _ = genericadmissioninitializer.WantsExternalKubeClientSet(&Plugin{})

// Plugin contains the client used by the admission controller
type Plugin struct {
	*admission.Handler
	client          kubernetes.Interface
	namespaceLister corev1listers.NamespaceLister
	pluginConfig    *pluginapi.Configuration
}

// Admit checks the admission policy and triggers corresponding actions
func (p *Plugin) Admit(ctx context.Context, a admission.Attributes, o admission.ObjectInterfaces) error {
	if shouldIgnore(a) {
		return nil
	}

	if !p.WaitForReady() {
		return admission.NewForbidden(a, fmt.Errorf("not yet ready to handle request"))
	}

	pod := a.GetObject().(*api.Pod)
	var extraTolerations []api.Toleration
	if a.GetOperation() == admission.Create {
		ts, err := p.getNamespaceDefaultTolerations(a.GetNamespace())
		if err != nil {
			return err
		}

		// If the namespace has not specified its default tolerations,
		// fall back to cluster's default tolerations.
		if ts == nil {
			ts = p.pluginConfig.Default
		}

		extraTolerations = ts
	}

	if qoshelper.GetPodQOS(pod) != api.PodQOSBestEffort {
		extraTolerations = append(extraTolerations, api.Toleration{
			Key:      corev1.TaintNodeMemoryPressure,
			Operator: api.TolerationOpExists,
			Effect:   api.TaintEffectNoSchedule,
		})
	}
	// Final merge of tolerations irrespective of pod type.
	if len(extraTolerations) > 0 {
		mergedOldTolerations := tolerations.MergeTolerations(pod.Spec.Tolerations, nil)
		mergedNewTolerations := tolerations.MergeTolerations(pod.Spec.Tolerations, extraTolerations)
		if len(mergedOldTolerations) != len(pod.Spec.Tolerations) {
			// skip if no change
			if len(mergedNewTolerations) > len(mergedOldTolerations) {
				pod.Spec.Tolerations = append(pod.Spec.Tolerations, extraTolerations...)
			}
		} else {
			pod.Spec.Tolerations = mergedNewTolerations
		}
	}
	return p.Validate(ctx, a, o)
}

// Validate we can obtain a whitelist of tolerations
func (p *Plugin) Validate(ctx context.Context, a admission.Attributes, o admission.ObjectInterfaces) error {
	if shouldIgnore(a) {
		return nil
	}

	if !p.WaitForReady() {
		return admission.NewForbidden(a, fmt.Errorf("not yet ready to handle request"))
	}

	// whitelist verification.
	pod := a.GetObject().(*api.Pod)
	if len(pod.Spec.Tolerations) > 0 {
		whitelist, err := p.getNamespaceTolerationsWhitelist(a.GetNamespace())
		whitelistScope := "namespace"
		if err != nil {
			return err
		}

		// If the namespace has not specified its tolerations whitelist,
		// fall back to cluster's whitelist of tolerations.
		if whitelist == nil {
			whitelist = p.pluginConfig.Whitelist
			whitelistScope = "cluster"
		}

		if len(whitelist) > 0 {
			// check if the merged pod tolerations satisfy its namespace whitelist
			if !tolerations.VerifyAgainstWhitelist(pod.Spec.Tolerations, whitelist) {
				return fmt.Errorf("pod tolerations (possibly merged with namespace default tolerations) conflict with its %s whitelist", whitelistScope)
			}
		}
	}

	return nil
}

func shouldIgnore(a admission.Attributes) bool {
	resource := a.GetResource().GroupResource()
	if resource != api.Resource("pods") {
		return true
	}
	if a.GetSubresource() != "" {
		// only run the checks below on pods proper and not subresources
		return true
	}

	obj := a.GetObject()
	_, ok := obj.(*api.Pod)
	if !ok {
		klog.Errorf("expected pod but got %s", a.GetKind().Kind)
		return true
	}

	return false
}

// NewPodTolerationsPlugin initializes a Plugin
func NewPodTolerationsPlugin(pluginConfig *pluginapi.Configuration) *Plugin {
	return &Plugin{
		Handler:      admission.NewHandler(admission.Create, admission.Update),
		pluginConfig: pluginConfig,
	}
}

// SetExternalKubeClientSet sets th client
func (p *Plugin) SetExternalKubeClientSet(client kubernetes.Interface) {
	p.client = client
}

// SetExternalKubeInformerFactory initializes the Informer Factory
func (p *Plugin) SetExternalKubeInformerFactory(f informers.SharedInformerFactory) {
	namespaceInformer := f.Core().V1().Namespaces()
	p.namespaceLister = namespaceInformer.Lister()
	p.SetReadyFunc(namespaceInformer.Informer().HasSynced)

}

// ValidateInitialization checks the object is properly initialized
func (p *Plugin) ValidateInitialization() error {
	if p.namespaceLister == nil {
		return fmt.Errorf("missing namespaceLister")
	}
	if p.client == nil {
		return fmt.Errorf("missing client")
	}
	return nil
}

// in exceptional cases, this can result in two live calls, but once the cache catches up, that will stop.
func (p *Plugin) getNamespace(nsName string) (*corev1.Namespace, error) {
	namespace, err := p.namespaceLister.Get(nsName)
	if errors.IsNotFound(err) {
		// in case of latency in our caches, make a call direct to storage to verify that it truly exists or not
		namespace, err = p.client.CoreV1().Namespaces().Get(context.TODO(), nsName, metav1.GetOptions{})
		if err != nil {
			if errors.IsNotFound(err) {
				return nil, err
			}
			return nil, errors.NewInternalError(err)
		}
	} else if err != nil {
		return nil, errors.NewInternalError(err)
	}

	return namespace, nil
}

func (p *Plugin) getNamespaceDefaultTolerations(nsName string) ([]api.Toleration, error) {
	ns, err := p.getNamespace(nsName)
	if err != nil {
		return nil, err
	}
	return extractNSTolerations(ns, NSDefaultTolerations)
}

func (p *Plugin) getNamespaceTolerationsWhitelist(nsName string) ([]api.Toleration, error) {
	ns, err := p.getNamespace(nsName)
	if err != nil {
		return nil, err
	}
	return extractNSTolerations(ns, NSWLTolerations)
}

// extractNSTolerations extracts default or whitelist of tolerations from
// following namespace annotations keys: "scheduler.alpha.kubernetes.io/defaultTolerations"
// and "scheduler.alpha.kubernetes.io/tolerationsWhitelist". If these keys are
// unset (nil), extractNSTolerations returns nil. If the value to these
// keys are set to empty, an empty toleration is returned, otherwise
// configured tolerations are returned.
func extractNSTolerations(ns *corev1.Namespace, key string) ([]api.Toleration, error) {
	// if a namespace does not have any annotations
	if len(ns.Annotations) == 0 {
		return nil, nil
	}

	// if NSWLTolerations or NSDefaultTolerations does not exist
	if _, ok := ns.Annotations[key]; !ok {
		return nil, nil
	}

	// if value is set to empty
	if len(ns.Annotations[key]) == 0 {
		return []api.Toleration{}, nil
	}

	var v1Tolerations []corev1.Toleration
	err := json.Unmarshal([]byte(ns.Annotations[key]), &v1Tolerations)
	if err != nil {
		return nil, err
	}

	ts := make([]api.Toleration, len(v1Tolerations))
	for i := range v1Tolerations {
		if err := k8s_api_v1.Convert_v1_Toleration_To_core_Toleration(&v1Tolerations[i], &ts[i], nil); err != nil {
			return nil, err
		}
	}

	return ts, nil
}
