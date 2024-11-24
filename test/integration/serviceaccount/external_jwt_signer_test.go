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

package serviceaccount

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"k8s.io/kubernetes/cmd/kube-apiserver/app/options"
	"k8s.io/kubernetes/test/integration/framework"
	"k8s.io/kubernetes/test/utils/ktesting"

	authv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	"k8s.io/kubernetes/pkg/features"
	v1alpha1testing "k8s.io/kubernetes/pkg/serviceaccount/externaljwt/plugin/testing/v1alpha1"
)

func TestExternalJWTSigningAndAuth(t *testing.T) {
	// Enable feature gate for external JWT signer.
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.ExternalServiceAccountTokenSigner, true)

	// Prep some keys to use with test.
	key1, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic("Error while generating first RSA key")
	}
	pubKey1Bytes, err := x509.MarshalPKIXPublicKey(&key1.PublicKey)
	if err != nil {
		panic("Error while marshaling first public key")
	}

	tCtx := ktesting.Init(t)
	ctx, cancel := context.WithCancel(tCtx)
	defer cancel()

	// create and start mock signer.
	socketPath := fmt.Sprintf("@mock-external-jwt-signer-%d.sock", time.Now().Nanosecond())
	t.Cleanup(func() { _ = os.Remove(socketPath) })
	mockSigner := v1alpha1testing.NewMockSigner(t, socketPath)
	defer mockSigner.CleanUp()

	// Start Api server configured with external signer.
	client, _, tearDownFn := framework.StartTestServer(ctx, t, framework.TestServerSetup{
		ModifyServerRunOptions: func(opt *options.ServerRunOptions) {
			opt.ServiceAccountSigningEndpoint = socketPath
			opt.ServiceAccountSigningKeyFile = ""
			opt.Authentication.ServiceAccounts.KeyFiles = []string{}
		},
	})
	defer tearDownFn()

	// Create Namesapce (ns-1) to work with.
	if _, err := client.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "ns-1",
		},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("Error when creating namespace: %v", err)
	}

	// Create ServiceAccount (sa-1) to work with.
	if _, err := client.CoreV1().ServiceAccounts("ns-1").Create(ctx, &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name: "sa-1",
		},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("Error when creating service-account: %v", err)
	}

	testCases := []struct {
		desc                      string
		preTestSignerUpdate       func()
		preValidationSignerUpdate func()
		wantTokenReqErr           error
		shouldPassAuth            bool
	}{
		{
			desc:                      "signing key supported.",
			preTestSignerUpdate:       func() { /*no-op*/ },
			preValidationSignerUpdate: func() { /*no-op*/ },
			shouldPassAuth:            true,
		},
		{
			desc: "signing key not among supported set",
			preTestSignerUpdate: func() {
				mockSigner.SigningKey = key1
				mockSigner.SigningKeyID = "updated-kid-1"
			},
			preValidationSignerUpdate: func() { /*no-op*/ },
			shouldPassAuth:            false,
		},
		{
			desc: "signing key corresponds to public key that is excluded from OIDC",
			preTestSignerUpdate: func() {
				mockSigner.SigningKey = key1
				mockSigner.SigningKeyID = "updated-kid-1"

				cpy := make(map[string]v1alpha1testing.KeyT)
				for key, value := range mockSigner.GetSupportedKeys() {
					cpy[key] = value
				}
				cpy["updated-kid-1"] = v1alpha1testing.KeyT{
					Key:                      pubKey1Bytes,
					ExcludeFromOidcDiscovery: true,
				}
				mockSigner.SetSupportedKeys(cpy)
			},
			preValidationSignerUpdate: func() { /*no-op*/ },
			wantTokenReqErr:           fmt.Errorf("failed to generate token: while validating header: key used for signing JWT (kid: updated-kid-1) is excluded from OIDC discovery docs"),
		},
		{
			desc: "different signing and supported keys with same id",
			preTestSignerUpdate: func() {
				mockSigner.SigningKey = key1
			},
			preValidationSignerUpdate: func() { /*no-op*/ },
			shouldPassAuth:            false,
		},
		{
			desc: "token gen failure with un-supported Alg type",
			preTestSignerUpdate: func() {
				mockSigner.SigningAlg = "ABC"
			},
			preValidationSignerUpdate: func() { /*no-op*/ },
			wantTokenReqErr:           fmt.Errorf("failed to generate token: while validating header: bad signing algorithm \"ABC\""),
		},
		{
			desc: "token gen failure with un-supported token type",
			preTestSignerUpdate: func() {
				mockSigner.TokenType = "ABC"
			},
			preValidationSignerUpdate: func() { /*no-op*/ },
			wantTokenReqErr:           fmt.Errorf("failed to generate token: while validating header: bad type"),
		},
		{
			desc: "change of supported keys not picked immediately",
			preTestSignerUpdate: func() {
				mockSigner.SigningKey = key1
			},
			preValidationSignerUpdate: func() {
				mockSigner.SetSupportedKeys(map[string]v1alpha1testing.KeyT{})
			},
			shouldPassAuth: false,
		},
		{
			desc: "change of supported keys picked up after periodic sync",
			preTestSignerUpdate: func() {
				mockSigner.SigningKey = key1
			},
			preValidationSignerUpdate: func() {
				cpy := make(map[string]v1alpha1testing.KeyT)
				for key, value := range mockSigner.GetSupportedKeys() {
					cpy[key] = value
				}
				cpy["kid-1"] = v1alpha1testing.KeyT{Key: pubKey1Bytes}
				mockSigner.SetSupportedKeys(cpy)
				mockSigner.WaitForSupportedKeysFetch()
				// allow some time for plugin to be ready with new keys before proceeding
				time.Sleep(2 * time.Second)
			},
			shouldPassAuth: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.desc, func(t *testing.T) {
			// Start fresh
			err := mockSigner.Reset()
			if err != nil {
				t.Fatalf("failed to reset signer for the test %q: %v", tc.desc, err)
			}
			mockSigner.WaitForSupportedKeysFetch()

			// Adjust parameters on mock signer for the test.
			tc.preTestSignerUpdate()

			// Request a token for ns-1:sa-1.
			tokenExpirationSec := int64(2 * 60 * 60) // 2h
			tokenRequest, err := client.CoreV1().ServiceAccounts("ns-1").CreateToken(ctx, "sa-1", &authv1.TokenRequest{
				Spec: authv1.TokenRequestSpec{
					ExpirationSeconds: &tokenExpirationSec,
				},
			}, metav1.CreateOptions{})
			if tc.wantTokenReqErr != nil {
				if err == nil || !strings.Contains(err.Error(), tc.wantTokenReqErr.Error()) {
					t.Fatalf("wanted error: %v, got error: %v", tc.wantTokenReqErr, err)
				}
				return
			} else if err != nil {
				t.Fatalf("Error when creating token: %v", err)
			}

			// Adjust parameters on mock signer for the test.
			tc.preValidationSignerUpdate()

			// Try Validating the token.
			tokenReviewResult, err := client.AuthenticationV1().TokenReviews().Create(ctx, &authv1.TokenReview{
				Spec: authv1.TokenReviewSpec{
					Token: tokenRequest.Status.Token,
				},
			}, metav1.CreateOptions{})
			if err != nil {
				t.Fatalf("Error when validating token: %v", err)
			}

			if !tokenReviewResult.Status.Authenticated && tc.shouldPassAuth {
				t.Fatalf("Expected Authentication to succeed, got %v", tokenReviewResult.Status.Error)
			} else if tokenReviewResult.Status.Authenticated && !tc.shouldPassAuth {
				t.Fatal("Expected Authentication to fail")
			}
		})
	}
}

