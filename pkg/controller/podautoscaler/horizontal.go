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

package podautoscaler

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	autoscalingv1 "k8s.io/api/autoscaling/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	v1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	autoscalinginformers "k8s.io/client-go/informers/autoscaling/v2"
	coreinformers "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes/scheme"
	autoscalingclient "k8s.io/client-go/kubernetes/typed/autoscaling/v2"
	v1core "k8s.io/client-go/kubernetes/typed/core/v1"
	autoscalinglisters "k8s.io/client-go/listers/autoscaling/v2"
	corelisters "k8s.io/client-go/listers/core/v1"
	scaleclient "k8s.io/client-go/scale"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/controller"
	metricsclient "k8s.io/kubernetes/pkg/controller/podautoscaler/metrics"
	"k8s.io/kubernetes/pkg/controller/podautoscaler/monitor"
	"k8s.io/kubernetes/pkg/controller/util/selectors"
)

var (
	scaleUpLimitFactor  = 2.0
	scaleUpLimitMinimum = 4.0
)

var (
	// errSpec is used to determine if the error comes from the spec of HPA object in reconcileAutoscaler.
	// All such errors should have this error as a root error so that the upstream function can distinguish spec errors from internal errors.
	// e.g., fmt.Errorf("invalid spec%w", errSpec)
	errSpec error = errors.New("")
)

type timestampedRecommendation struct {
	recommendation int32
	timestamp      time.Time
}

type timestampedScaleEvent struct {
	replicaChange int32 // absolute value, non-negative
	timestamp     time.Time
	outdated      bool
}

// HorizontalController is responsible for the synchronizing HPA objects stored
// in the system with the actual deployments/replication controllers they
// control.
type HorizontalController struct {
	scaleNamespacer scaleclient.ScalesGetter
	hpaNamespacer   autoscalingclient.HorizontalPodAutoscalersGetter
	mapper          apimeta.RESTMapper

	replicaCalc   *ReplicaCalculator
	eventRecorder record.EventRecorder

	downscaleStabilisationWindow time.Duration

	monitor monitor.Monitor

	// hpaLister is able to list/get HPAs from the shared cache from the informer passed in to
	// NewHorizontalController.
	hpaLister       autoscalinglisters.HorizontalPodAutoscalerLister
	hpaListerSynced cache.InformerSynced

	// podLister is able to list/get Pods from the shared cache from the informer passed in to
	// NewHorizontalController.
	podLister       corelisters.PodLister
	podListerSynced cache.InformerSynced

	// Controllers that need to be synced
	queue workqueue.TypedRateLimitingInterface[string]

	// Latest unstabilized recommendations for each autoscaler.
	recommendations     map[string][]timestampedRecommendation
	recommendationsLock sync.Mutex

	// Latest autoscaler events
	scaleUpEvents       map[string][]timestampedScaleEvent
	scaleUpEventsLock   sync.RWMutex
	scaleDownEvents     map[string][]timestampedScaleEvent
	scaleDownEventsLock sync.RWMutex

	// Storage of HPAs and their selectors.
	hpaSelectors    *selectors.BiMultimap
	hpaSelectorsMux sync.Mutex
}

// NewHorizontalController creates a new HorizontalController.
func NewHorizontalController(
	ctx context.Context,
	evtNamespacer v1core.EventsGetter,
	scaleNamespacer scaleclient.ScalesGetter,
	hpaNamespacer autoscalingclient.HorizontalPodAutoscalersGetter,
	mapper apimeta.RESTMapper,
	metricsClient metricsclient.MetricsClient,
	hpaInformer autoscalinginformers.HorizontalPodAutoscalerInformer,
	podInformer coreinformers.PodInformer,
	resyncPeriod time.Duration,
	downscaleStabilisationWindow time.Duration,
	tolerance float64,
	cpuInitializationPeriod,
	delayOfInitialReadinessStatus time.Duration,
) *HorizontalController {
	broadcaster := record.NewBroadcaster(record.WithContext(ctx))
	broadcaster.StartStructuredLogging(3)
	broadcaster.StartRecordingToSink(&v1core.EventSinkImpl{Interface: evtNamespacer.Events("")})
	recorder := broadcaster.NewRecorder(scheme.Scheme, v1.EventSource{Component: "horizontal-pod-autoscaler"})

	hpaController := &HorizontalController{
		eventRecorder:                recorder,
		scaleNamespacer:              scaleNamespacer,
		hpaNamespacer:                hpaNamespacer,
		downscaleStabilisationWindow: downscaleStabilisationWindow,
		monitor:                      monitor.New(),
		queue: workqueue.NewTypedRateLimitingQueueWithConfig(
			NewDefaultHPARateLimiter(resyncPeriod),
			workqueue.TypedRateLimitingQueueConfig[string]{
				Name: "horizontalpodautoscaler",
			},
		),
		mapper:              mapper,
		recommendations:     map[string][]timestampedRecommendation{},
		recommendationsLock: sync.Mutex{},
		scaleUpEvents:       map[string][]timestampedScaleEvent{},
		scaleUpEventsLock:   sync.RWMutex{},
		scaleDownEvents:     map[string][]timestampedScaleEvent{},
		scaleDownEventsLock: sync.RWMutex{},
		hpaSelectors:        selectors.NewBiMultimap(),
	}

	hpaInformer.Informer().AddEventHandlerWithResyncPeriod(
		cache.ResourceEventHandlerFuncs{
			AddFunc:    hpaController.enqueueHPA,
			UpdateFunc: hpaController.updateHPA,
			DeleteFunc: hpaController.deleteHPA,
		},
		resyncPeriod,
	)
	hpaController.hpaLister = hpaInformer.Lister()
	hpaController.hpaListerSynced = hpaInformer.Informer().HasSynced

	hpaController.podLister = podInformer.Lister()
	hpaController.podListerSynced = podInformer.Informer().HasSynced

	replicaCalc := NewReplicaCalculator(
		metricsClient,
		hpaController.podLister,
		tolerance,
		cpuInitializationPeriod,
		delayOfInitialReadinessStatus,
	)
	hpaController.replicaCalc = replicaCalc

	monitor.Register()

	return hpaController
}

// Run begins watching and syncing.
func (a *HorizontalController) Run(ctx context.Context, workers int) {
	defer utilruntime.HandleCrash()
	defer a.queue.ShutDown()

	logger := klog.FromContext(ctx)
	logger.Info("Starting HPA controller")
	defer logger.Info("Shutting down HPA controller")

	if !cache.WaitForNamedCacheSync("HPA", ctx.Done(), a.hpaListerSynced, a.podListerSynced) {
		return
	}

	for i := 0; i < workers; i++ {
		go wait.UntilWithContext(ctx, a.worker, time.Second)
	}

	<-ctx.Done()
}

// obj could be an *v1.HorizontalPodAutoscaler, or a DeletionFinalStateUnknown marker item.
func (a *HorizontalController) updateHPA(old, cur interface{}) {
	a.enqueueHPA(cur)
}

// obj could be an *v1.HorizontalPodAutoscaler, or a DeletionFinalStateUnknown marker item.
func (a *HorizontalController) enqueueHPA(obj interface{}) {
	key, err := controller.KeyFunc(obj)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for object %+v: %v", obj, err))
		return
	}

	// Requests are always added to queue with resyncPeriod delay.  If there's already
	// request for the HPA in the queue then a new request is always dropped. Requests spend resync
	// interval in queue so HPAs are processed every resync interval.
	a.queue.AddRateLimited(key)

	// Register HPA in the hpaSelectors map if it's not present yet. Attaching the Nothing selector
	// that does not select objects. The actual selector is going to be updated
	// when it's available during the autoscaler reconciliation.
	a.hpaSelectorsMux.Lock()
	defer a.hpaSelectorsMux.Unlock()
	if hpaKey := selectors.Parse(key); !a.hpaSelectors.SelectorExists(hpaKey) {
		a.hpaSelectors.PutSelector(hpaKey, labels.Nothing())
	}
}

func (a *HorizontalController) deleteHPA(obj interface{}) {
	key, err := controller.KeyFunc(obj)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for object %+v: %v", obj, err))
		return
	}

	// TODO: could we leak if we fail to get the key?
	a.queue.Forget(key)

	// Remove HPA and attached selector.
	a.hpaSelectorsMux.Lock()
	defer a.hpaSelectorsMux.Unlock()
	a.hpaSelectors.DeleteSelector(selectors.Parse(key))
}

func (a *HorizontalController) worker(ctx context.Context) {
	for a.processNextWorkItem(ctx) {
	}
	logger := klog.FromContext(ctx)
	logger.Info("Horizontal Pod Autoscaler controller worker shutting down")
}

