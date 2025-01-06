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
	"context"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/internal"
	"google.golang.org/grpc/internal/channelz"
	icredentials "google.golang.org/grpc/internal/credentials"
	"google.golang.org/grpc/internal/grpclog"
	"google.golang.org/grpc/internal/grpcsync"
	"google.golang.org/grpc/internal/grpcutil"
	imetadata "google.golang.org/grpc/internal/metadata"
	istatus "google.golang.org/grpc/internal/status"
	isyscall "google.golang.org/grpc/internal/syscall"
	"google.golang.org/grpc/internal/transport/networktype"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/mem"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/resolver"
	"google.golang.org/grpc/stats"
	"google.golang.org/grpc/status"
)

// clientConnectionCounter counts the number of connections a client has
// initiated (equal to the number of http2Clients created). Must be accessed
// atomically.
var clientConnectionCounter uint64

var goAwayLoopyWriterTimeout = 5 * time.Second

var metadataFromOutgoingContextRaw = internal.FromOutgoingContextRaw.(func(context.Context) (metadata.MD, [][]string, bool))

// http2Client implements the ClientTransport interface with HTTP2.
type http2Client struct {
	lastRead  int64 // Keep this field 64-bit aligned. Accessed atomically.
	ctx       context.Context
	cancel    context.CancelFunc
	ctxDone   <-chan struct{} // Cache the ctx.Done() chan.
	userAgent string
	// address contains the resolver returned address for this transport.
	// If the `ServerName` field is set, it takes precedence over `CallHdr.Host`
	// passed to `NewStream`, when determining the :authority header.
	address    resolver.Address
	md         metadata.MD
	conn       net.Conn // underlying communication channel
	loopy      *loopyWriter
	remoteAddr net.Addr
	localAddr  net.Addr
	authInfo   credentials.AuthInfo // auth info about the connection

	readerDone chan struct{} // sync point to enable testing.
	writerDone chan struct{} // sync point to enable testing.
	// goAway is closed to notify the upper layer (i.e., addrConn.transportMonitor)
	// that the server sent GoAway on this transport.
	goAway        chan struct{}
	keepaliveDone chan struct{} // Closed when the keepalive goroutine exits.
	framer        *framer
	// controlBuf delivers all the control related tasks (e.g., window
	// updates, reset streams, and various settings) to the controller.
	// Do not access controlBuf with mu held.
	controlBuf *controlBuffer
	fc         *trInFlow
	// The scheme used: https if TLS is on, http otherwise.
	scheme string

	isSecure bool

	perRPCCreds []credentials.PerRPCCredentials

	kp               keepalive.ClientParameters
	keepaliveEnabled bool

	statsHandlers []stats.Handler

	initialWindowSize int32

	// configured by peer through SETTINGS_MAX_HEADER_LIST_SIZE
	maxSendHeaderListSize *uint32

	bdpEst *bdpEstimator

	maxConcurrentStreams  uint32
	streamQuota           int64
	streamsQuotaAvailable chan struct{}
	waitingStreams        uint32
	registeredCompressors string

	// Do not access controlBuf with mu held.
	mu            sync.Mutex // guard the following variables
	nextID        uint32
	state         transportState
	activeStreams map[uint32]*Stream
	// prevGoAway ID records the Last-Stream-ID in the previous GOAway frame.
	prevGoAwayID uint32
	// goAwayReason records the http2.ErrCode and debug data received with the
	// GoAway frame.
	goAwayReason GoAwayReason
	// goAwayDebugMessage contains a detailed human readable string about a
	// GoAway frame, useful for error messages.
	goAwayDebugMessage string
	// A condition variable used to signal when the keepalive goroutine should
	// go dormant. The condition for dormancy is based on the number of active
	// streams and the `PermitWithoutStream` keepalive client parameter. And
	// since the number of active streams is guarded by the above mutex, we use
	// the same for this condition variable as well.
	kpDormancyCond *sync.Cond
	// A boolean to track whether the keepalive goroutine is dormant or not.
	// This is checked before attempting to signal the above condition
	// variable.
	kpDormant bool

	channelz *channelz.Socket

	onClose func(GoAwayReason)

	bufferPool mem.BufferPool

	connectionID uint64
	logger       *grpclog.PrefixLogger
}

func dial(ctx context.Context, fn func(context.Context, string) (net.Conn, error), addr resolver.Address, useProxy bool, grpcUA string) (net.Conn, error) {
	address := addr.Addr
	networkType, ok := networktype.Get(addr)
	if fn != nil {
		// Special handling for unix scheme with custom dialer. Back in the day,
		// we did not have a unix resolver and therefore targets with a unix
		// scheme would end up using the passthrough resolver. So, user's used a
		// custom dialer in this case and expected the original dial target to
		// be passed to the custom dialer. Now, we have a unix resolver. But if
		// a custom dialer is specified, we want to retain the old behavior in
		// terms of the address being passed to the custom dialer.
		if networkType == "unix" && !strings.HasPrefix(address, "\x00") {
			// Supported unix targets are either "unix://absolute-path" or
			// "unix:relative-path".
			if filepath.IsAbs(address) {
				return fn(ctx, "unix://"+address)
			}
			return fn(ctx, "unix:"+address)
		}
		return fn(ctx, address)
	}
	if !ok {
		networkType, address = parseDialTarget(address)
	}
	if networkType == "tcp" && useProxy {
		return proxyDial(ctx, address, grpcUA)
	}
	return internal.NetDialerWithTCPKeepalive().DialContext(ctx, networkType, address)
}

func isTemporary(err error) bool {
	switch err := err.(type) {
	case interface {
		Temporary() bool
	}:
		return err.Temporary()
	case interface {
		Timeout() bool
	}:
		// Timeouts may be resolved upon retry, and are thus treated as
		// temporary.
		return err.Timeout()
	}
	return true
}

