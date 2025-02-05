/*
Copyright 2014 The Kubernetes Authors.

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

package storage

import (
	"context"
	"testing"
	"time"

	"gopkg.in/go-jose/go-jose.v2/jwt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apiserver/pkg/audit"
	"k8s.io/apiserver/pkg/authentication/authenticator"
	"k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/registry/generic"
	genericregistrytest "k8s.io/apiserver/pkg/registry/generic/testing"
	"k8s.io/apiserver/pkg/registry/rest"
	etcd3testing "k8s.io/apiserver/pkg/storage/etcd3/testing"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	authenticationapi "k8s.io/kubernetes/pkg/apis/authentication"
	api "k8s.io/kubernetes/pkg/apis/core"
	"k8s.io/kubernetes/pkg/features"
	"k8s.io/kubernetes/pkg/registry/registrytest"
	token "k8s.io/kubernetes/pkg/serviceaccount"
)

func newStorage(t *testing.T) (*REST, *etcd3testing.EtcdTestServer) {
	return newTokenStorage(t, fakeTokenGenerator{"fake"}, nil, panicGetter{}, panicGetter{}, nil)
}

func newTokenStorage(t *testing.T, issuer token.TokenGenerator, auds authenticator.Audiences, podStorage, secretStorage, nodeStorage rest.Getter) (*REST, *etcd3testing.EtcdTestServer) {
	etcdStorage, server := registrytest.NewEtcdStorage(t, "")
	restOptions := generic.RESTOptions{
		StorageConfig:           etcdStorage,
		Decorator:               generic.UndecoratedStorage,
		DeleteCollectionWorkers: 1,
		ResourcePrefix:          "serviceaccounts",
	}
	// set issuer, podStore and secretStore to allow the token endpoint to be initialised
	rest, err := NewREST(restOptions, issuer, auds, 0, podStorage, secretStorage, nodeStorage, false, time.Hour*9999)
	if err != nil {
		t.Fatalf("unexpected error from REST storage: %v", err)
	}
	return rest, server
}

// A basic fake token generator which always returns a static string
type fakeTokenGenerator struct {
	staticToken string
}

func (f fakeTokenGenerator) GenerateToken(ctx context.Context, claims *jwt.Claims, privateClaims interface{}) (string, error) {
	return f.staticToken, nil
}

var _ token.TokenGenerator = fakeTokenGenerator{}

// Currently this getter only panics as the only test case doesn't actually need the getters to function.
// When more test cases are added, this getter will need extending/replacing to have a real test implementation.
type panicGetter struct{}

func (f panicGetter) Get(ctx context.Context, name string, options *metav1.GetOptions) (runtime.Object, error) {
	panic("not implemented")
}

var _ rest.Getter = panicGetter{}

func validNewServiceAccount(name string) *api.ServiceAccount {
	return &api.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: metav1.NamespaceDefault,
		},
		Secrets: []api.ObjectReference{},
	}
}

func TestCreate(t *testing.T) {
	storage, server := newStorage(t)
	defer server.Terminate(t)
	defer storage.Store.DestroyFunc()
	test := genericregistrytest.New(t, storage.Store)
	serviceAccount := validNewServiceAccount("foo")
	serviceAccount.ObjectMeta = metav1.ObjectMeta{GenerateName: "foo-"}
	test.TestCreate(
		// valid
		serviceAccount,
		// invalid
		&api.ServiceAccount{},
		&api.ServiceAccount{
			ObjectMeta: metav1.ObjectMeta{Name: "name with spaces"},
		},
	)
}

func TestCreate_Token_SetsCredentialIDAuditAnnotation(t *testing.T) {
	storage, server := newStorage(t)
	defer server.Terminate(t)
	defer storage.Store.DestroyFunc()

	// Enable JTI feature
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.ServiceAccountTokenJTI, true)

	ctx := context.Background()
	// Create a test service account
	serviceAccount := validNewServiceAccount("foo")
	// add the namespace to the context as it is required
	ctx = request.WithNamespace(ctx, serviceAccount.Namespace)
	_, err := storage.Store.Create(ctx, serviceAccount, rest.ValidateAllObjectFunc, &metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("failed creating test service account: %v", err)
	}

	// create an audit context to allow recording audit information
	ctx = audit.WithAuditContext(ctx)
	_, err = storage.Token.Create(ctx, serviceAccount.Name, &authenticationapi.TokenRequest{
		ObjectMeta: metav1.ObjectMeta{
			Name:      serviceAccount.Name,
			Namespace: serviceAccount.Namespace,
		},
		Spec: authenticationapi.TokenRequestSpec{ExpirationSeconds: 3600},
	}, rest.ValidateAllObjectFunc, &metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("failed calling /token endpoint for service account: %v", err)
	}

	auditContext := audit.AuditContextFrom(ctx)
	issuedCredentialID, ok := auditContext.GetEventAnnotation("authentication.kubernetes.io/issued-credential-id")
	if !ok || len(issuedCredentialID) == 0 {
		t.Errorf("did not find issued-credential-id in audit event annotations")
	}
}

func TestUpdate(t *testing.T) {
	storage, server := newStorage(t)
	defer server.Terminate(t)
	defer storage.Store.DestroyFunc()
	test := genericregistrytest.New(t, storage.Store)
	test.TestUpdate(
		// valid
		validNewServiceAccount("foo"),
		// updateFunc
		func(obj runtime.Object) runtime.Object {
			object := obj.(*api.ServiceAccount)
			object.Secrets = []api.ObjectReference{{}}
			return object
		},
	)
}

func TestDelete(t *testing.T) {
	storage, server := newStorage(t)
	defer server.Terminate(t)
	defer storage.Store.DestroyFunc()
	test := genericregistrytest.New(t, storage.Store).ReturnDeletedObject()
	test.TestDelete(validNewServiceAccount("foo"))
}

func TestGet(t *testing.T) {
	storage, server := newStorage(t)
	defer server.Terminate(t)
	defer storage.Store.DestroyFunc()
	test := genericregistrytest.New(t, storage.Store)
	test.TestGet(validNewServiceAccount("foo"))
}

func TestList(t *testing.T) {
	storage, server := newStorage(t)
	defer server.Terminate(t)
	defer storage.Store.DestroyFunc()
	test := genericregistrytest.New(t, storage.Store)
	test.TestList(validNewServiceAccount("foo"))
}

func TestWatch(t *testing.T) {
	storage, server := newStorage(t)
	defer server.Terminate(t)
	defer storage.Store.DestroyFunc()
	test := genericregistrytest.New(t, storage.Store)
	test.TestWatch(
		validNewServiceAccount("foo"),
		// matching labels
		[]labels.Set{},
		// not matching labels
		[]labels.Set{
			{"foo": "bar"},
		},
		// matching fields
		[]fields.Set{
			{"metadata.name": "foo"},
		},
		// not matching fields
		[]fields.Set{
			{"metadata.name": "bar"},
			{"name": "foo"},
		},
	)
}

func TestShortNames(t *testing.T) {
	storage, server := newStorage(t)
	defer server.Terminate(t)
	defer storage.Store.DestroyFunc()
	expected := []string{"sa"}
	registrytest.AssertShortNames(t, storage, expected)
}
