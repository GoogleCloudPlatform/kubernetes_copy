/*
 *
 * Copyright 2016 gRPC authors.
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

// This file is the implementation of a gRPC server using HTTP/2 which
// uses the standard Go http2 Server implementation (via the
// http.Handler interface), rather than speaking low-level HTTP/2
// frames itself. It is the implementation of *grpc.Server.ServeHTTP.

package transport

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/http2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/internal/grpclog"
	"google.golang.org/grpc/internal/grpcutil"
	"google.golang.org/grpc/mem"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/stats"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// NewServerHandlerTransport returns a ServerTransport handling gRPC from
// inside an http.Handler, or writes an HTTP error to w and returns an error.
// It requires that the http Server supports HTTP/2.
func NewServerHandlerTransport(w http.ResponseWriter, r *http.Request, stats []stats.Handler, bufferPool mem.BufferPool) (ServerTransport, error) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		msg := fmt.Sprintf("invalid gRPC request method %q", r.Method)
		http.Error(w, msg, http.StatusMethodNotAllowed)
		return nil, errors.New(msg)
	}
	contentType := r.Header.Get("Content-Type")
	// TODO: do we assume contentType is lowercase? we did before
	contentSubtype, validContentType := grpcutil.ContentSubtype(contentType)
	if !validContentType {
		msg := fmt.Sprintf("invalid gRPC request content-type %q", contentType)
		http.Error(w, msg, http.StatusUnsupportedMediaType)
		return nil, errors.New(msg)
	}
	if r.ProtoMajor != 2 {
		msg := "gRPC requires HTTP/2"
		http.Error(w, msg, http.StatusHTTPVersionNotSupported)
		return nil, errors.New(msg)
	}
	if _, ok := w.(http.Flusher); !ok {
		msg := "gRPC requires a ResponseWriter supporting http.Flusher"
		http.Error(w, msg, http.StatusInternalServerError)
		return nil, errors.New(msg)
	}

	var localAddr net.Addr
	if la := r.Context().Value(http.LocalAddrContextKey); la != nil {
		localAddr, _ = la.(net.Addr)
	}
	var authInfo credentials.AuthInfo
	if r.TLS != nil {
		authInfo = credentials.TLSInfo{State: *r.TLS, CommonAuthInfo: credentials.CommonAuthInfo{SecurityLevel: credentials.PrivacyAndIntegrity}}
	}
	p := peer.Peer{
		Addr:      strAddr(r.RemoteAddr),
		LocalAddr: localAddr,
		AuthInfo:  authInfo,
	}
	st := &serverHandlerTransport{
		rw:             w,
		req:            r,
		closedCh:       make(chan struct{}),
		writes:         make(chan func()),
		peer:           p,
		contentType:    contentType,
		contentSubtype: contentSubtype,
		stats:          stats,
		bufferPool:     bufferPool,
	}
	st.logger = prefixLoggerForServerHandlerTransport(st)

	if v := r.Header.Get("grpc-timeout"); v != "" {
		to, err := decodeTimeout(v)
		if err != nil {
			msg := fmt.Sprintf("malformed grpc-timeout: %v", err)
			http.Error(w, msg, http.StatusBadRequest)
			return nil, status.Error(codes.Internal, msg)
		}
		st.timeoutSet = true
		st.timeout = to
	}

	metakv := []string{"content-type", contentType}
	if r.Host != "" {
		metakv = append(metakv, ":authority", r.Host)
	}
	for k, vv := range r.Header {
		k = strings.ToLower(k)
		if isReservedHeader(k) && !isWhitelistedHeader(k) {
			continue
		}
		for _, v := range vv {
			v, err := decodeMetadataHeader(k, v)
			if err != nil {
				msg := fmt.Sprintf("malformed binary metadata %q in header %q: %v", v, k, err)
				http.Error(w, msg, http.StatusBadRequest)
				return nil, status.Error(codes.Internal, msg)
			}
			metakv = append(metakv, k, v)
		}
	}
	st.headerMD = metadata.Pairs(metakv...)

	return st, nil
}

// serverHandlerTransport is an implementation of ServerTransport
// which replies to exactly one gRPC request (exactly one HTTP request),
// using the net/http.Handler interface. This http.Handler is guaranteed
// at this point to be speaking over HTTP/2, so it's able to speak valid
// gRPC.
type serverHandlerTransport struct {
	rw         http.ResponseWriter
	req        *http.Request
	timeoutSet bool
	timeout    time.Duration

	headerMD metadata.MD

	peer peer.Peer

	closeOnce sync.Once
	closedCh  chan struct{} // closed on Close

	// writes is a channel of code to run serialized in the
	// ServeHTTP (HandleStreams) goroutine. The channel is closed
	// when WriteStatus is called.
	writes chan func()

	// block concurrent WriteStatus calls
	// e.g. grpc/(*serverStream).SendMsg/RecvMsg
	writeStatusMu sync.Mutex

	// we just mirror the request content-type
	contentType string
	// we store both contentType and contentSubtype so we don't keep recreating them
	// TODO make sure this is consistent across handler_server and http2_server
	contentSubtype string

	stats  []stats.Handler
	logger *grpclog.PrefixLogger

	bufferPool mem.BufferPool
}

func (ht *serverHandlerTransport) Close(err error) {
	ht.closeOnce.Do(func() {
		if ht.logger.V(logLevel) {
			ht.logger.Infof("Closing: %v", err)
		}
		close(ht.closedCh)
	})
}

func (ht *serverHandlerTransport) Peer() *peer.Peer {
	return &peer.Peer{
		Addr:      ht.peer.Addr,
		LocalAddr: ht.peer.LocalAddr,
		AuthInfo:  ht.peer.AuthInfo,
	}
}

// strAddr is a net.Addr backed by either a TCP "ip:port" string, or
// the empty string if unknown.
type strAddr string

func (a strAddr) Network() string {
	if a != "" {
		// Per the documentation on net/http.Request.RemoteAddr, if this is
		// set, it's set to the IP:port of the peer (hence, TCP):
		// https://golang.org/pkg/net/http/#Request
		//
		// If we want to support Unix sockets later, we can
		// add our own grpc-specific convention within the
		// grpc codebase to set RemoteAddr to a different
		// format, or probably better: we can attach it to the
		// context and use that from serverHandlerTransport.RemoteAddr.
		return "tcp"
	}
	return ""
}

func (a strAddr) String() string { return string(a) }

// do runs fn in the ServeHTTP goroutine.
func (ht *serverHandlerTransport) do(fn func()) error {
	select {
	case <-ht.closedCh:
		return ErrConnClosing
	case ht.writes <- fn:
		return nil
	}
}

func (ht *serverHandlerTransport) WriteStatus(s *Stream, st *status.Status) error {
	ht.writeStatusMu.Lock()
	defer ht.writeStatusMu.Unlock()

	headersWritten := s.updateHeaderSent()
	err := ht.do(func() {
		if !headersWritten {
			ht.writePendingHeaders(s)
		}

		// And flush, in case no header or body has been sent yet.
		// This forces a separation of headers and trailers if this is the
		// first call (for example, in end2end tests's TestNoService).
		ht.rw.(http.Flusher).Flush()

		h := ht.rw.Header()
		h.Set("Grpc-Status", fmt.Sprintf("%d", st.Code()))
		if m := st.Message(); m != "" {
			h.Set("Grpc-Message", encodeGrpcMessage(m))
		}

		s.hdrMu.Lock()
		defer s.hdrMu.Unlock()
		if p := st.Proto(); p != nil && len(p.Details) > 0 {
			delete(s.trailer, grpcStatusDetailsBinHeader)
			stBytes, err := proto.Marshal(p)
			if err != nil {
				// TODO: return error instead, when callers are able to handle it.
				panic(err)
			}

			h.Set(grpcStatusDetailsBinHeader, encodeBinHeader(stBytes))
		}

		if len(s.trailer) > 0 {
			for k, vv := range s.trailer {
				// Clients don't tolerate reading restricted headers after some non restricted ones were sent.
				if isReservedHeader(k) {
					continue
				}
				for _, v := range vv {
					// http2 ResponseWriter mechanism to send undeclared Trailers after
					// the headers have possibly been written.
					h.Add(http2.TrailerPrefix+k, encodeMetadataHeader(k, v))
				}
			}
		}
	})

	if err == nil { // transport has not been closed
		// Note: The trailer fields are compressed with hpack after this call returns.
		// No WireLength field is set here.
		for _, sh := range ht.stats {
			sh.HandleRPC(s.Context(), &stats.OutTrailer{
				Trailer: s.trailer.Copy(),
			})
		}
	}
	ht.Close(errors.New("finished writing status"))
	return err
}

// writePendingHeaders sets common and custom headers on the first
// write call (Write, WriteHeader, or WriteStatus)
func (ht *serverHandlerTransport) writePendingHeaders(s *Stream) {
	ht.writeCommonHeaders(s)
	ht.writeCustomHeaders(s)
}

// writeCommonHeaders sets common headers on the first write
// call (Write, WriteHeader, or WriteStatus).
func (ht *serverHandlerTransport) writeCommonHeaders(s *Stream) {
	h := ht.rw.Header()
	h["Date"] = nil // suppress Date to make tests happy; TODO: restore
	h.Set("Content-Type", ht.contentType)

	// Predeclare trailers we'll set later in WriteStatus (after the body).
	// This is a SHOULD in the HTTP RFC, and the way you add (known)
	// Trailers per the net/http.ResponseWriter contract.
	// See https://golang.org/pkg/net/http/#ResponseWriter
	// and https://golang.org/pkg/net/http/#example_ResponseWriter_trailers
	h.Add("Trailer", "Grpc-Status")
	h.Add("Trailer", "Grpc-Message")
	h.Add("Trailer", "Grpc-Status-Details-Bin")

	if s.sendCompress != "" {
		h.Set("Grpc-Encoding", s.sendCompress)
	}
}

// writeCustomHeaders sets custom headers set on the stream via SetHeader
// on the first write call (Write, WriteHeader, or WriteStatus)
func (ht *serverHandlerTransport) writeCustomHeaders(s *Stream) {
	h := ht.rw.Header()

	s.hdrMu.Lock()
	for k, vv := range s.header {
		if isReservedHeader(k) {
			continue
		}
		for _, v := range vv {
			h.Add(k, encodeMetadataHeader(k, v))
		}
	}

	s.hdrMu.Unlock()
}

func (ht *serverHandlerTransport) Write(s *Stream, hdr []byte, data mem.BufferSlice, _ *Options) error {
	// Always take a reference because otherwise there is no guarantee the data will
	// be available after this function returns. This is what callers to Write
	// expect.
	data.Ref()
	headersWritten := s.updateHeaderSent()
	err := ht.do(func() {
		defer data.Free()
		if !headersWritten {
			ht.writePendingHeaders(s)
		}
		ht.rw.Write(hdr)
		for _, b := range data {
			_, _ = ht.rw.Write(b.ReadOnlyData())
		}
		ht.rw.(http.Flusher).Flush()
	})
	if err != nil {
		data.Free()
		return err
	}
	return nil
}

func (ht *serverHandlerTransport) WriteHeader(s *Stream, md metadata.MD) error {
	if err := s.SetHeader(md); err != nil {
		return err
	}

	headersWritten := s.updateHeaderSent()
	err := ht.do(func() {
		if !headersWritten {
			ht.writePendingHeaders(s)
		}

		ht.rw.WriteHeader(200)
		ht.rw.(http.Flusher).Flush()
	})

	if err == nil {
		for _, sh := range ht.stats {
			// Note: The header fields are compressed with hpack after this call returns.
			// No WireLength field is set here.
			sh.HandleRPC(s.Context(), &stats.OutHeader{
				Header:      md.Copy(),
				Compression: s.sendCompress,
			})
		}
	}
	return err
}

func (ht *serverHandlerTransport) HandleStreams(ctx context.Context, startStream func(*Stream)) {
	// With this transport type there will be exactly 1 stream: this HTTP request.
	var cancel context.CancelFunc
	if ht.timeoutSet {
		ctx, cancel = context.WithTimeout(ctx, ht.timeout)
	} else {
		ctx, cancel = context.WithCancel(ctx)
	}

	// requestOver is closed when the status has been written via WriteStatus.
	requestOver := make(chan struct{})
	go func() {
		select {
		case <-requestOver:
		case <-ht.closedCh:
		case <-ht.req.Context().Done():
		}
		cancel()
		ht.Close(errors.New("request is done processing"))
	}()

	ctx = metadata.NewIncomingContext(ctx, ht.headerMD)
	req := ht.req
	s := &Stream{
		id:               0, // irrelevant
		ctx:              ctx,
		requestRead:      func(int) {},
		cancel:           cancel,
		buf:              newRecvBuffer(),
		st:               ht,
		method:           req.URL.Path,
		recvCompress:     req.Header.Get("grpc-encoding"),
		contentSubtype:   ht.contentSubtype,
		headerWireLength: 0, // won't have access to header wire length until golang/go#18997.
	}
	s.trReader = &transportReader{
		reader:        &recvBufferReader{ctx: s.ctx, ctxDone: s.ctx.Done(), recv: s.buf},
		windowHandler: func(int) {},
	}

	// readerDone is closed when the Body.Read-ing goroutine exits.
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)

		for {
			buf := ht.bufferPool.Get(http2MaxFrameLen)
			n, err := req.Body.Read(*buf)
			if n > 0 {
				*buf = (*buf)[:n]
				s.buf.put(recvMsg{buffer: mem.NewBuffer(buf, ht.bufferPool)})
			} else {
				ht.bufferPool.Put(buf)
			}
			if err != nil {
				s.buf.put(recvMsg{err: mapRecvMsgError(err)})
				return
			}
		}
	}()

	// startStream is provided by the *grpc.Server's serveStreams.
	// It starts a goroutine serving s and exits immediately.
	// The goroutine that is started is the one that then calls
	// into ht, calling WriteHeader, Write, WriteStatus, Close, etc.
	startStream(s)

	ht.runStream()
	close(requestOver)

	// Wait for reading goroutine to finish.
	req.Body.Close()
	<-readerDone
}

func (ht *serverHandlerTransport) runStream() {
	for {
		select {
		case fn := <-ht.writes:
			fn()
		case <-ht.closedCh:
			return
		}
	}
}

func (ht *serverHandlerTransport) IncrMsgSent() {}

func (ht *serverHandlerTransport) IncrMsgRecv() {}

func (ht *serverHandlerTransport) Drain(string) {
	panic("Drain() is not implemented")
}

// mapRecvMsgError returns the non-nil err into the appropriate
// error value as expected by callers of *grpc.parser.recvMsg.
// In particular, in can only be:
//   - io.EOF
//   - io.ErrUnexpectedEOF
//   - of type transport.ConnectionError
//   - an error from the status package
func mapRecvMsgError(err error) error {
	if err == io.EOF || err == io.ErrUnexpectedEOF {
		return err
	}
	if se, ok := err.(http2.StreamError); ok {
		if code, ok := http2ErrConvTab[se.Code]; ok {
			return status.Error(code, se.Error())
		}
	}
	if strings.Contains(err.Error(), "body closed by handler") {
		return status.Error(codes.Canceled, err.Error())
	}
	return connectionErrorf(true, err, err.Error())
}