// newHTTP2Client constructs a connected ClientTransport to addr based on HTTP2
// and starts to receive messages on it. Non-nil error returns if construction
// fails.
func newHTTP2Client(connectCtx, ctx context.Context, addr resolver.Address, opts ConnectOptions, onClose func(GoAwayReason)) (_ *http2Client, err error) {
	scheme := "http"
	ctx, cancel := context.WithCancel(ctx)
	defer func() {
		if err != nil {
			cancel()
		}
	}()

	// gRPC, resolver, balancer etc. can specify arbitrary data in the
	// Attributes field of resolver.Address, which is shoved into connectCtx
	// and passed to the dialer and credential handshaker. This makes it possible for
	// address specific arbitrary data to reach custom dialers and credential handshakers.
	connectCtx = icredentials.NewClientHandshakeInfoContext(connectCtx, credentials.ClientHandshakeInfo{Attributes: addr.Attributes})

	conn, err := dial(connectCtx, opts.Dialer, addr, opts.UseProxy, opts.UserAgent)
	if err != nil {
		if opts.FailOnNonTempDialError {
			return nil, connectionErrorf(isTemporary(err), err, "transport: error while dialing: %v", err)
		}
		return nil, connectionErrorf(true, err, "transport: Error while dialing: %v", err)
	}

	// Any further errors will close the underlying connection
	defer func(conn net.Conn) {
		if err != nil {
			conn.Close()
		}
	}(conn)

	// The following defer and goroutine monitor the connectCtx for cancellation
	// and deadline.  On context expiration, the connection is hard closed and
	// this function will naturally fail as a result.  Otherwise, the defer
	// waits for the goroutine to exit to prevent the context from being
	// monitored (and to prevent the connection from ever being closed) after
	// returning from this function.
	ctxMonitorDone := grpcsync.NewEvent()
	newClientCtx, newClientDone := context.WithCancel(connectCtx)
	defer func() {
		newClientDone()         // Awaken the goroutine below if connectCtx hasn't expired.
		<-ctxMonitorDone.Done() // Wait for the goroutine below to exit.
	}()
	go func(conn net.Conn) {
		defer ctxMonitorDone.Fire() // Signal this goroutine has exited.
		<-newClientCtx.Done()       // Block until connectCtx expires or the defer above executes.
		if err := connectCtx.Err(); err != nil {
			// connectCtx expired before exiting the function.  Hard close the connection.
			if logger.V(logLevel) {
				logger.Infof("Aborting due to connect deadline expiring: %v", err)
			}
			conn.Close()
		}
	}(conn)

	kp := opts.KeepaliveParams
	// Validate keepalive parameters.
	if kp.Time == 0 {
		kp.Time = defaultClientKeepaliveTime
	}
	if kp.Timeout == 0 {
		kp.Timeout = defaultClientKeepaliveTimeout
	}
	keepaliveEnabled := false
	if kp.Time != infinity {
		if err = isyscall.SetTCPUserTimeout(conn, kp.Timeout); err != nil {
			return nil, connectionErrorf(false, err, "transport: failed to set TCP_USER_TIMEOUT: %v", err)
		}
		keepaliveEnabled = true
	}
	var (
		isSecure bool
		authInfo credentials.AuthInfo
	)
	transportCreds := opts.TransportCredentials
	perRPCCreds := opts.PerRPCCredentials

	if b := opts.CredsBundle; b != nil {
		if t := b.TransportCredentials(); t != nil {
			transportCreds = t
		}
		if t := b.PerRPCCredentials(); t != nil {
			perRPCCreds = append(perRPCCreds, t)
		}
	}
	if transportCreds != nil {
		conn, authInfo, err = transportCreds.ClientHandshake(connectCtx, addr.ServerName, conn)
		if err != nil {
			return nil, connectionErrorf(isTemporary(err), err, "transport: authentication handshake failed: %v", err)
		}
		for _, cd := range perRPCCreds {
			if cd.RequireTransportSecurity() {
				if ci, ok := authInfo.(interface {
					GetCommonAuthInfo() credentials.CommonAuthInfo
				}); ok {
					secLevel := ci.GetCommonAuthInfo().SecurityLevel
					if secLevel != credentials.InvalidSecurityLevel && secLevel < credentials.PrivacyAndIntegrity {
						return nil, connectionErrorf(true, nil, "transport: cannot send secure credentials on an insecure connection")
					}
				}
			}
		}
		isSecure = true
		if transportCreds.Info().SecurityProtocol == "tls" {
			scheme = "https"
		}
	}
	dynamicWindow := true
	icwz := int32(initialWindowSize)
	if opts.InitialConnWindowSize >= defaultWindowSize {
		icwz = opts.InitialConnWindowSize
		dynamicWindow = false
	}
	writeBufSize := opts.WriteBufferSize
	readBufSize := opts.ReadBufferSize
	maxHeaderListSize := defaultClientMaxHeaderListSize
	if opts.MaxHeaderListSize != nil {
		maxHeaderListSize = *opts.MaxHeaderListSize
	}

	t := &http2Client{
		ctx:                   ctx,
		ctxDone:               ctx.Done(), // Cache Done chan.
		cancel:                cancel,
		userAgent:             opts.UserAgent,
		registeredCompressors: grpcutil.RegisteredCompressors(),
		address:               addr,
		conn:                  conn,
		remoteAddr:            conn.RemoteAddr(),
		localAddr:             conn.LocalAddr(),
		authInfo:              authInfo,
		readerDone:            make(chan struct{}),
		writerDone:            make(chan struct{}),
		goAway:                make(chan struct{}),
		keepaliveDone:         make(chan struct{}),
		framer:                newFramer(conn, writeBufSize, readBufSize, opts.SharedWriteBuffer, maxHeaderListSize),
		fc:                    &trInFlow{limit: uint32(icwz)},
		scheme:                scheme,
		activeStreams:         make(map[uint32]*Stream),
		isSecure:              isSecure,
		perRPCCreds:           perRPCCreds,
		kp:                    kp,
		statsHandlers:         opts.StatsHandlers,
		initialWindowSize:     initialWindowSize,
		nextID:                1,
		maxConcurrentStreams:  defaultMaxStreamsClient,
		streamQuota:           defaultMaxStreamsClient,
		streamsQuotaAvailable: make(chan struct{}, 1),
		keepaliveEnabled:      keepaliveEnabled,
		bufferPool:            opts.BufferPool,
		onClose:               onClose,
	}
	var czSecurity credentials.ChannelzSecurityValue
	if au, ok := authInfo.(credentials.ChannelzSecurityInfo); ok {
		czSecurity = au.GetSecurityValue()
	}
	t.channelz = channelz.RegisterSocket(
		&channelz.Socket{
			SocketType:       channelz.SocketTypeNormal,
			Parent:           opts.ChannelzParent,
			SocketMetrics:    channelz.SocketMetrics{},
			EphemeralMetrics: t.socketMetrics,
			LocalAddr:        t.localAddr,
			RemoteAddr:       t.remoteAddr,
			SocketOptions:    channelz.GetSocketOption(t.conn),
			Security:         czSecurity,
		})
	t.logger = prefixLoggerForClientTransport(t)
	// Add peer information to the http2client context.
	t.ctx = peer.NewContext(t.ctx, t.getPeer())

	if md, ok := addr.Metadata.(*metadata.MD); ok {
		t.md = *md
	} else if md := imetadata.Get(addr); md != nil {
		t.md = md
	}
	t.controlBuf = newControlBuffer(t.ctxDone)
	if opts.InitialWindowSize >= defaultWindowSize {
		t.initialWindowSize = opts.InitialWindowSize
		dynamicWindow = false
	}
	if dynamicWindow {
		t.bdpEst = &bdpEstimator{
			bdp:               initialWindowSize,
			updateFlowControl: t.updateFlowControl,
		}
	}
	for _, sh := range t.statsHandlers {
		t.ctx = sh.TagConn(t.ctx, &stats.ConnTagInfo{
			RemoteAddr: t.remoteAddr,
			LocalAddr:  t.localAddr,
		})
		connBegin := &stats.ConnBegin{
			Client: true,
		}
		sh.HandleConn(t.ctx, connBegin)
	}
	if t.keepaliveEnabled {
		t.kpDormancyCond = sync.NewCond(&t.mu)
		go t.keepalive()
	}

	// Start the reader goroutine for incoming messages. Each transport has a
	// dedicated goroutine which reads HTTP2 frames from the network. Then it
	// dispatches the frame to the corresponding stream entity.  When the
	// server preface is received, readerErrCh is closed.  If an error occurs
	// first, an error is pushed to the channel.  This must be checked before
	// returning from this function.
	readerErrCh := make(chan error, 1)
	go t.reader(readerErrCh)
	defer func() {
		if err != nil {
			// writerDone should be closed since the loopy goroutine
			// wouldn't have started in the case this function returns an error.
			close(t.writerDone)
			t.Close(err)
		}
	}()

	// Send connection preface to server.
	n, err := t.conn.Write(clientPreface)
	if err != nil {
		err = connectionErrorf(true, err, "transport: failed to write client preface: %v", err)
		return nil, err
	}
	if n != len(clientPreface) {
		err = connectionErrorf(true, nil, "transport: preface mismatch, wrote %d bytes; want %d", n, len(clientPreface))
		return nil, err
	}
	var ss []http2.Setting

	if t.initialWindowSize != defaultWindowSize {
		ss = append(ss, http2.Setting{
			ID:  http2.SettingInitialWindowSize,
			Val: uint32(t.initialWindowSize),
		})
	}
	if opts.MaxHeaderListSize != nil {
		ss = append(ss, http2.Setting{
			ID:  http2.SettingMaxHeaderListSize,
			Val: *opts.MaxHeaderListSize,
		})
	}
	err = t.framer.fr.WriteSettings(ss...)
	if err != nil {
		err = connectionErrorf(true, err, "transport: failed to write initial settings frame: %v", err)
		return nil, err
	}
	// Adjust the connection flow control window if needed.
	if delta := uint32(icwz - defaultWindowSize); delta > 0 {
		if err := t.framer.fr.WriteWindowUpdate(0, delta); err != nil {
			err = connectionErrorf(true, err, "transport: failed to write window update: %v", err)
			return nil, err
		}
	}

	t.connectionID = atomic.AddUint64(&clientConnectionCounter, 1)

	if err := t.framer.writer.Flush(); err != nil {
		return nil, err
	}
	// Block until the server preface is received successfully or an error occurs.
	if err = <-readerErrCh; err != nil {
		return nil, err
	}
	go func() {
		t.loopy = newLoopyWriter(clientSide, t.framer, t.controlBuf, t.bdpEst, t.conn, t.logger, t.outgoingGoAwayHandler, t.bufferPool)
		if err := t.loopy.run(); !isIOError(err) {
			// Immediately close the connection, as the loopy writer returns
			// when there are no more active streams and we were draining (the
			// server sent a GOAWAY).  For I/O errors, the reader will hit it
			// after draining any remaining incoming data.
			t.conn.Close()
		}
		close(t.writerDone)
	}()
	return t, nil
}

