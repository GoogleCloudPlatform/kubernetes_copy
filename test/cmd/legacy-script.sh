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

# This contains util code for testing kubectl.

set -o errexit
set -o nounset
set -o pipefail

# Set locale to ensure english responses from kubectl commands
export LANG=C

KUBE_ROOT=$(dirname "${BASH_SOURCE[0]}")/../..
# Expects the following has already been done by whatever sources this script
# source "${KUBE_ROOT}/hack/lib/init.sh"
# source "${KUBE_ROOT}/hack/lib/test.sh"
source "${KUBE_ROOT}/test/cmd/apply.sh"
source "${KUBE_ROOT}/test/cmd/apps.sh"
source "${KUBE_ROOT}/test/cmd/auth_whoami.sh"
source "${KUBE_ROOT}/test/cmd/authentication.sh"
source "${KUBE_ROOT}/test/cmd/authorization.sh"
source "${KUBE_ROOT}/test/cmd/batch.sh"
source "${KUBE_ROOT}/test/cmd/certificate.sh"
source "${KUBE_ROOT}/test/cmd/convert.sh"
source "${KUBE_ROOT}/test/cmd/core.sh"
source "${KUBE_ROOT}/test/cmd/crd.sh"
source "${KUBE_ROOT}/test/cmd/create.sh"
source "${KUBE_ROOT}/test/cmd/debug.sh"
source "${KUBE_ROOT}/test/cmd/delete.sh"
source "${KUBE_ROOT}/test/cmd/diff.sh"
source "${KUBE_ROOT}/test/cmd/discovery.sh"
source "${KUBE_ROOT}/test/cmd/events.sh"
source "${KUBE_ROOT}/test/cmd/exec.sh"
source "${KUBE_ROOT}/test/cmd/generic-resources.sh"
source "${KUBE_ROOT}/test/cmd/get.sh"
source "${KUBE_ROOT}/test/cmd/help.sh"
source "${KUBE_ROOT}/test/cmd/kubeconfig.sh"
source "${KUBE_ROOT}/test/cmd/kuberc.sh"
source "${KUBE_ROOT}/test/cmd/node-management.sh"
source "${KUBE_ROOT}/test/cmd/plugins.sh"
source "${KUBE_ROOT}/test/cmd/proxy.sh"
source "${KUBE_ROOT}/test/cmd/rbac.sh"
source "${KUBE_ROOT}/test/cmd/request-timeout.sh"
source "${KUBE_ROOT}/test/cmd/results.sh"
source "${KUBE_ROOT}/test/cmd/run.sh"
source "${KUBE_ROOT}/test/cmd/save-config.sh"
source "${KUBE_ROOT}/test/cmd/storage.sh"
source "${KUBE_ROOT}/test/cmd/template-output.sh"
source "${KUBE_ROOT}/test/cmd/version.sh"
source "${KUBE_ROOT}/test/cmd/wait.sh"


ETCD_HOST=${ETCD_HOST:-127.0.0.1}
ETCD_PORT=${ETCD_PORT:-2379}
SECURE_API_PORT=${SECURE_API_PORT:-6443}
API_HOST=${API_HOST:-127.0.0.1}
KUBELET_HEALTHZ_PORT=${KUBELET_HEALTHZ_PORT:-10248}
SECURE_CTLRMGR_PORT=${SECURE_CTLRMGR_PORT:-10257}
PROXY_HOST=127.0.0.1 # kubectl only serves on localhost.

IMAGE_NGINX="registry.k8s.io/nginx:1.7.9"
export IMAGE_DEPLOYMENT_R1="registry.k8s.io/nginx:test-cmd"  # deployment-revision1.yaml
export IMAGE_DEPLOYMENT_R2="$IMAGE_NGINX"  # deployment-revision2.yaml
export IMAGE_PERL="registry.k8s.io/perl"
export IMAGE_PAUSE_V2="registry.k8s.io/pause:2.0"
export IMAGE_DAEMONSET_R2="registry.k8s.io/pause:latest"
export IMAGE_DAEMONSET_R2_2="registry.k8s.io/nginx:test-cmd"  # rollingupdate-daemonset-rv2.yaml
export IMAGE_STATEFULSET_R1="registry.k8s.io/nginx-slim:0.7"
export IMAGE_STATEFULSET_R2="registry.k8s.io/nginx-slim:0.8"

# Expose kubectl directly for readability
PATH="${THIS_PLATFORM_BIN}":$PATH

