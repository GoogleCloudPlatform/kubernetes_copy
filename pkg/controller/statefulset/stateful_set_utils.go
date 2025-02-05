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
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"

	apps "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/klog/v2"
	podutil "k8s.io/kubernetes/pkg/api/v1/pod"
	"k8s.io/kubernetes/pkg/controller"
	"k8s.io/kubernetes/pkg/controller/history"
	"k8s.io/kubernetes/pkg/features"
)

var patchCodec = scheme.Codecs.LegacyCodec(apps.SchemeGroupVersion)

// overlappingStatefulSets sorts a list of StatefulSets by creation timestamp, using their names as a tie breaker.
// Generally used to tie break between StatefulSets that have overlapping selectors.
type overlappingStatefulSets []*apps.StatefulSet

func (o overlappingStatefulSets) Len() int { return len(o) }

func (o overlappingStatefulSets) Swap(i, j int) { o[i], o[j] = o[j], o[i] }

func (o overlappingStatefulSets) Less(i, j int) bool {
	if o[i].CreationTimestamp.Equal(&o[j].CreationTimestamp) {
		return o[i].Name < o[j].Name
	}
	return o[i].CreationTimestamp.Before(&o[j].CreationTimestamp)
}

// statefulPodRegex is a regular expression that extracts the parent StatefulSet and ordinal from the Name of a Pod
var statefulPodRegex = regexp.MustCompile("(.*)-([0-9]+)$")

// getParentNameAndOrdinal gets the name of pod's parent StatefulSet and pod's ordinal as extracted from its Name. If
// the Pod was not created by a StatefulSet, its parent is considered to be empty string, and its ordinal is considered
// to be -1.
func getParentNameAndOrdinal(pod *v1.Pod) (string, int) {
	parent := ""
	ordinal := -1
	subMatches := statefulPodRegex.FindStringSubmatch(pod.Name)
	if len(subMatches) < 3 {
		return parent, ordinal
	}
	parent = subMatches[1]
	if i, err := strconv.ParseInt(subMatches[2], 10, 32); err == nil {
		ordinal = int(i)
	}
	return parent, ordinal
}

// getParentName gets the name of pod's parent StatefulSet. If pod has not parent, the empty string is returned.
func getParentName(pod *v1.Pod) string {
	parent, _ := getParentNameAndOrdinal(pod)
	return parent
}

// getOrdinal gets pod's ordinal. If pod has no ordinal, -1 is returned.
func getOrdinal(pod *v1.Pod) int {
	_, ordinal := getParentNameAndOrdinal(pod)
	return ordinal
}

// getStartOrdinal gets the first possible ordinal (inclusive).
// Returns spec.ordinals.start if spec.ordinals is set, otherwise returns 0.
func getStartOrdinal(set *apps.StatefulSet) int {
	if set.Spec.Ordinals != nil {
		return int(set.Spec.Ordinals.Start)
	}
	return 0
}

// getEndOrdinal gets the last possible ordinal (inclusive).
func getEndOrdinal(set *apps.StatefulSet) int {
	return getStartOrdinal(set) + int(*set.Spec.Replicas) - 1
}

// podInOrdinalRange returns true if the pod ordinal is within the allowed
// range of ordinals that this StatefulSet is set to control.
func podInOrdinalRange(pod *v1.Pod, set *apps.StatefulSet) bool {
	ordinal := getOrdinal(pod)
	return ordinal >= getStartOrdinal(set) && ordinal <= getEndOrdinal(set)
}

// getPodName gets the name of set's child Pod with an ordinal index of ordinal
func getPodName(set *apps.StatefulSet, ordinal int) string {
	return fmt.Sprintf("%s-%d", set.Name, ordinal)
}

// getPersistentVolumeClaimName gets the name of PersistentVolumeClaim for a Pod with an ordinal index of ordinal. claim
// must be a PersistentVolumeClaim from set's VolumeClaims template.
func getPersistentVolumeClaimName(set *apps.StatefulSet, claim *v1.PersistentVolumeClaim, ordinal int) string {
	// NOTE: This name format is used by the heuristics for zone spreading in ChooseZoneForVolume
	return fmt.Sprintf("%s-%s-%d", claim.Name, set.Name, ordinal)
}

