/*
Copyright 2018 The Kubernetes Authors.

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

package util

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	policy "k8s.io/api/policy/v1"
	resourceapi "k8s.io/api/resource/v1beta1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apiserver/pkg/admission"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	cacheddiscovery "k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/informers"
	clientset "k8s.io/client-go/kubernetes"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/metadata"
	"k8s.io/client-go/metadata/metadatainformer"
	restclient "k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/scale"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/events"
	cliflag "k8s.io/component-base/cli/flag"
	pvutil "k8s.io/component-helpers/storage/volume"
	"k8s.io/controller-manager/pkg/informerfactory"
	"k8s.io/klog/v2"
	kubeschedulerconfigv1 "k8s.io/kube-scheduler/config/v1"
	"k8s.io/kubernetes/cmd/kube-apiserver/app/options"
	podutil "k8s.io/kubernetes/pkg/api/v1/pod"
	"k8s.io/kubernetes/pkg/controller/disruption"
	"k8s.io/kubernetes/pkg/controller/garbagecollector"
	"k8s.io/kubernetes/pkg/controller/namespace"
	"k8s.io/kubernetes/pkg/controller/resourceclaim"
	"k8s.io/kubernetes/pkg/controlplane"
	"k8s.io/kubernetes/pkg/features"
	"k8s.io/kubernetes/pkg/scheduler"
	kubeschedulerconfig "k8s.io/kubernetes/pkg/scheduler/apis/config"
	configtesting "k8s.io/kubernetes/pkg/scheduler/apis/config/testing"
	schedulerframework "k8s.io/kubernetes/pkg/scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/defaultpreemption"
	frameworkruntime "k8s.io/kubernetes/pkg/scheduler/framework/runtime"
	"k8s.io/kubernetes/pkg/scheduler/profile"
	st "k8s.io/kubernetes/pkg/scheduler/testing"
	taintutils "k8s.io/kubernetes/pkg/util/taints"
	"k8s.io/kubernetes/test/integration/framework"
	imageutils "k8s.io/kubernetes/test/utils/image"
	"k8s.io/kubernetes/test/utils/ktesting"
	"k8s.io/utils/ptr"
)

// ShutdownFunc represents the function handle to be called, typically in a defer handler, to shutdown a running module
type ShutdownFunc func()

// StartScheduler configures and starts a scheduler given a handle to the clientSet interface
// and event broadcaster. It returns the running scheduler and podInformer. Background goroutines
// will keep running until the context is canceled.
func StartScheduler(ctx context.Context, clientSet clientset.Interface, kubeConfig *restclient.Config, cfg *kubeschedulerconfig.KubeSchedulerConfiguration, outOfTreePluginRegistry frameworkruntime.Registry) (*scheduler.Scheduler, informers.SharedInformerFactory) {
	informerFactory := scheduler.NewInformerFactory(clientSet, 0)
	evtBroadcaster := events.NewBroadcaster(&events.EventSinkImpl{
		Interface: clientSet.EventsV1()})
	go func() {
		<-ctx.Done()
		evtBroadcaster.Shutdown()
	}()

	evtBroadcaster.StartRecordingToSink(ctx.Done())

	logger := klog.FromContext(ctx)

	sched, err := scheduler.New(
		ctx,
		clientSet,
		informerFactory,
		nil,
		profile.NewRecorderFactory(evtBroadcaster),
		scheduler.WithKubeConfig(kubeConfig),
		scheduler.WithProfiles(cfg.Profiles...),
		scheduler.WithPercentageOfNodesToScore(cfg.PercentageOfNodesToScore),
		scheduler.WithPodMaxBackoffSeconds(cfg.PodMaxBackoffSeconds),
		scheduler.WithPodInitialBackoffSeconds(cfg.PodInitialBackoffSeconds),
		scheduler.WithExtenders(cfg.Extenders...),
		scheduler.WithParallelism(cfg.Parallelism),
		scheduler.WithFrameworkOutOfTreeRegistry(outOfTreePluginRegistry),
	)
	if err != nil {
		logger.Error(err, "Error creating scheduler")
		klog.FlushAndExit(klog.ExitFlushTimeout, 1)
	}

	informerFactory.Start(ctx.Done())
	informerFactory.WaitForCacheSync(ctx.Done())
	if err = sched.WaitForHandlersSync(ctx); err != nil {
		logger.Error(err, "Failed waiting for handlers to sync")
		klog.FlushAndExit(klog.ExitFlushTimeout, 1)
	}
	logger.V(3).Info("Handlers synced")
	go sched.Run(ctx)

	return sched, informerFactory
}

func CreateResourceClaimController(ctx context.Context, tb ktesting.TB, clientSet clientset.Interface, informerFactory informers.SharedInformerFactory) func() {
	podInformer := informerFactory.Core().V1().Pods()
	claimInformer := informerFactory.Resource().V1beta1().ResourceClaims()
	claimTemplateInformer := informerFactory.Resource().V1beta1().ResourceClaimTemplates()
	features := resourceclaim.Features{
		AdminAccess:     true,
		PrioritizedList: true,
	}
	claimController, err := resourceclaim.NewController(klog.FromContext(ctx), features, clientSet, podInformer, claimInformer, claimTemplateInformer)
	if err != nil {
		tb.Fatalf("Error creating claim controller: %v", err)
	}
	return func() {
		go claimController.Run(ctx, 5 /* workers */)
	}
}

