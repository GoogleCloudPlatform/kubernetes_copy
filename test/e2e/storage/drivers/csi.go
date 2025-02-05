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

/*
 * This file defines various csi volume test drivers for TestSuites.
 *
 * There are two ways, how to prepare test drivers:
 * 1) With containerized server (NFS, Ceph, iSCSI, ...)
 * It creates a server pod which defines one volume for the tests.
 * These tests work only when privileged containers are allowed, exporting
 * various filesystems (ex: NFS) usually needs some mounting or
 * other privileged magic in the server pod.
 *
 * Note that the server containers are for testing purposes only and should not
 * be used in production.
 *
 * 2) With server or cloud provider outside of Kubernetes (Cinder, GCE, AWS, Azure, ...)
 * Appropriate server or cloud provider must exist somewhere outside
 * the tested Kubernetes cluster. CreateVolume will create a new volume to be
 * used in the TestSuites for inlineVolume or DynamicPV tests.
 */

package drivers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/onsi/ginkgo/v2"
	spb "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	storagev1 "k8s.io/api/storage/v1"
	storagev1beta1 "k8s.io/api/storage/v1beta1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/test/e2e/feature"
	"k8s.io/kubernetes/test/e2e/framework"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"
	e2evolume "k8s.io/kubernetes/test/e2e/framework/volume"
	mockdriver "k8s.io/kubernetes/test/e2e/storage/drivers/csi-test/driver"
	mockservice "k8s.io/kubernetes/test/e2e/storage/drivers/csi-test/mock/service"
	"k8s.io/kubernetes/test/e2e/storage/drivers/proxy"
	storageframework "k8s.io/kubernetes/test/e2e/storage/framework"
	"k8s.io/kubernetes/test/e2e/storage/utils"

	"google.golang.org/grpc"
)

const (
	// GCEPDCSIDriverName is the name of GCE Persistent Disk CSI driver
	GCEPDCSIDriverName = "pd.csi.storage.gke.io"
	// GCEPDCSIZoneTopologyKey is the key of GCE Persistent Disk CSI zone topology
	GCEPDCSIZoneTopologyKey = "topology.gke.io/zone"

	// Prefix of the mock driver grpc log
	grpcCallPrefix = "gRPCCall:"

	// Parameter to use in hostpath CSI driver VolumeAttributesClass
	// Must be passed to the driver via --accepted-mutable-parameter-names
	hostpathCSIDriverMutableParameterName  = "e2eVacTest"
	hostpathCSIDriverMutableParameterValue = "test-value"
)

// hostpathCSI
type hostpathCSIDriver struct {
	driverInfo       storageframework.DriverInfo
	manifests        []string
	volumeAttributes []map[string]string
}

func initHostPathCSIDriver(name string, capabilities map[storageframework.Capability]bool, volumeAttributes []map[string]string, manifests ...string) storageframework.TestDriver {
	return &hostpathCSIDriver{
		driverInfo: storageframework.DriverInfo{
			Name:        name,
			MaxFileSize: storageframework.FileSizeMedium,
			SupportedFsType: sets.NewString(
				"", // Default fsType
			),
			SupportedSizeRange: e2evolume.SizeRange{
				Min: "1Mi",
			},
			Capabilities: capabilities,
			StressTestOptions: &storageframework.StressTestOptions{
				NumPods:     10,
				NumRestarts: 10,
			},
			VolumeSnapshotStressTestOptions: &storageframework.VolumeSnapshotStressTestOptions{
				NumPods:      10,
				NumSnapshots: 10,
			},
			PerformanceTestOptions: &storageframework.PerformanceTestOptions{
				ProvisioningOptions: &storageframework.PerformanceTestProvisioningOptions{
					VolumeSize: "1Mi",
					Count:      300,
					// Volume provisioning metrics are compared to a high baseline.
					// Failure to pass would suggest a performance regression.
					ExpectedMetrics: &storageframework.Metrics{
						AvgLatency: 2 * time.Minute,
						Throughput: 0.5,
					},
				},
			},
		},
		manifests:        manifests,
		volumeAttributes: volumeAttributes,
	}
}

var _ storageframework.TestDriver = &hostpathCSIDriver{}
var _ storageframework.DynamicPVTestDriver = &hostpathCSIDriver{}
var _ storageframework.SnapshottableTestDriver = &hostpathCSIDriver{}
var _ storageframework.EphemeralTestDriver = &hostpathCSIDriver{}

