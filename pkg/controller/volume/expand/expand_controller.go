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

package expand

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"k8s.io/klog/v2"
	"k8s.io/mount-utils"
	utilexec "k8s.io/utils/exec"

	authenticationv1 "k8s.io/api/authentication/v1"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	coreinformers "k8s.io/client-go/informers/core/v1"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	toolsevents "k8s.io/client-go/tools/events"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/kubernetes/pkg/controller/volume/events"
	"k8s.io/kubernetes/pkg/features"
	"k8s.io/kubernetes/pkg/volume"
	"k8s.io/kubernetes/pkg/volume/csimigration"
	"k8s.io/kubernetes/pkg/volume/util"
	"k8s.io/kubernetes/pkg/volume/util/operationexecutor"
	"k8s.io/kubernetes/pkg/volume/util/subpath"
	volumetypes "k8s.io/kubernetes/pkg/volume/util/types"
	"k8s.io/kubernetes/pkg/volume/util/volumepathhandler"
)

const (
	// number of default volume expansion workers
	defaultWorkerCount = 10
)

// ExpandController expands the pvs
type ExpandController interface {
	Run(ctx context.Context)
}

// CSINameTranslator can get the CSI Driver name based on the in-tree plugin name
type CSINameTranslator interface {
	GetCSINameFromInTreeName(pluginName string) (string, error)
}

// Deprecated: This controller is deprecated and for now exists for the sole purpose of adding
// necessary annotations if necessary, so as volume can be expanded externally in the control-plane
type expandController struct {
	// kubeClient is the kube API client used by volumehost to communicate with
	// the API server.
	kubeClient clientset.Interface

	// pvcLister is the shared PVC lister used to fetch and store PVC
	// objects from the API server. It is shared with other controllers and
	// therefore the PVC objects in its store should be treated as immutable.
	pvcLister  corelisters.PersistentVolumeClaimLister
	pvcsSynced cache.InformerSynced

	// volumePluginMgr used to initialize and fetch volume plugins
	volumePluginMgr volume.VolumePluginMgr

	// recorder is used to record events in the API server
	recorder toolsevents.EventRecorder
	// broadcaster is broadcasting events
	broadcaster        toolsevents.EventBroadcaster
	operationGenerator operationexecutor.OperationGenerator

	queue workqueue.TypedRateLimitingInterface[string]

	translator CSINameTranslator

	csiMigratedPluginManager csimigration.PluginManager
}

// NewExpandController expands the pvs
func NewExpandController(
	ctx context.Context,
	kubeClient clientset.Interface,
	pvcInformer coreinformers.PersistentVolumeClaimInformer,
	plugins []volume.VolumePlugin,
	translator CSINameTranslator,
	csiMigratedPluginManager csimigration.PluginManager) (ExpandController, error) {

	expc := &expandController{
		kubeClient: kubeClient,
		pvcLister:  pvcInformer.Lister(),
		pvcsSynced: pvcInformer.Informer().HasSynced,
		queue: workqueue.NewTypedRateLimitingQueueWithConfig(
			workqueue.DefaultTypedControllerRateLimiter[string](),
			workqueue.TypedRateLimitingQueueConfig[string]{Name: "volume_expand"},
		),
		translator:               translator,
		csiMigratedPluginManager: csiMigratedPluginManager,
	}

	if err := expc.volumePluginMgr.InitPlugins(plugins, nil, expc); err != nil {
		return nil, fmt.Errorf("could not initialize volume plugins for Expand Controller : %+v", err)
	}

	expc.broadcaster = toolsevents.NewBroadcaster(&toolsevents.EventSinkImpl{Interface: kubeClient.EventsV1()})
	expc.broadcaster.StartStructuredLogging(3)
	expc.recorder = expc.broadcaster.NewRecorder(scheme.Scheme, "volume_expand")

	blkutil := volumepathhandler.NewBlockVolumePathHandler()

	expc.operationGenerator = operationexecutor.NewOperationGenerator(
		kubeClient,
		&expc.volumePluginMgr,
		expc.recorder,
		blkutil)

	pvcInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: expc.enqueuePVC,
		UpdateFunc: func(old, new interface{}) {
			oldPVC, ok := old.(*v1.PersistentVolumeClaim)
			if !ok {
				return
			}

			oldReq := oldPVC.Spec.Resources.Requests[v1.ResourceStorage]
			oldCap := oldPVC.Status.Capacity[v1.ResourceStorage]
			newPVC, ok := new.(*v1.PersistentVolumeClaim)
			if !ok {
				return
			}
			newReq := newPVC.Spec.Resources.Requests[v1.ResourceStorage]
			newCap := newPVC.Status.Capacity[v1.ResourceStorage]
			// PVC will be enqueued under 2 circumstances
			// 1. User has increased PVC's request capacity --> volume needs to be expanded
			// 2. PVC status capacity has been expanded --> claim's bound PV has likely recently gone through filesystem resize, so remove AnnPreResizeCapacity annotation from PV
			if newReq.Cmp(oldReq) > 0 || newCap.Cmp(oldCap) > 0 {
				expc.enqueuePVC(new)
			}
		},
		DeleteFunc: expc.enqueuePVC,
	})

	return expc, nil
}

