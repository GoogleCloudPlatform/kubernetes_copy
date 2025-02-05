/*
Copyright 2018 The Kubernetes Authors.

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

package drivers

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/uuid"

	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/test/e2e/framework"
)

func shredFile(filePath string) {
	if _, err := os.Stat(filePath); os.IsNotExist(err) {
		framework.Logf("File %v was not found, skipping shredding", filePath)
		return
	}
	framework.Logf("Shredding file %v", filePath)
	_, _, err := framework.RunCmd("shred", "--remove", filePath)
	if err != nil {
		framework.Logf("Failed to shred file %v: %v", filePath, err)
	}
	if _, err := os.Stat(filePath); os.IsNotExist(err) {
		framework.Logf("File %v successfully shredded", filePath)
		return
	}
	// Shred failed Try to remove the file for good measure
	err = os.Remove(filePath)
	framework.ExpectNoError(err, "Failed to remove service account file %s", filePath)

}

// createGCESecrets downloads the GCP IAM Key for the default compute service account
// and puts it in a secret for the GCE PD CSI Driver to consume
func createGCESecrets(client clientset.Interface, ns string) {
	saEnv := "E2E_GOOGLE_APPLICATION_CREDENTIALS"
	saFile := fmt.Sprintf("/tmp/%s/cloud-sa.json", string(uuid.NewUUID()))

	os.MkdirAll(path.Dir(saFile), 0750)
	defer os.Remove(path.Dir(saFile))

	premadeSAFile, ok := os.LookupEnv(saEnv)
	if !ok {
		framework.Logf("Could not find env var %v, please either create cloud-sa"+
			" secret manually or rerun test after setting %v to the filepath of"+
			" the GCP Service Account to give to the GCE Persistent Disk CSI Driver", saEnv, saEnv)
		return
	}

	framework.Logf("Found CI service account key at %v", premadeSAFile)
	// Need to copy it saFile
	stdout, stderr, err := framework.RunCmd("cp", premadeSAFile, saFile)
	framework.ExpectNoError(err, "error copying service account key: %s\nstdout: %s\nstderr: %s", err, stdout, stderr)
	defer shredFile(saFile)
	// Create Secret with this Service Account
	fileBytes, err := os.ReadFile(saFile)
	framework.ExpectNoError(err, "Failed to read file %v", saFile)

	s := &v1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cloud-sa",
			Namespace: ns,
		},
		Type: v1.SecretTypeOpaque,
		Data: map[string][]byte{
			filepath.Base(saFile): fileBytes,
		},
	}

	_, err = client.CoreV1().Secrets(ns).Create(context.TODO(), s, metav1.CreateOptions{})
	if !apierrors.IsAlreadyExists(err) {
		framework.ExpectNoError(err, "Failed to create Secret %v", s.GetName())
	}
}