func (a *HorizontalController) processNextWorkItem(ctx context.Context) bool {
	key, quit := a.queue.Get()
	if quit {
		return false
	}
	defer a.queue.Done(key)

	deleted, err := a.reconcileKey(ctx, key)
	if err != nil {
		utilruntime.HandleError(err)
	}
	// Add request processing HPA to queue with resyncPeriod delay.
	// Requests are always added to queue with resyncPeriod delay. If there's already request
	// for the HPA in the queue then a new request is always dropped. Requests spend resyncPeriod
	// in queue so HPAs are processed every resyncPeriod.
	// Request is added here just in case last resync didn't insert request into the queue. This
	// happens quite often because there is race condition between adding request after resyncPeriod
	// and removing them from queue. Request can be added by resync before previous request is
	// removed from queue. If we didn't add request here then in this case one request would be dropped
	// and HPA would process after 2 x resyncPeriod.
	if !deleted {
		a.queue.AddRateLimited(key)
	}

	return true
}

// computeReplicasForMetrics computes the desired number of replicas for the metric specifications listed in the HPA,
// returning the maximum of the computed replica counts, a description of the associated metric, and the statuses of
// all metrics computed.
// It may return both valid metricDesiredReplicas and an error,
// when some metrics still work and HPA should perform scaling based on them.
// If HPA cannot do anything due to error, it returns -1 in metricDesiredReplicas as a failure signal.
func (a *HorizontalController) computeReplicasForMetrics(ctx context.Context, hpa *autoscalingv2.HorizontalPodAutoscaler, scale *autoscalingv1.Scale,
	metricSpecs []autoscalingv2.MetricSpec) (replicas int32, metric string, statuses []autoscalingv2.MetricStatus, timestamp time.Time, err error) {

	selector, err := a.validateAndParseSelector(hpa, scale.Status.Selector)
	if err != nil {
		return -1, "", nil, time.Time{}, err
	}

	specReplicas := scale.Spec.Replicas
	statusReplicas := scale.Status.Replicas
	statuses = make([]autoscalingv2.MetricStatus, len(metricSpecs))

	invalidMetricsCount := 0
	var invalidMetricError error
	var invalidMetricCondition autoscalingv2.HorizontalPodAutoscalerCondition

	for i, metricSpec := range metricSpecs {
		replicaCountProposal, metricNameProposal, timestampProposal, condition, err := a.computeReplicasForMetric(ctx, hpa, metricSpec, specReplicas, statusReplicas, selector, &statuses[i])

		if err != nil {
			if invalidMetricsCount <= 0 {
				invalidMetricCondition = condition
				invalidMetricError = err
			}
			invalidMetricsCount++
			continue
		}
		if replicas == 0 || replicaCountProposal > replicas {
			timestamp = timestampProposal
			replicas = replicaCountProposal
			metric = metricNameProposal
		}
	}

	if invalidMetricError != nil {
		invalidMetricError = fmt.Errorf("invalid metrics (%v invalid out of %v), first error is: %v", invalidMetricsCount, len(metricSpecs), invalidMetricError)
	}

	// If all metrics are invalid or some are invalid and we would scale down,
	// return an error and set the condition of the hpa based on the first invalid metric.
	// Otherwise set the condition as scaling active as we're going to scale
	if invalidMetricsCount >= len(metricSpecs) || (invalidMetricsCount > 0 && replicas < specReplicas) {
		setCondition(hpa, invalidMetricCondition.Type, invalidMetricCondition.Status, invalidMetricCondition.Reason, "%s", invalidMetricCondition.Message)
		return -1, "", statuses, time.Time{}, invalidMetricError
	}
	setCondition(hpa, autoscalingv2.ScalingActive, v1.ConditionTrue, "ValidMetricFound", "the HPA was able to successfully calculate a replica count from %s", metric)

	return replicas, metric, statuses, timestamp, invalidMetricError
}

// hpasControllingPodsUnderSelector returns a list of keys of all HPAs that control a given list of pods.
func (a *HorizontalController) hpasControllingPodsUnderSelector(pods []*v1.Pod) []selectors.Key {
	a.hpaSelectorsMux.Lock()
	defer a.hpaSelectorsMux.Unlock()

	hpas := map[selectors.Key]struct{}{}
	for _, p := range pods {
		podKey := selectors.Key{Name: p.Name, Namespace: p.Namespace}
		a.hpaSelectors.Put(podKey, p.Labels)

		selectingHpas, ok := a.hpaSelectors.ReverseSelect(podKey)
		if !ok {
			continue
		}
		for _, hpa := range selectingHpas {
			hpas[hpa] = struct{}{}
		}
	}
	// Clean up all added pods.
	a.hpaSelectors.KeepOnly([]selectors.Key{})

	hpaList := []selectors.Key{}
	for hpa := range hpas {
		hpaList = append(hpaList, hpa)
	}
	return hpaList
}

// validateAndParseSelector verifies that:
// - selector is not empty;
// - selector format is valid;
// - all pods by current selector are controlled by only one HPA.
// Returns an error if the check has failed or the parsed selector if succeeded.
// In case of an error the ScalingActive is set to false with the corresponding reason.
func (a *HorizontalController) validateAndParseSelector(hpa *autoscalingv2.HorizontalPodAutoscaler, selector string) (labels.Selector, error) {
	if selector == "" {
		errMsg := "selector is required"
		a.eventRecorder.Event(hpa, v1.EventTypeWarning, "SelectorRequired", errMsg)
		setCondition(hpa, autoscalingv2.ScalingActive, v1.ConditionFalse, "InvalidSelector", "the HPA target's scale is missing a selector")
		return nil, errors.New(errMsg)
	}

	parsedSelector, err := labels.Parse(selector)
	if err != nil {
		errMsg := fmt.Sprintf("couldn't convert selector into a corresponding internal selector object: %v", err)
		a.eventRecorder.Event(hpa, v1.EventTypeWarning, "InvalidSelector", errMsg)
		setCondition(hpa, autoscalingv2.ScalingActive, v1.ConditionFalse, "InvalidSelector", "%s", errMsg)
		return nil, errors.New(errMsg)
	}

	hpaKey := selectors.Key{Name: hpa.Name, Namespace: hpa.Namespace}
	a.hpaSelectorsMux.Lock()
	if a.hpaSelectors.SelectorExists(hpaKey) {
		// Update HPA selector only if the HPA was registered in enqueueHPA.
		a.hpaSelectors.PutSelector(hpaKey, parsedSelector)
	}
	a.hpaSelectorsMux.Unlock()

	pods, err := a.podLister.Pods(hpa.Namespace).List(parsedSelector)
	if err != nil {
		return nil, err
	}

	selectingHpas := a.hpasControllingPodsUnderSelector(pods)
	if len(selectingHpas) > 1 {
		errMsg := fmt.Sprintf("pods by selector %v are controlled by multiple HPAs: %v", selector, selectingHpas)
		a.eventRecorder.Event(hpa, v1.EventTypeWarning, "AmbiguousSelector", errMsg)
		setCondition(hpa, autoscalingv2.ScalingActive, v1.ConditionFalse, "AmbiguousSelector", "%s", errMsg)
		return nil, errors.New(errMsg)
	}

	return parsedSelector, nil
}

