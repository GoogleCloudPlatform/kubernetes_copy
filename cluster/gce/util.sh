#!/usr/bin/env bash

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

# A library of helper functions and constant for the local config.

# Use the config file specified in $KUBE_CONFIG_FILE, or default to
# config-default.sh.
readonly GCE_MAX_LOCAL_SSD=8

BASE64_HELP=$(base64 --help)
BASE64_INPUT_FLAG=""
if [[ $BASE64_HELP =~ "--input" ]]; then
  BASE64_INPUT_FLAG="--input"
else
    BASE64_INPUT_FLAG=""
fi

KUBE_ROOT=$(dirname "${BASH_SOURCE[0]}")/../..
source "${KUBE_ROOT}/cluster/gce/${KUBE_CONFIG_FILE-"config-default.sh"}"
source "${KUBE_ROOT}/cluster/common.sh"
source "${KUBE_ROOT}/hack/lib/util.sh"

if [[ "${NODE_OS_DISTRIBUTION}" == "gci" || "${NODE_OS_DISTRIBUTION}" == "ubuntu" || "${NODE_OS_DISTRIBUTION}" == "custom" ]]; then
  source "${KUBE_ROOT}/cluster/gce/${NODE_OS_DISTRIBUTION}/node-helper.sh"
else
  echo "Cannot operate on cluster using node os distro: ${NODE_OS_DISTRIBUTION}" >&2
  exit 1
fi

source "${KUBE_ROOT}/cluster/gce/windows/node-helper.sh"

if [[ "${MASTER_OS_DISTRIBUTION}" == "trusty" || "${MASTER_OS_DISTRIBUTION}" == "gci" || "${MASTER_OS_DISTRIBUTION}" == "ubuntu" ]]; then
  source "${KUBE_ROOT}/cluster/gce/${MASTER_OS_DISTRIBUTION}/master-helper.sh"
else
  echo "Cannot operate on cluster using master os distro: ${MASTER_OS_DISTRIBUTION}" >&2
  exit 1
fi

if [[ ${NODE_LOCAL_SSDS:-} -ge 1 ]] && [[ -n ${NODE_LOCAL_SSDS_EXT:-} ]] ; then
  echo -e "${color_red:-}Local SSD: Only one of NODE_LOCAL_SSDS and NODE_LOCAL_SSDS_EXT can be specified at once${color_norm:-}" >&2
  exit 2
fi

if [[ "${MASTER_OS_DISTRIBUTION}" == "gci" ]]; then
    DEFAULT_GCI_PROJECT=google-containers
    if [[ "${GCI_VERSION}" == "cos"* ]] || [[ "${MASTER_IMAGE_FAMILY}" == "cos"* ]]; then
        DEFAULT_GCI_PROJECT=cos-cloud
    fi
    export MASTER_IMAGE_PROJECT=${KUBE_GCE_MASTER_PROJECT:-${DEFAULT_GCI_PROJECT}}

    # If the master image is not set, we use the latest image based on image
    # family.
    kube_master_image="${KUBE_GCE_MASTER_IMAGE:-${GCI_VERSION}}"
    if [[ -z "${kube_master_image}" ]]; then
      kube_master_image=$(gcloud compute images list --project="${MASTER_IMAGE_PROJECT}" --no-standard-images --filter="family:${MASTER_IMAGE_FAMILY}" --format 'value(name)')
    fi

    echo "Using image: ${kube_master_image} from project: ${MASTER_IMAGE_PROJECT} as master image" >&2
    export MASTER_IMAGE="${kube_master_image}"
fi

# Sets node image based on the specified os distro. Currently this function only
# supports gci and debian.
#
# Requires:
#   NODE_OS_DISTRIBUTION
# Sets:
#   DEFAULT_GCI_PROJECT
#   NODE_IMAGE
#   NODE_IMAGE_PROJECT
function set-linux-node-image() {
  if [[ "${NODE_OS_DISTRIBUTION}" == "gci" ]]; then
    DEFAULT_GCI_PROJECT=google-containers
    if [[ "${GCI_VERSION}" == "cos"* ]] || [[ "${NODE_IMAGE_FAMILY}" == "cos"* ]]; then
      DEFAULT_GCI_PROJECT=cos-cloud
    fi

    # If the node image is not set, we use the latest image based on image
    # family.
    # Otherwise, we respect whatever is set by the user.
    NODE_IMAGE_PROJECT=${KUBE_GCE_NODE_PROJECT:-${DEFAULT_GCI_PROJECT}}
    local kube_node_image

    kube_node_image="${KUBE_GCE_NODE_IMAGE:-${GCI_VERSION}}"
    if [[ -z "${kube_node_image}" ]]; then
      kube_node_image=$(gcloud compute images list --project="${NODE_IMAGE_PROJECT}" --no-standard-images --filter="family:${NODE_IMAGE_FAMILY}" --format 'value(name)')
    fi

    echo "Using image: ${kube_node_image} from project: ${NODE_IMAGE_PROJECT} as node image" >&2
    export NODE_IMAGE="${kube_node_image}"
  fi
}

# Requires:
#   WINDOWS_NODE_OS_DISTRIBUTION
# Sets:
#   WINDOWS_NODE_IMAGE_PROJECT
#   WINDOWS_NODE_IMAGE
function set-windows-node-image() {
  WINDOWS_NODE_IMAGE_PROJECT="windows-cloud"
  if [[ "${WINDOWS_NODE_OS_DISTRIBUTION}" == "win2019" ]]; then
    WINDOWS_NODE_IMAGE="windows-server-2019-dc-core-v20210914"
  elif [[ "${WINDOWS_NODE_OS_DISTRIBUTION}" == "win1909" ]]; then
    WINDOWS_NODE_IMAGE="windows-server-1909-dc-core-v20210413"
  elif [[ "${WINDOWS_NODE_OS_DISTRIBUTION}" == "win2004" ]]; then
    WINDOWS_NODE_IMAGE="windows-server-2004-dc-core-v20210914"
  elif [[ "${WINDOWS_NODE_OS_DISTRIBUTION,,}" == "win20h2" ]]; then
    WINDOWS_NODE_IMAGE="windows-server-20h2-dc-core-v20210914"
  elif [[ "${WINDOWS_NODE_OS_DISTRIBUTION,,}" == "win2022" ]]; then
    WINDOWS_NODE_IMAGE="windows-server-2022-dc-core-v20220513"
  else
    echo "Unknown WINDOWS_NODE_OS_DISTRIBUTION ${WINDOWS_NODE_OS_DISTRIBUTION}" >&2
    exit 1
  fi
}

set-linux-node-image
set-windows-node-image

# Verify cluster autoscaler configuration.
if [[ "${ENABLE_CLUSTER_AUTOSCALER}" == "true" ]]; then
  if [[ -z $AUTOSCALER_MIN_NODES ]]; then
    echo "AUTOSCALER_MIN_NODES not set."
    exit 1
  fi
  if [[ -z $AUTOSCALER_MAX_NODES ]]; then
    echo "AUTOSCALER_MAX_NODES not set."
    exit 1
  fi
fi

# These prefixes must not be prefixes of each other, so that they can be used to
# detect mutually exclusive sets of nodes.
NODE_INSTANCE_PREFIX=${NODE_INSTANCE_PREFIX:-"${INSTANCE_PREFIX}-minion"}
WINDOWS_NODE_INSTANCE_PREFIX=${WINDOWS_NODE_INSTANCE_PREFIX:-"${INSTANCE_PREFIX}-windows-node"}

# NODE_TAG (expected to be) defined by caller
# shellcheck disable=SC2153
NODE_TAGS="${NODE_TAG}"

ALLOCATE_NODE_CIDRS=true
PREEXISTING_NETWORK=false
PREEXISTING_NETWORK_MODE=""

KUBE_PROMPT_FOR_UPDATE=${KUBE_PROMPT_FOR_UPDATE:-"n"}
# How long (in seconds) to wait for cluster initialization.
KUBE_CLUSTER_INITIALIZATION_TIMEOUT=${KUBE_CLUSTER_INITIALIZATION_TIMEOUT:-300}

function join_csv() {
  local IFS=','; echo "$*";
}

# This function returns the first string before the comma
function split_csv() {
  echo "$*" | cut -d',' -f1
}

# Verify prereqs
function verify-prereqs() {
  local cmd

  # we use openssl to generate certs
  kube::util::test_openssl_installed

  # ensure a version supported by easyrsa is installed
  if [ "$(openssl version | cut -d\  -f1)" == "LibreSSL" ]; then
    echo "LibreSSL is not supported. Please ensure openssl points to an OpenSSL binary"
    if [ "$(uname -s)" == "Darwin" ]; then
      # We want this print just the way it is
      # shellcheck disable=SC2016
      echo 'On macOS we recommend using homebrew and adding "$(brew --prefix openssl)/bin" to your PATH'
    fi
    exit 1
  fi

  # we use gcloud to create the cluster, gsutil to stage binaries and data
  for cmd in gcloud gsutil; do
    if ! which "${cmd}" >/dev/null; then
      echo "Can't find ${cmd} in PATH, please fix and retry. The Google Cloud " >&2
      echo "SDK can be downloaded from https://cloud.google.com/sdk/." >&2
      exit 1
    fi
  done
  update-or-verify-gcloud
}

# Use the gcloud defaults to find the project.  If it is already set in the
# environment then go with that.
#
# Vars set:
#   PROJECT
#   NETWORK_PROJECT
#   PROJECT_REPORTED
function detect-project() {
  if [[ -z "${PROJECT-}" ]]; then
    PROJECT=$(gcloud config list project --format 'value(core.project)')
  fi

  NETWORK_PROJECT=${NETWORK_PROJECT:-${PROJECT}}

  if [[ -z "${PROJECT-}" ]]; then
    echo "Could not detect Google Cloud Platform project.  Set the default project using " >&2
    echo "'gcloud config set project <PROJECT>'" >&2
    exit 1
  fi
  if [[ -z "${PROJECT_REPORTED-}" ]]; then
    echo "Project: ${PROJECT}" >&2
    echo "Network Project: ${NETWORK_PROJECT}" >&2
    echo "Zone: ${ZONE}" >&2
    PROJECT_REPORTED=true
  fi
}

# Use gsutil to get the md5 hash for a particular tar
function gsutil_get_tar_md5() {
  # location_tar could be local or in the cloud
  # local tar_location example ./_output/release-tars/kubernetes-server-linux-amd64.tar.gz
  # cloud tar_location example gs://kubernetes-staging-PROJECT/kubernetes-devel/kubernetes-server-linux-amd64.tar.gz
  local -r tar_location=$1
  #parse the output and return the md5 hash
  #the sed command at the end removes whitespace
  local -r tar_md5=$(gsutil hash -h -m "${tar_location}" 2>/dev/null | grep "Hash (md5):" | awk -F ':' '{print $2}' | sed 's/^[[:space:]]*//g')
  echo "${tar_md5}"
}

# Copy a release tar and its accompanying hash.
function copy-to-staging() {
  local -r staging_path=$1
  local -r gs_url=$2
  local -r tar=$3
  local -r hash=$4
  local -r basename_tar=$(basename "${tar}")

  #check whether this tar alread exists and has the same hash
  #if it matches, then don't bother uploading it again

  #remote_tar_md5 checks the remote location for the existing tarball and its md5
  #staging_path example gs://kubernetes-staging-PROJECT/kubernetes-devel
  #basename_tar example kubernetes-server-linux-amd64.tar.gz
  local -r remote_tar_md5=$(gsutil_get_tar_md5 "${staging_path}/${basename_tar}")
  if [[ -n ${remote_tar_md5} ]]; then
    #local_tar_md5 checks the remote location for the existing tarball and its md5 hash
    #tar example ./_output/release-tars/kubernetes-server-linux-amd64.tar.gz
    local -r local_tar_md5=$(gsutil_get_tar_md5 "${tar}")
    if [[ "${remote_tar_md5}" == "${local_tar_md5}" ]]; then
      echo "+++ ${basename_tar} uploaded earlier, cloud and local file md5 match (md5 = ${local_tar_md5})"
      return 0
    fi
  fi

  echo "${hash}" > "${tar}.sha512"
  gsutil -m -q -h "Cache-Control:private, max-age=0" cp "${tar}" "${tar}.sha512" "${staging_path}"
  gsutil -m acl ch -g all:R "${gs_url}" "${gs_url}.sha512" >/dev/null 2>&1 || true
  echo "+++ ${basename_tar} uploaded (sha512 = ${hash})"
}


# Given the cluster zone, return the list of regional GCS release
# bucket suffixes for the release in preference order. GCS doesn't
# give us an API for this, so we hardcode it.
#
# Assumed vars:
#   RELEASE_REGION_FALLBACK
#   REGIONAL_KUBE_ADDONS
#   ZONE
# Vars set:
#   PREFERRED_REGION
function set-preferred-region() {
  case ${ZONE} in
    asia-*)
      PREFERRED_REGION=("asia-northeast1" "us-central1" "europe-west6")
      ;;
    europe-*)
      PREFERRED_REGION=("europe-west6" "us-central1" "asia-northeast1")
      ;;
    *)
      PREFERRED_REGION=("us-central1" "europe-west6" "asia-northeast1")
      ;;
  esac

  if [[ "${RELEASE_REGION_FALLBACK}" != "true" ]]; then
    PREFERRED_REGION=( "${PREFERRED_REGION[0]}" )
  fi
}

# Take the local tar files and upload them to Google Storage.  They will then be
# downloaded by the master as part of the start up script for the master.
#
# Assumed vars:
#   PROJECT
#   SERVER_BINARY_TAR
#   KUBE_MANIFESTS_TAR
#   ZONE
# Vars set:
#   SERVER_BINARY_TAR_URL
#   SERVER_BINARY_TAR_HASH
#   NODE_BINARY_TAR_URL
#   NODE_BINARY_TAR_HASH
#   KUBE_MANIFESTS_TAR_URL
#   KUBE_MANIFESTS_TAR_HASH
function upload-tars() {
  SERVER_BINARY_TAR_URL=
  SERVER_BINARY_TAR_HASH=
  NODE_BINARY_TAR_URL=
  NODE_BINARY_TAR_HASH=
  KUBE_MANIFESTS_TAR_URL=
  KUBE_MANIFESTS_TAR_HASH=

  local project_hash
  if which md5 > /dev/null 2>&1; then
    project_hash=$(md5 -q -s "$PROJECT")
  else
    project_hash=$(echo -n "$PROJECT" | md5sum)
    project_hash=${project_hash%%[[:blank:]]*}
  fi

  # This requires 1 million projects before the probability of collision is 50%
  # that's probably good enough for now :P
  project_hash=${project_hash:0:10}

  set-preferred-region

  if [[ "${ENABLE_DOCKER_REGISTRY_CACHE:-}" == "true" ]]; then
    DOCKER_REGISTRY_MIRROR_URL="https://mirror.gcr.io"
  fi

  SERVER_BINARY_TAR_HASH=$(sha512sum-file "${SERVER_BINARY_TAR}")

  if [[ -n "${NODE_BINARY_TAR:-}" ]]; then
    NODE_BINARY_TAR_HASH=$(sha512sum-file "${NODE_BINARY_TAR}")
  fi
  if [[ -n "${KUBE_MANIFESTS_TAR:-}" ]]; then
    KUBE_MANIFESTS_TAR_HASH=$(sha512sum-file "${KUBE_MANIFESTS_TAR}")
  fi

  local server_binary_tar_urls=()
  local node_binary_tar_urls=()

  for region in "${PREFERRED_REGION[@]}"; do
    suffix="-${region}"
    local staging_bucket="gs://kubernetes-staging-${project_hash}${suffix}"

    # Ensure the buckets are created
    if ! gsutil ls "${staging_bucket}" >/dev/null; then
      echo "Creating ${staging_bucket}"
      gsutil mb -l "${region}" -p "${PROJECT}" "${staging_bucket}"
    fi

    local staging_path="${staging_bucket}/${INSTANCE_PREFIX}-devel"

    echo "+++ Staging tars to Google Storage: ${staging_path}"
    local server_binary_gs_url="${staging_path}/${SERVER_BINARY_TAR##*/}"
    copy-to-staging "${staging_path}" "${server_binary_gs_url}" "${SERVER_BINARY_TAR}" "${SERVER_BINARY_TAR_HASH}"

    if [[ -n "${NODE_BINARY_TAR:-}" ]]; then
      local node_binary_gs_url="${staging_path}/${NODE_BINARY_TAR##*/}"
      copy-to-staging "${staging_path}" "${node_binary_gs_url}" "${NODE_BINARY_TAR}" "${NODE_BINARY_TAR_HASH}"
    fi

    # Convert from gs:// URL to an https:// URL
    server_binary_tar_urls+=("${server_binary_gs_url/gs:\/\//https://storage.googleapis.com/}")
    if [[ -n "${NODE_BINARY_TAR:-}" ]]; then
      node_binary_tar_urls+=("${node_binary_gs_url/gs:\/\//https://storage.googleapis.com/}")
    fi
    if [[ -n "${KUBE_MANIFESTS_TAR:-}" ]]; then
      local kube_manifests_gs_url="${staging_path}/${KUBE_MANIFESTS_TAR##*/}"
      copy-to-staging "${staging_path}" "${kube_manifests_gs_url}" "${KUBE_MANIFESTS_TAR}" "${KUBE_MANIFESTS_TAR_HASH}"
      # Convert from gs:// URL to an https:// URL
      kube_manifests_tar_urls+=("${kube_manifests_gs_url/gs:\/\//https://storage.googleapis.com/}")
    fi
  done

  SERVER_BINARY_TAR_URL=$(join_csv "${server_binary_tar_urls[@]}")
  if [[ -n "${NODE_BINARY_TAR:-}" ]]; then
    NODE_BINARY_TAR_URL=$(join_csv "${node_binary_tar_urls[@]}")
  fi
  if [[ -n "${KUBE_MANIFESTS_TAR:-}" ]]; then
    KUBE_MANIFESTS_TAR_URL=$(join_csv "${kube_manifests_tar_urls[@]}")
  fi
}

# Detect Linux and Windows nodes created in the instance group.
#
# Assumed vars:
#   NODE_INSTANCE_PREFIX
#   WINDOWS_NODE_INSTANCE_PREFIX
# Vars set:
#   NODE_NAMES
#   INSTANCE_GROUPS
#   WINDOWS_NODE_NAMES
#   WINDOWS_INSTANCE_GROUPS
function detect-node-names() {
  detect-project
  INSTANCE_GROUPS=()
  kube::util::read-array INSTANCE_GROUPS < <(gcloud compute instance-groups managed list \
    --project "${PROJECT}" \
    --filter "name ~ '${NODE_INSTANCE_PREFIX}-.+' AND zone:(${ZONE})" \
    --format='value(name)' || true)
  WINDOWS_INSTANCE_GROUPS=()
  kube::util::read-array WINDOWS_INSTANCE_GROUPS < <(gcloud compute instance-groups managed list \
    --project "${PROJECT}" \
    --filter "name ~ '${WINDOWS_NODE_INSTANCE_PREFIX}-.+' AND zone:(${ZONE})" \
    --format='value(name)' || true)

  NODE_NAMES=()
  if [[ -n "${INSTANCE_GROUPS[*]:-}" ]]; then
    for group in "${INSTANCE_GROUPS[@]}"; do
      kube::util::read-array NODE_NAMES < <(gcloud compute instance-groups managed list-instances \
        "${group}" --zone "${ZONE}" --project "${PROJECT}" \
        --format='value(name)')
    done
  fi
  # Add heapster node name to the list too (if it exists).
  if [[ -n "${HEAPSTER_MACHINE_TYPE:-}" ]]; then
    NODE_NAMES+=("${NODE_INSTANCE_PREFIX}-heapster")
  fi
  export NODE_NAMES
  WINDOWS_NODE_NAMES=()
  if [[ -n "${WINDOWS_INSTANCE_GROUPS[*]:-}" ]]; then
    for group in "${WINDOWS_INSTANCE_GROUPS[@]}"; do
      kube::util::read-array WINDOWS_NODE_NAMES < <(gcloud compute instance-groups managed \
        list-instances "${group}" --zone "${ZONE}" --project "${PROJECT}" \
        --format='value(name)')
    done
  fi
  export WINDOWS_NODE_NAMES

  echo "INSTANCE_GROUPS=${INSTANCE_GROUPS[*]:-}" >&2
  echo "NODE_NAMES=${NODE_NAMES[*]:-}" >&2
}

