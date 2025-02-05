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

package autoscaling

import (
	"context"
	"time"

	"github.com/onsi/ginkgo/v2"
	"k8s.io/pod-security-admission/api"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/kubernetes/test/e2e/feature"
	"k8s.io/kubernetes/test/e2e/framework"
	e2eautoscaling "k8s.io/kubernetes/test/e2e/framework/autoscaling"
)

const (
	titleUp                 = "Should scale from 1 pod to 3 pods and then from 3 pods to 5 pods"
	titleDown               = "Should scale from 5 pods to 3 pods and then from 3 pods to 1 pod"
	titleAverageUtilization = " using Average Utilization for aggregation"
	titleAverageValue       = " using Average Value for aggregation"
	valueMetricType         = autoscalingv2.AverageValueMetricType
	utilizationMetricType   = autoscalingv2.UtilizationMetricType
	cpuResource             = v1.ResourceCPU
	memResource             = v1.ResourceMemory
)

var _ = SIGDescribe(feature.HPA, "Horizontal pod autoscaling (scale resource: CPU)", func() {
	f := framework.NewDefaultFramework("horizontal-pod-autoscaling")
	f.NamespacePodSecurityLevel = api.LevelBaseline

	f.Describe(framework.WithSerial(), framework.WithSlow(), "Deployment (Pod Resource)", func() {
		ginkgo.It(titleUp+titleAverageUtilization, func(ctx context.Context) {
			scaleUp(ctx, "test-deployment", e2eautoscaling.KindDeployment, cpuResource, utilizationMetricType, false, f)
		})
		ginkgo.It(titleDown+titleAverageUtilization, func(ctx context.Context) {
			scaleDown(ctx, "test-deployment", e2eautoscaling.KindDeployment, cpuResource, utilizationMetricType, false, f)
		})
		ginkgo.It(titleUp+titleAverageValue, func(ctx context.Context) {
			scaleUp(ctx, "test-deployment", e2eautoscaling.KindDeployment, cpuResource, valueMetricType, false, f)
		})
	})

	f.Describe(framework.WithSerial(), framework.WithSlow(), "Deployment (Container Resource)", func() {
		ginkgo.It(titleUp+titleAverageUtilization, func(ctx context.Context) {
			scaleUpContainerResource(ctx, "test-deployment", e2eautoscaling.KindDeployment, cpuResource, utilizationMetricType, f)
		})
		ginkgo.It(titleUp+titleAverageValue, func(ctx context.Context) {
			scaleUpContainerResource(ctx, "test-deployment", e2eautoscaling.KindDeployment, cpuResource, valueMetricType, f)
		})
	})

	f.Describe(framework.WithSerial(), framework.WithSlow(), "ReplicaSet", func() {
		ginkgo.It(titleUp, func(ctx context.Context) {
			scaleUp(ctx, "rs", e2eautoscaling.KindReplicaSet, cpuResource, utilizationMetricType, false, f)
		})
		ginkgo.It(titleDown, func(ctx context.Context) {
			scaleDown(ctx, "rs", e2eautoscaling.KindReplicaSet, cpuResource, utilizationMetricType, false, f)
		})
	})

	// These tests take ~20 minutes each.
	f.Describe(framework.WithSerial(), framework.WithSlow(), "ReplicationController", func() {
		ginkgo.It(titleUp+" and verify decision stability", func(ctx context.Context) {
			scaleUp(ctx, "rc", e2eautoscaling.KindRC, cpuResource, utilizationMetricType, true, f)
		})
		ginkgo.It(titleDown+" and verify decision stability", func(ctx context.Context) {
			scaleDown(ctx, "rc", e2eautoscaling.KindRC, cpuResource, utilizationMetricType, true, f)
		})
	})

	f.Describe("ReplicationController light", func() {
		ginkgo.It("Should scale from 1 pod to 2 pods", func(ctx context.Context) {
			st := &HPAScaleTest{
				initPods:         1,
				initCPUTotal:     150,
				perPodCPURequest: 200,
				targetValue:      50,
				minPods:          1,
				maxPods:          2,
				firstScale:       2,
				resourceType:     cpuResource,
				metricTargetType: utilizationMetricType,
			}
			st.run(ctx, "rc-light", e2eautoscaling.KindRC, f)
		})
		f.It(f.WithSlow(), "Should scale from 2 pods to 1 pod", func(ctx context.Context) {
			st := &HPAScaleTest{
				initPods:         2,
				initCPUTotal:     50,
				perPodCPURequest: 200,
				targetValue:      50,
				minPods:          1,
				maxPods:          2,
				firstScale:       1,
				resourceType:     cpuResource,
				metricTargetType: utilizationMetricType,
			}
			st.run(ctx, "rc-light", e2eautoscaling.KindRC, f)
		})
	})

	f.Describe(framework.WithSerial(), framework.WithSlow(), "ReplicaSet with idle sidecar (ContainerResource use case)", func() {
		// ContainerResource CPU autoscaling on idle sidecar
		ginkgo.It(titleUp+" on a busy application with an idle sidecar container", func(ctx context.Context) {
			scaleOnIdleSideCar(ctx, "rs", e2eautoscaling.KindReplicaSet, cpuResource, utilizationMetricType, false, f)
		})

		// ContainerResource CPU autoscaling on busy sidecar
		ginkgo.It("Should not scale up on a busy sidecar with an idle application", func(ctx context.Context) {
			doNotScaleOnBusySidecar(ctx, "rs", e2eautoscaling.KindReplicaSet, cpuResource, utilizationMetricType, true, f)
		})
	})

	f.Describe("CustomResourceDefinition", func() {
		ginkgo.It("Should scale with a CRD targetRef", func(ctx context.Context) {
			scaleTest := &HPAScaleTest{
				initPods:         1,
				initCPUTotal:     150,
				perPodCPURequest: 200,
				targetValue:      50,
				minPods:          1,
				maxPods:          2,
				firstScale:       2,
				resourceType:     cpuResource,
				metricTargetType: utilizationMetricType,
			}
			scaleTest.run(ctx, "foo-crd", e2eautoscaling.KindCRD, f)
		})
	})
})