// Computes the desired number of replicas for a specific hpa and metric specification,
// returning the metric status and a proposed condition to be set on the HPA object.
func (a *HorizontalController) computeReplicasForMetric(ctx context.Context, hpa *autoscalingv2.HorizontalPodAutoscaler, spec autoscalingv2.MetricSpec,
	specReplicas, statusReplicas int32, selector labels.Selector, status *autoscalingv2.MetricStatus) (replicaCountProposal int32, metricNameProposal string,
	timestampProposal time.Time, condition autoscalingv2.HorizontalPodAutoscalerCondition, err error) {
	// actionLabel is used to report which actions this reconciliation has taken.
	start := time.Now()
	defer func() {
		actionLabel := monitor.ActionLabelNone
		switch {
		case replicaCountProposal > hpa.Status.CurrentReplicas:
			actionLabel = monitor.ActionLabelScaleUp
		case replicaCountProposal < hpa.Status.CurrentReplicas:
			actionLabel = monitor.ActionLabelScaleDown
		}

		errorLabel := monitor.ErrorLabelNone
		if err != nil {
			// In case of error, set "internal" as default.
			errorLabel = monitor.ErrorLabelInternal
			actionLabel = monitor.ActionLabelNone
		}
		if errors.Is(err, errSpec) {
			errorLabel = monitor.ErrorLabelSpec
		}

		a.monitor.ObserveMetricComputationResult(actionLabel, errorLabel, time.Since(start), spec.Type)
	}()

	switch spec.Type {
	case autoscalingv2.ObjectMetricSourceType:
		metricSelector, err := metav1.LabelSelectorAsSelector(spec.Object.Metric.Selector)
		if err != nil {
			condition := a.getUnableComputeReplicaCountCondition(hpa, "FailedGetObjectMetric", err)
			return 0, "", time.Time{}, condition, fmt.Errorf("failed to get object metric value: %v", err)
		}
		replicaCountProposal, timestampProposal, metricNameProposal, condition, err = a.computeStatusForObjectMetric(specReplicas, statusReplicas, spec, hpa, selector, status, metricSelector)
		if err != nil {
			return 0, "", time.Time{}, condition, fmt.Errorf("failed to get object metric value: %v", err)
		}
	case autoscalingv2.PodsMetricSourceType:
		metricSelector, err := metav1.LabelSelectorAsSelector(spec.Pods.Metric.Selector)
		if err != nil {
			condition := a.getUnableComputeReplicaCountCondition(hpa, "FailedGetPodsMetric", err)
			return 0, "", time.Time{}, condition, fmt.Errorf("failed to get pods metric value: %v", err)
		}
		replicaCountProposal, timestampProposal, metricNameProposal, condition, err = a.computeStatusForPodsMetric(specReplicas, spec, hpa, selector, status, metricSelector)
		if err != nil {
			return 0, "", time.Time{}, condition, fmt.Errorf("failed to get pods metric value: %v", err)
		}
	case autoscalingv2.ResourceMetricSourceType:
		replicaCountProposal, timestampProposal, metricNameProposal, condition, err = a.computeStatusForResourceMetric(ctx, specReplicas, spec, hpa, selector, status)
		if err != nil {
			return 0, "", time.Time{}, condition, fmt.Errorf("failed to get %s resource metric value: %v", spec.Resource.Name, err)
		}
	case autoscalingv2.ContainerResourceMetricSourceType:
		replicaCountProposal, timestampProposal, metricNameProposal, condition, err = a.computeStatusForContainerResourceMetric(ctx, specReplicas, spec, hpa, selector, status)
		if err != nil {
			return 0, "", time.Time{}, condition, fmt.Errorf("failed to get %s container metric value: %v", spec.ContainerResource.Container, err)
		}
	case autoscalingv2.ExternalMetricSourceType:
		replicaCountProposal, timestampProposal, metricNameProposal, condition, err = a.computeStatusForExternalMetric(specReplicas, statusReplicas, spec, hpa, selector, status)
		if err != nil {
			return 0, "", time.Time{}, condition, fmt.Errorf("failed to get %s external metric value: %v", spec.External.Metric.Name, err)
		}
	default:
		// It shouldn't reach here as invalid metric source type is filtered out in the api-server's validation.
		err = fmt.Errorf("unknown metric source type %q%w", string(spec.Type), errSpec)
		condition := a.getUnableComputeReplicaCountCondition(hpa, "InvalidMetricSourceType", err)
		return 0, "", time.Time{}, condition, err
	}
	return replicaCountProposal, metricNameProposal, timestampProposal, autoscalingv2.HorizontalPodAutoscalerCondition{}, nil
}

func (a *HorizontalController) reconcileKey(ctx context.Context, key string) (deleted bool, err error) {
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return true, err
	}

	logger := klog.FromContext(ctx)

	hpa, err := a.hpaLister.HorizontalPodAutoscalers(namespace).Get(name)
	if k8serrors.IsNotFound(err) {
		logger.Info("Horizontal Pod Autoscaler has been deleted", "HPA", klog.KRef(namespace, name))

		a.recommendationsLock.Lock()
		delete(a.recommendations, key)
		a.recommendationsLock.Unlock()

		a.scaleUpEventsLock.Lock()
		delete(a.scaleUpEvents, key)
		a.scaleUpEventsLock.Unlock()

		a.scaleDownEventsLock.Lock()
		delete(a.scaleDownEvents, key)
		a.scaleDownEventsLock.Unlock()

		return true, nil
	}
	if err != nil {
		return false, err
	}

	return false, a.reconcileAutoscaler(ctx, hpa, key)
}

// computeStatusForObjectMetric computes the desired number of replicas for the specified metric of type ObjectMetricSourceType.
func (a *HorizontalController) computeStatusForObjectMetric(specReplicas, statusReplicas int32, metricSpec autoscalingv2.MetricSpec, hpa *autoscalingv2.HorizontalPodAutoscaler, selector labels.Selector, status *autoscalingv2.MetricStatus, metricSelector labels.Selector) (replicas int32, timestamp time.Time, metricName string, condition autoscalingv2.HorizontalPodAutoscalerCondition, err error) {
	metricStatus := autoscalingv2.MetricStatus{
		Type: autoscalingv2.ObjectMetricSourceType,
		Object: &autoscalingv2.ObjectMetricStatus{
			DescribedObject: metricSpec.Object.DescribedObject,
			Metric: autoscalingv2.MetricIdentifier{
				Name:     metricSpec.Object.Metric.Name,
				Selector: metricSpec.Object.Metric.Selector,
			},
		},
	}
	if metricSpec.Object.Target.Type == autoscalingv2.ValueMetricType && metricSpec.Object.Target.Value != nil {
		replicaCountProposal, usageProposal, timestampProposal, err := a.replicaCalc.GetObjectMetricReplicas(specReplicas, metricSpec.Object.Target.Value.MilliValue(), metricSpec.Object.Metric.Name, hpa.Namespace, &metricSpec.Object.DescribedObject, selector, metricSelector)
		if err != nil {
			condition := a.getUnableComputeReplicaCountCondition(hpa, "FailedGetObjectMetric", err)
			return 0, timestampProposal, "", condition, err
		}
		metricStatus.Object.Current.Value = resource.NewMilliQuantity(usageProposal, resource.DecimalSI)
		*status = metricStatus
		return replicaCountProposal, timestampProposal, fmt.Sprintf("%s metric %s", metricSpec.Object.DescribedObject.Kind, metricSpec.Object.Metric.Name), autoscalingv2.HorizontalPodAutoscalerCondition{}, nil
	} else if metricSpec.Object.Target.Type == autoscalingv2.AverageValueMetricType && metricSpec.Object.Target.AverageValue != nil {
		replicaCountProposal, usageProposal, timestampProposal, err := a.replicaCalc.GetObjectPerPodMetricReplicas(statusReplicas, metricSpec.Object.Target.AverageValue.MilliValue(), metricSpec.Object.Metric.Name, hpa.Namespace, &metricSpec.Object.DescribedObject, metricSelector)
		if err != nil {
			condition := a.getUnableComputeReplicaCountCondition(hpa, "FailedGetObjectMetric", err)
			return 0, time.Time{}, "", condition, fmt.Errorf("failed to get %s object metric: %v", metricSpec.Object.Metric.Name, err)
		}
		metricStatus.Object.Current.AverageValue = resource.NewMilliQuantity(usageProposal, resource.DecimalSI)
		*status = metricStatus
		return replicaCountProposal, timestampProposal, fmt.Sprintf("external metric %s(%+v)", metricSpec.Object.Metric.Name, metricSpec.Object.Metric.Selector), autoscalingv2.HorizontalPodAutoscalerCondition{}, nil
	}
	errMsg := "invalid object metric source: neither a value target nor an average value target was set"
	err = errors.New(errMsg)
	condition = a.getUnableComputeReplicaCountCondition(hpa, "FailedGetObjectMetric", err)
	return 0, time.Time{}, "", condition, err
}

// computeStatusForPodsMetric computes the desired number of replicas for the specified metric of type PodsMetricSourceType.
func (a *HorizontalController) computeStatusForPodsMetric(currentReplicas int32, metricSpec autoscalingv2.MetricSpec, hpa *autoscalingv2.HorizontalPodAutoscaler, selector labels.Selector, status *autoscalingv2.MetricStatus, metricSelector labels.Selector) (replicaCountProposal int32, timestampProposal time.Time, metricNameProposal string, condition autoscalingv2.HorizontalPodAutoscalerCondition, err error) {
	replicaCountProposal, usageProposal, timestampProposal, err := a.replicaCalc.GetMetricReplicas(currentReplicas, metricSpec.Pods.Target.AverageValue.MilliValue(), metricSpec.Pods.Metric.Name, hpa.Namespace, selector, metricSelector)
	if err != nil {
		condition = a.getUnableComputeReplicaCountCondition(hpa, "FailedGetPodsMetric", err)
		return 0, timestampProposal, "", condition, err
	}
	*status = autoscalingv2.MetricStatus{
		Type: autoscalingv2.PodsMetricSourceType,
		Pods: &autoscalingv2.PodsMetricStatus{
			Metric: autoscalingv2.MetricIdentifier{
				Name:     metricSpec.Pods.Metric.Name,
				Selector: metricSpec.Pods.Metric.Selector,
			},
			Current: autoscalingv2.MetricValueStatus{
				AverageValue: resource.NewMilliQuantity(usageProposal, resource.DecimalSI),
			},
		},
	}

	return replicaCountProposal, timestampProposal, fmt.Sprintf("pods metric %s", metricSpec.Pods.Metric.Name), autoscalingv2.HorizontalPodAutoscalerCondition{}, nil
}

