/*
Copyright 2023 The Kubernetes Authors.

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
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"

	v1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/dynamic-resource-allocation/resourceclaim"
	"k8s.io/klog/v2"
	drapb "k8s.io/kubelet/pkg/apis/dra/v1beta1"
	"k8s.io/kubernetes/pkg/kubelet/cm/dra/plugin"
	"k8s.io/kubernetes/pkg/kubelet/cm/dra/state"
	"k8s.io/kubernetes/test/utils/ktesting"
)

const (
	driverClassName = "test"
	podName         = "test-pod"
	containerName   = "test-container"
)

type fakeDRADriverGRPCServer struct {
	drapb.UnimplementedDRAPluginServer
	driverName                 string
	timeout                    *time.Duration
	prepareResourceCalls       atomic.Uint32
	unprepareResourceCalls     atomic.Uint32
	prepareResourcesResponse   *drapb.NodePrepareResourcesResponse
	unprepareResourcesResponse *drapb.NodeUnprepareResourcesResponse
}

func (s *fakeDRADriverGRPCServer) NodePrepareResources(ctx context.Context, req *drapb.NodePrepareResourcesRequest) (*drapb.NodePrepareResourcesResponse, error) {
	s.prepareResourceCalls.Add(1)

	if s.timeout != nil {
		time.Sleep(*s.timeout)
	}

	if s.prepareResourcesResponse == nil {
		cdiDeviceName := "claim-" + req.Claims[0].UID
		cdiID := s.driverName + "/" + driverClassName + "=" + cdiDeviceName
		return &drapb.NodePrepareResourcesResponse{
			Claims: map[string]*drapb.NodePrepareResourceResponse{
				req.Claims[0].UID: {
					Devices: []*drapb.Device{
						{
							PoolName:     poolName,
							DeviceName:   deviceName,
							RequestNames: []string{req.Claims[0].Name},
							CDIDeviceIDs: []string{cdiID},
						},
					},
				},
			},
		}, nil
	}

	return s.prepareResourcesResponse, nil
}

func (s *fakeDRADriverGRPCServer) NodeUnprepareResources(ctx context.Context, req *drapb.NodeUnprepareResourcesRequest) (*drapb.NodeUnprepareResourcesResponse, error) {
	s.unprepareResourceCalls.Add(1)

	if s.timeout != nil {
		time.Sleep(*s.timeout)
	}

	if s.unprepareResourcesResponse == nil {
		return &drapb.NodeUnprepareResourcesResponse{
			Claims: map[string]*drapb.NodeUnprepareResourceResponse{
				req.Claims[0].UID: {},
			},
		}, nil
	}

	return s.unprepareResourcesResponse, nil
}

type tearDown func()

type fakeDRAServerInfo struct {
	// fake DRA server
	server *fakeDRADriverGRPCServer
	// fake DRA plugin socket name
	socketName string
	// teardownFn stops fake gRPC server
	teardownFn tearDown
}

func setupFakeDRADriverGRPCServer(ctx context.Context, shouldTimeout bool, pluginClientTimeout *time.Duration, prepareResourcesResponse *drapb.NodePrepareResourcesResponse, unprepareResourcesResponse *drapb.NodeUnprepareResourcesResponse) (fakeDRAServerInfo, error) {
	socketDir, err := os.MkdirTemp("", "dra")
	if err != nil {
		return fakeDRAServerInfo{
			server:     nil,
			socketName: "",
			teardownFn: nil,
		}, err
	}

	socketName := filepath.Join(socketDir, "server.sock")
	stopCh := make(chan struct{})

	teardown := func() {
		close(stopCh)
		if err := os.Remove(socketName); err != nil {
			logger := klog.FromContext(ctx)
			logger.Error(err, "failed to remove socket file", "path", socketName)
		}
	}

	l, err := net.Listen("unix", socketName)
	if err != nil {
		teardown()
		return fakeDRAServerInfo{
			server:     nil,
			socketName: "",
			teardownFn: nil,
		}, err
	}

	s := grpc.NewServer()
	fakeDRADriverGRPCServer := &fakeDRADriverGRPCServer{
		driverName:                 driverName,
		prepareResourcesResponse:   prepareResourcesResponse,
		unprepareResourcesResponse: unprepareResourcesResponse,
	}
	if shouldTimeout {
		timeout := *pluginClientTimeout * 2
		fakeDRADriverGRPCServer.timeout = &timeout
	}

	drapb.RegisterDRAPluginServer(s, fakeDRADriverGRPCServer)

	go func(ctx context.Context) {
		go func() {
			if err := s.Serve(l); err != nil {
				logger := klog.FromContext(ctx)
				logger.Error(err, "failed to serve gRPC")
			}
		}()
		<-stopCh
		s.GracefulStop()
	}(ctx)

	return fakeDRAServerInfo{
		server:     fakeDRADriverGRPCServer,
		socketName: socketName,
		teardownFn: teardown,
	}, nil
}

func TestNewManagerImpl(t *testing.T) {
	kubeClient := fake.NewSimpleClientset()
	for _, test := range []struct {
		description        string
		stateFileDirectory string
		wantErr            bool
	}{
		{
			description:        "invalid directory path",
			stateFileDirectory: "",
			wantErr:            true,
		},
		{
			description:        "valid directory path",
			stateFileDirectory: t.TempDir(),
		},
	} {
		t.Run(test.description, func(t *testing.T) {
			manager, err := NewManagerImpl(kubeClient, test.stateFileDirectory, "worker")
			if test.wantErr {
				assert.Error(t, err)
				return
			}

			require.NoError(t, err)
			assert.NotNil(t, manager.cache)
			assert.NotNil(t, manager.kubeClient)
		})
	}
}

// genTestPod generates pod object
func genTestPod() *v1.Pod {
	claimName := claimName
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: namespace,
			UID:       podUID,
		},
		Spec: v1.PodSpec{
			ResourceClaims: []v1.PodResourceClaim{
				{
					Name:              claimName,
					ResourceClaimName: &claimName,
				},
			},
			Containers: []v1.Container{
				{
					Resources: v1.ResourceRequirements{
						Claims: []v1.ResourceClaim{
							{
								Name: claimName,
							},
						},
					},
				},
			},
		},
	}
}

// getTestClaim generates resource claim object
func genTestClaim(name, driver, device, podUID string) *resourceapi.ResourceClaim {
	return &resourceapi.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			UID:       types.UID(fmt.Sprintf("%s-uid", name)),
		},
		Spec: resourceapi.ResourceClaimSpec{
			Devices: resourceapi.DeviceClaim{
				Requests: []resourceapi.DeviceRequest{
					{
						Name:            requestName,
						DeviceClassName: className,
					},
				},
			},
		},
		Status: resourceapi.ResourceClaimStatus{
			Allocation: &resourceapi.AllocationResult{
				Devices: resourceapi.DeviceAllocationResult{
					Results: []resourceapi.DeviceRequestAllocationResult{
						{
							Request: requestName,
							Pool:    poolName,
							Device:  device,
							Driver:  driver,
						},
					},
				},
			},
			ReservedFor: []resourceapi.ResourceClaimConsumerReference{
				{UID: types.UID(podUID)},
			},
		},
	}
}

// genTestClaimInfo generates claim info object
func genTestClaimInfo(podUIDs []string, prepared bool) *ClaimInfo {
	return &ClaimInfo{
		ClaimInfoState: state.ClaimInfoState{
			ClaimUID:  claimUID,
			ClaimName: claimName,
			Namespace: namespace,
			PodUIDs:   sets.New[string](podUIDs...),
			DriverState: map[string]state.DriverState{
				driverName: {
					Devices: []state.Device{{
						PoolName:     poolName,
						DeviceName:   deviceName,
						RequestNames: []string{requestName},
						CDIDeviceIDs: []string{cdiID},
					}},
				},
			},
		},
		prepared: prepared,
	}
}

// genClaimInfoState generates claim info state object
func genClaimInfoState(cdiDeviceID string) state.ClaimInfoState {
	s := state.ClaimInfoState{
		ClaimUID:  claimUID,
		ClaimName: claimName,
		Namespace: namespace,
		PodUIDs:   sets.New[string](podUID),
		DriverState: map[string]state.DriverState{
			driverName: {},
		},
	}
	if cdiDeviceID != "" {
		s.DriverState[driverName] = state.DriverState{Devices: []state.Device{{PoolName: poolName, DeviceName: deviceName, RequestNames: []string{requestName}, CDIDeviceIDs: []string{cdiDeviceID}}}}
	}
	return s
}

func TestGetResources(t *testing.T) {
	kubeClient := fake.NewSimpleClientset()

	for _, test := range []struct {
		description string
		container   *v1.Container
		pod         *v1.Pod
		claimInfo   *ClaimInfo
		wantErr     bool
	}{
		{
			description: "claim info with devices",
			container: &v1.Container{
				Name: containerName,
				Resources: v1.ResourceRequirements{
					Claims: []v1.ResourceClaim{
						{
							Name: claimName,
						},
					},
				},
			},
			pod:       genTestPod(),
			claimInfo: genTestClaimInfo(nil, false),
		},
		{
			description: "nil claiminfo",
			container: &v1.Container{
				Name: containerName,
				Resources: v1.ResourceRequirements{
					Claims: []v1.ResourceClaim{
						{
							Name: claimName,
						},
					},
				},
			},
			pod:     genTestPod(),
			wantErr: true,
		},
	} {
		t.Run(test.description, func(t *testing.T) {
			manager, err := NewManagerImpl(kubeClient, t.TempDir(), "worker")
			require.NoError(t, err)

			if test.claimInfo != nil {
				manager.cache.add(test.claimInfo)
			}

			containerInfo, err := manager.GetResources(test.pod, test.container)
			if test.wantErr {
				assert.Error(t, err)
			} else {
				require.NoError(t, err)
				assert.Equal(t, test.claimInfo.DriverState[driverName].Devices[0].CDIDeviceIDs[0], containerInfo.CDIDevices[0].Name)
			}
		})
	}
}

func getFakeNode() (*v1.Node, error) {
	return &v1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker"}}, nil
}

func TestPrepareResources(t *testing.T) {
	claimName := claimName
	fakeKubeClient := fake.NewSimpleClientset()

	for _, test := range []struct {
		description         string
		driverName          string
		pod                 *v1.Pod
		claimInfo           *ClaimInfo
		claim               *resourceapi.ResourceClaim
		resp                *drapb.NodePrepareResourcesResponse
		wantTimeout         bool
		wantResourceSkipped bool

		expectedErrMsg         string
		expectedClaimInfoState state.ClaimInfoState
		expectedPrepareCalls   uint32
	}{
		{
			description:    "claim doesn't exist",
			driverName:     driverName,
			pod:            genTestPod(),
			expectedErrMsg: "failed to fetch ResourceClaim ",
		},
		{
			description:    "unknown driver",
			pod:            genTestPod(),
			claim:          genTestClaim(claimName, "unknown driver", deviceName, podUID),
			expectedErrMsg: "plugin name unknown driver not found in the list of registered DRA plugins",
		},
		{
			description:            "should prepare resources, driver returns nil value",
			driverName:             driverName,
			pod:                    genTestPod(),
			claim:                  genTestClaim(claimName, driverName, deviceName, podUID),
			resp:                   &drapb.NodePrepareResourcesResponse{Claims: map[string]*drapb.NodePrepareResourceResponse{string(claimUID): nil}},
			expectedClaimInfoState: genClaimInfoState(""),
			expectedPrepareCalls:   1,
		},
		{
			description:          "driver returns empty result",
			driverName:           driverName,
			pod:                  genTestPod(),
			claim:                genTestClaim(claimName, driverName, deviceName, podUID),
			resp:                 &drapb.NodePrepareResourcesResponse{Claims: map[string]*drapb.NodePrepareResourceResponse{}},
			expectedPrepareCalls: 1,
			expectedErrMsg:       "NodePrepareResources left out 1 claims",
		},
		{
			description:    "pod is not allowed to use resource claim",
			driverName:     driverName,
			pod:            genTestPod(),
			claim:          genTestClaim(claimName, driverName, deviceName, ""),
			expectedErrMsg: "is not allowed to use resource claim ",
		},
		{
			description: "no container uses the claim",
			driverName:  driverName,
			pod: &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      podName,
					Namespace: namespace,
					UID:       podUID,
				},
				Spec: v1.PodSpec{
					ResourceClaims: []v1.PodResourceClaim{
						{
							Name:              claimName,
							ResourceClaimName: &claimName,
						},
					},
					Containers: []v1.Container{
						{},
					},
				},
			},
			claim:                  genTestClaim(claimName, driverName, deviceName, podUID),
			expectedPrepareCalls:   1,
			expectedClaimInfoState: genClaimInfoState(cdiID),
			resp: &drapb.NodePrepareResourcesResponse{Claims: map[string]*drapb.NodePrepareResourceResponse{
				string(claimUID): {
					Devices: []*drapb.Device{
						{
							PoolName:     poolName,
							DeviceName:   deviceName,
							RequestNames: []string{requestName},
							CDIDeviceIDs: []string{cdiID},
						},
					},
				},
			}},
		},
		{
			description:            "resource already prepared",
			driverName:             driverName,
			pod:                    genTestPod(),
			claim:                  genTestClaim(claimName, driverName, deviceName, podUID),
			claimInfo:              genTestClaimInfo([]string{podUID}, true),
			expectedClaimInfoState: genClaimInfoState(cdiID),
			resp: &drapb.NodePrepareResourcesResponse{Claims: map[string]*drapb.NodePrepareResourceResponse{
				string(claimUID): {
					Devices: []*drapb.Device{
						{
							PoolName:     poolName,
							DeviceName:   deviceName,
							RequestNames: []string{requestName},
							CDIDeviceIDs: []string{cdiID},
						},
					},
				},
			}},
		},
		{
			description:          "should timeout",
			driverName:           driverName,
			pod:                  genTestPod(),
			claim:                genTestClaim(claimName, driverName, deviceName, podUID),
			wantTimeout:          true,
			expectedPrepareCalls: 1,
			expectedErrMsg:       "NodePrepareResources failed: rpc error: code = DeadlineExceeded desc = context deadline exceeded",
		},
		{
			description:            "should prepare resource, claim not in cache",
			driverName:             driverName,
			pod:                    genTestPod(),
			claim:                  genTestClaim(claimName, driverName, deviceName, podUID),
			expectedClaimInfoState: genClaimInfoState(cdiID),
			resp: &drapb.NodePrepareResourcesResponse{Claims: map[string]*drapb.NodePrepareResourceResponse{
				string(claimUID): {
					Devices: []*drapb.Device{
						{
							PoolName:     poolName,
							DeviceName:   deviceName,
							RequestNames: []string{requestName},
							CDIDeviceIDs: []string{cdiID},
						},
					},
				},
			}},
			expectedPrepareCalls: 1,
		},
		{
			description:            "resource already prepared",
			driverName:             driverName,
			pod:                    genTestPod(),
			claim:                  genTestClaim(claimName, driverName, deviceName, podUID),
			claimInfo:              genTestClaimInfo([]string{podUID}, true),
			expectedClaimInfoState: genClaimInfoState(cdiID),
			resp: &drapb.NodePrepareResourcesResponse{Claims: map[string]*drapb.NodePrepareResourceResponse{
				string(claimUID): {
					Devices: []*drapb.Device{
						{
							PoolName:     poolName,
							DeviceName:   deviceName,
							RequestNames: []string{requestName},
							CDIDeviceIDs: []string{cdiID},
						},
					},
				},
			}},
		},
	} {
		t.Run(test.description, func(t *testing.T) {
			tCtx := ktesting.Init(t)
			cache, err := newClaimInfoCache(t.TempDir(), draManagerStateFileName)
			if err != nil {
				t.Fatalf("failed to newClaimInfoCache, err:%v", err)
			}

			manager := &ManagerImpl{
				kubeClient: fakeKubeClient,
				cache:      cache,
			}

			if test.claim != nil {
				if _, err := fakeKubeClient.ResourceV1beta1().ResourceClaims(test.pod.Namespace).Create(tCtx, test.claim, metav1.CreateOptions{}); err != nil {
					t.Fatalf("failed to create ResourceClaim %s: %+v", test.claim.Name, err)
				}
				defer func() {
					require.NoError(t, fakeKubeClient.ResourceV1beta1().ResourceClaims(test.pod.Namespace).Delete(tCtx, test.claim.Name, metav1.DeleteOptions{}))
				}()
			}

			var pluginClientTimeout *time.Duration
			if test.wantTimeout {
				timeout := time.Millisecond * 20
				pluginClientTimeout = &timeout
			}

			draServerInfo, err := setupFakeDRADriverGRPCServer(tCtx, test.wantTimeout, pluginClientTimeout, test.resp, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer draServerInfo.teardownFn()

			plg := plugin.NewRegistrationHandler(nil, getFakeNode, time.Second /* very short wiping delay for testing */)
			if err := plg.RegisterPlugin(test.driverName, draServerInfo.socketName, []string{drapb.DRAPluginService}, pluginClientTimeout); err != nil {
				t.Fatalf("failed to register plugin %s, err: %v", test.driverName, err)
			}
			defer plg.DeRegisterPlugin(test.driverName, draServerInfo.socketName) // for sake of next tests

			if test.claimInfo != nil {
				manager.cache.add(test.claimInfo)
			}

			err = manager.PrepareResources(tCtx, test.pod)

			assert.Equal(t, test.expectedPrepareCalls, draServerInfo.server.prepareResourceCalls.Load())

			if test.expectedErrMsg != "" {
				assert.Error(t, err)
				if err != nil {
					assert.ErrorContains(t, err, test.expectedErrMsg)
				}
				return // PrepareResources returned an error so stopping the test case here
			}

			require.NoError(t, err)

			if test.wantResourceSkipped {
				return // resource skipped so no need to continue
			}

			// check the cache contains the expected claim info
			claimName, _, err := resourceclaim.Name(test.pod, &test.pod.Spec.ResourceClaims[0])
			if err != nil {
				t.Fatal(err)
			}
			claimInfo, ok := manager.cache.get(*claimName, test.pod.Namespace)
			if !ok {
				t.Fatalf("claimInfo not found in cache for claim %s", *claimName)
			}
			if len(claimInfo.PodUIDs) != 1 || !claimInfo.PodUIDs.Has(string(test.pod.UID)) {
				t.Fatalf("podUIDs mismatch: expected [%s], got %v", test.pod.UID, claimInfo.PodUIDs)
			}

			assert.Equal(t, test.expectedClaimInfoState, claimInfo.ClaimInfoState)
		})
	}
}

