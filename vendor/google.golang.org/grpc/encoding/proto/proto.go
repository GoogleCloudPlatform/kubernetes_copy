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

// Package proto defines the protobuf codec. Importing this package will
// register the codec.
package proto

import (
	"fmt"

	"google.golang.org/grpc/encoding"
	"google.golang.org/grpc/mem"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/protoadapt"
)

// Name is the name registered for the proto compressor.
const Name = "proto"

func init() {
	encoding.RegisterCodecV2(&codecV2{})
}

// codec is a CodecV2 implementation with protobuf. It is the default codec for
// gRPC.
type codecV2 struct{}

func (c *codecV2) Marshal(v any) (data mem.BufferSlice, err error) {
	vv := messageV2Of(v)
	if vv == nil {
		return nil, fmt.Errorf("proto: failed to marshal, message is %T, want proto.Message", v)
	}

	size := proto.Size(vv)
	if mem.IsBelowBufferPoolingThreshold(size) {
		buf, err := proto.Marshal(vv)
		if err != nil {
			return nil, err
		}
		data = append(data, mem.SliceBuffer(buf))
	} else {
		pool := mem.DefaultBufferPool()
		buf := pool.Get(size)
		if _, err := (proto.MarshalOptions{}).MarshalAppend((*buf)[:0], vv); err != nil {
			pool.Put(buf)
			return nil, err
		}
		data = append(data, mem.NewBuffer(buf, pool))
	}

	return data, nil
}

func (c *codecV2) Unmarshal(data mem.BufferSlice, v any) (err error) {
	vv := messageV2Of(v)
	if vv == nil {
		return fmt.Errorf("failed to unmarshal, message is %T, want proto.Message", v)
	}

	buf := data.MaterializeToBuffer(mem.DefaultBufferPool())
	defer buf.Free()
	// TODO: Upgrade proto.Unmarshal to support mem.BufferSlice. Right now, it's not
	//  really possible without a major overhaul of the proto package, but the
	//  vtprotobuf library may be able to support this.
	return proto.Unmarshal(buf.ReadOnlyData(), vv)
}

func messageV2Of(v any) proto.Message {
	switch v := v.(type) {
	case protoadapt.MessageV1:
		return protoadapt.MessageV2Of(v)
	case protoadapt.MessageV2:
		return v
	}

	return nil
}

func (c *codecV2) Name() string {
	return Name
}
