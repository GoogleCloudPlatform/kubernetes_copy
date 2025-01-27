/*
Copyright 2022 The Kubernetes Authors.

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

package dra

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"net"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"github.com/onsi/gomega/format"
	"google.golang.org/grpc"

	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1beta1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/apiserver/pkg/authentication/serviceaccount"
	"k8s.io/client-go/discovery/cached/memory"
	resourceapiinformer "k8s.io/client-go/informers/resource/v1beta1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/cache"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/test/e2e/dra/test-driver/app"
	"k8s.io/kubernetes/test/e2e/framework"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	e2ereplicaset "k8s.io/kubernetes/test/e2e/framework/replicaset"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"
	"k8s.io/kubernetes/test/e2e/storage/drivers/proxy"
	"k8s.io/kubernetes/test/e2e/storage/utils"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/yaml"
)

const (
	NodePrepareResourcesMethod   = "/k8s.io.kubelet.pkg.apis.dra.v1beta1.DRAPlugin/NodePrepareResources"
	NodeUnprepareResourcesMethod = "/k8s.io.kubelet.pkg.apis.dra.v1beta1.DRAPlugin/NodeUnprepareResources"
)

type Nodes struct {
	NodeNames []string
}

type Resources struct {
	NodeLocal bool

	// Nodes is a fixed list of node names on which resources are
	// available. Mutually exclusive with NodeLabels.
	Nodes []string

	// Number of devices called "device-000", "device-001", ... on each node or in the cluster.
	MaxAllocations int
}

//go:embed test-driver/deploy/example/plugin-permissions.yaml
var pluginPermissions string

// NewNodes selects nodes to run the test on.
//
// Call this outside of ginkgo.It, then use the instance inside ginkgo.It.
func NewNodes(f *framework.Framework, minNodes, maxNodes int) *Nodes {
	nodes := &Nodes{}
	ginkgo.BeforeEach(func(ctx context.Context) {
		nodes.init(ctx, f, minNodes, maxNodes)
	})
	return nodes
}

// NewNodesNow is a variant of NewNodes which can be used inside a ginkgo.It.
func NewNodesNow(ctx context.Context, f *framework.Framework, minNodes, maxNodes int) *Nodes {
	nodes := &Nodes{}
	nodes.init(ctx, f, minNodes, maxNodes)
	return nodes
}

func (nodes *Nodes) init(ctx context.Context, f *framework.Framework, minNodes, maxNodes int) {
	ginkgo.By("selecting nodes")
	// The kubelet plugin is harder. We deploy the builtin manifest
	// after patching in the driver name and all nodes on which we
	// want the plugin to run.
	//
	// Only a subset of the nodes are picked to avoid causing
	// unnecessary load on a big cluster.
	nodeList, err := e2enode.GetBoundedReadySchedulableNodes(ctx, f.ClientSet, maxNodes)
	framework.ExpectNoError(err, "get nodes")
	numNodes := int32(len(nodeList.Items))
	if int(numNodes) < minNodes {
		e2eskipper.Skipf("%d ready nodes required, only have %d", minNodes, numNodes)
	}
	nodes.NodeNames = nil
	for _, node := range nodeList.Items {
		nodes.NodeNames = append(nodes.NodeNames, node.Name)
	}
	sort.Strings(nodes.NodeNames)
	framework.Logf("testing on nodes %v", nodes.NodeNames)

	// Watch claims in the namespace. This is useful for monitoring a test
	// and enables additional sanity checks.
	claimInformer := resourceapiinformer.NewResourceClaimInformer(f.ClientSet, f.Namespace.Name, 100*time.Hour /* resync */, nil)
	cancelCtx, cancel := context.WithCancelCause(context.Background())
	var wg sync.WaitGroup
	ginkgo.DeferCleanup(func() {
		cancel(errors.New("test has completed"))
		wg.Wait()
	})
	_, err = claimInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) {
			defer ginkgo.GinkgoRecover()
			claim := obj.(*resourceapi.ResourceClaim)
			framework.Logf("New claim:\n%s", format.Object(claim, 1))
			validateClaim(claim)
		},
		UpdateFunc: func(oldObj, newObj any) {
			defer ginkgo.GinkgoRecover()
			oldClaim := oldObj.(*resourceapi.ResourceClaim)
			newClaim := newObj.(*resourceapi.ResourceClaim)
			framework.Logf("Updated claim:\n%s\nDiff:\n%s", format.Object(newClaim, 1), cmp.Diff(oldClaim, newClaim))
			validateClaim(newClaim)
		},
		DeleteFunc: func(obj any) {
			defer ginkgo.GinkgoRecover()
			claim := obj.(*resourceapi.ResourceClaim)
			framework.Logf("Deleted claim:\n%s", format.Object(claim, 1))
		},
	})
	framework.ExpectNoError(err, "AddEventHandler")
	wg.Add(1)
	go func() {
		defer wg.Done()
		claimInformer.Run(cancelCtx.Done())
	}()
}

