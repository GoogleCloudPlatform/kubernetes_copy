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

package services

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	cliflag "k8s.io/component-base/cli/flag"
	"k8s.io/klog/v2"
	kubeletconfigv1beta1 "k8s.io/kubelet/config/v1beta1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/kubernetes/cmd/kubelet/app/options"
	"k8s.io/kubernetes/pkg/cluster/ports"
	kubeletconfig "k8s.io/kubernetes/pkg/kubelet/apis/config"
	"k8s.io/kubernetes/pkg/kubelet/kubeletconfig/configfiles"
	kubeletconfigcodec "k8s.io/kubernetes/pkg/kubelet/kubeletconfig/util/codec"
	utilfs "k8s.io/kubernetes/pkg/util/filesystem"
	"k8s.io/kubernetes/test/e2e/framework"
	"k8s.io/kubernetes/test/e2e_node/builder"
	"k8s.io/kubernetes/test/e2e_node/remote"
)

// TODO(random-liu): Replace this with standard kubelet launcher.

// args is the type used to accumulate args from the flags with the same name.
type args []string

// String function of flag.Value
func (a *args) String() string {
	return fmt.Sprint(*a)
}

// Set function of flag.Value
func (a *args) Set(value string) error {
	// Note that we assume all white space in flag string is separating fields
	na := strings.Fields(value)
	*a = append(*a, na...)
	return nil
}

// kubeletArgs is the override kubelet args specified by the test runner.
var kubeletArgs args
var kubeletConfigFile = "./kubeletconfig.yaml"

func init() {
	flag.Var(&kubeletArgs, "kubelet-flags", "Kubelet flags passed to kubelet, this will override default kubelet flags in the test. Flags specified in multiple kubelet-flags will be concatenate. Deprecated, see: --kubelet-config-file.")
	if flag.Lookup("kubelet-config-file") == nil {
		flag.StringVar(&kubeletConfigFile, "kubelet-config-file", kubeletConfigFile, "The base KubeletConfiguration to use when setting up the kubelet. This configuration will then be minimially modified to support requirements from the test suite.")
	}
}

// RunKubelet starts kubelet and waits for termination signal. Once receives the
// termination signal, it will stop the kubelet gracefully.
func RunKubelet(featureGates map[string]bool) {
	var err error
	// Enable monitorParent to make sure kubelet will receive termination signal
	// when test process exits.
	e := NewE2EServices(true /* monitorParent */)
	defer e.Stop()
	e.kubelet, err = e.startKubelet(featureGates)
	if err != nil {
		klog.Fatalf("Failed to start kubelet: %v", err)
	}
	// Wait until receiving a termination signal.
	waitForTerminationSignal()
}

const (
	// KubeletRootDirectory specifies the directory where the kubelet runtime information is stored.
	KubeletRootDirectory = "/var/lib/kubelet"
)

// Health check url of kubelet
var kubeletHealthCheckURL = fmt.Sprintf("http://127.0.0.1:%d/healthz", ports.KubeletHealthzPort)

func baseKubeConfiguration(cfgPath string) (*kubeletconfig.KubeletConfiguration, error) {
	cfgPath, err := filepath.Abs(cfgPath)
	if err != nil {
		return nil, err
	}

	_, err = os.Stat(cfgPath)
	if err != nil {
		// If the kubeletconfig exists, but for some reason we can't read it, then
		// return an error to avoid silently skipping it.
		if !os.IsNotExist(err) {
			return nil, err
		}

		// If the kubeletconfig file doesn't exist, then use a default configuration
		// as the base.
		kc, err := options.NewKubeletConfiguration()
		if err != nil {
			return nil, err
		}

		// The following values should match the contents of
		// test/e2e_node/cos-init/default-kubelet-config.yaml. We can't use go embed
		// here to fallback as default config lives in a parallel directory.
		// TODO(endocrimes): Remove fallback for lack of kubelet config when all
		//                   uses of e2e_node switch to providing one (or move to
		//                   kubetest2 and pick up the default).
		kc.CgroupRoot = "/"
		kc.VolumeStatsAggPeriod = metav1.Duration{Duration: 10 * time.Second}
		kc.SerializeImagePulls = false
		kc.FileCheckFrequency = metav1.Duration{Duration: 10 * time.Second}
		kc.PodCIDR = "10.100.0.0/24"
		kc.EvictionPressureTransitionPeriod = metav1.Duration{Duration: 30 * time.Second}
		kc.EvictionHard = map[string]string{
			"memory.available":  "250Mi",
			"nodefs.available":  "10%",
			"nodefs.inodesFree": "5%",
		}
		kc.EvictionMinimumReclaim = map[string]string{
			"nodefs.available":  "5%",
			"nodefs.inodesFree": "5%",
		}

		return kc, nil
	}

	loader, err := configfiles.NewFsLoader(&utilfs.DefaultFs{}, cfgPath)
	if err != nil {
		return nil, err
	}

	return loader.Load()
}

