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

# This presents several functions for packages which want to use kubernetes
# code-generation tools.

# These functions insist that your input IDL (commented go) files be located in
# go packages following the pattern $input_pkg_root/$something_sans_slash/$api_version .
# Those $something_sans_slash will be propagated into the output directory structure.

set -o errexit
set -o nounset
set -o pipefail

KUBE_CODEGEN_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"

# Callers which want a specific tag of the k8s.io/code-generator repo should
# set the KUBE_CODEGEN_TAG to the tag name, e.g. KUBE_CODEGEN_TAG="release-1.32"
# before sourcing this file.
CODEGEN_VERSION_SPEC="${KUBE_CODEGEN_TAG:+"@${KUBE_CODEGEN_TAG}"}"

function kube::codegen::internal::findz() {
    # We use `find` rather than `git ls-files` because sometimes external
    # projects use this across repos.  This is an imperfect wrapper of find,
    # but good enough for this script.
    find "$@" -print0
}

function kube::codegen::internal::grep() {
    # We use `grep` rather than `git grep` because sometimes external projects
    # use this across repos.
    grep "$@" \
        --exclude-dir .git \
        --exclude-dir _output \
        --exclude-dir vendor
}

# Generate tagged helper code: conversions, deepcopy, and defaults
#
# USAGE: kube::codegen::gen_helpers [FLAGS] <input-dir>
#
# <input-dir>
#   The root directory under which to search for Go files which request code to
#   be generated.  This must be a local path, not a Go package.
#
#   See note at the top about package structure below that.
#
# FLAGS:
#
#   --boilerplate <string = path_to_kube_codegen_boilerplate>
#     An optional override for the header file to insert into generated files.
#
#   --extra-peer-dir <string>
#     An optional list (this flag may be specified multiple times) of "extra"
#     directories to consider during conversion generation.
#
function kube::codegen::gen_helpers() {
    local in_dir=""
    local boilerplate="${KUBE_CODEGEN_ROOT}/hack/boilerplate.go.txt"
    local v="${KUBE_VERBOSE:-0}"
    local extra_peers=()

    while [ "$#" -gt 0 ]; do
        case "$1" in
            "--boilerplate")
                boilerplate="$2"
                shift 2
                ;;
            "--extra-peer-dir")
                extra_peers+=("$2")
                shift 2
                ;;
            *)
                if [[ "$1" =~ ^-- ]]; then
                    echo "unknown argument: $1" >&2
                    return 1
                fi
                if [ -n "$in_dir" ]; then
                    echo "too many arguments: $1 (already have $in_dir)" >&2
                    return 1
                fi
                in_dir="$1"
                shift
                ;;
        esac
    done

    if [ -z "${in_dir}" ]; then
        echo "input-dir argument is required" >&2
        return 1
    fi

    (
        # To support running this from anywhere, first cd into this directory,
        # and then install with forced module mode on and fully qualified name.
        cd "${KUBE_CODEGEN_ROOT}"
        BINS=(
            conversion-gen"${CODEGEN_VERSION_SPEC}"
            deepcopy-gen"${CODEGEN_VERSION_SPEC}"
            defaulter-gen"${CODEGEN_VERSION_SPEC}"
        )
        # shellcheck disable=2046 # printf word-splitting is intentional
        GO111MODULE=on go install $(printf "k8s.io/code-generator/cmd/%s " "${BINS[@]}")
    )
    # Go installs in $GOBIN if defined, and $GOPATH/bin otherwise
    gobin="${GOBIN:-$(go env GOPATH)/bin}"

    # Deepcopy
    #
    local input_pkgs=()
    while read -r dir; do
        pkg="$(cd "${dir}" && GO111MODULE=on go list -find .)"
        input_pkgs+=("${pkg}")
    done < <(
        ( kube::codegen::internal::grep -l --null \
            -e '^\s*//\s*+k8s:deepcopy-gen=' \
            -r "${in_dir}" \
            --include '*.go' \
            || true \
        ) | while read -r -d $'\0' F; do dirname "${F}"; done \
          | LC_ALL=C sort -u
    )

    if [ "${#input_pkgs[@]}" != 0 ]; then
        echo "Generating deepcopy code for ${#input_pkgs[@]} targets"

        kube::codegen::internal::findz \
            "${in_dir}" \
            -type f \
            -name zz_generated.deepcopy.go \
            | xargs -0 rm -f

        "${gobin}/deepcopy-gen" \
            -v "${v}" \
            --output-file zz_generated.deepcopy.go \
            --go-header-file "${boilerplate}" \
            "${input_pkgs[@]}"
    fi

    # Defaults
    #
    local input_pkgs=()
    while read -r dir; do
        pkg="$(cd "${dir}" && GO111MODULE=on go list -find .)"
        input_pkgs+=("${pkg}")
    done < <(
        ( kube::codegen::internal::grep -l --null \
            -e '^\s*//\s*+k8s:defaulter-gen=' \
            -r "${in_dir}" \
            --include '*.go' \
            || true \
        ) | while read -r -d $'\0' F; do dirname "${F}"; done \
          | LC_ALL=C sort -u
    )

    if [ "${#input_pkgs[@]}" != 0 ]; then
        echo "Generating defaulter code for ${#input_pkgs[@]} targets"

        kube::codegen::internal::findz \
            "${in_dir}" \
            -type f \
            -name zz_generated.defaults.go \
            | xargs -0 rm -f

        "${gobin}/defaulter-gen" \
            -v "${v}" \
            --output-file zz_generated.defaults.go \
            --go-header-file "${boilerplate}" \
            "${input_pkgs[@]}"
    fi

    # Conversions
    #
    local input_pkgs=()
    while read -r dir; do
        pkg="$(cd "${dir}" && GO111MODULE=on go list -find .)"
        input_pkgs+=("${pkg}")
    done < <(
        ( kube::codegen::internal::grep -l --null \
            -e '^\s*//\s*+k8s:conversion-gen=' \
            -r "${in_dir}" \
            --include '*.go' \
            || true \
        ) | while read -r -d $'\0' F; do dirname "${F}"; done \
          | LC_ALL=C sort -u
    )

    if [ "${#input_pkgs[@]}" != 0 ]; then
        echo "Generating conversion code for ${#input_pkgs[@]} targets"

        kube::codegen::internal::findz \
            "${in_dir}" \
            -type f \
            -name zz_generated.conversion.go \
            | xargs -0 rm -f

        local extra_peer_args=()
        for arg in "${extra_peers[@]:+"${extra_peers[@]}"}"; do
            extra_peer_args+=("--extra-peer-dirs" "$arg")
        done
        "${gobin}/conversion-gen" \
            -v "${v}" \
            --output-file zz_generated.conversion.go \
            --go-header-file "${boilerplate}" \
            "${extra_peer_args[@]:+"${extra_peer_args[@]}"}" \
            "${input_pkgs[@]}"
    fi
}

