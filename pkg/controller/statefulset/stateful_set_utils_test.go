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

package statefulset

import (
	"fmt"
	"math/rand"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	apps "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/klog/v2"
	"k8s.io/klog/v2/ktesting"
	podutil "k8s.io/kubernetes/pkg/api/v1/pod"
	"k8s.io/kubernetes/pkg/controller/history"
	"k8s.io/utils/ptr"
)

// noopRecorder is an EventRecorder that does nothing. record.FakeRecorder has a fixed
// buffer size, which causes tests to hang if that buffer's exceeded.
type noopRecorder struct{}

func (r *noopRecorder) Event(object runtime.Object, eventtype, reason, message string) {}
func (r *noopRecorder) Eventf(object runtime.Object, eventtype, reason, messageFmt string, args ...interface{}) {
}
func (r *noopRecorder) AnnotatedEventf(object runtime.Object, annotations map[string]string, eventtype, reason, messageFmt string, args ...interface{}) {
}

// getClaimPodName gets the name of the Pod associated with the Claim, or an empty string if this doesn't look matching.
func getClaimPodName(set *apps.StatefulSet, claim *v1.PersistentVolumeClaim) string {
	podName := ""

	statefulClaimRegex := regexp.MustCompile(fmt.Sprintf(".*-(%s-[0-9]+)$", set.Name))
	matches := statefulClaimRegex.FindStringSubmatch(claim.Name)
	if len(matches) != 2 {
		return podName
	}
	return matches[1]
}

// ownerRefsChanged returns true if newRefs does not match originalRefs.
func ownerRefsChanged(originalRefs, newRefs []metav1.OwnerReference) bool {
	if len(originalRefs) != len(newRefs) {
		return true
	}
	key := func(ref *metav1.OwnerReference) string {
		return fmt.Sprintf("%s-%s-%s", ref.APIVersion, ref.Kind, ref.Name)
	}
	refs := map[string]bool{}
	for i := range originalRefs {
		refs[key(&originalRefs[i])] = true
	}
	for i := range newRefs {
		k := key(&newRefs[i])
		if val, found := refs[k]; !found || !val {
			return true
		}
		refs[k] = false
	}
	return false
}

func TestOwnerRefsChanged(t *testing.T) {
	toRefs := func(strs []string) []metav1.OwnerReference {
		refs := []metav1.OwnerReference{}
		for _, s := range strs {
			pieces := strings.Split(s, "/")
			refs = append(refs, metav1.OwnerReference{
				APIVersion: pieces[0],
				Kind:       pieces[1],
				Name:       pieces[2],
			})
		}
		return refs
	}
	testCases := []struct {
		orig, new []string
		changed   bool
	}{
		{
			orig:    []string{"v1/pod/foo"},
			new:     []string{},
			changed: true,
		},
		{
			orig:    []string{"v1/pod/foo"},
			new:     []string{"v1/pod/foo"},
			changed: false,
		},
		{
			orig:    []string{"v1/pod/foo"},
			new:     []string{"v1/pod/bar"},
			changed: true,
		},
		{
			orig:    []string{"v1/pod/foo", "v1/set/bob"},
			new:     []string{"v1/pod/foo", "v1/set/alice"},
			changed: true,
		},
		{
			orig:    []string{"v1/pod/foo", "v1/set/bob"},
			new:     []string{"v1/pod/foo", "v1/set/bob"},
			changed: false,
		},
		{
			orig:    []string{"v1/pod/foo", "v1/set/bob"},
			new:     []string{"v1/pod/foo", "v1/set/bob", "v1/set/bob"},
			changed: true,
		},
		{
			orig:    []string{"v1/pod/foo", "v1/set/bob"},
			new:     []string{"v1/set/bob", "v1/pod/foo"},
			changed: false,
		},
	}
	for _, tc := range testCases {
		if ownerRefsChanged(toRefs(tc.orig), toRefs(tc.new)) != tc.changed {
			t.Errorf("Expected change=%t but got %t for %v vs %v", tc.changed, !tc.changed, tc.orig, tc.new)
		}
	}
}

func TestGetParentNameAndOrdinal(t *testing.T) {
	set := newStatefulSet(3)
	pod := newStatefulSetPod(set, 1)
	if parent, ordinal := getParentNameAndOrdinal(pod); parent != set.Name {
		t.Errorf("Extracted the wrong parent name expected %s found %s", set.Name, parent)
	} else if ordinal != 1 {
		t.Errorf("Extracted the wrong ordinal expected %d found %d", 1, ordinal)
	}
	pod.Name = "1-bar"
	if parent, ordinal := getParentNameAndOrdinal(pod); parent != "" {
		t.Error("Expected empty string for non-member Pod parent")
	} else if ordinal != -1 {
		t.Error("Expected -1 for non member Pod ordinal")
	}
}

func TestGetClaimPodName(t *testing.T) {
	set := apps.StatefulSet{}
	set.Name = "my-set"
	claim := v1.PersistentVolumeClaim{}
	claim.Name = "volume-my-set-2"
	if pod := getClaimPodName(&set, &claim); pod != "my-set-2" {
		t.Errorf("Expected my-set-2 found %s", pod)
	}
	claim.Name = "long-volume-my-set-20"
	if pod := getClaimPodName(&set, &claim); pod != "my-set-20" {
		t.Errorf("Expected my-set-20 found %s", pod)
	}
	claim.Name = "volume-2-my-set"
	if pod := getClaimPodName(&set, &claim); pod != "" {
		t.Errorf("Expected empty string found %s", pod)
	}
	claim.Name = "volume-pod-2"
	if pod := getClaimPodName(&set, &claim); pod != "" {
		t.Errorf("Expected empty string found %s", pod)
	}
}

func TestIsMemberOf(t *testing.T) {
	set := newStatefulSet(3)
	set2 := newStatefulSet(3)
	set2.Name = "foo2"
	pod := newStatefulSetPod(set, 1)
	if !isMemberOf(set, pod) {
		t.Error("isMemberOf returned false negative")
	}
	if isMemberOf(set2, pod) {
		t.Error("isMemberOf returned false positive")
	}
}

func TestIdentityMatches(t *testing.T) {
	set := newStatefulSet(3)
	pod := newStatefulSetPod(set, 1)
	if !identityMatches(set, pod) {
		t.Error("Newly created Pod has a bad identity")
	}
	pod.Name = "foo"
	if identityMatches(set, pod) {
		t.Error("identity matches for a Pod with the wrong name")
	}
	pod = newStatefulSetPod(set, 1)
	pod.Namespace = ""
	if identityMatches(set, pod) {
		t.Error("identity matches for a Pod with the wrong namespace")
	}
	pod = newStatefulSetPod(set, 1)
	delete(pod.Labels, apps.StatefulSetPodNameLabel)
	if identityMatches(set, pod) {
		t.Error("identity matches for a Pod with the wrong statefulSetPodNameLabel")
	}
}

func TestStorageMatches(t *testing.T) {
	set := newStatefulSet(3)
	pod := newStatefulSetPod(set, 1)
	if !storageMatches(set, pod) {
		t.Error("Newly created Pod has a invalid storage")
	}
	pod.Spec.Volumes = nil
	if storageMatches(set, pod) {
		t.Error("Pod with invalid Volumes has valid storage")
	}
	pod = newStatefulSetPod(set, 1)
	for i := range pod.Spec.Volumes {
		pod.Spec.Volumes[i].PersistentVolumeClaim = nil
	}
	if storageMatches(set, pod) {
		t.Error("Pod with invalid Volumes claim valid storage")
	}
	pod = newStatefulSetPod(set, 1)
	for i := range pod.Spec.Volumes {
		if pod.Spec.Volumes[i].PersistentVolumeClaim != nil {
			pod.Spec.Volumes[i].PersistentVolumeClaim.ClaimName = "foo"
		}
	}
	if storageMatches(set, pod) {
		t.Error("Pod with invalid Volumes claim valid storage")
	}
	pod = newStatefulSetPod(set, 1)
	pod.Name = "bar"
	if storageMatches(set, pod) {
		t.Error("Pod with invalid ordinal has valid storage")
	}
}

