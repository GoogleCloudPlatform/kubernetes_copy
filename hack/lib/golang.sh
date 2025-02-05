#!/usr/bin/env bash

# Copyright 2014 The Kubernetes Authors.
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

# shellcheck disable=SC2034 # Variables sourced in other scripts.

readonly KUBE_GOPATH="${KUBE_GOPATH:-"${KUBE_OUTPUT}/go"}"
export KUBE_GOPATH

# The server platform we are building on.
readonly KUBE_SUPPORTED_SERVER_PLATFORMS=(
  linux/amd64
  linux/arm64
  linux/s390x
  linux/ppc64le
)

# The node platforms we build for
readonly KUBE_SUPPORTED_NODE_PLATFORMS=(
  linux/amd64
  linux/arm64
  linux/s390x
  linux/ppc64le
  windows/amd64
)

# If we update this we should also update the set of platforms whose standard
# library is precompiled for in build/build-image/cross/Dockerfile
readonly KUBE_SUPPORTED_CLIENT_PLATFORMS=(
  linux/amd64
  linux/386
  linux/arm
  linux/arm64
  linux/s390x
  linux/ppc64le
  darwin/amd64
  darwin/arm64
  windows/amd64
  windows/386
  windows/arm64
)

# Which platforms we should compile test targets for.
# Not all client platforms need these tests
readonly KUBE_SUPPORTED_TEST_PLATFORMS=(
  linux/amd64
  linux/arm64
  linux/s390x
  linux/ppc64le
  darwin/amd64
  darwin/arm64
  windows/amd64
  windows/arm64
)

# The set of server targets that we are only building for Linux
kube::golang::server_targets() {
  local targets=(
    cmd/kube-proxy
    cmd/kube-apiserver
    cmd/kube-controller-manager
    cmd/kubelet
    cmd/kubeadm
    cmd/kube-scheduler
    staging/src/k8s.io/component-base/logs/kube-log-runner
    staging/src/k8s.io/kube-aggregator
    staging/src/k8s.io/apiextensions-apiserver
    cluster/gce/gci/mounter
  )
  echo "${targets[@]}"
}

IFS=" " read -ra KUBE_SERVER_TARGETS <<< "$(kube::golang::server_targets)"
readonly KUBE_SERVER_TARGETS
readonly KUBE_SERVER_BINARIES=("${KUBE_SERVER_TARGETS[@]##*/}")

# The set of server targets we build docker images for
kube::golang::server_image_targets() {
  # NOTE: this contains cmd targets for kube::build::get_docker_wrapped_binaries
  local targets=(
    cmd/kube-apiserver
    cmd/kube-controller-manager
    cmd/kube-scheduler
    cmd/kube-proxy
    cmd/kubectl
  )
  echo "${targets[@]}"
}

IFS=" " read -ra KUBE_SERVER_IMAGE_TARGETS <<< "$(kube::golang::server_image_targets)"
readonly KUBE_SERVER_IMAGE_TARGETS
readonly KUBE_SERVER_IMAGE_BINARIES=("${KUBE_SERVER_IMAGE_TARGETS[@]##*/}")

# The set of conformance targets we build docker image for
kube::golang::conformance_image_targets() {
  # NOTE: this contains cmd targets for kube::release::build_conformance_image
  local targets=(
    ginkgo
    test/e2e/e2e.test
    test/conformance/image/go-runner
    cmd/kubectl
  )
  echo "${targets[@]}"
}

IFS=" " read -ra KUBE_CONFORMANCE_IMAGE_TARGETS <<< "$(kube::golang::conformance_image_targets)"
readonly KUBE_CONFORMANCE_IMAGE_TARGETS

# The set of server targets that we are only building for Kubernetes nodes
kube::golang::node_targets() {
  local targets=(
    cmd/kube-proxy
    cmd/kubeadm
    cmd/kubelet
    staging/src/k8s.io/component-base/logs/kube-log-runner
  )
  echo "${targets[@]}"
}

IFS=" " read -ra KUBE_NODE_TARGETS <<< "$(kube::golang::node_targets)"
readonly KUBE_NODE_TARGETS
readonly KUBE_NODE_BINARIES=("${KUBE_NODE_TARGETS[@]##*/}")
readonly KUBE_NODE_BINARIES_WIN=("${KUBE_NODE_BINARIES[@]/%/.exe}")

# ------------
# NOTE: All functions that return lists should use newlines.
# bash functions can't return arrays, and spaces are tricky, so newline
# separators are the preferred pattern.
# To transform a string of newline-separated items to an array, use kube::util::read-array:
# kube::util::read-array FOO < <(kube::golang::dups a b c a)
#
# ALWAYS remember to quote your subshells. Not doing so will break in
# bash 4.3, and potentially cause other issues.
# ------------

# Returns a sorted newline-separated list containing only duplicated items.
kube::golang::dups() {
  # We use printf to insert newlines, which are required by sort.
  printf "%s\n" "$@" | sort | uniq -d
}

