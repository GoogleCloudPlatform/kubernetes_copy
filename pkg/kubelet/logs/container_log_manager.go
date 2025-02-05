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
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	internalapi "k8s.io/cri-api/pkg/apis"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
	"k8s.io/utils/clock"
)

const (
	// timestampFormat is format of the timestamp suffix for rotated log.
	// See https://golang.org/pkg/time/#Time.Format.
	timestampFormat = "20060102-150405"
	// compressSuffix is the suffix for compressed log.
	compressSuffix = ".gz"
	// tmpSuffix is the suffix for temporary file.
	tmpSuffix = ".tmp"
)

// ContainerLogManager manages lifecycle of all container logs.
//
// Implementation is thread-safe.
type ContainerLogManager interface {
	// TODO(random-liu): Add RotateLogs function and call it under disk pressure.
	// Start container log manager.
	Start()
	// Clean removes all logs of specified container.
	Clean(ctx context.Context, containerID string) error
}

// LogRotatePolicy is a policy for container log rotation. The policy applies to all
// containers managed by kubelet.
type LogRotatePolicy struct {
	// MaxSize in bytes of the container log file before it is rotated. Negative
	// number means to disable container log rotation.
	MaxSize int64
	// MaxFiles is the maximum number of log files that can be present.
	// If rotating the logs creates excess files, the oldest file is removed.
	MaxFiles int
}

// GetAllLogs gets all inuse (rotated/compressed) logs for a specific container log.
// Returned logs are sorted in oldest to newest order.
// TODO(#59902): Leverage this function to support log rotation in `kubectl logs`.
func GetAllLogs(log string) ([]string, error) {
	// pattern is used to match all rotated files.
	pattern := fmt.Sprintf("%s.*", log)
	logs, err := filepath.Glob(pattern)
	if err != nil {
		return nil, fmt.Errorf("failed to list all log files with pattern %q: %w", pattern, err)
	}
	inuse, _ := filterUnusedLogs(logs)
	sort.Strings(inuse)
	return append(inuse, log), nil
}

// compressReadCloser wraps gzip.Reader with a function to close file handler.
type compressReadCloser struct {
	f *os.File
	*gzip.Reader
}

func (rc *compressReadCloser) Close() error {
	ferr := rc.f.Close()
	rerr := rc.Reader.Close()
	if ferr != nil {
		return ferr
	}
	if rerr != nil {
		return rerr
	}
	return nil
}

// UncompressLog compresses a compressed log and return a readcloser for the
// stream of the uncompressed content.
// TODO(#59902): Leverage this function to support log rotation in `kubectl logs`.
func UncompressLog(log string) (_ io.ReadCloser, retErr error) {
	if !strings.HasSuffix(log, compressSuffix) {
		return nil, fmt.Errorf("log is not compressed")
	}
	f, err := os.Open(log)
	if err != nil {
		return nil, fmt.Errorf("failed to open log: %w", err)
	}
	defer func() {
		if retErr != nil {
			f.Close()
		}
	}()
	r, err := gzip.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("failed to create gzip reader: %w", err)
	}
	return &compressReadCloser{f: f, Reader: r}, nil
}

// parseMaxSize parses quantity string to int64 max size in bytes.
func parseMaxSize(size string) (int64, error) {
	quantity, err := resource.ParseQuantity(size)
	if err != nil {
		return 0, err
	}
	maxSize, ok := quantity.AsInt64()
	if !ok {
		return 0, fmt.Errorf("invalid max log size")
	}
	return maxSize, nil
}

type containerLogManager struct {
	runtimeService   internalapi.RuntimeService
	osInterface      kubecontainer.OSInterface
	policy           LogRotatePolicy
	clock            clock.Clock
	mutex            sync.Mutex
	queue            workqueue.TypedRateLimitingInterface[string]
	maxWorkers       int
	monitoringPeriod metav1.Duration
}

