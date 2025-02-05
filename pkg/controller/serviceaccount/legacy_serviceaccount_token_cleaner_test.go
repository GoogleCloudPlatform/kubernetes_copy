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
	"reflect"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	applyv1 "k8s.io/client-go/applyconfigurations/core/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	core "k8s.io/client-go/testing"
	"k8s.io/kubernetes/pkg/controller"
	"k8s.io/kubernetes/pkg/controlplane/controller/legacytokentracking"
	"k8s.io/kubernetes/pkg/serviceaccount"
	testingclock "k8s.io/utils/clock/testing"
)

func configuredConfigMap(label string) *v1.ConfigMap {
	if label == "" {
		return &v1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Namespace: metav1.NamespaceSystem, Name: legacytokentracking.ConfigMapName},
		}
	}
	return &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: metav1.NamespaceSystem, Name: legacytokentracking.ConfigMapName},
		Data:       map[string]string{legacytokentracking.ConfigMapDataKey: label},
	}
}

func configuredServiceAccountTokenSecret(lastUsedLabel, invalidSinceLabel, creationTimeString, serviceAccountName, serviceAccountUID, deletionTimeString string) *v1.Secret {
	var deletionTime *metav1.Time
	if deletionTimeString == "" {
		deletionTime = nil
	} else {
		deletionTime = &metav1.Time{Time: time.Now().UTC()}
	}
	creationTime, _ := time.Parse(dateFormat, creationTimeString)
	labels := map[string]string{}
	if lastUsedLabel != "" {
		labels[serviceaccount.LastUsedLabelKey] = lastUsedLabel
	}
	if invalidSinceLabel != "" {
		labels[serviceaccount.InvalidSinceLabelKey] = invalidSinceLabel
	}
	return &v1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "token-secret-1",
			Namespace:         "default",
			UID:               "23456",
			ResourceVersion:   "1",
			Labels:            labels,
			CreationTimestamp: metav1.NewTime(creationTime),
			DeletionTimestamp: deletionTime,
			Annotations: map[string]string{
				v1.ServiceAccountNameKey: serviceAccountName,
				v1.ServiceAccountUIDKey:  serviceAccountUID,
			},
		},
		Type: v1.SecretTypeServiceAccountToken,
		Data: map[string][]byte{
			"token":     []byte("ABC"),
			"ca.crt":    []byte("CA Data"),
			"namespace": []byte("default"),
		},
	}
}

func configuredLegacyTokenCleanUpPeriod(start string) time.Duration {
	current := time.Now().UTC()
	startTime, _ := time.Parse(dateFormat, start)
	return current.Sub(startTime)
}

func configuredPod(withSecretMount bool) *v1.Pod {
	if !withSecretMount {
		return &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "pod-1",
				Namespace: "default",
			},
		}
	}
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pod-1",
			Namespace: "default",
		},
		Spec: v1.PodSpec{
			Volumes: []v1.Volume{{Name: "foo", VolumeSource: v1.VolumeSource{Secret: &v1.SecretVolumeSource{SecretName: "token-secret-1"}}}},
		},
	}
}

func patchContent(namespace, name, invalidSince string, uID types.UID) []byte {
	patch, _ := json.Marshal(applyv1.Secret(name, namespace).WithUID(uID).WithLabels(map[string]string{serviceaccount.InvalidSinceLabelKey: invalidSince}))
	return patch
}