# Returns a sorted newline-separated list with duplicated items removed.
kube::golang::dedup() {
  # We use printf to insert newlines, which are required by sort.
  printf "%s\n" "$@" | sort -u
}

# Depends on values of user-facing KUBE_BUILD_PLATFORMS, KUBE_FASTBUILD,
# and KUBE_BUILDER_OS.
# Configures KUBE_SERVER_PLATFORMS, KUBE_NODE_PLATFOMRS,
# KUBE_TEST_PLATFORMS, and KUBE_CLIENT_PLATFORMS, then sets them
# to readonly.
# The configured vars will only contain platforms allowed by the
# KUBE_SUPPORTED* vars at the top of this file.
declare -a KUBE_SERVER_PLATFORMS
declare -a KUBE_CLIENT_PLATFORMS
declare -a KUBE_NODE_PLATFORMS
declare -a KUBE_TEST_PLATFORMS
kube::golang::setup_platforms() {
  if [[ -n "${KUBE_BUILD_PLATFORMS:-}" ]]; then
    # KUBE_BUILD_PLATFORMS needs to be read into an array before the next
    # step, or quoting treats it all as one element.
    local -a platforms
    IFS=" " read -ra platforms <<< "${KUBE_BUILD_PLATFORMS}"

    # Deduplicate to ensure the intersection trick with kube::golang::dups
    # is not defeated by duplicates in user input.
    kube::util::read-array platforms < <(kube::golang::dedup "${platforms[@]}")

    # Use kube::golang::dups to restrict the builds to the platforms in
    # KUBE_SUPPORTED_*_PLATFORMS. Items should only appear at most once in each
    # set, so if they appear twice after the merge they are in the intersection.
    kube::util::read-array KUBE_SERVER_PLATFORMS < <(kube::golang::dups \
        "${platforms[@]}" \
        "${KUBE_SUPPORTED_SERVER_PLATFORMS[@]}" \
      )
    readonly KUBE_SERVER_PLATFORMS

    kube::util::read-array KUBE_NODE_PLATFORMS < <(kube::golang::dups \
        "${platforms[@]}" \
        "${KUBE_SUPPORTED_NODE_PLATFORMS[@]}" \
      )
    readonly KUBE_NODE_PLATFORMS

    kube::util::read-array KUBE_TEST_PLATFORMS < <(kube::golang::dups \
        "${platforms[@]}" \
        "${KUBE_SUPPORTED_TEST_PLATFORMS[@]}" \
      )
    readonly KUBE_TEST_PLATFORMS

    kube::util::read-array KUBE_CLIENT_PLATFORMS < <(kube::golang::dups \
        "${platforms[@]}" \
        "${KUBE_SUPPORTED_CLIENT_PLATFORMS[@]}" \
      )
    readonly KUBE_CLIENT_PLATFORMS

  elif [[ "${KUBE_FASTBUILD:-}" == "true" ]]; then
    host_arch=$(kube::util::host_arch)
    if [[ "${host_arch}" != "amd64" && "${host_arch}" != "arm64" && "${host_arch}" != "ppc64le" && "${host_arch}" != "s390x" ]]; then
      # on any platform other than amd64, arm64, ppc64le and s390x, we just default to amd64
      host_arch="amd64"
    fi
    KUBE_SERVER_PLATFORMS=("linux/${host_arch}")
    readonly KUBE_SERVER_PLATFORMS
    KUBE_NODE_PLATFORMS=("linux/${host_arch}")
    readonly KUBE_NODE_PLATFORMS
    if [[ "${KUBE_BUILDER_OS:-}" == "darwin"* ]]; then
      KUBE_TEST_PLATFORMS=(
        "darwin/${host_arch}"
        "linux/${host_arch}"
      )
      readonly KUBE_TEST_PLATFORMS
      KUBE_CLIENT_PLATFORMS=(
        "darwin/${host_arch}"
        "linux/${host_arch}"
      )
      readonly KUBE_CLIENT_PLATFORMS
    else
      KUBE_TEST_PLATFORMS=("linux/${host_arch}")
      readonly KUBE_TEST_PLATFORMS
      KUBE_CLIENT_PLATFORMS=("linux/${host_arch}")
      readonly KUBE_CLIENT_PLATFORMS
    fi
  else
    KUBE_SERVER_PLATFORMS=("${KUBE_SUPPORTED_SERVER_PLATFORMS[@]}")
    readonly KUBE_SERVER_PLATFORMS

    KUBE_NODE_PLATFORMS=("${KUBE_SUPPORTED_NODE_PLATFORMS[@]}")
    readonly KUBE_NODE_PLATFORMS

    KUBE_CLIENT_PLATFORMS=("${KUBE_SUPPORTED_CLIENT_PLATFORMS[@]}")
    readonly KUBE_CLIENT_PLATFORMS

    KUBE_TEST_PLATFORMS=("${KUBE_SUPPORTED_TEST_PLATFORMS[@]}")
    readonly KUBE_TEST_PLATFORMS
  fi
}

kube::golang::setup_platforms

