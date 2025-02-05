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

package node

import (
	"context"
	"errors"
	"fmt"
	"time"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	coreinformers "k8s.io/client-go/informers/core/v1"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	v1core "k8s.io/client-go/kubernetes/typed/core/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	clientretry "k8s.io/client-go/util/retry"
	"k8s.io/client-go/util/workqueue"
	cloudprovider "k8s.io/cloud-provider"
	cloudproviderapi "k8s.io/cloud-provider/api"
	cloudnodeutil "k8s.io/cloud-provider/node/helpers"
	controllersmetrics "k8s.io/component-base/metrics/prometheus/controllers"
	nodeutil "k8s.io/component-helpers/node/util"
	"k8s.io/klog/v2"
)

func init() {
	registerMetrics()
}

type labelsKeys map[string]struct{}

var (
	instanceTypeKeys = map[string]struct{}{
		v1.LabelInstanceType:       {},
		v1.LabelInstanceTypeStable: {},
	}

	zoneKeys = map[string]struct{}{
		v1.LabelFailureDomainBetaZone: {},
		v1.LabelTopologyZone:          {},
	}

	regionKeys = map[string]struct{}{
		v1.LabelFailureDomainBetaRegion: {},
		v1.LabelTopologyRegion:          {},
	}
)

// labelReconcileInfo lists Node labels to reconcile, and how to reconcile them.
// primaryKey and secondaryKey are keys of labels to reconcile.
//   - If both keys exist, but their values don't match. Use the value from the
//     primaryKey as the source of truth to reconcile.
//   - If ensureSecondaryExists is true, and the secondaryKey does not
//     exist, secondaryKey will be added with the value of the primaryKey.
var labelReconcileInfo = []struct {
	primaryKey            string
	secondaryKey          string
	ensureSecondaryExists bool
}{
	{
		// Reconcile the beta and the GA zone label using the beta label as
		// the source of truth
		// TODO: switch the primary key to GA labels in v1.21
		primaryKey:            v1.LabelFailureDomainBetaZone,
		secondaryKey:          v1.LabelTopologyZone,
		ensureSecondaryExists: true,
	},
	{
		// Reconcile the beta and the stable region label using the beta label as
		// the source of truth
		// TODO: switch the primary key to GA labels in v1.21
		primaryKey:            v1.LabelFailureDomainBetaRegion,
		secondaryKey:          v1.LabelTopologyRegion,
		ensureSecondaryExists: true,
	},
	{
		// Reconcile the beta and the stable instance-type label using the beta label as
		// the source of truth
		// TODO: switch the primary key to GA labels in v1.21
		primaryKey:            v1.LabelInstanceType,
		secondaryKey:          v1.LabelInstanceTypeStable,
		ensureSecondaryExists: true,
	},
}

var UpdateNodeSpecBackoff = wait.Backoff{
	Steps:    20,
	Duration: 50 * time.Millisecond,
	Jitter:   1.0,
}

// CloudNodeController is the controller implementation for Node resources
type CloudNodeController struct {
	nodeInformer coreinformers.NodeInformer
	kubeClient   clientset.Interface

	broadcaster record.EventBroadcaster
	recorder    record.EventRecorder

	cloud cloudprovider.Interface

	nodeStatusUpdateFrequency time.Duration
	workerCount               int32

	nodesLister corelisters.NodeLister
	nodesSynced cache.InformerSynced
	workqueue   workqueue.TypedRateLimitingInterface[string]
}