func (a *HorizontalController) computeStatusForResourceMetricGeneric(ctx context.Context, currentReplicas int32, target autoscalingv2.MetricTarget,
	resourceName v1.ResourceName, namespace string, container string, selector labels.Selector, sourceType autoscalingv2.MetricSourceType) (replicaCountProposal int32,
	metricStatus *autoscalingv2.MetricValueStatus, timestampProposal time.Time, metricNameProposal string,
	condition autoscalingv2.HorizontalPodAutoscalerCondition, err error) {
	if target.AverageValue != nil {
		var rawProposal int64
		replicaCountProposal, rawProposal, timestampProposal, err := a.replicaCalc.GetRawResourceReplicas(ctx, currentReplicas, target.AverageValue.MilliValue(), resourceName, namespace, selector, container)
		if err != nil {
			return 0, nil, time.Time{}, "", condition, fmt.Errorf("failed to get %s usage: %v", resourceName, err)
		}
		metricNameProposal = fmt.Sprintf("%s resource", resourceName.String())
		status := autoscalingv2.MetricValueStatus{
			AverageValue: resource.NewMilliQuantity(rawProposal, resource.DecimalSI),
		}
		return replicaCountProposal, &status, timestampProposal, metricNameProposal, autoscalingv2.HorizontalPodAutoscalerCondition{}, nil
	}

	if target.AverageUtilization == nil {
		errMsg := "invalid resource metric source: neither an average utilization target nor an average value (usage) target was set"
		return 0, nil, time.Time{}, "", condition, errors.New(errMsg)
	}

	targetUtilization := *target.AverageUtilization
	replicaCountProposal, percentageProposal, rawProposal, timestampProposal, err := a.replicaCalc.GetResourceReplicas(ctx, currentReplicas, targetUtilization, resourceName, namespace, selector, container)
	if err != nil {
		return 0, nil, time.Time{}, "", condition, fmt.Errorf("failed to get %s utilization: %v", resourceName, err)
	}

	metricNameProposal = fmt.Sprintf("%s resource utilization (percentage of request)", resourceName)
	if sourceType == autoscalingv2.ContainerResourceMetricSourceType {
		metricNameProposal = fmt.Sprintf("%s container resource utilization (percentage of request)", resourceName)
	}
	status := autoscalingv2.MetricValueStatus{
		AverageUtilization: &percentageProposal,
		AverageValue:       resource.NewMilliQuantity(rawProposal, resource.DecimalSI),
	}
	return replicaCountProposal, &status, timestampProposal, metricNameProposal, autoscalingv2.HorizontalPodAutoscalerCondition{}, nil
}

// computeStatusForResourceMetric computes the desired number of replicas for the specified metric of type ResourceMetricSourceType.
func (a *HorizontalController) computeStatusForResourceMetric(ctx context.Context, currentReplicas int32, metricSpec autoscalingv2.MetricSpec, hpa *autoscalingv2.HorizontalPodAutoscaler,
	selector labels.Selector, status *autoscalingv2.MetricStatus) (replicaCountProposal int32, timestampProposal time.Time,
	metricNameProposal string, condition autoscalingv2.HorizontalPodAutoscalerCondition, err error) {
	replicaCountProposal, metricValueStatus, timestampProposal, metricNameProposal, condition, err := a.computeStatusForResourceMetricGeneric(ctx, currentReplicas, metricSpec.Resource.Target, metricSpec.Resource.Name, hpa.Namespace, "", selector, autoscalingv2.ResourceMetricSourceType)
	if err != nil {
		condition = a.getUnableComputeReplicaCountCondition(hpa, "FailedGetResourceMetric", err)
		return replicaCountProposal, timestampProposal, metricNameProposal, condition, err
	}
	*status = autoscalingv2.MetricStatus{
		Type: autoscalingv2.ResourceMetricSourceType,
		Resource: &autoscalingv2.ResourceMetricStatus{
			Name:    metricSpec.Resource.Name,
			Current: *metricValueStatus,
		},
	}
	return replicaCountProposal, timestampProposal, metricNameProposal, condition, nil
}

// computeStatusForContainerResourceMetric computes the desired number of replicas for the specified metric of type ResourceMetricSourceType.
func (a *HorizontalController) computeStatusForContainerResourceMetric(ctx context.Context, currentReplicas int32, metricSpec autoscalingv2.MetricSpec, hpa *autoscalingv2.HorizontalPodAutoscaler,
	selector labels.Selector, status *autoscalingv2.MetricStatus) (replicaCountProposal int32, timestampProposal time.Time,
	metricNameProposal string, condition autoscalingv2.HorizontalPodAutoscalerCondition, err error) {
	replicaCountProposal, metricValueStatus, timestampProposal, metricNameProposal, condition, err := a.computeStatusForResourceMetricGeneric(ctx, currentReplicas, metricSpec.ContainerResource.Target, metricSpec.ContainerResource.Name, hpa.Namespace, metricSpec.ContainerResource.Container, selector, autoscalingv2.ContainerResourceMetricSourceType)
	if err != nil {
		condition = a.getUnableComputeReplicaCountCondition(hpa, "FailedGetContainerResourceMetric", err)
		return replicaCountProposal, timestampProposal, metricNameProposal, condition, err
	}
	*status = autoscalingv2.MetricStatus{
		Type: autoscalingv2.ContainerResourceMetricSourceType,
		ContainerResource: &autoscalingv2.ContainerResourceMetricStatus{
			Name:      metricSpec.ContainerResource.Name,
			Container: metricSpec.ContainerResource.Container,
			Current:   *metricValueStatus,
		},
	}
	return replicaCountProposal, timestampProposal, metricNameProposal, condition, nil
}

// computeStatusForExternalMetric computes the desired number of replicas for the specified metric of type ExternalMetricSourceType.
func (a *HorizontalController) computeStatusForExternalMetric(specReplicas, statusReplicas int32, metricSpec autoscalingv2.MetricSpec, hpa *autoscalingv2.HorizontalPodAutoscaler, selector labels.Selector, status *autoscalingv2.MetricStatus) (replicaCountProposal int32, timestampProposal time.Time, metricNameProposal string, condition autoscalingv2.HorizontalPodAutoscalerCondition, err error) {
	if metricSpec.External.Target.AverageValue != nil {
		replicaCountProposal, usageProposal, timestampProposal, err := a.replicaCalc.GetExternalPerPodMetricReplicas(statusReplicas, metricSpec.External.Target.AverageValue.MilliValue(), metricSpec.External.Metric.Name, hpa.Namespace, metricSpec.External.Metric.Selector)
		if err != nil {
			condition = a.getUnableComputeReplicaCountCondition(hpa, "FailedGetExternalMetric", err)
			return 0, time.Time{}, "", condition, fmt.Errorf("failed to get %s external metric: %v", metricSpec.External.Metric.Name, err)
		}
		*status = autoscalingv2.MetricStatus{
			Type: autoscalingv2.ExternalMetricSourceType,
			External: &autoscalingv2.ExternalMetricStatus{
				Metric: autoscalingv2.MetricIdentifier{
					Name:     metricSpec.External.Metric.Name,
					Selector: metricSpec.External.Metric.Selector,
				},
				Current: autoscalingv2.MetricValueStatus{
					AverageValue: resource.NewMilliQuantity(usageProposal, resource.DecimalSI),
				},
			},
		}
		return replicaCountProposal, timestampProposal, fmt.Sprintf("external metric %s(%+v)", metricSpec.External.Metric.Name, metricSpec.External.Metric.Selector), autoscalingv2.HorizontalPodAutoscalerCondition{}, nil
	}
	if metricSpec.External.Target.Value != nil {
		replicaCountProposal, usageProposal, timestampProposal, err := a.replicaCalc.GetExternalMetricReplicas(specReplicas, metricSpec.External.Target.Value.MilliValue(), metricSpec.External.Metric.Name, hpa.Namespace, metricSpec.External.Metric.Selector, selector)
		if err != nil {
			condition = a.getUnableComputeReplicaCountCondition(hpa, "FailedGetExternalMetric", err)
			return 0, time.Time{}, "", condition, fmt.Errorf("failed to get external metric %s: %v", metricSpec.External.Metric.Name, err)
		}
		*status = autoscalingv2.MetricStatus{
			Type: autoscalingv2.ExternalMetricSourceType,
			External: &autoscalingv2.ExternalMetricStatus{
				Metric: autoscalingv2.MetricIdentifier{
					Name:     metricSpec.External.Metric.Name,
					Selector: metricSpec.External.Metric.Selector,
				},
				Current: autoscalingv2.MetricValueStatus{
					Value: resource.NewMilliQuantity(usageProposal, resource.DecimalSI),
				},
			},
		}
		return replicaCountProposal, timestampProposal, fmt.Sprintf("external metric %s(%+v)", metricSpec.External.Metric.Name, metricSpec.External.Metric.Selector), autoscalingv2.HorizontalPodAutoscalerCondition{}, nil
	}
	errMsg := "invalid external metric source: neither a value target nor an average value target was set"
	err = errors.New(errMsg)
	condition = a.getUnableComputeReplicaCountCondition(hpa, "FailedGetExternalMetric", err)
	return 0, time.Time{}, "", condition, errors.New(errMsg)
}