# The set of client targets that we are building for all platforms
readonly KUBE_CLIENT_TARGETS=(
  cmd/kubectl
  cmd/kubectl-convert
)
readonly KUBE_CLIENT_BINARIES=("${KUBE_CLIENT_TARGETS[@]##*/}")
readonly KUBE_CLIENT_BINARIES_WIN=("${KUBE_CLIENT_BINARIES[@]/%/.exe}")

# The set of test targets that we are building for all platforms
kube::golang::test_targets() {
  local targets=(
    ginkgo
    test/e2e/e2e.test
    test/conformance/image/go-runner
  )
  echo "${targets[@]}"
}
IFS=" " read -ra KUBE_TEST_TARGETS <<< "$(kube::golang::test_targets)"
readonly KUBE_TEST_TARGETS
readonly KUBE_TEST_BINARIES=("${KUBE_TEST_TARGETS[@]##*/}")
readonly KUBE_TEST_BINARIES_WIN=("${KUBE_TEST_BINARIES[@]/%/.exe}")
readonly KUBE_TEST_PORTABLE=(
  test/e2e/testing-manifests
  test/kubemark
  hack/e2e-internal
  hack/get-build.sh
  hack/ginkgo-e2e.sh
  hack/lib
)

# Test targets which run on the Kubernetes clusters directly, so we only
# need to target server platforms.
# These binaries will be distributed in the kubernetes-test tarball.
kube::golang::server_test_targets() {
  local targets=(
    cmd/kubemark
    ginkgo
  )

  if [[ "${OSTYPE:-}" == "linux"* ]]; then
    targets+=( test/e2e_node/e2e_node.test )
  fi

  echo "${targets[@]}"
}

IFS=" " read -ra KUBE_TEST_SERVER_TARGETS <<< "$(kube::golang::server_test_targets)"
readonly KUBE_TEST_SERVER_TARGETS
readonly KUBE_TEST_SERVER_BINARIES=("${KUBE_TEST_SERVER_TARGETS[@]##*/}")
readonly KUBE_TEST_SERVER_PLATFORMS=("${KUBE_SERVER_PLATFORMS[@]:+"${KUBE_SERVER_PLATFORMS[@]}"}")

# Gigabytes necessary for parallel platform builds.
# As of March 2021 (go 1.16/amd64), the RSS usage is 2GiB by using cached
# memory of 15GiB.
# This variable can be overwritten at your own risk.
# It's defaulting to 20G to provide some headroom.
readonly KUBE_PARALLEL_BUILD_MEMORY=${KUBE_PARALLEL_BUILD_MEMORY:-20}

readonly KUBE_ALL_TARGETS=(
  "${KUBE_SERVER_TARGETS[@]}"
  "${KUBE_CLIENT_TARGETS[@]}"
  "${KUBE_TEST_TARGETS[@]}"
  "${KUBE_TEST_SERVER_TARGETS[@]}"
)
readonly KUBE_ALL_BINARIES=("${KUBE_ALL_TARGETS[@]##*/}")

readonly KUBE_STATIC_BINARIES=(
  apiextensions-apiserver
  kube-aggregator
  kube-apiserver
  kube-controller-manager
  kube-scheduler
  kube-proxy
  kube-log-runner
  kubeadm
  kubectl
  kubectl-convert
  kubemark
  mounter
)

# Fully-qualified package names that we want to instrument for coverage information.
readonly KUBE_COVERAGE_INSTRUMENTED_PACKAGES=(
  k8s.io/kubernetes/cmd/kube-apiserver
  k8s.io/kubernetes/cmd/kube-controller-manager
  k8s.io/kubernetes/cmd/kube-scheduler
  k8s.io/kubernetes/cmd/kube-proxy
  k8s.io/kubernetes/cmd/kubelet
)

# KUBE_CGO_OVERRIDES is a space-separated list of binaries which should be built
# with CGO enabled, assuming CGO is supported on the target platform.
# This overrides any entry in KUBE_STATIC_BINARIES.
IFS=" " read -ra KUBE_CGO_OVERRIDES_LIST <<< "${KUBE_CGO_OVERRIDES:-}"
readonly KUBE_CGO_OVERRIDES_LIST
# KUBE_STATIC_OVERRIDES is a space-separated list of binaries which should be
# built with CGO disabled. This is in addition to the list in
# KUBE_STATIC_BINARIES.
IFS=" " read -ra KUBE_STATIC_OVERRIDES_LIST <<< "${KUBE_STATIC_OVERRIDES:-}"
readonly KUBE_STATIC_OVERRIDES_LIST

kube::golang::is_statically_linked() {
  local e
  # Explicitly enable cgo when building kubectl for darwin from darwin.
  [[ "$(go env GOHOSTOS)" == "darwin" && "$(go env GOOS)" == "darwin" &&
    "$1" == *"/kubectl" ]] && return 1
  if [[ -n "${KUBE_CGO_OVERRIDES_LIST:+x}" ]]; then
    for e in "${KUBE_CGO_OVERRIDES_LIST[@]}"; do [[ "${1}" == *"/${e}" ]] && return 1; done;
  fi
  for e in "${KUBE_STATIC_BINARIES[@]}"; do [[ "${1}" == *"/${e}" ]] && return 0; done;
  if [[ -n "${KUBE_STATIC_OVERRIDES_LIST:+x}" ]]; then
    for e in "${KUBE_STATIC_OVERRIDES_LIST[@]}"; do [[ "${1}" == *"/${e}" ]] && return 0; done;
  fi
  return 1;
}

