// Copyright 2016 The Linux Foundation
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

package specs

import "fmt"

const (
	// VersionMajor is for an API incompatible changes
	VersionMajor = 1
	// VersionMinor is for functionality in a backwards-compatible manner
	VersionMinor = 1
	// VersionPatch is for backwards-compatible bug fixes
	VersionPatch = 0

	// VersionDev indicates development branch. Releases will be empty string.
	VersionDev = ""
)

// Version is the specification version that the package types support.
var Version = fmt.Sprintf("%d.%d.%d%s", VersionMajor, VersionMinor, VersionPatch, VersionDev)
