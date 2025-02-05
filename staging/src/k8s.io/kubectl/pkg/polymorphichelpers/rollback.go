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

package polymorphichelpers

import (
	"bytes"
	"context"
	"fmt"
	"sort"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/json"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/kubectl/pkg/apps"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/scheme"
	deploymentutil "k8s.io/kubectl/pkg/util/deployment"
)

const (
	rollbackSuccess = "rolled back"
	rollbackSkipped = "skipped rollback"
)

// Rollbacker provides an interface for resources that can be rolled back.
type Rollbacker interface {
	Rollback(obj runtime.Object, updatedAnnotations map[string]string, toRevision int64, dryRunStrategy cmdutil.DryRunStrategy) (string, error)
}

type RollbackVisitor struct {
	clientset kubernetes.Interface
	result    Rollbacker
}

func (v *RollbackVisitor) VisitDeployment(elem apps.GroupKindElement) {
	v.result = &DeploymentRollbacker{v.clientset}
}

func (v *RollbackVisitor) VisitStatefulSet(kind apps.GroupKindElement) {
	v.result = &StatefulSetRollbacker{v.clientset}
}

func (v *RollbackVisitor) VisitDaemonSet(kind apps.GroupKindElement) {
	v.result = &DaemonSetRollbacker{v.clientset}
}

func (v *RollbackVisitor) VisitJob(kind apps.GroupKindElement)                   {}
func (v *RollbackVisitor) VisitPod(kind apps.GroupKindElement)                   {}
func (v *RollbackVisitor) VisitReplicaSet(kind apps.GroupKindElement)            {}
func (v *RollbackVisitor) VisitReplicationController(kind apps.GroupKindElement) {}
func (v *RollbackVisitor) VisitCronJob(kind apps.GroupKindElement)               {}

// RollbackerFor returns an implementation of Rollbacker interface for the given schema kind
func RollbackerFor(kind schema.GroupKind, c kubernetes.Interface) (Rollbacker, error) {
	elem := apps.GroupKindElement(kind)
	visitor := &RollbackVisitor{
		clientset: c,
	}

	err := elem.Accept(visitor)

	if err != nil {
		return nil, fmt.Errorf("error retrieving rollbacker for %q, %v", kind.String(), err)
	}

	if visitor.result == nil {
		return nil, fmt.Errorf("no rollbacker has been implemented for %q", kind)
	}

	return visitor.result, nil
}

type DeploymentRollbacker struct {
	c kubernetes.Interface
}

func (r *DeploymentRollbacker) Rollback(obj runtime.Object, updatedAnnotations map[string]string, toRevision int64, dryRunStrategy cmdutil.DryRunStrategy) (string, error) {
	if toRevision < 0 {
		return "", revisionNotFoundErr(toRevision)
	}
	accessor, err := meta.Accessor(obj)
	if err != nil {
		return "", fmt.Errorf("failed to create accessor for kind %v: %s", obj.GetObjectKind(), err.Error())
	}
	name := accessor.GetName()
	namespace := accessor.GetNamespace()

	// TODO: Fix this after kubectl has been removed from core. It is not possible to convert the runtime.Object
	// to the external appsv1 Deployment without round-tripping through an internal version of Deployment. We're
	// currently getting rid of all internal versions of resources. So we specifically request the appsv1 version
	// here. This follows the same pattern as for DaemonSet and StatefulSet.
	deployment, err := r.c.AppsV1().Deployments(namespace).Get(context.TODO(), name, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("failed to retrieve Deployment %s: %v", name, err)
	}

	rsForRevision, err := deploymentRevision(deployment, r.c, toRevision)
	if err != nil {
		return "", err
	}
	if dryRunStrategy == cmdutil.DryRunClient {
		return printTemplate(&rsForRevision.Spec.Template)
	}
	if deployment.Spec.Paused {
		return "", fmt.Errorf("you cannot rollback a paused deployment; resume it first with 'kubectl rollout resume' and try again")
	}

	// Skip if the revision already matches current Deployment
	if equalIgnoreHash(&rsForRevision.Spec.Template, &deployment.Spec.Template) {
		return fmt.Sprintf("%s (current template already matches revision %d)", rollbackSkipped, toRevision), nil
	}

	// remove hash label before patching back into the deployment
	delete(rsForRevision.Spec.Template.Labels, appsv1.DefaultDeploymentUniqueLabelKey)

	// compute deployment annotations
	annotations := map[string]string{}
	for k := range annotationsToSkip {
		if v, ok := deployment.Annotations[k]; ok {
			annotations[k] = v
		}
	}
	for k, v := range rsForRevision.Annotations {
		if !annotationsToSkip[k] {
			annotations[k] = v
		}
	}

	// make patch to restore
	patchType, patch, err := getDeploymentPatch(&rsForRevision.Spec.Template, annotations)
	if err != nil {
		return "", fmt.Errorf("failed restoring revision %d: %v", toRevision, err)
	}

	patchOptions := metav1.PatchOptions{}
	if dryRunStrategy == cmdutil.DryRunServer {
		patchOptions.DryRun = []string{metav1.DryRunAll}
	}
	// Restore revision
	if _, err = r.c.AppsV1().Deployments(namespace).Patch(context.TODO(), name, patchType, patch, patchOptions); err != nil {
		return "", fmt.Errorf("failed restoring revision %d: %v", toRevision, err)
	}
	return rollbackSuccess, nil
}