// StartFakePVController is a simplified pv controller logic that sets PVC VolumeName and annotation for each PV binding.
// TODO(mborsz): Use a real PV controller here.
func StartFakePVController(ctx context.Context, clientSet clientset.Interface, informerFactory informers.SharedInformerFactory) {
	pvInformer := informerFactory.Core().V1().PersistentVolumes()

	syncPV := func(obj *v1.PersistentVolume) {
		if obj.Spec.ClaimRef != nil {
			claimRef := obj.Spec.ClaimRef
			pvc, err := clientSet.CoreV1().PersistentVolumeClaims(claimRef.Namespace).Get(ctx, claimRef.Name, metav1.GetOptions{})
			if err != nil {
				// Note that the error can be anything, because components like
				// apiserver are also shutting down at the same time, but this
				// check is conservative and only ignores the "context canceled"
				// error while shutting down.
				if ctx.Err() == nil || !errors.Is(err, context.Canceled) {
					klog.Errorf("error while getting %v/%v: %v", claimRef.Namespace, claimRef.Name, err)
				}
				return
			}

			if pvc.Spec.VolumeName == "" {
				pvc.Spec.VolumeName = obj.Name
				metav1.SetMetaDataAnnotation(&pvc.ObjectMeta, pvutil.AnnBindCompleted, "yes")
				_, err := clientSet.CoreV1().PersistentVolumeClaims(claimRef.Namespace).Update(ctx, pvc, metav1.UpdateOptions{})
				if err != nil {
					if ctx.Err() == nil || !errors.Is(err, context.Canceled) {
						// Shutting down, no need to record this.
						klog.Errorf("error while updating %v/%v: %v", claimRef.Namespace, claimRef.Name, err)
					}
					return
				}
			}
		}
	}

	pvInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			syncPV(obj.(*v1.PersistentVolume))
		},
		UpdateFunc: func(_, obj interface{}) {
			syncPV(obj.(*v1.PersistentVolume))
		},
	})
}

// CreateGCController creates a garbage controller and returns a run function
// for it. The informer factory needs to be started before invoking that
// function.
func CreateGCController(ctx context.Context, tb ktesting.TB, restConfig restclient.Config, informerSet informers.SharedInformerFactory) func() {
	restclient.AddUserAgent(&restConfig, "gc-controller")
	clientSet := clientset.NewForConfigOrDie(&restConfig)
	metadataClient, err := metadata.NewForConfig(&restConfig)
	if err != nil {
		tb.Fatalf("Failed to create metadataClient: %v", err)
	}
	restMapper := restmapper.NewDeferredDiscoveryRESTMapper(cacheddiscovery.NewMemCacheClient(clientSet.Discovery()))
	restMapper.Reset()
	metadataInformers := metadatainformer.NewSharedInformerFactory(metadataClient, 0)
	alwaysStarted := make(chan struct{})
	close(alwaysStarted)
	gc, err := garbagecollector.NewGarbageCollector(
		ctx,
		clientSet,
		metadataClient,
		restMapper,
		garbagecollector.DefaultIgnoredResources(),
		informerfactory.NewInformerFactory(informerSet, metadataInformers),
		alwaysStarted,
	)
	if err != nil {
		tb.Fatalf("Failed creating garbage collector")
	}
	startGC := func() {
		syncPeriod := 5 * time.Second
		go wait.Until(func() {
			restMapper.Reset()
		}, syncPeriod, ctx.Done())
		go gc.Run(ctx, 1, syncPeriod)
		go gc.Sync(ctx, clientSet.Discovery(), syncPeriod)
	}
	return startGC
}

// CreateNamespaceController creates a namespace controller and returns a run
// function for it. The informer factory needs to be started before invoking
// that function.
func CreateNamespaceController(ctx context.Context, tb ktesting.TB, restConfig restclient.Config, informerSet informers.SharedInformerFactory) func() {
	restclient.AddUserAgent(&restConfig, "namespace-controller")
	clientSet := clientset.NewForConfigOrDie(&restConfig)
	metadataClient, err := metadata.NewForConfig(&restConfig)
	if err != nil {
		tb.Fatalf("Failed to create metadataClient: %v", err)
	}
	discoverResourcesFn := clientSet.Discovery().ServerPreferredNamespacedResources
	controller := namespace.NewNamespaceController(
		ctx,
		clientSet,
		metadataClient,
		discoverResourcesFn,
		informerSet.Core().V1().Namespaces(),
		10*time.Hour,
		v1.FinalizerKubernetes)
	return func() {
		go controller.Run(ctx, 5)
	}
}

// TestContext store necessary context info.
// It also contains some optional parameters for InitTestScheduler.
type TestContext struct {
	// DisableEventSink, if set to true before calling InitTestScheduler,
	// will skip the eventBroadcaster.StartRecordingToSink and thus
	// some extra goroutines which are tricky to get rid of after
	// a test.
	DisableEventSink bool

	NS                 *v1.Namespace
	ClientSet          clientset.Interface
	KubeConfig         *restclient.Config
	InformerFactory    informers.SharedInformerFactory
	DynInformerFactory dynamicinformer.DynamicSharedInformerFactory
	Scheduler          *scheduler.Scheduler
	// This is the top context when initializing the test environment.
	Ctx context.Context
	// CloseFn will stop the apiserver and clean up the resources
	// after itself, including shutting down its storage layer.
	CloseFn framework.TearDownFunc
	// This is the context when initializing scheduler.
	SchedulerCtx context.Context
	// SchedulerCloseFn will tear down the resources in creating scheduler,
	// including the scheduler itself.
	SchedulerCloseFn framework.TearDownFunc

	// RoundTrip, if set, will be called for every HTTP request going to the apiserver.
	// It can be used for error injection.
	RoundTrip atomic.Pointer[RoundTripWrapper]
}

type RoundTripWrapper func(http.RoundTripper, *http.Request) (*http.Response, error)

type roundTripWrapper struct {
	tc        *TestContext
	transport http.RoundTripper
}

func (r roundTripWrapper) RoundTrip(req *http.Request) (*http.Response, error) {
	wrapper := r.tc.RoundTrip.Load()
	if wrapper != nil {
		return (*wrapper)(r.transport, req)
	}
	return r.transport.RoundTrip(req)
}

var _ http.RoundTripper = roundTripWrapper{}

