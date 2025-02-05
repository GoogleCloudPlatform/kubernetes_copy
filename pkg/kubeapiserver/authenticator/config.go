/*
Copyright 2014 The Kubernetes Authors.

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

package authenticator

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	utilnet "k8s.io/apimachinery/pkg/util/net"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apiserver/pkg/apis/apiserver"
	"k8s.io/apiserver/pkg/authentication/authenticator"
	"k8s.io/apiserver/pkg/authentication/authenticatorfactory"
	"k8s.io/apiserver/pkg/authentication/group"
	"k8s.io/apiserver/pkg/authentication/request/anonymous"
	"k8s.io/apiserver/pkg/authentication/request/bearertoken"
	"k8s.io/apiserver/pkg/authentication/request/headerrequest"
	"k8s.io/apiserver/pkg/authentication/request/union"
	"k8s.io/apiserver/pkg/authentication/request/websocket"
	"k8s.io/apiserver/pkg/authentication/request/x509"
	tokencache "k8s.io/apiserver/pkg/authentication/token/cache"
	"k8s.io/apiserver/pkg/authentication/token/tokenfile"
	tokenunion "k8s.io/apiserver/pkg/authentication/token/union"
	"k8s.io/apiserver/pkg/server/dynamiccertificates"
	webhookutil "k8s.io/apiserver/pkg/util/webhook"
	"k8s.io/apiserver/plugin/pkg/authenticator/token/oidc"
	"k8s.io/apiserver/plugin/pkg/authenticator/token/webhook"
	typedv1core "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/kube-openapi/pkg/spec3"
	"k8s.io/kube-openapi/pkg/validation/spec"

	// Initialize all known client auth plugins.
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	"k8s.io/kubernetes/pkg/serviceaccount"
)

// Config contains the data on how to authenticate a request to the Kube API Server
type Config struct {
	// Anonymous holds the effective anonymous config, specified either via config file
	// (hoisted out of AuthenticationConfig) or via flags (constructed from flag-specified values).
	Anonymous apiserver.AnonymousAuthConfig

	BootstrapToken bool

	TokenAuthFile               string
	AuthenticationConfig        *apiserver.AuthenticationConfiguration
	AuthenticationConfigData    string
	OIDCSigningAlgs             []string
	ServiceAccountLookup        bool
	ServiceAccountIssuers       []string
	APIAudiences                authenticator.Audiences
	WebhookTokenAuthnConfigFile string
	WebhookTokenAuthnVersion    string
	WebhookTokenAuthnCacheTTL   time.Duration
	// WebhookRetryBackoff specifies the backoff parameters for the authentication webhook retry logic.
	// This allows us to configure the sleep time at each iteration and the maximum number of retries allowed
	// before we fail the webhook call in order to limit the fan out that ensues when the system is degraded.
	WebhookRetryBackoff *wait.Backoff

	TokenSuccessCacheTTL time.Duration
	TokenFailureCacheTTL time.Duration

	RequestHeaderConfig *authenticatorfactory.RequestHeaderConfig

	// ServiceAccountPublicKeysGetter returns public keys for verifying service account tokens.
	ServiceAccountPublicKeysGetter serviceaccount.PublicKeysGetter
	// ServiceAccountTokenGetter fetches API objects used to verify bound objects in service account token claims.
	ServiceAccountTokenGetter   serviceaccount.ServiceAccountTokenGetter
	SecretsWriter               typedv1core.SecretsGetter
	BootstrapTokenAuthenticator authenticator.Token
	// ClientCAContentProvider are the options for verifying incoming connections using mTLS and directly assigning to users.
	// Generally this is the CA bundle file used to authenticate client certificates
	// If this value is nil, then mutual TLS is disabled.
	ClientCAContentProvider dynamiccertificates.CAContentProvider

	// Optional field, custom dial function used to connect to webhook
	CustomDial utilnet.DialFunc

	// APIServerID is the ID of the API server
	APIServerID string
}

// New returns an authenticator.Request or an error that supports the standard
// Kubernetes authentication mechanisms.
func (config Config) New(serverLifecycle context.Context) (authenticator.Request, func(context.Context, *apiserver.AuthenticationConfiguration) error, *spec.SecurityDefinitions, spec3.SecuritySchemes, error) {
	var authenticators []authenticator.Request
	var tokenAuthenticators []authenticator.Token
	securityDefinitionsV2 := spec.SecurityDefinitions{}
	securitySchemesV3 := spec3.SecuritySchemes{}

	// front-proxy, BasicAuth methods, local first, then remote
	// Add the front proxy authenticator if requested
	if config.RequestHeaderConfig != nil {
		requestHeaderAuthenticator := headerrequest.NewDynamicVerifyOptionsSecure(
			config.RequestHeaderConfig.CAContentProvider.VerifyOptions,
			config.RequestHeaderConfig.AllowedClientNames,
			config.RequestHeaderConfig.UsernameHeaders,
			config.RequestHeaderConfig.UIDHeaders,
			config.RequestHeaderConfig.GroupHeaders,
			config.RequestHeaderConfig.ExtraHeaderPrefixes,
		)
		authenticators = append(authenticators, authenticator.WrapAudienceAgnosticRequest(config.APIAudiences, requestHeaderAuthenticator))
	}

	// X509 methods
	if config.ClientCAContentProvider != nil {
		certAuth := x509.NewDynamic(config.ClientCAContentProvider.VerifyOptions, x509.CommonNameUserConversion)
		authenticators = append(authenticators, certAuth)
	}

	// Bearer token methods, local first, then remote
	if len(config.TokenAuthFile) > 0 {
		tokenAuth, err := newAuthenticatorFromTokenFile(config.TokenAuthFile)
		if err != nil {
			return nil, nil, nil, nil, err
		}
		tokenAuthenticators = append(tokenAuthenticators, authenticator.WrapAudienceAgnosticToken(config.APIAudiences, tokenAuth))
	}
	if config.ServiceAccountPublicKeysGetter != nil {
		serviceAccountAuth, err := newLegacyServiceAccountAuthenticator(config.ServiceAccountPublicKeysGetter, config.ServiceAccountLookup, config.APIAudiences, config.ServiceAccountTokenGetter, config.SecretsWriter)
		if err != nil {
			return nil, nil, nil, nil, err
		}
		tokenAuthenticators = append(tokenAuthenticators, serviceAccountAuth)
	}
	if len(config.ServiceAccountIssuers) > 0 && config.ServiceAccountPublicKeysGetter != nil {
		serviceAccountAuth, err := newServiceAccountAuthenticator(config.ServiceAccountIssuers, config.ServiceAccountPublicKeysGetter, config.APIAudiences, config.ServiceAccountTokenGetter)
		if err != nil {
			return nil, nil, nil, nil, err
		}
		tokenAuthenticators = append(tokenAuthenticators, serviceAccountAuth)
	}

	if config.BootstrapToken && config.BootstrapTokenAuthenticator != nil {
		tokenAuthenticators = append(tokenAuthenticators, authenticator.WrapAudienceAgnosticToken(config.APIAudiences, config.BootstrapTokenAuthenticator))
	}

	// NOTE(ericchiang): Keep the OpenID Connect after Service Accounts.
	//
	// Because both plugins verify JWTs whichever comes first in the union experiences
	// cache misses for all requests using the other. While the service account plugin
	// simply returns an error, the OpenID Connect plugin may query the provider to
	// update the keys, causing performance hits.
	var updateAuthenticationConfig func(context.Context, *apiserver.AuthenticationConfiguration) error
	if config.AuthenticationConfig != nil {
		initialJWTAuthenticator, err := newJWTAuthenticator(serverLifecycle, config.AuthenticationConfig, config.OIDCSigningAlgs, config.APIAudiences, config.ServiceAccountIssuers, config.APIServerID)
		if err != nil {
			return nil, nil, nil, nil, err
		}

		jwtAuthenticatorPtr := &atomic.Pointer[jwtAuthenticatorWithCancel]{}
		jwtAuthenticatorPtr.Store(initialJWTAuthenticator)

		updateAuthenticationConfig = (&authenticationConfigUpdater{
			serverLifecycle:     serverLifecycle,
			config:              config,
			jwtAuthenticatorPtr: jwtAuthenticatorPtr,
			apiServerID:         config.APIServerID,
		}).updateAuthenticationConfig

		tokenAuthenticators = append(tokenAuthenticators,
			authenticator.TokenFunc(func(ctx context.Context, token string) (*authenticator.Response, bool, error) {
				return jwtAuthenticatorPtr.Load().jwtAuthenticator.AuthenticateToken(ctx, token)
			}),
		)
	}

	if len(config.WebhookTokenAuthnConfigFile) > 0 {
		webhookTokenAuth, err := newWebhookTokenAuthenticator(config)
		if err != nil {
			return nil, nil, nil, nil, err
		}

		tokenAuthenticators = append(tokenAuthenticators, webhookTokenAuth)
	}

	if len(tokenAuthenticators) > 0 {
		// Union the token authenticators
		tokenAuth := tokenunion.New(tokenAuthenticators...)
		// Optionally cache authentication results
		if config.TokenSuccessCacheTTL > 0 || config.TokenFailureCacheTTL > 0 {
			tokenAuth = tokencache.New(tokenAuth, true, config.TokenSuccessCacheTTL, config.TokenFailureCacheTTL)
		}
		authenticators = append(authenticators, bearertoken.New(tokenAuth), websocket.NewProtocolAuthenticator(tokenAuth))

		securityDefinitionsV2["BearerToken"] = &spec.SecurityScheme{
			SecuritySchemeProps: spec.SecuritySchemeProps{
				Type:        "apiKey",
				Name:        "authorization",
				In:          "header",
				Description: "Bearer Token authentication",
			},
		}
		securitySchemesV3["BearerToken"] = &spec3.SecurityScheme{
			SecuritySchemeProps: spec3.SecuritySchemeProps{
				Type:        "apiKey",
				Name:        "authorization",
				In:          "header",
				Description: "Bearer Token authentication",
			},
		}
	}

	if len(authenticators) == 0 {
		if config.Anonymous.Enabled {
			return anonymous.NewAuthenticator(config.Anonymous.Conditions), nil, &securityDefinitionsV2, securitySchemesV3, nil
		}
		return nil, nil, &securityDefinitionsV2, securitySchemesV3, nil
	}

	authenticator := union.New(authenticators...)

	authenticator = group.NewAuthenticatedGroupAdder(authenticator)

	if config.Anonymous.Enabled {
		// If the authenticator chain returns an error, return an error (don't consider a bad bearer token
		// or invalid username/password combination anonymous).
		authenticator = union.NewFailOnError(authenticator, anonymous.NewAuthenticator(config.Anonymous.Conditions))
	}

	return authenticator, updateAuthenticationConfig, &securityDefinitionsV2, securitySchemesV3, nil
}

type jwtAuthenticatorWithCancel struct {
	jwtAuthenticator authenticator.Token
	healthCheck      func() error
	cancel           func()
}

func newJWTAuthenticator(serverLifecycle context.Context, config *apiserver.AuthenticationConfiguration, oidcSigningAlgs []string, apiAudiences authenticator.Audiences, disallowedIssuers []string, apiServerID string) (_ *jwtAuthenticatorWithCancel, buildErr error) {
	ctx, cancel := context.WithCancel(serverLifecycle)

	defer func() {
		if buildErr != nil {
			cancel()
		}
	}()
	var jwtAuthenticators []authenticator.Token
	var healthChecks []func() error
	for _, jwtAuthenticator := range config.JWT {
		// TODO remove this CAContentProvider indirection
		var oidcCAContent oidc.CAContentProvider
		if len(jwtAuthenticator.Issuer.CertificateAuthority) > 0 {
			var oidcCAError error
			oidcCAContent, oidcCAError = dynamiccertificates.NewStaticCAContent("oidc-authenticator", []byte(jwtAuthenticator.Issuer.CertificateAuthority))
			if oidcCAError != nil {
				return nil, oidcCAError
			}
		}
		oidcAuth, err := oidc.New(ctx, oidc.Options{
			JWTAuthenticator:     jwtAuthenticator,
			CAContentProvider:    oidcCAContent,
			SupportedSigningAlgs: oidcSigningAlgs,
			DisallowedIssuers:    disallowedIssuers,
			APIServerID:          apiServerID,
		})
		if err != nil {
			return nil, err
		}
		jwtAuthenticators = append(jwtAuthenticators, oidcAuth)
		healthChecks = append(healthChecks, oidcAuth.HealthCheck)
	}
	return &jwtAuthenticatorWithCancel{
		jwtAuthenticator: authenticator.WrapAudienceAgnosticToken(apiAudiences, tokenunion.NewFailOnError(jwtAuthenticators...)), // this handles the empty jwtAuthenticators slice case correctly
		healthCheck: func() error {
			var errs []error
			for _, check := range healthChecks {
				if err := check(); err != nil {
					errs = append(errs, err)
				}
			}
			return utilerrors.NewAggregate(errs)
		},
		cancel: cancel,
	}, nil
}

type authenticationConfigUpdater struct {
	serverLifecycle     context.Context
	config              Config
	jwtAuthenticatorPtr *atomic.Pointer[jwtAuthenticatorWithCancel]
	apiServerID         string
}

// the input ctx controls the timeout for updateAuthenticationConfig to return, not the lifetime of the constructed authenticators.
func (c *authenticationConfigUpdater) updateAuthenticationConfig(ctx context.Context, authConfig *apiserver.AuthenticationConfiguration) error {
	updatedJWTAuthenticator, err := newJWTAuthenticator(c.serverLifecycle, authConfig, c.config.OIDCSigningAlgs, c.config.APIAudiences, c.config.ServiceAccountIssuers, c.apiServerID)
	if err != nil {
		return err
	}

	var lastErr error
	if waitErr := wait.PollUntilContextCancel(ctx, 10*time.Second, true, func(_ context.Context) (done bool, err error) {
		lastErr = updatedJWTAuthenticator.healthCheck()
		return lastErr == nil, nil
	}); lastErr != nil || waitErr != nil {
		updatedJWTAuthenticator.cancel()
		return utilerrors.NewAggregate([]error{lastErr, waitErr}) // filters out nil errors
	}

	// Reset the metrics for the jwt authenticators, since we have a new set of them
	// and don't want the metrics to be stale or grow unbounded.
	// This will reset the following metrics:
	// - jwtAuthenticatorLatencyMetric
	// - jwksFetchLastTimestampSeconds
	// - jwksFetchLastKeySetHash
	// as they are split by the jwt issuer hash.
	//
	// We swap the new authenticator in the next step, and cancel the old ones after a minute.
	// This can lead to a brief period when the old authenticators are still in use and might
	// record metrics. These stale metrics would get reset in the next reload cycle and the
	// metrics will not grow unbounded.
	//
	// TODO(aramase): Investigate if we want to optimize this behavior in the v1.32 release
	// to prevent the brief period of stale metrics.
	oidc.ResetMetrics()

	oldJWTAuthenticator := c.jwtAuthenticatorPtr.Swap(updatedJWTAuthenticator)
	go func() {
		t := time.NewTimer(time.Minute)
		defer t.Stop()
		select {
		case <-c.serverLifecycle.Done():
		case <-t.C:
		}
		// TODO maybe track requests so we know when this is safe to do
		oldJWTAuthenticator.cancel()
	}()

	return nil
}

// newAuthenticatorFromTokenFile returns an authenticator.Token or an error
func newAuthenticatorFromTokenFile(tokenAuthFile string) (authenticator.Token, error) {
	tokenAuthenticator, err := tokenfile.NewCSV(tokenAuthFile)
	if err != nil {
		return nil, err
	}

	return tokenAuthenticator, nil
}

// newLegacyServiceAccountAuthenticator returns an authenticator.Token or an error
func newLegacyServiceAccountAuthenticator(publicKeysGetter serviceaccount.PublicKeysGetter, lookup bool, apiAudiences authenticator.Audiences, serviceAccountGetter serviceaccount.ServiceAccountTokenGetter, secretsWriter typedv1core.SecretsGetter) (authenticator.Token, error) {
	if publicKeysGetter == nil {
		return nil, fmt.Errorf("no public key getter provided")
	}
	validator, err := serviceaccount.NewLegacyValidator(lookup, serviceAccountGetter, secretsWriter)
	if err != nil {
		return nil, fmt.Errorf("while creating legacy validator, err: %w", err)
	}

	tokenAuthenticator := serviceaccount.JWTTokenAuthenticator([]string{serviceaccount.LegacyIssuer}, publicKeysGetter, apiAudiences, validator)
	return tokenAuthenticator, nil
}

// newServiceAccountAuthenticator returns an authenticator.Token or an error
func newServiceAccountAuthenticator(issuers []string, publicKeysGetter serviceaccount.PublicKeysGetter, apiAudiences authenticator.Audiences, serviceAccountGetter serviceaccount.ServiceAccountTokenGetter) (authenticator.Token, error) {
	if publicKeysGetter == nil {
		return nil, fmt.Errorf("no public key getter provided")
	}
	tokenAuthenticator := serviceaccount.JWTTokenAuthenticator(issuers, publicKeysGetter, apiAudiences, serviceaccount.NewValidator(serviceAccountGetter))
	return tokenAuthenticator, nil
}

func newWebhookTokenAuthenticator(config Config) (authenticator.Token, error) {
	if config.WebhookRetryBackoff == nil {
		return nil, errors.New("retry backoff parameters for authentication webhook has not been specified")
	}

	clientConfig, err := webhookutil.LoadKubeconfig(config.WebhookTokenAuthnConfigFile, config.CustomDial)
	if err != nil {
		return nil, err
	}
	webhookTokenAuthenticator, err := webhook.New(clientConfig, config.WebhookTokenAuthnVersion, config.APIAudiences, *config.WebhookRetryBackoff)
	if err != nil {
		return nil, err
	}

	return tokencache.New(webhookTokenAuthenticator, false, config.WebhookTokenAuthnCacheTTL, config.WebhookTokenAuthnCacheTTL), nil
}