// InitHostPathCSIDriver returns hostpathCSIDriver that implements TestDriver interface
func InitHostPathCSIDriver() storageframework.TestDriver {
	capabilities := map[storageframework.Capability]bool{
		storageframework.CapPersistence:                    true,
		storageframework.CapSnapshotDataSource:             true,
		storageframework.CapMultiPODs:                      true,
		storageframework.CapBlock:                          true,
		storageframework.CapPVCDataSource:                  true,
		storageframework.CapControllerExpansion:            true,
		storageframework.CapOfflineExpansion:               true,
		storageframework.CapOnlineExpansion:                true,
		storageframework.CapSingleNodeVolume:               true,
		storageframework.CapReadWriteOncePod:               true,
		storageframework.CapMultiplePVsSameID:              true,
		storageframework.CapFSResizeFromSourceNotSupported: true,
		storageframework.CapVolumeGroupSnapshot:            true,

		// This is needed for the
		// testsuites/volumelimits.go `should support volume limits`
		// test. --maxvolumespernode=10 gets
		// added when patching the deployment.
		storageframework.CapVolumeLimits: true,
	}
	return initHostPathCSIDriver("csi-hostpath",
		capabilities,
		// Volume attributes don't matter, but we have to provide at least one map.
		[]map[string]string{
			{"foo": "bar"},
		},
		"test/e2e/testing-manifests/storage-csi/external-attacher/rbac.yaml",
		"test/e2e/testing-manifests/storage-csi/external-provisioner/rbac.yaml",
		"test/e2e/testing-manifests/storage-csi/external-snapshotter/csi-snapshotter/rbac-csi-snapshotter.yaml",
		"test/e2e/testing-manifests/storage-csi/external-health-monitor/external-health-monitor-controller/rbac.yaml",
		"test/e2e/testing-manifests/storage-csi/external-resizer/rbac.yaml",
		"test/e2e/testing-manifests/storage-csi/hostpath/hostpath/csi-hostpath-driverinfo.yaml",
		"test/e2e/testing-manifests/storage-csi/hostpath/hostpath/csi-hostpath-plugin.yaml",
		"test/e2e/testing-manifests/storage-csi/hostpath/hostpath/e2e-test-rbac.yaml",
	)
}

func (h *hostpathCSIDriver) GetDriverInfo() *storageframework.DriverInfo {
	return &h.driverInfo
}

func (h *hostpathCSIDriver) SkipUnsupportedTest(pattern storageframework.TestPattern) {
	if pattern.VolType == storageframework.CSIInlineVolume && len(h.volumeAttributes) == 0 {
		e2eskipper.Skipf("%s has no volume attributes defined, doesn't support ephemeral inline volumes", h.driverInfo.Name)
	}
}

func (h *hostpathCSIDriver) GetDynamicProvisionStorageClass(ctx context.Context, config *storageframework.PerTestConfig, fsType string) *storagev1.StorageClass {
	provisioner := config.GetUniqueDriverName()
	parameters := map[string]string{}
	ns := config.Framework.Namespace.Name

	return storageframework.GetStorageClass(provisioner, parameters, nil, ns)
}

func (h *hostpathCSIDriver) GetVolume(config *storageframework.PerTestConfig, volumeNumber int) (map[string]string, bool, bool) {
	return h.volumeAttributes[volumeNumber%len(h.volumeAttributes)], false /* not shared */, false /* read-write */
}

func (h *hostpathCSIDriver) GetCSIDriverName(config *storageframework.PerTestConfig) string {
	return config.GetUniqueDriverName()
}

func (h *hostpathCSIDriver) GetSnapshotClass(ctx context.Context, config *storageframework.PerTestConfig, parameters map[string]string) *unstructured.Unstructured {
	snapshotter := config.GetUniqueDriverName()
	ns := config.Framework.Namespace.Name

	return utils.GenerateSnapshotClassSpec(snapshotter, parameters, ns)
}

func (h *hostpathCSIDriver) GetVolumeAttributesClass(_ context.Context, config *storageframework.PerTestConfig) *storagev1beta1.VolumeAttributesClass {
	return storageframework.CopyVolumeAttributesClass(&storagev1beta1.VolumeAttributesClass{
		DriverName: config.GetUniqueDriverName(),
		Parameters: map[string]string{
			hostpathCSIDriverMutableParameterName: hostpathCSIDriverMutableParameterValue,
		},
	}, config.Framework.Namespace.Name, "e2e-vac-hostpath")
}
func (h *hostpathCSIDriver) GetVolumeGroupSnapshotClass(ctx context.Context, config *storageframework.PerTestConfig, parameters map[string]string) *unstructured.Unstructured {
	snapshotter := config.GetUniqueDriverName()
	ns := config.Framework.Namespace.Name

	return utils.GenerateVolumeGroupSnapshotClassSpec(snapshotter, parameters, ns)
}