// CleanupNodes cleans all nodes which were created during integration test
func CleanupNodes(cs clientset.Interface, t *testing.T) {
	err := cs.CoreV1().Nodes().DeleteCollection(context.TODO(), *metav1.NewDeleteOptions(0), metav1.ListOptions{})
	if err != nil {
		t.Errorf("error while deleting all nodes: %v", err)
	}
}

// PodDeleted returns true if a pod is not found in the given namespace.
func PodDeleted(ctx context.Context, c clientset.Interface, podNamespace, podName string) wait.ConditionWithContextFunc {
	return func(context.Context) (bool, error) {
		pod, err := c.CoreV1().Pods(podNamespace).Get(ctx, podName, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		if pod.DeletionTimestamp != nil {
			return true, nil
		}
		return false, nil
	}
}

// PodsCleanedUp returns true if all pods are deleted in the specific namespace.
func PodsCleanedUp(ctx context.Context, c clientset.Interface, namespace string) wait.ConditionWithContextFunc {
	return func(context.Context) (bool, error) {
		list, err := c.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			return false, err
		}
		return len(list.Items) == 0, nil
	}
}

// SyncSchedulerInformerFactory starts informer and waits for caches to be synced
func SyncSchedulerInformerFactory(testCtx *TestContext) {
	testCtx.InformerFactory.Start(testCtx.SchedulerCtx.Done())
	if testCtx.DynInformerFactory != nil {
		testCtx.DynInformerFactory.Start(testCtx.SchedulerCtx.Done())
	}
	testCtx.InformerFactory.WaitForCacheSync(testCtx.SchedulerCtx.Done())
	if testCtx.DynInformerFactory != nil {
		testCtx.DynInformerFactory.WaitForCacheSync(testCtx.SchedulerCtx.Done())
	}
}

// CleanupTest cleans related resources which were created during integration test
func CleanupTest(t *testing.T, testCtx *TestContext) {
	// Cleanup nodes and namespaces.
	if err := testCtx.ClientSet.CoreV1().Nodes().DeleteCollection(testCtx.Ctx, *metav1.NewDeleteOptions(0), metav1.ListOptions{}); err != nil {
		t.Errorf("error while cleaning up nodes, error: %v", err)
	}
	framework.DeleteNamespaceOrDie(testCtx.ClientSet, testCtx.NS, t)
	// Terminate the scheduler and apiserver.
	testCtx.CloseFn()
}

func RemovePodFinalizersInNamespace(ctx context.Context, cs clientset.Interface, t *testing.T, ns string) {
	t.Helper()
	pods, err := cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("Failed obtaining list of pods: %v", err)
	}
	RemovePodFinalizers(ctx, cs, t, pods.Items...)
}

// RemovePodFinalizers removes pod finalizers for the pods
func RemovePodFinalizers(ctx context.Context, cs clientset.Interface, t *testing.T, pods ...v1.Pod) {
	t.Helper()
	for _, p := range pods {
		pod, err := cs.CoreV1().Pods(p.Namespace).Get(ctx, p.Name, metav1.GetOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			t.Errorf("error while removing pod finalizers for %v: %v", klog.KObj(&p), err)
		} else if pod != nil && len(pod.Finalizers) > 0 {
			// Use Patch to remove finalizer, instead of Update, to avoid transient
			// conflicts.
			patchBytes, _ := json.Marshal(map[string]interface{}{
				"metadata": map[string]interface{}{
					"$deleteFromPrimitiveList/finalizers": pod.Finalizers,
				},
			})
			_, err = cs.CoreV1().Pods(pod.Namespace).Patch(ctx, pod.Name, types.StrategicMergePatchType, patchBytes, metav1.PatchOptions{})
			if err != nil {
				t.Errorf("error while updating pod status for %v: %v", klog.KObj(&p), err)
			}
		}
	}
}

// CleanupPods deletes the given pods and waits for them to be actually deleted.
func CleanupPods(ctx context.Context, cs clientset.Interface, t *testing.T, pods []*v1.Pod) {
	for _, p := range pods {
		err := cs.CoreV1().Pods(p.Namespace).Delete(ctx, p.Name, *metav1.NewDeleteOptions(0))
		if err != nil && !apierrors.IsNotFound(err) {
			t.Errorf("error while deleting pod %v/%v: %v", p.Namespace, p.Name, err)
		}
	}
	for _, p := range pods {
		if err := wait.PollUntilContextTimeout(ctx, time.Duration(time.Microsecond.Seconds()), wait.ForeverTestTimeout, true,
			PodDeleted(ctx, cs, p.Namespace, p.Name)); err != nil {
			t.Errorf("error while waiting for pod  %v/%v to get deleted: %v", p.Namespace, p.Name, err)
		}
	}
}

// AddTaintToNode add taints to specific node
func AddTaintToNode(cs clientset.Interface, nodeName string, taint v1.Taint) error {
	node, err := cs.CoreV1().Nodes().Get(context.TODO(), nodeName, metav1.GetOptions{})
	if err != nil {
		return err
	}
	node.Spec.Taints = append(node.Spec.Taints, taint)
	_, err = cs.CoreV1().Nodes().Update(context.TODO(), node, metav1.UpdateOptions{})
	return err
}

// RemoveTaintOffNode removes a specific taint from a node
func RemoveTaintOffNode(cs clientset.Interface, nodeName string, taint v1.Taint) error {
	node, err := cs.CoreV1().Nodes().Get(context.TODO(), nodeName, metav1.GetOptions{})
	if err != nil {
		return err
	}
	var taints []v1.Taint
	for _, t := range node.Spec.Taints {
		if !t.MatchTaint(&taint) {
			taints = append(taints, t)
		}
	}
	node.Spec.Taints = taints
	_, err = cs.CoreV1().Nodes().Update(context.TODO(), node, metav1.UpdateOptions{})
	return err
}

// WaitForNodeTaints waits for a node to have the target taints and returns
// an error if it does not have taints within the given timeout.
func WaitForNodeTaints(ctx context.Context, cs clientset.Interface, node *v1.Node, taints []v1.Taint) error {
	return wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, 30*time.Second, false, NodeTainted(ctx, cs, node.Name, taints))
}

