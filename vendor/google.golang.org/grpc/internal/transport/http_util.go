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

package transport

import (
	"bufio"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
	"google.golang.org/grpc/codes"
)

const (
	// http2MaxFrameLen specifies the max length of a HTTP2 frame.
	http2MaxFrameLen = 16384 // 16KB frame
	// https://httpwg.org/specs/rfc7540.html#SettingValues
	http2InitHeaderTableSize = 4096
)

var (
	clientPreface   = []byte(http2.ClientPreface)
	http2ErrConvTab = map[http2.ErrCode]codes.Code{
		http2.ErrCodeNo:                 codes.Internal,
		http2.ErrCodeProtocol:           codes.Internal,
		http2.ErrCodeInternal:           codes.Internal,
		http2.ErrCodeFlowControl:        codes.ResourceExhausted,
		http2.ErrCodeSettingsTimeout:    codes.Internal,
		http2.ErrCodeStreamClosed:       codes.Internal,
		http2.ErrCodeFrameSize:          codes.Internal,
		http2.ErrCodeRefusedStream:      codes.Unavailable,
		http2.ErrCodeCancel:             codes.Canceled,
		http2.ErrCodeCompression:        codes.Internal,
		http2.ErrCodeConnect:            codes.Internal,
		http2.ErrCodeEnhanceYourCalm:    codes.ResourceExhausted,
		http2.ErrCodeInadequateSecurity: codes.PermissionDenied,
		http2.ErrCodeHTTP11Required:     codes.Internal,
	}
	// HTTPStatusConvTab is the HTTP status code to gRPC error code conversion table.
	HTTPStatusConvTab = map[int]codes.Code{
		// 400 Bad Request - INTERNAL.
		http.StatusBadRequest: codes.Internal,
		// 401 Unauthorized  - UNAUTHENTICATED.
		http.StatusUnauthorized: codes.Unauthenticated,
		// 403 Forbidden - PERMISSION_DENIED.
		http.StatusForbidden: codes.PermissionDenied,
		// 404 Not Found - UNIMPLEMENTED.
		http.StatusNotFound: codes.Unimplemented,
		// 429 Too Many Requests - UNAVAILABLE.
		http.StatusTooManyRequests: codes.Unavailable,
		// 502 Bad Gateway - UNAVAILABLE.
		http.StatusBadGateway: codes.Unavailable,
		// 503 Service Unavailable - UNAVAILABLE.
		http.StatusServiceUnavailable: codes.Unavailable,
		// 504 Gateway timeout - UNAVAILABLE.
		http.StatusGatewayTimeout: codes.Unavailable,
	}
)

var grpcStatusDetailsBinHeader = "grpc-status-details-bin"

// isReservedHeader checks whether hdr belongs to HTTP2 headers
// reserved by gRPC protocol. Any other headers are classified as the
// user-specified metadata.
func isReservedHeader(hdr string) bool {
	if hdr != "" && hdr[0] == ':' {
		return true
	}
	switch hdr {
	case "content-type",
		"user-agent",
		"grpc-message-type",
		"grpc-encoding",
		"grpc-message",
		"grpc-status",
		"grpc-timeout",
		// Intentionally exclude grpc-previous-rpc-attempts and
		// grpc-retry-pushback-ms, which are "reserved", but their API
		// intentionally works via metadata.
		"te":
		return true
	default:
		return false
	}
}

// isWhitelistedHeader checks whether hdr should be propagated into metadata
// visible to users, even though it is classified as "reserved", above.
func isWhitelistedHeader(hdr string) bool {
	switch hdr {
	case ":authority", "user-agent":
		return true
	default:
		return false
	}
}

const binHdrSuffix = "-bin"

func encodeBinHeader(v []byte) string {
	return base64.RawStdEncoding.EncodeToString(v)
}

func decodeBinHeader(v string) ([]byte, error) {
	if len(v)%4 == 0 {
		// Input was padded, or padding was not necessary.
		return base64.StdEncoding.DecodeString(v)
	}
	return base64.RawStdEncoding.DecodeString(v)
}

func encodeMetadataHeader(k, v string) string {
	if strings.HasSuffix(k, binHdrSuffix) {
		return encodeBinHeader(([]byte)(v))
	}
	return v
}

func decodeMetadataHeader(k, v string) (string, error) {
	if strings.HasSuffix(k, binHdrSuffix) {
		b, err := decodeBinHeader(v)
		return string(b), err
	}
	return v, nil
}

