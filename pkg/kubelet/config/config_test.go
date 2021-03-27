/*
Copyright 2014 The Kubernetes Authors.

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

package config

import (
	"context"
	"math/rand"
	"reflect"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	clientsetfake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"
	kubetypes "k8s.io/kubernetes/pkg/kubelet/types"
	"k8s.io/kubernetes/pkg/securitycontext"
	"k8s.io/kubernetes/test/utils/ktesting"
)

const (
	TestSource = "test"
)

func expectEmptyChannel(t *testing.T, ch <-chan interface{}) {
	select {
	case update := <-ch:
		t.Errorf("Expected no update in channel, Got %v", update)
	default:
	}
}

type sortedPods []*v1.Pod

func (s sortedPods) Len() int {
	return len(s)
}
func (s sortedPods) Swap(i, j int) {
	s[i], s[j] = s[j], s[i]
}
func (s sortedPods) Less(i, j int) bool {
	return s[i].Namespace < s[j].Namespace
}

type mockPodStartupSLIObserver struct{}

func (m *mockPodStartupSLIObserver) ObservedPodOnWatch(pod *v1.Pod, when time.Time) {}

func CreateValidPod(name, namespace string) *v1.Pod {
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			UID:       types.UID(name + namespace), // for the purpose of testing, this is unique enough
			Name:      name,
			Namespace: namespace,
		},
		Spec: v1.PodSpec{
			RestartPolicy: v1.RestartPolicyAlways,
			DNSPolicy:     v1.DNSClusterFirst,
			Containers: []v1.Container{
				{
					Name:                     "ctr",
					Image:                    "image",
					ImagePullPolicy:          "IfNotPresent",
					SecurityContext:          securitycontext.ValidSecurityContextWithContainerDefaults(),
					TerminationMessagePolicy: v1.TerminationMessageReadFile,
				},
			},
		},
	}
}

func CreatePodUpdate(op kubetypes.PodOperation, source string, pods ...*v1.Pod) kubetypes.PodUpdate {
	return kubetypes.PodUpdate{Pods: pods, Op: op, Source: source}
}

func createPodConfigTester(ctx context.Context, mode PodConfigNotificationMode) (chan<- interface{}, <-chan kubetypes.PodUpdate, *PodConfig) {
	client := clientsetfake.NewSimpleClientset()
	eventBroadcaster := events.NewBroadcaster(&events.EventSinkImpl{Interface: client.EventsV1()})
	config := NewPodConfig(mode, eventBroadcaster.NewRecorder(scheme.Scheme, "kubelet"), &mockPodStartupSLIObserver{})
	channel := config.Channel(ctx, TestSource)
	ch := config.Updates()
	return channel, ch, config
}

func expectPodUpdate(t *testing.T, ch <-chan kubetypes.PodUpdate, expected ...kubetypes.PodUpdate) {
	for i := range expected {
		update := <-ch
		sort.Sort(sortedPods(update.Pods))
		sort.Sort(sortedPods(expected[i].Pods))
		// Make copies of the expected/actual update to compare all fields
		// except for "Pods", which are compared separately below.
		expectedCopy, updateCopy := expected[i], update
		expectedCopy.Pods, updateCopy.Pods = nil, nil
		if !apiequality.Semantic.DeepEqual(expectedCopy, updateCopy) {
			t.Fatalf("Expected %#v, Got %#v", expectedCopy, updateCopy)
		}

		if len(expected[i].Pods) != len(update.Pods) {
			t.Fatalf("Expected %#v, Got %#v", expected[i], update)
		}
		// Compare pods one by one. This is necessary because we don't want to
		// compare local annotations.
		for j := range expected[i].Pods {
			if podsDifferSemantically(expected[i].Pods[j], update.Pods[j]) || !reflect.DeepEqual(expected[i].Pods[j].Status, update.Pods[j].Status) {
				t.Fatalf("Expected %#v, Got %#v", expected[i].Pods[j], update.Pods[j])
			}
		}
	}
	expectNoPodUpdate(t, ch)
}

func expectNoPodUpdate(t *testing.T, ch <-chan kubetypes.PodUpdate) {
	select {
	case update := <-ch:
		t.Errorf("Expected no update in channel, Got %#v", update)
	default:
	}
}

func TestNewPodAdded(t *testing.T) {
	tCtx := ktesting.Init(t)

	channel, ch, config := createPodConfigTester(tCtx, PodConfigNotificationIncremental)

	// see an update
	podUpdate := CreatePodUpdate(kubetypes.ADD, TestSource, CreateValidPod("foo", "new"))
	channel <- podUpdate
	expectPodUpdate(t, ch, CreatePodUpdate(kubetypes.ADD, TestSource, CreateValidPod("foo", "new")))

	config.Sync()
	expectPodUpdate(t, ch, CreatePodUpdate(kubetypes.SET, kubetypes.AllSource, CreateValidPod("foo", "new")))
}

func TestNewPodAddedInvalidNamespace(t *testing.T) {
	tCtx := ktesting.Init(t)

	channel, ch, config := createPodConfigTester(tCtx, PodConfigNotificationIncremental)

	// see an update
	podUpdate := CreatePodUpdate(kubetypes.ADD, TestSource, CreateValidPod("foo", ""))
	channel <- podUpdate
	expectPodUpdate(t, ch, CreatePodUpdate(kubetypes.ADD, TestSource, CreateValidPod("foo", "")))

	config.Sync()
	expectPodUpdate(t, ch, CreatePodUpdate(kubetypes.SET, kubetypes.AllSource, CreateValidPod("foo", "")))
}

func TestNewPodAddedDefaultNamespace(t *testing.T) {
	tCtx := ktesting.Init(t)

	channel, ch, config := createPodConfigTester(tCtx, PodConfigNotificationIncremental)

	// see an update
	podUpdate := CreatePodUpdate(kubetypes.ADD, TestSource, CreateValidPod("foo", "default"))
	channel <- podUpdate
	expectPodUpdate(t, ch, CreatePodUpdate(kubetypes.ADD, TestSource, CreateValidPod("foo", "default")))

	config.Sync()
	expectPodUpdate(t, ch, CreatePodUpdate(kubetypes.SET, kubetypes.AllSource, CreateValidPod("foo", "default")))
}

func TestNewPodAddedDifferentNamespaces(t *testing.T) {
	tCtx := ktesting.Init(t)

	channel, ch, config := createPodConfigTester(tCtx, PodConfigNotificationIncremental)

	// see an update
	podUpdate := CreatePodUpdate(kubetypes.ADD, TestSource, CreateValidPod("foo", "default"))
	channel <- podUpdate
	expectPodUpdate(t, ch, CreatePodUpdate(kubetypes.ADD, TestSource, CreateValidPod("foo", "default")))

	// see an update in another namespace
	podUpdate = CreatePodUpdate(kubetypes.ADD, TestSource, CreateValidPod("foo", "new"))
	channel <- podUpdate
	expectPodUpdate(t, ch, CreatePodUpdate(kubetypes.ADD, TestSource, CreateValidPod("foo", "new")))

	config.Sync()
	expectPodUpdate(t, ch, CreatePodUpdate(kubetypes.SET, kubetypes.AllSource, CreateValidPod("foo", "default"), CreateValidPod("foo", "new")))
}

func TestInvalidPodFiltered(t *testing.T) {
	tCtx := ktesting.Init(t)

	channel, ch, _ := createPodConfigTester(tCtx, PodConfigNotificationIncremental)

	// see an update
	podUpdate := CreatePodUpdate(kubetypes.ADD, TestSource, CreateValidPod("foo", "new"))
	channel <- podUpdate
	expectPodUpdate(t, ch, CreatePodUpdate(kubetypes.ADD, TestSource, CreateValidPod("foo", "new")))

	// add an invalid update, pod with the same name
	podUpdate = CreatePodUpdate(kubetypes.ADD, TestSource, CreateValidPod("foo", "new"))
	channel <- podUpdate
	expectNoPodUpdate(t, ch)
}

func TestNewPodAddedSnapshotAndUpdates(t *testing.T) {
	tCtx := ktesting.Init(t)

	channel, ch, config := createPodConfigTester(tCtx, PodConfigNotificationSnapshotAndUpdates)

	// see an set
	podUpdate := CreatePodUpdate(kubetypes.ADD, TestSource, CreateValidPod("foo", "new"))
	channel <- podUpdate
	expectPodUpdate(t, ch, CreatePodUpdate(kubetypes.SET, TestSource, CreateValidPod("foo", "new")))

	config.Sync()
	expectPodUpdate(t, ch, CreatePodUpdate(kubetypes.SET, kubetypes.AllSource, CreateValidPod("foo", "new")))

	// container updates are separated as UPDATE
	pod := *podUpdate.Pods[0]
	pod.Spec.Containers = []v1.Container{{Name: "bar", Image: "test", ImagePullPolicy: v1.PullIfNotPresent, TerminationMessagePolicy: v1.TerminationMessageReadFile}}
	channel <- CreatePodUpdate(kubetypes.ADD, TestSource, &pod)
	expectPodUpdate(t, ch, CreatePodUpdate(kubetypes.UPDATE, TestSource, &pod))
}

func TestNewPodAddedSnapshot(t *testing.T) {
	tCtx := ktesting.Init(t)

	channel, ch, config := createPodConfigTester(tCtx, PodConfigNotificationSnapshot)

	// see an set
	podUpdate := CreatePodUpdate(kubetypes.ADD, TestSource, CreateValidPod("foo", "new"))
	channel <- podUpdate
	expectPodUpdate(t, ch, CreatePodUpdate(kubetypes.SET, TestSource, CreateValidPod("foo", "new")))

	config.Sync()
	expectPodUpdate(t, ch, CreatePodUpdate(kubetypes.SET, kubetypes.AllSource, CreateValidPod("foo", "new")))

	// container updates are separated as UPDATE
	pod := *podUpdate.Pods[0]
	pod.Spec.Containers = []v1.Container{{Name: "bar", Image: "test", ImagePullPolicy: v1.PullIfNotPresent, TerminationMessagePolicy: v1.TerminationMessageReadFile}}
	channel <- CreatePodUpdate(kubetypes.ADD, TestSource, &pod)
	expectPodUpdate(t, ch, CreatePodUpdate(kubetypes.SET, TestSource, &pod))
}

func TestNewPodAddedUpdatedRemoved(t *testing.T) {
	tCtx := ktesting.Init(t)

	channel, ch, _ := createPodConfigTester(tCtx, PodConfigNotificationIncremental)

	// should register an add
	podUpdate := CreatePodUpdate(kubetypes.ADD, TestSource, CreateValidPod("foo", "new"))
	channel <- podUpdate
	expectPodUpdate(t, ch, CreatePodUpdate(kubetypes.ADD, TestSource, CreateValidPod("foo", "new")))

	// should ignore ADDs that are identical
	expectNoPodUpdate(t, ch)

	// an kubetypes.ADD should be converted to kubetypes.UPDATE
	pod := CreateValidPod("foo", "new")
	pod.Spec.Containers = []v1.Container{{Name: "bar", Image: "test", ImagePullPolicy: v1.PullIfNotPresent, TerminationMessagePolicy: v1.TerminationMessageReadFile}}
	podUpdate = CreatePodUpdate(kubetypes.ADD, TestSource, pod)
	channel <- podUpdate
	expectPodUpdate(t, ch, CreatePodUpdate(kubetypes.UPDATE, TestSource, pod))

	podUpdate = CreatePodUpdate(kubetypes.REMOVE, TestSource, CreateValidPod("foo", "new"))
	channel <- podUpdate
	expectPodUpdate(t, ch, CreatePodUpdate(kubetypes.REMOVE, TestSource, pod))
}

func TestNewPodAddedDelete(t *testing.T) {
	tCtx := ktesting.Init(t)

	channel, ch, _ := createPodConfigTester(tCtx, PodConfigNotificationIncremental)

	// should register an add
	addedPod := CreateValidPod("foo", "new")
	podUpdate := CreatePodUpdate(kubetypes.ADD, TestSource, addedPod)
	channel <- podUpdate
	expectPodUpdate(t, ch, CreatePodUpdate(kubetypes.ADD, TestSource, addedPod))

	// mark this pod as deleted
	timestamp := metav1.NewTime(time.Now())
	deletedPod := CreateValidPod("foo", "new")
	deletedPod.ObjectMeta.DeletionTimestamp = &timestamp
	podUpdate = CreatePodUpdate(kubetypes.DELETE, TestSource, deletedPod)
	channel <- podUpdate
	// the existing pod should be gracefully deleted
	expectPodUpdate(t, ch, CreatePodUpdate(kubetypes.DELETE, TestSource, addedPod))
}

func TestNewPodAddedUpdatedSet(t *testing.T) {
	tCtx := ktesting.Init(t)

	channel, ch, _ := createPodConfigTester(tCtx, PodConfigNotificationIncremental)

	// should register an add
	podUpdate := CreatePodUpdate(kubetypes.ADD, TestSource, CreateValidPod("foo", "new"), CreateValidPod("foo2", "new"), CreateValidPod("foo3", "new"))
	channel <- podUpdate
	expectPodUpdate(t, ch, CreatePodUpdate(kubetypes.ADD, TestSource, CreateValidPod("foo", "new"), CreateValidPod("foo2", "new"), CreateValidPod("foo3", "new")))

	// should ignore ADDs that are identical
	expectNoPodUpdate(t, ch)

	// should be converted to an kubetypes.ADD, kubetypes.REMOVE, and kubetypes.UPDATE
	pod := CreateValidPod("foo2", "new")
	pod.Spec.Containers = []v1.Container{{Name: "bar", Image: "test", ImagePullPolicy: v1.PullIfNotPresent, TerminationMessagePolicy: v1.TerminationMessageReadFile}}
	podUpdate = CreatePodUpdate(kubetypes.SET, TestSource, pod, CreateValidPod("foo3", "new"), CreateValidPod("foo4", "new"))
	channel <- podUpdate
	expectPodUpdate(t, ch,
		CreatePodUpdate(kubetypes.REMOVE, TestSource, CreateValidPod("foo", "new")),
		CreatePodUpdate(kubetypes.ADD, TestSource, CreateValidPod("foo4", "new")),
		CreatePodUpdate(kubetypes.UPDATE, TestSource, pod))
}

func TestNewPodAddedSetReconciled(t *testing.T) {
	tCtx := ktesting.Init(t)

	// Create and touch new test pods, return the new pods and touched pod. We should create new pod list
	// before touching to avoid data race.
	newTestPods := func(touchStatus, touchSpec bool) ([]*v1.Pod, *v1.Pod) {
		pods := []*v1.Pod{
			CreateValidPod("changeable-pod-0", "new"),
			CreateValidPod("constant-pod-1", "new"),
			CreateValidPod("constant-pod-2", "new"),
		}
		if touchStatus {
			pods[0].Status = v1.PodStatus{Message: strconv.Itoa(rand.Int())}
		}
		if touchSpec {
			pods[0].Spec.Containers[0].Name = strconv.Itoa(rand.Int())
		}
		return pods, pods[0]
	}
	for _, op := range []kubetypes.PodOperation{
		kubetypes.ADD,
		kubetypes.SET,
	} {
		var podWithStatusChange *v1.Pod
		pods, _ := newTestPods(false, false)
		channel, ch, _ := createPodConfigTester(tCtx, PodConfigNotificationIncremental)

		// Use SET to initialize the config, especially initialize the source set
		channel <- CreatePodUpdate(kubetypes.SET, TestSource, pods...)
		expectPodUpdate(t, ch, CreatePodUpdate(kubetypes.ADD, TestSource, pods...))

		// If status is not changed, no reconcile should be triggered
		channel <- CreatePodUpdate(op, TestSource, pods...)
		expectNoPodUpdate(t, ch)

		// If the pod status is changed and not updated, a reconcile should be triggered
		pods, podWithStatusChange = newTestPods(true, false)
		channel <- CreatePodUpdate(op, TestSource, pods...)
		expectPodUpdate(t, ch, CreatePodUpdate(kubetypes.RECONCILE, TestSource, podWithStatusChange))

		// If the pod status is changed, but the pod is also updated, no reconcile should be triggered
		pods, podWithStatusChange = newTestPods(true, true)
		channel <- CreatePodUpdate(op, TestSource, pods...)
		expectPodUpdate(t, ch, CreatePodUpdate(kubetypes.UPDATE, TestSource, podWithStatusChange))
	}
}

func TestInitialEmptySet(t *testing.T) {
	tCtx := ktesting.Init(t)

	for _, test := range []struct {
		mode PodConfigNotificationMode
		op   kubetypes.PodOperation
	}{
		{PodConfigNotificationIncremental, kubetypes.ADD},
		{PodConfigNotificationSnapshot, kubetypes.SET},
		{PodConfigNotificationSnapshotAndUpdates, kubetypes.SET},
	} {
		channel, ch, _ := createPodConfigTester(tCtx, test.mode)

		// should register an empty PodUpdate operation
		podUpdate := CreatePodUpdate(kubetypes.SET, TestSource)
		channel <- podUpdate
		expectPodUpdate(t, ch, CreatePodUpdate(test.op, TestSource))

		// should ignore following empty sets
		podUpdate = CreatePodUpdate(kubetypes.SET, TestSource)
		channel <- podUpdate
		podUpdate = CreatePodUpdate(kubetypes.ADD, TestSource, CreateValidPod("foo", "new"))
		channel <- podUpdate
		expectPodUpdate(t, ch, CreatePodUpdate(test.op, TestSource, CreateValidPod("foo", "new")))
	}
}

func TestPodUpdateAnnotations(t *testing.T) {
	tCtx := ktesting.Init(t)

	channel, ch, _ := createPodConfigTester(tCtx, PodConfigNotificationIncremental)

	pod := CreateValidPod("foo2", "new")
	pod.Annotations = make(map[string]string)
	pod.Annotations["kubernetes.io/blah"] = "blah"

	clone := pod.DeepCopy()

	podUpdate := CreatePodUpdate(kubetypes.SET, TestSource, CreateValidPod("foo1", "new"), clone, CreateValidPod("foo3", "new"))
	channel <- podUpdate
	expectPodUpdate(t, ch, CreatePodUpdate(kubetypes.ADD, TestSource, CreateValidPod("foo1", "new"), pod, CreateValidPod("foo3", "new")))

	pod.Annotations["kubernetes.io/blah"] = "superblah"
	podUpdate = CreatePodUpdate(kubetypes.SET, TestSource, CreateValidPod("foo1", "new"), pod, CreateValidPod("foo3", "new"))
	channel <- podUpdate
	expectPodUpdate(t, ch, CreatePodUpdate(kubetypes.UPDATE, TestSource, pod))

	pod.Annotations["kubernetes.io/otherblah"] = "doh"
	podUpdate = CreatePodUpdate(kubetypes.SET, TestSource, CreateValidPod("foo1", "new"), pod, CreateValidPod("foo3", "new"))
	channel <- podUpdate
	expectPodUpdate(t, ch, CreatePodUpdate(kubetypes.UPDATE, TestSource, pod))

	delete(pod.Annotations, "kubernetes.io/blah")
	podUpdate = CreatePodUpdate(kubetypes.SET, TestSource, CreateValidPod("foo1", "new"), pod, CreateValidPod("foo3", "new"))
	channel <- podUpdate
	expectPodUpdate(t, ch, CreatePodUpdate(kubetypes.UPDATE, TestSource, pod))
}

func TestPodUpdateLabels(t *testing.T) {
	tCtx := ktesting.Init(t)

	channel, ch, _ := createPodConfigTester(tCtx, PodConfigNotificationIncremental)

	pod := CreateValidPod("foo2", "new")
	pod.Labels = make(map[string]string)
	pod.Labels["key"] = "value"

	clone := pod.DeepCopy()

	podUpdate := CreatePodUpdate(kubetypes.SET, TestSource, clone)
	channel <- podUpdate
	expectPodUpdate(t, ch, CreatePodUpdate(kubetypes.ADD, TestSource, pod))

	pod.Labels["key"] = "newValue"
	podUpdate = CreatePodUpdate(kubetypes.SET, TestSource, pod)
	channel <- podUpdate
	expectPodUpdate(t, ch, CreatePodUpdate(kubetypes.UPDATE, TestSource, pod))

}

func TestPodConfigRace(t *testing.T) {
	tCtx := ktesting.Init(t)

	client := clientsetfake.NewSimpleClientset()
	eventBroadcaster := events.NewBroadcaster(&events.EventSinkImpl{Interface: client.EventsV1()})
	config := NewPodConfig(PodConfigNotificationIncremental, eventBroadcaster.NewRecorder(scheme.Scheme, "kubelet"), &mockPodStartupSLIObserver{})
	seenSources := sets.New[string](TestSource)
	var wg sync.WaitGroup
	const iterations = 100
	wg.Add(2)

	go func() {
		ctx, cancel := context.WithCancel(tCtx)
		defer cancel()
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			config.Channel(ctx, strconv.Itoa(i))
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			config.SeenAllSources(seenSources)
		}
	}()

	wg.Wait()
}
