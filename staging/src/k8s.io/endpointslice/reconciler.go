/*
Copyright 2019 The Kubernetes Authors.

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

package endpointslice

import (
	"context"
	"fmt"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/conversion"
	"k8s.io/apimachinery/pkg/types"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/sets"
	clientset "k8s.io/client-go/kubernetes"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/record"
	"k8s.io/endpointslice/metrics"
	"k8s.io/endpointslice/topologycache"
	"k8s.io/endpointslice/trafficdist"
	endpointsliceutil "k8s.io/endpointslice/util"
	"k8s.io/klog/v2"
)

// Reconciler is responsible for transforming current EndpointSlice state into
// desired state
type Reconciler struct {
	client               clientset.Interface
	nodeLister           corelisters.NodeLister
	maxEndpointsPerSlice int32
	endpointSliceTracker *endpointsliceutil.EndpointSliceTracker
	metricsCache         *metrics.Cache
	// topologyCache tracks the distribution of Nodes and endpoints across zones
	// to enable TopologyAwareHints.
	topologyCache *topologycache.TopologyCache
	// trafficDistributionEnabled determines if endpointDistribution field is to
	// be considered when reconciling EndpointSlice hints.
	trafficDistributionEnabled bool
	// eventRecorder allows Reconciler to record and publish events.
	eventRecorder  record.EventRecorder
	controllerName string
}

type ReconcilerOption func(*Reconciler)

// WithTrafficDistributionEnabled controls whether the Reconciler considers the
// `trafficDistribution` field while reconciling EndpointSlices.
func WithTrafficDistributionEnabled(enabled bool) ReconcilerOption {
	return func(r *Reconciler) {
		r.trafficDistributionEnabled = enabled
	}
}

// endpointMeta includes the attributes we group slices on, this type helps with
// that logic in Reconciler
type endpointMeta struct {
	ports       []discovery.EndpointPort
	addressType discovery.AddressType
}

// Reconcile takes a set of pods currently matching a service selector and
// compares them with the endpoints already present in any existing endpoint
// slices for the given service. It creates, updates, or deletes endpoint slices
// to ensure the desired set of pods are represented by endpoint slices.
func (r *Reconciler) Reconcile(logger klog.Logger, service *corev1.Service, pods []*corev1.Pod, existingSlices []*discovery.EndpointSlice, triggerTime time.Time) error {
	slicesToDelete := []*discovery.EndpointSlice{}                                    // slices that are no longer  matching any address the service has
	errs := []error{}                                                                 // all errors generated in the process of reconciling
	slicesByAddressType := make(map[discovery.AddressType][]*discovery.EndpointSlice) // slices by address type

	// addresses that this service supports [o(1) find]
	serviceSupportedAddressesTypes := getAddressTypesForService(logger, service)

	// loop through slices identifying their address type.
	// slices that no longer match address type supported by services
	// go to delete, other slices goes to the Reconciler machinery
	// for further adjustment
	for _, existingSlice := range existingSlices {
		// service no longer supports that address type, add it to deleted slices
		if !serviceSupportedAddressesTypes.Has(existingSlice.AddressType) {
			if r.topologyCache != nil {
				svcKey, err := ServiceControllerKey(existingSlice)
				if err != nil {
					logger.Info("Couldn't get key to remove EndpointSlice from topology cache", "existingSlice", existingSlice, "err", err)
				} else {
					r.topologyCache.RemoveHints(svcKey, existingSlice.AddressType)
				}
			}

			slicesToDelete = append(slicesToDelete, existingSlice)
			continue
		}

		// add list if it is not on our map
		if _, ok := slicesByAddressType[existingSlice.AddressType]; !ok {
			slicesByAddressType[existingSlice.AddressType] = make([]*discovery.EndpointSlice, 0, 1)
		}

		slicesByAddressType[existingSlice.AddressType] = append(slicesByAddressType[existingSlice.AddressType], existingSlice)
	}

	spMetrics := metrics.NewServicePortCache()

	// reconcile for existing.
	for addressType := range serviceSupportedAddressesTypes {
		existingSlices := slicesByAddressType[addressType]
		err := r.reconcileByAddressType(logger, service, pods, existingSlices, triggerTime, addressType, spMetrics)
		if err != nil {
			errs = append(errs, err)
		}
	}

	serviceNN := types.NamespacedName{Name: service.Name, Namespace: service.Namespace}
	r.metricsCache.UpdateServicePortCache(serviceNN, spMetrics)

	// delete those which are of addressType that is no longer supported
	// by the service
	for _, sliceToDelete := range slicesToDelete {
		err := r.client.DiscoveryV1().EndpointSlices(service.Namespace).Delete(context.TODO(), sliceToDelete.Name, metav1.DeleteOptions{})
		if err != nil {
			errs = append(errs, fmt.Errorf("error deleting %s EndpointSlice for Service %s/%s: %w", sliceToDelete.Name, service.Namespace, service.Name, err))
		} else {
			r.endpointSliceTracker.ExpectDeletion(sliceToDelete)
			metrics.EndpointSliceChanges.WithLabelValues("delete").Inc()
		}
	}

	return utilerrors.NewAggregate(errs)
}

// reconcileByAddressType takes a set of pods currently matching a service selector and
// compares them with the endpoints already present in any existing endpoint
// slices (by address type) for the given service. It creates, updates, or deletes endpoint slices
// to ensure the desired set of pods are represented by endpoint slices.
func (r *Reconciler) reconcileByAddressType(logger klog.Logger, service *corev1.Service, pods []*corev1.Pod, existingSlices []*discovery.EndpointSlice, triggerTime time.Time, addressType discovery.AddressType, spMetrics *metrics.ServicePortCache) error {
	errs := []error{}

	slicesToCreate := []*discovery.EndpointSlice{}
	slicesToUpdate := []*discovery.EndpointSlice{}
	slicesToDelete := []*discovery.EndpointSlice{}
	events := []*topologycache.EventBuilder{}

	// Build data structures for existing state.
	existingSlicesByPortMap := map[endpointsliceutil.PortMapKey][]*discovery.EndpointSlice{}
	for _, existingSlice := range existingSlices {
		if ownedBy(existingSlice, service) {
			epHash := endpointsliceutil.NewPortMapKey(existingSlice.Ports)
			existingSlicesByPortMap[epHash] = append(existingSlicesByPortMap[epHash], existingSlice)
		} else {
			slicesToDelete = append(slicesToDelete, existingSlice)
		}
	}

	// Build data structures for desired state.
	desiredMetaByPortMap := map[endpointsliceutil.PortMapKey]*endpointMeta{}
	desiredEndpointsByPortMap := map[endpointsliceutil.PortMapKey]endpointsliceutil.EndpointSet{}

	for _, pod := range pods {
		if !endpointsliceutil.ShouldPodBeInEndpoints(pod, true) {
			continue
		}

		endpointPorts := getEndpointPorts(logger, service, pod)
		epHash := endpointsliceutil.NewPortMapKey(endpointPorts)
		if _, ok := desiredEndpointsByPortMap[epHash]; !ok {
			desiredEndpointsByPortMap[epHash] = endpointsliceutil.EndpointSet{}
		}

		if _, ok := desiredMetaByPortMap[epHash]; !ok {
			desiredMetaByPortMap[epHash] = &endpointMeta{
				addressType: addressType,
				ports:       endpointPorts,
			}
		}

		node, err := r.nodeLister.Get(pod.Spec.NodeName)
		if err != nil {
			// we are getting the information from the local informer,
			// an error different than IsNotFound should not happen
			if !errors.IsNotFound(err) {
				return err
			}
			// If the Node specified by the Pod doesn't exist we want to requeue the Service so we
			// retry later, but also update the EndpointSlice without the problematic Pod.
			// Theoretically, the pod Garbage Collector will remove the Pod, but we want to avoid
			// situations where a reference from a Pod to a missing node can leave the EndpointSlice
			// stuck forever.
			// On the other side, if the service.Spec.PublishNotReadyAddresses is set we just add the
			// Pod, since the user is explicitly indicating that the Pod address should be published.
			if !service.Spec.PublishNotReadyAddresses {
				logger.Info("skipping Pod for Service, Node not found", "pod", klog.KObj(pod), "service", klog.KObj(service), "node", klog.KRef("", pod.Spec.NodeName))
				errs = append(errs, fmt.Errorf("skipping Pod %s for Service %s/%s: Node %s Not Found", pod.Name, service.Namespace, service.Name, pod.Spec.NodeName))
				continue
			}
		}
		endpoint := podToEndpoint(pod, node, service, addressType)
		if len(endpoint.Addresses) > 0 {
			desiredEndpointsByPortMap[epHash].Insert(&endpoint)
		}
	}

	totalAdded := 0
	totalRemoved := 0

	// Determine changes necessary for each group of slices by port map.
	for portMap, desiredEndpoints := range desiredEndpointsByPortMap {
		numEndpoints := len(desiredEndpoints)
		pmSlicesToCreate, pmSlicesToUpdate, pmSlicesToDelete, added, removed := r.reconcileByPortMapping(
			logger, service, existingSlicesByPortMap[portMap], desiredEndpoints, desiredMetaByPortMap[portMap])

		totalAdded += added
		totalRemoved += removed

		spMetrics.Set(
			newAddrTypePortMapKey(portMap, addressType),
			metrics.EfficiencyInfo{
				Endpoints: numEndpoints,
				Slices:    len(existingSlicesByPortMap[portMap]) + len(pmSlicesToCreate) - len(pmSlicesToDelete),
			})

		slicesToCreate = append(slicesToCreate, pmSlicesToCreate...)
		slicesToUpdate = append(slicesToUpdate, pmSlicesToUpdate...)
		slicesToDelete = append(slicesToDelete, pmSlicesToDelete...)
	}

	// If there are unique sets of ports that are no longer desired, mark
	// the corresponding endpoint slices for deletion.
	for portMap, existingSlices := range existingSlicesByPortMap {
		if _, ok := desiredEndpointsByPortMap[portMap]; !ok {
			slicesToDelete = append(slicesToDelete, existingSlices...)
		}
	}

	// When no endpoint slices would usually exist, we need to add a placeholder.
	if len(existingSlices) == len(slicesToDelete) && len(slicesToCreate) < 1 {
		// Check for existing placeholder slice outside of the core control flow
		placeholderSlice := newEndpointSlice(logger, service, &endpointMeta{ports: []discovery.EndpointPort{}, addressType: addressType}, r.controllerName)
		if len(slicesToDelete) == 1 && placeholderSliceCompare.DeepEqual(slicesToDelete[0], placeholderSlice) {
			// We are about to unnecessarily delete/recreate the placeholder, remove it now.
			slicesToDelete = slicesToDelete[:0]
		} else {
			slicesToCreate = append(slicesToCreate, placeholderSlice)
		}
		spMetrics.Set(
			newAddrTypePortMapKey(endpointsliceutil.NewPortMapKey(placeholderSlice.Ports), addressType),
			metrics.EfficiencyInfo{
				Endpoints: 0,
				Slices:    1,
			})
	}

	metrics.EndpointsAddedPerSync.WithLabelValues().Observe(float64(totalAdded))
	metrics.EndpointsRemovedPerSync.WithLabelValues().Observe(float64(totalRemoved))

	serviceNN := types.NamespacedName{Name: service.Name, Namespace: service.Namespace}

	// Topology hints are assigned per address type. This means it is
	// theoretically possible for endpoints of one address type to be assigned
	// hints while another endpoints of another address type are not.
	si := &topologycache.SliceInfo{
		ServiceKey:  fmt.Sprintf("%s/%s", service.Namespace, service.Name),
		AddressType: addressType,
		ToCreate:    slicesToCreate,
		ToUpdate:    slicesToUpdate,
		Unchanged:   unchangedSlices(existingSlices, slicesToUpdate, slicesToDelete),
	}

	canUseTrafficDistribution := r.trafficDistributionEnabled && !hintsEnabled(service.Annotations)

	// Check if we need to add/remove hints based on the topology annotation.
	//
	// This if/else clause can be removed once the annotation has been deprecated.
	// Ref: https://github.com/kubernetes/enhancements/tree/master/keps/sig-network/4444-service-routing-preference
	if r.topologyCache != nil && hintsEnabled(service.Annotations) {
		// Reaching this point means that we need to configure hints based on the
		// topology annotation.
		slicesToCreate, slicesToUpdate, events = r.topologyCache.AddHints(logger, si)

	} else {
		// Reaching this point means that we will not be configuring hints based on
		// the topology annotation. We need to do 2 things:
		//  1. If hints were added previously based on the annotation, we need to
		//     clear up any locally cached hints from the topologyCache object.
		//  2. Optionally remove the actual hints from the EndpointSlice if we know
		//     that the `trafficDistribution` field is also NOT being used. In other
		//     words, if we know that the `trafficDistribution` field has been
		//     correctly configured by the customer, we DO NOT remove the hints and
		//     wait for the trafficDist handlers to correctly configure them. Always
		//     unconditionally removing hints here (and letting them get readded by
		//     the trafficDist) adds extra overhead in the form of DeepCopy (done
		//     within topologyCache.RemoveHints)

		// Check 1.
		if r.topologyCache != nil {
			if r.topologyCache.HasPopulatedHints(si.ServiceKey) {
				logger.Info("TopologyAwareHints annotation has changed, removing hints", "serviceKey", si.ServiceKey, "addressType", si.AddressType)
				events = append(events, &topologycache.EventBuilder{
					EventType: corev1.EventTypeWarning,
					Reason:    "TopologyAwareHintsDisabled",
					Message:   topologycache.FormatWithAddressType(topologycache.TopologyAwareHintsDisabled, si.AddressType),
				})
			}
			r.topologyCache.RemoveHints(si.ServiceKey, addressType)
		}

		// Check 2.
		if !canUseTrafficDistribution {
			slicesToCreate, slicesToUpdate = topologycache.RemoveHintsFromSlices(si)
		}
	}

	if canUseTrafficDistribution {
		r.metricsCache.UpdateTrafficDistributionForService(serviceNN, service.Spec.TrafficDistribution)
		slicesToCreate, slicesToUpdate, _ = trafficdist.ReconcileHints(service.Spec.TrafficDistribution, slicesToCreate, slicesToUpdate, unchangedSlices(existingSlices, slicesToUpdate, slicesToDelete))
	} else {
		r.metricsCache.UpdateTrafficDistributionForService(serviceNN, nil)
	}

	err := r.finalize(service, slicesToCreate, slicesToUpdate, slicesToDelete, triggerTime)
	if err != nil {
		errs = append(errs, err)
	}
	for _, event := range events {
		r.eventRecorder.Event(service, event.EventType, event.Reason, event.Message)
	}
	return utilerrors.NewAggregate(errs)

}

func NewReconciler(client clientset.Interface, nodeLister corelisters.NodeLister, maxEndpointsPerSlice int32, endpointSliceTracker *endpointsliceutil.EndpointSliceTracker, topologyCache *topologycache.TopologyCache, eventRecorder record.EventRecorder, controllerName string, options ...ReconcilerOption) *Reconciler {
	r := &Reconciler{
		client:               client,
		nodeLister:           nodeLister,
		maxEndpointsPerSlice: maxEndpointsPerSlice,
		endpointSliceTracker: endpointSliceTracker,
		metricsCache:         metrics.NewCache(maxEndpointsPerSlice),
		topologyCache:        topologyCache,
		eventRecorder:        eventRecorder,
		controllerName:       controllerName,
	}
	for _, option := range options {
		option(r)
	}
	return r
}

// placeholderSliceCompare is a conversion func for comparing two placeholder endpoint slices.
// It only compares the specific fields we care about.
var placeholderSliceCompare = conversion.EqualitiesOrDie(
	func(a, b metav1.OwnerReference) bool {
		return a.String() == b.String()
	},
	func(a, b metav1.ObjectMeta) bool {
		if a.Namespace != b.Namespace {
			return false
		}
		for k, v := range a.Labels {
			if b.Labels[k] != v {
				return false
			}
		}
		for k, v := range b.Labels {
			if a.Labels[k] != v {
				return false
			}
		}
		return true
	},
)

// finalize creates, updates, and deletes slices as specified
func (r *Reconciler) finalize(
	service *corev1.Service,
	slicesToCreate,
	slicesToUpdate,
	slicesToDelete []*discovery.EndpointSlice,
	triggerTime time.Time,
) error {
	// If there are slices to create and delete, change the creates to updates
	// of the slices that would otherwise be deleted.
	for i := 0; i < len(slicesToDelete); {
		if len(slicesToCreate) == 0 {
			break
		}
		sliceToDelete := slicesToDelete[i]
		slice := slicesToCreate[len(slicesToCreate)-1]
		// Only update EndpointSlices that are owned by this Service and have
		// the same AddressType. We need to avoid updating EndpointSlices that
		// are being garbage collected for an old Service with the same name.
		// The AddressType field is immutable. Since Services also consider
		// IPFamily immutable, the only case where this should matter will be
		// the migration from IP to IPv4 and IPv6 AddressTypes, where there's a
		// chance EndpointSlices with an IP AddressType would otherwise be
		// updated to IPv4 or IPv6 without this check.
		if sliceToDelete.AddressType == slice.AddressType && ownedBy(sliceToDelete, service) {
			slice.Name = sliceToDelete.Name
			slicesToCreate = slicesToCreate[:len(slicesToCreate)-1]
			slicesToUpdate = append(slicesToUpdate, slice)
			slicesToDelete = append(slicesToDelete[:i], slicesToDelete[i+1:]...)
		} else {
			i++
		}
	}

	// Don't create new EndpointSlices if the Service is pending deletion. This
	// is to avoid a potential race condition with the garbage collector where
	// it tries to delete EndpointSlices as this controller replaces them.
	if service.DeletionTimestamp == nil {
		for _, endpointSlice := range slicesToCreate {
			addTriggerTimeAnnotation(endpointSlice, triggerTime)
			createdSlice, err := r.client.DiscoveryV1().EndpointSlices(service.Namespace).Create(context.TODO(), endpointSlice, metav1.CreateOptions{})
			if err != nil {
				// If the namespace is terminating, creates will continue to fail. Simply drop the item.
				if errors.HasStatusCause(err, corev1.NamespaceTerminatingCause) {
					return nil
				}
				return fmt.Errorf("failed to create EndpointSlice for Service %s/%s: %v", service.Namespace, service.Name, err)
			}
			r.endpointSliceTracker.Update(createdSlice)
			metrics.EndpointSliceChanges.WithLabelValues("create").Inc()
		}
	}

	for _, endpointSlice := range slicesToUpdate {
		addTriggerTimeAnnotation(endpointSlice, triggerTime)
		updatedSlice, err := r.client.DiscoveryV1().EndpointSlices(service.Namespace).Update(context.TODO(), endpointSlice, metav1.UpdateOptions{})
		if err != nil {
			return fmt.Errorf("failed to update %s EndpointSlice for Service %s/%s: %v", endpointSlice.Name, service.Namespace, service.Name, err)
		}
		r.endpointSliceTracker.Update(updatedSlice)
		metrics.EndpointSliceChanges.WithLabelValues("update").Inc()
	}

	for _, endpointSlice := range slicesToDelete {
		err := r.client.DiscoveryV1().EndpointSlices(service.Namespace).Delete(context.TODO(), endpointSlice.Name, metav1.DeleteOptions{})
		if err != nil {
			return fmt.Errorf("failed to delete %s EndpointSlice for Service %s/%s: %v", endpointSlice.Name, service.Namespace, service.Name, err)
		}
		r.endpointSliceTracker.ExpectDeletion(endpointSlice)
		metrics.EndpointSliceChanges.WithLabelValues("delete").Inc()
	}

	topologyLabel := "Disabled"
	if r.topologyCache != nil && hintsEnabled(service.Annotations) {
		topologyLabel = "Auto"
	}
	var trafficDistribution string
	if r.trafficDistributionEnabled && !hintsEnabled(service.Annotations) {
		if service.Spec.TrafficDistribution != nil && *service.Spec.TrafficDistribution == corev1.ServiceTrafficDistributionPreferClose {
			trafficDistribution = *service.Spec.TrafficDistribution
		}
	}

	numSlicesChanged := len(slicesToCreate) + len(slicesToUpdate) + len(slicesToDelete)
	metrics.EndpointSlicesChangedPerSync.WithLabelValues(topologyLabel, trafficDistribution).Observe(float64(numSlicesChanged))

	return nil
}

// reconcileByPortMapping compares the endpoints found in existing slices with
// the list of desired endpoints and returns lists of slices to create, update,
// and delete. It also checks that the slices mirror the parent services labels.
// The logic is split up into several main steps:
//  1. Iterate through existing slices, delete endpoints that are no longer
//     desired and update matching endpoints that have changed. It also checks
//     if the slices have the labels of the parent services, and updates them if not.
//  2. Iterate through slices that have been modified in 1 and fill them up with
//     any remaining desired endpoints.
//  3. If there still desired endpoints left, try to fit them into a previously
//     unchanged slice and/or create new ones.
func (r *Reconciler) reconcileByPortMapping(
	logger klog.Logger,
	service *corev1.Service,
	existingSlices []*discovery.EndpointSlice,
	desiredSet endpointsliceutil.EndpointSet,
	endpointMeta *endpointMeta,
) ([]*discovery.EndpointSlice, []*discovery.EndpointSlice, []*discovery.EndpointSlice, int, int) {
	slicesByName := map[string]*discovery.EndpointSlice{}
	sliceNamesUnchanged := sets.New[string]()
	sliceNamesToUpdate := sets.New[string]()
	sliceNamesToDelete := sets.New[string]()
	numRemoved := 0

	// 1. Iterate through existing slices to delete endpoints no longer desired
	//    and update endpoints that have changed
	for _, existingSlice := range existingSlices {
		slicesByName[existingSlice.Name] = existingSlice
		newEndpoints := []discovery.Endpoint{}
		endpointUpdated := false
		for _, endpoint := range existingSlice.Endpoints {
			got := desiredSet.Get(&endpoint)
			// If endpoint is desired add it to list of endpoints to keep.
			if got != nil {
				newEndpoints = append(newEndpoints, *got)
				// If existing version of endpoint doesn't match desired version
				// set endpointUpdated to ensure endpoint changes are persisted.
				if !endpointsliceutil.EndpointsEqualBeyondHash(got, &endpoint) {
					endpointUpdated = true
				}
				// once an endpoint has been placed/found in a slice, it no
				// longer needs to be handled
				desiredSet.Delete(&endpoint)
			}
		}

		// generate the slice labels and check if parent labels have changed
		labels, labelsChanged := setEndpointSliceLabels(logger, existingSlice, service, r.controllerName)

		// If an endpoint was updated or removed, mark for update or delete
		if endpointUpdated || len(existingSlice.Endpoints) != len(newEndpoints) {
			if len(existingSlice.Endpoints) > len(newEndpoints) {
				numRemoved += len(existingSlice.Endpoints) - len(newEndpoints)
			}
			if len(newEndpoints) == 0 {
				// if no endpoints desired in this slice, mark for deletion
				sliceNamesToDelete.Insert(existingSlice.Name)
			} else {
				// otherwise, copy and mark for update
				epSlice := existingSlice.DeepCopy()
				epSlice.Endpoints = newEndpoints
				epSlice.Labels = labels
				slicesByName[existingSlice.Name] = epSlice
				sliceNamesToUpdate.Insert(epSlice.Name)
			}
		} else if labelsChanged {
			// if labels have changed, copy and mark for update
			epSlice := existingSlice.DeepCopy()
			epSlice.Labels = labels
			slicesByName[existingSlice.Name] = epSlice
			sliceNamesToUpdate.Insert(epSlice.Name)
		} else {
			// slices with no changes will be useful if there are leftover endpoints
			sliceNamesUnchanged.Insert(existingSlice.Name)
		}
	}

	numAdded := desiredSet.Len()

	// 2. If we still have desired endpoints to add and slices marked for update,
	//    iterate through the slices and fill them up with the desired endpoints.
	if desiredSet.Len() > 0 && sliceNamesToUpdate.Len() > 0 {
		slices := []*discovery.EndpointSlice{}
		for _, sliceName := range sliceNamesToUpdate.UnsortedList() {
			slices = append(slices, slicesByName[sliceName])
		}
		// Sort endpoint slices by length so we're filling up the fullest ones
		// first.
		sort.Sort(endpointSliceEndpointLen(slices))

		// Iterate through slices and fill them up with desired endpoints.
		for _, slice := range slices {
			for desiredSet.Len() > 0 && len(slice.Endpoints) < int(r.maxEndpointsPerSlice) {
				endpoint, _ := desiredSet.PopAny()
				slice.Endpoints = append(slice.Endpoints, *endpoint)
			}
		}
	}

	// 3. If there are still desired endpoints left at this point, we try to fit
	//    the endpoints in a single existing slice. If there are no slices with
	//    that capacity, we create new slices for the endpoints.
	slicesToCreate := []*discovery.EndpointSlice{}

	for desiredSet.Len() > 0 {
		var sliceToFill *discovery.EndpointSlice

		// If the remaining amounts of endpoints is smaller than the max
		// endpoints per slice and we have slices that haven't already been
		// filled, try to fit them in one.
		if desiredSet.Len() < int(r.maxEndpointsPerSlice) && sliceNamesUnchanged.Len() > 0 {
			unchangedSlices := []*discovery.EndpointSlice{}
			for _, sliceName := range sliceNamesUnchanged.UnsortedList() {
				unchangedSlices = append(unchangedSlices, slicesByName[sliceName])
			}
			sliceToFill = getSliceToFill(unchangedSlices, desiredSet.Len(), int(r.maxEndpointsPerSlice))
		}

		// If we didn't find a sliceToFill, generate a new empty one.
		if sliceToFill == nil {
			sliceToFill = newEndpointSlice(logger, service, endpointMeta, r.controllerName)
		} else {
			// deep copy required to modify this slice.
			sliceToFill = sliceToFill.DeepCopy()
			slicesByName[sliceToFill.Name] = sliceToFill
		}

		// Fill the slice up with remaining endpoints.
		for desiredSet.Len() > 0 && len(sliceToFill.Endpoints) < int(r.maxEndpointsPerSlice) {
			endpoint, _ := desiredSet.PopAny()
			sliceToFill.Endpoints = append(sliceToFill.Endpoints, *endpoint)
		}

		// New slices will not have a Name set, use this to determine whether
		// this should be an update or create.
		if sliceToFill.Name != "" {
			sliceNamesToUpdate.Insert(sliceToFill.Name)
			sliceNamesUnchanged.Delete(sliceToFill.Name)
		} else {
			slicesToCreate = append(slicesToCreate, sliceToFill)
		}
	}

	// Build slicesToUpdate from slice names.
	slicesToUpdate := []*discovery.EndpointSlice{}
	for _, sliceName := range sliceNamesToUpdate.UnsortedList() {
		slicesToUpdate = append(slicesToUpdate, slicesByName[sliceName])
	}

	// Build slicesToDelete from slice names.
	slicesToDelete := []*discovery.EndpointSlice{}
	for _, sliceName := range sliceNamesToDelete.UnsortedList() {
		slicesToDelete = append(slicesToDelete, slicesByName[sliceName])
	}

	return slicesToCreate, slicesToUpdate, slicesToDelete, numAdded, numRemoved
}

func (r *Reconciler) DeleteService(namespace, name string) {
	r.metricsCache.DeleteService(types.NamespacedName{Namespace: namespace, Name: name})
}

func (r *Reconciler) GetControllerName() string {
	return r.controllerName
}

// ManagedByChanged returns true if one of the provided EndpointSlices is
// managed by the EndpointSlice controller while the other is not.
func (r *Reconciler) ManagedByChanged(endpointSlice1, endpointSlice2 *discovery.EndpointSlice) bool {
	return r.ManagedByController(endpointSlice1) != r.ManagedByController(endpointSlice2)
}

// ManagedByController returns true if the controller of the provided
// EndpointSlices is the EndpointSlice controller.
func (r *Reconciler) ManagedByController(endpointSlice *discovery.EndpointSlice) bool {
	managedBy := endpointSlice.Labels[discovery.LabelManagedBy]
	return managedBy == r.controllerName
}
