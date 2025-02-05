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
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	apps "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/version"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/client-go/kubernetes/fake"
	corelisters "k8s.io/client-go/listers/core/v1"
	core "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	"k8s.io/klog/v2/ktesting"
	_ "k8s.io/kubernetes/pkg/apis/apps/install"
	_ "k8s.io/kubernetes/pkg/apis/core/install"
	"k8s.io/kubernetes/pkg/features"
	"k8s.io/utils/ptr"
)

func TestStatefulPodControlCreatesPods(t *testing.T) {
	recorder := record.NewFakeRecorder(10)
	set := newStatefulSet(3)
	pod := newStatefulSetPod(set, 0)
	fakeClient := &fake.Clientset{}
	claimIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
	claimLister := corelisters.NewPersistentVolumeClaimLister(claimIndexer)
	control := NewStatefulPodControl(fakeClient, nil, claimLister, recorder)
	fakeClient.AddReactor("get", "persistentvolumeclaims", func(action core.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewNotFound(action.GetResource().GroupResource(), action.GetResource().Resource)
	})
	fakeClient.AddReactor("create", "persistentvolumeclaims", func(action core.Action) (bool, runtime.Object, error) {
		create := action.(core.CreateAction)
		claimIndexer.Add(create.GetObject())
		return true, create.GetObject(), nil
	})
	fakeClient.AddReactor("create", "pods", func(action core.Action) (bool, runtime.Object, error) {
		create := action.(core.CreateAction)
		return true, create.GetObject(), nil
	})
	if err := control.CreateStatefulPod(context.TODO(), set, pod); err != nil {
		t.Errorf("StatefulPodControl failed to create Pod error: %s", err)
	}
	events := collectEvents(recorder.Events)
	if eventCount := len(events); eventCount != 2 {
		t.Errorf("Expected 2 events for successful create found %d", eventCount)
	}
	for i := range events {
		if !strings.Contains(events[i], v1.EventTypeNormal) {
			t.Errorf("Found unexpected non-normal event %s", events[i])
		}
	}
}

func TestStatefulPodControlCreatePodExists(t *testing.T) {
	recorder := record.NewFakeRecorder(10)
	set := newStatefulSet(3)
	pod := newStatefulSetPod(set, 0)
	fakeClient := &fake.Clientset{}
	pvcs := getPersistentVolumeClaims(set, pod)
	pvcIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
	for k := range pvcs {
		pvc := pvcs[k]
		pvcIndexer.Add(&pvc)
	}
	pvcLister := corelisters.NewPersistentVolumeClaimLister(pvcIndexer)
	control := NewStatefulPodControl(fakeClient, nil, pvcLister, recorder)
	fakeClient.AddReactor("create", "persistentvolumeclaims", func(action core.Action) (bool, runtime.Object, error) {
		create := action.(core.CreateAction)
		return true, create.GetObject(), nil
	})
	fakeClient.AddReactor("create", "pods", func(action core.Action) (bool, runtime.Object, error) {
		return true, pod, apierrors.NewAlreadyExists(action.GetResource().GroupResource(), pod.Name)
	})
	if err := control.CreateStatefulPod(context.TODO(), set, pod); !apierrors.IsAlreadyExists(err) {
		t.Errorf("Failed to create Pod error: %s", err)
	}
	events := collectEvents(recorder.Events)
	if eventCount := len(events); eventCount != 0 {
		t.Errorf("Pod and PVC exist: got %d events, but want 0", eventCount)
		for i := range events {
			t.Log(events[i])
		}
	}
}

