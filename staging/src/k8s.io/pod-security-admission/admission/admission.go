/*
Copyright 2021 The Kubernetes Authors.

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

package admission

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	"k8s.io/klog/v2"

	admissionv1 "k8s.io/api/admission/v1"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
	admissionapi "k8s.io/pod-security-admission/admission/api"
	"k8s.io/pod-security-admission/admission/api/validation"
	"k8s.io/pod-security-admission/api"
	"k8s.io/pod-security-admission/metrics"
	"k8s.io/pod-security-admission/policy"
)

const (
	defaultNamespaceMaxPodsToCheck  = 3000
	defaultNamespacePodCheckTimeout = 1 * time.Second
)

// Admission implements the core admission logic for the Pod Security Admission controller.
// The admission logic can be
type Admission struct {
	Configuration *admissionapi.PodSecurityConfiguration

	// Getting policy checks per level/version
	Evaluator policy.Evaluator

	// Metrics
	Metrics metrics.Recorder

	// Arbitrary object --> PodSpec
	PodSpecExtractor PodSpecExtractor

	// API connections
	NamespaceGetter NamespaceGetter
	PodLister       PodLister

	defaultPolicy api.Policy

	namespaceMaxPodsToCheck  int
	namespacePodCheckTimeout time.Duration
}

type NamespaceGetter interface {
	GetNamespace(ctx context.Context, name string) (*corev1.Namespace, error)
}

type PodLister interface {
	ListPods(ctx context.Context, namespace string) ([]*corev1.Pod, error)
}

// PodSpecExtractor extracts a PodSpec from pod-controller resources that embed a PodSpec.
// This interface can be extended to enforce policy on CRDs for custom pod-controllers.
type PodSpecExtractor interface {
	// HasPodSpec returns true if the given resource type MAY contain an extractable PodSpec.
	HasPodSpec(schema.GroupResource) bool
	// ExtractPodSpec returns a pod spec and metadata to evaluate from the object.
	// An error returned here does not block admission of the pod-spec-containing object and is not returned to the user.
	// If the object has no pod spec, return `nil, nil, nil`.
	ExtractPodSpec(runtime.Object) (*metav1.ObjectMeta, *corev1.PodSpec, error)
}

var defaultPodSpecResources = map[schema.GroupResource]bool{
	corev1.Resource("pods"):                   true,
	corev1.Resource("replicationcontrollers"): true,
	corev1.Resource("podtemplates"):           true,
	appsv1.Resource("replicasets"):            true,
	appsv1.Resource("deployments"):            true,
	appsv1.Resource("statefulsets"):           true,
	appsv1.Resource("daemonsets"):             true,
	batchv1.Resource("jobs"):                  true,
	batchv1.Resource("cronjobs"):              true,
}

type DefaultPodSpecExtractor struct{}

func (DefaultPodSpecExtractor) HasPodSpec(gr schema.GroupResource) bool {
	return defaultPodSpecResources[gr]
}

func (DefaultPodSpecExtractor) ExtractPodSpec(obj runtime.Object) (*metav1.ObjectMeta, *corev1.PodSpec, error) {
	switch o := obj.(type) {
	case *corev1.Pod:
		return &o.ObjectMeta, &o.Spec, nil
	case *corev1.PodTemplate:
		return extractPodSpecFromTemplate(&o.Template)
	case *corev1.ReplicationController:
		return extractPodSpecFromTemplate(o.Spec.Template)
	case *appsv1.ReplicaSet:
		return extractPodSpecFromTemplate(&o.Spec.Template)
	case *appsv1.Deployment:
		return extractPodSpecFromTemplate(&o.Spec.Template)
	case *appsv1.DaemonSet:
		return extractPodSpecFromTemplate(&o.Spec.Template)
	case *appsv1.StatefulSet:
		return extractPodSpecFromTemplate(&o.Spec.Template)
	case *batchv1.Job:
		return extractPodSpecFromTemplate(&o.Spec.Template)
	case *batchv1.CronJob:
		return extractPodSpecFromTemplate(&o.Spec.JobTemplate.Spec.Template)
	default:
		return nil, nil, fmt.Errorf("unexpected object type: %s", obj.GetObjectKind().GroupVersionKind().String())
	}
}

func (DefaultPodSpecExtractor) PodSpecResources() []schema.GroupResource {
	retval := make([]schema.GroupResource, 0, len(defaultPodSpecResources))
	for r := range defaultPodSpecResources {
		retval = append(retval, r)
	}
	return retval
}

func extractPodSpecFromTemplate(template *corev1.PodTemplateSpec) (*metav1.ObjectMeta, *corev1.PodSpec, error) {
	if template == nil {
		return nil, nil, nil
	}
	return &template.ObjectMeta, &template.Spec, nil
}

// CompleteConfiguration sets up default or derived configuration.
func (a *Admission) CompleteConfiguration() error {
	if a.Configuration != nil {
		if p, err := admissionapi.ToPolicy(a.Configuration.Defaults); err != nil {
			return err
		} else {
			a.defaultPolicy = p
		}
	}
	a.namespaceMaxPodsToCheck = defaultNamespaceMaxPodsToCheck
	a.namespacePodCheckTimeout = defaultNamespacePodCheckTimeout

	if a.PodSpecExtractor == nil {
		a.PodSpecExtractor = &DefaultPodSpecExtractor{}
	}

	return nil
}

// ValidateConfiguration ensures all required fields are set with valid values.
func (a *Admission) ValidateConfiguration() error {
	if a.Configuration == nil {
		return fmt.Errorf("configuration required")
	} else if errs := validation.ValidatePodSecurityConfiguration(a.Configuration); len(errs) > 0 {
		return errs.ToAggregate()
	} else {
		if p, err := admissionapi.ToPolicy(a.Configuration.Defaults); err != nil {
			return err
		} else if !reflect.DeepEqual(p, a.defaultPolicy) {
			return fmt.Errorf("default policy does not match; CompleteConfiguration() was not called before ValidateConfiguration()")
		}
	}
	if a.namespaceMaxPodsToCheck == 0 || a.namespacePodCheckTimeout == 0 {
		return fmt.Errorf("namespace configuration not set; CompleteConfiguration() was not called before ValidateConfiguration()")
	}
	if a.Metrics == nil {
		return fmt.Errorf("Metrics recorder required")
	}
	if a.PodSpecExtractor == nil {
		return fmt.Errorf("PodSpecExtractor required")
	}
	if a.Evaluator == nil {
		return fmt.Errorf("Evaluator required")
	}
	if a.NamespaceGetter == nil {
		return fmt.Errorf("NamespaceGetter required")
	}
	if a.PodLister == nil {
		return fmt.Errorf("PodLister required")
	}
	return nil
}

var (
	namespacesResource = corev1.Resource("namespaces")
	podsResource       = corev1.Resource("pods")
)

// Validate admits an API request.
// The objects in admission attributes are expected to be external v1 objects that we care about.
// The returned response may be shared and must not be mutated.
func (a *Admission) Validate(ctx context.Context, attrs api.Attributes) *admissionv1.AdmissionResponse {
	var response *admissionv1.AdmissionResponse
	switch attrs.GetResource().GroupResource() {
	case namespacesResource:
		response = a.ValidateNamespace(ctx, attrs)
	case podsResource:
		response = a.ValidatePod(ctx, attrs)
	default:
		response = a.ValidatePodController(ctx, attrs)
	}
	return response
}

// ValidateNamespace evaluates a namespace create or update request to ensure the pod security labels are valid,
// and checks existing pods in the namespace for violations of the new policy when updating the enforce level on a namespace.
// The returned response may be shared between evaluations and must not be mutated.
func (a *Admission) ValidateNamespace(ctx context.Context, attrs api.Attributes) *admissionv1.AdmissionResponse {
	// short-circuit on subresources
	if attrs.GetSubresource() != "" {
		return sharedAllowedResponse
	}
	obj, err := attrs.GetObject()
	if err != nil {
		klog.FromContext(ctx).Error(err, "Failed to decode object")
		return errorResponse(err, &apierrors.NewBadRequest("failed to decode object").ErrStatus)
	}
	namespace, ok := obj.(*corev1.Namespace)
	if !ok {
		klog.FromContext(ctx).Info("Failed to assert namespace type", "type", reflect.TypeOf(obj))
		return errorResponse(nil, &apierrors.NewBadRequest("failed to decode namespace").ErrStatus)
	}

	newPolicy, newErrs := a.PolicyToEvaluate(namespace.Labels)

	switch attrs.GetOperation() {
	case admissionv1.Create:
		// require valid labels on create
		if len(newErrs) > 0 {
			return invalidResponse(attrs, newErrs)
		}
		if a.exemptNamespace(attrs.GetNamespace()) {
			if warning := a.exemptNamespaceWarning(namespace.Name, newPolicy, namespace.Labels); warning != "" {
				response := allowedResponse()
				response.Warnings = append(response.Warnings, warning)
				return response
			}
		}
		return sharedAllowedResponse

	case admissionv1.Update:
		// if update, check if policy labels changed
		oldObj, err := attrs.GetOldObject()
		if err != nil {
			klog.FromContext(ctx).Error(err, "Failed to decode old object")
			return errorResponse(err, &apierrors.NewBadRequest("failed to decode  old object").ErrStatus)
		}
		oldNamespace, ok := oldObj.(*corev1.Namespace)
		if !ok {
			klog.FromContext(ctx).Info("Failed to assert old namespace type", "type", reflect.TypeOf(oldObj))
			return errorResponse(nil, &apierrors.NewBadRequest("failed to decode  old namespace").ErrStatus)
		}
		oldPolicy, oldErrs := a.PolicyToEvaluate(oldNamespace.Labels)

		// require valid labels on update if they have changed
		if len(newErrs) > 0 && (len(oldErrs) == 0 || !reflect.DeepEqual(newErrs, oldErrs)) {
			return invalidResponse(attrs, newErrs)
		}

		// Skip dry-running pods:
		// * if the enforce policy is unchanged
		// * if the new enforce policy is privileged
		// * if the new enforce is the same version and level was relaxed
		// * for exempt namespaces
		if newPolicy.Enforce == oldPolicy.Enforce {
			return sharedAllowedResponse
		}
		if newPolicy.Enforce.Level == api.LevelPrivileged {
			return sharedAllowedResponse
		}
		if newPolicy.Enforce.Version == oldPolicy.Enforce.Version &&
			api.CompareLevels(newPolicy.Enforce.Level, oldPolicy.Enforce.Level) < 1 {
			return sharedAllowedResponse
		}
		if a.exemptNamespace(attrs.GetNamespace()) {
			if warning := a.exemptNamespaceWarning(namespace.Name, newPolicy, namespace.Labels); warning != "" {
				response := allowedResponse()
				response.Warnings = append(response.Warnings, warning)
				return response
			}
			return sharedAllowedResponse
		}
		response := allowedResponse()
		response.Warnings = a.EvaluatePodsInNamespace(ctx, namespace.Name, newPolicy.Enforce)
		return response

	default:
		return sharedAllowedResponse
	}
}

// ignoredPodSubresources is a set of ignored Pod subresources.
// Any other subresource is expected to be a *v1.Pod type and is evaluated.
// This ensures a version skewed webhook fails safe and denies an unknown pod subresource that allows modifying the pod spec.
var ignoredPodSubresources = map[string]bool{
	"exec":        true,
	"attach":      true,
	"binding":     true,
	"eviction":    true,
	"log":         true,
	"portforward": true,
	"proxy":       true,
	"status":      true,
}

// ValidatePod evaluates a pod create or update request against the effective policy for the namespace.
// The returned response may be shared between evaluations and must not be mutated.
func (a *Admission) ValidatePod(ctx context.Context, attrs api.Attributes) *admissionv1.AdmissionResponse {
	// short-circuit on ignored subresources
	if ignoredPodSubresources[attrs.GetSubresource()] {
		return sharedAllowedResponse
	}
	// short-circuit on exempt namespaces and users
	if a.exemptNamespace(attrs.GetNamespace()) {
		a.Metrics.RecordExemption(attrs)
		return sharedAllowedByNamespaceExemptionResponse
	}

	if a.exemptUser(attrs.GetUserName()) {
		a.Metrics.RecordExemption(attrs)
		return sharedAllowedByUserExemptionResponse
	}

	// short-circuit on privileged enforce+audit+warn namespaces
	namespace, err := a.NamespaceGetter.GetNamespace(ctx, attrs.GetNamespace())
	if err != nil {
		klog.FromContext(ctx).Error(err, "Failed to fetch pod namespace", "namespace", attrs.GetNamespace())
		a.Metrics.RecordError(true, attrs)
		return errorResponse(err, &apierrors.NewInternalError(fmt.Errorf("failed to lookup namespace %q", attrs.GetNamespace())).ErrStatus)
	}
	nsPolicy, nsPolicyErrs := a.PolicyToEvaluate(namespace.Labels)
	if len(nsPolicyErrs) == 0 && nsPolicy.FullyPrivileged() {
		a.Metrics.RecordEvaluation(metrics.DecisionAllow, nsPolicy.Enforce, metrics.ModeEnforce, attrs)
		return sharedAllowedPrivilegedResponse
	}

	obj, err := attrs.GetObject()
	if err != nil {
		klog.FromContext(ctx).Error(err, "Failed to decode object")
		a.Metrics.RecordError(true, attrs)
		return errorResponse(err, &apierrors.NewBadRequest("failed to decode object").ErrStatus)
	}
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		klog.FromContext(ctx).Info("Failed to assert pod type", "type", reflect.TypeOf(obj))
		a.Metrics.RecordError(true, attrs)
		return errorResponse(nil, &apierrors.NewBadRequest("failed to decode pod").ErrStatus)
	}
	if attrs.GetOperation() == admissionv1.Update {
		oldObj, err := attrs.GetOldObject()
		if err != nil {
			klog.FromContext(ctx).Error(err, "Failed to decode old object")
			a.Metrics.RecordError(true, attrs)
			return errorResponse(err, &apierrors.NewBadRequest("failed to decode old object").ErrStatus)
		}
		oldPod, ok := oldObj.(*corev1.Pod)
		if !ok {
			klog.FromContext(ctx).Info("Failed to assert old pod type", "type", reflect.TypeOf(oldObj))
			a.Metrics.RecordError(true, attrs)
			return errorResponse(nil, &apierrors.NewBadRequest("failed to decode old pod").ErrStatus)
		}
		if !isSignificantPodUpdate(pod, oldPod) {
			// Nothing we care about changed, so always allow the update.
			return sharedAllowedResponse
		}
	}
	return a.EvaluatePod(ctx, nsPolicy, nsPolicyErrs.ToAggregate(), &pod.ObjectMeta, &pod.Spec, attrs, true)
}

// ValidatePodController evaluates a pod controller create or update request against the effective policy for the namespace.
// The returned response may be shared between evaluations and must not be mutated.
func (a *Admission) ValidatePodController(ctx context.Context, attrs api.Attributes) *admissionv1.AdmissionResponse {
	// short-circuit on subresources
	if attrs.GetSubresource() != "" {
		return sharedAllowedResponse
	}
	// short-circuit on exempt namespaces and users
	if a.exemptNamespace(attrs.GetNamespace()) {
		a.Metrics.RecordExemption(attrs)
		return sharedAllowedByNamespaceExemptionResponse
	}

	if a.exemptUser(attrs.GetUserName()) {
		a.Metrics.RecordExemption(attrs)
		return sharedAllowedByUserExemptionResponse
	}

	// short-circuit on privileged audit+warn namespaces
	namespace, err := a.NamespaceGetter.GetNamespace(ctx, attrs.GetNamespace())
	if err != nil {
		klog.FromContext(ctx).Error(err, "Failed to fetch pod namespace", "namespace", attrs.GetNamespace())
		a.Metrics.RecordError(true, attrs)
		response := allowedResponse()
		response.AuditAnnotations = map[string]string{
			"error": fmt.Sprintf("failed to lookup namespace %q: %v", attrs.GetNamespace(), err),
		}
		return response
	}
	nsPolicy, nsPolicyErrs := a.PolicyToEvaluate(namespace.Labels)
	if len(nsPolicyErrs) == 0 && nsPolicy.Warn.Level == api.LevelPrivileged && nsPolicy.Audit.Level == api.LevelPrivileged {
		return sharedAllowedResponse
	}

	obj, err := attrs.GetObject()
	if err != nil {
		klog.FromContext(ctx).Error(err, "Failed to decode object")
		a.Metrics.RecordError(true, attrs)
		response := allowedResponse()
		response.AuditAnnotations = map[string]string{
			"error": fmt.Sprintf("failed to decode object: %v", err),
		}
		return response
	}
	podMetadata, podSpec, err := a.PodSpecExtractor.ExtractPodSpec(obj)
	if err != nil {
		klog.FromContext(ctx).Error(err, "Failed to extract pod spec")
		a.Metrics.RecordError(true, attrs)
		response := allowedResponse()
		response.AuditAnnotations = map[string]string{
			"error": fmt.Sprintf("failed to extract pod template: %v", err),
		}
		return response
	}
	if podMetadata == nil && podSpec == nil {
		// if a controller with an optional pod spec does not contain a pod spec, skip validation
		return sharedAllowedResponse
	}
	return a.EvaluatePod(ctx, nsPolicy, nsPolicyErrs.ToAggregate(), podMetadata, podSpec, attrs, false)
}

// EvaluatePod evaluates the given policy against the given pod(-like) object.
// The enforce policy is only checked if enforce=true.
// The returned response may be shared between evaluations and must not be mutated.
func (a *Admission) EvaluatePod(ctx context.Context, nsPolicy api.Policy, nsPolicyErr error, podMetadata *metav1.ObjectMeta, podSpec *corev1.PodSpec, attrs api.Attributes, enforce bool) *admissionv1.AdmissionResponse {
	logger := klog.FromContext(ctx)
	// short-circuit on exempt runtimeclass
	if a.exemptRuntimeClass(podSpec.RuntimeClassName) {
		a.Metrics.RecordExemption(attrs)
		return sharedAllowedByRuntimeClassExemptionResponse
	}

	auditAnnotations := map[string]string{}
	if nsPolicyErr != nil {
		logger.V(2).Info("failed to parse PodSecurity namespace labels", "err", nsPolicyErr)
		auditAnnotations["error"] = fmt.Sprintf("Failed to parse policy: %v", nsPolicyErr)
		a.Metrics.RecordError(false, attrs)
	}

	if klogV := logger.V(5); klogV.Enabled() {
		klogV.Info("PodSecurity evaluation", "policy", fmt.Sprintf("%v", nsPolicy), "op", attrs.GetOperation(), "resource", attrs.GetResource(), "namespace", attrs.GetNamespace(), "name", attrs.GetName())
	}
	cachedResults := make(map[api.LevelVersion]policy.AggregateCheckResult)
	response := allowedResponse()
	if enforce {
		auditAnnotations[api.EnforcedPolicyAnnotationKey] = nsPolicy.Enforce.String()

		result := policy.AggregateCheckResults(a.Evaluator.EvaluatePod(nsPolicy.Enforce, podMetadata, podSpec))
		if !result.Allowed {
			response = forbiddenResponse(attrs, fmt.Errorf(
				"violates PodSecurity %q: %s",
				nsPolicy.Enforce.String(),
				result.ForbiddenDetail(),
			))
			a.Metrics.RecordEvaluation(metrics.DecisionDeny, nsPolicy.Enforce, metrics.ModeEnforce, attrs)
		} else {
			a.Metrics.RecordEvaluation(metrics.DecisionAllow, nsPolicy.Enforce, metrics.ModeEnforce, attrs)
		}
		cachedResults[nsPolicy.Enforce] = result
	}

	// reuse previous evaluation if audit level+version is the same as enforce level+version

	auditResult, ok := cachedResults[nsPolicy.Audit]
	if !ok {
		auditResult = policy.AggregateCheckResults(a.Evaluator.EvaluatePod(nsPolicy.Audit, podMetadata, podSpec))
		cachedResults[nsPolicy.Audit] = auditResult
	}
	if !auditResult.Allowed {
		auditAnnotations[api.AuditViolationsAnnotationKey] = fmt.Sprintf(
			"would violate PodSecurity %q: %s",
			nsPolicy.Audit.String(),
			auditResult.ForbiddenDetail(),
		)
		a.Metrics.RecordEvaluation(metrics.DecisionDeny, nsPolicy.Audit, metrics.ModeAudit, attrs)
	}

	// avoid adding warnings to a request we're already going to reject with an error
	if response.Allowed {
		// reuse previous evaluation if warn level+version is the same as audit or enforce level+version
		warnResult, ok := cachedResults[nsPolicy.Warn]
		if !ok {
			warnResult = policy.AggregateCheckResults(a.Evaluator.EvaluatePod(nsPolicy.Warn, podMetadata, podSpec))
		}
		if !warnResult.Allowed {
			// TODO: Craft a better user-facing warning message
			response.Warnings = append(response.Warnings, fmt.Sprintf(
				"would violate PodSecurity %q: %s",
				nsPolicy.Warn.String(),
				warnResult.ForbiddenDetail(),
			))
			a.Metrics.RecordEvaluation(metrics.DecisionDeny, nsPolicy.Warn, metrics.ModeWarn, attrs)
		}
	}

	response.AuditAnnotations = auditAnnotations
	return response
}

// podCount is used to track the number of pods sharing identical warnings when validating a namespace
type podCount struct {
	// podName is the lexically first pod name for the given warning
	podName string

	// podCount is the total number of pods with the same warnings
	podCount int
}

func (a *Admission) EvaluatePodsInNamespace(ctx context.Context, namespace string, enforce api.LevelVersion) []string {
	// start with the default timeout
	timeout := a.namespacePodCheckTimeout
	if deadline, ok := ctx.Deadline(); ok {
		timeRemaining := time.Until(deadline) / 2 // don't take more than half the remaining time
		if timeout > timeRemaining {
			timeout = timeRemaining
		}
	}
	deadline := time.Now().Add(timeout)
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	pods, err := a.PodLister.ListPods(ctx, namespace)
	if err != nil {
		klog.FromContext(ctx).Error(err, "Failed to list pods", "namespace", namespace)
		return []string{"failed to list pods while checking new PodSecurity enforce level"}
	}

	var (
		warnings []string

		podWarnings        []string
		podWarningsToCount = make(map[string]podCount)
		prioritizedPods    = a.prioritizePods(pods)
	)

	totalPods := len(prioritizedPods)
	if len(prioritizedPods) > a.namespaceMaxPodsToCheck {
		prioritizedPods = prioritizedPods[0:a.namespaceMaxPodsToCheck]
	}

	checkedPods := len(prioritizedPods)
	for i, pod := range prioritizedPods {
		r := policy.AggregateCheckResults(a.Evaluator.EvaluatePod(enforce, &pod.ObjectMeta, &pod.Spec))
		if !r.Allowed {
			warning := r.ForbiddenReason()
			c, seen := podWarningsToCount[warning]
			if !seen {
				c.podName = pod.Name
				podWarnings = append(podWarnings, warning)
			} else if pod.Name < c.podName {
				c.podName = pod.Name
			}
			c.podCount++
			podWarningsToCount[warning] = c
		}
		if err := ctx.Err(); err != nil { // deadline exceeded or context was cancelled
			checkedPods = i + 1
			break
		}
	}

	if checkedPods < totalPods {
		warnings = append(warnings, fmt.Sprintf("new PodSecurity enforce level only checked against the first %d of %d existing pods", checkedPods, totalPods))
	}

	if len(podWarnings) > 0 {
		warnings = append(warnings, fmt.Sprintf("existing pods in namespace %q violate the new PodSecurity enforce level %q", namespace, enforce.String()))
	}

	// prepend pod names to warnings
	decoratePodWarnings(podWarningsToCount, podWarnings)
	// put warnings in a deterministic order
	sort.Strings(podWarnings)

	return append(warnings, podWarnings...)
}

// prefixes warnings with the pod names related to that warning
func decoratePodWarnings(podWarningsToCount map[string]podCount, warnings []string) {
	for i, warning := range warnings {
		c := podWarningsToCount[warning]
		switch c.podCount {
		case 0:
			// unexpected, just leave the warning alone
		case 1:
			warnings[i] = fmt.Sprintf("%s: %s", c.podName, warning)
		case 2:
			warnings[i] = fmt.Sprintf("%s (and 1 other pod): %s", c.podName, warning)
		default:
			warnings[i] = fmt.Sprintf("%s (and %d other pods): %s", c.podName, c.podCount-1, warning)
		}
	}
}

func (a *Admission) PolicyToEvaluate(labels map[string]string) (api.Policy, field.ErrorList) {
	return api.PolicyToEvaluate(labels, a.defaultPolicy)
}

// isSignificantPodUpdate determines whether a pod update should trigger a policy evaluation.
// Relevant mutable pod fields as of 1.21 are image annotations:
// * https://github.com/kubernetes/kubernetes/blob/release-1.21/pkg/apis/core/validation/validation.go#L3947-L3949
func isSignificantPodUpdate(pod, oldPod *corev1.Pod) bool {
	// TODO: invert this logic to only allow specific update types.
	if len(pod.Spec.Containers) != len(oldPod.Spec.Containers) {
		return true
	}
	if len(pod.Spec.InitContainers) != len(oldPod.Spec.InitContainers) {
		return true
	}
	for i := 0; i < len(pod.Spec.Containers); i++ {
		if isSignificantContainerUpdate(&pod.Spec.Containers[i], &oldPod.Spec.Containers[i]) {
			return true
		}
	}
	for i := 0; i < len(pod.Spec.InitContainers); i++ {
		if isSignificantContainerUpdate(&pod.Spec.InitContainers[i], &oldPod.Spec.InitContainers[i]) {
			return true
		}
	}
	for _, c := range pod.Spec.EphemeralContainers {
		var oldC *corev1.Container
		for i, oc := range oldPod.Spec.EphemeralContainers {
			if oc.Name == c.Name {
				oldC = (*corev1.Container)(&oldPod.Spec.EphemeralContainers[i].EphemeralContainerCommon)
				break
			}
		}
		if oldC == nil {
			return true // EphemeralContainer added
		}
		if isSignificantContainerUpdate((*corev1.Container)(&c.EphemeralContainerCommon), oldC) {
			return true
		}
	}
	return false
}

// isSignificantContainerUpdate determines whether a container update should trigger a policy evaluation.
func isSignificantContainerUpdate(container, oldContainer *corev1.Container) bool {
	return container.Image != oldContainer.Image
}

func (a *Admission) exemptNamespace(namespace string) bool {
	if len(namespace) == 0 {
		return false
	}
	// TODO: consider optimizing to O(1) lookup
	return containsString(namespace, a.Configuration.Exemptions.Namespaces)
}
func (a *Admission) exemptUser(username string) bool {
	if len(username) == 0 {
		return false
	}
	// TODO: consider optimizing to O(1) lookup
	return containsString(username, a.Configuration.Exemptions.Usernames)
}
func (a *Admission) exemptRuntimeClass(runtimeClass *string) bool {
	if runtimeClass == nil || len(*runtimeClass) == 0 {
		return false
	}
	// TODO: consider optimizing to O(1) lookup
	return containsString(*runtimeClass, a.Configuration.Exemptions.RuntimeClasses)
}

// Filter and prioritize pods based on runtimeclass and uniqueness of the controller respectively for evaluation.
// The input slice is modified in place and should not be reused.
func (a *Admission) prioritizePods(pods []*corev1.Pod) []*corev1.Pod {
	// accumulate the list of prioritized pods in-place to avoid double-allocating
	prioritizedPods := pods[:0]
	// accumulate any additional replicated pods after the first one encountered for a given controller uid
	var duplicateReplicatedPods []*corev1.Pod
	evaluatedControllers := make(map[types.UID]bool)
	for _, pod := range pods {
		// short-circuit on exempt runtimeclass
		if a.exemptRuntimeClass(pod.Spec.RuntimeClassName) {
			continue
		}
		// short-circuit if pod from the same controller is evaluated
		podOwnerControllerRef := metav1.GetControllerOfNoCopy(pod)
		if podOwnerControllerRef == nil {
			prioritizedPods = append(prioritizedPods, pod)
			continue
		}
		if evaluatedControllers[podOwnerControllerRef.UID] {
			duplicateReplicatedPods = append(duplicateReplicatedPods, pod)
			continue
		}
		prioritizedPods = append(prioritizedPods, pod)
		evaluatedControllers[podOwnerControllerRef.UID] = true
	}
	return append(prioritizedPods, duplicateReplicatedPods...)
}

func containsString(needle string, haystack []string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// exemptNamespaceWarning returns a non-empty warning message if the exempt namespace has a
// non-privileged policy and sets pod security labels.
func (a *Admission) exemptNamespaceWarning(exemptNamespace string, policy api.Policy, nsLabels map[string]string) string {
	if policy.FullyPrivileged() || policy.Equivalent(&a.defaultPolicy) {
		return ""
	}

	// Build a compact representation of the policy, only printing non-privileged modes that have
	// been explicitly set.
	sb := strings.Builder{}
	_, hasEnforceLevel := nsLabels[api.EnforceLevelLabel]
	_, hasEnforceVersion := nsLabels[api.EnforceVersionLabel]
	if policy.Enforce.Level != api.LevelPrivileged && (hasEnforceLevel || hasEnforceVersion) {
		sb.WriteString("enforce=")
		sb.WriteString(policy.Enforce.String())
	}
	_, hasAuditLevel := nsLabels[api.AuditLevelLabel]
	_, hasAuditVersion := nsLabels[api.AuditVersionLabel]
	if policy.Audit.Level != api.LevelPrivileged && (hasAuditLevel || hasAuditVersion) {
		if sb.Len() > 0 {
			sb.WriteString(", ")
		}
		sb.WriteString("audit=")
		sb.WriteString(policy.Audit.String())
	}
	_, hasWarnLevel := nsLabels[api.WarnLevelLabel]
	_, hasWarnVersion := nsLabels[api.WarnVersionLabel]
	if policy.Warn.Level != api.LevelPrivileged && (hasWarnLevel || hasWarnVersion) {
		if sb.Len() > 0 {
			sb.WriteString(", ")
		}
		sb.WriteString("warn=")
		sb.WriteString(policy.Warn.String())
	}

	return fmt.Sprintf("namespace %q is exempt from Pod Security, and the policy (%s) will be ignored",
		exemptNamespace, sb.String())
}