func TestUpdateIdentity(t *testing.T) {
	set := newStatefulSet(3)
	pod := newStatefulSetPod(set, 1)
	if !identityMatches(set, pod) {
		t.Error("Newly created Pod has a bad identity")
	}
	pod.Namespace = ""
	if identityMatches(set, pod) {
		t.Error("identity matches for a Pod with the wrong namespace")
	}
	updateIdentity(set, pod)
	if !identityMatches(set, pod) {
		t.Error("updateIdentity failed to update the Pods namespace")
	}
	delete(pod.Labels, apps.StatefulSetPodNameLabel)
	updateIdentity(set, pod)
	if !identityMatches(set, pod) {
		t.Error("updateIdentity failed to restore the statefulSetPodName label")
	}
}

func TestUpdateStorage(t *testing.T) {
	set := newStatefulSet(3)
	pod := newStatefulSetPod(set, 1)
	if !storageMatches(set, pod) {
		t.Error("Newly created Pod has a invalid storage")
	}
	pod.Spec.Volumes = nil
	if storageMatches(set, pod) {
		t.Error("Pod with invalid Volumes has valid storage")
	}
	updateStorage(set, pod)
	if !storageMatches(set, pod) {
		t.Error("updateStorage failed to recreate volumes")
	}
	pod = newStatefulSetPod(set, 1)
	for i := range pod.Spec.Volumes {
		pod.Spec.Volumes[i].PersistentVolumeClaim = nil
	}
	if storageMatches(set, pod) {
		t.Error("Pod with invalid Volumes claim valid storage")
	}
	updateStorage(set, pod)
	if !storageMatches(set, pod) {
		t.Error("updateStorage failed to recreate volume claims")
	}
	pod = newStatefulSetPod(set, 1)
	for i := range pod.Spec.Volumes {
		if pod.Spec.Volumes[i].PersistentVolumeClaim != nil {
			pod.Spec.Volumes[i].PersistentVolumeClaim.ClaimName = "foo"
		}
	}
	if storageMatches(set, pod) {
		t.Error("Pod with invalid Volumes claim valid storage")
	}
	updateStorage(set, pod)
	if !storageMatches(set, pod) {
		t.Error("updateStorage failed to recreate volume claim names")
	}
}

func TestGetPersistentVolumeClaimRetentionPolicy(t *testing.T) {
	retainPolicy := apps.StatefulSetPersistentVolumeClaimRetentionPolicy{
		WhenScaled:  apps.RetainPersistentVolumeClaimRetentionPolicyType,
		WhenDeleted: apps.RetainPersistentVolumeClaimRetentionPolicyType,
	}
	scaledownPolicy := apps.StatefulSetPersistentVolumeClaimRetentionPolicy{
		WhenScaled:  apps.DeletePersistentVolumeClaimRetentionPolicyType,
		WhenDeleted: apps.RetainPersistentVolumeClaimRetentionPolicyType,
	}

	set := apps.StatefulSet{}
	set.Spec.PersistentVolumeClaimRetentionPolicy = &retainPolicy
	got := getPersistentVolumeClaimRetentionPolicy(&set)
	if got.WhenScaled != apps.RetainPersistentVolumeClaimRetentionPolicyType || got.WhenDeleted != apps.RetainPersistentVolumeClaimRetentionPolicyType {
		t.Errorf("Expected retain policy")
	}
	set.Spec.PersistentVolumeClaimRetentionPolicy = &scaledownPolicy
	got = getPersistentVolumeClaimRetentionPolicy(&set)
	if got.WhenScaled != apps.DeletePersistentVolumeClaimRetentionPolicyType || got.WhenDeleted != apps.RetainPersistentVolumeClaimRetentionPolicyType {
		t.Errorf("Expected scaledown policy")
	}
}

func TestMatchesRef(t *testing.T) {
	testCases := []struct {
		name        string
		ref         metav1.OwnerReference
		obj         metav1.ObjectMeta
		schema      schema.GroupVersionKind
		shouldMatch bool
	}{
		{
			name: "full match",
			ref: metav1.OwnerReference{
				APIVersion: "v1",
				Kind:       "Pod",
				Name:       "fred",
				UID:        "abc",
			},
			obj: metav1.ObjectMeta{
				Name: "fred",
				UID:  "abc",
			},
			schema:      podKind,
			shouldMatch: true,
		},
		{
			name: "match without UID",
			ref: metav1.OwnerReference{
				APIVersion: "v1",
				Kind:       "Pod",
				Name:       "fred",
				UID:        "abc",
			},
			obj: metav1.ObjectMeta{
				Name: "fred",
				UID:  "not-matching",
			},
			schema:      podKind,
			shouldMatch: true,
		},
		{
			name: "mismatch name",
			ref: metav1.OwnerReference{
				APIVersion: "v1",
				Kind:       "Pod",
				Name:       "fred",
				UID:        "abc",
			},
			obj: metav1.ObjectMeta{
				Name: "joan",
				UID:  "abc",
			},
			schema:      podKind,
			shouldMatch: false,
		},
		{
			name: "wrong schema",
			ref: metav1.OwnerReference{
				APIVersion: "beta2",
				Kind:       "Pod",
				Name:       "fred",
				UID:        "abc",
			},
			obj: metav1.ObjectMeta{
				Name: "fred",
				UID:  "abc",
			},
			schema:      podKind,
			shouldMatch: false,
		},
	}
	for _, tc := range testCases {
		got := matchesRef(&tc.ref, &tc.obj, tc.schema)
		if got != tc.shouldMatch {
			t.Errorf("Failed %s: got %t, expected %t", tc.name, got, tc.shouldMatch)
		}
	}
}

