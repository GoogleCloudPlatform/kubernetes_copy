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

package options

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/spf13/pflag"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apiserver/pkg/apis/apiserver"
	"k8s.io/apiserver/pkg/apis/apiserver/install"
	apiservervalidation "k8s.io/apiserver/pkg/apis/apiserver/validation"
	"k8s.io/apiserver/pkg/authentication/authenticator"
	authenticationcel "k8s.io/apiserver/pkg/authentication/cel"
	genericfeatures "k8s.io/apiserver/pkg/features"
	genericapiserver "k8s.io/apiserver/pkg/server"
	"k8s.io/apiserver/pkg/server/egressselector"
	genericoptions "k8s.io/apiserver/pkg/server/options"
	authenticationconfigmetrics "k8s.io/apiserver/pkg/server/options/authenticationconfig/metrics"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/apiserver/plugin/pkg/authenticator/token/oidc"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	v1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/util/keyutil"
	cliflag "k8s.io/component-base/cli/flag"
	"k8s.io/klog/v2"
	openapicommon "k8s.io/kube-openapi/pkg/common"
	serviceaccountcontroller "k8s.io/kubernetes/pkg/controller/serviceaccount"
	"k8s.io/kubernetes/pkg/features"
	kubeauthenticator "k8s.io/kubernetes/pkg/kubeapiserver/authenticator"
	authzmodes "k8s.io/kubernetes/pkg/kubeapiserver/authorizer/modes"
	"k8s.io/kubernetes/pkg/serviceaccount"
	"k8s.io/kubernetes/pkg/util/filesystem"
	"k8s.io/kubernetes/plugin/pkg/auth/authenticator/token/bootstrap"
	"k8s.io/utils/pointer"
)

const (
	oidcIssuerURLFlag      = "oidc-issuer-url"
	oidcClientIDFlag       = "oidc-client-id"
	oidcCAFileFlag         = "oidc-ca-file"
	oidcUsernameClaimFlag  = "oidc-username-claim"
	oidcUsernamePrefixFlag = "oidc-username-prefix"
	oidcGroupsClaimFlag    = "oidc-groups-claim"
	oidcGroupsPrefixFlag   = "oidc-groups-prefix"
	oidcSigningAlgsFlag    = "oidc-signing-algs"
	oidcRequiredClaimFlag  = "oidc-required-claim"
)

// UpdateAuthenticationConfigTimeout controls how long we wait for calls to updateAuthenticationConfig to succeed.
// Exported as a variable so that it can be overridden in integration tests.
var UpdateAuthenticationConfigTimeout = time.Minute

// BuiltInAuthenticationOptions contains all build-in authentication options for API Server
type BuiltInAuthenticationOptions struct {
	APIAudiences    []string
	Anonymous       *AnonymousAuthenticationOptions
	BootstrapToken  *BootstrapTokenAuthenticationOptions
	ClientCert      *genericoptions.ClientCertAuthenticationOptions
	OIDC            *OIDCAuthenticationOptions
	RequestHeader   *genericoptions.RequestHeaderAuthenticationOptions
	ServiceAccounts *ServiceAccountAuthenticationOptions
	TokenFile       *TokenFileAuthenticationOptions
	WebHook         *WebHookAuthenticationOptions

	AuthenticationConfigFile string

	TokenSuccessCacheTTL time.Duration
	TokenFailureCacheTTL time.Duration
}

// AnonymousAuthenticationOptions contains anonymous authentication options for API Server
type AnonymousAuthenticationOptions struct {
	Allow       bool
	areFlagsSet func() bool
}

// BootstrapTokenAuthenticationOptions contains bootstrap token authentication options for API Server
type BootstrapTokenAuthenticationOptions struct {
	Enable bool
}

// OIDCAuthenticationOptions contains OIDC authentication options for API Server
type OIDCAuthenticationOptions struct {
	CAFile         string
	ClientID       string
	IssuerURL      string
	UsernameClaim  string
	UsernamePrefix string
	GroupsClaim    string
	GroupsPrefix   string
	SigningAlgs    []string
	RequiredClaims map[string]string

	// areFlagsConfigured is a function that returns true if any of the oidc-* flags are configured.
	areFlagsConfigured func() bool
}

// ServiceAccountAuthenticationOptions contains service account authentication options for API Server
type ServiceAccountAuthenticationOptions struct {
	KeyFiles              []string
	Lookup                bool
	Issuers               []string
	JWKSURI               string
	ExtendExpiration      bool
	MaxExpiration         time.Duration
	MaxExtendedExpiration time.Duration
	// OptionalTokenGetter is a function that returns a service account token getter.
	// If not set, the default token getter will be used.
	OptionalTokenGetter func(factory informers.SharedInformerFactory) serviceaccount.ServiceAccountTokenGetter
	// ExternalPublicKeysGetter gets set if `--service-account-signing-endpoint` is passed.
	// ExternalPublicKeysGetter is mutually exclusive with KeyFiles.
	ExternalPublicKeysGetter serviceaccount.PublicKeysGetter
}

// TokenFileAuthenticationOptions contains token file authentication options for API Server
type TokenFileAuthenticationOptions struct {
	TokenFile string
}

