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

package walpb

import (
	"errors"
	"fmt"
)

var (
	ErrCRCMismatch = errors.New("walpb: crc mismatch")
)

func (rec *Record) Validate(crc uint32) error {
	if rec.Crc == crc {
		return nil
	}
	return fmt.Errorf("%w: expected: %x computed: %x", ErrCRCMismatch, rec.Crc, crc)
}

// ValidateSnapshotForWrite ensures the Snapshot the newly written snapshot is valid.
//
// There might exist log-entries written by old etcd versions that does not conform
// to the requirements.
func ValidateSnapshotForWrite(e *Snapshot) error {
	// Since etcd>=3.5.0
	if e.ConfState == nil && e.Index > 0 {
		return errors.New("Saved (not-initial) snapshot is missing ConfState: " + e.String())
	}
	return nil
}
