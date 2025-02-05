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

package authorizer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"sync"
	"sync/atomic"
	"time"

	"k8s.io/apimachinery/pkg/util/sets"
	authzconfig "k8s.io/apiserver/pkg/apis/apiserver"
	"k8s.io/apiserver/pkg/authentication/user"
	"k8s.io/apiserver/pkg/authorization/authorizer"
	"k8s.io/apiserver/pkg/authorization/authorizerfactory"
	authorizationcel "k8s.io/apiserver/pkg/authorization/cel"
	authorizationmetrics "k8s.io/apiserver/pkg/authorization/metrics"
	"k8s.io/apiserver/pkg/authorization/union"
	"k8s.io/apiserver/pkg/server/options/authorizationconfig/metrics"
	webhookutil "k8s.io/apiserver/pkg/util/webhook"
	"k8s.io/apiserver/plugin/pkg/authorizer/webhook"
	webhookmetrics "k8s.io/apiserver/plugin/pkg/authorizer/webhook/metrics"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/auth/authorizer/abac"
	"k8s.io/kubernetes/pkg/kubeapiserver/authorizer/modes"
	"k8s.io/kubernetes/pkg/util/filesystem"
	"k8s.io/kubernetes/plugin/pkg/auth/authorizer/node"
	"k8s.io/kubernetes/plugin/pkg/auth/authorizer/rbac"
)

type reloadableAuthorizerResolver struct {
	// initialConfig holds the ReloadFile used to initiate background reloading,
	// and information used to construct webhooks that isn't exposed in the authorization
	// configuration file (dial function, backoff settings, etc)
	initialConfig Config

	apiServerID string

	reloadInterval         time.Duration
	requireNonWebhookTypes sets.Set[authzconfig.AuthorizerType]

	nodeAuthorizer *node.NodeAuthorizer
	rbacAuthorizer *rbac.RBACAuthorizer
	abacAuthorizer abac.PolicyList
	compiler       authorizationcel.Compiler // non-nil and shared across reloads.

	lastLoadedLock   sync.Mutex
	lastLoadedConfig *authzconfig.AuthorizationConfiguration
	lastReadData     []byte

	current atomic.Pointer[authorizerResolver]
}

type authorizerResolver struct {
	authorizer   authorizer.Authorizer
	ruleResolver authorizer.RuleResolver
}

func (r *reloadableAuthorizerResolver) Authorize(ctx context.Context, a authorizer.Attributes) (authorized authorizer.Decision, reason string, err error) {
	return r.current.Load().authorizer.Authorize(ctx, a)
}

func (r *reloadableAuthorizerResolver) RulesFor(ctx context.Context, user user.Info, namespace string) ([]authorizer.ResourceRuleInfo, []authorizer.NonResourceRuleInfo, bool, error) {
	return r.current.Load().ruleResolver.RulesFor(ctx, user, namespace)
}

