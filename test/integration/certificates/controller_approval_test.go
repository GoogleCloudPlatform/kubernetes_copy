/*
Copyright 2020 The Kubernetes Authors.

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

package certificates

import (
	"context"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"testing"
	"time"

	certv1 "k8s.io/api/certificates/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/informers"
	clientset "k8s.io/client-go/kubernetes"
	restclient "k8s.io/client-go/rest"
	kubeapiservertesting "k8s.io/kubernetes/cmd/kube-apiserver/app/testing"
	"k8s.io/kubernetes/pkg/controller/certificates"
	"k8s.io/kubernetes/pkg/controller/certificates/approver"
	"k8s.io/kubernetes/test/integration/authutil"
	"k8s.io/kubernetes/test/integration/framework"
	"k8s.io/kubernetes/test/utils/ktesting"
)

// Integration tests that verify the behaviour of the CSR auto-approving controller.
func TestController_AutoApproval(t *testing.T) {
	validKubeAPIServerClientKubeletUsername := "system:node:abc"
	validKubeAPIServerClientKubeletCSR := pemWithTemplate(&x509.CertificateRequest{
		Subject: pkix.Name{
			CommonName:   validKubeAPIServerClientKubeletUsername,
			Organization: []string{"system:nodes"},
		},
	})
	validKubeAPIServerClientKubeletUsages := []certv1.KeyUsage{
		certv1.UsageDigitalSignature,
		certv1.UsageKeyEncipherment,
		certv1.UsageClientAuth,
	}
	tests := map[string]struct {
		signerName          string
		request             []byte
		usages              []certv1.KeyUsage
		username            string
		autoApproved        bool
		grantNodeClient     bool
		grantSelfNodeClient bool
	}{
		"should auto-approve CSR that has kube-apiserver-client-kubelet signerName and matches requirements": {
			signerName:          certv1.KubeAPIServerClientKubeletSignerName,
			request:             validKubeAPIServerClientKubeletCSR,
			usages:              validKubeAPIServerClientKubeletUsages,
			username:            validKubeAPIServerClientKubeletUsername,
			grantSelfNodeClient: true,
			autoApproved:        true,
		},
		"should auto-approve CSR that has kube-apiserver-client-kubelet signerName and matches requirements despite missing username if nodeclient permissions are granted": {
			signerName:      certv1.KubeAPIServerClientKubeletSignerName,
			request:         validKubeAPIServerClientKubeletCSR,
			usages:          validKubeAPIServerClientKubeletUsages,
			username:        "does-not-match-cn",
			grantNodeClient: true,
			autoApproved:    true,
		},
		"should not auto-approve CSR that has kube-apiserver-client signerName that DOES match kubelet CSR requirements": {
			signerName:   certv1.KubeAPIServerClientSignerName,
			request:      validKubeAPIServerClientKubeletCSR,
			usages:       validKubeAPIServerClientKubeletUsages,
			username:     validKubeAPIServerClientKubeletUsername,
			autoApproved: false,
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			tCtx := ktesting.Init(t)
			// Run an apiserver with the default configuration options.
			s := kubeapiservertesting.StartTestServerOrDie(t, kubeapiservertesting.NewDefaultTestServerOptions(), []string{""}, framework.SharedEtcd())
			defer s.TearDownFn()
			defer tCtx.Cancel("test has completed")
			client := clientset.NewForConfigOrDie(s.ClientConfig)
			informers := informers.NewSharedInformerFactory(clientset.NewForConfigOrDie(restclient.AddUserAgent(s.ClientConfig, "certificatesigningrequest-informers")), time.Second)

			// Register the controller
			c := approver.NewCSRApprovingController(tCtx, client, informers.Certificates().V1().CertificateSigningRequests())
			// Start the controller & informers
			informers.Start(tCtx.Done())
			go c.Run(tCtx, 1)

			// Configure appropriate permissions
			if test.grantNodeClient {
				grantUserNodeClientPermissions(t, client, test.username, false)
			}
			if test.grantSelfNodeClient {
				grantUserNodeClientPermissions(t, client, test.username, true)
			}

			// Use a client that impersonates the test case 'username' to ensure the `spec.username`
			// field on the CSR is set correctly.
			impersonationConfig := restclient.CopyConfig(s.ClientConfig)
			impersonationConfig.Impersonate.UserName = test.username
			impersonationClient, err := clientset.NewForConfig(impersonationConfig)
			if err != nil {
				t.Fatalf("Error in create clientset: %v", err)
			}
			csr := &certv1.CertificateSigningRequest{
				ObjectMeta: metav1.ObjectMeta{
					Name: "csr",
				},
				Spec: certv1.CertificateSigningRequestSpec{
					Request:    test.request,
					Usages:     test.usages,
					SignerName: test.signerName,
				},
			}
			_, err = impersonationClient.CertificatesV1().CertificateSigningRequests().Create(tCtx, csr, metav1.CreateOptions{})
			if err != nil {
				t.Fatalf("failed to create testing CSR: %v", err)
			}

			if test.autoApproved {
				if err := waitForCertificateRequestApproved(client, csr.Name); err != nil {
					t.Errorf("failed to wait for CSR to be auto-approved: %v", err)
				}
			} else {
				if err := ensureCertificateRequestNotApproved(client, csr.Name); err != nil {
					t.Errorf("failed to ensure that CSR was not auto-approved: %v", err)
				}
			}
		})
	}
}

const (
	interval = 100 * time.Millisecond
	timeout  = 5 * time.Second
)

func waitForCertificateRequestApproved(client clientset.Interface, name string) error {
	if err := wait.Poll(interval, timeout, func() (bool, error) {
		csr, err := client.CertificatesV1().CertificateSigningRequests().Get(context.TODO(), name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		if certificates.IsCertificateRequestApproved(csr) {
			return true, nil
		}
		return false, nil
	}); err != nil {
		return err
	}
	return nil
}

func ensureCertificateRequestNotApproved(client clientset.Interface, name string) error {
	// If waiting for the CSR to be approved times out, we class this as 'not auto approved'.
	// There is currently no way to explicitly check if the CSR has been rejected for auto-approval.
	err := waitForCertificateRequestApproved(client, name)
	switch {
	case err == wait.ErrWaitTimeout:
		return nil
	case err == nil:
		return fmt.Errorf("CertificateSigningRequest was auto-approved")
	default:
		return err
	}
}

func grantUserNodeClientPermissions(t *testing.T, client clientset.Interface, username string, selfNodeClient bool) {
	resourceType := "certificatesigningrequests/nodeclient"
	if selfNodeClient {
		resourceType = "certificatesigningrequests/selfnodeclient"
	}
	cr := buildNodeClientRoleForUser("role", resourceType)
	crb := buildClusterRoleBindingForUser("rolebinding", username, cr.Name)
	if _, err := client.RbacV1().ClusterRoles().Create(context.TODO(), cr, metav1.CreateOptions{}); err != nil {
		t.Fatalf("failed to create test fixtures: %v", err)
	}
	if _, err := client.RbacV1().ClusterRoleBindings().Create(context.TODO(), crb, metav1.CreateOptions{}); err != nil {
		t.Fatalf("failed to create test fixtures: %v", err)
	}
	rule := cr.Rules[0]
	authutil.WaitForNamedAuthorizationUpdate(t, context.TODO(), client.AuthorizationV1(), username, "", rule.Verbs[0], "", schema.GroupResource{Group: rule.APIGroups[0], Resource: rule.Resources[0]}, true)
}

func buildNodeClientRoleForUser(name string, resourceType string) *rbacv1.ClusterRole {
	return &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Rules: []rbacv1.PolicyRule{
			{
				Verbs:     []string{"create"},
				APIGroups: []string{certv1.SchemeGroupVersion.Group},
				Resources: []string{resourceType},
			},
		},
	}
}