# kube::golang::best_guess_go_targets takes a list of build targets, which might
# be Go-style names (e.g. example.com/foo/bar or ./foo/bar) or just local paths
# (e.g. foo/bar) and produces a respective list (on stdout) of our best guess at
# Go target names.
kube::golang::best_guess_go_targets() {
  local target
  for target; do
    if [ "${target}" = "ginkgo" ] ||
       [ "${target}" = "github.com/onsi/ginkgo/ginkgo" ] ||
       [ "${target}" = "vendor/github.com/onsi/ginkgo/ginkgo" ]; then
      # Aliases that build the ginkgo CLI for hack/ginkgo-e2e.sh.
      # "ginkgo" is the one that is documented in the Makefile. The others
      # are for backwards compatibility.
      echo "github.com/onsi/ginkgo/v2/ginkgo"
      continue
    fi

    if [[ "${target}" =~ ^([[:alnum:]]+".")+[[:alnum:]]+"/".+ ]]; then
      # If the target starts with what looks like a domain name, assume it has a
      # fully-qualified Go package name.
      echo "${target}"
      continue
    fi

    if [[ "${target}" =~ ^vendor/ ]]; then
      # Strip vendor/ prefix, since we're building in gomodule mode.  This is
      # for backwards compatibility.
      echo "${target#"vendor/"}"
      continue
    fi

    # If the target starts with "./", assume it is a local path which qualifies
    # as a Go target name.
    if [[ "${target}" =~ ^\./ ]]; then
      echo "${target}"
      continue
    fi

    # Otherwise assume it's a relative path (e.g. foo/bar or foo/bar/bar.test).
    # We probably SHOULDN'T accept this, but we did in the past and it would be
    # rude to break things if we don't NEED to.  We can't really test if it
    # exists or not, because the last element might be an output file (e.g.
    # bar.test) or even "...".
    echo "./${target}"
  done
}

kube::golang::internal::lazy_normalize() {
  target="$1"

  if [[ "${target}" =~ ^([[:alnum:]]+".")+[[:alnum:]]+"/".+ ]]; then
    # If the target starts with what looks like a domain name, assume it has a
    # fully-qualified Go package name.
    echo "${target}"
    return
  fi

  go list -find -e "${target}"
}

# kube::golang::normalize_go_targets takes a list of build targets, which might
# be Go-style names (e.g. example.com/foo/bar or ./foo/bar) or just local paths
# (e.g. foo/bar) and produces a respective list (on stdout) of Go package
# names.
#
# If this cannot find (go list -find -e) one or more inputs, it will emit the
# them on stdout, so callers can at least get a useful error.
kube::golang::normalize_go_targets() {
  local targets=()
  kube::util::read-array targets < <(kube::golang::best_guess_go_targets "$@")
  kube::util::read-array targets < <(kube::golang::dedup "${targets[@]}")
  set -- "${targets[@]}"

  for target; do
    if [[ "${target}" =~ ".test"$ ]]; then
      local dir
      dir="$(dirname "${target}")"
      local tst
      tst="$(basename "${target}")"
      local pkg
      pkg="$(kube::golang::internal::lazy_normalize "${dir}")"
      echo "${pkg}/${tst}"
      continue
    fi
    if [[ "${target}" =~ "/..."$ ]]; then
      local dir
      dir="$(dirname "${target}")"
      local pkg
      pkg="$(kube::golang::internal::lazy_normalize "${dir}")"
      echo "${pkg}/..."
      continue
    fi
    kube::golang::internal::lazy_normalize "${target}"
  done
}

# Asks golang what it thinks the host platform is. The go tool chain does some
# slightly different things when the target platform matches the host platform.
kube::golang::host_platform() {
  echo "$(go env GOHOSTOS)/$(go env GOHOSTARCH)"
}

