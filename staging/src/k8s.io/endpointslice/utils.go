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
	"fmt"
	"time"

	v1 "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apimachineryvalidation "k8s.io/apimachinery/pkg/api/validation"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/sets"
	endpointutil "k8s.io/endpointslice/util"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"
)

// podToEndpoint returns an Endpoint object generated from a Pod, a Node, and a Service for a particular addressType.
func podToEndpoint(pod *v1.Pod, node *v1.Node, service *v1.Service, addressType discovery.AddressType) discovery.Endpoint {
	serving := endpointutil.IsPodReady(pod)
	terminating := pod.DeletionTimestamp != nil
	// For compatibility reasons, "ready" should never be "true" if a pod is terminatng, unless
	// publishNotReadyAddresses was set.
	ready := service.Spec.PublishNotReadyAddresses || (serving && !terminating)
	ep := discovery.Endpoint{
		Addresses: getEndpointAddresses(pod.Status, service, addressType),
		Conditions: discovery.EndpointConditions{
			Ready:       &ready,
			Serving:     &serving,
			Terminating: &terminating,
		},
		TargetRef: &v1.ObjectReference{
			Kind:      "Pod",
			Namespace: pod.ObjectMeta.Namespace,
			Name:      pod.ObjectMeta.Name,
			UID:       pod.ObjectMeta.UID,
		},
	}

	if pod.Spec.NodeName != "" {
		ep.NodeName = &pod.Spec.NodeName
	}

	if node != nil && node.Labels[v1.LabelTopologyZone] != "" {
		zone := node.Labels[v1.LabelTopologyZone]
		ep.Zone = &zone
	}

	if endpointutil.ShouldSetHostname(pod, service) {
		ep.Hostname = &pod.Spec.Hostname
	}

	return ep
}

// getEndpointPorts returns a list of EndpointPorts generated from a Service
// and Pod.
func getEndpointPorts(logger klog.Logger, service *v1.Service, pod *v1.Pod) []discovery.EndpointPort {
	endpointPorts := []discovery.EndpointPort{}

	// Allow headless service not to have ports.
	if len(service.Spec.Ports) == 0 && service.Spec.ClusterIP == v1.ClusterIPNone {
		return endpointPorts
	}

	for i := range service.Spec.Ports {
		servicePort := &service.Spec.Ports[i]

		portName := servicePort.Name
		portProto := servicePort.Protocol
		portNum, err := FindPort(pod, servicePort)
		if err != nil {
			logger.V(4).Info("Failed to find port for service", "service", klog.KObj(service), "err", err)
			continue
		}

		i32PortNum := int32(portNum)
		endpointPorts = append(endpointPorts, discovery.EndpointPort{
			Name:        &portName,
			Port:        &i32PortNum,
			Protocol:    &portProto,
			AppProtocol: servicePort.AppProtocol,
		})
	}

	return endpointPorts
}

// getEndpointAddresses returns a list of addresses generated from a pod status.
func getEndpointAddresses(podStatus v1.PodStatus, service *v1.Service, addressType discovery.AddressType) []string {
	addresses := []string{}

	for _, podIP := range podStatus.PodIPs {
		isIPv6PodIP := utilnet.IsIPv6String(podIP.IP)
		if isIPv6PodIP && addressType == discovery.AddressTypeIPv6 {
			addresses = append(addresses, podIP.IP)
		}

		if !isIPv6PodIP && addressType == discovery.AddressTypeIPv4 {
			addresses = append(addresses, podIP.IP)
		}
	}

	return addresses
}

