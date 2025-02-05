//go:build !windows
// +build !windows

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

// Package kmsv2 transforms values for storage at rest using a Envelope v2 provider
package kmsv2

import (
	"bytes"
	"context"
	"encoding/base64"
	"flag"
	"fmt"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gogo/protobuf/proto"
	"go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	utilrand "k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/apimachinery/pkg/util/uuid"
	genericapirequest "k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/storage/value"
	kmstypes "k8s.io/apiserver/pkg/storage/value/encrypt/envelope/kmsv2/v2"
	"k8s.io/apiserver/pkg/storage/value/encrypt/envelope/metrics"
	"k8s.io/component-base/metrics/legacyregistry"
	"k8s.io/component-base/metrics/testutil"
	"k8s.io/klog/v2"
	kmsservice "k8s.io/kms/pkg/service"
	"k8s.io/utils/clock"
	testingclock "k8s.io/utils/clock/testing"
)

const (
	testText            = "abcdefghijklmnopqrstuvwxyz"
	testContextText     = "0123456789"
	testKeyHash         = "sha256:6b86b273ff34fce19d6b804eff5a3f5747ada4eaa22f1d49c01e52ddb7875b4b"
	testKeyVersion      = "1"
	testAPIServerID     = "testAPIServerID"
	testAPIServerIDHash = "sha256:14f9d63e669337ac6bfda2e2162915ee6a6067743eddd4e5c374b572f951ff37"
)

// testEnvelopeService is a mock Envelope service which can be used to simulate remote Envelope services
// for testing of Envelope based encryption providers.
type testEnvelopeService struct {
	annotations  map[string][]byte
	disabled     bool
	keyVersion   string
	ciphertext   []byte
	decryptCalls int32
}

func (t *testEnvelopeService) Decrypt(ctx context.Context, uid string, req *kmsservice.DecryptRequest) ([]byte, error) {
	atomic.AddInt32(&t.decryptCalls, 1)
	if t.disabled {
		return nil, fmt.Errorf("Envelope service was disabled")
	}
	if len(uid) == 0 {
		return nil, fmt.Errorf("uid is required")
	}
	if len(req.KeyID) == 0 {
		return nil, fmt.Errorf("keyID is required")
	}
	return base64.StdEncoding.DecodeString(string(req.Ciphertext))
}

func (t *testEnvelopeService) Encrypt(ctx context.Context, uid string, data []byte) (*kmsservice.EncryptResponse, error) {
	if t.disabled {
		return nil, fmt.Errorf("Envelope service was disabled")
	}
	if len(uid) == 0 {
		return nil, fmt.Errorf("uid is required")
	}
	annotations := make(map[string][]byte)
	if t.annotations != nil {
		for k, v := range t.annotations {
			annotations[k] = v
		}
	} else {
		annotations["local-kek.kms.kubernetes.io"] = []byte("encrypted-local-kek")
	}

	ciphertext := t.ciphertext
	if ciphertext == nil {
		ciphertext = []byte(base64.StdEncoding.EncodeToString(data))
	}

	return &kmsservice.EncryptResponse{Ciphertext: ciphertext, KeyID: t.keyVersion, Annotations: annotations}, nil
}

func (t *testEnvelopeService) Status(ctx context.Context) (*kmsservice.StatusResponse, error) {
	if t.disabled {
		return nil, fmt.Errorf("Envelope service was disabled")
	}
	return &kmsservice.StatusResponse{KeyID: t.keyVersion}, nil
}

func (t *testEnvelopeService) SetDisabledStatus(status bool) {
	t.disabled = status
}

func (t *testEnvelopeService) SetAnnotations(annotations map[string][]byte) {
	t.annotations = annotations
}

func (t *testEnvelopeService) SetCiphertext(ciphertext []byte) {
	t.ciphertext = ciphertext
}

func (t *testEnvelopeService) Rotate() {
	i, _ := strconv.Atoi(t.keyVersion)
	t.keyVersion = strconv.FormatInt(int64(i+1), 10)
}

func newTestEnvelopeService() *testEnvelopeService {
	return &testEnvelopeService{
		keyVersion: testKeyVersion,
	}
}

// Throw error if Envelope transformer tries to contact Envelope without hitting cache.
func TestEnvelopeCaching(t *testing.T) {
	testCases := []struct {
		desc                     string
		cacheTTL                 time.Duration
		simulateKMSPluginFailure bool
		expectedError            string
		expectedDecryptCalls     int
	}{
		{
			desc:                     "entry in cache should withstand plugin failure",
			cacheTTL:                 5 * time.Minute,
			simulateKMSPluginFailure: true,
			expectedDecryptCalls:     0, // should not hit KMS plugin
		},
		{
			desc:                     "cache entry expired should not withstand plugin failure",
			cacheTTL:                 1 * time.Millisecond,
			simulateKMSPluginFailure: true,
			expectedError:            "failed to decrypt DEK, error: Envelope service was disabled",
			expectedDecryptCalls:     10, // should hit KMS plugin for each read after cache entry expired and fail
		},
		{
			desc:                     "cache entry expired should work after cache refresh",
			cacheTTL:                 1 * time.Millisecond,
			simulateKMSPluginFailure: false,
			expectedDecryptCalls:     1, // should hit KMS plugin just for the 1st read after cache entry expired
		},
	}

	for _, tt := range testCases {
		t.Run(tt.desc, func(t *testing.T) {
			ctx := testContext(t)

			envelopeService := newTestEnvelopeService()
			fakeClock := testingclock.NewFakeClock(time.Now())

			useSeed := randomBool()

			state, err := testStateFunc(ctx, envelopeService, fakeClock, useSeed)()
			if err != nil {
				t.Fatal(err)
			}

			transformer := newEnvelopeTransformerWithClock(envelopeService, testProviderName,
				func() (State, error) { return state, nil }, testAPIServerID,
				tt.cacheTTL, fakeClock)

			dataCtx := value.DefaultContext(testContextText)
			originalText := []byte(testText)

			transformedData, err := transformer.TransformToStorage(ctx, originalText, dataCtx)
			if err != nil {
				t.Fatalf("envelopeTransformer: error while transforming data to storage: %s", err)
			}
			untransformedData, _, err := transformer.TransformFromStorage(ctx, transformedData, dataCtx)
			if err != nil {
				t.Fatalf("could not decrypt Envelope transformer's encrypted data even once: %v", err)
			}
			if !bytes.Equal(untransformedData, originalText) {
				t.Fatalf("envelopeTransformer transformed data incorrectly. Expected: %v, got %v", originalText, untransformedData)
			}

			// advance the clock to allow cache entries to expire depending on TTL
			fakeClock.Step(2 * time.Minute)
			// force GC to run by performing a write
			transformer.(*envelopeTransformer).cache.set([]byte("some-other-unrelated-key"), &envelopeTransformer{})

			state, err = testStateFunc(ctx, envelopeService, fakeClock, useSeed)()
			if err != nil {
				t.Fatal(err)
			}
			envelopeService.SetDisabledStatus(tt.simulateKMSPluginFailure)

			for i := 0; i < 10; i++ {
				// Subsequent reads for the same data should work fine due to caching.
				untransformedData, _, err = transformer.TransformFromStorage(ctx, transformedData, dataCtx)
				if tt.expectedError != "" {
					if err == nil {
						t.Fatalf("expected error: %v, got nil", tt.expectedError)
					}
					if err.Error() != tt.expectedError {
						t.Fatalf("expected error: %v, got: %v", tt.expectedError, err)
					}
				} else {
					if err != nil {
						t.Fatalf("unexpected error: %v", err)
					}
					if !bytes.Equal(untransformedData, originalText) {
						t.Fatalf("envelopeTransformer transformed data incorrectly. Expected: %v, got %v", originalText, untransformedData)
					}
				}
			}
			if int(envelopeService.decryptCalls) != tt.expectedDecryptCalls {
				t.Fatalf("expected %d decrypt calls, got %d", tt.expectedDecryptCalls, envelopeService.decryptCalls)
			}
		})
	}
}

