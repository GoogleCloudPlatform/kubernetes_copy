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

package serviceaccount

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	applyv1 "k8s.io/client-go/applyconfigurations/core/v1"
	coreinformers "k8s.io/client-go/informers/core/v1"
	clientset "k8s.io/client-go/kubernetes"
	listersv1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
	podutil "k8s.io/kubernetes/pkg/api/v1/pod"
	"k8s.io/kubernetes/pkg/controlplane/controller/legacytokentracking"
	"k8s.io/kubernetes/pkg/serviceaccount"
	"k8s.io/utils/clock"
)

const (
	dateFormat                 = "2006-01-02"
	DefaultCleanerSyncInterval = 24 * time.Hour
)

// TokenCleanerOptions contains options for the LegacySATokenCleaner
type LegacySATokenCleanerOptions struct {
	// CleanUpPeriod is the period of time since the last usage of an legacy token before it can be deleted.
	CleanUpPeriod time.Duration
	SyncInterval  time.Duration
}

// LegacySATokenCleaner is a controller that deletes legacy serviceaccount tokens that are not in use for a specified period of time.
type LegacySATokenCleaner struct {
	client           clientset.Interface
	clock            clock.Clock
	saLister         listersv1.ServiceAccountLister
	saInformerSynced cache.InformerSynced

	secretLister         listersv1.SecretLister
	secretInformerSynced cache.InformerSynced

	podLister         listersv1.PodLister
	podInformerSynced cache.InformerSynced

	syncInterval         time.Duration
	minimumSinceLastUsed time.Duration
}

// NewLegacySATokenCleaner returns a new *NewLegacySATokenCleaner.
func NewLegacySATokenCleaner(saInformer coreinformers.ServiceAccountInformer, secretInformer coreinformers.SecretInformer, podInformer coreinformers.PodInformer, client clientset.Interface, cl clock.Clock, options LegacySATokenCleanerOptions) (*LegacySATokenCleaner, error) {
	if !(options.CleanUpPeriod > 0) {
		return nil, fmt.Errorf("invalid CleanUpPeriod: %v", options.CleanUpPeriod)
	}
	if !(options.SyncInterval > 0) {
		return nil, fmt.Errorf("invalid SyncInterval: %v", options.SyncInterval)
	}

	tc := &LegacySATokenCleaner{
		client:               client,
		clock:                cl,
		saLister:             saInformer.Lister(),
		saInformerSynced:     saInformer.Informer().HasSynced,
		secretLister:         secretInformer.Lister(),
		secretInformerSynced: secretInformer.Informer().HasSynced,
		podLister:            podInformer.Lister(),
		podInformerSynced:    podInformer.Informer().HasSynced,
		minimumSinceLastUsed: options.CleanUpPeriod,
		syncInterval:         options.SyncInterval,
	}

	return tc, nil
}

func (tc *LegacySATokenCleaner) Run(ctx context.Context) {
	defer utilruntime.HandleCrash()

	logger := klog.FromContext(ctx)
	logger.Info("Starting legacy service account token cleaner controller")
	defer logger.Info("Shutting down legacy service account token cleaner controller")

	if !cache.WaitForNamedCacheSync("legacy-service-account-token-cleaner", ctx.Done(), tc.saInformerSynced, tc.secretInformerSynced, tc.podInformerSynced) {
		return
	}

	go wait.UntilWithContext(ctx, tc.evaluateSATokens, tc.syncInterval)

	<-ctx.Done()
}