func TestUnprepareResources(t *testing.T) {
	fakeKubeClient := fake.NewSimpleClientset()
	for _, test := range []struct {
		description         string
		driverName          string
		pod                 *v1.Pod
		claimInfo           *ClaimInfo
		claim               *resourceapi.ResourceClaim
		resp                *drapb.NodeUnprepareResourcesResponse
		wantTimeout         bool
		wantResourceSkipped bool

		expectedUnprepareCalls uint32
		expectedErrMsg         string
	}{
		{
			description:    "unknown driver",
			pod:            genTestPod(),
			claim:          genTestClaim(claimName, "unknown driver", deviceName, podUID),
			claimInfo:      genTestClaimInfo([]string{podUID}, true),
			expectedErrMsg: "plugin name test-driver not found in the list of registered DRA plugins",
		},
		{
			description:         "resource claim referenced by other pod(s)",
			driverName:          driverName,
			pod:                 genTestPod(),
			claimInfo:           genTestClaimInfo([]string{podUID, "another-pod-uid"}, true),
			wantResourceSkipped: true,
		},
		{
			description:            "should timeout",
			driverName:             driverName,
			pod:                    genTestPod(),
			claimInfo:              genTestClaimInfo([]string{podUID}, true),
			wantTimeout:            true,
			expectedUnprepareCalls: 1,
			expectedErrMsg:         "context deadline exceeded",
		},
		{
			description:            "should fail when driver returns empty response",
			driverName:             driverName,
			pod:                    genTestPod(),
			claimInfo:              genTestClaimInfo([]string{podUID}, true),
			resp:                   &drapb.NodeUnprepareResourcesResponse{Claims: map[string]*drapb.NodeUnprepareResourceResponse{}},
			expectedUnprepareCalls: 1,
			expectedErrMsg:         "NodeUnprepareResources left out 1 claims",
		},
		{
			description:            "should unprepare resource",
			driverName:             driverName,
			pod:                    genTestPod(),
			claim:                  genTestClaim(claimName, driverName, deviceName, podUID),
			claimInfo:              genTestClaimInfo([]string{podUID}, false),
			expectedUnprepareCalls: 1,
		},
		{
			description:            "should unprepare already prepared resource",
			driverName:             driverName,
			pod:                    genTestPod(),
			claim:                  genTestClaim(claimName, driverName, deviceName, podUID),
			claimInfo:              genTestClaimInfo([]string{podUID}, true),
			expectedUnprepareCalls: 1,
		},
		{
			description:            "should unprepare resource when driver returns nil value",
			driverName:             driverName,
			pod:                    genTestPod(),
			claimInfo:              genTestClaimInfo([]string{podUID}, true),
			resp:                   &drapb.NodeUnprepareResourcesResponse{Claims: map[string]*drapb.NodeUnprepareResourceResponse{string(claimUID): nil}},
			expectedUnprepareCalls: 1,
		},
	} {
		t.Run(test.description, func(t *testing.T) {
			tCtx := ktesting.Init(t)
			cache, err := newClaimInfoCache(t.TempDir(), draManagerStateFileName)
			if err != nil {
				t.Fatalf("failed to create a new instance of the claimInfoCache, err: %v", err)
			}

			var pluginClientTimeout *time.Duration
			if test.wantTimeout {
				timeout := time.Millisecond * 20
				pluginClientTimeout = &timeout
			}

			draServerInfo, err := setupFakeDRADriverGRPCServer(tCtx, test.wantTimeout, pluginClientTimeout, nil, test.resp)
			if err != nil {
				t.Fatal(err)
			}
			defer draServerInfo.teardownFn()

			plg := plugin.NewRegistrationHandler(nil, getFakeNode, time.Second /* very short wiping delay for testing */)
			if err := plg.RegisterPlugin(test.driverName, draServerInfo.socketName, []string{drapb.DRAPluginService}, pluginClientTimeout); err != nil {
				t.Fatalf("failed to register plugin %s, err: %v", test.driverName, err)
			}
			defer plg.DeRegisterPlugin(test.driverName, draServerInfo.socketName) // for sake of next tests

			manager := &ManagerImpl{
				kubeClient: fakeKubeClient,
				cache:      cache,
			}

			if test.claimInfo != nil {
				manager.cache.add(test.claimInfo)
			}

			err = manager.UnprepareResources(tCtx, test.pod)

			assert.Equal(t, test.expectedUnprepareCalls, draServerInfo.server.unprepareResourceCalls.Load())

			if test.expectedErrMsg != "" {
				assert.Error(t, err)
				if err != nil {
					assert.ErrorContains(t, err, test.expectedErrMsg)
				}
				return // PrepareResources returned an error so stopping the test case here
			}

			require.NoError(t, err)

			if test.wantResourceSkipped {
				return // resource skipped so no need to continue
			}

			// Check that the cache has been updated correctly
			claimName, _, err := resourceclaim.Name(test.pod, &test.pod.Spec.ResourceClaims[0])
			if err != nil {
				t.Fatal(err)
			}
			if manager.cache.contains(*claimName, test.pod.Namespace) {
				t.Fatalf("claimInfo still found in cache after calling UnprepareResources")
			}
		})
	}
}

