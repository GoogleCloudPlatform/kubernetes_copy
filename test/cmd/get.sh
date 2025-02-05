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

run_kubectl_get_tests() {
  set -o nounset
  set -o errexit

  create_and_use_new_namespace
  kube::log::status "Testing kubectl get"
  ### Test retrieval of non-existing pods
  # Pre-condition: no POD exists
  kube::test::get_object_assert pods "{{range.items}}{{${id_field:?}}}:{{end}}" ''
  # Command
  output_message=$(! kubectl get pods abc 2>&1 "${kube_flags[@]:?}")
  # Post-condition: POD abc should error since it doesn't exist
  kube::test::if_has_string "${output_message}" 'pods "abc" not found'

  ### Test retrieval of non-existing POD with output flag specified
  # Pre-condition: no POD exists
  kube::test::get_object_assert pods "{{range.items}}{{$id_field}}:{{end}}" ''
  # Command
  output_message=$(! kubectl get pods abc 2>&1 "${kube_flags[@]}" -o name)
  # Post-condition: POD abc should error since it doesn't exist
  kube::test::if_has_string "${output_message}" 'pods "abc" not found'

  ### Test retrieval of pods when none exist with non-human readable output format flag specified
  # Pre-condition: no pods exist
  kube::test::get_object_assert pods "{{range.items}}{{$id_field}}:{{end}}" ''
  # Command
  output_message=$(kubectl get pods 2>&1 "${kube_flags[@]}" -o json)
  # Post-condition: The text "No resources found" should not be part of the output
  kube::test::if_has_not_string "${output_message}" 'No resources found'
  # Command
  output_message=$(kubectl get pods 2>&1 "${kube_flags[@]}" -o yaml)
  # Post-condition: The text "No resources found" should not be part of the output
  kube::test::if_has_not_string "${output_message}" 'No resources found'
  # Command
  output_message=$(kubectl get pods 2>&1 "${kube_flags[@]}" -o name)
  # Post-condition: The text "No resources found" should not be part of the output
  kube::test::if_has_not_string "${output_message}" 'No resources found'
  # Command
  output_message=$(kubectl get pods 2>&1 "${kube_flags[@]}" -o jsonpath='{.items}')
  # Post-condition: The text "No resources found" should not be part of the output
  kube::test::if_has_not_string "${output_message}" 'No resources found'
  # Command
  output_message=$(kubectl get pods 2>&1 "${kube_flags[@]}" -o go-template='{{.items}}')
  # Post-condition: The text "No resources found" should not be part of the output
  kube::test::if_has_not_string "${output_message}" 'No resources found'
  # Command
  output_message=$(kubectl get pods 2>&1 "${kube_flags[@]}" -o custom-columns=NAME:.metadata.name)
  # Post-condition: The text "No resources found" should not be part of the output
  kube::test::if_has_not_string "${output_message}" 'No resources found'

  ### Test retrieval of pods when none exist, with human-readable output format flag specified
  # Pre-condition: no pods exist
  kube::test::get_object_assert pods "{{range.items}}{{$id_field}}:{{end}}" ''
  # Command
  output_message=$(! kubectl get foobar 2>&1 "${kube_flags[@]}")
  # Post-condition: The text "No resources found" should not be part of the output when an error occurs
  kube::test::if_has_not_string "${output_message}" 'No resources found'
  # Command
  output_message=$(kubectl get pods 2>&1 "${kube_flags[@]}")
  # Post-condition: The text "No resources found" should be part of the output
  kube::test::if_has_string "${output_message}" 'No resources found'
  # Command
  output_message=$(kubectl get pods --ignore-not-found 2>&1 "${kube_flags[@]}")
  # Post-condition: The text "No resources found" should not be part of the output
  kube::test::if_has_not_string "${output_message}" 'No resources found'
  # Command
  output_message=$(kubectl get pods 2>&1 "${kube_flags[@]}" -o wide)
  # Post-condition: The text "No resources found" should be part of the output
  kube::test::if_has_string "${output_message}" 'No resources found'

  ### Test retrieval of non-existing POD with json output flag specified
  # Pre-condition: no POD exists
  kube::test::get_object_assert pods "{{range.items}}{{$id_field}}:{{end}}" ''
  # Command
  output_message=$(! kubectl get pods abc 2>&1 "${kube_flags[@]}" -o json)
  # Post-condition: POD abc should error since it doesn't exist
  kube::test::if_has_string "${output_message}" 'pods "abc" not found'
  # Post-condition: make sure we don't display an empty List
  kube::test::if_has_not_string "${output_message}" 'List'

  ### Test kubectl get all
  output_message=$(kubectl --v=6 --namespace default get all --chunk-size=0 2>&1 "${kube_flags[@]}")
  # Post-condition: Check if we get 200 OK from all the url(s)
  kube::test::if_has_string "${output_message}" '"Response" verb="GET" url=".*/api/v1/namespaces/default/pods" status="200 OK"'
  kube::test::if_has_string "${output_message}" '"Response" verb="GET" url=".*/api/v1/namespaces/default/replicationcontrollers" status="200 OK"'
  kube::test::if_has_string "${output_message}" '"Response" verb="GET" url=".*/api/v1/namespaces/default/services" status="200 OK"'
  kube::test::if_has_string "${output_message}" '"Response" verb="GET" url=".*/apis/apps/v1/namespaces/default/daemonsets" status="200 OK"'
  kube::test::if_has_string "${output_message}" '"Response" verb="GET" url=".*/apis/apps/v1/namespaces/default/deployments" status="200 OK"'
  kube::test::if_has_string "${output_message}" '"Response" verb="GET" url=".*/apis/apps/v1/namespaces/default/replicasets" status="200 OK"'
  kube::test::if_has_string "${output_message}" '"Response" verb="GET" url=".*/apis/apps/v1/namespaces/default/statefulsets" status="200 OK"'
  kube::test::if_has_string "${output_message}" '"Response" verb="GET" url=".*/apis/autoscaling/v2/namespaces/default/horizontalpodautoscalers" status="200 OK"'
  kube::test::if_has_string "${output_message}" '"Response" verb="GET" url=".*/apis/batch/v1/namespaces/default/jobs" status="200 OK"'
  kube::test::if_has_not_string "${output_message}" '"Response" verb="GET" url=".*/apis/extensions/v1beta1/namespaces/default/daemonsets" status="200 OK"'
  kube::test::if_has_not_string "${output_message}" '"Response" verb="GET" url=".*/apis/extensions/v1beta1/namespaces/default/deployments" status="200 OK"'
  kube::test::if_has_not_string "${output_message}" '"Response" verb="GET" url=".*/apis/extensions/v1beta1/namespaces/default/replicasets" status="200 OK"'

  ### Test kubectl get chunk size
  output_message=$(kubectl --v=6 get clusterrole --chunk-size=10 2>&1 "${kube_flags[@]}")
  # Post-condition: Check if we get a limit and continue
  kube::test::if_has_string "${output_message}" '"Response" verb="GET" url=".*/clusterroles?limit=10" status="200 OK"'
  kube::test::if_has_string "${output_message}" '"Response" verb="GET" url=".*/v1/clusterroles?continue=.*'

  ### Test kubectl get chunk size defaults to 500
  output_message=$(kubectl --v=6 get clusterrole 2>&1 "${kube_flags[@]}")
  # Post-condition: Check if we get a limit and continue
  kube::test::if_has_string "${output_message}" '"Response" verb="GET" url=".*/clusterroles?limit=500" status="200 OK"'

  ### Test kubectl get accumulates pages
  output_message=$(kubectl get namespaces --chunk-size=1 --no-headers "${kube_flags[@]}")
  # Post-condition: Check we got multiple pages worth of namespaces
  kube::test::if_has_string "${output_message}" "default"
  kube::test::if_has_string "${output_message}" "kube-public"
  kube::test::if_has_string "${output_message}" "kube-system"

  ### Test kubectl get chunk size does not result in a --watch error when resource list is served in multiple chunks
  # Pre-condition: ConfigMap one two tree does not exist
  kube::test::get_object_assert 'configmaps' "{{range.items}}{{ if eq $id_field \"one\" }}found{{end}}{{end}}:" ':'
  kube::test::get_object_assert 'configmaps' "{{range.items}}{{ if eq $id_field \"two\" }}found{{end}}{{end}}:" ':'
  kube::test::get_object_assert 'configmaps' "{{range.items}}{{ if eq $id_field \"three\" }}found{{end}}{{end}}:" ':'

  # Post-condition: Create three configmaps and ensure that we can --watch them with a --chunk-size of 1
  kubectl create cm one "${kube_flags[@]}"
  kubectl create cm two "${kube_flags[@]}"
  kubectl create cm three "${kube_flags[@]}"
  output_message=$(kubectl get configmap --chunk-size=1 --watch --request-timeout=1s 2>&1 "${kube_flags[@]}")
  kube::test::if_has_not_string "${output_message}" "watch is only supported on individual resources"
  output_message=$(kubectl get configmap --chunk-size=1 --watch-only --request-timeout=1s 2>&1 "${kube_flags[@]}")
  kube::test::if_has_not_string "${output_message}" "watch is only supported on individual resources"

  ### Test --allow-missing-template-keys
  # Pre-condition: no POD exists
  create_and_use_new_namespace
  kube::test::get_object_assert pods "{{range.items}}{{$id_field}}:{{end}}" ''
  # Command
  kubectl create -f test/fixtures/doc-yaml/admin/limitrange/valid-pod.yaml "${kube_flags[@]}"
  # Post-condition: valid-pod POD is created
  kubectl get "${kube_flags[@]}" pods -o json
  kube::test::get_object_assert pods "{{range.items}}{{$id_field}}:{{end}}" 'valid-pod:'

  ## check --allow-missing-template-keys defaults to true for jsonpath templates
  kubectl get "${kube_flags[@]}" pod valid-pod -o jsonpath='{.missing}'

  ## check --allow-missing-template-keys defaults to true for go templates
  kubectl get "${kube_flags[@]}" pod valid-pod -o go-template='{{.missing}}'

  ## check --template flag causes go-template to be printed, even when no --output value is provided
  output_message=$(kubectl get "${kube_flags[@]}" pod valid-pod --template="{{$id_field}}:")
  kube::test::if_has_string "${output_message}" 'valid-pod:'

  ## check --allow-missing-template-keys=false results in an error for a missing key with jsonpath
  output_message=$(! kubectl get pod valid-pod --allow-missing-template-keys=false -o jsonpath='{.missing}' 2>&1 "${kube_flags[@]}")
  kube::test::if_has_string "${output_message}" 'missing is not found'

  ## check --allow-missing-template-keys=false results in an error for a missing key with go
  output_message=$(! kubectl get pod valid-pod --allow-missing-template-keys=false -o go-template='{{.missing}}' "${kube_flags[@]}")
  kube::test::if_has_string "${output_message}" 'map has no entry for key "missing"'

  ## check --subresource=status works
  output_message=$(kubectl get "${kube_flags[@]}" pod valid-pod --subresource=status)
  kube::test::if_has_string "${output_message}" 'valid-pod'

   ## check --subresource=scale returns an error for pods
  output_message=$(! kubectl get pod valid-pod --subresource=scale 2>&1 "${kube_flags[@]:?}")
  kube::test::if_has_string "${output_message}" 'the server could not find the requested resource'

  ### Test kubectl get watch
  output_message=$(kubectl get pods -w --request-timeout=1 "${kube_flags[@]}")
  kube::test::if_has_string "${output_message}" 'STATUS'    # headers
  kube::test::if_has_string "${output_message}" 'valid-pod' # pod details
  output_message=$(kubectl get pods/valid-pod -o name -w --request-timeout=1 "${kube_flags[@]}")
  kube::test::if_has_not_string "${output_message}" 'STATUS' # no headers
  kube::test::if_has_string     "${output_message}" 'pod/valid-pod' # resource name
  output_message=$(kubectl get pods/valid-pod -o yaml -w --request-timeout=1 "${kube_flags[@]}")
  kube::test::if_has_not_string "${output_message}" 'STATUS'          # no headers
  kube::test::if_has_string     "${output_message}" 'name: valid-pod' # yaml
  output_message=$(! kubectl get pods/invalid-pod -w --request-timeout=1 "${kube_flags[@]}" 2>&1)
  kube::test::if_has_string "${output_message}" '"invalid-pod" not found'

  # cleanup
  kubectl delete pods valid-pod "${kube_flags[@]}"

  ### Test 'kubectl get -f <file> -o <non default printer>' prints all the items in the file's list
  # Pre-condition: no POD exists
  kube::test::get_object_assert pods "{{range.items}}{{$id_field}}:{{end}}" ''
  # Command
  kubectl create -f test/fixtures/doc-yaml/user-guide/multi-pod.yaml "${kube_flags[@]}"
  # Post-condition: PODs redis-master and valid-pod exist

  # Check that all items in the list are printed
  output_message=$(kubectl get -f test/fixtures/doc-yaml/user-guide/multi-pod.yaml -o jsonpath="{..metadata.name}" "${kube_flags[@]}")
  kube::test::if_has_string "${output_message}" "redis-master valid-pod"

  # cleanup
  kubectl delete pods redis-master valid-pod "${kube_flags[@]}"

  ### Test 'kubectl get -k <dir>' prints all the items built from a kustomization directory
  # Pre-Condition: No configmaps with name=test-the-map, no Deployment, Service exist
  kube::test::get_object_assert 'configmaps --field-selector=metadata.name=test-the-map' "{{range.items}}{{${id_field:?}}}:{{end}}" ''
  kube::test::get_object_assert deployment "{{range.items}}{{$id_field}}:{{end}}" ''
  kube::test::get_object_assert services "{{range.items}}{{$id_field}}:{{end}}" ''
  # Command
  kubectl apply -k hack/testdata/kustomize
  # Post-condition: test-the-map, test-the-deployment, test-the-service exist

  # Check that all items in the list are printed
  output_message=$(kubectl get -k hack/testdata/kustomize -o jsonpath="{..metadata.name}" "${kube_flags[@]}")
  kube::test::if_has_string "${output_message}" "test-the-map"
  kube::test::if_has_string "${output_message}" "test-the-deployment"
  kube::test::if_has_string "${output_message}" "test-the-service"

  # cleanup
  kubectl delete -k hack/testdata/kustomize

  # Check that all items in the list are deleted
  kube::test::get_object_assert 'configmaps --field-selector=metadata.name=test-the-map' "{{range.items}}{{${id_field:?}}}:{{end}}" ''
  kube::test::get_object_assert deployment "{{range.items}}{{$id_field}}:{{end}}" ''
  kube::test::get_object_assert services "{{range.items}}{{$id_field}}:{{end}}" ''

  set +o nounset
  set +o errexit
}

