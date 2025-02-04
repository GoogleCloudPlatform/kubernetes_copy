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

package logs

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/kubernetes/pkg/kubelet/container"

	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
	critest "k8s.io/cri-api/pkg/apis/testing"
	testingclock "k8s.io/utils/clock/testing"
)

func TestGetAllLogs(t *testing.T) {
	dir, err := os.MkdirTemp("", "test-get-all-logs")
	require.NoError(t, err)
	defer os.RemoveAll(dir)
	testLogs := []string{
		"test-log.11111111-111111.gz",
		"test-log",
		"test-log.00000000-000000.gz",
		"test-log.19900322-000000.gz",
		"test-log.19900322-111111.gz",
		"test-log.19880620-000000", // unused log
		"test-log.19880620-000000.gz",
		"test-log.19880620-111111.gz",
		"test-log.20180101-000000",
		"test-log.20180101-000000.tmp", // temporary log
	}
	expectLogs := []string{
		"test-log.00000000-000000.gz",
		"test-log.11111111-111111.gz",
		"test-log.19880620-000000.gz",
		"test-log.19880620-111111.gz",
		"test-log.19900322-000000.gz",
		"test-log.19900322-111111.gz",
		"test-log.20180101-000000",
		"test-log",
	}
	for i := range testLogs {
		f, err := os.Create(filepath.Join(dir, testLogs[i]))
		require.NoError(t, err)
		f.Close()
	}
	got, err := GetAllLogs(filepath.Join(dir, "test-log"))
	assert.NoError(t, err)
	for i := range expectLogs {
		expectLogs[i] = filepath.Join(dir, expectLogs[i])
	}
	assert.Equal(t, expectLogs, got)
}

func TestRotateLogs(t *testing.T) {
	ctx := context.Background()
	dir, err := os.MkdirTemp("", "test-rotate-logs")
	require.NoError(t, err)
	defer os.RemoveAll(dir)

	const (
		testMaxFiles = 3
		testMaxSize  = 10
	)
	now := time.Now()
	f := critest.NewFakeRuntimeService()
	c := &containerLogManager{
		runtimeService: f,
		policy: LogRotatePolicy{
			MaxSize:  testMaxSize,
			MaxFiles: testMaxFiles,
		},
		osInterface: container.RealOS{},
		clock:       testingclock.NewFakeClock(now),
		mutex:       sync.Mutex{},
		queue: workqueue.NewTypedRateLimitingQueueWithConfig(
			workqueue.DefaultTypedControllerRateLimiter[string](),
			workqueue.TypedRateLimitingQueueConfig[string]{Name: "kubelet_log_rotate_manager"},
		),
		maxWorkers:       10,
		monitoringPeriod: v1.Duration{Duration: 10 * time.Second},
	}
	testLogs := []string{
		"test-log-1",
		"test-log-2",
		"test-log-3",
		"test-log-4",
		"test-log-3.00000000-000001",
		"test-log-3.00000000-000000.gz",
	}
	testContent := []string{
		"short",
		"longer than 10 bytes",
		"longer than 10 bytes",
		"longer than 10 bytes",
		"the length doesn't matter",
		"the length doesn't matter",
	}
	for i := range testLogs {
		f, err := os.Create(filepath.Join(dir, testLogs[i]))
		require.NoError(t, err)
		_, err = f.Write([]byte(testContent[i]))
		require.NoError(t, err)
		f.Close()
	}
	testContainers := []*critest.FakeContainer{
		{
			ContainerStatus: runtimeapi.ContainerStatus{
				Id:      "container-not-need-rotate",
				State:   runtimeapi.ContainerState_CONTAINER_RUNNING,
				LogPath: filepath.Join(dir, testLogs[0]),
			},
		},
		{
			ContainerStatus: runtimeapi.ContainerStatus{
				Id:      "container-need-rotate",
				State:   runtimeapi.ContainerState_CONTAINER_RUNNING,
				LogPath: filepath.Join(dir, testLogs[1]),
			},
		},
		{
			ContainerStatus: runtimeapi.ContainerStatus{
				Id:      "container-has-excess-log",
				State:   runtimeapi.ContainerState_CONTAINER_RUNNING,
				LogPath: filepath.Join(dir, testLogs[2]),
			},
		},
		{
			ContainerStatus: runtimeapi.ContainerStatus{
				Id:      "container-is-not-running",
				State:   runtimeapi.ContainerState_CONTAINER_EXITED,
				LogPath: filepath.Join(dir, testLogs[3]),
			},
		},
	}
	f.SetFakeContainers(testContainers)

	// Push the items into the queue for before starting the worker to avoid issue with the queue being empty.
	require.NoError(t, c.rotateLogs(ctx))

	// Start a routine that can monitor the queue and shutdown the queue to trigger the return from the processQueueItems
	// Keeping the monitor duration smaller in order to keep the unwanted delay in the test to a minimal.
	go func() {
		pollTimeoutCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		err = wait.PollUntilContextCancel(pollTimeoutCtx, 5*time.Millisecond, false, func(ctx context.Context) (done bool, err error) {
			return c.queue.Len() == 0, nil
		})
		require.NoError(t, err)
		c.queue.ShutDown()
	}()
	// This is a blocking call. But the above routine takes care of ensuring that this is terminated once the queue is shutdown
	c.processQueueItems(ctx, 1)

	timestamp := now.Format(timestampFormat)
	logs, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Len(t, logs, 5)
	assert.Equal(t, testLogs[0], logs[0].Name())
	assert.Equal(t, testLogs[1]+"."+timestamp, logs[1].Name())
	assert.Equal(t, testLogs[4]+compressSuffix, logs[2].Name())
	assert.Equal(t, testLogs[2]+"."+timestamp, logs[3].Name())
	assert.Equal(t, testLogs[3], logs[4].Name())
}