func TestIsClaimOwnerUpToDate(t *testing.T) {
	testCases := []struct {
		name            string
		scaleDownPolicy apps.PersistentVolumeClaimRetentionPolicyType
		setDeletePolicy apps.PersistentVolumeClaimRetentionPolicyType
		needsPodRef     bool
		needsSetRef     bool
		replicas        int32
		ordinal         int
	}{
		{
			name:            "retain",
			scaleDownPolicy: apps.RetainPersistentVolumeClaimRetentionPolicyType,
			setDeletePolicy: apps.RetainPersistentVolumeClaimRetentionPolicyType,
			needsPodRef:     false,
			needsSetRef:     false,
		},
		{
			name:            "on SS delete",
			scaleDownPolicy: apps.RetainPersistentVolumeClaimRetentionPolicyType,
			setDeletePolicy: apps.DeletePersistentVolumeClaimRetentionPolicyType,
			needsPodRef:     false,
			needsSetRef:     true,
		},
		{
			name:            "on scaledown only, condemned",
			scaleDownPolicy: apps.DeletePersistentVolumeClaimRetentionPolicyType,
			setDeletePolicy: apps.RetainPersistentVolumeClaimRetentionPolicyType,
			needsPodRef:     true,
			needsSetRef:     false,
			replicas:        2,
			ordinal:         2,
		},
		{
			name:            "on scaledown only, remains",
			scaleDownPolicy: apps.DeletePersistentVolumeClaimRetentionPolicyType,
			setDeletePolicy: apps.RetainPersistentVolumeClaimRetentionPolicyType,
			needsPodRef:     false,
			needsSetRef:     false,
			replicas:        2,
			ordinal:         1,
		},
		{
			name:            "on both, condemned",
			scaleDownPolicy: apps.DeletePersistentVolumeClaimRetentionPolicyType,
			setDeletePolicy: apps.DeletePersistentVolumeClaimRetentionPolicyType,
			needsPodRef:     true,
			needsSetRef:     false,
			replicas:        2,
			ordinal:         2,
		},
		{
			name:            "on both, remains",
			scaleDownPolicy: apps.DeletePersistentVolumeClaimRetentionPolicyType,
			setDeletePolicy: apps.DeletePersistentVolumeClaimRetentionPolicyType,
			needsPodRef:     false,
			needsSetRef:     true,
			replicas:        2,
			ordinal:         1,
		},
	}

	for _, tc := range testCases {
		for _, useOtherRefs := range []bool{false, true} {
			for _, setPodRef := range []bool{false, true} {
				for _, setSetRef := range []bool{false, true} {
					_, ctx := ktesting.NewTestContext(t)
					logger := klog.FromContext(ctx)
					claim := v1.PersistentVolumeClaim{}
					claim.Name = "target-claim"
					pod := v1.Pod{}
					pod.Name = fmt.Sprintf("pod-%d", tc.ordinal)
					pod.GetObjectMeta().SetUID("pod-123")
					set := apps.StatefulSet{}
					set.Name = "stateful-set"
					set.GetObjectMeta().SetUID("ss-456")
					set.Spec.PersistentVolumeClaimRetentionPolicy = &apps.StatefulSetPersistentVolumeClaimRetentionPolicy{
						WhenScaled:  tc.scaleDownPolicy,
						WhenDeleted: tc.setDeletePolicy,
					}
					set.Spec.Replicas = &tc.replicas
					claimRefs := claim.GetOwnerReferences()
					if setPodRef {
						claimRefs = addControllerRef(claimRefs, &pod, podKind)
					}
					if setSetRef {
						claimRefs = addControllerRef(claimRefs, &set, controllerKind)
					}
					if useOtherRefs {
						claimRefs = append(
							claimRefs,
							metav1.OwnerReference{
								Name:       "rand1",
								APIVersion: "v1",
								Kind:       "Pod",
								UID:        "rand1-uid",
							},
							metav1.OwnerReference{
								Name:       "rand2",
								APIVersion: "v1",
								Kind:       "Pod",
								UID:        "rand2-uid",
							})
					}
					claim.SetOwnerReferences(claimRefs)
					shouldMatch := setPodRef == tc.needsPodRef && setSetRef == tc.needsSetRef
					if isClaimOwnerUpToDate(logger, &claim, &set, &pod) != shouldMatch {
						t.Errorf("Bad match for %s with pod=%v,set=%v,others=%v", tc.name, setPodRef, setSetRef, useOtherRefs)
					}
				}
			}
		}
	}
}

func TestClaimOwnerUpToDateEdgeCases(t *testing.T) {
	_, ctx := ktesting.NewTestContext(t)
	logger := klog.FromContext(ctx)

	testCases := []struct {
		name        string
		ownerRefs   []metav1.OwnerReference
		policy      apps.StatefulSetPersistentVolumeClaimRetentionPolicy
		shouldMatch bool
	}{
		{
			name: "normal controller, pod",
			ownerRefs: []metav1.OwnerReference{
				{
					Name:       "pod-1",
					APIVersion: "v1",
					Kind:       "Pod",
					UID:        "pod-123",
					Controller: ptr.To(true),
				},
			},
			policy: apps.StatefulSetPersistentVolumeClaimRetentionPolicy{
				WhenScaled:  apps.DeletePersistentVolumeClaimRetentionPolicyType,
				WhenDeleted: apps.DeletePersistentVolumeClaimRetentionPolicyType,
			},
			shouldMatch: true,
		},
		{
			name: "non-controller causes policy mismatch, pod",
			ownerRefs: []metav1.OwnerReference{
				{
					Name:       "pod-1",
					APIVersion: "v1",
					Kind:       "Pod",
					UID:        "pod-123",
				},
			},
			policy: apps.StatefulSetPersistentVolumeClaimRetentionPolicy{
				WhenScaled:  apps.DeletePersistentVolumeClaimRetentionPolicyType,
				WhenDeleted: apps.DeletePersistentVolumeClaimRetentionPolicyType,
			},
			shouldMatch: false,
		},
		{
			name: "stale controller does not affect policy, pod",
			ownerRefs: []metav1.OwnerReference{
				{
					Name:       "pod-1",
					APIVersion: "v1",
					Kind:       "Pod",
					UID:        "pod-stale",
					Controller: ptr.To(true),
				},
			},
			policy: apps.StatefulSetPersistentVolumeClaimRetentionPolicy{
				WhenScaled:  apps.DeletePersistentVolumeClaimRetentionPolicyType,
				WhenDeleted: apps.DeletePersistentVolumeClaimRetentionPolicyType,
			},
			shouldMatch: true,
		},
		{
			name: "unexpected controller causes policy mismatch, pod",
			ownerRefs: []metav1.OwnerReference{
				{
					Name:       "pod-1",
					APIVersion: "v1",
					Kind:       "Pod",
					UID:        "pod-123",
					Controller: ptr.To(true),
				},
				{
					Name:       "Random",
					APIVersion: "v1",
					Kind:       "Pod",
					UID:        "random",
					Controller: ptr.To(true),
				},
			},
			policy: apps.StatefulSetPersistentVolumeClaimRetentionPolicy{
				WhenScaled:  apps.DeletePersistentVolumeClaimRetentionPolicyType,
				WhenDeleted: apps.DeletePersistentVolumeClaimRetentionPolicyType,
			},
			shouldMatch: false,
		},
		{
			name: "normal controller, set",
			ownerRefs: []metav1.OwnerReference{
				{
					Name:       "stateful-set",
					APIVersion: "apps/v1",
					Kind:       "StatefulSet",
					UID:        "ss-456",
					Controller: ptr.To(true),
				},
			},
			policy: apps.StatefulSetPersistentVolumeClaimRetentionPolicy{
				WhenScaled:  apps.RetainPersistentVolumeClaimRetentionPolicyType,
				WhenDeleted: apps.DeletePersistentVolumeClaimRetentionPolicyType,
			},
			shouldMatch: true,
		},
		{
			name: "non-controller causes policy mismatch, set",
			ownerRefs: []metav1.OwnerReference{
				{
					Name:       "stateful-set",
					APIVersion: "appsv1",
					Kind:       "StatefulSet",
					UID:        "ss-456",
				},
			},
			policy: apps.StatefulSetPersistentVolumeClaimRetentionPolicy{
				WhenScaled:  apps.RetainPersistentVolumeClaimRetentionPolicyType,
				WhenDeleted: apps.DeletePersistentVolumeClaimRetentionPolicyType,
			},
			shouldMatch: false,
		},
		{
			name: "stale controller ignored, set",
			ownerRefs: []metav1.OwnerReference{
				{
					Name:       "stateful-set",
					APIVersion: "apps/v1",
					Kind:       "StatefulSet",
					UID:        "set-stale",
					Controller: ptr.To(true),
				},
			},
			policy: apps.StatefulSetPersistentVolumeClaimRetentionPolicy{
				WhenScaled:  apps.RetainPersistentVolumeClaimRetentionPolicyType,
				WhenDeleted: apps.DeletePersistentVolumeClaimRetentionPolicyType,
			},
			shouldMatch: true,
		},
		{
			name: "unexpected controller causes policy mismatch, set",
			ownerRefs: []metav1.OwnerReference{
				{
					Name:       "stateful-set",
					APIVersion: "apps/v1",
					Kind:       "StatefulSet",
					UID:        "ss-456",
					Controller: ptr.To(true),
				},
				{
					Name:       "Random",
					APIVersion: "apps/v1",
					Kind:       "StatefulSet",
					UID:        "random",
					Controller: ptr.To(true),
				},
			},
			policy: apps.StatefulSetPersistentVolumeClaimRetentionPolicy{
				WhenScaled:  apps.RetainPersistentVolumeClaimRetentionPolicyType,
				WhenDeleted: apps.DeletePersistentVolumeClaimRetentionPolicyType,
			},
			shouldMatch: false,
		},
	}

	for _, tc := range testCases {
		claim := v1.PersistentVolumeClaim{}
		claim.Name = "target-claim"
		pod := v1.Pod{}
		pod.Name = "pod-1"
		pod.GetObjectMeta().SetUID("pod-123")
		set := apps.StatefulSet{}
		set.Name = "stateful-set"
		set.GetObjectMeta().SetUID("ss-456")
		set.Spec.PersistentVolumeClaimRetentionPolicy = &tc.policy
		set.Spec.Replicas = ptr.To(int32(1))
		claim.SetOwnerReferences(tc.ownerRefs)
		got := isClaimOwnerUpToDate(logger, &claim, &set, &pod)
		if got != tc.shouldMatch {
			t.Errorf("Unexpected match for %s, got %t expected %t", tc.name, got, tc.shouldMatch)
		}
	}
}

