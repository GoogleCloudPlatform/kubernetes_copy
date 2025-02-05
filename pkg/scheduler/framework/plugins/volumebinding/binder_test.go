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

package volumebinding

import (
	"context"
	"fmt"
	"os"
	"sort"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	v1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/informers"
	coreinformers "k8s.io/client-go/informers/core/v1"
	storageinformers "k8s.io/client-go/informers/storage/v1"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/component-helpers/storage/volume"
	"k8s.io/klog/v2"
	"k8s.io/klog/v2/ktesting"
	_ "k8s.io/klog/v2/ktesting/init"
	"k8s.io/kubernetes/pkg/controller"
	pvtesting "k8s.io/kubernetes/pkg/controller/volume/persistentvolume/testing"
	"k8s.io/kubernetes/pkg/scheduler/util/assumecache"
)

var (
	provisioner = "test-provisioner"

	// PVCs for manual binding
	// TODO: clean up all of these
	unboundPVC          = makeTestPVC("unbound-pvc", "1G", "", pvcUnbound, "", "1", &waitClass)
	unboundPVC2         = makeTestPVC("unbound-pvc2", "5G", "", pvcUnbound, "", "1", &waitClass)
	preboundPVC         = makeTestPVC("prebound-pvc", "1G", "", pvcPrebound, "pv-node1a", "1", &waitClass)
	preboundPVCNode1a   = makeTestPVC("unbound-pvc", "1G", "", pvcPrebound, "pv-node1a", "1", &waitClass)
	boundPVC            = makeTestPVC("bound-pvc", "1G", "", pvcBound, "pv-bound", "1", &waitClass)
	boundPVCNode1a      = makeTestPVC("unbound-pvc", "1G", "", pvcBound, "pv-node1a", "1", &waitClass)
	immediateUnboundPVC = makeTestPVC("immediate-unbound-pvc", "1G", "", pvcUnbound, "", "1", &immediateClass)
	immediateBoundPVC   = makeTestPVC("immediate-bound-pvc", "1G", "", pvcBound, "pv-bound-immediate", "1", &immediateClass)

	// PVCs for dynamic provisioning
	provisionedPVC              = makeTestPVC("provisioned-pvc", "1Gi", "", pvcUnbound, "", "1", &waitClassWithProvisioner)
	provisionedPVC2             = makeTestPVC("provisioned-pvc2", "1Gi", "", pvcUnbound, "", "1", &waitClassWithProvisioner)
	provisionedPVCHigherVersion = makeTestPVC("provisioned-pvc2", "1Gi", "", pvcUnbound, "", "2", &waitClassWithProvisioner)
	provisionedPVCBound         = makeTestPVC("provisioned-pvc", "1Gi", "", pvcBound, "pv-bound", "1", &waitClassWithProvisioner)
	noProvisionerPVC            = makeTestPVC("no-provisioner-pvc", "1Gi", "", pvcUnbound, "", "1", &waitClass)
	topoMismatchPVC             = makeTestPVC("topo-mismatch-pvc", "1Gi", "", pvcUnbound, "", "1", &topoMismatchClass)

	selectedNodePVC = makeTestPVC("provisioned-pvc", "1Gi", nodeLabelValue, pvcSelectedNode, "", "1", &waitClassWithProvisioner)

	// PVCs for CSI migration
	boundMigrationPVC     = makeTestPVC("pvc-migration-bound", "1G", "", pvcBound, "pv-migration-bound", "1", &waitClass)
	provMigrationPVCBound = makeTestPVC("pvc-migration-provisioned", "1Gi", "", pvcBound, "pv-migration-bound", "1", &waitClassWithProvisioner)

	// PVCs and PV for GenericEphemeralVolume
	conflictingGenericPVC = makeGenericEphemeralPVC("test-volume", false /* not owned*/)
	correctGenericPVC     = makeGenericEphemeralPVC("test-volume", true /* owned */)
	pvBoundGeneric        = makeTestPV("pv-bound", "node1", "1G", "1", correctGenericPVC, waitClass)

	// PVs for manual binding
	pvNode1a                   = makeTestPV("pv-node1a", "node1", "5G", "1", nil, waitClass)
	pvNode1b                   = makeTestPV("pv-node1b", "node1", "10G", "1", nil, waitClass)
	pvNode1c                   = makeTestPV("pv-node1b", "node1", "5G", "1", nil, waitClass)
	pvNode2                    = makeTestPV("pv-node2", "node2", "1G", "1", nil, waitClass)
	pvBound                    = makeTestPV("pv-bound", "node1", "1G", "1", boundPVC, waitClass)
	pvNode1aBound              = makeTestPV("pv-node1a", "node1", "5G", "1", unboundPVC, waitClass)
	pvNode1bBound              = makeTestPV("pv-node1b", "node1", "10G", "1", unboundPVC2, waitClass)
	pvNode1bBoundHigherVersion = makeTestPV("pv-node1b", "node1", "10G", "2", unboundPVC2, waitClass)
	pvBoundImmediate           = makeTestPV("pv-bound-immediate", "node1", "1G", "1", immediateBoundPVC, immediateClass)
	pvBoundImmediateNode2      = makeTestPV("pv-bound-immediate", "node2", "1G", "1", immediateBoundPVC, immediateClass)

	// PVs for CSI migration
	migrationPVBound             = makeTestPVForCSIMigration(zone1Labels, boundMigrationPVC, true)
	migrationPVBoundToUnbound    = makeTestPVForCSIMigration(zone1Labels, unboundPVC, true)
	nonmigrationPVBoundToUnbound = makeTestPVForCSIMigration(zone1Labels, unboundPVC, false)

	// storage class names
	waitClass                = "waitClass"
	immediateClass           = "immediateClass"
	waitClassWithProvisioner = "waitClassWithProvisioner"
	topoMismatchClass        = "topoMismatchClass"

	// nodes objects
	node1         = makeNode("node1").withLabel(nodeLabelKey, "node1").Node
	node2         = makeNode("node2").withLabel(nodeLabelKey, "node2").Node
	node1NoLabels = makeNode("node1").Node
	node1Zone1    = makeNode("node1").withLabel("topology.gke.io/zone", "us-east-1").Node
	node1Zone2    = makeNode("node1").withLabel("topology.gke.io/zone", "us-east-2").Node

	// csiNode objects
	csiNode1Migrated    = makeCSINode("node1", "kubernetes.io/gce-pd")
	csiNode1NotMigrated = makeCSINode("node1", "")

	// node topology
	nodeLabelKey   = "nodeKey"
	nodeLabelValue = "node1"

	// node topology for CSI migration
	zone1Labels = map[string]string{v1.LabelFailureDomainBetaZone: "us-east-1", v1.LabelFailureDomainBetaRegion: "us-east-1a"}

	// csiCapacity objects
	networkAttachedCapacity = makeCapacity("net", waitClassWithProvisioner, nil, "1Gi", "")
	node1Capacity           = makeCapacity("net", waitClassWithProvisioner, node1, "1Gi", "")
	node2Capacity           = makeCapacity("net", waitClassWithProvisioner, node2, "1Gi", "")
)

type testEnv struct {
	client                  clientset.Interface
	reactor                 *pvtesting.VolumeReactor
	binder                  SchedulerVolumeBinder
	internalBinder          *volumeBinder
	internalPodInformer     coreinformers.PodInformer
	internalNodeInformer    coreinformers.NodeInformer
	internalCSINodeInformer storageinformers.CSINodeInformer

	// For CSIStorageCapacity feature testing:
	internalCSIDriverInformer          storageinformers.CSIDriverInformer
	internalCSIStorageCapacityInformer storageinformers.CSIStorageCapacityInformer
}

func newTestBinder(t *testing.T, ctx context.Context) *testEnv {
	client := &fake.Clientset{}
	logger := klog.FromContext(ctx)
	reactor := pvtesting.NewVolumeReactor(ctx, client, nil, nil, nil)
	// TODO refactor all tests to use real watch mechanism, see #72327
	client.AddWatchReactor("*", func(action k8stesting.Action) (handled bool, ret watch.Interface, err error) {
		gvr := action.GetResource()
		ns := action.GetNamespace()
		watch, err := reactor.Watch(gvr, ns)
		if err != nil {
			return false, nil, err
		}
		return true, watch, nil
	})
	informerFactory := informers.NewSharedInformerFactory(client, controller.NoResyncPeriodFunc())

	podInformer := informerFactory.Core().V1().Pods()
	nodeInformer := informerFactory.Core().V1().Nodes()
	csiNodeInformer := informerFactory.Storage().V1().CSINodes()
	pvcInformer := informerFactory.Core().V1().PersistentVolumeClaims()
	classInformer := informerFactory.Storage().V1().StorageClasses()
	csiDriverInformer := informerFactory.Storage().V1().CSIDrivers()
	csiStorageCapacityInformer := informerFactory.Storage().V1().CSIStorageCapacities()
	capacityCheck := CapacityCheck{
		CSIDriverInformer:          csiDriverInformer,
		CSIStorageCapacityInformer: csiStorageCapacityInformer,
	}
	binder := NewVolumeBinder(
		logger,
		client,
		podInformer,
		nodeInformer,
		csiNodeInformer,
		pvcInformer,
		informerFactory.Core().V1().PersistentVolumes(),
		classInformer,
		capacityCheck,
		10*time.Second)

	// Wait for informers cache sync
	informerFactory.Start(ctx.Done())
	for v, synced := range informerFactory.WaitForCacheSync(ctx.Done()) {
		if !synced {
			logger.Error(nil, "Error syncing informer", "informer", v)
			os.Exit(1)
		}
	}

	// Add storageclasses
	waitMode := storagev1.VolumeBindingWaitForFirstConsumer
	immediateMode := storagev1.VolumeBindingImmediate
	classes := []*storagev1.StorageClass{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name: waitClassWithProvisioner,
			},
			VolumeBindingMode: &waitMode,
			Provisioner:       provisioner,
			AllowedTopologies: []v1.TopologySelectorTerm{
				{
					MatchLabelExpressions: []v1.TopologySelectorLabelRequirement{
						{
							Key:    nodeLabelKey,
							Values: []string{nodeLabelValue, "reference-value"},
						},
					},
				},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name: immediateClass,
			},
			VolumeBindingMode: &immediateMode,
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name: waitClass,
			},
			VolumeBindingMode: &waitMode,
			Provisioner:       "kubernetes.io/no-provisioner",
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name: topoMismatchClass,
			},
			VolumeBindingMode: &waitMode,
			Provisioner:       provisioner,
			AllowedTopologies: []v1.TopologySelectorTerm{
				{
					MatchLabelExpressions: []v1.TopologySelectorLabelRequirement{
						{
							Key:    nodeLabelKey,
							Values: []string{"reference-value"},
						},
					},
				},
			},
		},
	}
	for _, class := range classes {
		if err := classInformer.Informer().GetIndexer().Add(class); err != nil {
			t.Fatalf("Failed to add storage class to internal cache: %v", err)
		}
	}

	// Get internal types
	internalBinder, ok := binder.(*volumeBinder)
	if !ok {
		t.Fatalf("Failed to convert to internal binder")
	}

	return &testEnv{
		client:                  client,
		reactor:                 reactor,
		binder:                  binder,
		internalBinder:          internalBinder,
		internalPodInformer:     podInformer,
		internalNodeInformer:    nodeInformer,
		internalCSINodeInformer: csiNodeInformer,

		internalCSIDriverInformer:          csiDriverInformer,
		internalCSIStorageCapacityInformer: csiStorageCapacityInformer,
	}
}

func (env *testEnv) initNodes(cachedNodes []*v1.Node) {
	nodeInformer := env.internalNodeInformer.Informer()
	for _, node := range cachedNodes {
		nodeInformer.GetIndexer().Add(node)
	}
}

func (env *testEnv) initCSINodes(cachedCSINodes []*storagev1.CSINode) {
	csiNodeInformer := env.internalCSINodeInformer.Informer()
	for _, csiNode := range cachedCSINodes {
		csiNodeInformer.GetIndexer().Add(csiNode)
	}
}

func (env *testEnv) addCSIDriver(csiDriver *storagev1.CSIDriver) {
	csiDriverInformer := env.internalCSIDriverInformer.Informer()
	csiDriverInformer.GetIndexer().Add(csiDriver)
}

