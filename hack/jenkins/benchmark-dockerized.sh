#!/usr/bin/env bash

# Copyright 2018 The Kubernetes Authors.
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
set -o xtrace

retry() {
  for i in {1..5}; do
    if "$@"
    then
      return 0
    else
      sleep "${i}"
    fi
  done
  "$@"
}

# The root of the build/dist directory
KUBE_ROOT=$(dirname "${BASH_SOURCE[0]}")/../..
export KUBE_ROOT

# Runs benchmark integration tests, producing pretty-printed results
# in ${WORKSPACE}/artifacts. This script can also be run within a
# kubekins-test container with a kubernetes repo mounted (at the path
# /go/src/k8s.io/kubernetes).

export PATH=${GOPATH}/bin:${PWD}/third_party/etcd:/usr/local/go/bin:${PATH}

# Until all GOPATH references are removed from all build scripts as well,
# explicitly disable module mode to avoid picking up user-set GO111MODULE preferences.
# As individual scripts make use of go modules, they can explicitly set GO111MODULE=on
export GO111MODULE=off

# Install tools we need
(
  cd "${KUBE_ROOT}/hack/tools"
  # override only for installing tools (in subshell), if set
  if [ -n "${KUBE_HACK_TOOLS_GOTOOLCHAIN+x}" ]; then
    export GOTOOLCHAIN="${KUBE_HACK_TOOLS_GOTOOLCHAIN}";
  fi
  GO111MODULE=on go install github.com/cespare/prettybench
  GO111MODULE=on go install gotest.tools/gotestsum
)

# Disable the Go race detector.
export KUBE_RACE=" "
# Disable coverage report
export KUBE_COVER="n"
export ARTIFACTS=${ARTIFACTS:-"${WORKSPACE}/artifacts"}
export FULL_LOG="true"

mkdir -p "${ARTIFACTS}"
cd "${GOPATH}/src/k8s.io/kubernetes"

./hack/install-etcd.sh

# Run the benchmark tests and pretty-print the results into a separate file.
# Log output of the tests go to stderr.
make test-integration WHAT="$*" KUBE_TEST_ARGS="-run='XXX' -bench=${TEST_PREFIX:-.} -benchtime=${BENCHTIME:-1s} -benchmem  -data-items-dir=${ARTIFACTS}" \
  | (go run test/integration/benchmark/extractlog/main.go) \
  | tee \
   >(prettybench -no-passthrough > "${ARTIFACTS}/BenchmarkResults.txt") \
   >(go run test/integration/benchmark/jsonify/main.go "${ARTIFACTS}/BenchmarkResults_benchmark_$(date -u +%Y-%m-%dT%H:%M:%SZ).json" || cat > /dev/null)
