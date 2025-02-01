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
	"context"
	"fmt"
	"os"
	"os/exec"
	"path"
	"testing"

	"k8s.io/klog/v2"

	"k8s.io/kubernetes/test/e2e/framework"
)

// E2EServices starts and stops e2e services in a separate process. The test
// uses it to start and stop all e2e services.
type E2EServices struct {
	// monitorParent determines whether the sub-processes should watch and die with the current
	// process.
	rmDirs        []string
	monitorParent bool
	services      *server
	kubelet       *server
	logs          logFiles
}

// NewE2EServices returns a new E2EServices instance.
func NewE2EServices(monitorParent bool) *E2EServices {
	return &E2EServices{
		monitorParent: monitorParent,
		// Special log files that need to be collected for additional debugging.
		logs: getLogFiles(),
	}
}

// Start starts the e2e services in another process by calling back into the
// test binary.  Returns when all e2e services are ready or an error.
//
// We want to statically link e2e services into the test binary, but we don't
// want their glog output to pollute the test result. So we run the binary in
// run-services-mode to start e2e services in another process.
// The function starts 2 processes:
// * internal e2e services: services which statically linked in the test binary - apiserver, etcd and
// namespace controller.
// * kubelet: kubelet binary is outside. (We plan to move main kubelet start logic out when we have
// standard kubelet launcher)
func (e *E2EServices) Start(ctx context.Context, featureGates map[string]bool) error {
	var err error
	if e.services, err = e.startInternalServices(); err != nil {
		return fmt.Errorf("failed to start internal services: %w", err)
	}
	klog.Infof("Node services started.")
	// running the kubelet depends on whether we are running conformance test-suite
	if framework.TestContext.NodeConformance {
		klog.Info("nothing to do in node-e2e-services, running conformance suite")
	} else {
		// Start kubelet
		e.kubelet, err = e.startKubelet(ctx, featureGates)
		if err != nil {
			return fmt.Errorf("failed to start kubelet: %w", err)
		}
		klog.Infof("Kubelet started.")
	}
	return nil
}

// Stop stops the e2e services.
func (e *E2EServices) Stop() {
	defer func() {
		if !framework.TestContext.NodeConformance {
			// Collect log files.
			e.collectLogFiles()
		}
	}()
	if e.services != nil {
		if err := e.services.kill(); err != nil {
			klog.Errorf("Failed to stop services: %v", err)
		}
	}
	if e.kubelet != nil {
		if err := e.kubelet.kill(); err != nil {
			klog.Errorf("Failed to kill kubelet: %v", err)
		}
		// Stop the kubelet systemd unit which will delete the kubelet transient unit.
		if err := e.kubelet.stopUnit(); err != nil {
			klog.Errorf("Failed to stop kubelet systemd unit: %v", err)
		}
	}
	for _, d := range e.rmDirs {
		err := os.RemoveAll(d)
		if err != nil {
			klog.Errorf("Failed to delete directory %s: %v", d, err)
		}
	}
}

// RunE2EServices actually start the e2e services. This function is used to
// start e2e services in current process. This is only used in run-services-mode.
func RunE2EServices(t *testing.T) {
	e := newE2EServices()
	if err := e.run(t); err != nil {
		klog.Fatalf("Failed to run e2e services: %v", err)
	}
}

const (
	// services.log is the combined log of all services
	servicesLogFile = "services.log"
	// LogVerbosityLevel is consistent with the level used in a cluster e2e test.
	LogVerbosityLevel = "4"
)

// startInternalServices starts the internal services in a separate process.
func (e *E2EServices) startInternalServices() (*server, error) {
	testBin, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("can't get current binary: %w", err)
	}
	// Pass all flags into the child process, so that it will see the same flag set.
	startCmd := exec.Command(testBin,
		append(
			[]string{"--run-services-mode", fmt.Sprintf("--bearer-token=%s", framework.TestContext.BearerToken)},
			os.Args[1:]...,
		)...)
	server := newServer("services", startCmd, nil, nil, getServicesHealthCheckURLs(), servicesLogFile, e.monitorParent, false, "")
	return server, server.start()
}

// collectLogFiles collects logs of interest either via journalctl or by creating sym
// links. Since we scp files from the remote directory, symlinks will be
// treated as normal files and file contents will be copied over.
func (e *E2EServices) collectLogFiles() {
	// Nothing to do if report dir is not specified.
	if framework.TestContext.ReportDir == "" {
		return
	}
	klog.Info("Fetching log files...")
	journaldFound := isJournaldAvailable()
	for targetFileName, log := range e.logs {
		targetLink := path.Join(framework.TestContext.ReportDir, targetFileName)
		if journaldFound {
			// Skip log files that do not have an equivalent in journald-based machines.
			if len(log.JournalctlCommand) == 0 {
				continue
			}
			klog.Infof("Get log file %q with journalctl command %v.", targetFileName, log.JournalctlCommand)
			out, err := exec.Command("journalctl", log.JournalctlCommand...).CombinedOutput()
			if err != nil {
				klog.Errorf("failed to get %q from journald: %v, %v", targetFileName, string(out), err)
			} else {
				if err = os.WriteFile(targetLink, out, 0644); err != nil {
					klog.Errorf("failed to write logs to %q: %v", targetLink, err)
				}
			}
			continue
		}
		for _, file := range log.Files {
			if _, err := os.Stat(file); err != nil {
				// Expected file not found on this distro.
				continue
			}
			if err := copyLogFile(file, targetLink); err != nil {
				klog.Error(err)
			} else {
				break
			}
		}
	}
}

// isJournaldAvailable returns whether the system executing the tests uses
// journald.
func isJournaldAvailable() bool {
	_, err := exec.LookPath("journalctl")
	return err == nil
}

func copyLogFile(src, target string) error {
	// If not a journald based distro, then just symlink files.
	if out, err := exec.Command("cp", src, target).CombinedOutput(); err != nil {
		return fmt.Errorf("failed to copy %q to %q: %v, %v", src, target, out, err)
	}
	if out, err := exec.Command("chmod", "a+r", target).CombinedOutput(); err != nil {
		return fmt.Errorf("failed to make log file %q world readable: %v, %v", target, out, err)
	}
	return nil
}
