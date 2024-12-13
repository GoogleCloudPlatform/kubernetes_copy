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
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/encoding"
	"google.golang.org/grpc/encoding/proto"
	"google.golang.org/grpc/grpclog"
	"google.golang.org/grpc/internal"
	"google.golang.org/grpc/internal/binarylog"
	"google.golang.org/grpc/internal/channelz"
	"google.golang.org/grpc/internal/grpcsync"
	"google.golang.org/grpc/internal/grpcutil"
	"google.golang.org/grpc/internal/transport"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/mem"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/stats"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/tap"
)

const (
	defaultServerMaxReceiveMessageSize = 1024 * 1024 * 4
	defaultServerMaxSendMessageSize    = math.MaxInt32

	// Server transports are tracked in a map which is keyed on listener
	// address. For regular gRPC traffic, connections are accepted in Serve()
	// through a call to Accept(), and we use the actual listener address as key
	// when we add it to the map. But for connections received through
	// ServeHTTP(), we do not have a listener and hence use this dummy value.
	listenerAddressForServeHTTP = "listenerAddressForServeHTTP"
)

func init() {
	internal.GetServerCredentials = func(srv *Server) credentials.TransportCredentials {
		return srv.opts.creds
	}
	internal.IsRegisteredMethod = func(srv *Server, method string) bool {
		return srv.isRegisteredMethod(method)
	}
	internal.ServerFromContext = serverFromContext
	internal.AddGlobalServerOptions = func(opt ...ServerOption) {
		globalServerOptions = append(globalServerOptions, opt...)
	}
	internal.ClearGlobalServerOptions = func() {
		globalServerOptions = nil
	}
	internal.BinaryLogger = binaryLogger
	internal.JoinServerOptions = newJoinServerOption
	internal.BufferPool = bufferPool
}

var statusOK = status.New(codes.OK, "")
var logger = grpclog.Component("core")

type methodHandler func(srv any, ctx context.Context, dec func(any) error, interceptor UnaryServerInterceptor) (any, error)

// MethodDesc represents an RPC service's method specification.
type MethodDesc struct {
	MethodName string
	Handler    methodHandler
}

// ServiceDesc represents an RPC service's specification.
type ServiceDesc struct {
	ServiceName string
	// The pointer to the service interface. Used to check whether the user
	// provided implementation satisfies the interface requirements.
	HandlerType any
	Methods     []MethodDesc
	Streams     []StreamDesc
	Metadata    any
}

// serviceInfo wraps information about a service. It is very similar to
// ServiceDesc and is constructed from it for internal purposes.
type serviceInfo struct {
	// Contains the implementation for the methods in this service.
	serviceImpl any
	methods     map[string]*MethodDesc
	streams     map[string]*StreamDesc
	mdata       any
}

// Server is a gRPC server to serve RPC requests.
type Server struct {
	opts serverOptions

	mu  sync.Mutex // guards following
	lis map[net.Listener]bool
	// conns contains all active server transports. It is a map keyed on a
	// listener address with the value being the set of active transports
	// belonging to that listener.
	conns    map[string]map[transport.ServerTransport]bool
	serve    bool
	drain    bool
	cv       *sync.Cond              // signaled when connections close for GracefulStop
	services map[string]*serviceInfo // service name -> service info
	events   traceEventLog

	quit               *grpcsync.Event
	done               *grpcsync.Event
	channelzRemoveOnce sync.Once
	serveWG            sync.WaitGroup // counts active Serve goroutines for Stop/GracefulStop
	handlersWG         sync.WaitGroup // counts active method handler goroutines

	channelz *channelz.Server

	serverWorkerChannel      chan func()
	serverWorkerChannelClose func()
}

type serverOptions struct {
	creds                 credentials.TransportCredentials
	codec                 baseCodec
	cp                    Compressor
	dc                    Decompressor
	unaryInt              UnaryServerInterceptor
	streamInt             StreamServerInterceptor
	chainUnaryInts        []UnaryServerInterceptor
	chainStreamInts       []StreamServerInterceptor
	binaryLogger          binarylog.Logger
	inTapHandle           tap.ServerInHandle
	statsHandlers         []stats.Handler
	maxConcurrentStreams  uint32
	maxReceiveMessageSize int
	maxSendMessageSize    int
	unknownStreamDesc     *StreamDesc
	keepaliveParams       keepalive.ServerParameters
	keepalivePolicy       keepalive.EnforcementPolicy
	initialWindowSize     int32
	initialConnWindowSize int32
	writeBufferSize       int
	readBufferSize        int
	sharedWriteBuffer     bool
	connectionTimeout     time.Duration
	maxHeaderListSize     *uint32
	headerTableSize       *uint32
	numServerWorkers      uint32
	bufferPool            mem.BufferPool
	waitForHandlers       bool
}

var defaultServerOptions = serverOptions{
	maxConcurrentStreams:  math.MaxUint32,
	maxReceiveMessageSize: defaultServerMaxReceiveMessageSize,
	maxSendMessageSize:    defaultServerMaxSendMessageSize,
	connectionTimeout:     120 * time.Second,
	writeBufferSize:       defaultWriteBufSize,
	readBufferSize:        defaultReadBufSize,
	bufferPool:            mem.DefaultBufferPool(),
}
var globalServerOptions []ServerOption

// A ServerOption sets options such as credentials, codec and keepalive parameters, etc.
type ServerOption interface {
	apply(*serverOptions)
}

// EmptyServerOption does not alter the server configuration. It can be embedded
// in another structure to build custom server options.
//
// # Experimental
//
// Notice: This type is EXPERIMENTAL and may be changed or removed in a
// later release.
type EmptyServerOption struct{}

func (EmptyServerOption) apply(*serverOptions) {}

// funcServerOption wraps a function that modifies serverOptions into an
// implementation of the ServerOption interface.
type funcServerOption struct {
	f func(*serverOptions)
}

func (fdo *funcServerOption) apply(do *serverOptions) {
	fdo.f(do)
}

func newFuncServerOption(f func(*serverOptions)) *funcServerOption {
	return &funcServerOption{
		f: f,
	}
}

// joinServerOption provides a way to combine arbitrary number of server
// options into one.
type joinServerOption struct {
	opts []ServerOption
}

func (mdo *joinServerOption) apply(do *serverOptions) {
	for _, opt := range mdo.opts {
		opt.apply(do)
	}
}

func newJoinServerOption(opts ...ServerOption) ServerOption {
	return &joinServerOption{opts: opts}
}

// SharedWriteBuffer allows reusing per-connection transport write buffer.
// If this option is set to true every connection will release the buffer after
// flushing the data on the wire.
//
// # Experimental
//
// Notice: This API is EXPERIMENTAL and may be changed or removed in a
// later release.
func SharedWriteBuffer(val bool) ServerOption {
	return newFuncServerOption(func(o *serverOptions) {
		o.sharedWriteBuffer = val
	})
}

// WriteBufferSize determines how much data can be batched before doing a write
// on the wire. The default value for this buffer is 32KB. Zero or negative
// values will disable the write buffer such that each write will be on underlying
// connection. Note: A Send call may not directly translate to a write.
func WriteBufferSize(s int) ServerOption {
	return newFuncServerOption(func(o *serverOptions) {
		o.writeBufferSize = s
	})
}

// ReadBufferSize lets you set the size of read buffer, this determines how much
// data can be read at most for one read syscall. The default value for this
// buffer is 32KB. Zero or negative values will disable read buffer for a
// connection so data framer can access the underlying conn directly.
func ReadBufferSize(s int) ServerOption {
	return newFuncServerOption(func(o *serverOptions) {
		o.readBufferSize = s
	})
}

// InitialWindowSize returns a ServerOption that sets window size for stream.
// The lower bound for window size is 64K and any value smaller than that will be ignored.
func InitialWindowSize(s int32) ServerOption {
	return newFuncServerOption(func(o *serverOptions) {
		o.initialWindowSize = s
	})
}

// InitialConnWindowSize returns a ServerOption that sets window size for a connection.
// The lower bound for window size is 64K and any value smaller than that will be ignored.
func InitialConnWindowSize(s int32) ServerOption {
	return newFuncServerOption(func(o *serverOptions) {
		o.initialConnWindowSize = s
	})
}

// KeepaliveParams returns a ServerOption that sets keepalive and max-age parameters for the server.
func KeepaliveParams(kp keepalive.ServerParameters) ServerOption {
	if kp.Time > 0 && kp.Time < internal.KeepaliveMinServerPingTime {
		logger.Warning("Adjusting keepalive ping interval to minimum period of 1s")
		kp.Time = internal.KeepaliveMinServerPingTime
	}

	return newFuncServerOption(func(o *serverOptions) {
		o.keepaliveParams = kp
	})
}

// KeepaliveEnforcementPolicy returns a ServerOption that sets keepalive enforcement policy for the server.
func KeepaliveEnforcementPolicy(kep keepalive.EnforcementPolicy) ServerOption {
	return newFuncServerOption(func(o *serverOptions) {
		o.keepalivePolicy = kep
	})
}

