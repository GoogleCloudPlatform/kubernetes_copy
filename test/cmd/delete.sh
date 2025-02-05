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

set -o errexit
set -o nounset
set -o pipefail

# Function to create namespaces
create_namespaces() {
  ns_one="namespace-$(date +%s)-${RANDOM}"
  ns_two="namespace-$(date +%s)-${RANDOM}"
  kubectl create namespace "${ns_one}"
  kubectl create namespace "${ns_two}"
}

# Runs tests related to kubectl delete --all-namespaces.
run_kubectl_delete_allnamespaces_tests() {
  set -o nounset
  set -o errexit

  create_namespaces

  kubectl create configmap "one" --namespace="${ns_one}"
  kubectl create configmap "two" --namespace="${ns_two}"
  kubectl label configmap "one" --namespace="${ns_one}" deletetest=true
  kubectl label configmap "two" --namespace="${ns_two}" deletetest=true

  # dry-run
  kubectl delete configmap --dry-run=client -l deletetest=true --all-namespaces
  kubectl delete configmap --dry-run=server -l deletetest=true --all-namespaces
  kubectl config set-context "${CONTEXT}" --namespace="${ns_one}"
  kube::test::get_object_assert 'configmap -l deletetest' "{{range.items}}{{${id_field:?}}}:{{end}}" 'one:'
  kubectl config set-context "${CONTEXT}" --namespace="${ns_two}"
  kube::test::get_object_assert 'configmap -l deletetest' "{{range.items}}{{${id_field:?}}}:{{end}}" 'two:'

  kubectl delete configmap -l deletetest=true --all-namespaces

  # no configmaps should be in either of those namespaces with label deletetest
  kubectl config set-context "${CONTEXT}" --namespace="${ns_one}"
  kube::test::get_object_assert 'configmap -l deletetest' "{{range.items}}{{${id_field:?}}}:{{end}}" ''
  kubectl config set-context "${CONTEXT}" --namespace="${ns_two}"
  kube::test::get_object_assert 'configmap -l deletetest' "{{range.items}}{{${id_field:?}}}:{{end}}" ''

  set +o nounset
  set +o errexit
}

# Runs tests related to kubectl delete --confirm
run_kubectl_delete_interactive_tests() {
  set -o nounset
  set -o errexit

  create_namespaces

  # create configmaps
  kubectl create configmap "one" --namespace="${ns_one}"
  kubectl create configmap "two" --namespace="${ns_two}"
  kubectl label configmap "one" --namespace="${ns_one}" deletetest=true
  kubectl label configmap "two" --namespace="${ns_two}" deletetest=true

  # not confirm dry-run=server deletions
  output_message=$(kubectl delete configmap --dry-run=server --interactive -l deletetest=true --all-namespaces <<< $'n\n')
  kube::test::if_has_string "${output_message}" 'configmap/two' 'configmap/one'
  # confirm dry-run=server deletions
  kubectl delete configmap --dry-run=server --interactive -l deletetest=true --all-namespaces <<< $'y\n'
  # not confirm resource deletions
  output_message=$(kubectl delete configmap --interactive -l deletetest=true --all-namespaces <<< $'n\n')
  kube::test::if_has_string "${output_message}" 'configmap/two' 'configmap/one'
  kubectl config set-context "${CONTEXT}" --namespace="${ns_one}"
  kube::test::get_object_assert 'configmap -l deletetest' "{{range.items}}{{${id_field:?}}}:{{end}}" 'one:'
  kubectl config set-context "${CONTEXT}" --namespace="${ns_two}"
  kube::test::get_object_assert 'configmap -l deletetest' "{{range.items}}{{${id_field:?}}}:{{end}}" 'two:'

  # clean configmaps with label deletetest=true
  kubectl delete configmap -l deletetest=true --all-namespaces

  # create new configmaps
  kubectl create configmap "third" --namespace="${ns_one}"
  kubectl create configmap "fourth" --namespace="${ns_two}"
  kubectl label configmap "third" --namespace="${ns_one}" deletetest2=true
  kubectl label configmap "fourth" --namespace="${ns_two}" deletetest2=true

  # confirm all resource deletions with waiting
  kubectl delete configmaps --interactive -l deletetest2=true --all-namespaces --wait <<< $'y\n'

  # no configmaps should be in either of those namespaces with label deletetest2
  kubectl config set-context "${CONTEXT}" --namespace="${ns_one}"
  kube::test::get_object_assert 'configmap -l deletetest2' "{{range.items}}{{${id_field:?}}}:{{end}}" ''
  kubectl config set-context "${CONTEXT}" --namespace="${ns_two}"
  kube::test::get_object_assert 'configmap -l deletetest2' "{{range.items}}{{${id_field:?}}}:{{end}}" ''

  # clean configmaps with label deletetest2=true
  kubectl delete configmap -l deletetest2=true --all-namespaces

  # create new configmaps in one namespace
  kubectl create configmap "fifth" --namespace="${ns_one}"
  kubectl create configmap "sixth" --namespace="${ns_one}"
  kubectl label configmap "fifth" --namespace="${ns_one}" deletetest3=true
  kubectl label configmap "sixth" --namespace="${ns_one}" deletetest3=true

  # confirm all resource deletions with forcing and waiting
  kubectl delete configmaps -l deletetest3=true --force --interactive --namespace="${ns_one}" --wait <<< $'y\n'

  # no configmaps should be in either of those namespaces with label deletetest3
  kubectl config set-context "${CONTEXT}" --namespace="${ns_one}"
  kube::test::get_object_assert 'configmap -l deletetest3' "{{range.items}}{{${id_field:?}}}:{{end}}" ''

  set +o nounset
  set +o errexit
}