run_retrieve_multiple_tests() {
  set -o nounset
  set -o errexit

  # switch back to the default namespace
  kubectl config set-context "${CONTEXT}" --namespace=""
  kube::log::status "Testing kubectl(v1:multiget)"
  kube::test::get_object_assert 'nodes/127.0.0.1 service/kubernetes' "{{range.items}}{{$id_field}}:{{end}}" '127.0.0.1:kubernetes:'

  set +o nounset
  set +o errexit
}

run_kubectl_response_compression_tests() {
  set -o nounset
  set -o errexit

  create_and_use_new_namespace
  kube::log::status "Testing kubectl get objects with/without response compression"

  ### Test creation of large configmap objects
  # Pre-condition: no configmaps exists
  kube::test::get_object_assert configmaps "{{range.items}}{{${id_field:?}}}:{{end}}" ''
  # Commands to create 3 configmaps each of size 50KB. Sum of their sizes should be >128KB (defaultGzipThresholdBytes)
  # for apiserver to allow gzip compression of the response. This is required to test the disable-compression option
  # from client-side
  some_string=$(dd status=none if=/dev/urandom bs=1K count=50 2>/dev/null | base64)
  kubectl create configmap "cm-one" --from-literal=somekey="${some_string}"
  kubectl create configmap "cm-two" --from-literal=somekey="${some_string}"
  kubectl create configmap "cm-three" --from-literal=somekey="${some_string}"
  output_message=$(kubectl get configmaps 2>&1 "${kube_flags[@]:?}")
  # Post-condition: All configmaps should be created
  kube::test::if_has_string "${output_message}" "cm-one"
  kube::test::if_has_string "${output_message}" "cm-two"
  kube::test::if_has_string "${output_message}" "cm-three"

  ### Test list call WITH compression
  output_message=$(kubectl get configmaps --v=8 2>&1 "${kube_flags[@]:?}")
  # Post-condition: Response headers should include "accept-encoding" header
  kube::test::if_has_string "${output_message}" "Vary: Accept-Encoding"

  ### Test list call WITHOUT compression
  output_message=$(kubectl get configmaps --disable-compression=true --v=8 2>&1 "${kube_flags[@]:?}")
  # Post-condition: Response headers should NOT include "accept-encoding" header
  kube::test::if_has_not_string "${output_message}" "Vary: Accept-Encoding"

  # cleanup
  kubectl delete configmap "cm-one"
  kubectl delete configmap "cm-two"
  kubectl delete configmap "cm-three"

  set +o nounset
  set +o errexit
}

