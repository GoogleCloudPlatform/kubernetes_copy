//go:build !linux
// +build !linux

/*
 *
 * Copyright 2018 gRPC authors.
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

// Package syscall provides functionalities that grpc uses to get low-level
// operating system stats/info.
package syscall

import (
	"net"
	"sync"
	"time"

	"google.golang.org/grpc/grpclog"
)

var once sync.Once
var logger = grpclog.Component("core")

func log() {
	once.Do(func() {
		logger.Info("CPU time info is unavailable on non-linux environments.")
	})
}

// GetCPUTime returns the how much CPU time has passed since the start of this
// process. It always returns 0 under non-linux environments.
func GetCPUTime() int64 {
	log()
	return 0
}

// Rusage is an empty struct under non-linux environments.
type Rusage struct{}

// GetRusage is a no-op function under non-linux environments.
func GetRusage() *Rusage {
	log()
	return nil
}

// CPUTimeDiff returns the differences of user CPU time and system CPU time used
// between two Rusage structs. It a no-op function for non-linux environments.
func CPUTimeDiff(*Rusage, *Rusage) (float64, float64) {
	log()
	return 0, 0
}

// SetTCPUserTimeout is a no-op function under non-linux environments.
func SetTCPUserTimeout(net.Conn, time.Duration) error {
	log()
	return nil
}

// GetTCPUserTimeout is a no-op function under non-linux environments.
// A negative return value indicates the operation is not supported
func GetTCPUserTimeout(net.Conn) (int, error) {
	log()
	return -1, nil
}