// NewContainerLogManager creates a new container log manager.
func NewContainerLogManager(runtimeService internalapi.RuntimeService, osInterface kubecontainer.OSInterface, maxSize string, maxFiles int, maxWorkers int, monitorInterval metav1.Duration) (ContainerLogManager, error) {
	if maxFiles <= 1 {
		return nil, fmt.Errorf("invalid MaxFiles %d, must be > 1", maxFiles)
	}
	parsedMaxSize, err := parseMaxSize(maxSize)
	if err != nil {
		return nil, fmt.Errorf("failed to parse container log max size %q: %w", maxSize, err)
	}
	// Negative number means to disable container log rotation
	if parsedMaxSize < 0 {
		return NewStubContainerLogManager(), nil
	}
	// policy LogRotatePolicy
	return &containerLogManager{
		osInterface:    osInterface,
		runtimeService: runtimeService,
		policy: LogRotatePolicy{
			MaxSize:  parsedMaxSize,
			MaxFiles: maxFiles,
		},
		clock:      clock.RealClock{},
		mutex:      sync.Mutex{},
		maxWorkers: maxWorkers,
		queue: workqueue.NewTypedRateLimitingQueueWithConfig(
			workqueue.DefaultTypedControllerRateLimiter[string](),
			workqueue.TypedRateLimitingQueueConfig[string]{Name: "kubelet_log_rotate_manager"},
		),
		monitoringPeriod: monitorInterval,
	}, nil
}

// Start the container log manager.
func (c *containerLogManager) Start() {
	ctx := context.Background()
	klog.InfoS("Initializing container log rotate workers", "workers", c.maxWorkers, "monitorPeriod", c.monitoringPeriod)
	for i := 0; i < c.maxWorkers; i++ {
		worker := i + 1
		go c.processQueueItems(ctx, worker)
	}
	// Start a goroutine periodically does container log rotation.
	go wait.Forever(func() {
		if err := c.rotateLogs(ctx); err != nil {
			klog.ErrorS(err, "Failed to rotate container logs")
		}
	}, c.monitoringPeriod.Duration)
}

// Clean removes all logs of specified container (including rotated one).
func (c *containerLogManager) Clean(ctx context.Context, containerID string) error {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	resp, err := c.runtimeService.ContainerStatus(ctx, containerID, false)
	if err != nil {
		return fmt.Errorf("failed to get container status %q: %w", containerID, err)
	}
	if resp.GetStatus() == nil {
		return fmt.Errorf("container status is nil for %q", containerID)
	}
	pattern := fmt.Sprintf("%s*", resp.GetStatus().GetLogPath())
	logs, err := c.osInterface.Glob(pattern)
	if err != nil {
		return fmt.Errorf("failed to list all log files with pattern %q: %w", pattern, err)
	}

	for _, l := range logs {
		if err := c.osInterface.Remove(l); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("failed to remove container %q log %q: %w", containerID, l, err)
		}
	}

	return nil
}

func (c *containerLogManager) processQueueItems(ctx context.Context, worker int) {
	klog.V(4).InfoS("Starting container log rotation worker", "workerID", worker)
	for c.processContainer(ctx, worker) {
	}
	klog.V(4).InfoS("Terminating container log rotation worker", "workerID", worker)
}

func (c *containerLogManager) rotateLogs(ctx context.Context) error {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	klog.V(4).InfoS("Starting container log rotation sequence")
	// TODO(#59998): Use kubelet pod cache.
	containers, err := c.runtimeService.ListContainers(ctx, &runtimeapi.ContainerFilter{})
	if err != nil {
		return fmt.Errorf("failed to list containers: %w", err)
	}
	for _, container := range containers {
		// Only rotate logs for running containers. Non-running containers won't
		// generate new output, it doesn't make sense to keep an empty latest log.
		if container.GetState() != runtimeapi.ContainerState_CONTAINER_RUNNING {
			continue
		}
		// Doing this to avoid additional overhead with logging of label like arguments that can prove costly
		if v := klog.V(4); v.Enabled() {
			klog.V(4).InfoS("Adding new entry to the queue for processing", "id", container.GetId(), "name", container.Metadata.GetName(), "labels", container.GetLabels())
		}
		c.queue.Add(container.GetId())
	}
	return nil
}

