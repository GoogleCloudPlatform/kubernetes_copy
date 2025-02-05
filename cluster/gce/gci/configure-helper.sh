#!/usr/bin/env bash

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

# This script is for configuring kubernetes master and node instances. It is
# uploaded in the manifests tar ball.

# TODO: this script duplicates templating logic from cluster/saltbase/salt
# using sed. It should use an actual template parser on the manifest
# files.

set -o errexit
set -o nounset
set -o pipefail

### Hardcoded constants
METADATA_SERVER_IP="${METADATA_SERVER_IP:-169.254.169.254}"

# Standard curl flags.
CURL_FLAGS='--fail --silent --show-error --retry 5 --retry-delay 3 --connect-timeout 10 --retry-connrefused'

function convert-manifest-params {
  # A helper function to convert the manifest args from a string to a list of
  # flag arguments.
  # Old format:
  #   command=["/bin/sh", "-c", "exec KUBE_EXEC_BINARY --param1=val1 --param2-val2"].
  # New format:
  #   command=["KUBE_EXEC_BINARY"]  # No shell dependencies.
  #   args=["--param1=val1", "--param2-val2"]
  IFS=' ' read -ra FLAGS <<< "$1"
  params=""
  for flag in "${FLAGS[@]}"; do
    params+="\n\"$flag\","
  done
  if [ -n "$params" ]; then
    echo "${params::-1}"  #  drop trailing comma
  fi
}

function append-param-if-not-present {
  # A helper function to add flag to an arguments string
  # if no such flag is present already
  local params="$1"
  local -r flag="$2"
  local -r value="$3"
  if [[ ! "${params}" =~ "--${flag}"[=\ ] ]]; then
    params+=" --${flag}=${value}"
  fi
  echo "${params}"
}

function setup-os-params {
  # Reset core_pattern. On GCI, the default core_pattern pipes the core dumps to
  # /sbin/crash_reporter which is more restrictive in saving crash dumps. So for
  # now, set a generic core_pattern that users can work with.
  echo "/core.%e.%p.%t" > /proc/sys/kernel/core_pattern
}

# secure_random generates a secure random string of bytes. This function accepts
# a number of secure bytes desired and returns a base64 encoded string with at
# least the requested entropy. Rather than directly reading from /dev/urandom,
# we use uuidgen which calls getrandom(2). getrandom(2) verifies that the
# entropy pool has been initialized sufficiently for the desired operation
# before reading from /dev/urandom.
#
# ARGS:
#   #1: number of secure bytes to generate. We round up to the nearest factor of 32.
function secure_random {
  local infobytes="${1}"
  if ((infobytes <= 0)); then
    echo "Invalid argument to secure_random: infobytes='${infobytes}'" 1>&2
    return 1
  fi

  local out=""
  for (( i = 0; i < "${infobytes}"; i += 32 )); do
    # uuids have 122 random bits, sha256 sums have 256 bits, so concatenate
    # three uuids and take their sum. The sum is encoded in ASCII hex, hence the
    # 64 character cut.
    out+="$(
     (
       uuidgen --random;
       uuidgen --random;
       uuidgen --random;
     ) | sha256sum \
       | head -c 64
    )";
  done
  # Finally, convert the ASCII hex to base64 to increase the density.
  echo -n "${out}" | xxd -r -p | base64 -w 0
}

# Helper for configuring iptables rules for metadata server.
#
# $1 is the command flag (-I or -D).
# $2 is the firewall action (LOG or REJECT).
# $3 is the prefix for log output.
# $4 is "!" to optionally invert the uid range.
function gce-metadata-fw-helper {
  local -r command="$1"
  local action="$2"
  local -r prefix="$3"
  local -r invert="${4:-}"

  # Expand rule action to include relevant option flags.
  case "${action}" in
    LOG)
      action="LOG --log-prefix "${prefix}:" --log-uid --log-tcp-options --log-ip-option"
      ;;
  esac

  # Deliberately allow word split here
  # shellcheck disable=SC2086
  iptables -w ${command} OUTPUT -p tcp --dport 80 -d ${METADATA_SERVER_IP} -m owner ${invert:-} --uid-owner=${METADATA_SERVER_ALLOWED_UID_RANGE:-0-2999} -j ${action}
}

# WARNING: DO NOT USE THE FILTER TABLE! Some implementations of network policy
# think they own it and will stomp all over your changes. At this time, the
# mangle table is less contentious so use that if possible.
function config-ip-firewall {
  echo "Configuring IP firewall rules"

  # Do not consider loopback addresses as martian source or destination while
  # routing. This enables the use of 127/8 for local routing purposes.
  sysctl -w net.ipv4.conf.all.route_localnet=1

  # The GCI image has host firewall which drop most inbound/forwarded packets.
  # We need to add rules to accept all TCP/UDP/ICMP/SCTP packets.
  if iptables -w -L INPUT | grep "Chain INPUT (policy DROP)" > /dev/null; then
    echo "Add rules to accept all inbound TCP/UDP/ICMP packets"
    iptables -w -A INPUT -w -p TCP -j ACCEPT
    iptables -w -A INPUT -w -p UDP -j ACCEPT
    iptables -w -A INPUT -w -p ICMP -j ACCEPT
    iptables -w -A INPUT -w -p SCTP -j ACCEPT
  fi
  if iptables -w -L FORWARD | grep "Chain FORWARD (policy DROP)" > /dev/null; then
    echo "Add rules to accept all forwarded TCP/UDP/ICMP/SCTP packets"
    iptables -w -A FORWARD -w -p TCP -j ACCEPT
    iptables -w -A FORWARD -w -p UDP -j ACCEPT
    iptables -w -A FORWARD -w -p ICMP -j ACCEPT
    iptables -w -A FORWARD -w -p SCTP -j ACCEPT
  fi

  # Flush iptables nat table
  iptables -w -t nat -F || true

  if [[ "${NON_MASQUERADE_CIDR:-}" == "0.0.0.0/0" ]]; then
    echo "Add rules for ip masquerade"
    iptables -w -t nat -N IP-MASQ
    iptables -w -t nat -A POSTROUTING -m comment --comment "ip-masq: ensure nat POSTROUTING directs all non-LOCAL destination traffic to our custom IP-MASQ chain" -m addrtype ! --dst-type LOCAL -j IP-MASQ
    iptables -w -t nat -A IP-MASQ -d 169.254.0.0/16 -m comment --comment "ip-masq: local traffic is not subject to MASQUERADE" -j RETURN
    iptables -w -t nat -A IP-MASQ -d 10.0.0.0/8 -m comment --comment "ip-masq: RFC 1918 reserved range is not subject to MASQUERADE" -j RETURN
    iptables -w -t nat -A IP-MASQ -d 172.16.0.0/12 -m comment --comment "ip-masq: RFC 1918 reserved range is not subject to MASQUERADE" -j RETURN
    iptables -w -t nat -A IP-MASQ -d 192.168.0.0/16 -m comment --comment "ip-masq: RFC 1918 reserved range is not subject to MASQUERADE" -j RETURN
    iptables -w -t nat -A IP-MASQ -d 240.0.0.0/4 -m comment --comment "ip-masq: RFC 5735 reserved range is not subject to MASQUERADE" -j RETURN
    iptables -w -t nat -A IP-MASQ -d 192.0.2.0/24 -m comment --comment "ip-masq: RFC 5737 reserved range is not subject to MASQUERADE" -j RETURN
    iptables -w -t nat -A IP-MASQ -d 198.51.100.0/24 -m comment --comment "ip-masq: RFC 5737 reserved range is not subject to MASQUERADE" -j RETURN
    iptables -w -t nat -A IP-MASQ -d 203.0.113.0/24 -m comment --comment "ip-masq: RFC 5737 reserved range is not subject to MASQUERADE" -j RETURN
    iptables -w -t nat -A IP-MASQ -d 100.64.0.0/10 -m comment --comment "ip-masq: RFC 6598 reserved range is not subject to MASQUERADE" -j RETURN
    iptables -w -t nat -A IP-MASQ -d 198.18.0.0/15 -m comment --comment "ip-masq: RFC 6815 reserved range is not subject to MASQUERADE" -j RETURN
    iptables -w -t nat -A IP-MASQ -d 192.0.0.0/24 -m comment --comment "ip-masq: RFC 6890 reserved range is not subject to MASQUERADE" -j RETURN
    iptables -w -t nat -A IP-MASQ -d 192.88.99.0/24 -m comment --comment "ip-masq: RFC 7526 reserved range is not subject to MASQUERADE" -j RETURN
    iptables -w -t nat -A IP-MASQ -m comment --comment "ip-masq: outbound traffic is subject to MASQUERADE (must be last in chain)" -j MASQUERADE
  fi

  # If METADATA_CONCEALMENT_NO_FIREWALL is set, don't create a firewall on this
  # node because we don't expect the daemonset to run on this node.
  if [[ "${ENABLE_METADATA_CONCEALMENT:-}" == "true" ]] && [[ ! "${METADATA_CONCEALMENT_NO_FIREWALL:-}" == "true" ]]; then
    echo "Add rule for metadata concealment"
    ip addr add dev lo 169.254.169.252/32 scope host
    iptables -w -t nat -I PREROUTING -p tcp ! -i eth0 -d "${METADATA_SERVER_IP}" --dport 80 -m comment --comment "metadata-concealment: bridge traffic to metadata server goes to metadata proxy" -j DNAT --to-destination 169.254.169.252:988
    iptables -w -t nat -I PREROUTING -p tcp ! -i eth0 -d "${METADATA_SERVER_IP}" --dport 8080 -m comment --comment "metadata-concealment: bridge traffic to metadata server goes to metadata proxy" -j DNAT --to-destination 169.254.169.252:987
  fi
  iptables -w -t mangle -I OUTPUT -s 169.254.169.254 -j DROP

  # Log all metadata access not from approved processes.
  case "${METADATA_SERVER_FIREWALL_MODE:-off}" in
    log)
      echo "Installing metadata firewall logging rules"
      gce-metadata-fw-helper -I LOG "MetadataServerFirewallReject" !
      gce-metadata-fw-helper -I LOG "MetadataServerFirewallAccept"
      ;;
  esac
}

function create-dirs {
  echo "Creating required directories"
  mkdir -p /var/lib/kubelet
  mkdir -p /etc/kubernetes/manifests
  if [[ "${KUBERNETES_MASTER:-}" == "false" ]]; then
    mkdir -p /var/lib/kube-proxy
  fi
}

# Gets the total number of $(1) and $(2) type disks specified
# by the user in ${NODE_LOCAL_SSDS_EXT}
function get-local-disk-num() {
  local interface="${1}"
  local format="${2}"

  localdisknum=0
  if [[ -n "${NODE_LOCAL_SSDS_EXT:-}" ]]; then
    IFS=";" read -r -a ssdgroups <<< "${NODE_LOCAL_SSDS_EXT:-}"
    for ssdgroup in "${ssdgroups[@]}"; do
      IFS="," read -r -a ssdopts <<< "${ssdgroup}"
      local opnum="${ssdopts[0]}"
      local opinterface="${ssdopts[1]}"
      local opformat="${ssdopts[2]}"

      if [[ "${opformat,,}" == "${format,,}" && "${opinterface,,}" == "${interface,,}" ]]; then
        localdisknum=$((localdisknum+opnum))
      fi
    done
  fi
}

# Creates a symlink for a ($1) so that it may be used as block storage
function safe-block-symlink(){
  local device="${1}"
  local symdir="${2}"

  mkdir -p "${symdir}"

  get-or-generate-uuid "${device}"
  local myuuid="${retuuid}"

  local sym="${symdir}/local-ssd-${myuuid}"
  # Do not "mkdir -p ${sym}" as that will cause unintended symlink behavior
  ln -s "${device}" "${sym}"
  echo "Created a symlink for SSD $ssd at ${sym}"
  chmod a+w "${sym}"
}

# Gets a pregenerated UUID from ${ssdmap} if it exists, otherwise generates a new
# UUID and places it inside ${ssdmap}
function get-or-generate-uuid(){
  local device="${1}"

  local ssdmap="/home/kubernetes/localssdmap.txt"
  echo "Generating or getting UUID from ${ssdmap}"

  if [[ ! -e "${ssdmap}" ]]; then
    touch "${ssdmap}"
    chmod +w "${ssdmap}"
  fi

  # each line of the ssdmap looks like "${device} persistent-uuid"
  local myuuid
  if grep -q "${device}" "${ssdmap}"; then
    #create symlink based on saved uuid
    myuuid=$(grep "${device}" "${ssdmap}" | cut -d ' ' -f 2)
  else
    # generate new uuid and add it to the map
    if ! myuuid=$(uuidgen); then
      echo "Failed to generate valid UUID with uuidgen" >&2
      exit 2
    fi
    echo "${device} ${myuuid}" >> "${ssdmap}"
  fi

  if [[ -z "${myuuid}" ]]; then
    echo "Failed to get a uuid for device ${device} when symlinking." >&2
    exit 2
  fi

  retuuid="${myuuid}"
}

#Formats the given device ($1) if needed and mounts it at given mount point
# ($2).
function safe-format-and-mount() {
  local device
  local mountpoint
  device="$1"
  mountpoint="$2"

  # Format only if the disk is not already formatted.
  if ! tune2fs -l "${device}" ; then
    echo "Formatting '${device}'"
    mkfs.ext4 -F "${device}"
  fi

  mkdir -p "${mountpoint}"
  echo "Mounting '${device}' at '${mountpoint}'"
  mount -o discard,defaults "${device}" "${mountpoint}"
  chmod a+w "${mountpoint}"
}

# Gets a devices UUID and bind mounts the device to mount location in
# /mnt/disks/by-id/
function unique-uuid-bind-mount(){
  local mountpoint
  local actual_device
  mountpoint="$1"
  actual_device="$2"

  # Trigger udev refresh so that newly formatted devices are propagated in by-uuid
  udevadm control --reload-rules
  udevadm trigger
  udevadm settle

  # find uuid for actual_device
  local myuuid
  myuuid=$(find -L /dev/disk/by-uuid -maxdepth 1 -samefile /dev/"${actual_device}" -printf '%P')
  # myuuid should be the uuid of the device as found in /dev/disk/by-uuid/
  if [[ -z "${myuuid}" ]]; then
    echo "Failed to get a uuid for device ${actual_device} when mounting." >&2
    exit 2
  fi

  # bindpoint should be the full path of the to-be-bound device
  local bindpoint="${UUID_MNT_PREFIX}-${interface}-fs/local-ssd-${myuuid}"

  safe-bind-mount "${mountpoint}" "${bindpoint}"
}

# Bind mounts device at mountpoint to bindpoint
function safe-bind-mount(){
  local mountpoint="${1}"
  local bindpoint="${2}"

  # Mount device to the mountpoint
  mkdir -p "${bindpoint}"
  echo "Binding '${mountpoint}' at '${bindpoint}'"
  mount --bind "${mountpoint}" "${bindpoint}"
  chmod a+w "${bindpoint}"
}


# Mounts, bindmounts, or symlinks depending on the interface and format
# of the incoming device
function mount-ext(){
  local ssd="${1}"
  local devicenum="${2}"
  local interface="${3}"
  local format="${4}"


  if [[ -z "${devicenum}" ]]; then
    echo "Failed to get the local disk number for device ${ssd}" >&2
    exit 2
  fi

  # TODO: Handle partitioned disks. Right now this code just ignores partitions
  if [[ "${format}" == "fs" ]]; then
    if [[ "${interface}" == "scsi" ]]; then
      local actual_device
      actual_device=$(readlink -f "${ssd}" | cut -d '/' -f 3)
      # Error checking
      if [[ "${actual_device}" != sd* ]]; then
        echo "'actual_device' is not of the correct format. It must be the kernel name of the device, got ${actual_device} instead" >&2
        exit 1
      fi
      local mountpoint="/mnt/disks/ssd${devicenum}"
    else
      # This path is required because the existing Google images do not
      # expose NVMe devices in /dev/disk/by-id so we are using the /dev/nvme instead
      local actual_device
      actual_device=$(echo "${ssd}" | cut -d '/' -f 3)
      # Error checking
      if [[ "${actual_device}" != nvme* ]]; then
        echo "'actual_device' is not of the correct format. It must be the kernel name of the device, got ${actual_device} instead" >&2
        exit 1
      fi
      local mountpoint="/mnt/disks/ssd-nvme${devicenum}"
    fi

    safe-format-and-mount "${ssd}" "${mountpoint}"
    # We only do the bindmount if users are using the new local ssd request method
    # see https://github.com/kubernetes/kubernetes/pull/53466#discussion_r146431894
    if [[ -n "${NODE_LOCAL_SSDS_EXT:-}" ]]; then
      unique-uuid-bind-mount "${mountpoint}" "${actual_device}"
    fi
  elif [[ "${format}" == "block" ]]; then
    local symdir="${UUID_BLOCK_PREFIX}-${interface}-block"
    safe-block-symlink "${ssd}" "${symdir}"
  else
    echo "Disk format must be either fs or block, got ${format}"
  fi
}

# Local ssds, if present, are mounted or symlinked to their appropriate
# locations
function ensure-local-ssds() {
  if [ "${NODE_LOCAL_SSDS_EPHEMERAL:-false}" == "true" ]; then
    ensure-local-ssds-ephemeral-storage
    return
  fi
  get-local-disk-num "scsi" "block"
  local scsiblocknum="${localdisknum}"
  local i=0
  for ssd in /dev/disk/by-id/google-local-ssd-*; do
    if [ -e "${ssd}" ]; then
      local devicenum
      devicenum=$(echo "${ssd}" | sed -e 's/\/dev\/disk\/by-id\/google-local-ssd-\([0-9]*\)/\1/')
      if [[ "${i}" -lt "${scsiblocknum}" ]]; then
        mount-ext "${ssd}" "${devicenum}" "scsi" "block"
      else
        # GKE does not set NODE_LOCAL_SSDS so all non-block devices
        # are assumed to be filesystem devices
        mount-ext "${ssd}" "${devicenum}" "scsi" "fs"
      fi
      i=$((i+1))
    else
      echo "No local SCSI SSD disks found."
    fi
  done

  # The following mounts or symlinks NVMe devices
  get-local-disk-num "nvme" "block"
  local nvmeblocknum="${localdisknum}"
  get-local-disk-num "nvme" "fs"
  local nvmefsnum="${localdisknum}"
  # Check if NVMe SSD specified.
  if [ "${nvmeblocknum}" -eq "0" ] && [ "${nvmefsnum}" -eq "0" ]; then
    echo "No local NVMe SSD specified."
    return
  fi
  local i=0
  for ssd in /dev/nvme*; do
    if [ -e "${ssd}" ]; then
      # This workaround to find if the NVMe device is a disk is required because
      # the existing Google images does not expose NVMe devices in /dev/disk/by-id
      if [[ $(udevadm info --query=property --name="${ssd}" | grep DEVTYPE | sed "s/DEVTYPE=//") == "disk" ]]; then
        # shellcheck disable=SC2155
        local devicenum=$(echo "${ssd}" | sed -e 's/\/dev\/nvme0n\([0-9]*\)/\1/')
        if [[ "${i}" -lt "${nvmeblocknum}" ]]; then
          mount-ext "${ssd}" "${devicenum}" "nvme" "block"
        else
          mount-ext "${ssd}" "${devicenum}" "nvme" "fs"
        fi
        i=$((i+1))
      fi
    else
      echo "No local NVMe SSD disks found."
    fi
  done
}

