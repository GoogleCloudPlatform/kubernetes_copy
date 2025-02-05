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

package remote

import (
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/klog/v2"
)

var testTimeout = flag.Duration("test-timeout", 45*time.Minute, "How long (in golang duration format) to wait for ginkgo tests to complete.")
var resultsDir = flag.String("results-dir", "/tmp/", "Directory to scp test results to.")

const archiveName = "e2e_node_test.tar.gz"

func copyKubeletConfigIfExists(kubeletConfigFile, dstDir string) error {
	srcStat, err := os.Stat(kubeletConfigFile)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		} else {
			return err
		}
	}

	if !srcStat.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", kubeletConfigFile)
	}

	source, err := os.Open(kubeletConfigFile)
	if err != nil {
		return err
	}
	defer source.Close()

	dst := filepath.Join(dstDir, "kubeletconfig.yaml")
	destination, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer destination.Close()

	_, err = io.Copy(destination, source)
	return err
}

// CreateTestArchive creates the archive package for the node e2e test.
func CreateTestArchive(suite TestSuite, systemSpecName, kubeletConfigFile string) (string, error) {
	klog.V(2).Infof("Building archive...")
	tardir, err := os.MkdirTemp("", "node-e2e-archive")
	if err != nil {
		return "", fmt.Errorf("failed to create temporary directory %v", err)
	}
	defer os.RemoveAll(tardir)

	err = copyKubeletConfigIfExists(kubeletConfigFile, tardir)
	if err != nil {
		return "", fmt.Errorf("failed to copy kubelet config: %w", err)
	}

	// Call the suite function to setup the test package.
	err = suite.SetupTestPackage(tardir, systemSpecName)
	if err != nil {
		return "", fmt.Errorf("failed to setup test package %q: %w", tardir, err)
	}

	// Build the tar
	out, err := exec.Command("tar", "-zcvf", archiveName, "-C", tardir, ".").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("failed to build tar %v.  Output:\n%s", err, out)
	}

	dir, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("failed to get working directory %v", err)
	}
	return filepath.Join(dir, archiveName), nil
}

// RunRemote returns the command Output, whether the exit was ok, and any errors
type RunRemoteConfig struct {
	Suite                                                                                    TestSuite
	Archive                                                                                  string
	Host                                                                                     string
	Cleanup                                                                                  bool
	ImageDesc, JunitFileName, TestArgs, GinkgoArgs, SystemSpecName, ExtraEnvs, RuntimeConfig string
}

func RunRemote(cfg RunRemoteConfig) (string, bool, error) {
	// Create the temp staging directory
	klog.V(2).Infof("Staging test binaries on %q", cfg.Host)
	workspace := newWorkspaceDir()
	// Do not sudo here, so that we can use scp to copy test archive to the directory.
	if output, err := SSHNoSudo(cfg.Host, "mkdir", workspace); err != nil {
		// Exit failure with the error
		return "", false, fmt.Errorf("failed to create workspace directory %q on Host %q: %v Output: %q", workspace, cfg.Host, err, output)
	}
	if cfg.Cleanup {
		defer func() {
			output, err := SSH(cfg.Host, "rm", "-rf", workspace)
			if err != nil {
				klog.Errorf("failed to cleanup workspace %q on Host %q: %v.  Output:\n%s", workspace, cfg.Host, err, output)
			}
		}()
	}

	// Copy the archive to the staging directory
	if output, err := runSSHCommand(cfg.Host, "scp", cfg.Archive, fmt.Sprintf("%s:%s/", GetHostnameOrIP(cfg.Host), workspace)); err != nil {
		// Exit failure with the error
		return "", false, fmt.Errorf("failed to copy test archive: %v, Output: %q", err, output)
	}

	// Extract the archive
	cmd := getSSHCommand(" && ",
		fmt.Sprintf("cd %s", workspace),
		fmt.Sprintf("tar -xzvf ./%s", archiveName),
	)
	klog.V(2).Infof("Extracting tar on %q", cfg.Host)
	// Do not use sudo here, because `sudo tar -x` will recover the file ownership inside the tar ball, but
	// we want the extracted files to be owned by the current user.
	if output, err := SSHNoSudo(cfg.Host, "sh", "-c", cmd); err != nil {
		// Exit failure with the error
		return "", false, fmt.Errorf("failed to extract test archive: %v, Output: %q", err, output)
	}

	// Create the test result directory.
	resultDir := filepath.Join(workspace, "results")
	if output, err := SSHNoSudo(cfg.Host, "mkdir", resultDir); err != nil {
		// Exit failure with the error
		return "", false, fmt.Errorf("failed to create test result directory %q on Host %q: %v Output: %q", resultDir, cfg.Host, err, output)
	}

	allGinkgoFlags := cfg.GinkgoArgs
	if !strings.Contains(allGinkgoFlags, "-timeout") {
		klog.Warningf("ginkgo flags are missing explicit --timeout (ginkgo defaults to 60 minutes)")
		// see https://github.com/onsi/ginkgo/blob/master/docs/index.md#:~:text=ginkgo%20%2D%2Dtimeout%3Dduration
		// ginkgo suite timeout should be more than the default but less than the
		// full test timeout, so we should use the average of the two.
		suiteTimeout := int(testTimeout.Minutes())
		suiteTimeout = (60 + suiteTimeout) / 2
		allGinkgoFlags = fmt.Sprintf("%s --timeout=%dm", allGinkgoFlags, suiteTimeout)
		klog.Infof("updated ginkgo flags: %s", allGinkgoFlags)
	}

	klog.V(2).Infof("Running test on %q", cfg.Host)
	output, err := cfg.Suite.RunTest(cfg.Host, workspace, resultDir, cfg.ImageDesc, cfg.JunitFileName, cfg.TestArgs,
		allGinkgoFlags, cfg.SystemSpecName, cfg.ExtraEnvs, cfg.RuntimeConfig, *testTimeout)

	var aggErrs []error
	// Do not log the Output here, let the caller deal with the test Output.
	if err != nil {
		aggErrs = append(aggErrs, err)
		collectSystemLog(cfg.Host)
	}

	klog.V(2).Infof("Copying test artifacts from %q", cfg.Host)
	scpErr := getTestArtifacts(cfg.Host, workspace)
	if scpErr != nil {
		aggErrs = append(aggErrs, scpErr)
	}

	return output, len(aggErrs) == 0, utilerrors.NewAggregate(aggErrs)
}

