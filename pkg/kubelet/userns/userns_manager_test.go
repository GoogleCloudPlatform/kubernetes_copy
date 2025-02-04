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

package userns

import (
	"errors"
	"fmt"
	"os"
	goruntime "runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
	pkgfeatures "k8s.io/kubernetes/pkg/features"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
)

const (
	// skip the first block
	minimumMappingUID = userNsLength
	// allocate enough space for 2000 user namespaces
	mappingLen  = userNsLength * 2000
	testMaxPods = 110
)

type testUserNsPodsManager struct {
	podDir         string
	podList        []types.UID
	userns         bool
	maxPods        int
	mappingFirstID uint32
	mappingLen     uint32
}

func (m *testUserNsPodsManager) GetPodDir(podUID types.UID) string {
	if m.podDir == "" {
		return "/tmp/non-existant-dir.This-is-not-used-in-tests"
	}
	return m.podDir
}

func (m *testUserNsPodsManager) ListPodsFromDisk() ([]types.UID, error) {
	if len(m.podList) == 0 {
		return nil, nil
	}
	return m.podList, nil
}

func (m *testUserNsPodsManager) HandlerSupportsUserNamespaces(runtimeHandler string) (bool, error) {
	if runtimeHandler == "error" {
		return false, errors.New("unknown runtime")
	}
	return m.userns, nil
}

func (m *testUserNsPodsManager) GetKubeletMappings() (uint32, uint32, error) {
	if m.mappingFirstID != 0 {
		return m.mappingFirstID, m.mappingLen, nil
	}
	return minimumMappingUID, mappingLen, nil
}

func (m *testUserNsPodsManager) GetMaxPods() int {
	if m.maxPods != 0 {
		return m.maxPods
	}

	return testMaxPods
}

func TestUserNsManagerAllocate(t *testing.T) {
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, pkgfeatures.UserNamespacesSupport, true)

	testUserNsPodsManager := &testUserNsPodsManager{}
	m, err := MakeUserNsManager(testUserNsPodsManager)
	require.NoError(t, err)

	allocated, length, err := m.allocateOne("one")
	assert.NoError(t, err)
	assert.Equal(t, userNsLength, int(length), "m.isSet(%d).length=%v", allocated, length)
	assert.True(t, m.isSet(allocated), "m.isSet(%d)", allocated)

	allocated2, length2, err := m.allocateOne("two")
	assert.NoError(t, err)
	assert.NotEqual(t, allocated, allocated2, "allocated != allocated2")
	assert.Equal(t, length, length2, "length == length2")

	// verify that re-adding the same pod with the same settings won't fail
	err = m.record("two", allocated2, length2)
	assert.NoError(t, err)
	// but it fails if anything is different
	err = m.record("two", allocated2+1, length2)
	assert.Error(t, err)

	m.Release("one")
	m.Release("two")
	assert.False(t, m.isSet(allocated), "m.isSet(%d)", allocated)
	assert.False(t, m.isSet(allocated2), "m.nsSet(%d)", allocated2)

	var allocs []uint32
	for i := 0; i < 1000; i++ {
		allocated, length, err = m.allocateOne(types.UID(fmt.Sprintf("%d", i)))
		assert.Equal(t, userNsLength, int(length), "length is not the expected. iter: %v", i)
		assert.NoError(t, err)
		assert.GreaterOrEqual(t, allocated, uint32(minimumMappingUID))
		// The last ID of the userns range (allocated+userNsLength) should be within bounds.
		assert.LessOrEqual(t, allocated, uint32(minimumMappingUID+mappingLen-userNsLength))
		allocs = append(allocs, allocated)
	}
	for i, v := range allocs {
		assert.True(t, m.isSet(v), "m.isSet(%d) should be true", v)
		m.Release(types.UID(fmt.Sprintf("%d", i)))
		assert.False(t, m.isSet(v), "m.isSet(%d) should be false", v)

		err = m.record(types.UID(fmt.Sprintf("%d", i)), v, userNsLength)
		assert.NoError(t, err)
		m.Release(types.UID(fmt.Sprintf("%d", i)))
		assert.False(t, m.isSet(v), "m.isSet(%d) should be false", v)
	}
}

