/*
Copyright 2015 The Kubernetes Authors.

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

package ingress

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	genericapirequest "k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/kubernetes/pkg/apis/networking"
)

func newIngress() networking.Ingress {
	serviceBackend := &networking.IngressServiceBackend{
		Name: "default-backend",
		Port: networking.ServiceBackendPort{
			Name:   "",
			Number: 80,
		},
	}
	defaultBackend := networking.IngressBackend{
		Service: serviceBackend.DeepCopy(),
	}
	implementationPathType := networking.PathTypeImplementationSpecific
	return networking.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "foo",
			Namespace: metav1.NamespaceDefault,
		},
		Spec: networking.IngressSpec{
			DefaultBackend: defaultBackend.DeepCopy(),
			Rules: []networking.IngressRule{
				{
					Host: "foo.bar.com",
					IngressRuleValue: networking.IngressRuleValue{
						HTTP: &networking.HTTPIngressRuleValue{
							Paths: []networking.HTTPIngressPath{
								{
									Path:     "/foo",
									PathType: &implementationPathType,
									Backend:  *defaultBackend.DeepCopy(),
								},
							},
						},
					},
				},
			},
		},
		Status: networking.IngressStatus{
			LoadBalancer: networking.IngressLoadBalancerStatus{
				Ingress: []networking.IngressLoadBalancerIngress{
					{IP: "127.0.0.1"},
				},
			},
		},
	}
}

func TestIngressStrategy(t *testing.T) {
	ctx := genericapirequest.NewDefaultContext()
	apiRequest := genericapirequest.RequestInfo{APIGroup: "networking.k8s.io",
		APIVersion: "v1",
		Resource:   "ingresses",
	}
	ctx = genericapirequest.WithRequestInfo(ctx, &apiRequest)
	if !Strategy.NamespaceScoped() {
		t.Errorf("Ingress must be namespace scoped")
	}
	if Strategy.AllowCreateOnUpdate() {
		t.Errorf("Ingress should not allow create on update")
	}

	ingress := newIngress()
	Strategy.PrepareForCreate(ctx, &ingress)
	if len(ingress.Status.LoadBalancer.Ingress) != 0 {
		t.Error("Ingress should not allow setting status on create")
	}
	errs := Strategy.Validate(ctx, &ingress)
	if len(errs) != 0 {
		t.Errorf("Unexpected error validating %v", errs)
	}
	invalidIngress := newIngress()
	invalidIngress.ResourceVersion = "4"
	invalidIngress.Spec = networking.IngressSpec{}
	Strategy.PrepareForUpdate(ctx, &invalidIngress, &ingress)
	errs = Strategy.ValidateUpdate(ctx, &invalidIngress, &ingress)
	if len(errs) == 0 {
		t.Errorf("Expected a validation error")
	}
	if invalidIngress.ResourceVersion != "4" {
		t.Errorf("Incoming resource version on update should not be mutated")
	}
}

func TestIngressStatusStrategy(t *testing.T) {
	ctx := genericapirequest.NewDefaultContext()
	if !StatusStrategy.NamespaceScoped() {
		t.Errorf("Ingress must be namespace scoped")
	}
	if StatusStrategy.AllowCreateOnUpdate() {
		t.Errorf("Ingress should not allow create on update")
	}
	oldIngress := newIngress()
	newIngress := newIngress()
	oldIngress.ResourceVersion = "4"
	newIngress.ResourceVersion = "4"
	newIngress.Spec.DefaultBackend.Service.Name = "ignore"
	newIngress.Status = networking.IngressStatus{
		LoadBalancer: networking.IngressLoadBalancerStatus{
			Ingress: []networking.IngressLoadBalancerIngress{
				{IP: "127.0.0.2"},
			},
		},
	}
	StatusStrategy.PrepareForUpdate(ctx, &newIngress, &oldIngress)
	if newIngress.Status.LoadBalancer.Ingress[0].IP != "127.0.0.2" {
		t.Errorf("Ingress status updates should allow change of status fields")
	}
	if newIngress.Spec.DefaultBackend.Service.Name != "default-backend" {
		t.Errorf("PrepareForUpdate should have preserved old spec")
	}
	errs := StatusStrategy.ValidateUpdate(ctx, &newIngress, &oldIngress)
	if len(errs) != 0 {
		t.Errorf("Unexpected error %v", errs)
	}

	warnings := StatusStrategy.WarningsOnUpdate(ctx, &newIngress, &oldIngress)
	if len(warnings) != 0 {
		t.Errorf("Unexpected warnings %v", errs)
	}

	newIngress.Status.LoadBalancer.Ingress[0].IP = "127.000.000.002"
	warnings = StatusStrategy.WarningsOnUpdate(ctx, &newIngress, &oldIngress)
	if len(warnings) != 1 {
		t.Errorf("Did not get warning for bad IP")
	}
}
