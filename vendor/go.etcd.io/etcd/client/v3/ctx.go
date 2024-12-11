// Copyright 2020 The etcd Authors
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

package clientv3

import (
	"context"

	"google.golang.org/grpc/metadata"

	"go.etcd.io/etcd/api/v3/v3rpc/rpctypes"
	"go.etcd.io/etcd/api/v3/version"
)

// WithRequireLeader requires client requests to only succeed
// when the cluster has a leader.
func WithRequireLeader(ctx context.Context) context.Context {
	md, ok := metadata.FromOutgoingContext(ctx)
	if !ok { // no outgoing metadata ctx key, create one
		md = metadata.Pairs(rpctypes.MetadataRequireLeaderKey, rpctypes.MetadataHasLeader)
		return metadata.NewOutgoingContext(ctx, md)
	}
	copied := md.Copy() // avoid racey updates
	// overwrite/add 'hasleader' key/value
	copied.Set(rpctypes.MetadataRequireLeaderKey, rpctypes.MetadataHasLeader)
	return metadata.NewOutgoingContext(ctx, copied)
}

// embeds client version
func withVersion(ctx context.Context) context.Context {
	md, ok := metadata.FromOutgoingContext(ctx)
	if !ok { // no outgoing metadata ctx key, create one
		md = metadata.Pairs(rpctypes.MetadataClientAPIVersionKey, version.APIVersion)
		return metadata.NewOutgoingContext(ctx, md)
	}
	copied := md.Copy() // avoid racey updates
	// overwrite/add version key/value
	copied.Set(rpctypes.MetadataClientAPIVersionKey, version.APIVersion)
	return metadata.NewOutgoingContext(ctx, copied)
}
