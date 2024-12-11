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

package version

import "errors"

var (
	ErrInvalidDowngradeTargetVersion = errors.New("etcdserver: invalid downgrade target version")
	ErrDowngradeInProcess            = errors.New("etcdserver: cluster has a downgrade job in progress")
	ErrNoInflightDowngrade           = errors.New("etcdserver: no inflight downgrade job")
)
