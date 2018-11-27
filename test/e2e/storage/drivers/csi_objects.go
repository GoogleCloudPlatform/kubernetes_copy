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

// This file is used to deploy the CSI hostPath plugin
// More Information: https://github.com/kubernetes-csi/drivers/tree/master/pkg/hostpath

package drivers

import (
	"flag"
	"fmt"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"
	"time"

	"k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/apimachinery/pkg/util/wait"

	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/test/e2e/framework"
)

const (
	dsRetryPeriod  = 1 * time.Second
	dsRetryTimeout = 5 * time.Minute
)

var (
	csiImageVersion  = flag.String("storage.csi.image.version", "", "overrides the default tag used for hostpathplugin/csi-attacher/csi-provisioner/driver-registrar images")
	csiImageRegistry = flag.String("storage.csi.image.registry", "quay.io/k8scsi", "overrides the default repository used for hostpathplugin/csi-attacher/csi-provisioner/driver-registrar images")
	csiImageVersions = map[string]string{
		"hostpathplugin":   "v0.4.0",
		"csi-attacher":     "v0.4.0",
		"csi-provisioner":  "v0.4.0",
		"driver-registrar": "v0.4.0",
	}
	driverReadyString = "PluginRegistered:true"
)

func csiContainerImage(image string) string {
	var fullName string
	fullName += *csiImageRegistry + "/" + image + ":"
	if *csiImageVersion != "" {
		fullName += *csiImageVersion
	} else {
		fullName += csiImageVersions[image]
	}
	return fullName
}

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
	// Shred failed Try to remove the file for good meausure
	err = os.Remove(filePath)
	framework.ExpectNoError(err, "Failed to remove service account file %s", filePath)

}

// createGCESecrets downloads the GCP IAM Key for the default compute service account
// and puts it in a secret for the GCE PD CSI Driver to consume
func createGCESecrets(client clientset.Interface, config framework.VolumeTestConfig) {
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
	fileBytes, err := ioutil.ReadFile(saFile)
	framework.ExpectNoError(err, "Failed to read file %v", saFile)

	s := &v1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cloud-sa",
			Namespace: config.Namespace,
		},
		Type: v1.SecretTypeOpaque,
		Data: map[string][]byte{
			filepath.Base(saFile): fileBytes,
		},
	}

	_, err = client.CoreV1().Secrets(config.Namespace).Create(s)
	framework.ExpectNoError(err, "Failed to create Secret %v", s.GetName())
}

func waitForAllPodsInDaemonsetReady(f *framework.Framework, namespace string, dsName string) {
	err := wait.PollImmediate(dsRetryPeriod, dsRetryTimeout, checkDaemonsetDesiredState(f, namespace, dsName))
	framework.ExpectNoError(err, "daemonset %s/%s doesn't become ready: %v", namespace, dsName, err)
}

func checkDaemonsetDesiredState(f *framework.Framework, namespace string, dsName string) func() (bool, error) {
	return func() (bool, error) {
		ds, err := f.ClientSet.AppsV1().DaemonSets(namespace).Get(dsName, metav1.GetOptions{})
		if err != nil {
			return false, fmt.Errorf("Could not get daemonset %s/%s", namespace, dsName)
		}
		framework.Logf("daemonset %s/%s: desired:%d, ready:%d", namespace, dsName, ds.Status.DesiredNumberScheduled, ds.Status.NumberReady)
		return ds.Status.DesiredNumberScheduled > 0 && ds.Status.DesiredNumberScheduled == ds.Status.NumberReady, nil
	}
}

func getAllPodsInDaemonset(f *framework.Framework, namespace string, dsName string) ([]v1.Pod, error) {
	var pods []v1.Pod

	ds, err := f.ClientSet.AppsV1().DaemonSets(namespace).Get(dsName, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}

	podList, err := f.ClientSet.CoreV1().Pods(ds.Namespace).List(metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	for _, pod := range podList.Items {
		if metav1.IsControlledBy(&pod, ds) {
			pods = append(pods, pod)
		}
	}

	return pods, nil
}

func waitForDriverRegistrarReady(namespace string, podName string, container string) {
	_, err := framework.LookForStringInLog(namespace, podName, container, driverReadyString, framework.PodStartTimeout)
	framework.ExpectNoError(err, "Failed to find %q in driver registrar logs: %s", driverReadyString, err)
}

func waitForAllDriverRegistrarReady(f *framework.Framework, namespace string, dsName string, container string) {
	waitForAllPodsInDaemonsetReady(f, namespace, dsName)

	pods, err := getAllPodsInDaemonset(f, namespace, dsName)
	framework.ExpectNoError(err, "Failed to get all pods for driver registrar managed by %s/%s: %v", namespace, dsName, err)

	for _, pod := range pods {
		waitForDriverRegistrarReady(pod.Namespace, pod.Name, container)
	}
}
