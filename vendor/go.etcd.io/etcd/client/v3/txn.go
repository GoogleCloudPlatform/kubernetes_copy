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

import (
	"context"
	"sync"

	"google.golang.org/grpc"

	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
)

// Txn is the interface that wraps mini-transactions.
//
//	Txn(context.TODO()).If(
//	 Compare(Value(k1), ">", v1),
//	 Compare(Version(k1), "=", 2)
//	).Then(
//	 OpPut(k2,v2), OpPut(k3,v3)
//	).Else(
//	 OpPut(k4,v4), OpPut(k5,v5)
//	).Commit()
type Txn interface {
	// If takes a list of comparison. If all comparisons passed in succeed,
	// the operations passed into Then() will be executed. Or the operations
	// passed into Else() will be executed.
	If(cs ...Cmp) Txn

	// Then takes a list of operations. The Ops list will be executed, if the
	// comparisons passed in If() succeed.
	Then(ops ...Op) Txn

	// Else takes a list of operations. The Ops list will be executed, if the
	// comparisons passed in If() fail.
	Else(ops ...Op) Txn

	// Commit tries to commit the transaction.
	Commit() (*TxnResponse, error)
}

type txn struct {
	kv  *kv
	ctx context.Context

	mu    sync.Mutex
	cif   bool
	cthen bool
	celse bool

	isWrite bool

	cmps []*pb.Compare

	sus []*pb.RequestOp
	fas []*pb.RequestOp

	callOpts []grpc.CallOption
}

func (txn *txn) If(cs ...Cmp) Txn {
	txn.mu.Lock()
	defer txn.mu.Unlock()

	if txn.cif {
		panic("cannot call If twice!")
	}

	if txn.cthen {
		panic("cannot call If after Then!")
	}

	if txn.celse {
		panic("cannot call If after Else!")
	}

	txn.cif = true

	for i := range cs {
		txn.cmps = append(txn.cmps, (*pb.Compare)(&cs[i]))
	}

	return txn
}

func (txn *txn) Then(ops ...Op) Txn {
	txn.mu.Lock()
	defer txn.mu.Unlock()

	if txn.cthen {
		panic("cannot call Then twice!")
	}
	if txn.celse {
		panic("cannot call Then after Else!")
	}

	txn.cthen = true

	for _, op := range ops {
		txn.isWrite = txn.isWrite || op.isWrite()
		txn.sus = append(txn.sus, op.toRequestOp())
	}

	return txn
}

func (txn *txn) Else(ops ...Op) Txn {
	txn.mu.Lock()
	defer txn.mu.Unlock()

	if txn.celse {
		panic("cannot call Else twice!")
	}

	txn.celse = true

	for _, op := range ops {
		txn.isWrite = txn.isWrite || op.isWrite()
		txn.fas = append(txn.fas, op.toRequestOp())
	}

	return txn
}

func (txn *txn) Commit() (*TxnResponse, error) {
	txn.mu.Lock()
	defer txn.mu.Unlock()

	r := &pb.TxnRequest{Compare: txn.cmps, Success: txn.sus, Failure: txn.fas}

	var resp *pb.TxnResponse
	var err error
	resp, err = txn.kv.remote.Txn(txn.ctx, r, txn.callOpts...)
	if err != nil {
		return nil, ContextError(txn.ctx, err)
	}
	return (*TxnResponse)(resp), nil
}