// CustomCodec returns a ServerOption that sets a codec for message marshaling and unmarshaling.
//
// This will override any lookups by content-subtype for Codecs registered with RegisterCodec.
//
// Deprecated: register codecs using encoding.RegisterCodec. The server will
// automatically use registered codecs based on the incoming requests' headers.
// See also
// https://github.com/grpc/grpc-go/blob/master/Documentation/encoding.md#using-a-codec.
// Will be supported throughout 1.x.
func CustomCodec(codec Codec) ServerOption {
	return newFuncServerOption(func(o *serverOptions) {
		o.codec = newCodecV0Bridge(codec)
	})
}

// ForceServerCodec returns a ServerOption that sets a codec for message
// marshaling and unmarshaling.
//
// This will override any lookups by content-subtype for Codecs registered
// with RegisterCodec.
//
// See Content-Type on
// https://github.com/grpc/grpc/blob/master/doc/PROTOCOL-HTTP2.md#requests for
// more details. Also see the documentation on RegisterCodec and
// CallContentSubtype for more details on the interaction between encoding.Codec
// and content-subtype.
//
// This function is provided for advanced users; prefer to register codecs
// using encoding.RegisterCodec.
// The server will automatically use registered codecs based on the incoming
// requests' headers. See also
// https://github.com/grpc/grpc-go/blob/master/Documentation/encoding.md#using-a-codec.
// Will be supported throughout 1.x.
//
// # Experimental
//
// Notice: This API is EXPERIMENTAL and may be changed or removed in a
// later release.
func ForceServerCodec(codec encoding.Codec) ServerOption {
	return newFuncServerOption(func(o *serverOptions) {
		o.codec = newCodecV1Bridge(codec)
	})
}

// ForceServerCodecV2 is the equivalent of ForceServerCodec, but for the new
// CodecV2 interface.
//
// Will be supported throughout 1.x.
//
// # Experimental
//
// Notice: This API is EXPERIMENTAL and may be changed or removed in a
// later release.
func ForceServerCodecV2(codecV2 encoding.CodecV2) ServerOption {
	return newFuncServerOption(func(o *serverOptions) {
		o.codec = codecV2
	})
}

// RPCCompressor returns a ServerOption that sets a compressor for outbound
// messages.  For backward compatibility, all outbound messages will be sent
// using this compressor, regardless of incoming message compression.  By
// default, server messages will be sent using the same compressor with which
// request messages were sent.
//
// Deprecated: use encoding.RegisterCompressor instead. Will be supported
// throughout 1.x.
func RPCCompressor(cp Compressor) ServerOption {
	return newFuncServerOption(func(o *serverOptions) {
		o.cp = cp
	})
}

// RPCDecompressor returns a ServerOption that sets a decompressor for inbound
// messages.  It has higher priority than decompressors registered via
// encoding.RegisterCompressor.
//
// Deprecated: use encoding.RegisterCompressor instead. Will be supported
// throughout 1.x.
func RPCDecompressor(dc Decompressor) ServerOption {
	return newFuncServerOption(func(o *serverOptions) {
		o.dc = dc
	})
}

// MaxMsgSize returns a ServerOption to set the max message size in bytes the server can receive.
// If this is not set, gRPC uses the default limit.
//
// Deprecated: use MaxRecvMsgSize instead. Will be supported throughout 1.x.
func MaxMsgSize(m int) ServerOption {
	return MaxRecvMsgSize(m)
}

// MaxRecvMsgSize returns a ServerOption to set the max message size in bytes the server can receive.
// If this is not set, gRPC uses the default 4MB.
func MaxRecvMsgSize(m int) ServerOption {
	return newFuncServerOption(func(o *serverOptions) {
		o.maxReceiveMessageSize = m
	})
}

// MaxSendMsgSize returns a ServerOption to set the max message size in bytes the server can send.
// If this is not set, gRPC uses the default `math.MaxInt32`.
func MaxSendMsgSize(m int) ServerOption {
	return newFuncServerOption(func(o *serverOptions) {
		o.maxSendMessageSize = m
	})
}

// MaxConcurrentStreams returns a ServerOption that will apply a limit on the number
// of concurrent streams to each ServerTransport.
func MaxConcurrentStreams(n uint32) ServerOption {
	if n == 0 {
		n = math.MaxUint32
	}
	return newFuncServerOption(func(o *serverOptions) {
		o.maxConcurrentStreams = n
	})
}

// Creds returns a ServerOption that sets credentials for server connections.
func Creds(c credentials.TransportCredentials) ServerOption {
	return newFuncServerOption(func(o *serverOptions) {
		o.creds = c
	})
}

// UnaryInterceptor returns a ServerOption that sets the UnaryServerInterceptor for the
// server. Only one unary interceptor can be installed. The construction of multiple
// interceptors (e.g., chaining) can be implemented at the caller.
func UnaryInterceptor(i UnaryServerInterceptor) ServerOption {
	return newFuncServerOption(func(o *serverOptions) {
		if o.unaryInt != nil {
			panic("The unary server interceptor was already set and may not be reset.")
		}
		o.unaryInt = i
	})
}

// ChainUnaryInterceptor returns a ServerOption that specifies the chained interceptor
// for unary RPCs. The first interceptor will be the outer most,
// while the last interceptor will be the inner most wrapper around the real call.
// All unary interceptors added by this method will be chained.
func ChainUnaryInterceptor(interceptors ...UnaryServerInterceptor) ServerOption {
	return newFuncServerOption(func(o *serverOptions) {
		o.chainUnaryInts = append(o.chainUnaryInts, interceptors...)
	})
}

// StreamInterceptor returns a ServerOption that sets the StreamServerInterceptor for the
// server. Only one stream interceptor can be installed.
func StreamInterceptor(i StreamServerInterceptor) ServerOption {
	return newFuncServerOption(func(o *serverOptions) {
		if o.streamInt != nil {
			panic("The stream server interceptor was already set and may not be reset.")
		}
		o.streamInt = i
	})
}

// ChainStreamInterceptor returns a ServerOption that specifies the chained interceptor
// for streaming RPCs. The first interceptor will be the outer most,
// while the last interceptor will be the inner most wrapper around the real call.
// All stream interceptors added by this method will be chained.
func ChainStreamInterceptor(interceptors ...StreamServerInterceptor) ServerOption {
	return newFuncServerOption(func(o *serverOptions) {
		o.chainStreamInts = append(o.chainStreamInts, interceptors...)
	})
}

// InTapHandle returns a ServerOption that sets the tap handle for all the server
// transport to be created. Only one can be installed.
//
// # Experimental
//
// Notice: This API is EXPERIMENTAL and may be changed or removed in a
// later release.
func InTapHandle(h tap.ServerInHandle) ServerOption {
	return newFuncServerOption(func(o *serverOptions) {
		if o.inTapHandle != nil {
			panic("The tap handle was already set and may not be reset.")
		}
		o.inTapHandle = h
	})
}

// StatsHandler returns a ServerOption that sets the stats handler for the server.
func StatsHandler(h stats.Handler) ServerOption {
	return newFuncServerOption(func(o *serverOptions) {
		if h == nil {
			logger.Error("ignoring nil parameter in grpc.StatsHandler ServerOption")
			// Do not allow a nil stats handler, which would otherwise cause
			// panics.
			return
		}
		o.statsHandlers = append(o.statsHandlers, h)
	})
}

// binaryLogger returns a ServerOption that can set the binary logger for the
// server.
func binaryLogger(bl binarylog.Logger) ServerOption {
	return newFuncServerOption(func(o *serverOptions) {
		o.binaryLogger = bl
	})
}

// UnknownServiceHandler returns a ServerOption that allows for adding a custom
// unknown service handler. The provided method is a bidi-streaming RPC service
// handler that will be invoked instead of returning the "unimplemented" gRPC
// error whenever a request is received for an unregistered service or method.
// The handling function and stream interceptor (if set) have full access to
// the ServerStream, including its Context.
func UnknownServiceHandler(streamHandler StreamHandler) ServerOption {
	return newFuncServerOption(func(o *serverOptions) {
		o.unknownStreamDesc = &StreamDesc{
			StreamName: "unknown_service_handler",
			Handler:    streamHandler,
			// We need to assume that the users of the streamHandler will want to use both.
			ClientStreams: true,
			ServerStreams: true,
		}
	})
}

// ConnectionTimeout returns a ServerOption that sets the timeout for
// connection establishment (up to and including HTTP/2 handshaking) for all
// new connections.  If this is not set, the default is 120 seconds.  A zero or
// negative value will result in an immediate timeout.
//
// # Experimental
//
// Notice: This API is EXPERIMENTAL and may be changed or removed in a
// later release.
func ConnectionTimeout(d time.Duration) ServerOption {
	return newFuncServerOption(func(o *serverOptions) {
		o.connectionTimeout = d
	})
}

// MaxHeaderListSizeServerOption is a ServerOption that sets the max
// (uncompressed) size of header list that the server is prepared to accept.
type MaxHeaderListSizeServerOption struct {
	MaxHeaderListSize uint32
}

func (o MaxHeaderListSizeServerOption) apply(so *serverOptions) {
	so.maxHeaderListSize = &o.MaxHeaderListSize
}

// MaxHeaderListSize returns a ServerOption that sets the max (uncompressed) size
// of header list that the server is prepared to accept.
func MaxHeaderListSize(s uint32) ServerOption {
	return MaxHeaderListSizeServerOption{
		MaxHeaderListSize: s,
	}
}

