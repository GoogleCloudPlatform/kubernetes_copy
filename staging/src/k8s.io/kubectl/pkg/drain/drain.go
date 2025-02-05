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

package drain

import (
	"context"
	"fmt"
	"io"
	"math"
	"time"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	policyv1beta1 "k8s.io/api/policy/v1beta1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/cli-runtime/pkg/resource"
	"k8s.io/client-go/kubernetes"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
)

const (
	// EvictionKind represents the kind of evictions object
	EvictionKind = "Eviction"
	// EvictionSubresource represents the kind of evictions object as pod's subresource
	EvictionSubresource = "pods/eviction"
	podSkipMsgTemplate  = "pod %q has DeletionTimestamp older than %v seconds, skipping\n"
)

// Helper contains the parameters to control the behaviour of drainer
type Helper struct {
	Ctx    context.Context
	Client kubernetes.Interface
	Force  bool

	// GracePeriodSeconds is how long to wait for a pod to terminate.
	// IMPORTANT: 0 means "delete immediately"; set to a negative value
	// to use the pod's terminationGracePeriodSeconds.
	GracePeriodSeconds int

	IgnoreAllDaemonSets bool
	Timeout             time.Duration
	DeleteEmptyDirData  bool
	Selector            string
	PodSelector         string
	ChunkSize           int64

	// DisableEviction forces drain to use delete rather than evict
	DisableEviction bool

	// SkipWaitForDeleteTimeoutSeconds ignores pods that have a
	// DeletionTimeStamp > N seconds. It's up to the user to decide when this
	// option is appropriate; examples include the Node is unready and the pods
	// won't drain otherwise
	SkipWaitForDeleteTimeoutSeconds int

	// DelayBetweenNodeSeconds introduces a pause between node drains when
	// a drain command is issued that targets multiple nodes. This provides
	// an affordance for new workloads to start up during upgrades on clusters
	// that do not have appropriately configured PDBs.
	DelayBetweenNodeSeconds int

	// AdditionalFilters are applied sequentially after base drain filters to
	// exclude pods using custom logic.  Any filter that returns PodDeleteStatus
	// with Delete == false will immediately stop execution of further filters.
	AdditionalFilters []PodFilter

	Out    io.Writer
	ErrOut io.Writer

	DryRunStrategy cmdutil.DryRunStrategy

	// OnPodDeletedOrEvicted is called when a pod is evicted/deleted; for printing progress output
	// Deprecated: use OnPodDeletionOrEvictionFinished instead
	OnPodDeletedOrEvicted func(pod *corev1.Pod, usingEviction bool)

	// OnPodDeletionOrEvictionFinished is called when a pod is eviction/deletetion is failed; for printing progress output
	OnPodDeletionOrEvictionFinished func(pod *corev1.Pod, usingEviction bool, err error)

	// OnPodDeletionOrEvictionStarted is called when a pod eviction/deletion is started; for printing progress output
	OnPodDeletionOrEvictionStarted func(pod *corev1.Pod, usingEviction bool)
}

type waitForDeleteParams struct {
	ctx                             context.Context
	pods                            []corev1.Pod
	interval                        time.Duration
	timeout                         time.Duration
	usingEviction                   bool
	getPodFn                        func(string, string) (*corev1.Pod, error)
	onDoneFn                        func(pod *corev1.Pod, usingEviction bool)
	onFinishFn                      func(pod *corev1.Pod, usingEviction bool, err error)
	globalTimeout                   time.Duration
	skipWaitForDeleteTimeoutSeconds int
	out                             io.Writer
}

// CheckEvictionSupport uses Discovery API to find out if the server support
// eviction subresource If support, it will return its groupVersion; Otherwise,
// it will return an empty GroupVersion
func CheckEvictionSupport(clientset kubernetes.Interface) (schema.GroupVersion, error) {
	discoveryClient := clientset.Discovery()

	// version info available in subresources since v1.8.0 in https://github.com/kubernetes/kubernetes/pull/49971
	resourceList, err := discoveryClient.ServerResourcesForGroupVersion("v1")
	if err != nil {
		return schema.GroupVersion{}, err
	}
	for _, resource := range resourceList.APIResources {
		if resource.Name == EvictionSubresource && resource.Kind == EvictionKind && len(resource.Group) > 0 && len(resource.Version) > 0 {
			return schema.GroupVersion{Group: resource.Group, Version: resource.Version}, nil
		}
	}
	return schema.GroupVersion{}, nil
}