func (t *http2Client) newStream(ctx context.Context, callHdr *CallHdr) *Stream {
	// TODO(zhaoq): Handle uint32 overflow of Stream.id.
	s := &Stream{
		ct:             t,
		done:           make(chan struct{}),
		method:         callHdr.Method,
		sendCompress:   callHdr.SendCompress,
		buf:            newRecvBuffer(),
		headerChan:     make(chan struct{}),
		contentSubtype: callHdr.ContentSubtype,
		doneFunc:       callHdr.DoneFunc,
	}
	s.wq = newWriteQuota(defaultWriteQuota, s.done)
	s.requestRead = func(n int) {
		t.adjustWindow(s, uint32(n))
	}
	// The client side stream context should have exactly the same life cycle with the user provided context.
	// That means, s.ctx should be read-only. And s.ctx is done iff ctx is done.
	// So we use the original context here instead of creating a copy.
	s.ctx = ctx
	s.trReader = &transportReader{
		reader: &recvBufferReader{
			ctx:     s.ctx,
			ctxDone: s.ctx.Done(),
			recv:    s.buf,
			closeStream: func(err error) {
				t.CloseStream(s, err)
			},
		},
		windowHandler: func(n int) {
			t.updateWindow(s, uint32(n))
		},
	}
	return s
}

func (t *http2Client) getPeer() *peer.Peer {
	return &peer.Peer{
		Addr:      t.remoteAddr,
		AuthInfo:  t.authInfo, // Can be nil
		LocalAddr: t.localAddr,
	}
}

// OutgoingGoAwayHandler writes a GOAWAY to the connection.  Always returns (false, err) as we want the GoAway
// to be the last frame loopy writes to the transport.
func (t *http2Client) outgoingGoAwayHandler(g *goAway) (bool, error) {
	t.mu.Lock()
	maxStreamID := t.nextID - 2
	t.mu.Unlock()
	if err := t.framer.fr.WriteGoAway(maxStreamID, http2.ErrCodeNo, g.debugData); err != nil {
		return false, err
	}
	return false, g.closeConn
}

func (t *http2Client) createHeaderFields(ctx context.Context, callHdr *CallHdr) ([]hpack.HeaderField, error) {
	aud := t.createAudience(callHdr)
	ri := credentials.RequestInfo{
		Method:   callHdr.Method,
		AuthInfo: t.authInfo,
	}
	ctxWithRequestInfo := icredentials.NewRequestInfoContext(ctx, ri)
	authData, err := t.getTrAuthData(ctxWithRequestInfo, aud)
	if err != nil {
		return nil, err
	}
	callAuthData, err := t.getCallAuthData(ctxWithRequestInfo, aud, callHdr)
	if err != nil {
		return nil, err
	}
	// TODO(mmukhi): Benchmark if the performance gets better if count the metadata and other header fields
	// first and create a slice of that exact size.
	// Make the slice of certain predictable size to reduce allocations made by append.
	hfLen := 7 // :method, :scheme, :path, :authority, content-type, user-agent, te
	hfLen += len(authData) + len(callAuthData)
	headerFields := make([]hpack.HeaderField, 0, hfLen)
	headerFields = append(headerFields, hpack.HeaderField{Name: ":method", Value: "POST"})
	headerFields = append(headerFields, hpack.HeaderField{Name: ":scheme", Value: t.scheme})
	headerFields = append(headerFields, hpack.HeaderField{Name: ":path", Value: callHdr.Method})
	headerFields = append(headerFields, hpack.HeaderField{Name: ":authority", Value: callHdr.Host})
	headerFields = append(headerFields, hpack.HeaderField{Name: "content-type", Value: grpcutil.ContentType(callHdr.ContentSubtype)})
	headerFields = append(headerFields, hpack.HeaderField{Name: "user-agent", Value: t.userAgent})
	headerFields = append(headerFields, hpack.HeaderField{Name: "te", Value: "trailers"})
	if callHdr.PreviousAttempts > 0 {
		headerFields = append(headerFields, hpack.HeaderField{Name: "grpc-previous-rpc-attempts", Value: strconv.Itoa(callHdr.PreviousAttempts)})
	}

	registeredCompressors := t.registeredCompressors
	if callHdr.SendCompress != "" {
		headerFields = append(headerFields, hpack.HeaderField{Name: "grpc-encoding", Value: callHdr.SendCompress})
		// Include the outgoing compressor name when compressor is not registered
		// via encoding.RegisterCompressor. This is possible when client uses
		// WithCompressor dial option.
		if !grpcutil.IsCompressorNameRegistered(callHdr.SendCompress) {
			if registeredCompressors != "" {
				registeredCompressors += ","
			}
			registeredCompressors += callHdr.SendCompress
		}
	}

	if registeredCompressors != "" {
		headerFields = append(headerFields, hpack.HeaderField{Name: "grpc-accept-encoding", Value: registeredCompressors})
	}
	if dl, ok := ctx.Deadline(); ok {
		// Send out timeout regardless its value. The server can detect timeout context by itself.
		// TODO(mmukhi): Perhaps this field should be updated when actually writing out to the wire.
		timeout := time.Until(dl)
		headerFields = append(headerFields, hpack.HeaderField{Name: "grpc-timeout", Value: grpcutil.EncodeDuration(timeout)})
	}
	for k, v := range authData {
		headerFields = append(headerFields, hpack.HeaderField{Name: k, Value: encodeMetadataHeader(k, v)})
	}
	for k, v := range callAuthData {
		headerFields = append(headerFields, hpack.HeaderField{Name: k, Value: encodeMetadataHeader(k, v)})
	}
	if b := stats.OutgoingTags(ctx); b != nil {
		headerFields = append(headerFields, hpack.HeaderField{Name: "grpc-tags-bin", Value: encodeBinHeader(b)})
	}
	if b := stats.OutgoingTrace(ctx); b != nil {
		headerFields = append(headerFields, hpack.HeaderField{Name: "grpc-trace-bin", Value: encodeBinHeader(b)})
	}

	if md, added, ok := metadataFromOutgoingContextRaw(ctx); ok {
		var k string
		for k, vv := range md {
			// HTTP doesn't allow you to set pseudoheaders after non pseudoheaders were set.
			if isReservedHeader(k) {
				continue
			}
			for _, v := range vv {
				headerFields = append(headerFields, hpack.HeaderField{Name: k, Value: encodeMetadataHeader(k, v)})
			}
		}
		for _, vv := range added {
			for i, v := range vv {
				if i%2 == 0 {
					k = strings.ToLower(v)
					continue
				}
				// HTTP doesn't allow you to set pseudoheaders after non pseudoheaders were set.
				if isReservedHeader(k) {
					continue
				}
				headerFields = append(headerFields, hpack.HeaderField{Name: k, Value: encodeMetadataHeader(k, v)})
			}
		}
	}
	for k, vv := range t.md {
		if isReservedHeader(k) {
			continue
		}
		for _, v := range vv {
			headerFields = append(headerFields, hpack.HeaderField{Name: k, Value: encodeMetadataHeader(k, v)})
		}
	}
	return headerFields, nil
}

func (t *http2Client) createAudience(callHdr *CallHdr) string {
	// Create an audience string only if needed.
	if len(t.perRPCCreds) == 0 && callHdr.Creds == nil {
		return ""
	}
	// Construct URI required to get auth request metadata.
	// Omit port if it is the default one.
	host := strings.TrimSuffix(callHdr.Host, ":443")
	pos := strings.LastIndex(callHdr.Method, "/")
	if pos == -1 {
		pos = len(callHdr.Method)
	}
	return "https://" + host + callHdr.Method[:pos]
}

