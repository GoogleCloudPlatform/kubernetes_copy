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

package datadir

import "path/filepath"

const (
	memberDirSegment   = "member"
	snapDirSegment     = "snap"
	walDirSegment      = "wal"
	backendFileSegment = "db"
)

func ToBackendFileName(dataDir string) string {
	return filepath.Join(ToSnapDir(dataDir), backendFileSegment)
}

func ToSnapDir(dataDir string) string {
	return filepath.Join(ToMemberDir(dataDir), snapDirSegment)
}

// ToWalDir returns the directory path for the member's WAL.
//
// Deprecated: use ToWALDir instead.
//
//revive:disable-next-line:var-naming
func ToWalDir(dataDir string) string {
	return ToWALDir(dataDir)
}

func ToWALDir(dataDir string) string {
	return filepath.Join(ToMemberDir(dataDir), walDirSegment)
}

func ToMemberDir(dataDir string) string {
	return filepath.Join(dataDir, memberDirSegment)
}