func (env *testEnv) addCSIStorageCapacities(capacities []*storagev1.CSIStorageCapacity) {
	csiStorageCapacityInformer := env.internalCSIStorageCapacityInformer.Informer()
	for _, capacity := range capacities {
		csiStorageCapacityInformer.GetIndexer().Add(capacity)
	}
}

func (env *testEnv) initClaims(cachedPVCs []*v1.PersistentVolumeClaim, apiPVCs []*v1.PersistentVolumeClaim) {
	for _, pvc := range cachedPVCs {
		assumecache.AddTestObject(env.internalBinder.pvcCache.AssumeCache, pvc)
		if apiPVCs == nil {
			env.reactor.AddClaim(pvc)
		}
	}
	for _, pvc := range apiPVCs {
		env.reactor.AddClaim(pvc)
	}
}

func (env *testEnv) initVolumes(cachedPVs []*v1.PersistentVolume, apiPVs []*v1.PersistentVolume) {
	for _, pv := range cachedPVs {
		assumecache.AddTestObject(env.internalBinder.pvCache.AssumeCache, pv)
		if apiPVs == nil {
			env.reactor.AddVolume(pv)
		}
	}
	for _, pv := range apiPVs {
		env.reactor.AddVolume(pv)
	}

}

func (env *testEnv) updateVolumes(ctx context.Context, pvs []*v1.PersistentVolume) error {
	for i, pv := range pvs {
		newPv, err := env.client.CoreV1().PersistentVolumes().Update(ctx, pv, metav1.UpdateOptions{})
		if err != nil {
			return err
		}
		pvs[i] = newPv
	}
	return wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, 3*time.Second, false, func(ctx context.Context) (bool, error) {
		for _, pv := range pvs {
			obj, err := env.internalBinder.pvCache.GetAPIObj(pv.Name)
			if obj == nil || err != nil {
				return false, nil
			}
			pvInCache, ok := obj.(*v1.PersistentVolume)
			if !ok {
				return false, fmt.Errorf("PV %s invalid object", pvInCache.Name)
			}
			if versioner.CompareResourceVersion(pvInCache, pv) != 0 {
				return false, nil
			}
		}
		return true, nil
	})
}

func (env *testEnv) updateClaims(ctx context.Context, pvcs []*v1.PersistentVolumeClaim) error {
	for i, pvc := range pvcs {
		newPvc, err := env.client.CoreV1().PersistentVolumeClaims(pvc.Namespace).Update(ctx, pvc, metav1.UpdateOptions{})
		if err != nil {
			return err
		}
		pvcs[i] = newPvc
	}
	return wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, 3*time.Second, false, func(ctx context.Context) (bool, error) {
		for _, pvc := range pvcs {
			obj, err := env.internalBinder.pvcCache.GetAPIObj(getPVCName(pvc))
			if obj == nil || err != nil {
				return false, nil
			}
			pvcInCache, ok := obj.(*v1.PersistentVolumeClaim)
			if !ok {
				return false, fmt.Errorf("PVC %s invalid object", pvcInCache.Name)
			}
			if versioner.CompareResourceVersion(pvcInCache, pvc) != 0 {
				return false, nil
			}
		}
		return true, nil
	})
}

func (env *testEnv) deleteVolumes(pvs []*v1.PersistentVolume) {
	for _, pv := range pvs {
		assumecache.DeleteTestObject(env.internalBinder.pvCache.AssumeCache, pv)
	}
}

func (env *testEnv) deleteClaims(pvcs []*v1.PersistentVolumeClaim) {
	for _, pvc := range pvcs {
		assumecache.DeleteTestObject(env.internalBinder.pvcCache.AssumeCache, pvc)
	}
}

func (env *testEnv) assumeVolumes(t *testing.T, node string, pod *v1.Pod, bindings []*BindingInfo, provisionings []*v1.PersistentVolumeClaim) {
	pvCache := env.internalBinder.pvCache
	for _, binding := range bindings {
		if err := pvCache.Assume(binding.pv); err != nil {
			t.Fatalf("error: %v", err)
		}
	}

	pvcCache := env.internalBinder.pvcCache
	for _, pvc := range provisionings {
		if err := pvcCache.Assume(pvc); err != nil {
			t.Fatalf("error: %v", err)
		}
	}
}

func (env *testEnv) validatePodCache(t *testing.T, node string, pod *v1.Pod, podVolumes *PodVolumes, expectedBindings []*BindingInfo, expectedProvisionings []*DynamicProvision) {
	var (
		bindings          []*BindingInfo
		dynamicProvisions []*DynamicProvision
	)
	if podVolumes != nil {
		bindings = podVolumes.StaticBindings
		dynamicProvisions = podVolumes.DynamicProvisions
	}
	if aLen, eLen := len(bindings), len(expectedBindings); aLen != eLen {
		t.Errorf("expected %v bindings, got %v", eLen, aLen)
	} else if expectedBindings == nil && bindings != nil {
		// nil and empty are different
		t.Error("expected nil bindings, got empty")
	} else if expectedBindings != nil && bindings == nil {
		// nil and empty are different
		t.Error("expected empty bindings, got nil")
	} else {
		for i := 0; i < aLen; i++ {
			// Validate PV
			if diff := cmp.Diff(expectedBindings[i].pv, bindings[i].pv); diff != "" {
				t.Errorf("binding.pv doesn't match (-want, +got):\n%s", diff)
			}

			// Validate PVC
			if diff := cmp.Diff(expectedBindings[i].pvc, bindings[i].pvc); diff != "" {
				t.Errorf("binding.pvc doesn't match (-want, +got):\n%s", diff)
			}
		}
	}

	if aLen, eLen := len(dynamicProvisions), len(expectedProvisionings); aLen != eLen {
		t.Errorf("expected %v provisioned claims, got %v", eLen, aLen)
	} else if expectedProvisionings == nil && dynamicProvisions != nil {
		// nil and empty are different
		t.Error("expected nil provisionings, got empty")
	} else if expectedProvisionings != nil && dynamicProvisions == nil {
		// nil and empty are different
		t.Error("expected empty provisionings, got nil")
	} else {
		for i := 0; i < aLen; i++ {
			if diff := cmp.Diff(expectedProvisionings[i], dynamicProvisions[i]); diff != "" {
				t.Errorf("provisioned claims doesn't match (-want, +got):\n%s", diff)
			}
		}
	}
}

func (env *testEnv) validateAssume(t *testing.T, pod *v1.Pod, bindings []*BindingInfo, provisionings []*v1.PersistentVolumeClaim) {
	// Check pv cache
	pvCache := env.internalBinder.pvCache
	for _, b := range bindings {
		pv, err := pvCache.GetPV(b.pv.Name)
		if err != nil {
			t.Errorf("GetPV %q returned error: %v", b.pv.Name, err)
			continue
		}
		if pv.Spec.ClaimRef == nil {
			t.Errorf("PV %q ClaimRef is nil", b.pv.Name)
			continue
		}
		if pv.Spec.ClaimRef.Name != b.pvc.Name {
			t.Errorf("expected PV.ClaimRef.Name %q, got %q", b.pvc.Name, pv.Spec.ClaimRef.Name)
		}
		if pv.Spec.ClaimRef.Namespace != b.pvc.Namespace {
			t.Errorf("expected PV.ClaimRef.Namespace %q, got %q", b.pvc.Namespace, pv.Spec.ClaimRef.Namespace)
		}
	}

	// Check pvc cache
	pvcCache := env.internalBinder.pvcCache
	for _, p := range provisionings {
		pvcKey := getPVCName(p)
		pvc, err := pvcCache.GetPVC(pvcKey)
		if err != nil {
			t.Errorf("GetPVC %q returned error: %v", pvcKey, err)
			continue
		}
		if pvc.Annotations[volume.AnnSelectedNode] != nodeLabelValue {
			t.Errorf("expected volume.AnnSelectedNode of pvc %q to be %q, but got %q", pvcKey, nodeLabelValue, pvc.Annotations[volume.AnnSelectedNode])
		}
	}
}

func (env *testEnv) validateCacheRestored(t *testing.T, pod *v1.Pod, bindings []*BindingInfo, provisionings []*v1.PersistentVolumeClaim) {
	// All PVs have been unmodified in cache
	pvCache := env.internalBinder.pvCache
	for _, b := range bindings {
		pv, _ := pvCache.GetPV(b.pv.Name)
		apiPV, _ := pvCache.GetAPIPV(b.pv.Name)
		// PV could be nil if it's missing from cache
		if pv != nil && pv != apiPV {
			t.Errorf("PV %q was modified in cache", b.pv.Name)
		}
	}

	// Check pvc cache
	pvcCache := env.internalBinder.pvcCache
	for _, p := range provisionings {
		pvcKey := getPVCName(p)
		pvc, err := pvcCache.GetPVC(pvcKey)
		if err != nil {
			t.Errorf("GetPVC %q returned error: %v", pvcKey, err)
			continue
		}
		if pvc.Annotations[volume.AnnSelectedNode] != "" {
			t.Errorf("expected volume.AnnSelectedNode of pvc %q empty, but got %q", pvcKey, pvc.Annotations[volume.AnnSelectedNode])
		}
	}
}

func (env *testEnv) validateBind(
	t *testing.T,
	pod *v1.Pod,
	expectedPVs []*v1.PersistentVolume,
	expectedAPIPVs []*v1.PersistentVolume) {

	// Check pv cache
	pvCache := env.internalBinder.pvCache
	for _, pv := range expectedPVs {
		cachedPV, err := pvCache.GetPV(pv.Name)
		if err != nil {
			t.Errorf("GetPV %q returned error: %v", pv.Name, err)
		}
		// Cache may be overridden by API object with higher version, compare but ignore resource version.
		newCachedPV := cachedPV.DeepCopy()
		newCachedPV.ResourceVersion = pv.ResourceVersion
		if diff := cmp.Diff(pv, newCachedPV); diff != "" {
			t.Errorf("cached PV check failed (-want, +got):\n%s", diff)
		}
	}

	// Check reactor for API updates
	if err := env.reactor.CheckVolumes(expectedAPIPVs); err != nil {
		t.Errorf("API reactor validation failed: %v", err)
	}
}

func (env *testEnv) validateProvision(
	t *testing.T,
	pod *v1.Pod,
	expectedPVCs []*v1.PersistentVolumeClaim,
	expectedAPIPVCs []*v1.PersistentVolumeClaim) {

	// Check pvc cache
	pvcCache := env.internalBinder.pvcCache
	for _, pvc := range expectedPVCs {
		cachedPVC, err := pvcCache.GetPVC(getPVCName(pvc))
		if err != nil {
			t.Errorf("GetPVC %q returned error: %v", getPVCName(pvc), err)
		}
		// Cache may be overridden by API object with higher version, compare but ignore resource version.
		newCachedPVC := cachedPVC.DeepCopy()
		newCachedPVC.ResourceVersion = pvc.ResourceVersion
		if diff := cmp.Diff(pvc, newCachedPVC); diff != "" {
			t.Errorf("cached PVC check failed (-want, +got):\n%s", diff)
		}
	}

	// Check reactor for API updates
	if err := env.reactor.CheckClaims(expectedAPIPVCs); err != nil {
		t.Errorf("API reactor validation failed: %v", err)
	}
}

const (
	pvcUnbound = iota
	pvcPrebound
	pvcBound
	pvcSelectedNode
)

func makeGenericEphemeralPVC(volumeName string, owned bool) *v1.PersistentVolumeClaim {
	pod := makePod("test-pod").
		withNamespace("testns").
		withNodeName("node1").
		withGenericEphemeralVolume("").Pod

	pvc := makeTestPVC(pod.Name+"-"+volumeName, "1G", "", pvcBound, "pv-bound", "1", &immediateClass)
	if owned {
		controller := true
		pvc.OwnerReferences = []metav1.OwnerReference{
			{
				Name:       pod.Name,
				UID:        pod.UID,
				Controller: &controller,
			},
		}
	}
	return pvc
}