func TestStatefulPodControlCreatePodPvcCreateFailure(t *testing.T) {
	recorder := record.NewFakeRecorder(10)
	set := newStatefulSet(3)
	pod := newStatefulSetPod(set, 0)
	fakeClient := &fake.Clientset{}
	pvcIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
	pvcLister := corelisters.NewPersistentVolumeClaimLister(pvcIndexer)
	control := NewStatefulPodControl(fakeClient, nil, pvcLister, recorder)
	fakeClient.AddReactor("create", "persistentvolumeclaims", func(action core.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewInternalError(errors.New("API server down"))
	})
	fakeClient.AddReactor("create", "pods", func(action core.Action) (bool, runtime.Object, error) {
		create := action.(core.CreateAction)
		return true, create.GetObject(), nil
	})
	if err := control.CreateStatefulPod(context.TODO(), set, pod); err == nil {
		t.Error("Failed to produce error on PVC creation failure")
	}
	events := collectEvents(recorder.Events)
	if eventCount := len(events); eventCount != 2 {
		t.Errorf("PVC create failure: got %d events, but want 2", eventCount)
	}
	for i := range events {
		if !strings.Contains(events[i], v1.EventTypeWarning) {
			t.Errorf("Found unexpected non-warning event %s", events[i])
		}
	}
}
func TestStatefulPodControlCreatePodPVCDeleting(t *testing.T) {
	recorder := record.NewFakeRecorder(10)
	set := newStatefulSet(3)
	pod := newStatefulSetPod(set, 0)
	fakeClient := &fake.Clientset{}
	pvcs := getPersistentVolumeClaims(set, pod)
	pvcIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
	deleteTime := time.Date(2019, time.January, 1, 0, 0, 0, 0, time.UTC)
	for k := range pvcs {
		pvc := pvcs[k]
		pvc.DeletionTimestamp = &metav1.Time{Time: deleteTime}
		pvcIndexer.Add(&pvc)
	}
	pvcLister := corelisters.NewPersistentVolumeClaimLister(pvcIndexer)
	control := NewStatefulPodControl(fakeClient, nil, pvcLister, recorder)
	fakeClient.AddReactor("create", "persistentvolumeclaims", func(action core.Action) (bool, runtime.Object, error) {
		create := action.(core.CreateAction)
		return true, create.GetObject(), nil
	})
	fakeClient.AddReactor("create", "pods", func(action core.Action) (bool, runtime.Object, error) {
		create := action.(core.CreateAction)
		return true, create.GetObject(), nil
	})
	if err := control.CreateStatefulPod(context.TODO(), set, pod); err == nil {
		t.Error("Failed to produce error on deleting PVC")
	}
	events := collectEvents(recorder.Events)
	if eventCount := len(events); eventCount != 1 {
		t.Errorf("Deleting PVC: got %d events, but want 1", eventCount)
	}
	for i := range events {
		if !strings.Contains(events[i], v1.EventTypeWarning) {
			t.Errorf("Found unexpected non-warning event %s", events[i])
		}
	}
}

type fakeIndexer struct {
	cache.Indexer
	getError error
}

func (f *fakeIndexer) GetByKey(key string) (interface{}, bool, error) {
	return nil, false, f.getError
}

func TestStatefulPodControlCreatePodPvcGetFailure(t *testing.T) {
	recorder := record.NewFakeRecorder(10)
	set := newStatefulSet(3)
	pod := newStatefulSetPod(set, 0)
	fakeClient := &fake.Clientset{}
	pvcIndexer := &fakeIndexer{getError: errors.New("API server down")}
	pvcLister := corelisters.NewPersistentVolumeClaimLister(pvcIndexer)
	control := NewStatefulPodControl(fakeClient, nil, pvcLister, recorder)
	fakeClient.AddReactor("create", "persistentvolumeclaims", func(action core.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewInternalError(errors.New("API server down"))
	})
	fakeClient.AddReactor("create", "pods", func(action core.Action) (bool, runtime.Object, error) {
		create := action.(core.CreateAction)
		return true, create.GetObject(), nil
	})
	if err := control.CreateStatefulPod(context.TODO(), set, pod); err == nil {
		t.Error("Failed to produce error on PVC creation failure")
	}
	events := collectEvents(recorder.Events)
	if eventCount := len(events); eventCount != 2 {
		t.Errorf("PVC create failure: got %d events, but want 2", eventCount)
	}
	for i := range events {
		if !strings.Contains(events[i], v1.EventTypeWarning) {
			t.Errorf("Found unexpected non-warning event: %s", events[i])
		}
	}
}

func TestStatefulPodControlCreatePodFailed(t *testing.T) {
	recorder := record.NewFakeRecorder(10)
	set := newStatefulSet(3)
	pod := newStatefulSetPod(set, 0)
	fakeClient := &fake.Clientset{}
	pvcIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
	pvcLister := corelisters.NewPersistentVolumeClaimLister(pvcIndexer)
	control := NewStatefulPodControl(fakeClient, nil, pvcLister, recorder)
	fakeClient.AddReactor("create", "persistentvolumeclaims", func(action core.Action) (bool, runtime.Object, error) {
		create := action.(core.CreateAction)
		return true, create.GetObject(), nil
	})
	fakeClient.AddReactor("create", "pods", func(action core.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewInternalError(errors.New("API server down"))
	})
	if err := control.CreateStatefulPod(context.TODO(), set, pod); err == nil {
		t.Error("Failed to produce error on Pod creation failure")
	}
	events := collectEvents(recorder.Events)
	if eventCount := len(events); eventCount != 2 {
		t.Errorf("Pod create failed: got %d events, but want 2", eventCount)
	} else if !strings.Contains(events[0], v1.EventTypeNormal) {
		t.Errorf("Found unexpected non-normal event %s", events[0])

	} else if !strings.Contains(events[1], v1.EventTypeWarning) {
		t.Errorf("Found unexpected non-warning event %s", events[1])
	}
}