# Generate openapi code
#
# USAGE: kube::codegen::gen_openapi [FLAGS] <input-dir>
#
# <input-dir>
#   The root directory under which to search for Go files which request openapi
#   to be generated.  This must be a local path, not a Go package.
#
#   See note at the top about package structure below that.
#
# FLAGS:
#
#   --output-dir <string>
#     The directory into which to emit code.
#
#   --output-pkg <string>
#     The Go package path (import path) of the --output-dir.
#
#   --extra-pkgs <string>
#     An optional list of additional packages to be imported during openapi
#     generation.  The argument must be Go package syntax, e.g.
#     "k8s.io/foo/bar".  It may be a single value or a comma-delimited list.
#     This flag may be repeated.
#
#   --report-filename <string = "/dev/null">
#     An optional path at which to write an API violations report.  "-" means
#     stdout.
#
#   --update-report
#     If specified, update the report file in place, rather than diffing it.
#
#   --boilerplate <string = path_to_kube_codegen_boilerplate>
#     An optional override for the header file to insert into generated files.
#
function kube::codegen::gen_openapi() {
    local in_dir=""
    local out_dir=""
    local out_pkg=""
    local extra_pkgs=()
    local report="/dev/null"
    local update_report=""
    local boilerplate="${KUBE_CODEGEN_ROOT}/hack/boilerplate.go.txt"
    local v="${KUBE_VERBOSE:-0}"

    while [ "$#" -gt 0 ]; do
        case "$1" in
            "--output-dir")
                out_dir="$2"
                shift 2
                ;;
            "--output-pkg")
                out_pkg="$2"
                shift 2
                ;;
            "--extra-pkgs")
                extra_pkgs+=("$2")
                shift 2
                ;;
            "--report-filename")
                report="$2"
                shift 2
                ;;
            "--update-report")
                update_report="true"
                shift
                ;;
            "--boilerplate")
                boilerplate="$2"
                shift 2
                ;;
            *)
                if [[ "$1" =~ ^-- ]]; then
                    echo "unknown argument: $1" >&2
                    return 1
                fi
                if [ -n "$in_dir" ]; then
                    echo "too many arguments: $1 (already have $in_dir)" >&2
                    return 1
                fi
                in_dir="$1"
                shift
                ;;
        esac
    done

    if [ -z "${in_dir}" ]; then
        echo "input-dir argument is required" >&2
        return 1
    fi
    if [ -z "${out_dir}" ]; then
        echo "--output-dir is required" >&2
        return 1
    fi
    if [ -z "${out_pkg}" ]; then
        echo "--output-pkg is required" >&2
        return 1
    fi

    local new_report
    new_report="$(mktemp -t "$(basename "$0").api_violations.XXXXXX")"
    if [ -n "${update_report}" ]; then
        new_report="${report}"
    fi

    (
        # To support running this from anywhere, first cd into this directory,
        # and then install with forced module mode on and fully qualified name.
        cd "${KUBE_CODEGEN_ROOT}"
        BINS=(
            openapi-gen"${CODEGEN_VERSION_SPEC}"
        )
        # shellcheck disable=2046 # printf word-splitting is intentional
        GO111MODULE=on go install $(printf "k8s.io/kube-openapi/cmd/%s " "${BINS[@]}")
    )
    # Go installs in $GOBIN if defined, and $GOPATH/bin otherwise
    gobin="${GOBIN:-$(go env GOPATH)/bin}"

    local input_pkgs=( "${extra_pkgs[@]:+"${extra_pkgs[@]}"}")
    while read -r dir; do
        pkg="$(cd "${dir}" && GO111MODULE=on go list -find .)"
        input_pkgs+=("${pkg}")
    done < <(
        ( kube::codegen::internal::grep -l --null \
            -e '^\s*//\s*+k8s:openapi-gen=' \
            -r "${in_dir}" \
            --include '*.go' \
            || true \
        ) | while read -r -d $'\0' F; do dirname "${F}"; done \
          | LC_ALL=C sort -u
    )

    if [ "${#input_pkgs[@]}" != 0 ]; then
        echo "Generating openapi code for ${#input_pkgs[@]} targets"

        kube::codegen::internal::findz \
            "${in_dir}" \
            -type f \
            -name zz_generated.openapi.go \
            | xargs -0 rm -f

        "${gobin}/openapi-gen" \
            -v "${v}" \
            --output-file zz_generated.openapi.go \
            --go-header-file "${boilerplate}" \
            --output-dir "${out_dir}" \
            --output-pkg "${out_pkg}" \
            --report-filename "${new_report}" \
            "k8s.io/apimachinery/pkg/apis/meta/v1" \
            "k8s.io/apimachinery/pkg/runtime" \
            "k8s.io/apimachinery/pkg/version" \
            "${input_pkgs[@]}"
    fi

    if [ ! -e "${report}" ]; then
        touch "${report}" # in case it doesn't exist yet
    fi

    if ! diff -u "${report}" "${new_report}"; then
        echo -e "ERROR:"
        echo -e "\tAPI rule check failed for ${report}: new reported violations"
        echo -e "\tPlease read api/api-rules/README.md"
        return 1
    fi
}