// HeaderTableSize returns a ServerOption that sets the size of dynamic
// header table for stream.
//
// # Experimental
//
// Notice: This API is EXPERIMENTAL and may be changed or removed in a
// later release.
func HeaderTableSize(s uint32) ServerOption {
	return newFuncServerOption(func(o *serverOptions) {
		o.headerTableSize = &s
	})
}

// NumStreamWorkers returns a ServerOption that sets the number of worker
// goroutines that should be used to process incoming streams. Setting this to
// zero (default) will disable workers and spawn a new goroutine for each
// stream.
//
// # Experimental
//
// Notice: This API is EXPERIMENTAL and may be changed or removed in a
// later release.
func NumStreamWorkers(numServerWorkers uint32) ServerOption {
	// TODO: If/when this API gets stabilized (i.e. stream workers become the
	// only way streams are processed), change the behavior of the zero value to
	// a sane default. Preliminary experiments suggest that a value equal to the
	// number of CPUs available is most performant; requires thorough testing.
	return newFuncServerOption(func(o *serverOptions) {
		o.numServerWorkers = numServerWorkers
	})
}

// WaitForHandlers cause Stop to wait until all outstanding method handlers have
// exited before returning.  If false, Stop will return as soon as all
// connections have closed, but method handlers may still be running. By
// default, Stop does not wait for method handlers to return.
//
// # Experimental
//
// Notice: This API is EXPERIMENTAL and may be changed or removed in a
// later release.
func WaitForHandlers(w bool) ServerOption {
	return newFuncServerOption(func(o *serverOptions) {
		o.waitForHandlers = w
	})
}

func bufferPool(bufferPool mem.BufferPool) ServerOption {
	return newFuncServerOption(func(o *serverOptions) {
		o.bufferPool = bufferPool
	})
}

// serverWorkerResetThreshold defines how often the stack must be reset. Every
// N requests, by spawning a new goroutine in its place, a worker can reset its
// stack so that large stacks don't live in memory forever. 2^16 should allow
// each goroutine stack to live for at least a few seconds in a typical
// workload (assuming a QPS of a few thousand requests/sec).
const serverWorkerResetThreshold = 1 << 16

// serverWorker blocks on a *transport.Stream channel forever and waits for
// data to be fed by serveStreams. This allows multiple requests to be
// processed by the same goroutine, removing the need for expensive stack
// re-allocations (see the runtime.morestack problem [1]).
//
// [1] https://github.com/golang/go/issues/18138
func (s *Server) serverWorker() {
	for completed := 0; completed < serverWorkerResetThreshold; completed++ {
		f, ok := <-s.serverWorkerChannel
		if !ok {
			return
		}
		f()
	}
	go s.serverWorker()
}

// initServerWorkers creates worker goroutines and a channel to process incoming
// connections to reduce the time spent overall on runtime.morestack.
func (s *Server) initServerWorkers() {
	s.serverWorkerChannel = make(chan func())
	s.serverWorkerChannelClose = grpcsync.OnceFunc(func() {
		close(s.serverWorkerChannel)
	})
	for i := uint32(0); i < s.opts.numServerWorkers; i++ {
		go s.serverWorker()
	}
}

// NewServer creates a gRPC server which has no service registered and has not
// started to accept requests yet.
func NewServer(opt ...ServerOption) *Server {
	opts := defaultServerOptions
	for _, o := range globalServerOptions {
		o.apply(&opts)
	}
	for _, o := range opt {
		o.apply(&opts)
	}
	s := &Server{
		lis:      make(map[net.Listener]bool),
		opts:     opts,
		conns:    make(map[string]map[transport.ServerTransport]bool),
		services: make(map[string]*serviceInfo),
		quit:     grpcsync.NewEvent(),
		done:     grpcsync.NewEvent(),
		channelz: channelz.RegisterServer(""),
	}
	chainUnaryServerInterceptors(s)
	chainStreamServerInterceptors(s)
	s.cv = sync.NewCond(&s.mu)
	if EnableTracing {
		_, file, line, _ := runtime.Caller(1)
		s.events = newTraceEventLog("grpc.Server", fmt.Sprintf("%s:%d", file, line))
	}

	if s.opts.numServerWorkers > 0 {
		s.initServerWorkers()
	}

	channelz.Info(logger, s.channelz, "Server created")
	return s
}

// printf records an event in s's event log, unless s has been stopped.
// REQUIRES s.mu is held.
func (s *Server) printf(format string, a ...any) {
	if s.events != nil {
		s.events.Printf(format, a...)
	}
}

// errorf records an error in s's event log, unless s has been stopped.
// REQUIRES s.mu is held.
func (s *Server) errorf(format string, a ...any) {
	if s.events != nil {
		s.events.Errorf(format, a...)
	}
}

// ServiceRegistrar wraps a single method that supports service registration. It
// enables users to pass concrete types other than grpc.Server to the service
// registration methods exported by the IDL generated code.
type ServiceRegistrar interface {
	// RegisterService registers a service and its implementation to the
	// concrete type implementing this interface.  It may not be called
	// once the server has started serving.
	// desc describes the service and its methods and handlers. impl is the
	// service implementation which is passed to the method handlers.
	RegisterService(desc *ServiceDesc, impl any)
}

// RegisterService registers a service and its implementation to the gRPC
// server. It is called from the IDL generated code. This must be called before
// invoking Serve. If ss is non-nil (for legacy code), its type is checked to
// ensure it implements sd.HandlerType.
func (s *Server) RegisterService(sd *ServiceDesc, ss any) {
	if ss != nil {
		ht := reflect.TypeOf(sd.HandlerType).Elem()
		st := reflect.TypeOf(ss)
		if !st.Implements(ht) {
			logger.Fatalf("grpc: Server.RegisterService found the handler of type %v that does not satisfy %v", st, ht)
		}
	}
	s.register(sd, ss)
}

func (s *Server) register(sd *ServiceDesc, ss any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.printf("RegisterService(%q)", sd.ServiceName)
	if s.serve {
		logger.Fatalf("grpc: Server.RegisterService after Server.Serve for %q", sd.ServiceName)
	}
	if _, ok := s.services[sd.ServiceName]; ok {
		logger.Fatalf("grpc: Server.RegisterService found duplicate service registration for %q", sd.ServiceName)
	}
	info := &serviceInfo{
		serviceImpl: ss,
		methods:     make(map[string]*MethodDesc),
		streams:     make(map[string]*StreamDesc),
		mdata:       sd.Metadata,
	}
	for i := range sd.Methods {
		d := &sd.Methods[i]
		info.methods[d.MethodName] = d
	}
	for i := range sd.Streams {
		d := &sd.Streams[i]
		info.streams[d.StreamName] = d
	}
	s.services[sd.ServiceName] = info
}

// MethodInfo contains the information of an RPC including its method name and type.
type MethodInfo struct {
	// Name is the method name only, without the service name or package name.
	Name string
	// IsClientStream indicates whether the RPC is a client streaming RPC.
	IsClientStream bool
	// IsServerStream indicates whether the RPC is a server streaming RPC.
	IsServerStream bool
}

// ServiceInfo contains unary RPC method info, streaming RPC method info and metadata for a service.
type ServiceInfo struct {
	Methods []MethodInfo
	// Metadata is the metadata specified in ServiceDesc when registering service.
	Metadata any
}

// GetServiceInfo returns a map from service names to ServiceInfo.
// Service names include the package names, in the form of <package>.<service>.
func (s *Server) GetServiceInfo() map[string]ServiceInfo {
	ret := make(map[string]ServiceInfo)
	for n, srv := range s.services {
		methods := make([]MethodInfo, 0, len(srv.methods)+len(srv.streams))
		for m := range srv.methods {
			methods = append(methods, MethodInfo{
				Name:           m,
				IsClientStream: false,
				IsServerStream: false,
			})
		}
		for m, d := range srv.streams {
			methods = append(methods, MethodInfo{
				Name:           m,
				IsClientStream: d.ClientStreams,
				IsServerStream: d.ServerStreams,
			})
		}

		ret[n] = ServiceInfo{
			Methods:  methods,
			Metadata: srv.mdata,
		}
	}
	return ret
}

// ErrServerStopped indicates that the operation is now illegal because of
// the server being stopped.
var ErrServerStopped = errors.New("grpc: the server has been stopped")

type listenSocket struct {
	net.Listener
	channelz *channelz.Socket
}

func (l *listenSocket) Close() error {
	err := l.Listener.Close()
	channelz.RemoveEntry(l.channelz.ID)
	channelz.Info(logger, l.channelz, "ListenSocket deleted")
	return err
}

