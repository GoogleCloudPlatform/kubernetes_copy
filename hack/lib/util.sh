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

function kube::util::sourced_variable {
  # Call this function to tell shellcheck that a variable is supposed to
  # be used from other calling context. This helps quiet an "unused
  # variable" warning from shellcheck and also document your code.
  true
}

kube::util::sortable_date() {
  date "+%Y%m%d-%H%M%S"
}

# arguments: target, item1, item2, item3, ...
# returns 0 if target is in the given items, 1 otherwise.
kube::util::array_contains() {
  local search="$1"
  local element
  shift
  for element; do
    if [[ "${element}" == "${search}" ]]; then
      return 0
     fi
  done
  return 1
}

kube::util::wait_for_url() {
  local url=$1
  local prefix=${2:-}
  local wait=${3:-1}
  local times=${4:-30}
  local maxtime=${5:-1}

  command -v curl >/dev/null || {
    kube::log::usage "curl must be installed"
    exit 1
  }

  local i
  for i in $(seq 1 "${times}"); do
    local out
    if out=$(curl --max-time "${maxtime}" -gkfs "${@:6}" "${url}" 2>/dev/null); then
      kube::log::status "On try ${i}, ${prefix}: ${out}"
      return 0
    fi
    sleep "${wait}"
  done
  kube::log::error "Timed out waiting for ${prefix} to answer at ${url}; tried ${times} waiting ${wait} between each"
  return 1
}

kube::util::wait_for_url_with_bearer_token() {
  local url=$1
  local token=$2
  local prefix=${3:-}
  local wait=${4:-1}
  local times=${5:-30}
  local maxtime=${6:-1}

  kube::util::wait_for_url "${url}" "${prefix}" "${wait}" "${times}" "${maxtime}" -H "Authorization: Bearer ${token}"
}

# Example:  kube::util::wait_for_success 120 5 "kubectl get nodes|grep localhost"
# arguments: wait time, sleep time, shell command
# returns 0 if the shell command get output, 1 otherwise.
kube::util::wait_for_success(){
  local wait_time="$1"
  local sleep_time="$2"
  local cmd="$3"
  while [ "$wait_time" -gt 0 ]; do
    if eval "$cmd"; then
      return 0
    else
      sleep "$sleep_time"
      wait_time=$((wait_time-sleep_time))
    fi
  done
  return 1
}

# Example:  kube::util::trap_add 'echo "in trap DEBUG"' DEBUG
# See: http://stackoverflow.com/questions/3338030/multiple-bash-traps-for-the-same-signal
kube::util::trap_add() {
  local trap_add_cmd
  trap_add_cmd=$1
  shift

  for trap_add_name in "$@"; do
    local existing_cmd
    local new_cmd

    # Grab the currently defined trap commands for this trap
    existing_cmd=$(trap -p "${trap_add_name}" |  awk -F"'" '{print $2}')

    if [[ -z "${existing_cmd}" ]]; then
      new_cmd="${trap_add_cmd}"
    else
      new_cmd="${trap_add_cmd};${existing_cmd}"
    fi

    # Assign the test. Disable the shellcheck warning telling that trap
    # commands should be single quoted to avoid evaluating them at this
    # point instead evaluating them at run time. The logic of adding new
    # commands to a single trap requires them to be evaluated right away.
    # shellcheck disable=SC2064
    trap "${new_cmd}" "${trap_add_name}"
  done
}

# Opposite of kube::util::ensure-temp-dir()
kube::util::cleanup-temp-dir() {
  rm -rf "${KUBE_TEMP}"
}

# Create a temp dir that'll be deleted at the end of this bash session.
#
# Vars set:
#   KUBE_TEMP
kube::util::ensure-temp-dir() {
  if [[ -z ${KUBE_TEMP-} ]]; then
    KUBE_TEMP=$(mktemp -d 2>/dev/null || mktemp -d -t kubernetes.XXXXXX)
    kube::util::trap_add kube::util::cleanup-temp-dir EXIT
  fi
}

