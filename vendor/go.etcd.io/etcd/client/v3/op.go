// Copyright 2016 The etcd Authors
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

import pb "go.etcd.io/etcd/api/v3/etcdserverpb"

type opType int

const (
	// A default Op has opType 0, which is invalid.
	tRange opType = iota + 1
	tPut
	tDeleteRange
	tTxn
)

var noPrefixEnd = []byte{0}

// Op represents an Operation that kv can execute.
type Op struct {
	t   opType
	key []byte
	end []byte

	// for range
	limit        int64
	sort         *SortOption
	serializable bool
	keysOnly     bool
	countOnly    bool
	minModRev    int64
	maxModRev    int64
	minCreateRev int64
	maxCreateRev int64

	// for range, watch
	rev int64

	// for watch, put, delete
	prevKV bool

	// for watch
	// fragmentation should be disabled by default
	// if true, split watch events when total exceeds
	// "--max-request-bytes" flag value + 512-byte
	fragment bool

	// for put
	ignoreValue bool
	ignoreLease bool

	// progressNotify is for progress updates.
	progressNotify bool
	// createdNotify is for created event
	createdNotify bool
	// filters for watchers
	filterPut    bool
	filterDelete bool

	// for put
	val     []byte
	leaseID LeaseID

	// txn
	cmps    []Cmp
	thenOps []Op
	elseOps []Op

	isOptsWithFromKey bool
	isOptsWithPrefix  bool
}

// accessors / mutators

// IsTxn returns true if the "Op" type is transaction.
func (op Op) IsTxn() bool {
	return op.t == tTxn
}

// Txn returns the comparison(if) operations, "then" operations, and "else" operations.
func (op Op) Txn() ([]Cmp, []Op, []Op) {
	return op.cmps, op.thenOps, op.elseOps
}

// KeyBytes returns the byte slice holding the Op's key.
func (op Op) KeyBytes() []byte { return op.key }

// WithKeyBytes sets the byte slice for the Op's key.
func (op *Op) WithKeyBytes(key []byte) { op.key = key }

// RangeBytes returns the byte slice holding with the Op's range end, if any.
func (op Op) RangeBytes() []byte { return op.end }

// Rev returns the requested revision, if any.
func (op Op) Rev() int64 { return op.rev }

// IsPut returns true iff the operation is a Put.
func (op Op) IsPut() bool { return op.t == tPut }

// IsGet returns true iff the operation is a Get.
func (op Op) IsGet() bool { return op.t == tRange }

// IsDelete returns true iff the operation is a Delete.
func (op Op) IsDelete() bool { return op.t == tDeleteRange }

// IsSerializable returns true if the serializable field is true.
func (op Op) IsSerializable() bool { return op.serializable }

// IsKeysOnly returns whether keysOnly is set.
func (op Op) IsKeysOnly() bool { return op.keysOnly }

// IsCountOnly returns whether countOnly is set.
func (op Op) IsCountOnly() bool { return op.countOnly }

func (op Op) IsOptsWithFromKey() bool { return op.isOptsWithFromKey }

func (op Op) IsOptsWithPrefix() bool { return op.isOptsWithPrefix }

// MinModRev returns the operation's minimum modify revision.
func (op Op) MinModRev() int64 { return op.minModRev }

// MaxModRev returns the operation's maximum modify revision.
func (op Op) MaxModRev() int64 { return op.maxModRev }

// MinCreateRev returns the operation's minimum create revision.
func (op Op) MinCreateRev() int64 { return op.minCreateRev }

// MaxCreateRev returns the operation's maximum create revision.
func (op Op) MaxCreateRev() int64 { return op.maxCreateRev }

// WithRangeBytes sets the byte slice for the Op's range end.
func (op *Op) WithRangeBytes(end []byte) { op.end = end }

// ValueBytes returns the byte slice holding the Op's value, if any.
func (op Op) ValueBytes() []byte { return op.val }

// WithValueBytes sets the byte slice for the Op's value.
func (op *Op) WithValueBytes(v []byte) { op.val = v }