# Takes the platform name ($1) and sets the appropriate golang env variables
# for that platform.
kube::golang::set_platform_envs() {
  [[ -n ${1-} ]] || {
    kube::log::error_exit "!!! Internal error. No platform set in kube::golang::set_platform_envs"
  }

  export GOOS=${platform%/*}
  export GOARCH=${platform##*/}

  # Do not set CC when building natively on a platform, only if cross-compiling
  if [[ $(kube::golang::host_platform) != "$platform" ]]; then
    # Dynamic CGO linking for other server architectures than host architecture goes here
    # If you want to include support for more server platforms than these, add arch-specific gcc names here
    case "${platform}" in
      "linux/amd64")
        export CGO_ENABLED=1
        export CC=${KUBE_LINUX_AMD64_CC:-x86_64-linux-gnu-gcc}
        ;;
      "linux/arm")
        export CGO_ENABLED=1
        export CC=${KUBE_LINUX_ARM_CC:-arm-linux-gnueabihf-gcc}
        ;;
      "linux/arm64")
        export CGO_ENABLED=1
        export CC=${KUBE_LINUX_ARM64_CC:-aarch64-linux-gnu-gcc}
        ;;
      "linux/ppc64le")
        export CGO_ENABLED=1
        export CC=${KUBE_LINUX_PPC64LE_CC:-powerpc64le-linux-gnu-gcc}
        ;;
      "linux/s390x")
        export CGO_ENABLED=1
        export CC=${KUBE_LINUX_S390X_CC:-s390x-linux-gnu-gcc}
        ;;
    esac
  fi

  # if CC is defined for platform then always enable it
  ccenv=$(echo "$platform" | awk -F/ '{print "KUBE_" toupper($1) "_" toupper($2) "_CC"}')
  if [ -n "${!ccenv-}" ]; then
    export CGO_ENABLED=1
    export CC="${!ccenv}"
  fi
}

# Ensure the go tool exists and is a viable version.
# Inputs:
#   env-var GO_VERSION is the desired go version to use, downloading it if needed (defaults to content of .go-version)
#   env-var FORCE_HOST_GO set to a non-empty value uses the go version in the $PATH and skips ensuring $GO_VERSION is used
kube::golang::internal::verify_go_version() {
  # default GO_VERSION to content of .go-version
  GO_VERSION="${GO_VERSION:-"$(cat "${KUBE_ROOT}/.go-version")"}"
  if [ "${GOTOOLCHAIN:-auto}" != 'auto' ]; then
    # no-op, just respect GOTOOLCHAIN
    :
  elif [ -n "${FORCE_HOST_GO:-}" ]; then
    # ensure existing host version is used, like before GOTOOLCHAIN existed
    export GOTOOLCHAIN='local'
  else
    # otherwise, we want to ensure the go version matches GO_VERSION
    GOTOOLCHAIN="go${GO_VERSION}"
    export GOTOOLCHAIN
    # if go is either not installed or too old to respect GOTOOLCHAIN then use gimme
    if ! (command -v go >/dev/null && [ "$(go version | cut -d' ' -f3)" = "${GOTOOLCHAIN}" ]); then
      export GIMME_ENV_PREFIX=${GIMME_ENV_PREFIX:-"${KUBE_OUTPUT}/.gimme/envs"}
      export GIMME_VERSION_PREFIX=${GIMME_VERSION_PREFIX:-"${KUBE_OUTPUT}/.gimme/versions"}
      # eval because the output of this is shell to set PATH etc.
      eval "$("${KUBE_ROOT}/third_party/gimme/gimme" "${GO_VERSION}")"
    fi
  fi

  if [[ -z "$(command -v go)" ]]; then
    kube::log::usage_from_stdin <<EOF
Can't find 'go' in PATH, please fix and retry.
See http://golang.org/doc/install for installation instructions.
EOF
    return 2
  fi

  local go_version
  IFS=" " read -ra go_version <<< "$(GOFLAGS='' go version)"
  local minimum_go_version
  # TODO: Update to go1.24 as soon we are ready to merge this
  minimum_go_version=go1.23
  if [[ "${minimum_go_version}" != $(echo -e "${minimum_go_version}\n${go_version[2]}" | sort -s -t. -k 1,1 -k 2,2n -k 3,3n | head -n1) && "${go_version[2]}" != "devel" ]]; then
    kube::log::usage_from_stdin <<EOF
Detected go version: ${go_version[*]}.
Kubernetes requires ${minimum_go_version} or greater.
Please install ${minimum_go_version} or later.
EOF
    return 2
  fi
}

# kube::golang::setup_env will check that the `go` commands is available in
# ${PATH}. It will also check that the Go version is good enough for the
# Kubernetes build.
#
# Outputs:
#   env-var GOPATH points to our local output dir
#   env-var GOBIN is unset (we want binaries in a predictable place)
#   env-var PATH includes the local GOPATH
kube::golang::setup_env() {
  # Even in module mode, we need to set GOPATH for `go build` and `go install`
  # to work.  We build various tools (usually via `go install`) from a lot of
  # scripts.
  #   * We can't just set GOBIN because that does not work on cross-compiles.
  #   * We could always use `go build -o <something>`, but it's subtle wrt
  #     cross-compiles and whether the <something> is a file or a directory,
  #     and EVERY caller has to get it *just* right.
  #   * We could leave GOPATH alone and let `go install` write binaries
  #     wherever the user's GOPATH says (or doesn't say).
  #
  # Instead we set it to a phony local path and process the results ourselves.
  # In particular, GOPATH[0]/bin will be used for `go install`, with
  # cross-compiles adding an extra directory under that.
  export GOPATH="${KUBE_GOPATH}"

  # If these are not set, set them now.  This ensures that any subsequent
  # scripts we run (which may call this function again) use the same values.
  export GOCACHE="${GOCACHE:-"${KUBE_GOPATH}/cache/build"}"
  export GOMODCACHE="${GOMODCACHE:-"${KUBE_GOPATH}/cache/mod"}"

  # Make sure our own Go binaries are in PATH.
  export PATH="${KUBE_GOPATH}/bin:${PATH}"

  # Unset GOBIN in case it already exists in the current session.
  # Cross-compiles will not work with it set.
  unset GOBIN

  # Turn on modules and workspaces (both are default-on).
  unset GO111MODULE
  unset GOWORK

  # This may try to download our specific Go version.  Do it last so it uses
  # the above-configured environment.
  kube::golang::internal::verify_go_version
}

