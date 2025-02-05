// Copyright 2016-2022 The Linux Foundation
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

package v1

import digest "github.com/opencontainers/go-digest"

// Descriptor describes the disposition of targeted content.
// This structure provides `application/vnd.oci.descriptor.v1+json` mediatype
// when marshalled to JSON.
type Descriptor struct {
	// MediaType is the media type of the object this schema refers to.
	MediaType string `json:"mediaType"`

	// Digest is the digest of the targeted content.
	Digest digest.Digest `json:"digest"`

	// Size specifies the size in bytes of the blob.
	Size int64 `json:"size"`

	// URLs specifies a list of URLs from which this object MAY be downloaded
	URLs []string `json:"urls,omitempty"`

	// Annotations contains arbitrary metadata relating to the targeted content.
	Annotations map[string]string `json:"annotations,omitempty"`

	// Data is an embedding of the targeted content. This is encoded as a base64
	// string when marshalled to JSON (automatically, by encoding/json). If
	// present, Data can be used directly to avoid fetching the targeted content.
	Data []byte `json:"data,omitempty"`

	// Platform describes the platform which the image in the manifest runs on.
	//
	// This should only be used when referring to a manifest.
	Platform *Platform `json:"platform,omitempty"`

	// ArtifactType is the IANA media type of this artifact.
	ArtifactType string `json:"artifactType,omitempty"`
}

// Platform describes the platform which the image in the manifest runs on.
type Platform struct {
	// Architecture field specifies the CPU architecture, for example
	// `amd64` or `ppc64le`.
	Architecture string `json:"architecture"`

	// OS specifies the operating system, for example `linux` or `windows`.
	OS string `json:"os"`

	// OSVersion is an optional field specifying the operating system
	// version, for example on Windows `10.0.14393.1066`.
	OSVersion string `json:"os.version,omitempty"`

	// OSFeatures is an optional field specifying an array of strings,
	// each listing a required OS feature (for example on Windows `win32k`).
	OSFeatures []string `json:"os.features,omitempty"`

	// Variant is an optional field specifying a variant of the CPU, for
	// example `v7` to specify ARMv7 when architecture is `arm`.
	Variant string `json:"variant,omitempty"`
}

// DescriptorEmptyJSON is the descriptor of a blob with content of `{}`.
var DescriptorEmptyJSON = Descriptor{
	MediaType: MediaTypeEmptyJSON,
	Digest:    `sha256:44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a`,
	Size:      2,
	Data:      []byte(`{}`),
}