func makeTestPVC(name, size, node string, pvcBoundState int, pvName, resourceVersion string, className *string) *v1.PersistentVolumeClaim {
	fs := v1.PersistentVolumeFilesystem
	pvc := &v1.PersistentVolumeClaim{
		TypeMeta: metav1.TypeMeta{
			Kind:       "PersistentVolumeClaim",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Namespace:       "testns",
			UID:             types.UID("pvc-uid"),
			ResourceVersion: resourceVersion,
		},
		Spec: v1.PersistentVolumeClaimSpec{
			Resources: v1.VolumeResourceRequirements{
				Requests: v1.ResourceList{
					v1.ResourceName(v1.ResourceStorage): resource.MustParse(size),
				},
			},
			StorageClassName: className,
			VolumeMode:       &fs,
		},
	}

	switch pvcBoundState {
	case pvcSelectedNode:
		metav1.SetMetaDataAnnotation(&pvc.ObjectMeta, volume.AnnSelectedNode, node)
		// don't fallthrough
	case pvcBound:
		metav1.SetMetaDataAnnotation(&pvc.ObjectMeta, volume.AnnBindCompleted, "yes")
		fallthrough
	case pvcPrebound:
		pvc.Spec.VolumeName = pvName
	}
	return pvc
}

func makeTestPV(name, node, capacity, version string, boundToPVC *v1.PersistentVolumeClaim, className string) *v1.PersistentVolume {
	fs := v1.PersistentVolumeFilesystem
	pv := &v1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			ResourceVersion: version,
		},
		Spec: v1.PersistentVolumeSpec{
			Capacity: v1.ResourceList{
				v1.ResourceName(v1.ResourceStorage): resource.MustParse(capacity),
			},
			StorageClassName: className,
			VolumeMode:       &fs,
		},
		Status: v1.PersistentVolumeStatus{
			Phase: v1.VolumeAvailable,
		},
	}
	if node != "" {
		pv.Spec.NodeAffinity = &v1.VolumeNodeAffinity{
			Required: &v1.NodeSelector{
				NodeSelectorTerms: []v1.NodeSelectorTerm{
					{
						MatchExpressions: []v1.NodeSelectorRequirement{
							{
								Key:      nodeLabelKey,
								Operator: v1.NodeSelectorOpIn,
								Values:   []string{node},
							},
						},
					},
				},
			},
		}
	}

	if boundToPVC != nil {
		pv.Spec.ClaimRef = &v1.ObjectReference{
			Kind:            boundToPVC.Kind,
			APIVersion:      boundToPVC.APIVersion,
			ResourceVersion: boundToPVC.ResourceVersion,
			Name:            boundToPVC.Name,
			Namespace:       boundToPVC.Namespace,
			UID:             boundToPVC.UID,
		}
		metav1.SetMetaDataAnnotation(&pv.ObjectMeta, volume.AnnBoundByController, "yes")
	}

	return pv
}

func makeTestPVForCSIMigration(labels map[string]string, pvc *v1.PersistentVolumeClaim, migrationEnabled bool) *v1.PersistentVolume {
	pv := makeTestPV("pv-migration-bound", "node1", "1G", "1", pvc, waitClass)
	pv.Spec.NodeAffinity = nil // Will be written by the CSI translation lib
	pv.ObjectMeta.Labels = labels
	// GCEPersistentDisk is used when migration is enabled, as its featuregate is locked to GA.
	// RBD is used for the nonmigrated case, as its featuregate is still alpha. When RBD migration goes GA,
	// a different nonmigrated plugin should be used instead. If there are no other plugins, then the
	// nonmigrated test case is no longer relevant and can be removed.
	if migrationEnabled {
		pv.Spec.PersistentVolumeSource = v1.PersistentVolumeSource{
			GCEPersistentDisk: &v1.GCEPersistentDiskVolumeSource{
				PDName:    "test-disk",
				FSType:    "ext4",
				Partition: 0,
				ReadOnly:  false,
			},
		}
	} else {
		pv.Spec.PersistentVolumeSource = v1.PersistentVolumeSource{
			RBD: &v1.RBDPersistentVolumeSource{
				RBDImage: "test-disk",
			},
		}
	}
	return pv
}

func pvcSetSelectedNode(pvc *v1.PersistentVolumeClaim, node string) *v1.PersistentVolumeClaim {
	newPVC := pvc.DeepCopy()
	metav1.SetMetaDataAnnotation(&newPVC.ObjectMeta, volume.AnnSelectedNode, node)
	return newPVC
}

func pvcSetEmptyAnnotations(pvc *v1.PersistentVolumeClaim) *v1.PersistentVolumeClaim {
	newPVC := pvc.DeepCopy()
	newPVC.Annotations = map[string]string{}
	return newPVC
}

func pvRemoveClaimUID(pv *v1.PersistentVolume) *v1.PersistentVolume {
	newPV := pv.DeepCopy()
	newPV.Spec.ClaimRef.UID = ""
	return newPV
}

func makeCSINode(name, migratedPlugin string) *storagev1.CSINode {
	return &storagev1.CSINode{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Annotations: map[string]string{
				v1.MigratedPluginsAnnotationKey: migratedPlugin,
			},
		},
	}
}

func makeCSIDriver(name string, storageCapacity bool) *storagev1.CSIDriver {
	return &storagev1.CSIDriver{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: storagev1.CSIDriverSpec{
			StorageCapacity: &storageCapacity,
		},
	}
}

func makeCapacity(name, storageClassName string, node *v1.Node, capacityStr, maximumVolumeSizeStr string) *storagev1.CSIStorageCapacity {
	c := &storagev1.CSIStorageCapacity{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		StorageClassName: storageClassName,
		NodeTopology:     &metav1.LabelSelector{},
	}
	if node != nil {
		c.NodeTopology.MatchLabels = map[string]string{nodeLabelKey: node.Labels[nodeLabelKey]}
	}
	if capacityStr != "" {
		capacityQuantity := resource.MustParse(capacityStr)
		c.Capacity = &capacityQuantity
	}
	if maximumVolumeSizeStr != "" {
		maximumVolumeSizeQuantity := resource.MustParse(maximumVolumeSizeStr)
		c.MaximumVolumeSize = &maximumVolumeSizeQuantity
	}
	return c
}

func makeBinding(pvc *v1.PersistentVolumeClaim, pv *v1.PersistentVolume) *BindingInfo {
	return &BindingInfo{pvc: pvc.DeepCopy(), pv: pv.DeepCopy()}
}

func addProvisionAnn(pvc *v1.PersistentVolumeClaim) *v1.PersistentVolumeClaim {
	res := pvc.DeepCopy()
	// Add provision related annotations
	metav1.SetMetaDataAnnotation(&res.ObjectMeta, volume.AnnSelectedNode, nodeLabelValue)

	return res
}

// reasonNames pretty-prints a list of reasons with variable names in
// case of a test failure because that is easier to read than the full
// strings.
func reasonNames(reasons ConflictReasons) string {
	var varNames []string
	for _, reason := range reasons {
		switch reason {
		case ErrReasonBindConflict:
			varNames = append(varNames, "ErrReasonBindConflict")
		case ErrReasonNodeConflict:
			varNames = append(varNames, "ErrReasonNodeConflict")
		case ErrReasonNotEnoughSpace:
			varNames = append(varNames, "ErrReasonNotEnoughSpace")
		default:
			varNames = append(varNames, string(reason))
		}
	}
	return fmt.Sprintf("%v", varNames)
}

func checkReasons(t *testing.T, actual, expected ConflictReasons) {
	equal := len(actual) == len(expected)
	sort.Sort(actual)
	sort.Sort(expected)
	if equal {
		for i, reason := range actual {
			if reason != expected[i] {
				equal = false
				break
			}
		}
	}
	if !equal {
		t.Errorf("expected failure reasons %s, got %s", reasonNames(expected), reasonNames(actual))
	}
}

// findPodVolumes gets and finds volumes for given pod and node
func findPodVolumes(logger klog.Logger, binder SchedulerVolumeBinder, pod *v1.Pod, node *v1.Node) (*PodVolumes, ConflictReasons, error) {
	podVolumeClaims, err := binder.GetPodVolumeClaims(logger, pod)
	if err != nil {
		return nil, nil, err
	}
	if len(podVolumeClaims.unboundClaimsImmediate) > 0 {
		return nil, nil, fmt.Errorf("pod has unbound immediate PersistentVolumeClaims")
	}
	return binder.FindPodVolumes(logger, pod, podVolumeClaims, node)
}

func TestFindPodVolumesWithoutProvisioning(t *testing.T) {
	t.Parallel()

	type scenarioType struct {
		// Inputs
		pvs     []*v1.PersistentVolume
		podPVCs []*v1.PersistentVolumeClaim
		// If nil, use pod PVCs
		cachePVCs []*v1.PersistentVolumeClaim
		// If nil, makePod with podPVCs
		pod *v1.Pod

		// Expected podBindingCache fields
		expectedBindings []*BindingInfo

		// Expected return values
		reasons    ConflictReasons
		shouldFail bool
	}
	scenarios := map[string]scenarioType{
		"no-volumes": {
			pod: makePod("test-pod").
				withNamespace("testns").
				withNodeName("node1").Pod,
		},
		"no-pvcs": {
			pod: makePod("test-pod").
				withNamespace("testns").
				withNodeName("node1").
				withEmptyDirVolume().Pod,
		},
		"pvc-not-found": {
			cachePVCs:  []*v1.PersistentVolumeClaim{},
			podPVCs:    []*v1.PersistentVolumeClaim{boundPVC},
			shouldFail: true,
		},
		"bound-pvc": {
			podPVCs: []*v1.PersistentVolumeClaim{boundPVC},
			pvs:     []*v1.PersistentVolume{pvBound},
		},
		"bound-pvc,pv-not-exists": {
			podPVCs:    []*v1.PersistentVolumeClaim{boundPVC},
			shouldFail: false,
			reasons:    ConflictReasons{ErrReasonPVNotExist},
		},
		"prebound-pvc": {
			podPVCs:    []*v1.PersistentVolumeClaim{preboundPVC},
			pvs:        []*v1.PersistentVolume{pvNode1aBound},
			shouldFail: true,
		},
		"unbound-pvc,pv-same-node": {
			podPVCs:          []*v1.PersistentVolumeClaim{unboundPVC},
			pvs:              []*v1.PersistentVolume{pvNode2, pvNode1a, pvNode1b},
			expectedBindings: []*BindingInfo{makeBinding(unboundPVC, pvNode1a)},
		},
		"unbound-pvc,pv-different-node": {
			podPVCs: []*v1.PersistentVolumeClaim{unboundPVC},
			pvs:     []*v1.PersistentVolume{pvNode2},
			reasons: ConflictReasons{ErrReasonBindConflict},
		},
		"two-unbound-pvcs": {
			podPVCs:          []*v1.PersistentVolumeClaim{unboundPVC, unboundPVC2},
			pvs:              []*v1.PersistentVolume{pvNode1a, pvNode1b},
			expectedBindings: []*BindingInfo{makeBinding(unboundPVC, pvNode1a), makeBinding(unboundPVC2, pvNode1b)},
		},
		"two-unbound-pvcs,order-by-size": {
			podPVCs:          []*v1.PersistentVolumeClaim{unboundPVC2, unboundPVC},
			pvs:              []*v1.PersistentVolume{pvNode1a, pvNode1b},
			expectedBindings: []*BindingInfo{makeBinding(unboundPVC, pvNode1a), makeBinding(unboundPVC2, pvNode1b)},
		},
		"two-unbound-pvcs,partial-match": {
			podPVCs:          []*v1.PersistentVolumeClaim{unboundPVC, unboundPVC2},
			pvs:              []*v1.PersistentVolume{pvNode1a},
			expectedBindings: []*BindingInfo{makeBinding(unboundPVC, pvNode1a)},
			reasons:          ConflictReasons{ErrReasonBindConflict},
		},
		"one-bound,one-unbound": {
			podPVCs:          []*v1.PersistentVolumeClaim{unboundPVC, boundPVC},
			pvs:              []*v1.PersistentVolume{pvBound, pvNode1a},
			expectedBindings: []*BindingInfo{makeBinding(unboundPVC, pvNode1a)},
		},
		"one-bound,one-unbound,no-match": {
			podPVCs: []*v1.PersistentVolumeClaim{unboundPVC, boundPVC},
			pvs:     []*v1.PersistentVolume{pvBound, pvNode2},
			reasons: ConflictReasons{ErrReasonBindConflict},
		},
		"one-prebound,one-unbound": {
			podPVCs:    []*v1.PersistentVolumeClaim{unboundPVC, preboundPVC},
			pvs:        []*v1.PersistentVolume{pvNode1a, pvNode1b},
			shouldFail: true,
		},
		"immediate-bound-pvc": {
			podPVCs: []*v1.PersistentVolumeClaim{immediateBoundPVC},
			pvs:     []*v1.PersistentVolume{pvBoundImmediate},
		},
		"immediate-bound-pvc-wrong-node": {
			podPVCs: []*v1.PersistentVolumeClaim{immediateBoundPVC},
			pvs:     []*v1.PersistentVolume{pvBoundImmediateNode2},
			reasons: ConflictReasons{ErrReasonNodeConflict},
		},
		"immediate-unbound-pvc": {
			podPVCs:    []*v1.PersistentVolumeClaim{immediateUnboundPVC},
			shouldFail: true,
		},
		"immediate-unbound-pvc,delayed-mode-bound": {
			podPVCs:    []*v1.PersistentVolumeClaim{immediateUnboundPVC, boundPVC},
			pvs:        []*v1.PersistentVolume{pvBound},
			shouldFail: true,
		},
		"immediate-unbound-pvc,delayed-mode-unbound": {
			podPVCs:    []*v1.PersistentVolumeClaim{immediateUnboundPVC, unboundPVC},
			shouldFail: true,
		},
		"generic-ephemeral,no-pvc": {
			pod: makePod("test-pod").
				withNamespace("testns").
				withNodeName("node1").
				withGenericEphemeralVolume("no-such-pvc").Pod,
			shouldFail: true,
		},
		"generic-ephemeral,with-pvc": {
			pod: makePod("test-pod").
				withNamespace("testns").
				withNodeName("node1").
				withGenericEphemeralVolume("test-volume").Pod,
			cachePVCs: []*v1.PersistentVolumeClaim{correctGenericPVC},
			pvs:       []*v1.PersistentVolume{pvBoundGeneric},
		},
		"generic-ephemeral,wrong-pvc": {
			pod: makePod("test-pod").
				withNamespace("testns").
				withNodeName("node1").
				withGenericEphemeralVolume("test-volume").Pod,
			cachePVCs:  []*v1.PersistentVolumeClaim{conflictingGenericPVC},
			pvs:        []*v1.PersistentVolume{pvBoundGeneric},
			shouldFail: true,
		},
	}

	testNode := &v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "node1",
			Labels: map[string]string{
				nodeLabelKey: "node1",
			},
		},
	}

	run := func(t *testing.T, scenario scenarioType, csiDriver *storagev1.CSIDriver) {
		logger, ctx := ktesting.NewTestContext(t)
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()

		// Setup
		testEnv := newTestBinder(t, ctx)
		testEnv.initVolumes(scenario.pvs, scenario.pvs)
		if csiDriver != nil {
			testEnv.addCSIDriver(csiDriver)
		}

		// a. Init pvc cache
		if scenario.cachePVCs == nil {
			scenario.cachePVCs = scenario.podPVCs
		}
		testEnv.initClaims(scenario.cachePVCs, scenario.cachePVCs)

		// b. Generate pod with given claims
		if scenario.pod == nil {
			scenario.pod = makePod("test-pod").
				withNamespace("testns").
				withNodeName("node1").
				withPVCSVolume(scenario.podPVCs).Pod
		}

		// Execute
		podVolumes, reasons, err := findPodVolumes(logger, testEnv.binder, scenario.pod, testNode)

		// Validate
		if !scenario.shouldFail && err != nil {
			t.Errorf("returned error: %v", err)
		}
		if scenario.shouldFail && err == nil {
			t.Error("returned success but expected error")
		}
		checkReasons(t, reasons, scenario.reasons)
		testEnv.validatePodCache(t, testNode.Name, scenario.pod, podVolumes, scenario.expectedBindings, nil)
	}

	for description, csiDriver := range map[string]*storagev1.CSIDriver{
		"no CSIDriver":                        nil,
		"CSIDriver with capacity tracking":    makeCSIDriver(provisioner, true),
		"CSIDriver without capacity tracking": makeCSIDriver(provisioner, false),
	} {
		t.Run(description, func(t *testing.T) {
			for name, scenario := range scenarios {
				t.Run(name, func(t *testing.T) { run(t, scenario, csiDriver) })
			}
		})
	}
}