func (op Op) toRangeRequest() *pb.RangeRequest {
	if op.t != tRange {
		panic("op.t != tRange")
	}
	r := &pb.RangeRequest{
		Key:               op.key,
		RangeEnd:          op.end,
		Limit:             op.limit,
		Revision:          op.rev,
		Serializable:      op.serializable,
		KeysOnly:          op.keysOnly,
		CountOnly:         op.countOnly,
		MinModRevision:    op.minModRev,
		MaxModRevision:    op.maxModRev,
		MinCreateRevision: op.minCreateRev,
		MaxCreateRevision: op.maxCreateRev,
	}
	if op.sort != nil {
		r.SortOrder = pb.RangeRequest_SortOrder(op.sort.Order)
		r.SortTarget = pb.RangeRequest_SortTarget(op.sort.Target)
	}
	return r
}

func (op Op) toTxnRequest() *pb.TxnRequest {
	thenOps := make([]*pb.RequestOp, len(op.thenOps))
	for i, tOp := range op.thenOps {
		thenOps[i] = tOp.toRequestOp()
	}
	elseOps := make([]*pb.RequestOp, len(op.elseOps))
	for i, eOp := range op.elseOps {
		elseOps[i] = eOp.toRequestOp()
	}
	cmps := make([]*pb.Compare, len(op.cmps))
	for i := range op.cmps {
		cmps[i] = (*pb.Compare)(&op.cmps[i])
	}
	return &pb.TxnRequest{Compare: cmps, Success: thenOps, Failure: elseOps}
}

func (op Op) toRequestOp() *pb.RequestOp {
	switch op.t {
	case tRange:
		return &pb.RequestOp{Request: &pb.RequestOp_RequestRange{RequestRange: op.toRangeRequest()}}
	case tPut:
		r := &pb.PutRequest{Key: op.key, Value: op.val, Lease: int64(op.leaseID), PrevKv: op.prevKV, IgnoreValue: op.ignoreValue, IgnoreLease: op.ignoreLease}
		return &pb.RequestOp{Request: &pb.RequestOp_RequestPut{RequestPut: r}}
	case tDeleteRange:
		r := &pb.DeleteRangeRequest{Key: op.key, RangeEnd: op.end, PrevKv: op.prevKV}
		return &pb.RequestOp{Request: &pb.RequestOp_RequestDeleteRange{RequestDeleteRange: r}}
	case tTxn:
		return &pb.RequestOp{Request: &pb.RequestOp_RequestTxn{RequestTxn: op.toTxnRequest()}}
	default:
		panic("Unknown Op")
	}
}

func (op Op) isWrite() bool {
	if op.t == tTxn {
		for _, tOp := range op.thenOps {
			if tOp.isWrite() {
				return true
			}
		}
		for _, tOp := range op.elseOps {
			if tOp.isWrite() {
				return true
			}
		}
		return false
	}
	return op.t != tRange
}

func NewOp() *Op {
	return &Op{key: []byte("")}
}

// OpGet returns "get" operation based on given key and operation options.
func OpGet(key string, opts ...OpOption) Op {
	// WithPrefix and WithFromKey are not supported together
	if IsOptsWithPrefix(opts) && IsOptsWithFromKey(opts) {
		panic("`WithPrefix` and `WithFromKey` cannot be set at the same time, choose one")
	}
	ret := Op{t: tRange, key: []byte(key)}
	ret.applyOpts(opts)
	return ret
}

// OpDelete returns "delete" operation based on given key and operation options.
func OpDelete(key string, opts ...OpOption) Op {
	// WithPrefix and WithFromKey are not supported together
	if IsOptsWithPrefix(opts) && IsOptsWithFromKey(opts) {
		panic("`WithPrefix` and `WithFromKey` cannot be set at the same time, choose one")
	}
	ret := Op{t: tDeleteRange, key: []byte(key)}
	ret.applyOpts(opts)
	switch {
	case ret.leaseID != 0:
		panic("unexpected lease in delete")
	case ret.limit != 0:
		panic("unexpected limit in delete")
	case ret.rev != 0:
		panic("unexpected revision in delete")
	case ret.sort != nil:
		panic("unexpected sort in delete")
	case ret.serializable:
		panic("unexpected serializable in delete")
	case ret.countOnly:
		panic("unexpected countOnly in delete")
	case ret.minModRev != 0, ret.maxModRev != 0:
		panic("unexpected mod revision filter in delete")
	case ret.minCreateRev != 0, ret.maxCreateRev != 0:
		panic("unexpected create revision filter in delete")
	case ret.filterDelete, ret.filterPut:
		panic("unexpected filter in delete")
	case ret.createdNotify:
		panic("unexpected createdNotify in delete")
	}
	return ret
}