func TestUpdateClaimOwnerRefForSetAndPod(t *testing.T) {
	testCases := []struct {
		name                 string
		scaleDownPolicy      apps.PersistentVolumeClaimRetentionPolicyType
		setDeletePolicy      apps.PersistentVolumeClaimRetentionPolicyType
		condemned            bool
		needsPodRef          bool
		needsSetRef          bool
		unexpectedController bool
	}{
		{
			name:            "retain",
			scaleDownPolicy: apps.RetainPersistentVolumeClaimRetentionPolicyType,
			setDeletePolicy: apps.RetainPersistentVolumeClaimRetentionPolicyType,
			condemned:       false,
			needsPodRef:     false,
			needsSetRef:     false,
		},
		{
			name:            "delete with set",
			scaleDownPolicy: apps.RetainPersistentVolumeClaimRetentionPolicyType,
			setDeletePolicy: apps.DeletePersistentVolumeClaimRetentionPolicyType,
			condemned:       false,
			needsPodRef:     false,
			needsSetRef:     true,
		},
		{
			name:            "delete with scaledown, not condemned",
			scaleDownPolicy: apps.DeletePersistentVolumeClaimRetentionPolicyType,
			setDeletePolicy: apps.RetainPersistentVolumeClaimRetentionPolicyType,
			condemned:       false,
			needsPodRef:     false,
			needsSetRef:     false,
		},
		{
			name:            "delete on scaledown, condemned",
			scaleDownPolicy: apps.DeletePersistentVolumeClaimRetentionPolicyType,
			setDeletePolicy: apps.RetainPersistentVolumeClaimRetentionPolicyType,
			condemned:       true,
			needsPodRef:     true,
			needsSetRef:     false,
		},
		{
			name:            "delete on both, not condemned",
			scaleDownPolicy: apps.DeletePersistentVolumeClaimRetentionPolicyType,
			setDeletePolicy: apps.DeletePersistentVolumeClaimRetentionPolicyType,
			condemned:       false,
			needsPodRef:     false,
			needsSetRef:     true,
		},
		{
			name:            "delete on both, condemned",
			scaleDownPolicy: apps.DeletePersistentVolumeClaimRetentionPolicyType,
			setDeletePolicy: apps.DeletePersistentVolumeClaimRetentionPolicyType,
			condemned:       true,
			needsPodRef:     true,
			needsSetRef:     false,
		},
		{
			name:                 "unexpected controller",
			scaleDownPolicy:      apps.DeletePersistentVolumeClaimRetentionPolicyType,
			setDeletePolicy:      apps.DeletePersistentVolumeClaimRetentionPolicyType,
			condemned:            true,
			needsPodRef:          false,
			needsSetRef:          false,
			unexpectedController: true,
		},
	}
	for _, tc := range testCases {
		for variations := 0; variations < 8; variations++ {
			hasPodRef := (variations & 1) != 0
			hasSetRef := (variations & 2) != 0
			extraOwner := (variations & 3) != 0
			_, ctx := ktesting.NewTestContext(t)
			logger := klog.FromContext(ctx)
			set := apps.StatefulSet{}
			set.Name = "ss"
			numReplicas := int32(5)
			set.Spec.Replicas = &numReplicas
			set.SetUID("ss-123")
			set.Spec.PersistentVolumeClaimRetentionPolicy = &apps.StatefulSetPersistentVolumeClaimRetentionPolicy{
				WhenScaled:  tc.scaleDownPolicy,
				WhenDeleted: tc.setDeletePolicy,
			}
			pod := v1.Pod{}
			if tc.condemned {
				pod.Name = "pod-8"
			} else {
				pod.Name = "pod-1"
			}
			pod.SetUID("pod-456")
			claim := v1.PersistentVolumeClaim{}
			claimRefs := claim.GetOwnerReferences()
			if hasPodRef {
				claimRefs = addControllerRef(claimRefs, &pod, podKind)
			}
			if hasSetRef {
				claimRefs = addControllerRef(claimRefs, &set, controllerKind)
			}
			if extraOwner {
				// Note the extra owner should not affect our owner references.
				claimRefs = append(claimRefs, metav1.OwnerReference{
					APIVersion: "custom/v1",
					Kind:       "random",
					Name:       "random",
					UID:        "abc",
				})
			}
			if tc.unexpectedController {
				claimRefs = append(claimRefs, metav1.OwnerReference{
					APIVersion: "custom/v1",
					Kind:       "Unknown",
					Name:       "unknown",
					UID:        "xyz",
					Controller: ptr.To(true),
				})
			}
			claim.SetOwnerReferences(claimRefs)
			updateClaimOwnerRefForSetAndPod(logger, &claim, &set, &pod)
			// Confirm that after the update, the specified owner is set as the only controller.
			// Any other controllers will be cleaned update by the update.
			check := func(target, owner metav1.Object) bool {
				for _, ref := range target.GetOwnerReferences() {
					if ref.UID == owner.GetUID() {
						return ref.Controller != nil && *ref.Controller
					}
				}
				return false
			}
			if check(&claim, &pod) != tc.needsPodRef {
				t.Errorf("Bad pod ref for %s hasPodRef=%v hasSetRef=%v", tc.name, hasPodRef, hasSetRef)
			}
			if check(&claim, &set) != tc.needsSetRef {
				t.Errorf("Bad set ref for %s hasPodRef=%v hasSetRef=%v", tc.name, hasPodRef, hasSetRef)
			}
		}
	}
}