# Local SSDs, if present, are used in a single RAID 0 array and directories that
# back ephemeral storage are mounted on them (kubelet root, container runtime
# root and pod logs).
function ensure-local-ssds-ephemeral-storage() {
  local devices=()
  # Get nvme devices
  for ssd in /dev/nvme*n*; do
    if [ -e "${ssd}" ]; then
      # This workaround to find if the NVMe device is a local SSD is required
      # because the existing Google images does not them in /dev/disk/by-id
      if [[ "$(lsblk -o MODEL -dn "${ssd}")" == "nvme_card" ]]; then
        devices+=("${ssd}")
      fi
    fi
  done
  if [ "${#devices[@]}" -eq 0 ]; then
    echo "No local NVMe SSD disks found."
    return
  fi

  local device="${devices[0]}"
  if [ "${#devices[@]}" -ne 1 ]; then
    seen_arrays=(/dev/md/*)
    device=${seen_arrays[0]}
    echo "Setting RAID array with local SSDs on device ${device}"
    if [ ! -e "$device" ]; then
      device="/dev/md/0"
      echo "y" | mdadm --create "${device}" --level=0 --raid-devices=${#devices[@]} "${devices[@]}"
    fi
  fi

  local ephemeral_mountpoint="/mnt/stateful_partition/kube-ephemeral-ssd"
  safe-format-and-mount "${device}" "${ephemeral_mountpoint}"

  # mount container runtime root dir on SSD
  local container_runtime_name="${CONTAINER_RUNTIME_NAME:-containerd}"
  systemctl stop "$container_runtime_name"
  # Some images remount the container runtime root dir.
  umount "/var/lib/${container_runtime_name}" || true
  # Move the container runtime's directory to the new location to preserve
  # preloaded images.
  if [ ! -d "${ephemeral_mountpoint}/${container_runtime_name}" ]; then
    mv "/var/lib/${container_runtime_name}" "${ephemeral_mountpoint}/${container_runtime_name}"
  fi
  safe-bind-mount "${ephemeral_mountpoint}/${container_runtime_name}" "/var/lib/${container_runtime_name}"
  systemctl start "$container_runtime_name"

  # mount kubelet root dir on SSD
  mkdir -p "${ephemeral_mountpoint}/kubelet"
  safe-bind-mount "${ephemeral_mountpoint}/kubelet" "/var/lib/kubelet"

  # mount pod logs root dir on SSD
  mkdir -p "${ephemeral_mountpoint}/log_pods"
  safe-bind-mount "${ephemeral_mountpoint}/log_pods" "/var/log/pods"
}

# set journald configuration
function setup-journald() {
  if [[ "${SET_JOURNALD_CONFIGURATION:-true}" = "true" ]]; then
  cat <<EOF > /etc/systemd/journald.conf
[Journal]
Storage=persistent
SystemMaxUse=1G
SystemMaxFileSize=100M
RuntimeMaxUse=100M
EOF
    systemctl restart systemd-journald.service
  fi
}

# Installs logrotate configuration files
function setup-logrotate() {
  mkdir -p /etc/logrotate.d/

  if [[ "${ENABLE_LOGROTATE_FILES:-true}" = "true" ]]; then
    # Configure log rotation for all logs in /var/log, which is where k8s services
    # are configured to write their log files. Whenever logrotate is ran, this
    # config will:
    # * rotate the log file if its size is > 100Mb OR if one day has elapsed
    # * save rotated logs into a gzipped timestamped backup
    # * log file timestamp (controlled by 'dateformat') includes seconds too. This
    #   ensures that logrotate can generate unique logfiles during each rotation
    #   (otherwise it skips rotation if 'maxsize' is reached multiple times in a
    #   day).
    # * keep only 5 old (rotated) logs, and will discard older logs.
    cat > /etc/logrotate.d/allvarlogs <<EOF
/var/log/*.log {
    rotate ${LOGROTATE_FILES_MAX_COUNT:-5}
    copytruncate
    missingok
    notifempty
    compress
    maxsize ${LOGROTATE_MAX_SIZE:-100M}
    daily
    dateext
    dateformat -%Y%m%d-%s
    create 0644 root root
}
EOF
  fi

  if [[ "${ENABLE_POD_LOG:-false}" = "true" ]]; then
    # Configure log rotation for pod logs in /var/log/pods/NAMESPACE_NAME_UID.
    cat > /etc/logrotate.d/allpodlogs <<EOF
/var/log/pods/*/*.log {
    rotate ${POD_LOG_MAX_FILE:-5}
    copytruncate
    missingok
    notifempty
    compress
    maxsize ${POD_LOG_MAX_SIZE:-5M}
    daily
    dateext
    dateformat -%Y%m%d-%s
    create 0644 root root
}
EOF
  fi
}

# Finds the master PD device; returns it in MASTER_PD_DEVICE
function find-master-pd {
  MASTER_PD_DEVICE=""
  if [[ ! -e /dev/disk/by-id/google-master-pd ]]; then
    return
  fi
  device_info=$(ls -l /dev/disk/by-id/google-master-pd)
  relative_path=${device_info##* }
  MASTER_PD_DEVICE="/dev/disk/by-id/${relative_path}"
}

# Mounts a persistent disk (formatting if needed) to store the persistent data
# on the master -- etcd's data, a few settings, and security certs/keys/tokens.
# safe-format-and-mount only formats an unformatted disk, and mkdir -p will
# leave a directory be if it already exists.
function mount-master-pd {
  find-master-pd
  if [[ -z "${MASTER_PD_DEVICE:-}" ]]; then
    return
  fi

  echo "Mounting master-pd"
  local -r pd_path="/dev/disk/by-id/google-master-pd"
  local -r mount_point="/mnt/disks/master-pd"
  # Format and mount the disk, create directories on it for all of the master's
  # persistent data, and link them to where they're used.
  mkdir -p "${mount_point}"
  safe-format-and-mount "${pd_path}" "${mount_point}"
  echo "Mounted master-pd '${pd_path}' at '${mount_point}'"

  # NOTE: These locations on the PD store persistent data, so to maintain
  # upgradeability, these locations should not change.  If they do, take care
  # to maintain a migration path from these locations to whatever new
  # locations.

  # Contains all the data stored in etcd.
  mkdir -p "${mount_point}/var/etcd"
  chmod 700 "${mount_point}/var/etcd"
  ln -s -f "${mount_point}/var/etcd" /var/etcd
  mkdir -p /etc/srv
  # Contains the dynamically generated apiserver auth certs and keys.
  mkdir -p "${mount_point}/srv/kubernetes"
  ln -s -f "${mount_point}/srv/kubernetes" /etc/srv/kubernetes
  # Directory for kube-apiserver to store SSH key (if necessary).
  mkdir -p "${mount_point}/srv/sshproxy"
  ln -s -f "${mount_point}/srv/sshproxy" /etc/srv/sshproxy

  chown -R etcd "${mount_point}/var/etcd"
  chgrp -R etcd "${mount_point}/var/etcd"
}

# append_or_replace_prefixed_line ensures:
# 1. the specified file exists
# 2. existing lines with the specified ${prefix} are removed
# 3. a new line with the specified ${prefix}${suffix} is appended
function append_or_replace_prefixed_line {
  local -r file="${1:-}"
  local -r prefix="${2:-}"
  local -r suffix="${3:-}"
  local -r dirname=$(dirname "${file}")
  local -r tmpfile=$(mktemp "${dirname}/filtered.XXXX")

  touch "${file}"
  awk -v pfx="${prefix}" 'substr($0,1,length(pfx)) != pfx { print }' "${file}" > "${tmpfile}"
  echo "${prefix}${suffix}" >> "${tmpfile}"
  mv "${tmpfile}" "${file}"
}

function write-pki-data {
  local data="${1}"
  local path="${2}"
  # remove the path if it exists
  rm -f "${path}"
  if [[ -n "${KUBE_PKI_READERS_GROUP:-}" ]]; then
    (umask 027; echo "${data}" | base64 --decode > "${path}")
    chgrp "${KUBE_PKI_READERS_GROUP:-}" "${path}"
    chmod g+r "${path}"
  else
    (umask 077; echo "${data}" | base64 --decode > "${path}")
  fi
}

function create-node-pki {
  echo "Creating node pki files"

  local -r pki_dir="/etc/srv/kubernetes/pki"
  mkdir -p "${pki_dir}"

  if [[ -z "${CA_CERT_BUNDLE:-}" ]]; then
    CA_CERT_BUNDLE="${CA_CERT}"
  fi

  CA_CERT_BUNDLE_PATH="${pki_dir}/ca-certificates.crt"
  write-pki-data "${CA_CERT_BUNDLE}" "${CA_CERT_BUNDLE_PATH}"

  if [[ -n "${KUBELET_CERT:-}" && -n "${KUBELET_KEY:-}" ]]; then
    KUBELET_CERT_PATH="${pki_dir}/kubelet.crt"
    write-pki-data "${KUBELET_CERT}" "${KUBELET_CERT_PATH}"

    KUBELET_KEY_PATH="${pki_dir}/kubelet.key"
    write-pki-data "${KUBELET_KEY}" "${KUBELET_KEY_PATH}"
  fi

  if [[ "${KONNECTIVITY_SERVICE_PROXY_PROTOCOL_MODE:-grpc}" == 'http-connect' ]]; then
    mkdir -p "${pki_dir}/konnectivity-agent"
    KONNECTIVITY_AGENT_CA_CERT_PATH="${pki_dir}/konnectivity-agent/ca.crt"
    KONNECTIVITY_AGENT_CLIENT_KEY_PATH="${pki_dir}/konnectivity-agent/client.key"
    KONNECTIVITY_AGENT_CLIENT_CERT_PATH="${pki_dir}/konnectivity-agent/client.crt"
    write-pki-data "${KONNECTIVITY_AGENT_CA_CERT}" "${KONNECTIVITY_AGENT_CA_CERT_PATH}"
    write-pki-data "${KONNECTIVITY_AGENT_CLIENT_KEY}" "${KONNECTIVITY_AGENT_CLIENT_KEY_PATH}"
    write-pki-data "${KONNECTIVITY_AGENT_CLIENT_CERT}" "${KONNECTIVITY_AGENT_CLIENT_CERT_PATH}"
  fi
}

function create-master-pki {
  echo "Creating master pki files"

  local -r pki_dir="/etc/srv/kubernetes/pki"
  mkdir -p "${pki_dir}"

  CA_CERT_PATH="${pki_dir}/ca.crt"
  write-pki-data "${CA_CERT}" "${CA_CERT_PATH}"

  # this is not true on GKE
  if [[ -n "${CA_KEY:-}" ]]; then
    CA_KEY_PATH="${pki_dir}/ca.key"
    write-pki-data "${CA_KEY}" "${CA_KEY_PATH}"
  fi

  if [[ -z "${APISERVER_SERVER_CERT:-}" || -z "${APISERVER_SERVER_KEY:-}" ]]; then
    APISERVER_SERVER_CERT="${MASTER_CERT}"
    APISERVER_SERVER_KEY="${MASTER_KEY}"
  fi

  APISERVER_SERVER_CERT_PATH="${pki_dir}/apiserver.crt"
  write-pki-data "${APISERVER_SERVER_CERT}" "${APISERVER_SERVER_CERT_PATH}"

  APISERVER_SERVER_KEY_PATH="${pki_dir}/apiserver.key"
  write-pki-data "${APISERVER_SERVER_KEY}" "${APISERVER_SERVER_KEY_PATH}"

  if [[ -z "${APISERVER_CLIENT_CERT:-}" || -z "${APISERVER_CLIENT_KEY:-}" ]]; then
    APISERVER_CLIENT_CERT="${KUBEAPISERVER_CERT}"
    APISERVER_CLIENT_KEY="${KUBEAPISERVER_KEY}"
  fi

  APISERVER_CLIENT_CERT_PATH="${pki_dir}/apiserver-client.crt"
  write-pki-data "${APISERVER_CLIENT_CERT}" "${APISERVER_CLIENT_CERT_PATH}"

  APISERVER_CLIENT_KEY_PATH="${pki_dir}/apiserver-client.key"
  write-pki-data "${APISERVER_CLIENT_KEY}" "${APISERVER_CLIENT_KEY_PATH}"

  if [[ -z "${SERVICEACCOUNT_CERT:-}" || -z "${SERVICEACCOUNT_KEY:-}" ]]; then
    SERVICEACCOUNT_CERT="${MASTER_CERT}"
    SERVICEACCOUNT_KEY="${MASTER_KEY}"
  fi

  if [[ -n "${OLD_MASTER_CERT:-}" && -n "${OLD_MASTER_KEY:-}" ]]; then
    OLD_MASTER_CERT_PATH="${pki_dir}/oldapiserver.crt"
    echo "${OLD_MASTER_CERT}" | base64 --decode > "${OLD_MASTER_CERT_PATH}"
    OLD_MASTER_KEY_PATH="${pki_dir}/oldapiserver.key"
    echo "${OLD_MASTER_KEY}" | base64 --decode > "${OLD_MASTER_KEY_PATH}"
  fi

  SERVICEACCOUNT_CERT_PATH="${pki_dir}/serviceaccount.crt"
  write-pki-data "${SERVICEACCOUNT_CERT}" "${SERVICEACCOUNT_CERT_PATH}"

  SERVICEACCOUNT_KEY_PATH="${pki_dir}/serviceaccount.key"
  write-pki-data "${SERVICEACCOUNT_KEY}" "${SERVICEACCOUNT_KEY_PATH}"

  if [[ -n "${REQUESTHEADER_CA_CERT:-}" ]]; then
    REQUESTHEADER_CA_CERT_PATH="${pki_dir}/aggr_ca.crt"
    write-pki-data "${REQUESTHEADER_CA_CERT}" "${REQUESTHEADER_CA_CERT_PATH}"

    PROXY_CLIENT_KEY_PATH="${pki_dir}/proxy_client.key"
    write-pki-data "${PROXY_CLIENT_KEY}" "${PROXY_CLIENT_KEY_PATH}"

    PROXY_CLIENT_CERT_PATH="${pki_dir}/proxy_client.crt"
    write-pki-data "${PROXY_CLIENT_CERT}" "${PROXY_CLIENT_CERT_PATH}"
  fi

  if [[ -n "${KONNECTIVITY_SERVER_CA_CERT:-}" ]]; then
    mkdir -p "${pki_dir}"/konnectivity-server
    KONNECTIVITY_SERVER_CA_CERT_PATH="${pki_dir}/konnectivity-server/ca.crt"
    write-pki-data "${KONNECTIVITY_SERVER_CA_CERT}" "${KONNECTIVITY_SERVER_CA_CERT_PATH}"

    KONNECTIVITY_SERVER_KEY_PATH="${pki_dir}/konnectivity-server/server.key"
    write-pki-data "${KONNECTIVITY_SERVER_KEY}" "${KONNECTIVITY_SERVER_KEY_PATH}"

    KONNECTIVITY_SERVER_CERT_PATH="${pki_dir}/konnectivity-server/server.crt"
    write-pki-data "${KONNECTIVITY_SERVER_CERT}" "${KONNECTIVITY_SERVER_CERT_PATH}"

    KONNECTIVITY_SERVER_CLIENT_KEY_PATH="${pki_dir}/konnectivity-server/client.key"
    write-pki-data "${KONNECTIVITY_SERVER_CLIENT_KEY}" "${KONNECTIVITY_SERVER_CLIENT_KEY_PATH}"

    KONNECTIVITY_SERVER_CLIENT_CERT_PATH="${pki_dir}/konnectivity-server/client.crt"
    write-pki-data "${KONNECTIVITY_SERVER_CLIENT_CERT}" "${KONNECTIVITY_SERVER_CLIENT_CERT_PATH}"
  fi

  if [[ -n "${KONNECTIVITY_AGENT_CA_CERT:-}" ]]; then
    mkdir -p "${pki_dir}"/konnectivity-agent
    KONNECTIVITY_AGENT_CA_KEY_PATH="${pki_dir}/konnectivity-agent/ca.key"
    write-pki-data "${KONNECTIVITY_AGENT_CA_KEY}" "${KONNECTIVITY_AGENT_CA_KEY_PATH}"

    KONNECTIVITY_AGENT_CA_CERT_PATH="${pki_dir}/konnectivity-agent/ca.crt"
    write-pki-data "${KONNECTIVITY_AGENT_CA_CERT}" "${KONNECTIVITY_AGENT_CA_CERT_PATH}"

    KONNECTIVITY_AGENT_KEY_PATH="${pki_dir}/konnectivity-agent/server.key"
    write-pki-data "${KONNECTIVITY_AGENT_KEY}" "${KONNECTIVITY_AGENT_KEY_PATH}"

    KONNECTIVITY_AGENT_CERT_PATH="${pki_dir}/konnectivity-agent/server.crt"
    write-pki-data "${KONNECTIVITY_AGENT_CERT}" "${KONNECTIVITY_AGENT_CERT_PATH}"
  fi

  if [[ -n "${CLOUD_PVL_ADMISSION_CA_CERT:-}" ]]; then
    mkdir -p "${pki_dir}"/cloud-pvl-admission
    CLOUD_PVL_ADMISSION_CA_CERT_PATH="${pki_dir}/cloud-pvl-admission/ca.crt"
    write-pki-data "${CLOUD_PVL_ADMISSION_CA_CERT}" "${CLOUD_PVL_ADMISSION_CA_CERT_PATH}"

    CLOUD_PVL_ADMISSION_KEY_PATH="${pki_dir}/cloud-pvl-admission/server.key"
    write-pki-data "${CLOUD_PVL_ADMISSION_KEY}" "${CLOUD_PVL_ADMISSION_KEY_PATH}"

    CLOUD_PVL_ADMISSION_CERT_PATH="${pki_dir}/cloud-pvl-admission/server.crt"
    write-pki-data "${CLOUD_PVL_ADMISSION_CERT}" "${CLOUD_PVL_ADMISSION_CERT_PATH}"
  fi
}

# After the first boot and on upgrade, these files exist on the master-pd
# and should never be touched again (except perhaps an additional service
# account, see NB below.) One exception is if METADATA_CLOBBERS_CONFIG is
# enabled.
function create-master-auth {
  echo "Creating master auth files"
  local -r auth_dir="/etc/srv/kubernetes"
  local -r known_tokens_csv="${auth_dir}/known_tokens.csv"
  if [[ -e "${known_tokens_csv}" && "${METADATA_CLOBBERS_CONFIG:-false}" == "true" ]]; then
    rm "${known_tokens_csv}"
  fi
  if [[ -n "${KUBE_BEARER_TOKEN:-}" ]]; then
    append_or_replace_prefixed_line "${known_tokens_csv}" "${KUBE_BEARER_TOKEN},"             "admin,admin,system:masters"
  fi
  if [[ -n "${KUBE_BOOTSTRAP_TOKEN:-}" ]]; then
    append_or_replace_prefixed_line "${known_tokens_csv}" "${KUBE_BOOTSTRAP_TOKEN},"          "gcp:kube-bootstrap,uid:gcp:kube-bootstrap,system:masters"
  fi
  if [[ -n "${CLOUD_CONTROLLER_MANAGER_TOKEN:-}" ]]; then
    append_or_replace_prefixed_line "${known_tokens_csv}" "${CLOUD_CONTROLLER_MANAGER_TOKEN}," "system:cloud-controller-manager,uid:system:cloud-controller-manager"
  fi
  if [[ -n "${KUBE_CONTROLLER_MANAGER_TOKEN:-}" ]]; then
    append_or_replace_prefixed_line "${known_tokens_csv}" "${KUBE_CONTROLLER_MANAGER_TOKEN}," "system:kube-controller-manager,uid:system:kube-controller-manager"
  fi
  if [[ -n "${KUBE_SCHEDULER_TOKEN:-}" ]]; then
    append_or_replace_prefixed_line "${known_tokens_csv}" "${KUBE_SCHEDULER_TOKEN},"          "system:kube-scheduler,uid:system:kube-scheduler"
  fi
  if [[ -n "${KUBE_CLUSTER_AUTOSCALER_TOKEN:-}" ]]; then
    append_or_replace_prefixed_line "${known_tokens_csv}" "${KUBE_CLUSTER_AUTOSCALER_TOKEN}," "cluster-autoscaler,uid:cluster-autoscaler"
  fi
  if [[ -n "${KUBE_PROXY_TOKEN:-}" ]]; then
    append_or_replace_prefixed_line "${known_tokens_csv}" "${KUBE_PROXY_TOKEN},"              "system:kube-proxy,uid:kube_proxy"
  fi
  if [[ -n "${NODE_PROBLEM_DETECTOR_TOKEN:-}" ]]; then
    append_or_replace_prefixed_line "${known_tokens_csv}" "${NODE_PROBLEM_DETECTOR_TOKEN},"   "system:node-problem-detector,uid:node-problem-detector"
  fi
  if [[ -n "${GCE_GLBC_TOKEN:-}" ]]; then
    append_or_replace_prefixed_line "${known_tokens_csv}" "${GCE_GLBC_TOKEN},"                "system:controller:glbc,uid:system:controller:glbc"
  fi
  if [[ -n "${ADDON_MANAGER_TOKEN:-}" ]]; then
    append_or_replace_prefixed_line "${known_tokens_csv}" "${ADDON_MANAGER_TOKEN},"           "system:addon-manager,uid:system:addon-manager,system:masters"
  fi
  if [[ -n "${KONNECTIVITY_SERVER_TOKEN:-}" ]]; then
    append_or_replace_prefixed_line "${known_tokens_csv}" "${KONNECTIVITY_SERVER_TOKEN},"     "system:konnectivity-server,uid:system:konnectivity-server"
    create-kubeconfig "konnectivity-server" "${KONNECTIVITY_SERVER_TOKEN}"
  fi
  if [[ -n "${MONITORING_TOKEN:-}" ]]; then
    append_or_replace_prefixed_line "${known_tokens_csv}" "${MONITORING_TOKEN},"     "system:monitoring,uid:system:monitoring,system:monitoring"
  fi

  if [[ -n "${EXTRA_STATIC_AUTH_COMPONENTS:-}" ]]; then
    # Create a static Bearer token and kubeconfig for extra, comma-separated components.
    IFS="," read -r -a extra_components <<< "${EXTRA_STATIC_AUTH_COMPONENTS:-}"
    for extra_component in "${extra_components[@]}"; do
      local token
      token="$(secure_random 32)"
      append_or_replace_prefixed_line "${known_tokens_csv}" "${token}," "system:${extra_component},uid:system:${extra_component}"
      create-kubeconfig "${extra_component}" "${token}"
    done
  fi
  local use_cloud_config="false"
  cat <<EOF >/etc/gce.conf
[global]
EOF
  if [[ -n "${GCE_API_ENDPOINT:-}" ]]; then
    cat <<EOF >>/etc/gce.conf
api-endpoint = ${GCE_API_ENDPOINT}
EOF
  fi
  if [[ -n "${TOKEN_URL:-}" && -n "${TOKEN_BODY:-}" ]]; then
    use_cloud_config="true"
    cat <<EOF >>/etc/gce.conf
token-url = ${TOKEN_URL}
token-body = ${TOKEN_BODY}
EOF
  fi
  if [[ -n "${CONTAINER_API_ENDPOINT:-}" ]]; then
    use_cloud_config="true"
    cat <<EOF >>/etc/gce.conf
container-api-endpoint = ${CONTAINER_API_ENDPOINT}
EOF
  fi
  if [[ -n "${PROJECT_ID:-}" ]]; then
    use_cloud_config="true"
    cat <<EOF >>/etc/gce.conf
project-id = ${PROJECT_ID}
EOF
  fi
  if [[ -n "${NETWORK_PROJECT_ID:-}" ]]; then
    use_cloud_config="true"
    cat <<EOF >>/etc/gce.conf
network-project-id = ${NETWORK_PROJECT_ID}
EOF
  fi
  if [[ -n "${STACK_TYPE:-}" ]]; then
    use_cloud_config="true"
    cat <<EOF >>/etc/gce.conf
stack-type = ${STACK_TYPE}
EOF
  fi
  if [[ -n "${NODE_NETWORK:-}" ]]; then
    use_cloud_config="true"
    cat <<EOF >>/etc/gce.conf
network-name = ${NODE_NETWORK}
EOF
  fi
  if [[ -n "${NODE_SUBNETWORK:-}" ]]; then
    use_cloud_config="true"
    cat <<EOF >>/etc/gce.conf
subnetwork-name = ${NODE_SUBNETWORK}
EOF
  fi
  if [[ -n "${NODE_INSTANCE_PREFIX:-}" ]]; then
    use_cloud_config="true"
    if [[ -n "${NODE_TAGS:-}" ]]; then
      # split NODE_TAGS into an array by comma.
      IFS=',' read -r -a node_tags <<< "${NODE_TAGS}"
    else
      local -r node_tags=("${NODE_INSTANCE_PREFIX}")
    fi
    cat <<EOF >>/etc/gce.conf
node-instance-prefix = ${NODE_INSTANCE_PREFIX}
EOF
    for tag in "${node_tags[@]}"; do
      cat <<EOF >>/etc/gce.conf
node-tags = ${tag}
EOF
    done
  fi
  if [[ -n "${MULTIZONE:-}" ]]; then
    use_cloud_config="true"
    cat <<EOF >>/etc/gce.conf
multizone = ${MULTIZONE}
EOF
  fi
# Multimaster indicates that the cluster is HA.
# Currently the only HA clusters are regional.
# If we introduce zonal multimaster this will need to be revisited.
  if [[ -n "${MULTIMASTER:-}" ]]; then
    use_cloud_config="true"
    cat <<EOF >>/etc/gce.conf
regional = ${MULTIMASTER}
EOF
  fi
  if [[ -n "${GCE_ALPHA_FEATURES:-}" ]]; then
    use_cloud_config="true"
    # split GCE_ALPHA_FEATURES into an array by comma.
    IFS=',' read -r -a alpha_features <<< "${GCE_ALPHA_FEATURES}"
    for feature in "${alpha_features[@]}"; do
      cat <<EOF >>/etc/gce.conf
alpha-features = ${feature}
EOF
    done
  fi
  if [[ -n "${SECONDARY_RANGE_NAME:-}" ]]; then
    use_cloud_config="true"
    cat <<EOF >> /etc/gce.conf
secondary-range-name = ${SECONDARY_RANGE_NAME}
EOF
  fi
  if [[ "${use_cloud_config}" != "true" ]]; then
    rm -f /etc/gce.conf
  fi

  if [[ -n "${GCP_AUTHN_URL:-}" ]]; then
    cat <<EOF >/etc/gcp_authn.config
clusters:
  - name: gcp-authentication-server
    cluster:
      server: ${GCP_AUTHN_URL}
users:
  - name: kube-apiserver
    user:
      auth-provider:
        name: gcp
current-context: webhook
contexts:
- context:
    cluster: gcp-authentication-server
    user: kube-apiserver
  name: webhook
EOF
  fi

  if [[ -n "${GCP_AUTHZ_URL:-}" ]]; then
    cat <<EOF >/etc/gcp_authz.config
clusters:
  - name: gcp-authorization-server
    cluster:
      server: ${GCP_AUTHZ_URL}
users:
  - name: kube-apiserver
    user:
      auth-provider:
        name: gcp
current-context: webhook
contexts:
- context:
    cluster: gcp-authorization-server
    user: kube-apiserver
  name: webhook
EOF
  fi
  if [[ "${PREPARE_KONNECTIVITY_SERVICE:-false}" == "true" ]]; then
    if [[ "${KONNECTIVITY_SERVICE_PROXY_PROTOCOL_MODE:-grpc}" == 'grpc' ]]; then
      cat <<EOF >/etc/srv/kubernetes/egress_selector_configuration.yaml
apiVersion: apiserver.k8s.io/v1beta1
kind: EgressSelectorConfiguration
egressSelections:
- name: cluster
  connection:
    proxyProtocol: GRPC
    transport:
      uds:
        udsName: /etc/srv/kubernetes/konnectivity-server/konnectivity-server.socket
- name: controlplane
  connection:
    proxyProtocol: Direct
- name: etcd
  connection:
    proxyProtocol: Direct
EOF
    elif [[ "${KONNECTIVITY_SERVICE_PROXY_PROTOCOL_MODE:-grpc}" == 'http-connect' ]]; then
      cat <<EOF >/etc/srv/kubernetes/egress_selector_configuration.yaml
apiVersion: apiserver.k8s.io/v1beta1
kind: EgressSelectorConfiguration
egressSelections:
- name: cluster
  connection:
    proxyProtocol: HTTPConnect
    transport:
      tcp:
        url: https://127.0.0.1:8131
        tlsConfig:
          caBundle: /etc/srv/kubernetes/pki/konnectivity-server/ca.crt
          clientKey: /etc/srv/kubernetes/pki/konnectivity-server/client.key
          clientCert: /etc/srv/kubernetes/pki/konnectivity-server/client.crt
- name: controlplane
  connection:
    proxyProtocol: Direct
- name: etcd
  connection:
    proxyProtocol: Direct
EOF
    else
      echo "KONNECTIVITY_SERVICE_PROXY_PROTOCOL_MODE must be set to either grpc or http-connect"
      exit 1
    fi
  fi

  if [[ -n "${ADMISSION_CONTROL:-}" ]]; then
    # Emit a basic admission control configuration file, with no plugins specified.
    cat <<EOF >/etc/srv/kubernetes/admission_controller_config.yaml
apiVersion: apiserver.k8s.io/v1alpha1
kind: AdmissionConfiguration
plugins:
EOF

    # Add resourcequota config to limit critical pods to kube-system by default
    cat <<EOF >>/etc/srv/kubernetes/admission_controller_config.yaml
- name: "ResourceQuota"
  configuration:
    apiVersion: apiserver.config.k8s.io/v1
    kind: ResourceQuotaConfiguration
    limitedResources:
    - resource: pods
      matchScopes:
      - scopeName: PriorityClass
        operator: In
        values: ["system-node-critical", "system-cluster-critical"]
EOF

    if [[ "${ADMISSION_CONTROL:-}" == *"ImagePolicyWebhook"* ]]; then
      if [[ -z "${GCP_IMAGE_VERIFICATION_URL:-}" ]]; then
        1>&2 echo "The ImagePolicyWebhook admission control plugin was requested, but GCP_IMAGE_VERIFICATION_URL was not provided."
        exit 1
      fi

      1>&2 echo "ImagePolicyWebhook admission control plugin requested.  Configuring it to point at ${GCP_IMAGE_VERIFICATION_URL}"

      # ImagePolicyWebhook needs special kubeconfig for authenticating to the webhook endpoint.
      cat <<EOF >/etc/srv/kubernetes/gcp_image_review.kubeconfig
clusters:
  - name: gcp-image-review-server
    cluster:
      server: ${GCP_IMAGE_VERIFICATION_URL}
users:
  - name: kube-apiserver
    user:
      auth-provider:
        name: gcp
current-context: webhook
contexts:
- context:
    cluster: gcp-image-review-server
    user: kube-apiserver
  name: webhook
EOF

      # Append config for ImagePolicyWebhook to the shared admission controller
      # configuration file.
      cat <<EOF >>/etc/srv/kubernetes/admission_controller_config.yaml
- name: ImagePolicyWebhook
  configuration:
    imagePolicy:
      kubeConfigFile: /etc/srv/kubernetes/gcp_image_review.kubeconfig
      allowTTL: 30
      denyTTL: 30
      retryBackoff: 500
      defaultAllow: true
EOF
    fi
  fi
}

# Write the config for the audit policy.
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
      - group: "metrics.k8s.io"
      - group: "networking.k8s.io"
      - group: "node.k8s.io"
      - group: "policy"
      - group: "rbac.authorization.k8s.io"
      - group: "scheduling.k8s.io"
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
      - system:cloud-controller-manager
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
  - level: None
    users: ["cluster-autoscaler"]
    verbs: ["get", "update"]
    namespaces: ["kube-system"]
    resources:
      - group: "" # core
        resources: ["configmaps", "endpoints"]
  # Don't log HPA fetching metrics.
  - level: None
    users:
      - system:kube-controller-manager
      - system:cloud-controller-manager
    verbs: ["get", "list"]
    resources:
      - group: "metrics.k8s.io"

  # Don't log these read-only URLs.
  - level: None
    nonResourceURLs:
      - /healthz*
      - /version
      - /swagger*

  # Don't log events requests because of performance impact.
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

# Writes the configuration file used by the webhook advanced auditing backend.
function create-master-audit-webhook-config {
  local -r path="${1}"

  if [[ -n "${GCP_AUDIT_URL:-}" ]]; then
    # The webhook config file is a kubeconfig file describing the webhook endpoint.
    cat <<EOF >"${path}"
clusters:
  - name: gcp-audit-server
    cluster:
      server: ${GCP_AUDIT_URL}
users:
  - name: kube-apiserver
    user:
      auth-provider:
        name: gcp
current-context: webhook
contexts:
- context:
    cluster: gcp-audit-server
    user: kube-apiserver
  name: webhook
EOF
  fi
}

function create-kubeconfig {
  local component=$1
  local token=$2
  local path="/etc/srv/kubernetes/${component}/kubeconfig"
  mkdir -p "/etc/srv/kubernetes/${component}"

  if [[ -e "${KUBE_HOME}/bin/gke-internal-configure-helper.sh" ]]; then
    gke-internal-create-kubeconfig "${component}" "${token}" "${path}"
  else
    echo "Creating kubeconfig file for component ${component}"
    cat <<EOF >"${path}"
apiVersion: v1
kind: Config
users:
- name: ${component}
  user:
    token: ${token}
clusters:
- name: local
  cluster:
    insecure-skip-tls-verify: true
    server: https://localhost:443
contexts:
- context:
    cluster: local
    user: ${component}
  name: ${component}
current-context: ${component}
EOF
  fi
}

# Arg 1: the IP address of the API server
function create-kubelet-kubeconfig() {
  local apiserver_address="${1}"
  if [[ -z "${apiserver_address}" ]]; then
    echo "Must provide API server address to create Kubelet kubeconfig file!"
    exit 1
  fi
  if [[ "${CREATE_BOOTSTRAP_KUBECONFIG:-true}" == "true" ]]; then
    echo "Creating kubelet bootstrap-kubeconfig file"
    cat <<EOF >/var/lib/kubelet/bootstrap-kubeconfig
apiVersion: v1
kind: Config
users:
- name: kubelet
  user:
    client-certificate: ${KUBELET_CERT_PATH}
    client-key: ${KUBELET_KEY_PATH}
clusters:
- name: local
  cluster:
    server: https://${apiserver_address}
    certificate-authority: ${CA_CERT_BUNDLE_PATH}
contexts:
- context:
    cluster: local
    user: kubelet
  name: service-account-context
current-context: service-account-context
EOF
  elif [[ "${FETCH_BOOTSTRAP_KUBECONFIG:-false}" == "true" ]]; then
    echo "Fetching kubelet bootstrap-kubeconfig file from metadata"
    get-metadata-value "instance/attributes/bootstrap-kubeconfig" >/var/lib/kubelet/bootstrap-kubeconfig
  else
    echo "Fetching kubelet kubeconfig file from metadata"
    get-metadata-value "instance/attributes/kubeconfig" >/var/lib/kubelet/kubeconfig
  fi
}

# Uses KUBELET_CA_CERT (falling back to CA_CERT), KUBELET_CERT, and KUBELET_KEY
# to generate a kubeconfig file for the kubelet to securely connect to the apiserver.
# Set REGISTER_MASTER_KUBELET to true if kubelet on the master node
# should register to the apiserver.
function create-master-kubelet-auth {
  # Only configure the kubelet on the master if the required variables are
  # set in the environment.
  if [[ -n "${KUBELET_APISERVER:-}" && -n "${KUBELET_CERT:-}" && -n "${KUBELET_KEY:-}" ]]; then
    REGISTER_MASTER_KUBELET="true"
    create-kubelet-kubeconfig "${KUBELET_APISERVER}"
  fi
}

function create-kubeproxy-user-kubeconfig {
  echo "Creating kube-proxy user kubeconfig file"
  cat <<EOF >/var/lib/kube-proxy/kubeconfig
apiVersion: v1
kind: Config
users:
- name: kube-proxy
  user:
    token: ${KUBE_PROXY_TOKEN}
clusters:
- name: local
  cluster:
    certificate-authority-data: ${CA_CERT_BUNDLE}
contexts:
- context:
    cluster: local
    user: kube-proxy
  name: service-account-context
current-context: service-account-context
EOF
}

function create-kube-scheduler-config {
  echo "Creating kube-scheduler config file"
  mkdir -p /etc/srv/kubernetes/kube-scheduler
  cat <<EOF >/etc/srv/kubernetes/kube-scheduler/config
${KUBE_SCHEDULER_CONFIG}
EOF
}

# TODO(#92143): Remove legacy policy config creation once kube-scheduler config is GA.
function create-kubescheduler-policy-config {
  echo "Creating kube-scheduler policy config file"
  mkdir -p /etc/srv/kubernetes/kube-scheduler
  cat <<EOF >/etc/srv/kubernetes/kube-scheduler/policy-config
${SCHEDULER_POLICY_CONFIG}
EOF
}

function create-node-problem-detector-kubeconfig {
  local apiserver_address="${1}"
  if [[ -z "${apiserver_address}" ]]; then
    echo "Must provide API server address to create node-problem-detector kubeconfig file!"
    exit 1
  fi
  echo "Creating node-problem-detector kubeconfig file"
  mkdir -p /var/lib/node-problem-detector
  cat <<EOF >/var/lib/node-problem-detector/kubeconfig
apiVersion: v1
kind: Config
users:
- name: node-problem-detector
  user:
    token: ${NODE_PROBLEM_DETECTOR_TOKEN}
clusters:
- name: local
  cluster:
    server: https://${apiserver_address}
    certificate-authority-data: ${CA_CERT}
contexts:
- context:
    cluster: local
    user: node-problem-detector
  name: service-account-context
current-context: service-account-context
EOF
}

function create-node-problem-detector-kubeconfig-from-kubelet {
  echo "Creating node-problem-detector kubeconfig from /var/lib/kubelet/kubeconfig"
  mkdir -p /var/lib/node-problem-detector
  cp /var/lib/kubelet/kubeconfig /var/lib/node-problem-detector/kubeconfig
}

function create-master-etcd-auth {
  if [[ -n "${ETCD_CA_CERT:-}" && -n "${ETCD_PEER_KEY:-}" && -n "${ETCD_PEER_CERT:-}" ]]; then
    local -r auth_dir="/etc/srv/kubernetes"
    echo "${ETCD_CA_CERT}" | base64 --decode | gunzip > "${auth_dir}/etcd-ca.crt"
    echo "${ETCD_PEER_KEY}" | base64 --decode > "${auth_dir}/etcd-peer.key"
    echo "${ETCD_PEER_CERT}" | base64 --decode | gunzip > "${auth_dir}/etcd-peer.crt"
  fi
}

function create-master-etcd-apiserver-auth {
   if [[ -n "${ETCD_APISERVER_CA_CERT:-}" && -n "${ETCD_APISERVER_SERVER_KEY:-}" && -n "${ETCD_APISERVER_SERVER_CERT:-}" && -n "${ETCD_APISERVER_CLIENT_KEY:-}" && -n "${ETCD_APISERVER_CLIENT_CERT:-}" ]]; then
     local -r auth_dir="/etc/srv/kubernetes/pki"

     ETCD_APISERVER_CA_KEY_PATH="${auth_dir}/etcd-apiserver-ca.key"
     echo "${ETCD_APISERVER_CA_KEY}" | base64 --decode > "${ETCD_APISERVER_CA_KEY_PATH}"

     # Keep in sync with add-replica-to-etcd/remove-replica-from-etcd in util.sh.
     ETCD_APISERVER_CA_CERT_PATH="${auth_dir}/etcd-apiserver-ca.crt"
     echo "${ETCD_APISERVER_CA_CERT}" | base64 --decode | gunzip > "${ETCD_APISERVER_CA_CERT_PATH}"

     ETCD_APISERVER_SERVER_KEY_PATH="${auth_dir}/etcd-apiserver-server.key"
     echo "${ETCD_APISERVER_SERVER_KEY}" | base64 --decode > "${ETCD_APISERVER_SERVER_KEY_PATH}"

     ETCD_APISERVER_SERVER_CERT_PATH="${auth_dir}/etcd-apiserver-server.crt"
     echo "${ETCD_APISERVER_SERVER_CERT}" | base64 --decode | gunzip > "${ETCD_APISERVER_SERVER_CERT_PATH}"

     # Keep in sync with add-replica-to-etcd/remove-replica-from-etcd in util.sh.
     ETCD_APISERVER_CLIENT_KEY_PATH="${auth_dir}/etcd-apiserver-client.key"
     echo "${ETCD_APISERVER_CLIENT_KEY}" | base64 --decode > "${ETCD_APISERVER_CLIENT_KEY_PATH}"

     # Keep in sync with add-replica-to-etcd/remove-replica-from-etcd in util.sh.
     ETCD_APISERVER_CLIENT_CERT_PATH="${auth_dir}/etcd-apiserver-client.crt"
     echo "${ETCD_APISERVER_CLIENT_CERT}" | base64 --decode | gunzip > "${ETCD_APISERVER_CLIENT_CERT_PATH}"
   fi
}

function docker-installed {
    if systemctl cat docker.service &> /dev/null ; then
        return 0
    else
        return 1
    fi
}

# util function to add a docker option to daemon.json file only if the daemon.json file is present.
# accepts only one argument (docker options)
function addockeropt {
	DOCKER_OPTS_FILE=/etc/docker/daemon.json
	if [ "$#" -lt 1 ]; then
	echo "No arguments are passed while adding docker options. Expect one argument"
	exit 1
	elif [ "$#" -gt 1 ]; then
	echo "Only one argument is accepted"
	exit 1
	fi
	# appends the given input to the docker opts file i.e. /etc/docker/daemon.json file
	if [ -f "$DOCKER_OPTS_FILE" ]; then
	cat >> "${DOCKER_OPTS_FILE}" <<EOF
  $1
EOF
	fi
}

function disable_aufs() {
  # disable aufs module if aufs is loaded
  if lsmod | grep "aufs" &> /dev/null ; then
    sudo modprobe -r aufs
  fi
}

function set_docker_options_non_ubuntu() {
  # set docker options mtu and storage driver for non-ubuntu
  # as it is default for ubuntu
   if [[ -n "$(command -v lsb_release)" && $(lsb_release -si) == "Ubuntu" ]]; then
      echo "Not adding docker options on ubuntu, as these are default on ubuntu. Bailing out..."
      return
   fi

   addockeropt "\"mtu\": 1460,"
   addockeropt "\"storage-driver\": \"overlay2\","
   echo "setting live restore"
   # Disable live-restore if the environment variable is set.
   if [[ "${DISABLE_DOCKER_LIVE_RESTORE:-false}" == "true" ]]; then
      addockeropt "\"live-restore\": false,"
   else
      addockeropt "\"live-restore\": true,"
   fi
}

function assemble-docker-flags {
  echo "Assemble docker options"

    # log the contents of the /etc/docker/daemon.json if already exists
  if [ -f /etc/docker/daemon.json ]; then
    echo "Contents of the old docker config"
    cat /etc/docker/daemon.json
  fi

  cat <<EOF >/etc/docker/daemon.json
{
EOF

addockeropt "\"pidfile\": \"/var/run/docker.pid\",
  \"iptables\": false,
  \"ip-masq\": false,"

  echo "setting log-level"
  if [[ "${TEST_CLUSTER:-}" == "true" ]]; then
    addockeropt "\"log-level\": \"debug\","
  else
    addockeropt "\"log-level\": \"warn\","
  fi

  echo "setting network bridge"
  if [[ "${NETWORK_PROVIDER:-}" == "kubenet" || "${NETWORK_PROVIDER:-}" == "cni" ]]; then
    # set docker0 cidr to private ip address range to avoid conflict with cbr0 cidr range
    addockeropt "\"bip\": \"169.254.123.1/24\","
  else
    addockeropt "\"bridge\": \"cbr0\","
  fi

  echo "setting registry mirror"
  # TODO (vteratipally)  move the registry-mirror completely to /etc/docker/daemon.json
  local docker_opts=""
  # Decide whether to enable a docker registry mirror. This is taken from
  # the "kube-env" metadata value.
  if [[ -n "${DOCKER_REGISTRY_MIRROR_URL:-}" ]]; then
      docker_opts+="--registry-mirror=${DOCKER_REGISTRY_MIRROR_URL} "
  fi

  disable_aufs
  set_docker_options_non_ubuntu


  echo "setting docker logging options"
  # Configure docker logging
  addockeropt "\"log-driver\": \"${DOCKER_LOG_DRIVER:-json-file}\","
  addockeropt "\"log-opts\": {
      \"max-size\": \"${DOCKER_LOG_MAX_SIZE:-10m}\",
      \"max-file\": \"${DOCKER_LOG_MAX_FILE:-5}\"
    }"
  cat <<EOF >>/etc/docker/daemon.json
}
EOF
  echo "DOCKER_OPTS=\"${docker_opts}${EXTRA_DOCKER_OPTS:-}\"" > /etc/default/docker

  # Ensure TasksMax is sufficient for docker.
  # (https://github.com/kubernetes/kubernetes/issues/51977)
  echo "Extend the docker.service configuration to set a higher pids limit"
  mkdir -p /etc/systemd/system/docker.service.d
  cat <<EOF >/etc/systemd/system/docker.service.d/01tasksmax.conf
[Service]
TasksMax=infinity
EOF

    systemctl daemon-reload
    echo "Docker command line is updated. Restart docker to pick it up"
    systemctl restart docker
}

# This function assembles the kubelet systemd service file and starts it
# using systemctl.
function start-kubelet {
  echo "Start kubelet"

  local kubelet_bin="${KUBE_HOME}/bin/kubelet"
  local -r version="$("${kubelet_bin}" --version=true | cut -f2 -d " ")"
  local -r builtin_kubelet="/usr/bin/kubelet"
  if [[ "${TEST_CLUSTER:-}" == "true" ]]; then
    # Determine which binary to use on test clusters. We use the built-in
    # version only if the downloaded version is the same as the built-in
    # version. This allows GCI to run some of the e2e tests to qualify the
    # built-in kubelet.
    if [[ -x "${builtin_kubelet}" ]]; then
      local -r builtin_version="$("${builtin_kubelet}"  --version=true | cut -f2 -d " ")"
      if [[ "${builtin_version}" == "${version}" ]]; then
        kubelet_bin="${builtin_kubelet}"
      fi
    fi
  fi
  echo "Using kubelet binary at ${kubelet_bin}"

  local -r kubelet_env_file="/etc/default/kubelet"

  local kubelet_cgroup_driver=""
  if [[ "${CGROUP_CONFIG-}" == "cgroup2fs" ]]; then
    kubelet_cgroup_driver="--cgroup-driver=systemd"
  fi

  local kubelet_opts="${KUBELET_ARGS} ${KUBELET_CONFIG_FILE_ARG:-} ${kubelet_cgroup_driver:-}"
  echo "KUBELET_OPTS=\"${kubelet_opts}\"" > "${kubelet_env_file}"
  echo "KUBE_COVERAGE_FILE=\"/var/log/kubelet.cov\"" >> "${kubelet_env_file}"

  # Write the systemd service file for kubelet.
  cat <<EOF >/etc/systemd/system/kubelet.service
[Unit]
Description=Kubernetes kubelet
Requires=network-online.target
After=network-online.target

[Service]
Restart=always
RestartSec=10
WatchdogSec=30s
EnvironmentFile=${kubelet_env_file}
ExecStart=${kubelet_bin} \$KUBELET_OPTS

[Install]
WantedBy=multi-user.target
EOF

  systemctl daemon-reload
  systemctl start kubelet.service
}

# This function assembles the node problem detector systemd service file and
# starts it using systemctl.
function start-node-problem-detector {
  echo "Start node problem detector"
  local -r npd_bin="${KUBE_HOME}/bin/node-problem-detector"
  echo "Using node problem detector binary at ${npd_bin}"

  local flags="${NODE_PROBLEM_DETECTOR_CUSTOM_FLAGS:-}"
  if [[ -z "${flags}" ]]; then
    local -r km_config="${KUBE_HOME}/node-problem-detector/config/kernel-monitor.json"
    local -r rom_config="${KUBE_HOME}/node-problem-detector/config/readonly-monitor.json"
    # TODO(random-liu): Handle this for alternative container runtime.
    local -r dm_config="${KUBE_HOME}/node-problem-detector/config/docker-monitor.json"
    local -r sm_config="${KUBE_HOME}/node-problem-detector/config/systemd-monitor.json"
    local -r ssm_config="${KUBE_HOME}/node-problem-detector/config/system-stats-monitor.json"

    local -r custom_km_config="${KUBE_HOME}/node-problem-detector/config/kernel-monitor-counter.json"
    local -r custom_sm_config="${KUBE_HOME}/node-problem-detector/config/systemd-monitor-counter.json"

    flags="${NPD_TEST_LOG_LEVEL:-"--v=2"} ${NPD_TEST_ARGS:-}"
    flags+=" --logtostderr"
    flags+=" --config.system-log-monitor=${km_config},${dm_config},${sm_config},${rom_config}"
    flags+=" --config.system-stats-monitor=${ssm_config}"
    flags+=" --config.custom-plugin-monitor=${custom_km_config},${custom_sm_config}"
    local -r npd_port=${NODE_PROBLEM_DETECTOR_PORT:-20256}
    flags+=" --port=${npd_port}"
    if [[ -n "${EXTRA_NPD_ARGS:-}" ]]; then
      flags+=" ${EXTRA_NPD_ARGS}"
    fi
  fi
  flags+=" --apiserver-override=https://${KUBERNETES_MASTER_NAME}?inClusterConfig=false&auth=/var/lib/node-problem-detector/kubeconfig"

  # Write the systemd service file for node problem detector.
  cat <<EOF >/etc/systemd/system/node-problem-detector.service
[Unit]
Description=Kubernetes node problem detector
Requires=network-online.target
After=network-online.target

[Service]
Restart=always
RestartSec=10
ExecStart=${npd_bin} ${flags}

[Install]
WantedBy=multi-user.target
EOF

  systemctl start node-problem-detector.service
}

# Create the log file and set its properties.
#
# $1 is the file to create.
# $2: the log owner uid to set for the log file.
# $3: the log owner gid to set for the log file. If $KUBE_POD_LOG_READERS_GROUP
# is set then this value will not be used.
function prepare-log-file {
  touch "$1"
  if [[ -n "${KUBE_POD_LOG_READERS_GROUP:-}" ]]; then
    chmod 640 "$1"
    chown "${2:-root}":"${KUBE_POD_LOG_READERS_GROUP}" "$1"
  else
    chmod 644 "$1"
    chown "${2:-${LOG_OWNER_USER:-root}}":"${3:-${LOG_OWNER_GROUP:-root}}" "$1"
  fi
}

# Prepares parameters for kube-proxy manifest.
# $1 source path of kube-proxy manifest.
# Assumptions: HOST_PLATFORM and HOST_ARCH are specified by calling detect_host_info.
function prepare-kube-proxy-manifest-variables {
  local -r src_file=$1;

  local -r kubeconfig="--kubeconfig=/var/lib/kube-proxy/kubeconfig"
  local kube_docker_registry="registry.k8s.io"
  if [[ -n "${KUBE_DOCKER_REGISTRY:-}" ]]; then
    kube_docker_registry=${KUBE_DOCKER_REGISTRY}
  fi
  local -r kube_proxy_docker_tag=$(cat /home/kubernetes/kube-docker-files/kube-proxy.docker_tag)
  local api_servers="--master=https://${KUBERNETES_MASTER_NAME}"
  local params="${KUBEPROXY_TEST_LOG_LEVEL:-"--v=2"}"
  if [[ -n "${FEATURE_GATES:-}" ]]; then
    params+=" --feature-gates=${FEATURE_GATES}"
  fi

  case "${KUBE_PROXY_MODE:-iptables}" in
    iptables)
      params+=" --proxy-mode=iptables --iptables-sync-period=1m --iptables-min-sync-period=10s"
      ;;
    ipvs)
      # use 'nf_conntrack' instead of 'nf_conntrack_ipv4' for linux kernel >= 4.19
      # https://github.com/kubernetes/kubernetes/pull/70398
      local -r kernel_version=$(uname -r | cut -d\. -f1,2)
      local conntrack_module="nf_conntrack"
      if [[ $(printf '%s\n4.18\n' "${kernel_version}" | sort -V | tail -1) == "4.18" ]]; then
        conntrack_module="nf_conntrack_ipv4"
      fi

      if ! sudo modprobe -a ip_vs ip_vs_rr ip_vs_wrr ip_vs_sh ${conntrack_module}; then
        # If IPVS modules are not present, make sure the node does not come up as
        # healthy.
        exit 1
      fi
      params+=" --proxy-mode=ipvs --ipvs-sync-period=1m --ipvs-min-sync-period=10s"
      ;;
    nftables)
      # Pass --conntrack-tcp-be-liberal so we can test that this makes the
      # "proxy implementation should not be vulnerable to the invalid conntrack state bug"
      # test pass. https://issues.k8s.io/122663#issuecomment-1885024015
      params+=" --proxy-mode=nftables --conntrack-tcp-be-liberal"
      ;;
  esac

  if [[ -n "${KUBEPROXY_TEST_ARGS:-}" ]]; then
    params+=" ${KUBEPROXY_TEST_ARGS}"
  fi
  if [[ -n "${DETECT_LOCAL_MODE:-}" ]]; then
    params+=" --detect-local-mode=${DETECT_LOCAL_MODE}"
  fi

  local container_env=""
  local kube_cache_mutation_detector_env_name=""
  local kube_cache_mutation_detector_env_value=""
  if [[ -n "${ENABLE_CACHE_MUTATION_DETECTOR:-}" ]]; then
    container_env="env:"
    kube_cache_mutation_detector_env_name="- name: KUBE_CACHE_MUTATION_DETECTOR"
    kube_cache_mutation_detector_env_value="value: \"${ENABLE_CACHE_MUTATION_DETECTOR}\""
  fi
  local kube_watchlist_inconsistency_detector_env_name=""
  local kube_watchlist_inconsistency_detector_env_value=""
  if [[ -n "${ENABLE_KUBE_WATCHLIST_INCONSISTENCY_DETECTOR:-}" ]]; then
    if [[ -z "${container_env}" ]]; then
      container_env="env:"
    fi
    kube_watchlist_inconsistency_detector_env_name="- name: KUBE_WATCHLIST_INCONSISTENCY_DETECTOR"
    kube_watchlist_inconsistency_detector_env_value="value: \"${ENABLE_KUBE_WATCHLIST_INCONSISTENCY_DETECTOR}\""
  fi
  local kube_list_from_cache_inconsistency_detector_env_name=""
  local kube_list_from_cache_inconsistency_detector_env_value=""
  if [[ -n "${ENABLE_KUBE_LIST_FROM_CACHE_INCONSISTENCY_DETECTOR:-}" ]]; then
    if [[ -z "${container_env}" ]]; then
      container_env="env:"
    fi
    kube_list_from_cache_inconsistency_detector_env_name="- name: KUBE_LIST_FROM_CACHE_INCONSISTENCY_DETECTOR"
    kube_list_from_cache_inconsistency_detector_env_value="value: \"${ENABLE_KUBE_LIST_FROM_CACHE_INCONSISTENCY_DETECTOR}\""
  fi
  sed -i -e "s@{{kubeconfig}}@${kubeconfig}@g" "${src_file}"
  sed -i -e "s@{{pillar\['kube_docker_registry'\]}}@${kube_docker_registry}@g" "${src_file}"
  sed -i -e "s@{{pillar\['kube-proxy_docker_tag'\]}}@${kube_proxy_docker_tag}@g" "${src_file}"
  # TODO(#99245): Use multi-arch image and get rid of this.
  sed -i -e "s@{{pillar\['host_arch'\]}}@${HOST_ARCH}@g" "${src_file}"
  sed -i -e "s@{{params}}@${params}@g" "${src_file}"
  sed -i -e "s@{{container_env}}@${container_env}@g" "${src_file}"
  sed -i -e "s@{{kube_cache_mutation_detector_env_name}}@${kube_cache_mutation_detector_env_name}@g" "${src_file}"
  sed -i -e "s@{{kube_cache_mutation_detector_env_value}}@${kube_cache_mutation_detector_env_value}@g" "${src_file}"
  sed -i -e "s@{{kube_watchlist_inconsistency_detector_env_name}}@${kube_watchlist_inconsistency_detector_env_name}@g" "${src_file}"
  sed -i -e "s@{{kube_watchlist_inconsistency_detector_env_value}}@${kube_watchlist_inconsistency_detector_env_value}@g" "${src_file}"
  sed -i -e "s@{{kube_list_from_cache_inconsistency_detector_env_name}}@${kube_list_from_cache_inconsistency_detector_env_name}@g" "${src_file}"
  sed -i -e "s@{{kube_list_from_cache_inconsistency_detector_env_value}}@${kube_list_from_cache_inconsistency_detector_env_value}@g" "${src_file}"
  sed -i -e "s@{{ cpurequest }}@${KUBE_PROXY_CPU_REQUEST:-100m}@g" "${src_file}"
  sed -i -e "s@{{ memoryrequest }}@${KUBE_PROXY_MEMORY_REQUEST:-50Mi}@g" "${src_file}"
  sed -i -e "s@{{api_servers_with_port}}@${api_servers}@g" "${src_file}"
  sed -i -e "s@{{kubernetes_service_host_env_value}}@${KUBERNETES_MASTER_NAME}@g" "${src_file}"
  if [[ -n "${CLUSTER_IP_RANGE:-}" ]]; then
    sed -i -e "s@{{cluster_cidr}}@--cluster-cidr=${CLUSTER_IP_RANGE}@g" "${src_file}"
  fi
}

