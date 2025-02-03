#!/usr/bin/env bash

# Copyright 2020 The Kubernetes Authors.
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

# This script vets each package by `levee`.
# Usage: `hack/verify-govet-levee.sh`.

set -o errexit
set -o pipefail

KUBE_ROOT=$(dirname "${BASH_SOURCE[0]}")/..
source "${KUBE_ROOT}/hack/lib/init.sh"
source "${KUBE_ROOT}/hack/lib/util.sh"

kube::golang::verify_go_version

# Ensure that we find the binaries we build before anything else.
export GOBIN="${KUBE_OUTPUT_BINPATH}"
PATH="${GOBIN}:${PATH}"

# Install levee
(
  cd "${KUBE_ROOT}/hack/tools"
  # override only for installing tools (in subshell), if set
  if [ -n "${KUBE_HACK_TOOLS_GOTOOLCHAIN+x}" ]; then
    export GOTOOLCHAIN="${KUBE_HACK_TOOLS_GOTOOLCHAIN}";
  fi
  GO111MODULE=on go install github.com/google/go-flow-levee/cmd/levee
)

# Prefer full path for interaction with make vet
LEVEE_BIN="$(which levee)"
CONFIG_FILE="${KUBE_ROOT}/hack/testdata/levee/levee-config.yaml"

# Do not run on third_party directories or generated client code or build tools.
targets=()
while IFS='' read -r line; do
  targets+=("${line}")
done < <(go list --find -e ./... | grep -E -v "/(build|third_party|vendor|staging|clientset_generated|hack)/")

go vet -vettool="${LEVEE_BIN}" -config="${CONFIG_FILE}" "${targets[@]}"
