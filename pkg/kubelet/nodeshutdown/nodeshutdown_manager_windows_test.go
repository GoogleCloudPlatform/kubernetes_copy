//go:build windows
// +build windows

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

package nodeshutdown

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/client-go/tools/record"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	"k8s.io/klog/v2/ktesting"
	_ "k8s.io/klog/v2/ktesting/init" // activate ktesting command line flags
	pkgfeatures "k8s.io/kubernetes/pkg/features"
	kubeletconfig "k8s.io/kubernetes/pkg/kubelet/apis/config"
	"k8s.io/kubernetes/pkg/kubelet/eviction"
	"k8s.io/kubernetes/pkg/kubelet/prober"
	probetest "k8s.io/kubernetes/pkg/kubelet/prober/testing"
	"k8s.io/kubernetes/pkg/kubelet/volumemanager"
	"k8s.io/utils/clock"
	testingclock "k8s.io/utils/clock/testing"
)

func TestFeatureEnabled(t *testing.T) {
	var tests = []struct {
		desc                         string
		shutdownGracePeriodRequested time.Duration
		featureGateEnabled           bool
		expectEnabled                bool
	}{
		{
			desc:                         "shutdownGracePeriodRequested 0; disables feature",
			shutdownGracePeriodRequested: time.Duration(0 * time.Second),
			featureGateEnabled:           true,
			expectEnabled:                false,
		},
		{
			desc:                         "feature gate disabled; disables feature",
			shutdownGracePeriodRequested: time.Duration(100 * time.Second),
			featureGateEnabled:           false,
			expectEnabled:                false,
		},
		{
			desc:                         "feature gate enabled; shutdownGracePeriodRequested > 0; enables feature",
			shutdownGracePeriodRequested: time.Duration(100 * time.Second),
			featureGateEnabled:           true,
			expectEnabled:                true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			logger, _ := ktesting.NewTestContext(t)
			activePodsFunc := func() []*v1.Pod {
				return nil
			}
			killPodsFunc := func(pod *v1.Pod, evict bool, gracePeriodOverride *int64, fn func(*v1.PodStatus)) error {
				return nil
			}
			featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, pkgfeatures.WindowsGracefulNodeShutdown, tc.featureGateEnabled)

			proberManager := probetest.FakeManager{}
			fakeRecorder := &record.FakeRecorder{}
			fakeVolumeManager := volumemanager.NewFakeVolumeManager([]v1.UniqueVolumeName{}, 0, nil)
			nodeRef := &v1.ObjectReference{Kind: "Node", Name: "test", UID: types.UID("test"), Namespace: ""}

			manager := NewManager(&Config{
				Logger:                          logger,
				ProbeManager:                    proberManager,
				VolumeManager:                   fakeVolumeManager,
				Recorder:                        fakeRecorder,
				NodeRef:                         nodeRef,
				GetPodsFunc:                     activePodsFunc,
				KillPodFunc:                     killPodsFunc,
				SyncNodeStatusFunc:              func(ctx context.Context) {},
				ShutdownGracePeriodRequested:    tc.shutdownGracePeriodRequested,
				ShutdownGracePeriodCriticalPods: 0,
				StateDirectory:                  os.TempDir(),
			})
			assert.Equal(t, tc.expectEnabled, manager != managerStub{})
		})
	}
}