// NewCloudNodeController creates a CloudNodeController object
func NewCloudNodeController(
	nodeInformer coreinformers.NodeInformer,
	kubeClient clientset.Interface,
	cloud cloudprovider.Interface,
	nodeStatusUpdateFrequency time.Duration,
	workerCount int32) (*CloudNodeController, error) {

	_, instancesSupported := cloud.Instances()
	_, instancesV2Supported := cloud.InstancesV2()
	if !instancesSupported && !instancesV2Supported {
		return nil, errors.New("cloud provider does not support instances")
	}

	cnc := &CloudNodeController{
		nodeInformer:              nodeInformer,
		kubeClient:                kubeClient,
		cloud:                     cloud,
		nodeStatusUpdateFrequency: nodeStatusUpdateFrequency,
		workerCount:               workerCount,
		nodesLister:               nodeInformer.Lister(),
		nodesSynced:               nodeInformer.Informer().HasSynced,
		workqueue: workqueue.NewTypedRateLimitingQueueWithConfig(
			workqueue.DefaultTypedControllerRateLimiter[string](),
			workqueue.TypedRateLimitingQueueConfig[string]{Name: "Nodes"},
		),
	}

	// Use shared informer to listen to add/update of nodes. Note that any nodes
	// that exist before node controller starts will show up in the update method
	cnc.nodeInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    cnc.enqueueNode,
		UpdateFunc: func(oldObj, newObj interface{}) { cnc.enqueueNode(newObj) },
	})

	return cnc, nil
}

// Run will sync informer caches and starting workers.
// This controller updates newly registered nodes with information
// from the cloud provider. This call is blocking so should be called
// via a goroutine
//
//logcheck:context // RunWithContext should be used instead of Run in code which supports contextual logging.
func (cnc *CloudNodeController) Run(stopCh <-chan struct{}, controllerManagerMetrics *controllersmetrics.ControllerManagerMetrics) {
	cnc.RunWithContext(wait.ContextForChannel(stopCh), controllerManagerMetrics)
}

// RunWithContext will sync informer caches and starting workers.
// This controller updates newly registered nodes with information
// from the cloud provider. This call is blocking so should be called
// via a goroutine
func (cnc *CloudNodeController) RunWithContext(ctx context.Context, controllerManagerMetrics *controllersmetrics.ControllerManagerMetrics) {
	cnc.broadcaster = record.NewBroadcaster(record.WithContext(ctx))
	cnc.recorder = cnc.broadcaster.NewRecorder(scheme.Scheme, v1.EventSource{Component: "cloud-node-controller"})
	stopCh := ctx.Done()

	defer utilruntime.HandleCrash()
	defer cnc.workqueue.ShutDown()

	// Start event processing pipeline.
	klog.Infof("Sending events to api server.")
	controllerManagerMetrics.ControllerStarted("cloud-node")
	defer controllerManagerMetrics.ControllerStopped("cloud-node")

	cnc.broadcaster.StartStructuredLogging(0)
	cnc.broadcaster.StartRecordingToSink(&v1core.EventSinkImpl{Interface: cnc.kubeClient.CoreV1().Events("")})
	defer cnc.broadcaster.Shutdown()

	// Wait for the caches to be synced before starting workers
	klog.Info("Waiting for informer caches to sync")
	if ok := cache.WaitForCacheSync(stopCh, cnc.nodesSynced); !ok {
		klog.Errorf("failed to wait for caches to sync")
		return
	}

	// The periodic loop for updateNodeStatus polls the Cloud Provider periodically
	// to reconcile the nodes addresses and labels.
	go wait.UntilWithContext(ctx, func(ctx context.Context) {
		if err := cnc.UpdateNodeStatus(ctx); err != nil {
			klog.Errorf("failed to update node status: %v", err)
		}
	}, cnc.nodeStatusUpdateFrequency)

	// These workers initialize the nodes added to the cluster,
	// those that are Tainted with TaintExternalCloudProvider.
	for i := int32(0); i < cnc.workerCount; i++ {
		go wait.UntilWithContext(ctx, cnc.runWorker, time.Second)
	}

	<-stopCh
}

// runWorker is a long-running function that will continually call the
// processNextWorkItem function in order to read and process a message on the
// workqueue.
func (cnc *CloudNodeController) runWorker(ctx context.Context) {
	for cnc.processNextWorkItem(ctx) {
	}
}

