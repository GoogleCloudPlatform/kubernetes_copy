# Copyright 2016 The Kubernetes Authors.
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

# Build the etcd image
#
# Usage:
# 	[BUNDLED_ETCD_VERSIONS=3.4.18 3.5.17] [REGISTRY=registry.k8s.io] [ARCH=amd64] [BASEIMAGE=busybox] make (build|push)
#
# The image contains different etcd versions to simplify
# upgrades. Thus be careful when removing any versions from here.
#
# NOTE: The etcd upgrade rules are that you can upgrade only 1 minor
# version at a time, and patch release don't matter.
#
# Except from etcd-$(version) and etcdctl-$(version) binaries, we also
# need etcd and etcdctl binaries for backward compatibility reasons.
# That binary will be set to the last version from $(BUNDLED_ETCD_VERSIONS).
BUNDLED_ETCD_VERSIONS?=3.4.18 3.5.17

# LATEST_ETCD_VERSION identifies the most recent etcd version available.
LATEST_ETCD_VERSION?=3.5.17

# REVISION provides a version number for this image and all it's bundled
# artifacts. It should start at zero for each LATEST_ETCD_VERSION and increment
# for each revision of this image at that etcd version.
REVISION?=0

# IMAGE_TAG Uniquely identifies registry.k8s.io/etcd docker image with a tag of the form "<etcd-version>-<revision>".
IMAGE_TAG=$(LATEST_ETCD_VERSION)-$(REVISION)

ARCH?=amd64

# Operating systems supported: linux, windows
OS ?= linux
# OS Version for the Windows images: 1809, ltsc2022
OSVERSION ?= 1809

# The output type could either be docker (local), or registry.
# If it is registry, it will also allow us to push the Windows images.
OUTPUT_TYPE ?= docker

ALL_OS = linux windows
ALL_ARCH.linux = amd64 arm arm64 ppc64le s390x
ALL_OS_ARCH.linux = $(foreach arch, ${ALL_ARCH.linux}, linux-$(arch))
ALL_ARCH.windows = amd64
ALL_OSVERSIONS.windows := 1809 ltsc2022
ALL_OS_ARCH.windows = $(foreach arch, $(ALL_ARCH.windows), $(foreach osversion, ${ALL_OSVERSIONS.windows}, windows-$(arch)-${osversion}))
ALL_OS_ARCH = $(foreach os, $(ALL_OS), ${ALL_OS_ARCH.${os}})

IMAGE_SUFFIX.linux = $(OS)-$(ARCH)
IMAGE_SUFFIX.windows = $(OS)-$(ARCH)-$(OSVERSION)
IMAGE_SUFFIX := ${IMAGE_SUFFIX.${OS}}

# Image should be pulled from registry.k8s.io, which will auto-detect
# region (us, eu, asia, ...) and pull from the closest.
REGISTRY?=registry.k8s.io
# Images should be pushed to staging-k8s.gcr.io.
PUSH_REGISTRY?=staging-k8s.gcr.io

MANIFEST_IMAGE := $(PUSH_REGISTRY)/etcd

# Install binaries matching base distro permissions
BIN_INSTALL := install -m 0555

# Hosts running SELinux need :z added to volume mounts
SELINUX_ENABLED := $(shell cat /sys/fs/selinux/enforce 2> /dev/null || echo 0)

ifeq ($(SELINUX_ENABLED),1)
  DOCKER_VOL_OPTS?=:z
endif

# This option is for running docker manifest command
export DOCKER_CLI_EXPERIMENTAL := enabled
# golang version should match the golang version of the official build from https://github.com/etcd-io/etcd/releases.
GOLANG_VERSION := 1.22.9
GOARM?=7
TEMP_DIR:=$(shell mktemp -d)

DOCKERFILE.linux = Dockerfile
DOCKERFILE.windows = Dockerfile.windows
DOCKERFILE := ${DOCKERFILE.${OS}}

ifeq ($(ARCH),amd64)
    BASEIMAGE?=registry.k8s.io/build-image/debian-base:bookworm-v1.0.4
endif
ifeq ($(ARCH),arm)
    BASEIMAGE?=registry.k8s.io/build-image/debian-base-arm:bookworm-v1.0.4
endif
ifeq ($(ARCH),arm64)
    BASEIMAGE?=registry.k8s.io/build-image/debian-base-arm64:bookworm-v1.0.4
endif
ifeq ($(ARCH),ppc64le)
    BASEIMAGE?=registry.k8s.io/build-image/debian-base-ppc64le:bookworm-v1.0.4
endif
ifeq ($(ARCH),s390x)
    BASEIMAGE?=registry.k8s.io/build-image/debian-base-s390x:bookworm-v1.0.4
endif

BASE.windows = mcr.microsoft.com/windows/nanoserver

RUNNERIMAGE.windows?=$(BASE.windows):$(OSVERSION)
RUNNERIMAGE.linux?=gcr.io/distroless/static:latest
RUNNERIMAGE := ${RUNNERIMAGE.${OS}}

QEMUVERSION?=5.2.0-2

