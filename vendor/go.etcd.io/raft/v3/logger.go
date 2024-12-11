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

package raft

import (
	"fmt"
	"io"
	"log"
	"os"
	"sync"
)

type Logger interface {
	Debug(v ...interface{})
	Debugf(format string, v ...interface{})

	Error(v ...interface{})
	Errorf(format string, v ...interface{})

	Info(v ...interface{})
	Infof(format string, v ...interface{})

	Warning(v ...interface{})
	Warningf(format string, v ...interface{})

	Fatal(v ...interface{})
	Fatalf(format string, v ...interface{})

	Panic(v ...interface{})
	Panicf(format string, v ...interface{})
}

func SetLogger(l Logger) {
	raftLoggerMu.Lock()
	raftLogger = l
	raftLoggerMu.Unlock()
}

func ResetDefaultLogger() {
	SetLogger(defaultLogger)
}

func getLogger() Logger {
	raftLoggerMu.Lock()
	defer raftLoggerMu.Unlock()
	return raftLogger
}

var (
	defaultLogger = &DefaultLogger{Logger: log.New(os.Stderr, "raft", log.LstdFlags)}
	discardLogger = &DefaultLogger{Logger: log.New(io.Discard, "", 0)}
	raftLoggerMu  sync.Mutex
	raftLogger    = Logger(defaultLogger)
)

const (
	calldepth = 2
)

// DefaultLogger is a default implementation of the Logger interface.
type DefaultLogger struct {
	*log.Logger
	debug bool
}

func (l *DefaultLogger) EnableTimestamps() {
	l.SetFlags(l.Flags() | log.Ldate | log.Ltime)
}

func (l *DefaultLogger) EnableDebug() {
	l.debug = true
}

func (l *DefaultLogger) Debug(v ...interface{}) {
	if l.debug {
		l.Output(calldepth, header("DEBUG", fmt.Sprint(v...)))
	}
}

func (l *DefaultLogger) Debugf(format string, v ...interface{}) {
	if l.debug {
		l.Output(calldepth, header("DEBUG", fmt.Sprintf(format, v...)))
	}
}

func (l *DefaultLogger) Info(v ...interface{}) {
	l.Output(calldepth, header("INFO", fmt.Sprint(v...)))
}

func (l *DefaultLogger) Infof(format string, v ...interface{}) {
	l.Output(calldepth, header("INFO", fmt.Sprintf(format, v...)))
}

func (l *DefaultLogger) Error(v ...interface{}) {
	l.Output(calldepth, header("ERROR", fmt.Sprint(v...)))
}

func (l *DefaultLogger) Errorf(format string, v ...interface{}) {
	l.Output(calldepth, header("ERROR", fmt.Sprintf(format, v...)))
}

func (l *DefaultLogger) Warning(v ...interface{}) {
	l.Output(calldepth, header("WARN", fmt.Sprint(v...)))
}

func (l *DefaultLogger) Warningf(format string, v ...interface{}) {
	l.Output(calldepth, header("WARN", fmt.Sprintf(format, v...)))
}

func (l *DefaultLogger) Fatal(v ...interface{}) {
	l.Output(calldepth, header("FATAL", fmt.Sprint(v...)))
	os.Exit(1)
}

func (l *DefaultLogger) Fatalf(format string, v ...interface{}) {
	l.Output(calldepth, header("FATAL", fmt.Sprintf(format, v...)))
	os.Exit(1)
}

func (l *DefaultLogger) Panic(v ...interface{}) {
	l.Logger.Panic(v...)
}

func (l *DefaultLogger) Panicf(format string, v ...interface{}) {
	l.Logger.Panicf(format, v...)
}

func header(lvl, msg string) string {
	return fmt.Sprintf("%s: %s", lvl, msg)
}