func (a *HorizontalController) recordInitialRecommendation(currentReplicas int32, key string) {
	a.recommendationsLock.Lock()
	defer a.recommendationsLock.Unlock()
	if a.recommendations[key] == nil {
		a.recommendations[key] = []timestampedRecommendation{{currentReplicas, time.Now()}}
	}
}

func (a *HorizontalController) reconcileAutoscaler(ctx context.Context, hpaShared *autoscalingv2.HorizontalPodAutoscaler, key string) (retErr error) {
	// actionLabel is used to report which actions this reconciliation has taken.
	actionLabel := monitor.ActionLabelNone
	start := time.Now()
	defer func() {
		errorLabel := monitor.ErrorLabelNone
		if retErr != nil {
			// In case of error, set "internal" as default.
			errorLabel = monitor.ErrorLabelInternal
		}
		if errors.Is(retErr, errSpec) {
			errorLabel = monitor.ErrorLabelSpec
		}

		a.monitor.ObserveReconciliationResult(actionLabel, errorLabel, time.Since(start))
	}()

	// make a copy so that we never mutate the shared informer cache (conversion can mutate the object)
	hpa := hpaShared.DeepCopy()
	hpaStatusOriginal := hpa.Status.DeepCopy()

	reference := fmt.Sprintf("%s/%s/%s", hpa.Spec.ScaleTargetRef.Kind, hpa.Namespace, hpa.Spec.ScaleTargetRef.Name)

	targetGV, err := schema.ParseGroupVersion(hpa.Spec.ScaleTargetRef.APIVersion)
	if err != nil {
		a.eventRecorder.Event(hpa, v1.EventTypeWarning, "FailedGetScale", err.Error())
		setCondition(hpa, autoscalingv2.AbleToScale, v1.ConditionFalse, "FailedGetScale", "the HPA controller was unable to get the target's current scale: %v", err)
		if err := a.updateStatusIfNeeded(ctx, hpaStatusOriginal, hpa); err != nil {
			utilruntime.HandleError(err)
		}
		return fmt.Errorf("invalid API version in scale target reference: %v%w", err, errSpec)
	}

	targetGK := schema.GroupKind{
		Group: targetGV.Group,
		Kind:  hpa.Spec.ScaleTargetRef.Kind,
	}

	mappings, err := a.mapper.RESTMappings(targetGK)
	if err != nil {
		a.eventRecorder.Event(hpa, v1.EventTypeWarning, "FailedGetScale", err.Error())
		setCondition(hpa, autoscalingv2.AbleToScale, v1.ConditionFalse, "FailedGetScale", "the HPA controller was unable to get the target's current scale: %v", err)
		if err := a.updateStatusIfNeeded(ctx, hpaStatusOriginal, hpa); err != nil {
			utilruntime.HandleError(err)
		}
		return fmt.Errorf("unable to determine resource for scale target reference: %v", err)
	}

	scale, targetGR, err := a.scaleForResourceMappings(ctx, hpa.Namespace, hpa.Spec.ScaleTargetRef.Name, mappings)
	if err != nil {
		a.eventRecorder.Event(hpa, v1.EventTypeWarning, "FailedGetScale", err.Error())
		setCondition(hpa, autoscalingv2.AbleToScale, v1.ConditionFalse, "FailedGetScale", "the HPA controller was unable to get the target's current scale: %v", err)
		if err := a.updateStatusIfNeeded(ctx, hpaStatusOriginal, hpa); err != nil {
			utilruntime.HandleError(err)
		}
		return fmt.Errorf("failed to query scale subresource for %s: %v", reference, err)
	}
	setCondition(hpa, autoscalingv2.AbleToScale, v1.ConditionTrue, "SucceededGetScale", "the HPA controller was able to get the target's current scale")
	currentReplicas := scale.Spec.Replicas
	a.recordInitialRecommendation(currentReplicas, key)

	var (
		metricStatuses        []autoscalingv2.MetricStatus
		metricDesiredReplicas int32
		metricName            string
	)

	desiredReplicas := int32(0)
	rescaleReason := ""

	var minReplicas int32

	if hpa.Spec.MinReplicas != nil {
		minReplicas = *hpa.Spec.MinReplicas
	} else {
		// Default value
		minReplicas = 1
	}

	rescale := true
	logger := klog.FromContext(ctx)

	if currentReplicas == 0 && minReplicas != 0 {
		// Autoscaling is disabled for this resource
		desiredReplicas = 0
		rescale = false
		setCondition(hpa, autoscalingv2.ScalingActive, v1.ConditionFalse, "ScalingDisabled", "scaling is disabled since the replica count of the target is zero")
	} else if currentReplicas > hpa.Spec.MaxReplicas {
		rescaleReason = "Current number of replicas above Spec.MaxReplicas"
		desiredReplicas = hpa.Spec.MaxReplicas
	} else if currentReplicas < minReplicas {
		rescaleReason = "Current number of replicas below Spec.MinReplicas"
		desiredReplicas = minReplicas
	} else {
		var metricTimestamp time.Time
		metricDesiredReplicas, metricName, metricStatuses, metricTimestamp, err = a.computeReplicasForMetrics(ctx, hpa, scale, hpa.Spec.Metrics)
		// computeReplicasForMetrics may return both non-zero metricDesiredReplicas and an error.
		// That means some metrics still work and HPA should perform scaling based on them.
		if err != nil && metricDesiredReplicas == -1 {
			a.setCurrentReplicasAndMetricsInStatus(hpa, currentReplicas, metricStatuses)
			if err := a.updateStatusIfNeeded(ctx, hpaStatusOriginal, hpa); err != nil {
				utilruntime.HandleError(err)
			}
			a.eventRecorder.Event(hpa, v1.EventTypeWarning, "FailedComputeMetricsReplicas", err.Error())
			return fmt.Errorf("failed to compute desired number of replicas based on listed metrics for %s: %v", reference, err)
		}
		if err != nil {
			// We proceed to scaling, but return this error from reconcileAutoscaler() finally.
			retErr = err
		}

		logger.V(4).Info("Proposing desired replicas",
			"desiredReplicas", metricDesiredReplicas,
			"metric", metricName,
			"timestamp", metricTimestamp,
			"scaleTarget", reference)

		rescaleMetric := ""
		if metricDesiredReplicas > desiredReplicas {
			desiredReplicas = metricDesiredReplicas
			rescaleMetric = metricName
		}
		if desiredReplicas > currentReplicas {
			rescaleReason = fmt.Sprintf("%s above target", rescaleMetric)
		}
		if desiredReplicas < currentReplicas {
			rescaleReason = "All metrics below target"
		}
		if hpa.Spec.Behavior == nil {
			desiredReplicas = a.normalizeDesiredReplicas(hpa, key, currentReplicas, desiredReplicas, minReplicas)
		} else {
			desiredReplicas = a.normalizeDesiredReplicasWithBehaviors(hpa, key, currentReplicas, desiredReplicas, minReplicas)
		}
		rescale = desiredReplicas != currentReplicas
	}

	if rescale {
		scale.Spec.Replicas = desiredReplicas
		_, err = a.scaleNamespacer.Scales(hpa.Namespace).Update(ctx, targetGR, scale, metav1.UpdateOptions{})
		if err != nil {
			a.eventRecorder.Eventf(hpa, v1.EventTypeWarning, "FailedRescale", "New size: %d; reason: %s; error: %v", desiredReplicas, rescaleReason, err.Error())
			setCondition(hpa, autoscalingv2.AbleToScale, v1.ConditionFalse, "FailedUpdateScale", "the HPA controller was unable to update the target scale: %v", err)
			a.setCurrentReplicasAndMetricsInStatus(hpa, currentReplicas, metricStatuses)
			if err := a.updateStatusIfNeeded(ctx, hpaStatusOriginal, hpa); err != nil {
				utilruntime.HandleError(err)
			}
			return fmt.Errorf("failed to rescale %s: %v", reference, err)
		}
		setCondition(hpa, autoscalingv2.AbleToScale, v1.ConditionTrue, "SucceededRescale", "the HPA controller was able to update the target scale to %d", desiredReplicas)
		a.eventRecorder.Eventf(hpa, v1.EventTypeNormal, "SuccessfulRescale", "New size: %d; reason: %s", desiredReplicas, rescaleReason)
		a.storeScaleEvent(hpa.Spec.Behavior, key, currentReplicas, desiredReplicas)
		logger.Info("Successfully rescaled",
			"HPA", klog.KObj(hpa),
			"currentReplicas", currentReplicas,
			"desiredReplicas", desiredReplicas,
			"reason", rescaleReason)

		if desiredReplicas > currentReplicas {
			actionLabel = monitor.ActionLabelScaleUp
		} else {
			actionLabel = monitor.ActionLabelScaleDown
		}
	} else {
		logger.V(4).Info("Decided not to scale",
			"scaleTarget", reference,
			"desiredReplicas", desiredReplicas,
			"lastScaleTime", hpa.Status.LastScaleTime)
		desiredReplicas = currentReplicas
	}

	a.setStatus(hpa, currentReplicas, desiredReplicas, metricStatuses, rescale)

	err = a.updateStatusIfNeeded(ctx, hpaStatusOriginal, hpa)
	if err != nil {
		// we can overwrite retErr in this case because it's an internal error.
		return err
	}

	return retErr
}

