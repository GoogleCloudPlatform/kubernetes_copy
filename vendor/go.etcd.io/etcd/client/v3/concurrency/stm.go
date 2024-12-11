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

package concurrency

import (
	"context"
	"math"

	v3 "go.etcd.io/etcd/client/v3"
)

// STM is an interface for software transactional memory.
type STM interface {
	// Get returns the value for a key and inserts the key in the txn's read set.
	// If Get fails, it aborts the transaction with an error, never returning.
	Get(key ...string) string
	// Put adds a value for a key to the write set.
	Put(key, val string, opts ...v3.OpOption)
	// Rev returns the revision of a key in the read set.
	Rev(key string) int64
	// Del deletes a key.
	Del(key string)

	// commit attempts to apply the txn's changes to the server.
	commit() *v3.TxnResponse
	reset()
}

// Isolation is an enumeration of transactional isolation levels which
// describes how transactions should interfere and conflict.
type Isolation int

const (
	// SerializableSnapshot provides serializable isolation and also checks
	// for write conflicts.
	SerializableSnapshot Isolation = iota
	// Serializable reads within the same transaction attempt return data
	// from the at the revision of the first read.
	Serializable
	// RepeatableReads reads within the same transaction attempt always
	// return the same data.
	RepeatableReads
	// ReadCommitted reads keys from any committed revision.
	ReadCommitted
)

// stmError safely passes STM errors through panic to the STM error channel.
type stmError struct{ err error }

type stmOptions struct {
	iso      Isolation
	ctx      context.Context
	prefetch []string
}

type stmOption func(*stmOptions)

// WithIsolation specifies the transaction isolation level.
func WithIsolation(lvl Isolation) stmOption {
	return func(so *stmOptions) { so.iso = lvl }
}

// WithAbortContext specifies the context for permanently aborting the transaction.
func WithAbortContext(ctx context.Context) stmOption {
	return func(so *stmOptions) { so.ctx = ctx }
}

// WithPrefetch is a hint to prefetch a list of keys before trying to apply.
// If an STM transaction will unconditionally fetch a set of keys, prefetching
// those keys will save the round-trip cost from requesting each key one by one
// with Get().
func WithPrefetch(keys ...string) stmOption {
	return func(so *stmOptions) { so.prefetch = append(so.prefetch, keys...) }
}

// NewSTM initiates a new STM instance, using serializable snapshot isolation by default.
func NewSTM(c *v3.Client, apply func(STM) error, so ...stmOption) (*v3.TxnResponse, error) {
	opts := &stmOptions{ctx: c.Ctx()}
	for _, f := range so {
		f(opts)
	}
	if len(opts.prefetch) != 0 {
		f := apply
		apply = func(s STM) error {
			s.Get(opts.prefetch...)
			return f(s)
		}
	}
	return runSTM(mkSTM(c, opts), apply)
}

func mkSTM(c *v3.Client, opts *stmOptions) STM {
	switch opts.iso {
	case SerializableSnapshot:
		s := &stmSerializable{
			stm:      stm{client: c, ctx: opts.ctx},
			prefetch: make(map[string]*v3.GetResponse),
		}
		s.conflicts = func() []v3.Cmp {
			return append(s.rset.cmps(), s.wset.cmps(s.rset.first()+1)...)
		}
		return s
	case Serializable:
		s := &stmSerializable{
			stm:      stm{client: c, ctx: opts.ctx},
			prefetch: make(map[string]*v3.GetResponse),
		}
		s.conflicts = func() []v3.Cmp { return s.rset.cmps() }
		return s
	case RepeatableReads:
		s := &stm{client: c, ctx: opts.ctx, getOpts: []v3.OpOption{v3.WithSerializable()}}
		s.conflicts = func() []v3.Cmp { return s.rset.cmps() }
		return s
	case ReadCommitted:
		s := &stm{client: c, ctx: opts.ctx, getOpts: []v3.OpOption{v3.WithSerializable()}}
		s.conflicts = func() []v3.Cmp { return nil }
		return s
	default:
		panic("unsupported stm")
	}
}