kube::util::host_os() {
  local host_os
  case "$(uname -s)" in
    Darwin)
      host_os=darwin
      ;;
    Linux)
      host_os=linux
      ;;
    *)
      kube::log::error "Unsupported host OS.  Must be Linux or Mac OS X."
      exit 1
      ;;
  esac
  echo "${host_os}"
}

kube::util::host_arch() {
  local host_arch
  case "$(uname -m)" in
    x86_64*)
      host_arch=amd64
      ;;
    i?86_64*)
      host_arch=amd64
      ;;
    amd64*)
      host_arch=amd64
      ;;
    aarch64*)
      host_arch=arm64
      ;;
    arm64*)
      host_arch=arm64
      ;;
    arm*)
      host_arch=arm
      ;;
    i?86*)
      host_arch=x86
      ;;
    s390x*)
      host_arch=s390x
      ;;
    ppc64le*)
      host_arch=ppc64le
      ;;
    *)
      kube::log::error "Unsupported host arch. Must be x86_64, 386, arm, arm64, s390x or ppc64le."
      exit 1
      ;;
  esac
  echo "${host_arch}"
}

# This figures out the host platform without relying on golang.  We need this as
# we don't want a golang install to be a prerequisite to building yet we need
# this info to figure out where the final binaries are placed.
kube::util::host_platform() {
  echo "$(kube::util::host_os)/$(kube::util::host_arch)"
}

# looks for $1 in well-known output locations for the platform ($2)
# $KUBE_ROOT must be set
kube::util::find-binary-for-platform() {
  local -r lookfor="$1"
  local -r platform="$2"
  local locations=(
    "${KUBE_ROOT}/_output/bin/${lookfor}"
    "${KUBE_ROOT}/_output/dockerized/bin/${platform}/${lookfor}"
    "${KUBE_ROOT}/_output/local/bin/${platform}/${lookfor}"
    "${KUBE_ROOT}/platforms/${platform}/${lookfor}"
  )

  # if we're looking for the host platform, add local non-platform-qualified search paths
  if [[ "${platform}" = "$(kube::util::host_platform)" ]]; then
    locations+=(
      "${KUBE_ROOT}/_output/local/go/bin/${lookfor}"
      "${KUBE_ROOT}/_output/dockerized/go/bin/${lookfor}"
    );
  fi

  # looks for $1 in the $PATH
  if which "${lookfor}" >/dev/null; then
    local -r local_bin="$(which "${lookfor}")"
    locations+=( "${local_bin}"  );
  fi

  # List most recently-updated location.
  local -r bin=$( (ls -t "${locations[@]}" 2>/dev/null || true) | head -1 )

  if [[ -z "${bin}" ]]; then
    kube::log::error "Failed to find binary ${lookfor} for platform ${platform}"
    return 1
  fi

  echo -n "${bin}"
}

# looks for $1 in well-known output locations for the host platform
# $KUBE_ROOT must be set
kube::util::find-binary() {
  kube::util::find-binary-for-platform "$1" "$(kube::util::host_platform)"
}

