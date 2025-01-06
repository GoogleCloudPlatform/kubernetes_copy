//go:build unix

/*
 * Copyright 2023 gRPC authors.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 */

package internal

import (
	"net"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// NetDialerWithTCPKeepalive returns a net.Dialer that enables TCP keepalives on
// the underlying connection with OS default values for keepalive parameters.
//
// TODO: Once https://github.com/golang/go/issues/62254 lands, and the
// appropriate Go version becomes less than our least supported Go version, we
// should look into using the new API to make things more straightforward.
func NetDialerWithTCPKeepalive() *net.Dialer {
	return &net.Dialer{
		// Setting a negative value here prevents the Go stdlib from overriding
		// the values of TCP keepalive time and interval. It also prevents the
		// Go stdlib from enabling TCP keepalives by default.
		KeepAlive: time.Duration(-1),
		// This method is called after the underlying network socket is created,
		// but before dialing the socket (or calling its connect() method). The
		// combination of unconditionally enabling TCP keepalives here, and
		// disabling the overriding of TCP keepalive parameters by setting the
		// KeepAlive field to a negative value above, results in OS defaults for
		// the TCP keepalive interval and time parameters.
		Control: func(_, _ string, c syscall.RawConn) error {
			return c.Control(func(fd uintptr) {
				unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_KEEPALIVE, 1)
			})
		},
	}
}