// Serve accepts incoming connections on the listener lis, creating a new
// ServerTransport and service goroutine for each. The service goroutines
// read gRPC requests and then call the registered handlers to reply to them.
// Serve returns when lis.Accept fails with fatal errors.  lis will be closed when
// this method returns.
// Serve will return a non-nil error unless Stop or GracefulStop is called.
//
// Note: All supported releases of Go (as of December 2023) override the OS
// defaults for TCP keepalive time and interval to 15s. To enable TCP keepalive
// with OS defaults for keepalive time and interval, callers need to do the
// following two things:
//   - pass a net.Listener created by calling the Listen method on a
//     net.ListenConfig with the `KeepAlive` field set to a negative value. This
//     will result in the Go standard library not overriding OS defaults for TCP
//     keepalive interval and time. But this will also result in the Go standard
//     library not enabling TCP keepalives by default.
//   - override the Accept method on the passed in net.Listener and set the
//     SO_KEEPALIVE socket option to enable TCP keepalives, with OS defaults.
func (s *Server) Serve(lis net.Listener) error {
	s.mu.Lock()
	s.printf("serving")
	s.serve = true
	if s.lis == nil {
		// Serve called after Stop or GracefulStop.
		s.mu.Unlock()
		lis.Close()
		return ErrServerStopped
	}

	s.serveWG.Add(1)
	defer func() {
		s.serveWG.Done()
		if s.quit.HasFired() {
			// Stop or GracefulStop called; block until done and return nil.
			<-s.done.Done()
		}
	}()

	ls := &listenSocket{
		Listener: lis,
		channelz: channelz.RegisterSocket(&channelz.Socket{
			SocketType:    channelz.SocketTypeListen,
			Parent:        s.channelz,
			RefName:       lis.Addr().String(),
			LocalAddr:     lis.Addr(),
			SocketOptions: channelz.GetSocketOption(lis)},
		),
	}
	s.lis[ls] = true

	defer func() {
		s.mu.Lock()
		if s.lis != nil && s.lis[ls] {
			ls.Close()
			delete(s.lis, ls)
		}
		s.mu.Unlock()
	}()

	s.mu.Unlock()
	channelz.Info(logger, ls.channelz, "ListenSocket created")

	var tempDelay time.Duration // how long to sleep on accept failure
	for {
		rawConn, err := lis.Accept()
		if err != nil {
			if ne, ok := err.(interface {
				Temporary() bool
			}); ok && ne.Temporary() {
				if tempDelay == 0 {
					tempDelay = 5 * time.Millisecond
				} else {
					tempDelay *= 2
				}
				if max := 1 * time.Second; tempDelay > max {
					tempDelay = max
				}
				s.mu.Lock()
				s.printf("Accept error: %v; retrying in %v", err, tempDelay)
				s.mu.Unlock()
				timer := time.NewTimer(tempDelay)
				select {
				case <-timer.C:
				case <-s.quit.Done():
					timer.Stop()
					return nil
				}
				continue
			}
			s.mu.Lock()
			s.printf("done serving; Accept = %v", err)
			s.mu.Unlock()

			if s.quit.HasFired() {
				return nil
			}
			return err
		}
		tempDelay = 0
		// Start a new goroutine to deal with rawConn so we don't stall this Accept
		// loop goroutine.
		//
		// Make sure we account for the goroutine so GracefulStop doesn't nil out
		// s.conns before this conn can be added.
		s.serveWG.Add(1)
		go func() {
			s.handleRawConn(lis.Addr().String(), rawConn)
			s.serveWG.Done()
		}()
	}
}

// handleRawConn forks a goroutine to handle a just-accepted connection that
// has not had any I/O performed on it yet.
func (s *Server) handleRawConn(lisAddr string, rawConn net.Conn) {
	if s.quit.HasFired() {
		rawConn.Close()
		return
	}
	rawConn.SetDeadline(time.Now().Add(s.opts.connectionTimeout))

	// Finish handshaking (HTTP2)
	st := s.newHTTP2Transport(rawConn)
	rawConn.SetDeadline(time.Time{})
	if st == nil {
		return
	}

	if cc, ok := rawConn.(interface {
		PassServerTransport(transport.ServerTransport)
	}); ok {
		cc.PassServerTransport(st)
	}

	if !s.addConn(lisAddr, st) {
		return
	}
	go func() {
		s.serveStreams(context.Background(), st, rawConn)
		s.removeConn(lisAddr, st)
	}()
}

// newHTTP2Transport sets up a http/2 transport (using the
// gRPC http2 server transport in transport/http2_server.go).
func (s *Server) newHTTP2Transport(c net.Conn) transport.ServerTransport {
	config := &transport.ServerConfig{
		MaxStreams:            s.opts.maxConcurrentStreams,
		ConnectionTimeout:     s.opts.connectionTimeout,
		Credentials:           s.opts.creds,
		InTapHandle:           s.opts.inTapHandle,
		StatsHandlers:         s.opts.statsHandlers,
		KeepaliveParams:       s.opts.keepaliveParams,
		KeepalivePolicy:       s.opts.keepalivePolicy,
		InitialWindowSize:     s.opts.initialWindowSize,
		InitialConnWindowSize: s.opts.initialConnWindowSize,
		WriteBufferSize:       s.opts.writeBufferSize,
		ReadBufferSize:        s.opts.readBufferSize,
		SharedWriteBuffer:     s.opts.sharedWriteBuffer,
		ChannelzParent:        s.channelz,
		MaxHeaderListSize:     s.opts.maxHeaderListSize,
		HeaderTableSize:       s.opts.headerTableSize,
		BufferPool:            s.opts.bufferPool,
	}
	st, err := transport.NewServerTransport(c, config)
	if err != nil {
		s.mu.Lock()
		s.errorf("NewServerTransport(%q) failed: %v", c.RemoteAddr(), err)
		s.mu.Unlock()
		// ErrConnDispatched means that the connection was dispatched away from
		// gRPC; those connections should be left open.
		if err != credentials.ErrConnDispatched {
			// Don't log on ErrConnDispatched and io.EOF to prevent log spam.
			if err != io.EOF {
				channelz.Info(logger, s.channelz, "grpc: Server.Serve failed to create ServerTransport: ", err)
			}
			c.Close()
		}
		return nil
	}

	return st
}

func (s *Server) serveStreams(ctx context.Context, st transport.ServerTransport, rawConn net.Conn) {
	ctx = transport.SetConnection(ctx, rawConn)
	ctx = peer.NewContext(ctx, st.Peer())
	for _, sh := range s.opts.statsHandlers {
		ctx = sh.TagConn(ctx, &stats.ConnTagInfo{
			RemoteAddr: st.Peer().Addr,
			LocalAddr:  st.Peer().LocalAddr,
		})
		sh.HandleConn(ctx, &stats.ConnBegin{})
	}

	defer func() {
		st.Close(errors.New("finished serving streams for the server transport"))
		for _, sh := range s.opts.statsHandlers {
			sh.HandleConn(ctx, &stats.ConnEnd{})
		}
	}()

	streamQuota := newHandlerQuota(s.opts.maxConcurrentStreams)
	st.HandleStreams(ctx, func(stream *transport.Stream) {
		s.handlersWG.Add(1)
		streamQuota.acquire()
		f := func() {
			defer streamQuota.release()
			defer s.handlersWG.Done()
			s.handleStream(st, stream)
		}

		if s.opts.numServerWorkers > 0 {
			select {
			case s.serverWorkerChannel <- f:
				return
			default:
				// If all stream workers are busy, fallback to the default code path.
			}
		}
		go f()
	})
}

var _ http.Handler = (*Server)(nil)

// ServeHTTP implements the Go standard library's http.Handler
// interface by responding to the gRPC request r, by looking up
// the requested gRPC method in the gRPC server s.
//
// The provided HTTP request must have arrived on an HTTP/2
// connection. When using the Go standard library's server,
// practically this means that the Request must also have arrived
// over TLS.
//
// To share one port (such as 443 for https) between gRPC and an
// existing http.Handler, use a root http.Handler such as:
//
//	if r.ProtoMajor == 2 && strings.HasPrefix(
//		r.Header.Get("Content-Type"), "application/grpc") {
//		grpcServer.ServeHTTP(w, r)
//	} else {
//		yourMux.ServeHTTP(w, r)
//	}
//
// Note that ServeHTTP uses Go's HTTP/2 server implementation which is totally
// separate from grpc-go's HTTP/2 server. Performance and features may vary
// between the two paths. ServeHTTP does not support some gRPC features
// available through grpc-go's HTTP/2 server.
//
// # Experimental
//
// Notice: This API is EXPERIMENTAL and may be changed or removed in a
// later release.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	st, err := transport.NewServerHandlerTransport(w, r, s.opts.statsHandlers, s.opts.bufferPool)
	if err != nil {
		// Errors returned from transport.NewServerHandlerTransport have
		// already been written to w.
		return
	}
	if !s.addConn(listenerAddressForServeHTTP, st) {
		return
	}
	defer s.removeConn(listenerAddressForServeHTTP, st)
	s.serveStreams(r.Context(), st, nil)
}

func (s *Server) addConn(addr string, st transport.ServerTransport) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conns == nil {
		st.Close(errors.New("Server.addConn called when server has already been stopped"))
		return false
	}
	if s.drain {
		// Transport added after we drained our existing conns: drain it
		// immediately.
		st.Drain("")
	}

	if s.conns[addr] == nil {
		// Create a map entry if this is the first connection on this listener.
		s.conns[addr] = make(map[transport.ServerTransport]bool)
	}
	s.conns[addr][st] = true
	return true
}

func (s *Server) removeConn(addr string, st transport.ServerTransport) {
	s.mu.Lock()
	defer s.mu.Unlock()

	conns := s.conns[addr]
	if conns != nil {
		delete(conns, st)
		if len(conns) == 0 {
			// If the last connection for this address is being removed, also
			// remove the map entry corresponding to the address. This is used
			// in GracefulStop() when waiting for all connections to be closed.
			delete(s.conns, addr)
		}
		s.cv.Broadcast()
	}
}

