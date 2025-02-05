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

package subpath

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"k8s.io/klog/v2"
	"k8s.io/mount-utils"
)

// MaxPathLength is the maximum length of Windows path. Normally, it is 260, but if long path is enable,
// the max number is 32,767
const MaxPathLength = 32767

type subpath struct{}

// New returns a subpath.Interface for the current system
func New(mount.Interface) Interface {
	return &subpath{}
}

// isDriveLetterPath returns true if the given path is empty or it ends with ":" or ":\" or ":\\"
func isDriveLetterorEmptyPath(path string) bool {
	if path == "" || strings.HasSuffix(path, ":\\\\") || strings.HasSuffix(path, ":") || strings.HasSuffix(path, ":\\") {
		return true
	}
	return false
}

// isVolumePrefix returns true if the given path name starts with "Volume" or volume prefix including
// "\\.\", "\\?\" for device path or "UNC" or "\\" for UNC path. Otherwise, it returns false.
func isDeviceOrUncPath(path string) bool {
	if strings.HasPrefix(path, "Volume") || strings.HasPrefix(path, "\\\\?\\") || strings.HasPrefix(path, "\\\\.\\") || strings.HasPrefix(path, "UNC") {
		return true
	}
	return false
}

// getUpperPath removes the last level of directory.
func getUpperPath(path string) string {
	sep := fmt.Sprintf("%c", filepath.Separator)
	upperpath := strings.TrimSuffix(path, sep)
	return filepath.Dir(upperpath)
}

// Check whether a directory/file is a link type or not
// LinkType could be SymbolicLink, Junction, or HardLink
func isLinkPath(path string) (bool, error) {
	cmd := exec.Command("powershell", "/c", "$ErrorActionPreference = 'Stop'; (Get-Item -Force -LiteralPath $env:linkpath).LinkType")
	cmd.Env = append(os.Environ(), fmt.Sprintf("linkpath=%s", path))
	klog.V(8).Infof("Executing command: %q", cmd.String())
	output, err := cmd.CombinedOutput()
	if err != nil {
		return false, err
	}
	if strings.TrimSpace(string(output)) != "" {
		return true, nil
	}
	return false, nil
}

// evalSymlink returns the path name after the evaluation of any symbolic links.
// If the path after evaluation is a device path or network connection, the original path is returned
func evalSymlink(path string) (string, error) {
	path = mount.NormalizeWindowsPath(path)
	if isDeviceOrUncPath(path) || isDriveLetterorEmptyPath(path) {
		klog.V(4).Infof("Path '%s' is not a symlink, return its original form.", path)
		return path, nil
	}
	upperpath := path
	base := ""
	for i := 0; i < MaxPathLength; i++ {
		isLink, err := isLinkPath(upperpath)
		if err != nil {
			return "", err
		}
		if isLink {
			break
		}
		// continue to check next layer
		base = filepath.Join(filepath.Base(upperpath), base)
		upperpath = getUpperPath(upperpath)
		if isDriveLetterorEmptyPath(upperpath) {
			klog.V(4).Infof("Path '%s' is not a symlink, return its original form.", path)
			return path, nil
		}
	}
	// This command will give the target path of a given symlink
	// The -Force parameter will allow Get-Item to also evaluate hidden folders, like AppData.
	cmd := exec.Command("powershell", "/c", "$ErrorActionPreference = 'Stop'; (Get-Item -Force -LiteralPath $env:linkpath).Target")
	cmd.Env = append(os.Environ(), fmt.Sprintf("linkpath=%s", upperpath))
	klog.V(8).Infof("Executing command: %q", cmd.String())
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", err
	}
	klog.V(4).Infof("evaluate path %s: symlink from %s to %s", path, upperpath, string(output))
	linkedPath := strings.TrimSpace(string(output))
	if linkedPath == "" || isDeviceOrUncPath(linkedPath) {
		klog.V(4).Infof("Path '%s' has a target %s. Return its original form.", path, linkedPath)
		return path, nil
	}
	// If the target is not an absolute path, join iit with the current upperpath
	if !filepath.IsAbs(linkedPath) {
		linkedPath = filepath.Join(getUpperPath(upperpath), linkedPath)
	}
	nextLink, err := evalSymlink(linkedPath)
	if err != nil {
		return path, err
	}
	return filepath.Join(nextLink, base), nil
}

