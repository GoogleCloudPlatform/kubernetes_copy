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
	"net/http"
	"os"
	"os/exec"
	"path"
	"strconv"
	"strings"
	"time"

	"k8s.io/klog/v2"

	"k8s.io/kubernetes/test/e2e/framework"
)

var serverStartTimeout = flag.Duration("server-start-timeout", time.Second*120, "Time to wait for each server to become healthy.")

// A server manages a separate server process started and killed with
// commands.
type server struct {
	// name is the name of the server, it is only used for logging.
	name string
	// startCommand is the command used to start the server
	startCommand *exec.Cmd
	// killCommand is the command used to stop the server. It is not required. If it
	// is not specified, `kill` will be used to stop the server.
	killCommand *exec.Cmd
	// restartCommand is the command used to restart the server. If provided, it will be used
	// instead of startCommand when restarting the server.
	restartCommand *exec.Cmd
	// healthCheckUrls is the urls used to check whether the server is ready.
	healthCheckUrls []string
	// outFilename is the name of the log file. The stdout and stderr of the server
	// will be redirected to this file.
	outFilename string
	// monitorParent determines whether the server should watch its parent process and exit
	// if its parent is gone.
	monitorParent bool
	// restartOnExit determines whether a restart loop is launched with the server
	restartOnExit bool
	// Writing to this channel, if it is not nil, stops the restart loop.
	// When tearing down a server, you should check for this channel and write to it if it exists.
	stopRestartingCh chan<- bool
	// Read from this to confirm that the restart loop has stopped.
	ackStopRestartingCh <-chan bool
	// The systemd unit name for the service if it exists. If server is not managed by systemd, field is empty.
	systemdUnitName string
}

// newServer returns a new server with the given name, commands, health check
// URLs, etc.
func newServer(name string, start, kill, restart *exec.Cmd, urls []string, outputFileName string, monitorParent, restartOnExit bool, systemdUnitName string) *server {
	return &server{
		name:            name,
		startCommand:    start,
		killCommand:     kill,
		restartCommand:  restart,
		healthCheckUrls: urls,
		outFilename:     outputFileName,
		monitorParent:   monitorParent,
		restartOnExit:   restartOnExit,
		systemdUnitName: systemdUnitName,
	}
}

// commandToString format command to string.
func commandToString(c *exec.Cmd) string {
	if c == nil {
		return ""
	}
	return strings.Join(append([]string{c.Path}, c.Args[1:]...), " ")
}

func (s *server) String() string {
	return fmt.Sprintf("server %q start-command: `%s`, kill-command: `%s`, restart-command: `%s`, health-check: %v, output-file: %q", s.name,
		commandToString(s.startCommand), commandToString(s.killCommand), commandToString(s.restartCommand), s.healthCheckUrls, s.outFilename)
}

