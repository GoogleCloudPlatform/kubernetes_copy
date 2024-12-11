// Copyright 2015 The etcd Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build windows

package fileutil

import (
	"errors"
	"fmt"
	"os"
	"syscall"

	"golang.org/x/sys/windows"
)

var errLocked = errors.New("the process cannot access the file because another process has locked a portion of the file")

func TryLockFile(path string, flag int, perm os.FileMode) (*LockedFile, error) {
	f, err := open(path, flag, perm)
	if err != nil {
		return nil, err
	}
	if err := lockFile(windows.Handle(f.Fd()), windows.LOCKFILE_FAIL_IMMEDIATELY); err != nil {
		f.Close()
		return nil, err
	}
	return &LockedFile{f}, nil
}

func LockFile(path string, flag int, perm os.FileMode) (*LockedFile, error) {
	f, err := open(path, flag, perm)
	if err != nil {
		return nil, err
	}
	if err := lockFile(windows.Handle(f.Fd()), 0); err != nil {
		f.Close()
		return nil, err
	}
	return &LockedFile{f}, nil
}

func open(path string, flag int, perm os.FileMode) (*os.File, error) {
	if path == "" {
		return nil, errors.New("cannot open empty filename")
	}
	var access uint32
	switch flag {
	case syscall.O_RDONLY:
		access = syscall.GENERIC_READ
	case syscall.O_WRONLY:
		access = syscall.GENERIC_WRITE
	case syscall.O_RDWR:
		access = syscall.GENERIC_READ | syscall.GENERIC_WRITE
	case syscall.O_WRONLY | syscall.O_CREAT:
		access = syscall.GENERIC_ALL
	default:
		panic(fmt.Errorf("flag %v is not supported", flag))
	}
	fd, err := syscall.CreateFile(&(syscall.StringToUTF16(path)[0]),
		access,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE,
		nil,
		syscall.OPEN_ALWAYS,
		syscall.FILE_ATTRIBUTE_NORMAL,
		0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}

func lockFile(fd windows.Handle, flags uint32) error {
	if fd == windows.InvalidHandle {
		return nil
	}
	err := windows.LockFileEx(fd, flags|windows.LOCKFILE_EXCLUSIVE_LOCK, 0, 1, 0, &windows.Overlapped{})
	if err == nil {
		return nil
	} else if err.Error() == errLocked.Error() {
		return ErrLocked
	} else if err != windows.ERROR_LOCK_VIOLATION {
		return err
	}
	return nil
}