func validateClaim(claim *resourceapi.ResourceClaim) {
	// The apiserver doesn't enforce that a claim always has a finalizer
	// while being allocated. This is a convention that whoever allocates a
	// claim has to follow to prevent using a claim that is at risk of
	// being deleted.
	if claim.Status.Allocation != nil && len(claim.Finalizers) == 0 {
		framework.Failf("Invalid claim: allocated without any finalizer:\n%s", format.Object(claim, 1))
	}
}

// NewDriver sets up controller (as client of the cluster) and
// kubelet plugin (via proxy) before the test runs. It cleans
// up after the test.
//
// Call this outside of ginkgo.It, then use the instance inside ginkgo.It.
func NewDriver(f *framework.Framework, nodes *Nodes, configureResources func() Resources, devicesPerNode ...map[string]map[resourceapi.QualifiedName]resourceapi.DeviceAttribute) *Driver {
	d := NewDriverInstance(f)

	ginkgo.BeforeEach(func() {
		d.Run(nodes, configureResources, devicesPerNode...)
	})
	return d
}

// NewDriverInstance is a variant of NewDriver where the driver is inactive and must
// be started explicitly with Run. May be used inside ginkgo.It.
func NewDriverInstance(f *framework.Framework) *Driver {
	d := &Driver{
		f:          f,
		fail:       map[MethodInstance]bool{},
		callCounts: map[MethodInstance]int64{},
		// By default, test only with the latest gRPC API.
		NodeV1alpha4: false,
		NodeV1beta1:  true,
	}
	d.initName()
	return d
}

func (d *Driver) Run(nodes *Nodes, configureResources func() Resources, devicesPerNode ...map[string]map[resourceapi.QualifiedName]resourceapi.DeviceAttribute) {
	resources := configureResources()
	if len(resources.Nodes) == 0 {
		// This always has to be set because the driver might
		// not run on all nodes.
		resources.Nodes = nodes.NodeNames
	}
	d.SetUp(nodes, resources, devicesPerNode...)
	ginkgo.DeferCleanup(d.TearDown)
}

type MethodInstance struct {
	Nodename   string
	FullMethod string
}

type Driver struct {
	f                  *framework.Framework
	ctx                context.Context
	cleanup            []func(context.Context) // executed first-in-first-out
	wg                 sync.WaitGroup
	serviceAccountName string

	// NameSuffix can be set while registering a test to deploy different
	// drivers in the same test namespace.
	NameSuffix string

	// InstanceSuffix can be set while registering a test to deploy two different
	// instances of the same driver. Used to generate unique objects in the API server.
	// The socket path is still the same.
	InstanceSuffix string

	// SocketSuffix can be set while registering a test to deploy two different
	// instances of the same driver where the socket paths are different. Must
	// be combined with setting InstanceSuffix.
	SocketSuffix string

	// Name gets derived automatically from the current test namespace and
	// (if set) the NameSuffix while setting up the driver for a test.
	Name string

	// Nodes contains entries for each node selected for a test when the test runs.
	// In addition, there is one entry for a fictional node.
	Nodes map[string]KubeletPlugin

	NodeV1alpha4 bool
	NodeV1beta1  bool

	mutex      sync.Mutex
	fail       map[MethodInstance]bool
	callCounts map[MethodInstance]int64
}

