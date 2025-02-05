/*
Copyright 2019 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package resourcelock

import (
	"bytes"
	"context"
	"encoding/json"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

const (
	UnknownLeader = "leaderelection.k8s.io/unknown"
)

// MultiLock is used for lock's migration
type MultiLock struct {
	Primary   Interface
	Secondary Interface
}

// Get returns the older election record of the lock
func (ml *MultiLock) Get(ctx context.Context) (*LeaderElectionRecord, []byte, error) {
	primary, primaryRaw, err := ml.Primary.Get(ctx)
	if err != nil {
		return nil, nil, err
	}

	secondary, secondaryRaw, err := ml.Secondary.Get(ctx)
	if err != nil {
		// Lock is held by old client
		if apierrors.IsNotFound(err) && primary.HolderIdentity != ml.Identity() {
			return primary, primaryRaw, nil
		}
		return nil, nil, err
	}

	if primary.HolderIdentity != secondary.HolderIdentity {
		primary.HolderIdentity = UnknownLeader
		primaryRaw, err = json.Marshal(primary)
		if err != nil {
			return nil, nil, err
		}
	}
	return primary, ConcatRawRecord(primaryRaw, secondaryRaw), nil
}

// TODO(linxiulei): support MultiLock
// GetFromCache returns the older election record of the lock
func (ml *MultiLock) GetFromCache(ctx context.Context) (*LeaderElectionRecord, []byte, error) {
	return ml.Get(ctx)
}

// Create attempts to create both primary lock and secondary lock
func (ml *MultiLock) Create(ctx context.Context, ler LeaderElectionRecord) error {
	err := ml.Primary.Create(ctx, ler)
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	return ml.Secondary.Create(ctx, ler)
}

// Update will update and existing annotation on both two resources.
func (ml *MultiLock) Update(ctx context.Context, ler LeaderElectionRecord) error {
	err := ml.Primary.Update(ctx, ler)
	if err != nil {
		return err
	}
	_, _, err = ml.Secondary.Get(ctx)
	if err != nil && apierrors.IsNotFound(err) {
		return ml.Secondary.Create(ctx, ler)
	}
	return ml.Secondary.Update(ctx, ler)
}

// RecordEvent in leader election while adding meta-data
func (ml *MultiLock) RecordEvent(s string) {
	ml.Primary.RecordEvent(s)
	ml.Secondary.RecordEvent(s)
}

// Describe is used to convert details on current resource lock
// into a string
func (ml *MultiLock) Describe() string {
	return ml.Primary.Describe()
}

// Identity returns the Identity of the lock
func (ml *MultiLock) Identity() string {
	return ml.Primary.Identity()
}

// TODO(linxiulei): support MultiLock
// StartSync
func (ml *MultiLock) StartSync(_ context.Context) {
}

func ConcatRawRecord(primaryRaw, secondaryRaw []byte) []byte {
	return bytes.Join([][]byte{primaryRaw, secondaryRaw}, []byte(","))
}
