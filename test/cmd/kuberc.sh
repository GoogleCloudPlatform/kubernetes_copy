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

run_kuberc_tests() {
  set -o nounset
  set -o errexit

  create_and_use_new_namespace
  kube::log::status "Testing kuberc"

  # Enable KUBERC feature
  export KUBECTL_KUBERC=true

  cat > "${TMPDIR:-/tmp}"/kuberc_file << EOF
apiVersion: kubectl.config.k8s.io/v1alpha1
kind: Preference
aliases:
- name: crns
  command: create namespace
  appendArgs:
   - test-kuberc-ns
- name: getn
  command: get
  prependArgs:
   - namespace
  flags:
   - name: output
     default: wide
- name: crole
  command: create role
  flags:
  - name: verb
    default: get,watch
- name: getrole
  command: get
  flags:
  - name: output
    default: json
- name: runx
  command: run
  flags:
  - name: image
    default: nginx
  - name: labels
    default: app=test,env=test
  - name: env
    default: DNS_DOMAIN=test
  - name: namespace
    default: test-kuberc-ns
  appendArgs:
  - test-pod-2
  - --
  - custom-arg1
  - custom-arg2
- name: setx
  command: set image
  appendArgs:
  - pod/test-pod-2
  - test-pod-2=busybox
overrides:
- command: apply
  flags:
  - name: server-side
    default: "true"
  - name: dry-run
    default: "server"
  - name: validate
    default: "strict"
- command: delete
  flags:
  - name: interactive
    default: "true"
- command: get
  flags:
  - name: namespace
    default: "test-kuberc-ns"
  - name: output
    default: "json"
EOF

  # Pre-condition: the test-kuberc-ns namespace does not exist
  kube::test::get_object_assert 'namespaces' "{{range.items}}{{ if eq ${id_field:?} \"test-kuberc-ns\" }}found{{end}}{{end}}:" ':'
  # Alias command crns successfully creates namespace
  kubectl crns --kuberc="${TMPDIR:-/tmp}"/kuberc_file
  # Post-condition: namespace 'test-kuberc-ns' is created.
  kube::test::get_object_assert 'namespaces/test-kuberc-ns' "{{$id_field}}" 'test-kuberc-ns'

  # Alias command crns successfully creates namespace
  kubectl getn --kuberc="${TMPDIR:-/tmp}"/kuberc_file test-kuberc-ns
  # Post-condition: namespace 'test-kuberc-ns' is created.
  kube::test::get_object_assert 'namespaces/test-kuberc-ns' "{{$id_field}}" 'test-kuberc-ns'

  # Alias command crns successfully creates namespace
  kubectl getn test-kuberc-ns --output=json --kuberc="${TMPDIR:-/tmp}"/kuberc_file
  # Post-condition: namespace 'test-kuberc-ns' is created.
  kube::test::get_object_assert 'namespaces/test-kuberc-ns' "{{$id_field}}" 'test-kuberc-ns'

  # check array flags are appended after implicit defaults
  kubectl crole testkubercrole --verb=list --namespace test-kuberc-ns --resource=pods --kuberc="${TMPDIR:-/tmp}"/kuberc_file
  output_message=$(kubectl getrole role/testkubercrole -n test-kuberc-ns -oyaml --kuberc="${TMPDIR:-/tmp}"/kuberc_file)
  kube::test::if_has_string "${output_message}" 'list'
  kube::test::if_has_not_string "${output_message}" 'watch' 'get'
  # Post-condition: remove role
  kubectl delete role testkubercrole --namespace=test-kuberc-ns

  # Alias run command creates a pod with the given configurations
  kubectl runx --kuberc "${TMPDIR:-/tmp}"/kuberc_file
  # Post-Condition: assertion object exists
  kube::test::get_object_assert 'pod/test-pod-2 --namespace=test-kuberc-ns' "{{$id_field}}" 'test-pod-2'
  # Not explicitly pass namespace to assure that default flag value is used
  output_message=$(kubectl get pod/test-pod-2 2>&1 "${kube_flags[@]:?}" --kuberc="${TMPDIR:-/tmp}"/kuberc_file)
  kube::test::if_has_string "${output_message}" 'nginx' 'app=test' 'env=test' 'DNS_DOMAIN=test' 'custom-arg1'
  # output flag is defaulted to json and assure that it is correct format
  kube::test::if_has_string "${output_message}" '{'

  # pass explicit invalid namespace to assure that it takes precedence over the value in kuberc
  output_message=$(! kubectl get pod/test-pod-2 -n kube-system 2>&1 "${kube_flags[@]:?}" --kuberc="${TMPDIR:-/tmp}"/kuberc_file)
  kube::test::if_has_string "${output_message}" 'pods "test-pod-2" not found'

  # Alias set env command sets new env var
  kubectl setx --kuberc="${TMPDIR:-/tmp}"/kuberc_file -n test-kuberc-ns
  # explicitly pass same namespace also defined in kuberc
  output_message=$(kubectl get pod/test-pod-2 -n test-kuberc-ns 2>&1 "${kube_flags[@]:?}" --kuberc="${TMPDIR:-/tmp}"/kuberc_file)
  kube::test::if_has_string "${output_message}" 'busybox'
  kube::test::if_has_not_string "${output_message}" 'nginx'

  # default overrides should prevent actual apply as they are all dry-run=server
  # also assure that explicit flags are also passed
  output_message=$(kubectl apply -n test-kuberc-ns -f hack/testdata/pod.yaml --kuberc="${TMPDIR:-/tmp}"/kuberc_file)
  kube::test::if_has_string "${output_message}" 'serverside-applied (server dry run)'

  # interactive flag is defaulted to true and prompted as no
  output_message=$(kubectl delete pod/test-pod-2 -n test-kuberc-ns <<< $'n\n' --kuberc="${TMPDIR:-/tmp}"/kuberc_file)
  kube::test::if_has_string "${output_message}" 'pod/test-pod-2'
  # assure that it is not deleted
  output_message=$(kubectl get pod/test-pod-2 2>&1 "${kube_flags[@]:?}" --kuberc="${TMPDIR:-/tmp}"/kuberc_file)
  kube::test::if_has_string "${output_message}" "test-pod-2"

  cat > "${TMPDIR:-/tmp}"/kuberc_file_multi << EOF
---
apiVersion: kubectl.config.k8s.io/v1alpha1
kind: Preference
overrides:
- command: get
  flags:
  - name: namespace
    default: "test-kuberc-ns"
  - name: output
    default: "json"
unknown: invalid
---
apiVersion: kubectl.config.k8s.io/notexist
kind: Preference
overrides:
- command: get
  flags:
  - name: namespace
    default: "test-kuberc-ns"
  - name: output
    default: "json"
EOF

  # assure that it is not deleted
  output_message=$(kubectl get pod/test-pod-2 2>&1 "${kube_flags[@]:?}" --kuberc="${TMPDIR:-/tmp}"/kuberc_file_multi)
  # assure that correct kuberc is found and printed in output_message
  kube::test::if_has_string "${output_message}" "test-pod-2"
  # assure that warning message is also printed for the notexist kuberc version
  kube::test::if_has_string "${output_message}" "strict decoding error" "unknown"

  # explicitly overwriting the value that is also defaulted in kuberc and
  # assure that explicit value supersedes
  output_message=$(kubectl delete namespace/test-kuberc-ns --interactive=false --kuberc="${TMPDIR:-/tmp}"/kuberc_file)
  kube::test::if_has_string "${output_message}" 'namespace "test-kuberc-ns" deleted'

  unset KUBECTL_KUBERC

  set +o nounset
  set +o errexit
}