func (s *Server) incrCallsStarted() {
	s.channelz.ServerMetrics.CallsStarted.Add(1)
	s.channelz.ServerMetrics.LastCallStartedTimestamp.Store(time.Now().UnixNano())
}

func (s *Server) incrCallsSucceeded() {
	s.channelz.ServerMetrics.CallsSucceeded.Add(1)
}

func (s *Server) incrCallsFailed() {
	s.channelz.ServerMetrics.CallsFailed.Add(1)
}

func (s *Server) sendResponse(ctx context.Context, t transport.ServerTransport, stream *transport.Stream, msg any, cp Compressor, opts *transport.Options, comp encoding.Compressor) error {
	data, err := encode(s.getCodec(stream.ContentSubtype()), msg)
	if err != nil {
		channelz.Error(logger, s.channelz, "grpc: server failed to encode response: ", err)
		return err
	}

	compData, pf, err := compress(data, cp, comp, s.opts.bufferPool)
	if err != nil {
		data.Free()
		channelz.Error(logger, s.channelz, "grpc: server failed to compress response: ", err)
		return err
	}

	hdr, payload := msgHeader(data, compData, pf)

	defer func() {
		compData.Free()
		data.Free()
		// payload does not need to be freed here, it is either data or compData, both of
		// which are already freed.
	}()

	dataLen := data.Len()
	payloadLen := payload.Len()
	// TODO(dfawley): should we be checking len(data) instead?
	if payloadLen > s.opts.maxSendMessageSize {
		return status.Errorf(codes.ResourceExhausted, "grpc: trying to send message larger than max (%d vs. %d)", payloadLen, s.opts.maxSendMessageSize)
	}
	err = t.Write(stream, hdr, payload, opts)
	if err == nil {
		if len(s.opts.statsHandlers) != 0 {
			for _, sh := range s.opts.statsHandlers {
				sh.HandleRPC(ctx, outPayload(false, msg, dataLen, payloadLen, time.Now()))
			}
		}
	}
	return err
}

// chainUnaryServerInterceptors chains all unary server interceptors into one.
func chainUnaryServerInterceptors(s *Server) {
	// Prepend opts.unaryInt to the chaining interceptors if it exists, since unaryInt will
	// be executed before any other chained interceptors.
	interceptors := s.opts.chainUnaryInts
	if s.opts.unaryInt != nil {
		interceptors = append([]UnaryServerInterceptor{s.opts.unaryInt}, s.opts.chainUnaryInts...)
	}

	var chainedInt UnaryServerInterceptor
	if len(interceptors) == 0 {
		chainedInt = nil
	} else if len(interceptors) == 1 {
		chainedInt = interceptors[0]
	} else {
		chainedInt = chainUnaryInterceptors(interceptors)
	}

	s.opts.unaryInt = chainedInt
}

func chainUnaryInterceptors(interceptors []UnaryServerInterceptor) UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *UnaryServerInfo, handler UnaryHandler) (any, error) {
		return interceptors[0](ctx, req, info, getChainUnaryHandler(interceptors, 0, info, handler))
	}
}

func getChainUnaryHandler(interceptors []UnaryServerInterceptor, curr int, info *UnaryServerInfo, finalHandler UnaryHandler) UnaryHandler {
	if curr == len(interceptors)-1 {
		return finalHandler
	}
	return func(ctx context.Context, req any) (any, error) {
		return interceptors[curr+1](ctx, req, info, getChainUnaryHandler(interceptors, curr+1, info, finalHandler))
	}
}

func (s *Server) processUnaryRPC(ctx context.Context, t transport.ServerTransport, stream *transport.Stream, info *serviceInfo, md *MethodDesc, trInfo *traceInfo) (err error) {
	shs := s.opts.statsHandlers
	if len(shs) != 0 || trInfo != nil || channelz.IsOn() {
		if channelz.IsOn() {
			s.incrCallsStarted()
		}
		var statsBegin *stats.Begin
		for _, sh := range shs {
			beginTime := time.Now()
			statsBegin = &stats.Begin{
				BeginTime:      beginTime,
				IsClientStream: false,
				IsServerStream: false,
			}
			sh.HandleRPC(ctx, statsBegin)
		}
		if trInfo != nil {
			trInfo.tr.LazyLog(&trInfo.firstLine, false)
		}
		// The deferred error handling for tracing, stats handler and channelz are
		// combined into one function to reduce stack usage -- a defer takes ~56-64
		// bytes on the stack, so overflowing the stack will require a stack
		// re-allocation, which is expensive.
		//
		// To maintain behavior similar to separate deferred statements, statements
		// should be executed in the reverse order. That is, tracing first, stats
		// handler second, and channelz last. Note that panics *within* defers will
		// lead to different behavior, but that's an acceptable compromise; that
		// would be undefined behavior territory anyway.
		defer func() {
			if trInfo != nil {
				if err != nil && err != io.EOF {
					trInfo.tr.LazyLog(&fmtStringer{"%v", []any{err}}, true)
					trInfo.tr.SetError()
				}
				trInfo.tr.Finish()
			}

			for _, sh := range shs {
				end := &stats.End{
					BeginTime: statsBegin.BeginTime,
					EndTime:   time.Now(),
				}
				if err != nil && err != io.EOF {
					end.Error = toRPCErr(err)
				}
				sh.HandleRPC(ctx, end)
			}

			if channelz.IsOn() {
				if err != nil && err != io.EOF {
					s.incrCallsFailed()
				} else {
					s.incrCallsSucceeded()
				}
			}
		}()
	}
	var binlogs []binarylog.MethodLogger
	if ml := binarylog.GetMethodLogger(stream.Method()); ml != nil {
		binlogs = append(binlogs, ml)
	}
	if s.opts.binaryLogger != nil {
		if ml := s.opts.binaryLogger.GetMethodLogger(stream.Method()); ml != nil {
			binlogs = append(binlogs, ml)
		}
	}
	if len(binlogs) != 0 {
		md, _ := metadata.FromIncomingContext(ctx)
		logEntry := &binarylog.ClientHeader{
			Header:     md,
			MethodName: stream.Method(),
			PeerAddr:   nil,
		}
		if deadline, ok := ctx.Deadline(); ok {
			logEntry.Timeout = time.Until(deadline)
			if logEntry.Timeout < 0 {
				logEntry.Timeout = 0
			}
		}
		if a := md[":authority"]; len(a) > 0 {
			logEntry.Authority = a[0]
		}
		if peer, ok := peer.FromContext(ctx); ok {
			logEntry.PeerAddr = peer.Addr
		}
		for _, binlog := range binlogs {
			binlog.Log(ctx, logEntry)
		}
	}

	// comp and cp are used for compression.  decomp and dc are used for
	// decompression.  If comp and decomp are both set, they are the same;
	// however they are kept separate to ensure that at most one of the
	// compressor/decompressor variable pairs are set for use later.
	var comp, decomp encoding.Compressor
	var cp Compressor
	var dc Decompressor
	var sendCompressorName string

	// If dc is set and matches the stream's compression, use it.  Otherwise, try
	// to find a matching registered compressor for decomp.
	if rc := stream.RecvCompress(); s.opts.dc != nil && s.opts.dc.Type() == rc {
		dc = s.opts.dc
	} else if rc != "" && rc != encoding.Identity {
		decomp = encoding.GetCompressor(rc)
		if decomp == nil {
			st := status.Newf(codes.Unimplemented, "grpc: Decompressor is not installed for grpc-encoding %q", rc)
			t.WriteStatus(stream, st)
			return st.Err()
		}
	}

	// If cp is set, use it.  Otherwise, attempt to compress the response using
	// the incoming message compression method.
	//
	// NOTE: this needs to be ahead of all handling, https://github.com/grpc/grpc-go/issues/686.
	if s.opts.cp != nil {
		cp = s.opts.cp
		sendCompressorName = cp.Type()
	} else if rc := stream.RecvCompress(); rc != "" && rc != encoding.Identity {
		// Legacy compressor not specified; attempt to respond with same encoding.
		comp = encoding.GetCompressor(rc)
		if comp != nil {
			sendCompressorName = comp.Name()
		}
	}

	if sendCompressorName != "" {
		if err := stream.SetSendCompress(sendCompressorName); err != nil {
			return status.Errorf(codes.Internal, "grpc: failed to set send compressor: %v", err)
		}
	}

	var payInfo *payloadInfo
	if len(shs) != 0 || len(binlogs) != 0 {
		payInfo = &payloadInfo{}
		defer payInfo.free()
	}

	d, err := recvAndDecompress(&parser{r: stream, bufferPool: s.opts.bufferPool}, stream, dc, s.opts.maxReceiveMessageSize, payInfo, decomp, true)
	if err != nil {
		if e := t.WriteStatus(stream, status.Convert(err)); e != nil {
			channelz.Warningf(logger, s.channelz, "grpc: Server.processUnaryRPC failed to write status: %v", e)
		}
		return err
	}
	defer d.Free()
	if channelz.IsOn() {
		t.IncrMsgRecv()
	}
	df := func(v any) error {
		if err := s.getCodec(stream.ContentSubtype()).Unmarshal(d, v); err != nil {
			return status.Errorf(codes.Internal, "grpc: error unmarshalling request: %v", err)
		}

		for _, sh := range shs {
			sh.HandleRPC(ctx, &stats.InPayload{
				RecvTime:         time.Now(),
				Payload:          v,
				Length:           d.Len(),
				WireLength:       payInfo.compressedLength + headerLen,
				CompressedLength: payInfo.compressedLength,
			})
		}
		if len(binlogs) != 0 {
			cm := &binarylog.ClientMessage{
				Message: d.Materialize(),
			}
			for _, binlog := range binlogs {
				binlog.Log(ctx, cm)
			}
		}
		if trInfo != nil {
			trInfo.tr.LazyLog(&payload{sent: false, msg: v}, true)
		}
		return nil
	}
	ctx = NewContextWithServerTransportStream(ctx, stream)
	reply, appErr := md.Handler(info.serviceImpl, ctx, df, s.opts.unaryInt)
	if appErr != nil {
		appStatus, ok := status.FromError(appErr)
		if !ok {
			// Convert non-status application error to a status error with code
			// Unknown, but handle context errors specifically.
			appStatus = status.FromContextError(appErr)
			appErr = appStatus.Err()
		}
		if trInfo != nil {
			trInfo.tr.LazyLog(stringer(appStatus.Message()), true)
			trInfo.tr.SetError()
		}
		if e := t.WriteStatus(stream, appStatus); e != nil {
			channelz.Warningf(logger, s.channelz, "grpc: Server.processUnaryRPC failed to write status: %v", e)
		}
		if len(binlogs) != 0 {
			if h, _ := stream.Header(); h.Len() > 0 {
				// Only log serverHeader if there was header. Otherwise it can
				// be trailer only.
				sh := &binarylog.ServerHeader{
					Header: h,
				}
				for _, binlog := range binlogs {
					binlog.Log(ctx, sh)
				}
			}
			st := &binarylog.ServerTrailer{
				Trailer: stream.Trailer(),
				Err:     appErr,
			}
			for _, binlog := range binlogs {
				binlog.Log(ctx, st)
			}
		}
		return appErr
	}
	if trInfo != nil {
		trInfo.tr.LazyLog(stringer("OK"), false)
	}
	opts := &transport.Options{Last: true}

	// Server handler could have set new compressor by calling SetSendCompressor.
	// In case it is set, we need to use it for compressing outbound message.
	if stream.SendCompress() != sendCompressorName {
		comp = encoding.GetCompressor(stream.SendCompress())
	}
	if err := s.sendResponse(ctx, t, stream, reply, cp, opts, comp); err != nil {
		if err == io.EOF {
			// The entire stream is done (for unary RPC only).
			return err
		}
		if sts, ok := status.FromError(err); ok {
			if e := t.WriteStatus(stream, sts); e != nil {
				channelz.Warningf(logger, s.channelz, "grpc: Server.processUnaryRPC failed to write status: %v", e)
			}
		} else {
			switch st := err.(type) {
			case transport.ConnectionError:
				// Nothing to do here.
			default:
				panic(fmt.Sprintf("grpc: Unexpected error (%T) from sendResponse: %v", st, st))
			}
		}
		if len(binlogs) != 0 {
			h, _ := stream.Header()
			sh := &binarylog.ServerHeader{
				Header: h,
			}
			st := &binarylog.ServerTrailer{
				Trailer: stream.Trailer(),
				Err:     appErr,
			}
			for _, binlog := range binlogs {
				binlog.Log(ctx, sh)
				binlog.Log(ctx, st)
			}
		}
		return err
	}
	if len(binlogs) != 0 {
		h, _ := stream.Header()
		sh := &binarylog.ServerHeader{
			Header: h,
		}
		sm := &binarylog.ServerMessage{
			Message: reply,
		}
		for _, binlog := range binlogs {
			binlog.Log(ctx, sh)
			binlog.Log(ctx, sm)
		}
	}
	if channelz.IsOn() {
		t.IncrMsgSent()
	}
	if trInfo != nil {
		trInfo.tr.LazyLog(&payload{sent: true, msg: reply}, true)
	}
	// TODO: Should we be logging if writing status failed here, like above?
	// Should the logging be in WriteStatus?  Should we ignore the WriteStatus
	// error or allow the stats handler to see it?
	if len(binlogs) != 0 {
		st := &binarylog.ServerTrailer{
			Trailer: stream.Trailer(),
			Err:     appErr,
		}
		for _, binlog := range binlogs {
			binlog.Log(ctx, st)
		}
	}
	return t.WriteStatus(stream, statusOK)
}