// equalIgnoreHash returns true if two given podTemplateSpec are equal, ignoring the diff in value of Labels[pod-template-hash]
// We ignore pod-template-hash because:
//  1. The hash result would be different upon podTemplateSpec API changes
//     (e.g. the addition of a new field will cause the hash code to change)
//  2. The deployment template won't have hash labels
func equalIgnoreHash(template1, template2 *corev1.PodTemplateSpec) bool {
	t1Copy := template1.DeepCopy()
	t2Copy := template2.DeepCopy()
	// Remove hash labels from template.Labels before comparing
	delete(t1Copy.Labels, appsv1.DefaultDeploymentUniqueLabelKey)
	delete(t2Copy.Labels, appsv1.DefaultDeploymentUniqueLabelKey)
	return apiequality.Semantic.DeepEqual(t1Copy, t2Copy)
}

// annotationsToSkip lists the annotations that should be preserved from the deployment and not
// copied from the replicaset when rolling a deployment back
var annotationsToSkip = map[string]bool{
	corev1.LastAppliedConfigAnnotation:                     true,
	deploymentutil.RevisionAnnotation:                      true,
	deploymentutil.RevisionHistoryAnnotation:               true,
	deploymentutil.DesiredReplicasAnnotation:               true,
	deploymentutil.MaxReplicasAnnotation:                   true,
	deploymentutil.ReplicaSetReplicasBeforeScaleAnnotation: true,
	appsv1.DeprecatedRollbackTo:                            true,
}

// getPatch returns a patch that can be applied to restore a Deployment to a
// previous version. If the returned error is nil the patch is valid.
func getDeploymentPatch(podTemplate *corev1.PodTemplateSpec, annotations map[string]string) (types.PatchType, []byte, error) {
	// Create a patch of the Deployment that replaces spec.template
	patch, err := json.Marshal([]interface{}{
		map[string]interface{}{
			"op":    "replace",
			"path":  "/spec/template",
			"value": podTemplate,
		},
		map[string]interface{}{
			"op":    "replace",
			"path":  "/metadata/annotations",
			"value": annotations,
		},
	})
	return types.JSONPatchType, patch, err
}

func deploymentRevision(deployment *appsv1.Deployment, c kubernetes.Interface, toRevision int64) (revision *appsv1.ReplicaSet, err error) {

	_, allOldRSs, newRS, err := deploymentutil.GetAllReplicaSets(deployment, c.AppsV1())
	if err != nil {
		return nil, fmt.Errorf("failed to retrieve replica sets from deployment %s: %v", deployment.Name, err)
	}
	allRSs := allOldRSs
	if newRS != nil {
		allRSs = append(allRSs, newRS)
	}

	var (
		latestReplicaSet   *appsv1.ReplicaSet
		latestRevision     = int64(-1)
		previousReplicaSet *appsv1.ReplicaSet
		previousRevision   = int64(-1)
	)
	for _, rs := range allRSs {
		if v, err := deploymentutil.Revision(rs); err == nil {
			if toRevision == 0 {
				if latestRevision < v {
					// newest one we've seen so far
					previousRevision = latestRevision
					previousReplicaSet = latestReplicaSet
					latestRevision = v
					latestReplicaSet = rs
				} else if previousRevision < v {
					// second newest one we've seen so far
					previousRevision = v
					previousReplicaSet = rs
				}
			} else if toRevision == v {
				return rs, nil
			}
		}
	}

	if toRevision > 0 {
		return nil, revisionNotFoundErr(toRevision)
	}

	if previousReplicaSet == nil {
		return nil, fmt.Errorf("no rollout history found for deployment %q", deployment.Name)
	}
	return previousReplicaSet, nil
}