run_kubectl_sort_by_tests() {
  set -o nounset
  set -o errexit

  kube::log::status "Testing kubectl --sort-by"

  ### sort-by should not panic if no pod exists
  # Pre-condition: no POD exists
  kube::test::get_object_assert pods "{{range.items}}{{$id_field}}:{{end}}" ''
  # Command
  kubectl get pods --sort-by="{metadata.name}"
  kubectl get pods --sort-by="{metadata.creationTimestamp}"

  ### sort-by should works if pod exists
  # Create POD
  # Pre-condition: no POD exists
  kube::test::get_object_assert pods "{{range.items}}{{$id_field}}:{{end}}" ''
  # Command
  kubectl create "${kube_flags[@]}" -f test/fixtures/doc-yaml/admin/limitrange/valid-pod.yaml
  # Post-condition: valid-pod is created
  kube::test::get_object_assert pods "{{range.items}}{{$id_field}}:{{end}}" 'valid-pod:'
  # Check output of sort-by
  output_message=$(kubectl get pods --sort-by="{metadata.name}")
  kube::test::if_has_string "${output_message}" "valid-pod"
  # ensure sort-by receivers objects as Table
  output_message=$(kubectl get pods --v=8 --sort-by="{metadata.name}" 2>&1)
  kube::test::if_has_string "${output_message}" "as=Table"
  # ensure sort-by requests the full object
  kube::test::if_has_string "${output_message}" "includeObject=Object"
  ### Clean up
  # Pre-condition: valid-pod exists
  kube::test::get_object_assert pods "{{range.items}}{{$id_field}}:{{end}}" 'valid-pod:'
  # Command
  kubectl delete "${kube_flags[@]}" pod valid-pod --grace-period=0 --force
  # Post-condition: valid-pod doesn't exist
  kube::test::get_object_assert pods "{{range.items}}{{$id_field}}:{{end}}" ''

  ### sort-by should works by sorting by name
  # Create three PODs
  # Pre-condition: no POD exists
  kube::test::get_object_assert pods "{{range.items}}{{$id_field}}:{{end}}" ''
  # Command
  kubectl create "${kube_flags[@]}" -f hack/testdata/sorted-pods/sorted-pod1.yaml
  # Post-condition: sorted-pod1 is created
  kube::test::get_object_assert pods "{{range.items}}{{$id_field}}:{{end}}" 'sorted-pod1:'
  # Command
  kubectl create "${kube_flags[@]}" -f hack/testdata/sorted-pods/sorted-pod2.yaml
  # Post-condition: sorted-pod1 is created
  kube::test::get_object_assert pods "{{range.items}}{{$id_field}}:{{end}}" 'sorted-pod1:sorted-pod2:'
  # Command
  kubectl create "${kube_flags[@]}" -f hack/testdata/sorted-pods/sorted-pod3.yaml
  # Post-condition: sorted-pod1 is created
  kube::test::get_object_assert pods "{{range.items}}{{$id_field}}:{{end}}" 'sorted-pod1:sorted-pod2:sorted-pod3:'

  # Check output of sort-by '{metadata.name}'
  output_message=$(kubectl get pods --sort-by="{metadata.name}")
  kube::test::if_sort_by_has_correct_order "${output_message}" "sorted-pod1:sorted-pod2:sorted-pod3:"

  # Check output of sort-by '{metadata.labels.name}'
  output_message=$(kubectl get pods --sort-by="{metadata.labels.name}")
  kube::test::if_sort_by_has_correct_order "${output_message}" "sorted-pod3:sorted-pod2:sorted-pod1:"

  # if sorting, we should be able to use any field in our objects
  output_message=$(kubectl get pods --sort-by="{spec.containers[0].name}")
  kube::test::if_sort_by_has_correct_order "${output_message}" "sorted-pod2:sorted-pod1:sorted-pod3:"

  # ensure sorting by creation timestamps works
  output_message=$(kubectl get pods --sort-by="{metadata.creationTimestamp}")
  kube::test::if_sort_by_has_correct_order "${output_message}" "sorted-pod1:sorted-pod2:sorted-pod3:"

  # ensure sorting by resource memory request works
  output_message=$(kubectl get pods --sort-by="{.spec.containers[0].resources.requests.memory}")
  kube::test::if_sort_by_has_correct_order "${output_message}" "sorted-pod3:sorted-pod1:sorted-pod2:"

  # ensure sorting by resource cpu request works
  output_message=$(kubectl get pods --sort-by="{.spec.containers[0].resources.requests.cpu}")
  kube::test::if_sort_by_has_correct_order "${output_message}" "sorted-pod3:sorted-pod1:sorted-pod2:"

  # ensure sorting by resource memory limit works
  output_message=$(kubectl get pods --sort-by="{.spec.containers[0].resources.limits.memory}")
  kube::test::if_sort_by_has_correct_order "${output_message}" "sorted-pod3:sorted-pod1:sorted-pod2:"

  # ensure sorting by resource cpu limit works
   output_message=$(kubectl get pods --sort-by="{.spec.containers[0].resources.limits.cpu}")
  kube::test::if_sort_by_has_correct_order "${output_message}" "sorted-pod3:sorted-pod1:sorted-pod2:"

  # ensure sorting using fallback codepath still works
  output_message=$(kubectl get pods --sort-by="{spec.containers[0].name}" --server-print=false --v=8 2>&1)
  kube::test::if_sort_by_has_correct_order "${output_message}" "sorted-pod2:sorted-pod1:sorted-pod3:"
  kube::test::if_has_not_string "${output_message}" "Table"

  ### Clean up
  # Pre-condition: valid-pod exists
  kube::test::get_object_assert pods "{{range.items}}{{$id_field}}:{{end}}" 'sorted-pod1:sorted-pod2:sorted-pod3:'
  # Command
  kubectl delete "${kube_flags[@]}" pod --grace-period=0 --force --all
  # Post-condition: valid-pod doesn't exist
  kube::test::get_object_assert pods "{{range.items}}{{$id_field}}:{{end}}" ''

  set +o nounset
  set +o errexit
}

