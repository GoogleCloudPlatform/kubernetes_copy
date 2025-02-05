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

package kuberuntime

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
	"k8s.io/kubernetes/pkg/credentialprovider"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
)

func TestPullImage(t *testing.T) {
	ctx := context.Background()
	_, _, fakeManager, err := createTestRuntimeManager()
	assert.NoError(t, err)

	imageRef, err := fakeManager.PullImage(ctx, kubecontainer.ImageSpec{Image: "busybox"}, nil, nil, "")
	assert.NoError(t, err)
	assert.Equal(t, "busybox", imageRef)

	images, err := fakeManager.ListImages(ctx)
	assert.NoError(t, err)
	assert.Len(t, images, 1)
	assert.Equal(t, []string{"busybox"}, images[0].RepoTags)
}

func TestPullImageWithError(t *testing.T) {
	ctx := context.Background()
	_, fakeImageService, fakeManager, err := createTestRuntimeManager()
	assert.NoError(t, err)

	// trying to pull an image with an invalid name should return an error
	imageRef, err := fakeManager.PullImage(ctx, kubecontainer.ImageSpec{Image: ":invalid"}, nil, nil, "")
	assert.Error(t, err)
	assert.Equal(t, "", imageRef)

	fakeImageService.InjectError("PullImage", fmt.Errorf("test-error"))
	imageRef, err = fakeManager.PullImage(ctx, kubecontainer.ImageSpec{Image: "busybox"}, nil, nil, "")
	assert.Error(t, err)
	assert.Equal(t, "", imageRef)

	images, err := fakeManager.ListImages(ctx)
	assert.NoError(t, err)
	assert.Empty(t, images)
}

func TestPullImageWithInvalidImageName(t *testing.T) {
	_, fakeImageService, fakeManager, err := createTestRuntimeManager()
	assert.NoError(t, err)

	imageList := []string{"FAIL", "http://fail", "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"}
	fakeImageService.SetFakeImages(imageList)
	for _, val := range imageList {
		ctx := context.Background()
		imageRef, err := fakeManager.PullImage(ctx, kubecontainer.ImageSpec{Image: val}, nil, nil, "")
		assert.Error(t, err)
		assert.Equal(t, "", imageRef)

	}
}

func TestListImages(t *testing.T) {
	ctx := context.Background()
	_, fakeImageService, fakeManager, err := createTestRuntimeManager()
	assert.NoError(t, err)

	images := []string{"1111", "2222", "3333"}
	expected := sets.New[string](images...)
	fakeImageService.SetFakeImages(images)

	actualImages, err := fakeManager.ListImages(ctx)
	assert.NoError(t, err)
	actual := sets.New[string]()
	for _, i := range actualImages {
		actual.Insert(i.ID)
	}

	assert.Equal(t, sets.List(expected), sets.List(actual))
}

func TestListImagesPinnedField(t *testing.T) {
	ctx := context.Background()
	_, fakeImageService, fakeManager, err := createTestRuntimeManager()
	assert.NoError(t, err)

	imagesPinned := map[string]bool{
		"1111": false,
		"2222": true,
		"3333": false,
	}
	imageList := []string{}
	for image, pinned := range imagesPinned {
		fakeImageService.SetFakeImagePinned(image, pinned)
		imageList = append(imageList, image)
	}
	fakeImageService.SetFakeImages(imageList)

	actualImages, err := fakeManager.ListImages(ctx)
	assert.NoError(t, err)
	for _, image := range actualImages {
		assert.Equal(t, imagesPinned[image.ID], image.Pinned)
	}
}

func TestListImagesWithError(t *testing.T) {
	ctx := context.Background()
	_, fakeImageService, fakeManager, err := createTestRuntimeManager()
	assert.NoError(t, err)

	fakeImageService.InjectError("ListImages", fmt.Errorf("test-failure"))

	actualImages, err := fakeManager.ListImages(ctx)
	assert.Error(t, err)
	assert.Nil(t, actualImages)
}

func TestGetImageRef(t *testing.T) {
	ctx := context.Background()
	_, fakeImageService, fakeManager, err := createTestRuntimeManager()
	assert.NoError(t, err)

	image := "busybox"
	fakeImageService.SetFakeImages([]string{image})
	imageRef, err := fakeManager.GetImageRef(ctx, kubecontainer.ImageSpec{Image: image})
	assert.NoError(t, err)
	assert.Equal(t, image, imageRef)
}

func TestImageSize(t *testing.T) {
	ctx := context.Background()
	_, fakeImageService, fakeManager, err := createTestRuntimeManager()
	assert.NoError(t, err)

	const imageSize = uint64(64)
	fakeImageService.SetFakeImageSize(imageSize)
	image := "busybox"
	fakeImageService.SetFakeImages([]string{image})
	actualSize, err := fakeManager.GetImageSize(ctx, kubecontainer.ImageSpec{Image: image})
	assert.NoError(t, err)
	assert.Equal(t, imageSize, actualSize)
}

