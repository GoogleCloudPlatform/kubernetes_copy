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

// Package attachdetach implements a controller to manage volume attach and detach
// operations.
package attachdetach

import (
	"context"
	"fmt"
	"net"
	"time"

	"k8s.io/klog/v2"
	"k8s.io/mount-utils"
	utilexec "k8s.io/utils/exec"

	authenticationv1 "k8s.io/api/authentication/v1"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	coreinformers "k8s.io/client-go/informers/core/v1"
	storageinformersv1 "k8s.io/client-go/informers/storage/v1"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	corelisters "k8s.io/client-go/listers/core/v1"
	storagelistersv1 "k8s.io/client-go/listers/storage/v1"
	kcache "k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/events"
	"k8s.io/client-go/util/workqueue"
	csitrans "k8s.io/csi-translation-lib"
	"k8s.io/kubernetes/pkg/controller/volume/attachdetach/cache"
	"k8s.io/kubernetes/pkg/controller/volume/attachdetach/metrics"
	"k8s.io/kubernetes/pkg/controller/volume/attachdetach/populator"
	"k8s.io/kubernetes/pkg/controller/volume/attachdetach/reconciler"
	"k8s.io/kubernetes/pkg/controller/volume/attachdetach/statusupdater"
	"k8s.io/kubernetes/pkg/controller/volume/attachdetach/util"
	"k8s.io/kubernetes/pkg/controller/volume/common"
	"k8s.io/kubernetes/pkg/volume"
	"k8s.io/kubernetes/pkg/volume/csi"
	"k8s.io/kubernetes/pkg/volume/csimigration"
	volumeutil "k8s.io/kubernetes/pkg/volume/util"
	"k8s.io/kubernetes/pkg/volume/util/operationexecutor"
	"k8s.io/kubernetes/pkg/volume/util/subpath"
	"k8s.io/kubernetes/pkg/volume/util/volumepathhandler"
)

// TimerConfig contains configuration of internal attach/detach timers and
// should be used only to speed up tests. DefaultTimerConfig is the suggested
// timer configuration for production.
type TimerConfig struct {
	// ReconcilerLoopPeriod is the amount of time the reconciler loop waits
	// between successive executions
	ReconcilerLoopPeriod time.Duration

	// ReconcilerMaxWaitForUnmountDuration is the maximum amount of time the
	// attach detach controller will wait for a volume to be safely unmounted
	// from its node. Once this time has expired, the controller will assume the
	// node or kubelet are unresponsive and will detach the volume anyway.
	ReconcilerMaxWaitForUnmountDuration time.Duration

	// DesiredStateOfWorldPopulatorLoopSleepPeriod is the amount of time the
	// DesiredStateOfWorldPopulator loop waits between successive executions
	DesiredStateOfWorldPopulatorLoopSleepPeriod time.Duration

	// DesiredStateOfWorldPopulatorListPodsRetryDuration is the amount of
	// time the DesiredStateOfWorldPopulator loop waits between list pods
	// calls.
	DesiredStateOfWorldPopulatorListPodsRetryDuration time.Duration
}

// DefaultTimerConfig is the default configuration of Attach/Detach controller
// timers.
var DefaultTimerConfig = TimerConfig{
	ReconcilerLoopPeriod:                              100 * time.Millisecond,
	ReconcilerMaxWaitForUnmountDuration:               6 * time.Minute,
	DesiredStateOfWorldPopulatorLoopSleepPeriod:       1 * time.Minute,
	DesiredStateOfWorldPopulatorListPodsRetryDuration: 3 * time.Minute,
}

// AttachDetachController defines the operations supported by this controller.
type AttachDetachController interface {
	Run(ctx context.Context)
	GetDesiredStateOfWorld() cache.DesiredStateOfWorld
}