func (d *Helper) makeDeleteOptions() metav1.DeleteOptions {
	deleteOptions := metav1.DeleteOptions{}
	if d.GracePeriodSeconds >= 0 {
		gracePeriodSeconds := int64(d.GracePeriodSeconds)
		deleteOptions.GracePeriodSeconds = &gracePeriodSeconds
	}
	if d.DryRunStrategy == cmdutil.DryRunServer {
		deleteOptions.DryRun = []string{metav1.DryRunAll}
	}
	return deleteOptions
}

// DeletePod will delete the given pod, or return an error if it couldn't
func (d *Helper) DeletePod(pod corev1.Pod) error {
	return d.Client.CoreV1().Pods(pod.Namespace).Delete(d.getContext(), pod.Name, d.makeDeleteOptions())
}

// EvictPod will evict the given pod, or return an error if it couldn't
func (d *Helper) EvictPod(pod corev1.Pod, evictionGroupVersion schema.GroupVersion) error {
	delOpts := d.makeDeleteOptions()

	switch evictionGroupVersion {
	case policyv1.SchemeGroupVersion:
		// send policy/v1 if the server supports it
		eviction := &policyv1.Eviction{
			ObjectMeta: metav1.ObjectMeta{
				Name:      pod.Name,
				Namespace: pod.Namespace,
			},
			DeleteOptions: &delOpts,
		}
		return d.Client.PolicyV1().Evictions(eviction.Namespace).Evict(d.getContext(), eviction)

	default:
		// otherwise, fall back to policy/v1beta1, supported by all servers that support the eviction subresource
		eviction := &policyv1beta1.Eviction{
			ObjectMeta: metav1.ObjectMeta{
				Name:      pod.Name,
				Namespace: pod.Namespace,
			},
			DeleteOptions: &delOpts,
		}
		return d.Client.PolicyV1beta1().Evictions(eviction.Namespace).Evict(d.getContext(), eviction)
	}
}

// GetPodsForDeletion receives resource info for a node, and returns those pods as PodDeleteList,
// or error if it cannot list pods. All pods that are ready to be deleted can be obtained with .Pods(),
// and string with all warning can be obtained with .Warnings(), and .Errors() for all errors that
// occurred during deletion.
func (d *Helper) GetPodsForDeletion(nodeName string) (*PodDeleteList, []error) {
	labelSelector, err := labels.Parse(d.PodSelector)
	if err != nil {
		return nil, []error{err}
	}

	podList := &corev1.PodList{}
	initialOpts := &metav1.ListOptions{
		LabelSelector: labelSelector.String(),
		FieldSelector: fields.SelectorFromSet(fields.Set{"spec.nodeName": nodeName}).String(),
		Limit:         d.ChunkSize,
	}

	err = resource.FollowContinue(initialOpts, func(options metav1.ListOptions) (runtime.Object, error) {
		newPods, err := d.Client.CoreV1().Pods(metav1.NamespaceAll).List(d.getContext(), options)
		if err != nil {
			podR := corev1.SchemeGroupVersion.WithResource(corev1.ResourcePods.String())
			return nil, resource.EnhanceListError(err, options, podR.String())
		}
		podList.Items = append(podList.Items, newPods.Items...)
		return newPods, nil
	})

	if err != nil {
		return nil, []error{err}
	}

	list := filterPods(podList, d.makeFilters())
	if errs := list.errors(); len(errs) > 0 {
		return list, errs
	}

	return list, nil
}

func filterPods(podList *corev1.PodList, filters []PodFilter) *PodDeleteList {
	pods := []PodDelete{}
	for _, pod := range podList.Items {
		var status PodDeleteStatus
		for _, filter := range filters {
			status = filter(pod)
			if !status.Delete {
				// short-circuit as soon as pod is filtered out
				// at that point, there is no reason to run pod
				// through any additional filters
				break
			}
		}
		// Add the pod to PodDeleteList no matter what PodDeleteStatus is,
		// those pods whose PodDeleteStatus is false like DaemonSet will
		// be catched by list.errors()
		pod.Kind = "Pod"
		pod.APIVersion = "v1"
		pods = append(pods, PodDelete{
			Pod:    pod,
			Status: status,
		})
	}
	list := &PodDeleteList{items: pods}
	return list
}