func TestStatefulPodControlNoOpUpdate(t *testing.T) {
	_, ctx := ktesting.NewTestContext(t)
	recorder := record.NewFakeRecorder(10)
	set := newStatefulSet(3)
	pod := newStatefulSetPod(set, 0)
	fakeClient := &fake.Clientset{}
	claims := getPersistentVolumeClaims(set, pod)
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
	for k := range claims {
		claim := claims[k]
		indexer.Add(&claim)
	}
	claimLister := corelisters.NewPersistentVolumeClaimLister(indexer)
	control := NewStatefulPodControl(fakeClient, nil, claimLister, recorder)
	fakeClient.AddReactor("*", "*", func(action core.Action) (bool, runtime.Object, error) {
		t.Error("no-op update should not make any client invocation")
		return true, nil, apierrors.NewInternalError(errors.New("if we are here we have a problem"))
	})
	if err := control.UpdateStatefulPod(ctx, set, pod); err != nil {
		t.Errorf("Error returned on no-op update error: %s", err)
	}
	events := collectEvents(recorder.Events)
	if eventCount := len(events); eventCount != 0 {
		t.Errorf("no-op update: got %d events, but want 0", eventCount)
	}
}

func TestStatefulPodControlUpdatesIdentity(t *testing.T) {
	_, ctx := ktesting.NewTestContext(t)
	recorder := record.NewFakeRecorder(10)
	set := newStatefulSet(3)
	pod := newStatefulSetPod(set, 0)
	fakeClient := fake.NewSimpleClientset(set, pod)
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
	claimLister := corelisters.NewPersistentVolumeClaimLister(indexer)
	control := NewStatefulPodControl(fakeClient, nil, claimLister, recorder)
	var updated *v1.Pod
	fakeClient.PrependReactor("update", "pods", func(action core.Action) (bool, runtime.Object, error) {
		update := action.(core.UpdateAction)
		updated = update.GetObject().(*v1.Pod)
		return true, update.GetObject(), nil
	})
	pod.Name = "goo-0"
	if err := control.UpdateStatefulPod(ctx, set, pod); err != nil {
		t.Errorf("Successful update returned an error: %s", err)
	}
	events := collectEvents(recorder.Events)
	if eventCount := len(events); eventCount != 1 {
		t.Errorf("Pod update successful:got %d events,but want 1", eventCount)
	} else if !strings.Contains(events[0], v1.EventTypeNormal) {
		t.Errorf("Found unexpected non-normal event %s", events[0])
	}
	if !identityMatches(set, updated) {
		t.Error("Name update failed identity does not match")
	}
}

func TestStatefulPodControlUpdateIdentityFailure(t *testing.T) {
	_, ctx := ktesting.NewTestContext(t)
	recorder := record.NewFakeRecorder(10)
	set := newStatefulSet(3)
	pod := newStatefulSetPod(set, 0)
	fakeClient := &fake.Clientset{}
	podIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
	gooPod := newStatefulSetPod(set, 0)
	gooPod.Name = "goo-0"
	podIndexer.Add(gooPod)
	podLister := corelisters.NewPodLister(podIndexer)
	claimIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
	claimLister := corelisters.NewPersistentVolumeClaimLister(claimIndexer)
	control := NewStatefulPodControl(fakeClient, podLister, claimLister, recorder)
	fakeClient.AddReactor("update", "pods", func(action core.Action) (bool, runtime.Object, error) {
		pod.Name = "goo-0"
		return true, nil, apierrors.NewInternalError(errors.New("API server down"))
	})
	pod.Name = "goo-0"
	if err := control.UpdateStatefulPod(ctx, set, pod); err == nil {
		t.Error("Failed update does not generate an error")
	}
	events := collectEvents(recorder.Events)
	if eventCount := len(events); eventCount != 1 {
		t.Errorf("Pod update failed: got %d events, but want 1", eventCount)
	} else if !strings.Contains(events[0], v1.EventTypeWarning) {
		t.Errorf("Found unexpected non-warning event %s", events[0])
	}
	if identityMatches(set, pod) {
		t.Error("Failed update mutated Pod identity")
	}
}