func Test_managerImpl_ProcessShutdownEvent(t *testing.T) {
	var (
		probeManager      = probetest.FakeManager{}
		fakeRecorder      = &record.FakeRecorder{}
		fakeVolumeManager = volumemanager.NewFakeVolumeManager([]v1.UniqueVolumeName{}, 0, nil)
		syncNodeStatus    = func(ctx context.Context) {}
		nodeRef           = &v1.ObjectReference{Kind: "Node", Name: "test", UID: types.UID("test"), Namespace: ""}
		fakeclock         = testingclock.NewFakeClock(time.Now())
	)

	type fields struct {
		recorder                         record.EventRecorder
		nodeRef                          *v1.ObjectReference
		probeManager                     prober.Manager
		volumeManager                    volumemanager.VolumeManager
		shutdownGracePeriodByPodPriority []kubeletconfig.ShutdownGracePeriodByPodPriority
		getPods                          eviction.ActivePodsFunc
		killPodFunc                      eviction.KillPodFunc
		syncNodeStatus                   func(ctx context.Context)
		nodeShuttingDownNow              bool
		clock                            clock.Clock
	}
	tests := []struct {
		name                      string
		fields                    fields
		wantErr                   bool
		expectedOutputContains    string
		expectedOutputNotContains string
	}{
		{
			name: "kill pod func finished in time",
			fields: fields{
				recorder:      fakeRecorder,
				nodeRef:       nodeRef,
				probeManager:  probeManager,
				volumeManager: fakeVolumeManager,
				shutdownGracePeriodByPodPriority: []kubeletconfig.ShutdownGracePeriodByPodPriority{
					{
						Priority:                   1,
						ShutdownGracePeriodSeconds: 10,
					},
					{
						Priority:                   2,
						ShutdownGracePeriodSeconds: 20,
					},
				},
				getPods: func() []*v1.Pod {
					return []*v1.Pod{
						makePod("normal-pod", 1, nil),
						makePod("critical-pod", 2, nil),
					}
				},
				killPodFunc: func(pod *v1.Pod, isEvicted bool, gracePeriodOverride *int64, fn func(*v1.PodStatus)) error {
					fakeclock.Step(5 * time.Second)
					return nil
				},
				syncNodeStatus: syncNodeStatus,
				clock:          fakeclock,
			},
			wantErr:                   false,
			expectedOutputNotContains: "Shutdown manager pod killing time out",
		},
		{
			name: "kill pod func take too long",
			fields: fields{
				recorder:      fakeRecorder,
				nodeRef:       nodeRef,
				probeManager:  probeManager,
				volumeManager: fakeVolumeManager,
				shutdownGracePeriodByPodPriority: []kubeletconfig.ShutdownGracePeriodByPodPriority{
					{
						Priority:                   1,
						ShutdownGracePeriodSeconds: 10,
					},
					{
						Priority:                   2,
						ShutdownGracePeriodSeconds: 20,
					},
				},
				getPods: func() []*v1.Pod {
					return []*v1.Pod{
						makePod("normal-pod", 1, nil),
						makePod("critical-pod", 2, nil),
					}
				},
				killPodFunc: func(pod *v1.Pod, isEvicted bool, gracePeriodOverride *int64, fn func(*v1.PodStatus)) error {
					fakeclock.Step(60 * time.Second)
					return nil
				},
				syncNodeStatus: syncNodeStatus,
				clock:          fakeclock,
			},
			wantErr:                false,
			expectedOutputContains: "Shutdown manager pod killing time out",
		},
		{
			name: "volumeManager failed timed out",
			fields: fields{
				recorder:     fakeRecorder,
				nodeRef:      nodeRef,
				probeManager: probeManager,
				volumeManager: volumemanager.NewFakeVolumeManager(
					[]v1.UniqueVolumeName{},
					3*time.Second, // This value is intentionally longer than the shutdownGracePeriodSeconds (2s) to test the behavior
					// for volume unmount operations that take longer than the allowed grace period.
					fmt.Errorf("unmount timeout"),
				),
				shutdownGracePeriodByPodPriority: []kubeletconfig.ShutdownGracePeriodByPodPriority{
					{
						Priority:                   1,
						ShutdownGracePeriodSeconds: 2,
					},
				},
				getPods: func() []*v1.Pod {
					return []*v1.Pod{
						makePod("normal-pod", 1, nil),
						makePod("critical-pod", 2, nil),
					}
				},
				killPodFunc: func(pod *v1.Pod, isEvicted bool, gracePeriodOverride *int64, fn func(*v1.PodStatus)) error {
					return nil
				},
				syncNodeStatus: syncNodeStatus,
				clock:          fakeclock,
			},
			wantErr:                false,
			expectedOutputContains: "Failed while waiting for all the volumes belonging to Pods in this group to unmount",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logger := ktesting.NewLogger(t,
				ktesting.NewConfig(
					ktesting.BufferLogs(true),
				),
			)
			m := &managerImpl{
				logger:                logger,
				recorder:              tt.fields.recorder,
				nodeRef:               tt.fields.nodeRef,
				probeManager:          tt.fields.probeManager,
				getPods:               tt.fields.getPods,
				syncNodeStatus:        tt.fields.syncNodeStatus,
				nodeShuttingDownMutex: sync.Mutex{},
				nodeShuttingDownNow:   tt.fields.nodeShuttingDownNow,
				podManager: &podManager{
					logger:                           logger,
					volumeManager:                    tt.fields.volumeManager,
					shutdownGracePeriodByPodPriority: tt.fields.shutdownGracePeriodByPodPriority,
					killPodFunc:                      tt.fields.killPodFunc,
					clock:                            tt.fields.clock,
				},
			}
			if err := m.ProcessShutdownEvent(); (err != nil) != tt.wantErr {
				t.Errorf("managerImpl.processShutdownEvent() error = %v, wantErr %v", err, tt.wantErr)
			}

			underlier, ok := logger.GetSink().(ktesting.Underlier)
			if !ok {
				t.Fatalf("Should have had a ktesting LogSink, got %T", logger.GetSink())
			}

			log := underlier.GetBuffer().String()
			if tt.expectedOutputContains != "" && !strings.Contains(log, tt.expectedOutputContains) {
				// Log will be shown on failure. To see it
				// during a successful run use "go test -v".
				t.Errorf("managerImpl.processShutdownEvent() should have logged %s, see actual output above.", tt.expectedOutputContains)
			}

			if tt.expectedOutputNotContains != "" && strings.Contains(log, tt.expectedOutputNotContains) {
				// Log will be shown on failure. To see it
				// during a successful run use "go test -v".
				t.Errorf("managerImpl.processShutdownEvent() should have not logged %s, see actual output above.", tt.expectedOutputNotContains)
			}
		})
	}
}

