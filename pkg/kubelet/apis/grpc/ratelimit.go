/*
Copyright 2023 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package grpc

import (
	"context"

	gotimerate "golang.org/x/time/rate"
	"k8s.io/klog/v2"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var (
	ErrorLimitExceeded = status.Error(codes.ResourceExhausted, "rejected by rate limit")
)

// Limiter defines the interface to perform request rate limiting,
// based on the interface exposed by https://pkg.go.dev/golang.org/x/time/rate#Limiter
type Limiter interface {
	// Allow reports whether an event may happen now.
	Allow() bool
}

// LimiterUnaryServerInterceptor returns a new unary server interceptors that performs request rate limiting.
func LimiterUnaryServerInterceptor(limiter Limiter) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		if !limiter.Allow() {
			return nil, ErrorLimitExceeded
		}
		return handler(ctx, req)
	}
}

// WithRateLimiter creates new rate limiter with unary interceptor.
func WithRateLimiter(ctx context.Context, serviceName string, qps, burstTokens int32) grpc.ServerOption {
	logger := klog.FromContext(ctx)
	qpsVal := gotimerate.Limit(qps)
	burstVal := int(burstTokens)
	logger.Info("Setting rate limiting for endpoint", "service", serviceName, "qps", qpsVal, "burstTokens", burstVal)
	return grpc.UnaryInterceptor(LimiterUnaryServerInterceptor(gotimerate.NewLimiter(qpsVal, burstVal)))
}
