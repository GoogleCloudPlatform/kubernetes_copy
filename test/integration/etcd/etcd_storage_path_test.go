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

package etcd

import (
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	clientv3 "go.etcd.io/etcd/client/v3"

	v1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer/cbor"
	"k8s.io/apimachinery/pkg/runtime/serializer/json"
	"k8s.io/apimachinery/pkg/runtime/serializer/recognizer"
	utiljson "k8s.io/apimachinery/pkg/util/json"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/version"
	"k8s.io/apiserver/pkg/util/feature"
	"k8s.io/client-go/dynamic"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	componentbaseversion "k8s.io/component-base/version"
	"k8s.io/kubernetes/cmd/kube-apiserver/app/options"
	"k8s.io/kubernetes/pkg/features"
)

// Only add kinds to this list when this a virtual resource with get and create verbs that doesn't actually
// store into it's kind.  We've used this downstream for mappings before.
var kindAllowList = sets.NewString()

// namespace used for all tests, do not change this
const testNamespace = "etcdstoragepathtestnamespace"

// allowMissingTestdataFixtures contains the kinds expected to be missing serialization fixtures API testdata directory.
// this should only contain custom resources and built-in types with open issues tracking adding serialization fixtures.
// Do not add new built-in types to this list, add them to k8s.io/api/roundtrip_test.go instead.
var allowMissingTestdataFixtures = map[schema.GroupVersionKind]bool{
	// TODO(https://github.com/kubernetes/kubernetes/issues/79027)
	gvk("apiregistration.k8s.io", "v1", "APIService"):     true,
	gvk("apiregistration.k8s.io", "v1beta", "APIService"): true,

	// TODO(https://github.com/kubernetes/kubernetes/issues/79026)
	gvk("apiextensions.k8s.io", "v1beta1", "CustomResourceDefinition"): true,
	gvk("apiextensions.k8s.io", "v1", "CustomResourceDefinition"):      true,

	// Custom resources are not expected to have serialization fixtures in k8s.io/api
	gvk("awesome.bears.com", "v1", "Panda"):    true,
	gvk("cr.bar.com", "v1", "Foo"):             true,
	gvk("random.numbers.com", "v1", "Integer"): true,
	gvk("custom.fancy.com", "v2", "Pant"):      true,
}

// TestEtcdStoragePath tests to make sure that all objects are stored in an expected location in etcd.
// It will start failing when a new type is added to ensure that all future types are added to this test.
// It will also fail when a type gets moved to a different location. Be very careful in this situation because
// it essentially means that you will be break old clusters unless you create some migration path for the old data.
func TestEtcdStoragePath(t *testing.T) {
	supportedVersions := GetSupportedEmulatedVersions()
	for _, v := range supportedVersions {
		t.Run(v, func(t *testing.T) {
			testEtcdStoragePathWithVersion(t, v)
		})
	}
}

