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

package auth

import (
	"go.uber.org/zap"

	"go.etcd.io/etcd/api/v3/authpb"
	"go.etcd.io/etcd/pkg/v3/adt"
)

func getMergedPerms(tx UnsafeAuthReader, userName string) *unifiedRangePermissions {
	user := tx.UnsafeGetUser(userName)
	if user == nil {
		return nil
	}

	readPerms := adt.NewIntervalTree()
	writePerms := adt.NewIntervalTree()

	for _, roleName := range user.Roles {
		role := tx.UnsafeGetRole(roleName)
		if role == nil {
			continue
		}

		for _, perm := range role.KeyPermission {
			var ivl adt.Interval
			var rangeEnd []byte

			if len(perm.RangeEnd) != 1 || perm.RangeEnd[0] != 0 {
				rangeEnd = perm.RangeEnd
			}

			if len(perm.RangeEnd) != 0 {
				ivl = adt.NewBytesAffineInterval(perm.Key, rangeEnd)
			} else {
				ivl = adt.NewBytesAffinePoint(perm.Key)
			}

			switch perm.PermType {
			case authpb.READWRITE:
				readPerms.Insert(ivl, struct{}{})
				writePerms.Insert(ivl, struct{}{})

			case authpb.READ:
				readPerms.Insert(ivl, struct{}{})

			case authpb.WRITE:
				writePerms.Insert(ivl, struct{}{})
			}
		}
	}

	return &unifiedRangePermissions{
		readPerms:  readPerms,
		writePerms: writePerms,
	}
}

func checkKeyInterval(
	lg *zap.Logger,
	cachedPerms *unifiedRangePermissions,
	key, rangeEnd []byte,
	permtyp authpb.Permission_Type) bool {
	if isOpenEnded(rangeEnd) {
		rangeEnd = nil
		// nil rangeEnd will be converetd to []byte{}, the largest element of BytesAffineComparable,
		// in NewBytesAffineInterval().
	}

	ivl := adt.NewBytesAffineInterval(key, rangeEnd)
	switch permtyp {
	case authpb.READ:
		return cachedPerms.readPerms.Contains(ivl)
	case authpb.WRITE:
		return cachedPerms.writePerms.Contains(ivl)
	default:
		lg.Panic("unknown auth type", zap.String("auth-type", permtyp.String()))
	}
	return false
}

func checkKeyPoint(lg *zap.Logger, cachedPerms *unifiedRangePermissions, key []byte, permtyp authpb.Permission_Type) bool {
	pt := adt.NewBytesAffinePoint(key)
	switch permtyp {
	case authpb.READ:
		return cachedPerms.readPerms.Intersects(pt)
	case authpb.WRITE:
		return cachedPerms.writePerms.Intersects(pt)
	default:
		lg.Panic("unknown auth type", zap.String("auth-type", permtyp.String()))
	}
	return false
}

func (as *authStore) isRangeOpPermitted(userName string, key, rangeEnd []byte, permtyp authpb.Permission_Type) bool {
	// assumption: tx is Lock()ed
	as.rangePermCacheMu.RLock()
	defer as.rangePermCacheMu.RUnlock()

	rangePerm, ok := as.rangePermCache[userName]
	if !ok {
		as.lg.Error(
			"user doesn't exist",
			zap.String("user-name", userName),
		)
		return false
	}

	if len(rangeEnd) == 0 {
		return checkKeyPoint(as.lg, rangePerm, key, permtyp)
	}

	return checkKeyInterval(as.lg, rangePerm, key, rangeEnd, permtyp)
}

func (as *authStore) refreshRangePermCache(tx UnsafeAuthReader) {
	// Note that every authentication configuration update calls this method and it invalidates the entire
	// rangePermCache and reconstruct it based on information of users and roles stored in the backend.
	// This can be a costly operation.
	as.rangePermCacheMu.Lock()
	defer as.rangePermCacheMu.Unlock()

	as.lg.Debug("Refreshing rangePermCache")

	as.rangePermCache = make(map[string]*unifiedRangePermissions)

	users := tx.UnsafeGetAllUsers()
	for _, user := range users {
		userName := string(user.Name)
		perms := getMergedPerms(tx, userName)
		if perms == nil {
			as.lg.Error(
				"failed to create a merged permission",
				zap.String("user-name", userName),
			)
			continue
		}
		as.rangePermCache[userName] = perms
	}
}

type unifiedRangePermissions struct {
	readPerms  adt.IntervalTree
	writePerms adt.IntervalTree
}

// Constraints related to key range
// Assumptions:
// a1. key must be non-nil
// a2. []byte{} (in the case of string, "") is not a valid key of etcd
// For representing an open-ended range, BytesAffineComparable uses []byte{} as the largest element.
// a3. []byte{0x00} is the minimum valid etcd key
//
// Based on the above assumptions, key and rangeEnd must follow below rules:
// b1. for representing a single key point, rangeEnd should be nil or zero length byte array (in the case of string, "")
// Rule a2 guarantees that (X, []byte{}) for any X is not a valid range. So such ranges can be used for representing
// a single key permission.
//
// b2. key range with upper limit, like (X, Y), larger or equal to X and smaller than Y
//
// b3. key range with open-ended, like (X, <open ended>), is represented like (X, []byte{0x00})
// Because of rule a3, if we have (X, []byte{0x00}), such a range represents an empty range and makes no sense to have
// such a permission. So we use []byte{0x00} for representing an open-ended permission.
// Note that rangeEnd with []byte{0x00} will be converted into []byte{} before inserted into the interval tree
// (rule a2 ensures that this is the largest element).
// Special range like key = []byte{0x00} and rangeEnd = []byte{0x00} is treated as a range which matches with all keys.
//
// Treating a range whose rangeEnd with []byte{0x00} as an open-ended comes from the rules of Range() and Watch() API.

func isOpenEnded(rangeEnd []byte) bool { // check rule b3
	return len(rangeEnd) == 1 && rangeEnd[0] == 0
}

func isValidPermissionRange(key, rangeEnd []byte) bool {
	if len(key) == 0 {
		return false
	}
	if len(rangeEnd) == 0 { // ensure rule b1
		return true
	}

	begin := adt.BytesAffineComparable(key)
	end := adt.BytesAffineComparable(rangeEnd)
	if begin.Compare(end) == -1 { // rule b2
		return true
	}

	return isOpenEnded(rangeEnd)
}