func TestMakeUserNsManager(t *testing.T) {
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, pkgfeatures.UserNamespacesSupport, true)

	cases := []struct {
		name           string
		mappingFirstID uint32
		mappingLen     uint32
		maxPods        int
		success        bool
	}{
		{
			name:    "default",
			success: true,
		},
		{
			name:           "firstID not multiple",
			mappingFirstID: 65536 + 1,
		},
		{
			name:           "firstID is less than 65535",
			mappingFirstID: 1,
		},
		{
			name:           "mappingLen not multiple",
			mappingFirstID: 65536,
			mappingLen:     65536 + 1,
		},
		{
			name:           "range can't fit maxPods",
			mappingFirstID: 65536,
			mappingLen:     65536,
			maxPods:        2,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			testUserNsPodsManager := &testUserNsPodsManager{
				podDir:         t.TempDir(),
				mappingFirstID: tc.mappingFirstID,
				mappingLen:     tc.mappingLen,
				maxPods:        tc.maxPods,
			}
			_, err := MakeUserNsManager(testUserNsPodsManager)

			if tc.success {
				assert.NoError(t, err)
			} else {
				assert.Error(t, err)
			}
		})
	}
}

func TestUserNsManagerParseUserNsFile(t *testing.T) {
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, pkgfeatures.UserNamespacesSupport, true)

	cases := []struct {
		name    string
		file    string
		success bool
	}{
		{
			name: "basic",
			file: `{
	                        "uidMappings":[ { "hostId":131072, "containerId":0, "length":65536 } ],
	                        "gidMappings":[ { "hostId":131072, "containerId":0, "length":65536 } ]
                               }`,
			success: true,
		},
		{
			name: "invalid length",
			file: `{
	                        "uidMappings":[ { "hostId":131072, "containerId":0, "length":0 } ],
	                        "gidMappings":[ { "hostId":131072, "containerId":0, "length":0 } ]
                               }`,
			success: false,
		},
		{
			name: "wrong offset",
			file: `{
	                        "uidMappings":[ {"hostId":131072, "containerId":0, "length":65536 } ],
	                        "gidMappings":[ {"hostId":1, "containerId":0, "length":65536 } ]
                               }`,
			success: false,
		},
		{
			name: "two GID mappings",
			file: `{
	                        "uidMappings":[ { "hostId":131072, "containerId":0, "length":userNsLength } ],
	                        "gidMappings":[ { "hostId":131072, "containerId":0, "length":userNsLength }, { "hostId":196608, "containerId":0, "length":65536 } ]
                               }`,
			success: false,
		},
		{
			name: "two UID mappings",
			file: `{
	                        "uidMappings":[ { "hostId":131072, "containerId":0, "length":65536 }, { "hostId":196608, "containerId":0, "length":65536 } ],
	                        "gidMappings":[ { "hostId":131072, "containerId":0, "length":65536 } ]
                               }`,
			success: false,
		},
		{
			name: "no root UID",
			file: `{
	                        "uidMappings":[ { "hostId":131072, "containerId":1, "length":65536 } ],
	                        "gidMappings":[ { "hostId":131072, "containerId":0, "length":65536 } ]
                               }`,
			success: false,
		},
		{
			name: "no root GID",
			file: `{
	                        "uidMappings":[ { "hostId":131072, "containerId":0, "length":65536 } ],
	                        "gidMappings":[ { "hostId":131072, "containerId":1, "length":65536 } ]
                               }`,
			success: false,
		},
	}

	testUserNsPodsManager := &testUserNsPodsManager{}
	m, err := MakeUserNsManager(testUserNsPodsManager)
	assert.NoError(t, err)

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// We don't validate the result. It was parsed with the json parser, we trust that.
			_, err = m.parseUserNsFileAndRecord(types.UID(tc.name), []byte(tc.file))
			if (tc.success && err == nil) || (!tc.success && err != nil) {
				return
			}

			t.Errorf("expected success: %v but got error: %v", tc.success, err)
		})
	}
}