// NewAttachDetachController returns a new instance of AttachDetachController.
func NewAttachDetachController(
	ctx context.Context,
	kubeClient clientset.Interface,
	podInformer coreinformers.PodInformer,
	nodeInformer coreinformers.NodeInformer,
	pvcInformer coreinformers.PersistentVolumeClaimInformer,
	pvInformer coreinformers.PersistentVolumeInformer,
	csiNodeInformer storageinformersv1.CSINodeInformer,
	csiDriverInformer storageinformersv1.CSIDriverInformer,
	volumeAttachmentInformer storageinformersv1.VolumeAttachmentInformer,
	plugins []volume.VolumePlugin,
	prober volume.DynamicPluginProber,
	disableReconciliationSync bool,
	reconcilerSyncDuration time.Duration,
	disableForceDetachOnTimeout bool,
	timerConfig TimerConfig) (AttachDetachController, error) {

	logger := klog.FromContext(ctx)

	adc := &attachDetachController{
		kubeClient:  kubeClient,
		pvcLister:   pvcInformer.Lister(),
		pvcsSynced:  pvcInformer.Informer().HasSynced,
		pvLister:    pvInformer.Lister(),
		pvsSynced:   pvInformer.Informer().HasSynced,
		podLister:   podInformer.Lister(),
		podsSynced:  podInformer.Informer().HasSynced,
		podIndexer:  podInformer.Informer().GetIndexer(),
		nodeLister:  nodeInformer.Lister(),
		nodesSynced: nodeInformer.Informer().HasSynced,
		pvcQueue: workqueue.NewTypedRateLimitingQueueWithConfig(
			workqueue.DefaultTypedControllerRateLimiter[string](),
			workqueue.TypedRateLimitingQueueConfig[string]{Name: "pvcs"},
		),
	}

	adc.csiNodeLister = csiNodeInformer.Lister()
	adc.csiNodeSynced = csiNodeInformer.Informer().HasSynced

	adc.csiDriverLister = csiDriverInformer.Lister()
	adc.csiDriversSynced = csiDriverInformer.Informer().HasSynced

	adc.volumeAttachmentLister = volumeAttachmentInformer.Lister()
	adc.volumeAttachmentSynced = volumeAttachmentInformer.Informer().HasSynced

	if err := adc.volumePluginMgr.InitPlugins(plugins, prober, adc); err != nil {
		return nil, fmt.Errorf("could not initialize volume plugins for Attach/Detach Controller: %w", err)
	}

	adc.broadcaster = events.NewBroadcaster(&events.EventSinkImpl{Interface: kubeClient.EventsV1()})
	recorder := adc.broadcaster.NewRecorder(scheme.Scheme, "attachdetach-controller")
	blkutil := volumepathhandler.NewBlockVolumePathHandler()

	adc.desiredStateOfWorld = cache.NewDesiredStateOfWorld(&adc.volumePluginMgr)
	adc.actualStateOfWorld = cache.NewActualStateOfWorld(&adc.volumePluginMgr)
	adc.attacherDetacher =
		operationexecutor.NewOperationExecutor(operationexecutor.NewOperationGenerator(
			kubeClient,
			&adc.volumePluginMgr,
			recorder,
			blkutil))
	adc.nodeStatusUpdater = statusupdater.NewNodeStatusUpdater(
		kubeClient, nodeInformer.Lister(), adc.actualStateOfWorld)

	// Default these to values in options
	adc.reconciler = reconciler.NewReconciler(
		timerConfig.ReconcilerLoopPeriod,
		timerConfig.ReconcilerMaxWaitForUnmountDuration,
		reconcilerSyncDuration,
		disableReconciliationSync,
		disableForceDetachOnTimeout,
		adc.desiredStateOfWorld,
		adc.actualStateOfWorld,
		adc.attacherDetacher,
		adc.nodeStatusUpdater,
		adc.nodeLister,
		adc.pvcLister,
		recorder)

	csiTranslator := csitrans.New()
	adc.intreeToCSITranslator = csiTranslator
	adc.csiMigratedPluginManager = csimigration.NewPluginManager(csiTranslator, utilfeature.DefaultFeatureGate)

	adc.desiredStateOfWorldPopulator = populator.NewDesiredStateOfWorldPopulator(
		timerConfig.DesiredStateOfWorldPopulatorLoopSleepPeriod,
		timerConfig.DesiredStateOfWorldPopulatorListPodsRetryDuration,
		podInformer.Lister(),
		adc.desiredStateOfWorld,
		&adc.volumePluginMgr,
		pvcInformer.Lister(),
		pvInformer.Lister(),
		adc.csiMigratedPluginManager,
		adc.intreeToCSITranslator)

	podInformer.Informer().AddEventHandler(kcache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			adc.podAdd(logger, obj)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			adc.podUpdate(logger, oldObj, newObj)
		},
		DeleteFunc: func(obj interface{}) {
			adc.podDelete(logger, obj)
		},
	})

	// This custom indexer will index pods by its PVC keys. Then we don't need
	// to iterate all pods every time to find pods which reference given PVC.
	if err := common.AddPodPVCIndexerIfNotPresent(adc.podIndexer); err != nil {
		return nil, fmt.Errorf("could not initialize attach detach controller: %w", err)
	}

	nodeInformer.Informer().AddEventHandler(kcache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			adc.nodeAdd(logger, obj)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			adc.nodeUpdate(logger, oldObj, newObj)
		},
		DeleteFunc: func(obj interface{}) {
			adc.nodeDelete(logger, obj)
		},
	})

	pvcInformer.Informer().AddEventHandler(kcache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			adc.enqueuePVC(obj)
		},
		UpdateFunc: func(old, new interface{}) {
			adc.enqueuePVC(new)
		},
	})

	return adc, nil
}