type stmResponse struct {
	resp *v3.TxnResponse
	err  error
}

func runSTM(s STM, apply func(STM) error) (*v3.TxnResponse, error) {
	outc := make(chan stmResponse, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				e, ok := r.(stmError)
				if !ok {
					// client apply panicked
					panic(r)
				}
				outc <- stmResponse{nil, e.err}
			}
		}()
		var out stmResponse
		for {
			s.reset()
			if out.err = apply(s); out.err != nil {
				break
			}
			if out.resp = s.commit(); out.resp != nil {
				break
			}
		}
		outc <- out
	}()
	r := <-outc
	return r.resp, r.err
}

// stm implements repeatable-read software transactional memory over etcd
type stm struct {
	client *v3.Client
	ctx    context.Context
	// rset holds read key values and revisions
	rset readSet
	// wset holds overwritten keys and their values
	wset writeSet
	// getOpts are the opts used for gets
	getOpts []v3.OpOption
	// conflicts computes the current conflicts on the txn
	conflicts func() []v3.Cmp
}

type stmPut struct {
	val string
	op  v3.Op
}

type readSet map[string]*v3.GetResponse

func (rs readSet) add(keys []string, txnresp *v3.TxnResponse) {
	for i, resp := range txnresp.Responses {
		rs[keys[i]] = (*v3.GetResponse)(resp.GetResponseRange())
	}
}

// first returns the store revision from the first fetch
func (rs readSet) first() int64 {
	ret := int64(math.MaxInt64 - 1)
	for _, resp := range rs {
		if rev := resp.Header.Revision; rev < ret {
			ret = rev
		}
	}
	return ret
}

// cmps guards the txn from updates to read set
func (rs readSet) cmps() []v3.Cmp {
	cmps := make([]v3.Cmp, 0, len(rs))
	for k, rk := range rs {
		cmps = append(cmps, isKeyCurrent(k, rk))
	}
	return cmps
}

type writeSet map[string]stmPut

func (ws writeSet) get(keys ...string) *stmPut {
	for _, key := range keys {
		if wv, ok := ws[key]; ok {
			return &wv
		}
	}
	return nil
}

// cmps returns a cmp list testing no writes have happened past rev
func (ws writeSet) cmps(rev int64) []v3.Cmp {
	cmps := make([]v3.Cmp, 0, len(ws))
	for key := range ws {
		cmps = append(cmps, v3.Compare(v3.ModRevision(key), "<", rev))
	}
	return cmps
}

// puts is the list of ops for all pending writes
func (ws writeSet) puts() []v3.Op {
	puts := make([]v3.Op, 0, len(ws))
	for _, v := range ws {
		puts = append(puts, v.op)
	}
	return puts
}

func (s *stm) Get(keys ...string) string {
	if wv := s.wset.get(keys...); wv != nil {
		return wv.val
	}
	return respToValue(s.fetch(keys...))
}

func (s *stm) Put(key, val string, opts ...v3.OpOption) {
	s.wset[key] = stmPut{val, v3.OpPut(key, val, opts...)}
}

func (s *stm) Del(key string) { s.wset[key] = stmPut{"", v3.OpDelete(key)} }

func (s *stm) Rev(key string) int64 {
	if resp := s.fetch(key); resp != nil && len(resp.Kvs) != 0 {
		return resp.Kvs[0].ModRevision
	}
	return 0
}

func (s *stm) commit() *v3.TxnResponse {
	txnresp, err := s.client.Txn(s.ctx).If(s.conflicts()...).Then(s.wset.puts()...).Commit()
	if err != nil {
		panic(stmError{err})
	}
	if txnresp.Succeeded {
		return txnresp
	}
	return nil
}

