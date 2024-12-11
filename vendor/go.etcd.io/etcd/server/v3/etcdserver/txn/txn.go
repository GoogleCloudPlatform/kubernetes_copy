// Copyright 2022 The etcd Authors
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

package txn

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"time"

	"go.uber.org/zap"

	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	"go.etcd.io/etcd/pkg/v3/traceutil"
	"go.etcd.io/etcd/server/v3/auth"
	"go.etcd.io/etcd/server/v3/etcdserver/errors"
	"go.etcd.io/etcd/server/v3/lease"
	"go.etcd.io/etcd/server/v3/storage/mvcc"
)

func Put(ctx context.Context, lg *zap.Logger, lessor lease.Lessor, kv mvcc.KV, p *pb.PutRequest) (resp *pb.PutResponse, trace *traceutil.Trace, err error) {
	trace = traceutil.Get(ctx)
	// create put tracing if the trace in context is empty
	if trace.IsEmpty() {
		trace = traceutil.New("put",
			lg,
			traceutil.Field{Key: "key", Value: string(p.Key)},
			traceutil.Field{Key: "req_size", Value: p.Size()},
		)
		ctx = context.WithValue(ctx, traceutil.TraceKey{}, trace)
	}
	leaseID := lease.LeaseID(p.Lease)
	if leaseID != lease.NoLease {
		if l := lessor.Lookup(leaseID); l == nil {
			return nil, nil, lease.ErrLeaseNotFound
		}
	}
	txnWrite := kv.Write(trace)
	defer txnWrite.End()
	resp, err = put(ctx, txnWrite, p)
	return resp, trace, err
}

func put(ctx context.Context, txnWrite mvcc.TxnWrite, p *pb.PutRequest) (resp *pb.PutResponse, err error) {
	trace := traceutil.Get(ctx)
	resp = &pb.PutResponse{}
	resp.Header = &pb.ResponseHeader{}
	val, leaseID := p.Value, lease.LeaseID(p.Lease)

	var rr *mvcc.RangeResult
	if p.IgnoreValue || p.IgnoreLease || p.PrevKv {
		trace.StepWithFunction(func() {
			rr, err = txnWrite.Range(context.TODO(), p.Key, nil, mvcc.RangeOptions{})
		}, "get previous kv pair")

		if err != nil {
			return nil, err
		}
	}
	if p.IgnoreValue || p.IgnoreLease {
		if rr == nil || len(rr.KVs) == 0 {
			// ignore_{lease,value} flag expects previous key-value pair
			return nil, errors.ErrKeyNotFound
		}
	}
	if p.IgnoreValue {
		val = rr.KVs[0].Value
	}
	if p.IgnoreLease {
		leaseID = lease.LeaseID(rr.KVs[0].Lease)
	}
	if p.PrevKv {
		if rr != nil && len(rr.KVs) != 0 {
			resp.PrevKv = &rr.KVs[0]
		}
	}

	resp.Header.Revision = txnWrite.Put(p.Key, val, leaseID)
	trace.AddField(traceutil.Field{Key: "response_revision", Value: resp.Header.Revision})
	return resp, nil
}

func DeleteRange(ctx context.Context, lg *zap.Logger, kv mvcc.KV, dr *pb.DeleteRangeRequest) (resp *pb.DeleteRangeResponse, trace *traceutil.Trace, err error) {
	trace = traceutil.Get(ctx)
	// create delete tracing if the trace in context is empty
	if trace.IsEmpty() {
		trace = traceutil.New("delete_range",
			lg,
			traceutil.Field{Key: "key", Value: string(dr.Key)},
			traceutil.Field{Key: "range_end", Value: string(dr.RangeEnd)},
		)
		ctx = context.WithValue(ctx, traceutil.TraceKey{}, trace)
	}
	txnWrite := kv.Write(trace)
	defer txnWrite.End()
	resp, err = deleteRange(ctx, txnWrite, dr)
	return resp, trace, err
}

