// Copyright 2016 The etcd Authors
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

//go:build darwin

package fileutil

import (
	"errors"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

func preallocExtend(f *os.File, sizeInBytes int64) error {
	if err := preallocFixed(f, sizeInBytes); err != nil {
		return err
	}
	return preallocExtendTrunc(f, sizeInBytes)
}

func preallocFixed(f *os.File, sizeInBytes int64) error {
	// allocate all requested space or no space at all
	// TODO: allocate contiguous space on disk with F_ALLOCATECONTIG flag
	fstore := &unix.Fstore_t{
		Flags:   unix.F_ALLOCATEALL,
		Posmode: unix.F_PEOFPOSMODE,
		Length:  sizeInBytes,
	}
	err := unix.FcntlFstore(f.Fd(), unix.F_PREALLOCATE, fstore)
	if err == nil || errors.Is(err, unix.ENOTSUP) {
		return nil
	}

	// wrong argument to fallocate syscall
	if err == unix.EINVAL {
		// filesystem "st_blocks" are allocated in the units of
		// "Allocation Block Size" (run "diskutil info /" command)
		var stat syscall.Stat_t
		syscall.Fstat(int(f.Fd()), &stat)

		// syscall.Statfs_t.Bsize is "optimal transfer block size"
		// and contains matching 4096 value when latest OS X kernel
		// supports 4,096 KB filesystem block size
		var statfs syscall.Statfs_t
		syscall.Fstatfs(int(f.Fd()), &statfs)
		blockSize := int64(statfs.Bsize)

		if stat.Blocks*blockSize >= sizeInBytes {
			// enough blocks are already allocated
			return nil
		}
	}
	return err
}