run_kubectl_all_namespace_tests() {
  set -o nounset
  set -o errexit

  kube::log::status "Testing kubectl --all-namespace"

  # Pre-condition: the "default" namespace exists
  kube::test::get_object_assert namespaces "{{range.items}}{{if eq $id_field \"default\"}}{{$id_field}}:{{end}}{{end}}" 'default:'

  ### Create POD
  # Pre-condition: no POD exists
  kube::test::get_object_assert pods "{{range.items}}{{$id_field}}:{{end}}" ''
  # Command
  kubectl create "${kube_flags[@]}" -f test/fixtures/doc-yaml/admin/limitrange/valid-pod.yaml
  # Post-condition: valid-pod is created
  kube::test::get_object_assert pods "{{range.items}}{{$id_field}}:{{end}}" 'valid-pod:'

  ### Verify a specific namespace is ignored when all-namespaces is provided
  # Command
  kubectl get pods --all-namespaces --namespace=default

  ### Check --all-namespaces option shows namespaces
  # Create objects in multiple namespaces
  kubectl create "${kube_flags[@]}" namespace all-ns-test-1
  kubectl create "${kube_flags[@]}" serviceaccount test -n all-ns-test-1
  kubectl create "${kube_flags[@]}" namespace all-ns-test-2
  kubectl create "${kube_flags[@]}" serviceaccount test -n all-ns-test-2
  # Ensure listing across namespaces displays the namespace (--all-namespaces)
  output_message=$(kubectl get serviceaccounts --all-namespaces "${kube_flags[@]}")
  kube::test::if_has_string "${output_message}" "all-ns-test-1"
  kube::test::if_has_string "${output_message}" "all-ns-test-2"
  # Ensure listing across namespaces displays the namespace (-A)
  output_message=$(kubectl get serviceaccounts -A "${kube_flags[@]}")
  kube::test::if_has_string "${output_message}" "all-ns-test-1"
  kube::test::if_has_string "${output_message}" "all-ns-test-2"
  # Clean up
  kubectl delete "${kube_flags[@]}" namespace all-ns-test-1
  kubectl delete "${kube_flags[@]}" namespace all-ns-test-2

  ### Clean up
  # Pre-condition: valid-pod exists
  kube::test::get_object_assert pods "{{range.items}}{{$id_field}}:{{end}}" 'valid-pod:'
  # Command
  kubectl delete "${kube_flags[@]}" pod valid-pod --grace-period=0 --force
  # Post-condition: valid-pod doesn't exist
  kube::test::get_object_assert pods "{{range.items}}{{$id_field}}:{{end}}" ''

  ### Verify flag all-namespaces is ignored for rootScoped resources
  # Pre-condition: node exists
  kube::test::get_object_assert nodes "{{range.items}}{{$id_field}}:{{end}}" '127.0.0.1:'
  # Command
  output_message=$(kubectl get nodes --all-namespaces 2>&1)
  # Post-condition: output with no NAMESPACE field
  kube::test::if_has_not_string "${output_message}" "NAMESPACE"

  set +o nounset
  set +o errexit
}


