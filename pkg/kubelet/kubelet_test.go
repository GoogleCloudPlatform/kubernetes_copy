/*
Copyright 2014 The Kubernetes Authors.

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

package kubelet

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	goruntime "runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	oteltrace "go.opentelemetry.io/otel/trace"
	noopoteltrace "go.opentelemetry.io/otel/trace/noop"

	cadvisorapi "github.com/google/cadvisor/info/v1"
	cadvisorapiv2 "github.com/google/cadvisor/info/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/client-go/testing"
	"k8s.io/mount-utils"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/flowcontrol"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	"k8s.io/component-base/metrics/testutil"
	internalapi "k8s.io/cri-api/pkg/apis"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
	remote "k8s.io/cri-client/pkg"
	fakeremote "k8s.io/cri-client/pkg/fake"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/features"
	kubeletconfiginternal "k8s.io/kubernetes/pkg/kubelet/apis/config"
	cadvisortest "k8s.io/kubernetes/pkg/kubelet/cadvisor/testing"
	"k8s.io/kubernetes/pkg/kubelet/clustertrustbundle"
	"k8s.io/kubernetes/pkg/kubelet/cm"
	"k8s.io/kubernetes/pkg/kubelet/cm/topologymanager"
	"k8s.io/kubernetes/pkg/kubelet/config"
	"k8s.io/kubernetes/pkg/kubelet/configmap"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
	containertest "k8s.io/kubernetes/pkg/kubelet/container/testing"
	"k8s.io/kubernetes/pkg/kubelet/eviction"
	"k8s.io/kubernetes/pkg/kubelet/images"
	"k8s.io/kubernetes/pkg/kubelet/kuberuntime"
	"k8s.io/kubernetes/pkg/kubelet/lifecycle"
	"k8s.io/kubernetes/pkg/kubelet/logs"
	"k8s.io/kubernetes/pkg/kubelet/metrics"
	"k8s.io/kubernetes/pkg/kubelet/network/dns"
	"k8s.io/kubernetes/pkg/kubelet/nodeshutdown"
	"k8s.io/kubernetes/pkg/kubelet/pleg"
	"k8s.io/kubernetes/pkg/kubelet/pluginmanager"
	kubepod "k8s.io/kubernetes/pkg/kubelet/pod"
	podtest "k8s.io/kubernetes/pkg/kubelet/pod/testing"
	proberesults "k8s.io/kubernetes/pkg/kubelet/prober/results"
	probetest "k8s.io/kubernetes/pkg/kubelet/prober/testing"
	"k8s.io/kubernetes/pkg/kubelet/secret"
	"k8s.io/kubernetes/pkg/kubelet/server"
	serverstats "k8s.io/kubernetes/pkg/kubelet/server/stats"
	"k8s.io/kubernetes/pkg/kubelet/stats"
	"k8s.io/kubernetes/pkg/kubelet/status"
	"k8s.io/kubernetes/pkg/kubelet/status/state"
	statustest "k8s.io/kubernetes/pkg/kubelet/status/testing"
	"k8s.io/kubernetes/pkg/kubelet/sysctl"
	"k8s.io/kubernetes/pkg/kubelet/token"
	kubetypes "k8s.io/kubernetes/pkg/kubelet/types"
	kubeletutil "k8s.io/kubernetes/pkg/kubelet/util"
	"k8s.io/kubernetes/pkg/kubelet/util/queue"
	kubeletvolume "k8s.io/kubernetes/pkg/kubelet/volumemanager"
	schedulerframework "k8s.io/kubernetes/pkg/scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/tainttoleration"
	"k8s.io/kubernetes/pkg/util/oom"
	"k8s.io/kubernetes/pkg/volume"
	_ "k8s.io/kubernetes/pkg/volume/hostpath"
	volumesecret "k8s.io/kubernetes/pkg/volume/secret"
	volumetest "k8s.io/kubernetes/pkg/volume/testing"
	"k8s.io/kubernetes/pkg/volume/util"
	"k8s.io/kubernetes/pkg/volume/util/hostutil"
	"k8s.io/kubernetes/pkg/volume/util/subpath"
	"k8s.io/kubernetes/test/utils/ktesting"
	"k8s.io/utils/clock"
	testingclock "k8s.io/utils/clock/testing"
	"k8s.io/utils/ptr"
)

func init() {
}

const (
	testKubeletHostname = "127.0.0.1"
	testKubeletHostIP   = "127.0.0.1"
	testKubeletHostIPv6 = "::1"

	// TODO(harry) any global place for these two?
	// Reasonable size range of all container images. 90%ile of images on dockerhub drops into this range.
	minImgSize int64 = 23 * 1024 * 1024
	maxImgSize int64 = 1000 * 1024 * 1024
)

// fakeImageGCManager is a fake image gc manager for testing. It will return image
// list from fake runtime directly instead of caching it.
type fakeImageGCManager struct {
	fakeImageService kubecontainer.ImageService
	images.ImageGCManager
}

func (f *fakeImageGCManager) GetImageList() ([]kubecontainer.Image, error) {
	return f.fakeImageService.ListImages(context.Background())
}

type TestKubelet struct {
	kubelet              *Kubelet
	fakeRuntime          *containertest.FakeRuntime
	fakeContainerManager *cm.FakeContainerManager
	fakeKubeClient       *fake.Clientset
	fakeMirrorClient     *podtest.FakeMirrorClient
	fakeClock            *testingclock.FakeClock
	mounter              mount.Interface
	volumePlugin         *volumetest.FakeVolumePlugin
}

func (tk *TestKubelet) Cleanup() {
	if tk.kubelet != nil {
		os.RemoveAll(tk.kubelet.rootDirectory)
		tk.kubelet = nil
	}
}

// newTestKubelet returns test kubelet with two images.
func newTestKubelet(t *testing.T, controllerAttachDetachEnabled bool) *TestKubelet {
	imageList := []kubecontainer.Image{
		{
			ID:       "abc",
			RepoTags: []string{"registry.k8s.io:v1", "registry.k8s.io:v2"},
			Size:     123,
		},
		{
			ID:       "efg",
			RepoTags: []string{"registry.k8s.io:v3", "registry.k8s.io:v4"},
			Size:     456,
		},
	}
	return newTestKubeletWithImageList(t, imageList, controllerAttachDetachEnabled, true /*initFakeVolumePlugin*/, true /*localStorageCapacityIsolation*/)
}

func newTestKubeletWithImageList(
	t *testing.T,
	imageList []kubecontainer.Image,
	controllerAttachDetachEnabled bool,
	initFakeVolumePlugin bool,
	localStorageCapacityIsolation bool,
) *TestKubelet {
	logger, _ := ktesting.NewTestContext(t)

	fakeRuntime := &containertest.FakeRuntime{
		ImageList: imageList,
		// Set ready conditions by default.
		RuntimeStatus: &kubecontainer.RuntimeStatus{
			Conditions: []kubecontainer.RuntimeCondition{
				{Type: "RuntimeReady", Status: true},
				{Type: "NetworkReady", Status: true},
			},
		},
		VersionInfo: "1.5.0",
		RuntimeType: "test",
		T:           t,
	}

	fakeRecorder := &record.FakeRecorder{}
	fakeKubeClient := &fake.Clientset{}
	kubelet := &Kubelet{}
	kubelet.recorder = fakeRecorder
	kubelet.kubeClient = fakeKubeClient
	kubelet.heartbeatClient = fakeKubeClient
	kubelet.os = &containertest.FakeOS{}
	kubelet.mounter = mount.NewFakeMounter(nil)
	kubelet.hostutil = hostutil.NewFakeHostUtil(nil)
	kubelet.subpather = &subpath.FakeSubpath{}

	kubelet.hostname = testKubeletHostname
	kubelet.nodeName = types.NodeName(testKubeletHostname)
	kubelet.runtimeState = newRuntimeState(maxWaitForContainerRuntime)
	kubelet.runtimeState.setNetworkState(nil)
	kubelet.rootDirectory = t.TempDir()
	kubelet.podLogsDirectory = t.TempDir()
	kubelet.sourcesReady = config.NewSourcesReady(func(_ sets.Set[string]) bool { return true })
	kubelet.serviceLister = testServiceLister{}
	kubelet.serviceHasSynced = func() bool { return true }
	kubelet.nodeHasSynced = func() bool { return true }
	kubelet.nodeLister = testNodeLister{
		nodes: []*v1.Node{
			{
				ObjectMeta: metav1.ObjectMeta{
					Name: string(kubelet.nodeName),
				},
				Status: v1.NodeStatus{
					Conditions: []v1.NodeCondition{
						{
							Type:    v1.NodeReady,
							Status:  v1.ConditionTrue,
							Reason:  "Ready",
							Message: "Node ready",
						},
					},
					Addresses: []v1.NodeAddress{
						{
							Type:    v1.NodeInternalIP,
							Address: testKubeletHostIP,
						},
						{
							Type:    v1.NodeInternalIP,
							Address: testKubeletHostIPv6,
						},
					},
					VolumesAttached: []v1.AttachedVolume{
						{
							Name:       "fake/fake-device",
							DevicePath: "fake/path",
						},
					},
				},
			},
		},
	}
	kubelet.recorder = fakeRecorder
	if err := kubelet.setupDataDirs(); err != nil {
		t.Fatalf("can't initialize kubelet data dirs: %v", err)
	}
	kubelet.daemonEndpoints = &v1.NodeDaemonEndpoints{}

	kubelet.cadvisor = &cadvisortest.Fake{}
	machineInfo, _ := kubelet.cadvisor.MachineInfo()
	kubelet.setCachedMachineInfo(machineInfo)
	kubelet.tracer = noopoteltrace.NewTracerProvider().Tracer("")

	fakeMirrorClient := podtest.NewFakeMirrorClient()
	secretManager := secret.NewSimpleSecretManager(kubelet.kubeClient)
	kubelet.secretManager = secretManager
	configMapManager := configmap.NewSimpleConfigMapManager(kubelet.kubeClient)
	kubelet.configMapManager = configMapManager
	kubelet.mirrorPodClient = fakeMirrorClient
	kubelet.podManager = kubepod.NewBasicPodManager()
	podStartupLatencyTracker := kubeletutil.NewPodStartupLatencyTracker()
	kubelet.statusManager = status.NewManager(fakeKubeClient, kubelet.podManager, &statustest.FakePodDeletionSafetyProvider{}, podStartupLatencyTracker, kubelet.getRootDir())
	kubelet.nodeStartupLatencyTracker = kubeletutil.NewNodeStartupLatencyTracker()

	kubelet.containerRuntime = fakeRuntime
	kubelet.runtimeCache = containertest.NewFakeRuntimeCache(kubelet.containerRuntime)
	kubelet.reasonCache = NewReasonCache()
	kubelet.podCache = containertest.NewFakeCache(kubelet.containerRuntime)
	kubelet.podWorkers = &fakePodWorkers{
		syncPodFn: kubelet.SyncPod,
		cache:     kubelet.podCache,
		t:         t,
	}

	kubelet.probeManager = probetest.FakeManager{}
	kubelet.livenessManager = proberesults.NewManager()
	kubelet.readinessManager = proberesults.NewManager()
	kubelet.startupManager = proberesults.NewManager()

	fakeContainerManager := cm.NewFakeContainerManager()
	kubelet.containerManager = fakeContainerManager
	fakeNodeRef := &v1.ObjectReference{
		Kind:      "Node",
		Name:      testKubeletHostname,
		UID:       types.UID(testKubeletHostname),
		Namespace: "",
	}

	volumeStatsAggPeriod := time.Second * 10
	kubelet.resourceAnalyzer = serverstats.NewResourceAnalyzer(kubelet, volumeStatsAggPeriod, kubelet.recorder)

	fakeHostStatsProvider := stats.NewFakeHostStatsProvider()

	kubelet.StatsProvider = stats.NewCadvisorStatsProvider(
		kubelet.cadvisor,
		kubelet.resourceAnalyzer,
		kubelet.podManager,
		kubelet.runtimeCache,
		fakeRuntime,
		kubelet.statusManager,
		fakeHostStatsProvider,
	)
	fakeImageGCPolicy := images.ImageGCPolicy{
		HighThresholdPercent: 90,
		LowThresholdPercent:  80,
	}
	imageGCManager, err := images.NewImageGCManager(fakeRuntime, kubelet.StatsProvider, fakeRecorder, fakeNodeRef, fakeImageGCPolicy, noopoteltrace.NewTracerProvider())
	assert.NoError(t, err)
	kubelet.imageManager = &fakeImageGCManager{
		fakeImageService: fakeRuntime,
		ImageGCManager:   imageGCManager,
	}
	kubelet.containerLogManager = logs.NewStubContainerLogManager()
	containerGCPolicy := kubecontainer.GCPolicy{
		MinAge:             time.Duration(0),
		MaxPerPodContainer: 1,
		MaxContainers:      -1,
	}
	containerGC, err := kubecontainer.NewContainerGC(fakeRuntime, containerGCPolicy, kubelet.sourcesReady)
	assert.NoError(t, err)
	kubelet.containerGC = containerGC

	fakeClock := testingclock.NewFakeClock(time.Now())
	kubelet.backOff = flowcontrol.NewBackOff(time.Second, time.Minute)
	kubelet.backOff.Clock = fakeClock
	kubelet.resyncInterval = 10 * time.Second
	kubelet.workQueue = queue.NewBasicWorkQueue(fakeClock)
	// Relist period does not affect the tests.
	kubelet.pleg = pleg.NewGenericPLEG(logger, fakeRuntime, make(chan *pleg.PodLifecycleEvent, 100), &pleg.RelistDuration{RelistPeriod: time.Hour, RelistThreshold: genericPlegRelistThreshold}, kubelet.podCache, clock.RealClock{})
	kubelet.clock = fakeClock

	nodeRef := &v1.ObjectReference{
		Kind:      "Node",
		Name:      string(kubelet.nodeName),
		UID:       types.UID(kubelet.nodeName),
		Namespace: "",
	}
	// setup eviction manager
	evictionManager, evictionAdmitHandler := eviction.NewManager(kubelet.resourceAnalyzer, eviction.Config{},
		killPodNow(kubelet.podWorkers, fakeRecorder), kubelet.imageManager, kubelet.containerGC, fakeRecorder, nodeRef, kubelet.clock, kubelet.supportLocalStorageCapacityIsolation())

	kubelet.evictionManager = evictionManager
	kubelet.admitHandlers.AddPodAdmitHandler(evictionAdmitHandler)

	// setup shutdown manager
	shutdownManager := nodeshutdown.NewManager(&nodeshutdown.Config{
		Logger:                          logger,
		ProbeManager:                    kubelet.probeManager,
		Recorder:                        fakeRecorder,
		NodeRef:                         nodeRef,
		GetPodsFunc:                     kubelet.podManager.GetPods,
		KillPodFunc:                     killPodNow(kubelet.podWorkers, fakeRecorder),
		SyncNodeStatusFunc:              func() {},
		ShutdownGracePeriodRequested:    0,
		ShutdownGracePeriodCriticalPods: 0,
	})
	kubelet.shutdownManager = shutdownManager
	kubelet.admitHandlers.AddPodAdmitHandler(shutdownManager)

	// Add this as cleanup predicate pod admitter
	kubelet.admitHandlers.AddPodAdmitHandler(lifecycle.NewPredicateAdmitHandler(kubelet.getNodeAnyWay, lifecycle.NewAdmissionFailureHandlerStub(), kubelet.containerManager.UpdatePluginResources))

	allPlugins := []volume.VolumePlugin{}
	plug := &volumetest.FakeVolumePlugin{PluginName: "fake", Host: nil}
	if initFakeVolumePlugin {
		allPlugins = append(allPlugins, plug)
	} else {
		allPlugins = append(allPlugins, volumesecret.ProbeVolumePlugins()...)
	}

	var prober volume.DynamicPluginProber // TODO (#51147) inject mock
	kubelet.volumePluginMgr, err =
		NewInitializedVolumePluginMgr(kubelet, kubelet.secretManager, kubelet.configMapManager, token.NewManager(kubelet.kubeClient), &clustertrustbundle.NoopManager{}, allPlugins, prober)
	require.NoError(t, err, "Failed to initialize VolumePluginMgr")

	kubelet.volumeManager = kubeletvolume.NewVolumeManager(
		controllerAttachDetachEnabled,
		kubelet.nodeName,
		kubelet.podManager,
		kubelet.podWorkers,
		fakeKubeClient,
		kubelet.volumePluginMgr,
		fakeRuntime,
		kubelet.mounter,
		kubelet.hostutil,
		kubelet.getPodsDir(),
		kubelet.recorder,
		volumetest.NewBlockVolumePathHandler())

	kubelet.pluginManager = pluginmanager.NewPluginManager(
		kubelet.getPluginsRegistrationDir(), /* sockDir */
		kubelet.recorder,
	)
	kubelet.setNodeStatusFuncs = kubelet.defaultNodeStatusFuncs()

	// enable active deadline handler
	activeDeadlineHandler, err := newActiveDeadlineHandler(kubelet.statusManager, kubelet.recorder, kubelet.clock)
	require.NoError(t, err, "Can't initialize active deadline handler")

	kubelet.AddPodSyncLoopHandler(activeDeadlineHandler)
	kubelet.AddPodSyncHandler(activeDeadlineHandler)
	kubelet.kubeletConfiguration.LocalStorageCapacityIsolation = localStorageCapacityIsolation
	return &TestKubelet{kubelet, fakeRuntime, fakeContainerManager, fakeKubeClient, fakeMirrorClient, fakeClock, nil, plug}
}

func newTestPods(count int) []*v1.Pod {
	pods := make([]*v1.Pod, count)
	for i := 0; i < count; i++ {
		pods[i] = &v1.Pod{
			Spec: v1.PodSpec{
				HostNetwork: true,
			},
			ObjectMeta: metav1.ObjectMeta{
				UID:  types.UID(strconv.Itoa(10000 + i)),
				Name: fmt.Sprintf("pod%d", i),
			},
		}
	}
	return pods
}