// isMemberOf tests if pod is a member of set.
func isMemberOf(set *apps.StatefulSet, pod *v1.Pod) bool {
	return getParentName(pod) == set.Name
}

// identityMatches returns true if pod has a valid identity and network identity for a member of set.
func identityMatches(set *apps.StatefulSet, pod *v1.Pod) bool {
	parent, ordinal := getParentNameAndOrdinal(pod)
	return ordinal >= 0 &&
		set.Name == parent &&
		pod.Name == getPodName(set, ordinal) &&
		pod.Namespace == set.Namespace &&
		pod.Labels[apps.StatefulSetPodNameLabel] == pod.Name
}

// storageMatches returns true if pod's Volumes cover the set of PersistentVolumeClaims
func storageMatches(set *apps.StatefulSet, pod *v1.Pod) bool {
	ordinal := getOrdinal(pod)
	if ordinal < 0 {
		return false
	}
	volumes := make(map[string]v1.Volume, len(pod.Spec.Volumes))
	for _, volume := range pod.Spec.Volumes {
		volumes[volume.Name] = volume
	}
	for _, claim := range set.Spec.VolumeClaimTemplates {
		volume, found := volumes[claim.Name]
		if !found ||
			volume.VolumeSource.PersistentVolumeClaim == nil ||
			volume.VolumeSource.PersistentVolumeClaim.ClaimName !=
				getPersistentVolumeClaimName(set, &claim, ordinal) {
			return false
		}
	}
	return true
}

// getPersistentVolumeClaimPolicy returns the PVC policy for a StatefulSet, returning a retain policy if the set policy is nil.
func getPersistentVolumeClaimRetentionPolicy(set *apps.StatefulSet) apps.StatefulSetPersistentVolumeClaimRetentionPolicy {
	policy := apps.StatefulSetPersistentVolumeClaimRetentionPolicy{
		WhenDeleted: apps.RetainPersistentVolumeClaimRetentionPolicyType,
		WhenScaled:  apps.RetainPersistentVolumeClaimRetentionPolicyType,
	}
	if set.Spec.PersistentVolumeClaimRetentionPolicy != nil {
		policy = *set.Spec.PersistentVolumeClaimRetentionPolicy
	}
	return policy
}

// matchesRef returns true when the object matches the owner reference, that is the name and GVK are the same.
func matchesRef(ref *metav1.OwnerReference, obj metav1.Object, gvk schema.GroupVersionKind) bool {
	return gvk.GroupVersion().String() == ref.APIVersion && gvk.Kind == ref.Kind && ref.Name == obj.GetName()
}

// hasUnexpectedController returns true if the set has a retention policy and there is a controller
// for the claim that's not the set or pod. Since the retention policy may have been changed, it is
// always valid for the set or pod to be a controller.
func hasUnexpectedController(claim *v1.PersistentVolumeClaim, set *apps.StatefulSet, pod *v1.Pod) bool {
	policy := getPersistentVolumeClaimRetentionPolicy(set)
	const retain = apps.RetainPersistentVolumeClaimRetentionPolicyType
	if policy.WhenScaled == retain && policy.WhenDeleted == retain {
		// On a retain policy, it's not a problem for different controller to be managing the claims.
		return false
	}
	for _, ownerRef := range claim.GetOwnerReferences() {
		if matchesRef(&ownerRef, set, controllerKind) {
			if ownerRef.UID != set.GetUID() {
				// A UID mismatch means that pods were incorrectly orphaned. Treating this as an unexpected
				// controller means we won't touch the PVCs (eg, leave it to the garbage collector to clean
				// up if appropriate).
				return true
			}
			continue // This is us.
		}

		if matchesRef(&ownerRef, pod, podKind) {
			if ownerRef.UID != pod.GetUID() {
				// This is the same situation as the set UID mismatch, above.
				return true
			}
			continue // This is us.
		}
		if ownerRef.Controller != nil && *ownerRef.Controller {
			return true // This is another controller.
		}
	}
	return false
}

// hasNonControllerOwner returns true if the pod or set is an owner but not controller of the claim.
func hasNonControllerOwner(claim *v1.PersistentVolumeClaim, set *apps.StatefulSet, pod *v1.Pod) bool {
	for _, ownerRef := range claim.GetOwnerReferences() {
		if ownerRef.UID == set.GetUID() || ownerRef.UID == pod.GetUID() {
			if ownerRef.Controller == nil || !*ownerRef.Controller {
				return true
			}
		}
	}
	return false
}