func TestStatefulPodControlUpdatesPodStorage(t *testing.T) {
	_, ctx := ktesting.NewTestContext(t)
	recorder := record.NewFakeRecorder(10)
	set := newStatefulSet(3)
	pod := newStatefulSetPod(set, 0)
	fakeClient := &fake.Clientset{}
	pvcIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
	pvcLister := corelisters.NewPersistentVolumeClaimLister(pvcIndexer)
	control := NewStatefulPodControl(fakeClient, nil, pvcLister, recorder)
	pvcs := getPersistentVolumeClaims(set, pod)
	volumes := make([]v1.Volume, 0, len(pod.Spec.Volumes))
	for i := range pod.Spec.Volumes {
		if _, contains := pvcs[pod.Spec.Volumes[i].Name]; !contains {
			volumes = append(volumes, pod.Spec.Volumes[i])
		}
	}
	pod.Spec.Volumes = volumes
	fakeClient.AddReactor("update", "pods", func(action core.Action) (bool, runtime.Object, error) {
		update := action.(core.UpdateAction)
		return true, update.GetObject(), nil
	})
	fakeClient.AddReactor("create", "persistentvolumeclaims", func(action core.Action) (bool, runtime.Object, error) {
		update := action.(core.UpdateAction)
		return true, update.GetObject(), nil
	})
	var updated *v1.Pod
	fakeClient.PrependReactor("update", "pods", func(action core.Action) (bool, runtime.Object, error) {
		update := action.(core.UpdateAction)
		updated = update.GetObject().(*v1.Pod)
		return true, update.GetObject(), nil
	})
	if err := control.UpdateStatefulPod(ctx, set, pod); err != nil {
		t.Errorf("Successful update returned an error: %s", err)
	}
	events := collectEvents(recorder.Events)
	if eventCount := len(events); eventCount != 2 {
		t.Errorf("Pod storage update successful: got %d events, but want 2", eventCount)
	}
	for i := range events {
		if !strings.Contains(events[i], v1.EventTypeNormal) {
			t.Errorf("Found unexpected non-normal event %s", events[i])
		}
	}
	if !storageMatches(set, updated) {
		t.Error("Name update failed identity does not match")
	}
}

func TestStatefulPodControlUpdatePodStorageFailure(t *testing.T) {
	_, ctx := ktesting.NewTestContext(t)
	recorder := record.NewFakeRecorder(10)
	set := newStatefulSet(3)
	pod := newStatefulSetPod(set, 0)
	fakeClient := &fake.Clientset{}
	pvcIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
	pvcLister := corelisters.NewPersistentVolumeClaimLister(pvcIndexer)
	control := NewStatefulPodControl(fakeClient, nil, pvcLister, recorder)
	pvcs := getPersistentVolumeClaims(set, pod)
	volumes := make([]v1.Volume, 0, len(pod.Spec.Volumes))
	for i := range pod.Spec.Volumes {
		if _, contains := pvcs[pod.Spec.Volumes[i].Name]; !contains {
			volumes = append(volumes, pod.Spec.Volumes[i])
		}
	}
	pod.Spec.Volumes = volumes
	fakeClient.AddReactor("update", "pods", func(action core.Action) (bool, runtime.Object, error) {
		update := action.(core.UpdateAction)
		return true, update.GetObject(), nil
	})
	fakeClient.AddReactor("create", "persistentvolumeclaims", func(action core.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewInternalError(errors.New("API server down"))
	})
	if err := control.UpdateStatefulPod(ctx, set, pod); err == nil {
		t.Error("Failed Pod storage update did not return an error")
	}
	events := collectEvents(recorder.Events)
	if eventCount := len(events); eventCount != 2 {
		t.Errorf("Pod storage update failed: got %d events, but want 2", eventCount)
	}
	for i := range events {
		if !strings.Contains(events[i], v1.EventTypeWarning) {
			t.Errorf("Found unexpected non-normal event %s", events[i])
		}
	}
}

func TestStatefulPodControlUpdatePodConflictSuccess(t *testing.T) {
	_, ctx := ktesting.NewTestContext(t)
	recorder := record.NewFakeRecorder(10)
	set := newStatefulSet(3)
	pod := newStatefulSetPod(set, 0)
	fakeClient := &fake.Clientset{}
	podIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
	podLister := corelisters.NewPodLister(podIndexer)
	claimIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
	claimLister := corelisters.NewPersistentVolumeClaimLister(podIndexer)
	gooPod := newStatefulSetPod(set, 0)
	gooPod.Labels[apps.StatefulSetPodNameLabel] = "goo-starts"
	podIndexer.Add(gooPod)
	claims := getPersistentVolumeClaims(set, gooPod)
	for k := range claims {
		claim := claims[k]
		claimIndexer.Add(&claim)
	}
	control := NewStatefulPodControl(fakeClient, podLister, claimLister, recorder)
	conflict := false
	fakeClient.AddReactor("update", "pods", func(action core.Action) (bool, runtime.Object, error) {
		update := action.(core.UpdateAction)
		if !conflict {
			conflict = true
			return true, update.GetObject(), apierrors.NewConflict(action.GetResource().GroupResource(), pod.Name, errors.New("conflict"))
		}
		return true, update.GetObject(), nil

	})
	pod.Labels[apps.StatefulSetPodNameLabel] = "goo-0"
	if err := control.UpdateStatefulPod(ctx, set, pod); err != nil {
		t.Errorf("Successful update returned an error: %s", err)
	}
	events := collectEvents(recorder.Events)
	if eventCount := len(events); eventCount != 1 {
		t.Errorf("Pod update successful: got %d, but want 1", eventCount)
	} else if !strings.Contains(events[0], v1.EventTypeNormal) {
		t.Errorf("Found unexpected non-normal event %s", events[0])
	}
	if !identityMatches(set, pod) {
		t.Error("Name update failed identity does not match")
	}
}

