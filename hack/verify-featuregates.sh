#!/usr/bin/env bash

# Copyright 2024 The Kubernetes Authors.
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

# This script checks test/compatibility_lifecycle/reference/versioned_feature_list.yaml
# are up to date with all the feature gate features.
# We should run `hack/update-featuregates.sh` if the list is out of date.
# Usage: `hack/verify-featuregates.sh`.

set -o errexit
set -o nounset
set -o pipefail

KUBE_ROOT=$(dirname "${BASH_SOURCE[0]}")/..
source "${KUBE_ROOT}/hack/lib/init.sh"

kube::golang::setup_env

cd "${KUBE_ROOT}"

if ! go run test/compatibility_lifecycle/main.go feature-gates verify; then
  echo "Please run 'hack/update-featuregates.sh' to update the feature list."
  exit 1
fi
