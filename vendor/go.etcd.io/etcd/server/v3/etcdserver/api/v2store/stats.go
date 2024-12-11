// Copyright 2015 The etcd Authors
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

package v2store

import (
	"encoding/json"
	"sync/atomic"
)

const (
	SetSuccess = iota
	SetFail
	DeleteSuccess
	DeleteFail
	CreateSuccess
	CreateFail
	UpdateSuccess
	UpdateFail
	CompareAndSwapSuccess
	CompareAndSwapFail
	GetSuccess
	GetFail
	ExpireCount
	CompareAndDeleteSuccess
	CompareAndDeleteFail
)

type Stats struct {
	// Number of get requests

	GetSuccess uint64 `json:"getsSuccess"`
	GetFail    uint64 `json:"getsFail"`

	// Number of sets requests

	SetSuccess uint64 `json:"setsSuccess"`
	SetFail    uint64 `json:"setsFail"`

	// Number of delete requests

	DeleteSuccess uint64 `json:"deleteSuccess"`
	DeleteFail    uint64 `json:"deleteFail"`

	// Number of update requests

	UpdateSuccess uint64 `json:"updateSuccess"`
	UpdateFail    uint64 `json:"updateFail"`

	// Number of create requests

	CreateSuccess uint64 `json:"createSuccess"`
	CreateFail    uint64 `json:"createFail"`

	// Number of testAndSet requests

	CompareAndSwapSuccess uint64 `json:"compareAndSwapSuccess"`
	CompareAndSwapFail    uint64 `json:"compareAndSwapFail"`

	// Number of compareAndDelete requests

	CompareAndDeleteSuccess uint64 `json:"compareAndDeleteSuccess"`
	CompareAndDeleteFail    uint64 `json:"compareAndDeleteFail"`

	ExpireCount uint64 `json:"expireCount"`

	Watchers uint64 `json:"watchers"`
}

func newStats() *Stats {
	s := new(Stats)
	return s
}

func (s *Stats) clone() *Stats {
	return &Stats{
		GetSuccess:              atomic.LoadUint64(&s.GetSuccess),
		GetFail:                 atomic.LoadUint64(&s.GetFail),
		SetSuccess:              atomic.LoadUint64(&s.SetSuccess),
		SetFail:                 atomic.LoadUint64(&s.SetFail),
		DeleteSuccess:           atomic.LoadUint64(&s.DeleteSuccess),
		DeleteFail:              atomic.LoadUint64(&s.DeleteFail),
		UpdateSuccess:           atomic.LoadUint64(&s.UpdateSuccess),
		UpdateFail:              atomic.LoadUint64(&s.UpdateFail),
		CreateSuccess:           atomic.LoadUint64(&s.CreateSuccess),
		CreateFail:              atomic.LoadUint64(&s.CreateFail),
		CompareAndSwapSuccess:   atomic.LoadUint64(&s.CompareAndSwapSuccess),
		CompareAndSwapFail:      atomic.LoadUint64(&s.CompareAndSwapFail),
		CompareAndDeleteSuccess: atomic.LoadUint64(&s.CompareAndDeleteSuccess),
		CompareAndDeleteFail:    atomic.LoadUint64(&s.CompareAndDeleteFail),
		ExpireCount:             atomic.LoadUint64(&s.ExpireCount),
		Watchers:                atomic.LoadUint64(&s.Watchers),
	}
}

func (s *Stats) toJSON() []byte {
	b, _ := json.Marshal(s)
	return b
}

func (s *Stats) Inc(field int) {
	switch field {
	case SetSuccess:
		atomic.AddUint64(&s.SetSuccess, 1)
	case SetFail:
		atomic.AddUint64(&s.SetFail, 1)
	case CreateSuccess:
		atomic.AddUint64(&s.CreateSuccess, 1)
	case CreateFail:
		atomic.AddUint64(&s.CreateFail, 1)
	case DeleteSuccess:
		atomic.AddUint64(&s.DeleteSuccess, 1)
	case DeleteFail:
		atomic.AddUint64(&s.DeleteFail, 1)
	case GetSuccess:
		atomic.AddUint64(&s.GetSuccess, 1)
	case GetFail:
		atomic.AddUint64(&s.GetFail, 1)
	case UpdateSuccess:
		atomic.AddUint64(&s.UpdateSuccess, 1)
	case UpdateFail:
		atomic.AddUint64(&s.UpdateFail, 1)
	case CompareAndSwapSuccess:
		atomic.AddUint64(&s.CompareAndSwapSuccess, 1)
	case CompareAndSwapFail:
		atomic.AddUint64(&s.CompareAndSwapFail, 1)
	case CompareAndDeleteSuccess:
		atomic.AddUint64(&s.CompareAndDeleteSuccess, 1)
	case CompareAndDeleteFail:
		atomic.AddUint64(&s.CompareAndDeleteFail, 1)
	case ExpireCount:
		atomic.AddUint64(&s.ExpireCount, 1)
	}
}
