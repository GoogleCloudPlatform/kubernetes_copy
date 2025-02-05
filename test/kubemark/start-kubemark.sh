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

# Script that creates a Kubemark cluster for any given cloud provider.

set -o errexit
set -o nounset
set -o pipefail

TMP_ROOT="$(dirname "${BASH_SOURCE[@]}")/../.."
KUBE_ROOT=$(readlink -e "${TMP_ROOT}" 2> /dev/null || perl -MCwd -e 'print Cwd::abs_path shift' "${TMP_ROOT}")

source "${KUBE_ROOT}/test/kubemark/skeleton/util.sh"
source "${KUBE_ROOT}/test/kubemark/cloud-provider-config.sh"
source "${KUBE_ROOT}/test/kubemark/${CLOUD_PROVIDER}/util.sh"
source "${KUBE_ROOT}/cluster/kubemark/${CLOUD_PROVIDER}/config-default.sh"

if [[ -f "${KUBE_ROOT}/test/kubemark/${CLOUD_PROVIDER}/startup.sh" ]] ; then
  source "${KUBE_ROOT}/test/kubemark/${CLOUD_PROVIDER}/startup.sh"
fi

source "${KUBE_ROOT}/cluster/kubemark/util.sh"

KUBECTL="${KUBE_ROOT}/cluster/kubectl.sh"
KUBEMARK_DIRECTORY="${KUBE_ROOT}/test/kubemark"
RESOURCE_DIRECTORY="${KUBEMARK_DIRECTORY}/resources"
LOCAL_KUBECONFIG="${RESOURCE_DIRECTORY}/kubeconfig.kubemark"
INTERNAL_KUBECONFIG="${RESOURCE_DIRECTORY}/kubeconfig-internal.kubemark"

# Generate a random 6-digit alphanumeric tag for the kubemark image.
# Used to uniquify image builds across different invocations of this script.
KUBEMARK_IMAGE_TAG=$(head /dev/urandom | LC_CTYPE=C tr -dc 'a-z0-9' | fold -w 6 | head -n 1)

# Create a docker image for hollow-node and upload it to the appropriate docker registry.
function create-and-upload-hollow-node-image {
  authenticate-docker
  KUBEMARK_IMAGE_REGISTRY="${KUBEMARK_IMAGE_REGISTRY:-${CONTAINER_REGISTRY}/${PROJECT}}"
  # Build+push the image through makefile.
  build_cmd=("make" "${KUBEMARK_IMAGE_MAKE_TARGET}")
  MAKE_DIR="${KUBE_ROOT}/cluster/images/kubemark"
  KUBEMARK_BIN="$(kube::util::find-binary-for-platform kubemark linux/amd64)"
  if [[ -z "${KUBEMARK_BIN}" ]]; then
    echo 'Cannot find cmd/kubemark binary'
    exit 1
  fi
  echo "Copying kubemark binary to ${MAKE_DIR}"
  cp "${KUBEMARK_BIN}" "${MAKE_DIR}"
  CURR_DIR=$(pwd)
  cd "${MAKE_DIR}"
  REGISTRY=${KUBEMARK_IMAGE_REGISTRY} IMAGE_TAG=${KUBEMARK_IMAGE_TAG} run-cmd-with-retries "${build_cmd[@]}"
  rm kubemark
  cd "$CURR_DIR"
  echo "Created and uploaded the kubemark hollow-node image to docker registry."
  # Cleanup the kubemark image after the script exits.
  if [[ "${CLEANUP_KUBEMARK_IMAGE:-}" == "true" ]]; then
    trap delete-kubemark-image EXIT
  fi
}

function delete-kubemark-image {
  delete-image "${KUBEMARK_IMAGE_REGISTRY}/kubemark:${KUBEMARK_IMAGE_TAG}"
}