func TestUpdateClaimControllerRef(t *testing.T) {
	testCases := []struct {
		name         string
		originalRefs []metav1.OwnerReference
		expectedRefs []metav1.OwnerReference
	}{
		{
			name: "set correctly",
			originalRefs: []metav1.OwnerReference{
				{
					APIVersion: "apps/v1",
					Kind:       "StatefulSet",
					Name:       "sts",
					UID:        "123",
					Controller: ptr.To(true),
				},
				{
					APIVersion: "someone",
					Kind:       "Else",
					Name:       "foo",
				},
			},
			expectedRefs: []metav1.OwnerReference{
				{
					APIVersion: "apps/v1",
					Kind:       "StatefulSet",
					Name:       "sts",
					UID:        "123",
					Controller: ptr.To(true),
				},
				{
					APIVersion: "someone",
					Kind:       "Else",
					Name:       "foo",
				},
			},
		},
		{
			name: "missing controller",
			originalRefs: []metav1.OwnerReference{
				{
					APIVersion: "apps/v1",
					Kind:       "StatefulSet",
					Name:       "sts",
					UID:        "123",
				},
			},
			expectedRefs: []metav1.OwnerReference{
				{
					APIVersion: "apps/v1",
					Kind:       "StatefulSet",
					Name:       "sts",
					UID:        "123",
					Controller: ptr.To(true),
				},
			},
		},
		{
			name: "matching name but missing",
			originalRefs: []metav1.OwnerReference{
				{
					APIVersion: "someone",
					Kind:       "else",
					Name:       "sts",
					UID:        "456",
				},
			},
			expectedRefs: []metav1.OwnerReference{
				{
					APIVersion: "someone",
					Kind:       "else",
					Name:       "sts",
					UID:        "456",
				},
				{
					APIVersion: "apps/v1",
					Kind:       "StatefulSet",
					Name:       "sts",
					UID:        "123",
					Controller: ptr.To(true),
				},
			},
		},
		{
			name:         "not present",
			originalRefs: []metav1.OwnerReference{},
			expectedRefs: []metav1.OwnerReference{
				{
					APIVersion: "apps/v1",
					Kind:       "StatefulSet",
					Name:       "sts",
					UID:        "123",
					Controller: ptr.To(true),
				},
			},
		},
		{
			name: "controller, but no UID",
			originalRefs: []metav1.OwnerReference{
				{
					APIVersion: "apps/v1",
					Kind:       "StatefulSet",
					Name:       "sts",
					Controller: ptr.To(true),
				},
			},
			// The missing UID is interpreted as an unexpected stale reference.
			expectedRefs: []metav1.OwnerReference{},
		},
		{
			name: "neither controller nor UID",
			originalRefs: []metav1.OwnerReference{
				{
					APIVersion: "apps/v1",
					Kind:       "StatefulSet",
					Name:       "sts",
				},
			},
			// The missing UID is interpreted as an unexpected stale reference.
			expectedRefs: []metav1.OwnerReference{},
		},
	}
	for _, tc := range testCases {
		_, ctx := ktesting.NewTestContext(t)
		logger := klog.FromContext(ctx)
		set := apps.StatefulSet{}
		set.Name = "sts"
		set.Spec.Replicas = ptr.To(int32(1))
		set.SetUID("123")
		set.Spec.PersistentVolumeClaimRetentionPolicy = &apps.StatefulSetPersistentVolumeClaimRetentionPolicy{
			WhenScaled:  apps.DeletePersistentVolumeClaimRetentionPolicyType,
			WhenDeleted: apps.DeletePersistentVolumeClaimRetentionPolicyType,
		}
		pod := v1.Pod{}
		pod.Name = "pod-0"
		pod.SetUID("456")
		claim := v1.PersistentVolumeClaim{}
		claim.SetOwnerReferences(tc.originalRefs)
		updateClaimOwnerRefForSetAndPod(logger, &claim, &set, &pod)
		if ownerRefsChanged(tc.expectedRefs, claim.GetOwnerReferences()) {
			t.Errorf("%s: expected %v, got %v", tc.name, tc.expectedRefs, claim.GetOwnerReferences())
		}
	}
}

func TestHasOwnerRef(t *testing.T) {
	target := v1.Pod{}
	target.SetOwnerReferences([]metav1.OwnerReference{
		{UID: "123", Controller: ptr.To(true)},
		{UID: "456", Controller: ptr.To(false)},
		{UID: "789"},
	})
	testCases := []struct {
		uid    types.UID
		hasRef bool
	}{
		{
			uid:    "123",
			hasRef: true,
		},
		{
			uid:    "456",
			hasRef: true,
		},
		{
			uid:    "789",
			hasRef: true,
		},
		{
			uid:    "012",
			hasRef: false,
		},
	}
	for _, tc := range testCases {
		owner := v1.Pod{}
		owner.GetObjectMeta().SetUID(tc.uid)
		got := hasOwnerRef(&target, &owner)
		if got != tc.hasRef {
			t.Errorf("Expected %t for %s, got %t", tc.hasRef, tc.uid, got)
		}
	}
}

func TestHasUnexpectedController(t *testing.T) {
	// Each test case will be tested against a StatefulSet named "set" and a Pod named "pod" with UIDs "123".
	testCases := []struct {
		name                             string
		refs                             []metav1.OwnerReference
		shouldReportUnexpectedController bool
	}{
		{
			name: "custom controller",
			refs: []metav1.OwnerReference{
				{
					APIVersion: "chipmunks/v1",
					Kind:       "CustomController",
					Name:       "simon",
					UID:        "other-uid",
					Controller: ptr.To(true),
				},
			},
			shouldReportUnexpectedController: true,
		},
		{
			name: "custom non-controller",
			refs: []metav1.OwnerReference{
				{
					APIVersion: "chipmunks/v1",
					Kind:       "CustomController",
					Name:       "simon",
					UID:        "other-uid",
					Controller: ptr.To(false),
				},
			},
			shouldReportUnexpectedController: false,
		},
		{
			name: "custom unspecified controller",
			refs: []metav1.OwnerReference{
				{
					APIVersion: "chipmunks/v1",
					Kind:       "CustomController",
					Name:       "simon",
					UID:        "other-uid",
				},
			},
			shouldReportUnexpectedController: false,
		},
		{
			name: "other pod controller",
			refs: []metav1.OwnerReference{
				{
					APIVersion: "v1",
					Kind:       "Pod",
					Name:       "simon",
					UID:        "other-uid",
					Controller: ptr.To(true),
				},
			},
			shouldReportUnexpectedController: true,
		},
		{
			name: "other set controller",
			refs: []metav1.OwnerReference{
				{
					APIVersion: "apps/v1",
					Kind:       "Set",
					Name:       "simon",
					UID:        "other-uid",
					Controller: ptr.To(true),
				},
			},
			shouldReportUnexpectedController: true,
		},
		{
			name: "own set controller",
			refs: []metav1.OwnerReference{
				{
					APIVersion: "apps/v1",
					Kind:       "StatefulSet",
					Name:       "set",
					UID:        "set-uid",
					Controller: ptr.To(true),
				},
			},
			shouldReportUnexpectedController: false,
		},
		{
			name: "own set controller, stale uid",
			refs: []metav1.OwnerReference{
				{
					APIVersion: "apps/v1",
					Kind:       "StatefulSet",
					Name:       "set",
					UID:        "stale-uid",
					Controller: ptr.To(true),
				},
			},
			shouldReportUnexpectedController: true,
		},
		{
			name: "own pod controller",
			refs: []metav1.OwnerReference{
				{
					APIVersion: "v1",
					Kind:       "Pod",
					Name:       "pod",
					UID:        "pod-uid",
					Controller: ptr.To(true),
				},
			},
			shouldReportUnexpectedController: false,
		},
		{
			name: "own pod controller, stale uid",
			refs: []metav1.OwnerReference{
				{
					APIVersion: "v1",
					Kind:       "Pod",
					Name:       "pod",
					UID:        "stale-uid",
					Controller: ptr.To(true),
				},
			},
			shouldReportUnexpectedController: true,
		},
		{
			// API validation should prevent two controllers from being set,
			// but for completeness it is still tested.
			name: "own controller and another",
			refs: []metav1.OwnerReference{
				{
					APIVersion: "v1",
					Kind:       "Pod",
					Name:       "pod",
					UID:        "pod-uid",
					Controller: ptr.To(true),
				},
				{
					APIVersion: "chipmunks/v1",
					Kind:       "CustomController",
					Name:       "simon",
					UID:        "other-uid",
					Controller: ptr.To(true),
				},
			},
			shouldReportUnexpectedController: true,
		},
		{
			name: "own controller and a non-controller",
			refs: []metav1.OwnerReference{
				{
					APIVersion: "v1",
					Kind:       "Pod",
					Name:       "pod",
					UID:        "pod-uid",
					Controller: ptr.To(true),
				},
				{
					APIVersion: "chipmunks/v1",
					Kind:       "CustomController",
					Name:       "simon",
					UID:        "other-uid",
					Controller: ptr.To(false),
				},
			},
			shouldReportUnexpectedController: false,
		},
	}
	for _, tc := range testCases {
		target := &v1.PersistentVolumeClaim{}
		target.SetOwnerReferences(tc.refs)
		set := &apps.StatefulSet{}
		set.SetName("set")
		set.SetUID("set-uid")
		pod := &v1.Pod{}
		pod.SetName("pod")
		pod.SetUID("pod-uid")
		set.Spec.PersistentVolumeClaimRetentionPolicy = nil
		if hasUnexpectedController(target, set, pod) {
			t.Errorf("Any controller should be allowed when no retention policy (retain behavior) is specified. Incorrectly identified unexpected controller at %s", tc.name)
		}
		for _, policy := range []apps.StatefulSetPersistentVolumeClaimRetentionPolicy{
			{WhenDeleted: "Retain", WhenScaled: "Delete"},
			{WhenDeleted: "Delete", WhenScaled: "Retain"},
			{WhenDeleted: "Delete", WhenScaled: "Delete"},
		} {
			set.Spec.PersistentVolumeClaimRetentionPolicy = &policy
			got := hasUnexpectedController(target, set, pod)
			if got != tc.shouldReportUnexpectedController {
				t.Errorf("Unexpected controller mismatch at %s (policy %v)", tc.name, policy)
			}
		}
	}
}