// removeRefs removes any owner refs from the list matching predicate. Returns true if the list was changed and
// the new (or unchanged list).
func removeRefs(refs []metav1.OwnerReference, predicate func(ref *metav1.OwnerReference) bool) []metav1.OwnerReference {
	newRefs := []metav1.OwnerReference{}
	for _, ownerRef := range refs {
		if !predicate(&ownerRef) {
			newRefs = append(newRefs, ownerRef)
		}
	}
	return newRefs
}

// isClaimOwnerUpToDate returns false if the ownerRefs of the claim are not set consistently with the
// PVC deletion policy for the StatefulSet.
//
// If there are stale references or unexpected controllers, this returns true in order to not touch
// PVCs that have gotten into this unknown state. Otherwise the ownerships are checked to match the
// PVC retention policy:
//
//	Retain on scaling and set deletion: no owner ref
//	Retain on scaling and delete on set deletion: owner ref on the set only
//	Delete on scaling and retain on set deletion: owner ref on the pod only
//	Delete on scaling and set deletion: owner refs on both set and pod.
func isClaimOwnerUpToDate(logger klog.Logger, claim *v1.PersistentVolumeClaim, set *apps.StatefulSet, pod *v1.Pod) bool {
	if hasStaleOwnerRef(claim, set, controllerKind) || hasStaleOwnerRef(claim, pod, podKind) {
		// The claim is being managed by previous, presumably deleted, version of the controller. It should not be touched.
		return true
	}

	if hasUnexpectedController(claim, set, pod) {
		if hasOwnerRef(claim, set) || hasOwnerRef(claim, pod) {
			return false // Need to clean up the conflicting controllers
		}
		// The claim refs are good, we don't want to add any controllers on top of the unexpected one.
		return true
	}

	if hasNonControllerOwner(claim, set, pod) {
		// Some resource has an owner ref, but there is no controller. This needs to be updated.
		return false
	}

	policy := getPersistentVolumeClaimRetentionPolicy(set)
	const retain = apps.RetainPersistentVolumeClaimRetentionPolicyType
	const delete = apps.DeletePersistentVolumeClaimRetentionPolicyType
	switch {
	default:
		logger.Error(nil, "Unknown policy, treating as Retain", "policy", set.Spec.PersistentVolumeClaimRetentionPolicy)
		fallthrough
	case policy.WhenScaled == retain && policy.WhenDeleted == retain:
		if hasOwnerRef(claim, set) ||
			hasOwnerRef(claim, pod) {
			return false
		}
	case policy.WhenScaled == retain && policy.WhenDeleted == delete:
		if !hasOwnerRef(claim, set) ||
			hasOwnerRef(claim, pod) {
			return false
		}
	case policy.WhenScaled == delete && policy.WhenDeleted == retain:
		if hasOwnerRef(claim, set) {
			return false
		}
		podScaledDown := !podInOrdinalRange(pod, set)
		if podScaledDown != hasOwnerRef(claim, pod) {
			return false
		}
	case policy.WhenScaled == delete && policy.WhenDeleted == delete:
		podScaledDown := !podInOrdinalRange(pod, set)
		// If a pod is scaled down, there should be no set ref and a pod ref;
		// if the pod is not scaled down it's the other way around.
		if podScaledDown == hasOwnerRef(claim, set) {
			return false
		}
		if podScaledDown != hasOwnerRef(claim, pod) {
			return false
		}
	}
	return true
}

