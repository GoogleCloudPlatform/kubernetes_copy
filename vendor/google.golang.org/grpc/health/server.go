/*
 *
 * Copyright 2017 gRPC authors.
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

// Package health provides a service that exposes server's health and it must be
// imported to enable support for client-side health checks.
package health

import (
	"context"
	"sync"

	"google.golang.org/grpc/codes"
	healthgrpc "google.golang.org/grpc/health/grpc_health_v1"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
)

// Server implements `service Health`.
type Server struct {
	healthgrpc.UnimplementedHealthServer
	mu sync.RWMutex
	// If shutdown is true, it's expected all serving status is NOT_SERVING, and
	// will stay in NOT_SERVING.
	shutdown bool
	// statusMap stores the serving status of the services this Server monitors.
	statusMap map[string]healthpb.HealthCheckResponse_ServingStatus
	updates   map[string]map[healthgrpc.Health_WatchServer]chan healthpb.HealthCheckResponse_ServingStatus
}

// NewServer returns a new Server.
func NewServer() *Server {
	return &Server{
		statusMap: map[string]healthpb.HealthCheckResponse_ServingStatus{"": healthpb.HealthCheckResponse_SERVING},
		updates:   make(map[string]map[healthgrpc.Health_WatchServer]chan healthpb.HealthCheckResponse_ServingStatus),
	}
}

// Check implements `service Health`.
func (s *Server) Check(_ context.Context, in *healthpb.HealthCheckRequest) (*healthpb.HealthCheckResponse, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if servingStatus, ok := s.statusMap[in.Service]; ok {
		return &healthpb.HealthCheckResponse{
			Status: servingStatus,
		}, nil
	}
	return nil, status.Error(codes.NotFound, "unknown service")
}

// Watch implements `service Health`.
func (s *Server) Watch(in *healthpb.HealthCheckRequest, stream healthgrpc.Health_WatchServer) error {
	service := in.Service
	// update channel is used for getting service status updates.
	update := make(chan healthpb.HealthCheckResponse_ServingStatus, 1)
	s.mu.Lock()
	// Puts the initial status to the channel.
	if servingStatus, ok := s.statusMap[service]; ok {
		update <- servingStatus
	} else {
		update <- healthpb.HealthCheckResponse_SERVICE_UNKNOWN
	}

	// Registers the update channel to the correct place in the updates map.
	if _, ok := s.updates[service]; !ok {
		s.updates[service] = make(map[healthgrpc.Health_WatchServer]chan healthpb.HealthCheckResponse_ServingStatus)
	}
	s.updates[service][stream] = update
	defer func() {
		s.mu.Lock()
		delete(s.updates[service], stream)
		s.mu.Unlock()
	}()
	s.mu.Unlock()

	var lastSentStatus healthpb.HealthCheckResponse_ServingStatus = -1
	for {
		select {
		// Status updated. Sends the up-to-date status to the client.
		case servingStatus := <-update:
			if lastSentStatus == servingStatus {
				continue
			}
			lastSentStatus = servingStatus
			err := stream.Send(&healthpb.HealthCheckResponse{Status: servingStatus})
			if err != nil {
				return status.Error(codes.Canceled, "Stream has ended.")
			}
		// Context done. Removes the update channel from the updates map.
		case <-stream.Context().Done():
			return status.Error(codes.Canceled, "Stream has ended.")
		}
	}
}

// SetServingStatus is called when need to reset the serving status of a service
// or insert a new service entry into the statusMap.
func (s *Server) SetServingStatus(service string, servingStatus healthpb.HealthCheckResponse_ServingStatus) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.shutdown {
		logger.Infof("health: status changing for %s to %v is ignored because health service is shutdown", service, servingStatus)
		return
	}

	s.setServingStatusLocked(service, servingStatus)
}

func (s *Server) setServingStatusLocked(service string, servingStatus healthpb.HealthCheckResponse_ServingStatus) {
	s.statusMap[service] = servingStatus
	for _, update := range s.updates[service] {
		// Clears previous updates, that are not sent to the client, from the channel.
		// This can happen if the client is not reading and the server gets flow control limited.
		select {
		case <-update:
		default:
		}
		// Puts the most recent update to the channel.
		update <- servingStatus
	}
}

// Shutdown sets all serving status to NOT_SERVING, and configures the server to
// ignore all future status changes.
//
// This changes serving status for all services. To set status for a particular
// services, call SetServingStatus().
func (s *Server) Shutdown() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.shutdown = true
	for service := range s.statusMap {
		s.setServingStatusLocked(service, healthpb.HealthCheckResponse_NOT_SERVING)
	}
}

// Resume sets all serving status to SERVING, and configures the server to
// accept all future status changes.
//
// This changes serving status for all services. To set status for a particular
// services, call SetServingStatus().
func (s *Server) Resume() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.shutdown = false
	for service := range s.statusMap {
		s.setServingStatusLocked(service, healthpb.HealthCheckResponse_SERVING)
	}
}
