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

package storage

import (
	"reflect"
	"testing"

	"github.com/google/go-cmp/cmp"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/version"
	genericapirequest "k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/registry/generic"
	genericregistrytest "k8s.io/apiserver/pkg/registry/generic/testing"
	"k8s.io/apiserver/pkg/registry/rest"
	etcd3testing "k8s.io/apiserver/pkg/storage/etcd3/testing"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	api "k8s.io/kubernetes/pkg/apis/core"
	"k8s.io/kubernetes/pkg/features"
	"k8s.io/kubernetes/pkg/registry/registrytest"
)

func newStorage(t *testing.T) (*REST, *StatusREST, *etcd3testing.EtcdTestServer) {
	etcdStorage, server := registrytest.NewEtcdStorage(t, "")
	restOptions := generic.RESTOptions{
		StorageConfig:           etcdStorage,
		Decorator:               generic.UndecoratedStorage,
		DeleteCollectionWorkers: 1,
		ResourcePrefix:          "persistentvolumeclaims",
	}
	persistentVolumeClaimStorage, statusStorage, err := NewREST(restOptions)
	if err != nil {
		t.Fatalf("unexpected error from REST storage: %v", err)
	}
	return persistentVolumeClaimStorage, statusStorage, server
}

func validNewPersistentVolumeClaim(name, ns string) *api.PersistentVolumeClaim {
	pv := &api.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
		},
		Spec: api.PersistentVolumeClaimSpec{
			AccessModes: []api.PersistentVolumeAccessMode{api.ReadWriteOnce},
			Resources: api.VolumeResourceRequirements{
				Requests: api.ResourceList{
					api.ResourceName(api.ResourceStorage): resource.MustParse("10G"),
				},
			},
		},
		Status: api.PersistentVolumeClaimStatus{
			Phase: api.ClaimPending,
		},
	}
	return pv
}

func TestCreate(t *testing.T) {
	storage, _, server := newStorage(t)
	defer server.Terminate(t)
	defer storage.Store.DestroyFunc()
	test := genericregistrytest.New(t, storage.Store)
	pv := validNewPersistentVolumeClaim("foo", metav1.NamespaceDefault)
	pv.ObjectMeta = metav1.ObjectMeta{}
	test.TestCreate(
		// valid
		pv,
		// invalid
		&api.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: "*BadName!"},
		},
	)
}

func TestUpdate(t *testing.T) {
	storage, _, server := newStorage(t)
	defer server.Terminate(t)
	defer storage.Store.DestroyFunc()
	test := genericregistrytest.New(t, storage.Store)
	test.TestUpdate(
		// valid
		validNewPersistentVolumeClaim("foo", metav1.NamespaceDefault),
		// updateFunc
		func(obj runtime.Object) runtime.Object {
			object := obj.(*api.PersistentVolumeClaim)
			object.Spec.VolumeName = "onlyVolumeNameUpdateAllowed"
			return object
		},
	)
}

func TestDelete(t *testing.T) {
	storage, _, server := newStorage(t)
	defer server.Terminate(t)
	defer storage.Store.DestroyFunc()
	test := genericregistrytest.New(t, storage.Store).ReturnDeletedObject()
	test.TestDelete(validNewPersistentVolumeClaim("foo", metav1.NamespaceDefault))
}

func TestGet(t *testing.T) {
	storage, _, server := newStorage(t)
	defer server.Terminate(t)
	defer storage.Store.DestroyFunc()
	test := genericregistrytest.New(t, storage.Store)
	test.TestGet(validNewPersistentVolumeClaim("foo", metav1.NamespaceDefault))
}

func TestList(t *testing.T) {
	storage, _, server := newStorage(t)
	defer server.Terminate(t)
	defer storage.Store.DestroyFunc()
	test := genericregistrytest.New(t, storage.Store)
	test.TestList(validNewPersistentVolumeClaim("foo", metav1.NamespaceDefault))
}

func TestWatch(t *testing.T) {
	storage, _, server := newStorage(t)
	defer server.Terminate(t)
	defer storage.Store.DestroyFunc()
	test := genericregistrytest.New(t, storage.Store)
	test.TestWatch(
		validNewPersistentVolumeClaim("foo", metav1.NamespaceDefault),
		// matching labels
		[]labels.Set{},
		// not matching labels
		[]labels.Set{
			{"foo": "bar"},
		},
		// matching fields
		[]fields.Set{
			{"metadata.name": "foo"},
			{"name": "foo"},
		},
		// not matching fields
		[]fields.Set{
			{"metadata.name": "bar"},
		},
	)
}

