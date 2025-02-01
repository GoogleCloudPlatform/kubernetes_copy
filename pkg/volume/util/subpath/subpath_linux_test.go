//go:build linux
// +build linux

/*
Copyright 2014 The Kubernetes Authors.

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
	"io/fs"
	"io/ioutil"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"syscall"
	"testing"

	"k8s.io/klog/v2"
	"k8s.io/mount-utils"
)

func TestSafeMakeDir(t *testing.T) {
	defaultPerm := os.FileMode(0750) + os.ModeDir
	maxPerm := os.FileMode(0777) + os.ModeDir
	tests := []struct {
		name string
		// Function that prepares directory structure for the test under given
		// base.
		prepare     func(base string) error
		path        string
		checkPath   string
		perm        os.FileMode
		expectError bool
	}{
		{
			"directory-does-not-exist",
			func(base string) error {
				return nil
			},
			"test/directory",
			"test/directory",
			defaultPerm,
			false,
		},
		{
			"all-created-subpath-directory-with-permissions",
			func(base string) error {
				return nil
			},
			"test/directory",
			"test",
			maxPerm,
			false,
		},
		{
			"directory-with-sgid",
			func(base string) error {
				return nil
			},
			"test/directory",
			"test/directory",
			os.FileMode(0777) + os.ModeDir + os.ModeSetgid,
			false,
		},
		{
			"directory-with-suid",
			func(base string) error {
				return nil
			},
			"test/directory",
			"test/directory",
			os.FileMode(0777) + os.ModeDir + os.ModeSetuid,
			false,
		},
		{
			"directory-with-sticky-bit",
			func(base string) error {
				return nil
			},
			"test/directory",
			"test/directory",
			os.FileMode(0777) + os.ModeDir + os.ModeSticky,
			false,
		},
		{
			"directory-exists",
			func(base string) error {
				return os.MkdirAll(filepath.Join(base, "test/directory"), 0750)
			},
			"test/directory",
			"test/directory",
			defaultPerm,
			false,
		},
		{
			"create-base",
			func(base string) error {
				return nil
			},
			"",
			"",
			defaultPerm,
			false,
		},
		{
			"escape-base-using-dots",
			func(base string) error {
				return nil
			},
			"..",
			"",
			defaultPerm,
			true,
		},
		{
			"escape-base-using-dots-2",
			func(base string) error {
				return nil
			},
			"test/../../..",
			"",
			defaultPerm,
			true,
		},
		{
			"follow-symlinks",
			func(base string) error {
				if err := os.MkdirAll(filepath.Join(base, "destination"), defaultPerm); err != nil {
					return err
				}
				return os.Symlink("destination", filepath.Join(base, "test"))
			},
			"test/directory",
			"destination/directory",
			defaultPerm,
			false,
		},
		{
			"follow-symlink-loop",
			func(base string) error {
				return os.Symlink("test", filepath.Join(base, "test"))
			},
			"test/directory",
			"",
			defaultPerm,
			true,
		},
		{
			"follow-symlink-multiple follow",
			func(base string) error {
				/* test1/dir points to test2 and test2/dir points to test1 */
				if err := os.MkdirAll(filepath.Join(base, "test1"), defaultPerm); err != nil {
					return err
				}
				if err := os.MkdirAll(filepath.Join(base, "test2"), defaultPerm); err != nil {
					return err
				}
				if err := os.Symlink(filepath.Join(base, "test2"), filepath.Join(base, "test1/dir")); err != nil {
					return err
				}
				if err := os.Symlink(filepath.Join(base, "test1"), filepath.Join(base, "test2/dir")); err != nil {
					return err
				}
				return nil
			},
			"test1/dir/dir/dir/dir/dir/dir/dir/foo",
			"test2/foo",
			defaultPerm,
			false,
		},
		{
			"danglink-symlink",
			func(base string) error {
				return os.Symlink("non-existing", filepath.Join(base, "test"))
			},
			"test/directory",
			"",
			defaultPerm,
			true,
		},
		{
			"non-directory",
			func(base string) error {
				return ioutil.WriteFile(filepath.Join(base, "test"), []byte{}, defaultPerm)
			},
			"test/directory",
			"",
			defaultPerm,
			true,
		},
		{
			"non-directory-final",
			func(base string) error {
				return ioutil.WriteFile(filepath.Join(base, "test"), []byte{}, defaultPerm)
			},
			"test",
			"",
			defaultPerm,
			true,
		},
		{
			"escape-with-relative-symlink",
			func(base string) error {
				if err := os.MkdirAll(filepath.Join(base, "dir"), defaultPerm); err != nil {
					return err
				}
				if err := os.MkdirAll(filepath.Join(base, "exists"), defaultPerm); err != nil {
					return err
				}
				return os.Symlink("../exists", filepath.Join(base, "dir/test"))
			},
			"dir/test",
			"",
			defaultPerm,
			false,
		},
		{
			"escape-with-relative-symlink-not-exists",
			func(base string) error {
				if err := os.MkdirAll(filepath.Join(base, "dir"), defaultPerm); err != nil {
					return err
				}
				return os.Symlink("../not-exists", filepath.Join(base, "dir/test"))
			},
			"dir/test",
			"",
			defaultPerm,
			true,
		},
		{
			"escape-with-symlink",
			func(base string) error {
				return os.Symlink("/", filepath.Join(base, "test"))
			},
			"test/directory",
			"",
			defaultPerm,
			true,
		},
	}

	for i := range tests {
		test := tests[i]
		t.Run(test.name, func(t *testing.T) {
			base, err := ioutil.TempDir("", "safe-make-dir-"+test.name+"-")
			if err != nil {
				t.Fatal(err.Error())
			}
			defer os.RemoveAll(base)
			test.prepare(base)
			pathToCreate := filepath.Join(base, test.path)
			err = doSafeMakeDir(pathToCreate, base, test.perm)
			if err != nil && !test.expectError {
				t.Fatal(err)
			}
			if err != nil {
				t.Logf("got error: %s", err)
			}
			if err == nil && test.expectError {
				t.Fatalf("expected error, got none")
			}

			if test.checkPath != "" {
				st, err := os.Stat(filepath.Join(base, test.checkPath))
				if err != nil {
					t.Fatalf("cannot read path %s", test.checkPath)
				}
				actualMode := st.Mode()
				if actualMode != test.perm {
					if actualMode^test.perm == os.ModeSetgid && test.perm&os.ModeSetgid == 0 {
						// when TMPDIR is a kubernetes emptydir, the sticky gid bit is set due to fsgroup
						t.Logf("masking bit from %o", actualMode)
					} else {
						t.Errorf("expected permissions %o, got %o (%b)", test.perm, actualMode, test.perm^actualMode)
					}
				}
			}
		})
	}
}

