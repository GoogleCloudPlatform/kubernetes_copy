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

//go:build solaris

package fileutil

import (
	"os"
	"syscall"
)

func TryLockFile(path string, flag int, perm os.FileMode) (*LockedFile, error) {
	var lock syscall.Flock_t
	lock.Start = 0
	lock.Len = 0
	lock.Pid = 0
	lock.Type = syscall.F_WRLCK
	lock.Whence = 0
	lock.Pid = 0
	f, err := os.OpenFile(path, flag, perm)
	if err != nil {
		return nil, err
	}
	if err := syscall.FcntlFlock(f.Fd(), syscall.F_SETLK, &lock); err != nil {
		f.Close()
		if err == syscall.EAGAIN {
			err = ErrLocked
		}
		return nil, err
	}
	return &LockedFile{f}, nil
}

func LockFile(path string, flag int, perm os.FileMode) (*LockedFile, error) {
	var lock syscall.Flock_t
	lock.Start = 0
	lock.Len = 0
	lock.Pid = 0
	lock.Type = syscall.F_WRLCK
	lock.Whence = 0
	f, err := os.OpenFile(path, flag, perm)
	if err != nil {
		return nil, err
	}
	if err = syscall.FcntlFlock(f.Fd(), syscall.F_SETLKW, &lock); err != nil {
		f.Close()
		return nil, err
	}
	return &LockedFile{f}, nil
}