func TestClean(t *testing.T) {
	ctx := context.Background()
	dir, err := os.MkdirTemp("", "test-clean")
	require.NoError(t, err)
	defer os.RemoveAll(dir)

	const (
		testMaxFiles = 3
		testMaxSize  = 10
	)
	now := time.Now()
	f := critest.NewFakeRuntimeService()
	c := &containerLogManager{
		runtimeService: f,
		policy: LogRotatePolicy{
			MaxSize:  testMaxSize,
			MaxFiles: testMaxFiles,
		},
		osInterface: container.RealOS{},
		clock:       testingclock.NewFakeClock(now),
		mutex:       sync.Mutex{},
		queue: workqueue.NewTypedRateLimitingQueueWithConfig(
			workqueue.DefaultTypedControllerRateLimiter[string](),
			workqueue.TypedRateLimitingQueueConfig[string]{Name: "kubelet_log_rotate_manager"},
		),
		maxWorkers:       10,
		monitoringPeriod: v1.Duration{Duration: 10 * time.Second},
	}
	testLogs := []string{
		"test-log-1",
		"test-log-2",
		"test-log-3",
		"test-log-2.00000000-000000.gz",
		"test-log-2.00000000-000001",
		"test-log-3.00000000-000000.gz",
		"test-log-3.00000000-000001",
	}
	for i := range testLogs {
		f, err := os.Create(filepath.Join(dir, testLogs[i]))
		require.NoError(t, err)
		f.Close()
	}
	testContainers := []*critest.FakeContainer{
		{
			ContainerStatus: runtimeapi.ContainerStatus{
				Id:      "container-1",
				State:   runtimeapi.ContainerState_CONTAINER_RUNNING,
				LogPath: filepath.Join(dir, testLogs[0]),
			},
		},
		{
			ContainerStatus: runtimeapi.ContainerStatus{
				Id:      "container-2",
				State:   runtimeapi.ContainerState_CONTAINER_RUNNING,
				LogPath: filepath.Join(dir, testLogs[1]),
			},
		},
		{
			ContainerStatus: runtimeapi.ContainerStatus{
				Id:      "container-3",
				State:   runtimeapi.ContainerState_CONTAINER_EXITED,
				LogPath: filepath.Join(dir, testLogs[2]),
			},
		},
	}
	f.SetFakeContainers(testContainers)

	err = c.Clean(ctx, "container-3")
	require.NoError(t, err)

	logs, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Len(t, logs, 4)
	assert.Equal(t, testLogs[0], logs[0].Name())
	assert.Equal(t, testLogs[1], logs[1].Name())
	assert.Equal(t, testLogs[3], logs[2].Name())
	assert.Equal(t, testLogs[4], logs[3].Name())
}

func TestCleanupUnusedLog(t *testing.T) {
	dir, err := os.MkdirTemp("", "test-cleanup-unused-log")
	require.NoError(t, err)
	defer os.RemoveAll(dir)

	testLogs := []string{
		"test-log-1",     // regular log
		"test-log-1.tmp", // temporary log
		"test-log-2",     // unused log
		"test-log-2.gz",  // compressed log
	}

	for i := range testLogs {
		testLogs[i] = filepath.Join(dir, testLogs[i])
		f, err := os.Create(testLogs[i])
		require.NoError(t, err)
		f.Close()
	}

	c := &containerLogManager{
		osInterface: container.RealOS{},
	}
	got, err := c.cleanupUnusedLogs(testLogs)
	require.NoError(t, err)
	assert.Len(t, got, 2)
	assert.Equal(t, []string{testLogs[0], testLogs[3]}, got)

	logs, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Len(t, logs, 2)
	assert.Equal(t, testLogs[0], filepath.Join(dir, logs[0].Name()))
	assert.Equal(t, testLogs[3], filepath.Join(dir, logs[1].Name()))
}