func TestRemoveEmptyDirs(t *testing.T) {
	defaultPerm := os.FileMode(0750)
	tests := []struct {
		name string
		// Function that prepares directory structure for the test under given
		// base.
		prepare func(base string) error
		// Function that validates directory structure after the test
		validate    func(base string) error
		baseDir     string
		endDir      string
		expectError bool
	}{
		{
			name: "all-empty",
			prepare: func(base string) error {
				return os.MkdirAll(filepath.Join(base, "a/b/c"), defaultPerm)
			},
			validate: func(base string) error {
				return validateDirEmpty(filepath.Join(base, "a"))
			},
			baseDir:     "a",
			endDir:      "a/b/c",
			expectError: false,
		},
		{
			name: "dir-not-empty",
			prepare: func(base string) error {
				if err := os.MkdirAll(filepath.Join(base, "a/b/c"), defaultPerm); err != nil {
					return err
				}
				return os.Mkdir(filepath.Join(base, "a/b/d"), defaultPerm)
			},
			validate: func(base string) error {
				if err := validateDirNotExists(filepath.Join(base, "a/b/c")); err != nil {
					return err
				}
				return validateDirExists(filepath.Join(base, "a/b"))
			},
			baseDir:     "a",
			endDir:      "a/b/c",
			expectError: false,
		},
		{
			name: "path-not-within-base",
			prepare: func(base string) error {
				return os.MkdirAll(filepath.Join(base, "a/b/c"), defaultPerm)
			},
			validate: func(base string) error {
				return validateDirExists(filepath.Join(base, "a"))
			},
			baseDir:     "a",
			endDir:      "b/c",
			expectError: true,
		},
		{
			name: "path-already-deleted",
			prepare: func(base string) error {
				return nil
			},
			validate: func(base string) error {
				return nil
			},
			baseDir:     "a",
			endDir:      "a/b/c",
			expectError: false,
		},
		{
			name: "path-not-dir",
			prepare: func(base string) error {
				if err := os.MkdirAll(filepath.Join(base, "a/b"), defaultPerm); err != nil {
					return err
				}
				return ioutil.WriteFile(filepath.Join(base, "a/b", "c"), []byte{}, defaultPerm)
			},
			validate: func(base string) error {
				if err := validateDirExists(filepath.Join(base, "a/b")); err != nil {
					return err
				}
				return validateFileExists(filepath.Join(base, "a/b/c"))
			},
			baseDir:     "a",
			endDir:      "a/b/c",
			expectError: true,
		},
	}

	for _, test := range tests {
		klog.V(4).Infof("test %q", test.name)
		base, err := ioutil.TempDir("", "remove-empty-dirs-"+test.name+"-")
		if err != nil {
			t.Fatal(err.Error())
		}
		if err = test.prepare(base); err != nil {
			os.RemoveAll(base)
			t.Fatalf("failed to prepare test %q: %v", test.name, err.Error())
		}

		err = removeEmptyDirs(filepath.Join(base, test.baseDir), filepath.Join(base, test.endDir))
		if err != nil && !test.expectError {
			t.Errorf("test %q failed: %v", test.name, err)
		}
		if err == nil && test.expectError {
			t.Errorf("test %q failed: expected error, got success", test.name)
		}

		if err = test.validate(base); err != nil {
			t.Errorf("test %q failed validation: %v", test.name, err)
		}

		os.RemoveAll(base)
	}
}