const (
	// workspaceDirPrefix is the string prefix used in the workspace directory name.
	workspaceDirPrefix = "node-e2e-"
	// timestampFormat is the timestamp format used in the node e2e directory name.
	timestampFormat = "20060102T150405"
)

func getTimestamp() string {
	return fmt.Sprint(time.Now().Format(timestampFormat))
}

func newWorkspaceDir() string {
	return filepath.Join("/tmp", workspaceDirPrefix+getTimestamp())
}

// GetTimestampFromWorkspaceDir parses the workspace directory name and gets the timestamp part of it.
// This can later be used to name other artifacts (such as the
// kubelet-${instance}.service systemd transient service used to launch
// Kubelet) so that they can be matched to each other.
func GetTimestampFromWorkspaceDir(dir string) string {
	dirTimestamp := strings.TrimPrefix(filepath.Base(dir), workspaceDirPrefix)
	re := regexp.MustCompile("^\\d{8}T\\d{6}$")
	if re.MatchString(dirTimestamp) {
		return dirTimestamp
	}
	// Fallback: if we can't find that timestamp, default to using Now()
	return getTimestamp()
}

func getTestArtifacts(host, testDir string) error {
	logPath := filepath.Join(*resultsDir, host)
	if err := os.MkdirAll(logPath, 0755); err != nil {
		return fmt.Errorf("failed to create log directory %q: %w", logPath, err)
	}
	// Copy logs (if any) to artifacts/hostname
	if _, err := SSH(host, "ls", fmt.Sprintf("%s/results/*.log", testDir)); err == nil {
		if _, err := runSSHCommand(host, "scp", "-r", fmt.Sprintf("%s:%s/results/*.log", GetHostnameOrIP(host), testDir), logPath); err != nil {
			return err
		}
	}
	// Copy json files (if any) to artifacts.
	if _, err := SSH(host, "ls", fmt.Sprintf("%s/results/*.json", testDir)); err == nil {
		if _, err = runSSHCommand(host, "scp", "-r", fmt.Sprintf("%s:%s/results/*.json", GetHostnameOrIP(host), testDir), *resultsDir); err != nil {
			return err
		}
	}
	// Copy junit results (if any) to artifacts
	if _, err := SSH(host, "ls", fmt.Sprintf("%s/results/junit*", testDir)); err == nil {
		// Copy junit (if any) to the top of artifacts
		if _, err = runSSHCommand(host, "scp", fmt.Sprintf("%s:%s/results/junit*", GetHostnameOrIP(host), testDir), *resultsDir); err != nil {
			return err
		}
	}
	// Copy container logs to artifacts/hostname
	if _, err := SSH(host, "chmod", "-R", "a+r", "/var/log/pods"); err == nil {
		if _, err = runSSHCommand(host, "scp", "-r", fmt.Sprintf("%s:/var/log/pods/", GetHostnameOrIP(host)), logPath); err != nil {
			return err
		}
	}
	return nil
}

// collectSystemLog is a temporary hack to collect system log when encountered on
// unexpected error.
func collectSystemLog(host string) {
	// Encountered an unexpected error. The remote test harness may not
	// have finished retrieved and stored all the logs in this case. Try
	// to get some logs for debugging purposes.
	// TODO: This is a best-effort, temporary hack that only works for
	// journald nodes. We should have a more robust way to collect logs.
	var (
		logName  = "system.log"
		logPath  = fmt.Sprintf("/tmp/%s-%s", getTimestamp(), logName)
		destPath = fmt.Sprintf("%s/%s-%s", *resultsDir, host, logName)
	)
	klog.V(2).Infof("Test failed unexpectedly. Attempting to retrieving system logs (only works for nodes with journald)")
	// Try getting the system logs from journald and store it to a file.
	// Don't reuse the original test directory on the remote host because
	// it could've be been removed if the node was rebooted.
	if output, err := SSH(host, "sh", "-c", fmt.Sprintf("'journalctl --system --all > %s'", logPath)); err == nil {
		klog.V(2).Infof("Got the system logs from journald; copying it back...")
		if output, err := runSSHCommand(host, "scp", fmt.Sprintf("%s:%s", GetHostnameOrIP(host), logPath), destPath); err != nil {
			klog.V(2).Infof("Failed to copy the log: err: %v, output: %q", err, output)
		}
	} else {
		klog.V(2).Infof("Failed to run journactl (normal if it doesn't exist on the node): %v, output: %q", err, output)
	}
}

// WriteLog is a temporary function to make it possible to write log
// in the runner. This is used to collect serial console log.
// TODO(random-liu): Use the log-dump script in cluster e2e.
func WriteLog(host, filename, content string) error {
	logPath := filepath.Join(*resultsDir, host)
	if err := os.MkdirAll(logPath, 0755); err != nil {
		return fmt.Errorf("failed to create log directory %q: %w", logPath, err)
	}
	f, err := os.Create(filepath.Join(logPath, filename))
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(content)
	return err
}
