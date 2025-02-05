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

package kuberuntime

import (
	"context"
	"net/http"
	"time"

	cadvisorapi "github.com/google/cadvisor/info/v1"
	"go.opentelemetry.io/otel/trace"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/flowcontrol"
	"k8s.io/component-base/logs/logreduction"
	internalapi "k8s.io/cri-api/pkg/apis"
	"k8s.io/kubernetes/pkg/credentialprovider"
	"k8s.io/kubernetes/pkg/kubelet/cm"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
	"k8s.io/kubernetes/pkg/kubelet/images"
	"k8s.io/kubernetes/pkg/kubelet/lifecycle"
	"k8s.io/kubernetes/pkg/kubelet/logs"
	proberesults "k8s.io/kubernetes/pkg/kubelet/prober/results"
	"k8s.io/utils/ptr"
)

const (
	fakeSeccompProfileRoot = "/fakeSeccompProfileRoot"

	fakeNodeAllocatableMemory = "32Gi"
	fakeNodeAllocatableCPU    = "16"

	fakePodLogsDirectory = "/var/log/pods"
)

type fakeHTTP struct {
	req *http.Request
	err error
}

func (f *fakeHTTP) Do(req *http.Request) (*http.Response, error) {
	f.req = req
	return nil, f.err
}

type fakePodStateProvider struct {
	terminated map[types.UID]struct{}
	removed    map[types.UID]struct{}
}

func newFakePodStateProvider() *fakePodStateProvider {
	return &fakePodStateProvider{
		terminated: make(map[types.UID]struct{}),
		removed:    make(map[types.UID]struct{}),
	}
}

func (f *fakePodStateProvider) IsPodTerminationRequested(uid types.UID) bool {
	_, found := f.removed[uid]
	return found
}

func (f *fakePodStateProvider) ShouldPodRuntimeBeRemoved(uid types.UID) bool {
	_, found := f.terminated[uid]
	return found
}

func (f *fakePodStateProvider) ShouldPodContentBeRemoved(uid types.UID) bool {
	_, found := f.removed[uid]
	return found
}

type fakePodPullingTimeRecorder struct{}

func (f *fakePodPullingTimeRecorder) RecordImageStartedPulling(podUID types.UID) {}

func (f *fakePodPullingTimeRecorder) RecordImageFinishedPulling(podUID types.UID) {}

func newFakeKubeRuntimeManager(runtimeService internalapi.RuntimeService, imageService internalapi.ImageManagerService, machineInfo *cadvisorapi.MachineInfo, osInterface kubecontainer.OSInterface, runtimeHelper kubecontainer.RuntimeHelper, keyring credentialprovider.DockerKeyring, tracer trace.Tracer) (*kubeGenericRuntimeManager, error) {
	ctx := context.Background()
	recorder := &record.FakeRecorder{}
	logManager, err := logs.NewContainerLogManager(runtimeService, osInterface, "1", 2, 10, metav1.Duration{Duration: 10 * time.Second})
	if err != nil {
		return nil, err
	}
	kubeRuntimeManager := &kubeGenericRuntimeManager{
		recorder:               recorder,
		cpuCFSQuota:            false,
		cpuCFSQuotaPeriod:      metav1.Duration{Duration: time.Millisecond * 100},
		livenessManager:        proberesults.NewManager(),
		startupManager:         proberesults.NewManager(),
		machineInfo:            machineInfo,
		osInterface:            osInterface,
		containerManager:       cm.NewFakeContainerManager(),
		runtimeHelper:          runtimeHelper,
		runtimeService:         runtimeService,
		imageService:           imageService,
		keyring:                keyring,
		seccompProfileRoot:     fakeSeccompProfileRoot,
		internalLifecycle:      cm.NewFakeInternalContainerLifecycle(),
		logReduction:           logreduction.NewLogReduction(identicalErrorDelay),
		logManager:             logManager,
		memoryThrottlingFactor: 0.9,
		podLogsDirectory:       fakePodLogsDirectory,
	}

	typedVersion, err := runtimeService.Version(ctx, kubeRuntimeAPIVersion)
	if err != nil {
		return nil, err
	}

	podStateProvider := newFakePodStateProvider()
	kubeRuntimeManager.containerGC = newContainerGC(runtimeService, podStateProvider, kubeRuntimeManager, tracer)
	kubeRuntimeManager.podStateProvider = podStateProvider
	kubeRuntimeManager.runtimeName = typedVersion.RuntimeName
	kubeRuntimeManager.imagePuller = images.NewImageManager(
		kubecontainer.FilterEventRecorder(recorder),
		kubeRuntimeManager,
		flowcontrol.NewBackOff(time.Second, 300*time.Second),
		false,
		ptr.To[int32](0), // No limit on max parallel image pulls,
		0,                // Disable image pull throttling by setting QPS to 0,
		0,
		&fakePodPullingTimeRecorder{},
	)
	kubeRuntimeManager.runner = lifecycle.NewHandlerRunner(
		&fakeHTTP{},
		kubeRuntimeManager,
		kubeRuntimeManager,
		recorder)

	kubeRuntimeManager.getNodeAllocatable = func() v1.ResourceList {
		return v1.ResourceList{
			v1.ResourceMemory: resource.MustParse(fakeNodeAllocatableMemory),
			v1.ResourceCPU:    resource.MustParse(fakeNodeAllocatableCPU),
		}
	}

	return kubeRuntimeManager, nil
}
