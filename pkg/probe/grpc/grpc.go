/*
Copyright 2021 The Kubernetes Authors.

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
	"errors"
	"fmt"
	"net"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	grpchealth "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"k8s.io/component-base/version"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/probe"
)

// Prober is an interface that defines the Probe function for doing GRPC readiness/liveness/startup checks.
type Prober interface {
	Probe(host, service string, port int, timeout time.Duration) (probe.Result, string, error)
}

type grpcProber struct {
}

// New Prober for execute grpc probe
func New() Prober {
	return grpcProber{}
}

// Probe executes a grpc call to check the liveness/readiness/startup of container.
// Returns the Result status, command output, and errors if any.
// Any failure is considered as a probe failure to mimic grpc_health_probe tool behavior.
// err is always nil
func (p grpcProber) Probe(host, service string, port int, timeout time.Duration) (probe.Result, string, error) {
	v := version.Get()

	opts := []grpc.DialOption{
		grpc.WithUserAgent(fmt.Sprintf("kube-probe/%s.%s", v.Major, v.Minor)),
		grpc.WithTransportCredentials(insecure.NewCredentials()), //credentials are currently not supported
		grpc.WithContextDialer(func(ctx context.Context, addr string) (net.Conn, error) {
			return probe.ProbeDialer().DialContext(ctx, "tcp", addr)
		}),
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)

	defer cancel()

	addr := net.JoinHostPort(host, fmt.Sprintf("%d", port))
	conn, err := grpc.NewClient(addr, opts...)

	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			klog.V(4).ErrorS(err, "failed to connect grpc service due to timeout", "addr", addr, "service", service, "timeout", timeout)
			return probe.Failure, fmt.Sprintf("timeout: failed to connect service %q within %v: %+v", addr, timeout, err), nil
		} else {
			klog.V(4).ErrorS(err, "failed to connect grpc service", "service", addr)
			return probe.Failure, fmt.Sprintf("error: failed to connect service at %q: %+v", addr, err), nil
		}
	}

	defer func() {
		_ = conn.Close()
	}()

	client := grpchealth.NewHealthClient(conn)

	resp, err := client.Check(metadata.NewOutgoingContext(ctx, make(metadata.MD)), &grpchealth.HealthCheckRequest{
		Service: service,
	})

	if err != nil {
		stat, ok := status.FromError(err)
		if ok {
			switch stat.Code() {
			case codes.Unimplemented:
				klog.V(4).ErrorS(err, "server does not implement the grpc health protocol (grpc.health.v1.Health)", "addr", addr, "service", service)
				return probe.Failure, fmt.Sprintf("error: this server does not implement the grpc health protocol (grpc.health.v1.Health): %s", stat.Message()), nil
			case codes.DeadlineExceeded:
				klog.V(4).ErrorS(err, "rpc request not finished within timeout", "addr", addr, "service", service, "timeout", timeout)
				return probe.Failure, fmt.Sprintf("timeout: health rpc did not complete within %v", timeout), nil
			default:
				klog.V(4).ErrorS(err, "rpc probe failed")
			}
		} else {
			klog.V(4).ErrorS(err, "health rpc probe failed")
		}

		return probe.Failure, fmt.Sprintf("error: health rpc probe failed: %+v", err), nil
	}

	if resp.GetStatus() != grpchealth.HealthCheckResponse_SERVING {
		return probe.Failure, fmt.Sprintf("service unhealthy (responded with %q)", resp.GetStatus().String()), nil
	}

	return probe.Success, fmt.Sprintf("service healthy"), nil
}
