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

import (
	"time"

	digest "github.com/opencontainers/go-digest"
)

// ImageConfig defines the execution parameters which should be used as a base when running a container using an image.
type ImageConfig struct {
	// User defines the username or UID which the process in the container should run as.
	User string `json:"User,omitempty"`

	// ExposedPorts a set of ports to expose from a container running this image.
	ExposedPorts map[string]struct{} `json:"ExposedPorts,omitempty"`

	// Env is a list of environment variables to be used in a container.
	Env []string `json:"Env,omitempty"`

	// Entrypoint defines a list of arguments to use as the command to execute when the container starts.
	Entrypoint []string `json:"Entrypoint,omitempty"`

	// Cmd defines the default arguments to the entrypoint of the container.
	Cmd []string `json:"Cmd,omitempty"`

	// Volumes is a set of directories describing where the process is likely write data specific to a container instance.
	Volumes map[string]struct{} `json:"Volumes,omitempty"`

	// WorkingDir sets the current working directory of the entrypoint process in the container.
	WorkingDir string `json:"WorkingDir,omitempty"`

	// Labels contains arbitrary metadata for the container.
	Labels map[string]string `json:"Labels,omitempty"`

	// StopSignal contains the system call signal that will be sent to the container to exit.
	StopSignal string `json:"StopSignal,omitempty"`

	// ArgsEscaped
	//
	// Deprecated: This field is present only for legacy compatibility with
	// Docker and should not be used by new image builders.  It is used by Docker
	// for Windows images to indicate that the `Entrypoint` or `Cmd` or both,
	// contains only a single element array, that is a pre-escaped, and combined
	// into a single string `CommandLine`. If `true` the value in `Entrypoint` or
	// `Cmd` should be used as-is to avoid double escaping.
	// https://github.com/opencontainers/image-spec/pull/892
	ArgsEscaped bool `json:"ArgsEscaped,omitempty"`
}

// RootFS describes a layer content addresses
type RootFS struct {
	// Type is the type of the rootfs.
	Type string `json:"type"`

	// DiffIDs is an array of layer content hashes (DiffIDs), in order from bottom-most to top-most.
	DiffIDs []digest.Digest `json:"diff_ids"`
}

// History describes the history of a layer.
type History struct {
	// Created is the combined date and time at which the layer was created, formatted as defined by RFC 3339, section 5.6.
	Created *time.Time `json:"created,omitempty"`

	// CreatedBy is the command which created the layer.
	CreatedBy string `json:"created_by,omitempty"`

	// Author is the author of the build point.
	Author string `json:"author,omitempty"`

	// Comment is a custom message set when creating the layer.
	Comment string `json:"comment,omitempty"`

	// EmptyLayer is used to mark if the history item created a filesystem diff.
	EmptyLayer bool `json:"empty_layer,omitempty"`
}

// Image is the JSON structure which describes some basic information about the image.
// This provides the `application/vnd.oci.image.config.v1+json` mediatype when marshalled to JSON.
type Image struct {
	// Created is the combined date and time at which the image was created, formatted as defined by RFC 3339, section 5.6.
	Created *time.Time `json:"created,omitempty"`

	// Author defines the name and/or email address of the person or entity which created and is responsible for maintaining the image.
	Author string `json:"author,omitempty"`

	// Platform describes the platform which the image in the manifest runs on.
	Platform

	// Config defines the execution parameters which should be used as a base when running a container using the image.
	Config ImageConfig `json:"config,omitempty"`

	// RootFS references the layer content addresses used by the image.
	RootFS RootFS `json:"rootfs"`

	// History describes the history of each layer.
	History []History `json:"history,omitempty"`
}
