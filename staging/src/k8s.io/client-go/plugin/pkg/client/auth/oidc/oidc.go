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

package oidc

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"
	"k8s.io/apimachinery/pkg/util/net"
	restclient "k8s.io/client-go/rest"
	"k8s.io/klog/v2"
)

const (
	cfgIssuerURL                = "idp-issuer-url"
	cfgClientID                 = "client-id"
	cfgClientSecret             = "client-secret"
	cfgCertificateAuthority     = "idp-certificate-authority"
	cfgCertificateAuthorityData = "idp-certificate-authority-data"
	cfgIDToken                  = "id-token"
	cfgRefreshToken             = "refresh-token"

	// Unused. Scopes aren't sent during refreshing.
	cfgExtraScopes = "extra-scopes"
)

func init() {
	if err := restclient.RegisterAuthProviderPlugin("oidc", newOIDCAuthProvider); err != nil {
		//nolint:logcheck // Should not happen.
		klog.Fatalf("Failed to register oidc auth plugin: %v", err)
	}
}

// expiryDelta determines how earlier a token should be considered
// expired than its actual expiration time. It is used to avoid late
// expirations due to client-server time mismatches.
//
// NOTE(ericchiang): this is take from golang.org/x/oauth2
const expiryDelta = 10 * time.Second

var cache = newClientCache()

// Like TLS transports, keep a cache of OIDC clients indexed by issuer URL. This ensures
// current requests from different clients don't concurrently attempt to refresh the same
// set of credentials.
type clientCache struct {
	mu sync.RWMutex

	cache map[cacheKey]*oidcAuthProvider
}

func newClientCache() *clientCache {
	return &clientCache{cache: make(map[cacheKey]*oidcAuthProvider)}
}

type cacheKey struct {
	clusterAddress string
	// Canonical issuer URL string of the provider.
	issuerURL string
	clientID  string
}

func (c *clientCache) getClient(clusterAddress, issuer, clientID string) (*oidcAuthProvider, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	client, ok := c.cache[cacheKey{clusterAddress: clusterAddress, issuerURL: issuer, clientID: clientID}]
	return client, ok
}

// setClient attempts to put the client in the cache but may return any clients
// with the same keys set before. This is so there's only ever one client for a provider.
func (c *clientCache) setClient(clusterAddress, issuer, clientID string, client *oidcAuthProvider) *oidcAuthProvider {
	c.mu.Lock()
	defer c.mu.Unlock()
	key := cacheKey{clusterAddress: clusterAddress, issuerURL: issuer, clientID: clientID}

	// If another client has already initialized a client for the given provider we want
	// to use that client instead of the one we're trying to set. This is so all transports
	// share a client and can coordinate around the same mutex when refreshing and writing
	// to the kubeconfig.
	if oldClient, ok := c.cache[key]; ok {
		return oldClient
	}

	c.cache[key] = client
	return client
}

func newOIDCAuthProvider(clusterAddress string, cfg map[string]string, persister restclient.AuthProviderConfigPersister) (restclient.AuthProvider, error) {
	issuer := cfg[cfgIssuerURL]
	if issuer == "" {
		return nil, fmt.Errorf("Must provide %s", cfgIssuerURL)
	}

	clientID := cfg[cfgClientID]
	if clientID == "" {
		return nil, fmt.Errorf("Must provide %s", cfgClientID)
	}

	// Check cache for existing provider.
	if provider, ok := cache.getClient(clusterAddress, issuer, clientID); ok {
		return provider, nil
	}

	if len(cfg[cfgExtraScopes]) > 0 {
		// TODO (?): the auth provider factory API would have to be changed
		// to support contextual logging here. Not worth it?
		klog.TODO().V(2).Info("Ignoring deprecated extra scopes, refresh request don't send scopes", "ignoredConfigField", cfgExtraScopes)
	}

	var certAuthData []byte
	var err error
	if cfg[cfgCertificateAuthorityData] != "" {
		certAuthData, err = base64.StdEncoding.DecodeString(cfg[cfgCertificateAuthorityData])
		if err != nil {
			return nil, err
		}
	}

	clientConfig := restclient.Config{
		TLSClientConfig: restclient.TLSClientConfig{
			CAFile: cfg[cfgCertificateAuthority],
			CAData: certAuthData,
		},
	}

	trans, err := restclient.TransportFor(&clientConfig)
	if err != nil {
		return nil, err
	}
	hc := &http.Client{Transport: trans}

	provider := &oidcAuthProvider{
		client:    hc,
		now:       time.Now,
		cfg:       cfg,
		persister: persister,
	}

	return cache.setClient(clusterAddress, issuer, clientID, provider), nil
}

type oidcAuthProvider struct {
	client *http.Client

	// Method for determining the current time.
	now func() time.Time

	// Mutex guards persisting to the kubeconfig file and allows synchronized
	// updates to the in-memory config. It also ensures concurrent calls to
	// the RoundTripper only trigger a single refresh request.
	mu        sync.Mutex
	cfg       map[string]string
	persister restclient.AuthProviderConfigPersister
}

func (p *oidcAuthProvider) WrapTransport(rt http.RoundTripper) http.RoundTripper {
	return &roundTripper{
		wrapped:  rt,
		provider: p,
	}
}

func (p *oidcAuthProvider) Login() error {
	return errors.New("not yet implemented")
}

type roundTripper struct {
	provider *oidcAuthProvider
	wrapped  http.RoundTripper
}

var _ net.RoundTripperWrapper = &roundTripper{}