// processNextWorkItem will read a single work item off the workqueue and
// attempt to process it, by calling the syncHandler.
func (cnc *CloudNodeController) processNextWorkItem(ctx context.Context) bool {
	obj, shutdown := cnc.workqueue.Get()
	if shutdown {
		return false
	}

	// We wrap this block in a func so we can defer cnc.workqueue.Done.
	err := func(key string) error {
		defer cnc.workqueue.Done(key)

		// Run the syncHandler, passing it the key of the
		// Node resource to be synced.
		if err := cnc.syncHandler(ctx, key); err != nil {
			// Put the item back on the workqueue to handle any transient errors.
			cnc.workqueue.AddRateLimited(key)
			klog.Infof("error syncing '%s': %v, requeuing", key, err)
			return fmt.Errorf("error syncing '%s': %s, requeuing", key, err.Error())
		}

		// Finally, if no error occurs we Forget this item so it does not
		// get queued again until another change happens.
		cnc.workqueue.Forget(key)
		return nil
	}(obj)

	if err != nil {
		utilruntime.HandleError(err)
		return true
	}

	return true
}

// syncHandler implements the logic of the controller.
func (cnc *CloudNodeController) syncHandler(ctx context.Context, key string) error {
	_, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("invalid resource key: %s", key))
		return nil
	}

	return cnc.syncNode(ctx, name)
}

// UpdateNodeStatus updates the node status, such as node addresses
func (cnc *CloudNodeController) UpdateNodeStatus(ctx context.Context) error {
	start := time.Now()
	nodes, err := cnc.nodesLister.List(labels.Everything())
	if err != nil {
		klog.Errorf("Error monitoring node status: %v", err)
		return err
	}
	defer func() {
		klog.V(2).Infof("Update %d nodes status took %v.", len(nodes), time.Since(start))
	}()

	updateNodeFunc := func(piece int) {
		node := nodes[piece].DeepCopy()
		// Do not process nodes that are still tainted, those will be processed by syncNode()
		cloudTaint := getCloudTaint(node.Spec.Taints)
		if cloudTaint != nil {
			klog.V(5).Infof("This node %s is still tainted. Will not process.", node.Name)
			return
		}

		instanceMetadata, err := cnc.getInstanceMetadata(ctx, node)
		if err != nil {
			klog.Errorf("failed to get instance metadata for node %s: %v", node.Name, err)
			return
		}
		if instanceMetadata == nil {
			// do nothing when external cloud providers provide nil instanceMetadata
			klog.Infof("Skip sync node %s because cloud provided nil metadata", node.Name)
			return
		}

		cnc.updateNodeAddress(ctx, node, instanceMetadata)

		// Get Cloud Provider modifiers (for labels)
		nodeModifiers := cnc.getLableModifiers(ctx, node, instanceMetadata)
		if err := clientretry.RetryOnConflict(UpdateNodeSpecBackoff, func() error {
			curNode, err := cnc.nodesLister.Get(node.Name)
			if err != nil {

				return err
			}

			newNode := curNode.DeepCopy()
			for _, modify := range nodeModifiers {
				modify(newNode)
			}

			_, err = cnc.kubeClient.CoreV1().Nodes().Update(ctx, newNode, metav1.UpdateOptions{})
			if err != nil {
				return err
			}

			return nil
		}); err != nil {
			klog.Errorf("Error syncing node labels for node %q, err: %v", node.Name, err)
			return
		}
	}

	workqueue.ParallelizeUntil(ctx, int(cnc.workerCount), len(nodes), updateNodeFunc)
	return nil
}

// enqueueNode takes a Node resource and converts it into a key
// string which is then put onto the work queue.
func (cnc *CloudNodeController) enqueueNode(obj interface{}) {
	var key string
	var err error
	if key, err = cache.MetaNamespaceKeyFunc(obj); err != nil {
		utilruntime.HandleError(err)
		return
	}
	cnc.workqueue.Add(key)
}