# Generate secret and configMap for the hollow-node pods to work, prepare
# manifests of the hollow-node and heapster replication controllers from
# templates, and finally create these resources through kubectl.
function create-kube-hollow-node-resources {
  # Create kubemark namespace.
  "${KUBECTL}" create -f "${RESOURCE_DIRECTORY}/kubemark-ns.json"

  # Create configmap for configuring hollow- kubelet, proxy and npd.
  "${KUBECTL}" create configmap "node-configmap" --namespace="kubemark" \
    --from-file=kernel.monitor="${RESOURCE_DIRECTORY}/kernel-monitor.json"

  # Create secret for passing kubeconfigs to kubelet, kubeproxy and npd.
  # It's bad that all component shares the same kubeconfig.
  # TODO(https://github.com/kubernetes/kubernetes/issues/79883): Migrate all components to separate credentials.
  "${KUBECTL}" create secret generic "kubeconfig" --type=Opaque --namespace="kubemark" \
    --from-file=kubelet.kubeconfig="${HOLLOWNODE_KUBECONFIG}" \
    --from-file=kubeproxy.kubeconfig="${HOLLOWNODE_KUBECONFIG}" \
    --from-file=npd.kubeconfig="${HOLLOWNODE_KUBECONFIG}" \
    --from-file=heapster.kubeconfig="${HOLLOWNODE_KUBECONFIG}" \
    --from-file=cluster_autoscaler.kubeconfig="${HOLLOWNODE_KUBECONFIG}" \
    --from-file=dns.kubeconfig="${HOLLOWNODE_KUBECONFIG}"

  # Create addon pods.
  # Heapster.
  mkdir -p "${RESOURCE_DIRECTORY}/addons"
  sed "s@{{MASTER_IP}}@${MASTER_IP}@g" "${RESOURCE_DIRECTORY}/heapster_template.json" > "${RESOURCE_DIRECTORY}/addons/heapster.json"
  metrics_mem_per_node=4
  metrics_mem=$((200 + metrics_mem_per_node*NUM_NODES))
  sed -i'' -e "s@{{METRICS_MEM}}@${metrics_mem}@g" "${RESOURCE_DIRECTORY}/addons/heapster.json"
  metrics_cpu_per_node_numerator=${NUM_NODES}
  metrics_cpu_per_node_denominator=2
  metrics_cpu=$((80 + metrics_cpu_per_node_numerator / metrics_cpu_per_node_denominator))
  sed -i'' -e "s@{{METRICS_CPU}}@${metrics_cpu}@g" "${RESOURCE_DIRECTORY}/addons/heapster.json"
  eventer_mem_per_node=500
  eventer_mem=$((200 * 1024 + eventer_mem_per_node*NUM_NODES))
  sed -i'' -e "s@{{EVENTER_MEM}}@${eventer_mem}@g" "${RESOURCE_DIRECTORY}/addons/heapster.json"

  # Cluster Autoscaler.
  if [[ "${ENABLE_KUBEMARK_CLUSTER_AUTOSCALER:-}" == "true" ]]; then
    echo "Setting up Cluster Autoscaler"
    KUBEMARK_AUTOSCALER_MIG_NAME="${KUBEMARK_AUTOSCALER_MIG_NAME:-${NODE_INSTANCE_PREFIX}-group}"
    KUBEMARK_AUTOSCALER_MIN_NODES="${KUBEMARK_AUTOSCALER_MIN_NODES:-0}"
    KUBEMARK_AUTOSCALER_MAX_NODES="${KUBEMARK_AUTOSCALER_MAX_NODES:-10}"
    NUM_NODES=${KUBEMARK_AUTOSCALER_MAX_NODES}
    echo "Setting maximum cluster size to ${NUM_NODES}."
    KUBEMARK_MIG_CONFIG="autoscaling.k8s.io/nodegroup: ${KUBEMARK_AUTOSCALER_MIG_NAME}"
    sed "s/{{master_ip}}/${MASTER_IP}/g" "${RESOURCE_DIRECTORY}/cluster-autoscaler_template.json" > "${RESOURCE_DIRECTORY}/addons/cluster-autoscaler.json"
    sed -i'' -e "s@{{kubemark_autoscaler_mig_name}}@${KUBEMARK_AUTOSCALER_MIG_NAME}@g" "${RESOURCE_DIRECTORY}/addons/cluster-autoscaler.json"
    sed -i'' -e "s@{{kubemark_autoscaler_min_nodes}}@${KUBEMARK_AUTOSCALER_MIN_NODES}@g" "${RESOURCE_DIRECTORY}/addons/cluster-autoscaler.json"
    sed -i'' -e "s@{{kubemark_autoscaler_max_nodes}}@${KUBEMARK_AUTOSCALER_MAX_NODES}@g" "${RESOURCE_DIRECTORY}/addons/cluster-autoscaler.json"
  fi

  # Kube DNS.
  if [[ "${ENABLE_KUBEMARK_KUBE_DNS:-}" == "true" ]]; then
    echo "Setting up kube-dns"
    sed "s@{{dns_domain}}@${KUBE_DNS_DOMAIN}@g" "${RESOURCE_DIRECTORY}/kube_dns_template.yaml" > "${RESOURCE_DIRECTORY}/addons/kube_dns.yaml"
  fi

  "${KUBECTL}" create -f "${RESOURCE_DIRECTORY}/addons" --namespace="kubemark"

  # Create the replication controller for hollow-nodes.
  # We allow to override the NUM_REPLICAS when running Cluster Autoscaler.
  NUM_REPLICAS=${NUM_REPLICAS:-${NUM_NODES}}
  sed "s@{{numreplicas}}@${NUM_REPLICAS}@g" "${RESOURCE_DIRECTORY}/hollow-node_template.yaml" > "${RESOURCE_DIRECTORY}/hollow-node.yaml"
  proxy_cpu=20
  if [ "${NUM_NODES}" -gt 1000 ]; then
    proxy_cpu=50
  fi
  proxy_cpu=${KUBEMARK_HOLLOW_PROXY_MILLICPU:-$proxy_cpu}
  proxy_mem_per_node=${KUBEMARK_HOLLOW_PROXY_MEM_PER_NODE_KB:-50}
  proxy_mem=$((100 * 1024 + proxy_mem_per_node*NUM_NODES))
  hollow_node_labels=${HOLLOW_NODE_LABELS:-$(calculate-node-labels)}
  hollow_kubelet_params=$(eval "for param in ${HOLLOW_KUBELET_TEST_ARGS:-}; do echo -n \\\"\$param\\\",; done")
  hollow_kubelet_params=${hollow_kubelet_params%?}
  hollow_proxy_params=$(eval "for param in ${HOLLOW_PROXY_TEST_ARGS:-}; do echo -n \\\"\$param\\\",; done")
  hollow_proxy_params=${hollow_proxy_params%?}

  sed -i'' -e "s@{{hollow_kubelet_millicpu}}@${KUBEMARK_HOLLOW_KUBELET_MILLICPU:-40}@g" "${RESOURCE_DIRECTORY}/hollow-node.yaml"
  sed -i'' -e "s@{{hollow_kubelet_mem_Ki}}@${KUBEMARK_HOLLOW_KUBELET_MEM_KB:-$((100*1024))}@g" "${RESOURCE_DIRECTORY}/hollow-node.yaml"
  sed -i'' -e "s@{{hollow_proxy_millicpu}}@${proxy_cpu}@g" "${RESOURCE_DIRECTORY}/hollow-node.yaml"
  sed -i'' -e "s@{{hollow_proxy_mem_Ki}}@${proxy_mem}@g" "${RESOURCE_DIRECTORY}/hollow-node.yaml"
  sed -i'' -e "s@{{npd_millicpu}}@${KUBEMARK_NPD_MILLICPU:-20}@g" "${RESOURCE_DIRECTORY}/hollow-node.yaml"
  sed -i'' -e "s@{{npd_mem_Ki}}@${KUBEMARK_NPD_MEM_KB:-$((20*1024))}@g" "${RESOURCE_DIRECTORY}/hollow-node.yaml"
  sed -i'' -e "s@{{kubemark_image_registry}}@${KUBEMARK_IMAGE_REGISTRY}@g" "${RESOURCE_DIRECTORY}/hollow-node.yaml"
  sed -i'' -e "s@{{kubemark_image_tag}}@${KUBEMARK_IMAGE_TAG}@g" "${RESOURCE_DIRECTORY}/hollow-node.yaml"
  sed -i'' -e "s@{{master_ip}}@${MASTER_IP}@g" "${RESOURCE_DIRECTORY}/hollow-node.yaml"
  sed -i'' -e "s@{{hollow_node_labels}}@${hollow_node_labels}@g" "${RESOURCE_DIRECTORY}/hollow-node.yaml"
  sed -i'' -e "s@{{hollow_kubelet_params}}@${hollow_kubelet_params}@g" "${RESOURCE_DIRECTORY}/hollow-node.yaml"
  sed -i'' -e "s@{{hollow_proxy_params}}@${hollow_proxy_params}@g" "${RESOURCE_DIRECTORY}/hollow-node.yaml"
  sed -i'' -e "s@{{kubemark_mig_config}}@${KUBEMARK_MIG_CONFIG:-}@g" "${RESOURCE_DIRECTORY}/hollow-node.yaml"
  "${KUBECTL}" create -f "${RESOURCE_DIRECTORY}/hollow-node.yaml" --namespace="kubemark"

  echo "Created secrets, configMaps, replication-controllers required for hollow-nodes."
}