func deleteRange(ctx context.Context, txnWrite mvcc.TxnWrite, dr *pb.DeleteRangeRequest) (*pb.DeleteRangeResponse, error) {
	resp := &pb.DeleteRangeResponse{}
	resp.Header = &pb.ResponseHeader{}
	end := mkGteRange(dr.RangeEnd)

	if dr.PrevKv {
		rr, err := txnWrite.Range(ctx, dr.Key, end, mvcc.RangeOptions{})
		if err != nil {
			return nil, err
		}
		if rr != nil {
			resp.PrevKvs = make([]*mvccpb.KeyValue, len(rr.KVs))
			for i := range rr.KVs {
				resp.PrevKvs[i] = &rr.KVs[i]
			}
		}
	}

	resp.Deleted, resp.Header.Revision = txnWrite.DeleteRange(dr.Key, end)
	return resp, nil
}

func Range(ctx context.Context, lg *zap.Logger, kv mvcc.KV, r *pb.RangeRequest) (resp *pb.RangeResponse, trace *traceutil.Trace, err error) {
	trace = traceutil.Get(ctx)
	if trace.IsEmpty() {
		trace = traceutil.New("range", lg)
		ctx = context.WithValue(ctx, traceutil.TraceKey{}, trace)
	}
	defer func(start time.Time) {
		success := err == nil
		RangeSecObserve(success, time.Since(start))
	}(time.Now())
	txnRead := kv.Read(mvcc.ConcurrentReadTxMode, trace)
	defer txnRead.End()
	resp, err = executeRange(ctx, lg, txnRead, r)
	return resp, trace, err
}

func executeRange(ctx context.Context, lg *zap.Logger, txnRead mvcc.TxnRead, r *pb.RangeRequest) (*pb.RangeResponse, error) {
	trace := traceutil.Get(ctx)

	resp := &pb.RangeResponse{}
	resp.Header = &pb.ResponseHeader{}

	limit := r.Limit
	if r.SortOrder != pb.RangeRequest_NONE ||
		r.MinModRevision != 0 || r.MaxModRevision != 0 ||
		r.MinCreateRevision != 0 || r.MaxCreateRevision != 0 {
		// fetch everything; sort and truncate afterwards
		limit = 0
	}
	if limit > 0 {
		// fetch one extra for 'more' flag
		limit = limit + 1
	}

	ro := mvcc.RangeOptions{
		Limit: limit,
		Rev:   r.Revision,
		Count: r.CountOnly,
	}

	rr, err := txnRead.Range(ctx, r.Key, mkGteRange(r.RangeEnd), ro)
	if err != nil {
		return nil, err
	}

	if r.MaxModRevision != 0 {
		f := func(kv *mvccpb.KeyValue) bool { return kv.ModRevision > r.MaxModRevision }
		pruneKVs(rr, f)
	}
	if r.MinModRevision != 0 {
		f := func(kv *mvccpb.KeyValue) bool { return kv.ModRevision < r.MinModRevision }
		pruneKVs(rr, f)
	}
	if r.MaxCreateRevision != 0 {
		f := func(kv *mvccpb.KeyValue) bool { return kv.CreateRevision > r.MaxCreateRevision }
		pruneKVs(rr, f)
	}
	if r.MinCreateRevision != 0 {
		f := func(kv *mvccpb.KeyValue) bool { return kv.CreateRevision < r.MinCreateRevision }
		pruneKVs(rr, f)
	}

	sortOrder := r.SortOrder
	if r.SortTarget != pb.RangeRequest_KEY && sortOrder == pb.RangeRequest_NONE {
		// Since current mvcc.Range implementation returns results
		// sorted by keys in lexiographically ascending order,
		// sort ASCEND by default only when target is not 'KEY'
		sortOrder = pb.RangeRequest_ASCEND
	} else if r.SortTarget == pb.RangeRequest_KEY && sortOrder == pb.RangeRequest_ASCEND {
		// Since current mvcc.Range implementation returns results
		// sorted by keys in lexiographically ascending order,
		// don't re-sort when target is 'KEY' and order is ASCEND
		sortOrder = pb.RangeRequest_NONE
	}
	if sortOrder != pb.RangeRequest_NONE {
		var sorter sort.Interface
		switch {
		case r.SortTarget == pb.RangeRequest_KEY:
			sorter = &kvSortByKey{&kvSort{rr.KVs}}
		case r.SortTarget == pb.RangeRequest_VERSION:
			sorter = &kvSortByVersion{&kvSort{rr.KVs}}
		case r.SortTarget == pb.RangeRequest_CREATE:
			sorter = &kvSortByCreate{&kvSort{rr.KVs}}
		case r.SortTarget == pb.RangeRequest_MOD:
			sorter = &kvSortByMod{&kvSort{rr.KVs}}
		case r.SortTarget == pb.RangeRequest_VALUE:
			sorter = &kvSortByValue{&kvSort{rr.KVs}}
		default:
			lg.Panic("unexpected sort target", zap.Int32("sort-target", int32(r.SortTarget)))
		}
		switch {
		case sortOrder == pb.RangeRequest_ASCEND:
			sort.Sort(sorter)
		case sortOrder == pb.RangeRequest_DESCEND:
			sort.Sort(sort.Reverse(sorter))
		}
	}

	if r.Limit > 0 && len(rr.KVs) > int(r.Limit) {
		rr.KVs = rr.KVs[:r.Limit]
		resp.More = true
	}
	trace.Step("filter and sort the key-value pairs")
	resp.Header.Revision = rr.Rev
	resp.Count = int64(rr.Count)
	resp.Kvs = make([]*mvccpb.KeyValue, len(rr.KVs))
	for i := range rr.KVs {
		if r.KeysOnly {
			rr.KVs[i].Value = nil
		}
		resp.Kvs[i] = &rr.KVs[i]
	}
	trace.Step("assemble the response")
	return resp, nil
}