// updateClaimOwnerRefForSetAndPod updates the ownerRefs for the claim according to the deletion policy of
// the StatefulSet. Returns true if the claim was changed and should be updated and false otherwise.
// isClaimOwnerUpToDate should be called before this to avoid an expensive update operation.
func updateClaimOwnerRefForSetAndPod(logger klog.Logger, claim *v1.PersistentVolumeClaim, set *apps.StatefulSet, pod *v1.Pod) {
	refs := claim.GetOwnerReferences()

	unexpectedController := hasUnexpectedController(claim, set, pod)

	// Scrub any ownerRefs to our set & pod.
	refs = removeRefs(refs, func(ref *metav1.OwnerReference) bool {
		return matchesRef(ref, set, controllerKind) || matchesRef(ref, pod, podKind)
	})

	if unexpectedController {
		// Leave ownerRefs to our set & pod scrubbed and return without creating new ones.
		claim.SetOwnerReferences(refs)
		return
	}

	policy := getPersistentVolumeClaimRetentionPolicy(set)
	const retain = apps.RetainPersistentVolumeClaimRetentionPolicyType
	const delete = apps.DeletePersistentVolumeClaimRetentionPolicyType
	switch {
	default:
		logger.Error(nil, "Unknown policy, treating as Retain", "policy", set.Spec.PersistentVolumeClaimRetentionPolicy)
		// Nothing to do
	case policy.WhenScaled == retain && policy.WhenDeleted == retain:
		// Nothing to do
	case policy.WhenScaled == retain && policy.WhenDeleted == delete:
		refs = addControllerRef(refs, set, controllerKind)
	case policy.WhenScaled == delete && policy.WhenDeleted == retain:
		podScaledDown := !podInOrdinalRange(pod, set)
		if podScaledDown {
			refs = addControllerRef(refs, pod, podKind)
		}
	case policy.WhenScaled == delete && policy.WhenDeleted == delete:
		podScaledDown := !podInOrdinalRange(pod, set)
		if podScaledDown {
			refs = addControllerRef(refs, pod, podKind)
		}
		if !podScaledDown {
			refs = addControllerRef(refs, set, controllerKind)
		}
	}
	claim.SetOwnerReferences(refs)
}

// hasOwnerRef returns true if target has an ownerRef to owner (as its UID).
// This does not check if the owner is a controller.
func hasOwnerRef(target, owner metav1.Object) bool {
	ownerUID := owner.GetUID()
	for _, ownerRef := range target.GetOwnerReferences() {
		if ownerRef.UID == ownerUID {
			return true
		}
	}
	return false
}

// hasStaleOwnerRef returns true if target has a ref to owner that appears to be stale, that is,
// the ref matches the object but not the UID.
func hasStaleOwnerRef(target *v1.PersistentVolumeClaim, obj metav1.Object, gvk schema.GroupVersionKind) bool {
	for _, ownerRef := range target.GetOwnerReferences() {
		if matchesRef(&ownerRef, obj, gvk) {
			return ownerRef.UID != obj.GetUID()
		}
	}
	return false
}

// addControllerRef returns refs with owner added as a controller, if necessary.
func addControllerRef(refs []metav1.OwnerReference, owner metav1.Object, gvk schema.GroupVersionKind) []metav1.OwnerReference {
	for _, ref := range refs {
		if ref.UID == owner.GetUID() {
			// Already added. Since we scrub our refs before making any changes, we know it's already
			// a controller if appropriate.
			return refs
		}
	}

	return append(refs, *metav1.NewControllerRef(owner, gvk))
}

// getPersistentVolumeClaims gets a map of PersistentVolumeClaims to their template names, as defined in set. The
// returned PersistentVolumeClaims are each constructed with a the name specific to the Pod. This name is determined
// by getPersistentVolumeClaimName.
func getPersistentVolumeClaims(set *apps.StatefulSet, pod *v1.Pod) map[string]v1.PersistentVolumeClaim {
	ordinal := getOrdinal(pod)
	templates := set.Spec.VolumeClaimTemplates
	claims := make(map[string]v1.PersistentVolumeClaim, len(templates))
	for i := range templates {
		claim := templates[i].DeepCopy()
		claim.Name = getPersistentVolumeClaimName(set, claim, ordinal)
		claim.Namespace = set.Namespace
		if claim.Labels != nil {
			for key, value := range set.Spec.Selector.MatchLabels {
				claim.Labels[key] = value
			}
		} else {
			claim.Labels = set.Spec.Selector.MatchLabels
		}
		claims[templates[i].Name] = *claim
	}
	return claims
}