func TestStatefulPodControlDeletesStatefulPod(t *testing.T) {
	recorder := record.NewFakeRecorder(10)
	set := newStatefulSet(3)
	pod := newStatefulSetPod(set, 0)
	fakeClient := &fake.Clientset{}
	control := NewStatefulPodControl(fakeClient, nil, nil, recorder)
	fakeClient.AddReactor("delete", "pods", func(action core.Action) (bool, runtime.Object, error) {
		return true, nil, nil
	})
	if err := control.DeleteStatefulPod(set, pod); err != nil {
		t.Errorf("Error returned on successful delete: %s", err)
	}
	events := collectEvents(recorder.Events)
	if eventCount := len(events); eventCount != 1 {
		t.Errorf("delete successful: got %d events, but want 1", eventCount)
	} else if !strings.Contains(events[0], v1.EventTypeNormal) {
		t.Errorf("Found unexpected non-normal event %s", events[0])
	}
}

func TestStatefulPodControlDeleteFailure(t *testing.T) {
	recorder := record.NewFakeRecorder(10)
	set := newStatefulSet(3)
	pod := newStatefulSetPod(set, 0)
	fakeClient := &fake.Clientset{}
	control := NewStatefulPodControl(fakeClient, nil, nil, recorder)
	fakeClient.AddReactor("delete", "pods", func(action core.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewInternalError(errors.New("API server down"))
	})
	if err := control.DeleteStatefulPod(set, pod); err == nil {
		t.Error("Failed to return error on failed delete")
	}
	events := collectEvents(recorder.Events)
	if eventCount := len(events); eventCount != 1 {
		t.Errorf("delete failed: got %d events, but want 1", eventCount)
	} else if !strings.Contains(events[0], v1.EventTypeWarning) {
		t.Errorf("Found unexpected non-warning event %s", events[0])
	}
}

func TestStatefulPodControlClaimsMatchDeletionPolicy(t *testing.T) {
	// The isClaimOwnerUpToDate is tested exhaustively in stateful_set_utils_test; this
	// test is for the wiring to the method tested there.
	_, ctx := ktesting.NewTestContext(t)
	fakeClient := &fake.Clientset{}
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
	claimLister := corelisters.NewPersistentVolumeClaimLister(indexer)
	set := newStatefulSet(3)
	pod := newStatefulSetPod(set, 0)
	claims := getPersistentVolumeClaims(set, pod)
	for k := range claims {
		claim := claims[k]
		indexer.Add(&claim)
	}
	control := NewStatefulPodControl(fakeClient, nil, claimLister, &noopRecorder{})
	set.Spec.PersistentVolumeClaimRetentionPolicy = &apps.StatefulSetPersistentVolumeClaimRetentionPolicy{
		WhenDeleted: apps.RetainPersistentVolumeClaimRetentionPolicyType,
		WhenScaled:  apps.RetainPersistentVolumeClaimRetentionPolicyType,
	}
	if matches, err := control.ClaimsMatchRetentionPolicy(ctx, set, pod); err != nil {
		t.Errorf("Unexpected error for ClaimsMatchRetentionPolicy (retain): %v", err)
	} else if !matches {
		t.Error("Unexpected non-match for ClaimsMatchRetentionPolicy (retain)")
	}
	set.Spec.PersistentVolumeClaimRetentionPolicy = &apps.StatefulSetPersistentVolumeClaimRetentionPolicy{
		WhenDeleted: apps.DeletePersistentVolumeClaimRetentionPolicyType,
		WhenScaled:  apps.RetainPersistentVolumeClaimRetentionPolicyType,
	}
	if matches, err := control.ClaimsMatchRetentionPolicy(ctx, set, pod); err != nil {
		t.Errorf("Unexpected error for ClaimsMatchRetentionPolicy (set deletion): %v", err)
	} else if matches {
		t.Error("Unexpected match for ClaimsMatchRetentionPolicy (set deletion)")
	}
}

