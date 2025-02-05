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

# Script that starts kubelet on kubemark-master as a supervisord process
# and then runs the master components as pods using kubelet.

set -o errexit
set -o nounset
set -o pipefail

# Define key path variables.
KUBE_ROOT="/home/kubernetes"
KUBE_BINDIR="${KUBE_ROOT}/kubernetes/server/bin"

function config-ip-firewall {
  echo "Configuring IP firewall rules"
  # The GCI image has host firewall which drop most inbound/forwarded packets.
  # We need to add rules to accept all TCP/UDP/ICMP packets.
  if iptables -L INPUT | grep "Chain INPUT (policy DROP)" > /dev/null; then
    echo "Add rules to accept all inbound TCP/UDP/ICMP packets"
    iptables -A INPUT -w -p TCP -j ACCEPT
    iptables -A INPUT -w -p UDP -j ACCEPT
    iptables -A INPUT -w -p ICMP -j ACCEPT
  fi
  if iptables -L FORWARD | grep "Chain FORWARD (policy DROP)" > /dev/null; then
    echo "Add rules to accept all forwarded TCP/UDP/ICMP packets"
    iptables -A FORWARD -w -p TCP -j ACCEPT
    iptables -A FORWARD -w -p UDP -j ACCEPT
    iptables -A FORWARD -w -p ICMP -j ACCEPT
  fi
}

function create-dirs {
	echo "Creating required directories"
	mkdir -p /var/lib/kubelet
	mkdir -p /etc/kubernetes/manifests
	mkdir -p /etc/kubernetes/addons
}

# Setup working directory for kubelet.
function setup-kubelet-dir {
	echo "Making /var/lib/kubelet executable for kubelet"
	mount -B /var/lib/kubelet /var/lib/kubelet/
	mount -B -o remount,exec,suid,dev /var/lib/kubelet
}

# Remove any default etcd config dirs/files.
function delete-default-etcd-configs {
	if [[ -d /etc/etcd ]]; then
		rm -rf /etc/etcd
	fi
	if [[ -e /etc/default/etcd ]]; then
		rm -f /etc/default/etcd
	fi
	if [[ -e /etc/systemd/system/etcd.service ]]; then
		rm -f /etc/systemd/system/etcd.service
	fi
	if [[ -e /etc/init.d/etcd ]]; then
		rm -f /etc/init.d/etcd
	fi
}

# Compute etcd related variables.
function compute-etcd-variables {
	ETCD_IMAGE="${ETCD_IMAGE:-}"
	ETCD_QUOTA_BYTES=""
	if [ "${ETCD_VERSION:0:2}" == "3." ]; then
		# TODO: Set larger quota to see if that helps with
		# 'mvcc: database space exceeded' errors. If so, pipe
		# though our setup scripts.
		ETCD_QUOTA_BYTES=" --quota-backend-bytes=4294967296 "
	fi
}

# Formats the given device ($1) if needed and mounts it at given mount point
# ($2).
function safe-format-and-mount() {
	device=$1
	mountpoint=$2

	# Format only if the disk is not already formatted.
	if ! tune2fs -l "${device}" ; then
		echo "Formatting '${device}'"
		mkfs.ext4 -F "${device}"
	fi

	echo "Mounting '${device}' at '${mountpoint}'"
	mount -o discard,defaults "${device}" "${mountpoint}"
}

