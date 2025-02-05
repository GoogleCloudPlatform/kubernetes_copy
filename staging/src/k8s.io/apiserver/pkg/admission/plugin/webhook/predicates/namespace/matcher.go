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

package namespace

import (
	"context"
	"fmt"

	v1 "k8s.io/api/core/v1"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apiserver/pkg/admission"
	clientset "k8s.io/client-go/kubernetes"
	corelisters "k8s.io/client-go/listers/core/v1"
)

type NamespaceSelectorProvider interface {
	// GetNamespaceSelector gets the webhook NamespaceSelector field.
	GetParsedNamespaceSelector() (labels.Selector, error)
}

// Matcher decides if a request is exempted by the NamespaceSelector of a
// webhook configuration.
type Matcher struct {
	NamespaceLister corelisters.NamespaceLister
	Client          clientset.Interface
}

func (m *Matcher) GetNamespace(name string) (*v1.Namespace, error) {
	return m.NamespaceLister.Get(name)
}

// Validate checks if the Matcher has a NamespaceLister and Client.
func (m *Matcher) Validate() error {
	var errs []error
	if m.NamespaceLister == nil {
		errs = append(errs, fmt.Errorf("the namespace matcher requires a namespaceLister"))
	}
	if m.Client == nil {
		errs = append(errs, fmt.Errorf("the namespace matcher requires a client"))
	}
	return utilerrors.NewAggregate(errs)
}

// GetNamespaceLabels gets the labels of the namespace related to the attr.
func (m *Matcher) GetNamespaceLabels(attr admission.Attributes) (map[string]string, error) {
	// If the request itself is creating or updating a namespace, then get the
	// labels from attr.Object, because namespaceLister doesn't have the latest
	// namespace yet.
	//
	// However, if the request is deleting a namespace, then get the label from
	// the namespace in the namespaceLister, because a delete request is not
	// going to change the object, and attr.Object will be a DeleteOptions
	// rather than a namespace object.
	if attr.GetResource().Resource == "namespaces" &&
		len(attr.GetSubresource()) == 0 &&
		(attr.GetOperation() == admission.Create || attr.GetOperation() == admission.Update) {
		accessor, err := meta.Accessor(attr.GetObject())
		if err != nil {
			return nil, err
		}
		return accessor.GetLabels(), nil
	}

	namespaceName := attr.GetNamespace()
	namespace, err := m.NamespaceLister.Get(namespaceName)
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, err
	}
	if apierrors.IsNotFound(err) {
		// in case of latency in our caches, make a call direct to storage to verify that it truly exists or not
		namespace, err = m.Client.CoreV1().Namespaces().Get(context.TODO(), namespaceName, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
	}
	return namespace.Labels, nil
}

// MatchNamespaceSelector decides whether the request matches the
// namespaceSelector of the webhook. Only when they match, the webhook is called.
func (m *Matcher) MatchNamespaceSelector(p NamespaceSelectorProvider, attr admission.Attributes) (bool, *apierrors.StatusError) {
	namespaceName := attr.GetNamespace()
	if len(namespaceName) == 0 && attr.GetResource().Resource != "namespaces" {
		// If the request is about a cluster scoped resource, and it is not a
		// namespace, it is never exempted.
		// TODO: figure out a way selective exempt cluster scoped resources.
		// Also update the comment in types.go
		return true, nil
	}
	selector, err := p.GetParsedNamespaceSelector()
	if err != nil {
		return false, apierrors.NewInternalError(err)
	}
	if selector.Empty() {
		return true, nil
	}

	namespaceLabels, err := m.GetNamespaceLabels(attr)
	// this means the namespace is not found, for backwards compatibility,
	// return a 404
	if apierrors.IsNotFound(err) {
		status, ok := err.(apierrors.APIStatus)
		if !ok {
			return false, apierrors.NewInternalError(err)
		}
		return false, &apierrors.StatusError{ErrStatus: status.Status()}
	}
	if err != nil {
		return false, apierrors.NewInternalError(err)
	}
	return selector.Matches(labels.Set(namespaceLabels)), nil
}