func TestSyncLoopAbort(t *testing.T) {
	ctx := context.Background()
	testKubelet := newTestKubelet(t, false /* controllerAttachDetachEnabled */)
	defer testKubelet.Cleanup()
	kubelet := testKubelet.kubelet
	kubelet.runtimeState.setRuntimeSync(time.Now())
	// The syncLoop waits on time.After(resyncInterval), set it really big so that we don't race for
	// the channel close
	kubelet.resyncInterval = time.Second * 30

	ch := make(chan kubetypes.PodUpdate)
	close(ch)

	// sanity check (also prevent this test from hanging in the next step)
	ok := kubelet.syncLoopIteration(ctx, ch, kubelet, make(chan time.Time), make(chan time.Time), make(chan *pleg.PodLifecycleEvent, 1))
	require.False(t, ok, "Expected syncLoopIteration to return !ok since update chan was closed")

	// this should terminate immediately; if it hangs then the syncLoopIteration isn't aborting properly
	kubelet.syncLoop(ctx, ch, kubelet)
}

func TestSyncPodsStartPod(t *testing.T) {
	testKubelet := newTestKubelet(t, false /* controllerAttachDetachEnabled */)
	defer testKubelet.Cleanup()
	kubelet := testKubelet.kubelet
	fakeRuntime := testKubelet.fakeRuntime
	pods := []*v1.Pod{
		podWithUIDNameNsSpec("12345678", "foo", "new", v1.PodSpec{
			Containers: []v1.Container{
				{Name: "bar"},
			},
		}),
	}
	kubelet.podManager.SetPods(pods)
	kubelet.HandlePodSyncs(pods)
	fakeRuntime.AssertStartedPods([]string{string(pods[0].UID)})
}

func TestHandlePodCleanupsPerQOS(t *testing.T) {
	ctx := context.Background()
	testKubelet := newTestKubelet(t, false /* controllerAttachDetachEnabled */)
	defer testKubelet.Cleanup()

	pod := &kubecontainer.Pod{
		ID:        "12345678",
		Name:      "foo",
		Namespace: "new",
		Containers: []*kubecontainer.Container{
			{Name: "bar"},
		},
	}

	fakeRuntime := testKubelet.fakeRuntime
	fakeContainerManager := testKubelet.fakeContainerManager
	fakeContainerManager.PodContainerManager.AddPodFromCgroups(pod) // add pod to mock cgroup
	fakeRuntime.PodList = []*containertest.FakePod{
		{Pod: pod},
	}
	kubelet := testKubelet.kubelet
	kubelet.cgroupsPerQOS = true // enable cgroupsPerQOS to turn on the cgroups cleanup

	// HandlePodCleanups gets called every 2 seconds within the Kubelet's
	// housekeeping routine. This test registers the pod, removes the unwanted pod, then calls into
	// HandlePodCleanups a few more times. We should only see one Destroy() event. podKiller runs
	// within a goroutine so a two second delay should be enough time to
	// mark the pod as killed (within this test case).

	kubelet.HandlePodCleanups(ctx)

	// assert that unwanted pods were killed
	if actual, expected := kubelet.podWorkers.(*fakePodWorkers).triggeredDeletion, []types.UID{"12345678"}; !reflect.DeepEqual(actual, expected) {
		t.Fatalf("expected %v to be deleted, got %v", expected, actual)
	}
	fakeRuntime.AssertKilledPods([]string(nil))

	// simulate Runtime.KillPod
	fakeRuntime.PodList = nil

	kubelet.HandlePodCleanups(ctx)
	kubelet.HandlePodCleanups(ctx)
	kubelet.HandlePodCleanups(ctx)

	destroyCount := 0
	err := wait.Poll(100*time.Millisecond, 10*time.Second, func() (bool, error) {
		fakeContainerManager.PodContainerManager.Lock()
		defer fakeContainerManager.PodContainerManager.Unlock()
		destroyCount = 0
		for _, functionName := range fakeContainerManager.PodContainerManager.CalledFunctions {
			if functionName == "Destroy" {
				destroyCount = destroyCount + 1
			}
		}
		return destroyCount >= 1, nil
	})

	assert.NoError(t, err, "wait should not return error")
	// housekeeping can get called multiple times. The cgroup Destroy() is
	// done within a goroutine and can get called multiple times, so the
	// Destroy() count in not deterministic on the actual number.
	// https://github.com/kubernetes/kubernetes/blob/29fdbb065b5e0d195299eb2d260b975cbc554673/pkg/kubelet/kubelet_pods.go#L2006
	assert.GreaterOrEqual(t, destroyCount, 1, "Expect 1 or more destroys")
}

func TestDispatchWorkOfCompletedPod(t *testing.T) {
	testKubelet := newTestKubelet(t, false /* controllerAttachDetachEnabled */)
	defer testKubelet.Cleanup()
	kubelet := testKubelet.kubelet
	var got bool
	kubelet.podWorkers = &fakePodWorkers{
		syncPodFn: func(ctx context.Context, updateType kubetypes.SyncPodType, pod, mirrorPod *v1.Pod, podStatus *kubecontainer.PodStatus) (bool, error) {
			got = true
			return false, nil
		},
		cache: kubelet.podCache,
		t:     t,
	}
	now := metav1.NewTime(time.Now())
	pods := []*v1.Pod{
		{
			ObjectMeta: metav1.ObjectMeta{
				UID:         "1",
				Name:        "completed-pod1",
				Namespace:   "ns",
				Annotations: make(map[string]string),
			},
			Status: v1.PodStatus{
				Phase: v1.PodFailed,
				ContainerStatuses: []v1.ContainerStatus{
					{
						State: v1.ContainerState{
							Terminated: &v1.ContainerStateTerminated{},
						},
					},
				},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				UID:         "2",
				Name:        "completed-pod2",
				Namespace:   "ns",
				Annotations: make(map[string]string),
			},
			Status: v1.PodStatus{
				Phase: v1.PodSucceeded,
				ContainerStatuses: []v1.ContainerStatus{
					{
						State: v1.ContainerState{
							Terminated: &v1.ContainerStateTerminated{},
						},
					},
				},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				UID:               "3",
				Name:              "completed-pod3",
				Namespace:         "ns",
				Annotations:       make(map[string]string),
				DeletionTimestamp: &now,
			},
			Status: v1.PodStatus{
				ContainerStatuses: []v1.ContainerStatus{
					{
						State: v1.ContainerState{
							Terminated: &v1.ContainerStateTerminated{},
						},
					},
				},
			},
		},
	}
	for _, pod := range pods {
		kubelet.podWorkers.UpdatePod(UpdatePodOptions{
			Pod:        pod,
			UpdateType: kubetypes.SyncPodSync,
			StartTime:  time.Now(),
		})
		if !got {
			t.Errorf("Should not skip completed pod %q", pod.Name)
		}
		got = false
	}
}

func TestDispatchWorkOfActivePod(t *testing.T) {
	testKubelet := newTestKubelet(t, false /* controllerAttachDetachEnabled */)
	defer testKubelet.Cleanup()
	kubelet := testKubelet.kubelet
	var got bool
	kubelet.podWorkers = &fakePodWorkers{
		syncPodFn: func(ctx context.Context, updateType kubetypes.SyncPodType, pod, mirrorPod *v1.Pod, podStatus *kubecontainer.PodStatus) (bool, error) {
			got = true
			return false, nil
		},
		cache: kubelet.podCache,
		t:     t,
	}
	pods := []*v1.Pod{
		{
			ObjectMeta: metav1.ObjectMeta{
				UID:         "1",
				Name:        "active-pod1",
				Namespace:   "ns",
				Annotations: make(map[string]string),
			},
			Status: v1.PodStatus{
				Phase: v1.PodRunning,
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				UID:         "2",
				Name:        "active-pod2",
				Namespace:   "ns",
				Annotations: make(map[string]string),
			},
			Status: v1.PodStatus{
				Phase: v1.PodFailed,
				ContainerStatuses: []v1.ContainerStatus{
					{
						State: v1.ContainerState{
							Running: &v1.ContainerStateRunning{},
						},
					},
				},
			},
		},
	}

	for _, pod := range pods {
		kubelet.podWorkers.UpdatePod(UpdatePodOptions{
			Pod:        pod,
			UpdateType: kubetypes.SyncPodSync,
			StartTime:  time.Now(),
		})
		if !got {
			t.Errorf("Should not skip active pod %q", pod.Name)
		}
		got = false
	}
}

func TestHandlePodCleanups(t *testing.T) {
	ctx := context.Background()
	testKubelet := newTestKubelet(t, false /* controllerAttachDetachEnabled */)
	defer testKubelet.Cleanup()

	pod := &kubecontainer.Pod{
		ID:        "12345678",
		Name:      "foo",
		Namespace: "new",
		Containers: []*kubecontainer.Container{
			{Name: "bar"},
		},
	}

	fakeRuntime := testKubelet.fakeRuntime
	fakeRuntime.PodList = []*containertest.FakePod{
		{Pod: pod},
	}
	kubelet := testKubelet.kubelet

	kubelet.HandlePodCleanups(ctx)

	// assert that unwanted pods were queued to kill
	if actual, expected := kubelet.podWorkers.(*fakePodWorkers).triggeredDeletion, []types.UID{"12345678"}; !reflect.DeepEqual(actual, expected) {
		t.Fatalf("expected %v to be deleted, got %v", expected, actual)
	}
	fakeRuntime.AssertKilledPods([]string(nil))
}

func TestHandlePodRemovesWhenSourcesAreReady(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode.")
	}

	ready := false

	testKubelet := newTestKubelet(t, false /* controllerAttachDetachEnabled */)
	defer testKubelet.Cleanup()

	fakePod := &kubecontainer.Pod{
		ID:        "1",
		Name:      "foo",
		Namespace: "new",
		Containers: []*kubecontainer.Container{
			{Name: "bar"},
		},
	}

	pods := []*v1.Pod{
		podWithUIDNameNs("1", "foo", "new"),
	}

	fakeRuntime := testKubelet.fakeRuntime
	fakeRuntime.PodList = []*containertest.FakePod{
		{Pod: fakePod},
	}
	kubelet := testKubelet.kubelet
	kubelet.sourcesReady = config.NewSourcesReady(func(_ sets.Set[string]) bool { return ready })

	kubelet.HandlePodRemoves(pods)
	time.Sleep(2 * time.Second)

	// Sources are not ready yet. Don't remove any pods.
	if expect, actual := []types.UID(nil), kubelet.podWorkers.(*fakePodWorkers).triggeredDeletion; !reflect.DeepEqual(expect, actual) {
		t.Fatalf("expected %v kills, got %v", expect, actual)
	}

	ready = true
	kubelet.HandlePodRemoves(pods)
	time.Sleep(2 * time.Second)

	// Sources are ready. Remove unwanted pods.
	if expect, actual := []types.UID{"1"}, kubelet.podWorkers.(*fakePodWorkers).triggeredDeletion; !reflect.DeepEqual(expect, actual) {
		t.Fatalf("expected %v kills, got %v", expect, actual)
	}
}

type testNodeLister struct {
	nodes []*v1.Node
}

func (nl testNodeLister) Get(name string) (*v1.Node, error) {
	for _, node := range nl.nodes {
		if node.Name == name {
			return node, nil
		}
	}
	return nil, fmt.Errorf("Node with name: %s does not exist", name)
}

func (nl testNodeLister) List(_ labels.Selector) (ret []*v1.Node, err error) {
	return nl.nodes, nil
}

func checkPodStatus(t *testing.T, kl *Kubelet, pod *v1.Pod, phase v1.PodPhase) {
	t.Helper()
	status, found := kl.statusManager.GetPodStatus(pod.UID)
	require.True(t, found, "Status of pod %q is not found in the status map", pod.UID)
	require.Equal(t, phase, status.Phase)
}

// Tests that we handle port conflicts correctly by setting the failed status in status map.
func TestHandlePortConflicts(t *testing.T) {
	testKubelet := newTestKubelet(t, false /* controllerAttachDetachEnabled */)
	defer testKubelet.Cleanup()
	kl := testKubelet.kubelet

	kl.nodeLister = testNodeLister{nodes: []*v1.Node{
		{
			ObjectMeta: metav1.ObjectMeta{Name: string(kl.nodeName)},
			Status: v1.NodeStatus{
				Allocatable: v1.ResourceList{
					v1.ResourcePods: *resource.NewQuantity(110, resource.DecimalSI),
				},
			},
		},
	}}

	recorder := record.NewFakeRecorder(20)
	nodeRef := &v1.ObjectReference{
		Kind:      "Node",
		Name:      "testNode",
		UID:       types.UID("testNode"),
		Namespace: "",
	}
	testClusterDNSDomain := "TEST"
	kl.dnsConfigurer = dns.NewConfigurer(recorder, nodeRef, nil, nil, testClusterDNSDomain, "")

	spec := v1.PodSpec{NodeName: string(kl.nodeName), Containers: []v1.Container{{Ports: []v1.ContainerPort{{HostPort: 80}}}}}
	pods := []*v1.Pod{
		podWithUIDNameNsSpec("123456789", "newpod", "foo", spec),
		podWithUIDNameNsSpec("987654321", "oldpod", "foo", spec),
	}
	// Make sure the Pods are in the reverse order of creation time.
	pods[1].CreationTimestamp = metav1.NewTime(time.Now())
	pods[0].CreationTimestamp = metav1.NewTime(time.Now().Add(1 * time.Second))
	// The newer pod should be rejected.
	notfittingPod := pods[0]
	fittingPod := pods[1]
	kl.podWorkers.(*fakePodWorkers).running = map[types.UID]bool{
		pods[0].UID: true,
		pods[1].UID: true,
	}

	kl.HandlePodAdditions(pods)

	// Check pod status stored in the status map.
	checkPodStatus(t, kl, notfittingPod, v1.PodFailed)
	checkPodStatus(t, kl, fittingPod, v1.PodPending)
}

// Tests that we handle host name conflicts correctly by setting the failed status in status map.
func TestHandleHostNameConflicts(t *testing.T) {
	testKubelet := newTestKubelet(t, false /* controllerAttachDetachEnabled */)
	defer testKubelet.Cleanup()
	kl := testKubelet.kubelet

	kl.nodeLister = testNodeLister{nodes: []*v1.Node{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "127.0.0.1"},
			Status: v1.NodeStatus{
				Allocatable: v1.ResourceList{
					v1.ResourcePods: *resource.NewQuantity(110, resource.DecimalSI),
				},
			},
		},
	}}

	recorder := record.NewFakeRecorder(20)
	nodeRef := &v1.ObjectReference{
		Kind:      "Node",
		Name:      "testNode",
		UID:       types.UID("testNode"),
		Namespace: "",
	}
	testClusterDNSDomain := "TEST"
	kl.dnsConfigurer = dns.NewConfigurer(recorder, nodeRef, nil, nil, testClusterDNSDomain, "")

	// default NodeName in test is 127.0.0.1
	pods := []*v1.Pod{
		podWithUIDNameNsSpec("123456789", "notfittingpod", "foo", v1.PodSpec{NodeName: "127.0.0.2"}),
		podWithUIDNameNsSpec("987654321", "fittingpod", "foo", v1.PodSpec{NodeName: "127.0.0.1"}),
	}

	notfittingPod := pods[0]
	fittingPod := pods[1]

	kl.HandlePodAdditions(pods)

	// Check pod status stored in the status map.
	checkPodStatus(t, kl, notfittingPod, v1.PodFailed)
	checkPodStatus(t, kl, fittingPod, v1.PodPending)
}

// Tests that we handle not matching labels selector correctly by setting the failed status in status map.
func TestHandleNodeSelector(t *testing.T) {
	testKubelet := newTestKubelet(t, false /* controllerAttachDetachEnabled */)
	defer testKubelet.Cleanup()
	kl := testKubelet.kubelet
	nodes := []*v1.Node{
		{
			ObjectMeta: metav1.ObjectMeta{Name: testKubeletHostname, Labels: map[string]string{"key": "B"}},
			Status: v1.NodeStatus{
				Allocatable: v1.ResourceList{
					v1.ResourcePods: *resource.NewQuantity(110, resource.DecimalSI),
				},
			},
		},
	}
	kl.nodeLister = testNodeLister{nodes: nodes}

	recorder := record.NewFakeRecorder(20)
	nodeRef := &v1.ObjectReference{
		Kind:      "Node",
		Name:      "testNode",
		UID:       types.UID("testNode"),
		Namespace: "",
	}
	testClusterDNSDomain := "TEST"
	kl.dnsConfigurer = dns.NewConfigurer(recorder, nodeRef, nil, nil, testClusterDNSDomain, "")

	pods := []*v1.Pod{
		podWithUIDNameNsSpec("123456789", "podA", "foo", v1.PodSpec{NodeSelector: map[string]string{"key": "A"}}),
		podWithUIDNameNsSpec("987654321", "podB", "foo", v1.PodSpec{NodeSelector: map[string]string{"key": "B"}}),
	}
	// The first pod should be rejected.
	notfittingPod := pods[0]
	fittingPod := pods[1]

	kl.HandlePodAdditions(pods)

	// Check pod status stored in the status map.
	checkPodStatus(t, kl, notfittingPod, v1.PodFailed)
	checkPodStatus(t, kl, fittingPod, v1.PodPending)
}

// Tests that we handle not matching labels selector correctly by setting the failed status in status map.
func TestHandleNodeSelectorBasedOnOS(t *testing.T) {
	tests := []struct {
		name        string
		nodeLabels  map[string]string
		podSelector map[string]string
		podStatus   v1.PodPhase
	}{
		{
			name:        "correct OS label, wrong pod selector, admission denied",
			nodeLabels:  map[string]string{v1.LabelOSStable: goruntime.GOOS, v1.LabelArchStable: goruntime.GOARCH},
			podSelector: map[string]string{v1.LabelOSStable: "dummyOS"},
			podStatus:   v1.PodFailed,
		},
		{
			name:        "correct OS label, correct pod selector, admission denied",
			nodeLabels:  map[string]string{v1.LabelOSStable: goruntime.GOOS, v1.LabelArchStable: goruntime.GOARCH},
			podSelector: map[string]string{v1.LabelOSStable: goruntime.GOOS},
			podStatus:   v1.PodPending,
		},
		{
			// Expect no patching to happen, label B should be preserved and can be used for nodeAffinity.
			name:        "new node label, correct pod selector, admitted",
			nodeLabels:  map[string]string{v1.LabelOSStable: goruntime.GOOS, v1.LabelArchStable: goruntime.GOARCH, "key": "B"},
			podSelector: map[string]string{"key": "B"},
			podStatus:   v1.PodPending,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			testKubelet := newTestKubelet(t, false /* controllerAttachDetachEnabled */)
			defer testKubelet.Cleanup()
			kl := testKubelet.kubelet
			nodes := []*v1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{Name: testKubeletHostname, Labels: test.nodeLabels},
					Status: v1.NodeStatus{
						Allocatable: v1.ResourceList{
							v1.ResourcePods: *resource.NewQuantity(110, resource.DecimalSI),
						},
					},
				},
			}
			kl.nodeLister = testNodeLister{nodes: nodes}

			recorder := record.NewFakeRecorder(20)
			nodeRef := &v1.ObjectReference{
				Kind:      "Node",
				Name:      "testNode",
				UID:       types.UID("testNode"),
				Namespace: "",
			}
			testClusterDNSDomain := "TEST"
			kl.dnsConfigurer = dns.NewConfigurer(recorder, nodeRef, nil, nil, testClusterDNSDomain, "")

			pod := podWithUIDNameNsSpec("123456789", "podA", "foo", v1.PodSpec{NodeSelector: test.podSelector})

			kl.HandlePodAdditions([]*v1.Pod{pod})

			// Check pod status stored in the status map.
			checkPodStatus(t, kl, pod, test.podStatus)
		})
	}
}