// NodeTainted return a condition function that returns true if the given node contains
// the taints.
func NodeTainted(ctx context.Context, cs clientset.Interface, nodeName string, taints []v1.Taint) wait.ConditionWithContextFunc {
	return func(context.Context) (bool, error) {
		node, err := cs.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		if err != nil {
			return false, err
		}

		// node.Spec.Taints may have more taints
		if len(taints) > len(node.Spec.Taints) {
			return false, nil
		}

		for _, taint := range taints {
			if !taintutils.TaintExists(node.Spec.Taints, &taint) {
				return false, nil
			}
		}

		return true, nil
	}
}

// NodeReadyStatus returns the status of first condition with type NodeReady.
// If none of the condition is of type NodeReady, returns an error.
func NodeReadyStatus(conditions []v1.NodeCondition) (v1.ConditionStatus, error) {
	for _, c := range conditions {
		if c.Type != v1.NodeReady {
			continue
		}
		// Just return the first condition with type NodeReady
		return c.Status, nil
	}
	return v1.ConditionFalse, errors.New("None of the conditions is of type NodeReady")
}

// GetTolerationSeconds gets the period of time the toleration
func GetTolerationSeconds(tolerations []v1.Toleration) (int64, error) {
	for _, t := range tolerations {
		if t.Key == v1.TaintNodeNotReady && t.Effect == v1.TaintEffectNoExecute && t.Operator == v1.TolerationOpExists {
			return *t.TolerationSeconds, nil
		}
	}
	return 0, fmt.Errorf("cannot find toleration")
}

// NodeCopyWithConditions duplicates the ode object with conditions
func NodeCopyWithConditions(node *v1.Node, conditions []v1.NodeCondition) *v1.Node {
	copy := node.DeepCopy()
	copy.ResourceVersion = "0"
	copy.Status.Conditions = conditions
	for i := range copy.Status.Conditions {
		copy.Status.Conditions[i].LastHeartbeatTime = metav1.Now()
	}
	return copy
}

// UpdateNodeStatus updates the status of node.
func UpdateNodeStatus(cs clientset.Interface, node *v1.Node) error {
	_, err := cs.CoreV1().Nodes().UpdateStatus(context.TODO(), node, metav1.UpdateOptions{})
	return err
}

// InitTestAPIServer initializes a test environment and creates an API server with default
// configuration.
// It registers cleanup functions to t.Cleanup(), they will be called when the test completes,
// no need to do this again.
func InitTestAPIServer(t *testing.T, nsPrefix string, admission admission.Interface) *TestContext {
	tCtx := ktesting.Init(t)
	testCtx := &TestContext{Ctx: tCtx}

	testCtx.ClientSet, testCtx.KubeConfig, testCtx.CloseFn = framework.StartTestServer(tCtx, t, framework.TestServerSetup{
		ModifyServerRunOptions: func(options *options.ServerRunOptions) {
			options.Admission.GenericAdmission.DisablePlugins = []string{"ServiceAccount", "TaintNodesByCondition", "Priority", "StorageObjectInUseProtection"}
			if utilfeature.DefaultFeatureGate.Enabled(features.DynamicResourceAllocation) {
				options.APIEnablement.RuntimeConfig = cliflag.ConfigurationMap{
					resourceapi.SchemeGroupVersion.String(): "true",
				}
			}
		},
		ModifyServerConfig: func(config *controlplane.Config) {
			if admission != nil {
				config.ControlPlane.Generic.AdmissionControl = admission
			}
		},
	})

	// Support wrapping HTTP requests.
	testCtx.KubeConfig.Wrap(func(transport http.RoundTripper) http.RoundTripper {
		return roundTripWrapper{tc: testCtx, transport: transport}
	})
	var err error
	testCtx.ClientSet, err = clientset.NewForConfig(testCtx.KubeConfig)
	if err != nil {
		t.Fatal(err)
	}

	oldCloseFn := testCtx.CloseFn
	testCtx.CloseFn = func() {
		tCtx.Cancel("tearing down apiserver")
		oldCloseFn()
	}

	if nsPrefix != "default" {
		testCtx.NS = framework.CreateNamespaceOrDie(testCtx.ClientSet, nsPrefix+string(uuid.NewUUID()), t)
	} else {
		testCtx.NS = framework.CreateNamespaceOrDie(testCtx.ClientSet, "default", t)
	}

	t.Cleanup(func() {
		CleanupTest(t, testCtx)
	})

	return testCtx
}

// WaitForSchedulerCacheCleanup waits for cleanup of scheduler's cache to complete
func WaitForSchedulerCacheCleanup(ctx context.Context, sched *scheduler.Scheduler, t *testing.T) {
	schedulerCacheIsEmpty := func(context.Context) (bool, error) {
		dump := sched.Cache.Dump()

		return len(dump.Nodes) == 0 && len(dump.AssumedPods) == 0, nil
	}

	if err := wait.PollUntilContextTimeout(ctx, time.Second, wait.ForeverTestTimeout, false, schedulerCacheIsEmpty); err != nil {
		t.Errorf("Failed to wait for scheduler cache cleanup: %v", err)
	}
}

// InitTestScheduler initializes a test environment and creates a scheduler with default
// configuration.
func InitTestScheduler(
	t *testing.T,
	testCtx *TestContext,
) *TestContext {
	// Pod preemption is enabled by default scheduler configuration.
	return InitTestSchedulerWithOptions(t, testCtx, 0)
}