// chainStreamServerInterceptors chains all stream server interceptors into one.
func chainStreamServerInterceptors(s *Server) {
	// Prepend opts.streamInt to the chaining interceptors if it exists, since streamInt will
	// be executed before any other chained interceptors.
	interceptors := s.opts.chainStreamInts
	if s.opts.streamInt != nil {
		interceptors = append([]StreamServerInterceptor{s.opts.streamInt}, s.opts.chainStreamInts...)
	}

	var chainedInt StreamServerInterceptor
	if len(interceptors) == 0 {
		chainedInt = nil
	} else if len(interceptors) == 1 {
		chainedInt = interceptors[0]
	} else {
		chainedInt = chainStreamInterceptors(interceptors)
	}

	s.opts.streamInt = chainedInt
}

func chainStreamInterceptors(interceptors []StreamServerInterceptor) StreamServerInterceptor {
	return func(srv any, ss ServerStream, info *StreamServerInfo, handler StreamHandler) error {
		return interceptors[0](srv, ss, info, getChainStreamHandler(interceptors, 0, info, handler))
	}
}

func getChainStreamHandler(interceptors []StreamServerInterceptor, curr int, info *StreamServerInfo, finalHandler StreamHandler) StreamHandler {
	if curr == len(interceptors)-1 {
		return finalHandler
	}
	return func(srv any, stream ServerStream) error {
		return interceptors[curr+1](srv, stream, info, getChainStreamHandler(interceptors, curr+1, info, finalHandler))
	}
}