// reconcileNodeLabels reconciles node labels transitioning from beta to GA
func (cnc *CloudNodeController) reconcileNodeLabels(nodeName string) error {
	node, err := cnc.nodeInformer.Lister().Get(nodeName)
	if err != nil {
		// If node not found, just ignore it.
		if apierrors.IsNotFound(err) {
			return nil
		}

		return err
	}

	if node.Labels == nil {
		// Nothing to reconcile.
		return nil
	}

	labelsToUpdate := map[string]string{}
	for _, r := range labelReconcileInfo {
		primaryValue, primaryExists := node.Labels[r.primaryKey]
		secondaryValue, secondaryExists := node.Labels[r.secondaryKey]

		if !primaryExists {
			// The primary label key does not exist. This should not happen
			// within our supported version skew range, when no external
			// components/factors modifying the node object. Ignore this case.
			continue
		}
		if secondaryExists && primaryValue != secondaryValue {
			// Secondary label exists, but not consistent with the primary
			// label. Need to reconcile.
			labelsToUpdate[r.secondaryKey] = primaryValue

		} else if !secondaryExists && r.ensureSecondaryExists {
			// Apply secondary label based on primary label.
			labelsToUpdate[r.secondaryKey] = primaryValue
		}
	}

	if len(labelsToUpdate) == 0 {
		return nil
	}

	if !cloudnodeutil.AddOrUpdateLabelsOnNode(cnc.kubeClient, labelsToUpdate, node) {
		return fmt.Errorf("failed update labels for node %+v", node)
	}

	return nil
}

// UpdateNodeAddress updates the nodeAddress of a single node
func (cnc *CloudNodeController) updateNodeAddress(ctx context.Context, node *v1.Node, instanceMetadata *cloudprovider.InstanceMetadata) {
	// Do not process nodes that are still tainted
	cloudTaint := getCloudTaint(node.Spec.Taints)
	if cloudTaint != nil {
		klog.V(5).Infof("This node %s is still tainted. Will not process.", node.Name)
		return
	}

	nodeAddresses := instanceMetadata.NodeAddresses
	if len(nodeAddresses) == 0 {
		klog.V(5).Infof("Skipping node address update for node %q since cloud provider did not return any", node.Name)
		return
	}

	// Check if a hostname address exists in the cloud provided addresses
	hostnameExists := false
	for i := range nodeAddresses {
		if nodeAddresses[i].Type == v1.NodeHostName {
			hostnameExists = true
			break
		}
	}
	// If hostname was not present in cloud provided addresses, use the hostname
	// from the existing node (populated by kubelet)
	if !hostnameExists {
		for _, addr := range node.Status.Addresses {
			if addr.Type == v1.NodeHostName {
				nodeAddresses = append(nodeAddresses, addr)
			}
		}
	}
	// If kubelet provided a node IP, prefer it in the node address list
	nodeAddresses, err := updateNodeAddressesFromNodeIP(node, nodeAddresses)
	if err != nil {
		klog.Errorf("Failed to update node addresses for node %q: %v", node.Name, err)
		return
	}

	if !nodeAddressesChangeDetected(node.Status.Addresses, nodeAddresses) {
		return
	}
	newNode := node.DeepCopy()
	newNode.Status.Addresses = nodeAddresses
	if _, _, err := nodeutil.PatchNodeStatus(cnc.kubeClient.CoreV1(), types.NodeName(node.Name), node, newNode); err != nil {
		klog.Errorf("Error patching node with cloud ip addresses = [%v]", err)
	}
}

// nodeModifier is used to carry changes to node objects across multiple attempts to update them
// in a retry-if-conflict loop.
type nodeModifier func(*v1.Node)

