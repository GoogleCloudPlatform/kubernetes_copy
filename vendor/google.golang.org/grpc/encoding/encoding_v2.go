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

package encoding

import (
	"strings"

	"google.golang.org/grpc/mem"
)

// CodecV2 defines the interface gRPC uses to encode and decode messages. Note
// that implementations of this interface must be thread safe; a CodecV2's
// methods can be called from concurrent goroutines.
type CodecV2 interface {
	// Marshal returns the wire format of v. The buffers in the returned
	// [mem.BufferSlice] must have at least one reference each, which will be freed
	// by gRPC when they are no longer needed.
	Marshal(v any) (out mem.BufferSlice, err error)
	// Unmarshal parses the wire format into v. Note that data will be freed as soon
	// as this function returns. If the codec wishes to guarantee access to the data
	// after this function, it must take its own reference that it frees when it is
	// no longer needed.
	Unmarshal(data mem.BufferSlice, v any) error
	// Name returns the name of the Codec implementation. The returned string
	// will be used as part of content type in transmission.  The result must be
	// static; the result cannot change between calls.
	Name() string
}

// RegisterCodecV2 registers the provided CodecV2 for use with all gRPC clients and
// servers.
//
// The CodecV2 will be stored and looked up by result of its Name() method, which
// should match the content-subtype of the encoding handled by the CodecV2.  This
// is case-insensitive, and is stored and looked up as lowercase.  If the
// result of calling Name() is an empty string, RegisterCodecV2 will panic. See
// Content-Type on
// https://github.com/grpc/grpc/blob/master/doc/PROTOCOL-HTTP2.md#requests for
// more details.
//
// If both a Codec and CodecV2 are registered with the same name, the CodecV2
// will be used.
//
// NOTE: this function must only be called during initialization time (i.e. in
// an init() function), and is not thread-safe.  If multiple Codecs are
// registered with the same name, the one registered last will take effect.
func RegisterCodecV2(codec CodecV2) {
	if codec == nil {
		panic("cannot register a nil CodecV2")
	}
	if codec.Name() == "" {
		panic("cannot register CodecV2 with empty string result for Name()")
	}
	contentSubtype := strings.ToLower(codec.Name())
	registeredCodecs[contentSubtype] = codec
}

// GetCodecV2 gets a registered CodecV2 by content-subtype, or nil if no CodecV2 is
// registered for the content-subtype.
//
// The content-subtype is expected to be lowercase.
func GetCodecV2(contentSubtype string) CodecV2 {
	c, _ := registeredCodecs[contentSubtype].(CodecV2)
	return c
}
