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

package volumepathhandler

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMapDevice(t *testing.T) {
	tempDir := t.TempDir()
	testCases := []struct {
		name        string
		devicePath  string
		mapPath     string
		linkName    string
		bindMount   bool
		expectError string
	}{
		{
			name:        "valid symlink map",
			devicePath:  "/dev/fakeDevice",
			mapPath:     filepath.Join(tempDir, "test-map-valid"),
			linkName:    "validLink",
			bindMount:   false,
			expectError: "",
		},
		{
			name:        "empty device path",
			devicePath:  "",
			mapPath:     filepath.Join(tempDir, "test-map-empty-device"),
			linkName:    "emptyDeviceLink",
			bindMount:   false,
			expectError: "failed to map device to map path. devicePath is empty",
		},
		{
			name:        "empty map path",
			devicePath:  "/dev/fakeDevice",
			mapPath:     "",
			linkName:    "emptyMapLink",
			bindMount:   false,
			expectError: "failed to map device to map path. mapPath is empty",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.mapPath != "" {
				if err := os.MkdirAll(tc.mapPath, 0750); err != nil {
					t.Fatalf("failed to create mapPath: %v", err)
				}
			}
			handler := VolumePathHandler{}
			err := handler.MapDevice(tc.devicePath, tc.mapPath, tc.linkName, tc.bindMount)

			if tc.expectError != "" {
				if err == nil || err.Error() != tc.expectError {
					t.Fatalf("expected error: %v, got: %v", tc.expectError, err)
				}
			} else {
				if err != nil {
					t.Fatalf("expected no error, got: %v", err)
				}
				linkPath := filepath.Join(tc.mapPath, tc.linkName)
				if _, err := os.Lstat(linkPath); os.IsNotExist(err) {
					t.Fatalf("expected symlink to exist, got error: %v", err)
				}
			}
		})
	}
}

func TestUnmapDevice(t *testing.T) {
	tempDir := t.TempDir()
	testCases := []struct {
		name        string
		mapPath     string
		linkName    string
		bindMount   bool
		setup       func(string, string)
		expectError string
	}{
		{
			name:      "valid symlink unmap",
			mapPath:   filepath.Join(tempDir, "test-unmap-valid"),
			linkName:  "validLink",
			bindMount: false,
			setup: func(mapPath, linkName string) {
				if err := os.MkdirAll(mapPath, 0750); err != nil {
					t.Fatalf("failed to create mapPath in setup: %v", err)
				}
				if err := os.Symlink("/dev/fakeDevice", filepath.Join(mapPath, linkName)); err != nil {
					t.Fatalf("failed to create symlink in setup: %v", err)
				}
			},
			expectError: "",
		},
		{
			name:      "symlink does not exist",
			mapPath:   filepath.Join(tempDir, "test-unmap-nonexistent"),
			linkName:  "nonexistentLink",
			bindMount: false,
			setup: func(mapPath, linkName string) {
				if err := os.MkdirAll(mapPath, 0750); err != nil {
					t.Fatalf("failed to create mapPath in setup: %v", err)
				}
			},
			expectError: "",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			handler := VolumePathHandler{}
			if tc.setup != nil {
				tc.setup(tc.mapPath, tc.linkName)
			}

			err := handler.UnmapDevice(tc.mapPath, tc.linkName, tc.bindMount)
			if tc.expectError != "" {
				if err == nil || err.Error() != tc.expectError {
					t.Fatalf("expected error: %v, got: %v", tc.expectError, err)
				}
			} else {
				if err != nil {
					t.Fatalf("expected no error, got: %v", err)
				}
				linkPath := filepath.Join(tc.mapPath, tc.linkName)
				if _, err := os.Lstat(linkPath); !os.IsNotExist(err) {
					t.Fatalf("expected symlink to be removed, got error: %v", err)
				}
			}
		})
	}
}

func TestRemoveMapPath(t *testing.T) {
	tempDir := t.TempDir()
	mapPath := filepath.Join(tempDir, "test-remove-map-path")

	if err := os.MkdirAll(mapPath, 0755); err != nil {
		t.Fatalf("failed to create map path: %v", err)
	}

	testCases := []struct {
		name        string
		mapPath     string
		expectError string
	}{
		{
			name:        "Remove Existing Map Path",
			mapPath:     mapPath,
			expectError: "",
		},
		{
			name:        "Remove Non-existing Map Path",
			mapPath:     filepath.Join(tempDir, "non-existing-path"),
			expectError: "",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			handler := VolumePathHandler{}
			err := handler.RemoveMapPath(tc.mapPath)
			if tc.expectError != "" {
				if err == nil || err.Error() != tc.expectError {
					t.Fatalf("expected error: %v, got: %v", tc.expectError, err)
				}
			} else {
				if err != nil {
					t.Fatalf("expected no error, got: %v", err)
				}
				if _, err := os.Stat(tc.mapPath); !os.IsNotExist(err) {
					t.Fatalf("expected map path to be deleted, got error: %v", err)
				}
			}
		})
	}
}

func TestIsSymlinkExist(t *testing.T) {
	tempDir := t.TempDir()
	mapPath := filepath.Join(tempDir, "test-symlink")
	linkName := "test-symlink-link"

	if err := os.MkdirAll(mapPath, 0755); err != nil {
		t.Fatalf("failed to create test directory: %v", err)
	}

	if err := os.Symlink("/dev/fakeDevice", filepath.Join(mapPath, linkName)); err != nil {
		t.Fatalf("failed to create symlink: %v", err)
	}

	testCases := []struct {
		name           string
		mapPath        string
		expectedExists bool
	}{
		{
			name:           "Check Existing Symlink",
			mapPath:        filepath.Join(mapPath, linkName),
			expectedExists: true,
		},
		{
			name:           "Check Non-existing Symlink",
			mapPath:        filepath.Join(mapPath, "non-existing-symlink"),
			expectedExists: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			exists, err := VolumePathHandler{}.IsSymlinkExist(tc.mapPath)
			if err != nil {
				t.Fatalf("expected no error, got: %v", err)
			}
			if exists != tc.expectedExists {
				t.Fatalf("expected symlink existence to be: %v, got: %v", tc.expectedExists, exists)
			}
		})
	}
}