func TestNonController(t *testing.T) {
	testCases := []struct {
		name string
		refs []metav1.OwnerReference
		// The set and pod objects will be created with names "set" and "pod", respectively.
		setUID        types.UID
		podUID        types.UID
		nonController bool
	}{
		{
			// API validation should prevent two controllers from being set,
			// but for completeness the semantics here are tested.
			name: "set and pod controller",
			refs: []metav1.OwnerReference{
				{
					APIVersion: "v1",
					Kind:       "Pod",
					Name:       "pod",
					UID:        "pod",
					Controller: ptr.To(true),
				},
				{
					APIVersion: "apps/v1",
					Kind:       "Set",
					Name:       "set",
					UID:        "set",
					Controller: ptr.To(true),
				},
			},
			setUID:        "set",
			podUID:        "pod",
			nonController: false,
		},
		{
			name: "set controller, pod noncontroller",
			refs: []metav1.OwnerReference{
				{
					APIVersion: "v1",
					Kind:       "Pod",
					Name:       "pod",
					UID:        "pod",
				},
				{
					APIVersion: "apps/v1",
					Kind:       "Set",
					Name:       "set",
					UID:        "set",
					Controller: ptr.To(true),
				},
			},
			setUID:        "set",
			podUID:        "pod",
			nonController: true,
		},
		{
			name: "set noncontroller, pod controller",
			refs: []metav1.OwnerReference{
				{
					APIVersion: "v1",
					Kind:       "Pod",
					Name:       "pod",
					UID:        "pod",
					Controller: ptr.To(true),
				},
				{
					APIVersion: "apps/v1",
					Kind:       "Set",
					Name:       "set",
					UID:        "set",
				},
			},
			setUID:        "set",
			podUID:        "pod",
			nonController: true,
		},
		{
			name: "set controller",
			refs: []metav1.OwnerReference{
				{
					APIVersion: "apps/v1",
					Kind:       "Set",
					Name:       "set",
					UID:        "set",
					Controller: ptr.To(true),
				},
			},
			setUID:        "set",
			podUID:        "pod",
			nonController: false,
		},
		{
			name: "pod controller",
			refs: []metav1.OwnerReference{
				{
					APIVersion: "v1",
					Kind:       "Pod",
					Name:       "pod",
					UID:        "pod",
					Controller: ptr.To(true),
				},
			},
			setUID:        "set",
			podUID:        "pod",
			nonController: false,
		},
		{
			name:          "nothing",
			refs:          []metav1.OwnerReference{},
			setUID:        "set",
			podUID:        "pod",
			nonController: false,
		},
		{
			name: "set noncontroller",
			refs: []metav1.OwnerReference{
				{
					APIVersion: "apps/v1",
					Kind:       "Set",
					Name:       "set",
					UID:        "set",
				},
			},
			setUID:        "set",
			podUID:        "pod",
			nonController: true,
		},
		{
			name: "set noncontroller with ptr",
			refs: []metav1.OwnerReference{
				{
					APIVersion: "apps/v1",
					Kind:       "Set",
					Name:       "set",
					UID:        "set",
					Controller: ptr.To(false),
				},
			},
			setUID:        "set",
			podUID:        "pod",
			nonController: true,
		},
		{
			name: "pod noncontroller",
			refs: []metav1.OwnerReference{
				{
					APIVersion: "v1",
					Kind:       "pod",
					Name:       "pod",
					UID:        "pod",
				},
			},
			setUID:        "set",
			podUID:        "pod",
			nonController: true,
		},
		{
			name: "other noncontroller",
			refs: []metav1.OwnerReference{
				{
					APIVersion: "v1",
					Kind:       "pod",
					Name:       "pod",
					UID:        "not-matching",
				},
			},
			setUID:        "set",
			podUID:        "pod",
			nonController: false,
		},
	}

	for _, tc := range testCases {
		claim := v1.PersistentVolumeClaim{}
		claim.SetOwnerReferences(tc.refs)
		pod := v1.Pod{}
		pod.SetUID(tc.podUID)
		pod.SetName("pod")
		set := apps.StatefulSet{}
		set.SetUID(tc.setUID)
		set.SetName("set")
		got := hasNonControllerOwner(&claim, &set, &pod)
		if got != tc.nonController {
			t.Errorf("Failed %s: got %t, expected %t", tc.name, got, tc.nonController)
		}
	}
}

func TestHasStaleOwnerRef(t *testing.T) {
	target := v1.PersistentVolumeClaim{}
	target.SetOwnerReferences([]metav1.OwnerReference{
		{Name: "bob", UID: "123", APIVersion: "v1", Kind: "Pod"},
		{Name: "shirley", UID: "456", APIVersion: "v1", Kind: "Pod"},
	})
	ownerA := v1.Pod{}
	ownerA.SetUID("123")
	ownerA.Name = "bob"
	ownerB := v1.Pod{}
	ownerB.Name = "shirley"
	ownerB.SetUID("789")
	ownerC := v1.Pod{}
	ownerC.Name = "yvonne"
	ownerC.SetUID("345")
	if hasStaleOwnerRef(&target, &ownerA, podKind) {
		t.Error("ownerA should not be stale")
	}
	if !hasStaleOwnerRef(&target, &ownerB, podKind) {
		t.Error("ownerB should be stale")
	}
	if hasStaleOwnerRef(&target, &ownerC, podKind) {
		t.Error("ownerC should not be stale")
	}
}