# Wait until all hollow-nodes are running or there is a timeout.
function wait-for-hollow-nodes-to-run-or-timeout {
  echo -n "Waiting for all hollow-nodes to become Running"
  start=$(date +%s)
  nodes=$("${KUBECTL}" --kubeconfig="${LOCAL_KUBECONFIG}" get node 2> /dev/null) || true
  ready=$(($(echo "${nodes}" | grep -vc "NotReady") - 1))

  until [[ "${ready}" -ge "${NUM_REPLICAS}" ]]; do
    echo -n "."
    sleep 1
    now=$(date +%s)
    # Fail it if it already took more than 30 minutes.
    if [ $((now - start)) -gt 1800 ]; then
      echo ""
      "${KUBECTL}" --kubeconfig="${LOCAL_KUBECONFIG}" describe node
      # shellcheck disable=SC2154 # Color defined in sourced script
      echo -e "${color_red} Timeout waiting for all hollow-nodes to become Running. ${color_norm}"
      # Try listing nodes again - if it fails it means that API server is not responding
      if "${KUBECTL}" --kubeconfig="${LOCAL_KUBECONFIG}" get node &> /dev/null; then
        echo "Found only ${ready} ready hollow-nodes while waiting for ${NUM_NODES}."
      else
        echo "Got error while trying to list hollow-nodes. Probably API server is down."
      fi
      pods=$("${KUBECTL}" get pods -l name=hollow-node --namespace=kubemark) || true
      running=$(($(echo "${pods}" | grep -c "Running")))
      echo "${running} hollow-nodes are reported as 'Running'"
      not_running=$(($(echo "${pods}" | grep -vc "Running") - 1))
      echo "${not_running} hollow-nodes are reported as NOT 'Running'"
      echo "${pods}" | grep -v Running
      exit 1
    fi
    nodes=$("${KUBECTL}" --kubeconfig="${LOCAL_KUBECONFIG}" get node 2> /dev/null) || true
    ready=$(($(echo "${nodes}" | grep -vc "NotReady") - 1))
  done
  # shellcheck disable=SC2154 # Color defined in sourced script
  echo -e "${color_green} Done!${color_norm}"
}

