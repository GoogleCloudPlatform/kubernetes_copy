// Copyright 2017 The etcd Authors
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

package mvcc

import (
	"context"

	"go.etcd.io/etcd/pkg/v3/traceutil"
	"go.etcd.io/etcd/server/v3/lease"
)

type readView struct{ kv KV }

func (rv *readView) FirstRev() int64 {
	tr := rv.kv.Read(ConcurrentReadTxMode, traceutil.TODO())
	defer tr.End()
	return tr.FirstRev()
}

func (rv *readView) Rev() int64 {
	tr := rv.kv.Read(ConcurrentReadTxMode, traceutil.TODO())
	defer tr.End()
	return tr.Rev()
}

func (rv *readView) Range(ctx context.Context, key, end []byte, ro RangeOptions) (r *RangeResult, err error) {
	tr := rv.kv.Read(ConcurrentReadTxMode, traceutil.TODO())
	defer tr.End()
	return tr.Range(ctx, key, end, ro)
}

type writeView struct{ kv KV }

func (wv *writeView) DeleteRange(key, end []byte) (n, rev int64) {
	tw := wv.kv.Write(traceutil.TODO())
	defer tw.End()
	return tw.DeleteRange(key, end)
}

func (wv *writeView) Put(key, value []byte, lease lease.LeaseID) (rev int64) {
	tw := wv.kv.Write(traceutil.TODO())
	defer tw.End()
	return tw.Put(key, value, lease)
}