// start starts the server by running its commands, monitors it with a health
// check, and ensures that it is restarted if applicable.
//
// Note: restartOnExit == true requires len(s.healthCheckUrls) > 0 to work properly.
func (s *server) start() error {
	klog.Infof("Starting server %q with command %q", s.name, commandToString(s.startCommand))
	errCh := make(chan error)

	// Set up restart channels if the server is configured for restart on exit.
	var stopRestartingCh, ackStopRestartingCh chan bool
	if s.restartOnExit {
		if len(s.healthCheckUrls) == 0 {
			return fmt.Errorf("tried to start %s which has s.restartOnExit == true, but no health check urls provided", s)
		}

		stopRestartingCh = make(chan bool)
		ackStopRestartingCh = make(chan bool)

		s.stopRestartingCh = stopRestartingCh
		s.ackStopRestartingCh = ackStopRestartingCh
	}

	// This goroutine actually runs the start command for the server.
	go func() {
		defer close(errCh)

		// Create the output filename
		outPath := path.Join(framework.TestContext.ReportDir, s.outFilename)
		outfile, err := os.Create(outPath)
		if err != nil {
			errCh <- fmt.Errorf("failed to create file %q for `%s` %v", outPath, s, err)
			return
		}
		klog.Infof("Output file for server %q: %v", s.name, outfile.Name())
		defer outfile.Close()
		defer outfile.Sync()

		// Set the command to write the output file
		s.startCommand.Stdout = outfile
		s.startCommand.Stderr = outfile

		// Start the server
		err = startProcess(s.startCommand, s.monitorParent)
		if err != nil {
			errCh <- fmt.Errorf("failed to run %s: %w", s, err)
			return
		}
		if !s.restartOnExit {
			klog.Infof("Waiting for server %q start command to complete", s.name)
			// If we aren't planning on restarting, ok to Wait() here to release resources.
			// Otherwise, we Wait() in the restart loop.
			err = s.startCommand.Wait()
			if err != nil {
				errCh <- fmt.Errorf("failed to run start command for server %q: %w", s.name, err)
				return
			}
		} else {
			usedStartCmd := true
			for {
				klog.Infof("Running health check for service %q", s.name)
				// Wait for an initial health check to pass, so that we are sure the server started.
				err := readinessCheck(s.name, s.healthCheckUrls, nil)
				if err != nil {
					if usedStartCmd {
						klog.Infof("Waiting for server %q start command to complete after initial health check failed", s.name)
						s.startCommand.Wait() // Release resources if necessary.
					}
					// This should not happen, immediately stop the e2eService process.
					klog.Fatalf("Restart loop readinessCheck failed for %q", s.name)
				} else {
					klog.Infof("Initial health check passed for service %q", s.name)
				}

				// Initial health check passed, wait until a health check fails again.
			stillAlive:
				for {
					select {
					case <-stopRestartingCh:
						ackStopRestartingCh <- true
						return
					case <-time.After(time.Second):
						for _, url := range s.healthCheckUrls {
							resp, err := http.Head(url)
							if err != nil || resp.StatusCode != http.StatusOK {
								break stillAlive
							}
						}
					}
				}

				if usedStartCmd {
					s.startCommand.Wait() // Release resources from last cmd
					usedStartCmd = false
				}
				if s.restartCommand != nil {
					// Always make a fresh copy of restartCommand before
					// running, we may have to restart multiple times
					s.restartCommand = &exec.Cmd{
						Path:        s.restartCommand.Path,
						Args:        s.restartCommand.Args,
						Env:         s.restartCommand.Env,
						Dir:         s.restartCommand.Dir,
						Stdin:       s.restartCommand.Stdin,
						Stdout:      s.restartCommand.Stdout,
						Stderr:      s.restartCommand.Stderr,
						ExtraFiles:  s.restartCommand.ExtraFiles,
						SysProcAttr: s.restartCommand.SysProcAttr,
					}
					// Run and wait for exit. This command is assumed to have
					// short duration, e.g. systemctl restart
					klog.Infof("Restarting server %q with restart command", s.name)
					err = s.restartCommand.Run()
					if err != nil {
						// This should not happen, immediately stop the e2eService process.
						klog.Fatalf("Restarting server %s with restartCommand failed. Error: %v.", s, err)
					}
				} else {
					s.startCommand = &exec.Cmd{
						Path:        s.startCommand.Path,
						Args:        s.startCommand.Args,
						Env:         s.startCommand.Env,
						Dir:         s.startCommand.Dir,
						Stdin:       s.startCommand.Stdin,
						Stdout:      s.startCommand.Stdout,
						Stderr:      s.startCommand.Stderr,
						ExtraFiles:  s.startCommand.ExtraFiles,
						SysProcAttr: s.startCommand.SysProcAttr,
					}
					klog.Infof("Restarting server %q with start command", s.name)
					err = s.startCommand.Start()
					usedStartCmd = true
					if err != nil {
						// This should not happen, immediately stop the e2eService process.
						klog.Fatalf("Restarting server %s with startCommand failed. Error: %v.", s, err)
					}
				}
			}
		}
	}()

	return readinessCheck(s.name, s.healthCheckUrls, errCh)
}

// kill runs the server's kill command.
func (s *server) kill() error {
	klog.Infof("Kill server %q", s.name)
	name := s.name
	cmd := s.startCommand

	// If s has a restart loop, turn it off.
	if s.restartOnExit {
		s.stopRestartingCh <- true
		<-s.ackStopRestartingCh
	}

	if s.killCommand != nil {
		return s.killCommand.Run()
	}

	if cmd == nil {
		return fmt.Errorf("could not kill %q because both `killCommand` and `startCommand` are nil", name)
	}

	if cmd.Process == nil {
		klog.V(2).Infof("%q not running", name)
		return nil
	}
	pid := cmd.Process.Pid
	if pid <= 1 {
		return fmt.Errorf("invalid PID %d for %q", pid, name)
	}

	// Attempt to shut down the process in a friendly manner before forcing it.
	waitChan := make(chan error)
	go func() {
		_, err := cmd.Process.Wait()
		waitChan <- err
		close(waitChan)
	}()

	const timeout = 10 * time.Second
	for _, signal := range []string{"-TERM", "-KILL"} {
		klog.V(2).Infof("Killing process %d (%s) with %s", pid, name, signal)
		cmd := exec.Command("kill", signal, strconv.Itoa(pid))
		_, err := cmd.Output()
		if err != nil {
			klog.Errorf("Error signaling process %d (%s) with %s: %v", pid, name, signal, err)
			continue
		}

		select {
		case err := <-waitChan:
			if err != nil {
				return fmt.Errorf("error stopping %q: %w", name, err)
			}
			// Success!
			return nil
		case <-time.After(timeout):
			// Continue.
		}
	}

	return fmt.Errorf("unable to stop %q", name)
}

func (s *server) stopUnit() error {
	klog.Infof("Stopping systemd unit for server %q with unit name: %q", s.name, s.systemdUnitName)
	if s.systemdUnitName != "" {
		err := exec.Command("sudo", "systemctl", "stop", s.systemdUnitName).Run()
		if err != nil {
			return fmt.Errorf("Failed to stop systemd unit name: %q: %w", s.systemdUnitName, err)
		}
	}
	return nil
}