type DaemonSetRollbacker struct {
	c kubernetes.Interface
}

func (r *DaemonSetRollbacker) Rollback(obj runtime.Object, updatedAnnotations map[string]string, toRevision int64, dryRunStrategy cmdutil.DryRunStrategy) (string, error) {
	if toRevision < 0 {
		return "", revisionNotFoundErr(toRevision)
	}
	accessor, err := meta.Accessor(obj)
	if err != nil {
		return "", fmt.Errorf("failed to create accessor for kind %v: %s", obj.GetObjectKind(), err.Error())
	}
	ds, history, err := daemonSetHistory(r.c.AppsV1(), accessor.GetNamespace(), accessor.GetName())
	if err != nil {
		return "", err
	}
	if toRevision == 0 && len(history) <= 1 {
		return "", fmt.Errorf("no last revision to roll back to")
	}

	toHistory := findHistory(toRevision, history)
	if toHistory == nil {
		return "", revisionNotFoundErr(toRevision)
	}

	if dryRunStrategy == cmdutil.DryRunClient {
		appliedDS, err := applyDaemonSetHistory(ds, toHistory)
		if err != nil {
			return "", err
		}
		return printPodTemplate(&appliedDS.Spec.Template)
	}

	// Skip if the revision already matches current DaemonSet
	done, err := daemonSetMatch(ds, toHistory)
	if err != nil {
		return "", err
	}
	if done {
		return fmt.Sprintf("%s (current template already matches revision %d)", rollbackSkipped, toRevision), nil
	}

	patchOptions := metav1.PatchOptions{}
	if dryRunStrategy == cmdutil.DryRunServer {
		patchOptions.DryRun = []string{metav1.DryRunAll}
	}
	// Restore revision
	if _, err = r.c.AppsV1().DaemonSets(accessor.GetNamespace()).Patch(context.TODO(), accessor.GetName(), types.StrategicMergePatchType, toHistory.Data.Raw, patchOptions); err != nil {
		return "", fmt.Errorf("failed restoring revision %d: %v", toRevision, err)
	}

	return rollbackSuccess, nil
}

// daemonMatch check if the given DaemonSet's template matches the template stored in the given history.
func daemonSetMatch(ds *appsv1.DaemonSet, history *appsv1.ControllerRevision) (bool, error) {
	patch, err := getDaemonSetPatch(ds)
	if err != nil {
		return false, err
	}
	return bytes.Equal(patch, history.Data.Raw), nil
}

// getPatch returns a strategic merge patch that can be applied to restore a Daemonset to a
// previous version. If the returned error is nil the patch is valid. The current state that we save is just the
// PodSpecTemplate. We can modify this later to encompass more state (or less) and remain compatible with previously
// recorded patches.
func getDaemonSetPatch(ds *appsv1.DaemonSet) ([]byte, error) {
	dsBytes, err := json.Marshal(ds)
	if err != nil {
		return nil, err
	}
	var raw map[string]interface{}
	err = json.Unmarshal(dsBytes, &raw)
	if err != nil {
		return nil, err
	}
	objCopy := make(map[string]interface{})
	specCopy := make(map[string]interface{})

	// Create a patch of the DaemonSet that replaces spec.template
	spec := raw["spec"].(map[string]interface{})
	template := spec["template"].(map[string]interface{})
	specCopy["template"] = template
	template["$patch"] = "replace"
	objCopy["spec"] = specCopy
	patch, err := json.Marshal(objCopy)
	return patch, err
}

type StatefulSetRollbacker struct {
	c kubernetes.Interface
}