func testEtcdStoragePathWithVersion(t *testing.T, v string) {
	if v == componentbaseversion.DefaultKubeBinaryVersion {
		featuregatetesting.SetFeatureGateDuringTest(t, feature.DefaultFeatureGate, "AllAlpha", true)
		featuregatetesting.SetFeatureGateDuringTest(t, feature.DefaultFeatureGate, "AllBeta", true)
	} else {
		// Only test for beta and GA APIs with emulated version.
		featuregatetesting.SetFeatureGateEmulationVersionDuringTest(t, feature.DefaultFeatureGate, version.MustParse(v))
		featuregatetesting.SetFeatureGateDuringTest(t, feature.DefaultFeatureGate, "AllBeta", true)
		// Feature Gates that are GA and depend directly on the API version to work can not be emulated in previous versions.
		// Example feature:
		// v1.x-2 : FeatureGate alpha , API v1alpha1/feature
		// v1.x-1 : FeatureGate beta  , API v1beta1/feature
		// v1.x   : FeatureGate GA    , API v1/feature
		// The code in v1.x uses the clients with the v1 API, if we emulate v1.x-1 it will not work against apiserver that
		// only understand v1beta1.
		featuregatetesting.SetFeatureGateDuringTest(t, feature.DefaultFeatureGate, features.MultiCIDRServiceAllocator, false)
	}

	apiServer := StartRealAPIServerOrDie(t, func(opts *options.ServerRunOptions) {
		// Disable alphas when emulating previous versions.
		if v != componentbaseversion.DefaultKubeBinaryVersion {
			opts.Options.APIEnablement.RuntimeConfig["api/alpha"] = "false"
		}
	})

	defer apiServer.Cleanup()
	defer dumpEtcdKVOnFailure(t, apiServer.KV)

	client := &allClient{dynamicClient: apiServer.Dynamic}

	if _, err := apiServer.Client.CoreV1().Namespaces().Create(context.TODO(), &v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: testNamespace}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	var etcdStorageData map[schema.GroupVersionResource]StorageData
	if v == componentbaseversion.DefaultKubeBinaryVersion {
		etcdStorageData = GetEtcdStorageDataForNamespaceServedAt("etcdstoragepathtestnamespace", v, false)
	} else {
		// Drop alphas from etcd data fixtures when emulating previous versions
		// as alphas are not supported with emulation.
		etcdStorageData = GetEtcdStorageDataForNamespaceServedAt("etcdstoragepathtestnamespace", v, true)
	}

	kindSeen := sets.NewString()
	pathSeen := map[string][]schema.GroupVersionResource{}
	etcdSeen := map[schema.GroupVersionResource]empty{}
	cohabitatingResources := map[string]map[schema.GroupVersionKind]empty{}

	for _, resourceToPersist := range apiServer.Resources {
		t.Run(resourceToPersist.Mapping.Resource.String(), func(t *testing.T) {
			mapping := resourceToPersist.Mapping
			gvk := resourceToPersist.Mapping.GroupVersionKind
			gvResource := resourceToPersist.Mapping.Resource
			kind := gvk.Kind

			if kindAllowList.Has(kind) {
				kindSeen.Insert(kind)
				t.Skip("allowlisted")
			}

			etcdSeen[gvResource] = empty{}
			testData, hasTest := etcdStorageData[gvResource]

			if !hasTest {
				t.Fatalf("no test data for %s.  Please add a test for your new type to GetEtcdStorageData().", gvResource)
			}

			if len(testData.ExpectedEtcdPath) == 0 {
				t.Fatalf("empty test data for %s", gvResource)
			}

			shouldCreate := len(testData.Stub) != 0 // try to create only if we have a stub

			var (
				input *metaObject
				err   error
			)
			if shouldCreate {
				input = new(metaObject)
				if err = utiljson.Unmarshal([]byte(testData.Stub), input); err != nil || input.isEmpty() {
					t.Fatalf("invalid test data for %s: %v", gvResource, err)
				}
				// unset type meta fields - we only set these in the CRD test data and it makes
				// any CRD test with an expectedGVK override fail the DeepDerivative test
				input.Kind = ""
				input.APIVersion = ""
			}

			all := &[]cleanupData{}
			defer func() {
				if !t.Failed() { // do not cleanup if test has already failed since we may need things in the etcd dump
					if err := client.cleanup(all); err != nil {
						t.Fatalf("failed to clean up etcd: %#v", err)
					}
				}
			}()

			if err := client.createPrerequisites(apiServer.Mapper, testNamespace, testData.Prerequisites, all); err != nil {
				t.Fatalf("failed to create prerequisites for %s: %#v", gvResource, err)
			}

			if shouldCreate { // do not try to create items with no stub
				if err := client.create(testData.Stub, testNamespace, mapping, all); err != nil {
					t.Fatalf("failed to create stub for %s: %#v", gvResource, err)
				}
			}

			// Build a decoder that can decode JSON and CBOR from storage.
			scheme := runtime.NewScheme()
			if testData.ExpectedGVK != nil {
				scheme.AddKnownTypeWithName(*testData.ExpectedGVK, &metaObject{})
			} else {
				scheme.AddKnownTypeWithName(gvk, &metaObject{})

			}
			decoder := recognizer.NewDecoder(
				cbor.NewSerializer(scheme, scheme),
				json.NewSerializerWithOptions(json.DefaultMetaFactory, scheme, scheme, json.SerializerOptions{}),
			)

			output, err := getFromEtcd(decoder, apiServer.KV, testData.ExpectedEtcdPath)
			if err != nil {
				t.Fatalf("failed to get from etcd for %s: %#v", gvResource, err)
			}

			expectedGVK := gvk
			if testData.ExpectedGVK != nil {
				if gvk == *testData.ExpectedGVK {
					t.Errorf("GVK override %s for %s is unnecessary or something was changed incorrectly", testData.ExpectedGVK, gvk)
				}
				expectedGVK = *testData.ExpectedGVK
			}

			// if previous releases had a non-alpha version of this group/kind, make sure the storage version is understood by a previous release
			fixtureFilenameGroup := expectedGVK.Group
			if fixtureFilenameGroup == "" {
				fixtureFilenameGroup = "core"
			}
			// find all versions of this group/kind in all versions of the serialization fixture testdata
			releaseGroupKindFiles, err := filepath.Glob("../../../staging/src/k8s.io/api/testdata/*/" + fixtureFilenameGroup + ".*." + expectedGVK.Kind + ".yaml")
			if err != nil {
				t.Error(err)
			}
			if len(releaseGroupKindFiles) == 0 && !allowMissingTestdataFixtures[expectedGVK] {
				// We should at least find the HEAD fixtures
				t.Errorf("No testdata serialization files found for %#v, cannot determine if previous releases could read this group/kind. Add this group-version to k8s.io/api/roundtrip_test.go", expectedGVK)
			}

			// find non-alpha versions of this group/kind understood by current and previous releases
			currentNonAlphaVersions := sets.NewString()
			previousNonAlphaVersions := sets.NewString()
			for _, previousReleaseGroupKindFile := range releaseGroupKindFiles {
				parts := strings.Split(filepath.Base(previousReleaseGroupKindFile), ".")
				version := parts[len(parts)-3]
				if !strings.Contains(version, "alpha") {
					if serverVersion := filepath.Base(filepath.Dir(previousReleaseGroupKindFile)); serverVersion == "HEAD" {
						currentNonAlphaVersions.Insert(version)
					} else {
						previousNonAlphaVersions.Insert(version)
					}
				}
			}
			if len(currentNonAlphaVersions) > 0 && strings.Contains(expectedGVK.Version, "alpha") {
				t.Errorf("Non-alpha versions %q exist, but the expected storage version is %q. Prefer beta or GA storage versions over alpha.",
					currentNonAlphaVersions.List(),
					expectedGVK.Version,
				)
			}
			if !strings.Contains(expectedGVK.Version, "alpha") && len(previousNonAlphaVersions) > 0 && !previousNonAlphaVersions.Has(expectedGVK.Version) {
				t.Errorf("Previous releases understand non-alpha versions %q, but do not understand the expected current non-alpha storage version %q. "+
					"This means a current server will store data in etcd that is not understood by a previous version.",
					previousNonAlphaVersions.List(),
					expectedGVK.Version,
				)
			}

			actualGVK := output.GroupVersionKind()
			if actualGVK != expectedGVK {
				t.Errorf("GVK for %s does not match, expected %s got %s", kind, expectedGVK, actualGVK)
			}

			if !apiequality.Semantic.DeepDerivative(input, output) {
				t.Errorf("Test stub for %s does not match: %s", kind, cmp.Diff(input, output))
			}

			addGVKToEtcdBucket(cohabitatingResources, actualGVK, getEtcdBucket(testData.ExpectedEtcdPath))
			pathSeen[testData.ExpectedEtcdPath] = append(pathSeen[testData.ExpectedEtcdPath], mapping.Resource)
		})
	}

	if inEtcdData, inEtcdSeen := diffMaps(etcdStorageData, etcdSeen); len(inEtcdData) != 0 || len(inEtcdSeen) != 0 {
		t.Errorf("etcd data does not match the types we saw:\nin etcd data but not seen:\n%s\nseen but not in etcd data:\n%s", inEtcdData, inEtcdSeen)
	}
	if inKindData, inKindSeen := diffMaps(kindAllowList, kindSeen); len(inKindData) != 0 || len(inKindSeen) != 0 {
		t.Errorf("kind allowlist data does not match the types we saw:\nin kind allowlist but not seen:\n%s\nseen but not in kind allowlist:\n%s", inKindData, inKindSeen)
	}

	for bucket, gvks := range cohabitatingResources {
		if len(gvks) != 1 {
			gvkStrings := []string{}
			for key := range gvks {
				gvkStrings = append(gvkStrings, keyStringer(key))
			}
			t.Errorf("cohabitating resources in etcd bucket %s have inconsistent GVKs\nyou may need to use DefaultStorageFactory.AddCohabitatingResources to sync the GVK of these resources:\n%s", bucket, gvkStrings)
		}
	}

	for path, gvrs := range pathSeen {
		if len(gvrs) != 1 {
			gvrStrings := []string{}
			for _, key := range gvrs {
				gvrStrings = append(gvrStrings, keyStringer(key))
			}
			t.Errorf("invalid test data, please ensure all expectedEtcdPath are unique, path %s has duplicate GVRs:\n%s", path, gvrStrings)
		}
	}
}

