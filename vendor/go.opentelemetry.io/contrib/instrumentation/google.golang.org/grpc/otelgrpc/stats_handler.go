// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package otelgrpc // import "go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"

import (
	"context"
	"sync/atomic"
	"time"

	grpc_codes "google.golang.org/grpc/codes"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/stats"
	"google.golang.org/grpc/status"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc/internal"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	semconv "go.opentelemetry.io/otel/semconv/v1.17.0"
	"go.opentelemetry.io/otel/trace"
)

type gRPCContextKey struct{}

type gRPCContext struct {
	messagesReceived int64
	messagesSent     int64
	metricAttrs      []attribute.KeyValue
	record           bool
}

type serverHandler struct {
	*config
}

// NewServerHandler creates a stats.Handler for a gRPC server.
func NewServerHandler(opts ...Option) stats.Handler {
	h := &serverHandler{
		config: newConfig(opts, "server"),
	}

	return h
}

// TagConn can attach some information to the given context.
func (h *serverHandler) TagConn(ctx context.Context, info *stats.ConnTagInfo) context.Context {
	return ctx
}

// HandleConn processes the Conn stats.
func (h *serverHandler) HandleConn(ctx context.Context, info stats.ConnStats) {
}

// TagRPC can attach some information to the given context.
func (h *serverHandler) TagRPC(ctx context.Context, info *stats.RPCTagInfo) context.Context {
	ctx = extract(ctx, h.config.Propagators)

	name, attrs := internal.ParseFullMethod(info.FullMethodName)
	attrs = append(attrs, RPCSystemGRPC)
	ctx, _ = h.tracer.Start(
		trace.ContextWithRemoteSpanContext(ctx, trace.SpanContextFromContext(ctx)),
		name,
		trace.WithSpanKind(trace.SpanKindServer),
		trace.WithAttributes(append(attrs, h.config.SpanAttributes...)...),
	)

	gctx := gRPCContext{
		metricAttrs: append(attrs, h.config.MetricAttributes...),
		record:      true,
	}
	if h.config.Filter != nil {
		gctx.record = h.config.Filter(info)
	}
	return context.WithValue(ctx, gRPCContextKey{}, &gctx)
}

// HandleRPC processes the RPC stats.
func (h *serverHandler) HandleRPC(ctx context.Context, rs stats.RPCStats) {
	isServer := true
	h.handleRPC(ctx, rs, isServer)
}

type clientHandler struct {
	*config
}

// NewClientHandler creates a stats.Handler for a gRPC client.
func NewClientHandler(opts ...Option) stats.Handler {
	h := &clientHandler{
		config: newConfig(opts, "client"),
	}

	return h
}

// TagRPC can attach some information to the given context.
func (h *clientHandler) TagRPC(ctx context.Context, info *stats.RPCTagInfo) context.Context {
	name, attrs := internal.ParseFullMethod(info.FullMethodName)
	attrs = append(attrs, RPCSystemGRPC)
	ctx, _ = h.tracer.Start(
		ctx,
		name,
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(append(attrs, h.config.SpanAttributes...)...),
	)

	gctx := gRPCContext{
		metricAttrs: append(attrs, h.config.MetricAttributes...),
		record:      true,
	}
	if h.config.Filter != nil {
		gctx.record = h.config.Filter(info)
	}

	return inject(context.WithValue(ctx, gRPCContextKey{}, &gctx), h.config.Propagators)
}

// HandleRPC processes the RPC stats.
func (h *clientHandler) HandleRPC(ctx context.Context, rs stats.RPCStats) {
	isServer := false
	h.handleRPC(ctx, rs, isServer)
}

// TagConn can attach some information to the given context.
func (h *clientHandler) TagConn(ctx context.Context, info *stats.ConnTagInfo) context.Context {
	return ctx
}

// HandleConn processes the Conn stats.
func (h *clientHandler) HandleConn(context.Context, stats.ConnStats) {
	// no-op
}

func (c *config) handleRPC(ctx context.Context, rs stats.RPCStats, isServer bool) { // nolint: revive  // isServer is not a control flag.
	span := trace.SpanFromContext(ctx)
	var metricAttrs []attribute.KeyValue
	var messageId int64

	gctx, _ := ctx.Value(gRPCContextKey{}).(*gRPCContext)
	if gctx != nil {
		if !gctx.record {
			return
		}
		metricAttrs = make([]attribute.KeyValue, 0, len(gctx.metricAttrs)+1)
		metricAttrs = append(metricAttrs, gctx.metricAttrs...)
	}

	switch rs := rs.(type) {
	case *stats.Begin:
	case *stats.InPayload:
		if gctx != nil {
			messageId = atomic.AddInt64(&gctx.messagesReceived, 1)
			c.rpcRequestSize.Record(ctx, int64(rs.Length), metric.WithAttributeSet(attribute.NewSet(metricAttrs...)))
		}

		if c.ReceivedEvent {
			span.AddEvent("message",
				trace.WithAttributes(
					semconv.MessageTypeReceived,
					semconv.MessageIDKey.Int64(messageId),
					semconv.MessageCompressedSizeKey.Int(rs.CompressedLength),
					semconv.MessageUncompressedSizeKey.Int(rs.Length),
				),
			)
		}
	case *stats.OutPayload:
		if gctx != nil {
			messageId = atomic.AddInt64(&gctx.messagesSent, 1)
			c.rpcResponseSize.Record(ctx, int64(rs.Length), metric.WithAttributeSet(attribute.NewSet(metricAttrs...)))
		}

		if c.SentEvent {
			span.AddEvent("message",
				trace.WithAttributes(
					semconv.MessageTypeSent,
					semconv.MessageIDKey.Int64(messageId),
					semconv.MessageCompressedSizeKey.Int(rs.CompressedLength),
					semconv.MessageUncompressedSizeKey.Int(rs.Length),
				),
			)
		}
	case *stats.OutTrailer:
	case *stats.OutHeader:
		if p, ok := peer.FromContext(ctx); ok {
			span.SetAttributes(peerAttr(p.Addr.String())...)
		}
	case *stats.End:
		var rpcStatusAttr attribute.KeyValue

		if rs.Error != nil {
			s, _ := status.FromError(rs.Error)
			if isServer {
				statusCode, msg := serverStatus(s)
				span.SetStatus(statusCode, msg)
			} else {
				span.SetStatus(codes.Error, s.Message())
			}
			rpcStatusAttr = semconv.RPCGRPCStatusCodeKey.Int(int(s.Code()))
		} else {
			rpcStatusAttr = semconv.RPCGRPCStatusCodeKey.Int(int(grpc_codes.OK))
		}
		span.SetAttributes(rpcStatusAttr)
		span.End()

		metricAttrs = append(metricAttrs, rpcStatusAttr)
		// Allocate vararg slice once.
		recordOpts := []metric.RecordOption{metric.WithAttributeSet(attribute.NewSet(metricAttrs...))}

		// Use floating point division here for higher precision (instead of Millisecond method).
		// Measure right before calling Record() to capture as much elapsed time as possible.
		elapsedTime := float64(rs.EndTime.Sub(rs.BeginTime)) / float64(time.Millisecond)

		c.rpcDuration.Record(ctx, elapsedTime, recordOpts...)
		if gctx != nil {
			c.rpcRequestsPerRPC.Record(ctx, atomic.LoadInt64(&gctx.messagesReceived), recordOpts...)
			c.rpcResponsesPerRPC.Record(ctx, atomic.LoadInt64(&gctx.messagesSent), recordOpts...)
		}
	default:
		return
	}
}