# Starts kube-proxy static pod.
function start-kube-proxy {
  echo "Start kube-proxy static pod"
  prepare-log-file /var/log/kube-proxy.log
  local -r src_file="${KUBE_HOME}/kube-manifests/kubernetes/gci-trusty/kube-proxy.manifest"
  prepare-kube-proxy-manifest-variables "${src_file}"

  cp "${src_file}" /etc/kubernetes/manifests
}

# Replaces the variables in the etcd manifest file with the real values, and then
# copy the file to the manifest dir
# $1: value for variable 'suffix'
# $2: value for variable 'port'
# $3: value for variable 'server_port'
# $4: value for variable 'cpulimit'
# $5: pod name, which should be either etcd or etcd-events
function prepare-etcd-manifest {
  local host_name=${ETCD_HOSTNAME:-$(hostname -s)}

  local resolve_host_script_py='
import socket
import time
import sys

timeout_sec=300

def resolve(host):
  for attempt in range(timeout_sec):
    try:
      print(socket.gethostbyname(host))
      break
    except Exception as e:
      sys.stderr.write("error: resolving host %s to IP failed: %s\n" % (host, e))
      time.sleep(1)
      continue

'

  local -r host_ip=$(python3 -c "${resolve_host_script_py}"$'\n'"resolve(\"${host_name}\")")
  local etcd_cluster=""
  local cluster_state="new"
  local etcd_protocol="http"
  local etcd_apiserver_protocol="http"
  local etcd_creds=""
  local etcd_apiserver_creds="${ETCD_APISERVER_CREDS:-}"
  local etcd_extra_args="${ETCD_EXTRA_ARGS:-}"
  local suffix="$1"
  local etcd_listen_metrics_port="$2"
  local etcdctl_certs=""

  if [[ -n "${INITIAL_ETCD_CLUSTER_STATE:-}" ]]; then
    cluster_state="${INITIAL_ETCD_CLUSTER_STATE}"
  fi
  if [[ -n "${ETCD_CA_CERT:-}" && -n "${ETCD_PEER_KEY:-}" && -n "${ETCD_PEER_CERT:-}" ]]; then
    etcd_creds=" --peer-trusted-ca-file /etc/srv/kubernetes/etcd-ca.crt --peer-cert-file /etc/srv/kubernetes/etcd-peer.crt --peer-key-file /etc/srv/kubernetes/etcd-peer.key -peer-client-cert-auth "
    etcd_protocol="https"
  fi

  # mTLS should only be enabled for etcd server but not etcd-events. if $1 suffix is empty, it's etcd server.
  if [[ -z "${suffix}" && -n "${ETCD_APISERVER_CA_KEY:-}" && -n "${ETCD_APISERVER_CA_CERT:-}" && -n "${ETCD_APISERVER_SERVER_KEY:-}" && -n "${ETCD_APISERVER_SERVER_CERT:-}" && -n "${ETCD_APISERVER_CLIENT_KEY:-}" && -n "${ETCD_APISERVER_CLIENT_CERT:-}" ]]; then
    etcd_apiserver_creds=" --client-cert-auth --trusted-ca-file ${ETCD_APISERVER_CA_CERT_PATH} --cert-file ${ETCD_APISERVER_SERVER_CERT_PATH} --key-file ${ETCD_APISERVER_SERVER_KEY_PATH} "
    etcdctl_certs="--cacert ${ETCD_APISERVER_CA_CERT_PATH} --cert ${ETCD_APISERVER_CLIENT_CERT_PATH} --key ${ETCD_APISERVER_CLIENT_KEY_PATH}"
    etcd_apiserver_protocol="https"
    etcd_listen_metrics_port="2382"
    etcd_extra_args+=" --listen-metrics-urls=http://${ETCD_LISTEN_CLIENT_IP:-127.0.0.1}:${etcd_listen_metrics_port} "
  fi

  if [[ -n "${ETCD_PROGRESS_NOTIFY_INTERVAL:-}" ]]; then
    etcd_extra_args+=" --experimental-watch-progress-notify-interval=${ETCD_PROGRESS_NOTIFY_INTERVAL}"
  fi

  for host in $(echo "${INITIAL_ETCD_CLUSTER:-${host_name}}" | tr "," "\n"); do
    etcd_host="etcd-${host}=${etcd_protocol}://${host}:$3"
    if [[ -n "${etcd_cluster}" ]]; then
      etcd_cluster+=","
    fi
    etcd_cluster+="${etcd_host}"
  done

  local -r temp_file="/tmp/$5"
  cp "${KUBE_HOME}/kube-manifests/kubernetes/gci-trusty/etcd.manifest" "${temp_file}"
  sed -i -e "s@{{ *suffix *}}@$1@g" "${temp_file}"
  sed -i -e "s@{{ *port *}}@$2@g" "${temp_file}"
  sed -i -e "s@{{ *server_port *}}@$3@g" "${temp_file}"
  sed -i -e "s@{{ *cpulimit *}}@\"$4\"@g" "${temp_file}"
  sed -i -e "s@{{ *hostname *}}@$host_name@g" "${temp_file}"
  sed -i -e "s@{{ *host_ip *}}@$host_ip@g" "${temp_file}"
  sed -i -e "s@{{ *etcd_cluster *}}@$etcd_cluster@g" "${temp_file}"
  sed -i -e "s@{{ *liveness_probe_initial_delay *}}@${ETCD_LIVENESS_PROBE_INITIAL_DELAY_SEC:-15}@g" "${temp_file}"
  sed -i -e "s@{{ *listen_client_ip *}}@${ETCD_LISTEN_CLIENT_IP:-127.0.0.1}@g" "${temp_file}"
  # Get default storage backend from manifest file.
  local -r default_storage_backend=$( \
    grep -o "{{ *pillar\.get('storage_backend', '\(.*\)') *}}" "${temp_file}" | \
    sed -e "s@{{ *pillar\.get('storage_backend', '\(.*\)') *}}@\1@g")
  if [[ -n "${STORAGE_BACKEND:-}" ]]; then
    sed -i -e "s@{{ *pillar\.get('storage_backend', '\(.*\)') *}}@${STORAGE_BACKEND}@g" "${temp_file}"
  else
    sed -i -e "s@{{ *pillar\.get('storage_backend', '\(.*\)') *}}@\1@g" "${temp_file}"
  fi
  if [[ "${STORAGE_BACKEND:-${default_storage_backend}}" == "etcd3" ]]; then
    sed -i -e "s@{{ *quota_bytes *}}@--quota-backend-bytes=${ETCD_QUOTA_BACKEND_BYTES:-4294967296}@g" "${temp_file}"
  else
    sed -i -e "s@{{ *quota_bytes *}}@@g" "${temp_file}"
  fi
  sed -i -e "s@{{ *cluster_state *}}@$cluster_state@g" "${temp_file}"
  if [[ -n "${ETCD_IMAGE:-}" ]]; then
    sed -i -e "s@{{ *pillar\.get('etcd_docker_tag', '\(.*\)') *}}@${ETCD_IMAGE}@g" "${temp_file}"
  else
    sed -i -e "s@{{ *pillar\.get('etcd_docker_tag', '\(.*\)') *}}@\1@g" "${temp_file}"
  fi
  if [[ -n "${ETCD_DOCKER_REPOSITORY:-}" ]]; then
    sed -i -e "s@{{ *pillar\.get('etcd_docker_repository', '\(.*\)') *}}@${ETCD_DOCKER_REPOSITORY}@g" "${temp_file}"
  else
    sed -i -e "s@{{ *pillar\.get('etcd_docker_repository', '\(.*\)') *}}@\1@g" "${temp_file}"
  fi
  sed -i -e "s@{{ *etcd_protocol *}}@$etcd_protocol@g" "${temp_file}"
  sed -i -e "s@{{ *etcd_apiserver_protocol *}}@$etcd_apiserver_protocol@g" "${temp_file}"
  sed -i -e "s@{{ *etcd_creds *}}@$etcd_creds@g" "${temp_file}"
  sed -i -e "s@{{ *etcdctl_certs *}}@$etcdctl_certs@g" "${temp_file}"
  sed -i -e "s@{{ *etcd_apiserver_creds *}}@$etcd_apiserver_creds@g" "${temp_file}"
  sed -i -e "s@{{ *etcd_extra_args *}}@$etcd_extra_args@g" "${temp_file}"
  if [[ -n "${ETCD_VERSION:-}" ]]; then
    sed -i -e "s@{{ *pillar\.get('etcd_version', '\(.*\)') *}}@${ETCD_VERSION}@g" "${temp_file}"
  else
    sed -i -e "s@{{ *pillar\.get('etcd_version', '\(.*\)') *}}@\1@g" "${temp_file}"
  fi
  # Replace the volume host path.
  sed -i -e "s@/mnt/master-pd/var/etcd@/mnt/disks/master-pd/var/etcd@g" "${temp_file}"
  # Replace the run as user and run as group
  container_security_context=""
  if [[ -n "${ETCD_RUNASUSER:-}" && -n "${ETCD_RUNASGROUP:-}" ]]; then
    container_security_context="\"securityContext\": {\"runAsUser\": ${ETCD_RUNASUSER}, \"runAsGroup\": ${ETCD_RUNASGROUP}, \"allowPrivilegeEscalation\": false, \"capabilities\": {\"drop\": [\"all\"]}},"
  fi
  sed -i -e "s@{{security_context}}@${container_security_context}@g" "${temp_file}"
  mv "${temp_file}" /etc/kubernetes/manifests
}