func (expc *expandController) enqueuePVC(obj interface{}) {
	pvc, ok := obj.(*v1.PersistentVolumeClaim)
	if !ok {
		return
	}

	if pvc.Status.Phase == v1.ClaimBound {
		key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(pvc)
		if err != nil {
			runtime.HandleError(fmt.Errorf("couldn't get key for object %#v: %v", pvc, err))
			return
		}
		expc.queue.Add(key)
	}
}

func (expc *expandController) processNextWorkItem(ctx context.Context) bool {
	key, shutdown := expc.queue.Get()
	if shutdown {
		return false
	}
	defer expc.queue.Done(key)

	err := expc.syncHandler(ctx, key)
	if err == nil {
		expc.queue.Forget(key)
		return true
	}

	runtime.HandleError(fmt.Errorf("%v failed with : %v", key, err))
	expc.queue.AddRateLimited(key)

	return true
}

// syncHandler performs actual expansion of volume. If an error is returned
// from this function - PVC will be requeued for resizing.
func (expc *expandController) syncHandler(ctx context.Context, key string) error {
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}
	pvc, err := expc.pvcLister.PersistentVolumeClaims(namespace).Get(name)
	if apierrors.IsNotFound(err) {
		return nil
	}
	logger := klog.FromContext(ctx)
	if err != nil {
		logger.V(5).Info("Error getting PVC from informer", "pvcKey", key, "err", err)
		return err
	}

	pv, err := expc.getPersistentVolume(ctx, pvc)
	if err != nil {
		logger.V(5).Info("Error getting Persistent Volume for PVC from informer", "pvcKey", key, "pvcUID", pvc.UID, "err", err)
		return err
	}

	if pv.Spec.ClaimRef == nil || pvc.Namespace != pv.Spec.ClaimRef.Namespace || pvc.UID != pv.Spec.ClaimRef.UID {
		err := fmt.Errorf("persistent Volume is not bound to PVC being updated : %s", key)
		logger.V(4).Info("", "err", err)
		return err
	}

	pvcRequestSize := pvc.Spec.Resources.Requests[v1.ResourceStorage]
	pvcStatusSize := pvc.Status.Capacity[v1.ResourceStorage]

	// call expand operation only under two condition
	// 1. pvc's request size has been expanded and is larger than pvc's current status size
	// 2. pv has an pre-resize capacity annotation
	if pvcRequestSize.Cmp(pvcStatusSize) <= 0 && !metav1.HasAnnotation(pv.ObjectMeta, util.AnnPreResizeCapacity) {
		return nil
	}

	volumeSpec := volume.NewSpecFromPersistentVolume(pv, false)
	migratable, err := expc.csiMigratedPluginManager.IsMigratable(volumeSpec)
	if err != nil {
		logger.V(4).Info("Failed to check CSI migration status for PVC with error", "pvcKey", key, "err", err)
		return nil
	}
	// handle CSI migration scenarios before invoking FindExpandablePluginBySpec for in-tree
	if migratable {
		inTreePluginName, err := expc.csiMigratedPluginManager.GetInTreePluginNameFromSpec(volumeSpec.PersistentVolume, volumeSpec.Volume)
		if err != nil {
			logger.V(4).Info("Error getting in-tree plugin name from persistent volume", "volumeName", volumeSpec.PersistentVolume.Name, "err", err)
			return err
		}

		msg := fmt.Sprintf("CSI migration enabled for %s; waiting for external resizer to expand the pvc", inTreePluginName)
		expc.recorder.Eventf(pvc, nil, v1.EventTypeNormal, events.ExternalExpanding, "ExpandingPVC", msg)
		csiResizerName, err := expc.translator.GetCSINameFromInTreeName(inTreePluginName)
		if err != nil {
			errorMsg := fmt.Sprintf("error getting CSI driver name for pvc %s, with error %v", key, err)
			expc.recorder.Eventf(pvc, nil, v1.EventTypeWarning, events.ExternalExpanding, "ExpandingPVC", errorMsg)
			return errors.New(errorMsg)
		}

		pvc, err := util.SetClaimResizer(pvc, csiResizerName, expc.kubeClient)
		if err != nil {
			errorMsg := fmt.Sprintf("error setting resizer annotation to pvc %s, with error %v", key, err)
			expc.recorder.Eventf(pvc, nil, v1.EventTypeWarning, events.ExternalExpanding, "ExpandingPVC", errorMsg)
			return errors.New(errorMsg)
		}
		return nil
	}

	volumePlugin, err := expc.volumePluginMgr.FindExpandablePluginBySpec(volumeSpec)
	if err != nil || volumePlugin == nil {
		msg := "waiting for an external controller to expand this PVC"
		eventType := v1.EventTypeNormal
		if err != nil {
			eventType = v1.EventTypeWarning
		}
		expc.recorder.Eventf(pvc, nil, eventType, events.ExternalExpanding, "ExpandingPVC", msg)
		logger.Info("Waiting for an external controller to expand the PVC", "pvcKey", key, "pvcUID", pvc.UID)
		// If we are expecting that an external plugin will handle resizing this volume then
		// is no point in requeuing this PVC.
		return nil
	}

	volumeResizerName := volumePlugin.GetPluginName()
	return expc.expand(logger, pvc, pv, volumeResizerName)
}

