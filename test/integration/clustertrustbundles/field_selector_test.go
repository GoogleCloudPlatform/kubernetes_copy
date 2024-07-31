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

package clustertrustbundles

import (
	"context"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"testing"

	certsv1beta1 "k8s.io/api/certificates/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	kubeapiservertesting "k8s.io/kubernetes/cmd/kube-apiserver/app/testing"
	"k8s.io/kubernetes/test/integration/framework"
)

func TestCTBSignerNameFieldSelector(t *testing.T) {
	// KUBE_APISERVER_SERVE_REMOVED_APIS_FOR_ONE_RELEASE allows for APIs pending removal to not block tests
	// TODO: Remove this line once certificates v1alpha1 types to be removed in 1.32 are fully removed
	t.Setenv("KUBE_APISERVER_SERVE_REMOVED_APIS_FOR_ONE_RELEASE", "true")

	ctx := context.Background()

	server := kubeapiservertesting.StartTestServerOrDie(t, nil, []string{"--feature-gates=ClusterTrustBundle=true"}, framework.SharedEtcd())
	defer server.TearDownFn()

	client := kubernetes.NewForConfigOrDie(server.ClientConfig)

	bundle1 := &certsv1beta1.ClusterTrustBundle{
		ObjectMeta: metav1.ObjectMeta{
			Name: "foo.com:bar:v1",
		},
		Spec: certsv1beta1.ClusterTrustBundleSpec{
			SignerName: "foo.com/bar",
			TrustBundle: mustMakePEMBlock("CERTIFICATE", nil, mustMakeCertificate(t, &x509.Certificate{
				SerialNumber: big.NewInt(0),
				Subject: pkix.Name{
					CommonName: "root1",
				},
				IsCA:                  true,
				BasicConstraintsValid: true,
			})),
		},
	}
	if _, err := client.CertificatesV1beta1().ClusterTrustBundles().Create(ctx, bundle1, metav1.CreateOptions{}); err != nil {
		t.Fatalf("Error while creating bundle1: %v", err)
	}

	bundle2 := &certsv1beta1.ClusterTrustBundle{
		ObjectMeta: metav1.ObjectMeta{
			Name: "foo.com:bar:v2",
		},
		Spec: certsv1beta1.ClusterTrustBundleSpec{
			SignerName: "foo.com/bar",
			TrustBundle: mustMakePEMBlock("CERTIFICATE", nil, mustMakeCertificate(t, &x509.Certificate{
				SerialNumber: big.NewInt(0),
				Subject: pkix.Name{
					CommonName: "root2",
				},
				IsCA:                  true,
				BasicConstraintsValid: true,
			})),
		},
	}
	if _, err := client.CertificatesV1beta1().ClusterTrustBundles().Create(ctx, bundle2, metav1.CreateOptions{}); err != nil {
		t.Fatalf("Error while creating bundle2: %v", err)
	}

	bundle3 := &certsv1beta1.ClusterTrustBundle{
		ObjectMeta: metav1.ObjectMeta{
			Name: "baz.com:bar:v1",
		},
		Spec: certsv1beta1.ClusterTrustBundleSpec{
			SignerName: "baz.com/bar",
			TrustBundle: mustMakePEMBlock("CERTIFICATE", nil, mustMakeCertificate(t, &x509.Certificate{
				SerialNumber: big.NewInt(0),
				Subject: pkix.Name{
					CommonName: "root3",
				},
				IsCA:                  true,
				BasicConstraintsValid: true,
			})),
		},
	}
	if _, err := client.CertificatesV1beta1().ClusterTrustBundles().Create(ctx, bundle3, metav1.CreateOptions{}); err != nil {
		t.Fatalf("Error while creating bundle3: %v", err)
	}

	fooList, err := client.CertificatesV1beta1().ClusterTrustBundles().List(ctx, metav1.ListOptions{FieldSelector: "spec.signerName=foo.com/bar"})
	if err != nil {
		t.Fatalf("Unable to list ClusterTrustBundles with spec.signerName=foo.com/bar")
	}
	if len(fooList.Items) != 2 {
		t.Errorf("Wrong number of items in list for foo.com/bar; got %d, want 2", len(fooList.Items))
	}
	found1 := false
	found2 := false
	for _, ctb := range fooList.Items {
		if ctb.ObjectMeta.Name == "foo.com:bar:v1" {
			found1 = true
		}
		if ctb.ObjectMeta.Name == "foo.com:bar:v2" {
			found2 = true
		}
	}
	if !found1 {
		t.Errorf("Didn't find foo.com:bar:v1 in the list when listing for foo.com/bar")
	}
	if !found2 {
		t.Errorf("Didn't find foo.com:bar:v2 in the list when listing for foo.com/bar")
	}

	bazList, err := client.CertificatesV1beta1().ClusterTrustBundles().List(ctx, metav1.ListOptions{FieldSelector: "spec.signerName=baz.com/bar"})
	if err != nil {
		t.Fatalf("Unable to list ClusterTrustBundles with spec.signerName=baz.com/bar")
	}
	if len(bazList.Items) != 1 {
		t.Fatalf("Wrong number of items in list for baz.com/bar; got %d, want 1", len(bazList.Items))
	}
	if bazList.Items[0].ObjectMeta.Name != "baz.com:bar:v1" {
		t.Errorf("Didn't find baz.com:bar:v1 in the list when listing for baz.com/bar")
	}
}