# Starts etcd server pod (and etcd-events pod if needed).
# More specifically, it prepares dirs and files, sets the variable value
# in the manifests, and copies them to /etc/kubernetes/manifests.
function start-etcd-servers {
  echo "Start etcd pods"
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
  if [[ -n "${ETCD_RUNASUSER:-}" && -n "${ETCD_RUNASGROUP:-}" ]]; then
    chown -R "${ETCD_RUNASUSER}":"${ETCD_RUNASGROUP}" /mnt/disks/master-pd/var/etcd
  fi
  prepare-log-file /var/log/etcd.log "${ETCD_RUNASUSER:-0}"
  prepare-etcd-manifest "" "2379" "2380" "200m" "etcd.manifest"

  prepare-log-file /var/log/etcd-events.log "${ETCD_RUNASUSER:-0}"
  prepare-etcd-manifest "-events" "4002" "2381" "100m" "etcd-events.manifest"
}

# Replaces the variables in the konnectivity-server manifest file with the real values, and then
# copy the file to the manifest dir
# $1: value for variable "agent_port"
# $2: value for variable "health_port"
# $3: value for variable "admin_port"
function prepare-konnectivity-server-manifest {
  local -r temp_file="/tmp/konnectivity-server.yaml"
  params=()
  cp "${KUBE_HOME}/kube-manifests/kubernetes/gci-trusty/konnectivity-server.yaml" "${temp_file}"
  params+=("--log-file=/var/log/konnectivity-server.log")
  params+=("--logtostderr=false")
  params+=("--log-file-max-size=0")
  if [[ "${KONNECTIVITY_SERVICE_PROXY_PROTOCOL_MODE:-grpc}" == 'grpc' ]]; then
    params+=("--uds-name=/etc/srv/kubernetes/konnectivity-server/konnectivity-server.socket")
  elif [[ "${KONNECTIVITY_SERVICE_PROXY_PROTOCOL_MODE:-grpc}" == 'http-connect' ]]; then
    # HTTP-CONNECT can work with either UDS or mTLS.
    # Linking them here to make sure we get good coverage with two test configurations.
    params+=("--server-ca-cert=${KONNECTIVITY_SERVER_CA_CERT_PATH}")
    params+=("--server-cert=${KONNECTIVITY_SERVER_CERT_PATH}")
    params+=("--server-key=${KONNECTIVITY_SERVER_KEY_PATH}")
    params+=("--cluster-ca-cert=${KONNECTIVITY_AGENT_CA_CERT_PATH}")
  fi
  params+=("--cluster-cert=/etc/srv/kubernetes/pki/apiserver.crt")
  params+=("--cluster-key=/etc/srv/kubernetes/pki/apiserver.key")
  if [[ "${KONNECTIVITY_SERVICE_PROXY_PROTOCOL_MODE:-grpc}" == 'grpc' ]]; then
    params+=("--mode=grpc")
    params+=("--server-port=0")
    params+=("--agent-namespace=kube-system")
    params+=("--agent-service-account=konnectivity-agent")
    params+=("--authentication-audience=system:konnectivity-server")
    params+=("--kubeconfig=/etc/srv/kubernetes/konnectivity-server/kubeconfig")
    params+=("--proxy-strategies=default")
  elif [[ "${KONNECTIVITY_SERVICE_PROXY_PROTOCOL_MODE:-grpc}" == 'http-connect' ]]; then
    # GRPC can work with either UDS or mTLS.
    params+=("--mode=http-connect")
    params+=("--server-port=8131")
    params+=("--agent-namespace=")
    params+=("--agent-service-account=")
    params+=("--authentication-audience=")
    # Need to fix ANP code to allow kubeconfig to be set with mtls.
    params+=("--kubeconfig=")
    params+=("--proxy-strategies=destHost,default")
  else
    echo "KONNECTIVITY_SERVICE_PROXY_PROTOCOL_MODE must be set to either grpc or http-connect"
    exit 1
  fi

  params+=("--agent-port=$1")
  params+=("--health-port=$2")
  params+=("--admin-port=$3")
  params+=("--kubeconfig-qps=75")
  params+=("--kubeconfig-burst=150")
  params+=("--keepalive-time=60s")
  params+=("--frontend-keepalive-time=60s")
  konnectivity_args=""
  for param in "${params[@]}"; do
    konnectivity_args+=", \"${param}\""
  done
  sed -i -e "s@{{ *konnectivity_args *}}@${konnectivity_args}@g" "${temp_file}"
  sed -i -e "s@{{ *agent_port *}}@$1@g" "${temp_file}"
  sed -i -e "s@{{ *health_port *}}@$2@g" "${temp_file}"
  sed -i -e "s@{{ *admin_port *}}@$3@g" "${temp_file}"
  sed -i -e "s@{{ *liveness_probe_initial_delay *}}@30@g" "${temp_file}"
  if [[ -n "${KONNECTIVITY_SERVER_RUNASUSER:-}" && -n "${KONNECTIVITY_SERVER_RUNASGROUP:-}" && -n "${KONNECTIVITY_SERVER_SOCKET_WRITER_GROUP:-}" ]]; then
    sed -i -e "s@{{ *run_as_user *}}@runAsUser: ${KONNECTIVITY_SERVER_RUNASUSER}@g" "${temp_file}"
    sed -i -e "s@{{ *run_as_group *}}@runAsGroup: ${KONNECTIVITY_SERVER_RUNASGROUP}@g" "${temp_file}"
    sed -i -e "s@{{ *supplemental_groups *}}@supplementalGroups: [${KUBE_PKI_READERS_GROUP}]@g" "${temp_file}"
    sed -i -e "s@{{ *container_security_context *}}@securityContext:@g" "${temp_file}"
    sed -i -e "s@{{ *capabilities *}}@capabilities:@g" "${temp_file}"
    sed -i -e "s@{{ *drop_capabilities *}}@drop: [ ALL ]@g" "${temp_file}"
    sed -i -e "s@{{ *disallow_privilege_escalation *}}@allowPrivilegeEscalation: false@g" "${temp_file}"
    mkdir -p /etc/srv/kubernetes/konnectivity-server/
    chown -R "${KONNECTIVITY_SERVER_RUNASUSER}":"${KONNECTIVITY_SERVER_RUNASGROUP}" /etc/srv/kubernetes/konnectivity-server
    chmod g+w /etc/srv/kubernetes/konnectivity-server
  else
    sed -i -e "s@{{ *run_as_user *}}@@g" "${temp_file}"
    sed -i -e "s@{{ *run_as_group *}}@@g" "${temp_file}"
    sed -i -e "s@{{ *supplemental_groups *}}@@g" "${temp_file}"
    sed -i -e "s@{{ *container_security_context *}}@@g" "${temp_file}"
    sed -i -e "s@{{ *capabilities *}}@@g" "${temp_file}"
    sed -i -e "s@{{ *drop_capabilities *}}@@g" "${temp_file}"
    sed -i -e "s@{{ *disallow_privilege_escalation *}}@@g" "${temp_file}"
  fi
  mv "${temp_file}" /etc/kubernetes/manifests
}