// WebHookAuthenticationOptions contains web hook authentication options for API Server
type WebHookAuthenticationOptions struct {
	ConfigFile string
	Version    string
	CacheTTL   time.Duration

	// RetryBackoff specifies the backoff parameters for the authentication webhook retry logic.
	// This allows us to configure the sleep time at each iteration and the maximum number of retries allowed
	// before we fail the webhook call in order to limit the fan out that ensues when the system is degraded.
	RetryBackoff *wait.Backoff
}

// NewBuiltInAuthenticationOptions create a new BuiltInAuthenticationOptions, just set default token cache TTL
func NewBuiltInAuthenticationOptions() *BuiltInAuthenticationOptions {
	return &BuiltInAuthenticationOptions{
		TokenSuccessCacheTTL: 10 * time.Second,
		TokenFailureCacheTTL: 0 * time.Second,
	}
}

// WithAll set default value for every build-in authentication option
func (o *BuiltInAuthenticationOptions) WithAll() *BuiltInAuthenticationOptions {
	return o.
		WithAnonymous().
		WithBootstrapToken().
		WithClientCert().
		WithOIDC().
		WithRequestHeader().
		WithServiceAccounts().
		WithTokenFile().
		WithWebHook()
}

// WithAnonymous set default value for anonymous authentication
func (o *BuiltInAuthenticationOptions) WithAnonymous() *BuiltInAuthenticationOptions {
	o.Anonymous = &AnonymousAuthenticationOptions{
		Allow:       true,
		areFlagsSet: func() bool { return false },
	}
	return o
}

// WithBootstrapToken set default value for bootstrap token authentication
func (o *BuiltInAuthenticationOptions) WithBootstrapToken() *BuiltInAuthenticationOptions {
	o.BootstrapToken = &BootstrapTokenAuthenticationOptions{}
	return o
}

// WithClientCert set default value for client cert
func (o *BuiltInAuthenticationOptions) WithClientCert() *BuiltInAuthenticationOptions {
	o.ClientCert = &genericoptions.ClientCertAuthenticationOptions{}
	return o
}

// WithOIDC set default value for OIDC authentication
func (o *BuiltInAuthenticationOptions) WithOIDC() *BuiltInAuthenticationOptions {
	o.OIDC = &OIDCAuthenticationOptions{
		areFlagsConfigured: func() bool { return false },
		UsernameClaim:      "sub",
		SigningAlgs:        []string{"RS256"},
	}
	return o
}

// WithRequestHeader set default value for request header authentication
func (o *BuiltInAuthenticationOptions) WithRequestHeader() *BuiltInAuthenticationOptions {
	o.RequestHeader = &genericoptions.RequestHeaderAuthenticationOptions{}
	return o
}

// WithServiceAccounts set default value for service account authentication
func (o *BuiltInAuthenticationOptions) WithServiceAccounts() *BuiltInAuthenticationOptions {
	if o.ServiceAccounts == nil {
		o.ServiceAccounts = &ServiceAccountAuthenticationOptions{}
	}
	o.ServiceAccounts.Lookup = true
	o.ServiceAccounts.ExtendExpiration = true
	o.ServiceAccounts.MaxExtendedExpiration = serviceaccount.ExpirationExtensionSeconds * time.Second
	return o
}

// WithTokenFile set default value for token file authentication
func (o *BuiltInAuthenticationOptions) WithTokenFile() *BuiltInAuthenticationOptions {
	o.TokenFile = &TokenFileAuthenticationOptions{}
	return o
}

// WithWebHook set default value for web hook authentication
func (o *BuiltInAuthenticationOptions) WithWebHook() *BuiltInAuthenticationOptions {
	o.WebHook = &WebHookAuthenticationOptions{
		Version:      "v1beta1",
		CacheTTL:     2 * time.Minute,
		RetryBackoff: genericoptions.DefaultAuthWebhookRetryBackoff(),
	}
	return o
}

