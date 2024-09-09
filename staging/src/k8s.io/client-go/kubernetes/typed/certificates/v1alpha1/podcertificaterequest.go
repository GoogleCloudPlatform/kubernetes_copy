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
	context "context"

	certificatesv1alpha1 "k8s.io/api/certificates/v1alpha1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	types "k8s.io/apimachinery/pkg/types"
	watch "k8s.io/apimachinery/pkg/watch"
	applyconfigurationscertificatesv1alpha1 "k8s.io/client-go/applyconfigurations/certificates/v1alpha1"
	gentype "k8s.io/client-go/gentype"
	scheme "k8s.io/client-go/kubernetes/scheme"
)

// PodCertificateRequestsGetter has a method to return a PodCertificateRequestInterface.
// A group's client should implement this interface.
type PodCertificateRequestsGetter interface {
	PodCertificateRequests(namespace string) PodCertificateRequestInterface
}

// PodCertificateRequestInterface has methods to work with PodCertificateRequest resources.
type PodCertificateRequestInterface interface {
	Create(ctx context.Context, podCertificateRequest *certificatesv1alpha1.PodCertificateRequest, opts v1.CreateOptions) (*certificatesv1alpha1.PodCertificateRequest, error)
	Update(ctx context.Context, podCertificateRequest *certificatesv1alpha1.PodCertificateRequest, opts v1.UpdateOptions) (*certificatesv1alpha1.PodCertificateRequest, error)
	// Add a +genclient:noStatus comment above the type to avoid generating UpdateStatus().
	UpdateStatus(ctx context.Context, podCertificateRequest *certificatesv1alpha1.PodCertificateRequest, opts v1.UpdateOptions) (*certificatesv1alpha1.PodCertificateRequest, error)
	Delete(ctx context.Context, name string, opts v1.DeleteOptions) error
	DeleteCollection(ctx context.Context, opts v1.DeleteOptions, listOpts v1.ListOptions) error
	Get(ctx context.Context, name string, opts v1.GetOptions) (*certificatesv1alpha1.PodCertificateRequest, error)
	List(ctx context.Context, opts v1.ListOptions) (*certificatesv1alpha1.PodCertificateRequestList, error)
	Watch(ctx context.Context, opts v1.ListOptions) (watch.Interface, error)
	Patch(ctx context.Context, name string, pt types.PatchType, data []byte, opts v1.PatchOptions, subresources ...string) (result *certificatesv1alpha1.PodCertificateRequest, err error)
	Apply(ctx context.Context, podCertificateRequest *applyconfigurationscertificatesv1alpha1.PodCertificateRequestApplyConfiguration, opts v1.ApplyOptions) (result *certificatesv1alpha1.PodCertificateRequest, err error)
	// Add a +genclient:noStatus comment above the type to avoid generating ApplyStatus().
	ApplyStatus(ctx context.Context, podCertificateRequest *applyconfigurationscertificatesv1alpha1.PodCertificateRequestApplyConfiguration, opts v1.ApplyOptions) (result *certificatesv1alpha1.PodCertificateRequest, err error)
	PodCertificateRequestExpansion
}

// podCertificateRequests implements PodCertificateRequestInterface
type podCertificateRequests struct {
	*gentype.ClientWithListAndApply[*certificatesv1alpha1.PodCertificateRequest, *certificatesv1alpha1.PodCertificateRequestList, *applyconfigurationscertificatesv1alpha1.PodCertificateRequestApplyConfiguration]
}

// newPodCertificateRequests returns a PodCertificateRequests
func newPodCertificateRequests(c *CertificatesV1alpha1Client, namespace string) *podCertificateRequests {
	return &podCertificateRequests{
		gentype.NewClientWithListAndApply[*certificatesv1alpha1.PodCertificateRequest, *certificatesv1alpha1.PodCertificateRequestList, *applyconfigurationscertificatesv1alpha1.PodCertificateRequestApplyConfiguration](
			"podcertificaterequests",
			c.RESTClient(),
			scheme.ParameterCodec,
			namespace,
			func() *certificatesv1alpha1.PodCertificateRequest {
				return &certificatesv1alpha1.PodCertificateRequest{}
			},
			func() *certificatesv1alpha1.PodCertificateRequestList {
				return &certificatesv1alpha1.PodCertificateRequestList{}
			}),
	}
}