func (c *containerLogManager) processContainer(ctx context.Context, worker int) (ok bool) {
	key, quit := c.queue.Get()
	if quit {
		return false
	}
	defer func() {
		c.queue.Done(key)
		c.queue.Forget(key)
	}()
	// Always default the return to true to keep the processing of Queue ongoing
	ok = true
	id := key

	resp, err := c.runtimeService.ContainerStatus(ctx, id, false)
	if err != nil {
		klog.ErrorS(err, "Failed to get container status", "worker", worker, "containerID", id)
		return
	}
	if resp.GetStatus() == nil {
		klog.ErrorS(err, "Container status is nil", "worker", worker, "containerID", id)
		return
	}
	path := resp.GetStatus().GetLogPath()
	info, err := c.osInterface.Stat(path)

	if err != nil {
		if !os.IsNotExist(err) {
			klog.ErrorS(err, "Failed to stat container log", "worker", worker, "containerID", id, "path", path)
			return
		}

		if err = c.runtimeService.ReopenContainerLog(ctx, id); err != nil {
			klog.ErrorS(err, "Container log doesn't exist, reopen container log failed", "worker", worker, "containerID", id, "path", path)
			return
		}

		info, err = c.osInterface.Stat(path)
		if err != nil {
			klog.ErrorS(err, "Failed to stat container log after reopen", "worker", worker, "containerID", id, "path", path)
			return
		}
	}
	if info.Size() < c.policy.MaxSize {
		klog.V(7).InfoS("log file doesn't need to be rotated", "worker", worker, "containerID", id, "path", path, "currentSize", info.Size(), "maxSize", c.policy.MaxSize)
		return
	}

	if err := c.rotateLog(ctx, id, path); err != nil {
		klog.ErrorS(err, "Failed to rotate log for container", "worker", worker, "containerID", id, "path", path, "currentSize", info.Size(), "maxSize", c.policy.MaxSize)
		return
	}
	return
}

func (c *containerLogManager) rotateLog(ctx context.Context, id, log string) error {
	// pattern is used to match all rotated files.
	pattern := fmt.Sprintf("%s.*", log)
	logs, err := filepath.Glob(pattern)
	if err != nil {
		return fmt.Errorf("failed to list all log files with pattern %q: %w", pattern, err)
	}

	logs, err = c.cleanupUnusedLogs(logs)
	if err != nil {
		return fmt.Errorf("failed to cleanup logs: %w", err)
	}

	logs, err = c.removeExcessLogs(logs)
	if err != nil {
		return fmt.Errorf("failed to remove excess logs: %w", err)
	}

	// Compress uncompressed log files.
	for _, l := range logs {
		if strings.HasSuffix(l, compressSuffix) {
			continue
		}
		if err := c.compressLog(l); err != nil {
			return fmt.Errorf("failed to compress log %q: %w", l, err)
		}
	}

	if err := c.rotateLatestLog(ctx, id, log); err != nil {
		return fmt.Errorf("failed to rotate log %q: %w", log, err)
	}

	return nil
}

// cleanupUnusedLogs cleans up temporary or unused log files generated by previous log rotation
// failure.
func (c *containerLogManager) cleanupUnusedLogs(logs []string) ([]string, error) {
	inuse, unused := filterUnusedLogs(logs)
	for _, l := range unused {
		if err := c.osInterface.Remove(l); err != nil {
			return nil, fmt.Errorf("failed to remove unused log %q: %w", l, err)
		}
	}
	return inuse, nil
}

// filterUnusedLogs splits logs into 2 groups, the 1st group is in used logs,
// the second group is unused logs.
func filterUnusedLogs(logs []string) (inuse []string, unused []string) {
	for _, l := range logs {
		if isInUse(l, logs) {
			inuse = append(inuse, l)
		} else {
			unused = append(unused, l)
		}
	}
	return inuse, unused
}