func (t *http2Client) getTrAuthData(ctx context.Context, audience string) (map[string]string, error) {
	if len(t.perRPCCreds) == 0 {
		return nil, nil
	}
	authData := map[string]string{}
	for _, c := range t.perRPCCreds {
		data, err := c.GetRequestMetadata(ctx, audience)
		if err != nil {
			if st, ok := status.FromError(err); ok {
				// Restrict the code to the list allowed by gRFC A54.
				if istatus.IsRestrictedControlPlaneCode(st) {
					err = status.Errorf(codes.Internal, "transport: received per-RPC creds error with illegal status: %v", err)
				}
				return nil, err
			}

			return nil, status.Errorf(codes.Unauthenticated, "transport: per-RPC creds failed due to error: %v", err)
		}
		for k, v := range data {
			// Capital header names are illegal in HTTP/2.
			k = strings.ToLower(k)
			authData[k] = v
		}
	}
	return authData, nil
}

func (t *http2Client) getCallAuthData(ctx context.Context, audience string, callHdr *CallHdr) (map[string]string, error) {
	var callAuthData map[string]string
	// Check if credentials.PerRPCCredentials were provided via call options.
	// Note: if these credentials are provided both via dial options and call
	// options, then both sets of credentials will be applied.
	if callCreds := callHdr.Creds; callCreds != nil {
		if callCreds.RequireTransportSecurity() {
			ri, _ := credentials.RequestInfoFromContext(ctx)
			if !t.isSecure || credentials.CheckSecurityLevel(ri.AuthInfo, credentials.PrivacyAndIntegrity) != nil {
				return nil, status.Error(codes.Unauthenticated, "transport: cannot send secure credentials on an insecure connection")
			}
		}
		data, err := callCreds.GetRequestMetadata(ctx, audience)
		if err != nil {
			if st, ok := status.FromError(err); ok {
				// Restrict the code to the list allowed by gRFC A54.
				if istatus.IsRestrictedControlPlaneCode(st) {
					err = status.Errorf(codes.Internal, "transport: received per-RPC creds error with illegal status: %v", err)
				}
				return nil, err
			}
			return nil, status.Errorf(codes.Internal, "transport: per-RPC creds failed due to error: %v", err)
		}
		callAuthData = make(map[string]string, len(data))
		for k, v := range data {
			// Capital header names are illegal in HTTP/2
			k = strings.ToLower(k)
			callAuthData[k] = v
		}
	}
	return callAuthData, nil
}

// NewStreamError wraps an error and reports additional information.  Typically
// NewStream errors result in transparent retry, as they mean nothing went onto
// the wire.  However, there are two notable exceptions:
//
//  1. If the stream headers violate the max header list size allowed by the
//     server.  It's possible this could succeed on another transport, even if
//     it's unlikely, but do not transparently retry.
//  2. If the credentials errored when requesting their headers.  In this case,
//     it's possible a retry can fix the problem, but indefinitely transparently
//     retrying is not appropriate as it is likely the credentials, if they can
//     eventually succeed, would need I/O to do so.
type NewStreamError struct {
	Err error

	AllowTransparentRetry bool
}

func (e NewStreamError) Error() string {
	return e.Err.Error()
}

// NewStream creates a stream and registers it into the transport as "active"
// streams.  All non-nil errors returned will be *NewStreamError.
func (t *http2Client) NewStream(ctx context.Context, callHdr *CallHdr) (*Stream, error) {
	ctx = peer.NewContext(ctx, t.getPeer())

	// ServerName field of the resolver returned address takes precedence over
	// Host field of CallHdr to determine the :authority header. This is because,
	// the ServerName field takes precedence for server authentication during
	// TLS handshake, and the :authority header should match the value used
	// for server authentication.
	if t.address.ServerName != "" {
		newCallHdr := *callHdr
		newCallHdr.Host = t.address.ServerName
		callHdr = &newCallHdr
	}

	headerFields, err := t.createHeaderFields(ctx, callHdr)
	if err != nil {
		return nil, &NewStreamError{Err: err, AllowTransparentRetry: false}
	}
	s := t.newStream(ctx, callHdr)
	cleanup := func(err error) {
		if s.swapState(streamDone) == streamDone {
			// If it was already done, return.
			return
		}
		// The stream was unprocessed by the server.
		atomic.StoreUint32(&s.unprocessed, 1)
		s.write(recvMsg{err: err})
		close(s.done)
		// If headerChan isn't closed, then close it.
		if atomic.CompareAndSwapUint32(&s.headerChanClosed, 0, 1) {
			close(s.headerChan)
		}
	}
	hdr := &headerFrame{
		hf:        headerFields,
		endStream: false,
		initStream: func(uint32) error {
			t.mu.Lock()
			// TODO: handle transport closure in loopy instead and remove this
			// initStream is never called when transport is draining.
			if t.state == closing {
				t.mu.Unlock()
				cleanup(ErrConnClosing)
				return ErrConnClosing
			}
			if channelz.IsOn() {
				t.channelz.SocketMetrics.StreamsStarted.Add(1)
				t.channelz.SocketMetrics.LastLocalStreamCreatedTimestamp.Store(time.Now().UnixNano())
			}
			// If the keepalive goroutine has gone dormant, wake it up.
			if t.kpDormant {
				t.kpDormancyCond.Signal()
			}
			t.mu.Unlock()
			return nil
		},
		onOrphaned: cleanup,
		wq:         s.wq,
	}
	firstTry := true
	var ch chan struct{}
	transportDrainRequired := false
	checkForStreamQuota := func() bool {
		if t.streamQuota <= 0 { // Can go negative if server decreases it.
			if firstTry {
				t.waitingStreams++
			}
			ch = t.streamsQuotaAvailable
			return false
		}
		if !firstTry {
			t.waitingStreams--
		}
		t.streamQuota--

		t.mu.Lock()
		if t.state == draining || t.activeStreams == nil { // Can be niled from Close().
			t.mu.Unlock()
			return false // Don't create a stream if the transport is already closed.
		}

		hdr.streamID = t.nextID
		t.nextID += 2
		// Drain client transport if nextID > MaxStreamID which signals gRPC that
		// the connection is closed and a new one must be created for subsequent RPCs.
		transportDrainRequired = t.nextID > MaxStreamID

		s.id = hdr.streamID
		s.fc = &inFlow{limit: uint32(t.initialWindowSize)}
		t.activeStreams[s.id] = s
		t.mu.Unlock()

		if t.streamQuota > 0 && t.waitingStreams > 0 {
			select {
			case t.streamsQuotaAvailable <- struct{}{}:
			default:
			}
		}
		return true
	}
	var hdrListSizeErr error
	checkForHeaderListSize := func() bool {
		if t.maxSendHeaderListSize == nil {
			return true
		}
		var sz int64
		for _, f := range hdr.hf {
			if sz += int64(f.Size()); sz > int64(*t.maxSendHeaderListSize) {
				hdrListSizeErr = status.Errorf(codes.Internal, "header list size to send violates the maximum size (%d bytes) set by server", *t.maxSendHeaderListSize)
				return false
			}
		}
		return true
	}
	for {
		success, err := t.controlBuf.executeAndPut(func() bool {
			return checkForHeaderListSize() && checkForStreamQuota()
		}, hdr)
		if err != nil {
			// Connection closed.
			return nil, &NewStreamError{Err: err, AllowTransparentRetry: true}
		}
		if success {
			break
		}
		if hdrListSizeErr != nil {
			return nil, &NewStreamError{Err: hdrListSizeErr}
		}
		firstTry = false
		select {
		case <-ch:
		case <-ctx.Done():
			return nil, &NewStreamError{Err: ContextErr(ctx.Err())}
		case <-t.goAway:
			return nil, &NewStreamError{Err: errStreamDrain, AllowTransparentRetry: true}
		case <-t.ctx.Done():
			return nil, &NewStreamError{Err: ErrConnClosing, AllowTransparentRetry: true}
		}
	}
	if len(t.statsHandlers) != 0 {
		header, ok := metadata.FromOutgoingContext(ctx)
		if ok {
			header.Set("user-agent", t.userAgent)
		} else {
			header = metadata.Pairs("user-agent", t.userAgent)
		}
		for _, sh := range t.statsHandlers {
			// Note: The header fields are compressed with hpack after this call returns.
			// No WireLength field is set here.
			// Note: Creating a new stats object to prevent pollution.
			outHeader := &stats.OutHeader{
				Client:      true,
				FullMethod:  callHdr.Method,
				RemoteAddr:  t.remoteAddr,
				LocalAddr:   t.localAddr,
				Compression: callHdr.SendCompress,
				Header:      header,
			}
			sh.HandleRPC(s.ctx, outHeader)
		}
	}
	if transportDrainRequired {
		if t.logger.V(logLevel) {
			t.logger.Infof("Draining transport: t.nextID > MaxStreamID")
		}
		t.GracefulClose()
	}
	return s, nil
}