// stabilizeRecommendation:
// - replaces old recommendation with the newest recommendation,
// - returns max of recommendations that are not older than downscaleStabilisationWindow.
func (a *HorizontalController) stabilizeRecommendation(key string, prenormalizedDesiredReplicas int32) int32 {
	maxRecommendation := prenormalizedDesiredReplicas
	foundOldSample := false
	oldSampleIndex := 0
	cutoff := time.Now().Add(-a.downscaleStabilisationWindow)

	a.recommendationsLock.Lock()
	defer a.recommendationsLock.Unlock()
	for i, rec := range a.recommendations[key] {
		if rec.timestamp.Before(cutoff) {
			foundOldSample = true
			oldSampleIndex = i
		} else if rec.recommendation > maxRecommendation {
			maxRecommendation = rec.recommendation
		}
	}
	if foundOldSample {
		a.recommendations[key][oldSampleIndex] = timestampedRecommendation{prenormalizedDesiredReplicas, time.Now()}
	} else {
		a.recommendations[key] = append(a.recommendations[key], timestampedRecommendation{prenormalizedDesiredReplicas, time.Now()})
	}
	return maxRecommendation
}

// normalizeDesiredReplicas takes the metrics desired replicas value and normalizes it based on the appropriate conditions (i.e. < maxReplicas, >
// minReplicas, etc...)
func (a *HorizontalController) normalizeDesiredReplicas(hpa *autoscalingv2.HorizontalPodAutoscaler, key string, currentReplicas int32, prenormalizedDesiredReplicas int32, minReplicas int32) int32 {
	stabilizedRecommendation := a.stabilizeRecommendation(key, prenormalizedDesiredReplicas)
	if stabilizedRecommendation != prenormalizedDesiredReplicas {
		setCondition(hpa, autoscalingv2.AbleToScale, v1.ConditionTrue, "ScaleDownStabilized", "recent recommendations were higher than current one, applying the highest recent recommendation")
	} else {
		setCondition(hpa, autoscalingv2.AbleToScale, v1.ConditionTrue, "ReadyForNewScale", "recommended size matches current size")
	}

	desiredReplicas, reason, message := convertDesiredReplicasWithRules(currentReplicas, stabilizedRecommendation, minReplicas, hpa.Spec.MaxReplicas)

	if desiredReplicas == stabilizedRecommendation {
		setCondition(hpa, autoscalingv2.ScalingLimited, v1.ConditionFalse, reason, "%s", message)
	} else {
		setCondition(hpa, autoscalingv2.ScalingLimited, v1.ConditionTrue, reason, "%s", message)
	}

	return desiredReplicas
}

// NormalizationArg is used to pass all needed information between functions as one structure
type NormalizationArg struct {
	Key               string
	ScaleUpBehavior   *autoscalingv2.HPAScalingRules
	ScaleDownBehavior *autoscalingv2.HPAScalingRules
	MinReplicas       int32
	MaxReplicas       int32
	CurrentReplicas   int32
	DesiredReplicas   int32
}

// normalizeDesiredReplicasWithBehaviors takes the metrics desired replicas value and normalizes it:
// 1. Apply the basic conditions (i.e. < maxReplicas, > minReplicas, etc...)
// 2. Apply the scale up/down limits from the hpaSpec.Behaviors (i.e. add no more than 4 pods)
// 3. Apply the constraints period (i.e. add no more than 4 pods per minute)
// 4. Apply the stabilization (i.e. add no more than 4 pods per minute, and pick the smallest recommendation during last 5 minutes)
func (a *HorizontalController) normalizeDesiredReplicasWithBehaviors(hpa *autoscalingv2.HorizontalPodAutoscaler, key string, currentReplicas, prenormalizedDesiredReplicas, minReplicas int32) int32 {
	a.maybeInitScaleDownStabilizationWindow(hpa)
	normalizationArg := NormalizationArg{
		Key:               key,
		ScaleUpBehavior:   hpa.Spec.Behavior.ScaleUp,
		ScaleDownBehavior: hpa.Spec.Behavior.ScaleDown,
		MinReplicas:       minReplicas,
		MaxReplicas:       hpa.Spec.MaxReplicas,
		CurrentReplicas:   currentReplicas,
		DesiredReplicas:   prenormalizedDesiredReplicas}
	stabilizedRecommendation, reason, message := a.stabilizeRecommendationWithBehaviors(normalizationArg)
	normalizationArg.DesiredReplicas = stabilizedRecommendation
	if stabilizedRecommendation != prenormalizedDesiredReplicas {
		// "ScaleUpStabilized" || "ScaleDownStabilized"
		setCondition(hpa, autoscalingv2.AbleToScale, v1.ConditionTrue, reason, "%s", message)
	} else {
		setCondition(hpa, autoscalingv2.AbleToScale, v1.ConditionTrue, "ReadyForNewScale", "recommended size matches current size")
	}
	desiredReplicas, reason, message := a.convertDesiredReplicasWithBehaviorRate(normalizationArg)
	if desiredReplicas == stabilizedRecommendation {
		setCondition(hpa, autoscalingv2.ScalingLimited, v1.ConditionFalse, reason, "%s", message)
	} else {
		setCondition(hpa, autoscalingv2.ScalingLimited, v1.ConditionTrue, reason, "%s", message)
	}

	return desiredReplicas
}

func (a *HorizontalController) maybeInitScaleDownStabilizationWindow(hpa *autoscalingv2.HorizontalPodAutoscaler) {
	behavior := hpa.Spec.Behavior
	if behavior != nil && behavior.ScaleDown != nil && behavior.ScaleDown.StabilizationWindowSeconds == nil {
		stabilizationWindowSeconds := (int32)(a.downscaleStabilisationWindow.Seconds())
		hpa.Spec.Behavior.ScaleDown.StabilizationWindowSeconds = &stabilizationWindowSeconds
	}
}

// getReplicasChangePerPeriod function find all the replica changes per period
func getReplicasChangePerPeriod(periodSeconds int32, scaleEvents []timestampedScaleEvent) int32 {
	period := time.Second * time.Duration(periodSeconds)
	cutoff := time.Now().Add(-period)
	var replicas int32
	for _, rec := range scaleEvents {
		if rec.timestamp.After(cutoff) {
			replicas += rec.replicaChange
		}
	}
	return replicas

}

func (a *HorizontalController) getUnableComputeReplicaCountCondition(hpa runtime.Object, reason string, err error) (condition autoscalingv2.HorizontalPodAutoscalerCondition) {
	a.eventRecorder.Event(hpa, v1.EventTypeWarning, reason, err.Error())
	return autoscalingv2.HorizontalPodAutoscalerCondition{
		Type:    autoscalingv2.ScalingActive,
		Status:  v1.ConditionFalse,
		Reason:  reason,
		Message: fmt.Sprintf("the HPA was unable to compute the replica count: %v", err),
	}
}

// storeScaleEvent stores (adds or replaces outdated) scale event.
// outdated events to be replaced were marked as outdated in the `markScaleEventsOutdated` function
func (a *HorizontalController) storeScaleEvent(behavior *autoscalingv2.HorizontalPodAutoscalerBehavior, key string, prevReplicas, newReplicas int32) {
	if behavior == nil {
		return // we should not store any event as they will not be used
	}
	var oldSampleIndex int
	var longestPolicyPeriod int32
	foundOldSample := false
	if newReplicas > prevReplicas {
		longestPolicyPeriod = getLongestPolicyPeriod(behavior.ScaleUp)

		a.scaleUpEventsLock.Lock()
		defer a.scaleUpEventsLock.Unlock()
		markScaleEventsOutdated(a.scaleUpEvents[key], longestPolicyPeriod)
		replicaChange := newReplicas - prevReplicas
		for i, event := range a.scaleUpEvents[key] {
			if event.outdated {
				foundOldSample = true
				oldSampleIndex = i
			}
		}
		newEvent := timestampedScaleEvent{replicaChange, time.Now(), false}
		if foundOldSample {
			a.scaleUpEvents[key][oldSampleIndex] = newEvent
		} else {
			a.scaleUpEvents[key] = append(a.scaleUpEvents[key], newEvent)
		}
	} else {
		longestPolicyPeriod = getLongestPolicyPeriod(behavior.ScaleDown)

		a.scaleDownEventsLock.Lock()
		defer a.scaleDownEventsLock.Unlock()
		markScaleEventsOutdated(a.scaleDownEvents[key], longestPolicyPeriod)
		replicaChange := prevReplicas - newReplicas
		for i, event := range a.scaleDownEvents[key] {
			if event.outdated {
				foundOldSample = true
				oldSampleIndex = i
			}
		}
		newEvent := timestampedScaleEvent{replicaChange, time.Now(), false}
		if foundOldSample {
			a.scaleDownEvents[key][oldSampleIndex] = newEvent
		} else {
			a.scaleDownEvents[key] = append(a.scaleDownEvents[key], newEvent)
		}
	}
}