// DeleteOrEvictPods deletes or evicts the pods on the api server
func (d *Helper) DeleteOrEvictPods(pods []corev1.Pod) error {
	if len(pods) == 0 {
		return nil
	}

	// TODO(justinsb): unnecessary?
	getPodFn := func(namespace, name string) (*corev1.Pod, error) {
		return d.Client.CoreV1().Pods(namespace).Get(d.getContext(), name, metav1.GetOptions{})
	}

	if !d.DisableEviction {
		evictionGroupVersion, err := CheckEvictionSupport(d.Client)
		if err != nil {
			return err
		}

		if !evictionGroupVersion.Empty() {
			return d.evictPods(pods, evictionGroupVersion, getPodFn)
		}
	}

	return d.deletePods(pods, getPodFn)
}

func (d *Helper) evictPods(pods []corev1.Pod, evictionGroupVersion schema.GroupVersion, getPodFn func(namespace, name string) (*corev1.Pod, error)) error {
	returnCh := make(chan error, 1)
	// 0 timeout means infinite, we use MaxInt64 to represent it.
	var globalTimeout time.Duration
	if d.Timeout == 0 {
		globalTimeout = time.Duration(math.MaxInt64)
	} else {
		globalTimeout = d.Timeout
	}
	ctx, cancel := context.WithTimeout(d.getContext(), globalTimeout)
	defer cancel()
	for _, pod := range pods {
		go func(pod corev1.Pod, returnCh chan error) {
			refreshPod := false
			for {
				switch d.DryRunStrategy {
				case cmdutil.DryRunServer:
					fmt.Fprintf(d.Out, "evicting pod %s/%s (server dry run)\n", pod.Namespace, pod.Name)
				default:
					if d.OnPodDeletionOrEvictionStarted != nil {
						d.OnPodDeletionOrEvictionStarted(&pod, true)
					}
					fmt.Fprintf(d.Out, "evicting pod %s/%s\n", pod.Namespace, pod.Name)
				}
				select {
				case <-ctx.Done():
					// return here or we'll leak a goroutine.
					returnCh <- fmt.Errorf("error when evicting pods/%q -n %q: global timeout reached: %v", pod.Name, pod.Namespace, globalTimeout)
					return
				default:
				}

				// Create a temporary pod so we don't mutate the pod in the loop.
				activePod := pod
				if refreshPod {
					freshPod, err := getPodFn(pod.Namespace, pod.Name)
					// We ignore errors and let eviction sort it out with
					// the original pod.
					if err == nil {
						activePod = *freshPod
					}
					refreshPod = false
				}

				err := d.EvictPod(activePod, evictionGroupVersion)
				if err == nil {
					break
				} else if apierrors.IsNotFound(err) {
					returnCh <- nil
					return
				} else if apierrors.IsTooManyRequests(err) {
					fmt.Fprintf(d.ErrOut, "error when evicting pods/%q -n %q (will retry after 5s): %v\n", activePod.Name, activePod.Namespace, err)
					time.Sleep(5 * time.Second)
				} else if !activePod.ObjectMeta.DeletionTimestamp.IsZero() && apierrors.IsForbidden(err) && apierrors.HasStatusCause(err, corev1.NamespaceTerminatingCause) {
					// an eviction request in a deleting namespace will throw a forbidden error,
					// if the pod is already marked deleted, we can ignore this error, an eviction
					// request will never succeed, but we will waitForDelete for this pod.
					break
				} else if apierrors.IsForbidden(err) && apierrors.HasStatusCause(err, corev1.NamespaceTerminatingCause) {
					// an eviction request in a deleting namespace will throw a forbidden error,
					// if the pod is not marked deleted, we retry until it is.
					fmt.Fprintf(d.ErrOut, "error when evicting pod %q from terminating namespace %q (will retry after 5s): %v\n", activePod.Name, activePod.Namespace, err)
					time.Sleep(5 * time.Second)
				} else {
					returnCh <- fmt.Errorf("error when evicting pods/%q -n %q: %v", activePod.Name, activePod.Namespace, err)
					return
				}
			}
			if d.DryRunStrategy == cmdutil.DryRunServer {
				returnCh <- nil
				return
			}
			params := waitForDeleteParams{
				ctx:                             ctx,
				pods:                            []corev1.Pod{pod},
				interval:                        1 * time.Second,
				timeout:                         time.Duration(math.MaxInt64),
				usingEviction:                   true,
				getPodFn:                        getPodFn,
				onDoneFn:                        d.OnPodDeletedOrEvicted,
				onFinishFn:                      d.OnPodDeletionOrEvictionFinished,
				globalTimeout:                   globalTimeout,
				skipWaitForDeleteTimeoutSeconds: d.SkipWaitForDeleteTimeoutSeconds,
				out:                             d.Out,
			}
			_, err := waitForDelete(params)
			if err == nil {
				returnCh <- nil
			} else {
				returnCh <- fmt.Errorf("error when waiting for pod %q in namespace %q to terminate: %v", pod.Name, pod.Namespace, err)
			}
		}(pod, returnCh)
	}

	doneCount := 0
	var errors []error

	numPods := len(pods)
	for doneCount < numPods {
		select {
		case err := <-returnCh:
			doneCount++
			if err != nil {
				errors = append(errors, err)
			}
		}
	}

	return utilerrors.NewAggregate(errors)
}