type KubeletPlugin struct {
	*app.ExamplePlugin
	ClientSet kubernetes.Interface
}

func (d *Driver) initName() {
	d.Name = d.f.UniqueName + d.NameSuffix + ".k8s.io"
}

func (d *Driver) SetUp(nodes *Nodes, resources Resources, devicesPerNode ...map[string]map[resourceapi.QualifiedName]resourceapi.DeviceAttribute) {
	d.initName()
	ginkgo.By(fmt.Sprintf("deploying driver %s on nodes %v", d.Name, nodes.NodeNames))
	d.Nodes = make(map[string]KubeletPlugin)

	ctx, cancel := context.WithCancel(context.Background())
	logger := klog.FromContext(ctx)
	logger = klog.LoggerWithValues(logger, "driverName", d.Name)
	if d.InstanceSuffix != "" {
		instance, _ := strings.CutPrefix(d.InstanceSuffix, "-")
		logger = klog.LoggerWithValues(logger, "instance", instance)
	}
	ctx = klog.NewContext(ctx, logger)
	d.ctx = ctx
	d.cleanup = append(d.cleanup, func(context.Context) { cancel() })

	if !resources.NodeLocal {
		// Publish one resource pool with "network-attached" devices.
		slice := &resourceapi.ResourceSlice{
			ObjectMeta: metav1.ObjectMeta{
				Name: d.Name, // globally unique
			},
			Spec: resourceapi.ResourceSliceSpec{
				Driver: d.Name,
				Pool: resourceapi.ResourcePool{
					Name:               "network",
					Generation:         1,
					ResourceSliceCount: 1,
				},
				NodeSelector: &v1.NodeSelector{
					NodeSelectorTerms: []v1.NodeSelectorTerm{{
						// MatchExpressions allow multiple values,
						// MatchFields don't.
						MatchExpressions: []v1.NodeSelectorRequirement{{
							Key:      "kubernetes.io/hostname",
							Operator: v1.NodeSelectorOpIn,
							Values:   nodes.NodeNames,
						}},
					}},
				},
			},
		}
		maxAllocations := resources.MaxAllocations
		if maxAllocations <= 0 {
			// Cannot be empty, otherwise nothing runs.
			maxAllocations = 10
		}
		for i := 0; i < maxAllocations; i++ {
			slice.Spec.Devices = append(slice.Spec.Devices, resourceapi.Device{
				Name:  fmt.Sprintf("device-%d", i),
				Basic: &resourceapi.BasicDevice{},
			})
		}

		_, err := d.f.ClientSet.ResourceV1beta1().ResourceSlices().Create(ctx, slice, metav1.CreateOptions{})
		framework.ExpectNoError(err)
		ginkgo.DeferCleanup(func(ctx context.Context) {
			framework.ExpectNoError(d.f.ClientSet.ResourceV1beta1().ResourceSlices().Delete(ctx, slice.Name, metav1.DeleteOptions{}))
		})
	}

	manifests := []string{
		// The code below matches the content of this manifest (ports,
		// container names, etc.).
		"test/e2e/testing-manifests/dra/dra-test-driver-proxy.yaml",
	}
	var numDevices = -1 // disabled
	if resources.NodeLocal {
		numDevices = resources.MaxAllocations
	}

	// Create service account and corresponding RBAC rules.
	d.serviceAccountName = "dra-kubelet-plugin-" + d.Name + d.InstanceSuffix + "-service-account"
	content := pluginPermissions
	content = strings.ReplaceAll(content, "dra-kubelet-plugin-namespace", d.f.Namespace.Name)
	content = strings.ReplaceAll(content, "dra-kubelet-plugin", "dra-kubelet-plugin-"+d.Name+d.InstanceSuffix)
	d.createFromYAML(ctx, []byte(content), d.f.Namespace.Name)

	// Using a ReplicaSet instead of a DaemonSet has the advantage that we can control
	// the lifecycle explicitly, in particular run two pods per node long enough to
	// run checks.
	instanceKey := "app.kubernetes.io/instance"
	rsName := ""
	// These two could have the same name because they are in different directories.
	// Using different names is merely for debugging and logging.
	registrationSocketFilename := d.Name + d.SocketSuffix + "-reg.sock"
	draSocketFilename := d.Name + d.SocketSuffix + "-dra.sock"
	draAddr := path.Join(framework.TestContext.KubeletRootDir, "plugins", draSocketFilename)
	numNodes := int32(len(nodes.NodeNames))
	instanceName := d.Name + d.InstanceSuffix
	err := utils.CreateFromManifests(ctx, d.f, d.f.Namespace, func(item interface{}) error {
		switch item := item.(type) {
		case *appsv1.ReplicaSet:
			item.Name += d.NameSuffix + d.InstanceSuffix
			rsName = item.Name
			item.Spec.Replicas = &numNodes
			item.Spec.Selector.MatchLabels[instanceKey] = instanceName
			item.Spec.Template.Labels[instanceKey] = instanceName
			item.Spec.Template.Spec.ServiceAccountName = d.serviceAccountName
			item.Spec.Template.Spec.Affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution[0].LabelSelector.MatchLabels[instanceKey] = instanceName
			item.Spec.Template.Spec.Affinity.NodeAffinity = &v1.NodeAffinity{
				RequiredDuringSchedulingIgnoredDuringExecution: &v1.NodeSelector{
					NodeSelectorTerms: []v1.NodeSelectorTerm{
						{
							MatchExpressions: []v1.NodeSelectorRequirement{
								{
									Key:      "kubernetes.io/hostname",
									Operator: v1.NodeSelectorOpIn,
									Values:   nodes.NodeNames,
								},
							},
						},
					},
				},
			}
			item.Spec.Template.Spec.Volumes[0].HostPath.Path = path.Join(framework.TestContext.KubeletRootDir, "plugins")
			item.Spec.Template.Spec.Volumes[2].HostPath.Path = path.Join(framework.TestContext.KubeletRootDir, "plugins_registry")
			item.Spec.Template.Spec.Containers[0].Args = append(item.Spec.Template.Spec.Containers[0].Args, "--endpoint=/plugins_registry/"+registrationSocketFilename)
			item.Spec.Template.Spec.Containers[1].Args = append(item.Spec.Template.Spec.Containers[1].Args, "--endpoint=/dra/"+draSocketFilename)
		}
		return nil
	}, manifests...)
	framework.ExpectNoError(err, "deploy kubelet plugin replicaset")

	rs, err := d.f.ClientSet.AppsV1().ReplicaSets(d.f.Namespace.Name).Get(ctx, rsName, metav1.GetOptions{})
	framework.ExpectNoError(err, "get replicaset")

	// Wait for all pods to be running.
	if err := e2ereplicaset.WaitForReplicaSetTargetAvailableReplicas(ctx, d.f.ClientSet, rs, numNodes); err != nil {
		framework.ExpectNoError(err, "all kubelet plugin proxies running")
	}
	requirement, err := labels.NewRequirement(instanceKey, selection.Equals, []string{instanceName})
	framework.ExpectNoError(err, "create label selector requirement")
	selector := labels.NewSelector().Add(*requirement)
	pods, err := d.f.ClientSet.CoreV1().Pods(d.f.Namespace.Name).List(ctx, metav1.ListOptions{LabelSelector: selector.String()})
	framework.ExpectNoError(err, "list proxy pods")
	gomega.Expect(numNodes).To(gomega.Equal(int32(len(pods.Items))), "number of proxy pods")
	sort.Slice(pods.Items, func(i, j int) bool {
		return pods.Items[i].Spec.NodeName < pods.Items[j].Spec.NodeName
	})

	// Run registrar and plugin for each of the pods.
	for i, pod := range pods.Items {
		// Need a local variable, not the loop variable, for the anonymous
		// callback functions below.
		pod := pod
		nodename := pod.Spec.NodeName

		// Authenticate the plugin so that it has the exact same
		// permissions as the daemonset pod. This includes RBAC and a
		// validating admission policy which limits writes to per-node
		// ResourceSlices.
		//
		// We could retrieve
		// /var/run/secrets/kubernetes.io/serviceaccount/token from
		// each pod and use it. That would check that
		// ServiceAccountTokenNodeBindingValidation works. But that's
		// better covered by a test owned by SIG Auth (like the one in
		// https://github.com/kubernetes/kubernetes/pull/124711).
		//
		// Here we merely use impersonation, which is faster.
		driverClient := d.impersonateKubeletPlugin(&pod)

		logger := klog.LoggerWithValues(klog.LoggerWithName(logger, "kubelet-plugin"), "node", pod.Spec.NodeName, "pod", klog.KObj(&pod))
		loggerCtx := klog.NewContext(ctx, logger)
		fileOps := app.FileOperations{
			Create: func(name string, content []byte) error {
				klog.Background().Info("creating CDI file", "node", nodename, "filename", name, "content", string(content))
				return d.createFile(&pod, name, content)
			},
			Remove: func(name string) error {
				klog.Background().Info("deleting CDI file", "node", nodename, "filename", name)
				return d.removeFile(&pod, name)
			},
		}
		if i < len(devicesPerNode) {
			fileOps.Devices = devicesPerNode[i]
			fileOps.NumDevices = -1
		} else {
			fileOps.NumDevices = numDevices
		}
		plugin, err := app.StartPlugin(loggerCtx, "/cdi", d.Name, driverClient, nodename, fileOps,
			kubeletplugin.GRPCVerbosity(0),
			kubeletplugin.GRPCInterceptor(func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp interface{}, err error) {
				return d.interceptor(nodename, ctx, req, info, handler)
			}),
			kubeletplugin.GRPCStreamInterceptor(func(srv interface{}, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) (err error) {
				return d.streamInterceptor(nodename, srv, ss, info, handler)
			}),
			kubeletplugin.PluginListener(listen(ctx, d.f, pod.Name, "plugin", 9001)),
			kubeletplugin.RegistrarListener(listen(ctx, d.f, pod.Name, "registrar", 9000)),
			kubeletplugin.KubeletPluginSocketPath(draAddr),
			kubeletplugin.NodeV1alpha4(d.NodeV1alpha4),
			kubeletplugin.NodeV1beta1(d.NodeV1beta1),
		)
		framework.ExpectNoError(err, "start kubelet plugin for node %s", pod.Spec.NodeName)
		d.cleanup = append(d.cleanup, func(ctx context.Context) {
			// Depends on cancel being called first.
			plugin.Stop()

			// Also explicitly stop all pods.
			ginkgo.By("scaling down driver proxy pods for " + d.Name)
			rs, err := d.f.ClientSet.AppsV1().ReplicaSets(d.f.Namespace.Name).Get(ctx, rsName, metav1.GetOptions{})
			framework.ExpectNoError(err, "get ReplicaSet for driver "+d.Name)
			rs.Spec.Replicas = ptr.To(int32(0))
			rs, err = d.f.ClientSet.AppsV1().ReplicaSets(d.f.Namespace.Name).Update(ctx, rs, metav1.UpdateOptions{})
			framework.ExpectNoError(err, "scale down ReplicaSet for driver "+d.Name)
			if err := e2ereplicaset.WaitForReplicaSetTargetAvailableReplicas(ctx, d.f.ClientSet, rs, 0); err != nil {
				framework.ExpectNoError(err, "all kubelet plugin proxies stopped")
			}
		})
		d.Nodes[nodename] = KubeletPlugin{ExamplePlugin: plugin, ClientSet: driverClient}
	}

	// Wait for registration.
	ginkgo.By("wait for plugin registration")
	gomega.Eventually(func() map[string][]app.GRPCCall {
		notRegistered := make(map[string][]app.GRPCCall)
		for nodename, plugin := range d.Nodes {
			calls := plugin.GetGRPCCalls()
			if contains, err := app.BeRegistered.Match(calls); err != nil || !contains {
				notRegistered[nodename] = calls
			}
		}
		return notRegistered
	}).WithTimeout(time.Minute).Should(gomega.BeEmpty(), "hosts where the plugin has not been registered yet")
}