// updateStorage updates pod's Volumes to conform with the PersistentVolumeClaim of set's templates. If pod has
// conflicting local Volumes these are replaced with Volumes that conform to the set's templates.
func updateStorage(set *apps.StatefulSet, pod *v1.Pod) {
	currentVolumes := pod.Spec.Volumes
	claims := getPersistentVolumeClaims(set, pod)
	newVolumes := make([]v1.Volume, 0, len(claims))
	for name, claim := range claims {
		newVolumes = append(newVolumes, v1.Volume{
			Name: name,
			VolumeSource: v1.VolumeSource{
				PersistentVolumeClaim: &v1.PersistentVolumeClaimVolumeSource{
					ClaimName: claim.Name,
					// TODO: Use source definition to set this value when we have one.
					ReadOnly: false,
				},
			},
		})
	}
	for i := range currentVolumes {
		if _, ok := claims[currentVolumes[i].Name]; !ok {
			newVolumes = append(newVolumes, currentVolumes[i])
		}
	}
	pod.Spec.Volumes = newVolumes
}

func initIdentity(set *apps.StatefulSet, pod *v1.Pod) {
	updateIdentity(set, pod)
	// Set these immutable fields only on initial Pod creation, not updates.
	pod.Spec.Hostname = pod.Name
	pod.Spec.Subdomain = set.Spec.ServiceName
}

// updateIdentity updates pod's name, hostname, and subdomain, and StatefulSetPodNameLabel to conform to set's name
// and headless service.
func updateIdentity(set *apps.StatefulSet, pod *v1.Pod) {
	ordinal := getOrdinal(pod)
	pod.Name = getPodName(set, ordinal)
	pod.Namespace = set.Namespace
	if pod.Labels == nil {
		pod.Labels = make(map[string]string)
	}
	pod.Labels[apps.StatefulSetPodNameLabel] = pod.Name
	if utilfeature.DefaultFeatureGate.Enabled(features.PodIndexLabel) {
		pod.Labels[apps.PodIndexLabel] = strconv.Itoa(ordinal)
	}
}

// isRunningAndReady returns true if pod is in the PodRunning Phase, if it has a condition of PodReady.
func isRunningAndReady(pod *v1.Pod) bool {
	return pod.Status.Phase == v1.PodRunning && podutil.IsPodReady(pod)
}

func isRunningAndAvailable(pod *v1.Pod, minReadySeconds int32) bool {
	return podutil.IsPodAvailable(pod, minReadySeconds, metav1.Now())
}

// isCreated returns true if pod has been created and is maintained by the API server
func isCreated(pod *v1.Pod) bool {
	return pod.Status.Phase != ""
}

// isPending returns true if pod has a Phase of PodPending
func isPending(pod *v1.Pod) bool {
	return pod.Status.Phase == v1.PodPending
}

// isFailed returns true if pod has a Phase of PodFailed
func isFailed(pod *v1.Pod) bool {
	return pod.Status.Phase == v1.PodFailed
}

// isSucceeded returns true if pod has a Phase of PodSucceeded
func isSucceeded(pod *v1.Pod) bool {
	return pod.Status.Phase == v1.PodSucceeded
}

// isTerminating returns true if pod's DeletionTimestamp has been set
func isTerminating(pod *v1.Pod) bool {
	return pod.DeletionTimestamp != nil
}

// isHealthy returns true if pod is running and ready and has not been terminated
func isHealthy(pod *v1.Pod) bool {
	return isRunningAndReady(pod) && !isTerminating(pod)
}

// allowsBurst is true if the alpha burst annotation is set.
func allowsBurst(set *apps.StatefulSet) bool {
	return set.Spec.PodManagementPolicy == apps.ParallelPodManagement
}

// setPodRevision sets the revision of Pod to revision by adding the StatefulSetRevisionLabel
func setPodRevision(pod *v1.Pod, revision string) {
	if pod.Labels == nil {
		pod.Labels = make(map[string]string)
	}
	pod.Labels[apps.StatefulSetRevisionLabel] = revision
}

// getPodRevision gets the revision of Pod by inspecting the StatefulSetRevisionLabel. If pod has no revision the empty
// string is returned.
func getPodRevision(pod *v1.Pod) string {
	if pod.Labels == nil {
		return ""
	}
	return pod.Labels[apps.StatefulSetRevisionLabel]
}