// Tests that we handle exceeded resources correctly by setting the failed status in status map.
func TestHandleMemExceeded(t *testing.T) {
	testKubelet := newTestKubelet(t, false /* controllerAttachDetachEnabled */)
	defer testKubelet.Cleanup()
	kl := testKubelet.kubelet
	nodes := []*v1.Node{
		{ObjectMeta: metav1.ObjectMeta{Name: testKubeletHostname},
			Status: v1.NodeStatus{Capacity: v1.ResourceList{}, Allocatable: v1.ResourceList{
				v1.ResourceCPU:    *resource.NewMilliQuantity(10, resource.DecimalSI),
				v1.ResourceMemory: *resource.NewQuantity(100, resource.BinarySI),
				v1.ResourcePods:   *resource.NewQuantity(40, resource.DecimalSI),
			}}},
	}
	kl.nodeLister = testNodeLister{nodes: nodes}

	recorder := record.NewFakeRecorder(20)
	nodeRef := &v1.ObjectReference{
		Kind:      "Node",
		Name:      "testNode",
		UID:       types.UID("testNode"),
		Namespace: "",
	}
	testClusterDNSDomain := "TEST"
	kl.dnsConfigurer = dns.NewConfigurer(recorder, nodeRef, nil, nil, testClusterDNSDomain, "")

	spec := v1.PodSpec{NodeName: string(kl.nodeName),
		Containers: []v1.Container{{Resources: v1.ResourceRequirements{
			Requests: v1.ResourceList{
				v1.ResourceMemory: resource.MustParse("90"),
			},
		}}},
	}
	pods := []*v1.Pod{
		podWithUIDNameNsSpec("123456789", "newpod", "foo", spec),
		podWithUIDNameNsSpec("987654321", "oldpod", "foo", spec),
	}
	// Make sure the Pods are in the reverse order of creation time.
	pods[1].CreationTimestamp = metav1.NewTime(time.Now())
	pods[0].CreationTimestamp = metav1.NewTime(time.Now().Add(1 * time.Second))
	// The newer pod should be rejected.
	notfittingPod := pods[0]
	fittingPod := pods[1]
	kl.podWorkers.(*fakePodWorkers).running = map[types.UID]bool{
		pods[0].UID: true,
		pods[1].UID: true,
	}

	kl.HandlePodAdditions(pods)

	// Check pod status stored in the status map.
	checkPodStatus(t, kl, notfittingPod, v1.PodFailed)
	checkPodStatus(t, kl, fittingPod, v1.PodPending)
}

// Tests that we handle result of interface UpdatePluginResources correctly
// by setting corresponding status in status map.
func TestHandlePluginResources(t *testing.T) {
	testKubelet := newTestKubelet(t, false /* controllerAttachDetachEnabled */)
	defer testKubelet.Cleanup()
	kl := testKubelet.kubelet

	adjustedResource := v1.ResourceName("domain1.com/adjustedResource")
	emptyResource := v1.ResourceName("domain2.com/emptyResource")
	missingResource := v1.ResourceName("domain2.com/missingResource")
	failedResource := v1.ResourceName("domain2.com/failedResource")
	resourceQuantity0 := *resource.NewQuantity(int64(0), resource.DecimalSI)
	resourceQuantity1 := *resource.NewQuantity(int64(1), resource.DecimalSI)
	resourceQuantity2 := *resource.NewQuantity(int64(2), resource.DecimalSI)
	resourceQuantityInvalid := *resource.NewQuantity(int64(-1), resource.DecimalSI)
	allowedPodQuantity := *resource.NewQuantity(int64(10), resource.DecimalSI)
	nodes := []*v1.Node{
		{ObjectMeta: metav1.ObjectMeta{Name: testKubeletHostname},
			Status: v1.NodeStatus{Capacity: v1.ResourceList{}, Allocatable: v1.ResourceList{
				adjustedResource: resourceQuantity1,
				emptyResource:    resourceQuantity0,
				v1.ResourcePods:  allowedPodQuantity,
			}}},
	}
	kl.nodeLister = testNodeLister{nodes: nodes}

	updatePluginResourcesFunc := func(node *schedulerframework.NodeInfo, attrs *lifecycle.PodAdmitAttributes) error {
		// Maps from resourceName to the value we use to set node.allocatableResource[resourceName].
		// A resource with invalid value (< 0) causes the function to return an error
		// to emulate resource Allocation failure.
		// Resources not contained in this map will have their node.allocatableResource
		// quantity unchanged.
		updateResourceMap := map[v1.ResourceName]resource.Quantity{
			adjustedResource: resourceQuantity2,
			emptyResource:    resourceQuantity0,
			failedResource:   resourceQuantityInvalid,
		}
		pod := attrs.Pod
		newAllocatableResource := node.Allocatable.Clone()
		for _, container := range pod.Spec.Containers {
			for resource := range container.Resources.Requests {
				newQuantity, exist := updateResourceMap[resource]
				if !exist {
					continue
				}
				if newQuantity.Value() < 0 {
					return fmt.Errorf("Allocation failed")
				}
				newAllocatableResource.ScalarResources[resource] = newQuantity.Value()
			}
		}
		node.Allocatable = newAllocatableResource
		return nil
	}

	// add updatePluginResourcesFunc to admission handler, to test it's behavior.
	kl.admitHandlers = lifecycle.PodAdmitHandlers{}
	kl.admitHandlers.AddPodAdmitHandler(lifecycle.NewPredicateAdmitHandler(kl.getNodeAnyWay, lifecycle.NewAdmissionFailureHandlerStub(), updatePluginResourcesFunc))

	recorder := record.NewFakeRecorder(20)
	nodeRef := &v1.ObjectReference{
		Kind:      "Node",
		Name:      "testNode",
		UID:       types.UID("testNode"),
		Namespace: "",
	}
	testClusterDNSDomain := "TEST"
	kl.dnsConfigurer = dns.NewConfigurer(recorder, nodeRef, nil, nil, testClusterDNSDomain, "")

	// pod requiring adjustedResource can be successfully allocated because updatePluginResourcesFunc
	// adjusts node.allocatableResource for this resource to a sufficient value.
	fittingPodSpec := v1.PodSpec{NodeName: string(kl.nodeName),
		Containers: []v1.Container{{Resources: v1.ResourceRequirements{
			Limits: v1.ResourceList{
				adjustedResource: resourceQuantity2,
			},
			Requests: v1.ResourceList{
				adjustedResource: resourceQuantity2,
			},
		}}},
	}
	// pod requiring emptyResource (extended resources with 0 allocatable) will
	// not pass PredicateAdmit.
	emptyPodSpec := v1.PodSpec{NodeName: string(kl.nodeName),
		Containers: []v1.Container{{Resources: v1.ResourceRequirements{
			Limits: v1.ResourceList{
				emptyResource: resourceQuantity2,
			},
			Requests: v1.ResourceList{
				emptyResource: resourceQuantity2,
			},
		}}},
	}
	// pod requiring missingResource will pass PredicateAdmit.
	//
	// Extended resources missing in node status are ignored in PredicateAdmit.
	// This is required to support extended resources that are not managed by
	// device plugin, such as cluster-level resources.
	missingPodSpec := v1.PodSpec{NodeName: string(kl.nodeName),
		Containers: []v1.Container{{Resources: v1.ResourceRequirements{
			Limits: v1.ResourceList{
				missingResource: resourceQuantity2,
			},
			Requests: v1.ResourceList{
				missingResource: resourceQuantity2,
			},
		}}},
	}
	// pod requiring failedResource will fail with the resource failed to be allocated.
	failedPodSpec := v1.PodSpec{NodeName: string(kl.nodeName),
		Containers: []v1.Container{{Resources: v1.ResourceRequirements{
			Limits: v1.ResourceList{
				failedResource: resourceQuantity1,
			},
			Requests: v1.ResourceList{
				failedResource: resourceQuantity1,
			},
		}}},
	}

	fittingPod := podWithUIDNameNsSpec("1", "fittingpod", "foo", fittingPodSpec)
	emptyPod := podWithUIDNameNsSpec("2", "emptypod", "foo", emptyPodSpec)
	missingPod := podWithUIDNameNsSpec("3", "missingpod", "foo", missingPodSpec)
	failedPod := podWithUIDNameNsSpec("4", "failedpod", "foo", failedPodSpec)

	kl.HandlePodAdditions([]*v1.Pod{fittingPod, emptyPod, missingPod, failedPod})

	// Check pod status stored in the status map.
	checkPodStatus(t, kl, fittingPod, v1.PodPending)
	checkPodStatus(t, kl, emptyPod, v1.PodFailed)
	checkPodStatus(t, kl, missingPod, v1.PodPending)
	checkPodStatus(t, kl, failedPod, v1.PodFailed)
}

// TODO(filipg): This test should be removed once StatusSyncer can do garbage collection without external signal.
func TestPurgingObsoleteStatusMapEntries(t *testing.T) {
	ctx := context.Background()
	testKubelet := newTestKubelet(t, false /* controllerAttachDetachEnabled */)
	defer testKubelet.Cleanup()

	kl := testKubelet.kubelet
	pods := []*v1.Pod{
		{ObjectMeta: metav1.ObjectMeta{Name: "pod1", UID: "1234"}, Spec: v1.PodSpec{Containers: []v1.Container{{Ports: []v1.ContainerPort{{HostPort: 80}}}}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "pod2", UID: "4567"}, Spec: v1.PodSpec{Containers: []v1.Container{{Ports: []v1.ContainerPort{{HostPort: 80}}}}}},
	}
	podToTest := pods[1]
	// Run once to populate the status map.
	kl.HandlePodAdditions(pods)
	if _, found := kl.statusManager.GetPodStatus(podToTest.UID); !found {
		t.Fatalf("expected to have status cached for pod2")
	}
	// Sync with empty pods so that the entry in status map will be removed.
	kl.podManager.SetPods([]*v1.Pod{})
	kl.HandlePodCleanups(ctx)
	if _, found := kl.statusManager.GetPodStatus(podToTest.UID); found {
		t.Fatalf("expected to not have status cached for pod2")
	}
}

func TestValidateContainerLogStatus(t *testing.T) {
	testKubelet := newTestKubelet(t, false /* controllerAttachDetachEnabled */)
	defer testKubelet.Cleanup()
	kubelet := testKubelet.kubelet
	containerName := "x"
	testCases := []struct {
		statuses []v1.ContainerStatus
		success  bool // whether getting logs for the container should succeed.
		pSuccess bool // whether getting logs for the previous container should succeed.
	}{
		{
			statuses: []v1.ContainerStatus{
				{
					Name: containerName,
					State: v1.ContainerState{
						Running: &v1.ContainerStateRunning{},
					},
					LastTerminationState: v1.ContainerState{
						Terminated: &v1.ContainerStateTerminated{ContainerID: "docker://fakeid"},
					},
				},
			},
			success:  true,
			pSuccess: true,
		},
		{
			statuses: []v1.ContainerStatus{
				{
					Name: containerName,
					State: v1.ContainerState{
						Running: &v1.ContainerStateRunning{},
					},
				},
			},
			success:  true,
			pSuccess: false,
		},
		{
			statuses: []v1.ContainerStatus{
				{
					Name: containerName,
					State: v1.ContainerState{
						Terminated: &v1.ContainerStateTerminated{},
					},
				},
			},
			success:  false,
			pSuccess: false,
		},
		{
			statuses: []v1.ContainerStatus{
				{
					Name: containerName,
					State: v1.ContainerState{
						Terminated: &v1.ContainerStateTerminated{ContainerID: "docker://fakeid"},
					},
				},
			},
			success:  true,
			pSuccess: false,
		},
		{
			statuses: []v1.ContainerStatus{
				{
					Name: containerName,
					State: v1.ContainerState{
						Terminated: &v1.ContainerStateTerminated{},
					},
					LastTerminationState: v1.ContainerState{
						Terminated: &v1.ContainerStateTerminated{},
					},
				},
			},
			success:  false,
			pSuccess: false,
		},
		{
			statuses: []v1.ContainerStatus{
				{
					Name: containerName,
					State: v1.ContainerState{
						Terminated: &v1.ContainerStateTerminated{},
					},
					LastTerminationState: v1.ContainerState{
						Terminated: &v1.ContainerStateTerminated{ContainerID: "docker://fakeid"},
					},
				},
			},
			success:  true,
			pSuccess: true,
		},
		{
			statuses: []v1.ContainerStatus{
				{
					Name: containerName,
					State: v1.ContainerState{
						Waiting: &v1.ContainerStateWaiting{},
					},
				},
			},
			success:  false,
			pSuccess: false,
		},
		{
			statuses: []v1.ContainerStatus{
				{
					Name:  containerName,
					State: v1.ContainerState{Waiting: &v1.ContainerStateWaiting{Reason: "ErrImagePull"}},
				},
			},
			success:  false,
			pSuccess: false,
		},
		{
			statuses: []v1.ContainerStatus{
				{
					Name:  containerName,
					State: v1.ContainerState{Waiting: &v1.ContainerStateWaiting{Reason: "ErrImagePullBackOff"}},
				},
			},
			success:  false,
			pSuccess: false,
		},
	}

	for i, tc := range testCases {
		// Access the log of the most recent container
		previous := false
		podStatus := &v1.PodStatus{ContainerStatuses: tc.statuses}
		_, err := kubelet.validateContainerLogStatus("podName", podStatus, containerName, previous)
		if !tc.success {
			assert.Errorf(t, err, "[case %d] error", i)
		} else {
			assert.NoErrorf(t, err, "[case %d] error", i)
		}
		// Access the log of the previous, terminated container
		previous = true
		_, err = kubelet.validateContainerLogStatus("podName", podStatus, containerName, previous)
		if !tc.pSuccess {
			assert.Errorf(t, err, "[case %d] error", i)
		} else {
			assert.NoErrorf(t, err, "[case %d] error", i)
		}
		// Access the log of a container that's not in the pod
		_, err = kubelet.validateContainerLogStatus("podName", podStatus, "blah", false)
		assert.Errorf(t, err, "[case %d] invalid container name should cause an error", i)
	}
}

func TestCreateMirrorPod(t *testing.T) {
	tests := []struct {
		name       string
		updateType kubetypes.SyncPodType
	}{
		{
			name:       "SyncPodCreate",
			updateType: kubetypes.SyncPodCreate,
		},
		{
			name:       "SyncPodUpdate",
			updateType: kubetypes.SyncPodUpdate,
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			testKubelet := newTestKubelet(t, false /* controllerAttachDetachEnabled */)
			defer testKubelet.Cleanup()

			kl := testKubelet.kubelet
			manager := testKubelet.fakeMirrorClient
			pod := podWithUIDNameNs("12345678", "bar", "foo")
			pod.Annotations[kubetypes.ConfigSourceAnnotationKey] = "file"
			pods := []*v1.Pod{pod}
			kl.podManager.SetPods(pods)
			isTerminal, err := kl.SyncPod(context.Background(), tt.updateType, pod, nil, &kubecontainer.PodStatus{})
			assert.NoError(t, err)
			if isTerminal {
				t.Fatalf("pod should not be terminal: %#v", pod)
			}
			podFullName := kubecontainer.GetPodFullName(pod)
			assert.True(t, manager.HasPod(podFullName), "Expected mirror pod %q to be created", podFullName)
			assert.Equal(t, 1, manager.NumOfPods(), "Expected only 1 mirror pod %q, got %+v", podFullName, manager.GetPods())
		})
	}
}

func TestDeleteOutdatedMirrorPod(t *testing.T) {
	testKubelet := newTestKubelet(t, false /* controllerAttachDetachEnabled */)
	defer testKubelet.Cleanup()

	kl := testKubelet.kubelet
	manager := testKubelet.fakeMirrorClient
	pod := podWithUIDNameNsSpec("12345678", "foo", "ns", v1.PodSpec{
		Containers: []v1.Container{
			{Name: "1234", Image: "foo"},
		},
	})
	pod.Annotations[kubetypes.ConfigSourceAnnotationKey] = "file"

	// Mirror pod has an outdated spec.
	mirrorPod := podWithUIDNameNsSpec("11111111", "foo", "ns", v1.PodSpec{
		Containers: []v1.Container{
			{Name: "1234", Image: "bar"},
		},
	})
	mirrorPod.Annotations[kubetypes.ConfigSourceAnnotationKey] = "api"
	mirrorPod.Annotations[kubetypes.ConfigMirrorAnnotationKey] = "mirror"

	pods := []*v1.Pod{pod, mirrorPod}
	kl.podManager.SetPods(pods)
	isTerminal, err := kl.SyncPod(context.Background(), kubetypes.SyncPodUpdate, pod, mirrorPod, &kubecontainer.PodStatus{})
	assert.NoError(t, err)
	if isTerminal {
		t.Fatalf("pod should not be terminal: %#v", pod)
	}
	name := kubecontainer.GetPodFullName(pod)
	creates, deletes := manager.GetCounts(name)
	if creates != 1 || deletes != 1 {
		t.Errorf("expected 1 creation and 1 deletion of %q, got %d, %d", name, creates, deletes)
	}
}