// stabilizeRecommendationWithBehaviors:
// - replaces old recommendation with the newest recommendation,
// - returns {max,min} of recommendations that are not older than constraints.Scale{Up,Down}.DelaySeconds
func (a *HorizontalController) stabilizeRecommendationWithBehaviors(args NormalizationArg) (int32, string, string) {
	now := time.Now()

	foundOldSample := false
	oldSampleIndex := 0

	upRecommendation := args.DesiredReplicas
	upDelaySeconds := *args.ScaleUpBehavior.StabilizationWindowSeconds
	upCutoff := now.Add(-time.Second * time.Duration(upDelaySeconds))

	downRecommendation := args.DesiredReplicas
	downDelaySeconds := *args.ScaleDownBehavior.StabilizationWindowSeconds
	downCutoff := now.Add(-time.Second * time.Duration(downDelaySeconds))

	// Calculate the upper and lower stabilization limits.
	a.recommendationsLock.Lock()
	defer a.recommendationsLock.Unlock()
	for i, rec := range a.recommendations[args.Key] {
		if rec.timestamp.After(upCutoff) {
			upRecommendation = min(rec.recommendation, upRecommendation)
		}
		if rec.timestamp.After(downCutoff) {
			downRecommendation = max(rec.recommendation, downRecommendation)
		}
		if rec.timestamp.Before(upCutoff) && rec.timestamp.Before(downCutoff) {
			foundOldSample = true
			oldSampleIndex = i
		}
	}

	// Bring the recommendation to within the upper and lower limits (stabilize).
	recommendation := args.CurrentReplicas
	if recommendation < upRecommendation {
		recommendation = upRecommendation
	}
	if recommendation > downRecommendation {
		recommendation = downRecommendation
	}

	// Record the unstabilized recommendation.
	if foundOldSample {
		a.recommendations[args.Key][oldSampleIndex] = timestampedRecommendation{args.DesiredReplicas, time.Now()}
	} else {
		a.recommendations[args.Key] = append(a.recommendations[args.Key], timestampedRecommendation{args.DesiredReplicas, time.Now()})
	}

	// Determine a human-friendly message.
	var reason, message string
	if args.DesiredReplicas >= args.CurrentReplicas {
		reason = "ScaleUpStabilized"
		message = "recent recommendations were lower than current one, applying the lowest recent recommendation"
	} else {
		reason = "ScaleDownStabilized"
		message = "recent recommendations were higher than current one, applying the highest recent recommendation"
	}
	return recommendation, reason, message
}

// convertDesiredReplicasWithBehaviorRate performs the actual normalization, given the constraint rate
// It doesn't consider the stabilizationWindow, it is done separately
func (a *HorizontalController) convertDesiredReplicasWithBehaviorRate(args NormalizationArg) (int32, string, string) {
	var possibleLimitingReason, possibleLimitingMessage string

	if args.DesiredReplicas > args.CurrentReplicas {
		a.scaleUpEventsLock.RLock()
		defer a.scaleUpEventsLock.RUnlock()
		a.scaleDownEventsLock.RLock()
		defer a.scaleDownEventsLock.RUnlock()
		scaleUpLimit := calculateScaleUpLimitWithScalingRules(args.CurrentReplicas, a.scaleUpEvents[args.Key], a.scaleDownEvents[args.Key], args.ScaleUpBehavior)

		if scaleUpLimit < args.CurrentReplicas {
			// We shouldn't scale up further until the scaleUpEvents will be cleaned up
			scaleUpLimit = args.CurrentReplicas
		}
		maximumAllowedReplicas := args.MaxReplicas
		if maximumAllowedReplicas > scaleUpLimit {
			maximumAllowedReplicas = scaleUpLimit
			possibleLimitingReason = "ScaleUpLimit"
			possibleLimitingMessage = "the desired replica count is increasing faster than the maximum scale rate"
		} else {
			possibleLimitingReason = "TooManyReplicas"
			possibleLimitingMessage = "the desired replica count is more than the maximum replica count"
		}
		if args.DesiredReplicas > maximumAllowedReplicas {
			return maximumAllowedReplicas, possibleLimitingReason, possibleLimitingMessage
		}
	} else if args.DesiredReplicas < args.CurrentReplicas {
		a.scaleUpEventsLock.RLock()
		defer a.scaleUpEventsLock.RUnlock()
		a.scaleDownEventsLock.RLock()
		defer a.scaleDownEventsLock.RUnlock()
		scaleDownLimit := calculateScaleDownLimitWithBehaviors(args.CurrentReplicas, a.scaleUpEvents[args.Key], a.scaleDownEvents[args.Key], args.ScaleDownBehavior)

		if scaleDownLimit > args.CurrentReplicas {
			// We shouldn't scale down further until the scaleDownEvents will be cleaned up
			scaleDownLimit = args.CurrentReplicas
		}
		minimumAllowedReplicas := args.MinReplicas
		if minimumAllowedReplicas < scaleDownLimit {
			minimumAllowedReplicas = scaleDownLimit
			possibleLimitingReason = "ScaleDownLimit"
			possibleLimitingMessage = "the desired replica count is decreasing faster than the maximum scale rate"
		} else {
			possibleLimitingMessage = "the desired replica count is less than the minimum replica count"
			possibleLimitingReason = "TooFewReplicas"
		}
		if args.DesiredReplicas < minimumAllowedReplicas {
			return minimumAllowedReplicas, possibleLimitingReason, possibleLimitingMessage
		}
	}
	return args.DesiredReplicas, "DesiredWithinRange", "the desired count is within the acceptable range"
}

// convertDesiredReplicas performs the actual normalization, without depending on `HorizontalController` or `HorizontalPodAutoscaler`
func convertDesiredReplicasWithRules(currentReplicas, desiredReplicas, hpaMinReplicas, hpaMaxReplicas int32) (int32, string, string) {

	var minimumAllowedReplicas int32
	var maximumAllowedReplicas int32

	var possibleLimitingCondition string
	var possibleLimitingReason string

	minimumAllowedReplicas = hpaMinReplicas

	// Do not scaleup too much to prevent incorrect rapid increase of the number of master replicas caused by
	// bogus CPU usage report from heapster/kubelet (like in issue #32304).
	scaleUpLimit := calculateScaleUpLimit(currentReplicas)

	if hpaMaxReplicas > scaleUpLimit {
		maximumAllowedReplicas = scaleUpLimit
		possibleLimitingCondition = "ScaleUpLimit"
		possibleLimitingReason = "the desired replica count is increasing faster than the maximum scale rate"
	} else {
		maximumAllowedReplicas = hpaMaxReplicas
		possibleLimitingCondition = "TooManyReplicas"
		possibleLimitingReason = "the desired replica count is more than the maximum replica count"
	}

	if desiredReplicas < minimumAllowedReplicas {
		possibleLimitingCondition = "TooFewReplicas"
		possibleLimitingReason = "the desired replica count is less than the minimum replica count"

		return minimumAllowedReplicas, possibleLimitingCondition, possibleLimitingReason
	} else if desiredReplicas > maximumAllowedReplicas {
		return maximumAllowedReplicas, possibleLimitingCondition, possibleLimitingReason
	}

	return desiredReplicas, "DesiredWithinRange", "the desired count is within the acceptable range"
}

func calculateScaleUpLimit(currentReplicas int32) int32 {
	return int32(math.Max(scaleUpLimitFactor*float64(currentReplicas), scaleUpLimitMinimum))
}

// markScaleEventsOutdated set 'outdated=true' flag for all scale events that are not used by any HPA object
func markScaleEventsOutdated(scaleEvents []timestampedScaleEvent, longestPolicyPeriod int32) {
	period := time.Second * time.Duration(longestPolicyPeriod)
	cutoff := time.Now().Add(-period)
	for i, event := range scaleEvents {
		if event.timestamp.Before(cutoff) {
			// outdated scale event are marked for later reuse
			scaleEvents[i].outdated = true
		}
	}
}

func getLongestPolicyPeriod(scalingRules *autoscalingv2.HPAScalingRules) int32 {
	var longestPolicyPeriod int32
	for _, policy := range scalingRules.Policies {
		if policy.PeriodSeconds > longestPolicyPeriod {
			longestPolicyPeriod = policy.PeriodSeconds
		}
	}
	return longestPolicyPeriod
}