// newStatefulSetPod returns a new Pod conforming to the set's Spec with an identity generated from ordinal.
func newStatefulSetPod(set *apps.StatefulSet, ordinal int) *v1.Pod {
	pod, _ := controller.GetPodFromTemplate(&set.Spec.Template, set, metav1.NewControllerRef(set, controllerKind))
	pod.Name = getPodName(set, ordinal)
	initIdentity(set, pod)
	updateStorage(set, pod)
	return pod
}

// newVersionedStatefulSetPod creates a new Pod for a StatefulSet. currentSet is the representation of the set at the
// current revision. updateSet is the representation of the set at the updateRevision. currentRevision is the name of
// the current revision. updateRevision is the name of the update revision. ordinal is the ordinal of the Pod. If the
// returned error is nil, the returned Pod is valid.
func newVersionedStatefulSetPod(currentSet, updateSet *apps.StatefulSet, currentRevision, updateRevision string, ordinal int) *v1.Pod {
	if currentSet.Spec.UpdateStrategy.Type == apps.RollingUpdateStatefulSetStrategyType &&
		(currentSet.Spec.UpdateStrategy.RollingUpdate == nil && ordinal < (getStartOrdinal(currentSet)+int(currentSet.Status.CurrentReplicas))) ||
		(currentSet.Spec.UpdateStrategy.RollingUpdate != nil && ordinal < (getStartOrdinal(currentSet)+int(*currentSet.Spec.UpdateStrategy.RollingUpdate.Partition))) {
		pod := newStatefulSetPod(currentSet, ordinal)
		setPodRevision(pod, currentRevision)
		return pod
	}
	pod := newStatefulSetPod(updateSet, ordinal)
	setPodRevision(pod, updateRevision)
	return pod
}

// getPatch returns a strategic merge patch that can be applied to restore a StatefulSet to a
// previous version. If the returned error is nil the patch is valid. The current state that we save is just the
// PodSpecTemplate. We can modify this later to encompass more state (or less) and remain compatible with previously
// recorded patches.
func getPatch(set *apps.StatefulSet) ([]byte, error) {
	data, err := runtime.Encode(patchCodec, set)
	if err != nil {
		return nil, err
	}
	var raw map[string]interface{}
	err = json.Unmarshal(data, &raw)
	if err != nil {
		return nil, err
	}
	objCopy := make(map[string]interface{})
	specCopy := make(map[string]interface{})
	spec := raw["spec"].(map[string]interface{})
	template := spec["template"].(map[string]interface{})
	specCopy["template"] = template
	template["$patch"] = "replace"
	objCopy["spec"] = specCopy
	patch, err := json.Marshal(objCopy)
	return patch, err
}

// newRevision creates a new ControllerRevision containing a patch that reapplies the target state of set.
// The Revision of the returned ControllerRevision is set to revision. If the returned error is nil, the returned
// ControllerRevision is valid. StatefulSet revisions are stored as patches that re-apply the current state of set
// to a new StatefulSet using a strategic merge patch to replace the saved state of the new StatefulSet.
func newRevision(set *apps.StatefulSet, revision int64, collisionCount *int32) (*apps.ControllerRevision, error) {
	patch, err := getPatch(set)
	if err != nil {
		return nil, err
	}
	cr, err := history.NewControllerRevision(set,
		controllerKind,
		set.Spec.Template.Labels,
		runtime.RawExtension{Raw: patch},
		revision,
		collisionCount)
	if err != nil {
		return nil, err
	}
	if cr.ObjectMeta.Annotations == nil {
		cr.ObjectMeta.Annotations = make(map[string]string)
	}
	for key, value := range set.Annotations {
		cr.ObjectMeta.Annotations[key] = value
	}
	return cr, nil
}

// ApplyRevision returns a new StatefulSet constructed by restoring the state in revision to set. If the returned error
// is nil, the returned StatefulSet is valid.
func ApplyRevision(set *apps.StatefulSet, revision *apps.ControllerRevision) (*apps.StatefulSet, error) {
	clone := set.DeepCopy()
	patched, err := strategicpatch.StrategicMergePatch([]byte(runtime.EncodeOrDie(patchCodec, clone)), revision.Data.Raw, clone)
	if err != nil {
		return nil, err
	}
	restoredSet := &apps.StatefulSet{}
	err = json.Unmarshal(patched, restoredSet)
	if err != nil {
		return nil, err
	}
	return restoredSet, nil
}