type attachDetachController struct {
	// kubeClient is the kube API client used by volumehost to communicate with
	// the API server.
	kubeClient clientset.Interface

	// pvcLister is the shared PVC lister used to fetch and store PVC
	// objects from the API server. It is shared with other controllers and
	// therefore the PVC objects in its store should be treated as immutable.
	pvcLister  corelisters.PersistentVolumeClaimLister
	pvcsSynced kcache.InformerSynced

	// pvLister is the shared PV lister used to fetch and store PV objects
	// from the API server. It is shared with other controllers and therefore
	// the PV objects in its store should be treated as immutable.
	pvLister  corelisters.PersistentVolumeLister
	pvsSynced kcache.InformerSynced

	podLister  corelisters.PodLister
	podsSynced kcache.InformerSynced
	podIndexer kcache.Indexer

	nodeLister  corelisters.NodeLister
	nodesSynced kcache.InformerSynced

	csiNodeLister storagelistersv1.CSINodeLister
	csiNodeSynced kcache.InformerSynced

	// csiDriverLister is the shared CSIDriver lister used to fetch and store
	// CSIDriver objects from the API server. It is shared with other controllers
	// and therefore the CSIDriver objects in its store should be treated as immutable.
	csiDriverLister  storagelistersv1.CSIDriverLister
	csiDriversSynced kcache.InformerSynced

	// volumeAttachmentLister is the shared volumeAttachment lister used to fetch and store
	// VolumeAttachment objects from the API server. It is shared with other controllers
	// and therefore the VolumeAttachment objects in its store should be treated as immutable.
	volumeAttachmentLister storagelistersv1.VolumeAttachmentLister
	volumeAttachmentSynced kcache.InformerSynced

	// volumePluginMgr used to initialize and fetch volume plugins
	volumePluginMgr volume.VolumePluginMgr

	// desiredStateOfWorld is a data structure containing the desired state of
	// the world according to this controller: i.e. what nodes the controller
	// is managing, what volumes it wants be attached to these nodes, and which
	// pods are scheduled to those nodes referencing the volumes.
	// The data structure is populated by the controller using a stream of node
	// and pod API server objects fetched by the informers.
	desiredStateOfWorld cache.DesiredStateOfWorld

	// actualStateOfWorld is a data structure containing the actual state of
	// the world according to this controller: i.e. which volumes are attached
	// to which nodes.
	// The data structure is populated upon successful completion of attach and
	// detach actions triggered by the controller and a periodic sync with
	// storage providers for the "true" state of the world.
	actualStateOfWorld cache.ActualStateOfWorld

	// attacherDetacher is used to start asynchronous attach and operations
	attacherDetacher operationexecutor.OperationExecutor

	// reconciler is used to run an asynchronous periodic loop to reconcile the
	// desiredStateOfWorld with the actualStateOfWorld by triggering attach
	// detach operations using the attacherDetacher.
	reconciler reconciler.Reconciler

	// nodeStatusUpdater is used to update node status with the list of attached
	// volumes
	nodeStatusUpdater statusupdater.NodeStatusUpdater

	// desiredStateOfWorldPopulator runs an asynchronous periodic loop to
	// populate the current pods using podInformer.
	desiredStateOfWorldPopulator populator.DesiredStateOfWorldPopulator

	// broadcaster is broadcasting events
	broadcaster events.EventBroadcaster

	// pvcQueue is used to queue pvc objects
	pvcQueue workqueue.TypedRateLimitingInterface[string]

	// csiMigratedPluginManager detects in-tree plugins that have been migrated to CSI
	csiMigratedPluginManager csimigration.PluginManager

	// intreeToCSITranslator translates from in-tree volume specs to CSI
	intreeToCSITranslator csimigration.InTreeToCSITranslator
}

