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

package v1

const (
	// MediaTypeDescriptor specifies the media type for a content descriptor.
	MediaTypeDescriptor = "application/vnd.oci.descriptor.v1+json"

	// MediaTypeLayoutHeader specifies the media type for the oci-layout.
	MediaTypeLayoutHeader = "application/vnd.oci.layout.header.v1+json"

	// MediaTypeImageIndex specifies the media type for an image index.
	MediaTypeImageIndex = "application/vnd.oci.image.index.v1+json"

	// MediaTypeImageManifest specifies the media type for an image manifest.
	MediaTypeImageManifest = "application/vnd.oci.image.manifest.v1+json"

	// MediaTypeImageConfig specifies the media type for the image configuration.
	MediaTypeImageConfig = "application/vnd.oci.image.config.v1+json"

	// MediaTypeEmptyJSON specifies the media type for an unused blob containing the value "{}".
	MediaTypeEmptyJSON = "application/vnd.oci.empty.v1+json"
)

const (
	// MediaTypeImageLayer is the media type used for layers referenced by the manifest.
	MediaTypeImageLayer = "application/vnd.oci.image.layer.v1.tar"

	// MediaTypeImageLayerGzip is the media type used for gzipped layers
	// referenced by the manifest.
	MediaTypeImageLayerGzip = "application/vnd.oci.image.layer.v1.tar+gzip"

	// MediaTypeImageLayerZstd is the media type used for zstd compressed
	// layers referenced by the manifest.
	MediaTypeImageLayerZstd = "application/vnd.oci.image.layer.v1.tar+zstd"
)

// Non-distributable layer media-types.
//
// Deprecated: Non-distributable layers are deprecated, and not recommended
// for future use. Implementations SHOULD NOT produce new non-distributable
// layers.
// https://github.com/opencontainers/image-spec/pull/965
const (
	// MediaTypeImageLayerNonDistributable is the media type for layers referenced by
	// the manifest but with distribution restrictions.
	//
	// Deprecated: Non-distributable layers are deprecated, and not recommended
	// for future use. Implementations SHOULD NOT produce new non-distributable
	// layers.
	// https://github.com/opencontainers/image-spec/pull/965
	MediaTypeImageLayerNonDistributable = "application/vnd.oci.image.layer.nondistributable.v1.tar"

	// MediaTypeImageLayerNonDistributableGzip is the media type for
	// gzipped layers referenced by the manifest but with distribution
	// restrictions.
	//
	// Deprecated: Non-distributable layers are deprecated, and not recommended
	// for future use. Implementations SHOULD NOT produce new non-distributable
	// layers.
	// https://github.com/opencontainers/image-spec/pull/965
	MediaTypeImageLayerNonDistributableGzip = "application/vnd.oci.image.layer.nondistributable.v1.tar+gzip"

	// MediaTypeImageLayerNonDistributableZstd is the media type for zstd
	// compressed layers referenced by the manifest but with distribution
	// restrictions.
	//
	// Deprecated: Non-distributable layers are deprecated, and not recommended
	// for future use. Implementations SHOULD NOT produce new non-distributable
	// layers.
	// https://github.com/opencontainers/image-spec/pull/965
	MediaTypeImageLayerNonDistributableZstd = "application/vnd.oci.image.layer.nondistributable.v1.tar+zstd"
)
