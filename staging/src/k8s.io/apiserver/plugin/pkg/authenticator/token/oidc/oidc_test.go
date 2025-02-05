/*
Copyright 2015 The Kubernetes Authors.

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
	"bytes"
	"context"
	"crypto"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"text/template"
	"time"

	"gopkg.in/go-jose/go-jose.v2"

	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apiserver/pkg/apis/apiserver"
	"k8s.io/apiserver/pkg/authentication/user"
	"k8s.io/apiserver/pkg/features"
	"k8s.io/apiserver/pkg/server/dynamiccertificates"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	"k8s.io/component-base/metrics/legacyregistry"
	"k8s.io/component-base/metrics/testutil"
	"k8s.io/klog/v2"
	"k8s.io/utils/pointer"
)

// utilities for loading JOSE keys.

func loadRSAKey(t *testing.T, filepath string, alg jose.SignatureAlgorithm) *jose.JSONWebKey {
	return loadKey(t, filepath, alg, func(b []byte) (interface{}, error) {
		key, err := x509.ParsePKCS1PrivateKey(b)
		if err != nil {
			return nil, err
		}
		return key.Public(), nil
	})
}

func loadRSAPrivKey(t *testing.T, filepath string, alg jose.SignatureAlgorithm) *jose.JSONWebKey {
	return loadKey(t, filepath, alg, func(b []byte) (interface{}, error) {
		return x509.ParsePKCS1PrivateKey(b)
	})
}

func loadECDSAKey(t *testing.T, filepath string, alg jose.SignatureAlgorithm) *jose.JSONWebKey {
	return loadKey(t, filepath, alg, func(b []byte) (interface{}, error) {
		key, err := x509.ParseECPrivateKey(b)
		if err != nil {
			return nil, err
		}
		return key.Public(), nil
	})
}

func loadECDSAPrivKey(t *testing.T, filepath string, alg jose.SignatureAlgorithm) *jose.JSONWebKey {
	return loadKey(t, filepath, alg, func(b []byte) (interface{}, error) {
		return x509.ParseECPrivateKey(b)
	})
}

func loadKey(t *testing.T, filepath string, alg jose.SignatureAlgorithm, unmarshal func([]byte) (interface{}, error)) *jose.JSONWebKey {
	data, err := os.ReadFile(filepath)
	if err != nil {
		t.Fatalf("load file: %v", err)
	}
	block, _ := pem.Decode(data)
	if block == nil {
		t.Fatalf("file contained no PEM encoded data: %s", filepath)
	}
	priv, err := unmarshal(block.Bytes)
	if err != nil {
		t.Fatalf("unmarshal key: %v", err)
	}
	key := &jose.JSONWebKey{Key: priv, Use: "sig", Algorithm: string(alg)}
	thumbprint, err := key.Thumbprint(crypto.SHA256)
	if err != nil {
		t.Fatalf("computing thumbprint: %v", err)
	}
	key.KeyID = hex.EncodeToString(thumbprint)
	return key
}

// staticKeySet implements oidc.KeySet.
type staticKeySet struct {
	keys []*jose.JSONWebKey
}

func (s *staticKeySet) VerifySignature(ctx context.Context, jwt string) (payload []byte, err error) {
	jws, err := jose.ParseSigned(jwt)
	if err != nil {
		return nil, err
	}
	if len(jws.Signatures) == 0 {
		return nil, fmt.Errorf("jwt contained no signatures")
	}
	kid := jws.Signatures[0].Header.KeyID

	for _, key := range s.keys {
		if key.KeyID == kid {
			return jws.Verify(key)
		}
	}

	return nil, fmt.Errorf("no keys matches jwk keyid")
}

var (
	expired, _ = time.Parse(time.RFC3339Nano, "2009-11-10T22:00:00Z")
	now, _     = time.Parse(time.RFC3339Nano, "2009-11-10T23:00:00Z")
	valid, _   = time.Parse(time.RFC3339Nano, "2009-11-11T00:00:00Z")
)

type claimsTest struct {
	name                string
	options             Options
	optsFunc            func(*Options)
	signingKey          *jose.JSONWebKey
	pubKeys             []*jose.JSONWebKey
	claims              string
	want                *user.DefaultInfo
	wantSkip            bool
	wantErr             string
	wantInitErr         string
	wantHealthErrPrefix string
	claimToResponseMap  map[string]string
	openIDConfig        string
	fetchKeysFromRemote bool
}

type testClaimsServer struct {
	*httptest.Server

	keys jose.JSONWebKeySet
	mu   sync.RWMutex
}

func (c *testClaimsServer) setKeys(keys jose.JSONWebKeySet) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.keys = keys
}

func (c *testClaimsServer) getKeys() jose.JSONWebKeySet {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.keys
}

// Replace formats the contents of v into the provided template.
func replace(tmpl string, v interface{}) string {
	t := template.Must(template.New("test").Parse(tmpl))
	buf := bytes.NewBuffer(nil)
	t.Execute(buf, &v)
	ret := buf.String()
	klog.V(4).Infof("Replaced: %v into: %v", tmpl, ret)
	return ret
}

// newClaimServer returns a new test HTTPS server, which is rigged to return
// OIDC responses to requests that resolve distributed claims. signer is the
// signer used for the served JWT tokens.  claimToResponseMap is a map of
// responses that the server will return for each claim it is given.
func newClaimServer(t *testing.T, keys jose.JSONWebKeySet, signer jose.Signer, claimToResponseMap map[string]string, openIDConfig *string) *testClaimsServer {
	cs := &testClaimsServer{
		keys: keys,
		mu:   sync.RWMutex{},
	}

	cs.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		klog.V(5).Infof("request: %+v", *r)
		switch r.URL.Path {
		case "/.testing/keys":
			w.Header().Set("Content-Type", "application/json")
			keyBytes, err := json.Marshal(cs.getKeys())
			if err != nil {
				t.Fatalf("unexpected error while marshaling keys: %v", err)
			}
			klog.V(5).Infof("%v: returning: %+v", r.URL, string(keyBytes))
			w.Write(keyBytes)

		case "/.testing/invalidkeys":
			w.Header().Set("Content-Type", "application/json")
		case "/.testing/keys/badstatus":
			w.WriteHeader(http.StatusInternalServerError)

		// /c/d/bar/.well-known/openid-configuration is used to test issuer url and discovery url with a path
		case "/.well-known/openid-configuration", "/c/d/bar/.well-known/openid-configuration":
			w.Header().Set("Content-Type", "application/json")
			klog.V(5).Infof("%v: returning: %+v", r.URL, *openIDConfig)
			w.Write([]byte(*openIDConfig))
		// These claims are tested in the unit tests.
		case "/groups":
			fallthrough
		case "/rabbits":
			if claimToResponseMap == nil {
				t.Errorf("no claims specified in response")
			}
			claim := r.URL.Path[1:] // "/groups" -> "groups"
			expectedAuth := fmt.Sprintf("Bearer %v_token", claim)
			auth := r.Header.Get("Authorization")
			if auth != expectedAuth {
				t.Errorf("bearer token expected: %q, was %q", expectedAuth, auth)
			}
			jws, err := signer.Sign([]byte(claimToResponseMap[claim]))
			if err != nil {
				t.Errorf("while signing response token: %v", err)
			}
			token, err := jws.CompactSerialize()
			if err != nil {
				t.Errorf("while serializing response token: %v", err)
			}
			w.Write([]byte(token))
		default:
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprintf(w, "unexpected URL: %v", r.URL)
		}
	}))
	klog.V(4).Infof("Serving OIDC at: %v", cs.Server.URL)
	return cs
}

func toKeySet(keys []*jose.JSONWebKey) jose.JSONWebKeySet {
	ret := jose.JSONWebKeySet{}
	for _, k := range keys {
		ret.Keys = append(ret.Keys, *k)
	}
	return ret
}

func (c *claimsTest) run(t *testing.T) {
	var (
		signer jose.Signer
		err    error
	)
	if c.signingKey != nil {
		// Initialize the signer only in the tests that make use of it.  We can
		// not defer this initialization because the test server uses it too.
		signer, err = jose.NewSigner(jose.SigningKey{
			Algorithm: jose.SignatureAlgorithm(c.signingKey.Algorithm),
			Key:       c.signingKey,
		}, nil)
		if err != nil {
			t.Fatalf("initialize signer: %v", err)
		}
	}
	// The HTTPS server used for requesting distributed groups claims.
	ts := newClaimServer(t, toKeySet(c.pubKeys), signer, c.claimToResponseMap, &c.openIDConfig)
	defer ts.Close()

	// Make the certificate of the helper server available to the authenticator
	caBundle := pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: ts.Certificate().Raw,
	})
	caContent, err := dynamiccertificates.NewStaticCAContent("oidc-authenticator", caBundle)
	if err != nil {
		t.Fatalf("initialize ca: %v", err)
	}
	c.options.CAContentProvider = caContent

	// Allow claims to refer to the serving URL of the test server.  For this,
	// substitute all references to {{.URL}} in appropriate places.
	// Use {{.Expired}} to handle the token expiry date string with correct timezone handling.
	v := struct {
		URL     string
		Expired string
	}{
		URL:     ts.URL,
		Expired: fmt.Sprintf("%v", time.Unix(expired.Unix(), 0)),
	}
	c.claims = replace(c.claims, &v)
	c.openIDConfig = replace(c.openIDConfig, &v)
	c.options.JWTAuthenticator.Issuer.URL = replace(c.options.JWTAuthenticator.Issuer.URL, &v)
	c.options.JWTAuthenticator.Issuer.DiscoveryURL = replace(c.options.JWTAuthenticator.Issuer.DiscoveryURL, &v)
	for claim, response := range c.claimToResponseMap {
		c.claimToResponseMap[claim] = replace(response, &v)
	}
	c.wantErr = replace(c.wantErr, &v)
	c.wantInitErr = replace(c.wantInitErr, &v)

	if !c.fetchKeysFromRemote {
		// Set the verifier to use the public key set instead of reading from a remote.
		c.options.KeySet = &staticKeySet{keys: c.pubKeys}
	}

	if c.optsFunc != nil {
		c.optsFunc(&c.options)
	}

	expectInitErr := len(c.wantInitErr) > 0

	ctx := testContext(t)

	// Initialize the authenticator.
	a, err := New(ctx, c.options)
	if err != nil {
		if !expectInitErr {
			t.Fatalf("initialize authenticator: %v", err)
		}
		if got := err.Error(); c.wantInitErr != got {
			t.Fatalf("expected initialization error %q but got %q", c.wantInitErr, got)
		}
		return
	}
	if expectInitErr {
		t.Fatalf("wanted initialization error %q but got none", c.wantInitErr)
	}

	if len(c.wantHealthErrPrefix) > 0 {
		if err := wait.PollUntilContextTimeout(ctx, time.Second, time.Minute, true, func(context.Context) (bool, error) {
			healthErr := a.HealthCheck()
			if healthErr == nil {
				return false, fmt.Errorf("authenticator reported healthy when it should not")
			}

			if strings.HasPrefix(healthErr.Error(), c.wantHealthErrPrefix) {
				return true, nil
			}

			t.Logf("saw health error prefix that did not match: want=%q got=%q", c.wantHealthErrPrefix, healthErr.Error())
			return false, nil
		}); err != nil {
			t.Fatalf("authenticator did not match wanted health error: %v", err)
		}
		return
	}

	claims := struct{}{}
	if err := json.Unmarshal([]byte(c.claims), &claims); err != nil {
		t.Fatalf("failed to unmarshal claims: %v", err)
	}

	// Sign and serialize the claims in a JWT.
	jws, err := signer.Sign([]byte(c.claims))
	if err != nil {
		t.Fatalf("sign claims: %v", err)
	}
	token, err := jws.CompactSerialize()
	if err != nil {
		t.Fatalf("serialize token: %v", err)
	}

	// wait for the authenticator to be healthy
	err = wait.PollUntilContextCancel(ctx, time.Millisecond, true, func(context.Context) (bool, error) {
		return a.HealthCheck() == nil, nil
	})
	if err != nil {
		t.Fatalf("failed to initialize the authenticator: %v", err)
	}

	got, ok, err := a.AuthenticateToken(ctx, token)

	expectErr := len(c.wantErr) > 0

	if err != nil {
		if !expectErr {
			t.Fatalf("authenticate token: %v", err)
		}
		if got := err.Error(); c.wantErr != got {
			t.Fatalf("expected error %q when authenticating token but got %q", c.wantErr, got)
		}
		return
	}

	if expectErr {
		t.Fatalf("expected error %q when authenticating token but got none", c.wantErr)
	}
	if !ok {
		if !c.wantSkip {
			// We don't have any cases where we return (nil, false, nil)
			t.Fatalf("no error but token not authenticated")
		}
		return
	}
	if c.wantSkip {
		t.Fatalf("expected authenticator to skip token")
	}

	gotUser := got.User.(*user.DefaultInfo)
	if !reflect.DeepEqual(gotUser, c.want) {
		t.Fatalf("wanted user=%#v, got=%#v", c.want, gotUser)
	}
}

func TestToken(t *testing.T) {
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.StructuredAuthenticationConfiguration, true)

	synchronizeTokenIDVerifierForTest = true
	tests := []claimsTest{
		{
			name: "token",
			options: Options{
				JWTAuthenticator: apiserver.JWTAuthenticator{
					Issuer: apiserver.Issuer{
						URL:       "https://auth.example.com",
						Audiences: []string{"my-client"},
					},
					ClaimMappings: apiserver.ClaimMappings{
						Username: apiserver.PrefixedClaimOrExpression{
							Claim:  "username",
							Prefix: pointer.String(""),
						},
					},
				},
				now: func() time.Time { return now },
			},
			signingKey: loadRSAPrivKey(t, "testdata/rsa_1.pem", jose.RS256),
			pubKeys: []*jose.JSONWebKey{
				loadRSAKey(t, "testdata/rsa_1.pem", jose.RS256),
			},
			claims: fmt.Sprintf(`{
				"iss": "https://auth.example.com",
				"aud": "my-client",
				"username": "jane",
				"exp": %d
			}`, valid.Unix()),
			want: &user.DefaultInfo{
				Name: "jane",
			},
		},
		{
			name: "no-username",
			options: Options{
				JWTAuthenticator: apiserver.JWTAuthenticator{
					Issuer: apiserver.Issuer{
						URL:       "https://auth.example.com",
						Audiences: []string{"my-client"},
					},
					ClaimMappings: apiserver.ClaimMappings{
						Username: apiserver.PrefixedClaimOrExpression{
							Claim:  "username",
							Prefix: pointer.String("prefix:"),
						},
					},
				},
				now: func() time.Time { return now },
			},
			signingKey: loadRSAPrivKey(t, "testdata/rsa_1.pem", jose.RS256),
			pubKeys: []*jose.JSONWebKey{
				loadRSAKey(t, "testdata/rsa_1.pem", jose.RS256),
			},
			claims: fmt.Sprintf(`{
				"iss": "https://auth.example.com",
				"aud": "my-client",
				"exp": %d
			}`, valid.Unix()),
			wantErr: `oidc: parse username claims "username": claim not present`,
		},
		{
			name: "email",
			options: Options{
				JWTAuthenticator: apiserver.JWTAuthenticator{
					Issuer: apiserver.Issuer{
						URL:       "https://auth.example.com",
						Audiences: []string{"my-client"},
					},
					ClaimMappings: apiserver.ClaimMappings{
						Username: apiserver.PrefixedClaimOrExpression{
							Claim:  "email",
							Prefix: pointer.String(""),
						},
					},
				},
				now: func() time.Time { return now },
			},
			signingKey: loadRSAPrivKey(t, "testdata/rsa_1.pem", jose.RS256),
			pubKeys: []*jose.JSONWebKey{
				loadRSAKey(t, "testdata/rsa_1.pem", jose.RS256),
			},
			claims: fmt.Sprintf(`{
				"iss": "https://auth.example.com",
				"aud": "my-client",
				"email": "jane@example.com",
				"email_verified": true,
				"exp": %d
			}`, valid.Unix()),
			want: &user.DefaultInfo{
				Name: "jane@example.com",
			},
		},
		{
			name: "email-not-verified",
			options: Options{
				JWTAuthenticator: apiserver.JWTAuthenticator{
					Issuer: apiserver.Issuer{
						URL:       "https://auth.example.com",
						Audiences: []string{"my-client"},
					},
					ClaimMappings: apiserver.ClaimMappings{
						Username: apiserver.PrefixedClaimOrExpression{
							Claim:  "email",
							Prefix: pointer.String(""),
						},
					},
				},
				now: func() time.Time { return now },
			},
			signingKey: loadRSAPrivKey(t, "testdata/rsa_1.pem", jose.RS256),
			pubKeys: []*jose.JSONWebKey{
				loadRSAKey(t, "testdata/rsa_1.pem", jose.RS256),
			},
			claims: fmt.Sprintf(`{
				"iss": "https://auth.example.com",
				"aud": "my-client",
				"email": "jane@example.com",
				"email_verified": false,
				"exp": %d
			}`, valid.Unix()),
			wantErr: "oidc: email not verified",
		},
		{
			// If "email_verified" isn't present, assume true
			name: "no-email-verified-claim",
			options: Options{
				JWTAuthenticator: apiserver.JWTAuthenticator{
					Issuer: apiserver.Issuer{
						URL:       "https://auth.example.com",
						Audiences: []string{"my-client"},
					},
					ClaimMappings: apiserver.ClaimMappings{
						Username: apiserver.PrefixedClaimOrExpression{
							Claim:  "email",
							Prefix: pointer.String(""),
						},
					},
				},
				now: func() time.Time { return now },
			},
			signingKey: loadRSAPrivKey(t, "testdata/rsa_1.pem", jose.RS256),
			pubKeys: []*jose.JSONWebKey{
				loadRSAKey(t, "testdata/rsa_1.pem", jose.RS256),
			},
			claims: fmt.Sprintf(`{
				"iss": "https://auth.example.com",
				"aud": "my-client",
				"email": "jane@example.com",
				"exp": %d
			}`, valid.Unix()),
			want: &user.DefaultInfo{
				Name: "jane@example.com",
			},
		},
		{
			name: "invalid-email-verified-claim",
			options: Options{
				JWTAuthenticator: apiserver.JWTAuthenticator{
					Issuer: apiserver.Issuer{
						URL:       "https://auth.example.com",
						Audiences: []string{"my-client"},
					},
					ClaimMappings: apiserver.ClaimMappings{
						Username: apiserver.PrefixedClaimOrExpression{
							Claim:  "email",
							Prefix: pointer.String(""),
						},
					},
				},
				now: func() time.Time { return now },
			},
			signingKey: loadRSAPrivKey(t, "testdata/rsa_1.pem", jose.RS256),
			pubKeys: []*jose.JSONWebKey{
				loadRSAKey(t, "testdata/rsa_1.pem", jose.RS256),
			},
			// string value for "email_verified"
			claims: fmt.Sprintf(`{
				"iss": "https://auth.example.com",
				"aud": "my-client",
				"email": "jane@example.com",
				"email_verified": "false",
				"exp": %d
			}`, valid.Unix()),
			wantErr: "oidc: parse 'email_verified' claim: json: cannot unmarshal string into Go value of type bool",
		},
		{
			name: "groups",
			options: Options{
				JWTAuthenticator: apiserver.JWTAuthenticator{
					Issuer: apiserver.Issuer{
						URL:       "https://auth.example.com",
						Audiences: []string{"my-client"},
					},
					ClaimMappings: apiserver.ClaimMappings{
						Username: apiserver.PrefixedClaimOrExpression{
							Claim:  "username",
							Prefix: pointer.String(""),
						},
						Groups: apiserver.PrefixedClaimOrExpression{
							Claim:  "groups",
							Prefix: pointer.String(""),
						},
					},
				},
				now: func() time.Time { return now },
			},
			signingKey: loadRSAPrivKey(t, "testdata/rsa_1.pem", jose.RS256),
			pubKeys: []*jose.JSONWebKey{
				loadRSAKey(t, "testdata/rsa_1.pem", jose.RS256),
			},
			claims: fmt.Sprintf(`{
				"iss": "https://auth.example.com",
				"aud": "my-client",
				"username": "jane",
				"groups": ["team1", "team2"],
				"exp": %d
			}`, valid.Unix()),
			want: &user.DefaultInfo{
				Name:   "jane",
				Groups: []string{"team1", "team2"},
			},
		},
		{
			name: "groups-distributed",
			options: Options{
				JWTAuthenticator: apiserver.JWTAuthenticator{
					Issuer: apiserver.Issuer{
						URL:       "{{.URL}}",
						Audiences: []string{"my-client"},
					},
					ClaimMappings: apiserver.ClaimMappings{
						Username: apiserver.PrefixedClaimOrExpression{
							Claim:  "username",
							Prefix: pointer.String(""),
						},
						Groups: apiserver.PrefixedClaimOrExpression{
							Claim:  "groups",
							Prefix: pointer.String(""),
						},
					},
				},
				now: func() time.Time { return now },
			},
			signingKey: loadRSAPrivKey(t, "testdata/rsa_1.pem", jose.RS256),
			pubKeys: []*jose.JSONWebKey{
				loadRSAKey(t, "testdata/rsa_1.pem", jose.RS256),
			},
			claims: fmt.Sprintf(`{
				"iss": "{{.URL}}",
				"aud": "my-client",
				"username": "jane",
				"_claim_names": {
						"groups": "src1"
				},
				"_claim_sources": {
						"src1": {
								"endpoint": "{{.URL}}/groups",
								"access_token": "groups_token"
						}
				},
				"exp": %d
			}`, valid.Unix()),
			claimToResponseMap: map[string]string{
				"groups": fmt.Sprintf(`{
					"iss": "{{.URL}}",
				    "aud": "my-client",
					"groups": ["team1", "team2"],
					"exp": %d
			     }`, valid.Unix()),
			},
			openIDConfig: `{
					"issuer": "{{.URL}}",
					"jwks_uri": "{{.URL}}/.testing/keys"
			}`,
			want: &user.DefaultInfo{
				Name:   "jane",
				Groups: []string{"team1", "team2"},
			},
		},
		{
			name: "groups-distributed invalid client",
			options: Options{
				JWTAuthenticator: apiserver.JWTAuthenticator{
					Issuer: apiserver.Issuer{
						URL:       "{{.URL}}",
						Audiences: []string{"my-client"},
					},
					ClaimMappings: apiserver.ClaimMappings{
						Username: apiserver.PrefixedClaimOrExpression{
							Claim:  "username",
							Prefix: pointer.String(""),
						},
						Groups: apiserver.PrefixedClaimOrExpression{
							Claim:  "groups",
							Prefix: pointer.String(""),
						},
					},
				},
				Client: &http.Client{Transport: errTransport("some unexpected oidc error")}, // return an error that we can assert against
				now:    func() time.Time { return now },
			},
			signingKey: loadRSAPrivKey(t, "testdata/rsa_1.pem", jose.RS256),
			pubKeys: []*jose.JSONWebKey{
				loadRSAKey(t, "testdata/rsa_1.pem", jose.RS256),
			},
			claims: fmt.Sprintf(`{
				"iss": "{{.URL}}",
				"aud": "my-client",
				"username": "jane",
				"_claim_names": {
						"groups": "src1"
				},
				"_claim_sources": {
						"src1": {
								"endpoint": "{{.URL}}/groups",
								"access_token": "groups_token"
						}
				},
				"exp": %d
			}`, valid.Unix()),
			claimToResponseMap: map[string]string{
				"groups": fmt.Sprintf(`{
					"iss": "{{.URL}}",
				    "aud": "my-client",
					"groups": ["team1", "team2"],
					"exp": %d
			     }`, valid.Unix()),
			},
			openIDConfig: `{
					"issuer": "{{.URL}}",
					"jwks_uri": "{{.URL}}/.testing/keys"
			}`,
			optsFunc: func(opts *Options) {
				opts.CAContentProvider = nil // unset CA automatically set by the test to allow us to use a custom client
			},
			wantErr: `oidc: could not expand distributed claims: while getting distributed claim "groups": Get "{{.URL}}/groups": some unexpected oidc error`,
		},
		{
			name: "groups-distributed-malformed-claim-names",
			options: Options{
				JWTAuthenticator: apiserver.JWTAuthenticator{
					Issuer: apiserver.Issuer{
						URL:       "{{.URL}}",
						Audiences: []string{"my-client"},
					},
					ClaimMappings: apiserver.ClaimMappings{
						Username: apiserver.PrefixedClaimOrExpression{
							Claim:  "username",
							Prefix: pointer.String(""),
						},
						Groups: apiserver.PrefixedClaimOrExpression{
							Claim:  "groups",
							Prefix: pointer.String(""),
						},
					},
				},
				now: func() time.Time { return now },
			},
			signingKey: loadRSAPrivKey(t, "testdata/rsa_1.pem", jose.RS256),
			pubKeys: []*jose.JSONWebKey{
				loadRSAKey(t, "testdata/rsa_1.pem", jose.RS256),
			},
			claims: fmt.Sprintf(`{
				"iss": "{{.URL}}",
				"aud": "my-client",
				"username": "jane",
				"_claim_names": {
						"groups": "nonexistent-claim-source"
				},
				"_claim_sources": {
						"src1": {
								"endpoint": "{{.URL}}/groups",
								"access_token": "groups_token"
						}
				},
				"exp": %d
			}`, valid.Unix()),
			claimToResponseMap: map[string]string{
				"groups": fmt.Sprintf(`{
					"iss": "{{.URL}}",
				    "aud": "my-client",
					"groups": ["team1", "team2"],
					"exp": %d
			     }`, valid.Unix()),
			},
			openIDConfig: `{
					"issuer": "{{.URL}}",
					"jwks_uri": "{{.URL}}/.testing/keys"
			}`,
			wantErr: "oidc: verify token: oidc: source does not exist",
		},
		{
			name: "groups-distributed-malformed-names-and-sources",
			options: Options{
				JWTAuthenticator: apiserver.JWTAuthenticator{
					Issuer: apiserver.Issuer{
						URL:       "{{.URL}}",
						Audiences: []string{"my-client"},
					},
					ClaimMappings: apiserver.ClaimMappings{
						Username: apiserver.PrefixedClaimOrExpression{
							Claim:  "username",
							Prefix: pointer.String(""),
						},
						Groups: apiserver.PrefixedClaimOrExpression{
							Claim:  "groups",
							Prefix: pointer.String(""),
						},
					},
				},
				now: func() time.Time { return now },
			},
			signingKey: loadRSAPrivKey(t, "testdata/rsa_1.pem", jose.RS256),
			pubKeys: []*jose.JSONWebKey{
				loadRSAKey(t, "testdata/rsa_1.pem", jose.RS256),
			},
			claims: fmt.Sprintf(`{
				"iss": "{{.URL}}",
				"aud": "my-client",
				"username": "jane",
				"_claim_names": {
						"groups": "src1"
				},
				"exp": %d
			}`, valid.Unix()),
			claimToResponseMap: map[string]string{
				"groups": fmt.Sprintf(`{
					"iss": "{{.URL}}",
				    "aud": "my-client",
					"groups": ["team1", "team2"],
					"exp": %d
			     }`, valid.Unix()),
			},
			openIDConfig: `{
					"issuer": "{{.URL}}",
					"jwks_uri": "{{.URL}}/.testing/keys"
			}`,
			wantErr: "oidc: verify token: oidc: source does not exist",
		},
		{
			name: "groups-distributed-malformed-distributed-claim",
			options: Options{
				JWTAuthenticator: apiserver.JWTAuthenticator{
					Issuer: apiserver.Issuer{
						URL:       "{{.URL}}",
						Audiences: []string{"my-client"},
					},
					ClaimMappings: apiserver.ClaimMappings{
						Username: apiserver.PrefixedClaimOrExpression{
							Claim:  "username",
							Prefix: pointer.String(""),
						},
						Groups: apiserver.PrefixedClaimOrExpression{
							Claim:  "groups",
							Prefix: pointer.String(""),
						},
					},
				},
				now: func() time.Time { return now },
			},
			signingKey: loadRSAPrivKey(t, "testdata/rsa_1.pem", jose.RS256),
			pubKeys: []*jose.JSONWebKey{
				loadRSAKey(t, "testdata/rsa_1.pem", jose.RS256),
			},
			claims: fmt.Sprintf(`{
				"iss": "{{.URL}}",
				"aud": "my-client",
				"username": "jane",
				"_claim_names": {
						"groups": "src1"
				},
				"_claim_sources": {
						"src1": {
								"endpoint": "{{.URL}}/groups",
								"access_token": "groups_token"
						}
				},
				"exp": %d
			}`, valid.Unix()),
			claimToResponseMap: map[string]string{
				// Doesn't contain the "groups" claim as it promises.
				"groups": fmt.Sprintf(`{
					"iss": "{{.URL}}",
				    "aud": "my-client",
					"exp": %d
			     }`, valid.Unix()),
			},
			openIDConfig: `{
					"issuer": "{{.URL}}",
					"jwks_uri": "{{.URL}}/.testing/keys"
			}`,
			wantErr: `oidc: could not expand distributed claims: jwt returned by distributed claim endpoint "{{.URL}}/groups" did not contain claim: groups`,
		},
		{
			name: "groups-distributed-unusual-name",
			options: Options{
				JWTAuthenticator: apiserver.JWTAuthenticator{
					Issuer: apiserver.Issuer{
						URL:       "{{.URL}}",
						Audiences: []string{"my-client"},
					},
					ClaimMappings: apiserver.ClaimMappings{
						Username: apiserver.PrefixedClaimOrExpression{
							Claim:  "username",
							Prefix: pointer.String(""),
						},
						Groups: apiserver.PrefixedClaimOrExpression{
							Claim:  "rabbits",
							Prefix: pointer.String(""),
						},
					},
				},
				now: func() time.Time { return now },
			},
			signingKey: loadRSAPrivKey(t, "testdata/rsa_1.pem", jose.RS256),
			pubKeys: []*jose.JSONWebKey{
				loadRSAKey(t, "testdata/rsa_1.pem", jose.RS256),
			},
			claims: fmt.Sprintf(`{
				"iss": "{{.URL}}",
				"aud": "my-client",
				"username": "jane",
				"_claim_names": {
						"rabbits": "src1"
				},
				"_claim_sources": {
						"src1": {
								"endpoint": "{{.URL}}/rabbits",
								"access_token": "rabbits_token"
						}
				},
				"exp": %d
			}`, valid.Unix()),
			claimToResponseMap: map[string]string{
				"rabbits": fmt.Sprintf(`{
					"iss": "{{.URL}}",
				    "aud": "my-client",
					"rabbits": ["team1", "team2"],
					"exp": %d
			     }`, valid.Unix()),
			},
			openIDConfig: `{
					"issuer": "{{.URL}}",
					"jwks_uri": "{{.URL}}/.testing/keys"
			}`,
			want: &user.DefaultInfo{
				Name:   "jane",
				Groups: []string{"team1", "team2"},
			},
		},
		{
			name: "groups-distributed-wrong-audience",
			options: Options{
				JWTAuthenticator: apiserver.JWTAuthenticator{
					Issuer: apiserver.Issuer{
						URL:       "{{.URL}}",
						Audiences: []string{"my-client"},
					},
					ClaimMappings: apiserver.ClaimMappings{
						Username: apiserver.PrefixedClaimOrExpression{
							Claim:  "username",
							Prefix: pointer.String(""),
						},
						Groups: apiserver.PrefixedClaimOrExpression{
							Claim:  "groups",
							Prefix: pointer.String(""),
						},
					},
				},
				now: func() time.Time { return now },
			},
			signingKey: loadRSAPrivKey(t, "testdata/rsa_1.pem", jose.RS256),
			pubKeys: []*jose.JSONWebKey{
				loadRSAKey(t, "testdata/rsa_1.pem", jose.RS256),
			},
			claims: fmt.Sprintf(`{
				"iss": "{{.URL}}",
				"aud": "my-client",
				"username": "jane",
				"_claim_names": {
						"groups": "src1"
				},
				"_claim_sources": {
						"src1": {
								"endpoint": "{{.URL}}/groups",
								"access_token": "groups_token"
						}
				},
				"exp": %d
			}`, valid.Unix()),
			claimToResponseMap: map[string]string{
				// Note mismatching "aud"
				"groups": fmt.Sprintf(`{
					"iss": "{{.URL}}",
				    "aud": "your-client",
					"groups": ["team1", "team2"],
					"exp": %d
			     }`, valid.Unix()),
			},
			openIDConfig: `{
					"issuer": "{{.URL}}",
					"jwks_uri": "{{.URL}}/.testing/keys"
			}`,
			wantErr: `oidc: could not expand distributed claims: verify distributed claim token: oidc: expected audience "my-client" got ["your-client"]`,
		},
		{
			name: "groups-distributed-expired-token",
			options: Options{
				JWTAuthenticator: apiserver.JWTAuthenticator{
					Issuer: apiserver.Issuer{
						URL:       "{{.URL}}",
						Audiences: []string{"my-client"},
					},
					ClaimMappings: apiserver.ClaimMappings{
						Username: apiserver.PrefixedClaimOrExpression{
							Claim:  "username",
							Prefix: pointer.String(""),
						},
						Groups: apiserver.PrefixedClaimOrExpression{
							Claim:  "groups",
							Prefix: pointer.String(""),
						},
					},
				},
				now: func() time.Time { return now },
			},
			signingKey: loadRSAPrivKey(t, "testdata/rsa_1.pem", jose.RS256),
			pubKeys: []*jose.JSONWebKey{
				loadRSAKey(t, "testdata/rsa_1.pem", jose.RS256),
			},
			claims: fmt.Sprintf(`{
				"iss": "{{.URL}}",
				"aud": "my-client",
				"username": "jane",
				"_claim_names": {
						"groups": "src1"
				},
				"_claim_sources": {
						"src1": {
								"endpoint": "{{.URL}}/groups",
								"access_token": "groups_token"
						}
				},
				"exp": %d
			}`, valid.Unix()),
			claimToResponseMap: map[string]string{
				// Note expired timestamp.
				"groups": fmt.Sprintf(`{
					"iss": "{{.URL}}",
				    "aud": "my-client",
					"groups": ["team1", "team2"],
					"exp": %d
			     }`, expired.Unix()),
			},
			openIDConfig: `{
					"issuer": "{{.URL}}",
					"jwks_uri": "{{.URL}}/.testing/keys"
			}`,
			wantErr: "oidc: could not expand distributed claims: verify distributed claim token: oidc: token is expired (Token Expiry: {{.Expired}})",
		},
		{
			// Specs are unclear about this behavior.  We adopt a behavior where
			// normal claim wins over a distributed claim by the same name.
			name: "groups-distributed-normal-claim-wins",
			options: Options{
				JWTAuthenticator: apiserver.JWTAuthenticator{
					Issuer: apiserver.Issuer{
						URL:       "{{.URL}}",
						Audiences: []string{"my-client"},
					},
					ClaimMappings: apiserver.ClaimMappings{
						Username: apiserver.PrefixedClaimOrExpression{
							Claim:  "username",
							Prefix: pointer.String(""),
						},
						Groups: apiserver.PrefixedClaimOrExpression{
							Claim:  "groups",
							Prefix: pointer.String(""),
						},
					},
				},
				now: func() time.Time { return now },
			},
			signingKey: loadRSAPrivKey(t, "testdata/rsa_1.pem", jose.RS256),
			pubKeys: []*jose.JSONWebKey{
				loadRSAKey(t, "testdata/rsa_1.pem", jose.RS256),
			},
			claims: fmt.Sprintf(`{
				"iss": "{{.URL}}",
				"aud": "my-client",
				"username": "jane",
				"groups": "team1",
				"_claim_names": {
						"groups": "src1"
				},
				"_claim_sources": {
						"src1": {
								"endpoint": "{{.URL}}/groups",
								"access_token": "groups_token"
						}
				},
				"exp": %d
			}`, valid.Unix()),
			claimToResponseMap: map[string]string{
				"groups": fmt.Sprintf(`{
					"iss": "{{.URL}}",
				    "aud": "my-client",
					"groups": ["team2"],
					"exp": %d
			     }`, valid.Unix()),
			},
			openIDConfig: `{
					"issuer": "{{.URL}}",
					"jwks_uri": "{{.URL}}/.testing/keys"
			}`,
			want: &user.DefaultInfo{
				Name: "jane",
				// "team1" is from the normal "groups" claim.
				Groups: []string{"team1"},
			},
		},
		{
			// Groups should be able to be a single string, not just a slice.
			name: "group-string-claim",
			options: Options{
				JWTAuthenticator: apiserver.JWTAuthenticator{
					Issuer: apiserver.Issuer{
						URL:       "https://auth.example.com",
						Audiences: []string{"my-client"},
					},
					ClaimMappings: apiserver.ClaimMappings{
						Username: apiserver.PrefixedClaimOrExpression{
							Claim:  "username",
							Prefix: pointer.String(""),
						},
						Groups: apiserver.PrefixedClaimOrExpression{
							Claim:  "groups",
							Prefix: pointer.String(""),
						},
					},
				},
				now: func() time.Time { return now },
			},
			signingKey: loadRSAPrivKey(t, "testdata/rsa_1.pem", jose.RS256),
			pubKeys: []*jose.JSONWebKey{
				loadRSAKey(t, "testdata/rsa_1.pem", jose.RS256),
			},
			claims: fmt.Sprintf(`{
				"iss": "https://auth.example.com",
				"aud": "my-client",
				"username": "jane",
				"groups": "team1",
				"exp": %d
			}`, valid.Unix()),
			want: &user.DefaultInfo{
				Name:   "jane",
				Groups: []string{"team1"},
			},
		},
		{
			// Groups should be able to be a single string, not just a slice.
			name: "group-string-claim-distributed",
			options: Options{
				JWTAuthenticator: apiserver.JWTAuthenticator{
					Issuer: apiserver.Issuer{
						URL:       "{{.URL}}",
						Audiences: []string{"my-client"},
					},
					ClaimMappings: apiserver.ClaimMappings{
						Username: apiserver.PrefixedClaimOrExpression{
							Claim:  "username",
							Prefix: pointer.String(""),
						},
						Groups: apiserver.PrefixedClaimOrExpression{
							Claim:  "groups",
							Prefix: pointer.String(""),
						},
					},
				},
				now: func() time.Time { return now },
			},
			signingKey: loadRSAPrivKey(t, "testdata/rsa_1.pem", jose.RS256),
			pubKeys: []*jose.JSONWebKey{
				loadRSAKey(t, "testdata/rsa_1.pem", jose.RS256),
			},
			claims: fmt.Sprintf(`{
				"iss": "{{.URL}}",
				"aud": "my-client",
				"username": "jane",
				"_claim_names": {
						"groups": "src1"
				},
				"_claim_sources": {
						"src1": {
								"endpoint": "{{.URL}}/groups",
								"access_token": "groups_token"
						}
				},
				"exp": %d
			}`, valid.Unix()),
			claimToResponseMap: map[string]string{
				"groups": fmt.Sprintf(`{
					"iss": "{{.URL}}",
				    "aud": "my-client",
					"groups": "team1",
					"exp": %d
			     }`, valid.Unix()),
			},
			openIDConfig: `{
					"issuer": "{{.URL}}",
					"jwks_uri": "{{.URL}}/.testing/keys"
			}`,
			want: &user.DefaultInfo{
				Name:   "jane",
				Groups: []string{"team1"},
			},
		},
		{
			name: "group-string-claim-aggregated-not-supported",
			options: Options{
				JWTAuthenticator: apiserver.JWTAuthenticator{
					Issuer: apiserver.Issuer{
						URL:       "https://auth.example.com",
						Audiences: []string{"my-client"},
					},
					ClaimMappings: apiserver.ClaimMappings{
						Username: apiserver.PrefixedClaimOrExpression{
							Claim:  "username",
							Prefix: pointer.String(""),
						},
						Groups: apiserver.PrefixedClaimOrExpression{
							Claim:  "groups",
							Prefix: pointer.String(""),
						},
					},
				},
				now: func() time.Time { return now },
			},
			signingKey: loadRSAPrivKey(t, "testdata/rsa_1.pem", jose.RS256),
			pubKeys: []*jose.JSONWebKey{
				loadRSAKey(t, "testdata/rsa_1.pem", jose.RS256),
			},
			claims: fmt.Sprintf(`{
				"iss": "https://auth.example.com",
				"aud": "my-client",
				"username": "jane",
				"_claim_names": {
						"groups": "src1"
				},
				"_claim_sources": {
						"src1": {
								"JWT": "some.jwt.token"
						}
				},
				"exp": %d
			}`, valid.Unix()),
			want: &user.DefaultInfo{
				Name: "jane",
			},
		},
		{
			// if the groups claim isn't provided, this shouldn't error out
			name: "no-groups-claim",
			options: Options{
				JWTAuthenticator: apiserver.JWTAuthenticator{
					Issuer: apiserver.Issuer{
						URL:       "https://auth.example.com",
						Audiences: []string{"my-client"},
					},
					ClaimMappings: apiserver.ClaimMappings{
						Username: apiserver.PrefixedClaimOrExpression{
							Claim:  "username",
							Prefix: pointer.String(""),
						},
						Groups: apiserver.PrefixedClaimOrExpression{
							Claim:  "groups",
							Prefix: pointer.String(""),
						},
					},
				},
				now: func() time.Time { return now },
			},
			signingKey: loadRSAPrivKey(t, "testdata/rsa_1.pem", jose.RS256),
			pubKeys: []*jose.JSONWebKey{
				loadRSAKey(t, "testdata/rsa_1.pem", jose.RS256),
			},
			claims: fmt.Sprintf(`{
				"iss": "https://auth.example.com",
				"aud": "my-client",
				"username": "jane",
				"exp": %d
			}`, valid.Unix()),
			want: &user.DefaultInfo{
				Name: "jane",
			},
		},
		{
			name: "invalid-groups-claim",
			options: Options{
				JWTAuthenticator: apiserver.JWTAuthenticator{
					Issuer: apiserver.Issuer{
						URL:       "https://auth.example.com",
						Audiences: []string{"my-client"},
					},
					ClaimMappings: apiserver.ClaimMappings{
						Username: apiserver.PrefixedClaimOrExpression{
							Claim:  "username",
							Prefix: pointer.String(""),
						},
						Groups: apiserver.PrefixedClaimOrExpression{
							Claim:  "groups",
							Prefix: pointer.String(""),
						},
					},
				},
				now: func() time.Time { return now },
			},
			signingKey: loadRSAPrivKey(t, "testdata/rsa_1.pem", jose.RS256),
			pubKeys: []*jose.JSONWebKey{
				loadRSAKey(t, "testdata/rsa_1.pem", jose.RS256),
			},
			claims: fmt.Sprintf(`{
				"iss": "https://auth.example.com",
				"aud": "my-client",
				"username": "jane",
				"groups": 42,
				"exp": %d
			}`, valid.Unix()),
			wantErr: `oidc: parse groups claim "groups": json: cannot unmarshal number into Go value of type string`,
		},
		{
			name: "required-claim",
			options: Options{
				JWTAuthenticator: apiserver.JWTAuthenticator{
					Issuer: apiserver.Issuer{
						URL:       "https://auth.example.com",
						Audiences: []string{"my-client"},
					},
					ClaimMappings: apiserver.ClaimMappings{
						Username: apiserver.PrefixedClaimOrExpression{
							Claim:  "username",
							Prefix: pointer.String(""),
						},
						Groups: apiserver.PrefixedClaimOrExpression{
							Claim:  "groups",
							Prefix: pointer.String(""),
						},
					},
					ClaimValidationRules: []apiserver.ClaimValidationRule{
						{
							Claim:         "hd",
							RequiredValue: "example.com",
						},
						{
							Claim:         "sub",
							RequiredValue: "test",
						},
					},
				},
				now: func() time.Time { return now },
			},
			signingKey: loadRSAPrivKey(t, "testdata/rsa_1.pem", jose.RS256),
			pubKeys: []*jose.JSONWebKey{
				loadRSAKey(t, "testdata/rsa_1.pem", jose.RS256),
			},
			claims: fmt.Sprintf(`{
				"iss": "https://auth.example.com",
				"aud": "my-client",
				"username": "jane",
				"hd": "example.com",
				"sub": "test",
				"exp": %d
			}`, valid.Unix()),
			want: &user.DefaultInfo{
				Name: "jane",
			},
		},
		{
			name: "no-required-claim",
			options: Options{
				JWTAuthenticator: apiserver.JWTAuthenticator{
					Issuer: apiserver.Issuer{
						URL:       "https://auth.example.com",
						Audiences: []string{"my-client"},
					},
					ClaimMappings: apiserver.ClaimMappings{
						Username: apiserver.PrefixedClaimOrExpression{
							Claim:  "username",
							Prefix: pointer.String(""),
						},
						Groups: apiserver.PrefixedClaimOrExpression{
							Claim:  "groups",
							Prefix: pointer.String(""),
						},
					},
					ClaimValidationRules: []apiserver.ClaimValidationRule{
						{
							Claim:         "hd",
							RequiredValue: "example.com",
						},
					},
				},
				now: func() time.Time { return now },
			},
			signingKey: loadRSAPrivKey(t, "testdata/rsa_1.pem", jose.RS256),
			pubKeys: []*jose.JSONWebKey{
				loadRSAKey(t, "testdata/rsa_1.pem", jose.RS256),
			},
			claims: fmt.Sprintf(`{
				"iss": "https://auth.example.com",
				"aud": "my-client",
				"username": "jane",
				"exp": %d
			}`, valid.Unix()),
			wantErr: "oidc: required claim hd not present in ID token",
		},
		{
			name: "invalid-required-claim",
			options: Options{
				JWTAuthenticator: apiserver.JWTAuthenticator{
					Issuer: apiserver.Issuer{
						URL:       "https://auth.example.com",
						Audiences: []string{"my-client"},
					},
					ClaimMappings: apiserver.ClaimMappings{
						Username: apiserver.PrefixedClaimOrExpression{
							Claim:  "username",
							Prefix: pointer.String(""),
						},
						Groups: apiserver.PrefixedClaimOrExpression{
							Claim:  "groups",
							Prefix: pointer.String(""),
						},
					},
					ClaimValidationRules: []apiserver.ClaimValidationRule{
						{
							Claim:         "hd",
							RequiredValue: "example.com",
						},
					},
				},
				now: func() time.Time { return now },
			},
			signingKey: loadRSAPrivKey(t, "testdata/rsa_1.pem", jose.RS256),
			pubKeys: []*jose.JSONWebKey{
				loadRSAKey(t, "testdata/rsa_1.pem", jose.RS256),
			},
			claims: fmt.Sprintf(`{
				"iss": "https://auth.example.com",
				"aud": "my-client",
				"username": "jane",
				"hd": "example.org",
				"exp": %d
			}`, valid.Unix()),
			wantErr: "oidc: required claim hd value does not match. Got = example.org, want = example.com",
		},
		{
			name: "invalid-signature",
			options: Options{
				JWTAuthenticator: apiserver.JWTAuthenticator{
					Issuer: apiserver.Issuer{
						URL:       "https://auth.example.com",
						Audiences: []string{"my-client"},
					},
					ClaimMappings: apiserver.ClaimMappings{
						Username: apiserver.PrefixedClaimOrExpression{
							Claim:  "username",
							Prefix: pointer.String("prefix:"),
						},
					},
				},
				now: func() time.Time { return now },
			},
			signingKey: loadRSAPrivKey(t, "testdata/rsa_1.pem", jose.RS256),
			pubKeys: []*jose.JSONWebKey{
				loadRSAKey(t, "testdata/rsa_2.pem", jose.RS256),
			},
			claims: fmt.Sprintf(`{
				"iss": "https://auth.example.com",
				"aud": "my-client",
				"username": "jane",
				"exp": %d
			}`, valid.Unix()),
			wantErr: "oidc: verify token: failed to verify signature: no keys matches jwk keyid",
		},
		{
			name: "expired",
			options: Options{
				JWTAuthenticator: apiserver.JWTAuthenticator{
					Issuer: apiserver.Issuer{
						URL:       "https://auth.example.com",
						Audiences: []string{"my-client"},
					},
					ClaimMappings: apiserver.ClaimMappings{
						Username: apiserver.PrefixedClaimOrExpression{
							Claim:  "username",
							Prefix: pointer.String("prefix:"),
						},
					},
				},
				now: func() time.Time { return now },
			},
			signingKey: loadRSAPrivKey(t, "testdata/rsa_1.pem", jose.RS256),
			pubKeys: []*jose.JSONWebKey{
				loadRSAKey(t, "testdata/rsa_1.pem", jose.RS256),
			},
			claims: fmt.Sprintf(`{
				"iss": "https://auth.example.com",
				"aud": "my-client",
				"username": "jane",
				"exp": %d
			}`, expired.Unix()),
			wantErr: `oidc: verify token: oidc: token is expired (Token Expiry: {{.Expired}})`,
		},
		{
			name: "invalid-aud",
			options: Options{
				JWTAuthenticator: apiserver.JWTAuthenticator{
					Issuer: apiserver.Issuer{
						URL:       "https://auth.example.com",
						Audiences: []string{"my-client"},
					},
					ClaimMappings: apiserver.ClaimMappings{
						Username: apiserver.PrefixedClaimOrExpression{
							Claim:  "username",
							Prefix: pointer.String("prefix:"),
						},
					},
				},
				now: func() time.Time { return now },
			},
			signingKey: loadRSAPrivKey(t, "testdata/rsa_1.pem", jose.RS256),
			pubKeys: []*jose.JSONWebKey{
				loadRSAKey(t, "testdata/rsa_1.pem", jose.RS256),
			},
			claims: fmt.Sprintf(`{
				"iss": "https://auth.example.com",
				"aud": "not-my-client",
				"username": "jane",
				"exp": %d
			}`, valid.Unix()),
			wantErr: `oidc: verify token: oidc: expected audience "my-client" got ["not-my-client"]`,
		},
		{
			// ID tokens may contain multiple audiences:
			// https://openid.net/specs/openid-connect-core-1_0.html#IDToken
			name: "multiple-audiences",
			options: Options{
				JWTAuthenticator: apiserver.JWTAuthenticator{
					Issuer: apiserver.Issuer{
						URL:       "https://auth.example.com",
						Audiences: []string{"my-client"},
					},
					ClaimMappings: apiserver.ClaimMappings{
						Username: apiserver.PrefixedClaimOrExpression{
							Claim:  "username",
							Prefix: pointer.String(""),
						},
					},
				},
				now: func() time.Time { return now },
			},
			signingKey: loadRSAPrivKey(t, "testdata/rsa_1.pem", jose.RS256),
			pubKeys: []*jose.JSONWebKey{
				loadRSAKey(t, "testdata/rsa_1.pem", jose.RS256),
			},
			claims: fmt.Sprintf(`{
				"iss": "https://auth.example.com",
				"aud": ["not-my-client", "my-client"],
				"azp": "not-my-client",
				"username": "jane",
				"exp": %d
			}`, valid.Unix()),
			want: &user.DefaultInfo{
				Name: "jane",
			},
		},
		{
			name: "multiple-audiences in authentication config",
			options: Options{
				JWTAuthenticator: apiserver.JWTAuthenticator{
					Issuer: apiserver.Issuer{
						URL:                 "https://auth.example.com",
						Audiences:           []string{"random-client", "my-client"},
						AudienceMatchPolicy: "MatchAny",
					},
					ClaimMappings: apiserver.ClaimMappings{
						Username: apiserver.PrefixedClaimOrExpression{
							Claim:  "username",
							Prefix: pointer.String(""),
						},
					},
				},
				now: func() time.Time { return now },
			},
			signingKey: loadRSAPrivKey(t, "testdata/rsa_1.pem", jose.RS256),
			pubKeys: []*jose.JSONWebKey{
				loadRSAKey(t, "testdata/rsa_1.pem", jose.RS256),
			},
			claims: fmt.Sprintf(`{
				"iss": "https://auth.example.com",
				"aud": ["not-my-client", "my-client"],
				"azp": "not-my-client",
				"username": "jane",
				"exp": %d
			}`, valid.Unix()),
			want: &user.DefaultInfo{
				Name: "jane",
			},
		},
		{
			name: "multiple-audiences in authentication config, multiple matches",
			options: Options{
				JWTAuthenticator: apiserver.JWTAuthenticator{
					Issuer: apiserver.Issuer{
						URL:                 "https://auth.example.com",
						Audiences:           []string{"random-client", "my-client", "other-client"},
						AudienceMatchPolicy: "MatchAny",
					},
					ClaimMappings: apiserver.ClaimMappings{
						Username: apiserver.PrefixedClaimOrExpression{
							Claim:  "username",
							Prefix: pointer.String(""),
						},
					},
				},
				now: func() time.Time { return now },
			},
			signingKey: loadRSAPrivKey(t, "testdata/rsa_1.pem", jose.RS256),
			pubKeys: []*jose.JSONWebKey{
				loadRSAKey(t, "testdata/rsa_1.pem", jose.RS256),
			},
			claims: fmt.Sprintf(`{
				"iss": "https://auth.example.com",
				"aud": ["not-my-client", "my-client", "other-client"],
				"azp": "not-my-client",
				"username": "jane",
				"exp": %d
			}`, valid.Unix()),
			want: &user.DefaultInfo{
				Name: "jane",
			},
		},
		{
			name: "multiple-audiences in authentication config, no match",
			options: Options{
				JWTAuthenticator: apiserver.JWTAuthenticator{
					Issuer: apiserver.Issuer{
						URL:                 "https://auth.example.com",
						Audiences:           []string{"random-client", "my-client"},
						AudienceMatchPolicy: "MatchAny",
					},
					ClaimMappings: apiserver.ClaimMappings{
						Username: apiserver.PrefixedClaimOrExpression{
							Claim:  "username",
							Prefix: pointer.String(""),
						},
					},
				},
				now: func() time.Time { return now },
			},
			signingKey: loadRSAPrivKey(t, "testdata/rsa_1.pem", jose.RS256),
			pubKeys: []*jose.JSONWebKey{
				loadRSAKey(t, "testdata/rsa_1.pem", jose.RS256),
			},
			claims: fmt.Sprintf(`{
				"iss": "https://auth.example.com",
				"aud": ["not-my-client"],
				"azp": "not-my-client",
				"username": "jane",
				"exp": %d
			}`, valid.Unix()),
			wantErr: `oidc: verify token: oidc: expected audience in ["my-client" "random-client"] got ["not-my-client"]`,
		},
		{
			name: "nuanced audience validation using claim validation rules",
			options: Options{
				JWTAuthenticator: apiserver.JWTAuthenticator{
					Issuer: apiserver.Issuer{
						URL:                 "https://auth.example.com",
						Audiences:           []string{"bar", "foo", "baz"},
						AudienceMatchPolicy: "MatchAny",
					},
					ClaimMappings: apiserver.ClaimMappings{
						Username: apiserver.PrefixedClaimOrExpression{
							Claim:  "username",
							Prefix: pointer.String(""),
						},
					},
					ClaimValidationRules: []apiserver.ClaimValidationRule{
						{
							Expression: `sets.equivalent(claims.aud, ["bar", "foo", "baz"])`,
							Message:    "audience must exactly contain [bar, foo, baz]",
						},
					},
				},
				now: func() time.Time { return now },
			},
			signingKey: loadRSAPrivKey(t, "testdata/rsa_1.pem", jose.RS256),
			pubKeys: []*jose.JSONWebKey{
				loadRSAKey(t, "testdata/rsa_1.pem", jose.RS256),
			},
			claims: fmt.Sprintf(`{
				"iss": "https://auth.example.com",
				"aud": ["foo", "bar", "baz"],
				"azp": "not-my-client",
				"username": "jane",
				"exp": %d
			}`, valid.Unix()),
			want: &user.DefaultInfo{
				Name: "jane",
			},
		},
		{
			name: "audience validation using claim validation rules fails",
			options: Options{
				JWTAuthenticator: apiserver.JWTAuthenticator{
					Issuer: apiserver.Issuer{
						URL:                 "https://auth.example.com",
						Audiences:           []string{"bar", "foo", "baz"},
						AudienceMatchPolicy: "MatchAny",
					},
					ClaimMappings: apiserver.ClaimMappings{
						Username: apiserver.PrefixedClaimOrExpression{
							Claim:  "username",
							Prefix: pointer.String(""),
						},
					},
					ClaimValidationRules: []apiserver.ClaimValidationRule{
						{
							Expression: `sets.equivalent(claims.aud, ["bar", "foo", "baz"])`,
							Message:    "audience must exactly contain [bar, foo, baz]",
						},
					},
				},
				now: func() time.Time { return now },
			},
			signingKey: loadRSAPrivKey(t, "testdata/rsa_1.pem", jose.RS256),
			pubKeys: []*jose.JSONWebKey{
				loadRSAKey(t, "testdata/rsa_1.pem", jose.RS256),
			},
			claims: fmt.Sprintf(`{
				"iss": "https://auth.example.com",
				"aud": ["foo", "baz"],
				"azp": "not-my-client",
				"username": "jane",
				"exp": %d
			}`, valid.Unix()),
			wantErr: `oidc: error evaluating claim validation expression: validation expression 'sets.equivalent(claims.aud, ["bar", "foo", "baz"])' failed: audience must exactly contain [bar, foo, baz]`,
		},
		{
			name: "invalid-issuer",
			options: Options{
				JWTAuthenticator: apiserver.JWTAuthenticator{
					Issuer: apiserver.Issuer{
						URL:       "https://auth.example.com",
						Audiences: []string{"my-client"},
					},
					ClaimMappings: apiserver.ClaimMappings{
						Username: apiserver.PrefixedClaimOrExpression{
							Claim:  "username",
							Prefix: pointer.String("prefix:"),
						},
					},
				},
				now: func() time.Time { return now },
			},
			signingKey: loadRSAPrivKey(t, "testdata/rsa_1.pem", jose.RS256),
			pubKeys: []*jose.JSONWebKey{
				loadRSAKey(t, "testdata/rsa_1.pem", jose.RS256),
			},
			claims: fmt.Sprintf(`{
				"iss": "https://example.com",
				"aud": "my-client",
				"username": "jane",
				"exp": %d
			}`, valid.Unix()),
			wantSkip: true,
		},
		{
			name: "username-prefix",
			options: Options{
				JWTAuthenticator: apiserver.JWTAuthenticator{
					Issuer: apiserver.Issuer{
						URL:       "https://auth.example.com",
						Audiences: []string{"my-client"},
					},
					ClaimMappings: apiserver.ClaimMappings{
						Username: apiserver.PrefixedClaimOrExpression{
							Claim:  "username",
							Prefix: pointer.String("oidc:"),
						},
					},
				},
				now: func() time.Time { return now },
			},
			signingKey: loadRSAPrivKey(t, "testdata/rsa_1.pem", jose.RS256),
			pubKeys: []*jose.JSONWebKey{
				loadRSAKey(t, "testdata/rsa_1.pem", jose.RS256),
			},
			claims: fmt.Sprintf(`{
				"iss": "https://auth.example.com",
				"aud": "my-client",
				"username": "jane",
				"exp": %d
			}`, valid.Unix()),
			want: &user.DefaultInfo{
				Name: "oidc:jane",
			},
		},
		{
			name: "groups-prefix",
			options: Options{
				JWTAuthenticator: apiserver.JWTAuthenticator{
					Issuer: apiserver.Issuer{
						URL:       "https://auth.example.com",
						Audiences: []string{"my-client"},
					},
					ClaimMappings: apiserver.ClaimMappings{
						Username: apiserver.PrefixedClaimOrExpression{
							Claim:  "username",
							Prefix: pointer.String("oidc:"),
						},
						Groups: apiserver.PrefixedClaimOrExpression{
							Claim:  "groups",
							Prefix: pointer.String("groups:"),
						},
					},
				},
				now: func() time.Time { return now },
			},
			signingKey: loadRSAPrivKey(t, "testdata/rsa_1.pem", jose.RS256),
			pubKeys: []*jose.JSONWebKey{
				loadRSAKey(t, "testdata/rsa_1.pem", jose.RS256),
			},
			claims: fmt.Sprintf(`{
				"iss": "https://auth.example.com",
				"aud": "my-client",
				"username": "jane",
				"groups": ["team1", "team2"],
				"exp": %d
			}`, valid.Unix()),
			want: &user.DefaultInfo{
				Name:   "oidc:jane",
				Groups: []string{"groups:team1", "groups:team2"},
			},
		},
		{
			name: "groups-prefix-distributed",
			options: Options{
				JWTAuthenticator: apiserver.JWTAuthenticator{
					Issuer: apiserver.Issuer{
						URL:       "{{.URL}}",
						Audiences: []string{"my-client"},
					},
					ClaimMappings: apiserver.ClaimMappings{
						Username: apiserver.PrefixedClaimOrExpression{
							Claim:  "username",
							Prefix: pointer.String("oidc:"),
						},
						Groups: apiserver.PrefixedClaimOrExpression{
							Claim:  "groups",
							Prefix: pointer.String("groups:"),
						},
					},
				},
				now: func() time.Time { return now },
			},
			signingKey: loadRSAPrivKey(t, "testdata/rsa_1.pem", jose.RS256),
			pubKeys: []*jose.JSONWebKey{
				loadRSAKey(t, "testdata/rsa_1.pem", jose.RS256),
			},
			claims: fmt.Sprintf(`{
				"iss": "{{.URL}}",
				"aud": "my-client",
				"username": "jane",
				"_claim_names": {
						"groups": "src1"
				},
				"_claim_sources": {
						"src1": {
								"endpoint": "{{.URL}}/groups",
								"access_token": "groups_token"
						}
				},
				"exp": %d
			}`, valid.Unix()),
			claimToResponseMap: map[string]string{
				"groups": fmt.Sprintf(`{
					"iss": "{{.URL}}",
				    "aud": "my-client",
					"groups": ["team1", "team2"],
					"exp": %d
			     }`, valid.Unix()),
			},
			openIDConfig: `{
					"issuer": "{{.URL}}",
					"jwks_uri": "{{.URL}}/.testing/keys"
			}`,
			want: &user.DefaultInfo{
				Name:   "oidc:jane",
				Groups: []string{"groups:team1", "groups:team2"},
			},
		},
		{
			name: "invalid-signing-alg",
			options: Options{
				JWTAuthenticator: apiserver.JWTAuthenticator{
					Issuer: apiserver.Issuer{
						URL:       "https://auth.example.com",
						Audiences: []string{"my-client"},
					},
					ClaimMappings: apiserver.ClaimMappings{
						Username: apiserver.PrefixedClaimOrExpression{
							Claim:  "username",
							Prefix: pointer.String("prefix:"),
						},
					},
				},
				now: func() time.Time { return now },
			},
			// Correct key but invalid signature algorithm "PS256"
			signingKey: loadRSAPrivKey(t, "testdata/rsa_1.pem", jose.PS256),
			pubKeys: []*jose.JSONWebKey{
				loadRSAKey(t, "testdata/rsa_1.pem", jose.RS256),
			},
			claims: fmt.Sprintf(`{
				"iss": "https://auth.example.com",
				"aud": "my-client",
				"username": "jane",
				"exp": %d
			}`, valid.Unix()),
			wantErr: `oidc: verify token: oidc: id token signed with unsupported algorithm, expected ["RS256"] got "PS256"`,
		},
		{
			name: "ps256",
			options: Options{
				JWTAuthenticator: apiserver.JWTAuthenticator{
					Issuer: apiserver.Issuer{
						URL:       "https://auth.example.com",
						Audiences: []string{"my-client"},
					},
					ClaimMappings: apiserver.ClaimMappings{
						Username: apiserver.PrefixedClaimOrExpression{
							Claim:  "username",
							Prefix: pointer.String(""),
						},
					},
				},
				SupportedSigningAlgs: []string{"PS256"},
				now:                  func() time.Time { return now },
				APIServerID:          testAPIServerID,
			},
			signingKey: loadRSAPrivKey(t, "testdata/rsa_1.pem", jose.PS256),
			pubKeys: []*jose.JSONWebKey{
				loadRSAKey(t, "testdata/rsa_1.pem", jose.PS256),
			},
			claims: fmt.Sprintf(`{
				"iss": "https://auth.example.com",
				"aud": "my-client",
				"username": "jane",
				"exp": %d
			}`, valid.Unix()),
			want: &user.DefaultInfo{
				Name: "jane",
			},
		},
		{
			name: "es512",
			options: Options{
				JWTAuthenticator: apiserver.JWTAuthenticator{
					Issuer: apiserver.Issuer{
						URL:       "https://auth.example.com",
						Audiences: []string{"my-client"},
					},
					ClaimMappings: apiserver.ClaimMappings{
						Username: apiserver.PrefixedClaimOrExpression{
							Claim:  "username",
							Prefix: pointer.String(""),
						},
					},
				},
				SupportedSigningAlgs: []string{"ES512"},
				now:                  func() time.Time { return now },
				APIServerID:          testAPIServerID,
			},
			signingKey: loadECDSAPrivKey(t, "testdata/ecdsa_2.pem", jose.ES512),
			pubKeys: []*jose.JSONWebKey{
				loadECDSAKey(t, "testdata/ecdsa_1.pem", jose.ES512),
				loadECDSAKey(t, "testdata/ecdsa_2.pem", jose.ES512),
			},
			claims: fmt.Sprintf(`{
				"iss": "https://auth.example.com",
				"aud": "my-client",
				"username": "jane",
				"exp": %d
			}`, valid.Unix()),
			want: &user.DefaultInfo{
				Name: "jane",
			},
		},
		{
			name: "not-https",
			options: Options{
				JWTAuthenticator: apiserver.JWTAuthenticator{
					Issuer: apiserver.Issuer{
						URL:       "http://auth.example.com",
						Audiences: []string{"my-client"},
					},
					ClaimMappings: apiserver.ClaimMappings{
						Username: apiserver.PrefixedClaimOrExpression{
							Claim:  "username",
							Prefix: pointer.String("prefix:"),
						},
					},
				},
				now: func() time.Time { return now },
			},
			pubKeys: []*jose.JSONWebKey{
				loadRSAKey(t, "testdata/rsa_1.pem", jose.RS256),
			},
			wantInitErr: `issuer.url: Invalid value: "http://auth.example.com": URL scheme must be https`,
		},
		{
			name: "no-username-claim",
			options: Options{
				JWTAuthenticator: apiserver.JWTAuthenticator{
					Issuer: apiserver.Issuer{
						URL:       "https://auth.example.com",
						Audiences: []string{"my-client"},
					},
					ClaimMappings: apiserver.ClaimMappings{
						Username: apiserver.PrefixedClaimOrExpression{
							Prefix: pointer.String(""),
						},
					},
				},
				now: func() time.Time { return now },
			},
			pubKeys: []*jose.JSONWebKey{
				loadRSAKey(t, "testdata/rsa_1.pem", jose.RS256),
			},
			wantInitErr: `claimMappings.username: Required value: claim or expression is required`,
		},
		{
			name: "invalid-sig-alg",
			options: Options{
				JWTAuthenticator: apiserver.JWTAuthenticator{
					Issuer: apiserver.Issuer{
						URL:       "https://auth.example.com",
						Audiences: []string{"my-client"},
					},
					ClaimMappings: apiserver.ClaimMappings{
						Username: apiserver.PrefixedClaimOrExpression{
							Claim:  "username",
							Prefix: pointer.String("prefix:"),
						},
					},
				},
				SupportedSigningAlgs: []string{"HS256"},
				now:                  func() time.Time { return now },
				APIServerID:          testAPIServerID,
			},
			pubKeys: []*jose.JSONWebKey{
				loadRSAKey(t, "testdata/rsa_1.pem", jose.RS256),
			},
			wantInitErr: `oidc: unsupported signing alg: "HS256"`,
		},
		{
			name: "client and ca mutually exclusive",
			options: Options{
				JWTAuthenticator: apiserver.JWTAuthenticator{
					Issuer: apiserver.Issuer{
						URL:       "https://auth.example.com",
						Audiences: []string{"my-client"},
					},
					ClaimMappings: apiserver.ClaimMappings{
						Username: apiserver.PrefixedClaimOrExpression{
							Claim:  "username",
							Prefix: pointer.String("prefix:"),
						},
					},
				},
				SupportedSigningAlgs: []string{"RS256"},
				now:                  func() time.Time { return now },
				Client:               http.DefaultClient, // test automatically sets CAContentProvider
				APIServerID:          testAPIServerID,
			},
			pubKeys: []*jose.JSONWebKey{
				loadRSAKey(t, "testdata/rsa_1.pem", jose.RS256),
			},
			wantInitErr: "oidc: Client and CAContentProvider are mutually exclusive",
		},
		{
			name: "keyset and discovery URL mutually exclusive",
			options: Options{
				JWTAuthenticator: apiserver.JWTAuthenticator{
					Issuer: apiserver.Issuer{
						URL:          "https://auth.example.com",
						DiscoveryURL: "https://auth.example.com/foo",
						Audiences:    []string{"my-client"},
					},
					ClaimMappings: apiserver.ClaimMappings{
						Username: apiserver.PrefixedClaimOrExpression{
							Claim:  "username",
							Prefix: pointer.String("prefix:"),
						},
					},
				},
				SupportedSigningAlgs: []string{"RS256"},
				now:                  func() time.Time { return now },
				KeySet:               &staticKeySet{},
				APIServerID:          testAPIServerID,
			},
			pubKeys: []*jose.JSONWebKey{
				loadRSAKey(t, "testdata/rsa_1.pem", jose.RS256),
			},
			wantInitErr: "oidc: KeySet and DiscoveryURL are mutually exclusive",
		},
		{
			name: "health check failure",
			options: Options{
				JWTAuthenticator: apiserver.JWTAuthenticator{
					Issuer: apiserver.Issuer{
						URL:       "https://this-will-not-work.notatld",
						Audiences: []string{"my-client"},
					},
					ClaimMappings: apiserver.ClaimMappings{
						Username: apiserver.PrefixedClaimOrExpression{
							Claim:  "username",
							Prefix: pointer.String("prefix:"),
						},
					},
				},
				SupportedSigningAlgs: []string{"RS256"},
				APIServerID:          testAPIServerID,
			},
			fetchKeysFromRemote: true,
			wantHealthErrPrefix: `oidc: authenticator for issuer "https://this-will-not-work.notatld" is not healthy: Get "https://this-will-not-work.notatld/.well-known/openid-configuration": dial tcp: lookup this-will-not-work.notatld`,
		},
		{
			name: "accounts.google.com issuer",
			options: Options{
				JWTAuthenticator: apiserver.JWTAuthenticator{
					Issuer: apiserver.Issuer{
						URL:       "https://accounts.google.com",
						Audiences: []string{"my-client"},
					},
					ClaimMappings: apiserver.ClaimMappings{
						Username: apiserver.PrefixedClaimOrExpression{
							Claim:  "email",
							Prefix: pointer.String(""),
						},
					},
				},
				now: func() time.Time { return now },
			},
			claims: fmt.Sprintf(`{
				"iss": "accounts.google.com",
				"email": "thomas.jefferson@gmail.com",
				"aud": "my-client",
				"exp": %d
			}`, valid.Unix()),
			signingKey: loadRSAPrivKey(t, "testdata/rsa_1.pem", jose.RS256),
			pubKeys: []*jose.JSONWebKey{
				loadRSAKey(t, "testdata/rsa_1.pem", jose.RS256),
			},
			want: &user.DefaultInfo{
				Name: "thomas.jefferson@gmail.com",
			},
		},
		{
			name: "good token with bad client id",
			options: Options{
				JWTAuthenticator: apiserver.JWTAuthenticator{
					Issuer: apiserver.Issuer{
						URL:       "https://auth.example.com",
						Audiences: []string{"my-client"},
					},
					ClaimMappings: apiserver.ClaimMappings{
						Username: apiserver.PrefixedClaimOrExpression{
							Claim:  "username",
							Prefix: pointer.String("prefix:"),
						},
					},
				},
				now: func() time.Time { return now },
			},
			signingKey: loadRSAPrivKey(t, "testdata/rsa_1.pem", jose.RS256),
			pubKeys: []*jose.JSONWebKey{
				loadRSAKey(t, "testdata/rsa_1.pem", jose.RS256),
			},
			claims: fmt.Sprintf(`{
				"iss": "https://auth.example.com",
				"aud": "my-wrong-client",
				"username": "jane",
				"exp": %d
			}`, valid.Unix()),
			wantErr: `oidc: verify token: oidc: expected audience "my-client" got ["my-wrong-client"]`,
		},
		{
			name: "user validation rule fails for user.username",
			options: Options{
				JWTAuthenticator: apiserver.JWTAuthenticator{
					Issuer: apiserver.Issuer{
						URL:       "https://auth.example.com",
						Audiences: []string{"my-client"},
					},
					ClaimMappings: apiserver.ClaimMappings{
						Username: apiserver.PrefixedClaimOrExpression{
							Claim:  "username",
							Prefix: pointer.String("system:"),
						},
					},
					UserValidationRules: []apiserver.UserValidationRule{
						{
							Expression: "!user.username.startsWith('system:')",
							Message:    "username cannot used reserved system: prefix",
						},
					},
				},
				now: func() time.Time { return now },
			},
			signingKey: loadRSAPrivKey(t, "testdata/rsa_1.pem", jose.RS256),
			pubKeys: []*jose.JSONWebKey{
				loadRSAKey(t, "testdata/rsa_1.pem", jose.RS256),
			},
			claims: fmt.Sprintf(`{
				"iss": "https://auth.example.com",
				"aud": "my-client",
				"username": "jane",
				"exp": %d
			}`, valid.Unix()),
			wantErr: `oidc: error evaluating user info validation rule: validation expression '!user.username.startsWith('system:')' failed: username cannot used reserved system: prefix`,
		},
		{
			name: "user validation rule fails for user.groups",
			options: Options{
				JWTAuthenticator: apiserver.JWTAuthenticator{
					Issuer: apiserver.Issuer{
						URL:       "https://auth.example.com",
						Audiences: []string{"my-client"},
					},
					ClaimMappings: apiserver.ClaimMappings{
						Username: apiserver.PrefixedClaimOrExpression{
							Expression: "claims.username",
						},
						Groups: apiserver.PrefixedClaimOrExpression{
							Claim:  "groups",
							Prefix: pointer.String("system:"),
						},
					},
					UserValidationRules: []apiserver.UserValidationRule{
						{
							Expression: "user.groups.all(group, !group.startsWith('system:'))",
							Message:    "groups cannot used reserved system: prefix",
						},
					},
				},
				now: func() time.Time { return now },
			},
			signingKey: loadRSAPrivKey(t, "testdata/rsa_1.pem", jose.RS256),
			pubKeys: []*jose.JSONWebKey{
				loadRSAKey(t, "testdata/rsa_1.pem", jose.RS256),
			},
			claims: fmt.Sprintf(`{
				"iss": "https://auth.example.com",
				"aud": "my-client",
				"username": "jane",
				"exp": %d,
				"groups": ["team1", "team2"]
			}`, valid.Unix()),
			wantErr: `oidc: error evaluating user info validation rule: validation expression 'user.groups.all(group, !group.startsWith('system:'))' failed: groups cannot used reserved system: prefix`,
		},
		{
			name: "claim validation rule with expression fails",
			options: Options{
				JWTAuthenticator: apiserver.JWTAuthenticator{
					Issuer: apiserver.Issuer{
						URL:       "https://auth.example.com",
						Audiences: []string{"my-client"},
					},
					ClaimMappings: apiserver.ClaimMappings{
						Username: apiserver.PrefixedClaimOrExpression{
							Claim:  "username",
							Prefix: pointer.String(""),
						},
					},
					ClaimValidationRules: []apiserver.ClaimValidationRule{
						{
							Expression: `claims.hd == "example.com"`,
							Message:    "hd claim must be example.com",
						},
					},
				},
				now: func() time.Time { return now },
			},
			signingKey: loadRSAPrivKey(t, "testdata/rsa_1.pem", jose.RS256),
			pubKeys: []*jose.JSONWebKey{
				loadRSAKey(t, "testdata/rsa_1.pem", jose.RS256),
			},
			claims: fmt.Sprintf(`{
				"iss": "https://auth.example.com",
				"aud": "my-client",
				"username": "jane",
				"exp": %d
			}`, valid.Unix()),
			wantErr: `oidc: error evaluating claim validation expression: expression 'claims.hd == "example.com"' resulted in error: no such key: hd`,
		},
		{
			name: "claim validation rule with expression",
			options: Options{
				JWTAuthenticator: apiserver.JWTAuthenticator{
					Issuer: apiserver.Issuer{
						URL:       "https://auth.example.com",
						Audiences: []string{"my-client"},
					},
					ClaimMappings: apiserver.ClaimMappings{
						Username: apiserver.PrefixedClaimOrExpression{
							Claim:  "username",
							Prefix: pointer.String(""),
						},
					},
					ClaimValidationRules: []apiserver.ClaimValidationRule{
						{
							Expression: `claims.hd == "example.com"`,
							Message:    "hd claim must be example.com",
						},
					},
				},
				now: func() time.Time { return now },
			},
			signingKey: loadRSAPrivKey(t, "testdata/rsa_1.pem", jose.RS256),
			pubKeys: []*jose.JSONWebKey{
				loadRSAKey(t, "testdata/rsa_1.pem", jose.RS256),
			},
			claims: fmt.Sprintf(`{
				"iss": "https://auth.example.com",
				"aud": "my-client",
				"username": "jane",
				"exp": %d,
				"hd": "example.com"
			}`, valid.Unix()),
			want: &user.DefaultInfo{
				Name: "jane",
			},
		},
		{
			name: "claim validation rule with expression and nested claims",
			options: Options{
				JWTAuthenticator: apiserver.JWTAuthenticator{
					Issuer: apiserver.Issuer{
						URL:       "https://auth.example.com",
						Audiences: []string{"my-client"},
					},
					ClaimMappings: apiserver.ClaimMappings{
						Username: apiserver.PrefixedClaimOrExpression{
							Claim:  "username",
							Prefix: pointer.String(""),
						},
					},
					ClaimValidationRules: []apiserver.ClaimValidationRule{
						{
							Expression: `claims.foo.bar == "baz"`,
							Message:    "foo.bar claim must be baz",
						},
					},
				},
				now: func() time.Time { return now },
			},
			signingKey: loadRSAPrivKey(t, "testdata/rsa_1.pem", jose.RS256),
			pubKeys: []*jose.JSONWebKey{
				loadRSAKey(t, "testdata/rsa_1.pem", jose.RS256),
			},
			claims: fmt.Sprintf(`{
				"iss": "https://auth.example.com",
				"aud": "my-client",
				"username": "jane",
				"exp": %d,
				"hd": "example.com",
				"foo": {
					"bar": "baz"
				}
			}`, valid.Unix()),
			want: &user.DefaultInfo{
				Name: "jane",
			},
		},
		{
			name: "claim validation rule with mix of expression and claim",
			options: Options{
				JWTAuthenticator: apiserver.JWTAuthenticator{
					Issuer: apiserver.Issuer{
						URL:       "https://auth.example.com",
						Audiences: []string{"my-client"},
					},
					ClaimMappings: apiserver.ClaimMappings{
						Username: apiserver.PrefixedClaimOrExpression{
							Claim:  "username",
							Prefix: pointer.String(""),
						},
					},
					ClaimValidationRules: []apiserver.ClaimValidationRule{
						{
							Expression: `claims.foo.bar == "baz"`,
							Message:    "foo.bar claim must be baz",
						},
						{
							Claim:         "hd",
							RequiredValue: "example.com",
						},
					},
				},
				now: func() time.Time { return now },
			},
			signingKey: loadRSAPrivKey(t, "testdata/rsa_1.pem", jose.RS256),
			pubKeys: []*jose.JSONWebKey{
				loadRSAKey(t, "testdata/rsa_1.pem", jose.RS256),
			},
			claims: fmt.Sprintf(`{
				"iss": "https://auth.example.com",
				"aud": "my-client",
				"username": "jane",
				"exp": %d,
				"hd": "example.com",
				"foo": {
					"bar": "baz"
				}
			}`, valid.Unix()),
			want: &user.DefaultInfo{
				Name: "jane",
			},
		},
		{
			name: "username claim mapping with expression",
			options: Options{
				JWTAuthenticator: apiserver.JWTAuthenticator{
					Issuer: apiserver.Issuer{
						URL:       "https://auth.example.com",
						Audiences: []string{"my-client"},
					},
					ClaimMappings: apiserver.ClaimMappings{
						Username: apiserver.PrefixedClaimOrExpression{
							Expression: "claims.username",
						},
					},
				},
				now: func() time.Time { return now },
			},
			signingKey: loadRSAPrivKey(t, "testdata/rsa_1.pem", jose.RS256),
			pubKeys: []*jose.JSONWebKey{
				loadRSAKey(t, "testdata/rsa_1.pem", jose.RS256),
			},
			claims: fmt.Sprintf(`{
				"iss": "https://auth.example.com",
				"aud": "my-client",
				"username": "jane",
				"exp": %d
			}`, valid.Unix()),
			want: &user.DefaultInfo{
				Name: "jane",
			},
		},
		{
			name: "username claim mapping with expression and nested claim",
			options: Options{
				JWTAuthenticator: apiserver.JWTAuthenticator{
					Issuer: apiserver.Issuer{
						URL:       "https://auth.example.com",
						Audiences: []string{"my-client"},
					},
					ClaimMappings: apiserver.ClaimMappings{
						Username: apiserver.PrefixedClaimOrExpression{
							Expression: "claims.foo.username",
						},
					},
				},
				now: func() time.Time { return now },
			},
			signingKey: loadRSAPrivKey(t, "testdata/rsa_1.pem", jose.RS256),
			pubKeys: []*jose.JSONWebKey{
				loadRSAKey(t, "testdata/rsa_1.pem", jose.RS256),
			},
			claims: fmt.Sprintf(`{
				"iss": "https://auth.example.com",
				"aud": "my-client",
				"username": "jane",
				"exp": %d,
				"foo": {
					"username": "jane"
				}
			}`, valid.Unix()),
			want: &user.DefaultInfo{
				Name: "jane",
			},
		},
		{
			name: "groups claim mapping with expression",
			options: Options{
				JWTAuthenticator: apiserver.JWTAuthenticator{
					Issuer: apiserver.Issuer{
						URL:       "https://auth.example.com",
						Audiences: []string{"my-client"},
					},
					ClaimMappings: apiserver.ClaimMappings{
						Username: apiserver.PrefixedClaimOrExpression{
							Expression: "claims.username",
						},
						Groups: apiserver.PrefixedClaimOrExpression{
							Expression: "claims.groups",
						},
					},
				},
				now: func() time.Time { return now },
			},
			signingKey: loadRSAPrivKey(t, "testdata/rsa_1.pem", jose.RS256),
			pubKeys: []*jose.JSONWebKey{
				loadRSAKey(t, "testdata/rsa_1.pem", jose.RS256),
			},
			claims: fmt.Sprintf(`{
				"iss": "https://auth.example.com",
				"aud": "my-client",
				"username": "jane",
				"groups": ["team1", "team2"],
				"exp": %d
			}`, valid.Unix()),
			want: &user.DefaultInfo{
				Name:   "jane",
				Groups: []string{"team1", "team2"},
			},
		},
		{
			name: "groups claim with expression",
			options: Options{
				JWTAuthenticator: apiserver.JWTAuthenticator{
					Issuer: apiserver.Issuer{
						URL:       "https://auth.example.com",
						Audiences: []string{"my-client"},
					},
					ClaimMappings: apiserver.ClaimMappings{
						Username: apiserver.PrefixedClaimOrExpression{
							Claim:  "username",
							Prefix: pointer.String("oidc:"),
						},
						Groups: apiserver.PrefixedClaimOrExpression{
							Expression: `(claims.roles.split(",") + claims.other_roles.split(",")).map(role, "groups:" + role)`,
						},
					},
				},
				now: func() time.Time { return now },
			},
			signingKey: loadRSAPrivKey(t, "testdata/rsa_1.pem", jose.RS256),
			pubKeys: []*jose.JSONWebKey{
				loadRSAKey(t, "testdata/rsa_1.pem", jose.RS256),
			},
			claims: fmt.Sprintf(`{
				"iss": "https://auth.example.com",
				"aud": "my-client",
				"username": "jane",
				"roles": "foo,bar",
				"other_roles": "baz,qux",
				"exp": %d
			}`, valid.Unix()),
			want: &user.DefaultInfo{
				Name:   "oidc:jane",
				Groups: []string{"groups:foo", "groups:bar", "groups:baz", "groups:qux"},
			},
		},
		{
			name: "uid claim mapping with expression",
			options: Options{
				JWTAuthenticator: apiserver.JWTAuthenticator{
					Issuer: apiserver.Issuer{
						URL:       "https://auth.example.com",
						Audiences: []string{"my-client"},
					},
					ClaimMappings: apiserver.ClaimMappings{
						Username: apiserver.PrefixedClaimOrExpression{
							Expression: "claims.username",
						},
						Groups: apiserver.PrefixedClaimOrExpression{
							Expression: "claims.groups",
						},
						UID: apiserver.ClaimOrExpression{
							Expression: "claims.uid",
						},
					},
				},
				now: func() time.Time { return now },
			},
			signingKey: loadRSAPrivKey(t, "testdata/rsa_1.pem", jose.RS256),
			pubKeys: []*jose.JSONWebKey{
				loadRSAKey(t, "testdata/rsa_1.pem", jose.RS256),
			},
			claims: fmt.Sprintf(`{
				"iss": "https://auth.example.com",
				"aud": "my-client",
				"username": "jane",
				"groups": ["team1", "team2"],
				"exp": %d,
				"uid": "1234"
			}`, valid.Unix()),
			want: &user.DefaultInfo{
				Name:   "jane",
				Groups: []string{"team1", "team2"},
				UID:    "1234",
			},
		},
		{
			name: "uid claim mapping with claim",
			options: Options{
				JWTAuthenticator: apiserver.JWTAuthenticator{
					Issuer: apiserver.Issuer{
						URL:       "https://auth.example.com",
						Audiences: []string{"my-client"},
					},
					ClaimMappings: apiserver.ClaimMappings{
						Username: apiserver.PrefixedClaimOrExpression{
							Expression: "claims.username",
						},
						Groups: apiserver.PrefixedClaimOrExpression{
							Expression: "claims.groups",
						},
						UID: apiserver.ClaimOrExpression{
							Claim: "uid",
						},
					},
				},
				now: func() time.Time { return now },
			},
			signingKey: loadRSAPrivKey(t, "testdata/rsa_1.pem", jose.RS256),
			pubKeys: []*jose.JSONWebKey{
				loadRSAKey(t, "testdata/rsa_1.pem", jose.RS256),
			},
			claims: fmt.Sprintf(`{
				"iss": "https://auth.example.com",
				"aud": "my-client",
				"username": "jane",
				"groups": ["team1", "team2"],
				"exp": %d,
				"uid": "1234"
			}`, valid.Unix()),
			want: &user.DefaultInfo{
				Name:   "jane",
				Groups: []string{"team1", "team2"},
				UID:    "1234",
			},
		},
		{
			name: "extra claim mapping with expression",
			options: Options{
				JWTAuthenticator: apiserver.JWTAuthenticator{
					Issuer: apiserver.Issuer{
						URL:       "https://auth.example.com",
						Audiences: []string{"my-client"},
					},
					ClaimMappings: apiserver.ClaimMappings{
						Username: apiserver.PrefixedClaimOrExpression{
							Expression: "claims.username",
						},
						Groups: apiserver.PrefixedClaimOrExpression{
							Expression: "claims.groups",
						},
						UID: apiserver.ClaimOrExpression{
							Expression: "claims.uid",
						},
						Extra: []apiserver.ExtraMapping{
							{
								Key:             "example.org/foo",
								ValueExpression: "claims.foo",
							},
							{
								Key:             "example.org/bar",
								ValueExpression: "claims.bar",
							},
						},
					},
				},
				now: func() time.Time { return now },
			},
			signingKey: loadRSAPrivKey(t, "testdata/rsa_1.pem", jose.RS256),
			pubKeys: []*jose.JSONWebKey{
				loadRSAKey(t, "testdata/rsa_1.pem", jose.RS256),
			},
			claims: fmt.Sprintf(`{
				"iss": "https://auth.example.com",
				"aud": "my-client",
				"username": "jane",
				"groups": ["team1", "team2"],
				"exp": %d,
				"uid": "1234",
				"foo": "bar",
				"bar": [
					"baz",
					"qux"
				]
			}`, valid.Unix()),
			want: &user.DefaultInfo{
				Name:   "jane",
				Groups: []string{"team1", "team2"},
				UID:    "1234",
				Extra: map[string][]string{
					"example.org/foo": {"bar"},
					"example.org/bar": {"baz", "qux"},
				},
			},
		},
		{
			name: "extra claim mapping, value derived from claim value",
			options: Options{
				JWTAuthenticator: apiserver.JWTAuthenticator{
					Issuer: apiserver.Issuer{
						URL:       "https://auth.example.com",
						Audiences: []string{"my-client"},
					},
					ClaimMappings: apiserver.ClaimMappings{
						Username: apiserver.PrefixedClaimOrExpression{
							Expression: "claims.username",
						},
						Extra: []apiserver.ExtraMapping{
							{
								Key:             "example.org/admin",
								ValueExpression: `(has(claims.is_admin) && claims.is_admin) ? "true":""`,
							},
							{
								Key:             "example.org/admin_1",
								ValueExpression: `claims.?is_admin.orValue(false) == true ? "true":""`,
							},
							{
								Key:             "example.org/non_existent",
								ValueExpression: `claims.?non_existent.orValue("default") == "default" ? "true":""`,
							},
						},
					},
				},
				now: func() time.Time { return now },
			},
			signingKey: loadRSAPrivKey(t, "testdata/rsa_1.pem", jose.RS256),
			pubKeys: []*jose.JSONWebKey{
				loadRSAKey(t, "testdata/rsa_1.pem", jose.RS256),
			},
			claims: fmt.Sprintf(`{
				"iss": "https://auth.example.com",
				"aud": "my-client",
				"username": "jane",
				"exp": %d,
				"is_admin": true
			}`, valid.Unix()),
			want: &user.DefaultInfo{
				Name: "jane",
				Extra: map[string][]string{
					"example.org/admin":        {"true"},
					"example.org/admin_1":      {"true"},
					"example.org/non_existent": {"true"},
				},
			},
		},
		{
			name: "hardcoded extra claim mapping",
			options: Options{
				JWTAuthenticator: apiserver.JWTAuthenticator{
					Issuer: apiserver.Issuer{
						URL:       "https://auth.example.com",
						Audiences: []string{"my-client"},
					},
					ClaimMappings: apiserver.ClaimMappings{
						Username: apiserver.PrefixedClaimOrExpression{
							Expression: "claims.username",
						},
						Extra: []apiserver.ExtraMapping{
							{
								Key:             "example.org/admin",
								ValueExpression: `"true"`,
							},
						},
					},
				},
				now: func() time.Time { return now },
			},
			signingKey: loadRSAPrivKey(t, "testdata/rsa_1.pem", jose.RS256),
			pubKeys: []*jose.JSONWebKey{
				loadRSAKey(t, "testdata/rsa_1.pem", jose.RS256),
			},
			claims: fmt.Sprintf(`{
				"iss": "https://auth.example.com",
				"aud": "my-client",
				"username": "jane",
				"exp": %d,
				"is_admin": true
			}`, valid.Unix()),
			want: &user.DefaultInfo{
				Name: "jane",
				Extra: map[string][]string{
					"example.org/admin": {"true"},
				},
			},
		},
		{
			name: "extra claim mapping, multiple expressions for same key",
			options: Options{
				JWTAuthenticator: apiserver.JWTAuthenticator{
					Issuer: apiserver.Issuer{
						URL:       "https://auth.example.com",
						Audiences: []string{"my-client"},
					},
					ClaimMappings: apiserver.ClaimMappings{
						Username: apiserver.PrefixedClaimOrExpression{
							Expression: "claims.username",
						},
						Groups: apiserver.PrefixedClaimOrExpression{
							Expression: "claims.groups",
						},
						UID: apiserver.ClaimOrExpression{
							Expression: "claims.uid",
						},
						Extra: []apiserver.ExtraMapping{
							{
								Key:             "example.org/foo",
								ValueExpression: "claims.foo",
							},
							{
								Key:             "example.org/bar",
								ValueExpression: "claims.bar",
							},
							{
								Key:             "example.org/foo",
								ValueExpression: "claims.bar",
							},
						},
					},
				},
				now: func() time.Time { return now },
			},
			signingKey: loadRSAPrivKey(t, "testdata/rsa_1.pem", jose.RS256),
			pubKeys: []*jose.JSONWebKey{
				loadRSAKey(t, "testdata/rsa_1.pem", jose.RS256),
			},
			claims: fmt.Sprintf(`{
				"iss": "https://auth.example.com",
				"aud": "my-client",
				"username": "jane",
				"groups": ["team1", "team2"],
				"exp": %d,
				"uid": "1234",
				"foo": "bar",
				"bar": [
					"baz",
					"qux"
				]
			}`, valid.Unix()),
			wantInitErr: `claimMappings.extra[2].key: Duplicate value: "example.org/foo"`,
		},
		{
			name: "disallowed issuer via configured value",
			options: Options{
				JWTAuthenticator: apiserver.JWTAuthenticator{
					Issuer: apiserver.Issuer{
						URL:       "https://auth.example.com",
						Audiences: []string{"my-client"},
					},
					ClaimMappings: apiserver.ClaimMappings{
						Username: apiserver.PrefixedClaimOrExpression{
							Expression: "claims.username",
						},
						Groups: apiserver.PrefixedClaimOrExpression{
							Expression: "claims.groups",
						},
						UID: apiserver.ClaimOrExpression{
							Expression: "claims.uid",
						},
						Extra: []apiserver.ExtraMapping{
							{
								Key:             "example.org/foo",
								ValueExpression: "claims.foo",
							},
							{
								Key:             "example.org/bar",
								ValueExpression: "claims.bar",
							},
						},
					},
				},
				DisallowedIssuers: []string{"https://auth.example.com"},
				now:               func() time.Time { return now },
			},
			signingKey: loadRSAPrivKey(t, "testdata/rsa_1.pem", jose.RS256),
			pubKeys: []*jose.JSONWebKey{
				loadRSAKey(t, "testdata/rsa_1.pem", jose.RS256),
			},
			claims: fmt.Sprintf(`{
				"iss": "https://auth.example.com",
				"aud": "my-client",
				"username": "jane",
				"groups": ["team1", "team2"],
				"exp": %d,
				"uid": "1234",
				"foo": "bar",
				"bar": [
					"baz",
					"qux"
				]
			}`, valid.Unix()),
			wantInitErr: `issuer.url: Invalid value: "https://auth.example.com": URL must not overlap with disallowed issuers: [https://auth.example.com]`,
		},
		{
			name: "extra claim mapping, empty string value for key",
			options: Options{
				JWTAuthenticator: apiserver.JWTAuthenticator{
					Issuer: apiserver.Issuer{
						URL:       "https://auth.example.com",
						Audiences: []string{"my-client"},
					},
					ClaimMappings: apiserver.ClaimMappings{
						Username: apiserver.PrefixedClaimOrExpression{
							Expression: "claims.username",
						},
						Groups: apiserver.PrefixedClaimOrExpression{
							Expression: "claims.groups",
						},
						UID: apiserver.ClaimOrExpression{
							Expression: "claims.uid",
						},
						Extra: []apiserver.ExtraMapping{
							{
								Key:             "example.org/foo",
								ValueExpression: "claims.foo",
							},
							{
								Key:             "example.org/bar",
								ValueExpression: "claims.bar",
							},
						},
					},
				},
				now: func() time.Time { return now },
			},
			signingKey: loadRSAPrivKey(t, "testdata/rsa_1.pem", jose.RS256),
			pubKeys: []*jose.JSONWebKey{
				loadRSAKey(t, "testdata/rsa_1.pem", jose.RS256),
			},
			claims: fmt.Sprintf(`{
				"iss": "https://auth.example.com",
				"aud": "my-client",
				"username": "jane",
				"groups": ["team1", "team2"],
				"exp": %d,
				"uid": "1234",
				"foo": "",
				"bar": [
					"baz",
					"qux"
				]
			}`, valid.Unix()),
			want: &user.DefaultInfo{
				Name:   "jane",
				Groups: []string{"team1", "team2"},
				UID:    "1234",
				Extra: map[string][]string{
					"example.org/bar": {"baz", "qux"},
				},
			},
		},
		{
			name: "extra claim mapping with user validation rule succeeds",
			options: Options{
				JWTAuthenticator: apiserver.JWTAuthenticator{
					Issuer: apiserver.Issuer{
						URL:       "https://auth.example.com",
						Audiences: []string{"my-client"},
					},
					ClaimMappings: apiserver.ClaimMappings{
						Username: apiserver.PrefixedClaimOrExpression{
							Expression: "claims.username",
						},
						Groups: apiserver.PrefixedClaimOrExpression{
							Expression: "claims.groups",
						},
						UID: apiserver.ClaimOrExpression{
							Expression: "claims.uid",
						},
						Extra: []apiserver.ExtraMapping{
							{
								Key:             "example.org/foo",
								ValueExpression: "'bar'",
							},
							{
								Key:             "example.org/baz",
								ValueExpression: "claims.baz",
							},
						},
					},
					UserValidationRules: []apiserver.UserValidationRule{
						{
							Expression: "'bar' in user.extra['example.org/foo'] && 'qux' in user.extra['example.org/baz']",
							Message:    "example.org/foo must be bar and example.org/baz must be qux",
						},
					},
				},
				now: func() time.Time { return now },
			},
			signingKey: loadRSAPrivKey(t, "testdata/rsa_1.pem", jose.RS256),
			pubKeys: []*jose.JSONWebKey{
				loadRSAKey(t, "testdata/rsa_1.pem", jose.RS256),
			},
			claims: fmt.Sprintf(`{
				"iss": "https://auth.example.com",
				"aud": "my-client",
				"username": "jane",
				"groups": ["team1", "team2"],
				"exp": %d,
				"uid": "1234",
				"baz": "qux"
			}`, valid.Unix()),
			want: &user.DefaultInfo{
				Name:   "jane",
				Groups: []string{"team1", "team2"},
				UID:    "1234",
				Extra: map[string][]string{
					"example.org/foo": {"bar"},
					"example.org/baz": {"qux"},
				},
			},
		},
		{
			name: "groups expression returns null",
			options: Options{
				JWTAuthenticator: apiserver.JWTAuthenticator{
					Issuer: apiserver.Issuer{
						URL:       "https://auth.example.com",
						Audiences: []string{"my-client"},
					},
					ClaimMappings: apiserver.ClaimMappings{
						Username: apiserver.PrefixedClaimOrExpression{
							Expression: "claims.username",
						},
						Groups: apiserver.PrefixedClaimOrExpression{
							Expression: "claims.groups",
						},
					},
				},
				now: func() time.Time { return now },
			},
			signingKey: loadRSAPrivKey(t, "testdata/rsa_1.pem", jose.RS256),
			pubKeys: []*jose.JSONWebKey{
				loadRSAKey(t, "testdata/rsa_1.pem", jose.RS256),
			},
			claims: fmt.Sprintf(`{
				"iss": "https://auth.example.com",
				"aud": "my-client",
				"username": "jane",
				"groups": null,
				"exp": %d,
				"uid": "1234",
				"baz": "qux"
			}`, valid.Unix()),
			want: &user.DefaultInfo{
				Name: "jane",
			},
		},
		// test to ensure omitempty fields not included in user info
		// are set and accessible for CEL evaluation.
		{
			name: "test user validation rule doesn't fail when user info is empty except username",
			options: Options{
				JWTAuthenticator: apiserver.JWTAuthenticator{
					Issuer: apiserver.Issuer{
						URL:       "https://auth.example.com",
						Audiences: []string{"my-client"},
					},
					ClaimMappings: apiserver.ClaimMappings{
						Username: apiserver.PrefixedClaimOrExpression{
							Expression: "claims.username",
						},
						Groups: apiserver.PrefixedClaimOrExpression{
							Expression: "claims.groups",
						},
					},
					UserValidationRules: []apiserver.UserValidationRule{
						{
							Expression: `user.username == " "`,
							Message:    "username must be single space",
						},
						{
							Expression: `user.uid == ""`,
							Message:    "uid must be empty string",
						},
						{
							Expression: `!('bar' in user.groups)`,
							Message:    "groups must not contain bar",
						},
						{
							Expression: `!('bar' in user.extra)`,
							Message:    "extra must not contain bar",
						},
					},
				},
				now: func() time.Time { return now },
			},
			signingKey: loadRSAPrivKey(t, "testdata/rsa_1.pem", jose.RS256),
			pubKeys: []*jose.JSONWebKey{
				loadRSAKey(t, "testdata/rsa_1.pem", jose.RS256),
			},
			claims: fmt.Sprintf(`{
				"iss": "https://auth.example.com",
				"aud": "my-client",
				"username": " ",
				"groups": null,
				"exp": %d,
				"baz": "qux"
			}`, valid.Unix()),
			want: &user.DefaultInfo{Name: " "},
		},
		{
			name: "empty username is allowed via claim",
			options: Options{
				JWTAuthenticator: apiserver.JWTAuthenticator{
					Issuer: apiserver.Issuer{
						URL:       "https://auth.example.com",
						Audiences: []string{"my-client"},
					},
					ClaimMappings: apiserver.ClaimMappings{
						Username: apiserver.PrefixedClaimOrExpression{
							Claim:  "username",
							Prefix: pointer.String(""),
						},
						Groups: apiserver.PrefixedClaimOrExpression{
							Expression: "claims.groups",
						},
					},
					UserValidationRules: []apiserver.UserValidationRule{
						{
							Expression: `user.username == ""`,
							Message:    "username must be empty string",
						},
						{
							Expression: `user.uid == ""`,
							Message:    "uid must be empty string",
						},
						{
							Expression: `!('bar' in user.groups)`,
							Message:    "groups must not contain bar",
						},
						{
							Expression: `!('bar' in user.extra)`,
							Message:    "extra must not contain bar",
						},
					},
				},
				now: func() time.Time { return now },
			},
			signingKey: loadRSAPrivKey(t, "testdata/rsa_1.pem", jose.RS256),
			pubKeys: []*jose.JSONWebKey{
				loadRSAKey(t, "testdata/rsa_1.pem", jose.RS256),
			},
			claims: fmt.Sprintf(`{
				"iss": "https://auth.example.com",
				"aud": "my-client",
				"username": "",
				"groups": null,
				"exp": %d,
				"baz": "qux"
			}`, valid.Unix()),
			want: &user.DefaultInfo{},
		},
		// test to assert the minimum valid jwt payload
		// the required claims are iss, aud, exp and <claimMappings.Username> (in this case user).
		{
			name: "minimum valid jwt payload",
			options: Options{
				JWTAuthenticator: apiserver.JWTAuthenticator{
					Issuer: apiserver.Issuer{
						URL:       "https://auth.example.com",
						Audiences: []string{"my-client"},
					},
					ClaimMappings: apiserver.ClaimMappings{
						Username: apiserver.PrefixedClaimOrExpression{
							Expression: "claims.user",
						},
					},
				},
				now: func() time.Time { return now },
			},
			signingKey: loadRSAPrivKey(t, "testdata/rsa_1.pem", jose.RS256),
			pubKeys: []*jose.JSONWebKey{
				loadRSAKey(t, "testdata/rsa_1.pem", jose.RS256),
			},
			claims: fmt.Sprintf(`{
				"iss": "https://auth.example.com",
				"aud": "my-client",
				"user": "jane",
				"exp": %d
			}`, valid.Unix()),
			want: &user.DefaultInfo{
				Name: "jane",
			},
		},
		{
			name: "discovery-url",
			options: Options{
				JWTAuthenticator: apiserver.JWTAuthenticator{
					Issuer: apiserver.Issuer{
						URL:          "https://auth.example.com",
						DiscoveryURL: "{{.URL}}/.well-known/openid-configuration",
						Audiences:    []string{"my-client"},
					},
					ClaimMappings: apiserver.ClaimMappings{
						Username: apiserver.PrefixedClaimOrExpression{
							Claim:  "username",
							Prefix: pointer.String(""),
						},
					},
				},
				now: func() time.Time { return now },
			},
			signingKey: loadRSAPrivKey(t, "testdata/rsa_1.pem", jose.RS256),
			pubKeys: []*jose.JSONWebKey{
				loadRSAKey(t, "testdata/rsa_1.pem", jose.RS256),
			},
			claims: fmt.Sprintf(`{
				"iss": "https://auth.example.com",
				"aud": "my-client",
				"username": "jane",
				"exp": %d
			}`, valid.Unix()),
			openIDConfig: `{
					"issuer": "https://auth.example.com",
					"jwks_uri": "{{.URL}}/.testing/keys"
			}`,
			fetchKeysFromRemote: true,
			want: &user.DefaultInfo{
				Name: "jane",
			},
		},
		{
			name: "discovery url, issuer has a path",
			options: Options{
				JWTAuthenticator: apiserver.JWTAuthenticator{
					Issuer: apiserver.Issuer{
						URL:          "https://auth.example.com/a/b/foo",
						DiscoveryURL: "{{.URL}}/.well-known/openid-configuration",
						Audiences:    []string{"my-client"},
					},
					ClaimMappings: apiserver.ClaimMappings{
						Username: apiserver.PrefixedClaimOrExpression{
							Claim:  "username",
							Prefix: pointer.String(""),
						},
					},
				},
				now: func() time.Time { return now },
			},
			signingKey: loadRSAPrivKey(t, "testdata/rsa_1.pem", jose.RS256),
			pubKeys: []*jose.JSONWebKey{
				loadRSAKey(t, "testdata/rsa_1.pem", jose.RS256),
			},
			claims: fmt.Sprintf(`{
				"iss": "https://auth.example.com/a/b/foo",
				"aud": "my-client",
				"username": "jane",
				"exp": %d
			}`, valid.Unix()),
			openIDConfig: `{
					"issuer": "https://auth.example.com/a/b/foo",
					"jwks_uri": "{{.URL}}/.testing/keys"
			}`,
			fetchKeysFromRemote: true,
			want: &user.DefaultInfo{
				Name: "jane",
			},
		},
		{
			name: "discovery url has a path, issuer url has no path",
			options: Options{
				JWTAuthenticator: apiserver.JWTAuthenticator{
					Issuer: apiserver.Issuer{
						URL:          "https://auth.example.com",
						DiscoveryURL: "{{.URL}}/c/d/bar/.well-known/openid-configuration",
						Audiences:    []string{"my-client"},
					},
					ClaimMappings: apiserver.ClaimMappings{
						Username: apiserver.PrefixedClaimOrExpression{
							Claim:  "username",
							Prefix: pointer.String(""),
						},
					},
				},
				now: func() time.Time { return now },
			},
			signingKey: loadRSAPrivKey(t, "testdata/rsa_1.pem", jose.RS256),
			pubKeys: []*jose.JSONWebKey{
				loadRSAKey(t, "testdata/rsa_1.pem", jose.RS256),
			},
			claims: fmt.Sprintf(`{
				"iss": "https://auth.example.com",
				"aud": "my-client",
				"username": "jane",
				"exp": %d
			}`, valid.Unix()),
			openIDConfig: `{
					"issuer": "https://auth.example.com",
					"jwks_uri": "{{.URL}}/.testing/keys"
			}`,
			fetchKeysFromRemote: true,
			want: &user.DefaultInfo{
				Name: "jane",
			},
		},
		{
			name: "discovery url and issuer url have paths",
			options: Options{
				JWTAuthenticator: apiserver.JWTAuthenticator{
					Issuer: apiserver.Issuer{
						URL:          "https://auth.example.com/a/b/foo",
						DiscoveryURL: "{{.URL}}/c/d/bar/.well-known/openid-configuration",
						Audiences:    []string{"my-client"},
					},
					ClaimMappings: apiserver.ClaimMappings{
						Username: apiserver.PrefixedClaimOrExpression{
							Claim:  "username",
							Prefix: pointer.String(""),
						},
					},
				},
				now: func() time.Time { return now },
			},
			signingKey: loadRSAPrivKey(t, "testdata/rsa_1.pem", jose.RS256),
			pubKeys: []*jose.JSONWebKey{
				loadRSAKey(t, "testdata/rsa_1.pem", jose.RS256),
			},
			claims: fmt.Sprintf(`{
				"iss": "https://auth.example.com/a/b/foo",
				"aud": "my-client",
				"username": "jane",
				"exp": %d
			}`, valid.Unix()),
			openIDConfig: `{
					"issuer": "https://auth.example.com/a/b/foo",
					"jwks_uri": "{{.URL}}/.testing/keys"
			}`,
			fetchKeysFromRemote: true,
			want: &user.DefaultInfo{
				Name: "jane",
			},
		},
		{
			name: "discovery url and issuer url have paths, issuer url has trailing slash",
			options: Options{
				JWTAuthenticator: apiserver.JWTAuthenticator{
					Issuer: apiserver.Issuer{
						URL:          "https://auth.example.com/a/b/foo/",
						DiscoveryURL: "{{.URL}}/c/d/bar/.well-known/openid-configuration",
						Audiences:    []string{"my-client"},
					},
					ClaimMappings: apiserver.ClaimMappings{
						Username: apiserver.PrefixedClaimOrExpression{
							Claim:  "username",
							Prefix: pointer.String(""),
						},
					},
				},
				now: func() time.Time { return now },
			},
			signingKey: loadRSAPrivKey(t, "testdata/rsa_1.pem", jose.RS256),
			pubKeys: []*jose.JSONWebKey{
				loadRSAKey(t, "testdata/rsa_1.pem", jose.RS256),
			},
			claims: fmt.Sprintf(`{
				"iss": "https://auth.example.com/a/b/foo/",
				"aud": "my-client",
				"username": "jane",
				"exp": %d
			}`, valid.Unix()),
			openIDConfig: `{
					"issuer": "https://auth.example.com/a/b/foo/",
					"jwks_uri": "{{.URL}}/.testing/keys"
			}`,
			fetchKeysFromRemote: true,
			want: &user.DefaultInfo{
				Name: "jane",
			},
		},
		{
			name: "credential id set in extra even when no extra claim mappings are defined",
			options: Options{
				JWTAuthenticator: apiserver.JWTAuthenticator{
					Issuer: apiserver.Issuer{
						URL:       "https://auth.example.com",
						Audiences: []string{"my-client"},
					},
					ClaimMappings: apiserver.ClaimMappings{
						Username: apiserver.PrefixedClaimOrExpression{
							Expression: "claims.username",
						},
					},
				},
				now: func() time.Time { return now },
			},
			signingKey: loadRSAPrivKey(t, "testdata/rsa_1.pem", jose.RS256),
			pubKeys: []*jose.JSONWebKey{
				loadRSAKey(t, "testdata/rsa_1.pem", jose.RS256),
			},
			claims: fmt.Sprintf(`{
				"iss": "https://auth.example.com",
				"aud": "my-client",
				"username": "jane",
				"exp": %d,
				"jti": "1234"
			}`, valid.Unix()),
			want: &user.DefaultInfo{
				Name: "jane",
				Extra: map[string][]string{
					user.CredentialIDKey: {"JTI=1234"},
				},
			},
		},
		{
			name: "credential id set in extra when extra claim mappings are defined",
			options: Options{
				JWTAuthenticator: apiserver.JWTAuthenticator{
					Issuer: apiserver.Issuer{
						URL:       "https://auth.example.com",
						Audiences: []string{"my-client"},
					},
					ClaimMappings: apiserver.ClaimMappings{
						Username: apiserver.PrefixedClaimOrExpression{
							Expression: "claims.username",
						},
						Extra: []apiserver.ExtraMapping{
							{
								Key:             "example.org/foo",
								ValueExpression: "claims.foo",
							},
							{
								Key:             "example.org/bar",
								ValueExpression: "claims.bar",
							},
						},
					},
				},
				now: func() time.Time { return now },
			},
			signingKey: loadRSAPrivKey(t, "testdata/rsa_1.pem", jose.RS256),
			pubKeys: []*jose.JSONWebKey{
				loadRSAKey(t, "testdata/rsa_1.pem", jose.RS256),
			},
			claims: fmt.Sprintf(`{
				"iss": "https://auth.example.com",
				"aud": "my-client",
				"username": "jane",
				"exp": %d,
				"jti": "1234",
				"foo": "bar",
				"bar": [
					"baz",
					"qux"
				]
			}`, valid.Unix()),
			want: &user.DefaultInfo{
				Name: "jane",
				Extra: map[string][]string{
					user.CredentialIDKey: {"JTI=1234"},
					"example.org/foo":    {"bar"},
					"example.org/bar":    {"baz", "qux"},
				},
			},
		},
		{
			name: "non-string jti claim does not set credential id in extra or error",
			options: Options{
				JWTAuthenticator: apiserver.JWTAuthenticator{
					Issuer: apiserver.Issuer{
						URL:       "https://auth.example.com",
						Audiences: []string{"my-client"},
					},
					ClaimMappings: apiserver.ClaimMappings{
						Username: apiserver.PrefixedClaimOrExpression{
							Expression: "claims.username",
						},
					},
				},
				now: func() time.Time { return now },
			},
			signingKey: loadRSAPrivKey(t, "testdata/rsa_1.pem", jose.RS256),
			pubKeys: []*jose.JSONWebKey{
				loadRSAKey(t, "testdata/rsa_1.pem", jose.RS256),
			},
			claims: fmt.Sprintf(`{
				"iss": "https://auth.example.com",
				"aud": "my-client",
				"username": "jane",
				"exp": %d,
				"jti": 1234
			}`, valid.Unix()),
			want: &user.DefaultInfo{
				Name: "jane",
			},
		},
		{
			name: "missing jti claim does not set credential id in extra or error",
			options: Options{
				JWTAuthenticator: apiserver.JWTAuthenticator{
					Issuer: apiserver.Issuer{
						URL:       "https://auth.example.com",
						Audiences: []string{"my-client"},
					},
					ClaimMappings: apiserver.ClaimMappings{
						Username: apiserver.PrefixedClaimOrExpression{
							Expression: "claims.username",
						},
					},
				},
				now: func() time.Time { return now },
			},
			signingKey: loadRSAPrivKey(t, "testdata/rsa_1.pem", jose.RS256),
			pubKeys: []*jose.JSONWebKey{
				loadRSAKey(t, "testdata/rsa_1.pem", jose.RS256),
			},
			claims: fmt.Sprintf(`{
				"iss": "https://auth.example.com",
				"aud": "my-client",
				"username": "jane",
				"exp": %d
			}`, valid.Unix()),
			want: &user.DefaultInfo{
				Name: "jane",
			},
		},
	}

	var successTestCount, failureTestCount int
	for _, test := range tests {
		t.Run(test.name, test.run)
		if test.wantSkip || len(test.wantInitErr) > 0 || len(test.wantHealthErrPrefix) > 0 {
			continue
		}
		// check metrics for success and failure
		if test.wantErr == "" {
			successTestCount++
			testutil.AssertHistogramTotalCount(t, "apiserver_authentication_jwt_authenticator_latency_seconds", map[string]string{"result": "success"}, successTestCount)
		} else {
			failureTestCount++
			testutil.AssertHistogramTotalCount(t, "apiserver_authentication_jwt_authenticator_latency_seconds", map[string]string{"result": "failure"}, failureTestCount)
		}
	}
}

func TestUnmarshalClaimError(t *testing.T) {
	// Ensure error strings returned by unmarshaling claims don't include the claim.
	const token = "96bb299a-02e9-11e8-8673-54ee7553240e" // Fake token for testing.
	payload := fmt.Sprintf(`{
		"token": "%s"
	}`, token)

	var c claims
	if err := json.Unmarshal([]byte(payload), &c); err != nil {
		t.Fatal(err)
	}
	var n int
	err := c.unmarshalClaim("token", &n)
	if err == nil {
		t.Fatal("expected error")
	}

	if strings.Contains(err.Error(), token) {
		t.Fatalf("unmarshal error included token")
	}
}

func TestUnmarshalClaim(t *testing.T) {
	tests := []struct {
		name    string
		claims  string
		do      func(claims) (interface{}, error)
		want    interface{}
		wantErr bool
	}{
		{
			name:   "string claim",
			claims: `{"aud":"foo"}`,
			do: func(c claims) (interface{}, error) {
				var s string
				err := c.unmarshalClaim("aud", &s)
				return s, err
			},
			want: "foo",
		},
		{
			name:   "mismatched types",
			claims: `{"aud":"foo"}`,
			do: func(c claims) (interface{}, error) {
				var n int
				err := c.unmarshalClaim("aud", &n)
				return n, err

			},
			wantErr: true,
		},
		{
			name:   "bool claim",
			claims: `{"email":"foo@coreos.com","email_verified":true}`,
			do: func(c claims) (interface{}, error) {
				var verified bool
				err := c.unmarshalClaim("email_verified", &verified)
				return verified, err
			},
			want: true,
		},
		{
			name:   "strings claim",
			claims: `{"groups":["a","b","c"]}`,
			do: func(c claims) (interface{}, error) {
				var groups []string
				err := c.unmarshalClaim("groups", &groups)
				return groups, err
			},
			want: []string{"a", "b", "c"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var c claims
			if err := json.Unmarshal([]byte(test.claims), &c); err != nil {
				t.Fatal(err)
			}

			got, err := test.do(c)
			if err != nil {
				if test.wantErr {
					return
				}
				t.Fatalf("unexpected error: %v", err)
			}
			if test.wantErr {
				t.Fatalf("expected error")
			}

			if !reflect.DeepEqual(got, test.want) {
				t.Errorf("wanted=%#v, got=%#v", test.want, got)
			}
		})
	}
}

func TestJWKSMetricsKeySetError(t *testing.T) {
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.StructuredAuthenticationConfiguration, true)

	synchronizeTokenIDVerifierForTest = true
	signingKey := loadRSAPrivKey(t, "testdata/rsa_1.pem", jose.RS256)
	pubKeys := []*jose.JSONWebKey{
		loadRSAKey(t, "testdata/rsa_1.pem", jose.RS256),
	}
	signer, err := jose.NewSigner(jose.SigningKey{
		Algorithm: jose.SignatureAlgorithm(signingKey.Algorithm),
		Key:       signingKey,
	}, nil)
	if err != nil {
		t.Fatalf("initialize signer: %v", err)
	}

	tests := []struct {
		name         string
		openIDConfig string
	}{
		{
			name: "invalid keyset",
			openIDConfig: `{
				"issuer": "{{.URL}}",
				"jwks_uri": "{{.URL}}/.testing/invalidkeys"
			}`,
		},
		{
			name: "bad status code",
			openIDConfig: `{
				"issuer": "{{.URL}}",
				"jwks_uri": "{{.URL}}/.testing/keys/badstatus"
			}`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ResetMetrics()

			ts := newClaimServer(t, toKeySet(pubKeys), signer, nil, &test.openIDConfig)
			defer ts.Close()

			v := struct {
				URL string
			}{
				URL: ts.URL,
			}
			test.openIDConfig = replace(test.openIDConfig, &v)

			opts := Options{
				JWTAuthenticator: apiserver.JWTAuthenticator{
					Issuer: apiserver.Issuer{
						URL:       ts.URL,
						Audiences: []string{"my-client"},
					},
					ClaimMappings: apiserver.ClaimMappings{
						Username: apiserver.PrefixedClaimOrExpression{
							Claim:  "username",
							Prefix: pointer.String(""),
						},
					},
				},
				now:         func() time.Time { return now },
				APIServerID: "testAPIServerID",
			}

			// Make the certificate of the helper server available to the authenticator
			caBundle := pem.EncodeToMemory(&pem.Block{
				Type:  "CERTIFICATE",
				Bytes: ts.Certificate().Raw,
			})
			caContent, err := dynamiccertificates.NewStaticCAContent("oidc-authenticator", caBundle)
			if err != nil {
				t.Fatalf("initialize ca: %v", err)
			}
			opts.CAContentProvider = caContent

			ctx := testContext(t)
			// Initialize the authenticator.
			a, err := New(ctx, opts)
			if err != nil {
				t.Fatalf("initialize authenticator: %v", err)
			}

			claims := fmt.Sprintf(`{
"iss": "%s",
"aud": "my-client",
"username": "jane",
"exp": %d
}`, ts.URL, valid.Unix())

			jws, err := signer.Sign([]byte(claims))
			if err != nil {
				t.Fatalf("sign claims: %v", err)
			}
			token, err := jws.CompactSerialize()
			if err != nil {
				t.Fatalf("serialize token: %v", err)
			}

			ia, ok := a.(*instrumentedAuthenticator)
			if !ok {
				t.Fatalf("expected authenticator to be instrumented")
			}
			authenticator, ok := ia.delegate.(*jwtAuthenticator)
			if !ok {
				t.Fatalf("expected delegate to be Authenticator")
			}
			// wait for the authenticator to be initialized
			err = wait.PollUntilContextCancel(ctx, time.Millisecond, true, func(context.Context) (bool, error) {
				if v, _ := authenticator.idTokenVerifier(); v == nil {
					return false, nil
				}
				return true, nil
			})
			if err != nil {
				t.Fatalf("failed to initialize the authenticator: %v", err)
			}

			if _, _, err = a.AuthenticateToken(ctx, token); err == nil {
				t.Fatalf("expected error but got nil")
			}

			metricFamilies, err := legacyregistry.DefaultGatherer.Gather()
			if err != nil {
				t.Fatalf("failed to gather metrics: %v", err)
			}

			jwtIssuerHash := getHash(ts.URL)
			var timestamp float64
			for _, family := range metricFamilies {
				if family.GetName() != "apiserver_authentication_jwt_authenticator_jwks_fetch_last_timestamp_seconds" {
					continue
				}

				labelFilter := map[string]string{
					"apiserver_id_hash": testAPIServerIDHash,
					"jwt_issuer_hash":   jwtIssuerHash,
					"result":            "failure",
				}
				if !testutil.LabelsMatch(family.Metric[0], labelFilter) {
					t.Fatalf("unexpected metric: %v", family.Metric[0])
				}

				timestamp = *family.Metric[0].Gauge.Value
				break
			}
			if timestamp == 0 {
				t.Fatalf("failed to get the timestamp")
			}
		})
	}
}

func TestJWKSMetrics(t *testing.T) {
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.StructuredAuthenticationConfiguration, true)
	ResetMetrics()

	synchronizeTokenIDVerifierForTest = true
	signingKey := loadRSAPrivKey(t, "testdata/rsa_1.pem", jose.RS256)
	pubKeys := []*jose.JSONWebKey{
		loadRSAKey(t, "testdata/rsa_1.pem", jose.RS256),
	}
	signer, err := jose.NewSigner(jose.SigningKey{
		Algorithm: jose.SignatureAlgorithm(signingKey.Algorithm),
		Key:       signingKey,
	}, nil)
	if err != nil {
		t.Fatalf("initialize signer: %v", err)
	}
	openIDConfig := `{
		"issuer": "{{.URL}}",
		"jwks_uri": "{{.URL}}/.testing/keys"
	}`

	ts := newClaimServer(t, toKeySet(pubKeys), signer, nil, &openIDConfig)
	defer ts.Close()

	v := struct {
		URL string
	}{
		URL: ts.URL,
	}
	openIDConfig = replace(openIDConfig, &v)

	opts := Options{
		JWTAuthenticator: apiserver.JWTAuthenticator{
			Issuer: apiserver.Issuer{
				URL:       ts.URL,
				Audiences: []string{"my-client"},
			},
			ClaimMappings: apiserver.ClaimMappings{
				Username: apiserver.PrefixedClaimOrExpression{
					Claim:  "username",
					Prefix: pointer.String(""),
				},
			},
		},
		now:         func() time.Time { return now },
		APIServerID: "testAPIServerID",
	}

	// Make the certificate of the helper server available to the authenticator
	caBundle := pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: ts.Certificate().Raw,
	})
	caContent, err := dynamiccertificates.NewStaticCAContent("oidc-authenticator", caBundle)
	if err != nil {
		t.Fatalf("initialize ca: %v", err)
	}
	opts.CAContentProvider = caContent

	ctx := testContext(t)
	// Initialize the authenticator.
	a, err := New(ctx, opts)
	if err != nil {
		t.Fatalf("initialize authenticator: %v", err)
	}

	claims := fmt.Sprintf(`{
"iss": "%s",
"aud": "my-client",
"username": "jane",
"exp": %d
}`, ts.URL, valid.Unix())

	jws, err := signer.Sign([]byte(claims))
	if err != nil {
		t.Fatalf("sign claims: %v", err)
	}
	token, err := jws.CompactSerialize()
	if err != nil {
		t.Fatalf("serialize token: %v", err)
	}

	ia, ok := a.(*instrumentedAuthenticator)
	if !ok {
		t.Fatalf("expected authenticator to be instrumented")
	}
	authenticator, ok := ia.delegate.(*jwtAuthenticator)
	if !ok {
		t.Fatalf("expected delegate to be Authenticator")
	}
	// wait for the authenticator to be initialized
	err = wait.PollUntilContextCancel(ctx, time.Millisecond, true, func(context.Context) (bool, error) {
		if v, _ := authenticator.idTokenVerifier(); v == nil {
			return false, nil
		}
		return true, nil
	})
	if err != nil {
		t.Fatalf("failed to initialize the authenticator: %v", err)
	}

	jwtIssuerHash := getHash(ts.URL)
	// 1. Remote JWKS fetch success the first time we fetch the keys.
	if _, _, err = a.AuthenticateToken(ctx, token); err != nil {
		t.Fatalf("authenticate token: %v", err)
	}
	checkJWKSMetrics(t, jwtIssuerHash, 5.482153416488015e+18)

	// 2. Rotate the keys and update the server.
	signingKey = loadRSAPrivKey(t, "testdata/rsa_2.pem", jose.RS256)
	pubKeys = []*jose.JSONWebKey{
		loadRSAKey(t, "testdata/rsa_1.pem", jose.RS256),
		loadRSAKey(t, "testdata/rsa_2.pem", jose.RS256),
	}
	signer, err = jose.NewSigner(jose.SigningKey{
		Algorithm: jose.SignatureAlgorithm(signingKey.Algorithm),
		Key:       signingKey,
	}, nil)
	if err != nil {
		t.Fatalf("initialize signer: %v", err)
	}
	ts.setKeys(toKeySet(pubKeys))
	// Sign and serialize the claims in a JWT with the new key.
	// This should trigger a new JWKS fetch as kid not found in the cache.
	jws, err = signer.Sign([]byte(claims))
	if err != nil {
		t.Fatalf("sign claims: %v", err)
	}
	token, err = jws.CompactSerialize()
	if err != nil {
		t.Fatalf("serialize token: %v", err)
	}
	if _, _, err = a.AuthenticateToken(ctx, token); err != nil {
		t.Fatalf("authenticate token: %v", err)
	}
	checkJWKSMetrics(t, jwtIssuerHash, 1.598836820000112e+19)
}

func checkJWKSMetrics(t *testing.T, jwtIssuerHash string, expectedJWKSFetchLastKeySetHash float64) {
	t.Helper()

	expectedMetricValue := fmt.Sprintf(`
       # HELP apiserver_authentication_jwt_authenticator_jwks_fetch_last_key_set_hash [ALPHA] Hash of the last JWKS fetched by the JWT authenticator split by api server identity and jwt issuer.
	   # TYPE apiserver_authentication_jwt_authenticator_jwks_fetch_last_key_set_hash gauge
	   apiserver_authentication_jwt_authenticator_jwks_fetch_last_key_set_hash{apiserver_id_hash="sha256:14f9d63e669337ac6bfda2e2162915ee6a6067743eddd4e5c374b572f951ff37",jwt_issuer_hash="%s"} %f
	`, jwtIssuerHash, expectedJWKSFetchLastKeySetHash)

	if err := testutil.GatherAndCompare(legacyregistry.DefaultGatherer, strings.NewReader(expectedMetricValue), "apiserver_authentication_jwt_authenticator_jwks_fetch_last_key_set_hash"); err != nil {
		t.Fatalf("unexpected metrics:\n%s", err)
	}

	metricFamilies, err := legacyregistry.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("failed to gather metrics: %v", err)
	}

	var ts float64
	for _, family := range metricFamilies {
		if family.GetName() != "apiserver_authentication_jwt_authenticator_jwks_fetch_last_timestamp_seconds" {
			continue
		}

		labelFilter := map[string]string{
			"apiserver_id_hash": testAPIServerIDHash,
			"jwt_issuer_hash":   jwtIssuerHash,
			"result":            "success",
		}
		if !testutil.LabelsMatch(family.Metric[0], labelFilter) {
			t.Fatalf("unexpected metric: %v", family.Metric[0])
		}

		ts = *family.Metric[0].Gauge.Value
		break
	}
	if ts == 0 {
		t.Fatalf("failed to get the timestamp")
	}
}

type errTransport string

func (e errTransport) RoundTrip(_ *http.Request) (*http.Response, error) {
	return nil, fmt.Errorf("%s", e)
}

func testContext(t *testing.T) context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return ctx
}