func TestLegacyServiceAccountTokenCleanUp(t *testing.T) {
	testcases := map[string]struct {
		LegacyTokenCleanUpPeriod time.Duration

		ExistingServiceAccount *v1.ServiceAccount
		ExistingSecret         *v1.Secret
		ExistingPod            *v1.Pod
		ClientObjects          []runtime.Object

		ExpectedActions []core.Action
	}{
		"configmap does not exist": {
			ExistingSecret:           configuredServiceAccountTokenSecret("2022-12-27", "", "2022-12-27", "default", "12345", ""),
			ExistingServiceAccount:   serviceAccount(tokenSecretReferences()),
			ExistingPod:              configuredPod(false),
			LegacyTokenCleanUpPeriod: configuredLegacyTokenCleanUpPeriod("2022-12-28"),
			ExpectedActions: []core.Action{
				core.NewGetAction(schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}, metav1.NamespaceSystem, legacytokentracking.ConfigMapName),
			},
		},
		"configmap exists, but the configmap does not have tracked-since label": {
			ExistingSecret:           configuredServiceAccountTokenSecret("2022-12-27", "", "2022-12-27", "default", "12345", ""),
			ExistingServiceAccount:   serviceAccount(tokenSecretReferences()),
			ExistingPod:              configuredPod(false),
			ClientObjects:            []runtime.Object{configuredConfigMap("")},
			LegacyTokenCleanUpPeriod: configuredLegacyTokenCleanUpPeriod("2022-12-28"),
			ExpectedActions: []core.Action{
				core.NewGetAction(schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}, metav1.NamespaceSystem, legacytokentracking.ConfigMapName),
			},
		},
		"configmap exists, the time period since 'tracked-since' is smaller than the CleanUpPeriod": {
			ExistingSecret:           configuredServiceAccountTokenSecret("2022-12-27", "", "2022-12-27", "default", "12345", ""),
			ExistingServiceAccount:   serviceAccount(tokenSecretReferences()),
			ExistingPod:              configuredPod(false),
			ClientObjects:            []runtime.Object{configuredConfigMap("2022-12-29")},
			LegacyTokenCleanUpPeriod: configuredLegacyTokenCleanUpPeriod("2022-12-29"),
			ExpectedActions: []core.Action{
				core.NewGetAction(schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}, metav1.NamespaceSystem, legacytokentracking.ConfigMapName),
			},
		},
		"configmap exists, the 'tracked-since' cannot be parsed": {
			ExistingSecret:           configuredServiceAccountTokenSecret("2022-12-27", "", "2022-12-27", "default", "12345", ""),
			ExistingServiceAccount:   serviceAccount(tokenSecretReferences()),
			ExistingPod:              configuredPod(false),
			ClientObjects:            []runtime.Object{configuredConfigMap("2022-12-27-1")},
			LegacyTokenCleanUpPeriod: configuredLegacyTokenCleanUpPeriod("2022-12-29"),
			ExpectedActions: []core.Action{
				core.NewGetAction(schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}, metav1.NamespaceSystem, legacytokentracking.ConfigMapName),
			},
		},
		"secret is not SecretTypeServiceAccountToken type": {
			ExistingSecret:           opaqueSecret(),
			ExistingServiceAccount:   serviceAccount(tokenSecretReferences()),
			ExistingPod:              configuredPod(false),
			ClientObjects:            []runtime.Object{configuredConfigMap("2022-12-28")},
			LegacyTokenCleanUpPeriod: configuredLegacyTokenCleanUpPeriod("2022-12-29"),
			ExpectedActions: []core.Action{
				core.NewGetAction(schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}, metav1.NamespaceSystem, legacytokentracking.ConfigMapName),
			},
		},
		"secret is not referenced by serviceaccount": {
			ExistingSecret:           configuredServiceAccountTokenSecret("2022-12-27", "", "2022-12-27", "default", "12345", ""),
			ExistingServiceAccount:   serviceAccount(emptySecretReferences()),
			ExistingPod:              configuredPod(false),
			ClientObjects:            []runtime.Object{configuredConfigMap("2022-12-27")},
			LegacyTokenCleanUpPeriod: configuredLegacyTokenCleanUpPeriod("2022-12-29"),
			ExpectedActions: []core.Action{
				core.NewGetAction(schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}, metav1.NamespaceSystem, legacytokentracking.ConfigMapName),
			},
		},
		"auto-generated secret has a late creation time": {
			ExistingSecret:           configuredServiceAccountTokenSecret("2022-12-27", "", "2022-12-30", "default", "12345", ""),
			ExistingServiceAccount:   serviceAccount(tokenSecretReferences()),
			ExistingPod:              configuredPod(false),
			ClientObjects:            []runtime.Object{configuredConfigMap("2022-12-27")},
			LegacyTokenCleanUpPeriod: configuredLegacyTokenCleanUpPeriod("2022-12-29"),
			ExpectedActions: []core.Action{
				core.NewGetAction(schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}, metav1.NamespaceSystem, legacytokentracking.ConfigMapName),
			},
		},
		"auto-generated secret has a deletion time": {
			ExistingSecret:           configuredServiceAccountTokenSecret("2022-12-27", "", "2022-12-27", "default", "12345", "deleted"),
			ExistingServiceAccount:   serviceAccount(tokenSecretReferences()),
			ExistingPod:              configuredPod(false),
			ClientObjects:            []runtime.Object{configuredConfigMap("2022-12-27")},
			LegacyTokenCleanUpPeriod: configuredLegacyTokenCleanUpPeriod("2022-12-30"),
			ExpectedActions: []core.Action{
				core.NewGetAction(schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}, metav1.NamespaceSystem, legacytokentracking.ConfigMapName),
			},
		},
		"auto-generated secret has a late last-used time": {
			ExistingSecret:           configuredServiceAccountTokenSecret("2022-12-30", "", "2022-12-27", "default", "12345", ""),
			ExistingServiceAccount:   serviceAccount(tokenSecretReferences()),
			ExistingPod:              configuredPod(false),
			ClientObjects:            []runtime.Object{configuredConfigMap("2022-12-27")},
			LegacyTokenCleanUpPeriod: configuredLegacyTokenCleanUpPeriod("2022-12-29"),
			ExpectedActions: []core.Action{
				core.NewGetAction(schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}, metav1.NamespaceSystem, legacytokentracking.ConfigMapName),
			},
		},
		"auto-generated secret has a last-used label, but it can not be parsed": {
			ExistingSecret:           configuredServiceAccountTokenSecret("2022-12-27-1", "", "2022-12-27", "default", "12345", ""),
			ExistingServiceAccount:   serviceAccount(tokenSecretReferences()),
			ExistingPod:              configuredPod(false),
			ClientObjects:            []runtime.Object{configuredConfigMap("2022-12-27")},
			LegacyTokenCleanUpPeriod: configuredLegacyTokenCleanUpPeriod("2022-12-29"),
			ExpectedActions: []core.Action{
				core.NewGetAction(schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}, metav1.NamespaceSystem, legacytokentracking.ConfigMapName),
			},
		},
		"secret-referenced service account does not exist": {
			ExistingSecret:           configuredServiceAccountTokenSecret("2022-12-27", "", "2022-12-27", "default", "12345", ""),
			ExistingPod:              configuredPod(false),
			ClientObjects:            []runtime.Object{configuredConfigMap("2022-12-27")},
			LegacyTokenCleanUpPeriod: configuredLegacyTokenCleanUpPeriod("2022-12-29"),
			ExpectedActions: []core.Action{
				core.NewGetAction(schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}, metav1.NamespaceSystem, legacytokentracking.ConfigMapName),
			},
		},
		"secret-referenced service account uid does not match": {
			ExistingSecret:           configuredServiceAccountTokenSecret("2022-12-27", "", "2022-12-27", "default", "123456", ""),
			ExistingServiceAccount:   serviceAccount(tokenSecretReferences()),
			ExistingPod:              configuredPod(false),
			ClientObjects:            []runtime.Object{configuredConfigMap("2022-12-27")},
			LegacyTokenCleanUpPeriod: configuredLegacyTokenCleanUpPeriod("2022-12-29"),
			ExpectedActions: []core.Action{
				core.NewGetAction(schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}, metav1.NamespaceSystem, legacytokentracking.ConfigMapName),
			},
		},
		"secret-referenced service account name is empty": {
			ExistingSecret:           configuredServiceAccountTokenSecret("2022-12-27", "", "2022-12-27", "", "12345", ""),
			ExistingServiceAccount:   serviceAccount(tokenSecretReferences()),
			ExistingPod:              configuredPod(false),
			ClientObjects:            []runtime.Object{configuredConfigMap("2022-12-27")},
			LegacyTokenCleanUpPeriod: configuredLegacyTokenCleanUpPeriod("2022-12-29"),
			ExpectedActions: []core.Action{
				core.NewGetAction(schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}, metav1.NamespaceSystem, legacytokentracking.ConfigMapName),
			},
		},
		"auto-generated secret does not have 'last-used' label, has not been marked as invalid": {
			ExistingSecret:           configuredServiceAccountTokenSecret("", "", "2022-12-27", "default", "12345", ""),
			ExistingServiceAccount:   serviceAccount(tokenSecretReferences()),
			ExistingPod:              configuredPod(false),
			ClientObjects:            []runtime.Object{configuredConfigMap("2022-12-28")},
			LegacyTokenCleanUpPeriod: configuredLegacyTokenCleanUpPeriod("2022-12-30"),
			ExpectedActions: []core.Action{
				core.NewGetAction(schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}, metav1.NamespaceSystem, legacytokentracking.ConfigMapName),
				core.NewPatchAction(
					schema.GroupVersionResource{Version: "v1", Resource: "secrets"},
					metav1.NamespaceDefault, "token-secret-1",
					types.MergePatchType,
					patchContent(metav1.NamespaceDefault, "token-secret-1", time.Now().UTC().Format(dateFormat), types.UID("23456")),
				),
			},
		},
		"auto-generated secret does not have 'last-used' label, has been marked as invalid, invalid_since label can not be parsed": {
			ExistingSecret:           configuredServiceAccountTokenSecret("", "2022-12-29-1", "2022-12-27", "default", "12345", ""),
			ExistingServiceAccount:   serviceAccount(tokenSecretReferences()),
			ExistingPod:              configuredPod(false),
			ClientObjects:            []runtime.Object{configuredConfigMap("2022-12-28")},
			LegacyTokenCleanUpPeriod: configuredLegacyTokenCleanUpPeriod("2022-12-30"),
			ExpectedActions: []core.Action{
				core.NewGetAction(schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}, metav1.NamespaceSystem, legacytokentracking.ConfigMapName),
				core.NewPatchAction(
					schema.GroupVersionResource{Version: "v1", Resource: "secrets"},
					metav1.NamespaceDefault, "token-secret-1",
					types.MergePatchType,
					patchContent(metav1.NamespaceDefault, "token-secret-1", time.Now().UTC().Format(dateFormat), types.UID("23456")),
				),
			},
		},
		"auto-generated secret does not have 'last-used' label, has been marked as invalid, time period since invalid is less than CleanUpPeriod": {
			ExistingSecret:           configuredServiceAccountTokenSecret("", "2023-01-01", "2022-12-27", "default", "12345", ""),
			ExistingServiceAccount:   serviceAccount(tokenSecretReferences()),
			ExistingPod:              configuredPod(false),
			ClientObjects:            []runtime.Object{configuredConfigMap("2022-12-28")},
			LegacyTokenCleanUpPeriod: configuredLegacyTokenCleanUpPeriod("2022-12-29"),
			ExpectedActions: []core.Action{
				core.NewGetAction(schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}, metav1.NamespaceSystem, legacytokentracking.ConfigMapName),
			},
		},
		"auto-generated secret does not have 'last-used' label, has been marked as invalid, time period since invalid is larger than CleanUpPeriod": {
			ExistingSecret:           configuredServiceAccountTokenSecret("", "2022-12-29", "2022-12-27", "default", "12345", ""),
			ExistingServiceAccount:   serviceAccount(tokenSecretReferences()),
			ExistingPod:              configuredPod(false),
			ClientObjects:            []runtime.Object{configuredConfigMap("2022-12-28")},
			LegacyTokenCleanUpPeriod: configuredLegacyTokenCleanUpPeriod("2023-01-01"),
			ExpectedActions: []core.Action{
				core.NewGetAction(schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}, metav1.NamespaceSystem, legacytokentracking.ConfigMapName),
				core.NewDeleteActionWithOptions(
					schema.GroupVersionResource{Version: "v1", Resource: "secrets"},
					metav1.NamespaceDefault, "token-secret-1",
					metav1.DeleteOptions{
						Preconditions: &metav1.Preconditions{ResourceVersion: &configuredServiceAccountTokenSecret("", "2022-12-29", "2022-12-27", "default", "12345", "").ResourceVersion},
					}),
			},
		},
		"auto-generated secret is mounted by the pod": {
			ExistingSecret:           configuredServiceAccountTokenSecret("2022-12-27", "", "2022-12-27", "default", "12345", ""),
			ExistingServiceAccount:   serviceAccount(tokenSecretReferences()),
			ExistingPod:              configuredPod(true),
			ClientObjects:            []runtime.Object{configuredConfigMap("2022-12-28")},
			LegacyTokenCleanUpPeriod: configuredLegacyTokenCleanUpPeriod("2022-12-29"),
			ExpectedActions: []core.Action{
				core.NewGetAction(schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}, metav1.NamespaceSystem, legacytokentracking.ConfigMapName),
			},
		},
		"auto-generated secret has 'last-used' label, the time period since last-used is larger than CleanUpPeriod, secret has not been marked as invalid": {
			ExistingSecret:           configuredServiceAccountTokenSecret("2022-12-27", "", "2022-12-27", "default", "12345", ""),
			ExistingServiceAccount:   serviceAccount(tokenSecretReferences()),
			ExistingPod:              configuredPod(false),
			ClientObjects:            []runtime.Object{configuredConfigMap("2022-12-28")},
			LegacyTokenCleanUpPeriod: configuredLegacyTokenCleanUpPeriod("2022-12-30"),
			ExpectedActions: []core.Action{
				core.NewGetAction(schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}, metav1.NamespaceSystem, legacytokentracking.ConfigMapName),
				core.NewPatchAction(
					schema.GroupVersionResource{Version: "v1", Resource: "secrets"},
					metav1.NamespaceDefault, "token-secret-1",
					types.MergePatchType,
					patchContent(metav1.NamespaceDefault, "token-secret-1", time.Now().UTC().Format(dateFormat), types.UID("23456")),
				),
			},
		},
		"auto-generated secret has 'last-used' label, the time period since last-used is larger than CleanUpPeriod, secret has been marked as invalid, time period since invalid is less than CleanUpPeriod": {
			ExistingSecret:           configuredServiceAccountTokenSecret("2022-12-27", "2023-05-01", "2022-12-27", "default", "12345", ""),
			ExistingServiceAccount:   serviceAccount(tokenSecretReferences()),
			ExistingPod:              configuredPod(false),
			ClientObjects:            []runtime.Object{configuredConfigMap("2022-12-28")},
			LegacyTokenCleanUpPeriod: configuredLegacyTokenCleanUpPeriod("2022-12-30"),
			ExpectedActions: []core.Action{
				core.NewGetAction(schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}, metav1.NamespaceSystem, legacytokentracking.ConfigMapName),
			},
		},
		"auto-generated secret has 'last-used' label, the time period since last-used is larger than CleanUpPeriod, secret has been marked as invalid, time period since invalid is larger than CleanUpPeriod": {
			ExistingSecret:           configuredServiceAccountTokenSecret("2022-12-27", "2023-01-05", "2022-12-27", "default", "12345", ""),
			ExistingServiceAccount:   serviceAccount(tokenSecretReferences()),
			ExistingPod:              configuredPod(false),
			ClientObjects:            []runtime.Object{configuredConfigMap("2022-12-28")},
			LegacyTokenCleanUpPeriod: configuredLegacyTokenCleanUpPeriod("2023-05-01"),
			ExpectedActions: []core.Action{
				core.NewGetAction(schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}, metav1.NamespaceSystem, legacytokentracking.ConfigMapName),
				core.NewDeleteActionWithOptions(
					schema.GroupVersionResource{Version: "v1", Resource: "secrets"},
					metav1.NamespaceDefault, "token-secret-1",
					metav1.DeleteOptions{
						Preconditions: &metav1.Preconditions{ResourceVersion: &configuredServiceAccountTokenSecret("", "2023-01-05", "2022-12-27", "default", "12345", "").ResourceVersion},
					}),
			},
		},
	}

	for k, tc := range testcases {
		t.Run(k, func(t *testing.T) {
			tc.ClientObjects = append(tc.ClientObjects, tc.ExistingSecret)
			client := fake.NewSimpleClientset(tc.ClientObjects...)

			informers := informers.NewSharedInformerFactory(client, controller.NoResyncPeriodFunc())
			secretInformer := informers.Core().V1().Secrets()
			saInformer := informers.Core().V1().ServiceAccounts()
			podInformer := informers.Core().V1().Pods()
			secrets := secretInformer.Informer().GetStore()
			serviceAccounts := saInformer.Informer().GetStore()
			pods := podInformer.Informer().GetStore()
			options := LegacySATokenCleanerOptions{
				SyncInterval:  30 * time.Second,
				CleanUpPeriod: tc.LegacyTokenCleanUpPeriod,
			}
			cleaner, _ := NewLegacySATokenCleaner(saInformer, secretInformer, podInformer, client, testingclock.NewFakeClock(time.Now().UTC()), options)

			if tc.ExistingServiceAccount != nil {
				serviceAccounts.Add(tc.ExistingServiceAccount)
			}
			if tc.ExistingPod != nil {
				pods.Add(tc.ExistingPod)
			}
			secrets.Add(tc.ExistingSecret)

			ctx := context.TODO()
			cleaner.evaluateSATokens(ctx)

			actions := client.Actions()
			if len(actions) != len(tc.ExpectedActions) {
				t.Fatalf("got %d actions, wanted %d actions", len(actions), len(tc.ExpectedActions))
			}
			for i, action := range actions {
				if len(tc.ExpectedActions) < i+1 {
					t.Errorf("%s: %d unexpected actions: %+v", k, len(actions)-len(tc.ExpectedActions), actions[i:])
					break
				}
				expectedAction := tc.ExpectedActions[i]
				if !reflect.DeepEqual(expectedAction, action) {
					t.Errorf("got action %#v, wanted %v", action, expectedAction)
				}
			}
		})
	}
}