// Validate checks invalid config combination
func (o *BuiltInAuthenticationOptions) Validate() []error {
	if o == nil {
		return nil
	}

	var allErrors []error

	allErrors = append(allErrors, o.validateOIDCOptions()...)

	if o.ServiceAccounts != nil && len(o.ServiceAccounts.Issuers) > 0 {
		seen := make(map[string]bool)
		for _, issuer := range o.ServiceAccounts.Issuers {
			if strings.Contains(issuer, ":") {
				if _, err := url.Parse(issuer); err != nil {
					allErrors = append(allErrors, fmt.Errorf("service-account-issuer %q contained a ':' but was not a valid URL: %v", issuer, err))
					continue
				}
			}
			if issuer == "" {
				allErrors = append(allErrors, fmt.Errorf("service-account-issuer should not be an empty string"))
				continue
			}
			if seen[issuer] {
				allErrors = append(allErrors, fmt.Errorf("service-account-issuer %q is already specified", issuer))
				continue
			}
			seen[issuer] = true
		}
	}

	if o.ServiceAccounts != nil {
		if len(o.ServiceAccounts.Issuers) == 0 {
			allErrors = append(allErrors, errors.New("service-account-issuer is a required flag"))
		}
		if len(o.ServiceAccounts.KeyFiles) == 0 && o.ServiceAccounts.ExternalPublicKeysGetter == nil {
			allErrors = append(allErrors, errors.New("either `--service-account-key-file` or `--service-account-signing-endpoint` must be set"))
		}

		// Validate the JWKS URI when it is explicitly set.
		// When unset, it is later derived from ExternalHost.
		if o.ServiceAccounts.JWKSURI != "" {
			if u, err := url.Parse(o.ServiceAccounts.JWKSURI); err != nil {
				allErrors = append(allErrors, fmt.Errorf("service-account-jwks-uri must be a valid URL: %v", err))
			} else if u.Scheme != "https" {
				allErrors = append(allErrors, fmt.Errorf("service-account-jwks-uri requires https scheme, parsed as: %v", u.String()))
			}
		}
	}

	// verify that if ServiceAccountTokenNodeBinding is enabled, ServiceAccountTokenNodeBindingValidation is also enabled.
	if utilfeature.DefaultFeatureGate.Enabled(features.ServiceAccountTokenNodeBinding) && !utilfeature.DefaultFeatureGate.Enabled(features.ServiceAccountTokenNodeBindingValidation) {
		allErrors = append(allErrors, fmt.Errorf("the %q feature gate can only be enabled if the %q feature gate is also enabled", features.ServiceAccountTokenNodeBinding, features.ServiceAccountTokenNodeBindingValidation))
	}

	if o.WebHook != nil {
		retryBackoff := o.WebHook.RetryBackoff
		if retryBackoff != nil && retryBackoff.Steps <= 0 {
			allErrors = append(allErrors, fmt.Errorf("number of webhook retry attempts must be greater than 0, but is: %d", retryBackoff.Steps))
		}
	}

	if o.RequestHeader != nil {
		allErrors = append(allErrors, o.RequestHeader.Validate()...)
	}

	return allErrors
}