# Detect the information about the minions
#
# Assumed vars:
#   ZONE
# Vars set:
#   NODE_NAMES
#   KUBE_NODE_IP_ADDRESSES (array)
function detect-nodes() {
  detect-project
  detect-node-names
  KUBE_NODE_IP_ADDRESSES=()
  for (( i=0; i<${#NODE_NAMES[@]}; i++)); do
    local node_ip
    node_ip=$(gcloud compute instances describe --project "${PROJECT}" --zone "${ZONE}" \
      "${NODE_NAMES[$i]}" --format='value(networkInterfaces[0].accessConfigs[0].natIP)')
    if [[ -z "${node_ip-}" ]] ; then
      echo "Did not find ${NODE_NAMES[$i]}" >&2
    else
      echo "Found ${NODE_NAMES[$i]} at ${node_ip}"
      KUBE_NODE_IP_ADDRESSES+=("${node_ip}")
    fi
  done
  if [[ -z "${KUBE_NODE_IP_ADDRESSES-}" ]]; then
    echo "Could not detect Kubernetes minion nodes.  Make sure you've launched a cluster with 'kube-up.sh'" >&2
    exit 1
  fi
}

# Detect the IP for the master
#
# Assumed vars:
#   MASTER_NAME
#   ZONE
#   REGION
# Vars set:
#   KUBE_MASTER
#   KUBE_MASTER_IP
function detect-master() {
  detect-project
  KUBE_MASTER=${MASTER_NAME}
  echo "Trying to find master named '${MASTER_NAME}'" >&2
  if [[ -z "${KUBE_MASTER_IP-}" ]]; then
    local master_address_name="${MASTER_NAME}-ip"
    echo "Looking for address '${master_address_name}'" >&2
    if ! KUBE_MASTER_IP=$(gcloud compute addresses describe "${master_address_name}" \
      --project "${PROJECT}" --region "${REGION}" -q --format='value(address)') || \
      [[ -z "${KUBE_MASTER_IP-}" ]]; then
      echo "Could not detect Kubernetes master node.  Make sure you've launched a cluster with 'kube-up.sh'" >&2
      exit 1
    fi
  fi
  if [[ -z "${KUBE_MASTER_INTERNAL_IP-}" ]] && [[ ${GCE_PRIVATE_CLUSTER:-} == "true" ]]; then
      local master_address_name="${MASTER_NAME}-internal-ip"
      echo "Looking for address '${master_address_name}'" >&2
      if ! KUBE_MASTER_INTERNAL_IP=$(gcloud compute addresses describe "${master_address_name}" \
        --project "${PROJECT}" --region "${REGION}" -q --format='value(address)') || \
        [[ -z "${KUBE_MASTER_INTERNAL_IP-}" ]]; then
        echo "Could not detect Kubernetes master node.  Make sure you've launched a cluster with 'kube-up.sh'" >&2
        exit 1
      fi
  fi
  echo "Using master: $KUBE_MASTER (external IP: $KUBE_MASTER_IP; internal IP: ${KUBE_MASTER_INTERNAL_IP:-(not set)})" >&2
}

function load-or-gen-kube-bearertoken() {
  if [[ -n "${KUBE_CONTEXT:-}" ]]; then
    get-kubeconfig-bearertoken
  fi
  if [[ -z "${KUBE_BEARER_TOKEN:-}" ]]; then
    gen-kube-bearertoken
  fi
}

# Figure out which binary use on the server and assure it is available.
# If KUBE_VERSION is specified use binaries specified by it, otherwise
# use local dev binaries.
#
# Assumed vars:
#   KUBE_VERSION
#   KUBE_RELEASE_VERSION_REGEX
#   KUBE_CI_VERSION_REGEX
# Vars set:
#   KUBE_TAR_HASH
#   SERVER_BINARY_TAR_URL
#   SERVER_BINARY_TAR_HASH
function tars_from_version() {
  local sha512sum=""
  if which sha512sum >/dev/null 2>&1; then
    sha512sum="sha512sum"
  else
    sha512sum="shasum -a512"
  fi

  if [[ -z "${KUBE_VERSION-}" ]]; then
    find-release-tars
    upload-tars
  elif [[ ${KUBE_VERSION} =~ ${KUBE_RELEASE_VERSION_REGEX} ]]; then
    SERVER_BINARY_TAR_URL="https://dl.k8s.io/release/${KUBE_VERSION}/kubernetes-server-linux-amd64.tar.gz"
    # TODO: Clean this up.
    KUBE_MANIFESTS_TAR_URL="${SERVER_BINARY_TAR_URL/server-linux-amd64/manifests}"
    KUBE_MANIFESTS_TAR_HASH=$(curl -L "${KUBE_MANIFESTS_TAR_URL}" --silent --show-error | ${sha512sum})
    KUBE_MANIFESTS_TAR_HASH=${KUBE_MANIFESTS_TAR_HASH%%[[:blank:]]*}
  elif [[ ${KUBE_VERSION} =~ ${KUBE_CI_VERSION_REGEX} ]]; then
    SERVER_BINARY_TAR_URL="https://storage.googleapis.com/k8s-release-dev/ci/${KUBE_VERSION}/kubernetes-server-linux-amd64.tar.gz"
    # TODO: Clean this up.
    KUBE_MANIFESTS_TAR_URL="${SERVER_BINARY_TAR_URL/server-linux-amd64/manifests}"
    KUBE_MANIFESTS_TAR_HASH=$(curl "${KUBE_MANIFESTS_TAR_URL}" --silent --show-error | ${sha512sum})
    KUBE_MANIFESTS_TAR_HASH=${KUBE_MANIFESTS_TAR_HASH%%[[:blank:]]*}
  else
    echo "Version doesn't match regexp" >&2
    exit 1
  fi
  if ! SERVER_BINARY_TAR_HASH=$(curl -Ss --fail "${SERVER_BINARY_TAR_URL}.sha512"); then
    echo "Failure trying to curl release .sha512"
  fi

  if ! curl -Ss --head "${SERVER_BINARY_TAR_URL}" >&/dev/null; then
    echo "Can't find release at ${SERVER_BINARY_TAR_URL}" >&2
    exit 1
  fi
}

# Reads kube-env metadata from master
#
# Assumed vars:
#   KUBE_MASTER
#   PROJECT
#   ZONE
function get-master-env() {
  # TODO(zmerlynn): Make this more reliable with retries.
  gcloud compute --project "${PROJECT}" ssh --zone "${ZONE}" "${KUBE_MASTER}" --command \
    "curl --fail --silent -H 'Metadata-Flavor: Google' \
      'http://metadata/computeMetadata/v1/instance/attributes/kube-env'" 2>/dev/null
  gcloud compute --project "${PROJECT}" ssh --zone "${ZONE}" "${KUBE_MASTER}" --command \
    "curl --fail --silent -H 'Metadata-Flavor: Google' \
      'http://metadata/computeMetadata/v1/instance/attributes/kube-master-certs'" 2>/dev/null
}

# Quote something appropriate for a yaml string.
#
# TODO(zmerlynn): Note that this function doesn't so much "quote" as
# "strip out quotes", and we really should be using a YAML library for
# this, but PyYAML isn't shipped by default, and *rant rant rant ... SIGH*
function yaml-quote {
  echo "${@:-}" | sed -e "s/'/''/g;s/^/'/i;s/$/'/i"
}

# Writes the cluster location into a temporary file.
# Assumed vars
#   ZONE
function write-cluster-location {
  cat >"${KUBE_TEMP}/cluster-location.txt" << EOF
${ZONE}
EOF
}

# Writes the cluster name into a temporary file.
# Assumed vars
#   CLUSTER_NAME
function write-cluster-name {
  cat >"${KUBE_TEMP}/cluster-name.txt" << EOF
${CLUSTER_NAME}
EOF
}

function write-master-env {
  # If the user requested that the master be part of the cluster, set the
  # environment variable to program the master kubelet to register itself.
  if [[ "${REGISTER_MASTER_KUBELET:-}" == "true" && -z "${KUBELET_APISERVER:-}" ]]; then
    KUBELET_APISERVER="${MASTER_NAME}"
  fi
  if [[ -z "${KUBERNETES_MASTER_NAME:-}" ]]; then
    KUBERNETES_MASTER_NAME="${MASTER_NAME}"
  fi

  construct-linux-kubelet-flags "master"
  build-linux-kube-env true "${KUBE_TEMP}/master-kube-env.yaml"
  build-kubelet-config true "linux" "${KUBE_TEMP}/master-kubelet-config.yaml"
  build-kube-master-certs "${KUBE_TEMP}/kube-master-certs.yaml"
}

function write-linux-node-env {
  if [[ -z "${KUBERNETES_MASTER_NAME:-}" ]]; then
    KUBERNETES_MASTER_NAME="${MASTER_NAME}"
  fi

  construct-linux-kubelet-flags "heapster"
  build-linux-kube-env false "${KUBE_TEMP}/heapster-kube-env.yaml"
  construct-linux-kubelet-flags "node"
  build-linux-kube-env false "${KUBE_TEMP}/node-kube-env.yaml"
  build-kubelet-config false "linux" "${KUBE_TEMP}/node-kubelet-config.yaml"
}

function write-windows-node-env {
  construct-windows-kubelet-flags
  construct-windows-kubeproxy-flags
  build-windows-kube-env "${KUBE_TEMP}/windows-node-kube-env.yaml"
  build-kubelet-config false "windows" "${KUBE_TEMP}/windows-node-kubelet-config.yaml"
}

function build-linux-node-labels {
  local node_type=$1
  local node_labels=""
  if [[ "${KUBE_PROXY_DAEMONSET:-}" == "true" && "${node_type}" != "master" ]]; then
    # Add kube-proxy daemonset label to node to avoid situation during cluster
    # upgrade/downgrade when there are two instances of kube-proxy running on a node.
    node_labels="node.kubernetes.io/kube-proxy-ds-ready=true"
  fi
  if [[ -n "${NODE_LABELS:-}" ]]; then
    node_labels="${node_labels:+${node_labels},}${NODE_LABELS}"
  fi
  if [[ -n "${NON_MASTER_NODE_LABELS:-}" && "${node_type}" != "master" ]]; then
    node_labels="${node_labels:+${node_labels},}${NON_MASTER_NODE_LABELS}"
  fi
  if [[ -n "${MASTER_NODE_LABELS:-}" && "${node_type}" == "master" ]]; then
    node_labels="${node_labels:+${node_labels},}${MASTER_NODE_LABELS}"
  fi
  echo "$node_labels"
}

function build-windows-node-labels {
  local node_labels=""
  if [[ -n "${WINDOWS_NODE_LABELS:-}" ]]; then
    node_labels="${node_labels:+${node_labels},}${WINDOWS_NODE_LABELS}"
  fi
  if [[ -n "${WINDOWS_NON_MASTER_NODE_LABELS:-}" ]]; then
    node_labels="${node_labels:+${node_labels},}${WINDOWS_NON_MASTER_NODE_LABELS}"
  fi
  echo "$node_labels"
}

# yaml-map-string-stringarray converts the encoded structure to yaml format, and echoes the result
# under the provided name. If the encoded structure is empty, echoes nothing.
# 1: name to be output in yaml
# 2: encoded map-string-string (which may contain duplicate keys - resulting in map-string-stringarray)
# 3: key-value separator (defaults to ':')
# 4: item separator (defaults to ',')
function yaml-map-string-stringarray {
  declare -r name="${1}"
  declare -r encoded="${2}"
  declare -r kv_sep="${3:-:}"
  declare -r item_sep="${4:-,}"

  declare -a pairs # indexed array
  declare -A map # associative array
  IFS="${item_sep}" read -ra pairs <<<"${encoded}" # split on item_sep
  for pair in "${pairs[@]}"; do
    declare key
    declare value
    IFS="${kv_sep}" read -r key value <<<"${pair}" # split on kv_sep
    map[$key]="${map[$key]+${map[$key]}${item_sep}}${value}" # append values from duplicate keys
  done
  # only output if there is a non-empty map
  if [[ ${#map[@]} -gt 0 ]]; then
    echo "${name}:"
    for k in "${!map[@]}"; do
      echo "  ${k}:"
      declare -a values
      IFS="${item_sep}" read -ra values <<<"${map[$k]}"
      for val in "${values[@]}"; do
        # declare across two lines so errexit can catch failures
        declare v
        v=$(yaml-quote "${val}")
        echo "    - ${v}"
      done
    done
  fi
}

# yaml-map-string-string converts the encoded structure to yaml format, and echoes the result
# under the provided name. If the encoded structure is empty, echoes nothing.
# 1: name to be output in yaml
# 2: encoded map-string-string (no duplicate keys)
# 3: bool, whether to yaml-quote the value string in the output (defaults to true)
# 4: key-value separator (defaults to ':')
# 5: item separator (defaults to ',')
function yaml-map-string-string {
  declare -r name="${1}"
  declare -r encoded="${2}"
  declare -r quote_val_string="${3:-true}"
  declare -r kv_sep="${4:-:}"
  declare -r item_sep="${5:-,}"

  declare -a pairs # indexed array
  declare -A map # associative array
  IFS="${item_sep}" read -ra pairs <<<"${encoded}" # split on item_sep # TODO(mtaufen): try quoting this too
  for pair in "${pairs[@]}"; do
    declare key
    declare value
    IFS="${kv_sep}" read -r key value <<<"${pair}" # split on kv_sep
    map[$key]="${value}" # add to associative array
  done
  # only output if there is a non-empty map
  if [[ ${#map[@]} -gt 0 ]]; then
    echo "${name}:"
    for k in "${!map[@]}"; do
      if [[ "${quote_val_string}" == "true" ]]; then
        # declare across two lines so errexit can catch failures
        declare v
        v=$(yaml-quote "${map[$k]}")
        echo "  ${k}: ${v}"
      else
        echo "  ${k}: ${map[$k]}"
      fi
    done
  fi
}

# Returns kubelet flags used on both Linux and Windows nodes.
function construct-common-kubelet-flags {
  local flags="${KUBELET_TEST_LOG_LEVEL:-"--v=2"} ${KUBELET_TEST_ARGS:-}"
  flags+=" --cloud-provider=${CLOUD_PROVIDER_FLAG:-external}"
  # TODO(mtaufen): ROTATE_CERTIFICATES seems unused; delete it?
  if [[ -n "${ROTATE_CERTIFICATES:-}" ]]; then
    flags+=" --rotate-certificates=true"
  fi
  if [[ -n "${MAX_PODS_PER_NODE:-}" ]]; then
    flags+=" --max-pods=${MAX_PODS_PER_NODE}"
  fi
  echo "$flags"
}

# Sets KUBELET_ARGS with the kubelet flags for Linux nodes.
# $1: if 'true', we're rendering flags for a master, else a node
function construct-linux-kubelet-flags {
  local node_type="$1"
  local flags
  flags="$(construct-common-kubelet-flags)"
  # Keep in sync with CONTAINERIZED_MOUNTER_HOME in configure-helper.sh
  flags+=" --experimental-mounter-path=/home/kubernetes/containerized_mounter/mounter"
  # Keep in sync with the mkdir command in configure-helper.sh (until the TODO is resolved)
  flags+=" --cert-dir=/var/lib/kubelet/pki/"

  # If ENABLE_AUTH_PROVIDER_GCP is set to true, kubelet is enabled to use out-of-tree auth
  # credential provider instead of in-tree auth credential provider.
  # https://kubernetes.io/docs/tasks/kubelet-credential-provider/kubelet-credential-provider
  if [[ "${ENABLE_AUTH_PROVIDER_GCP:-true}" == "true" ]]; then
    # Keep the values of --image-credential-provider-config and --image-credential-provider-bin-dir
    # in sync with value of auth_config_file and auth_provider_dir set in install-auth-provider-gcp function
    # in gci/configure.sh.
    flags+="  --image-credential-provider-config=${AUTH_PROVIDER_GCP_LINUX_CONF_FILE}"
    flags+="  --image-credential-provider-bin-dir=${AUTH_PROVIDER_GCP_LINUX_BIN_DIR}"
  fi

  if [[ "${node_type}" == "master" ]]; then
    flags+=" ${MASTER_KUBELET_TEST_ARGS:-}"
    if [[ "${REGISTER_MASTER_KUBELET:-false}" == "true" ]]; then
      #TODO(mikedanese): allow static pods to start before creating a client
      #flags+=" --bootstrap-kubeconfig=/var/lib/kubelet/bootstrap-kubeconfig"
      #flags+=" --kubeconfig=/var/lib/kubelet/kubeconfig"
      flags+=" --register-with-taints=node-role.kubernetes.io/control-plane=:NoSchedule"
      flags+=" --kubeconfig=/var/lib/kubelet/bootstrap-kubeconfig"
    fi
    if [[ "${MASTER_OS_DISTRIBUTION}" == "ubuntu" ]]; then
      # Configure the file path for host dns configuration
      # as ubuntu uses systemd-resolved
      flags+=" --resolv-conf=/run/systemd/resolve/resolv.conf"
    fi
  else # For nodes
    flags+=" ${NODE_KUBELET_TEST_ARGS:-}"
    flags+=" --bootstrap-kubeconfig=/var/lib/kubelet/bootstrap-kubeconfig"
    flags+=" --kubeconfig=/var/lib/kubelet/kubeconfig"
    if [[ "${node_type}" == "heapster" ]]; then
        flags+=" ${HEAPSTER_KUBELET_TEST_ARGS:-}"
    fi
    if [[ "${NODE_OS_DISTRIBUTION}" == "ubuntu" ]]; then
      # Configure the file path for host dns configuration
      # as ubuntu uses systemd-resolved
      flags+=" --resolv-conf=/run/systemd/resolve/resolv.conf"
    fi
  fi
  flags+=" --volume-plugin-dir=${VOLUME_PLUGIN_DIR}"
  local node_labels
  node_labels="$(build-linux-node-labels "${node_type}")"
  if [[ -n "${node_labels:-}" ]]; then
    flags+=" --node-labels=${node_labels}"
  fi
  if [[ -n "${NODE_TAINTS:-}" ]]; then
    flags+=" --register-with-taints=${NODE_TAINTS}"
  fi

  CONTAINER_RUNTIME_ENDPOINT=${KUBE_CONTAINER_RUNTIME_ENDPOINT:-unix:///run/containerd/containerd.sock}
  flags+=" --container-runtime-endpoint=${CONTAINER_RUNTIME_ENDPOINT}"

  if [[ "${CONTAINER_RUNTIME_ENDPOINT}" =~ /containerd.sock$ ]]; then
    flags+=" --runtime-cgroups=/system.slice/containerd.service"
  fi

  KUBELET_ARGS="${flags}"
}

# Sets KUBELET_ARGS with the kubelet flags for Windows nodes.
# Note that to configure flags with explicit empty string values, we can't escape
# double-quotes, because they still break sc.exe after expansion in the
# binPath parameter, and single-quotes get parsed as characters instead of
# string delimiters.
function construct-windows-kubelet-flags {
  local flags
  flags="$(construct-common-kubelet-flags)"

  # Note: NODE_KUBELET_TEST_ARGS is empty in typical kube-up runs.
  flags+=" ${NODE_KUBELET_TEST_ARGS:-}"

  local node_labels
  node_labels="$(build-windows-node-labels)"
  if [[ -n "${node_labels:-}" ]]; then
    flags+=" --node-labels=${node_labels}"
  fi

  # Concatenate common and windows-only node taints and apply them.
  local node_taints="${NODE_TAINTS:-}"
  if [[ -n "${node_taints}" && -n "${WINDOWS_NODE_TAINTS:-}" ]]; then
    node_taints+=":${WINDOWS_NODE_TAINTS}"
  else
    node_taints="${WINDOWS_NODE_TAINTS:-}"
  fi
  if [[ -n "${node_taints}" ]]; then
    flags+=" --register-with-taints=${node_taints}"
  fi

  # Many of these flags were adapted from
  # https://github.com/Microsoft/SDN/blob/master/Kubernetes/windows/start-kubelet.ps1.
  flags+=" --config=${WINDOWS_KUBELET_CONFIG_FILE}"
  flags+=" --kubeconfig=${WINDOWS_KUBECONFIG_FILE}"

  # The directory where the TLS certs are located.
  flags+=" --cert-dir=${WINDOWS_PKI_DIR}"
  flags+=" --pod-manifest-path=${WINDOWS_MANIFESTS_DIR}"

  # Configure kubelet to run as a windows service.
  flags+=" --windows-service=true"

  # Configure the file path for host dns configuration
  flags+=" --resolv-conf=${WINDOWS_CNI_DIR}\hostdns.conf"

  # Both --cgroups-per-qos and --enforce-node-allocatable should be disabled on
  # windows; the latter requires the former to be enabled to work.
  flags+=" --cgroups-per-qos=false --enforce-node-allocatable="

  # Turn off kernel memory cgroup notification.
  flags+=" --kernel-memcg-notification=false"

  WINDOWS_CONTAINER_RUNTIME_ENDPOINT=${KUBE_WINDOWS_CONTAINER_RUNTIME_ENDPOINT:-npipe:////./pipe/containerd-containerd}
  flags+=" --container-runtime-endpoint=${WINDOWS_CONTAINER_RUNTIME_ENDPOINT}"

  # If ENABLE_AUTH_PROVIDER_GCP is set to true, kubelet is enabled to use out-of-tree auth
  # credential provider. https://kubernetes.io/docs/tasks/kubelet-credential-provider/kubelet-credential-provider
  if [[ "${ENABLE_AUTH_PROVIDER_GCP:-true}" == "true" ]]; then
    flags+="  --image-credential-provider-config=${AUTH_PROVIDER_GCP_WINDOWS_CONF_FILE}"
    flags+="  --image-credential-provider-bin-dir=${AUTH_PROVIDER_GCP_WINDOWS_BIN_DIR}"
  fi

  KUBELET_ARGS="${flags}"
}

function construct-windows-kubeproxy-flags {
  local flags=""

  # Use the same log level as the Kubelet during tests.
  flags+=" ${KUBELET_TEST_LOG_LEVEL:-"--v=2"}"

  # Windows uses kernelspace proxymode
  flags+=" --proxy-mode=kernelspace"

  # Configure kube-proxy to run as a windows service.
  flags+=" --windows-service=true"

  # Enabling Windows DSR mode unlocks newer network features and reduces
  # port usage for services.
  # https://techcommunity.microsoft.com/t5/networking-blog/direct-server-return-dsr-in-a-nutshell/ba-p/693710
  if [[ "${WINDOWS_ENABLE_DSR:-}" == "true" ]]; then
    flags+=" --feature-gates=WinDSR=true --enable-dsr=true "
  fi

  # Configure flags with explicit empty string values. We can't escape
  # double-quotes, because they still break sc.exe after expansion in the
  # binPath parameter, and single-quotes get parsed as characters instead
  # of string delimiters.

  KUBEPROXY_ARGS="${flags}"
}

# $1: if 'true', we're rendering config for a master, else a node
function build-kubelet-config {
  local master="$1"
  local os="$2"
  local file="$3"

  rm -f "${file}"
  {
    print-common-kubelet-config
    if [[ "${master}" == "true" ]]; then
      print-master-kubelet-config
    else
      print-common-node-kubelet-config
      if [[ "${os}" == "linux" ]]; then
        print-linux-node-kubelet-config
      elif [[ "${os}" == "windows" ]]; then
        print-windows-node-kubelet-config
      else
        echo "Unknown OS ${os}" >&2
        exit 1
      fi
    fi
  } > "${file}"
}

# cat the Kubelet config yaml in common between masters, linux nodes, and
# windows nodes
function print-common-kubelet-config {
  declare quoted_dns_server_ip
  declare quoted_dns_domain
  quoted_dns_server_ip=$(yaml-quote "${DNS_SERVER_IP}")
  quoted_dns_domain=$(yaml-quote "${DNS_DOMAIN}")
  cat <<EOF
kind: KubeletConfiguration
apiVersion: kubelet.config.k8s.io/v1beta1
cgroupRoot: /
clusterDNS:
  - ${quoted_dns_server_ip}
clusterDomain: ${quoted_dns_domain}
readOnlyPort: 10255
EOF

  # Note: ENABLE_MANIFEST_URL is used by GKE.
  # TODO(mtaufen): remove this since it's not used in kubernetes/kubernetes nor
  # kubernetes/test-infra.
  if [[ "${ENABLE_MANIFEST_URL:-}" == "true" ]]; then
    declare quoted_manifest_url
    quoted_manifest_url=$(yaml-quote "${MANIFEST_URL}")
    cat <<EOF
staticPodURL: ${quoted_manifest_url}
EOF
    yaml-map-string-stringarray 'staticPodURLHeader' "${MANIFEST_URL_HEADER}"
  fi

  if [[ -n "${EVICTION_HARD:-}" ]]; then
    yaml-map-string-string 'evictionHard' "${EVICTION_HARD}" true '<'
  fi

  if [[ -n "${FEATURE_GATES:-}" ]]; then
    yaml-map-string-string 'featureGates' "${FEATURE_GATES}" false '='
  fi
}

# cat the Kubelet config yaml for masters
function print-master-kubelet-config {
  cat <<EOF
enableDebuggingHandlers: ${MASTER_KUBELET_ENABLE_DEBUGGING_HANDLERS:-false}
hairpinMode: none
staticPodPath: /etc/kubernetes/manifests
authentication:
  webhook:
    enabled: false
  anonymous:
    enabled: true
authorization:
  mode: AlwaysAllow
EOF
  if [[ "${REGISTER_MASTER_KUBELET:-false}" == "false" ]]; then
     # Note: Standalone mode is used by GKE
    declare quoted_master_ip_range
    quoted_master_ip_range=$(yaml-quote "${MASTER_IP_RANGE}")
     cat <<EOF
podCidr: ${quoted_master_ip_range}
EOF
  fi
}

# cat the Kubelet config yaml in common between linux nodes and windows nodes
function print-common-node-kubelet-config {
  cat <<EOF
enableDebuggingHandlers: ${KUBELET_ENABLE_DEBUGGING_HANDLERS:-true}
EOF
  if [[ "${HAIRPIN_MODE:-}" == "promiscuous-bridge" ]] || \
     [[ "${HAIRPIN_MODE:-}" == "hairpin-veth" ]] || \
     [[ "${HAIRPIN_MODE:-}" == "none" ]]; then
      declare quoted_hairpin_mode
      quoted_hairpin_mode=$(yaml-quote "${HAIRPIN_MODE}")
      cat <<EOF
hairpinMode: ${quoted_hairpin_mode}
EOF
  fi
}

# cat the Kubelet config yaml for linux nodes
function print-linux-node-kubelet-config {
  # Keep authentication.x509.clientCAFile in sync with CA_CERT_BUNDLE_PATH in configure-helper.sh
  cat <<EOF
staticPodPath: /etc/kubernetes/manifests
authentication:
  x509:
    clientCAFile: /etc/srv/kubernetes/pki/ca-certificates.crt
EOF

  if [[ -n "${SHUTDOWN_GRACE_PERIOD:-}" ]]; then
  cat <<EOF
shutdownGracePeriod: ${SHUTDOWN_GRACE_PERIOD}
EOF
  fi

  if [[ -n "${SHUTDOWN_GRACE_PERIOD_CRITICAL_PODS:-}" ]]; then
  cat <<EOF
shutdownGracePeriodCriticalPods: ${SHUTDOWN_GRACE_PERIOD_CRITICAL_PODS}
EOF
  fi
}

# cat the Kubelet config yaml for windows nodes
function print-windows-node-kubelet-config {
  # Notes:
  # - We don't run any static pods on Windows nodes yet.

  # TODO(mtaufen): Does it make any sense to set eviction thresholds for inodes
  # on Windows?

  # TODO(pjh, mtaufen): It may make sense to use a different hairpin mode on
  # Windows. We're currently using hairpin-veth, but
  # https://github.com/Microsoft/SDN/blob/master/Kubernetes/windows/start-kubelet.ps1#L121
  # uses promiscuous-bridge.

  # TODO(pjh, mtaufen): Does cgroupRoot make sense for Windows?

  # Keep authentication.x509.clientCAFile in sync with CA_CERT_BUNDLE_PATH in
  # k8s-node-setup.psm1.
  cat <<EOF
authentication:
  x509:
    clientCAFile: '${WINDOWS_CA_FILE}'
EOF
}

function build-kube-master-certs {
  local file=$1
  rm -f "$file"
  cat >"$file" <<EOF
KUBEAPISERVER_CERT: $(yaml-quote "${KUBEAPISERVER_CERT_BASE64:-}")
KUBEAPISERVER_KEY: $(yaml-quote "${KUBEAPISERVER_KEY_BASE64:-}")
CA_KEY: $(yaml-quote "${CA_KEY_BASE64:-}")
AGGREGATOR_CA_KEY: $(yaml-quote "${AGGREGATOR_CA_KEY_BASE64:-}")
REQUESTHEADER_CA_CERT: $(yaml-quote "${REQUESTHEADER_CA_CERT_BASE64:-}")
PROXY_CLIENT_CERT: $(yaml-quote "${PROXY_CLIENT_CERT_BASE64:-}")
PROXY_CLIENT_KEY: $(yaml-quote "${PROXY_CLIENT_KEY_BASE64:-}")
ETCD_APISERVER_CA_KEY: $(yaml-quote "${ETCD_APISERVER_CA_KEY_BASE64:-}")
ETCD_APISERVER_CA_CERT: $(yaml-quote "${ETCD_APISERVER_CA_CERT_BASE64:-}")
ETCD_APISERVER_SERVER_KEY: $(yaml-quote "${ETCD_APISERVER_SERVER_KEY_BASE64:-}")
ETCD_APISERVER_SERVER_CERT: $(yaml-quote "${ETCD_APISERVER_SERVER_CERT_BASE64:-}")
ETCD_APISERVER_CLIENT_KEY: $(yaml-quote "${ETCD_APISERVER_CLIENT_KEY_BASE64:-}")
ETCD_APISERVER_CLIENT_CERT: $(yaml-quote "${ETCD_APISERVER_CLIENT_CERT_BASE64:-}")
CLOUD_PVL_ADMISSION_CA_KEY: $(yaml-quote "${CLOUD_PVL_ADMISSION_CA_KEY_BASE64:-}")
CLOUD_PVL_ADMISSION_CA_CERT: $(yaml-quote "${CLOUD_PVL_ADMISSION_CA_CERT_BASE64:-}")
CLOUD_PVL_ADMISSION_CERT: $(yaml-quote "${CLOUD_PVL_ADMISSION_CERT_BASE64:-}")
CLOUD_PVL_ADMISSION_KEY: $(yaml-quote "${CLOUD_PVL_ADMISSION_KEY_BASE64:-}")
KONNECTIVITY_SERVER_CA_KEY: $(yaml-quote "${KONNECTIVITY_SERVER_CA_KEY_BASE64:-}")
KONNECTIVITY_SERVER_CA_CERT: $(yaml-quote "${KONNECTIVITY_SERVER_CA_CERT_BASE64:-}")
KONNECTIVITY_SERVER_CERT: $(yaml-quote "${KONNECTIVITY_SERVER_CERT_BASE64:-}")
KONNECTIVITY_SERVER_KEY: $(yaml-quote "${KONNECTIVITY_SERVER_KEY_BASE64:-}")
KONNECTIVITY_SERVER_CLIENT_CERT: $(yaml-quote "${KONNECTIVITY_SERVER_CLIENT_CERT_BASE64:-}")
KONNECTIVITY_SERVER_CLIENT_KEY: $(yaml-quote "${KONNECTIVITY_SERVER_CLIENT_KEY_BASE64:-}")
KONNECTIVITY_AGENT_CA_KEY: $(yaml-quote "${KONNECTIVITY_AGENT_CA_KEY_BASE64:-}")
KONNECTIVITY_AGENT_CA_CERT: $(yaml-quote "${KONNECTIVITY_AGENT_CA_CERT_BASE64:-}")
KONNECTIVITY_AGENT_CERT: $(yaml-quote "${KONNECTIVITY_AGENT_CERT_BASE64:-}")
KONNECTIVITY_AGENT_KEY: $(yaml-quote "${KONNECTIVITY_AGENT_KEY_BASE64:-}")
KONNECTIVITY_AGENT_CLIENT_CERT: $(yaml-quote "${KONNECTIVITY_AGENT_CLIENT_CERT_BASE64:-}")
KONNECTIVITY_AGENT_CLIENT_KEY: $(yaml-quote "${KONNECTIVITY_AGENT_CLIENT_KEY_BASE64:-}")
EOF
}

# $1: if 'true', we're building a master yaml, else a node
function build-linux-kube-env {
  local master="$1"
  local file="$2"

  local server_binary_tar_url=$SERVER_BINARY_TAR_URL
  local kube_manifests_tar_url="${KUBE_MANIFESTS_TAR_URL:-}"
  if [[ "${master}" == "true" && "${MASTER_OS_DISTRIBUTION}" == "ubuntu" ]] || \
     [[ "${master}" == "false" && ("${NODE_OS_DISTRIBUTION}" == "ubuntu" || "${NODE_OS_DISTRIBUTION}" == "custom") ]]; then
    # TODO: Support fallback .tar.gz settings on Container Linux
    server_binary_tar_url=$(split_csv "${SERVER_BINARY_TAR_URL}")
    kube_manifests_tar_url=$(split_csv "${KUBE_MANIFESTS_TAR_URL}")
  fi

  rm -f "$file"
  cat >"$file" <<EOF
CLUSTER_NAME: $(yaml-quote "${CLUSTER_NAME}")
ENV_TIMESTAMP: $(yaml-quote "$(date -u +%Y-%m-%dT%T%z)")
INSTANCE_PREFIX: $(yaml-quote "${INSTANCE_PREFIX}")
NODE_INSTANCE_PREFIX: $(yaml-quote "${NODE_INSTANCE_PREFIX}")
NODE_TAGS: $(yaml-quote "${NODE_TAGS:-}")
NODE_NETWORK: $(yaml-quote "${NETWORK:-}")
NODE_SUBNETWORK: $(yaml-quote "${SUBNETWORK:-}")
CLUSTER_IP_RANGE: $(yaml-quote "${CLUSTER_IP_RANGE:-10.244.0.0/16}")
SERVER_BINARY_TAR_URL: $(yaml-quote "${server_binary_tar_url}")
SERVER_BINARY_TAR_HASH: $(yaml-quote "${SERVER_BINARY_TAR_HASH}")
PROJECT_ID: $(yaml-quote "${PROJECT}")
NETWORK_PROJECT_ID: $(yaml-quote "${NETWORK_PROJECT}")
SERVICE_CLUSTER_IP_RANGE: $(yaml-quote "${SERVICE_CLUSTER_IP_RANGE}")
KUBERNETES_MASTER_NAME: $(yaml-quote "${KUBERNETES_MASTER_NAME}")
ALLOCATE_NODE_CIDRS: $(yaml-quote "${ALLOCATE_NODE_CIDRS:-false}")
ENABLE_METRICS_SERVER: $(yaml-quote "${ENABLE_METRICS_SERVER:-false}")
ENABLE_METADATA_AGENT: $(yaml-quote "${ENABLE_METADATA_AGENT:-none}")
METADATA_AGENT_CPU_REQUEST: $(yaml-quote "${METADATA_AGENT_CPU_REQUEST:-}")
METADATA_AGENT_MEMORY_REQUEST: $(yaml-quote "${METADATA_AGENT_MEMORY_REQUEST:-}")
METADATA_AGENT_CLUSTER_LEVEL_CPU_REQUEST: $(yaml-quote "${METADATA_AGENT_CLUSTER_LEVEL_CPU_REQUEST:-}")
METADATA_AGENT_CLUSTER_LEVEL_MEMORY_REQUEST: $(yaml-quote "${METADATA_AGENT_CLUSTER_LEVEL_MEMORY_REQUEST:-}")
DOCKER_REGISTRY_MIRROR_URL: $(yaml-quote "${DOCKER_REGISTRY_MIRROR_URL:-}")
ENABLE_L7_LOADBALANCING: $(yaml-quote "${ENABLE_L7_LOADBALANCING:-none}")
ENABLE_CLUSTER_LOGGING: $(yaml-quote "${ENABLE_CLUSTER_LOGGING:-false}")
ENABLE_AUTH_PROVIDER_GCP: $(yaml-quote "${ENABLE_AUTH_PROVIDER_GCP:-true}")
ENABLE_NODE_PROBLEM_DETECTOR: $(yaml-quote "${ENABLE_NODE_PROBLEM_DETECTOR:-none}")
NODE_PROBLEM_DETECTOR_VERSION: $(yaml-quote "${NODE_PROBLEM_DETECTOR_VERSION:-}")
NODE_PROBLEM_DETECTOR_TAR_HASH: $(yaml-quote "${NODE_PROBLEM_DETECTOR_TAR_HASH:-}")
NODE_PROBLEM_DETECTOR_RELEASE_PATH: $(yaml-quote "${NODE_PROBLEM_DETECTOR_RELEASE_PATH:-}")
NODE_PROBLEM_DETECTOR_CUSTOM_FLAGS: $(yaml-quote "${NODE_PROBLEM_DETECTOR_CUSTOM_FLAGS:-}")
CNI_STORAGE_URL_BASE: $(yaml-quote "${CNI_STORAGE_URL_BASE:-}")
CNI_TAR_PREFIX: $(yaml-quote "${CNI_TAR_PREFIX:-}")
CNI_VERSION: $(yaml-quote "${CNI_VERSION:-}")
CNI_HASH: $(yaml-quote "${CNI_HASH:-}")
ENABLE_NODE_LOGGING: $(yaml-quote "${ENABLE_NODE_LOGGING:-false}")
LOGGING_DESTINATION: $(yaml-quote "${LOGGING_DESTINATION:-}")
ELASTICSEARCH_LOGGING_REPLICAS: $(yaml-quote "${ELASTICSEARCH_LOGGING_REPLICAS:-}")
ENABLE_CLUSTER_DNS: $(yaml-quote "${ENABLE_CLUSTER_DNS:-false}")
CLUSTER_DNS_CORE_DNS: $(yaml-quote "${CLUSTER_DNS_CORE_DNS:-true}")
ENABLE_NODELOCAL_DNS: $(yaml-quote "${ENABLE_NODELOCAL_DNS:-false}")
DNS_SERVER_IP: $(yaml-quote "${DNS_SERVER_IP:-}")
LOCAL_DNS_IP: $(yaml-quote "${LOCAL_DNS_IP:-}")
DNS_DOMAIN: $(yaml-quote "${DNS_DOMAIN:-}")
DNS_MEMORY_LIMIT: $(yaml-quote "${DNS_MEMORY_LIMIT:-}")
ENABLE_DNS_HORIZONTAL_AUTOSCALER: $(yaml-quote "${ENABLE_DNS_HORIZONTAL_AUTOSCALER:-false}")
KUBE_PROXY_DAEMONSET: $(yaml-quote "${KUBE_PROXY_DAEMONSET:-false}")
KUBE_PROXY_TOKEN: $(yaml-quote "${KUBE_PROXY_TOKEN:-}")
KUBE_PROXY_MODE: $(yaml-quote "${KUBE_PROXY_MODE:-iptables}")
DETECT_LOCAL_MODE: $(yaml-quote "${DETECT_LOCAL_MODE:-}")
NODE_PROBLEM_DETECTOR_TOKEN: $(yaml-quote "${NODE_PROBLEM_DETECTOR_TOKEN:-}")
ADMISSION_CONTROL: $(yaml-quote "${ADMISSION_CONTROL:-}")
MASTER_IP_RANGE: $(yaml-quote "${MASTER_IP_RANGE}")
RUNTIME_CONFIG: $(yaml-quote "${RUNTIME_CONFIG}")
CA_CERT: $(yaml-quote "${CA_CERT_BASE64:-}")
KUBELET_CERT: $(yaml-quote "${KUBELET_CERT_BASE64:-}")
KUBELET_KEY: $(yaml-quote "${KUBELET_KEY_BASE64:-}")
NETWORK_PROVIDER: $(yaml-quote "${NETWORK_PROVIDER:-}")
NETWORK_POLICY_PROVIDER: $(yaml-quote "${NETWORK_POLICY_PROVIDER:-}")
HAIRPIN_MODE: $(yaml-quote "${HAIRPIN_MODE:-}")
E2E_STORAGE_TEST_ENVIRONMENT: $(yaml-quote "${E2E_STORAGE_TEST_ENVIRONMENT:-}")
KUBE_DOCKER_REGISTRY: $(yaml-quote "${KUBE_DOCKER_REGISTRY:-}")
KUBE_ADDON_REGISTRY: $(yaml-quote "${KUBE_ADDON_REGISTRY:-}")
MULTIZONE: $(yaml-quote "${MULTIZONE:-}")
MULTIMASTER: $(yaml-quote "${MULTIMASTER:-}")
NON_MASQUERADE_CIDR: $(yaml-quote "${NON_MASQUERADE_CIDR:-}")
ENABLE_DEFAULT_STORAGE_CLASS: $(yaml-quote "${ENABLE_DEFAULT_STORAGE_CLASS:-}")
ENABLE_VOLUME_SNAPSHOTS: $(yaml-quote "${ENABLE_VOLUME_SNAPSHOTS:-}")
ENABLE_APISERVER_ADVANCED_AUDIT: $(yaml-quote "${ENABLE_APISERVER_ADVANCED_AUDIT:-}")
ENABLE_APISERVER_DYNAMIC_AUDIT: $(yaml-quote "${ENABLE_APISERVER_DYNAMIC_AUDIT:-}")
ENABLE_CACHE_MUTATION_DETECTOR: $(yaml-quote "${ENABLE_CACHE_MUTATION_DETECTOR:-false}")
ENABLE_KUBE_WATCHLIST_INCONSISTENCY_DETECTOR: $(yaml-quote "${ENABLE_KUBE_WATCHLIST_INCONSISTENCY_DETECTOR:-false}")
ENABLE_KUBE_LIST_FROM_CACHE_INCONSISTENCY_DETECTOR: $(yaml-quote "${ENABLE_KUBE_LIST_FROM_CACHE_INCONSISTENCY_DETECTOR:-false}")
ENABLE_PATCH_CONVERSION_DETECTOR: $(yaml-quote "${ENABLE_PATCH_CONVERSION_DETECTOR:-false}")
ADVANCED_AUDIT_POLICY: $(yaml-quote "${ADVANCED_AUDIT_POLICY:-}")
ADVANCED_AUDIT_BACKEND: $(yaml-quote "${ADVANCED_AUDIT_BACKEND:-log}")
ADVANCED_AUDIT_TRUNCATING_BACKEND: $(yaml-quote "${ADVANCED_AUDIT_TRUNCATING_BACKEND:-true}")
ADVANCED_AUDIT_LOG_MODE: $(yaml-quote "${ADVANCED_AUDIT_LOG_MODE:-}")
ADVANCED_AUDIT_LOG_BUFFER_SIZE: $(yaml-quote "${ADVANCED_AUDIT_LOG_BUFFER_SIZE:-}")
ADVANCED_AUDIT_LOG_MAX_BATCH_SIZE: $(yaml-quote "${ADVANCED_AUDIT_LOG_MAX_BATCH_SIZE:-}")
ADVANCED_AUDIT_LOG_MAX_BATCH_WAIT: $(yaml-quote "${ADVANCED_AUDIT_LOG_MAX_BATCH_WAIT:-}")
ADVANCED_AUDIT_LOG_THROTTLE_QPS: $(yaml-quote "${ADVANCED_AUDIT_LOG_THROTTLE_QPS:-}")
ADVANCED_AUDIT_LOG_THROTTLE_BURST: $(yaml-quote "${ADVANCED_AUDIT_LOG_THROTTLE_BURST:-}")
ADVANCED_AUDIT_LOG_INITIAL_BACKOFF: $(yaml-quote "${ADVANCED_AUDIT_LOG_INITIAL_BACKOFF:-}")
ADVANCED_AUDIT_WEBHOOK_MODE: $(yaml-quote "${ADVANCED_AUDIT_WEBHOOK_MODE:-}")
ADVANCED_AUDIT_WEBHOOK_BUFFER_SIZE: $(yaml-quote "${ADVANCED_AUDIT_WEBHOOK_BUFFER_SIZE:-}")
ADVANCED_AUDIT_WEBHOOK_MAX_BATCH_SIZE: $(yaml-quote "${ADVANCED_AUDIT_WEBHOOK_MAX_BATCH_SIZE:-}")
ADVANCED_AUDIT_WEBHOOK_MAX_BATCH_WAIT: $(yaml-quote "${ADVANCED_AUDIT_WEBHOOK_MAX_BATCH_WAIT:-}")
ADVANCED_AUDIT_WEBHOOK_THROTTLE_QPS: $(yaml-quote "${ADVANCED_AUDIT_WEBHOOK_THROTTLE_QPS:-}")
ADVANCED_AUDIT_WEBHOOK_THROTTLE_BURST: $(yaml-quote "${ADVANCED_AUDIT_WEBHOOK_THROTTLE_BURST:-}")
ADVANCED_AUDIT_WEBHOOK_INITIAL_BACKOFF: $(yaml-quote "${ADVANCED_AUDIT_WEBHOOK_INITIAL_BACKOFF:-}")
GCE_API_ENDPOINT: $(yaml-quote "${GCE_API_ENDPOINT:-}")
GCE_GLBC_IMAGE: $(yaml-quote "${GCE_GLBC_IMAGE:-}")
CUSTOM_INGRESS_YAML: |
${CUSTOM_INGRESS_YAML//\'/\'\'}
ENABLE_NODE_JOURNAL: $(yaml-quote "${ENABLE_NODE_JOURNAL:-false}")
PROMETHEUS_TO_SD_ENDPOINT: $(yaml-quote "${PROMETHEUS_TO_SD_ENDPOINT:-}")
PROMETHEUS_TO_SD_PREFIX: $(yaml-quote "${PROMETHEUS_TO_SD_PREFIX:-}")
ENABLE_PROMETHEUS_TO_SD: $(yaml-quote "${ENABLE_PROMETHEUS_TO_SD:-false}")
DISABLE_PROMETHEUS_TO_SD_IN_DS: $(yaml-quote "${DISABLE_PROMETHEUS_TO_SD_IN_DS:-false}")
CONTAINER_RUNTIME_ENDPOINT: $(yaml-quote "${CONTAINER_RUNTIME_ENDPOINT:-}")
CONTAINER_RUNTIME_NAME: $(yaml-quote "${CONTAINER_RUNTIME_NAME:-}")
CONTAINER_RUNTIME_TEST_HANDLER: $(yaml-quote "${CONTAINER_RUNTIME_TEST_HANDLER:-}")
CONTAINERD_INFRA_CONTAINER: $(yaml-quote "${CONTAINER_INFRA_CONTAINER:-}")
UBUNTU_INSTALL_CONTAINERD_VERSION: $(yaml-quote "${UBUNTU_INSTALL_CONTAINERD_VERSION:-}")
UBUNTU_INSTALL_RUNC_VERSION: $(yaml-quote "${UBUNTU_INSTALL_RUNC_VERSION:-}")
COS_INSTALL_CONTAINERD_VERSION: $(yaml-quote "${COS_INSTALL_CONTAINERD_VERSION:-}")
COS_INSTALL_RUNC_VERSION: $(yaml-quote "${COS_INSTALL_RUNC_VERSION:-}")
NODE_LOCAL_SSDS_EXT: $(yaml-quote "${NODE_LOCAL_SSDS_EXT:-}")
NODE_LOCAL_SSDS_EPHEMERAL: $(yaml-quote "${NODE_LOCAL_SSDS_EPHEMERAL:-}")
LOAD_IMAGE_COMMAND: $(yaml-quote "${LOAD_IMAGE_COMMAND:-}")
ZONE: $(yaml-quote "${ZONE}")
REGION: $(yaml-quote "${REGION}")
VOLUME_PLUGIN_DIR: $(yaml-quote "${VOLUME_PLUGIN_DIR}")
KUBELET_ARGS: $(yaml-quote "${KUBELET_ARGS}")
REQUIRE_METADATA_KUBELET_CONFIG_FILE: $(yaml-quote true)
ENABLE_NETD: $(yaml-quote "${ENABLE_NETD:-false}")
CUSTOM_NETD_YAML: |
${CUSTOM_NETD_YAML//\'/\'\'}
CUSTOM_CALICO_NODE_DAEMONSET_YAML: |
${CUSTOM_CALICO_NODE_DAEMONSET_YAML//\'/\'\'}
CUSTOM_TYPHA_DEPLOYMENT_YAML: |
${CUSTOM_TYPHA_DEPLOYMENT_YAML//\'/\'\'}
CONCURRENT_SERVICE_SYNCS: $(yaml-quote "${CONCURRENT_SERVICE_SYNCS:-}")
AUTH_PROVIDER_GCP_STORAGE_PATH: $(yaml-quote "${AUTH_PROVIDER_GCP_STORAGE_PATH}")
AUTH_PROVIDER_GCP_VERSION: $(yaml-quote "${AUTH_PROVIDER_GCP_VERSION}")
AUTH_PROVIDER_GCP_LINUX_BIN_DIR: $(yaml-quote "${AUTH_PROVIDER_GCP_LINUX_BIN_DIR}")
AUTH_PROVIDER_GCP_LINUX_CONF_FILE: $(yaml-quote "${AUTH_PROVIDER_GCP_LINUX_CONF_FILE}")
KUBERNETES_REGISTRY_PULL_THROUGH_HOST: $(yaml-quote "${KUBERNETES_REGISTRY_PULL_THROUGH_HOST:-}")
EOF
  if [[ -n "${KUBERNETES_REGISTRY_PULL_THROUGH_BASIC_AUTH_TOKEN_PATH:-}" ]]; then
    cat >>"$file" <<EOF
KUBERNETES_REGISTRY_PULL_THROUGH_BASIC_AUTH_TOKEN: $(yaml-quote "$(cat "${KUBERNETES_REGISTRY_PULL_THROUGH_BASIC_AUTH_TOKEN_PATH}")")
EOF
  fi
  if [[ "${master}" == "true" && "${MASTER_OS_DISTRIBUTION}" == "gci" ]] || \
     [[ "${master}" == "false" && "${NODE_OS_DISTRIBUTION}" == "gci" ]]  || \
     [[ "${master}" == "true" && "${MASTER_OS_DISTRIBUTION}" == "cos" ]] || \
     [[ "${master}" == "false" && "${NODE_OS_DISTRIBUTION}" == "cos" ]]; then
    cat >>"$file" <<EOF
REMOUNT_VOLUME_PLUGIN_DIR: $(yaml-quote "${REMOUNT_VOLUME_PLUGIN_DIR:-true}")
EOF
  fi
  if [[ "${master}" == "false" ]]; then
    cat >>"$file" <<EOF
KONNECTIVITY_AGENT_CA_CERT: $(yaml-quote "${KONNECTIVITY_AGENT_CA_CERT_BASE64:-}")
KONNECTIVITY_AGENT_CLIENT_KEY: $(yaml-quote "${KONNECTIVITY_AGENT_CLIENT_KEY_BASE64:-}")
KONNECTIVITY_AGENT_CLIENT_CERT: $(yaml-quote "${KONNECTIVITY_AGENT_CLIENT_CERT_BASE64:-}")
EOF
  fi
  if [ -n "${KUBE_APISERVER_REQUEST_TIMEOUT:-}" ]; then
    cat >>"$file" <<EOF
KUBE_APISERVER_REQUEST_TIMEOUT: $(yaml-quote "${KUBE_APISERVER_REQUEST_TIMEOUT}")
EOF
  fi
  if [ -n "${TERMINATED_POD_GC_THRESHOLD:-}" ]; then
    cat >>"$file" <<EOF
TERMINATED_POD_GC_THRESHOLD: $(yaml-quote "${TERMINATED_POD_GC_THRESHOLD}")
EOF
  fi
  if [[ "${master}" == "true" && ("${MASTER_OS_DISTRIBUTION}" == "trusty" || "${MASTER_OS_DISTRIBUTION}" == "gci" || "${MASTER_OS_DISTRIBUTION}" == "ubuntu") ]] || \
     [[ "${master}" == "false" && ("${NODE_OS_DISTRIBUTION}" == "trusty" || "${NODE_OS_DISTRIBUTION}" == "gci" || "${NODE_OS_DISTRIBUTION}" = "ubuntu" || "${NODE_OS_DISTRIBUTION}" = "custom") ]] ; then
    cat >>"$file" <<EOF
KUBE_MANIFESTS_TAR_URL: $(yaml-quote "${kube_manifests_tar_url}")
KUBE_MANIFESTS_TAR_HASH: $(yaml-quote "${KUBE_MANIFESTS_TAR_HASH}")
EOF
  fi
  if [ -n "${TEST_CLUSTER:-}" ]; then
    cat >>"$file" <<EOF
TEST_CLUSTER: $(yaml-quote "${TEST_CLUSTER}")
EOF
  fi
  if [ -n "${DOCKER_TEST_LOG_LEVEL:-}" ]; then
      cat >>"$file" <<EOF
DOCKER_TEST_LOG_LEVEL: $(yaml-quote "${DOCKER_TEST_LOG_LEVEL}")
EOF
  fi
  if [ -n "${DOCKER_LOG_DRIVER:-}" ]; then
      cat >>"$file" <<EOF
DOCKER_LOG_DRIVER: $(yaml-quote "${DOCKER_LOG_DRIVER}")
EOF
  fi
  if [ -n "${DOCKER_LOG_MAX_SIZE:-}" ]; then
      cat >>"$file" <<EOF
DOCKER_LOG_MAX_SIZE: $(yaml-quote "${DOCKER_LOG_MAX_SIZE}")
EOF
  fi
  if [ -n "${DOCKER_LOG_MAX_FILE:-}" ]; then
      cat >>"$file" <<EOF
DOCKER_LOG_MAX_FILE: $(yaml-quote "${DOCKER_LOG_MAX_FILE}")
EOF
  fi
  if [ -n "${CLOUD_PROVIDER_FLAG:-}" ]; then
    cat >>"$file" <<EOF
CLOUD_PROVIDER_FLAG: $(yaml-quote "${CLOUD_PROVIDER_FLAG}")
EOF
  fi
  if [ -n "${FEATURE_GATES:-}" ]; then
    cat >>"$file" <<EOF
FEATURE_GATES: $(yaml-quote "${FEATURE_GATES}")
EOF
  fi
  if [ -n "${RUN_CONTROLLERS:-}" ]; then
    cat >>"$file" <<EOF
RUN_CONTROLLERS: $(yaml-quote "${RUN_CONTROLLERS}")
EOF
  fi
  if [ -n "${RUN_CCM_CONTROLLERS:-}" ]; then
    cat >>"$file" <<EOF
RUN_CCM_CONTROLLERS: $(yaml-quote "${RUN_CCM_CONTROLLERS}")
EOF
  fi
  if [ -n "${PROVIDER_VARS:-}" ]; then
    local var_name
    local var_value

    for var_name in ${PROVIDER_VARS}; do
      eval "local var_value=\$(yaml-quote \${${var_name}})"
      cat >>"$file" <<EOF
${var_name}: ${var_value}
EOF
    done
  fi

  if [[ "${master}" == "true" ]]; then
    # Master-only env vars.
    cat >>"$file" <<EOF
KUBERNETES_MASTER: $(yaml-quote 'true')
KUBE_USER: $(yaml-quote "${KUBE_USER}")
KUBE_PASSWORD: $(yaml-quote "${KUBE_PASSWORD}")
KUBE_BEARER_TOKEN: $(yaml-quote "${KUBE_BEARER_TOKEN}")
MASTER_CERT: $(yaml-quote "${MASTER_CERT_BASE64:-}")
MASTER_KEY: $(yaml-quote "${MASTER_KEY_BASE64:-}")
KUBECFG_CERT: $(yaml-quote "${KUBECFG_CERT_BASE64:-}")
KUBECFG_KEY: $(yaml-quote "${KUBECFG_KEY_BASE64:-}")
KUBELET_APISERVER: $(yaml-quote "${KUBELET_APISERVER:-}")
NUM_NODES: $(yaml-quote "${NUM_NODES}")
STORAGE_BACKEND: $(yaml-quote "${STORAGE_BACKEND:-etcd3}")
STORAGE_MEDIA_TYPE: $(yaml-quote "${STORAGE_MEDIA_TYPE:-}")
ENABLE_GARBAGE_COLLECTOR: $(yaml-quote "${ENABLE_GARBAGE_COLLECTOR:-}")
ENABLE_LEGACY_ABAC: $(yaml-quote "${ENABLE_LEGACY_ABAC:-}")
MASTER_ADVERTISE_ADDRESS: $(yaml-quote "${MASTER_ADVERTISE_ADDRESS:-}")
ETCD_CA_KEY: $(yaml-quote "${ETCD_CA_KEY_BASE64:-}")
ETCD_CA_CERT: $(yaml-quote "${ETCD_CA_CERT_BASE64:-}")
ETCD_PEER_KEY: $(yaml-quote "${ETCD_PEER_KEY_BASE64:-}")
ETCD_PEER_CERT: $(yaml-quote "${ETCD_PEER_CERT_BASE64:-}")
SERVICEACCOUNT_ISSUER: $(yaml-quote "${SERVICEACCOUNT_ISSUER:-}")
KUBECTL_PRUNE_WHITELIST_OVERRIDE: $(yaml-quote "${KUBECTL_PRUNE_WHITELIST_OVERRIDE:-}")
CCM_FEATURE_GATES:  $(yaml-quote "${CCM_FEATURE_GATES:-}")
KUBE_SCHEDULER_RUNASUSER: 2001
KUBE_SCHEDULER_RUNASGROUP: 2001
KUBE_ADDON_MANAGER_RUNASUSER: 2002
KUBE_ADDON_MANAGER_RUNASGROUP: 2002
KUBE_CONTROLLER_MANAGER_RUNASUSER: 2003
KUBE_CONTROLLER_MANAGER_RUNASGROUP: 2003
KUBE_API_SERVER_RUNASUSER: 2004
KUBE_API_SERVER_RUNASGROUP: 2004
KUBE_PKI_READERS_GROUP: 2005
ETCD_RUNASUSER: 2006
ETCD_RUNASGROUP: 2006
KUBE_POD_LOG_READERS_GROUP: 2007
KONNECTIVITY_SERVER_RUNASUSER: 2008
KONNECTIVITY_SERVER_RUNASGROUP: 2008
KONNECTIVITY_SERVER_SOCKET_WRITER_GROUP: 2008
CLOUD_CONTROLLER_MANAGER_RUNASUSER: 2009
CLOUD_CONTROLLER_MANAGER_RUNASGROUP: 2009
CLUSTER_AUTOSCALER_RUNASUSER: 2010
CLUSTER_AUTOSCALER_RUNASGROUP: 2010

EOF
    # KUBE_APISERVER_REQUEST_TIMEOUT_SEC (if set) controls the --request-timeout
    # flag
    if [ -n "${KUBE_APISERVER_REQUEST_TIMEOUT_SEC:-}" ]; then
      cat >>"$file" <<EOF
KUBE_APISERVER_REQUEST_TIMEOUT_SEC: $(yaml-quote "${KUBE_APISERVER_REQUEST_TIMEOUT_SEC}")
EOF
    fi
    # KUBE_APISERVER_GODEBUG (if set) controls the value of GODEBUG env var for kube-apiserver.
    if [ -n "${KUBE_APISERVER_GODEBUG:-}" ]; then
      cat >>"$file" <<EOF
KUBE_APISERVER_GODEBUG: $(yaml-quote "${KUBE_APISERVER_GODEBUG}")
EOF
    fi
    # ETCD_IMAGE (if set) allows to use a custom etcd image.
    if [ -n "${ETCD_IMAGE:-}" ]; then
      cat >>"$file" <<EOF
ETCD_IMAGE: $(yaml-quote "${ETCD_IMAGE}")
EOF
    fi
    # ETCD_DOCKER_REPOSITORY (if set) allows to use a custom etcd docker repository to pull the etcd image from.
    if [ -n "${ETCD_DOCKER_REPOSITORY:-}" ]; then
      cat >>"$file" <<EOF
ETCD_DOCKER_REPOSITORY: $(yaml-quote "${ETCD_DOCKER_REPOSITORY}")
EOF
    fi
    # ETCD_VERSION (if set) allows you to use custom version of etcd.
    # The main purpose of using it may be rollback of etcd v3 API,
    # where we need 3.0.* image, but are rolling back to 2.3.7.
    if [ -n "${ETCD_VERSION:-}" ]; then
      cat >>"$file" <<EOF
ETCD_VERSION: $(yaml-quote "${ETCD_VERSION}")
EOF
    fi
    if [ -n "${ETCD_HOSTNAME:-}" ]; then
      cat >>"$file" <<EOF
ETCD_HOSTNAME: $(yaml-quote "${ETCD_HOSTNAME}")
EOF
    fi
    if [ -n "${ETCD_LIVENESS_PROBE_INITIAL_DELAY_SEC:-}" ]; then
      cat >>"$file" <<EOF
ETCD_LIVENESS_PROBE_INITIAL_DELAY_SEC: $(yaml-quote "${ETCD_LIVENESS_PROBE_INITIAL_DELAY_SEC}")
EOF
    fi
    if [ -n "${KUBE_APISERVER_LIVENESS_PROBE_INITIAL_DELAY_SEC:-}" ]; then
      cat >>"$file" <<EOF
KUBE_APISERVER_LIVENESS_PROBE_INITIAL_DELAY_SEC: $(yaml-quote "${KUBE_APISERVER_LIVENESS_PROBE_INITIAL_DELAY_SEC}")
EOF
    fi
    if [ -n "${ETCD_COMPACTION_INTERVAL_SEC:-}" ]; then
      cat >>"$file" <<EOF
ETCD_COMPACTION_INTERVAL_SEC: $(yaml-quote "${ETCD_COMPACTION_INTERVAL_SEC}")
EOF
    fi
    if [ -n "${ETCD_QUOTA_BACKEND_BYTES:-}" ]; then
      cat >>"$file" <<EOF
ETCD_QUOTA_BACKEND_BYTES: $(yaml-quote "${ETCD_QUOTA_BACKEND_BYTES}")
EOF
    fi
    if [ -n "${ETCD_EXTRA_ARGS:-}" ]; then
    cat >>"$file" <<EOF
ETCD_EXTRA_ARGS: $(yaml-quote "${ETCD_EXTRA_ARGS}")
EOF
    fi
    if [ -n "${ETCD_SERVERS:-}" ]; then
    cat >>"$file" <<EOF
ETCD_SERVERS: $(yaml-quote "${ETCD_SERVERS}")
EOF
    fi
    if [ -n "${ETCD_SERVERS_OVERRIDES:-}" ]; then
    cat >>"$file" <<EOF
ETCD_SERVERS_OVERRIDES: $(yaml-quote "${ETCD_SERVERS_OVERRIDES}")
EOF
    fi
    if [ -n "${APISERVER_TEST_ARGS:-}" ]; then
      cat >>"$file" <<EOF
APISERVER_TEST_ARGS: $(yaml-quote "${APISERVER_TEST_ARGS}")
EOF
    fi
    if [ -n "${CONTROLLER_MANAGER_TEST_ARGS:-}" ]; then
      cat >>"$file" <<EOF
CONTROLLER_MANAGER_TEST_ARGS: $(yaml-quote "${CONTROLLER_MANAGER_TEST_ARGS}")
EOF
    fi
    if [ -n "${KUBE_CONTROLLER_MANAGER_TEST_ARGS:-}" ]; then
      cat >>"$file" <<EOF
KUBE_CONTROLLER_MANAGER_TEST_ARGS: $(yaml-quote "${KUBE_CONTROLLER_MANAGER_TEST_ARGS}")
EOF
    fi
    if [ -n "${CONTROLLER_MANAGER_TEST_LOG_LEVEL:-}" ]; then
      cat >>"$file" <<EOF
CONTROLLER_MANAGER_TEST_LOG_LEVEL: $(yaml-quote "${CONTROLLER_MANAGER_TEST_LOG_LEVEL}")
EOF
    fi
    if [ -n "${SCHEDULER_TEST_ARGS:-}" ]; then
      cat >>"$file" <<EOF
SCHEDULER_TEST_ARGS: $(yaml-quote "${SCHEDULER_TEST_ARGS}")
EOF
    fi
    if [ -n "${SCHEDULER_TEST_LOG_LEVEL:-}" ]; then
      cat >>"$file" <<EOF
SCHEDULER_TEST_LOG_LEVEL: $(yaml-quote "${SCHEDULER_TEST_LOG_LEVEL}")
EOF
    fi
    if [ -n "${INITIAL_ETCD_CLUSTER:-}" ]; then
      cat >>"$file" <<EOF
INITIAL_ETCD_CLUSTER: $(yaml-quote "${INITIAL_ETCD_CLUSTER}")
EOF
    fi
    if [ -n "${INITIAL_ETCD_CLUSTER_STATE:-}" ]; then
      cat >>"$file" <<EOF
INITIAL_ETCD_CLUSTER_STATE: $(yaml-quote "${INITIAL_ETCD_CLUSTER_STATE}")
EOF
    fi
    if [ -n "${CLUSTER_SIGNING_DURATION:-}" ]; then
      cat >>"$file" <<EOF
CLUSTER_SIGNING_DURATION: $(yaml-quote "${CLUSTER_SIGNING_DURATION}")
EOF
    fi
    if [ -n "${ADDON_MANAGER_LEADER_ELECTION:-}" ]; then
      cat >>"$file" <<EOF
ADDON_MANAGER_LEADER_ELECTION: $(yaml-quote "${ADDON_MANAGER_LEADER_ELECTION}")
EOF
    fi
    if [ -n "${API_SERVER_TEST_LOG_LEVEL:-}" ]; then
      cat >>"$file" <<EOF
API_SERVER_TEST_LOG_LEVEL: $(yaml-quote "${API_SERVER_TEST_LOG_LEVEL}")
EOF
    fi
    if [ -n "${ETCD_LISTEN_CLIENT_IP:-}" ]; then
      cat >>"$file" <<EOF
ETCD_LISTEN_CLIENT_IP: $(yaml-quote "${ETCD_LISTEN_CLIENT_IP}")
EOF
    fi
    if [ -n "${ETCD_PROGRESS_NOTIFY_INTERVAL:-}" ]; then
      cat >>"$file" <<EOF
ETCD_PROGRESS_NOTIFY_INTERVAL: $(yaml-quote "${ETCD_PROGRESS_NOTIFY_INTERVAL}")
EOF
    fi

  else
    # Node-only env vars.
    cat >>"$file" <<EOF
KUBERNETES_MASTER: $(yaml-quote "false")
EXTRA_DOCKER_OPTS: $(yaml-quote "${EXTRA_DOCKER_OPTS:-}")
EOF
    if [ -n "${KUBEPROXY_TEST_ARGS:-}" ]; then
      cat >>"$file" <<EOF
KUBEPROXY_TEST_ARGS: $(yaml-quote "${KUBEPROXY_TEST_ARGS}")
EOF
    fi
    if [ -n "${KUBEPROXY_TEST_LOG_LEVEL:-}" ]; then
      cat >>"$file" <<EOF
KUBEPROXY_TEST_LOG_LEVEL: $(yaml-quote "${KUBEPROXY_TEST_LOG_LEVEL}")
EOF
    fi
  fi
  if [[ "${ENABLE_CLUSTER_AUTOSCALER}" == "true" ]]; then
      cat >>"$file" <<EOF
ENABLE_CLUSTER_AUTOSCALER: $(yaml-quote "${ENABLE_CLUSTER_AUTOSCALER}")
AUTOSCALER_MIG_CONFIG: $(yaml-quote "${AUTOSCALER_MIG_CONFIG}")
AUTOSCALER_EXPANDER_CONFIG: $(yaml-quote "${AUTOSCALER_EXPANDER_CONFIG}")
EOF
      if [[ "${master}" == "false" ]]; then
          # TODO(kubernetes/autoscaler#718): AUTOSCALER_ENV_VARS is a hotfix for cluster autoscaler,
          # which reads the kube-env to determine the shape of a node and was broken by #60020.
          # This should be removed as soon as a more reliable source of information is available!
          local node_labels
          local node_taints
          local autoscaler_env_vars
          node_labels="$(build-linux-node-labels node)"
          node_taints="${NODE_TAINTS:-}"
          autoscaler_env_vars="node_labels=${node_labels};node_taints=${node_taints}"
          cat >>"$file" <<EOF
AUTOSCALER_ENV_VARS: $(yaml-quote "${autoscaler_env_vars}")
EOF
      fi
  fi
  if [ -n "${SCHEDULING_ALGORITHM_PROVIDER:-}" ]; then
    cat >>"$file" <<EOF
SCHEDULING_ALGORITHM_PROVIDER: $(yaml-quote "${SCHEDULING_ALGORITHM_PROVIDER}")
EOF
  fi
  if [ -n "${MAX_PODS_PER_NODE:-}" ]; then
    cat >>"$file" <<EOF
MAX_PODS_PER_NODE: $(yaml-quote "${MAX_PODS_PER_NODE}")
EOF
  fi
  if [[ "${PREPARE_KONNECTIVITY_SERVICE:-false}" == "true" ]]; then
      cat >>"$file" <<EOF
PREPARE_KONNECTIVITY_SERVICE: $(yaml-quote "${PREPARE_KONNECTIVITY_SERVICE}")
EOF
  fi
  if [[ "${EGRESS_VIA_KONNECTIVITY:-false}" == "true" ]]; then
      cat >>"$file" <<EOF
EGRESS_VIA_KONNECTIVITY: $(yaml-quote "${EGRESS_VIA_KONNECTIVITY}")
EOF
  fi
  if [[ "${RUN_KONNECTIVITY_PODS:-false}" == "true" ]]; then
      cat >>"$file" <<EOF
RUN_KONNECTIVITY_PODS: $(yaml-quote "${RUN_KONNECTIVITY_PODS}")
EOF
  fi
  if [[ -n "${KONNECTIVITY_SERVICE_PROXY_PROTOCOL_MODE:-}" ]]; then
      cat >>"$file" <<EOF
KONNECTIVITY_SERVICE_PROXY_PROTOCOL_MODE: $(yaml-quote "${KONNECTIVITY_SERVICE_PROXY_PROTOCOL_MODE}")
EOF
  fi
}


function build-windows-kube-env {
  local file="$1"
  # For now the Windows kube-env is a superset of the Linux kube-env.
  build-linux-kube-env false "$file"

  cat >>"$file" <<EOF
WINDOWS_NODE_INSTANCE_PREFIX: $(yaml-quote "${WINDOWS_NODE_INSTANCE_PREFIX}")
NODE_BINARY_TAR_URL: $(yaml-quote "${NODE_BINARY_TAR_URL}")
NODE_BINARY_TAR_HASH: $(yaml-quote "${NODE_BINARY_TAR_HASH}")
CSI_PROXY_STORAGE_PATH: $(yaml-quote "${CSI_PROXY_STORAGE_PATH}")
CSI_PROXY_VERSION: $(yaml-quote "${CSI_PROXY_VERSION}")
CSI_PROXY_FLAGS: $(yaml-quote "${CSI_PROXY_FLAGS}")
ENABLE_CSI_PROXY: $(yaml-quote "${ENABLE_CSI_PROXY}")
K8S_DIR: $(yaml-quote "${WINDOWS_K8S_DIR}")
NODE_DIR: $(yaml-quote "${WINDOWS_NODE_DIR}")
LOGS_DIR: $(yaml-quote "${WINDOWS_LOGS_DIR}")
CNI_DIR: $(yaml-quote "${WINDOWS_CNI_DIR}")
CNI_CONFIG_DIR: $(yaml-quote "${WINDOWS_CNI_CONFIG_DIR}")
WINDOWS_CNI_STORAGE_PATH: $(yaml-quote "${WINDOWS_CNI_STORAGE_PATH}")
WINDOWS_CNI_VERSION: $(yaml-quote "${WINDOWS_CNI_VERSION}")
WINDOWS_CONTAINER_RUNTIME: $(yaml-quote "${WINDOWS_CONTAINER_RUNTIME}")
WINDOWS_CONTAINER_RUNTIME_ENDPOINT: $(yaml-quote "${WINDOWS_CONTAINER_RUNTIME_ENDPOINT:-}")
MANIFESTS_DIR: $(yaml-quote "${WINDOWS_MANIFESTS_DIR}")
PKI_DIR: $(yaml-quote "${WINDOWS_PKI_DIR}")
CA_FILE_PATH: $(yaml-quote "${WINDOWS_CA_FILE}")
KUBELET_CONFIG_FILE: $(yaml-quote "${WINDOWS_KUBELET_CONFIG_FILE}")
KUBEPROXY_ARGS: $(yaml-quote "${KUBEPROXY_ARGS}")
KUBECONFIG_FILE: $(yaml-quote "${WINDOWS_KUBECONFIG_FILE}")
BOOTSTRAP_KUBECONFIG_FILE: $(yaml-quote "${WINDOWS_BOOTSTRAP_KUBECONFIG_FILE}")
KUBEPROXY_KUBECONFIG_FILE: $(yaml-quote "${WINDOWS_KUBEPROXY_KUBECONFIG_FILE}")
WINDOWS_INFRA_CONTAINER: $(yaml-quote "${WINDOWS_INFRA_CONTAINER}")
WINDOWS_ENABLE_PIGZ: $(yaml-quote "${WINDOWS_ENABLE_PIGZ}")
WINDOWS_ENABLE_HYPERV: $(yaml-quote "${WINDOWS_ENABLE_HYPERV}")
ENABLE_AUTH_PROVIDER_GCP: $(yaml-quote "${ENABLE_AUTH_PROVIDER_GCP}")
ENABLE_NODE_PROBLEM_DETECTOR: $(yaml-quote "${WINDOWS_ENABLE_NODE_PROBLEM_DETECTOR}")
NODE_PROBLEM_DETECTOR_VERSION: $(yaml-quote "${NODE_PROBLEM_DETECTOR_VERSION}")
NODE_PROBLEM_DETECTOR_TAR_HASH: $(yaml-quote "${NODE_PROBLEM_DETECTOR_TAR_HASH}")
NODE_PROBLEM_DETECTOR_RELEASE_PATH: $(yaml-quote "${NODE_PROBLEM_DETECTOR_RELEASE_PATH}")
NODE_PROBLEM_DETECTOR_CUSTOM_FLAGS: $(yaml-quote "${WINDOWS_NODE_PROBLEM_DETECTOR_CUSTOM_FLAGS}")
NODE_PROBLEM_DETECTOR_TOKEN: $(yaml-quote "${NODE_PROBLEM_DETECTOR_TOKEN:-}")
WINDOWS_NODEPROBLEMDETECTOR_KUBECONFIG_FILE: $(yaml-quote "${WINDOWS_NODEPROBLEMDETECTOR_KUBECONFIG_FILE}")
AUTH_PROVIDER_GCP_STORAGE_PATH: $(yaml-quote "${AUTH_PROVIDER_GCP_STORAGE_PATH}")
AUTH_PROVIDER_GCP_VERSION: $(yaml-quote "${AUTH_PROVIDER_GCP_VERSION}")
AUTH_PROVIDER_GCP_HASH_WINDOWS_AMD64: $(yaml-quote "${AUTH_PROVIDER_GCP_HASH_WINDOWS_AMD64}")
AUTH_PROVIDER_GCP_WINDOWS_BIN_DIR: $(yaml-quote "${AUTH_PROVIDER_GCP_WINDOWS_BIN_DIR}")
AUTH_PROVIDER_GCP_WINDOWS_CONF_FILE: $(yaml-quote "${AUTH_PROVIDER_GCP_WINDOWS_CONF_FILE}")
EOF
}

function sha512sum-file() {
  local shasum
  if which sha512sum >/dev/null 2>&1; then
    shasum=$(sha512sum "$1")
  else
    shasum=$(shasum -a512 "$1")
  fi
  echo "${shasum%%[[:blank:]]*}"
}

# Create certificate pairs for the cluster.
# $1: The public IP for the master.
#
# These are used for static cert distribution (e.g. static clustering) at
# cluster creation time. This will be obsoleted once we implement dynamic
# clustering.
#
# The following certificate pairs are created:
#
#  - ca (the cluster's certificate authority)
#  - server
#  - kubelet
#  - kubecfg (for kubectl)
#
# TODO(roberthbailey): Replace easyrsa with a simple Go program to generate
# the certs that we need.
#
# Assumed vars
#   KUBE_TEMP
#   MASTER_NAME
#
# Vars set:
#   CERT_DIR
#   CA_CERT_BASE64
#   MASTER_CERT_BASE64
#   MASTER_KEY_BASE64
#   KUBELET_CERT_BASE64
#   KUBELET_KEY_BASE64
#   KUBECFG_CERT_BASE64
#   KUBECFG_KEY_BASE64
function create-certs {
  local -r primary_cn="${1}"

  # Determine extra certificate names for master

  # Create service_ip by stripping the network mask part from
  # SERVICE_CLUSTER_IP_RANGE and incrementing the host part with 1
  service_ip=${SERVICE_CLUSTER_IP_RANGE%/*}
  service_ip="${service_ip%.*}.$((${service_ip##*.} + 1))"
  local sans=""
  for extra in "$@"; do
    if [[ -n "${extra}" ]]; then
      sans="${sans}IP:${extra},"
    fi
  done
  sans="${sans}IP:${service_ip},DNS:kubernetes,DNS:kubernetes.default,DNS:kubernetes.default.svc,DNS:kubernetes.default.svc.${DNS_DOMAIN},DNS:${MASTER_NAME}"

  echo "Generating certs for alternate-names: ${sans}"

  setup-easyrsa
  PRIMARY_CN="${primary_cn}" SANS="${sans}" generate-certs
  AGGREGATOR_PRIMARY_CN="${primary_cn}" AGGREGATOR_SANS="${sans}" generate-aggregator-certs
  KONNECTIVITY_SERVER_PRIMARY_CN="${primary_cn}" KONNECTIVITY_SERVER_SANS="${sans}" generate-konnectivity-server-certs
  CLOUD_PVL_ADMISSION_PRIMARY_CN="${primary_cn}" CLOUD_PVL_ADMISSION_SANS="${sans}" generate-cloud-pvl-admission-certs

  # By default, linux wraps base64 output every 76 cols, so we use 'tr -d' to remove whitespaces.
  # Note 'base64 -w0' doesn't work on Mac OS X, which has different flags.
  CA_KEY_BASE64=$(base64 $BASE64_INPUT_FLAG "${CERT_DIR}/pki/private/ca.key" | tr -d '\r\n')
  CA_CERT_BASE64=$(base64 $BASE64_INPUT_FLAG "${CERT_DIR}/pki/ca.crt" | tr -d '\r\n')
  MASTER_CERT_BASE64=$(base64 $BASE64_INPUT_FLAG "${CERT_DIR}/pki/issued/${MASTER_NAME}.crt" | tr -d '\r\n')
  MASTER_KEY_BASE64=$(base64 $BASE64_INPUT_FLAG "${CERT_DIR}/pki/private/${MASTER_NAME}.key" | tr -d '\r\n')
  KUBELET_CERT_BASE64=$(base64 $BASE64_INPUT_FLAG "${CERT_DIR}/pki/issued/kubelet.crt" | tr -d '\r\n')
  KUBELET_KEY_BASE64=$(base64 $BASE64_INPUT_FLAG "${CERT_DIR}/pki/private/kubelet.key" | tr -d '\r\n')
  KUBECFG_CERT_BASE64=$(base64 $BASE64_INPUT_FLAG "${CERT_DIR}/pki/issued/kubecfg.crt" | tr -d '\r\n')
  KUBECFG_KEY_BASE64=$(base64 $BASE64_INPUT_FLAG "${CERT_DIR}/pki/private/kubecfg.key" | tr -d '\r\n')
  KUBEAPISERVER_CERT_BASE64=$(base64 $BASE64_INPUT_FLAG "${CERT_DIR}/pki/issued/kube-apiserver.crt" | tr -d '\r\n')
  KUBEAPISERVER_KEY_BASE64=$(base64 $BASE64_INPUT_FLAG "${CERT_DIR}/pki/private/kube-apiserver.key" | tr -d '\r\n')

  # Setting up an addition directory (beyond pki) as it is the simplest way to
  # ensure we get a different CA pair to sign the proxy-client certs and which
  # we can send CA public key to the user-apiserver to validate communication.
  AGGREGATOR_CA_KEY_BASE64=$(base64 $BASE64_INPUT_FLAG "${AGGREGATOR_CERT_DIR}/pki/private/ca.key" | tr -d '\r\n')
  REQUESTHEADER_CA_CERT_BASE64=$(base64 $BASE64_INPUT_FLAG "${AGGREGATOR_CERT_DIR}/pki/ca.crt" | tr -d '\r\n')
  PROXY_CLIENT_CERT_BASE64=$(base64 $BASE64_INPUT_FLAG "${AGGREGATOR_CERT_DIR}/pki/issued/proxy-client.crt" | tr -d '\r\n')
  PROXY_CLIENT_KEY_BASE64=$(base64 $BASE64_INPUT_FLAG "${AGGREGATOR_CERT_DIR}/pki/private/proxy-client.key" | tr -d '\r\n')

  # Setting up the Kubernetes API Server Konnectivity Server auth.
  # This includes certs for both API Server to Konnectivity Server and
  # Konnectivity Agent to Konnectivity Server.
  KONNECTIVITY_SERVER_CA_KEY_BASE64=$(base64 $BASE64_INPUT_FLAG "${KONNECTIVITY_SERVER_CERT_DIR}/pki/private/ca.key" | tr -d '\r\n')
  KONNECTIVITY_SERVER_CA_CERT_BASE64=$(base64 $BASE64_INPUT_FLAG "${KONNECTIVITY_SERVER_CERT_DIR}/pki/ca.crt" | tr -d '\r\n')
  KONNECTIVITY_SERVER_CERT_BASE64=$(base64 $BASE64_INPUT_FLAG "${KONNECTIVITY_SERVER_CERT_DIR}/pki/issued/server.crt" | tr -d '\r\n')
  KONNECTIVITY_SERVER_KEY_BASE64=$(base64 $BASE64_INPUT_FLAG "${KONNECTIVITY_SERVER_CERT_DIR}/pki/private/server.key" | tr -d '\r\n')
  KONNECTIVITY_SERVER_CLIENT_CERT_BASE64=$(base64 $BASE64_INPUT_FLAG "${KONNECTIVITY_SERVER_CERT_DIR}/pki/issued/client.crt" | tr -d '\r\n')
  KONNECTIVITY_SERVER_CLIENT_KEY_BASE64=$(base64 $BASE64_INPUT_FLAG "${KONNECTIVITY_SERVER_CERT_DIR}/pki/private/client.key" | tr -d '\r\n')
  KONNECTIVITY_AGENT_CA_KEY_BASE64=$(base64 $BASE64_INPUT_FLAG "${KONNECTIVITY_AGENT_CERT_DIR}/pki/private/ca.key" | tr -d '\r\n')
  KONNECTIVITY_AGENT_CA_CERT_BASE64=$(base64 $BASE64_INPUT_FLAG "${KONNECTIVITY_AGENT_CERT_DIR}/pki/ca.crt" | tr -d '\r\n')
  KONNECTIVITY_AGENT_CERT_BASE64=$(base64 $BASE64_INPUT_FLAG "${KONNECTIVITY_AGENT_CERT_DIR}/pki/issued/server.crt" | tr -d '\r\n')
  KONNECTIVITY_AGENT_KEY_BASE64=$(base64 $BASE64_INPUT_FLAG "${KONNECTIVITY_AGENT_CERT_DIR}/pki/private/server.key" | tr -d '\r\n')
  KONNECTIVITY_AGENT_CLIENT_CERT_BASE64=$(base64 $BASE64_INPUT_FLAG "${KONNECTIVITY_AGENT_CERT_DIR}/pki/issued/client.crt" | tr -d '\r\n')
  KONNECTIVITY_AGENT_CLIENT_KEY_BASE64=$(base64 $BASE64_INPUT_FLAG "${KONNECTIVITY_AGENT_CERT_DIR}/pki/private/client.key" | tr -d '\r\n')

  CLOUD_PVL_ADMISSION_CA_KEY_BASE64=$(base64 $BASE64_INPUT_FLAG "${CLOUD_PVL_ADMISSION_CERT_DIR}/pki/private/ca.key" | tr -d '\r\n')
  CLOUD_PVL_ADMISSION_CA_CERT_BASE64=$(base64 $BASE64_INPUT_FLAG "${CLOUD_PVL_ADMISSION_CERT_DIR}/pki/ca.crt" | tr -d '\r\n')
  CLOUD_PVL_ADMISSION_CERT_BASE64=$(base64 $BASE64_INPUT_FLAG "${CLOUD_PVL_ADMISSION_CERT_DIR}/pki/issued/server.crt" | tr -d '\r\n')
  CLOUD_PVL_ADMISSION_KEY_BASE64=$(base64 $BASE64_INPUT_FLAG "${CLOUD_PVL_ADMISSION_CERT_DIR}/pki/private/server.key" | tr -d '\r\n')
}

# Set up easy-rsa directory structure.
#
# Assumed vars
#   KUBE_TEMP
#
# Vars set:
#   CERT_DIR
#   AGGREGATOR_CERT_DIR
function setup-easyrsa {
  local -r cert_create_debug_output=$(mktemp "${KUBE_TEMP}/cert_create_debug_output.XXX")
  # Note: This was heavily cribbed from make-ca-cert.sh
  (set -x
    cd "${KUBE_TEMP}"
    curl -L -o easy-rsa.tar.gz --connect-timeout 20 --retry 6 --retry-delay 2 https://github.com/OpenVPN/easy-rsa/releases/download/v3.1.7/EasyRSA-3.1.7.tgz
    mkdir -p easy-rsa && tar xzf easy-rsa.tar.gz -C easy-rsa --strip-components 1
    local -r files_to_copy=(easyrsa openssl-easyrsa.cnf  x509-types vars.example)
    mkdir easy-rsa/aggregator
    # shellcheck disable=SC2145
    cp -r easy-rsa/"${files_to_copy[@]}" easy-rsa/aggregator
    mkdir easy-rsa/cloud-pvl-admission
    # shellcheck disable=SC2145
    cp -r easy-rsa/"${files_to_copy[@]}" easy-rsa/cloud-pvl-admission
    mkdir easy-rsa/konnectivity-server
    # shellcheck disable=SC2145
    cp -r easy-rsa/"${files_to_copy[@]}" easy-rsa/konnectivity-server
    mkdir easy-rsa/konnectivity-agent
    # shellcheck disable=SC2145
    cp -r easy-rsa/"${files_to_copy[@]}" easy-rsa/konnectivity-agent) &>"${cert_create_debug_output}" || true
  CERT_DIR="${KUBE_TEMP}/easy-rsa"
  AGGREGATOR_CERT_DIR="${KUBE_TEMP}/easy-rsa/aggregator"
  CLOUD_PVL_ADMISSION_CERT_DIR="${KUBE_TEMP}/easy-rsa/cloud-pvl-admission"
  KONNECTIVITY_SERVER_CERT_DIR="${KUBE_TEMP}/easy-rsa/konnectivity-server"
  KONNECTIVITY_AGENT_CERT_DIR="${KUBE_TEMP}/easy-rsa/konnectivity-agent"
  if [ ! -x "${CERT_DIR}/easyrsa" ] || [ ! -x "${AGGREGATOR_CERT_DIR}/easyrsa" ]; then
    # TODO(roberthbailey,porridge): add better error handling here,
    # see https://github.com/kubernetes/kubernetes/issues/55229
    cat "${cert_create_debug_output}" >&2
    echo "=== Failed to setup easy-rsa: Aborting ===" >&2
    exit 2
  fi
}

# Runs the easy RSA commands to generate certificate files.
# The generated files are IN ${CERT_DIR}
#
# Assumed vars (see shellcheck disable directives below)
#   KUBE_TEMP
#   MASTER_NAME
#   CERT_DIR
#   PRIMARY_CN: Primary canonical name
#   SANS: Subject alternate names
#
#
function generate-certs {
  local -r cert_create_debug_output=$(mktemp "${KUBE_TEMP}/cert_create_debug_output.XXX")
  # Note: This was heavily cribbed from make-ca-cert.sh
  (set -x
    cd "${CERT_DIR}"
    ./easyrsa init-pki
    # this puts the cert into pki/ca.crt and the key into pki/private/ca.key
    # PRIMARY_CN (expected to be) defined by caller
    # shellcheck disable=SC2153
    ./easyrsa --batch "--req-cn=${PRIMARY_CN}@$(date +%s)" build-ca nopass
    # SANS (expected to be) defined by caller
    # shellcheck disable=SC2153
    ./easyrsa --batch --subject-alt-name="${SANS}" build-server-full "${MASTER_NAME}" nopass
    ./easyrsa --batch build-client-full kube-apiserver nopass

    kube::util::ensure-cfssl "${KUBE_TEMP}/cfssl"

    # make the config for the signer
    echo '{"signing":{"default":{"expiry":"43800h","usages":["signing","key encipherment","client auth"]}}}' > "ca-config.json"
    # create the kubelet client cert with the correct groups
    echo '{"CN":"kubelet","names":[{"O":"system:nodes"}],"hosts":[""],"key":{"algo":"rsa","size":2048}}' | "${CFSSL_BIN}" gencert -ca=pki/ca.crt -ca-key=pki/private/ca.key -config=ca-config.json - | "${CFSSLJSON_BIN}" -bare kubelet
    mv "kubelet-key.pem" "pki/private/kubelet.key"
    mv "kubelet.pem" "pki/issued/kubelet.crt"
    rm -f "kubelet.csr"

    # Make a superuser client cert with subject "O=system:masters, CN=kubecfg"
    ./easyrsa --batch --dn-mode=org \
      --req-org=system:masters \
      --req-c= --req-st= --req-city= --req-email= --req-ou= \
      build-client-full kubecfg nopass) &>"${cert_create_debug_output}" || true
  local output_file_missing=0
  local output_file
  for output_file in \
    "${CERT_DIR}/pki/private/ca.key" \
    "${CERT_DIR}/pki/ca.crt" \
    "${CERT_DIR}/pki/issued/${MASTER_NAME}.crt" \
    "${CERT_DIR}/pki/private/${MASTER_NAME}.key" \
    "${CERT_DIR}/pki/issued/kubelet.crt" \
    "${CERT_DIR}/pki/private/kubelet.key" \
    "${CERT_DIR}/pki/issued/kubecfg.crt" \
    "${CERT_DIR}/pki/private/kubecfg.key" \
    "${CERT_DIR}/pki/issued/kube-apiserver.crt" \
    "${CERT_DIR}/pki/private/kube-apiserver.key"
  do
    if [[ ! -s "${output_file}" ]]; then
      echo "Expected file ${output_file} not created" >&2
      output_file_missing=1
    fi
  done
  if [ $output_file_missing -ne 0 ]; then
    # TODO(roberthbailey,porridge): add better error handling here,
    # see https://github.com/kubernetes/kubernetes/issues/55229
    cat "${cert_create_debug_output}" >&2
    echo "=== Failed to generate master certificates: Aborting ===" >&2
    exit 2
  fi
}

# Runs the easy RSA commands to generate aggregator certificate files.
# The generated files are in ${AGGREGATOR_CERT_DIR}
#
# Assumed vars
#   KUBE_TEMP
#   AGGREGATOR_MASTER_NAME
#   AGGREGATOR_CERT_DIR
#   AGGREGATOR_PRIMARY_CN: Primary canonical name
#   AGGREGATOR_SANS: Subject alternate names
#
#
function generate-aggregator-certs {
  local -r cert_create_debug_output=$(mktemp "${KUBE_TEMP}/cert_create_debug_output.XXX")
  # Note: This was heavily cribbed from make-ca-cert.sh
  (set -x
    cd "${KUBE_TEMP}/easy-rsa/aggregator"
    ./easyrsa init-pki
    # this puts the cert into pki/ca.crt and the key into pki/private/ca.key
    ./easyrsa --batch "--req-cn=${AGGREGATOR_PRIMARY_CN}@$(date +%s)" build-ca nopass
    ./easyrsa --batch --subject-alt-name="${AGGREGATOR_SANS}" build-server-full "${AGGREGATOR_MASTER_NAME}" nopass
    ./easyrsa --batch build-client-full aggregator-apiserver nopass

    kube::util::ensure-cfssl "${KUBE_TEMP}/cfssl"

    # make the config for the signer
    echo '{"signing":{"default":{"expiry":"43800h","usages":["signing","key encipherment","client auth"]}}}' > "ca-config.json"
    # create the aggregator client cert with the correct groups
    echo '{"CN":"aggregator","hosts":[""],"key":{"algo":"rsa","size":2048}}' | "${CFSSL_BIN}" gencert -ca=pki/ca.crt -ca-key=pki/private/ca.key -config=ca-config.json - | "${CFSSLJSON_BIN}" -bare proxy-client
    mv "proxy-client-key.pem" "pki/private/proxy-client.key"
    mv "proxy-client.pem" "pki/issued/proxy-client.crt"
    rm -f "proxy-client.csr"

    # Make a superuser client cert with subject "O=system:masters, CN=kubecfg"
    ./easyrsa --batch --dn-mode=org \
      --req-org=system:aggregator \
      --req-c= --req-st= --req-city= --req-email= --req-ou= \
      build-client-full proxy-clientcfg nopass) &>"${cert_create_debug_output}" || true
  local output_file_missing=0
  local output_file
  for output_file in \
    "${AGGREGATOR_CERT_DIR}/pki/private/ca.key" \
    "${AGGREGATOR_CERT_DIR}/pki/ca.crt" \
    "${AGGREGATOR_CERT_DIR}/pki/issued/proxy-client.crt" \
    "${AGGREGATOR_CERT_DIR}/pki/private/proxy-client.key"
  do
    if [[ ! -s "${output_file}" ]]; then
      echo "Expected file ${output_file} not created" >&2
      output_file_missing=1
    fi
  done
  if [ $output_file_missing -ne 0 ]; then
    # TODO(roberthbailey,porridge): add better error handling here,
    # see https://github.com/kubernetes/kubernetes/issues/55229
    cat "${cert_create_debug_output}" >&2
    echo "=== Failed to generate aggregator certificates: Aborting ===" >&2
    exit 2
  fi
}

# Runs the easy RSA commands to generate server side certificate files
# for the konnectivity server. This includes both server side to both
# konnectivity-server and konnectivity-agent.
# The generated files are in ${KONNECTIVITY_SERVER_CERT_DIR} and
# ${KONNECTIVITY_AGENT_CERT_DIR}
#
# Assumed vars
#   KUBE_TEMP
#   KONNECTIVITY_SERVER_CERT_DIR
#   KONNECTIVITY_SERVER_PRIMARY_CN: Primary canonical name
#   KONNECTIVITY_SERVER_SANS: Subject alternate names
#
function generate-konnectivity-server-certs {
  local -r cert_create_debug_output=$(mktemp "${KUBE_TEMP}/cert_create_debug_output.XXX")
  # Note: This was heavily cribbed from make-ca-cert.sh
  (set -x
    # Make the client <-> konnectivity server side certificates.
    cd "${KUBE_TEMP}/easy-rsa/konnectivity-server"
    ./easyrsa init-pki
    # this puts the cert into pki/ca.crt and the key into pki/private/ca.key
    ./easyrsa --batch "--req-cn=${KONNECTIVITY_SERVER_PRIMARY_CN}@$(date +%s)" build-ca nopass
    ./easyrsa --batch --subject-alt-name="IP:127.0.0.1,${KONNECTIVITY_SERVER_SANS}" build-server-full server nopass
    ./easyrsa --batch build-client-full client nopass

    kube::util::ensure-cfssl "${KUBE_TEMP}/cfssl"

    # make the config for the signer
    echo '{"signing":{"default":{"expiry":"43800h","usages":["signing","key encipherment","client auth"]}}}' > "ca-config.json"
    # create the konnectivity server cert with the correct groups
    echo '{"CN":"konnectivity-server","hosts":[""],"key":{"algo":"rsa","size":2048}}' | "${CFSSL_BIN}" gencert -ca=pki/ca.crt -ca-key=pki/private/ca.key -config=ca-config.json - | "${CFSSLJSON_BIN}" -bare konnectivity-server
    rm -f "konnectivity-server.csr"

    # Make the agent <-> konnectivity server side certificates.
    cd "${KUBE_TEMP}/easy-rsa/konnectivity-agent"
    ./easyrsa init-pki
    # this puts the cert into pki/ca.crt and the key into pki/private/ca.key
    ./easyrsa --batch "--req-cn=${KONNECTIVITY_SERVER_PRIMARY_CN}@$(date +%s)" build-ca nopass
    ./easyrsa --batch --subject-alt-name="${KONNECTIVITY_SERVER_SANS}" build-server-full server nopass
    ./easyrsa --batch build-client-full client nopass

    kube::util::ensure-cfssl "${KUBE_TEMP}/cfssl"

    # make the config for the signer
    echo '{"signing":{"default":{"expiry":"43800h","usages":["signing","key encipherment","agent auth"]}}}' > "ca-config.json"
    # create the konnectivity server cert with the correct groups
    echo '{"CN":"koonectivity-server","hosts":[""],"key":{"algo":"rsa","size":2048}}' | "${CFSSL_BIN}" gencert -ca=pki/ca.crt -ca-key=pki/private/ca.key -config=ca-config.json - | "${CFSSLJSON_BIN}" -bare konnectivity-agent
    rm -f "konnectivity-agent.csr"

    echo "completed main certificate section") &>"${cert_create_debug_output}" || true

  local output_file_missing=0
  local output_file
  for output_file in \
    "${KONNECTIVITY_SERVER_CERT_DIR}/pki/private/ca.key" \
    "${KONNECTIVITY_SERVER_CERT_DIR}/pki/ca.crt" \
    "${KONNECTIVITY_SERVER_CERT_DIR}/pki/issued/server.crt" \
    "${KONNECTIVITY_SERVER_CERT_DIR}/pki/private/server.key" \
    "${KONNECTIVITY_SERVER_CERT_DIR}/pki/issued/client.crt" \
    "${KONNECTIVITY_SERVER_CERT_DIR}/pki/private/client.key" \
    "${KONNECTIVITY_AGENT_CERT_DIR}/pki/private/ca.key" \
    "${KONNECTIVITY_AGENT_CERT_DIR}/pki/ca.crt" \
    "${KONNECTIVITY_AGENT_CERT_DIR}/pki/issued/server.crt" \
    "${KONNECTIVITY_AGENT_CERT_DIR}/pki/private/server.key" \
    "${KONNECTIVITY_AGENT_CERT_DIR}/pki/issued/client.crt" \
    "${KONNECTIVITY_AGENT_CERT_DIR}/pki/private/client.key"
  do
    if [[ ! -s "${output_file}" ]]; then
      echo "Expected file ${output_file} not created" >&2
      output_file_missing=1
    fi
  done
  if (( output_file_missing )); then
    # TODO(roberthbailey,porridge): add better error handling here,
    # see https://github.com/kubernetes/kubernetes/issues/55229
    cat "${cert_create_debug_output}" >&2
    echo "=== Failed to generate konnectivity-server certificates: Aborting ===" >&2
    exit 2
  fi
}

# Runs the easy RSA commands to generate server side certificate files
# for the cloud-pvl-admission webhook.
# The generated files are in ${CLOUD_PVL_ADMISSION_CERT_DIR}
#
# Assumed vars
#   KUBE_TEMP
#   CLOUD_PVL_ADMISSION_CERT_DIR
#   CLOUD_PVL_ADMISSION_PRIMARY_CN: Primary canonical name
#   CLOUD_PVL_ADMISSION_SANS: Subject alternate names
#
function generate-cloud-pvl-admission-certs {
  local -r cert_create_debug_output=$(mktemp "${KUBE_TEMP}/cert_create_debug_output.XXX")
  # Note: This was heavily cribbed from make-ca-cert.sh
  (set -x
    # Make the client <-> cloud-pvl-admission server side certificates.
    cd "${KUBE_TEMP}/easy-rsa/cloud-pvl-admission"
    ./easyrsa init-pki
    # this puts the cert into pki/ca.crt and the key into pki/private/ca.key
    ./easyrsa --batch "--req-cn=${CLOUD_PVL_ADMISSION_PRIMARY_CN}@$(date +%s)" build-ca nopass
    ./easyrsa --batch --subject-alt-name="IP:127.0.0.1,${CLOUD_PVL_ADMISSION_SANS}" build-server-full server nopass
    ./easyrsa --batch build-client-full client nopass

    kube::util::ensure-cfssl "${KUBE_TEMP}/cfssl"

    # make the config for the signer
    echo '{"signing":{"default":{"expiry":"43800h","usages":["signing","key encipherment","client auth"]}}}' > "ca-config.json"
    # create the cloud-pvl-admission cert with the correct groups
    echo '{"CN":"cloud-pvl-admission","hosts":[""],"key":{"algo":"rsa","size":2048}}' | "${CFSSL_BIN}" gencert -ca=pki/ca.crt -ca-key=pki/private/ca.key -config=ca-config.json - | "${CFSSLJSON_BIN}" -bare cloud-pvl-admission
    rm -f "cloud-pvl-admission.csr"

    # Make the cloud-pvl-admission server side certificates.
    cd "${KUBE_TEMP}/easy-rsa/cloud-pvl-admission"
    ./easyrsa init-pki
    # this puts the cert into pki/ca.crt and the key into pki/private/ca.key
    ./easyrsa --batch "--req-cn=${CLOUD_PVL_ADMISSION_PRIMARY_CN}@$(date +%s)" build-ca nopass
    ./easyrsa --batch --subject-alt-name="${CLOUD_PVL_ADMISSION_SANS}" build-server-full server nopass
    ./easyrsa --batch build-client-full client nopass

    kube::util::ensure-cfssl "${KUBE_TEMP}/cfssl"

    # make the config for the signer
    echo '{"signing":{"default":{"expiry":"43800h","usages":["signing","key encipherment","agent auth"]}}}' > "ca-config.json"
    # create the cloud-pvl-admission server cert with the correct groups
    echo '{"CN":"cloud-pvl-admission","hosts":[""],"key":{"algo":"rsa","size":2048}}' | "${CFSSL_BIN}" gencert -ca=pki/ca.crt -ca-key=pki/private/ca.key -config=ca-config.json - | "${CFSSLJSON_BIN}" -bare konnectivity-agent
    rm -f "konnectivity-agent.csr"

    echo "completed main certificate section") &>"${cert_create_debug_output}" || true

  local output_file_missing=0
  local output_file
  for output_file in \
    "${CLOUD_PVL_ADMISSION_CERT_DIR}/pki/private/ca.key" \
    "${CLOUD_PVL_ADMISSION_CERT_DIR}/pki/ca.crt" \
    "${CLOUD_PVL_ADMISSION_CERT_DIR}/pki/issued/server.crt" \
    "${CLOUD_PVL_ADMISSION_CERT_DIR}/pki/private/server.key" \
    "${CLOUD_PVL_ADMISSION_CERT_DIR}/pki/issued/client.crt" \
    "${CLOUD_PVL_ADMISSION_CERT_DIR}/pki/private/client.key"
  do
    if [[ ! -s "${output_file}" ]]; then
      echo "Expected file ${output_file} not created" >&2
      output_file_missing=1
    fi
  done
  if (( output_file_missing )); then
    # TODO(roberthbailey,porridge): add better error handling here,
    # see https://github.com/kubernetes/kubernetes/issues/55229
    cat "${cert_create_debug_output}" >&2
    echo "=== Failed to generate cloud-pvl-admission certificates: Aborting ===" >&2
    exit 2
  fi
}

# Using provided master env, extracts value from provided key.
#
# Args:
# $1 master env (kube-env of master; result of calling get-master-env)
# $2 env key to use
function get-env-val() {
  local match
  match=$( (echo "${1}" | grep -E "^${2}:") || echo '')
  if [[ -z "${match}" ]]; then
    echo ""
  fi
  echo "${match}" | cut -d : -f 2 | cut -d \' -f 2
}

# Load the master env by calling get-master-env, and extract important values
function parse-master-env() {
  # Get required master env vars
  local master_env
  master_env=$(get-master-env)
  KUBE_PROXY_TOKEN=$(get-env-val "${master_env}" "KUBE_PROXY_TOKEN")
  NODE_PROBLEM_DETECTOR_TOKEN=$(get-env-val "${master_env}" "NODE_PROBLEM_DETECTOR_TOKEN")
  CA_CERT_BASE64=$(get-env-val "${master_env}" "CA_CERT")
  CA_KEY_BASE64=$(get-env-val "${master_env}" "CA_KEY")
  KUBEAPISERVER_CERT_BASE64=$(get-env-val "${master_env}" "KUBEAPISERVER_CERT")
  KUBEAPISERVER_KEY_BASE64=$(get-env-val "${master_env}" "KUBEAPISERVER_KEY")
  EXTRA_DOCKER_OPTS=$(get-env-val "${master_env}" "EXTRA_DOCKER_OPTS")
  KUBELET_CERT_BASE64=$(get-env-val "${master_env}" "KUBELET_CERT")
  KUBELET_KEY_BASE64=$(get-env-val "${master_env}" "KUBELET_KEY")
  MASTER_CERT_BASE64=$(get-env-val "${master_env}" "MASTER_CERT")
  MASTER_KEY_BASE64=$(get-env-val "${master_env}" "MASTER_KEY")
  AGGREGATOR_CA_KEY_BASE64=$(get-env-val "${master_env}" "AGGREGATOR_CA_KEY")
  REQUESTHEADER_CA_CERT_BASE64=$(get-env-val "${master_env}" "REQUESTHEADER_CA_CERT")
  PROXY_CLIENT_CERT_BASE64=$(get-env-val "${master_env}" "PROXY_CLIENT_CERT")
  PROXY_CLIENT_KEY_BASE64=$(get-env-val "${master_env}" "PROXY_CLIENT_KEY")
  ENABLE_LEGACY_ABAC=$(get-env-val "${master_env}" "ENABLE_LEGACY_ABAC")
  ETCD_APISERVER_CA_KEY_BASE64=$(get-env-val "${master_env}" "ETCD_APISERVER_CA_KEY")
  ETCD_APISERVER_CA_CERT_BASE64=$(get-env-val "${master_env}" "ETCD_APISERVER_CA_CERT")
  ETCD_APISERVER_SERVER_KEY_BASE64=$(get-env-val "${master_env}" "ETCD_APISERVER_SERVER_KEY")
  ETCD_APISERVER_SERVER_CERT_BASE64=$(get-env-val "${master_env}" "ETCD_APISERVER_SERVER_CERT")
  ETCD_APISERVER_CLIENT_KEY_BASE64=$(get-env-val "${master_env}" "ETCD_APISERVER_CLIENT_KEY")
  ETCD_APISERVER_CLIENT_CERT_BASE64=$(get-env-val "${master_env}" "ETCD_APISERVER_CLIENT_CERT")
  CLOUD_PVL_ADMISSION_CA_KEY_BASE64=$(get-env-val "${master_env}" "CLOUD_PVL_ADMISSION_CA_KEY")
  CLOUD_PVL_ADMISSION_CA_CERT_BASE64=$(get-env-val "${master_env}" "CLOUD_PVL_ADMISSION_CA_CERT")
  CLOUD_PVL_ADMISSION_CERT_BASE64=$(get-env-val "${master_env}" "CLOUD_PVL_ADMISSION_CERT")
  CLOUD_PVL_ADMISSION_KEY_BASE64=$(get-env-val "${master_env}" "CLOUD_PVL_ADMISSION_KEY")
  KONNECTIVITY_SERVER_CA_KEY_BASE64=$(get-env-val "${master_env}" "KONNECTIVITY_SERVER_CA_KEY")
  KONNECTIVITY_SERVER_CA_CERT_BASE64=$(get-env-val "${master_env}" "KONNECTIVITY_SERVER_CA_CERT")
  KONNECTIVITY_SERVER_CERT_BASE64=$(get-env-val "${master_env}" "KONNECTIVITY_SERVER_CERT")
  KONNECTIVITY_SERVER_KEY_BASE64=$(get-env-val "${master_env}" "KONNECTIVITY_SERVER_KEY")
  KONNECTIVITY_SERVER_CLIENT_CERT_BASE64=$(get-env-val "${master_env}" "KONNECTIVITY_SERVER_CLIENT_CERT")
  KONNECTIVITY_SERVER_CLIENT_KEY_BASE64=$(get-env-val "${master_env}" "KONNECTIVITY_SERVER_CLIENT_KEY")
  KONNECTIVITY_AGENT_CA_KEY_BASE64=$(get-env-val "${master_env}" "KONNECTIVITY_AGENT_CA_KEY")
  KONNECTIVITY_AGENT_CA_CERT_BASE64=$(get-env-val "${master_env}" "KONNECTIVITY_AGENT_CA_CERT")
  KONNECTIVITY_AGENT_CERT_BASE64=$(get-env-val "${master_env}" "KONNECTIVITY_AGENT_CERT")
  KONNECTIVITY_AGENT_KEY_BASE64=$(get-env-val "${master_env}" "KONNECTIVITY_AGENT_KEY")
}

# Update or verify required gcloud components are installed
# at minimum required version.
# Assumed vars
#   KUBE_PROMPT_FOR_UPDATE
function update-or-verify-gcloud() {
  local sudo_prefix=""
  if [ ! -w "$(dirname "$(which gcloud)")" ]; then
    sudo_prefix="sudo"
  fi
  # update and install components as needed
  # (deliberately word split $gcloud_prompt)
  # shellcheck disable=SC2086
  if [[ "${KUBE_PROMPT_FOR_UPDATE}" == "y" ]]; then
    ${sudo_prefix} gcloud ${gcloud_prompt:-} components install alpha
    ${sudo_prefix} gcloud ${gcloud_prompt:-} components install beta
    ${sudo_prefix} gcloud ${gcloud_prompt:-} components update
  else
    local version
    version=$(gcloud version --format=json)
    python3 -c"
import json,sys
from distutils import version

minVersion = version.LooseVersion('1.3.0')
required = [ 'alpha', 'beta', 'core' ]
data = json.loads(sys.argv[1])
rel = data.get('Google Cloud SDK')
if 'CL @' in rel:
  print('Using dev version of gcloud: %s' %rel)
  exit(0)
if rel != 'HEAD' and version.LooseVersion(rel) < minVersion:
  print('gcloud version out of date ( < %s )' % minVersion)
  exit(1)
missing = []
for c in required:
  if not data.get(c):
    missing += [c]
if missing:
  for c in missing:
    print ('missing required gcloud component \"{0}\"'.format(c))
    print ('Try running \$(gcloud components install {0})'.format(c))
  exit(1)
    " "${version}"
  fi
}

# Robustly try to create a static ip.
# $1: The name of the ip to create
# $2: The name of the region to create the ip in.
function create-static-ip() {
  detect-project
  local attempt=0
  local REGION="$2"
  while true; do
    if gcloud compute addresses create "$1" \
      --project "${PROJECT}" \
      --region "${REGION}" -q > /dev/null; then
      # successful operation - wait until it's visible
      start="$(date +%s)"
      while true; do
        now="$(date +%s)"
        # Timeout set to 15 minutes
        if [[ $((now - start)) -gt 900 ]]; then
          echo "Timeout while waiting for master IP visibility"
          exit 2
        fi
        if gcloud compute addresses describe "$1" --project "${PROJECT}" --region "${REGION}" >/dev/null 2>&1; then
          break
        fi
        echo "Master IP not visible yet. Waiting..."
        sleep 5
      done
      break
    fi

    if gcloud compute addresses describe "$1" \
      --project "${PROJECT}" \
      --region "${REGION}" >/dev/null 2>&1; then
      # it exists - postcondition satisfied
      break
    fi

    if (( attempt > 4 )); then
      echo -e "${color_red}Failed to create static ip $1 ${color_norm}" >&2
      exit 2
    fi
    attempt=$((attempt + 1))
    echo -e "${color_yellow:-}Attempt $attempt failed to create static ip $1. Retrying.${color_norm:-}" >&2
    sleep $((attempt * 5))
  done
}

# Robustly try to create a firewall rule.
# $1: The name of firewall rule.
# $2: IP ranges.
# $3: Target tags for this firewall rule.
function create-firewall-rule() {
  detect-project
  local attempt=0
  while true; do
    if ! gcloud compute firewall-rules create "$1" \
      --project "${NETWORK_PROJECT}" \
      --network "${NETWORK}" \
      --source-ranges "$2" \
      --target-tags "$3" \
      --allow tcp,udp,icmp,esp,ah,sctp; then
      if (( attempt > 4 )); then
        echo -e "${color_red}Failed to create firewall rule $1 ${color_norm}" >&2
        exit 2
      fi
      echo -e "${color_yellow}Attempt $((attempt + 1)) failed to create firewall rule $1. Retrying.${color_norm}" >&2
      attempt=$((attempt + 1))
      sleep $((attempt * 5))
    else
        break
    fi
  done
}

# Format the string argument for gcloud network.
function make-gcloud-network-argument() {
  local network_project="$1"
  local region="$2"
  local network="$3"
  local subnet="$4"
  local address="$5"          # optional
  local enable_ip_alias="$6"  # optional
  local alias_size="$7"       # optional

  local networkURL="projects/${network_project}/global/networks/${network}"
  local subnetURL="projects/${network_project}/regions/${region}/subnetworks/${subnet:-}"

  local ret=""

  if [[ "${enable_ip_alias}" == 'true' ]]; then
    ret="--network-interface"
    ret="${ret} network=${networkURL}"
    if [[ "${address:-}" == "no-address" ]]; then
      ret="${ret},no-address"
    else
      ret="${ret},address=${address:-}"
    fi
    ret="${ret},subnet=${subnetURL}"
    ret="${ret},aliases=pods-default:${alias_size}"
    ret="${ret} --no-can-ip-forward"
  else
    if [[ -n ${subnet:-} ]]; then
      ret="${ret} --subnet ${subnetURL}"
    else
      ret="${ret} --network ${networkURL}"
    fi

    ret="${ret} --can-ip-forward"
    if [[ -n ${address:-} ]] && [[ "$address" != "no-address" ]]; then
      ret="${ret} --address ${address}"
    fi
  fi

  echo "${ret}"
}

# $1: version (required)
# $2: Prefix for the template name, i.e. NODE_INSTANCE_PREFIX or
#     WINDOWS_NODE_INSTANCE_PREFIX.
function get-template-name-from-version() {
  local -r version=${1}
  local -r template_prefix=${2}
  # trim template name to pass gce name validation
  echo "${template_prefix}-template-${version}" | cut -c 1-63 | sed 's/[\.\+]/-/g;s/-*$//g'
}

# validates the NODE_LOCAL_SSDS_EXT variable
function validate-node-local-ssds-ext(){
  ssdopts="${1}"

  if [[ -z "${ssdopts[0]}" || -z "${ssdopts[1]}" || -z "${ssdopts[2]}" ]]; then
    echo -e "${color_red}Local SSD: NODE_LOCAL_SSDS_EXT is malformed, found ${ssdopts[0]-_},${ssdopts[1]-_},${ssdopts[2]-_} ${color_norm}" >&2
    exit 2
  fi
  if [[ "${ssdopts[1]}" != "scsi" && "${ssdopts[1]}" != "nvme" ]]; then
    echo -e "${color_red}Local SSD: Interface must be scsi or nvme, found: ${ssdopts[1]} ${color_norm}" >&2
    exit 2
  fi
  if [[ "${ssdopts[2]}" != "fs" && "${ssdopts[2]}" != "block" ]]; then
    echo -e "${color_red}Local SSD: Filesystem type must be fs or block, found: ${ssdopts[2]} ${color_norm}"  >&2
    exit 2
  fi
  local_ssd_ext_count=$((local_ssd_ext_count+ssdopts[0]))
  if [[ "${local_ssd_ext_count}" -gt "${GCE_MAX_LOCAL_SSD}" || "${local_ssd_ext_count}" -lt 1 ]]; then
    echo -e "${color_red}Local SSD: Total number of local ssds must range from 1 to 8, found: ${local_ssd_ext_count} ${color_norm}" >&2
    exit 2
  fi
}

# Robustly try to create an instance template.
# $1: The name of the instance template.
# $2: The scopes flag.
# $3: String of comma-separated metadata-from-file entries.
# $4: String of comma-separated metadata (key=value) entries.
# $5: the node OS ("linux" or "windows").
function create-node-template() {
  detect-project
  detect-subnetworks
  local template_name="$1"
  local metadata_values="$4"
  local os="$5"
  local machine_type="$6"

  # First, ensure the template doesn't exist.
  # TODO(zmerlynn): To make this really robust, we need to parse the output and
  #                 add retries. Just relying on a non-zero exit code doesn't
  #                 distinguish an ephemeral failed call from a "not-exists".
  if gcloud compute instance-templates describe "${template_name}" --project "${PROJECT}" &>/dev/null; then
    echo "Instance template ${1} already exists; deleting." >&2
    if ! gcloud compute instance-templates delete "${template_name}" --project "${PROJECT}" --quiet &>/dev/null; then
      echo -e "${color_yellow}Failed to delete existing instance template${color_norm}" >&2
      exit 2
    fi
  fi

  local gcloud="gcloud"

  local accelerator_args=()
  # VMs with Accelerators cannot be live migrated.
  # More details here - https://cloud.google.com/compute/docs/gpus/add-gpus#create-new-gpu-instance
  if [[ -n "${NODE_ACCELERATORS}" ]]; then
    accelerator_args+=(--maintenance-policy TERMINATE --restart-on-failure --accelerator "${NODE_ACCELERATORS}")
    gcloud="gcloud beta"
  fi

  local preemptible_minions=()
  if [[ "${PREEMPTIBLE_NODE}" == "true" ]]; then
    preemptible_minions+=(--preemptible --maintenance-policy TERMINATE)
  fi

  local local_ssds=()
  local_ssd_ext_count=0
  if [[ -n "${NODE_LOCAL_SSDS_EXT:-}" ]]; then
    IFS=";" read -r -a ssdgroups <<< "${NODE_LOCAL_SSDS_EXT:-}"
    for ssdgroup in "${ssdgroups[@]}"
    do
      IFS="," read -r -a ssdopts <<< "${ssdgroup}"
      validate-node-local-ssds-ext "${ssdopts[@]}"
      for ((i=1; i<=ssdopts[0]; i++)); do
        local_ssds+=("--local-ssd=interface=${ssdopts[1]}")
      done
    done
  fi

  if [[ -n ${NODE_LOCAL_SSDS+x} ]]; then
    # The NODE_LOCAL_SSDS check below fixes issue #49171
    for ((i=1; i<=NODE_LOCAL_SSDS; i++)); do
      local_ssds+=('--local-ssd=interface=SCSI')
    done
  fi

  local address=""
  if [[ ${GCE_PRIVATE_CLUSTER:-} == "true" ]]; then
    address="no-address"
  fi

  local network
  network=$(make-gcloud-network-argument \
    "${NETWORK_PROJECT}" \
    "${REGION}" \
    "${NETWORK}" \
    "${SUBNETWORK:-}" \
    "${address}" \
    "${ENABLE_IP_ALIASES:-}" \
    "${IP_ALIAS_SIZE:-}")

  local node_image_flags=()
  if [[ "${os}" == 'linux' ]]; then
      node_image_flags+=(--image-project "${NODE_IMAGE_PROJECT}" --image "${NODE_IMAGE}")
  elif [[ "${os}" == 'windows' ]]; then
      node_image_flags+=(--image-project "${WINDOWS_NODE_IMAGE_PROJECT}" --image "${WINDOWS_NODE_IMAGE}")
  else
      echo "Unknown OS ${os}" >&2
      exit 1
  fi

  local metadata_flag="${metadata_values:+--metadata ${metadata_values}}"

  local attempt=1
  while true; do
    echo "Attempt ${attempt} to create ${1}" >&2
    # Deliberately word split ${network}, $2 and ${metadata_flag}
    # shellcheck disable=SC2086
    if ! ${gcloud} compute instance-templates create \
      "${template_name}" \
      --project "${PROJECT}" \
      --machine-type "${machine_type}" \
      --boot-disk-type "${NODE_DISK_TYPE}" \
      --boot-disk-size "${NODE_DISK_SIZE}" \
      "${node_image_flags[@]}" \
      --service-account "${NODE_SERVICE_ACCOUNT}" \
      --tags "${NODE_TAG}" \
      "${accelerator_args[@]}" \
      "${local_ssds[@]}" \
      --region "${REGION}" \
      ${network} \
      "${preemptible_minions[@]}" \
      $2 \
      --metadata-from-file "$3" \
      ${metadata_flag} >&2; then
        if (( attempt > 5 )); then
          echo -e "${color_red}Failed to create instance template ${template_name} ${color_norm}" >&2
          exit 2
        fi
        echo -e "${color_yellow}Attempt ${attempt} failed to create instance template ${template_name}. Retrying.${color_norm}" >&2
        attempt=$((attempt + 1))
        sleep $((attempt * 5))

        # In case the previous attempt failed with something like a
        # Backend Error and left the entry laying around, delete it
        # before we try again.
        gcloud compute instance-templates delete "${template_name}" --project "${PROJECT}" &>/dev/null || true
    else
        break
    fi
  done
}

# Instantiate a kubernetes cluster
#
# Assumed vars
#   KUBE_ROOT
#   <Various vars set in config file>
function kube-up() {
  kube::util::ensure-temp-dir
  detect-project

  load-or-gen-kube-basicauth
  load-or-gen-kube-bearertoken

  # Make sure we have the tar files staged on Google Storage
  find-release-tars
  upload-tars

  # ensure that environmental variables specifying number of migs to create
  set_num_migs

  if [[ ${KUBE_USE_EXISTING_MASTER:-} == "true" ]]; then
    detect-master
    parse-master-env
    create-subnetworks
    detect-subnetworks
    # Windows nodes take longer to boot and setup so create them first.
    create-windows-nodes
    create-linux-nodes
  elif [[ ${KUBE_REPLICATE_EXISTING_MASTER:-} == "true" ]]; then
    detect-master
    if  [[ "${MASTER_OS_DISTRIBUTION}" != "gci" && "${MASTER_OS_DISTRIBUTION}" != "ubuntu" ]]; then
      echo "Master replication supported only for gci and ubuntu"
      return 1
    fi
    if [[ ${GCE_PRIVATE_CLUSTER:-} == "true" ]]; then
      create-internal-loadbalancer
    fi
    create-loadbalancer
    # If replication of master fails, we need to ensure that the replica is removed from etcd clusters.
    if ! replicate-master; then
      remove-replica-from-etcd 2379 true || true
      remove-replica-from-etcd 4002 false || true
    fi
  else
    check-existing
    create-network
    create-subnetworks
    detect-subnetworks
    create-cloud-nat-router
    write-cluster-location
    write-cluster-name
    create-autoscaler-config
    create-master
    create-nodes-firewall
    create-nodes-template
    if [[ "${KUBE_CREATE_NODES}" == "true" ]]; then
      # Windows nodes take longer to boot and setup so create them first.
      create-windows-nodes
      create-linux-nodes
    fi
    check-cluster
  fi
}

function check-existing() {
  local running_in_terminal=false
  # May be false if tty is not allocated (for example with ssh -T).
  if [[ -t 1 ]]; then
    running_in_terminal=true
  fi

  if [[ ${running_in_terminal} == "true" || ${KUBE_UP_AUTOMATIC_CLEANUP} == "true" ]]; then
    if ! check-resources; then
      local run_kube_down="n"
      echo "${KUBE_RESOURCE_FOUND} found." >&2
      # Get user input only if running in terminal.
      if [[ ${running_in_terminal} == "true" && ${KUBE_UP_AUTOMATIC_CLEANUP} == "false" ]]; then
        read -r -p "Would you like to shut down the old cluster (call kube-down)? [y/N] " run_kube_down
      fi
      if [[ ${run_kube_down} == "y" || ${run_kube_down} == "Y" || ${KUBE_UP_AUTOMATIC_CLEANUP} == "true" ]]; then
        echo "... calling kube-down" >&2
        kube-down
      fi
    fi
  fi
}

function check-network-mode() {
  local mode
  mode=$(gcloud compute networks list --filter="name=('${NETWORK}')" --project "${NETWORK_PROJECT}" --format='value(x_gcloud_subnet_mode)' || true)
  # The deprecated field uses lower case. Convert to upper case for consistency.
  echo "$mode" | tr '[:lower:]' '[:upper:]'
}

function create-network() {
  if ! gcloud compute networks --project "${NETWORK_PROJECT}" describe "${NETWORK}" &>/dev/null; then
    # The network needs to be created synchronously or we have a race. The
    # firewalls can be added concurrent with instance creation.
    local network_mode="auto"
    if [[ "${CREATE_CUSTOM_NETWORK:-}" == "true" ]]; then
      network_mode="custom"
    fi
    echo "Creating new ${network_mode} network: ${NETWORK}"
    gcloud compute networks create --project "${NETWORK_PROJECT}" "${NETWORK}" --subnet-mode="${network_mode}"
  else
    PREEXISTING_NETWORK=true
    PREEXISTING_NETWORK_MODE="$(check-network-mode)"
    echo "Found existing network ${NETWORK} in ${PREEXISTING_NETWORK_MODE} mode."
  fi

  if ! gcloud compute firewall-rules --project "${NETWORK_PROJECT}" describe "${CLUSTER_NAME}-default-internal-master" &>/dev/null; then
    gcloud compute firewall-rules create "${CLUSTER_NAME}-default-internal-master" \
      --project "${NETWORK_PROJECT}" \
      --network "${NETWORK}" \
      --source-ranges "10.0.0.0/8" \
      --allow "tcp:1-2379,tcp:2382-65535,udp:1-65535,icmp" \
      --target-tags "${MASTER_TAG}"&
  fi

  if ! gcloud compute firewall-rules --project "${NETWORK_PROJECT}" describe "${CLUSTER_NAME}-default-internal-node" &>/dev/null; then
    gcloud compute firewall-rules create "${CLUSTER_NAME}-default-internal-node" \
      --project "${NETWORK_PROJECT}" \
      --network "${NETWORK}" \
      --source-ranges "10.0.0.0/8" \
      --allow "tcp:1-65535,udp:1-65535,icmp" \
      --target-tags "${NODE_TAG}"&
  fi

  if ! gcloud compute firewall-rules describe --project "${NETWORK_PROJECT}" "${NETWORK}-default-ssh" &>/dev/null; then
    gcloud compute firewall-rules create "${NETWORK}-default-ssh" \
      --project "${NETWORK_PROJECT}" \
      --network "${NETWORK}" \
      --source-ranges "0.0.0.0/0" \
      --allow "tcp:22" &
  fi

  # Open up TCP 3389 to allow RDP connections.
  if [[ ${NUM_WINDOWS_NODES} -gt 0 ]]; then
    if ! gcloud compute firewall-rules describe --project "${NETWORK_PROJECT}" "${NETWORK}-default-rdp" &>/dev/null; then
      gcloud compute firewall-rules create "${NETWORK}-default-rdp" \
        --project "${NETWORK_PROJECT}" \
        --network "${NETWORK}" \
        --source-ranges "0.0.0.0/0" \
        --allow "tcp:3389" &
    fi
  fi

  kube::util::wait-for-jobs || {
    code=$?
    echo -e "${color_red}Failed to create firewall rules.${color_norm}" >&2
    exit $code
  }
}

function expand-default-subnetwork() {
  gcloud compute networks update "${NETWORK}" \
    --switch-to-custom-subnet-mode \
    --project "${NETWORK_PROJECT}" \
    --quiet || true
  gcloud compute networks subnets expand-ip-range "${NETWORK}" \
    --region="${REGION}" \
    --project "${NETWORK_PROJECT}" \
    --prefix-length=19 \
    --quiet
}

function create-subnetworks() {
  case ${ENABLE_IP_ALIASES} in
    true) echo "IP aliases are enabled. Creating subnetworks.";;
    false)
      echo "IP aliases are disabled."
      if [[ "${ENABLE_BIG_CLUSTER_SUBNETS}" = "true" ]]; then
        if [[  "${PREEXISTING_NETWORK}" != "true" ]]; then
          expand-default-subnetwork
        else
          echo "${color_yellow}Using pre-existing network ${NETWORK}, subnets won't be expanded to /19!${color_norm}"
        fi
      elif [[ "${CREATE_CUSTOM_NETWORK:-}" == "true" && "${PREEXISTING_NETWORK}" != "true" ]]; then
          gcloud compute networks subnets create "${SUBNETWORK}" --project "${NETWORK_PROJECT}" --region "${REGION}" --network "${NETWORK}" --range "${NODE_IP_RANGE}"
      fi
      return;;
    *) echo "${color_red}Invalid argument to ENABLE_IP_ALIASES${color_norm}"
       exit 1;;
  esac

  # Look for the alias subnet, it must exist and have a secondary
  # range configured.
  local subnet
  subnet=$(gcloud compute networks subnets describe \
    --project "${NETWORK_PROJECT}" \
    --region "${REGION}" \
    "${IP_ALIAS_SUBNETWORK}" 2>/dev/null || true)
  if [[ -z "${subnet}" ]]; then
    echo "Creating subnet ${NETWORK}:${IP_ALIAS_SUBNETWORK}"
    gcloud compute networks subnets create \
      "${IP_ALIAS_SUBNETWORK}" \
      --description "Automatically generated subnet for ${INSTANCE_PREFIX} cluster. This will be removed on cluster teardown." \
      --project "${NETWORK_PROJECT}" \
      --network "${NETWORK}" \
      --region "${REGION}" \
      --range "${NODE_IP_RANGE}" \
      --secondary-range "pods-default=${CLUSTER_IP_RANGE}" \
      --secondary-range "services-default=${SERVICE_CLUSTER_IP_RANGE}"
    echo "Created subnetwork ${IP_ALIAS_SUBNETWORK}"
  else
    if ! echo "${subnet}" | grep --quiet secondaryIpRanges; then
      echo "${color_red}Subnet ${IP_ALIAS_SUBNETWORK} does not have a secondary range${color_norm}"
      exit 1
    fi
  fi
}

# detect-subnetworks sets the SUBNETWORK var if not already set
# Assumed vars:
#   NETWORK
#   REGION
#   NETWORK_PROJECT
#
# Optional vars:
#   SUBNETWORK
#   IP_ALIAS_SUBNETWORK
function detect-subnetworks() {
  if [[ -n ${SUBNETWORK:-} ]]; then
    echo "Using subnet ${SUBNETWORK}"
    return 0
  fi

  if [[ -n ${IP_ALIAS_SUBNETWORK:-} ]]; then
    SUBNETWORK=${IP_ALIAS_SUBNETWORK}
    echo "Using IP Alias subnet ${SUBNETWORK}"
    return 0
  fi

  SUBNETWORK=$(gcloud compute networks subnets list \
    --network="${NETWORK}" \
    --regions="${REGION}" \
    --project="${NETWORK_PROJECT}" \
    --limit=1 \
    --format='value(name)' 2>/dev/null)

  if [[ -n ${SUBNETWORK:-} ]]; then
    echo "Found subnet for region ${REGION} in network ${NETWORK}: ${SUBNETWORK}"
    return 0
  fi

  echo "${color_red}Could not find subnetwork with region ${REGION}, network ${NETWORK}, and project ${NETWORK_PROJECT}"
}

# Sets up Cloud NAT for the network.
# Assumed vars:
#   NETWORK_PROJECT
#   REGION
#   NETWORK
function create-cloud-nat-router() {
  if [[ ${GCE_PRIVATE_CLUSTER:-} == "true" ]]; then
    if gcloud compute routers describe "$NETWORK-nat-router" --project "$NETWORK_PROJECT" --region "$REGION" &>/dev/null; then
      echo "Cloud nat already exists"
      return 0
    fi
    gcloud compute routers create "$NETWORK-nat-router" \
      --project "$NETWORK_PROJECT" \
      --region "$REGION" \
      --network "$NETWORK"
    gcloud compute routers nats create "$NETWORK-nat-config" \
      --project "$NETWORK_PROJECT" \
      --router-region "$REGION" \
      --router "$NETWORK-nat-router" \
      --nat-primary-subnet-ip-ranges \
      --auto-allocate-nat-external-ips \
      ${GCE_PRIVATE_CLUSTER_PORTS_PER_VM:+--min-ports-per-vm ${GCE_PRIVATE_CLUSTER_PORTS_PER_VM}}
  fi
}

function delete-all-firewall-rules() {
  local -a fws
  kube::util::read-array fws < <(gcloud compute firewall-rules list --project "${NETWORK_PROJECT}" --filter="network=${NETWORK}" --format="value(name)")
  if (( "${#fws[@]}" > 0 )); then
    echo "Deleting firewall rules remaining in network ${NETWORK}: ${fws[*]}"
    delete-firewall-rules "${fws[@]}"
  else
    echo "No firewall rules in network ${NETWORK}"
  fi
}

# Ignores firewall rule arguments that do not exist in NETWORK_PROJECT.
function delete-firewall-rules() {
  for fw in "$@"; do
    if [[ -n $(gcloud compute firewall-rules --project "${NETWORK_PROJECT}" describe "${fw}" --format='value(name)' 2>/dev/null || true) ]]; then
      gcloud compute firewall-rules delete --project "${NETWORK_PROJECT}" --quiet "${fw}" &
    fi
  done
  kube::util::wait-for-jobs || {
    echo -e "${color_red}Failed to delete firewall rules.${color_norm}" >&2
  }
}

function delete-network() {
  if [[ -n $(gcloud compute networks --project "${NETWORK_PROJECT}" describe "${NETWORK}" --format='value(name)' 2>/dev/null || true) ]]; then
    if ! gcloud compute networks delete --project "${NETWORK_PROJECT}" --quiet "${NETWORK}"; then
      echo "Failed to delete network '${NETWORK}'. Listing firewall-rules:"
      gcloud compute firewall-rules --project "${NETWORK_PROJECT}" list --filter="network=${NETWORK}"
      return 1
    fi
  fi
}

function delete-cloud-nat-router() {
  if [[ ${GCE_PRIVATE_CLUSTER:-} == "true" ]]; then
    if [[ -n $(gcloud compute routers describe --project "${NETWORK_PROJECT}" --region "${REGION}" "${NETWORK}-nat-router" --format='value(name)' 2>/dev/null || true) ]]; then
      echo "Deleting Cloud NAT router..."
      gcloud compute routers delete --project "${NETWORK_PROJECT}" --region "${REGION}" --quiet "${NETWORK}-nat-router"
    fi
  fi
}

function delete-subnetworks() {
  # If running in custom mode network we need to delete subnets manually.
  mode="$(check-network-mode)"
  if [[ "${mode}" == "CUSTOM" ]]; then
    if [[ "${ENABLE_BIG_CLUSTER_SUBNETS}" = "true" ]]; then
      echo "Deleting default subnets..."
      # This value should be kept in sync with number of regions.
      local parallelism=9
      gcloud compute networks subnets list --network="${NETWORK}" --project "${NETWORK_PROJECT}" --format='value(region.basename())' | \
        xargs -I {} -P ${parallelism} gcloud --quiet compute networks subnets delete "${NETWORK}" --project "${NETWORK_PROJECT}" --region="{}" || true
    elif [[ "${CREATE_CUSTOM_NETWORK:-}" == "true" ]]; then
      echo "Deleting custom subnet..."
      gcloud --quiet compute networks subnets delete "${SUBNETWORK}" --project "${NETWORK_PROJECT}" --region="${REGION}" || true
    fi
    return
  fi

  # If we reached here, it means we're not using custom network.
  # So the only thing we need to check is if IP-aliases was turned
  # on and we created a subnet for it. If so, we should delete it.
  if [[ ${ENABLE_IP_ALIASES:-} == "true" ]]; then
    # Only delete the subnet if we created it (i.e it's not pre-existing).
    if [[ -z "${KUBE_GCE_IP_ALIAS_SUBNETWORK:-}" ]]; then
      echo "Removing auto-created subnet ${NETWORK}:${IP_ALIAS_SUBNETWORK}"
      if [[ -n $(gcloud compute networks subnets describe \
            --project "${NETWORK_PROJECT}" \
            --region "${REGION}" \
            "${IP_ALIAS_SUBNETWORK}" 2>/dev/null) ]]; then
        gcloud --quiet compute networks subnets delete \
          --project "${NETWORK_PROJECT}" \
          --region "${REGION}" \
          "${IP_ALIAS_SUBNETWORK}"
      fi
    fi
  fi
}

# Generates SSL certificates for etcd cluster peer to peer communication. Uses cfssl program.
#
# Assumed vars:
#   KUBE_TEMP: temporary directory
#
# Args:
#  $1: host name
#  $2: CA certificate
#  $3: CA key
#
# If CA cert/key is empty, the function will also generate certs for CA.
#
# Vars set:
#   ETCD_CA_KEY_BASE64
#   ETCD_CA_CERT_BASE64
#   ETCD_PEER_KEY_BASE64
#   ETCD_PEER_CERT_BASE64
#
function create-etcd-certs {
  local host=${1}
  local ca_cert=${2:-}
  local ca_key=${3:-}

  GEN_ETCD_CA_CERT="${ca_cert}" GEN_ETCD_CA_KEY="${ca_key}" \
    generate-etcd-cert "${KUBE_TEMP}/cfssl" "${host}" "peer" "peer"

  pushd "${KUBE_TEMP}/cfssl"
  ETCD_CA_KEY_BASE64=$(base64 $BASE64_INPUT_FLAG "ca-key.pem" | tr -d '\r\n')
  ETCD_CA_CERT_BASE64=$(gzip -c "ca.pem" | base64 | tr -d '\r\n')
  ETCD_PEER_KEY_BASE64=$(base64 $BASE64_INPUT_FLAG "peer-key.pem" | tr -d '\r\n')
  ETCD_PEER_CERT_BASE64=$(gzip -c "peer.pem" | base64 | tr -d '\r\n')
  popd
}

# Generates SSL certificates for etcd-client and kube-apiserver communication. Uses cfssl program.
#
# Assumed vars:
#   KUBE_TEMP: temporary directory
#
# Args:
#  $1: host server name
#  $2: host client name
#  $3: CA certificate
#  $4: CA key
#
# If CA cert/key is empty, the function will also generate certs for CA.
#
# Vars set:
#   ETCD_APISERVER_CA_KEY_BASE64
#   ETCD_APISERVER_CA_CERT_BASE64
#   ETCD_APISERVER_SERVER_KEY_BASE64
#   ETCD_APISERVER_SERVER_CERT_BASE64
#   ETCD_APISERVER_CLIENT_KEY_BASE64
#   ETCD_APISERVER_CLIENT_CERT_BASE64
#
function create-etcd-apiserver-certs {
  local hostServer=${1}
  local hostClient=${2}
  local etcd_apiserver_ca_cert=${3:-}
  local etcd_apiserver_ca_key=${4:-}

  GEN_ETCD_CA_CERT="${etcd_apiserver_ca_cert}" GEN_ETCD_CA_KEY="${etcd_apiserver_ca_key}" \
    generate-etcd-cert "${KUBE_TEMP}/cfssl" "${hostServer}" "server" "etcd-apiserver-server"
    generate-etcd-cert "${KUBE_TEMP}/cfssl" "${hostClient}" "client" "etcd-apiserver-client"

  pushd "${KUBE_TEMP}/cfssl"
  ETCD_APISERVER_CA_KEY_BASE64=$(base64 $BASE64_INPUT_FLAG "ca-key.pem" | tr -d '\r\n')
  ETCD_APISERVER_CA_CERT_BASE64=$(gzip -c "ca.pem" | base64 | tr -d '\r\n')
  ETCD_APISERVER_SERVER_KEY_BASE64=$(base64 $BASE64_INPUT_FLAG "etcd-apiserver-server-key.pem" | tr -d '\r\n')
  ETCD_APISERVER_SERVER_CERT_BASE64=$(gzip -c "etcd-apiserver-server.pem" | base64 | tr -d '\r\n')
  ETCD_APISERVER_CLIENT_KEY_BASE64=$(base64 $BASE64_INPUT_FLAG "etcd-apiserver-client-key.pem" | tr -d '\r\n')
  ETCD_APISERVER_CLIENT_CERT_BASE64=$(gzip -c "etcd-apiserver-client.pem" | base64 | tr -d '\r\n')
  popd
}


function create-master() {
  echo "Starting master and configuring firewalls"
  gcloud compute firewall-rules create "${MASTER_NAME}-https" \
    --project "${NETWORK_PROJECT}" \
    --network "${NETWORK}" \
    --target-tags "${MASTER_TAG}" \
    --allow tcp:443 &

  echo "Configuring firewall for apiserver konnectivity server"
  if [[ "${PREPARE_KONNECTIVITY_SERVICE:-false}" == "true" ]]; then
    gcloud compute firewall-rules create "${MASTER_NAME}-konnectivity-server" \
      --project "${NETWORK_PROJECT}" \
      --network "${NETWORK}" \
      --target-tags "${MASTER_TAG}" \
      --allow tcp:8132 &
  fi

  # We have to make sure the disk is created before creating the master VM, so
  # run this in the foreground.
  gcloud compute disks create "${MASTER_NAME}-pd" \
    --project "${PROJECT}" \
    --zone "${ZONE}" \
    --type "${MASTER_DISK_TYPE}" \
    --size "${MASTER_DISK_SIZE}"

  # Create rule for accessing and securing etcd servers.
  if ! gcloud compute firewall-rules --project "${NETWORK_PROJECT}" describe "${MASTER_NAME}-etcd" &>/dev/null; then
    gcloud compute firewall-rules create "${MASTER_NAME}-etcd" \
      --project "${NETWORK_PROJECT}" \
      --network "${NETWORK}" \
      --source-tags "${MASTER_TAG}" \
      --allow "tcp:2380,tcp:2381" \
      --target-tags "${MASTER_TAG}" &
  fi

  # Generate a bearer token for this cluster. We push this separately
  # from the other cluster variables so that the client (this
  # computer) can forget it later. This should disappear with
  # http://issue.k8s.io/3168
  KUBE_PROXY_TOKEN=$(dd if=/dev/urandom bs=128 count=1 2>/dev/null | base64 | tr -d "=+/" | dd bs=32 count=1 2>/dev/null)
  if [[ "${ENABLE_NODE_PROBLEM_DETECTOR:-}" == "standalone" ]]; then
    NODE_PROBLEM_DETECTOR_TOKEN=$(dd if=/dev/urandom bs=128 count=1 2>/dev/null | base64 | tr -d "=+/" | dd bs=32 count=1 2>/dev/null)
  fi

  # Reserve the master's IP so that it can later be transferred to another VM
  # without disrupting the kubelets.
  create-static-ip "${MASTER_NAME}-ip" "${REGION}"
  MASTER_RESERVED_IP=$(gcloud compute addresses describe "${MASTER_NAME}-ip" \
    --project "${PROJECT}" --region "${REGION}" -q --format='value(address)')

  if [[ "${REGISTER_MASTER_KUBELET:-}" == "true" ]]; then
    KUBELET_APISERVER="${MASTER_RESERVED_IP}"
  fi

  KUBERNETES_MASTER_NAME="${MASTER_RESERVED_IP}"
  MASTER_ADVERTISE_ADDRESS="${MASTER_RESERVED_IP}"

  MASTER_INTERNAL_IP=""
  if [[ ${GCE_PRIVATE_CLUSTER:-} == "true" ]]; then
    gcloud compute addresses create "${MASTER_NAME}-internal-ip" --project "${PROJECT}" --region "$REGION" --subnet "$SUBNETWORK"
    MASTER_INTERNAL_IP=$(gcloud compute addresses describe "${MASTER_NAME}-internal-ip" --project "${PROJECT}" --region "${REGION}" -q --format='value(address)')
    echo "Master internal ip is: $MASTER_INTERNAL_IP"
    KUBERNETES_MASTER_NAME="${MASTER_INTERNAL_IP}"
    MASTER_ADVERTISE_ADDRESS="${MASTER_INTERNAL_IP}"
  fi

  create-certs "${MASTER_RESERVED_IP}" "${MASTER_INTERNAL_IP}"
  create-etcd-certs "${MASTER_NAME}"
  create-etcd-apiserver-certs "etcd-${MASTER_NAME}" "${MASTER_NAME}"

  if [[ "$(get-num-nodes)" -ge "50" ]]; then
    # We block on master creation for large clusters to avoid doing too much
    # unnecessary work in case master start-up fails (like creation of nodes).
    create-master-instance "${MASTER_RESERVED_IP}" "${MASTER_INTERNAL_IP}"
  else
    create-master-instance "${MASTER_RESERVED_IP}" "${MASTER_INTERNAL_IP}" &
  fi

}

# Adds master replica to etcd cluster.
#
# Assumed vars:
#   REPLICA_NAME
#   PROJECT
#   EXISTING_MASTER_NAME
#   EXISTING_MASTER_ZONE
#
# $1: etcd client port
# $2: etcd internal port
# $3: whether etcd communication should use mtls
# returns the result of ssh command which adds replica
function add-replica-to-etcd() {
  local -r client_port="${1}"
  local -r internal_port="${2}"
  local -r use_mtls="${3}"

  TLSARG=""
  PROTO="http://"
  if [[ "${use_mtls}" == "true" ]]; then
    # Keep in sync with ETCD_APISERVER_CA_CERT_PATH, ETCD_APISERVER_CLIENT_CERT_PATH and ETCD_APISERVER_CLIENT_KEY_PATH in configure-helper.sh.
    TLSARG="--cacert /etc/srv/kubernetes/pki/etcd-apiserver-ca.crt --cert /etc/srv/kubernetes/pki/etcd-apiserver-client.crt --key /etc/srv/kubernetes/pki/etcd-apiserver-client.key"
    PROTO="https://"
  fi
  run-gcloud-command "${EXISTING_MASTER_NAME}" "${EXISTING_MASTER_ZONE}" "curl ${TLSARG} ${PROTO}127.0.0.1:${client_port}/v2/members -XPOST -H \"Content-Type: application/json\" -d '{\"peerURLs\":[\"https://${REPLICA_NAME}:${internal_port}\"]}' -s"
  return $?
}

# Sets EXISTING_MASTER_NAME and EXISTING_MASTER_ZONE variables.
#
# Assumed vars:
#   PROJECT
#
# NOTE: Must be in sync with get-replica-name-regexp
function set-existing-master() {
  local existing_master
  existing_master=$(gcloud compute instances list \
    --project "${PROJECT}" \
    --filter "name ~ '$(get-replica-name-regexp)'" \
    --format "value(name,zone)" | head -n1)
  EXISTING_MASTER_NAME="$(echo "${existing_master}" | cut -f1)"
  EXISTING_MASTER_ZONE="$(echo "${existing_master}" | cut -f2)"
}

function replicate-master() {
  set-replica-name
  set-existing-master

  echo "Experimental: replicating existing master ${EXISTING_MASTER_ZONE}/${EXISTING_MASTER_NAME} as ${ZONE}/${REPLICA_NAME}"

  # Before we do anything else, we should configure etcd to expect more replicas.
  if ! add-replica-to-etcd 2379 2380 true; then
    echo "Failed to add master replica to etcd cluster."
    return 1
  fi
  if ! add-replica-to-etcd 4002 2381 false; then
    echo "Failed to add master replica to etcd events cluster."
    return 1
  fi

  # We have to make sure the disk is created before creating the master VM, so
  # run this in the foreground.
  gcloud compute disks create "${REPLICA_NAME}-pd" \
    --project "${PROJECT}" \
    --zone "${ZONE}" \
    --type "${MASTER_DISK_TYPE}" \
    --size "${MASTER_DISK_SIZE}"

  local existing_master_replicas
  existing_master_replicas="$(get-all-replica-names)"
  replicate-master-instance "${EXISTING_MASTER_ZONE}" "${EXISTING_MASTER_NAME}" "${existing_master_replicas}"

  # Add new replica to the load balancer.
  gcloud compute target-pools add-instances "${MASTER_NAME}" \
    --project "${PROJECT}" \
    --zone "${ZONE}" \
    --instances "${REPLICA_NAME}"

  if [[ "${GCE_PRIVATE_CLUSTER:-}" == "true" ]]; then
    add-to-internal-loadbalancer "${REPLICA_NAME}" "${ZONE}"
  fi
}

# Detaches old and ataches new external IP to a VM.
#
# Arguments:
#   $1 - VM name
#   $2 - VM zone
#   $3 - external static IP; if empty will use an ephemeral IP address.
function attach-external-ip() {
  local NAME=${1}
  local ZONE=${2}
  local IP_ADDR=${3:-}
  local ACCESS_CONFIG_NAME
  ACCESS_CONFIG_NAME=$(gcloud compute instances describe "${NAME}" \
    --project "${PROJECT}" --zone "${ZONE}" \
    --format="value(networkInterfaces[0].accessConfigs[0].name)")
  gcloud compute instances delete-access-config "${NAME}" \
    --project "${PROJECT}" --zone "${ZONE}" \
    --access-config-name "${ACCESS_CONFIG_NAME}"
  if [[ -z "${IP_ADDR}" ]]; then
    gcloud compute instances add-access-config "${NAME}" \
      --project "${PROJECT}" --zone "${ZONE}" \
      --access-config-name "${ACCESS_CONFIG_NAME}"
  else
    gcloud compute instances add-access-config "${NAME}" \
      --project "${PROJECT}" --zone "${ZONE}" \
      --access-config-name "${ACCESS_CONFIG_NAME}" \
      --address "${IP_ADDR}"
  fi
}

# Creates load balancer in front of apiserver if it doesn't exists already. Assumes there's only one
# existing master replica.
#
# Assumes:
#   PROJECT
#   MASTER_NAME
#   ZONE
#   REGION
function create-loadbalancer() {
  # Step 0: Return early if LB is already configured.
  if gcloud compute forwarding-rules describe "${MASTER_NAME}" \
    --project "${PROJECT}" --region "${REGION}" > /dev/null 2>&1; then
    echo "Load balancer already exists"
    return
  fi

  local EXISTING_MASTER_NAME
  local EXISTING_MASTER_ZONE
  EXISTING_MASTER_NAME="$(get-all-replica-names)"
  EXISTING_MASTER_ZONE=$(gcloud compute instances list "${EXISTING_MASTER_NAME}" \
    --project "${PROJECT}" --format='value(zone)')

  echo "Creating load balancer in front of an already existing master in ${EXISTING_MASTER_ZONE}"

  # Step 1: Detach master IP address and attach ephemeral address to the existing master
  attach-external-ip "${EXISTING_MASTER_NAME}" "${EXISTING_MASTER_ZONE}"

  # Step 2: Create target pool.
  gcloud compute target-pools create "${MASTER_NAME}" --project "${PROJECT}" --region "${REGION}"
  # TODO: We should also add master instances with suffixes
  gcloud compute target-pools add-instances "${MASTER_NAME}" --instances "${EXISTING_MASTER_NAME}" --project "${PROJECT}" --zone "${EXISTING_MASTER_ZONE}"

  # Step 3: Create forwarding rule.
  # TODO: This step can take up to 20 min. We need to speed this up...
  gcloud compute forwarding-rules create "${MASTER_NAME}" \
    --project "${PROJECT}" --region "${REGION}" \
    --target-pool "${MASTER_NAME}" --address="${KUBE_MASTER_IP}" --ports=443

  echo -n "Waiting for the load balancer configuration to propagate..."
  local counter=0
  until curl -k -m1 "https://${KUBE_MASTER_IP}" &> /dev/null; do
    counter=$((counter+1))
    echo -n .
    if [[ ${counter} -ge 1800 ]]; then
      echo -e "${color_red}TIMEOUT${color_norm}" >&2
      echo -e "${color_red}Load balancer failed to initialize within ${counter} seconds.${color_norm}" >&2
      exit 2
    fi
  done
  echo "DONE"
}


# attach-internal-master-ip attach internal ip to existing master.
#
# Assumes:
# * PROJECT
function attach-internal-master-ip() {
  local name="${1}"
  local zone="${2}"
  local ip="${3}"

  local aliases
  aliases=$(gcloud compute instances describe "${name}" --project "${PROJECT}" --zone "${zone}" --flatten='networkInterfaces[0].aliasIpRanges[]' --format='value[separator=':'](networkInterfaces[0].aliasIpRanges.subnetworkRangeName,networkInterfaces[0].aliasIpRanges.ipCidrRange)' | sed 's/^://' | paste -s -d';' -)
  aliases="${aliases:+${aliases};}${ip}/32"
  echo "Setting ${name}'s aliases to '${aliases}' (added ${ip})"
  # Attach ${ip} to ${name}
  gcloud compute instances network-interfaces update "${name}" --project "${PROJECT}" --zone "${zone}" --aliases="${aliases}"
  gcloud compute instances add-metadata "${name}" --zone "${zone}" --metadata=kube-master-internal-ip="${ip}"
  run-gcloud-command "${name}" "${zone}" 'sudo /bin/bash /home/kubernetes/bin/kube-master-internal-route.sh' || true
  return $?
}


# detach-internal-master-ip detaches internal ip from existing master.
#
# Assumes:
# * PROJECT
function detach-internal-master-ip() {
  local name="${1}"
  local zone="${2}"
  local ip="${3}"

  local aliases
  aliases=$(gcloud compute instances describe "${name}" --project "${PROJECT}" --zone "${zone}" --flatten='networkInterfaces[0].aliasIpRanges[]' --format='value[separator=':'](networkInterfaces[0].aliasIpRanges.subnetworkRangeName,networkInterfaces[0].aliasIpRanges.ipCidrRange)' | sed 's/^://' | grep -v "${ip}" | paste -s -d';' -)
  echo "Setting ${name}'s aliases to '${aliases}' (removed ${ip})"
  # Detach ${MASTER_NAME}-internal-ip from ${name}
  gcloud compute instances network-interfaces update "${name}" --project "${PROJECT}" --zone "${zone}" --aliases="${aliases}"
  gcloud compute instances remove-metadata "${name}" --zone "${zone}" --keys=kube-master-internal-ip
  # We want `ip route` to be run in the cloud and not this host
  run-gcloud-command "${name}" "${zone}" "sudo ip route del to local ${ip}/32 dev \$(ip route | grep default | while read -r _ _ _ _ dev _; do echo \$dev; done)" || true
  return $?
}

# create-internal-loadbalancer creates an internal load balacer in front of existing master.
#
# Assumes:
# * MASTER_NAME
# * PROJECT
# * REGION
function create-internal-loadbalancer() {
  if gcloud compute forwarding-rules describe "${MASTER_NAME}-internal" \
    --project "${PROJECT}" --region "${REGION}" > /dev/null 2>&1; then
    echo "Load balancer already exists"
    return
  fi

  local EXISTING_MASTER_NAME
  local EXISTING_MASTER_ZONE
  EXISTING_MASTER_NAME="$(get-all-replica-names)"
  EXISTING_MASTER_ZONE=$(gcloud compute instances list "${EXISTING_MASTER_NAME}" \
    --project "${PROJECT}" --format='value(zone)')

  echo "Detaching ${KUBE_MASTER_INTERNAL_IP} from ${EXISTING_MASTER_NAME}/${EXISTING_MASTER_ZONE}"
  detach-internal-master-ip "${EXISTING_MASTER_NAME}" "${EXISTING_MASTER_ZONE}" "${KUBE_MASTER_INTERNAL_IP}"

  echo "Creating internal load balancer with IP: ${KUBE_MASTER_INTERNAL_IP}"
  gcloud compute health-checks --project "${PROJECT}" create tcp "${MASTER_NAME}-hc" --port=443

  gcloud compute backend-services create "${MASTER_NAME}" \
    --project "${PROJECT}" \
    --region "${REGION}" \
    --protocol tcp \
    --region "${REGION}" \
    --load-balancing-scheme internal \
    --health-checks "${MASTER_NAME}-hc"

  gcloud compute forwarding-rules create "${MASTER_NAME}-internal" \
    --project "${PROJECT}" \
    --region "${REGION}" \
    --load-balancing-scheme internal \
    --network "${NETWORK}" \
    --subnet "${SUBNETWORK}" \
    --address "${KUBE_MASTER_INTERNAL_IP}" \
    --ip-protocol TCP \
    --ports 443 \
    --backend-service "${MASTER_NAME}" \
    --backend-service-region "${REGION}"

  echo "Adding ${EXISTING_MASTER_NAME}/${EXISTING_MASTER_ZONE} to the load balancer"
  add-to-internal-loadbalancer "${EXISTING_MASTER_NAME}" "${EXISTING_MASTER_ZONE}"
}

# add-to-internal-loadbalancer adds an instance to ILB.
# Assumes:
# * MASTER_NAME
# * PROJECT
# * REGION
function add-to-internal-loadbalancer() {
  local name="${1}"
  local zone="${2}"

  gcloud compute instance-groups unmanaged create "${name}" --project "${PROJECT}" --zone "${zone}"
  gcloud compute instance-groups unmanaged add-instances "${name}" --project "${PROJECT}" --zone "${zone}" --instances "${name}"
  gcloud compute backend-services add-backend "${MASTER_NAME}" \
    --project "${PROJECT}" \
    --region "${REGION}" \
    --instance-group "${name}" \
    --instance-group-zone "${zone}"
}

# remove-from-internal-loadbalancer removes an instance from ILB.
# Assumes:
# * MASTER_NAME
# * PROJECT
# * REGION
function remove-from-internal-loadbalancer() {
  local name="${1}"
  local zone="${2}"

  if gcloud compute instance-groups unmanaged describe "${name}" --project "${PROJECT}" --zone "${zone}" &>/dev/null; then
    gcloud compute backend-services remove-backend "${MASTER_NAME}" \
          --project "${PROJECT}" \
          --region "${REGION}" \
          --instance-group "${name}" \
          --instance-group-zone "${zone}"
    gcloud compute instance-groups unmanaged delete "${name}" --project "${PROJECT}" --zone "${zone}" --quiet
  fi
}

function delete-internal-loadbalancer() {
  if gcloud compute forwarding-rules describe "${MASTER_NAME}-internal" --project "${PROJECT}" --region "${REGION}" &>/dev/null; then
    gcloud compute forwarding-rules delete "${MASTER_NAME}-internal" --project "${PROJECT}" --region "${REGION}" --quiet
  fi

  if gcloud compute backend-services describe "${MASTER_NAME}" --project "${PROJECT}" --region "${REGION}" &>/dev/null; then
    gcloud compute backend-services delete "${MASTER_NAME}" --project "${PROJECT}" --region "${REGION}" --quiet
  fi
  if gcloud compute health-checks describe "${MASTER_NAME}-gc" --project "${PROJECT}" &>/dev/null; then
    gcloud compute health-checks delete "${MASTER_NAME}-gc" --project "${PROJECT}" --quiet
  fi
}

function create-nodes-firewall() {
  # Create a single firewall rule for all minions.
  create-firewall-rule "${NODE_TAG}-all" "${CLUSTER_IP_RANGE}" "${NODE_TAG}" &

  # Report logging choice (if any).
  if [[ "${ENABLE_NODE_LOGGING-}" == "true" ]]; then
    echo "+++ Logging using Fluentd to ${LOGGING_DESTINATION:-unknown}"
  fi

  # Wait for last batch of jobs
  kube::util::wait-for-jobs || {
    code=$?
    echo -e "${color_red}Failed to create firewall rule.${color_norm}" >&2
    exit $code
  }
}

function get-scope-flags() {
  local scope_flags=
  if [[ -n "${NODE_SCOPES}" ]]; then
    scope_flags="--scopes ${NODE_SCOPES}"
  else
    scope_flags="--no-scopes"
  fi
  echo "${scope_flags}"
}

function create-nodes-template() {
  echo "Creating nodes."

  local scope_flags
  scope_flags=$(get-scope-flags)

  write-linux-node-env
  write-windows-node-env

  # NOTE: these template names and their format must match
  # create-[linux,windows]-nodes() as well as get-template()!
  local linux_template_name="${NODE_INSTANCE_PREFIX}-template"
  local windows_template_name="${WINDOWS_NODE_INSTANCE_PREFIX}-template"
  create-linux-node-instance-template "$linux_template_name"
  create-windows-node-instance-template "$windows_template_name" "${scope_flags[*]}"
  if [[ -n "${ADDITIONAL_MACHINE_TYPE:-}" ]]; then
    local linux_extra_template_name="${NODE_INSTANCE_PREFIX}-extra-template"
    create-linux-node-instance-template "$linux_extra_template_name" "${ADDITIONAL_MACHINE_TYPE}"
  fi
}

# Assumes:
# - MAX_INSTANCES_PER_MIG
# - NUM_NODES
# - NUM_WINDOWS_NODES
# exports:
# - NUM_MIGS
# - NUM_WINDOWS_MIGS
function set_num_migs() {
  local defaulted_max_instances_per_mig=${MAX_INSTANCES_PER_MIG:-1000}

  if [[ ${defaulted_max_instances_per_mig} -le "0" ]]; then
    echo "MAX_INSTANCES_PER_MIG cannot be negative. Assuming default 1000"
    defaulted_max_instances_per_mig=1000
  fi
  export NUM_MIGS=$(((NUM_NODES + defaulted_max_instances_per_mig - 1) / defaulted_max_instances_per_mig))
  export NUM_WINDOWS_MIGS=$(((NUM_WINDOWS_NODES + defaulted_max_instances_per_mig - 1) / defaulted_max_instances_per_mig))
}

# Assumes:
# - NUM_MIGS
# - NODE_INSTANCE_PREFIX
# - NUM_NODES
# - PROJECT
# - ZONE
function create-linux-nodes() {
  local template_name="${NODE_INSTANCE_PREFIX}-template"
  local extra_template_name="${NODE_INSTANCE_PREFIX}-extra-template"

  local nodes="${NUM_NODES}"
  if [[ -n "${HEAPSTER_MACHINE_TYPE:-}" ]]; then
    echo "Creating a special node for heapster with machine-type ${HEAPSTER_MACHINE_TYPE}"
    create-heapster-node
    nodes=$(( nodes - 1 ))
  fi

  if [[ -n "${ADDITIONAL_MACHINE_TYPE:-}" && "${NUM_ADDITIONAL_NODES:-}" -gt 0 ]]; then
    local num_additional="${NUM_ADDITIONAL_NODES}"
    if [[ "${NUM_ADDITIONAL_NODES:-}" -gt "${nodes}" ]]; then
      echo "Capping NUM_ADDITIONAL_NODES to ${nodes}"
      num_additional="${nodes}"
    fi
    if [[ "${num_additional:-}" -gt 0 ]]; then
      echo "Creating ${num_additional} special nodes with machine-type ${ADDITIONAL_MACHINE_TYPE}"
      local extra_group_name="${NODE_INSTANCE_PREFIX}-extra"
      gcloud compute instance-groups managed \
          create "${extra_group_name}" \
          --project "${PROJECT}" \
          --zone "${ZONE}" \
          --base-instance-name "${extra_group_name}" \
          --size "${num_additional}" \
          --template "${extra_template_name}" || true;
      gcloud compute instance-groups managed wait-until --stable \
          "${extra_group_name}" \
          --zone "${ZONE}" \
          --project "${PROJECT}" \
          --timeout "${MIG_WAIT_UNTIL_STABLE_TIMEOUT}" || true
      nodes=$(( nodes - num_additional ))
    fi
  fi

  local instances_left=${nodes}

  for ((i=1; i<=NUM_MIGS; i++)); do
    local group_name="${NODE_INSTANCE_PREFIX}-group-$i"
    if [[ $i -eq ${NUM_MIGS} ]]; then
      # TODO: We don't add a suffix for the last group to keep backward compatibility when there's only one MIG.
      # We should change it at some point, but note #18545 when changing this.
      group_name="${NODE_INSTANCE_PREFIX}-group"
    fi
    # Spread the remaining number of nodes evenly
    this_mig_size=$((instances_left / (NUM_MIGS - i + 1)))
    instances_left=$((instances_left - this_mig_size))

    # Run instance-groups creation in parallel.
    {
      gcloud compute instance-groups managed \
          create "${group_name}" \
          --project "${PROJECT}" \
          --zone "${ZONE}" \
          --base-instance-name "${group_name}" \
          --size "${this_mig_size}" \
          --template "${template_name}" || true;
      gcloud compute instance-groups managed wait-until --stable \
          "${group_name}" \
          --zone "${ZONE}" \
          --project "${PROJECT}" \
          --timeout "${MIG_WAIT_UNTIL_STABLE_TIMEOUT}" || true
    } &
  done
  wait
}

# Assumes:
# - NUM_WINDOWS_MIGS
# - WINDOWS_NODE_INSTANCE_PREFIX
# - NUM_WINDOWS_NODES
# - PROJECT
# - ZONE
function create-windows-nodes() {
  local template_name="${WINDOWS_NODE_INSTANCE_PREFIX}-template"

  local -r nodes="${NUM_WINDOWS_NODES}"
  local instances_left=${nodes}

  for ((i=1; i <= NUM_WINDOWS_MIGS; i++)); do
    local group_name="${WINDOWS_NODE_INSTANCE_PREFIX}-group-$i"
    if [[ $i -eq ${NUM_WINDOWS_MIGS} ]]; then
      # TODO: We don't add a suffix for the last group to keep backward compatibility when there's only one MIG.
      # We should change it at some point, but note #18545 when changing this.
      group_name="${WINDOWS_NODE_INSTANCE_PREFIX}-group"
    fi
    # Spread the remaining number of nodes evenly
    this_mig_size=$((instances_left / (NUM_WINDOWS_MIGS - i + 1)))
    instances_left=$((instances_left - this_mig_size))

    gcloud compute instance-groups managed \
        create "${group_name}" \
        --project "${PROJECT}" \
        --zone "${ZONE}" \
        --base-instance-name "${group_name}" \
        --size "${this_mig_size}" \
        --template "${template_name}" || true;
    gcloud compute instance-groups managed wait-until --stable \
        "${group_name}" \
        --zone "${ZONE}" \
        --project "${PROJECT}" \
        --timeout "${MIG_WAIT_UNTIL_STABLE_TIMEOUT}" || true;
  done
}

# Assumes:
# - NODE_INSTANCE_PREFIX
# - PROJECT
# - NETWORK_PROJECT
# - REGION
# - ZONE
# - HEAPSTER_MACHINE_TYPE
# - NODE_DISK_TYPE
# - NODE_DISK_SIZE
# - NODE_IMAGE_PROJECT
# - NODE_IMAGE
# - NODE_SERVICE_ACCOUNT
# - NODE_TAG
# - NETWORK
# - ENABLE_IP_ALIASES
# - SUBNETWORK
# - IP_ALIAS_SIZE
function create-heapster-node() {
  local gcloud="gcloud"

  local network
  network=$(make-gcloud-network-argument \
      "${NETWORK_PROJECT}" \
      "${REGION}" \
      "${NETWORK}" \
      "${SUBNETWORK:-}" \
      "" \
      "${ENABLE_IP_ALIASES:-}" \
      "${IP_ALIAS_SIZE:-}")

  # Deliberately word split ${network} and $(get-scope-flags)
  # shellcheck disable=SC2086 disable=SC2046
  ${gcloud} compute instances \
      create "${NODE_INSTANCE_PREFIX}-heapster" \
      --project "${PROJECT}" \
      --zone "${ZONE}" \
      --machine-type="${HEAPSTER_MACHINE_TYPE}" \
      --boot-disk-type "${NODE_DISK_TYPE}" \
      --boot-disk-size "${NODE_DISK_SIZE}" \
      --image-project="${NODE_IMAGE_PROJECT}" \
      --image "${NODE_IMAGE}" \
      --service-account "${NODE_SERVICE_ACCOUNT}" \
      --tags "${NODE_TAG}" \
      ${network} \
      $(get-scope-flags) \
      --metadata-from-file "$(get-node-instance-metadata-from-file "heapster-kube-env")"
}

# Assumes:
# - NUM_MIGS
# - NODE_INSTANCE_PREFIX
# - PROJECT
# - ZONE
# - AUTOSCALER_MAX_NODES
# - AUTOSCALER_MIN_NODES
# Exports
# - AUTOSCALER_MIG_CONFIG
function create-cluster-autoscaler-mig-config() {

  # Each MIG must have at least one node, so the min number of nodes
  # must be greater or equal to the number of migs.
  if [[ ${AUTOSCALER_MIN_NODES} -lt 0 ]]; then
    echo "AUTOSCALER_MIN_NODES must be greater or equal 0"
    exit 2
  fi

  # Each MIG must have at least one node, so the min number of nodes
  # must be greater or equal to the number of migs.
  if [[ ${AUTOSCALER_MAX_NODES} -lt ${NUM_MIGS} ]]; then
    echo "AUTOSCALER_MAX_NODES must be greater or equal ${NUM_MIGS}"
    exit 2
  fi
  if [[ ${NUM_WINDOWS_MIGS} -gt 0 ]]; then
    # TODO(pjh): implement Windows support in this function.
    echo "Not implemented yet: autoscaler config for Windows MIGs"
    exit 2
  fi

  # The code assumes that the migs were created with create-nodes
  # function which tries to evenly spread nodes across the migs.
  AUTOSCALER_MIG_CONFIG=""

  local left_min=${AUTOSCALER_MIN_NODES}
  local left_max=${AUTOSCALER_MAX_NODES}

  for ((i=1; i <= NUM_MIGS; i++)); do
    local group_name="${NODE_INSTANCE_PREFIX}-group-$i"
    if [[ $i -eq ${NUM_MIGS} ]]; then
      # TODO: We don't add a suffix for the last group to keep backward compatibility when there's only one MIG.
      # We should change it at some point, but note #18545 when changing this.
      group_name="${NODE_INSTANCE_PREFIX}-group"
    fi

    this_mig_min=$((left_min/(NUM_MIGS-i+1)))
    this_mig_max=$((left_max/(NUM_MIGS-i+1)))
    left_min=$((left_min-this_mig_min))
    left_max=$((left_max-this_mig_max))

    local mig_url="https://www.googleapis.com/compute/v1/projects/${PROJECT}/zones/${ZONE}/instanceGroups/${group_name}"
    AUTOSCALER_MIG_CONFIG="${AUTOSCALER_MIG_CONFIG} --nodes=${this_mig_min}:${this_mig_max}:${mig_url}"
  done

  AUTOSCALER_MIG_CONFIG="${AUTOSCALER_MIG_CONFIG} --scale-down-enabled=${AUTOSCALER_ENABLE_SCALE_DOWN}"
}

# Assumes:
# - NUM_MIGS
# - NODE_INSTANCE_PREFIX
# - PROJECT
# - ZONE
# - ENABLE_CLUSTER_AUTOSCALER
# - AUTOSCALER_MAX_NODES
# - AUTOSCALER_MIN_NODES
function create-autoscaler-config() {
  # Create autoscaler for nodes configuration if requested
  if [[ "${ENABLE_CLUSTER_AUTOSCALER}" == "true" ]]; then
    create-cluster-autoscaler-mig-config
    echo "Using autoscaler config: ${AUTOSCALER_MIG_CONFIG} ${AUTOSCALER_EXPANDER_CONFIG}"
  fi
}

function check-cluster() {
  detect-node-names
  detect-master

  echo "Waiting up to ${KUBE_CLUSTER_INITIALIZATION_TIMEOUT} seconds for cluster initialization."
  echo
  echo "  This will continually check to see if the API for kubernetes is reachable."
  echo "  This may time out if there was some uncaught error during start up."
  echo

  # curl in mavericks is borked.
  secure=""
  if which sw_vers >& /dev/null; then
    if [[ $(sw_vers | grep ProductVersion | awk '{print $2}') = "10.9."* ]]; then
      secure="--insecure"
    fi
  fi

  local start_time
  local curl_out
  start_time=$(date +%s)
  curl_out=$(mktemp)
  kube::util::trap_add "rm -f ${curl_out}" EXIT
  until curl -vsS --cacert "${CERT_DIR}/pki/ca.crt" \
          -H "Authorization: Bearer ${KUBE_BEARER_TOKEN}" \
          ${secure} \
          --max-time 5 --fail \
          "https://${KUBE_MASTER_IP}/api/v1/pods?limit=100" > "${curl_out}" 2>&1; do
      local elapsed
      elapsed=$(($(date +%s) - start_time))
      if [[ ${elapsed} -gt ${KUBE_CLUSTER_INITIALIZATION_TIMEOUT} ]]; then
          echo -e "${color_red}Cluster failed to initialize within ${KUBE_CLUSTER_INITIALIZATION_TIMEOUT} seconds.${color_norm}" >&2
          echo "Last output from querying API server follows:" >&2
          echo "-----------------------------------------------------" >&2
          cat "${curl_out}" >&2
          echo "-----------------------------------------------------" >&2
          exit 2
      fi
      printf "."
      sleep 2
  done

  echo "Kubernetes cluster created."

  export KUBE_CERT="${CERT_DIR}/pki/issued/kubecfg.crt"
  export KUBE_KEY="${CERT_DIR}/pki/private/kubecfg.key"
  export CA_CERT="${CERT_DIR}/pki/ca.crt"
  export CONTEXT="${PROJECT}_${INSTANCE_PREFIX}"
  (
   umask 077

   # Update the user's kubeconfig to include credentials for this apiserver.
   create-kubeconfig
  )

  # ensures KUBECONFIG is set
  get-kubeconfig-basicauth

  if [[ ${GCE_UPLOAD_KUBCONFIG_TO_MASTER_METADATA:-} == "true" ]]; then
    gcloud compute instances add-metadata "${MASTER_NAME}" --project="${PROJECT}" --zone="${ZONE}"  --metadata-from-file="kubeconfig=${KUBECONFIG}" || true
  fi

  echo
  echo -e "${color_green:-}Kubernetes cluster is running.  The master is running at:"
  echo
  echo -e "${color_yellow}  https://${KUBE_MASTER_IP}"
  echo
  echo -e "${color_green}The user name and password to use is located in ${KUBECONFIG}.${color_norm}"
  echo

}

# Removes master replica from etcd cluster.
#
# Assumed vars:
#   REPLICA_NAME
#   PROJECT
#   EXISTING_MASTER_NAME
#   EXISTING_MASTER_ZONE
#
# $1: etcd client port
# $2: whether etcd communication should use mtls
# returns the result of ssh command which removes replica
function remove-replica-from-etcd() {
  local -r port="${1}"
  local -r use_mtls="${2}"

  TLSARG=""
  PROTO="http://"
  if [[ "${use_mtls}" == "true" ]]; then
    # Keep in sync with ETCD_APISERVER_CA_CERT_PATH, ETCD_APISERVER_CLIENT_CERT_PATH and ETCD_APISERVER_CLIENT_KEY_PATH in configure-helper.sh.
    TLSARG="--cacert /etc/srv/kubernetes/pki/etcd-apiserver-ca.crt --cert /etc/srv/kubernetes/pki/etcd-apiserver-client.crt --key /etc/srv/kubernetes/pki/etcd-apiserver-client.key"
    PROTO="https://"
  fi
  [[ -n "${EXISTING_MASTER_NAME}" ]] || return
  run-gcloud-command "${EXISTING_MASTER_NAME}" "${EXISTING_MASTER_ZONE}" "curl -s ${TLSARG} ${PROTO}127.0.0.1:${port}/v2/members/\$(curl -s ${TLSARG} ${PROTO}127.0.0.1:${port}/v2/members -XGET | sed 's/{\\\"id/\n/g' | grep ${REPLICA_NAME}\\\" | cut -f 3 -d \\\") -XDELETE -L 2>/dev/null"
  local -r res=$?
  echo "Removing etcd replica, name: ${REPLICA_NAME}, port: ${port}, result: ${res}"
  return "${res}"
}

# Delete a kubernetes cluster. This is called from test-teardown.
#
# Assumed vars:
#   MASTER_NAME
#   NODE_INSTANCE_PREFIX
#   WINDOWS_NODE_INSTANCE_PREFIX
#   ZONE
# This function tears down cluster resources 10 at a time to avoid issuing too many
# API calls and exceeding API quota. It is important to bring down the instances before bringing
# down the firewall rules and routes.
function kube-down() {
  local -r batch=200

  detect-project
  detect-node-names # For INSTANCE_GROUPS and WINDOWS_INSTANCE_GROUPS

  echo "Bringing down cluster"
  set +e  # Do not stop on error

  if [[ "${KUBE_DELETE_NODES:-}" != "false" ]]; then
    # Get the name of the managed instance group template before we delete the
    # managed instance group. (The name of the managed instance group template may
    # change during a cluster upgrade.)
    local templates
    templates=$(get-template "${PROJECT}")

    # Deliberately allow globbing, do not change unless a bug is found
    # shellcheck disable=SC2206
    local all_instance_groups=(${INSTANCE_GROUPS[@]:-} ${WINDOWS_INSTANCE_GROUPS[@]:-})
    # Deliberately do not quote, do not change unless a bug is found
    # shellcheck disable=SC2068
    for group in ${all_instance_groups[@]:-}; do
      {
        if gcloud compute instance-groups managed describe "${group}" --project "${PROJECT}" --zone "${ZONE}" &>/dev/null; then
          gcloud compute instance-groups managed delete \
            --project "${PROJECT}" \
            --quiet \
            --zone "${ZONE}" \
            "${group}"
        fi
      } &
    done

    # Wait for last batch of jobs
    kube::util::wait-for-jobs || {
      echo -e "Failed to delete instance group(s)." >&2
    }

    # Deliberately do not quote, do not change unless a bug is found
    # shellcheck disable=SC2068
    for template in ${templates[@]:-}; do
      {
        if gcloud compute instance-templates describe --project "${PROJECT}" "${template}" &>/dev/null; then
          gcloud compute instance-templates delete \
            --project "${PROJECT}" \
            --quiet \
            "${template}"
        fi
      } &
    done

    # Wait for last batch of jobs
    kube::util::wait-for-jobs || {
      echo -e "Failed to delete instance template(s)." >&2
    }

    # Delete the special heapster node (if it exists).
    if [[ -n "${HEAPSTER_MACHINE_TYPE:-}" ]]; then
      local -r heapster_machine_name="${NODE_INSTANCE_PREFIX}-heapster"
      if gcloud compute instances describe "${heapster_machine_name}" --zone "${ZONE}" --project "${PROJECT}" &>/dev/null; then
        # Now we can safely delete the VM.
        gcloud compute instances delete \
          --project "${PROJECT}" \
          --quiet \
          --delete-disks all \
          --zone "${ZONE}" \
          "${heapster_machine_name}"
      fi
    fi
  fi

  local -r REPLICA_NAME="${KUBE_REPLICA_NAME:-$(get-replica-name)}"

  set-existing-master

  # Un-register the master replica from etcd and events etcd.
  remove-replica-from-etcd 2379 true
  remove-replica-from-etcd 4002 false

  # Delete the master replica (if it exists).
  if gcloud compute instances describe "${REPLICA_NAME}" --zone "${ZONE}" --project "${PROJECT}" &>/dev/null; then
    # If there is a load balancer in front of apiservers we need to first update its configuration.
    if gcloud compute target-pools describe "${MASTER_NAME}" --region "${REGION}" --project "${PROJECT}" &>/dev/null; then
      gcloud compute target-pools remove-instances "${MASTER_NAME}" \
        --project "${PROJECT}" \
        --zone "${ZONE}" \
        --instances "${REPLICA_NAME}"
    fi
    # Detach replica from LB if needed.
    if [[ ${GCE_PRIVATE_CLUSTER:-} == "true" ]]; then
      remove-from-internal-loadbalancer "${REPLICA_NAME}" "${ZONE}"
    fi
    # Now we can safely delete the VM.
    gcloud compute instances delete \
      --project "${PROJECT}" \
      --quiet \
      --delete-disks all \
      --zone "${ZONE}" \
      "${REPLICA_NAME}"
  fi

  # Delete the master replica pd (possibly leaked by kube-up if master create failed).
  # TODO(jszczepkowski): remove also possibly leaked replicas' pds
  local -r replica_pd="${REPLICA_NAME:-${MASTER_NAME}}-pd"
  if gcloud compute disks describe "${replica_pd}" --zone "${ZONE}" --project "${PROJECT}" &>/dev/null; then
    gcloud compute disks delete \
      --project "${PROJECT}" \
      --quiet \
      --zone "${ZONE}" \
      "${replica_pd}"
  fi

  # Check if this are any remaining master replicas.
  local REMAINING_MASTER_COUNT
  REMAINING_MASTER_COUNT=$(gcloud compute instances list \
    --project "${PROJECT}" \
    --filter="name ~ '$(get-replica-name-regexp)'" \
    --format "value(zone)" | wc -l)

  # In the replicated scenario, if there's only a single master left, we should also delete load balancer in front of it.
  if [[ "${REMAINING_MASTER_COUNT}" -eq 1 ]]; then
    detect-master
    local REMAINING_REPLICA_NAME
    local REMAINING_REPLICA_ZONE
    REMAINING_REPLICA_NAME="$(get-all-replica-names)"
    REMAINING_REPLICA_ZONE=$(gcloud compute instances list "${REMAINING_REPLICA_NAME}" \
      --project "${PROJECT}" --format='value(zone)')
    if gcloud compute forwarding-rules describe "${MASTER_NAME}" --region "${REGION}" --project "${PROJECT}" &>/dev/null; then
      gcloud compute forwarding-rules delete \
        --project "${PROJECT}" \
        --region "${REGION}" \
        --quiet \
        "${MASTER_NAME}"
      attach-external-ip "${REMAINING_REPLICA_NAME}" "${REMAINING_REPLICA_ZONE}" "${KUBE_MASTER_IP}"
      gcloud compute target-pools delete \
        --project "${PROJECT}" \
        --region "${REGION}" \
        --quiet \
        "${MASTER_NAME}"
    fi

    if [[ ${GCE_PRIVATE_CLUSTER:-} == "true" ]]; then
      remove-from-internal-loadbalancer "${REMAINING_REPLICA_NAME}" "${REMAINING_REPLICA_ZONE}"
      delete-internal-loadbalancer
      attach-internal-master-ip "${REMAINING_REPLICA_NAME}" "${REMAINING_REPLICA_ZONE}" "${KUBE_MASTER_INTERNAL_IP}"
    fi
  fi

  # If there are no more remaining master replicas, we should delete all remaining network resources.
  if [[ "${REMAINING_MASTER_COUNT}" -eq 0 ]]; then
    # Delete firewall rule for the master, etcd servers, and nodes.
    delete-firewall-rules "${MASTER_NAME}-https" "${MASTER_NAME}-etcd" "${NODE_TAG}-all" "${MASTER_NAME}-konnectivity-server"
    # Delete the master's reserved IP
    if gcloud compute addresses describe "${MASTER_NAME}-ip" --region "${REGION}" --project "${PROJECT}" &>/dev/null; then
      gcloud compute addresses delete \
        --project "${PROJECT}" \
        --region "${REGION}" \
        --quiet \
        "${MASTER_NAME}-ip"
    fi

    if gcloud compute addresses describe "${MASTER_NAME}-internal-ip" --region "${REGION}" --project "${PROJECT}" &>/dev/null; then
      gcloud compute addresses delete \
        --project "${PROJECT}" \
        --region "${REGION}" \
        --quiet \
        "${MASTER_NAME}-internal-ip"
    fi
  fi

  if [[ "${KUBE_DELETE_NODES:-}" != "false" ]]; then
    # Find out what minions are running.
    local -a minions
    kube::util::read-array minions < <(gcloud compute instances list \
      --project "${PROJECT}" \
      --filter="(name ~ '${NODE_INSTANCE_PREFIX}-.+' OR name ~ '${WINDOWS_NODE_INSTANCE_PREFIX}-.+') AND zone:(${ZONE})" \
      --format='value(name)')
    # If any minions are running, delete them in batches.
    while (( "${#minions[@]}" > 0 )); do
      echo Deleting nodes "${minions[*]::${batch}}"
      gcloud compute instances delete \
        --project "${PROJECT}" \
        --quiet \
        --delete-disks boot \
        --zone "${ZONE}" \
        "${minions[@]::${batch}}"
      minions=( "${minions[@]:${batch}}" )
    done
  fi

  # If there are no more remaining master replicas: delete routes, pd for influxdb and update kubeconfig
  if [[ "${REMAINING_MASTER_COUNT}" -eq 0 ]]; then
    # Delete routes.
    local -a routes
    # Clean up all routes w/ names like "<cluster-name>-<node-GUID>"
    # e.g. "kubernetes-12345678-90ab-cdef-1234-567890abcdef". The name is
    # determined by the node controller on the master.
    # Note that this is currently a noop, as synchronously deleting the node MIG
    # first allows the master to cleanup routes itself.
    local TRUNCATED_PREFIX="${INSTANCE_PREFIX:0:26}"
    kube::util::read-array routes < <(gcloud compute routes list --project "${NETWORK_PROJECT}" \
      --filter="name ~ '${TRUNCATED_PREFIX}-.{8}-.{4}-.{4}-.{4}-.{12}'" \
      --format='value(name)')
    while (( "${#routes[@]}" > 0 )); do
      echo Deleting routes "${routes[*]::${batch}}"
      gcloud compute routes delete \
        --project "${NETWORK_PROJECT}" \
        --quiet \
        "${routes[@]::${batch}}"
      routes=( "${routes[@]:${batch}}" )
    done

    # Delete persistent disk for influx-db.
    if gcloud compute disks describe "${INSTANCE_PREFIX}"-influxdb-pd --zone "${ZONE}" --project "${PROJECT}" &>/dev/null; then
      gcloud compute disks delete \
        --project "${PROJECT}" \
        --quiet \
        --zone "${ZONE}" \
        "${INSTANCE_PREFIX}"-influxdb-pd
    fi

    # Delete all remaining firewall rules and network.
    delete-firewall-rules \
      "${CLUSTER_NAME}-default-internal-master" \
      "${CLUSTER_NAME}-default-internal-node"

    if [[ "${KUBE_DELETE_NETWORK}" == "true" ]]; then
      delete-firewall-rules \
      "${NETWORK}-default-ssh" \
      "${NETWORK}-default-rdp" \
      "${NETWORK}-default-internal"  # Pre-1.5 clusters
      delete-cloud-nat-router
      # Delete all remaining firewall rules in the network.
      delete-all-firewall-rules || true
      delete-subnetworks || true
      delete-network || true  # might fail if there are leaked resources that reference the network
    fi

    # If there are no more remaining master replicas, we should update kubeconfig.
    export CONTEXT="${PROJECT}_${INSTANCE_PREFIX}"
    clear-kubeconfig
  else
  # If some master replicas remain: cluster has been changed, we need to re-validate it.
    echo "... calling validate-cluster" >&2
    # Override errexit
    (validate-cluster) && validate_result="$?" || validate_result="$?"

    # We have two different failure modes from validate cluster:
    # - 1: fatal error - cluster won't be working correctly
    # - 2: weak error - something went wrong, but cluster probably will be working correctly
    # We just print an error message in case 2).
    if [[ "${validate_result}" -eq 1 ]]; then
      exit 1
    elif [[ "${validate_result}" -eq 2 ]]; then
      echo "...ignoring non-fatal errors in validate-cluster" >&2
    fi
  fi
  set -e
}

# Prints name of one of the master replicas in the current zone. It will be either
# just MASTER_NAME or MASTER_NAME with a suffix for a replica (see get-replica-name-regexp).
#
# Assumed vars:
#   PROJECT
#   ZONE
#   MASTER_NAME
#
# NOTE: Must be in sync with get-replica-name-regexp and set-replica-name.
function get-replica-name() {
  # Ignore if gcloud compute fails and successfully echo any outcome
  # shellcheck disable=SC2005
  echo "$(gcloud compute instances list \
    --project "${PROJECT}" \
    --filter="name ~ '$(get-replica-name-regexp)' AND zone:(${ZONE})" \
    --format "value(name)" | head -n1)"
}

# Prints comma-separated names of all of the master replicas in all zones.
#
# Assumed vars:
#   PROJECT
#   MASTER_NAME
#
# NOTE: Must be in sync with get-replica-name-regexp and set-replica-name.
function get-all-replica-names() {
  # Ignore if gcloud compute fails and successfully echo any outcome
  # shellcheck disable=SC2005
  echo "$(gcloud compute instances list \
    --project "${PROJECT}" \
    --filter="name ~ '$(get-replica-name-regexp)'" \
    --format "value(name)" | tr "\n" "," | sed 's/,$//')"
}

# Prints the number of all of the master replicas in all zones.
#
# Assumed vars:
#   MASTER_NAME
function get-master-replicas-count() {
  detect-project
  local num_masters
  num_masters=$(gcloud compute instances list \
    --project "${PROJECT}" \
    --filter="name ~ '$(get-replica-name-regexp)'" \
    --format "value(zone)" | wc -l)
  echo -n "${num_masters}"
}

# Prints regexp for full master machine name. In a cluster with replicated master,
# VM names may either be MASTER_NAME or MASTER_NAME with a suffix for a replica.
function get-replica-name-regexp() {
  echo "^${MASTER_NAME}(-...)?"
}

# Sets REPLICA_NAME to a unique name for a master replica that will match
# expected regexp (see get-replica-name-regexp).
#
# Assumed vars:
#   PROJECT
#   ZONE
#   MASTER_NAME
#
# Sets:
#   REPLICA_NAME
function set-replica-name() {
  local instances
  instances=$(gcloud compute instances list \
    --project "${PROJECT}" \
    --filter="name ~ '$(get-replica-name-regexp)'" \
    --format "value(name)")

  suffix=""
  while echo "${instances}" | grep "${suffix}" &>/dev/null; do
    suffix="$(date | md5sum | head -c3)"
  done
  REPLICA_NAME="${MASTER_NAME}-${suffix}"
}

# Gets the instance templates in use by the cluster. It echos the template names
# so that the function output can be used.
# Assumed vars:
#   NODE_INSTANCE_PREFIX
#   WINDOWS_NODE_INSTANCE_PREFIX
#
# $1: project
function get-template() {
  local linux_filter="${NODE_INSTANCE_PREFIX}-(extra-)?template(-(${KUBE_RELEASE_VERSION_DASHED_REGEX}|${KUBE_CI_VERSION_DASHED_REGEX}))?"
  local windows_filter="${WINDOWS_NODE_INSTANCE_PREFIX}-template(-(${KUBE_RELEASE_VERSION_DASHED_REGEX}|${KUBE_CI_VERSION_DASHED_REGEX}))?"

  gcloud compute instance-templates list \
    --filter="name ~ '${linux_filter}' OR name ~ '${windows_filter}'" \
    --project="${1}" --format='value(name)'
}

# Checks if there are any present resources related kubernetes cluster.
#
# Assumed vars:
#   MASTER_NAME
#   NODE_INSTANCE_PREFIX
#   WINDOWS_NODE_INSTANCE_PREFIX
#   ZONE
#   REGION
# Vars set:
#   KUBE_RESOURCE_FOUND
function check-resources() {
  detect-project
  detect-node-names

  echo "Looking for already existing resources"
  KUBE_RESOURCE_FOUND=""

  if [[ -n "${INSTANCE_GROUPS[*]:-}" ]]; then
    KUBE_RESOURCE_FOUND="Managed instance groups ${INSTANCE_GROUPS[*]}"
    return 1
  fi
  if [[ -n "${WINDOWS_INSTANCE_GROUPS[*]:-}" ]]; then
    KUBE_RESOURCE_FOUND="Managed instance groups ${WINDOWS_INSTANCE_GROUPS[*]}"
    return 1
  fi

  if gcloud compute instance-templates describe --project "${PROJECT}" "${NODE_INSTANCE_PREFIX}-template" &>/dev/null; then
    KUBE_RESOURCE_FOUND="Instance template ${NODE_INSTANCE_PREFIX}-template"
    return 1
  fi
  if gcloud compute instance-templates describe --project "${PROJECT}" "${WINDOWS_NODE_INSTANCE_PREFIX}-template" &>/dev/null; then
    KUBE_RESOURCE_FOUND="Instance template ${WINDOWS_NODE_INSTANCE_PREFIX}-template"
    return 1
  fi

  if gcloud compute instances describe --project "${PROJECT}" "${MASTER_NAME}" --zone "${ZONE}" &>/dev/null; then
    KUBE_RESOURCE_FOUND="Kubernetes master ${MASTER_NAME}"
    return 1
  fi

  if gcloud compute disks describe --project "${PROJECT}" "${MASTER_NAME}"-pd --zone "${ZONE}" &>/dev/null; then
    KUBE_RESOURCE_FOUND="Persistent disk ${MASTER_NAME}-pd"
    return 1
  fi

  # Find out what minions are running.
  local -a minions
  kube::util::read-array minions < <(gcloud compute instances list \
    --project "${PROJECT}" \
    --filter="(name ~ '${NODE_INSTANCE_PREFIX}-.+' OR name ~ '${WINDOWS_NODE_INSTANCE_PREFIX}-.+') AND zone:(${ZONE})" \
    --format='value(name)')
  if (( "${#minions[@]}" > 0 )); then
    KUBE_RESOURCE_FOUND="${#minions[@]} matching ${NODE_INSTANCE_PREFIX}-.+ or ${WINDOWS_NODE_INSTANCE_PREFIX}-.+"
    return 1
  fi

  if gcloud compute firewall-rules describe --project "${NETWORK_PROJECT}" "${MASTER_NAME}-https" &>/dev/null; then
    KUBE_RESOURCE_FOUND="Firewall rules for ${MASTER_NAME}-https"
    return 1
  fi

  if gcloud compute firewall-rules describe --project "${NETWORK_PROJECT}" "${NODE_TAG}-all" &>/dev/null; then
    KUBE_RESOURCE_FOUND="Firewall rules for ${MASTER_NAME}-all"
    return 1
  fi

  local -a routes
  kube::util::read-array routes < <(gcloud compute routes list --project "${NETWORK_PROJECT}" \
    --filter="name ~ '${INSTANCE_PREFIX}-minion-.{4}'" --format='value(name)')
  if (( "${#routes[@]}" > 0 )); then
    KUBE_RESOURCE_FOUND="${#routes[@]} routes matching ${INSTANCE_PREFIX}-minion-.{4}"
    return 1
  fi

  if gcloud compute addresses describe --project "${PROJECT}" "${MASTER_NAME}-ip" --region "${REGION}" &>/dev/null; then
    KUBE_RESOURCE_FOUND="Master's reserved IP"
    return 1
  fi

  # No resources found.
  return 0
}

# -----------------------------------------------------------------------------
# Cluster specific test helpers

# Execute prior to running tests to build a release if required for env.
#
# Assumed Vars:
#   KUBE_ROOT
function test-build-release() {
  # Make a release
  "${KUBE_ROOT}/build/release.sh"
}

# Execute prior to running tests to initialize required structure.
#
# Assumed vars:
#   Variables from config.sh
function test-setup() {
  # Detect the project into $PROJECT if it isn't set
  detect-project

  if [[ ${MULTIZONE:-} == "true" && -n ${E2E_ZONES:-} ]]; then
    for KUBE_GCE_ZONE in ${E2E_ZONES}; do
      KUBE_GCE_ZONE="${KUBE_GCE_ZONE}" KUBE_USE_EXISTING_MASTER="${KUBE_USE_EXISTING_MASTER:-}" "${KUBE_ROOT}/cluster/kube-up.sh"
      KUBE_USE_EXISTING_MASTER="true" # For subsequent zones we use the existing master
    done
  else
    "${KUBE_ROOT}/cluster/kube-up.sh"
  fi

  # Open up port 80 & 8080 so common containers on minions can be reached
  # TODO(roberthbailey): Remove this once we are no longer relying on hostPorts.
  local start
  start=$(date +%s)
  gcloud compute firewall-rules create \
    --project "${NETWORK_PROJECT}" \
    --target-tags "${NODE_TAG}" \
    --allow tcp:80,tcp:8080 \
    --network "${NETWORK}" \
    "${NODE_TAG}-http-alt" 2> /dev/null || true
  # As there is no simple way to wait longer for this operation we need to manually
  # wait some additional time (20 minutes altogether).
  while ! gcloud compute firewall-rules describe --project "${NETWORK_PROJECT}" "${NODE_TAG}-http-alt" 2> /dev/null; do
    if [[ $((start + 1200)) -lt $(date +%s) ]]; then
      echo -e "${color_red:-}Failed to create firewall ${NODE_TAG}-http-alt in ${NETWORK_PROJECT}" >&2
      exit 1
    fi
    sleep 5
  done

  # Open up the NodePort range
  # TODO(justinsb): Move to main setup, if we decide whether we want to do this by default.
  start=$(date +%s)
  gcloud compute firewall-rules create \
    --project "${NETWORK_PROJECT}" \
    --target-tags "${NODE_TAG}" \
    --allow tcp:30000-32767,udp:30000-32767 \
    --network "${NETWORK}" \
    "${NODE_TAG}-nodeports" 2> /dev/null || true
  # As there is no simple way to wait longer for this operation we need to manually
  # wait some additional time (20 minutes altogether).
  while ! gcloud compute firewall-rules describe --project "${NETWORK_PROJECT}" "${NODE_TAG}-nodeports" 2> /dev/null; do
    if [[ $((start + 1200)) -lt $(date +%s) ]]; then
      echo -e "${color_red}Failed to create firewall ${NODE_TAG}-nodeports in ${PROJECT}" >&2
      exit 1
    fi
    sleep 5
  done
}

# Execute after running tests to perform any required clean-up.
function test-teardown() {
  detect-project
  echo "Shutting down test cluster in background."
  delete-firewall-rules \
    "${NODE_TAG}-http-alt" \
    "${NODE_TAG}-nodeports"
  if [[ ${MULTIZONE:-} == "true" && -n ${E2E_ZONES:-} ]]; then
    local zones
    read -r -a zones <<< "${E2E_ZONES}"
    # tear them down in reverse order, finally tearing down the master too.
    for ((zone_num=${#zones[@]}-1; zone_num>0; zone_num--)); do
      KUBE_GCE_ZONE="${zones[zone_num]}" KUBE_USE_EXISTING_MASTER="true" "${KUBE_ROOT}/cluster/kube-down.sh"
    done
    KUBE_GCE_ZONE="${zones[0]}" KUBE_USE_EXISTING_MASTER="false" "${KUBE_ROOT}/cluster/kube-down.sh"
  else
    "${KUBE_ROOT}/cluster/kube-down.sh"
  fi
}

# SSH to a node by name ($1) and run a command ($2).
function ssh-to-node() {
  local node="$1"
  local cmd="$2"
  # Loop until we can successfully ssh into the box
  for (( i=0; i<5; i++)); do
    if gcloud compute ssh --ssh-flag='-o LogLevel=quiet' --ssh-flag='-o ConnectTimeout=30' --project "${PROJECT}" --zone="${ZONE}" "${node}" --command 'echo test > /dev/null'; then
      break
    fi
    sleep 5
  done
  # Then actually try the command.
  gcloud compute ssh --ssh-flag="-o LogLevel=quiet" --ssh-flag="-o ConnectTimeout=30" --project "${PROJECT}" --zone="${ZONE}" "${node}" --command "${cmd}"
}

# Perform preparations required to run e2e tests
function prepare-e2e() {
  detect-project
}

# Delete the image given by $1.
function delete-image() {
  gcloud container images delete --quiet "$1"
}