func Test_addToExistingOrder(t *testing.T) {
	var tests = []struct {
		desc          string
		dependencies  []string
		existingOrder []string
		expectedOrder []string
	}{
		{
			desc:          "dependencies and existingOrder are empty, expectedOrder to be empty",
			dependencies:  []string{},
			existingOrder: []string{},
			expectedOrder: []string{},
		},
		{
			desc:          "dependencies are empty, expectedOrder to be the same as existingOrder",
			dependencies:  []string{},
			existingOrder: []string{"kubelet", "a", "b", "c"},
			expectedOrder: []string{"kubelet", "a", "b", "c"},
		},
		{
			desc:          "existingOrder is empty, expectedOrder has the content of 'kubelet' and dependencies",
			dependencies:  []string{"a", "b", "c"},
			existingOrder: []string{},
			expectedOrder: []string{"kubelet", "a", "b", "c"},
		},
		{
			desc:          "dependencies and existingOrder have no overlap, expectedOrder having the 'kubelet' and dependencies to the end of the existingOrder",
			dependencies:  []string{"a", "b", "c"},
			existingOrder: []string{"d", "e", "f"},
			expectedOrder: []string{"d", "e", "f", "kubelet", "a", "b", "c"},
		},
		{
			desc:          "dependencies and existingOrder have overlaps, expectedOrder having the 'kubelet' and dependencies and hornor the order in existingorder",
			dependencies:  []string{"a", "b", "c"},
			existingOrder: []string{"d", "b", "a", "f"},
			expectedOrder: []string{"d", "kubelet", "b", "a", "f", "c"},
		},
		{
			desc:          "existingOrder has 'kubelet', expectedOrder move the kubelet to the correct order",
			dependencies:  []string{"a", "b", "c"},
			existingOrder: []string{"d", "b", "kubelet", "a", "f"},
			expectedOrder: []string{"d", "kubelet", "b", "a", "f", "c"},
		},
		{
			desc:          "existingOrder has been in the correct order, expectedOrder keep the order",
			dependencies:  []string{"a", "b", "c"},
			existingOrder: []string{"d", "f", "kubelet", "a", "b", "c"},
			expectedOrder: []string{"d", "f", "kubelet", "a", "b", "c"},
		},
		// The following two should not happen in practice, but we should handle it gracefully
		{
			desc:          "dependencies has redundant string, expectedOrder remove the redundant string",
			dependencies:  []string{"a", "b", "b", "c"},
			existingOrder: []string{"d", "b", "a", "f"},
			expectedOrder: []string{"d", "kubelet", "b", "a", "f", "c"},
		},
		{
			desc:          "existingOrder has redundant string, expectedOrder remove the redundant string",
			dependencies:  []string{"a", "b", "c"},
			existingOrder: []string{"d", "b", "a", "f", "b"},
			expectedOrder: []string{"d", "kubelet", "b", "a", "f", "b", "c"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			order := addToExistingOrder(tc.dependencies, tc.existingOrder)

			assert.Equal(t, order, tc.expectedOrder)
		})
	}
}