# Starts konnectivity server pod.
# More specifically, it prepares dirs and files, sets the variable value
# in the manifests, and copies them to /etc/kubernetes/manifests.
function start-konnectivity-server {
  echo "Start konnectivity server pods"
  prepare-log-file /var/log/konnectivity-server.log "${KONNECTIVITY_SERVER_RUNASUSER:-0}"
  prepare-konnectivity-server-manifest "8132" "8133" "8134"
}

# Calculates the following variables based on env variables, which will be used
# by the manifests of several kube-master components.
#   CLOUD_CONFIG_OPT
#   CLOUD_CONFIG_VOLUME
#   CLOUD_CONFIG_MOUNT
#   DOCKER_REGISTRY
#   FLEXVOLUME_HOSTPATH_MOUNT
#   FLEXVOLUME_HOSTPATH_VOLUME
#   INSECURE_PORT_MAPPING
function compute-master-manifest-variables {
  CLOUD_CONFIG_OPT=""
  CLOUD_CONFIG_VOLUME=""
  CLOUD_CONFIG_MOUNT=""
  if [[ -f /etc/gce.conf ]]; then
    CLOUD_CONFIG_OPT="--cloud-config=/etc/gce.conf"
    CLOUD_CONFIG_VOLUME="{\"name\": \"cloudconfigmount\",\"hostPath\": {\"path\": \"/etc/gce.conf\", \"type\": \"FileOrCreate\"}},"
    CLOUD_CONFIG_MOUNT="{\"name\": \"cloudconfigmount\",\"mountPath\": \"/etc/gce.conf\", \"readOnly\": true},"
  fi
  DOCKER_REGISTRY="registry.k8s.io"
  if [[ -n "${KUBE_DOCKER_REGISTRY:-}" ]]; then
    DOCKER_REGISTRY="${KUBE_DOCKER_REGISTRY}"
  fi

  FLEXVOLUME_HOSTPATH_MOUNT=""
  FLEXVOLUME_HOSTPATH_VOLUME=""
  if [[ -n "${VOLUME_PLUGIN_DIR:-}" ]]; then
    FLEXVOLUME_HOSTPATH_MOUNT="{ \"name\": \"flexvolumedir\", \"mountPath\": \"${VOLUME_PLUGIN_DIR}\", \"readOnly\": true},"
    FLEXVOLUME_HOSTPATH_VOLUME="{ \"name\": \"flexvolumedir\", \"hostPath\": {\"path\": \"${VOLUME_PLUGIN_DIR}\"}},"
  fi

  INSECURE_PORT_MAPPING=""
  if [[ "${ENABLE_APISERVER_INSECURE_PORT:-false}" == "true" ]]; then
    # INSECURE_PORT_MAPPING is used by sed
    # shellcheck disable=SC2089
    INSECURE_PORT_MAPPING='{ "name": "local", "containerPort": 8080, "hostPort": 8080},'
  fi
  # shellcheck disable=SC2090
  export INSECURE_PORT_MAPPING
}

# A helper function that bind mounts kubelet dirs for running mount in a chroot
function prepare-mounter-rootfs {
  echo "Prepare containerized mounter"
  mount --bind "${CONTAINERIZED_MOUNTER_HOME}" "${CONTAINERIZED_MOUNTER_HOME}"
  mount -o remount,exec "${CONTAINERIZED_MOUNTER_HOME}"
  CONTAINERIZED_MOUNTER_ROOTFS="${CONTAINERIZED_MOUNTER_HOME}/rootfs"
  mount --rbind /var/lib/kubelet/ "${CONTAINERIZED_MOUNTER_ROOTFS}/var/lib/kubelet"
  mount --make-rshared "${CONTAINERIZED_MOUNTER_ROOTFS}/var/lib/kubelet"
  mount --bind -o ro /proc "${CONTAINERIZED_MOUNTER_ROOTFS}/proc"
  mount --bind -o ro /dev "${CONTAINERIZED_MOUNTER_ROOTFS}/dev"
  cp /etc/resolv.conf "${CONTAINERIZED_MOUNTER_ROOTFS}/etc/"
}

# Updates node labels used by addons.
function update-legacy-addon-node-labels() {
  # need kube-apiserver to be ready
  until kubectl get nodes; do
    sleep 5
  done
  update-node-label "beta.kubernetes.io/metadata-proxy-ready=true,cloud.google.com/metadata-proxy-ready!=true" "cloud.google.com/metadata-proxy-ready=true"
  update-node-label "beta.kubernetes.io/kube-proxy-ds-ready=true,node.kubernetes.io/kube-proxy-ds-ready!=true" "node.kubernetes.io/kube-proxy-ds-ready=true"
  update-node-label "beta.kubernetes.io/masq-agent-ds-ready=true,node.kubernetes.io/masq-agent-ds-ready!=true" "node.kubernetes.io/masq-agent-ds-ready=true"
}

# A helper function for labeling all nodes matching a given selector.
# Runs: kubectl label --overwrite nodes -l "${1}" "${2}"
# Retries on failure
#
# $1: label selector of nodes
# $2: label to apply
function update-node-label() {
  local selector="$1"
  local label="$2"
  local retries=5
  until (( retries == 0 )); do
    if kubectl label --overwrite nodes -l "${selector}" "${label}"; then
      break
    fi
    (( retries-- ))
    sleep 3
  done
}

# Starts kubernetes controller manager.
# It prepares the log file, loads the docker image, calculates variables, sets them
# in the manifest file, and then copies the manifest file to /etc/kubernetes/manifests.
#
# Assumed vars (which are calculated in function compute-master-manifest-variables)
#   CLOUD_CONFIG_OPT
#   CLOUD_CONFIG_VOLUME
#   CLOUD_CONFIG_MOUNT
#   DOCKER_REGISTRY
function start-kube-controller-manager {
  if [[ -e "${KUBE_HOME}/bin/gke-internal-configure-helper.sh" ]]; then
    if ! deploy-kube-controller-manager-via-kube-up; then
      echo "kube-controller-manager is configured to not be deployed through kube-up."
      return
    fi
  fi
  echo "Start kubernetes controller-manager"
  create-kubeconfig "kube-controller-manager" "${KUBE_CONTROLLER_MANAGER_TOKEN}"
  prepare-log-file /var/log/kube-controller-manager.log "${KUBE_CONTROLLER_MANAGER_RUNASUSER:-0}"
  # Calculate variables and assemble the command line.
  local params=("${CONTROLLER_MANAGER_TEST_LOG_LEVEL:-"--v=4"}" "${KUBE_CONTROLLER_MANAGER_TEST_ARGS:-}" "${CONTROLLER_MANAGER_TEST_ARGS:-}" "${CLOUD_CONFIG_OPT}")
  local config_path='/etc/srv/kubernetes/kube-controller-manager/kubeconfig'
  params+=("--use-service-account-credentials")
  params+=("--cloud-provider=${CLOUD_PROVIDER_FLAG:-external}")
  params+=("--kubeconfig=${config_path}" "--authentication-kubeconfig=${config_path}" "--authorization-kubeconfig=${config_path}")
  params+=("--root-ca-file=${CA_CERT_BUNDLE_PATH}")
  params+=("--service-account-private-key-file=${SERVICEACCOUNT_KEY_PATH}")
  if [[ -n "${ENABLE_GARBAGE_COLLECTOR:-}" ]]; then
    params+=("--enable-garbage-collector=${ENABLE_GARBAGE_COLLECTOR}")
  fi
  if [[ -n "${INSTANCE_PREFIX:-}" ]]; then
    params+=("--cluster-name=${INSTANCE_PREFIX}")
  fi
  if [[ -n "${CLUSTER_IP_RANGE:-}" ]]; then
    params+=("--cluster-cidr=${CLUSTER_IP_RANGE}")
  fi
  if [[ -n "${CA_KEY:-}" ]]; then
    params+=("--cluster-signing-cert-file=${CA_CERT_PATH}")
    params+=("--cluster-signing-key-file=${CA_KEY_PATH}")
  fi
  if [[ -n "${SERVICE_CLUSTER_IP_RANGE:-}" ]]; then
    params+=("--service-cluster-ip-range=${SERVICE_CLUSTER_IP_RANGE}")
  fi
  if [[ "${NETWORK_PROVIDER:-}" == "kubenet" ]]; then
    params+=("--allocate-node-cidrs=true")
  elif [[ -n "${ALLOCATE_NODE_CIDRS:-}" ]]; then
    params+=("--allocate-node-cidrs=${ALLOCATE_NODE_CIDRS}")
  fi
  if [[ -n "${TERMINATED_POD_GC_THRESHOLD:-}" ]]; then
    params+=("--terminated-pod-gc-threshold=${TERMINATED_POD_GC_THRESHOLD}")
  fi
  if [[ -n "${FEATURE_GATES:-}" ]]; then
    params+=("--feature-gates=${FEATURE_GATES}")
  fi
  if [[ -n "${KUBE_EMULATED_VERSION:-}" ]]; then
    params+=("--emulated-version=kube=${KUBE_EMULATED_VERSION}")
  fi
  if [[ -n "${VOLUME_PLUGIN_DIR:-}" ]]; then
    params+=("--flex-volume-plugin-dir=${VOLUME_PLUGIN_DIR}")
  fi
  if [[ -n "${CLUSTER_SIGNING_DURATION:-}" ]]; then
    params+=("--cluster-signing-duration=$CLUSTER_SIGNING_DURATION")
  fi
  if [[ -n "${PV_RECYCLER_OVERRIDE_TEMPLATE:-}" ]]; then
    params+=("--pv-recycler-pod-template-filepath-nfs=$PV_RECYCLER_OVERRIDE_TEMPLATE")
    params+=("--pv-recycler-pod-template-filepath-hostpath=$PV_RECYCLER_OVERRIDE_TEMPLATE")
  fi
  if [[ -n "${RUN_CONTROLLERS:-}" ]]; then
    params+=("--controllers=${RUN_CONTROLLERS}")
  fi

  local -r kube_rc_docker_tag=$(cat /home/kubernetes/kube-docker-files/kube-controller-manager.docker_tag)
  local container_env=""
  if [[ -n "${ENABLE_CACHE_MUTATION_DETECTOR:-}" ]]; then
    container_env="{\"name\": \"KUBE_CACHE_MUTATION_DETECTOR\", \"value\": \"${ENABLE_CACHE_MUTATION_DETECTOR}\"}"
  fi
  if [[ -n "${ENABLE_KUBE_WATCHLIST_INCONSISTENCY_DETECTOR:-}" ]]; then
    if [[ -n "${container_env}" ]]; then
      container_env="${container_env}, "
    fi
    container_env+="{\"name\": \"KUBE_WATCHLIST_INCONSISTENCY_DETECTOR\", \"value\": \"${ENABLE_KUBE_WATCHLIST_INCONSISTENCY_DETECTOR}\"}"
  fi
  if [[ -n "${ENABLE_KUBE_LIST_FROM_CACHE_INCONSISTENCY_DETECTOR:-}" ]]; then
    if [[ -n "${container_env}" ]]; then
      container_env="${container_env}, "
    fi
    container_env+="{\"name\": \"KUBE_LIST_FROM_CACHE_INCONSISTENCY_DETECTOR\", \"value\": \"${ENABLE_KUBE_LIST_FROM_CACHE_INCONSISTENCY_DETECTOR}\"}"
  fi
  if [[ -n "${container_env}" ]]; then
    container_env="\"env\":[${container_env}],"
  fi
  local paramstring
  paramstring="$(convert-manifest-params "${params[*]}")"
  local -r src_file="${KUBE_HOME}/kube-manifests/kubernetes/gci-trusty/kube-controller-manager.manifest"
  # Evaluate variables.
  sed -i -e "s@{{pillar\['kube_docker_registry'\]}}@${DOCKER_REGISTRY}@g" "${src_file}"
  sed -i -e "s@{{pillar\['kube-controller-manager_docker_tag'\]}}@${kube_rc_docker_tag}@g" "${src_file}"
  sed -i -e "s@{{params}}@${paramstring}@g" "${src_file}"
  sed -i -e "s@{{container_env}}@${container_env}@g" "${src_file}"
  sed -i -e "s@{{cloud_config_mount}}@${CLOUD_CONFIG_MOUNT}@g" "${src_file}"
  sed -i -e "s@{{cloud_config_volume}}@${CLOUD_CONFIG_VOLUME}@g" "${src_file}"
  sed -i -e "s@{{additional_cloud_config_mount}}@@g" "${src_file}"
  sed -i -e "s@{{additional_cloud_config_volume}}@@g" "${src_file}"
  sed -i -e "s@{{pv_recycler_mount}}@${PV_RECYCLER_MOUNT}@g" "${src_file}"
  sed -i -e "s@{{pv_recycler_volume}}@${PV_RECYCLER_VOLUME}@g" "${src_file}"
  sed -i -e "s@{{flexvolume_hostpath_mount}}@${FLEXVOLUME_HOSTPATH_MOUNT}@g" "${src_file}"
  sed -i -e "s@{{flexvolume_hostpath}}@${FLEXVOLUME_HOSTPATH_VOLUME}@g" "${src_file}"
  sed -i -e "s@{{cpurequest}}@${KUBE_CONTROLLER_MANAGER_CPU_REQUEST}@g" "${src_file}"

  if [[ -n "${KUBE_CONTROLLER_MANAGER_RUNASUSER:-}" && -n "${KUBE_CONTROLLER_MANAGER_RUNASGROUP:-}" ]]; then
    sed -i -e "s@{{runAsUser}}@\"runAsUser\": ${KUBE_CONTROLLER_MANAGER_RUNASUSER},@g" "${src_file}"
    sed -i -e "s@{{runAsGroup}}@\"runAsGroup\":${KUBE_CONTROLLER_MANAGER_RUNASGROUP},@g" "${src_file}"
    sed -i -e "s@{{supplementalGroups}}@\"supplementalGroups\": [ ${KUBE_PKI_READERS_GROUP} ],@g" "${src_file}"
  else
    sed -i -e "s@{{runAsUser}}@@g" "${src_file}"
    sed -i -e "s@{{runAsGroup}}@@g" "${src_file}"
    sed -i -e "s@{{supplementalGroups}}@@g" "${src_file}"
  fi

  cp "${src_file}" /etc/kubernetes/manifests
}

# Starts cloud controller manager.
# It prepares the log file, loads the docker image, calculates variables, sets them
# in the manifest file, and then copies the manifest file to /etc/kubernetes/manifests.
#
# Assumed vars (which are calculated in function compute-master-manifest-variables)
#   CLOUD_CONFIG_OPT
#   CLOUD_CONFIG_VOLUME
#   CLOUD_CONFIG_MOUNT
#   DOCKER_REGISTRY
function start-cloud-controller-manager {
  echo "Start cloud provider controller-manager"
  setup-addon-manifests "addons" "cloud-controller-manager"

  create-kubeconfig "cloud-controller-manager" "${CLOUD_CONTROLLER_MANAGER_TOKEN}"
  echo "Preparing cloud provider controller-manager log file"
  prepare-log-file /var/log/cloud-controller-manager.log "${CLOUD_CONTROLLER_MANAGER_RUNASUSER:-0}"
  # Calculate variables and assemble the command line.
  local params=("${CONTROLLER_MANAGER_TEST_LOG_LEVEL:-"--v=4"}" "${CONTROLLER_MANAGER_TEST_ARGS:-}" "${CLOUD_CONFIG_OPT}")
  params+=("--secure-port=10258")
  params+=("--use-service-account-credentials")
  params+=("--cloud-provider=gce")
  params+=("--concurrent-node-syncs=10")
  params+=("--kubeconfig=/etc/srv/kubernetes/cloud-controller-manager/kubeconfig")
  params+=("--authorization-kubeconfig=/etc/srv/kubernetes/cloud-controller-manager/kubeconfig")
  params+=("--authentication-kubeconfig=/etc/srv/kubernetes/cloud-controller-manager/kubeconfig")
  if [[ -n "${INSTANCE_PREFIX:-}" ]]; then
    params+=("--cluster-name=${INSTANCE_PREFIX}")
  fi
  if [[ -n "${CLUSTER_IP_RANGE:-}" ]]; then
    params+=("--cluster-cidr=${CLUSTER_IP_RANGE}")
  fi
  if [[ -n "${CONCURRENT_SERVICE_SYNCS:-}" ]]; then
    params+=("--concurrent-service-syncs=${CONCURRENT_SERVICE_SYNCS}")
  fi
  if [[ "${NETWORK_PROVIDER:-}" == "kubenet" ]]; then
    params+=("--allocate-node-cidrs=true")
  elif [[ -n "${ALLOCATE_NODE_CIDRS:-}" ]]; then
    params+=("--allocate-node-cidrs=${ALLOCATE_NODE_CIDRS}")
  fi
  if [[ "${ENABLE_IP_ALIASES:-}" == 'true' ]]; then
    params+=("--cidr-allocator-type=${NODE_IPAM_MODE}")
    params+=("--configure-cloud-routes=false")
  fi
  if [[ -n "${FEATURE_GATES:-}" ]]; then
    # remove non-GCP feature gates, since the CCM will early exit
    # if given a feature gate it doesn't recognize
    echo "Setting feature gates for cloud provider controller-manager from ${CCM_FEATURE_GATES}"
    local CCM_FEATURE_GATES_FILTER
    CCM_FEATURE_GATES_FILTER=$(echo "${CCM_FEATURE_GATES}" | sed "s/^/(/" | sed "s/,/=[^,]*|/g" | sed "s/$/=[^,]*)/")
    echo "Computing safe feature gates for cloud provider controller-manager from ${FEATURE_GATES} and filter ${CCM_FEATURE_GATES_FILTER}"
    local safe_feature_gates
    safe_feature_gates=$(echo "${FEATURE_GATES}" | { grep -E -o "(${CCM_FEATURE_GATES_FILTER})" || true; } | tr "\n" "," | sed "s/,$//")
    echo "Setting safe feature gates for cloud provider controller-manager with ${safe_feature_gates}"
    if [[ -n "${safe_feature_gates:-}" ]]; then
      params+=("--feature-gates=${safe_feature_gates}")
      echo "Computing unsafe feature gates for cloud provider controller-manager from ${CCM_FEATURE_GATES_FILTER}"
      local filtered_feature_gates
      filtered_feature_gates=$(echo "${FEATURE_GATES}" | sed "s/,/\n/g" | { grep -E -v "(${CCM_FEATURE_GATES_FILTER})" || true; } | sed -z "s/\n/,/g;s/,$/\n/")
      echo "Feature gates that did not pass through the GCP filter:" "${filtered_feature_gates}"
    else
      echo "None of the given feature gates (${FEATURE_GATES}) were found to be safe to pass to the CCM"
    fi
  fi
  if [[ -n "${RUN_CCM_CONTROLLERS:-}" ]]; then
    params+=("--controllers=${RUN_CCM_CONTROLLERS}")
  fi

  echo "Converting manifest for cloud provider controller-manager"
  local paramstring
  paramstring="$(convert-manifest-params "${params[*]}")"
  local container_env=""
  if [[ -n "${ENABLE_CACHE_MUTATION_DETECTOR:-}" ]]; then
    container_env="{\"name\": \"KUBE_CACHE_MUTATION_DETECTOR\", \"value\": \"${ENABLE_CACHE_MUTATION_DETECTOR}\"}"
  fi
  if [[ -n "${ENABLE_KUBE_WATCHLIST_INCONSISTENCY_DETECTOR:-}" ]]; then
    if [[ -n "${container_env}" ]]; then
      container_env="${container_env}, "
    fi
    container_env+="{\"name\": \"KUBE_WATCHLIST_INCONSISTENCY_DETECTOR\", \"value\": \"${ENABLE_KUBE_WATCHLIST_INCONSISTENCY_DETECTOR}\"}"
  fi
  if [[ -n "${ENABLE_KUBE_LIST_FROM_CACHE_INCONSISTENCY_DETECTOR:-}" ]]; then
    if [[ -n "${container_env}" ]]; then
      container_env="${container_env}, "
    fi
    container_env+="{\"name\": \"KUBE_LIST_FROM_CACHE_INCONSISTENCY_DETECTOR\", \"value\": \"${ENABLE_KUBE_LIST_FROM_CACHE_INCONSISTENCY_DETECTOR}\"}"
  fi
  if [[ -n "${container_env}" ]]; then
    container_env="\"env\":[${container_env}],"
  fi

  echo "Applying over-rides for manifest for cloud provider controller-manager"
  local -r src_file="${KUBE_HOME}/kube-manifests/kubernetes/gci-trusty/cloud-controller-manager.manifest"
  # Evaluate variables.
  sed -i -e "s@{{pillar\['kube_docker_registry'\]}}@${DOCKER_REGISTRY}@g" "${src_file}"
  sed -i -e "s@{{params}}@${paramstring}@g" "${src_file}"
  sed -i -e "s@{{container_env}}@${container_env}@g" "${src_file}"
  sed -i -e "s@{{cloud_config_mount}}@${CLOUD_CONFIG_MOUNT}@g" "${src_file}"
  sed -i -e "s@{{cloud_config_volume}}@${CLOUD_CONFIG_VOLUME}@g" "${src_file}"
  sed -i -e "s@{{additional_cloud_config_mount}}@@g" "${src_file}"
  sed -i -e "s@{{additional_cloud_config_volume}}@@g" "${src_file}"
  sed -i -e "s@{{pv_recycler_mount}}@${PV_RECYCLER_MOUNT}@g" "${src_file}"
  sed -i -e "s@{{pv_recycler_volume}}@${PV_RECYCLER_VOLUME}@g" "${src_file}"
  sed -i -e "s@{{flexvolume_hostpath_mount}}@${FLEXVOLUME_HOSTPATH_MOUNT}@g" "${src_file}"
  sed -i -e "s@{{flexvolume_hostpath}}@${FLEXVOLUME_HOSTPATH_VOLUME}@g" "${src_file}"
  sed -i -e "s@{{cpurequest}}@${CLOUD_CONTROLLER_MANAGER_CPU_REQUEST}@g" "${src_file}"

  if [[ -n "${CLOUD_CONTROLLER_MANAGER_RUNASUSER:-}" && -n "${CLOUD_CONTROLLER_MANAGER_RUNASGROUP:-}" ]]; then
    #run-cloud-controller-manager-as-non-root
    sed -i -e "s@{{runAsUser}}@\"runAsUser\": ${CLOUD_CONTROLLER_MANAGER_RUNASUSER},@g" "${src_file}"
    sed -i -e "s@{{runAsGroup}}@\"runAsGroup\":${CLOUD_CONTROLLER_MANAGER_RUNASGROUP},@g" "${src_file}"
    sed -i -e "s@{{supplementalGroups}}@\"supplementalGroups\": [ ${KUBE_PKI_READERS_GROUP} ],@g" "${src_file}"
  else
    sed -i -e "s@{{runAsUser}}@@g" "${src_file}"
    sed -i -e "s@{{runAsGroup}}@@g" "${src_file}"
    sed -i -e "s@{{supplementalGroups}}@@g" "${src_file}"
  fi

  echo "Writing manifest for cloud provider controller-manager"
  cp "${src_file}" /etc/kubernetes/manifests

  setup-addon-manifests "addons" "cloud-pvl-admission"
  setup-cloud-pvl-admission-manifest
}