func (d *Driver) impersonateKubeletPlugin(pod *v1.Pod) kubernetes.Interface {
	ginkgo.GinkgoHelper()
	driverUserInfo := (&serviceaccount.ServiceAccountInfo{
		Name:      d.serviceAccountName,
		Namespace: pod.Namespace,
		NodeName:  pod.Spec.NodeName,
		PodName:   pod.Name,
		PodUID:    string(pod.UID),
	}).UserInfo()
	driverClientConfig := d.f.ClientConfig()
	driverClientConfig.Impersonate = rest.ImpersonationConfig{
		UserName: driverUserInfo.GetName(),
		Groups:   driverUserInfo.GetGroups(),
		Extra:    driverUserInfo.GetExtra(),
	}
	driverClient, err := kubernetes.NewForConfig(driverClientConfig)
	framework.ExpectNoError(err, "create client for driver")
	return driverClient
}

func (d *Driver) createFile(pod *v1.Pod, name string, content []byte) error {
	buffer := bytes.NewBuffer(content)
	// Writing the content can be slow. Better create a temporary file and
	// move it to the final destination once it is complete.
	tmpName := name + ".tmp"
	if err := d.podIO(pod).CreateFile(tmpName, buffer); err != nil {
		_ = d.podIO(pod).RemoveAll(tmpName)
		return err
	}
	return d.podIO(pod).Rename(tmpName, name)
}

