//go:build aix
// +build aix

package bbolt

import (
	"fmt"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// flock acquires an advisory lock on a file descriptor.
func flock(db *DB, exclusive bool, timeout time.Duration) error {
	var t time.Time
	if timeout != 0 {
		t = time.Now()
	}
	fd := db.file.Fd()
	var lockType int16
	if exclusive {
		lockType = syscall.F_WRLCK
	} else {
		lockType = syscall.F_RDLCK
	}
	for {
		// Attempt to obtain an exclusive lock.
		lock := syscall.Flock_t{Type: lockType}
		err := syscall.FcntlFlock(fd, syscall.F_SETLK, &lock)
		if err == nil {
			return nil
		} else if err != syscall.EAGAIN {
			return err
		}

		// If we timed out then return an error.
		if timeout != 0 && time.Since(t) > timeout-flockRetryTimeout {
			return ErrTimeout
		}

		// Wait for a bit and try again.
		time.Sleep(flockRetryTimeout)
	}
}

// funlock releases an advisory lock on a file descriptor.
func funlock(db *DB) error {
	var lock syscall.Flock_t
	lock.Start = 0
	lock.Len = 0
	lock.Type = syscall.F_UNLCK
	lock.Whence = 0
	return syscall.FcntlFlock(uintptr(db.file.Fd()), syscall.F_SETLK, &lock)
}

// mmap memory maps a DB's data file.
func mmap(db *DB, sz int) error {
	// Map the data file to memory.
	b, err := unix.Mmap(int(db.file.Fd()), 0, sz, syscall.PROT_READ, syscall.MAP_SHARED|db.MmapFlags)
	if err != nil {
		return err
	}

	// Advise the kernel that the mmap is accessed randomly.
	if err := unix.Madvise(b, syscall.MADV_RANDOM); err != nil {
		return fmt.Errorf("madvise: %s", err)
	}

	// Save the original byte slice and convert to a byte array pointer.
	db.dataref = b
	db.data = (*[maxMapSize]byte)(unsafe.Pointer(&b[0]))
	db.datasz = sz
	return nil
}

// munmap unmaps a DB's data file from memory.
func munmap(db *DB) error {
	// Ignore the unmap if we have no mapped data.
	if db.dataref == nil {
		return nil
	}

	// Unmap using the original byte slice.
	err := unix.Munmap(db.dataref)
	db.dataref = nil
	db.data = nil
	db.datasz = 0
	return err
}
