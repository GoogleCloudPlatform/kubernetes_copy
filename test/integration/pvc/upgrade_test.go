/*
Copyright 2022 The Kubernetes Authors.

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

package pvc

import (
	"context"
	"testing"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	kubeapiservertesting "k8s.io/kubernetes/cmd/kube-apiserver/app/testing"
	"k8s.io/kubernetes/pkg/api/legacyscheme"
	"k8s.io/kubernetes/test/integration/framework"
)

func Test_UpgradePVC(t *testing.T) {
	t.Run("feature_enabled", func(t *testing.T) { test_UpgradePVC(t, true) })
	t.Run("feature_disabled", func(t *testing.T) { test_UpgradePVC(t, false) })
}

func test_UpgradePVC(t *testing.T, featureEnabled bool) {
	etcdOptions := framework.SharedEtcd()
	apiServerOptions := kubeapiservertesting.NewDefaultTestServerOptions()
	s := kubeapiservertesting.StartTestServerOrDie(t, apiServerOptions, nil, etcdOptions)
	defer s.TearDownFn()
	pvcName := "test-old-pvc"
	ns := "old-pvc-ns"

	kubeclient, err := kubernetes.NewForConfig(s.ClientConfig)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if _, err := kubeclient.CoreV1().Namespaces().Create(context.TODO(), (&v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	// Create a pvc and store it in etcd with missing fields representing an old version
	pvc := &v1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:              pvcName,
			Namespace:         ns,
			CreationTimestamp: metav1.Now(),
			UID:               "08675309-9376-9376-9376-086753099999",
		},
		Spec: v1.PersistentVolumeClaimSpec{
			Resources: v1.VolumeResourceRequirements{
				Requests: v1.ResourceList{
					v1.ResourceName(v1.ResourceStorage): resource.MustParse("10G"),
				},
			},
			DataSource: &v1.TypedLocalObjectReference{
				APIGroup: nil,
				Kind:     "PersistentVolumeClaim",
				Name:     "foo",
			},
			AccessModes: []v1.PersistentVolumeAccessMode{v1.ReadWriteOnce},
		},
	}
	pvcJSON, err := runtime.Encode(legacyscheme.Codecs.LegacyCodec(v1.SchemeGroupVersion), pvc)
	if err != nil {
		t.Fatalf("Failed creating pvc JSON: %v", err)
	}
	key := "/" + etcdOptions.Prefix + "/persistentvolumeclaims/" + ns + "/" + pvcName
	if _, err := s.EtcdClient.Put(context.Background(), key, string(pvcJSON)); err != nil {
		t.Error(err)
	}
	t.Logf("PVC stored in etcd %v", string(pvcJSON))

	// Try to update the pvc as a no-op write of the original content
	{
		_, err := kubeclient.CoreV1().PersistentVolumeClaims(ns).Update(context.TODO(), pvc, metav1.UpdateOptions{DryRun: []string{"All"}})
		if err != nil {
			t.Errorf("write of original content failed: %v", err)
		}
	}

	// Try to update the pvc as an internal server no-op patch of the original content
	{
		_, err := kubeclient.CoreV1().PersistentVolumeClaims(ns).Patch(context.TODO(), pvc.Name, types.MergePatchType, []byte(`{}`), metav1.PatchOptions{DryRun: []string{"All"}})
		if err != nil {
			t.Errorf("no-op patch failed: %v", err)
		}
	}

	// Try to update the pvc as a no-op get/update
	{
		getPVC, err := kubeclient.CoreV1().PersistentVolumeClaims(ns).Get(context.TODO(), pvc.Name, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		_, err = kubeclient.CoreV1().PersistentVolumeClaims(ns).Update(context.TODO(), getPVC, metav1.UpdateOptions{DryRun: []string{"All"}})
		if err != nil {
			t.Errorf("no-op get/put failed: %v", err)
		}
	}
}