// startKubelet starts the Kubelet in a separate process or returns an error
// if the Kubelet fails to start.
func (e *E2EServices) startKubelet(featureGates map[string]bool) (*server, error) {
	klog.Info("Starting kubelet")

	framework.Logf("Standalone mode: %v", framework.TestContext.StandaloneMode)

	var kubeconfigPath string

	if !framework.TestContext.StandaloneMode {
		var err error
		// Build kubeconfig
		kubeconfigPath, err = createKubeconfigCWD()
		if err != nil {
			return nil, err
		}
	}

	// KubeletConfiguration file path
	kubeletConfigPath, err := kubeletConfigCWDPath()
	if err != nil {
		return nil, err
	}

	// KubeletDropInConfiguration directory path
	framework.TestContext.KubeletConfigDropinDir, err = KubeletConfigDirCWDDir()
	if err != nil {
		return nil, err
	}

	// Create pod directory
	podPath, err := createPodDirectory()
	if err != nil {
		return nil, err
	}
	e.rmDirs = append(e.rmDirs, podPath)
	err = createRootDirectory(KubeletRootDirectory)
	if err != nil {
		return nil, err
	}

	lookup := flag.Lookup("kubelet-config-file")
	if lookup != nil {
		kubeletConfigFile = lookup.Value.String()
	}
	kc, err := baseKubeConfiguration(kubeletConfigFile)
	if err != nil {
		return nil, fmt.Errorf("failed to load base kubelet configuration: %w", err)
	}

	// Apply overrides to allow access to the Kubelet API from the test suite.
	// These are insecure and should generally not be used outside of test infra.

	// --anonymous-auth
	kc.Authentication.Anonymous.Enabled = true
	// --authentication-token-webhook
	kc.Authentication.Webhook.Enabled = false
	// --authorization-mode
	kc.Authorization.Mode = kubeletconfig.KubeletAuthorizationModeAlwaysAllow
	// --read-only-port
	kc.ReadOnlyPort = ports.KubeletReadOnlyPort

	// Static Pods are in a per-test location, so we override them for tests.
	kc.StaticPodPath = podPath

	var killCommand, restartCommand *exec.Cmd
	var isSystemd bool
	var unitName string
	// Apply default kubelet flags.
	cmdArgs := []string{}
	if systemdRun, err := exec.LookPath("systemd-run"); err == nil {
		// On systemd services, detection of a service / unit works reliably while
		// detection of a process started from an ssh session does not work.
		// Since kubelet will typically be run as a service it also makes more
		// sense to test it that way
		isSystemd = true

		// If we are running on systemd >=240, we can append to the
		// same log file on restarts
		logLocation := "StandardError=file:"
		if version, verr := exec.Command("systemd-run", "--version").Output(); verr == nil {
			// sample output from $ systemd-run --version
			// systemd 245 (245.4-4ubuntu3.13)
			re := regexp.MustCompile(`systemd (\d+)`)
			if match := re.FindSubmatch(version); len(match) > 1 {
				num, _ := strconv.Atoi(string(match[1]))
				if num >= 240 {
					logLocation = "StandardError=append:"
				}
			}
		}
		// We can ignore errors, to have GetTimestampFromWorkspaceDir() fallback
		// to the current time.
		cwd, _ := os.Getwd()
		// Use the timestamp from the current directory to name the systemd unit.
		unitTimestamp := remote.GetTimestampFromWorkspaceDir(cwd)
		unitName = fmt.Sprintf("kubelet-%s.service", unitTimestamp)
		cmdArgs = append(cmdArgs,
			systemdRun,
			"-p", "Delegate=true",
			"-p", logLocation+framework.TestContext.ReportDir+"/kubelet.log",
			"--unit="+unitName,
			"--slice=runtime.slice",
			"--remain-after-exit",
			builder.GetKubeletServerBin())

		killCommand = exec.Command("systemctl", "kill", unitName)
		restartCommand = exec.Command("systemctl", "restart", unitName)

		kc.KubeletCgroups = "/kubelet.slice"
	} else {
		cmdArgs = append(cmdArgs, builder.GetKubeletServerBin())
		// TODO(random-liu): Get rid of this docker specific thing.
		cmdArgs = append(cmdArgs, "--runtime-cgroups=/docker-daemon")

		kc.KubeletCgroups = "/kubelet"

		kc.SystemCgroups = "/system"
	}

	if !framework.TestContext.StandaloneMode {
		cmdArgs = append(cmdArgs,
			"--kubeconfig", kubeconfigPath,
		)
	}

	cmdArgs = append(cmdArgs,
		"--root-dir", KubeletRootDirectory,
		"--v", LogVerbosityLevel,
	)

	// Apply test framework feature gates by default. This could also be overridden
	// by kubelet-flags.
	if len(featureGates) > 0 {
		cmdArgs = append(cmdArgs, "--feature-gates", cliflag.NewMapStringBool(&featureGates).String())
		kc.FeatureGates = featureGates
	}

	// Add the KubeletDropinConfigDirectory flag if set.
	cmdArgs = append(cmdArgs, "--config-dir", framework.TestContext.KubeletConfigDropinDir)

	// Keep hostname override for convenience.
	if framework.TestContext.NodeName != "" { // If node name is specified, set hostname override.
		cmdArgs = append(cmdArgs, "--hostname-override", framework.TestContext.NodeName)
	}

	if framework.TestContext.ContainerRuntimeEndpoint != "" {
		cmdArgs = append(cmdArgs, "--container-runtime-endpoint", framework.TestContext.ContainerRuntimeEndpoint)
	}

	if framework.TestContext.ImageServiceEndpoint != "" {
		cmdArgs = append(cmdArgs, "--image-service-endpoint", framework.TestContext.ImageServiceEndpoint)
	}

	if err := WriteKubeletConfigFile(kc, kubeletConfigPath); err != nil {
		return nil, err
	}
	// add the flag to load config from a file
	cmdArgs = append(cmdArgs, "--config", kubeletConfigPath)

	// Override the default kubelet flags.
	cmdArgs = append(cmdArgs, kubeletArgs...)

	// Adjust the args if we are running kubelet with systemd.
	if isSystemd {
		adjustArgsForSystemd(cmdArgs)
	}

	cmd := exec.Command(cmdArgs[0], cmdArgs[1:]...)
	restartOnExit := framework.TestContext.RestartKubelet
	server := newServer(
		"kubelet",
		cmd,
		killCommand,
		restartCommand,
		[]string{kubeletHealthCheckURL},
		"kubelet.log",
		e.monitorParent,
		restartOnExit,
		unitName)
	return server, server.start()
}

