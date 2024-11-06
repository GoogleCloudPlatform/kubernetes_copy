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
	// AnnotationCreated is the annotation key for the date and time on which the image was built (date-time string as defined by RFC 3339).
	AnnotationCreated = "org.opencontainers.image.created"

	// AnnotationAuthors is the annotation key for the contact details of the people or organization responsible for the image (freeform string).
	AnnotationAuthors = "org.opencontainers.image.authors"

	// AnnotationURL is the annotation key for the URL to find more information on the image.
	AnnotationURL = "org.opencontainers.image.url"

	// AnnotationDocumentation is the annotation key for the URL to get documentation on the image.
	AnnotationDocumentation = "org.opencontainers.image.documentation"

	// AnnotationSource is the annotation key for the URL to get source code for building the image.
	AnnotationSource = "org.opencontainers.image.source"

	// AnnotationVersion is the annotation key for the version of the packaged software.
	// The version MAY match a label or tag in the source code repository.
	// The version MAY be Semantic versioning-compatible.
	AnnotationVersion = "org.opencontainers.image.version"

	// AnnotationRevision is the annotation key for the source control revision identifier for the packaged software.
	AnnotationRevision = "org.opencontainers.image.revision"

	// AnnotationVendor is the annotation key for the name of the distributing entity, organization or individual.
	AnnotationVendor = "org.opencontainers.image.vendor"

	// AnnotationLicenses is the annotation key for the license(s) under which contained software is distributed as an SPDX License Expression.
	AnnotationLicenses = "org.opencontainers.image.licenses"

	// AnnotationRefName is the annotation key for the name of the reference for a target.
	// SHOULD only be considered valid when on descriptors on `index.json` within image layout.
	AnnotationRefName = "org.opencontainers.image.ref.name"

	// AnnotationTitle is the annotation key for the human-readable title of the image.
	AnnotationTitle = "org.opencontainers.image.title"

	// AnnotationDescription is the annotation key for the human-readable description of the software packaged in the image.
	AnnotationDescription = "org.opencontainers.image.description"

	// AnnotationBaseImageDigest is the annotation key for the digest of the image's base image.
	AnnotationBaseImageDigest = "org.opencontainers.image.base.digest"

	// AnnotationBaseImageName is the annotation key for the image reference of the image's base image.
	AnnotationBaseImageName = "org.opencontainers.image.base.name"
)