func TestDeleteOrphanedMirrorPods(t *testing.T) {
	ctx := context.Background()
	testKubelet := newTestKubelet(t, false /* controllerAttachDetachEnabled */)
	defer testKubelet.Cleanup()

	kl := testKubelet.kubelet
	manager := testKubelet.fakeMirrorClient
	orphanPods := []*v1.Pod{
		{
			ObjectMeta: metav1.ObjectMeta{
				UID:       "12345678",
				Name:      "pod1",
				Namespace: "ns",
				Annotations: map[string]string{
					kubetypes.ConfigSourceAnnotationKey: "api",
					kubetypes.ConfigMirrorAnnotationKey: "mirror",
				},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				UID:       "12345679",
				Name:      "pod2",
				Namespace: "ns",
				Annotations: map[string]string{
					kubetypes.ConfigSourceAnnotationKey: "api",
					kubetypes.ConfigMirrorAnnotationKey: "mirror",
				},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				UID:       "12345670",
				Name:      "pod3",
				Namespace: "ns",
				Annotations: map[string]string{
					kubetypes.ConfigSourceAnnotationKey: "api",
					kubetypes.ConfigMirrorAnnotationKey: "mirror",
				},
			},
		},
	}

	kl.podManager.SetPods(orphanPods)

	// a static pod that is terminating will not be deleted
	kl.podWorkers.(*fakePodWorkers).terminatingStaticPods = map[string]bool{
		kubecontainer.GetPodFullName(orphanPods[2]): true,
	}

	// Sync with an empty pod list to delete all mirror pods.
	kl.HandlePodCleanups(ctx)
	assert.Empty(t, manager.GetPods(), "Expected no mirror pods")
	for i, pod := range orphanPods {
		name := kubecontainer.GetPodFullName(pod)
		creates, deletes := manager.GetCounts(name)
		switch i {
		case 2:
			if creates != 0 || deletes != 0 {
				t.Errorf("expected 0 creation and 0 deletion of %q, got %d, %d", name, creates, deletes)
			}
		default:
			if creates != 0 || deletes != 1 {
				t.Errorf("expected 0 creation and one deletion of %q, got %d, %d", name, creates, deletes)
			}
		}
	}
}

func TestNetworkErrorsWithoutHostNetwork(t *testing.T) {
	testKubelet := newTestKubelet(t, false /* controllerAttachDetachEnabled */)
	defer testKubelet.Cleanup()
	kubelet := testKubelet.kubelet

	kubelet.runtimeState.setNetworkState(fmt.Errorf("simulated network error"))

	pod := podWithUIDNameNsSpec("12345678", "hostnetwork", "new", v1.PodSpec{
		HostNetwork: false,

		Containers: []v1.Container{
			{Name: "foo"},
		},
	})

	kubelet.podManager.SetPods([]*v1.Pod{pod})
	isTerminal, err := kubelet.SyncPod(context.Background(), kubetypes.SyncPodUpdate, pod, nil, &kubecontainer.PodStatus{})
	assert.Error(t, err, "expected pod with hostNetwork=false to fail when network in error")
	if isTerminal {
		t.Fatalf("pod should not be terminal: %#v", pod)
	}

	pod.Annotations[kubetypes.ConfigSourceAnnotationKey] = kubetypes.FileSource
	pod.Spec.HostNetwork = true
	isTerminal, err = kubelet.SyncPod(context.Background(), kubetypes.SyncPodUpdate, pod, nil, &kubecontainer.PodStatus{})
	assert.NoError(t, err, "expected pod with hostNetwork=true to succeed when network in error")
	if isTerminal {
		t.Fatalf("pod should not be terminal: %#v", pod)
	}
}

func TestFilterOutInactivePods(t *testing.T) {
	testKubelet := newTestKubelet(t, false /* controllerAttachDetachEnabled */)
	defer testKubelet.Cleanup()
	kubelet := testKubelet.kubelet
	pods := newTestPods(8)
	now := metav1.NewTime(time.Now())

	// terminal pods are excluded
	pods[0].Status.Phase = v1.PodFailed
	pods[1].Status.Phase = v1.PodSucceeded

	// deleted pod is included unless it's known to be terminated
	pods[2].Status.Phase = v1.PodRunning
	pods[2].DeletionTimestamp = &now
	pods[2].Status.ContainerStatuses = []v1.ContainerStatus{
		{State: v1.ContainerState{
			Running: &v1.ContainerStateRunning{
				StartedAt: now,
			},
		}},
	}

	// pending and running pods are included
	pods[3].Status.Phase = v1.PodPending
	pods[4].Status.Phase = v1.PodRunning

	// pod that is running but has been rejected by admission is excluded
	pods[5].Status.Phase = v1.PodRunning
	kubelet.statusManager.SetPodStatus(pods[5], v1.PodStatus{Phase: v1.PodFailed})

	// pod that is running according to the api but is known terminated is excluded
	pods[6].Status.Phase = v1.PodRunning
	kubelet.podWorkers.(*fakePodWorkers).terminated = map[types.UID]bool{
		pods[6].UID: true,
	}

	// pod that is failed but still terminating is included (it may still be consuming
	// resources)
	pods[7].Status.Phase = v1.PodFailed
	kubelet.podWorkers.(*fakePodWorkers).terminationRequested = map[types.UID]bool{
		pods[7].UID: true,
	}

	expected := []*v1.Pod{pods[2], pods[3], pods[4], pods[7]}
	kubelet.podManager.SetPods(pods)
	actual := kubelet.filterOutInactivePods(pods)
	assert.Equal(t, expected, actual)
}

func TestCheckpointContainer(t *testing.T) {
	testKubelet := newTestKubelet(t, false /* controllerAttachDetachEnabled */)
	defer testKubelet.Cleanup()
	kubelet := testKubelet.kubelet

	fakeRuntime := testKubelet.fakeRuntime
	containerID := kubecontainer.ContainerID{
		Type: "test",
		ID:   "abc1234",
	}

	fakePod := &containertest.FakePod{
		Pod: &kubecontainer.Pod{
			ID:        "12345678",
			Name:      "podFoo",
			Namespace: "nsFoo",
			Containers: []*kubecontainer.Container{
				{
					Name: "containerFoo",
					ID:   containerID,
				},
			},
		},
	}

	fakeRuntime.PodList = []*containertest.FakePod{fakePod}
	wrongContainerName := "wrongContainerName"

	tests := []struct {
		name               string
		containerName      string
		checkpointLocation string
		expectedStatus     error
		expectedLocation   string
	}{
		{
			name:               "Checkpoint with wrong container name",
			containerName:      wrongContainerName,
			checkpointLocation: "",
			expectedStatus:     fmt.Errorf("container %s not found", wrongContainerName),
			expectedLocation:   "",
		},
		{
			name:               "Checkpoint with default checkpoint location",
			containerName:      fakePod.Pod.Containers[0].Name,
			checkpointLocation: "",
			expectedStatus:     nil,
			expectedLocation: filepath.Join(
				kubelet.getCheckpointsDir(),
				fmt.Sprintf(
					"checkpoint-%s_%s-%s",
					fakePod.Pod.Name,
					fakePod.Pod.Namespace,
					fakePod.Pod.Containers[0].Name,
				),
			),
		},
		{
			name:               "Checkpoint with ignored location",
			containerName:      fakePod.Pod.Containers[0].Name,
			checkpointLocation: "somethingThatWillBeIgnored",
			expectedStatus:     nil,
			expectedLocation: filepath.Join(
				kubelet.getCheckpointsDir(),
				fmt.Sprintf(
					"checkpoint-%s_%s-%s",
					fakePod.Pod.Name,
					fakePod.Pod.Namespace,
					fakePod.Pod.Containers[0].Name,
				),
			),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			options := &runtimeapi.CheckpointContainerRequest{}
			if test.checkpointLocation != "" {
				options.Location = test.checkpointLocation
			}
			status := kubelet.CheckpointContainer(
				ctx,
				fakePod.Pod.ID,
				fmt.Sprintf(
					"%s_%s",
					fakePod.Pod.Name,
					fakePod.Pod.Namespace,
				),
				test.containerName,
				options,
			)
			require.Equal(t, test.expectedStatus, status)

			if status != nil {
				return
			}

			require.True(
				t,
				strings.HasPrefix(
					options.Location,
					test.expectedLocation,
				),
			)
			require.Equal(
				t,
				options.ContainerId,
				containerID.ID,
			)

		})
	}
}

func TestSyncPodsSetStatusToFailedForPodsThatRunTooLong(t *testing.T) {
	testKubelet := newTestKubelet(t, false /* controllerAttachDetachEnabled */)
	defer testKubelet.Cleanup()
	fakeRuntime := testKubelet.fakeRuntime
	kubelet := testKubelet.kubelet

	now := metav1.Now()
	startTime := metav1.NewTime(now.Time.Add(-1 * time.Minute))
	exceededActiveDeadlineSeconds := int64(30)

	pods := []*v1.Pod{
		{
			ObjectMeta: metav1.ObjectMeta{
				UID:       "12345678",
				Name:      "bar",
				Namespace: "new",
			},
			Spec: v1.PodSpec{
				Containers: []v1.Container{
					{Name: "foo"},
				},
				ActiveDeadlineSeconds: &exceededActiveDeadlineSeconds,
			},
			Status: v1.PodStatus{
				StartTime: &startTime,
			},
		},
	}

	fakeRuntime.PodList = []*containertest.FakePod{
		{Pod: &kubecontainer.Pod{
			ID:        "12345678",
			Name:      "bar",
			Namespace: "new",
			Containers: []*kubecontainer.Container{
				{Name: "foo"},
			},
		}},
	}

	// Let the pod worker sets the status to fail after this sync.
	kubelet.HandlePodUpdates(pods)
	status, found := kubelet.statusManager.GetPodStatus(pods[0].UID)
	assert.True(t, found, "expected to found status for pod %q", pods[0].UID)
	assert.Equal(t, v1.PodFailed, status.Phase)
	// check pod status contains ContainerStatuses, etc.
	assert.NotNil(t, status.ContainerStatuses)
}

func TestSyncPodsDoesNotSetPodsThatDidNotRunTooLongToFailed(t *testing.T) {
	testKubelet := newTestKubelet(t, false /* controllerAttachDetachEnabled */)
	defer testKubelet.Cleanup()
	fakeRuntime := testKubelet.fakeRuntime

	kubelet := testKubelet.kubelet

	now := metav1.Now()
	startTime := metav1.NewTime(now.Time.Add(-1 * time.Minute))
	exceededActiveDeadlineSeconds := int64(300)

	pods := []*v1.Pod{
		{
			ObjectMeta: metav1.ObjectMeta{
				UID:       "12345678",
				Name:      "bar",
				Namespace: "new",
			},
			Spec: v1.PodSpec{
				Containers: []v1.Container{
					{Name: "foo"},
				},
				ActiveDeadlineSeconds: &exceededActiveDeadlineSeconds,
			},
			Status: v1.PodStatus{
				StartTime: &startTime,
			},
		},
	}

	fakeRuntime.PodList = []*containertest.FakePod{
		{Pod: &kubecontainer.Pod{
			ID:        "12345678",
			Name:      "bar",
			Namespace: "new",
			Containers: []*kubecontainer.Container{
				{Name: "foo"},
			},
		}},
	}

	kubelet.podManager.SetPods(pods)
	kubelet.HandlePodUpdates(pods)
	status, found := kubelet.statusManager.GetPodStatus(pods[0].UID)
	assert.True(t, found, "expected to found status for pod %q", pods[0].UID)
	assert.NotEqual(t, v1.PodFailed, status.Phase)
}

func podWithUIDNameNs(uid types.UID, name, namespace string) *v1.Pod {
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			UID:         uid,
			Name:        name,
			Namespace:   namespace,
			Annotations: map[string]string{},
		},
	}
}

func podWithUIDNameNsSpec(uid types.UID, name, namespace string, spec v1.PodSpec) *v1.Pod {
	pod := podWithUIDNameNs(uid, name, namespace)
	pod.Spec = spec
	return pod
}

func TestDeletePodDirsForDeletedPods(t *testing.T) {
	ctx := context.Background()
	testKubelet := newTestKubelet(t, false /* controllerAttachDetachEnabled */)
	defer testKubelet.Cleanup()
	kl := testKubelet.kubelet
	pods := []*v1.Pod{
		podWithUIDNameNs("12345678", "pod1", "ns"),
		podWithUIDNameNs("12345679", "pod2", "ns"),
	}

	kl.podManager.SetPods(pods)
	// Sync to create pod directories.
	kl.HandlePodSyncs(kl.podManager.GetPods())
	for i := range pods {
		assert.True(t, dirExists(kl.getPodDir(pods[i].UID)), "Expected directory to exist for pod %d", i)
	}

	// Pod 1 has been deleted and no longer exists.
	kl.podManager.SetPods([]*v1.Pod{pods[0]})
	kl.HandlePodCleanups(ctx)
	assert.True(t, dirExists(kl.getPodDir(pods[0].UID)), "Expected directory to exist for pod 0")
	assert.False(t, dirExists(kl.getPodDir(pods[1].UID)), "Expected directory to be deleted for pod 1")
}

func syncAndVerifyPodDir(t *testing.T, testKubelet *TestKubelet, pods []*v1.Pod, podsToCheck []*v1.Pod, shouldExist bool) {
	ctx := context.Background()
	t.Helper()
	kl := testKubelet.kubelet

	kl.podManager.SetPods(pods)
	kl.HandlePodSyncs(pods)
	kl.HandlePodCleanups(ctx)
	for i, pod := range podsToCheck {
		exist := dirExists(kl.getPodDir(pod.UID))
		assert.Equal(t, shouldExist, exist, "directory of pod %d", i)
	}
}

func TestDoesNotDeletePodDirsForTerminatedPods(t *testing.T) {
	testKubelet := newTestKubelet(t, false /* controllerAttachDetachEnabled */)
	defer testKubelet.Cleanup()
	kl := testKubelet.kubelet
	pods := []*v1.Pod{
		podWithUIDNameNs("12345678", "pod1", "ns"),
		podWithUIDNameNs("12345679", "pod2", "ns"),
		podWithUIDNameNs("12345680", "pod3", "ns"),
	}
	syncAndVerifyPodDir(t, testKubelet, pods, pods, true)
	// Pod 1 failed, and pod 2 succeeded. None of the pod directories should be
	// deleted.
	kl.statusManager.SetPodStatus(pods[1], v1.PodStatus{Phase: v1.PodFailed})
	kl.statusManager.SetPodStatus(pods[2], v1.PodStatus{Phase: v1.PodSucceeded})
	syncAndVerifyPodDir(t, testKubelet, pods, pods, true)
}

func TestDoesNotDeletePodDirsIfContainerIsRunning(t *testing.T) {
	testKubelet := newTestKubelet(t, false /* controllerAttachDetachEnabled */)
	defer testKubelet.Cleanup()
	runningPod := &kubecontainer.Pod{
		ID:        "12345678",
		Name:      "pod1",
		Namespace: "ns",
	}
	apiPod := podWithUIDNameNs(runningPod.ID, runningPod.Name, runningPod.Namespace)

	// Sync once to create pod directory; confirm that the pod directory has
	// already been created.
	pods := []*v1.Pod{apiPod}
	testKubelet.kubelet.podWorkers.(*fakePodWorkers).running = map[types.UID]bool{apiPod.UID: true}
	syncAndVerifyPodDir(t, testKubelet, pods, []*v1.Pod{apiPod}, true)

	// Pretend the pod is deleted from apiserver, but is still active on the node.
	// The pod directory should not be removed.
	pods = []*v1.Pod{}
	testKubelet.fakeRuntime.PodList = []*containertest.FakePod{{Pod: runningPod, NetnsPath: ""}}
	syncAndVerifyPodDir(t, testKubelet, pods, []*v1.Pod{apiPod}, true)

	// The pod is deleted and also not active on the node. The pod directory
	// should be removed.
	pods = []*v1.Pod{}
	testKubelet.fakeRuntime.PodList = []*containertest.FakePod{}
	testKubelet.kubelet.podWorkers.(*fakePodWorkers).running = nil
	syncAndVerifyPodDir(t, testKubelet, pods, []*v1.Pod{apiPod}, false)
}

func TestGetPodsToSync(t *testing.T) {
	testKubelet := newTestKubelet(t, false /* controllerAttachDetachEnabled */)
	defer testKubelet.Cleanup()
	kubelet := testKubelet.kubelet
	clock := testKubelet.fakeClock
	pods := newTestPods(5)

	exceededActiveDeadlineSeconds := int64(30)
	notYetActiveDeadlineSeconds := int64(120)
	startTime := metav1.NewTime(clock.Now())
	pods[0].Status.StartTime = &startTime
	pods[0].Spec.ActiveDeadlineSeconds = &exceededActiveDeadlineSeconds
	pods[1].Status.StartTime = &startTime
	pods[1].Spec.ActiveDeadlineSeconds = &notYetActiveDeadlineSeconds
	pods[2].Status.StartTime = &startTime
	pods[2].Spec.ActiveDeadlineSeconds = &exceededActiveDeadlineSeconds

	kubelet.podManager.SetPods(pods)
	kubelet.workQueue.Enqueue(pods[2].UID, 0)
	kubelet.workQueue.Enqueue(pods[3].UID, 30*time.Second)
	kubelet.workQueue.Enqueue(pods[4].UID, 2*time.Minute)

	clock.Step(1 * time.Minute)

	expected := []*v1.Pod{pods[2], pods[3], pods[0]}
	podsToSync := kubelet.getPodsToSync()
	sort.Sort(podsByUID(expected))
	sort.Sort(podsByUID(podsToSync))
	assert.Equal(t, expected, podsToSync)
}

func TestGenerateAPIPodStatusWithSortedContainers(t *testing.T) {
	testKubelet := newTestKubelet(t, false /* controllerAttachDetachEnabled */)
	defer testKubelet.Cleanup()
	kubelet := testKubelet.kubelet
	numContainers := 10
	expectedOrder := []string{}
	cStatuses := []*kubecontainer.Status{}
	specContainerList := []v1.Container{}
	for i := 0; i < numContainers; i++ {
		id := fmt.Sprintf("%v", i)
		containerName := fmt.Sprintf("%vcontainer", id)
		expectedOrder = append(expectedOrder, containerName)
		cStatus := &kubecontainer.Status{
			ID:   kubecontainer.BuildContainerID("test", id),
			Name: containerName,
		}
		// Rearrange container statuses
		if i%2 == 0 {
			cStatuses = append(cStatuses, cStatus)
		} else {
			cStatuses = append([]*kubecontainer.Status{cStatus}, cStatuses...)
		}
		specContainerList = append(specContainerList, v1.Container{Name: containerName})
	}
	pod := podWithUIDNameNs("uid1", "foo", "test")
	pod.Spec = v1.PodSpec{
		Containers: specContainerList,
	}

	status := &kubecontainer.PodStatus{
		ID:                pod.UID,
		Name:              pod.Name,
		Namespace:         pod.Namespace,
		ContainerStatuses: cStatuses,
	}
	for i := 0; i < 5; i++ {
		apiStatus := kubelet.generateAPIPodStatus(pod, status, false)
		for i, c := range apiStatus.ContainerStatuses {
			if expectedOrder[i] != c.Name {
				t.Fatalf("Container status not sorted, expected %v at index %d, but found %v", expectedOrder[i], i, c.Name)
			}
		}
	}
}