func (h *hostpathCSIDriver) PrepareTest(ctx context.Context, f *framework.Framework) *storageframework.PerTestConfig {
	// Create secondary namespace which will be used for creating driver
	driverNamespace := utils.CreateDriverNamespace(ctx, f)
	driverns := driverNamespace.Name

	ginkgo.By(fmt.Sprintf("deploying %s driver", h.driverInfo.Name))
	cancelLogging := utils.StartPodLogs(ctx, f, driverNamespace)
	cs := f.ClientSet

	// The hostpath CSI driver only works when everything runs on the same node.
	node, err := e2enode.GetRandomReadySchedulableNode(ctx, cs)
	framework.ExpectNoError(err)
	config := &storageframework.PerTestConfig{
		Driver:              h,
		Prefix:              "hostpath",
		Framework:           f,
		ClientNodeSelection: e2epod.NodeSelection{Name: node.Name},
		DriverNamespace:     driverNamespace,
	}

	patches := []utils.PatchCSIOptions{}

	patches = append(patches, utils.PatchCSIOptions{
		OldDriverName:       h.driverInfo.Name,
		NewDriverName:       config.GetUniqueDriverName(),
		DriverContainerName: "hostpath",
		DriverContainerArguments: []string{"--drivername=" + config.GetUniqueDriverName(),
			// This is needed for the
			// testsuites/volumelimits.go `should support volume limits`
			// test.
			"--maxvolumespernode=10",
			// Enable volume lifecycle checks, to report failure if
			// the volume is not unpublished / unstaged correctly.
			"--check-volume-lifecycle=true",
		},
		ProvisionerContainerName: "csi-provisioner",
		SnapshotterContainerName: "csi-snapshotter",
		NodeName:                 node.Name,
	})

	// VAC E2E HostPath patch
	// Enables ModifyVolume support in the hostpath CSI driver, and adds an enabled parameter name
	patches = append(patches, utils.PatchCSIOptions{
		DriverContainerName:      "hostpath",
		DriverContainerArguments: []string{"--enable-controller-modify-volume=true", "--accepted-mutable-parameter-names=e2eVacTest"},
	})

	// VAC E2E FeatureGate patches
	// TODO: These can be removed after the VolumeAttributesClass feature is default enabled
	patches = append(patches, utils.PatchCSIOptions{
		DriverContainerName:      "csi-provisioner",
		DriverContainerArguments: []string{"--feature-gates=VolumeAttributesClass=true"},
	})
	patches = append(patches, utils.PatchCSIOptions{
		DriverContainerName:      "csi-resizer",
		DriverContainerArguments: []string{"--feature-gates=VolumeAttributesClass=true"},
	})

	err = utils.CreateFromManifests(ctx, config.Framework, driverNamespace, func(item interface{}) error {
		for _, o := range patches {
			if err := utils.PatchCSIDeployment(config.Framework, o, item); err != nil {
				return err
			}
		}

		// Remove csi-external-health-monitor-agent and
		// csi-external-health-monitor-controller
		// containers. The agent is obsolete.
		// The controller is not needed for any of the
		// tests and is causing too much overhead when
		// running in a large cluster (see
		// https://github.com/kubernetes/kubernetes/issues/102452#issuecomment-856991009).
		switch item := item.(type) {
		case *appsv1.StatefulSet:
			var containers []v1.Container
			for _, container := range item.Spec.Template.Spec.Containers {
				switch container.Name {
				case "csi-external-health-monitor-agent", "csi-external-health-monitor-controller":
					// Remove these containers.
				default:
					// Keep the others.
					containers = append(containers, container)
				}
			}
			item.Spec.Template.Spec.Containers = containers
		}
		return nil
	}, h.manifests...)

	if err != nil {
		framework.Failf("deploying %s driver: %v", h.driverInfo.Name, err)
	}

	cleanupFunc := generateDriverCleanupFunc(
		f,
		h.driverInfo.Name,
		driverns,
		cancelLogging)
	ginkgo.DeferCleanup(cleanupFunc)

	return config
}

// mockCSI
type mockCSIDriver struct {
	driverInfo                    storageframework.DriverInfo
	manifests                     []string
	podInfo                       *bool
	storageCapacity               *bool
	attachable                    bool
	attachLimit                   int
	enableTopology                bool
	enableNodeExpansion           bool
	hooks                         Hooks
	tokenRequests                 []storagev1.TokenRequest
	requiresRepublish             *bool
	fsGroupPolicy                 *storagev1.FSGroupPolicy
	enableVolumeMountGroup        bool
	enableNodeVolumeCondition     bool
	embedded                      bool
	calls                         MockCSICalls
	embeddedCSIDriver             *mockdriver.CSIDriver
	enableSELinuxMount            *bool
	enableRecoverExpansionFailure bool
	disableControllerExpansion    bool

	// Additional values set during PrepareTest
	clientSet       clientset.Interface
	driverNamespace *v1.Namespace
}

// Hooks to be run to execute while handling gRPC calls.
//
// At the moment, only generic pre- and post-function call
// hooks are implemented. Those hooks can cast the request and
// response values if needed. More hooks inside specific
// functions could be added if needed.
type Hooks struct {
	// Pre is called before invoking the mock driver's implementation of a method.
	// If either a non-nil reply or error are returned, then those are returned to the caller.
	Pre func(ctx context.Context, method string, request interface{}) (reply interface{}, err error)

	// Post is called after invoking the mock driver's implementation of a method.
	// What it returns is used as actual result.
	Post func(ctx context.Context, method string, request, reply interface{}, err error) (finalReply interface{}, finalErr error)
}

// MockCSITestDriver provides additional functions specific to the CSI mock driver.
type MockCSITestDriver interface {
	storageframework.DynamicPVTestDriver

	// GetCalls returns all currently observed gRPC calls. Only valid
	// after PrepareTest.
	GetCalls(ctx context.Context) ([]MockCSICall, error)
}

