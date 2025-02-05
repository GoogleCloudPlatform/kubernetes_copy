/*
Copyright 2019 The Kubernetes Authors.

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

// Package runtimeclass contains an admission controller for modifying and validating new Pods to
// take RuntimeClass into account. For RuntimeClass definitions which describe an overhead associated
// with running a pod, this admission controller will set the pod.Spec.Overhead field accordingly. This
// field should only be set through this controller, so validation will be carried out to ensure the pod's
// value matches what is defined in the coresponding RuntimeClass.
package runtimeclass

import (
	"context"
	"fmt"
	"io"

	nodev1 "k8s.io/api/node/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apiserver/pkg/admission"
	genericadmissioninitailizer "k8s.io/apiserver/pkg/admission/initializer"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	nodev1client "k8s.io/client-go/kubernetes/typed/node/v1"
	nodev1listers "k8s.io/client-go/listers/node/v1"
	api "k8s.io/kubernetes/pkg/apis/core"
	node "k8s.io/kubernetes/pkg/apis/node"
	apinodev1 "k8s.io/kubernetes/pkg/apis/node/v1"
	"k8s.io/kubernetes/pkg/util/tolerations"
)

// PluginName indicates name of admission plugin.
const PluginName = "RuntimeClass"

// Register registers a plugin
func Register(plugins *admission.Plugins) {
	plugins.Register(PluginName, func(config io.Reader) (admission.Interface, error) {
		return NewRuntimeClass(), nil
	})
}

// RuntimeClass is an implementation of admission.Interface.
// It looks at all new pods and sets pod.Spec.Overhead if a RuntimeClass is specified which
// defines an Overhead. If pod.Spec.Overhead is set but a RuntimeClass with matching overhead is
// not specified, the pod is rejected.
type RuntimeClass struct {
	*admission.Handler
	runtimeClassLister nodev1listers.RuntimeClassLister
	runtimeClassClient nodev1client.RuntimeClassInterface
}

var _ admission.MutationInterface = &RuntimeClass{}
var _ admission.ValidationInterface = &RuntimeClass{}

var _ genericadmissioninitailizer.WantsExternalKubeInformerFactory = &RuntimeClass{}
var _ genericadmissioninitailizer.WantsExternalKubeClientSet = &RuntimeClass{}

// SetExternalKubeClientSet sets the client for the plugin
func (r *RuntimeClass) SetExternalKubeClientSet(client kubernetes.Interface) {
	r.runtimeClassClient = client.NodeV1().RuntimeClasses()
}

// SetExternalKubeInformerFactory implements the WantsExternalKubeInformerFactory interface.
func (r *RuntimeClass) SetExternalKubeInformerFactory(f informers.SharedInformerFactory) {
	runtimeClassInformer := f.Node().V1().RuntimeClasses()
	r.SetReadyFunc(runtimeClassInformer.Informer().HasSynced)
	r.runtimeClassLister = runtimeClassInformer.Lister()
}

// ValidateInitialization implements the WantsExternalKubeInformerFactory interface.
func (r *RuntimeClass) ValidateInitialization() error {
	if r.runtimeClassLister == nil {
		return fmt.Errorf("missing RuntimeClass lister")
	}
	if r.runtimeClassClient == nil {
		return fmt.Errorf("missing RuntimeClass client")
	}
	return nil
}

// Admit makes an admission decision based on the request attributes
func (r *RuntimeClass) Admit(ctx context.Context, attributes admission.Attributes, o admission.ObjectInterfaces) error {
	// Ignore all calls to subresources or resources other than pods.
	if shouldIgnore(attributes) {
		return nil
	}

	pod, runtimeClass, err := r.prepareObjects(ctx, attributes)
	if err != nil {
		return err
	}
	if err := setOverhead(attributes, pod, runtimeClass); err != nil {
		return err
	}

	if err := setScheduling(attributes, pod, runtimeClass); err != nil {
		return err
	}

	return nil
}

// Validate makes sure that pod adhere's to RuntimeClass's definition
func (r *RuntimeClass) Validate(ctx context.Context, attributes admission.Attributes, o admission.ObjectInterfaces) error {
	// Ignore all calls to subresources or resources other than pods.
	if shouldIgnore(attributes) {
		return nil
	}

	pod, runtimeClass, err := r.prepareObjects(ctx, attributes)
	if err != nil {
		return err
	}
	if err := validateOverhead(attributes, pod, runtimeClass); err != nil {
		return err
	}

	return nil
}

// NewRuntimeClass creates a new RuntimeClass admission control handler
func NewRuntimeClass() *RuntimeClass {
	return &RuntimeClass{
		Handler: admission.NewHandler(admission.Create),
	}
}

// prepareObjects returns pod and runtimeClass types from the given admission attributes
func (r *RuntimeClass) prepareObjects(ctx context.Context, attributes admission.Attributes) (pod *api.Pod, runtimeClass *nodev1.RuntimeClass, err error) {
	pod, ok := attributes.GetObject().(*api.Pod)
	if !ok {
		return nil, nil, apierrors.NewBadRequest("Resource was marked with kind Pod but was unable to be converted")
	}

	if pod.Spec.RuntimeClassName == nil {
		return pod, nil, nil
	}

	// get RuntimeClass object
	runtimeClass, err = r.runtimeClassLister.Get(*pod.Spec.RuntimeClassName)
	if apierrors.IsNotFound(err) {
		// if not found, our informer cache could be lagging, do a live lookup
		runtimeClass, err = r.runtimeClassClient.Get(ctx, *pod.Spec.RuntimeClassName, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return pod, nil, admission.NewForbidden(attributes, fmt.Errorf("pod rejected: RuntimeClass %q not found", *pod.Spec.RuntimeClassName))
		}
	}

	// return the pod and runtimeClass.
	return pod, runtimeClass, err
}

func setOverhead(a admission.Attributes, pod *api.Pod, runtimeClass *nodev1.RuntimeClass) (err error) {
	if runtimeClass == nil || runtimeClass.Overhead == nil {
		return nil
	}

	// convert to internal type and assign to pod's Overhead
	nodeOverhead := &node.Overhead{}
	if err = apinodev1.Convert_v1_Overhead_To_node_Overhead(runtimeClass.Overhead, nodeOverhead, nil); err != nil {
		return err
	}

	// reject pod if Overhead is already set that differs from what is defined in RuntimeClass
	if len(pod.Spec.Overhead) > 0 && !apiequality.Semantic.DeepEqual(nodeOverhead.PodFixed, pod.Spec.Overhead) {
		return admission.NewForbidden(a, fmt.Errorf("pod rejected: Pod's Overhead doesn't match RuntimeClass's defined Overhead"))
	}

	pod.Spec.Overhead = nodeOverhead.PodFixed

	return nil
}

func setScheduling(a admission.Attributes, pod *api.Pod, runtimeClass *nodev1.RuntimeClass) (err error) {
	if runtimeClass == nil || runtimeClass.Scheduling == nil {
		return nil
	}

	// convert to internal type and assign to pod's Scheduling
	nodeScheduling := &node.Scheduling{}
	if err = apinodev1.Convert_v1_Scheduling_To_node_Scheduling(runtimeClass.Scheduling, nodeScheduling, nil); err != nil {
		return err
	}

	runtimeNodeSelector := nodeScheduling.NodeSelector
	newNodeSelector := pod.Spec.NodeSelector
	if newNodeSelector == nil {
		newNodeSelector = runtimeNodeSelector
	} else {
		for key, runtimeClassValue := range runtimeNodeSelector {
			if podValue, ok := newNodeSelector[key]; ok && podValue != runtimeClassValue {
				return admission.NewForbidden(a, fmt.Errorf("conflict: runtimeClass.scheduling.nodeSelector[%s] = %s; pod.spec.nodeSelector[%s] = %s", key, runtimeClassValue, key, podValue))
			}
			newNodeSelector[key] = runtimeClassValue
		}
	}

	newTolerations := tolerations.MergeTolerations(pod.Spec.Tolerations, nodeScheduling.Tolerations)

	pod.Spec.NodeSelector = newNodeSelector
	pod.Spec.Tolerations = newTolerations

	return nil
}

func validateOverhead(a admission.Attributes, pod *api.Pod, runtimeClass *nodev1.RuntimeClass) (err error) {
	if len(pod.Spec.Overhead) == 0 {
		return nil
	}
	if runtimeClass != nil && runtimeClass.Overhead != nil {
		// If the Overhead set doesn't match what is provided in the RuntimeClass definition, reject the pod
		nodeOverhead := &node.Overhead{}
		if err := apinodev1.Convert_v1_Overhead_To_node_Overhead(runtimeClass.Overhead, nodeOverhead, nil); err != nil {
			return err
		}
		if !apiequality.Semantic.DeepEqual(nodeOverhead.PodFixed, pod.Spec.Overhead) {
			return admission.NewForbidden(a, fmt.Errorf("pod rejected: Pod's Overhead doesn't match RuntimeClass's defined Overhead"))
		}
	} else {
		// If RuntimeClass with Overhead is not defined but an Overhead is set for pod, reject the pod
		return admission.NewForbidden(a, fmt.Errorf("pod rejected: Pod Overhead set without corresponding RuntimeClass defined Overhead"))
	}

	return nil
}

func shouldIgnore(attributes admission.Attributes) bool {
	// Ignore all calls to subresources or resources other than pods.
	if len(attributes.GetSubresource()) != 0 || attributes.GetResource().GroupResource() != api.Resource("pods") {
		return true
	}

	return false
}