// syncNode handles updating existing nodes registered with the cloud taint
// and processes nodes that were added into the cluster, and cloud initialize them if appropriate.
func (cnc *CloudNodeController) syncNode(ctx context.Context, nodeName string) error {
	curNode, err := cnc.nodeInformer.Lister().Get(nodeName)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}

		return err
	}

	cloudTaint := getCloudTaint(curNode.Spec.Taints)
	if cloudTaint == nil {
		// Node object was already initialized, only need to reconcile the labels
		return cnc.reconcileNodeLabels(nodeName)
	}

	klog.Infof("Initializing node %s with cloud provider", nodeName)

	copyNode := curNode.DeepCopy()

	instanceMetadata, err := cnc.getInstanceMetadata(ctx, copyNode)
	if err != nil {
		return fmt.Errorf("failed to get instance metadata for node %s: %v", nodeName, err)
	}
	if instanceMetadata == nil {
		// do nothing when external cloud providers provide nil instanceMetadata
		klog.Infof("Skip sync node %s because cloud provided nil metadata", nodeName)
		return nil
	}

	nodeModifiers, err := cnc.getNodeModifiersFromCloudProvider(ctx, copyNode, instanceMetadata)
	if err != nil {
		return fmt.Errorf("failed to get node modifiers from cloud provider: %v", err)
	}

	nodeModifiers = append(nodeModifiers, func(n *v1.Node) {
		n.Spec.Taints = excludeCloudTaint(n.Spec.Taints)
	})

	err = clientretry.RetryOnConflict(UpdateNodeSpecBackoff, func() error {
		curNode, err = cnc.nodeInformer.Lister().Get(nodeName)
		if err != nil {
			return err
		}

		newNode := curNode.DeepCopy()
		for _, modify := range nodeModifiers {
			modify(newNode)
		}

		_, err = cnc.kubeClient.CoreV1().Nodes().Update(ctx, newNode, metav1.UpdateOptions{})
		if err != nil {
			return err
		}

		removeCloudProviderTaintDelay.Observe(time.Since(newNode.ObjectMeta.CreationTimestamp.Time).Seconds())

		// After adding, call UpdateNodeAddress to set the CloudProvider provided IPAddresses
		// So that users do not see any significant delay in IP addresses being filled into the node
		cnc.updateNodeAddress(ctx, newNode, instanceMetadata)

		klog.Infof("Successfully initialized node %s with cloud provider", nodeName)
		return nil
	})
	if err != nil {
		return err
	}

	cnc.recorder.Event(copyNode, v1.EventTypeNormal, "Synced", "Node synced successfully")
	initialNodeSyncDelay.Observe(time.Since(curNode.ObjectMeta.CreationTimestamp.Time).Seconds())
	return nil
}

// getNodeModifiersFromCloudProvider returns a slice of nodeModifiers that update
// a node object with provider-specific information.
// All of the returned functions are idempotent, because they are used in a retry-if-conflict
// loop, meaning they could get called multiple times.
func (cnc *CloudNodeController) getNodeModifiersFromCloudProvider(
	ctx context.Context,
	node *v1.Node,
	instanceMeta *cloudprovider.InstanceMetadata,
) ([]nodeModifier, error) {

	var nodeModifiers []nodeModifier
	if node.Spec.ProviderID == "" {
		if instanceMeta.ProviderID != "" {
			nodeModifiers = append(nodeModifiers, func(n *v1.Node) { n.Spec.ProviderID = instanceMeta.ProviderID })
		}
	}

	// If kubelet annotated the node with a node IP, ensure that it is valid
	// and can be applied to the discovered node addresses before removing
	// the taint on the node.
	_, err := updateNodeAddressesFromNodeIP(node, instanceMeta.NodeAddresses)
	if err != nil {
		return nil, fmt.Errorf("provided node ip for node %q is not valid: %w", node.Name, err)
	}

	nodeModifiers = append(nodeModifiers, cnc.getLableModifiers(ctx, node, instanceMeta)...)

	return nodeModifiers, nil
}

func (cnc *CloudNodeController) getLableModifiers(
	ctx context.Context,
	node *v1.Node,
	instanceMeta *cloudprovider.InstanceMetadata,
) []nodeModifier {
	var nodeModifiers []nodeModifier

	labelsToAdd := make(map[string]string)
	if instanceMeta.InstanceType != "" {
		addProviderLabels(instanceTypeKeys, labelsToAdd, instanceMeta.InstanceType)
	}

	if instanceMeta.Zone != "" {
		addProviderLabels(zoneKeys, labelsToAdd, instanceMeta.Zone)
	}

	if instanceMeta.Region != "" {
		addProviderLabels(regionKeys, labelsToAdd, instanceMeta.Region)
	}

	nodeModifiers = append(nodeModifiers, func(n *v1.Node) {
		if n.Labels == nil {
			n.Labels = map[string]string{}
		}

		for k, v := range labelsToAdd {
			// If node has that lable with same value - skip it
			if originalVal, ok := n.Labels[k]; ok && originalVal == v {
				klog.Warningf("Discarding node label %s that is already present", k)
				continue
			}

			n.Labels[k] = v
		}
	})

	return nodeModifiers
}

