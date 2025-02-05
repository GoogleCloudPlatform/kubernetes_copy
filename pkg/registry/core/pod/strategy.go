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

package pod

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	netutils "k8s.io/utils/net"
	"sigs.k8s.io/structured-merge-diff/v4/fieldpath"

	apiv1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilnet "k8s.io/apimachinery/pkg/util/net"
	utilvalidation "k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/apiserver/pkg/registry/generic"
	"k8s.io/apiserver/pkg/storage"
	"k8s.io/apiserver/pkg/storage/names"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/apiserver/pkg/warning"
	"k8s.io/client-go/tools/cache"
	"k8s.io/kubernetes/pkg/api/legacyscheme"
	podutil "k8s.io/kubernetes/pkg/api/pod"
	api "k8s.io/kubernetes/pkg/apis/core"
	"k8s.io/kubernetes/pkg/apis/core/helper/qos"
	corevalidation "k8s.io/kubernetes/pkg/apis/core/validation"
	"k8s.io/kubernetes/pkg/features"
	"k8s.io/kubernetes/pkg/kubelet/client"
)

// podStrategy implements behavior for Pods
type podStrategy struct {
	runtime.ObjectTyper
	names.NameGenerator
}

// Strategy is the default logic that applies when creating and updating Pod
// objects via the REST API.
var Strategy = podStrategy{legacyscheme.Scheme, names.SimpleNameGenerator}

// NamespaceScoped is true for pods.
func (podStrategy) NamespaceScoped() bool {
	return true
}

// GetResetFields returns the set of fields that get reset by the strategy
// and should not be modified by the user.
func (podStrategy) GetResetFields() map[fieldpath.APIVersion]*fieldpath.Set {
	fields := map[fieldpath.APIVersion]*fieldpath.Set{
		"v1": fieldpath.NewSet(
			fieldpath.MakePathOrDie("status"),
		),
	}

	return fields
}

// PrepareForCreate clears fields that are not allowed to be set by end users on creation.
func (podStrategy) PrepareForCreate(ctx context.Context, obj runtime.Object) {
	pod := obj.(*api.Pod)
	pod.Status = api.PodStatus{
		Phase:    api.PodPending,
		QOSClass: qos.GetPodQOS(pod),
	}

	podutil.DropDisabledPodFields(pod, nil)

	applySchedulingGatedCondition(pod)
	mutatePodAffinity(pod)
	applyAppArmorVersionSkew(ctx, pod)
}

// PrepareForUpdate clears fields that are not allowed to be set by end users on update.
func (podStrategy) PrepareForUpdate(ctx context.Context, obj, old runtime.Object) {
	newPod := obj.(*api.Pod)
	oldPod := old.(*api.Pod)
	newPod.Status = oldPod.Status
	podutil.DropDisabledPodFields(newPod, oldPod)
}

// Validate validates a new pod.
func (podStrategy) Validate(ctx context.Context, obj runtime.Object) field.ErrorList {
	pod := obj.(*api.Pod)
	opts := podutil.GetValidationOptionsFromPodSpecAndMeta(&pod.Spec, nil, &pod.ObjectMeta, nil)
	opts.ResourceIsPod = true
	return corevalidation.ValidatePodCreate(pod, opts)
}

// WarningsOnCreate returns warnings for the creation of the given object.
func (podStrategy) WarningsOnCreate(ctx context.Context, obj runtime.Object) []string {
	newPod := obj.(*api.Pod)
	var warnings []string
	if msgs := utilvalidation.IsDNS1123Label(newPod.Name); len(msgs) != 0 {
		warnings = append(warnings, fmt.Sprintf("metadata.name: this is used in the Pod's hostname, which can result in surprising behavior; a DNS label is recommended: %v", msgs))
	}
	warnings = append(warnings, podutil.GetWarningsForPod(ctx, newPod, nil)...)
	return warnings
}

// Canonicalize normalizes the object after validation.
func (podStrategy) Canonicalize(obj runtime.Object) {
}

// AllowCreateOnUpdate is false for pods.
func (podStrategy) AllowCreateOnUpdate() bool {
	return false
}

// ValidateUpdate is the default update validation for an end user.
func (podStrategy) ValidateUpdate(ctx context.Context, obj, old runtime.Object) field.ErrorList {
	// Allow downward api usage of hugepages on pod update if feature is enabled or if the old pod already had used them.
	pod := obj.(*api.Pod)
	oldPod := old.(*api.Pod)
	opts := podutil.GetValidationOptionsFromPodSpecAndMeta(&pod.Spec, &oldPod.Spec, &pod.ObjectMeta, &oldPod.ObjectMeta)
	opts.ResourceIsPod = true
	return corevalidation.ValidatePodUpdate(obj.(*api.Pod), old.(*api.Pod), opts)
}

