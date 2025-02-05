#!/usr/bin/env bash

# Copyright 2023 The Kubernetes Authors.
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

# This script checks that golangci-strict.yaml and golangci.yaml remain in
# sync. Lines that are intentionally different must have a comment which
# mentions golangci.yaml or golangci-lint.yaml.

set -o errexit
set -o nounset
set -o pipefail

KUBE_ROOT=$(dirname "${BASH_SOURCE[0]}")/..
source "${KUBE_ROOT}/hack/lib/init.sh"

# This sets up the environment, like GOCACHE, which keeps the worktree cleaner.
kube::golang::setup_env

# Remove all files, some of them might be obsolete.
rm -f hack/golangci*.yaml

generate () {
    out="$1"
    shift
    echo "Generating $out from hack/golangci.yaml.in with ./cmd/gotemplate $*"
    go run ./cmd/gotemplate <hack/golangci.yaml.in >"${out}" "$@"
}

# Regenerate.
generate hack/golangci.yaml Base=1
generate hack/golangci-hints.yaml Hints=1