func TestGetImageRefImageNotAvailableLocally(t *testing.T) {
	ctx := context.Background()
	_, _, fakeManager, err := createTestRuntimeManager()
	assert.NoError(t, err)

	image := "busybox"

	imageRef, err := fakeManager.GetImageRef(ctx, kubecontainer.ImageSpec{Image: image})
	assert.NoError(t, err)

	imageNotAvailableLocallyRef := ""
	assert.Equal(t, imageNotAvailableLocallyRef, imageRef)
}

func TestGetImageRefWithError(t *testing.T) {
	ctx := context.Background()
	_, fakeImageService, fakeManager, err := createTestRuntimeManager()
	assert.NoError(t, err)

	image := "busybox"

	fakeImageService.InjectError("ImageStatus", fmt.Errorf("test-error"))

	imageRef, err := fakeManager.GetImageRef(ctx, kubecontainer.ImageSpec{Image: image})
	assert.Error(t, err)
	assert.Equal(t, "", imageRef)
}

func TestRemoveImage(t *testing.T) {
	ctx := context.Background()
	_, fakeImageService, fakeManager, err := createTestRuntimeManager()
	assert.NoError(t, err)

	_, err = fakeManager.PullImage(ctx, kubecontainer.ImageSpec{Image: "busybox"}, nil, nil, "")
	assert.NoError(t, err)
	assert.Len(t, fakeImageService.Images, 1)

	err = fakeManager.RemoveImage(ctx, kubecontainer.ImageSpec{Image: "busybox"})
	assert.NoError(t, err)
	assert.Empty(t, fakeImageService.Images)
}

func TestRemoveImageNoOpIfImageNotLocal(t *testing.T) {
	ctx := context.Background()
	_, _, fakeManager, err := createTestRuntimeManager()
	assert.NoError(t, err)

	err = fakeManager.RemoveImage(ctx, kubecontainer.ImageSpec{Image: "busybox"})
	assert.NoError(t, err)
}

func TestRemoveImageWithError(t *testing.T) {
	ctx := context.Background()
	_, fakeImageService, fakeManager, err := createTestRuntimeManager()
	assert.NoError(t, err)

	_, err = fakeManager.PullImage(ctx, kubecontainer.ImageSpec{Image: "busybox"}, nil, nil, "")
	assert.NoError(t, err)
	assert.Len(t, fakeImageService.Images, 1)

	fakeImageService.InjectError("RemoveImage", fmt.Errorf("test-failure"))

	err = fakeManager.RemoveImage(ctx, kubecontainer.ImageSpec{Image: "busybox"})
	assert.Error(t, err)
	assert.Len(t, fakeImageService.Images, 1)
}

func TestImageStats(t *testing.T) {
	ctx := context.Background()
	_, fakeImageService, fakeManager, err := createTestRuntimeManager()
	assert.NoError(t, err)

	const imageSize = 64
	fakeImageService.SetFakeImageSize(imageSize)
	images := []string{"1111", "2222", "3333"}
	fakeImageService.SetFakeImages(images)

	actualStats, err := fakeManager.ImageStats(ctx)
	assert.NoError(t, err)
	expectedStats := &kubecontainer.ImageStats{TotalStorageBytes: imageSize * uint64(len(images))}
	assert.Equal(t, expectedStats, actualStats)
}

func TestImageStatsWithError(t *testing.T) {
	ctx := context.Background()
	_, fakeImageService, fakeManager, err := createTestRuntimeManager()
	assert.NoError(t, err)

	fakeImageService.InjectError("ListImages", fmt.Errorf("test-failure"))

	actualImageStats, err := fakeManager.ImageStats(ctx)
	assert.Error(t, err)
	assert.Nil(t, actualImageStats)
}

