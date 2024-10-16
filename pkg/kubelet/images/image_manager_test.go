/*
Copyright 2015 The Kubernetes Authors.

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

package images

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/stretchr/testify/assert"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/client-go/util/flowcontrol"
	"k8s.io/component-base/featuregate"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	crierrors "k8s.io/cri-api/pkg/errors"
	"k8s.io/kubernetes/pkg/controller/testutil"
	"k8s.io/kubernetes/pkg/credentialprovider"
	"k8s.io/kubernetes/pkg/features"
	kubeletconfiginternal "k8s.io/kubernetes/pkg/kubelet/apis/config"
	. "k8s.io/kubernetes/pkg/kubelet/container"
	ctest "k8s.io/kubernetes/pkg/kubelet/container/testing"
	"k8s.io/kubernetes/test/utils/ktesting"
	testingclock "k8s.io/utils/clock/testing"
	"k8s.io/utils/ptr"
)

type pullerExpects struct {
	calls                           []string
	err                             error
	shouldRecordStartedPullingTime  bool
	shouldRecordFinishedPullingTime bool
	events                          []v1.Event
	msg                             string
}

type pullerTestCase struct {
	testName           string
	containerImage     string
	policy             v1.PullPolicy
	pullSecrets        []v1.Secret
	allowedCredentials map[string][]kubeletconfiginternal.ImagePullSecret // image -> allowedCredentials; nil means allow all
	inspectErr         error
	pullerErr          error
	qps                float32
	burst              int
	expected           []pullerExpects
	enableFeatures     []featuregate.Feature
}

func pullerTestCases() []pullerTestCase {
	return append(
		noFGPullerTestCases(),
		ensureSecretImagesTestCases()...,
	)
}

// noFGPullerTestCases returns all test cases that test the default behavior without any
// feature gate required
func noFGPullerTestCases() []pullerTestCase {
	return []pullerTestCase{
		{ // pull missing image
			testName:       "image missing, pull",
			containerImage: "missing_image",
			policy:         v1.PullIfNotPresent,
			inspectErr:     nil,
			pullerErr:      nil,
			qps:            0.0,
			burst:          0,
			expected: []pullerExpects{
				{[]string{"GetImageRef", "PullImage", "GetImageSize"}, nil, true, true,
					[]v1.Event{
						{Reason: "Pulling"},
						{Reason: "Pulled"},
					}, ""},
			}},

		{ // image present, don't pull
			testName:       "image present, allow all, don't pull ",
			containerImage: "present_image",
			policy:         v1.PullIfNotPresent,
			inspectErr:     nil,
			pullerErr:      nil,
			qps:            0.0,
			burst:          0,
			expected: []pullerExpects{
				{[]string{"GetImageRef"}, nil, false, false,
					[]v1.Event{
						{Reason: "Pulled"},
					}, ""},
				{[]string{"GetImageRef"}, nil, false, false,
					[]v1.Event{
						{Reason: "Pulled"},
					}, ""},
				{[]string{"GetImageRef"}, nil, false, false,
					[]v1.Event{
						{Reason: "Pulled"},
					}, ""},
			}},
		// image present, pull it
		{containerImage: "present_image",
			testName:   "image present, pull ",
			policy:     v1.PullAlways,
			inspectErr: nil,
			pullerErr:  nil,
			qps:        0.0,
			burst:      0,
			expected: []pullerExpects{
				{[]string{"PullImage", "GetImageSize"}, nil, true, true,
					[]v1.Event{
						{Reason: "Pulling"},
						{Reason: "Pulled"},
					}, ""},
				{[]string{"PullImage", "GetImageSize"}, nil, true, true,
					[]v1.Event{
						{Reason: "Pulling"},
						{Reason: "Pulled"},
					}, ""},
				{[]string{"PullImage", "GetImageSize"}, nil, true, true,
					[]v1.Event{
						{Reason: "Pulling"},
						{Reason: "Pulled"},
					}, ""},
			}},
		// missing image, error PullNever
		{containerImage: "missing_image",
			testName:   "image missing, never pull",
			policy:     v1.PullNever,
			inspectErr: nil,
			pullerErr:  nil,
			qps:        0.0,
			burst:      0,
			expected: []pullerExpects{
				{[]string{"GetImageRef"}, ErrImageNeverPull, false, false,
					[]v1.Event{
						{Reason: "ErrImageNeverPull"},
					}, ""},
				{[]string{"GetImageRef"}, ErrImageNeverPull, false, false,
					[]v1.Event{
						{Reason: "ErrImageNeverPull"},
					}, ""},
				{[]string{"GetImageRef"}, ErrImageNeverPull, false, false,
					[]v1.Event{
						{Reason: "ErrImageNeverPull"},
					}, ""},
			}},
		// missing image, unable to inspect
		{containerImage: "missing_image",
			testName:   "image missing, pull if not present",
			policy:     v1.PullIfNotPresent,
			inspectErr: errors.New("unknown inspectError"),
			pullerErr:  nil,
			qps:        0.0,
			burst:      0,
			expected: []pullerExpects{
				{[]string{"GetImageRef"}, ErrImageInspect, false, false,
					[]v1.Event{
						{Reason: "InspectFailed"},
					}, ""},
				{[]string{"GetImageRef"}, ErrImageInspect, false, false,
					[]v1.Event{
						{Reason: "InspectFailed"},
					}, ""},
				{[]string{"GetImageRef"}, ErrImageInspect, false, false,
					[]v1.Event{
						{Reason: "InspectFailed"},
					}, ""},
			}},
		// missing image, unable to fetch
		{containerImage: "typo_image",
			testName:   "image missing, unable to fetch",
			policy:     v1.PullIfNotPresent,
			inspectErr: nil,
			pullerErr:  errors.New("404"),
			qps:        0.0,
			burst:      0,
			expected: []pullerExpects{
				{[]string{"GetImageRef", "PullImage"}, ErrImagePull, true, false,
					[]v1.Event{
						{Reason: "Pulling"},
						{Reason: "Failed"},
					}, ""},
				{[]string{"GetImageRef", "PullImage"}, ErrImagePull, true, false,
					[]v1.Event{
						{Reason: "Pulling"},
						{Reason: "Failed"},
					}, ""},
				{[]string{"GetImageRef"}, ErrImagePullBackOff, false, false,
					[]v1.Event{
						{Reason: "BackOff"},
					}, ""},
				{[]string{"GetImageRef", "PullImage"}, ErrImagePull, true, false,
					[]v1.Event{
						{Reason: "Pulling"},
						{Reason: "Failed"},
					}, ""},
				{[]string{"GetImageRef"}, ErrImagePullBackOff, false, false,
					[]v1.Event{
						{Reason: "BackOff"},
					}, ""},
				{[]string{"GetImageRef"}, ErrImagePullBackOff, false, false,
					[]v1.Event{
						{Reason: "BackOff"},
					}, ""},
			}},
		// image present, non-zero qps, try to pull
		{containerImage: "present_image",
			testName:   "image present and qps>0, pull",
			policy:     v1.PullAlways,
			inspectErr: nil,
			pullerErr:  nil,
			qps:        400.0,
			burst:      600,
			expected: []pullerExpects{
				{[]string{"PullImage", "GetImageSize"}, nil, true, true,
					[]v1.Event{
						{Reason: "Pulling"},
						{Reason: "Pulled"},
					}, ""},
				{[]string{"PullImage", "GetImageSize"}, nil, true, true,
					[]v1.Event{
						{Reason: "Pulling"},
						{Reason: "Pulled"},
					}, ""},
				{[]string{"PullImage", "GetImageSize"}, nil, true, true,
					[]v1.Event{
						{Reason: "Pulling"},
						{Reason: "Pulled"},
					}, ""},
			}},
		// image present, non-zero qps, try to pull when qps exceeded
		{containerImage: "present_image",
			testName:   "image present and excessive qps rate, pull",
			policy:     v1.PullAlways,
			inspectErr: nil,
			pullerErr:  nil,
			qps:        2000.0,
			burst:      0,
			expected: []pullerExpects{
				{[]string(nil), ErrImagePull, true, false,
					[]v1.Event{
						{Reason: "Pulling"},
						{Reason: "Failed"},
					}, ""},
				{[]string(nil), ErrImagePull, true, false,
					[]v1.Event{
						{Reason: "Pulling"},
						{Reason: "Failed"},
					}, ""},
				{[]string(nil), ErrImagePullBackOff, false, false,
					[]v1.Event{
						{Reason: "BackOff"},
					}, ""},
			}},
		// error case if image name fails validation due to invalid reference format
		{containerImage: "FAILED_IMAGE",
			testName:   "invalid image name, no pull",
			policy:     v1.PullAlways,
			inspectErr: nil,
			pullerErr:  nil,
			qps:        0.0,
			burst:      0,
			expected: []pullerExpects{
				{[]string(nil), ErrInvalidImageName, false, false,
					[]v1.Event{
						{Reason: "InspectFailed"},
					}, ""},
			}},
		// error case if image name contains http
		{containerImage: "http://url",
			testName:   "invalid image name with http, no pull",
			policy:     v1.PullAlways,
			inspectErr: nil,
			pullerErr:  nil,
			qps:        0.0,
			burst:      0,
			expected: []pullerExpects{
				{[]string(nil), ErrInvalidImageName, false, false,
					[]v1.Event{
						{Reason: "InspectFailed"},
					}, ""},
			}},
		// error case if image name contains sha256
		{containerImage: "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad",
			testName:   "invalid image name with sha256, no pull",
			policy:     v1.PullAlways,
			inspectErr: nil,
			pullerErr:  nil,
			qps:        0.0,
			burst:      0,
			expected: []pullerExpects{
				{[]string(nil), ErrInvalidImageName, false, false,
					[]v1.Event{
						{Reason: "InspectFailed"},
					}, ""},
			}},
		{containerImage: "typo_image",
			testName:   "image missing, SignatureValidationFailed",
			policy:     v1.PullIfNotPresent,
			inspectErr: nil,
			pullerErr:  crierrors.ErrSignatureValidationFailed,
			qps:        0.0,
			burst:      0,
			expected: []pullerExpects{
				{[]string{"GetImageRef", "PullImage"}, crierrors.ErrSignatureValidationFailed, true, false,
					[]v1.Event{
						{Reason: "Pulling"},
						{Reason: "Failed"},
					}, "image pull failed for typo_image because the signature validation failed"},
				{[]string{"GetImageRef", "PullImage"}, crierrors.ErrSignatureValidationFailed, true, false,
					[]v1.Event{
						{Reason: "Pulling"},
						{Reason: "Failed"},
					}, "image pull failed for typo_image because the signature validation failed"},
				{[]string{"GetImageRef"}, ErrImagePullBackOff, false, false,
					[]v1.Event{
						{Reason: "BackOff"},
					}, "Back-off pulling image \"typo_image\": SignatureValidationFailed: image pull failed for typo_image because the signature validation failed"},
				{[]string{"GetImageRef", "PullImage"}, crierrors.ErrSignatureValidationFailed, true, false,
					[]v1.Event{
						{Reason: "Pulling"},
						{Reason: "Failed"},
					}, "image pull failed for typo_image because the signature validation failed"},
				{[]string{"GetImageRef"}, ErrImagePullBackOff, false, false,
					[]v1.Event{
						{Reason: "BackOff"},
					}, "Back-off pulling image \"typo_image\": SignatureValidationFailed: image pull failed for typo_image because the signature validation failed"},
			}},
	}
}

// ensureSecretImages returns test cases specific for the KubeletEnsureSecretPulledImages
// featuregate plus a copy of all non-featuregated tests, but it requests the featuregate
// to be enabled there, too
func ensureSecretImagesTestCases() []pullerTestCase {
	testCases := []pullerTestCase{
		{
			testName:       "[KubeletEnsureSecretPulledImages] image present, unknown to image pull manager, pull",
			containerImage: "present_image",
			policy:         v1.PullIfNotPresent,
			allowedCredentials: map[string][]kubeletconfiginternal.ImagePullSecret{
				"another_image": {{Namespace: "testns", Name: "testname", UID: "testuid"}},
			},
			pullSecrets:    []v1.Secret{makeDockercfgSecretForRepo(metav1.ObjectMeta{Namespace: "testns", Name: "testname", UID: "testuid"}, "docker.io/library/present_image")},
			inspectErr:     nil,
			pullerErr:      nil,
			qps:            0.0,
			burst:          0,
			enableFeatures: []featuregate.Feature{features.KubeletEnsureSecretPulledImages},
			expected: []pullerExpects{
				{[]string{"GetImageRef", "PullImage", "GetImageSize"}, nil, true, true,
					[]v1.Event{
						{Reason: "Pulling"},
						{Reason: "Pulled"},
					}, ""},
				{[]string{"GetImageRef", "PullImage", "GetImageSize"}, nil, true, true,
					[]v1.Event{
						{Reason: "Pulling"},
						{Reason: "Pulled"},
					}, ""},
				{[]string{"GetImageRef", "PullImage", "GetImageSize"}, nil, true, true,
					[]v1.Event{
						{Reason: "Pulling"},
						{Reason: "Pulled"},
					}, ""},
			}},
		{
			testName:       "[KubeletEnsureSecretPulledImages] image present, unknown secret to image pull manager, pull",
			containerImage: "present_image",
			policy:         v1.PullIfNotPresent,
			allowedCredentials: map[string][]kubeletconfiginternal.ImagePullSecret{
				"present_image": {{Namespace: "testns", Name: "testname", UID: "testuid"}},
			},
			pullSecrets:    []v1.Secret{makeDockercfgSecretForRepo(metav1.ObjectMeta{Namespace: "testns", Name: "testname", UID: "someothertestuid"}, "docker.io/library/present_image")},
			inspectErr:     nil,
			pullerErr:      nil,
			qps:            0.0,
			burst:          0,
			enableFeatures: []featuregate.Feature{features.KubeletEnsureSecretPulledImages},
			expected: []pullerExpects{
				{[]string{"GetImageRef", "PullImage", "GetImageSize"}, nil, true, true,
					[]v1.Event{
						{Reason: "Pulling"},
						{Reason: "Pulled"},
					}, ""},
				{[]string{"GetImageRef", "PullImage", "GetImageSize"}, nil, true, true,
					[]v1.Event{
						{Reason: "Pulling"},
						{Reason: "Pulled"},
					}, ""},
				{[]string{"GetImageRef", "PullImage", "GetImageSize"}, nil, true, true,
					[]v1.Event{
						{Reason: "Pulling"},
						{Reason: "Pulled"},
					}, ""},
			},
		},
		{
			testName:       "[KubeletEnsureSecretPulledImages] image present, unknown secret to image pull manager, never pull policy -> fail",
			containerImage: "present_image",
			policy:         v1.PullNever,
			allowedCredentials: map[string][]kubeletconfiginternal.ImagePullSecret{
				"present_image": {{Namespace: "testns", Name: "testname", UID: "testuid"}},
			},
			pullSecrets:    []v1.Secret{makeDockercfgSecretForRepo(metav1.ObjectMeta{Namespace: "testns", Name: "testname", UID: "someothertestuid"}, "docker.io/library/present_image")},
			inspectErr:     nil,
			pullerErr:      nil,
			qps:            0.0,
			burst:          0,
			enableFeatures: []featuregate.Feature{features.KubeletEnsureSecretPulledImages},
			expected: []pullerExpects{
				{[]string{"GetImageRef"}, ErrImageNeverPull, false, false,
					[]v1.Event{
						{Reason: "ErrImageNeverPull"},
					}, ""},
				{[]string{"GetImageRef"}, ErrImageNeverPull, false, false,
					[]v1.Event{
						{Reason: "ErrImageNeverPull"},
					}, ""},
				{[]string{"GetImageRef"}, ErrImageNeverPull, false, false,
					[]v1.Event{
						{Reason: "ErrImageNeverPull"},
					}, ""},
			},
		},
		{
			testName:       "[KubeletEnsureSecretPulledImages] image present, a secret matches one of known to the image pull manager, don't pull",
			containerImage: "present_image",
			policy:         v1.PullIfNotPresent,
			allowedCredentials: map[string][]kubeletconfiginternal.ImagePullSecret{
				"present_image": {{Namespace: "testns", Name: "testname", UID: "testuid"}},
			},
			pullSecrets: []v1.Secret{
				makeDockercfgSecretForRepo(metav1.ObjectMeta{Namespace: "testns", Name: "testname", UID: "someothertestuid"}, "docker.io/library/present_image"),
				makeDockercfgSecretForRepo(metav1.ObjectMeta{Namespace: "testns", Name: "testname", UID: "testuid"}, "docker.io/library/present_image"),
			},
			inspectErr:     nil,
			pullerErr:      nil,
			qps:            0.0,
			burst:          0,
			enableFeatures: []featuregate.Feature{features.KubeletEnsureSecretPulledImages},
			expected: []pullerExpects{
				{[]string{"GetImageRef"}, nil, false, false,
					[]v1.Event{
						{Reason: "Pulled"},
					}, ""},
				{[]string{"GetImageRef"}, nil, false, false,
					[]v1.Event{
						{Reason: "Pulled"},
					}, ""},
				{[]string{"GetImageRef"}, nil, false, false,
					[]v1.Event{
						{Reason: "Pulled"},
					}, ""},
			},
		},
	}

	for _, tc := range noFGPullerTestCases() {
		tc.testName = "[KubeletEnsureSecretPulledImages] " + tc.testName
		tc.enableFeatures = append(tc.enableFeatures, features.KubeletEnsureSecretPulledImages)
		testCases = append(testCases, tc)
	}

	return testCases
}

type mockPodPullingTimeRecorder struct {
	sync.Mutex
	startedPullingRecorded  bool
	finishedPullingRecorded bool
}

func (m *mockPodPullingTimeRecorder) RecordImageStartedPulling(podUID types.UID) {
	m.Lock()
	defer m.Unlock()
	m.startedPullingRecorded = true
}

func (m *mockPodPullingTimeRecorder) RecordImageFinishedPulling(podUID types.UID) {
	m.Lock()
	defer m.Unlock()
	m.finishedPullingRecorded = true
}

func (m *mockPodPullingTimeRecorder) reset() {
	m.Lock()
	defer m.Unlock()
	m.startedPullingRecorded = false
	m.finishedPullingRecorded = false
}

type mockImagePullManager struct {
	NoopImagePullManager

	imageAllowlist map[string]sets.Set[kubeletconfiginternal.ImagePullSecret]
	allowAll       bool
}

func (m *mockImagePullManager) MustAttemptImagePull(image, _ string, podSecrets []kubeletconfiginternal.ImagePullSecret) bool {
	if m.allowAll == true {
		return false
	}

	cachedSecrets, ok := m.imageAllowlist[image]
	if !ok {
		return true
	}

	// cut off all the hashes and only determine the match based on the secret coords to simplify testing
	for _, s := range podSecrets {
		if cachedSecrets.Has(kubeletconfiginternal.ImagePullSecret{Namespace: s.Namespace, Name: s.Name, UID: s.UID}) {
			return false
		}
	}

	return true
}

func pullerTestEnv(
	t *testing.T,
	c pullerTestCase,
	serialized bool,
	maxParallelImagePulls *int32,
) (
	puller ImageManager,
	fakeClock *testingclock.FakeClock,
	fakeRuntime *ctest.FakeRuntime,
	container *v1.Container,
	fakePodPullingTimeRecorder *mockPodPullingTimeRecorder,
	fakeRecorder *testutil.FakeRecorder,
) {
	container = &v1.Container{
		Name:            "container_name",
		Image:           c.containerImage,
		ImagePullPolicy: c.policy,
	}

	backOff := flowcontrol.NewBackOff(time.Second, time.Minute)
	fakeClock = testingclock.NewFakeClock(time.Now())
	backOff.Clock = fakeClock

	fakeRuntime = &ctest.FakeRuntime{T: t}
	fakeRecorder = testutil.NewFakeRecorder()

	fakeRuntime.ImageList = []Image{{ID: "present_image:latest"}}
	fakeRuntime.Err = c.pullerErr
	fakeRuntime.InspectErr = c.inspectErr

	fakePodPullingTimeRecorder = &mockPodPullingTimeRecorder{}

	pullManager := &mockImagePullManager{allowAll: true}
	if c.allowedCredentials != nil {
		pullManager.allowAll = false
		pullManager.imageAllowlist = make(map[string]sets.Set[kubeletconfiginternal.ImagePullSecret])
		for image, secrets := range c.allowedCredentials {
			pullManager.imageAllowlist[image] = sets.New(secrets...)
		}
	}

	for _, fg := range c.enableFeatures {
		featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, fg, true)
	}
	puller = NewImageManager(fakeRecorder, &credentialprovider.BasicDockerKeyring{}, fakeRuntime, pullManager, backOff, serialized, maxParallelImagePulls, c.qps, c.burst, fakePodPullingTimeRecorder)
	return
}

func TestParallelPuller(t *testing.T) {
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "test_pod",
			Namespace:       "test-ns",
			UID:             "bar",
			ResourceVersion: "42",
		}}

	cases := pullerTestCases()

	useSerializedEnv := false
	for _, c := range cases {
		t.Run(c.testName, func(t *testing.T) {
			ctx := context.Background()
			puller, fakeClock, fakeRuntime, container, fakePodPullingTimeRecorder, _ := pullerTestEnv(t, c, useSerializedEnv, nil)

			for _, expected := range c.expected {
				fakeRuntime.CalledFunctions = nil
				fakeClock.Step(time.Second)

				_, msg, err := puller.EnsureImageExists(ctx, nil, pod, container.Image, c.pullSecrets, nil, "", container.ImagePullPolicy)
				fakeRuntime.AssertCalls(expected.calls)
				assert.Equal(t, expected.err, err)
				assert.Equal(t, expected.shouldRecordStartedPullingTime, fakePodPullingTimeRecorder.startedPullingRecorded)
				assert.Equal(t, expected.shouldRecordFinishedPullingTime, fakePodPullingTimeRecorder.finishedPullingRecorded)
				assert.Contains(t, msg, expected.msg)
				fakePodPullingTimeRecorder.reset()
			}
		})
	}
}

func TestSerializedPuller(t *testing.T) {
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "test_pod",
			Namespace:       "test-ns",
			UID:             "bar",
			ResourceVersion: "42",
		}}

	cases := pullerTestCases()

	useSerializedEnv := true
	for _, c := range cases {
		t.Run(c.testName, func(t *testing.T) {
			ctx := context.Background()
			puller, fakeClock, fakeRuntime, container, fakePodPullingTimeRecorder, _ := pullerTestEnv(t, c, useSerializedEnv, nil)

			for _, expected := range c.expected {
				fakeRuntime.CalledFunctions = nil
				fakeClock.Step(time.Second)

				_, msg, err := puller.EnsureImageExists(ctx, nil, pod, container.Image, c.pullSecrets, nil, "", container.ImagePullPolicy)
				fakeRuntime.AssertCalls(expected.calls)
				assert.Equal(t, expected.err, err)
				assert.Equal(t, expected.shouldRecordStartedPullingTime, fakePodPullingTimeRecorder.startedPullingRecorded)
				assert.Equal(t, expected.shouldRecordFinishedPullingTime, fakePodPullingTimeRecorder.finishedPullingRecorded)
				assert.Contains(t, msg, expected.msg)
				fakePodPullingTimeRecorder.reset()
			}
		})
	}
}

func TestApplyDefaultImageTag(t *testing.T) {
	for _, testCase := range []struct {
		testName string
		Input    string
		Output   string
	}{
		{testName: "root", Input: "root", Output: "root:latest"},
		{testName: "root:tag", Input: "root:tag", Output: "root:tag"},
		{testName: "root@sha", Input: "root@sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", Output: "root@sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"},
		{testName: "root:latest@sha", Input: "root:latest@sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", Output: "root:latest@sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"},
		{testName: "root:latest", Input: "root:latest", Output: "root:latest"},
	} {
		t.Run(testCase.testName, func(t *testing.T) {
			image, err := applyDefaultImageTag(testCase.Input)
			if err != nil {
				t.Errorf("applyDefaultImageTag(%s) failed: %v", testCase.Input, err)
			} else if image != testCase.Output {
				t.Errorf("Expected image reference: %q, got %q", testCase.Output, image)
			}
		})
	}
}

func TestPullAndListImageWithPodAnnotations(t *testing.T) {
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "test_pod",
			Namespace:       "test-ns",
			UID:             "bar",
			ResourceVersion: "42",
			Annotations: map[string]string{
				"kubernetes.io/runtimehandler": "handler_name",
			},
		}}
	c := pullerTestCase{ // pull missing image
		testName:       "test pull and list image with pod annotations",
		containerImage: "missing_image",
		policy:         v1.PullIfNotPresent,
		inspectErr:     nil,
		pullerErr:      nil,
		expected: []pullerExpects{
			{[]string{"GetImageRef", "PullImage", "GetImageSize"}, nil, true, true, nil, ""},
		}}

	useSerializedEnv := true
	t.Run(c.testName, func(t *testing.T) {
		ctx := context.Background()
		puller, fakeClock, fakeRuntime, container, fakePodPullingTimeRecorder, _ := pullerTestEnv(t, c, useSerializedEnv, nil)
		fakeRuntime.CalledFunctions = nil
		fakeRuntime.ImageList = []Image{}
		fakeClock.Step(time.Second)

		_, _, err := puller.EnsureImageExists(ctx, nil, pod, container.Image, c.pullSecrets, nil, "", container.ImagePullPolicy)
		fakeRuntime.AssertCalls(c.expected[0].calls)
		assert.Equal(t, c.expected[0].err, err, "tick=%d", 0)
		assert.Equal(t, c.expected[0].shouldRecordStartedPullingTime, fakePodPullingTimeRecorder.startedPullingRecorded)
		assert.Equal(t, c.expected[0].shouldRecordFinishedPullingTime, fakePodPullingTimeRecorder.finishedPullingRecorded)

		images, _ := fakeRuntime.ListImages(ctx)
		assert.Len(t, images, 1, "ListImages() count")

		image := images[0]
		assert.Equal(t, "missing_image:latest", image.ID, "Image ID")
		assert.Equal(t, "", image.Spec.RuntimeHandler, "image.Spec.RuntimeHandler not empty", "ImageID", image.ID)

		expectedAnnotations := []Annotation{
			{
				Name:  "kubernetes.io/runtimehandler",
				Value: "handler_name",
			}}
		assert.Equal(t, expectedAnnotations, image.Spec.Annotations, "image spec annotations")
	})
}

func TestPullAndListImageWithRuntimeHandlerInImageCriAPIFeatureGate(t *testing.T) {
	runtimeHandler := "handler_name"
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "test_pod",
			Namespace:       "test-ns",
			UID:             "bar",
			ResourceVersion: "42",
			Annotations: map[string]string{
				"kubernetes.io/runtimehandler": runtimeHandler,
			},
		},
		Spec: v1.PodSpec{
			RuntimeClassName: &runtimeHandler,
		},
	}
	c := pullerTestCase{ // pull missing image
		testName:       "test pull and list image with pod annotations",
		containerImage: "missing_image",
		policy:         v1.PullIfNotPresent,
		inspectErr:     nil,
		pullerErr:      nil,
		expected: []pullerExpects{
			{[]string{"GetImageRef", "PullImage", "GetImageSize"}, nil, true, true, nil, ""},
		}}

	useSerializedEnv := true
	t.Run(c.testName, func(t *testing.T) {
		featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.RuntimeClassInImageCriAPI, true)
		ctx := context.Background()
		puller, fakeClock, fakeRuntime, container, fakePodPullingTimeRecorder, _ := pullerTestEnv(t, c, useSerializedEnv, nil)
		fakeRuntime.CalledFunctions = nil
		fakeRuntime.ImageList = []Image{}
		fakeClock.Step(time.Second)

		_, _, err := puller.EnsureImageExists(ctx, nil, pod, container.Image, c.pullSecrets, nil, runtimeHandler, container.ImagePullPolicy)
		fakeRuntime.AssertCalls(c.expected[0].calls)
		assert.Equal(t, c.expected[0].err, err, "tick=%d", 0)
		assert.Equal(t, c.expected[0].shouldRecordStartedPullingTime, fakePodPullingTimeRecorder.startedPullingRecorded)
		assert.Equal(t, c.expected[0].shouldRecordFinishedPullingTime, fakePodPullingTimeRecorder.finishedPullingRecorded)

		images, _ := fakeRuntime.ListImages(ctx)
		assert.Len(t, images, 1, "ListImages() count")

		image := images[0]
		assert.Equal(t, "missing_image:latest", image.ID, "Image ID")

		// when RuntimeClassInImageCriAPI feature gate is enabled, check runtime
		// handler information for every image in the ListImages() response
		assert.Equal(t, runtimeHandler, image.Spec.RuntimeHandler, "runtime handler returned not as expected", "Image ID", image)

		expectedAnnotations := []Annotation{
			{
				Name:  "kubernetes.io/runtimehandler",
				Value: "handler_name",
			}}
		assert.Equal(t, expectedAnnotations, image.Spec.Annotations, "image spec annotations")
	})
}

func TestMaxParallelImagePullsLimit(t *testing.T) {
	ctx := context.Background()
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "test_pod",
			Namespace:       "test-ns",
			UID:             "bar",
			ResourceVersion: "42",
		}}

	testCase := &pullerTestCase{
		containerImage: "present_image",
		testName:       "image present, pull ",
		policy:         v1.PullAlways,
		inspectErr:     nil,
		pullerErr:      nil,
		qps:            0.0,
		burst:          0,
	}

	useSerializedEnv := false
	maxParallelImagePulls := 5
	var wg sync.WaitGroup

	puller, fakeClock, fakeRuntime, container, _, _ := pullerTestEnv(t, *testCase, useSerializedEnv, ptr.To(int32(maxParallelImagePulls)))
	fakeRuntime.BlockImagePulls = true
	fakeRuntime.CalledFunctions = nil
	fakeRuntime.T = t
	fakeClock.Step(time.Second)

	// First 5 EnsureImageExists should result in runtime calls
	for i := 0; i < maxParallelImagePulls; i++ {
		wg.Add(1)
		go func() {
			_, _, err := puller.EnsureImageExists(ctx, nil, pod, container.Image, testCase.pullSecrets, nil, "", container.ImagePullPolicy)
			assert.NoError(t, err)
			wg.Done()
		}()
	}
	time.Sleep(1 * time.Second)
	fakeRuntime.AssertCallCounts("PullImage", 5)

	// Next two EnsureImageExists should be blocked because maxParallelImagePulls is hit
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			_, _, err := puller.EnsureImageExists(ctx, nil, pod, container.Image, testCase.pullSecrets, nil, "", container.ImagePullPolicy)
			assert.NoError(t, err)
			wg.Done()
		}()
	}
	time.Sleep(1 * time.Second)
	fakeRuntime.AssertCallCounts("PullImage", 5)

	// Unblock two image pulls from runtime, and two EnsureImageExists can go through
	fakeRuntime.UnblockImagePulls(2)
	time.Sleep(1 * time.Second)
	fakeRuntime.AssertCallCounts("PullImage", 7)

	// Unblock the remaining 5 image pulls from runtime, and all EnsureImageExists can go through
	fakeRuntime.UnblockImagePulls(5)

	wg.Wait()
	fakeRuntime.AssertCallCounts("PullImage", 7)
}

func TestEvalCRIPullErr(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		input  error
		assert func(string, error)
	}{
		{
			name:  "fallback error",
			input: errors.New("test"),
			assert: func(msg string, err error) {
				assert.ErrorIs(t, err, ErrImagePull)
				assert.Contains(t, msg, "test")
			},
		},
		{
			name:  "registry is unavailable",
			input: crierrors.ErrRegistryUnavailable,
			assert: func(msg string, err error) {
				assert.ErrorIs(t, err, crierrors.ErrRegistryUnavailable)
				assert.Equal(t, "image pull failed for test because the registry is unavailable", msg)
			},
		},
		{
			name:  "registry is unavailable with additional error message",
			input: fmt.Errorf("%v: foo", crierrors.ErrRegistryUnavailable),
			assert: func(msg string, err error) {
				assert.ErrorIs(t, err, crierrors.ErrRegistryUnavailable)
				assert.Equal(t, "image pull failed for test because the registry is unavailable: foo", msg)
			},
		},
		{
			name:  "signature is invalid",
			input: crierrors.ErrSignatureValidationFailed,
			assert: func(msg string, err error) {
				assert.ErrorIs(t, err, crierrors.ErrSignatureValidationFailed)
				assert.Equal(t, "image pull failed for test because the signature validation failed", msg)
			},
		},
		{
			name:  "signature is invalid with additional error message (wrapped)",
			input: fmt.Errorf("%w: bar", crierrors.ErrSignatureValidationFailed),
			assert: func(msg string, err error) {
				assert.ErrorIs(t, err, crierrors.ErrSignatureValidationFailed)
				assert.Equal(t, "image pull failed for test because the signature validation failed: bar", msg)
			},
		},
	} {
		testInput := tc.input
		testAssert := tc.assert

		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			msg, err := evalCRIPullErr("test", testInput)
			testAssert(msg, err)
		})
	}
}

func TestImagePullPrecheck(t *testing.T) {
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "test_pod",
			Namespace:       "test-ns",
			UID:             "bar",
			ResourceVersion: "42",
		}}

	cases := pullerTestCases()

	useSerializedEnv := true
	for _, c := range cases {
		t.Run(c.testName, func(t *testing.T) {
			ctx := ktesting.Init(t)
			puller, fakeClock, fakeRuntime, container, _, fakeRecorder := pullerTestEnv(t, c, useSerializedEnv, nil)

			for _, expected := range c.expected {
				fakeRuntime.CalledFunctions = nil
				fakeRecorder.Events = []*v1.Event{}
				fakeClock.Step(time.Second)

				_, _, err := puller.EnsureImageExists(ctx, &v1.ObjectReference{}, pod, container.Image, c.pullSecrets, nil, "", container.ImagePullPolicy)
				fakeRuntime.AssertCalls(expected.calls)
				var recorderEvents []v1.Event
				for _, event := range fakeRecorder.Events {
					recorderEvents = append(recorderEvents, v1.Event{Reason: event.Reason})
				}
				if diff := cmp.Diff(recorderEvents, expected.events); diff != "" {
					t.Errorf("unexpected events diff (-want +got):\n%s", diff)
				}
				if diff := cmp.Diff(expected.err, err, cmpopts.EquateErrors()); diff != "" {
					ctx.Errorf("did not get expected error: %v\ndiff (-want, +got):\n%s", err, diff)
				}
			}
		})
	}
}

func makeDockercfgSecretForRepo(sMeta metav1.ObjectMeta, repo string) v1.Secret {
	return v1.Secret{
		ObjectMeta: sMeta,
		Type:       v1.SecretTypeDockerConfigJson,
		Data: map[string][]byte{
			v1.DockerConfigJsonKey: []byte(`{"auths": {"` + repo + `": {"auth": "dXNlcjpwYXNzd29yZA=="}}}`),
		},
	}
}
