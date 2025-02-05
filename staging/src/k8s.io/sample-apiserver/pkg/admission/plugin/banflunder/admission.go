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

package banflunder

import (
	"context"
	"fmt"
	"io"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apiserver/pkg/admission"
	"k8s.io/sample-apiserver/pkg/admission/wardleinitializer"
	"k8s.io/sample-apiserver/pkg/apis/wardle"
	informers "k8s.io/sample-apiserver/pkg/generated/informers/externalversions"
	listers "k8s.io/sample-apiserver/pkg/generated/listers/wardle/v1alpha1"
)

// Register registers a plugin
func Register(plugins *admission.Plugins) {
	plugins.Register("BanFlunder", func(config io.Reader) (admission.Interface, error) {
		return New()
	})
}

// DisallowFlunder is a ban flunder admission plugin
type DisallowFlunder struct {
	*admission.Handler
	lister listers.FischerLister
}

var _ = wardleinitializer.WantsInternalWardleInformerFactory(&DisallowFlunder{})

// Admit ensures that the object in-flight is of kind Flunder.
// In addition checks that the Name is not on the banned list.
// The list is stored in Fischers API objects.
func (d *DisallowFlunder) Admit(ctx context.Context, a admission.Attributes, o admission.ObjectInterfaces) error {
	// we are only interested in flunders
	if a.GetKind().GroupKind() != wardle.Kind("Flunder") {
		return nil
	}

	if !d.WaitForReady() {
		return admission.NewForbidden(a, fmt.Errorf("not yet ready to handle request"))
	}

	metaAccessor, err := meta.Accessor(a.GetObject())
	if err != nil {
		return err
	}
	flunderName := metaAccessor.GetName()

	fischers, err := d.lister.List(labels.Everything())
	if err != nil {
		return err
	}

	for _, fischer := range fischers {
		for _, disallowedFlunder := range fischer.DisallowedFlunders {
			if flunderName == disallowedFlunder {
				return errors.NewForbidden(
					a.GetResource().GroupResource(),
					a.GetName(),
					fmt.Errorf("this name may not be used, please change the resource name"),
				)
			}
		}
	}
	return nil
}

// SetInternalWardleInformerFactory gets Lister from SharedInformerFactory.
// The lister knows how to list Fischers.
func (d *DisallowFlunder) SetInternalWardleInformerFactory(f informers.SharedInformerFactory) {
	d.lister = f.Wardle().V1alpha1().Fischers().Lister()
	d.SetReadyFunc(f.Wardle().V1alpha1().Fischers().Informer().HasSynced)
}

// ValidateInitialization checks whether the plugin was correctly initialized.
func (d *DisallowFlunder) ValidateInitialization() error {
	if d.lister == nil {
		return fmt.Errorf("missing fischer lister")
	}
	return nil
}

// New creates a new ban flunder admission plugin
func New() (*DisallowFlunder, error) {
	return &DisallowFlunder{
		Handler: admission.NewHandler(admission.Create),
	}, nil
}