// CloseStream clears the footprint of a stream when the stream is not needed any more.
// This must not be executed in reader's goroutine.
func (t *http2Client) CloseStream(s *Stream, err error) {
	var (
		rst     bool
		rstCode http2.ErrCode
	)
	if err != nil {
		rst = true
		rstCode = http2.ErrCodeCancel
	}
	t.closeStream(s, err, rst, rstCode, status.Convert(err), nil, false)
}

func (t *http2Client) closeStream(s *Stream, err error, rst bool, rstCode http2.ErrCode, st *status.Status, mdata map[string][]string, eosReceived bool) {
	// Set stream status to done.
	if s.swapState(streamDone) == streamDone {
		// If it was already done, return.  If multiple closeStream calls
		// happen simultaneously, wait for the first to finish.
		<-s.done
		return
	}
	// status and trailers can be updated here without any synchronization because the stream goroutine will
	// only read it after it sees an io.EOF error from read or write and we'll write those errors
	// only after updating this.
	s.status = st
	if len(mdata) > 0 {
		s.trailer = mdata
	}
	if err != nil {
		// This will unblock reads eventually.
		s.write(recvMsg{err: err})
	}
	// If headerChan isn't closed, then close it.
	if atomic.CompareAndSwapUint32(&s.headerChanClosed, 0, 1) {
		s.noHeaders = true
		close(s.headerChan)
	}
	cleanup := &cleanupStream{
		streamID: s.id,
		onWrite: func() {
			t.mu.Lock()
			if t.activeStreams != nil {
				delete(t.activeStreams, s.id)
			}
			t.mu.Unlock()
			if channelz.IsOn() {
				if eosReceived {
					t.channelz.SocketMetrics.StreamsSucceeded.Add(1)
				} else {
					t.channelz.SocketMetrics.StreamsFailed.Add(1)
				}
			}
		},
		rst:     rst,
		rstCode: rstCode,
	}
	addBackStreamQuota := func() bool {
		t.streamQuota++
		if t.streamQuota > 0 && t.waitingStreams > 0 {
			select {
			case t.streamsQuotaAvailable <- struct{}{}:
			default:
			}
		}
		return true
	}
	t.controlBuf.executeAndPut(addBackStreamQuota, cleanup)
	// This will unblock write.
	close(s.done)
	if s.doneFunc != nil {
		s.doneFunc()
	}
}

// Close kicks off the shutdown process of the transport. This should be called
// only once on a transport. Once it is called, the transport should not be
// accessed anymore.
func (t *http2Client) Close(err error) {
	t.conn.SetWriteDeadline(time.Now().Add(time.Second * 10))
	t.mu.Lock()
	// Make sure we only close once.
	if t.state == closing {
		t.mu.Unlock()
		return
	}
	if t.logger.V(logLevel) {
		t.logger.Infof("Closing: %v", err)
	}
	// Call t.onClose ASAP to prevent the client from attempting to create new
	// streams.
	if t.state != draining {
		t.onClose(GoAwayInvalid)
	}
	t.state = closing
	streams := t.activeStreams
	t.activeStreams = nil
	if t.kpDormant {
		// If the keepalive goroutine is blocked on this condition variable, we
		// should unblock it so that the goroutine eventually exits.
		t.kpDormancyCond.Signal()
	}
	// Append info about previous goaways if there were any, since this may be important
	// for understanding the root cause for this connection to be closed.
	goAwayDebugMessage := t.goAwayDebugMessage
	t.mu.Unlock()

	// Per HTTP/2 spec, a GOAWAY frame must be sent before closing the
	// connection. See https://httpwg.org/specs/rfc7540.html#GOAWAY. It
	// also waits for loopyWriter to be closed with a timer to avoid the
	// long blocking in case the connection is blackholed, i.e. TCP is
	// just stuck.
	t.controlBuf.put(&goAway{code: http2.ErrCodeNo, debugData: []byte("client transport shutdown"), closeConn: err})
	timer := time.NewTimer(goAwayLoopyWriterTimeout)
	defer timer.Stop()
	select {
	case <-t.writerDone: // success
	case <-timer.C:
		t.logger.Infof("Failed to write a GOAWAY frame as part of connection close after %s. Giving up and closing the transport.", goAwayLoopyWriterTimeout)
	}
	t.cancel()
	t.conn.Close()
	// Waits for the reader and keepalive goroutines to exit before returning to
	// ensure all resources are cleaned up before Close can return.
	<-t.readerDone
	if t.keepaliveEnabled {
		<-t.keepaliveDone
	}
	channelz.RemoveEntry(t.channelz.ID)
	var st *status.Status
	if len(goAwayDebugMessage) > 0 {
		st = status.Newf(codes.Unavailable, "closing transport due to: %v, received prior goaway: %v", err, goAwayDebugMessage)
		err = st.Err()
	} else {
		st = status.New(codes.Unavailable, err.Error())
	}

	// Notify all active streams.
	for _, s := range streams {
		t.closeStream(s, err, false, http2.ErrCodeNo, st, nil, false)
	}
	for _, sh := range t.statsHandlers {
		connEnd := &stats.ConnEnd{
			Client: true,
		}
		sh.HandleConn(t.ctx, connEnd)
	}
}

// GracefulClose sets the state to draining, which prevents new streams from
// being created and causes the transport to be closed when the last active
// stream is closed.  If there are no active streams, the transport is closed
// immediately.  This does nothing if the transport is already draining or
// closing.
func (t *http2Client) GracefulClose() {
	t.mu.Lock()
	// Make sure we move to draining only from active.
	if t.state == draining || t.state == closing {
		t.mu.Unlock()
		return
	}
	if t.logger.V(logLevel) {
		t.logger.Infof("GracefulClose called")
	}
	t.onClose(GoAwayInvalid)
	t.state = draining
	active := len(t.activeStreams)
	t.mu.Unlock()
	if active == 0 {
		t.Close(connectionErrorf(true, nil, "no active streams left to process while draining"))
		return
	}
	t.controlBuf.put(&incomingGoAway{})
}

// Write formats the data into HTTP2 data frame(s) and sends it out. The caller
// should proceed only if Write returns nil.
func (t *http2Client) Write(s *Stream, hdr []byte, data mem.BufferSlice, opts *Options) error {
	reader := data.Reader()

	if opts.Last {
		// If it's the last message, update stream state.
		if !s.compareAndSwapState(streamActive, streamWriteDone) {
			_ = reader.Close()
			return errStreamDone
		}
	} else if s.getState() != streamActive {
		_ = reader.Close()
		return errStreamDone
	}
	df := &dataFrame{
		streamID:  s.id,
		endStream: opts.Last,
		h:         hdr,
		reader:    reader,
	}
	if hdr != nil || df.reader.Remaining() != 0 { // If it's not an empty data frame, check quota.
		if err := s.wq.get(int32(len(hdr) + df.reader.Remaining())); err != nil {
			_ = reader.Close()
			return err
		}
	}
	if err := t.controlBuf.put(df); err != nil {
		_ = reader.Close()
		return err
	}
	return nil
}

func (t *http2Client) getStream(f http2.Frame) *Stream {
	t.mu.Lock()
	s := t.activeStreams[f.Header().StreamID]
	t.mu.Unlock()
	return s
}

// adjustWindow sends out extra window update over the initial window size
// of stream if the application is requesting data larger in size than
// the window.
func (t *http2Client) adjustWindow(s *Stream, n uint32) {
	if w := s.fc.maybeAdjust(n); w > 0 {
		t.controlBuf.put(&outgoingWindowUpdate{streamID: s.id, increment: w})
	}
}

