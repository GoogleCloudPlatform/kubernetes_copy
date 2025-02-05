/*
Copyright 2022 The Kubernetes Authors.

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

package etcd3

import (
	"context"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
	endpointsrequest "k8s.io/apiserver/pkg/endpoints/request"
)

// NewETCDLatencyTracker returns an implementation of
// clientv3.KV that times the calls from the specified
// 'delegate' KV instance in order to track latency incurred.
func NewETCDLatencyTracker(delegate clientv3.KV) clientv3.KV {
	return &clientV3KVLatencyTracker{KV: delegate}
}

// clientV3KVLatencyTracker decorates a clientv3.KV instance and times
// each call so we can track the latency an API request incurs in etcd
// round trips (the time it takes to send data to etcd and get the
// complete response back)
//
// If an API request involves N (N>=1) round trips to etcd, then we will sum
// up the latency incurred in each roundtrip.

// It uses the context associated with the request in flight, so there
// are no states shared among the requests in flight, and so there is no
// concurrency overhead.
// If the goroutine executing the request handler makes concurrent calls
// to the underlying storage layer, that is protected since the latency
// tracking function TrackStorageLatency is thread safe.
//
// NOTE: Compact is an asynchronous process and is not associated with
// any request, so we will not be tracking its latency.
type clientV3KVLatencyTracker struct {
	clientv3.KV
}

func (c *clientV3KVLatencyTracker) Put(ctx context.Context, key, val string, opts ...clientv3.OpOption) (*clientv3.PutResponse, error) {
	startedAt := time.Now()
	defer func() {
		endpointsrequest.TrackStorageLatency(ctx, time.Since(startedAt))
	}()

	return c.KV.Put(ctx, key, val, opts...)
}

func (c *clientV3KVLatencyTracker) Get(ctx context.Context, key string, opts ...clientv3.OpOption) (*clientv3.GetResponse, error) {
	startedAt := time.Now()
	defer func() {
		endpointsrequest.TrackStorageLatency(ctx, time.Since(startedAt))
	}()

	return c.KV.Get(ctx, key, opts...)
}

func (c *clientV3KVLatencyTracker) Delete(ctx context.Context, key string, opts ...clientv3.OpOption) (*clientv3.DeleteResponse, error) {
	startedAt := time.Now()
	defer func() {
		endpointsrequest.TrackStorageLatency(ctx, time.Since(startedAt))
	}()

	return c.KV.Delete(ctx, key, opts...)
}

func (c *clientV3KVLatencyTracker) Do(ctx context.Context, op clientv3.Op) (clientv3.OpResponse, error) {
	startedAt := time.Now()
	defer func() {
		endpointsrequest.TrackStorageLatency(ctx, time.Since(startedAt))
	}()

	return c.KV.Do(ctx, op)
}

func (c *clientV3KVLatencyTracker) Txn(ctx context.Context) clientv3.Txn {
	return &clientV3TxnTracker{ctx: ctx, Txn: c.KV.Txn(ctx)}
}

type clientV3TxnTracker struct {
	ctx context.Context
	clientv3.Txn
}

func (t *clientV3TxnTracker) Commit() (*clientv3.TxnResponse, error) {
	startedAt := time.Now()
	defer func() {
		endpointsrequest.TrackStorageLatency(t.ctx, time.Since(startedAt))
	}()

	return t.Txn.Commit()
}
