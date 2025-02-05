/*
Copyright 2017 The Kubernetes Authors.

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

package apiserver

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/kubernetes/test/integration/framework"
)

// Tests that the apiserver retries patches
func TestPatchConflicts(t *testing.T) {
	ctx, clientSet, _, tearDownFn := setup(t)
	defer tearDownFn()

	ns := framework.CreateNamespaceOrDie(clientSet, "status-code", t)
	defer framework.DeleteNamespaceOrDie(clientSet, ns, t)

	// Performance depends on the system on which the test runs.
	// In the Prow CI, more than 100 concurrent patches were possible
	// when not using race detection. With race detection, 63 were
	// possible.
	numOfConcurrentPatches := 50

	UIDs := make([]types.UID, numOfConcurrentPatches)
	ownerRefs := []metav1.OwnerReference{}
	for i := 0; i < numOfConcurrentPatches; i++ {
		uid := types.UID(uuid.New().String())
		ownerName := fmt.Sprintf("owner-%d", i)
		UIDs[i] = uid
		ownerRefs = append(ownerRefs, metav1.OwnerReference{
			APIVersion: "example.com/v1",
			Kind:       "Foo",
			Name:       ownerName,
			UID:        uid,
		})
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "test",
			OwnerReferences: ownerRefs,
		},
	}

	// Create the object we're going to conflict on
	_, err := clientSet.CoreV1().Secrets(ns.Name).Create(ctx, secret, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	client := clientSet.CoreV1().RESTClient()

	successes := int32(0)

	// Run a lot of simultaneous patch operations to exercise internal API server retry of application of patches that do not specify resourceVersion.
	// They should all succeed.
	wg := sync.WaitGroup{}
	for i := 0; i < numOfConcurrentPatches; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			labelName := fmt.Sprintf("label-%d", i)
			value := uuid.New().String()

			obj, err := client.Patch(types.StrategicMergePatchType).
				Namespace(ns.Name).
				Resource("secrets").
				Name("test").
				Body([]byte(fmt.Sprintf(`{"metadata":{"labels":{"%s":"%s"}, "ownerReferences":[{"$patch":"delete","uid":"%s"}]}}`, labelName, value, UIDs[i]))).
				Do(ctx).
				Get()

			if apierrors.IsConflict(err) {
				t.Logf("tolerated conflict error patching %s: %v", "secrets", err)
				return
			}
			if err != nil {
				t.Errorf("error patching %s: %v", "secrets", err)
				return
			}

			accessor, err := meta.Accessor(obj)
			if err != nil {
				t.Errorf("error getting object from %s: %v", "secrets", err)
				return
			}
			// make sure the label we wanted was effective
			if accessor.GetLabels()[labelName] != value {
				t.Errorf("patch of %s was ineffective, expected %s=%s, got labels %#v", "secrets", labelName, value, accessor.GetLabels())
				return
			}
			// make sure the patch directive didn't get lost, and that an entry in the ownerReference list was deleted.
			found := findOwnerRefByUID(accessor.GetOwnerReferences(), UIDs[i])
			if found {
				t.Errorf("patch of %s with $patch directive was ineffective, didn't delete the entry in the ownerReference slice: %#v", "secrets", UIDs[i])
			}

			atomic.AddInt32(&successes, 1)
		}(i)
	}
	wg.Wait()

	if successes < int32(numOfConcurrentPatches) {
		t.Errorf("Expected at least %d successful patches for %s, got %d", numOfConcurrentPatches, "secrets", successes)
	} else {
		t.Logf("Got %d successful patches for %s", successes, "secrets")
	}

}

func findOwnerRefByUID(ownerRefs []metav1.OwnerReference, uid types.UID) bool {
	for _, of := range ownerRefs {
		if of.UID == uid {
			return true
		}
	}
	return false
}

// Shows that a strategic merge patch with a nested patch which is merged
// with an empty slice is handled property
// https://github.com/kubernetes/kubernetes/issues/117470
func TestNestedStrategicMergePatchWithEmpty(t *testing.T) {
	ctx, clientSet, _, tearDownFn := setup(t)
	defer tearDownFn()

	url := "https://foo.com"
	se := admissionregistrationv1.SideEffectClassNone

	_, err := clientSet.
		AdmissionregistrationV1().
		ValidatingWebhookConfigurations().
		Create(
			ctx,
			&admissionregistrationv1.ValidatingWebhookConfiguration{
				ObjectMeta: metav1.ObjectMeta{
					Name: "base-validation",
				},
				Webhooks: []admissionregistrationv1.ValidatingWebhook{
					{
						AdmissionReviewVersions: []string{"v1"},
						ClientConfig:            admissionregistrationv1.WebhookClientConfig{URL: &url},
						Name:                    "foo.bar.com",
						SideEffects:             &se,
					},
				},
			},
			metav1.CreateOptions{
				FieldManager:    "kubectl-client-side-apply",
				FieldValidation: metav1.FieldValidationStrict,
			},
		)
	require.NoError(t, err)

	_, err = clientSet.
		AdmissionregistrationV1().
		ValidatingWebhookConfigurations().
		Patch(
			ctx,
			"base-validation",
			types.StrategicMergePatchType,
			[]byte(`
	{
		"webhooks": null
	}
`),
			metav1.PatchOptions{
				FieldManager:    "kubectl-edit",
				FieldValidation: metav1.FieldValidationStrict,
			},
		)
	require.NoError(t, err)

	// Try to apply a patch to the webhook
	_, err = clientSet.
		AdmissionregistrationV1().
		ValidatingWebhookConfigurations().
		Patch(
			ctx,
			"base-validation",
			types.StrategicMergePatchType,
			[]byte(`{"$setElementOrder/webhooks":[{"name":"new.foo.com"}],"metadata":{"annotations":{"kubectl.kubernetes.io/last-applied-configuration":"{\"apiVersion\":\"admissionregistration.k8s.io/v1\",\"kind\":\"ValidatingWebhookConfiguration\",\"metadata\":{\"annotations\":{},\"name\":\"base-validation\"},\"webhooks\":[{\"admissionReviewVersions\":[\"v1\"],\"clientConfig\":{\"url\":\"https://foo.com\"},\"name\":\"new.foo.com\",\"sideEffects\":\"None\"}]}\n"}},"webhooks":[{"admissionReviewVersions":["v1"],"clientConfig":{"url":"https://foo.com"},"name":"new.foo.com","sideEffects":"None"},{"$patch":"delete","name":"foo.bar.com"}]}`),
			metav1.PatchOptions{
				FieldManager:    "kubectl-client-side-apply",
				FieldValidation: metav1.FieldValidationStrict,
			},
		)
	require.NoError(t, err)
}
