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

package config

type V2DeprecationEnum string

const (
	// No longer supported in v3.6
	V2Depr0NotYet = V2DeprecationEnum("not-yet")
	// No longer supported in v3.6
	//
	// Deprecated: Please use V2Depr0NotYet.
	//revive:disable-next-line:var-naming
	V2_DEPR_0_NOT_YET = V2Depr0NotYet
	// Default in v3.6.  Meaningful v2 state is not allowed.
	// The V2 files are maintained for v3.5 rollback.

	V2Depr1WriteOnly = V2DeprecationEnum("write-only")
	// Default in v3.6.  Meaningful v2 state is not allowed.
	// The V2 files are maintained for v3.5 rollback.
	//
	// Deprecated: Please use V2Depr1WriteOnly.
	//revive:disable-next-line:var-naming
	V2_DEPR_1_WRITE_ONLY = V2Depr1WriteOnly

	// V2store is WIPED if found !!!
	V2Depr1WriteOnlyDrop = V2DeprecationEnum("write-only-drop-data")
	// V2store is WIPED if found !!!
	//
	// Deprecated: Pleae use V2Depr1WriteOnlyDrop.
	//revive:disable-next-line:var-naming
	V2_DEPR_1_WRITE_ONLY_DROP = V2Depr1WriteOnlyDrop

	// V2store is neither written nor read. Usage of this configuration is blocking
	// ability to rollback to etcd v3.5.
	V2Depr2Gone = V2DeprecationEnum("gone")
	// V2store is neither written nor read. Usage of this configuration is blocking
	// ability to rollback to etcd v3.5.
	//
	// Deprecated: Please use V2Depr2Gone
	//revive:disable-next-line:var-naming
	V2_DEPR_2_GONE = V2Depr2Gone

	// Default deprecation level.
	V2DeprDefault = V2Depr1WriteOnly
	// Default deprecation level.
	//
	// Deprecated: Please use V2DeprDefault.
	//revive:disable-next-line:var-naming
	V2_DEPR_DEFAULT = V2DeprDefault
)

func (e V2DeprecationEnum) IsAtLeast(v2d V2DeprecationEnum) bool {
	return e.level() >= v2d.level()
}

func (e V2DeprecationEnum) level() int {
	switch e {
	case V2Depr0NotYet:
		return 0
	case V2Depr1WriteOnly:
		return 1
	case V2Depr1WriteOnlyDrop:
		return 2
	case V2Depr2Gone:
		return 3
	}
	panic("Unknown V2DeprecationEnum: " + e)
}