func testStateFunc(ctx context.Context, envelopeService kmsservice.Service, clock clock.Clock, useSeed bool) func() (State, error) {
	return func() (State, error) {
		transformer, encObject, cacheKey, errGen := GenerateTransformer(ctx, string(uuid.NewUUID()), envelopeService, useSeed)
		if errGen != nil {
			return State{}, errGen
		}
		return State{
			Transformer:         transformer,
			EncryptedObject:     *encObject,
			UID:                 "panda",
			ExpirationTimestamp: clock.Now().Add(time.Hour),
			CacheKey:            cacheKey,
		}, nil
	}
}

// TestEnvelopeTransformerStaleness validates that staleness checks on read honor the data returned from the StateFunc.
func TestEnvelopeTransformerStaleness(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		desc          string
		expectedStale bool
		testErr       error
		testKeyID     string
		useSeedWrite  bool
		useSeedRead   bool
	}{
		{
			desc:          "stateFunc returns err",
			expectedStale: false,
			testErr:       fmt.Errorf("failed to perform status section of the healthz check for KMS Provider"),
			testKeyID:     "",
		},
		{
			desc:          "stateFunc returns same keyID, not using seed",
			expectedStale: false,
			testErr:       nil,
			testKeyID:     testKeyVersion,
		},
		{
			desc:          "stateFunc returns same keyID, using seed",
			expectedStale: false,
			testErr:       nil,
			testKeyID:     testKeyVersion,
			useSeedWrite:  true,
			useSeedRead:   true,
		},
		{
			desc:          "stateFunc returns same keyID, migrating away from seed",
			expectedStale: true,
			testErr:       nil,
			testKeyID:     testKeyVersion,
			useSeedWrite:  true,
			useSeedRead:   false,
		},
		{
			desc:          "stateFunc returns same keyID, migrating to seed",
			expectedStale: true,
			testErr:       nil,
			testKeyID:     testKeyVersion,
			useSeedWrite:  false,
			useSeedRead:   true,
		},
		{
			desc:          "stateFunc returns different keyID",
			expectedStale: true,
			testErr:       nil,
			testKeyID:     "2",
		},
	}

	for _, tt := range testCases {
		tt := tt
		t.Run(tt.desc, func(t *testing.T) {
			t.Parallel()

			ctx := testContext(t)

			envelopeService := newTestEnvelopeService()
			state, err := testStateFunc(ctx, envelopeService, clock.RealClock{}, tt.useSeedWrite)()
			if err != nil {
				t.Fatal(err)
			}
			var stateErr error

			transformer := NewEnvelopeTransformer(envelopeService, testProviderName,
				func() (State, error) { return state, stateErr }, testAPIServerID,
			)

			dataCtx := value.DefaultContext(testContextText)
			originalText := []byte(testText)

			transformedData, err := transformer.TransformToStorage(ctx, originalText, dataCtx)
			if err != nil {
				t.Fatalf("envelopeTransformer: error while transforming data (%v) to storage: %s", originalText, err)
			}

			// inject test data before performing a read
			state.EncryptedObject.KeyID = tt.testKeyID
			if tt.useSeedRead {
				state.EncryptedObject.EncryptedDEKSourceType = kmstypes.EncryptedDEKSourceType_HKDF_SHA256_XNONCE_AES_GCM_SEED
			} else {
				state.EncryptedObject.EncryptedDEKSourceType = kmstypes.EncryptedDEKSourceType_AES_GCM_KEY
			}
			stateErr = tt.testErr

			_, stale, err := transformer.TransformFromStorage(ctx, transformedData, dataCtx)
			if tt.testErr != nil {
				if err == nil {
					t.Fatalf("envelopeTransformer: expected error: %v, got nil", tt.testErr)
				}
				if err.Error() != tt.testErr.Error() {
					t.Fatalf("envelopeTransformer: expected error: %v, got: %v", tt.testErr, err)
				}
			} else {
				if err != nil {
					t.Fatalf("envelopeTransformer: unexpected error: %v", err)
				}
				if stale != tt.expectedStale {
					t.Fatalf("envelopeTransformer TransformFromStorage determined keyID staleness incorrectly, expected: %v, got %v", tt.expectedStale, stale)
				}
			}
		})
	}
}