// WarningsOnUpdate returns warnings for the given update.
func (podStrategy) WarningsOnUpdate(ctx context.Context, obj, old runtime.Object) []string {
	// skip warnings on pod update, since humans don't typically interact directly with pods,
	// and we don't want to pay the evaluation cost on what might be a high-frequency update path
	return nil
}

// AllowUnconditionalUpdate allows pods to be overwritten
func (podStrategy) AllowUnconditionalUpdate() bool {
	return true
}

// CheckGracefulDelete allows a pod to be gracefully deleted. It updates the DeleteOptions to
// reflect the desired grace value.
func (podStrategy) CheckGracefulDelete(ctx context.Context, obj runtime.Object, options *metav1.DeleteOptions) bool {
	if options == nil {
		return false
	}
	pod := obj.(*api.Pod)
	period := int64(0)
	// user has specified a value
	if options.GracePeriodSeconds != nil {
		period = *options.GracePeriodSeconds
	} else {
		// use the default value if set, or deletes the pod immediately (0)
		if pod.Spec.TerminationGracePeriodSeconds != nil {
			period = *pod.Spec.TerminationGracePeriodSeconds
		}
	}
	// if the pod is not scheduled, delete immediately
	if len(pod.Spec.NodeName) == 0 {
		period = 0
	}
	// if the pod is already terminated, delete immediately
	if pod.Status.Phase == api.PodFailed || pod.Status.Phase == api.PodSucceeded {
		period = 0
	}

	if period < 0 {
		period = 1
	}

	// ensure the options and the pod are in sync
	options.GracePeriodSeconds = &period
	return true
}

type podStatusStrategy struct {
	podStrategy
}

// StatusStrategy wraps and exports the used podStrategy for the storage package.
var StatusStrategy = podStatusStrategy{Strategy}

// GetResetFields returns the set of fields that get reset by the strategy
// and should not be modified by the user.
func (podStatusStrategy) GetResetFields() map[fieldpath.APIVersion]*fieldpath.Set {
	return map[fieldpath.APIVersion]*fieldpath.Set{
		"v1": fieldpath.NewSet(
			fieldpath.MakePathOrDie("spec"),
			fieldpath.MakePathOrDie("metadata", "deletionTimestamp"),
			fieldpath.MakePathOrDie("metadata", "ownerReferences"),
		),
	}
}

func (podStatusStrategy) PrepareForUpdate(ctx context.Context, obj, old runtime.Object) {
	newPod := obj.(*api.Pod)
	oldPod := old.(*api.Pod)
	newPod.Spec = oldPod.Spec
	newPod.DeletionTimestamp = nil

	// don't allow the pods/status endpoint to touch owner references since old kubelets corrupt them in a way
	// that breaks garbage collection
	newPod.OwnerReferences = oldPod.OwnerReferences
	// the Pod QoS is immutable and populated at creation time by the kube-apiserver.
	// we need to backfill it for backward compatibility because the old kubelet dropped this field when the pod was rejected.
	if newPod.Status.QOSClass == "" {
		newPod.Status.QOSClass = oldPod.Status.QOSClass
	}
}

func (podStatusStrategy) ValidateUpdate(ctx context.Context, obj, old runtime.Object) field.ErrorList {
	pod := obj.(*api.Pod)
	oldPod := old.(*api.Pod)
	opts := podutil.GetValidationOptionsFromPodSpecAndMeta(&pod.Spec, &oldPod.Spec, &pod.ObjectMeta, &oldPod.ObjectMeta)
	opts.ResourceIsPod = true

	return corevalidation.ValidatePodStatusUpdate(obj.(*api.Pod), old.(*api.Pod), opts)
}

// WarningsOnUpdate returns warnings for the given update.
func (podStatusStrategy) WarningsOnUpdate(ctx context.Context, obj, old runtime.Object) []string {
	return nil
}

type podEphemeralContainersStrategy struct {
	podStrategy
}

// EphemeralContainersStrategy wraps and exports the used podStrategy for the storage package.
var EphemeralContainersStrategy = podEphemeralContainersStrategy{Strategy}

// dropNonEphemeralContainerUpdates discards all changes except for pod.Spec.EphemeralContainers and certain metadata
func dropNonEphemeralContainerUpdates(newPod, oldPod *api.Pod) *api.Pod {
	pod := dropPodUpdates(newPod, oldPod)
	pod.Spec.EphemeralContainers = newPod.Spec.EphemeralContainers
	return pod
}

func (podEphemeralContainersStrategy) PrepareForUpdate(ctx context.Context, obj, old runtime.Object) {
	newPod := obj.(*api.Pod)
	oldPod := old.(*api.Pod)

	*newPod = *dropNonEphemeralContainerUpdates(newPod, oldPod)
	podutil.DropDisabledPodFields(newPod, oldPod)
}