var _ = SIGDescribe(feature.HPA, "Horizontal pod autoscaling (scale resource: Memory)", func() {
	f := framework.NewDefaultFramework("horizontal-pod-autoscaling")
	f.NamespacePodSecurityLevel = api.LevelBaseline

	f.Describe(framework.WithSerial(), framework.WithSlow(), "Deployment (Pod Resource)", func() {
		ginkgo.It(titleUp+titleAverageUtilization, func(ctx context.Context) {
			scaleUp(ctx, "test-deployment", e2eautoscaling.KindDeployment, memResource, utilizationMetricType, false, f)
		})
		ginkgo.It(titleUp+titleAverageValue, func(ctx context.Context) {
			scaleUp(ctx, "test-deployment", e2eautoscaling.KindDeployment, memResource, valueMetricType, false, f)
		})
	})

	f.Describe(framework.WithSerial(), framework.WithSlow(), "Deployment (Container Resource)", func() {
		ginkgo.It(titleUp+titleAverageUtilization, func(ctx context.Context) {
			scaleUpContainerResource(ctx, "test-deployment", e2eautoscaling.KindDeployment, memResource, utilizationMetricType, f)
		})
		ginkgo.It(titleUp+titleAverageValue, func(ctx context.Context) {
			scaleUpContainerResource(ctx, "test-deployment", e2eautoscaling.KindDeployment, memResource, valueMetricType, f)
		})
	})
})

// HPAScaleTest struct is used by the scale(...) function.
type HPAScaleTest struct {
	initPods         int
	initCPUTotal     int
	initMemTotal     int
	perPodCPURequest int64
	perPodMemRequest int64
	targetValue      int32
	minPods          int32
	maxPods          int32
	firstScale       int
	firstScaleStasis time.Duration
	cpuBurst         int
	memBurst         int
	secondScale      int32
	resourceType     v1.ResourceName
	metricTargetType autoscalingv2.MetricTargetType
}

