/*
 *
 * Copyright 2020 gRPC authors.
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

// Package grpclog provides logging functionality for internal gRPC packages,
// outside of the functionality provided by the external `grpclog` package.
package grpclog

import (
	"fmt"

	"google.golang.org/grpc/grpclog"
)

// PrefixLogger does logging with a prefix.
//
// Logging method on a nil logs without any prefix.
type PrefixLogger struct {
	logger grpclog.DepthLoggerV2
	prefix string
}

// Infof does info logging.
func (pl *PrefixLogger) Infof(format string, args ...any) {
	if pl != nil {
		// Handle nil, so the tests can pass in a nil logger.
		format = pl.prefix + format
		pl.logger.InfoDepth(1, fmt.Sprintf(format, args...))
		return
	}
	grpclog.InfoDepth(1, fmt.Sprintf(format, args...))
}

// Warningf does warning logging.
func (pl *PrefixLogger) Warningf(format string, args ...any) {
	if pl != nil {
		format = pl.prefix + format
		pl.logger.WarningDepth(1, fmt.Sprintf(format, args...))
		return
	}
	grpclog.WarningDepth(1, fmt.Sprintf(format, args...))
}

// Errorf does error logging.
func (pl *PrefixLogger) Errorf(format string, args ...any) {
	if pl != nil {
		format = pl.prefix + format
		pl.logger.ErrorDepth(1, fmt.Sprintf(format, args...))
		return
	}
	grpclog.ErrorDepth(1, fmt.Sprintf(format, args...))
}

// V reports whether verbosity level l is at least the requested verbose level.
func (pl *PrefixLogger) V(l int) bool {
	if pl != nil {
		return pl.logger.V(l)
	}
	return true
}

// NewPrefixLogger creates a prefix logger with the given prefix.
func NewPrefixLogger(logger grpclog.DepthLoggerV2, prefix string) *PrefixLogger {
	return &PrefixLogger{logger: logger, prefix: prefix}
}