func (expc *expandController) expand(logger klog.Logger, pvc *v1.PersistentVolumeClaim, pv *v1.PersistentVolume, resizerName string) error {
	// if node expand is complete and pv's annotation can be removed, remove the annotation from pv and return
	if expc.isNodeExpandComplete(logger, pvc, pv) && metav1.HasAnnotation(pv.ObjectMeta, util.AnnPreResizeCapacity) {
		return util.DeleteAnnPreResizeCapacity(pv, expc.GetKubeClient())
	}

	var generatedOptions volumetypes.GeneratedOperations
	var err error
	if utilfeature.DefaultFeatureGate.Enabled(features.RecoverVolumeExpansionFailure) {
		generatedOptions, err = expc.operationGenerator.GenerateExpandAndRecoverVolumeFunc(pvc, pv, resizerName)
		if err != nil {
			logger.Error(err, "Error starting ExpandVolume for pvc", "PVC", klog.KObj(pvc))
			return err
		}
	} else {
		pvc, err := util.MarkResizeInProgressWithResizer(pvc, resizerName, expc.kubeClient)
		if err != nil {
			logger.Error(err, "Error setting PVC in progress with error", "PVC", klog.KObj(pvc), "err", err)
			return err
		}

		generatedOptions, err = expc.operationGenerator.GenerateExpandVolumeFunc(pvc, pv)
		if err != nil {
			logger.Error(err, "Error starting ExpandVolume for pvc with error", "PVC", klog.KObj(pvc), "err", err)
			return err
		}
	}

	logger.V(5).Info("Starting ExpandVolume for volume", "volumeName", util.GetPersistentVolumeClaimQualifiedName(pvc))
	_, detailedErr := generatedOptions.Run()

	return detailedErr
}

// TODO make concurrency configurable (workers argument). previously, nestedpendingoperations spawned unlimited goroutines
func (expc *expandController) Run(ctx context.Context) {
	defer runtime.HandleCrash()
	defer expc.queue.ShutDown()

	// Start events processing pipeline.
	expc.broadcaster.StartStructuredLogging(0)
	expc.broadcaster.StartRecordingToSink(ctx.Done())
	defer expc.broadcaster.Shutdown()

	logger := klog.FromContext(ctx)
	logger.Info("Starting expand controller")
	defer logger.Info("Shutting down expand controller")

	if !cache.WaitForNamedCacheSync("expand", ctx.Done(), expc.pvcsSynced) {
		return
	}

	for i := 0; i < defaultWorkerCount; i++ {
		go wait.UntilWithContext(ctx, expc.runWorker, time.Second)
	}

	<-ctx.Done()
}

func (expc *expandController) runWorker(ctx context.Context) {
	for expc.processNextWorkItem(ctx) {
	}
}

func (expc *expandController) getPersistentVolume(ctx context.Context, pvc *v1.PersistentVolumeClaim) (*v1.PersistentVolume, error) {
	volumeName := pvc.Spec.VolumeName
	pv, err := expc.kubeClient.CoreV1().PersistentVolumes().Get(ctx, volumeName, metav1.GetOptions{})

	if err != nil {
		return nil, fmt.Errorf("failed to get PV %q: %v", volumeName, err)
	}

	return pv, nil
}

