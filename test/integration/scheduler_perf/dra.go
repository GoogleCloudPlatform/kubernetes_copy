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

package benchmark

import (
	"fmt"
	"math/rand/v2"
	"path/filepath"
	"reflect"
	"sync"

	"github.com/stretchr/testify/require"

	v1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1beta1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/dynamic-resource-allocation/cel"
	"k8s.io/dynamic-resource-allocation/structured"
	"k8s.io/kubernetes/pkg/features"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/dynamicresources"
	"k8s.io/kubernetes/pkg/scheduler/util/assumecache"
	"k8s.io/kubernetes/test/utils/ktesting"
	"k8s.io/utils/ptr"
)

// createResourceClaimsOp defines an op where resource claims are created.
type createResourceClaimsOp struct {
	// Must be createResourceClaimsOpcode.
	Opcode operationCode
	// Number of claims to create. Parameterizable through CountParam.
	Count int
	// Template parameter for Count.
	CountParam string
	// Namespace the claims should be created in.
	Namespace string
	// Path to spec file describing the claims to create.
	TemplatePath string
}

var _ realOp = &createResourceClaimsOp{}
var _ runnableOp = &createResourceClaimsOp{}

func (op *createResourceClaimsOp) isValid(allowParameterization bool) error {
	if !isValidCount(allowParameterization, op.Count, op.CountParam) {
		return fmt.Errorf("invalid Count=%d / CountParam=%q", op.Count, op.CountParam)
	}
	if op.Namespace == "" {
		return fmt.Errorf("Namespace must be set")
	}
	if op.TemplatePath == "" {
		return fmt.Errorf("TemplatePath must be set")
	}
	return nil
}

func (op *createResourceClaimsOp) collectsMetrics() bool {
	return false
}
func (op *createResourceClaimsOp) patchParams(w *workload) (realOp, error) {
	if op.CountParam != "" {
		var err error
		op.Count, err = w.Params.get(op.CountParam[1:])
		if err != nil {
			return nil, err
		}
	}
	return op, op.isValid(false)
}

func (op *createResourceClaimsOp) requiredNamespaces() []string {
	return []string{op.Namespace}
}

func (op *createResourceClaimsOp) run(tCtx ktesting.TContext) {
	tCtx.Logf("creating %d claims in namespace %q", op.Count, op.Namespace)

	var claimTemplate *resourceapi.ResourceClaim
	if err := getSpecFromFile(&op.TemplatePath, &claimTemplate); err != nil {
		tCtx.Fatalf("parsing ResourceClaim %q: %v", op.TemplatePath, err)
	}
	var createErr error
	var mutex sync.Mutex
	create := func(i int) {
		err := func() error {
			if _, err := tCtx.Client().ResourceV1beta1().ResourceClaims(op.Namespace).Create(tCtx, claimTemplate.DeepCopy(), metav1.CreateOptions{}); err != nil {
				return fmt.Errorf("create claim: %v", err)
			}
			return nil
		}()
		if err != nil {
			mutex.Lock()
			defer mutex.Unlock()
			createErr = err
		}
	}

	workers := op.Count
	if workers > 30 {
		workers = 30
	}
	workqueue.ParallelizeUntil(tCtx, workers, op.Count, create)
	if createErr != nil {
		tCtx.Fatal(createErr.Error())
	}
}

// createResourceDriverOp defines an op where resource claims are created.
type createResourceDriverOp struct {
	// Must be createResourceDriverOpcode.
	Opcode operationCode
	// Name of the driver, used to reference it in a resource class.
	DriverName string
	// Number of claims to allow per node. Parameterizable through MaxClaimsPerNodeParam.
	MaxClaimsPerNode int
	// Template parameter for MaxClaimsPerNode.
	MaxClaimsPerNodeParam string
	// Nodes matching this glob pattern have resources managed by the driver.
	Nodes string
}

var _ realOp = &createResourceDriverOp{}
var _ runnableOp = &createResourceDriverOp{}

