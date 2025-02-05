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
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	kubeapiservertesting "k8s.io/kubernetes/cmd/kube-apiserver/app/testing"
	"k8s.io/kubernetes/test/integration/authutil"
	"k8s.io/kubernetes/test/integration/framework"
)

// Verifies that the ClusterTrustBundle attest admission plugin correctly
// enforces that a user has "attest" on the affected signer name.
func TestCTBAttestPlugin(t *testing.T) {
	// KUBE_APISERVER_SERVE_REMOVED_APIS_FOR_ONE_RELEASE allows for APIs pending removal to not block tests
	// TODO: Remove this line once certificates v1alpha1 types to be removed in 1.32 are fully removed
	t.Setenv("KUBE_APISERVER_SERVE_REMOVED_APIS_FOR_ONE_RELEASE", "true")

	testCases := []struct {
		description       string
		trustBundleName   string
		allowedSignerName string
		targetSignerName  string
		wantError         string
	}{
		{
			description:       "should admit if the clustertrustbundle doesn't target a signer",
			trustBundleName:   "foo",
			allowedSignerName: "foo.com/bar",
		},
		{
			description:       "should admit if the user has attest for the exact signer name",
			trustBundleName:   "foo.com:bar:abc",
			allowedSignerName: "foo.com/bar",
			targetSignerName:  "foo.com/bar",
		},
		{
			description:       "should admit if the user has attest for the wildcard-suffixed signer name",
			trustBundleName:   "foo.com:bar:abc",
			allowedSignerName: "foo.com/*",
			targetSignerName:  "foo.com/bar",
		},
		{
			description:       "should deny if the user does not have permission for the signer name",
			trustBundleName:   "foo.com:bar:abc",
			allowedSignerName: "abc.com/def",
			targetSignerName:  "foo.com/bar",
			wantError:         "clustertrustbundles.certificates.k8s.io \"foo.com:bar:abc\" is forbidden: user not permitted to attest for signerName \"foo.com/bar\"",
		},
	}
	for _, tc := range testCases {
		t.Run(tc.description, func(t *testing.T) {
			ctx := context.Background()

			server := kubeapiservertesting.StartTestServerOrDie(t, nil, []string{"--authorization-mode=RBAC", "--feature-gates=ClusterTrustBundle=true"}, framework.SharedEtcd())
			defer server.TearDownFn()

			client := kubernetes.NewForConfigOrDie(server.ClientConfig)

			if tc.allowedSignerName != "" {
				grantUserPermissionToAttestFor(ctx, t, client, "test-user", tc.allowedSignerName)
			}

			// Create a second client that impersonates test-user.
			testUserConfig := rest.CopyConfig(server.ClientConfig)
			testUserConfig.Impersonate = rest.ImpersonationConfig{UserName: "test-user"}
			testUserClient := kubernetes.NewForConfigOrDie(testUserConfig)

			bundle := &certsv1beta1.ClusterTrustBundle{
				ObjectMeta: metav1.ObjectMeta{
					Name: tc.trustBundleName,
				},
				Spec: certsv1beta1.ClusterTrustBundleSpec{
					SignerName: tc.targetSignerName,
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
			_, err := testUserClient.CertificatesV1beta1().ClusterTrustBundles().Create(ctx, bundle, metav1.CreateOptions{})
			if err != nil && err.Error() != tc.wantError {
				t.Fatalf("Bad error while creating ClusterTrustBundle; got %q want %q", err.Error(), tc.wantError)
			} else if err == nil && tc.wantError != "" {
				t.Fatalf("Bad error while creating ClusterTrustBundle; got nil want %q", tc.wantError)
			}
		})
	}
}

func grantUserPermissionToAttestFor(ctx context.Context, t *testing.T, client kubernetes.Interface, username string, signerNames ...string) {
	resourceName := "signername-" + username
	cr := buildApprovalClusterRoleForSigners(resourceName, signerNames...)
	crb := buildClusterRoleBindingForUser(resourceName, username, cr.Name)
	if _, err := client.RbacV1().ClusterRoles().Create(ctx, cr, metav1.CreateOptions{}); err != nil {
		t.Fatalf("unable to create test fixture RBAC rules: %v", err)
	}
	if _, err := client.RbacV1().ClusterRoleBindings().Create(ctx, crb, metav1.CreateOptions{}); err != nil {
		t.Fatalf("unable to create test fixture RBAC rules: %v", err)
	}
	attestRule := cr.Rules[0]
	createRule := cr.Rules[1]
	authutil.WaitForNamedAuthorizationUpdate(t, ctx, client.AuthorizationV1(), username, "", attestRule.Verbs[0], attestRule.ResourceNames[0], schema.GroupResource{Group: attestRule.APIGroups[0], Resource: attestRule.Resources[0]}, true)
	authutil.WaitForNamedAuthorizationUpdate(t, ctx, client.AuthorizationV1(), username, "", createRule.Verbs[0], "", schema.GroupResource{Group: createRule.APIGroups[0], Resource: createRule.Resources[0]}, true)
}

func buildApprovalClusterRoleForSigners(name string, signerNames ...string) *rbacv1.ClusterRole {
	return &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Rules: []rbacv1.PolicyRule{
			{
				Verbs:         []string{"attest"},
				APIGroups:     []string{"certificates.k8s.io"},
				Resources:     []string{"signers"},
				ResourceNames: signerNames,
			},
			{
				Verbs:     []string{"create"},
				APIGroups: []string{"certificates.k8s.io"},
				Resources: []string{"clustertrustbundles"},
			},
		},
	}
}

func buildClusterRoleBindingForUser(name, username, clusterRoleName string) *rbacv1.ClusterRoleBinding {
	return &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Subjects: []rbacv1.Subject{
			{
				Kind: rbacv1.UserKind,
				Name: username,
			},
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.SchemeGroupVersion.Group,
			Kind:     "ClusterRole",
			Name:     clusterRoleName,
		},
	}
}