func addProviderLabels(keys labelsKeys, lables map[string]string, v string) {
	for k := range keys {
		klog.V(2).Infof("Adding node label from cloud provider: %s=%s", k, v)
		lables[k] = v
	}
}

// getInstanceMetadata get providerdID, instance type and nodeAddresses, use Instances if InstancesV2 is off.
// ProviderID is expected to be available, but to keep backward compatibility,
// we should handle some scenarios where it can be missing. It returns an error
// if providerID is missing, except when is not implemented by GetInstanceProviderID.
func (cnc *CloudNodeController) getInstanceMetadata(ctx context.Context, node *v1.Node) (*cloudprovider.InstanceMetadata, error) {
	// kubelet can set the provider ID using the flag and is inmutable
	providerID := node.Spec.ProviderID
	// InstancesV2 require ProviderID to be present
	if instancesV2, ok := cnc.cloud.InstancesV2(); instancesV2 != nil && ok {
		metadata, err := instancesV2.InstanceMetadata(ctx, node)
		if err != nil {
			return nil, err
		}
		// spec.ProviderID is required for multiple controllers, like loadbalancers, so we should not
		// untaint the node until is set. Once it is set, the field is immutable, so no need to reconcile.
		// We only set this value during initialization and is never reconciled, so if for some reason
		// we are not able to set it, the instance will never be able to acquire it.
		// Before external cloud providers were enabled by default, the field was set by the kubelet, and the
		// node was created with the value.
		// xref: https://issues.k8s.io/123024
		if metadata != nil && metadata.ProviderID == "" {
			if providerID == "" {
				return metadata, fmt.Errorf("cloud provider does not set node provider ID for node %s", node.Name)
			}
			metadata.ProviderID = providerID
		}
		return metadata, nil
	}

	// If InstancesV2 not implement, use Instances.
	instances, ok := cnc.cloud.Instances()
	if !ok {
		return nil, fmt.Errorf("failed to get instances from cloud provider")
	}

	var err error
	if providerID == "" {
		providerID, err = cloudprovider.GetInstanceProviderID(ctx, cnc.cloud, types.NodeName(node.Name))
		if err != nil {
			// This is the only case where ProviderID can be skipped
			if errors.Is(err, cloudprovider.NotImplemented) {
				// if the cloud provider being used does not support provider IDs,
				// we can safely continue since we will attempt to set node
				// addresses given the node name in getNodeAddressesByProviderIDOrName
				klog.Warningf("cloud provider does not set node provider ID, using node name to discover node %s", node.Name)
			} else {
				// if the cloud provider being used supports provider IDs, we want
				// to propagate the error so that we re-try in the future; if we
				// do not, the taint will be removed, and this will not be retried
				return nil, err
			}
		}
	}

	nodeAddresses, err := getNodeAddressesByProviderIDOrName(ctx, instances, providerID, node.Name)
	if err != nil {
		return nil, err
	}
	instanceType, err := getInstanceTypeByProviderIDOrName(ctx, instances, providerID, node.Name)
	if err != nil {
		return nil, err
	}

	instanceMetadata := &cloudprovider.InstanceMetadata{
		ProviderID:    providerID,
		InstanceType:  instanceType,
		NodeAddresses: nodeAddresses,
	}

	zones, ok := cnc.cloud.Zones()
	if !ok {
		return instanceMetadata, nil
	}

	zone, err := getZoneByProviderIDOrName(ctx, zones, providerID, node.Name)
	if err != nil {
		return nil, fmt.Errorf("failed to get zone from cloud provider: %v", err)
	}

	if zone.FailureDomain != "" {
		instanceMetadata.Zone = zone.FailureDomain
	}

	if zone.Region != "" {
		instanceMetadata.Region = zone.Region
	}

	return instanceMetadata, nil
}

