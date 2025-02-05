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

package apiserver

import (
	"context"
	"net"
	"testing"

	extensionsapiserver "k8s.io/apiextensions-apiserver/pkg/apiserver"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	apiserveroptions "k8s.io/apiserver/pkg/server/options"
	cliflag "k8s.io/component-base/cli/flag"
	aggregatorscheme "k8s.io/kube-aggregator/pkg/apiserver/scheme"
	"k8s.io/kubernetes/pkg/api/legacyscheme"
	"k8s.io/kubernetes/pkg/controlplane/apiserver/options"
	generatedopenapi "k8s.io/kubernetes/pkg/generated/openapi"
	netutils "k8s.io/utils/net"
)

func TestBuildGenericConfig(t *testing.T) {
	opts := options.NewOptions()
	s := (&apiserveroptions.SecureServingOptions{
		BindAddress: netutils.ParseIPSloppy("127.0.0.1"),
	}).WithLoopback()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen on 127.0.0.1:0")
	}
	defer ln.Close()
	s.Listener = ln
	s.BindPort = ln.Addr().(*net.TCPAddr).Port
	opts.SecureServing = s

	completedOptions, err := opts.Complete(context.TODO(), cliflag.NamedFlagSets{}, nil, nil)
	if err != nil {
		t.Fatalf("Failed to complete apiserver options: %v", err)
	}

	genericConfig, _, storageFactory, err := BuildGenericConfig(
		"test-apiserver",
		completedOptions,
		[]*runtime.Scheme{legacyscheme.Scheme, extensionsapiserver.Scheme, aggregatorscheme.Scheme},
		nil,
		generatedopenapi.GetOpenAPIDefinitions,
	)
	if err != nil {
		t.Fatalf("Failed to build generic config: %v", err)
	}
	if genericConfig.StorageObjectCountTracker == nil {
		t.Errorf("genericConfig StorageObjectCountTracker is absent")
	}
	if genericConfig.StorageObjectCountTracker != storageFactory.StorageConfig.StorageObjectCountTracker {
		t.Errorf("There are different StorageObjectCountTracker in genericConfig and storageFactory")
	}

	restOptions, err := genericConfig.RESTOptionsGetter.GetRESTOptions(schema.GroupResource{Group: "", Resource: ""}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if restOptions.StorageConfig.StorageObjectCountTracker != genericConfig.StorageObjectCountTracker {
		t.Errorf("There are different StorageObjectCountTracker in restOptions and serverConfig")
	}
}
