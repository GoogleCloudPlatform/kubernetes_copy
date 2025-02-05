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

// This file should be consistent with pkg/apis/core/annotation_key_constants.go.

package v1

const (
	// ImagePolicyFailedOpenKey is added to pods created by failing open when the image policy
	// webhook backend fails.
	ImagePolicyFailedOpenKey string = "alpha.image-policy.k8s.io/failed-open"

	// MirrorPodAnnotationKey represents the annotation key set by kubelets when creating mirror pods
	MirrorPodAnnotationKey string = "kubernetes.io/config.mirror"

	// TolerationsAnnotationKey represents the key of tolerations data (json serialized)
	// in the Annotations of a Pod.
	TolerationsAnnotationKey string = "scheduler.alpha.kubernetes.io/tolerations"

	// TaintsAnnotationKey represents the key of taints data (json serialized)
	// in the Annotations of a Node.
	TaintsAnnotationKey string = "scheduler.alpha.kubernetes.io/taints"

	// SeccompPodAnnotationKey represents the key of a seccomp profile applied
	// to all containers of a pod.
	// Deprecated: set a pod security context `seccompProfile` field.
	SeccompPodAnnotationKey string = "seccomp.security.alpha.kubernetes.io/pod"

	// SeccompContainerAnnotationKeyPrefix represents the key of a seccomp profile applied
	// to one container of a pod.
	// Deprecated: set a container security context `seccompProfile` field.
	SeccompContainerAnnotationKeyPrefix string = "container.seccomp.security.alpha.kubernetes.io/"

	// SeccompProfileRuntimeDefault represents the default seccomp profile used by container runtime.
	// Deprecated: set a pod or container security context `seccompProfile` of type "RuntimeDefault" instead.
	SeccompProfileRuntimeDefault string = "runtime/default"

	// SeccompProfileNameUnconfined is the unconfined seccomp profile.
	SeccompProfileNameUnconfined string = "unconfined"

	// SeccompLocalhostProfileNamePrefix is the prefix for specifying profiles loaded from the node's disk.
	SeccompLocalhostProfileNamePrefix = "localhost/"

	// DeprecatedAppArmorBetaContainerAnnotationKeyPrefix is the prefix to an annotation key specifying a container's apparmor profile.
	// Deprecated: use a pod or container security context `appArmorProfile` field instead.
	DeprecatedAppArmorBetaContainerAnnotationKeyPrefix = "container.apparmor.security.beta.kubernetes.io/"

	// DeprecatedAppArmorBetaProfileRuntimeDefault is the profile specifying the runtime default.
	DeprecatedAppArmorBetaProfileRuntimeDefault = "runtime/default"

	// DeprecatedAppArmorBetaProfileNamePrefix is the prefix for specifying profiles loaded on the node.
	DeprecatedAppArmorBetaProfileNamePrefix = "localhost/"

	// DeprecatedAppArmorBetaProfileNameUnconfined is the Unconfined AppArmor profile
	DeprecatedAppArmorBetaProfileNameUnconfined = "unconfined"

	// DeprecatedSeccompProfileDockerDefault represents the default seccomp profile used by docker.
	// Deprecated: set a pod or container security context `seccompProfile` of type "RuntimeDefault" instead.
	DeprecatedSeccompProfileDockerDefault string = "docker/default"

	// PreferAvoidPodsAnnotationKey represents the key of preferAvoidPods data (json serialized)
	// in the Annotations of a Node.
	PreferAvoidPodsAnnotationKey string = "scheduler.alpha.kubernetes.io/preferAvoidPods"

	// ObjectTTLAnnotationKey represents a suggestion for kubelet for how long it can cache
	// an object (e.g. secret, config map) before fetching it again from apiserver.
	// This annotation can be attached to node.
	ObjectTTLAnnotationKey string = "node.alpha.kubernetes.io/ttl"

	// NonConvertibleAnnotationPrefix is the annotation key prefix used to identify non-convertible json paths.
	NonConvertibleAnnotationPrefix = "non-convertible.kubernetes.io"

	kubectlPrefix = "kubectl.kubernetes.io/"

	// LastAppliedConfigAnnotation is the annotation used to store the previous
	// configuration of a resource for use in a three way diff by UpdateApplyAnnotation.
	LastAppliedConfigAnnotation = kubectlPrefix + "last-applied-configuration"

	// AnnotationLoadBalancerSourceRangesKey is the key of the annotation on a service to set allowed ingress ranges on their LoadBalancers
	//
	// It should be a comma-separated list of CIDRs, e.g. `0.0.0.0/0` to
	// allow full access (the default) or `18.0.0.0/8,56.0.0.0/8` to allow
	// access only from the CIDRs currently allocated to MIT & the USPS.
	//
	// Not all cloud providers support this annotation, though AWS & GCE do.
	AnnotationLoadBalancerSourceRangesKey = "service.beta.kubernetes.io/load-balancer-source-ranges"

	// EndpointsLastChangeTriggerTime is the annotation key, set for endpoints objects, that
	// represents the timestamp (stored as RFC 3339 date-time string, e.g. '2018-10-22T19:32:52.1Z')
	// of the last change, of some Pod or Service object, that triggered the endpoints object change.
	// In other words, if a Pod / Service changed at time T0, that change was observed by endpoints
	// controller at T1, and the Endpoints object was changed at T2, the
	// EndpointsLastChangeTriggerTime would be set to T0.
	//
	// The "endpoints change trigger" here means any Pod or Service change that resulted in the
	// Endpoints object change.
	//
	// Given the definition of the "endpoints change trigger", please note that this annotation will
	// be set ONLY for endpoints object changes triggered by either Pod or Service change. If the
	// Endpoints object changes due to other reasons, this annotation won't be set (or updated if it's
	// already set).
	//
	// This annotation will be used to compute the in-cluster network programming latency SLI, see
	// https://github.com/kubernetes/community/blob/master/sig-scalability/slos/network_programming_latency.md
	EndpointsLastChangeTriggerTime = "endpoints.kubernetes.io/last-change-trigger-time"

	// EndpointsOverCapacity will be set on an Endpoints resource when it
	// exceeds the maximum capacity of 1000 addresses. Initially the Endpoints
	// controller will set this annotation with a value of "warning". In a
	// future release, the controller may set this annotation with a value of
	// "truncated" to indicate that any addresses exceeding the limit of 1000
	// have been truncated from the Endpoints resource.
	EndpointsOverCapacity = "endpoints.kubernetes.io/over-capacity"

	// MigratedPluginsAnnotationKey is the annotation key, set for CSINode objects, that is a comma-separated
	// list of in-tree plugins that will be serviced by the CSI backend on the Node represented by CSINode.
	// This annotation is used by the Attach Detach Controller to determine whether to use the in-tree or
	// CSI Backend for a volume plugin on a specific node.
	MigratedPluginsAnnotationKey = "storage.alpha.kubernetes.io/migrated-plugins"

	// PodDeletionCost can be used to set to an int32 that represent the cost of deleting
	// a pod compared to other pods belonging to the same ReplicaSet. Pods with lower
	// deletion cost are preferred to be deleted before pods with higher deletion cost.
	// Note that this is honored on a best-effort basis, and so it does not offer guarantees on
	// pod deletion order.
	// The implicit deletion cost for pods that don't set the annotation is 0, negative values are permitted.
	//
	// This annotation is beta-level and is only honored when PodDeletionCost feature is enabled.
	PodDeletionCost = "controller.kubernetes.io/pod-deletion-cost"

	// DeprecatedAnnotationTopologyAwareHints can be used to enable or disable
	// Topology Aware Hints for a Service. This may be set to "Auto" or
	// "Disabled". Any other value is treated as "Disabled". This annotation has
	// been deprecated in favor of the "service.kubernetes.io/topology-mode"
	// annotation.
	DeprecatedAnnotationTopologyAwareHints = "service.kubernetes.io/topology-aware-hints"

	// AnnotationTopologyMode can be used to enable or disable Topology Aware
	// Routing for a Service. Well known values are "Auto" and "Disabled".
	// Implementations may choose to develop new topology approaches, exposing
	// them with domain-prefixed values. For example, "example.com/lowest-rtt"
	// could be a valid implementation-specific value for this annotation. These
	// heuristics will often populate topology hints on EndpointSlices, but that
	// is not a requirement.
	AnnotationTopologyMode = "service.kubernetes.io/topology-mode"

	nodeAutoscalingPrefix = "node-autoscaling.kubernetes.io"

	// NodeAutoscalingSafeToEvictKey can be used on a Pod to control Node autoscaler drain behavior:
	// - If the value is "false": This pod is not safe to evict, and Node autoscalers shouldn't consolidate a Node where
	//   such a pod is present.
	// - If the value is "true": This pod is safe to evict, and Node autoscalers should not block consolidation of a
	//   Node because of it, when they normally would. For example, if a Node autoscaler normally blocks consolidating
	//   Nodes on which kube-system Pods are running, this can be used to opt some pods out of the blocking.
	NodeAutoscalingSafeToEvictKey = nodeAutoscalingPrefix + "/safe-to-evict"
	// NodeAutoscalingSafeToEvict is the value indicating that the Pod is safe to evict.
	NodeAutoscalingSafeToEvict = "true"
	// NodeAutoscalingNotSafeToEvict is the value indicating that the Pod is not safe to evict.
	NodeAutoscalingNotSafeToEvict = "false"
)
