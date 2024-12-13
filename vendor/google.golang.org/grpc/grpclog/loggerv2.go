/*
 *
 * Copyright 2017 gRPC authors.
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

package grpclog

import (
	"io"
	"os"
	"strconv"
	"strings"

	"google.golang.org/grpc/grpclog/internal"
)

// LoggerV2 does underlying logging work for grpclog.
type LoggerV2 internal.LoggerV2

// SetLoggerV2 sets logger that is used in grpc to a V2 logger.
// Not mutex-protected, should be called before any gRPC functions.
func SetLoggerV2(l LoggerV2) {
	if _, ok := l.(*componentData); ok {
		panic("cannot use component logger as grpclog logger")
	}
	internal.LoggerV2Impl = l
	internal.DepthLoggerV2Impl, _ = l.(internal.DepthLoggerV2)
}

// NewLoggerV2 creates a loggerV2 with the provided writers.
// Fatal logs will be written to errorW, warningW, infoW, followed by exit(1).
// Error logs will be written to errorW, warningW and infoW.
// Warning logs will be written to warningW and infoW.
// Info logs will be written to infoW.
func NewLoggerV2(infoW, warningW, errorW io.Writer) LoggerV2 {
	return internal.NewLoggerV2(infoW, warningW, errorW, internal.LoggerV2Config{})
}

// NewLoggerV2WithVerbosity creates a loggerV2 with the provided writers and
// verbosity level.
func NewLoggerV2WithVerbosity(infoW, warningW, errorW io.Writer, v int) LoggerV2 {
	return internal.NewLoggerV2(infoW, warningW, errorW, internal.LoggerV2Config{Verbosity: v})
}

// newLoggerV2 creates a loggerV2 to be used as default logger.
// All logs are written to stderr.
func newLoggerV2() LoggerV2 {
	errorW := io.Discard
	warningW := io.Discard
	infoW := io.Discard

	logLevel := os.Getenv("GRPC_GO_LOG_SEVERITY_LEVEL")
	switch logLevel {
	case "", "ERROR", "error": // If env is unset, set level to ERROR.
		errorW = os.Stderr
	case "WARNING", "warning":
		warningW = os.Stderr
	case "INFO", "info":
		infoW = os.Stderr
	}

	var v int
	vLevel := os.Getenv("GRPC_GO_LOG_VERBOSITY_LEVEL")
	if vl, err := strconv.Atoi(vLevel); err == nil {
		v = vl
	}

	jsonFormat := strings.EqualFold(os.Getenv("GRPC_GO_LOG_FORMATTER"), "json")

	return internal.NewLoggerV2(infoW, warningW, errorW, internal.LoggerV2Config{
		Verbosity:  v,
		FormatJSON: jsonFormat,
	})
}

// DepthLoggerV2 logs at a specified call frame. If a LoggerV2 also implements
// DepthLoggerV2, the below functions will be called with the appropriate stack
// depth set for trivial functions the logger may ignore.
//
// # Experimental
//
// Notice: This type is EXPERIMENTAL and may be changed or removed in a
// later release.
type DepthLoggerV2 internal.DepthLoggerV2