// isNodeExpandComplete returns true if  pvc.Status.Capacity >= pv.Spec.Capacity
func (expc *expandController) isNodeExpandComplete(logger klog.Logger, pvc *v1.PersistentVolumeClaim, pv *v1.PersistentVolume) bool {
	logger.V(4).Info("pv and pvc capacity", "PV", klog.KObj(pv), "pvCapacity", pv.Spec.Capacity[v1.ResourceStorage], "PVC", klog.KObj(pvc), "pvcCapacity", pvc.Status.Capacity[v1.ResourceStorage])
	pvcSpecCap := pvc.Spec.Resources.Requests.Storage()
	pvcStatusCap, pvCap := pvc.Status.Capacity[v1.ResourceStorage], pv.Spec.Capacity[v1.ResourceStorage]

	// since we allow shrinking volumes, we must compare both pvc status and capacity
	// with pv spec capacity.
	if pvcStatusCap.Cmp(*pvcSpecCap) >= 0 && pvcStatusCap.Cmp(pvCap) >= 0 {
		return true
	}
	return false
}

// Implementing VolumeHost interface
func (expc *expandController) GetPluginDir(pluginName string) string {
	return ""
}

func (expc *expandController) GetVolumeDevicePluginDir(pluginName string) string {
	return ""
}

func (expc *expandController) GetPodsDir() string {
	return ""
}

func (expc *expandController) GetPodVolumeDir(podUID types.UID, pluginName string, volumeName string) string {
	return ""
}

func (expc *expandController) GetPodVolumeDeviceDir(podUID types.UID, pluginName string) string {
	return ""
}

func (expc *expandController) GetPodPluginDir(podUID types.UID, pluginName string) string {
	return ""
}

func (expc *expandController) GetKubeClient() clientset.Interface {
	return expc.kubeClient
}

func (expc *expandController) NewWrapperMounter(volName string, spec volume.Spec, pod *v1.Pod) (volume.Mounter, error) {
	return nil, fmt.Errorf("NewWrapperMounter not supported by expand controller's VolumeHost implementation")
}

func (expc *expandController) NewWrapperUnmounter(volName string, spec volume.Spec, podUID types.UID) (volume.Unmounter, error) {
	return nil, fmt.Errorf("NewWrapperUnmounter not supported by expand controller's VolumeHost implementation")
}

func (expc *expandController) GetMounter(pluginName string) mount.Interface {
	return nil
}

func (expc *expandController) GetExec(pluginName string) utilexec.Interface {
	return utilexec.New()
}

func (expc *expandController) GetHostName() string {
	return ""
}

func (expc *expandController) GetHostIP() (net.IP, error) {
	return nil, fmt.Errorf("GetHostIP not supported by expand controller's VolumeHost implementation")
}

func (expc *expandController) GetNodeAllocatable() (v1.ResourceList, error) {
	return v1.ResourceList{}, nil
}

func (expc *expandController) GetSecretFunc() func(namespace, name string) (*v1.Secret, error) {
	return func(_, _ string) (*v1.Secret, error) {
		return nil, fmt.Errorf("GetSecret unsupported in expandController")
	}
}

func (expc *expandController) GetConfigMapFunc() func(namespace, name string) (*v1.ConfigMap, error) {
	return func(_, _ string) (*v1.ConfigMap, error) {
		return nil, fmt.Errorf("GetConfigMap unsupported in expandController")
	}
}

func (expc *expandController) GetAttachedVolumesFromNodeStatus() (map[v1.UniqueVolumeName]string, error) {
	return map[v1.UniqueVolumeName]string{}, nil
}

func (expc *expandController) GetServiceAccountTokenFunc() func(_, _ string, _ *authenticationv1.TokenRequest) (*authenticationv1.TokenRequest, error) {
	return func(_, _ string, _ *authenticationv1.TokenRequest) (*authenticationv1.TokenRequest, error) {
		return nil, fmt.Errorf("GetServiceAccountToken unsupported in expandController")
	}
}

func (expc *expandController) DeleteServiceAccountTokenFunc() func(types.UID) {
	return func(types.UID) {
		//nolint:logcheck
		klog.ErrorS(nil, "DeleteServiceAccountToken unsupported in expandController")
	}
}

func (expc *expandController) GetNodeLabels() (map[string]string, error) {
	return nil, fmt.Errorf("GetNodeLabels unsupported in expandController")
}

func (expc *expandController) GetNodeName() types.NodeName {
	return ""
}

func (expc *expandController) GetEventRecorder() toolsevents.EventRecorder {
	return expc.recorder
}

func (expc *expandController) GetSubpather() subpath.Interface {
	// not needed for expand controller
	return nil
}
