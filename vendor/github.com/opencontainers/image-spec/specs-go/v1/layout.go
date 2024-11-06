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
	// ImageLayoutFile is the file name containing ImageLayout in an OCI Image Layout
	ImageLayoutFile = "oci-layout"
	// ImageLayoutVersion is the version of ImageLayout
	ImageLayoutVersion = "1.0.0"
	// ImageIndexFile is the file name of the entry point for references and descriptors in an OCI Image Layout
	ImageIndexFile = "index.json"
	// ImageBlobsDir is the directory name containing content addressable blobs in an OCI Image Layout
	ImageBlobsDir = "blobs"
)

// ImageLayout is the structure in the "oci-layout" file, found in the root
// of an OCI Image-layout directory.
type ImageLayout struct {
	Version string `json:"imageLayoutVersion"`
}