// newEndpointSlice returns an EndpointSlice generated from a service and
// endpointMeta.
func newEndpointSlice(logger klog.Logger, service *v1.Service, endpointMeta *endpointMeta, controllerName string) *discovery.EndpointSlice {
	gvk := schema.GroupVersionKind{Version: "v1", Kind: "Service"}
	ownerRef := metav1.NewControllerRef(service, gvk)
	epSlice := &discovery.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Labels:          map[string]string{},
			GenerateName:    getEndpointSlicePrefix(service.Name),
			OwnerReferences: []metav1.OwnerReference{*ownerRef},
			Namespace:       service.Namespace,
		},
		Ports:       endpointMeta.ports,
		AddressType: endpointMeta.addressType,
		Endpoints:   []discovery.Endpoint{},
	}
	// add parent service labels
	epSlice.Labels, _ = setEndpointSliceLabels(logger, epSlice, service, controllerName)

	return epSlice
}

// getEndpointSlicePrefix returns a suitable prefix for an EndpointSlice name.
func getEndpointSlicePrefix(serviceName string) string {
	// use the dash (if the name isn't too long) to make the pod name a bit prettier
	prefix := fmt.Sprintf("%s-", serviceName)
	if len(apimachineryvalidation.NameIsDNSSubdomain(prefix, true)) != 0 {
		prefix = serviceName
	}
	return prefix
}

// ownedBy returns true if the provided EndpointSlice is owned by the provided
// Service.
func ownedBy(endpointSlice *discovery.EndpointSlice, svc *v1.Service) bool {
	for _, o := range endpointSlice.OwnerReferences {
		if o.UID == svc.UID && o.Kind == "Service" && o.APIVersion == "v1" {
			return true
		}
	}
	return false
}

// getSliceToFill will return the EndpointSlice that will be closest to full
// when numEndpoints are added. If no EndpointSlice can be found, a nil pointer
// will be returned.
func getSliceToFill(endpointSlices []*discovery.EndpointSlice, numEndpoints, maxEndpoints int) (slice *discovery.EndpointSlice) {
	closestDiff := maxEndpoints
	var closestSlice *discovery.EndpointSlice
	for _, endpointSlice := range endpointSlices {
		currentDiff := maxEndpoints - (numEndpoints + len(endpointSlice.Endpoints))
		if currentDiff >= 0 && currentDiff < closestDiff {
			closestDiff = currentDiff
			closestSlice = endpointSlice
			if closestDiff == 0 {
				return closestSlice
			}
		}
	}
	return closestSlice
}

// addTriggerTimeAnnotation adds a triggerTime annotation to an EndpointSlice
func addTriggerTimeAnnotation(endpointSlice *discovery.EndpointSlice, triggerTime time.Time) {
	if endpointSlice.Annotations == nil {
		endpointSlice.Annotations = make(map[string]string)
	}

	if !triggerTime.IsZero() {
		endpointSlice.Annotations[v1.EndpointsLastChangeTriggerTime] = triggerTime.UTC().Format(time.RFC3339Nano)
	} else { // No new trigger time, clear the annotation.
		delete(endpointSlice.Annotations, v1.EndpointsLastChangeTriggerTime)
	}
}

// ServiceControllerKey returns a controller key for a Service but derived from
// an EndpointSlice.
func ServiceControllerKey(endpointSlice *discovery.EndpointSlice) (string, error) {
	if endpointSlice == nil {
		return "", fmt.Errorf("nil EndpointSlice passed to ServiceControllerKey()")
	}
	serviceName, ok := endpointSlice.Labels[discovery.LabelServiceName]
	if !ok || serviceName == "" {
		return "", fmt.Errorf("EndpointSlice missing %s label", discovery.LabelServiceName)
	}
	return fmt.Sprintf("%s/%s", endpointSlice.Namespace, serviceName), nil
}

