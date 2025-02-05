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
	"fmt"
	"net"
	"regexp"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/util/errors"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apiserver/pkg/server"
	"k8s.io/apiserver/pkg/util/compatibility"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	basecompatibility "k8s.io/component-base/compatibility"

	"github.com/spf13/pflag"
)

const (
	corsAllowedOriginsHelpText = "List of allowed origins for CORS, comma separated. " +
		"An allowed origin can be a regular expression to support subdomain matching. " +
		"If this list is empty CORS will not be enabled. " +
		"Please ensure each expression matches the entire hostname by anchoring " +
		"to the start with '^' or including the '//' prefix, and by anchoring to the " +
		"end with '$' or including the ':' port separator suffix. " +
		"Examples of valid expressions are '//example\\.com(:|$)' and '^https://example\\.com(:|$)'"
)

// ServerRunOptions contains the options while running a generic api server.
type ServerRunOptions struct {
	AdvertiseAddress net.IP

	CorsAllowedOriginList        []string
	HSTSDirectives               []string
	ExternalHost                 string
	MaxRequestsInFlight          int
	MaxMutatingRequestsInFlight  int
	RequestTimeout               time.Duration
	GoawayChance                 float64
	LivezGracePeriod             time.Duration
	MinRequestTimeout            int
	StorageInitializationTimeout time.Duration
	ShutdownDelayDuration        time.Duration
	// We intentionally did not add a flag for this option. Users of the
	// apiserver library can wire it to a flag.
	JSONPatchMaxCopyBytes int64
	// The limit on the request body size that would be accepted and
	// decoded in a write request. 0 means no limit.
	// We intentionally did not add a flag for this option. Users of the
	// apiserver library can wire it to a flag.
	MaxRequestBodyBytes int64

	// ShutdownSendRetryAfter dictates when to initiate shutdown of the HTTP
	// Server during the graceful termination of the apiserver. If true, we wait
	// for non longrunning requests in flight to be drained and then initiate a
	// shutdown of the HTTP Server. If false, we initiate a shutdown of the HTTP
	// Server as soon as ShutdownDelayDuration has elapsed.
	// If enabled, after ShutdownDelayDuration elapses, any incoming request is
	// rejected with a 429 status code and a 'Retry-After' response.
	ShutdownSendRetryAfter bool

	// ShutdownWatchTerminationGracePeriod, if set to a positive value,
	// is the maximum duration the apiserver will wait for all active
	// watch request(s) to drain.
	// Once this grace period elapses, the apiserver will no longer
	// wait for any active watch request(s) in flight to drain, it will
	// proceed to the next step in the graceful server shutdown process.
	// If set to a positive value, the apiserver will keep track of the
	// number of active watch request(s) in flight and during shutdown
	// it will wait, at most, for the specified duration and allow these
	// active watch requests to drain with some rate limiting in effect.
	// The default is zero, which implies the apiserver will not keep
	// track of active watch request(s) in flight and will not wait
	// for them to drain, this maintains backward compatibility.
	// This grace period is orthogonal to other grace periods, and
	// it is not overridden by any other grace period.
	ShutdownWatchTerminationGracePeriod time.Duration

	// ComponentGlobalsRegistry is the registry where the effective versions and feature gates for all components are stored.
	ComponentGlobalsRegistry basecompatibility.ComponentGlobalsRegistry
	// ComponentName is name under which the server's global variabled are registered in the ComponentGlobalsRegistry.
	ComponentName string
}

func NewServerRunOptions() *ServerRunOptions {
	if compatibility.DefaultComponentGlobalsRegistry.EffectiveVersionFor(basecompatibility.DefaultKubeComponent) == nil {
		featureGate := utilfeature.DefaultMutableFeatureGate
		effectiveVersion := compatibility.DefaultBuildEffectiveVersion()
		utilruntime.Must(compatibility.DefaultComponentGlobalsRegistry.Register(basecompatibility.DefaultKubeComponent, effectiveVersion, featureGate))
	}

	return NewServerRunOptionsForComponent(basecompatibility.DefaultKubeComponent, compatibility.DefaultComponentGlobalsRegistry)
}

