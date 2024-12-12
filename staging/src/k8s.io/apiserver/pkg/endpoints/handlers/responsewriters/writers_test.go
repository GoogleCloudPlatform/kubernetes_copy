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

package responsewriters

import (
	"bytes"
	"compress/gzip"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	v1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/apiserver/pkg/features"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
)

const benchmarkSeed = 100

func TestSerializeObjectParallel(t *testing.T) {
	largePayload := bytes.Repeat([]byte("0123456789abcdef"), defaultGzipThresholdBytes/16+1)
	type test struct {
		name string

		mediaType  string
		out        []byte
		outErrs    []error
		req        *http.Request
		statusCode int
		object     runtime.Object

		wantCode    int
		wantHeaders http.Header
	}
	newTest := func() test {
		return test{
			name:      "compress on gzip",
			out:       largePayload,
			mediaType: "application/json",
			req: &http.Request{
				Header: http.Header{
					"Accept-Encoding": []string{"gzip"},
				},
				URL: &url.URL{Path: "/path"},
			},
			wantCode: http.StatusOK,
			wantHeaders: http.Header{
				"Content-Type":     []string{"application/json"},
				"Content-Encoding": []string{"gzip"},
				"Vary":             []string{"Accept-Encoding"},
			},
		}
	}
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.APIResponseCompression, true)
	for i := 0; i < 100; i++ {
		ctt := newTest()
		t.Run(ctt.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("recovered from err %v", r)
				}
			}()
			t.Parallel()

			encoder := &fakeEncoder{
				buf:  ctt.out,
				errs: ctt.outErrs,
			}
			if ctt.statusCode == 0 {
				ctt.statusCode = http.StatusOK
			}
			recorder := &fakeResponseRecorder{
				ResponseRecorder:   httptest.NewRecorder(),
				fe:                 encoder,
				errorAfterEncoding: true,
			}
			SerializeObject(ctt.mediaType, encoder, recorder, ctt.req, ctt.statusCode, ctt.object)
			result := recorder.Result()
			if result.StatusCode != ctt.wantCode {
				t.Fatalf("unexpected code: %v", result.StatusCode)
			}
			if !reflect.DeepEqual(result.Header, ctt.wantHeaders) {
				t.Fatal(cmp.Diff(ctt.wantHeaders, result.Header))
			}
		})
	}
}

