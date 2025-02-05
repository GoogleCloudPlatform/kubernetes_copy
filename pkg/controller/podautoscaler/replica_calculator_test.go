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

package podautoscaler

import (
	"context"
	"fmt"
	"math"
	"testing"
	"time"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta/testrestmapper"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	core "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
	"k8s.io/kubernetes/pkg/api/legacyscheme"
	"k8s.io/kubernetes/pkg/controller"
	metricsclient "k8s.io/kubernetes/pkg/controller/podautoscaler/metrics"
	cmapi "k8s.io/metrics/pkg/apis/custom_metrics/v1beta2"
	emapi "k8s.io/metrics/pkg/apis/external_metrics/v1beta1"
	metricsapi "k8s.io/metrics/pkg/apis/metrics/v1beta1"
	metricsfake "k8s.io/metrics/pkg/client/clientset/versioned/fake"
	cmfake "k8s.io/metrics/pkg/client/custom_metrics/fake"
	emfake "k8s.io/metrics/pkg/client/external_metrics/fake"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type resourceInfo struct {
	name     v1.ResourceName
	requests []resource.Quantity
	levels   [][]int64
	// only applies to pod names returned from "heapster"
	podNames []string

	targetUtilization   int32
	expectedUtilization int32
	expectedValue       int64
}

type metricType int

const (
	objectMetric metricType = iota
	objectPerPodMetric
	externalMetric
	externalPerPodMetric
	podMetric
)

type metricInfo struct {
	name         string
	levels       []int64
	singleObject *autoscalingv2.CrossVersionObjectReference
	selector     *metav1.LabelSelector
	metricType   metricType

	targetUsage       int64
	perPodTargetUsage int64
	expectedUsage     int64
}

type replicaCalcTestCase struct {
	currentReplicas  int32
	expectedReplicas int32
	expectedError    error

	timestamp time.Time

	resource  *resourceInfo
	metric    *metricInfo
	container string

	podReadiness         []v1.ConditionStatus
	podStartTime         []metav1.Time
	podPhase             []v1.PodPhase
	podDeletionTimestamp []bool
}

const (
	testNamespace       = "test-namespace"
	podNamePrefix       = "test-pod"
	numContainersPerPod = 2
)

func (tc *replicaCalcTestCase) prepareTestClientSet() *fake.Clientset {
	fakeClient := &fake.Clientset{}
	fakeClient.AddReactor("list", "pods", func(action core.Action) (handled bool, ret runtime.Object, err error) {
		obj := &v1.PodList{}
		podsCount := int(tc.currentReplicas)
		// Failed pods are not included in tc.currentReplicas
		if tc.podPhase != nil && len(tc.podPhase) > podsCount {
			podsCount = len(tc.podPhase)
		}
		for i := 0; i < podsCount; i++ {
			podReadiness := v1.ConditionTrue
			if tc.podReadiness != nil && i < len(tc.podReadiness) {
				podReadiness = tc.podReadiness[i]
			}
			var podStartTime metav1.Time
			if tc.podStartTime != nil {
				podStartTime = tc.podStartTime[i]
			}
			podPhase := v1.PodRunning
			if tc.podPhase != nil {
				podPhase = tc.podPhase[i]
			}
			podDeletionTimestamp := false
			if tc.podDeletionTimestamp != nil {
				podDeletionTimestamp = tc.podDeletionTimestamp[i]
			}
			podName := fmt.Sprintf("%s-%d", podNamePrefix, i)
			pod := v1.Pod{
				Status: v1.PodStatus{
					Phase:     podPhase,
					StartTime: &podStartTime,
					Conditions: []v1.PodCondition{
						{
							Type:   v1.PodReady,
							Status: podReadiness,
						},
					},
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:      podName,
					Namespace: testNamespace,
					Labels: map[string]string{
						"name": podNamePrefix,
					},
				},
				Spec: v1.PodSpec{
					Containers: []v1.Container{{Name: "container1"}, {Name: "container2"}},
				},
			}
			if podDeletionTimestamp {
				pod.DeletionTimestamp = &metav1.Time{Time: time.Now()}
			}

			if tc.resource != nil && i < len(tc.resource.requests) {
				pod.Spec.Containers[0].Resources = v1.ResourceRequirements{
					Requests: v1.ResourceList{
						tc.resource.name: tc.resource.requests[i],
					},
				}
				pod.Spec.Containers[1].Resources = v1.ResourceRequirements{
					Requests: v1.ResourceList{
						tc.resource.name: tc.resource.requests[i],
					},
				}
			}
			obj.Items = append(obj.Items, pod)
		}
		return true, obj, nil
	})
	return fakeClient
}

func (tc *replicaCalcTestCase) prepareTestMetricsClient() *metricsfake.Clientset {
	fakeMetricsClient := &metricsfake.Clientset{}
	// NB: we have to sound like Gollum due to gengo's inability to handle already-plural resource names
	fakeMetricsClient.AddReactor("list", "pods", func(action core.Action) (handled bool, ret runtime.Object, err error) {
		if tc.resource != nil {
			metrics := &metricsapi.PodMetricsList{}
			for i, resValue := range tc.resource.levels {
				podName := fmt.Sprintf("%s-%d", podNamePrefix, i)
				if len(tc.resource.podNames) > i {
					podName = tc.resource.podNames[i]
				}
				// NB: the list reactor actually does label selector filtering for us,
				// so we have to make sure our results match the label selector
				podMetric := metricsapi.PodMetrics{
					ObjectMeta: metav1.ObjectMeta{
						Name:      podName,
						Namespace: testNamespace,
						Labels:    map[string]string{"name": podNamePrefix},
					},
					Timestamp:  metav1.Time{Time: tc.timestamp},
					Window:     metav1.Duration{Duration: time.Minute},
					Containers: make([]metricsapi.ContainerMetrics, numContainersPerPod),
				}

				for i, m := range resValue {
					podMetric.Containers[i] = metricsapi.ContainerMetrics{
						Name: fmt.Sprintf("container%v", i+1),
						Usage: v1.ResourceList{
							tc.resource.name: *resource.NewMilliQuantity(m, resource.DecimalSI),
						},
					}
				}
				metrics.Items = append(metrics.Items, podMetric)
			}
			return true, metrics, nil
		}

		return true, nil, fmt.Errorf("no pod resource metrics specified in test client")
	})
	return fakeMetricsClient
}

func (tc *replicaCalcTestCase) prepareTestCMClient(t *testing.T) *cmfake.FakeCustomMetricsClient {
	fakeCMClient := &cmfake.FakeCustomMetricsClient{}
	fakeCMClient.AddReactor("get", "*", func(action core.Action) (handled bool, ret runtime.Object, err error) {
		getForAction, wasGetFor := action.(cmfake.GetForAction)
		if !wasGetFor {
			return true, nil, fmt.Errorf("expected a get-for action, got %v instead", action)
		}

		if tc.metric == nil {
			return true, nil, fmt.Errorf("no custom metrics specified in test client")
		}

		assert.Equal(t, tc.metric.name, getForAction.GetMetricName(), "the metric requested should have matched the one specified")

		if getForAction.GetName() == "*" {
			metrics := cmapi.MetricValueList{}

			// multiple objects
			assert.Equal(t, "pods", getForAction.GetResource().Resource, "the type of object that we requested multiple metrics for should have been pods")

			for i, level := range tc.metric.levels {
				podMetric := cmapi.MetricValue{
					DescribedObject: v1.ObjectReference{
						Kind:      "Pod",
						Name:      fmt.Sprintf("%s-%d", podNamePrefix, i),
						Namespace: testNamespace,
					},
					Timestamp: metav1.Time{Time: tc.timestamp},
					Metric: cmapi.MetricIdentifier{
						Name: tc.metric.name,
					},
					Value: *resource.NewMilliQuantity(level, resource.DecimalSI),
				}
				metrics.Items = append(metrics.Items, podMetric)
			}

			return true, &metrics, nil
		}
		name := getForAction.GetName()
		mapper := testrestmapper.TestOnlyStaticRESTMapper(legacyscheme.Scheme)
		metrics := &cmapi.MetricValueList{}
		assert.NotNil(t, tc.metric.singleObject, "should have only requested a single-object metric when calling GetObjectMetricReplicas")
		gk := schema.FromAPIVersionAndKind(tc.metric.singleObject.APIVersion, tc.metric.singleObject.Kind).GroupKind()
		mapping, err := mapper.RESTMapping(gk)
		if err != nil {
			return true, nil, fmt.Errorf("unable to get mapping for %s: %v", gk.String(), err)
		}
		groupResource := mapping.Resource.GroupResource()

		assert.Equal(t, groupResource.String(), getForAction.GetResource().Resource, "should have requested metrics for the resource matching the GroupKind passed in")
		assert.Equal(t, tc.metric.singleObject.Name, name, "should have requested metrics for the object matching the name passed in")

		metrics.Items = []cmapi.MetricValue{
			{
				DescribedObject: v1.ObjectReference{
					Kind:       tc.metric.singleObject.Kind,
					APIVersion: tc.metric.singleObject.APIVersion,
					Name:       name,
				},
				Timestamp: metav1.Time{Time: tc.timestamp},
				Metric: cmapi.MetricIdentifier{
					Name: tc.metric.name,
				},
				Value: *resource.NewMilliQuantity(int64(tc.metric.levels[0]), resource.DecimalSI),
			},
		}

		return true, metrics, nil
	})
	return fakeCMClient
}

func (tc *replicaCalcTestCase) prepareTestEMClient(t *testing.T) *emfake.FakeExternalMetricsClient {
	fakeEMClient := &emfake.FakeExternalMetricsClient{}
	fakeEMClient.AddReactor("list", "*", func(action core.Action) (handled bool, ret runtime.Object, err error) {
		listAction, wasList := action.(core.ListAction)
		if !wasList {
			return true, nil, fmt.Errorf("expected a list-for action, got %v instead", action)
		}

		if tc.metric == nil {
			return true, nil, fmt.Errorf("no external metrics specified in test client")
		}

		assert.Equal(t, tc.metric.name, listAction.GetResource().Resource, "the metric requested should have matched the one specified")

		selector, err := metav1.LabelSelectorAsSelector(tc.metric.selector)
		if err != nil {
			return true, nil, fmt.Errorf("failed to convert label selector specified in test client")
		}
		assert.Equal(t, selector, listAction.GetListRestrictions().Labels, "the metric selector should have matched the one specified")

		metrics := emapi.ExternalMetricValueList{}

		for _, level := range tc.metric.levels {
			metric := emapi.ExternalMetricValue{
				Timestamp:  metav1.Time{Time: tc.timestamp},
				MetricName: tc.metric.name,
				Value:      *resource.NewMilliQuantity(level, resource.DecimalSI),
			}
			metrics.Items = append(metrics.Items, metric)
		}

		return true, &metrics, nil
	})
	return fakeEMClient
}

func (tc *replicaCalcTestCase) prepareTestClient(t *testing.T) (*fake.Clientset, *metricsfake.Clientset, *cmfake.FakeCustomMetricsClient, *emfake.FakeExternalMetricsClient) {
	fakeClient := tc.prepareTestClientSet()
	fakeMetricsClient := tc.prepareTestMetricsClient()
	fakeCMClient := tc.prepareTestCMClient(t)
	fakeEMClient := tc.prepareTestEMClient(t)
	return fakeClient, fakeMetricsClient, fakeCMClient, fakeEMClient
}

func (tc *replicaCalcTestCase) runTest(t *testing.T) {
	testClient, testMetricsClient, testCMClient, testEMClient := tc.prepareTestClient(t)
	metricsClient := metricsclient.NewRESTMetricsClient(testMetricsClient.MetricsV1beta1(), testCMClient, testEMClient)

	informerFactory := informers.NewSharedInformerFactory(testClient, controller.NoResyncPeriodFunc())
	informer := informerFactory.Core().V1().Pods()

	replicaCalc := NewReplicaCalculator(metricsClient, informer.Lister(), defaultTestingTolerance, defaultTestingCPUInitializationPeriod, defaultTestingDelayOfInitialReadinessStatus)

	stop := make(chan struct{})
	defer close(stop)
	informerFactory.Start(stop)
	if !cache.WaitForNamedCacheSync("HPA", stop, informer.Informer().HasSynced) {
		return
	}

	selector, err := metav1.LabelSelectorAsSelector(&metav1.LabelSelector{
		MatchLabels: map[string]string{"name": podNamePrefix},
	})
	require.NoError(t, err, "something went horribly wrong...")

	if tc.resource != nil {
		outReplicas, outUtilization, outRawValue, outTimestamp, err := replicaCalc.GetResourceReplicas(context.TODO(), tc.currentReplicas, tc.resource.targetUtilization, tc.resource.name, testNamespace, selector, tc.container)

		if tc.expectedError != nil {
			require.Error(t, err, "there should be an error calculating the replica count")
			assert.ErrorContains(t, err, tc.expectedError.Error(), "the error message should have contained the expected error message")
			return
		}
		require.NoError(t, err, "there should not have been an error calculating the replica count")
		assert.Equal(t, tc.expectedReplicas, outReplicas, "replicas should be as expected")
		assert.Equal(t, tc.resource.expectedUtilization, outUtilization, "utilization should be as expected")
		assert.Equal(t, tc.resource.expectedValue, outRawValue, "raw value should be as expected")
		assert.True(t, tc.timestamp.Equal(outTimestamp), "timestamp should be as expected")
		return
	}

	var outReplicas int32
	var outUsage int64
	var outTimestamp time.Time
	switch tc.metric.metricType {
	case objectMetric:
		if tc.metric.singleObject == nil {
			t.Fatal("Metric specified as objectMetric but metric.singleObject is nil.")
		}
		outReplicas, outUsage, outTimestamp, err = replicaCalc.GetObjectMetricReplicas(tc.currentReplicas, tc.metric.targetUsage, tc.metric.name, testNamespace, tc.metric.singleObject, selector, nil)
	case objectPerPodMetric:
		if tc.metric.singleObject == nil {
			t.Fatal("Metric specified as objectMetric but metric.singleObject is nil.")
		}
		outReplicas, outUsage, outTimestamp, err = replicaCalc.GetObjectPerPodMetricReplicas(tc.currentReplicas, tc.metric.perPodTargetUsage, tc.metric.name, testNamespace, tc.metric.singleObject, nil)
	case externalMetric:
		if tc.metric.selector == nil {
			t.Fatal("Metric specified as externalMetric but metric.selector is nil.")
		}
		if tc.metric.targetUsage <= 0 {
			t.Fatalf("Metric specified as externalMetric but metric.targetUsage is %d which is <=0.", tc.metric.targetUsage)
		}
		outReplicas, outUsage, outTimestamp, err = replicaCalc.GetExternalMetricReplicas(tc.currentReplicas, tc.metric.targetUsage, tc.metric.name, testNamespace, tc.metric.selector, selector)
	case externalPerPodMetric:
		if tc.metric.selector == nil {
			t.Fatal("Metric specified as externalPerPodMetric but metric.selector is nil.")
		}
		if tc.metric.perPodTargetUsage <= 0 {
			t.Fatalf("Metric specified as externalPerPodMetric but metric.perPodTargetUsage is %d which is <=0.", tc.metric.perPodTargetUsage)
		}

		outReplicas, outUsage, outTimestamp, err = replicaCalc.GetExternalPerPodMetricReplicas(tc.currentReplicas, tc.metric.perPodTargetUsage, tc.metric.name, testNamespace, tc.metric.selector)
	case podMetric:
		outReplicas, outUsage, outTimestamp, err = replicaCalc.GetMetricReplicas(tc.currentReplicas, tc.metric.targetUsage, tc.metric.name, testNamespace, selector, nil)
	default:
		t.Fatalf("Unknown metric type: %d", tc.metric.metricType)
	}

	if tc.expectedError != nil {
		require.Error(t, err, "there should be an error calculating the replica count")
		assert.ErrorContains(t, err, tc.expectedError.Error(), "the error message should have contained the expected error message")
		return
	}
	require.NoError(t, err, "there should not have been an error calculating the replica count")
	assert.Equal(t, tc.expectedReplicas, outReplicas, "replicas should be as expected")
	assert.Equal(t, tc.metric.expectedUsage, outUsage, "usage should be as expected")
	assert.True(t, tc.timestamp.Equal(outTimestamp), "timestamp should be as expected")
}
func makePodMetricLevels(containerMetric ...int64) [][]int64 {
	metrics := make([][]int64, len(containerMetric))
	for i := 0; i < len(containerMetric); i++ {
		metrics[i] = make([]int64, numContainersPerPod)
		for j := 0; j < numContainersPerPod; j++ {
			metrics[i][j] = containerMetric[i]
		}
	}
	return metrics
}
func TestReplicaCalcDisjointResourcesMetrics(t *testing.T) {
	tc := replicaCalcTestCase{
		currentReplicas: 1,
		expectedError:   fmt.Errorf("no metrics returned matched known pods"),
		resource: &resourceInfo{
			name:     v1.ResourceCPU,
			requests: []resource.Quantity{resource.MustParse("1.0")},
			levels:   makePodMetricLevels(100),
			podNames: []string{"an-older-pod-name"},

			targetUtilization: 100,
		},
	}
	tc.runTest(t)
}

func TestReplicaCalcScaleUp(t *testing.T) {
	tc := replicaCalcTestCase{
		currentReplicas:  3,
		expectedReplicas: 5,
		resource: &resourceInfo{
			name:     v1.ResourceCPU,
			requests: []resource.Quantity{resource.MustParse("1.0"), resource.MustParse("1.0"), resource.MustParse("1.0")},
			levels:   makePodMetricLevels(300, 500, 700),

			targetUtilization:   30,
			expectedUtilization: 50,
			expectedValue:       numContainersPerPod * 500,
		},
	}
	tc.runTest(t)
}

func TestReplicaCalcContainerScaleUp(t *testing.T) {
	tc := replicaCalcTestCase{
		currentReplicas:  3,
		expectedReplicas: 5,
		resource: &resourceInfo{
			name:     v1.ResourceCPU,
			requests: []resource.Quantity{resource.MustParse("1.0"), resource.MustParse("1.0"), resource.MustParse("1.0")},
			levels:   [][]int64{{1000, 300}, {1000, 500}, {1000, 700}},

			targetUtilization:   30,
			expectedUtilization: 50,
			expectedValue:       500,
		},
		container: "container2",
	}
	tc.runTest(t)
}

func TestReplicaCalcScaleUpUnreadyLessScale(t *testing.T) {
	tc := replicaCalcTestCase{
		currentReplicas:  3,
		expectedReplicas: 4,
		podReadiness:     []v1.ConditionStatus{v1.ConditionFalse, v1.ConditionTrue, v1.ConditionTrue},
		resource: &resourceInfo{
			name:     v1.ResourceCPU,
			requests: []resource.Quantity{resource.MustParse("1.0"), resource.MustParse("1.0"), resource.MustParse("1.0")},
			levels:   makePodMetricLevels(300, 500, 700),

			targetUtilization:   30,
			expectedUtilization: 60,
			expectedValue:       numContainersPerPod * 600,
		},
	}
	tc.runTest(t)
}

func TestReplicaCalcScaleUpContainerHotCpuLessScale(t *testing.T) {
	tc := replicaCalcTestCase{
		currentReplicas:  3,
		expectedReplicas: 4,
		podStartTime:     []metav1.Time{hotCPUCreationTime(), coolCPUCreationTime(), coolCPUCreationTime()},
		resource: &resourceInfo{
			name:     v1.ResourceCPU,
			requests: []resource.Quantity{resource.MustParse("1.0"), resource.MustParse("1.0"), resource.MustParse("1.0")},
			levels:   [][]int64{{0, 300}, {0, 500}, {0, 700}},

			targetUtilization:   30,
			expectedUtilization: 60,
			expectedValue:       600,
		},
		container: "container2",
	}
	tc.runTest(t)
}

func TestReplicaCalcScaleUpHotCpuLessScale(t *testing.T) {
	tc := replicaCalcTestCase{
		currentReplicas:  3,
		expectedReplicas: 4,
		podStartTime:     []metav1.Time{hotCPUCreationTime(), coolCPUCreationTime(), coolCPUCreationTime()},
		resource: &resourceInfo{
			name:     v1.ResourceCPU,
			requests: []resource.Quantity{resource.MustParse("1.0"), resource.MustParse("1.0"), resource.MustParse("1.0")},
			levels:   makePodMetricLevels(300, 500, 700),

			targetUtilization:   30,
			expectedUtilization: 60,
			expectedValue:       numContainersPerPod * 600,
		},
	}
	tc.runTest(t)
}

func TestReplicaCalcScaleUpUnreadyNoScale(t *testing.T) {
	tc := replicaCalcTestCase{
		currentReplicas:  3,
		expectedReplicas: 3,
		podReadiness:     []v1.ConditionStatus{v1.ConditionTrue, v1.ConditionFalse, v1.ConditionFalse},
		resource: &resourceInfo{
			name:     v1.ResourceCPU,
			requests: []resource.Quantity{resource.MustParse("1.0"), resource.MustParse("1.0"), resource.MustParse("1.0")},
			levels:   makePodMetricLevels(400, 500, 700),

			targetUtilization:   30,
			expectedUtilization: 40,
			expectedValue:       numContainersPerPod * 400,
		},
	}
	tc.runTest(t)
}

func TestReplicaCalcScaleHotCpuNoScale(t *testing.T) {
	tc := replicaCalcTestCase{
		currentReplicas:  3,
		expectedReplicas: 3,
		podReadiness:     []v1.ConditionStatus{v1.ConditionTrue, v1.ConditionFalse, v1.ConditionFalse},
		podStartTime:     []metav1.Time{coolCPUCreationTime(), hotCPUCreationTime(), hotCPUCreationTime()},
		resource: &resourceInfo{
			name:     v1.ResourceCPU,
			requests: []resource.Quantity{resource.MustParse("1.0"), resource.MustParse("1.0"), resource.MustParse("1.0")},
			levels:   makePodMetricLevels(400, 500, 700),

			targetUtilization:   30,
			expectedUtilization: 40,
			expectedValue:       numContainersPerPod * 400,
		},
	}
	tc.runTest(t)
}

func TestReplicaCalcScaleUpIgnoresFailedPods(t *testing.T) {
	tc := replicaCalcTestCase{
		currentReplicas:  2,
		expectedReplicas: 4,
		podReadiness:     []v1.ConditionStatus{v1.ConditionTrue, v1.ConditionTrue, v1.ConditionFalse, v1.ConditionFalse},
		podPhase:         []v1.PodPhase{v1.PodRunning, v1.PodRunning, v1.PodFailed, v1.PodFailed},
		resource: &resourceInfo{
			name:     v1.ResourceCPU,
			requests: []resource.Quantity{resource.MustParse("1.0"), resource.MustParse("1.0"), resource.MustParse("1.0"), resource.MustParse("1.0")},
			levels:   makePodMetricLevels(500, 700),

			targetUtilization:   30,
			expectedUtilization: 60,
			expectedValue:       numContainersPerPod * 600,
		},
	}
	tc.runTest(t)
}

func TestReplicaCalcMissingContainerMetricScaleUpIgnoresFailedPods(t *testing.T) {
	tc := replicaCalcTestCase{
		currentReplicas:  2,
		expectedReplicas: 4,
		podReadiness:     []v1.ConditionStatus{v1.ConditionTrue, v1.ConditionTrue, v1.ConditionFalse, v1.ConditionFalse},
		podPhase:         []v1.PodPhase{v1.PodRunning, v1.PodRunning, v1.PodFailed, v1.PodFailed},
		resource: &resourceInfo{
			name:     v1.ResourceCPU,
			requests: []resource.Quantity{resource.MustParse("1.0"), resource.MustParse("1.0"), resource.MustParse("1.0"), resource.MustParse("1.0")},
			levels:   [][]int64{{1000, 500}, {9000, 700}, {1000}, {9000}},

			targetUtilization:   30,
			expectedUtilization: 60,
			expectedValue:       600,
		},
		container: "container2",
	}
	tc.runTest(t)
}

func TestReplicaCalcScaleUpContainerIgnoresFailedPods(t *testing.T) {
	tc := replicaCalcTestCase{
		currentReplicas:  2,
		expectedReplicas: 4,
		podReadiness:     []v1.ConditionStatus{v1.ConditionTrue, v1.ConditionTrue, v1.ConditionFalse, v1.ConditionFalse},
		podPhase:         []v1.PodPhase{v1.PodRunning, v1.PodRunning, v1.PodFailed, v1.PodFailed},
		resource: &resourceInfo{
			name:     v1.ResourceCPU,
			requests: []resource.Quantity{resource.MustParse("1.0"), resource.MustParse("1.0"), resource.MustParse("1.0"), resource.MustParse("1.0")},
			levels:   [][]int64{{1000, 500}, {9000, 700}},

			targetUtilization:   30,
			expectedUtilization: 60,
			expectedValue:       600,
		},
		container: "container2",
	}
	tc.runTest(t)
}

func TestReplicaCalcScaleUpIgnoresDeletionPods(t *testing.T) {
	tc := replicaCalcTestCase{
		currentReplicas:      2,
		expectedReplicas:     4,
		podReadiness:         []v1.ConditionStatus{v1.ConditionTrue, v1.ConditionTrue, v1.ConditionFalse, v1.ConditionFalse},
		podPhase:             []v1.PodPhase{v1.PodRunning, v1.PodRunning, v1.PodRunning, v1.PodRunning},
		podDeletionTimestamp: []bool{false, false, true, true},
		resource: &resourceInfo{
			name:     v1.ResourceCPU,
			requests: []resource.Quantity{resource.MustParse("1.0"), resource.MustParse("1.0"), resource.MustParse("1.0"), resource.MustParse("1.0")},
			levels:   makePodMetricLevels(500, 700),

			targetUtilization:   30,
			expectedUtilization: 60,
			expectedValue:       numContainersPerPod * 600,
		},
	}
	tc.runTest(t)
}

func TestReplicaCalcScaleUpContainerIgnoresDeletionPods(t *testing.T) {
	tc := replicaCalcTestCase{
		currentReplicas:      2,
		expectedReplicas:     4,
		podReadiness:         []v1.ConditionStatus{v1.ConditionTrue, v1.ConditionTrue, v1.ConditionFalse, v1.ConditionFalse},
		podPhase:             []v1.PodPhase{v1.PodRunning, v1.PodRunning, v1.PodRunning, v1.PodRunning},
		podDeletionTimestamp: []bool{false, false, true, true},
		resource: &resourceInfo{
			name:     v1.ResourceCPU,
			requests: []resource.Quantity{resource.MustParse("1.0"), resource.MustParse("1.0"), resource.MustParse("1.0"), resource.MustParse("1.0")},
			levels:   makePodMetricLevels(500, 700), // TODO: This test is broken and works only because of missing metrics

			targetUtilization:   30,
			expectedUtilization: 60,
			expectedValue:       600,
		},
		container: "container1",
	}
	tc.runTest(t)
}

func TestReplicaCalcScaleUpCM(t *testing.T) {
	tc := replicaCalcTestCase{
		currentReplicas:  3,
		expectedReplicas: 4,
		metric: &metricInfo{
			name:          "qps",
			levels:        []int64{20000, 10000, 30000},
			targetUsage:   15000,
			expectedUsage: 20000,
			metricType:    podMetric,
		},
	}
	tc.runTest(t)
}

func TestReplicaCalcScaleUpCMUnreadyHotCpuNoLessScale(t *testing.T) {
	tc := replicaCalcTestCase{
		currentReplicas:  3,
		expectedReplicas: 6,
		podReadiness:     []v1.ConditionStatus{v1.ConditionTrue, v1.ConditionTrue, v1.ConditionFalse},
		podStartTime:     []metav1.Time{coolCPUCreationTime(), coolCPUCreationTime(), hotCPUCreationTime()},
		metric: &metricInfo{
			name:          "qps",
			levels:        []int64{50000, 10000, 30000},
			targetUsage:   15000,
			expectedUsage: 30000,
			metricType:    podMetric,
		},
	}
	tc.runTest(t)
}

func TestReplicaCalcScaleUpCMUnreadyHotCpuScaleWouldScaleDown(t *testing.T) {
	tc := replicaCalcTestCase{
		currentReplicas:  3,
		expectedReplicas: 7,
		podReadiness:     []v1.ConditionStatus{v1.ConditionFalse, v1.ConditionTrue, v1.ConditionFalse},
		podStartTime:     []metav1.Time{hotCPUCreationTime(), coolCPUCreationTime(), hotCPUCreationTime()},
		metric: &metricInfo{
			name:          "qps",
			levels:        []int64{50000, 15000, 30000},
			targetUsage:   15000,
			expectedUsage: 31666,
			metricType:    podMetric,
		},
	}
	tc.runTest(t)
}

func TestReplicaCalcScaleUpCMObject(t *testing.T) {
	tc := replicaCalcTestCase{
		currentReplicas:  3,
		expectedReplicas: 4,
		metric: &metricInfo{
			name:          "qps",
			levels:        []int64{20000},
			targetUsage:   15000,
			expectedUsage: 20000,
			singleObject: &autoscalingv2.CrossVersionObjectReference{
				Kind:       "Deployment",
				APIVersion: "apps/v1",
				Name:       "some-deployment",
			},
		},
	}
	tc.runTest(t)
}

func TestReplicaCalcScaleUpCMPerPodObject(t *testing.T) {
	tc := replicaCalcTestCase{
		currentReplicas:  3,
		expectedReplicas: 4,
		metric: &metricInfo{
			metricType:        objectPerPodMetric,
			name:              "qps",
			levels:            []int64{20000},
			perPodTargetUsage: 5000,
			expectedUsage:     6667,
			singleObject: &autoscalingv2.CrossVersionObjectReference{
				Kind:       "Deployment",
				APIVersion: "apps/v1",
				Name:       "some-deployment",
			},
		},
	}
	tc.runTest(t)
}

func TestReplicaCalcScaleUpCMObjectIgnoresUnreadyPods(t *testing.T) {
	tc := replicaCalcTestCase{
		currentReplicas:  3,
		expectedReplicas: 5, // If we did not ignore unready pods, we'd expect 15 replicas.
		podReadiness:     []v1.ConditionStatus{v1.ConditionFalse, v1.ConditionTrue, v1.ConditionFalse},
		metric: &metricInfo{
			name:          "qps",
			levels:        []int64{50000},
			targetUsage:   10000,
			expectedUsage: 50000,
			singleObject: &autoscalingv2.CrossVersionObjectReference{
				Kind:       "Deployment",
				APIVersion: "apps/v1",
				Name:       "some-deployment",
			},
		},
	}
	tc.runTest(t)
}

func TestReplicaCalcScaleUpCMExternal(t *testing.T) {
	tc := replicaCalcTestCase{
		currentReplicas:  1,
		expectedReplicas: 2,
		metric: &metricInfo{
			name:          "qps",
			levels:        []int64{8600},
			targetUsage:   4400,
			expectedUsage: 8600,
			selector:      &metav1.LabelSelector{MatchLabels: map[string]string{"label": "value"}},
			metricType:    podMetric,
		},
	}
	tc.runTest(t)
}

func TestReplicaCalcScaleUpCMExternalIgnoresUnreadyPods(t *testing.T) {
	tc := replicaCalcTestCase{
		currentReplicas:  3,
		expectedReplicas: 2, // Would expect 6 if we didn't ignore unready pods
		podReadiness:     []v1.ConditionStatus{v1.ConditionFalse, v1.ConditionTrue, v1.ConditionFalse},
		metric: &metricInfo{
			name:          "qps",
			levels:        []int64{8600},
			targetUsage:   4400,
			expectedUsage: 8600,
			selector:      &metav1.LabelSelector{MatchLabels: map[string]string{"label": "value"}},
			metricType:    externalMetric,
		},
	}
	tc.runTest(t)
}

func TestReplicaCalcScaleUpCMExternalNoLabels(t *testing.T) {
	tc := replicaCalcTestCase{
		currentReplicas:  1,
		expectedReplicas: 2,
		metric: &metricInfo{
			name:          "qps",
			levels:        []int64{8600},
			targetUsage:   4400,
			expectedUsage: 8600,
			metricType:    podMetric,
		},
	}
	tc.runTest(t)
}

func TestReplicaCalcScaleUpPerPodCMExternal(t *testing.T) {
	tc := replicaCalcTestCase{
		currentReplicas:  3,
		expectedReplicas: 4,
		metric: &metricInfo{
			name:              "qps",
			levels:            []int64{8600},
			perPodTargetUsage: 2150,
			expectedUsage:     2867,
			selector:          &metav1.LabelSelector{MatchLabels: map[string]string{"label": "value"}},
			metricType:        externalPerPodMetric,
		},
	}
	tc.runTest(t)
}

func TestReplicaCalcScaleDown(t *testing.T) {
	tc := replicaCalcTestCase{
		currentReplicas:  5,
		expectedReplicas: 3,
		resource: &resourceInfo{
			name:     v1.ResourceCPU,
			requests: []resource.Quantity{resource.MustParse("1.0"), resource.MustParse("1.0"), resource.MustParse("1.0"), resource.MustParse("1.0"), resource.MustParse("1.0")},
			levels:   makePodMetricLevels(100, 300, 500, 250, 250),

			targetUtilization:   50,
			expectedUtilization: 28,
			expectedValue:       numContainersPerPod * 280,
		},
	}
	tc.runTest(t)
}

func TestReplicaCalcContainerScaleDown(t *testing.T) {
	tc := replicaCalcTestCase{
		currentReplicas:  5,
		expectedReplicas: 3,
		resource: &resourceInfo{
			name:     v1.ResourceCPU,
			requests: []resource.Quantity{resource.MustParse("1.0"), resource.MustParse("1.0"), resource.MustParse("1.0"), resource.MustParse("1.0"), resource.MustParse("1.0")},
			levels:   [][]int64{{1000, 100}, {1000, 300}, {1000, 500}, {1000, 250}, {1000, 250}},

			targetUtilization:   50,
			expectedUtilization: 28,
			expectedValue:       280,
		},
		container: "container2",
	}
	tc.runTest(t)
}

func TestReplicaCalcScaleDownCM(t *testing.T) {
	tc := replicaCalcTestCase{
		currentReplicas:  5,
		expectedReplicas: 3,
		metric: &metricInfo{
			name:          "qps",
			levels:        []int64{12000, 12000, 12000, 12000, 12000},
			targetUsage:   20000,
			expectedUsage: 12000,
			metricType:    podMetric,
		},
	}
	tc.runTest(t)
}

func TestReplicaCalcScaleDownPerPodCMObject(t *testing.T) {
	tc := replicaCalcTestCase{
		currentReplicas:  5,
		expectedReplicas: 3,
		metric: &metricInfo{
			name:              "qps",
			levels:            []int64{6000},
			perPodTargetUsage: 2000,
			expectedUsage:     1200,
			singleObject: &autoscalingv2.CrossVersionObjectReference{
				Kind:       "Deployment",
				APIVersion: "apps/v1",
				Name:       "some-deployment",
			},
			metricType: objectPerPodMetric,
		},
	}
	tc.runTest(t)
}

func TestReplicaCalcScaleDownCMObject(t *testing.T) {
	tc := replicaCalcTestCase{
		currentReplicas:  5,
		expectedReplicas: 3,
		metric: &metricInfo{
			name:          "qps",
			levels:        []int64{12000},
			targetUsage:   20000,
			expectedUsage: 12000,
			singleObject: &autoscalingv2.CrossVersionObjectReference{
				Kind:       "Deployment",
				APIVersion: "apps/v1",
				Name:       "some-deployment",
			},
		},
	}
	tc.runTest(t)
}

func TestReplicaCalcScaleDownCMExternal(t *testing.T) {
	tc := replicaCalcTestCase{
		currentReplicas:  5,
		expectedReplicas: 3,
		metric: &metricInfo{
			name:          "qps",
			levels:        []int64{8600},
			targetUsage:   14334,
			expectedUsage: 8600,
			selector:      &metav1.LabelSelector{MatchLabels: map[string]string{"label": "value"}},
			metricType:    externalMetric,
		},
	}
	tc.runTest(t)
}

func TestReplicaCalcScaleDownPerPodCMExternal(t *testing.T) {
	tc := replicaCalcTestCase{
		currentReplicas:  5,
		expectedReplicas: 3,
		metric: &metricInfo{
			name:              "qps",
			levels:            []int64{8600},
			perPodTargetUsage: 2867,
			expectedUsage:     1720,
			selector:          &metav1.LabelSelector{MatchLabels: map[string]string{"label": "value"}},
			metricType:        externalPerPodMetric,
		},
	}
	tc.runTest(t)
}

func TestReplicaCalcScaleDownExcludeUnreadyPods(t *testing.T) {
	tc := replicaCalcTestCase{
		currentReplicas:  5,
		expectedReplicas: 2,
		podReadiness:     []v1.ConditionStatus{v1.ConditionTrue, v1.ConditionTrue, v1.ConditionTrue, v1.ConditionFalse, v1.ConditionFalse},
		resource: &resourceInfo{
			name:     v1.ResourceCPU,
			requests: []resource.Quantity{resource.MustParse("1.0"), resource.MustParse("1.0"), resource.MustParse("1.0"), resource.MustParse("1.0"), resource.MustParse("1.0")},
			levels:   makePodMetricLevels(100, 300, 500, 250, 250),

			targetUtilization:   50,
			expectedUtilization: 30,
			expectedValue:       numContainersPerPod * 300,
		},
	}
	tc.runTest(t)
}

func TestReplicaCalcScaleDownContainerExcludeUnreadyPods(t *testing.T) {
	tc := replicaCalcTestCase{
		currentReplicas:  5,
		expectedReplicas: 2,
		podReadiness:     []v1.ConditionStatus{v1.ConditionTrue, v1.ConditionTrue, v1.ConditionTrue, v1.ConditionFalse, v1.ConditionFalse},
		resource: &resourceInfo{
			name:     v1.ResourceCPU,
			requests: []resource.Quantity{resource.MustParse("1.0"), resource.MustParse("1.0"), resource.MustParse("1.0"), resource.MustParse("1.0"), resource.MustParse("1.0")},
			levels:   [][]int64{{1000, 100}, {1000, 300}, {1000, 500}, {1000, 250}, {1000, 250}},

			targetUtilization:   50,
			expectedUtilization: 30,
			expectedValue:       300,
		},
		container: "container2",
	}
	tc.runTest(t)
}

func TestReplicaCalcScaleDownExcludeUnscheduledPods(t *testing.T) {
	tc := replicaCalcTestCase{
		currentReplicas:  5,
		expectedReplicas: 1,
		podReadiness:     []v1.ConditionStatus{v1.ConditionTrue, v1.ConditionFalse, v1.ConditionFalse, v1.ConditionFalse, v1.ConditionFalse},
		podPhase:         []v1.PodPhase{v1.PodRunning, v1.PodPending, v1.PodPending, v1.PodPending, v1.PodPending},
		resource: &resourceInfo{
			name:     v1.ResourceCPU,
			requests: []resource.Quantity{resource.MustParse("1.0"), resource.MustParse("1.0"), resource.MustParse("1.0"), resource.MustParse("1.0"), resource.MustParse("1.0")},
			levels:   makePodMetricLevels(100),

			targetUtilization:   50,
			expectedUtilization: 10,
			expectedValue:       numContainersPerPod * 100,
		},
	}
	tc.runTest(t)
}

func TestReplicaCalcScaleDownContainerExcludeUnscheduledPods(t *testing.T) {
	tc := replicaCalcTestCase{
		currentReplicas:  5,
		expectedReplicas: 1,
		podReadiness:     []v1.ConditionStatus{v1.ConditionTrue, v1.ConditionFalse, v1.ConditionFalse, v1.ConditionFalse, v1.ConditionFalse},
		podPhase:         []v1.PodPhase{v1.PodRunning, v1.PodPending, v1.PodPending, v1.PodPending, v1.PodPending},
		resource: &resourceInfo{
			name:     v1.ResourceCPU,
			requests: []resource.Quantity{resource.MustParse("1.0"), resource.MustParse("1.0"), resource.MustParse("1.0"), resource.MustParse("1.0"), resource.MustParse("1.0")},
			levels:   [][]int64{{1000, 100}, {1000, 300}, {1000, 500}, {1000, 250}, {1000, 250}},

			targetUtilization:   50,
			expectedUtilization: 10,
			expectedValue:       100,
		},
		container: "container2",
	}
	tc.runTest(t)
}

func TestReplicaCalcScaleDownIgnoreHotCpuPods(t *testing.T) {
	tc := replicaCalcTestCase{
		currentReplicas:  5,
		expectedReplicas: 2,
		podStartTime:     []metav1.Time{coolCPUCreationTime(), coolCPUCreationTime(), coolCPUCreationTime(), hotCPUCreationTime(), hotCPUCreationTime()},
		resource: &resourceInfo{
			name:     v1.ResourceCPU,
			requests: []resource.Quantity{resource.MustParse("1.0"), resource.MustParse("1.0"), resource.MustParse("1.0"), resource.MustParse("1.0"), resource.MustParse("1.0")},
			levels:   makePodMetricLevels(100, 300, 500, 250, 250),

			targetUtilization:   50,
			expectedUtilization: 30,
			expectedValue:       numContainersPerPod * 300,
		},
	}
	tc.runTest(t)
}

func TestReplicaCalcScaleDownContainerIgnoreHotCpuPods(t *testing.T) {
	tc := replicaCalcTestCase{
		currentReplicas:  5,
		expectedReplicas: 2,
		podStartTime:     []metav1.Time{coolCPUCreationTime(), coolCPUCreationTime(), coolCPUCreationTime(), hotCPUCreationTime(), hotCPUCreationTime()},
		resource: &resourceInfo{
			name:     v1.ResourceCPU,
			requests: []resource.Quantity{resource.MustParse("1.0"), resource.MustParse("1.0"), resource.MustParse("1.0"), resource.MustParse("1.0"), resource.MustParse("1.0")},
			levels:   [][]int64{{1000, 100}, {1000, 300}, {1000, 500}, {1000, 1000}, {1000, 1000}},

			targetUtilization:   50,
			expectedUtilization: 30,
			expectedValue:       300,
		},
		container: "container2",
	}
	tc.runTest(t)
}

func TestReplicaCalcScaleDownIgnoresFailedPods(t *testing.T) {
	tc := replicaCalcTestCase{
		currentReplicas:  5,
		expectedReplicas: 3,
		podReadiness:     []v1.ConditionStatus{v1.ConditionTrue, v1.ConditionTrue, v1.ConditionTrue, v1.ConditionTrue, v1.ConditionTrue, v1.ConditionFalse, v1.ConditionFalse},
		podPhase:         []v1.PodPhase{v1.PodRunning, v1.PodRunning, v1.PodRunning, v1.PodRunning, v1.PodRunning, v1.PodFailed, v1.PodFailed},
		resource: &resourceInfo{
			name:     v1.ResourceCPU,
			requests: []resource.Quantity{resource.MustParse("1.0"), resource.MustParse("1.0"), resource.MustParse("1.0"), resource.MustParse("1.0"), resource.MustParse("1.0"), resource.MustParse("1.0"), resource.MustParse("1.0")},
			levels:   makePodMetricLevels(100, 300, 500, 250, 250),

			targetUtilization:   50,
			expectedUtilization: 28,
			expectedValue:       numContainersPerPod * 280,
		},
	}
	tc.runTest(t)
}

func TestReplicaCalcScaleDownContainerIgnoresFailedPods(t *testing.T) {
	tc := replicaCalcTestCase{
		currentReplicas:  5,
		expectedReplicas: 3,
		podReadiness:     []v1.ConditionStatus{v1.ConditionTrue, v1.ConditionTrue, v1.ConditionTrue, v1.ConditionTrue, v1.ConditionTrue, v1.ConditionFalse, v1.ConditionFalse},
		podPhase:         []v1.PodPhase{v1.PodRunning, v1.PodRunning, v1.PodRunning, v1.PodRunning, v1.PodRunning, v1.PodFailed, v1.PodFailed},
		resource: &resourceInfo{
			name:     v1.ResourceCPU,
			requests: []resource.Quantity{resource.MustParse("1.0"), resource.MustParse("1.0"), resource.MustParse("1.0"), resource.MustParse("1.0"), resource.MustParse("1.0"), resource.MustParse("1.0"), resource.MustParse("1.0")},
			levels:   [][]int64{{1000, 100}, {1000, 300}, {1000, 500}, {1000, 250}, {1000, 250}}, // TODO: Test is broken

			targetUtilization:   50,
			expectedUtilization: 28,
			expectedValue:       280,
		},
		container: "container2",
	}
	tc.runTest(t)
}

func TestReplicaCalcScaleDownIgnoresDeletionPods(t *testing.T) {
	tc := replicaCalcTestCase{
		currentReplicas:      5,
		expectedReplicas:     3,
		podReadiness:         []v1.ConditionStatus{v1.ConditionTrue, v1.ConditionTrue, v1.ConditionTrue, v1.ConditionTrue, v1.ConditionTrue, v1.ConditionFalse, v1.ConditionFalse},
		podPhase:             []v1.PodPhase{v1.PodRunning, v1.PodRunning, v1.PodRunning, v1.PodRunning, v1.PodRunning, v1.PodRunning, v1.PodRunning},
		podDeletionTimestamp: []bool{false, false, false, false, false, true, true},
		resource: &resourceInfo{
			name:     v1.ResourceCPU,
			requests: []resource.Quantity{resource.MustParse("1.0"), resource.MustParse("1.0"), resource.MustParse("1.0"), resource.MustParse("1.0"), resource.MustParse("1.0"), resource.MustParse("1.0"), resource.MustParse("1.0")},
			levels:   makePodMetricLevels(100, 300, 500, 250, 250),

			targetUtilization:   50,
			expectedUtilization: 28,
			expectedValue:       numContainersPerPod * 280,
		},
	}
	tc.runTest(t)
}

// Regression test for https://github.com/kubernetes/kubernetes/issues/83561
func TestReplicaCalcScaleDownIgnoresDeletionPods_StillRunning(t *testing.T) {
	tc := replicaCalcTestCase{
		currentReplicas:      5,
		expectedReplicas:     3,
		podReadiness:         []v1.ConditionStatus{v1.ConditionTrue, v1.ConditionTrue, v1.ConditionTrue, v1.ConditionTrue, v1.ConditionTrue, v1.ConditionFalse, v1.ConditionFalse},
		podPhase:             []v1.PodPhase{v1.PodRunning, v1.PodRunning, v1.PodRunning, v1.PodRunning, v1.PodRunning, v1.PodRunning, v1.PodRunning},
		podDeletionTimestamp: []bool{false, false, false, false, false, true, true},
		resource: &resourceInfo{
			name:     v1.ResourceCPU,
			requests: []resource.Quantity{resource.MustParse("1.0"), resource.MustParse("1.0"), resource.MustParse("1.0"), resource.MustParse("1.0"), resource.MustParse("1.0"), resource.MustParse("1.0"), resource.MustParse("1.0")},
			levels:   [][]int64{{1000, 100}, {1000, 300}, {1000, 500}, {1000, 250}, {1000, 250}},

			targetUtilization:   50,
			expectedUtilization: 28,
			expectedValue:       280,
		},
		container: "container2",
	}
	tc.runTest(t)
}

func TestReplicaCalcTolerance(t *testing.T) {
	tc := replicaCalcTestCase{
		currentReplicas:  3,
		expectedReplicas: 3,
		resource: &resourceInfo{
			name:     v1.ResourceCPU,
			requests: []resource.Quantity{resource.MustParse("0.9"), resource.MustParse("1.0"), resource.MustParse("1.1")},
			levels:   makePodMetricLevels(1010, 1030, 1020),

			targetUtilization:   100,
			expectedUtilization: 102,
			expectedValue:       numContainersPerPod * 1020,
		},
	}
	tc.runTest(t)
}

func TestReplicaCalcToleranceCM(t *testing.T) {
	tc := replicaCalcTestCase{
		currentReplicas:  3,
		expectedReplicas: 3,
		metric: &metricInfo{
			name:          "qps",
			levels:        []int64{20000, 21000, 21000},
			targetUsage:   20000,
			expectedUsage: 20666,
			metricType:    podMetric,
		},
	}
	tc.runTest(t)
}

func TestReplicaCalcToleranceCMObject(t *testing.T) {
	tc := replicaCalcTestCase{
		currentReplicas:  3,
		expectedReplicas: 3,
		metric: &metricInfo{
			name:          "qps",
			levels:        []int64{20666},
			targetUsage:   20000,
			expectedUsage: 20666,
			singleObject: &autoscalingv2.CrossVersionObjectReference{
				Kind:       "Deployment",
				APIVersion: "apps/v1",
				Name:       "some-deployment",
			},
		},
	}
	tc.runTest(t)
}

func TestReplicaCalcTolerancePerPodCMObject(t *testing.T) {
	tc := replicaCalcTestCase{
		currentReplicas:  4,
		expectedReplicas: 4,
		metric: &metricInfo{
			metricType:        objectPerPodMetric,
			name:              "qps",
			levels:            []int64{20166},
			perPodTargetUsage: 5000,
			expectedUsage:     5042,
			singleObject: &autoscalingv2.CrossVersionObjectReference{
				Kind:       "Deployment",
				APIVersion: "apps/v1",
				Name:       "some-deployment",
			},
		},
	}
	tc.runTest(t)
}

func TestReplicaCalcToleranceCMExternal(t *testing.T) {
	tc := replicaCalcTestCase{
		currentReplicas:  3,
		expectedReplicas: 3,
		metric: &metricInfo{
			name:          "qps",
			levels:        []int64{8600},
			targetUsage:   8888,
			expectedUsage: 8600,
			selector:      &metav1.LabelSelector{MatchLabels: map[string]string{"label": "value"}},
			metricType:    externalMetric,
		},
	}
	tc.runTest(t)
}

func TestReplicaCalcTolerancePerPodCMExternal(t *testing.T) {
	tc := replicaCalcTestCase{
		currentReplicas:  3,
		expectedReplicas: 3,
		metric: &metricInfo{
			name:              "qps",
			levels:            []int64{8600},
			perPodTargetUsage: 2900,
			expectedUsage:     2867,
			selector:          &metav1.LabelSelector{MatchLabels: map[string]string{"label": "value"}},
			metricType:        externalPerPodMetric,
		},
	}
	tc.runTest(t)
}

func TestReplicaCalcSuperfluousMetrics(t *testing.T) {
	tc := replicaCalcTestCase{
		currentReplicas:  4,
		expectedReplicas: 24,
		resource: &resourceInfo{
			name:                v1.ResourceCPU,
			requests:            []resource.Quantity{resource.MustParse("1.0"), resource.MustParse("1.0"), resource.MustParse("1.0"), resource.MustParse("1.0")},
			levels:              makePodMetricLevels(4000, 9500, 3000, 7000, 3200, 2000),
			targetUtilization:   100,
			expectedUtilization: 587,
			expectedValue:       numContainersPerPod * 5875,
		},
	}
	tc.runTest(t)
}

func TestReplicaCalcMissingMetrics(t *testing.T) {
	tc := replicaCalcTestCase{
		currentReplicas:  4,
		expectedReplicas: 3,
		resource: &resourceInfo{
			name:     v1.ResourceCPU,
			requests: []resource.Quantity{resource.MustParse("1.0"), resource.MustParse("1.0"), resource.MustParse("1.0"), resource.MustParse("1.0")},
			levels:   makePodMetricLevels(400, 95),

			targetUtilization:   100,
			expectedUtilization: 24,
			expectedValue:       495, // numContainersPerPod * 247, for sufficiently large values of 247
		},
	}
	tc.runTest(t)
}

func TestReplicaCalcEmptyMetrics(t *testing.T) {
	tc := replicaCalcTestCase{
		currentReplicas: 4,
		expectedError:   fmt.Errorf("unable to get metrics for resource cpu: no metrics returned from resource metrics API"),
		resource: &resourceInfo{
			name:     v1.ResourceCPU,
			requests: []resource.Quantity{resource.MustParse("1.0"), resource.MustParse("1.0"), resource.MustParse("1.0")},
			levels:   makePodMetricLevels(),

			targetUtilization: 100,
		},
	}
	tc.runTest(t)
}

func TestReplicaCalcEmptyCPURequest(t *testing.T) {
	tc := replicaCalcTestCase{
		currentReplicas: 1,
		expectedError:   fmt.Errorf("missing request for"),
		resource: &resourceInfo{
			name:     v1.ResourceCPU,
			requests: []resource.Quantity{},
			levels:   makePodMetricLevels(200),

			targetUtilization: 100,
		},
	}
	tc.runTest(t)
}

func TestPlainMetricReplicaCalcMissingMetricsNoChangeEq(t *testing.T) {
	tc := replicaCalcTestCase{
		currentReplicas:  5,
		expectedReplicas: 5,
		metric: &metricInfo{
			name:          "qps",
			levels:        []int64{20000, 19000, 21000},
			targetUsage:   20000,
			expectedUsage: 20000,
			metricType:    podMetric,
		},
	}
	tc.runTest(t)
}

func TestReplicaCalcMissingMetricsNoChangeEq(t *testing.T) {
	tc := replicaCalcTestCase{
		currentReplicas:  2,
		expectedReplicas: 2,
		resource: &resourceInfo{
			name:     v1.ResourceCPU,
			requests: []resource.Quantity{resource.MustParse("1.0"), resource.MustParse("1.0")},
			levels:   makePodMetricLevels(1000),

			targetUtilization:   100,
			expectedUtilization: 100,
			expectedValue:       numContainersPerPod * 1000,
		},
	}
	tc.runTest(t)
}

func TestReplicaCalcMissingMetricsNoChangeGt(t *testing.T) {
	tc := replicaCalcTestCase{
		currentReplicas:  2,
		expectedReplicas: 2,
		resource: &resourceInfo{
			name:     v1.ResourceCPU,
			requests: []resource.Quantity{resource.MustParse("1.0"), resource.MustParse("1.0")},
			levels:   makePodMetricLevels(1900),

			targetUtilization:   100,
			expectedUtilization: 190,
			expectedValue:       numContainersPerPod * 1900,
		},
	}
	tc.runTest(t)
}

func TestReplicaCalcMissingMetricsNoChangeLt(t *testing.T) {
	tc := replicaCalcTestCase{
		currentReplicas:  2,
		expectedReplicas: 2,
		resource: &resourceInfo{
			name:     v1.ResourceCPU,
			requests: []resource.Quantity{resource.MustParse("1.0"), resource.MustParse("1.0")},
			levels:   makePodMetricLevels(600),

			targetUtilization:   100,
			expectedUtilization: 60,
			expectedValue:       numContainersPerPod * 600,
		},
	}
	tc.runTest(t)
}

func TestReplicaCalcMissingMetricsUnreadyChange(t *testing.T) {
	tc := replicaCalcTestCase{
		currentReplicas:  3,
		expectedReplicas: 3,
		podReadiness:     []v1.ConditionStatus{v1.ConditionFalse, v1.ConditionTrue, v1.ConditionTrue},
		resource: &resourceInfo{
			name:     v1.ResourceCPU,
			requests: []resource.Quantity{resource.MustParse("1.0"), resource.MustParse("1.0"), resource.MustParse("1.0")},
			levels:   makePodMetricLevels(100, 450),

			targetUtilization:   50,
			expectedUtilization: 45,
			expectedValue:       numContainersPerPod * 450,
		},
	}
	tc.runTest(t)
}

func TestReplicaCalcMissingMetricsHotCpuNoChange(t *testing.T) {
	tc := replicaCalcTestCase{
		currentReplicas:  3,
		expectedReplicas: 3,
		podStartTime:     []metav1.Time{hotCPUCreationTime(), coolCPUCreationTime(), coolCPUCreationTime()},
		resource: &resourceInfo{
			name:     v1.ResourceCPU,
			requests: []resource.Quantity{resource.MustParse("1.0"), resource.MustParse("1.0"), resource.MustParse("1.0")},
			levels:   makePodMetricLevels(100, 450),

			targetUtilization:   50,
			expectedUtilization: 45,
			expectedValue:       numContainersPerPod * 450,
		},
	}
	tc.runTest(t)
}

func TestReplicaCalcMissingMetricsUnreadyScaleUp(t *testing.T) {
	tc := replicaCalcTestCase{
		currentReplicas:  3,
		expectedReplicas: 4,
		podReadiness:     []v1.ConditionStatus{v1.ConditionFalse, v1.ConditionTrue, v1.ConditionTrue},
		resource: &resourceInfo{
			name:     v1.ResourceCPU,
			requests: []resource.Quantity{resource.MustParse("1.0"), resource.MustParse("1.0"), resource.MustParse("1.0")},
			levels:   makePodMetricLevels(100, 2000),

			targetUtilization:   50,
			expectedUtilization: 200,
			expectedValue:       numContainersPerPod * 2000,
		},
	}
	tc.runTest(t)
}

func TestReplicaCalcMissingMetricsHotCpuScaleUp(t *testing.T) {
	tc := replicaCalcTestCase{
		currentReplicas:  3,
		expectedReplicas: 4,
		podReadiness:     []v1.ConditionStatus{v1.ConditionFalse, v1.ConditionTrue, v1.ConditionTrue},
		podStartTime:     []metav1.Time{hotCPUCreationTime(), coolCPUCreationTime(), coolCPUCreationTime()},
		resource: &resourceInfo{
			name:     v1.ResourceCPU,
			requests: []resource.Quantity{resource.MustParse("1.0"), resource.MustParse("1.0"), resource.MustParse("1.0")},
			levels:   makePodMetricLevels(100, 2000),

			targetUtilization:   50,
			expectedUtilization: 200,
			expectedValue:       numContainersPerPod * 2000,
		},
	}
	tc.runTest(t)
}

func TestReplicaCalcMissingMetricsUnreadyScaleDown(t *testing.T) {
	tc := replicaCalcTestCase{
		currentReplicas:  4,
		expectedReplicas: 3,
		podReadiness:     []v1.ConditionStatus{v1.ConditionFalse, v1.ConditionTrue, v1.ConditionTrue, v1.ConditionTrue},
		resource: &resourceInfo{
			name:     v1.ResourceCPU,
			requests: []resource.Quantity{resource.MustParse("1.0"), resource.MustParse("1.0"), resource.MustParse("1.0"), resource.MustParse("1.0")},
			levels:   makePodMetricLevels(100, 100, 100),

			targetUtilization:   50,
			expectedUtilization: 10,
			expectedValue:       numContainersPerPod * 100,
		},
	}
	tc.runTest(t)
}

func TestReplicaCalcMissingMetricsScaleDownTargetOver100(t *testing.T) {
	tc := replicaCalcTestCase{
		currentReplicas:  4,
		expectedReplicas: 2,
		podReadiness:     []v1.ConditionStatus{v1.ConditionFalse, v1.ConditionTrue, v1.ConditionTrue, v1.ConditionTrue},
		resource: &resourceInfo{
			name:     v1.ResourceCPU,
			requests: []resource.Quantity{resource.MustParse("1.0"), resource.MustParse("1.0"), resource.MustParse("2.0"), resource.MustParse("2.0")},
			levels:   makePodMetricLevels(200, 100, 100),

			targetUtilization:   300,
			expectedUtilization: 6,
			expectedValue:       numContainersPerPod * 100,
		},
	}
	tc.runTest(t)
}

func TestReplicaCalcDuringRollingUpdateWithMaxSurge(t *testing.T) {
	tc := replicaCalcTestCase{
		currentReplicas:  2,
		expectedReplicas: 2,
		podPhase:         []v1.PodPhase{v1.PodRunning, v1.PodRunning, v1.PodRunning},
		resource: &resourceInfo{
			name:     v1.ResourceCPU,
			requests: []resource.Quantity{resource.MustParse("1.0"), resource.MustParse("1.0"), resource.MustParse("1.0")},
			levels:   makePodMetricLevels(100, 100),

			targetUtilization:   50,
			expectedUtilization: 10,
			expectedValue:       numContainersPerPod * 100,
		},
	}
	tc.runTest(t)
}

func TestReplicaCalcDuringRollingUpdateWithMaxSurgeCM(t *testing.T) {
	tc := replicaCalcTestCase{
		currentReplicas:  2,
		expectedReplicas: 2,
		podPhase:         []v1.PodPhase{v1.PodRunning, v1.PodRunning, v1.PodRunning},
		metric: &metricInfo{
			name:          "qps",
			levels:        []int64{10000, 10000},
			targetUsage:   17000,
			expectedUsage: 10000,
			metricType:    podMetric,
		},
	}
	tc.runTest(t)
}

// TestComputedToleranceAlgImplementation is a regression test which
// back-calculates a minimal percentage for downscaling based on a small percentage
// increase in pod utilization which is calibrated against the tolerance value.
func TestReplicaCalcComputedToleranceAlgImplementation(t *testing.T) {

	startPods := int32(10)
	// 150 mCPU per pod.
	totalUsedCPUOfAllPods := int64(startPods * 150)
	// Each pod starts out asking for 2X what is really needed.
	// This means we will have a 50% ratio of used/requested
	totalRequestedCPUOfAllPods := int32(2 * totalUsedCPUOfAllPods)
	requestedToUsed := float64(totalRequestedCPUOfAllPods / int32(totalUsedCPUOfAllPods))
	// Spread the amount we ask over 10 pods.  We can add some jitter later in reportedLevels.
	perPodRequested := totalRequestedCPUOfAllPods / startPods

	// Force a minimal scaling event by satisfying  (tolerance < 1 - resourcesUsedRatio).
	target := math.Abs(1/(requestedToUsed*(1-defaultTestingTolerance))) + .01
	finalCPUPercentTarget := int32(target * 100)
	resourcesUsedRatio := float64(totalUsedCPUOfAllPods) / float64(float64(totalRequestedCPUOfAllPods)*target)

	// i.e. .60 * 20 -> scaled down expectation.
	finalPods := int32(math.Ceil(resourcesUsedRatio * float64(startPods)))

	// To breach tolerance we will create a utilization ratio difference of tolerance to usageRatioToleranceValue)
	tc := replicaCalcTestCase{
		currentReplicas:  startPods,
		expectedReplicas: finalPods,
		resource: &resourceInfo{
			name: v1.ResourceCPU,
			levels: makePodMetricLevels(
				totalUsedCPUOfAllPods/10,
				totalUsedCPUOfAllPods/10,
				totalUsedCPUOfAllPods/10,
				totalUsedCPUOfAllPods/10,
				totalUsedCPUOfAllPods/10,
				totalUsedCPUOfAllPods/10,
				totalUsedCPUOfAllPods/10,
				totalUsedCPUOfAllPods/10,
				totalUsedCPUOfAllPods/10,
				totalUsedCPUOfAllPods/10,
			),
			requests: []resource.Quantity{
				resource.MustParse(fmt.Sprint(perPodRequested+100) + "m"),
				resource.MustParse(fmt.Sprint(perPodRequested-100) + "m"),
				resource.MustParse(fmt.Sprint(perPodRequested+10) + "m"),
				resource.MustParse(fmt.Sprint(perPodRequested-10) + "m"),
				resource.MustParse(fmt.Sprint(perPodRequested+2) + "m"),
				resource.MustParse(fmt.Sprint(perPodRequested-2) + "m"),
				resource.MustParse(fmt.Sprint(perPodRequested+1) + "m"),
				resource.MustParse(fmt.Sprint(perPodRequested-1) + "m"),
				resource.MustParse(fmt.Sprint(perPodRequested) + "m"),
				resource.MustParse(fmt.Sprint(perPodRequested) + "m"),
			},

			targetUtilization:   finalCPUPercentTarget,
			expectedUtilization: int32(totalUsedCPUOfAllPods*100) / totalRequestedCPUOfAllPods,
			expectedValue:       numContainersPerPod * totalUsedCPUOfAllPods / 10,
		},
	}

	tc.runTest(t)

	// Reuse the data structure above, now testing "unscaling".
	// Now, we test that no scaling happens if we are in a very close margin to the tolerance
	target = math.Abs(1/(requestedToUsed*(1-defaultTestingTolerance))) + .004
	finalCPUPercentTarget = int32(target * 100)
	tc.resource.targetUtilization = finalCPUPercentTarget
	tc.currentReplicas = startPods
	tc.expectedReplicas = startPods
	tc.runTest(t)
}

func TestGroupPods(t *testing.T) {
	tests := []struct {
		name                string
		pods                []*v1.Pod
		metrics             metricsclient.PodMetricsInfo
		resource            v1.ResourceName
		expectReadyPodCount int
		expectUnreadyPods   sets.Set[string]
		expectMissingPods   sets.Set[string]
		expectIgnoredPods   sets.Set[string]
	}{
		{
			name:                "void",
			pods:                []*v1.Pod{},
			metrics:             metricsclient.PodMetricsInfo{},
			resource:            v1.ResourceCPU,
			expectReadyPodCount: 0,
			expectUnreadyPods:   sets.New[string](),
			expectMissingPods:   sets.New[string](),
			expectIgnoredPods:   sets.New[string](),
		}, {
			name: "count in a ready pod - memory",
			pods: []*v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "bentham",
					},
					Status: v1.PodStatus{
						Phase: v1.PodSucceeded,
					},
				},
			},
			metrics: metricsclient.PodMetricsInfo{
				"bentham": metricsclient.PodMetric{Value: 1, Timestamp: time.Now(), Window: time.Minute},
			},
			resource:            v1.ResourceMemory,
			expectReadyPodCount: 1,
			expectUnreadyPods:   sets.New[string](),
			expectMissingPods:   sets.New[string](),
			expectIgnoredPods:   sets.New[string](),
		}, {
			name: "unready a pod without ready condition - CPU",
			pods: []*v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "lucretius",
					},
					Status: v1.PodStatus{
						Phase: v1.PodSucceeded,
						StartTime: &metav1.Time{
							Time: time.Now(),
						},
					},
				},
			},
			metrics: metricsclient.PodMetricsInfo{
				"lucretius": metricsclient.PodMetric{Value: 1},
			},
			resource:            v1.ResourceCPU,
			expectReadyPodCount: 0,
			expectUnreadyPods:   sets.New[string]("lucretius"),
			expectMissingPods:   sets.New[string](),
			expectIgnoredPods:   sets.New[string](),
		}, {
			name: "count in a ready pod with fresh metrics during initialization period - CPU",
			pods: []*v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "bentham",
					},
					Status: v1.PodStatus{
						Phase: v1.PodSucceeded,
						StartTime: &metav1.Time{
							Time: time.Now().Add(-1 * time.Minute),
						},
						Conditions: []v1.PodCondition{
							{
								Type:               v1.PodReady,
								LastTransitionTime: metav1.Time{Time: time.Now().Add(-30 * time.Second)},
								Status:             v1.ConditionTrue,
							},
						},
					},
				},
			},
			metrics: metricsclient.PodMetricsInfo{
				"bentham": metricsclient.PodMetric{Value: 1, Timestamp: time.Now(), Window: 30 * time.Second},
			},
			resource:            v1.ResourceCPU,
			expectReadyPodCount: 1,
			expectUnreadyPods:   sets.New[string](),
			expectMissingPods:   sets.New[string](),
			expectIgnoredPods:   sets.New[string](),
		}, {
			name: "unready a ready pod without fresh metrics during initialization period - CPU",
			pods: []*v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "bentham",
					},
					Status: v1.PodStatus{
						Phase: v1.PodSucceeded,
						StartTime: &metav1.Time{
							Time: time.Now().Add(-1 * time.Minute),
						},
						Conditions: []v1.PodCondition{
							{
								Type:               v1.PodReady,
								LastTransitionTime: metav1.Time{Time: time.Now().Add(-30 * time.Second)},
								Status:             v1.ConditionTrue,
							},
						},
					},
				},
			},
			metrics: metricsclient.PodMetricsInfo{
				"bentham": metricsclient.PodMetric{Value: 1, Timestamp: time.Now(), Window: 60 * time.Second},
			},
			resource:            v1.ResourceCPU,
			expectReadyPodCount: 0,
			expectUnreadyPods:   sets.New[string]("bentham"),
			expectMissingPods:   sets.New[string](),
			expectIgnoredPods:   sets.New[string](),
		}, {
			name: "unready an unready pod during initialization period - CPU",
			pods: []*v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "lucretius",
					},
					Status: v1.PodStatus{
						Phase: v1.PodSucceeded,
						StartTime: &metav1.Time{
							Time: time.Now().Add(-10 * time.Minute),
						},
						Conditions: []v1.PodCondition{
							{
								Type:               v1.PodReady,
								LastTransitionTime: metav1.Time{Time: time.Now().Add(-9*time.Minute - 54*time.Second)},
								Status:             v1.ConditionFalse,
							},
						},
					},
				},
			},
			metrics: metricsclient.PodMetricsInfo{
				"lucretius": metricsclient.PodMetric{Value: 1},
			},
			resource:            v1.ResourceCPU,
			expectReadyPodCount: 0,
			expectUnreadyPods:   sets.New[string]("lucretius"),
			expectMissingPods:   sets.New[string](),
			expectIgnoredPods:   sets.New[string](),
		}, {
			name: "count in a ready pod without fresh metrics after initialization period - CPU",
			pods: []*v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "bentham",
					},
					Status: v1.PodStatus{
						Phase: v1.PodSucceeded,
						StartTime: &metav1.Time{
							Time: time.Now().Add(-3 * time.Minute),
						},
						Conditions: []v1.PodCondition{
							{
								Type:               v1.PodReady,
								LastTransitionTime: metav1.Time{Time: time.Now().Add(-3 * time.Minute)},
								Status:             v1.ConditionTrue,
							},
						},
					},
				},
			},
			metrics: metricsclient.PodMetricsInfo{
				"bentham": metricsclient.PodMetric{Value: 1, Timestamp: time.Now().Add(-2 * time.Minute), Window: time.Minute},
			},
			resource:            v1.ResourceCPU,
			expectReadyPodCount: 1,
			expectUnreadyPods:   sets.New[string](),
			expectMissingPods:   sets.New[string](),
			expectIgnoredPods:   sets.New[string](),
		}, {
			name: "count in an unready pod that was ready after initialization period - CPU",
			pods: []*v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "lucretius",
					},
					Status: v1.PodStatus{
						Phase: v1.PodSucceeded,
						StartTime: &metav1.Time{
							Time: time.Now().Add(-10 * time.Minute),
						},
						Conditions: []v1.PodCondition{
							{
								Type:               v1.PodReady,
								LastTransitionTime: metav1.Time{Time: time.Now().Add(-9 * time.Minute)},
								Status:             v1.ConditionFalse,
							},
						},
					},
				},
			},
			metrics: metricsclient.PodMetricsInfo{
				"lucretius": metricsclient.PodMetric{Value: 1},
			},
			resource:            v1.ResourceCPU,
			expectReadyPodCount: 1,
			expectUnreadyPods:   sets.New[string](),
			expectMissingPods:   sets.New[string](),
			expectIgnoredPods:   sets.New[string](),
		}, {
			name: "unready pod that has never been ready after initialization period - CPU",
			pods: []*v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "lucretius",
					},
					Status: v1.PodStatus{
						Phase: v1.PodSucceeded,
						StartTime: &metav1.Time{
							Time: time.Now().Add(-10 * time.Minute),
						},
						Conditions: []v1.PodCondition{
							{
								Type:               v1.PodReady,
								LastTransitionTime: metav1.Time{Time: time.Now().Add(-9*time.Minute - 50*time.Second)},
								Status:             v1.ConditionFalse,
							},
						},
					},
				},
			},
			metrics: metricsclient.PodMetricsInfo{
				"lucretius": metricsclient.PodMetric{Value: 1},
			},
			resource:            v1.ResourceCPU,
			expectReadyPodCount: 1,
			expectUnreadyPods:   sets.New[string](),
			expectMissingPods:   sets.New[string](),
			expectIgnoredPods:   sets.New[string](),
		}, {
			name: "a missing pod",
			pods: []*v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "epicurus",
					},
					Status: v1.PodStatus{
						Phase: v1.PodSucceeded,
						StartTime: &metav1.Time{
							Time: time.Now().Add(-3 * time.Minute),
						},
					},
				},
			},
			metrics:             metricsclient.PodMetricsInfo{},
			resource:            v1.ResourceCPU,
			expectReadyPodCount: 0,
			expectUnreadyPods:   sets.New[string](),
			expectMissingPods:   sets.New[string]("epicurus"),
			expectIgnoredPods:   sets.New[string](),
		}, {
			name: "several pods",
			pods: []*v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "lucretius",
					},
					Status: v1.PodStatus{
						Phase: v1.PodSucceeded,
						StartTime: &metav1.Time{
							Time: time.Now(),
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "niccolo",
					},
					Status: v1.PodStatus{
						Phase: v1.PodSucceeded,
						StartTime: &metav1.Time{
							Time: time.Now().Add(-3 * time.Minute),
						},
						Conditions: []v1.PodCondition{
							{
								Type:               v1.PodReady,
								LastTransitionTime: metav1.Time{Time: time.Now().Add(-3 * time.Minute)},
								Status:             v1.ConditionTrue,
							},
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "epicurus",
					},
					Status: v1.PodStatus{
						Phase: v1.PodSucceeded,
						StartTime: &metav1.Time{
							Time: time.Now().Add(-3 * time.Minute),
						},
					},
				},
			},
			metrics: metricsclient.PodMetricsInfo{
				"lucretius": metricsclient.PodMetric{Value: 1},
				"niccolo":   metricsclient.PodMetric{Value: 1},
			},
			resource:            v1.ResourceCPU,
			expectReadyPodCount: 1,
			expectUnreadyPods:   sets.New[string]("lucretius"),
			expectMissingPods:   sets.New[string]("epicurus"),
			expectIgnoredPods:   sets.New[string](),
		}, {
			name: "pending pods are unreadied",
			pods: []*v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "unscheduled",
					},
					Status: v1.PodStatus{
						Phase: v1.PodPending,
					},
				},
			},
			metrics:             metricsclient.PodMetricsInfo{},
			resource:            v1.ResourceCPU,
			expectReadyPodCount: 0,
			expectUnreadyPods:   sets.New[string]("unscheduled"),
			expectMissingPods:   sets.New[string](),
			expectIgnoredPods:   sets.New[string](),
		}, {
			name: "ignore pods with deletion timestamps",
			pods: []*v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:              "deleted",
						DeletionTimestamp: &metav1.Time{Time: time.Unix(1, 0)},
					},
					Status: v1.PodStatus{
						Phase: v1.PodPending,
					},
				},
			},
			metrics: metricsclient.PodMetricsInfo{
				"deleted": metricsclient.PodMetric{Value: 1},
			},
			resource:            v1.ResourceCPU,
			expectReadyPodCount: 0,
			expectUnreadyPods:   sets.New[string](),
			expectMissingPods:   sets.New[string](),
			expectIgnoredPods:   sets.New[string]("deleted"),
		}, {
			name: "ignore pods in a failed state",
			pods: []*v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "failed",
					},
					Status: v1.PodStatus{
						Phase: v1.PodFailed,
					},
				},
			},
			metrics: metricsclient.PodMetricsInfo{
				"failed": metricsclient.PodMetric{Value: 1},
			},
			resource:            v1.ResourceCPU,
			expectReadyPodCount: 0,
			expectUnreadyPods:   sets.New[string](),
			expectMissingPods:   sets.New[string](),
			expectIgnoredPods:   sets.New[string]("failed"),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			readyPodCount, unreadyPods, missingPods, ignoredPods := groupPods(tc.pods, tc.metrics, tc.resource, defaultTestingCPUInitializationPeriod, defaultTestingDelayOfInitialReadinessStatus)
			if readyPodCount != tc.expectReadyPodCount {
				t.Errorf("%s got readyPodCount %d, expected %d", tc.name, readyPodCount, tc.expectReadyPodCount)
			}
			if !unreadyPods.Equal(tc.expectUnreadyPods) {
				t.Errorf("%s got unreadyPods %v, expected %v", tc.name, unreadyPods, tc.expectUnreadyPods)
			}
			if !missingPods.Equal(tc.expectMissingPods) {
				t.Errorf("%s got missingPods %v, expected %v", tc.name, missingPods, tc.expectMissingPods)
			}
			if !ignoredPods.Equal(tc.expectIgnoredPods) {
				t.Errorf("%s got ignoredPods %v, expected %v", tc.name, ignoredPods, tc.expectIgnoredPods)
			}
		})
	}
}

