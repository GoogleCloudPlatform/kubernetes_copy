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

package namespace_test

import (
	"reflect"
	"testing"

	registrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apiserver/pkg/admission"
	"k8s.io/apiserver/pkg/admission/plugin/webhook"
	"k8s.io/apiserver/pkg/admission/plugin/webhook/predicates/namespace"
)

type fakeNamespaceLister struct {
	namespaces map[string]*corev1.Namespace
}

func (f fakeNamespaceLister) List(selector labels.Selector) (ret []*corev1.Namespace, err error) {
	return nil, nil
}
func (f fakeNamespaceLister) Get(name string) (*corev1.Namespace, error) {
	ns, ok := f.namespaces[name]
	if ok {
		return ns, nil
	}
	return nil, errors.NewNotFound(corev1.Resource("namespaces"), name)
}

func TestGetNamespaceLabels(t *testing.T) {
	namespace1Labels := map[string]string{
		"runlevel": "1",
	}
	namespace1 := corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "1",
			Labels: namespace1Labels,
		},
	}
	namespace2Labels := map[string]string{
		"runlevel": "2",
	}
	namespace2 := corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "2",
			Labels: namespace2Labels,
		},
	}
	namespaceLister := fakeNamespaceLister{map[string]*corev1.Namespace{
		"1": &namespace1,
	},
	}

	tests := []struct {
		name           string
		attr           admission.Attributes
		expectedLabels map[string]string
	}{
		{
			name:           "request is for creating namespace, the labels should be from the object itself",
			attr:           admission.NewAttributesRecord(&namespace2, nil, schema.GroupVersionKind{}, "", namespace2.Name, schema.GroupVersionResource{Resource: "namespaces"}, "", admission.Create, &metav1.CreateOptions{}, false, nil),
			expectedLabels: namespace2Labels,
		},
		{
			name:           "request is for updating namespace, the labels should be from the new object",
			attr:           admission.NewAttributesRecord(&namespace2, nil, schema.GroupVersionKind{}, namespace2.Name, namespace2.Name, schema.GroupVersionResource{Resource: "namespaces"}, "", admission.Update, &metav1.UpdateOptions{}, false, nil),
			expectedLabels: namespace2Labels,
		},
		{
			name:           "request is for deleting namespace, the labels should be from the cache",
			attr:           admission.NewAttributesRecord(&namespace2, nil, schema.GroupVersionKind{}, namespace1.Name, namespace1.Name, schema.GroupVersionResource{Resource: "namespaces"}, "", admission.Delete, &metav1.DeleteOptions{}, false, nil),
			expectedLabels: namespace1Labels,
		},
		{
			name:           "request is for namespace/finalizer",
			attr:           admission.NewAttributesRecord(nil, nil, schema.GroupVersionKind{}, namespace1.Name, "mock-name", schema.GroupVersionResource{Resource: "namespaces"}, "finalizers", admission.Create, &metav1.CreateOptions{}, false, nil),
			expectedLabels: namespace1Labels,
		},
		{
			name:           "request is for pod",
			attr:           admission.NewAttributesRecord(nil, nil, schema.GroupVersionKind{}, namespace1.Name, "mock-name", schema.GroupVersionResource{Resource: "pods"}, "", admission.Create, &metav1.CreateOptions{}, false, nil),
			expectedLabels: namespace1Labels,
		},
	}
	matcher := namespace.Matcher{
		NamespaceProvider: &namespace.Provider{
			NamespaceLister: namespaceLister,
		},
	}
	for _, tt := range tests {
		actualLabels, err := matcher.GetNamespaceLabels(tt.attr)
		if err != nil {
			t.Error(err)
		}
		if !reflect.DeepEqual(actualLabels, tt.expectedLabels) {
			t.Errorf("expected labels to be %#v, got %#v", tt.expectedLabels, actualLabels)
		}
	}
}

func TestNotExemptClusterScopedResource(t *testing.T) {
	hook := &registrationv1.ValidatingWebhook{
		NamespaceSelector: &metav1.LabelSelector{},
	}
	attr := admission.NewAttributesRecord(nil, nil, schema.GroupVersionKind{}, "", "mock-name", schema.GroupVersionResource{Version: "v1", Resource: "nodes"}, "", admission.Create, &metav1.CreateOptions{}, false, nil)
	matcher := namespace.Matcher{}
	matches, err := matcher.MatchNamespaceSelector(webhook.NewValidatingWebhookAccessor("mock-hook", "mock-cfg", hook), attr)
	if err != nil {
		t.Fatal(err)
	}
	if !matches {
		t.Errorf("cluster scoped resources (but not a namespace) should not be exempted from webhooks")
	}
}