func TestFindPodVolumesWithProvisioning(t *testing.T) {
	t.Parallel()

	type scenarioType struct {
		// Inputs
		pvs     []*v1.PersistentVolume
		podPVCs []*v1.PersistentVolumeClaim
		// If nil, use pod PVCs
		cachePVCs []*v1.PersistentVolumeClaim
		// If nil, makePod with podPVCs
		pod *v1.Pod

		// Expected podBindingCache fields
		expectedBindings   []*BindingInfo
		expectedProvisions []*DynamicProvision

		// Expected return values
		reasons       ConflictReasons
		shouldFail    bool
		needsCapacity bool
	}
	scenarios := map[string]scenarioType{
		"one-provisioned": {
			podPVCs:            []*v1.PersistentVolumeClaim{provisionedPVC},
			expectedProvisions: []*DynamicProvision{{PVC: provisionedPVC}},
			needsCapacity:      true,
		},
		"two-unbound-pvcs,one-matched,one-provisioned": {
			podPVCs:            []*v1.PersistentVolumeClaim{unboundPVC, provisionedPVC},
			pvs:                []*v1.PersistentVolume{pvNode1a},
			expectedBindings:   []*BindingInfo{makeBinding(unboundPVC, pvNode1a)},
			expectedProvisions: []*DynamicProvision{{PVC: provisionedPVC}},
			needsCapacity:      true,
		},
		"one-bound,one-provisioned": {
			podPVCs:            []*v1.PersistentVolumeClaim{boundPVC, provisionedPVC},
			pvs:                []*v1.PersistentVolume{pvBound},
			expectedProvisions: []*DynamicProvision{{PVC: provisionedPVC}},
			needsCapacity:      true,
		},
		"one-binding,one-selected-node": {
			podPVCs:            []*v1.PersistentVolumeClaim{boundPVC, selectedNodePVC},
			pvs:                []*v1.PersistentVolume{pvBound},
			expectedProvisions: []*DynamicProvision{{PVC: selectedNodePVC}},
			needsCapacity:      true,
		},
		"immediate-unbound-pvc": {
			podPVCs:    []*v1.PersistentVolumeClaim{immediateUnboundPVC},
			shouldFail: true,
		},
		"one-immediate-bound,one-provisioned": {
			podPVCs:            []*v1.PersistentVolumeClaim{immediateBoundPVC, provisionedPVC},
			pvs:                []*v1.PersistentVolume{pvBoundImmediate},
			expectedProvisions: []*DynamicProvision{{PVC: provisionedPVC}},
			needsCapacity:      true,
		},
		"invalid-provisioner": {
			podPVCs: []*v1.PersistentVolumeClaim{noProvisionerPVC},
			reasons: ConflictReasons{ErrReasonBindConflict},
		},
		"volume-topology-unsatisfied": {
			podPVCs: []*v1.PersistentVolumeClaim{topoMismatchPVC},
			reasons: ConflictReasons{ErrReasonBindConflict},
		},
	}

	testNode := &v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "node1",
			Labels: map[string]string{
				nodeLabelKey: "node1",
			},
		},
	}

	run := func(t *testing.T, scenario scenarioType, csiDriver *storagev1.CSIDriver) {
		logger, ctx := ktesting.NewTestContext(t)
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()

		// Setup
		testEnv := newTestBinder(t, ctx)
		testEnv.initVolumes(scenario.pvs, scenario.pvs)
		if csiDriver != nil {
			testEnv.addCSIDriver(csiDriver)
		}

		// a. Init pvc cache
		if scenario.cachePVCs == nil {
			scenario.cachePVCs = scenario.podPVCs
		}
		testEnv.initClaims(scenario.cachePVCs, scenario.cachePVCs)

		// b. Generate pod with given claims
		if scenario.pod == nil {
			scenario.pod = makePod("test-pod").
				withNamespace("testns").
				withNodeName("node1").
				withPVCSVolume(scenario.podPVCs).Pod
		}

		// Execute
		podVolumes, reasons, err := findPodVolumes(logger, testEnv.binder, scenario.pod, testNode)

		// Validate
		if !scenario.shouldFail && err != nil {
			t.Errorf("returned error: %v", err)
		}
		if scenario.shouldFail && err == nil {
			t.Error("returned success but expected error")
		}
		expectedReasons := scenario.reasons
		expectedProvisions := scenario.expectedProvisions
		if scenario.needsCapacity &&
			csiDriver != nil && csiDriver.Spec.StorageCapacity != nil && *csiDriver.Spec.StorageCapacity {
			// Without CSIStorageCapacity objects, provisioning is blocked.
			expectedReasons = append(expectedReasons, ErrReasonNotEnoughSpace)
			expectedProvisions = nil
		}
		checkReasons(t, reasons, expectedReasons)
		testEnv.validatePodCache(t, testNode.Name, scenario.pod, podVolumes, scenario.expectedBindings, expectedProvisions)
	}

	for description, csiDriver := range map[string]*storagev1.CSIDriver{
		"no CSIDriver":                        nil,
		"CSIDriver with capacity tracking":    makeCSIDriver(provisioner, true),
		"CSIDriver without capacity tracking": makeCSIDriver(provisioner, false),
	} {
		t.Run(description, func(t *testing.T) {
			for name, scenario := range scenarios {
				t.Run(name, func(t *testing.T) { run(t, scenario, csiDriver) })
			}
		})
	}
}

// TestFindPodVolumesWithCSIMigration aims to test the node affinity check procedure that's
// done in FindPodVolumes. In order to reach this code path, the given PVCs must be bound to a PV.
func TestFindPodVolumesWithCSIMigration(t *testing.T) {
	type scenarioType struct {
		// Inputs
		pvs     []*v1.PersistentVolume
		podPVCs []*v1.PersistentVolumeClaim
		// If nil, use pod PVCs
		cachePVCs []*v1.PersistentVolumeClaim
		// If nil, makePod with podPVCs
		pod *v1.Pod

		// Setup
		initNodes    []*v1.Node
		initCSINodes []*storagev1.CSINode

		// Expected return values
		reasons    ConflictReasons
		shouldFail bool
	}
	scenarios := map[string]scenarioType{
		"pvc-bound": {
			podPVCs:      []*v1.PersistentVolumeClaim{boundMigrationPVC},
			pvs:          []*v1.PersistentVolume{migrationPVBound},
			initNodes:    []*v1.Node{node1Zone1},
			initCSINodes: []*storagev1.CSINode{csiNode1Migrated},
		},
		"pvc-bound,csinode-not-migrated": {
			podPVCs:      []*v1.PersistentVolumeClaim{boundMigrationPVC},
			pvs:          []*v1.PersistentVolume{migrationPVBound},
			initNodes:    []*v1.Node{node1Zone1},
			initCSINodes: []*storagev1.CSINode{csiNode1NotMigrated},
		},
		"pvc-bound,missing-csinode": {
			podPVCs:   []*v1.PersistentVolumeClaim{boundMigrationPVC},
			pvs:       []*v1.PersistentVolume{migrationPVBound},
			initNodes: []*v1.Node{node1Zone1},
		},
		"pvc-bound,node-different-zone": {
			podPVCs:      []*v1.PersistentVolumeClaim{boundMigrationPVC},
			pvs:          []*v1.PersistentVolume{migrationPVBound},
			initNodes:    []*v1.Node{node1Zone2},
			initCSINodes: []*storagev1.CSINode{csiNode1Migrated},
			reasons:      ConflictReasons{ErrReasonNodeConflict},
		},
	}

	run := func(t *testing.T, scenario scenarioType) {
		logger, ctx := ktesting.NewTestContext(t)
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()

		// Setup
		testEnv := newTestBinder(t, ctx)
		testEnv.initVolumes(scenario.pvs, scenario.pvs)

		var node *v1.Node
		if len(scenario.initNodes) > 0 {
			testEnv.initNodes(scenario.initNodes)
			node = scenario.initNodes[0]
		} else {
			node = node1
		}

		if len(scenario.initCSINodes) > 0 {
			testEnv.initCSINodes(scenario.initCSINodes)
		}

		// a. Init pvc cache
		if scenario.cachePVCs == nil {
			scenario.cachePVCs = scenario.podPVCs
		}
		testEnv.initClaims(scenario.cachePVCs, scenario.cachePVCs)

		// b. Generate pod with given claims
		if scenario.pod == nil {
			scenario.pod = makePod("test-pod").
				withNamespace("testns").
				withNodeName("node1").
				withPVCSVolume(scenario.podPVCs).Pod
		}

		// Execute
		_, reasons, err := findPodVolumes(logger, testEnv.binder, scenario.pod, node)

		// Validate
		if !scenario.shouldFail && err != nil {
			t.Errorf("returned error: %v", err)
		}
		if scenario.shouldFail && err == nil {
			t.Error("returned success but expected error")
		}
		checkReasons(t, reasons, scenario.reasons)
	}

	for name, scenario := range scenarios {
		t.Run(name, func(t *testing.T) { run(t, scenario) })
	}
}

