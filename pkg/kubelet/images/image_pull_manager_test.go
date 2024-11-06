/*
Copyright 2024 The Kubernetes Authors.

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
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	kubeletconfiginternal "k8s.io/kubernetes/pkg/kubelet/apis/config"
	"k8s.io/kubernetes/pkg/kubelet/container"
	ctesting "k8s.io/kubernetes/pkg/kubelet/container/testing"
	"k8s.io/kubernetes/test/utils/ktesting"
)

func Test_pulledRecordMergeNewCreds(t *testing.T) {
	testTime := metav1.Time{Time: time.Date(2022, 2, 22, 12, 00, 00, 00, time.Local)}
	testRecord := &kubeletconfiginternal.ImagePulledRecord{
		ImageRef:        "testImageRef",
		LastUpdatedTime: testTime,
		CredentialMapping: map[string]kubeletconfiginternal.ImagePullCredentials{
			"test-image1": {
				KubernetesSecrets: []kubeletconfiginternal.ImagePullSecret{
					{UID: "uid1", Namespace: "namespace1", Name: "name1", CredentialHash: "hash1"},
				},
			},
			"test-image2": {
				KubernetesSecrets: []kubeletconfiginternal.ImagePullSecret{
					{UID: "uid1", Namespace: "namespace1", Name: "name1", CredentialHash: "hash1"},
					{UID: "uid2", Namespace: "namespace2", Name: "name2", CredentialHash: "hash2"},
				},
			},
			"test-nodewide": {
				NodePodsAccessible: true,
			},
		},
	}

	tests := []struct {
		name            string
		current         *kubeletconfiginternal.ImagePulledRecord
		image           string
		credsForMerging *kubeletconfiginternal.ImagePullCredentials
		expectedRecord  *kubeletconfiginternal.ImagePulledRecord
		wantUpdate      bool
	}{
		{
			name:    "create a new image record",
			image:   "new-image",
			current: testRecord.DeepCopy(),
			credsForMerging: &kubeletconfiginternal.ImagePullCredentials{
				KubernetesSecrets: []kubeletconfiginternal.ImagePullSecret{
					{UID: "newuid", Namespace: "newnamespace", Name: "newname", CredentialHash: "newhash"},
				},
			},
			expectedRecord: withImageRecord(testRecord.DeepCopy(), "new-image",
				kubeletconfiginternal.ImagePullCredentials{
					KubernetesSecrets: []kubeletconfiginternal.ImagePullSecret{
						{UID: "newuid", Namespace: "newnamespace", Name: "newname", CredentialHash: "newhash"},
					},
				},
			),
			wantUpdate: true,
		},
		{
			name:    "merge with an existing image secret",
			image:   "test-image1",
			current: testRecord.DeepCopy(),
			credsForMerging: &kubeletconfiginternal.ImagePullCredentials{
				KubernetesSecrets: []kubeletconfiginternal.ImagePullSecret{
					{UID: "newuid", Namespace: "newnamespace", Name: "newname", CredentialHash: "newhash"},
				},
			},
			expectedRecord: withImageRecord(testRecord.DeepCopy(), "test-image1",
				kubeletconfiginternal.ImagePullCredentials{
					KubernetesSecrets: []kubeletconfiginternal.ImagePullSecret{
						{UID: "uid1", Namespace: "namespace1", Name: "name1", CredentialHash: "hash1"},
						{UID: "newuid", Namespace: "newnamespace", Name: "newname", CredentialHash: "newhash"},
					},
				},
			),
			wantUpdate: true,
		},

		{
			name:    "merge with existing image record secrets",
			image:   "test-image2",
			current: testRecord.DeepCopy(),
			credsForMerging: &kubeletconfiginternal.ImagePullCredentials{
				KubernetesSecrets: []kubeletconfiginternal.ImagePullSecret{
					{UID: "newuid", Namespace: "namespace1", Name: "newname", CredentialHash: "newhash"},
				},
			},
			expectedRecord: withImageRecord(testRecord.DeepCopy(), "test-image2",
				kubeletconfiginternal.ImagePullCredentials{
					KubernetesSecrets: []kubeletconfiginternal.ImagePullSecret{
						{UID: "uid1", Namespace: "namespace1", Name: "name1", CredentialHash: "hash1"},
						{UID: "newuid", Namespace: "namespace1", Name: "newname", CredentialHash: "newhash"},
						{UID: "uid2", Namespace: "namespace2", Name: "name2", CredentialHash: "hash2"},
					},
				},
			),
			wantUpdate: true,
		},
		{
			name:    "node-accessible overrides all secrets",
			image:   "test-image2",
			current: testRecord.DeepCopy(),
			credsForMerging: &kubeletconfiginternal.ImagePullCredentials{
				NodePodsAccessible: true,
			},
			expectedRecord: withImageRecord(testRecord.DeepCopy(), "test-image2",
				kubeletconfiginternal.ImagePullCredentials{
					NodePodsAccessible: true,
				},
			),
			wantUpdate: true,
		},
		{
			name:    "new creds have the same secret coordinates but a different hash",
			image:   "test-image2",
			current: testRecord.DeepCopy(),
			credsForMerging: &kubeletconfiginternal.ImagePullCredentials{
				KubernetesSecrets: []kubeletconfiginternal.ImagePullSecret{
					{UID: "uid2", Namespace: "namespace2", Name: "name2", CredentialHash: "newhash"},
				},
			},
			expectedRecord: withImageRecord(testRecord.DeepCopy(), "test-image2",
				kubeletconfiginternal.ImagePullCredentials{
					KubernetesSecrets: []kubeletconfiginternal.ImagePullSecret{
						{UID: "uid1", Namespace: "namespace1", Name: "name1", CredentialHash: "hash1"},
						{UID: "uid2", Namespace: "namespace2", Name: "name2", CredentialHash: "newhash"},
					},
				},
			),
			wantUpdate: true,
		},
		{
			name:    "new creds have the same hash but a different coordinates",
			image:   "test-image2",
			current: testRecord.DeepCopy(),
			credsForMerging: &kubeletconfiginternal.ImagePullCredentials{
				KubernetesSecrets: []kubeletconfiginternal.ImagePullSecret{
					{UID: "uid3", Namespace: "namespace2", Name: "name3", CredentialHash: "hash2"},
				},
			},
			expectedRecord: withImageRecord(testRecord.DeepCopy(), "test-image2",
				kubeletconfiginternal.ImagePullCredentials{
					KubernetesSecrets: []kubeletconfiginternal.ImagePullSecret{
						{UID: "uid1", Namespace: "namespace1", Name: "name1", CredentialHash: "hash1"},
						{UID: "uid2", Namespace: "namespace2", Name: "name2", CredentialHash: "hash2"},
						{UID: "uid3", Namespace: "namespace2", Name: "name3", CredentialHash: "hash2"},
					},
				},
			),
			wantUpdate: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotRecord, gotUpdate := pulledRecordMergeNewCreds(tt.current, tt.image, tt.credsForMerging)
			if gotUpdate != tt.wantUpdate {
				t.Errorf("pulledRecordMergeNewCreds() gotUpdate = %v, wantUpdate %v", gotUpdate, tt.wantUpdate)
			}
			if origTime, newTime := tt.expectedRecord.LastUpdatedTime, gotRecord.LastUpdatedTime; tt.wantUpdate && !origTime.Before(&newTime) {
				t.Errorf("expected the new update time to be after the original update time: %v > %v", origTime, newTime)
			}
			// make the new update time equal to the expected time for the below comparison now
			gotRecord.LastUpdatedTime = tt.expectedRecord.LastUpdatedTime

			if !reflect.DeepEqual(gotRecord, tt.expectedRecord) {
				t.Errorf("pulledRecordMergeNewCreds() difference between got/expected: %v", cmp.Diff(tt.expectedRecord, gotRecord))
			}
		})
	}
}

func TestFileBasedImagePullManager_MustAttemptImagePull(t *testing.T) {
	tests := []struct {
		name            string
		imagePullPolicy ImagePullPolicyEnforcer
		podSecrets      []kubeletconfiginternal.ImagePullSecret
		image           string
		imageRef        string
		pulledFiles     []string
		pullingFiles    []string
		want            bool
	}{
		{
			name:            "image exists and is recorded with pod's exact secret",
			imagePullPolicy: NeverVerifyPreloadedPullPolicy,
			podSecrets: []kubeletconfiginternal.ImagePullSecret{
				{
					UID: "testsecretuid", Namespace: "default", Name: "pull-secret", CredentialHash: "testsecrethash",
				},
			},
			image:       "docker.io/testing/test:latest",
			imageRef:    "testimageref",
			pulledFiles: []string{"sha256-b3c0cc4278800b03a308ceb2611161430df571ca733122f0a40ac8b9792a9064"},
			want:        false,
		},
		{
			name:            "image exists and is recorded, no pod secrets",
			imagePullPolicy: NeverVerifyPreloadedPullPolicy,
			image:           "docker.io/testing/test:latest",
			imageRef:        "testimageref",
			pulledFiles:     []string{"sha256-b3c0cc4278800b03a308ceb2611161430df571ca733122f0a40ac8b9792a9064"},
			want:            true,
		},
		{
			name:            "image exists and is recorded with the same secret but different credential hash",
			imagePullPolicy: NeverVerifyPreloadedPullPolicy,
			podSecrets: []kubeletconfiginternal.ImagePullSecret{
				{
					UID: "testsecretuid", Namespace: "default", Name: "pull-secret", CredentialHash: "differenthash",
				},
			},
			image:       "docker.io/testing/test:latest",
			imageRef:    "testimageref",
			pulledFiles: []string{"sha256-b3c0cc4278800b03a308ceb2611161430df571ca733122f0a40ac8b9792a9064"},
			want:        false,
		},
		{
			name:            "image exists and is recorded with a different secret with a different UID",
			imagePullPolicy: NeverVerifyPreloadedPullPolicy,
			podSecrets: []kubeletconfiginternal.ImagePullSecret{
				{
					UID: "different uid", Namespace: "default", Name: "pull-secret", CredentialHash: "differenthash",
				},
			},
			image:       "docker.io/testing/test:latest",
			imageRef:    "testimageref",
			pulledFiles: []string{"sha256-b3c0cc4278800b03a308ceb2611161430df571ca733122f0a40ac8b9792a9064"},
			want:        true,
		},
		{
			name:            "image exists and is recorded with a different secret",
			imagePullPolicy: NeverVerifyPreloadedPullPolicy,
			podSecrets: []kubeletconfiginternal.ImagePullSecret{
				{
					UID: "testsecretuid", Namespace: "differentns", Name: "pull-secret", CredentialHash: "differenthash",
				},
			},
			image:       "docker.io/testing/test:latest",
			imageRef:    "testimageref",
			pulledFiles: []string{"sha256-b3c0cc4278800b03a308ceb2611161430df571ca733122f0a40ac8b9792a9064"},
			want:        true,
		},
		{
			name:            "image exists and is recorded with a different secret with the same credential hash",
			imagePullPolicy: NeverVerifyPreloadedPullPolicy,
			podSecrets: []kubeletconfiginternal.ImagePullSecret{
				{
					UID: "testsecretuid", Namespace: "differentns", Name: "pull-secret", CredentialHash: "testsecrethash",
				},
			},
			image:       "docker.io/testing/test:latest",
			imageRef:    "testimageref",
			pulledFiles: []string{"sha256-b3c0cc4278800b03a308ceb2611161430df571ca733122f0a40ac8b9792a9064"},
			want:        false,
		},
		{
			name:            "image exists but the pull is recorded with a different image name but with the exact same secret",
			imagePullPolicy: NeverVerifyPreloadedPullPolicy,
			podSecrets: []kubeletconfiginternal.ImagePullSecret{
				{
					UID: "testsecretuid", Namespace: "default", Name: "pull-secret", CredentialHash: "testsecrethash",
				},
			},
			image:       "docker.io/testing/different:latest",
			imageRef:    "testimageref",
			pulledFiles: []string{"sha256-b3c0cc4278800b03a308ceb2611161430df571ca733122f0a40ac8b9792a9064"},
			want:        true,
		},
		{
			name:            "image exists and is recorded with empty credential mapping",
			imagePullPolicy: NeverVerifyPreloadedPullPolicy,
			image:           "docker.io/testing/test:latest",
			imageRef:        "testemptycredmapping",
			pulledFiles:     []string{"sha256-f8778b6393eaf39315e767a58cbeacf2c4b270d94b4d6926ee993d9e49444991"},
			want:            true,
		},
		{
			name:            "image does not exist and there are no records of it",
			imagePullPolicy: NeverVerifyPreloadedPullPolicy,
			image:           "docker.io/testing/test:latest",
			imageRef:        "",
			want:            true,
		},
		{
			name:            "image exists and there are no records of it with NeverVerifyPreloadedImages pull policy",
			imagePullPolicy: NeverVerifyPreloadedPullPolicy,
			image:           "docker.io/testing/test:latest",
			imageRef:        "testexistingref",
			want:            false,
		},
		{
			name:            "image exists and there are no records of it with AlwaysVerify pull policy",
			imagePullPolicy: AlwaysVerifyImagePullPolicy,
			image:           "docker.io/testing/test:latest",
			imageRef:        "testexistingref",
			want:            true,
		},
		{
			name:            "image exists but is only recorded via pulling intent",
			imagePullPolicy: NeverVerifyPreloadedPullPolicy,
			podSecrets: []kubeletconfiginternal.ImagePullSecret{
				{
					UID: "testsecretuid", Namespace: "default", Name: "pull-secret", CredentialHash: "testsecrethash",
				},
			},
			image:        "docker.io/testing/test:latest",
			imageRef:     "testexistingref",
			pullingFiles: []string{"sha256-aef2af226629a35d5f3ef0fdbb29fdbebf038d0acd8850590e8c48e1e283aa56"},
			want:         true,
		},
		{
			name:            "image exists but is only recorded via pulling intent - NeverVerify policy",
			imagePullPolicy: NeverVerifyImagePullPolicy,
			podSecrets: []kubeletconfiginternal.ImagePullSecret{
				{
					UID: "testsecretuid", Namespace: "default", Name: "pull-secret", CredentialHash: "testsecrethash",
				},
			},
			image:        "docker.io/testing/test:latest",
			imageRef:     "testexistingref",
			pullingFiles: []string{"sha256-aef2af226629a35d5f3ef0fdbb29fdbebf038d0acd8850590e8c48e1e283aa56"},
			want:         false,
		},
		{
			name:            "image exists and is recorded as node-accessible, no pod secrets",
			imagePullPolicy: NeverVerifyPreloadedPullPolicy,
			image:           "docker.io/testing/test:latest",
			imageRef:        "testimage-anonpull",
			pulledFiles:     []string{"sha256-a2eace2182b24cdbbb730798e47b10709b9ef5e0f0c1624a3bc06c8ca987727a"},
			want:            false,
		},
		{
			name:            "image exists and is recorded as node-accessible, request with pod secrets",
			imagePullPolicy: NeverVerifyPreloadedPullPolicy,
			podSecrets: []kubeletconfiginternal.ImagePullSecret{
				{
					UID: "testsecretuid", Namespace: "default", Name: "pull-secret", CredentialHash: "testsecrethash",
				},
			},
			image:       "docker.io/testing/test:latest",
			imageRef:    "testimage-anonpull",
			pulledFiles: []string{"sha256-a2eace2182b24cdbbb730798e47b10709b9ef5e0f0c1624a3bc06c8ca987727a"},
			want:        false,
		},
		{
			name:            "image exists and is recorded with empty hash as its hashing originally failed, the same fail for a different pod secret",
			imagePullPolicy: NeverVerifyPreloadedPullPolicy,
			podSecrets: []kubeletconfiginternal.ImagePullSecret{
				{
					UID: "testsecretuid", Namespace: "differentns", Name: "pull-secret", CredentialHash: "",
				},
			},
			image:       "docker.io/testing/test:latest",
			imageRef:    "test-brokenhash",
			pulledFiles: []string{"sha256-38a8906435c4dd5f4258899d46621bfd8eea3ad6ff494ee3c2f17ef0321625bd"},
			want:        true,
		},
		{
			name:            "image exists and is recorded with empty hash as its hashing originally failed, the same fail for the same pod secret",
			imagePullPolicy: NeverVerifyPreloadedPullPolicy,
			podSecrets: []kubeletconfiginternal.ImagePullSecret{
				{
					UID: "testsecretuid", Namespace: "default", Name: "pull-secret", CredentialHash: "",
				},
			},
			image:       "docker.io/testing/test:latest",
			imageRef:    "test-brokenhash",
			pulledFiles: []string{"sha256-38a8906435c4dd5f4258899d46621bfd8eea3ad6ff494ee3c2f17ef0321625bd"},
			want:        false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			encoder, decoder, err := createKubeletConfigSchemeEncoderDecoder()
			require.NoError(t, err)

			testDir := t.TempDir()
			pullingDir := filepath.Join(testDir, "pulling")
			pulledDir := filepath.Join(testDir, "pulled")

			copyTestData(t, pullingDir, "pulling", tt.pullingFiles)
			copyTestData(t, pulledDir, "pulled", tt.pulledFiles)

			fsRecordAccessor := &fsPullRecordsAccessor{
				pullingDir: pullingDir,
				pulledDir:  pulledDir,
				encoder:    encoder,
				decoder:    decoder,
			}

			f := &PullManager{
				recordsAccessor:               fsRecordAccessor,
				requireCredentialVerification: tt.imagePullPolicy,
				intentAccessors:               NewNamedLockSet(),
				intentCounters:                map[string]int{},
				pulledAccessors:               NewNamedLockSet(),
			}
			if got := f.MustAttemptImagePull(tt.image, tt.imageRef, tt.podSecrets); got != tt.want {
				t.Errorf("FileBasedImagePullManager.MustAttemptImagePull() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestFileBasedImagePullManager_RecordPullIntent(t *testing.T) {
	tests := []struct {
		name         string
		inputImage   string
		wantFile     string
		startCounter int
		wantCounter  int
	}{
		{
			name:        "first pull",
			inputImage:  "repo.repo/test/test:latest",
			wantFile:    "sha256-7d8c031e2f1aeaa71649ca3e0b64c9902370ed460ef57fb07582a87a5a1e1c02",
			wantCounter: 1,
		},
		{
			name:         "first pull",
			inputImage:   "repo.repo/test/test:latest",
			wantFile:     "sha256-7d8c031e2f1aeaa71649ca3e0b64c9902370ed460ef57fb07582a87a5a1e1c02",
			startCounter: 1,
			wantCounter:  2,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			encoder, decoder, err := createKubeletConfigSchemeEncoderDecoder()
			require.NoError(t, err)

			testDir := t.TempDir()
			pullingDir := filepath.Join(testDir, "pulling")

			fsRecordAccessor := &fsPullRecordsAccessor{
				pullingDir: pullingDir,
				encoder:    encoder,
				decoder:    decoder,
			}

			f := &PullManager{
				recordsAccessor: fsRecordAccessor,
				intentAccessors: NewNamedLockSet(),
				intentCounters:  make(map[string]int),
			}

			if tt.startCounter > 0 {
				f.intentCounters[tt.inputImage] = tt.startCounter
			}

			_ = f.RecordPullIntent(tt.inputImage)

			expectFilename := filepath.Join(pullingDir, tt.wantFile)
			require.FileExists(t, expectFilename)
			require.Equal(t, tt.wantCounter, f.intentCounters[tt.inputImage], "pull intent counter does not match")

			expected := kubeletconfiginternal.ImagePullIntent{
				Image: tt.inputImage,
			}

			gotBytes, err := os.ReadFile(expectFilename)
			if err != nil {
				t.Fatalf("failed to read the expected file: %v", err)
			}

			var got kubeletconfiginternal.ImagePullIntent
			if _, _, err := decoder.Decode(gotBytes, nil, &got); err != nil {
				t.Fatalf("failed to unmarshal the created file data: %v", err)
			}

			if !reflect.DeepEqual(expected, got) {
				t.Errorf("expected ImagePullIntent != got; diff: %s", cmp.Diff(expected, got))
			}
		})
	}
}

func TestFileBasedImagePullManager_RecordImagePulled(t *testing.T) {
	tests := []struct {
		name                 string
		image                string
		imageRef             string
		creds                *kubeletconfiginternal.ImagePullCredentials
		existingPulling      []string
		existingPulled       []string
		expectPullingRemoved string
		expectPulled         []string
		checkedPullFile      string
		expectedPullRecord   kubeletconfiginternal.ImagePulledRecord
		expectUpdated        bool
	}{
		{
			name:                 "new pull record",
			image:                "repo.repo/test/test:v1",
			imageRef:             "testimageref",
			creds:                &kubeletconfiginternal.ImagePullCredentials{NodePodsAccessible: true},
			expectPulled:         []string{"sha256-b3c0cc4278800b03a308ceb2611161430df571ca733122f0a40ac8b9792a9064"},
			existingPulling:      []string{"sha256-ee81caca15454863449fb55a1d942904d56d5ed9f9b20a7cb3453944ea2c7e11"},
			expectPullingRemoved: "sha256-ee81caca15454863449fb55a1d942904d56d5ed9f9b20a7cb3453944ea2c7e11",
			checkedPullFile:      "sha256-b3c0cc4278800b03a308ceb2611161430df571ca733122f0a40ac8b9792a9064",
			expectUpdated:        true,
			expectedPullRecord: kubeletconfiginternal.ImagePulledRecord{
				ImageRef: "testimageref",
				CredentialMapping: map[string]kubeletconfiginternal.ImagePullCredentials{
					"repo.repo/test/test": {
						NodePodsAccessible: true,
					},
				},
			},
		},
		{
			name:     "merge into existing record",
			image:    "repo.repo/test/test:v1",
			imageRef: "testimageref",
			creds:    &kubeletconfiginternal.ImagePullCredentials{NodePodsAccessible: true},
			existingPulled: []string{
				"sha256-b3c0cc4278800b03a308ceb2611161430df571ca733122f0a40ac8b9792a9064",
				"sha256-a2eace2182b24cdbbb730798e47b10709b9ef5e0f0c1624a3bc06c8ca987727a",
				"sha256-f8778b6393eaf39315e767a58cbeacf2c4b270d94b4d6926ee993d9e49444991",
			},
			expectPulled: []string{
				"sha256-b3c0cc4278800b03a308ceb2611161430df571ca733122f0a40ac8b9792a9064",
				"sha256-a2eace2182b24cdbbb730798e47b10709b9ef5e0f0c1624a3bc06c8ca987727a",
				"sha256-f8778b6393eaf39315e767a58cbeacf2c4b270d94b4d6926ee993d9e49444991",
			},
			existingPulling:      []string{"sha256-ee81caca15454863449fb55a1d942904d56d5ed9f9b20a7cb3453944ea2c7e11"},
			expectPullingRemoved: "sha256-ee81caca15454863449fb55a1d942904d56d5ed9f9b20a7cb3453944ea2c7e11",
			checkedPullFile:      "sha256-b3c0cc4278800b03a308ceb2611161430df571ca733122f0a40ac8b9792a9064",
			expectUpdated:        true,
			expectedPullRecord: kubeletconfiginternal.ImagePulledRecord{
				ImageRef: "testimageref",
				CredentialMapping: map[string]kubeletconfiginternal.ImagePullCredentials{
					"repo.repo/test/test": {
						NodePodsAccessible: true,
					},
					"docker.io/testing/test": {
						KubernetesSecrets: []kubeletconfiginternal.ImagePullSecret{
							{UID: "testsecretuid", Namespace: "default", Name: "pull-secret", CredentialHash: "testsecrethash"},
						},
					},
				},
			},
		},
		{
			name:     "merge into existing record - existing key in creds mapping",
			image:    "docker.io/testing/test:something",
			imageRef: "testimageref",
			creds: &kubeletconfiginternal.ImagePullCredentials{
				KubernetesSecrets: []kubeletconfiginternal.ImagePullSecret{{UID: "newuid", Namespace: "newns", Name: "newname", CredentialHash: "somehash"}},
			},
			existingPulled:       []string{"sha256-b3c0cc4278800b03a308ceb2611161430df571ca733122f0a40ac8b9792a9064"},
			expectPulled:         []string{"sha256-b3c0cc4278800b03a308ceb2611161430df571ca733122f0a40ac8b9792a9064"},
			existingPulling:      []string{"sha256-f24acc752be18b93b0504c86312bbaf482c9efb0c45e925bbccb0a591cebd7af"},
			expectPullingRemoved: "sha256-f24acc752be18b93b0504c86312bbaf482c9efb0c45e925bbccb0a591cebd7af",
			checkedPullFile:      "sha256-b3c0cc4278800b03a308ceb2611161430df571ca733122f0a40ac8b9792a9064",
			expectUpdated:        true,
			expectedPullRecord: kubeletconfiginternal.ImagePulledRecord{
				ImageRef: "testimageref",
				CredentialMapping: map[string]kubeletconfiginternal.ImagePullCredentials{
					"docker.io/testing/test": {
						KubernetesSecrets: []kubeletconfiginternal.ImagePullSecret{
							{UID: "testsecretuid", Namespace: "default", Name: "pull-secret", CredentialHash: "testsecrethash"},
							{UID: "newuid", Namespace: "newns", Name: "newname", CredentialHash: "somehash"},
						},
					},
				},
			},
		},
		{
			name:     "existing record stays unchanged",
			image:    "docker.io/testing/test:something",
			imageRef: "testimageref",
			creds: &kubeletconfiginternal.ImagePullCredentials{
				KubernetesSecrets: []kubeletconfiginternal.ImagePullSecret{{UID: "testsecretuid", Namespace: "default", Name: "pull-secret", CredentialHash: "testsecrethash"}},
			},
			existingPulled:       []string{"sha256-b3c0cc4278800b03a308ceb2611161430df571ca733122f0a40ac8b9792a9064"},
			expectPulled:         []string{"sha256-b3c0cc4278800b03a308ceb2611161430df571ca733122f0a40ac8b9792a9064"},
			existingPulling:      []string{"sha256-f24acc752be18b93b0504c86312bbaf482c9efb0c45e925bbccb0a591cebd7af"},
			expectPullingRemoved: "sha256-f24acc752be18b93b0504c86312bbaf482c9efb0c45e925bbccb0a591cebd7af",
			checkedPullFile:      "sha256-b3c0cc4278800b03a308ceb2611161430df571ca733122f0a40ac8b9792a9064",
			expectUpdated:        false,
			expectedPullRecord: kubeletconfiginternal.ImagePulledRecord{
				ImageRef: "testimageref",
				CredentialMapping: map[string]kubeletconfiginternal.ImagePullCredentials{
					"docker.io/testing/test": {
						KubernetesSecrets: []kubeletconfiginternal.ImagePullSecret{
							{UID: "testsecretuid", Namespace: "default", Name: "pull-secret", CredentialHash: "testsecrethash"},
						},
					},
				},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			encoder, decoder, err := createKubeletConfigSchemeEncoderDecoder()
			require.NoError(t, err)

			testDir := t.TempDir()
			pullingDir := filepath.Join(testDir, "pulling")
			pulledDir := filepath.Join(testDir, "pulled")

			copyTestData(t, pullingDir, "pulling", tt.existingPulling)
			copyTestData(t, pulledDir, "pulled", tt.existingPulled)

			fsRecordAccessor := &fsPullRecordsAccessor{
				pullingDir: pullingDir,
				pulledDir:  pulledDir,
				encoder:    encoder,
				decoder:    decoder,
			}

			f := &PullManager{
				recordsAccessor: fsRecordAccessor,
				intentAccessors: NewNamedLockSet(),
				intentCounters: map[string]int{
					tt.image: 1,
				},
				pulledAccessors: NewNamedLockSet(),
			}
			origIntentCounter := f.intentCounters[tt.image]
			f.RecordImagePulled(tt.image, tt.imageRef, tt.creds)
			require.Equal(t, f.intentCounters[tt.image], origIntentCounter-1, "intent counter for %s was not decremented", tt.image)

			for _, fname := range tt.expectPulled {
				expectFilename := filepath.Join(pulledDir, fname)
				require.FileExists(t, expectFilename)
			}

			if len(tt.expectPullingRemoved) > 0 {
				dontExpectFilename := filepath.Join(pullingDir, tt.expectPullingRemoved)
				require.NoFileExists(t, dontExpectFilename)
			}

			pulledBytes, err := os.ReadFile(filepath.Join(pulledDir, tt.checkedPullFile))
			if err != nil {
				t.Fatalf("failed to read the expected image pulled record: %v", err)
			}

			got, err := decodePulledRecord(decoder, pulledBytes)
			if err != nil {
				t.Fatalf("failed to deserialize the image pulled record: %v", err)
			}

			if tt.expectUpdated {
				require.True(t, got.LastUpdatedTime.After(time.Now().Add(-1*time.Minute)), "expected the record to be updated but it didn't - last update time %s", got.LastUpdatedTime.String())
			} else {
				require.True(t, got.LastUpdatedTime.Before(&metav1.Time{Time: time.Now().Add(-240 * time.Minute)}), "expected the record to NOT be updated but it was - last update time %s", got.LastUpdatedTime.String())
			}
			got.LastUpdatedTime = tt.expectedPullRecord.LastUpdatedTime

			if !reflect.DeepEqual(got, &tt.expectedPullRecord) {
				t.Errorf("expected ImagePulledRecord != got; diff: %s", cmp.Diff(tt.expectedPullRecord, got))
			}
		})
	}
}

func TestFileBasedImagePullManager_initialize(t *testing.T) {
	imageService := &ctesting.FakeRuntime{
		ImageList: []container.Image{
			{
				ID:       "testimageref1",
				RepoTags: []string{"repo.repo/test/test:docker", "docker.io/testing/test:something"},
			},
			{
				ID:          "testimageref2",
				RepoTags:    []string{"repo.repo/test/test:v2", "repo.repo/test/test:test2"},
				RepoDigests: []string{"repo.repo/test/test@dgst2"},
			},
			{
				ID:          "testimageref",
				RepoTags:    []string{"repo.repo/test/test:v1", "repo.repo/test/test:test1"},
				RepoDigests: []string{"repo.repo/test/test@dgst1"},
			},
			{
				ID:       "testimageref3",
				RepoTags: []string{"repo.repo/test/test:v3", "repo.repo/test/test:test3"},
			},
			{
				ID:          "testimageref4",
				RepoDigests: []string{"repo.repo/test/test@dgst4", "repo.repo/test/notatest@dgst44"},
			},
		},
	}

	tests := []struct {
		name                  string
		existingIntents       []string
		existingPulledRecords []string
		expectedIntents       sets.Set[string]
		expectedPulled        sets.Set[string]
	}{
		{
			name: "no pulling/pulled records",
		},
		{
			name: "only pulled records",
			existingPulledRecords: []string{
				"sha256-a2eace2182b24cdbbb730798e47b10709b9ef5e0f0c1624a3bc06c8ca987727a",
				"sha256-b3c0cc4278800b03a308ceb2611161430df571ca733122f0a40ac8b9792a9064",
				"sha256-f8778b6393eaf39315e767a58cbeacf2c4b270d94b4d6926ee993d9e49444991",
			},
			expectedPulled: sets.New(
				"sha256-a2eace2182b24cdbbb730798e47b10709b9ef5e0f0c1624a3bc06c8ca987727a",
				"sha256-b3c0cc4278800b03a308ceb2611161430df571ca733122f0a40ac8b9792a9064",
				"sha256-f8778b6393eaf39315e767a58cbeacf2c4b270d94b4d6926ee993d9e49444991"),
		},
		{
			name: "pulling intent that matches an existing image - no matching pulled record",
			existingPulledRecords: []string{
				"sha256-a2eace2182b24cdbbb730798e47b10709b9ef5e0f0c1624a3bc06c8ca987727a",
				"sha256-b3c0cc4278800b03a308ceb2611161430df571ca733122f0a40ac8b9792a9064",
				"sha256-f8778b6393eaf39315e767a58cbeacf2c4b270d94b4d6926ee993d9e49444991",
			},
			existingIntents: []string{
				"sha256-f24acc752be18b93b0504c86312bbaf482c9efb0c45e925bbccb0a591cebd7af",
			},
			expectedPulled: sets.New(
				"sha256-a2eace2182b24cdbbb730798e47b10709b9ef5e0f0c1624a3bc06c8ca987727a",
				"sha256-b3c0cc4278800b03a308ceb2611161430df571ca733122f0a40ac8b9792a9064",
				"sha256-f8778b6393eaf39315e767a58cbeacf2c4b270d94b4d6926ee993d9e49444991",
				"sha256-d77ed7480bc819274ea7a4dba5b2699b2d3f73c6e578762df42e5a8224771096",
			),
		},
		{
			name: "pulling intent that matches an existing image - a pull record matches",
			existingPulledRecords: []string{
				"sha256-a2eace2182b24cdbbb730798e47b10709b9ef5e0f0c1624a3bc06c8ca987727a",
				"sha256-b3c0cc4278800b03a308ceb2611161430df571ca733122f0a40ac8b9792a9064",
				"sha256-f8778b6393eaf39315e767a58cbeacf2c4b270d94b4d6926ee993d9e49444991",
			},
			existingIntents: []string{
				"sha256-ee81caca15454863449fb55a1d942904d56d5ed9f9b20a7cb3453944ea2c7e11",
			},
			expectedPulled: sets.New(
				"sha256-a2eace2182b24cdbbb730798e47b10709b9ef5e0f0c1624a3bc06c8ca987727a",
				"sha256-b3c0cc4278800b03a308ceb2611161430df571ca733122f0a40ac8b9792a9064",
				"sha256-f8778b6393eaf39315e767a58cbeacf2c4b270d94b4d6926ee993d9e49444991",
			),
		},
		{
			name: "multiple pulling intents that match existing images",
			existingPulledRecords: []string{
				"sha256-a2eace2182b24cdbbb730798e47b10709b9ef5e0f0c1624a3bc06c8ca987727a",
				"sha256-b3c0cc4278800b03a308ceb2611161430df571ca733122f0a40ac8b9792a9064",
				"sha256-f8778b6393eaf39315e767a58cbeacf2c4b270d94b4d6926ee993d9e49444991",
			},
			existingIntents: []string{
				"sha256-ee81caca15454863449fb55a1d942904d56d5ed9f9b20a7cb3453944ea2c7e11",
				"sha256-f24acc752be18b93b0504c86312bbaf482c9efb0c45e925bbccb0a591cebd7af",
			},
			expectedPulled: sets.New(
				"sha256-a2eace2182b24cdbbb730798e47b10709b9ef5e0f0c1624a3bc06c8ca987727a",
				"sha256-b3c0cc4278800b03a308ceb2611161430df571ca733122f0a40ac8b9792a9064",
				"sha256-f8778b6393eaf39315e767a58cbeacf2c4b270d94b4d6926ee993d9e49444991",
				"sha256-d77ed7480bc819274ea7a4dba5b2699b2d3f73c6e578762df42e5a8224771096",
			),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			testCtx := ktesting.Init(t)

			encoder, decoder, err := createKubeletConfigSchemeEncoderDecoder()
			require.NoError(t, err)

			testDir := t.TempDir()
			pullingDir := filepath.Join(testDir, "pulling")
			pulledDir := filepath.Join(testDir, "pulled")

			if err := os.MkdirAll(pullingDir, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(pulledDir, 0700); err != nil {
				t.Fatal(err)
			}

			copyTestData(t, pullingDir, "pulling", tt.existingIntents)
			copyTestData(t, pulledDir, "pulled", tt.existingPulledRecords)

			fsRecordAccessor := &fsPullRecordsAccessor{
				pullingDir: pullingDir,
				pulledDir:  pulledDir,
				encoder:    encoder,
				decoder:    decoder,
			}

			f := &PullManager{
				recordsAccessor: fsRecordAccessor,
				imageService:    imageService,
				intentAccessors: NewNamedLockSet(),
				intentCounters:  make(map[string]int),
				pulledAccessors: NewNamedLockSet(),
			}
			f.initialize(testCtx)

			gotIntents := sets.New[string]()

			if err := processDirFiles(pullingDir, func(filePath string, fileContent []byte) error {
				gotIntents.Insert(filepath.Base(filePath))
				return nil
			}); err != nil {
				t.Fatalf("there was an error processing file in the test output pulling dir: %v", err)
			}

			gotPulled := sets.New[string]()
			if err := processDirFiles(pulledDir, func(filePath string, fileContent []byte) error {
				gotPulled.Insert(filepath.Base(filePath))
				return nil
			}); err != nil {
				t.Fatalf("there was an error processing file in the test output pulled dir: %v", err)
			}

			if !gotIntents.Equal(tt.expectedIntents) {
				t.Errorf("difference between expected and received pull intent files: %v", cmp.Diff(tt.expectedIntents, gotIntents))
			}

			if !gotPulled.Equal(tt.expectedPulled) {
				t.Errorf("difference between expected and received pull record files: %v", cmp.Diff(tt.expectedPulled, gotPulled))
			}
		})
	}
}

func TestFileBasedImagePullManager_PruneUnknownRecords(t *testing.T) {
	tests := []struct {
		name        string
		imageList   []string
		gcStartTime time.Time
		pulledFiles []string
		wantFiles   sets.Set[string]
	}{
		{
			name:        "all images present",
			imageList:   []string{"testimage-anonpull", "testimageref", "testemptycredmapping"},
			gcStartTime: time.Date(2024, 12, 25, 00, 01, 00, 00, time.UTC),
			pulledFiles: []string{
				"sha256-a2eace2182b24cdbbb730798e47b10709b9ef5e0f0c1624a3bc06c8ca987727a",
				"sha256-b3c0cc4278800b03a308ceb2611161430df571ca733122f0a40ac8b9792a9064",
				"sha256-f8778b6393eaf39315e767a58cbeacf2c4b270d94b4d6926ee993d9e49444991",
			},
			wantFiles: sets.New(
				"sha256-a2eace2182b24cdbbb730798e47b10709b9ef5e0f0c1624a3bc06c8ca987727a",
				"sha256-b3c0cc4278800b03a308ceb2611161430df571ca733122f0a40ac8b9792a9064",
				"sha256-f8778b6393eaf39315e767a58cbeacf2c4b270d94b4d6926ee993d9e49444991",
			),
		},
		{
			name:        "remove all records on empty list from the GC",
			imageList:   []string{},
			gcStartTime: time.Date(2024, 12, 25, 00, 01, 00, 00, time.UTC),
			pulledFiles: []string{
				"sha256-a2eace2182b24cdbbb730798e47b10709b9ef5e0f0c1624a3bc06c8ca987727a",
				"sha256-b3c0cc4278800b03a308ceb2611161430df571ca733122f0a40ac8b9792a9064",
				"sha256-f8778b6393eaf39315e767a58cbeacf2c4b270d94b4d6926ee993d9e49444991",
			},
		},
		{
			name:        "remove all records on list of untracked images from the GC",
			imageList:   []string{"untracked1", "different-untracked"},
			gcStartTime: time.Date(2024, 12, 25, 00, 01, 00, 00, time.UTC),
			pulledFiles: []string{
				"sha256-a2eace2182b24cdbbb730798e47b10709b9ef5e0f0c1624a3bc06c8ca987727a",
				"sha256-b3c0cc4278800b03a308ceb2611161430df571ca733122f0a40ac8b9792a9064",
				"sha256-f8778b6393eaf39315e767a58cbeacf2c4b270d94b4d6926ee993d9e49444991",
			},
		},
		{
			name:        "remove records without a match in the image list from the GC",
			imageList:   []string{"testimage-anonpull", "untracked1", "testimageref", "different-untracked"},
			gcStartTime: time.Date(2024, 12, 25, 00, 01, 00, 00, time.UTC),
			pulledFiles: []string{
				"sha256-a2eace2182b24cdbbb730798e47b10709b9ef5e0f0c1624a3bc06c8ca987727a",
				"sha256-b3c0cc4278800b03a308ceb2611161430df571ca733122f0a40ac8b9792a9064",
				"sha256-f8778b6393eaf39315e767a58cbeacf2c4b270d94b4d6926ee993d9e49444991",
			},
			wantFiles: sets.New(
				"sha256-a2eace2182b24cdbbb730798e47b10709b9ef5e0f0c1624a3bc06c8ca987727a",
				"sha256-b3c0cc4278800b03a308ceb2611161430df571ca733122f0a40ac8b9792a9064",
			),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			encoder, decoder, err := createKubeletConfigSchemeEncoderDecoder()
			require.NoError(t, err)

			testDir := t.TempDir()
			pulledDir := filepath.Join(testDir, "pulled")
			if err := os.MkdirAll(pulledDir, 0700); err != nil {
				t.Fatalf("failed to create testing dir %q: %v", pulledDir, err)
			}

			copyTestData(t, pulledDir, "pulled", tt.pulledFiles)

			fsRecordAccessor := &fsPullRecordsAccessor{
				pulledDir: pulledDir,
				encoder:   encoder,
				decoder:   decoder,
			}

			f := &PullManager{
				recordsAccessor: fsRecordAccessor,
				pulledAccessors: NewNamedLockSet(),
			}
			f.PruneUnknownRecords(tt.imageList, tt.gcStartTime)

			filesLeft := sets.New[string]()
			err = filepath.Walk(pulledDir, func(path string, info fs.FileInfo, err error) error {
				if err != nil {
					return err
				}

				if path == pulledDir {
					return nil
				}

				filesLeft.Insert(info.Name())
				return nil
			})
			if err != nil {
				t.Fatalf("failed to walk the pull dir after prune: %v", err)
			}

			if !tt.wantFiles.Equal(filesLeft) {
				t.Errorf("expected equal sets, diff: %s", cmp.Diff(tt.wantFiles, filesLeft))
			}
		})
	}
}

func copyTestData(t *testing.T, dstDir string, testdataDir string, src []string) {
	for _, f := range src {
		testBytes, err := os.ReadFile(filepath.Join("testdata", testdataDir, f))
		if err != nil {
			t.Fatalf("failed to read test data: %v", err)
		}
		if err := writeFile(dstDir, f, testBytes); err != nil {
			t.Fatalf("failed to write test data: %v", err)
		}
	}
}

func withImageRecord(r *kubeletconfiginternal.ImagePulledRecord, image string, record kubeletconfiginternal.ImagePullCredentials) *kubeletconfiginternal.ImagePulledRecord {
	r.CredentialMapping[image] = record
	return r
}