// getInstanceAddresses returns InstanceMetadata.NodeAddresses. If InstancesV2 not supported, it won't get instanceType
// which avoid an api call compared with getInstanceMetadata.
func (cnc *CloudNodeController) getInstanceNodeAddresses(ctx context.Context, node *v1.Node) (*cloudprovider.InstanceMetadata, error) {
	if instancesV2, ok := cnc.cloud.InstancesV2(); instancesV2 != nil && ok {
		return instancesV2.InstanceMetadata(ctx, node)
	}

	// If InstancesV2 not implement, use Instances.
	instances, ok := cnc.cloud.Instances()
	if !ok {
		return nil, fmt.Errorf("failed to get instances from cloud provider")
	}
	nodeAddresses, err := getNodeAddressesByProviderIDOrName(ctx, instances, node.Spec.ProviderID, node.Name)
	if err != nil {
		return nil, err
	}

	return &cloudprovider.InstanceMetadata{
		NodeAddresses: nodeAddresses,
	}, nil
}

func getCloudTaint(taints []v1.Taint) *v1.Taint {
	for _, taint := range taints {
		if taint.Key == cloudproviderapi.TaintExternalCloudProvider {
			return &taint
		}
	}
	return nil
}

func excludeCloudTaint(taints []v1.Taint) []v1.Taint {
	newTaints := []v1.Taint{}
	for _, taint := range taints {
		if taint.Key == cloudproviderapi.TaintExternalCloudProvider {
			continue
		}
		newTaints = append(newTaints, taint)
	}
	return newTaints
}

func getNodeAddressesByProviderIDOrName(ctx context.Context, instances cloudprovider.Instances, providerID, nodeName string) ([]v1.NodeAddress, error) {
	nodeAddresses, err := instances.NodeAddressesByProviderID(ctx, providerID)
	if err != nil {
		providerIDErr := err
		nodeAddresses, err = instances.NodeAddresses(ctx, types.NodeName(nodeName))
		if err != nil {
			return nil, fmt.Errorf("error fetching node by provider ID: %v, and error by node name: %v", providerIDErr, err)
		}
	}
	return nodeAddresses, nil
}

func nodeAddressesChangeDetected(addressSet1, addressSet2 []v1.NodeAddress) bool {
	if len(addressSet1) != len(addressSet2) {
		return true
	}
	addressMap1 := map[v1.NodeAddressType]string{}

	for i := range addressSet1 {
		addressMap1[addressSet1[i].Type] = addressSet1[i].Address
	}

	for _, v := range addressSet2 {
		if addressMap1[v.Type] != v.Address {
			return true
		}
	}
	return false
}

func updateNodeAddressesFromNodeIP(node *v1.Node, nodeAddresses []v1.NodeAddress) ([]v1.NodeAddress, error) {
	var err error

	providedNodeIP, exists := node.ObjectMeta.Annotations[cloudproviderapi.AnnotationAlphaProvidedIPAddr]
	if exists {
		nodeAddresses, err = cloudnodeutil.GetNodeAddressesFromNodeIP(providedNodeIP, nodeAddresses)
	}

	return nodeAddresses, err
}

// getInstanceTypeByProviderIDOrName will attempt to get the instance type of node using its providerID
// then it's name. If both attempts fail, an error is returned.
func getInstanceTypeByProviderIDOrName(ctx context.Context, instances cloudprovider.Instances, providerID, nodeName string) (string, error) {
	instanceType, err := instances.InstanceTypeByProviderID(ctx, providerID)
	if err != nil {
		providerIDErr := err
		instanceType, err = instances.InstanceType(ctx, types.NodeName(nodeName))
		if err != nil {
			return "", fmt.Errorf("InstanceType: Error fetching by providerID: %v Error fetching by NodeName: %v", providerIDErr, err)
		}
	}
	return instanceType, err
}

// getZoneByProviderIDorName will attempt to get the zone of node using its providerID
// then it's name. If both attempts fail, an error is returned.
func getZoneByProviderIDOrName(ctx context.Context, zones cloudprovider.Zones, providerID, nodeName string) (cloudprovider.Zone, error) {
	zone, err := zones.GetZoneByProviderID(ctx, providerID)
	if err != nil {
		providerIDErr := err
		zone, err = zones.GetZoneByNodeName(ctx, types.NodeName(nodeName))
		if err != nil {
			return cloudprovider.Zone{}, fmt.Errorf("Zone: Error fetching by providerID: %v Error fetching by NodeName: %v", providerIDErr, err)
		}
	}

	return zone, nil
}
