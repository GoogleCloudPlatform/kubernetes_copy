// Copyright 2017 The etcd Authors
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

//go:build windows

package backend

import bolt "go.etcd.io/bbolt"

var boltOpenOptions *bolt.Options = nil

// setting mmap size != 0 on windows will allocate the entire
// mmap size for the file, instead of growing it. So, force 0.

func (bcfg *BackendConfig) mmapSize() int { return 0 }
