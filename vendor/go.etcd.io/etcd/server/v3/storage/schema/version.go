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
	"github.com/coreos/go-semver/semver"

	"go.etcd.io/bbolt"
	"go.etcd.io/etcd/server/v3/storage/backend"
)

// ReadStorageVersion loads storage version from given backend transaction.
// Populated since v3.6
func ReadStorageVersion(tx backend.ReadTx) *semver.Version {
	tx.RLock()
	defer tx.RUnlock()
	return UnsafeReadStorageVersion(tx)
}

// UnsafeReadStorageVersion loads storage version from given backend transaction.
// Populated since v3.6
func UnsafeReadStorageVersion(tx backend.UnsafeReader) *semver.Version {
	_, vs := tx.UnsafeRange(Meta, MetaStorageVersionName, nil, 1)
	if len(vs) == 0 {
		return nil
	}
	v, err := semver.NewVersion(string(vs[0]))
	if err != nil {
		return nil
	}
	return v
}

// ReadStorageVersionFromSnapshot loads storage version from given bbolt transaction.
// Populated since v3.6
func ReadStorageVersionFromSnapshot(tx *bbolt.Tx) *semver.Version {
	v := tx.Bucket(Meta.Name()).Get(MetaStorageVersionName)
	version, err := semver.NewVersion(string(v))
	if err != nil {
		return nil
	}
	return version
}

// UnsafeSetStorageVersion updates etcd storage version in backend.
// Populated since v3.6
func UnsafeSetStorageVersion(tx backend.UnsafeWriter, v *semver.Version) {
	sv := semver.Version{Major: v.Major, Minor: v.Minor}
	tx.UnsafePut(Meta, MetaStorageVersionName, []byte(sv.String()))
}

// UnsafeClearStorageVersion removes etcd storage version in backend.
func UnsafeClearStorageVersion(tx backend.UnsafeWriter) {
	tx.UnsafeDelete(Meta, MetaStorageVersionName)
}
