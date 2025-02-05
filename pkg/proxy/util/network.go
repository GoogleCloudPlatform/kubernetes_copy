/*
Copyright 2018 The Kubernetes Authors.

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

package util

import (
	"net"
)

// NetworkInterfacer defines an interface for several net library functions. Production
// code will forward to net library functions, and unit tests will override the methods
// for testing purposes.
type NetworkInterfacer interface {
	InterfaceAddrs() ([]net.Addr, error)
}

// RealNetwork implements the NetworkInterfacer interface for production code, just
// wrapping the underlying net library function calls.
type RealNetwork struct{}

// InterfaceAddrs wraps net.InterfaceAddrs(), it's a part of NetworkInterfacer interface.
func (RealNetwork) InterfaceAddrs() ([]net.Addr, error) {
	addr, err := net.InterfaceAddrs()
	if err != nil {
		err = GetInterfaceAddrsByInterfaces(&addr)
		if err != nil {
			return nil, err
		}
	}
	return addr, nil
}

var _ NetworkInterfacer = &RealNetwork{}
