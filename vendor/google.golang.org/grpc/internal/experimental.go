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

var (
	// WithBufferPool is implemented by the grpc package and returns a dial
	// option to configure a shared buffer pool for a grpc.ClientConn.
	WithBufferPool any // func (grpc.SharedBufferPool) grpc.DialOption

	// BufferPool is implemented by the grpc package and returns a server
	// option to configure a shared buffer pool for a grpc.Server.
	BufferPool any // func (grpc.SharedBufferPool) grpc.ServerOption
)