// InitTestSchedulerWithOptions initializes a test environment and creates a scheduler with default
// configuration and other options.
func InitTestSchedulerWithOptions(
	t *testing.T,
	testCtx *TestContext,
	resyncPeriod time.Duration,
	opts ...scheduler.Option,
) *TestContext {
	ctx, cancel := context.WithCancel(testCtx.Ctx)
	testCtx.SchedulerCtx = ctx

	// 1. Create scheduler
	testCtx.InformerFactory = scheduler.NewInformerFactory(testCtx.ClientSet, resyncPeriod)
	if testCtx.KubeConfig != nil {
		dynClient := dynamic.NewForConfigOrDie(testCtx.KubeConfig)
		testCtx.DynInformerFactory = dynamicinformer.NewFilteredDynamicSharedInformerFactory(dynClient, 0, v1.NamespaceAll, nil)
	}

	var err error
	eventBroadcaster := events.NewBroadcaster(&events.EventSinkImpl{
		Interface: testCtx.ClientSet.EventsV1(),
	})

	opts = append(opts, scheduler.WithKubeConfig(testCtx.KubeConfig))
	testCtx.Scheduler, err = scheduler.New(
		ctx,
		testCtx.ClientSet,
		testCtx.InformerFactory,
		testCtx.DynInformerFactory,
		profile.NewRecorderFactory(eventBroadcaster),
		opts...,
	)

	if err != nil {
		t.Fatalf("Couldn't create scheduler: %v", err)
	}

	if !testCtx.DisableEventSink {
		eventBroadcaster.StartRecordingToSink(ctx.Done())
	}

	oldCloseFn := testCtx.CloseFn
	testCtx.CloseFn = func() {
		oldCloseFn()
		eventBroadcaster.Shutdown()
	}

	testCtx.SchedulerCloseFn = func() {
		cancel()
		eventBroadcaster.Shutdown()
	}

	return testCtx
}

// WaitForPodToScheduleWithTimeout waits for a pod to get scheduled and returns
// an error if it does not scheduled within the given timeout.
func WaitForPodToScheduleWithTimeout(ctx context.Context, cs clientset.Interface, pod *v1.Pod, timeout time.Duration) error {
	return wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, timeout, false, PodScheduled(cs, pod.Namespace, pod.Name))
}

// WaitForPodToSchedule waits for a pod to get scheduled and returns an error if
// it does not get scheduled within the timeout duration (30 seconds).
func WaitForPodToSchedule(ctx context.Context, cs clientset.Interface, pod *v1.Pod) error {
	return WaitForPodToScheduleWithTimeout(ctx, cs, pod, 30*time.Second)
}

// PodScheduled checks if the pod has been scheduled
func PodScheduled(c clientset.Interface, podNamespace, podName string) wait.ConditionWithContextFunc {
	return func(ctx context.Context) (bool, error) {
		pod, err := c.CoreV1().Pods(podNamespace).Get(ctx, podName, metav1.GetOptions{})
		if err != nil {
			// This could be a connection error so we want to retry.
			return false, nil
		}
		if pod.Spec.NodeName == "" {
			return false, nil
		}
		return true, nil
	}
}

// InitDisruptionController initializes and runs a Disruption Controller to properly
// update PodDisuptionBudget objects.
func InitDisruptionController(t *testing.T, testCtx *TestContext) *disruption.DisruptionController {
	informers := informers.NewSharedInformerFactory(testCtx.ClientSet, 12*time.Hour)

	discoveryClient := cacheddiscovery.NewMemCacheClient(testCtx.ClientSet.Discovery())
	mapper := restmapper.NewDeferredDiscoveryRESTMapper(discoveryClient)

	config := restclient.CopyConfig(testCtx.KubeConfig)
	scaleKindResolver := scale.NewDiscoveryScaleKindResolver(testCtx.ClientSet.Discovery())
	scaleClient, err := scale.NewForConfig(config, mapper, dynamic.LegacyAPIPathResolverFunc, scaleKindResolver)
	if err != nil {
		t.Fatalf("Error in create scaleClient: %v", err)
	}

	dc := disruption.NewDisruptionController(
		testCtx.Ctx,
		informers.Core().V1().Pods(),
		informers.Policy().V1().PodDisruptionBudgets(),
		informers.Core().V1().ReplicationControllers(),
		informers.Apps().V1().ReplicaSets(),
		informers.Apps().V1().Deployments(),
		informers.Apps().V1().StatefulSets(),
		testCtx.ClientSet,
		mapper,
		scaleClient,
		testCtx.ClientSet.Discovery())

	informers.Start(testCtx.Scheduler.StopEverything)
	informers.WaitForCacheSync(testCtx.Scheduler.StopEverything)
	go dc.Run(testCtx.Ctx)
	return dc
}

// InitTestSchedulerWithNS initializes a test environment and creates API server and scheduler with default
// configuration.
func InitTestSchedulerWithNS(t *testing.T, nsPrefix string, opts ...scheduler.Option) *TestContext {
	testCtx := InitTestSchedulerWithOptions(t, InitTestAPIServer(t, nsPrefix, nil), 0, opts...)
	SyncSchedulerInformerFactory(testCtx)
	go testCtx.Scheduler.Run(testCtx.SchedulerCtx)
	return testCtx
}

// InitTestDisablePreemption initializes a test environment and creates API server and scheduler with default
// configuration but with pod preemption disabled.
func InitTestDisablePreemption(t *testing.T, nsPrefix string) *TestContext {
	cfg := configtesting.V1ToInternalWithDefaults(t, kubeschedulerconfigv1.KubeSchedulerConfiguration{
		Profiles: []kubeschedulerconfigv1.KubeSchedulerProfile{{
			SchedulerName: ptr.To(v1.DefaultSchedulerName),
			Plugins: &kubeschedulerconfigv1.Plugins{
				PostFilter: kubeschedulerconfigv1.PluginSet{
					Disabled: []kubeschedulerconfigv1.Plugin{
						{Name: defaultpreemption.Name},
					},
				},
			},
		}},
	})
	testCtx := InitTestSchedulerWithOptions(
		t, InitTestAPIServer(t, nsPrefix, nil),
		0,
		scheduler.WithProfiles(cfg.Profiles...))
	SyncSchedulerInformerFactory(testCtx)
	go testCtx.Scheduler.Run(testCtx.SchedulerCtx)
	return testCtx
}

