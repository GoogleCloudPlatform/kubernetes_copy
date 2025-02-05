/*
Copyright 2017 The Kubernetes Authors.

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

package pvcprotection

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/dump"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
	"k8s.io/klog/v2/ktesting"
	"k8s.io/kubernetes/pkg/controller"
	volumeutil "k8s.io/kubernetes/pkg/volume/util"
)

type reaction struct {
	verb      string
	resource  string
	reactorfn clienttesting.ReactionFunc
}

const (
	defaultNS       = "default"
	namespace2      = "namespace-2"
	defaultPVCName  = "pvc1"
	defaultPodName  = "pod1"
	defaultNodeName = "node1"
	defaultUID      = "uid1"
)

func pod() *v1.Pod {
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      defaultPodName,
			Namespace: defaultNS,
			UID:       defaultUID,
		},
		Spec: v1.PodSpec{
			NodeName: defaultNodeName,
		},
		Status: v1.PodStatus{
			Phase: v1.PodPending,
		},
	}
}

func podWithConfig(name string, namespace string) *v1.Pod {
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			UID:       defaultUID,
		},
		Spec: v1.PodSpec{
			NodeName: defaultNodeName,
		},
		Status: v1.PodStatus{
			Phase: v1.PodPending,
		},
	}
}

func unscheduled(pod *v1.Pod) *v1.Pod {
	pod.Spec.NodeName = ""
	return pod
}

func withPVC(pvcName string, pod *v1.Pod) *v1.Pod {
	volume := v1.Volume{
		Name: pvcName,
		VolumeSource: v1.VolumeSource{
			PersistentVolumeClaim: &v1.PersistentVolumeClaimVolumeSource{
				ClaimName: pvcName,
			},
		},
	}
	pod.Spec.Volumes = append(pod.Spec.Volumes, volume)
	return pod
}

func withEmptyDir(pod *v1.Pod) *v1.Pod {
	volume := v1.Volume{
		Name: "emptyDir",
		VolumeSource: v1.VolumeSource{
			EmptyDir: &v1.EmptyDirVolumeSource{},
		},
	}
	pod.Spec.Volumes = append(pod.Spec.Volumes, volume)
	return pod
}

func withStatus(phase v1.PodPhase, pod *v1.Pod) *v1.Pod {
	pod.Status.Phase = phase
	return pod
}

func withUID(uid types.UID, pod *v1.Pod) *v1.Pod {
	pod.ObjectMeta.UID = uid
	return pod
}

func pvc() *v1.PersistentVolumeClaim {
	return &v1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      defaultPVCName,
			Namespace: defaultNS,
		},
	}
}

func pvcWithConfig(name string, namespace string) *v1.PersistentVolumeClaim {
	return &v1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
	}
}

func withProtectionFinalizer(pvc *v1.PersistentVolumeClaim) *v1.PersistentVolumeClaim {
	pvc.Finalizers = append(pvc.Finalizers, volumeutil.PVCProtectionFinalizer)
	return pvc
}

func deleted(pvc *v1.PersistentVolumeClaim) *v1.PersistentVolumeClaim {
	pvc.DeletionTimestamp = &metav1.Time{}
	return pvc
}

func generateUpdateErrorFunc(t *testing.T, failures int) clienttesting.ReactionFunc {
	i := 0
	return func(action clienttesting.Action) (bool, runtime.Object, error) {
		i++
		if i <= failures {
			// Update fails
			update, ok := action.(clienttesting.UpdateAction)

			if !ok {
				t.Fatalf("Reactor got non-update action: %+v", action)
			}
			acc, _ := meta.Accessor(update.GetObject())
			return true, nil, apierrors.NewForbidden(update.GetResource().GroupResource(), acc.GetName(), errors.New("Mock error"))
		}
		// Update succeeds
		return false, nil, nil
	}
}

func generatePodListErrorFunc(t *testing.T, failures int) clienttesting.ReactionFunc {
	i := 0
	return func(action clienttesting.Action) (bool, runtime.Object, error) {
		i++
		if i <= failures {
			// Pod List fails
			list, ok := action.(clienttesting.ListAction)
			if !ok {
				t.Fatalf("Reactor got non-list action: %+v", action)
			}
			return true, nil, apierrors.NewForbidden(list.GetResource().GroupResource(), "mock pod", errors.New("Mock error"))
		}
		// List succeeds
		return false, nil, nil
	}
}

func TestPVCProtectionController(t *testing.T) {
	pvcGVR := schema.GroupVersionResource{
		Group:    v1.GroupName,
		Version:  "v1",
		Resource: "persistentvolumeclaims",
	}
	podGVR := schema.GroupVersionResource{
		Group:    v1.GroupName,
		Version:  "v1",
		Resource: "pods",
	}
	podGVK := schema.GroupVersionKind{
		Group:   v1.GroupName,
		Version: "v1",
		Kind:    "Pod",
	}

	tests := []struct {
		name string
		// Object to insert into fake kubeclient before the test starts.
		initialObjects []runtime.Object
		// Whether not to insert the content of initialObjects into the
		// informers before the test starts. Set it to true to simulate the case
		// where informers have not been notified yet of certain API objects.
		informersAreLate bool
		// Optional client reactors.
		reactors []reaction
		// PVC event to simulate. This PVC will be automatically added to
		// initialObjects.
		updatedPVCs []*v1.PersistentVolumeClaim
		// Pod event to simulate. This Pod will be automatically added to
		// initialObjects.
		updatedPod *v1.Pod
		// Pod event to simulate. This Pod is *not* added to
		// initialObjects.
		deletedPod *v1.Pod
		// List of expected kubeclient actions that should happen during the
		// test.
		expectedActions []clienttesting.Action
	}{
		//
		// PVC events
		//
		{
			name:        "PVC without finalizer -> finalizer is added",
			updatedPVCs: []*v1.PersistentVolumeClaim{pvc(), pvcWithConfig("pvc2", "namespace-2")},
			expectedActions: []clienttesting.Action{
				clienttesting.NewUpdateAction(pvcGVR, defaultNS, withProtectionFinalizer(pvc())),
				clienttesting.NewUpdateAction(pvcGVR, "namespace-2", withProtectionFinalizer(pvcWithConfig("pvc2", "namespace-2"))),
			},
		},
		{
			name:            "PVC with finalizer -> no action",
			updatedPVCs:     []*v1.PersistentVolumeClaim{withProtectionFinalizer(pvc()), withProtectionFinalizer(pvcWithConfig("pvc2", "namespace-2"))},
			expectedActions: []clienttesting.Action{},
		},
		{
			name:        "saving PVC finalizer fails -> controller retries",
			updatedPVCs: []*v1.PersistentVolumeClaim{pvc()},
			reactors: []reaction{
				{
					verb:      "update",
					resource:  "persistentvolumeclaims",
					reactorfn: generateUpdateErrorFunc(t, 2 /* update fails twice*/),
				},
			},
			expectedActions: []clienttesting.Action{
				// This fails
				clienttesting.NewUpdateAction(pvcGVR, defaultNS, withProtectionFinalizer(pvc())),
				// This fails too
				clienttesting.NewUpdateAction(pvcGVR, defaultNS, withProtectionFinalizer(pvc())),
				// This succeeds
				clienttesting.NewUpdateAction(pvcGVR, defaultNS, withProtectionFinalizer(pvc())),
			},
		},
		{
			name: "deleted PVC with finalizers across different namespaces -> finalizer is removed",
			updatedPVCs: []*v1.PersistentVolumeClaim{deleted(withProtectionFinalizer(pvc())),
				deleted(withProtectionFinalizer(pvcWithConfig("pvc2", "namespace-2")))},
			expectedActions: []clienttesting.Action{
				clienttesting.NewListAction(podGVR, podGVK, defaultNS, metav1.ListOptions{}),
				clienttesting.NewUpdateAction(pvcGVR, defaultNS, deleted(pvc())),
				clienttesting.NewListAction(podGVR, podGVK, "namespace-2", metav1.ListOptions{}),
				clienttesting.NewUpdateAction(pvcGVR, "namespace-2", deleted(pvcWithConfig("pvc2", "namespace-2"))),
			},
		},
		{
			name: "multiple PVCs with finalizer for the same namespace; no alive pods -> finalizer is removed",
			updatedPVCs: []*v1.PersistentVolumeClaim{deleted(withProtectionFinalizer(pvc())),
				deleted(withProtectionFinalizer(pvcWithConfig("pvc2", defaultNS)))},
			expectedActions: []clienttesting.Action{
				clienttesting.NewListAction(podGVR, podGVK, defaultNS, metav1.ListOptions{}),
				clienttesting.NewUpdateAction(pvcGVR, defaultNS, deleted(pvc())),
				clienttesting.NewListAction(podGVR, podGVK, defaultNS, metav1.ListOptions{}),
				clienttesting.NewUpdateAction(pvcGVR, defaultNS, deleted(pvcWithConfig("pvc2", defaultNS))),
			},
		},
		{
			name:        "finalizer removal fails -> controller retries",
			updatedPVCs: []*v1.PersistentVolumeClaim{deleted(withProtectionFinalizer(pvc()))},
			reactors: []reaction{
				{
					verb:      "update",
					resource:  "persistentvolumeclaims",
					reactorfn: generateUpdateErrorFunc(t, 2 /* update fails twice*/),
				},
			},
			expectedActions: []clienttesting.Action{
				clienttesting.NewListAction(podGVR, podGVK, defaultNS, metav1.ListOptions{}),
				// Fails
				clienttesting.NewUpdateAction(pvcGVR, defaultNS, deleted(pvc())),
				clienttesting.NewListAction(podGVR, podGVK, defaultNS, metav1.ListOptions{}),
				// Fails too
				clienttesting.NewUpdateAction(pvcGVR, defaultNS, deleted(pvc())),
				clienttesting.NewListAction(podGVR, podGVK, defaultNS, metav1.ListOptions{}),
				// Succeeds
				clienttesting.NewUpdateAction(pvcGVR, defaultNS, deleted(pvc())),
			},
		},
		{
			name:        "delete multiple PVCs of the same namespace; pod list fails for one PVC -> add failing PVC back to queue and continue to the next PVC",
			updatedPVCs: []*v1.PersistentVolumeClaim{deleted(withProtectionFinalizer(pvc())), deleted(withProtectionFinalizer(pvcWithConfig("pvc2", defaultNS)))},
			reactors: []reaction{
				{
					verb:      "list",
					resource:  "pods",
					reactorfn: generatePodListErrorFunc(t, 1 /* update fails twice*/),
				},
			},
			expectedActions: []clienttesting.Action{
				// Fails
				clienttesting.NewListAction(podGVR, podGVK, defaultNS, metav1.ListOptions{}),
				// Succeed with next pvc in queue
				clienttesting.NewListAction(podGVR, podGVK, defaultNS, metav1.ListOptions{}),
				clienttesting.NewUpdateAction(pvcGVR, defaultNS, deleted(pvcWithConfig("pvc2", defaultNS))),

				clienttesting.NewListAction(podGVR, podGVK, defaultNS, metav1.ListOptions{}),
				clienttesting.NewUpdateAction(pvcGVR, defaultNS, deleted(pvc())),
			},
		},
		{
			name: "deleted PVC with finalizer + pod with the PVC exists -> finalizer is not removed",
			initialObjects: []runtime.Object{
				withPVC(defaultPVCName, pod()),
			},
			updatedPVCs:     []*v1.PersistentVolumeClaim{deleted(withProtectionFinalizer(pvc()))},
			expectedActions: []clienttesting.Action{},
		},
		{
			name: "deleted PVC with finalizer + pod with unrelated PVC and EmptyDir exists -> finalizer is removed",
			initialObjects: []runtime.Object{
				withEmptyDir(withPVC("unrelatedPVC", pod())),
			},
			updatedPVCs: []*v1.PersistentVolumeClaim{deleted(withProtectionFinalizer(pvc()))},
			expectedActions: []clienttesting.Action{
				clienttesting.NewListAction(podGVR, podGVK, defaultNS, metav1.ListOptions{}),
				clienttesting.NewUpdateAction(pvcGVR, defaultNS, deleted(pvc())),
			},
		},
		{
			name: "deleted multiple PVCs with finalizer (same namespace) + pod with unrelated PVC and EmptyDir exists",
			initialObjects: []runtime.Object{
				withEmptyDir(withPVC("unrelatedPVC", pod())),
			},
			updatedPVCs: []*v1.PersistentVolumeClaim{deleted(withProtectionFinalizer(pvc())),
				deleted(withProtectionFinalizer(pvcWithConfig("pvc2", defaultNS))),
				deleted(withProtectionFinalizer(pvcWithConfig("pvc3", defaultNS))),
			},
			expectedActions: []clienttesting.Action{
				clienttesting.NewListAction(podGVR, podGVK, defaultNS, metav1.ListOptions{}),
				clienttesting.NewUpdateAction(pvcGVR, defaultNS, deleted(pvc())),
				clienttesting.NewListAction(podGVR, podGVK, defaultNS, metav1.ListOptions{}),
				clienttesting.NewUpdateAction(pvcGVR, defaultNS, deleted(pvcWithConfig("pvc2", defaultNS))),
				clienttesting.NewListAction(podGVR, podGVK, defaultNS, metav1.ListOptions{}),
				clienttesting.NewUpdateAction(pvcGVR, defaultNS, deleted(pvcWithConfig("pvc3", defaultNS))),
			},
		},
		{
			name: "deleted PVC with finalizer + pod with the PVC finished but is not deleted -> finalizer is not removed",
			initialObjects: []runtime.Object{
				withStatus(v1.PodFailed, withPVC(defaultPVCName, pod())),
			},
			updatedPVCs:     []*v1.PersistentVolumeClaim{deleted(withProtectionFinalizer(pvc()))},
			expectedActions: []clienttesting.Action{},
		},
		{
			name: "deleted PVC with finalizer + pod with the PVC exists but is not in the Informer's cache yet -> finalizer is not removed",
			initialObjects: []runtime.Object{
				withPVC(defaultPVCName, pod()),
			},
			informersAreLate: true,
			updatedPVCs:      []*v1.PersistentVolumeClaim{deleted(withProtectionFinalizer(pvc()))},
			expectedActions: []clienttesting.Action{
				clienttesting.NewListAction(podGVR, podGVK, defaultNS, metav1.ListOptions{}),
			},
		},

		{
			name: "mix of deleted PVCs with some used by a pod and some unused -> finalizer is removed only for unused PVCs",
			initialObjects: []runtime.Object{
				withPVC(defaultPVCName, pod()),
				withPVC("pvc3", podWithConfig("pod2", "namespace-3")),
			},
			informersAreLate: true,
			updatedPVCs:      []*v1.PersistentVolumeClaim{deleted(withProtectionFinalizer(pvc())), deleted(withProtectionFinalizer(pvcWithConfig("pvc2", defaultNS))), deleted(withProtectionFinalizer(pvcWithConfig("pvc3", "namespace-3")))},
			expectedActions: []clienttesting.Action{
				clienttesting.NewListAction(podGVR, podGVK, defaultNS, metav1.ListOptions{}),
				clienttesting.NewListAction(podGVR, podGVK, defaultNS, metav1.ListOptions{}),
				clienttesting.NewUpdateAction(pvcGVR, defaultNS, deleted(pvcWithConfig("pvc2", defaultNS))),
				clienttesting.NewListAction(podGVR, podGVK, "namespace-3", metav1.ListOptions{}),
			},
		},
		//
		// Pod events
		//
		{
			name: "updated running Pod -> no action",
			initialObjects: []runtime.Object{
				deleted(withProtectionFinalizer(pvc())),
			},
			updatedPod:      withStatus(v1.PodRunning, withPVC(defaultPVCName, pod())),
			expectedActions: []clienttesting.Action{},
		},
		{
			name: "updated finished Pod -> finalizer is not removed",
			initialObjects: []runtime.Object{
				deleted(withProtectionFinalizer(pvc())),
			},
			updatedPod:      withStatus(v1.PodSucceeded, withPVC(defaultPVCName, pod())),
			expectedActions: []clienttesting.Action{},
		},
		{
			name: "updated unscheduled Pod -> finalizer is removed",
			initialObjects: []runtime.Object{
				deleted(withProtectionFinalizer(pvc())),
			},
			updatedPod: unscheduled(withPVC(defaultPVCName, pod())),
			expectedActions: []clienttesting.Action{
				clienttesting.NewListAction(podGVR, podGVK, defaultNS, metav1.ListOptions{}),
				clienttesting.NewUpdateAction(pvcGVR, defaultNS, deleted(pvc())),
			},
		},
		{
			name: "deleted running Pod -> finalizer is removed",
			initialObjects: []runtime.Object{
				deleted(withProtectionFinalizer(pvc())),
			},
			deletedPod: withStatus(v1.PodRunning, withPVC(defaultPVCName, pod())),
			expectedActions: []clienttesting.Action{
				clienttesting.NewListAction(podGVR, podGVK, defaultNS, metav1.ListOptions{}),
				clienttesting.NewUpdateAction(pvcGVR, defaultNS, deleted(pvc())),
			},
		},
		{
			name: "pod delete and create with same namespaced name seen as an update, old pod used deleted PVC -> finalizer is removed",
			initialObjects: []runtime.Object{
				deleted(withProtectionFinalizer(pvc())),
			},
			deletedPod: withPVC(defaultPVCName, pod()),
			updatedPod: withUID("uid2", pod()),
			expectedActions: []clienttesting.Action{
				clienttesting.NewListAction(podGVR, podGVK, defaultNS, metav1.ListOptions{}),
				clienttesting.NewUpdateAction(pvcGVR, defaultNS, deleted(pvc())),
			},
		},
		{
			name: "pod delete and create with same namespaced name seen as an update, old pod used non-deleted PVC -> finalizer is not removed",
			initialObjects: []runtime.Object{
				withProtectionFinalizer(pvc()),
			},
			deletedPod:      withPVC(defaultPVCName, pod()),
			updatedPod:      withUID("uid2", pod()),
			expectedActions: []clienttesting.Action{},
		},
		{
			name: "pod delete and create with same namespaced name seen as an update, both pods reference deleted PVC -> finalizer is not removed",
			initialObjects: []runtime.Object{
				deleted(withProtectionFinalizer(pvc())),
			},
			deletedPod:      withPVC(defaultPVCName, pod()),
			updatedPod:      withUID("uid2", withPVC(defaultPVCName, pod())),
			expectedActions: []clienttesting.Action{},
		},
		{
			name: "pod update from unscheduled to scheduled, deleted PVC is referenced -> finalizer is not removed",
			initialObjects: []runtime.Object{
				deleted(withProtectionFinalizer(pvc())),
			},
			deletedPod:      unscheduled(withPVC(defaultPVCName, pod())),
			updatedPod:      withPVC(defaultPVCName, pod()),
			expectedActions: []clienttesting.Action{},
		},
	}

	for _, test := range tests {
		// Create initial data for client and informers.
		var (
			clientObjs    []runtime.Object
			informersObjs []runtime.Object
		)
		if test.updatedPVCs != nil {
			for i := 0; i < len(test.updatedPVCs); i++ {
				clientObjs = append(clientObjs, test.updatedPVCs[i])
				informersObjs = append(informersObjs, test.updatedPVCs[i])
			}
		}
		if test.updatedPod != nil {
			clientObjs = append(clientObjs, test.updatedPod)
			informersObjs = append(informersObjs, test.updatedPod)
		}
		clientObjs = append(clientObjs, test.initialObjects...)
		if !test.informersAreLate {
			informersObjs = append(informersObjs, test.initialObjects...)
		}
		// Create client with initial data
		client := fake.NewSimpleClientset(clientObjs...)

		// Create informers
		informers := informers.NewSharedInformerFactory(client, controller.NoResyncPeriodFunc())
		pvcInformer := informers.Core().V1().PersistentVolumeClaims()
		podInformer := informers.Core().V1().Pods()

		// Create the controller
		logger, _ := ktesting.NewTestContext(t)
		ctrl, err := NewPVCProtectionController(logger, pvcInformer, podInformer, client)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		// Populate the informers with initial objects so the controller can
		// Get() and List() it.
		for _, obj := range informersObjs {
			switch obj.(type) {
			case *v1.PersistentVolumeClaim:
				pvcInformer.Informer().GetStore().Add(obj)
			case *v1.Pod:
				podInformer.Informer().GetStore().Add(obj)
			default:
				t.Fatalf("Unknown initialObject type: %+v", obj)
			}
		}

		// Add reactor to inject test errors.
		for _, reactor := range test.reactors {
			client.Fake.PrependReactor(reactor.verb, reactor.resource, reactor.reactorfn)
		}

		// Start the test by simulating an event
		if test.updatedPVCs != nil {
			for i := 0; i < len(test.updatedPVCs); i++ {
				ctrl.pvcAddedUpdated(logger, test.updatedPVCs[i])
			}
		}
		switch {
		case test.deletedPod != nil && test.updatedPod != nil && test.deletedPod.Namespace == test.updatedPod.Namespace && test.deletedPod.Name == test.updatedPod.Name:
			ctrl.podAddedDeletedUpdated(logger, test.deletedPod, test.updatedPod, false)
		case test.updatedPod != nil:
			ctrl.podAddedDeletedUpdated(logger, nil, test.updatedPod, false)
		case test.deletedPod != nil:
			ctrl.podAddedDeletedUpdated(logger, nil, test.deletedPod, true)
		}

		// Process the controller queue until we get expected results
		timeout := time.Now().Add(10 * time.Second)
		lastReportedActionCount := 0
		for {
			if time.Now().After(timeout) {
				t.Errorf("Test %q: timed out", test.name)
				break
			}
			if ctrl.queue.Len() > 0 {
				logger.V(5).Info("Non-empty queue, processing one", "test", test.name, "queueLength", ctrl.queue.Len())
				ctx := context.TODO()
				ctrl.processNextWorkItem()
				for ctrl.pvcProcessingStore.namespaceQueue.Len() != 0 {
					ctrl.processPVCsByNamespace(ctx)
				}
			}

			if ctrl.queue.Len() > 0 {
				// There is still some work in the queue, process it now
				continue
			}

			currentActionCount := len(client.Actions())
			if currentActionCount < len(test.expectedActions) {
				// Do not log every wait, only when the action count changes.
				if lastReportedActionCount < currentActionCount {
					logger.V(5).Info("Waiting for the remaining actions", "test", test.name, "currentActionCount", currentActionCount, "expectedActionCount", len(test.expectedActions))
					lastReportedActionCount = currentActionCount
				}
				// The test expected more to happen, wait for the actions.
				// Most probably it's exponential backoff
				time.Sleep(10 * time.Millisecond)
				continue
			}
			break
		}
		actions := client.Actions()
		for i, action := range actions {
			if len(test.expectedActions) < i+1 {
				t.Errorf("Test %q: %d unexpected actions: %+v", test.name, len(actions)-len(test.expectedActions), dump.Pretty(actions[i:]))
				break
			}

			expectedAction := test.expectedActions[i]
			if !reflect.DeepEqual(expectedAction, action) {
				t.Errorf("Test %q: action %d\nExpected:\n%s\ngot:\n%s", test.name, i, dump.Pretty(expectedAction), dump.Pretty(action))
			}
		}

		if len(test.expectedActions) > len(actions) {
			t.Errorf("Test %q: %d additional expected actions", test.name, len(test.expectedActions)-len(actions))
			for _, a := range test.expectedActions[len(actions):] {
				t.Logf("    %+v", a)
			}
		}

	}
}