func TestStatefulPodControlUpdatePodClaimForRetentionPolicy(t *testing.T) {
	// All the update conditions are tested exhaustively in stateful_set_utils_test. This
	// tests the wiring from the pod control to that method.

	testFn := func(t *testing.T) {
		_, ctx := ktesting.NewTestContext(t)
		testCases := []struct {
			name      string
			ownerRef  []metav1.OwnerReference
			expectRef bool
		}{
			{
				name:      "bare PVC",
				expectRef: true,
			},
			{
				name:      "PVC already controller",
				ownerRef:  []metav1.OwnerReference{{Controller: ptr.To(true), Name: "foobar"}},
				expectRef: false,
			},
		}

		for _, tc := range testCases {
			fakeClient := &fake.Clientset{}
			indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
			claimLister := corelisters.NewPersistentVolumeClaimLister(indexer)
			fakeClient.AddReactor("update", "persistentvolumeclaims", func(action core.Action) (bool, runtime.Object, error) {
				update := action.(core.UpdateAction)
				if err := indexer.Update(update.GetObject()); err != nil {
					t.Fatalf("could not update index: %v", err)
				}
				return true, update.GetObject(), nil
			})
			set := newStatefulSet(3)
			set.GetObjectMeta().SetUID("set-123")
			pod0 := newStatefulSetPod(set, 0)
			claims0 := getPersistentVolumeClaims(set, pod0)
			for k := range claims0 {
				claim := claims0[k]
				if tc.ownerRef != nil {
					claim.SetOwnerReferences(tc.ownerRef)
				}
				if err := indexer.Add(&claim); err != nil {
					t.Errorf("Could not add claim %s: %v", k, err)
				}
			}
			control := NewStatefulPodControl(fakeClient, nil, claimLister, &noopRecorder{})
			set.Spec.PersistentVolumeClaimRetentionPolicy = &apps.StatefulSetPersistentVolumeClaimRetentionPolicy{
				WhenDeleted: apps.DeletePersistentVolumeClaimRetentionPolicyType,
				WhenScaled:  apps.RetainPersistentVolumeClaimRetentionPolicyType,
			}
			if utilfeature.DefaultFeatureGate.Enabled(features.StatefulSetAutoDeletePVC) {
				if err := control.UpdatePodClaimForRetentionPolicy(ctx, set, pod0); err != nil {
					t.Errorf("Unexpected error for UpdatePodClaimForRetentionPolicy (retain), pod0: %v", err)
				}
			}
			expectRef := tc.expectRef && utilfeature.DefaultFeatureGate.Enabled(features.StatefulSetAutoDeletePVC)
			for k := range claims0 {
				claim, err := claimLister.PersistentVolumeClaims(claims0[k].Namespace).Get(claims0[k].Name)
				if err != nil {
					t.Errorf("Unexpected error getting Claim %s/%s: %v", claim.Namespace, claim.Name, err)
				}
				if hasOwnerRef(claim, set) != expectRef {
					t.Errorf("%s: Claim %s/%s bad set owner ref", tc.name, claim.Namespace, claim.Name)
				}
			}
		}
	}
	t.Run("StatefulSetAutoDeletePVCEnabled", func(t *testing.T) {
		featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.StatefulSetAutoDeletePVC, true)
		testFn(t)
	})
	t.Run("StatefulSetAutoDeletePVCDisabled", func(t *testing.T) {
		// TODO: this will be removed in 1.35
		featuregatetesting.SetFeatureGateEmulationVersionDuringTest(t, utilfeature.DefaultFeatureGate, version.MustParse("1.31"))
		featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.StatefulSetAutoDeletePVC, false)
		testFn(t)
	})
}

func TestPodClaimIsStale(t *testing.T) {
	const missing = "missing"
	const exists = "exists"
	const stale = "stale"
	const withRef = "with-ref"
	testCases := []struct {
		name        string
		claimStates []string
		expected    bool
		skipPodUID  bool
	}{
		{
			name:        "all missing",
			claimStates: []string{missing, missing},
			expected:    false,
		},
		{
			name:        "no claims",
			claimStates: []string{},
			expected:    false,
		},
		{
			name:        "exists",
			claimStates: []string{missing, exists},
			expected:    false,
		},
		{
			name:        "all refs",
			claimStates: []string{withRef, withRef},
			expected:    false,
		},
		{
			name:        "stale & exists",
			claimStates: []string{stale, exists},
			expected:    true,
		},
		{
			name:        "stale & missing",
			claimStates: []string{stale, missing},
			expected:    true,
		},
		{
			name:        "withRef & stale",
			claimStates: []string{withRef, stale},
			expected:    true,
		},
		{
			name:        "withRef, no UID",
			claimStates: []string{withRef},
			skipPodUID:  true,
			expected:    true,
		},
	}
	for _, tc := range testCases {
		set := apps.StatefulSet{}
		set.Name = "set"
		set.Namespace = "default"
		set.Spec.PersistentVolumeClaimRetentionPolicy = &apps.StatefulSetPersistentVolumeClaimRetentionPolicy{
			WhenDeleted: apps.RetainPersistentVolumeClaimRetentionPolicyType,
			WhenScaled:  apps.DeletePersistentVolumeClaimRetentionPolicyType,
		}
		set.Spec.Selector = &metav1.LabelSelector{MatchLabels: map[string]string{"key": "value"}}
		claimIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
		for i, claimState := range tc.claimStates {
			claim := v1.PersistentVolumeClaim{}
			claim.Name = fmt.Sprintf("claim-%d", i)
			set.Spec.VolumeClaimTemplates = append(set.Spec.VolumeClaimTemplates, claim)
			claim.Name = fmt.Sprintf("%s-set-3", claim.Name)
			claim.Namespace = set.Namespace
			switch claimState {
			case missing:
			// Do nothing, the claim shouldn't exist.
			case exists:
				claimIndexer.Add(&claim)
			case stale:
				claim.SetOwnerReferences([]metav1.OwnerReference{
					{
						Name:       "set-3",
						UID:        types.UID("stale"),
						APIVersion: "v1",
						Kind:       "Pod",
					},
				})
				claimIndexer.Add(&claim)
			case withRef:
				claim.SetOwnerReferences([]metav1.OwnerReference{
					{
						Name:       "set-3",
						UID:        types.UID("123"),
						APIVersion: "v1",
						Kind:       "Pod",
					},
				})
				claimIndexer.Add(&claim)
			}
		}
		pod := v1.Pod{}
		pod.Name = "set-3"
		if !tc.skipPodUID {
			pod.SetUID("123")
		}
		claimLister := corelisters.NewPersistentVolumeClaimLister(claimIndexer)
		control := NewStatefulPodControl(&fake.Clientset{}, nil, claimLister, &noopRecorder{})
		expected := tc.expected
		// Note that the error isn't / can't be tested.
		if stale, _ := control.PodClaimIsStale(&set, &pod); stale != expected {
			t.Errorf("unexpected stale for %s", tc.name)
		}
	}
}