// nextRevision finds the next valid revision number based on revisions. If the length of revisions
// is 0 this is 1. Otherwise, it is 1 greater than the largest revision's Revision. This method
// assumes that revisions has been sorted by Revision.
func nextRevision(revisions []*apps.ControllerRevision) int64 {
	count := len(revisions)
	if count <= 0 {
		return 1
	}
	return revisions[count-1].Revision + 1
}

// inconsistentStatus returns true if the ObservedGeneration of status is greater than set's
// Generation or if any of the status's fields do not match those of set's status.
func inconsistentStatus(set *apps.StatefulSet, status *apps.StatefulSetStatus) bool {
	return status.ObservedGeneration > set.Status.ObservedGeneration ||
		status.Replicas != set.Status.Replicas ||
		status.CurrentReplicas != set.Status.CurrentReplicas ||
		status.ReadyReplicas != set.Status.ReadyReplicas ||
		status.UpdatedReplicas != set.Status.UpdatedReplicas ||
		status.CurrentRevision != set.Status.CurrentRevision ||
		status.AvailableReplicas != set.Status.AvailableReplicas ||
		status.UpdateRevision != set.Status.UpdateRevision
}

// completeRollingUpdate completes a rolling update when all of set's replica Pods have been updated
// to the updateRevision. status's currentRevision is set to updateRevision and its' updateRevision
// is set to the empty string. status's currentReplicas is set to updateReplicas and its updateReplicas
// are set to 0.
func completeRollingUpdate(set *apps.StatefulSet, status *apps.StatefulSetStatus) {
	if set.Spec.UpdateStrategy.Type == apps.RollingUpdateStatefulSetStrategyType &&
		status.UpdatedReplicas == *set.Spec.Replicas &&
		status.ReadyReplicas == *set.Spec.Replicas &&
		status.Replicas == *set.Spec.Replicas {
		status.CurrentReplicas = status.UpdatedReplicas
		status.CurrentRevision = status.UpdateRevision
	}
}

// ascendingOrdinal is a sort.Interface that Sorts a list of Pods based on the ordinals extracted
// from the Pod. Pod's that have not been constructed by StatefulSet's have an ordinal of -1, and are therefore pushed
// to the front of the list.
type ascendingOrdinal []*v1.Pod

func (ao ascendingOrdinal) Len() int {
	return len(ao)
}

func (ao ascendingOrdinal) Swap(i, j int) {
	ao[i], ao[j] = ao[j], ao[i]
}

func (ao ascendingOrdinal) Less(i, j int) bool {
	return getOrdinal(ao[i]) < getOrdinal(ao[j])
}

// descendingOrdinal is a sort.Interface that Sorts a list of Pods based on the ordinals extracted
// from the Pod. Pod's that have not been constructed by StatefulSet's have an ordinal of -1, and are therefore pushed
// to the end of the list.
type descendingOrdinal []*v1.Pod

func (do descendingOrdinal) Len() int {
	return len(do)
}

func (do descendingOrdinal) Swap(i, j int) {
	do[i], do[j] = do[j], do[i]
}

func (do descendingOrdinal) Less(i, j int) bool {
	return getOrdinal(do[i]) > getOrdinal(do[j])
}

// getStatefulSetMaxUnavailable calculates the real maxUnavailable number according to the replica count
// and maxUnavailable from rollingUpdateStrategy. The number defaults to 1 if the maxUnavailable field is
// not set, and it will be round down to at least 1 if the maxUnavailable value is a percentage.
// Note that API validation has already guaranteed the maxUnavailable field to be >1 if it is an integer
// or 0% < value <= 100% if it is a percentage, so we don't have to consider other cases.
func getStatefulSetMaxUnavailable(maxUnavailable *intstr.IntOrString, replicaCount int) (int, error) {
	maxUnavailableNum, err := intstr.GetScaledValueFromIntOrPercent(intstr.ValueOrDefault(maxUnavailable, intstr.FromInt32(1)), replicaCount, false)
	if err != nil {
		return 0, err
	}
	// maxUnavailable might be zero for small percentage with round down.
	// So we have to enforce it not to be less than 1.
	if maxUnavailableNum < 1 {
		maxUnavailableNum = 1
	}
	return maxUnavailableNum, nil
}