func (d *Driver) removeFile(pod *v1.Pod, name string) error {
	return d.podIO(pod).RemoveAll(name)
}

func (d *Driver) createFromYAML(ctx context.Context, content []byte, namespace string) {
	// Not caching the discovery result isn't very efficient, but good enough.
	discoveryCache := memory.NewMemCacheClient(d.f.ClientSet.Discovery())
	restMapper := restmapper.NewDeferredDiscoveryRESTMapper(discoveryCache)

	for _, content := range bytes.Split(content, []byte("---\n")) {
		if len(content) == 0 {
			continue
		}

		var obj *unstructured.Unstructured
		framework.ExpectNoError(yaml.UnmarshalStrict(content, &obj), fmt.Sprintf("Full YAML:\n%s\n", string(content)))

		gv, err := schema.ParseGroupVersion(obj.GetAPIVersion())
		framework.ExpectNoError(err, fmt.Sprintf("extract group+version from object %q", klog.KObj(obj)))
		gk := schema.GroupKind{Group: gv.Group, Kind: obj.GetKind()}

		mapping, err := restMapper.RESTMapping(gk, gv.Version)
		framework.ExpectNoError(err, fmt.Sprintf("map %q to resource", gk))

		resourceClient := d.f.DynamicClient.Resource(mapping.Resource)
		options := metav1.CreateOptions{
			// If the YAML input is invalid, then we want the
			// apiserver to tell us via an error. This can
			// happen because decoding into an unstructured object
			// doesn't validate.
			FieldValidation: "Strict",
		}
		switch mapping.Scope.Name() {
		case meta.RESTScopeNameRoot:
			_, err = resourceClient.Create(ctx, obj, options)
		case meta.RESTScopeNameNamespace:
			if namespace == "" {
				framework.Failf("need namespace for object type %s", gk)
			}
			_, err = resourceClient.Namespace(namespace).Create(ctx, obj, options)
		}
		framework.ExpectNoError(err, "create object")
		ginkgo.DeferCleanup(func(ctx context.Context) {
			del := resourceClient.Delete
			if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
				del = resourceClient.Namespace(namespace).Delete
			}
			err := del(ctx, obj.GetName(), metav1.DeleteOptions{})
			if !apierrors.IsNotFound(err) {
				framework.ExpectNoError(err, fmt.Sprintf("deleting %s.%s %s", obj.GetKind(), obj.GetAPIVersion(), klog.KObj(obj)))
			}
		})
	}
}