# Takes a group/version and returns the path to its location on disk, sans
# "pkg". E.g.:
# * default behavior: extensions/v1beta1 -> apis/extensions/v1beta1
# * default behavior for only a group: experimental -> apis/experimental
# * Special handling for empty group: v1 -> api/v1, unversioned -> api/unversioned
# * Special handling for groups suffixed with ".k8s.io": foo.k8s.io/v1 -> apis/foo/v1
# * Very special handling for when both group and version are "": / -> api
#
# $KUBE_ROOT must be set.
kube::util::group-version-to-pkg-path() {
  local group_version="$1"

  # Make a list of all know APIs by listing their dirs.
  local apidirs=()
  kube::util::read-array apidirs < <(
      cd "${KUBE_ROOT}/staging/src/k8s.io/api" || return 1 # make shellcheck happy
      find . -name types.go -exec dirname {} \; \
        | sed "s|\./||g" \
        | LC_ALL=C sort -u)

  # Compare each API dir against the requested GV, and if we find it, no
  # special handling needed.
  for api in "${apidirs[@]}"; do
    # Change "foo.bar.k8s.io/v1" -> "foo/v1" notation.
    local simple_gv="${group_version/.*k8s.io/}"
    if [[ "${api}" = "${simple_gv}" ]]; then
      echo "staging/src/k8s.io/api/${simple_gv}"
      return
    fi
  done

  # "v1" is the API GroupVersion
  if [[ "${group_version}" == "v1" ]]; then
    echo "staging/src/k8s.io/api/core/v1"
    return
  fi

  # Special cases first.
  # TODO(lavalamp): Simplify this by moving pkg/api/v1 and splitting pkg/api,
  # moving the results to pkg/apis/api.
  case "${group_version}" in
    # both group and version are "", this occurs when we generate deep copies for internal objects of the legacy v1 API.
    __internal)
      echo "pkg/apis/core"
      ;;
    meta/v1)
      echo "staging/src/k8s.io/apimachinery/pkg/apis/meta/v1"
      ;;
    meta/v1beta1)
      echo "staging/src/k8s.io/apimachinery/pkg/apis/meta/v1beta1"
      ;;
    internal.apiserver.k8s.io/v1alpha1)
      echo "staging/src/k8s.io/api/apiserverinternal/v1alpha1"
      ;;
    *.k8s.io)
      echo "pkg/apis/${group_version%.*k8s.io}"
      ;;
    *.k8s.io/*)
      echo "pkg/apis/${group_version/.*k8s.io/}"
      ;;
    *)
      echo "pkg/apis/${group_version%__internal}"
      ;;
  esac
}

# Takes a group/version and returns the swagger-spec file name.
# default behavior: extensions/v1beta1 -> extensions_v1beta1
# special case for v1: v1 -> v1
kube::util::gv-to-swagger-name() {
  local group_version="$1"
  case "${group_version}" in
    v1)
      echo "v1"
      ;;
    *)
      echo "${group_version%/*}_${group_version#*/}"
      ;;
  esac
}

# Returns the name of the upstream remote repository name for the local git
# repo, e.g. "upstream" or "origin".
kube::util::git_upstream_remote_name() {
  git remote -v | grep fetch |\
    grep -E 'github.com[/:]kubernetes/kubernetes|k8s.io/kubernetes' |\
    head -n 1 | awk '{print $1}'
}

# Exits script if working directory is dirty. If it's run interactively in the terminal
# the user can commit changes in a second terminal. This script will wait.
kube::util::ensure_clean_working_dir() {
  while ! git diff HEAD --exit-code &>/dev/null; do
    echo -e "\nUnexpected dirty working directory:\n"
    if tty -s; then
        git status -s
    else
        git diff -a # be more verbose in log files without tty
        exit 1
    fi | sed 's/^/  /'
    echo -e "\nCommit your changes in another terminal and then continue here by pressing enter."
    read -r
  done 1>&2
}

# Find the base commit using:
# $PULL_BASE_SHA if set (from Prow)
# current ref from the remote upstream branch
kube::util::base_ref() {
  local -r git_branch=$1

  if [[ -n ${PULL_BASE_SHA:-} ]]; then
    echo "${PULL_BASE_SHA}"
    return
  fi

  full_branch="$(kube::util::git_upstream_remote_name)/${git_branch}"

  # make sure the branch is valid, otherwise the check will pass erroneously.
  if ! git describe "${full_branch}" >/dev/null; then
    # abort!
    exit 1
  fi

  echo "${full_branch}"
}