func verifyContainerStatuses(t *testing.T, statuses []v1.ContainerStatus, expectedState, expectedLastTerminationState map[string]v1.ContainerState, message string) {
	for _, s := range statuses {
		assert.Equal(t, expectedState[s.Name], s.State, "%s: state", message)
		assert.Equal(t, expectedLastTerminationState[s.Name], s.LastTerminationState, "%s: last terminated state", message)
	}
}

// Test generateAPIPodStatus with different reason cache and old api pod status.
func TestGenerateAPIPodStatusWithReasonCache(t *testing.T) {
	// The following waiting reason and message  are generated in convertStatusToAPIStatus()
	testTimestamp := time.Unix(123456789, 987654321)
	testErrorReason := fmt.Errorf("test-error")
	emptyContainerID := (&kubecontainer.ContainerID{}).String()
	testKubelet := newTestKubelet(t, false /* controllerAttachDetachEnabled */)
	defer testKubelet.Cleanup()
	kubelet := testKubelet.kubelet
	pod := podWithUIDNameNs("12345678", "foo", "new")
	pod.Spec = v1.PodSpec{RestartPolicy: v1.RestartPolicyOnFailure}

	podStatus := &kubecontainer.PodStatus{
		ID:        pod.UID,
		Name:      pod.Name,
		Namespace: pod.Namespace,
	}
	tests := []struct {
		containers    []v1.Container
		statuses      []*kubecontainer.Status
		reasons       map[string]error
		oldStatuses   []v1.ContainerStatus
		expectedState map[string]v1.ContainerState
		// Only set expectedInitState when it is different from expectedState
		expectedInitState            map[string]v1.ContainerState
		expectedLastTerminationState map[string]v1.ContainerState
	}{
		// For container with no historical record, State should be Waiting, LastTerminationState should be retrieved from
		// old status from apiserver.
		{
			containers: []v1.Container{{Name: "without-old-record"}, {Name: "with-old-record"}},
			statuses:   []*kubecontainer.Status{},
			reasons:    map[string]error{},
			oldStatuses: []v1.ContainerStatus{{
				Name:                 "with-old-record",
				LastTerminationState: v1.ContainerState{Terminated: &v1.ContainerStateTerminated{}},
			}},
			expectedState: map[string]v1.ContainerState{
				"without-old-record": {Waiting: &v1.ContainerStateWaiting{
					Reason: ContainerCreating,
				}},
				"with-old-record": {Waiting: &v1.ContainerStateWaiting{
					Reason: ContainerCreating,
				}},
			},
			expectedInitState: map[string]v1.ContainerState{
				"without-old-record": {Waiting: &v1.ContainerStateWaiting{
					Reason: PodInitializing,
				}},
				"with-old-record": {Waiting: &v1.ContainerStateWaiting{
					Reason: PodInitializing,
				}},
			},
			expectedLastTerminationState: map[string]v1.ContainerState{
				"with-old-record": {Terminated: &v1.ContainerStateTerminated{}},
			},
		},
		// For running container, State should be Running, LastTerminationState should be retrieved from latest terminated status.
		{
			containers: []v1.Container{{Name: "running"}},
			statuses: []*kubecontainer.Status{
				{
					Name:      "running",
					State:     kubecontainer.ContainerStateRunning,
					StartedAt: testTimestamp,
				},
				{
					Name:     "running",
					State:    kubecontainer.ContainerStateExited,
					ExitCode: 1,
				},
			},
			reasons:     map[string]error{},
			oldStatuses: []v1.ContainerStatus{},
			expectedState: map[string]v1.ContainerState{
				"running": {Running: &v1.ContainerStateRunning{
					StartedAt: metav1.NewTime(testTimestamp),
				}},
			},
			expectedLastTerminationState: map[string]v1.ContainerState{
				"running": {Terminated: &v1.ContainerStateTerminated{
					ExitCode:    1,
					ContainerID: emptyContainerID,
				}},
			},
		},
		// For terminated container:
		// * If there is no recent start error record, State should be Terminated, LastTerminationState should be retrieved from
		// second latest terminated status;
		// * If there is recent start error record, State should be Waiting, LastTerminationState should be retrieved from latest
		// terminated status;
		// * If ExitCode = 0, restart policy is RestartPolicyOnFailure, the container shouldn't be restarted. No matter there is
		// recent start error or not, State should be Terminated, LastTerminationState should be retrieved from second latest
		// terminated status.
		{
			containers: []v1.Container{{Name: "without-reason"}, {Name: "with-reason"}},
			statuses: []*kubecontainer.Status{
				{
					Name:     "without-reason",
					State:    kubecontainer.ContainerStateExited,
					ExitCode: 1,
				},
				{
					Name:     "with-reason",
					State:    kubecontainer.ContainerStateExited,
					ExitCode: 2,
				},
				{
					Name:     "without-reason",
					State:    kubecontainer.ContainerStateExited,
					ExitCode: 3,
				},
				{
					Name:     "with-reason",
					State:    kubecontainer.ContainerStateExited,
					ExitCode: 4,
				},
				{
					Name:     "succeed",
					State:    kubecontainer.ContainerStateExited,
					ExitCode: 0,
				},
				{
					Name:     "succeed",
					State:    kubecontainer.ContainerStateExited,
					ExitCode: 5,
				},
			},
			reasons:     map[string]error{"with-reason": testErrorReason, "succeed": testErrorReason},
			oldStatuses: []v1.ContainerStatus{},
			expectedState: map[string]v1.ContainerState{
				"without-reason": {Terminated: &v1.ContainerStateTerminated{
					ExitCode:    1,
					ContainerID: emptyContainerID,
				}},
				"with-reason": {Waiting: &v1.ContainerStateWaiting{Reason: testErrorReason.Error()}},
				"succeed": {Terminated: &v1.ContainerStateTerminated{
					ExitCode:    0,
					ContainerID: emptyContainerID,
				}},
			},
			expectedLastTerminationState: map[string]v1.ContainerState{
				"without-reason": {Terminated: &v1.ContainerStateTerminated{
					ExitCode:    3,
					ContainerID: emptyContainerID,
				}},
				"with-reason": {Terminated: &v1.ContainerStateTerminated{
					ExitCode:    2,
					ContainerID: emptyContainerID,
				}},
				"succeed": {Terminated: &v1.ContainerStateTerminated{
					ExitCode:    5,
					ContainerID: emptyContainerID,
				}},
			},
		},
		// For Unknown Container Status:
		// * In certain situations a container can be running and fail to retrieve the status which results in
		// * a transition to the Unknown state. Prior to this fix, a container would make an invalid transition
		// * from Running->Waiting. This test validates the correct behavior of transitioning from Running->Terminated.
		{
			containers: []v1.Container{{Name: "unknown"}},
			statuses: []*kubecontainer.Status{
				{
					Name:  "unknown",
					State: kubecontainer.ContainerStateUnknown,
				},
				{
					Name:  "unknown",
					State: kubecontainer.ContainerStateRunning,
				},
			},
			reasons: map[string]error{},
			oldStatuses: []v1.ContainerStatus{{
				Name:  "unknown",
				State: v1.ContainerState{Running: &v1.ContainerStateRunning{}},
			}},
			expectedState: map[string]v1.ContainerState{
				"unknown": {Terminated: &v1.ContainerStateTerminated{
					ExitCode: 137,
					Message:  "The container could not be located when the pod was terminated",
					Reason:   kubecontainer.ContainerReasonStatusUnknown,
				}},
			},
			expectedLastTerminationState: map[string]v1.ContainerState{
				"unknown": {Running: &v1.ContainerStateRunning{}},
			},
		},
	}

	for i, test := range tests {
		kubelet.reasonCache = NewReasonCache()
		for n, e := range test.reasons {
			kubelet.reasonCache.add(pod.UID, n, e, "")
		}
		pod.Spec.Containers = test.containers
		pod.Status.ContainerStatuses = test.oldStatuses
		podStatus.ContainerStatuses = test.statuses
		apiStatus := kubelet.generateAPIPodStatus(pod, podStatus, false)
		verifyContainerStatuses(t, apiStatus.ContainerStatuses, test.expectedState, test.expectedLastTerminationState, fmt.Sprintf("case %d", i))
	}

	// Everything should be the same for init containers
	for i, test := range tests {
		kubelet.reasonCache = NewReasonCache()
		for n, e := range test.reasons {
			kubelet.reasonCache.add(pod.UID, n, e, "")
		}
		pod.Spec.InitContainers = test.containers
		pod.Status.InitContainerStatuses = test.oldStatuses
		podStatus.ContainerStatuses = test.statuses
		apiStatus := kubelet.generateAPIPodStatus(pod, podStatus, false)
		expectedState := test.expectedState
		if test.expectedInitState != nil {
			expectedState = test.expectedInitState
		}
		verifyContainerStatuses(t, apiStatus.InitContainerStatuses, expectedState, test.expectedLastTerminationState, fmt.Sprintf("case %d", i))
	}
}

// Test generateAPIPodStatus with different restart policies.
func TestGenerateAPIPodStatusWithDifferentRestartPolicies(t *testing.T) {
	testErrorReason := fmt.Errorf("test-error")
	emptyContainerID := (&kubecontainer.ContainerID{}).String()
	testKubelet := newTestKubelet(t, false /* controllerAttachDetachEnabled */)
	defer testKubelet.Cleanup()
	kubelet := testKubelet.kubelet
	pod := podWithUIDNameNs("12345678", "foo", "new")
	containers := []v1.Container{{Name: "succeed"}, {Name: "failed"}}
	podStatus := &kubecontainer.PodStatus{
		ID:        pod.UID,
		Name:      pod.Name,
		Namespace: pod.Namespace,
		ContainerStatuses: []*kubecontainer.Status{
			{
				Name:     "succeed",
				State:    kubecontainer.ContainerStateExited,
				ExitCode: 0,
			},
			{
				Name:     "failed",
				State:    kubecontainer.ContainerStateExited,
				ExitCode: 1,
			},
			{
				Name:     "succeed",
				State:    kubecontainer.ContainerStateExited,
				ExitCode: 2,
			},
			{
				Name:     "failed",
				State:    kubecontainer.ContainerStateExited,
				ExitCode: 3,
			},
		},
	}
	kubelet.reasonCache.add(pod.UID, "succeed", testErrorReason, "")
	kubelet.reasonCache.add(pod.UID, "failed", testErrorReason, "")
	for c, test := range []struct {
		restartPolicy                v1.RestartPolicy
		expectedState                map[string]v1.ContainerState
		expectedLastTerminationState map[string]v1.ContainerState
		// Only set expectedInitState when it is different from expectedState
		expectedInitState map[string]v1.ContainerState
		// Only set expectedInitLastTerminationState when it is different from expectedLastTerminationState
		expectedInitLastTerminationState map[string]v1.ContainerState
	}{
		{
			restartPolicy: v1.RestartPolicyNever,
			expectedState: map[string]v1.ContainerState{
				"succeed": {Terminated: &v1.ContainerStateTerminated{
					ExitCode:    0,
					ContainerID: emptyContainerID,
				}},
				"failed": {Terminated: &v1.ContainerStateTerminated{
					ExitCode:    1,
					ContainerID: emptyContainerID,
				}},
			},
			expectedLastTerminationState: map[string]v1.ContainerState{
				"succeed": {Terminated: &v1.ContainerStateTerminated{
					ExitCode:    2,
					ContainerID: emptyContainerID,
				}},
				"failed": {Terminated: &v1.ContainerStateTerminated{
					ExitCode:    3,
					ContainerID: emptyContainerID,
				}},
			},
		},
		{
			restartPolicy: v1.RestartPolicyOnFailure,
			expectedState: map[string]v1.ContainerState{
				"succeed": {Terminated: &v1.ContainerStateTerminated{
					ExitCode:    0,
					ContainerID: emptyContainerID,
				}},
				"failed": {Waiting: &v1.ContainerStateWaiting{Reason: testErrorReason.Error()}},
			},
			expectedLastTerminationState: map[string]v1.ContainerState{
				"succeed": {Terminated: &v1.ContainerStateTerminated{
					ExitCode:    2,
					ContainerID: emptyContainerID,
				}},
				"failed": {Terminated: &v1.ContainerStateTerminated{
					ExitCode:    1,
					ContainerID: emptyContainerID,
				}},
			},
		},
		{
			restartPolicy: v1.RestartPolicyAlways,
			expectedState: map[string]v1.ContainerState{
				"succeed": {Waiting: &v1.ContainerStateWaiting{Reason: testErrorReason.Error()}},
				"failed":  {Waiting: &v1.ContainerStateWaiting{Reason: testErrorReason.Error()}},
			},
			expectedLastTerminationState: map[string]v1.ContainerState{
				"succeed": {Terminated: &v1.ContainerStateTerminated{
					ExitCode:    0,
					ContainerID: emptyContainerID,
				}},
				"failed": {Terminated: &v1.ContainerStateTerminated{
					ExitCode:    1,
					ContainerID: emptyContainerID,
				}},
			},
			// If the init container is terminated with exit code 0, it won't be restarted even when the
			// restart policy is RestartAlways.
			expectedInitState: map[string]v1.ContainerState{
				"succeed": {Terminated: &v1.ContainerStateTerminated{
					ExitCode:    0,
					ContainerID: emptyContainerID,
				}},
				"failed": {Waiting: &v1.ContainerStateWaiting{Reason: testErrorReason.Error()}},
			},
			expectedInitLastTerminationState: map[string]v1.ContainerState{
				"succeed": {Terminated: &v1.ContainerStateTerminated{
					ExitCode:    2,
					ContainerID: emptyContainerID,
				}},
				"failed": {Terminated: &v1.ContainerStateTerminated{
					ExitCode:    1,
					ContainerID: emptyContainerID,
				}},
			},
		},
	} {
		pod.Spec.RestartPolicy = test.restartPolicy
		// Test normal containers
		pod.Spec.Containers = containers
		apiStatus := kubelet.generateAPIPodStatus(pod, podStatus, false)
		expectedState, expectedLastTerminationState := test.expectedState, test.expectedLastTerminationState
		verifyContainerStatuses(t, apiStatus.ContainerStatuses, expectedState, expectedLastTerminationState, fmt.Sprintf("case %d", c))
		pod.Spec.Containers = nil

		// Test init containers
		pod.Spec.InitContainers = containers
		apiStatus = kubelet.generateAPIPodStatus(pod, podStatus, false)
		if test.expectedInitState != nil {
			expectedState = test.expectedInitState
		}
		if test.expectedInitLastTerminationState != nil {
			expectedLastTerminationState = test.expectedInitLastTerminationState
		}
		verifyContainerStatuses(t, apiStatus.InitContainerStatuses, expectedState, expectedLastTerminationState, fmt.Sprintf("case %d", c))
		pod.Spec.InitContainers = nil
	}
}

// testPodAdmitHandler is a lifecycle.PodAdmitHandler for testing.
type testPodAdmitHandler struct {
	// list of pods to reject.
	podsToReject []*v1.Pod
}

// Admit rejects all pods in the podsToReject list with a matching UID.
func (a *testPodAdmitHandler) Admit(attrs *lifecycle.PodAdmitAttributes) lifecycle.PodAdmitResult {
	for _, podToReject := range a.podsToReject {
		if podToReject.UID == attrs.Pod.UID {
			return lifecycle.PodAdmitResult{Admit: false, Reason: "Rejected", Message: "Pod is rejected"}
		}
	}
	return lifecycle.PodAdmitResult{Admit: true}
}

// Test verifies that the kubelet invokes an admission handler during HandlePodAdditions.
func TestHandlePodAdditionsInvokesPodAdmitHandlers(t *testing.T) {
	testKubelet := newTestKubelet(t, false /* controllerAttachDetachEnabled */)
	defer testKubelet.Cleanup()
	kl := testKubelet.kubelet
	kl.nodeLister = testNodeLister{nodes: []*v1.Node{
		{
			ObjectMeta: metav1.ObjectMeta{Name: string(kl.nodeName)},
			Status: v1.NodeStatus{
				Allocatable: v1.ResourceList{
					v1.ResourcePods: *resource.NewQuantity(110, resource.DecimalSI),
				},
			},
		},
	}}

	pods := []*v1.Pod{
		{
			ObjectMeta: metav1.ObjectMeta{
				UID:       "123456789",
				Name:      "podA",
				Namespace: "foo",
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				UID:       "987654321",
				Name:      "podB",
				Namespace: "foo",
			},
		},
	}
	podToReject := pods[0]
	podToAdmit := pods[1]
	podsToReject := []*v1.Pod{podToReject}

	kl.admitHandlers.AddPodAdmitHandler(&testPodAdmitHandler{podsToReject: podsToReject})

	kl.HandlePodAdditions(pods)

	// Check pod status stored in the status map.
	checkPodStatus(t, kl, podToReject, v1.PodFailed)
	checkPodStatus(t, kl, podToAdmit, v1.PodPending)
}