# Generate client code
#
# USAGE: kube::codegen::gen_client [FLAGS] <input-dir>
#
# <input-dir>
#   The root package under which to search for Go files which request clients
#   to be generated. This must be a local path, not a Go package.
#
#   See note at the top about package structure below that.
#
# FLAGS:
#   --one-input-api <string>
#     A specific API (a directory) under the input-dir for which to generate a
#     client.  If this is not set, clients for all APIs under the input-dir
#     will be generated (under the --output-pkg).
#
#   --output-dir <string>
#     The root directory under which to emit code.  Each aspect of client
#     generation will make one or more subdirectories.
#
#   --output-pkg <string>
#     The Go package path (import path) of the --output-dir.  Each aspect of
#     client generation will make one or more sub-packages.
#
#   --boilerplate <string = path_to_kube_codegen_boilerplate>
#     An optional override for the header file to insert into generated files.
#
#   --clientset-name <string = "clientset">
#     An optional override for the leaf name of the generated "clientset" directory.
#
#   --versioned-name <string = "versioned">
#     An optional override for the leaf name of the generated
#     "<clientset>/versioned" directory.
#
#   --with-applyconfig
#     Enables generation of applyconfiguration files.
#
#   --applyconfig-name <string = "applyconfiguration">
#     An optional override for the leaf name of the generated "applyconfiguration" directory.
#
#   --applyconfig-externals <string = "">
#     An optional list of comma separated external apply configurations locations
#     in <type-package>.<type-name>:<applyconfiguration-package> form.
#
#   --with-watch
#     Enables generation of listers and informers for APIs which support WATCH.
#
#   --listers-name <string = "listers">
#     An optional override for the leaf name of the generated "listers" directory.
#
#   --informers-name <string = "informers">
#     An optional override for the leaf name of the generated "informers" directory.
#
#   --plural-exceptions <string = "">
#     An optional list of comma separated plural exception definitions in Type:PluralizedType form.
#
#   --prefers-protobuf
#     Enables generation of clientsets that use protobuf for API requests.
#
function kube::codegen::gen_client() {
    local in_dir=""
    local one_input_api=""
    local out_dir=""
    local out_pkg=""
    local clientset_subdir="clientset"
    local clientset_versioned_name="versioned"
    local applyconfig="false"
    local applyconfig_subdir="applyconfiguration"
    local applyconfig_external=""
    local applyconfig_openapi_schema=""
    local watchable="false"
    local listers_subdir="listers"
    local informers_subdir="informers"
    local boilerplate="${KUBE_CODEGEN_ROOT}/hack/boilerplate.go.txt"
    local plural_exceptions=""
    local v="${KUBE_VERBOSE:-0}"
    local prefers_protobuf="false"

    while [ "$#" -gt 0 ]; do
        case "$1" in
            "--one-input-api")
                one_input_api="/$2"
                shift 2
                ;;
            "--output-dir")
                out_dir="$2"
                shift 2
                ;;
            "--output-pkg")
                out_pkg="$2"
                shift 2
                ;;
            "--boilerplate")
                boilerplate="$2"
                shift 2
                ;;
            "--clientset-name")
                clientset_subdir="$2"
                shift 2
                ;;
            "--versioned-name")
                clientset_versioned_name="$2"
                shift 2
                ;;
            "--with-applyconfig")
                applyconfig="true"
                shift
                ;;
            "--applyconfig-name")
                applyconfig_subdir="$2"
                shift 2
                ;;
            "--applyconfig-externals")
                applyconfig_external="$2"
                shift 2
                ;;
            "--applyconfig-openapi-schema")
                applyconfig_openapi_schema="$2"
                shift 2
                ;;
            "--with-watch")
                watchable="true"
                shift
                ;;
            "--listers-name")
                listers_subdir="$2"
                shift 2
                ;;
            "--informers-name")
                informers_subdir="$2"
                shift 2
                ;;
            "--plural-exceptions")
                plural_exceptions="$2"
                shift 2
                ;;
            "--prefers-protobuf")
                prefers_protobuf="true"
                shift
                ;;
            *)
                if [[ "$1" =~ ^-- ]]; then
                    echo "unknown argument: $1" >&2
                    return 1
                fi
                if [ -n "$in_dir" ]; then
                    echo "too many arguments: $1 (already have $in_dir)" >&2
                    return 1
                fi
                in_dir="$1"
                shift
                ;;
        esac
    done

    if [ -z "${in_dir}" ]; then
        echo "input-dir argument is required" >&2
        return 1
    fi
    if [ -z "${out_dir}" ]; then
        echo "--output-dir is required" >&2
        return 1
    fi
    if [ -z "${out_pkg}" ]; then
        echo "--output-pkg is required" >&2
    fi

    mkdir -p "${out_dir}"

    (
        # To support running this from anywhere, first cd into this directory,
        # and then install with forced module mode on and fully qualified name.
        cd "${KUBE_CODEGEN_ROOT}"
        BINS=(
            applyconfiguration-gen"${CODEGEN_VERSION_SPEC}"
            client-gen"${CODEGEN_VERSION_SPEC}"
            informer-gen"${CODEGEN_VERSION_SPEC}"
            lister-gen"${CODEGEN_VERSION_SPEC}"
        )
        # shellcheck disable=2046 # printf word-splitting is intentional
        GO111MODULE=on go install $(printf "k8s.io/code-generator/cmd/%s " "${BINS[@]}")
    )
    # Go installs in $GOBIN if defined, and $GOPATH/bin otherwise
    gobin="${GOBIN:-$(go env GOPATH)/bin}"

    local group_versions=()
    local input_pkgs=()
    while read -r dir; do
        pkg="$(cd "${dir}" && GO111MODULE=on go list -find .)"
        leaf="$(basename "${dir}")"
        if grep -E -q '^v[0-9]+((alpha|beta)[0-9]+)?$' <<< "${leaf}"; then
            input_pkgs+=("${pkg}")

            dir2="$(dirname "${dir}")"
            leaf2="$(basename "${dir2}")"
            group_versions+=("${leaf2}/${leaf}")
        fi
    done < <(
        ( kube::codegen::internal::grep -l --null \
            -e '^\s*//\s*+genclient' \
            -r "${in_dir}${one_input_api}" \
            --include '*.go' \
            || true \
        ) | while read -r -d $'\0' F; do dirname "${F}"; done \
          | LC_ALL=C sort -u
    )

    if [ "${#group_versions[@]}" == 0 ]; then
        return 0
    fi

    applyconfig_pkg="" # set this for later use, iff enabled
    if [ "${applyconfig}" == "true" ]; then
        applyconfig_pkg="${out_pkg}/${applyconfig_subdir}"

        echo "Generating applyconfig code for ${#input_pkgs[@]} targets"

        ( kube::codegen::internal::grep -l --null \
            -e '^// Code generated by applyconfiguration-gen. DO NOT EDIT.$' \
            -r "${out_dir}/${applyconfig_subdir}" \
            --include '*.go' \
            || true \
        ) | xargs -0 rm -f

        "${gobin}/applyconfiguration-gen" \
            -v "${v}" \
            --go-header-file "${boilerplate}" \
            --output-dir "${out_dir}/${applyconfig_subdir}" \
            --output-pkg "${applyconfig_pkg}" \
            --external-applyconfigurations "${applyconfig_external}" \
            --openapi-schema "${applyconfig_openapi_schema}" \
            "${input_pkgs[@]}"
    fi

    echo "Generating client code for ${#group_versions[@]} targets"

    ( kube::codegen::internal::grep -l --null \
        -e '^// Code generated by client-gen. DO NOT EDIT.$' \
        -r "${out_dir}/${clientset_subdir}" \
        --include '*.go' \
        || true \
    ) | xargs -0 rm -f

    local inputs=()
    for arg in "${group_versions[@]}"; do
        inputs+=("--input" "$arg")
    done
     "${gobin}/client-gen" \
        -v "${v}" \
        --go-header-file "${boilerplate}" \
        --output-dir "${out_dir}/${clientset_subdir}" \
        --output-pkg "${out_pkg}/${clientset_subdir}" \
        --clientset-name "${clientset_versioned_name}" \
        --apply-configuration-package "${applyconfig_pkg}" \
        --input-base "$(cd "${in_dir}" && pwd -P)" `# must be absolute path or Go import path"` \
        --plural-exceptions "${plural_exceptions}" \
        --prefers-protobuf="${prefers_protobuf}" \
        "${inputs[@]}"

    if [ "${watchable}" == "true" ]; then
        echo "Generating lister code for ${#input_pkgs[@]} targets"

        ( kube::codegen::internal::grep -l --null \
            -e '^// Code generated by lister-gen. DO NOT EDIT.$' \
            -r "${out_dir}/${listers_subdir}" \
            --include '*.go' \
            || true \
        ) | xargs -0 rm -f

        "${gobin}/lister-gen" \
            -v "${v}" \
            --go-header-file "${boilerplate}" \
            --output-dir "${out_dir}/${listers_subdir}" \
            --output-pkg "${out_pkg}/${listers_subdir}" \
            --plural-exceptions "${plural_exceptions}" \
            "${input_pkgs[@]}"

        echo "Generating informer code for ${#input_pkgs[@]} targets"

        ( kube::codegen::internal::grep -l --null \
            -e '^// Code generated by informer-gen. DO NOT EDIT.$' \
            -r "${out_dir}/${informers_subdir}" \
            --include '*.go' \
            || true \
        ) | xargs -0 rm -f

        "${gobin}/informer-gen" \
            -v "${v}" \
            --go-header-file "${boilerplate}" \
            --output-dir "${out_dir}/${informers_subdir}" \
            --output-pkg "${out_pkg}/${informers_subdir}" \
            --versioned-clientset-package "${out_pkg}/${clientset_subdir}/${clientset_versioned_name}" \
            --listers-package "${out_pkg}/${listers_subdir}" \
            --plural-exceptions "${plural_exceptions}" \
            "${input_pkgs[@]}"
    fi
}