# Finds a PD device with name '$1' attached to the master.
function find-attached-pd() {
	local -r pd_name=$1
	if [[ ! -e /dev/disk/by-id/${pd_name} ]]; then
		echo ""
	fi
	device_info=$(ls -l "/dev/disk/by-id/${pd_name}")
	relative_path=${device_info##* }
	echo "/dev/disk/by-id/${relative_path}"
}

# Mounts a persistent disk (formatting if needed) to store the persistent data
# on the master. safe-format-and-mount only formats an unformatted disk, and
# mkdir -p will leave a directory be if it already exists.
function mount-pd() {
	local -r pd_name=$1
	local -r mount_point=$2

	if [[ -z "${find-attached-pd ${pd_name}}" ]]; then
		echo "Can't find ${pd_name}. Skipping mount."
		return
	fi

	local -r pd_path="/dev/disk/by-id/${pd_name}"
	echo "Mounting PD '${pd_path}' at '${mount_point}'"
	# Format and mount the disk, create directories on it for all of the master's
	# persistent data, and link them to where they're used.
	mkdir -p "${mount_point}"
	safe-format-and-mount "${pd_path}" "${mount_point}"
	echo "Mounted PD '${pd_path}' at '${mount_point}'"

	# NOTE: These locations on the PD store persistent data, so to maintain
	# upgradeability, these locations should not change.  If they do, take care
	# to maintain a migration path from these locations to whatever new
	# locations.
}

# Create kubeconfig for controller-manager's service account authentication.
function create-kubecontrollermanager-kubeconfig {
	echo "Creating kube-controller-manager kubeconfig file"
	mkdir -p "${KUBE_ROOT}/k8s_auth_data/kube-controller-manager"
	cat <<EOF >"${KUBE_ROOT}/k8s_auth_data/kube-controller-manager/kubeconfig"
apiVersion: v1
kind: Config
users:
- name: kube-controller-manager
  user:
    token: ${KUBE_CONTROLLER_MANAGER_TOKEN}
clusters:
- name: local
  cluster:
    insecure-skip-tls-verify: true
    server: https://localhost:443
contexts:
- context:
    cluster: local
    user: kube-controller-manager
  name: service-account-context
current-context: service-account-context
EOF
}

function create-kubescheduler-kubeconfig {
  echo "Creating kube-scheduler kubeconfig file"
  mkdir -p "${KUBE_ROOT}/k8s_auth_data/kube-scheduler"
  cat <<EOF >"${KUBE_ROOT}/k8s_auth_data/kube-scheduler/kubeconfig"
apiVersion: v1
kind: Config
users:
- name: kube-scheduler
  user:
    token: ${KUBE_SCHEDULER_TOKEN}
clusters:
- name: local
  cluster:
    insecure-skip-tls-verify: true
    server: https://localhost:443
contexts:
- context:
    cluster: local
    user: kube-scheduler
  name: kube-scheduler
current-context: kube-scheduler
EOF
}

function create-addonmanager-kubeconfig {
  echo "Creating addonmanager kubeconfig file"
  mkdir -p "${KUBE_ROOT}/k8s_auth_data/addon-manager"
  cat <<EOF >"${KUBE_ROOT}/k8s_auth_data/addon-manager/kubeconfig"
apiVersion: v1
kind: Config
users:
- name: addon-manager
  user:
    token: ${ADDON_MANAGER_TOKEN}
clusters:
- name: local
  cluster:
    insecure-skip-tls-verify: true
    server: https://localhost:443
contexts:
- context:
    cluster: local
    user: addon-manager
  name: addon-manager
current-context: addon-manager
EOF
}

function assemble-docker-flags {
	echo "Assemble docker command line flags"
	local docker_opts="-p /var/run/docker.pid --iptables=false --ip-masq=false"
	docker_opts+=" --log-level=debug"  # Since it's a test cluster
	# TODO(shyamjvs): Incorporate network plugin options, etc later.
	echo "DOCKER_OPTS=\"${docker_opts}\"" > /etc/default/docker
	echo "DOCKER_NOFILE=65536" >> /etc/default/docker  # For setting ulimit -n
	systemctl restart docker
}

# A helper function for loading a docker image. It keeps trying up to 5 times.
#
# $1: Full path of the docker image
function try-load-docker-image {
	local -r img=$1
	echo "Try to load docker image file ${img}"
	# Temporarily turn off errexit, because we don't want to exit on first failure.
	set +e
	local -r max_attempts=5
	local -i attempt_num=1
	until timeout 30 docker load -i "${img}"; do
		if [[ "${attempt_num}" == "${max_attempts}" ]]; then
			echo "Fail to load docker image file ${img} after ${max_attempts} retries. Exit!!"
			exit 1
		else
			attempt_num=$((attempt_num+1))
			sleep 5
		fi
	done
	# Re-enable errexit.
	set -e
}

# Loads kube-system docker images. It is better to do it before starting kubelet,
# as kubelet will restart docker daemon, which may interfere with loading images.
function load-docker-images {
	echo "Start loading kube-system docker images"
	local -r img_dir="${KUBE_BINDIR}"
	try-load-docker-image "${img_dir}/kube-apiserver.tar"
	try-load-docker-image "${img_dir}/kube-controller-manager.tar"
	try-load-docker-image "${img_dir}/kube-scheduler.tar"
}

# Computes command line arguments to be passed to kubelet.
function compute-kubelet-params {
	local params="${KUBELET_TEST_ARGS:-}"
	params+=" --cgroup-root=/"
	params+=" --cloud-provider=gce"
	params+=" --pod-manifest-path=/etc/kubernetes/manifests"
	if [[ -n "${KUBELET_PORT:-}" ]]; then
		params+=" --port=${KUBELET_PORT}"
	fi
	params+=" --enable-debugging-handlers=false"
	params+=" --hairpin-mode=none"
	echo "${params}"
}

# Creates the systemd config file for kubelet.service.
function create-kubelet-conf() {
	local -r kubelet_bin="$1"
	local -r kubelet_env_file="/etc/default/kubelet"
	local -r flags=$(compute-kubelet-params)
	echo "KUBELET_OPTS=\"${flags}\"" > "${kubelet_env_file}"

	# Write the systemd service file for kubelet.
	cat <<EOF >/etc/systemd/system/kubelet.service
[Unit]
Description=Kubermark kubelet
Requires=network-online.target
After=network-online.target

[Service]
Restart=always
RestartSec=10
EnvironmentFile=${kubelet_env_file}
ExecStart=${kubelet_bin} \$KUBELET_OPTS

[Install]
WantedBy=multi-user.target
EOF
}

# This function assembles the kubelet systemd service file and starts it using
# systemctl, on the kubemark master.
function start-kubelet {
	# Create systemd config.
	local -r kubelet_bin="/usr/bin/kubelet"
	create-kubelet-conf "${kubelet_bin}"

	# Flush iptables nat table
  	iptables -t nat -F || true

	# Start the kubelet service.
	systemctl start kubelet.service
}

# Create the log file and set its properties.
#
# $1 is the file to create.
function prepare-log-file {
	touch "$1"
	chmod 644 "$1"
	chown root:root "$1"
}

# A helper function for copying addon manifests and set dir/files
# permissions.
#
# $1: addon category under /etc/kubernetes
# $2: manifest source dir
function setup-addon-manifests {
  local -r src_dir="${KUBE_ROOT}/$2"
  local -r dst_dir="/etc/kubernetes/$1/$2"

  if [[ ! -d "${dst_dir}" ]]; then
    mkdir -p "${dst_dir}"
  fi

  local files
  files=$(find "${src_dir}" -maxdepth 1 -name "*.yaml")
  if [[ -n "${files}" ]]; then
    cp "${src_dir}/"*.yaml "${dst_dir}"
  fi
  chown -R root:root "${dst_dir}"
  chmod 755 "${dst_dir}"
  chmod 644 "${dst_dir}"/*
}

# Write the config for the audit policy.
# Note: This duplicates the function in cluster/gce/gci/configure-helper.sh.
# TODO: Get rid of this function when #53321 is fixed.
function create-master-audit-policy {
  local -r path="${1}"
  local -r policy="${2:-}"

  if [[ -n "${policy}" ]]; then
    echo "${policy}" > "${path}"
    return
  fi

  # Known api groups
  local -r known_apis='
      - group: "" # core
      - group: "admissionregistration.k8s.io"
      - group: "apiextensions.k8s.io"
      - group: "apiregistration.k8s.io"
      - group: "apps"
      - group: "authentication.k8s.io"
      - group: "authorization.k8s.io"
      - group: "autoscaling"
      - group: "batch"
      - group: "certificates.k8s.io"
      - group: "extensions"
      - group: "metrics"
      - group: "networking.k8s.io"
      - group: "policy"
      - group: "rbac.authorization.k8s.io"
      - group: "storage.k8s.io"'

  cat <<EOF >"${path}"
apiVersion: audit.k8s.io/v1
kind: Policy
rules:
  # The following requests were manually identified as high-volume and low-risk,
  # so drop them.
  - level: None
    users: ["system:kube-proxy"]
    verbs: ["watch"]
    resources:
      - group: "" # core
        resources: ["endpoints", "services", "services/status"]
  - level: None
    # Ingress controller reads 'configmaps/ingress-uid' through the unsecured port.
    # TODO(#46983): Change this to the ingress controller service account.
    users: ["system:unsecured"]
    namespaces: ["kube-system"]
    verbs: ["get"]
    resources:
      - group: "" # core
        resources: ["configmaps"]
  - level: None
    users: ["kubelet"] # legacy kubelet identity
    verbs: ["get"]
    resources:
      - group: "" # core
        resources: ["nodes", "nodes/status"]
  - level: None
    userGroups: ["system:nodes"]
    verbs: ["get"]
    resources:
      - group: "" # core
        resources: ["nodes", "nodes/status"]
  - level: None
    users:
      - system:kube-controller-manager
      - system:kube-scheduler
      - system:serviceaccount:kube-system:endpoint-controller
    verbs: ["get", "update"]
    namespaces: ["kube-system"]
    resources:
      - group: "" # core
        resources: ["endpoints"]
  - level: None
    users: ["system:apiserver"]
    verbs: ["get"]
    resources:
      - group: "" # core
        resources: ["namespaces", "namespaces/status", "namespaces/finalize"]
  # Don't log HPA fetching metrics.
  - level: None
    users:
      - system:kube-controller-manager
    verbs: ["get", "list"]
    resources:
      - group: "metrics"
  # Don't log these read-only URLs.
  - level: None
    nonResourceURLs:
      - /healthz*
      - /version
      - /swagger*
  # Don't log events requests.
  - level: None
    resources:
      - group: "" # core
        resources: ["events"]
  # node and pod status calls from nodes are high-volume and can be large, don't log responses for expected updates from nodes
  - level: Request
    users: ["kubelet", "system:node-problem-detector", "system:serviceaccount:kube-system:node-problem-detector"]
    verbs: ["update","patch"]
    resources:
      - group: "" # core
        resources: ["nodes/status", "pods/status"]
    omitStages:
      - "RequestReceived"
  - level: Request
    userGroups: ["system:nodes"]
    verbs: ["update","patch"]
    resources:
      - group: "" # core
        resources: ["nodes/status", "pods/status"]
    omitStages:
      - "RequestReceived"
  # deletecollection calls can be large, don't log responses for expected namespace deletions
  - level: Request
    users: ["system:serviceaccount:kube-system:namespace-controller"]
    verbs: ["deletecollection"]
    omitStages:
      - "RequestReceived"
  # Secrets, ConfigMaps, TokenRequest and TokenReviews can contain sensitive & binary data,
  # so only log at the Metadata level.
  - level: Metadata
    resources:
      - group: "" # core
        resources: ["secrets", "configmaps", "serviceaccounts/token"]
      - group: authentication.k8s.io
        resources: ["tokenreviews"]
    omitStages:
      - "RequestReceived"
  # Get responses can be large; skip them.
  - level: Request
    verbs: ["get", "list", "watch"]
    resources: ${known_apis}
    omitStages:
      - "RequestReceived"
  # Default level for known APIs
  - level: RequestResponse
    resources: ${known_apis}
    omitStages:
      - "RequestReceived"
  # Default level for all other requests.
  - level: Metadata
    omitStages:
      - "RequestReceived"
EOF
}

# Computes command line arguments to be passed to etcd.
function compute-etcd-params {
	local params="${ETCD_TEST_ARGS:-}"
	params+=" --name=etcd-$(hostname -s)"
	params+=" --listen-peer-urls=http://127.0.0.1:2380"
	params+=" --advertise-client-urls=http://127.0.0.1:2379"
	params+=" --listen-client-urls=http://0.0.0.0:2379"

	# Enable apiserver->etcd auth.
	params+=" --client-cert-auth"
	params+=" --trusted-ca-file /etc/srv/kubernetes/etcd-apiserver-ca.crt"
	params+=" --cert-file /etc/srv/kubernetes/etcd-apiserver-server.crt"
	params+=" --key-file /etc/srv/kubernetes/etcd-apiserver-server.key"

	params+=" --data-dir=/var/etcd/data"
	params+=" ${ETCD_QUOTA_BYTES}"
	echo "${params}"
}

# Computes command line arguments to be passed to etcd-events.
function compute-etcd-events-params {
	local params="${ETCD_TEST_ARGS:-}"
	params+=" --name=etcd-$(hostname -s)"
	params+=" --listen-peer-urls=http://127.0.0.1:2381"
	params+=" --advertise-client-urls=http://127.0.0.1:4002"
	params+=" --listen-client-urls=http://0.0.0.0:4002"
	params+=" --data-dir=/var/etcd/data-events"
	params+=" ${ETCD_QUOTA_BYTES}"
	echo "${params}"
}

# Computes command line arguments to be passed to apiserver.
function compute-kube-apiserver-params {
	local params="--insecure-bind-address=0.0.0.0"
	params+=" --etcd-servers=${ETCD_SERVERS:-http://127.0.0.1:2379}"
	if [[ -z "${ETCD_SERVERS:-}" ]]; then
		params+=" --etcd-servers-overrides=${ETCD_SERVERS_OVERRIDES:-/events#${EVENT_STORE_URL}}"
	elif [[ -n "${ETCD_SERVERS_OVERRIDES:-}" ]]; then
		params+=" --etcd-servers-overrides=${ETCD_SERVERS_OVERRIDES:-}"
	fi
	# Enable apiserver->etcd auth.
	params+=" --etcd-cafile=/etc/srv/kubernetes/etcd-apiserver-ca.crt"
	params+=" --etcd-certfile=/etc/srv/kubernetes/etcd-apiserver-client.crt"
	params+=" --etcd-keyfile=/etc/srv/kubernetes/etcd-apiserver-client.key"

	params+=" --tls-cert-file=/etc/srv/kubernetes/server.cert"
	params+=" --tls-private-key-file=/etc/srv/kubernetes/server.key"
	params+=" --requestheader-client-ca-file=/etc/srv/kubernetes/aggr_ca.crt"
	params+=" --requestheader-allowed-names=aggregator"
	params+=" --requestheader-extra-headers-prefix=X-Remote-Extra-"
	params+=" --requestheader-group-headers=X-Remote-Group"
	params+=" --requestheader-username-headers=X-Remote-User"
	params+=" --proxy-client-cert-file=/etc/srv/kubernetes/proxy_client.crt"
	params+=" --proxy-client-key-file=/etc/srv/kubernetes/proxy_client.key"
	params+=" --enable-aggregator-routing=true"
	params+=" --client-ca-file=/etc/srv/kubernetes/ca.crt"
	params+=" --token-auth-file=/etc/srv/kubernetes/known_tokens.csv"
	params+=" --secure-port=443"
	params+=" --service-cluster-ip-range=${SERVICE_CLUSTER_IP_RANGE}"
	params+=" --admission-control=${CUSTOM_ADMISSION_PLUGINS}"
	params+=" --authorization-mode=Node,RBAC"
	params+=" --allow-privileged=true"
	if [[ -n "${STORAGE_BACKEND:-}" ]]; then
		params+=" --storage-backend=${STORAGE_BACKEND}"
	fi
	if [[ -n "${STORAGE_MEDIA_TYPE:-}" ]]; then
		params+=" --storage-media-type=${STORAGE_MEDIA_TYPE}"
	fi
  if [[ -n "${ETCD_COMPACTION_INTERVAL_SEC:-}" ]]; then
    params+=" --etcd-compaction-interval=${ETCD_COMPACTION_INTERVAL_SEC}s"
  fi
	if [[ -n "${KUBE_APISERVER_REQUEST_TIMEOUT:-}" ]]; then
		params+=" --min-request-timeout=${KUBE_APISERVER_REQUEST_TIMEOUT}"
	fi
	if [[ "${NUM_NODES}" -ge 3000 ]]; then
		params+=" --max-requests-inflight=3000 --max-mutating-requests-inflight=1000"
	elif [[ "${NUM_NODES}" -ge 1000 ]]; then
		params+=" --max-requests-inflight=1500 --max-mutating-requests-inflight=500"
	fi
	if [[ -n "${RUNTIME_CONFIG:-}" ]]; then
		params+=" --runtime-config=${RUNTIME_CONFIG}"
	fi
	if [[ -n "${FEATURE_GATES:-}" ]]; then
		params+=" --feature-gates=${FEATURE_GATES}"
	fi
	if [[ "${ENABLE_APISERVER_ADVANCED_AUDIT:-}" == "true" ]]; then
		# Create the audit policy file, and mount it into the apiserver pod.
		create-master-audit-policy "${audit_policy_file}" "${ADVANCED_AUDIT_POLICY:-}"

		# The config below matches the one in cluster/gce/gci/configure-helper.sh.
		# TODO: Currently supporting just log backend. Support webhook if needed.
		params+=" --audit-policy-file=${audit_policy_file}"
		params+=" --audit-log-path=/var/log/kube-apiserver-audit.log"
		params+=" --audit-log-maxage=0"
		params+=" --audit-log-maxbackup=0"
		params+=" --audit-log-maxsize=2000000000"
	fi
        # Append APISERVER_TEST_ARGS to the end, which will allow for
        # the above defaults to be overridden.
	params+=" ${APISERVER_TEST_ARGS:-}"
	echo "${params}"
}

# Computes command line arguments to be passed to controller-manager.
function compute-kube-controller-manager-params {
	local params="${CONTROLLER_MANAGER_TEST_ARGS:-}"
	params+=" --use-service-account-credentials"
	params+=" --kubeconfig=/etc/srv/kubernetes/kube-controller-manager/kubeconfig"
	params+=" --service-account-private-key-file=/etc/srv/kubernetes/server.key"
	params+=" --root-ca-file=/etc/srv/kubernetes/ca.crt"
	params+=" --allocate-node-cidrs=${ALLOCATE_NODE_CIDRS}"
	params+=" --cluster-cidr=${CLUSTER_IP_RANGE}"
	params+=" --service-cluster-ip-range=${SERVICE_CLUSTER_IP_RANGE}"
	params+=" --terminated-pod-gc-threshold=${TERMINATED_POD_GC_THRESHOLD}"
	echo "${params}"
}

# Computes command line arguments to be passed to scheduler.
function compute-kube-scheduler-params {
	local params="${SCHEDULER_TEST_ARGS:-}"
	params+=" --kubeconfig=/etc/srv/kubernetes/kube-scheduler/kubeconfig"
	echo "${params}"
}

# Computes command line arguments to be passed to addon-manager.
function compute-kube-addon-manager-params {
	echo ""
}

# Start a kubernetes master component '$1' which can be any of the following:
# 1. etcd
# 2. etcd-events
# 3. kube-apiserver
# 4. kube-controller-manager
# 5. kube-scheduler
# 6. kube-addon-manager
#
# It prepares the log file, loads the docker tag, calculates variables, sets them
# in the manifest file, and then copies the manifest file to /etc/kubernetes/manifests.
#
# Assumed vars:
#   DOCKER_REGISTRY
function start-kubemaster-component() {
	echo "Start master component $1"
	local -r component=$1
	prepare-log-file /var/log/"${component}".log
	local -r src_file="${KUBE_ROOT}/${component}.yaml"
	local -r params=$("compute-${component}-params")

	# Evaluate variables.
	sed -i -e "s@{{params}}@${params}@g" "${src_file}"
	sed -i -e "s@{{kube_docker_registry}}@${DOCKER_REGISTRY}@g" "${src_file}"
	sed -i -e "s@{{instance_prefix}}@${INSTANCE_PREFIX}@g" "${src_file}"
	if [ "${component:0:4}" == "etcd" ]; then
		sed -i -e "s@{{etcd_image}}@${ETCD_IMAGE}@g" "${src_file}"
	elif [ "${component}" == "kube-addon-manager" ]; then
		setup-addon-manifests "addons" "kubemark-rbac-bindings"
	else
		local -r component_docker_tag=$(cat "${KUBE_BINDIR}/${component}.docker_tag")
		sed -i -e "s@{{${component}_docker_tag}}@${component_docker_tag}@g" "${src_file}"
		if [ "${component}" == "kube-apiserver" ]; then
			local audit_policy_config_mount=""
			local audit_policy_config_volume=""
			if [[ "${ENABLE_APISERVER_ADVANCED_AUDIT:-}" == "true" ]]; then
				read -r -d '' audit_policy_config_mount << EOF
- name: auditpolicyconfigmount
  mountPath: ${audit_policy_file}
  readOnly: true
EOF
				read -r -d '' audit_policy_config_volume << EOF
- name: auditpolicyconfigmount
  hostPath:
    path: ${audit_policy_file}
    type: FileOrCreate
EOF
			fi
			sed -i -e "s@{{audit_policy_config_mount}}@${audit_policy_config_mount}@g" "${src_file}"
			sed -i -e "s@{{audit_policy_config_volume}}@${audit_policy_config_volume}@g" "${src_file}"
		fi
	fi
	cp "${src_file}" /etc/kubernetes/manifests
}

############################### Main Function ########################################
echo "Start to configure master instance for kubemark"

# Extract files from the server tar and setup master env variables.
cd "${KUBE_ROOT}"
if [[ ! -d "${KUBE_ROOT}/kubernetes" ]]; then
	tar xzf kubernetes-server-linux-amd64.tar.gz
fi
source "${KUBE_ROOT}/kubemark-master-env.sh"

# Setup IP firewall rules, required directory structure and etcd config.
config-ip-firewall
create-dirs
setup-kubelet-dir
delete-default-etcd-configs
compute-etcd-variables

# Setup authentication tokens and kubeconfigs for kube-controller-manager and kube-scheduler,
# only if their kubeconfigs don't already exist as this script could be running on reboot.
if [[ ! -f "${KUBE_ROOT}/k8s_auth_data/kube-controller-manager/kubeconfig" ]]; then
	KUBE_CONTROLLER_MANAGER_TOKEN=$(dd if=/dev/urandom bs=128 count=1 2>/dev/null | base64 | LC_CTYPE=C tr -d "=+/" | dd bs=32 count=1 2>/dev/null)
	echo "${KUBE_CONTROLLER_MANAGER_TOKEN},system:kube-controller-manager,uid:system:kube-controller-manager" >> "${KUBE_ROOT}/k8s_auth_data/known_tokens.csv"
	create-kubecontrollermanager-kubeconfig
fi
if [[ ! -f "${KUBE_ROOT}/k8s_auth_data/kube-scheduler/kubeconfig" ]]; then
	KUBE_SCHEDULER_TOKEN=$(dd if=/dev/urandom bs=128 count=1 2>/dev/null | base64 | LC_CTYPE=C tr -d "=+/" | dd bs=32 count=1 2>/dev/null)
	echo "${KUBE_SCHEDULER_TOKEN},system:kube-scheduler,uid:system:kube-scheduler" >> "${KUBE_ROOT}/k8s_auth_data/known_tokens.csv"
	create-kubescheduler-kubeconfig
fi

ADDON_MANAGER_TOKEN=$(dd if=/dev/urandom bs=128 count=1 2>/dev/null | base64 | LC_CTYPE=C tr -d "=+/" | dd bs=32 count=1 2>/dev/null)
echo "${ADDON_MANAGER_TOKEN},system:addon-manager,admin,system:masters" >> "${KUBE_ROOT}/k8s_auth_data/known_tokens.csv"
create-addonmanager-kubeconfig

# Mount master PD for etcd and create symbolic links to it.
{
	main_etcd_mount_point="/mnt/disks/master-pd"
	mount-pd "google-master-pd" "${main_etcd_mount_point}"
	# Contains all the data stored in etcd.
	mkdir -p "${main_etcd_mount_point}/var/etcd"
	chmod 700 "${main_etcd_mount_point}/var/etcd"
	ln -s -f "${main_etcd_mount_point}/var/etcd" /var/etcd
	mkdir -p /etc/srv
	# Setup the dynamically generated apiserver auth certs and keys to pd.
	mkdir -p "${main_etcd_mount_point}/srv/kubernetes"
	ln -s -f "${main_etcd_mount_point}/srv/kubernetes" /etc/srv/kubernetes
	# Copy the files to the PD only if they don't exist (so we do it only the first time).
	if [[ "$(ls -A ${main_etcd_mount_point}/srv/kubernetes/)" == "" ]]; then
		cp -r "${KUBE_ROOT}"/k8s_auth_data/* "${main_etcd_mount_point}/srv/kubernetes/"
	fi
	# Directory for kube-apiserver to store SSH key (if necessary).
	mkdir -p "${main_etcd_mount_point}/srv/sshproxy"
	ln -s -f "${main_etcd_mount_point}/srv/sshproxy" /etc/srv/sshproxy
}

# Mount master PD for event-etcd (if required) and create symbolic links to it.
{
	EVENT_STORE_IP="${EVENT_STORE_IP:-127.0.0.1}"
	EVENT_STORE_URL="${EVENT_STORE_URL:-http://${EVENT_STORE_IP}:4002}"
	if [ "${EVENT_PD:-}" == "true" ]; then
		event_etcd_mount_point="/mnt/disks/master-event-pd"
		mount-pd "google-master-event-pd" "${event_etcd_mount_point}"
		# Contains all the data stored in event etcd.
		mkdir -p "${event_etcd_mount_point}/var/etcd/events"
		chmod 700 "${event_etcd_mount_point}/var/etcd/events"
		ln -s -f "${event_etcd_mount_point}/var/etcd/events" /var/etcd/events
	fi
}

# Setup docker flags and load images of the master components.
assemble-docker-flags
DOCKER_REGISTRY="registry.k8s.io"
load-docker-images

readonly audit_policy_file="/etc/audit_policy.config"

# Start kubelet as a supervisord process and master components as pods.
start-kubelet
if [[ -z "${ETCD_SERVERS:-}" ]]; then
	start-kubemaster-component "etcd"
	if [ "${EVENT_STORE_IP:-}" == "127.0.0.1" ]; then
		start-kubemaster-component "etcd-events"
	fi
fi
start-kubemaster-component "kube-apiserver"
start-kubemaster-component "kube-controller-manager"
start-kubemaster-component "kube-scheduler"
start-kubemaster-component "kube-addon-manager"

# Wait till apiserver is working fine or timeout.
echo -n "Waiting for apiserver to be healthy"
start=$(date +%s)
until [ "$(curl 127.0.0.1:8080/healthz 2> /dev/null)" == "ok" ]; do
	echo -n "."
	sleep 1
	now=$(date +%s)
	if [ $((now - start)) -gt 300 ]; then
		echo "Timeout!"
		exit 1
	fi
done

echo "Done for the configuration for kubemark master"