func (adc *attachDetachController) Run(ctx context.Context) {
	defer runtime.HandleCrash()
	defer adc.pvcQueue.ShutDown()

	// Start events processing pipeline.
	adc.broadcaster.StartStructuredLogging(3)
	adc.broadcaster.StartRecordingToSink(ctx.Done())
	defer adc.broadcaster.Shutdown()

	logger := klog.FromContext(ctx)
	logger.Info("Starting attach detach controller")
	defer logger.Info("Shutting down attach detach controller")

	synced := []kcache.InformerSynced{adc.podsSynced, adc.nodesSynced, adc.pvcsSynced, adc.pvsSynced,
		adc.csiNodeSynced, adc.csiDriversSynced, adc.volumeAttachmentSynced}
	if !kcache.WaitForNamedCacheSync("attach detach", ctx.Done(), synced...) {
		return
	}

	err := adc.populateActualStateOfWorld(logger)
	if err != nil {
		logger.Error(err, "Error populating the actual state of world")
	}
	err = adc.populateDesiredStateOfWorld(logger)
	if err != nil {
		logger.Error(err, "Error populating the desired state of world")
	}
	go adc.reconciler.Run(ctx)
	go adc.desiredStateOfWorldPopulator.Run(ctx)
	go wait.UntilWithContext(ctx, adc.pvcWorker, time.Second)
	metrics.Register(adc.pvcLister,
		adc.pvLister,
		adc.podLister,
		adc.actualStateOfWorld,
		adc.desiredStateOfWorld,
		&adc.volumePluginMgr,
		adc.csiMigratedPluginManager,
		adc.intreeToCSITranslator)

	<-ctx.Done()
}

func (adc *attachDetachController) populateActualStateOfWorld(logger klog.Logger) error {
	logger.V(5).Info("Populating ActualStateOfworld")
	nodes, err := adc.nodeLister.List(labels.Everything())
	if err != nil {
		return err
	}

	for _, node := range nodes {
		nodeName := types.NodeName(node.Name)

		for _, attachedVolume := range node.Status.VolumesAttached {
			uniqueName := attachedVolume.Name
			// The nil VolumeSpec is safe only in the case the volume is not in use by any pod.
			// In such a case it should be detached in the first reconciliation cycle and the
			// volume spec is not needed to detach a volume. If the volume is used by a pod, it
			// its spec can be: this would happen during in the populateDesiredStateOfWorld which
			// scans the pods and updates their volumes in the ActualStateOfWorld too.
			err = adc.actualStateOfWorld.MarkVolumeAsAttached(logger, uniqueName, nil /* VolumeSpec */, nodeName, attachedVolume.DevicePath)
			if err != nil {
				logger.Error(err, "Failed to mark the volume as attached")
				continue
			}
		}
		adc.actualStateOfWorld.SetVolumesMountedByNode(logger, node.Status.VolumesInUse, nodeName)
		adc.addNodeToDswp(node, types.NodeName(node.Name))
	}
	err = adc.processVolumeAttachments(logger)
	if err != nil {
		logger.Error(err, "Failed to process volume attachments")
	}
	return err
}

func (adc *attachDetachController) getNodeVolumeDevicePath(
	volumeName v1.UniqueVolumeName, nodeName types.NodeName) (string, error) {
	var devicePath string
	var found bool
	node, err := adc.nodeLister.Get(string(nodeName))
	if err != nil {
		return devicePath, err
	}
	for _, attachedVolume := range node.Status.VolumesAttached {
		if volumeName == attachedVolume.Name {
			devicePath = attachedVolume.DevicePath
			found = true
			break
		}
	}
	if !found {
		err = fmt.Errorf("Volume %s not found on node %s", volumeName, nodeName)
	}

	return devicePath, err
}