// CSIMockDriverOpts defines options used for csi driver
type CSIMockDriverOpts struct {
	RegisterDriver                bool
	DisableAttach                 bool
	PodInfo                       *bool
	StorageCapacity               *bool
	AttachLimit                   int
	EnableTopology                bool
	EnableResizing                bool
	EnableNodeExpansion           bool
	DisableControllerExpansion    bool
	EnableSnapshot                bool
	EnableVolumeMountGroup        bool
	EnableNodeVolumeCondition     bool
	TokenRequests                 []storagev1.TokenRequest
	RequiresRepublish             *bool
	FSGroupPolicy                 *storagev1.FSGroupPolicy
	EnableSELinuxMount            *bool
	EnableRecoverExpansionFailure bool

	// Embedded defines whether the CSI mock driver runs
	// inside the cluster (false, the default) or just a proxy
	// runs inside the cluster and all gRPC calls are handled
	// inside the e2e.test binary.
	Embedded bool

	// Hooks that will be called if (and only if!) the embedded
	// mock driver is used. Beware that hooks are invoked
	// asynchronously in different goroutines.
	Hooks Hooks
}

// Dummy structure that parses just volume_attributes and error code out of logged CSI call
type MockCSICall struct {
	json string // full log entry

	Method  string
	Request struct {
		VolumeContext map[string]string `json:"volume_context"`
		Secrets       map[string]string `json:"secrets"`
	}
	FullError struct {
		Code    codes.Code `json:"code"`
		Message string     `json:"message"`
	}
	Error string
}

// MockCSICalls is a Thread-safe storage for MockCSICall instances.
type MockCSICalls struct {
	calls []MockCSICall
	mutex sync.Mutex
}

// Get returns all currently recorded calls.
func (c *MockCSICalls) Get() []MockCSICall {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	return c.calls[:]
}

// Add appends one new call at the end.
func (c *MockCSICalls) Add(call MockCSICall) {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	c.calls = append(c.calls, call)
}

// LogGRPC takes individual parameters from the mock CSI driver and adds them.
func (c *MockCSICalls) LogGRPC(method string, request, reply interface{}, err error) {
	// Encoding to JSON and decoding mirrors the traditional way of capturing calls.
	// Probably could be simplified now...
	logMessage := struct {
		Method   string
		Request  interface{}
		Response interface{}
		// Error as string, for backward compatibility.
		// "" on no error.
		Error string
		// Full error dump, to be able to parse out full gRPC error code and message separately in a test.
		FullError *spb.Status
	}{
		Method:   method,
		Request:  request,
		Response: reply,
	}

	if err != nil {
		logMessage.Error = err.Error()
		logMessage.FullError = grpcstatus.Convert(err).Proto()
	}

	msg, _ := json.Marshal(logMessage)
	call := MockCSICall{
		json: string(msg),
	}
	json.Unmarshal(msg, &call)

	klog.Infof("%s %s", grpcCallPrefix, string(msg))

	// Trim gRPC service name, i.e. "/csi.v1.Identity/Probe" -> "Probe"
	methodParts := strings.Split(call.Method, "/")
	call.Method = methodParts[len(methodParts)-1]

	c.Add(call)
}

var _ storageframework.TestDriver = &mockCSIDriver{}
var _ storageframework.DynamicPVTestDriver = &mockCSIDriver{}
var _ storageframework.SnapshottableTestDriver = &mockCSIDriver{}

// InitMockCSIDriver returns a mockCSIDriver that implements TestDriver interface
func InitMockCSIDriver(driverOpts CSIMockDriverOpts) MockCSITestDriver {
	driverManifests := []string{
		"test/e2e/testing-manifests/storage-csi/external-attacher/rbac.yaml",
		"test/e2e/testing-manifests/storage-csi/external-provisioner/rbac.yaml",
		"test/e2e/testing-manifests/storage-csi/external-resizer/rbac.yaml",
		"test/e2e/testing-manifests/storage-csi/external-snapshotter/csi-snapshotter/rbac-csi-snapshotter.yaml",
		"test/e2e/testing-manifests/storage-csi/mock/csi-mock-rbac.yaml",
		"test/e2e/testing-manifests/storage-csi/mock/csi-storageclass.yaml",
	}
	if driverOpts.Embedded {
		driverManifests = append(driverManifests, "test/e2e/testing-manifests/storage-csi/mock/csi-mock-proxy.yaml")
	} else {
		driverManifests = append(driverManifests, "test/e2e/testing-manifests/storage-csi/mock/csi-mock-driver.yaml")
	}

	if driverOpts.RegisterDriver {
		driverManifests = append(driverManifests, "test/e2e/testing-manifests/storage-csi/mock/csi-mock-driverinfo.yaml")
	}

	if !driverOpts.DisableAttach {
		driverManifests = append(driverManifests, "test/e2e/testing-manifests/storage-csi/mock/csi-mock-driver-attacher.yaml")
	}

	if driverOpts.EnableResizing {
		driverManifests = append(driverManifests, "test/e2e/testing-manifests/storage-csi/mock/csi-mock-driver-resizer.yaml")
	}

	if driverOpts.EnableSnapshot {
		driverManifests = append(driverManifests, "test/e2e/testing-manifests/storage-csi/mock/csi-mock-driver-snapshotter.yaml")
	}

	return &mockCSIDriver{
		driverInfo: storageframework.DriverInfo{
			Name:        "csi-mock",
			MaxFileSize: storageframework.FileSizeMedium,
			SupportedFsType: sets.NewString(
				"", // Default fsType
			),
			Capabilities: map[storageframework.Capability]bool{
				storageframework.CapPersistence:       false,
				storageframework.CapFsGroup:           false,
				storageframework.CapExec:              false,
				storageframework.CapVolumeLimits:      true,
				storageframework.CapMultiplePVsSameID: true,
			},
		},
		manifests:                     driverManifests,
		podInfo:                       driverOpts.PodInfo,
		storageCapacity:               driverOpts.StorageCapacity,
		enableTopology:                driverOpts.EnableTopology,
		attachable:                    !driverOpts.DisableAttach,
		attachLimit:                   driverOpts.AttachLimit,
		enableNodeExpansion:           driverOpts.EnableNodeExpansion,
		enableNodeVolumeCondition:     driverOpts.EnableNodeVolumeCondition,
		disableControllerExpansion:    driverOpts.DisableControllerExpansion,
		tokenRequests:                 driverOpts.TokenRequests,
		requiresRepublish:             driverOpts.RequiresRepublish,
		fsGroupPolicy:                 driverOpts.FSGroupPolicy,
		enableVolumeMountGroup:        driverOpts.EnableVolumeMountGroup,
		enableSELinuxMount:            driverOpts.EnableSELinuxMount,
		enableRecoverExpansionFailure: driverOpts.EnableRecoverExpansionFailure,
		embedded:                      driverOpts.Embedded,
		hooks:                         driverOpts.Hooks,
	}
}