func TestCleanSubPaths(t *testing.T) {
	defaultPerm := os.FileMode(0750)
	testVol := "vol1"

	tests := []struct {
		name string
		// Function that prepares directory structure for the test under given
		// base.
		prepare func(base string) ([]mount.MountPoint, error)
		// Function that validates directory structure after the test
		validate    func(base string) error
		expectError bool
		unmount     func(path string) error
	}{
		{
			name: "not-exists",
			prepare: func(base string) ([]mount.MountPoint, error) {
				return nil, nil
			},
			validate: func(base string) error {
				return nil
			},
			expectError: false,
		},
		{
			name: "subpath-not-mount",
			prepare: func(base string) ([]mount.MountPoint, error) {
				return nil, os.MkdirAll(filepath.Join(base, containerSubPathDirectoryName, testVol, "container1", "0"), defaultPerm)
			},
			validate: func(base string) error {
				return validateDirNotExists(filepath.Join(base, containerSubPathDirectoryName))
			},
			expectError: false,
		},
		{
			name: "subpath-file",
			prepare: func(base string) ([]mount.MountPoint, error) {
				path := filepath.Join(base, containerSubPathDirectoryName, testVol, "container1")
				if err := os.MkdirAll(path, defaultPerm); err != nil {
					return nil, err
				}
				return nil, ioutil.WriteFile(filepath.Join(path, "0"), []byte{}, defaultPerm)
			},
			validate: func(base string) error {
				return validateDirNotExists(filepath.Join(base, containerSubPathDirectoryName))
			},
			expectError: false,
		},
		{
			name: "subpath-container-not-dir",
			prepare: func(base string) ([]mount.MountPoint, error) {
				path := filepath.Join(base, containerSubPathDirectoryName, testVol)
				if err := os.MkdirAll(path, defaultPerm); err != nil {
					return nil, err
				}
				return nil, ioutil.WriteFile(filepath.Join(path, "container1"), []byte{}, defaultPerm)
			},
			validate: func(base string) error {
				return validateDirExists(filepath.Join(base, containerSubPathDirectoryName, testVol))
			},
			expectError: true,
		},
		{
			name: "subpath-multiple-container-not-dir",
			prepare: func(base string) ([]mount.MountPoint, error) {
				path := filepath.Join(base, containerSubPathDirectoryName, testVol)
				if err := os.MkdirAll(filepath.Join(path, "container1"), defaultPerm); err != nil {
					return nil, err
				}
				return nil, ioutil.WriteFile(filepath.Join(path, "container2"), []byte{}, defaultPerm)
			},
			validate: func(base string) error {
				path := filepath.Join(base, containerSubPathDirectoryName, testVol)
				if err := validateDirNotExists(filepath.Join(path, "container1")); err != nil {
					return err
				}
				return validateFileExists(filepath.Join(path, "container2"))
			},
			expectError: true,
		},
		{
			name: "subpath-mount",
			prepare: func(base string) ([]mount.MountPoint, error) {
				path := filepath.Join(base, containerSubPathDirectoryName, testVol, "container1", "0")
				if err := os.MkdirAll(path, defaultPerm); err != nil {
					return nil, err
				}
				mounts := []mount.MountPoint{{Device: "/dev/sdb", Path: path}}
				return mounts, nil
			},
			validate: func(base string) error {
				return validateDirNotExists(filepath.Join(base, containerSubPathDirectoryName))
			},
		},
		{
			name: "subpath-mount-multiple",
			prepare: func(base string) ([]mount.MountPoint, error) {
				path := filepath.Join(base, containerSubPathDirectoryName, testVol, "container1", "0")
				path2 := filepath.Join(base, containerSubPathDirectoryName, testVol, "container1", "1")
				path3 := filepath.Join(base, containerSubPathDirectoryName, testVol, "container2", "1")
				if err := os.MkdirAll(path, defaultPerm); err != nil {
					return nil, err
				}
				if err := os.MkdirAll(path2, defaultPerm); err != nil {
					return nil, err
				}
				if err := os.MkdirAll(path3, defaultPerm); err != nil {
					return nil, err
				}
				mounts := []mount.MountPoint{
					{Device: "/dev/sdb", Path: path},
					{Device: "/dev/sdb", Path: path3},
				}
				return mounts, nil
			},
			validate: func(base string) error {
				return validateDirNotExists(filepath.Join(base, containerSubPathDirectoryName))
			},
		},
		{
			name: "subpath-mount-multiple-vols",
			prepare: func(base string) ([]mount.MountPoint, error) {
				path := filepath.Join(base, containerSubPathDirectoryName, testVol, "container1", "0")
				path2 := filepath.Join(base, containerSubPathDirectoryName, "vol2", "container1", "1")
				if err := os.MkdirAll(path, defaultPerm); err != nil {
					return nil, err
				}
				if err := os.MkdirAll(path2, defaultPerm); err != nil {
					return nil, err
				}
				mounts := []mount.MountPoint{
					{Device: "/dev/sdb", Path: path},
				}
				return mounts, nil
			},
			validate: func(base string) error {
				baseSubdir := filepath.Join(base, containerSubPathDirectoryName)
				if err := validateDirNotExists(filepath.Join(baseSubdir, testVol)); err != nil {
					return err
				}
				return validateDirExists(baseSubdir)
			},
		},
		{
			name: "subpath-with-files",
			prepare: func(base string) ([]mount.MountPoint, error) {
				containerPath := filepath.Join(base, containerSubPathDirectoryName, testVol, "container1")
				if err := os.MkdirAll(containerPath, defaultPerm); err != nil {
					return nil, err
				}

				file0 := filepath.Join(containerPath, "0")
				if err := ioutil.WriteFile(file0, []byte{}, defaultPerm); err != nil {
					return nil, err
				}

				dir1 := filepath.Join(containerPath, "1")
				if err := os.MkdirAll(filepath.Join(dir1, "my-dir-1"), defaultPerm); err != nil {
					return nil, err
				}

				dir2 := filepath.Join(containerPath, "2")
				if err := os.MkdirAll(filepath.Join(dir2, "my-dir-2"), defaultPerm); err != nil {
					return nil, err
				}

				file3 := filepath.Join(containerPath, "3")
				if err := ioutil.WriteFile(file3, []byte{}, defaultPerm); err != nil {
					return nil, err
				}

				mounts := []mount.MountPoint{
					{Device: "/dev/sdb", Path: file0},
					{Device: "/dev/sdc", Path: dir1},
					{Device: "/dev/sdd", Path: dir2},
					{Device: "/dev/sde", Path: file3},
				}
				return mounts, nil
			},
			unmount: func(mountpath string) error {
				err := filepath.WalkDir(mountpath, func(path string, d fs.DirEntry, _ error) error {
					if path == mountpath {
						// Skip top level directory
						return nil
					}

					if err := os.Remove(path); err != nil {
						return err
					}
					return filepath.SkipDir
				})
				if err != nil {
					return fmt.Errorf("error processing %s: %s", mountpath, err)
				}

				return nil
			},
			validate: func(base string) error {
				return validateDirNotExists(filepath.Join(base, containerSubPathDirectoryName))
			},
		},
	}

	for _, test := range tests {
		klog.V(4).Infof("test %q", test.name)
		base, err := ioutil.TempDir("", "clean-subpaths-"+test.name+"-")
		if err != nil {
			t.Fatal(err.Error())
		}
		mounts, err := test.prepare(base)
		if err != nil {
			os.RemoveAll(base)
			t.Fatalf("failed to prepare test %q: %v", test.name, err.Error())
		}

		fm := mount.NewFakeMounter(mounts)
		fm.UnmountFunc = test.unmount

		err = doCleanSubPaths(fm, base, testVol)
		if err != nil && !test.expectError {
			t.Errorf("test %q failed: %v", test.name, err)
		}
		if err == nil && test.expectError {
			t.Errorf("test %q failed: expected error, got success", test.name)
		}
		if err = test.validate(base); err != nil {
			t.Errorf("test %q failed validation: %v", test.name, err)
		}

		os.RemoveAll(base)
	}
}