func TestStatefulPodControlRetainDeletionPolicyUpdate(t *testing.T) {
	testFn := func(t *testing.T) {
		_, ctx := ktesting.NewTestContext(t)
		recorder := record.NewFakeRecorder(10)
		set := newStatefulSet(1)
		set.Spec.PersistentVolumeClaimRetentionPolicy = &apps.StatefulSetPersistentVolumeClaimRetentionPolicy{
			WhenDeleted: apps.RetainPersistentVolumeClaimRetentionPolicyType,
			WhenScaled:  apps.RetainPersistentVolumeClaimRetentionPolicyType,
		}
		pod := newStatefulSetPod(set, 0)
		fakeClient := &fake.Clientset{}
		podIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
		podLister := corelisters.NewPodLister(podIndexer)
		claimIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
		claimLister := corelisters.NewPersistentVolumeClaimLister(claimIndexer)
		if err := podIndexer.Add(pod); err != nil {
			t.Errorf("couldn't add %s to index: %v", pod.GetName(), err)
		}
		claims := getPersistentVolumeClaims(set, pod)
		if len(claims) < 1 {
			t.Errorf("Unexpected missing PVCs")
		}
		for k := range claims {
			claim := claims[k]
			// This ownerRef should be removed in the update.
			claim.SetOwnerReferences(addControllerRef(claim.GetOwnerReferences(), set, controllerKind))
			if err := claimIndexer.Add(&claim); err != nil {
				t.Errorf("couldn't add %s to index: %v", claim.GetName(), err)
			}
		}
		control := NewStatefulPodControl(fakeClient, podLister, claimLister, recorder)
		if err := control.UpdateStatefulPod(ctx, set, pod); err != nil {
			t.Errorf("Successful update returned an error: %s", err)
		}
		for k := range claims {
			claim := claims[k]
			if hasOwnerRef(&claim, set) {
				t.Errorf("ownerRef not removed: %s/%s", claim.Namespace, claim.Name)
			}
		}
		events := collectEvents(recorder.Events)
		if utilfeature.DefaultFeatureGate.Enabled(features.StatefulSetAutoDeletePVC) {
			if eventCount := len(events); eventCount != 1 {
				t.Errorf("delete failed: got %d events, but want 1", eventCount)
			}
		} else {
			if len(events) != 0 {
				t.Errorf("delete failed: expected no events, but got %v", events)
			}
		}
	}
	t.Run("StatefulSetAutoDeletePVCEnabled", func(t *testing.T) {
		featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.StatefulSetAutoDeletePVC, true)
		testFn(t)
	})
	t.Run("StatefulSetAutoDeletePVCDisabled", func(t *testing.T) {
		// TODO: this will be removed in 1.35
		featuregatetesting.SetFeatureGateEmulationVersionDuringTest(t, utilfeature.DefaultFeatureGate, version.MustParse("1.31"))
		featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.StatefulSetAutoDeletePVC, false)
		testFn(t)
	})
}

