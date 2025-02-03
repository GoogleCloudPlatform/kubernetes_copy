#!/usr/bin/env bash

# Copyright 2019 The Kubernetes Authors.
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

# This script verifies that dependencies are up-to-date across different files
# Usage: `hack/verify-external-dependencies-version.sh`.

set -o errexit
set -o nounset
set -o pipefail

KUBE_ROOT=$(dirname "${BASH_SOURCE[0]}")/..
source "${KUBE_ROOT}/hack/lib/init.sh"

kube::golang::verify_go_version

# Ensure that we find the binaries we build before anything else.
export GOBIN="${KUBE_OUTPUT_BINPATH}"
PATH="${GOBIN}:${PATH}"

# Install zeitgeist
(
  cd "${KUBE_ROOT}/hack/tools"
  # override only for installing tools (in subshell), if set
  if [ -n "${KUBE_HACK_TOOLS_GOTOOLCHAIN+x}" ]; then
    export GOTOOLCHAIN="${KUBE_HACK_TOOLS_GOTOOLCHAIN}";
  fi
  GO111MODULE=on go install sigs.k8s.io/zeitgeist@v0.2.0
)

# Prefer full path for running zeitgeist
ZEITGEIST_BIN="$(which zeitgeist)"

# TODO: revert sed hack when zetigeist respects CLICOLOR/ttys
CLICOLOR=0 "${ZEITGEIST_BIN}" validate \
  --local \
  --base-path "${KUBE_ROOT}" \
  --config "${KUBE_ROOT}"/build/dependencies.yaml \
  2> >(sed -e $'s/\x1b\[[0-9;]*m//g' >&2)
