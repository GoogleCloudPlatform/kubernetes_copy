/*
Copyright 2016 The Kubernetes Authors.

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

package podnodeselector

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apiserver/pkg/admission"
	genericadmissioninitializer "k8s.io/apiserver/pkg/admission/initializer"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	api "k8s.io/kubernetes/pkg/apis/core"
)

// TestPodAdmission verifies various scenarios involving pod/namespace/global node label selectors
func TestPodAdmission(t *testing.T) {
	namespace := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "testNamespace",
			Namespace: "",
		},
	}

	mockClient := fake.NewSimpleClientset(namespace)
	handler, informerFactory, err := newHandlerForTest(mockClient)
	if err != nil {
		t.Errorf("unexpected error initializing handler: %v", err)
	}
	stopCh := make(chan struct{})
	defer close(stopCh)
	informerFactory.Start(stopCh)

	pod := &api.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "testPod", Namespace: "testNamespace"},
	}

	tests := []struct {
		defaultNodeSelector             string
		namespaceNodeSelector           string
		whitelist                       string
		podNodeSelector                 map[string]string
		mergedNodeSelector              labels.Set
		ignoreTestNamespaceNodeSelector bool
		admit                           bool
		testName                        string
	}{
		{
			defaultNodeSelector:             "",
			podNodeSelector:                 map[string]string{},
			mergedNodeSelector:              labels.Set{},
			ignoreTestNamespaceNodeSelector: true,
			admit:                           true,
			testName:                        "No node selectors",
		},
		{
			defaultNodeSelector:             "infra = false",
			podNodeSelector:                 map[string]string{},
			mergedNodeSelector:              labels.Set{"infra": "false"},
			ignoreTestNamespaceNodeSelector: true,
			admit:                           true,
			testName:                        "Default node selector and no conflicts",
		},
		{
			defaultNodeSelector:   "",
			namespaceNodeSelector: " infra = false ",
			podNodeSelector:       map[string]string{},
			mergedNodeSelector:    labels.Set{"infra": "false"},
			admit:                 true,
			testName:              "TestNamespace node selector with whitespaces and no conflicts",
		},
		{
			defaultNodeSelector:   "infra = false",
			namespaceNodeSelector: "infra=true",
			podNodeSelector:       map[string]string{},
			mergedNodeSelector:    labels.Set{"infra": "true"},
			admit:                 true,
			testName:              "Default and namespace node selector, no conflicts",
		},
		{
			defaultNodeSelector:   "infra = false",
			namespaceNodeSelector: "",
			podNodeSelector:       map[string]string{},
			mergedNodeSelector:    labels.Set{},
			admit:                 true,
			testName:              "Empty namespace node selector and no conflicts",
		},
		{
			defaultNodeSelector:   "infra = false",
			namespaceNodeSelector: "infra=true",
			podNodeSelector:       map[string]string{"env": "test"},
			mergedNodeSelector:    labels.Set{"infra": "true", "env": "test"},
			admit:                 true,
			testName:              "TestNamespace and pod node selector, no conflicts",
		},
		{
			defaultNodeSelector:   "env = test",
			namespaceNodeSelector: "infra=true",
			podNodeSelector:       map[string]string{"infra": "false"},
			admit:                 false,
			testName:              "Conflicting pod and namespace node selector, one label",
		},
		{
			defaultNodeSelector:   "env=dev",
			namespaceNodeSelector: "infra=false, env = test",
			podNodeSelector:       map[string]string{"env": "dev", "color": "blue"},
			admit:                 false,
			testName:              "Conflicting pod and namespace node selector, multiple labels",
		},
		{
			defaultNodeSelector:   "env=dev",
			namespaceNodeSelector: "infra=false, env = dev",
			whitelist:             "env=dev, infra=false, color=blue",
			podNodeSelector:       map[string]string{"env": "dev", "color": "blue"},
			mergedNodeSelector:    labels.Set{"infra": "false", "env": "dev", "color": "blue"},
			admit:                 true,
			testName:              "Merged pod node selectors satisfy the whitelist",
		},
		{
			defaultNodeSelector:   "env=dev",
			namespaceNodeSelector: "infra=false, env = dev",
			whitelist:             "env=dev, infra=true, color=blue",
			podNodeSelector:       map[string]string{"env": "dev", "color": "blue"},
			admit:                 false,
			testName:              "Merged pod node selectors conflict with the whitelist",
		},
		{
			defaultNodeSelector:             "env=dev",
			ignoreTestNamespaceNodeSelector: true,
			whitelist:                       "env=prd",
			podNodeSelector:                 map[string]string{},
			admit:                           false,
			testName:                        "Default node selector conflict with the whitelist",
		},
	}
	for _, test := range tests {
		if !test.ignoreTestNamespaceNodeSelector {
			namespace.ObjectMeta.Annotations = map[string]string{"scheduler.alpha.kubernetes.io/node-selector": test.namespaceNodeSelector}
			informerFactory.Core().V1().Namespaces().Informer().GetStore().Update(namespace)
		}
		handler.clusterNodeSelectors = make(map[string]string)
		handler.clusterNodeSelectors["clusterDefaultNodeSelector"] = test.defaultNodeSelector
		handler.clusterNodeSelectors[namespace.Name] = test.whitelist
		pod.Spec = api.PodSpec{NodeSelector: test.podNodeSelector}

		err := handler.Admit(context.TODO(), admission.NewAttributesRecord(pod, nil, api.Kind("Pod").WithVersion("version"), "testNamespace", namespace.ObjectMeta.Name, api.Resource("pods").WithVersion("version"), "", admission.Create, &metav1.CreateOptions{}, false, nil), nil)
		if test.admit && err != nil {
			t.Errorf("Test: %s, expected no error but got: %s", test.testName, err)
		} else if !test.admit && err == nil {
			t.Errorf("Test: %s, expected an error", test.testName)
		}
		if test.admit && !labels.Equals(test.mergedNodeSelector, labels.Set(pod.Spec.NodeSelector)) {
			t.Errorf("Test: %s, expected: %s but got: %s", test.testName, test.mergedNodeSelector, pod.Spec.NodeSelector)
		}
		err = handler.Validate(context.TODO(), admission.NewAttributesRecord(pod, nil, api.Kind("Pod").WithVersion("version"), "testNamespace", namespace.ObjectMeta.Name, api.Resource("pods").WithVersion("version"), "", admission.Create, &metav1.CreateOptions{}, false, nil), nil)
		if test.admit && err != nil {
			t.Errorf("Test: %s, expected no error but got: %s", test.testName, err)
		} else if !test.admit && err == nil {
			t.Errorf("Test: %s, expected an error", test.testName)
		}
	}
}

func TestHandles(t *testing.T) {
	for op, shouldHandle := range map[admission.Operation]bool{
		admission.Create:  true,
		admission.Update:  false,
		admission.Connect: false,
		admission.Delete:  false,
	} {
		nodeEnvironment := NewPodNodeSelector(nil)
		if e, a := shouldHandle, nodeEnvironment.Handles(op); e != a {
			t.Errorf("%v: shouldHandle=%t, handles=%t", op, e, a)
		}
	}
}

// newHandlerForTest returns the admission controller configured for testing.
func newHandlerForTest(c kubernetes.Interface) (*Plugin, informers.SharedInformerFactory, error) {
	f := informers.NewSharedInformerFactory(c, 5*time.Minute)
	handler := NewPodNodeSelector(nil)
	pluginInitializer := genericadmissioninitializer.New(c, nil, f, nil, nil, nil, nil)
	pluginInitializer.Initialize(handler)
	err := admission.ValidateInitialization(handler)
	return handler, f, err
}