func TestRemoveExcessLog(t *testing.T) {
	for desc, test := range map[string]struct {
		max    int
		expect []string
	}{
		"MaxFiles equal to 2": {
			max:    2,
			expect: []string{},
		},
		"MaxFiles more than 2": {
			max:    3,
			expect: []string{"test-log-4"},
		},
		"MaxFiles more than log file number": {
			max:    6,
			expect: []string{"test-log-1", "test-log-2", "test-log-3", "test-log-4"},
		},
	} {
		t.Logf("TestCase %q", desc)
		dir, err := os.MkdirTemp("", "test-remove-excess-log")
		require.NoError(t, err)
		defer os.RemoveAll(dir)

		testLogs := []string{"test-log-3", "test-log-1", "test-log-2", "test-log-4"}

		for i := range testLogs {
			testLogs[i] = filepath.Join(dir, testLogs[i])
			f, err := os.Create(testLogs[i])
			require.NoError(t, err)
			f.Close()
		}

		c := &containerLogManager{
			policy:      LogRotatePolicy{MaxFiles: test.max},
			osInterface: container.RealOS{},
		}
		got, err := c.removeExcessLogs(testLogs)
		require.NoError(t, err)
		require.Len(t, got, len(test.expect))
		for i, name := range test.expect {
			assert.Equal(t, name, filepath.Base(got[i]))
		}

		logs, err := os.ReadDir(dir)
		require.NoError(t, err)
		require.Len(t, logs, len(test.expect))
		for i, name := range test.expect {
			assert.Equal(t, name, logs[i].Name())
		}
	}
}

func TestCompressLog(t *testing.T) {
	dir, err := os.MkdirTemp("", "test-compress-log")
	require.NoError(t, err)
	defer os.RemoveAll(dir)

	testFile, err := os.CreateTemp(dir, "test-rotate-latest-log")
	require.NoError(t, err)
	defer testFile.Close()
	testContent := "test log content"
	_, err = testFile.Write([]byte(testContent))
	require.NoError(t, err)
	testFile.Close()

	testLog := testFile.Name()
	c := &containerLogManager{osInterface: container.RealOS{}}
	require.NoError(t, c.compressLog(testLog))
	_, err = os.Stat(testLog + compressSuffix)
	assert.NoError(t, err, "log should be compressed")
	_, err = os.Stat(testLog + tmpSuffix)
	assert.Error(t, err, "temporary log should be renamed")
	_, err = os.Stat(testLog)
	assert.Error(t, err, "original log should be removed")

	rc, err := UncompressLog(testLog + compressSuffix)
	require.NoError(t, err)
	defer rc.Close()
	var buf bytes.Buffer
	_, err = io.Copy(&buf, rc)
	require.NoError(t, err)
	assert.Equal(t, testContent, buf.String())
}

func TestRotateLatestLog(t *testing.T) {
	ctx := context.Background()
	dir, err := os.MkdirTemp("", "test-rotate-latest-log")
	require.NoError(t, err)
	defer os.RemoveAll(dir)

	for desc, test := range map[string]struct {
		runtimeError   error
		maxFiles       int
		expectError    bool
		expectOriginal bool
		expectRotated  bool
	}{
		"should successfully rotate log when MaxFiles is 2": {
			maxFiles:       2,
			expectError:    false,
			expectOriginal: false,
			expectRotated:  true,
		},
		"should restore original log when ReopenContainerLog fails": {
			runtimeError:   fmt.Errorf("random error"),
			maxFiles:       2,
			expectError:    true,
			expectOriginal: true,
			expectRotated:  false,
		},
	} {
		t.Logf("TestCase %q", desc)
		now := time.Now()
		f := critest.NewFakeRuntimeService()
		c := &containerLogManager{
			runtimeService: f,
			policy:         LogRotatePolicy{MaxFiles: test.maxFiles},
			osInterface:    container.RealOS{},
			clock:          testingclock.NewFakeClock(now),
			mutex:          sync.Mutex{},
			queue: workqueue.NewTypedRateLimitingQueueWithConfig(
				workqueue.DefaultTypedControllerRateLimiter[string](),
				workqueue.TypedRateLimitingQueueConfig[string]{Name: "kubelet_log_rotate_manager"},
			),
			maxWorkers:       10,
			monitoringPeriod: v1.Duration{Duration: 10 * time.Second},
		}
		if test.runtimeError != nil {
			f.InjectError("ReopenContainerLog", test.runtimeError)
		}
		testFile, err := os.CreateTemp(dir, "test-rotate-latest-log")
		require.NoError(t, err)
		testFile.Close()
		defer testFile.Close()
		testLog := testFile.Name()
		rotatedLog := fmt.Sprintf("%s.%s", testLog, now.Format(timestampFormat))
		err = c.rotateLatestLog(ctx, "test-id", testLog)
		assert.Equal(t, test.expectError, err != nil)
		_, err = os.Stat(testLog)
		assert.Equal(t, test.expectOriginal, err == nil)
		_, err = os.Stat(rotatedLog)
		assert.Equal(t, test.expectRotated, err == nil)
		assert.NoError(t, f.AssertCalls([]string{"ReopenContainerLog"}))
	}
}