// OpPut returns "put" operation based on given key-value and operation options.
func OpPut(key, val string, opts ...OpOption) Op {
	ret := Op{t: tPut, key: []byte(key), val: []byte(val)}
	ret.applyOpts(opts)
	switch {
	case ret.end != nil:
		panic("unexpected range in put")
	case ret.limit != 0:
		panic("unexpected limit in put")
	case ret.rev != 0:
		panic("unexpected revision in put")
	case ret.sort != nil:
		panic("unexpected sort in put")
	case ret.serializable:
		panic("unexpected serializable in put")
	case ret.countOnly:
		panic("unexpected countOnly in put")
	case ret.minModRev != 0, ret.maxModRev != 0:
		panic("unexpected mod revision filter in put")
	case ret.minCreateRev != 0, ret.maxCreateRev != 0:
		panic("unexpected create revision filter in put")
	case ret.filterDelete, ret.filterPut:
		panic("unexpected filter in put")
	case ret.createdNotify:
		panic("unexpected createdNotify in put")
	}
	return ret
}

// OpTxn returns "txn" operation based on given transaction conditions.
func OpTxn(cmps []Cmp, thenOps []Op, elseOps []Op) Op {
	return Op{t: tTxn, cmps: cmps, thenOps: thenOps, elseOps: elseOps}
}

func opWatch(key string, opts ...OpOption) Op {
	ret := Op{t: tRange, key: []byte(key)}
	ret.applyOpts(opts)
	switch {
	case ret.leaseID != 0:
		panic("unexpected lease in watch")
	case ret.limit != 0:
		panic("unexpected limit in watch")
	case ret.sort != nil:
		panic("unexpected sort in watch")
	case ret.serializable:
		panic("unexpected serializable in watch")
	case ret.countOnly:
		panic("unexpected countOnly in watch")
	case ret.minModRev != 0, ret.maxModRev != 0:
		panic("unexpected mod revision filter in watch")
	case ret.minCreateRev != 0, ret.maxCreateRev != 0:
		panic("unexpected create revision filter in watch")
	}
	return ret
}

func (op *Op) applyOpts(opts []OpOption) {
	for _, opt := range opts {
		opt(op)
	}
}

// OpOption configures Operations like Get, Put, Delete.
type OpOption func(*Op)

// WithLease attaches a lease ID to a key in 'Put' request.
func WithLease(leaseID LeaseID) OpOption {
	return func(op *Op) { op.leaseID = leaseID }
}

// WithLimit limits the number of results to return from 'Get' request.
// If WithLimit is given a 0 limit, it is treated as no limit.
func WithLimit(n int64) OpOption { return func(op *Op) { op.limit = n } }

// WithRev specifies the store revision for 'Get' request.
// Or the start revision of 'Watch' request.
func WithRev(rev int64) OpOption { return func(op *Op) { op.rev = rev } }

// WithSort specifies the ordering in 'Get' request. It requires
// 'WithRange' and/or 'WithPrefix' to be specified too.
// 'target' specifies the target to sort by: key, version, revisions, value.
// 'order' can be either 'SortNone', 'SortAscend', 'SortDescend'.
func WithSort(target SortTarget, order SortOrder) OpOption {
	return func(op *Op) {
		if target == SortByKey && order == SortAscend {
			// If order != SortNone, server fetches the entire key-space,
			// and then applies the sort and limit, if provided.
			// Since by default the server returns results sorted by keys
			// in lexicographically ascending order, the client should ignore
			// SortOrder if the target is SortByKey.
			order = SortNone
		}
		op.sort = &SortOption{target, order}
	}
}

