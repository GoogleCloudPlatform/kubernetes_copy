// Copyright 2016 The etcd Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Based on github.com/grpc-ecosystem/go-grpc-middleware/retry, but modified to support the more
// fine grained error checking required by write-at-most-once retry semantics of etcd.

package clientv3

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"go.etcd.io/etcd/api/v3/v3rpc/rpctypes"
)

// unaryClientInterceptor returns a new retrying unary client interceptor.
//
// The default configuration of the interceptor is to not retry *at all*. This behaviour can be
// changed through options (e.g. WithMax) on creation of the interceptor or on call (through grpc.CallOptions).
func (c *Client) unaryClientInterceptor(optFuncs ...retryOption) grpc.UnaryClientInterceptor {
	intOpts := reuseOrNewWithCallOptions(defaultOptions, optFuncs)
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		ctx = withVersion(ctx)
		grpcOpts, retryOpts := filterCallOptions(opts)
		callOpts := reuseOrNewWithCallOptions(intOpts, retryOpts)
		// short circuit for simplicity, and avoiding allocations.
		if callOpts.max == 0 {
			return invoker(ctx, method, req, reply, cc, grpcOpts...)
		}
		var lastErr error
		for attempt := uint(0); attempt < callOpts.max; attempt++ {
			if err := waitRetryBackoff(ctx, attempt, callOpts); err != nil {
				return err
			}
			c.GetLogger().Debug(
				"retrying of unary invoker",
				zap.String("target", cc.Target()),
				zap.String("method", method),
				zap.Uint("attempt", attempt),
			)
			lastErr = invoker(ctx, method, req, reply, cc, grpcOpts...)
			if lastErr == nil {
				return nil
			}
			c.GetLogger().Warn(
				"retrying of unary invoker failed",
				zap.String("target", cc.Target()),
				zap.String("method", method),
				zap.Uint("attempt", attempt),
				zap.Error(lastErr),
			)
			if isContextError(lastErr) {
				if ctx.Err() != nil {
					// its the context deadline or cancellation.
					return lastErr
				}
				// its the callCtx deadline or cancellation, in which case try again.
				continue
			}
			if c.shouldRefreshToken(lastErr, callOpts) {
				gtErr := c.refreshToken(ctx)
				if gtErr != nil {
					c.GetLogger().Warn(
						"retrying of unary invoker failed to fetch new auth token",
						zap.String("target", cc.Target()),
						zap.Error(gtErr),
					)
					return gtErr // lastErr must be invalid auth token
				}
				continue
			}
			if !isSafeRetry(c, lastErr, callOpts) {
				return lastErr
			}
		}
		return lastErr
	}
}

// streamClientInterceptor returns a new retrying stream client interceptor for server side streaming calls.
//
// The default configuration of the interceptor is to not retry *at all*. This behaviour can be
// changed through options (e.g. WithMax) on creation of the interceptor or on call (through grpc.CallOptions).
//
// Retry logic is available *only for ServerStreams*, i.e. 1:n streams, as the internal logic needs
// to buffer the messages sent by the client. If retry is enabled on any other streams (ClientStreams,
// BidiStreams), the retry interceptor will fail the call.
func (c *Client) streamClientInterceptor(optFuncs ...retryOption) grpc.StreamClientInterceptor {
	intOpts := reuseOrNewWithCallOptions(defaultOptions, optFuncs)
	return func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
		ctx = withVersion(ctx)
		// getToken automatically. Otherwise, auth token may be invalid after watch reconnection because the token has expired
		// (see https://github.com/etcd-io/etcd/issues/11954 for more).
		err := c.getToken(ctx)
		if err != nil {
			c.GetLogger().Error("clientv3/retry_interceptor: getToken failed", zap.Error(err))
			return nil, err
		}
		grpcOpts, retryOpts := filterCallOptions(opts)
		callOpts := reuseOrNewWithCallOptions(intOpts, retryOpts)
		// short circuit for simplicity, and avoiding allocations.
		if callOpts.max == 0 {
			return streamer(ctx, desc, cc, method, grpcOpts...)
		}
		if desc.ClientStreams {
			return nil, status.Errorf(codes.Unimplemented, "clientv3/retry_interceptor: cannot retry on ClientStreams, set Disable()")
		}
		newStreamer, err := streamer(ctx, desc, cc, method, grpcOpts...)
		if err != nil {
			c.GetLogger().Error("streamer failed to create ClientStream", zap.Error(err))
			return nil, err // TODO(mwitkow): Maybe dial and transport errors should be retriable?
		}
		retryingStreamer := &serverStreamingRetryingStream{
			client:       c,
			ClientStream: newStreamer,
			callOpts:     callOpts,
			ctx:          ctx,
			streamerCall: func(ctx context.Context) (grpc.ClientStream, error) {
				return streamer(ctx, desc, cc, method, grpcOpts...)
			},
		}
		return retryingStreamer, nil
	}
}

