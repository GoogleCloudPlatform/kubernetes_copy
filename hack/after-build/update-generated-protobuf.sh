#!/bin/bash

# Copyright 2015 The Kubernetes Authors All rights reserved.
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

KUBE_ROOT=$(dirname "${BASH_SOURCE}")/../..
source "${KUBE_ROOT}/hack/lib/init.sh"

gotoprotobuf=$(kube::util::find-binary "go-to-protobuf")

# requires the 'proto' tag to build (will remove when ready)
# searches for the protoc-gen-gogo extension in the output directory
# satisfies import of github.com/gogo/protobuf/gogoproto/gogo.proto and the core Google protobuf types
PATH="${KUBE_ROOT}/_output/local/go/bin:${PATH}" "${gotoprotobuf}" \
  --conditional="proto" \
  --proto-import="${KUBE_ROOT}/Godeps/_workspace/src" \
  --proto-import="${KUBE_ROOT}/Godeps/_workspace/src/github.com/gogo/protobuf/protobuf"
