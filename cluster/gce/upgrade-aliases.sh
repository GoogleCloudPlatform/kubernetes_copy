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

# !!!EXPERIMENTAL!!! Upgrade a K8s cluster from routes to IP aliases for
# node connectivity on GCE. This is only for migration.

set -o errexit
set -o nounset
set -o pipefail

if [[ "${KUBERNETES_PROVIDER:-gce}" != "gce" ]]; then
  echo "ERR: KUBERNETES_PROVIDER must be gce" >&2
  exit 1
fi

KUBE_ROOT=$(dirname "${BASH_SOURCE[0]}")/../..
source "${KUBE_ROOT}/hack/lib/util.sh"
source "${KUBE_ROOT}/cluster/kube-util.sh"

# Print the number of routes used for K8s cluster node connectivity.
#
# Assumed vars:
#   PROJECT
function get-k8s-node-routes-count() {
  local k8s_node_routes_count
  k8s_node_routes_count=$(gcloud compute routes list \
    --project="${PROJECT}" --filter='description=k8s-node-route' \
    --format='value(name)' | wc -l)
  echo -n "${k8s_node_routes_count}"
}

# Detect the subnetwork where the K8s cluster resides.
#
# Assumed vars:
#  KUBE_MASTER
#  PROJECT
#  ZONE
# Vars set:
#  IP_ALIAS_SUBNETWORK
function detect-k8s-subnetwork() {
  local subnetwork_url
  subnetwork_url=$(gcloud compute instances describe \
    "${KUBE_MASTER}" --project="${PROJECT}" --zone="${ZONE}" \
    --format='value(networkInterfaces[0].subnetwork)')
  if [[ -n ${subnetwork_url} ]]; then
    IP_ALIAS_SUBNETWORK=${subnetwork_url##*/}
  fi
}

# Set IP_ALIAS_SUBNETWORK's allowSubnetCidrRoutesOverlap to a boolean value.
# $1: true or false for the desired allowSubnetCidrRoutesOverlap.
#
# Assumed vars:
#   IP_ALIAS_SUBNETWORK
#   GCE_API_ENDPOINT
#   PROJECT
#   REGION
function set-allow-subnet-cidr-routes-overlap() {
  local allow_subnet_cidr_routes_overlap
  allow_subnet_cidr_routes_overlap=$(gcloud compute networks subnets \
    describe "${IP_ALIAS_SUBNETWORK}" --project="${PROJECT}" --region="${REGION}" \
    --format='value(allowSubnetCidrRoutesOverlap)')
  local allow_overlap=$1
  if [ "${allow_subnet_cidr_routes_overlap,,}" = "${allow_overlap}" ]; then
    echo "Subnet ${IP_ALIAS_SUBNETWORK}'s allowSubnetCidrRoutesOverlap is already set as $1"
    return
  fi

  echo "Setting subnet \"${IP_ALIAS_SUBNETWORK}\" allowSubnetCidrRoutesOverlap to $1"
  local fingerprint
  fingerprint=$(gcloud compute networks subnets describe \
    "${IP_ALIAS_SUBNETWORK}" --project="${PROJECT}" --region="${REGION}" \
    --format='value(fingerprint)')
  local access_token
  access_token=$(gcloud auth print-access-token)
  local request="{\"allowSubnetCidrRoutesOverlap\":$1, \"fingerprint\":\"${fingerprint}\"}"
  local subnetwork_url
  subnetwork_url="${GCE_API_ENDPOINT}projects/${PROJECT}/regions/${REGION}/subnetworks/${IP_ALIAS_SUBNETWORK}"
  until curl -s --header "Content-Type: application/json" --header "Authorization: Bearer ${access_token}" \
    -X PATCH -d "${request}" "${subnetwork_url}" --output /dev/null; do
    printf "."
    sleep 1
  done
}

# Add secondary ranges to K8s subnet.
#
# Assumed vars:
#   IP_ALIAS_SUBNETWORK
#   PROJECT
#   REGION
#   CLUSTER_IP_RANGE
#   SERVICE_CLUSTER_IP_RANGE
function add-k8s-subnet-secondary-ranges() {
  local secondary_ranges
  secondary_ranges=$(gcloud compute networks subnets describe "${IP_ALIAS_SUBNETWORK}" \
    --project="${PROJECT}" --region="${REGION}" \
    --format='value(secondaryIpRanges)')
  if [[ "${secondary_ranges}" =~ "pods-default" && "${secondary_ranges}" =~ "services-default" ]]; then
    echo "${secondary_ranges} already contains both pods-default and services-default secondary ranges"
    return
  fi

  echo "Adding secondary ranges: pods-default (${CLUSTER_IP_RANGE}), services-default (${SERVICE_CLUSTER_IP_RANGE})"
  until gcloud compute networks subnets update "${IP_ALIAS_SUBNETWORK}" \
    --project="${PROJECT}" --region="${REGION}" \
    --add-secondary-ranges="pods-default=${CLUSTER_IP_RANGE},services-default=${SERVICE_CLUSTER_IP_RANGE}"; do
    printf "."
    sleep 1
  done
}

# Delete all K8s node routes.
#
# Assumed vars:
#   PROJECT
function delete-k8s-node-routes() {
  local -a routes
  local -r batch=200
  routes=()
  while IFS=$'\n' read -r route; do
    routes+=( "${route}" )
  done < <(gcloud compute routes list \
    --project="${PROJECT}" --filter='description=k8s-node-route' \
    --format='value(name)')
  while (( "${#routes[@]}" > 0 )); do
      echo Deleting k8s node routes "${routes[*]::${batch}}"
      gcloud compute routes delete --project "${PROJECT}" --quiet "${routes[@]::${batch}}"
      routes=( "${routes[@]:${batch}}" )
  done
}

detect-project
detect-master

k8s_node_routes_count=$(get-k8s-node-routes-count)
if [[ "${k8s_node_routes_count}" -eq 0 ]]; then
  echo "No k8s node routes found and IP alias should already be enabled. Exiting..."
  exit 0
fi
echo "Found ${k8s_node_routes_count} K8s node routes. Proceeding to upgrade them to IP aliases based connectivity..."

detect-k8s-subnetwork
if [ -z "${IP_ALIAS_SUBNETWORK}" ]; then
  echo "No k8s cluster subnetwork found. Exiting..."
  exit 1
fi
echo "k8s cluster sits on subnetwork \"${IP_ALIAS_SUBNETWORK}\""

set-allow-subnet-cidr-routes-overlap true
add-k8s-subnet-secondary-ranges

echo "Changing K8s master envs and restarting..."
export KUBE_GCE_IP_ALIAS_SUBNETWORK=${IP_ALIAS_SUBNETWORK}
export KUBE_GCE_NODE_IPAM_MODE="IPAMFromCluster"
export KUBE_GCE_ENABLE_IP_ALIASES=true
export SECONDARY_RANGE_NAME="pods-default"
export STORAGE_BACKEND="etcd3"
export STORAGE_MEDIA_TYPE="application/vnd.kubernetes.protobuf"
export ETCD_IMAGE=3.5.17-0
export ETCD_VERSION=3.5.17

# Upgrade master with updated kube envs
"${KUBE_ROOT}/cluster/gce/upgrade.sh" -M -l

delete-k8s-node-routes
set-allow-subnet-cidr-routes-overlap false