func TestPullWithSecrets(t *testing.T) {
	ctx := context.Background()
	// auth value is equivalent to: "username":"passed-user","password":"passed-password"
	dockerCfg := map[string]map[string]string{"index.docker.io/v1/": {"email": "passed-email", "auth": "cGFzc2VkLXVzZXI6cGFzc2VkLXBhc3N3b3Jk"}}
	dockercfgContent, err := json.Marshal(dockerCfg)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	dockerConfigJSON := map[string]map[string]map[string]string{"auths": dockerCfg}
	dockerConfigJSONContent, err := json.Marshal(dockerConfigJSON)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	tests := map[string]struct {
		imageName           string
		passedSecrets       []v1.Secret
		builtInDockerConfig credentialprovider.DockerConfig
		expectedAuth        *runtimeapi.AuthConfig
	}{
		"no matching secrets": {
			"ubuntu",
			[]v1.Secret{},
			credentialprovider.DockerConfig(map[string]credentialprovider.DockerConfigEntry{}),
			nil,
		},
		"default keyring secrets": {
			"ubuntu",
			[]v1.Secret{},
			credentialprovider.DockerConfig(map[string]credentialprovider.DockerConfigEntry{
				"index.docker.io/v1/": {Username: "built-in", Password: "password", Provider: nil},
			}),
			&runtimeapi.AuthConfig{Username: "built-in", Password: "password"},
		},
		"default keyring secrets unused": {
			"ubuntu",
			[]v1.Secret{},
			credentialprovider.DockerConfig(map[string]credentialprovider.DockerConfigEntry{
				"extraneous": {Username: "built-in", Password: "password", Provider: nil},
			}),
			nil,
		},
		"builtin keyring secrets, but use passed": {
			"ubuntu",
			[]v1.Secret{{Type: v1.SecretTypeDockercfg, Data: map[string][]byte{v1.DockerConfigKey: dockercfgContent}}},
			credentialprovider.DockerConfig(map[string]credentialprovider.DockerConfigEntry{
				"index.docker.io/v1/": {Username: "built-in", Password: "password", Provider: nil},
			}),
			&runtimeapi.AuthConfig{Username: "passed-user", Password: "passed-password"},
		},
		"builtin keyring secrets, but use passed with new docker config": {
			"ubuntu",
			[]v1.Secret{{Type: v1.SecretTypeDockerConfigJson, Data: map[string][]byte{v1.DockerConfigJsonKey: dockerConfigJSONContent}}},
			credentialprovider.DockerConfig(map[string]credentialprovider.DockerConfigEntry{
				"index.docker.io/v1/": {Username: "built-in", Password: "password", Provider: nil},
			}),
			&runtimeapi.AuthConfig{Username: "passed-user", Password: "passed-password"},
		},
	}
	for description, test := range tests {
		builtInKeyRing := &credentialprovider.BasicDockerKeyring{}
		builtInKeyRing.Add(test.builtInDockerConfig)
		_, fakeImageService, fakeManager, err := customTestRuntimeManager(builtInKeyRing)
		require.NoError(t, err)

		_, err = fakeManager.PullImage(ctx, kubecontainer.ImageSpec{Image: test.imageName}, test.passedSecrets, nil, "")
		require.NoError(t, err)
		fakeImageService.AssertImagePulledWithAuth(t, &runtimeapi.ImageSpec{Image: test.imageName, Annotations: make(map[string]string)}, test.expectedAuth, description)
	}
}

func TestPullWithSecretsWithError(t *testing.T) {
	ctx := context.Background()

	dockerCfg := map[string]map[string]map[string]string{
		"auths": {
			"index.docker.io/v1/": {
				"email": "passed-email",
				"auth":  "cGFzc2VkLXVzZXI6cGFzc2VkLXBhc3N3b3Jk",
			},
		},
	}

	dockerConfigJSON, err := json.Marshal(dockerCfg)
	if err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name              string
		imageName         string
		passedSecrets     []v1.Secret
		shouldInjectError bool
	}{
		{
			name:          "invalid docker secret",
			imageName:     "ubuntu",
			passedSecrets: []v1.Secret{{Type: v1.SecretTypeDockercfg, Data: map[string][]byte{v1.DockerConfigKey: []byte("invalid")}}},
		},
		{
			name:      "secret provided, pull failed",
			imageName: "ubuntu",
			passedSecrets: []v1.Secret{
				{Type: v1.SecretTypeDockerConfigJson, Data: map[string][]byte{v1.DockerConfigKey: dockerConfigJSON}},
			},
			shouldInjectError: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, fakeImageService, fakeManager, err := createTestRuntimeManager()
			assert.NoError(t, err)

			if test.shouldInjectError {
				fakeImageService.InjectError("PullImage", fmt.Errorf("test-error"))
			}

			imageRef, err := fakeManager.PullImage(ctx, kubecontainer.ImageSpec{Image: test.imageName}, test.passedSecrets, nil, "")
			assert.Error(t, err)
			assert.Equal(t, "", imageRef)

			images, err := fakeManager.ListImages(ctx)
			assert.NoError(t, err)
			assert.Empty(t, images)
		})
	}
}

func TestPullThenListWithAnnotations(t *testing.T) {
	ctx := context.Background()
	_, _, fakeManager, err := createTestRuntimeManager()
	assert.NoError(t, err)

	imageSpec := kubecontainer.ImageSpec{
		Image: "12345",
		Annotations: []kubecontainer.Annotation{
			{Name: "kubernetes.io/runtimehandler", Value: "handler_name"},
		},
	}

	_, err = fakeManager.PullImage(ctx, imageSpec, nil, nil, "")
	assert.NoError(t, err)

	images, err := fakeManager.ListImages(ctx)
	assert.NoError(t, err)
	assert.Len(t, images, 1)
	assert.Equal(t, images[0].Spec, imageSpec)
}
