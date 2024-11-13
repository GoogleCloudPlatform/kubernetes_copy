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

# A set of helpers for starting/running etcd for tests

ETCD_VERSION=${ETCD_VERSION:-3.5.17}
ETCD_HOST=${ETCD_HOST:-127.0.0.1}
ETCD_PORT=${ETCD_PORT:-2379}
# This is intentionally not called ETCD_LOG_LEVEL:
# etcd checks that and compains when it is set in addition
# to the command line argument, even when both have the same value.
ETCD_LOGLEVEL=${ETCD_LOGLEVEL:-warn}
export KUBE_INTEGRATION_ETCD_URL="http://${ETCD_HOST}:${ETCD_PORT}"

kube::etcd::validate() {
  # validate if in path
  command -v etcd >/dev/null || {
    kube::log::usage "etcd must be in your PATH"
    kube::log::info "You can use 'hack/install-etcd.sh' to install a copy in third_party/."
    exit 1
  }

  # validate etcd port is free
  local port_check_command
  if command -v ss &> /dev/null && ss -Version | grep 'iproute2' &> /dev/null; then
    port_check_command="ss"
  elif command -v netstat &>/dev/null; then
    port_check_command="netstat"
  else
    kube::log::usage "unable to identify if etcd is bound to port ${ETCD_PORT}. unable to find ss or netstat utilities."
    exit 1
  fi
  if ${port_check_command} -nat | grep "LISTEN" | grep "[\.:]${ETCD_PORT:?}" >/dev/null 2>&1; then
    kube::log::usage "unable to start etcd as port ${ETCD_PORT} is in use. please stop the process listening on this port and retry."
    kube::log::usage "$(${port_check_command} -nat | grep "LISTEN" | grep "[\.:]${ETCD_PORT:?}")"
    exit 1
  fi

  # need set the env of "ETCD_UNSUPPORTED_ARCH" on unstable arch.
  arch=$(uname -m)
  if [[ $arch =~ arm* ]]; then
	  export ETCD_UNSUPPORTED_ARCH=arm
  fi
  # validate installed version is at least equal to minimum
  version=$(etcd --version | grep Version | head -n 1 | cut -d " " -f 3)
  if [[ $(kube::etcd::version "${ETCD_VERSION}") -gt $(kube::etcd::version "${version}") ]]; then
   export PATH=${KUBE_ROOT}/third_party/etcd:${PATH}
   hash etcd
   echo "${PATH}"
   version=$(etcd --version | grep Version | head -n 1 | cut -d " " -f 3)
   if [[ $(kube::etcd::version "${ETCD_VERSION}") -gt $(kube::etcd::version "${version}") ]]; then
    kube::log::usage "etcd version ${ETCD_VERSION} or greater required."
    kube::log::info "You can use 'hack/install-etcd.sh' to install a copy in third_party/."
    exit 1
   fi
  fi
}

kube::etcd::version() {
  printf '%s\n' "${@}" | awk -F . '{ printf("%d%03d%03d\n", $1, $2, $3) }'
}

kube::etcd::start() {
  # validate before running
  kube::etcd::validate

  # Start etcd
  ETCD_DIR=${ETCD_DIR:-$(mktemp -d 2>/dev/null || mktemp -d -t test-etcd.XXXXXX)}
  if [[ -d "${ARTIFACTS:-}" ]]; then
    ETCD_LOGFILE="${ARTIFACTS}/etcd.$(uname -n).$(id -un).log.DEBUG.$(date +%Y%m%d-%H%M%S).$$"
  else
    ETCD_LOGFILE=${ETCD_LOGFILE:-"/dev/null"}
  fi
  kube::log::info "etcd --advertise-client-urls ${KUBE_INTEGRATION_ETCD_URL} --data-dir ${ETCD_DIR} --listen-client-urls http://${ETCD_HOST}:${ETCD_PORT} --log-level=${ETCD_LOGLEVEL} 2> \"${ETCD_LOGFILE}\" >/dev/null"
  etcd --advertise-client-urls "${KUBE_INTEGRATION_ETCD_URL}" --data-dir "${ETCD_DIR}" --listen-client-urls "${KUBE_INTEGRATION_ETCD_URL}" --log-level="${ETCD_LOGLEVEL}" 2> "${ETCD_LOGFILE}" >/dev/null &
  ETCD_PID=$!

  echo "Waiting for etcd to come up."
  kube::util::wait_for_url "${KUBE_INTEGRATION_ETCD_URL}/health" "etcd: " 0.25 80
  curl -fs -X POST "${KUBE_INTEGRATION_ETCD_URL}/v3/kv/put" -d '{"key": "X3Rlc3Q=", "value": ""}'
}