// calculateScaleUpLimitWithScalingRules returns the maximum number of pods that could be added for the given HPAScalingRules
func calculateScaleUpLimitWithScalingRules(currentReplicas int32, scaleUpEvents, scaleDownEvents []timestampedScaleEvent, scalingRules *autoscalingv2.HPAScalingRules) int32 {
	var result int32
	var proposed int32
	var selectPolicyFn func(int32, int32) int32
	if *scalingRules.SelectPolicy == autoscalingv2.DisabledPolicySelect {
		return currentReplicas // Scaling is disabled
	} else if *scalingRules.SelectPolicy == autoscalingv2.MinChangePolicySelect {
		result = math.MaxInt32
		selectPolicyFn = minInt32 // For scaling up, the lowest change ('min' policy) produces a minimum value
	} else {
		result = math.MinInt32
		selectPolicyFn = maxInt32 // Use the default policy otherwise to produce a highest possible change
	}
	for _, policy := range scalingRules.Policies {
		replicasAddedInCurrentPeriod := getReplicasChangePerPeriod(policy.PeriodSeconds, scaleUpEvents)
		replicasDeletedInCurrentPeriod := getReplicasChangePerPeriod(policy.PeriodSeconds, scaleDownEvents)
		periodStartReplicas := currentReplicas - replicasAddedInCurrentPeriod + replicasDeletedInCurrentPeriod
		if policy.Type == autoscalingv2.PodsScalingPolicy {
			proposed = periodStartReplicas + policy.Value
		} else if policy.Type == autoscalingv2.PercentScalingPolicy {
			// the proposal has to be rounded up because the proposed change might not increase the replica count causing the target to never scale up
			proposed = int32(math.Ceil(float64(periodStartReplicas) * (1 + float64(policy.Value)/100)))
		}
		result = selectPolicyFn(result, proposed)
	}
	return result
}

// calculateScaleDownLimitWithBehavior returns the maximum number of pods that could be deleted for the given HPAScalingRules
func calculateScaleDownLimitWithBehaviors(currentReplicas int32, scaleUpEvents, scaleDownEvents []timestampedScaleEvent, scalingRules *autoscalingv2.HPAScalingRules) int32 {
	var result int32
	var proposed int32
	var selectPolicyFn func(int32, int32) int32
	if *scalingRules.SelectPolicy == autoscalingv2.DisabledPolicySelect {
		return currentReplicas // Scaling is disabled
	} else if *scalingRules.SelectPolicy == autoscalingv2.MinChangePolicySelect {
		result = math.MinInt32
		selectPolicyFn = maxInt32 // For scaling down, the lowest change ('min' policy) produces a maximum value
	} else {
		result = math.MaxInt32
		selectPolicyFn = minInt32 // Use the default policy otherwise to produce a highest possible change
	}
	for _, policy := range scalingRules.Policies {
		replicasAddedInCurrentPeriod := getReplicasChangePerPeriod(policy.PeriodSeconds, scaleUpEvents)
		replicasDeletedInCurrentPeriod := getReplicasChangePerPeriod(policy.PeriodSeconds, scaleDownEvents)
		periodStartReplicas := currentReplicas - replicasAddedInCurrentPeriod + replicasDeletedInCurrentPeriod
		if policy.Type == autoscalingv2.PodsScalingPolicy {
			proposed = periodStartReplicas - policy.Value
		} else if policy.Type == autoscalingv2.PercentScalingPolicy {
			proposed = int32(float64(periodStartReplicas) * (1 - float64(policy.Value)/100))
		}
		result = selectPolicyFn(result, proposed)
	}
	return result
}

// scaleForResourceMappings attempts to fetch the scale for the
// resource with the given name and namespace, trying each RESTMapping
// in turn until a working one is found.  If none work, the first error
// is returned.  It returns both the scale, as well as the group-resource from
// the working mapping.
func (a *HorizontalController) scaleForResourceMappings(ctx context.Context, namespace, name string, mappings []*apimeta.RESTMapping) (*autoscalingv1.Scale, schema.GroupResource, error) {
	var firstErr error
	for i, mapping := range mappings {
		targetGR := mapping.Resource.GroupResource()
		scale, err := a.scaleNamespacer.Scales(namespace).Get(ctx, targetGR, name, metav1.GetOptions{})
		if err == nil {
			return scale, targetGR, nil
		}

		// if this is the first error, remember it,
		// then go on and try other mappings until we find a good one
		if i == 0 {
			firstErr = err
		}
	}

	// make sure we handle an empty set of mappings
	if firstErr == nil {
		firstErr = fmt.Errorf("unrecognized resource")
	}

	return nil, schema.GroupResource{}, firstErr
}

// setCurrentReplicasAndMetricsInStatus sets the current replica count and metrics in the status of the HPA.
func (a *HorizontalController) setCurrentReplicasAndMetricsInStatus(hpa *autoscalingv2.HorizontalPodAutoscaler, currentReplicas int32, metricStatuses []autoscalingv2.MetricStatus) {
	a.setStatus(hpa, currentReplicas, hpa.Status.DesiredReplicas, metricStatuses, false)
}

// setStatus recreates the status of the given HPA, updating the current and
// desired replicas, as well as the metric statuses
func (a *HorizontalController) setStatus(hpa *autoscalingv2.HorizontalPodAutoscaler, currentReplicas, desiredReplicas int32, metricStatuses []autoscalingv2.MetricStatus, rescale bool) {
	hpa.Status = autoscalingv2.HorizontalPodAutoscalerStatus{
		CurrentReplicas: currentReplicas,
		DesiredReplicas: desiredReplicas,
		LastScaleTime:   hpa.Status.LastScaleTime,
		CurrentMetrics:  metricStatuses,
		Conditions:      hpa.Status.Conditions,
	}

	if rescale {
		now := metav1.NewTime(time.Now())
		hpa.Status.LastScaleTime = &now
	}
}

// updateStatusIfNeeded calls updateStatus only if the status of the new HPA is not the same as the old status
func (a *HorizontalController) updateStatusIfNeeded(ctx context.Context, oldStatus *autoscalingv2.HorizontalPodAutoscalerStatus, newHPA *autoscalingv2.HorizontalPodAutoscaler) error {
	// skip a write if we wouldn't need to update
	if apiequality.Semantic.DeepEqual(oldStatus, &newHPA.Status) {
		return nil
	}
	return a.updateStatus(ctx, newHPA)
}

// updateStatus actually does the update request for the status of the given HPA
func (a *HorizontalController) updateStatus(ctx context.Context, hpa *autoscalingv2.HorizontalPodAutoscaler) error {
	_, err := a.hpaNamespacer.HorizontalPodAutoscalers(hpa.Namespace).UpdateStatus(ctx, hpa, metav1.UpdateOptions{})
	if err != nil {
		a.eventRecorder.Event(hpa, v1.EventTypeWarning, "FailedUpdateStatus", err.Error())
		return fmt.Errorf("failed to update status for %s: %v", hpa.Name, err)
	}
	logger := klog.FromContext(ctx)
	logger.V(2).Info("Successfully updated status", "HPA", klog.KObj(hpa))
	return nil
}

// setCondition sets the specific condition type on the given HPA to the specified value with the given reason
// and message.  The message and args are treated like a format string.  The condition will be added if it is
// not present.
func setCondition(hpa *autoscalingv2.HorizontalPodAutoscaler, conditionType autoscalingv2.HorizontalPodAutoscalerConditionType, status v1.ConditionStatus, reason, message string, args ...interface{}) {
	hpa.Status.Conditions = setConditionInList(hpa.Status.Conditions, conditionType, status, reason, message, args...)
}

// setConditionInList sets the specific condition type on the given HPA to the specified value with the given
// reason and message.  The message and args are treated like a format string.  The condition will be added if
// it is not present.  The new list will be returned.
func setConditionInList(inputList []autoscalingv2.HorizontalPodAutoscalerCondition, conditionType autoscalingv2.HorizontalPodAutoscalerConditionType, status v1.ConditionStatus, reason, message string, args ...interface{}) []autoscalingv2.HorizontalPodAutoscalerCondition {
	resList := inputList
	var existingCond *autoscalingv2.HorizontalPodAutoscalerCondition
	for i, condition := range resList {
		if condition.Type == conditionType {
			// can't take a pointer to an iteration variable
			existingCond = &resList[i]
			break
		}
	}

	if existingCond == nil {
		resList = append(resList, autoscalingv2.HorizontalPodAutoscalerCondition{
			Type: conditionType,
		})
		existingCond = &resList[len(resList)-1]
	}

	if existingCond.Status != status {
		existingCond.LastTransitionTime = metav1.Now()
	}

	existingCond.Status = status
	existingCond.Reason = reason
	existingCond.Message = fmt.Sprintf(message, args...)

	return resList
}

// minInt32 is a wrapper around the min builtin to be used as a function value.
func minInt32(a, b int32) int32 {
	return min(a, b)
}

// maxInt32 is a wrapper around the max builtin to be used as a function value.
func maxInt32(a, b int32) int32 {
	return max(a, b)
}
