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

package node

import (
	"context"

	"github.com/onsi/ginkgo/v2"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/uuid"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/kubernetes/test/e2e/feature"
	"k8s.io/kubernetes/test/e2e/framework"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	imageutils "k8s.io/kubernetes/test/utils/image"
	admissionapi "k8s.io/pod-security-admission/api"
)

var _ = SIGDescribe("ImageCredentialProvider", feature.KubeletCredentialProviders, func() {
	f := framework.NewDefaultFramework("image-credential-provider")
	f.NamespacePodSecurityLevel = admissionapi.LevelPrivileged
	var serviceAccountClient typedcorev1.ServiceAccountInterface
	var podClient *e2epod.PodClient

	ginkgo.BeforeEach(func() {
		podClient = e2epod.NewPodClient(f)
		serviceAccountClient = f.ClientSet.CoreV1().ServiceAccounts(f.Namespace.Name)
	})

	/*
		Release: v1.24
		Testname: Test kubelet image pull with external credential provider plugins
		Description: Create Pod with an image from a private registry. This test assumes that the kubelet credential provider plugin is enabled for the registry hosting imageutils.AgnhostPrivate.
	*/
	ginkgo.It("should be able to create pod with image credentials fetched from external credential provider ", func(ctx context.Context) {
		privateimage := imageutils.GetConfig(imageutils.AgnhostPrivate)
		name := "pod-auth-image-" + string(uuid.NewUUID())

		// The service account is required to exist for the credential provider plugin that's configured to use service account token.
		serviceAccount := &v1.ServiceAccount{
			ObjectMeta: metav1.ObjectMeta{
				Name: "test-service-account",
				// these annotations are validated by the test gcp-credential-provider-with-sa plugin
				// that runs in service account token mode.
				Annotations: map[string]string{
					"domain.io/identity-id":   "123456",
					"domain.io/identity-type": "serviceaccount",
				},
			},
		}
		_, err := serviceAccountClient.Create(ctx, serviceAccount, metav1.CreateOptions{})
		framework.ExpectNoError(err)

		pod := &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: name,
			},
			Spec: v1.PodSpec{
				ServiceAccountName: "test-service-account",
				Containers: []v1.Container{
					{
						Name:            "container-auth-image",
						Image:           privateimage.GetE2EImage(),
						ImagePullPolicy: v1.PullAlways,
					},
				},
			},
		}

		// CreateSync tests that the Pod is running and ready
		podClient.CreateSync(ctx, pod)
	})
})
