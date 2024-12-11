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

package backend

type HookFunc func(tx UnsafeReadWriter)

// Hooks allow to add additional logic executed during transaction lifetime.
type Hooks interface {
	// OnPreCommitUnsafe is executed before Commit of transactions.
	// The given transaction is already locked.
	OnPreCommitUnsafe(tx UnsafeReadWriter)
}

type hooks struct {
	onPreCommitUnsafe HookFunc
}

func (h hooks) OnPreCommitUnsafe(tx UnsafeReadWriter) {
	h.onPreCommitUnsafe(tx)
}

func NewHooks(onPreCommitUnsafe HookFunc) Hooks {
	return hooks{onPreCommitUnsafe: onPreCommitUnsafe}
}