func (podEphemeralContainersStrategy) ValidateUpdate(ctx context.Context, obj, old runtime.Object) field.ErrorList {
	newPod := obj.(*api.Pod)
	oldPod := old.(*api.Pod)
	opts := podutil.GetValidationOptionsFromPodSpecAndMeta(&newPod.Spec, &oldPod.Spec, &newPod.ObjectMeta, &oldPod.ObjectMeta)
	opts.ResourceIsPod = true
	return corevalidation.ValidatePodEphemeralContainersUpdate(newPod, oldPod, opts)
}

// WarningsOnUpdate returns warnings for the given update.
func (podEphemeralContainersStrategy) WarningsOnUpdate(ctx context.Context, obj, old runtime.Object) []string {
	return nil
}

type podResizeStrategy struct {
	podStrategy

	resetFieldsFilter fieldpath.Filter
}

// ResizeStrategy wraps and exports the used podStrategy for the storage package.
var ResizeStrategy = podResizeStrategy{
	podStrategy: Strategy,
	resetFieldsFilter: fieldpath.NewIncludeMatcherFilter(
		fieldpath.MakePrefixMatcherOrDie("spec", "containers", fieldpath.MatchAnyPathElement(), "resources"),
		fieldpath.MakePrefixMatcherOrDie("spec", "containers", fieldpath.MatchAnyPathElement(), "resizePolicy"),
	),
}

// dropNonResizeUpdates discards all changes except for pod.Spec.Containers[*].Resources, pod.Spec.InitContainers[*].Resources, ResizePolicy and certain metadata
func dropNonResizeUpdates(newPod, oldPod *api.Pod) *api.Pod {
	pod := dropPodUpdates(newPod, oldPod)

	// Containers are not allowed to be re-ordered, but in case they were,
	// we don't want to corrupt them here. It will get caught in validation.
	oldCtrToIndex := make(map[string]int)
	oldInitCtrToIndex := make(map[string]int)
	for idx, ctr := range pod.Spec.Containers {
		oldCtrToIndex[ctr.Name] = idx
	}
	for idx, ctr := range pod.Spec.InitContainers {
		oldInitCtrToIndex[ctr.Name] = idx
	}

	for _, ctr := range newPod.Spec.Containers {
		idx, ok := oldCtrToIndex[ctr.Name]
		if !ok {
			continue
		}
		pod.Spec.Containers[idx].Resources = ctr.Resources
		pod.Spec.Containers[idx].ResizePolicy = ctr.ResizePolicy
	}

	if utilfeature.DefaultFeatureGate.Enabled(features.SidecarContainers) {
		for _, ctr := range newPod.Spec.InitContainers {
			idx, ok := oldInitCtrToIndex[ctr.Name]
			if !ok {
				continue
			}
			pod.Spec.InitContainers[idx].Resources = ctr.Resources
			pod.Spec.InitContainers[idx].ResizePolicy = ctr.ResizePolicy
		}
	}
	return pod
}

func (podResizeStrategy) PrepareForUpdate(ctx context.Context, obj, old runtime.Object) {
	newPod := obj.(*api.Pod)
	oldPod := old.(*api.Pod)

	*newPod = *dropNonResizeUpdates(newPod, oldPod)
	podutil.MarkPodProposedForResize(oldPod, newPod)
	podutil.DropDisabledPodFields(newPod, oldPod)
}

func (podResizeStrategy) ValidateUpdate(ctx context.Context, obj, old runtime.Object) field.ErrorList {
	newPod := obj.(*api.Pod)
	oldPod := old.(*api.Pod)
	opts := podutil.GetValidationOptionsFromPodSpecAndMeta(&newPod.Spec, &oldPod.Spec, &newPod.ObjectMeta, &oldPod.ObjectMeta)
	opts.ResourceIsPod = true
	return corevalidation.ValidatePodResize(newPod, oldPod, opts)
}

// WarningsOnUpdate returns warnings for the given update.
func (podResizeStrategy) WarningsOnUpdate(ctx context.Context, obj, old runtime.Object) []string {
	return nil
}

// GetResetFieldsFilter returns a set of fields filter reset by the strategy
// and should not be modified by the user.
func (p podResizeStrategy) GetResetFieldsFilter() map[fieldpath.APIVersion]fieldpath.Filter {
	return map[fieldpath.APIVersion]fieldpath.Filter{
		"v1": p.resetFieldsFilter,
	}
}

// dropPodUpdates drops any changes in the pod.
func dropPodUpdates(newPod, oldPod *api.Pod) *api.Pod {
	pod := oldPod.DeepCopy()
	pod.Name = newPod.Name
	pod.Namespace = newPod.Namespace
	pod.ResourceVersion = newPod.ResourceVersion
	pod.UID = newPod.UID

	return pod
}

