/*
Copyright 2016 The Kubernetes Authors.

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

package factory

import (
	"context"
	"os"
	"path"
	"path/filepath"
	"testing"

	"go.etcd.io/etcd/client/pkg/v3/transport"
	noopoteltrace "go.opentelemetry.io/otel/trace/noop"

	apitesting "k8s.io/apimachinery/pkg/api/apitesting"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apiserver/pkg/apis/example"
	examplev1 "k8s.io/apiserver/pkg/apis/example/v1"
	"k8s.io/apiserver/pkg/storage/etcd3/testing/testingcert"
	"k8s.io/apiserver/pkg/storage/etcd3/testserver"
	"k8s.io/apiserver/pkg/storage/storagebackend"
)

var scheme = runtime.NewScheme()
var codecs = serializer.NewCodecFactory(scheme)

func init() {
	metav1.AddToGroupVersion(scheme, metav1.SchemeGroupVersion)
	utilruntime.Must(example.AddToScheme(scheme))
	utilruntime.Must(examplev1.AddToScheme(scheme))
}

func TestTLSConnection(t *testing.T) {
	codec := apitesting.TestCodec(codecs, examplev1.SchemeGroupVersion)

	certFile, keyFile, caFile := configureTLSCerts(t)
	defer os.RemoveAll(filepath.Dir(certFile))

	// override server config to be TLS-enabled
	etcdConfig := testserver.NewTestConfig(t)
	etcdConfig.ClientTLSInfo = transport.TLSInfo{
		CertFile:      certFile,
		KeyFile:       keyFile,
		TrustedCAFile: caFile,
	}
	for i := range etcdConfig.ListenClientUrls {
		etcdConfig.ListenClientUrls[i].Scheme = "https"
	}
	for i := range etcdConfig.AdvertiseClientUrls {
		etcdConfig.AdvertiseClientUrls[i].Scheme = "https"
	}

	client := testserver.RunEtcd(t, etcdConfig)
	cfg := storagebackend.Config{
		Type: storagebackend.StorageTypeETCD3,
		Transport: storagebackend.TransportConfig{
			ServerList:     client.Endpoints(),
			CertFile:       certFile,
			KeyFile:        keyFile,
			TrustedCAFile:  caFile,
			TracerProvider: noopoteltrace.NewTracerProvider(),
		},
		Codec: codec,
	}
	storage, destroyFunc, err := newETCD3Storage(*cfg.ForResource(schema.GroupResource{Resource: "pods"}), nil, nil, "")
	defer destroyFunc()
	if err != nil {
		t.Fatal(err)
	}
	err = storage.Create(context.TODO(), "/abc", &example.Pod{}, nil, 0)
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}
}

func configureTLSCerts(t *testing.T) (certFile, keyFile, caFile string) {
	baseDir := os.TempDir()
	tempDir, err := os.MkdirTemp(baseDir, "etcd_certificates")
	if err != nil {
		t.Fatal(err)
	}
	certFile = path.Join(tempDir, "etcdcert.pem")
	if err := os.WriteFile(certFile, []byte(testingcert.CertFileContent), 0644); err != nil {
		t.Fatal(err)
	}
	keyFile = path.Join(tempDir, "etcdkey.pem")
	if err := os.WriteFile(keyFile, []byte(testingcert.KeyFileContent), 0644); err != nil {
		t.Fatal(err)
	}
	caFile = path.Join(tempDir, "ca.pem")
	if err := os.WriteFile(caFile, []byte(testingcert.CAFileContent), 0644); err != nil {
		t.Fatal(err)
	}
	return certFile, keyFile, caFile
}