# Checks whether there are any files matching pattern $2 changed between the
# current branch and upstream branch named by $1.
# Returns 1 (false) if there are no changes
#         0 (true) if there are changes detected.
kube::util::has_changes() {
  local -r git_branch=$1
  local -r pattern=$2
  local -r not_pattern=${3:-totallyimpossiblepattern}

  local base_ref
  base_ref=$(kube::util::base_ref "${git_branch}")
  echo "Checking for '${pattern}' changes against '${base_ref}'"

  # notice this uses ... to find the first shared ancestor
  if git diff --name-only "${base_ref}...HEAD" | grep -v -E "${not_pattern}" | grep "${pattern}" > /dev/null; then
    return 0
  fi
  # also check for pending changes
  if git status --porcelain | grep -v -E "${not_pattern}" | grep "${pattern}" > /dev/null; then
    echo "Detected '${pattern}' uncommitted changes."
    return 0
  fi
  echo "No '${pattern}' changes detected."
  return 1
}

kube::util::download_file() {
  local -r url=$1
  local -r destination_file=$2

  rm "${destination_file}" 2&> /dev/null || true

  for i in $(seq 5)
  do
    if ! curl -fsSL --retry 3 --keepalive-time 2 "${url}" -o "${destination_file}"; then
      echo "Downloading ${url} failed. $((5-i)) retries left."
      sleep 1
    else
      echo "Downloading ${url} succeed"
      return 0
    fi
  done
  return 1
}

# Test whether openssl is installed.
# Sets:
#  OPENSSL_BIN: The path to the openssl binary to use
function kube::util::test_openssl_installed {
    if ! openssl version >& /dev/null; then
      echo "Failed to run openssl. Please ensure openssl is installed"
      exit 1
    fi

    OPENSSL_BIN=$(command -v openssl)
}

# Query the API server for client certificate authentication capabilities
function kube::util::test_client_certificate_authentication_enabled {
  local output
  kube::util::test_openssl_installed

  output=$(echo \
    | "${OPENSSL_BIN}" s_client -connect "127.0.0.1:${SECURE_API_PORT}" 2> /dev/null \
    | grep -A3 'Acceptable client certificate CA names')

  if [[ "${output}" != *"/CN=127.0.0.1"* ]] && [[ "${output}" != *"CN = 127.0.0.1"* ]]; then
    echo "API server not configured for client certificate authentication"
    echo "Output of from acceptable client certificate check: ${output}"
    exit 1
  fi
}

# creates a client CA, args are sudo, dest-dir, ca-id, purpose
# purpose is dropped in after "key encipherment", you usually want
# '"client auth"'
# '"server auth"'
# '"client auth","server auth"'
function kube::util::create_signing_certkey {
    local sudo=$1
    local dest_dir=$2
    local id=$3
    local purpose=$4
    # Create client ca
    ${sudo} /usr/bin/env bash -e <<EOF
    rm -f "${dest_dir}/${id}-ca.crt" "${dest_dir}/${id}-ca.key"
    ${OPENSSL_BIN} req -x509 -sha256 -new -nodes -days 365 -newkey rsa:2048 -keyout "${dest_dir}/${id}-ca.key" -out "${dest_dir}/${id}-ca.crt" -subj "/C=xx/ST=x/L=x/O=x/OU=x/CN=ca/emailAddress=x/"
    echo '{"signing":{"default":{"expiry":"43800h","usages":["signing","key encipherment",${purpose}]}}}' > "${dest_dir}/${id}-ca-config.json"
EOF
}

# signs a client certificate: args are sudo, dest-dir, CA, filename (roughly), username, groups...
function kube::util::create_client_certkey {
    local sudo=$1
    local dest_dir=$2
    local ca=$3
    local id=$4
    local cn=${5:-$4}
    local groups=""
    local SEP=""
    shift 5
    while [ -n "${1:-}" ]; do
        groups+="${SEP}{\"O\":\"$1\"}"
        SEP=","
        shift 1
    done
    ${sudo} /usr/bin/env bash -e <<EOF
    cd ${dest_dir}
    echo '{"CN":"${cn}","names":[${groups}],"hosts":[""],"key":{"algo":"rsa","size":2048}}' | ${CFSSL_BIN} gencert -ca=${ca}.crt -ca-key=${ca}.key -config=${ca}-config.json - | ${CFSSLJSON_BIN} -bare client-${id}
    mv "client-${id}-key.pem" "client-${id}.key"
    mv "client-${id}.pem" "client-${id}.crt"
    rm -f "client-${id}.csr"
EOF
}