func (m *mockCSIDriver) GetDriverInfo() *storageframework.DriverInfo {
	return &m.driverInfo
}

func (m *mockCSIDriver) SkipUnsupportedTest(pattern storageframework.TestPattern) {
}

func (m *mockCSIDriver) GetDynamicProvisionStorageClass(ctx context.Context, config *storageframework.PerTestConfig, fsType string) *storagev1.StorageClass {
	provisioner := config.GetUniqueDriverName()
	parameters := map[string]string{}
	ns := config.Framework.Namespace.Name

	return storageframework.GetStorageClass(provisioner, parameters, nil, ns)
}

func (m *mockCSIDriver) GetSnapshotClass(ctx context.Context, config *storageframework.PerTestConfig, parameters map[string]string) *unstructured.Unstructured {
	snapshotter := m.driverInfo.Name + "-" + config.Framework.UniqueName
	ns := config.Framework.Namespace.Name

	return utils.GenerateSnapshotClassSpec(snapshotter, parameters, ns)
}

func (m *mockCSIDriver) PrepareTest(ctx context.Context, f *framework.Framework) *storageframework.PerTestConfig {
	m.clientSet = f.ClientSet

	// Create secondary namespace which will be used for creating driver
	m.driverNamespace = utils.CreateDriverNamespace(ctx, f)
	driverns := m.driverNamespace.Name

	if m.embedded {
		ginkgo.By("deploying csi mock proxy")
	} else {
		ginkgo.By("deploying csi mock driver")
	}
	cancelLogging := utils.StartPodLogs(ctx, f, m.driverNamespace)
	cs := f.ClientSet

	// pods should be scheduled on the node
	node, err := e2enode.GetRandomReadySchedulableNode(ctx, cs)
	framework.ExpectNoError(err)

	embeddedCleanup := func() {}
	containerArgs := []string{}
	if m.embedded {
		// Run embedded CSI driver.
		//
		// For now we start exactly one instance which implements controller,
		// node and identity services. It matches with the one pod that we run
		// inside the cluster. The name and namespace of that one is deterministic,
		// so we know what to connect to.
		//
		// Long-term we could also deploy one central controller and multiple
		// node instances, with knowledge about provisioned volumes shared in
		// this process.
		podname := "csi-mockplugin-0"
		containername := "mock"

		// Must keep running even after the test context is cancelled
		// for cleanup callbacks.
		ctx, cancel := context.WithCancel(context.Background())
		serviceConfig := mockservice.Config{
			DisableAttach:               !m.attachable,
			DriverName:                  "csi-mock-" + f.UniqueName,
			AttachLimit:                 int64(m.attachLimit),
			NodeExpansionRequired:       m.enableNodeExpansion,
			DisableControllerExpansion:  m.disableControllerExpansion,
			NodeVolumeConditionRequired: m.enableNodeVolumeCondition,
			VolumeMountGroupRequired:    m.enableVolumeMountGroup,
			EnableTopology:              m.enableTopology,
			IO: proxy.PodDirIO{
				F:             f,
				Namespace:     m.driverNamespace.Name,
				PodName:       podname,
				ContainerName: "busybox",
			},
		}
		s := mockservice.New(serviceConfig)
		servers := &mockdriver.CSIDriverServers{
			Controller: s,
			Identity:   s,
			Node:       s,
		}
		m.embeddedCSIDriver = mockdriver.NewCSIDriver(servers)
		l, err := proxy.Listen(ctx, f.ClientSet, f.ClientConfig(),
			proxy.Addr{
				Namespace:     m.driverNamespace.Name,
				PodName:       podname,
				ContainerName: containername,
				Port:          9000,
			},
		)

		framework.ExpectNoError(err, "start connecting to proxy pod")
		err = m.embeddedCSIDriver.Start(l, m.interceptGRPC)
		framework.ExpectNoError(err, "start mock driver")

		embeddedCleanup = func() {
			// Kill all goroutines and delete resources of the mock driver.
			m.embeddedCSIDriver.Stop()
			l.Close()
			cancel()
		}
	} else {
		// When using the mock driver inside the cluster it has to be reconfigured
		// via command line parameters.
		containerArgs = append(containerArgs, "--drivername=csi-mock-"+f.UniqueName)

		if m.attachable {
			containerArgs = append(containerArgs, "--enable-attach")
		}

		if m.enableTopology {
			containerArgs = append(containerArgs, "--enable-topology")
		}

		if m.attachLimit > 0 {
			containerArgs = append(containerArgs, "--attach-limit", strconv.Itoa(m.attachLimit))
		}

		if m.enableNodeExpansion {
			containerArgs = append(containerArgs, "--node-expand-required=true")
		}
	}

	config := &storageframework.PerTestConfig{
		Driver:              m,
		Prefix:              "mock",
		Framework:           f,
		ClientNodeSelection: e2epod.NodeSelection{Name: node.Name},
		DriverNamespace:     m.driverNamespace,
	}

	o := utils.PatchCSIOptions{
		OldDriverName:            "csi-mock",
		NewDriverName:            "csi-mock-" + f.UniqueName,
		DriverContainerName:      "mock",
		DriverContainerArguments: containerArgs,
		ProvisionerContainerName: "csi-provisioner",
		NodeName:                 node.Name,
		PodInfo:                  m.podInfo,
		StorageCapacity:          m.storageCapacity,
		CanAttach:                &m.attachable,
		VolumeLifecycleModes: &[]storagev1.VolumeLifecycleMode{
			storagev1.VolumeLifecyclePersistent,
			storagev1.VolumeLifecycleEphemeral,
		},
		TokenRequests:     m.tokenRequests,
		RequiresRepublish: m.requiresRepublish,
		FSGroupPolicy:     m.fsGroupPolicy,
		SELinuxMount:      m.enableSELinuxMount,
		Features:          map[string][]string{},
	}

	if m.enableRecoverExpansionFailure {
		o.Features["csi-resizer"] = []string{"RecoverVolumeExpansionFailure=true"}
	}

	err = utils.CreateFromManifests(ctx, f, m.driverNamespace, func(item interface{}) error {
		if err := utils.PatchCSIDeployment(config.Framework, o, item); err != nil {
			return err
		}

		switch item := item.(type) {
		case *rbacv1.ClusterRole:
			if strings.HasPrefix(item.Name, "external-snapshotter-runner") {
				// Re-enable access to secrets for the snapshotter sidecar for
				// https://github.com/kubernetes/kubernetes/blob/6ede5ca95f78478fa627ecfea8136e0dff34436b/test/e2e/storage/csi_mock_volume.go#L1539-L1548
				// It was disabled in https://github.com/kubernetes-csi/external-snapshotter/blob/501cc505846c03ee665355132f2da0ce7d5d747d/deploy/kubernetes/csi-snapshotter/rbac-csi-snapshotter.yaml#L26-L32
				item.Rules = append(item.Rules, rbacv1.PolicyRule{
					APIGroups: []string{""},
					Resources: []string{"secrets"},
					Verbs:     []string{"get", "list"},
				})
			}
		}

		return nil
	}, m.manifests...)

	if err != nil {
		framework.Failf("deploying csi mock driver: %v", err)
	}

	driverCleanupFunc := generateDriverCleanupFunc(
		f,
		"mock",
		driverns,
		cancelLogging)

	ginkgo.DeferCleanup(func(ctx context.Context) {
		embeddedCleanup()
		driverCleanupFunc(ctx)
	})

	return config
}