// shouldRefreshToken checks whether there's a need to refresh the token based on the error and callOptions,
// and returns a boolean value.
func (c *Client) shouldRefreshToken(err error, callOpts *options) bool {
	if errors.Is(rpctypes.Error(err), rpctypes.ErrUserEmpty) {
		// refresh the token when username, password is present but the server returns ErrUserEmpty
		// which is possible when the client token is cleared somehow
		return c.authTokenBundle != nil // equal to c.Username != "" && c.Password != ""
	}

	return callOpts.retryAuth &&
		(errors.Is(rpctypes.Error(err), rpctypes.ErrInvalidAuthToken) || errors.Is(rpctypes.Error(err), rpctypes.ErrAuthOldRevision))
}

func (c *Client) refreshToken(ctx context.Context) error {
	if c.authTokenBundle == nil {
		// c.authTokenBundle will be initialized only when
		// c.Username != "" && c.Password != "".
		//
		// When users use the TLS CommonName based authentication, the
		// authTokenBundle is always nil. But it's possible for the clients
		// to get `rpctypes.ErrAuthOldRevision` response when the clients
		// concurrently modify auth data (e.g, addUser, deleteUser etc.).
		// In this case, there is no need to refresh the token; instead the
		// clients just need to retry the operations (e.g. Put, Delete etc).
		return nil
	}

	return c.getToken(ctx)
}

// type serverStreamingRetryingStream is the implementation of grpc.ClientStream that acts as a
// proxy to the underlying call. If any of the RecvMsg() calls fail, it will try to reestablish
// a new ClientStream according to the retry policy.
type serverStreamingRetryingStream struct {
	grpc.ClientStream
	client        *Client
	bufferedSends []any // single message that the client can sen
	receivedGood  bool  // indicates whether any prior receives were successful
	wasClosedSend bool  // indicates that CloseSend was closed
	ctx           context.Context
	callOpts      *options
	streamerCall  func(ctx context.Context) (grpc.ClientStream, error)
	mu            sync.RWMutex
}

func (s *serverStreamingRetryingStream) setStream(clientStream grpc.ClientStream) {
	s.mu.Lock()
	s.ClientStream = clientStream
	s.mu.Unlock()
}

func (s *serverStreamingRetryingStream) getStream() grpc.ClientStream {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.ClientStream
}

func (s *serverStreamingRetryingStream) SendMsg(m any) error {
	s.mu.Lock()
	s.bufferedSends = append(s.bufferedSends, m)
	s.mu.Unlock()
	return s.getStream().SendMsg(m)
}

func (s *serverStreamingRetryingStream) CloseSend() error {
	s.mu.Lock()
	s.wasClosedSend = true
	s.mu.Unlock()
	return s.getStream().CloseSend()
}

func (s *serverStreamingRetryingStream) Header() (metadata.MD, error) {
	return s.getStream().Header()
}

func (s *serverStreamingRetryingStream) Trailer() metadata.MD {
	return s.getStream().Trailer()
}

func (s *serverStreamingRetryingStream) RecvMsg(m any) error {
	attemptRetry, lastErr := s.receiveMsgAndIndicateRetry(m)
	if !attemptRetry {
		return lastErr // success or hard failure
	}

	// We start off from attempt 1, because zeroth was already made on normal SendMsg().
	for attempt := uint(1); attempt < s.callOpts.max; attempt++ {
		if err := waitRetryBackoff(s.ctx, attempt, s.callOpts); err != nil {
			return err
		}
		newStream, err := s.reestablishStreamAndResendBuffer(s.ctx)
		if err != nil {
			s.client.lg.Error("failed reestablishStreamAndResendBuffer", zap.Error(err))
			return err // TODO(mwitkow): Maybe dial and transport errors should be retriable?
		}
		s.setStream(newStream)

		s.client.lg.Warn("retrying RecvMsg", zap.Error(lastErr))
		attemptRetry, lastErr = s.receiveMsgAndIndicateRetry(m)
		if !attemptRetry {
			return lastErr
		}
	}
	return lastErr
}

func (s *serverStreamingRetryingStream) receiveMsgAndIndicateRetry(m any) (bool, error) {
	s.mu.RLock()
	wasGood := s.receivedGood
	s.mu.RUnlock()
	err := s.getStream().RecvMsg(m)
	if err == nil || errors.Is(err, io.EOF) {
		s.mu.Lock()
		s.receivedGood = true
		s.mu.Unlock()
		return false, err
	} else if wasGood {
		// previous RecvMsg in the stream succeeded, no retry logic should interfere
		return false, err
	}
	if isContextError(err) {
		if s.ctx.Err() != nil {
			return false, err
		}
		// its the callCtx deadline or cancellation, in which case try again.
		return true, err
	}
	if s.client.shouldRefreshToken(err, s.callOpts) {
		gtErr := s.client.refreshToken(s.ctx)
		if gtErr != nil {
			s.client.lg.Warn("retry failed to fetch new auth token", zap.Error(gtErr))
			return false, err // return the original error for simplicity
		}
		return true, err
	}
	return isSafeRetry(s.client, err, s.callOpts), err
}

