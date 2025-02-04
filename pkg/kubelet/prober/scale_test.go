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

package prober

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/record"
	kubepod "k8s.io/kubernetes/pkg/kubelet/pod"
	"k8s.io/kubernetes/pkg/kubelet/prober/results"
	"k8s.io/kubernetes/pkg/kubelet/status"
	statustest "k8s.io/kubernetes/pkg/kubelet/status/testing"
	kubeletutil "k8s.io/kubernetes/pkg/kubelet/util"
	"k8s.io/utils/ptr"
)

// TCP sockets goes through a TIME-WAIT state (default 60 sec) before being freed,
// causing conntrack entries and ephemeral ports to be hold for 60 seconds
// despite the probe may have finished in less than 1 second.
// If the rate of probes is higher than the rate the OS recycles the ports used,
// it can consume a considerable number of ephemeral ports or conntrack entries.
// These tests verify that after certain period the probes keep working, if the probes
// don't close the sockets faster, they will start to fail.
// The test creates a TCP or HTTP server to fake a pod. It creates 1 pod with 600 fake
// containers each and runs one probe for each of these containers (all the probes comes
// from the same process, same as in the Kubelet, and targets the same IP:port to verify
// that the ephemeral port is not exhausted.

// The default port range on a normal Linux system has 28321 free ephemeral ports per
// tuple srcIP,srcPort:dstIP:dstPort:Proto: /proc/sys/net/ipv4/ip_local_port_range 32768 60999
// 1 pods x 600 containers/pod x 1 probes/container x 1 req/sec = 600 req/sec
// 600 req/sec x 59 sec = 35400
// The test should run out of ephemeral ports in less than one minute and start failing connections
// Ref: https://github.com/kubernetes/kubernetes/issues/89898#issuecomment-1383207322

func TestTCPPortExhaustion(t *testing.T) {
	// This test creates a considerable number of connections in a short time
	// and flakes on constrained environments, thus it is skipped by default.
	// The test is left for manual verification or experimentation with new
	// changes on the probes.
	t.Skip("skipping TCP port exhaustion tests")

	const (
		numTestPods   = 1
		numContainers = 600
	)

	tests := []struct {
		name string
		http bool // it can be tcp or http
	}{
		{"TCP", false},
		{"HTTP", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			testRootDir := ""
			if tempDir, err := os.MkdirTemp("", "kubelet_test."); err != nil {
				t.Fatalf("can't make a temp rootdir: %v", err)
			} else {
				testRootDir = tempDir
			}
			podManager := kubepod.NewBasicPodManager()
			podStartupLatencyTracker := kubeletutil.NewPodStartupLatencyTracker()
			m := NewManager(
				status.NewManager(&fake.Clientset{}, podManager, &statustest.FakePodDeletionSafetyProvider{}, podStartupLatencyTracker, testRootDir),
				results.NewManager(),
				results.NewManager(),
				results.NewManager(),
				nil, // runner
				&record.FakeRecorder{},
			).(*manager)
			defer cleanup(t, m)

			now := time.Now()
			fakePods := make([]*fakePod, numTestPods)
			for i := 0; i < numTestPods; i++ {
				fake, err := newFakePod(tt.http)
				if err != nil {
					t.Fatalf("unexpected error creating fake pod: %v", err)
				}
				defer fake.stop()
				handler := fake.probeHandler()
				fakePods[i] = fake

				pod := v1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						UID:       types.UID(fmt.Sprintf("pod%d", i)),
						Name:      fmt.Sprintf("pod%d", i),
						Namespace: "test",
					},
					Spec: v1.PodSpec{},
					Status: v1.PodStatus{
						Phase:  v1.PodPhase(v1.PodReady),
						PodIPs: []v1.PodIP{{IP: "127.0.0.1"}},
					},
				}
				for j := 0; j < numContainers; j++ {
					// use only liveness probes for simplicity, initial state is success for them
					pod.Spec.Containers = append(pod.Spec.Containers, v1.Container{
						Name:          fmt.Sprintf("container%d", j),
						LivenessProbe: newProbe(handler),
					})
					pod.Status.ContainerStatuses = append(pod.Status.ContainerStatuses, v1.ContainerStatus{
						Name:        fmt.Sprintf("container%d", j),
						ContainerID: fmt.Sprintf("pod%d://container%d", i, j),
						State: v1.ContainerState{
							Running: &v1.ContainerStateRunning{
								StartedAt: metav1.Now(),
							},
						},
						Started: ptr.To(true),
					})
				}
				podManager.AddPod(&pod)
				m.statusManager.SetPodStatus(&pod, pod.Status)
				m.AddPod(&pod)
			}
			t.Logf("Adding %d pods with %d containers each in %v", numTestPods, numContainers, time.Since(now))

			ctx, cancel := context.WithTimeout(context.Background(), 59*time.Second)
			defer cancel()
			var wg sync.WaitGroup

			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					var result results.Update
					var probeType string
					select {
					case result = <-m.startupManager.Updates():
						probeType = "startup"
					case result = <-m.livenessManager.Updates():
						probeType = "liveness"
					case result = <-m.readinessManager.Updates():
						probeType = "readiness"
					case <-ctx.Done():
						return
					}
					switch result.Result.String() {
					// The test will fail if any of the probes fails
					case "Failure":
						t.Errorf("Failure %s on contantinerID: %v Pod %v", probeType, result.ContainerID, result.PodUID)
					case "UNKNOWN": // startup probes
						t.Logf("UNKNOWN state for %v", result)
					default:
					}
				}
			}()
			wg.Wait()

			// log the number of connections received in each pod for debugging test failures.
			for _, pod := range fakePods {
				n := pod.connections()
				t.Logf("Number of connections %d", n)
			}

		})
	}

}

