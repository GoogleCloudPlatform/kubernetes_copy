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

//go:build !windows

package fileutil

import "os"

const (
	// PrivateDirMode grants owner to make/remove files inside the directory.
	PrivateDirMode = 0o700
)

// OpenDir opens a directory for syncing.
func OpenDir(path string) (*os.File, error) { return os.Open(path) }