# Define variables for resource types to prevent typos.
clusterroles="clusterroles"
configmaps="configmaps"
csr="csr"
cronjob="cronjobs"
deployments="deployments"
namespaces="namespaces"
nodes="nodes"
persistentvolumeclaims="persistentvolumeclaims"
persistentvolumes="persistentvolumes"
pods="pods"
podtemplates="podtemplates"
replicasets="replicasets"
replicationcontrollers="replicationcontrollers"
roles="roles"
secrets="secrets"
selfsubjectreviews="selfsubjectreviews"
serviceaccounts="serviceaccounts"
services="services"
statefulsets="statefulsets"
storageclass="storageclass"
subjectaccessreviews="subjectaccessreviews"
customresourcedefinitions="customresourcedefinitions"
daemonsets="daemonsets"
controllerrevisions="controllerrevisions"
job="jobs"

# A junit-style XML test report will be generated in the directory specified by KUBE_JUNIT_REPORT_DIR, if set.
# If KUBE_JUNIT_REPORT_DIR is unset, and ARTIFACTS is set, then use what is set in ARTIFACTS.
if [[ -z "${KUBE_JUNIT_REPORT_DIR:-}" && -n "${ARTIFACTS:-}" ]]; then
  export KUBE_JUNIT_REPORT_DIR="${ARTIFACTS}"
fi

# include shell2junit library
sh2ju="${KUBE_ROOT}/third_party/forked/shell2junit/sh2ju.sh"
if [[ -f "${sh2ju}" ]]; then
  source "${sh2ju}"
else
  echo "failed to find third_party/forked/shell2junit/sh2ju.sh"
  exit 1
fi

# record_command runs the command and records its output/error messages in junit format
# it expects the first to be the name of the command
# Example:
# record_command run_kubectl_tests
#
# WARNING: Variable changes in the command will NOT be effective after record_command returns.
#          This is because the command runs in subshell.
function record_command() {
    set +o nounset
    set +o errexit

    local name="$1"
    local output="${KUBE_JUNIT_REPORT_DIR:-/tmp/junit-results}"
    echo "Recording: ${name}"
    echo "Running command: $*"
    juLog -output="${output}" -class="test-cmd" -name="${name}" "$@"
    local exitCode=$?
    if [[ ${exitCode} -ne 0 ]]; then
      # Record failures for any non-canary commands
      if [ "${name}" != "record_command_canary" ]; then
        echo "Error when running ${name}"
        foundError="${foundError}""${name}"", "
      fi
    elif [ "${name}" == "record_command_canary" ]; then
      # If the canary command passed, fail
      echo "record_command_canary succeeded unexpectedly"
      foundError="${foundError}""${name}"", "
    fi

    set -o nounset
    set -o errexit
}

# Ensure our record_command stack correctly propagates and detects errexit failures in invoked commands - see https://issue.k8s.io/84871
foundError=""
function record_command_canary()
{
  set -o nounset
  set -o errexit
  bogus-expected-to-fail
  set +o nounset
  set +o errexit
}
KUBE_JUNIT_REPORT_DIR=$(mktemp -d /tmp/record_command_canary.XXXXX) record_command record_command_canary
if [[ -n "${foundError}" ]]; then
  echo "FAILED TESTS: record_command_canary"
  exit 1
fi

# Stops the running kubectl proxy, if there is one.
function stop-proxy()
{
  [[ -n "${PROXY_PORT-}" ]] && kube::log::status "Stopping proxy on port ${PROXY_PORT}"
  [[ -n "${PROXY_PID-}" ]] && kill -9 "${PROXY_PID}" 1>&2 2>/dev/null
  [[ -n "${PROXY_PORT_FILE-}" ]] && rm -f "${PROXY_PORT_FILE}"
  PROXY_PID=
  PROXY_PORT=
  PROXY_PORT_FILE=
}