kube::golang::setup_gomaxprocs() {
  # GOMAXPROCS by default does not reflect the number of cpu(s) available
  # when running in a container, please see https://github.com/golang/go/issues/33803
  if [[ -z "${GOMAXPROCS:-}" ]]; then
    if ! command -v ncpu >/dev/null 2>&1; then
      go -C "${KUBE_ROOT}/hack/tools" install ./ncpu || echo "Will not automatically set GOMAXPROCS"
    fi
    if command -v ncpu >/dev/null 2>&1; then
      GOMAXPROCS=$(ncpu)
      export GOMAXPROCS
      kube::log::status "Set GOMAXPROCS automatically to ${GOMAXPROCS}"
    fi
  fi
}

# This will take binaries from $GOPATH/bin and copy them to the appropriate
# place in ${KUBE_OUTPUT_BIN}
#
# Ideally this wouldn't be necessary and we could just set GOBIN to
# KUBE_OUTPUT_BIN but that won't work in the face of cross compilation.  'go
# install' will place binaries that match the host platform directly in $GOBIN
# while placing cross compiled binaries into `platform_arch` subdirs.  This
# complicates pretty much everything else we do around packaging and such.
kube::golang::place_bins() {
  local host_platform
  host_platform=$(kube::golang::host_platform)

  V=2 kube::log::status "Placing binaries"

  local platform
  for platform in "${KUBE_CLIENT_PLATFORMS[@]}"; do
    # The substitution on platform_src below will replace all slashes with
    # underscores.  It'll transform darwin/amd64 -> darwin_amd64.
    local platform_src="/${platform//\//_}"
    if [[ "${platform}" == "${host_platform}" ]]; then
      platform_src=""
      rm -f "${THIS_PLATFORM_BIN}"
      mkdir -p "$(dirname "${THIS_PLATFORM_BIN}")"
      ln -s "${KUBE_OUTPUT_BIN}/${platform}" "${THIS_PLATFORM_BIN}"
    fi

    V=3 kube::log::status "Placing binaries for ${platform} in ${KUBE_OUTPUT_BIN}/${platform}"
    local full_binpath_src="${KUBE_GOPATH}/bin${platform_src}"
    if [[ -d "${full_binpath_src}" ]]; then
      mkdir -p "${KUBE_OUTPUT_BIN}/${platform}"
      find "${full_binpath_src}" -maxdepth 1 -type f -exec \
        rsync -pc {} "${KUBE_OUTPUT_BIN}/${platform}" \;
    fi
  done
}

# Try and replicate the native binary placement of go install without
# calling go install.
kube::golang::outfile_for_binary() {
  local binary=$1
  local platform=$2
  local output_path="${KUBE_GOPATH}/bin"
  local bin
  bin=$(basename "${binary}")
  if [[ "${platform}" != "${host_platform}" ]]; then
    output_path="${output_path}/${platform//\//_}"
  fi
  if [[ ${GOOS} == "windows" ]]; then
    bin="${bin}.exe"
  fi
  echo "${output_path}/${bin}"
}

# Argument: the name of a Kubernetes package.
# Returns 0 if the binary can be built with coverage, 1 otherwise.
# NB: this ignores whether coverage is globally enabled or not.
kube::golang::is_instrumented_package() {
  if kube::util::array_contains "$1" "${KUBE_COVERAGE_INSTRUMENTED_PACKAGES[@]}"; then
    return 0
  fi
  # Some cases, like `make kubectl`, pass $1 as "./cmd/kubectl" rather than
  # "k8s.io/kubernetes/kubectl".  Try to normalize and handle that.  We don't
  # do this always because it is a bit slow.
  pkg=$(go list -find "$1")
  if kube::util::array_contains "${pkg}" "${KUBE_COVERAGE_INSTRUMENTED_PACKAGES[@]}"; then
    return 0
  fi
  return 1
}

# Argument: the name of a Kubernetes package (e.g. k8s.io/kubernetes/cmd/kube-scheduler)
# Echos the path to a dummy test used for coverage information.
kube::golang::path_for_coverage_dummy_test() {
  local package="$1"
  local path
  path=$(go list -find -f '{{.Dir}}' "${package}")
  local name
  name=$(basename "${package}")
  echo "${path}/zz_generated_${name}_test.go"
}