# Generate register code
#
# USAGE: kube::codegen::gen_register [FLAGS] <input-dir>
#
# <input-dir>
#   The root directory under which to search for Go files which request code to
#   be generated.  This must be a local path, not a Go package.
#
#   See note at the top about package structure below that.
#
# FLAGS:
#
#   --boilerplate <string = path_to_kube_codegen_boilerplate>
#     An optional override for the header file to insert into generated files.
#
function kube::codegen::gen_register() {
    local in_dir=""
    local boilerplate="${KUBE_CODEGEN_ROOT}/hack/boilerplate.go.txt"
    local v="${KUBE_VERBOSE:-0}"

    while [ "$#" -gt 0 ]; do
        case "$1" in
            "--boilerplate")
                boilerplate="$2"
                shift 2
                ;;
            *)
                if [[ "$1" =~ ^-- ]]; then
                    echo "unknown argument: $1" >&2
                    return 1
                fi
                if [ -n "$in_dir" ]; then
                    echo "too many arguments: $1 (already have $in_dir)" >&2
                    return 1
                fi
                in_dir="$1"
                shift
                ;;
        esac
    done

    if [ -z "${in_dir}" ]; then
        echo "input-dir argument is required" >&2
        return 1
    fi

    (
        # To support running this from anywhere, first cd into this directory,
        # and then install with forced module mode on and fully qualified name.
        cd "${KUBE_CODEGEN_ROOT}"
        BINS=(
            register-gen"${CODEGEN_VERSION_SPEC}"
        )
        # shellcheck disable=2046 # printf word-splitting is intentional
        GO111MODULE=on go install $(printf "k8s.io/code-generator/cmd/%s " "${BINS[@]}")
    )
    # Go installs in $GOBIN if defined, and $GOPATH/bin otherwise
    gobin="${GOBIN:-$(go env GOPATH)/bin}"

    # Register
    #
    local input_pkgs=()
    while read -r dir; do
        pkg="$(cd "${dir}" && GO111MODULE=on go list -find .)"
        input_pkgs+=("${pkg}")
    done < <(
        ( kube::codegen::internal::grep -l --null \
            -e '^\s*//\s*+groupName' \
            -r "${in_dir}" \
            --include '*.go' \
            || true \
        ) | while read -r -d $'\0' F; do dirname "${F}"; done \
          | LC_ALL=C sort -u
    )

    if [ "${#input_pkgs[@]}" != 0 ]; then
        echo "Generating register code for ${#input_pkgs[@]} targets"

        kube::codegen::internal::findz \
            "${in_dir}" \
            -type f \
            -name zz_generated.register.go \
            | xargs -0 rm -f

        "${gobin}/register-gen" \
            -v "${v}" \
            --output-file zz_generated.register.go \
            --go-header-file "${boilerplate}" \
            "${input_pkgs[@]}"
    fi
}