kube::etcd::start_scraping() {
  if [[ -d "${ARTIFACTS:-}" ]]; then
    ETCD_SCRAPE_DIR="${ARTIFACTS}/etcd-scrapes"
  else
    ETCD_SCRAPE_DIR=$(mktemp -d -t test.XXXXXX)/etcd-scrapes
  fi
  kube::log::info "Periodically scraping etcd to ${ETCD_SCRAPE_DIR} ."
  mkdir -p "${ETCD_SCRAPE_DIR}"
  (
    while sleep 30; do
      kube::etcd::scrape
    done
  ) &
  ETCD_SCRAPE_PID=$!
}

kube::etcd::scrape() {
    curl -s -S "${KUBE_INTEGRATION_ETCD_URL}/metrics" > "${ETCD_SCRAPE_DIR}/next" && mv "${ETCD_SCRAPE_DIR}/next" "${ETCD_SCRAPE_DIR}/$(date +%s).scrape"
}


kube::etcd::stop() {
  if [[ -n "${ETCD_SCRAPE_PID:-}" ]] && [[ -n "${ETCD_SCRAPE_DIR:-}" ]] ; then
    kill "${ETCD_SCRAPE_PID}" &>/dev/null || :
    wait "${ETCD_SCRAPE_PID}" &>/dev/null || :
    kube::etcd::scrape || :
    (
      # shellcheck disable=SC2015
      cd "${ETCD_SCRAPE_DIR}"/.. && \
      tar czf etcd-scrapes.tgz etcd-scrapes && \
      rm -rf etcd-scrapes || :
    )
  fi
  if [[ -n "${ETCD_PID-}" ]]; then
    kill "${ETCD_PID}" &>/dev/null || :
    wait "${ETCD_PID}" &>/dev/null || :
  fi
}

kube::etcd::clean_etcd_dir() {
  if [[ -n "${ETCD_DIR-}" ]]; then
    rm -rf "${ETCD_DIR}"
  fi
}

kube::etcd::cleanup() {
  kube::etcd::stop
  kube::etcd::clean_etcd_dir
}

kube::etcd::install() {
  # Make sure that we will abort if the inner shell fails.
  set -o errexit
  set -o pipefail
  set -o nounset

  # We change directories below, so this subshell is needed.
  (
    local os
    local arch

    os=$(kube::util::host_os)
    arch=$(kube::util::host_arch)

    cd "${KUBE_ROOT}/third_party" || return 1
    if [[ $(readlink etcd) == etcd-v${ETCD_VERSION}-${os}-* ]]; then
      V=4 kube::log::info "etcd v${ETCD_VERSION} is already installed"
      return 0 # already installed
    fi

    if [[ ${os} == "darwin" ]]; then
      download_file="etcd-v${ETCD_VERSION}-${os}-${arch}.zip"
      url="https://github.com/etcd-io/etcd/releases/download/v${ETCD_VERSION}/${download_file}"
      kube::util::download_file "${url}" "${download_file}"
      unzip -o "${download_file}"
      ln -fns "etcd-v${ETCD_VERSION}-${os}-${arch}" etcd
      rm "${download_file}"
    elif [[ ${os} == "linux" ]]; then
      url="https://github.com/etcd-io/etcd/releases/download/v${ETCD_VERSION}/etcd-v${ETCD_VERSION}-${os}-${arch}.tar.gz"
      download_file="etcd-v${ETCD_VERSION}-${os}-${arch}.tar.gz"
      kube::util::download_file "${url}" "${download_file}"
      tar xzf "${download_file}"
      ln -fns "etcd-v${ETCD_VERSION}-${os}-${arch}" etcd
      rm "${download_file}"
    else
      kube::log::info "${os} is NOT supported."
      return 1
    fi
    V=4 kube::log::info "installed etcd v${ETCD_VERSION}"
    return 0 # newly installed
  )
  # Through the magic of errexit, we will not get here if the above shell
  # fails!
  PATH="${KUBE_ROOT}/third_party/etcd:${PATH}" # export into current process
  export PATH
  V=3 kube::log::info "added etcd to PATH: ${KUBE_ROOT}/third_party/etcd"
}
