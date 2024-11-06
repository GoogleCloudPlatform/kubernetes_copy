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

package clustertrustbundles

import (
	"context"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"k8s.io/api/certificates/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured/unstructuredscheme"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	certutil "k8s.io/client-go/util/cert"
	kubeapiservertesting "k8s.io/kubernetes/cmd/kube-apiserver/app/testing"
	kubecontrollermanagertesting "k8s.io/kubernetes/cmd/kube-controller-manager/app/testing"
	"k8s.io/kubernetes/test/integration/framework"
	"k8s.io/kubernetes/test/utils/ktesting"
	"k8s.io/kubernetes/test/utils/kubeconfig"
)

func TestClusterTrustBundlesPublisherController(t *testing.T) {
	// KUBE_APISERVER_SERVE_REMOVED_APIS_FOR_ONE_RELEASE allows for APIs pending removal to not block tests
	// TODO: Remove this line once certificates v1alpha1 types to be removed in 1.32 are fully removed
	t.Setenv("KUBE_APISERVER_SERVE_REMOVED_APIS_FOR_ONE_RELEASE", "true")
	ctx := ktesting.Init(t)

	certBytes := mustMakeCertificate(t, &x509.Certificate{
		SerialNumber: big.NewInt(0),
		Subject: pkix.Name{
			CommonName: "testsigner-kas",
		},
		IsCA:                  true,
		BasicConstraintsValid: true,
	})

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certBytes})

	tmpDir := t.TempDir()
	cacertPath := filepath.Join(tmpDir, "kube-apiserver-serving.crt")
	if err := certutil.WriteCert(cacertPath, certPEM); err != nil {
		t.Fatalf("failed to write the CA cert into a file: %v", err)
	}

	apiServerFlags := []string{
		"--disable-admission-plugins", "ServiceAccount",
		"--authorization-mode=RBAC",
		"--feature-gates", "ClusterTrustBundle=true",
	}
	storageConfig := framework.SharedEtcd()
	server := kubeapiservertesting.StartTestServerOrDie(t, nil, apiServerFlags, storageConfig)
	defer server.TearDownFn()

	kubeConfigFile := createKubeConfigFileForRestConfig(t, server.ClientConfig)

	kcm := kubecontrollermanagertesting.StartTestServerOrDie(ctx, []string{
		"--kubeconfig=" + kubeConfigFile,
		"--controllers=kube-apiserver-serving-clustertrustbundle-publisher-controller", // these are the only controllers needed for this test
		"--use-service-account-credentials=true",                                       // exercise RBAC of kube-apiserver-serving-clustertrustbundle-publisher controller
		"--leader-elect=false",                                                         // KCM leader election calls os.Exit when it ends, so it is easier to just turn it off altogether
		"--root-ca-file=" + cacertPath,
		"--feature-gates=ClusterTrustBundle=true",
	})
	defer kcm.TearDownFn()

	// setup finished, tests follow
	clientSet, err := clientset.NewForConfig(server.ClientConfig)
	if err != nil {
		t.Fatalf("error in create clientset: %v", err)
	}

	unrelatedSigner := mustMakeCertificate(t, &x509.Certificate{
		SerialNumber: big.NewInt(0),
		Subject: pkix.Name{
			CommonName: "testsigner-kas",
		},
		IsCA:                  true,
		BasicConstraintsValid: true,
	})
	unrelatedPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: unrelatedSigner})
	// set up a signer that's completely unrelated to the controller to check
	// it's not anyhow handled by it
	unrelatedCTB, err := clientSet.CertificatesV1beta1().ClusterTrustBundles().Create(ctx,
		&v1beta1.ClusterTrustBundle{
			ObjectMeta: metav1.ObjectMeta{
				Name: "test.test:unrelated:0",
			},
			Spec: v1beta1.ClusterTrustBundleSpec{
				SignerName:  "test.test/unrelated",
				TrustBundle: string(unrelatedPEM),
			},
		},
		metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("failed to set up an unrelated signer CTB: %v", err)
	}

	t.Log("check that the controller creates a single buundle with expected PEM content")
	waitUntilSingleKASSignerCTB(ctx, t, clientSet, certPEM)

	t.Log("check that the controller deletes any additional bundles for the same signer")
	if _, err := clientSet.CertificatesV1beta1().ClusterTrustBundles().Create(ctx, &v1beta1.ClusterTrustBundle{
		ObjectMeta: metav1.ObjectMeta{
			Name: "kubernetes.io:kube-apiserver-serving:testname",
		},
		Spec: v1beta1.ClusterTrustBundleSpec{
			SignerName:  "kubernetes.io/kube-apiserver-serving",
			TrustBundle: string(certPEM),
		},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("failed to create an additional cluster trust bundle: %v", err)
	}

	waitUntilSingleKASSignerCTB(ctx, t, clientSet, certPEM)

	t.Log("check that the controller reconciles the bundle back to its original state if changed")
	differentSigner := mustMakeCertificate(t, &x509.Certificate{
		SerialNumber: big.NewInt(0),
		Subject: pkix.Name{
			CommonName: "testsigner-kas-different",
		},
		IsCA:                  true,
		BasicConstraintsValid: true,
	})
	differentSignerPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: differentSigner})

	ctbList, err := clientSet.CertificatesV1beta1().ClusterTrustBundles().List(ctx, metav1.ListOptions{
		FieldSelector: "spec.signerName=kubernetes.io/kube-apiserver-serving",
	})
	if err != nil || len(ctbList.Items) != 1 {
		t.Fatalf("failed to retrieve CTB list containing the single CTB for the KAS serving signer: %v", err)
	}

	ctbToUpdate := ctbList.Items[0].DeepCopy()
	ctbToUpdate.Spec.TrustBundle = string(differentSignerPEM)

	if _, err = clientSet.CertificatesV1beta1().ClusterTrustBundles().Update(ctx, ctbToUpdate, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("failed to update ctb with new PEM bundle: %v", err)
	}

	waitUntilSingleKASSignerCTB(ctx, t, clientSet, certPEM)

	unrelatedCTB, err = clientSet.CertificatesV1beta1().ClusterTrustBundles().Get(ctx, unrelatedCTB.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed to get the unrelated CTB back: %v", err)
	}
	if unrelatedCTB.Spec.TrustBundle != string(unrelatedPEM) {
		t.Fatalf("the PEM content changed for the unrelated CTB:\n%s\n", unrelatedCTB.Spec.TrustBundle)
	}

	totalSynncs := getTotalSyncMetric(ctx, t, server.ClientConfig, "clustertrustbundle_publisher_sync_total")
	if totalSynncs <= 0 {
		t.Fatalf("expected non-zero total syncs: %d", totalSynncs)
	}
}

