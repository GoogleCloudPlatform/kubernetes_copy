/*
Copyright 2024 The Kubernetes Authors.

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
	"bytes"
	"flag"
	"os"
	"testing"

	"k8s.io/component-base/logs/kube-log-runner/internal/logrotation"
)

// TestConfigureAndRun tests configureAndRun with various flag inputs.
func TestConfigureAndRun(t *testing.T) {
	tests := []struct {
		name         string
		commandLine  []string
		outputWanted string
	}{
		{
			name:         "No_log-file_flag",
			commandLine:  []string{"echo", "hello"},
			outputWanted: "hello\n",
		},
		{
			name:         "No_log-file_flag_with_extra_other_flags",
			commandLine:  []string{"-enable-flush=true", "-log-file-size=1Gi", "-log-file-age=24h30m", "echo", "hello"},
			outputWanted: "hello\n",
		},
		{
			name:         "log-file_flag_only",
			commandLine:  []string{"-log-file=test.log", "echo", "hello"},
			outputWanted: "filePath: test.log, enableFlush: false, maxSize: 0, maxAge: 0s",
		},
		{
			name:         "log-file_flag_with_enable-flush_flag",
			commandLine:  []string{"-log-file=test.log", "-enable-flush=true", "echo", "hello"},
			outputWanted: "filePath: test.log, enableFlush: true, maxSize: 0, maxAge: 0s",
		},
		{
			name:         "log-file_flag_with_enable-flush_flag_and_log-file-size_flag",
			commandLine:  []string{"-log-file=test.log", "-enable-flush=true", "-log-file-size=15M", "echo", "hello"},
			outputWanted: "filePath: test.log, enableFlush: true, maxSize: 15000000, maxAge: 0s",
		},
		{
			name:         "Invalid_CPU_format_log-file-size_flag",
			commandLine:  []string{"-log-file=test.log", "-enable-flush=true", "-log-file-size=125m", "echo", "hello"},
			outputWanted: "filePath: test.log, enableFlush: true, maxSize: 1, maxAge: 0s",
		},
		{
			name:         "log-file_flag_with_enable-flush_flag_and_log-file-size_flag_and_log-file-age_flag",
			commandLine:  []string{"-log-file=test.log", "-enable-flush=true", "-log-file-size=1Gi", "-log-file-age=24h30m", "echo", "hello"},
			outputWanted: "filePath: test.log, enableFlush: true, maxSize: 1073741824, maxAge: 24h30m0s",
		},
		{
			name:         "log-file_flag_with_enable-flush_flag_and_log-file-size_flag_and_log-file-age_flag_and_also-stdout_flag",
			commandLine:  []string{"-log-file=test.log", "-enable-flush=true", "-log-file-size=1500", "-log-file-age=24h30m", "-also-stdout", "echo", "hello"},
			outputWanted: "filePath: test.log, enableFlush: true, maxSize: 1500, maxAge: 24h30m0shello\n",
		},
		{
			name:         "log-file_flag_with_enable-flush_flag_and_log-file-size_flag_and_log-file-age_flag_and_also-stdout_flag_and_redirect-stderr",
			commandLine:  []string{"-log-file=test.log", "-enable-flush=true", "-log-file-size=1500", "-log-file-age=24h30m", "-also-stdout", "-redirect-stderr=false", "echo", "hello"},
			outputWanted: "filePath: test.log, enableFlush: true, maxSize: 1500, maxAge: 24h30m0shello\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resetFlags()
			err := flag.CommandLine.Parse(tt.commandLine)
			if err != nil {
				t.Fatalf("Failed to parse commandline: %v", err)
				return
			}
			// Capture the log output and compare
			originalStdout := os.Stdout
			reader, writer, err := os.Pipe()
			if err != nil {
				t.Fatalf("Failed to create pipe: %v", err)
				return
			}
			defer func() {
				err = reader.Close()
				if err != nil {
					t.Fatalf("Failed to close pipe reader: %v", err)
					return
				}
			}()

			os.Stdout = writer

			var buf bytes.Buffer

			err = configureAndRun(logrotation.OpenStub)
			if err != nil {
				t.Errorf("Expected no error, got %v", err)
			}

			err = writer.Close()
			if err != nil {
				t.Fatalf("Failed to close pipe writer: %v", err)
				return
			}
			// <-done

			_, err = buf.ReadFrom(reader)
			if err != nil {
				t.Fatalf("Failed to read from pipe: %v", err)
				return
			}
			output := buf.String()
			if output != tt.outputWanted {
				t.Errorf("Expected '%s' but got '%s'", tt.outputWanted, output)
			}

			// Restore original os.Stdout
			os.Stdout = originalStdout
		})
	}
}

func TestNegativeConfigureAndRun(t *testing.T) {
	tests := []struct {
		name        string
		commandLine []string
		errorWanted string
	}{
		{
			name:        "Empty_commandline",
			commandLine: []string{},
			errorWanted: "not enough arguments to run",
		},
		{
			name:        "Invalid_log-file-size_flag",
			commandLine: []string{"-log-file-size=10MM", "test"},
			errorWanted: "invalid value \"10MM\" for flag -log-file-size: unable to parse quantity's suffix",
		},
		{
			name:        "Negative_log-file-size_flag",
			commandLine: []string{"-log-file-size=-10M", "test"},
			errorWanted: "log-file-size must be non-negative quantity",
		},
		{
			name:        "Invalid_log-file-age_flag",
			commandLine: []string{"-log-file-age=-10h", "test"},
			errorWanted: "log-file-age must be non-negative",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resetFlags()
			err := flag.CommandLine.Parse(tt.commandLine)
			if err != nil {
				if err.Error() != tt.errorWanted {
					t.Errorf("Expected error: %v, got: %v", tt.errorWanted, err)
				}
				return
			}
			err = configureAndRun(logrotation.OpenStub)
			if err == nil || err.Error() != tt.errorWanted {
				t.Errorf("Expected error: %v, got: %v", tt.errorWanted, err)
			}
		})
	}
}

func resetFlags() {
	// Reinitialize the flag to the default value
	flag.CommandLine = flag.NewFlagSet(os.Args[0], flag.ContinueOnError)
	initFlags()
}