func (adc *attachDetachController) populateDesiredStateOfWorld(logger klog.Logger) error {
	logger.V(5).Info("Populating DesiredStateOfworld")

	pods, err := adc.podLister.List(labels.Everything())
	if err != nil {
		return err
	}
	for _, pod := range pods {
		podToAdd := pod
		adc.podAdd(logger, podToAdd)
		for _, podVolume := range podToAdd.Spec.Volumes {
			nodeName := types.NodeName(podToAdd.Spec.NodeName)
			// The volume specs present in the ActualStateOfWorld are nil, let's replace those
			// with the correct ones found on pods. The present in the ASW with no corresponding
			// pod will be detached and the spec is irrelevant.
			volumeSpec, err := util.CreateVolumeSpecWithNodeMigration(logger, podVolume, podToAdd, nodeName, &adc.volumePluginMgr, adc.pvcLister, adc.pvLister, adc.csiMigratedPluginManager, adc.intreeToCSITranslator)
			if err != nil {
				logger.Error(
					err,
					"Error creating spec for volume of pod",
					"pod", klog.KObj(podToAdd),
					"volumeName", podVolume.Name)
				continue
			}
			plugin, err := adc.volumePluginMgr.FindAttachablePluginBySpec(volumeSpec)
			if err != nil || plugin == nil {
				logger.V(10).Info(
					"Skipping volume for pod: it does not implement attacher interface",
					"pod", klog.KObj(podToAdd),
					"volumeName", podVolume.Name,
					"err", err)
				continue
			}
			volumeName, err := volumeutil.GetUniqueVolumeNameFromSpec(plugin, volumeSpec)
			if err != nil {
				logger.Error(
					err,
					"Failed to find unique name for volume of pod",
					"pod", klog.KObj(podToAdd),
					"volumeName", podVolume.Name)
				continue
			}
			attachState := adc.actualStateOfWorld.GetAttachState(volumeName, nodeName)
			if attachState == cache.AttachStateAttached {
				logger.V(10).Info("Volume is attached to node. Marking as attached in ActualStateOfWorld",
					"node", klog.KRef("", string(nodeName)),
					"volumeName", volumeName)
				devicePath, err := adc.getNodeVolumeDevicePath(volumeName, nodeName)
				if err != nil {
					logger.Error(err, "Failed to find device path")
					continue
				}
				err = adc.actualStateOfWorld.MarkVolumeAsAttached(logger, volumeName, volumeSpec, nodeName, devicePath)
				if err != nil {
					logger.Error(err, "Failed to update volume spec for node", "node", klog.KRef("", string(nodeName)))
				}
			}
		}
	}

	return nil
}

func (adc *attachDetachController) podAdd(logger klog.Logger, obj interface{}) {
	pod, ok := obj.(*v1.Pod)
	if pod == nil || !ok {
		return
	}
	if pod.Spec.NodeName == "" {
		// Ignore pods without NodeName, indicating they are not scheduled.
		return
	}

	volumeActionFlag := util.DetermineVolumeAction(
		pod,
		adc.desiredStateOfWorld,
		true /* default volume action */)

	util.ProcessPodVolumes(logger, pod, volumeActionFlag, /* addVolumes */
		adc.desiredStateOfWorld, &adc.volumePluginMgr, adc.pvcLister, adc.pvLister, adc.csiMigratedPluginManager, adc.intreeToCSITranslator)
}

// GetDesiredStateOfWorld returns desired state of world associated with controller
func (adc *attachDetachController) GetDesiredStateOfWorld() cache.DesiredStateOfWorld {
	return adc.desiredStateOfWorld
}

func (adc *attachDetachController) podUpdate(logger klog.Logger, oldObj, newObj interface{}) {
	pod, ok := newObj.(*v1.Pod)
	if pod == nil || !ok {
		return
	}
	if pod.Spec.NodeName == "" {
		// Ignore pods without NodeName, indicating they are not scheduled.
		return
	}

	volumeActionFlag := util.DetermineVolumeAction(
		pod,
		adc.desiredStateOfWorld,
		true /* default volume action */)

	util.ProcessPodVolumes(logger, pod, volumeActionFlag, /* addVolumes */
		adc.desiredStateOfWorld, &adc.volumePluginMgr, adc.pvcLister, adc.pvLister, adc.csiMigratedPluginManager, adc.intreeToCSITranslator)
}

func (adc *attachDetachController) podDelete(logger klog.Logger, obj interface{}) {
	pod, ok := obj.(*v1.Pod)
	if pod == nil || !ok {
		return
	}

	util.ProcessPodVolumes(logger, pod, false, /* addVolumes */
		adc.desiredStateOfWorld, &adc.volumePluginMgr, adc.pvcLister, adc.pvLister, adc.csiMigratedPluginManager, adc.intreeToCSITranslator)
}

func (adc *attachDetachController) nodeAdd(logger klog.Logger, obj interface{}) {
	node, ok := obj.(*v1.Node)
	// TODO: investigate if nodeName is empty then if we can return
	// kubernetes/kubernetes/issues/37777
	if node == nil || !ok {
		return
	}
	nodeName := types.NodeName(node.Name)
	adc.nodeUpdate(logger, nil, obj)
	// kubernetes/kubernetes/issues/37586
	// This is to workaround the case when a node add causes to wipe out
	// the attached volumes field. This function ensures that we sync with
	// the actual status.
	adc.actualStateOfWorld.SetNodeStatusUpdateNeeded(logger, nodeName)
}

func (adc *attachDetachController) nodeUpdate(logger klog.Logger, oldObj, newObj interface{}) {
	node, ok := newObj.(*v1.Node)
	// TODO: investigate if nodeName is empty then if we can return
	if node == nil || !ok {
		return
	}

	nodeName := types.NodeName(node.Name)
	adc.addNodeToDswp(node, nodeName)
	adc.processVolumesInUse(logger, nodeName, node.Status.VolumesInUse)
}