// isInUse checks whether a container log file is still inuse.
func isInUse(l string, logs []string) bool {
	// All temporary files are not in use.
	if strings.HasSuffix(l, tmpSuffix) {
		return false
	}
	// All compressed logs are in use.
	if strings.HasSuffix(l, compressSuffix) {
		return true
	}
	// Files has already been compressed are not in use.
	for _, another := range logs {
		if l+compressSuffix == another {
			return false
		}
	}
	return true
}

// removeExcessLogs removes old logs to make sure there are only at most MaxFiles log files.
func (c *containerLogManager) removeExcessLogs(logs []string) ([]string, error) {
	// Sort log files in oldest to newest order.
	sort.Strings(logs)
	// Container will create a new log file, and we'll rotate the latest log file.
	// Other than those 2 files, we can have at most MaxFiles-2 rotated log files.
	// Keep MaxFiles-2 files by removing old files.
	// We should remove from oldest to newest, so as not to break ongoing `kubectl logs`.
	maxRotatedFiles := c.policy.MaxFiles - 2
	if maxRotatedFiles < 0 {
		maxRotatedFiles = 0
	}
	i := 0
	for ; i < len(logs)-maxRotatedFiles; i++ {
		if err := c.osInterface.Remove(logs[i]); err != nil {
			return nil, fmt.Errorf("failed to remove old log %q: %w", logs[i], err)
		}
	}
	logs = logs[i:]
	return logs, nil
}

// compressLog compresses a log to log.gz with gzip.
func (c *containerLogManager) compressLog(log string) error {
	r, err := c.osInterface.Open(log)
	if err != nil {
		return fmt.Errorf("failed to open log %q: %w", log, err)
	}
	defer r.Close()
	logInfo, err := os.Stat(log)
	if err != nil {
		return fmt.Errorf("failed to get log info %q: %w", log, err)
	}
	tmpLog := log + tmpSuffix
	f, err := c.osInterface.OpenFile(tmpLog, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, logInfo.Mode())
	if err != nil {
		return fmt.Errorf("failed to create temporary log %q: %w", tmpLog, err)
	}
	defer func() {
		// Best effort cleanup of tmpLog.
		c.osInterface.Remove(tmpLog)
	}()
	defer f.Close()
	w := gzip.NewWriter(f)
	defer w.Close()
	if _, err := io.Copy(w, r); err != nil {
		return fmt.Errorf("failed to compress %q to %q: %w", log, tmpLog, err)
	}
	// The archive needs to be closed before renaming, otherwise an error will occur on Windows.
	w.Close()
	f.Close()
	compressedLog := log + compressSuffix
	if err := c.osInterface.Rename(tmpLog, compressedLog); err != nil {
		return fmt.Errorf("failed to rename %q to %q: %w", tmpLog, compressedLog, err)
	}
	// Remove old log file.
	r.Close()
	if err := c.osInterface.Remove(log); err != nil {
		return fmt.Errorf("failed to remove log %q after compress: %w", log, err)
	}
	return nil
}

// rotateLatestLog rotates latest log without compression, so that container can still write
// and fluentd can finish reading.
func (c *containerLogManager) rotateLatestLog(ctx context.Context, id, log string) error {
	timestamp := c.clock.Now().Format(timestampFormat)
	rotated := fmt.Sprintf("%s.%s", log, timestamp)
	if err := c.osInterface.Rename(log, rotated); err != nil {
		return fmt.Errorf("failed to rotate log %q to %q: %w", log, rotated, err)
	}
	if err := c.runtimeService.ReopenContainerLog(ctx, id); err != nil {
		// Rename the rotated log back, so that we can try rotating it again
		// next round.
		// If kubelet gets restarted at this point, we'll lose original log.
		if renameErr := c.osInterface.Rename(rotated, log); renameErr != nil {
			// This shouldn't happen.
			// Report an error if this happens, because we will lose original
			// log.
			klog.ErrorS(renameErr, "Failed to rename rotated log", "rotatedLog", rotated, "newLog", log, "containerID", id)
		}
		return fmt.Errorf("failed to reopen container log %q: %w", id, err)
	}
	return nil
}