var (
	testVol       = "vol1"
	testPod       = "pod0"
	testContainer = "container0"
	testSubpath   = 1
)

func setupFakeMounter(testMounts []string) *mount.FakeMounter {
	mounts := []mount.MountPoint{}
	for _, mountPoint := range testMounts {
		mounts = append(mounts, mount.MountPoint{Device: "/foo", Path: mountPoint})
	}
	return mount.NewFakeMounter(mounts)
}

func getTestPaths(base string) (string, string) {
	return filepath.Join(base, testVol),
		filepath.Join(base, testPod, containerSubPathDirectoryName, testVol, testContainer, strconv.Itoa(testSubpath))
}

func TestBindSubPath(t *testing.T) {
	defaultPerm := os.FileMode(0750)

	tests := []struct {
		name string
		// Function that prepares directory structure for the test under given
		// base.
		prepare     func(base string) ([]string, string, string, error)
		expectError bool
	}{
		{
			name: "subpath-dir",
			prepare: func(base string) ([]string, string, string, error) {
				volpath, _ := getTestPaths(base)
				subpath := filepath.Join(volpath, "dir0")
				return nil, volpath, subpath, os.MkdirAll(subpath, defaultPerm)
			},
			expectError: false,
		},
		{
			name: "subpath-dir-symlink",
			prepare: func(base string) ([]string, string, string, error) {
				volpath, _ := getTestPaths(base)
				subpath := filepath.Join(volpath, "dir0")
				if err := os.MkdirAll(subpath, defaultPerm); err != nil {
					return nil, "", "", err
				}
				subpathLink := filepath.Join(volpath, "dirLink")
				return nil, volpath, subpath, os.Symlink(subpath, subpathLink)
			},
			expectError: false,
		},
		{
			name: "subpath-file",
			prepare: func(base string) ([]string, string, string, error) {
				volpath, _ := getTestPaths(base)
				subpath := filepath.Join(volpath, "file0")
				if err := os.MkdirAll(volpath, defaultPerm); err != nil {
					return nil, "", "", err
				}
				return nil, volpath, subpath, ioutil.WriteFile(subpath, []byte{}, defaultPerm)
			},
			expectError: false,
		},
		{
			name: "subpath-not-exists",
			prepare: func(base string) ([]string, string, string, error) {
				volpath, _ := getTestPaths(base)
				subpath := filepath.Join(volpath, "file0")
				return nil, volpath, subpath, nil
			},
			expectError: true,
		},
		{
			name: "subpath-outside",
			prepare: func(base string) ([]string, string, string, error) {
				volpath, _ := getTestPaths(base)
				subpath := filepath.Join(volpath, "dir0")
				if err := os.MkdirAll(volpath, defaultPerm); err != nil {
					return nil, "", "", err
				}
				return nil, volpath, subpath, os.Symlink(base, subpath)
			},
			expectError: true,
		},
		{
			name: "subpath-symlink-child-outside",
			prepare: func(base string) ([]string, string, string, error) {
				volpath, _ := getTestPaths(base)
				subpathDir := filepath.Join(volpath, "dir0")
				subpath := filepath.Join(subpathDir, "child0")
				if err := os.MkdirAll(subpathDir, defaultPerm); err != nil {
					return nil, "", "", err
				}
				return nil, volpath, subpath, os.Symlink(base, subpath)
			},
			expectError: true,
		},
		{
			name: "subpath-child-outside-exists",
			prepare: func(base string) ([]string, string, string, error) {
				volpath, _ := getTestPaths(base)
				subpathDir := filepath.Join(volpath, "dir0")
				child := filepath.Join(base, "child0")
				subpath := filepath.Join(subpathDir, "child0")
				if err := os.MkdirAll(volpath, defaultPerm); err != nil {
					return nil, "", "", err
				}
				// touch file outside
				if err := ioutil.WriteFile(child, []byte{}, defaultPerm); err != nil {
					return nil, "", "", err
				}

				// create symlink for subpath dir
				return nil, volpath, subpath, os.Symlink(base, subpathDir)
			},
			expectError: true,
		},
		{
			name: "subpath-child-outside-not-exists",
			prepare: func(base string) ([]string, string, string, error) {
				volpath, _ := getTestPaths(base)
				subpathDir := filepath.Join(volpath, "dir0")
				subpath := filepath.Join(subpathDir, "child0")
				if err := os.MkdirAll(volpath, defaultPerm); err != nil {
					return nil, "", "", err
				}
				// create symlink for subpath dir
				return nil, volpath, subpath, os.Symlink(base, subpathDir)
			},
			expectError: true,
		},
		{
			name: "subpath-child-outside-exists-middle-dir-symlink",
			prepare: func(base string) ([]string, string, string, error) {
				volpath, _ := getTestPaths(base)
				subpathDir := filepath.Join(volpath, "dir0")
				symlinkDir := filepath.Join(subpathDir, "linkDir0")
				child := filepath.Join(base, "child0")
				subpath := filepath.Join(symlinkDir, "child0")
				if err := os.MkdirAll(subpathDir, defaultPerm); err != nil {
					return nil, "", "", err
				}
				// touch file outside
				if err := ioutil.WriteFile(child, []byte{}, defaultPerm); err != nil {
					return nil, "", "", err
				}

				// create symlink for middle dir
				return nil, volpath, subpath, os.Symlink(base, symlinkDir)
			},
			expectError: true,
		},
		{
			name: "subpath-backstepping",
			prepare: func(base string) ([]string, string, string, error) {
				volpath, _ := getTestPaths(base)
				subpath := filepath.Join(volpath, "dir0")
				symlinkBase := filepath.Join(volpath, "..")
				if err := os.MkdirAll(volpath, defaultPerm); err != nil {
					return nil, "", "", err
				}

				// create symlink for subpath
				return nil, volpath, subpath, os.Symlink(symlinkBase, subpath)
			},
			expectError: true,
		},
		{
			name: "subpath-mountdir-already-exists",
			prepare: func(base string) ([]string, string, string, error) {
				volpath, subpathMount := getTestPaths(base)
				if err := os.MkdirAll(subpathMount, defaultPerm); err != nil {
					return nil, "", "", err
				}

				subpath := filepath.Join(volpath, "dir0")
				return nil, volpath, subpath, os.MkdirAll(subpath, defaultPerm)
			},
			expectError: false,
		},
		{
			name: "subpath-mount-already-exists",
			prepare: func(base string) ([]string, string, string, error) {
				volpath, subpathMount := getTestPaths(base)
				mounts := []string{subpathMount}
				if err := os.MkdirAll(subpathMount, defaultPerm); err != nil {
					return nil, "", "", err
				}

				subpath := filepath.Join(volpath, "dir0")
				return mounts, volpath, subpath, os.MkdirAll(subpath, defaultPerm)
			},
			expectError: false,
		},
		{
			name: "mount-unix-socket",
			prepare: func(base string) ([]string, string, string, error) {
				volpath, subpathMount := getTestPaths(base)
				mounts := []string{subpathMount}
				if err := os.MkdirAll(volpath, defaultPerm); err != nil {
					return nil, "", "", err
				}

				socketFile, socketCreateError := createSocketFile(volpath)

				return mounts, volpath, socketFile, socketCreateError
			},
			expectError: false,
		},
		{
			name: "subpath-mounting-fifo",
			prepare: func(base string) ([]string, string, string, error) {
				volpath, subpathMount := getTestPaths(base)
				mounts := []string{subpathMount}
				if err := os.MkdirAll(volpath, defaultPerm); err != nil {
					return nil, "", "", err
				}

				testFifo := filepath.Join(volpath, "mount_test.fifo")
				err := syscall.Mkfifo(testFifo, 0)
				return mounts, volpath, testFifo, err
			},
			expectError: false,
		},
	}

	for _, test := range tests {
		klog.V(4).Infof("test %q", test.name)
		base, err := ioutil.TempDir("", "bind-subpath-"+test.name+"-")
		if err != nil {
			t.Fatal(err.Error())
		}

		mounts, volPath, subPath, err := test.prepare(base)
		if err != nil {
			os.RemoveAll(base)
			t.Fatalf("failed to prepare test %q: %v", test.name, err.Error())
		}

		fm := setupFakeMounter(mounts)

		subpath := Subpath{
			VolumeMountIndex: testSubpath,
			Path:             subPath,
			VolumeName:       testVol,
			VolumePath:       volPath,
			PodDir:           filepath.Join(base, "pod0"),
			ContainerName:    testContainer,
		}

		_, subpathMount := getTestPaths(base)
		bindPathTarget, err := doBindSubPath(fm, subpath)
		if test.expectError {
			if err == nil {
				t.Errorf("test %q failed: expected error, got success", test.name)
			}
			if bindPathTarget != "" {
				t.Errorf("test %q failed: expected empty bindPathTarget, got %v", test.name, bindPathTarget)
			}
			if err = validateDirNotExists(subpathMount); err != nil {
				t.Errorf("test %q failed: %v", test.name, err)
			}
		}
		if !test.expectError {
			if err != nil {
				t.Errorf("test %q failed: %v", test.name, err)
			}
			if bindPathTarget != subpathMount {
				t.Errorf("test %q failed: expected bindPathTarget %v, got %v", test.name, subpathMount, bindPathTarget)
			}
			if err = validateFileExists(subpathMount); err != nil {
				t.Errorf("test %q failed: %v", test.name, err)
			}
		}

		os.RemoveAll(base)
	}
}