func NewServerRunOptionsForComponent(componentName string, componentGlobalsRegistry basecompatibility.ComponentGlobalsRegistry) *ServerRunOptions {
	defaults := server.NewConfig(serializer.CodecFactory{})
	return &ServerRunOptions{
		MaxRequestsInFlight:                 defaults.MaxRequestsInFlight,
		MaxMutatingRequestsInFlight:         defaults.MaxMutatingRequestsInFlight,
		RequestTimeout:                      defaults.RequestTimeout,
		LivezGracePeriod:                    defaults.LivezGracePeriod,
		MinRequestTimeout:                   defaults.MinRequestTimeout,
		StorageInitializationTimeout:        defaults.StorageInitializationTimeout,
		ShutdownDelayDuration:               defaults.ShutdownDelayDuration,
		ShutdownWatchTerminationGracePeriod: defaults.ShutdownWatchTerminationGracePeriod,
		JSONPatchMaxCopyBytes:               defaults.JSONPatchMaxCopyBytes,
		MaxRequestBodyBytes:                 defaults.MaxRequestBodyBytes,
		ShutdownSendRetryAfter:              false,
		ComponentName:                       componentName,
		ComponentGlobalsRegistry:            componentGlobalsRegistry,
	}
}

// ApplyTo applies the run options to the method receiver and returns self
func (s *ServerRunOptions) ApplyTo(c *server.Config) error {
	if err := s.ComponentGlobalsRegistry.SetFallback(); err != nil {
		return err
	}
	c.CorsAllowedOriginList = s.CorsAllowedOriginList
	c.HSTSDirectives = s.HSTSDirectives
	c.ExternalAddress = s.ExternalHost
	c.MaxRequestsInFlight = s.MaxRequestsInFlight
	c.MaxMutatingRequestsInFlight = s.MaxMutatingRequestsInFlight
	c.LivezGracePeriod = s.LivezGracePeriod
	c.RequestTimeout = s.RequestTimeout
	c.GoawayChance = s.GoawayChance
	c.MinRequestTimeout = s.MinRequestTimeout
	c.StorageInitializationTimeout = s.StorageInitializationTimeout
	c.ShutdownDelayDuration = s.ShutdownDelayDuration
	c.JSONPatchMaxCopyBytes = s.JSONPatchMaxCopyBytes
	c.MaxRequestBodyBytes = s.MaxRequestBodyBytes
	c.PublicAddress = s.AdvertiseAddress
	c.ShutdownSendRetryAfter = s.ShutdownSendRetryAfter
	c.ShutdownWatchTerminationGracePeriod = s.ShutdownWatchTerminationGracePeriod
	c.EffectiveVersion = s.ComponentGlobalsRegistry.EffectiveVersionFor(s.ComponentName)
	c.FeatureGate = s.ComponentGlobalsRegistry.FeatureGateFor(s.ComponentName)

	return nil
}

// DefaultAdvertiseAddress sets the field AdvertiseAddress if unset. The field will be set based on the SecureServingOptions.
func (s *ServerRunOptions) DefaultAdvertiseAddress(secure *SecureServingOptions) error {
	if secure == nil {
		return nil
	}

	if s.AdvertiseAddress == nil || s.AdvertiseAddress.IsUnspecified() {
		hostIP, err := secure.DefaultExternalAddress()
		if err != nil {
			return fmt.Errorf("Unable to find suitable network address.error='%v'. "+
				"Try to set the AdvertiseAddress directly or provide a valid BindAddress to fix this.", err)
		}
		s.AdvertiseAddress = hostIP
	}

	return nil
}

// Validate checks validation of ServerRunOptions
func (s *ServerRunOptions) Validate() []error {
	errors := []error{}

	if s.LivezGracePeriod < 0 {
		errors = append(errors, fmt.Errorf("--livez-grace-period can not be a negative value"))
	}

	if s.MaxRequestsInFlight < 0 {
		errors = append(errors, fmt.Errorf("--max-requests-inflight can not be negative value"))
	}
	if s.MaxMutatingRequestsInFlight < 0 {
		errors = append(errors, fmt.Errorf("--max-mutating-requests-inflight can not be negative value"))
	}

	if s.RequestTimeout.Nanoseconds() < 0 {
		errors = append(errors, fmt.Errorf("--request-timeout can not be negative value"))
	}

	if s.GoawayChance < 0 || s.GoawayChance > 0.02 {
		errors = append(errors, fmt.Errorf("--goaway-chance can not be less than 0 or greater than 0.02"))
	}

	if s.MinRequestTimeout < 0 {
		errors = append(errors, fmt.Errorf("--min-request-timeout can not be negative value"))
	}

	if s.StorageInitializationTimeout < 0 {
		errors = append(errors, fmt.Errorf("--storage-initialization-timeout can not be negative value"))
	}

	if s.ShutdownDelayDuration < 0 {
		errors = append(errors, fmt.Errorf("--shutdown-delay-duration can not be negative value"))
	}

	if s.ShutdownWatchTerminationGracePeriod < 0 {
		errors = append(errors, fmt.Errorf("shutdown-watch-termination-grace-period, if provided, can not be a negative value"))
	}

	if s.JSONPatchMaxCopyBytes < 0 {
		errors = append(errors, fmt.Errorf("ServerRunOptions.JSONPatchMaxCopyBytes can not be negative value"))
	}

	if s.MaxRequestBodyBytes < 0 {
		errors = append(errors, fmt.Errorf("ServerRunOptions.MaxRequestBodyBytes can not be negative value"))
	}

	if err := validateHSTSDirectives(s.HSTSDirectives); err != nil {
		errors = append(errors, err)
	}

	if err := validateCorsAllowedOriginList(s.CorsAllowedOriginList); err != nil {
		errors = append(errors, err)
	}
	if errs := s.ComponentGlobalsRegistry.Validate(); len(errs) != 0 {
		errors = append(errors, errs...)
	}
	return errors
}

