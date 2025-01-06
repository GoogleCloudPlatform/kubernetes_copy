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

// Logger mimics golang's standard Logger as an interface.
//
// Deprecated: use LoggerV2.
type Logger interface {
	Fatal(args ...any)
	Fatalf(format string, args ...any)
	Fatalln(args ...any)
	Print(args ...any)
	Printf(format string, args ...any)
	Println(args ...any)
}

// LoggerWrapper wraps Logger into a LoggerV2.
type LoggerWrapper struct {
	Logger
}

// Info logs to INFO log. Arguments are handled in the manner of fmt.Print.
func (l *LoggerWrapper) Info(args ...any) {
	l.Logger.Print(args...)
}

// Infoln logs to INFO log. Arguments are handled in the manner of fmt.Println.
func (l *LoggerWrapper) Infoln(args ...any) {
	l.Logger.Println(args...)
}

// Infof logs to INFO log. Arguments are handled in the manner of fmt.Printf.
func (l *LoggerWrapper) Infof(format string, args ...any) {
	l.Logger.Printf(format, args...)
}

// Warning logs to WARNING log. Arguments are handled in the manner of fmt.Print.
func (l *LoggerWrapper) Warning(args ...any) {
	l.Logger.Print(args...)
}

// Warningln logs to WARNING log. Arguments are handled in the manner of fmt.Println.
func (l *LoggerWrapper) Warningln(args ...any) {
	l.Logger.Println(args...)
}

// Warningf logs to WARNING log. Arguments are handled in the manner of fmt.Printf.
func (l *LoggerWrapper) Warningf(format string, args ...any) {
	l.Logger.Printf(format, args...)
}

// Error logs to ERROR log. Arguments are handled in the manner of fmt.Print.
func (l *LoggerWrapper) Error(args ...any) {
	l.Logger.Print(args...)
}

// Errorln logs to ERROR log. Arguments are handled in the manner of fmt.Println.
func (l *LoggerWrapper) Errorln(args ...any) {
	l.Logger.Println(args...)
}

// Errorf logs to ERROR log. Arguments are handled in the manner of fmt.Printf.
func (l *LoggerWrapper) Errorf(format string, args ...any) {
	l.Logger.Printf(format, args...)
}

// V reports whether verbosity level l is at least the requested verbose level.
func (*LoggerWrapper) V(int) bool {
	// Returns true for all verbose level.
	return true
}