// GetPrefixRangeEnd gets the range end of the prefix.
// 'Get(foo, WithPrefix())' is equal to 'Get(foo, WithRange(GetPrefixRangeEnd(foo))'.
func GetPrefixRangeEnd(prefix string) string {
	return string(getPrefix([]byte(prefix)))
}

func getPrefix(key []byte) []byte {
	end := make([]byte, len(key))
	copy(end, key)
	for i := len(end) - 1; i >= 0; i-- {
		if end[i] < 0xff {
			end[i] = end[i] + 1
			end = end[:i+1]
			return end
		}
	}
	// next prefix does not exist (e.g., 0xffff);
	// default to WithFromKey policy
	return noPrefixEnd
}

// WithPrefix enables 'Get', 'Delete', or 'Watch' requests to operate
// on the keys with matching prefix. For example, 'Get(foo, WithPrefix())'
// can return 'foo1', 'foo2', and so on.
func WithPrefix() OpOption {
	return func(op *Op) {
		op.isOptsWithPrefix = true
		if len(op.key) == 0 {
			op.key, op.end = []byte{0}, []byte{0}
			return
		}
		op.end = getPrefix(op.key)
	}
}

// WithRange specifies the range of 'Get', 'Delete', 'Watch' requests.
// For example, 'Get' requests with 'WithRange(end)' returns
// the keys in the range [key, end).
// endKey must be lexicographically greater than start key.
func WithRange(endKey string) OpOption {
	return func(op *Op) { op.end = []byte(endKey) }
}

// WithFromKey specifies the range of 'Get', 'Delete', 'Watch' requests
// to be equal or greater than the key in the argument.
func WithFromKey() OpOption {
	return func(op *Op) {
		if len(op.key) == 0 {
			op.key = []byte{0}
		}
		op.end = []byte("\x00")
		op.isOptsWithFromKey = true
	}
}

// WithSerializable makes `Get` and `MemberList` requests serializable.
// By default, they are linearizable. Serializable requests are better
// for lower latency requirement, but users should be aware that they
// could get stale data with serializable requests.
//
// In some situations users may want to use serializable requests. For
// example, when adding a new member to a one-node cluster, it's reasonable
// and safe to use serializable request before the new added member gets
// started.
func WithSerializable() OpOption {
	return func(op *Op) { op.serializable = true }
}

// WithKeysOnly makes the 'Get' request return only the keys and the corresponding
// values will be omitted.
func WithKeysOnly() OpOption {
	return func(op *Op) { op.keysOnly = true }
}

// WithCountOnly makes the 'Get' request return only the count of keys.
func WithCountOnly() OpOption {
	return func(op *Op) { op.countOnly = true }
}

// WithMinModRev filters out keys for Get with modification revisions less than the given revision.
func WithMinModRev(rev int64) OpOption { return func(op *Op) { op.minModRev = rev } }

// WithMaxModRev filters out keys for Get with modification revisions greater than the given revision.
func WithMaxModRev(rev int64) OpOption { return func(op *Op) { op.maxModRev = rev } }

// WithMinCreateRev filters out keys for Get with creation revisions less than the given revision.
func WithMinCreateRev(rev int64) OpOption { return func(op *Op) { op.minCreateRev = rev } }

// WithMaxCreateRev filters out keys for Get with creation revisions greater than the given revision.
func WithMaxCreateRev(rev int64) OpOption { return func(op *Op) { op.maxCreateRev = rev } }

// WithFirstCreate gets the key with the oldest creation revision in the request range.
func WithFirstCreate() []OpOption { return withTop(SortByCreateRevision, SortAscend) }

// WithLastCreate gets the key with the latest creation revision in the request range.
func WithLastCreate() []OpOption { return withTop(SortByCreateRevision, SortDescend) }

// WithFirstKey gets the lexically first key in the request range.
func WithFirstKey() []OpOption { return withTop(SortByKey, SortAscend) }

// WithLastKey gets the lexically last key in the request range.
func WithLastKey() []OpOption { return withTop(SortByKey, SortDescend) }

// WithFirstRev gets the key with the oldest modification revision in the request range.
func WithFirstRev() []OpOption { return withTop(SortByModRevision, SortAscend) }