# signs a serving certificate: args are sudo, dest-dir, ca, filename (roughly), subject, hosts...
function kube::util::create_serving_certkey {
    local sudo=$1
    local dest_dir=$2
    local ca=$3
    local id=$4
    local cn=${5:-$4}
    local hosts=""
    local SEP=""
    shift 5
    while [ -n "${1:-}" ]; do
        hosts+="${SEP}\"$1\""
        SEP=","
        shift 1
    done
    ${sudo} /usr/bin/env bash -e <<EOF
    cd ${dest_dir}
    echo '{"CN":"${cn}","hosts":[${hosts}],"key":{"algo":"rsa","size":2048}}' | ${CFSSL_BIN} gencert -ca=${ca}.crt -ca-key=${ca}.key -config=${ca}-config.json - | ${CFSSLJSON_BIN} -bare serving-${id}
    mv "serving-${id}-key.pem" "serving-${id}.key"
    mv "serving-${id}.pem" "serving-${id}.crt"
    rm -f "serving-${id}.csr"
EOF
}

# creates a self-contained kubeconfig: args are sudo, dest-dir, ca file, host, port, client id, token(optional)
function kube::util::write_client_kubeconfig {
    local sudo=$1
    local dest_dir=$2
    local ca_file=$3
    local api_host=$4
    local api_port=$5
    local client_id=$6
    local token=${7:-}
    cat <<EOF | ${sudo} tee "${dest_dir}"/"${client_id}".kubeconfig > /dev/null
apiVersion: v1
kind: Config
clusters:
  - cluster:
      certificate-authority: ${ca_file}
      server: https://${api_host}:${api_port}/
    name: local-up-cluster
users:
  - user:
      token: ${token}
      client-certificate: ${dest_dir}/client-${client_id}.crt
      client-key: ${dest_dir}/client-${client_id}.key
    name: local-up-cluster
contexts:
  - context:
      cluster: local-up-cluster
      user: local-up-cluster
    name: local-up-cluster
current-context: local-up-cluster
EOF

    # flatten the kubeconfig files to make them self contained
    username=$(whoami)
    ${sudo} /usr/bin/env bash -e <<EOF
    $(kube::util::find-binary kubectl) --kubeconfig="${dest_dir}/${client_id}.kubeconfig" config view --minify --flatten > "/tmp/${client_id}.kubeconfig"
    mv -f "/tmp/${client_id}.kubeconfig" "${dest_dir}/${client_id}.kubeconfig"
    chown ${username} "${dest_dir}/${client_id}.kubeconfig"
EOF
}

# list_staging_repos outputs a sorted list of repos in staging/src/k8s.io
# each entry will just be the $repo portion of staging/src/k8s.io/$repo/...
# $KUBE_ROOT must be set.
function kube::util::list_staging_repos() {
  (
    cd "${KUBE_ROOT}/staging/src/k8s.io" && \
    find . -mindepth 1 -maxdepth 1 -type d | cut -c 3- | sort
  )
}


