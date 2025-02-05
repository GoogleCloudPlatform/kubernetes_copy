/*
Copyright 2015 The Kubernetes Authors.

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

package job

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	batch "k8s.io/api/batch/v1"
	v1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/uuid"
	utilversion "k8s.io/apimachinery/pkg/util/version"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/apiserver/pkg/util/feature"
	"k8s.io/client-go/informers"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	restclient "k8s.io/client-go/rest"
	core "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	metricstestutil "k8s.io/component-base/metrics/testutil"
	"k8s.io/klog/v2"
	"k8s.io/klog/v2/ktesting"
	_ "k8s.io/kubernetes/pkg/apis/core/install"
	"k8s.io/kubernetes/pkg/controller"
	"k8s.io/kubernetes/pkg/controller/job/metrics"
	"k8s.io/kubernetes/pkg/controller/testutil"
	"k8s.io/kubernetes/pkg/features"
	"k8s.io/utils/clock"
	clocktesting "k8s.io/utils/clock/testing"
	"k8s.io/utils/ptr"
)

var realClock = &clock.RealClock{}
var alwaysReady = func() bool { return true }

const fastSyncJobBatchPeriod = 10 * time.Millisecond
const fastJobApiBackoff = 10 * time.Millisecond
const fastRequeue = 10 * time.Millisecond

// testFinishedAt represents time one second later than unix epoch
// this will be used in various test cases where we don't want back-off to kick in
var testFinishedAt = metav1.NewTime((time.Time{}).Add(time.Second))

func newJobWithName(name string, parallelism, completions, backoffLimit int32, completionMode batch.CompletionMode) *batch.Job {
	j := &batch.Job{
		TypeMeta: metav1.TypeMeta{Kind: "Job"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			UID:       uuid.NewUUID(),
			Namespace: metav1.NamespaceDefault,
		},
		Spec: batch.JobSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"foo": "bar"},
			},
			Template: v1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"foo": "bar",
					},
				},
				Spec: v1.PodSpec{
					Containers: []v1.Container{
						{Image: "foo/bar"},
					},
				},
			},
		},
	}
	if completionMode != "" {
		j.Spec.CompletionMode = &completionMode
	}
	// Special case: -1 for either completions or parallelism means leave nil (negative is not allowed
	// in practice by validation.
	if completions >= 0 {
		j.Spec.Completions = &completions
	} else {
		j.Spec.Completions = nil
	}
	if parallelism >= 0 {
		j.Spec.Parallelism = &parallelism
	} else {
		j.Spec.Parallelism = nil
	}
	j.Spec.BackoffLimit = &backoffLimit

	return j
}

func newJob(parallelism, completions, backoffLimit int32, completionMode batch.CompletionMode) *batch.Job {
	return newJobWithName("foobar", parallelism, completions, backoffLimit, completionMode)
}

func newControllerFromClient(ctx context.Context, t *testing.T, kubeClient clientset.Interface, resyncPeriod controller.ResyncPeriodFunc) (*Controller, informers.SharedInformerFactory) {
	t.Helper()
	return newControllerFromClientWithClock(ctx, t, kubeClient, resyncPeriod, realClock)
}

func newControllerFromClientWithClock(ctx context.Context, t *testing.T, kubeClient clientset.Interface, resyncPeriod controller.ResyncPeriodFunc, clock clock.WithTicker) (*Controller, informers.SharedInformerFactory) {
	t.Helper()
	sharedInformers := informers.NewSharedInformerFactory(kubeClient, resyncPeriod())
	jm, err := newControllerWithClock(ctx, sharedInformers.Core().V1().Pods(), sharedInformers.Batch().V1().Jobs(), kubeClient, clock)
	if err != nil {
		t.Fatalf("Error creating Job controller: %v", err)
	}
	jm.podControl = &controller.FakePodControl{}
	return jm, sharedInformers
}

func newPod(name string, job *batch.Job) *v1.Pod {
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			UID:             types.UID(name),
			Labels:          job.Spec.Selector.MatchLabels,
			Namespace:       job.Namespace,
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(job, controllerKind)},
		},
	}
}

// create count pods with the given phase for the given job
func newPodList(count int, status v1.PodPhase, job *batch.Job) []*v1.Pod {
	var pods []*v1.Pod
	for i := 0; i < count; i++ {
		newPod := newPod(fmt.Sprintf("pod-%v", rand.String(10)), job)
		newPod.Status = v1.PodStatus{Phase: status}
		newPod.Status.ContainerStatuses = []v1.ContainerStatus{
			{
				State: v1.ContainerState{
					Terminated: &v1.ContainerStateTerminated{
						FinishedAt: testFinishedAt,
					},
				},
			},
		}
		newPod.Finalizers = append(newPod.Finalizers, batch.JobTrackingFinalizer)
		pods = append(pods, newPod)
	}
	return pods
}

func setPodsStatuses(podIndexer cache.Indexer, job *batch.Job, pendingPods, activePods, succeededPods, failedPods, terminatingPods, readyPods int) {
	for _, pod := range newPodList(pendingPods, v1.PodPending, job) {
		podIndexer.Add(pod)
	}
	running := newPodList(activePods, v1.PodRunning, job)
	for i, p := range running {
		if i >= readyPods {
			break
		}
		p.Status.Conditions = append(p.Status.Conditions, v1.PodCondition{
			Type:   v1.PodReady,
			Status: v1.ConditionTrue,
		})
	}
	for _, pod := range running {
		podIndexer.Add(pod)
	}
	for _, pod := range newPodList(succeededPods, v1.PodSucceeded, job) {
		podIndexer.Add(pod)
	}
	for _, pod := range newPodList(failedPods, v1.PodFailed, job) {
		podIndexer.Add(pod)
	}
	terminating := newPodList(terminatingPods, v1.PodRunning, job)
	for _, p := range terminating {
		now := metav1.Now()
		p.DeletionTimestamp = &now
	}
	for _, pod := range terminating {
		podIndexer.Add(pod)
	}
}

func setPodsStatusesWithIndexes(podIndexer cache.Indexer, job *batch.Job, status []indexPhase) {
	for _, s := range status {
		p := newPod(fmt.Sprintf("pod-%s", rand.String(10)), job)
		p.Status = v1.PodStatus{Phase: s.Phase}
		if s.Phase == v1.PodFailed || s.Phase == v1.PodSucceeded {
			p.Status.ContainerStatuses = []v1.ContainerStatus{
				{
					State: v1.ContainerState{
						Terminated: &v1.ContainerStateTerminated{
							FinishedAt: testFinishedAt,
						},
					},
				},
			}
		}
		if s.Index != noIndex {
			p.Annotations = map[string]string{
				batch.JobCompletionIndexAnnotation: s.Index,
			}
			p.Spec.Hostname = fmt.Sprintf("%s-%s", job.Name, s.Index)
		}
		p.Finalizers = append(p.Finalizers, batch.JobTrackingFinalizer)
		podIndexer.Add(p)
	}
}

type jobInitialStatus struct {
	active     int
	succeed    int
	failed     int
	startTime  *time.Time
	conditions []batch.JobCondition
}

func TestControllerSyncJob(t *testing.T) {
	_, ctx := ktesting.NewTestContext(t)
	referenceTime := time.Now()

	testCases := map[string]struct {
		// job setup
		parallelism          int32
		completions          int32
		backoffLimit         int32
		deleting             bool
		podLimit             int
		completionMode       batch.CompletionMode
		wasSuspended         bool
		suspend              bool
		podReplacementPolicy *batch.PodReplacementPolicy
		podFailurePolicy     *batch.PodFailurePolicy
		initialStatus        *jobInitialStatus
		backoffRecord        *backoffRecord
		controllerTime       *time.Time

		// pod setup

		// If a podControllerError is set, finalizers are not able to be removed.
		// This means that there is no status update so the counters for
		// failedPods and succeededPods cannot be incremented.
		podControllerError        error
		pendingPods               int
		activePods                int
		readyPods                 int
		succeededPods             int
		failedPods                int
		terminatingPods           int
		podsWithIndexes           []indexPhase
		fakeExpectationAtCreation int32 // negative: ExpectDeletions, positive: ExpectCreations

		// expectations
		expectedCreations      int32
		expectedDeletions      int32
		expectedActive         int32
		expectedReady          *int32
		expectedSucceeded      int32
		expectedCompletedIdxs  string
		expectedFailed         int32
		expectedTerminating    *int32
		expectedConditions     []batch.JobCondition
		expectedCreatedIndexes sets.Set[int]
		expectedPodPatches     int

		// features
		podIndexLabelDisabled   bool
		jobPodReplacementPolicy bool
		jobSuccessPolicy        bool
		jobManagedBy            bool
	}{
		"job start": {
			parallelism:       2,
			completions:       5,
			backoffLimit:      6,
			expectedCreations: 2,
			expectedActive:    2,
			expectedReady:     ptr.To[int32](0),
		},
		"WQ job start": {
			parallelism:       2,
			completions:       -1,
			backoffLimit:      6,
			expectedCreations: 2,
			expectedActive:    2,
			expectedReady:     ptr.To[int32](0),
		},
		"pending pods": {
			parallelism:    2,
			completions:    5,
			backoffLimit:   6,
			pendingPods:    2,
			expectedActive: 2,
			expectedReady:  ptr.To[int32](0),
		},
		"correct # of pods": {
			parallelism:    3,
			completions:    5,
			backoffLimit:   6,
			activePods:     3,
			readyPods:      2,
			expectedActive: 3,
			expectedReady:  ptr.To[int32](2),
		},
		"WQ job: correct # of pods": {
			parallelism:    2,
			completions:    -1,
			backoffLimit:   6,
			activePods:     2,
			expectedActive: 2,
			expectedReady:  ptr.To[int32](0),
		},
		"too few active pods": {
			parallelism:        2,
			completions:        5,
			backoffLimit:       6,
			activePods:         1,
			succeededPods:      1,
			expectedCreations:  1,
			expectedActive:     2,
			expectedSucceeded:  1,
			expectedPodPatches: 1,
			expectedReady:      ptr.To[int32](0),
		},
		"WQ job: recreate pods when failed": {
			parallelism:             1,
			completions:             -1,
			backoffLimit:            6,
			activePods:              1,
			failedPods:              1,
			podReplacementPolicy:    podReplacementPolicy(batch.Failed),
			jobPodReplacementPolicy: true,
			terminatingPods:         1,
			expectedTerminating:     ptr.To[int32](1),
			expectedReady:           ptr.To[int32](0),
			// Removes finalizer and deletes one failed pod
			expectedPodPatches: 1,
			expectedFailed:     1,
			expectedActive:     1,
		},
		"WQ job: turn on PodReplacementPolicy but not set PodReplacementPolicy": {
			parallelism:             1,
			completions:             1,
			backoffLimit:            6,
			activePods:              1,
			failedPods:              1,
			jobPodReplacementPolicy: true,
			expectedTerminating:     ptr.To[int32](1),
			expectedReady:           ptr.To[int32](0),
			terminatingPods:         1,
			expectedActive:          1,
			expectedPodPatches:      2,
			expectedFailed:          2,
		},
		"WQ job: recreate pods when terminating or failed": {
			parallelism:             1,
			completions:             -1,
			backoffLimit:            6,
			activePods:              1,
			failedPods:              1,
			podReplacementPolicy:    podReplacementPolicy(batch.TerminatingOrFailed),
			jobPodReplacementPolicy: true,
			terminatingPods:         1,
			expectedTerminating:     ptr.To[int32](1),
			expectedReady:           ptr.To[int32](0),
			expectedActive:          1,
			expectedPodPatches:      2,
			expectedFailed:          2,
		},
		"more terminating pods than parallelism": {
			parallelism:             1,
			completions:             1,
			backoffLimit:            6,
			activePods:              2,
			failedPods:              0,
			terminatingPods:         4,
			podReplacementPolicy:    podReplacementPolicy(batch.Failed),
			jobPodReplacementPolicy: true,
			expectedTerminating:     ptr.To[int32](5),
			expectedReady:           ptr.To[int32](0),
			expectedActive:          1,
			expectedDeletions:       1,
			expectedPodPatches:      1,
		},
		"more terminating pods than parallelism; PodFailurePolicy used": {
			// Repro for https://github.com/kubernetes/kubernetes/issues/122235
			parallelism:         1,
			completions:         1,
			backoffLimit:        6,
			activePods:          2,
			failedPods:          0,
			terminatingPods:     4,
			podFailurePolicy:    &batch.PodFailurePolicy{},
			expectedTerminating: nil,
			expectedReady:       ptr.To[int32](0),
			expectedActive:      1,
			expectedDeletions:   1,
			expectedPodPatches:  1,
		},
		"too few active pods and active back-off": {
			parallelism:  1,
			completions:  1,
			backoffLimit: 6,
			backoffRecord: &backoffRecord{
				failuresAfterLastSuccess: 1,
				lastFailureTime:          &referenceTime,
			},
			initialStatus: &jobInitialStatus{
				startTime: func() *time.Time {
					now := time.Now()
					return &now
				}(),
			},
			activePods:         0,
			succeededPods:      0,
			expectedCreations:  0,
			expectedActive:     0,
			expectedSucceeded:  0,
			expectedPodPatches: 0,
			expectedReady:      ptr.To[int32](0),
			controllerTime:     &referenceTime,
		},
		"too few active pods and no back-offs": {
			parallelism:  1,
			completions:  1,
			backoffLimit: 6,
			backoffRecord: &backoffRecord{
				failuresAfterLastSuccess: 0,
				lastFailureTime:          &referenceTime,
			},
			activePods:         0,
			succeededPods:      0,
			expectedCreations:  1,
			expectedActive:     1,
			expectedSucceeded:  0,
			expectedPodPatches: 0,
			expectedReady:      ptr.To[int32](0),
			controllerTime:     &referenceTime,
		},
		"too few active pods with a dynamic job": {
			parallelism:       2,
			completions:       -1,
			backoffLimit:      6,
			activePods:        1,
			expectedCreations: 1,
			expectedActive:    2,
			expectedReady:     ptr.To[int32](0),
		},
		"too few active pods, with controller error": {
			parallelism:        2,
			completions:        5,
			backoffLimit:       6,
			podControllerError: fmt.Errorf("fake error"),
			activePods:         1,
			succeededPods:      1,
			expectedCreations:  1,
			expectedActive:     1,
			expectedSucceeded:  0,
			expectedPodPatches: 1,
			expectedReady:      ptr.To[int32](0),
		},
		"too many active pods": {
			parallelism:        2,
			completions:        5,
			backoffLimit:       6,
			activePods:         3,
			expectedDeletions:  1,
			expectedActive:     2,
			expectedPodPatches: 1,
			expectedReady:      ptr.To[int32](0),
		},
		"too many active pods; PodReplacementPolicy enabled": {
			parallelism:             2,
			completions:             5,
			backoffLimit:            6,
			activePods:              3,
			jobPodReplacementPolicy: true,
			expectedDeletions:       1,
			expectedActive:          2,
			expectedPodPatches:      1,
			expectedReady:           ptr.To[int32](0),
			expectedTerminating:     ptr.To[int32](1),
		},
		"too many active pods, with controller error": {
			parallelism:        2,
			completions:        5,
			backoffLimit:       6,
			podControllerError: fmt.Errorf("fake error"),
			activePods:         3,
			expectedDeletions:  0,
			expectedPodPatches: 1,
			expectedActive:     3,
			expectedReady:      ptr.To[int32](0),
		},
		"count ready pods when too many active pods": {
			parallelism:        2,
			completions:        5,
			backoffLimit:       6,
			activePods:         3,
			readyPods:          3,
			expectedDeletions:  1,
			expectedActive:     2,
			expectedPodPatches: 1,
			expectedReady:      ptr.To[int32](2),
		},
		"failed + succeed pods: reset backoff delay": {
			parallelism:        2,
			completions:        5,
			backoffLimit:       6,
			activePods:         1,
			succeededPods:      1,
			failedPods:         1,
			expectedCreations:  1,
			expectedActive:     2,
			expectedSucceeded:  1,
			expectedFailed:     1,
			expectedPodPatches: 2,
			expectedReady:      ptr.To[int32](0),
		},
		"new failed pod": {
			parallelism:        2,
			completions:        5,
			backoffLimit:       6,
			activePods:         1,
			failedPods:         1,
			expectedCreations:  1,
			expectedActive:     2,
			expectedFailed:     1,
			expectedPodPatches: 1,
			expectedReady:      ptr.To[int32](0),
		},
		"no new pod; possible finalizer update of failed pod": {
			parallelism:  1,
			completions:  1,
			backoffLimit: 6,
			initialStatus: &jobInitialStatus{
				active:  1,
				succeed: 0,
				failed:  1,
			},
			activePods:         1,
			failedPods:         0,
			expectedCreations:  0,
			expectedActive:     1,
			expectedFailed:     1,
			expectedPodPatches: 0,
			expectedReady:      ptr.To[int32](0),
		},
		"only new failed pod with controller error": {
			parallelism:        2,
			completions:        5,
			backoffLimit:       6,
			podControllerError: fmt.Errorf("fake error"),
			activePods:         1,
			failedPods:         1,
			expectedCreations:  1,
			expectedActive:     1,
			expectedFailed:     0,
			expectedPodPatches: 1,
			expectedReady:      ptr.To[int32](0),
		},
		"job finish": {
			parallelism:       2,
			completions:       5,
			backoffLimit:      6,
			succeededPods:     5,
			expectedSucceeded: 5,
			expectedConditions: []batch.JobCondition{
				{
					Type:   batch.JobComplete,
					Status: v1.ConditionTrue,
				},
			},
			expectedPodPatches: 5,
			expectedReady:      ptr.To[int32](0),
		},
		"WQ job finishing": {
			parallelism:        2,
			completions:        -1,
			backoffLimit:       6,
			activePods:         1,
			succeededPods:      1,
			expectedActive:     1,
			expectedSucceeded:  1,
			expectedPodPatches: 1,
			expectedReady:      ptr.To[int32](0),
		},
		"WQ job all finished": {
			parallelism:       2,
			completions:       -1,
			backoffLimit:      6,
			succeededPods:     2,
			expectedSucceeded: 2,
			expectedConditions: []batch.JobCondition{
				{
					Type:   batch.JobComplete,
					Status: v1.ConditionTrue,
				},
			},
			expectedPodPatches: 2,
			expectedReady:      ptr.To[int32](0),
		},
		"WQ job all finished despite one failure": {
			parallelism:       2,
			completions:       -1,
			backoffLimit:      6,
			succeededPods:     1,
			failedPods:        1,
			expectedSucceeded: 1,
			expectedFailed:    1,
			expectedConditions: []batch.JobCondition{
				{
					Type:   batch.JobComplete,
					Status: v1.ConditionTrue,
				},
			},
			expectedPodPatches: 2,
			expectedReady:      ptr.To[int32](0),
		},
		"more active pods than parallelism": {
			parallelism:        2,
			completions:        5,
			backoffLimit:       6,
			activePods:         10,
			expectedDeletions:  8,
			expectedActive:     2,
			expectedPodPatches: 8,
			expectedReady:      ptr.To[int32](0),
		},
		"more active pods than remaining completions": {
			parallelism:        3,
			completions:        4,
			backoffLimit:       6,
			activePods:         3,
			succeededPods:      2,
			expectedDeletions:  1,
			expectedActive:     2,
			expectedSucceeded:  2,
			expectedPodPatches: 3,
			expectedReady:      ptr.To[int32](0),
		},
		"status change": {
			parallelism:        2,
			completions:        5,
			backoffLimit:       6,
			activePods:         2,
			succeededPods:      2,
			expectedActive:     2,
			expectedSucceeded:  2,
			expectedPodPatches: 2,
			expectedReady:      ptr.To[int32](0),
		},
		"deleting job": {
			parallelism:        2,
			completions:        5,
			backoffLimit:       6,
			deleting:           true,
			pendingPods:        1,
			activePods:         1,
			succeededPods:      1,
			expectedActive:     2,
			expectedSucceeded:  1,
			expectedPodPatches: 3,
			expectedReady:      ptr.To[int32](0),
		},
		"limited pods": {
			parallelism:       100,
			completions:       200,
			backoffLimit:      6,
			podLimit:          10,
			expectedCreations: 10,
			expectedActive:    10,
			expectedReady:     ptr.To[int32](0),
		},
		"too many job failures": {
			parallelism:    2,
			completions:    5,
			deleting:       true,
			failedPods:     1,
			expectedFailed: 1,
			expectedConditions: []batch.JobCondition{
				{
					Type:    batch.JobFailed,
					Status:  v1.ConditionTrue,
					Reason:  batch.JobReasonBackoffLimitExceeded,
					Message: "Job has reached the specified backoff limit",
				},
			},
			expectedPodPatches: 1,
			expectedReady:      ptr.To[int32](0),
		},
		"job failures, unsatisfied expectations": {
			parallelism:               2,
			completions:               5,
			deleting:                  true,
			failedPods:                1,
			fakeExpectationAtCreation: 1,
			expectedFailed:            1,
			expectedPodPatches:        1,
			expectedReady:             ptr.To[int32](0),
		},
		"indexed job start": {
			parallelism:            2,
			completions:            5,
			backoffLimit:           6,
			completionMode:         batch.IndexedCompletion,
			expectedCreations:      2,
			expectedActive:         2,
			expectedCreatedIndexes: sets.New(0, 1),
			expectedReady:          ptr.To[int32](0),
		},
		"indexed job with some pods deleted, podReplacementPolicy Failed": {
			parallelism:             2,
			completions:             5,
			backoffLimit:            6,
			completionMode:          batch.IndexedCompletion,
			expectedCreations:       1,
			expectedActive:          1,
			expectedCreatedIndexes:  sets.New(0),
			podReplacementPolicy:    podReplacementPolicy(batch.Failed),
			jobPodReplacementPolicy: true,
			terminatingPods:         1,
			expectedTerminating:     ptr.To[int32](1),
			expectedReady:           ptr.To[int32](0),
		},
		"indexed job with some pods deleted, podReplacementPolicy TerminatingOrFailed": {
			parallelism:             2,
			completions:             5,
			backoffLimit:            6,
			completionMode:          batch.IndexedCompletion,
			expectedCreations:       2,
			expectedActive:          2,
			expectedCreatedIndexes:  sets.New(0, 1),
			podReplacementPolicy:    podReplacementPolicy(batch.TerminatingOrFailed),
			jobPodReplacementPolicy: true,
			terminatingPods:         1,
			expectedTerminating:     ptr.To[int32](1),
			expectedReady:           ptr.To[int32](0),
			expectedPodPatches:      1,
		},
		"indexed job completed": {
			parallelism:    2,
			completions:    3,
			backoffLimit:   6,
			completionMode: batch.IndexedCompletion,
			podsWithIndexes: []indexPhase{
				{"0", v1.PodSucceeded},
				{"1", v1.PodFailed},
				{"1", v1.PodSucceeded},
				{"2", v1.PodSucceeded},
			},
			expectedSucceeded:     3,
			expectedFailed:        1,
			expectedCompletedIdxs: "0-2",
			expectedConditions: []batch.JobCondition{
				{
					Type:   batch.JobComplete,
					Status: v1.ConditionTrue,
				},
			},
			expectedPodPatches: 4,
			expectedReady:      ptr.To[int32](0),
		},
		"indexed job failed": {
			parallelism:    2,
			completions:    3,
			backoffLimit:   0,
			completionMode: batch.IndexedCompletion,
			podsWithIndexes: []indexPhase{
				{"0", v1.PodSucceeded},
				{"1", v1.PodFailed},
				{"2", v1.PodRunning},
			},
			expectedSucceeded:     1,
			expectedFailed:        2,
			expectedCompletedIdxs: "0",
			expectedConditions: []batch.JobCondition{
				{
					Type:    batch.JobFailed,
					Status:  v1.ConditionTrue,
					Reason:  batch.JobReasonBackoffLimitExceeded,
					Message: "Job has reached the specified backoff limit",
				},
			},
			expectedPodPatches: 3,
			expectedReady:      ptr.To[int32](0),
			expectedDeletions:  1,
		},
		"count terminating pods when indexed job fails and PodReplacementPolicy enabled": {
			parallelism:    2,
			completions:    3,
			backoffLimit:   0,
			completionMode: batch.IndexedCompletion,
			podsWithIndexes: []indexPhase{
				{"0", v1.PodSucceeded},
				{"1", v1.PodFailed},
				{"2", v1.PodRunning},
			},
			jobPodReplacementPolicy: true,
			expectedSucceeded:       1,
			expectedFailed:          2,
			expectedCompletedIdxs:   "0",
			expectedConditions: []batch.JobCondition{
				{
					Type:    batch.JobFailureTarget,
					Status:  v1.ConditionTrue,
					Reason:  batch.JobReasonBackoffLimitExceeded,
					Message: "Job has reached the specified backoff limit",
				},
			},
			expectedPodPatches:  3,
			expectedReady:       ptr.To[int32](0),
			expectedDeletions:   1,
			expectedTerminating: ptr.To[int32](1),
		},
		"count ready pods when job fails": {
			parallelism:    2,
			completions:    3,
			backoffLimit:   0,
			activePods:     2,
			readyPods:      2,
			failedPods:     1,
			expectedFailed: 3,
			expectedConditions: []batch.JobCondition{
				{
					Type:    batch.JobFailed,
					Status:  v1.ConditionTrue,
					Reason:  batch.JobReasonBackoffLimitExceeded,
					Message: "Job has reached the specified backoff limit",
				},
			},
			expectedPodPatches: 3,
			expectedReady:      ptr.To[int32](0),
			expectedDeletions:  2,
		},
		"indexed job repeated completed index": {
			parallelism:    2,
			completions:    3,
			backoffLimit:   6,
			completionMode: batch.IndexedCompletion,
			podsWithIndexes: []indexPhase{
				{"0", v1.PodSucceeded},
				{"1", v1.PodSucceeded},
				{"1", v1.PodSucceeded},
			},
			expectedCreations:      1,
			expectedActive:         1,
			expectedSucceeded:      2,
			expectedCompletedIdxs:  "0,1",
			expectedCreatedIndexes: sets.New(2),
			expectedPodPatches:     3,
			expectedReady:          ptr.To[int32](0),
		},
		"indexed job some running and completed pods": {
			parallelism:    8,
			completions:    20,
			backoffLimit:   6,
			completionMode: batch.IndexedCompletion,
			podsWithIndexes: []indexPhase{
				{"0", v1.PodRunning},
				{"2", v1.PodSucceeded},
				{"3", v1.PodPending},
				{"4", v1.PodSucceeded},
				{"5", v1.PodSucceeded},
				{"7", v1.PodSucceeded},
				{"8", v1.PodSucceeded},
				{"9", v1.PodSucceeded},
			},
			expectedCreations:      6,
			expectedActive:         8,
			expectedSucceeded:      6,
			expectedCompletedIdxs:  "2,4,5,7-9",
			expectedCreatedIndexes: sets.New(1, 6, 10, 11, 12, 13),
			expectedPodPatches:     6,
			expectedReady:          ptr.To[int32](0),
		},
		"indexed job some failed pods": {
			parallelism:    3,
			completions:    4,
			backoffLimit:   6,
			completionMode: batch.IndexedCompletion,
			podsWithIndexes: []indexPhase{
				{"0", v1.PodFailed},
				{"1", v1.PodPending},
				{"2", v1.PodFailed},
			},
			expectedCreations:      2,
			expectedActive:         3,
			expectedFailed:         2,
			expectedCreatedIndexes: sets.New(0, 2),
			expectedPodPatches:     2,
			expectedReady:          ptr.To[int32](0),
		},
		"indexed job some pods without index": {
			parallelism:    2,
			completions:    5,
			backoffLimit:   6,
			completionMode: batch.IndexedCompletion,
			activePods:     1,
			succeededPods:  1,
			failedPods:     1,
			podsWithIndexes: []indexPhase{
				{"invalid", v1.PodRunning},
				{"invalid", v1.PodSucceeded},
				{"invalid", v1.PodFailed},
				{"invalid", v1.PodPending},
				{"0", v1.PodSucceeded},
				{"1", v1.PodRunning},
				{"2", v1.PodRunning},
			},
			expectedDeletions:     3,
			expectedActive:        2,
			expectedSucceeded:     1,
			expectedFailed:        0,
			expectedCompletedIdxs: "0",
			expectedPodPatches:    8,
			expectedReady:         ptr.To[int32](0),
		},
		"indexed job repeated indexes": {
			parallelism:    5,
			completions:    5,
			backoffLimit:   6,
			completionMode: batch.IndexedCompletion,
			succeededPods:  1,
			failedPods:     1,
			podsWithIndexes: []indexPhase{
				{"invalid", v1.PodRunning},
				{"0", v1.PodSucceeded},
				{"1", v1.PodRunning},
				{"2", v1.PodRunning},
				{"2", v1.PodPending},
			},
			expectedCreations:     0,
			expectedDeletions:     2,
			expectedActive:        2,
			expectedSucceeded:     1,
			expectedCompletedIdxs: "0",
			expectedPodPatches:    5,
			expectedReady:         ptr.To[int32](0),
		},
		"indexed job with indexes outside of range": {
			parallelism:    2,
			completions:    5,
			backoffLimit:   6,
			completionMode: batch.IndexedCompletion,
			podsWithIndexes: []indexPhase{
				{"0", v1.PodSucceeded},
				{"5", v1.PodRunning},
				{"6", v1.PodSucceeded},
				{"7", v1.PodPending},
				{"8", v1.PodFailed},
			},
			expectedCreations:     0, // only one of creations and deletions can happen in a sync
			expectedSucceeded:     1,
			expectedDeletions:     2,
			expectedCompletedIdxs: "0",
			expectedActive:        0,
			expectedFailed:        0,
			expectedPodPatches:    5,
			expectedReady:         ptr.To[int32](0),
		},
		"suspending a job with satisfied expectations": {
			// Suspended Job should delete active pods when expectations are
			// satisfied.
			suspend:           true,
			parallelism:       2,
			activePods:        2, // parallelism == active, expectations satisfied
			completions:       4,
			backoffLimit:      6,
			expectedCreations: 0,
			expectedDeletions: 2,
			expectedActive:    0,
			expectedConditions: []batch.JobCondition{
				{
					Type:    batch.JobSuspended,
					Status:  v1.ConditionTrue,
					Reason:  "JobSuspended",
					Message: "Job suspended",
				},
			},
			expectedPodPatches: 2,
			expectedReady:      ptr.To[int32](0),
		},
		"suspending a job with satisfied expectations; PodReplacementPolicy enabled": {
			// Suspended Job should delete active pods when expectations are
			// satisfied.
			suspend:                 true,
			parallelism:             2,
			activePods:              2, // parallelism == active, expectations satisfied
			completions:             4,
			backoffLimit:            6,
			jobPodReplacementPolicy: true,
			expectedCreations:       0,
			expectedDeletions:       2,
			expectedActive:          0,
			expectedConditions: []batch.JobCondition{
				{
					Type:    batch.JobSuspended,
					Status:  v1.ConditionTrue,
					Reason:  "JobSuspended",
					Message: "Job suspended",
				},
			},
			expectedPodPatches:  2,
			expectedReady:       ptr.To[int32](0),
			expectedTerminating: ptr.To[int32](2),
		},
		"count ready pods when suspending a job with satisfied expectations": {
			suspend:           true,
			parallelism:       2,
			activePods:        2, // parallelism == active, expectations satisfied
			readyPods:         2,
			completions:       4,
			backoffLimit:      6,
			expectedCreations: 0,
			expectedDeletions: 2,
			expectedActive:    0,
			expectedConditions: []batch.JobCondition{
				{
					Type:    batch.JobSuspended,
					Status:  v1.ConditionTrue,
					Reason:  "JobSuspended",
					Message: "Job suspended",
				},
			},
			expectedPodPatches: 2,
			expectedReady:      ptr.To[int32](0),
		},
		"suspending a job with unsatisfied expectations": {
			// Unlike the previous test, we expect the controller to NOT suspend the
			// Job in the syncJob call because the controller will wait for
			// expectations to be satisfied first. The next syncJob call (not tested
			// here) will be the same as the previous test.
			suspend:                   true,
			parallelism:               2,
			activePods:                3,  // active > parallelism, expectations unsatisfied
			fakeExpectationAtCreation: -1, // the controller is expecting a deletion
			completions:               4,
			backoffLimit:              6,
			expectedCreations:         0,
			expectedDeletions:         0,
			expectedActive:            3,
			expectedReady:             ptr.To[int32](0),
		},
		"count ready pods when suspending a job with unsatisfied expectations": {
			suspend:                   true,
			parallelism:               2,
			activePods:                3, // active > parallelism, expectations unsatisfied
			readyPods:                 3,
			fakeExpectationAtCreation: -1, // the controller is expecting a deletion
			completions:               4,
			backoffLimit:              6,
			expectedCreations:         0,
			expectedDeletions:         0,
			expectedActive:            3,
			expectedReady:             ptr.To[int32](3),
		},
		"suspending a job with unsatisfied expectations; PodReplacementPolicy enabled": {
			suspend:                   true,
			parallelism:               2,
			activePods:                3,  // active > parallelism, expectations unsatisfied
			fakeExpectationAtCreation: -1, // the controller is expecting a deletion
			completions:               4,
			backoffLimit:              6,
			jobPodReplacementPolicy:   true,
			expectedCreations:         0,
			expectedDeletions:         0,
			expectedActive:            3,
			expectedReady:             ptr.To[int32](0),
			expectedTerminating:       ptr.To[int32](0),
		},
		"resuming a suspended job": {
			wasSuspended:      true,
			suspend:           false,
			parallelism:       2,
			completions:       4,
			backoffLimit:      6,
			expectedCreations: 2,
			expectedDeletions: 0,
			expectedActive:    2,
			expectedConditions: []batch.JobCondition{
				{
					Type:    batch.JobSuspended,
					Status:  v1.ConditionFalse,
					Reason:  "JobResumed",
					Message: "Job resumed",
				},
			},
			expectedReady: ptr.To[int32](0),
		},
		"suspending a deleted job": {
			// We would normally expect the active pods to be deleted (see a few test
			// cases above), but since this job is being deleted, we don't expect
			// anything changed here from before the job was suspended. The
			// JobSuspended condition is also missing.
			suspend:            true,
			deleting:           true,
			parallelism:        2,
			activePods:         2, // parallelism == active, expectations satisfied
			completions:        4,
			backoffLimit:       6,
			expectedCreations:  0,
			expectedDeletions:  0,
			expectedActive:     2,
			expectedPodPatches: 2,
			expectedReady:      ptr.To[int32](0),
		},
		"suspending a deleted job; PodReplacementPolicy enabled": {
			suspend:                 true,
			deleting:                true,
			parallelism:             2,
			activePods:              2, // parallelism == active, expectations satisfied
			completions:             4,
			backoffLimit:            6,
			jobPodReplacementPolicy: true,
			expectedCreations:       0,
			expectedDeletions:       0,
			expectedActive:          2,
			expectedPodPatches:      2,
			expectedReady:           ptr.To[int32](0),
			expectedTerminating:     ptr.To[int32](0),
		},
		"indexed job with podIndexLabel feature disabled": {
			parallelism:            2,
			completions:            5,
			backoffLimit:           6,
			completionMode:         batch.IndexedCompletion,
			expectedCreations:      2,
			expectedActive:         2,
			expectedCreatedIndexes: sets.New(0, 1),
			podIndexLabelDisabled:  true,
			expectedReady:          ptr.To[int32](0),
		},
		"FailureTarget=False condition added manually is ignored": {
			parallelism: 1,
			completions: 1,
			activePods:  1,
			readyPods:   1,
			initialStatus: &jobInitialStatus{
				active: 1,
				startTime: func() *time.Time {
					now := time.Now()
					return &now
				}(),
				conditions: []batch.JobCondition{
					{
						Type:    batch.JobFailureTarget,
						Status:  v1.ConditionFalse,
						Reason:  "ConditionAddedManually",
						Message: "Testing",
					},
				},
			},
			expectedActive: 1,
			expectedReady:  ptr.To[int32](1),
			expectedConditions: []batch.JobCondition{
				{
					Type:    batch.JobFailureTarget,
					Status:  v1.ConditionFalse,
					Reason:  "ConditionAddedManually",
					Message: "Testing",
				},
			},
		},
		"SuccessCriteriaMet=False condition added manually is ignored": {
			jobSuccessPolicy: true,
			parallelism:      1,
			completions:      1,
			activePods:       1,
			readyPods:        1,
			initialStatus: &jobInitialStatus{
				active: 1,
				startTime: func() *time.Time {
					now := time.Now()
					return &now
				}(),
				conditions: []batch.JobCondition{
					{
						Type:    batch.JobSuccessCriteriaMet,
						Status:  v1.ConditionFalse,
						Reason:  "ConditionAddedManually",
						Message: "Testing",
					},
				},
			},
			expectedActive: 1,
			expectedReady:  ptr.To[int32](1),
			expectedConditions: []batch.JobCondition{
				{
					Type:    batch.JobSuccessCriteriaMet,
					Status:  v1.ConditionFalse,
					Reason:  "ConditionAddedManually",
					Message: "Testing",
				},
			},
		},
		"backoff limit exceeded; JobManagedBy enabled": {
			jobManagedBy:   true,
			parallelism:    2,
			completions:    3,
			backoffLimit:   0,
			completionMode: batch.IndexedCompletion,
			podsWithIndexes: []indexPhase{
				{"0", v1.PodSucceeded},
				{"1", v1.PodFailed},
				{"2", v1.PodRunning},
			},
			expectedSucceeded:     1,
			expectedFailed:        2,
			expectedCompletedIdxs: "0",
			expectedConditions: []batch.JobCondition{
				{
					Type:    batch.JobFailureTarget,
					Status:  v1.ConditionTrue,
					Reason:  batch.JobReasonBackoffLimitExceeded,
					Message: "Job has reached the specified backoff limit",
				},
			},
			expectedPodPatches: 3,
			expectedReady:      ptr.To[int32](0),
			expectedDeletions:  1,
		},
	}

	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			logger, _ := ktesting.NewTestContext(t)
			if tc.podIndexLabelDisabled {
				// TODO: this will be removed in 1.35 when 1.31 will fall out of support matrix
				featuregatetesting.SetFeatureGateEmulationVersionDuringTest(t, feature.DefaultFeatureGate, utilversion.MustParse("1.31"))
			}
			featuregatetesting.SetFeatureGateDuringTest(t, feature.DefaultFeatureGate, features.PodIndexLabel, !tc.podIndexLabelDisabled)
			featuregatetesting.SetFeatureGateDuringTest(t, feature.DefaultFeatureGate, features.JobPodReplacementPolicy, tc.jobPodReplacementPolicy)
			featuregatetesting.SetFeatureGateDuringTest(t, feature.DefaultFeatureGate, features.JobSuccessPolicy, tc.jobSuccessPolicy)
			featuregatetesting.SetFeatureGateDuringTest(t, feature.DefaultFeatureGate, features.JobManagedBy, tc.jobManagedBy)
			// job manager setup
			clientSet := clientset.NewForConfigOrDie(&restclient.Config{Host: "", ContentConfig: restclient.ContentConfig{GroupVersion: &schema.GroupVersion{Group: "", Version: "v1"}}})

			var fakeClock clock.WithTicker
			if tc.controllerTime != nil {
				fakeClock = clocktesting.NewFakeClock(*tc.controllerTime)
			} else {
				fakeClock = clocktesting.NewFakeClock(time.Now())
			}

			manager, sharedInformerFactory := newControllerFromClientWithClock(ctx, t, clientSet, controller.NoResyncPeriodFunc, fakeClock)
			fakePodControl := controller.FakePodControl{Err: tc.podControllerError, CreateLimit: tc.podLimit}
			manager.podControl = &fakePodControl
			manager.podStoreSynced = alwaysReady
			manager.jobStoreSynced = alwaysReady

			// job & pods setup
			job := newJob(tc.parallelism, tc.completions, tc.backoffLimit, tc.completionMode)
			job.Spec.Suspend = ptr.To(tc.suspend)
			if tc.jobPodReplacementPolicy {
				job.Spec.PodReplacementPolicy = tc.podReplacementPolicy
			}
			job.Spec.PodFailurePolicy = tc.podFailurePolicy
			if tc.initialStatus != nil {
				startTime := metav1.Now()
				job.Status.StartTime = &startTime
				job.Status.Active = int32(tc.initialStatus.active)
				job.Status.Succeeded = int32(tc.initialStatus.succeed)
				job.Status.Failed = int32(tc.initialStatus.failed)
				if tc.initialStatus.startTime != nil {
					startTime := metav1.NewTime(*tc.initialStatus.startTime)
					job.Status.StartTime = &startTime
				}
				job.Status.Conditions = append(job.Status.Conditions, tc.initialStatus.conditions...)
			}

			key, err := controller.KeyFunc(job)
			if err != nil {
				t.Errorf("Unexpected error getting job key: %v", err)
			}

			if tc.backoffRecord != nil {
				tc.backoffRecord.key = key
				manager.podBackoffStore.updateBackoffRecord(*tc.backoffRecord)
			}
			if tc.fakeExpectationAtCreation < 0 {
				manager.expectations.ExpectDeletions(logger, key, int(-tc.fakeExpectationAtCreation))
			} else if tc.fakeExpectationAtCreation > 0 {
				manager.expectations.ExpectCreations(logger, key, int(tc.fakeExpectationAtCreation))
			}
			if tc.wasSuspended {
				job.Status.Conditions = append(job.Status.Conditions, *newCondition(batch.JobSuspended, v1.ConditionTrue, "JobSuspended", "Job suspended", realClock.Now()))
			}
			if tc.deleting {
				now := metav1.Now()
				job.DeletionTimestamp = &now
			}
			sharedInformerFactory.Batch().V1().Jobs().Informer().GetIndexer().Add(job)
			podIndexer := sharedInformerFactory.Core().V1().Pods().Informer().GetIndexer()
			setPodsStatuses(podIndexer, job, tc.pendingPods, tc.activePods, tc.succeededPods, tc.failedPods, tc.terminatingPods, tc.readyPods)
			setPodsStatusesWithIndexes(podIndexer, job, tc.podsWithIndexes)

			actual := job
			manager.updateStatusHandler = func(ctx context.Context, job *batch.Job) (*batch.Job, error) {
				actual = job
				return job, nil
			}

			// run
			err = manager.syncJob(ctx, testutil.GetKey(job, t))
			// We need requeue syncJob task if podController error
			if tc.podControllerError != nil {
				if err == nil {
					t.Error("Syncing jobs expected to return error on podControl exception")
				}
			} else if tc.podLimit != 0 && fakePodControl.CreateCallCount > tc.podLimit {
				if err == nil {
					t.Error("Syncing jobs expected to return error when reached the podControl limit")
				}
			} else if err != nil {
				t.Errorf("Unexpected error when syncing jobs: %v", err)
			}
			// validate created/deleted pods
			if int32(len(fakePodControl.Templates)) != tc.expectedCreations {
				t.Errorf("Unexpected number of creates.  Expected %d, saw %d\n", tc.expectedCreations, len(fakePodControl.Templates))
			}
			if tc.completionMode == batch.IndexedCompletion {
				checkIndexedJobPods(t, &fakePodControl, tc.expectedCreatedIndexes, job.Name, tc.podIndexLabelDisabled)
			} else {
				for _, p := range fakePodControl.Templates {
					// Fake pod control doesn't add generate name from the owner reference.
					if p.GenerateName != "" {
						t.Errorf("Got pod generate name %s, want %s", p.GenerateName, "")
					}
					if p.Spec.Hostname != "" {
						t.Errorf("Got pod hostname %q, want none", p.Spec.Hostname)
					}
				}
			}
			if int32(len(fakePodControl.DeletePodName)) != tc.expectedDeletions {
				t.Errorf("Unexpected number of deletes.  Expected %d, saw %d\n", tc.expectedDeletions, len(fakePodControl.DeletePodName))
			}
			// Each create should have an accompanying ControllerRef.
			if len(fakePodControl.ControllerRefs) != int(tc.expectedCreations) {
				t.Errorf("Unexpected number of ControllerRefs.  Expected %d, saw %d\n", tc.expectedCreations, len(fakePodControl.ControllerRefs))
			}
			// Make sure the ControllerRefs are correct.
			for _, controllerRef := range fakePodControl.ControllerRefs {
				if got, want := controllerRef.APIVersion, "batch/v1"; got != want {
					t.Errorf("controllerRef.APIVersion = %q, want %q", got, want)
				}
				if got, want := controllerRef.Kind, "Job"; got != want {
					t.Errorf("controllerRef.Kind = %q, want %q", got, want)
				}
				if got, want := controllerRef.Name, job.Name; got != want {
					t.Errorf("controllerRef.Name = %q, want %q", got, want)
				}
				if got, want := controllerRef.UID, job.UID; got != want {
					t.Errorf("controllerRef.UID = %q, want %q", got, want)
				}
				if controllerRef.Controller == nil || *controllerRef.Controller != true {
					t.Errorf("controllerRef.Controller is not set to true")
				}
			}
			// validate status
			if actual.Status.Active != tc.expectedActive {
				t.Errorf("Unexpected number of active pods.  Expected %d, saw %d\n", tc.expectedActive, actual.Status.Active)
			}
			if diff := cmp.Diff(tc.expectedReady, actual.Status.Ready); diff != "" {
				t.Errorf("Unexpected number of ready pods (-want,+got): %s", diff)
			}
			if actual.Status.Succeeded != tc.expectedSucceeded {
				t.Errorf("Unexpected number of succeeded pods.  Expected %d, saw %d\n", tc.expectedSucceeded, actual.Status.Succeeded)
			}
			if diff := cmp.Diff(tc.expectedCompletedIdxs, actual.Status.CompletedIndexes); diff != "" {
				t.Errorf("Unexpected completed indexes (-want,+got):\n%s", diff)
			}
			if actual.Status.Failed != tc.expectedFailed {
				t.Errorf("Unexpected number of failed pods.  Expected %d, saw %d\n", tc.expectedFailed, actual.Status.Failed)
			}
			if diff := cmp.Diff(tc.expectedTerminating, actual.Status.Terminating); diff != "" {
				t.Errorf("Unexpected number of terminating pods (-want,+got): %s", diff)
			}
			if actual.Status.StartTime != nil && tc.suspend {
				t.Error("Unexpected .status.startTime not nil when suspend is true")
			}
			if actual.Status.StartTime == nil && !tc.suspend {
				t.Error("Missing .status.startTime")
			}
			// validate conditions
			if diff := cmp.Diff(tc.expectedConditions, actual.Status.Conditions, cmpopts.IgnoreFields(batch.JobCondition{}, "LastProbeTime", "LastTransitionTime")); diff != "" {
				t.Errorf("unexpected conditions (-want,+got):\n%s", diff)
			}
			// validate slow start
			expectedLimit := 0
			for pass := uint8(0); expectedLimit <= tc.podLimit; pass++ {
				expectedLimit += controller.SlowStartInitialBatchSize << pass
			}
			if tc.podLimit > 0 && fakePodControl.CreateCallCount > expectedLimit {
				t.Errorf("Unexpected number of create calls.  Expected <= %d, saw %d\n", fakePodControl.CreateLimit*2, fakePodControl.CreateCallCount)
			}
			if p := len(fakePodControl.Patches); p != tc.expectedPodPatches {
				t.Errorf("Got %d pod patches, want %d", p, tc.expectedPodPatches)
			}
		})
	}
}

func checkIndexedJobPods(t *testing.T, control *controller.FakePodControl, wantIndexes sets.Set[int], jobName string, podIndexLabelDisabled bool) {
	t.Helper()
	gotIndexes := sets.New[int]()
	for _, p := range control.Templates {
		checkJobCompletionEnvVariable(t, &p.Spec, podIndexLabelDisabled)
		if !podIndexLabelDisabled {
			checkJobCompletionLabel(t, &p)
		}
		ix := getCompletionIndex(p.Annotations)
		if ix == -1 {
			t.Errorf("Created pod %s didn't have completion index", p.Name)
		} else {
			gotIndexes.Insert(ix)
		}
		expectedName := fmt.Sprintf("%s-%d", jobName, ix)
		if expectedName != p.Spec.Hostname {
			t.Errorf("Got pod hostname %s, want %s", p.Spec.Hostname, expectedName)
		}
		expectedName += "-"
		if expectedName != p.GenerateName {
			t.Errorf("Got pod generate name %s, want %s", p.GenerateName, expectedName)
		}
	}
	if diff := cmp.Diff(sets.List(wantIndexes), sets.List(gotIndexes)); diff != "" {
		t.Errorf("Unexpected created completion indexes (-want,+got):\n%s", diff)
	}
}

func TestGetNewFinishedPods(t *testing.T) {
	cases := map[string]struct {
		job                  batch.Job
		pods                 []*v1.Pod
		expectedRmFinalizers sets.Set[types.UID]
		wantSucceeded        int32
		wantFailed           int32
	}{
		"some counted": {
			job: batch.Job{
				Status: batch.JobStatus{
					Succeeded:               2,
					Failed:                  1,
					UncountedTerminatedPods: &batch.UncountedTerminatedPods{},
				},
			},
			pods: []*v1.Pod{
				buildPod().uid("a").phase(v1.PodSucceeded).Pod,
				buildPod().uid("b").phase(v1.PodSucceeded).trackingFinalizer().Pod,
				buildPod().uid("c").phase(v1.PodSucceeded).trackingFinalizer().Pod,
				buildPod().uid("d").phase(v1.PodFailed).Pod,
				buildPod().uid("e").phase(v1.PodFailed).trackingFinalizer().Pod,
				buildPod().uid("f").phase(v1.PodRunning).Pod,
			},
			wantSucceeded: 4,
			wantFailed:    2,
		},
		"some uncounted": {
			job: batch.Job{
				Status: batch.JobStatus{
					Succeeded: 1,
					Failed:    1,
					UncountedTerminatedPods: &batch.UncountedTerminatedPods{
						Succeeded: []types.UID{"a", "c"},
						Failed:    []types.UID{"e", "f"},
					},
				},
			},
			pods: []*v1.Pod{
				buildPod().uid("a").phase(v1.PodSucceeded).Pod,
				buildPod().uid("b").phase(v1.PodSucceeded).Pod,
				buildPod().uid("c").phase(v1.PodSucceeded).trackingFinalizer().Pod,
				buildPod().uid("d").phase(v1.PodSucceeded).trackingFinalizer().Pod,
				buildPod().uid("e").phase(v1.PodFailed).Pod,
				buildPod().uid("f").phase(v1.PodFailed).trackingFinalizer().Pod,
				buildPod().uid("g").phase(v1.PodFailed).trackingFinalizer().Pod,
			},
			wantSucceeded: 4,
			wantFailed:    4,
		},
		"with expected removed finalizers": {
			job: batch.Job{
				Status: batch.JobStatus{
					Succeeded: 2,
					Failed:    2,
					UncountedTerminatedPods: &batch.UncountedTerminatedPods{
						Succeeded: []types.UID{"a"},
						Failed:    []types.UID{"d"},
					},
				},
			},
			expectedRmFinalizers: sets.New[types.UID]("b", "f"),
			pods: []*v1.Pod{
				buildPod().uid("a").phase(v1.PodSucceeded).Pod,
				buildPod().uid("b").phase(v1.PodSucceeded).trackingFinalizer().Pod,
				buildPod().uid("c").phase(v1.PodSucceeded).trackingFinalizer().Pod,
				buildPod().uid("d").phase(v1.PodFailed).Pod,
				buildPod().uid("e").phase(v1.PodFailed).trackingFinalizer().Pod,
				buildPod().uid("f").phase(v1.PodFailed).trackingFinalizer().Pod,
				buildPod().uid("g").phase(v1.PodFailed).trackingFinalizer().Pod,
			},
			wantSucceeded: 4,
			wantFailed:    5,
		},
		"deleted pods": {
			job: batch.Job{
				Status: batch.JobStatus{
					Succeeded:               1,
					Failed:                  1,
					UncountedTerminatedPods: &batch.UncountedTerminatedPods{},
				},
			},
			pods: []*v1.Pod{
				buildPod().uid("a").phase(v1.PodSucceeded).trackingFinalizer().deletionTimestamp().Pod,
				buildPod().uid("b").phase(v1.PodFailed).trackingFinalizer().deletionTimestamp().Pod,
				buildPod().uid("c").phase(v1.PodRunning).trackingFinalizer().deletionTimestamp().Pod,
				buildPod().uid("d").phase(v1.PodPending).trackingFinalizer().deletionTimestamp().Pod,
				buildPod().uid("e").phase(v1.PodRunning).deletionTimestamp().Pod,
				buildPod().uid("f").phase(v1.PodPending).deletionTimestamp().Pod,
			},
			wantSucceeded: 2,
			wantFailed:    4,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			uncounted := newUncountedTerminatedPods(*tc.job.Status.UncountedTerminatedPods)
			jobCtx := &syncJobCtx{job: &tc.job, pods: tc.pods, uncounted: uncounted, expectedRmFinalizers: tc.expectedRmFinalizers}
			succeededPods, failedPods := getNewFinishedPods(jobCtx)
			succeeded := int32(len(succeededPods)) + tc.job.Status.Succeeded + int32(len(uncounted.succeeded))
			failed := int32(len(failedPods)) + tc.job.Status.Failed + int32(len(uncounted.failed))
			if succeeded != tc.wantSucceeded {
				t.Errorf("getStatus reports %d succeeded pods, want %d", succeeded, tc.wantSucceeded)
			}
			if failed != tc.wantFailed {
				t.Errorf("getStatus reports %d succeeded pods, want %d", failed, tc.wantFailed)
			}
		})
	}
}

func TestTrackJobStatusAndRemoveFinalizers(t *testing.T) {
	logger, ctx := ktesting.NewTestContext(t)
	fakeClock := clocktesting.NewFakeClock(time.Now())
	now := fakeClock.Now()
	minuteAgo := now.Add(-time.Minute)
	completedCond := newCondition(batch.JobComplete, v1.ConditionTrue, "", "", now)
	succeededCond := newCondition(batch.JobSuccessCriteriaMet, v1.ConditionTrue, "", "", minuteAgo)
	failedCond := newCondition(batch.JobFailed, v1.ConditionTrue, "", "", now)
	failureTargetCond := newCondition(batch.JobFailureTarget, v1.ConditionTrue, "", "", now)
	indexedCompletion := batch.IndexedCompletion
	mockErr := errors.New("mock error")
	cases := map[string]struct {
		job                     batch.Job
		pods                    []*v1.Pod
		finishedCond            *batch.JobCondition
		expectedRmFinalizers    sets.Set[types.UID]
		needsFlush              bool
		statusUpdateErr         error
		podControlErr           error
		wantErr                 error
		wantRmFinalizers        int
		wantStatusUpdates       []batch.JobStatus
		wantSucceededPodsMetric int
		wantFailedPodsMetric    int

		// features
		enableJobBackoffLimitPerIndex bool
		enableJobSuccessPolicy        bool
		enableJobPodReplacementPolicy bool
		enableJobManagedBy            bool
	}{
		"no updates": {},
		"new active": {
			job: batch.Job{
				Status: batch.JobStatus{
					Active: 1,
				},
			},
			needsFlush: true,
			wantStatusUpdates: []batch.JobStatus{
				{
					UncountedTerminatedPods: &batch.UncountedTerminatedPods{},
					Active:                  1,
				},
			},
		},
		"track finished pods": {
			pods: []*v1.Pod{
				buildPod().uid("a").phase(v1.PodSucceeded).trackingFinalizer().Pod,
				buildPod().uid("b").phase(v1.PodFailed).trackingFinalizer().Pod,
				buildPod().uid("c").phase(v1.PodSucceeded).trackingFinalizer().deletionTimestamp().Pod,
				buildPod().uid("d").phase(v1.PodFailed).trackingFinalizer().deletionTimestamp().Pod,
				buildPod().uid("e").phase(v1.PodPending).trackingFinalizer().deletionTimestamp().Pod,
				buildPod().phase(v1.PodPending).trackingFinalizer().Pod,
				buildPod().phase(v1.PodRunning).trackingFinalizer().Pod,
			},
			wantRmFinalizers: 5,
			wantStatusUpdates: []batch.JobStatus{
				{
					UncountedTerminatedPods: &batch.UncountedTerminatedPods{
						Succeeded: []types.UID{"a", "c"},
						Failed:    []types.UID{"b", "d", "e"},
					},
				},
				{
					UncountedTerminatedPods: &batch.UncountedTerminatedPods{},
					Succeeded:               2,
					Failed:                  3,
				},
			},
			wantSucceededPodsMetric: 2,
			wantFailedPodsMetric:    3,
		},
		"past and new finished pods": {
			job: batch.Job{
				Status: batch.JobStatus{
					Active:    1,
					Succeeded: 2,
					Failed:    3,
					UncountedTerminatedPods: &batch.UncountedTerminatedPods{
						Succeeded: []types.UID{"a", "e"},
						Failed:    []types.UID{"b", "f"},
					},
				},
			},
			pods: []*v1.Pod{
				buildPod().uid("e").phase(v1.PodSucceeded).Pod,
				buildPod().phase(v1.PodFailed).Pod,
				buildPod().phase(v1.PodPending).Pod,
				buildPod().uid("a").phase(v1.PodSucceeded).trackingFinalizer().Pod,
				buildPod().uid("b").phase(v1.PodFailed).trackingFinalizer().Pod,
				buildPod().uid("c").phase(v1.PodSucceeded).trackingFinalizer().Pod,
				buildPod().uid("d").phase(v1.PodFailed).trackingFinalizer().Pod,
			},
			wantRmFinalizers: 4,
			wantStatusUpdates: []batch.JobStatus{
				{
					UncountedTerminatedPods: &batch.UncountedTerminatedPods{
						Succeeded: []types.UID{"a", "c"},
						Failed:    []types.UID{"b", "d"},
					},
					Active:    1,
					Succeeded: 3,
					Failed:    4,
				},
				{
					UncountedTerminatedPods: &batch.UncountedTerminatedPods{},
					Active:                  1,
					Succeeded:               5,
					Failed:                  6,
				},
			},
			wantSucceededPodsMetric: 3,
			wantFailedPodsMetric:    3,
		},
		"expecting removed finalizers": {
			job: batch.Job{
				Status: batch.JobStatus{
					Succeeded: 2,
					Failed:    3,
					UncountedTerminatedPods: &batch.UncountedTerminatedPods{
						Succeeded: []types.UID{"a", "g"},
						Failed:    []types.UID{"b", "h"},
					},
				},
			},
			expectedRmFinalizers: sets.New[types.UID]("c", "d", "g", "h"),
			pods: []*v1.Pod{
				buildPod().uid("a").phase(v1.PodSucceeded).trackingFinalizer().Pod,
				buildPod().uid("b").phase(v1.PodFailed).trackingFinalizer().Pod,
				buildPod().uid("c").phase(v1.PodSucceeded).trackingFinalizer().Pod,
				buildPod().uid("d").phase(v1.PodFailed).trackingFinalizer().Pod,
				buildPod().uid("e").phase(v1.PodSucceeded).trackingFinalizer().Pod,
				buildPod().uid("f").phase(v1.PodFailed).trackingFinalizer().Pod,
				buildPod().uid("g").phase(v1.PodSucceeded).trackingFinalizer().Pod,
				buildPod().uid("h").phase(v1.PodFailed).trackingFinalizer().Pod,
			},
			wantRmFinalizers: 4,
			wantStatusUpdates: []batch.JobStatus{
				{
					UncountedTerminatedPods: &batch.UncountedTerminatedPods{
						Succeeded: []types.UID{"a", "e"},
						Failed:    []types.UID{"b", "f"},
					},
					Succeeded: 3,
					Failed:    4,
				},
				{
					UncountedTerminatedPods: &batch.UncountedTerminatedPods{},
					Succeeded:               5,
					Failed:                  6,
				},
			},
			wantSucceededPodsMetric: 3,
			wantFailedPodsMetric:    3,
		},
		"succeeding job by JobSuccessPolicy": {
			pods: []*v1.Pod{
				buildPod().uid("a").phase(v1.PodSucceeded).trackingFinalizer().Pod,
				buildPod().uid("b").phase(v1.PodFailed).trackingFinalizer().Pod,
				buildPod().uid("c").phase(v1.PodPending).trackingFinalizer().Pod,
			},
			finishedCond:     succeededCond,
			wantRmFinalizers: 3,
			wantStatusUpdates: []batch.JobStatus{
				{
					UncountedTerminatedPods: &batch.UncountedTerminatedPods{
						Succeeded: []types.UID{"a"},
						Failed:    []types.UID{"b"},
					},
					Conditions: []batch.JobCondition{*succeededCond},
				},
				{
					UncountedTerminatedPods: &batch.UncountedTerminatedPods{},
					Succeeded:               1,
					Failed:                  1,
					Conditions:              []batch.JobCondition{*succeededCond, *completedCond},
					CompletionTime:          ptr.To(metav1.NewTime(now)),
				},
			},
			wantSucceededPodsMetric: 1,
			wantFailedPodsMetric:    1,
			enableJobSuccessPolicy:  true,
		},
		"completing job": {
			pods: []*v1.Pod{
				buildPod().uid("a").phase(v1.PodSucceeded).trackingFinalizer().Pod,
				buildPod().uid("b").phase(v1.PodFailed).trackingFinalizer().Pod,
			},
			finishedCond:     completedCond,
			wantRmFinalizers: 2,
			wantStatusUpdates: []batch.JobStatus{
				{
					UncountedTerminatedPods: &batch.UncountedTerminatedPods{
						Succeeded: []types.UID{"a"},
						Failed:    []types.UID{"b"},
					},
				},
				{
					UncountedTerminatedPods: &batch.UncountedTerminatedPods{},
					Succeeded:               1,
					Failed:                  1,
					Conditions:              []batch.JobCondition{*completedCond},
					CompletionTime:          &completedCond.LastTransitionTime,
				},
			},
			wantSucceededPodsMetric: 1,
			wantFailedPodsMetric:    1,
		},
		"failing job": {
			pods: []*v1.Pod{
				buildPod().uid("a").phase(v1.PodSucceeded).trackingFinalizer().Pod,
				buildPod().uid("b").phase(v1.PodFailed).trackingFinalizer().Pod,
				buildPod().uid("c").phase(v1.PodRunning).trackingFinalizer().Pod,
			},
			finishedCond: failedCond,
			// Running pod counts as failed.
			wantRmFinalizers: 3,
			wantStatusUpdates: []batch.JobStatus{
				{
					UncountedTerminatedPods: &batch.UncountedTerminatedPods{
						Succeeded: []types.UID{"a"},
						Failed:    []types.UID{"b", "c"},
					},
				},
				{
					UncountedTerminatedPods: &batch.UncountedTerminatedPods{},
					Succeeded:               1,
					Failed:                  2,
					Conditions:              []batch.JobCondition{*failedCond},
				},
			},
			wantSucceededPodsMetric: 1,
			wantFailedPodsMetric:    2,
		},
		"deleted job": {
			job: batch.Job{
				ObjectMeta: metav1.ObjectMeta{
					DeletionTimestamp: &metav1.Time{},
				},
				Status: batch.JobStatus{
					Active: 1,
				},
			},
			pods: []*v1.Pod{
				buildPod().uid("a").phase(v1.PodSucceeded).trackingFinalizer().Pod,
				buildPod().uid("b").phase(v1.PodFailed).trackingFinalizer().Pod,
				buildPod().phase(v1.PodRunning).trackingFinalizer().Pod,
			},
			// Removing finalizer from Running pod, but doesn't count as failed.
			wantRmFinalizers: 3,
			wantStatusUpdates: []batch.JobStatus{
				{
					UncountedTerminatedPods: &batch.UncountedTerminatedPods{
						Succeeded: []types.UID{"a"},
						Failed:    []types.UID{"b"},
					},
					Active: 1,
				},
				{
					UncountedTerminatedPods: &batch.UncountedTerminatedPods{},
					Active:                  1,
					Succeeded:               1,
					Failed:                  1,
				},
			},
			wantSucceededPodsMetric: 1,
			wantFailedPodsMetric:    1,
		},
		"status update error": {
			pods: []*v1.Pod{
				buildPod().uid("a").phase(v1.PodSucceeded).trackingFinalizer().Pod,
				buildPod().uid("b").phase(v1.PodFailed).trackingFinalizer().Pod,
			},
			statusUpdateErr: mockErr,
			wantErr:         mockErr,
			wantStatusUpdates: []batch.JobStatus{
				{
					UncountedTerminatedPods: &batch.UncountedTerminatedPods{
						Succeeded: []types.UID{"a"},
						Failed:    []types.UID{"b"},
					},
				},
			},
		},
		"pod patch errors": {
			pods: []*v1.Pod{
				buildPod().uid("a").phase(v1.PodSucceeded).trackingFinalizer().Pod,
				buildPod().uid("b").phase(v1.PodFailed).trackingFinalizer().Pod,
			},
			podControlErr:    mockErr,
			wantErr:          mockErr,
			wantRmFinalizers: 2,
			wantStatusUpdates: []batch.JobStatus{
				{
					UncountedTerminatedPods: &batch.UncountedTerminatedPods{
						Succeeded: []types.UID{"a"},
						Failed:    []types.UID{"b"},
					},
				},
			},
		},
		"pod patch errors with partial success": {
			job: batch.Job{
				Status: batch.JobStatus{
					UncountedTerminatedPods: &batch.UncountedTerminatedPods{
						Succeeded: []types.UID{"a"},
						Failed:    []types.UID{"b"},
					},
				},
			},
			pods: []*v1.Pod{
				buildPod().uid("a").phase(v1.PodSucceeded).Pod,
				buildPod().uid("c").phase(v1.PodSucceeded).trackingFinalizer().Pod,
				buildPod().uid("d").phase(v1.PodFailed).trackingFinalizer().Pod,
			},
			podControlErr:    mockErr,
			wantErr:          mockErr,
			wantRmFinalizers: 2,
			wantStatusUpdates: []batch.JobStatus{
				{
					UncountedTerminatedPods: &batch.UncountedTerminatedPods{
						Succeeded: []types.UID{"c"},
						Failed:    []types.UID{"d"},
					},
					Succeeded: 1,
					Failed:    1,
				},
			},
		},
		"indexed job new successful pods": {
			job: batch.Job{
				Spec: batch.JobSpec{
					CompletionMode: &indexedCompletion,
					Completions:    ptr.To[int32](6),
				},
				Status: batch.JobStatus{
					Active: 1,
				},
			},
			pods: []*v1.Pod{
				buildPod().phase(v1.PodSucceeded).trackingFinalizer().index("1").Pod,
				buildPod().phase(v1.PodSucceeded).trackingFinalizer().index("3").Pod,
				buildPod().phase(v1.PodSucceeded).trackingFinalizer().index("3").Pod,
				buildPod().phase(v1.PodRunning).trackingFinalizer().index("5").Pod,
				buildPod().phase(v1.PodSucceeded).trackingFinalizer().Pod,
			},
			wantRmFinalizers: 4,
			wantStatusUpdates: []batch.JobStatus{
				{
					Active:                  1,
					Succeeded:               2,
					CompletedIndexes:        "1,3",
					UncountedTerminatedPods: &batch.UncountedTerminatedPods{},
				},
			},
			wantSucceededPodsMetric: 2,
		},
		"indexed job prev successful pods outside current completions index range with no new succeeded pods": {
			job: batch.Job{
				Spec: batch.JobSpec{
					CompletionMode: &indexedCompletion,
					Completions:    ptr.To[int32](2),
					Parallelism:    ptr.To[int32](2),
				},
				Status: batch.JobStatus{
					Active:           2,
					Succeeded:        1,
					CompletedIndexes: "3",
				},
			},
			pods: []*v1.Pod{
				buildPod().phase(v1.PodRunning).trackingFinalizer().index("0").Pod,
				buildPod().phase(v1.PodRunning).trackingFinalizer().index("1").Pod,
			},
			wantRmFinalizers: 0,
			wantStatusUpdates: []batch.JobStatus{
				{
					Active:                  2,
					Succeeded:               0,
					CompletedIndexes:        "",
					UncountedTerminatedPods: &batch.UncountedTerminatedPods{},
				},
			},
		},
		"indexed job prev successful pods outside current completions index range with new succeeded pods in range": {
			job: batch.Job{
				Spec: batch.JobSpec{
					CompletionMode: &indexedCompletion,
					Completions:    ptr.To[int32](2),
					Parallelism:    ptr.To[int32](2),
				},
				Status: batch.JobStatus{
					Active:           2,
					Succeeded:        1,
					CompletedIndexes: "3",
				},
			},
			pods: []*v1.Pod{
				buildPod().phase(v1.PodRunning).trackingFinalizer().index("0").Pod,
				buildPod().phase(v1.PodSucceeded).trackingFinalizer().index("1").Pod,
			},
			wantRmFinalizers: 1,
			wantStatusUpdates: []batch.JobStatus{
				{
					Active:                  2,
					Succeeded:               1,
					CompletedIndexes:        "1",
					UncountedTerminatedPods: &batch.UncountedTerminatedPods{},
				},
			},
			wantSucceededPodsMetric: 1,
		},
		"indexed job new failed pods": {
			job: batch.Job{
				Spec: batch.JobSpec{
					CompletionMode: &indexedCompletion,
					Completions:    ptr.To[int32](6),
				},
				Status: batch.JobStatus{
					Active: 1,
				},
			},
			pods: []*v1.Pod{
				buildPod().uid("a").phase(v1.PodFailed).trackingFinalizer().index("1").Pod,
				buildPod().uid("b").phase(v1.PodFailed).trackingFinalizer().index("3").Pod,
				buildPod().uid("c").phase(v1.PodFailed).trackingFinalizer().index("3").Pod,
				buildPod().uid("d").phase(v1.PodRunning).trackingFinalizer().index("5").Pod,
				buildPod().phase(v1.PodFailed).trackingFinalizer().Pod,
			},
			wantRmFinalizers: 4,
			wantStatusUpdates: []batch.JobStatus{
				{
					Active: 1,
					UncountedTerminatedPods: &batch.UncountedTerminatedPods{
						Failed: []types.UID{"a", "b", "c"},
					},
				},
				{
					Active:                  1,
					Failed:                  3,
					UncountedTerminatedPods: &batch.UncountedTerminatedPods{},
				},
			},
			wantFailedPodsMetric: 3,
		},
		"indexed job past and new pods": {
			job: batch.Job{
				Spec: batch.JobSpec{
					CompletionMode: &indexedCompletion,
					Completions:    ptr.To[int32](7),
				},
				Status: batch.JobStatus{
					Failed:           2,
					Succeeded:        5,
					CompletedIndexes: "0-2,4,6,7",
				},
			},
			pods: []*v1.Pod{
				buildPod().phase(v1.PodSucceeded).index("0").Pod,
				buildPod().phase(v1.PodFailed).index("1").Pod,
				buildPod().phase(v1.PodSucceeded).trackingFinalizer().index("1").Pod,
				buildPod().phase(v1.PodSucceeded).trackingFinalizer().index("3").Pod,
				buildPod().uid("a").phase(v1.PodFailed).trackingFinalizer().index("2").Pod,
				buildPod().uid("b").phase(v1.PodFailed).trackingFinalizer().index("5").Pod,
			},
			wantRmFinalizers: 4,
			wantStatusUpdates: []batch.JobStatus{
				{
					Succeeded:        6,
					Failed:           2,
					CompletedIndexes: "0-4,6",
					UncountedTerminatedPods: &batch.UncountedTerminatedPods{
						Failed: []types.UID{"a", "b"},
					},
				},
				{
					Succeeded:               6,
					Failed:                  4,
					CompletedIndexes:        "0-4,6",
					UncountedTerminatedPods: &batch.UncountedTerminatedPods{},
				},
			},
			wantSucceededPodsMetric: 1,
			wantFailedPodsMetric:    2,
		},
		"too many finished": {
			job: batch.Job{
				Status: batch.JobStatus{
					UncountedTerminatedPods: &batch.UncountedTerminatedPods{
						Failed: []types.UID{"a", "b"},
					},
				},
			},
			pods: func() []*v1.Pod {
				pods := make([]*v1.Pod, 500)
				for i := range pods {
					pods[i] = buildPod().uid(strconv.Itoa(i)).phase(v1.PodSucceeded).trackingFinalizer().Pod
				}
				pods = append(pods, buildPod().uid("b").phase(v1.PodFailed).trackingFinalizer().Pod)
				return pods
			}(),
			wantRmFinalizers: 499,
			wantStatusUpdates: []batch.JobStatus{
				{
					UncountedTerminatedPods: &batch.UncountedTerminatedPods{
						Succeeded: func() []types.UID {
							uids := make([]types.UID, 499)
							for i := range uids {
								uids[i] = types.UID(strconv.Itoa(i))
							}
							return uids
						}(),
						Failed: []types.UID{"b"},
					},
					Failed: 1,
				},
				{
					UncountedTerminatedPods: &batch.UncountedTerminatedPods{
						Failed: []types.UID{"b"},
					},
					Succeeded: 499,
					Failed:    1,
				},
			},
			wantSucceededPodsMetric: 499,
			wantFailedPodsMetric:    1,
		},
		"too many indexed finished": {
			job: batch.Job{
				Spec: batch.JobSpec{
					CompletionMode: &indexedCompletion,
					Completions:    ptr.To[int32](501),
				},
			},
			pods: func() []*v1.Pod {
				pods := make([]*v1.Pod, 501)
				for i := range pods {
					pods[i] = buildPod().uid(strconv.Itoa(i)).index(strconv.Itoa(i)).phase(v1.PodSucceeded).trackingFinalizer().Pod
				}
				return pods
			}(),
			wantRmFinalizers: 500,
			wantStatusUpdates: []batch.JobStatus{
				{
					UncountedTerminatedPods: &batch.UncountedTerminatedPods{},
					CompletedIndexes:        "0-499",
					Succeeded:               500,
				},
			},
			wantSucceededPodsMetric: 500,
		},
		"pod flips from failed to succeeded": {
			job: batch.Job{
				Spec: batch.JobSpec{
					Completions: ptr.To[int32](2),
					Parallelism: ptr.To[int32](2),
				},
				Status: batch.JobStatus{
					UncountedTerminatedPods: &batch.UncountedTerminatedPods{
						Failed: []types.UID{"a", "b"},
					},
				},
			},
			pods: []*v1.Pod{
				buildPod().uid("a").phase(v1.PodFailed).trackingFinalizer().Pod,
				buildPod().uid("b").phase(v1.PodSucceeded).trackingFinalizer().Pod,
			},
			finishedCond:     failedCond,
			wantRmFinalizers: 2,
			wantStatusUpdates: []batch.JobStatus{
				{
					UncountedTerminatedPods: &batch.UncountedTerminatedPods{},
					Failed:                  2,
					Conditions:              []batch.JobCondition{*failedCond},
				},
			},
			wantFailedPodsMetric: 2,
		},
		"pod is terminal; JobFailed condition is set": {
			job: batch.Job{
				Spec: batch.JobSpec{
					Completions: ptr.To[int32](1),
					Parallelism: ptr.To[int32](1),
				},
				Status: batch.JobStatus{
					UncountedTerminatedPods: &batch.UncountedTerminatedPods{
						Failed: []types.UID{"a"},
					},
				},
			},
			pods: []*v1.Pod{
				buildPod().uid("a").phase(v1.PodFailed).trackingFinalizer().Pod,
			},
			finishedCond:     failedCond,
			wantRmFinalizers: 1,
			wantStatusUpdates: []batch.JobStatus{
				{
					UncountedTerminatedPods: &batch.UncountedTerminatedPods{},
					Failed:                  1,
					Conditions:              []batch.JobCondition{*failedCond},
				},
			},
			wantFailedPodsMetric: 1,
		},
		"pod is terminating; counted as failed, but the JobFailed condition is delayed; JobPodReplacementPolicy enabled": {
			enableJobPodReplacementPolicy: true,
			job: batch.Job{
				Spec: batch.JobSpec{
					Completions: ptr.To[int32](1),
					Parallelism: ptr.To[int32](1),
				},
				Status: batch.JobStatus{
					UncountedTerminatedPods: &batch.UncountedTerminatedPods{
						Failed: []types.UID{"a"},
					},
					Conditions: []batch.JobCondition{*failureTargetCond},
				},
			},
			pods: []*v1.Pod{
				buildPod().uid("a").phase(v1.PodRunning).deletionTimestamp().Pod,
			},
			finishedCond: failedCond,
			wantStatusUpdates: []batch.JobStatus{
				{
					UncountedTerminatedPods: &batch.UncountedTerminatedPods{},
					Failed:                  1,
					Conditions:              []batch.JobCondition{*failureTargetCond},
				},
			},
			wantFailedPodsMetric: 1,
		},
		"pod is terminating; counted as failed, but the JobFailed condition is delayed; JobManagedBy enabled": {
			enableJobManagedBy: true,
			job: batch.Job{
				Spec: batch.JobSpec{
					Completions: ptr.To[int32](1),
					Parallelism: ptr.To[int32](1),
				},
				Status: batch.JobStatus{
					UncountedTerminatedPods: &batch.UncountedTerminatedPods{
						Failed: []types.UID{"a"},
					},
					Conditions: []batch.JobCondition{*failureTargetCond},
				},
			},
			pods: []*v1.Pod{
				buildPod().uid("a").phase(v1.PodRunning).deletionTimestamp().Pod,
			},
			finishedCond: failedCond,
			wantStatusUpdates: []batch.JobStatus{
				{
					UncountedTerminatedPods: &batch.UncountedTerminatedPods{},
					Failed:                  1,
					Conditions:              []batch.JobCondition{*failureTargetCond},
				},
			},
			wantFailedPodsMetric: 1,
		},
		"pod is terminating; counted as failed, JobFailed condition is not delayed; JobPodReplacementPolicy and JobManagedBy disabled": {
			job: batch.Job{
				Spec: batch.JobSpec{
					Completions: ptr.To[int32](1),
					Parallelism: ptr.To[int32](1),
				},
				Status: batch.JobStatus{
					UncountedTerminatedPods: &batch.UncountedTerminatedPods{
						Failed: []types.UID{"a"},
					},
					Conditions: []batch.JobCondition{*failureTargetCond},
				},
			},
			pods: []*v1.Pod{
				buildPod().uid("a").phase(v1.PodRunning).deletionTimestamp().Pod,
			},
			finishedCond: failedCond,
			wantStatusUpdates: []batch.JobStatus{
				{
					UncountedTerminatedPods: &batch.UncountedTerminatedPods{},
					Failed:                  1,
					Conditions:              []batch.JobCondition{*failureTargetCond},
				},
				{
					UncountedTerminatedPods: &batch.UncountedTerminatedPods{},
					Failed:                  1,
					Conditions:              []batch.JobCondition{*failureTargetCond, *failedCond},
				},
			},
			wantFailedPodsMetric: 1,
		},
		"pod is terminating; JobSuccessCriteriaMet, but JobComplete condition is delayed; JobPodReplacementPolicy enabled": {
			enableJobPodReplacementPolicy: true,
			job: batch.Job{
				Spec: batch.JobSpec{
					Completions: ptr.To[int32](1),
					Parallelism: ptr.To[int32](1),
				},
				Status: batch.JobStatus{
					UncountedTerminatedPods: &batch.UncountedTerminatedPods{
						Failed:    []types.UID{"a"},
						Succeeded: []types.UID{"b"},
					},
					Conditions: []batch.JobCondition{*succeededCond},
				},
			},
			pods: []*v1.Pod{
				buildPod().uid("a").phase(v1.PodRunning).deletionTimestamp().Pod,
			},
			finishedCond: completedCond,
			wantStatusUpdates: []batch.JobStatus{
				{
					UncountedTerminatedPods: &batch.UncountedTerminatedPods{},
					Succeeded:               1,
					Failed:                  1,
					Conditions:              []batch.JobCondition{*succeededCond},
				},
			},
			wantFailedPodsMetric:    1,
			wantSucceededPodsMetric: 1,
		},
		"pod is terminating; JobSuccessCriteriaMet, but JobComplete condition is delayed; JobManagedBy enabled": {
			enableJobManagedBy: true,
			job: batch.Job{
				Spec: batch.JobSpec{
					Completions: ptr.To[int32](1),
					Parallelism: ptr.To[int32](1),
				},
				Status: batch.JobStatus{
					UncountedTerminatedPods: &batch.UncountedTerminatedPods{
						Failed:    []types.UID{"a"},
						Succeeded: []types.UID{"b"},
					},
					Conditions: []batch.JobCondition{*succeededCond},
				},
			},
			pods: []*v1.Pod{
				buildPod().uid("a").phase(v1.PodRunning).deletionTimestamp().Pod,
			},
			finishedCond: completedCond,
			wantStatusUpdates: []batch.JobStatus{
				{
					UncountedTerminatedPods: &batch.UncountedTerminatedPods{},
					Succeeded:               1,
					Failed:                  1,
					Conditions:              []batch.JobCondition{*succeededCond},
				},
			},
			wantFailedPodsMetric:    1,
			wantSucceededPodsMetric: 1,
		},
		"pod is terminating; JobSuccessCriteriaMet, JobComplete condition is not delayed; JobPodReplacementPolicy and JobManagedBy disabled": {
			enableJobSuccessPolicy: true,
			job: batch.Job{
				Spec: batch.JobSpec{
					Completions: ptr.To[int32](1),
					Parallelism: ptr.To[int32](1),
				},
				Status: batch.JobStatus{
					UncountedTerminatedPods: &batch.UncountedTerminatedPods{
						Failed:    []types.UID{"a"},
						Succeeded: []types.UID{"b"},
					},
					Conditions: []batch.JobCondition{*succeededCond},
				},
			},
			pods: []*v1.Pod{
				buildPod().uid("a").phase(v1.PodRunning).deletionTimestamp().Pod,
			},
			finishedCond: completedCond,
			wantStatusUpdates: []batch.JobStatus{
				{
					UncountedTerminatedPods: &batch.UncountedTerminatedPods{},
					Succeeded:               1,
					Failed:                  1,
					Conditions:              []batch.JobCondition{*succeededCond},
				},
				{
					UncountedTerminatedPods: &batch.UncountedTerminatedPods{},
					Succeeded:               1,
					Failed:                  1,
					Conditions:              []batch.JobCondition{*succeededCond, *completedCond},
					CompletionTime:          ptr.To(metav1.NewTime(now)),
				},
			},
			wantFailedPodsMetric:    1,
			wantSucceededPodsMetric: 1,
		},

		"indexed job with a failed pod with delayed finalizer removal; the pod is not counted": {
			enableJobBackoffLimitPerIndex: true,
			job: batch.Job{
				Spec: batch.JobSpec{
					CompletionMode:       &indexedCompletion,
					Completions:          ptr.To[int32](6),
					BackoffLimitPerIndex: ptr.To[int32](1),
				},
			},
			pods: []*v1.Pod{
				buildPod().uid("a").phase(v1.PodFailed).indexFailureCount("0").trackingFinalizer().index("1").Pod,
			},
			wantStatusUpdates: []batch.JobStatus{
				{
					UncountedTerminatedPods: &batch.UncountedTerminatedPods{},
					FailedIndexes:           ptr.To(""),
				},
			},
		},
		"indexed job with a failed pod which is recreated by a running pod; the pod is counted": {
			enableJobBackoffLimitPerIndex: true,
			job: batch.Job{
				Spec: batch.JobSpec{
					CompletionMode:       &indexedCompletion,
					Completions:          ptr.To[int32](6),
					BackoffLimitPerIndex: ptr.To[int32](1),
				},
				Status: batch.JobStatus{
					Active: 1,
				},
			},
			pods: []*v1.Pod{
				buildPod().uid("a1").phase(v1.PodFailed).indexFailureCount("0").trackingFinalizer().index("1").Pod,
				buildPod().uid("a2").phase(v1.PodRunning).indexFailureCount("1").trackingFinalizer().index("1").Pod,
			},
			wantRmFinalizers: 1,
			wantStatusUpdates: []batch.JobStatus{
				{
					Active: 1,
					UncountedTerminatedPods: &batch.UncountedTerminatedPods{
						Failed: []types.UID{"a1"},
					},
					FailedIndexes: ptr.To(""),
				},
				{
					Active:                  1,
					Failed:                  1,
					UncountedTerminatedPods: &batch.UncountedTerminatedPods{},
					FailedIndexes:           ptr.To(""),
				},
			},
			wantFailedPodsMetric: 1,
		},
		"indexed job with a failed pod for a failed index; the pod is counted": {
			enableJobBackoffLimitPerIndex: true,
			job: batch.Job{
				Spec: batch.JobSpec{
					CompletionMode:       &indexedCompletion,
					Completions:          ptr.To[int32](6),
					BackoffLimitPerIndex: ptr.To[int32](1),
				},
			},
			pods: []*v1.Pod{
				buildPod().uid("a").phase(v1.PodFailed).indexFailureCount("1").trackingFinalizer().index("1").Pod,
			},
			wantRmFinalizers: 1,
			wantStatusUpdates: []batch.JobStatus{
				{
					FailedIndexes: ptr.To("1"),
					UncountedTerminatedPods: &batch.UncountedTerminatedPods{
						Failed: []types.UID{"a"},
					},
				},
				{
					Failed:                  1,
					FailedIndexes:           ptr.To("1"),
					UncountedTerminatedPods: &batch.UncountedTerminatedPods{},
				},
			},
			wantFailedPodsMetric: 1,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			featuregatetesting.SetFeatureGateDuringTest(t, feature.DefaultFeatureGate, features.JobBackoffLimitPerIndex, tc.enableJobBackoffLimitPerIndex)
			featuregatetesting.SetFeatureGateDuringTest(t, feature.DefaultFeatureGate, features.JobSuccessPolicy, tc.enableJobSuccessPolicy)
			featuregatetesting.SetFeatureGateDuringTest(t, feature.DefaultFeatureGate, features.JobPodReplacementPolicy, tc.enableJobPodReplacementPolicy)
			featuregatetesting.SetFeatureGateDuringTest(t, feature.DefaultFeatureGate, features.JobManagedBy, tc.enableJobManagedBy)

			clientSet := clientset.NewForConfigOrDie(&restclient.Config{Host: "", ContentConfig: restclient.ContentConfig{GroupVersion: &schema.GroupVersion{Group: "", Version: "v1"}}})
			manager, _ := newControllerFromClientWithClock(ctx, t, clientSet, controller.NoResyncPeriodFunc, fakeClock)
			fakePodControl := controller.FakePodControl{Err: tc.podControlErr}
			metrics.JobPodsFinished.Reset()
			manager.podControl = &fakePodControl
			var statusUpdates []batch.JobStatus
			manager.updateStatusHandler = func(ctx context.Context, job *batch.Job) (*batch.Job, error) {
				statusUpdates = append(statusUpdates, *job.Status.DeepCopy())
				return job, tc.statusUpdateErr
			}
			job := tc.job.DeepCopy()
			if job.Status.UncountedTerminatedPods == nil {
				job.Status.UncountedTerminatedPods = &batch.UncountedTerminatedPods{}
			}
			jobCtx := &syncJobCtx{
				job:                  job,
				pods:                 tc.pods,
				uncounted:            newUncountedTerminatedPods(*job.Status.UncountedTerminatedPods),
				expectedRmFinalizers: tc.expectedRmFinalizers,
				finishedCondition:    tc.finishedCond,
			}
			jobCtx.activePods = controller.FilterActivePods(logger, tc.pods)
			if isIndexedJob(job) {
				jobCtx.succeededIndexes = parseIndexesFromString(logger, job.Status.CompletedIndexes, int(*job.Spec.Completions))
				if tc.enableJobBackoffLimitPerIndex && job.Spec.BackoffLimitPerIndex != nil {
					jobCtx.failedIndexes = calculateFailedIndexes(logger, job, tc.pods)
					jobCtx.podsWithDelayedDeletionPerIndex = getPodsWithDelayedDeletionPerIndex(logger, jobCtx)
				}
			}
			jobCtx.terminating = ptr.To(controller.CountTerminatingPods(tc.pods))

			err := manager.trackJobStatusAndRemoveFinalizers(ctx, jobCtx, tc.needsFlush)
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("Got error %v, want %v", err, tc.wantErr)
			}
			if diff := cmp.Diff(tc.wantStatusUpdates, statusUpdates,
				cmpopts.IgnoreFields(batch.JobCondition{}, "LastProbeTime", "LastTransitionTime")); diff != "" {
				t.Errorf("Unexpected status updates (-want,+got):\n%s", diff)
			}
			rmFinalizers := len(fakePodControl.Patches)
			if rmFinalizers != tc.wantRmFinalizers {
				t.Errorf("Removed %d finalizers, want %d", rmFinalizers, tc.wantRmFinalizers)
			}
			if tc.wantErr == nil {
				completionMode := completionModeStr(job)
				v, err := metricstestutil.GetCounterMetricValue(metrics.JobPodsFinished.WithLabelValues(completionMode, metrics.Succeeded))
				if err != nil {
					t.Fatalf("Obtaining succeeded job_pods_finished_total: %v", err)
				}
				if float64(tc.wantSucceededPodsMetric) != v {
					t.Errorf("Metric reports %.0f succeeded pods, want %d", v, tc.wantSucceededPodsMetric)
				}
				v, err = metricstestutil.GetCounterMetricValue(metrics.JobPodsFinished.WithLabelValues(completionMode, metrics.Failed))
				if err != nil {
					t.Fatalf("Obtaining failed job_pods_finished_total: %v", err)
				}
				if float64(tc.wantFailedPodsMetric) != v {
					t.Errorf("Metric reports %.0f failed pods, want %d", v, tc.wantFailedPodsMetric)
				}
			}
		})
	}
}

// TestSyncJobPastDeadline verifies tracking of active deadline in a single syncJob call.
func TestSyncJobPastDeadline(t *testing.T) {
	_, ctx := ktesting.NewTestContext(t)
	testCases := map[string]struct {
		// job setup
		parallelism           int32
		completions           int32
		activeDeadlineSeconds int64
		startTime             int64
		backoffLimit          int32
		suspend               bool

		// pod setup
		activePods    int
		succeededPods int
		failedPods    int

		// expectations
		expectedDeletions  int32
		expectedActive     int32
		expectedSucceeded  int32
		expectedFailed     int32
		expectedConditions []batch.JobCondition

		// features
		enableJobPodReplacementPolicy bool
		enableJobManagedBy            bool
	}{
		"activeDeadlineSeconds less than single pod execution": {
			parallelism:           1,
			completions:           1,
			activeDeadlineSeconds: 10,
			startTime:             15,
			backoffLimit:          6,
			activePods:            1,
			expectedDeletions:     1,
			expectedFailed:        1,
			expectedConditions: []batch.JobCondition{
				{
					Type:    batch.JobFailed,
					Status:  v1.ConditionTrue,
					Reason:  batch.JobReasonDeadlineExceeded,
					Message: "Job was active longer than specified deadline",
				},
			},
		},
		"activeDeadlineSeconds bigger than single pod execution": {
			parallelism:           1,
			completions:           2,
			activeDeadlineSeconds: 10,
			startTime:             15,
			backoffLimit:          6,
			activePods:            1,
			succeededPods:         1,
			expectedDeletions:     1,
			expectedSucceeded:     1,
			expectedFailed:        1,
			expectedConditions: []batch.JobCondition{
				{
					Type:    batch.JobFailed,
					Status:  v1.ConditionTrue,
					Reason:  batch.JobReasonDeadlineExceeded,
					Message: "Job was active longer than specified deadline",
				},
			},
		},
		"activeDeadlineSeconds times-out before any pod starts": {
			parallelism:           1,
			completions:           1,
			activeDeadlineSeconds: 10,
			startTime:             10,
			backoffLimit:          6,
			expectedConditions: []batch.JobCondition{
				{
					Type:    batch.JobFailed,
					Status:  v1.ConditionTrue,
					Reason:  batch.JobReasonDeadlineExceeded,
					Message: "Job was active longer than specified deadline",
				},
			},
		},
		"activeDeadlineSeconds with backofflimit reach": {
			parallelism:           1,
			completions:           1,
			activeDeadlineSeconds: 1,
			startTime:             10,
			failedPods:            1,
			expectedFailed:        1,
			expectedConditions: []batch.JobCondition{
				{
					Type:    batch.JobFailed,
					Status:  v1.ConditionTrue,
					Reason:  batch.JobReasonBackoffLimitExceeded,
					Message: "Job has reached the specified backoff limit",
				},
			},
		},
		"activeDeadlineSeconds is not triggered when Job is suspended": {
			suspend:               true,
			parallelism:           1,
			completions:           2,
			activeDeadlineSeconds: 10,
			startTime:             15,
			backoffLimit:          6,
			expectedConditions: []batch.JobCondition{
				{
					Type:    batch.JobSuspended,
					Status:  v1.ConditionTrue,
					Reason:  "JobSuspended",
					Message: "Job suspended",
				},
			},
		},
		"activeDeadlineSeconds exceeded; JobPodReplacementPolicy enabled": {
			enableJobPodReplacementPolicy: true,
			parallelism:                   1,
			completions:                   2,
			activeDeadlineSeconds:         10,
			startTime:                     15,
			backoffLimit:                  6,
			activePods:                    1,
			succeededPods:                 1,
			expectedDeletions:             1,
			expectedSucceeded:             1,
			expectedFailed:                1,
			expectedConditions: []batch.JobCondition{
				{
					Type:    batch.JobFailureTarget,
					Status:  v1.ConditionTrue,
					Reason:  batch.JobReasonDeadlineExceeded,
					Message: "Job was active longer than specified deadline",
				},
			},
		},
		"activeDeadlineSeconds exceeded; JobManagedBy enabled": {
			enableJobManagedBy: true,

			parallelism:           1,
			completions:           2,
			activeDeadlineSeconds: 10,
			startTime:             15,
			backoffLimit:          6,
			activePods:            1,
			succeededPods:         1,
			expectedDeletions:     1,
			expectedSucceeded:     1,
			expectedFailed:        1,
			expectedConditions: []batch.JobCondition{
				{
					Type:    batch.JobFailureTarget,
					Status:  v1.ConditionTrue,
					Reason:  batch.JobReasonDeadlineExceeded,
					Message: "Job was active longer than specified deadline",
				},
			},
		},
		"activeDeadlineSeconds exceeded and backofflimit reached; JobManagedBy enabled": {
			enableJobManagedBy: true,

			parallelism:           1,
			completions:           1,
			activeDeadlineSeconds: 1,
			startTime:             10,
			failedPods:            1,
			expectedFailed:        1,
			expectedConditions: []batch.JobCondition{
				{
					Type:    batch.JobFailureTarget,
					Status:  v1.ConditionTrue,
					Reason:  batch.JobReasonBackoffLimitExceeded,
					Message: "Job has reached the specified backoff limit",
				},
				{
					Type:    batch.JobFailed,
					Status:  v1.ConditionTrue,
					Reason:  batch.JobReasonBackoffLimitExceeded,
					Message: "Job has reached the specified backoff limit",
				},
			},
		},
	}

	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			featuregatetesting.SetFeatureGateDuringTest(t, feature.DefaultFeatureGate, features.JobManagedBy, tc.enableJobManagedBy)
			featuregatetesting.SetFeatureGateDuringTest(t, feature.DefaultFeatureGate, features.JobPodReplacementPolicy, tc.enableJobPodReplacementPolicy)
			// job manager setup
			clientSet := clientset.NewForConfigOrDie(&restclient.Config{Host: "", ContentConfig: restclient.ContentConfig{GroupVersion: &schema.GroupVersion{Group: "", Version: "v1"}}})
			manager, sharedInformerFactory := newControllerFromClient(ctx, t, clientSet, controller.NoResyncPeriodFunc)
			fakePodControl := controller.FakePodControl{}
			manager.podControl = &fakePodControl
			manager.podStoreSynced = alwaysReady
			manager.jobStoreSynced = alwaysReady
			var actual *batch.Job
			manager.updateStatusHandler = func(ctx context.Context, job *batch.Job) (*batch.Job, error) {
				actual = job
				return job, nil
			}

			// job & pods setup
			job := newJob(tc.parallelism, tc.completions, tc.backoffLimit, batch.NonIndexedCompletion)
			job.Spec.ActiveDeadlineSeconds = &tc.activeDeadlineSeconds
			job.Spec.Suspend = ptr.To(tc.suspend)
			start := metav1.Unix(metav1.Now().Time.Unix()-tc.startTime, 0)
			job.Status.StartTime = &start
			sharedInformerFactory.Batch().V1().Jobs().Informer().GetIndexer().Add(job)
			podIndexer := sharedInformerFactory.Core().V1().Pods().Informer().GetIndexer()
			setPodsStatuses(podIndexer, job, 0, tc.activePods, tc.succeededPods, tc.failedPods, 0, 0)

			// run
			err := manager.syncJob(ctx, testutil.GetKey(job, t))
			if err != nil {
				t.Errorf("Unexpected error when syncing jobs %v", err)
			}
			// validate created/deleted pods
			if int32(len(fakePodControl.Templates)) != 0 {
				t.Errorf("Unexpected number of creates.  Expected 0, saw %d\n", len(fakePodControl.Templates))
			}
			if int32(len(fakePodControl.DeletePodName)) != tc.expectedDeletions {
				t.Errorf("Unexpected number of deletes.  Expected %d, saw %d\n", tc.expectedDeletions, len(fakePodControl.DeletePodName))
			}
			// validate status
			if actual.Status.Active != tc.expectedActive {
				t.Errorf("Unexpected number of active pods.  Expected %d, saw %d\n", tc.expectedActive, actual.Status.Active)
			}
			if actual.Status.Succeeded != tc.expectedSucceeded {
				t.Errorf("Unexpected number of succeeded pods.  Expected %d, saw %d\n", tc.expectedSucceeded, actual.Status.Succeeded)
			}
			if actual.Status.Failed != tc.expectedFailed {
				t.Errorf("Unexpected number of failed pods.  Expected %d, saw %d\n", tc.expectedFailed, actual.Status.Failed)
			}
			if actual.Status.StartTime == nil {
				t.Error("Missing .status.startTime")
			}
			// validate conditions
			if diff := cmp.Diff(tc.expectedConditions, actual.Status.Conditions, cmpopts.IgnoreFields(batch.JobCondition{}, "LastProbeTime", "LastTransitionTime")); diff != "" {
				t.Errorf("unexpected conditions (-want,+got):\n%s", diff)
			}
		})
	}
}

func getCondition(job *batch.Job, condition batch.JobConditionType, status v1.ConditionStatus, reason string) bool {
	for _, v := range job.Status.Conditions {
		if v.Type == condition && v.Status == status && v.Reason == reason {
			return true
		}
	}
	return false
}

// TestPastDeadlineJobFinished ensures that a Job is correctly tracked until
// reaching the active deadline, at which point it is marked as Failed.
func TestPastDeadlineJobFinished(t *testing.T) {
	_, ctx := ktesting.NewTestContext(t)
	ctx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	clientset := fake.NewClientset()
	fakeClock := clocktesting.NewFakeClock(time.Now().Truncate(time.Second))
	manager, sharedInformerFactory := newControllerFromClientWithClock(ctx, t, clientset, controller.NoResyncPeriodFunc, fakeClock)
	manager.podStoreSynced = alwaysReady
	manager.jobStoreSynced = alwaysReady
	manager.expectations = FakeJobExpectations{
		controller.NewControllerExpectations(), true, func() {
		},
	}
	sharedInformerFactory.Start(ctx.Done())
	sharedInformerFactory.WaitForCacheSync(ctx.Done())

	go manager.Run(ctx, 1)

	tests := []struct {
		name         string
		setStartTime bool
		jobName      string
	}{
		{
			name:         "New job created without start time being set",
			setStartTime: false,
			jobName:      "job1",
		},
		{
			name:         "New job created with start time being set",
			setStartTime: true,
			jobName:      "job2",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			job := newJobWithName(tc.jobName, 1, 1, 6, batch.NonIndexedCompletion)
			job.Spec.ActiveDeadlineSeconds = ptr.To[int64](1)
			if tc.setStartTime {
				start := metav1.NewTime(fakeClock.Now())
				job.Status.StartTime = &start
			}

			_, err := clientset.BatchV1().Jobs(job.GetNamespace()).Create(ctx, job, metav1.CreateOptions{})
			if err != nil {
				t.Errorf("Could not create Job: %v", err)
			}

			var j *batch.Job
			err = wait.PollUntilContextTimeout(ctx, 200*time.Microsecond, 3*time.Second, true, func(ctx context.Context) (done bool, err error) {
				j, err = clientset.BatchV1().Jobs(metav1.NamespaceDefault).Get(ctx, job.GetName(), metav1.GetOptions{})
				if err != nil {
					return false, err
				}
				return j.Status.StartTime != nil, nil
			})
			if err != nil {
				t.Errorf("Job failed to ensure that start time was set: %v", err)
			}
			err = wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, 3*time.Second, false, func(ctx context.Context) (done bool, err error) {
				j, err = clientset.BatchV1().Jobs(metav1.NamespaceDefault).Get(ctx, job.GetName(), metav1.GetOptions{})
				if err != nil {
					return false, nil
				}
				if getCondition(j, batch.JobFailed, v1.ConditionTrue, batch.JobReasonDeadlineExceeded) {
					if manager.clock.Since(j.Status.StartTime.Time) < time.Duration(*j.Spec.ActiveDeadlineSeconds)*time.Second {
						return true, errors.New("Job contains DeadlineExceeded condition earlier than expected")
					}
					return true, nil
				}
				manager.clock.Sleep(100 * time.Millisecond)
				return false, nil
			})
			if err != nil {
				t.Errorf("Job failed to enforce activeDeadlineSeconds configuration. Expected condition with Reason 'DeadlineExceeded' was not found in %v", j.Status)
			}
		})
	}
}

func TestSingleJobFailedCondition(t *testing.T) {
	_, ctx := ktesting.NewTestContext(t)
	clientset := clientset.NewForConfigOrDie(&restclient.Config{Host: "", ContentConfig: restclient.ContentConfig{GroupVersion: &schema.GroupVersion{Group: "", Version: "v1"}}})
	manager, sharedInformerFactory := newControllerFromClient(ctx, t, clientset, controller.NoResyncPeriodFunc)
	fakePodControl := controller.FakePodControl{}
	manager.podControl = &fakePodControl
	manager.podStoreSynced = alwaysReady
	manager.jobStoreSynced = alwaysReady
	var actual *batch.Job
	manager.updateStatusHandler = func(ctx context.Context, job *batch.Job) (*batch.Job, error) {
		actual = job
		return job, nil
	}

	job := newJob(1, 1, 6, batch.NonIndexedCompletion)
	job.Spec.ActiveDeadlineSeconds = ptr.To[int64](10)
	start := metav1.Unix(metav1.Now().Time.Unix()-15, 0)
	job.Status.StartTime = &start
	job.Status.Conditions = append(job.Status.Conditions, *newCondition(batch.JobFailed, v1.ConditionFalse, "DeadlineExceeded", "Job was active longer than specified deadline", realClock.Now()))
	sharedInformerFactory.Batch().V1().Jobs().Informer().GetIndexer().Add(job)
	err := manager.syncJob(ctx, testutil.GetKey(job, t))
	if err != nil {
		t.Errorf("Unexpected error when syncing jobs %v", err)
	}
	if len(fakePodControl.DeletePodName) != 0 {
		t.Errorf("Unexpected number of deletes.  Expected %d, saw %d\n", 0, len(fakePodControl.DeletePodName))
	}
	if actual == nil {
		t.Error("Expected job modification\n")
	}
	failedConditions := getConditionsByType(actual.Status.Conditions, batch.JobFailed)
	if len(failedConditions) != 1 {
		t.Error("Unexpected number of failed conditions\n")
	}
	if failedConditions[0].Status != v1.ConditionTrue {
		t.Errorf("Unexpected status for the failed condition. Expected: %v, saw %v\n", v1.ConditionTrue, failedConditions[0].Status)
	}

}

func TestSyncJobComplete(t *testing.T) {
	_, ctx := ktesting.NewTestContext(t)
	clientset := clientset.NewForConfigOrDie(&restclient.Config{Host: "", ContentConfig: restclient.ContentConfig{GroupVersion: &schema.GroupVersion{Group: "", Version: "v1"}}})
	manager, sharedInformerFactory := newControllerFromClient(ctx, t, clientset, controller.NoResyncPeriodFunc)
	fakePodControl := controller.FakePodControl{}
	manager.podControl = &fakePodControl
	manager.podStoreSynced = alwaysReady
	manager.jobStoreSynced = alwaysReady

	job := newJob(1, 1, 6, batch.NonIndexedCompletion)
	job.Status.Conditions = append(job.Status.Conditions, *newCondition(batch.JobComplete, v1.ConditionTrue, "", "", realClock.Now()))
	sharedInformerFactory.Batch().V1().Jobs().Informer().GetIndexer().Add(job)
	err := manager.syncJob(ctx, testutil.GetKey(job, t))
	if err != nil {
		t.Fatalf("Unexpected error when syncing jobs %v", err)
	}
	actual, err := manager.jobLister.Jobs(job.Namespace).Get(job.Name)
	if err != nil {
		t.Fatalf("Unexpected error when trying to get job from the store: %v", err)
	}
	// Verify that after syncing a complete job, the conditions are the same.
	if got, expected := len(actual.Status.Conditions), 1; got != expected {
		t.Fatalf("Unexpected job status conditions amount; expected %d, got %d", expected, got)
	}
}

func TestSyncJobDeleted(t *testing.T) {
	_, ctx := ktesting.NewTestContext(t)
	clientset := clientset.NewForConfigOrDie(&restclient.Config{Host: "", ContentConfig: restclient.ContentConfig{GroupVersion: &schema.GroupVersion{Group: "", Version: "v1"}}})
	manager, _ := newControllerFromClient(ctx, t, clientset, controller.NoResyncPeriodFunc)
	fakePodControl := controller.FakePodControl{}
	manager.podControl = &fakePodControl
	manager.podStoreSynced = alwaysReady
	manager.jobStoreSynced = alwaysReady
	manager.updateStatusHandler = func(ctx context.Context, job *batch.Job) (*batch.Job, error) {
		return job, nil
	}
	job := newJob(2, 2, 6, batch.NonIndexedCompletion)
	if err := manager.syncJob(ctx, testutil.GetKey(job, t)); err != nil {
		t.Fatalf("error %v while reconciling the job %v", err, testutil.GetKey(job, t))
	}
	if len(fakePodControl.Templates) != 0 {
		t.Errorf("Unexpected number of creates.  Expected %d, saw %d\n", 0, len(fakePodControl.Templates))
	}
	if len(fakePodControl.DeletePodName) != 0 {
		t.Errorf("Unexpected number of deletes.  Expected %d, saw %d\n", 0, len(fakePodControl.DeletePodName))
	}
}

func TestSyncJobWhenManagedBy(t *testing.T) {
	_, ctx := ktesting.NewTestContext(t)
	now := metav1.Now()
	baseJob := batch.Job{
		TypeMeta: metav1.TypeMeta{Kind: "Job"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "foobar",
			Namespace: metav1.NamespaceDefault,
		},
		Spec: batch.JobSpec{
			Template: v1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"foo": "bar",
					},
				},
				Spec: v1.PodSpec{
					Containers: []v1.Container{
						{Image: "foo/bar"},
					},
				},
			},
			Parallelism:  ptr.To[int32](2),
			Completions:  ptr.To[int32](2),
			BackoffLimit: ptr.To[int32](6),
		},
		Status: batch.JobStatus{
			Active:    1,
			Ready:     ptr.To[int32](1),
			StartTime: &now,
		},
	}

	testCases := map[string]struct {
		enableJobManagedBy bool
		job                batch.Job
		wantStatus         batch.JobStatus
	}{
		"job with custom value of managedBy; feature enabled; the status is unchanged": {
			enableJobManagedBy: true,
			job: func() batch.Job {
				job := baseJob.DeepCopy()
				job.Spec.ManagedBy = ptr.To("custom-managed-by")
				return *job
			}(),
			wantStatus: baseJob.Status,
		},
		"job with well known value of the managedBy; feature enabled; the status is updated": {
			enableJobManagedBy: true,
			job: func() batch.Job {
				job := baseJob.DeepCopy()
				job.Spec.ManagedBy = ptr.To(batch.JobControllerName)
				return *job
			}(),
			wantStatus: batch.JobStatus{
				Active:                  2,
				Ready:                   ptr.To[int32](0),
				StartTime:               &now,
				Terminating:             ptr.To[int32](0),
				UncountedTerminatedPods: &batch.UncountedTerminatedPods{},
			},
		},
		"job with custom value of managedBy; feature disabled; the status is updated": {
			job: func() batch.Job {
				job := baseJob.DeepCopy()
				job.Spec.ManagedBy = ptr.To("custom-managed-by")
				return *job
			}(),
			wantStatus: batch.JobStatus{
				Active:                  2,
				Ready:                   ptr.To[int32](0),
				StartTime:               &now,
				Terminating:             ptr.To[int32](0),
				UncountedTerminatedPods: &batch.UncountedTerminatedPods{},
			},
		},
		"job without the managedBy; feature enabled; the status is updated": {
			enableJobManagedBy: true,
			job:                baseJob,
			wantStatus: batch.JobStatus{
				Active:                  2,
				Ready:                   ptr.To[int32](0),
				StartTime:               &now,
				Terminating:             ptr.To[int32](0),
				UncountedTerminatedPods: &batch.UncountedTerminatedPods{},
			},
		},
	}
	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			featuregatetesting.SetFeatureGateDuringTest(t, feature.DefaultFeatureGate, features.JobManagedBy, tc.enableJobManagedBy)

			clientset := clientset.NewForConfigOrDie(&restclient.Config{Host: "", ContentConfig: restclient.ContentConfig{GroupVersion: &schema.GroupVersion{Group: "", Version: "v1"}}})
			manager, sharedInformerFactory := newControllerFromClient(ctx, t, clientset, controller.NoResyncPeriodFunc)
			fakePodControl := controller.FakePodControl{}
			manager.podControl = &fakePodControl
			manager.podStoreSynced = alwaysReady
			manager.jobStoreSynced = alwaysReady
			job := &tc.job

			actual := job
			manager.updateStatusHandler = func(_ context.Context, job *batch.Job) (*batch.Job, error) {
				actual = job
				return job, nil
			}
			if err := sharedInformerFactory.Batch().V1().Jobs().Informer().GetIndexer().Add(job); err != nil {
				t.Fatalf("error %v while adding the %v job to the index", err, klog.KObj(job))
			}

			if err := manager.syncJob(ctx, testutil.GetKey(job, t)); err != nil {
				t.Fatalf("error %v while reconciling the job %v", err, testutil.GetKey(job, t))
			}

			if diff := cmp.Diff(tc.wantStatus, actual.Status); diff != "" {
				t.Errorf("Unexpected job status (-want,+got):\n%s", diff)
			}
		})
	}
}

func TestSyncJobWithJobPodFailurePolicy(t *testing.T) {
	_, ctx := ktesting.NewTestContext(t)
	now := metav1.Now()
	indexedCompletionMode := batch.IndexedCompletion
	validObjectMeta := metav1.ObjectMeta{
		Name:      "foobar",
		UID:       uuid.NewUUID(),
		Namespace: metav1.NamespaceDefault,
	}
	validSelector := &metav1.LabelSelector{
		MatchLabels: map[string]string{"foo": "bar"},
	}
	validTemplate := v1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{
				"foo": "bar",
			},
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{Image: "foo/bar"},
			},
		},
	}

	onExitCodeRules := []batch.PodFailurePolicyRule{
		{
			Action: batch.PodFailurePolicyActionIgnore,
			OnExitCodes: &batch.PodFailurePolicyOnExitCodesRequirement{
				Operator: batch.PodFailurePolicyOnExitCodesOpIn,
				Values:   []int32{1, 2, 3},
			},
		},
		{
			Action: batch.PodFailurePolicyActionFailJob,
			OnExitCodes: &batch.PodFailurePolicyOnExitCodesRequirement{
				Operator: batch.PodFailurePolicyOnExitCodesOpIn,
				Values:   []int32{5, 6, 7},
			},
		},
	}

	testCases := map[string]struct {
		enableJobPodReplacementPolicy bool
		enableJobManagedBy            bool
		job                           batch.Job
		pods                          []v1.Pod
		wantConditions                []batch.JobCondition
		wantStatusFailed              int32
		wantStatusActive              int32
		wantStatusSucceeded           int32
		wantStatusTerminating         *int32
	}{
		"default handling for pod failure if the container matching the exit codes does not match the containerName restriction": {
			job: batch.Job{
				TypeMeta:   metav1.TypeMeta{Kind: "Job"},
				ObjectMeta: validObjectMeta,
				Spec: batch.JobSpec{
					Selector:     validSelector,
					Template:     validTemplate,
					Parallelism:  ptr.To[int32](1),
					Completions:  ptr.To[int32](1),
					BackoffLimit: ptr.To[int32](6),
					PodFailurePolicy: &batch.PodFailurePolicy{
						Rules: []batch.PodFailurePolicyRule{
							{
								Action: batch.PodFailurePolicyActionIgnore,
								OnExitCodes: &batch.PodFailurePolicyOnExitCodesRequirement{
									ContainerName: ptr.To("main-container"),
									Operator:      batch.PodFailurePolicyOnExitCodesOpIn,
									Values:        []int32{1, 2, 3},
								},
							},
							{
								Action: batch.PodFailurePolicyActionFailJob,
								OnExitCodes: &batch.PodFailurePolicyOnExitCodesRequirement{
									ContainerName: ptr.To("main-container"),
									Operator:      batch.PodFailurePolicyOnExitCodesOpIn,
									Values:        []int32{5, 6, 7},
								},
							},
						},
					},
				},
			},
			pods: []v1.Pod{
				{
					Status: v1.PodStatus{
						Phase: v1.PodFailed,
						ContainerStatuses: []v1.ContainerStatus{
							{
								Name: "monitoring-container",
								State: v1.ContainerState{
									Terminated: &v1.ContainerStateTerminated{
										ExitCode: 5,
									},
								},
							},
							{
								Name: "main-container",
								State: v1.ContainerState{
									Terminated: &v1.ContainerStateTerminated{
										ExitCode:   42,
										FinishedAt: testFinishedAt,
									},
								},
							},
						},
					},
				},
			},
			wantConditions:      nil,
			wantStatusActive:    1,
			wantStatusSucceeded: 0,
			wantStatusFailed:    1,
		},
		"running pod should not result in job fail based on OnExitCodes": {
			job: batch.Job{
				TypeMeta:   metav1.TypeMeta{Kind: "Job"},
				ObjectMeta: validObjectMeta,
				Spec: batch.JobSpec{
					Selector:     validSelector,
					Template:     validTemplate,
					Parallelism:  ptr.To[int32](1),
					Completions:  ptr.To[int32](1),
					BackoffLimit: ptr.To[int32](6),
					PodFailurePolicy: &batch.PodFailurePolicy{
						Rules: onExitCodeRules,
					},
				},
			},
			pods: []v1.Pod{
				{
					Status: v1.PodStatus{
						Phase: v1.PodRunning,
						ContainerStatuses: []v1.ContainerStatus{
							{
								Name: "main-container",
								State: v1.ContainerState{
									Terminated: &v1.ContainerStateTerminated{
										ExitCode: 5,
									},
								},
							},
						},
					},
				},
			},
			wantConditions:      nil,
			wantStatusActive:    1,
			wantStatusFailed:    0,
			wantStatusSucceeded: 0,
		},
		"fail job based on OnExitCodes": {
			job: batch.Job{
				TypeMeta:   metav1.TypeMeta{Kind: "Job"},
				ObjectMeta: validObjectMeta,
				Spec: batch.JobSpec{
					Selector:     validSelector,
					Template:     validTemplate,
					Parallelism:  ptr.To[int32](1),
					Completions:  ptr.To[int32](1),
					BackoffLimit: ptr.To[int32](6),
					PodFailurePolicy: &batch.PodFailurePolicy{
						Rules: onExitCodeRules,
					},
				},
			},
			pods: []v1.Pod{
				{
					Status: v1.PodStatus{
						Phase: v1.PodFailed,
						ContainerStatuses: []v1.ContainerStatus{
							{
								Name: "main-container",
								State: v1.ContainerState{
									Terminated: &v1.ContainerStateTerminated{
										ExitCode: 5,
									},
								},
							},
						},
					},
				},
			},
			wantConditions: []batch.JobCondition{
				{
					Type:    batch.JobFailureTarget,
					Status:  v1.ConditionTrue,
					Reason:  batch.JobReasonPodFailurePolicy,
					Message: "Container main-container for pod default/mypod-0 failed with exit code 5 matching FailJob rule at index 1",
				},
				{
					Type:    batch.JobFailed,
					Status:  v1.ConditionTrue,
					Reason:  batch.JobReasonPodFailurePolicy,
					Message: "Container main-container for pod default/mypod-0 failed with exit code 5 matching FailJob rule at index 1",
				},
			},
			wantStatusActive:    0,
			wantStatusFailed:    1,
			wantStatusSucceeded: 0,
		},
		"job marked already as failure target with failed pod": {
			job: batch.Job{
				TypeMeta:   metav1.TypeMeta{Kind: "Job"},
				ObjectMeta: validObjectMeta,
				Spec: batch.JobSpec{
					Selector:     validSelector,
					Template:     validTemplate,
					Parallelism:  ptr.To[int32](1),
					Completions:  ptr.To[int32](1),
					BackoffLimit: ptr.To[int32](6),
					PodFailurePolicy: &batch.PodFailurePolicy{
						Rules: onExitCodeRules,
					},
				},
				Status: batch.JobStatus{
					Conditions: []batch.JobCondition{
						{
							Type:    batch.JobFailureTarget,
							Status:  v1.ConditionTrue,
							Reason:  batch.JobReasonPodFailurePolicy,
							Message: "Container main-container for pod default/mypod-0 failed with exit code 5 matching FailJob rule at index 1",
						},
					},
				},
			},
			pods: []v1.Pod{
				{
					Status: v1.PodStatus{
						Phase: v1.PodFailed,
						ContainerStatuses: []v1.ContainerStatus{
							{
								Name: "main-container",
								State: v1.ContainerState{
									Terminated: &v1.ContainerStateTerminated{
										ExitCode: 5,
									},
								},
							},
						},
					},
				},
			},
			wantConditions: []batch.JobCondition{
				{
					Type:    batch.JobFailureTarget,
					Status:  v1.ConditionTrue,
					Reason:  batch.JobReasonPodFailurePolicy,
					Message: "Container main-container for pod default/mypod-0 failed with exit code 5 matching FailJob rule at index 1",
				},
				{
					Type:    batch.JobFailed,
					Status:  v1.ConditionTrue,
					Reason:  batch.JobReasonPodFailurePolicy,
					Message: "Container main-container for pod default/mypod-0 failed with exit code 5 matching FailJob rule at index 1",
				},
			},
			wantStatusActive:    0,
			wantStatusFailed:    1,
			wantStatusSucceeded: 0,
		},
		"job marked already as failure target with failed pod, message based on already deleted pod": {
			job: batch.Job{
				TypeMeta:   metav1.TypeMeta{Kind: "Job"},
				ObjectMeta: validObjectMeta,
				Spec: batch.JobSpec{
					Selector:     validSelector,
					Template:     validTemplate,
					Parallelism:  ptr.To[int32](1),
					Completions:  ptr.To[int32](1),
					BackoffLimit: ptr.To[int32](6),
					PodFailurePolicy: &batch.PodFailurePolicy{
						Rules: onExitCodeRules,
					},
				},
				Status: batch.JobStatus{
					Conditions: []batch.JobCondition{
						{
							Type:    batch.JobFailureTarget,
							Status:  v1.ConditionTrue,
							Reason:  batch.JobReasonPodFailurePolicy,
							Message: "Container main-container for pod default/already-deleted-pod failed with exit code 5 matching FailJob rule at index 1",
						},
					},
				},
			},
			pods: []v1.Pod{
				{
					Status: v1.PodStatus{
						Phase: v1.PodFailed,
						ContainerStatuses: []v1.ContainerStatus{
							{
								Name: "main-container",
								State: v1.ContainerState{
									Terminated: &v1.ContainerStateTerminated{
										ExitCode: 5,
									},
								},
							},
						},
					},
				},
			},
			wantConditions: []batch.JobCondition{
				{
					Type:    batch.JobFailureTarget,
					Status:  v1.ConditionTrue,
					Reason:  batch.JobReasonPodFailurePolicy,
					Message: "Container main-container for pod default/already-deleted-pod failed with exit code 5 matching FailJob rule at index 1",
				},
				{
					Type:    batch.JobFailed,
					Status:  v1.ConditionTrue,
					Reason:  batch.JobReasonPodFailurePolicy,
					Message: "Container main-container for pod default/already-deleted-pod failed with exit code 5 matching FailJob rule at index 1",
				},
			},
			wantStatusActive:    0,
			wantStatusFailed:    1,
			wantStatusSucceeded: 0,
		},
		"fail job with multiple pods": {
			job: batch.Job{
				TypeMeta:   metav1.TypeMeta{Kind: "Job"},
				ObjectMeta: validObjectMeta,
				Spec: batch.JobSpec{
					Selector:     validSelector,
					Template:     validTemplate,
					Parallelism:  ptr.To[int32](2),
					Completions:  ptr.To[int32](2),
					BackoffLimit: ptr.To[int32](6),
					PodFailurePolicy: &batch.PodFailurePolicy{
						Rules: onExitCodeRules,
					},
				},
			},
			pods: []v1.Pod{
				{
					Status: v1.PodStatus{
						Phase: v1.PodRunning,
					},
				},
				{
					Status: v1.PodStatus{
						Phase: v1.PodFailed,
						ContainerStatuses: []v1.ContainerStatus{
							{
								Name: "main-container",
								State: v1.ContainerState{
									Terminated: &v1.ContainerStateTerminated{
										ExitCode: 5,
									},
								},
							},
						},
					},
				},
			},
			wantConditions: []batch.JobCondition{
				{
					Type:    batch.JobFailureTarget,
					Status:  v1.ConditionTrue,
					Reason:  batch.JobReasonPodFailurePolicy,
					Message: "Container main-container for pod default/mypod-1 failed with exit code 5 matching FailJob rule at index 1",
				},
				{
					Type:    batch.JobFailed,
					Status:  v1.ConditionTrue,
					Reason:  batch.JobReasonPodFailurePolicy,
					Message: "Container main-container for pod default/mypod-1 failed with exit code 5 matching FailJob rule at index 1",
				},
			},
			wantStatusActive:    0,
			wantStatusFailed:    2,
			wantStatusSucceeded: 0,
		},
		"fail job with multiple pods; JobManagedBy enabled delays setting terminal condition": {
			enableJobManagedBy: true,
			job: batch.Job{
				TypeMeta:   metav1.TypeMeta{Kind: "Job"},
				ObjectMeta: validObjectMeta,
				Spec: batch.JobSpec{
					Selector:     validSelector,
					Template:     validTemplate,
					Parallelism:  ptr.To[int32](2),
					Completions:  ptr.To[int32](2),
					BackoffLimit: ptr.To[int32](6),
					PodFailurePolicy: &batch.PodFailurePolicy{
						Rules: onExitCodeRules,
					},
				},
			},
			pods: []v1.Pod{
				{
					Status: v1.PodStatus{
						Phase: v1.PodRunning,
					},
				},
				{
					Status: v1.PodStatus{
						Phase: v1.PodFailed,
						ContainerStatuses: []v1.ContainerStatus{
							{
								Name: "main-container",
								State: v1.ContainerState{
									Terminated: &v1.ContainerStateTerminated{
										ExitCode: 5,
									},
								},
							},
						},
					},
				},
			},
			wantConditions: []batch.JobCondition{
				{
					Type:    batch.JobFailureTarget,
					Status:  v1.ConditionTrue,
					Reason:  batch.JobReasonPodFailurePolicy,
					Message: "Container main-container for pod default/mypod-1 failed with exit code 5 matching FailJob rule at index 1",
				},
			},
			wantStatusActive:    0,
			wantStatusFailed:    2,
			wantStatusSucceeded: 0,
		},
		"fail indexed job based on OnExitCodes": {
			job: batch.Job{
				TypeMeta:   metav1.TypeMeta{Kind: "Job"},
				ObjectMeta: validObjectMeta,
				Spec: batch.JobSpec{
					Selector:       validSelector,
					Template:       validTemplate,
					CompletionMode: &indexedCompletionMode,
					Parallelism:    ptr.To[int32](1),
					Completions:    ptr.To[int32](1),
					BackoffLimit:   ptr.To[int32](6),
					PodFailurePolicy: &batch.PodFailurePolicy{
						Rules: onExitCodeRules,
					},
				},
			},
			pods: []v1.Pod{
				{
					Status: v1.PodStatus{
						Phase: v1.PodFailed,
						ContainerStatuses: []v1.ContainerStatus{
							{
								Name: "main-container",
								State: v1.ContainerState{
									Terminated: &v1.ContainerStateTerminated{
										ExitCode: 5,
									},
								},
							},
						},
					},
				},
			},
			wantConditions: []batch.JobCondition{
				{
					Type:    batch.JobFailureTarget,
					Status:  v1.ConditionTrue,
					Reason:  batch.JobReasonPodFailurePolicy,
					Message: "Container main-container for pod default/mypod-0 failed with exit code 5 matching FailJob rule at index 1",
				},
				{
					Type:    batch.JobFailed,
					Status:  v1.ConditionTrue,
					Reason:  batch.JobReasonPodFailurePolicy,
					Message: "Container main-container for pod default/mypod-0 failed with exit code 5 matching FailJob rule at index 1",
				},
			},
			wantStatusActive:    0,
			wantStatusFailed:    1,
			wantStatusSucceeded: 0,
		},
		"fail job based on OnExitCodes with NotIn operator": {
			job: batch.Job{
				TypeMeta:   metav1.TypeMeta{Kind: "Job"},
				ObjectMeta: validObjectMeta,
				Spec: batch.JobSpec{
					Selector:     validSelector,
					Template:     validTemplate,
					Parallelism:  ptr.To[int32](1),
					Completions:  ptr.To[int32](1),
					BackoffLimit: ptr.To[int32](6),
					PodFailurePolicy: &batch.PodFailurePolicy{
						Rules: []batch.PodFailurePolicyRule{
							{
								Action: batch.PodFailurePolicyActionFailJob,
								OnExitCodes: &batch.PodFailurePolicyOnExitCodesRequirement{
									Operator: batch.PodFailurePolicyOnExitCodesOpNotIn,
									Values:   []int32{5, 6, 7},
								},
							},
						},
					},
				},
			},
			pods: []v1.Pod{
				{
					Status: v1.PodStatus{
						Phase: v1.PodFailed,
						ContainerStatuses: []v1.ContainerStatus{
							{
								Name: "main-container",
								State: v1.ContainerState{
									Terminated: &v1.ContainerStateTerminated{
										ExitCode: 42,
									},
								},
							},
						},
					},
				},
			},
			wantConditions: []batch.JobCondition{
				{
					Type:    batch.JobFailureTarget,
					Status:  v1.ConditionTrue,
					Reason:  batch.JobReasonPodFailurePolicy,
					Message: "Container main-container for pod default/mypod-0 failed with exit code 42 matching FailJob rule at index 0",
				},
				{
					Type:    batch.JobFailed,
					Status:  v1.ConditionTrue,
					Reason:  batch.JobReasonPodFailurePolicy,
					Message: "Container main-container for pod default/mypod-0 failed with exit code 42 matching FailJob rule at index 0",
				},
			},
			wantStatusActive:    0,
			wantStatusFailed:    1,
			wantStatusSucceeded: 0,
		},
		"default handling job based on OnExitCodes with NotIn operator": {
			job: batch.Job{
				TypeMeta:   metav1.TypeMeta{Kind: "Job"},
				ObjectMeta: validObjectMeta,
				Spec: batch.JobSpec{
					Selector:     validSelector,
					Template:     validTemplate,
					Parallelism:  ptr.To[int32](1),
					Completions:  ptr.To[int32](1),
					BackoffLimit: ptr.To[int32](6),
					PodFailurePolicy: &batch.PodFailurePolicy{
						Rules: []batch.PodFailurePolicyRule{
							{
								Action: batch.PodFailurePolicyActionFailJob,
								OnExitCodes: &batch.PodFailurePolicyOnExitCodesRequirement{
									Operator: batch.PodFailurePolicyOnExitCodesOpNotIn,
									Values:   []int32{5, 6, 7},
								},
							},
						},
					},
				},
			},
			pods: []v1.Pod{
				{
					Status: v1.PodStatus{
						Phase: v1.PodFailed,
						ContainerStatuses: []v1.ContainerStatus{
							{
								Name: "main-container",
								State: v1.ContainerState{
									Terminated: &v1.ContainerStateTerminated{
										ExitCode:   5,
										FinishedAt: testFinishedAt,
									},
								},
							},
						},
					},
				},
			},
			wantConditions:      nil,
			wantStatusActive:    1,
			wantStatusFailed:    1,
			wantStatusSucceeded: 0,
		},
		"fail job based on OnExitCodes for InitContainer": {
			job: batch.Job{
				TypeMeta:   metav1.TypeMeta{Kind: "Job"},
				ObjectMeta: validObjectMeta,
				Spec: batch.JobSpec{
					Selector:     validSelector,
					Template:     validTemplate,
					Parallelism:  ptr.To[int32](1),
					Completions:  ptr.To[int32](1),
					BackoffLimit: ptr.To[int32](6),
					PodFailurePolicy: &batch.PodFailurePolicy{
						Rules: onExitCodeRules,
					},
				},
			},
			pods: []v1.Pod{
				{
					Status: v1.PodStatus{
						Phase: v1.PodFailed,
						InitContainerStatuses: []v1.ContainerStatus{
							{
								Name: "init-container",
								State: v1.ContainerState{
									Terminated: &v1.ContainerStateTerminated{
										ExitCode: 5,
									},
								},
							},
						},
						ContainerStatuses: []v1.ContainerStatus{
							{
								Name: "main-container",
								State: v1.ContainerState{
									Terminated: &v1.ContainerStateTerminated{
										ExitCode: 143,
									},
								},
							},
						},
					},
				},
			},
			wantConditions: []batch.JobCondition{
				{
					Type:    batch.JobFailureTarget,
					Status:  v1.ConditionTrue,
					Reason:  batch.JobReasonPodFailurePolicy,
					Message: "Container init-container for pod default/mypod-0 failed with exit code 5 matching FailJob rule at index 1",
				},
				{
					Type:    batch.JobFailed,
					Status:  v1.ConditionTrue,
					Reason:  batch.JobReasonPodFailurePolicy,
					Message: "Container init-container for pod default/mypod-0 failed with exit code 5 matching FailJob rule at index 1",
				},
			},
			wantStatusActive:    0,
			wantStatusFailed:    1,
			wantStatusSucceeded: 0,
		},
		"ignore pod failure; both rules are matching, the first is executed only": {
			job: batch.Job{
				TypeMeta:   metav1.TypeMeta{Kind: "Job"},
				ObjectMeta: validObjectMeta,
				Spec: batch.JobSpec{
					Selector:     validSelector,
					Template:     validTemplate,
					Parallelism:  ptr.To[int32](1),
					Completions:  ptr.To[int32](1),
					BackoffLimit: ptr.To[int32](0),
					PodFailurePolicy: &batch.PodFailurePolicy{
						Rules: onExitCodeRules,
					},
				},
			},
			pods: []v1.Pod{
				{
					Status: v1.PodStatus{
						Phase: v1.PodFailed,
						ContainerStatuses: []v1.ContainerStatus{
							{
								Name: "container1",
								State: v1.ContainerState{
									Terminated: &v1.ContainerStateTerminated{
										ExitCode: 2,
									},
								},
							},
							{
								Name: "container2",
								State: v1.ContainerState{
									Terminated: &v1.ContainerStateTerminated{
										ExitCode: 6,
									},
								},
							},
						},
					},
				},
			},
			wantConditions:      nil,
			wantStatusActive:    1,
			wantStatusFailed:    0,
			wantStatusSucceeded: 0,
		},
		"ignore pod failure based on OnExitCodes": {
			job: batch.Job{
				TypeMeta:   metav1.TypeMeta{Kind: "Job"},
				ObjectMeta: validObjectMeta,
				Spec: batch.JobSpec{
					Selector:     validSelector,
					Template:     validTemplate,
					Parallelism:  ptr.To[int32](1),
					Completions:  ptr.To[int32](1),
					BackoffLimit: ptr.To[int32](0),
					PodFailurePolicy: &batch.PodFailurePolicy{
						Rules: onExitCodeRules,
					},
				},
			},
			pods: []v1.Pod{
				{
					Status: v1.PodStatus{
						Phase: v1.PodFailed,
						ContainerStatuses: []v1.ContainerStatus{
							{
								State: v1.ContainerState{
									Terminated: &v1.ContainerStateTerminated{
										ExitCode: 1,
									},
								},
							},
						},
					},
				},
			},
			wantConditions:      nil,
			wantStatusActive:    1,
			wantStatusFailed:    0,
			wantStatusSucceeded: 0,
		},
		"default job based on OnExitCodes": {
			job: batch.Job{
				TypeMeta:   metav1.TypeMeta{Kind: "Job"},
				ObjectMeta: validObjectMeta,
				Spec: batch.JobSpec{
					Selector:     validSelector,
					Template:     validTemplate,
					Parallelism:  ptr.To[int32](1),
					Completions:  ptr.To[int32](1),
					BackoffLimit: ptr.To[int32](0),
					PodFailurePolicy: &batch.PodFailurePolicy{
						Rules: onExitCodeRules,
					},
				},
			},
			pods: []v1.Pod{
				{
					Status: v1.PodStatus{
						Phase: v1.PodFailed,
						ContainerStatuses: []v1.ContainerStatus{
							{
								State: v1.ContainerState{
									Terminated: &v1.ContainerStateTerminated{
										ExitCode: 10,
									},
								},
							},
						},
					},
				},
			},
			wantConditions: []batch.JobCondition{
				{
					Type:    batch.JobFailed,
					Status:  v1.ConditionTrue,
					Reason:  batch.JobReasonBackoffLimitExceeded,
					Message: "Job has reached the specified backoff limit",
				},
			},
			wantStatusActive:    0,
			wantStatusFailed:    1,
			wantStatusSucceeded: 0,
		},
		"default job based on OnExitCodes; JobManagedBy enabled triggers adding interim condition": {
			enableJobManagedBy: true,
			job: batch.Job{
				TypeMeta:   metav1.TypeMeta{Kind: "Job"},
				ObjectMeta: validObjectMeta,
				Spec: batch.JobSpec{
					Selector:     validSelector,
					Template:     validTemplate,
					Parallelism:  ptr.To[int32](1),
					Completions:  ptr.To[int32](1),
					BackoffLimit: ptr.To[int32](0),
					PodFailurePolicy: &batch.PodFailurePolicy{
						Rules: onExitCodeRules,
					},
				},
			},
			pods: []v1.Pod{
				{
					Status: v1.PodStatus{
						Phase: v1.PodFailed,
						ContainerStatuses: []v1.ContainerStatus{
							{
								State: v1.ContainerState{
									Terminated: &v1.ContainerStateTerminated{
										ExitCode: 10,
									},
								},
							},
						},
					},
				},
			},
			wantConditions: []batch.JobCondition{
				{
					Type:    batch.JobFailureTarget,
					Status:  v1.ConditionTrue,
					Reason:  batch.JobReasonBackoffLimitExceeded,
					Message: "Job has reached the specified backoff limit",
				},
				{
					Type:    batch.JobFailed,
					Status:  v1.ConditionTrue,
					Reason:  batch.JobReasonBackoffLimitExceeded,
					Message: "Job has reached the specified backoff limit",
				},
			},
			wantStatusActive:    0,
			wantStatusFailed:    1,
			wantStatusSucceeded: 0,
		},
		"count pod failure based on OnExitCodes; both rules are matching, the first is executed only": {
			job: batch.Job{
				TypeMeta:   metav1.TypeMeta{Kind: "Job"},
				ObjectMeta: validObjectMeta,
				Spec: batch.JobSpec{
					Selector:     validSelector,
					Template:     validTemplate,
					Parallelism:  ptr.To[int32](1),
					Completions:  ptr.To[int32](1),
					BackoffLimit: ptr.To[int32](6),
					PodFailurePolicy: &batch.PodFailurePolicy{
						Rules: []batch.PodFailurePolicyRule{
							{
								Action: batch.PodFailurePolicyActionCount,
								OnExitCodes: &batch.PodFailurePolicyOnExitCodesRequirement{
									Operator: batch.PodFailurePolicyOnExitCodesOpIn,
									Values:   []int32{1, 2},
								},
							},
							{
								Action: batch.PodFailurePolicyActionIgnore,
								OnExitCodes: &batch.PodFailurePolicyOnExitCodesRequirement{
									Operator: batch.PodFailurePolicyOnExitCodesOpIn,
									Values:   []int32{2, 3},
								},
							},
						},
					},
				},
			},
			pods: []v1.Pod{
				{
					Status: v1.PodStatus{
						Phase: v1.PodFailed,
						ContainerStatuses: []v1.ContainerStatus{
							{
								State: v1.ContainerState{
									Terminated: &v1.ContainerStateTerminated{
										ExitCode:   2,
										FinishedAt: testFinishedAt,
									},
								},
							},
						},
					},
				},
			},
			wantConditions:      nil,
			wantStatusActive:    1,
			wantStatusFailed:    1,
			wantStatusSucceeded: 0,
		},
		"count pod failure based on OnPodConditions; both rules are matching, the first is executed only": {
			job: batch.Job{
				TypeMeta:   metav1.TypeMeta{Kind: "Job"},
				ObjectMeta: validObjectMeta,
				Spec: batch.JobSpec{
					Selector:     validSelector,
					Template:     validTemplate,
					Parallelism:  ptr.To[int32](1),
					Completions:  ptr.To[int32](1),
					BackoffLimit: ptr.To[int32](6),
					PodFailurePolicy: &batch.PodFailurePolicy{
						Rules: []batch.PodFailurePolicyRule{
							{
								Action: batch.PodFailurePolicyActionCount,
								OnPodConditions: []batch.PodFailurePolicyOnPodConditionsPattern{
									{
										Type:   v1.PodConditionType("ResourceLimitExceeded"),
										Status: v1.ConditionTrue,
									},
								},
							},
							{
								Action: batch.PodFailurePolicyActionIgnore,
								OnPodConditions: []batch.PodFailurePolicyOnPodConditionsPattern{
									{
										Type:   v1.DisruptionTarget,
										Status: v1.ConditionTrue,
									},
								},
							},
						},
					},
				},
			},
			pods: []v1.Pod{
				{
					Status: v1.PodStatus{
						Phase: v1.PodFailed,
						Conditions: []v1.PodCondition{
							{
								Type:   v1.PodConditionType("ResourceLimitExceeded"),
								Status: v1.ConditionTrue,
							},
							{
								Type:   v1.DisruptionTarget,
								Status: v1.ConditionTrue,
							},
						},
						ContainerStatuses: []v1.ContainerStatus{
							{
								State: v1.ContainerState{
									Terminated: &v1.ContainerStateTerminated{
										FinishedAt: testFinishedAt,
									},
								},
							},
						},
					},
				},
			},
			wantConditions:      nil,
			wantStatusActive:    1,
			wantStatusFailed:    1,
			wantStatusSucceeded: 0,
		},
		"ignore pod failure based on OnPodConditions": {
			job: batch.Job{
				TypeMeta:   metav1.TypeMeta{Kind: "Job"},
				ObjectMeta: validObjectMeta,
				Spec: batch.JobSpec{
					Selector:     validSelector,
					Template:     validTemplate,
					Parallelism:  ptr.To[int32](1),
					Completions:  ptr.To[int32](1),
					BackoffLimit: ptr.To[int32](0),
					PodFailurePolicy: &batch.PodFailurePolicy{
						Rules: []batch.PodFailurePolicyRule{
							{
								Action: batch.PodFailurePolicyActionIgnore,
								OnPodConditions: []batch.PodFailurePolicyOnPodConditionsPattern{
									{
										Type:   v1.DisruptionTarget,
										Status: v1.ConditionTrue,
									},
								},
							},
						},
					},
				},
			},
			pods: []v1.Pod{
				{
					Status: v1.PodStatus{
						Phase: v1.PodFailed,
						Conditions: []v1.PodCondition{
							{
								Type:   v1.DisruptionTarget,
								Status: v1.ConditionTrue,
							},
						},
					},
				},
			},
			wantConditions:      nil,
			wantStatusActive:    1,
			wantStatusFailed:    0,
			wantStatusSucceeded: 0,
		},
		"ignore pod failure based on OnPodConditions, ignored failures delays pod recreation": {
			job: batch.Job{
				TypeMeta:   metav1.TypeMeta{Kind: "Job"},
				ObjectMeta: validObjectMeta,
				Spec: batch.JobSpec{
					Selector:     validSelector,
					Template:     validTemplate,
					Parallelism:  ptr.To[int32](1),
					Completions:  ptr.To[int32](1),
					BackoffLimit: ptr.To[int32](0),
					PodFailurePolicy: &batch.PodFailurePolicy{
						Rules: []batch.PodFailurePolicyRule{
							{
								Action: batch.PodFailurePolicyActionIgnore,
								OnPodConditions: []batch.PodFailurePolicyOnPodConditionsPattern{
									{
										Type:   v1.DisruptionTarget,
										Status: v1.ConditionTrue,
									},
								},
							},
						},
					},
				},
			},
			pods: []v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						DeletionTimestamp: &now,
					},
					Status: v1.PodStatus{
						Phase: v1.PodFailed,
						Conditions: []v1.PodCondition{
							{
								Type:   v1.DisruptionTarget,
								Status: v1.ConditionTrue,
							},
						},
					},
				},
			},
			wantConditions:      nil,
			wantStatusActive:    0,
			wantStatusFailed:    0,
			wantStatusSucceeded: 0,
		},
		"fail job based on OnPodConditions": {
			job: batch.Job{
				TypeMeta:   metav1.TypeMeta{Kind: "Job"},
				ObjectMeta: validObjectMeta,
				Spec: batch.JobSpec{
					Selector:     validSelector,
					Template:     validTemplate,
					Parallelism:  ptr.To[int32](1),
					Completions:  ptr.To[int32](1),
					BackoffLimit: ptr.To[int32](6),
					PodFailurePolicy: &batch.PodFailurePolicy{
						Rules: []batch.PodFailurePolicyRule{
							{
								Action: batch.PodFailurePolicyActionFailJob,
								OnPodConditions: []batch.PodFailurePolicyOnPodConditionsPattern{
									{
										Type:   v1.DisruptionTarget,
										Status: v1.ConditionTrue,
									},
								},
							},
						},
					},
				},
			},
			pods: []v1.Pod{
				{
					Status: v1.PodStatus{
						Phase: v1.PodFailed,
						Conditions: []v1.PodCondition{
							{
								Type:   v1.DisruptionTarget,
								Status: v1.ConditionTrue,
							},
						},
					},
				},
			},
			wantConditions: []batch.JobCondition{
				{
					Type:    batch.JobFailureTarget,
					Status:  v1.ConditionTrue,
					Reason:  batch.JobReasonPodFailurePolicy,
					Message: "Pod default/mypod-0 has condition DisruptionTarget matching FailJob rule at index 0",
				},
				{
					Type:    batch.JobFailed,
					Status:  v1.ConditionTrue,
					Reason:  batch.JobReasonPodFailurePolicy,
					Message: "Pod default/mypod-0 has condition DisruptionTarget matching FailJob rule at index 0",
				},
			},
			wantStatusActive:    0,
			wantStatusFailed:    1,
			wantStatusSucceeded: 0,
		},
		"terminating Pod not considered failed when JobPodFailurePolicy is enabled and used": {
			job: batch.Job{
				TypeMeta:   metav1.TypeMeta{Kind: "Job"},
				ObjectMeta: validObjectMeta,
				Spec: batch.JobSpec{
					Parallelism:  ptr.To[int32](1),
					Selector:     validSelector,
					Template:     validTemplate,
					BackoffLimit: ptr.To[int32](0),
					PodFailurePolicy: &batch.PodFailurePolicy{
						Rules: []batch.PodFailurePolicyRule{
							{
								Action: batch.PodFailurePolicyActionCount,
								OnPodConditions: []batch.PodFailurePolicyOnPodConditionsPattern{
									{
										Type:   v1.DisruptionTarget,
										Status: v1.ConditionTrue,
									},
								},
							},
						},
					},
				},
			},
			pods: []v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						DeletionTimestamp: &now,
					},
				},
			},
		},
	}
	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			featuregatetesting.SetFeatureGateDuringTest(t, feature.DefaultFeatureGate, features.JobPodReplacementPolicy, tc.enableJobPodReplacementPolicy)
			featuregatetesting.SetFeatureGateDuringTest(t, feature.DefaultFeatureGate, features.JobManagedBy, tc.enableJobManagedBy)

			if tc.job.Spec.PodReplacementPolicy == nil {
				tc.job.Spec.PodReplacementPolicy = podReplacementPolicy(batch.Failed)
			}
			clientset := clientset.NewForConfigOrDie(&restclient.Config{Host: "", ContentConfig: restclient.ContentConfig{GroupVersion: &schema.GroupVersion{Group: "", Version: "v1"}}})
			manager, sharedInformerFactory := newControllerFromClient(ctx, t, clientset, controller.NoResyncPeriodFunc)
			fakePodControl := controller.FakePodControl{}
			manager.podControl = &fakePodControl
			manager.podStoreSynced = alwaysReady
			manager.jobStoreSynced = alwaysReady
			job := &tc.job

			actual := job
			manager.updateStatusHandler = func(ctx context.Context, job *batch.Job) (*batch.Job, error) {
				actual = job
				return job, nil
			}
			sharedInformerFactory.Batch().V1().Jobs().Informer().GetIndexer().Add(job)
			for i, pod := range tc.pods {
				pod := pod
				pb := podBuilder{Pod: &pod}.name(fmt.Sprintf("mypod-%d", i)).job(job)
				if job.Spec.CompletionMode != nil && *job.Spec.CompletionMode == batch.IndexedCompletion {
					pb.index(fmt.Sprintf("%v", i))
				}
				pb = pb.trackingFinalizer()
				sharedInformerFactory.Core().V1().Pods().Informer().GetIndexer().Add(pb.Pod)
			}

			if err := manager.syncJob(ctx, testutil.GetKey(job, t)); err != nil {
				t.Fatalf("error %v while reconciling the job %v", err, testutil.GetKey(job, t))
			}

			if diff := cmp.Diff(tc.wantConditions, actual.Status.Conditions, cmpopts.IgnoreFields(batch.JobCondition{}, "LastProbeTime", "LastTransitionTime")); diff != "" {
				t.Errorf("unexpected job conditions (-want,+got):\n%s", diff)
			}
			// validate status
			if actual.Status.Active != tc.wantStatusActive {
				t.Errorf("unexpected number of active pods. Expected %d, saw %d\n", tc.wantStatusActive, actual.Status.Active)
			}
			if actual.Status.Succeeded != tc.wantStatusSucceeded {
				t.Errorf("unexpected number of succeeded pods. Expected %d, saw %d\n", tc.wantStatusSucceeded, actual.Status.Succeeded)
			}
			if actual.Status.Failed != tc.wantStatusFailed {
				t.Errorf("unexpected number of failed pods. Expected %d, saw %d\n", tc.wantStatusFailed, actual.Status.Failed)
			}
			if ptr.Deref(actual.Status.Terminating, 0) != ptr.Deref(tc.wantStatusTerminating, 0) {
				t.Errorf("unexpected number of terminating pods. Expected %d, saw %d\n", ptr.Deref(tc.wantStatusTerminating, 0), ptr.Deref(actual.Status.Terminating, 0))
			}
		})
	}
}

func TestSyncJobWithJobSuccessPolicy(t *testing.T) {
	now := time.Now()
	validTypeMeta := metav1.TypeMeta{
		APIVersion: batch.SchemeGroupVersion.String(),
		Kind:       "Job",
	}
	validObjectMeta := metav1.ObjectMeta{
		Name:      "foobar",
		UID:       uuid.NewUUID(),
		Namespace: metav1.NamespaceDefault,
	}
	validSelector := &metav1.LabelSelector{
		MatchLabels: map[string]string{"foo": "bar"},
	}
	validTemplate := v1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{
				"foo": "bar",
			},
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{Image: "foobar"},
			},
		},
	}

	testCases := map[string]struct {
		enableBackoffLimitPerIndex    bool
		enableJobSuccessPolicy        bool
		enableJobPodReplacementPolicy bool
		enableJobManagedBy            bool
		job                           batch.Job
		pods                          []v1.Pod
		wantStatus                    batch.JobStatus
	}{
		"job with successPolicy; jobPodReplacementPolicy feature enabled; job has SuccessCriteriaMet condition if job meets to successPolicy and some indexes fail": {
			enableJobSuccessPolicy:        true,
			enableJobPodReplacementPolicy: true,
			job: batch.Job{
				TypeMeta:   validTypeMeta,
				ObjectMeta: validObjectMeta,
				Spec: batch.JobSpec{
					Selector:       validSelector,
					Template:       validTemplate,
					CompletionMode: ptr.To(batch.IndexedCompletion),
					Parallelism:    ptr.To[int32](3),
					Completions:    ptr.To[int32](3),
					BackoffLimit:   ptr.To[int32](math.MaxInt32),
					SuccessPolicy: &batch.SuccessPolicy{
						Rules: []batch.SuccessPolicyRule{{
							SucceededIndexes: ptr.To("0,1"),
							SucceededCount:   ptr.To[int32](1),
						}},
					},
				},
			},
			pods: []v1.Pod{
				*buildPod().uid("a1").index("0").phase(v1.PodFailed).trackingFinalizer().Pod,
				*buildPod().uid("a2").index("0").phase(v1.PodRunning).trackingFinalizer().Pod,
				*buildPod().uid("b").index("1").phase(v1.PodSucceeded).trackingFinalizer().Pod,
				*buildPod().uid("c").index("2").phase(v1.PodRunning).trackingFinalizer().Pod,
			},
			wantStatus: batch.JobStatus{
				Failed:                  1,
				Succeeded:               1,
				Terminating:             ptr.To[int32](2),
				CompletedIndexes:        "1",
				UncountedTerminatedPods: &batch.UncountedTerminatedPods{},
				Conditions: []batch.JobCondition{
					{
						Type:    batch.JobSuccessCriteriaMet,
						Status:  v1.ConditionTrue,
						Reason:  batch.JobReasonSuccessPolicy,
						Message: "Matched rules at index 0",
					},
				},
			},
		},
		"job with successPolicy; jobPodReplacementPolicy feature disabled; job has SuccessCriteriaMet condition if job meets to successPolicy and some indexes fail": {
			enableJobSuccessPolicy:        true,
			enableJobPodReplacementPolicy: false,
			job: batch.Job{
				TypeMeta:   validTypeMeta,
				ObjectMeta: validObjectMeta,
				Spec: batch.JobSpec{
					Selector:       validSelector,
					Template:       validTemplate,
					CompletionMode: ptr.To(batch.IndexedCompletion),
					Parallelism:    ptr.To[int32](3),
					Completions:    ptr.To[int32](3),
					BackoffLimit:   ptr.To[int32](math.MaxInt32),
					SuccessPolicy: &batch.SuccessPolicy{
						Rules: []batch.SuccessPolicyRule{{
							SucceededIndexes: ptr.To("0,1"),
							SucceededCount:   ptr.To[int32](1),
						}},
					},
				},
			},
			pods: []v1.Pod{
				*buildPod().uid("a1").index("0").phase(v1.PodFailed).trackingFinalizer().Pod,
				*buildPod().uid("a2").index("0").phase(v1.PodRunning).trackingFinalizer().Pod,
				*buildPod().uid("b").index("1").phase(v1.PodSucceeded).trackingFinalizer().Pod,
				*buildPod().uid("c").index("2").phase(v1.PodRunning).trackingFinalizer().Pod,
			},
			wantStatus: batch.JobStatus{
				Failed:                  1,
				Succeeded:               1,
				CompletedIndexes:        "1",
				UncountedTerminatedPods: &batch.UncountedTerminatedPods{},
				Conditions: []batch.JobCondition{
					{
						Type:    batch.JobSuccessCriteriaMet,
						Status:  v1.ConditionTrue,
						Reason:  batch.JobReasonSuccessPolicy,
						Message: "Matched rules at index 0",
					},
					{
						Type:    batch.JobComplete,
						Status:  v1.ConditionTrue,
						Reason:  batch.JobReasonSuccessPolicy,
						Message: "Matched rules at index 0",
					},
				},
			},
		},
		"job with podFailurePolicy and successPolicy; jobPodReplacementPolicy feature enabled; job has SuccessCriteriaMet condition if job meets to successPolicy and doesn't meet to podFailurePolicy": {
			enableJobSuccessPolicy:        true,
			enableJobPodReplacementPolicy: true,
			job: batch.Job{
				TypeMeta:   validTypeMeta,
				ObjectMeta: validObjectMeta,
				Spec: batch.JobSpec{
					Selector:       validSelector,
					Template:       validTemplate,
					CompletionMode: ptr.To(batch.IndexedCompletion),
					Parallelism:    ptr.To[int32](2),
					Completions:    ptr.To[int32](2),
					BackoffLimit:   ptr.To[int32](math.MaxInt32),
					SuccessPolicy: &batch.SuccessPolicy{
						Rules: []batch.SuccessPolicyRule{{
							SucceededIndexes: ptr.To("0,1"),
							SucceededCount:   ptr.To[int32](1),
						}},
					},
					PodFailurePolicy: &batch.PodFailurePolicy{
						Rules: []batch.PodFailurePolicyRule{{
							Action: batch.PodFailurePolicyActionFailJob,
							OnPodConditions: []batch.PodFailurePolicyOnPodConditionsPattern{{
								Type:   v1.DisruptionTarget,
								Status: v1.ConditionTrue,
							}},
						}},
					},
				},
			},
			pods: []v1.Pod{
				*buildPod().uid("a1").index("0").phase(v1.PodFailed).trackingFinalizer().Pod,
				*buildPod().uid("a2").index("0").phase(v1.PodRunning).trackingFinalizer().Pod,
				*buildPod().uid("b").index("1").phase(v1.PodSucceeded).trackingFinalizer().Pod,
			},
			wantStatus: batch.JobStatus{
				Failed:                  1,
				Succeeded:               1,
				Terminating:             ptr.To[int32](1),
				CompletedIndexes:        "1",
				UncountedTerminatedPods: &batch.UncountedTerminatedPods{},
				Conditions: []batch.JobCondition{
					{
						Type:    batch.JobSuccessCriteriaMet,
						Status:  v1.ConditionTrue,
						Reason:  batch.JobReasonSuccessPolicy,
						Message: "Matched rules at index 0",
					},
				},
			},
		},
		"job with podFailurePolicy and successPolicy; jobPodReplacementPolicy feature disabled; job has SuccessCriteriaMet condition if job meets to successPolicy and doesn't meet to podFailurePolicy": {
			enableJobSuccessPolicy:        true,
			enableJobPodReplacementPolicy: false,
			job: batch.Job{
				TypeMeta:   validTypeMeta,
				ObjectMeta: validObjectMeta,
				Spec: batch.JobSpec{
					Selector:       validSelector,
					Template:       validTemplate,
					CompletionMode: ptr.To(batch.IndexedCompletion),
					Parallelism:    ptr.To[int32](2),
					Completions:    ptr.To[int32](2),
					BackoffLimit:   ptr.To[int32](math.MaxInt32),
					SuccessPolicy: &batch.SuccessPolicy{
						Rules: []batch.SuccessPolicyRule{{
							SucceededIndexes: ptr.To("0,1"),
							SucceededCount:   ptr.To[int32](1),
						}},
					},
					PodFailurePolicy: &batch.PodFailurePolicy{
						Rules: []batch.PodFailurePolicyRule{{
							Action: batch.PodFailurePolicyActionFailJob,
							OnPodConditions: []batch.PodFailurePolicyOnPodConditionsPattern{{
								Type:   v1.DisruptionTarget,
								Status: v1.ConditionTrue,
							}},
						}},
					},
				},
			},
			pods: []v1.Pod{
				*buildPod().uid("a1").index("0").phase(v1.PodFailed).trackingFinalizer().Pod,
				*buildPod().uid("a2").index("0").phase(v1.PodRunning).trackingFinalizer().Pod,
				*buildPod().uid("b").index("1").phase(v1.PodSucceeded).trackingFinalizer().Pod,
			},
			wantStatus: batch.JobStatus{
				Failed:                  1,
				Succeeded:               1,
				CompletedIndexes:        "1",
				UncountedTerminatedPods: &batch.UncountedTerminatedPods{},
				Conditions: []batch.JobCondition{
					{
						Type:    batch.JobSuccessCriteriaMet,
						Status:  v1.ConditionTrue,
						Reason:  batch.JobReasonSuccessPolicy,
						Message: "Matched rules at index 0",
					},
					{
						Type:    batch.JobComplete,
						Status:  v1.ConditionTrue,
						Reason:  batch.JobReasonSuccessPolicy,
						Message: "Matched rules at index 0",
					},
				},
			},
		},
		"job with backoffLimitPerIndex and successPolicy; jobPodReplacementPolicy feature enabled; job has SuccessCriteriaMet condition if job meets to successPolicy and doesn't meet backoffLimitPerIndex": {
			enableJobSuccessPolicy:        true,
			enableBackoffLimitPerIndex:    true,
			enableJobPodReplacementPolicy: true,
			job: batch.Job{
				TypeMeta:   validTypeMeta,
				ObjectMeta: validObjectMeta,
				Spec: batch.JobSpec{
					Selector:             validSelector,
					Template:             validTemplate,
					CompletionMode:       ptr.To(batch.IndexedCompletion),
					Parallelism:          ptr.To[int32](2),
					Completions:          ptr.To[int32](2),
					BackoffLimit:         ptr.To[int32](math.MaxInt32),
					BackoffLimitPerIndex: ptr.To[int32](2),
					SuccessPolicy: &batch.SuccessPolicy{
						Rules: []batch.SuccessPolicyRule{{
							SucceededIndexes: ptr.To("0,1"),
							SucceededCount:   ptr.To[int32](1),
						}},
					},
				},
			},
			pods: []v1.Pod{
				*buildPod().uid("a1").index("0").phase(v1.PodFailed).trackingFinalizer().Pod,
				*buildPod().uid("a2").index("0").phase(v1.PodRunning).trackingFinalizer().Pod,
				*buildPod().uid("b").index("1").phase(v1.PodSucceeded).trackingFinalizer().Pod,
			},
			wantStatus: batch.JobStatus{
				Failed:                  1,
				Succeeded:               1,
				Terminating:             ptr.To[int32](1),
				CompletedIndexes:        "1",
				FailedIndexes:           ptr.To(""),
				UncountedTerminatedPods: &batch.UncountedTerminatedPods{},
				Conditions: []batch.JobCondition{
					{
						Type:    batch.JobSuccessCriteriaMet,
						Status:  v1.ConditionTrue,
						Reason:  batch.JobReasonSuccessPolicy,
						Message: "Matched rules at index 0",
					},
				},
			},
		},
		"job with successPolicy; jobPodReplacementPolicy feature enabled; job has both Complete and SuccessCriteriaMet condition when job meets to successPolicy and all pods have been already removed": {
			enableJobSuccessPolicy:        true,
			enableJobPodReplacementPolicy: true,
			job: batch.Job{
				TypeMeta:   validTypeMeta,
				ObjectMeta: validObjectMeta,
				Spec: batch.JobSpec{
					Selector:             validSelector,
					Template:             validTemplate,
					CompletionMode:       ptr.To(batch.IndexedCompletion),
					Parallelism:          ptr.To[int32](2),
					Completions:          ptr.To[int32](2),
					BackoffLimit:         ptr.To[int32](math.MaxInt32),
					BackoffLimitPerIndex: ptr.To[int32](2),
					SuccessPolicy: &batch.SuccessPolicy{
						Rules: []batch.SuccessPolicyRule{{
							SucceededIndexes: ptr.To("0,1"),
							SucceededCount:   ptr.To[int32](1),
						}},
					},
				},
				Status: batch.JobStatus{
					Conditions: []batch.JobCondition{
						{
							Type:    batch.JobSuccessCriteriaMet,
							Status:  v1.ConditionTrue,
							Reason:  batch.JobReasonSuccessPolicy,
							Message: "Matched rules at index 0",
						},
					},
				},
			},
			pods: []v1.Pod{
				*buildPod().uid("a").index("0").phase(v1.PodFailed).trackingFinalizer().Pod,
				*buildPod().uid("b").index("1").phase(v1.PodSucceeded).trackingFinalizer().Pod,
			},
			wantStatus: batch.JobStatus{
				Failed:                  1,
				Succeeded:               1,
				Terminating:             ptr.To[int32](0),
				CompletedIndexes:        "1",
				UncountedTerminatedPods: &batch.UncountedTerminatedPods{},
				Conditions: []batch.JobCondition{
					{
						Type:    batch.JobSuccessCriteriaMet,
						Status:  v1.ConditionTrue,
						Reason:  batch.JobReasonSuccessPolicy,
						Message: "Matched rules at index 0",
					},
					{
						Type:    batch.JobComplete,
						Status:  v1.ConditionTrue,
						Reason:  batch.JobReasonSuccessPolicy,
						Message: "Matched rules at index 0",
					},
				},
			},
		},
		// REF: https://github.com/kubernetes/kubernetes/issues/123775
		"job with successPolicy; jobPodReplacementPolicy feature enabled; job has SuccessCriteriaMet condition when job meets to successPolicy and some pods still are running": {
			enableJobSuccessPolicy:        true,
			enableJobPodReplacementPolicy: true,
			job: batch.Job{
				TypeMeta:   validTypeMeta,
				ObjectMeta: validObjectMeta,
				Spec: batch.JobSpec{
					Selector:             validSelector,
					Template:             validTemplate,
					CompletionMode:       ptr.To(batch.IndexedCompletion),
					Parallelism:          ptr.To[int32](3),
					Completions:          ptr.To[int32](3),
					BackoffLimit:         ptr.To[int32](math.MaxInt32),
					BackoffLimitPerIndex: ptr.To[int32](3),
					SuccessPolicy: &batch.SuccessPolicy{
						Rules: []batch.SuccessPolicyRule{{
							SucceededIndexes: ptr.To("0,1"),
							SucceededCount:   ptr.To[int32](1),
						}},
					},
				},
				Status: batch.JobStatus{
					Conditions: []batch.JobCondition{
						{
							Type:    batch.JobSuccessCriteriaMet,
							Status:  v1.ConditionTrue,
							Reason:  batch.JobReasonSuccessPolicy,
							Message: "Matched rules at index 0",
						},
					},
				},
			},
			pods: []v1.Pod{
				*buildPod().uid("a1").index("0").phase(v1.PodFailed).trackingFinalizer().Pod,
				*buildPod().uid("a2").index("1").phase(v1.PodRunning).trackingFinalizer().Pod,
				*buildPod().uid("b").index("1").phase(v1.PodSucceeded).trackingFinalizer().Pod,
				*buildPod().uid("c").index("2").phase(v1.PodRunning).trackingFinalizer().Pod,
			},
			wantStatus: batch.JobStatus{
				Failed:                  1,
				Succeeded:               1,
				Terminating:             ptr.To[int32](2),
				CompletedIndexes:        "1",
				UncountedTerminatedPods: &batch.UncountedTerminatedPods{},
				Conditions: []batch.JobCondition{
					{
						Type:    batch.JobSuccessCriteriaMet,
						Status:  v1.ConditionTrue,
						Reason:  batch.JobReasonSuccessPolicy,
						Message: "Matched rules at index 0",
					},
				},
			},
		},
		// REF: https://github.com/kubernetes/kubernetes/issues/123775
		"job with successPolicy; JobManagedBy feature enabled; job has SuccessCriteriaMet condition when job meets to successPolicy and some pods still are running": {
			enableJobSuccessPolicy:        true,
			enableJobPodReplacementPolicy: false,
			enableJobManagedBy:            true,
			job: batch.Job{
				TypeMeta:   validTypeMeta,
				ObjectMeta: validObjectMeta,
				Spec: batch.JobSpec{
					Selector:             validSelector,
					Template:             validTemplate,
					CompletionMode:       ptr.To(batch.IndexedCompletion),
					Parallelism:          ptr.To[int32](3),
					Completions:          ptr.To[int32](3),
					BackoffLimit:         ptr.To[int32](math.MaxInt32),
					BackoffLimitPerIndex: ptr.To[int32](3),
					SuccessPolicy: &batch.SuccessPolicy{
						Rules: []batch.SuccessPolicyRule{{
							SucceededIndexes: ptr.To("0,1"),
							SucceededCount:   ptr.To[int32](1),
						}},
					},
				},
				Status: batch.JobStatus{
					Conditions: []batch.JobCondition{
						{
							Type:    batch.JobSuccessCriteriaMet,
							Status:  v1.ConditionTrue,
							Reason:  batch.JobReasonSuccessPolicy,
							Message: "Matched rules at index 0",
						},
					},
				},
			},
			pods: []v1.Pod{
				*buildPod().uid("a1").index("0").phase(v1.PodFailed).trackingFinalizer().Pod,
				*buildPod().uid("a2").index("1").phase(v1.PodRunning).trackingFinalizer().Pod,
				*buildPod().uid("b").index("1").phase(v1.PodSucceeded).trackingFinalizer().Pod,
				*buildPod().uid("c").index("2").phase(v1.PodRunning).trackingFinalizer().Pod,
			},
			wantStatus: batch.JobStatus{
				Failed:                  1,
				Succeeded:               1,
				Terminating:             nil,
				CompletedIndexes:        "1",
				UncountedTerminatedPods: &batch.UncountedTerminatedPods{},
				Conditions: []batch.JobCondition{
					{
						Type:    batch.JobSuccessCriteriaMet,
						Status:  v1.ConditionTrue,
						Reason:  batch.JobReasonSuccessPolicy,
						Message: "Matched rules at index 0",
					},
				},
			},
		},
		"job with successPolicy and podFailurePolicy; jobPodReplacementPolicy feature enabled; job has a failed condition when job meets to both successPolicy and podFailurePolicy": {
			enableJobSuccessPolicy:        true,
			enableJobPodReplacementPolicy: true,
			job: batch.Job{
				TypeMeta:   validTypeMeta,
				ObjectMeta: validObjectMeta,
				Spec: batch.JobSpec{
					Selector:       validSelector,
					Template:       validTemplate,
					CompletionMode: ptr.To(batch.IndexedCompletion),
					Parallelism:    ptr.To[int32](2),
					Completions:    ptr.To[int32](2),
					BackoffLimit:   ptr.To[int32](math.MaxInt32),
					SuccessPolicy: &batch.SuccessPolicy{
						Rules: []batch.SuccessPolicyRule{{
							SucceededIndexes: ptr.To("0,1"),
							SucceededCount:   ptr.To[int32](1),
						}},
					},
					PodFailurePolicy: &batch.PodFailurePolicy{
						Rules: []batch.PodFailurePolicyRule{{
							Action: batch.PodFailurePolicyActionFailJob,
							OnPodConditions: []batch.PodFailurePolicyOnPodConditionsPattern{{
								Type:   v1.DisruptionTarget,
								Status: v1.ConditionTrue,
							}},
						}},
					},
				},
			},
			pods: []v1.Pod{
				*buildPod().uid("a1").index("0").status(v1.PodStatus{
					Phase: v1.PodFailed,
					Conditions: []v1.PodCondition{{
						Type:   v1.DisruptionTarget,
						Status: v1.ConditionTrue,
					}},
				}).trackingFinalizer().Pod,
				*buildPod().uid("a2").index("0").phase(v1.PodRunning).trackingFinalizer().Pod,
				*buildPod().uid("b").index("1").phase(v1.PodSucceeded).trackingFinalizer().Pod,
			},
			wantStatus: batch.JobStatus{
				Failed:                  2,
				Succeeded:               1,
				Terminating:             ptr.To[int32](1),
				CompletedIndexes:        "1",
				UncountedTerminatedPods: &batch.UncountedTerminatedPods{},
				Conditions: []batch.JobCondition{
					{
						Type:    batch.JobFailureTarget,
						Status:  v1.ConditionTrue,
						Reason:  batch.JobReasonPodFailurePolicy,
						Message: "Pod default/mypod-0 has condition DisruptionTarget matching FailJob rule at index 0",
					},
				},
			},
		},
		"job with successPolicy and backoffLimitPerIndex; jobPodReplacementPolicy feature enabled; job has a failed condition when job meets to both successPolicy and backoffLimitPerIndex": {
			enableJobSuccessPolicy:        true,
			enableJobPodReplacementPolicy: true,
			enableBackoffLimitPerIndex:    true,
			job: batch.Job{
				TypeMeta:   validTypeMeta,
				ObjectMeta: validObjectMeta,
				Spec: batch.JobSpec{
					Selector:             validSelector,
					Template:             validTemplate,
					CompletionMode:       ptr.To(batch.IndexedCompletion),
					Parallelism:          ptr.To[int32](2),
					Completions:          ptr.To[int32](2),
					BackoffLimit:         ptr.To[int32](math.MaxInt32),
					BackoffLimitPerIndex: ptr.To[int32](1),
					SuccessPolicy: &batch.SuccessPolicy{
						Rules: []batch.SuccessPolicyRule{{
							SucceededIndexes: ptr.To("0,1"),
							SucceededCount:   ptr.To[int32](1),
						}},
					},
				},
			},
			pods: []v1.Pod{
				*buildPod().uid("a").index("0").phase(v1.PodFailed).indexFailureCount("1").trackingFinalizer().Pod,
				*buildPod().uid("b").index("1").phase(v1.PodSucceeded).trackingFinalizer().Pod,
			},
			wantStatus: batch.JobStatus{
				Failed:                  1,
				Succeeded:               1,
				Terminating:             ptr.To[int32](0),
				CompletedIndexes:        "1",
				FailedIndexes:           ptr.To("0"),
				UncountedTerminatedPods: &batch.UncountedTerminatedPods{},
				Conditions: []batch.JobCondition{
					{
						Type:    batch.JobFailureTarget,
						Status:  v1.ConditionTrue,
						Reason:  batch.JobReasonFailedIndexes,
						Message: "Job has failed indexes",
					},
					{
						Type:    batch.JobFailed,
						Status:  v1.ConditionTrue,
						Reason:  batch.JobReasonFailedIndexes,
						Message: "Job has failed indexes",
					},
				},
			},
		},
		"job with successPolicy and backoffLimit; jobPodReplacementPolicy feature enabled; job has a failed condition when job meets to both successPolicy and backoffLimit": {
			enableJobSuccessPolicy:        true,
			enableJobPodReplacementPolicy: true,
			job: batch.Job{
				TypeMeta:   validTypeMeta,
				ObjectMeta: validObjectMeta,
				Spec: batch.JobSpec{
					Selector:       validSelector,
					Template:       validTemplate,
					CompletionMode: ptr.To(batch.IndexedCompletion),
					Parallelism:    ptr.To[int32](2),
					Completions:    ptr.To[int32](2),
					BackoffLimit:   ptr.To[int32](1),
					SuccessPolicy: &batch.SuccessPolicy{
						Rules: []batch.SuccessPolicyRule{{
							SucceededIndexes: ptr.To("0,1"),
							SucceededCount:   ptr.To[int32](1),
						}},
					},
				},
			},
			pods: []v1.Pod{
				*buildPod().uid("a1").index("0").phase(v1.PodFailed).trackingFinalizer().Pod,
				*buildPod().uid("a2").index("0").phase(v1.PodFailed).trackingFinalizer().Pod,
				*buildPod().uid("b").index("1").phase(v1.PodSucceeded).trackingFinalizer().Pod,
			},
			wantStatus: batch.JobStatus{
				Failed:                  2,
				Succeeded:               1,
				Terminating:             ptr.To[int32](0),
				CompletedIndexes:        "1",
				UncountedTerminatedPods: &batch.UncountedTerminatedPods{},
				Conditions: []batch.JobCondition{
					{
						Type:    batch.JobFailureTarget,
						Status:  v1.ConditionTrue,
						Reason:  batch.JobReasonBackoffLimitExceeded,
						Message: "Job has reached the specified backoff limit",
					},
					{
						Type:    batch.JobFailed,
						Status:  v1.ConditionTrue,
						Reason:  batch.JobReasonBackoffLimitExceeded,
						Message: "Job has reached the specified backoff limit",
					},
				},
			},
		},
		"job with successPolicy and podFailurePolicy; jobPodReplacementPolicy feature enabled; job with SuccessCriteriaMet has never been transitioned to FailureTarget and Failed even if job meets podFailurePolicy": {
			enableJobSuccessPolicy:        true,
			enableJobPodReplacementPolicy: true,
			job: batch.Job{
				TypeMeta:   validTypeMeta,
				ObjectMeta: validObjectMeta,
				Spec: batch.JobSpec{
					Selector:       validSelector,
					Template:       validTemplate,
					CompletionMode: ptr.To(batch.IndexedCompletion),
					Parallelism:    ptr.To[int32](2),
					Completions:    ptr.To[int32](2),
					BackoffLimit:   ptr.To[int32](math.MaxInt32),
					SuccessPolicy: &batch.SuccessPolicy{
						Rules: []batch.SuccessPolicyRule{{
							SucceededCount: ptr.To[int32](1),
						}},
					},
					PodFailurePolicy: &batch.PodFailurePolicy{
						Rules: []batch.PodFailurePolicyRule{{
							Action: batch.PodFailurePolicyActionFailJob,
							OnPodConditions: []batch.PodFailurePolicyOnPodConditionsPattern{{
								Type:   v1.DisruptionTarget,
								Status: v1.ConditionTrue,
							}},
						}},
					},
				},
				Status: batch.JobStatus{
					Failed:                  0,
					Succeeded:               1,
					Terminating:             ptr.To[int32](0),
					CompletedIndexes:        "1",
					UncountedTerminatedPods: &batch.UncountedTerminatedPods{},
					Conditions: []batch.JobCondition{
						{
							Type:    batch.JobSuccessCriteriaMet,
							Status:  v1.ConditionTrue,
							Reason:  batch.JobReasonSuccessPolicy,
							Message: "Matched rules at index 0",
						},
					},
				},
			},
			pods: []v1.Pod{
				*buildPod().uid("a").index("0").status(v1.PodStatus{
					Phase: v1.PodFailed,
					Conditions: []v1.PodCondition{{
						Type:   v1.DisruptionTarget,
						Status: v1.ConditionTrue,
					}},
				}).trackingFinalizer().Pod,
				*buildPod().uid("b").index("1").phase(v1.PodSucceeded).trackingFinalizer().Pod,
			},
			wantStatus: batch.JobStatus{
				Failed:                  1,
				Succeeded:               1,
				Terminating:             ptr.To[int32](0),
				CompletedIndexes:        "1",
				UncountedTerminatedPods: &batch.UncountedTerminatedPods{},
				Conditions: []batch.JobCondition{
					{
						Type:    batch.JobSuccessCriteriaMet,
						Status:  v1.ConditionTrue,
						Reason:  batch.JobReasonSuccessPolicy,
						Message: "Matched rules at index 0",
					},
					{
						Type:    batch.JobComplete,
						Status:  v1.ConditionTrue,
						Reason:  batch.JobReasonSuccessPolicy,
						Message: "Matched rules at index 0",
					},
				},
			},
		},
		"job with successPolicy and backoffLimitPerIndex; jobPodReplacementPolicy feature enabled; job with SuccessCriteriaMet has never been transitioned to FailureTarget and Failed even if job meet backoffLimitPerIndex": {
			enableJobSuccessPolicy:        true,
			enableJobPodReplacementPolicy: true,
			enableBackoffLimitPerIndex:    true,
			job: batch.Job{
				TypeMeta:   validTypeMeta,
				ObjectMeta: validObjectMeta,
				Spec: batch.JobSpec{
					Selector:             validSelector,
					Template:             validTemplate,
					CompletionMode:       ptr.To(batch.IndexedCompletion),
					Parallelism:          ptr.To[int32](2),
					Completions:          ptr.To[int32](2),
					BackoffLimit:         ptr.To[int32](math.MaxInt32),
					BackoffLimitPerIndex: ptr.To[int32](1),
					SuccessPolicy: &batch.SuccessPolicy{
						Rules: []batch.SuccessPolicyRule{{
							SucceededIndexes: ptr.To("1"),
						}},
					},
				},
				Status: batch.JobStatus{
					Failed:                  0,
					Succeeded:               1,
					Terminating:             ptr.To[int32](0),
					CompletedIndexes:        "1",
					UncountedTerminatedPods: &batch.UncountedTerminatedPods{},
					Conditions: []batch.JobCondition{
						{
							Type:    batch.JobSuccessCriteriaMet,
							Status:  v1.ConditionTrue,
							Reason:  batch.JobReasonSuccessPolicy,
							Message: "Matched rules at index 0",
						},
					},
				},
			},
			pods: []v1.Pod{
				*buildPod().uid("a").index("0").phase(v1.PodFailed).indexFailureCount("1").trackingFinalizer().Pod,
				*buildPod().uid("b").index("1").phase(v1.PodSucceeded).trackingFinalizer().Pod,
			},
			wantStatus: batch.JobStatus{
				Failed:                  1,
				Succeeded:               1,
				Terminating:             ptr.To[int32](0),
				CompletedIndexes:        "1",
				FailedIndexes:           ptr.To("0"),
				UncountedTerminatedPods: &batch.UncountedTerminatedPods{},
				Conditions: []batch.JobCondition{
					{
						Type:    batch.JobSuccessCriteriaMet,
						Status:  v1.ConditionTrue,
						Reason:  batch.JobReasonSuccessPolicy,
						Message: "Matched rules at index 0",
					},
					{
						Type:    batch.JobComplete,
						Status:  v1.ConditionTrue,
						Reason:  batch.JobReasonSuccessPolicy,
						Message: "Matched rules at index 0",
					},
				},
			},
		},
		"job with successPolicy and backoffLimit; jobPodReplacementPolicy feature enabled; job with SuccessCriteriaMet has never been transitioned to FailureTarget and Failed even if job meets backoffLimit": {
			enableJobSuccessPolicy:        true,
			enableJobPodReplacementPolicy: true,
			job: batch.Job{
				TypeMeta:   validTypeMeta,
				ObjectMeta: validObjectMeta,
				Spec: batch.JobSpec{
					Selector:       validSelector,
					Template:       validTemplate,
					CompletionMode: ptr.To(batch.IndexedCompletion),
					Parallelism:    ptr.To[int32](2),
					Completions:    ptr.To[int32](2),
					BackoffLimit:   ptr.To[int32](1),
					SuccessPolicy: &batch.SuccessPolicy{
						Rules: []batch.SuccessPolicyRule{{
							SucceededIndexes: ptr.To("0,1"),
							SucceededCount:   ptr.To[int32](1),
						}},
					},
				},
				Status: batch.JobStatus{
					Failed:                  0,
					Succeeded:               1,
					Terminating:             ptr.To[int32](0),
					CompletedIndexes:        "1",
					UncountedTerminatedPods: &batch.UncountedTerminatedPods{},
					Conditions: []batch.JobCondition{
						{
							Type:    batch.JobSuccessCriteriaMet,
							Status:  v1.ConditionTrue,
							Reason:  batch.JobReasonSuccessPolicy,
							Message: "Matched rules at index 0",
						},
					},
				},
			},
			pods: []v1.Pod{
				*buildPod().uid("a").index("0").phase(v1.PodFailed).trackingFinalizer().Pod,
				*buildPod().uid("b").index("1").phase(v1.PodSucceeded).trackingFinalizer().Pod,
			},
			wantStatus: batch.JobStatus{
				Failed:                  1,
				Succeeded:               1,
				Terminating:             ptr.To[int32](0),
				CompletedIndexes:        "1",
				UncountedTerminatedPods: &batch.UncountedTerminatedPods{},
				Conditions: []batch.JobCondition{
					{
						Type:    batch.JobSuccessCriteriaMet,
						Status:  v1.ConditionTrue,
						Reason:  batch.JobReasonSuccessPolicy,
						Message: "Matched rules at index 0",
					},
					{
						Type:    batch.JobComplete,
						Status:  v1.ConditionTrue,
						Reason:  batch.JobReasonSuccessPolicy,
						Message: "Matched rules at index 0",
					},
				},
			},
		},
		"job with successPolicy and podFailureTarget; jobPodReplacementPolicy feature enabled; job with FailureTarget has never been transitioned to SuccessCriteriaMet even if job meets successPolicy": {
			enableJobSuccessPolicy:        true,
			enableJobPodReplacementPolicy: true,
			job: batch.Job{
				TypeMeta:   validTypeMeta,
				ObjectMeta: validObjectMeta,
				Spec: batch.JobSpec{
					Selector:       validSelector,
					Template:       validTemplate,
					CompletionMode: ptr.To(batch.IndexedCompletion),
					Parallelism:    ptr.To[int32](2),
					Completions:    ptr.To[int32](2),
					BackoffLimit:   ptr.To[int32](math.MaxInt32),
					SuccessPolicy: &batch.SuccessPolicy{
						Rules: []batch.SuccessPolicyRule{{
							SucceededIndexes: ptr.To("0,1"),
							SucceededCount:   ptr.To[int32](1),
						}},
					},
					PodFailurePolicy: &batch.PodFailurePolicy{
						Rules: []batch.PodFailurePolicyRule{{
							Action: batch.PodFailurePolicyActionFailJob,
							OnPodConditions: []batch.PodFailurePolicyOnPodConditionsPattern{{
								Type:   v1.DisruptionTarget,
								Status: v1.ConditionTrue,
							}},
						}},
					},
				},
				Status: batch.JobStatus{
					Failed:                  1,
					Succeeded:               0,
					Terminating:             ptr.To[int32](0),
					CompletedIndexes:        "1",
					UncountedTerminatedPods: &batch.UncountedTerminatedPods{},
					Conditions: []batch.JobCondition{
						{
							Type:    batch.JobFailureTarget,
							Status:  v1.ConditionTrue,
							Reason:  batch.JobReasonPodFailurePolicy,
							Message: "Pod default/mypod-0 has condition DisruptionTarget matching FailJob rule at index 0",
						},
					},
				},
			},
			pods: []v1.Pod{
				*buildPod().uid("a").index("0").status(v1.PodStatus{
					Phase: v1.PodFailed,
					Conditions: []v1.PodCondition{{
						Type:   v1.DisruptionTarget,
						Status: v1.ConditionTrue,
					}},
				}).Pod,
				*buildPod().uid("b").index("1").phase(v1.PodSucceeded).trackingFinalizer().Pod,
			},
			wantStatus: batch.JobStatus{
				Failed:                  1,
				Succeeded:               1,
				Terminating:             ptr.To[int32](0),
				CompletedIndexes:        "1",
				UncountedTerminatedPods: &batch.UncountedTerminatedPods{},
				Conditions: []batch.JobCondition{
					{
						Type:    batch.JobFailureTarget,
						Status:  v1.ConditionTrue,
						Reason:  batch.JobReasonPodFailurePolicy,
						Message: "Pod default/mypod-0 has condition DisruptionTarget matching FailJob rule at index 0",
					},
					{
						Type:    batch.JobFailed,
						Status:  v1.ConditionTrue,
						Reason:  batch.JobReasonPodFailurePolicy,
						Message: "Pod default/mypod-0 has condition DisruptionTarget matching FailJob rule at index 0",
					},
				},
			},
		},
		"job without successPolicy; jobSuccessPolicy is enabled; job got SuccessCriteriaMet and Completion with CompletionsReached reason conditions": {
			enableJobSuccessPolicy: true,
			enableJobManagedBy:     true,
			job: batch.Job{
				TypeMeta:   validTypeMeta,
				ObjectMeta: validObjectMeta,
				Spec: batch.JobSpec{
					Selector:       validSelector,
					Template:       validTemplate,
					CompletionMode: ptr.To(batch.IndexedCompletion),
					Completions:    ptr.To[int32](1),
					Parallelism:    ptr.To[int32](1),
					BackoffLimit:   ptr.To[int32](math.MaxInt32),
				},
			},
			pods: []v1.Pod{
				*buildPod().uid("a1").index("0").phase(v1.PodSucceeded).trackingFinalizer().Pod,
			},
			wantStatus: batch.JobStatus{
				Failed:                  0,
				Succeeded:               1,
				CompletedIndexes:        "0",
				UncountedTerminatedPods: &batch.UncountedTerminatedPods{},
				Conditions: []batch.JobCondition{
					{
						Type:    batch.JobSuccessCriteriaMet,
						Status:  v1.ConditionTrue,
						Reason:  batch.JobReasonCompletionsReached,
						Message: "Reached expected number of succeeded pods",
					},
					{
						Type:    batch.JobComplete,
						Status:  v1.ConditionTrue,
						Reason:  batch.JobReasonCompletionsReached,
						Message: "Reached expected number of succeeded pods",
					},
				},
			},
		},
		"when the JobSuccessPolicy is disabled, the Job never got SuccessCriteriaMet condition even if the Job has the successPolicy field": {
			job: batch.Job{
				TypeMeta:   validTypeMeta,
				ObjectMeta: validObjectMeta,
				Spec: batch.JobSpec{
					Selector:       validSelector,
					Template:       validTemplate,
					CompletionMode: ptr.To(batch.IndexedCompletion),
					Parallelism:    ptr.To[int32](3),
					Completions:    ptr.To[int32](3),
					BackoffLimit:   ptr.To[int32](math.MaxInt32),
					SuccessPolicy: &batch.SuccessPolicy{
						Rules: []batch.SuccessPolicyRule{{
							SucceededIndexes: ptr.To("0,1"),
							SucceededCount:   ptr.To[int32](1),
						}},
					},
				},
			},
			pods: []v1.Pod{
				*buildPod().uid("a2").index("0").phase(v1.PodRunning).trackingFinalizer().Pod,
				*buildPod().uid("b").index("1").phase(v1.PodSucceeded).trackingFinalizer().Pod,
				*buildPod().uid("c").index("2").phase(v1.PodRunning).trackingFinalizer().Pod,
			},
			wantStatus: batch.JobStatus{
				Active:                  2,
				Succeeded:               1,
				CompletedIndexes:        "1",
				UncountedTerminatedPods: &batch.UncountedTerminatedPods{},
			},
		},
	}
	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			featuregatetesting.SetFeatureGateDuringTest(t, feature.DefaultFeatureGate, features.JobBackoffLimitPerIndex, tc.enableBackoffLimitPerIndex)
			featuregatetesting.SetFeatureGateDuringTest(t, feature.DefaultFeatureGate, features.JobSuccessPolicy, tc.enableJobSuccessPolicy)
			featuregatetesting.SetFeatureGateDuringTest(t, feature.DefaultFeatureGate, features.JobPodReplacementPolicy, tc.enableJobPodReplacementPolicy)
			featuregatetesting.SetFeatureGateDuringTest(t, feature.DefaultFeatureGate, features.JobManagedBy, tc.enableJobManagedBy)

			clientSet := clientset.NewForConfigOrDie(&restclient.Config{Host: "", ContentConfig: restclient.ContentConfig{GroupVersion: &schema.GroupVersion{Group: "", Version: "v1"}}})
			fakeClock := clocktesting.NewFakeClock(now)
			_, ctx := ktesting.NewTestContext(t)
			manager, sharedInformerFactory := newControllerFromClientWithClock(ctx, t, clientSet, controller.NoResyncPeriodFunc, fakeClock)
			manager.podControl = &controller.FakePodControl{}
			manager.podStoreSynced = alwaysReady
			manager.jobStoreSynced = alwaysReady
			job := &tc.job

			actual := job
			manager.updateStatusHandler = func(_ context.Context, j *batch.Job) (*batch.Job, error) {
				actual = j
				return j, nil
			}
			if err := sharedInformerFactory.Batch().V1().Jobs().Informer().GetIndexer().Add(job); err != nil {
				t.Fatalf("Failed to add the Job %q to sharedInformer: %v", klog.KObj(job), err)
			}
			for i, pod := range tc.pods {
				pb := podBuilder{Pod: pod.DeepCopy()}.name(fmt.Sprintf("mypod-%d", i)).job(job)
				if isIndexedJob(job) {
					pb.index(strconv.Itoa(getCompletionIndex(pod.Annotations)))
				}
				if err := sharedInformerFactory.Core().V1().Pods().Informer().GetIndexer().Add(pb.Pod); err != nil {
					t.Fatalf("Failed to add the Pod %q to sharedInformer: %v", klog.KObj(pb.Pod), err)
				}
			}

			if err := manager.syncJob(ctx, testutil.GetKey(job, t)); err != nil {
				t.Fatalf("failed to complete syncJob: %v", err)
			}

			if diff := cmp.Diff(tc.wantStatus, actual.Status,
				cmpopts.IgnoreFields(batch.JobStatus{}, "StartTime", "CompletionTime", "Ready"),
				cmpopts.IgnoreFields(batch.JobCondition{}, "LastProbeTime", "LastTransitionTime")); diff != "" {
				t.Errorf("unexpected Job status (-want,+got):\n%s", diff)
			}
		})
	}
}

func TestSyncJobWithJobBackoffLimitPerIndex(t *testing.T) {
	_, ctx := ktesting.NewTestContext(t)
	now := time.Now()
	validObjectMeta := metav1.ObjectMeta{
		Name:      "foobar",
		UID:       uuid.NewUUID(),
		Namespace: metav1.NamespaceDefault,
	}
	validSelector := &metav1.LabelSelector{
		MatchLabels: map[string]string{"foo": "bar"},
	}
	validTemplate := v1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{
				"foo": "bar",
			},
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{Image: "foo/bar"},
			},
		},
	}

	testCases := map[string]struct {
		enableJobBackoffLimitPerIndex bool
		enableJobPodReplacementPolicy bool
		enableJobManagedBy            bool
		job                           batch.Job
		pods                          []v1.Pod
		wantStatus                    batch.JobStatus
	}{
		"successful job after a single failure within index": {
			enableJobBackoffLimitPerIndex: true,
			enableJobPodReplacementPolicy: true,
			job: batch.Job{
				TypeMeta:   metav1.TypeMeta{Kind: "Job"},
				ObjectMeta: validObjectMeta,
				Spec: batch.JobSpec{
					Selector:             validSelector,
					Template:             validTemplate,
					Parallelism:          ptr.To[int32](2),
					Completions:          ptr.To[int32](2),
					BackoffLimit:         ptr.To[int32](math.MaxInt32),
					CompletionMode:       ptr.To(batch.IndexedCompletion),
					BackoffLimitPerIndex: ptr.To[int32](1),
				},
			},
			pods: []v1.Pod{
				*buildPod().uid("a1").index("0").phase(v1.PodFailed).indexFailureCount("0").trackingFinalizer().Pod,
				*buildPod().uid("a2").index("0").phase(v1.PodSucceeded).indexFailureCount("1").trackingFinalizer().Pod,
				*buildPod().uid("b").index("1").phase(v1.PodSucceeded).indexFailureCount("0").trackingFinalizer().Pod,
			},
			wantStatus: batch.JobStatus{
				Failed:                  1,
				Succeeded:               2,
				Terminating:             ptr.To[int32](0),
				CompletedIndexes:        "0,1",
				FailedIndexes:           ptr.To(""),
				UncountedTerminatedPods: &batch.UncountedTerminatedPods{},
				Conditions: []batch.JobCondition{
					{
						Type:    batch.JobSuccessCriteriaMet,
						Status:  v1.ConditionTrue,
						Reason:  batch.JobReasonCompletionsReached,
						Message: "Reached expected number of succeeded pods",
					},
					{
						Type:    batch.JobComplete,
						Status:  v1.ConditionTrue,
						Reason:  batch.JobReasonCompletionsReached,
						Message: "Reached expected number of succeeded pods",
					},
				},
			},
		},
		"single failed pod, not counted as the replacement pod creation is delayed": {
			enableJobBackoffLimitPerIndex: true,
			enableJobPodReplacementPolicy: true,
			job: batch.Job{
				TypeMeta:   metav1.TypeMeta{Kind: "Job"},
				ObjectMeta: validObjectMeta,
				Spec: batch.JobSpec{
					Selector:             validSelector,
					Template:             validTemplate,
					Parallelism:          ptr.To[int32](2),
					Completions:          ptr.To[int32](2),
					BackoffLimit:         ptr.To[int32](math.MaxInt32),
					CompletionMode:       ptr.To(batch.IndexedCompletion),
					BackoffLimitPerIndex: ptr.To[int32](1),
				},
			},
			pods: []v1.Pod{
				*buildPod().uid("a").index("0").phase(v1.PodFailed).indexFailureCount("0").trackingFinalizer().Pod,
			},
			wantStatus: batch.JobStatus{
				Active:                  2,
				Terminating:             ptr.To[int32](0),
				UncountedTerminatedPods: &batch.UncountedTerminatedPods{},
				FailedIndexes:           ptr.To(""),
			},
		},
		"single failed pod replaced already": {
			enableJobBackoffLimitPerIndex: true,
			enableJobPodReplacementPolicy: true,
			job: batch.Job{
				TypeMeta:   metav1.TypeMeta{Kind: "Job"},
				ObjectMeta: validObjectMeta,
				Spec: batch.JobSpec{
					Selector:             validSelector,
					Template:             validTemplate,
					Parallelism:          ptr.To[int32](2),
					Completions:          ptr.To[int32](2),
					BackoffLimit:         ptr.To[int32](math.MaxInt32),
					CompletionMode:       ptr.To(batch.IndexedCompletion),
					BackoffLimitPerIndex: ptr.To[int32](1),
				},
			},
			pods: []v1.Pod{
				*buildPod().uid("a").index("0").phase(v1.PodFailed).indexFailureCount("0").trackingFinalizer().Pod,
				*buildPod().uid("b").index("0").phase(v1.PodPending).indexFailureCount("1").trackingFinalizer().Pod,
			},
			wantStatus: batch.JobStatus{
				Active:                  2,
				Failed:                  1,
				Terminating:             ptr.To[int32](0),
				UncountedTerminatedPods: &batch.UncountedTerminatedPods{},
				FailedIndexes:           ptr.To(""),
			},
		},
		"single failed index due to exceeding the backoff limit per index, the job continues": {
			enableJobBackoffLimitPerIndex: true,
			enableJobPodReplacementPolicy: true,
			job: batch.Job{
				TypeMeta:   metav1.TypeMeta{Kind: "Job"},
				ObjectMeta: validObjectMeta,
				Spec: batch.JobSpec{
					Selector:             validSelector,
					Template:             validTemplate,
					Parallelism:          ptr.To[int32](2),
					Completions:          ptr.To[int32](2),
					BackoffLimit:         ptr.To[int32](math.MaxInt32),
					CompletionMode:       ptr.To(batch.IndexedCompletion),
					BackoffLimitPerIndex: ptr.To[int32](1),
				},
			},
			pods: []v1.Pod{
				*buildPod().uid("a").index("0").phase(v1.PodFailed).indexFailureCount("1").trackingFinalizer().Pod,
			},
			wantStatus: batch.JobStatus{
				Active:                  1,
				Failed:                  1,
				FailedIndexes:           ptr.To("0"),
				Terminating:             ptr.To[int32](0),
				UncountedTerminatedPods: &batch.UncountedTerminatedPods{},
			},
		},
		"single failed index due to FailIndex action, the job continues": {
			enableJobBackoffLimitPerIndex: true,
			enableJobPodReplacementPolicy: true,
			job: batch.Job{
				TypeMeta:   metav1.TypeMeta{Kind: "Job"},
				ObjectMeta: validObjectMeta,
				Spec: batch.JobSpec{
					Selector:             validSelector,
					Template:             validTemplate,
					Parallelism:          ptr.To[int32](2),
					Completions:          ptr.To[int32](2),
					BackoffLimit:         ptr.To[int32](math.MaxInt32),
					CompletionMode:       ptr.To(batch.IndexedCompletion),
					BackoffLimitPerIndex: ptr.To[int32](1),
					PodFailurePolicy: &batch.PodFailurePolicy{
						Rules: []batch.PodFailurePolicyRule{
							{
								Action: batch.PodFailurePolicyActionFailIndex,
								OnExitCodes: &batch.PodFailurePolicyOnExitCodesRequirement{
									Operator: batch.PodFailurePolicyOnExitCodesOpIn,
									Values:   []int32{3},
								},
							},
						},
					},
				},
			},
			pods: []v1.Pod{
				*buildPod().uid("a").index("0").status(v1.PodStatus{
					Phase: v1.PodFailed,
					ContainerStatuses: []v1.ContainerStatus{
						{
							State: v1.ContainerState{
								Terminated: &v1.ContainerStateTerminated{
									ExitCode: 3,
								},
							},
						},
					},
				}).indexFailureCount("0").trackingFinalizer().Pod,
			},
			wantStatus: batch.JobStatus{
				Active:                  1,
				Failed:                  1,
				FailedIndexes:           ptr.To("0"),
				Terminating:             ptr.To[int32](0),
				UncountedTerminatedPods: &batch.UncountedTerminatedPods{},
			},
		},
		"job failed index due to FailJob action": {
			enableJobBackoffLimitPerIndex: true,
			enableJobPodReplacementPolicy: true,
			job: batch.Job{
				TypeMeta:   metav1.TypeMeta{Kind: "Job"},
				ObjectMeta: validObjectMeta,
				Spec: batch.JobSpec{
					Selector:             validSelector,
					Template:             validTemplate,
					Parallelism:          ptr.To[int32](2),
					Completions:          ptr.To[int32](2),
					BackoffLimit:         ptr.To[int32](6),
					CompletionMode:       ptr.To(batch.IndexedCompletion),
					BackoffLimitPerIndex: ptr.To[int32](1),
					PodFailurePolicy: &batch.PodFailurePolicy{
						Rules: []batch.PodFailurePolicyRule{
							{
								Action: batch.PodFailurePolicyActionFailJob,
								OnExitCodes: &batch.PodFailurePolicyOnExitCodesRequirement{
									Operator: batch.PodFailurePolicyOnExitCodesOpIn,
									Values:   []int32{3},
								},
							},
						},
					},
				},
			},
			pods: []v1.Pod{
				*buildPod().uid("a").index("0").status(v1.PodStatus{
					Phase: v1.PodFailed,
					ContainerStatuses: []v1.ContainerStatus{
						{
							Name: "x",
							State: v1.ContainerState{
								Terminated: &v1.ContainerStateTerminated{
									ExitCode: 3,
								},
							},
						},
					},
				}).indexFailureCount("0").trackingFinalizer().Pod,
			},
			wantStatus: batch.JobStatus{
				Active:                  0,
				Failed:                  1,
				FailedIndexes:           ptr.To(""),
				Terminating:             ptr.To[int32](0),
				UncountedTerminatedPods: &batch.UncountedTerminatedPods{},
				Conditions: []batch.JobCondition{
					{
						Type:    batch.JobFailureTarget,
						Status:  v1.ConditionTrue,
						Reason:  batch.JobReasonPodFailurePolicy,
						Message: "Container x for pod default/mypod-0 failed with exit code 3 matching FailJob rule at index 0",
					},
					{
						Type:    batch.JobFailed,
						Status:  v1.ConditionTrue,
						Reason:  batch.JobReasonPodFailurePolicy,
						Message: "Container x for pod default/mypod-0 failed with exit code 3 matching FailJob rule at index 0",
					},
				},
			},
		},
		"job pod failure ignored due to matching Ignore action": {
			enableJobBackoffLimitPerIndex: true,
			enableJobPodReplacementPolicy: true,
			job: batch.Job{
				TypeMeta:   metav1.TypeMeta{Kind: "Job"},
				ObjectMeta: validObjectMeta,
				Spec: batch.JobSpec{
					Selector:             validSelector,
					Template:             validTemplate,
					Parallelism:          ptr.To[int32](2),
					Completions:          ptr.To[int32](2),
					BackoffLimit:         ptr.To[int32](6),
					CompletionMode:       ptr.To(batch.IndexedCompletion),
					BackoffLimitPerIndex: ptr.To[int32](1),
					PodFailurePolicy: &batch.PodFailurePolicy{
						Rules: []batch.PodFailurePolicyRule{
							{
								Action: batch.PodFailurePolicyActionIgnore,
								OnExitCodes: &batch.PodFailurePolicyOnExitCodesRequirement{
									Operator: batch.PodFailurePolicyOnExitCodesOpIn,
									Values:   []int32{3},
								},
							},
						},
					},
				},
			},
			pods: []v1.Pod{
				*buildPod().uid("a").index("0").status(v1.PodStatus{
					Phase: v1.PodFailed,
					ContainerStatuses: []v1.ContainerStatus{
						{
							Name: "x",
							State: v1.ContainerState{
								Terminated: &v1.ContainerStateTerminated{
									ExitCode: 3,
								},
							},
						},
					},
				}).indexFailureCount("0").trackingFinalizer().Pod,
			},
			wantStatus: batch.JobStatus{
				Active:                  2,
				Failed:                  0,
				FailedIndexes:           ptr.To(""),
				Terminating:             ptr.To[int32](0),
				UncountedTerminatedPods: &batch.UncountedTerminatedPods{},
			},
		},
		"job failed due to exceeding backoffLimit before backoffLimitPerIndex": {
			enableJobBackoffLimitPerIndex: true,
			enableJobPodReplacementPolicy: true,
			job: batch.Job{
				TypeMeta:   metav1.TypeMeta{Kind: "Job"},
				ObjectMeta: validObjectMeta,
				Spec: batch.JobSpec{
					Selector:             validSelector,
					Template:             validTemplate,
					Parallelism:          ptr.To[int32](2),
					Completions:          ptr.To[int32](2),
					BackoffLimit:         ptr.To[int32](1),
					CompletionMode:       ptr.To(batch.IndexedCompletion),
					BackoffLimitPerIndex: ptr.To[int32](1),
				},
			},
			pods: []v1.Pod{
				*buildPod().uid("a").index("0").phase(v1.PodFailed).indexFailureCount("0").trackingFinalizer().Pod,
				*buildPod().uid("b").index("1").phase(v1.PodFailed).indexFailureCount("0").trackingFinalizer().Pod,
			},
			wantStatus: batch.JobStatus{
				Failed:                  2,
				Succeeded:               0,
				FailedIndexes:           ptr.To(""),
				Terminating:             ptr.To[int32](0),
				UncountedTerminatedPods: &batch.UncountedTerminatedPods{},
				Conditions: []batch.JobCondition{
					{
						Type:    batch.JobFailureTarget,
						Status:  v1.ConditionTrue,
						Reason:  batch.JobReasonBackoffLimitExceeded,
						Message: "Job has reached the specified backoff limit",
					},
					{
						Type:    batch.JobFailed,
						Status:  v1.ConditionTrue,
						Reason:  batch.JobReasonBackoffLimitExceeded,
						Message: "Job has reached the specified backoff limit",
					},
				},
			},
		},
		"job failed due to failed indexes": {
			enableJobBackoffLimitPerIndex: true,
			enableJobPodReplacementPolicy: true,
			job: batch.Job{
				TypeMeta:   metav1.TypeMeta{Kind: "Job"},
				ObjectMeta: validObjectMeta,
				Spec: batch.JobSpec{
					Selector:             validSelector,
					Template:             validTemplate,
					Parallelism:          ptr.To[int32](2),
					Completions:          ptr.To[int32](2),
					BackoffLimit:         ptr.To[int32](math.MaxInt32),
					CompletionMode:       ptr.To(batch.IndexedCompletion),
					BackoffLimitPerIndex: ptr.To[int32](1),
				},
			},
			pods: []v1.Pod{
				*buildPod().uid("a").index("0").phase(v1.PodFailed).indexFailureCount("1").trackingFinalizer().Pod,
				*buildPod().uid("b").index("1").phase(v1.PodSucceeded).indexFailureCount("0").trackingFinalizer().Pod,
			},
			wantStatus: batch.JobStatus{
				Failed:                  1,
				Succeeded:               1,
				Terminating:             ptr.To[int32](0),
				FailedIndexes:           ptr.To("0"),
				CompletedIndexes:        "1",
				UncountedTerminatedPods: &batch.UncountedTerminatedPods{},
				Conditions: []batch.JobCondition{
					{
						Type:    batch.JobFailureTarget,
						Status:  v1.ConditionTrue,
						Reason:  batch.JobReasonFailedIndexes,
						Message: "Job has failed indexes",
					},
					{
						Type:    batch.JobFailed,
						Status:  v1.ConditionTrue,
						Reason:  batch.JobReasonFailedIndexes,
						Message: "Job has failed indexes",
					},
				},
			},
		},
		"job failed due to exceeding max failed indexes": {
			enableJobBackoffLimitPerIndex: true,
			enableJobPodReplacementPolicy: true,
			job: batch.Job{
				TypeMeta:   metav1.TypeMeta{Kind: "Job"},
				ObjectMeta: validObjectMeta,
				Spec: batch.JobSpec{
					Selector:             validSelector,
					Template:             validTemplate,
					Parallelism:          ptr.To[int32](4),
					Completions:          ptr.To[int32](4),
					BackoffLimit:         ptr.To[int32](math.MaxInt32),
					CompletionMode:       ptr.To(batch.IndexedCompletion),
					BackoffLimitPerIndex: ptr.To[int32](1),
					MaxFailedIndexes:     ptr.To[int32](1),
				},
			},
			pods: []v1.Pod{
				*buildPod().uid("a").index("0").phase(v1.PodFailed).indexFailureCount("1").trackingFinalizer().Pod,
				*buildPod().uid("b").index("1").phase(v1.PodSucceeded).indexFailureCount("0").trackingFinalizer().Pod,
				*buildPod().uid("c").index("2").phase(v1.PodFailed).indexFailureCount("1").trackingFinalizer().Pod,
				*buildPod().uid("d").index("3").phase(v1.PodRunning).indexFailureCount("0").trackingFinalizer().Pod,
			},
			wantStatus: batch.JobStatus{
				Failed:                  3,
				Succeeded:               1,
				Terminating:             ptr.To[int32](1),
				FailedIndexes:           ptr.To("0,2"),
				CompletedIndexes:        "1",
				UncountedTerminatedPods: &batch.UncountedTerminatedPods{},
				Conditions: []batch.JobCondition{
					{
						Type:    batch.JobFailureTarget,
						Status:  v1.ConditionTrue,
						Reason:  batch.JobReasonMaxFailedIndexesExceeded,
						Message: "Job has exceeded the specified maximal number of failed indexes",
					},
				},
			},
		},
		"job with finished indexes; failedIndexes are cleaned when JobBackoffLimitPerIndex disabled": {
			enableJobBackoffLimitPerIndex: false,
			enableJobPodReplacementPolicy: true,
			job: batch.Job{
				TypeMeta:   metav1.TypeMeta{Kind: "Job"},
				ObjectMeta: validObjectMeta,
				Spec: batch.JobSpec{
					Selector:             validSelector,
					Template:             validTemplate,
					Parallelism:          ptr.To[int32](3),
					Completions:          ptr.To[int32](3),
					BackoffLimit:         ptr.To[int32](math.MaxInt32),
					CompletionMode:       ptr.To(batch.IndexedCompletion),
					BackoffLimitPerIndex: ptr.To[int32](1),
				},
				Status: batch.JobStatus{
					FailedIndexes:    ptr.To("0"),
					CompletedIndexes: "1",
				},
			},
			pods: []v1.Pod{
				*buildPod().uid("c").index("2").phase(v1.PodPending).indexFailureCount("1").trackingFinalizer().Pod,
			},
			wantStatus: batch.JobStatus{
				Active:                  2,
				Succeeded:               1,
				Terminating:             ptr.To[int32](0),
				CompletedIndexes:        "1",
				UncountedTerminatedPods: &batch.UncountedTerminatedPods{},
			},
		},
		"job failed due to failed indexes; JobPodReplacementPolicy and JobManagedBy disabled": {
			enableJobBackoffLimitPerIndex: true,
			job: batch.Job{
				TypeMeta:   metav1.TypeMeta{Kind: "Job"},
				ObjectMeta: validObjectMeta,
				Spec: batch.JobSpec{
					Selector:             validSelector,
					Template:             validTemplate,
					Parallelism:          ptr.To[int32](2),
					Completions:          ptr.To[int32](2),
					BackoffLimit:         ptr.To[int32](math.MaxInt32),
					CompletionMode:       ptr.To(batch.IndexedCompletion),
					BackoffLimitPerIndex: ptr.To[int32](1),
				},
			},
			pods: []v1.Pod{
				*buildPod().uid("a").index("0").phase(v1.PodFailed).indexFailureCount("1").trackingFinalizer().Pod,
				*buildPod().uid("b").index("1").phase(v1.PodSucceeded).indexFailureCount("0").trackingFinalizer().Pod,
			},
			wantStatus: batch.JobStatus{
				Failed:                  1,
				Succeeded:               1,
				FailedIndexes:           ptr.To("0"),
				CompletedIndexes:        "1",
				UncountedTerminatedPods: &batch.UncountedTerminatedPods{},
				Conditions: []batch.JobCondition{
					{
						Type:    batch.JobFailed,
						Status:  v1.ConditionTrue,
						Reason:  batch.JobReasonFailedIndexes,
						Message: "Job has failed indexes",
					},
				},
			},
		},
		"job failed due to failed indexes; JobManagedBy enabled": {
			enableJobBackoffLimitPerIndex: true,
			enableJobManagedBy:            true,
			job: batch.Job{
				TypeMeta:   metav1.TypeMeta{Kind: "Job"},
				ObjectMeta: validObjectMeta,
				Spec: batch.JobSpec{
					Selector:             validSelector,
					Template:             validTemplate,
					Parallelism:          ptr.To[int32](2),
					Completions:          ptr.To[int32](2),
					BackoffLimit:         ptr.To[int32](math.MaxInt32),
					CompletionMode:       ptr.To(batch.IndexedCompletion),
					BackoffLimitPerIndex: ptr.To[int32](1),
				},
			},
			pods: []v1.Pod{
				*buildPod().uid("a").index("0").phase(v1.PodFailed).indexFailureCount("1").trackingFinalizer().Pod,
				*buildPod().uid("b").index("1").phase(v1.PodSucceeded).indexFailureCount("0").trackingFinalizer().Pod,
			},
			wantStatus: batch.JobStatus{
				Failed:                  1,
				Succeeded:               1,
				FailedIndexes:           ptr.To("0"),
				CompletedIndexes:        "1",
				UncountedTerminatedPods: &batch.UncountedTerminatedPods{},
				Conditions: []batch.JobCondition{
					{
						Type:    batch.JobFailureTarget,
						Status:  v1.ConditionTrue,
						Reason:  batch.JobReasonFailedIndexes,
						Message: "Job has failed indexes",
					},
					{
						Type:    batch.JobFailed,
						Status:  v1.ConditionTrue,
						Reason:  batch.JobReasonFailedIndexes,
						Message: "Job has failed indexes",
					},
				},
			},
		},
		"job failed due to exceeding max failed indexes; JobPodReplacementPolicy and JobManagedBy disabled": {
			enableJobBackoffLimitPerIndex: true,
			job: batch.Job{
				TypeMeta:   metav1.TypeMeta{Kind: "Job"},
				ObjectMeta: validObjectMeta,
				Spec: batch.JobSpec{
					Selector:             validSelector,
					Template:             validTemplate,
					Parallelism:          ptr.To[int32](4),
					Completions:          ptr.To[int32](4),
					BackoffLimit:         ptr.To[int32](math.MaxInt32),
					CompletionMode:       ptr.To(batch.IndexedCompletion),
					BackoffLimitPerIndex: ptr.To[int32](1),
					MaxFailedIndexes:     ptr.To[int32](1),
				},
			},
			pods: []v1.Pod{
				*buildPod().uid("a").index("0").phase(v1.PodFailed).indexFailureCount("1").trackingFinalizer().Pod,
				*buildPod().uid("b").index("1").phase(v1.PodSucceeded).indexFailureCount("0").trackingFinalizer().Pod,
				*buildPod().uid("c").index("2").phase(v1.PodFailed).indexFailureCount("1").trackingFinalizer().Pod,
				*buildPod().uid("d").index("3").phase(v1.PodRunning).indexFailureCount("0").trackingFinalizer().Pod,
			},
			wantStatus: batch.JobStatus{
				Failed:                  3,
				Succeeded:               1,
				FailedIndexes:           ptr.To("0,2"),
				CompletedIndexes:        "1",
				UncountedTerminatedPods: &batch.UncountedTerminatedPods{},
				Conditions: []batch.JobCondition{
					{
						Type:    batch.JobFailed,
						Status:  v1.ConditionTrue,
						Reason:  batch.JobReasonMaxFailedIndexesExceeded,
						Message: "Job has exceeded the specified maximal number of failed indexes",
					},
				},
			},
		},
		"job failed due to exceeding max failed indexes; JobManagedBy enabled": {
			enableJobBackoffLimitPerIndex: true,
			enableJobManagedBy:            true,
			job: batch.Job{
				TypeMeta:   metav1.TypeMeta{Kind: "Job"},
				ObjectMeta: validObjectMeta,
				Spec: batch.JobSpec{
					Selector:             validSelector,
					Template:             validTemplate,
					Parallelism:          ptr.To[int32](4),
					Completions:          ptr.To[int32](4),
					BackoffLimit:         ptr.To[int32](math.MaxInt32),
					CompletionMode:       ptr.To(batch.IndexedCompletion),
					BackoffLimitPerIndex: ptr.To[int32](1),
					MaxFailedIndexes:     ptr.To[int32](1),
				},
			},
			pods: []v1.Pod{
				*buildPod().uid("a").index("0").phase(v1.PodFailed).indexFailureCount("1").trackingFinalizer().Pod,
				*buildPod().uid("b").index("1").phase(v1.PodSucceeded).indexFailureCount("0").trackingFinalizer().Pod,
				*buildPod().uid("c").index("2").phase(v1.PodFailed).indexFailureCount("1").trackingFinalizer().Pod,
				*buildPod().uid("d").index("3").phase(v1.PodRunning).indexFailureCount("0").trackingFinalizer().Pod,
			},
			wantStatus: batch.JobStatus{
				Failed:                  3,
				Succeeded:               1,
				FailedIndexes:           ptr.To("0,2"),
				CompletedIndexes:        "1",
				UncountedTerminatedPods: &batch.UncountedTerminatedPods{},
				Conditions: []batch.JobCondition{
					{
						Type:    batch.JobFailureTarget,
						Status:  v1.ConditionTrue,
						Reason:  batch.JobReasonMaxFailedIndexesExceeded,
						Message: "Job has exceeded the specified maximal number of failed indexes",
					},
				},
			},
		},
	}
	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			featuregatetesting.SetFeatureGateDuringTest(t, feature.DefaultFeatureGate, features.JobBackoffLimitPerIndex, tc.enableJobBackoffLimitPerIndex)
			featuregatetesting.SetFeatureGateDuringTest(t, feature.DefaultFeatureGate, features.JobPodReplacementPolicy, tc.enableJobPodReplacementPolicy)
			featuregatetesting.SetFeatureGateDuringTest(t, feature.DefaultFeatureGate, features.JobManagedBy, tc.enableJobManagedBy)
			clientset := clientset.NewForConfigOrDie(&restclient.Config{Host: "", ContentConfig: restclient.ContentConfig{GroupVersion: &schema.GroupVersion{Group: "", Version: "v1"}}})
			fakeClock := clocktesting.NewFakeClock(now)
			manager, sharedInformerFactory := newControllerFromClientWithClock(ctx, t, clientset, controller.NoResyncPeriodFunc, fakeClock)
			fakePodControl := controller.FakePodControl{}
			manager.podControl = &fakePodControl
			manager.podStoreSynced = alwaysReady
			manager.jobStoreSynced = alwaysReady
			job := &tc.job

			actual := job
			manager.updateStatusHandler = func(ctx context.Context, job *batch.Job) (*batch.Job, error) {
				actual = job
				return job, nil
			}
			sharedInformerFactory.Batch().V1().Jobs().Informer().GetIndexer().Add(job)
			for i, pod := range tc.pods {
				pod := pod
				pb := podBuilder{Pod: &pod}.name(fmt.Sprintf("mypod-%d", i)).job(job)
				if job.Spec.CompletionMode != nil && *job.Spec.CompletionMode == batch.IndexedCompletion {
					pb.index(fmt.Sprintf("%v", getCompletionIndex(pod.Annotations)))
				}
				pb = pb.trackingFinalizer()
				sharedInformerFactory.Core().V1().Pods().Informer().GetIndexer().Add(pb.Pod)
			}

			if err := manager.syncJob(ctx, testutil.GetKey(job, t)); err != nil {
				t.Fatalf("error %v while reconciling the job %v", err, testutil.GetKey(job, t))
			}

			// validate relevant fields of the status
			if diff := cmp.Diff(tc.wantStatus, actual.Status,
				cmpopts.IgnoreFields(batch.JobStatus{}, "StartTime", "CompletionTime", "Ready"),
				cmpopts.IgnoreFields(batch.JobCondition{}, "LastProbeTime", "LastTransitionTime")); diff != "" {
				t.Errorf("unexpected job status. Diff: %s\n", diff)
			}
		})
	}
}

func TestSyncJobUpdateRequeue(t *testing.T) {
	_, ctx := ktesting.NewTestContext(t)
	clientset := clientset.NewForConfigOrDie(&restclient.Config{Host: "", ContentConfig: restclient.ContentConfig{GroupVersion: &schema.GroupVersion{Group: "", Version: "v1"}}})
	cases := map[string]struct {
		updateErr    error
		wantRequeued bool
	}{
		"no error": {
			wantRequeued: false,
		},
		"generic error": {
			updateErr:    fmt.Errorf("update error"),
			wantRequeued: true,
		},
		"conflict error": {
			updateErr:    apierrors.NewConflict(schema.GroupResource{}, "", nil),
			wantRequeued: true,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Cleanup(setDurationDuringTest(&DefaultJobApiBackOff, fastJobApiBackoff))
			fakeClient := clocktesting.NewFakeClock(time.Now())
			manager, sharedInformerFactory := newControllerFromClientWithClock(ctx, t, clientset, controller.NoResyncPeriodFunc, fakeClient)
			fakePodControl := controller.FakePodControl{}
			manager.podControl = &fakePodControl
			manager.podStoreSynced = alwaysReady
			manager.jobStoreSynced = alwaysReady
			manager.updateStatusHandler = func(ctx context.Context, job *batch.Job) (*batch.Job, error) {
				return job, tc.updateErr
			}
			job := newJob(2, 2, 6, batch.NonIndexedCompletion)
			sharedInformerFactory.Batch().V1().Jobs().Informer().GetIndexer().Add(job)
			manager.queue.Add(testutil.GetKey(job, t))
			manager.processNextWorkItem(ctx)
			if tc.wantRequeued {
				verifyEmptyQueueAndAwaitForQueueLen(ctx, t, manager, 1)
			} else {
				// We advance the clock to make sure there are not items awaiting
				// to be added into the queue. We also sleep a little to give the
				// delaying queue time to move the potential items from pre-queue
				// into the queue asynchronously.
				manager.clock.Sleep(fastJobApiBackoff)
				time.Sleep(time.Millisecond)
				verifyEmptyQueue(t, manager)
			}
		})
	}
}

func TestUpdateJobRequeue(t *testing.T) {
	logger, ctx := ktesting.NewTestContext(t)
	clientset := clientset.NewForConfigOrDie(&restclient.Config{Host: "", ContentConfig: restclient.ContentConfig{GroupVersion: &schema.GroupVersion{Group: "", Version: "v1"}}})
	cases := map[string]struct {
		oldJob                  *batch.Job
		updateFn                func(job *batch.Job)
		wantRequeuedImmediately bool
	}{
		"spec update": {
			oldJob: newJob(1, 1, 1, batch.IndexedCompletion),
			updateFn: func(job *batch.Job) {
				job.Spec.Suspend = ptr.To(false)
				job.Generation++
			},
			wantRequeuedImmediately: true,
		},
		"status update": {
			oldJob: newJob(1, 1, 1, batch.IndexedCompletion),
			updateFn: func(job *batch.Job) {
				job.Status.StartTime = &metav1.Time{Time: time.Now()}
			},
			wantRequeuedImmediately: false,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			manager, sharedInformerFactory := newControllerFromClient(ctx, t, clientset, controller.NoResyncPeriodFunc)
			manager.podStoreSynced = alwaysReady
			manager.jobStoreSynced = alwaysReady

			sharedInformerFactory.Batch().V1().Jobs().Informer().GetIndexer().Add(tc.oldJob)
			newJob := tc.oldJob.DeepCopy()
			if tc.updateFn != nil {
				tc.updateFn(newJob)
			}
			manager.updateJob(logger, tc.oldJob, newJob)
			gotRequeuedImmediately := manager.queue.Len() > 0
			if tc.wantRequeuedImmediately != gotRequeuedImmediately {
				t.Fatalf("Want immediate requeue: %v, got immediate requeue: %v", tc.wantRequeuedImmediately, gotRequeuedImmediately)
			}
		})
	}
}

func TestGetPodCreationInfoForIndependentIndexes(t *testing.T) {
	logger, ctx := ktesting.NewTestContext(t)
	now := time.Now()
	clientset := clientset.NewForConfigOrDie(&restclient.Config{Host: "", ContentConfig: restclient.ContentConfig{GroupVersion: &schema.GroupVersion{Group: "", Version: "v1"}}})
	cases := map[string]struct {
		indexesToAdd                    []int
		podsWithDelayedDeletionPerIndex map[int]*v1.Pod
		wantIndexesToAdd                []int
		wantRemainingTime               time.Duration
	}{
		"simple index creation": {
			indexesToAdd:     []int{1, 3},
			wantIndexesToAdd: []int{1, 3},
		},
		"subset of indexes can be recreated now": {
			indexesToAdd: []int{1, 3},
			podsWithDelayedDeletionPerIndex: map[int]*v1.Pod{
				1: buildPod().indexFailureCount("0").index("1").customDeletionTimestamp(now).Pod,
			},
			wantIndexesToAdd: []int{3},
		},
		"subset of indexes can be recreated now as the pods failed long time ago": {
			indexesToAdd: []int{1, 3},
			podsWithDelayedDeletionPerIndex: map[int]*v1.Pod{
				1: buildPod().indexFailureCount("0").customDeletionTimestamp(now).Pod,
				3: buildPod().indexFailureCount("0").customDeletionTimestamp(now.Add(-DefaultJobPodFailureBackOff)).Pod,
			},
			wantIndexesToAdd: []int{3},
		},
		"no indexes can be recreated now, need to wait default pod failure backoff": {
			indexesToAdd: []int{1, 2, 3},
			podsWithDelayedDeletionPerIndex: map[int]*v1.Pod{
				1: buildPod().indexFailureCount("1").customDeletionTimestamp(now).Pod,
				2: buildPod().indexFailureCount("0").customDeletionTimestamp(now).Pod,
				3: buildPod().indexFailureCount("2").customDeletionTimestamp(now).Pod,
			},
			wantRemainingTime: DefaultJobPodFailureBackOff,
		},
		"no indexes can be recreated now, need to wait but 1s already passed": {
			indexesToAdd: []int{1, 2, 3},
			podsWithDelayedDeletionPerIndex: map[int]*v1.Pod{
				1: buildPod().indexFailureCount("1").customDeletionTimestamp(now.Add(-time.Second)).Pod,
				2: buildPod().indexFailureCount("0").customDeletionTimestamp(now.Add(-time.Second)).Pod,
				3: buildPod().indexFailureCount("2").customDeletionTimestamp(now.Add(-time.Second)).Pod,
			},
			wantRemainingTime: DefaultJobPodFailureBackOff - time.Second,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			fakeClock := clocktesting.NewFakeClock(now)
			manager, _ := newControllerFromClientWithClock(ctx, t, clientset, controller.NoResyncPeriodFunc, fakeClock)
			gotIndexesToAdd, gotRemainingTime := manager.getPodCreationInfoForIndependentIndexes(logger, tc.indexesToAdd, tc.podsWithDelayedDeletionPerIndex)
			if diff := cmp.Diff(tc.wantIndexesToAdd, gotIndexesToAdd); diff != "" {
				t.Fatalf("Unexpected indexes to add: %s", diff)
			}
			if diff := cmp.Diff(tc.wantRemainingTime, gotRemainingTime); diff != "" {
				t.Fatalf("Unexpected remaining time: %s", diff)
			}
		})
	}
}

func TestJobPodLookup(t *testing.T) {
	_, ctx := ktesting.NewTestContext(t)
	clientset := clientset.NewForConfigOrDie(&restclient.Config{Host: "", ContentConfig: restclient.ContentConfig{GroupVersion: &schema.GroupVersion{Group: "", Version: "v1"}}})
	manager, sharedInformerFactory := newControllerFromClient(ctx, t, clientset, controller.NoResyncPeriodFunc)
	manager.podStoreSynced = alwaysReady
	manager.jobStoreSynced = alwaysReady
	testCases := []struct {
		job *batch.Job
		pod *v1.Pod

		expectedName string
	}{
		// pods without labels don't match any job
		{
			job: &batch.Job{
				ObjectMeta: metav1.ObjectMeta{Name: "basic"},
			},
			pod: &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "foo1", Namespace: metav1.NamespaceAll},
			},
			expectedName: "",
		},
		// matching labels, different namespace
		{
			job: &batch.Job{
				ObjectMeta: metav1.ObjectMeta{Name: "foo"},
				Spec: batch.JobSpec{
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"foo": "bar"},
					},
				},
			},
			pod: &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "foo2",
					Namespace: "ns",
					Labels:    map[string]string{"foo": "bar"},
				},
			},
			expectedName: "",
		},
		// matching ns and labels returns
		{
			job: &batch.Job{
				ObjectMeta: metav1.ObjectMeta{Name: "bar", Namespace: "ns"},
				Spec: batch.JobSpec{
					Selector: &metav1.LabelSelector{
						MatchExpressions: []metav1.LabelSelectorRequirement{
							{
								Key:      "foo",
								Operator: metav1.LabelSelectorOpIn,
								Values:   []string{"bar"},
							},
						},
					},
				},
			},
			pod: &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "foo3",
					Namespace: "ns",
					Labels:    map[string]string{"foo": "bar"},
				},
			},
			expectedName: "bar",
		},
	}
	for _, tc := range testCases {
		sharedInformerFactory.Batch().V1().Jobs().Informer().GetIndexer().Add(tc.job)
		if jobs := manager.getPodJobs(tc.pod); len(jobs) > 0 {
			if got, want := len(jobs), 1; got != want {
				t.Errorf("len(jobs) = %v, want %v", got, want)
			}
			job := jobs[0]
			if tc.expectedName != job.Name {
				t.Errorf("Got job %+v expected %+v", job.Name, tc.expectedName)
			}
		} else if tc.expectedName != "" {
			t.Errorf("Expected a job %v pod %v, found none", tc.expectedName, tc.pod.Name)
		}
	}
}

func TestGetPodsForJob(t *testing.T) {
	_, ctx := ktesting.NewTestContext(t)
	job := newJob(1, 1, 6, batch.NonIndexedCompletion)
	job.Name = "test_job"
	otherJob := newJob(1, 1, 6, batch.NonIndexedCompletion)
	otherJob.Name = "other_job"
	cases := map[string]struct {
		jobDeleted        bool
		jobDeletedInCache bool
		pods              []*v1.Pod
		wantPods          []string
		wantPodsFinalizer []string
	}{
		"only matching": {
			pods: []*v1.Pod{
				buildPod().name("pod1").job(job).trackingFinalizer().Pod,
				buildPod().name("pod2").job(otherJob).Pod,
				buildPod().name("pod3").ns(job.Namespace).Pod,
				buildPod().name("pod4").job(job).Pod,
			},
			wantPods:          []string{"pod1", "pod4"},
			wantPodsFinalizer: []string{"pod1"},
		},
		"adopt": {
			pods: []*v1.Pod{
				buildPod().name("pod1").job(job).Pod,
				buildPod().name("pod2").job(job).clearOwner().Pod,
				buildPod().name("pod3").job(otherJob).Pod,
			},
			wantPods:          []string{"pod1", "pod2"},
			wantPodsFinalizer: []string{"pod2"},
		},
		"no adopt when deleting": {
			jobDeleted:        true,
			jobDeletedInCache: true,
			pods: []*v1.Pod{
				buildPod().name("pod1").job(job).Pod,
				buildPod().name("pod2").job(job).clearOwner().Pod,
			},
			wantPods: []string{"pod1"},
		},
		"no adopt when deleting race": {
			jobDeleted: true,
			pods: []*v1.Pod{
				buildPod().name("pod1").job(job).Pod,
				buildPod().name("pod2").job(job).clearOwner().Pod,
			},
			wantPods: []string{"pod1"},
		},
		"release": {
			pods: []*v1.Pod{
				buildPod().name("pod1").job(job).Pod,
				buildPod().name("pod2").job(job).clearLabels().Pod,
			},
			wantPods: []string{"pod1"},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			job := job.DeepCopy()
			if tc.jobDeleted {
				job.DeletionTimestamp = &metav1.Time{}
			}
			clientSet := fake.NewClientset(job, otherJob)
			jm, informer := newControllerFromClient(ctx, t, clientSet, controller.NoResyncPeriodFunc)
			jm.podStoreSynced = alwaysReady
			jm.jobStoreSynced = alwaysReady
			cachedJob := job.DeepCopy()
			if tc.jobDeletedInCache {
				cachedJob.DeletionTimestamp = &metav1.Time{}
			}
			informer.Batch().V1().Jobs().Informer().GetIndexer().Add(cachedJob)
			informer.Batch().V1().Jobs().Informer().GetIndexer().Add(otherJob)
			for _, p := range tc.pods {
				informer.Core().V1().Pods().Informer().GetIndexer().Add(p)
			}

			pods, err := jm.getPodsForJob(ctx, job)
			if err != nil {
				t.Fatalf("getPodsForJob() error: %v", err)
			}
			got := make([]string, len(pods))
			var gotFinalizer []string
			for i, p := range pods {
				got[i] = p.Name
				if hasJobTrackingFinalizer(p) {
					gotFinalizer = append(gotFinalizer, p.Name)
				}
			}
			sort.Strings(got)
			if diff := cmp.Diff(tc.wantPods, got); diff != "" {
				t.Errorf("getPodsForJob() returned (-want,+got):\n%s", diff)
			}
			sort.Strings(gotFinalizer)
			if diff := cmp.Diff(tc.wantPodsFinalizer, gotFinalizer); diff != "" {
				t.Errorf("Pods with finalizers (-want,+got):\n%s", diff)
			}
		})
	}
}

func TestAddPod(t *testing.T) {
	t.Cleanup(setDurationDuringTest(&syncJobBatchPeriod, fastSyncJobBatchPeriod))
	_, ctx := ktesting.NewTestContext(t)
	logger := klog.FromContext(ctx)

	clientset := clientset.NewForConfigOrDie(&restclient.Config{Host: "", ContentConfig: restclient.ContentConfig{GroupVersion: &schema.GroupVersion{Group: "", Version: "v1"}}})
	fakeClock := clocktesting.NewFakeClock(time.Now())
	jm, informer := newControllerFromClientWithClock(ctx, t, clientset, controller.NoResyncPeriodFunc, fakeClock)
	jm.podStoreSynced = alwaysReady
	jm.jobStoreSynced = alwaysReady

	job1 := newJob(1, 1, 6, batch.NonIndexedCompletion)
	job1.Name = "job1"
	job2 := newJob(1, 1, 6, batch.NonIndexedCompletion)
	job2.Name = "job2"
	informer.Batch().V1().Jobs().Informer().GetIndexer().Add(job1)
	informer.Batch().V1().Jobs().Informer().GetIndexer().Add(job2)

	pod1 := newPod("pod1", job1)
	pod2 := newPod("pod2", job2)
	informer.Core().V1().Pods().Informer().GetIndexer().Add(pod1)
	informer.Core().V1().Pods().Informer().GetIndexer().Add(pod2)

	jm.addPod(logger, pod1)
	verifyEmptyQueueAndAwaitForQueueLen(ctx, t, jm, 1)
	key, done := jm.queue.Get()
	if key == "" || done {
		t.Fatalf("failed to enqueue controller for pod %v", pod1.Name)
	}
	expectedKey, _ := controller.KeyFunc(job1)
	if got, want := key, expectedKey; got != want {
		t.Errorf("queue.Get() = %v, want %v", got, want)
	}

	jm.addPod(logger, pod2)
	verifyEmptyQueueAndAwaitForQueueLen(ctx, t, jm, 1)
	key, done = jm.queue.Get()
	if key == "" || done {
		t.Fatalf("failed to enqueue controller for pod %v", pod2.Name)
	}
	expectedKey, _ = controller.KeyFunc(job2)
	if got, want := key, expectedKey; got != want {
		t.Errorf("queue.Get() = %v, want %v", got, want)
	}
}

func TestAddPodOrphan(t *testing.T) {
	t.Cleanup(setDurationDuringTest(&syncJobBatchPeriod, fastSyncJobBatchPeriod))
	logger, ctx := ktesting.NewTestContext(t)
	clientset := clientset.NewForConfigOrDie(&restclient.Config{Host: "", ContentConfig: restclient.ContentConfig{GroupVersion: &schema.GroupVersion{Group: "", Version: "v1"}}})
	fakeClock := clocktesting.NewFakeClock(time.Now())
	jm, informer := newControllerFromClientWithClock(ctx, t, clientset, controller.NoResyncPeriodFunc, fakeClock)
	jm.podStoreSynced = alwaysReady
	jm.jobStoreSynced = alwaysReady

	job1 := newJob(1, 1, 6, batch.NonIndexedCompletion)
	job1.Name = "job1"
	job2 := newJob(1, 1, 6, batch.NonIndexedCompletion)
	job2.Name = "job2"
	job3 := newJob(1, 1, 6, batch.NonIndexedCompletion)
	job3.Name = "job3"
	job3.Spec.Selector.MatchLabels = map[string]string{"other": "labels"}
	informer.Batch().V1().Jobs().Informer().GetIndexer().Add(job1)
	informer.Batch().V1().Jobs().Informer().GetIndexer().Add(job2)
	informer.Batch().V1().Jobs().Informer().GetIndexer().Add(job3)

	pod1 := newPod("pod1", job1)
	// Make pod an orphan. Expect all matching controllers to be queued.
	pod1.OwnerReferences = nil
	informer.Core().V1().Pods().Informer().GetIndexer().Add(pod1)

	jm.addPod(logger, pod1)
	verifyEmptyQueueAndAwaitForQueueLen(ctx, t, jm, 2)
}

func TestUpdatePod(t *testing.T) {
	t.Cleanup(setDurationDuringTest(&syncJobBatchPeriod, fastSyncJobBatchPeriod))
	_, ctx := ktesting.NewTestContext(t)
	logger := klog.FromContext(ctx)
	clientset := clientset.NewForConfigOrDie(&restclient.Config{Host: "", ContentConfig: restclient.ContentConfig{GroupVersion: &schema.GroupVersion{Group: "", Version: "v1"}}})
	fakeClock := clocktesting.NewFakeClock(time.Now())
	jm, informer := newControllerFromClientWithClock(ctx, t, clientset, controller.NoResyncPeriodFunc, fakeClock)
	jm.podStoreSynced = alwaysReady
	jm.jobStoreSynced = alwaysReady

	job1 := newJob(1, 1, 6, batch.NonIndexedCompletion)
	job1.Name = "job1"
	job2 := newJob(1, 1, 6, batch.NonIndexedCompletion)
	job2.Name = "job2"
	informer.Batch().V1().Jobs().Informer().GetIndexer().Add(job1)
	informer.Batch().V1().Jobs().Informer().GetIndexer().Add(job2)

	pod1 := newPod("pod1", job1)
	pod2 := newPod("pod2", job2)
	informer.Core().V1().Pods().Informer().GetIndexer().Add(pod1)
	informer.Core().V1().Pods().Informer().GetIndexer().Add(pod2)

	prev := *pod1
	bumpResourceVersion(pod1)
	jm.updatePod(logger, &prev, pod1)
	verifyEmptyQueueAndAwaitForQueueLen(ctx, t, jm, 1)
	key, done := jm.queue.Get()
	if key == "" || done {
		t.Fatalf("failed to enqueue controller for pod %v", pod1.Name)
	}
	expectedKey, _ := controller.KeyFunc(job1)
	if got, want := key, expectedKey; got != want {
		t.Errorf("queue.Get() = %v, want %v", got, want)
	}

	prev = *pod2
	bumpResourceVersion(pod2)
	jm.updatePod(logger, &prev, pod2)
	verifyEmptyQueueAndAwaitForQueueLen(ctx, t, jm, 1)
	key, done = jm.queue.Get()
	if key == "" || done {
		t.Fatalf("failed to enqueue controller for pod %v", pod2.Name)
	}
	expectedKey, _ = controller.KeyFunc(job2)
	if got, want := key, expectedKey; got != want {
		t.Errorf("queue.Get() = %v, want %v", got, want)
	}
}

func TestUpdatePodOrphanWithNewLabels(t *testing.T) {
	t.Cleanup(setDurationDuringTest(&syncJobBatchPeriod, fastSyncJobBatchPeriod))
	logger, ctx := ktesting.NewTestContext(t)
	clientset := clientset.NewForConfigOrDie(&restclient.Config{Host: "", ContentConfig: restclient.ContentConfig{GroupVersion: &schema.GroupVersion{Group: "", Version: "v1"}}})
	fakeClock := clocktesting.NewFakeClock(time.Now())
	jm, informer := newControllerFromClientWithClock(ctx, t, clientset, controller.NoResyncPeriodFunc, fakeClock)
	jm.podStoreSynced = alwaysReady
	jm.jobStoreSynced = alwaysReady

	job1 := newJob(1, 1, 6, batch.NonIndexedCompletion)
	job1.Name = "job1"
	job2 := newJob(1, 1, 6, batch.NonIndexedCompletion)
	job2.Name = "job2"
	informer.Batch().V1().Jobs().Informer().GetIndexer().Add(job1)
	informer.Batch().V1().Jobs().Informer().GetIndexer().Add(job2)

	pod1 := newPod("pod1", job1)
	pod1.OwnerReferences = nil
	informer.Core().V1().Pods().Informer().GetIndexer().Add(pod1)

	// Labels changed on orphan. Expect newly matching controllers to queue.
	prev := *pod1
	prev.Labels = map[string]string{"foo2": "bar2"}
	bumpResourceVersion(pod1)
	jm.updatePod(logger, &prev, pod1)
	verifyEmptyQueueAndAwaitForQueueLen(ctx, t, jm, 2)
}

func TestUpdatePodChangeControllerRef(t *testing.T) {
	t.Cleanup(setDurationDuringTest(&syncJobBatchPeriod, fastSyncJobBatchPeriod))
	_, ctx := ktesting.NewTestContext(t)
	logger := klog.FromContext(ctx)
	clientset := clientset.NewForConfigOrDie(&restclient.Config{Host: "", ContentConfig: restclient.ContentConfig{GroupVersion: &schema.GroupVersion{Group: "", Version: "v1"}}})
	fakeClock := clocktesting.NewFakeClock(time.Now())
	jm, informer := newControllerFromClientWithClock(ctx, t, clientset, controller.NoResyncPeriodFunc, fakeClock)
	jm.podStoreSynced = alwaysReady
	jm.jobStoreSynced = alwaysReady

	job1 := newJob(1, 1, 6, batch.NonIndexedCompletion)
	job1.Name = "job1"
	job2 := newJob(1, 1, 6, batch.NonIndexedCompletion)
	job2.Name = "job2"
	informer.Batch().V1().Jobs().Informer().GetIndexer().Add(job1)
	informer.Batch().V1().Jobs().Informer().GetIndexer().Add(job2)

	pod1 := newPod("pod1", job1)
	informer.Core().V1().Pods().Informer().GetIndexer().Add(pod1)

	// Changed ControllerRef. Expect both old and new to queue.
	prev := *pod1
	prev.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(job2, controllerKind)}
	bumpResourceVersion(pod1)
	jm.updatePod(logger, &prev, pod1)
	verifyEmptyQueueAndAwaitForQueueLen(ctx, t, jm, 2)
}

func TestUpdatePodRelease(t *testing.T) {
	t.Cleanup(setDurationDuringTest(&syncJobBatchPeriod, fastSyncJobBatchPeriod))
	_, ctx := ktesting.NewTestContext(t)
	logger := klog.FromContext(ctx)
	clientset := clientset.NewForConfigOrDie(&restclient.Config{Host: "", ContentConfig: restclient.ContentConfig{GroupVersion: &schema.GroupVersion{Group: "", Version: "v1"}}})
	fakeClock := clocktesting.NewFakeClock(time.Now())
	jm, informer := newControllerFromClientWithClock(ctx, t, clientset, controller.NoResyncPeriodFunc, fakeClock)
	jm.podStoreSynced = alwaysReady
	jm.jobStoreSynced = alwaysReady

	job1 := newJob(1, 1, 6, batch.NonIndexedCompletion)
	job1.Name = "job1"
	job2 := newJob(1, 1, 6, batch.NonIndexedCompletion)
	job2.Name = "job2"
	informer.Batch().V1().Jobs().Informer().GetIndexer().Add(job1)
	informer.Batch().V1().Jobs().Informer().GetIndexer().Add(job2)

	pod1 := newPod("pod1", job1)
	informer.Core().V1().Pods().Informer().GetIndexer().Add(pod1)

	// Remove ControllerRef. Expect all matching to queue for adoption.
	prev := *pod1
	pod1.OwnerReferences = nil
	bumpResourceVersion(pod1)
	jm.updatePod(logger, &prev, pod1)
	verifyEmptyQueueAndAwaitForQueueLen(ctx, t, jm, 2)
}

func TestDeletePod(t *testing.T) {
	t.Cleanup(setDurationDuringTest(&syncJobBatchPeriod, fastSyncJobBatchPeriod))
	logger, ctx := ktesting.NewTestContext(t)
	clientset := clientset.NewForConfigOrDie(&restclient.Config{Host: "", ContentConfig: restclient.ContentConfig{GroupVersion: &schema.GroupVersion{Group: "", Version: "v1"}}})
	fakeClock := clocktesting.NewFakeClock(time.Now())
	jm, informer := newControllerFromClientWithClock(ctx, t, clientset, controller.NoResyncPeriodFunc, fakeClock)
	jm.podStoreSynced = alwaysReady
	jm.jobStoreSynced = alwaysReady

	job1 := newJob(1, 1, 6, batch.NonIndexedCompletion)
	job1.Name = "job1"
	job2 := newJob(1, 1, 6, batch.NonIndexedCompletion)
	job2.Name = "job2"
	informer.Batch().V1().Jobs().Informer().GetIndexer().Add(job1)
	informer.Batch().V1().Jobs().Informer().GetIndexer().Add(job2)

	pod1 := newPod("pod1", job1)
	pod2 := newPod("pod2", job2)
	informer.Core().V1().Pods().Informer().GetIndexer().Add(pod1)
	informer.Core().V1().Pods().Informer().GetIndexer().Add(pod2)

	jm.deletePod(logger, pod1, true)
	verifyEmptyQueueAndAwaitForQueueLen(ctx, t, jm, 1)
	key, done := jm.queue.Get()
	if key == "" || done {
		t.Fatalf("failed to enqueue controller for pod %v", pod1.Name)
	}
	expectedKey, _ := controller.KeyFunc(job1)
	if got, want := key, expectedKey; got != want {
		t.Errorf("queue.Get() = %v, want %v", got, want)
	}

	jm.deletePod(logger, pod2, true)
	verifyEmptyQueueAndAwaitForQueueLen(ctx, t, jm, 1)
	key, done = jm.queue.Get()
	if key == "" || done {
		t.Fatalf("failed to enqueue controller for pod %v", pod2.Name)
	}
	expectedKey, _ = controller.KeyFunc(job2)
	if got, want := key, expectedKey; got != want {
		t.Errorf("queue.Get() = %v, want %v", got, want)
	}
}

func TestDeletePodOrphan(t *testing.T) {
	// Disable batching of pod updates to show it does not get requeued at all
	t.Cleanup(setDurationDuringTest(&syncJobBatchPeriod, 0))
	logger, ctx := ktesting.NewTestContext(t)
	clientset := clientset.NewForConfigOrDie(&restclient.Config{Host: "", ContentConfig: restclient.ContentConfig{GroupVersion: &schema.GroupVersion{Group: "", Version: "v1"}}})
	jm, informer := newControllerFromClient(ctx, t, clientset, controller.NoResyncPeriodFunc)
	jm.podStoreSynced = alwaysReady
	jm.jobStoreSynced = alwaysReady

	job1 := newJob(1, 1, 6, batch.NonIndexedCompletion)
	job1.Name = "job1"
	job2 := newJob(1, 1, 6, batch.NonIndexedCompletion)
	job2.Name = "job2"
	job3 := newJob(1, 1, 6, batch.NonIndexedCompletion)
	job3.Name = "job3"
	job3.Spec.Selector.MatchLabels = map[string]string{"other": "labels"}
	informer.Batch().V1().Jobs().Informer().GetIndexer().Add(job1)
	informer.Batch().V1().Jobs().Informer().GetIndexer().Add(job2)
	informer.Batch().V1().Jobs().Informer().GetIndexer().Add(job3)

	pod1 := newPod("pod1", job1)
	pod1.OwnerReferences = nil
	informer.Core().V1().Pods().Informer().GetIndexer().Add(pod1)

	jm.deletePod(logger, pod1, true)
	if got, want := jm.queue.Len(), 0; got != want {
		t.Fatalf("queue.Len() = %v, want %v", got, want)
	}
}

type FakeJobExpectations struct {
	*controller.ControllerExpectations
	satisfied    bool
	expSatisfied func()
}

func (fe FakeJobExpectations) SatisfiedExpectations(logger klog.Logger, controllerKey string) bool {
	fe.expSatisfied()
	return fe.satisfied
}

// TestSyncJobExpectations tests that a pod cannot sneak in between counting active pods
// and checking expectations.
func TestSyncJobExpectations(t *testing.T) {
	_, ctx := ktesting.NewTestContext(t)
	clientset := clientset.NewForConfigOrDie(&restclient.Config{Host: "", ContentConfig: restclient.ContentConfig{GroupVersion: &schema.GroupVersion{Group: "", Version: "v1"}}})
	manager, sharedInformerFactory := newControllerFromClient(ctx, t, clientset, controller.NoResyncPeriodFunc)
	fakePodControl := controller.FakePodControl{}
	manager.podControl = &fakePodControl
	manager.podStoreSynced = alwaysReady
	manager.jobStoreSynced = alwaysReady
	manager.updateStatusHandler = func(ctx context.Context, job *batch.Job) (*batch.Job, error) {
		return job, nil
	}

	job := newJob(2, 2, 6, batch.NonIndexedCompletion)
	sharedInformerFactory.Batch().V1().Jobs().Informer().GetIndexer().Add(job)
	pods := newPodList(2, v1.PodPending, job)
	podIndexer := sharedInformerFactory.Core().V1().Pods().Informer().GetIndexer()
	podIndexer.Add(pods[0])

	manager.expectations = FakeJobExpectations{
		controller.NewControllerExpectations(), true, func() {
			// If we check active pods before checking expectations, the job
			// will create a new replica because it doesn't see this pod, but
			// has fulfilled its expectations.
			podIndexer.Add(pods[1])
		},
	}
	if err := manager.syncJob(ctx, testutil.GetKey(job, t)); err != nil {
		t.Fatalf("error %v while reconciling the job %v", err, testutil.GetKey(job, t))
	}
	if len(fakePodControl.Templates) != 0 {
		t.Errorf("Unexpected number of creates.  Expected %d, saw %d\n", 0, len(fakePodControl.Templates))
	}
	if len(fakePodControl.DeletePodName) != 0 {
		t.Errorf("Unexpected number of deletes.  Expected %d, saw %d\n", 0, len(fakePodControl.DeletePodName))
	}
}

func TestWatchJobs(t *testing.T) {
	_, ctx := ktesting.NewTestContext(t)
	clientset := fake.NewClientset()
	fakeWatch := watch.NewFake()
	clientset.PrependWatchReactor("jobs", core.DefaultWatchReactor(fakeWatch, nil))
	manager, sharedInformerFactory := newControllerFromClient(ctx, t, clientset, controller.NoResyncPeriodFunc)
	manager.podStoreSynced = alwaysReady
	manager.jobStoreSynced = alwaysReady

	var testJob batch.Job
	received := make(chan struct{})

	// The update sent through the fakeWatcher should make its way into the workqueue,
	// and eventually into the syncHandler.
	manager.syncHandler = func(ctx context.Context, key string) error {
		defer close(received)
		ns, name, err := cache.SplitMetaNamespaceKey(key)
		if err != nil {
			t.Errorf("Error getting namespace/name from key %v: %v", key, err)
		}
		job, err := manager.jobLister.Jobs(ns).Get(name)
		if err != nil || job == nil {
			t.Errorf("Expected to find job under key %v: %v", key, err)
			return nil
		}
		if !apiequality.Semantic.DeepDerivative(*job, testJob) {
			t.Errorf("Expected %#v, but got %#v", testJob, *job)
		}
		return nil
	}
	// Start only the job watcher and the workqueue, send a watch event,
	// and make sure it hits the sync method.
	sharedInformerFactory.Start(ctx.Done())
	sharedInformerFactory.WaitForCacheSync(ctx.Done())
	go manager.Run(ctx, 1)

	// We're sending new job to see if it reaches syncHandler.
	testJob.Namespace = "bar"
	testJob.Name = "foo"
	fakeWatch.Add(&testJob)
	t.Log("Waiting for job to reach syncHandler")
	<-received
}

func TestWatchPods(t *testing.T) {
	_, ctx := ktesting.NewTestContext(t)
	testJob := newJob(2, 2, 6, batch.NonIndexedCompletion)
	clientset := fake.NewClientset(testJob)
	fakeWatch := watch.NewFake()
	clientset.PrependWatchReactor("pods", core.DefaultWatchReactor(fakeWatch, nil))
	manager, sharedInformerFactory := newControllerFromClient(ctx, t, clientset, controller.NoResyncPeriodFunc)
	manager.podStoreSynced = alwaysReady
	manager.jobStoreSynced = alwaysReady

	// Put one job and one pod into the store
	sharedInformerFactory.Batch().V1().Jobs().Informer().GetIndexer().Add(testJob)
	received := make(chan struct{})
	// The pod update sent through the fakeWatcher should figure out the managing job and
	// send it into the syncHandler.
	manager.syncHandler = func(ctx context.Context, key string) error {
		ns, name, err := cache.SplitMetaNamespaceKey(key)
		if err != nil {
			t.Errorf("Error getting namespace/name from key %v: %v", key, err)
		}
		job, err := manager.jobLister.Jobs(ns).Get(name)
		if err != nil {
			t.Errorf("Expected to find job under key %v: %v", key, err)
		}
		if !apiequality.Semantic.DeepDerivative(job, testJob) {
			t.Errorf("\nExpected %#v,\nbut got %#v", testJob, job)
			close(received)
			return nil
		}
		close(received)
		return nil
	}
	// Start only the pod watcher and the workqueue, send a watch event,
	// and make sure it hits the sync method for the right job.
	go sharedInformerFactory.Core().V1().Pods().Informer().Run(ctx.Done())
	go manager.Run(ctx, 1)

	pods := newPodList(1, v1.PodRunning, testJob)
	testPod := pods[0]
	testPod.Status.Phase = v1.PodFailed
	fakeWatch.Add(testPod)

	t.Log("Waiting for pod to reach syncHandler")
	<-received
}

func TestWatchOrphanPods(t *testing.T) {
	_, ctx := ktesting.NewTestContext(t)
	ctx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	clientset := fake.NewClientset()
	sharedInformers := informers.NewSharedInformerFactory(clientset, controller.NoResyncPeriodFunc())
	manager, err := NewController(ctx, sharedInformers.Core().V1().Pods(), sharedInformers.Batch().V1().Jobs(), clientset)
	if err != nil {
		t.Fatalf("Error creating Job controller: %v", err)
	}
	manager.podStoreSynced = alwaysReady
	manager.jobStoreSynced = alwaysReady

	podInformer := sharedInformers.Core().V1().Pods().Informer()
	go podInformer.Run(ctx.Done())
	cache.WaitForCacheSync(ctx.Done(), podInformer.HasSynced)
	go manager.Run(ctx, 1)

	// Create job but don't add it to the store.
	cases := map[string]struct {
		job     *batch.Job
		inCache bool
	}{
		"job_does_not_exist": {
			job: newJob(2, 2, 6, batch.NonIndexedCompletion),
		},
		"orphan": {},
		"job_finished": {
			job: func() *batch.Job {
				j := newJob(2, 2, 6, batch.NonIndexedCompletion)
				j.Status.Conditions = append(j.Status.Conditions, batch.JobCondition{
					Type:   batch.JobComplete,
					Status: v1.ConditionTrue,
				})
				return j
			}(),
			inCache: true,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if tc.inCache {
				if err := sharedInformers.Batch().V1().Jobs().Informer().GetIndexer().Add(tc.job); err != nil {
					t.Fatalf("Failed to insert job in index: %v", err)
				}
				t.Cleanup(func() {
					sharedInformers.Batch().V1().Jobs().Informer().GetIndexer().Delete(tc.job)
				})
			}
			_, ctx := ktesting.NewTestContext(t)
			podBuilder := buildPod().name(name).deletionTimestamp().trackingFinalizer()
			if tc.job != nil {
				podBuilder = podBuilder.job(tc.job)
			}
			orphanPod := podBuilder.Pod
			orphanPod, err := clientset.CoreV1().Pods("default").Create(ctx, orphanPod, metav1.CreateOptions{})
			if err != nil {
				t.Fatalf("Creating orphan pod: %v", err)
			}

			if err := wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, wait.ForeverTestTimeout, false, func(ctx context.Context) (bool, error) {
				p, err := clientset.CoreV1().Pods(orphanPod.Namespace).Get(ctx, orphanPod.Name, metav1.GetOptions{})
				if err != nil {
					return false, err
				}
				return !hasJobTrackingFinalizer(p), nil
			}); err != nil {
				t.Errorf("Waiting for Pod to get the finalizer removed: %v", err)
			}
		})
	}
}

func TestSyncOrphanPod(t *testing.T) {
	_, ctx := ktesting.NewTestContext(t)
	clientset := fake.NewClientset()
	sharedInformers := informers.NewSharedInformerFactory(clientset, controller.NoResyncPeriodFunc())
	manager, err := NewController(ctx, sharedInformers.Core().V1().Pods(), sharedInformers.Batch().V1().Jobs(), clientset)
	if err != nil {
		t.Fatalf("Error creating Job controller: %v", err)
	}
	manager.podStoreSynced = alwaysReady
	manager.jobStoreSynced = alwaysReady

	podInformer := sharedInformers.Core().V1().Pods().Informer()
	go podInformer.Run(ctx.Done())
	cache.WaitForCacheSync(ctx.Done(), podInformer.HasSynced)
	go manager.Run(ctx, 1)

	cases := map[string]struct {
		owner                *metav1.OwnerReference
		job                  *batch.Job
		inCache              bool
		wantFinalizerRemoved bool
		podSelector          *metav1.LabelSelector
	}{
		"controlled_by_existing_running_job": {
			owner: &metav1.OwnerReference{
				APIVersion:         "batch/v1",
				Kind:               "Job",
				Name:               "j",
				UID:                "111",
				Controller:         ptr.To(true),
				BlockOwnerDeletion: ptr.To(true),
			},
			job: func() *batch.Job {
				j := newJob(2, 2, 6, batch.NonIndexedCompletion)
				j.UID = "111"
				j.Name = "j"
				return j
			}(),
			inCache:              true,
			wantFinalizerRemoved: false,
		},
		"controlled_by_existing_finished_job": {
			owner: &metav1.OwnerReference{
				APIVersion:         "batch/v1",
				Kind:               "Job",
				Name:               "j",
				UID:                "111",
				Controller:         ptr.To(true),
				BlockOwnerDeletion: ptr.To(true),
			},
			job: func() *batch.Job {
				j := newJob(2, 2, 6, batch.NonIndexedCompletion)
				j.UID = "111"
				j.Name = "j"
				j.Status.Conditions = append(j.Status.Conditions, batch.JobCondition{
					Type:   batch.JobComplete,
					Status: v1.ConditionTrue,
				})
				return j
			}(),
			inCache:              true,
			wantFinalizerRemoved: true,
		},
		"controlled_by_non_existing_job": {
			owner: &metav1.OwnerReference{
				APIVersion:         "batch/v1",
				Kind:               "Job",
				Name:               "j",
				UID:                "111",
				Controller:         ptr.To(true),
				BlockOwnerDeletion: ptr.To(true),
			},
			wantFinalizerRemoved: true,
		},
		"controlled_by_cronjob": {
			owner: &metav1.OwnerReference{
				APIVersion:         "batch/v1",
				Kind:               "CronJob",
				Name:               "cj",
				UID:                "111",
				Controller:         ptr.To(true),
				BlockOwnerDeletion: ptr.To(true),
			},
			wantFinalizerRemoved: false,
		},
		"not_controlled": {
			wantFinalizerRemoved: true,
		},
		"controlled_by_custom_crd": {
			owner: &metav1.OwnerReference{
				APIVersion:         "example/v1",
				Kind:               "Job",
				Name:               "job",
				UID:                "111",
				Controller:         ptr.To(true),
				BlockOwnerDeletion: ptr.To(true),
			},
			wantFinalizerRemoved: false,
		},
		"owned_by_existing_running_job": {
			owner: &metav1.OwnerReference{
				APIVersion: "batch/v1",
				Kind:       "Job",
				Name:       "j",
				UID:        "111",
			},
			job: func() *batch.Job {
				j := newJob(2, 2, 6, batch.NonIndexedCompletion)
				j.UID = "111"
				j.Name = "j"
				return j
			}(),
			inCache:              true,
			wantFinalizerRemoved: true,
		},
		"owned_by_custom_crd": {
			owner: &metav1.OwnerReference{
				APIVersion: "example/v1",
				Kind:       "Job",
				Name:       "job",
				UID:        "111",
			},
			wantFinalizerRemoved: true,
		},
		"orphan_pods_by_label_selector": {
			podSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app": "test",
				},
			},
			wantFinalizerRemoved: true,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if tc.inCache {
				if err := sharedInformers.Batch().V1().Jobs().Informer().GetIndexer().Add(tc.job); err != nil {
					t.Fatalf("Failed to insert job in index: %v", err)
				}
				t.Cleanup(func() {
					if err := sharedInformers.Batch().V1().Jobs().Informer().GetIndexer().Delete(tc.job); err != nil {
						t.Fatalf("Failed to delete job from index: %v", err)
					}
				})
			}
			podBuilder := buildPod().name(name).deletionTimestamp().trackingFinalizer()
			if tc.owner != nil {
				podBuilder = podBuilder.owner(*tc.owner)
			}
			orphanKey := orphanPodKey{
				kind:      OrphanPodKeyKindName,
				namespace: podBuilder.Pod.Namespace,
				value:     podBuilder.Pod.Name,
			}
			if tc.podSelector != nil {
				podBuilder = podBuilder.labels(tc.podSelector.MatchLabels)
				selector, err := metav1.LabelSelectorAsSelector(tc.podSelector)
				if err != nil {
					t.Fatalf("Error parsing pod label selector: %v", err)
				}
				orphanKey = orphanPodKey{
					kind:      OrphanPodKeyKindSelector,
					namespace: podBuilder.Pod.Namespace,
					value:     selector.String(),
				}
			}
			orphanPod, err := clientset.CoreV1().Pods("default").Create(ctx, podBuilder.Pod, metav1.CreateOptions{})
			if err != nil {
				t.Fatalf("Creating orphan pod: %v", err)
			}
			// Sync orphan pod by name or selector
			err = manager.syncOrphanPod(ctx, orphanKey)
			if err != nil {
				t.Fatalf("Failed to sync orphan pod: %v", err)
			}
			if tc.wantFinalizerRemoved {
				if err := wait.PollUntilContextTimeout(ctx, 10*time.Millisecond, wait.ForeverTestTimeout, false, func(ctx context.Context) (bool, error) {
					p, err := clientset.CoreV1().Pods(orphanPod.Namespace).Get(ctx, orphanPod.Name, metav1.GetOptions{})
					if err != nil {
						return false, err
					}
					return !hasJobTrackingFinalizer(p), nil
				}); err != nil {
					t.Errorf("Waiting for the Job's finalizer to be removed: %v", err)
				}
			} else {
				// we sleep a little bit to give potential time for the client's
				// cache to update after the finalizer removal.
				time.Sleep(time.Millisecond)
				orphanPod, err := clientset.CoreV1().Pods(orphanPod.Namespace).Get(ctx, orphanPod.Name, metav1.GetOptions{})
				if err != nil {
					t.Fatalf("Failed to retrieve the latest pod: %v", err)
				}
				if !hasJobTrackingFinalizer(orphanPod) {
					t.Errorf("Unexpected removal of the Job's finalizer")
				}
			}
		})
	}
}

func bumpResourceVersion(obj metav1.Object) {
	ver, _ := strconv.ParseInt(obj.GetResourceVersion(), 10, 32)
	obj.SetResourceVersion(strconv.FormatInt(ver+1, 10))
}

func TestJobApiBackoffReset(t *testing.T) {
	t.Cleanup(setDurationDuringTest(&DefaultJobApiBackOff, fastJobApiBackoff))
	_, ctx := ktesting.NewTestContext(t)

	clientset := clientset.NewForConfigOrDie(&restclient.Config{Host: "", ContentConfig: restclient.ContentConfig{GroupVersion: &schema.GroupVersion{Group: "", Version: "v1"}}})
	fakeClock := clocktesting.NewFakeClock(time.Now())
	manager, sharedInformerFactory := newControllerFromClientWithClock(ctx, t, clientset, controller.NoResyncPeriodFunc, fakeClock)
	fakePodControl := controller.FakePodControl{}
	manager.podControl = &fakePodControl
	manager.podStoreSynced = alwaysReady
	manager.jobStoreSynced = alwaysReady
	manager.updateStatusHandler = func(ctx context.Context, job *batch.Job) (*batch.Job, error) {
		return job, nil
	}

	job := newJob(1, 1, 2, batch.NonIndexedCompletion)
	key := testutil.GetKey(job, t)
	sharedInformerFactory.Batch().V1().Jobs().Informer().GetIndexer().Add(job)

	// error returned make the key requeued
	fakePodControl.Err = errors.New("Controller error")
	manager.queue.Add(key)
	manager.processNextWorkItem(ctx)
	retries := manager.queue.NumRequeues(key)
	if retries != 1 {
		t.Fatalf("%s: expected exactly 1 retry, got %d", job.Name, retries)
	}
	// await for the actual requeue after processing of the pending queue is done
	awaitForQueueLen(ctx, t, manager, 1)

	// the queue is emptied on success
	fakePodControl.Err = nil
	manager.processNextWorkItem(ctx)
	verifyEmptyQueue(t, manager)
}

var _ workqueue.TypedRateLimitingInterface[string] = &fakeRateLimitingQueue{}

type fakeRateLimitingQueue struct {
	workqueue.TypedInterface[string]
	requeues int
	item     string
	duration time.Duration
}

func (f *fakeRateLimitingQueue) AddRateLimited(item string) {}
func (f *fakeRateLimitingQueue) Forget(item string) {
	f.requeues = 0
}
func (f *fakeRateLimitingQueue) NumRequeues(item string) int {
	return f.requeues
}
func (f *fakeRateLimitingQueue) AddAfter(item string, duration time.Duration) {
	f.item = item
	f.duration = duration
}

func TestJobBackoff(t *testing.T) {
	_, ctx := ktesting.NewTestContext(t)
	logger := klog.FromContext(ctx)
	job := newJob(1, 1, 1, batch.NonIndexedCompletion)
	oldPod := newPod(fmt.Sprintf("pod-%v", rand.String(10)), job)
	oldPod.ResourceVersion = "1"
	newPod := oldPod.DeepCopy()
	newPod.ResourceVersion = "2"

	testCases := map[string]struct {
		requeues    int
		oldPodPhase v1.PodPhase
		phase       v1.PodPhase
		wantBackoff time.Duration
	}{
		"failure with pod updates batching": {
			requeues:    0,
			phase:       v1.PodFailed,
			wantBackoff: syncJobBatchPeriod,
		},
	}

	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			clientset := clientset.NewForConfigOrDie(&restclient.Config{Host: "", ContentConfig: restclient.ContentConfig{GroupVersion: &schema.GroupVersion{Group: "", Version: "v1"}}})
			manager, sharedInformerFactory := newControllerFromClient(ctx, t, clientset, controller.NoResyncPeriodFunc)
			fakePodControl := controller.FakePodControl{}
			manager.podControl = &fakePodControl
			manager.podStoreSynced = alwaysReady
			manager.jobStoreSynced = alwaysReady
			queue := &fakeRateLimitingQueue{}
			manager.queue = queue
			sharedInformerFactory.Batch().V1().Jobs().Informer().GetIndexer().Add(job)

			queue.requeues = tc.requeues
			newPod.Status.Phase = tc.phase
			oldPod.Status.Phase = v1.PodRunning
			if tc.oldPodPhase != "" {
				oldPod.Status.Phase = tc.oldPodPhase
			}
			manager.updatePod(logger, oldPod, newPod)
			if queue.duration != tc.wantBackoff {
				t.Errorf("unexpected backoff %v, expected %v", queue.duration, tc.wantBackoff)
			}
		})
	}
}

func TestJobBackoffForOnFailure(t *testing.T) {
	_, ctx := ktesting.NewTestContext(t)

	testCases := map[string]struct {
		// job setup
		parallelism  int32
		completions  int32
		backoffLimit int32
		suspend      bool

		// pod setup
		restartCounts []int32
		podPhase      v1.PodPhase

		// expectations
		expectedActive     int32
		expectedSucceeded  int32
		expectedFailed     int32
		expectedConditions []batch.JobCondition

		// features
		enableJobManagedBy            bool
		enableJobPodReplacementPolicy bool
	}{
		"backoffLimit 0 should have 1 pod active": {
			parallelism:        1,
			completions:        1,
			backoffLimit:       0,
			suspend:            false,
			restartCounts:      []int32{0},
			podPhase:           v1.PodRunning,
			expectedActive:     1,
			expectedSucceeded:  0,
			expectedFailed:     0,
			expectedConditions: nil,
		},
		"backoffLimit 1 with restartCount 0 should have 1 pod active": {
			parallelism:        1,
			completions:        1,
			backoffLimit:       1,
			suspend:            false,
			restartCounts:      []int32{0},
			podPhase:           v1.PodRunning,
			expectedActive:     1,
			expectedSucceeded:  0,
			expectedFailed:     0,
			expectedConditions: nil,
		},
		"backoffLimit 1 with restartCount 1 and podRunning should have 0 pod active": {
			parallelism:       1,
			completions:       1,
			backoffLimit:      1,
			suspend:           false,
			restartCounts:     []int32{1},
			podPhase:          v1.PodRunning,
			expectedActive:    0,
			expectedSucceeded: 0,
			expectedFailed:    1,
			expectedConditions: []batch.JobCondition{
				{
					Type:    batch.JobFailed,
					Status:  v1.ConditionTrue,
					Reason:  batch.JobReasonBackoffLimitExceeded,
					Message: "Job has reached the specified backoff limit",
				},
			},
		},
		"backoffLimit 1 with restartCount 1 and podPending should have 0 pod active": {
			parallelism:       1,
			completions:       1,
			backoffLimit:      1,
			suspend:           false,
			restartCounts:     []int32{1},
			podPhase:          v1.PodPending,
			expectedActive:    0,
			expectedSucceeded: 0,
			expectedFailed:    1,
			expectedConditions: []batch.JobCondition{
				{
					Type:    batch.JobFailed,
					Status:  v1.ConditionTrue,
					Reason:  batch.JobReasonBackoffLimitExceeded,
					Message: "Job has reached the specified backoff limit",
				},
			},
		},
		"too many job failures with podRunning - single pod": {
			parallelism:       1,
			completions:       5,
			backoffLimit:      2,
			suspend:           false,
			restartCounts:     []int32{2},
			podPhase:          v1.PodRunning,
			expectedActive:    0,
			expectedSucceeded: 0,
			expectedFailed:    1,
			expectedConditions: []batch.JobCondition{
				{
					Type:    batch.JobFailed,
					Status:  v1.ConditionTrue,
					Reason:  batch.JobReasonBackoffLimitExceeded,
					Message: "Job has reached the specified backoff limit",
				},
			},
		},
		"too many job failures with podPending - single pod": {
			parallelism:       1,
			completions:       5,
			backoffLimit:      2,
			suspend:           false,
			restartCounts:     []int32{2},
			podPhase:          v1.PodPending,
			expectedActive:    0,
			expectedSucceeded: 0,
			expectedFailed:    1,
			expectedConditions: []batch.JobCondition{
				{
					Type:    batch.JobFailed,
					Status:  v1.ConditionTrue,
					Reason:  batch.JobReasonBackoffLimitExceeded,
					Message: "Job has reached the specified backoff limit",
				},
			},
		},
		"too many job failures with podRunning - multiple pods": {
			parallelism:       2,
			completions:       5,
			backoffLimit:      2,
			suspend:           false,
			restartCounts:     []int32{1, 1},
			podPhase:          v1.PodRunning,
			expectedActive:    0,
			expectedSucceeded: 0,
			expectedFailed:    2,
			expectedConditions: []batch.JobCondition{
				{
					Type:    batch.JobFailed,
					Status:  v1.ConditionTrue,
					Reason:  batch.JobReasonBackoffLimitExceeded,
					Message: "Job has reached the specified backoff limit",
				},
			},
		},
		"too many job failures with podPending - multiple pods": {
			parallelism:       2,
			completions:       5,
			backoffLimit:      2,
			suspend:           false,
			restartCounts:     []int32{1, 1},
			podPhase:          v1.PodPending,
			expectedActive:    0,
			expectedSucceeded: 0,
			expectedFailed:    2,
			expectedConditions: []batch.JobCondition{
				{
					Type:    batch.JobFailed,
					Status:  v1.ConditionTrue,
					Reason:  batch.JobReasonBackoffLimitExceeded,
					Message: "Job has reached the specified backoff limit",
				},
			},
		},
		"not enough failures": {
			parallelism:        2,
			completions:        5,
			backoffLimit:       3,
			suspend:            false,
			restartCounts:      []int32{1, 1},
			podPhase:           v1.PodRunning,
			expectedActive:     2,
			expectedSucceeded:  0,
			expectedFailed:     0,
			expectedConditions: nil,
		},
		"suspending a job": {
			parallelism:       2,
			completions:       4,
			backoffLimit:      6,
			suspend:           true,
			restartCounts:     []int32{1, 1},
			podPhase:          v1.PodRunning,
			expectedActive:    0,
			expectedSucceeded: 0,
			expectedFailed:    0,
			expectedConditions: []batch.JobCondition{
				{
					Type:    batch.JobSuspended,
					Status:  v1.ConditionTrue,
					Reason:  "JobSuspended",
					Message: "Job suspended",
				},
			},
		},
		"finished job": {
			parallelism:       2,
			completions:       4,
			backoffLimit:      6,
			suspend:           true,
			restartCounts:     []int32{1, 1, 2, 0},
			podPhase:          v1.PodSucceeded,
			expectedActive:    0,
			expectedSucceeded: 4,
			expectedFailed:    0,
			expectedConditions: []batch.JobCondition{
				{
					Type:    batch.JobComplete,
					Status:  v1.ConditionTrue,
					Reason:  batch.JobReasonCompletionsReached,
					Message: "Reached expected number of succeeded pods",
				},
			},
		},
		"finished job; JobPodReplacementPolicy enabled": {
			enableJobPodReplacementPolicy: true,

			parallelism:       2,
			completions:       4,
			backoffLimit:      6,
			suspend:           true,
			restartCounts:     []int32{1, 1, 2, 0},
			podPhase:          v1.PodSucceeded,
			expectedActive:    0,
			expectedSucceeded: 4,
			expectedFailed:    0,
			expectedConditions: []batch.JobCondition{
				{
					Type:    batch.JobSuccessCriteriaMet,
					Status:  v1.ConditionTrue,
					Reason:  batch.JobReasonCompletionsReached,
					Message: "Reached expected number of succeeded pods",
				},
				{
					Type:    batch.JobComplete,
					Status:  v1.ConditionTrue,
					Reason:  batch.JobReasonCompletionsReached,
					Message: "Reached expected number of succeeded pods",
				},
			},
		},
		"finished job; JobManagedBy enabled": {
			enableJobManagedBy: true,

			parallelism:       2,
			completions:       4,
			backoffLimit:      6,
			suspend:           true,
			restartCounts:     []int32{1, 1, 2, 0},
			podPhase:          v1.PodSucceeded,
			expectedActive:    0,
			expectedSucceeded: 4,
			expectedFailed:    0,
			expectedConditions: []batch.JobCondition{
				{
					Type:    batch.JobSuccessCriteriaMet,
					Status:  v1.ConditionTrue,
					Reason:  batch.JobReasonCompletionsReached,
					Message: "Reached expected number of succeeded pods",
				},
				{
					Type:    batch.JobComplete,
					Status:  v1.ConditionTrue,
					Reason:  batch.JobReasonCompletionsReached,
					Message: "Reached expected number of succeeded pods",
				},
			},
		},
		"too many job failures with podRunning - multiple pods; JobPodReplacementPolicy enabled": {
			enableJobPodReplacementPolicy: true,

			parallelism:       2,
			completions:       5,
			backoffLimit:      2,
			suspend:           false,
			restartCounts:     []int32{1, 1},
			podPhase:          v1.PodRunning,
			expectedActive:    0,
			expectedSucceeded: 0,
			expectedFailed:    2,
			expectedConditions: []batch.JobCondition{
				{
					Type:    batch.JobFailureTarget,
					Status:  v1.ConditionTrue,
					Reason:  batch.JobReasonBackoffLimitExceeded,
					Message: "Job has reached the specified backoff limit",
				},
			},
		},
		"too many job failures with podRunning - multiple pods; JobManagedBy enabled": {
			enableJobManagedBy: true,

			parallelism:       2,
			completions:       5,
			backoffLimit:      2,
			suspend:           false,
			restartCounts:     []int32{1, 1},
			podPhase:          v1.PodRunning,
			expectedActive:    0,
			expectedSucceeded: 0,
			expectedFailed:    2,
			expectedConditions: []batch.JobCondition{
				{
					Type:    batch.JobFailureTarget,
					Status:  v1.ConditionTrue,
					Reason:  batch.JobReasonBackoffLimitExceeded,
					Message: "Job has reached the specified backoff limit",
				},
			},
		},
	}

	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			featuregatetesting.SetFeatureGateDuringTest(t, feature.DefaultFeatureGate, features.JobPodReplacementPolicy, tc.enableJobPodReplacementPolicy)
			featuregatetesting.SetFeatureGateDuringTest(t, feature.DefaultFeatureGate, features.JobManagedBy, tc.enableJobManagedBy)
			// job manager setup
			clientset := clientset.NewForConfigOrDie(&restclient.Config{Host: "", ContentConfig: restclient.ContentConfig{GroupVersion: &schema.GroupVersion{Group: "", Version: "v1"}}})
			manager, sharedInformerFactory := newControllerFromClient(ctx, t, clientset, controller.NoResyncPeriodFunc)
			fakePodControl := controller.FakePodControl{}
			manager.podControl = &fakePodControl
			manager.podStoreSynced = alwaysReady
			manager.jobStoreSynced = alwaysReady
			var actual *batch.Job
			manager.updateStatusHandler = func(ctx context.Context, job *batch.Job) (*batch.Job, error) {
				actual = job
				return job, nil
			}

			// job & pods setup
			job := newJob(tc.parallelism, tc.completions, tc.backoffLimit, batch.NonIndexedCompletion)
			job.Spec.Template.Spec.RestartPolicy = v1.RestartPolicyOnFailure
			job.Spec.Suspend = ptr.To(tc.suspend)
			sharedInformerFactory.Batch().V1().Jobs().Informer().GetIndexer().Add(job)
			podIndexer := sharedInformerFactory.Core().V1().Pods().Informer().GetIndexer()
			for i, pod := range newPodList(len(tc.restartCounts), tc.podPhase, job) {
				pod.Status.ContainerStatuses = []v1.ContainerStatus{{RestartCount: tc.restartCounts[i]}}
				podIndexer.Add(pod)
			}

			// run
			if err := manager.syncJob(ctx, testutil.GetKey(job, t)); err != nil {
				t.Errorf("unexpected error syncing job.  Got %#v", err)
			}
			// validate status
			if actual.Status.Active != tc.expectedActive {
				t.Errorf("unexpected number of active pods.  Expected %d, saw %d\n", tc.expectedActive, actual.Status.Active)
			}
			if actual.Status.Succeeded != tc.expectedSucceeded {
				t.Errorf("unexpected number of succeeded pods.  Expected %d, saw %d\n", tc.expectedSucceeded, actual.Status.Succeeded)
			}
			if actual.Status.Failed != tc.expectedFailed {
				t.Errorf("unexpected number of failed pods.  Expected %d, saw %d\n", tc.expectedFailed, actual.Status.Failed)
			}
			// validate conditions
			if diff := cmp.Diff(tc.expectedConditions, actual.Status.Conditions, cmpopts.IgnoreFields(batch.JobCondition{}, "LastProbeTime", "LastTransitionTime")); diff != "" {
				t.Errorf("unexpected conditions (-want,+got):\n%s", diff)
			}
		})
	}
}

func TestJobBackoffOnRestartPolicyNever(t *testing.T) {
	_, ctx := ktesting.NewTestContext(t)

	testCases := map[string]struct {
		// job setup
		parallelism  int32
		completions  int32
		backoffLimit int32

		// pod setup
		activePodsPhase v1.PodPhase
		activePods      int
		failedPods      int

		// expectations
		expectedActive     int32
		expectedSucceeded  int32
		expectedFailed     int32
		expectedConditions []batch.JobCondition
	}{
		"not enough failures with backoffLimit 0 - single pod": {
			parallelism:        1,
			completions:        1,
			backoffLimit:       0,
			activePodsPhase:    v1.PodRunning,
			activePods:         1,
			failedPods:         0,
			expectedActive:     1,
			expectedSucceeded:  0,
			expectedFailed:     0,
			expectedConditions: nil,
		},
		"not enough failures with backoffLimit 1 - single pod": {
			parallelism:        1,
			completions:        1,
			backoffLimit:       1,
			activePodsPhase:    "",
			activePods:         0,
			failedPods:         1,
			expectedActive:     1,
			expectedSucceeded:  0,
			expectedFailed:     1,
			expectedConditions: nil,
		},
		"too many failures with backoffLimit 1 - single pod": {
			parallelism:       1,
			completions:       1,
			backoffLimit:      1,
			activePodsPhase:   "",
			activePods:        0,
			failedPods:        2,
			expectedActive:    0,
			expectedSucceeded: 0,
			expectedFailed:    2,
			expectedConditions: []batch.JobCondition{
				{
					Type:    batch.JobFailureTarget,
					Status:  v1.ConditionTrue,
					Reason:  batch.JobReasonBackoffLimitExceeded,
					Message: "Job has reached the specified backoff limit",
				},
				{
					Type:    batch.JobFailed,
					Status:  v1.ConditionTrue,
					Reason:  batch.JobReasonBackoffLimitExceeded,
					Message: "Job has reached the specified backoff limit",
				},
			},
		},
		"not enough failures with backoffLimit 6 - multiple pods": {
			parallelism:        2,
			completions:        2,
			backoffLimit:       6,
			activePodsPhase:    v1.PodRunning,
			activePods:         1,
			failedPods:         6,
			expectedActive:     2,
			expectedSucceeded:  0,
			expectedFailed:     6,
			expectedConditions: nil,
		},
		"too many failures with backoffLimit 6 - multiple pods": {
			parallelism:       2,
			completions:       2,
			backoffLimit:      6,
			activePodsPhase:   "",
			activePods:        0,
			failedPods:        7,
			expectedActive:    0,
			expectedSucceeded: 0,
			expectedFailed:    7,
			expectedConditions: []batch.JobCondition{
				{
					Type:    batch.JobFailureTarget,
					Status:  v1.ConditionTrue,
					Reason:  batch.JobReasonBackoffLimitExceeded,
					Message: "Job has reached the specified backoff limit",
				},
				{
					Type:    batch.JobFailed,
					Status:  v1.ConditionTrue,
					Reason:  batch.JobReasonBackoffLimitExceeded,
					Message: "Job has reached the specified backoff limit",
				},
			},
		},
	}

	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			// job manager setup
			clientset := clientset.NewForConfigOrDie(&restclient.Config{Host: "", ContentConfig: restclient.ContentConfig{GroupVersion: &schema.GroupVersion{Group: "", Version: "v1"}}})
			manager, sharedInformerFactory := newControllerFromClient(ctx, t, clientset, controller.NoResyncPeriodFunc)
			fakePodControl := controller.FakePodControl{}
			manager.podControl = &fakePodControl
			manager.podStoreSynced = alwaysReady
			manager.jobStoreSynced = alwaysReady
			var actual *batch.Job
			manager.updateStatusHandler = func(ctx context.Context, job *batch.Job) (*batch.Job, error) {
				actual = job
				return job, nil
			}

			// job & pods setup
			job := newJob(tc.parallelism, tc.completions, tc.backoffLimit, batch.NonIndexedCompletion)
			job.Spec.Template.Spec.RestartPolicy = v1.RestartPolicyNever
			sharedInformerFactory.Batch().V1().Jobs().Informer().GetIndexer().Add(job)
			podIndexer := sharedInformerFactory.Core().V1().Pods().Informer().GetIndexer()
			for _, pod := range newPodList(tc.failedPods, v1.PodFailed, job) {
				pod.Status.ContainerStatuses = []v1.ContainerStatus{{State: v1.ContainerState{Terminated: &v1.ContainerStateTerminated{
					FinishedAt: testFinishedAt,
				}}}}
				podIndexer.Add(pod)
			}
			for _, pod := range newPodList(tc.activePods, tc.activePodsPhase, job) {
				podIndexer.Add(pod)
			}

			// run
			err := manager.syncJob(ctx, testutil.GetKey(job, t))
			if err != nil {
				t.Fatalf("unexpected error syncing job: %#v\n", err)
			}
			// validate status
			if actual.Status.Active != tc.expectedActive {
				t.Errorf("unexpected number of active pods. Expected %d, saw %d\n", tc.expectedActive, actual.Status.Active)
			}
			if actual.Status.Succeeded != tc.expectedSucceeded {
				t.Errorf("unexpected number of succeeded pods. Expected %d, saw %d\n", tc.expectedSucceeded, actual.Status.Succeeded)
			}
			if actual.Status.Failed != tc.expectedFailed {
				t.Errorf("unexpected number of failed pods. Expected %d, saw %d\n", tc.expectedFailed, actual.Status.Failed)
			}
			// validate conditions
			if diff := cmp.Diff(tc.expectedConditions, actual.Status.Conditions, cmpopts.IgnoreFields(batch.JobCondition{}, "LastProbeTime", "LastTransitionTime")); diff != "" {
				t.Errorf("unexpected conditions (-want,+got):\n%s", diff)
			}
		})
	}
}

func TestEnsureJobConditions(t *testing.T) {
	testCases := []struct {
		name         string
		haveList     []batch.JobCondition
		wantType     batch.JobConditionType
		wantStatus   v1.ConditionStatus
		wantReason   string
		expectList   []batch.JobCondition
		expectUpdate bool
	}{
		{
			name:         "append true condition",
			haveList:     []batch.JobCondition{},
			wantType:     batch.JobSuspended,
			wantStatus:   v1.ConditionTrue,
			wantReason:   "foo",
			expectList:   []batch.JobCondition{*newCondition(batch.JobSuspended, v1.ConditionTrue, "foo", "", realClock.Now())},
			expectUpdate: true,
		},
		{
			name:         "append false condition",
			haveList:     []batch.JobCondition{},
			wantType:     batch.JobSuspended,
			wantStatus:   v1.ConditionFalse,
			wantReason:   "foo",
			expectList:   []batch.JobCondition{},
			expectUpdate: false,
		},
		{
			name:         "update true condition reason",
			haveList:     []batch.JobCondition{*newCondition(batch.JobSuspended, v1.ConditionTrue, "foo", "", realClock.Now())},
			wantType:     batch.JobSuspended,
			wantStatus:   v1.ConditionTrue,
			wantReason:   "bar",
			expectList:   []batch.JobCondition{*newCondition(batch.JobSuspended, v1.ConditionTrue, "bar", "", realClock.Now())},
			expectUpdate: true,
		},
		{
			name:         "update true condition status",
			haveList:     []batch.JobCondition{*newCondition(batch.JobSuspended, v1.ConditionTrue, "foo", "", realClock.Now())},
			wantType:     batch.JobSuspended,
			wantStatus:   v1.ConditionFalse,
			wantReason:   "foo",
			expectList:   []batch.JobCondition{*newCondition(batch.JobSuspended, v1.ConditionFalse, "foo", "", realClock.Now())},
			expectUpdate: true,
		},
		{
			name:         "update false condition status",
			haveList:     []batch.JobCondition{*newCondition(batch.JobSuspended, v1.ConditionFalse, "foo", "", realClock.Now())},
			wantType:     batch.JobSuspended,
			wantStatus:   v1.ConditionTrue,
			wantReason:   "foo",
			expectList:   []batch.JobCondition{*newCondition(batch.JobSuspended, v1.ConditionTrue, "foo", "", realClock.Now())},
			expectUpdate: true,
		},
		{
			name:         "condition already exists",
			haveList:     []batch.JobCondition{*newCondition(batch.JobSuspended, v1.ConditionTrue, "foo", "", realClock.Now())},
			wantType:     batch.JobSuspended,
			wantStatus:   v1.ConditionTrue,
			wantReason:   "foo",
			expectList:   []batch.JobCondition{*newCondition(batch.JobSuspended, v1.ConditionTrue, "foo", "", realClock.Now())},
			expectUpdate: false,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			gotList, isUpdated := ensureJobConditionStatus(tc.haveList, tc.wantType, tc.wantStatus, tc.wantReason, "", realClock.Now())
			if isUpdated != tc.expectUpdate {
				t.Errorf("Got isUpdated=%v, want %v", isUpdated, tc.expectUpdate)
			}
			if len(gotList) != len(tc.expectList) {
				t.Errorf("got a list of length %d, want %d", len(gotList), len(tc.expectList))
			}
			if diff := cmp.Diff(tc.expectList, gotList, cmpopts.IgnoreFields(batch.JobCondition{}, "LastProbeTime", "LastTransitionTime")); diff != "" {
				t.Errorf("Unexpected JobCondition list: (-want,+got):\n%s", diff)
			}
		})
	}
}

func TestFinalizersRemovedExpectations(t *testing.T) {
	_, ctx := ktesting.NewTestContext(t)
	clientset := fake.NewClientset()
	sharedInformers := informers.NewSharedInformerFactory(clientset, controller.NoResyncPeriodFunc())
	manager, err := NewController(ctx, sharedInformers.Core().V1().Pods(), sharedInformers.Batch().V1().Jobs(), clientset)
	if err != nil {
		t.Fatalf("Error creating Job controller: %v", err)
	}
	manager.podStoreSynced = alwaysReady
	manager.jobStoreSynced = alwaysReady
	manager.podControl = &controller.FakePodControl{Err: errors.New("fake pod controller error")}
	manager.updateStatusHandler = func(ctx context.Context, job *batch.Job) (*batch.Job, error) {
		return job, nil
	}

	job := newJob(2, 2, 6, batch.NonIndexedCompletion)
	sharedInformers.Batch().V1().Jobs().Informer().GetIndexer().Add(job)
	pods := append(newPodList(2, v1.PodSucceeded, job), newPodList(2, v1.PodFailed, job)...)
	podInformer := sharedInformers.Core().V1().Pods().Informer()
	podIndexer := podInformer.GetIndexer()
	uids := sets.New[types.UID]()
	for i := range pods {
		clientset.Tracker().Add(pods[i])
		podIndexer.Add(pods[i])
		uids.Insert(pods[i].UID)
	}
	jobKey := testutil.GetKey(job, t)

	if err := manager.syncJob(ctx, jobKey); err == nil {
		t.Fatal("missing error as the podControl is mocked to error")
	}
	gotExpectedUIDs := manager.finalizerExpectations.getExpectedUIDs(jobKey)
	if len(gotExpectedUIDs) != 0 {
		t.Errorf("Got unwanted expectations for removed finalizers after first syncJob with client failures:\n%s", sets.List(gotExpectedUIDs))
	}

	// Remove failures and re-sync.
	manager.podControl.(*controller.FakePodControl).Err = nil
	if err := manager.syncJob(ctx, jobKey); err != nil {
		t.Fatalf("unexpected error syncing job: %#v\n", err)
	}
	gotExpectedUIDs = manager.finalizerExpectations.getExpectedUIDs(jobKey)
	if diff := cmp.Diff(uids, gotExpectedUIDs); diff != "" {
		t.Errorf("Different expectations for removed finalizers after syncJob (-want,+got):\n%s", diff)
	}

	go sharedInformers.Core().V1().Pods().Informer().Run(ctx.Done())
	cache.WaitForCacheSync(ctx.Done(), podInformer.HasSynced)

	// Make sure the first syncJob sets the expectations, even after the caches synced.
	gotExpectedUIDs = manager.finalizerExpectations.getExpectedUIDs(jobKey)
	if diff := cmp.Diff(uids, gotExpectedUIDs); diff != "" {
		t.Errorf("Different expectations for removed finalizers after syncJob and cacheSync (-want,+got):\n%s", diff)
	}

	// Change pods in different ways.

	podsResource := schema.GroupVersionResource{Version: "v1", Resource: "pods"}

	update := pods[0].DeepCopy()
	update.Finalizers = nil
	update.ResourceVersion = "1"
	err = clientset.Tracker().Update(podsResource, update, update.Namespace)
	if err != nil {
		t.Errorf("Removing finalizer: %v", err)
	}

	update = pods[1].DeepCopy()
	update.Finalizers = nil
	update.DeletionTimestamp = &metav1.Time{Time: time.Now()}
	update.ResourceVersion = "1"
	err = clientset.Tracker().Update(podsResource, update, update.Namespace)
	if err != nil {
		t.Errorf("Removing finalizer and setting deletion timestamp: %v", err)
	}

	// Preserve the finalizer.
	update = pods[2].DeepCopy()
	update.DeletionTimestamp = &metav1.Time{Time: time.Now()}
	update.ResourceVersion = "1"
	err = clientset.Tracker().Update(podsResource, update, update.Namespace)
	if err != nil {
		t.Errorf("Setting deletion timestamp: %v", err)
	}

	err = clientset.Tracker().Delete(podsResource, pods[3].Namespace, pods[3].Name)
	if err != nil {
		t.Errorf("Deleting pod that had finalizer: %v", err)
	}

	uids = sets.New(pods[2].UID)
	var diff string
	if err := wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, wait.ForeverTestTimeout, false, func(ctx context.Context) (bool, error) {
		gotExpectedUIDs = manager.finalizerExpectations.getExpectedUIDs(jobKey)
		diff = cmp.Diff(uids, gotExpectedUIDs)
		return diff == "", nil
	}); err != nil {
		t.Errorf("Timeout waiting for expectations (-want, +got):\n%s", diff)
	}
}

func TestFinalizerCleanup(t *testing.T) {
	_, ctx := ktesting.NewTestContext(t)
	ctx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)

	clientset := fake.NewClientset()
	sharedInformers := informers.NewSharedInformerFactory(clientset, controller.NoResyncPeriodFunc())
	manager, err := NewController(ctx, sharedInformers.Core().V1().Pods(), sharedInformers.Batch().V1().Jobs(), clientset)
	if err != nil {
		t.Fatalf("Error creating Job controller: %v", err)
	}
	manager.podStoreSynced = alwaysReady
	manager.jobStoreSynced = alwaysReady

	// Start the Pod and Job informers.
	sharedInformers.Start(ctx.Done())
	sharedInformers.WaitForCacheSync(ctx.Done())
	// Initialize the controller with 1 worker to make sure the
	// pod finalizers are removed by the "syncJob" function.
	go manager.Run(ctx, 1)
	// Make sure the pod finalizers are removed by the "orphanWorker" function.
	go wait.UntilWithContext(ctx, manager.orphanWorker, time.Second)

	// Create a simple Job
	job := newJob(1, 1, 1, batch.NonIndexedCompletion)
	job, err = clientset.BatchV1().Jobs(job.GetNamespace()).Create(ctx, job, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("Creating job: %v", err)
	}

	// Await for the Job to appear in the jobLister to ensure so that Job Pod gets tracked correctly.
	if err := wait.PollUntilContextTimeout(ctx, 10*time.Millisecond, wait.ForeverTestTimeout, false, func(ctx context.Context) (bool, error) {
		job, _ := manager.jobLister.Jobs(job.GetNamespace()).Get(job.Name)
		return job != nil, nil
	}); err != nil {
		t.Fatalf("Waiting for Job object to appear in jobLister: %v", err)
	}

	selector, err := metav1.LabelSelectorAsSelector(job.Spec.Selector)
	if err != nil {
		t.Fatalf("Error parsing job selector: %v", err)
	}

	var pods []*v1.Pod
	// Wait for the Pod to be created by the Job controller
	if err := wait.PollUntilContextTimeout(ctx, 10*time.Millisecond, wait.ForeverTestTimeout, false, func(ctx context.Context) (bool, error) {
		pods, err = manager.podStore.Pods(job.Namespace).List(selector)
		if err != nil {
			return false, err
		}
		if len(pods) > 0 {
			return true, nil
		}
		return false, nil
	}); err != nil {
		t.Fatalf("Waiting for Pod to be created by Job: %v", err)
	}

	// Mark Job as complete.
	job.Status.Conditions = append(job.Status.Conditions, batch.JobCondition{
		Type:   batch.JobComplete,
		Status: v1.ConditionTrue,
	})
	_, err = clientset.BatchV1().Jobs(job.GetNamespace()).UpdateStatus(ctx, job, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("Updating job status: %v", err)
	}

	// Verify the pod finalizer is removed for a finished Job
	if err := wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, wait.ForeverTestTimeout, true, func(ctx context.Context) (bool, error) {
		p, err := clientset.CoreV1().Pods(pods[0].Namespace).Get(ctx, pods[0].Name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		return !hasJobTrackingFinalizer(p), nil
	}); err != nil {
		t.Errorf("Waiting for Pod to get the finalizer removed: %v", err)
	}

}

func checkJobCompletionLabel(t *testing.T, p *v1.PodTemplateSpec) {
	t.Helper()
	labels := p.GetLabels()
	if labels == nil || labels[batch.JobCompletionIndexAnnotation] == "" {
		t.Errorf("missing expected pod label %s", batch.JobCompletionIndexAnnotation)
	}
}

func checkJobCompletionEnvVariable(t *testing.T, spec *v1.PodSpec, podIndexLabelDisabled bool) {
	t.Helper()
	var fieldPath string
	if podIndexLabelDisabled {
		fieldPath = fmt.Sprintf("metadata.annotations['%s']", batch.JobCompletionIndexAnnotation)
	} else {
		fieldPath = fmt.Sprintf("metadata.labels['%s']", batch.JobCompletionIndexAnnotation)
	}
	want := []v1.EnvVar{
		{
			Name: "JOB_COMPLETION_INDEX",
			ValueFrom: &v1.EnvVarSource{
				FieldRef: &v1.ObjectFieldSelector{
					FieldPath: fieldPath,
				},
			},
		},
	}
	for _, c := range spec.InitContainers {
		if diff := cmp.Diff(want, c.Env); diff != "" {
			t.Errorf("Unexpected Env in container %s (-want,+got):\n%s", c.Name, diff)
		}
	}
	for _, c := range spec.Containers {
		if diff := cmp.Diff(want, c.Env); diff != "" {
			t.Errorf("Unexpected Env in container %s (-want,+got):\n%s", c.Name, diff)
		}
	}
}

func podReplacementPolicy(m batch.PodReplacementPolicy) *batch.PodReplacementPolicy {
	return &m
}

func verifyEmptyQueueAndAwaitForQueueLen(ctx context.Context, t *testing.T, jm *Controller, wantQueueLen int) {
	t.Helper()
	verifyEmptyQueue(t, jm)
	awaitForQueueLen(ctx, t, jm, wantQueueLen)
}

func awaitForQueueLen(ctx context.Context, t *testing.T, jm *Controller, wantQueueLen int) {
	t.Helper()
	verifyEmptyQueue(t, jm)
	if err := wait.PollUntilContextTimeout(ctx, fastRequeue, time.Second, true, func(ctx context.Context) (bool, error) {
		if requeued := jm.queue.Len() == wantQueueLen; requeued {
			return true, nil
		}
		jm.clock.Sleep(fastRequeue)
		return false, nil
	}); err != nil {
		t.Errorf("Failed to await for expected queue.Len(). want %v, got: %v", wantQueueLen, jm.queue.Len())
	}
}

func verifyEmptyQueue(t *testing.T, jm *Controller) {
	t.Helper()
	if jm.queue.Len() > 0 {
		t.Errorf("Unexpected queue.Len(). Want: %d, got: %d", 0, jm.queue.Len())
	}
}

type podBuilder struct {
	*v1.Pod
}

func buildPod() podBuilder {
	return podBuilder{Pod: &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			UID: types.UID(rand.String(5)),
		},
	}}
}

func getConditionsByType(list []batch.JobCondition, cType batch.JobConditionType) []*batch.JobCondition {
	var result []*batch.JobCondition
	for i := range list {
		if list[i].Type == cType {
			result = append(result, &list[i])
		}
	}
	return result
}

func (pb podBuilder) name(n string) podBuilder {
	pb.Name = n
	return pb
}

func (pb podBuilder) ns(n string) podBuilder {
	pb.Namespace = n
	return pb
}

func (pb podBuilder) uid(u string) podBuilder {
	pb.UID = types.UID(u)
	return pb
}

func (pb podBuilder) labels(labels map[string]string) podBuilder {
	pb.Labels = labels
	return pb
}

func (pb podBuilder) job(j *batch.Job) podBuilder {
	pb.Labels = j.Spec.Selector.MatchLabels
	pb.Namespace = j.Namespace
	pb.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(j, controllerKind)}
	return pb
}

func (pb podBuilder) owner(ownerRef metav1.OwnerReference) podBuilder {
	pb.OwnerReferences = append(pb.OwnerReferences, ownerRef)
	return pb
}

func (pb podBuilder) clearOwner() podBuilder {
	pb.OwnerReferences = nil
	return pb
}

func (pb podBuilder) clearLabels() podBuilder {
	pb.Labels = nil
	return pb
}

func (pb podBuilder) index(ix string) podBuilder {
	return pb.annotation(batch.JobCompletionIndexAnnotation, ix)
}

func (pb podBuilder) indexFailureCount(count string) podBuilder {
	return pb.annotation(batch.JobIndexFailureCountAnnotation, count)
}

func (pb podBuilder) indexIgnoredFailureCount(count string) podBuilder {
	return pb.annotation(batch.JobIndexIgnoredFailureCountAnnotation, count)
}

func (pb podBuilder) annotation(key, value string) podBuilder {
	if pb.Annotations == nil {
		pb.Annotations = make(map[string]string)
	}
	pb.Annotations[key] = value
	return pb
}

func (pb podBuilder) status(s v1.PodStatus) podBuilder {
	pb.Status = s
	return pb
}

func (pb podBuilder) phase(p v1.PodPhase) podBuilder {
	pb.Status.Phase = p
	return pb
}

func (pb podBuilder) trackingFinalizer() podBuilder {
	for _, f := range pb.Finalizers {
		if f == batch.JobTrackingFinalizer {
			return pb
		}
	}
	pb.Finalizers = append(pb.Finalizers, batch.JobTrackingFinalizer)
	return pb
}

func (pb podBuilder) deletionTimestamp() podBuilder {
	pb.DeletionTimestamp = &metav1.Time{}
	return pb
}

func (pb podBuilder) customDeletionTimestamp(t time.Time) podBuilder {
	pb.DeletionTimestamp = &metav1.Time{Time: t}
	return pb
}

func setDurationDuringTest(val *time.Duration, newVal time.Duration) func() {
	origVal := *val
	*val = newVal
	return func() {
		*val = origVal
	}
}