// WriteKubeletConfigFile writes the kubelet config file based on the args and returns the filename
func WriteKubeletConfigFile(internal *kubeletconfig.KubeletConfiguration, path string) error {
	data, err := kubeletconfigcodec.EncodeKubeletConfig(internal, kubeletconfigv1beta1.SchemeGroupVersion)
	if err != nil {
		return err
	}
	// create the directory, if it does not exist
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	// write the file
	if err := os.WriteFile(path, data, 0755); err != nil {
		return err
	}
	return nil
}

// createPodDirectory creates pod directory.
func createPodDirectory() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("failed to get current working directory: %w", err)
	}
	path, err := os.MkdirTemp(cwd, "static-pods")
	if err != nil {
		return "", fmt.Errorf("failed to create static pod directory: %w", err)
	}
	return path, nil
}

// createKubeconfig creates a kubeconfig file at the fully qualified `path`. The parent dirs must exist.
func createKubeconfig(path string) error {
	kubeconfig := []byte(fmt.Sprintf(`apiVersion: v1
kind: Config
users:
- name: kubelet
  user:
    token: %s
clusters:
- cluster:
    server: %s
    insecure-skip-tls-verify: true
  name: local
contexts:
- context:
    cluster: local
    user: kubelet
  name: local-context
current-context: local-context`, framework.TestContext.BearerToken, getAPIServerClientURL()))

	if err := os.WriteFile(path, kubeconfig, 0666); err != nil {
		return err
	}
	return nil
}

func createRootDirectory(path string) error {
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return os.MkdirAll(path, os.FileMode(0755))
		}
		return err
	}
	return nil
}

func kubeconfigCWDPath() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("failed to get current working directory: %w", err)
	}
	return filepath.Join(cwd, "kubeconfig"), nil
}

func kubeletConfigCWDPath() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("failed to get current working directory: %w", err)
	}
	// DO NOT name this file "kubelet" - you will overwrite the kubelet binary and be very confused :)
	return filepath.Join(cwd, "kubelet-config"), nil
}

func KubeletConfigDirCWDDir() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("failed to get current working directory: %w", err)
	}
	dir := filepath.Join(cwd, "kubelet.conf.d")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", err
	}
	return dir, nil
}

// like createKubeconfig, but creates kubeconfig at current-working-directory/kubeconfig
// returns a fully-qualified path to the kubeconfig file
func createKubeconfigCWD() (string, error) {
	kubeconfigPath, err := kubeconfigCWDPath()
	if err != nil {
		return "", err
	}

	if err = createKubeconfig(kubeconfigPath); err != nil {
		return "", err
	}
	return kubeconfigPath, nil
}

// adjustArgsForSystemd escape special characters in kubelet arguments for systemd. Systemd
// may try to do auto expansion without escaping.
func adjustArgsForSystemd(args []string) {
	for i := range args {
		args[i] = strings.Replace(args[i], "%", "%%", -1)
		args[i] = strings.Replace(args[i], "$", "$$", -1)
	}
}