func TestPodMightNeedToUnprepareResources(t *testing.T) {
	fakeKubeClient := fake.NewSimpleClientset()

	cache, err := newClaimInfoCache(t.TempDir(), draManagerStateFileName)
	if err != nil {
		t.Fatalf("failed to newClaimInfoCache, err:%v", err)
	}

	manager := &ManagerImpl{
		kubeClient: fakeKubeClient,
		cache:      cache,
	}

	claimInfo := &ClaimInfo{
		ClaimInfoState: state.ClaimInfoState{PodUIDs: sets.New(podUID), ClaimName: claimName, Namespace: namespace},
	}
	manager.cache.add(claimInfo)
	if !manager.cache.contains(claimName, namespace) {
		t.Fatalf("failed to get claimInfo from cache for claim name %s, namespace %s: err:%v", claimName, namespace, err)
	}
	claimInfo.addPodReference(types.UID(podUID))
	needsUnprepare := manager.PodMightNeedToUnprepareResources(types.UID(podUID))
	assert.True(t, needsUnprepare)
}

func TestGetContainerClaimInfos(t *testing.T) {
	for _, test := range []struct {
		description string
		pod         *v1.Pod
		claimInfo   *ClaimInfo

		expectedClaimName string
		expectedErrMsg    string
	}{
		{
			description:       "should get claim info",
			expectedClaimName: claimName,
			pod:               genTestPod(),
			claimInfo:         genTestClaimInfo([]string{podUID}, false),
		},
		{
			description:    "should fail when claim info not found",
			pod:            genTestPod(),
			claimInfo:      &ClaimInfo{},
			expectedErrMsg: "unable to get claim info for claim ",
		},
		{
			description: "should fail when none of the supported fields are set",
			pod: &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      podName,
					Namespace: namespace,
					UID:       podUID,
				},
				Spec: v1.PodSpec{
					ResourceClaims: []v1.PodResourceClaim{
						{
							Name: claimName,
							// missing ResourceClaimName or ResourceClaimTemplateName
						},
					},
					Containers: []v1.Container{
						{
							Resources: v1.ResourceRequirements{
								Claims: []v1.ResourceClaim{
									{
										Name: claimName,
									},
								},
							},
						},
					},
				},
			},
			claimInfo:      genTestClaimInfo([]string{podUID}, false),
			expectedErrMsg: "none of the supported fields are set",
		},
		{
			description:    "should fail when claim info is not cached",
			pod:            genTestPod(),
			expectedErrMsg: "unable to get claim info for claim ",
		},
	} {
		t.Run(test.description, func(t *testing.T) {
			cache, err := newClaimInfoCache(t.TempDir(), draManagerStateFileName)
			if err != nil {
				t.Fatalf("error occur:%v", err)
			}
			manager := &ManagerImpl{
				cache: cache,
			}

			if test.claimInfo != nil {
				manager.cache.add(test.claimInfo)
			}

			claimInfos, err := manager.GetContainerClaimInfos(test.pod, &test.pod.Spec.Containers[0])

			if test.expectedErrMsg != "" {
				assert.Error(t, err)
				if err != nil {
					assert.ErrorContains(t, err, test.expectedErrMsg)
				}
				return
			}

			require.NoError(t, err)
			assert.Len(t, claimInfos, 1)
			assert.Equal(t, test.expectedClaimName, claimInfos[0].ClaimInfoState.ClaimName)
		})
	}
}