func Txn(ctx context.Context, lg *zap.Logger, rt *pb.TxnRequest, txnModeWriteWithSharedBuffer bool, kv mvcc.KV, lessor lease.Lessor) (*pb.TxnResponse, *traceutil.Trace, error) {
	trace := traceutil.Get(ctx)
	if trace.IsEmpty() {
		trace = traceutil.New("transaction", lg)
		ctx = context.WithValue(ctx, traceutil.TraceKey{}, trace)
	}
	isWrite := !IsTxnReadonly(rt)
	// When the transaction contains write operations, we use ReadTx instead of
	// ConcurrentReadTx to avoid extra overhead of copying buffer.
	var mode mvcc.ReadTxMode
	if isWrite && txnModeWriteWithSharedBuffer /*a.s.Cfg.ExperimentalTxnModeWriteWithSharedBuffer*/ {
		mode = mvcc.SharedBufReadTxMode
	} else {
		mode = mvcc.ConcurrentReadTxMode
	}
	txnRead := kv.Read(mode, trace)
	var txnPath []bool
	trace.StepWithFunction(
		func() {
			txnPath = compareToPath(txnRead, rt)
		},
		"compare",
	)
	if isWrite {
		trace.AddField(traceutil.Field{Key: "read_only", Value: false})
	}
	_, err := checkTxn(txnRead, rt, lessor, txnPath)
	if err != nil {
		txnRead.End()
		return nil, nil, err
	}
	trace.Step("check requests")
	// When executing mutable txnWrite ops, etcd must hold the txnWrite lock so
	// readers do not see any intermediate results. Since writes are
	// serialized on the raft loop, the revision in the read view will
	// be the revision of the write txnWrite.
	var txnWrite mvcc.TxnWrite
	if isWrite {
		txnRead.End()
		txnWrite = kv.Write(trace)
	} else {
		txnWrite = mvcc.NewReadOnlyTxnWrite(txnRead)
	}
	txnResp, err := txn(ctx, lg, txnWrite, rt, isWrite, txnPath)
	txnWrite.End()

	trace.AddField(
		traceutil.Field{Key: "number_of_response", Value: len(txnResp.Responses)},
		traceutil.Field{Key: "response_revision", Value: txnResp.Header.Revision},
	)
	return txnResp, trace, err
}