func validateHSTSDirectives(hstsDirectives []string) error {
	// HSTS Headers format: Strict-Transport-Security:max-age=expireTime [;includeSubDomains] [;preload]
	// See https://tools.ietf.org/html/rfc6797#section-6.1 for more information
	allErrors := []error{}
	for _, hstsDirective := range hstsDirectives {
		if len(strings.TrimSpace(hstsDirective)) == 0 {
			allErrors = append(allErrors, fmt.Errorf("empty value in strict-transport-security-directives"))
			continue
		}
		if hstsDirective != "includeSubDomains" && hstsDirective != "preload" {
			maxAgeDirective := strings.Split(hstsDirective, "=")
			if len(maxAgeDirective) != 2 || maxAgeDirective[0] != "max-age" {
				allErrors = append(allErrors, fmt.Errorf("--strict-transport-security-directives invalid, allowed values: max-age=expireTime, includeSubDomains, preload. see https://tools.ietf.org/html/rfc6797#section-6.1 for more information"))
			}
		}
	}
	return errors.NewAggregate(allErrors)
}

func validateCorsAllowedOriginList(corsAllowedOriginList []string) error {
	allErrors := []error{}
	validateRegexFn := func(regexpStr string) error {
		if _, err := regexp.Compile(regexpStr); err != nil {
			return err
		}

		// the regular expression should pin to the start and end of the host
		// in the origin header, this will prevent CVE-2022-1996.
		// possible ways it can pin to the start of host in the origin header:
		//   - match the start of the origin with '^'
		//   - match what separates the scheme and host with '//' or '://',
		//     this pins to the start of host in the origin header.
		// possible ways it can match the end of the host in the origin header:
		//   - match the end of the origin with '$'
		//   - with a capture group that matches the host and port separator '(:|$)'
		// We will relax the validation to check if these regex markers
		// are present in the user specified expression.
		var pinStart, pinEnd bool
		for _, prefix := range []string{"^", "//"} {
			if strings.Contains(regexpStr, prefix) {
				pinStart = true
				break
			}
		}
		for _, suffix := range []string{"$", ":"} {
			if strings.Contains(regexpStr, suffix) {
				pinEnd = true
				break
			}
		}
		if !pinStart || !pinEnd {
			return fmt.Errorf("regular expression does not pin to start/end of host in the origin header")
		}
		return nil
	}

	for _, regexp := range corsAllowedOriginList {
		if len(regexp) == 0 {
			allErrors = append(allErrors, fmt.Errorf("empty value in --cors-allowed-origins, help: %s", corsAllowedOriginsHelpText))
			continue
		}

		if err := validateRegexFn(regexp); err != nil {
			err = fmt.Errorf("--cors-allowed-origins has an invalid regular expression: %v, help: %s", err, corsAllowedOriginsHelpText)
			allErrors = append(allErrors, err)
		}
	}
	return errors.NewAggregate(allErrors)
}