func TestPodResourceAllocationReset(t *testing.T) {
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.InPlacePodVerticalScaling, true)
	testKubelet := newTestKubelet(t, false)
	defer testKubelet.Cleanup()
	kubelet := testKubelet.kubelet
	kubelet.statusManager = status.NewFakeManager()

	// fakePodWorkers trigger syncPodFn synchronously on update, but entering
	// kubelet.SyncPod while holding the podResizeMutex can lead to deadlock.
	kubelet.podWorkers.(*fakePodWorkers).syncPodFn =
		func(_ context.Context, _ kubetypes.SyncPodType, _, _ *v1.Pod, _ *kubecontainer.PodStatus) (bool, error) {
			return false, nil
		}

	nodes := []*v1.Node{
		{
			ObjectMeta: metav1.ObjectMeta{Name: testKubeletHostname},
			Status: v1.NodeStatus{
				Capacity: v1.ResourceList{
					v1.ResourceCPU:    resource.MustParse("8"),
					v1.ResourceMemory: resource.MustParse("8Gi"),
				},
				Allocatable: v1.ResourceList{
					v1.ResourceCPU:    resource.MustParse("4"),
					v1.ResourceMemory: resource.MustParse("4Gi"),
					v1.ResourcePods:   *resource.NewQuantity(40, resource.DecimalSI),
				},
			},
		},
	}
	kubelet.nodeLister = testNodeLister{nodes: nodes}

	cpu500m := resource.MustParse("500m")
	cpu800m := resource.MustParse("800m")
	mem500M := resource.MustParse("500Mi")
	mem800M := resource.MustParse("800Mi")
	cpu500mMem500MPodSpec := &v1.PodSpec{
		Containers: []v1.Container{
			{
				Name: "c1",
				Resources: v1.ResourceRequirements{
					Requests: v1.ResourceList{v1.ResourceCPU: cpu500m, v1.ResourceMemory: mem500M},
				},
			},
		},
	}
	cpu800mMem800MPodSpec := cpu500mMem500MPodSpec.DeepCopy()
	cpu800mMem800MPodSpec.Containers[0].Resources.Requests = v1.ResourceList{v1.ResourceCPU: cpu800m, v1.ResourceMemory: mem800M}
	cpu800mPodSpec := cpu500mMem500MPodSpec.DeepCopy()
	cpu800mPodSpec.Containers[0].Resources.Requests = v1.ResourceList{v1.ResourceCPU: cpu800m}
	mem800MPodSpec := cpu500mMem500MPodSpec.DeepCopy()
	mem800MPodSpec.Containers[0].Resources.Requests = v1.ResourceList{v1.ResourceMemory: mem800M}

	cpu500mPodSpec := cpu500mMem500MPodSpec.DeepCopy()
	cpu500mPodSpec.Containers[0].Resources.Requests = v1.ResourceList{v1.ResourceCPU: cpu500m}
	mem500MPodSpec := cpu500mMem500MPodSpec.DeepCopy()
	mem500MPodSpec.Containers[0].Resources.Requests = v1.ResourceList{v1.ResourceMemory: mem500M}
	emptyPodSpec := cpu500mMem500MPodSpec.DeepCopy()
	emptyPodSpec.Containers[0].Resources.Requests = v1.ResourceList{}

	tests := []struct {
		name                          string
		pod                           *v1.Pod
		existingPodAllocation         *v1.Pod
		expectedPodResourceAllocation state.PodResourceAllocation
	}{
		{
			name: "Having both memory and cpu, resource allocation not exists",
			pod:  podWithUIDNameNsSpec("1", "pod1", "foo", *cpu500mMem500MPodSpec),
			expectedPodResourceAllocation: state.PodResourceAllocation{
				"1": map[string]v1.ResourceRequirements{
					cpu500mMem500MPodSpec.Containers[0].Name: cpu500mMem500MPodSpec.Containers[0].Resources,
				},
			},
		},
		{
			name:                  "Having both memory and cpu, resource allocation exists",
			pod:                   podWithUIDNameNsSpec("2", "pod2", "foo", *cpu500mMem500MPodSpec),
			existingPodAllocation: podWithUIDNameNsSpec("2", "pod2", "foo", *cpu500mMem500MPodSpec),
			expectedPodResourceAllocation: state.PodResourceAllocation{
				"2": map[string]v1.ResourceRequirements{
					cpu500mMem500MPodSpec.Containers[0].Name: cpu500mMem500MPodSpec.Containers[0].Resources,
				},
			},
		},
		{
			name:                  "Having both memory and cpu, resource allocation exists (with different value)",
			pod:                   podWithUIDNameNsSpec("3", "pod3", "foo", *cpu500mMem500MPodSpec),
			existingPodAllocation: podWithUIDNameNsSpec("3", "pod3", "foo", *cpu800mMem800MPodSpec),
			expectedPodResourceAllocation: state.PodResourceAllocation{
				"3": map[string]v1.ResourceRequirements{
					cpu800mMem800MPodSpec.Containers[0].Name: cpu800mMem800MPodSpec.Containers[0].Resources,
				},
			},
		},
		{
			name: "Only has cpu, resource allocation not exists",
			pod:  podWithUIDNameNsSpec("4", "pod5", "foo", *cpu500mPodSpec),
			expectedPodResourceAllocation: state.PodResourceAllocation{
				"4": map[string]v1.ResourceRequirements{
					cpu500mPodSpec.Containers[0].Name: cpu500mPodSpec.Containers[0].Resources,
				},
			},
		},
		{
			name:                  "Only has cpu, resource allocation exists",
			pod:                   podWithUIDNameNsSpec("5", "pod5", "foo", *cpu500mPodSpec),
			existingPodAllocation: podWithUIDNameNsSpec("5", "pod5", "foo", *cpu500mPodSpec),
			expectedPodResourceAllocation: state.PodResourceAllocation{
				"5": map[string]v1.ResourceRequirements{
					cpu500mPodSpec.Containers[0].Name: cpu500mPodSpec.Containers[0].Resources,
				},
			},
		},
		{
			name:                  "Only has cpu, resource allocation exists (with different value)",
			pod:                   podWithUIDNameNsSpec("6", "pod6", "foo", *cpu500mPodSpec),
			existingPodAllocation: podWithUIDNameNsSpec("6", "pod6", "foo", *cpu800mPodSpec),
			expectedPodResourceAllocation: state.PodResourceAllocation{
				"6": map[string]v1.ResourceRequirements{
					cpu800mPodSpec.Containers[0].Name: cpu800mPodSpec.Containers[0].Resources,
				},
			},
		},
		{
			name: "Only has memory, resource allocation not exists",
			pod:  podWithUIDNameNsSpec("7", "pod7", "foo", *mem500MPodSpec),
			expectedPodResourceAllocation: state.PodResourceAllocation{
				"7": map[string]v1.ResourceRequirements{
					mem500MPodSpec.Containers[0].Name: mem500MPodSpec.Containers[0].Resources,
				},
			},
		},
		{
			name:                  "Only has memory, resource allocation exists",
			pod:                   podWithUIDNameNsSpec("8", "pod8", "foo", *mem500MPodSpec),
			existingPodAllocation: podWithUIDNameNsSpec("8", "pod8", "foo", *mem500MPodSpec),
			expectedPodResourceAllocation: state.PodResourceAllocation{
				"8": map[string]v1.ResourceRequirements{
					mem500MPodSpec.Containers[0].Name: mem500MPodSpec.Containers[0].Resources,
				},
			},
		},
		{
			name:                  "Only has memory, resource allocation exists (with different value)",
			pod:                   podWithUIDNameNsSpec("9", "pod9", "foo", *mem500MPodSpec),
			existingPodAllocation: podWithUIDNameNsSpec("9", "pod9", "foo", *mem800MPodSpec),
			expectedPodResourceAllocation: state.PodResourceAllocation{
				"9": map[string]v1.ResourceRequirements{
					mem800MPodSpec.Containers[0].Name: mem800MPodSpec.Containers[0].Resources,
				},
			},
		},
		{
			name: "No CPU and memory, resource allocation not exists",
			pod:  podWithUIDNameNsSpec("10", "pod10", "foo", *emptyPodSpec),
			expectedPodResourceAllocation: state.PodResourceAllocation{
				"10": map[string]v1.ResourceRequirements{
					emptyPodSpec.Containers[0].Name: emptyPodSpec.Containers[0].Resources,
				},
			},
		},
		{
			name:                  "No CPU and memory, resource allocation exists",
			pod:                   podWithUIDNameNsSpec("11", "pod11", "foo", *emptyPodSpec),
			existingPodAllocation: podWithUIDNameNsSpec("11", "pod11", "foo", *emptyPodSpec),
			expectedPodResourceAllocation: state.PodResourceAllocation{
				"11": map[string]v1.ResourceRequirements{
					emptyPodSpec.Containers[0].Name: emptyPodSpec.Containers[0].Resources,
				},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.existingPodAllocation != nil {
				// when kubelet restarts, AllocatedResources has already existed before adding pod
				err := kubelet.statusManager.SetPodAllocation(tc.existingPodAllocation)
				if err != nil {
					t.Fatalf("failed to set pod allocation: %v", err)
				}
			}
			kubelet.HandlePodAdditions([]*v1.Pod{tc.pod})

			allocatedResources, found := kubelet.statusManager.GetContainerResourceAllocation(string(tc.pod.UID), tc.pod.Spec.Containers[0].Name)
			if !found {
				t.Fatalf("resource allocation should exist: (pod: %#v, container: %s)", tc.pod, tc.pod.Spec.Containers[0].Name)
			}
			assert.Equal(t, tc.expectedPodResourceAllocation[string(tc.pod.UID)][tc.pod.Spec.Containers[0].Name], allocatedResources, tc.name)
		})
	}
}

func TestHandlePodResourcesResize(t *testing.T) {
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.InPlacePodVerticalScaling, true)
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.SidecarContainers, true)
	testKubelet := newTestKubelet(t, false)
	defer testKubelet.Cleanup()
	kubelet := testKubelet.kubelet
	containerRestartPolicyAlways := v1.ContainerRestartPolicyAlways

	cpu1m := resource.MustParse("1m")
	cpu2m := resource.MustParse("2m")
	cpu500m := resource.MustParse("500m")
	cpu1000m := resource.MustParse("1")
	cpu1500m := resource.MustParse("1500m")
	cpu2500m := resource.MustParse("2500m")
	cpu5000m := resource.MustParse("5000m")
	mem500M := resource.MustParse("500Mi")
	mem1000M := resource.MustParse("1Gi")
	mem1500M := resource.MustParse("1500Mi")
	mem2500M := resource.MustParse("2500Mi")
	mem4500M := resource.MustParse("4500Mi")

	nodes := []*v1.Node{
		{
			ObjectMeta: metav1.ObjectMeta{Name: testKubeletHostname},
			Status: v1.NodeStatus{
				Capacity: v1.ResourceList{
					v1.ResourceCPU:    resource.MustParse("8"),
					v1.ResourceMemory: resource.MustParse("8Gi"),
				},
				Allocatable: v1.ResourceList{
					v1.ResourceCPU:    resource.MustParse("4"),
					v1.ResourceMemory: resource.MustParse("4Gi"),
					v1.ResourcePods:   *resource.NewQuantity(40, resource.DecimalSI),
				},
			},
		},
	}
	kubelet.nodeLister = testNodeLister{nodes: nodes}

	testPod1 := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			UID:       "1111",
			Name:      "pod1",
			Namespace: "ns1",
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Name:  "c1",
					Image: "i1",
					Resources: v1.ResourceRequirements{
						Requests: v1.ResourceList{v1.ResourceCPU: cpu1000m, v1.ResourceMemory: mem1000M},
					},
				},
			},
		},
		Status: v1.PodStatus{
			Phase: v1.PodRunning,
			ContainerStatuses: []v1.ContainerStatus{
				{
					Name:               "c1",
					AllocatedResources: v1.ResourceList{v1.ResourceCPU: cpu1000m, v1.ResourceMemory: mem1000M},
					Resources:          &v1.ResourceRequirements{},
				},
			},
		},
	}
	testPod2 := testPod1.DeepCopy()
	testPod2.UID = "2222"
	testPod2.Name = "pod2"
	testPod2.Namespace = "ns2"
	testPod2.Spec = v1.PodSpec{
		InitContainers: []v1.Container{
			{
				Name:  "c1-init",
				Image: "i1",
				Resources: v1.ResourceRequirements{
					Requests: v1.ResourceList{v1.ResourceCPU: cpu1000m, v1.ResourceMemory: mem1000M},
				},
				RestartPolicy: &containerRestartPolicyAlways,
			},
		},
	}
	testPod2.Status = v1.PodStatus{
		Phase: v1.PodRunning,
		InitContainerStatuses: []v1.ContainerStatus{
			{
				Name:               "c1-init",
				AllocatedResources: v1.ResourceList{v1.ResourceCPU: cpu1000m, v1.ResourceMemory: mem1000M},
				Resources:          &v1.ResourceRequirements{},
			},
		},
	}
	testPod3 := testPod1.DeepCopy()
	testPod3.UID = "3333"
	testPod3.Name = "pod3"
	testPod3.Namespace = "ns2"

	testKubelet.fakeKubeClient = fake.NewSimpleClientset(testPod1, testPod2, testPod3)
	kubelet.kubeClient = testKubelet.fakeKubeClient
	defer testKubelet.fakeKubeClient.ClearActions()
	kubelet.podManager.AddPod(testPod1)
	kubelet.podManager.AddPod(testPod2)
	kubelet.podManager.AddPod(testPod3)
	kubelet.podWorkers.(*fakePodWorkers).running = map[types.UID]bool{
		testPod1.UID: true,
		testPod2.UID: true,
		testPod3.UID: true,
	}
	defer kubelet.podManager.RemovePod(testPod3)
	defer kubelet.podManager.RemovePod(testPod2)
	defer kubelet.podManager.RemovePod(testPod1)

	tests := []struct {
		name                  string
		originalRequests      v1.ResourceList
		newRequests           v1.ResourceList
		originalLimits        v1.ResourceList
		newLimits             v1.ResourceList
		newResourcesAllocated bool // Whether the new requests have already been allocated (but not actuated)
		expectedAllocatedReqs v1.ResourceList
		expectedAllocatedLims v1.ResourceList
		expectedResize        v1.PodResizeStatus
		expectBackoffReset    bool
		goos                  string
	}{
		{
			name:                  "Request CPU and memory decrease - expect InProgress",
			originalRequests:      v1.ResourceList{v1.ResourceCPU: cpu1000m, v1.ResourceMemory: mem1000M},
			newRequests:           v1.ResourceList{v1.ResourceCPU: cpu500m, v1.ResourceMemory: mem500M},
			expectedAllocatedReqs: v1.ResourceList{v1.ResourceCPU: cpu500m, v1.ResourceMemory: mem500M},
			expectedResize:        v1.PodResizeStatusInProgress,
			expectBackoffReset:    true,
		},
		{
			name:                  "Request CPU increase, memory decrease - expect InProgress",
			originalRequests:      v1.ResourceList{v1.ResourceCPU: cpu1000m, v1.ResourceMemory: mem1000M},
			newRequests:           v1.ResourceList{v1.ResourceCPU: cpu1500m, v1.ResourceMemory: mem500M},
			expectedAllocatedReqs: v1.ResourceList{v1.ResourceCPU: cpu1500m, v1.ResourceMemory: mem500M},
			expectedResize:        v1.PodResizeStatusInProgress,
			expectBackoffReset:    true,
		},
		{
			name:                  "Request CPU decrease, memory increase - expect InProgress",
			originalRequests:      v1.ResourceList{v1.ResourceCPU: cpu1000m, v1.ResourceMemory: mem1000M},
			newRequests:           v1.ResourceList{v1.ResourceCPU: cpu500m, v1.ResourceMemory: mem1500M},
			expectedAllocatedReqs: v1.ResourceList{v1.ResourceCPU: cpu500m, v1.ResourceMemory: mem1500M},
			expectedResize:        v1.PodResizeStatusInProgress,
			expectBackoffReset:    true,
		},
		{
			name:                  "Request CPU and memory increase beyond current capacity - expect Deferred",
			originalRequests:      v1.ResourceList{v1.ResourceCPU: cpu1000m, v1.ResourceMemory: mem1000M},
			newRequests:           v1.ResourceList{v1.ResourceCPU: cpu2500m, v1.ResourceMemory: mem2500M},
			expectedAllocatedReqs: v1.ResourceList{v1.ResourceCPU: cpu1000m, v1.ResourceMemory: mem1000M},
			expectedResize:        v1.PodResizeStatusDeferred,
		},
		{
			name:                  "Request CPU decrease and memory increase beyond current capacity - expect Deferred",
			originalRequests:      v1.ResourceList{v1.ResourceCPU: cpu1000m, v1.ResourceMemory: mem1000M},
			newRequests:           v1.ResourceList{v1.ResourceCPU: cpu500m, v1.ResourceMemory: mem2500M},
			expectedAllocatedReqs: v1.ResourceList{v1.ResourceCPU: cpu1000m, v1.ResourceMemory: mem1000M},
			expectedResize:        v1.PodResizeStatusDeferred,
		},
		{
			name:                  "Request memory increase beyond node capacity - expect Infeasible",
			originalRequests:      v1.ResourceList{v1.ResourceCPU: cpu1000m, v1.ResourceMemory: mem1000M},
			newRequests:           v1.ResourceList{v1.ResourceCPU: cpu1000m, v1.ResourceMemory: mem4500M},
			expectedAllocatedReqs: v1.ResourceList{v1.ResourceCPU: cpu1000m, v1.ResourceMemory: mem1000M},
			expectedResize:        v1.PodResizeStatusInfeasible,
		},
		{
			name:                  "Request CPU increase beyond node capacity - expect Infeasible",
			originalRequests:      v1.ResourceList{v1.ResourceCPU: cpu1000m, v1.ResourceMemory: mem1000M},
			newRequests:           v1.ResourceList{v1.ResourceCPU: cpu5000m, v1.ResourceMemory: mem1000M},
			expectedAllocatedReqs: v1.ResourceList{v1.ResourceCPU: cpu1000m, v1.ResourceMemory: mem1000M},
			expectedResize:        v1.PodResizeStatusInfeasible,
		},
		{
			name:                  "CPU increase in progress - expect InProgress",
			originalRequests:      v1.ResourceList{v1.ResourceCPU: cpu1000m, v1.ResourceMemory: mem1000M},
			newRequests:           v1.ResourceList{v1.ResourceCPU: cpu1500m, v1.ResourceMemory: mem1000M},
			newResourcesAllocated: true,
			expectedAllocatedReqs: v1.ResourceList{v1.ResourceCPU: cpu1500m, v1.ResourceMemory: mem1000M},
			expectedResize:        v1.PodResizeStatusInProgress,
		},
		{
			name:                  "No resize",
			originalRequests:      v1.ResourceList{v1.ResourceCPU: cpu1000m, v1.ResourceMemory: mem1000M},
			newRequests:           v1.ResourceList{v1.ResourceCPU: cpu1000m, v1.ResourceMemory: mem1000M},
			expectedAllocatedReqs: v1.ResourceList{v1.ResourceCPU: cpu1000m, v1.ResourceMemory: mem1000M},
			expectedResize:        "",
		},
		{
			name:                  "windows node, expect Infeasible",
			originalRequests:      v1.ResourceList{v1.ResourceCPU: cpu1000m, v1.ResourceMemory: mem1000M},
			newRequests:           v1.ResourceList{v1.ResourceCPU: cpu500m, v1.ResourceMemory: mem500M},
			expectedAllocatedReqs: v1.ResourceList{v1.ResourceCPU: cpu1000m, v1.ResourceMemory: mem1000M},
			expectedResize:        v1.PodResizeStatusInfeasible,
			goos:                  "windows",
		},
		{
			name:                  "Increase CPU from min shares",
			originalRequests:      v1.ResourceList{v1.ResourceCPU: cpu2m},
			newRequests:           v1.ResourceList{v1.ResourceCPU: cpu1000m},
			expectedAllocatedReqs: v1.ResourceList{v1.ResourceCPU: cpu1000m},
			expectedResize:        v1.PodResizeStatusInProgress,
			expectBackoffReset:    true,
		},
		{
			name:                  "Decrease CPU to min shares",
			originalRequests:      v1.ResourceList{v1.ResourceCPU: cpu1000m},
			newRequests:           v1.ResourceList{v1.ResourceCPU: cpu2m},
			expectedAllocatedReqs: v1.ResourceList{v1.ResourceCPU: cpu2m},
			expectedResize:        v1.PodResizeStatusInProgress,
			expectBackoffReset:    true,
		},
		{
			name:                  "Equivalent min CPU shares",
			originalRequests:      v1.ResourceList{v1.ResourceCPU: cpu1m},
			newRequests:           v1.ResourceList{v1.ResourceCPU: cpu2m},
			expectedAllocatedReqs: v1.ResourceList{v1.ResourceCPU: cpu2m},
			expectedResize:        "",
			// Even though the resize isn't being actuated, we still clear the container backoff
			// since the allocation is changing.
			expectBackoffReset: true,
		},
		{
			name:                  "Equivalent min CPU shares - already allocated",
			originalRequests:      v1.ResourceList{v1.ResourceCPU: cpu2m},
			newRequests:           v1.ResourceList{v1.ResourceCPU: cpu1m},
			newResourcesAllocated: true,
			expectedAllocatedReqs: v1.ResourceList{v1.ResourceCPU: cpu1m},
			expectedResize:        "",
		},
		{
			name:                  "Increase CPU from min limit",
			originalRequests:      v1.ResourceList{v1.ResourceCPU: resource.MustParse("10m")},
			originalLimits:        v1.ResourceList{v1.ResourceCPU: resource.MustParse("10m")},
			newRequests:           v1.ResourceList{v1.ResourceCPU: resource.MustParse("10m")}, // Unchanged
			newLimits:             v1.ResourceList{v1.ResourceCPU: resource.MustParse("20m")},
			expectedAllocatedReqs: v1.ResourceList{v1.ResourceCPU: resource.MustParse("10m")},
			expectedAllocatedLims: v1.ResourceList{v1.ResourceCPU: resource.MustParse("20m")},
			expectedResize:        v1.PodResizeStatusInProgress,
			expectBackoffReset:    true,
		},
		{
			name:                  "Decrease CPU to min limit",
			originalRequests:      v1.ResourceList{v1.ResourceCPU: resource.MustParse("10m")},
			originalLimits:        v1.ResourceList{v1.ResourceCPU: resource.MustParse("20m")},
			newRequests:           v1.ResourceList{v1.ResourceCPU: resource.MustParse("10m")}, // Unchanged
			newLimits:             v1.ResourceList{v1.ResourceCPU: resource.MustParse("10m")},
			expectedAllocatedReqs: v1.ResourceList{v1.ResourceCPU: resource.MustParse("10m")},
			expectedAllocatedLims: v1.ResourceList{v1.ResourceCPU: resource.MustParse("10m")},
			expectedResize:        v1.PodResizeStatusInProgress,
			expectBackoffReset:    true,
		},
		{
			name:                  "Equivalent min CPU limit",
			originalRequests:      v1.ResourceList{v1.ResourceCPU: resource.MustParse("5m")},
			originalLimits:        v1.ResourceList{v1.ResourceCPU: resource.MustParse("5m")},
			newRequests:           v1.ResourceList{v1.ResourceCPU: resource.MustParse("5m")}, // Unchanged
			newLimits:             v1.ResourceList{v1.ResourceCPU: resource.MustParse("10m")},
			expectedAllocatedReqs: v1.ResourceList{v1.ResourceCPU: resource.MustParse("5m")},
			expectedAllocatedLims: v1.ResourceList{v1.ResourceCPU: resource.MustParse("10m")},
			expectedResize:        "",
			// Even though the resize isn't being actuated, we still clear the container backoff
			// since the allocation is changing.
			expectBackoffReset: true,
		},
		{
			name:                  "Equivalent min CPU limit - already allocated",
			originalRequests:      v1.ResourceList{v1.ResourceCPU: resource.MustParse("5m")},
			originalLimits:        v1.ResourceList{v1.ResourceCPU: resource.MustParse("10m")},
			newRequests:           v1.ResourceList{v1.ResourceCPU: resource.MustParse("5m")}, // Unchanged
			newLimits:             v1.ResourceList{v1.ResourceCPU: resource.MustParse("5m")},
			expectedAllocatedReqs: v1.ResourceList{v1.ResourceCPU: resource.MustParse("5m")},
			expectedAllocatedLims: v1.ResourceList{v1.ResourceCPU: resource.MustParse("5m")},
			newResourcesAllocated: true,
			expectedResize:        "",
		},
	}

	for _, tt := range tests {
		for _, isSidecarContainer := range []bool{false, true} {
			t.Run(tt.name, func(t *testing.T) {
				oldGOOS := goos
				defer func() { goos = oldGOOS }()
				if tt.goos != "" {
					goos = tt.goos
				}
				kubelet.statusManager = status.NewFakeManager()

				var originalPod *v1.Pod
				var originalCtr *v1.Container
				if isSidecarContainer {
					originalPod = testPod2.DeepCopy()
					originalCtr = &originalPod.Spec.InitContainers[0]
				} else {
					originalPod = testPod1.DeepCopy()
					originalCtr = &originalPod.Spec.Containers[0]
				}
				originalCtr.Resources.Requests = tt.originalRequests
				originalCtr.Resources.Limits = tt.originalLimits

				kubelet.podManager.UpdatePod(originalPod)

				newPod := originalPod.DeepCopy()

				if isSidecarContainer {
					newPod.Spec.InitContainers[0].Resources.Requests = tt.newRequests
					newPod.Spec.InitContainers[0].Resources.Limits = tt.newLimits
				} else {
					newPod.Spec.Containers[0].Resources.Requests = tt.newRequests
					newPod.Spec.Containers[0].Resources.Limits = tt.newLimits
				}

				if !tt.newResourcesAllocated {
					require.NoError(t, kubelet.statusManager.SetPodAllocation(originalPod))
				} else {
					require.NoError(t, kubelet.statusManager.SetPodAllocation(newPod))
				}

				podStatus := &kubecontainer.PodStatus{
					ID:        originalPod.UID,
					Name:      originalPod.Name,
					Namespace: originalPod.Namespace,
				}

				setContainerStatus := func(podStatus *kubecontainer.PodStatus, c *v1.Container, idx int) {
					podStatus.ContainerStatuses[idx] = &kubecontainer.Status{
						Name:  c.Name,
						State: kubecontainer.ContainerStateRunning,
						Resources: &kubecontainer.ContainerResources{
							CPURequest:  c.Resources.Requests.Cpu(),
							CPULimit:    c.Resources.Limits.Cpu(),
							MemoryLimit: c.Resources.Limits.Memory(),
						},
					}
				}

				podStatus.ContainerStatuses = make([]*kubecontainer.Status, len(originalPod.Spec.Containers)+len(originalPod.Spec.InitContainers))
				for i, c := range originalPod.Spec.InitContainers {
					setContainerStatus(podStatus, &c, i)
				}
				for i, c := range originalPod.Spec.Containers {
					setContainerStatus(podStatus, &c, i+len(originalPod.Spec.InitContainers))
				}

				now := kubelet.clock.Now()
				// Put the container in backoff so we can confirm backoff is reset.
				backoffKey := kuberuntime.GetStableKey(originalPod, originalCtr)
				kubelet.backOff.Next(backoffKey, now)

				updatedPod, err := kubelet.handlePodResourcesResize(newPod, podStatus)
				require.NoError(t, err)

				var updatedPodCtr v1.Container
				if isSidecarContainer {
					updatedPodCtr = updatedPod.Spec.InitContainers[0]
				} else {
					updatedPodCtr = updatedPod.Spec.Containers[0]
				}
				assert.Equal(t, tt.expectedAllocatedReqs, updatedPodCtr.Resources.Requests, "updated pod spec requests")
				assert.Equal(t, tt.expectedAllocatedLims, updatedPodCtr.Resources.Limits, "updated pod spec limits")

				alloc, found := kubelet.statusManager.GetContainerResourceAllocation(string(newPod.UID), updatedPodCtr.Name)
				require.True(t, found, "container allocation")
				assert.Equal(t, tt.expectedAllocatedReqs, alloc.Requests, "stored container request allocation")
				assert.Equal(t, tt.expectedAllocatedLims, alloc.Limits, "stored container limit allocation")

				resizeStatus := kubelet.statusManager.GetPodResizeStatus(newPod.UID)
				assert.Equal(t, tt.expectedResize, resizeStatus)

				isInBackoff := kubelet.backOff.IsInBackOffSince(backoffKey, now)
				if tt.expectBackoffReset {
					assert.False(t, isInBackoff, "container backoff should be reset")
				} else {
					assert.True(t, isInBackoff, "container backoff should not be reset")
				}
			})
		}
	}
}