func (d *Helper) deletePods(pods []corev1.Pod, getPodFn func(namespace, name string) (*corev1.Pod, error)) error {
	// 0 timeout means infinite, we use MaxInt64 to represent it.
	var globalTimeout time.Duration
	if d.Timeout == 0 {
		globalTimeout = time.Duration(math.MaxInt64)
	} else {
		globalTimeout = d.Timeout
	}
	for _, pod := range pods {
		err := d.DeletePod(pod)
		if err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		if d.OnPodDeletionOrEvictionStarted != nil {
			d.OnPodDeletionOrEvictionStarted(&pod, false)
		}
	}
	ctx := d.getContext()
	params := waitForDeleteParams{
		ctx:                             ctx,
		pods:                            pods,
		interval:                        1 * time.Second,
		timeout:                         globalTimeout,
		usingEviction:                   false,
		getPodFn:                        getPodFn,
		onDoneFn:                        d.OnPodDeletedOrEvicted,
		onFinishFn:                      d.OnPodDeletionOrEvictionFinished,
		globalTimeout:                   globalTimeout,
		skipWaitForDeleteTimeoutSeconds: d.SkipWaitForDeleteTimeoutSeconds,
		out:                             d.Out,
	}
	_, err := waitForDelete(params)
	return err
}

func waitForDelete(params waitForDeleteParams) ([]corev1.Pod, error) {
	pods := params.pods
	if params.ctx == nil {
		params.ctx = context.Background()
	}
	err := wait.PollUntilContextTimeout(params.ctx, params.interval, params.timeout, true, func(ctx context.Context) (done bool, err error) {
		pendingPods := []corev1.Pod{}
		for i, pod := range pods {
			p, err := params.getPodFn(pod.Namespace, pod.Name)
			// The implementation of getPodFn that uses client-go returns an empty Pod struct when there is an error,
			// so we need to check that err == nil and p != nil to know that a pod was found successfully.
			if apierrors.IsNotFound(err) || (err == nil && p != nil && p.ObjectMeta.UID != pod.ObjectMeta.UID) {
				if params.onFinishFn != nil {
					params.onFinishFn(&pod, params.usingEviction, nil)
				} else if params.onDoneFn != nil {
					params.onDoneFn(&pod, params.usingEviction)
				}
				continue
			} else if err != nil {
				if params.onFinishFn != nil {
					params.onFinishFn(&pod, params.usingEviction, err)
				}
				return false, err
			} else {
				if shouldSkipPod(*p, params.skipWaitForDeleteTimeoutSeconds) {
					fmt.Fprintf(params.out, podSkipMsgTemplate, pod.Name, params.skipWaitForDeleteTimeoutSeconds)
					continue
				}
				pendingPods = append(pendingPods, pods[i])
			}
		}
		pods = pendingPods
		return len(pods) == 0, nil
	})
	return pods, err
}

// Since Helper does not have a constructor, we can't enforce Helper.Ctx != nil
// Multiple public methods prevent us from initializing the context in a single
// place as well.
func (d *Helper) getContext() context.Context {
	if d.Ctx != nil {
		return d.Ctx
	}
	return context.Background()
}
