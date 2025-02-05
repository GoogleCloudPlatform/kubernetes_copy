/*
Copyright 2019 The Kubernetes Authors.

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

package conversion

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/apimachinery/pkg/util/wait"
	etcd3watcher "k8s.io/apiserver/pkg/storage/etcd3"
	"k8s.io/client-go/dynamic"
	_ "k8s.io/component-base/logs/testinit" // enable logging flags

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextensionsv1beta1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1beta1"
	"k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	serveroptions "k8s.io/apiextensions-apiserver/pkg/cmd/server/options"
	"k8s.io/apiextensions-apiserver/test/integration/fixtures"
	"k8s.io/apiextensions-apiserver/test/integration/storage"
)

type Checker func(t *testing.T, ctc *conversionTestContext)

func checks(checkers ...Checker) []Checker {
	return checkers
}

func TestWebhookConverterWithWatchCache(t *testing.T) {
	testWebhookConverter(t, true)
}
func TestWebhookConverterWithoutWatchCache(t *testing.T) {
	testWebhookConverter(t, false)
}

func testWebhookConverter(t *testing.T, watchCache bool) {
	tests := []struct {
		group          string
		handler        http.Handler
		reviewVersions []string
		checks         []Checker
	}{
		{
			group:          "noop-converter-v1",
			handler:        NewObjectConverterWebhookHandler(t, noopConverter),
			reviewVersions: []string{"v1", "v1beta1"},
			checks:         checks(validateStorageVersion, validateServed, validateMixedStorageVersions("v1alpha1", "v1beta1")), // no v1beta2 as the schema differs
		},
		{
			group:          "noop-converter-v1beta1",
			handler:        NewObjectConverterWebhookHandler(t, noopConverter),
			reviewVersions: []string{"v1beta1", "v1"},
			checks:         checks(validateStorageVersion, validateServed, validateMixedStorageVersions("v1alpha1", "v1beta1")), // no v1beta2 as the schema differs
		},
		{
			group:          "nontrivial-converter-v1",
			handler:        NewObjectConverterWebhookHandler(t, nontrivialConverter),
			reviewVersions: []string{"v1", "v1beta1"},
			checks:         checks(validateStorageVersion, validateServed, validateMixedStorageVersions("v1alpha1", "v1beta1", "v1beta2"), validateNonTrivialConverted, validateNonTrivialConvertedList, validateStoragePruning, validateDefaulting),
		},
		{
			group:          "nontrivial-converter-v1beta1",
			handler:        NewObjectConverterWebhookHandler(t, nontrivialConverter),
			reviewVersions: []string{"v1beta1", "v1"},
			checks:         checks(validateStorageVersion, validateServed, validateMixedStorageVersions("v1alpha1", "v1beta1", "v1beta2"), validateNonTrivialConverted, validateNonTrivialConvertedList, validateStoragePruning, validateDefaulting),
		},
		{
			group:          "metadata-mutating-v1",
			handler:        NewObjectConverterWebhookHandler(t, metadataMutatingConverter),
			reviewVersions: []string{"v1", "v1beta1"},
			checks:         checks(validateObjectMetaMutation),
		},
		{
			group:          "metadata-mutating-v1beta1",
			handler:        NewObjectConverterWebhookHandler(t, metadataMutatingConverter),
			reviewVersions: []string{"v1beta1", "v1"},
			checks:         checks(validateObjectMetaMutation),
		},
		{
			group:          "metadata-uid-mutating-v1",
			handler:        NewObjectConverterWebhookHandler(t, uidMutatingConverter),
			reviewVersions: []string{"v1", "v1beta1"},
			checks:         checks(validateUIDMutation),
		},
		{
			group:          "metadata-uid-mutating-v1beta1",
			handler:        NewObjectConverterWebhookHandler(t, uidMutatingConverter),
			reviewVersions: []string{"v1beta1", "v1"},
			checks:         checks(validateUIDMutation),
		},
		{
			group:          "empty-response-v1",
			handler:        NewReviewWebhookHandler(t, nil, emptyV1ResponseConverter),
			reviewVersions: []string{"v1", "v1beta1"},
			checks:         checks(expectConversionFailureMessage("empty-response", "returned 0 objects, expected 1")),
		},
		{
			group:          "empty-response-v1beta1",
			handler:        NewReviewWebhookHandler(t, emptyV1Beta1ResponseConverter, nil),
			reviewVersions: []string{"v1beta1", "v1"},
			checks:         checks(expectConversionFailureMessage("empty-response", "returned 0 objects, expected 1")),
		},
		{
			group:          "failure-message-v1",
			handler:        NewReviewWebhookHandler(t, nil, failureV1ResponseConverter("custom webhook conversion error")),
			reviewVersions: []string{"v1", "v1beta1"},
			checks:         checks(expectConversionFailureMessage("failure-message", "custom webhook conversion error")),
		},
		{
			group:          "failure-message-v1beta1",
			handler:        NewReviewWebhookHandler(t, failureV1Beta1ResponseConverter("custom webhook conversion error"), nil),
			reviewVersions: []string{"v1beta1", "v1"},
			checks:         checks(expectConversionFailureMessage("failure-message", "custom webhook conversion error")),
		},
		{
			group:          "unhandled-v1",
			handler:        NewReviewWebhookHandler(t, nil, nil),
			reviewVersions: []string{"v1", "v1beta1"},
			checks:         checks(expectConversionFailureMessage("server-error", "the server rejected our request")),
		},
		{
			group:          "unhandled-v1beta1",
			handler:        NewReviewWebhookHandler(t, nil, nil),
			reviewVersions: []string{"v1beta1", "v1"},
			checks:         checks(expectConversionFailureMessage("server-error", "the server rejected our request")),
		},
	}

	// KUBE_APISERVER_SERVE_REMOVED_APIS_FOR_ONE_RELEASE allows for APIs pending removal to not block tests
	t.Setenv("KUBE_APISERVER_SERVE_REMOVED_APIS_FOR_ONE_RELEASE", "true")

	// TODO: Added for integration testing of conversion webhooks, where decode errors due to conversion webhook failures need to be tested.
	// Maybe we should identify conversion webhook related errors in decoding to avoid triggering this? Or maybe having this special casing
	// of test cases in production code should be removed?
	etcd3watcher.TestOnlySetFatalOnDecodeError(false)
	defer etcd3watcher.TestOnlySetFatalOnDecodeError(true)

	tearDown, config, options, err := fixtures.StartDefaultServer(t, fmt.Sprintf("--watch-cache=%v", watchCache))
	if err != nil {
		t.Fatal(err)
	}

	apiExtensionsClient, err := clientset.NewForConfig(config)
	if err != nil {
		tearDown()
		t.Fatal(err)
	}

	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		tearDown()
		t.Fatal(err)
	}
	defer tearDown()

	crd := multiVersionFixture.DeepCopy()

	RESTOptionsGetter := serveroptions.NewCRDRESTOptionsGetter(*options.RecommendedOptions.Etcd, nil, nil)
	restOptions, err := RESTOptionsGetter.GetRESTOptions(schema.GroupResource{Group: crd.Spec.Group, Resource: crd.Spec.Names.Plural}, nil)
	if err != nil {
		t.Fatal(err)
	}
	etcdClient, _, err := storage.GetEtcdClients(restOptions.StorageConfig.Transport)
	if err != nil {
		t.Fatal(err)
	}
	defer etcdClient.Close()

	etcdObjectReader := storage.NewEtcdObjectReader(etcdClient, &restOptions, crd)
	ctcTearDown, ctc := newConversionTestContext(t, apiExtensionsClient, dynamicClient, etcdObjectReader, crd)
	defer ctcTearDown()

	// read only object to read at a different version than stored when we need to force conversion
	marker, err := ctc.versionedClient("marker", "v1beta1").Create(context.TODO(), newConversionMultiVersionFixture("marker", "marker", "v1beta1"), metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}

	for _, test := range tests {
		t.Run(test.group, func(t *testing.T) {
			upCh, handler := closeOnCall(test.handler)
			tearDown, webhookClientConfig, err := StartConversionWebhookServer(handler)
			if err != nil {
				t.Fatal(err)
			}
			defer tearDown()

			ctc.setConversionWebhook(t, webhookClientConfig, test.reviewVersions)
			defer ctc.removeConversionWebhook(t)

			// wait until new webhook is called the first time
			if err := wait.PollUntilContextTimeout(context.Background(), time.Millisecond*100, wait.ForeverTestTimeout, true, func(ctx context.Context) (done bool, err error) {
				_, getErr := ctc.versionedClient(marker.GetNamespace(), "v1alpha1").Get(ctx, marker.GetName(), metav1.GetOptions{})
				select {
				case <-upCh:
					return true, nil
				default:
					t.Logf("Waiting for webhook to become effective, getting marker object: %v", getErr)
					return false, nil
				}
			}); err != nil {
				t.Fatal(err)
			}

			for i, checkFn := range test.checks {
				name := fmt.Sprintf("check-%d", i)
				t.Run(name, func(t *testing.T) {
					defer ctc.setAndWaitStorageVersion(t, "v1beta1")
					ctc.namespace = fmt.Sprintf("webhook-conversion-%s-%s", test.group, name)
					checkFn(t, ctc)
				})
			}
		})
	}
}

func validateStorageVersion(t *testing.T, ctc *conversionTestContext) {
	ns := ctc.namespace

	for _, version := range ctc.crd.Spec.Versions {
		t.Run(version.Name, func(t *testing.T) {
			name := "storageversion-" + version.Name
			client := ctc.versionedClient(ns, version.Name)
			obj, err := client.Create(context.TODO(), newConversionMultiVersionFixture(ns, name, version.Name), metav1.CreateOptions{})
			if err != nil {
				t.Fatal(err)
			}
			ctc.setAndWaitStorageVersion(t, "v1beta2")

			if _, err = client.Get(context.TODO(), obj.GetName(), metav1.GetOptions{}); err != nil {
				t.Fatal(err)
			}

			ctc.setAndWaitStorageVersion(t, "v1beta1")
		})
	}
}

// validateMixedStorageVersions ensures that identical custom resources written at different storage versions
// are readable and remain the same.
func validateMixedStorageVersions(versions ...string) func(t *testing.T, ctc *conversionTestContext) {
	return func(t *testing.T, ctc *conversionTestContext) {
		ns := ctc.namespace
		clients := ctc.versionedClients(ns)

		// Create CRs at all storage versions
		objNames := []string{}
		for _, version := range versions {
			ctc.setAndWaitStorageVersion(t, version)

			name := "mixedstorage-stored-as-" + version
			obj, err := clients[version].Create(context.TODO(), newConversionMultiVersionFixture(ns, name, version), metav1.CreateOptions{})
			if err != nil {
				t.Fatal(err)
			}
			objNames = append(objNames, obj.GetName())
		}

		// Ensure copies of an object have the same fields and values at each custom resource definition version regardless of storage version
		for clientVersion, client := range clients {
			t.Run(clientVersion, func(t *testing.T) {
				o1, err := client.Get(context.TODO(), objNames[0], metav1.GetOptions{})
				if err != nil {
					t.Fatal(err)
				}
				for _, objName := range objNames[1:] {
					o2, err := client.Get(context.TODO(), objName, metav1.GetOptions{})
					if err != nil {
						t.Fatal(err)
					}

					// ignore metadata for comparison purposes
					delete(o1.Object, "metadata")
					delete(o2.Object, "metadata")
					if !reflect.DeepEqual(o1.Object, o2.Object) {
						t.Errorf("Expected custom resource to be same regardless of which storage version is used to create, but got: %s", cmp.Diff(o1, o2))
					}
				}
			})
		}
	}
}

func validateServed(t *testing.T, ctc *conversionTestContext) {
	ns := ctc.namespace

	for _, version := range ctc.crd.Spec.Versions {
		t.Run(version.Name, func(t *testing.T) {
			name := "served-" + version.Name
			client := ctc.versionedClient(ns, version.Name)
			obj, err := client.Create(context.TODO(), newConversionMultiVersionFixture(ns, name, version.Name), metav1.CreateOptions{})
			if err != nil {
				t.Fatal(err)
			}
			ctc.setServed(t, version.Name, false)
			ctc.waitForServed(t, version.Name, false, client, obj)
			ctc.setServed(t, version.Name, true)
			ctc.waitForServed(t, version.Name, true, client, obj)
		})
	}
}

func validateNonTrivialConverted(t *testing.T, ctc *conversionTestContext) {
	ns := ctc.namespace

	for _, createVersion := range ctc.crd.Spec.Versions {
		t.Run(fmt.Sprintf("getting objects created as %s", createVersion.Name), func(t *testing.T) {
			name := "converted-" + createVersion.Name
			client := ctc.versionedClient(ns, createVersion.Name)

			fixture := newConversionMultiVersionFixture(ns, name, createVersion.Name)
			if err := unstructured.SetNestedField(fixture.Object, "foo", "garbage"); err != nil {
				t.Fatal(err)
			}
			if _, err := client.Create(context.TODO(), fixture, metav1.CreateOptions{}); err != nil {
				t.Fatal(err)
			}

			// verify that the right, pruned version is in storage
			obj, err := ctc.etcdObjectReader.GetStoredCustomResource(ns, name)
			if err != nil {
				t.Fatal(err)
			}
			verifyMultiVersionObject(t, "v1beta1", obj)

			for _, getVersion := range ctc.crd.Spec.Versions {
				client := ctc.versionedClient(ns, getVersion.Name)
				obj, err := client.Get(context.TODO(), name, metav1.GetOptions{})
				if err != nil {
					t.Fatal(err)
				}
				verifyMultiVersionObject(t, getVersion.Name, obj)
			}

			// send a non-trivial patch to the main resource to verify the oldObject is in the right version
			if _, err := client.Patch(context.TODO(), name, types.MergePatchType, []byte(`{"metadata":{"annotations":{"main":"true"}}}`), metav1.PatchOptions{}); err != nil {
				t.Fatal(err)
			}
			// verify that the right, pruned version is in storage
			obj, err = ctc.etcdObjectReader.GetStoredCustomResource(ns, name)
			if err != nil {
				t.Fatal(err)
			}
			verifyMultiVersionObject(t, "v1beta1", obj)

			// send a non-trivial patch to the status subresource to verify the oldObject is in the right version
			if _, err := client.Patch(context.TODO(), name, types.MergePatchType, []byte(`{"metadata":{"annotations":{"status":"true"}}}`), metav1.PatchOptions{}, "status"); err != nil {
				t.Fatal(err)
			}
			// verify that the right, pruned version is in storage
			obj, err = ctc.etcdObjectReader.GetStoredCustomResource(ns, name)
			if err != nil {
				t.Fatal(err)
			}
			verifyMultiVersionObject(t, "v1beta1", obj)
		})
	}
}

func validateNonTrivialConvertedList(t *testing.T, ctc *conversionTestContext) {
	ns := ctc.namespace + "-list"

	names := sets.String{}
	for _, createVersion := range ctc.crd.Spec.Versions {
		name := "converted-" + createVersion.Name
		client := ctc.versionedClient(ns, createVersion.Name)
		fixture := newConversionMultiVersionFixture(ns, name, createVersion.Name)
		if err := unstructured.SetNestedField(fixture.Object, "foo", "garbage"); err != nil {
			t.Fatal(err)
		}
		_, err := client.Create(context.TODO(), fixture, metav1.CreateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		names.Insert(name)
	}

	for _, listVersion := range ctc.crd.Spec.Versions {
		t.Run(fmt.Sprintf("listing objects as %s", listVersion.Name), func(t *testing.T) {
			client := ctc.versionedClient(ns, listVersion.Name)
			obj, err := client.List(context.TODO(), metav1.ListOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if len(obj.Items) != len(ctc.crd.Spec.Versions) {
				t.Fatal("unexpected number of items")
			}
			foundNames := sets.String{}
			for _, u := range obj.Items {
				foundNames.Insert(u.GetName())
				verifyMultiVersionObject(t, listVersion.Name, &u)
			}
			if !foundNames.Equal(names) {
				t.Errorf("unexpected set of returned items: %s", foundNames.Difference(names))
			}
		})
	}
}

func validateStoragePruning(t *testing.T, ctc *conversionTestContext) {
	ns := ctc.namespace

	for _, createVersion := range ctc.crd.Spec.Versions {
		t.Run(fmt.Sprintf("getting objects created as %s", createVersion.Name), func(t *testing.T) {
			name := "storagepruning-" + createVersion.Name
			client := ctc.versionedClient(ns, createVersion.Name)

			fixture := newConversionMultiVersionFixture(ns, name, createVersion.Name)
			if err := unstructured.SetNestedField(fixture.Object, "foo", "garbage"); err != nil {
				t.Fatal(err)
			}
			_, err := client.Create(context.TODO(), fixture, metav1.CreateOptions{})
			if err != nil {
				t.Fatal(err)
			}

			// verify that the right, pruned version is in storage
			obj, err := ctc.etcdObjectReader.GetStoredCustomResource(ns, name)
			if err != nil {
				t.Fatal(err)
			}
			verifyMultiVersionObject(t, "v1beta1", obj)

			// add garbage and set a label
			if err := unstructured.SetNestedField(obj.Object, "foo", "garbage"); err != nil {
				t.Fatal(err)
			}
			labels := obj.GetLabels()
			if labels == nil {
				labels = map[string]string{}
			}
			labels["mutated"] = "true"
			obj.SetLabels(labels)
			if err := ctc.etcdObjectReader.SetStoredCustomResource(ns, name, obj); err != nil {
				t.Fatal(err)
			}

			for _, getVersion := range ctc.crd.Spec.Versions {
				client := ctc.versionedClient(ns, getVersion.Name)
				obj, err := client.Get(context.TODO(), name, metav1.GetOptions{})
				if err != nil {
					t.Fatal(err)
				}

				// check that the direct mutation in etcd worked
				labels := obj.GetLabels()
				if labels["mutated"] != "true" {
					t.Errorf("expected object %s in version %s to have label 'mutated=true'", name, getVersion.Name)
				}

				verifyMultiVersionObject(t, getVersion.Name, obj)
			}
		})
	}
}

func validateObjectMetaMutation(t *testing.T, ctc *conversionTestContext) {
	ns := ctc.namespace

	t.Logf("Creating object in storage version v1beta1")
	storageVersion := "v1beta1"
	ctc.setAndWaitStorageVersion(t, storageVersion)
	name := "objectmeta-mutation-" + storageVersion
	client := ctc.versionedClient(ns, storageVersion)
	obj, err := client.Create(context.TODO(), newConversionMultiVersionFixture(ns, name, storageVersion), metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	validateObjectMetaMutationObject(t, false, false, obj)

	t.Logf("Getting object in other version v1beta2")
	client = ctc.versionedClient(ns, "v1beta2")
	obj, err = client.Get(context.TODO(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	validateObjectMetaMutationObject(t, true, true, obj)

	t.Logf("Creating object in non-storage version")
	name = "objectmeta-mutation-v1beta2"
	client = ctc.versionedClient(ns, "v1beta2")
	obj, err = client.Create(context.TODO(), newConversionMultiVersionFixture(ns, name, "v1beta2"), metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	validateObjectMetaMutationObject(t, true, true, obj)

	t.Logf("Listing objects in non-storage version")
	client = ctc.versionedClient(ns, "v1beta2")
	list, err := client.List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, obj := range list.Items {
		validateObjectMetaMutationObject(t, true, true, &obj)
	}
}

func validateObjectMetaMutationObject(t *testing.T, expectAnnotations, expectLabels bool, obj *unstructured.Unstructured) {
	if expectAnnotations {
		if _, found := obj.GetAnnotations()["from"]; !found {
			t.Errorf("expected 'from=stable.example.com/v1beta1' annotation")
		}
		if _, found := obj.GetAnnotations()["to"]; !found {
			t.Errorf("expected 'to=stable.example.com/v1beta2' annotation")
		}
	} else {
		if v, found := obj.GetAnnotations()["from"]; found {
			t.Errorf("unexpected 'from' annotation: %s", v)
		}
		if v, found := obj.GetAnnotations()["to"]; found {
			t.Errorf("unexpected 'to' annotation: %s", v)
		}
	}
	if expectLabels {
		if _, found := obj.GetLabels()["from"]; !found {
			t.Errorf("expected 'from=stable.example.com.v1beta1' label")
		}
		if _, found := obj.GetLabels()["to"]; !found {
			t.Errorf("expected 'to=stable.example.com.v1beta2' label")
		}
	} else {
		if v, found := obj.GetLabels()["from"]; found {
			t.Errorf("unexpected 'from' label: %s", v)
		}
		if v, found := obj.GetLabels()["to"]; found {
			t.Errorf("unexpected 'to' label: %s", v)
		}
	}
	if sets.NewString(obj.GetFinalizers()...).Has("foo") {
		t.Errorf("unexpected 'foo' finalizer")
	}
	if obj.GetGeneration() == 42 {
		t.Errorf("unexpected generation 42")
	}
	if v, found, err := unstructured.NestedString(obj.Object, "metadata", "garbage"); err != nil {
		t.Errorf("unexpected error accessing 'metadata.garbage': %v", err)
	} else if found {
		t.Errorf("unexpected 'metadata.garbage': %s", v)
	}
}

func validateUIDMutation(t *testing.T, ctc *conversionTestContext) {
	ns := ctc.namespace

	t.Logf("Creating object in non-storage version v1beta1")
	storageVersion := "v1beta1"
	ctc.setAndWaitStorageVersion(t, storageVersion)
	name := "uid-mutation-" + storageVersion
	client := ctc.versionedClient(ns, "v1beta2")
	obj, err := client.Create(context.TODO(), newConversionMultiVersionFixture(ns, name, "v1beta2"), metav1.CreateOptions{})
	if err == nil {
		t.Fatalf("expected creation error, but got: %v", obj)
	} else if !strings.Contains(err.Error(), "must have the same UID") {
		t.Errorf("expected 'must have the same UID' error message, but got: %v", err)
	}
}

func validateDefaulting(t *testing.T, ctc *conversionTestContext) {
	if _, defaulting := ctc.crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["defaults"]; !defaulting {
		return
	}

	ns := ctc.namespace
	storageVersion := "v1beta1"

	for _, createVersion := range ctc.crd.Spec.Versions {
		t.Run(fmt.Sprintf("getting objects created as %s", createVersion.Name), func(t *testing.T) {
			name := "defaulting-" + createVersion.Name
			client := ctc.versionedClient(ns, createVersion.Name)

			fixture := newConversionMultiVersionFixture(ns, name, createVersion.Name)
			if err := unstructured.SetNestedField(fixture.Object, map[string]interface{}{}, "defaults"); err != nil {
				t.Fatal(err)
			}
			created, err := client.Create(context.TODO(), fixture, metav1.CreateOptions{})
			if err != nil {
				t.Fatal(err)
			}

			// check that defaulting happens
			// - in the request version when doing no-op conversion when deserializing
			// - when reading back from storage in the storage version
			// only the first is persisted.
			defaults, found, err := unstructured.NestedMap(created.Object, "defaults")
			if err != nil {
				t.Fatal(err)
			} else if !found {
				t.Fatalf("expected .defaults to exist")
			}
			expectedLen := 1
			if !createVersion.Storage {
				expectedLen++
			}
			if len(defaults) != expectedLen {
				t.Fatalf("after %s create expected .defaults to have %d values, but got: %v", createVersion.Name, expectedLen, defaults)
			}
			if _, found := defaults[createVersion.Name].(bool); !found {
				t.Errorf("after %s create expected .defaults[%s] to be true, but .defaults is: %v", createVersion.Name, createVersion.Name, defaults)
			}
			if _, found := defaults[storageVersion].(bool); !found {
				t.Errorf("after %s create expected .defaults[%s] to be true because it is the storage version, but .defaults is: %v", createVersion.Name, storageVersion, defaults)
			}

			// verify that only the request version default is persisted
			persisted, err := ctc.etcdObjectReader.GetStoredCustomResource(ns, name)
			if err != nil {
				t.Fatal(err)
			}
			if _, found, err := unstructured.NestedBool(persisted.Object, "defaults", storageVersion); err != nil {
				t.Fatal(err)
			} else if createVersion.Name != storageVersion && found {
				t.Errorf("after %s create .defaults[storage version %s] not to be persisted, but got in etcd: %v", createVersion.Name, storageVersion, defaults)
			}

			// check that when reading any other version, we do not default that version, but only the (non-persisted) storage version default
			for _, v := range ctc.crd.Spec.Versions {
				if v.Name == createVersion.Name {
					// create version is persisted anyway, nothing to verify
					continue
				}

				got, err := ctc.versionedClient(ns, v.Name).Get(context.TODO(), created.GetName(), metav1.GetOptions{})
				if err != nil {
					t.Fatal(err)
				}

				if _, found, err := unstructured.NestedBool(got.Object, "defaults", v.Name); err != nil {
					t.Fatal(err)
				} else if v.Name != storageVersion && found {
					t.Errorf("after %s GET expected .defaults[%s] not to be true because only storage version %s is defaulted on read, but .defaults is: %v", v.Name, v.Name, storageVersion, defaults)
				}

				if _, found, err := unstructured.NestedBool(got.Object, "defaults", storageVersion); err != nil {
					t.Fatal(err)
				} else if !found {
					t.Errorf("after non-create, non-storage %s GET expected .defaults[storage version %s] to be true, but .defaults is: %v", v.Name, storageVersion, defaults)
				}
			}
		})
	}
}

func expectConversionFailureMessage(id, message string) func(t *testing.T, ctc *conversionTestContext) {
	return func(t *testing.T, ctc *conversionTestContext) {
		ns := ctc.namespace
		clients := ctc.versionedClients(ns)
		var err error
		// storage version is v1beta1, so this skips conversion
		obj, err := clients["v1beta1"].Create(context.TODO(), newConversionMultiVersionFixture(ns, id, "v1beta1"), metav1.CreateOptions{})
		if err != nil {
			t.Fatal(err)
		}

		// manually convert
		objv1beta2 := newConversionMultiVersionFixture(ns, id, "v1beta2")
		meta, _, _ := unstructured.NestedFieldCopy(obj.Object, "metadata")
		unstructured.SetNestedField(objv1beta2.Object, meta, "metadata")
		lastRV := objv1beta2.GetResourceVersion()

		for _, verb := range []string{"get", "list", "create", "update", "patch", "delete", "deletecollection"} {
			t.Run(verb, func(t *testing.T) {
				switch verb {
				case "get":
					_, err = clients["v1beta2"].Get(context.TODO(), obj.GetName(), metav1.GetOptions{})
				case "list":
					// With ResilientWatchCacheInitialization feature, List requests are rejected with 429 if watchcache is not initialized.
					// However, in some of these tests that install faulty converter webhook, watchcache will never initialize by definition (as list will never succeed due to faulty converter webook).
					// In such case, the returned error will differ from the one returned from the etcd, so we need to force the request to go to etcd.
					_, err = clients["v1beta2"].List(context.TODO(), metav1.ListOptions{ResourceVersion: lastRV, ResourceVersionMatch: metav1.ResourceVersionMatchExact})
				case "create":
					_, err = clients["v1beta2"].Create(context.TODO(), newConversionMultiVersionFixture(ns, id, "v1beta2"), metav1.CreateOptions{})
				case "update":
					_, err = clients["v1beta2"].Update(context.TODO(), objv1beta2, metav1.UpdateOptions{})
				case "patch":
					_, err = clients["v1beta2"].Patch(context.TODO(), obj.GetName(), types.MergePatchType, []byte(`{"metadata":{"annotations":{"patch":"true"}}}`), metav1.PatchOptions{})
				case "delete":
					err = clients["v1beta2"].Delete(context.TODO(), obj.GetName(), metav1.DeleteOptions{})
				case "deletecollection":
					err = clients["v1beta2"].DeleteCollection(context.TODO(), metav1.DeleteOptions{}, metav1.ListOptions{})
				default:
					t.Errorf("unknown verb %q", verb)
				}

				if err == nil {
					t.Errorf("expected error with message %s, but got no error", message)
				} else if !strings.Contains(err.Error(), message) {
					t.Errorf("expected error with message %s, but got %v", message, err)
				}
			})
		}
		for _, subresource := range []string{"status", "scale"} {
			for _, verb := range []string{"get", "update", "patch"} {
				t.Run(fmt.Sprintf("%s-%s", subresource, verb), func(t *testing.T) {
					switch verb {
					case "get":
						_, err = clients["v1beta2"].Get(context.TODO(), obj.GetName(), metav1.GetOptions{}, subresource)
					case "update":
						o := objv1beta2
						if subresource == "scale" {
							o = &unstructured.Unstructured{
								Object: map[string]interface{}{
									"apiVersion": "autoscaling/v1",
									"kind":       "Scale",
									"metadata": map[string]interface{}{
										"name": obj.GetName(),
									},
									"spec": map[string]interface{}{
										"replicas": 42,
									},
								},
							}
						}
						_, err = clients["v1beta2"].Update(context.TODO(), o, metav1.UpdateOptions{}, subresource)
					case "patch":
						_, err = clients["v1beta2"].Patch(context.TODO(), obj.GetName(), types.MergePatchType, []byte(`{"metadata":{"annotations":{"patch":"true"}}}`), metav1.PatchOptions{}, subresource)
					default:
						t.Errorf("unknown subresource verb %q", verb)
					}

					if err == nil {
						t.Errorf("expected error with message %s, but got no error", message)
					} else if !strings.Contains(err.Error(), message) {
						t.Errorf("expected error with message %s, but got %v", message, err)
					}
				})
			}
		}
	}
}

func noopConverter(desiredAPIVersion string, obj runtime.RawExtension) (runtime.RawExtension, error) {
	u := &unstructured.Unstructured{Object: map[string]interface{}{}}
	if err := json.Unmarshal(obj.Raw, u); err != nil {
		return runtime.RawExtension{}, fmt.Errorf("failed to deserialize object: %s with error: %v", string(obj.Raw), err)
	}
	u.Object["apiVersion"] = desiredAPIVersion
	raw, err := json.Marshal(u)
	if err != nil {
		return runtime.RawExtension{}, fmt.Errorf("failed to serialize object: %v with error: %v", u, err)
	}
	return runtime.RawExtension{Raw: raw}, nil
}

func emptyV1ResponseConverter(review *apiextensionsv1.ConversionReview) (*apiextensionsv1.ConversionReview, error) {
	review.Response = &apiextensionsv1.ConversionResponse{
		UID:              review.Request.UID,
		ConvertedObjects: []runtime.RawExtension{},
		Result:           metav1.Status{Status: "Success"},
	}
	return review, nil
}
func emptyV1Beta1ResponseConverter(review *apiextensionsv1beta1.ConversionReview) (*apiextensionsv1beta1.ConversionReview, error) {
	review.Response = &apiextensionsv1beta1.ConversionResponse{
		UID:              review.Request.UID,
		ConvertedObjects: []runtime.RawExtension{},
		Result:           metav1.Status{Status: "Success"},
	}
	return review, nil
}

func failureV1ResponseConverter(message string) func(review *apiextensionsv1.ConversionReview) (*apiextensionsv1.ConversionReview, error) {
	return func(review *apiextensionsv1.ConversionReview) (*apiextensionsv1.ConversionReview, error) {
		review.Response = &apiextensionsv1.ConversionResponse{
			UID:              review.Request.UID,
			ConvertedObjects: []runtime.RawExtension{},
			Result:           metav1.Status{Message: message, Status: "Failure"},
		}
		return review, nil
	}
}

func failureV1Beta1ResponseConverter(message string) func(review *apiextensionsv1beta1.ConversionReview) (*apiextensionsv1beta1.ConversionReview, error) {
	return func(review *apiextensionsv1beta1.ConversionReview) (*apiextensionsv1beta1.ConversionReview, error) {
		review.Response = &apiextensionsv1beta1.ConversionResponse{
			UID:              review.Request.UID,
			ConvertedObjects: []runtime.RawExtension{},
			Result:           metav1.Status{Message: message, Status: "Failure"},
		}
		return review, nil
	}
}

func nontrivialConverter(desiredAPIVersion string, obj runtime.RawExtension) (runtime.RawExtension, error) {
	u := &unstructured.Unstructured{Object: map[string]interface{}{}}
	if err := json.Unmarshal(obj.Raw, u); err != nil {
		return runtime.RawExtension{}, fmt.Errorf("failed to deserialize object: %s with error: %v", string(obj.Raw), err)
	}

	currentAPIVersion := u.GetAPIVersion()

	if currentAPIVersion == "stable.example.com/v1beta2" && (desiredAPIVersion == "stable.example.com/v1alpha1" || desiredAPIVersion == "stable.example.com/v1beta1") {
		u.Object["num"] = u.Object["numv2"]
		u.Object["content"] = u.Object["contentv2"]
		delete(u.Object, "numv2")
		delete(u.Object, "contentv2")
	} else if (currentAPIVersion == "stable.example.com/v1alpha1" || currentAPIVersion == "stable.example.com/v1beta1") && desiredAPIVersion == "stable.example.com/v1beta2" {
		u.Object["numv2"] = u.Object["num"]
		u.Object["contentv2"] = u.Object["content"]
		delete(u.Object, "num")
		delete(u.Object, "content")
	} else if currentAPIVersion == "stable.example.com/v1alpha1" && desiredAPIVersion == "stable.example.com/v1beta1" {
		// same schema
	} else if currentAPIVersion == "stable.example.com/v1beta1" && desiredAPIVersion == "stable.example.com/v1alpha1" {
		// same schema
	} else if currentAPIVersion != desiredAPIVersion {
		return runtime.RawExtension{}, fmt.Errorf("cannot convert from %s to %s", currentAPIVersion, desiredAPIVersion)
	}
	u.Object["apiVersion"] = desiredAPIVersion
	raw, err := json.Marshal(u)
	if err != nil {
		return runtime.RawExtension{}, fmt.Errorf("failed to serialize object: %v with error: %v", u, err)
	}
	return runtime.RawExtension{Raw: raw}, nil
}

func metadataMutatingConverter(desiredAPIVersion string, obj runtime.RawExtension) (runtime.RawExtension, error) {
	obj, err := nontrivialConverter(desiredAPIVersion, obj)
	if err != nil {
		return runtime.RawExtension{}, err
	}

	u := &unstructured.Unstructured{Object: map[string]interface{}{}}
	if err := json.Unmarshal(obj.Raw, u); err != nil {
		return runtime.RawExtension{}, fmt.Errorf("failed to deserialize object: %s with error: %v", string(obj.Raw), err)
	}

	// do not mutate the marker or the probe objects
	if !strings.Contains(u.GetName(), "mutation") {
		return obj, nil
	}

	currentAPIVersion := u.GetAPIVersion()

	// mutate annotations. This should be persisted.
	annotations := u.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations["from"] = currentAPIVersion
	annotations["to"] = desiredAPIVersion
	u.SetAnnotations(annotations)

	// mutate labels. This should be persisted.
	labels := u.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	labels["from"] = strings.Replace(currentAPIVersion, "/", ".", 1) // replace / with . because label values do not allow /
	labels["to"] = strings.Replace(desiredAPIVersion, "/", ".", 1)
	u.SetLabels(labels)

	// mutate other fields. This should be ignored.
	u.SetGeneration(42)
	u.SetOwnerReferences([]metav1.OwnerReference{{
		APIVersion:         "v1",
		Kind:               "Namespace",
		Name:               "default",
		UID:                "1234",
		Controller:         nil,
		BlockOwnerDeletion: nil,
	}})
	u.SetResourceVersion("42")
	u.SetFinalizers([]string{"foo"})
	if err := unstructured.SetNestedField(u.Object, "foo", "metadata", "garbage"); err != nil {
		return runtime.RawExtension{}, err
	}

	raw, err := json.Marshal(u)
	if err != nil {
		return runtime.RawExtension{}, fmt.Errorf("failed to serialize object: %v with error: %v", u, err)
	}
	return runtime.RawExtension{Raw: raw}, nil
}

func uidMutatingConverter(desiredAPIVersion string, obj runtime.RawExtension) (runtime.RawExtension, error) {
	u := &unstructured.Unstructured{Object: map[string]interface{}{}}
	if err := json.Unmarshal(obj.Raw, u); err != nil {
		return runtime.RawExtension{}, fmt.Errorf("failed to deserialize object: %s with error: %v", string(obj.Raw), err)
	}

	// do not mutate the marker or the probe objects
	if strings.Contains(u.GetName(), "mutation") {
		// mutate other fields. This should be ignored.
		if err := unstructured.SetNestedField(u.Object, "42", "metadata", "uid"); err != nil {
			return runtime.RawExtension{}, err
		}
	}

	u.Object["apiVersion"] = desiredAPIVersion
	raw, err := json.Marshal(u)
	if err != nil {
		return runtime.RawExtension{}, fmt.Errorf("failed to serialize object: %v with error: %v", u, err)
	}
	return runtime.RawExtension{Raw: raw}, nil
}

func newConversionTestContext(t *testing.T, apiExtensionsClient clientset.Interface, dynamicClient dynamic.Interface, etcdObjectReader *storage.EtcdObjectReader, v1CRD *apiextensionsv1.CustomResourceDefinition) (func(), *conversionTestContext) {
	v1CRD, err := fixtures.CreateNewV1CustomResourceDefinition(v1CRD, apiExtensionsClient, dynamicClient)
	if err != nil {
		t.Fatal(err)
	}
	crd, err := apiExtensionsClient.ApiextensionsV1().CustomResourceDefinitions().Get(context.TODO(), v1CRD.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}

	tearDown := func() {
		if err := fixtures.DeleteV1CustomResourceDefinition(crd, apiExtensionsClient); err != nil {
			t.Fatal(err)
		}
	}

	return tearDown, &conversionTestContext{apiExtensionsClient: apiExtensionsClient, dynamicClient: dynamicClient, crd: crd, etcdObjectReader: etcdObjectReader}
}

type conversionTestContext struct {
	namespace           string
	apiExtensionsClient clientset.Interface
	dynamicClient       dynamic.Interface
	crd                 *apiextensionsv1.CustomResourceDefinition
	etcdObjectReader    *storage.EtcdObjectReader
}

func (c *conversionTestContext) versionedClient(ns string, version string) dynamic.ResourceInterface {
	gvr := schema.GroupVersionResource{Group: c.crd.Spec.Group, Version: version, Resource: c.crd.Spec.Names.Plural}
	if c.crd.Spec.Scope != apiextensionsv1.ClusterScoped {
		return c.dynamicClient.Resource(gvr).Namespace(ns)
	}
	return c.dynamicClient.Resource(gvr)
}

func (c *conversionTestContext) versionedClients(ns string) map[string]dynamic.ResourceInterface {
	ret := map[string]dynamic.ResourceInterface{}
	for _, v := range c.crd.Spec.Versions {
		ret[v.Name] = c.versionedClient(ns, v.Name)
	}
	return ret
}

func (c *conversionTestContext) setConversionWebhook(t *testing.T, webhookClientConfig *apiextensionsv1.WebhookClientConfig, reviewVersions []string) {
	crd, err := c.apiExtensionsClient.ApiextensionsV1().CustomResourceDefinitions().Get(context.TODO(), c.crd.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	crd.Spec.Conversion = &apiextensionsv1.CustomResourceConversion{
		Strategy: apiextensionsv1.WebhookConverter,
		Webhook: &apiextensionsv1.WebhookConversion{
			ClientConfig:             webhookClientConfig,
			ConversionReviewVersions: reviewVersions,
		},
	}
	crd, err = c.apiExtensionsClient.ApiextensionsV1().CustomResourceDefinitions().Update(context.TODO(), crd, metav1.UpdateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	c.crd = crd

}

func (c *conversionTestContext) removeConversionWebhook(t *testing.T) {
	crd, err := c.apiExtensionsClient.ApiextensionsV1().CustomResourceDefinitions().Get(context.TODO(), c.crd.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	crd.Spec.Conversion = &apiextensionsv1.CustomResourceConversion{
		Strategy: apiextensionsv1.NoneConverter,
	}

	crd, err = c.apiExtensionsClient.ApiextensionsV1().CustomResourceDefinitions().Update(context.TODO(), crd, metav1.UpdateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	c.crd = crd
}

func (c *conversionTestContext) setAndWaitStorageVersion(t *testing.T, version string) {
	c.setStorageVersion(t, version)

	// create probe object. Version should be the default one to avoid webhook calls during test setup.
	client := c.versionedClient("probe", "v1beta1")
	name := fmt.Sprintf("probe-%v", uuid.NewUUID())
	storageProbe, err := client.Create(context.TODO(), newConversionMultiVersionFixture("probe", name, "v1beta1"), metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}

	// update object continuously and wait for etcd to have the target storage version.
	c.waitForStorageVersion(t, version, c.versionedClient(storageProbe.GetNamespace(), "v1beta1"), storageProbe)

	err = client.Delete(context.TODO(), name, metav1.DeleteOptions{})
	if err != nil {
		t.Fatal(err)
	}
}

func (c *conversionTestContext) setStorageVersion(t *testing.T, version string) {
	crd, err := c.apiExtensionsClient.ApiextensionsV1().CustomResourceDefinitions().Get(context.TODO(), c.crd.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for i, v := range crd.Spec.Versions {
		crd.Spec.Versions[i].Storage = v.Name == version
	}
	crd, err = c.apiExtensionsClient.ApiextensionsV1().CustomResourceDefinitions().Update(context.TODO(), crd, metav1.UpdateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	c.crd = crd
}

func (c *conversionTestContext) waitForStorageVersion(t *testing.T, version string, versionedClient dynamic.ResourceInterface, obj *unstructured.Unstructured) *unstructured.Unstructured {
	if err := c.etcdObjectReader.WaitForStorageVersion(version, obj.GetNamespace(), obj.GetName(), 30*time.Second, func() {
		if _, err := versionedClient.Patch(context.TODO(), obj.GetName(), types.MergePatchType, []byte(`{}`), metav1.PatchOptions{}); err != nil {
			t.Fatalf("failed to update object: %v", err)
		}
	}); err != nil {
		t.Fatalf("failed waiting for storage version %s: %v", version, err)
	}

	t.Logf("Effective storage version: %s", version)

	return obj
}

func (c *conversionTestContext) setServed(t *testing.T, version string, served bool) {
	crd, err := c.apiExtensionsClient.ApiextensionsV1().CustomResourceDefinitions().Get(context.TODO(), c.crd.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for i, v := range crd.Spec.Versions {
		if v.Name == version {
			crd.Spec.Versions[i].Served = served
		}
	}
	crd, err = c.apiExtensionsClient.ApiextensionsV1().CustomResourceDefinitions().Update(context.TODO(), crd, metav1.UpdateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	c.crd = crd
}

func (c *conversionTestContext) waitForServed(t *testing.T, version string, served bool, versionedClient dynamic.ResourceInterface, obj *unstructured.Unstructured) {
	timeout := 30 * time.Second
	waitCh := time.After(timeout)
	for {
		obj, err := versionedClient.Get(context.TODO(), obj.GetName(), metav1.GetOptions{})
		if (err == nil && served) || (errors.IsNotFound(err) && served == false) {
			return
		}
		select {
		case <-waitCh:
			t.Fatalf("Timed out after %v waiting for CRD served=%t for version %s for %v. Last error: %v", timeout, served, version, obj, err)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

var multiVersionFixture = &apiextensionsv1.CustomResourceDefinition{
	ObjectMeta: metav1.ObjectMeta{Name: "multiversion.stable.example.com"},
	Spec: apiextensionsv1.CustomResourceDefinitionSpec{
		Group: "stable.example.com",
		Names: apiextensionsv1.CustomResourceDefinitionNames{
			Plural:     "multiversion",
			Singular:   "multiversion",
			Kind:       "MultiVersion",
			ShortNames: []string{"mv"},
			ListKind:   "MultiVersionList",
			Categories: []string{"all"},
		},
		Scope:                 apiextensionsv1.NamespaceScoped,
		PreserveUnknownFields: false,
		Versions: []apiextensionsv1.CustomResourceDefinitionVersion{
			{
				// storage version, same schema as v1alpha1
				Name:    "v1beta1",
				Served:  true,
				Storage: true,
				Subresources: &apiextensionsv1.CustomResourceSubresources{
					Status: &apiextensionsv1.CustomResourceSubresourceStatus{},
					Scale: &apiextensionsv1.CustomResourceSubresourceScale{
						SpecReplicasPath:   ".spec.num.num1",
						StatusReplicasPath: ".status.num.num2",
					},
				},
				Schema: &apiextensionsv1.CustomResourceValidation{
					OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
						Type: "object",
						Properties: map[string]apiextensionsv1.JSONSchemaProps{
							"content": {
								Type: "object",
								Properties: map[string]apiextensionsv1.JSONSchemaProps{
									"key": {Type: "string"},
								},
							},
							"num": {
								Type: "object",
								Properties: map[string]apiextensionsv1.JSONSchemaProps{
									"num1": {Type: "integer"},
									"num2": {Type: "integer"},
								},
							},
							"defaults": {
								Type: "object",
								Properties: map[string]apiextensionsv1.JSONSchemaProps{
									"v1alpha1": {Type: "boolean"},
									"v1beta1":  {Type: "boolean", Default: jsonPtr(true)},
									"v1beta2":  {Type: "boolean"},
								},
							},
						},
					},
				},
			},
			{
				// same schema as v1beta1
				Name:    "v1alpha1",
				Served:  true,
				Storage: false,
				Subresources: &apiextensionsv1.CustomResourceSubresources{
					Status: &apiextensionsv1.CustomResourceSubresourceStatus{},
					Scale: &apiextensionsv1.CustomResourceSubresourceScale{
						SpecReplicasPath:   ".spec.num.num1",
						StatusReplicasPath: ".status.num.num2",
					},
				},
				Schema: &apiextensionsv1.CustomResourceValidation{
					OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
						Type: "object",
						Properties: map[string]apiextensionsv1.JSONSchemaProps{
							"content": {
								Type: "object",
								Properties: map[string]apiextensionsv1.JSONSchemaProps{
									"key": {Type: "string"},
								},
							},
							"num": {
								Type: "object",
								Properties: map[string]apiextensionsv1.JSONSchemaProps{
									"num1": {Type: "integer"},
									"num2": {Type: "integer"},
								},
							},
							"defaults": {
								Type: "object",
								Properties: map[string]apiextensionsv1.JSONSchemaProps{
									"v1alpha1": {Type: "boolean", Default: jsonPtr(true)},
									"v1beta1":  {Type: "boolean"},
									"v1beta2":  {Type: "boolean"},
								},
							},
						},
					},
				},
			},
			{
				// different schema than v1beta1 and v1alpha1
				Name:    "v1beta2",
				Served:  true,
				Storage: false,
				Subresources: &apiextensionsv1.CustomResourceSubresources{
					Status: &apiextensionsv1.CustomResourceSubresourceStatus{},
					Scale: &apiextensionsv1.CustomResourceSubresourceScale{
						SpecReplicasPath:   ".spec.num.num1",
						StatusReplicasPath: ".status.num.num2",
					},
				},
				Schema: &apiextensionsv1.CustomResourceValidation{
					OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
						Type: "object",
						Properties: map[string]apiextensionsv1.JSONSchemaProps{
							"contentv2": {
								Type: "object",
								Properties: map[string]apiextensionsv1.JSONSchemaProps{
									"key": {Type: "string"},
								},
							},
							"numv2": {
								Type: "object",
								Properties: map[string]apiextensionsv1.JSONSchemaProps{
									"num1": {Type: "integer"},
									"num2": {Type: "integer"},
								},
							},
							"defaults": {
								Type: "object",
								Properties: map[string]apiextensionsv1.JSONSchemaProps{
									"v1alpha1": {Type: "boolean"},
									"v1beta1":  {Type: "boolean"},
									"v1beta2":  {Type: "boolean", Default: jsonPtr(true)},
								},
							},
						},
					},
				},
			},
		},
	},
}

func newConversionMultiVersionFixture(namespace, name, version string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "stable.example.com/" + version,
			"kind":       "MultiVersion",
			"metadata": map[string]interface{}{
				"namespace": namespace,
				"name":      name,
			},
		},
	}

	switch version {
	case "v1alpha1":
		u.Object["content"] = map[string]interface{}{
			"key": "value",
		}
		u.Object["num"] = map[string]interface{}{
			"num1": int64(1),
			"num2": int64(1000000),
		}
	case "v1beta1":
		u.Object["content"] = map[string]interface{}{
			"key": "value",
		}
		u.Object["num"] = map[string]interface{}{
			"num1": int64(1),
			"num2": int64(1000000),
		}
	case "v1beta2":
		u.Object["contentv2"] = map[string]interface{}{
			"key": "value",
		}
		u.Object["numv2"] = map[string]interface{}{
			"num1": int64(1),
			"num2": int64(1000000),
		}
	default:
		panic(fmt.Sprintf("unknown version %s", version))
	}

	return u
}

func verifyMultiVersionObject(t *testing.T, v string, obj *unstructured.Unstructured) {
	j := runtime.DeepCopyJSON(obj.Object)

	if expected := "stable.example.com/" + v; obj.GetAPIVersion() != expected {
		t.Errorf("unexpected apiVersion %q, expected %q", obj.GetAPIVersion(), expected)
		return
	}

	delete(j, "metadata")

	var expected = map[string]map[string]interface{}{
		"v1alpha1": {
			"apiVersion": "stable.example.com/v1alpha1",
			"kind":       "MultiVersion",
			"content": map[string]interface{}{
				"key": "value",
			},
			"num": map[string]interface{}{
				"num1": int64(1),
				"num2": int64(1000000),
			},
		},
		"v1beta1": {
			"apiVersion": "stable.example.com/v1beta1",
			"kind":       "MultiVersion",
			"content": map[string]interface{}{
				"key": "value",
			},
			"num": map[string]interface{}{
				"num1": int64(1),
				"num2": int64(1000000),
			},
		},
		"v1beta2": {
			"apiVersion": "stable.example.com/v1beta2",
			"kind":       "MultiVersion",
			"contentv2": map[string]interface{}{
				"key": "value",
			},
			"numv2": map[string]interface{}{
				"num1": int64(1),
				"num2": int64(1000000),
			},
		},
	}
	if !reflect.DeepEqual(expected[v], j) {
		t.Errorf("unexpected %s object: %s", v, cmp.Diff(expected[v], j))
	}
}

func closeOnCall(h http.Handler) (chan struct{}, http.Handler) {
	ch := make(chan struct{})
	once := sync.Once{}
	return ch, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() {
			close(ch)
		})
		h.ServeHTTP(w, r)
	})
}

func jsonPtr(x interface{}) *apiextensionsv1.JSON {
	bs, err := json.Marshal(x)
	if err != nil {
		panic(err)
	}
	ret := apiextensionsv1.JSON{Raw: bs}
	return &ret
}