// check whether hostPath is within volume path
// this func will lock all intermediate subpath directories, need to close handle outside of this func after container started
func lockAndCheckSubPath(volumePath, hostPath string) ([]uintptr, error) {
	if len(volumePath) == 0 || len(hostPath) == 0 {
		return []uintptr{}, nil
	}

	finalSubPath, err := evalSymlink(hostPath)
	if err != nil {
		return []uintptr{}, fmt.Errorf("cannot evaluate link %s: %s", hostPath, err)
	}

	finalVolumePath, err := evalSymlink(volumePath)
	if err != nil {
		return []uintptr{}, fmt.Errorf("cannot read link %s: %s", volumePath, err)
	}

	return lockAndCheckSubPathWithoutSymlink(finalVolumePath, finalSubPath)
}

// lock all intermediate subPath directories and check they are all within volumePath
// volumePath & subPath should not contain any symlink, otherwise it will return error
func lockAndCheckSubPathWithoutSymlink(volumePath, subPath string) ([]uintptr, error) {
	if len(volumePath) == 0 || len(subPath) == 0 {
		return []uintptr{}, nil
	}

	// get relative path to volumePath
	relSubPath, err := filepath.Rel(volumePath, subPath)
	if err != nil {
		return []uintptr{}, fmt.Errorf("Rel(%s, %s) error: %v", volumePath, subPath, err)
	}
	if mount.StartsWithBackstep(relSubPath) {
		return []uintptr{}, fmt.Errorf("SubPath %q not within volume path %q", subPath, volumePath)
	}

	if relSubPath == "." {
		// volumePath and subPath are equal
		return []uintptr{}, nil
	}

	fileHandles := []uintptr{}
	var errorResult error

	currentFullPath := volumePath
	dirs := strings.Split(relSubPath, string(os.PathSeparator))
	for _, dir := range dirs {
		// lock intermediate subPath directory first
		currentFullPath = filepath.Join(currentFullPath, dir)
		handle, err := lockPath(currentFullPath)
		if err != nil {
			errorResult = fmt.Errorf("cannot lock path %s: %s", currentFullPath, err)
			break
		}
		fileHandles = append(fileHandles, handle)

		// make sure intermediate subPath directory does not contain symlink any more
		stat, err := os.Lstat(currentFullPath)
		if err != nil {
			errorResult = fmt.Errorf("Lstat(%q) error: %v", currentFullPath, err)
			break
		}
		if stat.Mode()&os.ModeSymlink != 0 {
			errorResult = fmt.Errorf("subpath %q is an unexpected symlink after EvalSymlinks", currentFullPath)
			break
		}

		// go1.23 behavior change: https://github.com/golang/go/issues/63703#issuecomment-2535941458
		if stat.Mode()&os.ModeIrregular != 0 {
			errorResult = fmt.Errorf("subpath %q is an unexpected symlink after EvalSymlinks", currentFullPath)
			break
		}

		if !mount.PathWithinBase(currentFullPath, volumePath) {
			errorResult = fmt.Errorf("SubPath %q not within volume path %q", currentFullPath, volumePath)
			break
		}
	}

	return fileHandles, errorResult
}

// unlockPath unlock directories
func unlockPath(fileHandles []uintptr) {
	if fileHandles != nil {
		for _, handle := range fileHandles {
			syscall.CloseHandle(syscall.Handle(handle))
		}
	}
}

// lockPath locks a directory or symlink, return handle, exec "syscall.CloseHandle(handle)" to unlock the path
func lockPath(path string) (uintptr, error) {
	if len(path) == 0 {
		return uintptr(syscall.InvalidHandle), syscall.ERROR_FILE_NOT_FOUND
	}
	pathp, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return uintptr(syscall.InvalidHandle), err
	}
	access := uint32(syscall.GENERIC_READ)
	sharemode := uint32(syscall.FILE_SHARE_READ)
	createmode := uint32(syscall.OPEN_EXISTING)
	flags := uint32(syscall.FILE_FLAG_BACKUP_SEMANTICS | syscall.FILE_FLAG_OPEN_REPARSE_POINT)
	fd, err := syscall.CreateFile(pathp, access, sharemode, nil, createmode, flags, 0)
	return uintptr(fd), err
}

// Lock all directories in subPath and check they're not symlinks.
func (sp *subpath) PrepareSafeSubpath(subPath Subpath) (newHostPath string, cleanupAction func(), err error) {
	handles, err := lockAndCheckSubPath(subPath.VolumePath, subPath.Path)

	// Unlock the directories when the container starts
	cleanupAction = func() {
		unlockPath(handles)
	}
	return subPath.Path, cleanupAction, err
}

// No bind-mounts for subpaths are necessary on Windows
func (sp *subpath) CleanSubPaths(podDir string, volumeName string) error {
	return nil
}