func txn(ctx context.Context, lg *zap.Logger, txnWrite mvcc.TxnWrite, rt *pb.TxnRequest, isWrite bool, txnPath []bool) (*pb.TxnResponse, error) {
	txnResp, _ := newTxnResp(rt, txnPath)
	_, err := executeTxn(ctx, lg, txnWrite, rt, txnPath, txnResp)
	if err != nil {
		if isWrite {
			// CAUTION: When a txn performing write operations starts, we always expect it to be successful.
			// If a write failure is seen we SHOULD NOT try to recover the server, but crash with a panic to make the failure explicit.
			// Trying to silently recover (e.g by ignoring the failed txn or calling txn.End() early) poses serious risks:
			// - violation of transaction atomicity if some write operations have been partially executed
			// - data inconsistency across different etcd members if they applied the txn asymmetrically
			lg.Panic("unexpected error during txn with writes", zap.Error(err))
		} else {
			lg.Error("unexpected error during readonly txn", zap.Error(err))
		}
	}
	rev := txnWrite.Rev()
	if len(txnWrite.Changes()) != 0 {
		rev++
	}
	txnResp.Header.Revision = rev
	return txnResp, err
}

// newTxnResp allocates a txn response for a txn request given a path.
func newTxnResp(rt *pb.TxnRequest, txnPath []bool) (txnResp *pb.TxnResponse, txnCount int) {
	reqs := rt.Success
	if !txnPath[0] {
		reqs = rt.Failure
	}
	resps := make([]*pb.ResponseOp, len(reqs))
	txnResp = &pb.TxnResponse{
		Responses: resps,
		Succeeded: txnPath[0],
		Header:    &pb.ResponseHeader{},
	}
	for i, req := range reqs {
		switch tv := req.Request.(type) {
		case *pb.RequestOp_RequestRange:
			resps[i] = &pb.ResponseOp{Response: &pb.ResponseOp_ResponseRange{}}
		case *pb.RequestOp_RequestPut:
			resps[i] = &pb.ResponseOp{Response: &pb.ResponseOp_ResponsePut{}}
		case *pb.RequestOp_RequestDeleteRange:
			resps[i] = &pb.ResponseOp{Response: &pb.ResponseOp_ResponseDeleteRange{}}
		case *pb.RequestOp_RequestTxn:
			resp, txns := newTxnResp(tv.RequestTxn, txnPath[1:])
			resps[i] = &pb.ResponseOp{Response: &pb.ResponseOp_ResponseTxn{ResponseTxn: resp}}
			txnPath = txnPath[1+txns:]
			txnCount += txns + 1
		default:
		}
	}
	return txnResp, txnCount
}