############################### Main Function ########################################

# Setup for hollow-nodes.
function start-hollow-nodes {
  # shellcheck disable=SC2154 # Color defined in sourced script
  echo -e "${color_yellow}STARTING SETUP FOR HOLLOW-NODES${color_norm}"
  create-and-upload-hollow-node-image
  create-kube-hollow-node-resources
  wait-for-hollow-nodes-to-run-or-timeout
}

# Annotates the node objects in the kubemark cluster to make their size
# similar to regular nodes.
# TODO(#90833): Replace this with image preloading from ClusterLoader to better
# reflect the reality in kubemark tests.
function resize-node-objects {
  if [[ -z "$KUBEMARK_NODE_OBJECT_SIZE_BYTES" ]]; then
    return 0
  fi

  annotation_size_bytes="${KUBEMARK_NODE_OBJECT_SIZE_BYTES}"
  echo "Annotating node objects with ${annotation_size_bytes} byte label"
  label=$( (LC_CTYPE=C < /dev/urandom tr -dc 'a-zA-Z0-9' | fold -w "$annotation_size_bytes"; true) | head -n 1)
  "${KUBECTL}" --kubeconfig="${LOCAL_KUBECONFIG}" get nodes -o name \
    | xargs -P50 -r -I% "${KUBECTL}" --kubeconfig="${LOCAL_KUBECONFIG}" annotate --overwrite % label="$label"
  echo "Annotating node objects completed"
}


detect-project &> /dev/null
create-kubemark-master

if [ -f "${INTERNAL_KUBECONFIG}" ]; then
    HOLLOWNODE_KUBECONFIG="${INTERNAL_KUBECONFIG}"
else
    HOLLOWNODE_KUBECONFIG="${LOCAL_KUBECONFIG}"
fi

MASTER_IP=$(grep server "${HOLLOWNODE_KUBECONFIG}" | awk -F "/" '{print $3}')

start-hollow-nodes
resize-node-objects

echo ""
echo "Master IP: ${MASTER_IP}"
echo "Kubeconfig for kubemark master is written in ${LOCAL_KUBECONFIG}"
