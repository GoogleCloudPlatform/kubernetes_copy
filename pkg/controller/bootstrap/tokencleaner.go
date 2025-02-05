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

package bootstrap

import (
	"context"
	"fmt"
	"time"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	coreinformers "k8s.io/client-go/informers/core/v1"
	clientset "k8s.io/client-go/kubernetes"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	bootstrapapi "k8s.io/cluster-bootstrap/token/api"
	bootstrapsecretutil "k8s.io/cluster-bootstrap/util/secrets"
	"k8s.io/klog/v2"
	api "k8s.io/kubernetes/pkg/apis/core"
	"k8s.io/kubernetes/pkg/controller"
)

// TokenCleanerOptions contains options for the TokenCleaner
type TokenCleanerOptions struct {
	// TokenSecretNamespace string is the namespace for token Secrets.
	TokenSecretNamespace string

	// SecretResync is the time.Duration at which to fully re-list secrets.
	// If zero, re-list will be delayed as long as possible
	SecretResync time.Duration
}

// DefaultTokenCleanerOptions returns a set of default options for creating a
// TokenCleaner
func DefaultTokenCleanerOptions() TokenCleanerOptions {
	return TokenCleanerOptions{
		TokenSecretNamespace: api.NamespaceSystem,
	}
}

// TokenCleaner is a controller that deletes expired tokens
type TokenCleaner struct {
	tokenSecretNamespace string

	client clientset.Interface

	// secretLister is able to list/get secrets and is populated by the shared informer passed to NewTokenCleaner.
	secretLister corelisters.SecretLister

	// secretSynced returns true if the secret shared informer has been synced at least once.
	secretSynced cache.InformerSynced

	queue workqueue.TypedRateLimitingInterface[string]
}

// NewTokenCleaner returns a new *NewTokenCleaner.
func NewTokenCleaner(cl clientset.Interface, secrets coreinformers.SecretInformer, options TokenCleanerOptions) (*TokenCleaner, error) {
	e := &TokenCleaner{
		client:               cl,
		secretLister:         secrets.Lister(),
		secretSynced:         secrets.Informer().HasSynced,
		tokenSecretNamespace: options.TokenSecretNamespace,
		queue: workqueue.NewTypedRateLimitingQueueWithConfig(
			workqueue.DefaultTypedControllerRateLimiter[string](),
			workqueue.TypedRateLimitingQueueConfig[string]{
				Name: "token_cleaner",
			},
		),
	}

	secrets.Informer().AddEventHandlerWithResyncPeriod(
		cache.FilteringResourceEventHandler{
			FilterFunc: func(obj interface{}) bool {
				switch t := obj.(type) {
				case *v1.Secret:
					return t.Type == bootstrapapi.SecretTypeBootstrapToken && t.Namespace == e.tokenSecretNamespace
				default:
					utilruntime.HandleError(fmt.Errorf("object passed to %T that is not expected: %T", e, obj))
					return false
				}
			},
			Handler: cache.ResourceEventHandlerFuncs{
				AddFunc:    e.enqueueSecrets,
				UpdateFunc: func(oldSecret, newSecret interface{}) { e.enqueueSecrets(newSecret) },
			},
		},
		options.SecretResync,
	)

	return e, nil
}

// Run runs controller loops and returns when they are done
func (tc *TokenCleaner) Run(ctx context.Context) {
	defer utilruntime.HandleCrash()
	defer tc.queue.ShutDown()

	logger := klog.FromContext(ctx)
	logger.Info("Starting token cleaner controller")
	defer logger.Info("Shutting down token cleaner controller")

	if !cache.WaitForNamedCacheSync("token_cleaner", ctx.Done(), tc.secretSynced) {
		return
	}

	go wait.UntilWithContext(ctx, tc.worker, 10*time.Second)

	<-ctx.Done()
}

func (tc *TokenCleaner) enqueueSecrets(obj interface{}) {
	key, err := controller.KeyFunc(obj)
	if err != nil {
		utilruntime.HandleError(err)
		return
	}
	tc.queue.Add(key)
}

// worker runs a thread that dequeues secrets, handles them, and marks them done.
func (tc *TokenCleaner) worker(ctx context.Context) {
	for tc.processNextWorkItem(ctx) {
	}
}

// processNextWorkItem deals with one key off the queue.  It returns false when it's time to quit.
func (tc *TokenCleaner) processNextWorkItem(ctx context.Context) bool {
	key, quit := tc.queue.Get()
	if quit {
		return false
	}
	defer tc.queue.Done(key)

	if err := tc.syncFunc(ctx, key); err != nil {
		tc.queue.AddRateLimited(key)
		utilruntime.HandleError(fmt.Errorf("Sync %v failed with : %v", key, err))
		return true
	}

	tc.queue.Forget(key)
	return true
}

func (tc *TokenCleaner) syncFunc(ctx context.Context, key string) error {
	logger := klog.FromContext(ctx)
	startTime := time.Now()
	defer func() {
		logger.V(4).Info("Finished syncing secret", "secret", key, "elapsedTime", time.Since(startTime))
	}()

	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}

	ret, err := tc.secretLister.Secrets(namespace).Get(name)
	if apierrors.IsNotFound(err) {
		logger.V(3).Info("Secret has been deleted", "secret", key)
		return nil
	}

	if err != nil {
		return err
	}

	if ret.Type == bootstrapapi.SecretTypeBootstrapToken {
		tc.evalSecret(ctx, ret)
	}
	return nil
}

func (tc *TokenCleaner) evalSecret(ctx context.Context, o interface{}) {
	logger := klog.FromContext(ctx)
	secret := o.(*v1.Secret)
	ttl, alreadyExpired := bootstrapsecretutil.GetExpiration(logger, secret, time.Now())
	if alreadyExpired {
		logger.V(3).Info("Deleting expired secret", "secret", klog.KObj(secret))
		var options metav1.DeleteOptions
		if len(secret.UID) > 0 {
			options.Preconditions = &metav1.Preconditions{UID: &secret.UID}
		}
		err := tc.client.CoreV1().Secrets(secret.Namespace).Delete(ctx, secret.Name, options)
		// NotFound isn't a real error (it's already been deleted)
		// Conflict isn't a real error (the UID precondition failed)
		if err != nil && !apierrors.IsConflict(err) && !apierrors.IsNotFound(err) {
			logger.V(3).Info("Error deleting secret", "err", err)
		}
	} else if ttl > 0 {
		key, err := controller.KeyFunc(o)
		if err != nil {
			utilruntime.HandleError(err)
			return
		}
		tc.queue.AddAfter(key, ttl)
	}
}