func executeTxn(ctx context.Context, lg *zap.Logger, txnWrite mvcc.TxnWrite, rt *pb.TxnRequest, txnPath []bool, tresp *pb.TxnResponse) (txns int, err error) {
	trace := traceutil.Get(ctx)
	reqs := rt.Success
	if !txnPath[0] {
		reqs = rt.Failure
	}

	for i, req := range reqs {
		respi := tresp.Responses[i].Response
		switch tv := req.Request.(type) {
		case *pb.RequestOp_RequestRange:
			trace.StartSubTrace(
				traceutil.Field{Key: "req_type", Value: "range"},
				traceutil.Field{Key: "range_begin", Value: string(tv.RequestRange.Key)},
				traceutil.Field{Key: "range_end", Value: string(tv.RequestRange.RangeEnd)})
			resp, err := executeRange(ctx, lg, txnWrite, tv.RequestRange)
			if err != nil {
				return 0, fmt.Errorf("applyTxn: failed Range: %w", err)
			}
			respi.(*pb.ResponseOp_ResponseRange).ResponseRange = resp
			trace.StopSubTrace()
		case *pb.RequestOp_RequestPut:
			trace.StartSubTrace(
				traceutil.Field{Key: "req_type", Value: "put"},
				traceutil.Field{Key: "key", Value: string(tv.RequestPut.Key)},
				traceutil.Field{Key: "req_size", Value: tv.RequestPut.Size()})
			resp, err := put(ctx, txnWrite, tv.RequestPut)
			if err != nil {
				return 0, fmt.Errorf("applyTxn: failed Put: %w", err)
			}
			respi.(*pb.ResponseOp_ResponsePut).ResponsePut = resp
			trace.StopSubTrace()
		case *pb.RequestOp_RequestDeleteRange:
			resp, err := deleteRange(ctx, txnWrite, tv.RequestDeleteRange)
			if err != nil {
				return 0, fmt.Errorf("applyTxn: failed DeleteRange: %w", err)
			}
			respi.(*pb.ResponseOp_ResponseDeleteRange).ResponseDeleteRange = resp
		case *pb.RequestOp_RequestTxn:
			resp := respi.(*pb.ResponseOp_ResponseTxn).ResponseTxn
			applyTxns, err := executeTxn(ctx, lg, txnWrite, tv.RequestTxn, txnPath[1:], resp)
			if err != nil {
				// don't wrap the error. It's a recursive call and err should be already wrapped
				return 0, err
			}
			txns += applyTxns + 1
			txnPath = txnPath[applyTxns+1:]
		default:
			// empty union
		}
	}
	return txns, nil
}

func checkPut(rv mvcc.ReadView, lessor lease.Lessor, req *pb.PutRequest) error {
	if req.IgnoreValue || req.IgnoreLease {
		// expects previous key-value, error if not exist
		rr, err := rv.Range(context.TODO(), req.Key, nil, mvcc.RangeOptions{})
		if err != nil {
			return err
		}
		if rr == nil || len(rr.KVs) == 0 {
			return errors.ErrKeyNotFound
		}
	}
	if lease.LeaseID(req.Lease) != lease.NoLease {
		if l := lessor.Lookup(lease.LeaseID(req.Lease)); l == nil {
			return lease.ErrLeaseNotFound
		}
	}
	return nil
}

func checkRange(rv mvcc.ReadView, req *pb.RangeRequest) error {
	switch {
	case req.Revision == 0:
		return nil
	case req.Revision > rv.Rev():
		return mvcc.ErrFutureRev
	case req.Revision < rv.FirstRev():
		return mvcc.ErrCompacted
	}
	return nil
}

func checkTxn(rv mvcc.ReadView, rt *pb.TxnRequest, lessor lease.Lessor, txnPath []bool) (int, error) {
	txnCount := 0
	reqs := rt.Success
	if !txnPath[0] {
		reqs = rt.Failure
	}
	for _, req := range reqs {
		var err error
		var txns int
		switch tv := req.Request.(type) {
		case *pb.RequestOp_RequestRange:
			err = checkRange(rv, tv.RequestRange)
		case *pb.RequestOp_RequestPut:
			err = checkPut(rv, lessor, tv.RequestPut)
		case *pb.RequestOp_RequestDeleteRange:
		case *pb.RequestOp_RequestTxn:
			txns, err = checkTxn(rv, tv.RequestTxn, lessor, txnPath[1:])
			txnCount += txns + 1
			txnPath = txnPath[txns+1:]
		default:
			// empty union
		}
		if err != nil {
			return 0, err
		}
	}
	return txnCount, nil
}