func (s *Server) processStreamingRPC(ctx context.Context, t transport.ServerTransport, stream *transport.Stream, info *serviceInfo, sd *StreamDesc, trInfo *traceInfo) (err error) {
	if channelz.IsOn() {
		s.incrCallsStarted()
	}
	shs := s.opts.statsHandlers
	var statsBegin *stats.Begin
	if len(shs) != 0 {
		beginTime := time.Now()
		statsBegin = &stats.Begin{
			BeginTime:      beginTime,
			IsClientStream: sd.ClientStreams,
			IsServerStream: sd.ServerStreams,
		}
		for _, sh := range shs {
			sh.HandleRPC(ctx, statsBegin)
		}
	}
	ctx = NewContextWithServerTransportStream(ctx, stream)
	ss := &serverStream{
		ctx:                   ctx,
		t:                     t,
		s:                     stream,
		p:                     &parser{r: stream, bufferPool: s.opts.bufferPool},
		codec:                 s.getCodec(stream.ContentSubtype()),
		maxReceiveMessageSize: s.opts.maxReceiveMessageSize,
		maxSendMessageSize:    s.opts.maxSendMessageSize,
		trInfo:                trInfo,
		statsHandler:          shs,
	}

	if len(shs) != 0 || trInfo != nil || channelz.IsOn() {
		// See comment in processUnaryRPC on defers.
		defer func() {
			if trInfo != nil {
				ss.mu.Lock()
				if err != nil && err != io.EOF {
					ss.trInfo.tr.LazyLog(&fmtStringer{"%v", []any{err}}, true)
					ss.trInfo.tr.SetError()
				}
				ss.trInfo.tr.Finish()
				ss.trInfo.tr = nil
				ss.mu.Unlock()
			}

			if len(shs) != 0 {
				end := &stats.End{
					BeginTime: statsBegin.BeginTime,
					EndTime:   time.Now(),
				}
				if err != nil && err != io.EOF {
					end.Error = toRPCErr(err)
				}
				for _, sh := range shs {
					sh.HandleRPC(ctx, end)
				}
			}

			if channelz.IsOn() {
				if err != nil && err != io.EOF {
					s.incrCallsFailed()
				} else {
					s.incrCallsSucceeded()
				}
			}
		}()
	}

	if ml := binarylog.GetMethodLogger(stream.Method()); ml != nil {
		ss.binlogs = append(ss.binlogs, ml)
	}
	if s.opts.binaryLogger != nil {
		if ml := s.opts.binaryLogger.GetMethodLogger(stream.Method()); ml != nil {
			ss.binlogs = append(ss.binlogs, ml)
		}
	}
	if len(ss.binlogs) != 0 {
		md, _ := metadata.FromIncomingContext(ctx)
		logEntry := &binarylog.ClientHeader{
			Header:     md,
			MethodName: stream.Method(),
			PeerAddr:   nil,
		}
		if deadline, ok := ctx.Deadline(); ok {
			logEntry.Timeout = time.Until(deadline)
			if logEntry.Timeout < 0 {
				logEntry.Timeout = 0
			}
		}
		if a := md[":authority"]; len(a) > 0 {
			logEntry.Authority = a[0]
		}
		if peer, ok := peer.FromContext(ss.Context()); ok {
			logEntry.PeerAddr = peer.Addr
		}
		for _, binlog := range ss.binlogs {
			binlog.Log(ctx, logEntry)
		}
	}

	// If dc is set and matches the stream's compression, use it.  Otherwise, try
	// to find a matching registered compressor for decomp.
	if rc := stream.RecvCompress(); s.opts.dc != nil && s.opts.dc.Type() == rc {
		ss.dc = s.opts.dc
	} else if rc != "" && rc != encoding.Identity {
		ss.decomp = encoding.GetCompressor(rc)
		if ss.decomp == nil {
			st := status.Newf(codes.Unimplemented, "grpc: Decompressor is not installed for grpc-encoding %q", rc)
			t.WriteStatus(ss.s, st)
			return st.Err()
		}
	}

	// If cp is set, use it.  Otherwise, attempt to compress the response using
	// the incoming message compression method.
	//
	// NOTE: this needs to be ahead of all handling, https://github.com/grpc/grpc-go/issues/686.
	if s.opts.cp != nil {
		ss.cp = s.opts.cp
		ss.sendCompressorName = s.opts.cp.Type()
	} else if rc := stream.RecvCompress(); rc != "" && rc != encoding.Identity {
		// Legacy compressor not specified; attempt to respond with same encoding.
		ss.comp = encoding.GetCompressor(rc)
		if ss.comp != nil {
			ss.sendCompressorName = rc
		}
	}

	if ss.sendCompressorName != "" {
		if err := stream.SetSendCompress(ss.sendCompressorName); err != nil {
			return status.Errorf(codes.Internal, "grpc: failed to set send compressor: %v", err)
		}
	}

	ss.ctx = newContextWithRPCInfo(ss.ctx, false, ss.codec, ss.cp, ss.comp)

	if trInfo != nil {
		trInfo.tr.LazyLog(&trInfo.firstLine, false)
	}
	var appErr error
	var server any
	if info != nil {
		server = info.serviceImpl
	}
	if s.opts.streamInt == nil {
		appErr = sd.Handler(server, ss)
	} else {
		info := &StreamServerInfo{
			FullMethod:     stream.Method(),
			IsClientStream: sd.ClientStreams,
			IsServerStream: sd.ServerStreams,
		}
		appErr = s.opts.streamInt(server, ss, info, sd.Handler)
	}
	if appErr != nil {
		appStatus, ok := status.FromError(appErr)
		if !ok {
			// Convert non-status application error to a status error with code
			// Unknown, but handle context errors specifically.
			appStatus = status.FromContextError(appErr)
			appErr = appStatus.Err()
		}
		if trInfo != nil {
			ss.mu.Lock()
			ss.trInfo.tr.LazyLog(stringer(appStatus.Message()), true)
			ss.trInfo.tr.SetError()
			ss.mu.Unlock()
		}
		if len(ss.binlogs) != 0 {
			st := &binarylog.ServerTrailer{
				Trailer: ss.s.Trailer(),
				Err:     appErr,
			}
			for _, binlog := range ss.binlogs {
				binlog.Log(ctx, st)
			}
		}
		t.WriteStatus(ss.s, appStatus)
		// TODO: Should we log an error from WriteStatus here and below?
		return appErr
	}
	if trInfo != nil {
		ss.mu.Lock()
		ss.trInfo.tr.LazyLog(stringer("OK"), false)
		ss.mu.Unlock()
	}
	if len(ss.binlogs) != 0 {
		st := &binarylog.ServerTrailer{
			Trailer: ss.s.Trailer(),
			Err:     appErr,
		}
		for _, binlog := range ss.binlogs {
			binlog.Log(ctx, st)
		}
	}
	return t.WriteStatus(ss.s, statusOK)
}

func (s *Server) handleStream(t transport.ServerTransport, stream *transport.Stream) {
	ctx := stream.Context()
	ctx = contextWithServer(ctx, s)
	var ti *traceInfo
	if EnableTracing {
		tr := newTrace("grpc.Recv."+methodFamily(stream.Method()), stream.Method())
		ctx = newTraceContext(ctx, tr)
		ti = &traceInfo{
			tr: tr,
			firstLine: firstLine{
				client:     false,
				remoteAddr: t.Peer().Addr,
			},
		}
		if dl, ok := ctx.Deadline(); ok {
			ti.firstLine.deadline = time.Until(dl)
		}
	}

	sm := stream.Method()
	if sm != "" && sm[0] == '/' {
		sm = sm[1:]
	}
	pos := strings.LastIndex(sm, "/")
	if pos == -1 {
		if ti != nil {
			ti.tr.LazyLog(&fmtStringer{"Malformed method name %q", []any{sm}}, true)
			ti.tr.SetError()
		}
		errDesc := fmt.Sprintf("malformed method name: %q", stream.Method())
		if err := t.WriteStatus(stream, status.New(codes.Unimplemented, errDesc)); err != nil {
			if ti != nil {
				ti.tr.LazyLog(&fmtStringer{"%v", []any{err}}, true)
				ti.tr.SetError()
			}
			channelz.Warningf(logger, s.channelz, "grpc: Server.handleStream failed to write status: %v", err)
		}
		if ti != nil {
			ti.tr.Finish()
		}
		return
	}
	service := sm[:pos]
	method := sm[pos+1:]

	md, _ := metadata.FromIncomingContext(ctx)
	for _, sh := range s.opts.statsHandlers {
		ctx = sh.TagRPC(ctx, &stats.RPCTagInfo{FullMethodName: stream.Method()})
		sh.HandleRPC(ctx, &stats.InHeader{
			FullMethod:  stream.Method(),
			RemoteAddr:  t.Peer().Addr,
			LocalAddr:   t.Peer().LocalAddr,
			Compression: stream.RecvCompress(),
			WireLength:  stream.HeaderWireLength(),
			Header:      md,
		})
	}
	// To have calls in stream callouts work. Will delete once all stats handler
	// calls come from the gRPC layer.
	stream.SetContext(ctx)

	srv, knownService := s.services[service]
	if knownService {
		if md, ok := srv.methods[method]; ok {
			s.processUnaryRPC(ctx, t, stream, srv, md, ti)
			return
		}
		if sd, ok := srv.streams[method]; ok {
			s.processStreamingRPC(ctx, t, stream, srv, sd, ti)
			return
		}
	}
	// Unknown service, or known server unknown method.
	if unknownDesc := s.opts.unknownStreamDesc; unknownDesc != nil {
		s.processStreamingRPC(ctx, t, stream, nil, unknownDesc, ti)
		return
	}
	var errDesc string
	if !knownService {
		errDesc = fmt.Sprintf("unknown service %v", service)
	} else {
		errDesc = fmt.Sprintf("unknown method %v for service %v", method, service)
	}
	if ti != nil {
		ti.tr.LazyPrintf("%s", errDesc)
		ti.tr.SetError()
	}
	if err := t.WriteStatus(stream, status.New(codes.Unimplemented, errDesc)); err != nil {
		if ti != nil {
			ti.tr.LazyLog(&fmtStringer{"%v", []any{err}}, true)
			ti.tr.SetError()
		}
		channelz.Warningf(logger, s.channelz, "grpc: Server.handleStream failed to write status: %v", err)
	}
	if ti != nil {
		ti.tr.Finish()
	}
}

// The key to save ServerTransportStream in the context.
type streamKey struct{}

// NewContextWithServerTransportStream creates a new context from ctx and
// attaches stream to it.
//
// # Experimental
//
// Notice: This API is EXPERIMENTAL and may be changed or removed in a
// later release.
func NewContextWithServerTransportStream(ctx context.Context, stream ServerTransportStream) context.Context {
	return context.WithValue(ctx, streamKey{}, stream)
}

// ServerTransportStream is a minimal interface that a transport stream must
// implement. This can be used to mock an actual transport stream for tests of
// handler code that use, for example, grpc.SetHeader (which requires some
// stream to be in context).
//
// See also NewContextWithServerTransportStream.
//
// # Experimental
//
// Notice: This type is EXPERIMENTAL and may be changed or removed in a
// later release.
type ServerTransportStream interface {
	Method() string
	SetHeader(md metadata.MD) error
	SendHeader(md metadata.MD) error
	SetTrailer(md metadata.MD) error
}

// ServerTransportStreamFromContext returns the ServerTransportStream saved in
// ctx. Returns nil if the given context has no stream associated with it
// (which implies it is not an RPC invocation context).
//
// # Experimental
//
// Notice: This API is EXPERIMENTAL and may be changed or removed in a
// later release.
func ServerTransportStreamFromContext(ctx context.Context) ServerTransportStream {
	s, _ := ctx.Value(streamKey{}).(ServerTransportStream)
	return s
}

// Stop stops the gRPC server. It immediately closes all open
// connections and listeners.
// It cancels all active RPCs on the server side and the corresponding
// pending RPCs on the client side will get notified by connection
// errors.
func (s *Server) Stop() {
	s.stop(false)
}

// GracefulStop stops the gRPC server gracefully. It stops the server from
// accepting new connections and RPCs and blocks until all the pending RPCs are
// finished.
func (s *Server) GracefulStop() {
	s.stop(true)
}