func TestAssumePodVolumes(t *testing.T) {
	type scenarioType struct {
		// Inputs
		podPVCs           []*v1.PersistentVolumeClaim
		pvs               []*v1.PersistentVolume
		bindings          []*BindingInfo
		dynamicProvisions []*DynamicProvision

		// Expected return values
		shouldFail       bool
		expectedAllBound bool

		expectedBindings      []*BindingInfo
		expectedProvisionings []*DynamicProvision
	}
	scenarios := map[string]scenarioType{
		"all-bound": {
			podPVCs:          []*v1.PersistentVolumeClaim{boundPVC},
			pvs:              []*v1.PersistentVolume{pvBound},
			expectedAllBound: true,
		},
		"one-binding": {
			podPVCs:               []*v1.PersistentVolumeClaim{unboundPVC},
			bindings:              []*BindingInfo{makeBinding(unboundPVC, pvNode1a)},
			pvs:                   []*v1.PersistentVolume{pvNode1a},
			expectedBindings:      []*BindingInfo{makeBinding(unboundPVC, pvNode1aBound)},
			expectedProvisionings: []*DynamicProvision{},
		},
		"two-bindings": {
			podPVCs:               []*v1.PersistentVolumeClaim{unboundPVC, unboundPVC2},
			bindings:              []*BindingInfo{makeBinding(unboundPVC, pvNode1a), makeBinding(unboundPVC2, pvNode1b)},
			pvs:                   []*v1.PersistentVolume{pvNode1a, pvNode1b},
			expectedBindings:      []*BindingInfo{makeBinding(unboundPVC, pvNode1aBound), makeBinding(unboundPVC2, pvNode1bBound)},
			expectedProvisionings: []*DynamicProvision{},
		},
		"pv-already-bound": {
			podPVCs:               []*v1.PersistentVolumeClaim{unboundPVC},
			bindings:              []*BindingInfo{makeBinding(unboundPVC, pvNode1aBound)},
			pvs:                   []*v1.PersistentVolume{pvNode1aBound},
			expectedBindings:      []*BindingInfo{makeBinding(unboundPVC, pvNode1aBound)},
			expectedProvisionings: []*DynamicProvision{},
		},
		"tmpupdate-failed": {
			podPVCs:    []*v1.PersistentVolumeClaim{unboundPVC},
			bindings:   []*BindingInfo{makeBinding(unboundPVC, pvNode1a), makeBinding(unboundPVC2, pvNode1b)},
			pvs:        []*v1.PersistentVolume{pvNode1a},
			shouldFail: true,
		},
		"one-binding, one-pvc-provisioned": {
			podPVCs:               []*v1.PersistentVolumeClaim{unboundPVC, provisionedPVC},
			bindings:              []*BindingInfo{makeBinding(unboundPVC, pvNode1a)},
			pvs:                   []*v1.PersistentVolume{pvNode1a},
			dynamicProvisions:     []*DynamicProvision{{PVC: provisionedPVC}},
			expectedBindings:      []*BindingInfo{makeBinding(unboundPVC, pvNode1aBound)},
			expectedProvisionings: []*DynamicProvision{{PVC: selectedNodePVC}},
		},
		"one-binding, one-provision-tmpupdate-failed": {
			podPVCs:           []*v1.PersistentVolumeClaim{unboundPVC, provisionedPVCHigherVersion},
			bindings:          []*BindingInfo{makeBinding(unboundPVC, pvNode1a)},
			pvs:               []*v1.PersistentVolume{pvNode1a},
			dynamicProvisions: []*DynamicProvision{{PVC: provisionedPVC2}},
			shouldFail:        true,
		},
	}

	run := func(t *testing.T, scenario scenarioType) {
		logger, ctx := ktesting.NewTestContext(t)
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()

		// Setup
		testEnv := newTestBinder(t, ctx)
		testEnv.initClaims(scenario.podPVCs, scenario.podPVCs)
		pod := makePod("test-pod").
			withNamespace("testns").
			withNodeName("node1").
			withPVCSVolume(scenario.podPVCs).Pod
		podVolumes := &PodVolumes{
			StaticBindings:    scenario.bindings,
			DynamicProvisions: scenario.dynamicProvisions,
		}
		testEnv.initVolumes(scenario.pvs, scenario.pvs)

		// Execute
		allBound, err := testEnv.binder.AssumePodVolumes(logger, pod, "node1", podVolumes)

		// Validate
		if !scenario.shouldFail && err != nil {
			t.Errorf("returned error: %v", err)
		}
		if scenario.shouldFail && err == nil {
			t.Error("returned success but expected error")
		}
		if scenario.expectedAllBound != allBound {
			t.Errorf("returned unexpected allBound: %v", allBound)
		}
		if scenario.expectedBindings == nil {
			scenario.expectedBindings = scenario.bindings
		}
		if scenario.expectedProvisionings == nil {
			scenario.expectedProvisionings = scenario.dynamicProvisions
		}
		if scenario.shouldFail {
			pvcs := convertDynamicProvisionsToPVCs(scenario.dynamicProvisions)
			testEnv.validateCacheRestored(t, pod, scenario.bindings, pvcs)
		} else {
			pvcs := convertDynamicProvisionsToPVCs(scenario.expectedProvisionings)
			testEnv.validateAssume(t, pod, scenario.expectedBindings, pvcs)
		}
		testEnv.validatePodCache(t, pod.Spec.NodeName, pod, podVolumes, scenario.expectedBindings, scenario.expectedProvisionings)
	}

	for name, scenario := range scenarios {
		t.Run(name, func(t *testing.T) { run(t, scenario) })
	}
}

func TestRevertAssumedPodVolumes(t *testing.T) {
	logger, ctx := ktesting.NewTestContext(t)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	podPVCs := []*v1.PersistentVolumeClaim{unboundPVC, provisionedPVC}
	bindings := []*BindingInfo{makeBinding(unboundPVC, pvNode1a)}
	pvs := []*v1.PersistentVolume{pvNode1a}
	dynamicProvisions := []*DynamicProvision{{PVC: provisionedPVC}}
	expectedBindings := []*BindingInfo{makeBinding(unboundPVC, pvNode1aBound)}
	expectedProvisionings := []*v1.PersistentVolumeClaim{selectedNodePVC}

	// Setup
	testEnv := newTestBinder(t, ctx)
	testEnv.initClaims(podPVCs, podPVCs)
	pod := makePod("test-pod").
		withNamespace("testns").
		withNodeName("node1").
		withPVCSVolume(podPVCs).Pod
	podVolumes := &PodVolumes{
		StaticBindings:    bindings,
		DynamicProvisions: dynamicProvisions,
	}
	testEnv.initVolumes(pvs, pvs)

	allbound, err := testEnv.binder.AssumePodVolumes(logger, pod, "node1", podVolumes)
	if allbound || err != nil {
		t.Errorf("No volumes are assumed")
	}
	testEnv.validateAssume(t, pod, expectedBindings, expectedProvisionings)

	claims := convertDynamicProvisionsToPVCs(dynamicProvisions)
	testEnv.binder.RevertAssumedPodVolumes(podVolumes)
	testEnv.validateCacheRestored(t, pod, bindings, claims)
}

func TestBindAPIUpdate(t *testing.T) {
	type scenarioType struct {
		// Inputs
		bindings  []*BindingInfo
		cachedPVs []*v1.PersistentVolume
		// if nil, use cachedPVs
		apiPVs []*v1.PersistentVolume

		provisionedPVCs []*v1.PersistentVolumeClaim
		cachedPVCs      []*v1.PersistentVolumeClaim
		// if nil, use cachedPVCs
		apiPVCs []*v1.PersistentVolumeClaim

		// Expected return values
		shouldFail  bool
		expectedPVs []*v1.PersistentVolume
		// if nil, use expectedPVs
		expectedAPIPVs []*v1.PersistentVolume

		expectedPVCs []*v1.PersistentVolumeClaim
		// if nil, use expectedPVCs
		expectedAPIPVCs []*v1.PersistentVolumeClaim
	}
	scenarios := map[string]scenarioType{
		"nothing-to-bind-nil": {
			shouldFail: true,
		},
		"nothing-to-bind-bindings-nil": {
			provisionedPVCs: []*v1.PersistentVolumeClaim{},
			shouldFail:      true,
		},
		"nothing-to-bind-provisionings-nil": {
			bindings:   []*BindingInfo{},
			shouldFail: true,
		},
		"nothing-to-bind-empty": {
			bindings:        []*BindingInfo{},
			provisionedPVCs: []*v1.PersistentVolumeClaim{},
		},
		"one-binding": {
			bindings:        []*BindingInfo{makeBinding(unboundPVC, pvNode1aBound)},
			cachedPVs:       []*v1.PersistentVolume{pvNode1a},
			expectedPVs:     []*v1.PersistentVolume{pvNode1aBound},
			provisionedPVCs: []*v1.PersistentVolumeClaim{},
		},
		"two-bindings": {
			bindings:        []*BindingInfo{makeBinding(unboundPVC, pvNode1aBound), makeBinding(unboundPVC2, pvNode1bBound)},
			cachedPVs:       []*v1.PersistentVolume{pvNode1a, pvNode1b},
			expectedPVs:     []*v1.PersistentVolume{pvNode1aBound, pvNode1bBound},
			provisionedPVCs: []*v1.PersistentVolumeClaim{},
		},
		"api-already-updated": {
			bindings:        []*BindingInfo{makeBinding(unboundPVC, pvNode1aBound)},
			cachedPVs:       []*v1.PersistentVolume{pvNode1aBound},
			expectedPVs:     []*v1.PersistentVolume{pvNode1aBound},
			provisionedPVCs: []*v1.PersistentVolumeClaim{},
		},
		"api-update-failed": {
			bindings:        []*BindingInfo{makeBinding(unboundPVC, pvNode1aBound), makeBinding(unboundPVC2, pvNode1bBound)},
			cachedPVs:       []*v1.PersistentVolume{pvNode1a, pvNode1b},
			apiPVs:          []*v1.PersistentVolume{pvNode1a, pvNode1bBoundHigherVersion},
			expectedPVs:     []*v1.PersistentVolume{pvNode1aBound, pvNode1b},
			expectedAPIPVs:  []*v1.PersistentVolume{pvNode1aBound, pvNode1bBoundHigherVersion},
			provisionedPVCs: []*v1.PersistentVolumeClaim{},
			shouldFail:      true,
		},
		"one-provisioned-pvc": {
			bindings:        []*BindingInfo{},
			provisionedPVCs: []*v1.PersistentVolumeClaim{addProvisionAnn(provisionedPVC)},
			cachedPVCs:      []*v1.PersistentVolumeClaim{provisionedPVC},
			expectedPVCs:    []*v1.PersistentVolumeClaim{addProvisionAnn(provisionedPVC)},
		},
		"provision-api-update-failed": {
			bindings:        []*BindingInfo{},
			provisionedPVCs: []*v1.PersistentVolumeClaim{addProvisionAnn(provisionedPVC), addProvisionAnn(provisionedPVC2)},
			cachedPVCs:      []*v1.PersistentVolumeClaim{provisionedPVC, provisionedPVC2},
			apiPVCs:         []*v1.PersistentVolumeClaim{provisionedPVC, provisionedPVCHigherVersion},
			expectedPVCs:    []*v1.PersistentVolumeClaim{addProvisionAnn(provisionedPVC), provisionedPVC2},
			expectedAPIPVCs: []*v1.PersistentVolumeClaim{addProvisionAnn(provisionedPVC), provisionedPVCHigherVersion},
			shouldFail:      true,
		},
		"binding-succeed, provision-api-update-failed": {
			bindings:        []*BindingInfo{makeBinding(unboundPVC, pvNode1aBound)},
			cachedPVs:       []*v1.PersistentVolume{pvNode1a},
			expectedPVs:     []*v1.PersistentVolume{pvNode1aBound},
			provisionedPVCs: []*v1.PersistentVolumeClaim{addProvisionAnn(provisionedPVC), addProvisionAnn(provisionedPVC2)},
			cachedPVCs:      []*v1.PersistentVolumeClaim{provisionedPVC, provisionedPVC2},
			apiPVCs:         []*v1.PersistentVolumeClaim{provisionedPVC, provisionedPVCHigherVersion},
			expectedPVCs:    []*v1.PersistentVolumeClaim{addProvisionAnn(provisionedPVC), provisionedPVC2},
			expectedAPIPVCs: []*v1.PersistentVolumeClaim{addProvisionAnn(provisionedPVC), provisionedPVCHigherVersion},
			shouldFail:      true,
		},
	}

	run := func(t *testing.T, scenario scenarioType) {
		_, ctx := ktesting.NewTestContext(t)
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()

		// Setup
		testEnv := newTestBinder(t, ctx)
		pod := makePod("test-pod").
			withNamespace("testns").
			withNodeName("node1").Pod
		if scenario.apiPVs == nil {
			scenario.apiPVs = scenario.cachedPVs
		}
		if scenario.apiPVCs == nil {
			scenario.apiPVCs = scenario.cachedPVCs
		}
		testEnv.initVolumes(scenario.cachedPVs, scenario.apiPVs)
		testEnv.initClaims(scenario.cachedPVCs, scenario.apiPVCs)
		testEnv.assumeVolumes(t, "node1", pod, scenario.bindings, scenario.provisionedPVCs)

		// Execute
		err := testEnv.internalBinder.bindAPIUpdate(ctx, pod, scenario.bindings, scenario.provisionedPVCs)

		// Validate
		if !scenario.shouldFail && err != nil {
			t.Errorf("returned error: %v", err)
		}
		if scenario.shouldFail && err == nil {
			t.Error("returned success but expected error")
		}
		if scenario.expectedAPIPVs == nil {
			scenario.expectedAPIPVs = scenario.expectedPVs
		}
		if scenario.expectedAPIPVCs == nil {
			scenario.expectedAPIPVCs = scenario.expectedPVCs
		}
		testEnv.validateBind(t, pod, scenario.expectedPVs, scenario.expectedAPIPVs)
		testEnv.validateProvision(t, pod, scenario.expectedPVCs, scenario.expectedAPIPVCs)
	}

	for name, scenario := range scenarios {
		t.Run(name, func(t *testing.T) { run(t, scenario) })
	}
}