// mkGteRange determines if the range end is a >= range. This works around grpc
// sending empty byte strings as nil; >= is encoded in the range end as '\0'.
// If it is a GTE range, then []byte{} is returned to indicate the empty byte
// string (vs nil being no byte string).
func mkGteRange(rangeEnd []byte) []byte {
	if len(rangeEnd) == 1 && rangeEnd[0] == 0 {
		return []byte{}
	}
	return rangeEnd
}

func pruneKVs(rr *mvcc.RangeResult, isPrunable func(*mvccpb.KeyValue) bool) {
	j := 0
	for i := range rr.KVs {
		rr.KVs[j] = rr.KVs[i]
		if !isPrunable(&rr.KVs[i]) {
			j++
		}
	}
	rr.KVs = rr.KVs[:j]
}

type kvSort struct{ kvs []mvccpb.KeyValue }

func (s *kvSort) Swap(i, j int) {
	t := s.kvs[i]
	s.kvs[i] = s.kvs[j]
	s.kvs[j] = t
}
func (s *kvSort) Len() int { return len(s.kvs) }

type kvSortByKey struct{ *kvSort }

func (s *kvSortByKey) Less(i, j int) bool {
	return bytes.Compare(s.kvs[i].Key, s.kvs[j].Key) < 0
}

type kvSortByVersion struct{ *kvSort }

func (s *kvSortByVersion) Less(i, j int) bool {
	return (s.kvs[i].Version - s.kvs[j].Version) < 0
}

type kvSortByCreate struct{ *kvSort }

func (s *kvSortByCreate) Less(i, j int) bool {
	return (s.kvs[i].CreateRevision - s.kvs[j].CreateRevision) < 0
}

type kvSortByMod struct{ *kvSort }

func (s *kvSortByMod) Less(i, j int) bool {
	return (s.kvs[i].ModRevision - s.kvs[j].ModRevision) < 0
}

type kvSortByValue struct{ *kvSort }

func (s *kvSortByValue) Less(i, j int) bool {
	return bytes.Compare(s.kvs[i].Value, s.kvs[j].Value) < 0
}

