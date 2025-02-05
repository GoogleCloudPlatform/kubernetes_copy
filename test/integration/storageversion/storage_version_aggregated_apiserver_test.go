/*
Copyright 2024 The Kubernetes Authors.

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

package storageversion

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apiserver/pkg/features"
	"k8s.io/apiserver/pkg/storageversion"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	clientset "k8s.io/client-go/kubernetes"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	kubeapiservertesting "k8s.io/kubernetes/cmd/kube-apiserver/app/testing"
	"k8s.io/kubernetes/test/integration/framework"
)

type storageVersionManagerConfig struct {
	startUpdateSV  chan struct{}
	finishUpdateSV chan struct{}
	updateFinished chan struct{}
	completed      chan struct{}
}

func TestStorageVersionAPI(t *testing.T) {
	// Start kube-apiserver
	etcdConfig := framework.SharedEtcd()
	server := kubeapiservertesting.StartTestServerOrDie(t, nil, nil, etcdConfig)
	server.TearDownFn()

	// Restart api server, enable the storage version API and the feature gates.
	defer featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.StorageVersionAPI, true)()
	defer featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.APIServerIdentity, true)()
	storageVersionManagerConfig := newFakeStorageVersionManagerConfig()
	server = kubeapiservertesting.StartTestServerOrDie(t,
		&kubeapiservertesting.TestServerInstanceOptions{
			EnableCertAuth:         false,
			StorageVersionWrapFunc: storageVersionManagerWrapperFunc(storageVersionManagerConfig),
		},
		[]string{
			"--runtime-config=internal.apiserver.k8s.io/v1alpha1=true",
		},
		etcdConfig)
	defer server.TearDownFn()

	signalStorageVersionUpdate(storageVersionManagerConfig)
	kubeconfig := server.ClientConfig

	// start aggregated apiserver
	storageVersionManagerConfigAggregatedServer := newFakeStorageVersionManagerConfig()
	tearDown, _, _, err := fixtures.StartDefaultAggregatedServer(t, kubeconfig,
		&fixtures.TestServerInstanceOptions{
			StorageVersionWrapFunc: storageVersionManagerWrapperFunc(storageVersionManagerConfigAggregatedServer),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	defer tearDown()

	signalStorageVersionUpdate(storageVersionManagerConfigAggregatedServer)
	kubeapiserverClient := clientset.NewForConfigOrDie(server.ClientConfig)
	if err != nil {
		t.Fatal(err)
	}

	_, err = kubeapiserverClient.InternalV1alpha1().StorageVersions().Get(context.TODO(), "wardle.example.com.fischers", metav1.GetOptions{})
	require.NoError(t, err)
}

func newFakeStorageVersionManagerConfig() storageVersionManagerConfig {
	return storageVersionManagerConfig{
		startUpdateSV:  make(chan struct{}),
		finishUpdateSV: make(chan struct{}),
		updateFinished: make(chan struct{}),
		completed:      make(chan struct{}),
	}
}

func storageVersionManagerWrapperFunc(config storageVersionManagerConfig) func(delegate storageversion.Manager) storageversion.Manager {
	return func(delegate storageversion.Manager) storageversion.Manager {
		return &wrappedStorageVersionManager{
			startUpdateSV:  config.startUpdateSV,
			finishUpdateSV: config.finishUpdateSV,
			updateFinished: config.updateFinished,
			completed:      config.completed,
			Manager:        delegate,
		}
	}
}

func signalStorageVersionUpdate(config storageVersionManagerConfig) {
	close(config.startUpdateSV)
	<-config.updateFinished
	close(config.completed)
	close(config.finishUpdateSV)
}