# Determines if docker can be run, failures may simply require that the user be added to the docker group.
function kube::util::ensure_docker_daemon_connectivity {
  DOCKER_OPTS=${DOCKER_OPTS:-""}
  IFS=" " read -ra docker_opts <<< "${DOCKER_OPTS}"
  if ! docker "${docker_opts[@]:+"${docker_opts[@]}"}" info > /dev/null 2>&1 ; then
    cat <<'EOF' >&2
Can't connect to 'docker' daemon.  please fix and retry.

Possible causes:
  - Docker Daemon not started
    - Linux: confirm via your init system
    - macOS w/ Docker for Mac: Check the menu bar and start the Docker application
  - DOCKER_HOST hasn't been set or is set incorrectly
    - Linux: domain socket is used, DOCKER_* should be unset. In Bash run `unset ${!DOCKER_*}`
    - macOS w/ Docker for Mac: domain socket is used, DOCKER_* should be unset. In Bash run `unset ${!DOCKER_*}`
  - Other things to check:
    - Linux: User isn't in 'docker' group.  Add and relogin.
      - Something like 'sudo usermod -a -G docker ${USER}'
      - RHEL7 bug and workaround: https://bugzilla.redhat.com/show_bug.cgi?id=1119282#c8
EOF
    return 1
  fi
}

# Wait for background jobs to finish. Return with
# an error status if any of the jobs failed.
kube::util::wait-for-jobs() {
  local fail=0
  local job
  for job in $(jobs -p); do
    wait "${job}" || fail=$((fail + 1))
  done
  return ${fail}
}

# kube::util::join <delim> <list...>
# Concatenates the list elements with the delimiter passed as first parameter
#
# Ex: kube::util::join , a b c
#  -> a,b,c
function kube::util::join {
  local IFS="$1"
  shift
  echo "$*"
}

# Downloads cfssl/cfssljson into $1 directory if they do not already exist in PATH
#
# Assumed vars:
#   $1 (cfssl directory) (optional)
#
# Sets:
#  CFSSL_BIN: The path of the installed cfssl binary
#  CFSSLJSON_BIN: The path of the installed cfssljson binary
#
# shellcheck disable=SC2120 # optional parameters
function kube::util::ensure-cfssl {
  if command -v cfssl &>/dev/null && command -v cfssljson &>/dev/null; then
    CFSSL_BIN=$(command -v cfssl)
    CFSSLJSON_BIN=$(command -v cfssljson)
    return 0
  fi

  host_arch=$(kube::util::host_arch)

  if [[ "${host_arch}" != "amd64" ]]; then
    echo "Cannot download cfssl on non-amd64 hosts and cfssl does not appear to be installed."
    echo "Please install cfssl and cfssljson and verify they are in \$PATH."
    echo "Hint: export PATH=\$PATH:\$GOPATH/bin; go install github.com/cloudflare/cfssl/cmd/...@latest"
    exit 1
  fi

  # Create a temp dir for cfssl if no directory was given
  local cfssldir=${1:-}
  if [[ -z "${cfssldir}" ]]; then
    kube::util::ensure-temp-dir
    cfssldir="${KUBE_TEMP}/cfssl"
  fi

  mkdir -p "${cfssldir}"
  pushd "${cfssldir}" > /dev/null || return 1

    echo "Unable to successfully run 'cfssl' from ${PATH}; downloading instead..."
    kernel=$(uname -s)
    case "${kernel}" in
      Linux)
        curl --retry 10 -L -o cfssl https://github.com/cloudflare/cfssl/releases/download/v1.5.0/cfssl_1.5.0_linux_amd64
        curl --retry 10 -L -o cfssljson https://github.com/cloudflare/cfssl/releases/download/v1.5.0/cfssljson_1.5.0_linux_amd64
        ;;
      Darwin)
        curl --retry 10 -L -o cfssl https://github.com/cloudflare/cfssl/releases/download/v1.5.0/cfssl_1.5.0_darwin_amd64
        curl --retry 10 -L -o cfssljson https://github.com/cloudflare/cfssl/releases/download/v1.5.0/cfssljson_1.5.0_darwin_amd64
        ;;
      *)
        echo "Unknown, unsupported platform: ${kernel}." >&2
        echo "Supported platforms: Linux, Darwin." >&2
        exit 2
    esac

    chmod +x cfssl || true
    chmod +x cfssljson || true

    CFSSL_BIN="${cfssldir}/cfssl"
    CFSSLJSON_BIN="${cfssldir}/cfssljson"
    if [[ ! -x ${CFSSL_BIN} || ! -x ${CFSSLJSON_BIN} ]]; then
      echo "Failed to download 'cfssl'. Please install cfssl and cfssljson and verify they are in \$PATH."
      echo "Hint: export PATH=\$PATH:\$GOPATH/bin; go install github.com/cloudflare/cfssl/cmd/...@latest"
      exit 1
    fi
  popd > /dev/null || return 1
}

