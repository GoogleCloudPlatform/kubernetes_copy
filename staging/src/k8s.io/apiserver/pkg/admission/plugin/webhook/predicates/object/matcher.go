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

package object

import (
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apiserver/pkg/admission"
	"k8s.io/klog/v2"
)

type ObjectSelectorProvider interface {
	// GetObjectSelector gets the webhook ObjectSelector field.
	GetParsedObjectSelector() (labels.Selector, error)
}

// Matcher decides if a request selected by the ObjectSelector.
type Matcher struct {
}

func matchObject(obj runtime.Object, selector labels.Selector) bool {
	if obj == nil {
		return false
	}
	accessor, err := meta.Accessor(obj)
	if err != nil {
		klog.V(5).InfoS("Accessing metadata failed", "object", obj, "err", err)
		return false
	}
	return selector.Matches(labels.Set(accessor.GetLabels()))

}

// MatchObjectSelector decides whether the request matches the ObjectSelector
// of the webhook. Only when they match, the webhook is called.
func (m *Matcher) MatchObjectSelector(p ObjectSelectorProvider, attr admission.Attributes) (bool, *apierrors.StatusError) {
	selector, err := p.GetParsedObjectSelector()
	if err != nil {
		return false, apierrors.NewInternalError(err)
	}
	if selector.Empty() {
		return true, nil
	}
	return matchObject(attr.GetObject(), selector) || matchObject(attr.GetOldObject(), selector), nil
}
