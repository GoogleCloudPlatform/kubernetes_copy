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

package eviction

import (
	"fmt"
	"sync"
	"time"

	libcontainercgroups "github.com/opencontainers/runc/libcontainer/cgroups"
	"golang.org/x/sys/unix"
	"k8s.io/klog/v2"
)

const (
	// eventSize is the number of bytes returned by a successful read from an eventfd
	// see http://man7.org/linux/man-pages/man2/eventfd.2.html for more information
	eventSize = 8
	// numFdEvents is the number of events we can record at once.
	// If EpollWait finds more than this, they will be missed.
	numFdEvents = 6
)

type linuxCgroupNotifier struct {
	eventfd  int
	epfd     int
	stop     chan struct{}
	stopLock sync.Mutex
}

var _ CgroupNotifier = &linuxCgroupNotifier{}

// NewCgroupNotifier returns a linuxCgroupNotifier, which performs cgroup control operations required
// to receive notifications from the cgroup when the threshold is crossed in either direction.
func NewCgroupNotifier(path, attribute string, threshold int64) (CgroupNotifier, error) {
	// cgroupv2 does not support monitoring cgroup memory thresholds using cgroup.event_control.
	// Instead long term, on cgroupv2 kubelet should rely on combining usage of memory.low on root pods cgroup with inotify notifications on memory.events and or PSI pressure.
	// For now, let's return a fake "disabled" cgroup notifier on cgroupv2.
	// https://github.com/kubernetes/kubernetes/issues/106331
	if libcontainercgroups.IsCgroup2UnifiedMode() {
		return &disabledThresholdNotifier{}, nil
	}

	var watchfd, eventfd, epfd, controlfd int
	var err error
	watchfd, err = unix.Open(fmt.Sprintf("%s/%s", path, attribute), unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	defer unix.Close(watchfd)
	controlfd, err = unix.Open(fmt.Sprintf("%s/cgroup.event_control", path), unix.O_WRONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	defer unix.Close(controlfd)
	eventfd, err = unix.Eventfd(0, unix.EFD_CLOEXEC)
	if err != nil {
		return nil, err
	}
	if eventfd < 0 {
		err = fmt.Errorf("eventfd call failed")
		return nil, err
	}
	defer func() {
		// Close eventfd if we get an error later in initialization
		if err != nil {
			unix.Close(eventfd)
		}
	}()
	epfd, err = unix.EpollCreate1(unix.EPOLL_CLOEXEC)
	if err != nil {
		return nil, err
	}
	if epfd < 0 {
		err = fmt.Errorf("EpollCreate1 call failed")
		return nil, err
	}
	defer func() {
		// Close epfd if we get an error later in initialization
		if err != nil {
			unix.Close(epfd)
		}
	}()
	config := fmt.Sprintf("%d %d %d", eventfd, watchfd, threshold)
	_, err = unix.Write(controlfd, []byte(config))
	if err != nil {
		return nil, err
	}
	return &linuxCgroupNotifier{
		eventfd: eventfd,
		epfd:    epfd,
		stop:    make(chan struct{}),
	}, nil
}

func (n *linuxCgroupNotifier) Start(eventCh chan<- struct{}) {
	err := unix.EpollCtl(n.epfd, unix.EPOLL_CTL_ADD, n.eventfd, &unix.EpollEvent{
		Fd:     int32(n.eventfd),
		Events: unix.EPOLLIN,
	})
	if err != nil {
		klog.InfoS("Eviction manager: error adding epoll eventfd", "err", err)
		return
	}
	buf := make([]byte, eventSize)
	for {
		select {
		case <-n.stop:
			return
		default:
		}
		event, err := wait(n.epfd, n.eventfd, notifierRefreshInterval)
		if err != nil {
			klog.InfoS("Eviction manager: error while waiting for memcg events", "err", err)
			return
		} else if !event {
			// Timeout on wait.  This is expected if the threshold was not crossed
			continue
		}
		// Consume the event from the eventfd
		_, err = unix.Read(n.eventfd, buf)
		if err != nil {
			klog.InfoS("Eviction manager: error reading memcg events", "err", err)
			return
		}
		eventCh <- struct{}{}
	}
}

// wait waits up to notifierRefreshInterval for an event on the Epoll FD for the
// eventfd we are concerned about.  It returns an error if one occurs, and true
// if the consumer should read from the eventfd.
func wait(epfd, eventfd int, timeout time.Duration) (bool, error) {
	events := make([]unix.EpollEvent, numFdEvents+1)
	timeoutMS := int(timeout / time.Millisecond)
	n, err := unix.EpollWait(epfd, events, timeoutMS)
	if n == -1 {
		if err == unix.EINTR {
			// Interrupt, ignore the error
			return false, nil
		}
		return false, err
	}
	if n == 0 {
		// Timeout
		return false, nil
	}
	if n > numFdEvents {
		return false, fmt.Errorf("epoll_wait returned more events than we know what to do with")
	}
	for _, event := range events[:n] {
		if event.Fd == int32(eventfd) {
			if event.Events&unix.EPOLLHUP != 0 || event.Events&unix.EPOLLERR != 0 || event.Events&unix.EPOLLIN != 0 {
				// EPOLLHUP: should not happen, but if it does, treat it as a wakeup.

				// EPOLLERR: If an error is waiting on the file descriptor, we should pretend
				// something is ready to read, and let unix.Read pick up the error.

				// EPOLLIN: There is data to read.
				return true, nil
			}
		}
	}
	// An event occurred that we don't care about.
	return false, nil
}

func (n *linuxCgroupNotifier) Stop() {
	n.stopLock.Lock()
	defer n.stopLock.Unlock()
	select {
	case <-n.stop:
		// the linuxCgroupNotifier is already stopped
		return
	default:
	}
	unix.Close(n.eventfd)
	unix.Close(n.epfd)
	close(n.stop)
}

// disabledThresholdNotifier is a fake disabled threshold notifier that performs no-ops.
type disabledThresholdNotifier struct{}

func (*disabledThresholdNotifier) Start(_ chan<- struct{}) {}
func (*disabledThresholdNotifier) Stop()                   {}