func TestSubpath_PrepareSafeSubpath(t *testing.T) {
	//complete code
	defaultPerm := os.FileMode(0750)

	tests := []struct {
		name string
		// Function that prepares directory structure for the test under given
		// base.
		prepare      func(base string) ([]string, string, string, error)
		expectError  bool
		expectAction []mount.FakeAction
		mountExists  bool
	}{
		{
			name: "subpath-mount-already-exists-with-mismatching-mount",
			prepare: func(base string) ([]string, string, string, error) {
				volpath, subpathMount := getTestPaths(base)
				mounts := []string{subpathMount}
				if err := os.MkdirAll(subpathMount, defaultPerm); err != nil {
					return nil, "", "", err
				}

				subpath := filepath.Join(volpath, "dir0")
				return mounts, volpath, subpath, os.MkdirAll(subpath, defaultPerm)
			},
			expectError:  false,
			expectAction: []mount.FakeAction{{Action: "unmount"}},
			mountExists:  false,
		},
		{
			name: "subpath-mount-already-exists-with-samefile",
			prepare: func(base string) ([]string, string, string, error) {
				volpath, subpathMount := getTestPaths(base)
				mounts := []string{subpathMount}
				subpathMountRoot := filepath.Dir(subpathMount)

				if err := os.MkdirAll(subpathMountRoot, defaultPerm); err != nil {
					return nil, "", "", err
				}
				targetFile, err := os.Create(subpathMount)
				if err != nil {
					return nil, "", "", err
				}
				defer targetFile.Close()

				if err := os.MkdirAll(volpath, defaultPerm); err != nil {
					return nil, "", "", err
				}
				subpath := filepath.Join(volpath, "file0")
				// using hard link to simulate bind mounts
				err = os.Link(subpathMount, subpath)
				if err != nil {
					return nil, "", "", err
				}
				return mounts, volpath, subpath, nil
			},
			expectError:  false,
			expectAction: []mount.FakeAction{},
			mountExists:  true,
		},
	}
	for _, test := range tests {
		klog.V(4).Infof("test %q", test.name)
		base, err := ioutil.TempDir("", "bind-subpath-"+test.name+"-")
		if err != nil {
			t.Fatal(err.Error())
		}
		defer os.RemoveAll(base)

		mounts, volPath, subPath, err := test.prepare(base)
		if err != nil {
			os.RemoveAll(base)
			t.Fatalf("failed to prepare test %q: %v", test.name, err.Error())
		}

		fm := setupFakeMounter(mounts)

		subpath := Subpath{
			VolumeMountIndex: testSubpath,
			Path:             subPath,
			VolumeName:       testVol,
			VolumePath:       volPath,
			PodDir:           filepath.Join(base, "pod0"),
			ContainerName:    testContainer,
		}

		_, subpathMount := getTestPaths(base)
		bindMountExists, bindPathTarget, err := prepareSubpathTarget(fm, subpath)

		if bindMountExists != test.mountExists {
			t.Errorf("test %q failed: expected bindMountExists %v, got %v", test.name, test.mountExists, bindMountExists)
		}

		logActions := fm.GetLog()
		if len(test.expectAction) == 0 && len(logActions) > 0 {
			t.Errorf("test %q failed: expected no actions, got %v", test.name, logActions)
		}

		if len(test.expectAction) > 0 {
			foundMatchingAction := false
			testAction := test.expectAction[0]
			for _, action := range logActions {
				if action.Action == testAction.Action {
					foundMatchingAction = true
					break
				}
			}
			if !foundMatchingAction {
				t.Errorf("test %q failed: expected action %q, got %v", test.name, testAction.Action, logActions)
			}
		}

		if test.expectError {
			if err == nil {
				t.Errorf("test %q failed: expected error, got success", test.name)
			}
			if bindPathTarget != "" {
				t.Errorf("test %q failed: expected empty bindPathTarget, got %v", test.name, bindPathTarget)
			}
			if err = validateDirNotExists(subpathMount); err != nil {
				t.Errorf("test %q failed: %v", test.name, err)
			}
		}
		if !test.expectError {
			if err != nil {
				t.Errorf("test %q failed: %v", test.name, err)
			}
			if bindPathTarget != subpathMount {
				t.Errorf("test %q failed: expected bindPathTarget %v, got %v", test.name, subpathMount, bindPathTarget)
			}
			if err = validateFileExists(subpathMount); err != nil {
				t.Errorf("test %q failed: %v", test.name, err)
			}
		}
	}
}