func (m *mockCSIDriver) interceptGRPC(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp interface{}, err error) {
	defer func() {
		// Always log the call and its final result,
		// regardless whether the result was from the real
		// implementation or a hook.
		m.calls.LogGRPC(info.FullMethod, req, resp, err)
	}()

	if m.hooks.Pre != nil {
		resp, err = m.hooks.Pre(ctx, info.FullMethod, req)
		if resp != nil || err != nil {
			return
		}
	}
	resp, err = handler(ctx, req)
	if m.hooks.Post != nil {
		resp, err = m.hooks.Post(ctx, info.FullMethod, req, resp, err)
	}
	return
}

func (m *mockCSIDriver) GetCalls(ctx context.Context) ([]MockCSICall, error) {
	if m.embedded {
		return m.calls.Get(), nil
	}

	if m.driverNamespace == nil {
		return nil, errors.New("PrepareTest not called yet")
	}

	// Name of CSI driver pod name (it's in a StatefulSet with a stable name)
	driverPodName := "csi-mockplugin-0"
	// Name of CSI driver container name
	driverContainerName := "mock"

	// Load logs of driver pod
	log, err := e2epod.GetPodLogs(ctx, m.clientSet, m.driverNamespace.Name, driverPodName, driverContainerName)
	if err != nil {
		return nil, fmt.Errorf("could not load CSI driver logs: %w", err)
	}

	logLines := strings.Split(log, "\n")
	var calls []MockCSICall
	for _, line := range logLines {
		index := strings.Index(line, grpcCallPrefix)
		if index == -1 {
			continue
		}
		line = line[index+len(grpcCallPrefix):]
		call := MockCSICall{
			json: string(line),
		}
		err := json.Unmarshal([]byte(line), &call)
		if err != nil {
			framework.Logf("Could not parse CSI driver log line %q: %s", line, err)
			continue
		}

		// Trim gRPC service name, i.e. "/csi.v1.Identity/Probe" -> "Probe"
		methodParts := strings.Split(call.Method, "/")
		call.Method = methodParts[len(methodParts)-1]

		calls = append(calls, call)
	}
	return calls, nil
}