// run is a method which runs an HPA lifecycle, from a starting state, to an expected
// The initial state is defined by the initPods parameter.
// The first state change is due to the CPU being consumed initially, which HPA responds to by changing pod counts.
// The second state change (optional) is due to the CPU burst parameter, which HPA again responds to.
// TODO The use of 3 states is arbitrary, we could eventually make this test handle "n" states once this test stabilizes.
func (st *HPAScaleTest) run(ctx context.Context, name string, kind schema.GroupVersionKind, f *framework.Framework) {
	const timeToWait = 15 * time.Minute
	initCPUTotal, initMemTotal := 0, 0
	if st.resourceType == cpuResource {
		initCPUTotal = st.initCPUTotal
	} else if st.resourceType == memResource {
		initMemTotal = st.initMemTotal
	}
	rc := e2eautoscaling.NewDynamicResourceConsumer(ctx, name, f.Namespace.Name, kind, st.initPods, initCPUTotal, initMemTotal, 0, st.perPodCPURequest, st.perPodMemRequest, f.ClientSet, f.ScalesGetter, e2eautoscaling.Disable, e2eautoscaling.Idle)
	ginkgo.DeferCleanup(rc.CleanUp)
	hpa := e2eautoscaling.CreateResourceHorizontalPodAutoscaler(ctx, rc, st.resourceType, st.metricTargetType, st.targetValue, st.minPods, st.maxPods)
	ginkgo.DeferCleanup(e2eautoscaling.DeleteHorizontalPodAutoscaler, rc, hpa.Name)

	rc.WaitForReplicas(ctx, st.firstScale, timeToWait)
	if st.firstScaleStasis > 0 {
		rc.EnsureDesiredReplicasInRange(ctx, st.firstScale, st.firstScale+1, st.firstScaleStasis, hpa.Name)
	}
	if st.resourceType == cpuResource && st.cpuBurst > 0 && st.secondScale > 0 {
		rc.ConsumeCPU(st.cpuBurst)
		rc.WaitForReplicas(ctx, int(st.secondScale), timeToWait)
	}
	if st.resourceType == memResource && st.memBurst > 0 && st.secondScale > 0 {
		rc.ConsumeMem(st.memBurst)
		rc.WaitForReplicas(ctx, int(st.secondScale), timeToWait)
	}
}

func scaleUp(ctx context.Context, name string, kind schema.GroupVersionKind, resourceType v1.ResourceName, metricTargetType autoscalingv2.MetricTargetType, checkStability bool, f *framework.Framework) {
	stasis := 0 * time.Minute
	if checkStability {
		stasis = 10 * time.Minute
	}
	st := &HPAScaleTest{
		initPods:         1,
		perPodCPURequest: 500,
		perPodMemRequest: 500,
		targetValue:      getTargetValueByType(100, 20, metricTargetType),
		minPods:          1,
		maxPods:          5,
		firstScale:       3,
		firstScaleStasis: stasis,
		secondScale:      5,
		resourceType:     resourceType,
		metricTargetType: metricTargetType,
	}
	if resourceType == cpuResource {
		st.initCPUTotal = 250
		st.cpuBurst = 700
	}
	if resourceType == memResource {
		st.initMemTotal = 250
		st.memBurst = 700
	}
	st.run(ctx, name, kind, f)
}

func scaleDown(ctx context.Context, name string, kind schema.GroupVersionKind, resourceType v1.ResourceName, metricTargetType autoscalingv2.MetricTargetType, checkStability bool, f *framework.Framework) {
	stasis := 0 * time.Minute
	if checkStability {
		stasis = 10 * time.Minute
	}
	st := &HPAScaleTest{
		initPods:         5,
		perPodCPURequest: 500,
		perPodMemRequest: 500,
		targetValue:      getTargetValueByType(150, 30, metricTargetType),
		minPods:          1,
		maxPods:          5,
		firstScale:       3,
		firstScaleStasis: stasis,
		cpuBurst:         10,
		secondScale:      1,
		resourceType:     resourceType,
		metricTargetType: metricTargetType,
	}
	if resourceType == cpuResource {
		st.initCPUTotal = 325
		st.cpuBurst = 10
	}
	if resourceType == memResource {
		st.initMemTotal = 325
		st.memBurst = 10
	}
	st.run(ctx, name, kind, f)
}

type HPAContainerResourceScaleTest struct {
	initPods               int
	initCPUTotal           int
	initMemTotal           int
	perContainerCPURequest int64
	perContainerMemRequest int64
	targetValue            int32
	minPods                int32
	maxPods                int32
	noScale                bool
	noScaleStasis          time.Duration
	firstScale             int
	firstScaleStasis       time.Duration
	cpuBurst               int
	memBurst               int
	secondScale            int32
	sidecarStatus          e2eautoscaling.SidecarStatusType
	sidecarType            e2eautoscaling.SidecarWorkloadType
	resourceType           v1.ResourceName
	metricTargetType       autoscalingv2.MetricTargetType
}

