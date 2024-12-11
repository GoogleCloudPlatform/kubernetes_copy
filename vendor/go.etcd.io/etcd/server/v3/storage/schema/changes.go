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

import "go.etcd.io/etcd/server/v3/storage/backend"

type schemaChange interface {
	upgradeAction() action
	downgradeAction() action
}

// addNewField represents adding new field when upgrading. Downgrade will remove the field.
func addNewField(bucket backend.Bucket, fieldName []byte, fieldValue []byte) schemaChange {
	return simpleSchemaChange{
		upgrade: setKeyAction{
			Bucket:     bucket,
			FieldName:  fieldName,
			FieldValue: fieldValue,
		},
		downgrade: deleteKeyAction{
			Bucket:    bucket,
			FieldName: fieldName,
		},
	}
}

type simpleSchemaChange struct {
	upgrade   action
	downgrade action
}

func (c simpleSchemaChange) upgradeAction() action {
	return c.upgrade
}

func (c simpleSchemaChange) downgradeAction() action {
	return c.downgrade
}