// GetAttrs returns labels and fields of a given object for filtering purposes.
func GetAttrs(obj runtime.Object) (labels.Set, fields.Set, error) {
	pod, ok := obj.(*api.Pod)
	if !ok {
		return nil, nil, fmt.Errorf("not a pod")
	}
	return labels.Set(pod.ObjectMeta.Labels), ToSelectableFields(pod), nil
}

// MatchPod returns a generic matcher for a given label and field selector.
func MatchPod(label labels.Selector, field fields.Selector) storage.SelectionPredicate {
	var indexFields = []string{"spec.nodeName"}
	if utilfeature.DefaultFeatureGate.Enabled(features.StorageNamespaceIndex) {
		indexFields = append(indexFields, "metadata.namespace")
	}
	return storage.SelectionPredicate{
		Label:       label,
		Field:       field,
		GetAttrs:    GetAttrs,
		IndexFields: indexFields,
	}
}

// NodeNameTriggerFunc returns value spec.nodename of given object.
func NodeNameTriggerFunc(obj runtime.Object) string {
	return obj.(*api.Pod).Spec.NodeName
}

// NodeNameIndexFunc return value spec.nodename of given object.
func NodeNameIndexFunc(obj interface{}) ([]string, error) {
	pod, ok := obj.(*api.Pod)
	if !ok {
		return nil, fmt.Errorf("not a pod")
	}
	return []string{pod.Spec.NodeName}, nil
}

// NamespaceIndexFunc return value name of given object.
func NamespaceIndexFunc(obj interface{}) ([]string, error) {
	pod, ok := obj.(*api.Pod)
	if !ok {
		return nil, fmt.Errorf("not a pod")
	}
	return []string{pod.Namespace}, nil
}

// Indexers returns the indexers for pod storage.
func Indexers() *cache.Indexers {
	var indexers = cache.Indexers{
		storage.FieldIndex("spec.nodeName"): NodeNameIndexFunc,
	}
	if utilfeature.DefaultFeatureGate.Enabled(features.StorageNamespaceIndex) {
		indexers[storage.FieldIndex("metadata.namespace")] = NamespaceIndexFunc
	}
	return &indexers
}

// ToSelectableFields returns a field set that represents the object
// TODO: fields are not labels, and the validation rules for them do not apply.
func ToSelectableFields(pod *api.Pod) fields.Set {
	// The purpose of allocation with a given number of elements is to reduce
	// amount of allocations needed to create the fields.Set. If you add any
	// field here or the number of object-meta related fields changes, this should
	// be adjusted.
	podSpecificFieldsSet := make(fields.Set, 10)
	podSpecificFieldsSet["spec.nodeName"] = pod.Spec.NodeName
	podSpecificFieldsSet["spec.restartPolicy"] = string(pod.Spec.RestartPolicy)
	podSpecificFieldsSet["spec.schedulerName"] = string(pod.Spec.SchedulerName)
	podSpecificFieldsSet["spec.serviceAccountName"] = string(pod.Spec.ServiceAccountName)
	if pod.Spec.SecurityContext != nil {
		podSpecificFieldsSet["spec.hostNetwork"] = strconv.FormatBool(pod.Spec.SecurityContext.HostNetwork)
	} else {
		// default to false
		podSpecificFieldsSet["spec.hostNetwork"] = strconv.FormatBool(false)
	}
	podSpecificFieldsSet["status.phase"] = string(pod.Status.Phase)
	// TODO: add podIPs as a downward API value(s) with proper format
	podIP := ""
	if len(pod.Status.PodIPs) > 0 {
		podIP = string(pod.Status.PodIPs[0].IP)
	}
	podSpecificFieldsSet["status.podIP"] = podIP
	podSpecificFieldsSet["status.nominatedNodeName"] = string(pod.Status.NominatedNodeName)
	return generic.AddObjectMetaFieldsSet(podSpecificFieldsSet, &pod.ObjectMeta, true)
}

// ResourceGetter is an interface for retrieving resources by ResourceLocation.
type ResourceGetter interface {
	Get(context.Context, string, *metav1.GetOptions) (runtime.Object, error)
}

