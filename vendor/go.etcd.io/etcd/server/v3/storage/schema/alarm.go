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

	"go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/server/v3/storage/backend"
)

type alarmBackend struct {
	lg *zap.Logger
	be backend.Backend
}

func NewAlarmBackend(lg *zap.Logger, be backend.Backend) *alarmBackend {
	return &alarmBackend{
		lg: lg,
		be: be,
	}
}

func (s *alarmBackend) CreateAlarmBucket() {
	tx := s.be.BatchTx()
	tx.LockOutsideApply()
	defer tx.Unlock()
	tx.UnsafeCreateBucket(Alarm)
}

func (s *alarmBackend) MustPutAlarm(alarm *etcdserverpb.AlarmMember) {
	tx := s.be.BatchTx()
	tx.LockInsideApply()
	defer tx.Unlock()
	s.mustUnsafePutAlarm(tx, alarm)
}

func (s *alarmBackend) mustUnsafePutAlarm(tx backend.UnsafeWriter, alarm *etcdserverpb.AlarmMember) {
	v, err := alarm.Marshal()
	if err != nil {
		s.lg.Panic("failed to marshal alarm member", zap.Error(err))
	}

	tx.UnsafePut(Alarm, v, nil)
}

func (s *alarmBackend) MustDeleteAlarm(alarm *etcdserverpb.AlarmMember) {
	tx := s.be.BatchTx()
	tx.LockInsideApply()
	defer tx.Unlock()
	s.mustUnsafeDeleteAlarm(tx, alarm)
}

func (s *alarmBackend) mustUnsafeDeleteAlarm(tx backend.UnsafeWriter, alarm *etcdserverpb.AlarmMember) {
	v, err := alarm.Marshal()
	if err != nil {
		s.lg.Panic("failed to marshal alarm member", zap.Error(err))
	}

	tx.UnsafeDelete(Alarm, v)
}

func (s *alarmBackend) GetAllAlarms() ([]*etcdserverpb.AlarmMember, error) {
	tx := s.be.ReadTx()
	tx.RLock()
	defer tx.RUnlock()
	return s.unsafeGetAllAlarms(tx)
}

func (s *alarmBackend) unsafeGetAllAlarms(tx backend.UnsafeReader) ([]*etcdserverpb.AlarmMember, error) {
	var ms []*etcdserverpb.AlarmMember
	err := tx.UnsafeForEach(Alarm, func(k, v []byte) error {
		var m etcdserverpb.AlarmMember
		if err := m.Unmarshal(k); err != nil {
			return err
		}
		ms = append(ms, &m)
		return nil
	})
	return ms, err
}

func (s alarmBackend) ForceCommit() {
	s.be.ForceCommit()
}
