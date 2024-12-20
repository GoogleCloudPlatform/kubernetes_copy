/*
Copyright 2017 The Kubernetes Authors.

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

package transformation

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"

	"sigs.k8s.io/yaml"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	apiserverv1 "k8s.io/apiserver/pkg/apis/apiserver/v1"
	"k8s.io/apiserver/pkg/storage/storagebackend"
	"k8s.io/apiserver/pkg/storage/value"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/component-base/metrics/legacyregistry"
	"k8s.io/klog/v2"
	kubeapiservertesting "k8s.io/kubernetes/cmd/kube-apiserver/app/testing"
	"k8s.io/kubernetes/test/integration"
	"k8s.io/kubernetes/test/integration/etcd"
	"k8s.io/kubernetes/test/integration/framework"
	"k8s.io/kubernetes/test/utils/ktesting"
	"k8s.io/utils/pointer"
)

const (
	secretKey                = "api_key"
	secretVal                = "086a7ffc-0225-11e8-ba89-0ed5f89f718b" // Fake value for testing.
	encryptionConfigFileName = "encryption.conf"
	testNamespace            = "secret-encryption-test"
	testSecret               = "test-secret"
	testConfigmap            = "test-configmap"
	metricsPrefix            = "apiserver_storage_"
	configMapKey             = "foo"
	configMapVal             = "bar"

	// precomputed key and secret for use with AES CBC
	// this looks exactly the same as the AES GCM secret but with a different value
	oldAESCBCKey = "e0/+tts8FS254BZimFZWtUsOCOUDSkvzB72PyimMlkY="
	oldSecret    = "azhzAAoMCgJ2MRIGU2VjcmV0En4KXwoLdGVzdC1zZWNyZXQSABoWc2VjcmV0LWVuY3J5cHRpb24tdGVzdCIAKiQ3MmRmZTVjNC0xNDU2LTQyMzktYjFlZC1hZGZmYTJmMWY3YmEyADgAQggI5Jy/7wUQAHoAEhMKB2FwaV9rZXkSCPCfpJfwn5C8GgZPcGFxdWUaACIA"
	oldSecretVal = "\xf0\x9f\xa4\x97\xf0\x9f\x90\xbc"
)

type unSealSecret func(ctx context.Context, cipherText []byte, dataCtx value.Context, config apiserverv1.ProviderConfiguration) ([]byte, error)

type transformTest struct {
	ktesting.TContext
	storageConfig     *storagebackend.Config
	configDir         string
	transformerConfig string
	kubeAPIServer     kubeapiservertesting.TestServer
	restClient        *kubernetes.Clientset
	ns                *corev1.Namespace
	secret            *corev1.Secret
}

func newTransformTest(tb testing.TB, transformerConfigYAML string, reload bool, configDir string, storageConfig *storagebackend.Config) (*transformTest, error) {
	tCtx := ktesting.Init(tb)
	if storageConfig == nil {
		storageConfig = framework.SharedEtcd()
	}
	e := transformTest{
		TContext:          tCtx,
		transformerConfig: transformerConfigYAML,
		storageConfig:     storageConfig,
	}

	var err error
	// create config dir with provided config yaml
	if transformerConfigYAML != "" && configDir == "" {
		if e.configDir, err = e.createEncryptionConfig(); err != nil {
			e.cleanUp()
			return nil, fmt.Errorf("error while creating KubeAPIServer encryption config: %w", err)
		}
	} else {
		// configDir already exists. api-server must be restarting with existing encryption config
		e.configDir = configDir
	}
	configFile := filepath.Join(e.configDir, encryptionConfigFileName)
	_, err = os.ReadFile(configFile)
	if err != nil {
		e.cleanUp()
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	if e.kubeAPIServer, err = kubeapiservertesting.StartTestServer(
		tb, nil,
		e.getEncryptionOptions(reload), e.storageConfig); err != nil {
		e.cleanUp()
		return nil, fmt.Errorf("failed to start KubeAPI server: %w", err)
	}
	klog.Infof("Started kube-apiserver %v", e.kubeAPIServer.ClientConfig.Host)

	if e.restClient, err = kubernetes.NewForConfig(e.kubeAPIServer.ClientConfig); err != nil {
		e.cleanUp()
		return nil, fmt.Errorf("error while creating rest client: %w", err)
	}

	if e.ns, err = e.createNamespace(testNamespace); err != nil {
		e.cleanUp()
		return nil, err
	}

	if transformerConfigYAML != "" && reload {
		// when reloading is enabled, this healthz endpoint is always present
		mustBeHealthy(tCtx, "/kms-providers", "ok", e.kubeAPIServer.ClientConfig)
		mustNotHaveLivez(tCtx, "/kms-providers", "404 page not found", e.kubeAPIServer.ClientConfig)

		// excluding healthz endpoints even if they do not exist should work
		mustBeHealthy(tCtx, "", `warn: some health checks cannot be excluded: no matches for "kms-provider-0","kms-provider-1","kms-provider-2","kms-provider-3"`,
			e.kubeAPIServer.ClientConfig, "kms-provider-0", "kms-provider-1", "kms-provider-2", "kms-provider-3")
	}

	return &e, nil
}

func (e *transformTest) cleanUp() {
	if e.configDir != "" {
		os.RemoveAll(e.configDir)
	}

	if e.kubeAPIServer.ClientConfig != nil {
		e.shutdownAPIServer()
	}
}

func (e *transformTest) shutdownAPIServer() {
	e.kubeAPIServer.TearDownFn()
}

func (e *transformTest) runResource(l kubeapiservertesting.Logger, unSealSecretFunc unSealSecret, expectedEnvelopePrefix,
	group,
	version,
	resource,
	name,
	namespaceName string,
) {
	response, err := e.readRawRecordFromETCD(e.getETCDPathForResource(e.storageConfig.Prefix, group, resource, name, namespaceName))
	if err != nil {
		l.Errorf("failed to read from etcd: %v", err)
		return
	}

	if !bytes.HasPrefix(response.Kvs[0].Value, []byte(expectedEnvelopePrefix)) {
		l.Errorf("expected data to be prefixed with %s, but got %s",
			expectedEnvelopePrefix, response.Kvs[0].Value)
		return
	}

	// etcd path of the key is used as the authenticated context - need to pass it to decrypt
	ctx := context.Background()
	dataCtx := value.DefaultContext(e.getETCDPathForResource(e.storageConfig.Prefix, group, resource, name, namespaceName))
	// Envelope header precedes the cipherTextPayload
	sealedData := response.Kvs[0].Value[len(expectedEnvelopePrefix):]
	transformerConfig, err := e.getEncryptionConfig()
	if err != nil {
		l.Errorf("failed to parse transformer config: %v", err)
	}
	v, err := unSealSecretFunc(ctx, sealedData, dataCtx, *transformerConfig)
	if err != nil {
		l.Errorf("failed to unseal secret: %v", err)
		return
	}
	if resource == "secrets" {
		if !strings.Contains(string(v), secretVal) {
			l.Errorf("expected %q after decryption, but got %q", secretVal, string(v))
		}
	} else if resource == "configmaps" {
		if !strings.Contains(string(v), configMapVal) {
			l.Errorf("expected %q after decryption, but got %q", configMapVal, string(v))
		}
	} else {
		if !strings.Contains(string(v), name) {
			l.Errorf("expected %q after decryption, but got %q", name, string(v))
		}
	}

	// Data should be un-enveloped on direct reads from Kube API Server.
	if resource == "secrets" {
		s, err := e.restClient.CoreV1().Secrets(testNamespace).Get(context.TODO(), name, metav1.GetOptions{})
		if err != nil {
			l.Fatalf("failed to get Secret from %s, err: %v", testNamespace, err)
		}
		if secretVal != string(s.Data[secretKey]) {
			l.Errorf("expected %s from KubeAPI, but got %s", secretVal, string(s.Data[secretKey]))
		}
	} else if resource == "configmaps" {
		s, err := e.restClient.CoreV1().ConfigMaps(namespaceName).Get(context.TODO(), name, metav1.GetOptions{})
		if err != nil {
			l.Fatalf("failed to get ConfigMap from %s, err: %v", namespaceName, err)
		}
		if configMapVal != string(s.Data[configMapKey]) {
			l.Errorf("expected %s from KubeAPI, but got %s", configMapVal, string(s.Data[configMapKey]))
		}
	} else if resource == "pods" {
		p, err := e.restClient.CoreV1().Pods(namespaceName).Get(context.TODO(), name, metav1.GetOptions{})
		if err != nil {
			l.Fatalf("failed to get Pod from %s, err: %v", namespaceName, err)
		}
		if p.Name != name {
			l.Errorf("expected %s from KubeAPI, but got %s", name, p.Name)
		}
	} else {
		l.Logf("Get object with dynamic client")
		fooResource := schema.GroupVersionResource{Group: group, Version: version, Resource: resource}
		obj, err := dynamic.NewForConfigOrDie(e.kubeAPIServer.ClientConfig).Resource(fooResource).Namespace(namespaceName).Get(context.TODO(), name, metav1.GetOptions{})
		if err != nil {
			l.Fatalf("Failed to get test instance: %v, name: %s", err, name)
		}
		if obj.GetObjectKind().GroupVersionKind().Group == group && obj.GroupVersionKind().Version == version && obj.GetKind() == resource && obj.GetNamespace() == namespaceName && obj.GetName() != name {
			l.Errorf("expected %s from KubeAPI, but got %s", name, obj.GetName())
		}
	}
}

func (e *transformTest) benchmark(b *testing.B) {
	for i := 0; i < b.N; i++ {
		_, err := e.createSecret(e.secret.Name+strconv.Itoa(i), e.ns.Name)
		if err != nil {
			b.Fatalf("failed to create a secret: %v", err)
		}
	}
}

func (e *transformTest) getETCDPathForResource(storagePrefix, group, resource, name, namespaceName string) string {
	groupResource := resource
	if group != "" {
		groupResource = fmt.Sprintf("%s/%s", group, resource)
	}
	if namespaceName == "" {
		return fmt.Sprintf("/%s/%s/%s", storagePrefix, groupResource, name)
	}
	return fmt.Sprintf("/%s/%s/%s/%s", storagePrefix, groupResource, namespaceName, name)
}

func (e *transformTest) getRawSecretFromETCD() ([]byte, error) {
	secretETCDPath := e.getETCDPathForResource(e.storageConfig.Prefix, "", "secrets", e.secret.Name, e.secret.Namespace)
	etcdResponse, err := e.readRawRecordFromETCD(secretETCDPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read %s from etcd: %v", secretETCDPath, err)
	}
	return etcdResponse.Kvs[0].Value, nil
}

func (e *transformTest) getEncryptionOptions(reload bool) []string {
	if e.transformerConfig != "" {
		return []string{
			"--encryption-provider-config", filepath.Join(e.configDir, encryptionConfigFileName),
			fmt.Sprintf("--encryption-provider-config-automatic-reload=%v", reload),
			"--disable-admission-plugins", "ServiceAccount",
			"--authorization-mode=RBAC"}
	}

	return nil
}

func (e *transformTest) createEncryptionConfig() (
	filePathForEncryptionConfig string,
	err error,
) {
	tempDir, err := os.MkdirTemp("", "secrets-encryption-test")
	if err != nil {
		return "", fmt.Errorf("failed to create temp directory: %v", err)
	}

	if err = os.WriteFile(filepath.Join(tempDir, encryptionConfigFileName), []byte(e.transformerConfig), 0644); err != nil {
		os.RemoveAll(tempDir)
		return tempDir, fmt.Errorf("error while writing encryption config: %v", err)
	}

	return tempDir, nil
}

func (e *transformTest) getEncryptionConfig() (*apiserverv1.ProviderConfiguration, error) {
	var config apiserverv1.EncryptionConfiguration
	err := yaml.Unmarshal([]byte(e.transformerConfig), &config)
	if err != nil {
		return nil, fmt.Errorf("failed to extract transformer key: %v", err)
	}

	return &config.Resources[0].Providers[0], nil
}

func (e *transformTest) createNamespace(name string) (*corev1.Namespace, error) {
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
	}

	if _, err := e.restClient.CoreV1().Namespaces().Create(context.TODO(), ns, metav1.CreateOptions{}); err != nil {
		if errors.IsAlreadyExists(err) {
			existingNs, err := e.restClient.CoreV1().Namespaces().Get(context.TODO(), name, metav1.GetOptions{})
			if err != nil {
				return nil, fmt.Errorf("unable to get testing namespace, err: [%v]", err)
			}
			return existingNs, nil
		}
		return nil, fmt.Errorf("unable to create testing namespace, err: [%v]", err)
	}

	return ns, nil
}

func (e *transformTest) createSecret(name, namespace string) (*corev1.Secret, error) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Data: map[string][]byte{
			secretKey: []byte(secretVal),
		},
	}
	if _, err := e.restClient.CoreV1().Secrets(secret.Namespace).Create(context.TODO(), secret, metav1.CreateOptions{}); err != nil {
		return nil, fmt.Errorf("error while writing secret: %v", err)
	}

	return secret, nil
}

func (e *transformTest) createConfigMap(name, namespace string) (*corev1.ConfigMap, error) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Data: map[string]string{
			configMapKey: configMapVal,
		},
	}
	if _, err := e.restClient.CoreV1().ConfigMaps(cm.Namespace).Create(context.TODO(), cm, metav1.CreateOptions{}); err != nil {
		return nil, fmt.Errorf("error while writing configmap: %v", err)
	}

	return cm, nil
}

// create jobs
func (e *transformTest) createJob(name, namespace string) (*batchv1.Job, error) {
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "test",
							Image: "test",
						},
					},
					RestartPolicy: corev1.RestartPolicyNever,
				},
			},
		},
	}
	if _, err := e.restClient.BatchV1().Jobs(job.Namespace).Create(context.TODO(), job, metav1.CreateOptions{}); err != nil {
		return nil, fmt.Errorf("error while creating job: %v", err)
	}

	return job, nil
}

// create deployment
func (e *transformTest) createDeployment(name, namespace string) (*appsv1.Deployment, error) {
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: pointer.Int32(2),
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app": "nginx",
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app": "nginx",
					},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "nginx",
							Image: "nginx:1.17",
							Ports: []corev1.ContainerPort{
								{
									Name:          "http",
									Protocol:      corev1.ProtocolTCP,
									ContainerPort: 80,
								},
							},
						},
					},
				},
			},
		},
	}
	if _, err := e.restClient.AppsV1().Deployments(deployment.Namespace).Create(context.TODO(), deployment, metav1.CreateOptions{}); err != nil {
		return nil, fmt.Errorf("error while creating deployment: %v", err)
	}

	return deployment, nil
}

func gvr(group, version, resource string) schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: group, Version: version, Resource: resource}
}

func createResource(client dynamic.Interface, gvr schema.GroupVersionResource, ns string) (*unstructured.Unstructured, error) {
	stubObj, err := getStubObj(gvr)
	if err != nil {
		return nil, err
	}
	return client.Resource(gvr).Namespace(ns).Create(context.TODO(), stubObj, metav1.CreateOptions{})
}

func inplaceUpdateResource(client dynamic.Interface, gvr schema.GroupVersionResource, ns string, obj *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	return client.Resource(gvr).Namespace(ns).Update(context.TODO(), obj, metav1.UpdateOptions{})
}

func getStubObj(gvr schema.GroupVersionResource) (*unstructured.Unstructured, error) {
	stub := ""
	if data, ok := etcd.GetEtcdStorageDataForNamespace(testNamespace)[gvr]; ok {
		stub = data.Stub
	}
	if len(stub) == 0 {
		return nil, fmt.Errorf("no stub data for %#v", gvr)
	}

	stubObj := &unstructured.Unstructured{Object: map[string]interface{}{}}
	if err := json.Unmarshal([]byte(stub), &stubObj.Object); err != nil {
		return nil, fmt.Errorf("error unmarshaling stub for %#v: %v", gvr, err)
	}
	return stubObj, nil
}

func (e *transformTest) createPod(namespace string, dynamicInterface dynamic.Interface) (*unstructured.Unstructured, error) {
	podGVR := gvr("", "v1", "pods")
	pod, err := createResource(dynamicInterface, podGVR, namespace)
	if err != nil {
		return nil, fmt.Errorf("error while writing pod: %v", err)
	}
	return pod, nil
}

func (e *transformTest) deletePod(namespace string, dynamicInterface dynamic.Interface) error {
	podGVR := gvr("", "v1", "pods")
	stubObj, err := getStubObj(podGVR)
	if err != nil {
		return err
	}
	return dynamicInterface.Resource(podGVR).Namespace(namespace).Delete(context.TODO(), stubObj.GetName(), metav1.DeleteOptions{})
}

func (e *transformTest) inplaceUpdatePod(namespace string, obj *unstructured.Unstructured, dynamicInterface dynamic.Interface) (*unstructured.Unstructured, error) {
	podGVR := gvr("", "v1", "pods")
	pod, err := inplaceUpdateResource(dynamicInterface, podGVR, namespace, obj)
	if err != nil {
		return nil, fmt.Errorf("error while writing pod: %v", err)
	}
	return pod, nil
}

func (e *transformTest) readRawRecordFromETCD(path string) (*clientv3.GetResponse, error) {
	rawClient, etcdClient, err := integration.GetEtcdClients(e.kubeAPIServer.ServerOpts.Etcd.StorageConfig.Transport)
	if err != nil {
		return nil, fmt.Errorf("failed to create etcd client: %v", err)
	}
	// kvClient is a wrapper around rawClient and to avoid leaking goroutines we need to
	// close the client (which we can do by closing rawClient).
	defer rawClient.Close()

	response, err := etcdClient.Get(context.Background(), path, clientv3.WithPrefix())
	if err != nil {
		return nil, fmt.Errorf("failed to retrieve secret from etcd %v", err)
	}

	return response, nil
}

func (e *transformTest) writeRawRecordToETCD(path string, data []byte) (*clientv3.PutResponse, error) {
	rawClient, etcdClient, err := integration.GetEtcdClients(e.kubeAPIServer.ServerOpts.Etcd.StorageConfig.Transport)
	if err != nil {
		return nil, fmt.Errorf("failed to create etcd client: %v", err)
	}
	// kvClient is a wrapper around rawClient and to avoid leaking goroutines we need to
	// close the client (which we can do by closing rawClient).
	defer rawClient.Close()

	response, err := etcdClient.Put(context.Background(), path, string(data))
	if err != nil {
		return nil, fmt.Errorf("failed to write secret to etcd %v", err)
	}

	return response, nil
}

func (e *transformTest) printMetrics() error {
	e.Logf("Transformation Metrics:")
	metrics, err := legacyregistry.DefaultGatherer.Gather()
	if err != nil {
		return fmt.Errorf("failed to gather metrics: %s", err)
	}

	for _, mf := range metrics {
		if strings.HasPrefix(*mf.Name, metricsPrefix) {
			e.Logf("%s", *mf.Name)
			for _, metric := range mf.GetMetric() {
				e.Logf("%v", metric)
			}
		}
	}

	return nil
}

func mustBeHealthy(t kubeapiservertesting.Logger, checkName, wantBodyContains string, clientConfig *rest.Config, excludes ...string) {
	t.Helper()
	var restErr error
	pollErr := wait.PollUntilContextTimeout(context.TODO(), 1*time.Second, 5*time.Minute, true, func(ctx context.Context) (bool, error) {
		body, ok, err := getHealthz(checkName, clientConfig, excludes...)
		restErr = err
		if err != nil {
			return false, err
		}
		done := ok && strings.Contains(body, wantBodyContains)
		if !done {
			t.Logf("expected server check %q to be healthy with message %q but it is not: %s", checkName, wantBodyContains, body)
		}
		return done, nil
	})

	if pollErr != nil {
		t.Fatalf("failed to get the expected healthz status of OK for check: %s, error: %v, debug inner error: %v", checkName, pollErr, restErr)
	}
}

func mustBeUnHealthy(t kubeapiservertesting.Logger, checkName, wantBodyContains string, clientConfig *rest.Config, excludes ...string) {
	t.Helper()
	var restErr error
	pollErr := wait.PollUntilContextTimeout(context.TODO(), 1*time.Second, 5*time.Minute, true, func(ctx context.Context) (bool, error) {
		body, ok, err := getHealthz(checkName, clientConfig, excludes...)
		restErr = err
		if err != nil {
			return false, err
		}
		done := !ok && strings.Contains(body, wantBodyContains)
		if !done {
			t.Logf("expected server check %q to be unhealthy with message %q but it is not: %s", checkName, wantBodyContains, body)
		}
		return done, nil
	})

	if pollErr != nil {
		t.Fatalf("failed to get the expected healthz status of !OK for check: %s, error: %v, debug inner error: %v", checkName, pollErr, restErr)
	}
}

func mustNotHaveLivez(t kubeapiservertesting.Logger, checkName, wantBodyContains string, clientConfig *rest.Config, excludes ...string) {
	t.Helper()
	var restErr error
	pollErr := wait.PollUntilContextTimeout(context.TODO(), 1*time.Second, 5*time.Minute, true, func(ctx context.Context) (bool, error) {
		body, ok, err := getLivez(checkName, clientConfig, excludes...)
		restErr = err
		if err != nil {
			return false, err
		}
		done := !ok && strings.Contains(body, wantBodyContains)
		if !done {
			t.Logf("expected server check %q with message %q but it is not: %s", checkName, wantBodyContains, body)
		}
		return done, nil
	})

	if pollErr != nil {
		t.Fatalf("failed to get the expected livez status of !OK for check: %s, error: %v, debug inner error: %v", checkName, pollErr, restErr)
	}
}

func getHealthz(checkName string, clientConfig *rest.Config, excludes ...string) (string, bool, error) {
	client, err := kubernetes.NewForConfig(clientConfig)
	if err != nil {
		return "", false, fmt.Errorf("failed to create a client: %v", err)
	}

	req := client.CoreV1().RESTClient().Get().AbsPath(fmt.Sprintf("/healthz%v", checkName)).Param("verbose", "true")
	for _, exclude := range excludes {
		req.Param("exclude", exclude)
	}
	body, err := req.DoRaw(context.TODO()) // we can still have a response body during an error case
	return string(body), err == nil, nil
}

func getLivez(checkName string, clientConfig *rest.Config, excludes ...string) (string, bool, error) {
	client, err := kubernetes.NewForConfig(clientConfig)
	if err != nil {
		return "", false, fmt.Errorf("failed to create a client: %v", err)
	}

	req := client.CoreV1().RESTClient().Get().AbsPath(fmt.Sprintf("/livez%v", checkName)).Param("verbose", "true")
	for _, exclude := range excludes {
		req.Param("exclude", exclude)
	}
	body, err := req.DoRaw(context.TODO()) // we can still have a response body during an error case
	return string(body), err == nil, nil
}