// WaitForReflection waits till the passFunc confirms that the object it expects
// to see is in the store. Used to observe reflected events.
func WaitForReflection(ctx context.Context, t *testing.T, nodeLister corelisters.NodeLister, key string,
	passFunc func(n interface{}) bool) error {
	var nodes []*v1.Node
	err := wait.PollUntilContextTimeout(ctx, time.Millisecond*100, wait.ForeverTestTimeout, false, func(context.Context) (bool, error) {
		n, err := nodeLister.Get(key)

		switch {
		case err == nil && passFunc(n):
			return true, nil
		case apierrors.IsNotFound(err):
			nodes = append(nodes, nil)
		case err != nil:
			t.Errorf("Unexpected error: %v", err)
		default:
			nodes = append(nodes, n)
		}

		return false, nil
	})
	if err != nil {
		t.Logf("Logging consecutive node versions received from store:")
		for i, n := range nodes {
			t.Logf("%d: %#v", i, n)
		}
	}
	return err
}

func UpdateNode(cs clientset.Interface, node *v1.Node) (*v1.Node, error) {
	return cs.CoreV1().Nodes().Update(context.TODO(), node, metav1.UpdateOptions{})
}

func CreateNode(cs clientset.Interface, node *v1.Node) (*v1.Node, error) {
	return cs.CoreV1().Nodes().Create(context.TODO(), node, metav1.CreateOptions{})
}

func createNodes(cs clientset.Interface, prefix string, wrapper *st.NodeWrapper, numNodes int) ([]*v1.Node, error) {
	nodes := make([]*v1.Node, numNodes)
	for i := 0; i < numNodes; i++ {
		nodeName := fmt.Sprintf("%v-%d", prefix, i)
		node, err := CreateNode(cs, wrapper.Name(nodeName).Label("kubernetes.io/hostname", nodeName).Obj())
		if err != nil {
			return nodes[:], err
		}
		nodes[i] = node
	}
	return nodes[:], nil
}

// CreateAndWaitForNodesInCache calls createNodes(), and wait for the created
// nodes to be present in scheduler cache.
func CreateAndWaitForNodesInCache(testCtx *TestContext, prefix string, wrapper *st.NodeWrapper, numNodes int) ([]*v1.Node, error) {
	existingNodes := testCtx.Scheduler.Cache.NodeCount()
	nodes, err := createNodes(testCtx.ClientSet, prefix, wrapper, numNodes)
	if err != nil {
		return nodes, fmt.Errorf("cannot create nodes: %v", err)
	}
	return nodes, WaitForNodesInCache(testCtx.Ctx, testCtx.Scheduler, numNodes+existingNodes)
}

// WaitForNodesInCache ensures at least <nodeCount> nodes are present in scheduler cache
// within 30 seconds; otherwise returns false.
func WaitForNodesInCache(ctx context.Context, sched *scheduler.Scheduler, nodeCount int) error {
	err := wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, wait.ForeverTestTimeout, false, func(context.Context) (bool, error) {
		return sched.Cache.NodeCount() >= nodeCount, nil
	})
	if err != nil {
		return fmt.Errorf("cannot obtain available nodes in scheduler cache: %v", err)
	}
	return nil
}

type PausePodConfig struct {
	Name                              string
	Namespace                         string
	Affinity                          *v1.Affinity
	Annotations, Labels, NodeSelector map[string]string
	Resources                         *v1.ResourceRequirements
	Tolerations                       []v1.Toleration
	NodeName                          string
	SchedulerName                     string
	Priority                          *int32
	PreemptionPolicy                  *v1.PreemptionPolicy
	PriorityClassName                 string
	Volumes                           []v1.Volume
}

// InitPausePod initializes a pod API object from the given config. It is used
// mainly in pod creation process.
func InitPausePod(conf *PausePodConfig) *v1.Pod {
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        conf.Name,
			Namespace:   conf.Namespace,
			Labels:      conf.Labels,
			Annotations: conf.Annotations,
		},
		Spec: v1.PodSpec{
			NodeSelector: conf.NodeSelector,
			Affinity:     conf.Affinity,
			Containers: []v1.Container{
				{
					Name:  conf.Name,
					Image: imageutils.GetPauseImageName(),
				},
			},
			Tolerations:       conf.Tolerations,
			NodeName:          conf.NodeName,
			SchedulerName:     conf.SchedulerName,
			Priority:          conf.Priority,
			PreemptionPolicy:  conf.PreemptionPolicy,
			PriorityClassName: conf.PriorityClassName,
			Volumes:           conf.Volumes,
		},
	}
	if conf.Resources != nil {
		pod.Spec.Containers[0].Resources = *conf.Resources
	}
	return pod
}

// CreatePausePod creates a pod with "Pause" image and the given config and
// return its pointer and error status.
func CreatePausePod(cs clientset.Interface, p *v1.Pod) (*v1.Pod, error) {
	return cs.CoreV1().Pods(p.Namespace).Create(context.TODO(), p, metav1.CreateOptions{})
}

// CreatePausePodWithResource creates a pod with "Pause" image and the given
// resources and returns its pointer and error status. The resource list can be
// nil.
func CreatePausePodWithResource(cs clientset.Interface, podName string,
	nsName string, res *v1.ResourceList) (*v1.Pod, error) {
	var conf PausePodConfig
	if res == nil {
		conf = PausePodConfig{
			Name:      podName,
			Namespace: nsName,
		}
	} else {
		conf = PausePodConfig{
			Name:      podName,
			Namespace: nsName,
			Resources: &v1.ResourceRequirements{
				Requests: *res,
			},
		}
	}
	return CreatePausePod(cs, InitPausePod(&conf))
}