# Argument: the name of a Kubernetes package (e.g. k8s.io/kubernetes/cmd/kube-scheduler).
# Creates a dummy unit test on disk in the source directory for the given package.
# This unit test will invoke the package's standard entry point when run.
kube::golang::create_coverage_dummy_test() {
  local package="$1"
  local name
  name="$(basename "${package}")"
  cat <<EOF > "$(kube::golang::path_for_coverage_dummy_test "${package}")"
package main
import (
  "testing"
  "k8s.io/kubernetes/pkg/util/coverage"
)

func TestMain(m *testing.M) {
  // Get coverage running
  coverage.InitCoverage("${name}")

  // Go!
  main()

  // Make sure we actually write the profiling information to disk, if we make it here.
  // On long-running services, or anything that calls os.Exit(), this is insufficient,
  // so we also flush periodically with a default period of five seconds (configurable by
  // the KUBE_COVERAGE_FLUSH_INTERVAL environment variable).
  coverage.FlushCoverage()
}
EOF
}

# Argument: the name of a Kubernetes package (e.g. k8s.io/kubernetes/cmd/kube-scheduler).
# Deletes a test generated by kube::golang::create_coverage_dummy_test.
# It is not an error to call this for a nonexistent test.
kube::golang::delete_coverage_dummy_test() {
  local package="$1"
  rm -f "$(kube::golang::path_for_coverage_dummy_test "${package}")"
}

# Arguments: a list of kubernetes packages to build.
# Expected variables: ${build_args} should be set to an array of Go build arguments.
# In addition, ${package} and ${platform} should have been set earlier, and if
# ${KUBE_BUILD_WITH_COVERAGE} is set, coverage instrumentation will be enabled.
#
# Invokes Go to actually build some packages. If coverage is disabled, simply invokes
# go install. If coverage is enabled, builds covered binaries using go test, temporarily
# producing the required unit test files and then cleaning up after itself.
# Non-covered binaries are then built using go install as usual.
#
# See comments in kube::golang::setup_env regarding where built binaries go.
kube::golang::build_some_binaries() {
  if [[ -n "${KUBE_BUILD_WITH_COVERAGE:-}" ]]; then
    local -a uncovered=()
    for package in "$@"; do
      if kube::golang::is_instrumented_package "${package}"; then
        V=2 kube::log::info "Building ${package} with coverage..."

        kube::golang::create_coverage_dummy_test "${package}"
        kube::util::trap_add "kube::golang::delete_coverage_dummy_test \"${package}\"" EXIT

        go test -c -o "$(kube::golang::outfile_for_binary "${package}" "${platform}")" \
          -covermode count \
          -coverpkg k8s.io/... \
          "${build_args[@]}" \
          -tags coverage \
          "${package}"
      else
        uncovered+=("${package}")
      fi
    done
    if [[ "${#uncovered[@]}" != 0 ]]; then
      V=2 kube::log::info "Building ${uncovered[*]} without coverage..."
      GOPROXY=off go install "${build_args[@]}" "${uncovered[@]}"
    else
      V=2 kube::log::info "Nothing to build without coverage."
    fi
  else
    V=2 kube::log::info "Coverage is disabled."
    GOPROXY=off go install "${build_args[@]}" "$@"
  fi
}

# Args:
#  $1: platform (e.g. darwin/amd64)
kube::golang::build_binaries_for_platform() {
  # This is for sanity.  Without it, user umasks can leak through.
  umask 0022

  local platform=$1

  local -a statics=()
  local -a nonstatics=()
  local -a tests=()

  for binary in "${binaries[@]}"; do
    if [[ "${binary}" =~ ".test"$ ]]; then
      tests+=("${binary}")
      kube::log::info "    ${binary} (test)"
    elif kube::golang::is_statically_linked "${binary}"; then
      statics+=("${binary}")
      kube::log::info "    ${binary} (static)"
    else
      nonstatics+=("${binary}")
      kube::log::info "    ${binary} (non-static)"
    fi
   done

  V=2 kube::log::info "Env for ${platform}: GOPATH=${GOPATH-} GOOS=${GOOS-} GOARCH=${GOARCH-} GOROOT=${GOROOT-} CGO_ENABLED=${CGO_ENABLED-} CC=${CC-}"
  V=3 kube::log::info "Building binaries with GCFLAGS=${gogcflags} LDFLAGS=${goldflags}"

  local -a build_args
  if [[ "${#statics[@]}" != 0 ]]; then
    build_args=(
      -installsuffix=static
      ${goflags:+"${goflags[@]}"}
      -gcflags="${gogcflags}"
      -ldflags="${goldflags}"
      -tags="${gotags:-}"
    )
    CGO_ENABLED=0 kube::golang::build_some_binaries "${statics[@]}"
  fi

  if [[ "${#nonstatics[@]}" != 0 ]]; then
    build_args=(
      ${goflags:+"${goflags[@]}"}
      -gcflags="${gogcflags}"
      -ldflags="${goldflags}"
      -tags="${gotags:-}"
    )
    kube::golang::build_some_binaries "${nonstatics[@]}"
  fi

  for test in "${tests[@]:+${tests[@]}}"; do
    local outfile testpkg
    outfile=$(kube::golang::outfile_for_binary "${test}" "${platform}")
    testpkg=$(dirname "${test}")

    mkdir -p "$(dirname "${outfile}")"
    go test -c \
      ${goflags:+"${goflags[@]}"} \
      -gcflags="${gogcflags}" \
      -ldflags="${goldflags}" \
      -tags="${gotags:-}" \
      -o "${outfile}" \
      "${testpkg}"
  done
}