# kube::util::ensure-docker-buildx
# Check if we have "docker buildx" commands available
#
function kube::util::ensure-docker-buildx {
  # podman returns 0 on `docker buildx version`, docker on `docker buildx`. One of them must succeed.
  if docker buildx version >/dev/null 2>&1 || docker buildx >/dev/null 2>&1; then
    return 0
  else
    echo "ERROR: docker buildx not available. Docker 19.03 or higher is required with experimental features enabled"
    exit 1
  fi
}

# kube::util::ensure-bash-version
# Check if we are using a supported bash version
#
function kube::util::ensure-bash-version {
  # shellcheck disable=SC2004
  if ((${BASH_VERSINFO[0]}<4)) || ( ((${BASH_VERSINFO[0]}==4)) && ((${BASH_VERSINFO[1]}<2)) ); then
    echo "ERROR: This script requires a minimum bash version of 4.2, but got version of ${BASH_VERSINFO[0]}.${BASH_VERSINFO[1]}"
    if [ "$(uname)" = 'Darwin' ]; then
      echo "On macOS with homebrew 'brew install bash' is sufficient, ensure the brewed bash is found first in your PATH (check with 'type bash'), for example:"
      echo "PATH=\"/opt/homebrew/bin:\${PATH}\" make"
    fi
    exit 1
  fi
}

# kube::util::ensure-gnu-sed
# Determines which sed binary is gnu-sed on linux/darwin
#
# Sets:
#  SED: The name of the gnu-sed binary
#
function kube::util::ensure-gnu-sed {
  # NOTE: the echo below is a workaround to ensure sed is executed before the grep.
  # see: https://github.com/kubernetes/kubernetes/issues/87251
  sed_help="$(LANG=C sed --help 2>&1 || true)"
  if echo "${sed_help}" | grep -q "GNU\|BusyBox"; then
    SED="sed"
  elif command -v gsed &>/dev/null; then
    SED="gsed"
  else
    kube::log::error "Failed to find GNU sed as sed or gsed. If you are on Mac: brew install gnu-sed." >&2
    return 1
  fi
  kube::util::sourced_variable "${SED}"
}

# kube::util::ensure-gnu-date
# Determines which date binary is gnu-date on linux/darwin
#
# Sets:
#  DATE: The name of the gnu-date binary
#
function kube::util::ensure-gnu-date {
  # NOTE: the echo below is a workaround to ensure date is executed before the grep.
  # see: https://github.com/kubernetes/kubernetes/issues/87251
  date_help="$(LANG=C date --help 2>&1 || true)"
  if echo "${date_help}" | grep -q "GNU\|BusyBox"; then
    DATE="date"
  elif command -v gdate &>/dev/null; then
    DATE="gdate"
  else
    kube::log::error "Failed to find GNU date as date or gdate. If you are on Mac: brew install coreutils." >&2
    return 1
  fi
  kube::util::sourced_variable "${DATE}"
}

# kube::util::check-file-in-alphabetical-order <file>
# Check that the file is in alphabetical order
#
function kube::util::check-file-in-alphabetical-order {
  local failure_file="$1"
  if ! diff -u "${failure_file}" <(LC_ALL=C sort "${failure_file}"); then
    {
      echo
      echo "${failure_file} is not in alphabetical order. Please sort it:"
      echo
      echo "  LC_ALL=C sort -o ${failure_file} ${failure_file}"
      echo
    } >&2
    false
  fi
}