func TestCalculatePodRequests(t *testing.T) {
	containerRestartPolicyAlways := v1.ContainerRestartPolicyAlways
	testPod := "test-pod"

	tests := []struct {
		name             string
		pods             []*v1.Pod
		container        string
		resource         v1.ResourceName
		expectedRequests map[string]int64
		expectedError    error
	}{
		{
			name:             "void",
			pods:             []*v1.Pod{},
			container:        "",
			resource:         v1.ResourceCPU,
			expectedRequests: map[string]int64{},
			expectedError:    nil,
		},
		{
			name: "pod with regular containers",
			pods: []*v1.Pod{{
				ObjectMeta: metav1.ObjectMeta{
					Name:      testPod,
					Namespace: testNamespace,
				},
				Spec: v1.PodSpec{
					Containers: []v1.Container{
						{Name: "container1", Resources: v1.ResourceRequirements{Requests: v1.ResourceList{v1.ResourceCPU: *resource.NewMilliQuantity(100, resource.DecimalSI)}}},
						{Name: "container2", Resources: v1.ResourceRequirements{Requests: v1.ResourceList{v1.ResourceCPU: *resource.NewMilliQuantity(50, resource.DecimalSI)}}},
					},
				},
			}},
			container:        "",
			resource:         v1.ResourceCPU,
			expectedRequests: map[string]int64{testPod: 150},
			expectedError:    nil,
		},
		{
			name: "calculate requests with special container",
			pods: []*v1.Pod{{
				ObjectMeta: metav1.ObjectMeta{
					Name:      testPod,
					Namespace: testNamespace,
				},
				Spec: v1.PodSpec{
					Containers: []v1.Container{
						{Name: "container1", Resources: v1.ResourceRequirements{Requests: v1.ResourceList{v1.ResourceCPU: *resource.NewMilliQuantity(100, resource.DecimalSI)}}},
						{Name: "container2", Resources: v1.ResourceRequirements{Requests: v1.ResourceList{v1.ResourceCPU: *resource.NewMilliQuantity(50, resource.DecimalSI)}}},
					},
				},
			}},
			container:        "container1",
			resource:         v1.ResourceCPU,
			expectedRequests: map[string]int64{testPod: 100},
			expectedError:    nil,
		},
		{
			name: "container missing requests",
			pods: []*v1.Pod{{
				ObjectMeta: metav1.ObjectMeta{
					Name:      testPod,
					Namespace: testNamespace,
				},
				Spec: v1.PodSpec{
					Containers: []v1.Container{
						{Name: "container1"},
					},
				},
			}},
			container:        "",
			resource:         v1.ResourceCPU,
			expectedRequests: nil,
			expectedError:    fmt.Errorf("missing request for %s in container %s of Pod %s", v1.ResourceCPU, "container1", testPod),
		},
		{
			name: "pod with restartable init containers",
			pods: []*v1.Pod{{
				ObjectMeta: metav1.ObjectMeta{
					Name:      testPod,
					Namespace: testNamespace,
				},
				Spec: v1.PodSpec{
					Containers: []v1.Container{
						{Name: "container1", Resources: v1.ResourceRequirements{Requests: v1.ResourceList{v1.ResourceCPU: *resource.NewMilliQuantity(100, resource.DecimalSI)}}},
					},
					InitContainers: []v1.Container{
						{Name: "init-container1", Resources: v1.ResourceRequirements{Requests: v1.ResourceList{v1.ResourceCPU: *resource.NewMilliQuantity(20, resource.DecimalSI)}}},
						{Name: "restartable-container1", RestartPolicy: &containerRestartPolicyAlways, Resources: v1.ResourceRequirements{Requests: v1.ResourceList{v1.ResourceCPU: *resource.NewMilliQuantity(50, resource.DecimalSI)}}},
					},
				},
			}},
			container:        "",
			resource:         v1.ResourceCPU,
			expectedRequests: map[string]int64{testPod: 150},
			expectedError:    nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			requests, err := calculatePodRequests(tc.pods, tc.container, tc.resource)
			assert.Equal(t, tc.expectedRequests, requests, "requests should be as expected")
			assert.Equal(t, tc.expectedError, err, "error should be as expected")
		})
	}
}
