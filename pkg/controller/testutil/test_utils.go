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

package testutil

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	"k8s.io/apimachinery/pkg/watch"
	v1apply "k8s.io/client-go/applyconfigurations/core/v1"
	"k8s.io/client-go/kubernetes/fake"
	v1core "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/cache"
	ref "k8s.io/client-go/tools/reference"
	utilnode "k8s.io/component-helpers/node/topology"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/api/legacyscheme"
	api "k8s.io/kubernetes/pkg/apis/core"
	"k8s.io/utils/clock"
	testingclock "k8s.io/utils/clock/testing"

	jsonpatch "github.com/evanphx/json-patch/v5"
)

var (
	keyFunc = cache.DeletionHandlingMetaNamespaceKeyFunc
)

// FakeNodeHandler is a fake implementation of NodesInterface and NodeInterface. It
// allows test cases to have fine-grained control over mock behaviors. We also need
// PodsInterface and PodInterface to test list & delete pods, which is implemented in
// the embedded client.Fake field.
type FakeNodeHandler struct {
	*fake.Clientset

	// Input: Hooks determine if request is valid or not
	CreateHook func(*FakeNodeHandler, *v1.Node) bool
	Existing   []*v1.Node
	AsyncCalls []func(*FakeNodeHandler)

	// Output
	CreatedNodes        []*v1.Node
	DeletedNodes        []*v1.Node
	UpdatedNodes        []*v1.Node
	UpdatedNodeStatuses []*v1.Node
	RequestCount        int

	// Synchronization
	lock           sync.Mutex
	DeleteWaitChan chan struct{}
	PatchWaitChan  chan struct{}
}

// FakeLegacyHandler is a fake implementation of CoreV1Interface.
type FakeLegacyHandler struct {
	v1core.CoreV1Interface
	n *FakeNodeHandler
}

// GetUpdatedNodesCopy returns a slice of Nodes with updates applied.
func (m *FakeNodeHandler) GetUpdatedNodesCopy() []*v1.Node {
	m.lock.Lock()
	defer m.lock.Unlock()
	updatedNodesCopy := make([]*v1.Node, len(m.UpdatedNodes), len(m.UpdatedNodes))
	copy(updatedNodesCopy, m.UpdatedNodes)
	return updatedNodesCopy
}

// Core returns fake CoreInterface.
func (m *FakeNodeHandler) Core() v1core.CoreV1Interface {
	return &FakeLegacyHandler{m.Clientset.CoreV1(), m}
}

// CoreV1 returns fake CoreV1Interface
func (m *FakeNodeHandler) CoreV1() v1core.CoreV1Interface {
	return &FakeLegacyHandler{m.Clientset.CoreV1(), m}
}

// Nodes return fake NodeInterfaces.
func (m *FakeLegacyHandler) Nodes() v1core.NodeInterface {
	return m.n
}

// Create adds a new Node to the fake store.
func (m *FakeNodeHandler) Create(_ context.Context, node *v1.Node, _ metav1.CreateOptions) (*v1.Node, error) {
	m.lock.Lock()
	defer func() {
		m.RequestCount++
		m.lock.Unlock()
	}()
	for _, n := range m.Existing {
		if n.Name == node.Name {
			return nil, apierrors.NewAlreadyExists(api.Resource("nodes"), node.Name)
		}
	}
	if m.CreateHook == nil || m.CreateHook(m, node) {
		nodeCopy := *node
		m.CreatedNodes = append(m.CreatedNodes, &nodeCopy)
		return node, nil
	}
	return nil, errors.New("create error")
}

// Get returns a Node from the fake store.
func (m *FakeNodeHandler) Get(ctx context.Context, name string, opts metav1.GetOptions) (*v1.Node, error) {
	m.lock.Lock()
	defer func() {
		m.RequestCount++
		m.runAsyncCalls()
		m.lock.Unlock()
	}()
	for i := range m.UpdatedNodes {
		if m.UpdatedNodes[i].Name == name {
			nodeCopy := *m.UpdatedNodes[i]
			return &nodeCopy, nil
		}
	}
	for i := range m.Existing {
		if m.Existing[i].Name == name {
			nodeCopy := *m.Existing[i]
			return &nodeCopy, nil
		}
	}
	return nil, apierrors.NewNotFound(schema.GroupResource{Resource: "nodes"}, name)
}

func (m *FakeNodeHandler) runAsyncCalls() {
	for _, a := range m.AsyncCalls {
		a(m)
	}
}