type timeoutUnit uint8

const (
	hour        timeoutUnit = 'H'
	minute      timeoutUnit = 'M'
	second      timeoutUnit = 'S'
	millisecond timeoutUnit = 'm'
	microsecond timeoutUnit = 'u'
	nanosecond  timeoutUnit = 'n'
)

func timeoutUnitToDuration(u timeoutUnit) (d time.Duration, ok bool) {
	switch u {
	case hour:
		return time.Hour, true
	case minute:
		return time.Minute, true
	case second:
		return time.Second, true
	case millisecond:
		return time.Millisecond, true
	case microsecond:
		return time.Microsecond, true
	case nanosecond:
		return time.Nanosecond, true
	default:
	}
	return
}

func decodeTimeout(s string) (time.Duration, error) {
	size := len(s)
	if size < 2 {
		return 0, fmt.Errorf("transport: timeout string is too short: %q", s)
	}
	if size > 9 {
		// Spec allows for 8 digits plus the unit.
		return 0, fmt.Errorf("transport: timeout string is too long: %q", s)
	}
	unit := timeoutUnit(s[size-1])
	d, ok := timeoutUnitToDuration(unit)
	if !ok {
		return 0, fmt.Errorf("transport: timeout unit is not recognized: %q", s)
	}
	t, err := strconv.ParseInt(s[:size-1], 10, 64)
	if err != nil {
		return 0, err
	}
	const maxHours = math.MaxInt64 / int64(time.Hour)
	if d == time.Hour && t > maxHours {
		// This timeout would overflow math.MaxInt64; clamp it.
		return time.Duration(math.MaxInt64), nil
	}
	return d * time.Duration(t), nil
}

const (
	spaceByte   = ' '
	tildeByte   = '~'
	percentByte = '%'
)

// encodeGrpcMessage is used to encode status code in header field
// "grpc-message". It does percent encoding and also replaces invalid utf-8
// characters with Unicode replacement character.
//
// It checks to see if each individual byte in msg is an allowable byte, and
// then either percent encoding or passing it through. When percent encoding,
// the byte is converted into hexadecimal notation with a '%' prepended.
func encodeGrpcMessage(msg string) string {
	if msg == "" {
		return ""
	}
	lenMsg := len(msg)
	for i := 0; i < lenMsg; i++ {
		c := msg[i]
		if !(c >= spaceByte && c <= tildeByte && c != percentByte) {
			return encodeGrpcMessageUnchecked(msg)
		}
	}
	return msg
}

func encodeGrpcMessageUnchecked(msg string) string {
	var sb strings.Builder
	for len(msg) > 0 {
		r, size := utf8.DecodeRuneInString(msg)
		for _, b := range []byte(string(r)) {
			if size > 1 {
				// If size > 1, r is not ascii. Always do percent encoding.
				fmt.Fprintf(&sb, "%%%02X", b)
				continue
			}

			// The for loop is necessary even if size == 1. r could be
			// utf8.RuneError.
			//
			// fmt.Sprintf("%%%02X", utf8.RuneError) gives "%FFFD".
			if b >= spaceByte && b <= tildeByte && b != percentByte {
				sb.WriteByte(b)
			} else {
				fmt.Fprintf(&sb, "%%%02X", b)
			}
		}
		msg = msg[size:]
	}
	return sb.String()
}

// decodeGrpcMessage decodes the msg encoded by encodeGrpcMessage.
func decodeGrpcMessage(msg string) string {
	if msg == "" {
		return ""
	}
	lenMsg := len(msg)
	for i := 0; i < lenMsg; i++ {
		if msg[i] == percentByte && i+2 < lenMsg {
			return decodeGrpcMessageUnchecked(msg)
		}
	}
	return msg
}

func decodeGrpcMessageUnchecked(msg string) string {
	var sb strings.Builder
	lenMsg := len(msg)
	for i := 0; i < lenMsg; i++ {
		c := msg[i]
		if c == percentByte && i+2 < lenMsg {
			parsed, err := strconv.ParseUint(msg[i+1:i+3], 16, 8)
			if err != nil {
				sb.WriteByte(c)
			} else {
				sb.WriteByte(byte(parsed))
				i += 2
			}
		} else {
			sb.WriteByte(c)
		}
	}
	return sb.String()
}