func getPod(ctx context.Context, getter ResourceGetter, name string) (*api.Pod, error) {
	obj, err := getter.Get(ctx, name, &metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	pod := obj.(*api.Pod)
	if pod == nil {
		return nil, fmt.Errorf("Unexpected object type: %#v", pod)
	}
	return pod, nil
}

// getPodIP returns primary IP for a Pod
func getPodIP(pod *api.Pod) string {
	if pod == nil {
		return ""
	}
	if len(pod.Status.PodIPs) > 0 {
		return pod.Status.PodIPs[0].IP
	}

	return ""
}

// ResourceLocation returns a URL to which one can send traffic for the specified pod.
func ResourceLocation(ctx context.Context, getter ResourceGetter, rt http.RoundTripper, id string) (*url.URL, http.RoundTripper, error) {
	// Allow ID as "podname" or "podname:port" or "scheme:podname:port".
	// If port is not specified, try to use the first defined port on the pod.
	scheme, name, port, valid := utilnet.SplitSchemeNamePort(id)
	if !valid {
		return nil, nil, errors.NewBadRequest(fmt.Sprintf("invalid pod request %q", id))
	}

	pod, err := getPod(ctx, getter, name)
	if err != nil {
		return nil, nil, err
	}

	// Try to figure out a port.
	if port == "" {
		for i := range pod.Spec.Containers {
			if len(pod.Spec.Containers[i].Ports) > 0 {
				port = fmt.Sprintf("%d", pod.Spec.Containers[i].Ports[0].ContainerPort)
				break
			}
		}
	}
	podIP := getPodIP(pod)
	if ip := netutils.ParseIPSloppy(podIP); ip == nil || !ip.IsGlobalUnicast() {
		return nil, nil, errors.NewBadRequest("address not allowed")
	}

	loc := &url.URL{
		Scheme: scheme,
	}
	if port == "" {
		// when using an ipv6 IP as a hostname in a URL, it must be wrapped in [...]
		// net.JoinHostPort does this for you.
		if strings.Contains(podIP, ":") {
			loc.Host = "[" + podIP + "]"
		} else {
			loc.Host = podIP
		}
	} else {
		loc.Host = net.JoinHostPort(podIP, port)
	}
	return loc, rt, nil
}

// LogLocation returns the log URL for a pod container. If opts.Container is blank
// and only one container is present in the pod, that container is used.
func LogLocation(
	ctx context.Context, getter ResourceGetter,
	connInfo client.ConnectionInfoGetter,
	name string,
	opts *api.PodLogOptions,
) (*url.URL, http.RoundTripper, error) {
	pod, err := getPod(ctx, getter, name)
	if err != nil {
		return nil, nil, err
	}

	// Try to figure out a container
	// If a container was provided, it must be valid
	container := opts.Container
	container, err = validateContainer(container, pod)
	if err != nil {
		return nil, nil, err
	}
	nodeName := types.NodeName(pod.Spec.NodeName)
	if len(nodeName) == 0 {
		// If pod has not been assigned a host, return an empty location
		return nil, nil, nil
	}
	nodeInfo, err := connInfo.GetConnectionInfo(ctx, nodeName)
	if err != nil {
		return nil, nil, err
	}
	params := url.Values{}
	if opts.Follow {
		params.Add("follow", "true")
	}
	if opts.Previous {
		params.Add("previous", "true")
	}
	if opts.Timestamps {
		params.Add("timestamps", "true")
	}
	if opts.SinceSeconds != nil {
		params.Add("sinceSeconds", strconv.FormatInt(*opts.SinceSeconds, 10))
	}
	if opts.SinceTime != nil {
		params.Add("sinceTime", opts.SinceTime.Format(time.RFC3339))
	}
	if opts.TailLines != nil {
		params.Add("tailLines", strconv.FormatInt(*opts.TailLines, 10))
	}
	if opts.LimitBytes != nil {
		params.Add("limitBytes", strconv.FormatInt(*opts.LimitBytes, 10))
	}
	if utilfeature.DefaultFeatureGate.Enabled(features.PodLogsQuerySplitStreams) {
		// With defaulters, We can be confident that opts.Stream is not nil here.
		params.Add("stream", string(*opts.Stream))
	}
	loc := &url.URL{
		Scheme:   nodeInfo.Scheme,
		Host:     net.JoinHostPort(nodeInfo.Hostname, nodeInfo.Port),
		Path:     fmt.Sprintf("/containerLogs/%s/%s/%s", pod.Namespace, pod.Name, container),
		RawQuery: params.Encode(),
	}

	if opts.InsecureSkipTLSVerifyBackend {
		return loc, nodeInfo.InsecureSkipTLSVerifyTransport, nil
	}
	return loc, nodeInfo.Transport, nil
}

func podHasContainerWithName(pod *api.Pod, containerName string) bool {
	var hasContainer bool
	podutil.VisitContainers(&pod.Spec, podutil.AllFeatureEnabledContainers(), func(c *api.Container, containerType podutil.ContainerType) bool {
		if c.Name == containerName {
			hasContainer = true
			return false
		}
		return true
	})
	return hasContainer
}

func streamParams(params url.Values, opts runtime.Object) error {
	switch opts := opts.(type) {
	case *api.PodExecOptions:
		if opts.Stdin {
			params.Add(api.ExecStdinParam, "1")
		}
		if opts.Stdout {
			params.Add(api.ExecStdoutParam, "1")
		}
		if opts.Stderr {
			params.Add(api.ExecStderrParam, "1")
		}
		if opts.TTY {
			params.Add(api.ExecTTYParam, "1")
		}
		for _, c := range opts.Command {
			params.Add("command", c)
		}
	case *api.PodAttachOptions:
		if opts.Stdin {
			params.Add(api.ExecStdinParam, "1")
		}
		if opts.Stdout {
			params.Add(api.ExecStdoutParam, "1")
		}
		if opts.Stderr {
			params.Add(api.ExecStderrParam, "1")
		}
		if opts.TTY {
			params.Add(api.ExecTTYParam, "1")
		}
	case *api.PodPortForwardOptions:
		if len(opts.Ports) > 0 {
			ports := make([]string, len(opts.Ports))
			for i, p := range opts.Ports {
				ports[i] = strconv.FormatInt(int64(p), 10)
			}
			params.Add(api.PortHeader, strings.Join(ports, ","))
		}
	default:
		return fmt.Errorf("Unknown object for streaming: %v", opts)
	}
	return nil
}

// AttachLocation returns the attach URL for a pod container. If opts.Container is blank
// and only one container is present in the pod, that container is used.
func AttachLocation(
	ctx context.Context,
	getter ResourceGetter,
	connInfo client.ConnectionInfoGetter,
	name string,
	opts *api.PodAttachOptions,
) (*url.URL, http.RoundTripper, error) {
	return streamLocation(ctx, getter, connInfo, name, opts, opts.Container, "attach")
}

// ExecLocation returns the exec URL for a pod container. If opts.Container is blank
// and only one container is present in the pod, that container is used.
func ExecLocation(
	ctx context.Context,
	getter ResourceGetter,
	connInfo client.ConnectionInfoGetter,
	name string,
	opts *api.PodExecOptions,
) (*url.URL, http.RoundTripper, error) {
	return streamLocation(ctx, getter, connInfo, name, opts, opts.Container, "exec")
}

func streamLocation(
	ctx context.Context,
	getter ResourceGetter,
	connInfo client.ConnectionInfoGetter,
	name string,
	opts runtime.Object,
	container,
	path string,
) (*url.URL, http.RoundTripper, error) {
	pod, err := getPod(ctx, getter, name)
	if err != nil {
		return nil, nil, err
	}

	// Try to figure out a container
	// If a container was provided, it must be valid
	container, err = validateContainer(container, pod)
	if err != nil {
		return nil, nil, err
	}

	nodeName := types.NodeName(pod.Spec.NodeName)
	if len(nodeName) == 0 {
		// If pod has not been assigned a host, return an empty location
		return nil, nil, errors.NewBadRequest(fmt.Sprintf("pod %s does not have a host assigned", name))
	}
	nodeInfo, err := connInfo.GetConnectionInfo(ctx, nodeName)
	if err != nil {
		return nil, nil, err
	}
	params := url.Values{}
	if err := streamParams(params, opts); err != nil {
		return nil, nil, err
	}
	loc := &url.URL{
		Scheme:   nodeInfo.Scheme,
		Host:     net.JoinHostPort(nodeInfo.Hostname, nodeInfo.Port),
		Path:     fmt.Sprintf("/%s/%s/%s/%s", path, pod.Namespace, pod.Name, container),
		RawQuery: params.Encode(),
	}
	return loc, nodeInfo.Transport, nil
}

// PortForwardLocation returns the port-forward URL for a pod.
func PortForwardLocation(
	ctx context.Context,
	getter ResourceGetter,
	connInfo client.ConnectionInfoGetter,
	name string,
	opts *api.PodPortForwardOptions,
) (*url.URL, http.RoundTripper, error) {
	pod, err := getPod(ctx, getter, name)
	if err != nil {
		return nil, nil, err
	}

	nodeName := types.NodeName(pod.Spec.NodeName)
	if len(nodeName) == 0 {
		// If pod has not been assigned a host, return an empty location
		return nil, nil, errors.NewBadRequest(fmt.Sprintf("pod %s does not have a host assigned", name))
	}
	nodeInfo, err := connInfo.GetConnectionInfo(ctx, nodeName)
	if err != nil {
		return nil, nil, err
	}
	params := url.Values{}
	if err := streamParams(params, opts); err != nil {
		return nil, nil, err
	}
	loc := &url.URL{
		Scheme:   nodeInfo.Scheme,
		Host:     net.JoinHostPort(nodeInfo.Hostname, nodeInfo.Port),
		Path:     fmt.Sprintf("/portForward/%s/%s", pod.Namespace, pod.Name),
		RawQuery: params.Encode(),
	}
	return loc, nodeInfo.Transport, nil
}

// validateContainer validate container is valid for pod, return valid container
func validateContainer(container string, pod *api.Pod) (string, error) {
	if len(container) == 0 {
		switch len(pod.Spec.Containers) {
		case 1:
			container = pod.Spec.Containers[0].Name
		case 0:
			return "", errors.NewBadRequest(fmt.Sprintf("a container name must be specified for pod %s", pod.Name))
		default:
			var containerNames []string
			podutil.VisitContainers(&pod.Spec, podutil.AllFeatureEnabledContainers(), func(c *api.Container, containerType podutil.ContainerType) bool {
				containerNames = append(containerNames, c.Name)
				return true
			})
			errStr := fmt.Sprintf("a container name must be specified for pod %s, choose one of: %s", pod.Name, containerNames)
			return "", errors.NewBadRequest(errStr)
		}
	} else {
		if !podHasContainerWithName(pod, container) {
			return "", errors.NewBadRequest(fmt.Sprintf("container %s is not valid for pod %s", container, pod.Name))
		}
	}

	return container, nil
}

// applyLabelKeysToLabelSelector obtains the label value from the given label set by the key in labelKeys,
// and merge to LabelSelector with the given operator:
func applyLabelKeysToLabelSelector(labelSelector *metav1.LabelSelector, labelKeys []string, operator metav1.LabelSelectorOperator, podLabels map[string]string) {
	for _, key := range labelKeys {
		if value, ok := podLabels[key]; ok {
			labelSelector.MatchExpressions = append(labelSelector.MatchExpressions, metav1.LabelSelectorRequirement{
				Key:      key,
				Operator: operator,
				Values:   []string{value},
			})
		}
	}
}

// applyMatchLabelKeysAndMismatchLabelKeys obtains the labels from the pod labels by the key in matchLabelKeys or mismatchLabelKeys,
// and merge to LabelSelector of PodAffinityTerm depending on field:
// - If matchLabelKeys, key in (value) is merged with LabelSelector.
// - If mismatchLabelKeys, key notin (value) is merged with LabelSelector.
func applyMatchLabelKeysAndMismatchLabelKeys(term *api.PodAffinityTerm, label map[string]string) {
	if (len(term.MatchLabelKeys) == 0 && len(term.MismatchLabelKeys) == 0) || term.LabelSelector == nil {
		// If LabelSelector is nil, we don't need to apply label keys to it because nil-LabelSelector is match none.
		return
	}

	applyLabelKeysToLabelSelector(term.LabelSelector, term.MatchLabelKeys, metav1.LabelSelectorOpIn, label)
	applyLabelKeysToLabelSelector(term.LabelSelector, term.MismatchLabelKeys, metav1.LabelSelectorOpNotIn, label)
}

func mutatePodAffinity(pod *api.Pod) {
	if !utilfeature.DefaultFeatureGate.Enabled(features.MatchLabelKeysInPodAffinity) || pod.Spec.Affinity == nil {
		return
	}
	if affinity := pod.Spec.Affinity.PodAffinity; affinity != nil {
		for i := range affinity.PreferredDuringSchedulingIgnoredDuringExecution {
			applyMatchLabelKeysAndMismatchLabelKeys(&affinity.PreferredDuringSchedulingIgnoredDuringExecution[i].PodAffinityTerm, pod.Labels)
		}
		for i := range affinity.RequiredDuringSchedulingIgnoredDuringExecution {
			applyMatchLabelKeysAndMismatchLabelKeys(&affinity.RequiredDuringSchedulingIgnoredDuringExecution[i], pod.Labels)
		}
	}
	if affinity := pod.Spec.Affinity.PodAntiAffinity; affinity != nil {
		for i := range affinity.PreferredDuringSchedulingIgnoredDuringExecution {
			applyMatchLabelKeysAndMismatchLabelKeys(&affinity.PreferredDuringSchedulingIgnoredDuringExecution[i].PodAffinityTerm, pod.Labels)
		}
		for i := range affinity.RequiredDuringSchedulingIgnoredDuringExecution {
			applyMatchLabelKeysAndMismatchLabelKeys(&affinity.RequiredDuringSchedulingIgnoredDuringExecution[i], pod.Labels)
		}
	}
}

// applySchedulingGatedCondition adds a {type:PodScheduled, reason:SchedulingGated} condition
// to a new-created Pod if necessary.
func applySchedulingGatedCondition(pod *api.Pod) {
	if len(pod.Spec.SchedulingGates) == 0 {
		return
	}

	// If found a condition with type PodScheduled, return.
	for _, condition := range pod.Status.Conditions {
		if condition.Type == api.PodScheduled {
			return
		}
	}

	podutil.UpdatePodCondition(&pod.Status, &api.PodCondition{
		Type:    api.PodScheduled,
		Status:  api.ConditionFalse,
		Reason:  apiv1.PodReasonSchedulingGated,
		Message: "Scheduling is blocked due to non-empty scheduling gates",
	})
}

// applyAppArmorVersionSkew implements the version skew behavior described in:
// https://github.com/kubernetes/enhancements/tree/master/keps/sig-node/24-apparmor#version-skew-strategy
func applyAppArmorVersionSkew(ctx context.Context, pod *api.Pod) {
	if pod.Spec.OS != nil && pod.Spec.OS.Name == api.Windows {
		return
	}

	var podProfile *api.AppArmorProfile
	if pod.Spec.SecurityContext != nil {
		podProfile = pod.Spec.SecurityContext.AppArmorProfile
	}

	// Handle the containers of the pod
	podutil.VisitContainers(&pod.Spec, podutil.AllFeatureEnabledContainers(),
		func(ctr *api.Container, _ podutil.ContainerType) bool {
			// get possible annotation and field
			key := api.DeprecatedAppArmorAnnotationKeyPrefix + ctr.Name
			annotation, hasAnnotation := pod.Annotations[key]

			var containerProfile *api.AppArmorProfile
			if ctr.SecurityContext != nil {
				containerProfile = ctr.SecurityContext.AppArmorProfile
			}

			// sync field and annotation
			if !hasAnnotation {
				newAnnotation := ""
				if containerProfile != nil {
					newAnnotation = appArmorAnnotationForField(containerProfile)
				} else if podProfile != nil {
					newAnnotation = appArmorAnnotationForField(podProfile)
				}

				if newAnnotation != "" {
					if pod.Annotations == nil {
						pod.Annotations = map[string]string{}
					}
					pod.Annotations[key] = newAnnotation
				}
			} else if containerProfile == nil {
				newField := apparmorFieldForAnnotation(annotation)
				if errs := corevalidation.ValidateAppArmorProfileField(newField, &field.Path{}); len(errs) > 0 {
					// Skip copying invalid value.
					newField = nil
				}

				// warn if we had an annotation that we couldn't derive a valid field from
				deprecationWarning := newField == nil

				// Only copy the annotation to the field if it is different from the pod-level profile.
				if newField != nil && !apiequality.Semantic.DeepEqual(newField, podProfile) {
					if ctr.SecurityContext == nil {
						ctr.SecurityContext = &api.SecurityContext{}
					}
					ctr.SecurityContext.AppArmorProfile = newField
					// warn if there was an annotation without a corresponding field
					deprecationWarning = true
				}

				if deprecationWarning {
					// Note: annotation deprecation warning must be added here rather than the
					// typical WarningsOnCreate path to emit the warning before syncing the
					// annotations & fields.
					fldPath := field.NewPath("metadata", "annotations").Key(key)
					warning.AddWarning(ctx, "", fmt.Sprintf(`%s: deprecated since v1.30; use the "appArmorProfile" field instead`, fldPath))
				}
			}

			return true
		})
}

// appArmorFieldForAnnotation takes a pod apparmor profile field and returns the
// converted annotation value
func appArmorAnnotationForField(field *api.AppArmorProfile) string {
	// If only apparmor fields are specified, add the corresponding annotations.
	// This ensures that the fields are enforced even if the node version
	// trails the API version
	switch field.Type {
	case api.AppArmorProfileTypeUnconfined:
		return api.DeprecatedAppArmorAnnotationValueUnconfined

	case api.AppArmorProfileTypeRuntimeDefault:
		return api.DeprecatedAppArmorAnnotationValueRuntimeDefault

	case api.AppArmorProfileTypeLocalhost:
		if field.LocalhostProfile != nil {
			return api.DeprecatedAppArmorAnnotationValueLocalhostPrefix + *field.LocalhostProfile
		}
	}

	// we can only reach this code path if the LocalhostProfile is nil but the
	// provided field type is AppArmorProfileTypeLocalhost or if an unrecognized
	// type is specified
	return ""
}

// apparmorFieldForAnnotation takes a pod annotation and returns the converted
// apparmor profile field.
func apparmorFieldForAnnotation(annotation string) *api.AppArmorProfile {
	if annotation == api.DeprecatedAppArmorAnnotationValueUnconfined {
		return &api.AppArmorProfile{Type: api.AppArmorProfileTypeUnconfined}
	}

	if annotation == api.DeprecatedAppArmorAnnotationValueRuntimeDefault {
		return &api.AppArmorProfile{Type: api.AppArmorProfileTypeRuntimeDefault}
	}

	if strings.HasPrefix(annotation, api.DeprecatedAppArmorAnnotationValueLocalhostPrefix) {
		localhostProfile := strings.TrimPrefix(annotation, api.DeprecatedAppArmorAnnotationValueLocalhostPrefix)
		if localhostProfile != "" {
			return &api.AppArmorProfile{
				Type:             api.AppArmorProfileTypeLocalhost,
				LocalhostProfile: &localhostProfile,
			}
		}
	}

	// we can only reach this code path if the localhostProfile name has a zero
	// length or if the annotation has an unrecognized value
	return nil
}