build:
	# Explicitly copy files to the temp directory
	$(BIN_INSTALL) migrate-if-needed.sh $(TEMP_DIR)
	$(BIN_INSTALL) migrate-if-needed.bat $(TEMP_DIR)
	install $(DOCKERFILE) $(TEMP_DIR)

	# Compile migrate
	migrate_tmp_dir=$(shell mktemp -d); \
	docker run --rm --interactive -v $(shell pwd)/../../../:/go/src/k8s.io/kubernetes$(DOCKER_VOL_OPTS) -v $${migrate_tmp_dir}:/build$(DOCKER_VOL_OPTS) -e GOOS=$(OS) -e GOARCH=$(ARCH) golang:$(GOLANG_VERSION) \
		/bin/bash -c "CGO_ENABLED=0 GO111MODULE=off go build -o /build/migrate k8s.io/kubernetes/cluster/images/etcd/migrate"; \
	$(BIN_INSTALL) $${migrate_tmp_dir}/migrate $(TEMP_DIR);

ifeq ($(ARCH),amd64)

	# Do not compile if we should make an image for amd64, use the official etcd binaries instead
	# For each release create a tmp dir 'etcd_release_tmp_dir' and unpack the release tar there.
ifeq ($(OS),windows)
	for version in $(BUNDLED_ETCD_VERSIONS); do \
		etcd_release_tmp_dir=$(shell mktemp -d); \
		curl -sSL --retry 5 https://github.com/etcd-io/etcd/releases/download/v$$version/etcd-v$$version-windows-amd64.zip -o etcd-v$$version-windows-amd64.zip; \
		unzip -q -d $$etcd_release_tmp_dir etcd-v$$version-windows-amd64.zip; \
		rm etcd-v$$version-windows-amd64.zip; \
		$(BIN_INSTALL) $$etcd_release_tmp_dir/etcd-v$$version-windows-amd64/etcd.exe $$etcd_release_tmp_dir/etcd-v$$version-windows-amd64/etcdctl.exe $(TEMP_DIR)/; \
		$(BIN_INSTALL) $(TEMP_DIR)/etcd.exe $(TEMP_DIR)/etcd-$$version.exe; \
		$(BIN_INSTALL) $(TEMP_DIR)/etcdctl.exe $(TEMP_DIR)/etcdctl-$$version.exe; \
	done
else
	for version in $(BUNDLED_ETCD_VERSIONS); do \
		etcd_release_tmp_dir=$(shell mktemp -d); \
		curl -sSL --retry 5 https://github.com/etcd-io/etcd/releases/download/v$$version/etcd-v$$version-linux-amd64.tar.gz | tar -xz -C $$etcd_release_tmp_dir --strip-components=1; \
		$(BIN_INSTALL) $$etcd_release_tmp_dir/etcd $$etcd_release_tmp_dir/etcdctl $(TEMP_DIR)/; \
		$(BIN_INSTALL) $(TEMP_DIR)/etcd $(TEMP_DIR)/etcd-$$version; \
		$(BIN_INSTALL) $(TEMP_DIR)/etcdctl $(TEMP_DIR)/etcdctl-$$version; \
	done
endif

else

	# Download etcd in a golang container and cross-compile it statically
	# For each release create a tmp dir 'etcd_release_tmp_dir' and unpack the release tar there.
	arch_prefix=""
        ifeq ($(ARCH),arm)
		arch_prefix="GOARM=$(GOARM)"
        endif

	# use '/go/src/go.etcd.io/etcd' to build etcd 3.4 and later.
	for version in $(BUNDLED_ETCD_VERSIONS); do \
		etcd_release_tmp_dir=$(shell mktemp -d); \
		etcd_build_dir="/go/src/github.com/coreos/etcd"; \
		if [ $$(echo $$version | cut -d. -f2) -gt 3 ]; then \
			etcd_build_dir="/go/src/go.etcd.io/etcd"; \
		fi; \
		docker run --rm --interactive -v $${etcd_release_tmp_dir}:/etcdbin golang:$(GOLANG_VERSION)$(DOCKER_VOL_OPTS) /bin/bash -c \
			"git clone https://github.com/etcd-io/etcd $$etcd_build_dir \
			&& cd $$etcd_build_dir \
			&& git checkout v$${version} \
			&& $(arch_prefix) GOARCH=$(ARCH) ./build.sh \
			&& cp -f bin/$(ARCH)/etcd* bin/etcd* /etcdbin; echo 'done'"; \
		$(BIN_INSTALL) $$etcd_release_tmp_dir/etcd $$etcd_release_tmp_dir/etcdctl $(TEMP_DIR)/; \
		$(BIN_INSTALL) $(TEMP_DIR)/etcd $(TEMP_DIR)/etcd-$$version; \
		$(BIN_INSTALL) $(TEMP_DIR)/etcdctl $(TEMP_DIR)/etcdctl-$$version; \
	done

	# Add this ENV variable in order to workaround an unsupported arch blocker
	# On arm (which is 32-bit), it can't handle >1GB data in-memory
        ifeq ($(ARCH),arm)
		cd $(TEMP_DIR) && echo "ENV ETCD_UNSUPPORTED_ARCH=$(ARCH)" >> $(DOCKERFILE)
        endif
