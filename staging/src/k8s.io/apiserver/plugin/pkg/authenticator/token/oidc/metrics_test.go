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

package oidc

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"k8s.io/apiserver/pkg/authentication/authenticator"
	"k8s.io/component-base/metrics/legacyregistry"
	"k8s.io/component-base/metrics/testutil"
)

const (
	testIssuer          = "testIssuer"
	testAPIServerID     = "testAPIServerID"
	testAPIServerIDHash = "sha256:14f9d63e669337ac6bfda2e2162915ee6a6067743eddd4e5c374b572f951ff37"
	testJWTIssuerHash   = "sha256:29b34beedc55b972f2428f21bc588f9d38e5e8f7a7af825486e7bb4fd9caa2ad"
)

func TestRecordAuthenticationLatency(t *testing.T) {
	tests := []struct {
		name            string
		authenticator   AuthenticatorTokenWithHealthCheck
		generateMetrics func()
		expectedValue   string
	}{
		{
			name:          "success",
			authenticator: &dummyAuthenticator{response: &authenticator.Response{}, ok: true},
			expectedValue: `
        # HELP apiserver_authentication_jwt_authenticator_latency_seconds [ALPHA] Latency of jwt authentication operations in seconds. This is the time spent authenticating a token for cache miss only (i.e. when the token is not found in the cache).
        # TYPE apiserver_authentication_jwt_authenticator_latency_seconds histogram
        apiserver_authentication_jwt_authenticator_latency_seconds_bucket{jwt_issuer_hash="sha256:29b34beedc55b972f2428f21bc588f9d38e5e8f7a7af825486e7bb4fd9caa2ad",result="success",le="0.001"} 1
        apiserver_authentication_jwt_authenticator_latency_seconds_bucket{jwt_issuer_hash="sha256:29b34beedc55b972f2428f21bc588f9d38e5e8f7a7af825486e7bb4fd9caa2ad",result="success",le="0.005"} 1
        apiserver_authentication_jwt_authenticator_latency_seconds_bucket{jwt_issuer_hash="sha256:29b34beedc55b972f2428f21bc588f9d38e5e8f7a7af825486e7bb4fd9caa2ad",result="success",le="0.01"} 1
        apiserver_authentication_jwt_authenticator_latency_seconds_bucket{jwt_issuer_hash="sha256:29b34beedc55b972f2428f21bc588f9d38e5e8f7a7af825486e7bb4fd9caa2ad",result="success",le="0.025"} 1
        apiserver_authentication_jwt_authenticator_latency_seconds_bucket{jwt_issuer_hash="sha256:29b34beedc55b972f2428f21bc588f9d38e5e8f7a7af825486e7bb4fd9caa2ad",result="success",le="0.05"} 1
        apiserver_authentication_jwt_authenticator_latency_seconds_bucket{jwt_issuer_hash="sha256:29b34beedc55b972f2428f21bc588f9d38e5e8f7a7af825486e7bb4fd9caa2ad",result="success",le="0.1"} 1
        apiserver_authentication_jwt_authenticator_latency_seconds_bucket{jwt_issuer_hash="sha256:29b34beedc55b972f2428f21bc588f9d38e5e8f7a7af825486e7bb4fd9caa2ad",result="success",le="0.25"} 1
        apiserver_authentication_jwt_authenticator_latency_seconds_bucket{jwt_issuer_hash="sha256:29b34beedc55b972f2428f21bc588f9d38e5e8f7a7af825486e7bb4fd9caa2ad",result="success",le="0.5"} 1
        apiserver_authentication_jwt_authenticator_latency_seconds_bucket{jwt_issuer_hash="sha256:29b34beedc55b972f2428f21bc588f9d38e5e8f7a7af825486e7bb4fd9caa2ad",result="success",le="1"} 1
        apiserver_authentication_jwt_authenticator_latency_seconds_bucket{jwt_issuer_hash="sha256:29b34beedc55b972f2428f21bc588f9d38e5e8f7a7af825486e7bb4fd9caa2ad",result="success",le="2.5"} 1
        apiserver_authentication_jwt_authenticator_latency_seconds_bucket{jwt_issuer_hash="sha256:29b34beedc55b972f2428f21bc588f9d38e5e8f7a7af825486e7bb4fd9caa2ad",result="success",le="5"} 1
        apiserver_authentication_jwt_authenticator_latency_seconds_bucket{jwt_issuer_hash="sha256:29b34beedc55b972f2428f21bc588f9d38e5e8f7a7af825486e7bb4fd9caa2ad",result="success",le="10"} 1
        apiserver_authentication_jwt_authenticator_latency_seconds_bucket{jwt_issuer_hash="sha256:29b34beedc55b972f2428f21bc588f9d38e5e8f7a7af825486e7bb4fd9caa2ad",result="success",le="+Inf"} 1
        apiserver_authentication_jwt_authenticator_latency_seconds_sum{jwt_issuer_hash="sha256:29b34beedc55b972f2428f21bc588f9d38e5e8f7a7af825486e7bb4fd9caa2ad",result="success"} 1e-09
        apiserver_authentication_jwt_authenticator_latency_seconds_count{jwt_issuer_hash="sha256:29b34beedc55b972f2428f21bc588f9d38e5e8f7a7af825486e7bb4fd9caa2ad",result="success"} 1
		`,
		},
		{
			name:          "error",
			authenticator: &dummyAuthenticator{response: &authenticator.Response{}, ok: false, err: fmt.Errorf("error")},
			expectedValue: `
        # HELP apiserver_authentication_jwt_authenticator_latency_seconds [ALPHA] Latency of jwt authentication operations in seconds. This is the time spent authenticating a token for cache miss only (i.e. when the token is not found in the cache).
        # TYPE apiserver_authentication_jwt_authenticator_latency_seconds histogram
        apiserver_authentication_jwt_authenticator_latency_seconds_bucket{jwt_issuer_hash="sha256:29b34beedc55b972f2428f21bc588f9d38e5e8f7a7af825486e7bb4fd9caa2ad",result="failure",le="0.001"} 1
        apiserver_authentication_jwt_authenticator_latency_seconds_bucket{jwt_issuer_hash="sha256:29b34beedc55b972f2428f21bc588f9d38e5e8f7a7af825486e7bb4fd9caa2ad",result="failure",le="0.005"} 1
        apiserver_authentication_jwt_authenticator_latency_seconds_bucket{jwt_issuer_hash="sha256:29b34beedc55b972f2428f21bc588f9d38e5e8f7a7af825486e7bb4fd9caa2ad",result="failure",le="0.01"} 1
        apiserver_authentication_jwt_authenticator_latency_seconds_bucket{jwt_issuer_hash="sha256:29b34beedc55b972f2428f21bc588f9d38e5e8f7a7af825486e7bb4fd9caa2ad",result="failure",le="0.025"} 1
        apiserver_authentication_jwt_authenticator_latency_seconds_bucket{jwt_issuer_hash="sha256:29b34beedc55b972f2428f21bc588f9d38e5e8f7a7af825486e7bb4fd9caa2ad",result="failure",le="0.05"} 1
        apiserver_authentication_jwt_authenticator_latency_seconds_bucket{jwt_issuer_hash="sha256:29b34beedc55b972f2428f21bc588f9d38e5e8f7a7af825486e7bb4fd9caa2ad",result="failure",le="0.1"} 1
        apiserver_authentication_jwt_authenticator_latency_seconds_bucket{jwt_issuer_hash="sha256:29b34beedc55b972f2428f21bc588f9d38e5e8f7a7af825486e7bb4fd9caa2ad",result="failure",le="0.25"} 1
        apiserver_authentication_jwt_authenticator_latency_seconds_bucket{jwt_issuer_hash="sha256:29b34beedc55b972f2428f21bc588f9d38e5e8f7a7af825486e7bb4fd9caa2ad",result="failure",le="0.5"} 1
        apiserver_authentication_jwt_authenticator_latency_seconds_bucket{jwt_issuer_hash="sha256:29b34beedc55b972f2428f21bc588f9d38e5e8f7a7af825486e7bb4fd9caa2ad",result="failure",le="1"} 1
        apiserver_authentication_jwt_authenticator_latency_seconds_bucket{jwt_issuer_hash="sha256:29b34beedc55b972f2428f21bc588f9d38e5e8f7a7af825486e7bb4fd9caa2ad",result="failure",le="2.5"} 1
        apiserver_authentication_jwt_authenticator_latency_seconds_bucket{jwt_issuer_hash="sha256:29b34beedc55b972f2428f21bc588f9d38e5e8f7a7af825486e7bb4fd9caa2ad",result="failure",le="5"} 1
        apiserver_authentication_jwt_authenticator_latency_seconds_bucket{jwt_issuer_hash="sha256:29b34beedc55b972f2428f21bc588f9d38e5e8f7a7af825486e7bb4fd9caa2ad",result="failure",le="10"} 1
        apiserver_authentication_jwt_authenticator_latency_seconds_bucket{jwt_issuer_hash="sha256:29b34beedc55b972f2428f21bc588f9d38e5e8f7a7af825486e7bb4fd9caa2ad",result="failure",le="+Inf"} 1
        apiserver_authentication_jwt_authenticator_latency_seconds_sum{jwt_issuer_hash="sha256:29b34beedc55b972f2428f21bc588f9d38e5e8f7a7af825486e7bb4fd9caa2ad",result="failure"} 1e-09
        apiserver_authentication_jwt_authenticator_latency_seconds_count{jwt_issuer_hash="sha256:29b34beedc55b972f2428f21bc588f9d38e5e8f7a7af825486e7bb4fd9caa2ad",result="failure"} 1
		`,
		},
		{
			name:          "no metrics when issuer doesn't match",
			authenticator: &dummyAuthenticator{response: &authenticator.Response{}, ok: false, err: nil},
			expectedValue: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			jwtAuthenticatorLatencyMetric.Reset()
			RegisterMetrics()

			a := newInstrumentedAuthenticatorWithClock(testIssuer, tt.authenticator, dummyClock{})
			_, _, _ = a.AuthenticateToken(context.Background(), "token")

			if err := testutil.GatherAndCompare(legacyregistry.DefaultGatherer, strings.NewReader(tt.expectedValue), "apiserver_authentication_jwt_authenticator_latency_seconds"); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRecordJWKSFetchKeySetHash(t *testing.T) {
	RegisterMetrics()
	jwksFetchLastKeySetHash.Reset()

	testKeySet := `{"keys":[{"use":"sig","kty":"RSA","kid":"d96cf58fcd9c6a2dba65f71df8b8a65cd9e3be8127695184fe6269b89fcc43d0","alg":"RS256","n":"0pXWMYjWRjBEds_fKj_u9r2E6SIDx0J-TAg-eyVeR20Ky9jZmIXW5zSxE_EKpNQpiBWm1e6G9kmhMuqjr7g455S7E-3rD3OVkdTT6SU5AKBNSFoRXUd-G_YJEtRzrpEYNtEJHkxUxWuyfCHblHSt-wsrE6t0DccCqC87lKQiGb_QfC8uP6ZS99SCjKBEFp1fZvyNkYwStFc2OH5fBGPXXb6SNsquvDeKX9NeWjXkmxDkbOg2kSkel4s_zw5KwcW3JzERfEcLStrDQ8fRbJ1C3uC088sUk4q4APQmKI_8FTvJe431Vne9sOSptphiqCjlR-Knja58rc_vt4TkSPZf2w","e":"AQAB"}]}`
	recordJWKSFetchKeySetHash(testIssuer, testAPIServerID, testKeySet)

	expectedMetricValue := `
       # HELP apiserver_authentication_jwt_authenticator_jwks_fetch_last_key_set_hash [ALPHA] Hash of the last JWKS fetched by the JWT authenticator split by api server identity and jwt issuer.
	   # TYPE apiserver_authentication_jwt_authenticator_jwks_fetch_last_key_set_hash gauge
	   apiserver_authentication_jwt_authenticator_jwks_fetch_last_key_set_hash{apiserver_id_hash="sha256:14f9d63e669337ac6bfda2e2162915ee6a6067743eddd4e5c374b572f951ff37",jwt_issuer_hash="sha256:29b34beedc55b972f2428f21bc588f9d38e5e8f7a7af825486e7bb4fd9caa2ad"} 5.482153416488015e+18
	`

	if err := testutil.GatherAndCompare(legacyregistry.DefaultGatherer, strings.NewReader(expectedMetricValue), "apiserver_authentication_jwt_authenticator_jwks_fetch_last_key_set_hash"); err != nil {
		t.Fatalf("unexpected metrics:\n%s", err)
	}
}

func TestRecordJWKSFetchTimestamp(t *testing.T) {
	RegisterMetrics()
	jwksFetchLastTimestampSeconds.Reset()

	recordJWKSFetchTimestamp("success", testIssuer, testAPIServerID)

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
			"jwt_issuer_hash":   testJWTIssuerHash,
			"result":            "success",
		}
		if !testutil.LabelsMatch(family.Metric[0], labelFilter) {
			t.Fatalf("unexpected metric: %v", family.Metric[0])
		}

		ts = *family.Metric[0].Gauge.Value
	}
	if ts == 0 {
		t.Fatalf("failed to get the timestamp")
	}
}

type dummyAuthenticator struct {
	response *authenticator.Response
	ok       bool
	err      error
}

func (a *dummyAuthenticator) AuthenticateToken(ctx context.Context, token string) (*authenticator.Response, bool, error) {
	return a.response, a.ok, a.err
}

func (a *dummyAuthenticator) HealthCheck() error {
	panic("should not be called")
}

type dummyClock struct {
}

func (d dummyClock) Now() time.Time {
	return time.Now()
}

func (d dummyClock) Since(t time.Time) time.Duration {
	return time.Duration(1)
}