// WithLastRev gets the key with the latest modification revision in the request range.
func WithLastRev() []OpOption { return withTop(SortByModRevision, SortDescend) }

// withTop gets the first key over the get's prefix given a sort order
func withTop(target SortTarget, order SortOrder) []OpOption {
	return []OpOption{WithPrefix(), WithSort(target, order), WithLimit(1)}
}

// WithProgressNotify makes watch server send periodic progress updates
// every 10 minutes when there is no incoming events.
// Progress updates have zero events in WatchResponse.
func WithProgressNotify() OpOption {
	return func(op *Op) {
		op.progressNotify = true
	}
}

// WithCreatedNotify makes watch server sends the created event.
func WithCreatedNotify() OpOption {
	return func(op *Op) {
		op.createdNotify = true
	}
}

// WithFilterPut discards PUT events from the watcher.
func WithFilterPut() OpOption {
	return func(op *Op) { op.filterPut = true }
}

// WithFilterDelete discards DELETE events from the watcher.
func WithFilterDelete() OpOption {
	return func(op *Op) { op.filterDelete = true }
}

// WithPrevKV gets the previous key-value pair before the event happens. If the previous KV is already compacted,
// nothing will be returned.
func WithPrevKV() OpOption {
	return func(op *Op) {
		op.prevKV = true
	}
}

// WithFragment to receive raw watch response with fragmentation.
// Fragmentation is disabled by default. If fragmentation is enabled,
// etcd watch server will split watch response before sending to clients
// when the total size of watch events exceed server-side request limit.
// The default server-side request limit is 1.5 MiB, which can be configured
// as "--max-request-bytes" flag value + gRPC-overhead 512 bytes.
// See "etcdserver/api/v3rpc/watch.go" for more details.
func WithFragment() OpOption {
	return func(op *Op) { op.fragment = true }
}

// WithIgnoreValue updates the key using its current value.
// This option can not be combined with non-empty values.
// Returns an error if the key does not exist.
func WithIgnoreValue() OpOption {
	return func(op *Op) {
		op.ignoreValue = true
	}
}

// WithIgnoreLease updates the key using its current lease.
// This option can not be combined with WithLease.
// Returns an error if the key does not exist.
func WithIgnoreLease() OpOption {
	return func(op *Op) {
		op.ignoreLease = true
	}
}

// LeaseOp represents an Operation that lease can execute.
type LeaseOp struct {
	id LeaseID

	// for TimeToLive
	attachedKeys bool
}

// LeaseOption configures lease operations.
type LeaseOption func(*LeaseOp)

func (op *LeaseOp) applyOpts(opts []LeaseOption) {
	for _, opt := range opts {
		opt(op)
	}
}

// WithAttachedKeys makes TimeToLive list the keys attached to the given lease ID.
func WithAttachedKeys() LeaseOption {
	return func(op *LeaseOp) { op.attachedKeys = true }
}

func toLeaseTimeToLiveRequest(id LeaseID, opts ...LeaseOption) *pb.LeaseTimeToLiveRequest {
	ret := &LeaseOp{id: id}
	ret.applyOpts(opts)
	return &pb.LeaseTimeToLiveRequest{ID: int64(id), Keys: ret.attachedKeys}
}

// IsOptsWithPrefix returns true if WithPrefix option is called in the given opts.
func IsOptsWithPrefix(opts []OpOption) bool {
	ret := NewOp()
	for _, opt := range opts {
		opt(ret)
	}

	return ret.isOptsWithPrefix
}

// IsOptsWithFromKey returns true if WithFromKey option is called in the given opts.
func IsOptsWithFromKey(opts []OpOption) bool {
	ret := NewOp()
	for _, opt := range opts {
		opt(ret)
	}

	return ret.isOptsWithFromKey
}

func (op Op) IsSortOptionValid() bool {
	if op.sort != nil {
		sortOrder := int32(op.sort.Order)
		sortTarget := int32(op.sort.Target)

		if _, ok := pb.RangeRequest_SortOrder_name[sortOrder]; !ok {
			return false
		}

		if _, ok := pb.RangeRequest_SortTarget_name[sortTarget]; !ok {
			return false
		}
	}
	return true
}