func TestCheckBindings(t *testing.T) {
	t.Parallel()

	type scenarioType struct {
		// Inputs
		initPVs  []*v1.PersistentVolume
		initPVCs []*v1.PersistentVolumeClaim

		bindings        []*BindingInfo
		provisionedPVCs []*v1.PersistentVolumeClaim

		// api updates before checking
		apiPVs  []*v1.PersistentVolume
		apiPVCs []*v1.PersistentVolumeClaim

		// delete objects before checking
		deletePVs  bool
		deletePVCs bool

		// Expected return values
		shouldFail    bool
		expectedBound bool
	}
	scenarios := map[string]scenarioType{
		"nothing-to-bind-nil": {
			shouldFail: true,
		},
		"nothing-to-bind-bindings-nil": {
			provisionedPVCs: []*v1.PersistentVolumeClaim{},
			shouldFail:      true,
		},
		"nothing-to-bind-provisionings-nil": {
			bindings:   []*BindingInfo{},
			shouldFail: true,
		},
		"nothing-to-bind": {
			bindings:        []*BindingInfo{},
			provisionedPVCs: []*v1.PersistentVolumeClaim{},
			expectedBound:   true,
		},
		"binding-bound": {
			bindings:        []*BindingInfo{makeBinding(unboundPVC, pvNode1aBound)},
			provisionedPVCs: []*v1.PersistentVolumeClaim{},
			initPVs:         []*v1.PersistentVolume{pvNode1aBound},
			initPVCs:        []*v1.PersistentVolumeClaim{boundPVCNode1a},
			expectedBound:   true,
		},
		"binding-prebound": {
			bindings:        []*BindingInfo{makeBinding(unboundPVC, pvNode1aBound)},
			provisionedPVCs: []*v1.PersistentVolumeClaim{},
			initPVs:         []*v1.PersistentVolume{pvNode1aBound},
			initPVCs:        []*v1.PersistentVolumeClaim{preboundPVCNode1a},
		},
		"binding-unbound": {
			bindings:        []*BindingInfo{makeBinding(unboundPVC, pvNode1aBound)},
			provisionedPVCs: []*v1.PersistentVolumeClaim{},
			initPVs:         []*v1.PersistentVolume{pvNode1aBound},
			initPVCs:        []*v1.PersistentVolumeClaim{unboundPVC},
		},
		"binding-pvc-not-exists": {
			bindings:        []*BindingInfo{makeBinding(unboundPVC, pvNode1aBound)},
			provisionedPVCs: []*v1.PersistentVolumeClaim{},
			initPVs:         []*v1.PersistentVolume{pvNode1aBound},
			shouldFail:      true,
		},
		"binding-pv-not-exists": {
			bindings:        []*BindingInfo{makeBinding(unboundPVC, pvNode1aBound)},
			provisionedPVCs: []*v1.PersistentVolumeClaim{},
			initPVs:         []*v1.PersistentVolume{pvNode1aBound},
			initPVCs:        []*v1.PersistentVolumeClaim{boundPVCNode1a},
			deletePVs:       true,
			shouldFail:      true,
		},
		"binding-claimref-nil": {
			bindings:        []*BindingInfo{makeBinding(unboundPVC, pvNode1aBound)},
			provisionedPVCs: []*v1.PersistentVolumeClaim{},
			initPVs:         []*v1.PersistentVolume{pvNode1a},
			initPVCs:        []*v1.PersistentVolumeClaim{boundPVCNode1a},
			apiPVs:          []*v1.PersistentVolume{pvNode1a},
			apiPVCs:         []*v1.PersistentVolumeClaim{boundPVCNode1a},
			shouldFail:      true,
		},
		"binding-claimref-uid-empty": {
			bindings:        []*BindingInfo{makeBinding(unboundPVC, pvNode1aBound)},
			provisionedPVCs: []*v1.PersistentVolumeClaim{},
			initPVs:         []*v1.PersistentVolume{pvNode1aBound},
			initPVCs:        []*v1.PersistentVolumeClaim{boundPVCNode1a},
			apiPVs:          []*v1.PersistentVolume{pvRemoveClaimUID(pvNode1aBound)},
			apiPVCs:         []*v1.PersistentVolumeClaim{boundPVCNode1a},
			shouldFail:      true,
		},
		"binding-one-bound,one-unbound": {
			bindings:        []*BindingInfo{makeBinding(unboundPVC, pvNode1aBound), makeBinding(unboundPVC2, pvNode1bBound)},
			provisionedPVCs: []*v1.PersistentVolumeClaim{},
			initPVs:         []*v1.PersistentVolume{pvNode1aBound, pvNode1bBound},
			initPVCs:        []*v1.PersistentVolumeClaim{boundPVCNode1a, unboundPVC2},
		},
		"provisioning-pvc-bound": {
			bindings:        []*BindingInfo{},
			provisionedPVCs: []*v1.PersistentVolumeClaim{addProvisionAnn(provisionedPVC)},
			initPVs:         []*v1.PersistentVolume{pvBound},
			initPVCs:        []*v1.PersistentVolumeClaim{provisionedPVCBound},
			apiPVCs:         []*v1.PersistentVolumeClaim{addProvisionAnn(provisionedPVCBound)},
			expectedBound:   true,
		},
		"provisioning-pvc-unbound": {
			bindings:        []*BindingInfo{},
			provisionedPVCs: []*v1.PersistentVolumeClaim{addProvisionAnn(provisionedPVC)},
			initPVCs:        []*v1.PersistentVolumeClaim{addProvisionAnn(provisionedPVC)},
		},
		"provisioning-pvc-not-exists": {
			bindings:        []*BindingInfo{},
			provisionedPVCs: []*v1.PersistentVolumeClaim{addProvisionAnn(provisionedPVC)},
			initPVCs:        []*v1.PersistentVolumeClaim{provisionedPVC},
			deletePVCs:      true,
			shouldFail:      true,
		},
		"provisioning-pvc-annotations-nil": {
			bindings:        []*BindingInfo{},
			provisionedPVCs: []*v1.PersistentVolumeClaim{addProvisionAnn(provisionedPVC)},
			initPVCs:        []*v1.PersistentVolumeClaim{provisionedPVC},
			apiPVCs:         []*v1.PersistentVolumeClaim{provisionedPVC},
			shouldFail:      true,
		},
		"provisioning-pvc-selected-node-dropped": {
			bindings:        []*BindingInfo{},
			provisionedPVCs: []*v1.PersistentVolumeClaim{addProvisionAnn(provisionedPVC)},
			initPVCs:        []*v1.PersistentVolumeClaim{provisionedPVC},
			apiPVCs:         []*v1.PersistentVolumeClaim{pvcSetEmptyAnnotations(provisionedPVC)},
			shouldFail:      true,
		},
		"provisioning-pvc-selected-node-wrong-node": {
			initPVCs:        []*v1.PersistentVolumeClaim{provisionedPVC},
			bindings:        []*BindingInfo{},
			provisionedPVCs: []*v1.PersistentVolumeClaim{addProvisionAnn(provisionedPVC)},
			apiPVCs:         []*v1.PersistentVolumeClaim{pvcSetSelectedNode(provisionedPVC, "wrong-node")},
			shouldFail:      true,
		},
		"binding-bound-provisioning-unbound": {
			bindings:        []*BindingInfo{makeBinding(unboundPVC, pvNode1aBound)},
			provisionedPVCs: []*v1.PersistentVolumeClaim{addProvisionAnn(provisionedPVC)},
			initPVs:         []*v1.PersistentVolume{pvNode1aBound},
			initPVCs:        []*v1.PersistentVolumeClaim{boundPVCNode1a, addProvisionAnn(provisionedPVC)},
		},
		"tolerate-provisioning-pvc-bound-pv-not-found": {
			initPVs:         []*v1.PersistentVolume{pvNode1a},
			initPVCs:        []*v1.PersistentVolumeClaim{provisionedPVC},
			bindings:        []*BindingInfo{},
			provisionedPVCs: []*v1.PersistentVolumeClaim{addProvisionAnn(provisionedPVC)},
			apiPVCs:         []*v1.PersistentVolumeClaim{addProvisionAnn(provisionedPVCBound)},
			deletePVs:       true,
		},
	}

	run := func(t *testing.T, scenario scenarioType) {
		logger, ctx := ktesting.NewTestContext(t)
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()
		// Setup
		pod := makePod("test-pod").
			withNamespace("testns").
			withNodeName("node1").Pod
		testEnv := newTestBinder(t, ctx)
		testEnv.internalPodInformer.Informer().GetIndexer().Add(pod)
		testEnv.initNodes([]*v1.Node{node1})
		testEnv.initVolumes(scenario.initPVs, nil)
		testEnv.initClaims(scenario.initPVCs, nil)
		testEnv.assumeVolumes(t, "node1", pod, scenario.bindings, scenario.provisionedPVCs)

		// Before execute
		if scenario.deletePVs {
			testEnv.deleteVolumes(scenario.initPVs)
		} else {
			if err := testEnv.updateVolumes(ctx, scenario.apiPVs); err != nil {
				t.Errorf("Failed to update PVs: %v", err)
			}
		}
		if scenario.deletePVCs {
			testEnv.deleteClaims(scenario.initPVCs)
		} else {
			if err := testEnv.updateClaims(ctx, scenario.apiPVCs); err != nil {
				t.Errorf("Failed to update PVCs: %v", err)
			}
		}

		// Execute
		allBound, err := testEnv.internalBinder.checkBindings(logger, pod, scenario.bindings, scenario.provisionedPVCs)

		// Validate
		if !scenario.shouldFail && err != nil {
			t.Errorf("returned error: %v", err)
		}
		if scenario.shouldFail && err == nil {
			t.Error("returned success but expected error")
		}
		if scenario.expectedBound != allBound {
			t.Errorf("returned bound %v", allBound)
		}
	}

	for name, scenario := range scenarios {
		t.Run(name, func(t *testing.T) { run(t, scenario) })
	}
}

