/*
Copyright 2020 The Kubernetes Authors.

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

package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/component-base/logs/kube-log-runner/internal/logrotation"
)

type OpenFunc func(filePath string, enableFlush bool, maxSize int64, maxAge time.Duration) (io.WriteCloser, error)

var (
	enableFlush    *bool
	logFilePath    *string
	logFileSize    *resource.QuantityValue
	logFileAge     *time.Duration
	alsoToStdOut   *bool
	redirectStderr *bool
)

func initFlags() {
	logFileSize = &resource.QuantityValue{}
	enableFlush = flag.Bool("enable-flush", false, "enable flush to log file in every 5 seconds")
	logFilePath = flag.String("log-file", "", "If non-empty, save stdout to this file")
	flag.Var(logFileSize, "log-file-size", "useful with log-file, in format of resource.quantity, default value 0, if non-zero, rotate log file when it reaches this size")
	logFileAge = flag.Duration("log-file-age", 0, "useful with log-file-size, in format of timeDuration, if non-zero, remove log files older than this duration")
	alsoToStdOut = flag.Bool("also-stdout", false, "useful with log-file, log to standard output as well as the log file")
	redirectStderr = flag.Bool("redirect-stderr", true, "treat stderr same as stdout")
}

func main() {
	initFlags()
	flag.Parse()

	if err := configureAndRun(logrotation.Open); err != nil {
		log.Fatal(err)
	}
}
func configureAndRun(open OpenFunc) error {
	var (
		outputStream io.Writer = os.Stdout
		errStream    io.Writer = os.Stderr
	)

	args := flag.Args()
	if len(args) == 0 {
		return fmt.Errorf("not enough arguments to run")
	}

	maxSize := logFileSize.Value()
	if maxSize < 0 {
		return fmt.Errorf("log-file-size must be non-negative quantity")
	}

	// Check the time.duration is not negative
	if *logFileAge < 0 {
		return fmt.Errorf("log-file-age must be non-negative")
	}

	if logFilePath != nil && *logFilePath != "" {
		logFile, err := open(*logFilePath, *enableFlush, maxSize, *logFileAge)

		if err != nil {
			return fmt.Errorf("failed to create log file %v: %w", *logFilePath, err)
		}
		if *alsoToStdOut {
			outputStream = io.MultiWriter(os.Stdout, logFile)
		} else {
			outputStream = logFile
		}
	}

	if *redirectStderr {
		errStream = outputStream
	}

	exe := args[0]
	var exeArgs []string
	if len(args) > 1 {
		exeArgs = args[1:]
	}
	cmd := exec.Command(exe, exeArgs...)
	cmd.Stdout = outputStream
	cmd.Stderr = errStream

	log.Printf("Running command:\n%v", cmdInfo(cmd))
	err := cmd.Start()
	if err != nil {
		return fmt.Errorf("starting command: %w", err)
	}

	// Handle signals and shutdown process gracefully.
	go setupSigHandler(cmd.Process)
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("running command: %w", err)
	}
	return nil
}

// cmdInfo generates a useful look at what the command is for printing/debug.
func cmdInfo(cmd *exec.Cmd) string {
	return fmt.Sprintf(
		`Command env: (enable-flush=%v, log-file=%v, log-file-size=%v, log-file-age=%v, also-stdout=%v, redirect-stderr=%v)
Run from directory: %v
Executable path: %v
Args (comma-delimited): %v`, *enableFlush, *logFilePath, logFileSize.Value(), *logFileAge, *alsoToStdOut, *redirectStderr,
		cmd.Dir, cmd.Path, strings.Join(cmd.Args, ","),
	)
}

// setupSigHandler will forward any termination signals to the process
func setupSigHandler(process *os.Process) {
	// terminationSignals are signals that cause the program to exit in the
	// supported platforms (linux, darwin, windows).
	terminationSignals := []os.Signal{syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT}

	c := make(chan os.Signal, 1)
	signal.Notify(c, terminationSignals...)

	// Block until a signal is received.
	log.Println("Now listening for interrupts")
	s := <-c
	log.Printf("Got signal: %v. Sending down to process (PID: %v)", s, process.Pid)
	if err := process.Signal(s); err != nil {
		log.Fatalf("Failed to signal process: %v", err)
	}
	log.Printf("Signalled process %v successfully.", process.Pid)
}