// AddUniversalFlags adds flags for a specific APIServer to the specified FlagSet
func (s *ServerRunOptions) AddUniversalFlags(fs *pflag.FlagSet) {
	// Note: the weird ""+ in below lines seems to be the only way to get gofmt to
	// arrange these text blocks sensibly. Grrr.

	fs.IPVar(&s.AdvertiseAddress, "advertise-address", s.AdvertiseAddress, ""+
		"The IP address on which to advertise the apiserver to members of the cluster. This "+
		"address must be reachable by the rest of the cluster. If blank, the --bind-address "+
		"will be used. If --bind-address is unspecified, the host's default interface will "+
		"be used.")

	fs.StringSliceVar(&s.CorsAllowedOriginList, "cors-allowed-origins", s.CorsAllowedOriginList, corsAllowedOriginsHelpText)

	fs.StringSliceVar(&s.HSTSDirectives, "strict-transport-security-directives", s.HSTSDirectives, ""+
		"List of directives for HSTS, comma separated. If this list is empty, then HSTS directives will not "+
		"be added. Example: 'max-age=31536000,includeSubDomains,preload'")

	fs.StringVar(&s.ExternalHost, "external-hostname", s.ExternalHost,
		"The hostname to use when generating externalized URLs for this master (e.g. Swagger API Docs or OpenID Discovery).")

	fs.IntVar(&s.MaxRequestsInFlight, "max-requests-inflight", s.MaxRequestsInFlight, ""+
		"This and --max-mutating-requests-inflight are summed to determine the server's total concurrency limit "+
		"(which must be positive) if --enable-priority-and-fairness is true. "+
		"Otherwise, this flag limits the maximum number of non-mutating requests in flight, "+
		"or a zero value disables the limit completely.")

	fs.IntVar(&s.MaxMutatingRequestsInFlight, "max-mutating-requests-inflight", s.MaxMutatingRequestsInFlight, ""+
		"This and --max-requests-inflight are summed to determine the server's total concurrency limit "+
		"(which must be positive) if --enable-priority-and-fairness is true. "+
		"Otherwise, this flag limits the maximum number of mutating requests in flight, "+
		"or a zero value disables the limit completely.")

	fs.DurationVar(&s.RequestTimeout, "request-timeout", s.RequestTimeout, ""+
		"An optional field indicating the duration a handler must keep a request open before timing "+
		"it out. This is the default request timeout for requests but may be overridden by flags such as "+
		"--min-request-timeout for specific types of requests.")

	fs.Float64Var(&s.GoawayChance, "goaway-chance", s.GoawayChance, ""+
		"To prevent HTTP/2 clients from getting stuck on a single apiserver, randomly close a connection (GOAWAY). "+
		"The client's other in-flight requests won't be affected, and the client will reconnect, likely landing on a different apiserver after going through the load balancer again. "+
		"This argument sets the fraction of requests that will be sent a GOAWAY. Clusters with single apiservers, or which don't use a load balancer, should NOT enable this. "+
		"Min is 0 (off), Max is .02 (1/50 requests); .001 (1/1000) is a recommended starting point.")

	fs.DurationVar(&s.LivezGracePeriod, "livez-grace-period", s.LivezGracePeriod, ""+
		"This option represents the maximum amount of time it should take for apiserver to complete its startup sequence "+
		"and become live. From apiserver's start time to when this amount of time has elapsed, /livez will assume "+
		"that unfinished post-start hooks will complete successfully and therefore return true.")

	fs.IntVar(&s.MinRequestTimeout, "min-request-timeout", s.MinRequestTimeout, ""+
		"An optional field indicating the minimum number of seconds a handler must keep "+
		"a request open before timing it out. Currently only honored by the watch request "+
		"handler, which picks a randomized value above this number as the connection timeout, "+
		"to spread out load.")

	fs.DurationVar(&s.StorageInitializationTimeout, "storage-initialization-timeout", s.StorageInitializationTimeout,
		"Maximum amount of time to wait for storage initialization before declaring apiserver ready. Defaults to 1m.")

	fs.DurationVar(&s.ShutdownDelayDuration, "shutdown-delay-duration", s.ShutdownDelayDuration, ""+
		"Time to delay the termination. During that time the server keeps serving requests normally. The endpoints /healthz and /livez "+
		"will return success, but /readyz immediately returns failure. Graceful termination starts after this delay "+
		"has elapsed. This can be used to allow load balancer to stop sending traffic to this server.")

	fs.BoolVar(&s.ShutdownSendRetryAfter, "shutdown-send-retry-after", s.ShutdownSendRetryAfter, ""+
		"If true the HTTP Server will continue listening until all non long running request(s) in flight have been drained, "+
		"during this window all incoming requests will be rejected with a status code 429 and a 'Retry-After' response header, "+
		"in addition 'Connection: close' response header is set in order to tear down the TCP connection when idle.")

	fs.DurationVar(&s.ShutdownWatchTerminationGracePeriod, "shutdown-watch-termination-grace-period", s.ShutdownWatchTerminationGracePeriod, ""+
		"This option, if set, represents the maximum amount of grace period the apiserver will wait "+
		"for active watch request(s) to drain during the graceful server shutdown window.")

	s.ComponentGlobalsRegistry.AddFlags(fs)
}

// Complete fills missing fields with defaults.
func (s *ServerRunOptions) Complete() error {
	return s.ComponentGlobalsRegistry.SetFallback()
}