// CreatePVC creates a PersistentVolumeClaim with the given config and returns
// its pointer and error status.
func CreatePVC(cs clientset.Interface, pvc *v1.PersistentVolumeClaim) (*v1.PersistentVolumeClaim, error) {
	return cs.CoreV1().PersistentVolumeClaims(pvc.Namespace).Create(context.TODO(), pvc, metav1.CreateOptions{})
}

// CreatePV creates a PersistentVolume with the given config and returns its
// pointer and error status.
func CreatePV(cs clientset.Interface, pv *v1.PersistentVolume) (*v1.PersistentVolume, error) {
	return cs.CoreV1().PersistentVolumes().Create(context.TODO(), pv, metav1.CreateOptions{})
}

// DeletePVC deletes the given PVC in the given namespace.
func DeletePVC(cs clientset.Interface, pvcName string, nsName string) error {
	return cs.CoreV1().PersistentVolumeClaims(nsName).Delete(context.TODO(), pvcName, *metav1.NewDeleteOptions(0))
}

// DeletePV deletes the given PV in the given namespace.
func DeletePV(cs clientset.Interface, pvName string) error {
	return cs.CoreV1().PersistentVolumes().Delete(context.TODO(), pvName, *metav1.NewDeleteOptions(0))
}

// RunPausePod creates a pod with "Pause" image and the given config and waits
// until it is scheduled. It returns its pointer and error status.
func RunPausePod(cs clientset.Interface, pod *v1.Pod) (*v1.Pod, error) {
	pod, err := cs.CoreV1().Pods(pod.Namespace).Create(context.TODO(), pod, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to create pause pod: %v", err)
	}
	if err = WaitForPodToSchedule(context.TODO(), cs, pod); err != nil {
		return pod, fmt.Errorf("Pod %v/%v didn't schedule successfully. Error: %v", pod.Namespace, pod.Name, err)
	}
	if pod, err = cs.CoreV1().Pods(pod.Namespace).Get(context.TODO(), pod.Name, metav1.GetOptions{}); err != nil {
		return pod, fmt.Errorf("failed to get pod %v/%v info: %v", pod.Namespace, pod.Name, err)
	}
	return pod, nil
}

type PodWithContainersConfig struct {
	Name       string
	Namespace  string
	Containers []v1.Container
}

// InitPodWithContainers initializes a pod API object from the given config. This is used primarily for generating
// pods with containers each having a specific image.
func InitPodWithContainers(cs clientset.Interface, conf *PodWithContainersConfig) *v1.Pod {
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      conf.Name,
			Namespace: conf.Namespace,
		},
		Spec: v1.PodSpec{
			Containers: conf.Containers,
		},
	}
	return pod
}

// RunPodWithContainers creates a pod with given config and containers and waits
// until it is scheduled. It returns its pointer and error status.
func RunPodWithContainers(cs clientset.Interface, pod *v1.Pod) (*v1.Pod, error) {
	pod, err := cs.CoreV1().Pods(pod.Namespace).Create(context.TODO(), pod, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to create pod-with-containers: %v", err)
	}
	if err = WaitForPodToSchedule(context.TODO(), cs, pod); err != nil {
		return pod, fmt.Errorf("Pod %v didn't schedule successfully. Error: %v", pod.Name, err)
	}
	if pod, err = cs.CoreV1().Pods(pod.Namespace).Get(context.TODO(), pod.Name, metav1.GetOptions{}); err != nil {
		return pod, fmt.Errorf("failed to get pod %v info: %v", pod.Name, err)
	}
	return pod, nil
}

// PodIsGettingEvicted returns true if the pod's deletion timestamp is set.
func PodIsGettingEvicted(c clientset.Interface, podNamespace, podName string) wait.ConditionWithContextFunc {
	return func(ctx context.Context) (bool, error) {
		pod, err := c.CoreV1().Pods(podNamespace).Get(ctx, podName, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		if pod.DeletionTimestamp != nil {
			return true, nil
		}
		return false, nil
	}
}

// PodScheduledIn returns true if a given pod is placed onto one of the expected nodes.
func PodScheduledIn(c clientset.Interface, podNamespace, podName string, nodeNames []string) wait.ConditionWithContextFunc {
	return func(ctx context.Context) (bool, error) {
		pod, err := c.CoreV1().Pods(podNamespace).Get(ctx, podName, metav1.GetOptions{})
		if err != nil {
			// This could be a connection error so we want to retry.
			return false, nil
		}
		if pod.Spec.NodeName == "" {
			return false, nil
		}
		for _, nodeName := range nodeNames {
			if pod.Spec.NodeName == nodeName {
				return true, nil
			}
		}
		return false, nil
	}
}

// PodUnschedulable returns a condition function that returns true if the given pod
// gets unschedulable status of reason 'Unschedulable'.
func PodUnschedulable(c clientset.Interface, podNamespace, podName string) wait.ConditionWithContextFunc {
	return func(ctx context.Context) (bool, error) {
		pod, err := c.CoreV1().Pods(podNamespace).Get(ctx, podName, metav1.GetOptions{})
		if err != nil {
			// This could be a connection error so we want to retry.
			return false, nil
		}
		_, cond := podutil.GetPodCondition(&pod.Status, v1.PodScheduled)
		return cond != nil && cond.Status == v1.ConditionFalse &&
			cond.Reason == v1.PodReasonUnschedulable && pod.Spec.NodeName == "", nil
	}
}

// PodSchedulingError returns a condition function that returns true if the given pod
// gets unschedulable status for reasons other than "Unschedulable". The scheduler
// records such reasons in case of error.
func PodSchedulingError(c clientset.Interface, podNamespace, podName string) wait.ConditionWithContextFunc {
	return func(ctx context.Context) (bool, error) {
		pod, err := c.CoreV1().Pods(podNamespace).Get(ctx, podName, metav1.GetOptions{})
		if err != nil {
			// This could be a connection error so we want to retry.
			return false, nil
		}
		_, cond := podutil.GetPodCondition(&pod.Status, v1.PodScheduled)
		return cond != nil && cond.Status == v1.ConditionFalse &&
			cond.Reason != v1.PodReasonUnschedulable, nil
	}
}

// PodSchedulingGated returns a condition function that returns true if the given pod
// gets unschedulable status of reason 'SchedulingGated'.
func PodSchedulingGated(ctx context.Context, c clientset.Interface, podNamespace, podName string) wait.ConditionWithContextFunc {
	return func(context.Context) (bool, error) {
		pod, err := c.CoreV1().Pods(podNamespace).Get(ctx, podName, metav1.GetOptions{})
		if err != nil {
			// This could be a connection error so we want to retry.
			return false, nil
		}
		_, cond := podutil.GetPodCondition(&pod.Status, v1.PodScheduled)
		return cond != nil && cond.Status == v1.ConditionFalse &&
			cond.Reason == v1.PodReasonSchedulingGated && pod.Spec.NodeName == "", nil
	}
}

// WaitForPodUnschedulableWithTimeout waits for a pod to fail scheduling and returns
// an error if it does not become unschedulable within the given timeout.
func WaitForPodUnschedulableWithTimeout(ctx context.Context, cs clientset.Interface, pod *v1.Pod, timeout time.Duration) error {
	return wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, timeout, false, PodUnschedulable(cs, pod.Namespace, pod.Name))
}

