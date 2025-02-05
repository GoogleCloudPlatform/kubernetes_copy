/*
Copyright 2023 The Kubernetes Authors.

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

package openapi

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	apiextensions "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	kubernetes "k8s.io/client-go/kubernetes"
	apiregistrationv1 "k8s.io/kube-aggregator/pkg/apis/apiregistration/v1"
	aggregator "k8s.io/kube-aggregator/pkg/client/clientset_generated/clientset"
	"k8s.io/kube-openapi/pkg/validation/spec"
	kubeapiservertesting "k8s.io/kubernetes/cmd/kube-apiserver/app/testing"
	testdiscovery "k8s.io/kubernetes/test/integration/apiserver/discovery"
	"k8s.io/kubernetes/test/integration/framework"
)

func TestSlowAPIServiceOpenAPIDoesNotBlockHealthCheck(t *testing.T) {
	ctx, cancelCtx := context.WithCancel(context.Background())
	defer cancelCtx()

	etcd := framework.SharedEtcd()
	setupServer := kubeapiservertesting.StartTestServerOrDie(t, nil, framework.DefaultTestServerFlags(), etcd)
	client := generateTestClient(t, setupServer)

	service := testdiscovery.NewFakeService("test-server", client, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/openapi/v2" {
			return
		}
		// Effectively let the APIService block until request timeout.
		<-ctx.Done()
		openapi := &spec.Swagger{
			SwaggerProps: spec.SwaggerProps{
				Paths: &spec.Paths{
					Paths: map[string]spec.PathItem{
						"/apis/wardle.example.com/v1alpha1": {},
					},
				},
			},
		}
		data, err := openapi.MarshalJSON()
		if err != nil {
			t.Error(err)
		}
		http.ServeContent(w, r, "/openapi/v2", time.Now(), bytes.NewReader(data))
	}))
	go func() {
		require.NoError(t, service.Run(ctx))
	}()
	require.NoError(t, service.WaitForReady(ctx))

	groupVersion := metav1.GroupVersion{
		Group:   "wardle.example.com",
		Version: "v1alpha1",
	}

	require.NoError(t, registerAPIService(ctx, client, groupVersion, service))

	setupServer.TearDownFn()
	server := kubeapiservertesting.StartTestServerOrDie(t, nil, framework.DefaultTestServerFlags(), etcd)
	t.Cleanup(server.TearDownFn)
	client2 := generateTestClient(t, server)

	err := wait.PollUntilContextTimeout(context.Background(), 100*time.Millisecond, 1*time.Second, true, func(context.Context) (bool, error) {
		var statusCode int
		client2.AdmissionregistrationV1().RESTClient().Get().AbsPath("/healthz").Do(context.TODO()).StatusCode(&statusCode)
		if statusCode == 200 {
			return true, nil
		}
		return false, nil
	})
	require.NoError(t, err)
}

func TestFetchingOpenAPIBeforeReady(t *testing.T) {
	ctx, cancelCtx := context.WithCancel(context.Background())
	defer cancelCtx()

	server := kubeapiservertesting.StartTestServerOrDie(t, nil, framework.DefaultTestServerFlags(), framework.SharedEtcd())
	t.Cleanup(server.TearDownFn)
	client := generateTestClient(t, server)

	readyCh := make(chan bool)
	defer close(readyCh)
	go func() {
		select {
		case <-readyCh:
		default:
			_, _ = client.Discovery().RESTClient().Get().AbsPath("/openapi/v2").Do(context.TODO()).Raw()
		}
	}()

	service := testdiscovery.NewFakeService("test-server", client, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		openapi := &spec.Swagger{
			SwaggerProps: spec.SwaggerProps{
				Paths: &spec.Paths{
					Paths: map[string]spec.PathItem{
						"/apis/wardle.example.com/v1alpha1/": {},
					},
				},
			},
		}
		data, err := openapi.MarshalJSON()
		if err != nil {
			t.Error(err)
		}
		http.ServeContent(w, r, "/openapi/v2", time.Now(), bytes.NewReader(data))
	}))
	go func() {
		require.NoError(t, service.Run(ctx))
	}()
	require.NoError(t, service.WaitForReady(ctx))

	groupVersion := metav1.GroupVersion{
		Group:   "wardle.example.com",
		Version: "v1alpha1",
	}

	require.NoError(t, registerAPIService(ctx, client, groupVersion, service))
	defer func() {
		require.NoError(t, unregisterAPIService(ctx, client, groupVersion))
	}()

	err := wait.PollUntilContextTimeout(context.Background(), time.Millisecond*10, time.Second, true, func(context.Context) (bool, error) {
		b, err := client.Discovery().RESTClient().Get().AbsPath("/openapi/v2").Do(context.TODO()).Raw()
		require.NoError(t, err)
		var openapi spec.Swagger
		require.NoError(t, openapi.UnmarshalJSON(b))
		if _, ok := openapi.Paths.Paths["/apis/wardle.example.com/v1alpha1/"]; ok {
			return true, nil
		}
		return false, nil
	})
	require.NoError(t, err)

}

// These definitions were copied from k8s.io/kubernetes/test/integration/apiserver/discovery
// and should be consolidated.
type kubeClientSet = kubernetes.Interface

type aggegatorClientSet = aggregator.Interface

type apiextensionsClientSet = apiextensions.Interface

type dynamicClientset = dynamic.Interface

type testClientSet struct {
	kubeClientSet
	aggegatorClientSet
	apiextensionsClientSet
	dynamicClientset
}

type testClient interface {
	kubernetes.Interface
	aggregator.Interface
	apiextensions.Interface
	dynamic.Interface
}

var _ testClient = testClientSet{}

func (t testClientSet) Discovery() discovery.DiscoveryInterface {
	return t.kubeClientSet.Discovery()
}

func generateTestClient(t *testing.T, server *kubeapiservertesting.TestServer) testClient {
	kubeClientSet, err := kubernetes.NewForConfig(server.ClientConfig)
	require.NoError(t, err)

	aggegatorClientSet, err := aggregator.NewForConfig(server.ClientConfig)
	require.NoError(t, err)

	apiextensionsClientSet, err := apiextensions.NewForConfig(server.ClientConfig)
	require.NoError(t, err)

	dynamicClientset, err := dynamic.NewForConfig(server.ClientConfig)
	require.NoError(t, err)

	client := testClientSet{
		kubeClientSet:          kubeClientSet,
		aggegatorClientSet:     aggegatorClientSet,
		apiextensionsClientSet: apiextensionsClientSet,
		dynamicClientset:       dynamicClientset,
	}
	return client
}

func registerAPIService(ctx context.Context, client aggregator.Interface, gv metav1.GroupVersion, service testdiscovery.FakeService) error {
	port := service.Port()
	if port == nil {
		return errors.New("service not yet started")
	}
	// Register the APIService
	patch := apiregistrationv1.APIService{
		ObjectMeta: metav1.ObjectMeta{
			Name: gv.Version + "." + gv.Group,
		},
		TypeMeta: metav1.TypeMeta{
			Kind:       "APIService",
			APIVersion: "apiregistration.k8s.io/v1",
		},
		Spec: apiregistrationv1.APIServiceSpec{
			Group:                 gv.Group,
			Version:               gv.Version,
			InsecureSkipTLSVerify: true,
			GroupPriorityMinimum:  1000,
			VersionPriority:       15,
			Service: &apiregistrationv1.ServiceReference{
				Namespace: "default",
				Name:      service.Name(),
				Port:      port,
			},
		},
	}

	_, err := client.
		ApiregistrationV1().
		APIServices().
		Create(context.TODO(), &patch, metav1.CreateOptions{FieldManager: "test-manager"})
	return err
}

func unregisterAPIService(ctx context.Context, client aggregator.Interface, gv metav1.GroupVersion) error {
	return client.ApiregistrationV1().APIServices().Delete(ctx, gv.Version+"."+gv.Group, metav1.DeleteOptions{})
}