func TestSerializeObject(t *testing.T) {
	smallPayload := []byte("{test-object,test-object}")
	largePayload := bytes.Repeat([]byte("0123456789abcdef"), defaultGzipThresholdBytes/16+1)
	tests := []struct {
		name string

		compressionEnabled bool

		mediaType  string
		out        []byte
		outErrs    []error
		req        *http.Request
		statusCode int
		object     runtime.Object

		wantCode    int
		wantHeaders http.Header
		wantBody    []byte
	}{
		{
			name:        "serialize object",
			out:         smallPayload,
			req:         &http.Request{Header: http.Header{}, URL: &url.URL{Path: "/path"}},
			wantCode:    http.StatusOK,
			wantHeaders: http.Header{"Content-Type": []string{""}},
			wantBody:    smallPayload,
		},

		{
			name:        "return content type",
			out:         smallPayload,
			mediaType:   "application/json",
			req:         &http.Request{Header: http.Header{}, URL: &url.URL{Path: "/path"}},
			wantCode:    http.StatusOK,
			wantHeaders: http.Header{"Content-Type": []string{"application/json"}},
			wantBody:    smallPayload,
		},

		{
			name:        "return status code",
			statusCode:  http.StatusBadRequest,
			out:         smallPayload,
			mediaType:   "application/json",
			req:         &http.Request{Header: http.Header{}, URL: &url.URL{Path: "/path"}},
			wantCode:    http.StatusBadRequest,
			wantHeaders: http.Header{"Content-Type": []string{"application/json"}},
			wantBody:    smallPayload,
		},

		{
			name:        "fail to encode object",
			out:         smallPayload,
			outErrs:     []error{fmt.Errorf("bad")},
			mediaType:   "application/json",
			req:         &http.Request{Header: http.Header{}, URL: &url.URL{Path: "/path"}},
			wantCode:    http.StatusInternalServerError,
			wantHeaders: http.Header{"Content-Type": []string{"application/json"}},
			wantBody:    smallPayload,
		},

		{
			name:        "fail to encode object or status",
			out:         smallPayload,
			outErrs:     []error{fmt.Errorf("bad"), fmt.Errorf("bad2")},
			mediaType:   "application/json",
			req:         &http.Request{Header: http.Header{}, URL: &url.URL{Path: "/path"}},
			wantCode:    http.StatusInternalServerError,
			wantHeaders: http.Header{"Content-Type": []string{"text/plain"}},
			wantBody:    []byte(": bad"),
		},

		{
			name:        "fail to encode object or status with status code",
			out:         smallPayload,
			outErrs:     []error{kerrors.NewNotFound(schema.GroupResource{}, "test"), fmt.Errorf("bad2")},
			mediaType:   "application/json",
			req:         &http.Request{Header: http.Header{}, URL: &url.URL{Path: "/path"}},
			statusCode:  http.StatusOK,
			wantCode:    http.StatusNotFound,
			wantHeaders: http.Header{"Content-Type": []string{"text/plain"}},
			wantBody:    []byte("NotFound:  \"test\" not found"),
		},

		{
			name:        "fail to encode object or status with status code and keeps previous error",
			out:         smallPayload,
			outErrs:     []error{kerrors.NewNotFound(schema.GroupResource{}, "test"), fmt.Errorf("bad2")},
			mediaType:   "application/json",
			req:         &http.Request{Header: http.Header{}, URL: &url.URL{Path: "/path"}},
			statusCode:  http.StatusNotAcceptable,
			wantCode:    http.StatusNotAcceptable,
			wantHeaders: http.Header{"Content-Type": []string{"text/plain"}},
			wantBody:    []byte("NotFound:  \"test\" not found"),
		},

		{
			name:      "compression requires feature gate",
			out:       largePayload,
			mediaType: "application/json",
			req: &http.Request{
				Header: http.Header{
					"Accept-Encoding": []string{"gzip"},
				},
				URL: &url.URL{Path: "/path"},
			},
			wantCode:    http.StatusOK,
			wantHeaders: http.Header{"Content-Type": []string{"application/json"}},
			wantBody:    largePayload,
		},

		{
			name:               "compress on gzip",
			compressionEnabled: true,
			out:                largePayload,
			mediaType:          "application/json",
			req: &http.Request{
				Header: http.Header{
					"Accept-Encoding": []string{"gzip"},
				},
				URL: &url.URL{Path: "/path"},
			},
			wantCode: http.StatusOK,
			wantHeaders: http.Header{
				"Content-Type":     []string{"application/json"},
				"Content-Encoding": []string{"gzip"},
				"Vary":             []string{"Accept-Encoding"},
			},
			wantBody: gzipContent(largePayload, defaultGzipContentEncodingLevel),
		},

		{
			name:               "compression is not performed on small objects",
			compressionEnabled: true,
			out:                smallPayload,
			mediaType:          "application/json",
			req: &http.Request{
				Header: http.Header{
					"Accept-Encoding": []string{"gzip"},
				},
				URL: &url.URL{Path: "/path"},
			},
			wantCode: http.StatusOK,
			wantHeaders: http.Header{
				"Content-Type": []string{"application/json"},
			},
			wantBody: smallPayload,
		},

		{
			name:               "compress when multiple encodings are requested",
			compressionEnabled: true,
			out:                largePayload,
			mediaType:          "application/json",
			req: &http.Request{
				Header: http.Header{
					"Accept-Encoding": []string{"deflate, , gzip,"},
				},
				URL: &url.URL{Path: "/path"},
			},
			wantCode: http.StatusOK,
			wantHeaders: http.Header{
				"Content-Type":     []string{"application/json"},
				"Content-Encoding": []string{"gzip"},
				"Vary":             []string{"Accept-Encoding"},
			},
			wantBody: gzipContent(largePayload, defaultGzipContentEncodingLevel),
		},

		{
			name:               "ignore compression on deflate",
			compressionEnabled: true,
			out:                largePayload,
			mediaType:          "application/json",
			req: &http.Request{
				Header: http.Header{
					"Accept-Encoding": []string{"deflate"},
				},
				URL: &url.URL{Path: "/path"},
			},
			wantCode: http.StatusOK,
			wantHeaders: http.Header{
				"Content-Type": []string{"application/json"},
			},
			wantBody: largePayload,
		},

		{
			name:               "ignore compression on unrecognized types",
			compressionEnabled: true,
			out:                largePayload,
			mediaType:          "application/json",
			req: &http.Request{
				Header: http.Header{
					"Accept-Encoding": []string{", ,  other, nothing, what, "},
				},
				URL: &url.URL{Path: "/path"},
			},
			wantCode: http.StatusOK,
			wantHeaders: http.Header{
				"Content-Type": []string{"application/json"},
			},
			wantBody: largePayload,
		},

		{
			name:               "errors are compressed",
			compressionEnabled: true,
			statusCode:         http.StatusInternalServerError,
			out:                smallPayload,
			outErrs:            []error{errors.New(string(largePayload)), errors.New("bad2")},
			mediaType:          "application/json",
			req: &http.Request{
				Header: http.Header{
					"Accept-Encoding": []string{"gzip"},
				},
				URL: &url.URL{Path: "/path"},
			},
			wantCode: http.StatusInternalServerError,
			wantHeaders: http.Header{
				"Content-Type":     []string{"text/plain"},
				"Content-Encoding": []string{"gzip"},
				"Vary":             []string{"Accept-Encoding"},
			},
			wantBody: gzipContent([]byte(": "+string(largePayload)), defaultGzipContentEncodingLevel),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.APIResponseCompression, tt.compressionEnabled)

			encoder := &fakeEncoder{
				buf:  tt.out,
				errs: tt.outErrs,
			}
			if tt.statusCode == 0 {
				tt.statusCode = http.StatusOK
			}
			recorder := httptest.NewRecorder()
			SerializeObject(tt.mediaType, encoder, recorder, tt.req, tt.statusCode, tt.object)
			result := recorder.Result()
			if result.StatusCode != tt.wantCode {
				t.Fatalf("unexpected code: %v", result.StatusCode)
			}
			if !reflect.DeepEqual(result.Header, tt.wantHeaders) {
				t.Fatal(cmp.Diff(tt.wantHeaders, result.Header))
			}
			body, _ := io.ReadAll(result.Body)
			if !bytes.Equal(tt.wantBody, body) {
				t.Fatalf("wanted:\n%s\ngot:\n%s", hex.Dump(tt.wantBody), hex.Dump(body))
			}
		})
	}
}