// WaitForPodUnschedulable waits for a pod to fail scheduling and returns
// an error if it does not become unschedulable within the timeout duration (30 seconds).
func WaitForPodUnschedulable(ctx context.Context, cs clientset.Interface, pod *v1.Pod) error {
	return WaitForPodUnschedulableWithTimeout(ctx, cs, pod, 30*time.Second)
}

// WaitForPodSchedulingGated waits for a pod to be in scheduling gated state
// and returns an error if it does not fall into this state within the given timeout.
func WaitForPodSchedulingGated(ctx context.Context, cs clientset.Interface, pod *v1.Pod, timeout time.Duration) error {
	return wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, timeout, false, PodSchedulingGated(ctx, cs, pod.Namespace, pod.Name))
}

// WaitForPDBsStable waits for PDBs to have "CurrentHealthy" status equal to
// the expected values.
func WaitForPDBsStable(testCtx *TestContext, pdbs []*policy.PodDisruptionBudget, pdbPodNum []int32) error {
	return wait.PollUntilContextTimeout(testCtx.Ctx, time.Second, 60*time.Second, false, func(context.Context) (bool, error) {
		pdbList, err := testCtx.ClientSet.PolicyV1().PodDisruptionBudgets(testCtx.NS.Name).List(testCtx.Ctx, metav1.ListOptions{})
		if err != nil {
			return false, err
		}
		if len(pdbList.Items) != len(pdbs) {
			return false, nil
		}
		for i, pdb := range pdbs {
			found := false
			for _, cpdb := range pdbList.Items {
				if pdb.Name == cpdb.Name && pdb.Namespace == cpdb.Namespace {
					found = true
					if cpdb.Status.CurrentHealthy != pdbPodNum[i] {
						return false, nil
					}
				}
			}
			if !found {
				return false, nil
			}
		}
		return true, nil
	})
}

// WaitCachedPodsStable waits until scheduler cache has the given pods.
func WaitCachedPodsStable(testCtx *TestContext, pods []*v1.Pod) error {
	return wait.PollUntilContextTimeout(testCtx.Ctx, time.Second, 30*time.Second, false, func(context.Context) (bool, error) {
		cachedPods, err := testCtx.Scheduler.Cache.PodCount()
		if err != nil {
			return false, err
		}
		if len(pods) != cachedPods {
			return false, nil
		}
		for _, p := range pods {
			actualPod, err1 := testCtx.ClientSet.CoreV1().Pods(p.Namespace).Get(testCtx.Ctx, p.Name, metav1.GetOptions{})
			if err1 != nil {
				return false, err1
			}
			cachedPod, err2 := testCtx.Scheduler.Cache.GetPod(actualPod)
			if err2 != nil || cachedPod == nil {
				return false, err2
			}
		}
		return true, nil
	})
}

// DeletePod deletes the given pod in the given namespace.
func DeletePod(cs clientset.Interface, podName string, nsName string) error {
	return cs.CoreV1().Pods(nsName).Delete(context.TODO(), podName, *metav1.NewDeleteOptions(0))
}

func GetPod(cs clientset.Interface, podName string, podNamespace string) (*v1.Pod, error) {
	return cs.CoreV1().Pods(podNamespace).Get(context.TODO(), podName, metav1.GetOptions{})
}

func CreateNamespacesWithLabels(cs clientset.Interface, namespaces []string, labels map[string]string) error {
	for _, n := range namespaces {
		ns := v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: n, Labels: labels}}
		if _, err := cs.CoreV1().Namespaces().Create(context.TODO(), &ns, metav1.CreateOptions{}); err != nil {
			return err
		}
	}
	return nil
}

// timeout returns a timeout error if the given `f` function doesn't
// complete within `d` duration; otherwise it returns nil.
func timeout(ctx context.Context, d time.Duration, f func()) error {
	ctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()

	done := make(chan struct{})
	go func() {
		f()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// NextPodOrDie returns the next Pod in the scheduler queue.
// The operation needs to be completed within 5 seconds; otherwise the test gets aborted.
func NextPodOrDie(t *testing.T, testCtx *TestContext) *schedulerframework.QueuedPodInfo {
	t.Helper()

	var podInfo *schedulerframework.QueuedPodInfo
	logger := klog.FromContext(testCtx.Ctx)
	// NextPod() is a blocking operation. Wrap it in timeout() to avoid relying on
	// default go testing timeout (10m) to abort.
	if err := timeout(testCtx.Ctx, time.Second*5, func() {
		podInfo, _ = testCtx.Scheduler.NextPod(logger)
	}); err != nil {
		t.Fatalf("Timed out waiting for the Pod to be popped: %v", err)
	}
	return podInfo
}