func compareInt64(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

func compareToPath(rv mvcc.ReadView, rt *pb.TxnRequest) []bool {
	txnPath := make([]bool, 1)
	ops := rt.Success
	if txnPath[0] = applyCompares(rv, rt.Compare); !txnPath[0] {
		ops = rt.Failure
	}
	for _, op := range ops {
		tv, ok := op.Request.(*pb.RequestOp_RequestTxn)
		if !ok || tv.RequestTxn == nil {
			continue
		}
		txnPath = append(txnPath, compareToPath(rv, tv.RequestTxn)...)
	}
	return txnPath
}

func applyCompares(rv mvcc.ReadView, cmps []*pb.Compare) bool {
	for _, c := range cmps {
		if !applyCompare(rv, c) {
			return false
		}
	}
	return true
}

// applyCompare applies the compare request.
// If the comparison succeeds, it returns true. Otherwise, returns false.
func applyCompare(rv mvcc.ReadView, c *pb.Compare) bool {
	// TODO: possible optimizations
	// * chunk reads for large ranges to conserve memory
	// * rewrite rules for common patterns:
	//	ex. "[a, b) createrev > 0" => "limit 1 /\ kvs > 0"
	// * caching
	rr, err := rv.Range(context.TODO(), c.Key, mkGteRange(c.RangeEnd), mvcc.RangeOptions{})
	if err != nil {
		return false
	}
	if len(rr.KVs) == 0 {
		if c.Target == pb.Compare_VALUE {
			// Always fail if comparing a value on a key/keys that doesn't exist;
			// nil == empty string in grpc; no way to represent missing value
			return false
		}
		return compareKV(c, mvccpb.KeyValue{})
	}
	for _, kv := range rr.KVs {
		if !compareKV(c, kv) {
			return false
		}
	}
	return true
}

func compareKV(c *pb.Compare, ckv mvccpb.KeyValue) bool {
	var result int
	rev := int64(0)
	switch c.Target {
	case pb.Compare_VALUE:
		var v []byte
		if tv, _ := c.TargetUnion.(*pb.Compare_Value); tv != nil {
			v = tv.Value
		}
		result = bytes.Compare(ckv.Value, v)
	case pb.Compare_CREATE:
		if tv, _ := c.TargetUnion.(*pb.Compare_CreateRevision); tv != nil {
			rev = tv.CreateRevision
		}
		result = compareInt64(ckv.CreateRevision, rev)
	case pb.Compare_MOD:
		if tv, _ := c.TargetUnion.(*pb.Compare_ModRevision); tv != nil {
			rev = tv.ModRevision
		}
		result = compareInt64(ckv.ModRevision, rev)
	case pb.Compare_VERSION:
		if tv, _ := c.TargetUnion.(*pb.Compare_Version); tv != nil {
			rev = tv.Version
		}
		result = compareInt64(ckv.Version, rev)
	case pb.Compare_LEASE:
		if tv, _ := c.TargetUnion.(*pb.Compare_Lease); tv != nil {
			rev = tv.Lease
		}
		result = compareInt64(ckv.Lease, rev)
	}
	switch c.Result {
	case pb.Compare_EQUAL:
		return result == 0
	case pb.Compare_NOT_EQUAL:
		return result != 0
	case pb.Compare_GREATER:
		return result > 0
	case pb.Compare_LESS:
		return result < 0
	}
	return true
}

func IsTxnSerializable(r *pb.TxnRequest) bool {
	for _, u := range r.Success {
		if r := u.GetRequestRange(); r == nil || !r.Serializable {
			return false
		}
	}
	for _, u := range r.Failure {
		if r := u.GetRequestRange(); r == nil || !r.Serializable {
			return false
		}
	}
	return true
}

func IsTxnReadonly(r *pb.TxnRequest) bool {
	for _, u := range r.Success {
		if r := u.GetRequestRange(); r == nil {
			return false
		}
	}
	for _, u := range r.Failure {
		if r := u.GetRequestRange(); r == nil {
			return false
		}
	}
	return true
}

func CheckTxnAuth(as auth.AuthStore, ai *auth.AuthInfo, rt *pb.TxnRequest) error {
	for _, c := range rt.Compare {
		if err := as.IsRangePermitted(ai, c.Key, c.RangeEnd); err != nil {
			return err
		}
	}
	if err := checkTxnReqsPermission(as, ai, rt.Success); err != nil {
		return err
	}
	return checkTxnReqsPermission(as, ai, rt.Failure)
}

func checkTxnReqsPermission(as auth.AuthStore, ai *auth.AuthInfo, reqs []*pb.RequestOp) error {
	for _, requ := range reqs {
		switch tv := requ.Request.(type) {
		case *pb.RequestOp_RequestRange:
			if tv.RequestRange == nil {
				continue
			}

			if err := as.IsRangePermitted(ai, tv.RequestRange.Key, tv.RequestRange.RangeEnd); err != nil {
				return err
			}

		case *pb.RequestOp_RequestPut:
			if tv.RequestPut == nil {
				continue
			}

			if err := as.IsPutPermitted(ai, tv.RequestPut.Key); err != nil {
				return err
			}

		case *pb.RequestOp_RequestDeleteRange:
			if tv.RequestDeleteRange == nil {
				continue
			}

			if tv.RequestDeleteRange.PrevKv {
				err := as.IsRangePermitted(ai, tv.RequestDeleteRange.Key, tv.RequestDeleteRange.RangeEnd)
				if err != nil {
					return err
				}
			}

			err := as.IsDeleteRangePermitted(ai, tv.RequestDeleteRange.Key, tv.RequestDeleteRange.RangeEnd)
			if err != nil {
				return err
			}
		}
	}

	return nil
}