// updateWindow adjusts the inbound quota for the stream.
// Window updates will be sent out when the cumulative quota
// exceeds the corresponding threshold.
func (t *http2Client) updateWindow(s *Stream, n uint32) {
	if w := s.fc.onRead(n); w > 0 {
		t.controlBuf.put(&outgoingWindowUpdate{streamID: s.id, increment: w})
	}
}

// updateFlowControl updates the incoming flow control windows
// for the transport and the stream based on the current bdp
// estimation.
func (t *http2Client) updateFlowControl(n uint32) {
	updateIWS := func() bool {
		t.initialWindowSize = int32(n)
		t.mu.Lock()
		for _, s := range t.activeStreams {
			s.fc.newLimit(n)
		}
		t.mu.Unlock()
		return true
	}
	t.controlBuf.executeAndPut(updateIWS, &outgoingWindowUpdate{streamID: 0, increment: t.fc.newLimit(n)})
	t.controlBuf.put(&outgoingSettings{
		ss: []http2.Setting{
			{
				ID:  http2.SettingInitialWindowSize,
				Val: n,
			},
		},
	})
}

func (t *http2Client) handleData(f *http2.DataFrame) {
	size := f.Header().Length
	var sendBDPPing bool
	if t.bdpEst != nil {
		sendBDPPing = t.bdpEst.add(size)
	}
	// Decouple connection's flow control from application's read.
	// An update on connection's flow control should not depend on
	// whether user application has read the data or not. Such a
	// restriction is already imposed on the stream's flow control,
	// and therefore the sender will be blocked anyways.
	// Decoupling the connection flow control will prevent other
	// active(fast) streams from starving in presence of slow or
	// inactive streams.
	//
	if w := t.fc.onData(size); w > 0 {
		t.controlBuf.put(&outgoingWindowUpdate{
			streamID:  0,
			increment: w,
		})
	}
	if sendBDPPing {
		// Avoid excessive ping detection (e.g. in an L7 proxy)
		// by sending a window update prior to the BDP ping.

		if w := t.fc.reset(); w > 0 {
			t.controlBuf.put(&outgoingWindowUpdate{
				streamID:  0,
				increment: w,
			})
		}

		t.controlBuf.put(bdpPing)
	}
	// Select the right stream to dispatch.
	s := t.getStream(f)
	if s == nil {
		return
	}
	if size > 0 {
		if err := s.fc.onData(size); err != nil {
			t.closeStream(s, io.EOF, true, http2.ErrCodeFlowControl, status.New(codes.Internal, err.Error()), nil, false)
			return
		}
		if f.Header().Flags.Has(http2.FlagDataPadded) {
			if w := s.fc.onRead(size - uint32(len(f.Data()))); w > 0 {
				t.controlBuf.put(&outgoingWindowUpdate{s.id, w})
			}
		}
		// TODO(bradfitz, zhaoq): A copy is required here because there is no
		// guarantee f.Data() is consumed before the arrival of next frame.
		// Can this copy be eliminated?
		if len(f.Data()) > 0 {
			pool := t.bufferPool
			if pool == nil {
				// Note that this is only supposed to be nil in tests. Otherwise, stream is
				// always initialized with a BufferPool.
				pool = mem.DefaultBufferPool()
			}
			s.write(recvMsg{buffer: mem.Copy(f.Data(), pool)})
		}
	}
	// The server has closed the stream without sending trailers.  Record that
	// the read direction is closed, and set the status appropriately.
	if f.StreamEnded() {
		t.closeStream(s, io.EOF, false, http2.ErrCodeNo, status.New(codes.Internal, "server closed the stream without sending trailers"), nil, true)
	}
}

func (t *http2Client) handleRSTStream(f *http2.RSTStreamFrame) {
	s := t.getStream(f)
	if s == nil {
		return
	}
	if f.ErrCode == http2.ErrCodeRefusedStream {
		// The stream was unprocessed by the server.
		atomic.StoreUint32(&s.unprocessed, 1)
	}
	statusCode, ok := http2ErrConvTab[f.ErrCode]
	if !ok {
		if t.logger.V(logLevel) {
			t.logger.Infof("Received a RST_STREAM frame with code %q, but found no mapped gRPC status", f.ErrCode)
		}
		statusCode = codes.Unknown
	}
	if statusCode == codes.Canceled {
		if d, ok := s.ctx.Deadline(); ok && !d.After(time.Now()) {
			// Our deadline was already exceeded, and that was likely the cause
			// of this cancellation.  Alter the status code accordingly.
			statusCode = codes.DeadlineExceeded
		}
	}
	t.closeStream(s, io.EOF, false, http2.ErrCodeNo, status.Newf(statusCode, "stream terminated by RST_STREAM with error code: %v", f.ErrCode), nil, false)
}

func (t *http2Client) handleSettings(f *http2.SettingsFrame, isFirst bool) {
	if f.IsAck() {
		return
	}
	var maxStreams *uint32
	var ss []http2.Setting
	var updateFuncs []func()
	f.ForeachSetting(func(s http2.Setting) error {
		switch s.ID {
		case http2.SettingMaxConcurrentStreams:
			maxStreams = new(uint32)
			*maxStreams = s.Val
		case http2.SettingMaxHeaderListSize:
			updateFuncs = append(updateFuncs, func() {
				t.maxSendHeaderListSize = new(uint32)
				*t.maxSendHeaderListSize = s.Val
			})
		default:
			ss = append(ss, s)
		}
		return nil
	})
	if isFirst && maxStreams == nil {
		maxStreams = new(uint32)
		*maxStreams = math.MaxUint32
	}
	sf := &incomingSettings{
		ss: ss,
	}
	if maxStreams != nil {
		updateStreamQuota := func() {
			delta := int64(*maxStreams) - int64(t.maxConcurrentStreams)
			t.maxConcurrentStreams = *maxStreams
			t.streamQuota += delta
			if delta > 0 && t.waitingStreams > 0 {
				close(t.streamsQuotaAvailable) // wake all of them up.
				t.streamsQuotaAvailable = make(chan struct{}, 1)
			}
		}
		updateFuncs = append(updateFuncs, updateStreamQuota)
	}
	t.controlBuf.executeAndPut(func() bool {
		for _, f := range updateFuncs {
			f()
		}
		return true
	}, sf)
}

func (t *http2Client) handlePing(f *http2.PingFrame) {
	if f.IsAck() {
		// Maybe it's a BDP ping.
		if t.bdpEst != nil {
			t.bdpEst.calculate(f.Data)
		}
		return
	}
	pingAck := &ping{ack: true}
	copy(pingAck.data[:], f.Data[:])
	t.controlBuf.put(pingAck)
}

func (t *http2Client) handleGoAway(f *http2.GoAwayFrame) error {
	t.mu.Lock()
	if t.state == closing {
		t.mu.Unlock()
		return nil
	}
	if f.ErrCode == http2.ErrCodeEnhanceYourCalm && string(f.DebugData()) == "too_many_pings" {
		// When a client receives a GOAWAY with error code ENHANCE_YOUR_CALM and debug
		// data equal to ASCII "too_many_pings", it should log the occurrence at a log level that is
		// enabled by default and double the configure KEEPALIVE_TIME used for new connections
		// on that channel.
		logger.Errorf("Client received GoAway with error code ENHANCE_YOUR_CALM and debug data equal to ASCII \"too_many_pings\".")
	}
	id := f.LastStreamID
	if id > 0 && id%2 == 0 {
		t.mu.Unlock()
		return connectionErrorf(true, nil, "received goaway with non-zero even-numbered stream id: %v", id)
	}
	// A client can receive multiple GoAways from the server (see
	// https://github.com/grpc/grpc-go/issues/1387).  The idea is that the first
	// GoAway will be sent with an ID of MaxInt32 and the second GoAway will be
	// sent after an RTT delay with the ID of the last stream the server will
	// process.
	//
	// Therefore, when we get the first GoAway we don't necessarily close any
	// streams. While in case of second GoAway we close all streams created after
	// the GoAwayId. This way streams that were in-flight while the GoAway from
	// server was being sent don't get killed.
	select {
	case <-t.goAway: // t.goAway has been closed (i.e.,multiple GoAways).
		// If there are multiple GoAways the first one should always have an ID greater than the following ones.
		if id > t.prevGoAwayID {
			t.mu.Unlock()
			return connectionErrorf(true, nil, "received goaway with stream id: %v, which exceeds stream id of previous goaway: %v", id, t.prevGoAwayID)
		}
	default:
		t.setGoAwayReason(f)
		close(t.goAway)
		defer t.controlBuf.put(&incomingGoAway{}) // Defer as t.mu is currently held.
		// Notify the clientconn about the GOAWAY before we set the state to
		// draining, to allow the client to stop attempting to create streams
		// before disallowing new streams on this connection.
		if t.state != draining {
			t.onClose(t.goAwayReason)
			t.state = draining
		}
	}
	// All streams with IDs greater than the GoAwayId
	// and smaller than the previous GoAway ID should be killed.
	upperLimit := t.prevGoAwayID
	if upperLimit == 0 { // This is the first GoAway Frame.
		upperLimit = math.MaxUint32 // Kill all streams after the GoAway ID.
	}

	t.prevGoAwayID = id
	if len(t.activeStreams) == 0 {
		t.mu.Unlock()
		return connectionErrorf(true, nil, "received goaway and there are no active streams")
	}

	streamsToClose := make([]*Stream, 0)
	for streamID, stream := range t.activeStreams {
		if streamID > id && streamID <= upperLimit {
			// The stream was unprocessed by the server.
			atomic.StoreUint32(&stream.unprocessed, 1)
			streamsToClose = append(streamsToClose, stream)
		}
	}
	t.mu.Unlock()
	// Called outside t.mu because closeStream can take controlBuf's mu, which
	// could induce deadlock and is not allowed.
	for _, stream := range streamsToClose {
		t.closeStream(stream, errStreamDrain, false, http2.ErrCodeNo, statusGoAway, nil, false)
	}
	return nil
}