func (s *serverStreamingRetryingStream) reestablishStreamAndResendBuffer(callCtx context.Context) (grpc.ClientStream, error) {
	s.mu.RLock()
	bufferedSends := s.bufferedSends
	s.mu.RUnlock()
	newStream, err := s.streamerCall(callCtx)
	if err != nil {
		return nil, err
	}
	for _, msg := range bufferedSends {
		if err := newStream.SendMsg(msg); err != nil {
			return nil, err
		}
	}
	if err := newStream.CloseSend(); err != nil {
		return nil, err
	}
	return newStream, nil
}

func waitRetryBackoff(ctx context.Context, attempt uint, callOpts *options) error {
	waitTime := time.Duration(0)
	if attempt > 0 {
		waitTime = callOpts.backoffFunc(attempt)
	}
	if waitTime > 0 {
		timer := time.NewTimer(waitTime)
		select {
		case <-ctx.Done():
			timer.Stop()
			return contextErrToGRPCErr(ctx.Err())
		case <-timer.C:
		}
	}
	return nil
}

// isSafeRetry returns "true", if request is safe for retry with the given error.
func isSafeRetry(c *Client, err error, callOpts *options) bool {
	if isContextError(err) {
		return false
	}

	// Situation when learner refuses RPC it is supposed to not serve is from the server
	// perspective not retryable.
	// But for backward-compatibility reasons we need  to support situation that
	// customer provides mix of learners (not yet voters) and voters with an
	// expectation to pick voter in the next attempt.
	// TODO: Ideally client should be 'aware' which endpoint represents: leader/voter/learner with high probability.
	if errors.Is(err, rpctypes.ErrGRPCNotSupportedForLearner) && len(c.Endpoints()) > 1 {
		return true
	}

	switch callOpts.retryPolicy {
	case repeatable:
		return isSafeRetryImmutableRPC(err)
	case nonRepeatable:
		return isSafeRetryMutableRPC(err)
	default:
		c.lg.Warn("unrecognized retry policy", zap.String("retryPolicy", callOpts.retryPolicy.String()))
		return false
	}
}

func isContextError(err error) bool {
	return status.Code(err) == codes.DeadlineExceeded || status.Code(err) == codes.Canceled
}

func contextErrToGRPCErr(err error) error {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return status.Errorf(codes.DeadlineExceeded, err.Error())
	case errors.Is(err, context.Canceled):
		return status.Errorf(codes.Canceled, err.Error())
	default:
		return status.Errorf(codes.Unknown, err.Error())
	}
}

var defaultOptions = &options{
	retryPolicy: nonRepeatable,
	max:         0, // disable
	backoffFunc: backoffLinearWithJitter(50*time.Millisecond /*jitter*/, 0.10),
	retryAuth:   true,
}

// backoffFunc denotes a family of functions that control the backoff duration between call retries.
//
// They are called with an identifier of the attempt, and should return a time the system client should
// hold off for. If the time returned is longer than the `context.Context.Deadline` of the request
// the deadline of the request takes precedence and the wait will be interrupted before proceeding
// with the next iteration.
type backoffFunc func(attempt uint) time.Duration

// withRepeatablePolicy sets the repeatable policy of this call.
func withRepeatablePolicy() retryOption {
	return retryOption{applyFunc: func(o *options) {
		o.retryPolicy = repeatable
	}}
}

// withMax sets the maximum number of retries on this call, or this interceptor.
func withMax(maxRetries uint) retryOption {
	return retryOption{applyFunc: func(o *options) {
		o.max = maxRetries
	}}
}

// WithBackoff sets the `BackoffFunc` used to control time between retries.
func withBackoff(bf backoffFunc) retryOption {
	return retryOption{applyFunc: func(o *options) {
		o.backoffFunc = bf
	}}
}

type options struct {
	retryPolicy retryPolicy
	max         uint
	backoffFunc backoffFunc
	retryAuth   bool
}

// retryOption is a grpc.CallOption that is local to clientv3's retry interceptor.
type retryOption struct {
	grpc.EmptyCallOption // make sure we implement private after() and before() fields so we don't panic.
	applyFunc            func(opt *options)
}

func reuseOrNewWithCallOptions(opt *options, retryOptions []retryOption) *options {
	if len(retryOptions) == 0 {
		return opt
	}
	optCopy := &options{}
	*optCopy = *opt
	for _, f := range retryOptions {
		f.applyFunc(optCopy)
	}
	return optCopy
}

func filterCallOptions(callOptions []grpc.CallOption) (grpcOptions []grpc.CallOption, retryOptions []retryOption) {
	for _, opt := range callOptions {
		if co, ok := opt.(retryOption); ok {
			retryOptions = append(retryOptions, co)
		} else {
			grpcOptions = append(grpcOptions, opt)
		}
	}
	return grpcOptions, retryOptions
}

// BackoffLinearWithJitter waits a set period of time, allowing for jitter (fractional adjustment).
//
// For example waitBetween=1s and jitter=0.10 can generate waits between 900ms and 1100ms.
func backoffLinearWithJitter(waitBetween time.Duration, jitterFraction float64) backoffFunc {
	return func(attempt uint) time.Duration {
		return jitterUp(waitBetween, jitterFraction)
	}
}
