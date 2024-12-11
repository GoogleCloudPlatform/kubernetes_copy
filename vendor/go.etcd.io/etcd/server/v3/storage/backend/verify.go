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

package backend

import (
	"fmt"
	"runtime/debug"
	"strings"

	"github.com/google/go-cmp/cmp"
	"go.uber.org/zap"

	"go.etcd.io/etcd/client/pkg/v3/verify"
)

const (
	EnvVerifyValueLock verify.VerificationType = "lock"
)

func ValidateCalledInsideApply(lg *zap.Logger) {
	if !verifyLockEnabled() {
		return
	}
	if !insideApply() {
		lg.Panic("Called outside of APPLY!", zap.Stack("stacktrace"))
	}
}

func ValidateCalledOutSideApply(lg *zap.Logger) {
	if !verifyLockEnabled() {
		return
	}
	if insideApply() {
		lg.Panic("Called inside of APPLY!", zap.Stack("stacktrace"))
	}
}

func ValidateCalledInsideUnittest(lg *zap.Logger) {
	if !verifyLockEnabled() {
		return
	}
	if !insideUnittest() {
		lg.Fatal("Lock called outside of unit test!", zap.Stack("stacktrace"))
	}
}

func verifyLockEnabled() bool {
	return verify.IsVerificationEnabled(EnvVerifyValueLock)
}

func insideApply() bool {
	stackTraceStr := string(debug.Stack())
	return strings.Contains(stackTraceStr, ".applyEntries")
}

func insideUnittest() bool {
	stackTraceStr := string(debug.Stack())
	return strings.Contains(stackTraceStr, "_test.go") && !strings.Contains(stackTraceStr, "tests/")
}

// VerifyBackendConsistency verifies data in ReadTx and BatchTx are consistent.
func VerifyBackendConsistency(b Backend, lg *zap.Logger, skipSafeRangeBucket bool, bucket ...Bucket) {
	verify.Verify(func() {
		if b == nil {
			return
		}
		if lg != nil {
			lg.Debug("verifyBackendConsistency", zap.Bool("skipSafeRangeBucket", skipSafeRangeBucket))
		}
		b.BatchTx().LockOutsideApply()
		defer b.BatchTx().Unlock()
		b.ReadTx().RLock()
		defer b.ReadTx().RUnlock()
		for _, bkt := range bucket {
			if skipSafeRangeBucket && bkt.IsSafeRangeBucket() {
				continue
			}
			unsafeVerifyTxConsistency(b, bkt)
		}
	})
}

func unsafeVerifyTxConsistency(b Backend, bucket Bucket) {
	dataFromWriteTxn := map[string]string{}
	b.BatchTx().UnsafeForEach(bucket, func(k, v []byte) error {
		dataFromWriteTxn[string(k)] = string(v)
		return nil
	})
	dataFromReadTxn := map[string]string{}
	b.ReadTx().UnsafeForEach(bucket, func(k, v []byte) error {
		dataFromReadTxn[string(k)] = string(v)
		return nil
	})
	if diff := cmp.Diff(dataFromWriteTxn, dataFromReadTxn); diff != "" {
		panic(fmt.Sprintf("bucket %s data mismatch\nwrite TXN: %v\nread TXN: %v\ndiff: %s", bucket.String(), dataFromWriteTxn, dataFromReadTxn, diff))
	}
}
