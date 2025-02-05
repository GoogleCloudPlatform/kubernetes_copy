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

package network

import (
	"context"
	appsv1 "k8s.io/api/apps/v1"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/uuid"
	clientset "k8s.io/client-go/kubernetes"
	imageutils "k8s.io/kubernetes/test/utils/image"

	"github.com/onsi/ginkgo/v2"
)

// TestFixture is a simple helper class to avoid too much boilerplate in tests
type TestFixture struct {
	ServiceName string
	Namespace   string
	Client      clientset.Interface

	TestID string
	Labels map[string]string

	deployments map[string]bool
	services    map[string]bool
	Name        string
	Image       string
}

// NewServerTest creates a new TestFixture for the tests.
func NewServerTest(client clientset.Interface, namespace string, serviceName string) *TestFixture {
	t := &TestFixture{}
	t.Client = client
	t.Namespace = namespace
	t.ServiceName = serviceName
	t.TestID = t.ServiceName + "-" + string(uuid.NewUUID())
	t.Labels = map[string]string{
		"testid": t.TestID,
	}

	t.deployments = make(map[string]bool)
	t.services = make(map[string]bool)

	t.Name = "webserver"
	t.Image = imageutils.GetE2EImage(imageutils.Agnhost)

	return t
}

// BuildServiceSpec builds default config for a service (which can then be changed)
func (t *TestFixture) BuildServiceSpec() *v1.Service {
	service := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      t.ServiceName,
			Namespace: t.Namespace,
		},
		Spec: v1.ServiceSpec{
			Selector: t.Labels,
			Ports: []v1.ServicePort{{
				Port:       80,
				TargetPort: intstr.FromInt32(80),
			}},
		},
	}
	return service
}

func (t *TestFixture) CreateDeployment(deployment *appsv1.Deployment) (*appsv1.Deployment, error) {
	deployment, err := t.Client.AppsV1().Deployments(t.Namespace).Create(context.TODO(), deployment, metav1.CreateOptions{})
	if err == nil {
		t.deployments[deployment.Name] = true
	}
	return deployment, err
}

// CreateService creates a service, and record it for cleanup
func (t *TestFixture) CreateService(service *v1.Service) (*v1.Service, error) {
	result, err := t.Client.CoreV1().Services(t.Namespace).Create(context.TODO(), service, metav1.CreateOptions{})
	if err == nil {
		t.services[service.Name] = true
	}
	return result, err
}

// DeleteService deletes a service, and remove it from the cleanup list
func (t *TestFixture) DeleteService(serviceName string) error {
	err := t.Client.CoreV1().Services(t.Namespace).Delete(context.TODO(), serviceName, metav1.DeleteOptions{})
	if err == nil {
		delete(t.services, serviceName)
	}
	return err
}

// Cleanup cleans all ReplicationControllers and Services which this object holds.
func (t *TestFixture) Cleanup() []error {
	var errs []error
	for deploymentName := range t.deployments {
		ginkgo.By("deleting deployment " + deploymentName + " in namespace " + t.Namespace)
		err := t.Client.AppsV1().Deployments(t.Namespace).Delete(context.TODO(), deploymentName, metav1.DeleteOptions{})
		if err != nil {
			if !apierrors.IsNotFound(err) {
				errs = append(errs, err)
			}
		}
	}

	for serviceName := range t.services {
		ginkgo.By("deleting service " + serviceName + " in namespace " + t.Namespace)
		err := t.Client.CoreV1().Services(t.Namespace).Delete(context.TODO(), serviceName, metav1.DeleteOptions{})
		if err != nil {
			if !apierrors.IsNotFound(err) {
				errs = append(errs, err)
			}
		}
	}

	return errs
}