var debug = false

func dumpEtcdKVOnFailure(t *testing.T, kvClient clientv3.KV) {
	if t.Failed() && debug {
		response, err := kvClient.Get(context.Background(), "/", clientv3.WithPrefix())
		if err != nil {
			t.Fatal(err)
		}

		for _, kv := range response.Kvs {
			t.Error(string(kv.Key), "->", string(kv.Value))
		}
	}
}

func addGVKToEtcdBucket(cohabitatingResources map[string]map[schema.GroupVersionKind]empty, gvk schema.GroupVersionKind, bucket string) {
	if cohabitatingResources[bucket] == nil {
		cohabitatingResources[bucket] = map[schema.GroupVersionKind]empty{}
	}
	cohabitatingResources[bucket][gvk] = empty{}
}

// getEtcdBucket assumes the last segment of the given etcd path is the name of the object.
// Thus it strips that segment to extract the object's storage "bucket" in etcd. We expect
// all objects that share the a bucket (cohabitating resources) to be stored as the same GVK.
func getEtcdBucket(path string) string {
	idx := strings.LastIndex(path, "/")
	if idx == -1 {
		panic("path with no slashes " + path)
	}
	bucket := path[:idx]
	if len(bucket) == 0 {
		panic("invalid bucket for path " + path)
	}
	return bucket
}