func TestGetOrCreateUserNamespaceMappings(t *testing.T) {
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, pkgfeatures.UserNamespacesSupport, true)

	trueVal := true
	falseVal := false

	cases := []struct {
		name           string
		pod            *v1.Pod
		expMode        runtimeapi.NamespaceMode
		runtimeUserns  bool
		runtimeHandler string
		success        bool
		skipOnWindows  bool
	}{
		{
			name:    "no user namespace",
			pod:     &v1.Pod{},
			expMode: runtimeapi.NamespaceMode_NODE,
			success: true,
		},
		{
			name:    "nil pod",
			pod:     nil,
			expMode: runtimeapi.NamespaceMode_NODE,
			success: true,
		},
		{
			name: "opt-in to host user namespace",
			pod: &v1.Pod{
				Spec: v1.PodSpec{
					HostUsers: &trueVal,
				},
			},
			expMode: runtimeapi.NamespaceMode_NODE,
			success: true,
		},
		{
			name: "user namespace",
			pod: &v1.Pod{
				Spec: v1.PodSpec{
					HostUsers: &falseVal,
				},
			},
			expMode:       runtimeapi.NamespaceMode_POD,
			runtimeUserns: true,
			success:       true,
			skipOnWindows: true,
		},
		{
			name: "user namespace, but no runtime support",
			pod: &v1.Pod{
				Spec: v1.PodSpec{
					HostUsers: &falseVal,
				},
			},
			runtimeUserns: false,
		},
		{
			name: "user namespace, but runtime returns error",
			pod: &v1.Pod{
				Spec: v1.PodSpec{
					HostUsers: &falseVal,
				},
			},
			// This handler name makes the fake runtime return an error.
			runtimeHandler: "error",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.skipOnWindows && goruntime.GOOS == "windows" {
				// TODO: remove skip once the failing test has been fixed.
				t.Skip("Skip failing test on Windows.")
			}
			// These tests will create the userns file, so use an existing podDir.
			testUserNsPodsManager := &testUserNsPodsManager{
				podDir: t.TempDir(),
				userns: tc.runtimeUserns,
			}
			m, err := MakeUserNsManager(testUserNsPodsManager)
			assert.NoError(t, err)

			userns, err := m.GetOrCreateUserNamespaceMappings(tc.pod, tc.runtimeHandler)
			if (tc.success && err != nil) || (!tc.success && err == nil) {
				t.Errorf("expected success: %v but got error: %v", tc.success, err)
			}

			if userns.GetMode() != tc.expMode {
				t.Errorf("expected mode: %v but got: %v", tc.expMode, userns.GetMode())
			}
		})
	}
}

