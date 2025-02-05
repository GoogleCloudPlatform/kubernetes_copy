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

package builder

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"k8s.io/klog/v2"
	"k8s.io/kubernetes/test/utils"
)

var k8sBinDir = flag.String("k8s-bin-dir", "", "Directory containing k8s kubelet binaries.")
var useDockerizedBuild = flag.Bool("use-dockerized-build", false, "Use dockerized build for test artifacts")
var targetBuildArch = flag.String("target-build-arch", "linux/amd64", "Target architecture for the test artifacts for dockerized build")

var buildCGOTargets = []string{
	"cmd/kubelet",
}

var buildNoCGOTargets = []string{
	"test/e2e_node/e2e_node.test",
	"github.com/onsi/ginkgo/v2/ginkgo",
	"cluster/gce/gci/mounter",
	"test/e2e_node/plugins/gcp-credential-provider",
}

// BuildGo builds k8s binaries.
func BuildGo() error {
	if err := BuildTargets(true); err != nil {
		return fmt.Errorf("unable to build cgo targets : %w", err)
	}
	if err := BuildTargets(false); err != nil {
		return fmt.Errorf("unable to build non-cgo targets : %w", err)
	}
	return nil
}

// BuildGo builds k8s binaries.
func BuildTargets(cgo bool) error {
	klog.Infof("Building k8s binaries...")
	k8sRoot, err := utils.GetK8sRootDir()
	if err != nil {
		return fmt.Errorf("failed to locate kubernetes root directory %v", err)
	}
	targets := buildCGOTargets
	if !cgo {
		targets = buildNoCGOTargets
	}
	what := strings.Join(targets, " ")
	cmd := exec.Command("make", "-C", k8sRoot,
		fmt.Sprintf("WHAT=%s", what))
	if cgo {
		cmd.Args = append(cmd.Args, "CGO_ENABLED=1")
	} else {
		cmd.Args = append(cmd.Args, "CGO_ENABLED=0")
	}
	if IsDockerizedBuild() {
		klog.Infof("Building dockerized k8s binaries targets %s for architecture %s", targets, GetTargetBuildArch())
		// Multi-architecture build is only supported in dockerized build
		cmd = exec.Command(filepath.Join(k8sRoot, "build/run.sh"), "make", fmt.Sprintf("WHAT=%s", what), fmt.Sprintf("KUBE_BUILD_PLATFORMS=%s", GetTargetBuildArch()))
	}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err = cmd.Run()
	if err != nil {
		return fmt.Errorf("failed to build go packages %v", err)
	}
	return nil
}

// IsDockerizedBuild returns if test needs to use dockerized build
func IsDockerizedBuild() bool {
	return *useDockerizedBuild
}

// GetTargetBuildArch returns the target build architecture for dockerized build
func GetTargetBuildArch() string {
	return *targetBuildArch
}

// IsTargetArchArm64 returns if the target is for linux/arm64 platform
func IsTargetArchArm64() bool {
	return GetTargetBuildArch() == "linux/arm64"
}

func getK8sBin(bin string) (string, error) {
	// Use commandline specified path
	if *k8sBinDir != "" {
		absPath, err := filepath.Abs(*k8sBinDir)
		if err != nil {
			return "", err
		}
		if _, err := os.Stat(filepath.Join(*k8sBinDir, bin)); err != nil {
			return "", fmt.Errorf("Could not find %s under directory %s", bin, absPath)
		}
		return filepath.Join(absPath, bin), nil
	}

	path, err := filepath.Abs(filepath.Dir(os.Args[0]))
	if err != nil {
		return "", fmt.Errorf("Could not find absolute path of directory containing the tests %s", filepath.Dir(os.Args[0]))
	}
	if _, err := os.Stat(filepath.Join(path, bin)); err == nil {
		return filepath.Join(path, bin), nil
	}

	buildOutputDir, err := utils.GetK8sBuildOutputDir(IsDockerizedBuild(), GetTargetBuildArch())
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(filepath.Join(buildOutputDir, bin)); err == nil {
		return filepath.Join(buildOutputDir, bin), nil
	}

	// Give up with error
	return "", fmt.Errorf("unable to locate %s, Can be defined using --k8s-path", bin)
}

// GetKubeletServerBin returns the path of kubelet binary.
func GetKubeletServerBin() string {
	var kubeletServer string

	switch runtime.GOOS {
	case "windows":
		kubeletServer = "kubelet.exe"
	default:
		kubeletServer = "kubelet"
	}

	bin, err := getK8sBin(kubeletServer)
	if err != nil {
		klog.Fatalf("Could not locate kubelet binary %v.", err)
	}
	return bin
}