// testPodSyncLoopHandler is a lifecycle.PodSyncLoopHandler that is used for testing.
type testPodSyncLoopHandler struct {
	// list of pods to sync
	podsToSync []*v1.Pod
}

// ShouldSync evaluates if the pod should be synced from the kubelet.
func (a *testPodSyncLoopHandler) ShouldSync(pod *v1.Pod) bool {
	for _, podToSync := range a.podsToSync {
		if podToSync.UID == pod.UID {
			return true
		}
	}
	return false
}

// TestGetPodsToSyncInvokesPodSyncLoopHandlers ensures that the get pods to sync routine invokes the handler.
func TestGetPodsToSyncInvokesPodSyncLoopHandlers(t *testing.T) {
	testKubelet := newTestKubelet(t, false /* controllerAttachDetachEnabled */)
	defer testKubelet.Cleanup()
	kubelet := testKubelet.kubelet
	pods := newTestPods(5)
	expected := []*v1.Pod{pods[0]}
	kubelet.AddPodSyncLoopHandler(&testPodSyncLoopHandler{expected})
	kubelet.podManager.SetPods(pods)

	podsToSync := kubelet.getPodsToSync()
	sort.Sort(podsByUID(expected))
	sort.Sort(podsByUID(podsToSync))
	assert.Equal(t, expected, podsToSync)
}

// testPodSyncHandler is a lifecycle.PodSyncHandler that is used for testing.
type testPodSyncHandler struct {
	// list of pods to evict.
	podsToEvict []*v1.Pod
	// the reason for the eviction
	reason string
	// the message for the eviction
	message string
}

// ShouldEvict evaluates if the pod should be evicted from the kubelet.
func (a *testPodSyncHandler) ShouldEvict(pod *v1.Pod) lifecycle.ShouldEvictResponse {
	for _, podToEvict := range a.podsToEvict {
		if podToEvict.UID == pod.UID {
			return lifecycle.ShouldEvictResponse{Evict: true, Reason: a.reason, Message: a.message}
		}
	}
	return lifecycle.ShouldEvictResponse{Evict: false}
}

// TestGenerateAPIPodStatusInvokesPodSyncHandlers invokes the handlers and reports the proper status
func TestGenerateAPIPodStatusInvokesPodSyncHandlers(t *testing.T) {
	testKubelet := newTestKubelet(t, false /* controllerAttachDetachEnabled */)
	defer testKubelet.Cleanup()
	kubelet := testKubelet.kubelet
	pod := newTestPods(1)[0]
	podsToEvict := []*v1.Pod{pod}
	kubelet.AddPodSyncHandler(&testPodSyncHandler{podsToEvict, "Evicted", "because"})
	status := &kubecontainer.PodStatus{
		ID:        pod.UID,
		Name:      pod.Name,
		Namespace: pod.Namespace,
	}
	apiStatus := kubelet.generateAPIPodStatus(pod, status, false)
	require.Equal(t, v1.PodFailed, apiStatus.Phase)
	require.Equal(t, "Evicted", apiStatus.Reason)
	require.Equal(t, "because", apiStatus.Message)
}

func TestSyncTerminatingPodKillPod(t *testing.T) {
	testKubelet := newTestKubelet(t, false /* controllerAttachDetachEnabled */)
	defer testKubelet.Cleanup()
	kl := testKubelet.kubelet
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			UID:       "12345678",
			Name:      "bar",
			Namespace: "foo",
		},
	}
	pods := []*v1.Pod{pod}
	kl.podManager.SetPods(pods)
	podStatus := &kubecontainer.PodStatus{ID: pod.UID}
	gracePeriodOverride := int64(0)
	err := kl.SyncTerminatingPod(context.Background(), pod, podStatus, &gracePeriodOverride, func(podStatus *v1.PodStatus) {
		podStatus.Phase = v1.PodFailed
		podStatus.Reason = "reason"
		podStatus.Message = "message"
	})
	require.NoError(t, err)

	// Check pod status stored in the status map.
	checkPodStatus(t, kl, pod, v1.PodFailed)
}

func TestSyncLabels(t *testing.T) {
	tests := []struct {
		name             string
		existingNode     *v1.Node
		isPatchingNeeded bool
	}{
		{
			name:             "no labels",
			existingNode:     &v1.Node{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{}}},
			isPatchingNeeded: true,
		},
		{
			name:             "wrong labels",
			existingNode:     &v1.Node{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{v1.LabelOSStable: "dummyOS", v1.LabelArchStable: "dummyArch"}}},
			isPatchingNeeded: true,
		},
		{
			name:             "correct labels",
			existingNode:     &v1.Node{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{v1.LabelOSStable: goruntime.GOOS, v1.LabelArchStable: goruntime.GOARCH}}},
			isPatchingNeeded: false,
		},
		{
			name:             "partially correct labels",
			existingNode:     &v1.Node{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{v1.LabelOSStable: goruntime.GOOS, v1.LabelArchStable: "dummyArch"}}},
			isPatchingNeeded: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			testKubelet := newTestKubelet(t, false)
			defer testKubelet.Cleanup()
			kl := testKubelet.kubelet
			kubeClient := testKubelet.fakeKubeClient

			test.existingNode.Name = string(kl.nodeName)

			kl.nodeLister = testNodeLister{nodes: []*v1.Node{test.existingNode}}
			go func() { kl.syncNodeStatus() }()

			err := retryWithExponentialBackOff(
				100*time.Millisecond,
				func() (bool, error) {
					var savedNode *v1.Node
					if test.isPatchingNeeded {
						actions := kubeClient.Actions()
						if len(actions) == 0 {
							t.Logf("No action yet")
							return false, nil
						}
						for _, action := range actions {
							if action.GetVerb() == "patch" {
								var (
									err          error
									patchAction  = action.(core.PatchActionImpl)
									patchContent = patchAction.GetPatch()
								)
								savedNode, err = applyNodeStatusPatch(test.existingNode, patchContent)
								if err != nil {
									t.Logf("node patching failed, %v", err)
									return false, nil
								}
							}
						}
					} else {
						savedNode = test.existingNode
					}
					if savedNode == nil || savedNode.Labels == nil {
						t.Logf("savedNode.Labels should not be nil")
						return false, nil
					}
					val, ok := savedNode.Labels[v1.LabelOSStable]
					if !ok {
						t.Logf("expected kubernetes.io/os label to be present")
						return false, nil
					}
					if val != goruntime.GOOS {
						t.Logf("expected kubernetes.io/os to match runtime.GOOS but got %v", val)
						return false, nil
					}
					val, ok = savedNode.Labels[v1.LabelArchStable]
					if !ok {
						t.Logf("expected kubernetes.io/arch label to be present")
						return false, nil
					}
					if val != goruntime.GOARCH {
						t.Logf("expected kubernetes.io/arch to match runtime.GOARCH but got %v", val)
						return false, nil
					}
					return true, nil
				},
			)
			if err != nil {
				t.Fatalf("expected labels to be reconciled but it failed with %v", err)
			}
		})
	}
}

func waitForVolumeUnmount(
	volumeManager kubeletvolume.VolumeManager,
	pod *v1.Pod) error {
	var podVolumes kubecontainer.VolumeMap
	err := retryWithExponentialBackOff(
		time.Duration(50*time.Millisecond),
		func() (bool, error) {
			// Verify volumes detached
			podVolumes = volumeManager.GetMountedVolumesForPod(
				util.GetUniquePodName(pod))

			if len(podVolumes) != 0 {
				return false, nil
			}

			return true, nil
		},
	)

	if err != nil {
		return fmt.Errorf(
			"Expected volumes to be unmounted. But some volumes are still mounted: %#v", podVolumes)
	}

	return nil
}

func waitForVolumeDetach(
	volumeName v1.UniqueVolumeName,
	volumeManager kubeletvolume.VolumeManager) error {
	attachedVolumes := []v1.UniqueVolumeName{}
	err := retryWithExponentialBackOff(
		time.Duration(50*time.Millisecond),
		func() (bool, error) {
			// Verify volumes detached
			volumeAttached := volumeManager.VolumeIsAttached(volumeName)
			return !volumeAttached, nil
		},
	)

	if err != nil {
		return fmt.Errorf(
			"Expected volumes to be detached. But some volumes are still attached: %#v", attachedVolumes)
	}

	return nil
}

func retryWithExponentialBackOff(initialDuration time.Duration, fn wait.ConditionFunc) error {
	backoff := wait.Backoff{
		Duration: initialDuration,
		Factor:   3,
		Jitter:   0,
		Steps:    6,
	}
	return wait.ExponentialBackoff(backoff, fn)
}

func simulateVolumeInUseUpdate(
	volumeName v1.UniqueVolumeName,
	stopCh <-chan struct{},
	volumeManager kubeletvolume.VolumeManager) {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			volumeManager.MarkVolumesAsReportedInUse(
				[]v1.UniqueVolumeName{volumeName})
		case <-stopCh:
			return
		}
	}
}

// dirExists returns true if the path exists and represents a directory.
func dirExists(path string) bool {
	s, err := os.Stat(path)
	if err != nil {
		return false
	}
	return s.IsDir()
}

// Sort pods by UID.
type podsByUID []*v1.Pod