// newForConfig constructs
func (r *reloadableAuthorizerResolver) newForConfig(authzConfig *authzconfig.AuthorizationConfiguration) (authorizer.Authorizer, authorizer.RuleResolver, error) {
	if len(authzConfig.Authorizers) == 0 {
		return nil, nil, fmt.Errorf("at least one authorization mode must be passed")
	}

	var (
		authorizers   []authorizer.Authorizer
		ruleResolvers []authorizer.RuleResolver
	)

	// Add SystemPrivilegedGroup as an authorizing group
	superuserAuthorizer := authorizerfactory.NewPrivilegedGroups(user.SystemPrivilegedGroup)
	authorizers = append(authorizers, superuserAuthorizer)

	for _, configuredAuthorizer := range authzConfig.Authorizers {
		// Keep cases in sync with constant list in k8s.io/kubernetes/pkg/kubeapiserver/authorizer/modes/modes.go.
		switch configuredAuthorizer.Type {
		case authzconfig.AuthorizerType(modes.ModeNode):
			if r.nodeAuthorizer == nil {
				return nil, nil, fmt.Errorf("authorizer type Node is not allowed if it was not enabled at initial server startup")
			}
			authorizers = append(authorizers, authorizationmetrics.InstrumentedAuthorizer(string(configuredAuthorizer.Type), configuredAuthorizer.Name, r.nodeAuthorizer))
			ruleResolvers = append(ruleResolvers, r.nodeAuthorizer)
		case authzconfig.AuthorizerType(modes.ModeAlwaysAllow):
			alwaysAllowAuthorizer := authorizerfactory.NewAlwaysAllowAuthorizer()
			authorizers = append(authorizers, authorizationmetrics.InstrumentedAuthorizer(string(configuredAuthorizer.Type), configuredAuthorizer.Name, alwaysAllowAuthorizer))
			ruleResolvers = append(ruleResolvers, alwaysAllowAuthorizer)
		case authzconfig.AuthorizerType(modes.ModeAlwaysDeny):
			alwaysDenyAuthorizer := authorizerfactory.NewAlwaysDenyAuthorizer()
			authorizers = append(authorizers, authorizationmetrics.InstrumentedAuthorizer(string(configuredAuthorizer.Type), configuredAuthorizer.Name, alwaysDenyAuthorizer))
			ruleResolvers = append(ruleResolvers, alwaysDenyAuthorizer)
		case authzconfig.AuthorizerType(modes.ModeABAC):
			if r.abacAuthorizer == nil {
				return nil, nil, fmt.Errorf("authorizer type ABAC is not allowed if it was not enabled at initial server startup")
			}
			authorizers = append(authorizers, authorizationmetrics.InstrumentedAuthorizer(string(configuredAuthorizer.Type), configuredAuthorizer.Name, r.abacAuthorizer))
			ruleResolvers = append(ruleResolvers, r.abacAuthorizer)
		case authzconfig.AuthorizerType(modes.ModeWebhook):
			if r.initialConfig.WebhookRetryBackoff == nil {
				return nil, nil, errors.New("retry backoff parameters for authorization webhook has not been specified")
			}
			clientConfig, err := webhookutil.LoadKubeconfig(*configuredAuthorizer.Webhook.ConnectionInfo.KubeConfigFile, r.initialConfig.CustomDial)
			if err != nil {
				return nil, nil, err
			}
			if configuredAuthorizer.Webhook.Timeout.Duration != 0 {
				clientConfig.Timeout = configuredAuthorizer.Webhook.Timeout.Duration
			}
			var decisionOnError authorizer.Decision
			switch configuredAuthorizer.Webhook.FailurePolicy {
			case authzconfig.FailurePolicyNoOpinion:
				decisionOnError = authorizer.DecisionNoOpinion
			case authzconfig.FailurePolicyDeny:
				decisionOnError = authorizer.DecisionDeny
			default:
				return nil, nil, fmt.Errorf("unknown failurePolicy %q", configuredAuthorizer.Webhook.FailurePolicy)
			}

			authorizedTTL, unauthorizedTTL := configuredAuthorizer.Webhook.AuthorizedTTL.Duration, configuredAuthorizer.Webhook.UnauthorizedTTL.Duration
			if !configuredAuthorizer.Webhook.CacheAuthorizedRequests {
				authorizedTTL = 0
			}
			if !configuredAuthorizer.Webhook.CacheUnauthorizedRequests {
				unauthorizedTTL = 0
			}
			webhookAuthorizer, err := webhook.New(clientConfig,
				configuredAuthorizer.Webhook.SubjectAccessReviewVersion,
				authorizedTTL,
				unauthorizedTTL,
				*r.initialConfig.WebhookRetryBackoff,
				decisionOnError,
				configuredAuthorizer.Webhook.MatchConditions,
				configuredAuthorizer.Name,
				kubeapiserverWebhookMetrics{WebhookMetrics: webhookmetrics.NewWebhookMetrics(), MatcherMetrics: authorizationcel.NewMatcherMetrics()},
				r.compiler,
			)
			if err != nil {
				return nil, nil, err
			}
			authorizers = append(authorizers, authorizationmetrics.InstrumentedAuthorizer(string(configuredAuthorizer.Type), configuredAuthorizer.Name, webhookAuthorizer))
			ruleResolvers = append(ruleResolvers, webhookAuthorizer)
		case authzconfig.AuthorizerType(modes.ModeRBAC):
			if r.rbacAuthorizer == nil {
				return nil, nil, fmt.Errorf("authorizer type RBAC is not allowed if it was not enabled at initial server startup")
			}
			authorizers = append(authorizers, authorizationmetrics.InstrumentedAuthorizer(string(configuredAuthorizer.Type), configuredAuthorizer.Name, r.rbacAuthorizer))
			ruleResolvers = append(ruleResolvers, r.rbacAuthorizer)
		default:
			return nil, nil, fmt.Errorf("unknown authorization mode %s specified", configuredAuthorizer.Type)
		}
	}

	return union.New(authorizers...), union.NewRuleResolvers(ruleResolvers...), nil
}

type kubeapiserverWebhookMetrics struct {
	// kube-apiserver doesn't report request metrics
	webhookmetrics.NoopRequestMetrics
	// kube-apiserver does report webhook metrics
	webhookmetrics.WebhookMetrics
	// kube-apiserver does report matchCondition metrics
	authorizationcel.MatcherMetrics
}

// runReload starts checking the config file for changes and reloads the authorizer when it changes.
// Blocks until ctx is complete.
func (r *reloadableAuthorizerResolver) runReload(ctx context.Context) {
	metrics.RegisterMetrics()
	metrics.RecordAuthorizationConfigAutomaticReloadSuccess(r.apiServerID)

	filesystem.WatchUntil(
		ctx,
		r.reloadInterval,
		r.initialConfig.ReloadFile,
		func() {
			r.checkFile(ctx)
		},
		func(err error) {
			klog.ErrorS(err, "watching authorization config file")
		},
	)
}

func (r *reloadableAuthorizerResolver) checkFile(ctx context.Context) {
	r.lastLoadedLock.Lock()
	defer r.lastLoadedLock.Unlock()

	data, err := os.ReadFile(r.initialConfig.ReloadFile)
	if err != nil {
		klog.ErrorS(err, "reloading authorization config")
		metrics.RecordAuthorizationConfigAutomaticReloadFailure(r.apiServerID)
		return
	}
	if bytes.Equal(data, r.lastReadData) {
		// no change
		return
	}
	klog.InfoS("found new authorization config data")
	r.lastReadData = data

	config, err := LoadAndValidateData(data, r.compiler, r.requireNonWebhookTypes)
	if err != nil {
		klog.ErrorS(err, "reloading authorization config")
		metrics.RecordAuthorizationConfigAutomaticReloadFailure(r.apiServerID)
		return
	}
	if reflect.DeepEqual(config, r.lastLoadedConfig) {
		// no change
		return
	}
	klog.InfoS("found new authorization config")
	r.lastLoadedConfig = config

	authorizer, ruleResolver, err := r.newForConfig(config)
	if err != nil {
		klog.ErrorS(err, "reloading authorization config")
		metrics.RecordAuthorizationConfigAutomaticReloadFailure(r.apiServerID)
		return
	}
	klog.InfoS("constructed new authorizer")

	r.current.Store(&authorizerResolver{
		authorizer:   authorizer,
		ruleResolver: ruleResolver,
	})
	klog.InfoS("reloaded authz config")
	metrics.RecordAuthorizationConfigAutomaticReloadSuccess(r.apiServerID)
}