// List returns a list of Nodes from the fake store.
func (m *FakeNodeHandler) List(_ context.Context, opts metav1.ListOptions) (*v1.NodeList, error) {
	m.lock.Lock()
	defer func() {
		m.RequestCount++
		m.lock.Unlock()
	}()
	var nodes []*v1.Node
	for i := 0; i < len(m.UpdatedNodes); i++ {
		if !contains(m.UpdatedNodes[i], m.DeletedNodes) {
			nodes = append(nodes, m.UpdatedNodes[i])
		}
	}
	for i := 0; i < len(m.Existing); i++ {
		if !contains(m.Existing[i], m.DeletedNodes) && !contains(m.Existing[i], nodes) {
			nodes = append(nodes, m.Existing[i])
		}
	}
	for i := 0; i < len(m.CreatedNodes); i++ {
		if !contains(m.CreatedNodes[i], m.DeletedNodes) && !contains(m.CreatedNodes[i], nodes) {
			nodes = append(nodes, m.CreatedNodes[i])
		}
	}
	nodeList := &v1.NodeList{}
	for _, node := range nodes {
		nodeList.Items = append(nodeList.Items, *node)
	}
	return nodeList, nil
}

// Delete deletes a Node from the fake store.
func (m *FakeNodeHandler) Delete(_ context.Context, id string, opt metav1.DeleteOptions) error {
	m.lock.Lock()
	defer func() {
		m.RequestCount++
		if m.DeleteWaitChan != nil {
			m.DeleteWaitChan <- struct{}{}
		}
		m.lock.Unlock()
	}()
	m.DeletedNodes = append(m.DeletedNodes, NewNode(id))
	return nil
}

// DeleteCollection deletes a collection of Nodes from the fake store.
func (m *FakeNodeHandler) DeleteCollection(_ context.Context, opt metav1.DeleteOptions, listOpts metav1.ListOptions) error {
	return nil
}

// Update updates a Node in the fake store.
func (m *FakeNodeHandler) Update(_ context.Context, node *v1.Node, _ metav1.UpdateOptions) (*v1.Node, error) {
	m.lock.Lock()
	defer func() {
		m.RequestCount++
		m.lock.Unlock()
	}()

	nodeCopy := *node
	for i, updateNode := range m.UpdatedNodes {
		if updateNode.Name == nodeCopy.Name {
			if updateNode.GetObjectMeta().GetResourceVersion() != nodeCopy.GetObjectMeta().GetResourceVersion() {
				return nil, apierrors.NewConflict(schema.GroupResource{}, "fake conflict", nil)
			}
			m.UpdatedNodes[i] = &nodeCopy
			return node, nil
		}
	}
	m.UpdatedNodes = append(m.UpdatedNodes, &nodeCopy)
	return node, nil
}

// UpdateStatus updates a status of a Node in the fake store.
func (m *FakeNodeHandler) UpdateStatus(_ context.Context, node *v1.Node, _ metav1.UpdateOptions) (*v1.Node, error) {
	m.lock.Lock()
	defer func() {
		m.RequestCount++
		m.lock.Unlock()
	}()

	var origNodeCopy v1.Node
	found := false
	for i := range m.Existing {
		if m.Existing[i].Name == node.Name {
			origNodeCopy = *m.Existing[i]
			found = true
			break
		}
	}
	updatedNodeIndex := -1
	for i := range m.UpdatedNodes {
		if m.UpdatedNodes[i].Name == node.Name {
			origNodeCopy = *m.UpdatedNodes[i]
			updatedNodeIndex = i
			found = true
			break
		}
	}

	if !found {
		return nil, fmt.Errorf("not found node %v", node)
	}

	origNodeCopy.Status = node.Status
	if updatedNodeIndex < 0 {
		m.UpdatedNodes = append(m.UpdatedNodes, &origNodeCopy)
	} else {
		m.UpdatedNodes[updatedNodeIndex] = &origNodeCopy
	}

	nodeCopy := *node
	m.UpdatedNodeStatuses = append(m.UpdatedNodeStatuses, &nodeCopy)
	return node, nil
}

// PatchStatus patches a status of a Node in the fake store.
func (m *FakeNodeHandler) PatchStatus(ctx context.Context, nodeName string, data []byte) (*v1.Node, error) {
	m.RequestCount++
	return m.Patch(ctx, nodeName, types.StrategicMergePatchType, data, metav1.PatchOptions{}, "status")
}

// Watch watches Nodes in a fake store.
func (m *FakeNodeHandler) Watch(_ context.Context, opts metav1.ListOptions) (watch.Interface, error) {
	return watch.NewFake(), nil
}