# Starts kubernetes scheduler.
# It prepares the log file, loads the docker image, calculates variables, sets them
# in the manifest file, and then copies the manifest file to /etc/kubernetes/manifests.
#
# Assumed vars (which are calculated in compute-master-manifest-variables)
#   DOCKER_REGISTRY
function start-kube-scheduler {
  if [[ -e "${KUBE_HOME}/bin/gke-internal-configure-helper.sh" ]]; then
    if ! deploy-kube-scheduler-via-kube-up; then
      echo "kube-scheduler is configured to not be deployed through kube-up."
      return
    fi
  fi
  echo "Start kubernetes scheduler"
  create-kubeconfig "kube-scheduler" "${KUBE_SCHEDULER_TOKEN}"
  # User and group should never contain characters that need to be quoted
  # shellcheck disable=SC2086
  prepare-log-file /var/log/kube-scheduler.log ${KUBE_SCHEDULER_RUNASUSER:-2001}

  # Calculate variables and set them in the manifest.
  params=("${SCHEDULER_TEST_LOG_LEVEL:-"--v=2"}" "${SCHEDULER_TEST_ARGS:-}")
  if [[ -n "${FEATURE_GATES:-}" ]]; then
    params+=("--feature-gates=${FEATURE_GATES}")
  fi
  if [[ -n "${KUBE_EMULATED_VERSION:-}" ]]; then
    params+=("--emulated-version=kube=${KUBE_EMULATED_VERSION}")
  fi

  # Scheduler Component Config takes precedence over some flags.
  if [[ -n "${KUBE_SCHEDULER_CONFIG:-}" ]]; then
    create-kube-scheduler-config
    params+=("--config=/etc/srv/kubernetes/kube-scheduler/config")
  else
    params+=("--kubeconfig=/etc/srv/kubernetes/kube-scheduler/kubeconfig")
    if [[ -n "${SCHEDULER_POLICY_CONFIG:-}" ]]; then
      create-kubescheduler-policy-config
      params+=("--use-legacy-policy-config")
      params+=("--policy-config-file=/etc/srv/kubernetes/kube-scheduler/policy-config")
    fi
  fi

  local config_path
  config_path='/etc/srv/kubernetes/kube-scheduler/kubeconfig'
  params+=("--authentication-kubeconfig=${config_path}" "--authorization-kubeconfig=${config_path}")

  local paramstring
  paramstring="$(convert-manifest-params "${params[*]}")"
  local -r kube_scheduler_docker_tag=$(cat "${KUBE_HOME}/kube-docker-files/kube-scheduler.docker_tag")

  # Remove salt comments and replace variables with values.
  local -r src_file="${KUBE_HOME}/kube-manifests/kubernetes/gci-trusty/kube-scheduler.manifest"

  sed -i -e "s@{{params}}@${paramstring}@g" "${src_file}"
  sed -i -e "s@{{pillar\['kube_docker_registry'\]}}@${DOCKER_REGISTRY}@g" "${src_file}"
  sed -i -e "s@{{pillar\['kube-scheduler_docker_tag'\]}}@${kube_scheduler_docker_tag}@g" "${src_file}"
  sed -i -e "s@{{cpurequest}}@${KUBE_SCHEDULER_CPU_REQUEST}@g" "${src_file}"
  sed -i -e "s@{{runAsUser}}@${KUBE_SCHEDULER_RUNASUSER:-2001}@g" "${src_file}"
  sed -i -e "s@{{runAsGroup}}@${KUBE_SCHEDULER_RUNASGROUP:-2001}@g" "${src_file}"
  cp "${src_file}" /etc/kubernetes/manifests
}

# Starts cluster autoscaler.
# Assumed vars (which are calculated in function compute-master-manifest-variables)
#   CLOUD_CONFIG_OPT
#   CLOUD_CONFIG_VOLUME
#   CLOUD_CONFIG_MOUNT
function start-cluster-autoscaler {
  if [[ "${ENABLE_CLUSTER_AUTOSCALER:-}" == "true" ]]; then
    echo "Start kubernetes cluster autoscaler"
    setup-addon-manifests "addons" "rbac/cluster-autoscaler"
    create-kubeconfig "cluster-autoscaler" "${KUBE_CLUSTER_AUTOSCALER_TOKEN}"
    prepare-log-file /var/log/cluster-autoscaler.log "${CLUSTER_AUTOSCALER_RUNASUSER:-0}"

    # Remove salt comments and replace variables with values
    local -r src_file="${KUBE_HOME}/kube-manifests/kubernetes/gci-trusty/cluster-autoscaler.manifest"

    local params
    read -r -a params <<< "${AUTOSCALER_MIG_CONFIG}"
    params+=("${CLOUD_CONFIG_OPT}" "${AUTOSCALER_EXPANDER_CONFIG:---expander=price}")
    params+=("--kubeconfig=/etc/srv/kubernetes/cluster-autoscaler/kubeconfig")

    # split the params into separate arguments passed to binary
    local params_split
    params_split=$(eval 'for param in "${params[@]}"; do echo -n \""$param"\",; done')
    params_split=${params_split%?}

    sed -i -e "s@{{params}}@${params_split}@g" "${src_file}"
    sed -i -e "s@{{cloud_config_mount}}@${CLOUD_CONFIG_MOUNT}@g" "${src_file}"
    sed -i -e "s@{{cloud_config_volume}}@${CLOUD_CONFIG_VOLUME}@g" "${src_file}"
    sed -i -e "s@{%.*%}@@g" "${src_file}"

    if [[ -n "${CLUSTER_AUTOSCALER_RUNASUSER:-}" && -n "${CLUSTER_AUTOSCALER_RUNASGROUP:-}" ]]; then
      #run-cluster-autoscaler-as-non-root
      sed -i -e "s@{{runAsUser}}@\"runAsUser\": ${CLUSTER_AUTOSCALER_RUNASUSER},@g" "${src_file}"
      sed -i -e "s@{{runAsGroup}}@\"runAsGroup\":${CLUSTER_AUTOSCALER_RUNASGROUP},@g" "${src_file}"
      sed -i -e "s@{{supplementalGroups}}@\"supplementalGroups\": [ ${KUBE_PKI_READERS_GROUP} ],@g" "${src_file}"
    else
      sed -i -e "s@{{runAsUser}}@@g" "${src_file}"
      sed -i -e "s@{{runAsGroup}}@@g" "${src_file}"
      sed -i -e "s@{{supplementalGroups}}@@g" "${src_file}"
    fi

    cp "${src_file}" /etc/kubernetes/manifests
  fi
}

# A helper function for setting up addon manifests.
#
# $1: addon category under /etc/kubernetes
# $2: manifest source dir
# $3: (optional) auxiliary manifest source dir
function setup-addon-manifests {
  local -r src_dir="${KUBE_HOME}/kube-manifests/kubernetes/gci-trusty"
  local -r dst_dir="/etc/kubernetes/$1/$2"

  copy-manifests "${src_dir}/$2" "${dst_dir}"
}

# A function that downloads extra addons from a URL and puts them in the GCI
# manifests directory.
function download-extra-addons {
  local -r out_dir="${KUBE_HOME}/kube-manifests/kubernetes/gci-trusty/gce-extras"

  mkdir -p "${out_dir}"

  # shellcheck disable=SC2206
  local curl_cmd=(
    "curl"
    ${CURL_FLAGS}
  )
  if [[ -n "${EXTRA_ADDONS_HEADER:-}" ]]; then
    curl_cmd+=("-H" "${EXTRA_ADDONS_HEADER}")
  fi
  curl_cmd+=("-o" "${out_dir}/extras.json")
  curl_cmd+=("${EXTRA_ADDONS_URL}")

  "${curl_cmd[@]}"
}

# A function that fetches a GCE metadata value and echoes it out.
# Args:
#   $1 : URL path after /computeMetadata/v1/ (without heading slash).
#   $2 : An optional default value to echo out if the fetch fails.
#
# NOTE: this function is duplicated in configure.sh, any changes here should be
# duplicated there as well.
function get-metadata-value {
  local default="${2:-}"

  local status
  # shellcheck disable=SC2086
  curl ${CURL_FLAGS} \
    -H 'Metadata-Flavor: Google' \
    "http://metadata/computeMetadata/v1/${1}" \
  || status="$?"
  status="${status:-0}"

  if [[ "${status}" -eq 0 || -z "${default}" ]]; then
    return "${status}"
  else
    echo "${default}"
  fi
}