func TestEnvelopeTransformerStateFunc(t *testing.T) {
	t.Parallel()

	ctx := testContext(t)

	useSeed := randomBool()

	envelopeService := newTestEnvelopeService()
	state, err := testStateFunc(ctx, envelopeService, clock.RealClock{}, useSeed)()
	if err != nil {
		t.Fatal(err)
	}

	// start with a broken state
	stateErr := fmt.Errorf("some state error")

	transformer := NewEnvelopeTransformer(envelopeService, testProviderName,
		func() (State, error) { return state, stateErr }, testAPIServerID,
	)

	dataCtx := value.DefaultContext(testContextText)
	originalText := []byte(testText)

	t.Run("nothing works when the state is broken", func(t *testing.T) {
		_, err := transformer.TransformToStorage(ctx, originalText, dataCtx)
		if err != stateErr {
			t.Fatalf("expected state error, got: %v", err)
		}
		o := &kmstypes.EncryptedObject{
			EncryptedData:      []byte{1},
			KeyID:              "2",
			EncryptedDEKSource: []byte{3},
			Annotations:        nil,
		}
		if useSeed {
			o.EncryptedDEKSourceType = kmstypes.EncryptedDEKSourceType_HKDF_SHA256_XNONCE_AES_GCM_SEED
		} else {
			o.EncryptedDEKSourceType = kmstypes.EncryptedDEKSourceType_AES_GCM_KEY
		}
		data, err := proto.Marshal(o)
		if err != nil {
			t.Fatal(err)
		}
		_, _, err = transformer.TransformFromStorage(ctx, data, dataCtx)
		if err != stateErr {
			t.Fatalf("expected state error, got: %v", err)
		}
	})

	// fix the state
	stateErr = nil

	var encryptedData []byte

	t.Run("everything works when the state is fixed", func(t *testing.T) {
		encryptedData, err = transformer.TransformToStorage(ctx, originalText, dataCtx)
		if err != nil {
			t.Fatal(err)
		}
		_, _, err = transformer.TransformFromStorage(ctx, encryptedData, dataCtx)
		if err != nil {
			t.Fatal(err)
		}
	})

	// break the plugin
	envelopeService.SetDisabledStatus(true)

	t.Run("everything works even when the plugin is down but the state is valid", func(t *testing.T) {
		data, err := transformer.TransformToStorage(ctx, originalText, dataCtx)
		if err != nil {
			t.Fatal(err)
		}
		_, _, err = transformer.TransformFromStorage(ctx, data, dataCtx)
		if err != nil {
			t.Fatal(err)
		}
	})

	// make the state invalid
	state.ExpirationTimestamp = time.Now().Add(-time.Hour)

	t.Run("writes fail when the plugin is down and the state is invalid", func(t *testing.T) {
		_, err := transformer.TransformToStorage(ctx, originalText, dataCtx)
		if !strings.Contains(errString(err), `encryptedDEKSource with keyID hash "sha256:6b86b273ff34fce19d6b804eff5a3f5747ada4eaa22f1d49c01e52ddb7875b4b" expired at`) {
			t.Fatalf("expected expiration error, got: %v", err)
		}
	})

	t.Run("reads succeed when the plugin is down and the state is invalid", func(t *testing.T) {
		_, _, err = transformer.TransformFromStorage(ctx, encryptedData, dataCtx)
		if err != nil {
			t.Fatal(err)
		}
	})

	t.Run("reads for a different DEK fail when the plugin is down and the state is invalid", func(t *testing.T) {
		obj := &kmstypes.EncryptedObject{}
		if err := proto.Unmarshal(encryptedData, obj); err != nil {
			t.Fatal(err)
		}

		obj.EncryptedDEKSource = append(obj.EncryptedDEKSource, 1) // skip StateFunc transformer

		data, err := proto.Marshal(obj)
		if err != nil {
			t.Fatal(err)
		}

		_, _, err = transformer.TransformFromStorage(ctx, data, dataCtx)
		if errString(err) != "failed to decrypt DEK, error: Envelope service was disabled" {
			t.Fatal(err)
		}
	})
}

func TestTransformToStorageError(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		name        string
		annotations map[string][]byte
	}{
		{
			name: "invalid annotation key",
			annotations: map[string][]byte{
				"http://foo.example.com": []byte("bar"),
			},
		},
		{
			name: "annotation value size too large",
			annotations: map[string][]byte{
				"simple": []byte(strings.Repeat("a", 32*1024)),
			},
		},
		{
			name: "annotations size too large",
			annotations: map[string][]byte{
				"simple":  []byte(strings.Repeat("a", 31*1024)),
				"simple2": []byte(strings.Repeat("a", 1024)),
			},
		},
	}

	for _, tt := range testCases {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ctx := testContext(t)

			envelopeService := newTestEnvelopeService()
			envelopeService.SetAnnotations(tt.annotations)
			transformer := NewEnvelopeTransformer(envelopeService, testProviderName,
				testStateFunc(ctx, envelopeService, clock.RealClock{}, randomBool()),
				testAPIServerID,
			)
			dataCtx := value.DefaultContext(testContextText)

			_, err := transformer.TransformToStorage(ctx, []byte(testText), dataCtx)
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !strings.Contains(err.Error(), "failed to validate annotations") {
				t.Fatalf("expected error to contain 'failed to validate annotations', got %v", err)
			}
		})
	}
}

func TestEncodeDecode(t *testing.T) {
	transformer := &envelopeTransformer{}

	obj := &kmstypes.EncryptedObject{
		EncryptedData:      []byte{0x01, 0x02, 0x03},
		KeyID:              "1",
		EncryptedDEKSource: []byte{0x04, 0x05, 0x06},
	}

	data, err := transformer.doEncode(obj)
	if err != nil {
		t.Fatalf("envelopeTransformer: error while encoding data: %s", err)
	}
	got, err := transformer.doDecode(data)
	if err != nil {
		t.Fatalf("envelopeTransformer: error while decoding data: %s", err)
	}
	// reset internal field modified by marshaling obj
	obj.XXX_sizecache = 0
	if !reflect.DeepEqual(got, obj) {
		t.Fatalf("envelopeTransformer: decoded data does not match original data. Got: %v, want %v", got, obj)
	}
}