type bufWriter struct {
	pool      *sync.Pool
	buf       []byte
	offset    int
	batchSize int
	conn      net.Conn
	err       error
}

func newBufWriter(conn net.Conn, batchSize int, pool *sync.Pool) *bufWriter {
	w := &bufWriter{
		batchSize: batchSize,
		conn:      conn,
		pool:      pool,
	}
	// this indicates that we should use non shared buf
	if pool == nil {
		w.buf = make([]byte, batchSize)
	}
	return w
}

func (w *bufWriter) Write(b []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	if w.batchSize == 0 { // Buffer has been disabled.
		n, err := w.conn.Write(b)
		return n, toIOError(err)
	}
	if w.buf == nil {
		b := w.pool.Get().(*[]byte)
		w.buf = *b
	}
	written := 0
	for len(b) > 0 {
		copied := copy(w.buf[w.offset:], b)
		b = b[copied:]
		written += copied
		w.offset += copied
		if w.offset < w.batchSize {
			continue
		}
		if err := w.flushKeepBuffer(); err != nil {
			return written, err
		}
	}
	return written, nil
}

func (w *bufWriter) Flush() error {
	err := w.flushKeepBuffer()
	// Only release the buffer if we are in a "shared" mode
	if w.buf != nil && w.pool != nil {
		b := w.buf
		w.pool.Put(&b)
		w.buf = nil
	}
	return err
}

func (w *bufWriter) flushKeepBuffer() error {
	if w.err != nil {
		return w.err
	}
	if w.offset == 0 {
		return nil
	}
	_, w.err = w.conn.Write(w.buf[:w.offset])
	w.err = toIOError(w.err)
	w.offset = 0
	return w.err
}

type ioError struct {
	error
}

func (i ioError) Unwrap() error {
	return i.error
}

func isIOError(err error) bool {
	return errors.As(err, &ioError{})
}

func toIOError(err error) error {
	if err == nil {
		return nil
	}
	return ioError{error: err}
}

type framer struct {
	writer *bufWriter
	fr     *http2.Framer
}

var writeBufferPoolMap = make(map[int]*sync.Pool)
var writeBufferMutex sync.Mutex

func newFramer(conn net.Conn, writeBufferSize, readBufferSize int, sharedWriteBuffer bool, maxHeaderListSize uint32) *framer {
	if writeBufferSize < 0 {
		writeBufferSize = 0
	}
	var r io.Reader = conn
	if readBufferSize > 0 {
		r = bufio.NewReaderSize(r, readBufferSize)
	}
	var pool *sync.Pool
	if sharedWriteBuffer {
		pool = getWriteBufferPool(writeBufferSize)
	}
	w := newBufWriter(conn, writeBufferSize, pool)
	f := &framer{
		writer: w,
		fr:     http2.NewFramer(w, r),
	}
	f.fr.SetMaxReadFrameSize(http2MaxFrameLen)
	// Opt-in to Frame reuse API on framer to reduce garbage.
	// Frames aren't safe to read from after a subsequent call to ReadFrame.
	f.fr.SetReuseFrames()
	f.fr.MaxHeaderListSize = maxHeaderListSize
	f.fr.ReadMetaHeaders = hpack.NewDecoder(http2InitHeaderTableSize, nil)
	return f
}

func getWriteBufferPool(size int) *sync.Pool {
	writeBufferMutex.Lock()
	defer writeBufferMutex.Unlock()
	pool, ok := writeBufferPoolMap[size]
	if ok {
		return pool
	}
	pool = &sync.Pool{
		New: func() any {
			b := make([]byte, size)
			return &b
		},
	}
	writeBufferPoolMap[size] = pool
	return pool
}

// parseDialTarget returns the network and address to pass to dialer.
func parseDialTarget(target string) (string, string) {
	net := "tcp"
	m1 := strings.Index(target, ":")
	m2 := strings.Index(target, ":/")
	// handle unix:addr which will fail with url.Parse
	if m1 >= 0 && m2 < 0 {
		if n := target[0:m1]; n == "unix" {
			return n, target[m1+1:]
		}
	}
	if m2 >= 0 {
		t, err := url.Parse(target)
		if err != nil {
			return net, target
		}
		scheme := t.Scheme
		addr := t.Path
		if scheme == "unix" {
			if addr == "" {
				addr = t.Host
			}
			return scheme, addr
		}
	}
	return net, target
}