# Starts "kubect proxy" to test the client proxy. $1: api_prefix
function start-proxy()
{
  stop-proxy

  PROXY_PORT_FILE=$(mktemp proxy-port.out.XXXXX)
  kube::log::status "Starting kubectl proxy on random port; output file in ${PROXY_PORT_FILE}; args: ${1-}"


  if [ $# -eq 0 ]; then
    kubectl proxy --port=0 --www=. 1>"${PROXY_PORT_FILE}" 2>&1 &
  else
    kubectl proxy --port=0 --www=. --api-prefix="$1" 1>"${PROXY_PORT_FILE}" 2>&1 &
  fi
  PROXY_PID=$!
  PROXY_PORT=

  local attempts=0
  while [[ -z ${PROXY_PORT} ]]; do
    if (( attempts > 9 )); then
      kill "${PROXY_PID}"
      kube::log::error_exit "Couldn't start proxy. Failed to read port after ${attempts} tries. Got: $(cat "${PROXY_PORT_FILE}")"
    fi
    sleep .5
    kube::log::status "Attempt ${attempts} to read ${PROXY_PORT_FILE}..."
    PROXY_PORT=$(sed 's/.*Starting to serve on 127.0.0.1:\([0-9]*\)$/\1/'< "${PROXY_PORT_FILE}")
    attempts=$((attempts+1))
  done

  kube::log::status "kubectl proxy running on port ${PROXY_PORT}"

  # We try checking kubectl proxy 30 times with 1s delays to avoid occasional
  # failures.
  if [ $# -eq 0 ]; then
    kube::util::wait_for_url "http://127.0.0.1:${PROXY_PORT}/healthz" "kubectl proxy"
  else
    kube::util::wait_for_url "http://127.0.0.1:${PROXY_PORT}/$1/healthz" "kubectl proxy --api-prefix=$1"
  fi
}

function cleanup()
{
  stop-proxy
  [[ -n "${CTLRMGR_PID-}" ]] && kill -9 "${CTLRMGR_PID}" 1>&2 2>/dev/null
  [[ -n "${KUBELET_PID-}" ]] && kill -9 "${KUBELET_PID}" 1>&2 2>/dev/null
  [[ -n "${APISERVER_PID-}" ]] && kill -9 "${APISERVER_PID}" 1>&2 2>/dev/null

  kube::etcd::cleanup
  rm -rf "${KUBE_TEMP}"

  local junit_dir="${KUBE_JUNIT_REPORT_DIR:-/tmp/junit-results}"
  echo "junit report dir:" "${junit_dir}"

  kube::log::status "Clean up complete"
}

# Executes curl against the proxy. $1 is the path to use, $2 is the desired
# return code. Prints a helpful message on failure.
function check-curl-proxy-code()
{
  local status
  local -r address=$1
  local -r desired=$2
  local -r full_address="${PROXY_HOST}:${PROXY_PORT}${address}"
  status=$(curl -w "%{http_code}" --silent --output /dev/null "${full_address}")
  if [ "${status}" == "${desired}" ]; then
    return 0
  fi
  echo "For address ${full_address}, got ${status} but wanted ${desired}"
  return 1
}

# TODO: Remove this function when we do the retry inside the kubectl commands. See #15333.
function kubectl-with-retry()
{
  ERROR_FILE="${KUBE_TEMP}/kubectl-error"
  preserve_err_file=${PRESERVE_ERR_FILE:-false}
  for count in {0..3}; do
    kubectl "$@" 2> "${ERROR_FILE}" || true
    if grep -q "the object has been modified" "${ERROR_FILE}"; then
      kube::log::status "retry $1, error: $(cat "${ERROR_FILE}")"
      rm "${ERROR_FILE}"
      sleep $((2**count))
    else
      if [ "$preserve_err_file" != true ] ; then
        rm "${ERROR_FILE}"
      fi
      break
    fi
  done
}

# Waits for the pods with the given label to match the list of names. Don't call
# this function unless you know the exact pod names, or expect no pods.
# $1: label to match
# $2: list of pod names sorted by name
# Example invocation:
# wait-for-pods-with-label "app=foo" "nginx-0nginx-1"
function wait-for-pods-with-label()
{
  local i
  for i in $(seq 1 10); do
    kubeout=$(kubectl get po -l "$1" --output=go-template --template='{{range.items}}{{.metadata.name}}{{end}}' --sort-by metadata.name "${kube_flags[@]}")
    if [[ $kubeout = "$2" ]]; then
        return
    fi
    echo Waiting for pods: "$2", found "$kubeout"
    sleep "$i"
  done
  kube::log::error_exit "Timeout waiting for pods with label $1"
}

# Code to be run before running the tests.
setup() {
  kube::util::trap_add cleanup EXIT SIGINT
  kube::util::ensure-temp-dir
  # ensure ~/.kube/config isn't loaded by tests
  HOME="${KUBE_TEMP}"

  kube::etcd::start

  # Find a standard sed instance for use with edit scripts
  kube::util::ensure-gnu-sed

  kube::log::status "Building kubectl"
  make -C "${KUBE_ROOT}" WHAT="cmd/kubectl cmd/kubectl-convert"

  # Check kubectl
  kube::log::status "Running kubectl with no options"
  "${THIS_PLATFORM_BIN}/kubectl"

  # TODO: we need to note down the current default namespace and set back to this
  # namespace after the tests are done.
  CONTEXT="test"
  kubectl config set-credentials test-admin --token admin-token
  kubectl config set-cluster local --insecure-skip-tls-verify --server "https://127.0.0.1:${SECURE_API_PORT}"
  kubectl config set-context "${CONTEXT}" --user test-admin --cluster local
  kubectl config use-context "${CONTEXT}"
  kubectl config view

  kube::log::status "Setup complete"
}

# Generate a random namespace name, based on the current time (to make
# debugging slightly easier) and a random number. Don't use `date +%N`
# because that doesn't work on OSX.
create_and_use_new_namespace() {
  local ns_name
  ns_name="namespace-$(date +%s)-${RANDOM}"
  kube::log::status "Creating namespace ${ns_name}"
  kubectl create namespace "${ns_name}"
  kubectl config set-context "${CONTEXT}" --namespace="${ns_name}"
}

# Runs all kubectl tests.
# Requires an env var SUPPORTED_RESOURCES which is a comma separated list of
# resources for which tests should be run.
runTests() {
  foundError=""

  if [ -z "${SUPPORTED_RESOURCES:-}" ]; then
    echo "Need to set SUPPORTED_RESOURCES env var. It is a list of resources that are supported and hence should be tested. Set it to (*) to test all resources"
    exit 1
  fi
  kube::log::status "Checking kubectl version"
  kubectl version

  kube_flags=( '-s' "https://127.0.0.1:${SECURE_API_PORT}" '--insecure-skip-tls-verify' )

  kube_flags_without_token=( "${kube_flags[@]}" )

  # token defined in hack/testdata/auth-tokens.csv
  kube_flags_with_token=( "${kube_flags_without_token[@]}" '--token=admin-token' )

  if [[ -z "${ALLOW_SKEW:-}" ]]; then
    kube_flags+=('--match-server-version')
    kube_flags_with_token+=('--match-server-version')
  fi
  if kube::test::if_supports_resource "${nodes}" ; then
    [ "$(kubectl get nodes -o go-template='{{ .apiVersion }}' "${kube_flags[@]}")" == "v1" ]
  fi

  # Define helper variables for fields to prevent typos.
  # They will be used in some other files under test/cmd,
  # Let's export them as https://github.com/koalaman/shellcheck/wiki/SC2034 suggested.
  export id_field=".metadata.name"
  export labels_field=".metadata.labels"
  export annotations_field=".metadata.annotations"
  export service_selector_field=".spec.selector"
  export rc_replicas_field=".spec.replicas"
  export rc_status_replicas_field=".status.replicas"
  export rc_container_image_field=".spec.template.spec.containers"
  export rs_replicas_field=".spec.replicas"
  export port_field="(index .spec.ports 0).port"
  export port_name="(index .spec.ports 0).name"
  export second_port_field="(index .spec.ports 1).port"
  export second_port_name="(index .spec.ports 1).name"
  export image_field="(index .spec.containers 0).image"
  export pod_container_name_field="(index .spec.containers 0).name"
  export container_name_field="(index .spec.template.spec.containers 0).name"
  export hpa_min_field=".spec.minReplicas"
  export hpa_max_field=".spec.maxReplicas"
  export hpa_cpu_field="(index .spec.metrics 0).resource.target.averageUtilization"
  export template_labels=".spec.template.metadata.labels.name"
  export statefulset_replicas_field=".spec.replicas"
  export statefulset_observed_generation=".status.observedGeneration"
  export job_parallelism_field=".spec.parallelism"
  export deployment_replicas=".spec.replicas"
  export secret_data=".data"
  export secret_type=".type"
  export change_cause_annotation='.*kubernetes.io/change-cause.*'
  export pdb_min_available=".spec.minAvailable"
  export pdb_max_unavailable=".spec.maxUnavailable"
  export generation_field=".metadata.generation"
  export container_len="(len .spec.template.spec.containers)"
  export image_field0="(index .spec.template.spec.containers 0).image"
  export image_field1="(index .spec.template.spec.containers 1).image"

  # Make sure "default" namespace exists.
  if kube::test::if_supports_resource "${namespaces}" ; then
    output_message=$(kubectl get "${kube_flags[@]}" namespaces)
    if ! grep -q "default" <<< "${output_message}"; then
      # Create default namespace
      kubectl create "${kube_flags[@]}" ns default
    fi
  fi

  # Make sure "kubernetes" service exists.
  if kube::test::if_supports_resource "${services}" ; then
    # Attempt to create the kubernetes service, tolerating failure (since it might already exist)
    kubectl create "${kube_flags[@]}" -f hack/testdata/kubernetes-service.yaml || true
    # Require the service to exist (either we created it or the API server did)
    kubectl get "${kube_flags[@]}" -f hack/testdata/kubernetes-service.yaml
  fi

  cleanup_tests(){
    kube::test::clear_all
    if [[ -n "${foundError}" ]]; then
      echo "FAILED TESTS: ""${foundError}"
      exit 1
    fi
  }

  if [[ -n "${WHAT-}" ]]; then
    for pkg in ${WHAT}
    do
      # running of kubeadm is captured in hack/make-targets/test-cmd.sh
      if [[ "${pkg}" != "kubeadm" ]]; then
        record_command "run_${pkg}_tests"
      fi
    done
    cleanup_tests
    return
  fi

  #########################
  # Kubectl version #
  #########################

  record_command run_kubectl_version_tests

  ############################
  # Kubectl result reporting #
  ############################

  record_command run_kubectl_results_tests

  #######################
  # kubectl config set #
  #######################

  record_command run_kubectl_config_set_tests

  ##############################
  # kubectl config set-cluster #
  ##############################

  record_command run_kubectl_config_set_cluster_tests

  ##################################
  # kubectl config set-credentials #
  ##################################

  record_command run_kubectl_config_set_credentials_tests

  #######################
  # kubectl local proxy #
  #######################

  record_command run_kubectl_local_proxy_tests

  #########################
  # RESTMapper evaluation #
  #########################

  record_command run_RESTMapper_evaluation_tests

  # find all resources
  kubectl "${kube_flags[@]}" api-resources
  # find all namespaced resources that support list by name and get them
  kubectl "${kube_flags[@]}" api-resources --verbs=list --namespaced -o name | xargs -n 1 kubectl "${kube_flags[@]}" get -o name

  ################
  # Cluster Role #
  ################

  if kube::test::if_supports_resource "${clusterroles}" ; then
    record_command run_clusterroles_tests
  fi

  ########
  # Role #
  ########
  if kube::test::if_supports_resource "${roles}" ; then
      record_command run_role_tests
  fi

  #########################
  # Assert short name     #
  #########################

  if kube::test::if_supports_resource "${customresourcedefinitions}" && kube::test::if_supports_resource "${pods}" && kube::test::if_supports_resource "${configmaps}" ; then
    record_command run_assert_short_name_tests
  fi

  #########################
  # Assert singular name  #
  #########################

  if kube::test::if_supports_resource "${customresourcedefinitions}" && kube::test::if_supports_resource "${pods}" ; then
    record_command run_assert_singular_name_tests
  fi

  #########################
  # Ambiguous short name  #
  #########################

  if kube::test::if_supports_resource "${customresourcedefinitions}" ; then
    record_command run_ambiguous_shortname_tests
  fi

  ################
  # Explain crd  #
  ################

  if kube::test::if_supports_resource "${customresourcedefinitions}" ; then
    record_command run_explain_crd_with_additional_properties_tests
  fi

  #########################
  # Assert categories     #
  #########################

  ## test if a category is exported during discovery
  if kube::test::if_supports_resource "${pods}" ; then
    record_command run_assert_categories_tests
  fi

  ###########################
  # POD creation / deletion #
  ###########################

  if kube::test::if_supports_resource "${pods}" ; then
    record_command run_pod_tests
  fi

  if kube::test::if_supports_resource "${pods}" ; then
    record_command run_save_config_tests
  fi

  if kube::test::if_supports_resource "${pods}" ; then
    record_command run_kubectl_create_error_tests
  fi

  if kube::test::if_supports_resource "${pods}" ; then
    record_command run_kubectl_apply_tests
    record_command run_kubectl_server_side_apply_tests
    record_command run_kubectl_run_tests
    record_command run_kubectl_create_filter_tests
  fi

  if kube::test::if_supports_resource "${deployments}" ; then
    record_command run_kubectl_apply_deployments_tests
  fi

  ################
  # Kubectl diff #
  ################
  record_command run_kubectl_diff_tests
  record_command run_kubectl_diff_same_names

  ###############
  # Kubectl get #
  ###############

  if kube::test::if_supports_resource "${pods}" ; then
    record_command run_kubectl_get_tests
  fi

  ################
  # Kubectl help #
  ################

  record_command run_kubectl_help_tests

  ##################
  # Kubectl events #
  ##################

  if kube::test::if_supports_resource "${cronjob}" ; then
    record_command run_kubectl_events_tests
  fi

  ################
  # Kubectl exec #
  ################

  if kube::test::if_supports_resource "${pods}"; then
    record_command run_kubectl_exec_pod_tests
    if kube::test::if_supports_resource "${replicasets}" && kube::test::if_supports_resource "${configmaps}"; then
      record_command run_kubectl_exec_resource_name_tests
    fi
  fi

  ######################
  # Create             #
  ######################
  if kube::test::if_supports_resource "${secrets}" ; then
    record_command run_create_secret_tests
  fi
  if kube::test::if_supports_resource "${deployments}"; then
    record_command run_kubectl_create_kustomization_directory_tests
    record_command run_kubectl_create_validate_tests
  fi

  ######################
  # Convert            #
  ######################
  if kube::test::if_supports_resource "${deployments}"; then
    record_command run_convert_tests
  fi

  ######################
  # Delete             #
  ######################
  if kube::test::if_supports_resource "${configmaps}" ; then
    record_command run_kubectl_delete_allnamespaces_tests
  fi

  ######################
  # Delete --interactive   #
  ######################
  if kube::test::if_supports_resource "${configmaps}" ; then
    record_command run_kubectl_delete_interactive_tests
  fi

  ##################
  # Global timeout #
  ##################

  if kube::test::if_supports_resource "${pods}" ; then
    record_command run_kubectl_request_timeout_tests
  fi

  #####################################
  # CustomResourceDefinitions         #
  #####################################

  # customresourcedefinitions cleanup after themselves.
  if kube::test::if_supports_resource "${customresourcedefinitions}" ; then
    record_command run_crd_tests
  fi

  #####################################
  # Recursive Resources via directory #
  #####################################

  if kube::test::if_supports_resource "${pods}" ; then
    record_command run_recursive_resources_tests
  fi


  ##############
  # Namespaces #
  ##############
  if kube::test::if_supports_resource "${namespaces}" ; then
    record_command run_namespace_tests
  fi


  ###########
  # Secrets #
  ###########
  if kube::test::if_supports_resource "${namespaces}" ; then
    if kube::test::if_supports_resource "${secrets}" ; then
      record_command run_secrets_test
    fi
  fi


  ######################
  # ConfigMap          #
  ######################

  if kube::test::if_supports_resource "${namespaces}"; then
    if kube::test::if_supports_resource "${configmaps}" ; then
      record_command run_configmap_tests
    fi
  fi

  ####################
  # Client Config    #
  ####################

  record_command run_client_config_tests

  ####################
  # Service Accounts #
  ####################

  if kube::test::if_supports_resource "${namespaces}" && kube::test::if_supports_resource "${serviceaccounts}" ; then
    record_command run_service_accounts_tests
  fi

  ####################
  # Job              #
  ####################

  if kube::test::if_supports_resource "${job}" ; then
    record_command run_job_tests
    record_command run_create_job_tests
  fi

  #################
  # Pod templates #
  #################

  if kube::test::if_supports_resource "${podtemplates}" ; then
    record_command run_pod_templates_tests
  fi

  ############
  # Services #
  ############

  if kube::test::if_supports_resource "${services}" ; then
    record_command run_service_tests
  fi

  ##################
  # DaemonSets     #
  ##################

  if kube::test::if_supports_resource "${daemonsets}" ; then
    record_command run_daemonset_tests
    if kube::test::if_supports_resource "${controllerrevisions}"; then
      record_command run_daemonset_history_tests
    fi
  fi

  ###########################
  # Replication controllers #
  ###########################

  if kube::test::if_supports_resource "${namespaces}" ; then
    if kube::test::if_supports_resource "${replicationcontrollers}" ; then
      record_command run_rc_tests
    fi
  fi

  ######################
  # Deployments       #
  ######################

  if kube::test::if_supports_resource "${deployments}" ; then
    record_command run_deployment_tests
  fi

  ######################
  # Replica Sets       #
  ######################

  if kube::test::if_supports_resource "${replicasets}" ; then
    record_command run_rs_tests
  fi

  #################
  # Stateful Sets #
  #################

  if kube::test::if_supports_resource "${statefulsets}" ; then
    record_command run_stateful_set_tests
    if kube::test::if_supports_resource "${controllerrevisions}"; then
      record_command run_statefulset_history_tests
    fi
  fi

  ######################
  # Lists              #
  ######################

  if kube::test::if_supports_resource "${services}" ; then
    if kube::test::if_supports_resource "${deployments}" ; then
      record_command run_lists_tests
    fi
  fi


  ######################
  # Multiple Resources #
  ######################
  if kube::test::if_supports_resource "${services}" ; then
    if kube::test::if_supports_resource "${replicationcontrollers}" ; then
      record_command run_multi_resources_tests
    fi
  fi

  ######################
  # Persistent Volumes #
  ######################

  if kube::test::if_supports_resource "${persistentvolumes}" ; then
    record_command run_persistent_volumes_tests
  fi

  ############################
  # Persistent Volume Claims #
  ############################

  if kube::test::if_supports_resource "${persistentvolumeclaims}" ; then
    record_command run_persistent_volume_claims_tests
  fi

  ############################
  # Storage Classes #
  ############################

  if kube::test::if_supports_resource "${storageclass}" ; then
    record_command run_storage_class_tests
  fi

  #########
  # Nodes #
  #########

  if kube::test::if_supports_resource "${nodes}" ; then
    record_command run_nodes_tests
  fi

  ########################
  # Authentication
  ########################

  record_command run_exec_credentials_tests
  record_command run_exec_credentials_interactive_tests

  if kube::test::if_supports_resource "${selfsubjectreviews}" ; then
    record_command run_kubectl_auth_whoami_tests
  fi

  ########################
  # authorization.k8s.io #
  ########################

  if kube::test::if_supports_resource "${subjectaccessreviews}" ; then
    record_command run_authorization_tests
  fi

  # kubectl auth can-i
  # kube-apiserver is started with authorization mode AlwaysAllow, so kubectl can-i always returns yes
  if kube::test::if_supports_resource "${subjectaccessreviews}" ; then
    output_message=$(kubectl auth can-i '*' '*' 2>&1 "${kube_flags[@]}")
    kube::test::if_has_string "${output_message}" "yes"

    output_message=$(kubectl auth can-i get pods --subresource=log 2>&1 "${kube_flags[@]}")
    kube::test::if_has_string "${output_message}" "yes"

    output_message=$(kubectl auth can-i get invalid_resource 2>&1 "${kube_flags[@]}")
    kube::test::if_has_string "${output_message}" "the server doesn't have a resource type"

    output_message=$(kubectl auth can-i get /logs/ 2>&1 "${kube_flags[@]}")
    kube::test::if_has_string "${output_message}" "yes"

    output_message=$(! kubectl auth can-i get /logs/ --subresource=log 2>&1 "${kube_flags[@]}")
    kube::test::if_has_string "${output_message}" "subresource can not be used with NonResourceURL"

    output_message=$(kubectl auth can-i list jobs.batch/bar -n foo --quiet 2>&1 "${kube_flags[@]}")
    kube::test::if_empty_string "${output_message}"

    output_message=$(kubectl auth can-i get pods --subresource=log 2>&1 "${kube_flags[@]}"; echo $?)
    kube::test::if_has_string "${output_message}" '0'

    output_message=$(kubectl auth can-i get pods --subresource=log --quiet 2>&1 "${kube_flags[@]}"; echo $?)
    kube::test::if_has_string "${output_message}" '0'

    # kubectl auth can-i get '*' does not warn about namespaced scope or print an error
    output_message=$(kubectl auth can-i get '*' 2>&1 "${kube_flags[@]}")
    kube::test::if_has_not_string "${output_message}" "Warning"

    # kubectl auth can-i get foo does not print a namespaced warning message, and only prints a single lookup error
    output_message=$(kubectl auth can-i get foo 2>&1 "${kube_flags[@]}")
    kube::test::if_has_string "${output_message}" "Warning: the server doesn't have a resource type 'foo'"
    kube::test::if_has_not_string "${output_message}" "Warning: resource 'foo' is not namespace scoped"

    # kubectl auth can-i get pods does not print a namespaced warning message or a lookup error
    output_message=$(kubectl auth can-i get pods 2>&1 "${kube_flags[@]}")
    kube::test::if_has_not_string "${output_message}" "Warning"

    # kubectl auth can-i get nodes prints a namespaced warning message
    output_message=$(kubectl auth can-i get nodes 2>&1 "${kube_flags[@]}")
    kube::test::if_has_string "${output_message}" "Warning: resource 'nodes' is not namespace scoped"

    # kubectl auth can-i get nodes --all-namespaces does not print a namespaced warning message
    output_message=$(kubectl auth can-i get nodes --all-namespaces 2>&1 "${kube_flags[@]}")
    kube::test::if_has_not_string "${output_message}" "Warning: resource 'nodes' is not namespace scoped"
  fi

  # kubectl auth reconcile
  if kube::test::if_supports_resource "${clusterroles}" ; then
    # dry-run command
    kubectl auth reconcile --dry-run=client "${kube_flags[@]}" -f test/fixtures/pkg/kubectl/cmd/auth/rbac-resource-plus.yaml
    kube::test::get_object_assert 'rolebindings -n some-other-random -l test-cmd=auth' "{{range.items}}{{$id_field}}:{{end}}" ''
    kube::test::get_object_assert 'roles -n some-other-random -l test-cmd=auth' "{{range.items}}{{$id_field}}:{{end}}" ''
    kube::test::get_object_assert 'clusterrolebindings -l test-cmd=auth' "{{range.items}}{{$id_field}}:{{end}}" ''
    kube::test::get_object_assert 'clusterroles -l test-cmd=auth' "{{range.items}}{{$id_field}}:{{end}}" ''

    # command
    kubectl auth reconcile "${kube_flags[@]}" -f test/fixtures/pkg/kubectl/cmd/auth/rbac-resource-plus.yaml
    kube::test::get_object_assert 'rolebindings -n some-other-random -l test-cmd=auth' "{{range.items}}{{$id_field}}:{{end}}" 'testing-RB:'
    kube::test::get_object_assert 'roles -n some-other-random -l test-cmd=auth' "{{range.items}}{{$id_field}}:{{end}}" 'testing-R:'
    kube::test::get_object_assert 'clusterrolebindings -l test-cmd=auth' "{{range.items}}{{$id_field}}:{{end}}" 'testing-CRB:'
    kube::test::get_object_assert 'clusterroles -l test-cmd=auth' "{{range.items}}{{$id_field}}:{{end}}" 'testing-CR:'

    failure_message=$(! kubectl auth reconcile "${kube_flags[@]}" -f test/fixtures/pkg/kubectl/cmd/auth/rbac-v1beta1.yaml 2>&1 )
    kube::test::if_has_string "${failure_message}" 'only rbac.authorization.k8s.io/v1 is supported'

    kubectl delete "${kube_flags[@]}" rolebindings,role,clusterroles,clusterrolebindings -n some-other-random -l test-cmd=auth
  fi



  #####################
  # Retrieve multiple #
  #####################

  if kube::test::if_supports_resource "${nodes}" ; then
    if kube::test::if_supports_resource "${services}" ; then
      record_command run_retrieve_multiple_tests
    fi
  fi


  #####################
  # Resource aliasing #
  #####################

  if kube::test::if_supports_resource "${services}" ; then
    if kube::test::if_supports_resource "${replicationcontrollers}" ; then
      record_command run_resource_aliasing_tests
    fi
  fi

  ###########
  # Explain #
  ###########

  if kube::test::if_supports_resource "${pods}" ; then
    record_command run_kubectl_explain_tests
  fi

  ##############################
  # CRD Deletion / Re-creation #
  ##############################

  if kube::test::if_supports_resource "${namespaces}" ; then
      record_command run_crd_deletion_recreation_tests
  fi

  ###########
  # Swagger #
  ###########

  record_command run_swagger_tests

  #####################
  # Kubectl --sort-by #
  #####################

  if kube::test::if_supports_resource "${pods}" ; then
    record_command run_kubectl_sort_by_tests
  fi

  ############################
  # Kubectl --all-namespaces #
  ############################

  if kube::test::if_supports_resource "${pods}" ; then
    if kube::test::if_supports_resource "${nodes}" ; then
      record_command run_kubectl_all_namespace_tests
    fi
  fi

  ############################
  # Kubectl deprecated APIs  #
  ############################

  if kube::test::if_supports_resource "${customresourcedefinitions}" ; then
    record_command run_deprecated_api_tests
  fi


  ######################
  # kubectl --template #
  ######################

  if kube::test::if_supports_resource "${pods}" ; then
    record_command run_template_output_tests
  fi

  ################
  # Certificates #
  ################

  if kube::test::if_supports_resource "${csr}" ; then
    record_command run_certificates_tests
  fi

  ######################
  # Cluster Management #
  ######################
  if kube::test::if_supports_resource "${nodes}" ; then
    record_command run_cluster_management_tests
  fi

  ###########
  # Plugins #
  ###########

  record_command run_plugins_tests


  #################
  # Impersonation #
  #################
  record_command run_impersonation_tests

  ####################
  # kubectl wait     #
  ####################

  record_command run_wait_tests

  ####################
  # kubectl debug    #
  ####################
  if kube::test::if_supports_resource "${pods}" ; then
    record_command run_kubectl_debug_pod_tests
    record_command run_kubectl_debug_general_tests
    record_command run_kubectl_debug_baseline_tests
    record_command run_kubectl_debug_restricted_tests
    record_command run_kubectl_debug_netadmin_tests
    record_command run_kubectl_debug_custom_profile_tests
  fi
  if kube::test::if_supports_resource "${nodes}" ; then
    record_command run_kubectl_debug_node_tests
    record_command run_kubectl_debug_general_node_tests
    record_command run_kubectl_debug_baseline_node_tests
    record_command run_kubectl_debug_restricted_node_tests
    record_command run_kubectl_debug_netadmin_node_tests
  fi

  #######################
  # kuberc              #
  #######################

  record_command run_kuberc_tests

  cleanup_tests
}
