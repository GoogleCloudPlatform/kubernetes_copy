/*
Copyright 2016 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package testing

import (
	"math/rand"
	"reflect"
	"sort"
	"testing"

	"github.com/google/go-cmp/cmp"
	fuzz "github.com/google/gofuzz"

	apiv1 "k8s.io/api/core/v1"
	extensionsv1beta1 "k8s.io/api/extensions/v1beta1"
	"k8s.io/apimachinery/pkg/api/apitesting/roundtrip"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/kubernetes/pkg/api/legacyscheme"
)

type orderedGroupVersionKinds []schema.GroupVersionKind

func (o orderedGroupVersionKinds) Len() int      { return len(o) }
func (o orderedGroupVersionKinds) Swap(i, j int) { o[i], o[j] = o[j], o[i] }
func (o orderedGroupVersionKinds) Less(i, j int) bool {
	return o[i].String() < o[j].String()
}

// TODO: add a reflexive test that verifies that all SetDefaults functions are registered
func TestDefaulting(t *testing.T) {
	// these are the known types with defaulters - you must add to this list if you add a top level defaulter
	typesWithDefaulting := map[schema.GroupVersionKind]struct{}{
		{Group: "", Version: "v1", Kind: "ConfigMap"}:                                                              {},
		{Group: "", Version: "v1", Kind: "ConfigMapList"}:                                                          {},
		{Group: "", Version: "v1", Kind: "Endpoints"}:                                                              {},
		{Group: "", Version: "v1", Kind: "EndpointsList"}:                                                          {},
		{Group: "", Version: "v1", Kind: "EphemeralContainers"}:                                                    {},
		{Group: "", Version: "v1", Kind: "Namespace"}:                                                              {},
		{Group: "", Version: "v1", Kind: "NamespaceList"}:                                                          {},
		{Group: "", Version: "v1", Kind: "Node"}:                                                                   {},
		{Group: "", Version: "v1", Kind: "NodeList"}:                                                               {},
		{Group: "", Version: "v1", Kind: "PersistentVolume"}:                                                       {},
		{Group: "", Version: "v1", Kind: "PersistentVolumeList"}:                                                   {},
		{Group: "", Version: "v1", Kind: "PersistentVolumeClaim"}:                                                  {},
		{Group: "", Version: "v1", Kind: "PersistentVolumeClaimList"}:                                              {},
		{Group: "", Version: "v1", Kind: "Pod"}:                                                                    {},
		{Group: "", Version: "v1", Kind: "PodList"}:                                                                {},
		{Group: "", Version: "v1", Kind: "PodTemplate"}:                                                            {},
		{Group: "", Version: "v1", Kind: "PodTemplateList"}:                                                        {},
		{Group: "", Version: "v1", Kind: "ReplicationController"}:                                                  {},
		{Group: "", Version: "v1", Kind: "ReplicationControllerList"}:                                              {},
		{Group: "", Version: "v1", Kind: "Secret"}:                                                                 {},
		{Group: "", Version: "v1", Kind: "SecretList"}:                                                             {},
		{Group: "", Version: "v1", Kind: "Service"}:                                                                {},
		{Group: "", Version: "v1", Kind: "ServiceList"}:                                                            {},
		{Group: "apps", Version: "v1beta1", Kind: "StatefulSet"}:                                                   {},
		{Group: "apps", Version: "v1beta1", Kind: "StatefulSetList"}:                                               {},
		{Group: "apps", Version: "v1beta2", Kind: "StatefulSet"}:                                                   {},
		{Group: "apps", Version: "v1beta2", Kind: "StatefulSetList"}:                                               {},
		{Group: "apps", Version: "v1", Kind: "StatefulSet"}:                                                        {},
		{Group: "apps", Version: "v1", Kind: "StatefulSetList"}:                                                    {},
		{Group: "autoscaling", Version: "v1", Kind: "HorizontalPodAutoscaler"}:                                     {},
		{Group: "autoscaling", Version: "v1", Kind: "HorizontalPodAutoscalerList"}:                                 {},
		{Group: "autoscaling", Version: "v2", Kind: "HorizontalPodAutoscaler"}:                                     {},
		{Group: "autoscaling", Version: "v2", Kind: "HorizontalPodAutoscalerList"}:                                 {},
		{Group: "autoscaling", Version: "v2beta1", Kind: "HorizontalPodAutoscaler"}:                                {},
		{Group: "autoscaling", Version: "v2beta1", Kind: "HorizontalPodAutoscalerList"}:                            {},
		{Group: "autoscaling", Version: "v2beta2", Kind: "HorizontalPodAutoscaler"}:                                {},
		{Group: "autoscaling", Version: "v2beta2", Kind: "HorizontalPodAutoscalerList"}:                            {},
		{Group: "batch", Version: "v1", Kind: "CronJob"}:                                                           {},
		{Group: "batch", Version: "v1", Kind: "CronJobList"}:                                                       {},
		{Group: "batch", Version: "v1", Kind: "Job"}:                                                               {},
		{Group: "batch", Version: "v1", Kind: "JobList"}:                                                           {},
		{Group: "batch", Version: "v1beta1", Kind: "CronJob"}:                                                      {},
		{Group: "batch", Version: "v1beta1", Kind: "CronJobList"}:                                                  {},
		{Group: "batch", Version: "v1beta1", Kind: "JobTemplate"}:                                                  {},
		{Group: "batch", Version: "v2alpha1", Kind: "CronJob"}:                                                     {},
		{Group: "batch", Version: "v2alpha1", Kind: "CronJobList"}:                                                 {},
		{Group: "batch", Version: "v2alpha1", Kind: "JobTemplate"}:                                                 {},
		{Group: "certificates.k8s.io", Version: "v1beta1", Kind: "CertificateSigningRequest"}:                      {},
		{Group: "certificates.k8s.io", Version: "v1beta1", Kind: "CertificateSigningRequestList"}:                  {},
		{Group: "discovery.k8s.io", Version: "v1", Kind: "EndpointSlice"}:                                          {},
		{Group: "discovery.k8s.io", Version: "v1", Kind: "EndpointSliceList"}:                                      {},
		{Group: "discovery.k8s.io", Version: "v1beta1", Kind: "EndpointSlice"}:                                     {},
		{Group: "discovery.k8s.io", Version: "v1beta1", Kind: "EndpointSliceList"}:                                 {},
		{Group: "extensions", Version: "v1beta1", Kind: "DaemonSet"}:                                               {},
		{Group: "extensions", Version: "v1beta1", Kind: "DaemonSetList"}:                                           {},
		{Group: "apps", Version: "v1beta2", Kind: "DaemonSet"}:                                                     {},
		{Group: "apps", Version: "v1beta2", Kind: "DaemonSetList"}:                                                 {},
		{Group: "apps", Version: "v1", Kind: "DaemonSet"}:                                                          {},
		{Group: "apps", Version: "v1", Kind: "DaemonSetList"}:                                                      {},
		{Group: "extensions", Version: "v1beta1", Kind: "Deployment"}:                                              {},
		{Group: "extensions", Version: "v1beta1", Kind: "DeploymentList"}:                                          {},
		{Group: "apps", Version: "v1beta1", Kind: "Deployment"}:                                                    {},
		{Group: "apps", Version: "v1beta1", Kind: "DeploymentList"}:                                                {},
		{Group: "apps", Version: "v1beta2", Kind: "Deployment"}:                                                    {},
		{Group: "apps", Version: "v1beta2", Kind: "DeploymentList"}:                                                {},
		{Group: "apps", Version: "v1", Kind: "Deployment"}:                                                         {},
		{Group: "apps", Version: "v1", Kind: "DeploymentList"}:                                                     {},
		{Group: "extensions", Version: "v1beta1", Kind: "Ingress"}:                                                 {},
		{Group: "extensions", Version: "v1beta1", Kind: "IngressList"}:                                             {},
		{Group: "apps", Version: "v1beta2", Kind: "ReplicaSet"}:                                                    {},
		{Group: "apps", Version: "v1beta2", Kind: "ReplicaSetList"}:                                                {},
		{Group: "apps", Version: "v1", Kind: "ReplicaSet"}:                                                         {},
		{Group: "apps", Version: "v1", Kind: "ReplicaSetList"}:                                                     {},
		{Group: "extensions", Version: "v1beta1", Kind: "ReplicaSet"}:                                              {},
		{Group: "extensions", Version: "v1beta1", Kind: "ReplicaSetList"}:                                          {},
		{Group: "extensions", Version: "v1beta1", Kind: "NetworkPolicy"}:                                           {},
		{Group: "extensions", Version: "v1beta1", Kind: "NetworkPolicyList"}:                                       {},
		{Group: "rbac.authorization.k8s.io", Version: "v1alpha1", Kind: "ClusterRoleBinding"}:                      {},
		{Group: "rbac.authorization.k8s.io", Version: "v1alpha1", Kind: "ClusterRoleBindingList"}:                  {},
		{Group: "rbac.authorization.k8s.io", Version: "v1alpha1", Kind: "RoleBinding"}:                             {},
		{Group: "rbac.authorization.k8s.io", Version: "v1alpha1", Kind: "RoleBindingList"}:                         {},
		{Group: "rbac.authorization.k8s.io", Version: "v1beta1", Kind: "ClusterRoleBinding"}:                       {},
		{Group: "rbac.authorization.k8s.io", Version: "v1beta1", Kind: "ClusterRoleBindingList"}:                   {},
		{Group: "rbac.authorization.k8s.io", Version: "v1beta1", Kind: "RoleBinding"}:                              {},
		{Group: "rbac.authorization.k8s.io", Version: "v1beta1", Kind: "RoleBindingList"}:                          {},
		{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "ClusterRoleBinding"}:                            {},
		{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "ClusterRoleBindingList"}:                        {},
		{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "RoleBinding"}:                                   {},
		{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "RoleBindingList"}:                               {},
		{Group: "resource.k8s.io", Version: "v1alpha3", Kind: "ResourceClaim"}:                                     {},
		{Group: "resource.k8s.io", Version: "v1alpha3", Kind: "ResourceClaimList"}:                                 {},
		{Group: "resource.k8s.io", Version: "v1alpha3", Kind: "ResourceClaimTemplate"}:                             {},
		{Group: "resource.k8s.io", Version: "v1alpha3", Kind: "ResourceClaimTemplateList"}:                         {},
		{Group: "resource.k8s.io", Version: "v1beta1", Kind: "ResourceClaim"}:                                      {},
		{Group: "resource.k8s.io", Version: "v1beta1", Kind: "ResourceClaimList"}:                                  {},
		{Group: "resource.k8s.io", Version: "v1beta1", Kind: "ResourceClaimTemplate"}:                              {},
		{Group: "resource.k8s.io", Version: "v1beta1", Kind: "ResourceClaimTemplateList"}:                          {},
		{Group: "resource.k8s.io", Version: "v1beta2", Kind: "ResourceClaim"}:                                      {},
		{Group: "resource.k8s.io", Version: "v1beta2", Kind: "ResourceClaimList"}:                                  {},
		{Group: "resource.k8s.io", Version: "v1beta2", Kind: "ResourceClaimTemplate"}:                              {},
		{Group: "resource.k8s.io", Version: "v1beta2", Kind: "ResourceClaimTemplateList"}:                          {},
		{Group: "admissionregistration.k8s.io", Version: "v1alpha1", Kind: "ValidatingAdmissionPolicy"}:            {},
		{Group: "admissionregistration.k8s.io", Version: "v1alpha1", Kind: "ValidatingAdmissionPolicyList"}:        {},
		{Group: "admissionregistration.k8s.io", Version: "v1alpha1", Kind: "ValidatingAdmissionPolicyBinding"}:     {},
		{Group: "admissionregistration.k8s.io", Version: "v1alpha1", Kind: "ValidatingAdmissionPolicyBindingList"}: {},
		{Group: "admissionregistration.k8s.io", Version: "v1alpha1", Kind: "MutatingAdmissionPolicy"}:              {},
		{Group: "admissionregistration.k8s.io", Version: "v1alpha1", Kind: "MutatingAdmissionPolicyList"}:          {},
		{Group: "admissionregistration.k8s.io", Version: "v1alpha1", Kind: "MutatingAdmissionPolicyBinding"}:       {},
		{Group: "admissionregistration.k8s.io", Version: "v1alpha1", Kind: "MutatingAdmissionPolicyBindingList"}:   {},
		{Group: "admissionregistration.k8s.io", Version: "v1beta1", Kind: "ValidatingWebhookConfiguration"}:        {},
		{Group: "admissionregistration.k8s.io", Version: "v1beta1", Kind: "ValidatingWebhookConfigurationList"}:    {},
		{Group: "admissionregistration.k8s.io", Version: "v1beta1", Kind: "MutatingWebhookConfiguration"}:          {},
		{Group: "admissionregistration.k8s.io", Version: "v1beta1", Kind: "MutatingWebhookConfigurationList"}:      {},
		{Group: "admissionregistration.k8s.io", Version: "v1beta1", Kind: "ValidatingAdmissionPolicy"}:             {},
		{Group: "admissionregistration.k8s.io", Version: "v1beta1", Kind: "ValidatingAdmissionPolicyList"}:         {},
		{Group: "admissionregistration.k8s.io", Version: "v1beta1", Kind: "ValidatingAdmissionPolicyBinding"}:      {},
		{Group: "admissionregistration.k8s.io", Version: "v1beta1", Kind: "ValidatingAdmissionPolicyBindingList"}:  {},
		{Group: "admissionregistration.k8s.io", Version: "v1", Kind: "ValidatingAdmissionPolicy"}:                  {},
		{Group: "admissionregistration.k8s.io", Version: "v1", Kind: "ValidatingAdmissionPolicyList"}:              {},
		{Group: "admissionregistration.k8s.io", Version: "v1", Kind: "ValidatingAdmissionPolicyBinding"}:           {},
		{Group: "admissionregistration.k8s.io", Version: "v1", Kind: "ValidatingAdmissionPolicyBindingList"}:       {},
		{Group: "admissionregistration.k8s.io", Version: "v1", Kind: "ValidatingWebhookConfiguration"}:             {},
		{Group: "admissionregistration.k8s.io", Version: "v1", Kind: "ValidatingWebhookConfigurationList"}:         {},
		{Group: "admissionregistration.k8s.io", Version: "v1", Kind: "MutatingWebhookConfiguration"}:               {},
		{Group: "admissionregistration.k8s.io", Version: "v1", Kind: "MutatingWebhookConfigurationList"}:           {},
		{Group: "networking.k8s.io", Version: "v1", Kind: "NetworkPolicy"}:                                         {},
		{Group: "networking.k8s.io", Version: "v1", Kind: "NetworkPolicyList"}:                                     {},
		{Group: "networking.k8s.io", Version: "v1beta1", Kind: "Ingress"}:                                          {},
		{Group: "networking.k8s.io", Version: "v1beta1", Kind: "IngressList"}:                                      {},
		{Group: "networking.k8s.io", Version: "v1", Kind: "IngressClass"}:                                          {},
		{Group: "networking.k8s.io", Version: "v1", Kind: "IngressClassList"}:                                      {},
		{Group: "storage.k8s.io", Version: "v1beta1", Kind: "StorageClass"}:                                        {},
		{Group: "storage.k8s.io", Version: "v1beta1", Kind: "StorageClassList"}:                                    {},
		{Group: "storage.k8s.io", Version: "v1beta1", Kind: "CSIDriver"}:                                           {},
		{Group: "storage.k8s.io", Version: "v1beta1", Kind: "CSIDriverList"}:                                       {},
		{Group: "storage.k8s.io", Version: "v1", Kind: "StorageClass"}:                                             {},
		{Group: "storage.k8s.io", Version: "v1", Kind: "StorageClassList"}:                                         {},
		{Group: "storage.k8s.io", Version: "v1", Kind: "VolumeAttachment"}:                                         {},
		{Group: "storage.k8s.io", Version: "v1", Kind: "VolumeAttachmentList"}:                                     {},
		{Group: "storage.k8s.io", Version: "v1", Kind: "CSIDriver"}:                                                {},
		{Group: "storage.k8s.io", Version: "v1", Kind: "CSIDriverList"}:                                            {},
		{Group: "storage.k8s.io", Version: "v1beta1", Kind: "VolumeAttachment"}:                                    {},
		{Group: "storage.k8s.io", Version: "v1beta1", Kind: "VolumeAttachmentList"}:                                {},
		{Group: "authentication.k8s.io", Version: "v1", Kind: "TokenRequest"}:                                      {},
		{Group: "scheduling.k8s.io", Version: "v1alpha1", Kind: "PriorityClass"}:                                   {},
		{Group: "scheduling.k8s.io", Version: "v1beta1", Kind: "PriorityClass"}:                                    {},
		{Group: "scheduling.k8s.io", Version: "v1", Kind: "PriorityClass"}:                                         {},
		{Group: "scheduling.k8s.io", Version: "v1alpha1", Kind: "PriorityClassList"}:                               {},
		{Group: "scheduling.k8s.io", Version: "v1beta1", Kind: "PriorityClassList"}:                                {},
		{Group: "scheduling.k8s.io", Version: "v1", Kind: "PriorityClassList"}:                                     {},
		{Group: "flowcontrol.apiserver.k8s.io", Version: "v1alpha1", Kind: "PriorityLevelConfiguration"}:           {},
		{Group: "flowcontrol.apiserver.k8s.io", Version: "v1alpha1", Kind: "PriorityLevelConfigurationList"}:       {},
		{Group: "flowcontrol.apiserver.k8s.io", Version: "v1beta1", Kind: "PriorityLevelConfiguration"}:            {},
		{Group: "flowcontrol.apiserver.k8s.io", Version: "v1beta1", Kind: "PriorityLevelConfigurationList"}:        {},
		{Group: "flowcontrol.apiserver.k8s.io", Version: "v1beta2", Kind: "PriorityLevelConfiguration"}:            {},
		{Group: "flowcontrol.apiserver.k8s.io", Version: "v1beta2", Kind: "PriorityLevelConfigurationList"}:        {},
		{Group: "flowcontrol.apiserver.k8s.io", Version: "v1beta3", Kind: "PriorityLevelConfiguration"}:            {},
		{Group: "flowcontrol.apiserver.k8s.io", Version: "v1beta3", Kind: "PriorityLevelConfigurationList"}:        {},
		{Group: "flowcontrol.apiserver.k8s.io", Version: "v1", Kind: "PriorityLevelConfiguration"}:                 {},
		{Group: "flowcontrol.apiserver.k8s.io", Version: "v1", Kind: "PriorityLevelConfigurationList"}:             {},
	}

	scheme := legacyscheme.Scheme
	var testTypes orderedGroupVersionKinds
	for gvk := range scheme.AllKnownTypes() {
		if gvk.Version == runtime.APIVersionInternal {
			continue
		}
		testTypes = append(testTypes, gvk)
	}
	sort.Sort(testTypes)

	for _, gvk := range testTypes {
		gvk := gvk
		t.Run(gvk.String(), func(t *testing.T) {
			// Each sub-tests gets its own fuzzer instance to make running it independent
			// from what other tests ran before.
			f := fuzz.New().NilChance(.5).NumElements(1, 1).RandSource(rand.NewSource(1))
			f.Funcs(
				func(s *runtime.RawExtension, c fuzz.Continue) {},
				func(s *metav1.LabelSelector, c fuzz.Continue) {
					c.FuzzNoCustom(s)
					s.MatchExpressions = nil // need to fuzz this specially
				},
				func(s *metav1.ListOptions, c fuzz.Continue) {
					c.FuzzNoCustom(s)
					s.LabelSelector = "" // need to fuzz requirement strings specially
					s.FieldSelector = "" // need to fuzz requirement strings specially
				},
				func(s *extensionsv1beta1.ScaleStatus, c fuzz.Continue) {
					c.FuzzNoCustom(s)
					s.TargetSelector = "" // need to fuzz requirement strings specially
				},
			)

			_, expectedChanged := typesWithDefaulting[gvk]
			iter := 0
			changedOnce := false
			for {
				if iter > *roundtrip.FuzzIters {
					if !expectedChanged || changedOnce {
						break
					}
					// This uses to be 300, but for ResourceClaimList that was not high enough
					// because depending on the starting conditions, the fuzzer never created the
					// one combination where defaulting kicked in (empty string in non-empty slice
					// in another non-empty slice).
					if iter > 3000 {
						t.Errorf("expected %s to trigger defaulting due to fuzzing", gvk)
						break
					}
					// if we expected defaulting, continue looping until the fuzzer gives us one
					// at worst, we will timeout
				}
				iter++

				src, err := scheme.New(gvk)
				if err != nil {
					t.Fatal(err)
				}
				f.Fuzz(src)

				src.GetObjectKind().SetGroupVersionKind(schema.GroupVersionKind{})

				original := src.DeepCopyObject()

				// get internal
				withDefaults := src.DeepCopyObject()
				scheme.Default(withDefaults)

				if !reflect.DeepEqual(original, withDefaults) {
					diff := cmp.Diff(original, withDefaults)
					if !changedOnce {
						t.Logf("got diff (-fuzzed, +with defaults):\n%s", diff)
						changedOnce = true
					}
					if !expectedChanged {
						t.Errorf("{Group: \"%s\", Version: \"%s\", Kind: \"%s\"} did not expect defaults to be set - update expected or check defaulter registering: %s", gvk.Group, gvk.Version, gvk.Kind, diff)
					}
				}
			}
		})
	}

}

func BenchmarkPodDefaulting(b *testing.B) {
	f := fuzz.New().NilChance(.5).NumElements(1, 1).RandSource(rand.NewSource(1))
	items := make([]apiv1.Pod, 100)
	for i := range items {
		f.Fuzz(&items[i])
	}

	scheme := legacyscheme.Scheme
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		pod := &items[i%len(items)]

		scheme.Default(pod)
	}
	b.StopTimer()
}