// SafeMakeDir makes sure that the created directory does not escape given base directory mis-using symlinks.
func (sp *subpath) SafeMakeDir(subdir string, base string, perm os.FileMode) error {
	realBase, err := evalSymlink(base)
	if err != nil {
		return fmt.Errorf("error resolving symlinks in %s: %s", base, err)
	}

	realFullPath := filepath.Join(realBase, subdir)
	return doSafeMakeDir(realFullPath, realBase, perm)
}

func doSafeMakeDir(pathname string, base string, perm os.FileMode) error {
	klog.V(4).Infof("Creating directory %q within base %q", pathname, base)

	if !mount.PathWithinBase(pathname, base) {
		return fmt.Errorf("path %s is outside of allowed base %s", pathname, base)
	}

	// Quick check if the directory already exists
	s, err := os.Stat(pathname)
	if err == nil {
		// Path exists
		if s.IsDir() {
			// The directory already exists. It can be outside of the parent,
			// but there is no race-proof check.
			klog.V(4).Infof("Directory %s already exists", pathname)
			return nil
		}
		return &os.PathError{Op: "mkdir", Path: pathname, Err: syscall.ENOTDIR}
	}

	// Find all existing directories
	existingPath, toCreate, err := findExistingPrefix(base, pathname)
	if err != nil {
		return fmt.Errorf("error opening directory %s: %s", pathname, err)
	}
	if len(toCreate) == 0 {
		return nil
	}

	// Ensure the existing directory is inside allowed base
	fullExistingPath, err := evalSymlink(existingPath)
	if err != nil {
		return fmt.Errorf("error opening existing directory %s: %s", existingPath, err)
	}
	fullBasePath, err := evalSymlink(base)
	if err != nil {
		return fmt.Errorf("cannot read link %s: %s", base, err)
	}
	if !mount.PathWithinBase(fullExistingPath, fullBasePath) {
		return fmt.Errorf("path %s is outside of allowed base %s", fullExistingPath, err)
	}

	// lock all intermediate directories from fullBasePath to fullExistingPath (top to bottom)
	fileHandles, err := lockAndCheckSubPathWithoutSymlink(fullBasePath, fullExistingPath)
	defer unlockPath(fileHandles)
	if err != nil {
		return err
	}

	klog.V(4).Infof("%q already exists, %q to create", fullExistingPath, filepath.Join(toCreate...))
	currentPath := fullExistingPath
	// create the directories one by one, making sure nobody can change
	// created directory into symlink by lock that directory immediately
	for _, dir := range toCreate {
		currentPath = filepath.Join(currentPath, dir)
		klog.V(4).Infof("Creating %s", dir)
		if err := os.Mkdir(currentPath, perm); err != nil {
			return fmt.Errorf("cannot create directory %s: %s", currentPath, err)
		}
		handle, err := lockPath(currentPath)
		if err != nil {
			return fmt.Errorf("cannot lock path %s: %s", currentPath, err)
		}
		defer syscall.CloseHandle(syscall.Handle(handle))
		// make sure newly created directory does not contain symlink after lock
		stat, err := os.Lstat(currentPath)
		if err != nil {
			return fmt.Errorf("Lstat(%q) error: %v", currentPath, err)
		}
		if stat.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("subpath %q is an unexpected symlink after Mkdir", currentPath)
		}
		// go1.23 behavior change: https://github.com/golang/go/issues/63703#issuecomment-2535941458
		if stat.Mode()&os.ModeIrregular != 0 {
			return fmt.Errorf("subpath %q is an unexpected symlink after Mkdir", currentPath)
		}
	}

	return nil
}

// findExistingPrefix finds prefix of pathname that exists. In addition, it
// returns list of remaining directories that don't exist yet.
func findExistingPrefix(base, pathname string) (string, []string, error) {
	rel, err := filepath.Rel(base, pathname)
	if err != nil {
		return base, nil, err
	}

	if mount.StartsWithBackstep(rel) {
		return base, nil, fmt.Errorf("pathname(%s) is not within base(%s)", pathname, base)
	}

	if rel == "." {
		// base and pathname are equal
		return pathname, []string{}, nil
	}

	dirs := strings.Split(rel, string(filepath.Separator))

	var parent string
	currentPath := base
	for i, dir := range dirs {
		parent = currentPath
		currentPath = filepath.Join(parent, dir)
		if _, err := os.Lstat(currentPath); err != nil {
			if os.IsNotExist(err) {
				return parent, dirs[i:], nil
			}
			return base, nil, err
		}
	}

	return pathname, []string{}, nil
}