# kube::util::require-jq
# Checks whether jq is installed.
function kube::util::require-jq {
  if ! command -v jq &>/dev/null; then
    kube::log::error  "jq not found. Please install."
    return 1
  fi
}

# outputs md5 hash of $1, works on macOS and Linux
function kube::util::md5() {
  if which md5 >/dev/null 2>&1; then
    md5 -q "$1"
  else
    md5sum "$1" | awk '{ print $1 }'
  fi
}

# kube::util::read-array
# Reads in stdin and adds it line by line to the array provided. This can be
# used instead of "mapfile -t", and is bash 3 compatible.  If the named array
# exists and is an array, it will be overwritten.  Otherwise it will be unset
# and recreated.
#
# Assumed vars:
#   $1 (name of array to create/modify)
#
# Example usage:
#   kube::util::read-array files < <(ls -1)
#
# When in doubt:
#  $ W=abc         # a string
#  $ X=(a b c)     # an array
#  $ declare -A Y  # an associative array
#  $ unset Z       # not set at all
#  $ declare -p W X Y Z
#  declare -- W="abc"
#  declare -a X=([0]="a" [1]="b" [2]="c")
#  declare -A Y
#  bash: line 26: declare: Z: not found
#  $ kube::util::read-array W < <(echo -ne "1 1\n2 2\n3 3\n")
#  bash: W is defined but isn't an array
#  $ kube::util::read-array X < <(echo -ne "1 1\n2 2\n3 3\n")
#  $ kube::util::read-array Y < <(echo -ne "1 1\n2 2\n3 3\n")
#  bash: Y is defined but isn't an array
#  $ kube::util::read-array Z < <(echo -ne "1 1\n2 2\n3 3\n")
#  $ declare -p W X Y Z
#  declare -- W="abc"
#  declare -a X=([0]="1 1" [1]="2 2" [2]="3 3")
#  declare -A Y
#  declare -a Z=([0]="1 1" [1]="2 2" [2]="3 3")
function kube::util::read-array {
  if [[ -z "$1" ]]; then
    echo "usage: ${FUNCNAME[0]} <varname>" >&2
    return 1
  fi
  if [[ -n $(declare -p "$1" 2>/dev/null) ]]; then
    if ! declare -p "$1" 2>/dev/null | grep -q '^declare -a'; then
      echo "${FUNCNAME[0]}: $1 is defined but isn't an array" >&2
      return 2
    fi
  fi
  # shellcheck disable=SC2034 # this variable _is_ used
  local __read_array_i=0
  while IFS= read -r "$1[__read_array_i++]"; do :; done
  if ! eval "[[ \${$1[--__read_array_i]} ]]"; then
    unset "$1[__read_array_i]" # ensures last element isn't empty
  fi
}

# kube::util::run-in
# Changes directory to "$1", runs the rest of the arguments, and restores the initial directory
# Returns 1 if a directory change fails, the result of running the arguments otherwise
function kube::util::run-in {
  pushd "$1" > /dev/null || return 1
  shift
  "$@"
  local result=$?
  popd > /dev/null || return 1
  return $result
}

# Some useful colors.
if [[ -z "${color_start-}" ]]; then
  declare -r color_start="\033["
  declare -r color_red="${color_start}0;31m"
  declare -r color_yellow="${color_start}0;33m"
  declare -r color_green="${color_start}0;32m"
  declare -r color_blue="${color_start}1;34m"
  declare -r color_cyan="${color_start}1;36m"
  declare -r color_norm="${color_start}0m"

  kube::util::sourced_variable "${color_start}"
  kube::util::sourced_variable "${color_red}"
  kube::util::sourced_variable "${color_yellow}"
  kube::util::sourced_variable "${color_green}"
  kube::util::sourced_variable "${color_blue}"
  kube::util::sourced_variable "${color_cyan}"
  kube::util::sourced_variable "${color_norm}"
fi

# ex: ts=2 sw=2 et filetype=sh