func randTime(t *time.Time, r *rand.Rand) {
	*t = time.Unix(r.Int63n(1000*365*24*60*60), r.Int63())
}

func randIP(s *string, r *rand.Rand) {
	*s = fmt.Sprintf("10.20.%d.%d", r.Int31n(256), r.Int31n(256))
}

// randPod changes fields in pod to mimic another pod from the same replicaset.
// The list fields here has been generated by picking two pods in the same replicaset
// and checking diff of their jsons.
func randPod(b *testing.B, pod *v1.Pod, r *rand.Rand) {
	pod.Name = fmt.Sprintf("%s-%x", pod.GenerateName, r.Int63n(1000))
	pod.UID = uuid.NewUUID()
	pod.ResourceVersion = strconv.Itoa(r.Int())
	pod.Spec.NodeName = fmt.Sprintf("some-node-prefix-%x", r.Int63n(1000))

	randTime(&pod.CreationTimestamp.Time, r)
	randTime(&pod.Status.StartTime.Time, r)
	for i := range pod.Status.Conditions {
		randTime(&pod.Status.Conditions[i].LastTransitionTime.Time, r)
	}
	for i := range pod.Status.ContainerStatuses {
		containerStatus := &pod.Status.ContainerStatuses[i]
		state := &containerStatus.State
		if state.Running != nil {
			randTime(&state.Running.StartedAt.Time, r)
		}
		containerStatus.ContainerID = fmt.Sprintf("docker://%x%x%x%x", r.Int63(), r.Int63(), r.Int63(), r.Int63())
	}
	for i := range pod.ManagedFields {
		randTime(&pod.ManagedFields[i].Time.Time, r)
	}

	randIP(&pod.Status.HostIP, r)
	randIP(&pod.Status.PodIP, r)
}

func benchmarkItems(b *testing.B, file string, n int) *v1.PodList {
	pod := v1.Pod{}
	f, err := os.Open(file)
	if err != nil {
		b.Fatalf("Failed to open %q: %v", file, err)
	}
	defer f.Close()
	err = json.NewDecoder(f).Decode(&pod)
	if err != nil {
		b.Fatalf("Failed to decode %q: %v", file, err)
	}

	list := &v1.PodList{
		Items: make([]v1.Pod, n),
	}

	r := rand.New(rand.NewSource(benchmarkSeed))
	for i := 0; i < n; i++ {
		list.Items[i] = *pod.DeepCopy()
		randPod(b, &list.Items[i], r)
	}
	return list
}

