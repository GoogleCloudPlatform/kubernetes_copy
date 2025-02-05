/*
Copyright 2023 The Kubernetes Authors.

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

package oidc

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/go-jose/go-jose.v2"

	authenticationv1 "k8s.io/api/authentication/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilrand "k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apiserver/pkg/features"
	genericapiserver "k8s.io/apiserver/pkg/server"
	authenticationconfigmetrics "k8s.io/apiserver/pkg/server/options/authenticationconfig/metrics"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/apiserver/plugin/pkg/authenticator/token/oidc"
	"k8s.io/client-go/kubernetes"
	_ "k8s.io/client-go/plugin/pkg/client/auth/oidc"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd/api"
	certutil "k8s.io/client-go/util/cert"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	kubeapiserverapptesting "k8s.io/kubernetes/cmd/kube-apiserver/app/testing"
	"k8s.io/kubernetes/pkg/apis/rbac"
	"k8s.io/kubernetes/pkg/kubeapiserver/options"
	"k8s.io/kubernetes/pkg/util/slice"
	"k8s.io/kubernetes/test/integration/framework"
	utilsoidc "k8s.io/kubernetes/test/utils/oidc"
	"k8s.io/kubernetes/test/utils/oidc/handlers"
	utilsnet "k8s.io/utils/net"
)

const (
	defaultNamespace           = "default"
	defaultOIDCClientID        = "f403b682-603f-4ec9-b3e4-cf111ef36f7c"
	defaultOIDCClaimedUsername = "john_doe"
	defaultOIDCUsernamePrefix  = "k8s-"
	defaultRBACRoleName        = "developer-role"
	defaultRBACRoleBindingName = "developer-role-binding"

	defaultStubRefreshToken = "_fake_refresh_token_"
	defaultStubAccessToken  = "_fake_access_token_"

	rsaKeyBitSize = 2048
)

var (
	defaultRole = &rbacv1.Role{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "Role"},
		ObjectMeta: metav1.ObjectMeta{Name: defaultRBACRoleName},
		Rules: []rbacv1.PolicyRule{
			{
				Verbs:         []string{"list"},
				Resources:     []string{"pods"},
				APIGroups:     []string{""},
				ResourceNames: []string{},
			},
		},
	}
	defaultRoleBinding = &rbacv1.RoleBinding{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "RoleBinding"},
		ObjectMeta: metav1.ObjectMeta{Name: defaultRBACRoleBindingName},
		Subjects: []rbacv1.Subject{
			{
				APIGroup: rbac.GroupName,
				Kind:     rbacv1.UserKind,
				Name:     defaultOIDCUsernamePrefix + defaultOIDCClaimedUsername,
			},
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbac.GroupName,
			Kind:     "Role",
			Name:     defaultRBACRoleName,
		},
	}
)

// authenticationConfigFunc is a function that returns a string representation of an authentication config.
type authenticationConfigFunc func(t *testing.T, issuerURL, caCert string) string

type apiServerOIDCConfig struct {
	oidcURL                  string
	oidcClientID             string
	oidcCAFilePath           string
	oidcUsernamePrefix       string
	oidcUsernameClaim        string
	authenticationConfigYAML string
}

func TestOIDC(t *testing.T) {
	t.Log("Testing OIDC authenticator with --oidc-* flags")
	runTests(t, false)
}

func TestStructuredAuthenticationConfig(t *testing.T) {
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.StructuredAuthenticationConfiguration, true)

	t.Log("Testing OIDC authenticator with authentication config")
	runTests(t, true)
}

func runTests(t *testing.T, useAuthenticationConfig bool) {
	genericapiserver.SetHostnameFuncForTests("testAPIServerID")

	var tests = []singleTest[*rsa.PrivateKey, *rsa.PublicKey]{
		{
			name: "ID token is ok",
			configureInfrastructure: func(t *testing.T, fn authenticationConfigFunc, keyFunc func(t *testing.T) (*rsa.PrivateKey, *rsa.PublicKey)) (
				oidcServer *utilsoidc.TestServer,
				apiServer *kubeapiserverapptesting.TestServer,
				signingPrivateKey *rsa.PrivateKey,
				caCertContent []byte,
				caFilePath string,
			) {
				caCertContent, _, caFilePath, caKeyFilePath := generateCert(t)
				signingPrivateKey, publicKey := keyFunc(t)
				oidcServer = utilsoidc.BuildAndRunTestServer(t, caFilePath, caKeyFilePath, "")

				if useAuthenticationConfig {
					authenticationConfig := fmt.Sprintf(`
apiVersion: apiserver.config.k8s.io/v1beta1
kind: AuthenticationConfiguration
jwt:
- issuer:
    url: %s
    audiences:
    - %s
    certificateAuthority: |
        %s
  claimMappings:
    username:
      claim: user
      prefix: %s
`, oidcServer.URL(), defaultOIDCClientID, indentCertificateAuthority(string(caCertContent)), defaultOIDCUsernamePrefix)
					apiServer = startTestAPIServerForOIDC(t, apiServerOIDCConfig{authenticationConfigYAML: authenticationConfig}, &signingPrivateKey.PublicKey)
				} else {
					apiServer = startTestAPIServerForOIDC(t, apiServerOIDCConfig{oidcURL: oidcServer.URL(), oidcClientID: defaultOIDCClientID,
						oidcCAFilePath: caFilePath, oidcUsernamePrefix: defaultOIDCUsernamePrefix, oidcUsernameClaim: "user"}, &signingPrivateKey.PublicKey)
				}
				oidcServer.JwksHandler().EXPECT().KeySet().RunAndReturn(utilsoidc.DefaultJwksHandlerBehavior(t, publicKey)).Maybe()

				adminClient := kubernetes.NewForConfigOrDie(apiServer.ClientConfig)
				configureRBAC(t, adminClient, defaultRole, defaultRoleBinding)

				return oidcServer, apiServer, signingPrivateKey, caCertContent, caFilePath
			}, configureOIDCServerBehaviour: func(t *testing.T, oidcServer *utilsoidc.TestServer, signingPrivateKey *rsa.PrivateKey) {
				idTokenLifetime := time.Second * 1200
				oidcServer.TokenHandler().EXPECT().Token().RunAndReturn(utilsoidc.TokenHandlerBehaviorReturningPredefinedJWT(
					t,
					signingPrivateKey,
					// This asserts the minimum valid claims for an ID token required by the authenticator.
					// "iss", "aud", "exp" and a claim for the username.
					map[string]interface{}{
						"iss":  oidcServer.URL(),
						"user": defaultOIDCClaimedUsername,
						"aud":  defaultOIDCClientID,
						"exp":  time.Now().Add(idTokenLifetime).Unix(),
					},
					defaultStubAccessToken,
					defaultStubRefreshToken,
				)).Times(1)
			},
			configureClient: configureClientFetchingOIDCCredentials,
			assertErrFn: func(t *testing.T, errorToCheck error) {
				assert.NoError(t, errorToCheck)
			},
		},
		{
			name:                    "ID token is expired",
			configureInfrastructure: configureTestInfrastructure[*rsa.PrivateKey, *rsa.PublicKey],
			configureOIDCServerBehaviour: func(t *testing.T, oidcServer *utilsoidc.TestServer, signingPrivateKey *rsa.PrivateKey) {
				configureOIDCServerToReturnExpiredIDToken(t, 2, oidcServer, signingPrivateKey)
			},
			configureClient: configureClientFetchingOIDCCredentials,
			assertErrFn: func(t *testing.T, errorToCheck error) {
				assert.True(t, apierrors.IsUnauthorized(errorToCheck), errorToCheck)
			},
			skipJWKSMetricsCheck: true,
		},
		{
			name:                    "wrong client ID",
			configureInfrastructure: configureTestInfrastructure[*rsa.PrivateKey, *rsa.PublicKey],
			configureOIDCServerBehaviour: func(t *testing.T, oidcServer *utilsoidc.TestServer, _ *rsa.PrivateKey) {
				oidcServer.TokenHandler().EXPECT().Token().Times(2).Return(handlers.Token{}, utilsoidc.ErrBadClientID)
			},
			configureClient: configureClientWithEmptyIDToken,
			assertErrFn: func(t *testing.T, errorToCheck error) {
				var urlError *url.Error
				ok := errors.As(errorToCheck, &urlError)
				require.True(t, ok)
				assert.Equal(
					t,
					"failed to refresh token: oauth2: cannot fetch token: 400 Bad Request\nResponse: client ID is bad\n",
					urlError.Err.Error(),
				)
			},
			skipJWKSMetricsCheck: true,
		},
		{
			name:                         "client has wrong CA",
			configureInfrastructure:      configureTestInfrastructure[*rsa.PrivateKey, *rsa.PublicKey],
			configureOIDCServerBehaviour: func(t *testing.T, oidcServer *utilsoidc.TestServer, _ *rsa.PrivateKey) {},
			configureClient: func(t *testing.T, restCfg *rest.Config, caCert []byte, _, oidcServerURL, oidcServerTokenURL string) kubernetes.Interface {
				tempDir := t.TempDir()
				certFilePath := filepath.Join(tempDir, "localhost_127.0.0.1_.crt")

				_, _, wantErr := certutil.GenerateSelfSignedCertKeyWithFixtures("localhost", []net.IP{utilsnet.ParseIPSloppy("127.0.0.1")}, nil, tempDir)
				require.NoError(t, wantErr)

				return configureClientWithEmptyIDToken(t, restCfg, caCert, certFilePath, oidcServerURL, oidcServerTokenURL)
			},
			assertErrFn: func(t *testing.T, errorToCheck error) {
				expectedErr := new(x509.UnknownAuthorityError)
				assert.ErrorAs(t, errorToCheck, expectedErr)
			},
			skipJWKSMetricsCheck: true,
		},
		{
			name:                    "refresh flow does not return ID Token",
			configureInfrastructure: configureTestInfrastructure[*rsa.PrivateKey, *rsa.PublicKey],
			configureOIDCServerBehaviour: func(t *testing.T, oidcServer *utilsoidc.TestServer, signingPrivateKey *rsa.PrivateKey) {
				configureOIDCServerToReturnExpiredIDToken(t, 1, oidcServer, signingPrivateKey)
				oidcServer.TokenHandler().EXPECT().Token().Times(1).Return(handlers.Token{
					IDToken:      "",
					AccessToken:  defaultStubAccessToken,
					RefreshToken: defaultStubRefreshToken,
					ExpiresIn:    time.Now().Add(time.Second * 1200).Unix(),
				}, nil)
			},
			configureClient: configureClientFetchingOIDCCredentials,
			assertErrFn: func(t *testing.T, errorToCheck error) {
				expectedError := new(apierrors.StatusError)
				require.ErrorAs(t, errorToCheck, &expectedError)
				assert.Equal(
					t,
					`pods is forbidden: User "system:anonymous" cannot list resource "pods" in API group "" in the namespace "default"`,
					errorToCheck.Error(),
				)
			},
			skipJWKSMetricsCheck: true,
		},
		{
			name: "ID token signature can not be verified due to wrong JWKs",
			configureInfrastructure: func(t *testing.T, fn authenticationConfigFunc, keyFunc func(t *testing.T) (*rsa.PrivateKey, *rsa.PublicKey)) (
				oidcServer *utilsoidc.TestServer,
				apiServer *kubeapiserverapptesting.TestServer,
				signingPrivateKey *rsa.PrivateKey,
				caCertContent []byte,
				caFilePath string,
			) {
				caCertContent, _, caFilePath, caKeyFilePath := generateCert(t)

				signingPrivateKey, _ = keyFunc(t)

				oidcServer = utilsoidc.BuildAndRunTestServer(t, caFilePath, caKeyFilePath, "")

				if useAuthenticationConfig {
					authenticationConfig := fmt.Sprintf(`
apiVersion: apiserver.config.k8s.io/v1alpha1
kind: AuthenticationConfiguration
jwt:
- issuer:
    url: %s
    audiences:
    - %s
    certificateAuthority: |
        %s
  claimMappings:
    username:
      claim: sub
      prefix: %s
`, oidcServer.URL(), defaultOIDCClientID, indentCertificateAuthority(string(caCertContent)), defaultOIDCUsernamePrefix)
					apiServer = startTestAPIServerForOIDC(t, apiServerOIDCConfig{authenticationConfigYAML: authenticationConfig}, &signingPrivateKey.PublicKey)
				} else {
					apiServer = startTestAPIServerForOIDC(t, apiServerOIDCConfig{oidcURL: oidcServer.URL(), oidcClientID: defaultOIDCClientID, oidcCAFilePath: caFilePath, oidcUsernamePrefix: defaultOIDCUsernamePrefix}, &signingPrivateKey.PublicKey)
				}

				adminClient := kubernetes.NewForConfigOrDie(apiServer.ClientConfig)
				configureRBAC(t, adminClient, defaultRole, defaultRoleBinding)

				anotherSigningPrivateKey, _ := keyFunc(t)

				oidcServer.JwksHandler().EXPECT().KeySet().RunAndReturn(utilsoidc.DefaultJwksHandlerBehavior(t, &anotherSigningPrivateKey.PublicKey)).Maybe()

				return oidcServer, apiServer, signingPrivateKey, caCertContent, caFilePath
			},
			configureOIDCServerBehaviour: func(t *testing.T, oidcServer *utilsoidc.TestServer, signingPrivateKey *rsa.PrivateKey) {
				oidcServer.TokenHandler().EXPECT().Token().RunAndReturn(utilsoidc.TokenHandlerBehaviorReturningPredefinedJWT(
					t,
					signingPrivateKey,
					map[string]interface{}{
						"iss": oidcServer.URL(),
						"sub": defaultOIDCClaimedUsername,
						"aud": defaultOIDCClientID,
						"exp": time.Now().Add(time.Second * 1200).Unix(),
					},
					defaultStubAccessToken,
					defaultStubRefreshToken,
				)).Times(1)
			},
			configureClient: configureClientFetchingOIDCCredentials,
			assertErrFn: func(t *testing.T, errorToCheck error) {
				assert.True(t, apierrors.IsUnauthorized(errorToCheck), errorToCheck)
			},
		},
		{
			name: "ID token is okay but username is empty",
			configureInfrastructure: func(t *testing.T, fn authenticationConfigFunc, keyFunc func(t *testing.T) (*rsa.PrivateKey, *rsa.PublicKey)) (
				oidcServer *utilsoidc.TestServer,
				apiServer *kubeapiserverapptesting.TestServer,
				signingPrivateKey *rsa.PrivateKey,
				caCertContent []byte,
				caFilePath string,
			) {
				caCertContent, _, caFilePath, caKeyFilePath := generateCert(t)

				signingPrivateKey, _ = keyFunc(t)

				oidcServer = utilsoidc.BuildAndRunTestServer(t, caFilePath, caKeyFilePath, "")

				if useAuthenticationConfig {
					authenticationConfig := fmt.Sprintf(`
apiVersion: apiserver.config.k8s.io/v1alpha1
kind: AuthenticationConfiguration
jwt:
- issuer:
    url: %s
    audiences:
    - %s
    certificateAuthority: |
        %s
  claimMappings:
    username:
      expression: claims.sub
`, oidcServer.URL(), defaultOIDCClientID, indentCertificateAuthority(string(caCertContent)))
					apiServer = startTestAPIServerForOIDC(t, apiServerOIDCConfig{authenticationConfigYAML: authenticationConfig}, &signingPrivateKey.PublicKey)
				} else {
					apiServer = startTestAPIServerForOIDC(t, apiServerOIDCConfig{
						oidcURL: oidcServer.URL(), oidcClientID: defaultOIDCClientID, oidcCAFilePath: caFilePath, oidcUsernamePrefix: "-",
					},
						&signingPrivateKey.PublicKey)
				}

				oidcServer.JwksHandler().EXPECT().KeySet().RunAndReturn(utilsoidc.DefaultJwksHandlerBehavior(t, &signingPrivateKey.PublicKey)).Maybe()

				return oidcServer, apiServer, signingPrivateKey, caCertContent, caFilePath
			},
			configureOIDCServerBehaviour: func(t *testing.T, oidcServer *utilsoidc.TestServer, signingPrivateKey *rsa.PrivateKey) {
				oidcServer.TokenHandler().EXPECT().Token().RunAndReturn(utilsoidc.TokenHandlerBehaviorReturningPredefinedJWT(
					t,
					signingPrivateKey,
					map[string]interface{}{
						"iss": oidcServer.URL(),
						"sub": "",
						"aud": defaultOIDCClientID,
						"exp": time.Now().Add(time.Second * 1200).Unix(),
					},
					defaultStubAccessToken,
					defaultStubRefreshToken,
				)).Times(1)
			},
			configureClient: configureClientFetchingOIDCCredentials,
			assertErrFn: func(t *testing.T, errorToCheck error) {
				if useAuthenticationConfig { // since the config uses a CEL expression
					assert.True(t, apierrors.IsUnauthorized(errorToCheck), errorToCheck)
				} else {
					// the claim based approach is still allowed to use empty usernames
					_ = assert.True(t, apierrors.IsForbidden(errorToCheck), errorToCheck) &&
						assert.Equal(
							t,
							`pods is forbidden: User "" cannot list resource "pods" in API group "" in the namespace "default"`,
							errorToCheck.Error(),
						)
				}
			},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, singleTestRunner(useAuthenticationConfig, rsaGenerateKey, tt))
	}

	for _, tt := range []singleTest[*ecdsa.PrivateKey, *ecdsa.PublicKey]{
		{
			name:                    "ID token is ok",
			configureInfrastructure: configureTestInfrastructure[*ecdsa.PrivateKey, *ecdsa.PublicKey],
			configureOIDCServerBehaviour: func(t *testing.T, oidcServer *utilsoidc.TestServer, signingPrivateKey *ecdsa.PrivateKey) {
				idTokenLifetime := time.Second * 1200
				oidcServer.TokenHandler().EXPECT().Token().RunAndReturn(utilsoidc.TokenHandlerBehaviorReturningPredefinedJWT(
					t,
					signingPrivateKey,
					map[string]interface{}{
						"iss": oidcServer.URL(),
						"sub": defaultOIDCClaimedUsername,
						"aud": defaultOIDCClientID,
						"exp": time.Now().Add(idTokenLifetime).Unix(),
					},
					defaultStubAccessToken,
					defaultStubRefreshToken,
				)).Times(1)
			},
			configureClient: configureClientFetchingOIDCCredentials,
			assertErrFn: func(t *testing.T, errorToCheck error) {
				assert.NoError(t, errorToCheck)
			},
		},
	} {
		t.Run(tt.name, singleTestRunner(useAuthenticationConfig, ecdsaGenerateKey, tt))
	}
}

type singleTest[K utilsoidc.JosePrivateKey, L utilsoidc.JosePublicKey] struct {
	name                    string
	configureInfrastructure func(t *testing.T, fn authenticationConfigFunc, keyFunc func(t *testing.T) (K, L)) (
		oidcServer *utilsoidc.TestServer,
		apiServer *kubeapiserverapptesting.TestServer,
		signingPrivateKey K,
		caCertContent []byte,
		caFilePath string,
	)
	configureOIDCServerBehaviour func(t *testing.T, oidcServer *utilsoidc.TestServer, signingPrivateKey K)
	configureClient              func(
		t *testing.T,
		restCfg *rest.Config,
		caCert []byte,
		certPath,
		oidcServerURL,
		oidcServerTokenURL string,
	) kubernetes.Interface
	assertErrFn          func(t *testing.T, errorToCheck error)
	skipJWKSMetricsCheck bool
}

func singleTestRunner[K utilsoidc.JosePrivateKey, L utilsoidc.JosePublicKey](
	useAuthenticationConfig bool,
	keyFunc func(t *testing.T) (K, L),
	tt singleTest[K, L],
) func(t *testing.T) {
	return func(t *testing.T) {
		t.Parallel()

		fn := func(t *testing.T, issuerURL, caCert string) string { return "" }
		if useAuthenticationConfig {
			fn = func(t *testing.T, issuerURL, caCert string) string {
				return fmt.Sprintf(`
apiVersion: apiserver.config.k8s.io/v1alpha1
kind: AuthenticationConfiguration
jwt:
- issuer:
    url: %s
    audiences:
    - %s
    certificateAuthority: |
        %s
  claimMappings:
    username:
      claim: sub
      prefix: %s
`, issuerURL, defaultOIDCClientID, indentCertificateAuthority(caCert), defaultOIDCUsernamePrefix)
			}
		}

		oidc.ResetMetrics()
		oidcServer, apiServer, signingPrivateKey, caCert, certPath := tt.configureInfrastructure(t, fn, keyFunc)

		tt.configureOIDCServerBehaviour(t, oidcServer, signingPrivateKey)

		tokenURL, err := oidcServer.TokenURL()
		require.NoError(t, err)

		client := tt.configureClient(t, apiServer.ClientConfig, caCert, certPath, oidcServer.URL(), tokenURL)

		ctx := testContext(t)
		_, err = client.CoreV1().Pods(defaultNamespace).List(ctx, metav1.ListOptions{})

		tt.assertErrFn(t, err)

		if !tt.skipJWKSMetricsCheck && useAuthenticationConfig {
			jwtIssuerHash := fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(oidcServer.URL())))
			keySet := oidcServer.JwksHandler().KeySet()
			keySetBytes, err := json.Marshal(keySet)
			require.NoError(t, err)

			hf := fnv.New64()
			hf.Write(keySetBytes)

			wantMetricStrings := []string{
				fmt.Sprintf(`apiserver_authentication_jwt_authenticator_jwks_fetch_last_key_set_hash{apiserver_id_hash="sha256:3c607df3b2bf22c9d9f01d5314b4bbf411c48ef43ff44ff29b1d55b41367c795",jwt_issuer_hash="%s"} %g`, jwtIssuerHash, float64(hf.Sum64())),
				fmt.Sprintf(`apiserver_authentication_jwt_authenticator_jwks_fetch_last_timestamp_seconds{apiserver_id_hash="sha256:3c607df3b2bf22c9d9f01d5314b4bbf411c48ef43ff44ff29b1d55b41367c795",jwt_issuer_hash="%s",result="success"} FP`, jwtIssuerHash),
			}
			adminClient := kubernetes.NewForConfigOrDie(apiServer.ClientConfig)
			body, err := adminClient.RESTClient().Get().AbsPath("/metrics").DoRaw(ctx)
			require.NoError(t, err)
			var gotMetricStrings []string
			trimFP := regexp.MustCompile(`(.*)(} \d+\.\d+.*)`)
			for _, line := range strings.Split(string(body), "\n") {
				if strings.HasPrefix(line, "apiserver_authentication_jwt_authenticator_jwks_") {
					if strings.Contains(line, "_seconds") {
						line = trimFP.ReplaceAllString(line, `$1`) + "} FP" // ignore floating point metric values
					}

					gotMetricStrings = append(gotMetricStrings, line)
				}
			}
			if diff := cmp.Diff(wantMetricStrings, gotMetricStrings); diff != "" {
				t.Errorf("unexpected metrics diff (-want +got): %s", diff)
			}
		}
	}
}

func TestUpdatingRefreshTokenInCaseOfExpiredIDToken(t *testing.T) {
	t.Parallel()

	type testRun[K utilsoidc.JosePrivateKey] struct {
		name                            string
		configureUpdatingTokenBehaviour func(t *testing.T, oidcServer *utilsoidc.TestServer, signingPrivateKey K)
		assertErrFn                     func(t *testing.T, errorToCheck error)
	}

	var tests = []testRun[*rsa.PrivateKey]{
		{
			name: "cache returns stale client if refresh token is not updated in config",
			configureUpdatingTokenBehaviour: func(t *testing.T, oidcServer *utilsoidc.TestServer, signingPrivateKey *rsa.PrivateKey) {
				oidcServer.TokenHandler().EXPECT().Token().RunAndReturn(utilsoidc.TokenHandlerBehaviorReturningPredefinedJWT(
					t,
					signingPrivateKey,
					map[string]interface{}{
						"iss": oidcServer.URL(),
						"sub": defaultOIDCClaimedUsername,
						"aud": defaultOIDCClientID,
						"exp": time.Now().Add(time.Second * 1200).Unix(),
					},
					defaultStubAccessToken,
					defaultStubRefreshToken,
				)).Times(1)
				configureOIDCServerToReturnExpiredRefreshTokenErrorOnTryingToUpdateIDToken(oidcServer)
			},
			assertErrFn: func(t *testing.T, errorToCheck error) {
				urlError, ok := errorToCheck.(*url.Error)
				require.True(t, ok)
				assert.Equal(
					t,
					"failed to refresh token: oauth2: cannot fetch token: 400 Bad Request\nResponse: refresh token is expired\n",
					urlError.Err.Error(),
				)
			},
		},
	}

	oidcServer, apiServer, signingPrivateKey, caCert, certPath := configureTestInfrastructure(t, func(t *testing.T, _, _ string) string { return "" }, rsaGenerateKey)

	tokenURL, err := oidcServer.TokenURL()
	require.NoError(t, err)

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			expiredIDToken, stubRefreshToken := fetchExpiredToken(t, oidcServer, caCert, signingPrivateKey)
			clientConfig := configureClientConfigForOIDC(t, apiServer.ClientConfig, defaultOIDCClientID, certPath, expiredIDToken, stubRefreshToken, oidcServer.URL())
			expiredClient := kubernetes.NewForConfigOrDie(clientConfig)
			configureOIDCServerToReturnExpiredRefreshTokenErrorOnTryingToUpdateIDToken(oidcServer)

			ctx := testContext(t)
			_, err = expiredClient.CoreV1().Pods(defaultNamespace).List(ctx, metav1.ListOptions{})
			require.Error(t, err)

			tt.configureUpdatingTokenBehaviour(t, oidcServer, signingPrivateKey)
			idToken, stubRefreshToken := fetchOIDCCredentials(t, tokenURL, caCert)
			clientConfig = configureClientConfigForOIDC(t, apiServer.ClientConfig, defaultOIDCClientID, certPath, idToken, stubRefreshToken, oidcServer.URL())
			expectedOkClient := kubernetes.NewForConfigOrDie(clientConfig)
			_, err = expectedOkClient.CoreV1().Pods(defaultNamespace).List(ctx, metav1.ListOptions{})

			tt.assertErrFn(t, err)
		})
	}
}

func TestStructuredAuthenticationConfigCEL(t *testing.T) {
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.StructuredAuthenticationConfiguration, true)

	type testRun[K utilsoidc.JosePrivateKey, L utilsoidc.JosePublicKey] struct {
		name                    string
		authConfigFn            authenticationConfigFunc
		configureInfrastructure func(t *testing.T, fn authenticationConfigFunc, keyFunc func(t *testing.T) (K, L)) (
			oidcServer *utilsoidc.TestServer,
			apiServer *kubeapiserverapptesting.TestServer,
			signingPrivateKey *rsa.PrivateKey,
			caCertContent []byte,
			caFilePath string,
		)
		configureOIDCServerBehaviour func(t *testing.T, oidcServer *utilsoidc.TestServer, signingPrivateKey K)
		configureClient              func(
			t *testing.T,
			restCfg *rest.Config,
			caCert []byte,
			certPath,
			oidcServerURL,
			oidcServerTokenURL string,
		) kubernetes.Interface
		assertErrFn func(t *testing.T, errorToCheck error)
		wantUser    *authenticationv1.UserInfo
	}

	tests := []testRun[*rsa.PrivateKey, *rsa.PublicKey]{
		{
			name: "username CEL expression is ok",
			authConfigFn: func(t *testing.T, issuerURL, caCert string) string {
				return fmt.Sprintf(`
apiVersion: apiserver.config.k8s.io/v1alpha1
kind: AuthenticationConfiguration
jwt:
- issuer:
    url: %s
    audiences:
    - %s
    - another-audience
    audienceMatchPolicy: MatchAny
    certificateAuthority: |
        %s
  claimMappings:
    username:
      expression: "'k8s-' + claims.sub"
`, issuerURL, defaultOIDCClientID, indentCertificateAuthority(caCert))
			},
			configureInfrastructure: configureTestInfrastructure[*rsa.PrivateKey, *rsa.PublicKey],
			configureOIDCServerBehaviour: func(t *testing.T, oidcServer *utilsoidc.TestServer, signingPrivateKey *rsa.PrivateKey) {
				idTokenLifetime := time.Second * 1200
				oidcServer.TokenHandler().EXPECT().Token().RunAndReturn(utilsoidc.TokenHandlerBehaviorReturningPredefinedJWT(
					t,
					signingPrivateKey,
					map[string]interface{}{
						"iss": oidcServer.URL(),
						"sub": defaultOIDCClaimedUsername,
						"aud": defaultOIDCClientID,
						"exp": time.Now().Add(idTokenLifetime).Unix(),
						"jti": "0123456789",
					},
					defaultStubAccessToken,
					defaultStubRefreshToken,
				)).Times(1)
			},
			configureClient: configureClientFetchingOIDCCredentials,
			assertErrFn: func(t *testing.T, errorToCheck error) {
				assert.NoError(t, errorToCheck)
			},
			wantUser: &authenticationv1.UserInfo{
				Username: "k8s-john_doe",
				Groups:   []string{"system:authenticated"},
				Extra: map[string]authenticationv1.ExtraValue{
					// validates credential id is set correctly when jti claim is present
					"authentication.kubernetes.io/credential-id": {"JTI=0123456789"},
				},
			},
		},
		{
			name: "groups CEL expression is ok",
			authConfigFn: func(t *testing.T, issuerURL, caCert string) string {
				return fmt.Sprintf(`
apiVersion: apiserver.config.k8s.io/v1alpha1
kind: AuthenticationConfiguration
jwt:
- issuer:
    url: %s
    audiences:
    - %s
    - another-audience
    audienceMatchPolicy: MatchAny
    certificateAuthority: |
        %s
  claimMappings:
    username:
      expression: "'k8s-' + claims.sub"
    groups:
      expression: '(claims.roles.split(",") + claims.other_roles.split(",")).map(role, "prefix:" + role)'
`, issuerURL, defaultOIDCClientID, indentCertificateAuthority(caCert))
			},
			configureInfrastructure: configureTestInfrastructure[*rsa.PrivateKey, *rsa.PublicKey],
			configureOIDCServerBehaviour: func(t *testing.T, oidcServer *utilsoidc.TestServer, signingPrivateKey *rsa.PrivateKey) {
				idTokenLifetime := time.Second * 1200
				oidcServer.TokenHandler().EXPECT().Token().RunAndReturn(utilsoidc.TokenHandlerBehaviorReturningPredefinedJWT(
					t,
					signingPrivateKey,
					map[string]interface{}{
						"iss":         oidcServer.URL(),
						"sub":         defaultOIDCClaimedUsername,
						"aud":         defaultOIDCClientID,
						"exp":         time.Now().Add(idTokenLifetime).Unix(),
						"roles":       "foo,bar",
						"other_roles": "baz,qux",
					},
					defaultStubAccessToken,
					defaultStubRefreshToken,
				)).Times(1)
			},
			configureClient: configureClientFetchingOIDCCredentials,
			assertErrFn: func(t *testing.T, errorToCheck error) {
				assert.NoError(t, errorToCheck)
			},
			wantUser: &authenticationv1.UserInfo{
				Username: "k8s-john_doe",
				Groups:   []string{"prefix:foo", "prefix:bar", "prefix:baz", "prefix:qux", "system:authenticated"},
			},
		},
		{
			name: "claim validation rule fails",
			authConfigFn: func(t *testing.T, issuerURL, caCert string) string {
				return fmt.Sprintf(`
apiVersion: apiserver.config.k8s.io/v1alpha1
kind: AuthenticationConfiguration
jwt:
- issuer:
    url: %s
    audiences:
    - %s
    - another-audience
    audienceMatchPolicy: MatchAny
    certificateAuthority: |
        %s
  claimMappings:
    username:
      expression: "'k8s-' + claims.sub"
  claimValidationRules:
  - expression: 'claims.hd == "example.com"'
    message: "the hd claim must be set to example.com"
`, issuerURL, defaultOIDCClientID, indentCertificateAuthority(caCert))
			},
			configureInfrastructure: configureTestInfrastructure[*rsa.PrivateKey, *rsa.PublicKey],
			configureOIDCServerBehaviour: func(t *testing.T, oidcServer *utilsoidc.TestServer, signingPrivateKey *rsa.PrivateKey) {
				idTokenLifetime := time.Second * 1200
				oidcServer.TokenHandler().EXPECT().Token().RunAndReturn(utilsoidc.TokenHandlerBehaviorReturningPredefinedJWT(
					t,
					signingPrivateKey,
					map[string]interface{}{
						"iss": oidcServer.URL(),
						"sub": defaultOIDCClaimedUsername,
						"aud": defaultOIDCClientID,
						"exp": time.Now().Add(idTokenLifetime).Unix(),
						"hd":  "notexample.com",
					},
					defaultStubAccessToken,
					defaultStubRefreshToken,
				)).Times(1)
			},
			configureClient: configureClientFetchingOIDCCredentials,
			assertErrFn: func(t *testing.T, errorToCheck error) {
				assert.True(t, apierrors.IsUnauthorized(errorToCheck), errorToCheck)
			},
		},
		{
			name: "extra mapping CEL expressions are ok",
			authConfigFn: func(t *testing.T, issuerURL, caCert string) string {
				return fmt.Sprintf(`
apiVersion: apiserver.config.k8s.io/v1alpha1
kind: AuthenticationConfiguration
jwt:
- issuer:
    url: %s
    audiences:
    - %s
    - another-audience
    audienceMatchPolicy: MatchAny
    certificateAuthority: |
        %s
  claimMappings:
    username:
      expression: "'k8s-' + claims.sub"
    extra:
    - key: "example.org/foo"
      valueExpression: "'bar'"
    - key: "example.org/baz"
      valueExpression: "claims.baz"
  userValidationRules:
  - expression: "'bar' in user.extra['example.org/foo'] && 'qux' in user.extra['example.org/baz']"
    message: "example.org/foo must be bar and example.org/baz must be qux"
`, issuerURL, defaultOIDCClientID, indentCertificateAuthority(caCert))
			},
			configureInfrastructure: configureTestInfrastructure[*rsa.PrivateKey, *rsa.PublicKey],
			configureOIDCServerBehaviour: func(t *testing.T, oidcServer *utilsoidc.TestServer, signingPrivateKey *rsa.PrivateKey) {
				idTokenLifetime := time.Second * 1200
				oidcServer.TokenHandler().EXPECT().Token().RunAndReturn(utilsoidc.TokenHandlerBehaviorReturningPredefinedJWT(
					t,
					signingPrivateKey,
					map[string]interface{}{
						"iss": oidcServer.URL(),
						"sub": defaultOIDCClaimedUsername,
						"aud": defaultOIDCClientID,
						"exp": time.Now().Add(idTokenLifetime).Unix(),
						"baz": "qux",
						"jti": "0123456789",
					},
					defaultStubAccessToken,
					defaultStubRefreshToken,
				)).Times(1)
			},
			configureClient: configureClientFetchingOIDCCredentials,
			assertErrFn: func(t *testing.T, errorToCheck error) {
				assert.NoError(t, errorToCheck)
			},
			wantUser: &authenticationv1.UserInfo{
				Username: "k8s-john_doe",
				Groups:   []string{"system:authenticated"},
				Extra: map[string]authenticationv1.ExtraValue{
					// validates credential id is set correctly and other extra fields are set
					"authentication.kubernetes.io/credential-id": {"JTI=0123456789"},
					"example.org/foo": {"bar"},
					"example.org/baz": {"qux"},
				},
			},
		},
		{
			name: "uid CEL expression is ok",
			authConfigFn: func(t *testing.T, issuerURL, caCert string) string {
				return fmt.Sprintf(`
apiVersion: apiserver.config.k8s.io/v1alpha1
kind: AuthenticationConfiguration
jwt:
- issuer:
    url: %s
    audiences:
    - %s
    - another-audience
    audienceMatchPolicy: MatchAny
    certificateAuthority: |
        %s
  claimMappings:
    username:
      expression: "'k8s-' + claims.sub"
    uid:
      expression: "claims.uid"
`, issuerURL, defaultOIDCClientID, indentCertificateAuthority(caCert))
			},
			configureInfrastructure: configureTestInfrastructure[*rsa.PrivateKey, *rsa.PublicKey],
			configureOIDCServerBehaviour: func(t *testing.T, oidcServer *utilsoidc.TestServer, signingPrivateKey *rsa.PrivateKey) {
				idTokenLifetime := time.Second * 1200
				oidcServer.TokenHandler().EXPECT().Token().RunAndReturn(utilsoidc.TokenHandlerBehaviorReturningPredefinedJWT(
					t,
					signingPrivateKey,
					map[string]interface{}{
						"iss": oidcServer.URL(),
						"sub": defaultOIDCClaimedUsername,
						"aud": defaultOIDCClientID,
						"exp": time.Now().Add(idTokenLifetime).Unix(),
						"uid": "1234",
					},
					defaultStubAccessToken,
					defaultStubRefreshToken,
				)).Times(1)
			},
			configureClient: configureClientFetchingOIDCCredentials,
			assertErrFn: func(t *testing.T, errorToCheck error) {
				assert.NoError(t, errorToCheck)
			},
			wantUser: &authenticationv1.UserInfo{
				Username: "k8s-john_doe",
				Groups:   []string{"system:authenticated"},
				UID:      "1234",
			},
		},
		{
			name: "user validation rule fails",
			authConfigFn: func(t *testing.T, issuerURL, caCert string) string {
				return fmt.Sprintf(`
apiVersion: apiserver.config.k8s.io/v1alpha1
kind: AuthenticationConfiguration
jwt:
- issuer:
    url: %s
    audiences:
    - %s
    - another-audience
    audienceMatchPolicy: MatchAny
    certificateAuthority: |
        %s
  claimMappings:
    username:
      expression: "'k8s-' + claims.sub"
    groups:
      expression: '(claims.roles.split(",") + claims.other_roles.split(",")).map(role, "system:" + role)'
  userValidationRules:
  - expression: "user.groups.all(group, !group.startsWith('system:'))"
    message: "groups cannot used reserved system: prefix"
`, issuerURL, defaultOIDCClientID, indentCertificateAuthority(caCert))
			},
			configureInfrastructure: configureTestInfrastructure[*rsa.PrivateKey, *rsa.PublicKey],
			configureOIDCServerBehaviour: func(t *testing.T, oidcServer *utilsoidc.TestServer, signingPrivateKey *rsa.PrivateKey) {
				idTokenLifetime := time.Second * 1200
				oidcServer.TokenHandler().EXPECT().Token().RunAndReturn(utilsoidc.TokenHandlerBehaviorReturningPredefinedJWT(
					t,
					signingPrivateKey,
					map[string]interface{}{
						"iss":         oidcServer.URL(),
						"sub":         defaultOIDCClaimedUsername,
						"aud":         defaultOIDCClientID,
						"exp":         time.Now().Add(idTokenLifetime).Unix(),
						"roles":       "foo,bar",
						"other_roles": "baz,qux",
					},
					defaultStubAccessToken,
					defaultStubRefreshToken,
				)).Times(1)
			},
			configureClient: configureClientFetchingOIDCCredentials,
			assertErrFn: func(t *testing.T, errorToCheck error) {
				assert.True(t, apierrors.IsUnauthorized(errorToCheck), errorToCheck)
			},
			wantUser: nil,
		},
		{
			name: "multiple audiences check with claim validation rule is ok",
			authConfigFn: func(t *testing.T, issuerURL, caCert string) string {
				return fmt.Sprintf(`
apiVersion: apiserver.config.k8s.io/v1alpha1
kind: AuthenticationConfiguration
jwt:
- issuer:
    url: %s
    audiences:
    - baz
    - foo
    audienceMatchPolicy: MatchAny
    certificateAuthority: |
        %s
  claimMappings:
    username:
      expression: "'k8s-' + claims.sub"
    uid:
      expression: "claims.uid"
  claimValidationRules:
  - expression: 'sets.equivalent(claims.aud, ["bar", "foo", "baz"])'
    message: 'aud claim must be exactly match list ["bar", "foo", "baz"]'
`, issuerURL, indentCertificateAuthority(caCert))
			},
			configureInfrastructure: configureTestInfrastructure[*rsa.PrivateKey, *rsa.PublicKey],
			configureOIDCServerBehaviour: func(t *testing.T, oidcServer *utilsoidc.TestServer, signingPrivateKey *rsa.PrivateKey) {
				idTokenLifetime := time.Second * 1200
				oidcServer.TokenHandler().EXPECT().Token().RunAndReturn(utilsoidc.TokenHandlerBehaviorReturningPredefinedJWT(
					t,
					signingPrivateKey,
					map[string]interface{}{
						"iss": oidcServer.URL(),
						"sub": defaultOIDCClaimedUsername,
						"aud": []string{"foo", "bar", "baz"},
						"exp": time.Now().Add(idTokenLifetime).Unix(),
						"uid": "1234",
					},
					defaultStubAccessToken,
					defaultStubRefreshToken,
				)).Times(1)
			},
			configureClient: configureClientFetchingOIDCCredentials,
			assertErrFn: func(t *testing.T, errorToCheck error) {
				assert.NoError(t, errorToCheck)
			},
			wantUser: &authenticationv1.UserInfo{
				Username: "k8s-john_doe",
				Groups:   []string{"system:authenticated"},
				UID:      "1234",
			},
		},
		{
			name: "non-string jti claim doesn't result in authentication error",
			authConfigFn: func(t *testing.T, issuerURL, caCert string) string {
				return fmt.Sprintf(`
apiVersion: apiserver.config.k8s.io/v1alpha1
kind: AuthenticationConfiguration
jwt:
- issuer:
    url: %s
    audiences:
    - %s
    - another-audience
    audienceMatchPolicy: MatchAny
    certificateAuthority: |
        %s
  claimMappings:
    username:
      expression: "'k8s-' + claims.sub"
`, issuerURL, defaultOIDCClientID, indentCertificateAuthority(caCert))
			},
			configureInfrastructure: configureTestInfrastructure[*rsa.PrivateKey, *rsa.PublicKey],
			configureOIDCServerBehaviour: func(t *testing.T, oidcServer *utilsoidc.TestServer, signingPrivateKey *rsa.PrivateKey) {
				idTokenLifetime := time.Second * 1200
				oidcServer.TokenHandler().EXPECT().Token().RunAndReturn(utilsoidc.TokenHandlerBehaviorReturningPredefinedJWT(
					t,
					signingPrivateKey,
					map[string]interface{}{
						"iss": oidcServer.URL(),
						"sub": defaultOIDCClaimedUsername,
						"aud": defaultOIDCClientID,
						"exp": time.Now().Add(idTokenLifetime).Unix(),
						"jti": 1234,
					},
					defaultStubAccessToken,
					defaultStubRefreshToken,
				)).Times(1)
			},
			configureClient: configureClientFetchingOIDCCredentials,
			assertErrFn: func(t *testing.T, errorToCheck error) {
				assert.NoError(t, errorToCheck)
			},
			wantUser: &authenticationv1.UserInfo{
				Username: "k8s-john_doe",
				Groups:   []string{"system:authenticated"},
			},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			oidcServer, apiServer, signingPrivateKey, caCert, certPath := tt.configureInfrastructure(t, tt.authConfigFn, rsaGenerateKey)

			tt.configureOIDCServerBehaviour(t, oidcServer, signingPrivateKey)

			tokenURL, err := oidcServer.TokenURL()
			require.NoError(t, err)

			client := tt.configureClient(t, apiServer.ClientConfig, caCert, certPath, oidcServer.URL(), tokenURL)

			ctx := testContext(t)

			if tt.wantUser != nil {
				res, err := client.AuthenticationV1().SelfSubjectReviews().Create(ctx, &authenticationv1.SelfSubjectReview{}, metav1.CreateOptions{})
				require.NoError(t, err)
				assert.Equal(t, *tt.wantUser, res.Status.UserInfo)
			}

			_, err = client.CoreV1().Pods(defaultNamespace).List(ctx, metav1.ListOptions{})
			tt.assertErrFn(t, err)
		})
	}
}

func TestStructuredAuthenticationConfigReload(t *testing.T) {
	genericapiserver.SetHostnameFuncForTests("testAPIServerID")
	const hardCodedTokenCacheTTLAndPollInterval = 10 * time.Second

	origUpdateAuthenticationConfigTimeout := options.UpdateAuthenticationConfigTimeout
	t.Cleanup(func() { options.UpdateAuthenticationConfigTimeout = origUpdateAuthenticationConfigTimeout })
	options.UpdateAuthenticationConfigTimeout = 2 * hardCodedTokenCacheTTLAndPollInterval // needs to be large enough for polling to run multiple times

	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.StructuredAuthenticationConfiguration, true)

	tests := []struct {
		name                          string
		authConfigFn, newAuthConfigFn authenticationConfigFunc
		assertErrFn, newAssertErrFn   func(t *testing.T, errorToCheck error)
		wantUser, newWantUser         *authenticationv1.UserInfo
		ignoreTransitionErrFn         func(error) bool
		waitAfterConfigSwap           bool
		wantMetricStrings             []string
	}{
		{
			name: "old valid config to new valid config",
			authConfigFn: func(t *testing.T, issuerURL, caCert string) string {
				return fmt.Sprintf(`
apiVersion: apiserver.config.k8s.io/v1alpha1
kind: AuthenticationConfiguration
jwt:
- issuer:
    url: %s
    audiences:
    - %s
    - another-audience
    audienceMatchPolicy: MatchAny
    certificateAuthority: |
        %s
  claimMappings:
    username:
      expression: "'k8s-' + claims.sub"
`, issuerURL, defaultOIDCClientID, indentCertificateAuthority(caCert))
			},
			newAuthConfigFn: func(t *testing.T, issuerURL, caCert string) string {
				return fmt.Sprintf(`
apiVersion: apiserver.config.k8s.io/v1alpha1
kind: AuthenticationConfiguration
jwt:
- issuer:
    url: %s
    audiences:
    - %s
    - another-audience
    audienceMatchPolicy: MatchAny
    certificateAuthority: |
        %s
  claimMappings:
    username:
      expression: "'panda-' + claims.sub"   # this is the only new part of the config
`, issuerURL, defaultOIDCClientID, indentCertificateAuthority(caCert))
			},
			assertErrFn: func(t *testing.T, errorToCheck error) {
				assert.NoError(t, errorToCheck)
			},
			wantUser: &authenticationv1.UserInfo{
				Username: "k8s-john_doe",
				Groups:   []string{"system:authenticated"},
			},
			newAssertErrFn: func(t *testing.T, errorToCheck error) {
				_ = assert.True(t, apierrors.IsForbidden(errorToCheck)) &&
					assert.Equal(
						t,
						`pods is forbidden: User "panda-john_doe" cannot list resource "pods" in API group "" in the namespace "default"`,
						errorToCheck.Error(),
					)
			},
			newWantUser: &authenticationv1.UserInfo{
				Username: "panda-john_doe",
				Groups:   []string{"system:authenticated"},
			},
			wantMetricStrings: []string{
				`apiserver_authentication_config_controller_automatic_reload_last_timestamp_seconds{apiserver_id_hash="sha256:3c607df3b2bf22c9d9f01d5314b4bbf411c48ef43ff44ff29b1d55b41367c795",status="success"} FP`,
				`apiserver_authentication_config_controller_automatic_reloads_total{apiserver_id_hash="sha256:3c607df3b2bf22c9d9f01d5314b4bbf411c48ef43ff44ff29b1d55b41367c795",status="success"} 1`,
			},
		},
		{
			name: "old empty config to new valid config",
			authConfigFn: func(t *testing.T, _, _ string) string {
				return `
apiVersion: apiserver.config.k8s.io/v1alpha1
kind: AuthenticationConfiguration
`
			},
			newAuthConfigFn: func(t *testing.T, issuerURL, caCert string) string {
				return fmt.Sprintf(`
apiVersion: apiserver.config.k8s.io/v1alpha1
kind: AuthenticationConfiguration
jwt:
- issuer:
    url: %s
    audiences:
    - %s
    - another-audience
    audienceMatchPolicy: MatchAny
    certificateAuthority: |
        %s
  claimMappings:
    username:
      expression: "'snorlax-' + claims.sub"
`, issuerURL, defaultOIDCClientID, indentCertificateAuthority(caCert))
			},
			assertErrFn: func(t *testing.T, errorToCheck error) {
				assert.True(t, apierrors.IsUnauthorized(errorToCheck))
			},
			wantUser:              nil,
			ignoreTransitionErrFn: apierrors.IsUnauthorized,
			newAssertErrFn: func(t *testing.T, errorToCheck error) {
				_ = assert.True(t, apierrors.IsForbidden(errorToCheck)) &&
					assert.Equal(
						t,
						`pods is forbidden: User "snorlax-john_doe" cannot list resource "pods" in API group "" in the namespace "default"`,
						errorToCheck.Error(),
					)
			},
			newWantUser: &authenticationv1.UserInfo{
				Username: "snorlax-john_doe",
				Groups:   []string{"system:authenticated"},
			},
			wantMetricStrings: []string{
				`apiserver_authentication_config_controller_automatic_reload_last_timestamp_seconds{apiserver_id_hash="sha256:3c607df3b2bf22c9d9f01d5314b4bbf411c48ef43ff44ff29b1d55b41367c795",status="success"} FP`,
				`apiserver_authentication_config_controller_automatic_reloads_total{apiserver_id_hash="sha256:3c607df3b2bf22c9d9f01d5314b4bbf411c48ef43ff44ff29b1d55b41367c795",status="success"} 1`,
			},
		},
		{
			name: "old invalid config to new valid config",
			authConfigFn: func(t *testing.T, issuerURL, _ string) string {
				return fmt.Sprintf(`
apiVersion: apiserver.config.k8s.io/v1alpha1
kind: AuthenticationConfiguration
jwt:
- issuer:
    url: %s
    audiences:
    - %s
    - another-audience
    audienceMatchPolicy: MatchAny
    certificateAuthority: ""  # missing CA
  claimMappings:
    username:
      expression: "'k8s-' + claims.sub"
`, issuerURL, defaultOIDCClientID)
			},
			newAuthConfigFn: func(t *testing.T, issuerURL, caCert string) string {
				return fmt.Sprintf(`
apiVersion: apiserver.config.k8s.io/v1alpha1
kind: AuthenticationConfiguration
jwt:
- issuer:
    url: %s
    audiences:
    - %s
    - another-audience
    audienceMatchPolicy: MatchAny
    # this is the only new part of the config
    certificateAuthority: |
        %s
  claimMappings:
    username:
      expression: "'k8s-' + claims.sub"
`, issuerURL, defaultOIDCClientID, indentCertificateAuthority(caCert))
			},
			assertErrFn: func(t *testing.T, errorToCheck error) {
				assert.True(t, apierrors.IsUnauthorized(errorToCheck))
			},
			wantUser:              nil,
			ignoreTransitionErrFn: apierrors.IsUnauthorized,
			newAssertErrFn: func(t *testing.T, errorToCheck error) {
				assert.NoError(t, errorToCheck)
			},
			newWantUser: &authenticationv1.UserInfo{
				Username: "k8s-john_doe",
				Groups:   []string{"system:authenticated"},
			},
			wantMetricStrings: []string{
				`apiserver_authentication_config_controller_automatic_reload_last_timestamp_seconds{apiserver_id_hash="sha256:3c607df3b2bf22c9d9f01d5314b4bbf411c48ef43ff44ff29b1d55b41367c795",status="success"} FP`,
				`apiserver_authentication_config_controller_automatic_reloads_total{apiserver_id_hash="sha256:3c607df3b2bf22c9d9f01d5314b4bbf411c48ef43ff44ff29b1d55b41367c795",status="success"} 1`,
			},
		},
		{
			name: "old valid config to new structurally invalid config (should be ignored)",
			authConfigFn: func(t *testing.T, issuerURL, caCert string) string {
				return fmt.Sprintf(`
apiVersion: apiserver.config.k8s.io/v1alpha1
kind: AuthenticationConfiguration
jwt:
- issuer:
    url: %s
    audiences:
    - %s
    - another-audience
    audienceMatchPolicy: MatchAny
    certificateAuthority: |
        %s
  claimMappings:
    username:
      expression: "'k8s-' + claims.sub"
`, issuerURL, defaultOIDCClientID, indentCertificateAuthority(caCert))
			},
			newAuthConfigFn: func(t *testing.T, issuerURL, caCert string) string {
				return fmt.Sprintf(`
apiVersion: apiserver.config.k8s.io/v1alpha1
kind: AuthenticationConfiguration
jwt:
- issuer:
    url: %s
    audiences:
    - %s
    - another-audience
    audienceMatchPolicy: MatchAny
    certificateAuthority: |
        %s
  claimMappings:
    username:
      expression: "'k8s-' + claimss.sub"  # has typo
`, issuerURL, defaultOIDCClientID, indentCertificateAuthority(caCert))
			},
			assertErrFn: func(t *testing.T, errorToCheck error) {
				assert.NoError(t, errorToCheck)
			},
			wantUser: &authenticationv1.UserInfo{
				Username: "k8s-john_doe",
				Groups:   []string{"system:authenticated"},
			},
			newAssertErrFn: func(t *testing.T, errorToCheck error) {
				assert.NoError(t, errorToCheck)
			},
			newWantUser: &authenticationv1.UserInfo{
				Username: "k8s-john_doe",
				Groups:   []string{"system:authenticated"},
			},
			waitAfterConfigSwap: true,
			wantMetricStrings: []string{
				`apiserver_authentication_config_controller_automatic_reload_last_timestamp_seconds{apiserver_id_hash="sha256:3c607df3b2bf22c9d9f01d5314b4bbf411c48ef43ff44ff29b1d55b41367c795",status="failure"} FP`,
				`apiserver_authentication_config_controller_automatic_reloads_total{apiserver_id_hash="sha256:3c607df3b2bf22c9d9f01d5314b4bbf411c48ef43ff44ff29b1d55b41367c795",status="failure"} 1`,
			},
		},
		{
			name: "old valid config to new valid empty config (should cause tokens to stop working)",
			authConfigFn: func(t *testing.T, issuerURL, caCert string) string {
				return fmt.Sprintf(`
apiVersion: apiserver.config.k8s.io/v1alpha1
kind: AuthenticationConfiguration
jwt:
- issuer:
    url: %s
    audiences:
    - %s
    - another-audience
    audienceMatchPolicy: MatchAny
    certificateAuthority: |
        %s
  claimMappings:
    username:
      expression: "'k8s-' + claims.sub"
`, issuerURL, defaultOIDCClientID, indentCertificateAuthority(caCert))
			},
			newAuthConfigFn: func(t *testing.T, _, _ string) string {
				return `
apiVersion: apiserver.config.k8s.io/v1alpha1
kind: AuthenticationConfiguration
`
			},
			assertErrFn: func(t *testing.T, errorToCheck error) {
				assert.NoError(t, errorToCheck)
			},
			wantUser: &authenticationv1.UserInfo{
				Username: "k8s-john_doe",
				Groups:   []string{"system:authenticated"},
			},
			newAssertErrFn: func(t *testing.T, errorToCheck error) {
				assert.True(t, apierrors.IsUnauthorized(errorToCheck))
			},
			newWantUser:         nil,
			waitAfterConfigSwap: true,
			wantMetricStrings: []string{
				`apiserver_authentication_config_controller_automatic_reload_last_timestamp_seconds{apiserver_id_hash="sha256:3c607df3b2bf22c9d9f01d5314b4bbf411c48ef43ff44ff29b1d55b41367c795",status="success"} FP`,
				`apiserver_authentication_config_controller_automatic_reloads_total{apiserver_id_hash="sha256:3c607df3b2bf22c9d9f01d5314b4bbf411c48ef43ff44ff29b1d55b41367c795",status="success"} 1`,
			},
		},
		{
			name: "old valid config to new valid config with typo (should be ignored)",
			authConfigFn: func(t *testing.T, issuerURL, caCert string) string {
				return fmt.Sprintf(`
apiVersion: apiserver.config.k8s.io/v1alpha1
kind: AuthenticationConfiguration
jwt:
- issuer:
    url: %s
    audiences:
    - %s
    - another-audience
    audienceMatchPolicy: MatchAny
    certificateAuthority: |
        %s
  claimMappings:
    username:
      expression: "'k8s-' + claims.sub"
`, issuerURL, defaultOIDCClientID, indentCertificateAuthority(caCert))
			},
			newAuthConfigFn: func(t *testing.T, issuerURL, _ string) string {
				return fmt.Sprintf(`
apiVersion: apiserver.config.k8s.io/v1alpha1
kind: AuthenticationConfiguration
jwt:
- issuer:
    url: %s
    audiences:
    - %s
    - another-audience
    audienceMatchPolicy: MatchAny
    certificateAuthority: ""  # missing CA
  claimMappings:
    username:
      expression: "'k8s-' + claims.sub"
`, issuerURL, defaultOIDCClientID)
			},
			assertErrFn: func(t *testing.T, errorToCheck error) {
				assert.NoError(t, errorToCheck)
			},
			wantUser: &authenticationv1.UserInfo{
				Username: "k8s-john_doe",
				Groups:   []string{"system:authenticated"},
			},
			newAssertErrFn: func(t *testing.T, errorToCheck error) {
				assert.NoError(t, errorToCheck)
			},
			newWantUser: &authenticationv1.UserInfo{
				Username: "k8s-john_doe",
				Groups:   []string{"system:authenticated"},
			},
			waitAfterConfigSwap: true,
			wantMetricStrings: []string{
				`apiserver_authentication_config_controller_automatic_reload_last_timestamp_seconds{apiserver_id_hash="sha256:3c607df3b2bf22c9d9f01d5314b4bbf411c48ef43ff44ff29b1d55b41367c795",status="failure"} FP`,
				`apiserver_authentication_config_controller_automatic_reloads_total{apiserver_id_hash="sha256:3c607df3b2bf22c9d9f01d5314b4bbf411c48ef43ff44ff29b1d55b41367c795",status="failure"} 1`,
			},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			authenticationconfigmetrics.ResetMetricsForTest()
			defer authenticationconfigmetrics.ResetMetricsForTest()

			ctx := testContext(t)

			oidcServer, apiServer, caCert, certPath := configureBasicTestInfrastructureWithRandomKeyType(t, tt.authConfigFn)

			tokenURL, err := oidcServer.TokenURL()
			require.NoError(t, err)

			client := configureClientFetchingOIDCCredentials(t, apiServer.ClientConfig, caCert, certPath, oidcServer.URL(), tokenURL)

			if tt.wantUser != nil {
				res, err := client.AuthenticationV1().SelfSubjectReviews().Create(ctx, &authenticationv1.SelfSubjectReview{}, metav1.CreateOptions{})
				require.NoError(t, err)
				assert.Equal(t, *tt.wantUser, res.Status.UserInfo)
			}

			_, err = client.CoreV1().Pods(defaultNamespace).List(ctx, metav1.ListOptions{})
			tt.assertErrFn(t, err)

			// Create a temporary file
			tempFile, err := os.CreateTemp("", "tempfile")
			require.NoError(t, err)
			defer func() {
				_ = tempFile.Close()
			}()

			// Write the new content to the temporary file
			_, err = tempFile.Write([]byte(tt.newAuthConfigFn(t, oidcServer.URL(), string(caCert))))
			require.NoError(t, err)

			// Atomically replace the original file with the temporary file
			err = os.Rename(tempFile.Name(), apiServer.ServerOpts.Authentication.AuthenticationConfigFile)
			require.NoError(t, err)

			if tt.waitAfterConfigSwap {
				time.Sleep(options.UpdateAuthenticationConfigTimeout + hardCodedTokenCacheTTLAndPollInterval) // has to be longer than UpdateAuthenticationConfigTimeout
			}

			if tt.newWantUser != nil {
				start := time.Now()
				err = wait.PollUntilContextTimeout(ctx, time.Second, 3*hardCodedTokenCacheTTLAndPollInterval, true, func(ctx context.Context) (done bool, err error) {
					res, err := client.AuthenticationV1().SelfSubjectReviews().Create(ctx, &authenticationv1.SelfSubjectReview{}, metav1.CreateOptions{})
					if err != nil {
						if tt.ignoreTransitionErrFn != nil && tt.ignoreTransitionErrFn(err) {
							return false, nil
						}
						return false, err
					}

					diff := cmp.Diff(*tt.newWantUser, res.Status.UserInfo)
					if len(diff) > 0 && time.Since(start) > 2*hardCodedTokenCacheTTLAndPollInterval {
						t.Logf("%s saw new user diff:\n%s", t.Name(), diff)
					}

					return len(diff) == 0, nil
				})
				require.NoError(t, err, "new authentication config not loaded")
			}

			_, err = client.CoreV1().Pods(defaultNamespace).List(ctx, metav1.ListOptions{})
			tt.newAssertErrFn(t, err)

			adminClient := kubernetes.NewForConfigOrDie(apiServer.ClientConfig)
			body, err := adminClient.RESTClient().Get().AbsPath("/metrics").DoRaw(ctx)
			require.NoError(t, err)
			var gotMetricStrings []string
			trimFP := regexp.MustCompile(`(.*)(} \d+\.\d+.*)`)
			for _, line := range strings.Split(string(body), "\n") {
				if strings.HasPrefix(line, "apiserver_authentication_config_controller_") {
					if strings.Contains(line, "_seconds") {
						line = trimFP.ReplaceAllString(line, `$1`) + "} FP" // ignore floating point metric values
					}
					gotMetricStrings = append(gotMetricStrings, line)
				}
			}
			if diff := cmp.Diff(tt.wantMetricStrings, gotMetricStrings); diff != "" {
				t.Errorf("unexpected metrics diff (-want +got): %s", diff)
			}
		})
	}
}

func configureBasicTestInfrastructureWithRandomKeyType(t *testing.T, fn authenticationConfigFunc) (
	oidcServer *utilsoidc.TestServer,
	apiServer *kubeapiserverapptesting.TestServer,
	caCertContent []byte,
	caFilePath string,
) {
	t.Helper()

	if randomBool() {
		return configureBasicTestInfrastructure(t, fn, rsaGenerateKey)
	}

	return configureBasicTestInfrastructure(t, fn, ecdsaGenerateKey)
}

func configureBasicTestInfrastructure[K utilsoidc.JosePrivateKey, L utilsoidc.JosePublicKey](t *testing.T, fn authenticationConfigFunc, keyFunc func(t *testing.T) (K, L)) (
	oidcServer *utilsoidc.TestServer,
	apiServer *kubeapiserverapptesting.TestServer,
	caCertContent []byte,
	caFilePath string,
) {
	t.Helper()

	oidcServer, apiServer, signingPrivateKey, caCertContent, caFilePath := configureTestInfrastructure(t, fn, keyFunc)

	oidcServer.TokenHandler().EXPECT().Token().RunAndReturn(utilsoidc.TokenHandlerBehaviorReturningPredefinedJWT(
		t,
		signingPrivateKey,
		map[string]interface{}{
			"iss": oidcServer.URL(),
			"sub": defaultOIDCClaimedUsername,
			"aud": defaultOIDCClientID,
			"exp": time.Now().Add(10 * time.Minute).Unix(),
		},
		defaultStubAccessToken,
		defaultStubRefreshToken,
	)).Times(1)

	return oidcServer, apiServer, caCertContent, caFilePath
}

// TestStructuredAuthenticationDiscoveryURL tests that the discovery URL configured in jwt.issuer.discoveryURL is used to
// fetch the discovery document and the issuer in jwt.issuer.url is used to validate the ID token.
func TestStructuredAuthenticationDiscoveryURL(t *testing.T) {
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.StructuredAuthenticationConfiguration, true)

	tests := []struct {
		name         string
		issuerURL    string
		discoveryURL func(baseURL string) string
	}{
		{
			name:         "discovery url and issuer url with no path",
			issuerURL:    "https://example.com",
			discoveryURL: func(baseURL string) string { return baseURL },
		},
		{
			name:         "discovery url has path, issuer url has no path",
			issuerURL:    "https://example.com",
			discoveryURL: func(baseURL string) string { return fmt.Sprintf("%s/c/d/bar", baseURL) },
		},
		{
			name:         "discovery url has no path, issuer url has path",
			issuerURL:    "https://example.com/a/b/foo",
			discoveryURL: func(baseURL string) string { return baseURL },
		},
		{
			name:      "discovery url and issuer url have paths",
			issuerURL: "https://example.com/a/b/foo",
			discoveryURL: func(baseURL string) string {
				return fmt.Sprintf("%s/c/d/bar", baseURL)
			},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			caCertContent, _, caFilePath, caKeyFilePath := generateCert(t)
			signingPrivateKey, publicKey := rsaGenerateKey(t)
			// set the issuer in the discovery document to issuer url (different from the discovery URL) to assert
			// 1. discovery URL is used to fetch the discovery document and
			// 2. issuer in the discovery document is used to validate the ID token
			oidcServer := utilsoidc.BuildAndRunTestServer(t, caFilePath, caKeyFilePath, tt.issuerURL)
			discoveryURL := strings.TrimSuffix(tt.discoveryURL(oidcServer.URL()), "/") + "/.well-known/openid-configuration"

			authenticationConfig := fmt.Sprintf(`
apiVersion: apiserver.config.k8s.io/v1alpha1
kind: AuthenticationConfiguration
jwt:
- issuer:
    url: %s
    discoveryURL: %s
    audiences:
    - foo
    audienceMatchPolicy: MatchAny
    certificateAuthority: |
        %s
  claimMappings:
    username:
      expression: "'k8s-' + claims.sub"
  claimValidationRules:
  - expression: 'claims.hd == "example.com"'
    message: "the hd claim must be set to example.com"
`, tt.issuerURL, discoveryURL, indentCertificateAuthority(string(caCertContent)))

			oidcServer.JwksHandler().EXPECT().KeySet().RunAndReturn(utilsoidc.DefaultJwksHandlerBehavior(t, publicKey)).Maybe()

			apiServer := startTestAPIServerForOIDC(t, apiServerOIDCConfig{authenticationConfigYAML: authenticationConfig}, publicKey)

			idTokenLifetime := time.Second * 1200
			oidcServer.TokenHandler().EXPECT().Token().RunAndReturn(utilsoidc.TokenHandlerBehaviorReturningPredefinedJWT(
				t,
				signingPrivateKey,
				map[string]interface{}{
					"iss": tt.issuerURL, // issuer in the discovery document is used to validate the ID token
					"sub": defaultOIDCClaimedUsername,
					"aud": "foo",
					"exp": time.Now().Add(idTokenLifetime).Unix(),
					"hd":  "example.com",
				},
				defaultStubAccessToken,
				defaultStubRefreshToken,
			)).Times(1)

			tokenURL, err := oidcServer.TokenURL()
			require.NoError(t, err)

			client := configureClientFetchingOIDCCredentials(t, apiServer.ClientConfig, caCertContent, caFilePath, oidcServer.URL(), tokenURL)
			ctx := testContext(t)
			res, err := client.AuthenticationV1().SelfSubjectReviews().Create(ctx, &authenticationv1.SelfSubjectReview{}, metav1.CreateOptions{})
			require.NoError(t, err)
			assert.Equal(t, authenticationv1.UserInfo{
				Username: "k8s-john_doe",
				Groups:   []string{"system:authenticated"},
			}, res.Status.UserInfo)
		})
	}
}

func TestMultipleJWTAuthenticators(t *testing.T) {
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.StructuredAuthenticationConfiguration, true)
	genericapiserver.SetHostnameFuncForTests("testAPIServerID")
	oidc.ResetMetrics()

	caCertContent1, _, caFilePath1, caKeyFilePath1 := generateCert(t)
	signingPrivateKey1, publicKey1 := rsaGenerateKey(t)
	oidcServer1 := utilsoidc.BuildAndRunTestServer(t, caFilePath1, caKeyFilePath1, "")

	caCertContent2, _, caFilePath2, caKeyFilePath2 := generateCert(t)
	signingPrivateKey2, publicKey2 := rsaGenerateKey(t)
	oidcServer2 := utilsoidc.BuildAndRunTestServer(t, caFilePath2, caKeyFilePath2, "https://example.com")

	authenticationConfig := fmt.Sprintf(`
apiVersion: apiserver.config.k8s.io/v1alpha1
kind: AuthenticationConfiguration
jwt:
- issuer:
    url: %s
    audiences:
    - foo
    audienceMatchPolicy: MatchAny
    certificateAuthority: |
        %s
  claimMappings:
    username:
      expression: "'k8s-' + claims.sub"
  claimValidationRules:
  - expression: 'claims.hd == "example.com"'
    message: "the hd claim must be set to example.com"
- issuer:
    url: "https://example.com"
    discoveryURL: %s/.well-known/openid-configuration
    audiences:
    - bar
    audienceMatchPolicy: MatchAny
    certificateAuthority: |
        %s
  claimMappings:
    username:
      expression: "'k8s-' + claims.sub"
    groups:
      expression: '(claims.roles.split(",") + claims.other_roles.split(",")).map(role, "system:" + role)'
    uid:
      expression: "claims.uid"
`, oidcServer1.URL(), indentCertificateAuthority(string(caCertContent1)), oidcServer2.URL(), indentCertificateAuthority(string(caCertContent2)))

	oidcServer1.JwksHandler().EXPECT().KeySet().RunAndReturn(utilsoidc.DefaultJwksHandlerBehavior(t, publicKey1)).Maybe()
	oidcServer2.JwksHandler().EXPECT().KeySet().RunAndReturn(utilsoidc.DefaultJwksHandlerBehavior(t, publicKey2)).Maybe()

	apiServer := startTestAPIServerForOIDC(t, apiServerOIDCConfig{authenticationConfigYAML: authenticationConfig}, publicKey1)

	idTokenLifetime := time.Second * 1200
	oidcServer1.TokenHandler().EXPECT().Token().RunAndReturn(utilsoidc.TokenHandlerBehaviorReturningPredefinedJWT(
		t,
		signingPrivateKey1,
		map[string]interface{}{
			"iss": oidcServer1.URL(),
			"sub": defaultOIDCClaimedUsername,
			"aud": "foo",
			"exp": time.Now().Add(idTokenLifetime).Unix(),
			"hd":  "example.com",
		},
		defaultStubAccessToken,
		defaultStubRefreshToken,
	)).Times(1)

	oidcServer2.TokenHandler().EXPECT().Token().RunAndReturn(utilsoidc.TokenHandlerBehaviorReturningPredefinedJWT(
		t,
		signingPrivateKey2,
		map[string]interface{}{
			"iss":         "https://example.com",
			"sub":         "not_john_doe",
			"aud":         "bar",
			"roles":       "role1,role2",
			"other_roles": "role3,role4",
			"exp":         time.Now().Add(idTokenLifetime).Unix(),
			"uid":         "1234",
		},
		defaultStubAccessToken,
		defaultStubRefreshToken,
	)).Times(1)

	tokenURL1, err := oidcServer1.TokenURL()
	require.NoError(t, err)

	tokenURL2, err := oidcServer2.TokenURL()
	require.NoError(t, err)

	client1 := configureClientFetchingOIDCCredentials(t, apiServer.ClientConfig, caCertContent1, caFilePath1, oidcServer1.URL(), tokenURL1)
	client2 := configureClientFetchingOIDCCredentials(t, apiServer.ClientConfig, caCertContent2, caFilePath2, oidcServer2.URL(), tokenURL2)

	ctx := testContext(t)
	res, err := client1.AuthenticationV1().SelfSubjectReviews().Create(ctx, &authenticationv1.SelfSubjectReview{}, metav1.CreateOptions{})
	require.NoError(t, err)
	assert.Equal(t, authenticationv1.UserInfo{
		Username: "k8s-john_doe",
		Groups:   []string{"system:authenticated"},
	}, res.Status.UserInfo)

	res, err = client2.AuthenticationV1().SelfSubjectReviews().Create(ctx, &authenticationv1.SelfSubjectReview{}, metav1.CreateOptions{})
	require.NoError(t, err)
	assert.Equal(t, authenticationv1.UserInfo{
		Username: "k8s-not_john_doe",
		Groups:   []string{"system:role1", "system:role2", "system:role3", "system:role4", "system:authenticated"},
		UID:      "1234",
	}, res.Status.UserInfo)

	jwtIssuerHash1 := fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(oidcServer1.URL())))
	jwtIssuerHash2 := fmt.Sprintf("sha256:%x", sha256.Sum256([]byte("https://example.com")))

	keySet1 := oidcServer1.JwksHandler().KeySet()
	keySet1Bytes, err := json.Marshal(keySet1)
	require.NoError(t, err)

	keySet2 := oidcServer2.JwksHandler().KeySet()
	keySet2Bytes, err := json.Marshal(keySet2)
	require.NoError(t, err)

	hf1 := fnv.New64()
	hf1.Write(keySet1Bytes)

	hf2 := fnv.New64()
	hf2.Write(keySet2Bytes)

	wantMetricStrings := []string{
		fmt.Sprintf(`apiserver_authentication_jwt_authenticator_jwks_fetch_last_key_set_hash{apiserver_id_hash="sha256:3c607df3b2bf22c9d9f01d5314b4bbf411c48ef43ff44ff29b1d55b41367c795",jwt_issuer_hash="%s"} %g`, jwtIssuerHash1, float64(hf1.Sum64())),
		fmt.Sprintf(`apiserver_authentication_jwt_authenticator_jwks_fetch_last_key_set_hash{apiserver_id_hash="sha256:3c607df3b2bf22c9d9f01d5314b4bbf411c48ef43ff44ff29b1d55b41367c795",jwt_issuer_hash="%s"} %g`, jwtIssuerHash2, float64(hf2.Sum64())),
		fmt.Sprintf(`apiserver_authentication_jwt_authenticator_jwks_fetch_last_timestamp_seconds{apiserver_id_hash="sha256:3c607df3b2bf22c9d9f01d5314b4bbf411c48ef43ff44ff29b1d55b41367c795",jwt_issuer_hash="%s",result="success"} FP`, jwtIssuerHash1),
		fmt.Sprintf(`apiserver_authentication_jwt_authenticator_jwks_fetch_last_timestamp_seconds{apiserver_id_hash="sha256:3c607df3b2bf22c9d9f01d5314b4bbf411c48ef43ff44ff29b1d55b41367c795",jwt_issuer_hash="%s",result="success"} FP`, jwtIssuerHash2),
	}
	adminClient := kubernetes.NewForConfigOrDie(apiServer.ClientConfig)
	body, err := adminClient.RESTClient().Get().AbsPath("/metrics").DoRaw(ctx)
	require.NoError(t, err)
	var gotMetricStrings []string
	trimFP := regexp.MustCompile(`(.*)(} \d+\.\d+.*)`)
	for _, line := range strings.Split(string(body), "\n") {
		if strings.HasPrefix(line, "apiserver_authentication_jwt_authenticator_jwks_") {
			if strings.Contains(line, "_seconds") {
				line = trimFP.ReplaceAllString(line, `$1`) + "} FP" // ignore floating point metric values
			}

			gotMetricStrings = append(gotMetricStrings, line)
		}
	}

	wantMetricStrings = slice.SortStrings(wantMetricStrings)
	gotMetricStrings = slice.SortStrings(gotMetricStrings)

	if diff := cmp.Diff(wantMetricStrings, gotMetricStrings); diff != "" {
		t.Errorf("unexpected metrics diff (-want +got): %s", diff)
	}
}

func rsaGenerateKey(t *testing.T) (*rsa.PrivateKey, *rsa.PublicKey) {
	t.Helper()

	privateKey, err := rsa.GenerateKey(rand.Reader, rsaKeyBitSize)
	require.NoError(t, err)

	return privateKey, &privateKey.PublicKey
}

func ecdsaGenerateKey(t *testing.T) (*ecdsa.PrivateKey, *ecdsa.PublicKey) {
	t.Helper()

	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	return privateKey, &privateKey.PublicKey
}

func configureTestInfrastructure[K utilsoidc.JosePrivateKey, L utilsoidc.JosePublicKey](t *testing.T, fn authenticationConfigFunc, keyFunc func(t *testing.T) (K, L)) (
	oidcServer *utilsoidc.TestServer,
	apiServer *kubeapiserverapptesting.TestServer,
	signingPrivateKey K,
	caCertContent []byte,
	caFilePath string,
) {
	t.Helper()

	caCertContent, _, caFilePath, caKeyFilePath := generateCert(t)

	signingPrivateKey, publicKey := keyFunc(t)

	oidcServer = utilsoidc.BuildAndRunTestServer(t, caFilePath, caKeyFilePath, "")

	authenticationConfig := fn(t, oidcServer.URL(), string(caCertContent))
	if len(authenticationConfig) > 0 {
		apiServer = startTestAPIServerForOIDC(t, apiServerOIDCConfig{authenticationConfigYAML: authenticationConfig}, publicKey)
	} else {
		apiServer = startTestAPIServerForOIDC(t, apiServerOIDCConfig{oidcURL: oidcServer.URL(), oidcClientID: defaultOIDCClientID, oidcCAFilePath: caFilePath, oidcUsernamePrefix: defaultOIDCUsernamePrefix}, publicKey)
	}

	oidcServer.JwksHandler().EXPECT().KeySet().RunAndReturn(utilsoidc.DefaultJwksHandlerBehavior(t, publicKey)).Maybe()

	adminClient := kubernetes.NewForConfigOrDie(apiServer.ClientConfig)
	configureRBAC(t, adminClient, defaultRole, defaultRoleBinding)

	return oidcServer, apiServer, signingPrivateKey, caCertContent, caFilePath
}

func configureClientFetchingOIDCCredentials(t *testing.T, restCfg *rest.Config, caCert []byte, certPath, oidcServerURL, oidcServerTokenURL string) kubernetes.Interface {
	idToken, stubRefreshToken := fetchOIDCCredentials(t, oidcServerTokenURL, caCert)
	clientConfig := configureClientConfigForOIDC(t, restCfg, defaultOIDCClientID, certPath, idToken, stubRefreshToken, oidcServerURL)
	return kubernetes.NewForConfigOrDie(clientConfig)
}

func configureClientWithEmptyIDToken(t *testing.T, restCfg *rest.Config, _ []byte, certPath, oidcServerURL, _ string) kubernetes.Interface {
	emptyIDToken, stubRefreshToken := "", defaultStubRefreshToken
	clientConfig := configureClientConfigForOIDC(t, restCfg, defaultOIDCClientID, certPath, emptyIDToken, stubRefreshToken, oidcServerURL)
	return kubernetes.NewForConfigOrDie(clientConfig)
}

func configureRBAC(t *testing.T, clientset kubernetes.Interface, role *rbacv1.Role, binding *rbacv1.RoleBinding) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second*30)
	defer cancel()

	_, err := clientset.RbacV1().Roles(defaultNamespace).Create(ctx, role, metav1.CreateOptions{})
	require.NoError(t, err)
	_, err = clientset.RbacV1().RoleBindings(defaultNamespace).Create(ctx, binding, metav1.CreateOptions{})
	require.NoError(t, err)
}

func configureClientConfigForOIDC(t *testing.T, config *rest.Config, clientID, caFilePath, idToken, refreshToken, oidcServerURL string) *rest.Config {
	t.Helper()
	cfg := rest.AnonymousClientConfig(config)
	cfg.AuthProvider = &api.AuthProviderConfig{
		Name: "oidc",
		Config: map[string]string{
			"client-id":                 clientID,
			"id-token":                  idToken,
			"idp-issuer-url":            oidcServerURL,
			"idp-certificate-authority": caFilePath,
			"refresh-token":             refreshToken,
		},
	}

	return cfg
}

func startTestAPIServerForOIDC[L utilsoidc.JosePublicKey](t *testing.T, c apiServerOIDCConfig, publicKey L) *kubeapiserverapptesting.TestServer {
	t.Helper()

	var customFlags []string
	if len(c.authenticationConfigYAML) > 0 {
		customFlags = []string{fmt.Sprintf("--authentication-config=%s", writeTempFile(t, c.authenticationConfigYAML))}
	} else {
		customFlags = []string{
			fmt.Sprintf("--oidc-issuer-url=%s", c.oidcURL),
			fmt.Sprintf("--oidc-client-id=%s", c.oidcClientID),
			fmt.Sprintf("--oidc-ca-file=%s", c.oidcCAFilePath),
			fmt.Sprintf("--oidc-username-prefix=%s", c.oidcUsernamePrefix),
		}
		if len(c.oidcUsernameClaim) > 0 {
			customFlags = append(customFlags, fmt.Sprintf("--oidc-username-claim=%s", c.oidcUsernameClaim))
		}
		customFlags = append(customFlags, maybeSetSigningAlgs(publicKey)...)
	}
	customFlags = append(customFlags, "--authorization-mode=RBAC")

	server, err := kubeapiserverapptesting.StartTestServer(
		t,
		kubeapiserverapptesting.NewDefaultTestServerOptions(),
		customFlags,
		framework.SharedEtcd(),
	)
	require.NoError(t, err)

	t.Cleanup(server.TearDownFn)

	return &server
}

func maybeSetSigningAlgs[K utilsoidc.JoseKey](key K) []string {
	alg := utilsoidc.GetSignatureAlgorithm(key)
	if alg == jose.RS256 && randomBool() {
		return nil // check the default case of RS256 by not always setting the flag
	}
	return []string{
		fmt.Sprintf("--oidc-signing-algs=%s", alg), // all other algs need to be manually set
	}
}

func randomBool() bool { return utilrand.Int()%2 == 1 }

func fetchOIDCCredentials(t *testing.T, oidcTokenURL string, caCertContent []byte) (idToken, refreshToken string) {
	t.Helper()

	req, err := http.NewRequest(http.MethodGet, oidcTokenURL, http.NoBody)
	require.NoError(t, err)

	caPool := x509.NewCertPool()
	ok := caPool.AppendCertsFromPEM(caCertContent)
	require.True(t, ok)

	client := http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{
			RootCAs: caPool,
		},
	}}

	token := new(handlers.Token)

	resp, err := client.Do(req)
	require.NoError(t, err)

	err = json.NewDecoder(resp.Body).Decode(token)
	require.NoError(t, err)

	return token.IDToken, token.RefreshToken
}

func fetchExpiredToken(t *testing.T, oidcServer *utilsoidc.TestServer, caCertContent []byte, signingPrivateKey *rsa.PrivateKey) (expiredToken, stubRefreshToken string) {
	t.Helper()

	tokenURL, err := oidcServer.TokenURL()
	require.NoError(t, err)

	configureOIDCServerToReturnExpiredIDToken(t, 1, oidcServer, signingPrivateKey)
	expiredToken, stubRefreshToken = fetchOIDCCredentials(t, tokenURL, caCertContent)

	return expiredToken, stubRefreshToken
}

func configureOIDCServerToReturnExpiredIDToken(t *testing.T, returningExpiredTokenTimes int, oidcServer *utilsoidc.TestServer, signingPrivateKey *rsa.PrivateKey) {
	t.Helper()

	oidcServer.TokenHandler().EXPECT().Token().RunAndReturn(func() (handlers.Token, error) {
		token, err := utilsoidc.TokenHandlerBehaviorReturningPredefinedJWT(
			t,
			signingPrivateKey,
			map[string]interface{}{
				"iss": oidcServer.URL(),
				"sub": defaultOIDCClaimedUsername,
				"aud": defaultOIDCClientID,
				"exp": time.Now().Add(-time.Millisecond).Unix(),
			},
			defaultStubAccessToken,
			defaultStubRefreshToken,
		)()
		return token, err
	}).Times(returningExpiredTokenTimes)
}

func configureOIDCServerToReturnExpiredRefreshTokenErrorOnTryingToUpdateIDToken(oidcServer *utilsoidc.TestServer) {
	oidcServer.TokenHandler().EXPECT().Token().Times(2).Return(handlers.Token{}, utilsoidc.ErrRefreshTokenExpired)
}

func generateCert(t *testing.T) (cert, key []byte, certFilePath, keyFilePath string) {
	t.Helper()

	tempDir := t.TempDir()
	certFilePath = filepath.Join(tempDir, "localhost_127.0.0.1_.crt")
	keyFilePath = filepath.Join(tempDir, "localhost_127.0.0.1_.key")

	cert, key, err := certutil.GenerateSelfSignedCertKeyWithFixtures("localhost", []net.IP{utilsnet.ParseIPSloppy("127.0.0.1")}, nil, tempDir)
	require.NoError(t, err)

	return cert, key, certFilePath, keyFilePath
}

func writeTempFile(t *testing.T, content string) string {
	t.Helper()
	file, err := os.CreateTemp("", "oidc-test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Remove(file.Name()); err != nil {
			t.Fatal(err)
		}
	})
	if err := os.WriteFile(file.Name(), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return file.Name()
}

// indentCertificateAuthority indents the certificate authority to match
// the format of the generated authentication config.
func indentCertificateAuthority(caCert string) string {
	return strings.ReplaceAll(caCert, "\n", "\n        ")
}

func testContext(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	return ctx
}