// setGoAwayReason sets the value of t.goAwayReason based
// on the GoAway frame received.
// It expects a lock on transport's mutex to be held by
// the caller.
func (t *http2Client) setGoAwayReason(f *http2.GoAwayFrame) {
	t.goAwayReason = GoAwayNoReason
	switch f.ErrCode {
	case http2.ErrCodeEnhanceYourCalm:
		if string(f.DebugData()) == "too_many_pings" {
			t.goAwayReason = GoAwayTooManyPings
		}
	}
	if len(f.DebugData()) == 0 {
		t.goAwayDebugMessage = fmt.Sprintf("code: %s", f.ErrCode)
	} else {
		t.goAwayDebugMessage = fmt.Sprintf("code: %s, debug data: %q", f.ErrCode, string(f.DebugData()))
	}
}

func (t *http2Client) GetGoAwayReason() (GoAwayReason, string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.goAwayReason, t.goAwayDebugMessage
}

func (t *http2Client) handleWindowUpdate(f *http2.WindowUpdateFrame) {
	t.controlBuf.put(&incomingWindowUpdate{
		streamID:  f.Header().StreamID,
		increment: f.Increment,
	})
}

// operateHeaders takes action on the decoded headers.
func (t *http2Client) operateHeaders(frame *http2.MetaHeadersFrame) {
	s := t.getStream(frame)
	if s == nil {
		return
	}
	endStream := frame.StreamEnded()
	atomic.StoreUint32(&s.bytesReceived, 1)
	initialHeader := atomic.LoadUint32(&s.headerChanClosed) == 0

	if !initialHeader && !endStream {
		// As specified by gRPC over HTTP2, a HEADERS frame (and associated CONTINUATION frames) can only appear at the start or end of a stream. Therefore, second HEADERS frame must have EOS bit set.
		st := status.New(codes.Internal, "a HEADERS frame cannot appear in the middle of a stream")
		t.closeStream(s, st.Err(), true, http2.ErrCodeProtocol, st, nil, false)
		return
	}

	// frame.Truncated is set to true when framer detects that the current header
	// list size hits MaxHeaderListSize limit.
	if frame.Truncated {
		se := status.New(codes.Internal, "peer header list size exceeded limit")
		t.closeStream(s, se.Err(), true, http2.ErrCodeFrameSize, se, nil, endStream)
		return
	}

	var (
		// If a gRPC Response-Headers has already been received, then it means
		// that the peer is speaking gRPC and we are in gRPC mode.
		isGRPC         = !initialHeader
		mdata          = make(map[string][]string)
		contentTypeErr = "malformed header: missing HTTP content-type"
		grpcMessage    string
		recvCompress   string
		httpStatusCode *int
		httpStatusErr  string
		rawStatusCode  = codes.Unknown
		// headerError is set if an error is encountered while parsing the headers
		headerError string
	)

	if initialHeader {
		httpStatusErr = "malformed header: missing HTTP status"
	}

	for _, hf := range frame.Fields {
		switch hf.Name {
		case "content-type":
			if _, validContentType := grpcutil.ContentSubtype(hf.Value); !validContentType {
				contentTypeErr = fmt.Sprintf("transport: received unexpected content-type %q", hf.Value)
				break
			}
			contentTypeErr = ""
			mdata[hf.Name] = append(mdata[hf.Name], hf.Value)
			isGRPC = true
		case "grpc-encoding":
			recvCompress = hf.Value
		case "grpc-status":
			code, err := strconv.ParseInt(hf.Value, 10, 32)
			if err != nil {
				se := status.New(codes.Internal, fmt.Sprintf("transport: malformed grpc-status: %v", err))
				t.closeStream(s, se.Err(), true, http2.ErrCodeProtocol, se, nil, endStream)
				return
			}
			rawStatusCode = codes.Code(uint32(code))
		case "grpc-message":
			grpcMessage = decodeGrpcMessage(hf.Value)
		case ":status":
			if hf.Value == "200" {
				httpStatusErr = ""
				statusCode := 200
				httpStatusCode = &statusCode
				break
			}

			c, err := strconv.ParseInt(hf.Value, 10, 32)
			if err != nil {
				se := status.New(codes.Internal, fmt.Sprintf("transport: malformed http-status: %v", err))
				t.closeStream(s, se.Err(), true, http2.ErrCodeProtocol, se, nil, endStream)
				return
			}
			statusCode := int(c)
			httpStatusCode = &statusCode

			httpStatusErr = fmt.Sprintf(
				"unexpected HTTP status code received from server: %d (%s)",
				statusCode,
				http.StatusText(statusCode),
			)
		default:
			if isReservedHeader(hf.Name) && !isWhitelistedHeader(hf.Name) {
				break
			}
			v, err := decodeMetadataHeader(hf.Name, hf.Value)
			if err != nil {
				headerError = fmt.Sprintf("transport: malformed %s: %v", hf.Name, err)
				logger.Warningf("Failed to decode metadata header (%q, %q): %v", hf.Name, hf.Value, err)
				break
			}
			mdata[hf.Name] = append(mdata[hf.Name], v)
		}
	}

	if !isGRPC || httpStatusErr != "" {
		var code = codes.Internal // when header does not include HTTP status, return INTERNAL

		if httpStatusCode != nil {
			var ok bool
			code, ok = HTTPStatusConvTab[*httpStatusCode]
			if !ok {
				code = codes.Unknown
			}
		}
		var errs []string
		if httpStatusErr != "" {
			errs = append(errs, httpStatusErr)
		}
		if contentTypeErr != "" {
			errs = append(errs, contentTypeErr)
		}
		// Verify the HTTP response is a 200.
		se := status.New(code, strings.Join(errs, "; "))
		t.closeStream(s, se.Err(), true, http2.ErrCodeProtocol, se, nil, endStream)
		return
	}

	if headerError != "" {
		se := status.New(codes.Internal, headerError)
		t.closeStream(s, se.Err(), true, http2.ErrCodeProtocol, se, nil, endStream)
		return
	}

	// For headers, set them in s.header and close headerChan.  For trailers or
	// trailers-only, closeStream will set the trailers and close headerChan as
	// needed.
	if !endStream {
		// If headerChan hasn't been closed yet (expected, given we checked it
		// above, but something else could have potentially closed the whole
		// stream).
		if atomic.CompareAndSwapUint32(&s.headerChanClosed, 0, 1) {
			s.headerValid = true
			// These values can be set without any synchronization because
			// stream goroutine will read it only after seeing a closed
			// headerChan which we'll close after setting this.
			s.recvCompress = recvCompress
			if len(mdata) > 0 {
				s.header = mdata
			}
			close(s.headerChan)
		}
	}

	for _, sh := range t.statsHandlers {
		if !endStream {
			inHeader := &stats.InHeader{
				Client:      true,
				WireLength:  int(frame.Header().Length),
				Header:      metadata.MD(mdata).Copy(),
				Compression: s.recvCompress,
			}
			sh.HandleRPC(s.ctx, inHeader)
		} else {
			inTrailer := &stats.InTrailer{
				Client:     true,
				WireLength: int(frame.Header().Length),
				Trailer:    metadata.MD(mdata).Copy(),
			}
			sh.HandleRPC(s.ctx, inTrailer)
		}
	}

	if !endStream {
		return
	}

	status := istatus.NewWithProto(rawStatusCode, grpcMessage, mdata[grpcStatusDetailsBinHeader])

	// If client received END_STREAM from server while stream was still active,
	// send RST_STREAM.
	rstStream := s.getState() == streamActive
	t.closeStream(s, io.EOF, rstStream, http2.ErrCodeNo, status, mdata, true)
}