func (d *Driver) podIO(pod *v1.Pod) proxy.PodDirIO {
	logger := klog.Background()
	return proxy.PodDirIO{
		F:             d.f,
		Namespace:     pod.Namespace,
		PodName:       pod.Name,
		ContainerName: "plugin",
		Logger:        &logger,
	}
}

func listen(ctx context.Context, f *framework.Framework, podName, containerName string, port int) net.Listener {
	addr := proxy.Addr{
		Namespace:     f.Namespace.Name,
		PodName:       podName,
		ContainerName: containerName,
		Port:          port,
	}
	listener, err := proxy.Listen(ctx, f.ClientSet, f.ClientConfig(), addr)
	framework.ExpectNoError(err, "listen for connections from %+v", addr)
	return listener
}

func (d *Driver) TearDown(ctx context.Context) {
	for _, c := range d.cleanup {
		c(ctx)
	}
	d.cleanup = nil
	d.wg.Wait()
}

// IsGone checks that the kubelet is done with the driver.
// This is done by waiting for the kubelet to remove the
// driver's ResourceSlices, which takes at least 5 minutes
// because of the delay in the kubelet. Only use this in slow
// tests...
func (d *Driver) IsGone(ctx context.Context) {
	gomega.Eventually(ctx, func(ctx context.Context) ([]resourceapi.ResourceSlice, error) {
		slices, err := d.f.ClientSet.ResourceV1beta1().ResourceSlices().List(ctx, metav1.ListOptions{FieldSelector: resourceapi.ResourceSliceSelectorDriver + "=" + d.Name})
		if err != nil {
			return nil, err
		}
		return slices.Items, err
	}).WithTimeout(7 * time.Minute).Should(gomega.BeEmpty())
}