func TestCheckBindingsWithCSIMigration(t *testing.T) {
	t.Parallel()

	type scenarioType struct {
		// Inputs
		initPVs      []*v1.PersistentVolume
		initPVCs     []*v1.PersistentVolumeClaim
		initNodes    []*v1.Node
		initCSINodes []*storagev1.CSINode

		bindings        []*BindingInfo
		provisionedPVCs []*v1.PersistentVolumeClaim

		// API updates before checking
		apiPVs  []*v1.PersistentVolume
		apiPVCs []*v1.PersistentVolumeClaim

		// Expected return values
		shouldFail    bool
		expectedBound bool
	}
	scenarios := map[string]scenarioType{
		"provisioning-pvc-bound": {
			bindings:        []*BindingInfo{},
			provisionedPVCs: []*v1.PersistentVolumeClaim{addProvisionAnn(provMigrationPVCBound)},
			initPVs:         []*v1.PersistentVolume{migrationPVBound},
			initPVCs:        []*v1.PersistentVolumeClaim{provMigrationPVCBound},
			initNodes:       []*v1.Node{node1Zone1},
			initCSINodes:    []*storagev1.CSINode{csiNode1Migrated},
			apiPVCs:         []*v1.PersistentVolumeClaim{addProvisionAnn(provMigrationPVCBound)},
			expectedBound:   true,
		},
		"binding-node-pv-same-zone": {
			bindings:        []*BindingInfo{makeBinding(unboundPVC, migrationPVBoundToUnbound)},
			provisionedPVCs: []*v1.PersistentVolumeClaim{},
			initPVs:         []*v1.PersistentVolume{migrationPVBoundToUnbound},
			initPVCs:        []*v1.PersistentVolumeClaim{unboundPVC},
			initNodes:       []*v1.Node{node1Zone1},
			initCSINodes:    []*storagev1.CSINode{csiNode1Migrated},
		},
		"binding-without-csinode": {
			bindings:        []*BindingInfo{makeBinding(unboundPVC, migrationPVBoundToUnbound)},
			provisionedPVCs: []*v1.PersistentVolumeClaim{},
			initPVs:         []*v1.PersistentVolume{migrationPVBoundToUnbound},
			initPVCs:        []*v1.PersistentVolumeClaim{unboundPVC},
			initNodes:       []*v1.Node{node1Zone1},
			initCSINodes:    []*storagev1.CSINode{},
		},
		"binding-non-migrated-plugin": {
			bindings:        []*BindingInfo{makeBinding(unboundPVC, migrationPVBoundToUnbound)},
			provisionedPVCs: []*v1.PersistentVolumeClaim{},
			initPVs:         []*v1.PersistentVolume{migrationPVBoundToUnbound},
			initPVCs:        []*v1.PersistentVolumeClaim{unboundPVC},
			initNodes:       []*v1.Node{node1Zone1},
			initCSINodes:    []*storagev1.CSINode{csiNode1NotMigrated},
		},
		"binding-node-pv-in-different-zones": {
			bindings:        []*BindingInfo{makeBinding(unboundPVC, migrationPVBoundToUnbound)},
			provisionedPVCs: []*v1.PersistentVolumeClaim{},
			initPVs:         []*v1.PersistentVolume{migrationPVBoundToUnbound},
			initPVCs:        []*v1.PersistentVolumeClaim{unboundPVC},
			initNodes:       []*v1.Node{node1Zone2},
			initCSINodes:    []*storagev1.CSINode{csiNode1Migrated},
			shouldFail:      true,
		},
		"binding-node-pv-different-zones-migration-off": {
			bindings:        []*BindingInfo{makeBinding(unboundPVC, nonmigrationPVBoundToUnbound)},
			provisionedPVCs: []*v1.PersistentVolumeClaim{},
			initPVs:         []*v1.PersistentVolume{nonmigrationPVBoundToUnbound},
			initPVCs:        []*v1.PersistentVolumeClaim{unboundPVC},
			initNodes:       []*v1.Node{node1Zone2},
			initCSINodes:    []*storagev1.CSINode{csiNode1Migrated},
		},
	}

	run := func(t *testing.T, scenario scenarioType) {
		logger, ctx := ktesting.NewTestContext(t)
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()

		// Setup
		pod := makePod("test-pod").
			withNamespace("testns").
			withNodeName("node1").Pod
		testEnv := newTestBinder(t, ctx)
		testEnv.internalPodInformer.Informer().GetIndexer().Add(pod)
		testEnv.initNodes(scenario.initNodes)
		testEnv.initCSINodes(scenario.initCSINodes)
		testEnv.initVolumes(scenario.initPVs, nil)
		testEnv.initClaims(scenario.initPVCs, nil)
		testEnv.assumeVolumes(t, "node1", pod, scenario.bindings, scenario.provisionedPVCs)

		// Before execute
		if err := testEnv.updateVolumes(ctx, scenario.apiPVs); err != nil {
			t.Errorf("Failed to update PVs: %v", err)
		}
		if err := testEnv.updateClaims(ctx, scenario.apiPVCs); err != nil {
			t.Errorf("Failed to update PVCs: %v", err)
		}

		// Execute
		allBound, err := testEnv.internalBinder.checkBindings(logger, pod, scenario.bindings, scenario.provisionedPVCs)

		// Validate
		if !scenario.shouldFail && err != nil {
			t.Errorf("returned error: %v", err)
		}
		if scenario.shouldFail && err == nil {
			t.Error("returned success but expected error")
		}
		if scenario.expectedBound != allBound {
			t.Errorf("returned bound %v", allBound)
		}
	}

	for name, scenario := range scenarios {
		t.Run(name, func(t *testing.T) { run(t, scenario) })
	}
}

func TestBindPodVolumes(t *testing.T) {
	t.Parallel()

	type scenarioType struct {
		// Inputs
		bindingsNil bool // Pass in nil bindings slice

		nodes []*v1.Node

		// before assume
		initPVs  []*v1.PersistentVolume
		initPVCs []*v1.PersistentVolumeClaim

		// assume PV & PVC with these binding results
		binding          *BindingInfo
		claimToProvision *v1.PersistentVolumeClaim

		// API updates after assume before bind
		apiPV  *v1.PersistentVolume
		apiPVC *v1.PersistentVolumeClaim

		// This function runs with a delay of 5 seconds
		delayFunc func(t *testing.T, ctx context.Context, testEnv *testEnv, pod *v1.Pod, pvs []*v1.PersistentVolume, pvcs []*v1.PersistentVolumeClaim)

		// Expected return values
		shouldFail bool
	}
	scenarios := map[string]scenarioType{
		"nothing-to-bind-nil": {
			bindingsNil: true,
			shouldFail:  true,
		},
		"nothing-to-bind-empty": {},
		"already-bound": {
			binding:  makeBinding(unboundPVC, pvNode1aBound),
			initPVs:  []*v1.PersistentVolume{pvNode1aBound},
			initPVCs: []*v1.PersistentVolumeClaim{boundPVCNode1a},
		},
		"binding-static-pv-succeeds-after-time": {
			initPVs:    []*v1.PersistentVolume{pvNode1a},
			initPVCs:   []*v1.PersistentVolumeClaim{unboundPVC},
			binding:    makeBinding(unboundPVC, pvNode1aBound),
			shouldFail: false, // Will succeed after PVC is fully bound to this PV by pv controller.
			delayFunc: func(t *testing.T, ctx context.Context, testEnv *testEnv, pod *v1.Pod, pvs []*v1.PersistentVolume, pvcs []*v1.PersistentVolumeClaim) {
				pvc := pvcs[0]
				pv := pvs[0]
				// Update PVC to be fully bound to PV
				newPVC := pvc.DeepCopy()
				newPVC.Spec.VolumeName = pv.Name
				metav1.SetMetaDataAnnotation(&newPVC.ObjectMeta, volume.AnnBindCompleted, "yes")
				if _, err := testEnv.client.CoreV1().PersistentVolumeClaims(newPVC.Namespace).Update(ctx, newPVC, metav1.UpdateOptions{}); err != nil {
					t.Errorf("failed to update PVC %q: %v", newPVC.Name, err)
				}
			},
		},
		"binding-dynamic-pv-succeeds-after-time": {
			claimToProvision: pvcSetSelectedNode(provisionedPVC, "node1"),
			initPVCs:         []*v1.PersistentVolumeClaim{provisionedPVC},
			delayFunc: func(t *testing.T, ctx context.Context, testEnv *testEnv, pod *v1.Pod, pvs []*v1.PersistentVolume, pvcs []*v1.PersistentVolumeClaim) {
				pvc := pvcs[0]
				// Update PVC to be fully bound to PV
				newPVC, err := testEnv.client.CoreV1().PersistentVolumeClaims(pvc.Namespace).Get(ctx, pvc.Name, metav1.GetOptions{})
				if err != nil {
					t.Errorf("failed to get PVC %q: %v", pvc.Name, err)
					return
				}
				dynamicPV := makeTestPV("dynamic-pv", "node1", "1G", "1", newPVC, waitClass)
				dynamicPV, err = testEnv.client.CoreV1().PersistentVolumes().Create(ctx, dynamicPV, metav1.CreateOptions{})
				if err != nil {
					t.Errorf("failed to create PV %q: %v", dynamicPV.Name, err)
					return
				}
				newPVC.Spec.VolumeName = dynamicPV.Name
				metav1.SetMetaDataAnnotation(&newPVC.ObjectMeta, volume.AnnBindCompleted, "yes")
				if _, err := testEnv.client.CoreV1().PersistentVolumeClaims(newPVC.Namespace).Update(ctx, newPVC, metav1.UpdateOptions{}); err != nil {
					t.Errorf("failed to update PVC %q: %v", newPVC.Name, err)
				}
			},
		},
		"bound-by-pv-controller-before-bind": {
			initPVs:    []*v1.PersistentVolume{pvNode1a},
			initPVCs:   []*v1.PersistentVolumeClaim{unboundPVC},
			binding:    makeBinding(unboundPVC, pvNode1aBound),
			apiPV:      pvNode1aBound,
			apiPVC:     boundPVCNode1a,
			shouldFail: true, // bindAPIUpdate will fail because API conflict
		},
		"pod-deleted-after-time": {
			binding:  makeBinding(unboundPVC, pvNode1aBound),
			initPVs:  []*v1.PersistentVolume{pvNode1a},
			initPVCs: []*v1.PersistentVolumeClaim{unboundPVC},
			delayFunc: func(t *testing.T, ctx context.Context, testEnv *testEnv, pod *v1.Pod, pvs []*v1.PersistentVolume, pvcs []*v1.PersistentVolumeClaim) {
				testEnv.client.CoreV1().Pods(pod.Namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{})
			},
			shouldFail: true,
		},
		"binding-times-out": {
			binding:    makeBinding(unboundPVC, pvNode1aBound),
			initPVs:    []*v1.PersistentVolume{pvNode1a},
			initPVCs:   []*v1.PersistentVolumeClaim{unboundPVC},
			shouldFail: true,
		},
		"binding-fails": {
			binding:    makeBinding(unboundPVC2, pvNode1bBound),
			initPVs:    []*v1.PersistentVolume{pvNode1b},
			initPVCs:   []*v1.PersistentVolumeClaim{unboundPVC2},
			shouldFail: true,
		},
		"check-fails": {
			binding:  makeBinding(unboundPVC, pvNode1aBound),
			initPVs:  []*v1.PersistentVolume{pvNode1a},
			initPVCs: []*v1.PersistentVolumeClaim{unboundPVC},
			delayFunc: func(t *testing.T, ctx context.Context, testEnv *testEnv, pod *v1.Pod, pvs []*v1.PersistentVolume, pvcs []*v1.PersistentVolumeClaim) {
				pvc := pvcs[0]
				// Delete PVC will fail check
				if err := testEnv.client.CoreV1().PersistentVolumeClaims(pvc.Namespace).Delete(ctx, pvc.Name, metav1.DeleteOptions{}); err != nil {
					t.Errorf("failed to delete PVC %q: %v", pvc.Name, err)
				}
			},
			shouldFail: true,
		},
		"node-affinity-fails": {
			binding:    makeBinding(unboundPVC, pvNode1aBound),
			initPVs:    []*v1.PersistentVolume{pvNode1aBound},
			initPVCs:   []*v1.PersistentVolumeClaim{boundPVCNode1a},
			nodes:      []*v1.Node{node1NoLabels},
			shouldFail: true,
		},
		"node-affinity-fails-dynamic-provisioning": {
			initPVs:          []*v1.PersistentVolume{pvNode1a, pvNode2},
			initPVCs:         []*v1.PersistentVolumeClaim{selectedNodePVC},
			claimToProvision: selectedNodePVC,
			nodes:            []*v1.Node{node1, node2},
			delayFunc: func(t *testing.T, ctx context.Context, testEnv *testEnv, pod *v1.Pod, pvs []*v1.PersistentVolume, pvcs []*v1.PersistentVolumeClaim) {
				// Update PVC to be fully bound to a PV with a different node
				newPVC := pvcs[0].DeepCopy()
				newPVC.Spec.VolumeName = pvNode2.Name
				metav1.SetMetaDataAnnotation(&newPVC.ObjectMeta, volume.AnnBindCompleted, "yes")
				if _, err := testEnv.client.CoreV1().PersistentVolumeClaims(newPVC.Namespace).Update(ctx, newPVC, metav1.UpdateOptions{}); err != nil {
					t.Errorf("failed to update PVC %q: %v", newPVC.Name, err)
				}
			},
			shouldFail: true,
		},
	}

	run := func(t *testing.T, scenario scenarioType) {
		logger, ctx := ktesting.NewTestContext(t)
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()
		// Setup
		pod := makePod("test-pod").
			withNamespace("testns").
			withNodeName("node1").Pod
		testEnv := newTestBinder(t, ctx)
		testEnv.internalPodInformer.Informer().GetIndexer().Add(pod)
		if scenario.nodes == nil {
			scenario.nodes = []*v1.Node{node1}
		}
		bindings := []*BindingInfo{}
		claimsToProvision := []*v1.PersistentVolumeClaim{}
		if !scenario.bindingsNil {
			if scenario.binding != nil {
				bindings = []*BindingInfo{scenario.binding}
			}
			if scenario.claimToProvision != nil {
				claimsToProvision = []*v1.PersistentVolumeClaim{scenario.claimToProvision}
			}
			testEnv.initNodes(scenario.nodes)
			testEnv.initVolumes(scenario.initPVs, scenario.initPVs)
			testEnv.initClaims(scenario.initPVCs, scenario.initPVCs)
			testEnv.assumeVolumes(t, "node1", pod, bindings, claimsToProvision)
		}

		// Before Execute
		if scenario.apiPV != nil {
			_, err := testEnv.client.CoreV1().PersistentVolumes().Update(ctx, scenario.apiPV, metav1.UpdateOptions{})
			if err != nil {
				t.Fatalf("failed to update PV %q", scenario.apiPV.Name)
			}
		}
		if scenario.apiPVC != nil {
			_, err := testEnv.client.CoreV1().PersistentVolumeClaims(scenario.apiPVC.Namespace).Update(ctx, scenario.apiPVC, metav1.UpdateOptions{})
			if err != nil {
				t.Fatalf("failed to update PVC %q", getPVCName(scenario.apiPVC))
			}
		}

		if scenario.delayFunc != nil {
			go func(scenario scenarioType) {
				time.Sleep(5 * time.Second)
				// Sleep a while to run after bindAPIUpdate in BindPodVolumes
				logger.V(5).Info("Running delay function")
				scenario.delayFunc(t, ctx, testEnv, pod, scenario.initPVs, scenario.initPVCs)
			}(scenario)
		}

		// Execute
		dynamicProvisions := []*DynamicProvision{}
		for _, claim := range claimsToProvision {
			dynamicProvisions = append(dynamicProvisions, &DynamicProvision{PVC: claim})
		}
		podVolumes := &PodVolumes{
			StaticBindings:    bindings,
			DynamicProvisions: dynamicProvisions,
		}
		err := testEnv.binder.BindPodVolumes(ctx, pod, podVolumes)

		// Validate
		if !scenario.shouldFail && err != nil {
			t.Errorf("returned error: %v", err)
		}
		if scenario.shouldFail && err == nil {
			t.Error("returned success but expected error")
		}
	}

	for name, scenario := range scenarios {
		scenario := scenario
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			run(t, scenario)
		})
	}
}