func TestCleanupOrphanedPodUsernsAllocations(t *testing.T) {
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, pkgfeatures.UserNamespacesSupport, true)

	cases := []struct {
		name                 string
		runningPods          []*kubecontainer.Pod
		pods                 []*v1.Pod
		listPods             []types.UID /* pods to list */
		podSetBeforeCleanup  []types.UID /* pods to record before cleanup */
		podSetAfterCleanup   []types.UID /* pods set expected after cleanup */
		podUnsetAfterCleanup []types.UID /* pods set expected after cleanup */
	}{
		{
			name:     "no stale pods",
			listPods: []types.UID{"pod-1", "pod-2"},
		},
		{
			name:                 "no stale pods set",
			podSetBeforeCleanup:  []types.UID{"pod-1", "pod-2"},
			listPods:             []types.UID{"pod-1", "pod-2"},
			podUnsetAfterCleanup: []types.UID{"pod-1", "pod-2"},
		},
		{
			name:                 "one running pod",
			listPods:             []types.UID{"pod-1", "pod-2"},
			podSetBeforeCleanup:  []types.UID{"pod-1", "pod-2"},
			runningPods:          []*kubecontainer.Pod{{ID: "pod-1"}},
			podSetAfterCleanup:   []types.UID{"pod-1"},
			podUnsetAfterCleanup: []types.UID{"pod-2"},
		},
		{
			name:                 "pod set before cleanup but not listed ==> unset",
			podSetBeforeCleanup:  []types.UID{"pod-1", "pod-2"},
			runningPods:          []*kubecontainer.Pod{{ID: "pod-1"}},
			podUnsetAfterCleanup: []types.UID{"pod-1", "pod-2"},
		},
		{
			name:                 "one pod",
			listPods:             []types.UID{"pod-1", "pod-2"},
			podSetBeforeCleanup:  []types.UID{"pod-1", "pod-2"},
			pods:                 []*v1.Pod{{ObjectMeta: metav1.ObjectMeta{UID: "pod-1"}}},
			podSetAfterCleanup:   []types.UID{"pod-1"},
			podUnsetAfterCleanup: []types.UID{"pod-2"},
		},
		{
			name:                 "no listed pods ==> all unset",
			podSetBeforeCleanup:  []types.UID{"pod-1", "pod-2"},
			podUnsetAfterCleanup: []types.UID{"pod-1", "pod-2"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			testUserNsPodsManager := &testUserNsPodsManager{
				podDir:  t.TempDir(),
				podList: tc.listPods,
			}
			m, err := MakeUserNsManager(testUserNsPodsManager)
			require.NoError(t, err)

			// Record the userns range as used
			for i, pod := range tc.podSetBeforeCleanup {
				err := m.record(pod, uint32((i+1)*65536), 65536)
				require.NoError(t, err)
			}

			err = m.CleanupOrphanedPodUsernsAllocations(tc.pods, tc.runningPods)
			require.NoError(t, err)

			for _, pod := range tc.podSetAfterCleanup {
				ok := m.podAllocated(pod)
				assert.True(t, ok, "pod %q should be allocated", pod)
			}

			for _, pod := range tc.podUnsetAfterCleanup {
				ok := m.podAllocated(pod)
				assert.False(t, ok, "pod %q should not be allocated", pod)
			}
		})
	}
}

type failingUserNsPodsManager struct {
	testUserNsPodsManager
}

func (m *failingUserNsPodsManager) ListPodsFromDisk() ([]types.UID, error) {
	return nil, os.ErrPermission
}

func TestMakeUserNsManagerFailsListPod(t *testing.T) {
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, pkgfeatures.UserNamespacesSupport, true)

	testUserNsPodsManager := &failingUserNsPodsManager{}
	_, err := MakeUserNsManager(testUserNsPodsManager)
	assert.Error(t, err)
	assert.ErrorContains(t, err, "read pods from disk")
}

func TestRecordBounds(t *testing.T) {
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, pkgfeatures.UserNamespacesSupport, true)

	// Allow exactly for 1 pod
	testUserNsPodsManager := &testUserNsPodsManager{
		mappingFirstID: 65536,
		mappingLen:     65536,
		maxPods:        1,
	}
	m, err := MakeUserNsManager(testUserNsPodsManager)
	require.NoError(t, err)

	// The first pod allocation should succeed.
	err = m.record(types.UID(fmt.Sprintf("%d", 0)), 65536, 65536)
	require.NoError(t, err)

	// The next allocation should fail, as there is no space left.
	err = m.record(types.UID(fmt.Sprintf("%d", 2)), uint32(2*65536), 65536)
	assert.Error(t, err)
	assert.ErrorContains(t, err, "out of range")
}