func TestStatefulPodControlRetentionPolicyUpdate(t *testing.T) {
	_, ctx := ktesting.NewTestContext(t)

	recorder := record.NewFakeRecorder(10)
	set := newStatefulSet(1)
	set.Spec.PersistentVolumeClaimRetentionPolicy = &apps.StatefulSetPersistentVolumeClaimRetentionPolicy{
		WhenDeleted: apps.DeletePersistentVolumeClaimRetentionPolicyType,
		WhenScaled:  apps.RetainPersistentVolumeClaimRetentionPolicyType,
	}
	pod := newStatefulSetPod(set, 0)
	fakeClient := &fake.Clientset{}
	podIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
	claimIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
	podIndexer.Add(pod)
	claims := getPersistentVolumeClaims(set, pod)
	if len(claims) != 1 {
		t.Errorf("Unexpected or missing PVCs")
	}
	var claim v1.PersistentVolumeClaim
	for k := range claims {
		claim = claims[k]
		claimIndexer.Add(&claim)
	}
	fakeClient.AddReactor("update", "persistentvolumeclaims", func(action core.Action) (bool, runtime.Object, error) {
		update := action.(core.UpdateAction)
		claimIndexer.Update(update.GetObject())
		return true, update.GetObject(), nil
	})
	podLister := corelisters.NewPodLister(podIndexer)
	claimLister := corelisters.NewPersistentVolumeClaimLister(claimIndexer)
	control := NewStatefulPodControl(fakeClient, podLister, claimLister, recorder)
	if err := control.UpdateStatefulPod(ctx, set, pod); err != nil {
		t.Errorf("Successful update returned an error: %s", err)
	}
	updatedClaim, err := claimLister.PersistentVolumeClaims(claim.Namespace).Get(claim.Name)
	if err != nil {
		t.Errorf("Error retrieving claim %s/%s: %v", claim.Namespace, claim.Name, err)
	}
	if !hasOwnerRef(updatedClaim, set) {
		t.Errorf("ownerRef not added: %s/%s", claim.Namespace, claim.Name)
	}
	events := collectEvents(recorder.Events)
	if eventCount := len(events); eventCount != 1 {
		t.Errorf("update failed: got %d events, but want 1", eventCount)
	}
}

func TestStatefulPodControlRetentionPolicyUpdateMissingClaims(t *testing.T) {
	_, ctx := ktesting.NewTestContext(t)

	recorder := record.NewFakeRecorder(10)
	set := newStatefulSet(1)
	set.Spec.PersistentVolumeClaimRetentionPolicy = &apps.StatefulSetPersistentVolumeClaimRetentionPolicy{
		WhenDeleted: apps.DeletePersistentVolumeClaimRetentionPolicyType,
		WhenScaled:  apps.RetainPersistentVolumeClaimRetentionPolicyType,
	}
	pod := newStatefulSetPod(set, 0)
	fakeClient := &fake.Clientset{}
	podIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
	podLister := corelisters.NewPodLister(podIndexer)
	claimIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
	claimLister := corelisters.NewPersistentVolumeClaimLister(claimIndexer)
	podIndexer.Add(pod)
	fakeClient.AddReactor("update", "persistentvolumeclaims", func(action core.Action) (bool, runtime.Object, error) {
		update := action.(core.UpdateAction)
		claimIndexer.Update(update.GetObject())
		return true, update.GetObject(), nil
	})
	control := NewStatefulPodControl(fakeClient, podLister, claimLister, recorder)
	if err := control.UpdateStatefulPod(ctx, set, pod); err != nil {
		t.Error("Unexpected error on pod update when PVCs are missing")
	}
	claims := getPersistentVolumeClaims(set, pod)
	if len(claims) != 1 {
		t.Errorf("Unexpected or missing PVCs")
	}
	var claim v1.PersistentVolumeClaim
	for k := range claims {
		claim = claims[k]
		claimIndexer.Add(&claim)
	}

	if err := control.UpdateStatefulPod(ctx, set, pod); err != nil {
		t.Errorf("Expected update to succeed, saw error %v", err)
	}
	updatedClaim, err := claimLister.PersistentVolumeClaims(claim.Namespace).Get(claim.Name)
	if err != nil {
		t.Errorf("Error retrieving claim %s/%s: %v", claim.Namespace, claim.Name, err)
	}
	if !hasOwnerRef(updatedClaim, set) {
		t.Errorf("ownerRef not added: %s/%s", claim.Namespace, claim.Name)
	}
	events := collectEvents(recorder.Events)
	if eventCount := len(events); eventCount != 1 {
		t.Errorf("update failed: got %d events, but want 2", eventCount)
	}
	if !strings.Contains(events[0], "SuccessfulUpdate") {
		t.Errorf("expected first event to be a successful update: %s", events[1])
	}
}

func collectEvents(source <-chan string) []string {
	done := false
	events := make([]string, 0)
	for !done {
		select {
		case event := <-source:
			events = append(events, event)
		default:
			done = true
		}
	}
	return events
}