# A helper function for copying manifests and setting dir/files
# permissions.
#
# $1: absolute source dir
# $2: absolute destination dir
function copy-manifests {
  local -r src_dir="$1"
  local -r dst_dir="$2"
  if [[ ! -d "${dst_dir}" ]]; then
    mkdir -p "${dst_dir}"
  fi
  if [[ -n "$(ls "${src_dir}"/*.yaml 2>/dev/null)" ]]; then
    cp "${src_dir}/"*.yaml "${dst_dir}"
  fi
  if [[ -n "$(ls "${src_dir}"/*.json 2>/dev/null)" ]]; then
    cp "${src_dir}/"*.json "${dst_dir}"
  fi
  if [[ -n "$(ls "${src_dir}"/*.yaml.in 2>/dev/null)" ]]; then
    cp "${src_dir}/"*.yaml.in "${dst_dir}"
  fi
  chown -R root:root "${dst_dir}"
  chmod 755 "${dst_dir}"
  chmod 644 "${dst_dir}"/*
}

# Fluentd resources are modified using ScalingPolicy CR, which may not be
# available at this point. Run this as a background process.
function wait-for-apiserver-and-update-fluentd {
  local any_overrides=false
  if [[ -n "${FLUENTD_GCP_MEMORY_LIMIT:-}" ]]; then
    any_overrides=true
  fi
  if [[ -n "${FLUENTD_GCP_CPU_REQUEST:-}" ]]; then
    any_overrides=true
  fi
  if [[ -n "${FLUENTD_GCP_MEMORY_REQUEST:-}" ]]; then
    any_overrides=true
  fi
  if ! $any_overrides; then
    # Nothing to do here.
    exit
  fi

  # Wait until ScalingPolicy CRD is in place.
  until kubectl get scalingpolicies.scalingpolicy.kope.io
  do
    sleep 10
  done

  # Single-shot, not managed by addon manager. Can be later modified or removed
  # at will.
  cat <<EOF | kubectl apply -f -
apiVersion: scalingpolicy.kope.io/v1alpha1
kind: ScalingPolicy
metadata:
  name: fluentd-gcp-scaling-policy
  namespace: kube-system
spec:
  containers:
  - name: fluentd-gcp
    resources:
      requests:
      - resource: cpu
        base: ${FLUENTD_GCP_CPU_REQUEST:-}
      - resource: memory
        base: ${FLUENTD_GCP_MEMORY_REQUEST:-}
      limits:
      - resource: memory
        base: ${FLUENTD_GCP_MEMORY_LIMIT:-}
EOF
}

# Trigger background process that will ultimately update fluentd resource
# requirements.
function start-fluentd-resource-update {
  wait-for-apiserver-and-update-fluentd &
}

# VolumeSnapshot CRDs and controller are installed by cluster addon manager,
# which may not be available at this point. Run this as a background process.
function wait-for-volumesnapshot-crd-and-controller {
  # Wait until volumesnapshot CRDs and controller are in place.
  echo "Wait until volume snapshot CRDs are installed"
  until kubectl get volumesnapshotclasses.snapshot.storage.k8s.io
  do
    sleep 10
  done

  until kubectl get volumesnapshotcontents.snapshot.storage.k8s.io
  do
    sleep 10
  done

  until kubectl get volumesnapshots.snapshot.storage.k8s.io
  do
    sleep 10
  done

  echo "Wait until volume snapshot RBAC rules are installed"
  until kubectl get clusterrolebinding volume-snapshot-controller-role
  do
    sleep 10
  done

  echo "Wait until volume snapshot controller is installed"
  until kubectl get statefulset volume-snapshot-controller | grep volume-snapshot-controller | grep "1/1"
  do
    sleep 10
  done
}

# Trigger background process that will wait for volumesnapshot CRDs
# and snapshot-controller to be installed
function start-volumesnapshot-crd-and-controller {
  wait-for-volumesnapshot-crd-and-controller &
}

# Update {{ fluentd_container_runtime_service }} with actual container runtime name,
# and {{ container_runtime_endpoint }} with actual container runtime
# endpoint.
function update-container-runtime {
  local -r file="$1"
  local -r container_runtime_endpoint="${CONTAINER_RUNTIME_ENDPOINT:-unix:///run/containerd/containerd.sock}"
  sed -i \
    -e "s@{{ *fluentd_container_runtime_service *}}@${FLUENTD_CONTAINER_RUNTIME_SERVICE:-${CONTAINER_RUNTIME_NAME:-containerd}}@g" \
    -e "s@{{ *container_runtime_endpoint *}}@${container_runtime_endpoint#unix://}@g" \
    "${file}"
}

# Remove configuration in yaml file if node journal is not enabled.
function update-node-journal {
  local -r configmap_yaml="$1"
  if [[ "${ENABLE_NODE_JOURNAL:-}" != "true" ]]; then
    # Removes all lines between two patterns (throws away node-journal)
    sed -i -e "/# BEGIN_NODE_JOURNAL/,/# END_NODE_JOURNAL/d" "${configmap_yaml}"
  fi
}

# Updates parameters in yaml file for prometheus-to-sd configuration, or
# removes component if it is disabled.
function update-prometheus-to-sd-parameters {
  if [[ "${ENABLE_PROMETHEUS_TO_SD:-}" == "true" ]]; then
    sed -i -e "s@{{ *prometheus_to_sd_prefix *}}@${PROMETHEUS_TO_SD_PREFIX}@g" "$1"
    sed -i -e "s@{{ *prometheus_to_sd_endpoint *}}@${PROMETHEUS_TO_SD_ENDPOINT}@g" "$1"
  else
    # Removes all lines between two patterns (throws away prometheus-to-sd)
    sed -i -e "/# BEGIN_PROMETHEUS_TO_SD/,/# END_PROMETHEUS_TO_SD/d" "$1"
   fi
}

# Updates parameters in yaml file for prometheus-to-sd configuration in daemon sets, or
# removes component if it is disabled.
function update-daemon-set-prometheus-to-sd-parameters {
  if [[ "${DISABLE_PROMETHEUS_TO_SD_IN_DS:-}" == "true" ]]; then
    # Removes all lines between two patterns (throws away prometheus-to-sd)
    sed -i -e "/# BEGIN_PROMETHEUS_TO_SD/,/# END_PROMETHEUS_TO_SD/d" "$1"
  else
    update-prometheus-to-sd-parameters "$1"
  fi
}

# Updates parameters in yaml file for event-exporter configuration
function update-event-exporter {
    local -r stackdriver_resource_model="${LOGGING_STACKDRIVER_RESOURCE_TYPES:-old}"
    sed -i -e "s@{{ exporter_sd_resource_model }}@${stackdriver_resource_model}@g" "$1"
    sed -i -e "s@{{ exporter_sd_endpoint }}@${STACKDRIVER_ENDPOINT:-}@g" "$1"
}

# Sets up the manifests of coreDNS for k8s addons.
function setup-coredns-manifest {
  setup-addon-manifests "addons" "0-dns/coredns"
  local -r coredns_file="${dst_dir}/0-dns/coredns/coredns.yaml"
  mv "${dst_dir}/0-dns/coredns/coredns.yaml.in" "${coredns_file}"
  # Replace the salt configurations with variable values.
  sed -i -e "s@dns_domain@${DNS_DOMAIN}@g" "${coredns_file}"
  sed -i -e "s@dns_server@${DNS_SERVER_IP}@g" "${coredns_file}"
  sed -i -e "s@{{ *pillar\['service_cluster_ip_range'\] *}}@${SERVICE_CLUSTER_IP_RANGE}@g" "${coredns_file}"
  sed -i -e "s@dns_memory_limit@${DNS_MEMORY_LIMIT:-170Mi}@g" "${coredns_file}"

  if [[ "${ENABLE_DNS_HORIZONTAL_AUTOSCALER:-}" == "true" ]]; then
    setup-addon-manifests "addons" "dns-horizontal-autoscaler" "gce"
    local -r dns_autoscaler_file="${dst_dir}/dns-horizontal-autoscaler/dns-horizontal-autoscaler.yaml"
    sed -i'' -e "s@{{.Target}}@${COREDNS_AUTOSCALER}@g" "${dns_autoscaler_file}"
  fi
}

# Sets up the manifests of Fluentd configmap and yamls for k8s addons.
function setup-fluentd {
  local -r dst_dir="$1"
  local -r fluentd_gcp_yaml="${dst_dir}/fluentd-gcp/fluentd-gcp-ds.yaml"
  local -r fluentd_gcp_scaler_yaml="${dst_dir}/fluentd-gcp/scaler-deployment.yaml"
  # Ingest logs against new resources like "k8s_container" and "k8s_node" if
  # LOGGING_STACKDRIVER_RESOURCE_TYPES is "new".
  # Ingest logs against old resources like "gke_container" and "gce_instance" if
  # LOGGING_STACKDRIVER_RESOURCE_TYPES is "old".
  if [[ "${LOGGING_STACKDRIVER_RESOURCE_TYPES:-old}" == "new" ]]; then
    local -r fluentd_gcp_configmap_yaml="${dst_dir}/fluentd-gcp/fluentd-gcp-configmap.yaml"
    fluentd_gcp_configmap_name="fluentd-gcp-config"
  else
    local -r fluentd_gcp_configmap_yaml="${dst_dir}/fluentd-gcp/fluentd-gcp-configmap-old.yaml"
    fluentd_gcp_configmap_name="fluentd-gcp-config-old"
  fi
  sed -i -e "s@{{ fluentd_gcp_configmap_name }}@${fluentd_gcp_configmap_name}@g" "${fluentd_gcp_yaml}"
  fluentd_gcp_yaml_version="${FLUENTD_GCP_YAML_VERSION:-v3.2.0}"
  sed -i -e "s@{{ fluentd_gcp_yaml_version }}@${fluentd_gcp_yaml_version}@g" "${fluentd_gcp_yaml}"
  sed -i -e "s@{{ fluentd_gcp_yaml_version }}@${fluentd_gcp_yaml_version}@g" "${fluentd_gcp_scaler_yaml}"
  fluentd_gcp_version="${FLUENTD_GCP_VERSION:-1.6.17}"
  sed -i -e "s@{{ fluentd_gcp_version }}@${fluentd_gcp_version}@g" "${fluentd_gcp_yaml}"
  update-daemon-set-prometheus-to-sd-parameters "${fluentd_gcp_yaml}"
  start-fluentd-resource-update "${fluentd_gcp_yaml}"
  update-container-runtime "${fluentd_gcp_configmap_yaml}"
  update-node-journal "${fluentd_gcp_configmap_yaml}"
}

# Sets up the manifests of kube-dns for k8s addons.
function setup-kube-dns-manifest {
  setup-addon-manifests "addons" "0-dns/kube-dns"
  local -r kubedns_file="${dst_dir}/0-dns/kube-dns/kube-dns.yaml"
  mv "${dst_dir}/0-dns/kube-dns/kube-dns.yaml.in" "${kubedns_file}"
  if [ -n "${CUSTOM_KUBE_DNS_YAML:-}" ]; then
    # Replace with custom GKE kube-dns deployment.
    cat > "${kubedns_file}" <<EOF
$CUSTOM_KUBE_DNS_YAML
EOF
    update-prometheus-to-sd-parameters "${kubedns_file}"
  fi
  # Replace the salt configurations with variable values.
  sed -i -e "s@dns_domain@${DNS_DOMAIN}@g" "${kubedns_file}"
  sed -i -e "s@dns_server@${DNS_SERVER_IP}@g" "${kubedns_file}"
  sed -i -e "s@dns_memory_limit@${DNS_MEMORY_LIMIT:-170Mi}@g" "${kubedns_file}"

  if [[ "${ENABLE_DNS_HORIZONTAL_AUTOSCALER:-}" == "true" ]]; then
    setup-addon-manifests "addons" "dns-horizontal-autoscaler" "gce"
    local -r dns_autoscaler_file="${dst_dir}/dns-horizontal-autoscaler/dns-horizontal-autoscaler.yaml"
    sed -i'' -e "s@{{.Target}}@${KUBEDNS_AUTOSCALER}@g" "${dns_autoscaler_file}"
  fi
}

# Sets up the manifests of local dns cache agent for k8s addons.
function setup-nodelocaldns-manifest {
  setup-addon-manifests "addons" "0-dns/nodelocaldns"
  local -r localdns_file="${dst_dir}/0-dns/nodelocaldns/nodelocaldns.yaml"
  setup-addon-custom-yaml "addons" "0-dns/nodelocaldns" "nodelocaldns.yaml" "${CUSTOM_NODELOCAL_DNS_YAML:-}"
  # eventually all the __PILLAR__ stuff will be gone, but theyre still in nodelocaldns for backward compat.
  sed -i -e "s/__PILLAR__DNS__DOMAIN__/${DNS_DOMAIN}/g" "${localdns_file}"
  sed -i -e "s/__PILLAR__DNS__SERVER__/${DNS_SERVER_IP}/g" "${localdns_file}"
  sed -i -e "s/__PILLAR__LOCAL__DNS__/${LOCAL_DNS_IP}/g" "${localdns_file}"
}

# Sets up the manifests of netd for k8s addons.
function setup-netd-manifest {
  local -r netd_file="${dst_dir}/netd/netd.yaml"
  mkdir -p "${dst_dir}/netd"
  touch "${netd_file}"
  if [ -n "${CUSTOM_NETD_YAML:-}" ]; then
    # Replace with custom GCP netd deployment.
    cat > "${netd_file}" <<EOF
$CUSTOM_NETD_YAML
EOF
  fi
}

# A helper function to set up a custom yaml for a k8s addon.
#
# $1: addon category under /etc/kubernetes
# $2: manifest source dir
# $3: manifest file
# $4: custom yaml
function setup-addon-custom-yaml {
  local -r manifest_path="/etc/kubernetes/$1/$2/$3"
  local -r custom_yaml="$4"
  if [ -n "${custom_yaml:-}" ]; then
    # Replace with custom manifest.
    cat > "${manifest_path}" <<EOF
$custom_yaml
EOF
  fi
}

# Prepares the manifests of k8s addons, and starts the addon manager.
# Vars assumed:
#   CLUSTER_NAME
function start-kube-addons {
  echo "Prepare kube-addons manifests and start kube addon manager"
  local -r src_dir="${KUBE_HOME}/kube-manifests/kubernetes/gci-trusty"
  local -r dst_dir="/etc/kubernetes/addons"

  create-kubeconfig "addon-manager" "${ADDON_MANAGER_TOKEN}"
  # User and group should never contain characters that need to be quoted
  # shellcheck disable=SC2086
  prepare-log-file /var/log/kube-addon-manager.log ${KUBE_ADDON_MANAGER_RUNASUSER:-2002}

  # prep addition kube-up specific rbac objects
  setup-addon-manifests "addons" "rbac/kubelet-api-auth"
  setup-addon-manifests "addons" "rbac/kubelet-cert-rotation"
  if [[ "${REGISTER_MASTER_KUBELET:-false}" == "true" ]]; then
    setup-addon-manifests "addons" "rbac/legacy-kubelet-user"
  else
    setup-addon-manifests "addons" "rbac/legacy-kubelet-user-disable"
  fi

  # Set up manifests of other addons.
  if [[ "${KUBE_PROXY_DAEMONSET:-}" == "true" ]] && [[ "${KUBE_PROXY_DISABLE:-}" != "true" ]]; then
    if [ -n "${CUSTOM_KUBE_PROXY_YAML:-}" ]; then
      # Replace with custom GKE kube proxy.
      cat > "$src_dir/kube-proxy/kube-proxy-ds.yaml" <<EOF
$CUSTOM_KUBE_PROXY_YAML
EOF
      update-daemon-set-prometheus-to-sd-parameters "$src_dir/kube-proxy/kube-proxy-ds.yaml"
    fi
    prepare-kube-proxy-manifest-variables "$src_dir/kube-proxy/kube-proxy-ds.yaml"
    setup-addon-manifests "addons" "kube-proxy"
  fi
  if [[ "${ENABLE_CLUSTER_LOGGING:-}" == "true" ]] &&
     [[ "${LOGGING_DESTINATION:-}" == "gcp" ]]; then
    if [[ "${ENABLE_METADATA_AGENT:-}" == "stackdriver" ]]; then
      metadata_agent_cpu_request="${METADATA_AGENT_CPU_REQUEST:-40m}"
      metadata_agent_memory_request="${METADATA_AGENT_MEMORY_REQUEST:-50Mi}"
      metadata_agent_cluster_level_cpu_request="${METADATA_AGENT_CLUSTER_LEVEL_CPU_REQUEST:-40m}"
      metadata_agent_cluster_level_memory_request="${METADATA_AGENT_CLUSTER_LEVEL_MEMORY_REQUEST:-50Mi}"
      setup-addon-manifests "addons" "metadata-agent/stackdriver"
      metadata_agent_yaml="${dst_dir}/metadata-agent/stackdriver/metadata-agent.yaml"
      sed -i -e "s@{{ metadata_agent_cpu_request }}@${metadata_agent_cpu_request}@g" "${metadata_agent_yaml}"
      sed -i -e "s@{{ metadata_agent_memory_request }}@${metadata_agent_memory_request}@g" "${metadata_agent_yaml}"
      sed -i -e "s@{{ metadata_agent_cluster_level_cpu_request }}@${metadata_agent_cluster_level_cpu_request}@g" "${metadata_agent_yaml}"
      sed -i -e "s@{{ metadata_agent_cluster_level_memory_request }}@${metadata_agent_cluster_level_memory_request}@g" "${metadata_agent_yaml}"
    fi
  fi
  if [[ "${ENABLE_METRICS_SERVER:-}" == "true" ]]; then
    setup-addon-manifests "addons" "metrics-server"
    base_metrics_server_cpu="40m"
    base_metrics_server_memory="40Mi"
    metrics_server_memory_per_node="4"
    metrics_server_min_cluster_size="16"
    if [[ "${ENABLE_SYSTEM_ADDON_RESOURCE_OPTIMIZATIONS:-}" == "true" ]]; then
      base_metrics_server_cpu="40m"
      base_metrics_server_memory="35Mi"
      metrics_server_memory_per_node="4"
      metrics_server_min_cluster_size="5"
    fi
    local -r metrics_server_yaml="${dst_dir}/metrics-server/metrics-server-deployment.yaml"
    sed -i -e "s@{{ base_metrics_server_cpu }}@${base_metrics_server_cpu}@g" "${metrics_server_yaml}"
    sed -i -e "s@{{ base_metrics_server_memory }}@${base_metrics_server_memory}@g" "${metrics_server_yaml}"
    sed -i -e "s@{{ metrics_server_memory_per_node }}@${metrics_server_memory_per_node}@g" "${metrics_server_yaml}"
    sed -i -e "s@{{ metrics_server_min_cluster_size }}@${metrics_server_min_cluster_size}@g" "${metrics_server_yaml}"
  fi
  # Setting up the konnectivity-agent daemonset
  if [[ "${RUN_KONNECTIVITY_PODS:-false}" == "true" ]]; then
    setup-addon-manifests "addons" "konnectivity-agent"
    setup-konnectivity-agent-manifest
  fi
  if [[ "${ENABLE_CLUSTER_DNS:-}" == "true" ]]; then
    # Create a new directory for the DNS addon and prepend a "0" on the name.
    # Prepending "0" to the directory ensures that add-on manager
    # creates the dns service first. This ensures no other add-on
    # can "steal" the designated DNS clusterIP.
    BASE_ADDON_DIR=${KUBE_HOME}/kube-manifests/kubernetes/gci-trusty
    BASE_DNS_DIR=${BASE_ADDON_DIR}/dns
    NEW_DNS_DIR=${BASE_ADDON_DIR}/0-dns
    mkdir "${NEW_DNS_DIR}" && mv "${BASE_DNS_DIR}"/* "${NEW_DNS_DIR}" && rm -r "${BASE_DNS_DIR}"
    if [[ "${CLUSTER_DNS_CORE_DNS:-}" == "true" ]]; then
      setup-coredns-manifest
    else
      setup-kube-dns-manifest
    fi
    if [[ "${ENABLE_NODELOCAL_DNS:-}" == "true" ]]; then
      setup-nodelocaldns-manifest
    fi
  fi
  if [[ "${ENABLE_NETD:-}" == "true" ]]; then
    setup-netd-manifest
  fi
  if [[ "${ENABLE_NODE_LOGGING:-}" == "true" ]] && \
     [[ "${LOGGING_DESTINATION:-}" == "elasticsearch" ]] && \
     [[ "${ENABLE_CLUSTER_LOGGING:-}" == "true" ]]; then
    echo "fluentd-elasticsearch addon is no longer included here. Terminate cluster initialization."
    echo "The addon can be installed from https://github.com/kubernetes-sigs/instrumentation-addons"
    exit 1
  fi
  if [[ "${ENABLE_NODE_LOGGING:-}" == "true" ]] && \
     [[ "${LOGGING_DESTINATION:-}" == "gcp" ]]; then
    setup-addon-manifests "addons" "fluentd-gcp"
    setup-fluentd ${dst_dir}
    local -r event_exporter_yaml="${dst_dir}/fluentd-gcp/event-exporter.yaml"
    update-event-exporter ${event_exporter_yaml}
    update-prometheus-to-sd-parameters ${event_exporter_yaml}
  fi
  if [[ "${ENABLE_NODE_PROBLEM_DETECTOR:-}" == "daemonset" ]]; then
    setup-addon-manifests "addons" "node-problem-detector"
  fi
  if [[ "${ENABLE_NODE_PROBLEM_DETECTOR:-}" == "standalone" ]]; then
    # Setup role binding(s) for standalone node problem detector.
    if [[ -n "${NODE_PROBLEM_DETECTOR_TOKEN:-}" ]]; then
      setup-addon-manifests "addons" "node-problem-detector/standalone"
    fi
    setup-addon-manifests "addons" "node-problem-detector/kubelet-user-standalone" "node-problem-detector"
  fi
  if echo "${ADMISSION_CONTROL:-}" | grep -q "LimitRanger"; then
    setup-addon-manifests "admission-controls" "limit-range" "gce"
  fi
  setup-addon-manifests "addons" "admission-resource-quota-critical-pods"
  if [[ "${NETWORK_POLICY_PROVIDER:-}" == "calico" ]]; then
    setup-addon-manifests "addons" "calico-policy-controller"

    setup-addon-custom-yaml "addons" "calico-policy-controller" "calico-node-daemonset.yaml" "${CUSTOM_CALICO_NODE_DAEMONSET_YAML:-}"
    setup-addon-custom-yaml "addons" "calico-policy-controller" "typha-deployment.yaml" "${CUSTOM_TYPHA_DEPLOYMENT_YAML:-}"

    # Configure Calico CNI directory.
    local -r ds_file="${dst_dir}/calico-policy-controller/calico-node-daemonset.yaml"
    sed -i -e "s@__CALICO_CNI_DIR__@/home/kubernetes/bin@g" "${ds_file}"
  fi
  if [[ "${NETWORK_POLICY_PROVIDER:-}" == "kube-network-policies" ]]; then
    setup-addon-manifests "addons" "kube-network-policies"
  fi
  if [[ "${ENABLE_DEFAULT_STORAGE_CLASS:-}" == "true" ]]; then
    setup-addon-manifests "addons" "storage-class/gce"
  fi
  if [[ "${ENABLE_VOLUME_SNAPSHOTS:-}" == "true" ]]; then
    setup-addon-manifests "addons" "volumesnapshots/crd"
    setup-addon-manifests "addons" "volumesnapshots/volume-snapshot-controller"
    start-volumesnapshot-crd-and-controller
  fi
  if [[ "${ENABLE_IP_MASQ_AGENT:-}" == "true" ]]; then
    setup-addon-manifests "addons" "ip-masq-agent"
  fi
  if [[ "${ENABLE_METADATA_CONCEALMENT:-}" == "true" ]]; then
    setup-addon-manifests "addons" "metadata-proxy/gce"
    local -r metadata_proxy_yaml="${dst_dir}/metadata-proxy/gce/metadata-proxy.yaml"
    update-daemon-set-prometheus-to-sd-parameters ${metadata_proxy_yaml}
  fi
  if [[ "${ENABLE_ISTIO:-}" == "true" ]]; then
    if [[ "${ISTIO_AUTH_TYPE:-}" == "MUTUAL_TLS" ]]; then
      setup-addon-manifests "addons" "istio/auth"
    else
      setup-addon-manifests "addons" "istio/noauth"
    fi
  fi
  if [[ -n "${EXTRA_ADDONS_URL:-}" ]]; then
    download-extra-addons
    setup-addon-manifests "addons" "gce-extras"
  fi


  # Place addon manager pod manifest.
  src_file="${src_dir}/kube-addon-manager.yaml"
  sed -i -e "s@{{kubectl_prune_whitelist_override}}@${KUBECTL_PRUNE_WHITELIST_OVERRIDE:-}@g" "${src_file}"
  sed -i -e "s@{{kubectl_extra_prune_whitelist}}@${ADDON_MANAGER_PRUNE_WHITELIST:-}@g" "${src_file}"
  sed -i -e "s@{{runAsUser}}@${KUBE_ADDON_MANAGER_RUNASUSER:-2002}@g" "${src_file}"
  sed -i -e "s@{{runAsGroup}}@${KUBE_ADDON_MANAGER_RUNASGROUP:-2002}@g" "${src_file}"
  cp "${src_file}" /etc/kubernetes/manifests
}

function setup-konnectivity-agent-manifest {
    local -r manifest="/etc/kubernetes/addons/konnectivity-agent/konnectivity-agent-ds.yaml"
    sed -i "s|__APISERVER_IP__|${KUBERNETES_MASTER_NAME}|g" "${manifest}"
    if [[ "${KONNECTIVITY_SERVICE_PROXY_PROTOCOL_MODE:-grpc}" == 'http-connect' ]]; then
      sed -i "s|__EXTRA_PARAMS__|\t\t\"--agent-cert=/etc/srv/kubernetes/pki/konnectivity-agent/client.crt\",\n\t\t\"--agent-key=/etc/srv/kubernetes/pki/konnectivity-agent/client.key\",|g" "${manifest}"
      sed -i "s|__EXTRA_VOL_MNTS__|            - name: pki\n              mountPath: /etc/srv/kubernetes/pki/konnectivity-agent|g" "${manifest}"
      sed -i "s|__EXTRA_VOLS__|        - name: pki\n          hostPath:\n            path: /etc/srv/kubernetes/pki/konnectivity-agent|g" "${manifest}"
    else
      sed -i "s|__EXTRA_PARAMS__||g" "${manifest}"
      sed -i "s|__EXTRA_VOL_MNTS__||g" "${manifest}"
      sed -i "s|__EXTRA_VOLS__||g" "${manifest}"
    fi
}

function setup-cloud-pvl-admission-manifest {
  local -r manifest="/etc/kubernetes/addons/cloud-pvl-admission/mutating-webhook-configuration.yaml"
  sed -i "s|__CLOUD_PVL_ADMISSION_CA_CERT__|${CLOUD_PVL_ADMISSION_CA_CERT}|g" "${manifest}"
}

# Setups manifests for ingress controller and gce-specific policies for service controller.
function start-lb-controller {
  setup-addon-manifests "addons" "loadbalancing"

  # Starts a l7 loadbalancing controller for ingress.
  if [[ "${ENABLE_L7_LOADBALANCING:-}" == "glbc" ]]; then
    echo "Start GCE L7 pod"
    prepare-log-file /var/log/glbc.log
    setup-addon-manifests "addons" "cluster-loadbalancing/glbc"
    setup-addon-manifests "addons" "rbac/cluster-loadbalancing/glbc"
    create-kubeconfig "l7-lb-controller" "${GCE_GLBC_TOKEN}"

    local -r src_manifest="${KUBE_HOME}/kube-manifests/kubernetes/gci-trusty/glbc.manifest"
    local -r dest_manifest="/etc/kubernetes/manifests/glbc.manifest"

    if [[ -n "${CUSTOM_INGRESS_YAML:-}" ]]; then
      echo "${CUSTOM_INGRESS_YAML}" > "${dest_manifest}"
    else
      cp "${src_manifest}" "${dest_manifest}"
    fi

    # Override the glbc image if GCE_GLBC_IMAGE is specified.
    if [[ -n "${GCE_GLBC_IMAGE:-}" ]]; then
      sed -i "s|image:.*|image: ${GCE_GLBC_IMAGE}|" "${dest_manifest}"
    fi
  fi
}

# Setup working directory for kubelet.
function setup-kubelet-dir {
    echo "Making /var/lib/kubelet executable for kubelet"
    mount -B /var/lib/kubelet /var/lib/kubelet/
    mount -B -o remount,exec,suid,dev /var/lib/kubelet

    # TODO(#60123): The kubelet should create the cert-dir directory if it doesn't exist
    mkdir -p /var/lib/kubelet/pki/

    # Mount /var/lib/kubelet/pki on a tmpfs so it doesn't persist across
    # reboots. This can help avoid some rare instances of corrupt cert files
    # (e.g. created but not written during a shutdown). Kubelet crash-loops
    # in these cases. Do this after above mount calls so it isn't overwritten.
    echo "Mounting /var/lib/kubelet/pki on tmpfs"
    mount -t tmpfs tmpfs /var/lib/kubelet/pki
}

# Override for GKE custom master setup scripts (no-op outside of GKE).
function gke-master-start {
  if [[ -e "${KUBE_HOME}/bin/gke-internal-configure-helper.sh" ]]; then
    echo "Running GKE internal configuration script"
    . "${KUBE_HOME}/bin/gke-internal-configure-helper.sh"
    gke-internal-master-start
 elif [[ -n "${KUBE_BEARER_TOKEN:-}" ]]; then
   echo "setting up local admin kubeconfig"
   create-kubeconfig "local-admin" "${KUBE_BEARER_TOKEN}"
   echo "export KUBECONFIG=/etc/srv/kubernetes/local-admin/kubeconfig" > /etc/profile.d/kubeconfig.sh
  fi
}

function reset-motd {
  # kubelet is installed both on the master and nodes, and the version is easy to parse (unlike kubectl)
  local -r version="$("${KUBE_HOME}"/bin/kubelet --version=true | cut -f2 -d " ")"
  # This logic grabs either a release tag (v1.2.1 or v1.2.1-alpha.1),
  # or the git hash that's in the build info.
  local gitref
  gitref="$(echo "${version}" | sed -r "s/(v[0-9]+\.[0-9]+\.[0-9]+)(-[a-z]+\.[0-9]+)?.*/\1\2/g")"
  local devel=""
  if [[ "${gitref}" != "${version}" ]]; then
    devel="
Note: This looks like a development version, which might not be present on GitHub.
If it isn't, the closest tag is at:
  https://github.com/kubernetes/kubernetes/tree/${gitref}
"
    gitref="${version//*+/}"
  fi
  cat > /etc/motd <<EOF

Welcome to Kubernetes ${version}!

You can find documentation for Kubernetes at:
  http://docs.kubernetes.io/

The source for this release can be found at:
  /home/kubernetes/kubernetes-src.tar.gz
Or you can download it at:
  https://storage.googleapis.com/gke-release/kubernetes/release/${version}/kubernetes-src.tar.gz

It is based on the Kubernetes source at:
  https://github.com/kubernetes/kubernetes/tree/${gitref/-gke.*/}
${devel}
For Kubernetes copyright and licensing information, see:
  /home/kubernetes/LICENSES

EOF
}

