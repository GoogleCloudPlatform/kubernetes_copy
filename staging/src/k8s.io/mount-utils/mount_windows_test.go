//go:build windows
// +build windows

/*
Copyright 2017 The Kubernetes Authors.

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

package mount

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	testingexec "k8s.io/utils/exec/testing"
)

func makeLink(link, target string) error {
	if output, err := exec.Command("cmd", "/c", "mklink", "/D", link, target).CombinedOutput(); err != nil {
		return fmt.Errorf("mklink failed: %v, link(%q) target(%q) output: %q", err, link, target, string(output))
	}
	return nil
}

func removeLink(link string) error {
	if output, err := exec.Command("cmd", "/c", "rmdir", link).CombinedOutput(); err != nil {
		return fmt.Errorf("rmdir failed: %v, output: %q", err, string(output))
	}
	return nil
}

func setEquivalent(set1, set2 []string) bool {
	map1 := make(map[string]bool)
	map2 := make(map[string]bool)
	for _, s := range set1 {
		map1[s] = true
	}
	for _, s := range set2 {
		map2[s] = true
	}

	for s := range map1 {
		if !map2[s] {
			return false
		}
	}
	for s := range map2 {
		if !map1[s] {
			return false
		}
	}
	return true
}

// this func must run in admin mode, otherwise it will fail
func TestGetMountRefs(t *testing.T) {
	tests := []struct {
		mountPath    string
		expectedRefs []string
	}{
		{
			mountPath:    `c:\windows`,
			expectedRefs: []string{`c:\windows`},
		},
		{
			mountPath:    `c:\doesnotexist`,
			expectedRefs: []string{},
		},
	}

	mounter := Mounter{"fake/path"}

	for _, test := range tests {
		if refs, err := mounter.GetMountRefs(test.mountPath); err != nil || !setEquivalent(test.expectedRefs, refs) {
			t.Errorf("getMountRefs(%q) = %v, error: %v; expected %v", test.mountPath, refs, err, test.expectedRefs)
		}
	}
}

func TestPathWithinBase(t *testing.T) {
	tests := []struct {
		fullPath       string
		basePath       string
		expectedResult bool
	}{
		{
			fullPath:       `c:\tmp\a\b\c`,
			basePath:       `c:\tmp`,
			expectedResult: true,
		},
		{
			fullPath:       `c:\tmp1`,
			basePath:       `c:\tmp2`,
			expectedResult: false,
		},
		{
			fullPath:       `c:\tmp`,
			basePath:       `c:\tmp`,
			expectedResult: true,
		},
		{
			fullPath:       `c:\tmp`,
			basePath:       `c:\tmp\a\b\c`,
			expectedResult: false,
		},
		{
			fullPath:       `c:\kubelet\pods\uuid\volumes\kubernetes.io~configmap\config\..timestamp\file.txt`,
			basePath:       `c:\kubelet\pods\uuid\volumes\kubernetes.io~configmap\config`,
			expectedResult: true,
		},
	}

	for _, test := range tests {
		result := PathWithinBase(test.fullPath, test.basePath)
		assert.Equal(t, result, test.expectedResult, "Expect result not equal with PathWithinBase(%s, %s) return: %q, expected: %q",
			test.fullPath, test.basePath, result, test.expectedResult)
	}
}

func TestIsLikelyNotMountPoint(t *testing.T) {
	mounter := Mounter{"fake/path"}

	tests := []struct {
		fileName       string
		targetLinkName string
		setUp          func(base, fileName, targetLinkName string) error
		expectedResult bool
		expectError    bool
	}{
		{
			"Dir",
			"",
			func(base, fileName, targetLinkName string) error {
				return os.Mkdir(filepath.Join(base, fileName), 0o750)
			},
			true,
			false,
		},
		{
			"InvalidDir",
			"",
			func(base, fileName, targetLinkName string) error {
				return nil
			},
			true,
			true,
		},
		{
			"ValidSymLink",
			"targetSymLink",
			func(base, fileName, targetLinkName string) error {
				targeLinkPath := filepath.Join(base, targetLinkName)
				if err := os.Mkdir(targeLinkPath, 0o750); err != nil {
					return err
				}

				filePath := filepath.Join(base, fileName)
				if err := makeLink(filePath, targeLinkPath); err != nil {
					return err
				}
				return nil
			},
			false,
			false,
		},
		{
			"InvalidSymLink",
			"targetSymLink2",
			func(base, fileName, targetLinkName string) error {
				targeLinkPath := filepath.Join(base, targetLinkName)
				if err := os.Mkdir(targeLinkPath, 0o750); err != nil {
					return err
				}

				filePath := filepath.Join(base, fileName)
				if err := makeLink(filePath, targeLinkPath); err != nil {
					return err
				}
				return removeLink(targeLinkPath)
			},
			false,
			false,
		},
		{
			"junction",
			"targetDir",
			func(base, fileName, targetLinkName string) error {
				target := filepath.Join(base, targetLinkName)
				if err := os.Mkdir(target, 0o750); err != nil {
					return err
				}

				// create a Junction file type on Windows
				junction := filepath.Join(base, fileName)
				if output, err := exec.Command("cmd", "/c", "mklink", "/J", junction, target).CombinedOutput(); err != nil {
					return fmt.Errorf("mklink failed: %v, link(%q) target(%q) output: %q", err, junction, target, string(output))
				}
				return nil
			},
			false,
			false,
		},
	}

	for _, test := range tests {
		base := t.TempDir()

		if err := test.setUp(base, test.fileName, test.targetLinkName); err != nil {
			t.Fatalf("unexpected error in setUp(%s, %s): %v", test.fileName, test.targetLinkName, err)
		}

		filePath := filepath.Join(base, test.fileName)
		result, err := mounter.IsLikelyNotMountPoint(filePath)
		assert.Equal(t, test.expectedResult, result, "Expect result not equal with IsLikelyNotMountPoint(%s) return: %q, expected: %q",
			filePath, result, test.expectedResult)

		if test.expectError {
			assert.NotNil(t, err, "Expect error during IsLikelyNotMountPoint(%s)", filePath)
		} else {
			assert.Nil(t, err, "Expect error is nil during IsLikelyNotMountPoint(%s)", filePath)
		}
	}
}

func TestFormatAndMount(t *testing.T) {
	tests := []struct {
		device       string
		target       string
		fstype       string
		execScripts  []ExecArgs
		mountOptions []string
		expectError  bool
	}{
		{
			device: "0",
			target: "disk",
			fstype: "NTFS",
			execScripts: []ExecArgs{
				{"powershell", []string{"/c", "Get-Disk", "-Number"}, "0", nil},
				{"powershell", []string{"/c", "Get-Partition", "-DiskNumber"}, "0", nil},
				{"cmd", []string{"/c", "mklink", "/D"}, "", nil},
			},
			mountOptions: []string{},
			expectError:  false,
		},
		{
			device: "0",
			target: "disk",
			fstype: "",
			execScripts: []ExecArgs{
				{"powershell", []string{"/c", "Get-Disk", "-Number"}, "0", nil},
				{"powershell", []string{"/c", "Get-Partition", "-DiskNumber"}, "0", nil},
				{"cmd", []string{"/c", "mklink", "/D"}, "", nil},
			},
			mountOptions: []string{},
			expectError:  false,
		},
		{
			device:       "invalidDevice",
			target:       "disk",
			fstype:       "NTFS",
			mountOptions: []string{},
			expectError:  true,
		},
	}

	for _, test := range tests {
		fakeMounter := ErrorMounter{NewFakeMounter(nil), 0, nil}
		fakeExec := &testingexec.FakeExec{}
		for _, script := range test.execScripts {
			fakeCmd := &testingexec.FakeCmd{}
			cmdAction := makeFakeCmd(fakeCmd, script.command, script.args...)
			outputAction := makeFakeOutput(script.output, script.err)
			fakeCmd.CombinedOutputScript = append(fakeCmd.CombinedOutputScript, outputAction)
			fakeExec.CommandScript = append(fakeExec.CommandScript, cmdAction)
		}
		mounter := SafeFormatAndMount{
			Interface: &fakeMounter,
			Exec:      fakeExec,
		}
		target := filepath.Join(t.TempDir(), test.target)
		err := mounter.FormatAndMount(test.device, target, test.fstype, test.mountOptions)
		if test.expectError {
			assert.NotNil(t, err, "Expect error during FormatAndMount(%s, %s, %s, %v)", test.device, test.target, test.fstype, test.mountOptions)
		} else {
			assert.Nil(t, err, "Expect error is nil during FormatAndMount(%s, %s, %s, %v)", test.device, test.target, test.fstype, test.mountOptions)
		}
	}
}

func TestNewSMBMapping(t *testing.T) {
	tests := []struct {
		username    string
		password    string
		remotepath  string
		expectError bool
	}{
		{
			"",
			"password",
			`\\remotepath`,
			true,
		},
		{
			"username",
			"",
			`\\remotepath`,
			true,
		},
		{
			"username",
			"password",
			"",
			true,
		},
	}

	for _, test := range tests {
		_, err := newSMBMapping(test.username, test.password, test.remotepath)
		if test.expectError {
			assert.NotNil(t, err, "Expect error during newSMBMapping(%s, %s, %s, %v)", test.username, test.password, test.remotepath)
		} else {
			assert.Nil(t, err, "Expect error is nil during newSMBMapping(%s, %s, %s, %v)", test.username, test.password, test.remotepath)
		}
	}
}

func TestIsValidPath(t *testing.T) {
	tests := []struct {
		remotepath     string
		expectedResult bool
		expectError    bool
	}{
		{
			"c:",
			true,
			false,
		},
		{
			"invalid-path",
			false,
			false,
		},
	}

	for _, test := range tests {
		result, err := isValidPath(test.remotepath)
		assert.Equal(t, result, test.expectedResult, "Expect result not equal with isValidPath(%s) return: %q, expected: %q, error: %v",
			test.remotepath, result, test.expectedResult, err)
		if test.expectError {
			assert.NotNil(t, err, "Expect error during isValidPath(%s)", test.remotepath)
		} else {
			assert.Nil(t, err, "Expect error is nil during isValidPath(%s)", test.remotepath)
		}
	}
}

func TestIsAccessDeniedError(t *testing.T) {
	tests := []struct {
		err            error
		expectedResult bool
	}{
		{
			nil,
			false,
		},
		{
			fmt.Errorf("other error message"),
			false,
		},
		{
			fmt.Errorf(`PathValid(\\xxx\share) failed with returned output: Test-Path : Access is denied`),
			true,
		},
	}

	for _, test := range tests {
		result := isAccessDeniedError(test.err)
		assert.Equal(t, result, test.expectedResult, "Expect result not equal with isAccessDeniedError(%v) return: %q, expected: %q",
			test.err, result, test.expectedResult)
	}
}