func (op *createResourceDriverOp) isValid(allowParameterization bool) error {
	if !isValidCount(allowParameterization, op.MaxClaimsPerNode, op.MaxClaimsPerNodeParam) {
		return fmt.Errorf("invalid MaxClaimsPerNode=%d / MaxClaimsPerNodeParam=%q", op.MaxClaimsPerNode, op.MaxClaimsPerNodeParam)
	}
	if op.DriverName == "" {
		return fmt.Errorf("DriverName must be set")
	}
	if op.Nodes == "" {
		return fmt.Errorf("Nodes must be set")
	}
	return nil
}

func (op *createResourceDriverOp) collectsMetrics() bool {
	return false
}
func (op *createResourceDriverOp) patchParams(w *workload) (realOp, error) {
	if op.MaxClaimsPerNodeParam != "" {
		var err error
		op.MaxClaimsPerNode, err = w.Params.get(op.MaxClaimsPerNodeParam[1:])
		if err != nil {
			return nil, err
		}
	}
	return op, op.isValid(false)
}

func (op *createResourceDriverOp) requiredNamespaces() []string { return nil }

func (op *createResourceDriverOp) run(tCtx ktesting.TContext) {
	tCtx.Logf("creating resource driver %q for nodes matching %q", op.DriverName, op.Nodes)

	var driverNodes []string

	nodes, err := tCtx.Client().CoreV1().Nodes().List(tCtx, metav1.ListOptions{})
	if err != nil {
		tCtx.Fatalf("list nodes: %v", err)
	}
	for _, node := range nodes.Items {
		match, err := filepath.Match(op.Nodes, node.Name)
		if err != nil {
			tCtx.Fatalf("matching glob pattern %q against node name %q: %v", op.Nodes, node.Name, err)
		}
		if match {
			driverNodes = append(driverNodes, node.Name)
		}
	}

	for _, nodeName := range driverNodes {
		slice := resourceSlice(op.DriverName, nodeName, op.MaxClaimsPerNode)
		_, err := tCtx.Client().ResourceV1beta1().ResourceSlices().Create(tCtx, slice, metav1.CreateOptions{})
		tCtx.ExpectNoError(err, "create node resource slice")
	}
	tCtx.CleanupCtx(func(tCtx ktesting.TContext) {
		err := tCtx.Client().ResourceV1beta1().ResourceSlices().DeleteCollection(tCtx,
			metav1.DeleteOptions{},
			metav1.ListOptions{FieldSelector: resourceapi.ResourceSliceSelectorDriver + "=" + op.DriverName},
		)
		tCtx.ExpectNoError(err, "delete node resource slices")
	})
}

func resourceSlice(driverName, nodeName string, capacity int) *resourceapi.ResourceSlice {
	slice := &resourceapi.ResourceSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name: nodeName,
		},

		Spec: resourceapi.ResourceSliceSpec{
			Driver:   driverName,
			NodeName: nodeName,
			Pool: resourceapi.ResourcePool{
				Name:               nodeName,
				ResourceSliceCount: 1,
			},
		},
	}

	for i := 0; i < capacity; i++ {
		slice.Spec.Devices = append(slice.Spec.Devices,
			resourceapi.Device{
				Name: fmt.Sprintf("instance-%d", i),
				Basic: &resourceapi.BasicDevice{
					Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
						"model":                {StringValue: ptr.To("A100")},
						"family":               {StringValue: ptr.To("GPU")},
						"driverVersion":        {VersionValue: ptr.To("1.2.3")},
						"dra.example.com/numa": {IntValue: ptr.To(int64(i))},
					},
					Capacity: map[resourceapi.QualifiedName]resourceapi.DeviceCapacity{
						"memory": {Value: resource.MustParse("1Gi")},
					},
				},
			},
		)
	}

	return slice
}

// allocResourceClaimsOp defines an op where resource claims with structured
// parameters get allocated without being associated with a pod.
type allocResourceClaimsOp struct {
	// Must be allocResourceClaimsOpcode.
	Opcode operationCode
	// Namespace where claims are to be allocated, all namespaces if empty.
	Namespace string
}

var _ realOp = &allocResourceClaimsOp{}
var _ runnableOp = &allocResourceClaimsOp{}

func (op *allocResourceClaimsOp) isValid(allowParameterization bool) error {
	return nil
}

func (op *allocResourceClaimsOp) collectsMetrics() bool {
	return false
}
func (op *allocResourceClaimsOp) patchParams(w *workload) (realOp, error) {
	return op, op.isValid(false)
}

