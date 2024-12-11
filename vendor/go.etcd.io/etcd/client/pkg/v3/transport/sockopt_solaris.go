// Copyright 2021 The etcd Authors
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

package transport

import (
	"errors"
	"syscall"

	"golang.org/x/sys/unix"
)

func setReusePort(network, address string, c syscall.RawConn) error {
	return errors.New("port reuse is not supported on Solaris")
}

func setReuseAddress(network, address string, conn syscall.RawConn) error {
	return conn.Control(func(fd uintptr) {
		syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, unix.SO_REUSEADDR, 1)
	})
}
