/*
 *
 * Copyright 2018 gRPC authors.
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

package binarylog

import (
	"context"
	"net"
	"strings"
	"sync/atomic"
	"time"

	binlogpb "google.golang.org/grpc/binarylog/grpc_binarylog_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type callIDGenerator struct {
	id uint64
}

func (g *callIDGenerator) next() uint64 {
	id := atomic.AddUint64(&g.id, 1)
	return id
}

// reset is for testing only, and doesn't need to be thread safe.
func (g *callIDGenerator) reset() {
	g.id = 0
}

var idGen callIDGenerator

// MethodLogger is the sub-logger for each method.
//
// This is used in the 1.0 release of gcp/observability, and thus must not be
// deleted or changed.
type MethodLogger interface {
	Log(context.Context, LogEntryConfig)
}

// TruncatingMethodLogger is a method logger that truncates headers and messages
// based on configured fields.
type TruncatingMethodLogger struct {
	headerMaxLen, messageMaxLen uint64

	callID          uint64
	idWithinCallGen *callIDGenerator

	sink Sink // TODO(blog): make this pluggable.
}

// NewTruncatingMethodLogger returns a new truncating method logger.
//
// This is used in the 1.0 release of gcp/observability, and thus must not be
// deleted or changed.
func NewTruncatingMethodLogger(h, m uint64) *TruncatingMethodLogger {
	return &TruncatingMethodLogger{
		headerMaxLen:  h,
		messageMaxLen: m,

		callID:          idGen.next(),
		idWithinCallGen: &callIDGenerator{},

		sink: DefaultSink, // TODO(blog): make it pluggable.
	}
}

// Build is an internal only method for building the proto message out of the
// input event. It's made public to enable other library to reuse as much logic
// in TruncatingMethodLogger as possible.
func (ml *TruncatingMethodLogger) Build(c LogEntryConfig) *binlogpb.GrpcLogEntry {
	m := c.toProto()
	timestamp := timestamppb.Now()
	m.Timestamp = timestamp
	m.CallId = ml.callID
	m.SequenceIdWithinCall = ml.idWithinCallGen.next()

	switch pay := m.Payload.(type) {
	case *binlogpb.GrpcLogEntry_ClientHeader:
		m.PayloadTruncated = ml.truncateMetadata(pay.ClientHeader.GetMetadata())
	case *binlogpb.GrpcLogEntry_ServerHeader:
		m.PayloadTruncated = ml.truncateMetadata(pay.ServerHeader.GetMetadata())
	case *binlogpb.GrpcLogEntry_Message:
		m.PayloadTruncated = ml.truncateMessage(pay.Message)
	}
	return m
}

// Log creates a proto binary log entry, and logs it to the sink.
func (ml *TruncatingMethodLogger) Log(_ context.Context, c LogEntryConfig) {
	ml.sink.Write(ml.Build(c))
}

func (ml *TruncatingMethodLogger) truncateMetadata(mdPb *binlogpb.Metadata) (truncated bool) {
	if ml.headerMaxLen == maxUInt {
		return false
	}
	var (
		bytesLimit = ml.headerMaxLen
		index      int
	)
	// At the end of the loop, index will be the first entry where the total
	// size is greater than the limit:
	//
	// len(entry[:index]) <= ml.hdr && len(entry[:index+1]) > ml.hdr.
	for ; index < len(mdPb.Entry); index++ {
		entry := mdPb.Entry[index]
		if entry.Key == "grpc-trace-bin" {
			// "grpc-trace-bin" is a special key. It's kept in the log entry,
			// but not counted towards the size limit.
			continue
		}
		currentEntryLen := uint64(len(entry.GetKey())) + uint64(len(entry.GetValue()))
		if currentEntryLen > bytesLimit {
			break
		}
		bytesLimit -= currentEntryLen
	}
	truncated = index < len(mdPb.Entry)
	mdPb.Entry = mdPb.Entry[:index]
	return truncated
}

func (ml *TruncatingMethodLogger) truncateMessage(msgPb *binlogpb.Message) (truncated bool) {
	if ml.messageMaxLen == maxUInt {
		return false
	}
	if ml.messageMaxLen >= uint64(len(msgPb.Data)) {
		return false
	}
	msgPb.Data = msgPb.Data[:ml.messageMaxLen]
	return true
}

// LogEntryConfig represents the configuration for binary log entry.
//
// This is used in the 1.0 release of gcp/observability, and thus must not be
// deleted or changed.
type LogEntryConfig interface {
	toProto() *binlogpb.GrpcLogEntry
}

// ClientHeader configs the binary log entry to be a ClientHeader entry.
type ClientHeader struct {
	OnClientSide bool
	Header       metadata.MD
	MethodName   string
	Authority    string
	Timeout      time.Duration
	// PeerAddr is required only when it's on server side.
	PeerAddr net.Addr
}

func (c *ClientHeader) toProto() *binlogpb.GrpcLogEntry {
	// This function doesn't need to set all the fields (e.g. seq ID). The Log
	// function will set the fields when necessary.
	clientHeader := &binlogpb.ClientHeader{
		Metadata:   mdToMetadataProto(c.Header),
		MethodName: c.MethodName,
		Authority:  c.Authority,
	}
	if c.Timeout > 0 {
		clientHeader.Timeout = durationpb.New(c.Timeout)
	}
	ret := &binlogpb.GrpcLogEntry{
		Type: binlogpb.GrpcLogEntry_EVENT_TYPE_CLIENT_HEADER,
		Payload: &binlogpb.GrpcLogEntry_ClientHeader{
			ClientHeader: clientHeader,
		},
	}
	if c.OnClientSide {
		ret.Logger = binlogpb.GrpcLogEntry_LOGGER_CLIENT
	} else {
		ret.Logger = binlogpb.GrpcLogEntry_LOGGER_SERVER
	}
	if c.PeerAddr != nil {
		ret.Peer = addrToProto(c.PeerAddr)
	}
	return ret
}

// ServerHeader configs the binary log entry to be a ServerHeader entry.
type ServerHeader struct {
	OnClientSide bool
	Header       metadata.MD
	// PeerAddr is required only when it's on client side.
	PeerAddr net.Addr
}

func (c *ServerHeader) toProto() *binlogpb.GrpcLogEntry {
	ret := &binlogpb.GrpcLogEntry{
		Type: binlogpb.GrpcLogEntry_EVENT_TYPE_SERVER_HEADER,
		Payload: &binlogpb.GrpcLogEntry_ServerHeader{
			ServerHeader: &binlogpb.ServerHeader{
				Metadata: mdToMetadataProto(c.Header),
			},
		},
	}
	if c.OnClientSide {
		ret.Logger = binlogpb.GrpcLogEntry_LOGGER_CLIENT
	} else {
		ret.Logger = binlogpb.GrpcLogEntry_LOGGER_SERVER
	}
	if c.PeerAddr != nil {
		ret.Peer = addrToProto(c.PeerAddr)
	}
	return ret
}

// ClientMessage configs the binary log entry to be a ClientMessage entry.
type ClientMessage struct {
	OnClientSide bool
	// Message can be a proto.Message or []byte. Other messages formats are not
	// supported.
	Message any
}

func (c *ClientMessage) toProto() *binlogpb.GrpcLogEntry {
	var (
		data []byte
		err  error
	)
	if m, ok := c.Message.(proto.Message); ok {
		data, err = proto.Marshal(m)
		if err != nil {
			grpclogLogger.Infof("binarylogging: failed to marshal proto message: %v", err)
		}
	} else if b, ok := c.Message.([]byte); ok {
		data = b
	} else {
		grpclogLogger.Infof("binarylogging: message to log is neither proto.message nor []byte")
	}
	ret := &binlogpb.GrpcLogEntry{
		Type: binlogpb.GrpcLogEntry_EVENT_TYPE_CLIENT_MESSAGE,
		Payload: &binlogpb.GrpcLogEntry_Message{
			Message: &binlogpb.Message{
				Length: uint32(len(data)),
				Data:   data,
			},
		},
	}
	if c.OnClientSide {
		ret.Logger = binlogpb.GrpcLogEntry_LOGGER_CLIENT
	} else {
		ret.Logger = binlogpb.GrpcLogEntry_LOGGER_SERVER
	}
	return ret
}

// ServerMessage configs the binary log entry to be a ServerMessage entry.
type ServerMessage struct {
	OnClientSide bool
	// Message can be a proto.Message or []byte. Other messages formats are not
	// supported.
	Message any
}

func (c *ServerMessage) toProto() *binlogpb.GrpcLogEntry {
	var (
		data []byte
		err  error
	)
	if m, ok := c.Message.(proto.Message); ok {
		data, err = proto.Marshal(m)
		if err != nil {
			grpclogLogger.Infof("binarylogging: failed to marshal proto message: %v", err)
		}
	} else if b, ok := c.Message.([]byte); ok {
		data = b
	} else {
		grpclogLogger.Infof("binarylogging: message to log is neither proto.message nor []byte")
	}
	ret := &binlogpb.GrpcLogEntry{
		Type: binlogpb.GrpcLogEntry_EVENT_TYPE_SERVER_MESSAGE,
		Payload: &binlogpb.GrpcLogEntry_Message{
			Message: &binlogpb.Message{
				Length: uint32(len(data)),
				Data:   data,
			},
		},
	}
	if c.OnClientSide {
		ret.Logger = binlogpb.GrpcLogEntry_LOGGER_CLIENT
	} else {
		ret.Logger = binlogpb.GrpcLogEntry_LOGGER_SERVER
	}
	return ret
}

// ClientHalfClose configs the binary log entry to be a ClientHalfClose entry.
type ClientHalfClose struct {
	OnClientSide bool
}

func (c *ClientHalfClose) toProto() *binlogpb.GrpcLogEntry {
	ret := &binlogpb.GrpcLogEntry{
		Type:    binlogpb.GrpcLogEntry_EVENT_TYPE_CLIENT_HALF_CLOSE,
		Payload: nil, // No payload here.
	}
	if c.OnClientSide {
		ret.Logger = binlogpb.GrpcLogEntry_LOGGER_CLIENT
	} else {
		ret.Logger = binlogpb.GrpcLogEntry_LOGGER_SERVER
	}
	return ret
}

// ServerTrailer configs the binary log entry to be a ServerTrailer entry.
type ServerTrailer struct {
	OnClientSide bool
	Trailer      metadata.MD
	// Err is the status error.
	Err error
	// PeerAddr is required only when it's on client side and the RPC is trailer
	// only.
	PeerAddr net.Addr
}

func (c *ServerTrailer) toProto() *binlogpb.GrpcLogEntry {
	st, ok := status.FromError(c.Err)
	if !ok {
		grpclogLogger.Info("binarylogging: error in trailer is not a status error")
	}
	var (
		detailsBytes []byte
		err          error
	)
	stProto := st.Proto()
	if stProto != nil && len(stProto.Details) != 0 {
		detailsBytes, err = proto.Marshal(stProto)
		if err != nil {
			grpclogLogger.Infof("binarylogging: failed to marshal status proto: %v", err)
		}
	}
	ret := &binlogpb.GrpcLogEntry{
		Type: binlogpb.GrpcLogEntry_EVENT_TYPE_SERVER_TRAILER,
		Payload: &binlogpb.GrpcLogEntry_Trailer{
			Trailer: &binlogpb.Trailer{
				Metadata:      mdToMetadataProto(c.Trailer),
				StatusCode:    uint32(st.Code()),
				StatusMessage: st.Message(),
				StatusDetails: detailsBytes,
			},
		},
	}
	if c.OnClientSide {
		ret.Logger = binlogpb.GrpcLogEntry_LOGGER_CLIENT
	} else {
		ret.Logger = binlogpb.GrpcLogEntry_LOGGER_SERVER
	}
	if c.PeerAddr != nil {
		ret.Peer = addrToProto(c.PeerAddr)
	}
	return ret
}

// Cancel configs the binary log entry to be a Cancel entry.
type Cancel struct {
	OnClientSide bool
}

func (c *Cancel) toProto() *binlogpb.GrpcLogEntry {
	ret := &binlogpb.GrpcLogEntry{
		Type:    binlogpb.GrpcLogEntry_EVENT_TYPE_CANCEL,
		Payload: nil,
	}
	if c.OnClientSide {
		ret.Logger = binlogpb.GrpcLogEntry_LOGGER_CLIENT
	} else {
		ret.Logger = binlogpb.GrpcLogEntry_LOGGER_SERVER
	}
	return ret
}

// metadataKeyOmit returns whether the metadata entry with this key should be
// omitted.
func metadataKeyOmit(key string) bool {
	switch key {
	case "lb-token", ":path", ":authority", "content-encoding", "content-type", "user-agent", "te":
		return true
	case "grpc-trace-bin": // grpc-trace-bin is special because it's visible to users.
		return false
	}
	return strings.HasPrefix(key, "grpc-")
}

func mdToMetadataProto(md metadata.MD) *binlogpb.Metadata {
	ret := &binlogpb.Metadata{}
	for k, vv := range md {
		if metadataKeyOmit(k) {
			continue
		}
		for _, v := range vv {
			ret.Entry = append(ret.Entry,
				&binlogpb.MetadataEntry{
					Key:   k,
					Value: []byte(v),
				},
			)
		}
	}
	return ret
}

func addrToProto(addr net.Addr) *binlogpb.Address {
	ret := &binlogpb.Address{}
	switch a := addr.(type) {
	case *net.TCPAddr:
		if a.IP.To4() != nil {
			ret.Type = binlogpb.Address_TYPE_IPV4
		} else if a.IP.To16() != nil {
			ret.Type = binlogpb.Address_TYPE_IPV6
		} else {
			ret.Type = binlogpb.Address_TYPE_UNKNOWN
			// Do not set address and port fields.
			break
		}
		ret.Address = a.IP.String()
		ret.IpPort = uint32(a.Port)
	case *net.UnixAddr:
		ret.Type = binlogpb.Address_TYPE_UNIX
		ret.Address = a.String()
	default:
		ret.Type = binlogpb.Address_TYPE_UNKNOWN
	}
	return ret
}