func TestSafeOpen(t *testing.T) {
	defaultPerm := os.FileMode(0750)

	tests := []struct {
		name string
		// Function that prepares directory structure for the test under given
		// base.
		prepare     func(base string) error
		path        string
		expectError bool
	}{
		{
			"directory-does-not-exist",
			func(base string) error {
				return nil
			},
			"test/directory",
			true,
		},
		{
			"directory-exists",
			func(base string) error {
				return os.MkdirAll(filepath.Join(base, "test/directory"), 0750)
			},
			"test/directory",
			false,
		},
		{
			"escape-base-using-dots",
			func(base string) error {
				return nil
			},
			"..",
			true,
		},
		{
			"escape-base-using-dots-2",
			func(base string) error {
				return os.MkdirAll(filepath.Join(base, "test"), 0750)
			},
			"test/../../..",
			true,
		},
		{
			"symlink",
			func(base string) error {
				if err := os.MkdirAll(filepath.Join(base, "destination"), defaultPerm); err != nil {
					return err
				}
				return os.Symlink("destination", filepath.Join(base, "test"))
			},
			"test",
			true,
		},
		{
			"symlink-nested",
			func(base string) error {
				if err := os.MkdirAll(filepath.Join(base, "dir1/dir2"), defaultPerm); err != nil {
					return err
				}
				return os.Symlink("dir1", filepath.Join(base, "dir1/dir2/test"))
			},
			"test",
			true,
		},
		{
			"symlink-loop",
			func(base string) error {
				return os.Symlink("test", filepath.Join(base, "test"))
			},
			"test",
			true,
		},
		{
			"symlink-not-exists",
			func(base string) error {
				return os.Symlink("non-existing", filepath.Join(base, "test"))
			},
			"test",
			true,
		},
		{
			"non-directory",
			func(base string) error {
				return ioutil.WriteFile(filepath.Join(base, "test"), []byte{}, defaultPerm)
			},
			"test/directory",
			true,
		},
		{
			"non-directory-final",
			func(base string) error {
				return ioutil.WriteFile(filepath.Join(base, "test"), []byte{}, defaultPerm)
			},
			"test",
			false,
		},
		{
			"escape-with-relative-symlink",
			func(base string) error {
				if err := os.MkdirAll(filepath.Join(base, "dir"), defaultPerm); err != nil {
					return err
				}
				if err := os.MkdirAll(filepath.Join(base, "exists"), defaultPerm); err != nil {
					return err
				}
				return os.Symlink("../exists", filepath.Join(base, "dir/test"))
			},
			"dir/test",
			true,
		},
		{
			"escape-with-relative-symlink-not-exists",
			func(base string) error {
				if err := os.MkdirAll(filepath.Join(base, "dir"), defaultPerm); err != nil {
					return err
				}
				return os.Symlink("../not-exists", filepath.Join(base, "dir/test"))
			},
			"dir/test",
			true,
		},
		{
			"escape-with-symlink",
			func(base string) error {
				return os.Symlink("/", filepath.Join(base, "test"))
			},
			"test",
			true,
		},
		{
			"mount-unix-socket",
			func(base string) error {
				socketFile, socketError := createSocketFile(base)

				if socketError != nil {
					return fmt.Errorf("error preparing socket file %s with %w", socketFile, socketError)
				}
				return nil
			},
			"mt.sock",
			false,
		},
		{
			"mounting-unix-socket-in-middle",
			func(base string) error {
				testSocketFile, socketError := createSocketFile(base)

				if socketError != nil {
					return fmt.Errorf("error preparing socket file %s with %w", testSocketFile, socketError)
				}
				return nil
			},
			"mt.sock/bar",
			true,
		},
	}

	for _, test := range tests {
		klog.V(4).Infof("test %q", test.name)
		base, err := ioutil.TempDir("", "safe-open-"+test.name+"-")
		if err != nil {
			t.Fatal(err.Error())
		}

		test.prepare(base)
		pathToCreate := filepath.Join(base, test.path)
		fd, err := doSafeOpen(pathToCreate, base)
		if err != nil && !test.expectError {
			t.Errorf("test %q: %s", test.name, err)
		}
		if err != nil {
			klog.Infof("got error: %s", err)
		}
		if err == nil && test.expectError {
			t.Errorf("test %q: expected error, got none", test.name)
		}

		syscall.Close(fd)
		os.RemoveAll(base)
	}
}