function override-kubectl {
    echo "overriding kubectl"
    echo "export PATH=${KUBE_HOME}/bin:\$PATH" > /etc/profile.d/kube_env.sh

    # source the file explicitly otherwise we have
    # issues on a ubuntu OS image finding the kubectl
    # shellcheck disable=SC1091
    source /etc/profile.d/kube_env.sh

    # Add ${KUBE_HOME}/bin into sudoer secure path.
    local sudo_path
    sudo_path=$(sudo env | grep "^PATH=")
    if [[ -n "${sudo_path}" ]]; then
      sudo_path=${sudo_path#PATH=}
      (
        umask 027
        echo "Defaults secure_path=\"${KUBE_HOME}/bin:${sudo_path}\"" > /etc/sudoers.d/kube_secure_path
      )
    fi
}

function detect-cgroup-config {
  CGROUP_CONFIG=$(stat -fc %T /sys/fs/cgroup/)
  echo "Detected cgroup config as ${CGROUP_CONFIG}"
}

function override-pv-recycler {
  if [[ -z "${PV_RECYCLER_OVERRIDE_TEMPLATE:-}" ]]; then
    echo "PV_RECYCLER_OVERRIDE_TEMPLATE is not set"
    exit 1
  fi

  PV_RECYCLER_VOLUME="{\"name\": \"pv-recycler-mount\",\"hostPath\": {\"path\": \"${PV_RECYCLER_OVERRIDE_TEMPLATE}\", \"type\": \"FileOrCreate\"}},"
  PV_RECYCLER_MOUNT="{\"name\": \"pv-recycler-mount\",\"mountPath\": \"${PV_RECYCLER_OVERRIDE_TEMPLATE}\", \"readOnly\": true},"

  cat > "${PV_RECYCLER_OVERRIDE_TEMPLATE}" <<\EOF
version: v1
kind: Pod
metadata:
  generateName: pv-recycler-
  namespace: default
spec:
  activeDeadlineSeconds: 60
  restartPolicy: Never
  volumes:
  - name: vol
  containers:
  - name: pv-recycler
    image: registry.k8s.io/build-image/debian-base:bookworm-v1.0.4
    command:
    - /bin/sh
    args:
    - -c
    - test -e /scrub && find /scrub -mindepth 1 -delete && test -z $(ls -A /scrub) || exit 1
    volumeMounts:
    - name: vol
      mountPath: /scrub
EOF

# fixup the alternate registry if specified
if [[ -n "${KUBE_ADDON_REGISTRY:-}" ]]; then
  sed -i -e "s@registry.k8s.io@${KUBE_ADDON_REGISTRY}@g" "${PV_RECYCLER_OVERRIDE_TEMPLATE}"
fi
}

function wait-till-apiserver-ready() {
  until kubectl get nodes; do
    sleep 5
  done
}

function ensure-master-bootstrap-kubectl-auth {
  # By default, `kubectl` uses http://localhost:8080
  # If the insecure port is disabled, kubectl will need to use an admin-authenticated kubeconfig.
  if [[ -n "${KUBE_BOOTSTRAP_TOKEN:-}" ]]; then
    create-kubeconfig "kube-bootstrap" "${KUBE_BOOTSTRAP_TOKEN}"
    export KUBECONFIG=/etc/srv/kubernetes/kube-bootstrap/kubeconfig
  fi
}

function setup-containerd {
  echo "Generate containerd config"
  local config_path="${CONTAINERD_CONFIG_PATH:-"/etc/containerd/config.toml"}"
  mkdir -p "$(dirname "${config_path}")"
  local cni_template_path="${KUBE_HOME}/cni.template"
  cat > "${cni_template_path}" <<EOF
{
  "name": "k8s-pod-network",
  "cniVersion": "0.3.1",
  "plugins": [
    {
      "type": "ptp",
      "mtu": 1460,
      "ipam": {
        "type": "host-local",
        "subnet": "{{.PodCIDR}}",
        "routes": [
          {
            "dst": "0.0.0.0/0"
          }
        ]
      }
    },
    {
      "type": "portmap",
      "capabilities": {
        "portMappings": true
      }
    }
  ]
}
EOF
  if [[ "${KUBERNETES_MASTER:-}" != "true" ]]; then
    if [[ "${NETWORK_POLICY_PROVIDER:-"none"}" == "calico" || "${ENABLE_NETD:-}" == "true" ]]; then
      # Use Kubernetes cni daemonset on node if network policy provider calico is specified
      # or netd is enabled.
      cni_template_path=""
    fi
  fi

   # Use systemd cgroup driver when running on cgroupv2
  local systemdCgroup="false"
  if [[ "${CGROUP_CONFIG-}" == "cgroup2fs" ]]; then
    systemdCgroup="true"
  fi

  cat > "${config_path}" <<EOF
version = 2
# Kubernetes requires the cri plugin.
required_plugins = ["io.containerd.grpc.v1.cri"]
# Kubernetes doesn't use containerd restart manager.
disabled_plugins = ["io.containerd.internal.v1.restart"]
oom_score = -999

[debug]
  level = "${CONTAINERD_LOG_LEVEL:-"info"}"

[plugins."io.containerd.grpc.v1.cri"]
  stream_server_address = "127.0.0.1"
  max_container_log_line_size = ${CONTAINERD_MAX_CONTAINER_LOG_LINE:-262144}
  sandbox_image = "${CONTAINERD_INFRA_CONTAINER:-"registry.k8s.io/pause:3.10"}"
[plugins."io.containerd.grpc.v1.cri".cni]
  bin_dir = "${KUBE_HOME}/bin"
  conf_dir = "/etc/cni/net.d"
  conf_template = "${cni_template_path}"
[plugins."io.containerd.grpc.v1.cri".containerd]
  default_runtime_name = "runc"
[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.runc]
  runtime_type = "io.containerd.runc.v2"
[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.runc.options]
  SystemdCgroup = ${systemdCgroup}
# enable hosts config
[plugins."io.containerd.grpc.v1.cri".registry]
  config_path = "/etc/containerd/certs.d"
EOF

  # used for 5k node scale tests with private pull-through cache
  if [[ -n "${KUBERNETES_REGISTRY_PULL_THROUGH_HOST:-}" ]]; then
    registry_config_dir="/etc/containerd/certs.d/registry.k8s.io"
    mkdir -p "${registry_config_dir}"
    {
      # NOTE: we need literal double quotes around some of these values
      echo 'server="'"${KUBERNETES_REGISTRY_PULL_THROUGH_HOST}"'"'
      echo ''
      echo '[host."'"${KUBERNETES_REGISTRY_PULL_THROUGH_HOST}"'"]'
      echo '  override_path = true'
      echo '  capabilities = ["pull", "resolve"]'
      # TODO: this is a hack. https://github.com/containerd/containerd/issues/7385
      echo '[host."'"${KUBERNETES_REGISTRY_PULL_THROUGH_HOST}"'".header]'
      if [[ -n "${KUBERNETES_REGISTRY_PULL_THROUGH_BASIC_AUTH_TOKEN:-}" ]]; then
        echo "  authorization = '""${KUBERNETES_REGISTRY_PULL_THROUGH_BASIC_AUTH_TOKEN}""'"
      fi
    } > "${registry_config_dir}/hosts.toml"
  fi

  if [[ "${CONTAINER_RUNTIME_TEST_HANDLER:-}" == "true" ]]; then
  cat >> "${config_path}" <<EOF
# Setup a runtime with the magic name ("test-handler") used for Kubernetes
# runtime class tests ...
[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.test-handler]
  runtime_type = "io.containerd.runc.v2"
EOF
  fi

  # Reuse docker group for containerd.
  local -r containerd_gid="$(grep ^docker: /etc/group | cut -d: -f 3)"
  if [[ -n "${containerd_gid:-}" ]]; then
    cat >> "${config_path}" <<EOF
# reuse id of the docker group
[grpc]
  gid = ${containerd_gid}
EOF
  fi
  chmod 644 "${config_path}"

  echo "Restart containerd to load the config change"
  systemctl restart containerd
}

# This function detects the platform/arch of the machine where the script runs,
# and sets the HOST_PLATFORM and HOST_ARCH environment variables accordingly.
# Callers can specify HOST_PLATFORM_OVERRIDE and HOST_ARCH_OVERRIDE to skip the detection.
# This function is adapted from the detect_client_info function in cluster/get-kube-binaries.sh
# and kube::util::host_os, kube::util::host_arch functions in hack/lib/util.sh
# This function should be synced with detect_host_info in ./configure.sh
function detect_host_info() {
  HOST_PLATFORM=${HOST_PLATFORM_OVERRIDE:-"$(uname -s)"}
  case "${HOST_PLATFORM}" in
    Linux|linux)
      HOST_PLATFORM="linux"
      ;;
    *)
      echo "Unknown, unsupported platform: ${HOST_PLATFORM}." >&2
      echo "Supported platform(s): linux." >&2
      echo "Bailing out." >&2
      exit 2
  esac

  HOST_ARCH=${HOST_ARCH_OVERRIDE:-"$(uname -m)"}
  case "${HOST_ARCH}" in
    x86_64*|i?86_64*|amd64*)
      HOST_ARCH="amd64"
      ;;
    aHOST_arch64*|aarch64*|arm64*)
      HOST_ARCH="arm64"
      ;;
    *)
      echo "Unknown, unsupported architecture (${HOST_ARCH})." >&2
      echo "Supported architecture(s): amd64 and arm64." >&2
      echo "Bailing out." >&2
      exit 2
      ;;
  esac
}

# Initializes variables used by the log-* functions.
#
# get-metadata-value must be defined before calling this function.
#
# NOTE: this function is duplicated in configure.sh, any changes here should be
# duplicated there as well.
function log-init {
  # Used by log-* functions.
  LOG_CLUSTER_ID=$(get-metadata-value 'instance/attributes/cluster-uid' 'get-metadata-value-error')
  LOG_INSTANCE_NAME=$(hostname)
  LOG_BOOT_ID=$(journalctl --list-boots | grep -E '^ *0' | awk '{print $2}')
  declare -Ag LOG_START_TIMES
  declare -ag LOG_TRAP_STACK

  LOG_STATUS_STARTED='STARTED'
  LOG_STATUS_COMPLETED='COMPLETED'
  LOG_STATUS_ERROR='ERROR'
}

# Sets an EXIT trap.
# Args:
#   $1:... : the trap command.
#
# NOTE: this function is duplicated in configure.sh, any changes here should be
# duplicated there as well.
function log-trap-push {
  local t="${*:1}"
  LOG_TRAP_STACK+=("${t}")
  # shellcheck disable=2064
  trap "${t}" EXIT
}

# Removes and restores an EXIT trap.
#
# NOTE: this function is duplicated in configure.sh, any changes here should be
# duplicated there as well.
function log-trap-pop {
  # Remove current trap.
  unset 'LOG_TRAP_STACK[-1]'

  # Restore previous trap.
  if [ ${#LOG_TRAP_STACK[@]} -ne 0 ]; then
    local t="${LOG_TRAP_STACK[-1]}"
    # shellcheck disable=2064
    trap "${t}" EXIT
  else
    # If no traps in stack, clear.
    trap EXIT
  fi
}

# Logs the end of a bootstrap step that errored.
# Args:
#  $1 : bootstrap step name.
#
# NOTE: this function is duplicated in configure.sh, any changes here should be
# duplicated there as well.
function log-error {
  local bootstep="$1"

  log-proto "${bootstep}" "${LOG_STATUS_ERROR}" "encountered non-zero exit code"
}

# Wraps a command with bootstrap logging.
# Args:
#   $1    : bootstrap step name.
#   $2... : the command to run.
#
# NOTE: this function is duplicated in configure.sh, any changes here should be
# duplicated there as well.
function log-wrap {
  local bootstep="$1"
  local command="${*:2}"

  log-trap-push "log-error ${bootstep}"
  log-proto "${bootstep}" "${LOG_STATUS_STARTED}"
  $command
  log-proto "${bootstep}" "${LOG_STATUS_COMPLETED}"
  log-trap-pop
}

# Logs a bootstrap step start. Prefer log-wrap.
# Args:
#   $1 : bootstrap step name.
#
# NOTE: this function is duplicated in configure.sh, any changes here should be
# duplicated there as well.
function log-start {
  local bootstep="$1"

  log-trap-push "log-error ${bootstep}"
  log-proto "${bootstep}" "${LOG_STATUS_STARTED}"
}

# Logs a bootstrap step end. Prefer log-wrap.
# Args:
#   $1 : bootstrap step name.
#
# NOTE: this function is duplicated in configure.sh, any changes here should be
# duplicated there as well.
function log-end {
  local bootstep="$1"

  log-proto "${bootstep}" "${LOG_STATUS_COMPLETED}"
  log-trap-pop
}

# Writes a log proto to stdout.
# Args:
#   $1: bootstrap step name.
#   $2: status. Either 'STARTED', 'COMPLETED', or 'ERROR'.
#   $3: optional status reason.
#
# NOTE: this function is duplicated in configure.sh, any changes here should be
# duplicated there as well.
function log-proto {
  local bootstep="$1"
  local status="$2"
  local status_reason="${3:-}"

  # Get current time.
  local current_time
  current_time="$(date --utc '+%s.%N')"
  # ...formatted as UTC RFC 3339.
  local timestamp
  timestamp="$(date --utc --date="@${current_time}" '+%FT%T.%NZ')"

  # Calculate latency.
  local latency='null'
  if [ "${status}" == "${LOG_STATUS_STARTED}" ]; then
    LOG_START_TIMES["${bootstep}"]="${current_time}"
  else
    local start_time="${LOG_START_TIMES["${bootstep}"]}"
    unset 'LOG_START_TIMES['"${bootstep}"']'

    # Bash cannot do non-integer math, shell out to awk.
    latency="$(echo "${current_time} ${start_time}" | awk '{print $1 - $2}')s"

    # The default latency is null which cannot be wrapped as a string so we must
    # do it here instead of the printf.
    latency="\"${latency}\""
  fi

  printf '[cloud.kubernetes.monitoring.proto.SerialportLog] {"cluster_hash":"%s","vm_instance_name":"%s","boot_id":"%s","timestamp":"%s","bootstrap_status":{"step_name":"%s","status":"%s","status_reason":"%s","latency":%s}}\n' \
  "${LOG_CLUSTER_ID}" "${LOG_INSTANCE_NAME}" "${LOG_BOOT_ID}" "${timestamp}" "${bootstep}" "${status}" "${status_reason}" "${latency}"
}

########### Main Function ###########
function main() {
  echo "Start to configure instance for kubernetes"
  log-wrap 'DetectHostInfo' detect_host_info

  readonly UUID_MNT_PREFIX="/mnt/disks/by-uuid/google-local-ssds"
  readonly UUID_BLOCK_PREFIX="/dev/disk/by-uuid/google-local-ssds"
  readonly COREDNS_AUTOSCALER="Deployment/coredns"
  readonly KUBEDNS_AUTOSCALER="Deployment/kube-dns"

  # Resource requests of master components.
  CLOUD_CONTROLLER_MANAGER_CPU_REQUEST="${KUBE_CONTROLLER_MANAGER_CPU_REQUEST:-50m}"
  KUBE_CONTROLLER_MANAGER_CPU_REQUEST="${KUBE_CONTROLLER_MANAGER_CPU_REQUEST:-200m}"
  KUBE_SCHEDULER_CPU_REQUEST="${KUBE_SCHEDULER_CPU_REQUEST:-75m}"

  KUBE_HOME="/home/kubernetes"
  KUBE_BIN=${KUBE_HOME}/bin
  CONTAINERIZED_MOUNTER_HOME="${KUBE_HOME}/containerized_mounter"
  PV_RECYCLER_OVERRIDE_TEMPLATE="${KUBE_HOME}/kube-manifests/kubernetes/pv-recycler-template.yaml"

  log-start 'SourceKubeEnv'
  if [[ ! -e "${KUBE_HOME}/kube-env" ]]; then
    echo "The ${KUBE_HOME}/kube-env file does not exist!! Terminate cluster initialization."
    exit 1
  fi
  source "${KUBE_HOME}/kube-env"
  log-end 'SourceKubeEnv'

  if [[ -f "${KUBE_HOME}/kubelet-config.yaml" ]]; then
    echo "Found Kubelet config file at ${KUBE_HOME}/kubelet-config.yaml"
    KUBELET_CONFIG_FILE_ARG="--config ${KUBE_HOME}/kubelet-config.yaml"
  fi

  if [[ -e "${KUBE_HOME}/kube-master-certs" ]]; then
    log-wrap 'SourceKubeMasterCerts' source "${KUBE_HOME}/kube-master-certs"
  fi

  log-start 'VerifyKubeUser'
  if [[ -n "${KUBE_USER:-}" ]]; then
    if ! [[ "${KUBE_USER}" =~ ^[-._@a-zA-Z0-9]+$ ]]; then
      echo "Bad KUBE_USER format."
      exit 1
    fi
  fi
  log-end 'VerifyKubeUser'

  log-start 'GenerateTokens'
  KUBE_CONTROLLER_MANAGER_TOKEN="$(secure_random 32)"
  CLOUD_CONTROLLER_MANAGER_TOKEN="$(secure_random 32)"
  KUBE_SCHEDULER_TOKEN="$(secure_random 32)"
  KUBE_CLUSTER_AUTOSCALER_TOKEN="$(secure_random 32)"
  if [[ "${ENABLE_L7_LOADBALANCING:-}" == "glbc" ]]; then
    GCE_GLBC_TOKEN="$(secure_random 32)"
  fi
  ADDON_MANAGER_TOKEN="$(secure_random 32)"
  if [[ "${ENABLE_APISERVER_INSECURE_PORT:-false}" != "true" ]]; then
    KUBE_BOOTSTRAP_TOKEN="$(secure_random 32)"
  fi
  if [[ "${PREPARE_KONNECTIVITY_SERVICE:-false}" == "true" ]]; then
    KONNECTIVITY_SERVER_TOKEN="$(secure_random 32)"
  fi
  if [[ "${ENABLE_MONITORING_TOKEN:-false}" == "true" ]]; then
    MONITORING_TOKEN="$(secure_random 32)"
  fi
  log-end 'GenerateTokens'

  log-wrap 'SetupOSParams' setup-os-params
  log-wrap 'ConfigIPFirewall' config-ip-firewall
  log-wrap 'CreateDirs' create-dirs
  log-wrap 'EnsureLocalSSDs' ensure-local-ssds
  log-wrap 'SetupKubeletDir' setup-kubelet-dir
  log-wrap 'SetupJournald' setup-journald
  log-wrap 'SetupLogrotate' setup-logrotate
  if [[ "${KUBERNETES_MASTER:-}" == "true" ]]; then
    log-wrap 'MountMasterPD' mount-master-pd
    log-wrap 'CreateNodePKI' create-node-pki
    log-wrap 'CreateMasterPKI' create-master-pki
    log-wrap 'CreateMasterAuth' create-master-auth
    log-wrap 'EnsureMasterBootstrapKubectlAuth' ensure-master-bootstrap-kubectl-auth
    log-wrap 'CreateMasterKubeletAuth' create-master-kubelet-auth
    log-wrap 'CreateMasterEtcdAuth' create-master-etcd-auth
    log-wrap 'CreateMasterEtcdApiserverAuth' create-master-etcd-apiserver-auth
    log-wrap 'OverridePVRecycler' override-pv-recycler
    log-wrap 'GKEMasterStart' gke-master-start
  else
    log-wrap 'CreateNodePKI' create-node-pki
    log-wrap 'CreateKubeletKubeconfig' create-kubelet-kubeconfig "${KUBERNETES_MASTER_NAME}"
    if [[ "${KUBE_PROXY_DAEMONSET:-}" != "true" ]] && [[ "${KUBE_PROXY_DISABLE:-}" != "true" ]]; then
      log-wrap 'CreateKubeproxyUserKubeconfig' create-kubeproxy-user-kubeconfig
    fi
    if [[ "${ENABLE_NODE_PROBLEM_DETECTOR:-}" == "standalone" ]]; then
      if [[ -n "${NODE_PROBLEM_DETECTOR_TOKEN:-}" ]]; then
        log-wrap 'CreateNodeProblemDetectorKubeconfig' create-node-problem-detector-kubeconfig "${KUBERNETES_MASTER_NAME}"
      elif [[ -f "/var/lib/kubelet/kubeconfig" ]]; then
        log-wrap 'CreateNodeProblemDetectorKubeconfigFromKubelet' create-node-problem-detector-kubeconfig-from-kubelet
      else
        echo "Either NODE_PROBLEM_DETECTOR_TOKEN or /var/lib/kubelet/kubeconfig must be set"
        exit 1
      fi
    fi
  fi

  log-wrap 'DetectCgroupConfig' detect-cgroup-config
  log-wrap 'OverrideKubectl' override-kubectl
  if docker-installed; then
    # We still need to configure docker so it wouldn't reserver the 172.17.0/16 subnet
    # And if somebody will start docker to build or pull something, logging will also be set up
    log-wrap 'AssembleDockerFlags' assemble-docker-flags
    # stop docker if it is present as we want to use just containerd
    log-wrap 'StopDocker' systemctl stop docker || echo "unable to stop docker"
  fi

  if [[ ! -e "/etc/profile.d/containerd_env.sh" ]]; then
    log-wrap 'SetupContainerd' setup-containerd
  else
    echo "Skipping SetupContainerd step because containerd has already been setup by containerd's configure.sh script"
  fi

  log-start 'SetupKubePodLogReadersGroupDir'
  if [[ -n "${KUBE_POD_LOG_READERS_GROUP:-}" ]]; then
     mkdir -p /var/log/pods/
     chgrp -R "${KUBE_POD_LOG_READERS_GROUP:-}" /var/log/pods/
     chmod -R g+s /var/log/pods/
  fi
  log-end 'SetupKubePodLogReadersGroupDir'

  # Note prepare-mounter-rootfs must be called before the kubelet starts, as
  # kubelet startup updates its nameserver.
  log-wrap 'PrepareMounterRootfs' prepare-mounter-rootfs

  log-wrap 'StartKubelet' start-kubelet

  if [[ "${KUBERNETES_MASTER:-}" == "true" ]]; then
    log-wrap 'ComputeMasterManifestVariables' compute-master-manifest-variables
    if [[ -z "${ETCD_SERVERS:-}" ]]; then
      log-wrap 'StartEtcdServers' start-etcd-servers
    fi
    log-wrap 'SourceConfigureKubeApiserver' source ${KUBE_BIN}/configure-kubeapiserver.sh
    log-wrap 'StartKubeApiserver' start-kube-apiserver
    if [[ "${RUN_KONNECTIVITY_PODS:-false}" == "true" ]]; then
      log-wrap 'StartKonnectivityServer' start-konnectivity-server
    fi
    log-wrap 'StartKubeControllerManager' start-kube-controller-manager
    if [[ "${CLOUD_PROVIDER_FLAG:-external}" == "external" ]]; then
      log-wrap 'StartCloudControllerManager' start-cloud-controller-manager
    fi
    log-wrap 'StartKubeScheduler' start-kube-scheduler
    log-wrap 'WaitTillApiserverReady' wait-till-apiserver-ready
    log-wrap 'StartKubeAddons' start-kube-addons
    log-wrap 'StartClusterAutoscaler' start-cluster-autoscaler
    log-wrap 'StartLBController' start-lb-controller
    log-wrap 'UpdateLegacyAddonNodeLabels' update-legacy-addon-node-labels &
  else
    if [[ "${KUBE_PROXY_DAEMONSET:-}" != "true" ]] && [[ "${KUBE_PROXY_DISABLE:-}" != "true" ]]; then
      log-wrap 'StartKubeProxy' start-kube-proxy
    fi
    if [[ "${ENABLE_NODE_PROBLEM_DETECTOR:-}" == "standalone" ]]; then
      log-wrap 'StartNodeProblemDetector' start-node-problem-detector
    fi
  fi
  log-wrap 'ResetMotd' reset-motd

  # Wait for all background jobs to finish.
  wait
  echo "Done for the configuration for kubernetes"
}

if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
  log-init
  log-wrap 'ConfigureHelperMain' main "${@}"

  if [[ "${KUBERNETES_MASTER:-}" == "true" ]]; then
    # Give kube-bootstrap-logs-forwarder.service some time to write all logs.
    sleep 3
  fi
fi