// AddFlags returns flags of authentication for a API Server
func (o *BuiltInAuthenticationOptions) AddFlags(fs *pflag.FlagSet) {
	if o == nil {
		return
	}

	fs.StringVar(&o.AuthenticationConfigFile, "authentication-config", o.AuthenticationConfigFile, ""+
		"File with Authentication Configuration to configure the JWT Token authenticator or the anonymous authenticator. "+
		"Requires the StructuredAuthenticationConfiguration feature gate. "+
		"Also requires the feature gate AnonymousAuthConfigurableEndpoints to configure the anonymous authenticator in the config file. "+
		"This flag is mutually exclusive with the --oidc-* flags if the file configures the JWT Token authenticator. "+
		"This flag is mutually exclusive with --anonymous-auth if the file configures the Anonymous authenticator.")

	fs.StringSliceVar(&o.APIAudiences, "api-audiences", o.APIAudiences, ""+
		"Identifiers of the API. The service account token authenticator will validate that "+
		"tokens used against the API are bound to at least one of these audiences. If the "+
		"--service-account-issuer flag is configured and this flag is not, this field "+
		"defaults to a single element list containing the issuer URL.")

	if o.Anonymous != nil {
		fs.BoolVar(&o.Anonymous.Allow, "anonymous-auth", o.Anonymous.Allow, ""+
			"Enables anonymous requests to the secure port of the API server. "+
			"Requests that are not rejected by another authentication method are treated as anonymous requests. "+
			"Anonymous requests have a username of system:anonymous, and a group name of system:unauthenticated.")

		o.Anonymous.areFlagsSet = func() bool {
			return fs.Changed("anonymous-auth")
		}
	}

	if o.BootstrapToken != nil {
		fs.BoolVar(&o.BootstrapToken.Enable, "enable-bootstrap-token-auth", o.BootstrapToken.Enable, ""+
			"Enable to allow secrets of type 'bootstrap.kubernetes.io/token' in the 'kube-system' "+
			"namespace to be used for TLS bootstrapping authentication.")
	}

	if o.ClientCert != nil {
		o.ClientCert.AddFlags(fs)
	}

	if o.OIDC != nil {
		fs.StringVar(&o.OIDC.IssuerURL, oidcIssuerURLFlag, o.OIDC.IssuerURL, ""+
			"The URL of the OpenID issuer, only HTTPS scheme will be accepted. "+
			"If set, it will be used to verify the OIDC JSON Web Token (JWT).")

		fs.StringVar(&o.OIDC.ClientID, oidcClientIDFlag, o.OIDC.ClientID, ""+
			"The client ID for the OpenID Connect client, must be set if oidc-issuer-url is set.")

		fs.StringVar(&o.OIDC.CAFile, oidcCAFileFlag, o.OIDC.CAFile, ""+
			"If set, the OpenID server's certificate will be verified by one of the authorities "+
			"in the oidc-ca-file, otherwise the host's root CA set will be used.")

		fs.StringVar(&o.OIDC.UsernameClaim, oidcUsernameClaimFlag, o.OIDC.UsernameClaim, ""+
			"The OpenID claim to use as the user name. Note that claims other than the default ('sub') "+
			"is not guaranteed to be unique and immutable. This flag is experimental, please see "+
			"the authentication documentation for further details.")

		fs.StringVar(&o.OIDC.UsernamePrefix, oidcUsernamePrefixFlag, o.OIDC.UsernamePrefix, ""+
			"If provided, all usernames will be prefixed with this value. If not provided, "+
			"username claims other than 'email' are prefixed by the issuer URL to avoid "+
			"clashes. To skip any prefixing, provide the value '-'.")

		fs.StringVar(&o.OIDC.GroupsClaim, oidcGroupsClaimFlag, o.OIDC.GroupsClaim, ""+
			"If provided, the name of a custom OpenID Connect claim for specifying user groups. "+
			"The claim value is expected to be a string or array of strings. This flag is experimental, "+
			"please see the authentication documentation for further details.")

		fs.StringVar(&o.OIDC.GroupsPrefix, oidcGroupsPrefixFlag, o.OIDC.GroupsPrefix, ""+
			"If provided, all groups will be prefixed with this value to prevent conflicts with "+
			"other authentication strategies.")

		fs.StringSliceVar(&o.OIDC.SigningAlgs, oidcSigningAlgsFlag, o.OIDC.SigningAlgs, ""+
			"Comma-separated list of allowed JOSE asymmetric signing algorithms. JWTs with a "+
			"supported 'alg' header values are: RS256, RS384, RS512, ES256, ES384, ES512, PS256, PS384, PS512. "+
			"Values are defined by RFC 7518 https://tools.ietf.org/html/rfc7518#section-3.1.")

		fs.Var(cliflag.NewMapStringStringNoSplit(&o.OIDC.RequiredClaims), oidcRequiredClaimFlag, ""+
			"A key=value pair that describes a required claim in the ID Token. "+
			"If set, the claim is verified to be present in the ID Token with a matching value. "+
			"Repeat this flag to specify multiple claims.")

		o.OIDC.areFlagsConfigured = func() bool {
			return fs.Changed(oidcIssuerURLFlag) ||
				fs.Changed(oidcClientIDFlag) ||
				fs.Changed(oidcCAFileFlag) ||
				fs.Changed(oidcUsernameClaimFlag) ||
				fs.Changed(oidcUsernamePrefixFlag) ||
				fs.Changed(oidcGroupsClaimFlag) ||
				fs.Changed(oidcGroupsPrefixFlag) ||
				fs.Changed(oidcSigningAlgsFlag) ||
				fs.Changed(oidcRequiredClaimFlag)
		}
	}

	if o.RequestHeader != nil {
		o.RequestHeader.AddFlags(fs)
	}

	if o.ServiceAccounts != nil {
		fs.StringArrayVar(&o.ServiceAccounts.KeyFiles, "service-account-key-file", o.ServiceAccounts.KeyFiles, ""+
			"File containing PEM-encoded x509 RSA or ECDSA private or public keys, used to verify "+
			"ServiceAccount tokens. The specified file can contain multiple keys, and the flag can "+
			"be specified multiple times with different files. If unspecified, "+
			"--tls-private-key-file is used. Must be specified when "+
			"--service-account-signing-key-file is provided")

		fs.BoolVar(&o.ServiceAccounts.Lookup, "service-account-lookup", o.ServiceAccounts.Lookup,
			"If true, validate ServiceAccount tokens exist in etcd as part of authentication.")

		fs.StringArrayVar(&o.ServiceAccounts.Issuers, "service-account-issuer", o.ServiceAccounts.Issuers, ""+
			"Identifier of the service account token issuer. The issuer will assert this identifier "+
			"in \"iss\" claim of issued tokens. This value is a string or URI. If this option is not "+
			"a valid URI per the OpenID Discovery 1.0 spec, the ServiceAccountIssuerDiscovery feature "+
			"will remain disabled, even if the feature gate is set to true. It is highly recommended "+
			"that this value comply with the OpenID spec: https://openid.net/specs/openid-connect-discovery-1_0.html. "+
			"In practice, this means that service-account-issuer must be an https URL. It is also highly "+
			"recommended that this URL be capable of serving OpenID discovery documents at "+
			"{service-account-issuer}/.well-known/openid-configuration. "+
			"When this flag is specified multiple times, the first is used to generate tokens "+
			"and all are used to determine which issuers are accepted.")

		fs.StringVar(&o.ServiceAccounts.JWKSURI, "service-account-jwks-uri", o.ServiceAccounts.JWKSURI, ""+
			"Overrides the URI for the JSON Web Key Set in the discovery doc served at "+
			"/.well-known/openid-configuration. This flag is useful if the discovery doc"+
			"and key set are served to relying parties from a URL other than the "+
			"API server's external (as auto-detected or overridden with external-hostname). ")

		fs.DurationVar(&o.ServiceAccounts.MaxExpiration, "service-account-max-token-expiration", o.ServiceAccounts.MaxExpiration, ""+
			"The maximum validity duration of a token created by the service account token issuer. If an otherwise valid "+
			"TokenRequest with a validity duration larger than this value is requested, a token will be issued with a validity duration of this value.")

		fs.BoolVar(&o.ServiceAccounts.ExtendExpiration, "service-account-extend-token-expiration", o.ServiceAccounts.ExtendExpiration, ""+
			"Turns on projected service account expiration extension during token generation, "+
			"which helps safe transition from legacy token to bound service account token feature. "+
			"If this flag is enabled, admission injected tokens would be extended up to 1 year to "+
			"prevent unexpected failure during transition, ignoring value of service-account-max-token-expiration.")
	}

	if o.TokenFile != nil {
		fs.StringVar(&o.TokenFile.TokenFile, "token-auth-file", o.TokenFile.TokenFile, ""+
			"If set, the file that will be used to secure the secure port of the API server "+
			"via token authentication.")
	}

	if o.WebHook != nil {
		fs.StringVar(&o.WebHook.ConfigFile, "authentication-token-webhook-config-file", o.WebHook.ConfigFile, ""+
			"File with webhook configuration for token authentication in kubeconfig format. "+
			"The API server will query the remote service to determine authentication for bearer tokens.")

		fs.StringVar(&o.WebHook.Version, "authentication-token-webhook-version", o.WebHook.Version, ""+
			"The API version of the authentication.k8s.io TokenReview to send to and expect from the webhook.")

		fs.DurationVar(&o.WebHook.CacheTTL, "authentication-token-webhook-cache-ttl", o.WebHook.CacheTTL,
			"The duration to cache responses from the webhook token authenticator.")
	}
}

