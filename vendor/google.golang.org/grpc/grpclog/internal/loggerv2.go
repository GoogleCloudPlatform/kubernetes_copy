/*
 *
 * Copyright 2024 gRPC authors.
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
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
)

// LoggerV2 does underlying logging work for grpclog.
type LoggerV2 interface {
	// Info logs to INFO log. Arguments are handled in the manner of fmt.Print.
	Info(args ...any)
	// Infoln logs to INFO log. Arguments are handled in the manner of fmt.Println.
	Infoln(args ...any)
	// Infof logs to INFO log. Arguments are handled in the manner of fmt.Printf.
	Infof(format string, args ...any)
	// Warning logs to WARNING log. Arguments are handled in the manner of fmt.Print.
	Warning(args ...any)
	// Warningln logs to WARNING log. Arguments are handled in the manner of fmt.Println.
	Warningln(args ...any)
	// Warningf logs to WARNING log. Arguments are handled in the manner of fmt.Printf.
	Warningf(format string, args ...any)
	// Error logs to ERROR log. Arguments are handled in the manner of fmt.Print.
	Error(args ...any)
	// Errorln logs to ERROR log. Arguments are handled in the manner of fmt.Println.
	Errorln(args ...any)
	// Errorf logs to ERROR log. Arguments are handled in the manner of fmt.Printf.
	Errorf(format string, args ...any)
	// Fatal logs to ERROR log. Arguments are handled in the manner of fmt.Print.
	// gRPC ensures that all Fatal logs will exit with os.Exit(1).
	// Implementations may also call os.Exit() with a non-zero exit code.
	Fatal(args ...any)
	// Fatalln logs to ERROR log. Arguments are handled in the manner of fmt.Println.
	// gRPC ensures that all Fatal logs will exit with os.Exit(1).
	// Implementations may also call os.Exit() with a non-zero exit code.
	Fatalln(args ...any)
	// Fatalf logs to ERROR log. Arguments are handled in the manner of fmt.Printf.
	// gRPC ensures that all Fatal logs will exit with os.Exit(1).
	// Implementations may also call os.Exit() with a non-zero exit code.
	Fatalf(format string, args ...any)
	// V reports whether verbosity level l is at least the requested verbose level.
	V(l int) bool
}

// DepthLoggerV2 logs at a specified call frame. If a LoggerV2 also implements
// DepthLoggerV2, the below functions will be called with the appropriate stack
// depth set for trivial functions the logger may ignore.
//
// # Experimental
//
// Notice: This type is EXPERIMENTAL and may be changed or removed in a
// later release.
type DepthLoggerV2 interface {
	LoggerV2
	// InfoDepth logs to INFO log at the specified depth. Arguments are handled in the manner of fmt.Println.
	InfoDepth(depth int, args ...any)
	// WarningDepth logs to WARNING log at the specified depth. Arguments are handled in the manner of fmt.Println.
	WarningDepth(depth int, args ...any)
	// ErrorDepth logs to ERROR log at the specified depth. Arguments are handled in the manner of fmt.Println.
	ErrorDepth(depth int, args ...any)
	// FatalDepth logs to FATAL log at the specified depth. Arguments are handled in the manner of fmt.Println.
	FatalDepth(depth int, args ...any)
}

const (
	// infoLog indicates Info severity.
	infoLog int = iota
	// warningLog indicates Warning severity.
	warningLog
	// errorLog indicates Error severity.
	errorLog
	// fatalLog indicates Fatal severity.
	fatalLog
)

// severityName contains the string representation of each severity.
var severityName = []string{
	infoLog:    "INFO",
	warningLog: "WARNING",
	errorLog:   "ERROR",
	fatalLog:   "FATAL",
}

// loggerT is the default logger used by grpclog.
type loggerT struct {
	m          []*log.Logger
	v          int
	jsonFormat bool
}

func (g *loggerT) output(severity int, s string) {
	sevStr := severityName[severity]
	if !g.jsonFormat {
		g.m[severity].Output(2, fmt.Sprintf("%v: %v", sevStr, s))
		return
	}
	// TODO: we can also include the logging component, but that needs more
	// (API) changes.
	b, _ := json.Marshal(map[string]string{
		"severity": sevStr,
		"message":  s,
	})
	g.m[severity].Output(2, string(b))
}

func (g *loggerT) Info(args ...any) {
	g.output(infoLog, fmt.Sprint(args...))
}

func (g *loggerT) Infoln(args ...any) {
	g.output(infoLog, fmt.Sprintln(args...))
}

func (g *loggerT) Infof(format string, args ...any) {
	g.output(infoLog, fmt.Sprintf(format, args...))
}

func (g *loggerT) Warning(args ...any) {
	g.output(warningLog, fmt.Sprint(args...))
}

func (g *loggerT) Warningln(args ...any) {
	g.output(warningLog, fmt.Sprintln(args...))
}

func (g *loggerT) Warningf(format string, args ...any) {
	g.output(warningLog, fmt.Sprintf(format, args...))
}

func (g *loggerT) Error(args ...any) {
	g.output(errorLog, fmt.Sprint(args...))
}

func (g *loggerT) Errorln(args ...any) {
	g.output(errorLog, fmt.Sprintln(args...))
}

func (g *loggerT) Errorf(format string, args ...any) {
	g.output(errorLog, fmt.Sprintf(format, args...))
}

func (g *loggerT) Fatal(args ...any) {
	g.output(fatalLog, fmt.Sprint(args...))
	os.Exit(1)
}

func (g *loggerT) Fatalln(args ...any) {
	g.output(fatalLog, fmt.Sprintln(args...))
	os.Exit(1)
}

func (g *loggerT) Fatalf(format string, args ...any) {
	g.output(fatalLog, fmt.Sprintf(format, args...))
	os.Exit(1)
}

func (g *loggerT) V(l int) bool {
	return l <= g.v
}

// LoggerV2Config configures the LoggerV2 implementation.
type LoggerV2Config struct {
	// Verbosity sets the verbosity level of the logger.
	Verbosity int
	// FormatJSON controls whether the logger should output logs in JSON format.
	FormatJSON bool
}

// NewLoggerV2 creates a new LoggerV2 instance with the provided configuration.
// The infoW, warningW, and errorW writers are used to write log messages of
// different severity levels.
func NewLoggerV2(infoW, warningW, errorW io.Writer, c LoggerV2Config) LoggerV2 {
	var m []*log.Logger
	flag := log.LstdFlags
	if c.FormatJSON {
		flag = 0
	}
	m = append(m, log.New(infoW, "", flag))
	m = append(m, log.New(io.MultiWriter(infoW, warningW), "", flag))
	ew := io.MultiWriter(infoW, warningW, errorW) // ew will be used for error and fatal.
	m = append(m, log.New(ew, "", flag))
	m = append(m, log.New(ew, "", flag))
	return &loggerT{m: m, v: c.Verbosity, jsonFormat: c.FormatJSON}
}