// Patch patches a Node in the fake store.
func (m *FakeNodeHandler) Patch(ctx context.Context, name string, pt types.PatchType, data []byte, _ metav1.PatchOptions, subresources ...string) (*v1.Node, error) {
	m.lock.Lock()
	defer func() {
		m.RequestCount++
		if m.PatchWaitChan != nil {
			m.PatchWaitChan <- struct{}{}
		}
		m.lock.Unlock()
	}()
	var nodeCopy v1.Node
	for i := range m.Existing {
		if m.Existing[i].Name == name {
			nodeCopy = *m.Existing[i]
		}
	}
	updatedNodeIndex := -1
	for i := range m.UpdatedNodes {
		if m.UpdatedNodes[i].Name == name {
			nodeCopy = *m.UpdatedNodes[i]
			updatedNodeIndex = i
		}
	}

	originalObjJS, err := json.Marshal(nodeCopy)
	if err != nil {
		klog.FromContext(ctx).Error(nil, "Failed to marshal", "node", klog.KObj(&nodeCopy))
		return nil, nil
	}
	var originalNode v1.Node
	if err = json.Unmarshal(originalObjJS, &originalNode); err != nil {
		klog.FromContext(ctx).Error(err, "Failed to unmarshal original object")
		return nil, nil
	}

	var patchedObjJS []byte
	switch pt {
	case types.JSONPatchType:
		patchObj, err := jsonpatch.DecodePatch(data)
		if err != nil {
			klog.FromContext(ctx).Error(err, "")
			return nil, nil
		}
		if patchedObjJS, err = patchObj.Apply(originalObjJS); err != nil {
			klog.FromContext(ctx).Error(err, "")
			return nil, nil
		}
	case types.MergePatchType:
		if patchedObjJS, err = jsonpatch.MergePatch(originalObjJS, data); err != nil {
			klog.FromContext(ctx).Error(err, "")
			return nil, nil
		}
	case types.StrategicMergePatchType:
		if patchedObjJS, err = strategicpatch.StrategicMergePatch(originalObjJS, data, originalNode); err != nil {
			klog.FromContext(ctx).Error(err, "")
			return nil, nil
		}
	default:
		klog.FromContext(ctx).Error(nil, "Unknown Content-Type header", "patch", pt)
		return nil, nil
	}

	var updatedNode v1.Node
	if err = json.Unmarshal(patchedObjJS, &updatedNode); err != nil {
		klog.FromContext(ctx).Error(err, "Failed to unmarshal patched object")
		return nil, nil
	}

	if updatedNodeIndex < 0 {
		m.UpdatedNodes = append(m.UpdatedNodes, &updatedNode)
	} else {
		if updatedNode.GetObjectMeta().GetResourceVersion() != m.UpdatedNodes[updatedNodeIndex].GetObjectMeta().GetResourceVersion() {
			return nil, apierrors.NewConflict(schema.GroupResource{}, "fake conflict", nil)
		}
		m.UpdatedNodes[updatedNodeIndex] = &updatedNode
	}

	return &updatedNode, nil
}

// Apply applies a NodeApplyConfiguration to a Node in the fake store.
func (m *FakeNodeHandler) Apply(ctx context.Context, node *v1apply.NodeApplyConfiguration, opts metav1.ApplyOptions) (*v1.Node, error) {
	patchOpts := opts.ToPatchOptions()
	data, err := json.Marshal(node)
	if err != nil {
		return nil, err
	}
	name := node.Name
	if name == nil {
		return nil, fmt.Errorf("deployment.Name must be provided to Apply")
	}

	return m.Patch(ctx, *name, types.ApplyPatchType, data, patchOpts)
}

// ApplyStatus applies a status of a Node in the fake store.
func (m *FakeNodeHandler) ApplyStatus(ctx context.Context, node *v1apply.NodeApplyConfiguration, opts metav1.ApplyOptions) (*v1.Node, error) {
	patchOpts := opts.ToPatchOptions()
	data, err := json.Marshal(node)
	if err != nil {
		return nil, err
	}
	name := node.Name
	if name == nil {
		return nil, fmt.Errorf("deployment.Name must be provided to Apply")
	}

	return m.Patch(ctx, *name, types.ApplyPatchType, data, patchOpts, "status")
}

// FakeRecorder is used as a fake during testing.
type FakeRecorder struct {
	sync.Mutex
	source v1.EventSource
	Events []*v1.Event
	clock  clock.Clock
}

// Event emits a fake event to the fake recorder
func (f *FakeRecorder) Event(obj runtime.Object, eventtype, reason, message string) {
	f.generateEvent(obj, metav1.Now(), eventtype, reason, message)
}