func TestValidateEncryptedObject(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		desc          string
		originalData  *kmstypes.EncryptedObject
		expectedError error
	}{
		{
			desc:          "encrypted object is nil",
			originalData:  nil,
			expectedError: fmt.Errorf("encrypted object is nil"),
		},
		{
			desc: "encrypted data is nil",
			originalData: &kmstypes.EncryptedObject{
				KeyID:              "1",
				EncryptedDEKSource: []byte{0x01, 0x02, 0x03},
			},
			expectedError: fmt.Errorf("encrypted data is empty"),
		},
		{
			desc: "encrypted data is []byte{}",
			originalData: &kmstypes.EncryptedObject{
				EncryptedDEKSource: []byte{0x01, 0x02, 0x03},
				EncryptedData:      []byte{},
			},
			expectedError: fmt.Errorf("encrypted data is empty"),
		},
		{
			desc: "invalid dek source type",
			originalData: &kmstypes.EncryptedObject{
				EncryptedDEKSource:     []byte{0x01, 0x02, 0x03},
				EncryptedData:          []byte{0},
				EncryptedDEKSourceType: 55,
			},
			expectedError: fmt.Errorf("unknown encryptedDEKSourceType: 55"),
		},
		{
			desc: "empty dek source",
			originalData: &kmstypes.EncryptedObject{
				EncryptedData:          []byte{0},
				EncryptedDEKSourceType: 1,
				KeyID:                  "1",
			},
			expectedError: fmt.Errorf("failed to validate encrypted DEK source: encrypted DEK source is empty"),
		},
		{
			desc: "empty key ID",
			originalData: &kmstypes.EncryptedObject{
				EncryptedDEKSource:     []byte{0x01, 0x02, 0x03},
				EncryptedData:          []byte{0},
				EncryptedDEKSourceType: 1,
			},
			expectedError: fmt.Errorf("failed to validate key id: keyID is empty"),
		},
		{
			desc: "invalid annotations",
			originalData: &kmstypes.EncryptedObject{
				EncryptedDEKSource:     []byte{0x01, 0x02, 0x03},
				EncryptedData:          []byte{0},
				EncryptedDEKSourceType: 1,
				KeyID:                  "1",
				Annotations:            map[string][]byte{"@": nil},
			},
			expectedError: fmt.Errorf(`failed to validate annotations: annotations: Invalid value: "@": a lowercase RFC 1123 subdomain must consist of lower case alphanumeric characters, '-' or '.', and must start and end with an alphanumeric character (e.g. 'example.com', regex used for validation is '[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*')`),
		},
	}

	for _, tt := range testCases {
		t.Run(tt.desc, func(t *testing.T) {
			err := ValidateEncryptedObject(tt.originalData)
			if err == nil {
				t.Fatalf("envelopeTransformer: expected error while decoding data, got nil")
			}

			if err.Error() != tt.expectedError.Error() {
				t.Fatalf("doDecode() error: expected %v, got %v", tt.expectedError, err)
			}
		})
	}
}

func TestValidateAnnotations(t *testing.T) {
	t.Parallel()
	successCases := []map[string][]byte{
		{"a.com": []byte("bar")},
		{"k8s.io": []byte("bar")},
		{"dev.k8s.io": []byte("bar")},
		{"dev.k8s.io.": []byte("bar")},
		{"foo.example.com": []byte("bar")},
		{"this.is.a.really.long.fqdn": []byte("bar")},
		{"bbc.co.uk": []byte("bar")},
		{"10.0.0.1": []byte("bar")}, // DNS labels can start with numbers and there is no requirement for letters.
		{"hyphens-are-good.k8s.io": []byte("bar")},
		{strings.Repeat("a", 63) + ".k8s.io": []byte("bar")},
		{strings.Repeat("a", 63) + "." + strings.Repeat("b", 63) + "." + strings.Repeat("c", 63) + "." + strings.Repeat("d", 54) + ".k8s.io": []byte("bar")},
	}
	t.Run("success", func(t *testing.T) {
		for i := range successCases {
			i := i
			t.Run(fmt.Sprintf("%d", i), func(t *testing.T) {
				t.Parallel()
				if err := validateAnnotations(successCases[i]); err != nil {
					t.Errorf("case[%d] expected success, got %#v", i, err)
				}
			})
		}
	})

	atleastTwoSegmentsErrorMsg := "should be a domain with at least two segments separated by dots"
	moreThan63CharsErrorMsg := "must be no more than 63 characters"
	moreThan253CharsErrorMsg := "must be no more than 253 characters"
	dns1123SubdomainErrorMsg := "a lowercase RFC 1123 subdomain must consist of lower case alphanumeric characters, '-' or '.', and must start and end with an alphanumeric character"

	annotationsNameErrorCases := []struct {
		annotations map[string][]byte
		expect      string
	}{
		{map[string][]byte{".": []byte("bar")}, dns1123SubdomainErrorMsg},
		{map[string][]byte{"...": []byte("bar")}, dns1123SubdomainErrorMsg},
		{map[string][]byte{".io": []byte("bar")}, dns1123SubdomainErrorMsg},
		{map[string][]byte{"com": []byte("bar")}, atleastTwoSegmentsErrorMsg},
		{map[string][]byte{".com": []byte("bar")}, dns1123SubdomainErrorMsg},
		{map[string][]byte{"Dev.k8s.io": []byte("bar")}, dns1123SubdomainErrorMsg},
		{map[string][]byte{".foo.example.com": []byte("bar")}, dns1123SubdomainErrorMsg},
		{map[string][]byte{"*.example.com": []byte("bar")}, dns1123SubdomainErrorMsg},
		{map[string][]byte{"*.bar.com": []byte("bar")}, dns1123SubdomainErrorMsg},
		{map[string][]byte{"*.foo.bar.com": []byte("bar")}, dns1123SubdomainErrorMsg},
		{map[string][]byte{"underscores_are_bad.k8s.io": []byte("bar")}, dns1123SubdomainErrorMsg},
		{map[string][]byte{"foo@bar.example.com": []byte("bar")}, dns1123SubdomainErrorMsg},
		{map[string][]byte{"http://foo.example.com": []byte("bar")}, dns1123SubdomainErrorMsg},
		{map[string][]byte{strings.Repeat("a", 64) + ".k8s.io": []byte("bar")}, moreThan63CharsErrorMsg},
		{map[string][]byte{strings.Repeat("a", 63) + "." + strings.Repeat("b", 63) + "." + strings.Repeat("c", 63) + "." + strings.Repeat("d", 55) + ".k8s.io": []byte("bar")}, moreThan253CharsErrorMsg},
	}

	t.Run("name error", func(t *testing.T) {
		for i := range annotationsNameErrorCases {
			i := i
			t.Run(fmt.Sprintf("%d", i), func(t *testing.T) {
				t.Parallel()
				err := validateAnnotations(annotationsNameErrorCases[i].annotations)
				if err == nil {
					t.Errorf("case[%d]: expected failure", i)
				} else {
					if !strings.Contains(err.Error(), annotationsNameErrorCases[i].expect) {
						t.Errorf("case[%d]: error details do not include %q: %q", i, annotationsNameErrorCases[i].expect, err)
					}
				}
			})
		}
	})

	maxSizeErrMsg := "which exceeds the max size of"
	annotationsSizeErrorCases := []struct {
		annotations map[string][]byte
		expect      string
	}{
		{map[string][]byte{"simple": []byte(strings.Repeat("a", 33*1024))}, maxSizeErrMsg},
		{map[string][]byte{"simple": []byte(strings.Repeat("a", 32*1024))}, maxSizeErrMsg},
		{map[string][]byte{"simple": []byte(strings.Repeat("a", 64*1024))}, maxSizeErrMsg},
		{map[string][]byte{"simple": []byte(strings.Repeat("a", 31*1024)), "simple2": []byte(strings.Repeat("a", 1024))}, maxSizeErrMsg},
		{map[string][]byte{strings.Repeat("a", 253): []byte(strings.Repeat("a", 32*1024))}, maxSizeErrMsg},
	}
	t.Run("size error", func(t *testing.T) {
		for i := range annotationsSizeErrorCases {
			i := i
			t.Run(fmt.Sprintf("%d", i), func(t *testing.T) {
				t.Parallel()
				err := validateAnnotations(annotationsSizeErrorCases[i].annotations)
				if err == nil {
					t.Errorf("case[%d]: expected failure", i)
				} else {
					if !strings.Contains(err.Error(), annotationsSizeErrorCases[i].expect) {
						t.Errorf("case[%d]: error details do not include %q: %q", i, annotationsSizeErrorCases[i].expect, err)
					}
				}
			})
		}
	})
}

