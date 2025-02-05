/*
   Copyright The containerd Authors.

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

package types

import oci "github.com/opencontainers/image-spec/specs-go/v1"

// OCIPlatformToProto converts from a slice of OCI [specs.Platform] to a
// slice of the protobuf definition [Platform].
func OCIPlatformToProto(platforms []oci.Platform) []*Platform {
	ap := make([]*Platform, len(platforms))
	for i := range platforms {
		ap[i] = &Platform{
			OS:           platforms[i].OS,
			OSVersion:    platforms[i].OSVersion,
			Architecture: platforms[i].Architecture,
			Variant:      platforms[i].Variant,
		}
	}
	return ap
}

// OCIPlatformFromProto converts a slice of the protobuf definition [Platform]
// to a slice of OCI [specs.Platform].
func OCIPlatformFromProto(platforms []*Platform) []oci.Platform {
	op := make([]oci.Platform, len(platforms))
	for i := range platforms {
		op[i] = oci.Platform{
			OS:           platforms[i].OS,
			OSVersion:    platforms[i].OSVersion,
			Architecture: platforms[i].Architecture,
			Variant:      platforms[i].Variant,
		}
	}
	return op
}
