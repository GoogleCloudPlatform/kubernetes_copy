/*
Copyright 2022 The Kubernetes Authors.

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

package aggregated_test

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"

	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	fuzz "github.com/google/gofuzz"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	apidiscoveryv2 "k8s.io/api/apidiscovery/v2"
	apidiscoveryv2beta1 "k8s.io/api/apidiscovery/v2beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	runtimeserializer "k8s.io/apimachinery/pkg/runtime/serializer"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	utilversion "k8s.io/apimachinery/pkg/util/version"
	"k8s.io/apimachinery/pkg/version"
	apidiscoveryv2conversion "k8s.io/apiserver/pkg/apis/apidiscovery/v2"
	discoveryendpoint "k8s.io/apiserver/pkg/endpoints/discovery/aggregated"
)

var scheme = runtime.NewScheme()
var codecs = runtimeserializer.NewCodecFactory(scheme)

const discoveryPath = "/apis"

func init() {
	utilruntime.Must(apidiscoveryv2.AddToScheme(scheme))
	utilruntime.Must(apidiscoveryv2beta1.AddToScheme(scheme))
	// Register conversion for apidiscovery
	utilruntime.Must(apidiscoveryv2conversion.RegisterConversions(scheme))
	codecs = runtimeserializer.NewCodecFactory(scheme)
}

func fuzzAPIGroups(atLeastNumGroups, maxNumGroups int, seed int64) apidiscoveryv2.APIGroupDiscoveryList {
	fuzzer := fuzz.NewWithSeed(seed)
	fuzzer.NumElements(atLeastNumGroups, maxNumGroups)
	fuzzer.NilChance(0)
	fuzzer.Funcs(func(o *apidiscoveryv2.APIGroupDiscovery, c fuzz.Continue) {
		c.FuzzNoCustom(o)

		// The ResourceManager will just not serve the group if its versions
		// list is empty
		atLeastOne := apidiscoveryv2.APIVersionDiscovery{}
		c.Fuzz(&atLeastOne)
		o.Versions = append(o.Versions, atLeastOne)
		sort.Slice(o.Versions[:], func(i, j int) bool {
			return version.CompareKubeAwareVersionStrings(o.Versions[i].Version, o.Versions[j].Version) > 0
		})

		o.TypeMeta = metav1.TypeMeta{}
		var name string
		c.Fuzz(&name)
		o.ObjectMeta = metav1.ObjectMeta{
			Name: name,
		}
	})

	var apis []apidiscoveryv2.APIGroupDiscovery
	fuzzer.Fuzz(&apis)
	sort.Slice(apis[:], func(i, j int) bool {
		return apis[i].Name < apis[j].Name
	})

	return apidiscoveryv2.APIGroupDiscoveryList{
		TypeMeta: metav1.TypeMeta{
			Kind:       "APIGroupDiscoveryList",
			APIVersion: "apidiscovery.k8s.io/v2",
		},
		Items: apis,
	}
}

func fetchPathV2Beta1(handler http.Handler, acceptPrefix string, path string, etag string) (*http.Response, []byte, *apidiscoveryv2beta1.APIGroupDiscoveryList, error) {
	acceptSuffix := ";g=apidiscovery.k8s.io;v=v2beta1;as=APIGroupDiscoveryList"
	r, bytes := fetchPathHelper(handler, acceptPrefix+acceptSuffix, path, etag)
	var decoded *apidiscoveryv2beta1.APIGroupDiscoveryList
	var err error
	if len(bytes) > 0 {
		decoded = &apidiscoveryv2beta1.APIGroupDiscoveryList{}
		err = runtime.DecodeInto(codecs.UniversalDecoder(), bytes, decoded)
	}
	return r, bytes, decoded, err
}

func fetchPath(handler http.Handler, acceptPrefix string, path string, etag string) (*http.Response, []byte, *apidiscoveryv2.APIGroupDiscoveryList) {
	acceptSuffix := ";g=apidiscovery.k8s.io;v=v2;as=APIGroupDiscoveryList,"
	acceptSuffixV2Beta1 := ";g=apidiscovery.k8s.io;v=v2beta1;as=APIGroupDiscoveryList,"
	r, bytes := fetchPathHelper(handler, acceptPrefix+acceptSuffix+","+acceptPrefix+acceptSuffixV2Beta1, path, etag)
	var decoded *apidiscoveryv2.APIGroupDiscoveryList
	if len(bytes) > 0 {
		decoded = &apidiscoveryv2.APIGroupDiscoveryList{}
		err := runtime.DecodeInto(codecs.UniversalDecoder(), bytes, decoded)
		if err != nil {
			panic(fmt.Sprintf("failed to decode response: %v", err))
		}
	}
	return r, bytes, decoded
}

func fetchPathHelper(handler http.Handler, accept string, path string, etag string) (*http.Response, []byte) {
	// Expect json-formatted apis group list
	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", discoveryPath, nil)

	// Ask for JSON response
	req.Header.Set("Accept", accept)

	if etag != "" {
		// Quote provided etag if unquoted
		quoted := etag
		if !strings.HasPrefix(etag, "\"") {
			quoted = strconv.Quote(etag)
		}
		req.Header.Set("If-None-Match", quoted)
	}

	handler.ServeHTTP(w, req)

	bytes := w.Body.Bytes()
	return w.Result(), bytes
}

// Add all builtin APIServices to the manager and check the output
func TestBasicResponse(t *testing.T) {
	manager := discoveryendpoint.NewResourceManager("apis")

	apis := fuzzAPIGroups(1, 3, 10)
	manager.SetGroups(apis.Items)

	response, body, decoded := fetchPath(manager, "application/json", discoveryPath, "")

	jsonFormatted, err := json.Marshal(&apis)
	require.NoError(t, err, "json marshal should always succeed")

	assert.Equal(t, http.StatusOK, response.StatusCode, "response should be 200 OK")
	assert.Equal(t, "application/json;g=apidiscovery.k8s.io;v=v2;as=APIGroupDiscoveryList", response.Header.Get("Content-Type"), "Content-Type response header should be as requested in Accept header if supported")
	assert.NotEmpty(t, response.Header.Get("ETag"), "E-Tag should be set")

	assert.NoError(t, err, "decode should always succeed")
	assert.EqualValues(t, &apis, decoded, "decoded value should equal input")
	assert.Equal(t, string(jsonFormatted)+"\n", string(body), "response should be the api group list")
}

// Test that protobuf is outputted correctly
func TestBasicResponseProtobuf(t *testing.T) {
	manager := discoveryendpoint.NewResourceManager("apis")

	apis := fuzzAPIGroups(1, 3, 10)
	manager.SetGroups(apis.Items)

	response, _, decoded := fetchPath(manager, "application/vnd.kubernetes.protobuf", discoveryPath, "")
	assert.Equal(t, http.StatusOK, response.StatusCode, "response should be 200 OK")
	assert.Equal(t, "application/vnd.kubernetes.protobuf;g=apidiscovery.k8s.io;v=v2;as=APIGroupDiscoveryList", response.Header.Get("Content-Type"), "Content-Type response header should be as requested in Accept header if supported")
	assert.NotEmpty(t, response.Header.Get("ETag"), "E-Tag should be set")
	assert.EqualValues(t, &apis, decoded, "decoded value should equal input")
}

// V2Beta1 should not be served on emulated version >= 1.33
func TestV2Beta1NotServed(t *testing.T) {
	manager := discoveryendpoint.NewResourceManagerWithEmulatedVersion("apis", utilversion.MustParse("1.33"))

	apis := fuzzAPIGroups(1, 3, 10)
	manager.SetGroups(apis.Items)

	_, _, _, err := fetchPathV2Beta1(manager, "application/json", discoveryPath, "")
	if err == nil {
		t.Error("Expected err fetching v2beta1 resources, got no err")
	}
}

// V2Beta1 should only be served on emulated version < 1.33
func TestV2Beta1SkewSupport(t *testing.T) {
	manager := discoveryendpoint.NewResourceManagerWithEmulatedVersion("apis", utilversion.MustParse("1.32"))

	apis := fuzzAPIGroups(1, 3, 10)
	manager.SetGroups(apis.Items)

	converted, err := scheme.ConvertToVersion(&apis, &schema.GroupVersion{Group: "apidiscovery.k8s.io", Version: "v2beta1"})
	if err != nil {
		t.Fatal(err)
	}

	v2beta1apis := converted.(*apidiscoveryv2beta1.APIGroupDiscoveryList)

	response, body, decoded, err := fetchPathV2Beta1(manager, "application/json", discoveryPath, "")
	if err != nil {
		t.Fatal(err)
	}

	jsonFormatted, err := json.Marshal(v2beta1apis)
	require.NoError(t, err, "json marshal should always succeed")

	assert.Equal(t, http.StatusOK, response.StatusCode, "response should be 200 OK")
	assert.Equal(t, "application/json;g=apidiscovery.k8s.io;v=v2beta1;as=APIGroupDiscoveryList", response.Header.Get("Content-Type"), "Content-Type response header should be as requested in Accept header if supported")
	assert.NotEmpty(t, response.Header.Get("ETag"), "E-Tag should be set")

	assert.NoError(t, err, "decode should always succeed")
	assert.EqualValues(t, v2beta1apis, decoded, "decoded value should equal input")
	assert.Equal(t, string(jsonFormatted)+"\n", string(body), "response should be the api group list")
}

// Test that an etag associated with the service only depends on the apiresources
// e.g.: Multiple services with the same contents should have the same etag.
func TestEtagConsistent(t *testing.T) {
	// Create 2 managers, add a bunch of services to each
	manager1 := discoveryendpoint.NewResourceManager("apis")
	manager2 := discoveryendpoint.NewResourceManager("apis")

	apis := fuzzAPIGroups(1, 3, 11)
	manager1.SetGroups(apis.Items)
	manager2.SetGroups(apis.Items)

	// Make sure etag of each is the same
	res1_initial, _, _ := fetchPath(manager1, "application/json", discoveryPath, "")
	res2_initial, _, _ := fetchPath(manager2, "application/json", discoveryPath, "")

	assert.NotEmpty(t, res1_initial.Header.Get("ETag"), "Etag should be populated")
	assert.NotEmpty(t, res2_initial.Header.Get("ETag"), "Etag should be populated")
	assert.Equal(t, res1_initial.Header.Get("ETag"), res2_initial.Header.Get("ETag"), "etag should be deterministic")

	// Then add one service to only one.
	// Make sure etag is changed, but other is the same
	apis = fuzzAPIGroups(1, 1, 11)
	for _, group := range apis.Items {
		for _, version := range group.Versions {
			manager1.AddGroupVersion(group.Name, version)
		}
	}

	res1_addedToOne, _, _ := fetchPath(manager1, "application/json", discoveryPath, "")
	res2_addedToOne, _, _ := fetchPath(manager2, "application/json", discoveryPath, "")

	assert.NotEmpty(t, res1_addedToOne.Header.Get("ETag"), "Etag should be populated")
	assert.NotEmpty(t, res2_addedToOne.Header.Get("ETag"), "Etag should be populated")
	assert.NotEqual(t, res1_initial.Header.Get("ETag"), res1_addedToOne.Header.Get("ETag"), "ETag should be changed since version was added")
	assert.Equal(t, res2_initial.Header.Get("ETag"), res2_addedToOne.Header.Get("ETag"), "ETag should be unchanged since data was unchanged")

	// Then add service to other one
	// Make sure etag is the same
	for _, group := range apis.Items {
		for _, version := range group.Versions {
			manager2.AddGroupVersion(group.Name, version)
		}
	}

	res1_addedToBoth, _, _ := fetchPath(manager1, "application/json", discoveryPath, "")
	res2_addedToBoth, _, _ := fetchPath(manager2, "application/json", discoveryPath, "")

	assert.NotEmpty(t, res1_addedToOne.Header.Get("ETag"), "Etag should be populated")
	assert.NotEmpty(t, res2_addedToOne.Header.Get("ETag"), "Etag should be populated")
	assert.Equal(t, res1_addedToBoth.Header.Get("ETag"), res2_addedToBoth.Header.Get("ETag"), "ETags should be equal since content is equal")
	assert.NotEqual(t, res2_initial.Header.Get("ETag"), res2_addedToBoth.Header.Get("ETag"), "ETag should be changed since data was changed")

	// Remove the group version from both. Initial E-Tag should be restored
	for _, group := range apis.Items {
		for _, version := range group.Versions {
			manager1.RemoveGroupVersion(metav1.GroupVersion{
				Group:   group.Name,
				Version: version.Version,
			})
			manager2.RemoveGroupVersion(metav1.GroupVersion{
				Group:   group.Name,
				Version: version.Version,
			})
		}
	}

	res1_removeFromBoth, _, _ := fetchPath(manager1, "application/json", discoveryPath, "")
	res2_removeFromBoth, _, _ := fetchPath(manager2, "application/json", discoveryPath, "")

	assert.NotEmpty(t, res1_addedToOne.Header.Get("ETag"), "Etag should be populated")
	assert.NotEmpty(t, res2_addedToOne.Header.Get("ETag"), "Etag should be populated")
	assert.Equal(t, res1_removeFromBoth.Header.Get("ETag"), res2_removeFromBoth.Header.Get("ETag"), "ETags should be equal since content is equal")
	assert.Equal(t, res1_initial.Header.Get("ETag"), res1_removeFromBoth.Header.Get("ETag"), "ETag should be equal to initial value since added content was removed")
}

// Test that if a request comes in with an If-None-Match header with an incorrect
// E-Tag, that fresh content is returned.
func TestEtagNonMatching(t *testing.T) {
	manager := discoveryendpoint.NewResourceManager("apis")
	apis := fuzzAPIGroups(1, 3, 12)
	manager.SetGroups(apis.Items)

	// fetch the document once
	initial, _, _ := fetchPath(manager, "application/json", discoveryPath, "")
	assert.NotEmpty(t, initial.Header.Get("ETag"), "ETag should be populated")

	// Send another request with a wrong e-tag. The same response should
	// get sent again
	second, _, _ := fetchPath(manager, "application/json", discoveryPath, "wrongetag")

	assert.Equal(t, http.StatusOK, initial.StatusCode, "response should be 200 OK")
	assert.Equal(t, http.StatusOK, second.StatusCode, "response should be 200 OK")
	assert.Equal(t, initial.Header.Get("ETag"), second.Header.Get("ETag"), "ETag of both requests should be equal")
}

// Test that if a request comes in with an If-None-Match header with a correct
// E-Tag, that 304 Not Modified is returned
func TestEtagMatching(t *testing.T) {
	manager := discoveryendpoint.NewResourceManager("apis")
	apis := fuzzAPIGroups(1, 3, 12)
	manager.SetGroups(apis.Items)

	// fetch the document once
	initial, initialBody, _ := fetchPath(manager, "application/json", discoveryPath, "")
	assert.NotEmpty(t, initial.Header.Get("ETag"), "ETag should be populated")
	assert.NotEmpty(t, initialBody, "body should not be empty")

	// Send another request with a wrong e-tag. The same response should
	// get sent again
	second, secondBody, _ := fetchPath(manager, "application/json", discoveryPath, initial.Header.Get("ETag"))

	assert.Equal(t, http.StatusOK, initial.StatusCode, "initial response should be 200 OK")
	assert.Equal(t, http.StatusNotModified, second.StatusCode, "second response should be 304 Not Modified")
	assert.Equal(t, initial.Header.Get("ETag"), second.Header.Get("ETag"), "ETag of both requests should be equal")
	assert.Empty(t, secondBody, "body should be empty when returning 304 Not Modified")
}

// Test that if a request comes in with an If-None-Match header with an old
// E-Tag, that fresh content is returned
func TestEtagOutdated(t *testing.T) {
	manager := discoveryendpoint.NewResourceManager("apis")
	apis := fuzzAPIGroups(1, 3, 15)
	manager.SetGroups(apis.Items)

	// fetch the document once
	initial, initialBody, _ := fetchPath(manager, "application/json", discoveryPath, "")
	assert.NotEmpty(t, initial.Header.Get("ETag"), "ETag should be populated")
	assert.NotEmpty(t, initialBody, "body should not be empty")

	// Then add some services so the etag changes
	apis = fuzzAPIGroups(1, 3, 14)
	for _, group := range apis.Items {
		for _, version := range group.Versions {
			manager.AddGroupVersion(group.Name, version)
		}
	}

	// Send another request with the old e-tag. Response should not be 304 Not Modified
	second, secondBody, _ := fetchPath(manager, "application/json", discoveryPath, initial.Header.Get("ETag"))

	assert.Equal(t, http.StatusOK, initial.StatusCode, "initial response should be 200 OK")
	assert.Equal(t, http.StatusOK, second.StatusCode, "second response should be 304 Not Modified")
	assert.NotEqual(t, initial.Header.Get("ETag"), second.Header.Get("ETag"), "ETag of both requests should be unequal since contents differ")
	assert.NotEmpty(t, secondBody, "body should be not empty when returning 304 Not Modified")
}

// Test that an api service can be added or removed
func TestAddRemove(t *testing.T) {
	manager := discoveryendpoint.NewResourceManager("apis")
	apis := fuzzAPIGroups(1, 3, 15)
	for _, group := range apis.Items {
		for _, version := range group.Versions {
			manager.AddGroupVersion(group.Name, version)
		}
	}

	_, _, initialDocument := fetchPath(manager, "application/json", discoveryPath, "")

	for _, group := range apis.Items {
		for _, version := range group.Versions {
			manager.RemoveGroupVersion(metav1.GroupVersion{
				Group:   group.Name,
				Version: version.Version,
			})
		}
	}

	_, _, secondDocument := fetchPath(manager, "application/json", discoveryPath, "")

	require.NotNil(t, initialDocument, "initial document should parse")
	require.NotNil(t, secondDocument, "second document should parse")
	assert.Len(t, initialDocument.Items, len(apis.Items), "initial document should have set number of groups")
	assert.Empty(t, secondDocument.Items, "second document should have no groups")
}

// Show that updating an existing service replaces and does not add the entry
// and instead replaces it
func TestUpdateService(t *testing.T) {
	manager := discoveryendpoint.NewResourceManager("apis")
	apis := fuzzAPIGroups(1, 3, 15)
	for _, group := range apis.Items {
		for _, version := range group.Versions {
			manager.AddGroupVersion(group.Name, version)
		}
	}

	_, _, initialDocument := fetchPath(manager, "application/json", discoveryPath, "")

	assert.Equal(t, initialDocument, &apis, "should have returned expected document")

	b, err := json.Marshal(apis)
	if err != nil {
		t.Error(err)
	}
	var newapis apidiscoveryv2.APIGroupDiscoveryList
	err = json.Unmarshal(b, &newapis)
	if err != nil {
		t.Error(err)
	}

	newapis.Items[0].Versions[0].Resources[0].Resource = "changed a resource name!"
	for _, group := range newapis.Items {
		for _, version := range group.Versions {
			manager.AddGroupVersion(group.Name, version)
		}
	}

	_, _, secondDocument := fetchPath(manager, "application/json", discoveryPath, "")
	assert.Equal(t, secondDocument, &newapis, "should have returned expected document")
	assert.NotEqual(t, secondDocument, initialDocument, "should have returned expected document")
}

func TestMultipleSources(t *testing.T) {
	type pair struct {
		manager discoveryendpoint.ResourceManager
		apis    apidiscoveryv2.APIGroupDiscoveryList
	}

	pairs := []pair{}

	defaultManager := discoveryendpoint.NewResourceManager("apis")
	for i := 0; i < 10; i++ {
		name := discoveryendpoint.Source(100 * i)
		manager := defaultManager.WithSource(name)
		apis := fuzzAPIGroups(1, 3, int64(15+i))

		// Give the groups deterministic names
		for i := range apis.Items {
			apis.Items[i].Name = fmt.Sprintf("%v.%v.com", i, name)
		}

		pairs = append(pairs, pair{manager, apis})
	}

	expectedResult := []apidiscoveryv2.APIGroupDiscovery{}

	groupCounter := 0
	for _, p := range pairs {
		for gi, g := range p.apis.Items {
			for vi, v := range g.Versions {
				p.manager.AddGroupVersion(g.Name, v)

				// Use index for priority so we dont have to do any sorting
				// Use negative index since it is sorted descending
				p.manager.SetGroupVersionPriority(metav1.GroupVersion{Group: g.Name, Version: v.Version}, -gi-groupCounter, -vi)
			}

			expectedResult = append(expectedResult, g)
		}

		groupCounter += len(p.apis.Items)
	}

	// Show discovery document is what we expect
	_, _, initialDocument := fetchPath(defaultManager, "application/json", discoveryPath, "")

	require.Len(t, initialDocument.Items, len(expectedResult))
	require.Equal(t, expectedResult, initialDocument.Items)
}

// Shows that if you have multiple sources including Default source using
// with the same group name the groups added by the "Default" source are used
func TestSourcePrecedence(t *testing.T) {
	defaultManager := discoveryendpoint.NewResourceManager("apis")
	otherManager := defaultManager.WithSource(500)
	apis := fuzzAPIGroups(1, 3, int64(15))
	for _, g := range apis.Items {
		for i, v := range g.Versions {
			v.Freshness = apidiscoveryv2.DiscoveryFreshnessCurrent
			g.Versions[i] = v
			otherManager.AddGroupVersion(g.Name, v)
		}
	}

	_, _, initialDocument := fetchPath(defaultManager, "application/json", discoveryPath, "")
	require.Equal(t, apis.Items, initialDocument.Items)

	// Add the first groupversion under default.
	// No versions should appear in discovery document except this one
	overrideVersion := initialDocument.Items[0].Versions[0]
	overrideVersion.Freshness = apidiscoveryv2.DiscoveryFreshnessStale
	defaultManager.AddGroupVersion(initialDocument.Items[0].Name, overrideVersion)

	_, _, maskedDocument := fetchPath(defaultManager, "application/json", discoveryPath, "")
	masked := initialDocument.DeepCopy()
	masked.Items[0].Versions[0].Freshness = apidiscoveryv2.DiscoveryFreshnessStale

	require.Equal(t, masked.Items, maskedDocument.Items)

	// Wipe out default group. The other versions from the other group should now
	// appear since the group is not being overridden by defaults ource
	defaultManager.RemoveGroup(apis.Items[0].Name)

	_, _, resetDocument := fetchPath(defaultManager, "application/json", discoveryPath, "")
	require.Equal(t, resetDocument.Items, initialDocument.Items)
}

// Show the discovery manager is capable of serving requests to multiple users
// with unchanging data
func TestConcurrentRequests(t *testing.T) {
	manager := discoveryendpoint.NewResourceManager("apis")
	apis := fuzzAPIGroups(1, 3, 15)
	manager.SetGroups(apis.Items)

	waitGroup := sync.WaitGroup{}

	numReaders := 100
	numRequestsPerReader := 100

	// Spawn a bunch of readers that will keep sending requests to the server
	for i := 0; i < numReaders; i++ {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			etag := ""
			for j := 0; j < numRequestsPerReader; j++ {
				usedEtag := etag
				if j%2 == 0 {
					// Disable use of etag for every second request
					usedEtag = ""
				}
				response, body, document := fetchPath(manager, "application/json", discoveryPath, usedEtag)

				if usedEtag != "" {
					assert.Equal(t, http.StatusNotModified, response.StatusCode, "response should be Not Modified if etag was used")
					assert.Empty(t, body, "body should be empty if etag used")
				} else {
					assert.Equal(t, http.StatusOK, response.StatusCode, "response should be OK if etag was unused")
					assert.Equal(t, &apis, document, "document should be equal")
				}

				etag = response.Header.Get("ETag")
			}
		}()
	}
	waitGroup.Wait()
}

// Show the handler is capable of serving many concurrent readers and many
// concurrent writers without tripping up. Good to run with go '-race' detector
// since there are not many "correctness" checks
func TestAbuse(t *testing.T) {
	manager := discoveryendpoint.NewResourceManager("apis")

	numReaders := 100
	numRequestsPerReader := 1000

	numWriters := 10
	numWritesPerWriter := 1000

	waitGroup := sync.WaitGroup{}

	// Spawn a bunch of writers that randomly add groups, remove groups, and
	// reset the list of groups
	for i := 0; i < numWriters; i++ {
		source := rand.NewSource(int64(i))

		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()

			// track list of groups we've added so that we can remove them
			// randomly
			var addedGroups []metav1.GroupVersion

			for j := 0; j < numWritesPerWriter; j++ {
				switch source.Int63() % 3 {
				case 0:
					// Add a fuzzed group
					apis := fuzzAPIGroups(1, 2, 15)
					for _, group := range apis.Items {
						for _, version := range group.Versions {
							manager.AddGroupVersion(group.Name, version)
							addedGroups = append(addedGroups, metav1.GroupVersion{
								Group:   group.Name,
								Version: version.Version,
							})
						}
					}
				case 1:
					// Remove a group that we have added
					if len(addedGroups) > 0 {
						manager.RemoveGroupVersion(addedGroups[0])
						addedGroups = addedGroups[1:]
					} else {
						// Send a request and try to remove a group someone else
						// might have added
						_, _, document := fetchPath(manager, "application/json", discoveryPath, "")
						assert.NotNil(t, document, "manager should always succeed in returning a document")

						if len(document.Items) > 0 {
							manager.RemoveGroupVersion(metav1.GroupVersion{
								Group:   document.Items[0].Name,
								Version: document.Items[0].Versions[0].Version,
							})
						}

					}
				case 2:
					manager.SetGroups(nil)
					addedGroups = nil
				default:
					panic("unreachable")
				}
			}
		}()
	}

	// Spawn a bunch of readers that will keep sending requests to the server
	// and making sure the response makes sense
	for i := 0; i < numReaders; i++ {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()

			etag := ""
			for j := 0; j < numRequestsPerReader; j++ {
				response, body, document := fetchPath(manager, "application/json", discoveryPath, etag)

				if response.StatusCode == http.StatusNotModified {
					assert.Equal(t, etag, response.Header.Get("ETag"))
					assert.Empty(t, body, "body should be empty if etag used")
					assert.Nil(t, document)
				} else {
					assert.Equal(t, http.StatusOK, response.StatusCode, "response should be OK if etag was unused")
					assert.NotNil(t, document)
				}

				etag = response.Header.Get("ETag")
			}
		}()
	}

	waitGroup.Wait()
}

func TestVersionSortingNoPriority(t *testing.T) {
	manager := discoveryendpoint.NewResourceManager("apis")

	manager.AddGroupVersion("default", apidiscoveryv2.APIVersionDiscovery{
		Version: "v1alpha1",
	})
	manager.AddGroupVersion("default", apidiscoveryv2.APIVersionDiscovery{
		Version: "v2beta1",
	})
	manager.AddGroupVersion("default", apidiscoveryv2.APIVersionDiscovery{
		Version: "v1",
	})
	manager.AddGroupVersion("default", apidiscoveryv2.APIVersionDiscovery{
		Version: "v1beta1",
	})
	manager.AddGroupVersion("default", apidiscoveryv2.APIVersionDiscovery{
		Version: "v2",
	})

	response, _, decoded := fetchPath(manager, "application/json", discoveryPath, "")
	assert.Equal(t, http.StatusOK, response.StatusCode, "response should be 200 OK")

	versions := decoded.Items[0].Versions

	// Ensure that v1 is sorted before v1alpha1
	assert.Equal(t, "v2", versions[0].Version)
	assert.Equal(t, "v1", versions[1].Version)
	assert.Equal(t, "v2beta1", versions[2].Version)
	assert.Equal(t, "v1beta1", versions[3].Version)
	assert.Equal(t, "v1alpha1", versions[4].Version)
}

func TestVersionSortingWithPriority(t *testing.T) {
	manager := discoveryendpoint.NewResourceManager("apis")

	manager.AddGroupVersion("default", apidiscoveryv2.APIVersionDiscovery{
		Version: "v1",
	})
	manager.SetGroupVersionPriority(metav1.GroupVersion{Group: "default", Version: "v1"}, 1000, 100)
	manager.AddGroupVersion("default", apidiscoveryv2.APIVersionDiscovery{
		Version: "v1alpha1",
	})
	manager.SetGroupVersionPriority(metav1.GroupVersion{Group: "default", Version: "v1alpha1"}, 1000, 200)

	response, _, decoded := fetchPath(manager, "application/json", discoveryPath, "")
	assert.Equal(t, http.StatusOK, response.StatusCode, "response should be 200 OK")

	versions := decoded.Items[0].Versions

	// Ensure that reverse alpha sort order can be overridden by setting group version priorities.
	assert.Equal(t, "v1alpha1", versions[0].Version)
	assert.Equal(t, "v1", versions[1].Version)
}

// if two apiservices declare conflicting priorities for their group priority, take the higher one.
func TestGroupVersionSortingConflictingPriority(t *testing.T) {
	manager := discoveryendpoint.NewResourceManager("apis")

	manager.AddGroupVersion("default", apidiscoveryv2.APIVersionDiscovery{
		Version: "v1",
	})
	manager.SetGroupVersionPriority(metav1.GroupVersion{Group: "default", Version: "v1"}, 1000, 100)
	manager.AddGroupVersion("test", apidiscoveryv2.APIVersionDiscovery{
		Version: "v1alpha1",
	})
	manager.SetGroupVersionPriority(metav1.GroupVersion{Group: "test", Version: "v1alpha1"}, 500, 100)
	manager.AddGroupVersion("test", apidiscoveryv2.APIVersionDiscovery{
		Version: "v1alpha2",
	})
	manager.SetGroupVersionPriority(metav1.GroupVersion{Group: "test", Version: "v1alpha1"}, 2000, 100)

	response, _, decoded := fetchPath(manager, "application/json", discoveryPath, "")
	assert.Equal(t, http.StatusOK, response.StatusCode, "response should be 200 OK")

	groups := decoded.Items

	// Ensure that reverse alpha sort order can be overridden by setting group version priorities.
	assert.Equal(t, "test", groups[0].Name)
	assert.Equal(t, "default", groups[1].Name)
}

// Show that the GroupPriorityMinimum is not sticky if a higher group version is removed
// after a lower one is added
func TestStatelessGroupPriorityMinimum(t *testing.T) {
	manager := discoveryendpoint.NewResourceManager("apis")

	stableGroup := "stable.example.com"
	experimentalGroup := "experimental.example.com"

	manager.AddGroupVersion(stableGroup, apidiscoveryv2.APIVersionDiscovery{
		Version: "v1",
	})
	manager.SetGroupVersionPriority(metav1.GroupVersion{Group: stableGroup, Version: "v1"}, 1000, 100)

	manager.AddGroupVersion(experimentalGroup, apidiscoveryv2.APIVersionDiscovery{
		Version: "v1",
	})
	manager.SetGroupVersionPriority(metav1.GroupVersion{Group: experimentalGroup, Version: "v1"}, 100, 100)

	manager.AddGroupVersion(experimentalGroup, apidiscoveryv2.APIVersionDiscovery{
		Version: "v1alpha1",
	})
	manager.SetGroupVersionPriority(metav1.GroupVersion{Group: experimentalGroup, Version: "v1alpha1"}, 10000, 100)

	// Expect v1alpha1's group priority to be used and sort it first in the list
	response, _, decoded := fetchPath(manager, "application/json", discoveryPath, "")
	assert.Equal(t, http.StatusOK, response.StatusCode, "response should be 200 OK")
	assert.Equal(t, "experimental.example.com", decoded.Items[0].Name)
	assert.Equal(t, "stable.example.com", decoded.Items[1].Name)

	// Remove v1alpha1 and expect the new lower priority to take hold
	manager.RemoveGroupVersion(metav1.GroupVersion{Group: experimentalGroup, Version: "v1alpha1"})

	response, _, decoded = fetchPath(manager, "application/json", discoveryPath, "")
	assert.Equal(t, http.StatusOK, response.StatusCode, "response should be 200 OK")

	assert.Equal(t, "stable.example.com", decoded.Items[0].Name)
	assert.Equal(t, "experimental.example.com", decoded.Items[1].Name)
}