// toRevision is a non-negative integer, with 0 being reserved to indicate rolling back to previous configuration
func (r *StatefulSetRollbacker) Rollback(obj runtime.Object, updatedAnnotations map[string]string, toRevision int64, dryRunStrategy cmdutil.DryRunStrategy) (string, error) {
	if toRevision < 0 {
		return "", revisionNotFoundErr(toRevision)
	}
	accessor, err := meta.Accessor(obj)
	if err != nil {
		return "", fmt.Errorf("failed to create accessor for kind %v: %s", obj.GetObjectKind(), err.Error())
	}
	sts, history, err := statefulSetHistory(r.c.AppsV1(), accessor.GetNamespace(), accessor.GetName())
	if err != nil {
		return "", err
	}
	if toRevision == 0 && len(history) <= 1 {
		return "", fmt.Errorf("no last revision to roll back to")
	}

	toHistory := findHistory(toRevision, history)
	if toHistory == nil {
		return "", revisionNotFoundErr(toRevision)
	}

	if dryRunStrategy == cmdutil.DryRunClient {
		appliedSS, err := applyRevision(sts, toHistory)
		if err != nil {
			return "", err
		}
		return printPodTemplate(&appliedSS.Spec.Template)
	}

	// Skip if the revision already matches current StatefulSet
	done, err := statefulsetMatch(sts, toHistory)
	if err != nil {
		return "", err
	}
	if done {
		return fmt.Sprintf("%s (current template already matches revision %d)", rollbackSkipped, toRevision), nil
	}

	patchOptions := metav1.PatchOptions{}
	if dryRunStrategy == cmdutil.DryRunServer {
		patchOptions.DryRun = []string{metav1.DryRunAll}
	}
	// Restore revision
	if _, err = r.c.AppsV1().StatefulSets(sts.Namespace).Patch(context.TODO(), sts.Name, types.StrategicMergePatchType, toHistory.Data.Raw, patchOptions); err != nil {
		return "", fmt.Errorf("failed restoring revision %d: %v", toRevision, err)
	}

	return rollbackSuccess, nil
}

var appsCodec = scheme.Codecs.LegacyCodec(appsv1.SchemeGroupVersion)

// applyRevision returns a new StatefulSet constructed by restoring the state in revision to set. If the returned error
// is nil, the returned StatefulSet is valid.
func applyRevision(set *appsv1.StatefulSet, revision *appsv1.ControllerRevision) (*appsv1.StatefulSet, error) {
	patched, err := strategicpatch.StrategicMergePatch([]byte(runtime.EncodeOrDie(appsCodec, set)), revision.Data.Raw, set)
	if err != nil {
		return nil, err
	}
	result := &appsv1.StatefulSet{}
	err = json.Unmarshal(patched, result)
	if err != nil {
		return nil, err
	}
	return result, nil
}

// statefulsetMatch check if the given StatefulSet's template matches the template stored in the given history.
func statefulsetMatch(ss *appsv1.StatefulSet, history *appsv1.ControllerRevision) (bool, error) {
	patch, err := getStatefulSetPatch(ss)
	if err != nil {
		return false, err
	}
	return bytes.Equal(patch, history.Data.Raw), nil
}

// getStatefulSetPatch returns a strategic merge patch that can be applied to restore a StatefulSet to a
// previous version. If the returned error is nil the patch is valid. The current state that we save is just the
// PodSpecTemplate. We can modify this later to encompass more state (or less) and remain compatible with previously
// recorded patches.
func getStatefulSetPatch(set *appsv1.StatefulSet) ([]byte, error) {
	str, err := runtime.Encode(appsCodec, set)
	if err != nil {
		return nil, err
	}
	var raw map[string]interface{}
	if err := json.Unmarshal([]byte(str), &raw); err != nil {
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

// findHistory returns a controllerrevision of a specific revision from the given controllerrevisions.
// It returns nil if no such controllerrevision exists.
// If toRevision is 0, the last previously used history is returned.
func findHistory(toRevision int64, allHistory []*appsv1.ControllerRevision) *appsv1.ControllerRevision {
	if toRevision == 0 && len(allHistory) <= 1 {
		return nil
	}

	// Find the history to rollback to
	var toHistory *appsv1.ControllerRevision
	if toRevision == 0 {
		// If toRevision == 0, find the latest revision (2nd max)
		sort.Sort(historiesByRevision(allHistory))
		toHistory = allHistory[len(allHistory)-2]
	} else {
		for _, h := range allHistory {
			if h.Revision == toRevision {
				// If toRevision != 0, find the history with matching revision
				return h
			}
		}
	}

	return toHistory
}

// printPodTemplate converts a given pod template into a human-readable string.
func printPodTemplate(specTemplate *corev1.PodTemplateSpec) (string, error) {
	podSpec, err := printTemplate(specTemplate)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("will roll back to %s", podSpec), nil
}

func revisionNotFoundErr(r int64) error {
	return fmt.Errorf("unable to find specified revision %v in history", r)
}

// TODO: copied from daemon controller, should extract to a library
type historiesByRevision []*appsv1.ControllerRevision

func (h historiesByRevision) Len() int      { return len(h) }
func (h historiesByRevision) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h historiesByRevision) Less(i, j int) bool {
	return h[i].Revision < h[j].Revision
}