func TestIsRunningAndReady(t *testing.T) {
	set := newStatefulSet(3)
	pod := newStatefulSetPod(set, 1)
	if isRunningAndReady(pod) {
		t.Error("isRunningAndReady does not respect Pod phase")
	}
	pod.Status.Phase = v1.PodRunning
	if isRunningAndReady(pod) {
		t.Error("isRunningAndReady does not respect Pod condition")
	}
	condition := v1.PodCondition{Type: v1.PodReady, Status: v1.ConditionTrue}
	podutil.UpdatePodCondition(&pod.Status, &condition)
	if !isRunningAndReady(pod) {
		t.Error("Pod should be running and ready")
	}
}

func TestAscendingOrdinal(t *testing.T) {
	set := newStatefulSet(10)
	pods := make([]*v1.Pod, 10)
	perm := rand.Perm(10)
	for i, v := range perm {
		pods[i] = newStatefulSetPod(set, v)
	}
	sort.Sort(ascendingOrdinal(pods))
	if !sort.IsSorted(ascendingOrdinal(pods)) {
		t.Error("ascendingOrdinal fails to sort Pods")
	}
}

func TestOverlappingStatefulSets(t *testing.T) {
	sets := make([]*apps.StatefulSet, 10)
	perm := rand.Perm(10)
	for i, v := range perm {
		sets[i] = newStatefulSet(10)
		sets[i].CreationTimestamp = metav1.NewTime(sets[i].CreationTimestamp.Add(time.Duration(v) * time.Second))
	}
	sort.Sort(overlappingStatefulSets(sets))
	if !sort.IsSorted(overlappingStatefulSets(sets)) {
		t.Error("ascendingOrdinal fails to sort Pods")
	}
	for i, v := range perm {
		sets[i] = newStatefulSet(10)
		sets[i].Name = strconv.FormatInt(int64(v), 10)
	}
	sort.Sort(overlappingStatefulSets(sets))
	if !sort.IsSorted(overlappingStatefulSets(sets)) {
		t.Error("ascendingOrdinal fails to sort Pods")
	}
}

func TestNewPodControllerRef(t *testing.T) {
	set := newStatefulSet(1)
	pod := newStatefulSetPod(set, 0)
	controllerRef := metav1.GetControllerOf(pod)
	if controllerRef == nil {
		t.Fatalf("No ControllerRef found on new pod")
	}
	if got, want := controllerRef.APIVersion, apps.SchemeGroupVersion.String(); got != want {
		t.Errorf("controllerRef.APIVersion = %q, want %q", got, want)
	}
	if got, want := controllerRef.Kind, "StatefulSet"; got != want {
		t.Errorf("controllerRef.Kind = %q, want %q", got, want)
	}
	if got, want := controllerRef.Name, set.Name; got != want {
		t.Errorf("controllerRef.Name = %q, want %q", got, want)
	}
	if got, want := controllerRef.UID, set.UID; got != want {
		t.Errorf("controllerRef.UID = %q, want %q", got, want)
	}
	if got, want := *controllerRef.Controller, true; got != want {
		t.Errorf("controllerRef.Controller = %v, want %v", got, want)
	}
}

func TestCreateApplyRevision(t *testing.T) {
	set := newStatefulSet(1)
	set.Status.CollisionCount = new(int32)
	revision, err := newRevision(set, 1, set.Status.CollisionCount)
	if err != nil {
		t.Fatal(err)
	}
	set.Spec.Template.Spec.Containers[0].Name = "foo"
	if set.Annotations == nil {
		set.Annotations = make(map[string]string)
	}
	key := "foo"
	expectedValue := "bar"
	set.Annotations[key] = expectedValue
	restoredSet, err := ApplyRevision(set, revision)
	if err != nil {
		t.Fatal(err)
	}
	restoredRevision, err := newRevision(restoredSet, 2, restoredSet.Status.CollisionCount)
	if err != nil {
		t.Fatal(err)
	}
	if !history.EqualRevision(revision, restoredRevision) {
		t.Errorf("wanted %v got %v", string(revision.Data.Raw), string(restoredRevision.Data.Raw))
	}
	value, ok := restoredRevision.Annotations[key]
	if !ok {
		t.Errorf("missing annotation %s", key)
	}
	if value != expectedValue {
		t.Errorf("for annotation %s wanted %s got %s", key, expectedValue, value)
	}
}

func TestRollingUpdateApplyRevision(t *testing.T) {
	set := newStatefulSet(1)
	set.Status.CollisionCount = new(int32)
	currentSet := set.DeepCopy()
	currentRevision, err := newRevision(set, 1, set.Status.CollisionCount)
	if err != nil {
		t.Fatal(err)
	}

	set.Spec.Template.Spec.Containers[0].Env = []v1.EnvVar{{Name: "foo", Value: "bar"}}
	updateSet := set.DeepCopy()
	updateRevision, err := newRevision(set, 2, set.Status.CollisionCount)
	if err != nil {
		t.Fatal(err)
	}

	restoredCurrentSet, err := ApplyRevision(set, currentRevision)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(currentSet.Spec.Template, restoredCurrentSet.Spec.Template) {
		t.Errorf("want %v got %v", currentSet.Spec.Template, restoredCurrentSet.Spec.Template)
	}

	restoredUpdateSet, err := ApplyRevision(set, updateRevision)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(updateSet.Spec.Template, restoredUpdateSet.Spec.Template) {
		t.Errorf("want %v got %v", updateSet.Spec.Template, restoredUpdateSet.Spec.Template)
	}
}

func TestGetPersistentVolumeClaims(t *testing.T) {

	// nil inherits statefulset labels
	pod := newPod()
	statefulSet := newStatefulSet(1)
	statefulSet.Spec.Selector.MatchLabels = nil
	claims := getPersistentVolumeClaims(statefulSet, pod)
	pvc := newPVC("datadir-foo-0")
	resultClaims := map[string]v1.PersistentVolumeClaim{"datadir": pvc}

	if !reflect.DeepEqual(claims, resultClaims) {
		t.Fatalf("Unexpected pvc:\n %+v\n, desired pvc:\n %+v", claims, resultClaims)
	}

	// nil inherits statefulset labels
	statefulSet.Spec.Selector.MatchLabels = map[string]string{"test": "test"}
	claims = getPersistentVolumeClaims(statefulSet, pod)
	pvc.SetLabels(map[string]string{"test": "test"})
	resultClaims = map[string]v1.PersistentVolumeClaim{"datadir": pvc}
	if !reflect.DeepEqual(claims, resultClaims) {
		t.Fatalf("Unexpected pvc:\n %+v\n, desired pvc:\n %+v", claims, resultClaims)
	}

	// non-nil with non-overlapping labels merge pvc and statefulset labels
	statefulSet.Spec.Selector.MatchLabels = map[string]string{"name": "foo"}
	statefulSet.Spec.VolumeClaimTemplates[0].ObjectMeta.Labels = map[string]string{"test": "test"}
	claims = getPersistentVolumeClaims(statefulSet, pod)
	pvc.SetLabels(map[string]string{"test": "test", "name": "foo"})
	resultClaims = map[string]v1.PersistentVolumeClaim{"datadir": pvc}
	if !reflect.DeepEqual(claims, resultClaims) {
		t.Fatalf("Unexpected pvc:\n %+v\n, desired pvc:\n %+v", claims, resultClaims)
	}

	// non-nil with overlapping labels merge pvc and statefulset labels and prefer statefulset labels
	statefulSet.Spec.Selector.MatchLabels = map[string]string{"test": "foo"}
	statefulSet.Spec.VolumeClaimTemplates[0].ObjectMeta.Labels = map[string]string{"test": "test"}
	claims = getPersistentVolumeClaims(statefulSet, pod)
	pvc.SetLabels(map[string]string{"test": "foo"})
	resultClaims = map[string]v1.PersistentVolumeClaim{"datadir": pvc}
	if !reflect.DeepEqual(claims, resultClaims) {
		t.Fatalf("Unexpected pvc:\n %+v\n, desired pvc:\n %+v", claims, resultClaims)
	}
}