// setEndpointSliceLabels returns a map with the new endpoint slices labels and true if there was an update.
// Slices labels must be equivalent to the Service labels except for the reserved IsHeadlessService, LabelServiceName and LabelManagedBy labels
// Changes to IsHeadlessService, LabelServiceName and LabelManagedBy labels on the Service do not result in updates to EndpointSlice labels.
func setEndpointSliceLabels(logger klog.Logger, epSlice *discovery.EndpointSlice, service *v1.Service, controllerName string) (map[string]string, bool) {
	updated := false
	epLabels := make(map[string]string)
	svcLabels := make(map[string]string)

	// check if the endpoint slice and the service have the same labels
	// clone current slice labels except the reserved labels
	for key, value := range epSlice.Labels {
		if isReservedLabelKey(key) {
			continue
		}
		// copy endpoint slice labels
		epLabels[key] = value
	}

	for key, value := range service.Labels {
		if isReservedLabelKey(key) {
			logger.Info("Service using reserved endpoint slices label", "service", klog.KObj(service), "skipping", key, "label", value)
			continue
		}
		// copy service labels
		svcLabels[key] = value
	}

	// if the labels are not identical update the slice with the corresponding service labels
	if !apiequality.Semantic.DeepEqual(epLabels, svcLabels) {
		updated = true
	}

	// add or remove headless label depending on the service Type
	if !isServiceIPSet(service) {
		svcLabels[v1.IsHeadlessService] = ""
	} else {
		delete(svcLabels, v1.IsHeadlessService)
	}

	// override endpoint slices reserved labels
	svcLabels[discovery.LabelServiceName] = service.Name
	svcLabels[discovery.LabelManagedBy] = controllerName

	return svcLabels, updated
}

// isReservedLabelKey return true if the label is one of the reserved label for slices
func isReservedLabelKey(label string) bool {
	if label == discovery.LabelServiceName ||
		label == discovery.LabelManagedBy ||
		label == v1.IsHeadlessService {
		return true
	}
	return false
}

// endpointSliceEndpointLen helps sort endpoint slices by the number of
// endpoints they contain.
type endpointSliceEndpointLen []*discovery.EndpointSlice

func (sl endpointSliceEndpointLen) Len() int      { return len(sl) }
func (sl endpointSliceEndpointLen) Swap(i, j int) { sl[i], sl[j] = sl[j], sl[i] }
func (sl endpointSliceEndpointLen) Less(i, j int) bool {
	return len(sl[i].Endpoints) > len(sl[j].Endpoints)
}

// returns a map of address types used by a service
func getAddressTypesForService(logger klog.Logger, service *v1.Service) sets.Set[discovery.AddressType] {
	serviceSupportedAddresses := sets.New[discovery.AddressType]()
	// TODO: (khenidak) when address types are removed in favor of
	// v1.IPFamily this will need to be removed, and work directly with
	// v1.IPFamily types

	// IMPORTANT: we assume that IP of (discovery.AddressType enum) is never in use
	// as it gets deprecated
	for _, family := range service.Spec.IPFamilies {
		if family == v1.IPv4Protocol {
			serviceSupportedAddresses.Insert(discovery.AddressTypeIPv4)
		}

		if family == v1.IPv6Protocol {
			serviceSupportedAddresses.Insert(discovery.AddressTypeIPv6)
		}
	}

	if serviceSupportedAddresses.Len() > 0 {
		return serviceSupportedAddresses // we have found families for this service
	}

	// TODO (khenidak) remove when (1) dual stack becomes
	// enabled by default (2) v1.19 falls off supported versions

	// Why do we need this:
	// a cluster being upgraded to the new apis
	// will have service.spec.IPFamilies: nil
	// if the controller manager connected to old api
	// server. This will have the nasty side effect of
	// removing all slices already created for this service.
	// this will disable all routing to service vip (ClusterIP)
	// this ensures that this does not happen. Same for headless services
	// we assume it is dual stack, until they get defaulted by *new* api-server
	// this ensures that traffic is not disrupted  until then. But *may*
	// include undesired families for headless services until then.

	if len(service.Spec.ClusterIP) > 0 && service.Spec.ClusterIP != v1.ClusterIPNone { // headfull
		addrType := discovery.AddressTypeIPv4
		if utilnet.IsIPv6String(service.Spec.ClusterIP) {
			addrType = discovery.AddressTypeIPv6
		}
		serviceSupportedAddresses.Insert(addrType)
		logger.V(2).Info("Couldn't find ipfamilies for service. This could happen if controller manager is connected to an old apiserver that does not support ip families yet. EndpointSlices for this Service will use addressType as the IP Family based on familyOf(ClusterIP).", "service", klog.KObj(service), "addressType", addrType, "clusterIP", service.Spec.ClusterIP)
		return serviceSupportedAddresses
	}

	// headless
	// for now we assume two families. This should have minimal side effect
	// if the service is headless with no selector, then this will remain the case
	// if the service is headless with selector then chances are pods are still using single family
	// since kubelet will need to restart in order to start patching pod status with multiple ips
	serviceSupportedAddresses.Insert(discovery.AddressTypeIPv4)
	serviceSupportedAddresses.Insert(discovery.AddressTypeIPv6)
	logger.V(2).Info("Couldn't find ipfamilies for headless service, likely because controller manager is likely connected to an old apiserver that does not support ip families yet. The service endpoint slice will use dual stack families until api-server default it correctly", "service", klog.KObj(service))
	return serviceSupportedAddresses
}