func (adc *attachDetachController) nodeDelete(logger klog.Logger, obj interface{}) {
	node, ok := obj.(*v1.Node)
	if node == nil || !ok {
		return
	}

	nodeName := types.NodeName(node.Name)
	if err := adc.desiredStateOfWorld.DeleteNode(nodeName); err != nil {
		// This might happen during drain, but we still want it to appear in our logs
		logger.Info("Error removing node from desired-state-of-world", "node", klog.KObj(node), "err", err)
	}

	adc.processVolumesInUse(logger, nodeName, node.Status.VolumesInUse)
}

func (adc *attachDetachController) enqueuePVC(obj interface{}) {
	key, err := kcache.DeletionHandlingMetaNamespaceKeyFunc(obj)
	if err != nil {
		runtime.HandleError(fmt.Errorf("Couldn't get key for object %+v: %v", obj, err))
		return
	}
	adc.pvcQueue.Add(key)
}

// pvcWorker processes items from pvcQueue
func (adc *attachDetachController) pvcWorker(ctx context.Context) {
	for adc.processNextItem(klog.FromContext(ctx)) {
	}
}

func (adc *attachDetachController) processNextItem(logger klog.Logger) bool {
	keyObj, shutdown := adc.pvcQueue.Get()
	if shutdown {
		return false
	}
	defer adc.pvcQueue.Done(keyObj)

	if err := adc.syncPVCByKey(logger, keyObj); err != nil {
		// Rather than wait for a full resync, re-add the key to the
		// queue to be processed.
		adc.pvcQueue.AddRateLimited(keyObj)
		runtime.HandleError(fmt.Errorf("failed to sync pvc %q, will retry again: %w", keyObj, err))
		return true
	}

	// Finally, if no error occurs we Forget this item so it does not
	// get queued again until another change happens.
	adc.pvcQueue.Forget(keyObj)
	return true
}

func (adc *attachDetachController) syncPVCByKey(logger klog.Logger, key string) error {
	logger.V(5).Info("syncPVCByKey", "pvcKey", key)
	namespace, name, err := kcache.SplitMetaNamespaceKey(key)
	if err != nil {
		logger.V(4).Info("Error getting namespace & name of pvc to get pvc from informer", "pvcKey", key, "err", err)
		return nil
	}
	pvc, err := adc.pvcLister.PersistentVolumeClaims(namespace).Get(name)
	if apierrors.IsNotFound(err) {
		logger.V(4).Info("Error getting pvc from informer", "pvcKey", key, "err", err)
		return nil
	}
	if err != nil {
		return err
	}

	if pvc.Status.Phase != v1.ClaimBound || pvc.Spec.VolumeName == "" {
		// Skip unbound PVCs.
		return nil
	}

	objs, err := adc.podIndexer.ByIndex(common.PodPVCIndex, key)
	if err != nil {
		return err
	}
	for _, obj := range objs {
		pod, ok := obj.(*v1.Pod)
		if !ok {
			continue
		}
		// we are only interested in active pods with nodeName set
		if len(pod.Spec.NodeName) == 0 || volumeutil.IsPodTerminated(pod, pod.Status) {
			continue
		}
		volumeActionFlag := util.DetermineVolumeAction(
			pod,
			adc.desiredStateOfWorld,
			true /* default volume action */)

		util.ProcessPodVolumes(logger, pod, volumeActionFlag, /* addVolumes */
			adc.desiredStateOfWorld, &adc.volumePluginMgr, adc.pvcLister, adc.pvLister, adc.csiMigratedPluginManager, adc.intreeToCSITranslator)
	}
	return nil
}

// processVolumesInUse processes the list of volumes marked as "in-use"
// according to the specified Node's Status.VolumesInUse and updates the
// corresponding volume in the actual state of the world to indicate that it is
// mounted.
func (adc *attachDetachController) processVolumesInUse(
	logger klog.Logger, nodeName types.NodeName, volumesInUse []v1.UniqueVolumeName) {
	logger.V(4).Info("processVolumesInUse for node", "node", klog.KRef("", string(nodeName)))
	adc.actualStateOfWorld.SetVolumesMountedByNode(logger, volumesInUse, nodeName)
}