// gce-pd
type gcePDCSIDriver struct {
	driverInfo storageframework.DriverInfo
}

var _ storageframework.TestDriver = &gcePDCSIDriver{}
var _ storageframework.DynamicPVTestDriver = &gcePDCSIDriver{}
var _ storageframework.SnapshottableTestDriver = &gcePDCSIDriver{}

// InitGcePDCSIDriver returns gcePDCSIDriver that implements TestDriver interface
func InitGcePDCSIDriver() storageframework.TestDriver {
	return &gcePDCSIDriver{
		driverInfo: storageframework.DriverInfo{
			Name:        GCEPDCSIDriverName,
			TestTags:    []interface{}{framework.WithSerial()},
			MaxFileSize: storageframework.FileSizeMedium,
			SupportedSizeRange: e2evolume.SizeRange{
				Min: "5Gi",
			},
			SupportedFsType: sets.NewString(
				"", // Default fsType
				"ext2",
				"ext3",
				"ext4",
				"xfs",
			),
			SupportedMountOption: sets.NewString("debug", "nouid32"),
			Capabilities: map[storageframework.Capability]bool{
				storageframework.CapPersistence: true,
				storageframework.CapBlock:       true,
				storageframework.CapFsGroup:     true,
				storageframework.CapExec:        true,
				storageframework.CapMultiPODs:   true,
				// GCE supports volume limits, but the test creates large
				// number of volumes and times out test suites.
				storageframework.CapVolumeLimits:                   false,
				storageframework.CapTopology:                       true,
				storageframework.CapControllerExpansion:            true,
				storageframework.CapOfflineExpansion:               true,
				storageframework.CapOnlineExpansion:                true,
				storageframework.CapNodeExpansion:                  true,
				storageframework.CapSnapshotDataSource:             true,
				storageframework.CapReadWriteOncePod:               true,
				storageframework.CapMultiplePVsSameID:              true,
				storageframework.CapFSResizeFromSourceNotSupported: true, //TODO: remove when CI tests use the fixed driver with: https://github.com/kubernetes-sigs/gcp-compute-persistent-disk-csi-driver/pull/972
			},
			RequiredAccessModes: []v1.PersistentVolumeAccessMode{v1.ReadWriteOnce},
			TopologyKeys:        []string{GCEPDCSIZoneTopologyKey},
			StressTestOptions: &storageframework.StressTestOptions{
				NumPods:     10,
				NumRestarts: 10,
			},
			VolumeSnapshotStressTestOptions: &storageframework.VolumeSnapshotStressTestOptions{
				// GCE only allows for one snapshot per volume to be created at a time,
				// which can cause test timeouts. We reduce the likelihood of test timeouts
				// by increasing the number of pods (and volumes) and reducing the number
				// of snapshots per volume.
				NumPods:      20,
				NumSnapshots: 2,
			},
		},
	}
}

func (g *gcePDCSIDriver) GetDriverInfo() *storageframework.DriverInfo {
	return &g.driverInfo
}

func (g *gcePDCSIDriver) SkipUnsupportedTest(pattern storageframework.TestPattern) {
	e2eskipper.SkipUnlessProviderIs("gce")
	if pattern.FsType == "xfs" {
		e2eskipper.SkipUnlessNodeOSDistroIs("ubuntu", "custom")
	}
	for _, tag := range pattern.TestTags {
		if framework.TagsEqual(tag, feature.Windows) {
			e2eskipper.Skipf("Skipping tests for windows since CSI does not support it yet")
		}
	}
}

func (g *gcePDCSIDriver) GetDynamicProvisionStorageClass(ctx context.Context, config *storageframework.PerTestConfig, fsType string) *storagev1.StorageClass {
	ns := config.Framework.Namespace.Name
	provisioner := g.driverInfo.Name

	parameters := map[string]string{"type": "pd-standard"}
	if fsType != "" {
		parameters["csi.storage.k8s.io/fstype"] = fsType
	}
	delayedBinding := storagev1.VolumeBindingWaitForFirstConsumer

	return storageframework.GetStorageClass(provisioner, parameters, &delayedBinding, ns)
}

func (g *gcePDCSIDriver) GetSnapshotClass(ctx context.Context, config *storageframework.PerTestConfig, parameters map[string]string) *unstructured.Unstructured {
	snapshotter := g.driverInfo.Name
	ns := config.Framework.Namespace.Name

	return utils.GenerateSnapshotClassSpec(snapshotter, parameters, ns)
}