// ToAuthenticationConfig convert BuiltInAuthenticationOptions to kubeauthenticator.Config. Returns
// an empty config if o is nil.
func (o *BuiltInAuthenticationOptions) ToAuthenticationConfig() (kubeauthenticator.Config, error) {
	if o == nil {
		return kubeauthenticator.Config{}, nil
	}

	ret := kubeauthenticator.Config{
		TokenSuccessCacheTTL: o.TokenSuccessCacheTTL,
		TokenFailureCacheTTL: o.TokenFailureCacheTTL,
	}

	if o.BootstrapToken != nil {
		ret.BootstrapToken = o.BootstrapToken.Enable
	}

	if o.ClientCert != nil {
		var err error
		ret.ClientCAContentProvider, err = o.ClientCert.GetClientCAContentProvider()
		if err != nil {
			return kubeauthenticator.Config{}, err
		}
	}

	// When the StructuredAuthenticationConfiguration feature is enabled and the authentication config file is provided,
	// load the authentication config from the file, otherwise set up an empty configuration.
	if len(o.AuthenticationConfigFile) > 0 {
		var err error
		if ret.AuthenticationConfig, ret.AuthenticationConfigData, err = loadAuthenticationConfig(o.AuthenticationConfigFile); err != nil {
			return kubeauthenticator.Config{}, err
		}
	} else {
		ret.AuthenticationConfig = &apiserver.AuthenticationConfiguration{}
	}

	// Set up JWT authenticators from config file or from flags
	if len(o.AuthenticationConfigFile) > 0 {
		// all known signing algs are allowed when using authentication config
		// TODO: what we really want to express is 'any alg is fine as long it matches a public key'
		ret.OIDCSigningAlgs = oidc.AllValidSigningAlgorithms()
	} else if o.OIDC != nil && len(o.OIDC.IssuerURL) > 0 && len(o.OIDC.ClientID) > 0 {
		usernamePrefix := o.OIDC.UsernamePrefix

		if o.OIDC.UsernamePrefix == "" && o.OIDC.UsernameClaim != "email" {
			// Legacy CLI flag behavior. If a usernamePrefix isn't provided, prefix all claims other than "email"
			// with the issuerURL.
			//
			// See https://github.com/kubernetes/kubernetes/issues/31380
			usernamePrefix = o.OIDC.IssuerURL + "#"
		}
		if o.OIDC.UsernamePrefix == "-" {
			// Special value indicating usernames shouldn't be prefixed.
			usernamePrefix = ""
		}

		jwtAuthenticator := apiserver.JWTAuthenticator{
			Issuer: apiserver.Issuer{
				URL:       o.OIDC.IssuerURL,
				Audiences: []string{o.OIDC.ClientID},
			},
			ClaimMappings: apiserver.ClaimMappings{
				Username: apiserver.PrefixedClaimOrExpression{
					Prefix: pointer.String(usernamePrefix),
					Claim:  o.OIDC.UsernameClaim,
				},
			},
		}

		if len(o.OIDC.GroupsClaim) > 0 {
			jwtAuthenticator.ClaimMappings.Groups = apiserver.PrefixedClaimOrExpression{
				Prefix: pointer.String(o.OIDC.GroupsPrefix),
				Claim:  o.OIDC.GroupsClaim,
			}
		}

		if len(o.OIDC.CAFile) != 0 {
			caContent, err := os.ReadFile(o.OIDC.CAFile)
			if err != nil {
				return kubeauthenticator.Config{}, err
			}
			jwtAuthenticator.Issuer.CertificateAuthority = string(caContent)
		}

		if len(o.OIDC.RequiredClaims) > 0 {
			claimValidationRules := make([]apiserver.ClaimValidationRule, 0, len(o.OIDC.RequiredClaims))
			for claim, value := range o.OIDC.RequiredClaims {
				claimValidationRules = append(claimValidationRules, apiserver.ClaimValidationRule{
					Claim:         claim,
					RequiredValue: value,
				})
			}
			jwtAuthenticator.ClaimValidationRules = claimValidationRules
		}

		ret.AuthenticationConfig.JWT = []apiserver.JWTAuthenticator{jwtAuthenticator}

		ret.OIDCSigningAlgs = o.OIDC.SigningAlgs
	}

	// Set up anonymous authenticator from config file or flags
	if o.Anonymous != nil {
		switch {
		case ret.AuthenticationConfig.Anonymous != nil && o.Anonymous.areFlagsSet():
			// Flags and config file are mutually exclusive
			return kubeauthenticator.Config{}, field.Forbidden(field.NewPath("anonymous"), "--anonynous-auth flag cannot be set when anonymous field is configured in authentication configuration file")
		case ret.AuthenticationConfig.Anonymous != nil:
			// Use the config-file-specified values
			ret.Anonymous = *ret.AuthenticationConfig.Anonymous
		default:
			// Use the flag-specified values
			ret.Anonymous = apiserver.AnonymousAuthConfig{Enabled: o.Anonymous.Allow}
		}
	}

	if err := apiservervalidation.ValidateAuthenticationConfiguration(authenticationcel.NewDefaultCompiler(), ret.AuthenticationConfig, ret.ServiceAccountIssuers).ToAggregate(); err != nil {
		return kubeauthenticator.Config{}, err
	}

	if o.RequestHeader != nil {
		var err error
		ret.RequestHeaderConfig, err = o.RequestHeader.ToAuthenticationRequestHeaderConfig()
		if err != nil {
			return kubeauthenticator.Config{}, err
		}
	}

	ret.APIAudiences = o.APIAudiences
	if o.ServiceAccounts != nil {
		if len(o.ServiceAccounts.Issuers) != 0 && len(o.APIAudiences) == 0 {
			ret.APIAudiences = authenticator.Audiences(o.ServiceAccounts.Issuers)
		}

		switch {
		case len(o.ServiceAccounts.KeyFiles) > 0 && o.ServiceAccounts.ExternalPublicKeysGetter != nil:
			return kubeauthenticator.Config{}, fmt.Errorf("cannot set mutually exclusive flags `--service-account-key-file` and `--service-account-signing-endpoint` at the same time")
		case len(o.ServiceAccounts.KeyFiles) > 0:
			allPublicKeys := []interface{}{}
			for _, keyfile := range o.ServiceAccounts.KeyFiles {
				publicKeys, err := keyutil.PublicKeysFromFile(keyfile)
				if err != nil {
					return kubeauthenticator.Config{}, err
				}
				allPublicKeys = append(allPublicKeys, publicKeys...)
			}
			keysGetter, err := serviceaccount.StaticPublicKeysGetter(allPublicKeys)
			if err != nil {
				return kubeauthenticator.Config{}, fmt.Errorf("failed to set up public service account keys: %w", err)
			}
			ret.ServiceAccountPublicKeysGetter = keysGetter
		case o.ServiceAccounts.ExternalPublicKeysGetter != nil:
			ret.ServiceAccountPublicKeysGetter = o.ServiceAccounts.ExternalPublicKeysGetter
		}

		ret.ServiceAccountIssuers = o.ServiceAccounts.Issuers
		ret.ServiceAccountLookup = o.ServiceAccounts.Lookup
	}

	if o.TokenFile != nil {
		ret.TokenAuthFile = o.TokenFile.TokenFile
	}

	if o.WebHook != nil {
		ret.WebhookTokenAuthnConfigFile = o.WebHook.ConfigFile
		ret.WebhookTokenAuthnVersion = o.WebHook.Version
		ret.WebhookTokenAuthnCacheTTL = o.WebHook.CacheTTL
		ret.WebhookRetryBackoff = o.WebHook.RetryBackoff

		if len(o.WebHook.ConfigFile) > 0 && o.WebHook.CacheTTL > 0 {
			if o.TokenSuccessCacheTTL > 0 && o.WebHook.CacheTTL < o.TokenSuccessCacheTTL {
				klog.Warningf("the webhook cache ttl of %s is shorter than the overall cache ttl of %s for successful token authentication attempts.", o.WebHook.CacheTTL, o.TokenSuccessCacheTTL)
			}
			if o.TokenFailureCacheTTL > 0 && o.WebHook.CacheTTL < o.TokenFailureCacheTTL {
				klog.Warningf("the webhook cache ttl of %s is shorter than the overall cache ttl of %s for failed token authentication attempts.", o.WebHook.CacheTTL, o.TokenFailureCacheTTL)
			}
		}
	}

	return ret, nil
}