// Process Volume-Attachment objects.
// Should be called only after populating attached volumes in the ASW.
// For each VA object, this function checks if its present in the ASW.
// If not, adds the volume to ASW as an "uncertain" attachment.
// In the reconciler, the logic checks if the volume is present in the DSW;
//
//	if yes, the reconciler will attempt attach on the volume;
//	if not (could be a dangling attachment), the reconciler will detach this volume.
func (adc *attachDetachController) processVolumeAttachments(logger klog.Logger) error {
	vas, err := adc.volumeAttachmentLister.List(labels.Everything())
	if err != nil {
		logger.Error(err, "Failed to list VolumeAttachment objects")
		return err
	}
	for _, va := range vas {
		nodeName := types.NodeName(va.Spec.NodeName)
		pvName := va.Spec.Source.PersistentVolumeName
		if pvName == nil {
			// Currently VA objects are created for CSI volumes only. nil pvName is unexpected, generate a warning
			logger.Info("Skipping the va as its pvName is nil", "node", klog.KRef("", string(nodeName)), "vaName", va.Name)
			continue
		}
		pv, err := adc.pvLister.Get(*pvName)
		if err != nil {
			logger.Error(err, "Unable to lookup pv object", "PV", klog.KRef("", *pvName))
			continue
		}

		var plugin volume.AttachableVolumePlugin
		volumeSpec := volume.NewSpecFromPersistentVolume(pv, false)

		// Consult csiMigratedPluginManager first before querying the plugins registered during runtime in volumePluginMgr.
		// In-tree plugins that provisioned PVs will not be registered anymore after migration to CSI, once the respective
		// feature gate is enabled.
		if inTreePluginName, err := adc.csiMigratedPluginManager.GetInTreePluginNameFromSpec(pv, nil); err == nil {
			if adc.csiMigratedPluginManager.IsMigrationEnabledForPlugin(inTreePluginName) {
				// PV is migrated and should be handled by the CSI plugin instead of the in-tree one
				plugin, _ = adc.volumePluginMgr.FindAttachablePluginByName(csi.CSIPluginName)
				// podNamespace is not needed here for Azurefile as the volumeName generated will be the same with or without podNamespace
				volumeSpec, err = csimigration.TranslateInTreeSpecToCSI(logger, volumeSpec, "" /* podNamespace */, adc.intreeToCSITranslator)
				if err != nil {
					logger.Error(err, "Failed to translate intree volumeSpec to CSI volumeSpec for volume", "node", klog.KRef("", string(nodeName)), "inTreePluginName", inTreePluginName, "vaName", va.Name, "PV", klog.KRef("", *pvName))
					continue
				}
			}
		}

		if plugin == nil {
			plugin, err = adc.volumePluginMgr.FindAttachablePluginBySpec(volumeSpec)
			if err != nil || plugin == nil {
				// Currently VA objects are created for CSI volumes only. nil plugin is unexpected, generate a warning
				logger.Info("Skipping processing the volume on node, no attacher interface found", "node", klog.KRef("", string(nodeName)), "PV", klog.KRef("", *pvName), "err", err)
				continue
			}
		}

		volumeName, err := volumeutil.GetUniqueVolumeNameFromSpec(plugin, volumeSpec)
		if err != nil {
			logger.Error(err, "Failed to find unique name for volume", "node", klog.KRef("", string(nodeName)), "vaName", va.Name, "PV", klog.KRef("", *pvName))
			continue
		}
		attachState := adc.actualStateOfWorld.GetAttachState(volumeName, nodeName)
		if attachState == cache.AttachStateDetached {
			logger.V(1).Info("Marking volume attachment as uncertain as volume is not attached", "node", klog.KRef("", string(nodeName)), "volumeName", volumeName, "attachState", attachState)
			err = adc.actualStateOfWorld.MarkVolumeAsUncertain(logger, volumeName, volumeSpec, nodeName)
			if err != nil {
				logger.Error(err, "MarkVolumeAsUncertain fail to add the volume to ASW", "node", klog.KRef("", string(nodeName)), "volumeName", volumeName)
			}
		}
	}
	return nil
}

var _ volume.VolumeHost = &attachDetachController{}
var _ volume.AttachDetachVolumeHost = &attachDetachController{}

func (adc *attachDetachController) CSINodeLister() storagelistersv1.CSINodeLister {
	return adc.csiNodeLister
}

func (adc *attachDetachController) CSIDriverLister() storagelistersv1.CSIDriverLister {
	return adc.csiDriverLister
}

func (adc *attachDetachController) IsAttachDetachController() bool {
	return true
}

func (adc *attachDetachController) VolumeAttachmentLister() storagelistersv1.VolumeAttachmentLister {
	return adc.volumeAttachmentLister
}