func (s *stm) fetch(keys ...string) *v3.GetResponse {
	if len(keys) == 0 {
		return nil
	}
	ops := make([]v3.Op, len(keys))
	for i, key := range keys {
		if resp, ok := s.rset[key]; ok {
			return resp
		}
		ops[i] = v3.OpGet(key, s.getOpts...)
	}
	txnresp, err := s.client.Txn(s.ctx).Then(ops...).Commit()
	if err != nil {
		panic(stmError{err})
	}
	s.rset.add(keys, txnresp)
	return (*v3.GetResponse)(txnresp.Responses[0].GetResponseRange())
}

func (s *stm) reset() {
	s.rset = make(map[string]*v3.GetResponse)
	s.wset = make(map[string]stmPut)
}

type stmSerializable struct {
	stm
	prefetch map[string]*v3.GetResponse
}

func (s *stmSerializable) Get(keys ...string) string {
	if len(keys) == 0 {
		return ""
	}

	if wv := s.wset.get(keys...); wv != nil {
		return wv.val
	}
	firstRead := len(s.rset) == 0
	for _, key := range keys {
		if resp, ok := s.prefetch[key]; ok {
			delete(s.prefetch, key)
			s.rset[key] = resp
		}
	}
	resp := s.stm.fetch(keys...)
	if firstRead {
		// txn's base revision is defined by the first read
		s.getOpts = []v3.OpOption{
			v3.WithRev(resp.Header.Revision),
			v3.WithSerializable(),
		}
	}
	return respToValue(resp)
}

func (s *stmSerializable) Rev(key string) int64 {
	s.Get(key)
	return s.stm.Rev(key)
}

func (s *stmSerializable) gets() ([]string, []v3.Op) {
	keys := make([]string, 0, len(s.rset))
	ops := make([]v3.Op, 0, len(s.rset))
	for k := range s.rset {
		keys = append(keys, k)
		ops = append(ops, v3.OpGet(k))
	}
	return keys, ops
}

func (s *stmSerializable) commit() *v3.TxnResponse {
	keys, getops := s.gets()
	txn := s.client.Txn(s.ctx).If(s.conflicts()...).Then(s.wset.puts()...)
	// use Else to prefetch keys in case of conflict to save a round trip
	txnresp, err := txn.Else(getops...).Commit()
	if err != nil {
		panic(stmError{err})
	}
	if txnresp.Succeeded {
		return txnresp
	}
	// load prefetch with Else data
	s.rset.add(keys, txnresp)
	s.prefetch = s.rset
	s.getOpts = nil
	return nil
}

func isKeyCurrent(k string, r *v3.GetResponse) v3.Cmp {
	if len(r.Kvs) != 0 {
		return v3.Compare(v3.ModRevision(k), "=", r.Kvs[0].ModRevision)
	}
	return v3.Compare(v3.ModRevision(k), "=", 0)
}

func respToValue(resp *v3.GetResponse) string {
	if resp == nil || len(resp.Kvs) == 0 {
		return ""
	}
	return string(resp.Kvs[0].Value)
}

// NewSTMRepeatable is deprecated.
func NewSTMRepeatable(ctx context.Context, c *v3.Client, apply func(STM) error) (*v3.TxnResponse, error) {
	return NewSTM(c, apply, WithAbortContext(ctx), WithIsolation(RepeatableReads))
}

// NewSTMSerializable is deprecated.
func NewSTMSerializable(ctx context.Context, c *v3.Client, apply func(STM) error) (*v3.TxnResponse, error) {
	return NewSTM(c, apply, WithAbortContext(ctx), WithIsolation(Serializable))
}

// NewSTMReadCommitted is deprecated.
func NewSTMReadCommitted(ctx context.Context, c *v3.Client, apply func(STM) error) (*v3.TxnResponse, error) {
	return NewSTM(c, apply, WithAbortContext(ctx), WithIsolation(ReadCommitted))
}