// ApplyTo requires already applied OpenAPIConfig and EgressSelector if present.
// The input context controls the lifecycle of background goroutines started to reload the authentication config file.
func (o *BuiltInAuthenticationOptions) ApplyTo(
	ctx context.Context,
	authInfo *genericapiserver.AuthenticationInfo,
	secureServing *genericapiserver.SecureServingInfo,
	egressSelector *egressselector.EgressSelector,
	openAPIConfig *openapicommon.Config,
	openAPIV3Config *openapicommon.OpenAPIV3Config,
	extclient kubernetes.Interface,
	versionedInformer informers.SharedInformerFactory,
	apiServerID string) error {
	if o == nil {
		return nil
	}

	if openAPIConfig == nil {
		return errors.New("uninitialized OpenAPIConfig")
	}

	authenticatorConfig, err := o.ToAuthenticationConfig()
	if err != nil {
		return err
	}

	if authenticatorConfig.ClientCAContentProvider != nil {
		if err = authInfo.ApplyClientCert(authenticatorConfig.ClientCAContentProvider, secureServing); err != nil {
			return fmt.Errorf("unable to load client CA file: %v", err)
		}
	}
	if authenticatorConfig.RequestHeaderConfig != nil && authenticatorConfig.RequestHeaderConfig.CAContentProvider != nil {
		if err = authInfo.ApplyClientCert(authenticatorConfig.RequestHeaderConfig.CAContentProvider, secureServing); err != nil {
			return fmt.Errorf("unable to load client CA file: %v", err)
		}
	}

	authInfo.RequestHeaderConfig = authenticatorConfig.RequestHeaderConfig
	authInfo.APIAudiences = o.APIAudiences
	if o.ServiceAccounts != nil && len(o.ServiceAccounts.Issuers) != 0 && len(o.APIAudiences) == 0 {
		authInfo.APIAudiences = authenticator.Audiences(o.ServiceAccounts.Issuers)
	}

	// If the optional token getter function is set, use it. Otherwise, use the default token getter.
	if o.ServiceAccounts != nil && o.ServiceAccounts.OptionalTokenGetter != nil {
		authenticatorConfig.ServiceAccountTokenGetter = o.ServiceAccounts.OptionalTokenGetter(versionedInformer)
	} else {
		var nodeLister v1listers.NodeLister
		if utilfeature.DefaultFeatureGate.Enabled(features.ServiceAccountTokenNodeBindingValidation) {
			nodeLister = versionedInformer.Core().V1().Nodes().Lister()
		}

		authenticatorConfig.ServiceAccountTokenGetter = serviceaccountcontroller.NewGetterFromClient(
			extclient,
			versionedInformer.Core().V1().Secrets().Lister(),
			versionedInformer.Core().V1().ServiceAccounts().Lister(),
			versionedInformer.Core().V1().Pods().Lister(),
			nodeLister,
		)
	}
	authenticatorConfig.SecretsWriter = extclient.CoreV1()

	if authenticatorConfig.BootstrapToken {
		authenticatorConfig.BootstrapTokenAuthenticator = bootstrap.NewTokenAuthenticator(
			versionedInformer.Core().V1().Secrets().Lister().Secrets(metav1.NamespaceSystem),
		)
	}

	if egressSelector != nil {
		egressDialer, err := egressSelector.Lookup(egressselector.ControlPlane.AsNetworkContext())
		if err != nil {
			return err
		}
		authenticatorConfig.CustomDial = egressDialer
	}

	authenticatorConfig.APIServerID = apiServerID
	// var openAPIV3SecuritySchemes spec3.SecuritySchemes
	authenticator, updateAuthenticationConfig, openAPIV2SecurityDefinitions, openAPIV3SecuritySchemes, err := authenticatorConfig.New(ctx)
	if err != nil {
		return err
	}
	authInfo.Authenticator = authenticator

	if len(o.AuthenticationConfigFile) > 0 {
		authenticationconfigmetrics.RegisterMetrics()
		trackedAuthenticationConfigData := authenticatorConfig.AuthenticationConfigData
		var mu sync.Mutex

		// ensure anonymous config doesn't change on reload
		originalFileAnonymousConfig := authenticatorConfig.AuthenticationConfig.DeepCopy().Anonymous

		go filesystem.WatchUntil(
			ctx,
			time.Minute,
			o.AuthenticationConfigFile,
			func() {
				// TODO collapse onto shared logic with DynamicEncryptionConfigContent controller

				mu.Lock()
				defer mu.Unlock()

				authConfigBytes, err := os.ReadFile(o.AuthenticationConfigFile)
				if err != nil {
					klog.ErrorS(err, "failed to read authentication config file")
					authenticationconfigmetrics.RecordAuthenticationConfigAutomaticReloadFailure(apiServerID)
					// we do not update the tracker here because this error could eventually resolve as we keep retrying
					return
				}

				authConfigData := string(authConfigBytes)

				if authConfigData == trackedAuthenticationConfigData {
					return
				}

				authConfig, err := loadAuthenticationConfigFromData(authConfigBytes)
				if err != nil {
					klog.ErrorS(err, "failed to load authentication config")
					authenticationconfigmetrics.RecordAuthenticationConfigAutomaticReloadFailure(apiServerID)
					// this config is not structurally valid and never will be, update the tracker so we stop retrying
					trackedAuthenticationConfigData = authConfigData
					return
				}

				validationErrs := apiservervalidation.ValidateAuthenticationConfiguration(authenticationcel.NewDefaultCompiler(), authConfig, authenticatorConfig.ServiceAccountIssuers)
				if !reflect.DeepEqual(originalFileAnonymousConfig, authConfig.Anonymous) {
					validationErrs = append(validationErrs, field.Forbidden(field.NewPath("anonymous"), "changed from initial configuration file"))
				}
				if err := validationErrs.ToAggregate(); err != nil {
					klog.ErrorS(err, "failed to validate authentication config")
					authenticationconfigmetrics.RecordAuthenticationConfigAutomaticReloadFailure(apiServerID)
					// this config is not semantically valid and never will be, update the tracker so we stop retrying
					trackedAuthenticationConfigData = authConfigData
					return
				}

				timeoutCtx, timeoutCancel := context.WithTimeout(ctx, UpdateAuthenticationConfigTimeout)
				defer timeoutCancel()
				if err := updateAuthenticationConfig(timeoutCtx, authConfig); err != nil {
					klog.ErrorS(err, "failed to update authentication config")
					authenticationconfigmetrics.RecordAuthenticationConfigAutomaticReloadFailure(apiServerID)
					// we do not update the tracker here because this error could eventually resolve as we keep retrying
					return
				}

				trackedAuthenticationConfigData = authConfigData
				klog.InfoS("reloaded authentication config")
				authenticationconfigmetrics.RecordAuthenticationConfigAutomaticReloadSuccess(apiServerID)
			},
			func(err error) { klog.ErrorS(err, "watching authentication config file") },
		)
	}

	openAPIConfig.SecurityDefinitions = openAPIV2SecurityDefinitions
	if openAPIV3Config != nil {
		openAPIV3Config.SecuritySchemes = openAPIV3SecuritySchemes
	}
	return nil
}