func waitUntilSingleKASSignerCTB(ctx context.Context, t *testing.T, clientSet *clientset.Clientset, caPEM []byte) {
	err := wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (done bool, err error) {
		ctbList, err := clientSet.CertificatesV1beta1().ClusterTrustBundles().List(ctx, metav1.ListOptions{
			FieldSelector: "spec.signerName=kubernetes.io/kube-apiserver-serving",
		})

		if err != nil {
			t.Logf("failed to list kube-apiserver-signer trust bundles: %v", err)
			return false, nil
		}

		if len(ctbList.Items) != 1 {
			t.Logf("expected a single CTB, got %v", ctbList.Items)
			return false, nil
		}

		if ctbList.Items[0].Spec.TrustBundle != string(caPEM) {
			t.Logf("CTB trustBundles are different")
			return false, nil
		}
		return true, nil
	})

	if err != nil {
		t.Fatalf("there has always been a wrong number of trust bundles: %v", err)
	}

}

func createKubeConfigFileForRestConfig(t *testing.T, restConfig *rest.Config) string {
	t.Helper()

	clientConfig := kubeconfig.CreateKubeConfig(restConfig)

	kubeConfigFile := filepath.Join(t.TempDir(), "kubeconfig.yaml")
	if err := clientcmd.WriteToFile(*clientConfig, kubeConfigFile); err != nil {
		t.Fatal(err)
	}
	return kubeConfigFile
}

func getTotalSyncMetric(ctx context.Context, t *testing.T, clientConfig *rest.Config, metric string) int {
	t.Helper()

	copyConfig := rest.CopyConfig(clientConfig)
	copyConfig.GroupVersion = &schema.GroupVersion{}
	copyConfig.NegotiatedSerializer = unstructuredscheme.NewUnstructuredNegotiatedSerializer()
	rc, err := rest.RESTClientFor(copyConfig)
	if err != nil {
		t.Fatalf("Failed to create REST client: %v", err)
	}

	body, err := rc.Get().AbsPath("/metrics").DoRaw(ctx)
	if err != nil {
		t.Fatal(err)
	}

	metricRegex := regexp.MustCompile(fmt.Sprintf(`%s{.*} (\d+)`, metric))
	for _, line := range strings.Split(string(body), "\n") {
		if strings.HasPrefix(line, metric) {
			matches := metricRegex.FindStringSubmatch(line)
			if len(matches) == 2 {
				metricValue, err := strconv.Atoi(matches[1])
				if err != nil {
					t.Fatalf("Failed to convert metric value to integer: %v", err)
				}
				return metricValue
			}
		}
	}

	t.Fatalf("metric %q not seen in body:\n%s\n", metric, string(body))
	return 0
}
