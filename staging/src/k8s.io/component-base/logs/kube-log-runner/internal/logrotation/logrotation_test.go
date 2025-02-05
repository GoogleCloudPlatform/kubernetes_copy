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

package logrotation

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLogrotationWrite(t *testing.T) {
	tests := []struct {
		name             string
		enableFlush      bool
		maxSize          int64
		maxAge           time.Duration
		rotationExpected bool
		cleanupExpected  bool
	}{
		{
			name:             "no rotation",
			enableFlush:      false,
			maxSize:          0,
			maxAge:           10 * time.Hour,
			rotationExpected: false,
			cleanupExpected:  false,
		},
		{
			name:             "Rotation with enableFlush, maxSize, no cleanup",
			enableFlush:      true,
			maxSize:          int64(1024 * 1024),
			maxAge:           time.Duration(0),
			rotationExpected: true,
			cleanupExpected:  false,
		},
		{
			name:             "Rotation with maxSize and cleanup with maxAge",
			enableFlush:      false,
			maxSize:          int64(1024 * 1024),
			maxAge:           24 * time.Hour,
			rotationExpected: true,
			cleanupExpected:  true,
		},
		{
			name:             "Rotation with enableFlush, maxSize and cleanup with maxAge",
			enableFlush:      true,
			maxSize:          int64(1024 * 1024),
			maxAge:           24 * time.Hour,
			rotationExpected: true,
			cleanupExpected:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpDir, err := os.MkdirTemp("", "logrotation_test")
			if err != nil {
				t.Fatalf("Failed to create temp dir: %v", err)
			}
			t.Cleanup(func() {
				if err = os.RemoveAll(tmpDir); err != nil {
					t.Errorf("Failed to remove temp directory: %v", err)
				}
			})

			logFilePath := filepath.Join(tmpDir, "test.log")
			enableFlush := tt.enableFlush
			maxSize := tt.maxSize
			maxAge := tt.maxAge

			rotationFile, err := Open(logFilePath, enableFlush, maxSize, maxAge)
			if err != nil {
				t.Fatalf("Failed to open RotationFile: %v", err)
			}

			t.Cleanup(func() {
				if err := rotationFile.Close(); err != nil {
					t.Errorf("Failed to close rotationFile: %v", err)
				}
			})

			testData := []byte("This is a test log entry.")
			n, err := rotationFile.Write(testData)
			if err != nil {
				t.Fatalf("Failed to write to RotationFile: %v", err)
			}
			if n != len(testData) {
				t.Errorf("Expected to write %d bytes, but wrote %d bytes", len(testData), n)
			}

			// Check if data is written to the file
			content, err := os.ReadFile(logFilePath)
			if err != nil {
				t.Fatalf("Failed to read log file: %v", err)
			}
			if string(content) != string(testData) {
				t.Errorf("Expected log file content to be %q, but got %q", string(testData), string(content))
			}

			// Write more data to trigger rotation
			largeData := make([]byte, 1024*1024) // 1 MB
			n, err = rotationFile.Write(largeData)
			if err != nil {
				t.Fatalf("Failed to write large data to RotationFile: %v", err)
			}
			if n != len(largeData) {
				t.Errorf("Expected to write %d bytes, but wrote %d bytes", len(largeData), n)
			}

			// Check if rotation happened
			rotatedFiles, err := filepath.Glob(filepath.Join(tmpDir, "test-*.log"))
			if err != nil {
				t.Fatalf("Failed to list rotated files: %v", err)
			}

			if tt.rotationExpected {
				numberFiles := 1
				if len(rotatedFiles) != numberFiles {
					t.Errorf("Expected %d rotated log files, but found %d", numberFiles, len(rotatedFiles))
				}

				// Check if new log file is created
				newContent, err := os.ReadFile(logFilePath)
				if err != nil {
					t.Fatalf("Failed to read new log file: %v", err)
				}
				// Rotation after the write, new log file should be empty
				if len(newContent) != 0 {
					t.Errorf("Expected new log file content to be 0 bytes, but got %d bytes", len(newContent))
				}

				// Check if content is append to the rotated log file
				content, err := os.ReadFile(rotatedFiles[0])
				if err != nil {
					t.Fatalf("Failed to read log file: %v", err)
				}
				if len(content) != len(testData)+len(largeData) {
					t.Errorf("Expected log file content to be %d bytes, but got %d bytes", len(testData)+len(largeData), len(content))
				}
			} else {
				numberFiles := 0
				if len(rotatedFiles) != numberFiles {
					t.Errorf("Expected %d rotated log files, but found %d", numberFiles, len(rotatedFiles))
				}

				// Check if content is append to the existing log file
				content, err := os.ReadFile(logFilePath)
				if err != nil {
					t.Fatalf("Failed to read log file: %v", err)
				}
				if len(content) != len(testData)+len(largeData) {
					t.Errorf("Expected log file content to be %d bytes, but got %d bytes", len(testData)+len(largeData), len(content))
				}
			}

			// Check if rotated file was able to be cleaned up
			if len(rotatedFiles) != 0 {
				err = os.Chtimes(rotatedFiles[0], time.Now(), time.Now().AddDate(0, 0, -10))
				if err != nil {
					t.Fatalf("Failed to change access time of rotated file: %v", err)
				}
			}

			// Trigger another rotation and check if the old rotated file has been cleaned up
			time.Sleep(1 * time.Second)
			largeData2 := make([]byte, 1024*1025)
			_, err = rotationFile.Write(largeData2)
			if err != nil {
				t.Fatalf("Failed to write to RotationFile: %v", err)
			}
			rotatedFiles2, err := filepath.Glob(filepath.Join(tmpDir, "test-*.log"))
			if err != nil {
				t.Fatalf("Failed to list rotated files: %v", err)
			}

			// Read the content of the log file
			content, err = os.ReadFile(logFilePath)
			if err != nil {
				t.Fatalf("Failed to read log file: %v", err)
			}

			if !tt.rotationExpected {
				if len(rotatedFiles2) != 0 {
					t.Errorf("Expected rotated log files to be 0, but found %d", len(rotatedFiles))
				}
				if len(content) != len(testData)+len(largeData)+len(largeData2) {
					t.Errorf("Expected log file content to be %d bytes, but got %d bytes", len(testData)+len(largeData)+len(largeData2), len(content))
				}
			} else {
				if tt.cleanupExpected {
					// clean up is performed in a async goroutine, wait for it to finish
					end := time.Now().Add(5 * time.Second)
					for time.Now().Before(end) {
						rotatedFiles2, err = filepath.Glob(filepath.Join(tmpDir, "test-*.log"))
						if err != nil {
							t.Fatalf("Failed to list rotated files: %v", err)
						}
						if len(rotatedFiles2) == 1 {
							break
						}
						time.Sleep(10 * time.Millisecond)
					}
					rotatedFiles2, err = filepath.Glob(filepath.Join(tmpDir, "test-*.log"))
					if err != nil {
						t.Fatalf("Failed to list rotated files: %v", err)
					}
					if len(rotatedFiles2) != 1 {
						t.Errorf("Expected rotated log files to be 1, but found %d", len(rotatedFiles2))
					}
					if rotatedFiles[0] == rotatedFiles2[0] {
						t.Errorf("Expected rotated log files to be different, but found same")
					}
				} else if len(rotatedFiles2) != 2 {
					t.Errorf("Expected rotated log files to be 2, but found %d", len(rotatedFiles2))
				}
				// Rotation after the write, new log file should be empty
				if len(content) != 0 {
					t.Errorf("Expected new log file content to be 0 bytes, but got %d bytes", len(content))
				}
			}
		})
	}
}