func (s *Server) stop(graceful bool) {
	s.quit.Fire()
	defer s.done.Fire()

	s.channelzRemoveOnce.Do(func() { channelz.RemoveEntry(s.channelz.ID) })
	s.mu.Lock()
	s.closeListenersLocked()
	// Wait for serving threads to be ready to exit.  Only then can we be sure no
	// new conns will be created.
	s.mu.Unlock()
	s.serveWG.Wait()

	s.mu.Lock()
	defer s.mu.Unlock()

	if graceful {
		s.drainAllServerTransportsLocked()
	} else {
		s.closeServerTransportsLocked()
	}

	for len(s.conns) != 0 {
		s.cv.Wait()
	}
	s.conns = nil

	if s.opts.numServerWorkers > 0 {
		// Closing the channel (only once, via grpcsync.OnceFunc) after all the
		// connections have been closed above ensures that there are no
		// goroutines executing the callback passed to st.HandleStreams (where
		// the channel is written to).
		s.serverWorkerChannelClose()
	}

	if graceful || s.opts.waitForHandlers {
		s.handlersWG.Wait()
	}

	if s.events != nil {
		s.events.Finish()
		s.events = nil
	}
}

// s.mu must be held by the caller.
func (s *Server) closeServerTransportsLocked() {
	for _, conns := range s.conns {
		for st := range conns {
			st.Close(errors.New("Server.Stop called"))
		}
	}
}

// s.mu must be held by the caller.
func (s *Server) drainAllServerTransportsLocked() {
	if !s.drain {
		for _, conns := range s.conns {
			for st := range conns {
				st.Drain("graceful_stop")
			}
		}
		s.drain = true
	}
}

// s.mu must be held by the caller.
func (s *Server) closeListenersLocked() {
	for lis := range s.lis {
		lis.Close()
	}
	s.lis = nil
}

// contentSubtype must be lowercase
// cannot return nil
func (s *Server) getCodec(contentSubtype string) baseCodec {
	if s.opts.codec != nil {
		return s.opts.codec
	}
	if contentSubtype == "" {
		return getCodec(proto.Name)
	}
	codec := getCodec(contentSubtype)
	if codec == nil {
		logger.Warningf("Unsupported codec %q. Defaulting to %q for now. This will start to fail in future releases.", contentSubtype, proto.Name)
		return getCodec(proto.Name)
	}
	return codec
}

type serverKey struct{}

// serverFromContext gets the Server from the context.
func serverFromContext(ctx context.Context) *Server {
	s, _ := ctx.Value(serverKey{}).(*Server)
	return s
}

// contextWithServer sets the Server in the context.
func contextWithServer(ctx context.Context, server *Server) context.Context {
	return context.WithValue(ctx, serverKey{}, server)
}

// isRegisteredMethod returns whether the passed in method is registered as a
// method on the server. /service/method and service/method will match if the
// service and method are registered on the server.
func (s *Server) isRegisteredMethod(serviceMethod string) bool {
	if serviceMethod != "" && serviceMethod[0] == '/' {
		serviceMethod = serviceMethod[1:]
	}
	pos := strings.LastIndex(serviceMethod, "/")
	if pos == -1 { // Invalid method name syntax.
		return false
	}
	service := serviceMethod[:pos]
	method := serviceMethod[pos+1:]
	srv, knownService := s.services[service]
	if knownService {
		if _, ok := srv.methods[method]; ok {
			return true
		}
		if _, ok := srv.streams[method]; ok {
			return true
		}
	}
	return false
}

// SetHeader sets the header metadata to be sent from the server to the client.
// The context provided must be the context passed to the server's handler.
//
// Streaming RPCs should prefer the SetHeader method of the ServerStream.
//
// When called multiple times, all the provided metadata will be merged.  All
// the metadata will be sent out when one of the following happens:
//
//   - grpc.SendHeader is called, or for streaming handlers, stream.SendHeader.
//   - The first response message is sent.  For unary handlers, this occurs when
//     the handler returns; for streaming handlers, this can happen when stream's
//     SendMsg method is called.
//   - An RPC status is sent out (error or success).  This occurs when the handler
//     returns.
//
// SetHeader will fail if called after any of the events above.
//
// The error returned is compatible with the status package.  However, the
// status code will often not match the RPC status as seen by the client
// application, and therefore, should not be relied upon for this purpose.
func SetHeader(ctx context.Context, md metadata.MD) error {
	if md.Len() == 0 {
		return nil
	}
	stream := ServerTransportStreamFromContext(ctx)
	if stream == nil {
		return status.Errorf(codes.Internal, "grpc: failed to fetch the stream from the context %v", ctx)
	}
	return stream.SetHeader(md)
}

// SendHeader sends header metadata. It may be called at most once, and may not
// be called after any event that causes headers to be sent (see SetHeader for
// a complete list).  The provided md and headers set by SetHeader() will be
// sent.
//
// The error returned is compatible with the status package.  However, the
// status code will often not match the RPC status as seen by the client
// application, and therefore, should not be relied upon for this purpose.
func SendHeader(ctx context.Context, md metadata.MD) error {
	stream := ServerTransportStreamFromContext(ctx)
	if stream == nil {
		return status.Errorf(codes.Internal, "grpc: failed to fetch the stream from the context %v", ctx)
	}
	if err := stream.SendHeader(md); err != nil {
		return toRPCErr(err)
	}
	return nil
}

// SetSendCompressor sets a compressor for outbound messages from the server.
// It must not be called after any event that causes headers to be sent
// (see ServerStream.SetHeader for the complete list). Provided compressor is
// used when below conditions are met:
//
//   - compressor is registered via encoding.RegisterCompressor
//   - compressor name must exist in the client advertised compressor names
//     sent in grpc-accept-encoding header. Use ClientSupportedCompressors to
//     get client supported compressor names.
//
// The context provided must be the context passed to the server's handler.
// It must be noted that compressor name encoding.Identity disables the
// outbound compression.
// By default, server messages will be sent using the same compressor with
// which request messages were sent.
//
// It is not safe to call SetSendCompressor concurrently with SendHeader and
// SendMsg.
//
// # Experimental
//
// Notice: This function is EXPERIMENTAL and may be changed or removed in a
// later release.
func SetSendCompressor(ctx context.Context, name string) error {
	stream, ok := ServerTransportStreamFromContext(ctx).(*transport.Stream)
	if !ok || stream == nil {
		return fmt.Errorf("failed to fetch the stream from the given context")
	}

	if err := validateSendCompressor(name, stream.ClientAdvertisedCompressors()); err != nil {
		return fmt.Errorf("unable to set send compressor: %w", err)
	}

	return stream.SetSendCompress(name)
}

// ClientSupportedCompressors returns compressor names advertised by the client
// via grpc-accept-encoding header.
//
// The context provided must be the context passed to the server's handler.
//
// # Experimental
//
// Notice: This function is EXPERIMENTAL and may be changed or removed in a
// later release.
func ClientSupportedCompressors(ctx context.Context) ([]string, error) {
	stream, ok := ServerTransportStreamFromContext(ctx).(*transport.Stream)
	if !ok || stream == nil {
		return nil, fmt.Errorf("failed to fetch the stream from the given context %v", ctx)
	}

	return stream.ClientAdvertisedCompressors(), nil
}

// SetTrailer sets the trailer metadata that will be sent when an RPC returns.
// When called more than once, all the provided metadata will be merged.
//
// The error returned is compatible with the status package.  However, the
// status code will often not match the RPC status as seen by the client
// application, and therefore, should not be relied upon for this purpose.
func SetTrailer(ctx context.Context, md metadata.MD) error {
	if md.Len() == 0 {
		return nil
	}
	stream := ServerTransportStreamFromContext(ctx)
	if stream == nil {
		return status.Errorf(codes.Internal, "grpc: failed to fetch the stream from the context %v", ctx)
	}
	return stream.SetTrailer(md)
}

// Method returns the method string for the server context.  The returned
// string is in the format of "/service/method".
func Method(ctx context.Context) (string, bool) {
	s := ServerTransportStreamFromContext(ctx)
	if s == nil {
		return "", false
	}
	return s.Method(), true
}

// validateSendCompressor returns an error when given compressor name cannot be
// handled by the server or the client based on the advertised compressors.
func validateSendCompressor(name string, clientCompressors []string) error {
	if name == encoding.Identity {
		return nil
	}

	if !grpcutil.IsCompressorNameRegistered(name) {
		return fmt.Errorf("compressor not registered %q", name)
	}

	for _, c := range clientCompressors {
		if c == name {
			return nil // found match
		}
	}
	return fmt.Errorf("client does not support compressor %q", name)
}

// atomicSemaphore implements a blocking, counting semaphore. acquire should be
// called synchronously; release may be called asynchronously.
type atomicSemaphore struct {
	n    atomic.Int64
	wait chan struct{}
}

func (q *atomicSemaphore) acquire() {
	if q.n.Add(-1) < 0 {
		// We ran out of quota.  Block until a release happens.
		<-q.wait
	}
}

func (q *atomicSemaphore) release() {
	// N.B. the "<= 0" check below should allow for this to work with multiple
	// concurrent calls to acquire, but also note that with synchronous calls to
	// acquire, as our system does, n will never be less than -1.  There are
	// fairness issues (queuing) to consider if this was to be generalized.
	if q.n.Add(1) <= 0 {
		// An acquire was waiting on us.  Unblock it.
		q.wait <- struct{}{}
	}
}

func newHandlerQuota(n uint32) *atomicSemaphore {
	a := &atomicSemaphore{wait: make(chan struct{}, 1)}
	a.n.Store(int64(n))
	return a
}
