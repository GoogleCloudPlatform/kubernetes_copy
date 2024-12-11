// Copyright 2015 The etcd Authors
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

	"google.golang.org/grpc"

	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/v3rpc/rpctypes"
)

type (
	CompactResponse pb.CompactionResponse
	PutResponse     pb.PutResponse
	GetResponse     pb.RangeResponse
	DeleteResponse  pb.DeleteRangeResponse
	TxnResponse     pb.TxnResponse
)

type KV interface {
	// Put puts a key-value pair into etcd.
	// Note that key,value can be plain bytes array and string is
	// an immutable representation of that bytes array.
	// To get a string of bytes, do string([]byte{0x10, 0x20}).
	Put(ctx context.Context, key, val string, opts ...OpOption) (*PutResponse, error)

	// Get retrieves keys.
	// By default, Get will return the value for "key", if any.
	// When passed WithRange(end), Get will return the keys in the range [key, end).
	// When passed WithFromKey(), Get returns keys greater than or equal to key.
	// When passed WithRev(rev) with rev > 0, Get retrieves keys at the given revision;
	// if the required revision is compacted, the request will fail with ErrCompacted .
	// When passed WithLimit(limit), the number of returned keys is bounded by limit.
	// When passed WithSort(), the keys will be sorted.
	Get(ctx context.Context, key string, opts ...OpOption) (*GetResponse, error)

	// Delete deletes a key, or optionally using WithRange(end), [key, end).
	Delete(ctx context.Context, key string, opts ...OpOption) (*DeleteResponse, error)

	// Compact compacts etcd KV history before the given rev.
	Compact(ctx context.Context, rev int64, opts ...CompactOption) (*CompactResponse, error)

	// Do applies a single Op on KV without a transaction.
	// Do is useful when creating arbitrary operations to be issued at a
	// later time; the user can range over the operations, calling Do to
	// execute them. Get/Put/Delete, on the other hand, are best suited
	// for when the operation should be issued at the time of declaration.
	Do(ctx context.Context, op Op) (OpResponse, error)

	// Txn creates a transaction.
	Txn(ctx context.Context) Txn
}

type OpResponse struct {
	put *PutResponse
	get *GetResponse
	del *DeleteResponse
	txn *TxnResponse
}

func (op OpResponse) Put() *PutResponse    { return op.put }
func (op OpResponse) Get() *GetResponse    { return op.get }
func (op OpResponse) Del() *DeleteResponse { return op.del }
func (op OpResponse) Txn() *TxnResponse    { return op.txn }

func (resp *PutResponse) OpResponse() OpResponse {
	return OpResponse{put: resp}
}

func (resp *GetResponse) OpResponse() OpResponse {
	return OpResponse{get: resp}
}

func (resp *DeleteResponse) OpResponse() OpResponse {
	return OpResponse{del: resp}
}

func (resp *TxnResponse) OpResponse() OpResponse {
	return OpResponse{txn: resp}
}

type kv struct {
	remote   pb.KVClient
	callOpts []grpc.CallOption
}

func NewKV(c *Client) KV {
	api := &kv{remote: RetryKVClient(c)}
	if c != nil {
		api.callOpts = c.callOpts
	}
	return api
}

func NewKVFromKVClient(remote pb.KVClient, c *Client) KV {
	api := &kv{remote: remote}
	if c != nil {
		api.callOpts = c.callOpts
	}
	return api
}

func (kv *kv) Put(ctx context.Context, key, val string, opts ...OpOption) (*PutResponse, error) {
	r, err := kv.Do(ctx, OpPut(key, val, opts...))
	return r.put, ContextError(ctx, err)
}

func (kv *kv) Get(ctx context.Context, key string, opts ...OpOption) (*GetResponse, error) {
	r, err := kv.Do(ctx, OpGet(key, opts...))
	return r.get, ContextError(ctx, err)
}

func (kv *kv) Delete(ctx context.Context, key string, opts ...OpOption) (*DeleteResponse, error) {
	r, err := kv.Do(ctx, OpDelete(key, opts...))
	return r.del, ContextError(ctx, err)
}

func (kv *kv) Compact(ctx context.Context, rev int64, opts ...CompactOption) (*CompactResponse, error) {
	resp, err := kv.remote.Compact(ctx, OpCompact(rev, opts...).toRequest(), kv.callOpts...)
	if err != nil {
		return nil, ContextError(ctx, err)
	}
	return (*CompactResponse)(resp), err
}

func (kv *kv) Txn(ctx context.Context) Txn {
	return &txn{
		kv:       kv,
		ctx:      ctx,
		callOpts: kv.callOpts,
	}
}

func (kv *kv) Do(ctx context.Context, op Op) (OpResponse, error) {
	var err error
	switch op.t {
	case tRange:
		if op.IsSortOptionValid() {
			var resp *pb.RangeResponse
			resp, err = kv.remote.Range(ctx, op.toRangeRequest(), kv.callOpts...)
			if err == nil {
				return OpResponse{get: (*GetResponse)(resp)}, nil
			}
		} else {
			err = rpctypes.ErrInvalidSortOption
		}
	case tPut:
		var resp *pb.PutResponse
		r := &pb.PutRequest{Key: op.key, Value: op.val, Lease: int64(op.leaseID), PrevKv: op.prevKV, IgnoreValue: op.ignoreValue, IgnoreLease: op.ignoreLease}
		resp, err = kv.remote.Put(ctx, r, kv.callOpts...)
		if err == nil {
			return OpResponse{put: (*PutResponse)(resp)}, nil
		}
	case tDeleteRange:
		var resp *pb.DeleteRangeResponse
		r := &pb.DeleteRangeRequest{Key: op.key, RangeEnd: op.end, PrevKv: op.prevKV}
		resp, err = kv.remote.DeleteRange(ctx, r, kv.callOpts...)
		if err == nil {
			return OpResponse{del: (*DeleteResponse)(resp)}, nil
		}
	case tTxn:
		var resp *pb.TxnResponse
		resp, err = kv.remote.Txn(ctx, op.toTxnRequest(), kv.callOpts...)
		if err == nil {
			return OpResponse{txn: (*TxnResponse)(resp)}, nil
		}
	default:
		panic("Unknown op")
	}
	return OpResponse{}, ContextError(ctx, err)
}
