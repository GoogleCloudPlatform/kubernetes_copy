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

package filesystem

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// DefaultFs implements Filesystem using same-named functions from "os" and "io"
type DefaultFs struct {
	root string
}

var _ Filesystem = &DefaultFs{}

// NewTempFs returns a fake Filesystem in temporary directory, useful for unit tests
func NewTempFs() Filesystem {
	path, _ := os.MkdirTemp("", "tmpfs")
	return &DefaultFs{
		root: path,
	}
}

func (fs *DefaultFs) prefix(path string) string {
	if len(fs.root) == 0 {
		return path
	}
	return filepath.Join(fs.root, path)
}

// Stat via os.Stat
func (fs *DefaultFs) Stat(name string) (os.FileInfo, error) {
	return os.Stat(fs.prefix(name))
}

// Create via os.Create
func (fs *DefaultFs) Create(name string) (File, error) {
	file, err := os.Create(fs.prefix(name))
	if err != nil {
		return nil, err
	}
	return &defaultFile{file}, nil
}

// Rename via os.Rename
func (fs *DefaultFs) Rename(oldpath, newpath string) error {
	if !strings.HasPrefix(oldpath, fs.root) {
		oldpath = fs.prefix(oldpath)
	}
	if !strings.HasPrefix(newpath, fs.root) {
		newpath = fs.prefix(newpath)
	}
	return os.Rename(oldpath, newpath)
}

func (fs *DefaultFs) MkdirAll(path string, perm os.FileMode) error {
	return MkdirAll(fs.prefix(path), perm)
}

// MkdirAllWithPathCheck checks if path exists already. If not, it creates a directory
// named path, along with any necessary parents, and returns nil, or else returns an error.
// Permission bits perm (before umask) are used for all directories that
// MkdirAllWithPathCheck creates.
// If path is already a directory, MkdirAllWithPathCheck does nothing and returns nil.
// NOTE: In case of Windows NTFS, mount points are implemented as reparse-point
// (similar to symlink) and do not represent actual directory. Hence Directory existence
// check for windows NTFS will NOT check for dir, but for symlink presence.
func MkdirAllWithPathCheck(path string, perm os.FileMode) error {
	if dir, err := os.Lstat(path); err == nil {
		// If the path exists already,
		// 1. for Unix/Linux OS, check if the path is directory.
		// 2. for windows NTFS, check if the path is symlink instead of directory.
		if dir.IsDir() ||
			(runtime.GOOS == "windows" && (dir.Mode()&os.ModeSymlink != 0 || dir.Mode()&os.ModeIrregular != 0)) {
			return nil
		}
		return fmt.Errorf("path %v exists but is not a directory", path)
	}
	// If existence of path not known, attempt to create it.
	if err := MkdirAll(path, perm); err != nil {
		return err
	}
	return nil
}

// Chtimes via os.Chtimes
func (fs *DefaultFs) Chtimes(name string, atime time.Time, mtime time.Time) error {
	return os.Chtimes(fs.prefix(name), atime, mtime)
}

// RemoveAll via os.RemoveAll
func (fs *DefaultFs) RemoveAll(path string) error {
	return os.RemoveAll(fs.prefix(path))
}

// Remove via os.Remove
func (fs *DefaultFs) Remove(name string) error {
	return os.Remove(fs.prefix(name))
}

// ReadFile via os.ReadFile
func (fs *DefaultFs) ReadFile(filename string) ([]byte, error) {
	return os.ReadFile(fs.prefix(filename))
}

// TempDir via os.MkdirTemp
func (fs *DefaultFs) TempDir(dir, prefix string) (string, error) {
	return os.MkdirTemp(fs.prefix(dir), prefix)
}

// TempFile via os.CreateTemp
func (fs *DefaultFs) TempFile(dir, prefix string) (File, error) {
	file, err := os.CreateTemp(fs.prefix(dir), prefix)
	if err != nil {
		return nil, err
	}
	return &defaultFile{file}, nil
}

// ReadDir via os.ReadDir
func (fs *DefaultFs) ReadDir(dirname string) ([]os.DirEntry, error) {
	return os.ReadDir(fs.prefix(dirname))
}

// Walk via filepath.Walk
func (fs *DefaultFs) Walk(root string, walkFn filepath.WalkFunc) error {
	return filepath.Walk(fs.prefix(root), walkFn)
}

// defaultFile implements File using same-named functions from "os"
type defaultFile struct {
	file *os.File
}

// Name via os.File.Name
func (file *defaultFile) Name() string {
	return file.file.Name()
}

// Write via os.File.Write
func (file *defaultFile) Write(b []byte) (n int, err error) {
	return file.file.Write(b)
}

// Sync via os.File.Sync
func (file *defaultFile) Sync() error {
	return file.file.Sync()
}

// Close via os.File.Close
func (file *defaultFile) Close() error {
	return file.file.Close()
}
