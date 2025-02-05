/*
Copyright 2014 The Kubernetes Authors.

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

package clientcmd

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	utiltesting "k8s.io/client-go/util/testing"

	"github.com/google/go-cmp/cmp"
	"sigs.k8s.io/yaml"

	"k8s.io/apimachinery/pkg/runtime"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	clientcmdlatest "k8s.io/client-go/tools/clientcmd/api/latest"
	"k8s.io/klog/v2"
)

var (
	testConfigAlfa = clientcmdapi.Config{
		AuthInfos: map[string]*clientcmdapi.AuthInfo{
			"red-user": {Token: "red-token"}},
		Clusters: map[string]*clientcmdapi.Cluster{
			"cow-cluster": {Server: "http://cow.org:8080"}},
		Contexts: map[string]*clientcmdapi.Context{
			"federal-context": {AuthInfo: "red-user", Cluster: "cow-cluster", Namespace: "hammer-ns"}},
	}
	testConfigBravo = clientcmdapi.Config{
		AuthInfos: map[string]*clientcmdapi.AuthInfo{
			"black-user": {Token: "black-token"}},
		Clusters: map[string]*clientcmdapi.Cluster{
			"pig-cluster": {Server: "http://pig.org:8080"}},
		Contexts: map[string]*clientcmdapi.Context{
			"queen-anne-context": {AuthInfo: "black-user", Cluster: "pig-cluster", Namespace: "saw-ns"}},
	}
	testConfigCharlie = clientcmdapi.Config{
		AuthInfos: map[string]*clientcmdapi.AuthInfo{
			"green-user": {Token: "green-token"}},
		Clusters: map[string]*clientcmdapi.Cluster{
			"horse-cluster": {Server: "http://horse.org:8080"}},
		Contexts: map[string]*clientcmdapi.Context{
			"shaker-context": {AuthInfo: "green-user", Cluster: "horse-cluster", Namespace: "chisel-ns"}},
	}
	testConfigDelta = clientcmdapi.Config{
		AuthInfos: map[string]*clientcmdapi.AuthInfo{
			"blue-user": {Token: "blue-token"}},
		Clusters: map[string]*clientcmdapi.Cluster{
			"chicken-cluster": {Server: "http://chicken.org:8080"}},
		Contexts: map[string]*clientcmdapi.Context{
			"gothic-context": {AuthInfo: "blue-user", Cluster: "chicken-cluster", Namespace: "plane-ns"}},
	}

	testConfigConflictAlfa = clientcmdapi.Config{
		AuthInfos: map[string]*clientcmdapi.AuthInfo{
			"red-user":    {Token: "a-different-red-token"},
			"yellow-user": {Token: "yellow-token"}},
		Clusters: map[string]*clientcmdapi.Cluster{
			"cow-cluster":    {Server: "http://a-different-cow.org:8080", InsecureSkipTLSVerify: true, DisableCompression: true},
			"donkey-cluster": {Server: "http://donkey.org:8080", InsecureSkipTLSVerify: true, DisableCompression: true}},
		CurrentContext: "federal-context",
	}
)

func TestNilOutMap(t *testing.T) {
	var fakeKubeconfigData = `apiVersion: v1
kind: Config
clusters:
- cluster:
    certificate-authority-data: UEhPTlkK
    server: https://1.1.1.1
  name: production
contexts:
- context:
    cluster: production
    user: production
  name: production
current-context: production
users:
- name: production
  user:
    auth-provider:
      name: gcp`

	_, _, err := clientcmdlatest.Codec.Decode([]byte(fakeKubeconfigData), nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestNonExistentCommandLineFile(t *testing.T) {
	loadingRules := ClientConfigLoadingRules{
		ExplicitPath: "bogus_file",
	}

	_, err := loadingRules.Load()
	if err == nil {
		t.Fatalf("Expected error for missing command-line file, got none")
	}
	if !strings.Contains(err.Error(), "bogus_file") {
		t.Fatalf("Expected error about 'bogus_file', got %s", err.Error())
	}
}

//nolint:logcheck // Tests klog APIs.
func TestToleratingMissingFiles(t *testing.T) {
	envVarValue := "bogus"
	loadingRules := ClientConfigLoadingRules{
		Precedence:       []string{"bogus1", "bogus2", "bogus3"},
		WarnIfAllMissing: true,
		Warner:           func(err error) { klog.Warning(err) },
	}

	buffer := &bytes.Buffer{}

	klog.LogToStderr(false)
	klog.SetOutput(buffer)

	_, err := loadingRules.Load()
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	klog.Flush()
	expectedLog := fmt.Sprintf("Config not found: %s", envVarValue)
	if !strings.Contains(buffer.String(), expectedLog) {
		t.Fatalf("expected log: \"%s\"", expectedLog)
	}
}

//nolint:logcheck // Tests klog APIs.
func TestWarningMissingFiles(t *testing.T) {
	envVarValue := "bogus"
	t.Setenv(RecommendedConfigPathEnvVar, envVarValue)
	loadingRules := NewDefaultClientConfigLoadingRules()

	buffer := &bytes.Buffer{}

	flags := &flag.FlagSet{}
	klog.InitFlags(flags)
	flags.Set("v", "1")
	klog.LogToStderr(false)
	klog.SetOutput(buffer)

	_, err := loadingRules.Load()
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	klog.Flush()

	expectedLog := fmt.Sprintf("Config not found: %s", envVarValue)
	if !strings.Contains(buffer.String(), expectedLog) {
		t.Fatalf("expected log: \"%s\"", expectedLog)
	}
}

//nolint:logcheck // Tests klog APIs.
func TestNoWarningMissingFiles(t *testing.T) {
	envVarValue := "bogus"
	t.Setenv(RecommendedConfigPathEnvVar, envVarValue)
	loadingRules := NewDefaultClientConfigLoadingRules()

	buffer := &bytes.Buffer{}

	flags := &flag.FlagSet{}
	klog.InitFlags(flags)
	flags.Set("v", "0")
	klog.LogToStderr(false)
	klog.SetOutput(buffer)

	_, err := loadingRules.Load()
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	klog.Flush()

	logNotExpected := fmt.Sprintf("Config not found: %s", envVarValue)
	if strings.Contains(buffer.String(), logNotExpected) {
		t.Fatalf("log not expected: \"%s\"", logNotExpected)
	}
}

func TestErrorReadingFile(t *testing.T) {
	commandLineFile, _ := os.CreateTemp("", "")
	defer utiltesting.CloseAndRemove(t, commandLineFile)

	if err := os.WriteFile(commandLineFile.Name(), []byte("bogus value"), 0644); err != nil {
		t.Fatalf("Error creating tempfile: %v", err)
	}

	loadingRules := ClientConfigLoadingRules{
		ExplicitPath: commandLineFile.Name(),
	}

	_, err := loadingRules.Load()
	if err == nil {
		t.Fatalf("Expected error for unloadable file, got none")
	}
	if !strings.Contains(err.Error(), commandLineFile.Name()) {
		t.Fatalf("Expected error about '%s', got %s", commandLineFile.Name(), err.Error())
	}
}

func TestErrorReadingNonFile(t *testing.T) {
	tmpdir, err := os.MkdirTemp("", "")
	if err != nil {
		t.Fatalf("Couldn't create tmpdir")
	}
	defer os.RemoveAll(tmpdir)

	loadingRules := ClientConfigLoadingRules{
		ExplicitPath: tmpdir,
	}

	_, err = loadingRules.Load()
	if err == nil {
		t.Fatalf("Expected error for non-file, got none")
	}
	if !strings.Contains(err.Error(), tmpdir) {
		t.Fatalf("Expected error about '%s', got %s", tmpdir, err.Error())
	}
}

func TestConflictingCurrentContext(t *testing.T) {
	commandLineFile, _ := os.CreateTemp("", "")
	envVarFile, _ := os.CreateTemp("", "")
	defer utiltesting.CloseAndRemove(t, commandLineFile, envVarFile)

	mockCommandLineConfig := clientcmdapi.Config{
		CurrentContext: "any-context-value",
	}
	mockEnvVarConfig := clientcmdapi.Config{
		CurrentContext: "a-different-context",
	}

	WriteToFile(mockCommandLineConfig, commandLineFile.Name())
	WriteToFile(mockEnvVarConfig, envVarFile.Name())

	loadingRules := ClientConfigLoadingRules{
		ExplicitPath: commandLineFile.Name(),
		Precedence:   []string{envVarFile.Name()},
	}

	mergedConfig, err := loadingRules.Load()
	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}

	if mergedConfig.CurrentContext != mockCommandLineConfig.CurrentContext {
		t.Errorf("expected %v, got %v", mockCommandLineConfig.CurrentContext, mergedConfig.CurrentContext)
	}
}

func TestEncodeYAML(t *testing.T) {
	config := clientcmdapi.Config{
		CurrentContext: "any-context-value",
		Contexts: map[string]*clientcmdapi.Context{
			"433e40": {
				Cluster: "433e40",
			},
		},
		Clusters: map[string]*clientcmdapi.Cluster{
			"0": {
				Server: "https://localhost:1234",
			},
			"1": {
				Server: "https://localhost:1234",
			},
			"433e40": {
				Server: "https://localhost:1234",
			},
		},
	}
	data, err := Write(config)
	if err != nil {
		t.Fatal(err)
	}
	expected := []byte(`apiVersion: v1
clusters:
- cluster:
    server: https://localhost:1234
  name: "0"
- cluster:
    server: https://localhost:1234
  name: "1"
- cluster:
    server: https://localhost:1234
  name: "433e40"
contexts:
- context:
    cluster: "433e40"
    user: ""
  name: "433e40"
current-context: any-context-value
kind: Config
preferences: {}
users: null
`)
	if !bytes.Equal(expected, data) {
		t.Error(cmp.Diff(string(expected), string(data)))
	}
}

func TestLoadingEmptyMaps(t *testing.T) {
	configFile, _ := os.CreateTemp("", "")
	defer utiltesting.CloseAndRemove(t, configFile)

	mockConfig := clientcmdapi.Config{
		CurrentContext: "any-context-value",
	}

	WriteToFile(mockConfig, configFile.Name())

	config, err := LoadFromFile(configFile.Name())
	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}

	if config.Clusters == nil {
		t.Error("expected config.Clusters to be non-nil")
	}
	if config.AuthInfos == nil {
		t.Error("expected config.AuthInfos to be non-nil")
	}
	if config.Contexts == nil {
		t.Error("expected config.Contexts to be non-nil")
	}
}

func TestDuplicateClusterName(t *testing.T) {
	configFile, _ := os.CreateTemp("", "")
	defer utiltesting.CloseAndRemove(t, configFile)

	err := os.WriteFile(configFile.Name(), []byte(`
kind: Config
apiVersion: v1
clusters:
- cluster:
    api-version: v1
    server: https://kubernetes.default.svc:443
    certificate-authority: /var/run/secrets/kubernetes.io/serviceaccount/ca.crt
  name: kubeconfig-cluster
- cluster:
    api-version: v2
    server: https://test.example.server:443
    certificate-authority: /var/run/secrets/test.example.io/serviceaccount/ca.crt
  name: kubeconfig-cluster
contexts:
- context:
    cluster: kubeconfig-cluster
    namespace: default
    user: kubeconfig-user
  name: kubeconfig-context
current-context: kubeconfig-context
users:
- name: kubeconfig-user
  user:
    tokenFile: /var/run/secrets/kubernetes.io/serviceaccount/token
`), os.FileMode(0755))

	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}

	_, err = LoadFromFile(configFile.Name())
	if err == nil || !strings.Contains(err.Error(),
		"error converting *[]NamedCluster into *map[string]*api.Cluster: duplicate name \"kubeconfig-cluster\" in list") {
		t.Error("Expected error in loading duplicate cluster name, got none")
	}
}

func TestDuplicateContextName(t *testing.T) {
	configFile, _ := os.CreateTemp("", "")
	defer utiltesting.CloseAndRemove(t, configFile)

	err := os.WriteFile(configFile.Name(), []byte(`
kind: Config
apiVersion: v1
clusters:
- cluster:
    api-version: v1
    server: https://kubernetes.default.svc:443
    certificate-authority: /var/run/secrets/kubernetes.io/serviceaccount/ca.crt
  name: kubeconfig-cluster
contexts:
- context:
    cluster: kubeconfig-cluster
    namespace: default
    user: kubeconfig-user
  name: kubeconfig-context
- context:
    cluster: test-example-cluster
    namespace: test-example
    user: test-example-user
  name: kubeconfig-context
current-context: kubeconfig-context
users:
- name: kubeconfig-user
  user:
    tokenFile: /var/run/secrets/kubernetes.io/serviceaccount/token
`), os.FileMode(0755))

	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}

	_, err = LoadFromFile(configFile.Name())
	if err == nil || !strings.Contains(err.Error(),
		"error converting *[]NamedContext into *map[string]*api.Context: duplicate name \"kubeconfig-context\" in list") {
		t.Error("Expected error in loading duplicate context name, got none")
	}
}

func TestDuplicateUserName(t *testing.T) {
	configFile, _ := os.CreateTemp("", "")
	defer utiltesting.CloseAndRemove(t, configFile)

	err := os.WriteFile(configFile.Name(), []byte(`
kind: Config
apiVersion: v1
clusters:
- cluster:
    api-version: v1
    server: https://kubernetes.default.svc:443
    certificate-authority: /var/run/secrets/kubernetes.io/serviceaccount/ca.crt
  name: kubeconfig-cluster
contexts:
- context:
    cluster: kubeconfig-cluster
    namespace: default
    user: kubeconfig-user
  name: kubeconfig-context
current-context: kubeconfig-context
users:
- name: kubeconfig-user
  user:
    tokenFile: /var/run/secrets/kubernetes.io/serviceaccount/token
- name: kubeconfig-user
  user:
    tokenFile: /var/run/secrets/test.example.com/serviceaccount/token
`), os.FileMode(0755))

	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}

	_, err = LoadFromFile(configFile.Name())
	if err == nil || !strings.Contains(err.Error(),
		"error converting *[]NamedAuthInfo into *map[string]*api.AuthInfo: duplicate name \"kubeconfig-user\" in list") {
		t.Error("Expected error in loading duplicate user name, got none")
	}
}

func TestDuplicateExtensionName(t *testing.T) {
	configFile, _ := os.CreateTemp("", "")
	defer utiltesting.CloseAndRemove(t, configFile)

	err := os.WriteFile(configFile.Name(), []byte(`
kind: Config
apiVersion: v1
clusters:
- cluster:
    api-version: v1
    server: https://kubernetes.default.svc:443
    certificate-authority: /var/run/secrets/kubernetes.io/serviceaccount/ca.crt
  name: kubeconfig-cluster
contexts:
- context:
    cluster: kubeconfig-cluster
    namespace: default
    user: kubeconfig-user
  name: kubeconfig-context
current-context: kubeconfig-context
users:
- name: kubeconfig-user
  user:
    tokenFile: /var/run/secrets/kubernetes.io/serviceaccount/token
extensions:
- extension:
    bytes: test
  name: test-extension
- extension:
    bytes: some-example
  name: test-extension
`), os.FileMode(0755))

	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}

	_, err = LoadFromFile(configFile.Name())
	if err == nil || !strings.Contains(err.Error(),
		"error converting *[]NamedExtension into *map[string]runtime.Object: duplicate name \"test-extension\" in list") {
		t.Error("Expected error in loading duplicate extension name, got none")
	}
}

func TestResolveRelativePaths(t *testing.T) {
	pathResolutionConfig1 := clientcmdapi.Config{
		AuthInfos: map[string]*clientcmdapi.AuthInfo{
			"relative-user-1": {ClientCertificate: "relative/client/cert", ClientKey: "../relative/client/key"},
			"absolute-user-1": {ClientCertificate: "/absolute/client/cert", ClientKey: "/absolute/client/key"},
			"relative-cmd-1":  {Exec: &clientcmdapi.ExecConfig{Command: "../relative/client/cmd"}},
			"absolute-cmd-1":  {Exec: &clientcmdapi.ExecConfig{Command: "/absolute/client/cmd"}},
			"PATH-cmd-1":      {Exec: &clientcmdapi.ExecConfig{Command: "cmd"}},
		},
		Clusters: map[string]*clientcmdapi.Cluster{
			"relative-server-1": {CertificateAuthority: "../relative/ca"},
			"absolute-server-1": {CertificateAuthority: "/absolute/ca"},
		},
	}
	pathResolutionConfig2 := clientcmdapi.Config{
		AuthInfos: map[string]*clientcmdapi.AuthInfo{
			"relative-user-2": {ClientCertificate: "relative/client/cert2", ClientKey: "../relative/client/key2"},
			"absolute-user-2": {ClientCertificate: "/absolute/client/cert2", ClientKey: "/absolute/client/key2"},
		},
		Clusters: map[string]*clientcmdapi.Cluster{
			"relative-server-2": {CertificateAuthority: "../relative/ca2"},
			"absolute-server-2": {CertificateAuthority: "/absolute/ca2"},
		},
	}

	configDir1, _ := os.MkdirTemp("", "")
	defer os.RemoveAll(configDir1)
	configFile1 := filepath.Join(configDir1, ".kubeconfig")
	configDir1, _ = filepath.Abs(configDir1)

	configDir2, _ := os.MkdirTemp("", "")
	defer os.RemoveAll(configDir2)
	configDir2, _ = os.MkdirTemp(configDir2, "")
	configFile2 := filepath.Join(configDir2, ".kubeconfig")
	configDir2, _ = filepath.Abs(configDir2)

	WriteToFile(pathResolutionConfig1, configFile1)
	WriteToFile(pathResolutionConfig2, configFile2)

	loadingRules := ClientConfigLoadingRules{
		Precedence: []string{configFile1, configFile2},
	}

	mergedConfig, err := loadingRules.Load()
	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}

	foundClusterCount := 0
	for key, cluster := range mergedConfig.Clusters {
		if key == "relative-server-1" {
			foundClusterCount++
			matchStringArg(filepath.Join(configDir1, pathResolutionConfig1.Clusters["relative-server-1"].CertificateAuthority), cluster.CertificateAuthority, t)
		}
		if key == "relative-server-2" {
			foundClusterCount++
			matchStringArg(filepath.Join(configDir2, pathResolutionConfig2.Clusters["relative-server-2"].CertificateAuthority), cluster.CertificateAuthority, t)
		}
		if key == "absolute-server-1" {
			foundClusterCount++
			matchStringArg(pathResolutionConfig1.Clusters["absolute-server-1"].CertificateAuthority, cluster.CertificateAuthority, t)
		}
		if key == "absolute-server-2" {
			foundClusterCount++
			matchStringArg(pathResolutionConfig2.Clusters["absolute-server-2"].CertificateAuthority, cluster.CertificateAuthority, t)
		}
	}
	if foundClusterCount != 4 {
		t.Errorf("Expected 4 clusters, found %v: %v", foundClusterCount, mergedConfig.Clusters)
	}

	foundAuthInfoCount := 0
	for key, authInfo := range mergedConfig.AuthInfos {
		if key == "relative-user-1" {
			foundAuthInfoCount++
			matchStringArg(filepath.Join(configDir1, pathResolutionConfig1.AuthInfos["relative-user-1"].ClientCertificate), authInfo.ClientCertificate, t)
			matchStringArg(filepath.Join(configDir1, pathResolutionConfig1.AuthInfos["relative-user-1"].ClientKey), authInfo.ClientKey, t)
		}
		if key == "relative-user-2" {
			foundAuthInfoCount++
			matchStringArg(filepath.Join(configDir2, pathResolutionConfig2.AuthInfos["relative-user-2"].ClientCertificate), authInfo.ClientCertificate, t)
			matchStringArg(filepath.Join(configDir2, pathResolutionConfig2.AuthInfos["relative-user-2"].ClientKey), authInfo.ClientKey, t)
		}
		if key == "absolute-user-1" {
			foundAuthInfoCount++
			matchStringArg(pathResolutionConfig1.AuthInfos["absolute-user-1"].ClientCertificate, authInfo.ClientCertificate, t)
			matchStringArg(pathResolutionConfig1.AuthInfos["absolute-user-1"].ClientKey, authInfo.ClientKey, t)
		}
		if key == "absolute-user-2" {
			foundAuthInfoCount++
			matchStringArg(pathResolutionConfig2.AuthInfos["absolute-user-2"].ClientCertificate, authInfo.ClientCertificate, t)
			matchStringArg(pathResolutionConfig2.AuthInfos["absolute-user-2"].ClientKey, authInfo.ClientKey, t)
		}
		if key == "relative-cmd-1" {
			foundAuthInfoCount++
			matchStringArg(filepath.Join(configDir1, pathResolutionConfig1.AuthInfos[key].Exec.Command), authInfo.Exec.Command, t)
		}
		if key == "absolute-cmd-1" {
			foundAuthInfoCount++
			matchStringArg(pathResolutionConfig1.AuthInfos[key].Exec.Command, authInfo.Exec.Command, t)
		}
		if key == "PATH-cmd-1" {
			foundAuthInfoCount++
			matchStringArg(pathResolutionConfig1.AuthInfos[key].Exec.Command, authInfo.Exec.Command, t)
		}
	}
	if foundAuthInfoCount != 7 {
		t.Errorf("Expected 7 users, found %v: %v", foundAuthInfoCount, mergedConfig.AuthInfos)
	}

}

func TestMigratingFile(t *testing.T) {
	sourceFile, _ := os.CreateTemp("", "")
	defer utiltesting.CloseAndRemove(t, sourceFile)
	destinationFile, _ := os.CreateTemp("", "")
	// delete the file so that we'll write to it
	os.Remove(destinationFile.Name())

	WriteToFile(testConfigAlfa, sourceFile.Name())

	loadingRules := ClientConfigLoadingRules{
		MigrationRules: map[string]string{destinationFile.Name(): sourceFile.Name()},
	}

	if _, err := loadingRules.Load(); err != nil {
		t.Errorf("unexpected error %v", err)
	}
	// the load should have recreated this file
	defer utiltesting.CloseAndRemove(t, destinationFile)

	sourceContent, err := os.ReadFile(sourceFile.Name())
	if err != nil {
		t.Errorf("unexpected error %v", err)
	}
	destinationContent, err := os.ReadFile(destinationFile.Name())
	if err != nil {
		t.Errorf("unexpected error %v", err)
	}

	if !reflect.DeepEqual(sourceContent, destinationContent) {
		t.Errorf("source and destination do not match")
	}
}

func TestMigratingFileLeaveExistingFileAlone(t *testing.T) {
	sourceFile, _ := os.CreateTemp("", "")
	destinationFile, _ := os.CreateTemp("", "")
	defer utiltesting.CloseAndRemove(t, sourceFile, destinationFile)

	WriteToFile(testConfigAlfa, sourceFile.Name())

	loadingRules := ClientConfigLoadingRules{
		MigrationRules: map[string]string{destinationFile.Name(): sourceFile.Name()},
	}

	if _, err := loadingRules.Load(); err != nil {
		t.Errorf("unexpected error %v", err)
	}

	destinationContent, err := os.ReadFile(destinationFile.Name())
	if err != nil {
		t.Errorf("unexpected error %v", err)
	}

	if len(destinationContent) > 0 {
		t.Errorf("destination should not have been touched")
	}
}

func TestMigratingFileSourceMissingSkip(t *testing.T) {
	sourceFilename := "some-missing-file"
	destinationFile, _ := os.CreateTemp("", "")
	// delete the file so that we'll write to it
	utiltesting.CloseAndRemove(t, destinationFile)

	loadingRules := ClientConfigLoadingRules{
		MigrationRules: map[string]string{destinationFile.Name(): sourceFilename},
	}

	if _, err := loadingRules.Load(); err != nil {
		t.Errorf("unexpected error %v", err)
	}

	if _, err := os.Stat(destinationFile.Name()); !os.IsNotExist(err) {
		t.Errorf("destination should not exist")
	}
}

func TestFileLocking(t *testing.T) {
	f, _ := os.CreateTemp("", "")
	defer utiltesting.CloseAndRemove(t, f)

	err := lockFile(f.Name())
	if err != nil {
		t.Errorf("unexpected error while locking file: %v", err)
	}
	defer unlockFile(f.Name())

	err = lockFile(f.Name())
	if err == nil {
		t.Error("expected error while locking file.")
	}
}

func Example_noMergingOnExplicitPaths() {
	commandLineFile, _ := os.CreateTemp("", "")
	envVarFile, _ := os.CreateTemp("", "")
	defer utiltesting.CloseAndRemove(&testing.T{}, commandLineFile, envVarFile)

	WriteToFile(testConfigAlfa, commandLineFile.Name())
	WriteToFile(testConfigConflictAlfa, envVarFile.Name())

	loadingRules := ClientConfigLoadingRules{
		ExplicitPath: commandLineFile.Name(),
		Precedence:   []string{envVarFile.Name()},
	}

	mergedConfig, err := loadingRules.Load()
	if err != nil {
		fmt.Printf("Unexpected error: %v", err)
	}
	json, err := runtime.Encode(clientcmdlatest.Codec, mergedConfig)
	if err != nil {
		fmt.Printf("Unexpected error: %v", err)
	}
	output, err := yaml.JSONToYAML(json)
	if err != nil {
		fmt.Printf("Unexpected error: %v", err)
	}

	fmt.Printf("%v", string(output))
	// Output:
	// apiVersion: v1
	// clusters:
	// - cluster:
	//     server: http://cow.org:8080
	//   name: cow-cluster
	// contexts:
	// - context:
	//     cluster: cow-cluster
	//     namespace: hammer-ns
	//     user: red-user
	//   name: federal-context
	// current-context: ""
	// kind: Config
	// preferences: {}
	// users:
	// - name: red-user
	//   user:
	//     token: red-token
}

func Example_mergingSomeWithConflict() {
	commandLineFile, _ := os.CreateTemp("", "")
	envVarFile, _ := os.CreateTemp("", "")
	defer utiltesting.CloseAndRemove(&testing.T{}, commandLineFile, envVarFile)

	WriteToFile(testConfigAlfa, commandLineFile.Name())
	WriteToFile(testConfigConflictAlfa, envVarFile.Name())

	loadingRules := ClientConfigLoadingRules{
		Precedence: []string{commandLineFile.Name(), envVarFile.Name()},
	}

	mergedConfig, err := loadingRules.Load()
	if err != nil {
		fmt.Printf("Unexpected error: %v", err)
	}
	json, err := runtime.Encode(clientcmdlatest.Codec, mergedConfig)
	if err != nil {
		fmt.Printf("Unexpected error: %v", err)
	}
	output, err := yaml.JSONToYAML(json)
	if err != nil {
		fmt.Printf("Unexpected error: %v", err)
	}

	fmt.Printf("%v", string(output))
	// Output:
	// apiVersion: v1
	// clusters:
	// - cluster:
	//     server: http://cow.org:8080
	//   name: cow-cluster
	// - cluster:
	//     disable-compression: true
	//     insecure-skip-tls-verify: true
	//     server: http://donkey.org:8080
	//   name: donkey-cluster
	// contexts:
	// - context:
	//     cluster: cow-cluster
	//     namespace: hammer-ns
	//     user: red-user
	//   name: federal-context
	// current-context: federal-context
	// kind: Config
	// preferences: {}
	// users:
	// - name: red-user
	//   user:
	//     token: red-token
	// - name: yellow-user
	//   user:
	//     token: yellow-token
}

func Example_mergingEverythingNoConflicts() {
	commandLineFile, _ := os.CreateTemp("", "")
	envVarFile, _ := os.CreateTemp("", "")
	currentDirFile, _ := os.CreateTemp("", "")
	homeDirFile, _ := os.CreateTemp("", "")
	defer utiltesting.CloseAndRemove(&testing.T{}, commandLineFile, envVarFile, currentDirFile, homeDirFile)

	WriteToFile(testConfigAlfa, commandLineFile.Name())
	WriteToFile(testConfigBravo, envVarFile.Name())
	WriteToFile(testConfigCharlie, currentDirFile.Name())
	WriteToFile(testConfigDelta, homeDirFile.Name())

	loadingRules := ClientConfigLoadingRules{
		Precedence: []string{commandLineFile.Name(), envVarFile.Name(), currentDirFile.Name(), homeDirFile.Name()},
	}

	mergedConfig, err := loadingRules.Load()
	if err != nil {
		fmt.Printf("Unexpected error: %v", err)
	}
	json, err := runtime.Encode(clientcmdlatest.Codec, mergedConfig)
	if err != nil {
		fmt.Printf("Unexpected error: %v", err)
	}
	output, err := yaml.JSONToYAML(json)
	if err != nil {
		fmt.Printf("Unexpected error: %v", err)
	}

	fmt.Printf("%v", string(output))
	// Output:
	// 	apiVersion: v1
	// clusters:
	// - cluster:
	//     server: http://chicken.org:8080
	//   name: chicken-cluster
	// - cluster:
	//     server: http://cow.org:8080
	//   name: cow-cluster
	// - cluster:
	//     server: http://horse.org:8080
	//   name: horse-cluster
	// - cluster:
	//     server: http://pig.org:8080
	//   name: pig-cluster
	// contexts:
	// - context:
	//     cluster: cow-cluster
	//     namespace: hammer-ns
	//     user: red-user
	//   name: federal-context
	// - context:
	//     cluster: chicken-cluster
	//     namespace: plane-ns
	//     user: blue-user
	//   name: gothic-context
	// - context:
	//     cluster: pig-cluster
	//     namespace: saw-ns
	//     user: black-user
	//   name: queen-anne-context
	// - context:
	//     cluster: horse-cluster
	//     namespace: chisel-ns
	//     user: green-user
	//   name: shaker-context
	// current-context: ""
	// kind: Config
	// preferences: {}
	// users:
	// - name: black-user
	//   user:
	//     token: black-token
	// - name: blue-user
	//   user:
	//     token: blue-token
	// - name: green-user
	//   user:
	//     token: green-token
	// - name: red-user
	//   user:
	//     token: red-token
}

func TestDeduplicate(t *testing.T) {
	testCases := []struct {
		src    []string
		expect []string
	}{
		{
			src:    []string{"a", "b", "c", "d", "e", "f"},
			expect: []string{"a", "b", "c", "d", "e", "f"},
		},
		{
			src:    []string{"a", "b", "c", "b", "e", "f"},
			expect: []string{"a", "b", "c", "e", "f"},
		},
		{
			src:    []string{"a", "a", "b", "b", "c", "b"},
			expect: []string{"a", "b", "c"},
		},
	}

	for _, testCase := range testCases {
		get := deduplicate(testCase.src)
		if !reflect.DeepEqual(get, testCase.expect) {
			t.Errorf("expect: %v, get: %v", testCase.expect, get)
		}
	}
}

func TestLoadingGetLoadingPrecedence(t *testing.T) {
	testCases := map[string]struct {
		rules      *ClientConfigLoadingRules
		env        string
		precedence []string
	}{
		"default": {
			precedence: []string{filepath.Join(os.Getenv("HOME"), ".kube/config")},
		},
		"explicit": {
			rules: &ClientConfigLoadingRules{
				ExplicitPath: "/explicit/kubeconfig",
			},
			precedence: []string{"/explicit/kubeconfig"},
		},
		"envvar-single": {
			env:        "/env/kubeconfig",
			precedence: []string{"/env/kubeconfig"},
		},
		"envvar-multiple": {
			env:        "/env/kubeconfig:/other/kubeconfig",
			precedence: []string{"/env/kubeconfig", "/other/kubeconfig"},
		},
	}

	for name, test := range testCases {
		t.Run(name, func(t *testing.T) {
			t.Setenv("KUBECONFIG", test.env)
			rules := test.rules
			if rules == nil {
				rules = NewDefaultClientConfigLoadingRules()
			}
			actual := rules.GetLoadingPrecedence()
			if !reflect.DeepEqual(actual, test.precedence) {
				t.Errorf("expect %v, got %v", test.precedence, actual)
			}
		})
	}
}