// VolumeHost implementation
// This is an unfortunate requirement of the current factoring of volume plugin
// initializing code. It requires kubelet specific methods used by the mounting
// code to be implemented by all initializers even if the initializer does not
// do mounting (like this attach/detach controller).
// Issue kubernetes/kubernetes/issues/14217 to fix this.
func (adc *attachDetachController) GetPluginDir(podUID string) string {
	return ""
}

func (adc *attachDetachController) GetVolumeDevicePluginDir(podUID string) string {
	return ""
}

func (adc *attachDetachController) GetPodsDir() string {
	return ""
}

func (adc *attachDetachController) GetPodVolumeDir(podUID types.UID, pluginName, volumeName string) string {
	return ""
}

func (adc *attachDetachController) GetPodPluginDir(podUID types.UID, pluginName string) string {
	return ""
}

func (adc *attachDetachController) GetPodVolumeDeviceDir(podUID types.UID, pluginName string) string {
	return ""
}

func (adc *attachDetachController) GetKubeClient() clientset.Interface {
	return adc.kubeClient
}

func (adc *attachDetachController) NewWrapperMounter(volName string, spec volume.Spec, pod *v1.Pod) (volume.Mounter, error) {
	return nil, fmt.Errorf("NewWrapperMounter not supported by Attach/Detach controller's VolumeHost implementation")
}

func (adc *attachDetachController) NewWrapperUnmounter(volName string, spec volume.Spec, podUID types.UID) (volume.Unmounter, error) {
	return nil, fmt.Errorf("NewWrapperUnmounter not supported by Attach/Detach controller's VolumeHost implementation")
}

func (adc *attachDetachController) GetMounter(pluginName string) mount.Interface {
	return nil
}

func (adc *attachDetachController) GetHostName() string {
	return ""
}

func (adc *attachDetachController) GetHostIP() (net.IP, error) {
	return nil, fmt.Errorf("GetHostIP() not supported by Attach/Detach controller's VolumeHost implementation")
}

func (adc *attachDetachController) GetNodeAllocatable() (v1.ResourceList, error) {
	return v1.ResourceList{}, nil
}

func (adc *attachDetachController) GetAttachedVolumesFromNodeStatus() (map[v1.UniqueVolumeName]string, error) {
	return map[v1.UniqueVolumeName]string{}, nil
}

func (adc *attachDetachController) GetSecretFunc() func(namespace, name string) (*v1.Secret, error) {
	return func(_, _ string) (*v1.Secret, error) {
		return nil, fmt.Errorf("GetSecret unsupported in attachDetachController")
	}
}

func (adc *attachDetachController) GetConfigMapFunc() func(namespace, name string) (*v1.ConfigMap, error) {
	return func(_, _ string) (*v1.ConfigMap, error) {
		return nil, fmt.Errorf("GetConfigMap unsupported in attachDetachController")
	}
}

func (adc *attachDetachController) GetServiceAccountTokenFunc() func(_, _ string, _ *authenticationv1.TokenRequest) (*authenticationv1.TokenRequest, error) {
	return func(_, _ string, _ *authenticationv1.TokenRequest) (*authenticationv1.TokenRequest, error) {
		return nil, fmt.Errorf("GetServiceAccountToken unsupported in attachDetachController")
	}
}

func (adc *attachDetachController) DeleteServiceAccountTokenFunc() func(types.UID) {
	return func(types.UID) {
		// nolint:logcheck
		klog.ErrorS(nil, "DeleteServiceAccountToken unsupported in attachDetachController")
	}
}

func (adc *attachDetachController) GetExec(pluginName string) utilexec.Interface {
	return utilexec.New()
}

func (adc *attachDetachController) addNodeToDswp(node *v1.Node, nodeName types.NodeName) {
	if _, exists := node.Annotations[volumeutil.ControllerManagedAttachAnnotation]; exists {
		// Node specifies annotation indicating it should be managed by attach
		// detach controller. Add it to desired state of world.
		adc.desiredStateOfWorld.AddNode(nodeName)
	}
}

func (adc *attachDetachController) GetNodeLabels() (map[string]string, error) {
	return nil, fmt.Errorf("GetNodeLabels() unsupported in Attach/Detach controller")
}

func (adc *attachDetachController) GetNodeName() types.NodeName {
	return ""
}

func (adc *attachDetachController) GetEventRecorder() events.EventRecorder {
	return nil
}

func (adc *attachDetachController) GetSubpather() subpath.Interface {
	// Subpaths not needed in attachdetach controller
	return nil
}

func (adc *attachDetachController) GetCSIDriverLister() storagelistersv1.CSIDriverLister {
	return adc.csiDriverLister
}