// Eventf emits a fake formatted event to the fake recorder
func (f *FakeRecorder) Eventf(obj runtime.Object, eventtype, reason, messageFmt string, args ...interface{}) {
	f.Event(obj, eventtype, reason, fmt.Sprintf(messageFmt, args...))
}

// AnnotatedEventf emits a fake formatted event to the fake recorder
func (f *FakeRecorder) AnnotatedEventf(obj runtime.Object, annotations map[string]string, eventtype, reason, messageFmt string, args ...interface{}) {
	f.Eventf(obj, eventtype, reason, messageFmt, args...)
}

func (f *FakeRecorder) generateEvent(obj runtime.Object, timestamp metav1.Time, eventtype, reason, message string) {
	f.Lock()
	defer f.Unlock()
	ctx := context.TODO()
	ref, err := ref.GetReference(legacyscheme.Scheme, obj)
	if err != nil {
		klog.FromContext(ctx).Error(err, "Encountered error while getting reference")
		return
	}
	event := f.makeEvent(ref, eventtype, reason, message)
	event.Source = f.source
	if f.Events != nil {
		f.Events = append(f.Events, event)
	}
}

func (f *FakeRecorder) makeEvent(ref *v1.ObjectReference, eventtype, reason, message string) *v1.Event {
	t := metav1.Time{Time: f.clock.Now()}
	namespace := ref.Namespace
	if namespace == "" {
		namespace = metav1.NamespaceDefault
	}

	clientref := v1.ObjectReference{
		Kind:            ref.Kind,
		Namespace:       ref.Namespace,
		Name:            ref.Name,
		UID:             ref.UID,
		APIVersion:      ref.APIVersion,
		ResourceVersion: ref.ResourceVersion,
		FieldPath:       ref.FieldPath,
	}

	return &v1.Event{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%v.%x", ref.Name, t.UnixNano()),
			Namespace: namespace,
		},
		InvolvedObject: clientref,
		Reason:         reason,
		Message:        message,
		FirstTimestamp: t,
		LastTimestamp:  t,
		Count:          1,
		Type:           eventtype,
	}
}

// NewFakeRecorder returns a pointer to a newly constructed FakeRecorder.
func NewFakeRecorder() *FakeRecorder {
	return &FakeRecorder{
		source: v1.EventSource{Component: "nodeControllerTest"},
		Events: []*v1.Event{},
		clock:  testingclock.NewFakeClock(time.Now()),
	}
}

// NewNode is a helper function for creating Nodes for testing.
func NewNode(name string) *v1.Node {
	return &v1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: v1.NodeStatus{
			Capacity: v1.ResourceList{
				v1.ResourceName(v1.ResourceCPU):    resource.MustParse("10"),
				v1.ResourceName(v1.ResourceMemory): resource.MustParse("10G"),
			},
		},
	}
}

// NewPod is a helper function for creating Pods for testing.
func NewPod(name, host string) *v1.Pod {
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      name,
		},
		Spec: v1.PodSpec{
			NodeName: host,
		},
		Status: v1.PodStatus{
			Conditions: []v1.PodCondition{
				{
					Type:   v1.PodReady,
					Status: v1.ConditionTrue,
				},
			},
		},
	}

	return pod
}

func contains(node *v1.Node, nodes []*v1.Node) bool {
	for i := 0; i < len(nodes); i++ {
		if node.Name == nodes[i].Name {
			return true
		}
	}
	return false
}

// GetZones returns list of zones for all Nodes stored in FakeNodeHandler
func GetZones(nodeHandler *FakeNodeHandler) []string {
	nodes, _ := nodeHandler.List(context.TODO(), metav1.ListOptions{})
	zones := sets.NewString()
	for _, node := range nodes.Items {
		zones.Insert(utilnode.GetZoneKey(&node))
	}
	return zones.List()
}

// CreateZoneID returns a single zoneID for a given region and zone.
func CreateZoneID(region, zone string) string {
	return region + ":\x00:" + zone
}

// GetKey is a helper function used by controllers unit tests to get the
// key for a given kubernetes resource.
func GetKey(obj interface{}, t *testing.T) string {
	t.Helper()
	tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
	if ok {
		// if tombstone , try getting the value from tombstone.Obj
		obj = tombstone.Obj
	}
	val := reflect.ValueOf(obj).Elem()
	name := val.FieldByName("Name").String()
	if len(name) == 0 {
		t.Errorf("Unexpected object %v", obj)
	}

	key, err := keyFunc(obj)
	if err != nil {
		t.Errorf("Unexpected error getting key for %T %v: %v", val.Interface(), name, err)
		return ""
	}
	return key
}