// ApplyAuthorization will conditionally modify the authentication options based on the authorization options
func (o *BuiltInAuthenticationOptions) ApplyAuthorization(authorization *BuiltInAuthorizationOptions) {
	if o == nil || authorization == nil || o.Anonymous == nil {
		return
	}

	// authorization ModeAlwaysAllow cannot be combined with AnonymousAuth.
	// in such a case the AnonymousAuth is stomped to false and you get a message
	if o.Anonymous.Allow && sets.NewString(authorization.Modes...).Has(authzmodes.ModeAlwaysAllow) {
		klog.Warningf("AnonymousAuth is not allowed with the AlwaysAllow authorizer. Resetting AnonymousAuth to false. You should use a different authorizer")
		o.Anonymous.Allow = false
	}
}

func (o *BuiltInAuthenticationOptions) validateOIDCOptions() []error {
	var allErrors []error

	// Existing validation when jwt authenticator is configured with oidc-* flags
	if len(o.AuthenticationConfigFile) == 0 {
		if o.OIDC != nil && o.OIDC.areFlagsConfigured() && (len(o.OIDC.IssuerURL) == 0 || len(o.OIDC.ClientID) == 0) {
			allErrors = append(allErrors, fmt.Errorf("oidc-issuer-url and oidc-client-id must be specified together when any oidc-* flags are set"))
		}

		return allErrors
	}

	// New validation when authentication config file is provided

	// Authentication config file is only supported when the StructuredAuthenticationConfiguration feature is enabled
	if !utilfeature.DefaultFeatureGate.Enabled(genericfeatures.StructuredAuthenticationConfiguration) {
		allErrors = append(allErrors, fmt.Errorf("set --feature-gates=%s=true to use authentication-config file", genericfeatures.StructuredAuthenticationConfiguration))
	}

	// Authentication config file and oidc-* flags are mutually exclusive
	if o.OIDC != nil && o.OIDC.areFlagsConfigured() {
		allErrors = append(allErrors, fmt.Errorf("authentication-config file and oidc-* flags are mutually exclusive"))
	}

	return allErrors
}