endif

	docker run --rm --privileged multiarch/qemu-user-static:$(QEMUVERSION) --reset -p yes
	docker buildx version
	BUILDER=$(shell docker buildx create --use)

	# And build the image
	docker buildx build \
		--pull \
		--provenance=false \
		--sbom=false \
		--output=type=$(OUTPUT_TYPE) \
		--platform "$(OS)/$(ARCH)" \
		-t $(REGISTRY)/etcd:$(IMAGE_TAG)-$(IMAGE_SUFFIX) \
		--build-arg BASEIMAGE=$(BASEIMAGE) \
		--build-arg RUNNERIMAGE=$(RUNNERIMAGE) \
		-f $(TEMP_DIR)/$(DOCKERFILE) \
		$(TEMP_DIR)
	docker buildx rm $$BUILDER

push: build

# split words on hyphen, access by 1-index
word-hyphen = $(word $2,$(subst -, ,$1))

sub-build-%:
	$(MAKE) OUTPUT_TYPE=docker OS=$(call word-hyphen,$*,1) ARCH=$(call word-hyphen,$*,2) build

all-build: $(addprefix sub-build-,$(ALL_OS_ARCH))

sub-push-image-%:
	$(MAKE) OUTPUT_TYPE=registry OS=$(call word-hyphen,$*,1) ARCH=$(call word-hyphen,$*,2) OSVERSION=$(call word-hyphen,$*,3) REGISTRY=$(PUSH_REGISTRY) push

all-push-images: $(addprefix sub-push-image-,$(ALL_OS_ARCH))

# NOTE(claudiub): A non-default builder instance is needed in order to build Windows images.
all-push: all-push-images push-manifest

push-manifest:
	docker manifest create --amend $(MANIFEST_IMAGE):$(IMAGE_TAG) $(shell echo $(ALL_OS_ARCH) | sed -e "s~[^ ]*~$(MANIFEST_IMAGE):$(IMAGE_TAG)\-&~g")
	set -x; for arch in $(ALL_ARCH.linux); do docker manifest annotate --os linux --arch $${arch} ${MANIFEST_IMAGE}:${IMAGE_TAG} ${MANIFEST_IMAGE}:${IMAGE_TAG}-linux-$${arch}; done
	# For Windows images, we also need to include the "os.version" in the manifest list, so the Windows node can pull the proper image it needs.
	# we use awk to also trim the quotes around the OS version string.
	set -x; \
	for arch in $(ALL_ARCH.windows);  do \
		for osversion in ${ALL_OSVERSIONS.windows}; do \
			full_version=`docker manifest inspect ${BASE.windows}:$${osversion} | grep "os.version" | head -n 1 | awk -F\" '{print $$4}'` || true; \
			docker manifest annotate --os windows --arch $${arch} --os-version $${full_version} ${MANIFEST_IMAGE}:${IMAGE_TAG} ${MANIFEST_IMAGE}:${IMAGE_TAG}-windows-$${arch}-$${osversion}; \
		done; \
	done
	docker manifest push --purge ${MANIFEST_IMAGE}:${IMAGE_TAG}

unit-test:
	docker run --rm --interactive -v $(shell pwd)/../../../:/go/src/k8s.io/kubernetes$(DOCKER_VOL_OPTS) -e GOARCH=$(ARCH) golang:$(GOLANG_VERSION) \
		/bin/bash -c "CGO_ENABLED=0 go test -v k8s.io/kubernetes/cluster/images/etcd/migrate"

# Integration tests require both a golang build environment and all the etcd binaries from a `registry.k8s.io/etcd` image (`/usr/local/bin/etcd-<version>`, ...).
# Since the `registry.k8s.io/etcd` image is for runtime only and does not have a build golang environment, we create a new docker image to run integration tests
# with.
build-integration-test-image: build
	cp -r $(TEMP_DIR) $(TEMP_DIR)_integration_test
	cp Dockerfile $(TEMP_DIR)_integration_test/Dockerfile
	docker build \
		--pull \
		-t etcd-integration-test \
		--build-arg BASEIMAGE=golang:$(GOLANG_VERSION) \
		--build-arg RUNNERIMAGE=$(RUNNERIMAGE) \
		$(TEMP_DIR)_integration_test

integration-test:
	docker run --rm --interactive -v $(shell pwd)/../../../:/go/src/k8s.io/kubernetes$(DOCKER_VOL_OPTS) -e GOARCH=$(ARCH) etcd-integration-test \
		/bin/bash -c "CGO_ENABLED=0 go test -tags=integration k8s.io/kubernetes/cluster/images/etcd/migrate -args -v 10 -logtostderr true"

integration-build-test: build-integration-test-image integration-test
test: unit-test integration-build-test
all: all-build test
.PHONY:	build push push-manifest all-push all-push-images all-build unit-test build-integration-test-image integration-test integration-build-test test