func (tc *LegacySATokenCleaner) evaluateSATokens(ctx context.Context) {
	logger := klog.FromContext(ctx)

	now := tc.clock.Now().UTC()
	trackedSince, err := tc.latestPossibleTrackedSinceTime(ctx)
	if err != nil {
		logger.Error(err, "Getting latest possible tracked_since time")
		return
	}

	if now.Before(trackedSince.Add(tc.minimumSinceLastUsed)) {
		// we haven't been tracking long enough
		return
	}

	preserveCreatedOnOrAfter := now.Add(-tc.minimumSinceLastUsed)
	preserveUsedOnOrAfter := now.Add(-tc.minimumSinceLastUsed).Format(dateFormat)

	secretList, err := tc.secretLister.Secrets(metav1.NamespaceAll).List(labels.Everything())
	if err != nil {
		logger.Error(err, "Getting cached secret list")
		return
	}

	namespaceToUsedSecretNames := make(map[string]sets.String)
	for _, secret := range secretList {
		if secret.Type != v1.SecretTypeServiceAccountToken {
			continue
		}
		if !secret.CreationTimestamp.Time.Before(preserveCreatedOnOrAfter) {
			continue
		}

		if secret.DeletionTimestamp != nil {
			continue
		}

		// if LastUsedLabelKey does not exist, we think the secret has not been used
		// since the legacy token starts to track.
		lastUsed, ok := secret.Labels[serviceaccount.LastUsedLabelKey]
		if ok {
			_, err := time.Parse(dateFormat, lastUsed)
			if err != nil {
				// the lastUsed value is not well-formed thus we cannot determine it
				logger.Error(err, "Parsing lastUsed time", "secret", klog.KRef(secret.Namespace, secret.Name))
				continue
			}
			if lastUsed >= preserveUsedOnOrAfter {
				continue
			}
		}

		sa, saErr := tc.getServiceAccount(secret)

		if saErr != nil {
			logger.Error(saErr, "Getting service account", "secret", klog.KRef(secret.Namespace, secret.Name))
			continue
		}
		if sa == nil || !hasSecretReference(sa, secret.Name) {
			// can't determine if this is an auto-generated token
			continue
		}

		mountedSecretNames, err := tc.getMountedSecretNames(secret.Namespace, namespaceToUsedSecretNames)
		if err != nil {
			logger.Error(err, "Resolving mounted secrets", "secret", klog.KRef(secret.Namespace, secret.Name))
			continue
		}
		if mountedSecretNames.Has(secret.Name) {
			// still used by pods
			continue
		}

		invalidSince := secret.Labels[serviceaccount.InvalidSinceLabelKey]
		// If the secret has not been labeled with invalid since date or the label value has invalid format, update the invalidSince label with the current date value.
		_, err = time.Parse(dateFormat, invalidSince)
		if err != nil {
			invalidSince = now.Format(dateFormat)
			logger.Info("Mark the auto-generated service account token as invalid", "invalidSince", invalidSince, "secret", klog.KRef(secret.Namespace, secret.Name))
			patchContent, err := json.Marshal(applyv1.Secret(secret.Name, secret.Namespace).WithUID(secret.UID).WithLabels(map[string]string{serviceaccount.InvalidSinceLabelKey: invalidSince}))
			if err != nil {
				logger.Error(err, "Failed to marshal invalid since label")
			} else {
				if _, err := tc.client.CoreV1().Secrets(secret.Namespace).Patch(ctx, secret.Name, types.MergePatchType, patchContent, metav1.PatchOptions{}); err != nil {
					logger.Error(err, "Failed to label legacy service account token secret with invalid since date")
				}
			}
			continue
		}

		if invalidSince >= preserveUsedOnOrAfter {
			continue
		}

		logger.Info("Delete auto-generated service account token", "secret", klog.KRef(secret.Namespace, secret.Name), "creationTime", secret.CreationTimestamp, "lastUsed", lastUsed, "invalidSince", invalidSince)
		if err := tc.client.CoreV1().Secrets(secret.Namespace).Delete(ctx, secret.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{ResourceVersion: &secret.ResourceVersion}}); err != nil && !apierrors.IsConflict(err) && !apierrors.IsNotFound(err) {
			logger.Error(err, "Deleting legacy service account token", "secret", klog.KRef(secret.Namespace, secret.Name), "serviceaccount", sa.Name)
		}
	}
}

func (tc *LegacySATokenCleaner) getMountedSecretNames(secretNamespace string, namespaceToUsedSecretNames map[string]sets.String) (sets.String, error) {
	if secrets, ok := namespaceToUsedSecretNames[secretNamespace]; ok {
		return secrets, nil
	}

	podList, err := tc.podLister.Pods(secretNamespace).List(labels.Everything())
	if err != nil {
		return nil, fmt.Errorf("failed to get pod list from pod cache: %v", err)
	}

	var secrets sets.String
	for _, pod := range podList {
		podutil.VisitPodSecretNames(pod, func(secretName string) bool {
			if secrets == nil {
				secrets = sets.NewString()
			}
			secrets.Insert(secretName)
			return true
		})
	}
	if secrets != nil {
		namespaceToUsedSecretNames[secretNamespace] = secrets
	}
	return secrets, nil
}

func (tc *LegacySATokenCleaner) getServiceAccount(secret *v1.Secret) (*v1.ServiceAccount, error) {
	saName := secret.Annotations[v1.ServiceAccountNameKey]
	if len(saName) == 0 {
		return nil, nil
	}
	saUID := types.UID(secret.Annotations[v1.ServiceAccountUIDKey])
	sa, err := tc.saLister.ServiceAccounts(secret.Namespace).Get(saName)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	// Ensure UID matches if given
	if len(saUID) == 0 || saUID == sa.UID {
		return sa, nil
	}

	return nil, nil
}

// get the latest possible TrackedSince time information from the configMap label.
func (tc *LegacySATokenCleaner) latestPossibleTrackedSinceTime(ctx context.Context) (time.Time, error) {
	configMap, err := tc.client.CoreV1().ConfigMaps(metav1.NamespaceSystem).Get(ctx, legacytokentracking.ConfigMapName, metav1.GetOptions{})
	if err != nil {
		return time.Time{}, err
	}
	trackedSince, exist := configMap.Data[legacytokentracking.ConfigMapDataKey]
	if !exist {
		return time.Time{}, fmt.Errorf("configMap does not have since label")
	}
	trackedSinceTime, err := time.Parse(dateFormat, trackedSince)
	if err != nil {
		return time.Time{}, fmt.Errorf("error parsing trackedSince time: %v", err)
	}
	// make sure the time to be 00:00 on the day just after the date starts to track
	return trackedSinceTime.AddDate(0, 0, 1), nil
}

func hasSecretReference(serviceAccount *v1.ServiceAccount, secretName string) bool {
	for _, secret := range serviceAccount.Secrets {
		if secret.Name == secretName {
			return true
		}
	}
	return false
}