// readServerPreface reads and handles the initial settings frame from the
// server.
func (t *http2Client) readServerPreface() error {
	frame, err := t.framer.fr.ReadFrame()
	if err != nil {
		return connectionErrorf(true, err, "error reading server preface: %v", err)
	}
	sf, ok := frame.(*http2.SettingsFrame)
	if !ok {
		return connectionErrorf(true, nil, "initial http2 frame from server is not a settings frame: %T", frame)
	}
	t.handleSettings(sf, true)
	return nil
}

// reader verifies the server preface and reads all subsequent data from
// network connection.  If the server preface is not read successfully, an
// error is pushed to errCh; otherwise errCh is closed with no error.
func (t *http2Client) reader(errCh chan<- error) {
	var errClose error
	defer func() {
		close(t.readerDone)
		if errClose != nil {
			t.Close(errClose)
		}
	}()

	if err := t.readServerPreface(); err != nil {
		errCh <- err
		return
	}
	close(errCh)
	if t.keepaliveEnabled {
		atomic.StoreInt64(&t.lastRead, time.Now().UnixNano())
	}

	// loop to keep reading incoming messages on this transport.
	for {
		t.controlBuf.throttle()
		frame, err := t.framer.fr.ReadFrame()
		if t.keepaliveEnabled {
			atomic.StoreInt64(&t.lastRead, time.Now().UnixNano())
		}
		if err != nil {
			// Abort an active stream if the http2.Framer returns a
			// http2.StreamError. This can happen only if the server's response
			// is malformed http2.
			if se, ok := err.(http2.StreamError); ok {
				t.mu.Lock()
				s := t.activeStreams[se.StreamID]
				t.mu.Unlock()
				if s != nil {
					// use error detail to provide better err message
					code := http2ErrConvTab[se.Code]
					errorDetail := t.framer.fr.ErrorDetail()
					var msg string
					if errorDetail != nil {
						msg = errorDetail.Error()
					} else {
						msg = "received invalid frame"
					}
					t.closeStream(s, status.Error(code, msg), true, http2.ErrCodeProtocol, status.New(code, msg), nil, false)
				}
				continue
			}
			// Transport error.
			errClose = connectionErrorf(true, err, "error reading from server: %v", err)
			return
		}
		switch frame := frame.(type) {
		case *http2.MetaHeadersFrame:
			t.operateHeaders(frame)
		case *http2.DataFrame:
			t.handleData(frame)
		case *http2.RSTStreamFrame:
			t.handleRSTStream(frame)
		case *http2.SettingsFrame:
			t.handleSettings(frame, false)
		case *http2.PingFrame:
			t.handlePing(frame)
		case *http2.GoAwayFrame:
			errClose = t.handleGoAway(frame)
		case *http2.WindowUpdateFrame:
			t.handleWindowUpdate(frame)
		default:
			if logger.V(logLevel) {
				logger.Errorf("transport: http2Client.reader got unhandled frame type %v.", frame)
			}
		}
	}
}

// keepalive running in a separate goroutine makes sure the connection is alive by sending pings.
func (t *http2Client) keepalive() {
	var err error
	defer func() {
		close(t.keepaliveDone)
		if err != nil {
			t.Close(err)
		}
	}()
	p := &ping{data: [8]byte{}}
	// True iff a ping has been sent, and no data has been received since then.
	outstandingPing := false
	// Amount of time remaining before which we should receive an ACK for the
	// last sent ping.
	timeoutLeft := time.Duration(0)
	// Records the last value of t.lastRead before we go block on the timer.
	// This is required to check for read activity since then.
	prevNano := time.Now().UnixNano()
	timer := time.NewTimer(t.kp.Time)
	for {
		select {
		case <-timer.C:
			lastRead := atomic.LoadInt64(&t.lastRead)
			if lastRead > prevNano {
				// There has been read activity since the last time we were here.
				outstandingPing = false
				// Next timer should fire at kp.Time seconds from lastRead time.
				timer.Reset(time.Duration(lastRead) + t.kp.Time - time.Duration(time.Now().UnixNano()))
				prevNano = lastRead
				continue
			}
			if outstandingPing && timeoutLeft <= 0 {
				err = connectionErrorf(true, nil, "keepalive ping failed to receive ACK within timeout")
				return
			}
			t.mu.Lock()
			if t.state == closing {
				// If the transport is closing, we should exit from the
				// keepalive goroutine here. If not, we could have a race
				// between the call to Signal() from Close() and the call to
				// Wait() here, whereby the keepalive goroutine ends up
				// blocking on the condition variable which will never be
				// signalled again.
				t.mu.Unlock()
				return
			}
			if len(t.activeStreams) < 1 && !t.kp.PermitWithoutStream {
				// If a ping was sent out previously (because there were active
				// streams at that point) which wasn't acked and its timeout
				// hadn't fired, but we got here and are about to go dormant,
				// we should make sure that we unconditionally send a ping once
				// we awaken.
				outstandingPing = false
				t.kpDormant = true
				t.kpDormancyCond.Wait()
			}
			t.kpDormant = false
			t.mu.Unlock()

			// We get here either because we were dormant and a new stream was
			// created which unblocked the Wait() call, or because the
			// keepalive timer expired. In both cases, we need to send a ping.
			if !outstandingPing {
				if channelz.IsOn() {
					t.channelz.SocketMetrics.KeepAlivesSent.Add(1)
				}
				t.controlBuf.put(p)
				timeoutLeft = t.kp.Timeout
				outstandingPing = true
			}
			// The amount of time to sleep here is the minimum of kp.Time and
			// timeoutLeft. This will ensure that we wait only for kp.Time
			// before sending out the next ping (for cases where the ping is
			// acked).
			sleepDuration := min(t.kp.Time, timeoutLeft)
			timeoutLeft -= sleepDuration
			timer.Reset(sleepDuration)
		case <-t.ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return
		}
	}
}

func (t *http2Client) Error() <-chan struct{} {
	return t.ctx.Done()
}

func (t *http2Client) GoAway() <-chan struct{} {
	return t.goAway
}

func (t *http2Client) socketMetrics() *channelz.EphemeralSocketMetrics {
	return &channelz.EphemeralSocketMetrics{
		LocalFlowControlWindow:  int64(t.fc.getSize()),
		RemoteFlowControlWindow: t.getOutFlowWindow(),
	}
}

func (t *http2Client) RemoteAddr() net.Addr { return t.remoteAddr }

func (t *http2Client) IncrMsgSent() {
	t.channelz.SocketMetrics.MessagesSent.Add(1)
	t.channelz.SocketMetrics.LastMessageSentTimestamp.Store(time.Now().UnixNano())
}

func (t *http2Client) IncrMsgRecv() {
	t.channelz.SocketMetrics.MessagesReceived.Add(1)
	t.channelz.SocketMetrics.LastMessageReceivedTimestamp.Store(time.Now().UnixNano())
}

func (t *http2Client) getOutFlowWindow() int64 {
	resp := make(chan uint32, 1)
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	t.controlBuf.put(&outFlowControlSizeRequest{resp})
	select {
	case sz := <-resp:
		return int64(sz)
	case <-t.ctxDone:
		return -1
	case <-timer.C:
		return -2
	}
}

func (t *http2Client) stateForTesting() transportState {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.state
}
