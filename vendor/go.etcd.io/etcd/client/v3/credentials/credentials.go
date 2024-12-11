// Copyright 2019 The etcd Authors
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

// Package credentials implements gRPC credential interface with etcd specific logic.
// e.g., client handshake with custom authority parameter
package credentials

import (
	"context"
	"crypto/tls"
	"sync"

	grpccredentials "google.golang.org/grpc/credentials"

	"go.etcd.io/etcd/api/v3/v3rpc/rpctypes"
)

func NewTransportCredential(cfg *tls.Config) grpccredentials.TransportCredentials {
	return grpccredentials.NewTLS(cfg)
}

// PerRPCCredentialsBundle defines gRPC credential interface.
type PerRPCCredentialsBundle interface {
	UpdateAuthToken(token string)
	PerRPCCredentials() grpccredentials.PerRPCCredentials
}

func NewPerRPCCredentialBundle() PerRPCCredentialsBundle {
	return &perRPCCredentialBundle{
		rc: &perRPCCredential{},
	}
}

// perRPCCredentialBundle implements `PerRPCCredentialsBundle` interface.
type perRPCCredentialBundle struct {
	rc *perRPCCredential
}

func (b *perRPCCredentialBundle) UpdateAuthToken(token string) {
	if b.rc == nil {
		return
	}
	b.rc.UpdateAuthToken(token)
}

func (b *perRPCCredentialBundle) PerRPCCredentials() grpccredentials.PerRPCCredentials {
	return b.rc
}

// perRPCCredential implements `grpccredentials.PerRPCCredentials` interface.
type perRPCCredential struct {
	authToken   string
	authTokenMu sync.RWMutex
}

func (rc *perRPCCredential) RequireTransportSecurity() bool { return false }

func (rc *perRPCCredential) GetRequestMetadata(ctx context.Context, s ...string) (map[string]string, error) {
	rc.authTokenMu.RLock()
	authToken := rc.authToken
	rc.authTokenMu.RUnlock()
	if authToken == "" {
		return nil, nil
	}
	return map[string]string{rpctypes.TokenFieldNameGRPC: authToken}, nil
}

func (rc *perRPCCredential) UpdateAuthToken(token string) {
	rc.authTokenMu.Lock()
	rc.authToken = token
	rc.authTokenMu.Unlock()
}