func newProbe(handler v1.ProbeHandler) *v1.Probe {
	return &v1.Probe{
		ProbeHandler:     handler,
		TimeoutSeconds:   1,
		PeriodSeconds:    1,
		SuccessThreshold: 1,
		FailureThreshold: 3,
	}
}

// newFakePod runs a server (TCP or HTTP) in a random port
func newFakePod(httpServer bool) (*fakePod, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("failed to bind: %v", err)
	}
	f := &fakePod{ln: ln, http: httpServer}

	// spawn an http server or a TCP server that counts the number of connections received
	if httpServer {
		var mu sync.Mutex
		visitors := map[string]struct{}{}
		go http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			defer mu.Unlock()
			if _, ok := visitors[r.RemoteAddr]; !ok {
				atomic.AddInt64(&f.numConnection, 1)
				visitors[r.RemoteAddr] = struct{}{}
			}
		}))
	} else {
		go func() {
			for {
				conn, err := ln.Accept()
				if err != nil {
					// exit when the listener is closed
					return
				}
				atomic.AddInt64(&f.numConnection, 1)
				// handle request but not block
				go func(c net.Conn) {
					defer c.Close()
					// read but swallow the errors since the probe doesn't send data
					buffer := make([]byte, 1024)
					c.Read(buffer)
					// respond
					conn.Write([]byte("Hi back!\n"))
				}(conn)

			}
		}()
	}
	return f, nil

}

type fakePod struct {
	ln            net.Listener
	numConnection int64
	http          bool
}

func (f *fakePod) probeHandler() v1.ProbeHandler {
	port := f.ln.Addr().(*net.TCPAddr).Port
	var handler v1.ProbeHandler
	if f.http {
		handler = v1.ProbeHandler{
			HTTPGet: &v1.HTTPGetAction{
				Host: "127.0.0.1",
				Port: intstr.FromInt32(int32(port)),
			},
		}
	} else {
		handler = v1.ProbeHandler{
			TCPSocket: &v1.TCPSocketAction{
				Host: "127.0.0.1",
				Port: intstr.FromInt32(int32(port)),
			},
		}
	}
	return handler
}

func (f *fakePod) stop() {
	f.ln.Close()
}

func (f *fakePod) connections() int {
	return int(atomic.LoadInt64(&f.numConnection))
}
