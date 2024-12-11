// Copyright 2021 The etcd Authors
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

package schema

import (
	"go.uber.org/zap"

	"go.etcd.io/etcd/server/v3/storage/backend"
)

type action interface {
	// unsafeDo executes the action and returns revert action, when executed
	// should restore the state from before.
	unsafeDo(tx backend.UnsafeReadWriter) (revert action, err error)
}

type setKeyAction struct {
	Bucket     backend.Bucket
	FieldName  []byte
	FieldValue []byte
}

func (a setKeyAction) unsafeDo(tx backend.UnsafeReadWriter) (action, error) {
	revert := restoreFieldValueAction(tx, a.Bucket, a.FieldName)
	tx.UnsafePut(a.Bucket, a.FieldName, a.FieldValue)
	return revert, nil
}

type deleteKeyAction struct {
	Bucket    backend.Bucket
	FieldName []byte
}

func (a deleteKeyAction) unsafeDo(tx backend.UnsafeReadWriter) (action, error) {
	revert := restoreFieldValueAction(tx, a.Bucket, a.FieldName)
	tx.UnsafeDelete(a.Bucket, a.FieldName)
	return revert, nil
}

func restoreFieldValueAction(tx backend.UnsafeReader, bucket backend.Bucket, fieldName []byte) action {
	_, vs := tx.UnsafeRange(bucket, fieldName, nil, 1)
	if len(vs) == 1 {
		return &setKeyAction{
			Bucket:     bucket,
			FieldName:  fieldName,
			FieldValue: vs[0],
		}
	}
	return &deleteKeyAction{
		Bucket:    bucket,
		FieldName: fieldName,
	}
}

type ActionList []action

// unsafeExecute executes actions one by one. If one of actions returns error,
// it will revert them.
func (as ActionList) unsafeExecute(lg *zap.Logger, tx backend.UnsafeReadWriter) error {
	var revertActions = make(ActionList, 0, len(as))
	for _, a := range as {
		revert, err := a.unsafeDo(tx)

		if err != nil {
			revertActions.unsafeExecuteInReversedOrder(lg, tx)
			return err
		}
		revertActions = append(revertActions, revert)
	}
	return nil
}

// unsafeExecuteInReversedOrder executes actions in revered order. Will panic on
// action error. Should be used when reverting.
func (as ActionList) unsafeExecuteInReversedOrder(lg *zap.Logger, tx backend.UnsafeReadWriter) {
	for j := len(as) - 1; j >= 0; j-- {
		_, err := as[j].unsafeDo(tx)
		if err != nil {
			lg.Panic("Cannot recover from revert error", zap.Error(err))
		}
	}
}