func unchangedSlices(existingSlices, slicesToUpdate, slicesToDelete []*discovery.EndpointSlice) []*discovery.EndpointSlice {
	changedSliceNames := sets.New[string]()
	for _, slice := range slicesToUpdate {
		changedSliceNames.Insert(slice.Name)
	}
	for _, slice := range slicesToDelete {
		changedSliceNames.Insert(slice.Name)
	}
	unchangedSlices := []*discovery.EndpointSlice{}
	for _, slice := range existingSlices {
		if !changedSliceNames.Has(slice.Name) {
			unchangedSlices = append(unchangedSlices, slice)
		}
	}

	return unchangedSlices
}

// hintsEnabled returns true if the provided annotations include either
// v1.AnnotationTopologyMode or v1.DeprecatedAnnotationTopologyAwareHints key
// with a value set to "Auto" or "auto". When both are set,
// v1.DeprecatedAnnotationTopologyAwareHints has precedence.
func hintsEnabled(annotations map[string]string) bool {
	val, ok := annotations[v1.DeprecatedAnnotationTopologyAwareHints]
	if !ok {
		val, ok = annotations[v1.AnnotationTopologyMode]
		if !ok {
			return false
		}
	}
	return val == "Auto" || val == "auto"
}

// isServiceIPSet aims to check if the service's ClusterIP is set or not
// the objective is not to perform validation here
// copied from k8s.io/kubernetes/pkg/apis/core/v1/helper
func isServiceIPSet(service *v1.Service) bool {
	return service.Spec.ClusterIP != v1.ClusterIPNone && service.Spec.ClusterIP != ""
}

// FindPort locates the container port for the given pod and portName.  If the
// targetPort is a number, use that.  If the targetPort is a string, look that
// string up in all named ports in all containers in the target pod.  If no
// match is found, fail.
func FindPort(pod *v1.Pod, svcPort *v1.ServicePort) (int, error) {
	portName := svcPort.TargetPort
	switch portName.Type {
	case intstr.String:
		name := portName.StrVal
		for _, container := range pod.Spec.Containers {
			for _, port := range container.Ports {
				if port.Name == name && port.Protocol == svcPort.Protocol {
					return int(port.ContainerPort), nil
				}
			}
		}
		// also support sidecar container (initContainer with restartPolicy=Always)
		for _, container := range pod.Spec.InitContainers {
			if container.RestartPolicy == nil || *container.RestartPolicy != v1.ContainerRestartPolicyAlways {
				continue
			}
			for _, port := range container.Ports {
				if port.Name == name && port.Protocol == svcPort.Protocol {
					return int(port.ContainerPort), nil
				}
			}
		}
	case intstr.Int:
		return portName.IntValue(), nil
	}

	return 0, fmt.Errorf("no suitable port for manifest: %s", pod.UID)
}
