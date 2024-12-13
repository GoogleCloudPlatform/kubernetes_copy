/*
 *
 * Copyright 2014 gRPC authors.
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
	"compress/gzip"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/encoding"
	"google.golang.org/grpc/encoding/proto"
	"google.golang.org/grpc/internal/transport"
	"google.golang.org/grpc/mem"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/stats"
	"google.golang.org/grpc/status"
)

// Compressor defines the interface gRPC uses to compress a message.
//
// Deprecated: use package encoding.
type Compressor interface {
	// Do compresses p into w.
	Do(w io.Writer, p []byte) error
	// Type returns the compression algorithm the Compressor uses.
	Type() string
}

type gzipCompressor struct {
	pool sync.Pool
}

// NewGZIPCompressor creates a Compressor based on GZIP.
//
// Deprecated: use package encoding/gzip.
func NewGZIPCompressor() Compressor {
	c, _ := NewGZIPCompressorWithLevel(gzip.DefaultCompression)
	return c
}

// NewGZIPCompressorWithLevel is like NewGZIPCompressor but specifies the gzip compression level instead
// of assuming DefaultCompression.
//
// The error returned will be nil if the level is valid.
//
// Deprecated: use package encoding/gzip.
func NewGZIPCompressorWithLevel(level int) (Compressor, error) {
	if level < gzip.DefaultCompression || level > gzip.BestCompression {
		return nil, fmt.Errorf("grpc: invalid compression level: %d", level)
	}
	return &gzipCompressor{
		pool: sync.Pool{
			New: func() any {
				w, err := gzip.NewWriterLevel(io.Discard, level)
				if err != nil {
					panic(err)
				}
				return w
			},
		},
	}, nil
}

func (c *gzipCompressor) Do(w io.Writer, p []byte) error {
	z := c.pool.Get().(*gzip.Writer)
	defer c.pool.Put(z)
	z.Reset(w)
	if _, err := z.Write(p); err != nil {
		return err
	}
	return z.Close()
}

func (c *gzipCompressor) Type() string {
	return "gzip"
}

// Decompressor defines the interface gRPC uses to decompress a message.
//
// Deprecated: use package encoding.
type Decompressor interface {
	// Do reads the data from r and uncompress them.
	Do(r io.Reader) ([]byte, error)
	// Type returns the compression algorithm the Decompressor uses.
	Type() string
}

type gzipDecompressor struct {
	pool sync.Pool
}

// NewGZIPDecompressor creates a Decompressor based on GZIP.
//
// Deprecated: use package encoding/gzip.
func NewGZIPDecompressor() Decompressor {
	return &gzipDecompressor{}
}

func (d *gzipDecompressor) Do(r io.Reader) ([]byte, error) {
	var z *gzip.Reader
	switch maybeZ := d.pool.Get().(type) {
	case nil:
		newZ, err := gzip.NewReader(r)
		if err != nil {
			return nil, err
		}
		z = newZ
	case *gzip.Reader:
		z = maybeZ
		if err := z.Reset(r); err != nil {
			d.pool.Put(z)
			return nil, err
		}
	}

	defer func() {
		z.Close()
		d.pool.Put(z)
	}()
	return io.ReadAll(z)
}

func (d *gzipDecompressor) Type() string {
	return "gzip"
}

// callInfo contains all related configuration and information about an RPC.
type callInfo struct {
	compressorType        string
	failFast              bool
	maxReceiveMessageSize *int
	maxSendMessageSize    *int
	creds                 credentials.PerRPCCredentials
	contentSubtype        string
	codec                 baseCodec
	maxRetryRPCBufferSize int
	onFinish              []func(err error)
}

func defaultCallInfo() *callInfo {
	return &callInfo{
		failFast:              true,
		maxRetryRPCBufferSize: 256 * 1024, // 256KB
	}
}

// CallOption configures a Call before it starts or extracts information from
// a Call after it completes.
type CallOption interface {
	// before is called before the call is sent to any server.  If before
	// returns a non-nil error, the RPC fails with that error.
	before(*callInfo) error

	// after is called after the call has completed.  after cannot return an
	// error, so any failures should be reported via output parameters.
	after(*callInfo, *csAttempt)
}

// EmptyCallOption does not alter the Call configuration.
// It can be embedded in another structure to carry satellite data for use
// by interceptors.
type EmptyCallOption struct{}

func (EmptyCallOption) before(*callInfo) error      { return nil }
func (EmptyCallOption) after(*callInfo, *csAttempt) {}

// StaticMethod returns a CallOption which specifies that a call is being made
// to a method that is static, which means the method is known at compile time
// and doesn't change at runtime. This can be used as a signal to stats plugins
// that this method is safe to include as a key to a measurement.
func StaticMethod() CallOption {
	return StaticMethodCallOption{}
}

// StaticMethodCallOption is a CallOption that specifies that a call comes
// from a static method.
type StaticMethodCallOption struct {
	EmptyCallOption
}

// Header returns a CallOptions that retrieves the header metadata
// for a unary RPC.
func Header(md *metadata.MD) CallOption {
	return HeaderCallOption{HeaderAddr: md}
}

// HeaderCallOption is a CallOption for collecting response header metadata.
// The metadata field will be populated *after* the RPC completes.
//
// # Experimental
//
// Notice: This type is EXPERIMENTAL and may be changed or removed in a
// later release.
type HeaderCallOption struct {
	HeaderAddr *metadata.MD
}

func (o HeaderCallOption) before(*callInfo) error { return nil }
func (o HeaderCallOption) after(_ *callInfo, attempt *csAttempt) {
	*o.HeaderAddr, _ = attempt.s.Header()
}

// Trailer returns a CallOptions that retrieves the trailer metadata
// for a unary RPC.
func Trailer(md *metadata.MD) CallOption {
	return TrailerCallOption{TrailerAddr: md}
}

// TrailerCallOption is a CallOption for collecting response trailer metadata.
// The metadata field will be populated *after* the RPC completes.
//
// # Experimental
//
// Notice: This type is EXPERIMENTAL and may be changed or removed in a
// later release.
type TrailerCallOption struct {
	TrailerAddr *metadata.MD
}

func (o TrailerCallOption) before(*callInfo) error { return nil }
func (o TrailerCallOption) after(_ *callInfo, attempt *csAttempt) {
	*o.TrailerAddr = attempt.s.Trailer()
}

// Peer returns a CallOption that retrieves peer information for a unary RPC.
// The peer field will be populated *after* the RPC completes.
func Peer(p *peer.Peer) CallOption {
	return PeerCallOption{PeerAddr: p}
}

// PeerCallOption is a CallOption for collecting the identity of the remote
// peer. The peer field will be populated *after* the RPC completes.
//
// # Experimental
//
// Notice: This type is EXPERIMENTAL and may be changed or removed in a
// later release.
type PeerCallOption struct {
	PeerAddr *peer.Peer
}

func (o PeerCallOption) before(*callInfo) error { return nil }
func (o PeerCallOption) after(_ *callInfo, attempt *csAttempt) {
	if x, ok := peer.FromContext(attempt.s.Context()); ok {
		*o.PeerAddr = *x
	}
}

// WaitForReady configures the RPC's behavior when the client is in
// TRANSIENT_FAILURE, which occurs when all addresses fail to connect.  If
// waitForReady is false, the RPC will fail immediately.  Otherwise, the client
// will wait until a connection becomes available or the RPC's deadline is
// reached.
//
// By default, RPCs do not "wait for ready".
func WaitForReady(waitForReady bool) CallOption {
	return FailFastCallOption{FailFast: !waitForReady}
}

// FailFast is the opposite of WaitForReady.
//
// Deprecated: use WaitForReady.
func FailFast(failFast bool) CallOption {
	return FailFastCallOption{FailFast: failFast}
}

// FailFastCallOption is a CallOption for indicating whether an RPC should fail
// fast or not.
//
// # Experimental
//
// Notice: This type is EXPERIMENTAL and may be changed or removed in a
// later release.
type FailFastCallOption struct {
	FailFast bool
}

func (o FailFastCallOption) before(c *callInfo) error {
	c.failFast = o.FailFast
	return nil
}
func (o FailFastCallOption) after(*callInfo, *csAttempt) {}

// OnFinish returns a CallOption that configures a callback to be called when
// the call completes. The error passed to the callback is the status of the
// RPC, and may be nil. The onFinish callback provided will only be called once
// by gRPC. This is mainly used to be used by streaming interceptors, to be
// notified when the RPC completes along with information about the status of
// the RPC.
//
// # Experimental
//
// Notice: This API is EXPERIMENTAL and may be changed or removed in a
// later release.
func OnFinish(onFinish func(err error)) CallOption {
	return OnFinishCallOption{
		OnFinish: onFinish,
	}
}

// OnFinishCallOption is CallOption that indicates a callback to be called when
// the call completes.
//
// # Experimental
//
// Notice: This type is EXPERIMENTAL and may be changed or removed in a
// later release.
type OnFinishCallOption struct {
	OnFinish func(error)
}

func (o OnFinishCallOption) before(c *callInfo) error {
	c.onFinish = append(c.onFinish, o.OnFinish)
	return nil
}

func (o OnFinishCallOption) after(*callInfo, *csAttempt) {}

// MaxCallRecvMsgSize returns a CallOption which sets the maximum message size
// in bytes the client can receive. If this is not set, gRPC uses the default
// 4MB.
func MaxCallRecvMsgSize(bytes int) CallOption {
	return MaxRecvMsgSizeCallOption{MaxRecvMsgSize: bytes}
}

// MaxRecvMsgSizeCallOption is a CallOption that indicates the maximum message
// size in bytes the client can receive.
//
// # Experimental
//
// Notice: This type is EXPERIMENTAL and may be changed or removed in a
// later release.
type MaxRecvMsgSizeCallOption struct {
	MaxRecvMsgSize int
}

func (o MaxRecvMsgSizeCallOption) before(c *callInfo) error {
	c.maxReceiveMessageSize = &o.MaxRecvMsgSize
	return nil
}
func (o MaxRecvMsgSizeCallOption) after(*callInfo, *csAttempt) {}

// MaxCallSendMsgSize returns a CallOption which sets the maximum message size
// in bytes the client can send. If this is not set, gRPC uses the default
// `math.MaxInt32`.
func MaxCallSendMsgSize(bytes int) CallOption {
	return MaxSendMsgSizeCallOption{MaxSendMsgSize: bytes}
}

// MaxSendMsgSizeCallOption is a CallOption that indicates the maximum message
// size in bytes the client can send.
//
// # Experimental
//
// Notice: This type is EXPERIMENTAL and may be changed or removed in a
// later release.
type MaxSendMsgSizeCallOption struct {
	MaxSendMsgSize int
}

func (o MaxSendMsgSizeCallOption) before(c *callInfo) error {
	c.maxSendMessageSize = &o.MaxSendMsgSize
	return nil
}
func (o MaxSendMsgSizeCallOption) after(*callInfo, *csAttempt) {}

// PerRPCCredentials returns a CallOption that sets credentials.PerRPCCredentials
// for a call.
func PerRPCCredentials(creds credentials.PerRPCCredentials) CallOption {
	return PerRPCCredsCallOption{Creds: creds}
}

// PerRPCCredsCallOption is a CallOption that indicates the per-RPC
// credentials to use for the call.
//
// # Experimental
//
// Notice: This type is EXPERIMENTAL and may be changed or removed in a
// later release.
type PerRPCCredsCallOption struct {
	Creds credentials.PerRPCCredentials
}

func (o PerRPCCredsCallOption) before(c *callInfo) error {
	c.creds = o.Creds
	return nil
}
func (o PerRPCCredsCallOption) after(*callInfo, *csAttempt) {}

// UseCompressor returns a CallOption which sets the compressor used when
// sending the request.  If WithCompressor is also set, UseCompressor has
// higher priority.
//
// # Experimental
//
// Notice: This API is EXPERIMENTAL and may be changed or removed in a
// later release.
func UseCompressor(name string) CallOption {
	return CompressorCallOption{CompressorType: name}
}

// CompressorCallOption is a CallOption that indicates the compressor to use.
//
// # Experimental
//
// Notice: This type is EXPERIMENTAL and may be changed or removed in a
// later release.
type CompressorCallOption struct {
	CompressorType string
}

func (o CompressorCallOption) before(c *callInfo) error {
	c.compressorType = o.CompressorType
	return nil
}
func (o CompressorCallOption) after(*callInfo, *csAttempt) {}

// CallContentSubtype returns a CallOption that will set the content-subtype
// for a call. For example, if content-subtype is "json", the Content-Type over
// the wire will be "application/grpc+json". The content-subtype is converted
// to lowercase before being included in Content-Type. See Content-Type on
// https://github.com/grpc/grpc/blob/master/doc/PROTOCOL-HTTP2.md#requests for
// more details.
//
// If ForceCodec is not also used, the content-subtype will be used to look up
// the Codec to use in the registry controlled by RegisterCodec. See the
// documentation on RegisterCodec for details on registration. The lookup of
// content-subtype is case-insensitive. If no such Codec is found, the call
// will result in an error with code codes.Internal.
//
// If ForceCodec is also used, that Codec will be used for all request and
// response messages, with the content-subtype set to the given contentSubtype
// here for requests.
func CallContentSubtype(contentSubtype string) CallOption {
	return ContentSubtypeCallOption{ContentSubtype: strings.ToLower(contentSubtype)}
}

// ContentSubtypeCallOption is a CallOption that indicates the content-subtype
// used for marshaling messages.
//
// # Experimental
//
// Notice: This type is EXPERIMENTAL and may be changed or removed in a
// later release.
type ContentSubtypeCallOption struct {
	ContentSubtype string
}

func (o ContentSubtypeCallOption) before(c *callInfo) error {
	c.contentSubtype = o.ContentSubtype
	return nil
}
func (o ContentSubtypeCallOption) after(*callInfo, *csAttempt) {}

// ForceCodec returns a CallOption that will set codec to be used for all
// request and response messages for a call. The result of calling Name() will
// be used as the content-subtype after converting to lowercase, unless
// CallContentSubtype is also used.
//
// See Content-Type on
// https://github.com/grpc/grpc/blob/master/doc/PROTOCOL-HTTP2.md#requests for
// more details. Also see the documentation on RegisterCodec and
// CallContentSubtype for more details on the interaction between Codec and
// content-subtype.
//
// This function is provided for advanced users; prefer to use only
// CallContentSubtype to select a registered codec instead.
//
// # Experimental
//
// Notice: This API is EXPERIMENTAL and may be changed or removed in a
// later release.
func ForceCodec(codec encoding.Codec) CallOption {
	return ForceCodecCallOption{Codec: codec}
}

// ForceCodecCallOption is a CallOption that indicates the codec used for
// marshaling messages.
//
// # Experimental
//
// Notice: This type is EXPERIMENTAL and may be changed or removed in a
// later release.
type ForceCodecCallOption struct {
	Codec encoding.Codec
}

func (o ForceCodecCallOption) before(c *callInfo) error {
	c.codec = newCodecV1Bridge(o.Codec)
	return nil
}
func (o ForceCodecCallOption) after(*callInfo, *csAttempt) {}

// ForceCodecV2 returns a CallOption that will set codec to be used for all
// request and response messages for a call. The result of calling Name() will
// be used as the content-subtype after converting to lowercase, unless
// CallContentSubtype is also used.
//
// See Content-Type on
// https://github.com/grpc/grpc/blob/master/doc/PROTOCOL-HTTP2.md#requests for
// more details. Also see the documentation on RegisterCodec and
// CallContentSubtype for more details on the interaction between Codec and
// content-subtype.
//
// This function is provided for advanced users; prefer to use only
// CallContentSubtype to select a registered codec instead.
//
// # Experimental
//
// Notice: This API is EXPERIMENTAL and may be changed or removed in a
// later release.
func ForceCodecV2(codec encoding.CodecV2) CallOption {
	return ForceCodecV2CallOption{CodecV2: codec}
}

// ForceCodecV2CallOption is a CallOption that indicates the codec used for
// marshaling messages.
//
// # Experimental
//
// Notice: This type is EXPERIMENTAL and may be changed or removed in a
// later release.
type ForceCodecV2CallOption struct {
	CodecV2 encoding.CodecV2
}

func (o ForceCodecV2CallOption) before(c *callInfo) error {
	c.codec = o.CodecV2
	return nil
}

func (o ForceCodecV2CallOption) after(*callInfo, *csAttempt) {}

// CallCustomCodec behaves like ForceCodec, but accepts a grpc.Codec instead of
// an encoding.Codec.
//
// Deprecated: use ForceCodec instead.
func CallCustomCodec(codec Codec) CallOption {
	return CustomCodecCallOption{Codec: codec}
}

// CustomCodecCallOption is a CallOption that indicates the codec used for
// marshaling messages.
//
// # Experimental
//
// Notice: This type is EXPERIMENTAL and may be changed or removed in a
// later release.
type CustomCodecCallOption struct {
	Codec Codec
}

func (o CustomCodecCallOption) before(c *callInfo) error {
	c.codec = newCodecV0Bridge(o.Codec)
	return nil
}
func (o CustomCodecCallOption) after(*callInfo, *csAttempt) {}

// MaxRetryRPCBufferSize returns a CallOption that limits the amount of memory
// used for buffering this RPC's requests for retry purposes.
//
// # Experimental
//
// Notice: This API is EXPERIMENTAL and may be changed or removed in a
// later release.
func MaxRetryRPCBufferSize(bytes int) CallOption {
	return MaxRetryRPCBufferSizeCallOption{bytes}
}

// MaxRetryRPCBufferSizeCallOption is a CallOption indicating the amount of
// memory to be used for caching this RPC for retry purposes.
//
// # Experimental
//
// Notice: This type is EXPERIMENTAL and may be changed or removed in a
// later release.
type MaxRetryRPCBufferSizeCallOption struct {
	MaxRetryRPCBufferSize int
}

func (o MaxRetryRPCBufferSizeCallOption) before(c *callInfo) error {
	c.maxRetryRPCBufferSize = o.MaxRetryRPCBufferSize
	return nil
}
func (o MaxRetryRPCBufferSizeCallOption) after(*callInfo, *csAttempt) {}

// The format of the payload: compressed or not?
type payloadFormat uint8

const (
	compressionNone payloadFormat = 0 // no compression
	compressionMade payloadFormat = 1 // compressed
)

func (pf payloadFormat) isCompressed() bool {
	return pf == compressionMade
}

type streamReader interface {
	ReadHeader(header []byte) error
	Read(n int) (mem.BufferSlice, error)
}

// parser reads complete gRPC messages from the underlying reader.
type parser struct {
	// r is the underlying reader.
	// See the comment on recvMsg for the permissible
	// error types.
	r streamReader

	// The header of a gRPC message. Find more detail at
	// https://github.com/grpc/grpc/blob/master/doc/PROTOCOL-HTTP2.md
	header [5]byte

	// bufferPool is the pool of shared receive buffers.
	bufferPool mem.BufferPool
}

// recvMsg reads a complete gRPC message from the stream.
//
// It returns the message and its payload (compression/encoding)
// format. The caller owns the returned msg memory.
//
// If there is an error, possible values are:
//   - io.EOF, when no messages remain
//   - io.ErrUnexpectedEOF
//   - of type transport.ConnectionError
//   - an error from the status package
//
// No other error values or types must be returned, which also means
// that the underlying streamReader must not return an incompatible
// error.
func (p *parser) recvMsg(maxReceiveMessageSize int) (payloadFormat, mem.BufferSlice, error) {
	err := p.r.ReadHeader(p.header[:])
	if err != nil {
		return 0, nil, err
	}

	pf := payloadFormat(p.header[0])
	length := binary.BigEndian.Uint32(p.header[1:])

	if length == 0 {
		return pf, nil, nil
	}
	if int64(length) > int64(maxInt) {
		return 0, nil, status.Errorf(codes.ResourceExhausted, "grpc: received message larger than max length allowed on current machine (%d vs. %d)", length, maxInt)
	}
	if int(length) > maxReceiveMessageSize {
		return 0, nil, status.Errorf(codes.ResourceExhausted, "grpc: received message larger than max (%d vs. %d)", length, maxReceiveMessageSize)
	}

	data, err := p.r.Read(int(length))
	if err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return 0, nil, err
	}
	return pf, data, nil
}

// encode serializes msg and returns a buffer containing the message, or an
// error if it is too large to be transmitted by grpc.  If msg is nil, it
// generates an empty message.
func encode(c baseCodec, msg any) (mem.BufferSlice, error) {
	if msg == nil { // NOTE: typed nils will not be caught by this check
		return nil, nil
	}
	b, err := c.Marshal(msg)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "grpc: error while marshaling: %v", err.Error())
	}
	if uint(b.Len()) > math.MaxUint32 {
		b.Free()
		return nil, status.Errorf(codes.ResourceExhausted, "grpc: message too large (%d bytes)", len(b))
	}
	return b, nil
}

// compress returns the input bytes compressed by compressor or cp.
// If both compressors are nil, or if the message has zero length, returns nil,
// indicating no compression was done.
//
// TODO(dfawley): eliminate cp parameter by wrapping Compressor in an encoding.Compressor.
func compress(in mem.BufferSlice, cp Compressor, compressor encoding.Compressor, pool mem.BufferPool) (mem.BufferSlice, payloadFormat, error) {
	if (compressor == nil && cp == nil) || in.Len() == 0 {
		return nil, compressionNone, nil
	}
	var out mem.BufferSlice
	w := mem.NewWriter(&out, pool)
	wrapErr := func(err error) error {
		out.Free()
		return status.Errorf(codes.Internal, "grpc: error while compressing: %v", err.Error())
	}
	if compressor != nil {
		z, err := compressor.Compress(w)
		if err != nil {
			return nil, 0, wrapErr(err)
		}
		for _, b := range in {
			if _, err := z.Write(b.ReadOnlyData()); err != nil {
				return nil, 0, wrapErr(err)
			}
		}
		if err := z.Close(); err != nil {
			return nil, 0, wrapErr(err)
		}
	} else {
		// This is obviously really inefficient since it fully materializes the data, but
		// there is no way around this with the old Compressor API. At least it attempts
		// to return the buffer to the provider, in the hopes it can be reused (maybe
		// even by a subsequent call to this very function).
		buf := in.MaterializeToBuffer(pool)
		defer buf.Free()
		if err := cp.Do(w, buf.ReadOnlyData()); err != nil {
			return nil, 0, wrapErr(err)
		}
	}
	return out, compressionMade, nil
}

const (
	payloadLen = 1
	sizeLen    = 4
	headerLen  = payloadLen + sizeLen
)

// msgHeader returns a 5-byte header for the message being transmitted and the
// payload, which is compData if non-nil or data otherwise.
func msgHeader(data, compData mem.BufferSlice, pf payloadFormat) (hdr []byte, payload mem.BufferSlice) {
	hdr = make([]byte, headerLen)
	hdr[0] = byte(pf)

	var length uint32
	if pf.isCompressed() {
		length = uint32(compData.Len())
		payload = compData
	} else {
		length = uint32(data.Len())
		payload = data
	}

	// Write length of payload into buf
	binary.BigEndian.PutUint32(hdr[payloadLen:], length)
	return hdr, payload
}

func outPayload(client bool, msg any, dataLength, payloadLength int, t time.Time) *stats.OutPayload {
	return &stats.OutPayload{
		Client:           client,
		Payload:          msg,
		Length:           dataLength,
		WireLength:       payloadLength + headerLen,
		CompressedLength: payloadLength,
		SentTime:         t,
	}
}

func checkRecvPayload(pf payloadFormat, recvCompress string, haveCompressor bool, isServer bool) *status.Status {
	switch pf {
	case compressionNone:
	case compressionMade:
		if recvCompress == "" || recvCompress == encoding.Identity {
			return status.New(codes.Internal, "grpc: compressed flag set with identity or empty encoding")
		}
		if !haveCompressor {
			if isServer {
				return status.Newf(codes.Unimplemented, "grpc: Decompressor is not installed for grpc-encoding %q", recvCompress)
			}
			return status.Newf(codes.Internal, "grpc: Decompressor is not installed for grpc-encoding %q", recvCompress)
		}
	default:
		return status.Newf(codes.Internal, "grpc: received unexpected payload format %d", pf)
	}
	return nil
}

type payloadInfo struct {
	compressedLength  int // The compressed length got from wire.
	uncompressedBytes mem.BufferSlice
}

func (p *payloadInfo) free() {
	if p != nil && p.uncompressedBytes != nil {
		p.uncompressedBytes.Free()
	}
}

// recvAndDecompress reads a message from the stream, decompressing it if necessary.
//
// Cancelling the returned cancel function releases the buffer back to the pool. So the caller should cancel as soon as
// the buffer is no longer needed.
// TODO: Refactor this function to reduce the number of arguments.
// See: https://google.github.io/styleguide/go/best-practices.html#function-argument-lists
func recvAndDecompress(p *parser, s *transport.Stream, dc Decompressor, maxReceiveMessageSize int, payInfo *payloadInfo, compressor encoding.Compressor, isServer bool,
) (out mem.BufferSlice, err error) {
	pf, compressed, err := p.recvMsg(maxReceiveMessageSize)
	if err != nil {
		return nil, err
	}

	compressedLength := compressed.Len()

	if st := checkRecvPayload(pf, s.RecvCompress(), compressor != nil || dc != nil, isServer); st != nil {
		compressed.Free()
		return nil, st.Err()
	}

	var size int
	if pf.isCompressed() {
		defer compressed.Free()

		// To match legacy behavior, if the decompressor is set by WithDecompressor or RPCDecompressor,
		// use this decompressor as the default.
		if dc != nil {
			var uncompressedBuf []byte
			uncompressedBuf, err = dc.Do(compressed.Reader())
			if err == nil {
				out = mem.BufferSlice{mem.NewBuffer(&uncompressedBuf, nil)}
			}
			size = len(uncompressedBuf)
		} else {
			out, size, err = decompress(compressor, compressed, maxReceiveMessageSize, p.bufferPool)
		}
		if err != nil {
			return nil, status.Errorf(codes.Internal, "grpc: failed to decompress the received message: %v", err)
		}
		if size > maxReceiveMessageSize {
			out.Free()
			// TODO: Revisit the error code. Currently keep it consistent with java
			// implementation.
			return nil, status.Errorf(codes.ResourceExhausted, "grpc: received message after decompression larger than max (%d vs. %d)", size, maxReceiveMessageSize)
		}
	} else {
		out = compressed
	}

	if payInfo != nil {
		payInfo.compressedLength = compressedLength
		out.Ref()
		payInfo.uncompressedBytes = out
	}

	return out, nil
}

// Using compressor, decompress d, returning data and size.
// Optionally, if data will be over maxReceiveMessageSize, just return the size.
func decompress(compressor encoding.Compressor, d mem.BufferSlice, maxReceiveMessageSize int, pool mem.BufferPool) (mem.BufferSlice, int, error) {
	dcReader, err := compressor.Decompress(d.Reader())
	if err != nil {
		return nil, 0, err
	}

	// TODO: Can/should this still be preserved with the new BufferSlice API? Are
	//  there any actual benefits to allocating a single large buffer instead of
	//  multiple smaller ones?
	//if sizer, ok := compressor.(interface {
	//	DecompressedSize(compressedBytes []byte) int
	//}); ok {
	//	if size := sizer.DecompressedSize(d); size >= 0 {
	//		if size > maxReceiveMessageSize {
	//			return nil, size, nil
	//		}
	//		// size is used as an estimate to size the buffer, but we
	//		// will read more data if available.
	//		// +MinRead so ReadFrom will not reallocate if size is correct.
	//		//
	//		// TODO: If we ensure that the buffer size is the same as the DecompressedSize,
	//		// we can also utilize the recv buffer pool here.
	//		buf := bytes.NewBuffer(make([]byte, 0, size+bytes.MinRead))
	//		bytesRead, err := buf.ReadFrom(io.LimitReader(dcReader, int64(maxReceiveMessageSize)+1))
	//		return buf.Bytes(), int(bytesRead), err
	//	}
	//}

	var out mem.BufferSlice
	_, err = io.Copy(mem.NewWriter(&out, pool), io.LimitReader(dcReader, int64(maxReceiveMessageSize)+1))
	if err != nil {
		out.Free()
		return nil, 0, err
	}
	return out, out.Len(), nil
}

// For the two compressor parameters, both should not be set, but if they are,
// dc takes precedence over compressor.
// TODO(dfawley): wrap the old compressor/decompressor using the new API?
func recv(p *parser, c baseCodec, s *transport.Stream, dc Decompressor, m any, maxReceiveMessageSize int, payInfo *payloadInfo, compressor encoding.Compressor, isServer bool) error {
	data, err := recvAndDecompress(p, s, dc, maxReceiveMessageSize, payInfo, compressor, isServer)
	if err != nil {
		return err
	}

	// If the codec wants its own reference to the data, it can get it. Otherwise, always
	// free the buffers.
	defer data.Free()

	if err := c.Unmarshal(data, m); err != nil {
		return status.Errorf(codes.Internal, "grpc: failed to unmarshal the received message: %v", err)
	}

	return nil
}

// Information about RPC
type rpcInfo struct {
	failfast      bool
	preloaderInfo *compressorInfo
}

// Information about Preloader
// Responsible for storing codec, and compressors
// If stream (s) has  context s.Context which stores rpcInfo that has non nil
// pointers to codec, and compressors, then we can use preparedMsg for Async message prep
// and reuse marshalled bytes
type compressorInfo struct {
	codec baseCodec
	cp    Compressor
	comp  encoding.Compressor
}

type rpcInfoContextKey struct{}

func newContextWithRPCInfo(ctx context.Context, failfast bool, codec baseCodec, cp Compressor, comp encoding.Compressor) context.Context {
	return context.WithValue(ctx, rpcInfoContextKey{}, &rpcInfo{
		failfast: failfast,
		preloaderInfo: &compressorInfo{
			codec: codec,
			cp:    cp,
			comp:  comp,
		},
	})
}

func rpcInfoFromContext(ctx context.Context) (s *rpcInfo, ok bool) {
	s, ok = ctx.Value(rpcInfoContextKey{}).(*rpcInfo)
	return
}

// Code returns the error code for err if it was produced by the rpc system.
// Otherwise, it returns codes.Unknown.
//
// Deprecated: use status.Code instead.
func Code(err error) codes.Code {
	return status.Code(err)
}

// ErrorDesc returns the error description of err if it was produced by the rpc system.
// Otherwise, it returns err.Error() or empty string when err is nil.
//
// Deprecated: use status.Convert and Message method instead.
func ErrorDesc(err error) string {
	return status.Convert(err).Message()
}

// Errorf returns an error containing an error code and a description;
// Errorf returns nil if c is OK.
//
// Deprecated: use status.Errorf instead.
func Errorf(c codes.Code, format string, a ...any) error {
	return status.Errorf(c, format, a...)
}

var errContextCanceled = status.Error(codes.Canceled, context.Canceled.Error())
var errContextDeadline = status.Error(codes.DeadlineExceeded, context.DeadlineExceeded.Error())

// toRPCErr converts an error into an error from the status package.
func toRPCErr(err error) error {
	switch err {
	case nil, io.EOF:
		return err
	case context.DeadlineExceeded:
		return errContextDeadline
	case context.Canceled:
		return errContextCanceled
	case io.ErrUnexpectedEOF:
		return status.Error(codes.Internal, err.Error())
	}

	switch e := err.(type) {
	case transport.ConnectionError:
		return status.Error(codes.Unavailable, e.Desc)
	case *transport.NewStreamError:
		return toRPCErr(e.Err)
	}

	if _, ok := status.FromError(err); ok {
		return err
	}

	return status.Error(codes.Unknown, err.Error())
}

// setCallInfoCodec should only be called after CallOptions have been applied.
func setCallInfoCodec(c *callInfo) error {
	if c.codec != nil {
		// codec was already set by a CallOption; use it, but set the content
		// subtype if it is not set.
		if c.contentSubtype == "" {
			// c.codec is a baseCodec to hide the difference between grpc.Codec and
			// encoding.Codec (Name vs. String method name).  We only support
			// setting content subtype from encoding.Codec to avoid a behavior
			// change with the deprecated version.
			if ec, ok := c.codec.(encoding.CodecV2); ok {
				c.contentSubtype = strings.ToLower(ec.Name())
			}
		}
		return nil
	}

	if c.contentSubtype == "" {
		// No codec specified in CallOptions; use proto by default.
		c.codec = getCodec(proto.Name)
		return nil
	}

	// c.contentSubtype is already lowercased in CallContentSubtype
	c.codec = getCodec(c.contentSubtype)
	if c.codec == nil {
		return status.Errorf(codes.Internal, "no codec registered for content-subtype %s", c.contentSubtype)
	}
	return nil
}

// The SupportPackageIsVersion variables are referenced from generated protocol
// buffer files to ensure compatibility with the gRPC version used.  The latest
// support package version is 9.
//
// Older versions are kept for compatibility.
//
// These constants should not be referenced from any other code.
const (
	SupportPackageIsVersion3 = true
	SupportPackageIsVersion4 = true
	SupportPackageIsVersion5 = true
	SupportPackageIsVersion6 = true
	SupportPackageIsVersion7 = true
	SupportPackageIsVersion8 = true
	SupportPackageIsVersion9 = true
)

const grpcUA = "grpc-go/" + Version
