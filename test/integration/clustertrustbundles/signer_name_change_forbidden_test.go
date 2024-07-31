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
	"fmt"
	"math/big"
	"testing"

	certsv1beta1 "k8s.io/api/certificates/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	kubeapiservertesting "k8s.io/kubernetes/cmd/kube-apiserver/app/testing"
	"k8s.io/kubernetes/test/integration/framework"
)

func TestCTBSignerNameChangeForbidden(t *testing.T) {
	// KUBE_APISERVER_SERVE_REMOVED_APIS_FOR_ONE_RELEASE allows for APIs pending removal to not block tests
	// TODO: Remove this line once certificates v1alpha1 types to be removed in 1.32 are fully removed
	t.Setenv("KUBE_APISERVER_SERVE_REMOVED_APIS_FOR_ONE_RELEASE", "true")

	testCases := []struct {
		objectName string
		signer1    string
		signer2    string
	}{
		{
			objectName: "foo",
			signer1:    "",
			signer2:    "foo.com/bar",
		},
		{
			objectName: "foo.com:bar:abc",
			signer1:    "foo.com/bar",
			signer2:    "",
		},
		{
			objectName: "foo.com:bar:abc",
			signer1:    "foo.com/bar",
			signer2:    "foo.com/bar2",
		},
	}

	for _, tc := range testCases {
		t.Run(fmt.Sprintf("%s -> %s", tc.signer1, tc.signer2), func(t *testing.T) {

			ctx := context.Background()

			server := kubeapiservertesting.StartTestServerOrDie(t, nil, []string{"--feature-gates=ClusterTrustBundle=true"}, framework.SharedEtcd())
			defer server.TearDownFn()

			client := kubernetes.NewForConfigOrDie(server.ClientConfig)

			bundle1 := &certsv1beta1.ClusterTrustBundle{
				ObjectMeta: metav1.ObjectMeta{
					Name: tc.objectName,
				},
				Spec: certsv1beta1.ClusterTrustBundleSpec{
					SignerName: tc.signer1,
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
			bundle1, err := client.CertificatesV1beta1().ClusterTrustBundles().Create(ctx, bundle1, metav1.CreateOptions{})
			if err != nil {
				t.Fatalf("Error while creating bundle1: %v", err)
			}

			// Pick a new signer name that is still compatible with the admission
			// restrictions on object name.  That way the admission plugin won't get in
			// the way by forbidding the update due to an incompatible name on the
			// cluster trust bundle.
			bundle1.Spec.SignerName = tc.signer2

			_, err = client.CertificatesV1beta1().ClusterTrustBundles().Update(ctx, bundle1, metav1.UpdateOptions{})
			if err == nil {
				t.Fatalf("Got nil error from updating bundle foo-com--bar from signerName=foo.com/bar to signerName=foo.com/bar2, but wanted an error")
			}
		})
	}

}