// stable fields to compare as a sanity check
type metaObject struct {
	metav1.TypeMeta `json:",inline"`

	// parts of object meta
	Metadata struct {
		Name      string `json:"name,omitempty"`
		Namespace string `json:"namespace,omitempty"`
	} `json:"metadata,omitempty"`
}

var _ runtime.Object = &metaObject{}

func (*metaObject) DeepCopyObject() runtime.Object {
	panic("unimplemented")
}

func (obj *metaObject) isEmpty() bool {
	return obj == nil || *obj == metaObject{} // compare to zero value since all fields are strings
}

type empty struct{}

type cleanupData struct {
	obj      *unstructured.Unstructured
	resource schema.GroupVersionResource
}

func keyStringer(i interface{}) string {
	base := "\n\t"
	switch key := i.(type) {
	case string:
		return base + key
	case schema.GroupVersionResource:
		return base + key.String()
	case schema.GroupVersionKind:
		return base + key.String()
	default:
		panic("unexpected type")
	}
}

type allClient struct {
	dynamicClient dynamic.Interface
}

func (c *allClient) create(stub, ns string, mapping *meta.RESTMapping, all *[]cleanupData) error {
	resourceClient, obj, err := JSONToUnstructured(stub, ns, mapping, c.dynamicClient)
	if err != nil {
		return err
	}

	actual, err := resourceClient.Create(context.TODO(), obj, metav1.CreateOptions{})
	if err != nil {
		return err
	}

	*all = append(*all, cleanupData{obj: actual, resource: mapping.Resource})

	return nil
}

func (c *allClient) cleanup(all *[]cleanupData) error {
	for i := len(*all) - 1; i >= 0; i-- { // delete in reverse order in case creation order mattered
		obj := (*all)[i].obj
		gvr := (*all)[i].resource

		if err := c.dynamicClient.Resource(gvr).Namespace(obj.GetNamespace()).Delete(context.TODO(), obj.GetName(), metav1.DeleteOptions{}); err != nil {
			return err
		}
	}
	return nil
}

func (c *allClient) createPrerequisites(mapper meta.RESTMapper, ns string, prerequisites []Prerequisite, all *[]cleanupData) error {
	for _, prerequisite := range prerequisites {
		gvk, err := mapper.KindFor(prerequisite.GvrData)
		if err != nil {
			return err
		}
		mapping, err := mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
		if err != nil {
			return err
		}
		if err := c.create(prerequisite.Stub, ns, mapping, all); err != nil {
			return err
		}
	}
	return nil
}

func getFromEtcd(decoder runtime.Decoder, keys clientv3.KV, path string) (*metaObject, error) {
	response, err := keys.Get(context.Background(), path)
	if err != nil {
		return nil, err
	}
	if response.More || response.Count != 1 || len(response.Kvs) != 1 {
		return nil, fmt.Errorf("Invalid etcd response (not found == %v): %#v", response.Count == 0, response)
	}
	var obj metaObject
	if err := runtime.DecodeInto(decoder, response.Kvs[0].Value, &obj); err != nil {
		return nil, err
	}
	return &obj, nil
}

func diffMaps(a, b interface{}) ([]string, []string) {
	inA := diffMapKeys(a, b, keyStringer)
	inB := diffMapKeys(b, a, keyStringer)
	return inA, inB
}

func diffMapKeys(a, b interface{}, stringer func(interface{}) string) []string {
	av := reflect.ValueOf(a)
	bv := reflect.ValueOf(b)
	ret := []string{}

	for _, ka := range av.MapKeys() {
		kat := ka.Interface()
		found := false
		for _, kb := range bv.MapKeys() {
			kbt := kb.Interface()
			if kat == kbt {
				found = true
				break
			}
		}
		if !found {
			ret = append(ret, stringer(kat))
		}
	}

	return ret
}