# Generate register code
#
# USAGE: kube::codegen::prerelease [FLAGS] <input-dir>
# <input-dir>
#   The root directory under which to search for Go files which request code to
#   be generated.  This must be a local path, not a Go package.
#
#   See note at the top about package structure below that.
#
# FLAGS:
#
#   --boilerplate <string = path_to_kube_codegen_boilerplate>
#     An optional override for the header file to insert into generated files.
#
function kube::codegen::prerelease() {
    local in_dir=""
    local boilerplate="${KUBE_CODEGEN_ROOT}/hack/boilerplate.go.txt"
    local v="${KUBE_VERBOSE:-0}"

    while [ "$#" -gt 0 ]; do
        case "$1" in
            "--boilerplate")
                boilerplate="$2"
                shift 2
                ;;
            *)
                if [[ "$1" =~ ^-- ]]; then
                    echo "unknown argument: $1" >&2
                    return 1
                fi
                if [ -n "$in_dir" ]; then
                    echo "too many arguments: $1 (already have $in_dir)" >&2
                    return 1
                fi
                in_dir="$1"
                shift
                ;;
        esac
    done

    if [ -z "${in_dir}" ]; then
        echo "input-dir argument is required" >&2
        return 1
    fi

    (
        # To support running this from anywhere, first cd into this directory,
        # and then install with forced module mode on and fully qualified name.
        cd "${KUBE_CODEGEN_ROOT}"
        BINS=(
            prerelease-lifecycle-gen
        )
        # shellcheck disable=2046 # printf word-splitting is intentional
        GO111MODULE=on go install $(printf "k8s.io/code-generator/cmd/%s " "${BINS[@]}")
    )
    # Go installs in $GOBIN if defined, and $GOPATH/bin otherwise
    gobin="${GOBIN:-$(go env GOPATH)/bin}"

    # Prerelease lifecycle
    #
    local input_pkgs=()
    while read -r dir; do
        pkg="$(cd "${dir}" && GO111MODULE=on go list -find .)"
        input_pkgs+=("${pkg}")
    done < <(
        ( kube::codegen::internal::grep -l --null \
            -e '^\s*//\s*+k8s:prerelease-lifecycle-gen=' \
            -r "${in_dir}" \
            --include '*.go' \
            || true \
        ) | while read -r -d $'\0' F; do dirname "${F}"; done \
          | LC_ALL=C sort -u
    )

    if [ "${#input_pkgs[@]}" != 0 ]; then
        echo "Generating prerelease lifecycle code for ${#input_pkgs[@]} targets"

        kube::codegen::internal::findz \
            "${in_dir}" \
            -type f \
            -name zz_generated.prerelease-lifecycle.go \
            | xargs -0 rm -f

        "${gobin}/prerelease-lifecycle-gen" \
            -v "${v}" \
            --output-file zz_generated.prerelease-lifecycle.go \
            --go-header-file "${boilerplate}" \
            "${input_pkgs[@]}"
    fi
}