func (op *allocResourceClaimsOp) requiredNamespaces() []string { return nil }

func (op *allocResourceClaimsOp) run(tCtx ktesting.TContext) {
	claims, err := tCtx.Client().ResourceV1beta1().ResourceClaims(op.Namespace).List(tCtx, metav1.ListOptions{})
	tCtx.ExpectNoError(err, "list claims")
	tCtx.Logf("allocating %d ResourceClaims", len(claims.Items))
	tCtx = ktesting.WithCancel(tCtx)
	defer tCtx.Cancel("allocResourceClaimsOp.run is done")

	// Track cluster state.
	informerFactory := informers.NewSharedInformerFactory(tCtx.Client(), 0)
	claimInformer := informerFactory.Resource().V1beta1().ResourceClaims().Informer()
	nodeLister := informerFactory.Core().V1().Nodes().Lister()
	draManager := dynamicresources.NewDRAManager(tCtx, assumecache.NewAssumeCache(tCtx.Logger(), claimInformer, "ResourceClaim", "", nil), informerFactory)
	informerFactory.Start(tCtx.Done())
	defer func() {
		tCtx.Cancel("allocResourceClaimsOp.run is shutting down")
		informerFactory.Shutdown()
	}()
	syncedInformers := informerFactory.WaitForCacheSync(tCtx.Done())
	expectSyncedInformers := map[reflect.Type]bool{
		reflect.TypeOf(&resourceapi.DeviceClass{}):   true,
		reflect.TypeOf(&resourceapi.ResourceClaim{}): true,
		reflect.TypeOf(&resourceapi.ResourceSlice{}): true,
		reflect.TypeOf(&v1.Node{}):                   true,
	}
	require.Equal(tCtx, expectSyncedInformers, syncedInformers, "synced informers")
	celCache := cel.NewCache(10)

	// The set of nodes is assumed to be fixed at this point.
	nodes, err := nodeLister.List(labels.Everything())
	tCtx.ExpectNoError(err, "list nodes")
	slices, err := draManager.ResourceSlices().List()
	tCtx.ExpectNoError(err, "list slices")

	// Allocate one claim at a time, picking nodes randomly. Each
	// allocation is stored immediately, using the claim cache to avoid
	// having to wait for the actual informer update.
claims:
	for i := range claims.Items {
		claim := &claims.Items[i]
		if claim.Status.Allocation != nil {
			continue
		}

		claims, err := draManager.ResourceClaims().List()
		tCtx.ExpectNoError(err, "list claims")
		allocatedDevices := sets.New[structured.DeviceID]()
		for _, claim := range claims {
			if claim.Status.Allocation == nil {
				continue
			}
			for _, result := range claim.Status.Allocation.Devices.Results {
				allocatedDevices.Insert(structured.MakeDeviceID(result.Driver, result.Pool, result.Device))
			}
		}

		allocator, err := structured.NewAllocator(tCtx, utilfeature.DefaultFeatureGate.Enabled(features.DRAAdminAccess), utilfeature.DefaultFeatureGate.Enabled(features.DRAPrioritizedList),
			[]*resourceapi.ResourceClaim{claim}, allocatedDevices, draManager.DeviceClasses(), slices, celCache)
		tCtx.ExpectNoError(err, "create allocator")

		rand.Shuffle(len(nodes), func(i, j int) {
			nodes[i], nodes[j] = nodes[j], nodes[i]
		})
		for _, node := range nodes {
			result, err := allocator.Allocate(tCtx, node)
			tCtx.ExpectNoError(err, "allocate claim")
			if result != nil {
				claim = claim.DeepCopy()
				claim.Status.Allocation = &result[0]
				claim, err := tCtx.Client().ResourceV1beta1().ResourceClaims(claim.Namespace).UpdateStatus(tCtx, claim, metav1.UpdateOptions{})
				tCtx.ExpectNoError(err, "update claim status with allocation")
				tCtx.ExpectNoError(draManager.ResourceClaims().AssumeClaimAfterAPICall(claim), "assume claim")
				continue claims
			}
		}
		tCtx.Fatalf("Could not allocate claim %d out of %d", i, len(claims))
	}
}
