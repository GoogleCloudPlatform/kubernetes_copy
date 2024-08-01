/*
Copyright The Kubernetes Authors.

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

// Code generated by client-gen. DO NOT EDIT.

package v1alpha1

import (
	"context"

	v1alpha1 "k8s.io/api/admissionregistration/v1alpha1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	types "k8s.io/apimachinery/pkg/types"
	watch "k8s.io/apimachinery/pkg/watch"
	admissionregistrationv1alpha1 "k8s.io/client-go/applyconfigurations/admissionregistration/v1alpha1"
	gentype "k8s.io/client-go/gentype"
	scheme "k8s.io/client-go/kubernetes/scheme"
)

// MutatingAdmissionPolicyBindingsGetter has a method to return a MutatingAdmissionPolicyBindingInterface.
// A group's client should implement this interface.
type MutatingAdmissionPolicyBindingsGetter interface {
	MutatingAdmissionPolicyBindings() MutatingAdmissionPolicyBindingInterface
}

// MutatingAdmissionPolicyBindingInterface has methods to work with MutatingAdmissionPolicyBinding resources.
type MutatingAdmissionPolicyBindingInterface interface {
	Create(ctx context.Context, mutatingAdmissionPolicyBinding *v1alpha1.MutatingAdmissionPolicyBinding, opts v1.CreateOptions) (*v1alpha1.MutatingAdmissionPolicyBinding, error)
	Update(ctx context.Context, mutatingAdmissionPolicyBinding *v1alpha1.MutatingAdmissionPolicyBinding, opts v1.UpdateOptions) (*v1alpha1.MutatingAdmissionPolicyBinding, error)
	Delete(ctx context.Context, name string, opts v1.DeleteOptions) error
	DeleteCollection(ctx context.Context, opts v1.DeleteOptions, listOpts v1.ListOptions) error
	Get(ctx context.Context, name string, opts v1.GetOptions) (*v1alpha1.MutatingAdmissionPolicyBinding, error)
	List(ctx context.Context, opts v1.ListOptions) (*v1alpha1.MutatingAdmissionPolicyBindingList, error)
	Watch(ctx context.Context, opts v1.ListOptions) (watch.Interface, error)
	Patch(ctx context.Context, name string, pt types.PatchType, data []byte, opts v1.PatchOptions, subresources ...string) (result *v1alpha1.MutatingAdmissionPolicyBinding, err error)
	Apply(ctx context.Context, mutatingAdmissionPolicyBinding *admissionregistrationv1alpha1.MutatingAdmissionPolicyBindingApplyConfiguration, opts v1.ApplyOptions) (result *v1alpha1.MutatingAdmissionPolicyBinding, err error)
	MutatingAdmissionPolicyBindingExpansion
}

// mutatingAdmissionPolicyBindings implements MutatingAdmissionPolicyBindingInterface
type mutatingAdmissionPolicyBindings struct {
	*gentype.ClientWithListAndApply[*v1alpha1.MutatingAdmissionPolicyBinding, *v1alpha1.MutatingAdmissionPolicyBindingList, *admissionregistrationv1alpha1.MutatingAdmissionPolicyBindingApplyConfiguration]
}

// newMutatingAdmissionPolicyBindings returns a MutatingAdmissionPolicyBindings
func newMutatingAdmissionPolicyBindings(c *AdmissionregistrationV1alpha1Client) *mutatingAdmissionPolicyBindings {
	return &mutatingAdmissionPolicyBindings{
		gentype.NewClientWithListAndApply[*v1alpha1.MutatingAdmissionPolicyBinding, *v1alpha1.MutatingAdmissionPolicyBindingList, *admissionregistrationv1alpha1.MutatingAdmissionPolicyBindingApplyConfiguration](
			"mutatingadmissionpolicybindings",
			c.RESTClient(),
			scheme.ParameterCodec,
			"",
			func() *v1alpha1.MutatingAdmissionPolicyBinding { return &v1alpha1.MutatingAdmissionPolicyBinding{} },
			func() *v1alpha1.MutatingAdmissionPolicyBindingList {
				return &v1alpha1.MutatingAdmissionPolicyBindingList{}
			}),
	}
}