func TestUpdateStatus(t *testing.T) {
	storage, statusStorage, server := newStorage(t)
	defer server.Terminate(t)
	defer storage.Store.DestroyFunc()
	ctx := genericapirequest.NewDefaultContext()

	key, _ := storage.KeyFunc(ctx, "foo")
	pvcStart := validNewPersistentVolumeClaim("foo", metav1.NamespaceDefault)
	err := storage.Storage.Create(ctx, key, pvcStart, nil, 0, false)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	pvc := &api.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "foo",
			Namespace: metav1.NamespaceDefault,
		},
		Spec: api.PersistentVolumeClaimSpec{
			AccessModes: []api.PersistentVolumeAccessMode{api.ReadWriteOnce},
			Resources: api.VolumeResourceRequirements{
				Requests: api.ResourceList{
					api.ResourceName(api.ResourceStorage): resource.MustParse("3Gi"),
				},
			},
		},
		Status: api.PersistentVolumeClaimStatus{
			Phase: api.ClaimBound,
		},
	}

	_, _, err = statusStorage.Update(ctx, pvc.Name, rest.DefaultUpdatedObjectInfo(pvc), rest.ValidateAllObjectFunc, rest.ValidateAllObjectUpdateFunc, false, &metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	obj, err := storage.Get(ctx, "foo", &metav1.GetOptions{})
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	pvcOut := obj.(*api.PersistentVolumeClaim)
	// only compare relevant changes b/c of difference in metadata
	if !apiequality.Semantic.DeepEqual(pvc.Status, pvcOut.Status) {
		t.Errorf("unexpected object: %s", cmp.Diff(pvc.Status, pvcOut.Status))
	}
}

func TestShortNames(t *testing.T) {
	storage, _, server := newStorage(t)
	defer server.Terminate(t)
	defer storage.Store.DestroyFunc()
	expected := []string{"pvc"}
	registrytest.AssertShortNames(t, storage, expected)
}

func TestDefaultOnReadPvc(t *testing.T) {
	storage, _, server := newStorage(t)
	defer server.Terminate(t)
	defer storage.Store.DestroyFunc()
	dataSource := api.TypedLocalObjectReference{
		Kind: "PersistentVolumeClaim",
		Name: "my-pvc",
	}
	dataSourceRef := api.TypedObjectReference{
		Kind: "PersistentVolumeClaim",
		Name: "my-pvc",
	}

	var tests = map[string]struct {
		anyEnabled    bool
		dataSource    bool
		dataSourceRef bool
		want          bool
		wantRef       bool
	}{
		"any disabled with empty ds": {
			anyEnabled: false,
		},
		"any disabled with volume ds": {
			dataSource: true,
			want:       true,
		},
		"any disabled with volume ds ref": {
			dataSourceRef: true,
			wantRef:       true,
		},
		"any disabled with both data sources": {
			dataSource:    true,
			dataSourceRef: true,
			want:          true,
			wantRef:       true,
		},
		"any enabled with empty ds": {
			anyEnabled: true,
		},
		"any enabled with volume ds": {
			anyEnabled: true,
			dataSource: true,
			want:       true,
			wantRef:    true,
		},
		"any enabled with volume ds ref": {
			anyEnabled:    true,
			dataSourceRef: true,
			want:          true,
			wantRef:       true,
		},
		"any enabled with both data sources": {
			anyEnabled:    true,
			dataSource:    true,
			dataSourceRef: true,
			want:          true,
			wantRef:       true,
		},
	}

	for testName, test := range tests {
		t.Run(testName, func(t *testing.T) {
			// TODO: this will be removed in 1.36
			featuregatetesting.SetFeatureGateEmulationVersionDuringTest(t, utilfeature.DefaultFeatureGate, version.MustParse("1.32"))
			featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.AnyVolumeDataSource, test.anyEnabled)
			pvc := new(api.PersistentVolumeClaim)
			if test.dataSource {
				pvc.Spec.DataSource = dataSource.DeepCopy()
			}
			if test.dataSourceRef {
				pvc.Spec.DataSourceRef = dataSourceRef.DeepCopy()
			}
			var expectDataSource *api.TypedLocalObjectReference
			if test.want {
				expectDataSource = &dataSource
			}
			var expectDataSourceRef *api.TypedObjectReference
			if test.wantRef {
				expectDataSourceRef = &dataSourceRef
			}

			// Method under test
			storage.defaultOnReadPvc(pvc)

			if !reflect.DeepEqual(pvc.Spec.DataSource, expectDataSource) {
				t.Errorf("data source does not match, test: %s, anyEnabled: %v, dataSource: %v, expected: %v",
					testName, test.anyEnabled, test.dataSource, test.want)
			}
			if !reflect.DeepEqual(pvc.Spec.DataSourceRef, expectDataSourceRef) {
				t.Errorf("data source ref does not match, test: %s, anyEnabled: %v, dataSourceRef: %v, expected: %v",
					testName, test.anyEnabled, test.dataSourceRef, test.wantRef)
			}
		})
	}
}