var (
	cfgScheme = runtime.NewScheme()
	codecs    = serializer.NewCodecFactory(cfgScheme, serializer.EnableStrict)
)

func init() {
	install.Install(cfgScheme)
}

// loadAuthenticationConfig parses the authentication configuration from the given file and returns it and the file's contents.
func loadAuthenticationConfig(configFilePath string) (*apiserver.AuthenticationConfiguration, string, error) {
	data, err := os.ReadFile(configFilePath)
	if err != nil {
		return nil, "", err
	}

	configuration, err := loadAuthenticationConfigFromData(data)
	if err != nil {
		return nil, "", err
	}

	return configuration, string(data), nil
}

func loadAuthenticationConfigFromData(data []byte) (*apiserver.AuthenticationConfiguration, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("empty config data")
	}

	decodedObj, err := runtime.Decode(codecs.UniversalDecoder(), data)
	if err != nil {
		return nil, err
	}
	configuration, ok := decodedObj.(*apiserver.AuthenticationConfiguration)
	if !ok {
		return nil, fmt.Errorf("expected AuthenticationConfiguration, got %T", decodedObj)
	}
	if configuration == nil { // sanity check, this should never happen but check just in case since we rely on it
		return nil, fmt.Errorf("expected non-nil AuthenticationConfiguration")
	}

	return configuration, nil
}