func TestValidateKeyID(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		name              string
		keyID             string
		expectedError     string
		expectedErrorCode string
	}{
		{
			name:              "valid key ID",
			keyID:             "1234",
			expectedError:     "",
			expectedErrorCode: "ok",
		},
		{
			name:              "empty key ID",
			keyID:             "",
			expectedError:     "keyID is empty",
			expectedErrorCode: "empty",
		},
		{
			name:              "keyID size is greater than 1 kB",
			keyID:             strings.Repeat("a", 1024+1),
			expectedError:     "which exceeds the max size of",
			expectedErrorCode: "too_long",
		},
	}

	for _, tt := range testCases {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			errCode, err := ValidateKeyID(tt.keyID)
			if tt.expectedError != "" {
				if err == nil {
					t.Fatalf("expected error %q, got nil", tt.expectedError)
				}
				if !strings.Contains(err.Error(), tt.expectedError) {
					t.Fatalf("expected error %q, got %q", tt.expectedError, err)
				}
			} else {
				if err != nil {
					t.Fatalf("expected no error, got %q", err)
				}
			}
			if tt.expectedErrorCode != string(errCode) {
				t.Fatalf("expected %s errCode, got %s", tt.expectedErrorCode, string(errCode))
			}
		})
	}
}

func TestValidateEncryptedDEKSource(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		name               string
		encryptedDEKSource []byte
		expectedError      string
	}{
		{
			name:               "encrypted DEK source is nil",
			encryptedDEKSource: nil,
			expectedError:      "encrypted DEK source is empty",
		},
		{
			name:               "encrypted DEK source is empty",
			encryptedDEKSource: []byte{},
			expectedError:      "encrypted DEK source is empty",
		},
		{
			name:               "encrypted DEK source size is greater than 1 kB",
			encryptedDEKSource: bytes.Repeat([]byte("a"), 1024+1),
			expectedError:      "which exceeds the max size of",
		},
		{
			name:               "valid encrypted DEK source",
			encryptedDEKSource: []byte{0x01, 0x02, 0x03},
			expectedError:      "",
		},
	}

	for _, tt := range testCases {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := validateEncryptedDEKSource(tt.encryptedDEKSource)
			if tt.expectedError != "" {
				if err == nil {
					t.Fatalf("expected error %q, got nil", tt.expectedError)
				}
				if !strings.Contains(err.Error(), tt.expectedError) {
					t.Fatalf("expected error %q, got %q", tt.expectedError, err)
				}
			} else {
				if err != nil {
					t.Fatalf("expected no error, got %q", err)
				}
			}
		})
	}
}