func TestDelayedStartForSigner(t *testing.T) {
	// Enable feature gate for external JWT signer.
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.ExternalServiceAccountTokenSigner, true)

	tCtx := ktesting.Init(t)
	ctx, cancel := context.WithCancel(tCtx)
	defer cancel()

	// Schedule signer to start on socket after 20 sec
	socketPath := "@mock-external-jwt-signer.sock"
	t.Cleanup(func() { _ = os.Remove(socketPath) })
	go func() {
		time.Sleep(20 * time.Second)
		v1alpha1testing.NewMockSigner(t, socketPath)
	}()

	// Start Api server configured with external signer.
	client, _, tearDownFn := framework.StartTestServer(ctx, t, framework.TestServerSetup{
		ModifyServerRunOptions: func(opt *options.ServerRunOptions) {
			opt.ServiceAccountSigningEndpoint = socketPath
			opt.ServiceAccountSigningKeyFile = ""
			opt.Authentication.ServiceAccounts.KeyFiles = []string{}
		},
	})
	defer tearDownFn()

	// Create Namesapce (ns-1) to work with.
	if _, err := client.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "ns-1",
		},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("Error when creating namespace: %v", err)
	}

	// Create ServiceAccount (sa-1) to work with.
	if _, err := client.CoreV1().ServiceAccounts("ns-1").Create(ctx, &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name: "sa-1",
		},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("Error when creating service-account: %v", err)
	}

	// Request a token for ns-1:sa-1.
	tokenExpirationSec := int64(2 * 60 * 60) // 2h
	tokenRequest, err := client.CoreV1().ServiceAccounts("ns-1").CreateToken(ctx, "sa-1", &authv1.TokenRequest{
		Spec: authv1.TokenRequestSpec{
			ExpirationSeconds: &tokenExpirationSec,
		},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("Error when creating token: %v", err)
	}

	// Try Validating the token.
	tokenReviewResult, err := client.AuthenticationV1().TokenReviews().Create(ctx, &authv1.TokenReview{
		Spec: authv1.TokenReviewSpec{
			Token: tokenRequest.Status.Token,
		},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("Error when validating token: %v", err)
	}
	if !tokenReviewResult.Status.Authenticated {
		t.Fatal("Expected Authentication to succeed")
	}
}