func createSocketFile(socketDir string) (string, error) {
	testSocketFile := filepath.Join(socketDir, "mt.sock")

	// Switch to volume path and create the socket file
	// socket file can not have length of more than 108 character
	// and hence we must use relative path
	oldDir, _ := os.Getwd()

	err := os.Chdir(socketDir)
	if err != nil {
		return "", err
	}
	defer func() {
		os.Chdir(oldDir)
	}()
	_, socketCreateError := net.Listen("unix", "mt.sock")
	return testSocketFile, socketCreateError
}

func TestFindExistingPrefix(t *testing.T) {
	defaultPerm := os.FileMode(0750)
	tests := []struct {
		name string
		// Function that prepares directory structure for the test under given
		// base.
		prepare      func(base string) error
		path         string
		expectedPath string
		expectedDirs []string
		expectError  bool
	}{
		{
			"directory-does-not-exist",
			func(base string) error {
				return nil
			},
			"directory",
			"",
			[]string{"directory"},
			false,
		},
		{
			"directory-exists",
			func(base string) error {
				return os.MkdirAll(filepath.Join(base, "test/directory"), 0750)
			},
			"test/directory",
			"test/directory",
			[]string{},
			false,
		},
		{
			"follow-symlinks",
			func(base string) error {
				if err := os.MkdirAll(filepath.Join(base, "destination/directory"), defaultPerm); err != nil {
					return err
				}
				return os.Symlink("destination", filepath.Join(base, "test"))
			},
			"test/directory",
			"test/directory",
			[]string{},
			false,
		},
		{
			"follow-symlink-loop",
			func(base string) error {
				return os.Symlink("test", filepath.Join(base, "test"))
			},
			"test/directory",
			"",
			nil,
			true,
		},
		{
			"follow-symlink-multiple follow",
			func(base string) error {
				/* test1/dir points to test2 and test2/dir points to test1 */
				if err := os.MkdirAll(filepath.Join(base, "test1"), defaultPerm); err != nil {
					return err
				}
				if err := os.MkdirAll(filepath.Join(base, "test2"), defaultPerm); err != nil {
					return err
				}
				if err := os.Symlink(filepath.Join(base, "test2"), filepath.Join(base, "test1/dir")); err != nil {
					return err
				}
				if err := os.Symlink(filepath.Join(base, "test1"), filepath.Join(base, "test2/dir")); err != nil {
					return err
				}
				return nil
			},
			"test1/dir/dir/foo/bar",
			"test1/dir/dir",
			[]string{"foo", "bar"},
			false,
		},
		{
			"danglink-symlink",
			func(base string) error {
				return os.Symlink("non-existing", filepath.Join(base, "test"))
			},
			// OS returns IsNotExist error both for dangling symlink and for
			// non-existing directory.
			"test/directory",
			"",
			[]string{"test", "directory"},
			false,
		},
		{
			"with-fifo-in-middle",
			func(base string) error {
				testFifo := filepath.Join(base, "mount_test.fifo")
				return syscall.Mkfifo(testFifo, 0)
			},
			"mount_test.fifo/directory",
			"",
			nil,
			true,
		},
	}

	for _, test := range tests {
		klog.V(4).Infof("test %q", test.name)
		base, err := ioutil.TempDir("", "find-prefix-"+test.name+"-")
		if err != nil {
			t.Fatal(err.Error())
		}
		test.prepare(base)
		path := filepath.Join(base, test.path)
		existingPath, dirs, err := findExistingPrefix(base, path)
		if err != nil && !test.expectError {
			t.Errorf("test %q: %s", test.name, err)
		}
		if err != nil {
			klog.Infof("got error: %s", err)
		}
		if err == nil && test.expectError {
			t.Errorf("test %q: expected error, got none", test.name)
		}

		fullExpectedPath := filepath.Join(base, test.expectedPath)
		if existingPath != fullExpectedPath {
			t.Errorf("test %q: expected path %q, got %q", test.name, fullExpectedPath, existingPath)
		}
		if !reflect.DeepEqual(dirs, test.expectedDirs) {
			t.Errorf("test %q: expected dirs %v, got %v", test.name, test.expectedDirs, dirs)
		}
		os.RemoveAll(base)
	}
}

func validateDirEmpty(dir string) error {
	files, err := ioutil.ReadDir(dir)
	if err != nil {
		return err
	}

	if len(files) != 0 {
		return fmt.Errorf("directory %q is not empty", dir)
	}
	return nil
}

func validateDirExists(dir string) error {
	_, err := ioutil.ReadDir(dir)
	if err != nil {
		return err
	}
	return nil
}

func validateDirNotExists(dir string) error {
	_, err := ioutil.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	return fmt.Errorf("dir %q still exists", dir)
}

func validateFileExists(file string) error {
	if _, err := os.Stat(file); err != nil {
		return err
	}
	return nil
}
