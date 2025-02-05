/*
Copyright 2024 The Kubernetes Authors.

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

package storageversionmigrator

import (
	"context"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/metadata"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/controller"

	svmv1alpha1 "k8s.io/api/storagemigration/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	svminformers "k8s.io/client-go/informers/storagemigration/v1alpha1"
	clientset "k8s.io/client-go/kubernetes"
	svmlisters "k8s.io/client-go/listers/storagemigration/v1alpha1"
)

const (
	// this name is guaranteed to be not present in the cluster as it not a valid namespace name
	fakeSVMNamespaceName          string = "@fake:svm_ns!"
	ResourceVersionControllerName string = "resource-version-controller"
)

// ResourceVersionController adds the resource version obtained from a randomly nonexistent namespace
// to the SVM status before the migration is initiated. This resource version is utilized for checking
// freshness of GC cache before the migration is initiated.
type ResourceVersionController struct {
	discoveryClient *discovery.DiscoveryClient
	metadataClient  metadata.Interface
	svmListers      svmlisters.StorageVersionMigrationLister
	svmSynced       cache.InformerSynced
	queue           workqueue.TypedRateLimitingInterface[string]
	kubeClient      clientset.Interface
	mapper          meta.ResettableRESTMapper
}

func NewResourceVersionController(
	ctx context.Context,
	kubeClient clientset.Interface,
	discoveryClient *discovery.DiscoveryClient,
	metadataClient metadata.Interface,
	svmInformer svminformers.StorageVersionMigrationInformer,
	mapper meta.ResettableRESTMapper,
) *ResourceVersionController {
	logger := klog.FromContext(ctx)

	rvController := &ResourceVersionController{
		kubeClient:      kubeClient,
		discoveryClient: discoveryClient,
		metadataClient:  metadataClient,
		svmListers:      svmInformer.Lister(),
		svmSynced:       svmInformer.Informer().HasSynced,
		mapper:          mapper,
		queue: workqueue.NewTypedRateLimitingQueueWithConfig(
			workqueue.DefaultTypedControllerRateLimiter[string](),
			workqueue.TypedRateLimitingQueueConfig[string]{Name: ResourceVersionControllerName},
		),
	}

	_, _ = svmInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			rvController.addSVM(logger, obj)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			rvController.updateSVM(logger, oldObj, newObj)
		},
	})

	return rvController
}

func (rv *ResourceVersionController) addSVM(logger klog.Logger, obj interface{}) {
	svm := obj.(*svmv1alpha1.StorageVersionMigration)
	logger.V(4).Info("Adding", "svm", klog.KObj(svm))
	rv.enqueue(svm)
}

func (rv *ResourceVersionController) updateSVM(logger klog.Logger, oldObj, newObj interface{}) {
	oldSVM := oldObj.(*svmv1alpha1.StorageVersionMigration)
	newSVM := newObj.(*svmv1alpha1.StorageVersionMigration)
	logger.V(4).Info("Updating", "svm", klog.KObj(oldSVM))
	rv.enqueue(newSVM)
}

func (rv *ResourceVersionController) enqueue(svm *svmv1alpha1.StorageVersionMigration) {
	key, err := controller.KeyFunc(svm)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for object %#v: %w", svm, err))
		return
	}

	rv.queue.Add(key)
}

func (rv *ResourceVersionController) Run(ctx context.Context) {
	defer utilruntime.HandleCrash()
	defer rv.queue.ShutDown()

	logger := klog.FromContext(ctx)
	logger.Info("Starting", "controller", ResourceVersionControllerName)
	defer logger.Info("Shutting down", "controller", ResourceVersionControllerName)

	if !cache.WaitForNamedCacheSync(ResourceVersionControllerName, ctx.Done(), rv.svmSynced) {
		return
	}

	go wait.UntilWithContext(ctx, rv.worker, time.Second)

	<-ctx.Done()
}

func (rv *ResourceVersionController) worker(ctx context.Context) {
	for rv.processNext(ctx) {
	}
}

func (rv *ResourceVersionController) processNext(ctx context.Context) bool {
	key, quit := rv.queue.Get()
	if quit {
		return false
	}
	defer rv.queue.Done(key)

	err := rv.sync(ctx, key)
	if err == nil {
		rv.queue.Forget(key)
		return true
	}

	klog.FromContext(ctx).V(2).Info("Error syncing SVM resource, retrying", "svm", key, "err", err)
	rv.queue.AddRateLimited(key)

	return true
}

func (rv *ResourceVersionController) sync(ctx context.Context, key string) error {
	logger := klog.FromContext(ctx)
	startTime := time.Now()

	// SVM is a cluster scoped resource so we don't care about the namespace
	_, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}

	svm, err := rv.svmListers.Get(name)
	if apierrors.IsNotFound(err) {
		// no work to do, don't fail and requeue
		return nil
	}
	if err != nil {
		return err
	}
	// working with copy to avoid race condition between this and migration controller
	toBeProcessedSVM := svm.DeepCopy()
	gvr := getGVRFromResource(toBeProcessedSVM)

	if IsConditionTrue(toBeProcessedSVM, svmv1alpha1.MigrationSucceeded) || IsConditionTrue(toBeProcessedSVM, svmv1alpha1.MigrationFailed) {
		logger.V(4).Info("Migration has already succeeded or failed previously, skipping", "svm", name)
		return nil
	}

	if len(toBeProcessedSVM.Status.ResourceVersion) != 0 {
		logger.V(4).Info("Resource version is already set", "svm", name)
		return nil
	}

	exists, err := rv.resourceExists(gvr)
	if err != nil {
		return err
	}
	if !exists {
		_, err = rv.kubeClient.StoragemigrationV1alpha1().
			StorageVersionMigrations().
			UpdateStatus(
				ctx,
				setStatusConditions(toBeProcessedSVM, svmv1alpha1.MigrationFailed, migrationFailedStatusReason, "resource does not exist in discovery"),
				metav1.UpdateOptions{},
			)
		if err != nil {
			return err
		}

		return nil
	}

	toBeProcessedSVM.Status.ResourceVersion, err = rv.getLatestResourceVersion(gvr, ctx)
	if err != nil {
		return err
	}

	_, err = rv.kubeClient.StoragemigrationV1alpha1().
		StorageVersionMigrations().
		UpdateStatus(ctx, toBeProcessedSVM, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("error updating status for %s: %w", toBeProcessedSVM.Name, err)
	}

	logger.V(4).Info("Resource version has been successfully added", "svm", key, "elapsed", time.Since(startTime))
	return nil
}

func (rv *ResourceVersionController) getLatestResourceVersion(gvr schema.GroupVersionResource, ctx context.Context) (string, error) {
	isResourceNamespaceScoped, err := rv.isResourceNamespaceScoped(gvr)
	if err != nil {
		return "", err
	}

	var randomList *metav1.PartialObjectMetadataList
	if isResourceNamespaceScoped {
		// get list resourceVersion from random non-existent namespace for the given GVR
		randomList, err = rv.metadataClient.Resource(gvr).
			Namespace(fakeSVMNamespaceName).
			List(ctx, metav1.ListOptions{
				Limit: 1,
			})
	} else {
		randomList, err = rv.metadataClient.Resource(gvr).
			List(ctx, metav1.ListOptions{
				Limit: 1,
			})
	}
	if err != nil {
		// error here is very abstract. adding additional context for better debugging
		return "", fmt.Errorf("error getting latest resourceVersion for %s: %w", gvr.String(), err)
	}

	return randomList.GetResourceVersion(), err
}

func (rv *ResourceVersionController) resourceExists(gvr schema.GroupVersionResource) (bool, error) {
	mapperGVRs, err := rv.mapper.ResourcesFor(gvr)
	if err != nil {
		return false, err
	}

	for _, mapperGVR := range mapperGVRs {
		if mapperGVR.Group == gvr.Group &&
			mapperGVR.Version == gvr.Version &&
			mapperGVR.Resource == gvr.Resource {
			return true, nil
		}
	}

	return false, nil
}

func (rv *ResourceVersionController) isResourceNamespaceScoped(gvr schema.GroupVersionResource) (bool, error) {
	resourceList, err := rv.discoveryClient.ServerResourcesForGroupVersion(gvr.GroupVersion().String())
	if err != nil {
		return false, err
	}

	for _, resource := range resourceList.APIResources {
		if resource.Name == gvr.Resource {
			return resource.Namespaced, nil
		}
	}

	return false, fmt.Errorf("resource %q not found", gvr.String())
}