func toProtoBuf(b *testing.B, list *v1.PodList) []byte {
	out, err := list.Marshal()
	if err != nil {
		b.Fatalf("Failed to marshal list to protobuf: %v", err)
	}
	return out
}

func toJSON(b *testing.B, list *v1.PodList) []byte {
	out, err := json.Marshal(list)
	if err != nil {
		b.Fatalf("Failed to marshal list to json: %v", err)
	}
	return out
}

func benchmarkSerializeObject(b *testing.B, payload []byte) {
	input, output := len(payload), len(gzipContent(payload, defaultGzipContentEncodingLevel))
	b.Logf("Payload size: %d, expected output size: %d, ratio: %.2f", input, output, float64(output)/float64(input))

	req := &http.Request{
		Header: http.Header{
			"Accept-Encoding": []string{"gzip"},
		},
		URL: &url.URL{Path: "/path"},
	}
	featuregatetesting.SetFeatureGateDuringTest(b, utilfeature.DefaultFeatureGate, features.APIResponseCompression, true)

	encoder := &fakeEncoder{
		buf: payload,
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		recorder := httptest.NewRecorder()
		SerializeObject("application/json", encoder, recorder, req, http.StatusOK, nil /* object */)
		result := recorder.Result()
		if result.StatusCode != http.StatusOK {
			b.Fatalf("incorrect status code: got %v;  want: %v", result.StatusCode, http.StatusOK)
		}
	}
}

func BenchmarkSerializeObject1000PodsPB(b *testing.B) {
	benchmarkSerializeObject(b, toProtoBuf(b, benchmarkItems(b, "testdata/pod.json", 1000)))
}
func BenchmarkSerializeObject10000PodsPB(b *testing.B) {
	benchmarkSerializeObject(b, toProtoBuf(b, benchmarkItems(b, "testdata/pod.json", 10000)))
}
func BenchmarkSerializeObject100000PodsPB(b *testing.B) {
	benchmarkSerializeObject(b, toProtoBuf(b, benchmarkItems(b, "testdata/pod.json", 100000)))
}

func BenchmarkSerializeObject1000PodsJSON(b *testing.B) {
	benchmarkSerializeObject(b, toJSON(b, benchmarkItems(b, "testdata/pod.json", 1000)))
}
func BenchmarkSerializeObject10000PodsJSON(b *testing.B) {
	benchmarkSerializeObject(b, toJSON(b, benchmarkItems(b, "testdata/pod.json", 10000)))
}
func BenchmarkSerializeObject100000PodsJSON(b *testing.B) {
	benchmarkSerializeObject(b, toJSON(b, benchmarkItems(b, "testdata/pod.json", 100000)))
}

type fakeResponseRecorder struct {
	*httptest.ResponseRecorder
	fe                 *fakeEncoder
	errorAfterEncoding bool
}

func (frw *fakeResponseRecorder) Write(buf []byte) (int, error) {
	if frw.errorAfterEncoding && frw.fe.encodeCalled {
		return 0, errors.New("returning a requested error")
	}
	return frw.ResponseRecorder.Write(buf)
}

type fakeEncoder struct {
	obj  runtime.Object
	buf  []byte
	errs []error

	encodeCalled bool
}

func (e *fakeEncoder) Encode(obj runtime.Object, w io.Writer) error {
	e.obj = obj
	if len(e.errs) > 0 {
		err := e.errs[0]
		e.errs = e.errs[1:]
		return err
	}
	_, err := w.Write(e.buf)
	e.encodeCalled = true
	return err
}

func (e *fakeEncoder) Identifier() runtime.Identifier {
	return runtime.Identifier("fake")
}

func gzipContent(data []byte, level int) []byte {
	buf := &bytes.Buffer{}
	gw, err := gzip.NewWriterLevel(buf, level)
	if err != nil {
		panic(err)
	}
	if _, err := gw.Write(data); err != nil {
		panic(err)
	}
	if err := gw.Close(); err != nil {
		panic(err)
	}
	return buf.Bytes()
}
