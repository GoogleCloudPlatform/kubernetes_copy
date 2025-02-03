#!/usr/bin/env bash

# Copyright 2015 The Kubernetes Authors.
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

# Runs the unit and integration tests, producing JUnit-style XML test
# reports in ${WORKSPACE}/artifacts. This script is intended to be run from
# kubekins-test container with a kubernetes repo mapped in. See
# k8s.io/test-infra/scenarios/kubernetes_verify.py

export PATH=${GOPATH}/bin:${PWD}/third_party/etcd:/usr/local/go/bin:${PATH}

# Until all GOPATH references are removed from all build scripts as well,
# explicitly disable module mode to avoid picking up user-set GO111MODULE preferences.
# As individual scripts make use of go modules, they can explicitly set GO111MODULE=on
export GO111MODULE=off

# Install tools we need
(
  cd "./hack/tools"
  # override only for installing tools (in subshell), if set
  if [ -n "${KUBE_HACK_TOOLS_GOTOOLCHAIN+x}" ]; then
    export GOTOOLCHAIN="${KUBE_HACK_TOOLS_GOTOOLCHAIN}";
  fi
  GO111MODULE=on go install gotest.tools/gotestsum
)

# Disable coverage report
export KUBE_COVER="n"
# Set artifacts directory
export ARTIFACTS=${ARTIFACTS:-"${WORKSPACE}/artifacts"}
# Save the verbose stdout as well.
export KUBE_KEEP_VERBOSE_TEST_OUTPUT=y
export KUBE_INTEGRATION_TEST_MAX_CONCURRENCY=4
export LOG_LEVEL=4

cd "${GOPATH}/src/k8s.io/kubernetes"

./hack/install-etcd.sh

make test-cmd
make test-integration
