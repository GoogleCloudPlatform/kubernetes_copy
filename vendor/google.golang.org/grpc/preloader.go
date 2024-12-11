/*
 *
 * Copyright 2019 gRPC authors.
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

package grpc

import (
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/mem"
	"google.golang.org/grpc/status"
)

// PreparedMsg is responsible for creating a Marshalled and Compressed object.
//
// # Experimental
//
// Notice: This type is EXPERIMENTAL and may be changed or removed in a
// later release.
type PreparedMsg struct {
	// Struct for preparing msg before sending them
	encodedData mem.BufferSlice
	hdr         []byte
	payload     mem.BufferSlice
	pf          payloadFormat
}

// Encode marshalls and compresses the message using the codec and compressor for the stream.
func (p *PreparedMsg) Encode(s Stream, msg any) error {
	ctx := s.Context()
	rpcInfo, ok := rpcInfoFromContext(ctx)
	if !ok {
		return status.Errorf(codes.Internal, "grpc: unable to get rpcInfo")
	}

	// check if the context has the relevant information to prepareMsg
	if rpcInfo.preloaderInfo == nil {
		return status.Errorf(codes.Internal, "grpc: rpcInfo.preloaderInfo is nil")
	}
	if rpcInfo.preloaderInfo.codec == nil {
		return status.Errorf(codes.Internal, "grpc: rpcInfo.preloaderInfo.codec is nil")
	}

	// prepare the msg
	data, err := encode(rpcInfo.preloaderInfo.codec, msg)
	if err != nil {
		return err
	}

	materializedData := data.Materialize()
	data.Free()
	p.encodedData = mem.BufferSlice{mem.NewBuffer(&materializedData, nil)}

	// TODO: it should be possible to grab the bufferPool from the underlying
	//  stream implementation with a type cast to its actual type (such as
	//  addrConnStream) and accessing the buffer pool directly.
	var compData mem.BufferSlice
	compData, p.pf, err = compress(p.encodedData, rpcInfo.preloaderInfo.cp, rpcInfo.preloaderInfo.comp, mem.DefaultBufferPool())
	if err != nil {
		return err
	}

	if p.pf.isCompressed() {
		materializedCompData := compData.Materialize()
		compData.Free()
		compData = mem.BufferSlice{mem.NewBuffer(&materializedCompData, nil)}
	}

	p.hdr, p.payload = msgHeader(p.encodedData, compData, p.pf)

	return nil
}