func (st *HPAContainerResourceScaleTest) run(ctx context.Context, name string, kind schema.GroupVersionKind, f *framework.Framework) {
	const timeToWait = 15 * time.Minute
	initCPUTotal, initMemTotal := 0, 0
	if st.resourceType == cpuResource {
		initCPUTotal = st.initCPUTotal
	} else if st.resourceType == memResource {
		initMemTotal = st.initMemTotal
	}
	rc := e2eautoscaling.NewDynamicResourceConsumer(ctx, name, f.Namespace.Name, kind, st.initPods, initCPUTotal, initMemTotal, 0, st.perContainerCPURequest, st.perContainerMemRequest, f.ClientSet, f.ScalesGetter, st.sidecarStatus, st.sidecarType)
	ginkgo.DeferCleanup(rc.CleanUp)
	hpa := e2eautoscaling.CreateContainerResourceHorizontalPodAutoscaler(ctx, rc, st.resourceType, st.metricTargetType, st.targetValue, st.minPods, st.maxPods)
	ginkgo.DeferCleanup(e2eautoscaling.DeleteContainerResourceHPA, rc, hpa.Name)

	if st.noScale {
		if st.noScaleStasis > 0 {
			rc.EnsureDesiredReplicasInRange(ctx, st.initPods, st.initPods, st.noScaleStasis, hpa.Name)
		}
	} else {
		rc.WaitForReplicas(ctx, st.firstScale, timeToWait)
		if st.firstScaleStasis > 0 {
			rc.EnsureDesiredReplicasInRange(ctx, st.firstScale, st.firstScale+1, st.firstScaleStasis, hpa.Name)
		}
		if st.resourceType == cpuResource && st.cpuBurst > 0 && st.secondScale > 0 {
			rc.ConsumeCPU(st.cpuBurst)
			rc.WaitForReplicas(ctx, int(st.secondScale), timeToWait)
		}
		if st.resourceType == memResource && st.memBurst > 0 && st.secondScale > 0 {
			rc.ConsumeMem(st.memBurst)
			rc.WaitForReplicas(ctx, int(st.secondScale), timeToWait)
		}
	}
}

func scaleUpContainerResource(ctx context.Context, name string, kind schema.GroupVersionKind, resourceType v1.ResourceName, metricTargetType autoscalingv2.MetricTargetType, f *framework.Framework) {
	st := &HPAContainerResourceScaleTest{
		initPods:               1,
		perContainerCPURequest: 500,
		perContainerMemRequest: 500,
		targetValue:            getTargetValueByType(100, 20, metricTargetType),
		minPods:                1,
		maxPods:                5,
		firstScale:             3,
		firstScaleStasis:       0,
		secondScale:            5,
		resourceType:           resourceType,
		metricTargetType:       metricTargetType,
		sidecarStatus:          e2eautoscaling.Disable,
		sidecarType:            e2eautoscaling.Idle,
	}
	if resourceType == cpuResource {
		st.initCPUTotal = 250
		st.cpuBurst = 700
	}
	if resourceType == memResource {
		st.initMemTotal = 250
		st.memBurst = 700
	}
	st.run(ctx, name, kind, f)
}

func scaleOnIdleSideCar(ctx context.Context, name string, kind schema.GroupVersionKind, resourceType v1.ResourceName, metricTargetType autoscalingv2.MetricTargetType, checkStability bool, f *framework.Framework) {
	// Scale up on a busy application with an idle sidecar container
	stasis := 0 * time.Minute
	if checkStability {
		stasis = 10 * time.Minute
	}
	st := &HPAContainerResourceScaleTest{
		initPods:               1,
		initCPUTotal:           125,
		perContainerCPURequest: 250,
		targetValue:            20,
		minPods:                1,
		maxPods:                5,
		firstScale:             3,
		firstScaleStasis:       stasis,
		cpuBurst:               500,
		secondScale:            5,
		resourceType:           resourceType,
		metricTargetType:       metricTargetType,
		sidecarStatus:          e2eautoscaling.Enable,
		sidecarType:            e2eautoscaling.Idle,
	}
	st.run(ctx, name, kind, f)
}

func doNotScaleOnBusySidecar(ctx context.Context, name string, kind schema.GroupVersionKind, resourceType v1.ResourceName, metricTargetType autoscalingv2.MetricTargetType, checkStability bool, f *framework.Framework) {
	// Do not scale up on a busy sidecar with an idle application
	stasis := 0 * time.Minute
	if checkStability {
		stasis = 1 * time.Minute
	}
	st := &HPAContainerResourceScaleTest{
		initPods:               1,
		initCPUTotal:           250,
		perContainerCPURequest: 500,
		targetValue:            20,
		minPods:                1,
		maxPods:                5,
		cpuBurst:               700,
		sidecarStatus:          e2eautoscaling.Enable,
		sidecarType:            e2eautoscaling.Busy,
		resourceType:           resourceType,
		metricTargetType:       metricTargetType,
		noScale:                true,
		noScaleStasis:          stasis,
	}
	st.run(ctx, name, kind, f)
}

func getTargetValueByType(averageValueTarget, averageUtilizationTarget int, targetType autoscalingv2.MetricTargetType) int32 {
	if targetType == utilizationMetricType {
		return int32(averageUtilizationTarget)
	}
	return int32(averageValueTarget)
}