func newPod() *v1.Pod {
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "foo-0",
			Namespace: v1.NamespaceDefault,
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Name:  "nginx",
					Image: "nginx",
				},
			},
		},
	}
}

func newPVC(name string) v1.PersistentVolumeClaim {
	return v1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: v1.NamespaceDefault,
			Name:      name,
		},
		Spec: v1.PersistentVolumeClaimSpec{
			Resources: v1.VolumeResourceRequirements{
				Requests: v1.ResourceList{
					v1.ResourceStorage: *resource.NewQuantity(1, resource.BinarySI),
				},
			},
		},
	}
}

func newStatefulSetWithVolumes(replicas int32, name string, petMounts []v1.VolumeMount, podMounts []v1.VolumeMount) *apps.StatefulSet {
	mounts := append(petMounts, podMounts...)
	claims := []v1.PersistentVolumeClaim{}
	for _, m := range petMounts {
		claims = append(claims, newPVC(m.Name))
	}

	vols := []v1.Volume{}
	for _, m := range podMounts {
		vols = append(vols, v1.Volume{
			Name: m.Name,
			VolumeSource: v1.VolumeSource{
				HostPath: &v1.HostPathVolumeSource{
					Path: fmt.Sprintf("/tmp/%v", m.Name),
				},
			},
		})
	}

	template := v1.PodTemplateSpec{
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Name:         "nginx",
					Image:        "nginx",
					VolumeMounts: mounts,
				},
			},
			Volumes: vols,
		},
	}

	template.Labels = map[string]string{"foo": "bar"}

	return &apps.StatefulSet{
		TypeMeta: metav1.TypeMeta{
			Kind:       "StatefulSet",
			APIVersion: "apps/v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: v1.NamespaceDefault,
			UID:       types.UID("test"),
		},
		Spec: apps.StatefulSetSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"foo": "bar"},
			},
			Replicas:             ptr.To(replicas),
			Template:             template,
			VolumeClaimTemplates: claims,
			ServiceName:          "governingsvc",
			UpdateStrategy:       apps.StatefulSetUpdateStrategy{Type: apps.RollingUpdateStatefulSetStrategyType},
			PersistentVolumeClaimRetentionPolicy: &apps.StatefulSetPersistentVolumeClaimRetentionPolicy{
				WhenScaled:  apps.RetainPersistentVolumeClaimRetentionPolicyType,
				WhenDeleted: apps.RetainPersistentVolumeClaimRetentionPolicyType,
			},
			RevisionHistoryLimit: func() *int32 {
				limit := int32(2)
				return &limit
			}(),
		},
	}
}

func newStatefulSet(replicas int32) *apps.StatefulSet {
	petMounts := []v1.VolumeMount{
		{Name: "datadir", MountPath: "/tmp/zookeeper"},
	}
	podMounts := []v1.VolumeMount{
		{Name: "home", MountPath: "/home"},
	}
	return newStatefulSetWithVolumes(replicas, "foo", petMounts, podMounts)
}

func newStatefulSetWithLabels(replicas int32, name string, uid types.UID, labels map[string]string) *apps.StatefulSet {
	// Converting all the map-only selectors to set-based selectors.
	var testMatchExpressions []metav1.LabelSelectorRequirement
	for key, value := range labels {
		sel := metav1.LabelSelectorRequirement{
			Key:      key,
			Operator: metav1.LabelSelectorOpIn,
			Values:   []string{value},
		}
		testMatchExpressions = append(testMatchExpressions, sel)
	}
	return &apps.StatefulSet{
		TypeMeta: metav1.TypeMeta{
			Kind:       "StatefulSet",
			APIVersion: "apps/v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: v1.NamespaceDefault,
			UID:       uid,
		},
		Spec: apps.StatefulSetSpec{
			Selector: &metav1.LabelSelector{
				// Purposely leaving MatchLabels nil, so to ensure it will break if any link
				// in the chain ignores the set-based MatchExpressions.
				MatchLabels:      nil,
				MatchExpressions: testMatchExpressions,
			},
			Replicas: ptr.To(replicas),
			PersistentVolumeClaimRetentionPolicy: &apps.StatefulSetPersistentVolumeClaimRetentionPolicy{
				WhenScaled:  apps.RetainPersistentVolumeClaimRetentionPolicyType,
				WhenDeleted: apps.RetainPersistentVolumeClaimRetentionPolicyType,
			},
			Template: v1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
				},
				Spec: v1.PodSpec{
					Containers: []v1.Container{
						{
							Name:  "nginx",
							Image: "nginx",
							VolumeMounts: []v1.VolumeMount{
								{Name: "datadir", MountPath: "/tmp/"},
								{Name: "home", MountPath: "/home"},
							},
						},
					},
					Volumes: []v1.Volume{{
						Name: "home",
						VolumeSource: v1.VolumeSource{
							HostPath: &v1.HostPathVolumeSource{
								Path: fmt.Sprintf("/tmp/%v", "home"),
							},
						}}},
				},
			},
			VolumeClaimTemplates: []v1.PersistentVolumeClaim{
				{
					ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "datadir"},
					Spec: v1.PersistentVolumeClaimSpec{
						Resources: v1.VolumeResourceRequirements{
							Requests: v1.ResourceList{
								v1.ResourceStorage: *resource.NewQuantity(1, resource.BinarySI),
							},
						},
					},
				},
			},
			ServiceName: "governingsvc",
		},
	}
}

func TestGetStatefulSetMaxUnavailable(t *testing.T) {
	testCases := []struct {
		maxUnavailable         *intstr.IntOrString
		replicaCount           int
		expectedMaxUnavailable int
	}{
		// it wouldn't hurt to also test 0 and 0%, even if they should have been forbidden by API validation.
		{maxUnavailable: nil, replicaCount: 10, expectedMaxUnavailable: 1},
		{maxUnavailable: ptr.To(intstr.FromInt32(3)), replicaCount: 10, expectedMaxUnavailable: 3},
		{maxUnavailable: ptr.To(intstr.FromInt32(3)), replicaCount: 0, expectedMaxUnavailable: 3},
		{maxUnavailable: ptr.To(intstr.FromInt32(0)), replicaCount: 0, expectedMaxUnavailable: 1},
		{maxUnavailable: ptr.To(intstr.FromString("10%")), replicaCount: 25, expectedMaxUnavailable: 2},
		{maxUnavailable: ptr.To(intstr.FromString("100%")), replicaCount: 5, expectedMaxUnavailable: 5},
		{maxUnavailable: ptr.To(intstr.FromString("50%")), replicaCount: 5, expectedMaxUnavailable: 2},
		{maxUnavailable: ptr.To(intstr.FromString("10%")), replicaCount: 5, expectedMaxUnavailable: 1},
		{maxUnavailable: ptr.To(intstr.FromString("1%")), replicaCount: 0, expectedMaxUnavailable: 1},
		{maxUnavailable: ptr.To(intstr.FromString("0%")), replicaCount: 0, expectedMaxUnavailable: 1},
	}

	for i, tc := range testCases {
		t.Run(fmt.Sprintf("case %d", i), func(t *testing.T) {
			gotMaxUnavailable, err := getStatefulSetMaxUnavailable(tc.maxUnavailable, tc.replicaCount)
			if err != nil {
				t.Fatal(err)
			}
			if gotMaxUnavailable != tc.expectedMaxUnavailable {
				t.Errorf("Expected maxUnavailable %v, got pods %v", tc.expectedMaxUnavailable, gotMaxUnavailable)
			}
		})
	}
}