func TestFindAssumeVolumes(t *testing.T) {
	// Test case
	podPVCs := []*v1.PersistentVolumeClaim{unboundPVC}
	pvs := []*v1.PersistentVolume{pvNode2, pvNode1a, pvNode1c}

	// Setup
	logger, ctx := ktesting.NewTestContext(t)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	testEnv := newTestBinder(t, ctx)
	testEnv.initVolumes(pvs, pvs)
	testEnv.initClaims(podPVCs, podPVCs)
	pod := makePod("test-pod").
		withNamespace("testns").
		withNodeName("node1").
		withPVCSVolume(podPVCs).Pod

	testNode := &v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "node1",
			Labels: map[string]string{
				nodeLabelKey: "node1",
			},
		},
	}

	// Execute
	// 1. Find matching PVs
	podVolumes, reasons, err := findPodVolumes(logger, testEnv.binder, pod, testNode)
	if err != nil {
		t.Errorf("Test failed: FindPodVolumes returned error: %v", err)
	}
	if len(reasons) > 0 {
		t.Errorf("Test failed: couldn't find PVs for all PVCs: %v", reasons)
	}
	expectedBindings := podVolumes.StaticBindings

	// 2. Assume matches
	allBound, err := testEnv.binder.AssumePodVolumes(logger, pod, testNode.Name, podVolumes)
	if err != nil {
		t.Errorf("Test failed: AssumePodVolumes returned error: %v", err)
	}
	if allBound {
		t.Errorf("Test failed: detected unbound volumes as bound")
	}
	testEnv.validateAssume(t, pod, expectedBindings, nil)

	// After assume, claimref should be set on pv
	expectedBindings = podVolumes.StaticBindings

	// 3. Find matching PVs again
	// This should always return the original chosen pv
	// Run this many times in case sorting returns different orders for the two PVs.
	for i := 0; i < 50; i++ {
		podVolumes, reasons, err := findPodVolumes(logger, testEnv.binder, pod, testNode)
		if err != nil {
			t.Errorf("Test failed: FindPodVolumes returned error: %v", err)
		}
		if len(reasons) > 0 {
			t.Errorf("Test failed: couldn't find PVs for all PVCs: %v", reasons)
		}
		testEnv.validatePodCache(t, testNode.Name, pod, podVolumes, expectedBindings, nil)
	}
}

// TestCapacity covers different scenarios involving CSIStorageCapacity objects.
// Scenarios without those are covered by TestFindPodVolumesWithProvisioning.
func TestCapacity(t *testing.T) {
	type scenarioType struct {
		// Inputs
		pvcs       []*v1.PersistentVolumeClaim
		capacities []*storagev1.CSIStorageCapacity

		// Expected return values
		expectedProvisions []*DynamicProvision
		reasons            ConflictReasons
		shouldFail         bool
	}
	scenarios := map[string]scenarioType{
		"network-attached": {
			pvcs: []*v1.PersistentVolumeClaim{provisionedPVC},
			capacities: []*storagev1.CSIStorageCapacity{
				networkAttachedCapacity,
			},
			expectedProvisions: []*DynamicProvision{{
				PVC:      provisionedPVC,
				Capacity: networkAttachedCapacity,
			}},
		},
		"local-storage": {
			pvcs: []*v1.PersistentVolumeClaim{provisionedPVC},
			capacities: []*storagev1.CSIStorageCapacity{
				node1Capacity,
			},
			expectedProvisions: []*DynamicProvision{{
				PVC:      provisionedPVC,
				Capacity: node1Capacity,
			}},
		},
		"multiple": {
			pvcs: []*v1.PersistentVolumeClaim{provisionedPVC},
			capacities: []*storagev1.CSIStorageCapacity{
				networkAttachedCapacity,
				node2Capacity,
				node1Capacity,
			},
			expectedProvisions: []*DynamicProvision{{
				PVC:      provisionedPVC,
				Capacity: node1Capacity,
			}},
		},
		"no-storage": {
			pvcs:    []*v1.PersistentVolumeClaim{provisionedPVC},
			reasons: ConflictReasons{ErrReasonNotEnoughSpace},
		},
		"wrong-node": {
			pvcs: []*v1.PersistentVolumeClaim{provisionedPVC},
			capacities: []*storagev1.CSIStorageCapacity{
				makeCapacity("net", waitClassWithProvisioner, node2, "1Gi", ""),
			},
			reasons: ConflictReasons{ErrReasonNotEnoughSpace},
		},
		"wrong-storage-class": {
			pvcs: []*v1.PersistentVolumeClaim{provisionedPVC},
			capacities: []*storagev1.CSIStorageCapacity{
				makeCapacity("net", waitClass, node1, "1Gi", ""),
			},
			reasons: ConflictReasons{ErrReasonNotEnoughSpace},
		},
		"insufficient-storage": {
			pvcs: []*v1.PersistentVolumeClaim{provisionedPVC},
			capacities: []*storagev1.CSIStorageCapacity{
				makeCapacity("net", waitClassWithProvisioner, node1, "1Mi", ""),
			},
			reasons: ConflictReasons{ErrReasonNotEnoughSpace},
		},
		"insufficient-volume-size": {
			pvcs: []*v1.PersistentVolumeClaim{provisionedPVC},
			capacities: []*storagev1.CSIStorageCapacity{
				makeCapacity("net", waitClassWithProvisioner, node1, "1Gi", "1Mi"),
			},
			reasons: ConflictReasons{ErrReasonNotEnoughSpace},
		},
		"zero-storage": {
			pvcs: []*v1.PersistentVolumeClaim{provisionedPVC},
			capacities: []*storagev1.CSIStorageCapacity{
				makeCapacity("net", waitClassWithProvisioner, node1, "0Mi", ""),
			},
			reasons: ConflictReasons{ErrReasonNotEnoughSpace},
		},
		"zero-volume-size": {
			pvcs: []*v1.PersistentVolumeClaim{provisionedPVC},
			capacities: []*storagev1.CSIStorageCapacity{
				makeCapacity("net", waitClassWithProvisioner, node1, "", "0Mi"),
			},
			reasons: ConflictReasons{ErrReasonNotEnoughSpace},
		},
		"nil-storage": {
			pvcs: []*v1.PersistentVolumeClaim{provisionedPVC},
			capacities: []*storagev1.CSIStorageCapacity{
				makeCapacity("net", waitClassWithProvisioner, node1, "", ""),
			},
			reasons: ConflictReasons{ErrReasonNotEnoughSpace},
		},
	}

	testNode := &v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "node1",
			Labels: map[string]string{
				nodeLabelKey: "node1",
			},
		},
	}

	run := func(t *testing.T, scenario scenarioType, optIn bool) {
		logger, ctx := ktesting.NewTestContext(t)
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()

		// Setup: the driver has the feature enabled, but the scheduler might not.
		testEnv := newTestBinder(t, ctx)
		testEnv.addCSIDriver(makeCSIDriver(provisioner, optIn))
		testEnv.addCSIStorageCapacities(scenario.capacities)

		// a. Init pvc cache
		testEnv.initClaims(scenario.pvcs, scenario.pvcs)

		// b. Generate pod with given claims
		pod := makePod("test-pod").
			withNamespace("testns").
			withNodeName("node1").
			withPVCSVolume(scenario.pvcs).Pod

		// Execute
		podVolumes, reasons, err := findPodVolumes(logger, testEnv.binder, pod, testNode)

		// Validate
		shouldFail := scenario.shouldFail
		expectedReasons := scenario.reasons
		if !optIn {
			shouldFail = false
			expectedReasons = nil
		}
		if !shouldFail && err != nil {
			t.Errorf("returned error: %v", err)
		}
		if shouldFail && err == nil {
			t.Error("returned success but expected error")
		}
		checkReasons(t, reasons, expectedReasons)
		expectedProvisions := scenario.expectedProvisions
		if !optIn {
			for i := 0; i < len(expectedProvisions); i++ {
				expectedProvisions[i].Capacity = nil
			}
		}
		if len(scenario.reasons) > 0 {
			expectedProvisions = podVolumes.DynamicProvisions
		}
		testEnv.validatePodCache(t, pod.Spec.NodeName, pod, podVolumes, nil, expectedProvisions)
	}

	yesNo := []bool{true, false}
	for _, optIn := range yesNo {
		name := fmt.Sprintf("CSIDriver.StorageCapacity=%v", optIn)
		t.Run(name, func(t *testing.T) {
			for name, scenario := range scenarios {
				t.Run(name, func(t *testing.T) { run(t, scenario, optIn) })
			}
		})
	}
}
