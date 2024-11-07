#!/usr/bin/env bash

# Copyright 2017 The Kubernetes Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

set -o errexit
set -o nounset
set -o pipefail

# Short-circuit if protoc.sh has already been sourced
[[ $(type -t kube::protoc::loaded) == function ]] && return 0

# The root of the build/dist directory
KUBE_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)"
source "${KUBE_ROOT}/hack/lib/init.sh"

PROTOC_VERSION=23.4

# Generates $1/api.pb.go from the protobuf file $1/api.proto
# and formats it correctly
# $1: Full path to the directory where the api.proto file is
function kube::protoc::generate_proto() {
  kube::golang::setup_env
  go -C "${KUBE_ROOT}/hack/tools" install google.golang.org/protobuf/cmd/protoc-gen-go
  go -C "${KUBE_ROOT}/hack/tools" install google.golang.org/grpc/cmd/protoc-gen-go-grpc

  kube::protoc::check_protoc

  local package=${1}
  kube::protoc::protoc "${package}"
  kube::protoc::format "${package}"
}

# Generates $1/api.pb.go from the protobuf file $1/api.proto
# and formats it correctly
# $1: Full path to the directory where the api.proto file is
function kube::protoc::generate_proto_gogo() {
  kube::golang::setup_env
  GOPROXY=off go install k8s.io/code-generator/cmd/go-to-protobuf/protoc-gen-gogo

  kube::protoc::check_protoc

  local package=${1}
  kube::protoc::protoc_gogo "${package}"
  kube::protoc::format "${package}"
}

# Checks that the current protoc version matches the required version and
# exit 1 if it's not the case
function kube::protoc::check_protoc() {
  if [[ -z "$(which protoc)" || "$(protoc --version)" != "libprotoc ${PROTOC_VERSION}"* ]]; then
    echo "Generating protobuf requires protoc ${PROTOC_VERSION}."
    echo "Run hack/install-protoc.sh or download and install the"
    echo "platform-appropriate Protobuf package for your OS from"
    echo "https://github.com/protocolbuffers/protobuf/releases"
    return 1
  fi
}

# Generates $1/api.pb.go from the protobuf file $1/api.proto
# $1: Full path to the directory where the api.proto file is
function kube::protoc::protoc_gogo() {
  local package=${1}
  gogopath=$(dirname "$(kube::util::find-binary "protoc-gen-gogo")")

  (
    cd "${package}"

    # This invocation of --gogo_out produces its output in the current
    # directory (despite gogo docs saying it would be source-relative, it
    # isn't).  The inputs to this function do not all have a common root, so
    # this works best for all inputs.
    PATH="${gogopath}:${PATH}" protoc \
      --proto_path="$(pwd -P)" \
      --proto_path="${KUBE_ROOT}/vendor" \
      --proto_path="${KUBE_ROOT}/staging/src" \
      --proto_path="${KUBE_ROOT}/third_party/protobuf" \
      --gogo_out=paths=source_relative,plugins=grpc:. \
      api.proto
  )
}

# Generates $1/api.pb.go from the protobuf file $1/api.proto without using gogo
# $1: Full path to the directory where the api.proto file is
function kube::protoc::protoc() {
  local package=${1}

  protoc \
    --proto_path="$(pwd -P)" \
    --proto_path="${KUBE_ROOT}/vendor" \
    --proto_path="${KUBE_ROOT}/staging/src" \
    --proto_path="${KUBE_ROOT}/third_party/protobuf" \
    --go_out=. \
    --go_opt=paths=source_relative \
    --go-grpc_out=. \
    --go-grpc_opt=paths=source_relative \
    "${package}"/api.proto
}

# Formats $1/api.pb.go, adds the boilerplate comments and run gofmt on it
# $1: Full path to the directory where the api.proto file is
function kube::protoc::format() {
  local package=${1}

  # Run gofmt to clean up the generated code.
  kube::golang::setup_env

  # Update boilerplate for the generated files.
  for file in "${package}"/api*.pb.go ; do
    cat hack/boilerplate/boilerplate.generatego.txt "${file}" > tmpfile && mv tmpfile "${file}"
    gofmt -s -w "${file}"
  done
}

# Compares the contents of $1 and $2
# Echo's $3 in case of error and exits 1
function kube::protoc::diff() {
  local ret=0
  diff -I "gzipped FileDescriptorProto" -I "0x" -Naupr "${1}" "${2}" || ret=$?
  if [[ ${ret} -ne 0 ]]; then
    echo "${3}"
    exit 1
  fi
}

function kube::protoc::install() {
  # run in a subshell to isolate caller from directory changes
  (
    local os
    local arch
    local download_folder
    local download_file

    os=$(kube::util::host_os)
    arch=$(kube::util::host_arch)
    download_folder="protoc-v${PROTOC_VERSION}-${os}-${arch}"
    download_file="${download_folder}.zip"

    cd "${KUBE_ROOT}/third_party" || return 1
    if [[ $(readlink protoc) != "${download_folder}" ]]; then
      local url
      if [[ ${os} == "darwin" ]]; then
        # TODO: switch to universal binary when updating to 3.20+
        url="https://github.com/protocolbuffers/protobuf/releases/download/v${PROTOC_VERSION}/protoc-${PROTOC_VERSION}-osx-x86_64.zip"
      elif [[ ${os} == "linux" && ${arch} == "amd64" ]]; then
        url="https://github.com/protocolbuffers/protobuf/releases/download/v${PROTOC_VERSION}/protoc-${PROTOC_VERSION}-linux-x86_64.zip"
      elif [[ ${os} == "linux" && ${arch} == "arm64" ]]; then
        url="https://github.com/protocolbuffers/protobuf/releases/download/v${PROTOC_VERSION}/protoc-${PROTOC_VERSION}-linux-aarch_64.zip"
      else
        kube::log::info "This install script does not support ${os}/${arch}"
        return 1
      fi
      kube::util::download_file "${url}" "${download_file}"
      unzip -o "${download_file}" -d "${download_folder}"
      ln -fns "${download_folder}" protoc
      mv protoc/bin/protoc protoc/protoc
      chmod -R +rX protoc/protoc
      rm -fr protoc/include
      rm "${download_file}"
    fi
    kube::log::info "protoc v${PROTOC_VERSION} installed. To use:"
    kube::log::info "export PATH=\"$(pwd)/protoc:\${PATH}\""
  )
}

# Marker function to indicate protoc.sh has been fully sourced
kube::protoc::loaded() {
  return 0
}
