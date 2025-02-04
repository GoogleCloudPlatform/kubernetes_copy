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

REGISTRY ?= registry.k8s.io/e2e-test-images
GOARM ?= 7
DOCKER_CERT_BASE_PATH ?=
QEMUVERSION=v5.1.0-2
GOLANG_VERSION=1.23.5
export

ifndef WHAT
$(error WHAT is a required variable, ex: make all WHAT=net)
endif

# Build code.
#
# Args:
#   WHAT: Directory names to build.
#
# Example:
#   make all WHAT=httpd
all: all-container

all-container:
	./image-util.sh build $(WHAT) "docker"

all-push:
	bash -x ./image-util.sh build $(WHAT) "registry"
	bash -x ./image-util.sh push $(WHAT)

all-build-and-push:
	./image-util.sh build_and_push ${WHAT}

.PHONY: all all-build-and-push all-push all-container