func (p podsByUID) Len() int           { return len(p) }
func (p podsByUID) Swap(i, j int)      { p[i], p[j] = p[j], p[i] }
func (p podsByUID) Less(i, j int) bool { return p[i].UID < p[j].UID }

// createAndStartFakeRemoteRuntime creates and starts fakeremote.RemoteRuntime.
// It returns the RemoteRuntime, endpoint on success.
// Users should call fakeRuntime.Stop() to cleanup the server.
func createAndStartFakeRemoteRuntime(t *testing.T) (*fakeremote.RemoteRuntime, string) {
	endpoint, err := fakeremote.GenerateEndpoint()
	require.NoError(t, err)

	fakeRuntime := fakeremote.NewFakeRemoteRuntime()
	fakeRuntime.Start(endpoint)

	return fakeRuntime, endpoint
}

func createRemoteRuntimeService(endpoint string, t *testing.T, tp oteltrace.TracerProvider) internalapi.RuntimeService {
	logger := klog.Background()
	runtimeService, err := remote.NewRemoteRuntimeService(endpoint, 15*time.Second, tp, &logger)
	require.NoError(t, err)
	return runtimeService
}

func TestNewMainKubeletStandAlone(t *testing.T) {
	tCtx := ktesting.Init(t)
	tempDir, err := os.MkdirTemp("", "logs")
	ContainerLogsDir = tempDir
	assert.NoError(t, err)
	defer os.RemoveAll(ContainerLogsDir)
	kubeCfg := &kubeletconfiginternal.KubeletConfiguration{
		SyncFrequency: metav1.Duration{Duration: time.Minute},
		ConfigMapAndSecretChangeDetectionStrategy: kubeletconfiginternal.WatchChangeDetectionStrategy,
		ContainerLogMaxSize:                       "10Mi",
		ContainerLogMaxFiles:                      5,
		MemoryThrottlingFactor:                    ptr.To[float64](0),
	}
	var prober volume.DynamicPluginProber
	tp := noopoteltrace.NewTracerProvider()
	cadvisor := cadvisortest.NewMockInterface(t)
	cadvisor.EXPECT().MachineInfo().Return(&cadvisorapi.MachineInfo{}, nil).Maybe()
	cadvisor.EXPECT().ImagesFsInfo(tCtx).Return(cadvisorapiv2.FsInfo{
		Usage:     400,
		Capacity:  1000,
		Available: 600,
	}, nil).Maybe()
	tlsOptions := &server.TLSOptions{
		Config: &tls.Config{
			MinVersion: 0,
		},
	}
	fakeRuntime, endpoint := createAndStartFakeRemoteRuntime(t)
	defer func() {
		fakeRuntime.Stop()
	}()
	fakeRecorder := &record.FakeRecorder{}
	rtSvc := createRemoteRuntimeService(endpoint, t, noopoteltrace.NewTracerProvider())
	kubeDep := &Dependencies{
		Auth:                 nil,
		CAdvisorInterface:    cadvisor,
		Cloud:                nil,
		ContainerManager:     cm.NewStubContainerManager(),
		KubeClient:           nil, // standalone mode
		HeartbeatClient:      nil,
		EventClient:          nil,
		TracerProvider:       tp,
		HostUtil:             hostutil.NewFakeHostUtil(nil),
		Mounter:              mount.NewFakeMounter(nil),
		Recorder:             fakeRecorder,
		RemoteRuntimeService: rtSvc,
		RemoteImageService:   fakeRuntime.ImageService,
		Subpather:            &subpath.FakeSubpath{},
		OOMAdjuster:          oom.NewOOMAdjuster(),
		OSInterface:          kubecontainer.RealOS{},
		DynamicPluginProber:  prober,
		TLSOptions:           tlsOptions,
	}
	crOptions := &config.ContainerRuntimeOptions{}

	testMainKubelet, err := NewMainKubelet(
		kubeCfg,
		kubeDep,
		crOptions,
		"hostname",
		false,
		"hostname",
		[]net.IP{},
		"",
		"external",
		"/tmp/cert",
		"/tmp/rootdir",
		tempDir,
		"",
		"",
		false,
		[]v1.Taint{},
		[]string{},
		"",
		false,
		false,
		metav1.Duration{Duration: time.Minute},
		1024,
		110,
		true,
		map[string]string{},
		1024,
		false,
	)
	assert.NoError(t, err, "NewMainKubelet should succeed")
	assert.NotNil(t, testMainKubelet, "testMainKubelet should not be nil")

	testMainKubelet.BirthCry()
	testMainKubelet.StartGarbageCollection()
	// Nil pointer panic can be reproduced if configmap manager is not nil.
	// See https://github.com/kubernetes/kubernetes/issues/113492
	// pod := &v1.Pod{
	// 	ObjectMeta: metav1.ObjectMeta{
	// 		UID:       "12345678",
	// 		Name:      "bar",
	// 		Namespace: "foo",
	// 	},
	// 	Spec: v1.PodSpec{
	// 		Containers: []v1.Container{{
	// 			EnvFrom: []v1.EnvFromSource{{
	// 				ConfigMapRef: &v1.ConfigMapEnvSource{
	// 					LocalObjectReference: v1.LocalObjectReference{Name: "config-map"}}},
	// 			}}},
	// 		Volumes: []v1.Volume{{
	// 			VolumeSource: v1.VolumeSource{
	// 				ConfigMap: &v1.ConfigMapVolumeSource{
	// 					LocalObjectReference: v1.LocalObjectReference{
	// 						Name: "config-map"}}}}},
	// 	},
	// }
	// testMainKubelet.configMapManager.RegisterPod(pod)
	// testMainKubelet.secretManager.RegisterPod(pod)
	assert.Nil(t, testMainKubelet.configMapManager, "configmap manager should be nil if kubelet is in standalone mode")
	assert.Nil(t, testMainKubelet.secretManager, "secret manager should be nil if kubelet is in standalone mode")
}

func TestSyncPodSpans(t *testing.T) {
	testKubelet := newTestKubelet(t, false)
	kubelet := testKubelet.kubelet

	recorder := record.NewFakeRecorder(20)
	nodeRef := &v1.ObjectReference{
		Kind:      "Node",
		Name:      "testNode",
		UID:       types.UID("testNode"),
		Namespace: "",
	}
	kubelet.dnsConfigurer = dns.NewConfigurer(recorder, nodeRef, nil, nil, "TEST", "")

	kubeCfg := &kubeletconfiginternal.KubeletConfiguration{
		SyncFrequency: metav1.Duration{Duration: time.Minute},
		ConfigMapAndSecretChangeDetectionStrategy: kubeletconfiginternal.WatchChangeDetectionStrategy,
		ContainerLogMaxSize:                       "10Mi",
		ContainerLogMaxFiles:                      5,
		MemoryThrottlingFactor:                    ptr.To[float64](0),
	}

	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSyncer(exp),
	)
	kubelet.tracer = tp.Tracer(instrumentationScope)

	fakeRuntime, endpoint := createAndStartFakeRemoteRuntime(t)
	defer func() {
		fakeRuntime.Stop()
	}()
	runtimeSvc := createRemoteRuntimeService(endpoint, t, tp)
	kubelet.runtimeService = runtimeSvc

	fakeRuntime.ImageService.SetFakeImageSize(100)
	fakeRuntime.ImageService.SetFakeImages([]string{"test:latest"})
	logger := klog.Background()
	imageSvc, err := remote.NewRemoteImageService(endpoint, 15*time.Second, tp, &logger)
	assert.NoError(t, err)

	kubelet.containerRuntime, err = kuberuntime.NewKubeGenericRuntimeManager(
		kubelet.recorder,
		kubelet.livenessManager,
		kubelet.readinessManager,
		kubelet.startupManager,
		kubelet.rootDirectory,
		kubelet.podLogsDirectory,
		kubelet.machineInfo,
		kubelet.podWorkers,
		kubelet.os,
		kubelet,
		nil,
		kubelet.backOff,
		kubeCfg.SerializeImagePulls,
		kubeCfg.MaxParallelImagePulls,
		float32(kubeCfg.RegistryPullQPS),
		int(kubeCfg.RegistryBurst),
		"",
		"",
		nil,
		kubeCfg.CPUCFSQuota,
		kubeCfg.CPUCFSQuotaPeriod,
		runtimeSvc,
		imageSvc,
		kubelet.containerManager,
		kubelet.containerLogManager,
		kubelet.runtimeClassManager,
		false,
		kubeCfg.MemorySwap.SwapBehavior,
		kubelet.containerManager.GetNodeAllocatableAbsolute,
		*kubeCfg.MemoryThrottlingFactor,
		kubeletutil.NewPodStartupLatencyTracker(),
		tp,
	)
	assert.NoError(t, err)

	pod := podWithUIDNameNsSpec("12345678", "foo", "new", v1.PodSpec{
		Containers: []v1.Container{
			{
				Name:            "bar",
				Image:           "test:latest",
				ImagePullPolicy: v1.PullAlways,
			},
		},
		EnableServiceLinks: ptr.To(false),
	})

	_, err = kubelet.SyncPod(context.Background(), kubetypes.SyncPodCreate, pod, nil, &kubecontainer.PodStatus{})
	require.NoError(t, err)

	require.NoError(t, err)
	assert.NotEmpty(t, exp.GetSpans())

	// find root span for syncPod
	var rootSpan *tracetest.SpanStub
	spans := exp.GetSpans()
	for i, span := range spans {
		if span.Name == "syncPod" {
			rootSpan = &spans[i]
			break
		}
	}
	assert.NotNil(t, rootSpan)

	imageServiceSpans := make([]tracetest.SpanStub, 0)
	runtimeServiceSpans := make([]tracetest.SpanStub, 0)
	for _, span := range exp.GetSpans() {
		if span.SpanContext.TraceID() == rootSpan.SpanContext.TraceID() {
			switch {
			case strings.HasPrefix(span.Name, "runtime.v1.ImageService"):
				imageServiceSpans = append(imageServiceSpans, span)
			case strings.HasPrefix(span.Name, "runtime.v1.RuntimeService"):
				runtimeServiceSpans = append(runtimeServiceSpans, span)
			}
		}
	}
	assert.NotEmpty(t, imageServiceSpans, "syncPod trace should have image service spans")
	assert.NotEmpty(t, runtimeServiceSpans, "syncPod trace should have runtime service spans")

	for _, span := range imageServiceSpans {
		assert.Equalf(t, span.Parent.SpanID(), rootSpan.SpanContext.SpanID(), "image service span %s %s should be child of root span", span.Name, span.Parent.SpanID())
	}

	for _, span := range runtimeServiceSpans {
		assert.Equalf(t, span.Parent.SpanID(), rootSpan.SpanContext.SpanID(), "runtime service span %s %s should be child of root span", span.Name, span.Parent.SpanID())
	}
}

func TestRecordAdmissionRejection(t *testing.T) {
	metrics.Register()

	testCases := []struct {
		name   string
		reason string
		wants  string
	}{
		{
			name:   "AppArmor",
			reason: lifecycle.AppArmorNotAdmittedReason,
			wants: `
				# HELP kubelet_admission_rejections_total [ALPHA] Cumulative number pod admission rejections by the Kubelet.
				# TYPE kubelet_admission_rejections_total counter
				kubelet_admission_rejections_total{reason="AppArmor"} 1
			`,
		},
		{
			name:   "PodOSSelectorNodeLabelDoesNotMatch",
			reason: lifecycle.PodOSSelectorNodeLabelDoesNotMatch,
			wants: `
                # HELP kubelet_admission_rejections_total [ALPHA] Cumulative number pod admission rejections by the Kubelet.
                # TYPE kubelet_admission_rejections_total counter
                kubelet_admission_rejections_total{reason="PodOSSelectorNodeLabelDoesNotMatch"} 1
            `,
		},
		{
			name:   "PodOSNotSupported",
			reason: lifecycle.PodOSNotSupported,
			wants: `
                # HELP kubelet_admission_rejections_total [ALPHA] Cumulative number pod admission rejections by the Kubelet.
                # TYPE kubelet_admission_rejections_total counter
                kubelet_admission_rejections_total{reason="PodOSNotSupported"} 1
            `,
		},
		{
			name:   "InvalidNodeInfo",
			reason: lifecycle.InvalidNodeInfo,
			wants: `
                # HELP kubelet_admission_rejections_total [ALPHA] Cumulative number pod admission rejections by the Kubelet.
                # TYPE kubelet_admission_rejections_total counter
                kubelet_admission_rejections_total{reason="InvalidNodeInfo"} 1
            `,
		},
		{
			name:   "InitContainerRestartPolicyForbidden",
			reason: lifecycle.InitContainerRestartPolicyForbidden,
			wants: `
                # HELP kubelet_admission_rejections_total [ALPHA] Cumulative number pod admission rejections by the Kubelet.
                # TYPE kubelet_admission_rejections_total counter
                kubelet_admission_rejections_total{reason="InitContainerRestartPolicyForbidden"} 1
            `,
		},
		{
			name:   "UnexpectedAdmissionError",
			reason: lifecycle.UnexpectedAdmissionError,
			wants: `
                # HELP kubelet_admission_rejections_total [ALPHA] Cumulative number pod admission rejections by the Kubelet.
                # TYPE kubelet_admission_rejections_total counter
                kubelet_admission_rejections_total{reason="UnexpectedAdmissionError"} 1
            `,
		},
		{
			name:   "UnknownReason",
			reason: lifecycle.UnknownReason,
			wants: `
                # HELP kubelet_admission_rejections_total [ALPHA] Cumulative number pod admission rejections by the Kubelet.
                # TYPE kubelet_admission_rejections_total counter
                kubelet_admission_rejections_total{reason="UnknownReason"} 1
            `,
		},
		{
			name:   "UnexpectedPredicateFailureType",
			reason: lifecycle.UnexpectedPredicateFailureType,
			wants: `
                # HELP kubelet_admission_rejections_total [ALPHA] Cumulative number pod admission rejections by the Kubelet.
                # TYPE kubelet_admission_rejections_total counter
                kubelet_admission_rejections_total{reason="UnexpectedPredicateFailureType"} 1
            `,
		},
		{
			name:   "node(s) had taints that the pod didn't tolerate",
			reason: tainttoleration.ErrReasonNotMatch,
			wants: `
                # HELP kubelet_admission_rejections_total [ALPHA] Cumulative number pod admission rejections by the Kubelet.
                # TYPE kubelet_admission_rejections_total counter
                kubelet_admission_rejections_total{reason="node(s) had taints that the pod didn't tolerate"} 1
            `,
		},
		{
			name:   "Evicted",
			reason: eviction.Reason,
			wants: `
                # HELP kubelet_admission_rejections_total [ALPHA] Cumulative number pod admission rejections by the Kubelet.
                # TYPE kubelet_admission_rejections_total counter
                kubelet_admission_rejections_total{reason="Evicted"} 1
            `,
		},
		{
			name:   "SysctlForbidden",
			reason: sysctl.ForbiddenReason,
			wants: `
                # HELP kubelet_admission_rejections_total [ALPHA] Cumulative number pod admission rejections by the Kubelet.
                # TYPE kubelet_admission_rejections_total counter
                kubelet_admission_rejections_total{reason="SysctlForbidden"} 1
            `,
		},
		{
			name:   "TopologyAffinityError",
			reason: topologymanager.ErrorTopologyAffinity,
			wants: `
                # HELP kubelet_admission_rejections_total [ALPHA] Cumulative number pod admission rejections by the Kubelet.
                # TYPE kubelet_admission_rejections_total counter
                kubelet_admission_rejections_total{reason="TopologyAffinityError"} 1
            `,
		},
		{
			name:   "NodeShutdown",
			reason: nodeshutdown.NodeShutdownNotAdmittedReason,
			wants: `
                # HELP kubelet_admission_rejections_total [ALPHA] Cumulative number pod admission rejections by the Kubelet.
                # TYPE kubelet_admission_rejections_total counter
                kubelet_admission_rejections_total{reason="NodeShutdown"} 1
            `,
		},
		{
			name:   "OutOfcpu",
			reason: "OutOfcpu",
			wants: `
                # HELP kubelet_admission_rejections_total [ALPHA] Cumulative number pod admission rejections by the Kubelet.
                # TYPE kubelet_admission_rejections_total counter
                kubelet_admission_rejections_total{reason="OutOfcpu"} 1
            `,
		},
		{
			name:   "OutOfmemory",
			reason: "OutOfmemory",
			wants: `
                # HELP kubelet_admission_rejections_total [ALPHA] Cumulative number pod admission rejections by the Kubelet.
                # TYPE kubelet_admission_rejections_total counter
                kubelet_admission_rejections_total{reason="OutOfmemory"} 1
            `,
		},
		{
			name:   "OutOfephemeral-storage",
			reason: "OutOfephemeral-storage",
			wants: `
                # HELP kubelet_admission_rejections_total [ALPHA] Cumulative number pod admission rejections by the Kubelet.
                # TYPE kubelet_admission_rejections_total counter
                kubelet_admission_rejections_total{reason="OutOfephemeral-storage"} 1
            `,
		},
		{
			name:   "OutOfpods",
			reason: "OutOfpods",
			wants: `
                # HELP kubelet_admission_rejections_total [ALPHA] Cumulative number pod admission rejections by the Kubelet.
                # TYPE kubelet_admission_rejections_total counter
                kubelet_admission_rejections_total{reason="OutOfpods"} 1
            `,
		},
		{
			name:   "OutOfgpu",
			reason: "OutOfgpu",
			wants: `
                # HELP kubelet_admission_rejections_total [ALPHA] Cumulative number pod admission rejections by the Kubelet.
                # TYPE kubelet_admission_rejections_total counter
                kubelet_admission_rejections_total{reason="OutOfExtendedResources"} 1
            `,
		},
		{
			name:   "OtherReason",
			reason: "OtherReason",
			wants: `
                # HELP kubelet_admission_rejections_total [ALPHA] Cumulative number pod admission rejections by the Kubelet.
                # TYPE kubelet_admission_rejections_total counter
                kubelet_admission_rejections_total{reason="Other"} 1
            `,
		},
	}

	// Run tests.
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Clear the metrics after the test.
			metrics.AdmissionRejectionsTotal.Reset()

			// Call the function.
			recordAdmissionRejection(tc.reason)

			if err := testutil.GatherAndCompare(metrics.GetGather(), strings.NewReader(tc.wants), "kubelet_admission_rejections_total"); err != nil {
				t.Error(err)
			}
		})
	}
}