func (d *Driver) interceptor(nodename string, ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp interface{}, err error) {
	d.mutex.Lock()
	defer d.mutex.Unlock()

	m := MethodInstance{nodename, info.FullMethod}
	d.callCounts[m]++
	if d.fail[m] {
		return nil, errors.New("injected error")
	}

	return handler(ctx, req)
}

func (d *Driver) streamInterceptor(nodename string, srv interface{}, stream grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	// Stream calls block for a long time. We must not hold the lock while
	// they are running.
	d.mutex.Lock()
	m := MethodInstance{nodename, info.FullMethod}
	d.callCounts[m]++
	fail := d.fail[m]
	d.mutex.Unlock()

	if fail {
		return errors.New("injected error")
	}

	return handler(srv, stream)
}

func (d *Driver) Fail(m MethodInstance, injectError bool) {
	d.mutex.Lock()
	defer d.mutex.Unlock()

	d.fail[m] = injectError
}

func (d *Driver) CallCount(m MethodInstance) int64 {
	d.mutex.Lock()
	defer d.mutex.Unlock()

	return d.callCounts[m]
}

func (d *Driver) Nodenames() (nodenames []string) {
	for nodename := range d.Nodes {
		nodenames = append(nodenames, nodename)
	}
	sort.Strings(nodenames)
	return
}