run_deprecated_api_tests() {
  set -o nounset
  set -o errexit

  kube::log::status "Testing deprecated APIs"

  # Create deprecated CRD
  kubectl "${kube_flags_with_token[@]:?}" create -f - << __EOF__
{
  "kind": "CustomResourceDefinition",
  "apiVersion": "apiextensions.k8s.io/v1",
  "metadata": {
    "name": "deprecated.example.com"
  },
  "spec": {
    "group": "example.com",
    "scope": "Namespaced",
    "names": {
      "plural": "deprecated",
      "kind": "DeprecatedKind"
    },
    "versions": [
      {
        "name": "v1",
        "served": true,
        "storage": true,
        "schema": {
          "openAPIV3Schema": {
            "x-kubernetes-preserve-unknown-fields": true,
            "type": "object"
          }
        }
      },
      {
        "name": "v1beta1",
        "deprecated": true,
        "served": true,
        "storage": false,
        "schema": {
          "openAPIV3Schema": {
            "x-kubernetes-preserve-unknown-fields": true,
            "type": "object"
          }
        }
      }
    ]
  }
}
__EOF__

  # Ensure the API server has recognized and started serving the associated CR API
  local tries=5
  for i in $(seq 1 $tries); do
      local output
      output=$(kubectl "${kube_flags[@]:?}" api-resources --api-group example.com -oname || true)
      if kube::test::if_has_string "$output" deprecated.example.com; then
          break
      fi
      echo "${i}: Waiting for CR API to be available"
      sleep "$i"
  done

  # Test deprecated API request output
  output_message=$(kubectl get deprecated.v1beta1.example.com 2>&1 "${kube_flags[@]}")
  kube::test::if_has_string "${output_message}" 'example.com/v1beta1 DeprecatedKind is deprecated'
  output_message=$(! kubectl get deprecated.v1beta1.example.com --warnings-as-errors 2>&1 "${kube_flags[@]}")
  kube::test::if_has_string "${output_message}" 'example.com/v1beta1 DeprecatedKind is deprecated'
  kube::test::if_has_string "${output_message}" 'error: 1 warning received'

  # Delete deprecated CRD
  kubectl delete "${kube_flags[@]}" crd deprecated.example.com

  set +o nounset
  set +o errexit
}

run_kubectl_proxy_url_tests() {
  set -o nounset

  kube::log::status "Testing kubectl --proxy-url=socks5://127.0.0.1:12345"

  # Port 12345 is unreachable so the command will fail.
  output_message=$(kubectl --proxy-url=socks5://127.0.0.1:12345 get pods --v=9 2>&1 )

  set -o errexit

  kube::test::if_has_string "${output_message}" "Dial to tcp:127.0.0.1:12345"

  set +o nounset
  set +o errexit
}