func (r *roundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if len(req.Header.Get("Authorization")) != 0 {
		return r.wrapped.RoundTrip(req)
	}
	token, err := r.provider.idToken()
	if err != nil {
		return nil, err
	}

	// shallow copy of the struct
	r2 := new(http.Request)
	*r2 = *req
	// deep copy of the Header so we don't modify the original
	// request's Header (as per RoundTripper contract).
	r2.Header = make(http.Header)
	for k, s := range req.Header {
		r2.Header[k] = s
	}
	r2.Header.Set("Authorization", fmt.Sprintf("Bearer %s", token))

	return r.wrapped.RoundTrip(r2)
}

func (r *roundTripper) WrappedRoundTripper() http.RoundTripper { return r.wrapped }

func (p *oidcAuthProvider) idToken() (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if idToken, ok := p.cfg[cfgIDToken]; ok && len(idToken) > 0 {
		valid, err := idTokenExpired(p.now, idToken)
		if err != nil {
			return "", err
		}
		if valid {
			// If the cached id token is still valid use it.
			return idToken, nil
		}
	}

	// Try to request a new token using the refresh token.
	rt, ok := p.cfg[cfgRefreshToken]
	if !ok || len(rt) == 0 {
		return "", errors.New("No valid id-token, and cannot refresh without refresh-token")
	}

	// Determine provider's OAuth2 token endpoint.
	tokenURL, err := tokenEndpoint(p.client, p.cfg[cfgIssuerURL])
	if err != nil {
		return "", err
	}

	config := oauth2.Config{
		ClientID:     p.cfg[cfgClientID],
		ClientSecret: p.cfg[cfgClientSecret],
		Endpoint:     oauth2.Endpoint{TokenURL: tokenURL},
	}

	ctx := context.WithValue(context.Background(), oauth2.HTTPClient, p.client)
	token, err := config.TokenSource(ctx, &oauth2.Token{RefreshToken: rt}).Token()
	if err != nil {
		return "", fmt.Errorf("failed to refresh token: %v", err)
	}

	idToken, ok := token.Extra("id_token").(string)
	if !ok {
		// id_token isn't a required part of a refresh token response, so some
		// providers (Okta) don't return this value.
		//
		// See https://github.com/kubernetes/kubernetes/issues/36847
		return "", fmt.Errorf("token response did not contain an id_token, either the scope \"openid\" wasn't requested upon login, or the provider doesn't support id_tokens as part of the refresh response")
	}

	// Create a new config to persist.
	newCfg := make(map[string]string)
	for key, val := range p.cfg {
		newCfg[key] = val
	}

	// Update the refresh token if the server returned another one.
	if token.RefreshToken != "" && token.RefreshToken != rt {
		newCfg[cfgRefreshToken] = token.RefreshToken
	}
	newCfg[cfgIDToken] = idToken

	// Persist new config and if successful, update the in memory config.
	if err = p.persister.Persist(newCfg); err != nil {
		return "", fmt.Errorf("could not persist new tokens: %v", err)
	}
	p.cfg = newCfg

	return idToken, nil
}

// tokenEndpoint uses OpenID Connect discovery to determine the OAuth2 token
// endpoint for the provider, the endpoint the client will use the refresh
// token against.
func tokenEndpoint(client *http.Client, issuer string) (string, error) {
	// Well known URL for getting OpenID Connect metadata.
	//
	// https://openid.net/specs/openid-connect-discovery-1_0.html#ProviderConfig
	wellKnown := strings.TrimSuffix(issuer, "/") + "/.well-known/openid-configuration"
	resp, err := client.Get(wellKnown)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		// Don't produce an error that's too huge (e.g. if we get HTML back for some reason).
		const n = 80
		if len(body) > n {
			body = append(body[:n], []byte("...")...)
		}
		return "", fmt.Errorf("oidc: failed to query metadata endpoint %s: %q", resp.Status, body)
	}

	// Metadata object. We only care about the token_endpoint, the thing endpoint
	// we'll be refreshing against.
	//
	// https://openid.net/specs/openid-connect-discovery-1_0.html#ProviderMetadata
	var metadata struct {
		TokenURL string `json:"token_endpoint"`
	}
	if err := json.Unmarshal(body, &metadata); err != nil {
		return "", fmt.Errorf("oidc: failed to decode provider discovery object: %v", err)
	}
	if metadata.TokenURL == "" {
		return "", fmt.Errorf("oidc: discovery object doesn't contain a token_endpoint")
	}
	return metadata.TokenURL, nil
}

func idTokenExpired(now func() time.Time, idToken string) (bool, error) {
	parts := strings.Split(idToken, ".")
	if len(parts) != 3 {
		return false, fmt.Errorf("ID Token is not a valid JWT")
	}

	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return false, err
	}
	var claims struct {
		Expiry jsonTime `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return false, fmt.Errorf("parsing claims: %v", err)
	}

	return now().Add(expiryDelta).Before(time.Time(claims.Expiry)), nil
}

// jsonTime is a json.Unmarshaler that parses a unix timestamp.
// Because JSON numbers don't differentiate between ints and floats,
// we want to ensure we can parse either.
type jsonTime time.Time

func (j *jsonTime) UnmarshalJSON(b []byte) error {
	var n json.Number
	if err := json.Unmarshal(b, &n); err != nil {
		return err
	}
	var unix int64

	if t, err := n.Int64(); err == nil {
		unix = t
	} else {
		f, err := n.Float64()
		if err != nil {
			return err
		}
		unix = int64(f)
	}
	*j = jsonTime(time.Unix(unix, 0))
	return nil
}

func (j jsonTime) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Time(j).Unix())
}