func (g *gcePDCSIDriver) PrepareTest(ctx context.Context, f *framework.Framework) *storageframework.PerTestConfig {
	cfg := &storageframework.PerTestConfig{
		Driver:    g,
		Prefix:    "gcepd",
		Framework: f,
	}

	// Check if the cluster is already running gce-pd CSI Driver
	deploy, err := f.ClientSet.AppsV1().Deployments("gce-pd-csi-driver").Get(ctx, "csi-gce-pd-controller", metav1.GetOptions{})
	if err == nil && deploy != nil {
		framework.Logf("The csi gce-pd driver is already installed.")
		return cfg
	}
	ginkgo.By("deploying csi gce-pd driver")
	// Create secondary namespace which will be used for creating driver
	driverNamespace := utils.CreateDriverNamespace(ctx, f)
	driverns := driverNamespace.Name

	cancelLogging := utils.StartPodLogs(ctx, f, driverNamespace)
	// It would be safer to rename the gcePD driver, but that
	// hasn't been done before either and attempts to do so now led to
	// errors during driver registration, therefore it is disabled
	// by passing a nil function below.
	//
	// These are the options which would have to be used:
	// o := utils.PatchCSIOptions{
	// 	OldDriverName:            g.driverInfo.Name,
	// 	NewDriverName:            storageframework.GetUniqueDriverName(g),
	// 	DriverContainerName:      "gce-driver",
	// 	ProvisionerContainerName: "csi-external-provisioner",
	// }
	createGCESecrets(f.ClientSet, driverns)

	manifests := []string{
		"test/e2e/testing-manifests/storage-csi/external-attacher/rbac.yaml",
		"test/e2e/testing-manifests/storage-csi/external-provisioner/rbac.yaml",
		"test/e2e/testing-manifests/storage-csi/gce-pd/csi-controller-rbac.yaml",
		"test/e2e/testing-manifests/storage-csi/gce-pd/node_ds.yaml",
		"test/e2e/testing-manifests/storage-csi/gce-pd/controller_ss.yaml",
	}

	err = utils.CreateFromManifests(ctx, f, driverNamespace, nil, manifests...)
	if err != nil {
		framework.Failf("deploying csi gce-pd driver: %v", err)
	}

	if err = WaitForCSIDriverRegistrationOnAllNodes(ctx, GCEPDCSIDriverName, f.ClientSet); err != nil {
		framework.Failf("waiting for csi driver node registration on: %v", err)
	}

	cleanupFunc := generateDriverCleanupFunc(
		f,
		"gce-pd",
		driverns,
		cancelLogging)
	ginkgo.DeferCleanup(cleanupFunc)

	return &storageframework.PerTestConfig{
		Driver:          g,
		Prefix:          "gcepd",
		Framework:       f,
		DriverNamespace: driverNamespace,
	}
}

// WaitForCSIDriverRegistrationOnAllNodes waits for the CSINode object to be updated
// with the given driver on all schedulable nodes.
func WaitForCSIDriverRegistrationOnAllNodes(ctx context.Context, driverName string, cs clientset.Interface) error {
	nodes, err := e2enode.GetReadySchedulableNodes(ctx, cs)
	if err != nil {
		return err
	}
	for _, node := range nodes.Items {
		if err := WaitForCSIDriverRegistrationOnNode(ctx, node.Name, driverName, cs); err != nil {
			return err
		}
	}
	return nil
}

// WaitForCSIDriverRegistrationOnNode waits for the CSINode object generated by the node-registrar on a certain node
func WaitForCSIDriverRegistrationOnNode(ctx context.Context, nodeName string, driverName string, cs clientset.Interface) error {
	framework.Logf("waiting for CSIDriver %v to register on node %v", driverName, nodeName)

	// About 8.6 minutes timeout
	backoff := wait.Backoff{
		Duration: 2 * time.Second,
		Factor:   1.5,
		Steps:    12,
	}

	waitErr := wait.ExponentialBackoff(backoff, func() (bool, error) {
		csiNode, err := cs.StorageV1().CSINodes().Get(ctx, nodeName, metav1.GetOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return false, err
		}
		for _, driver := range csiNode.Spec.Drivers {
			if driver.Name == driverName {
				return true, nil
			}
		}
		return false, nil
	})
	if waitErr != nil {
		return fmt.Errorf("error waiting for CSI driver %s registration on node %s: %v", driverName, nodeName, waitErr)
	}
	return nil
}

func tryFunc(f func()) error {
	var err error
	if f == nil {
		return nil
	}
	defer func() {
		if recoverError := recover(); recoverError != nil {
			err = fmt.Errorf("%v", recoverError)
		}
	}()
	f()
	return err
}

func generateDriverCleanupFunc(
	f *framework.Framework,
	driverName, driverns string,
	cancelLogging func()) func(ctx context.Context) {

	// Cleanup CSI driver and namespaces. This function needs to be idempotent and can be
	// concurrently called from defer (or AfterEach) and AfterSuite action hooks.
	cleanupFunc := func(ctx context.Context) {
		ginkgo.By(fmt.Sprintf("uninstalling csi %s driver", driverName))
		_ = tryFunc(cancelLogging)

		ginkgo.By(fmt.Sprintf("deleting the driver namespace: %s", driverns))
		_ = tryFunc(func() { f.DeleteNamespace(ctx, driverns) })
	}

	return cleanupFunc
}
