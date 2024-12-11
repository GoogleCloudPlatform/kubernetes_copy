// Copyright 2023 The etcd Authors
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

package v3rpc

import (
	"go.uber.org/zap"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	"go.etcd.io/etcd/server/v3/etcdserver"
	"go.etcd.io/etcd/server/v3/features"
)

const (
	allGRPCServices = ""
)

type notifier interface {
	defragStarted()
	defragFinished()
}

func newHealthNotifier(hs *health.Server, s *etcdserver.EtcdServer) notifier {
	if hs == nil {
		panic("unexpected nil gRPC health server")
	}
	hc := &healthNotifier{hs: hs, lg: s.Logger(), stopGRPCServiceOnDefrag: s.FeatureEnabled(features.StopGRPCServiceOnDefrag)}
	// set grpc health server as serving status blindly since
	// the grpc server will serve iff s.ReadyNotify() is closed.
	hc.startServe()
	return hc
}

type healthNotifier struct {
	hs *health.Server
	lg *zap.Logger

	stopGRPCServiceOnDefrag bool
}

func (hc *healthNotifier) defragStarted() {
	if !hc.stopGRPCServiceOnDefrag {
		return
	}
	hc.stopServe("defrag is active")
}

func (hc *healthNotifier) defragFinished() { hc.startServe() }

func (hc *healthNotifier) startServe() {
	hc.lg.Info(
		"grpc service status changed",
		zap.String("service", allGRPCServices),
		zap.String("status", healthpb.HealthCheckResponse_SERVING.String()),
	)
	hc.hs.SetServingStatus(allGRPCServices, healthpb.HealthCheckResponse_SERVING)
}

func (hc *healthNotifier) stopServe(reason string) {
	hc.lg.Warn(
		"grpc service status changed",
		zap.String("service", allGRPCServices),
		zap.String("status", healthpb.HealthCheckResponse_NOT_SERVING.String()),
		zap.String("reason", reason),
	)
	hc.hs.SetServingStatus(allGRPCServices, healthpb.HealthCheckResponse_NOT_SERVING)
}
