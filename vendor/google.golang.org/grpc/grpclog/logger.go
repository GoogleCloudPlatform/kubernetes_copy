/*
 *
 * Copyright 2015 gRPC authors.
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

import "google.golang.org/grpc/grpclog/internal"

// Logger mimics golang's standard Logger as an interface.
//
// Deprecated: use LoggerV2.
type Logger internal.Logger

// SetLogger sets the logger that is used in grpc. Call only from
// init() functions.
//
// Deprecated: use SetLoggerV2.
func SetLogger(l Logger) {
	internal.LoggerV2Impl = &internal.LoggerWrapper{Logger: l}
}