func TestEnvelopeMetrics(t *testing.T) {
	envelopeService := newTestEnvelopeService()
	transformer := NewEnvelopeTransformer(envelopeService, testProviderName,
		testStateFunc(testContext(t), envelopeService, clock.RealClock{}, randomBool()),
		testAPIServerID,
	)

	dataCtx := value.DefaultContext(testContextText)

	kmsv2Transformer := value.PrefixTransformer{Prefix: []byte("k8s:enc:kms:v2:"), Transformer: transformer}

	testCases := []struct {
		desc                  string
		keyVersionFromEncrypt string
		prefix                value.Transformer
		metrics               []string
		want                  string
	}{
		{
			desc:                  "keyIDHash total",
			keyVersionFromEncrypt: testKeyVersion,
			prefix:                value.NewPrefixTransformers(nil, kmsv2Transformer),
			metrics: []string{
				"apiserver_envelope_encryption_key_id_hash_total",
			},
			want: fmt.Sprintf(`
				# HELP apiserver_envelope_encryption_key_id_hash_total [ALPHA] Number of times a keyID is used split by transformation type, provider, and apiserver identity.
				# TYPE apiserver_envelope_encryption_key_id_hash_total counter
				apiserver_envelope_encryption_key_id_hash_total{apiserver_id_hash="%s",key_id_hash="%s",provider_name="%s",transformation_type="%s"} 1
				apiserver_envelope_encryption_key_id_hash_total{apiserver_id_hash="%s",key_id_hash="%s",provider_name="%s",transformation_type="%s"} 1
				`, testAPIServerIDHash, testKeyHash, testProviderName, metrics.FromStorageLabel, testAPIServerIDHash, testKeyHash, testProviderName, metrics.ToStorageLabel),
		},
	}

	metrics.KeyIDHashTotal.Reset()
	metrics.InvalidKeyIDFromStatusTotal.Reset()

	for _, tt := range testCases {
		t.Run(tt.desc, func(t *testing.T) {
			defer metrics.KeyIDHashTotal.Reset()
			defer metrics.InvalidKeyIDFromStatusTotal.Reset()
			ctx := testContext(t)
			envelopeService.keyVersion = tt.keyVersionFromEncrypt
			transformedData, err := tt.prefix.TransformToStorage(ctx, []byte(testText), dataCtx)
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := tt.prefix.TransformFromStorage(ctx, transformedData, dataCtx); err != nil {
				t.Fatal(err)
			}

			if err := testutil.GatherAndCompare(legacyregistry.DefaultGatherer, strings.NewReader(tt.want), tt.metrics...); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// TestEnvelopeMetricsCache validates the correctness of the apiserver_envelope_encryption_dek_source_cache_size metric
// and asserts that all of the associated logic is go routine safe.
// 1. Multiple transformers are created, which should result in unique cache size for each provider
// 2. A transformer with known number of states was created to encrypt, then on restart, another transformer
// was created, which should result in expected number of cache keys for all the decryption calls for each
// state used previously for encryption.
func TestEnvelopeMetricsCache(t *testing.T) {
	envelopeService := newTestEnvelopeService()
	envelopeService.keyVersion = testKeyVersion
	state, err := testStateFunc(testContext(t), envelopeService, clock.RealClock{}, randomBool())()
	if err != nil {
		t.Fatal(err)
	}
	ctx := testContext(t)
	dataCtx := value.DefaultContext(testContextText)
	provider1 := "one"
	provider2 := "two"
	numOfStates := 10

	testCases := []struct {
		desc    string
		metrics []string
		want    string
	}{
		{
			desc: "dek source cache size",
			metrics: []string{
				"apiserver_envelope_encryption_dek_source_cache_size",
			},
			want: fmt.Sprintf(`
				# HELP apiserver_envelope_encryption_dek_source_cache_size [ALPHA] Number of records in data encryption key (DEK) source cache. On a restart, this value is an approximation of the number of decrypt RPC calls the server will make to the KMS plugin.
				# TYPE apiserver_envelope_encryption_dek_source_cache_size gauge
        		apiserver_envelope_encryption_dek_source_cache_size{provider_name="%s"} %d
        		apiserver_envelope_encryption_dek_source_cache_size{provider_name="%s"} 1
				`, provider1, numOfStates, provider2),
		},
	}
	transformer1 := NewEnvelopeTransformer(envelopeService, provider1, func() (State, error) {
		// return different states to ensure we get expected number of cache keys after restart on decryption
		return testStateFunc(ctx, envelopeService, clock.RealClock{}, randomBool())()
	}, testAPIServerID)
	transformer2 := NewEnvelopeTransformer(envelopeService, provider2, func() (State, error) { return state, nil }, testAPIServerID)
	// used for restart
	transformer3 := NewEnvelopeTransformer(envelopeService, provider1, func() (State, error) { return state, nil }, testAPIServerID)
	var transformedDatas [][]byte
	for j := 0; j < numOfStates; j++ {
		transformedData, err := transformer1.TransformToStorage(ctx, []byte(testText), dataCtx)
		if err != nil {
			t.Fatal(err)
		}
		transformedDatas = append(transformedDatas, transformedData)
	}

	for _, tt := range testCases {
		t.Run(tt.desc, func(t *testing.T) {
			metrics.DekSourceCacheSize.Reset()
			var wg sync.WaitGroup
			wg.Add(2 * numOfStates)
			for i := 0; i < numOfStates; i++ {
				i := i
				go func() {
					defer wg.Done()
					// mimic a restart, the server will make decrypt RPC calls to the KMS plugin
					// check cache metrics for the decrypt / read flow, which should repopulate the cache
					if _, _, err := transformer3.TransformFromStorage(ctx, transformedDatas[i], dataCtx); err != nil {
						panic(err)
					}
				}()
				go func() {
					defer wg.Done()
					// check cache metrics for the encrypt / write flow
					_, err := transformer2.TransformToStorage(ctx, []byte(testText), dataCtx)
					if err != nil {
						panic(err)
					}
				}()
			}
			wg.Wait()
			if err := testutil.GatherAndCompare(legacyregistry.DefaultGatherer, strings.NewReader(tt.want), tt.metrics...); err != nil {
				t.Fatal(err)
			}
		})
	}
}

var flagOnce sync.Once // support running `go test -count X`

func TestEnvelopeLogging(t *testing.T) {
	flagOnce.Do(func() {
		klog.InitFlags(nil)
	})
	flag.Set("v", "6")
	flag.Parse()

	testCases := []struct {
		desc     string
		ctx      context.Context
		wantLogs []string
	}{
		{
			desc: "no request info in context",
			ctx:  testContext(t),
			wantLogs: []string{
				`"encrypting content using envelope service" uid="UID"`,
				`"encrypting content using DEK" uid="UID" key="0123456789" group="" version="" resource="" subresource="" verb="" namespace="" name=""`,
				`"decrypting content using envelope service" uid="UID" key="0123456789" group="" version="" resource="" subresource="" verb="" namespace="" name=""`,
			},
		},
		{
			desc: "request info in context",
			ctx: genericapirequest.WithRequestInfo(testContext(t), &genericapirequest.RequestInfo{
				APIGroup:    "awesome.bears.com",
				APIVersion:  "v1",
				Resource:    "pandas",
				Subresource: "status",
				Namespace:   "kube-system",
				Name:        "panda",
				Verb:        "update",
			}),
			wantLogs: []string{
				`"encrypting content using envelope service" uid="UID"`,
				`"encrypting content using DEK" uid="UID" key="0123456789" group="awesome.bears.com" version="v1" resource="pandas" subresource="status" verb="update" namespace="kube-system" name="panda"`,
				`"decrypting content using envelope service" uid="UID" key="0123456789" group="awesome.bears.com" version="v1" resource="pandas" subresource="status" verb="update" namespace="kube-system" name="panda"`,
			},
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.desc, func(t *testing.T) {
			var buf bytes.Buffer
			klog.SetOutput(&buf)
			klog.LogToStderr(false)
			defer klog.LogToStderr(true)

			envelopeService := newTestEnvelopeService()
			fakeClock := testingclock.NewFakeClock(time.Now())
			transformer := newEnvelopeTransformerWithClock(envelopeService, testProviderName,
				testStateFunc(tc.ctx, envelopeService, clock.RealClock{}, randomBool()), testAPIServerID, 1*time.Second, fakeClock)

			dataCtx := value.DefaultContext([]byte(testContextText))
			originalText := []byte(testText)

			transformedData, err := transformer.TransformToStorage(tc.ctx, originalText, dataCtx)
			if err != nil {
				t.Fatalf("envelopeTransformer: error while transforming data to storage: %v", err)
			}

			// advance the clock to trigger cache to expire, so we make a decrypt call that will log
			fakeClock.Step(2 * time.Second)
			// force GC to run by performing a write
			transformer.(*envelopeTransformer).cache.set([]byte("some-other-unrelated-key"), &envelopeTransformer{})

			_, _, err = transformer.TransformFromStorage(tc.ctx, transformedData, dataCtx)
			if err != nil {
				t.Fatalf("could not decrypt Envelope transformer's encrypted data even once: %v", err)
			}

			klog.Flush()
			klog.SetOutput(&bytes.Buffer{}) // prevent further writes into buf
			capturedOutput := buf.String()

			// replace the uid with a constant to make the test output stable and assertable
			capturedOutput = regexp.MustCompile(`uid="[^"]+"`).ReplaceAllString(capturedOutput, `uid="UID"`)

			for _, wantLog := range tc.wantLogs {
				if !strings.Contains(capturedOutput, wantLog) {
					t.Errorf("expected log %q, got %q", wantLog, capturedOutput)
				}
			}
		})
	}
}

func TestCacheNotCorrupted(t *testing.T) {
	ctx := testContext(t)

	envelopeService := newTestEnvelopeService()
	envelopeService.SetAnnotations(map[string][]byte{
		"encrypted-dek.kms.kubernetes.io": []byte("encrypted-dek-0"),
	})

	fakeClock := testingclock.NewFakeClock(time.Now())

	state, err := testStateFunc(ctx, envelopeService, fakeClock, randomBool())()
	if err != nil {
		t.Fatal(err)
	}

	transformer := newEnvelopeTransformerWithClock(envelopeService, testProviderName,
		func() (State, error) { return state, nil }, testAPIServerID,
		1*time.Second, fakeClock)

	dataCtx := value.DefaultContext(testContextText)
	originalText := []byte(testText)

	transformedData1, err := transformer.TransformToStorage(ctx, originalText, dataCtx)
	if err != nil {
		t.Fatalf("envelopeTransformer: error while transforming data to storage: %s", err)
	}

	// this is to mimic a plugin that sets a static response for ciphertext
	// but uses the annotation field to send the actual encrypted DEK source.
	envelopeService.SetCiphertext(state.EncryptedObject.EncryptedDEKSource)
	// for this plugin, it indicates a change in the remote key ID as the returned
	// encrypted DEK source is different.
	envelopeService.SetAnnotations(map[string][]byte{
		"encrypted-dek.kms.kubernetes.io": []byte("encrypted-dek-1"),
	})

	state, err = testStateFunc(ctx, envelopeService, fakeClock, randomBool())()
	if err != nil {
		t.Fatal(err)
	}

	transformer = newEnvelopeTransformerWithClock(envelopeService, testProviderName,
		func() (State, error) { return state, nil }, testAPIServerID,
		1*time.Second, fakeClock)

	transformedData2, err := transformer.TransformToStorage(ctx, originalText, dataCtx)
	if err != nil {
		t.Fatalf("envelopeTransformer: error while transforming data to storage: %s", err)
	}

	if _, _, err := transformer.TransformFromStorage(ctx, transformedData1, dataCtx); err != nil {
		t.Fatal(err)
	}
	if _, _, err := transformer.TransformFromStorage(ctx, transformedData2, dataCtx); err != nil {
		t.Fatal(err)
	}
}

func TestGenerateCacheKey(t *testing.T) {
	encryptedDEKSource1 := []byte{1, 2, 3}
	keyID1 := "id1"
	annotations1 := map[string][]byte{"a": {4, 5}, "b": {6, 7}}
	encryptedDEKSourceType1 := kmstypes.EncryptedDEKSourceType_AES_GCM_KEY

	encryptedDEKSource2 := []byte{4, 5, 6}
	keyID2 := "id2"
	annotations2 := map[string][]byte{"x": {9, 10}, "y": {11, 12}}
	encryptedDEKSourceType2 := kmstypes.EncryptedDEKSourceType_HKDF_SHA256_XNONCE_AES_GCM_SEED

	// generate all possible combinations of the above
	testCases := []struct {
		encryptedDEKSourceType kmstypes.EncryptedDEKSourceType
		encryptedDEKSource     []byte
		keyID                  string
		annotations            map[string][]byte
	}{
		{encryptedDEKSourceType1, encryptedDEKSource1, keyID1, annotations1},
		{encryptedDEKSourceType1, encryptedDEKSource1, keyID1, annotations2},
		{encryptedDEKSourceType1, encryptedDEKSource1, keyID2, annotations1},
		{encryptedDEKSourceType1, encryptedDEKSource1, keyID2, annotations2},
		{encryptedDEKSourceType1, encryptedDEKSource2, keyID1, annotations1},
		{encryptedDEKSourceType1, encryptedDEKSource2, keyID1, annotations2},
		{encryptedDEKSourceType1, encryptedDEKSource2, keyID2, annotations1},
		{encryptedDEKSourceType1, encryptedDEKSource2, keyID2, annotations2},

		{encryptedDEKSourceType2, encryptedDEKSource1, keyID1, annotations1},
		{encryptedDEKSourceType2, encryptedDEKSource1, keyID1, annotations2},
		{encryptedDEKSourceType2, encryptedDEKSource1, keyID2, annotations1},
		{encryptedDEKSourceType2, encryptedDEKSource1, keyID2, annotations2},
		{encryptedDEKSourceType2, encryptedDEKSource2, keyID1, annotations1},
		{encryptedDEKSourceType2, encryptedDEKSource2, keyID1, annotations2},
		{encryptedDEKSourceType2, encryptedDEKSource2, keyID2, annotations1},
		{encryptedDEKSourceType2, encryptedDEKSource2, keyID2, annotations2},
	}

	for _, tc := range testCases {
		tc := tc
		for _, tc2 := range testCases {
			tc2 := tc2
			t.Run(fmt.Sprintf("%+v-%+v", tc, tc2), func(t *testing.T) {
				key1, err1 := generateCacheKey(tc.encryptedDEKSourceType, tc.encryptedDEKSource, tc.keyID, tc.annotations)
				key2, err2 := generateCacheKey(tc2.encryptedDEKSourceType, tc2.encryptedDEKSource, tc2.keyID, tc2.annotations)
				if err1 != nil || err2 != nil {
					t.Errorf("generateCacheKey() want err=nil, got err1=%q, err2=%q", errString(err1), errString(err2))
				}
				if bytes.Equal(key1, key2) != reflect.DeepEqual(tc, tc2) {
					t.Errorf("expected %v, got %v", reflect.DeepEqual(tc, tc2), bytes.Equal(key1, key2))
				}
			})
		}
	}
}

func TestGenerateTransformer(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		name            string
		envelopeService func() kmsservice.Service
		expectedErr     string
	}{
		{
			name: "encrypt call fails",
			envelopeService: func() kmsservice.Service {
				envelopeService := newTestEnvelopeService()
				envelopeService.SetDisabledStatus(true)
				return envelopeService
			},
			expectedErr: "Envelope service was disabled",
		},
		{
			name: "invalid key ID",
			envelopeService: func() kmsservice.Service {
				envelopeService := newTestEnvelopeService()
				envelopeService.keyVersion = ""
				return envelopeService
			},
			expectedErr: "failed to validate key id: keyID is empty",
		},
		{
			name: "invalid encrypted DEK",
			envelopeService: func() kmsservice.Service {
				envelopeService := newTestEnvelopeService()
				envelopeService.SetCiphertext([]byte{})
				return envelopeService
			},
			expectedErr: "failed to validate encrypted DEK source: encrypted DEK source is empty",
		},
		{
			name: "invalid annotations",
			envelopeService: func() kmsservice.Service {
				envelopeService := newTestEnvelopeService()
				envelopeService.SetAnnotations(map[string][]byte{"invalid": {}})
				return envelopeService
			},
			expectedErr: "failed to validate annotations: annotations: Invalid value: \"invalid\": should be a domain with at least two segments separated by dots",
		},
		{
			name: "success",
			envelopeService: func() kmsservice.Service {
				return newTestEnvelopeService()
			},
			expectedErr: "",
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			transformer, encObject, cacheKey, err := GenerateTransformer(testContext(t), "panda", tc.envelopeService(), randomBool())
			if tc.expectedErr == "" {
				if err != nil {
					t.Errorf("expected no error, got %q", errString(err))
				}
				if transformer == nil {
					t.Error("expected transformer, got nil")
				}
				if encObject == nil {
					t.Error("expected encrypt response, got nil")
				}
				if cacheKey == nil {
					t.Error("expected cache key, got nil")
				}
			} else {
				if err == nil || !strings.Contains(err.Error(), tc.expectedErr) {
					t.Errorf("expected error %q, got %q", tc.expectedErr, errString(err))
				}
			}
		})
	}
}

func TestEnvelopeTracing_TransformToStorage(t *testing.T) {
	testCases := []struct {
		desc     string
		expected []string
	}{
		{
			desc: "encrypt",
			expected: []string{
				"About to encrypt data using DEK",
				"Data encryption succeeded",
				"About to encode encrypted object",
				"Encoded encrypted object",
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.desc, func(t *testing.T) {
			fakeRecorder := tracetest.NewSpanRecorder()
			otelTracer := trace.NewTracerProvider(trace.WithSpanProcessor(fakeRecorder)).Tracer("test")

			ctx := testContext(t)
			ctx, span := otelTracer.Start(ctx, "parent")
			defer span.End()

			envelopeService := newTestEnvelopeService()
			fakeClock := testingclock.NewFakeClock(time.Now())
			state, err := testStateFunc(ctx, envelopeService, clock.RealClock{}, randomBool())()
			if err != nil {
				t.Fatal(err)
			}

			transformer := newEnvelopeTransformerWithClock(envelopeService, testProviderName,
				func() (State, error) { return state, nil }, testAPIServerID, 1*time.Second, fakeClock)

			dataCtx := value.DefaultContext([]byte(testContextText))
			originalText := []byte(testText)

			if _, err := transformer.TransformToStorage(ctx, originalText, dataCtx); err != nil {
				t.Fatalf("envelopeTransformer: error while transforming data to storage: %v", err)
			}

			output := fakeRecorder.Ended()
			if len(output) != 1 {
				t.Fatalf("expected 1 span, got %d", len(output))
			}
			out := output[0]
			validateTraceSpan(t, out, "TransformToStorage with envelopeTransformer", testProviderName, testAPIServerID, tc.expected)
		})
	}
}

func TestEnvelopeTracing_TransformFromStorage(t *testing.T) {
	testCases := []struct {
		desc                     string
		cacheTTL                 time.Duration
		simulateKMSPluginFailure bool
		expected                 []string
	}{
		{
			desc:     "decrypt",
			cacheTTL: 5 * time.Second,
			expected: []string{
				"About to decode encrypted object",
				"Decoded encrypted object",
				"About to decrypt data using DEK",
				"Data decryption succeeded",
			},
		},
		{
			desc:     "decrypt with cache miss",
			cacheTTL: 1 * time.Second,
			expected: []string{
				"About to decode encrypted object",
				"Decoded encrypted object",
				"About to decrypt DEK using remote service",
				"DEK decryption succeeded",
				"About to decrypt data using DEK",
				"Data decryption succeeded",
			},
		},
		{
			desc:                     "decrypt with cache miss, simulate KMS plugin failure",
			cacheTTL:                 1 * time.Second,
			simulateKMSPluginFailure: true,
			expected: []string{
				"About to decode encrypted object",
				"Decoded encrypted object",
				"About to decrypt DEK using remote service",
				"DEK decryption failed",
				"exception",
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.desc, func(t *testing.T) {
			fakeRecorder := tracetest.NewSpanRecorder()
			otelTracer := trace.NewTracerProvider(trace.WithSpanProcessor(fakeRecorder)).Tracer("test")

			ctx := testContext(t)

			envelopeService := newTestEnvelopeService()
			fakeClock := testingclock.NewFakeClock(time.Now())
			state, err := testStateFunc(ctx, envelopeService, clock.RealClock{}, randomBool())()
			if err != nil {
				t.Fatal(err)
			}

			transformer := newEnvelopeTransformerWithClock(envelopeService, testProviderName,
				func() (State, error) { return state, nil }, testAPIServerID, tc.cacheTTL, fakeClock)

			dataCtx := value.DefaultContext([]byte(testContextText))
			originalText := []byte(testText)

			transformedData, _ := transformer.TransformToStorage(ctx, originalText, dataCtx)

			// advance the clock to allow cache entries to expire depending on TTL
			fakeClock.Step(2 * time.Second)
			// force GC to run by performing a write
			transformer.(*envelopeTransformer).cache.set([]byte("some-other-unrelated-key"), &envelopeTransformer{})

			envelopeService.SetDisabledStatus(tc.simulateKMSPluginFailure)

			// start recording only for the decrypt call
			ctx, span := otelTracer.Start(ctx, "parent")
			defer span.End()

			_, _, _ = transformer.TransformFromStorage(ctx, transformedData, dataCtx)

			output := fakeRecorder.Ended()
			validateTraceSpan(t, output[0], "TransformFromStorage with envelopeTransformer", testProviderName, testAPIServerID, tc.expected)
		})
	}
}

func validateTraceSpan(t *testing.T, span trace.ReadOnlySpan, spanName, providerName, apiserverID string, expected []string) {
	t.Helper()

	if span.Name() != spanName {
		t.Fatalf("expected span name %q, got %q", spanName, span.Name())
	}
	attrs := span.Attributes()
	if len(attrs) != 1 {
		t.Fatalf("expected 1 attributes, got %d", len(attrs))
	}
	if attrs[0].Key != "transformer.provider.name" && attrs[0].Value.AsString() != providerName {
		t.Errorf("expected providerName %q, got %q", providerName, attrs[0].Value.AsString())
	}
	if len(span.Events()) != len(expected) {
		t.Fatalf("expected %d events, got %d", len(expected), len(span.Events()))
	}
	for i, event := range span.Events() {
		if event.Name != expected[i] {
			t.Errorf("expected event %q, got %q", expected[i], event.Name)
		}
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}

	return err.Error()
}

func randomBool() bool { return utilrand.Int()%2 == 1 }