// TestParallelPrepareUnprepareResources calls PrepareResources and UnprepareResources APIs in parallel
// to detect possible data races
func TestParallelPrepareUnprepareResources(t *testing.T) {
	tCtx := ktesting.Init(t)

	// Setup and register fake DRA driver
	draServerInfo, err := setupFakeDRADriverGRPCServer(tCtx, false, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer draServerInfo.teardownFn()

	plg := plugin.NewRegistrationHandler(nil, getFakeNode, time.Second /* very short wiping delay for testing */)
	if err := plg.RegisterPlugin(driverName, draServerInfo.socketName, []string{drapb.DRAPluginService}, nil); err != nil {
		t.Fatalf("failed to register plugin %s, err: %v", driverName, err)
	}
	defer plg.DeRegisterPlugin(driverName, draServerInfo.socketName)

	// Create ClaimInfo cache
	cache, err := newClaimInfoCache(t.TempDir(), draManagerStateFileName)
	if err != nil {
		t.Errorf("failed to newClaimInfoCache, err: %+v", err)
		return
	}

	// Create fake Kube client and DRA manager
	fakeKubeClient := fake.NewSimpleClientset()
	manager := &ManagerImpl{kubeClient: fakeKubeClient, cache: cache}

	// Call PrepareResources in parallel
	var wgSync, wgStart sync.WaitGroup // groups to sync goroutines
	numGoroutines := 30
	wgSync.Add(numGoroutines)
	wgStart.Add(1)
	for i := 0; i < numGoroutines; i++ {
		go func(t *testing.T, goRoutineNum int) {
			defer wgSync.Done()
			wgStart.Wait() // Wait to start all goroutines at the same time

			var err error
			claimName := fmt.Sprintf("test-pod-claim-%d", goRoutineNum)
			podUID := types.UID(fmt.Sprintf("test-pod-uid-%d", goRoutineNum))
			pod := &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("test-pod-%d", goRoutineNum),
					Namespace: namespace,
					UID:       podUID,
				},
				Spec: v1.PodSpec{
					ResourceClaims: []v1.PodResourceClaim{
						{
							Name: claimName,
							ResourceClaimName: func() *string {
								s := claimName
								return &s
							}(),
						},
					},
					Containers: []v1.Container{
						{
							Resources: v1.ResourceRequirements{
								Claims: []v1.ResourceClaim{
									{
										Name: claimName,
									},
								},
							},
						},
					},
				},
			}
			claim := genTestClaim(claimName, driverName, deviceName, string(podUID))

			if _, err = fakeKubeClient.ResourceV1beta1().ResourceClaims(pod.Namespace).Create(tCtx, claim, metav1.CreateOptions{}); err != nil {
				t.Errorf("failed to create ResourceClaim %s: %+v", claim.Name, err)
				return
			}

			if err = manager.PrepareResources(tCtx, pod); err != nil {
				t.Errorf("pod: %s: PrepareResources failed: %+v", pod.Name, err)
				return
			}

			if err = manager.UnprepareResources(tCtx, pod); err != nil {
				t.Errorf("pod: %s: UnprepareResources failed: %+v", pod.Name, err)
				return
			}

		}(t, i)
	}
	wgStart.Done() // Start executing goroutines
	wgSync.Wait()  // Wait for all goroutines to finish
}
