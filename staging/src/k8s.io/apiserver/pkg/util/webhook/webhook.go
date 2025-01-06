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

// Package webhook implements a generic HTTP webhook plugin.
package webhook

import (
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	utilnet "k8s.io/apimachinery/pkg/util/net"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apiserver/pkg/util/x509metrics"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// defaultRequestTimeout is set for all webhook request. This is the absolute
// timeout of the HTTP request, including reading the response body.
const defaultRequestTimeout = 30 * time.Second

// DefaultRetryBackoffWithInitialDelay returns the default backoff parameters for webhook retry from a given initial delay.
// Handy for the client that provides a custom initial delay only.
func DefaultRetryBackoffWithInitialDelay(initialBackoffDelay time.Duration) wait.Backoff {
	return wait.Backoff{
		Duration: initialBackoffDelay,
		Factor:   1.5,
		Jitter:   0.2,
		Steps:    5,
	}
}

// GenericWebhook defines a generic client for webhooks with commonly used capabilities,
// such as retry requests.
type GenericWebhook struct {
	RestClient   *rest.RESTClient
	RetryBackoff wait.Backoff
	ShouldRetry  func(error) bool
}

// DefaultShouldRetry is a default implementation for the GenericWebhook ShouldRetry function property.
//
// It is a suitable retry policy for interactive requests.
func DefaultShouldRetry(err error) bool {
	// these errors indicate a transient error that should be retried.
	switch {
	case utilnet.IsConnectionReset(err):
		return true
	case utilnet.IsHTTP2ConnectionLost(err):
		return true
	case apierrors.IsInternalError(err):
		return true
	case apierrors.IsTimeout(err):
		return true
	case apierrors.IsTooManyRequests(err):
		return true
	case apierrors.IsServiceUnavailable(err):
		return true
	}
	// if the error sends the Retry-After header, we respect it as an explicit confirmation we should retry.
	if _, shouldRetry := apierrors.SuggestsClientDelay(err); shouldRetry {
		return true
	}
	return false
}

// NewGenericWebhook creates a new GenericWebhook from the provided rest.Config.
func NewGenericWebhook(scheme *runtime.Scheme, codecFactory serializer.CodecFactory, config *rest.Config, groupVersions []schema.GroupVersion, retryBackoff wait.Backoff) (*GenericWebhook, error) {
	for _, groupVersion := range groupVersions {
		if !scheme.IsVersionRegistered(groupVersion) {
			return nil, fmt.Errorf("webhook plugin requires enabling extension resource: %s", groupVersion)
		}
	}

	clientConfig := rest.CopyConfig(config)

	codec := codecFactory.LegacyCodec(groupVersions...)
	clientConfig.ContentConfig.NegotiatedSerializer = serializer.NegotiatedSerializerWrapper(runtime.SerializerInfo{Serializer: codec})

	clientConfig.Wrap(x509metrics.NewDeprecatedCertificateRoundTripperWrapperConstructor(
		x509MissingSANCounter,
		x509InsecureSHA1Counter,
	))

	restClient, err := rest.UnversionedRESTClientFor(clientConfig)
	if err != nil {
		return nil, err
	}

	return &GenericWebhook{restClient, retryBackoff, DefaultShouldRetry}, nil
}

// WithExponentialBackoff will retry webhookFn() as specified by the given backoff parameters with exponentially
// increasing backoff when it returns an error for which this GenericWebhook's ShouldRetry function returns true,
// confirming it to be retriable. If no ShouldRetry has been defined for the webhook,
// then the default one is used (DefaultShouldRetry).
func (g *GenericWebhook) WithExponentialBackoff(ctx context.Context, webhookFn func() rest.Result) rest.Result {
	var result rest.Result
	shouldRetry := g.ShouldRetry
	if shouldRetry == nil {
		shouldRetry = DefaultShouldRetry
	}
	WithExponentialBackoff(ctx, g.RetryBackoff, func() error {
		result = webhookFn()
		return result.Error()
	}, shouldRetry)
	return result
}

// WithExponentialBackoff will retry webhookFn up to 5 times with exponentially increasing backoff when
// it returns an error for which shouldRetry returns true, confirming it to be retriable.
func WithExponentialBackoff(ctx context.Context, retryBackoff wait.Backoff, webhookFn func() error, shouldRetry func(error) bool) error {
	// having a webhook error allows us to track the last actual webhook error for requests that
	// are later cancelled or time out.
	var webhookErr error
	err := wait.ExponentialBackoffWithContext(ctx, retryBackoff, func(_ context.Context) (bool, error) {
		webhookErr = webhookFn()
		if shouldRetry(webhookErr) {
			return false, nil
		}
		if webhookErr != nil {
			return false, webhookErr
		}
		return true, nil
	})

	switch {
	// we check for webhookErr first, if webhookErr is set it's the most important error to return.
	case webhookErr != nil:
		return webhookErr
	case err != nil:
		return fmt.Errorf("webhook call failed: %s", err.Error())
	default:
		return nil
	}
}

func LoadKubeconfig(kubeConfigFile string, customDial utilnet.DialFunc) (*rest.Config, error) {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	loadingRules.ExplicitPath = kubeConfigFile
	loader := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, &clientcmd.ConfigOverrides{})

	clientConfig, err := loader.ClientConfig()
	if err != nil {
		return nil, err
	}

	clientConfig.Dial = customDial

	// Kubeconfigs can't set a timeout, this can only be set through a command line flag.
	//
	// https://github.com/kubernetes/client-go/blob/master/tools/clientcmd/overrides.go
	//
	// Set this to something reasonable so request to webhooks don't hang forever.
	clientConfig.Timeout = defaultRequestTimeout

	// Avoid client-side rate limiting talking to the webhook backend.
	// Rate limiting should happen when deciding how many requests to serve.
	clientConfig.QPS = -1

	return clientConfig, nil
}