# Return approximate physical memory available in gigabytes.
kube::golang::get_physmem() {
  local mem

  # Linux kernel version >=3.14, in kb
  if mem=$(grep MemAvailable /proc/meminfo | awk '{ print $2 }'); then
    echo $(( mem / 1048576 ))
    return
  fi

  # Linux, in kb
  if mem=$(grep MemTotal /proc/meminfo | awk '{ print $2 }'); then
    echo $(( mem / 1048576 ))
    return
  fi

  # OS X, in bytes. Note that get_physmem, as used, should only ever
  # run in a Linux container (because it's only used in the multiple
  # platform case, which is a Dockerized build), but this is provided
  # for completeness.
  if mem=$(sysctl -n hw.memsize 2>/dev/null); then
    echo $(( mem / 1073741824 ))
    return
  fi

  # If we can't infer it, just give up and assume a low memory system
  echo 1
}

# Build binaries targets specified
#
# Input:
#   $@ - targets and go flags.  If no targets are set then all binaries targets
#     are built.
#   KUBE_BUILD_PLATFORMS - Incoming variable of targets to build for.  If unset
#     then just the host architecture is built.
kube::golang::build_binaries() {
  V=2 kube::log::info "Go version: $(GOFLAGS='' go version)"

  local host_platform
  host_platform=$(kube::golang::host_platform)

  # These are "local" but are visible to and relied on by functions this
  # function calls.  They are effectively part of the calling API to
  # build_binaries_for_platform.
  local goflags goldflags gogcflags gotags

  goflags=()
  gogcflags="${GOGCFLAGS:-}"
  goldflags="all=$(kube::version::ldflags) ${GOLDFLAGS:-}"

  if [[ "${DBG:-}" == 1 ]]; then
      # Debugging - disable optimizations and inlining and trimPath
      gogcflags="${gogcflags} all=-N -l"
  else
      # Not debugging - disable symbols and DWARF, trim embedded paths
      goldflags="${goldflags} -s -w"
      goflags+=("-trimpath")
  fi

  # Extract tags if any specified in GOFLAGS
  gotags="selinux,notest,$(echo "${GOFLAGS:-}" | sed -ne 's|.*-tags=\([^-]*\).*|\1|p')"

  local -a targets=()
  local arg

  for arg; do
    if [[ "${arg}" == -* ]]; then
      # Assume arguments starting with a dash are flags to pass to go.
      goflags+=("${arg}")
    else
      targets+=("${arg}")
    fi
  done

  local -a platforms
  IFS=" " read -ra platforms <<< "${KUBE_BUILD_PLATFORMS:-}"
  if [[ ${#platforms[@]} -eq 0 ]]; then
    platforms=("${host_platform}")
  fi

  if [[ ${#targets[@]} -eq 0 ]]; then
    targets=("${KUBE_ALL_TARGETS[@]}")
  fi
  kube::util::read-array targets < <(kube::golang::dedup "${targets[@]}")

  local -a binaries
  kube::util::read-array binaries < <(kube::golang::normalize_go_targets "${targets[@]}")
  kube::util::read-array binaries < <(kube::golang::dedup "${binaries[@]}")

  local parallel=false
  if [[ ${#platforms[@]} -gt 1 ]]; then
    local gigs
    gigs=$(kube::golang::get_physmem)

    if [[ ${gigs} -ge ${KUBE_PARALLEL_BUILD_MEMORY} ]]; then
      kube::log::status "Multiple platforms requested and available ${gigs}G >= threshold ${KUBE_PARALLEL_BUILD_MEMORY}G, building platforms in parallel"
      parallel=true
    else
      kube::log::status "Multiple platforms requested, but available ${gigs}G < threshold ${KUBE_PARALLEL_BUILD_MEMORY}G, building platforms in serial"
      parallel=false
    fi
  fi

  if [[ "${parallel}" == "true" ]]; then
    kube::log::status "Building go targets for {${platforms[*]}} in parallel (output will appear in a burst when complete):" "${targets[@]}"
    local platform
    for platform in "${platforms[@]}"; do (
        kube::golang::set_platform_envs "${platform}"
        kube::log::status "${platform}: build started"
        kube::golang::build_binaries_for_platform "${platform}"
        kube::log::status "${platform}: build finished"
      ) &> "/tmp//${platform//\//_}.build" &
    done

    local fails=0
    for job in $(jobs -p); do
      wait "${job}" || (( fails+=1 ))
    done

    for platform in "${platforms[@]}"; do
      cat "/tmp//${platform//\//_}.build"
    done

    return "${fails}"
  else
    for platform in "${platforms[@]}"; do
      kube::log::status "Building go targets for ${platform}"
      (
        kube::golang::set_platform_envs "${platform}"
        kube::golang::build_binaries_for_platform "${platform}"
      )
    done
  fi
}
