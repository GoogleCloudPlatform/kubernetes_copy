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

package prober

import (
	"context"
	"fmt"
	"io"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/record"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
	"k8s.io/kubernetes/pkg/kubelet/events"
	"k8s.io/kubernetes/pkg/kubelet/prober/results"
	"k8s.io/kubernetes/pkg/kubelet/util/format"
	"k8s.io/kubernetes/pkg/probe"
	execprobe "k8s.io/kubernetes/pkg/probe/exec"
	grpcprobe "k8s.io/kubernetes/pkg/probe/grpc"
	httpprobe "k8s.io/kubernetes/pkg/probe/http"
	tcpprobe "k8s.io/kubernetes/pkg/probe/tcp"
	"k8s.io/utils/exec"

	"k8s.io/klog/v2"
)

const maxProbeRetries = 3

// Prober helps to check the liveness/readiness/startup of a container.
type prober struct {
	exec   execprobe.Prober
	http   httpprobe.Prober
	tcp    tcpprobe.Prober
	grpc   grpcprobe.Prober
	runner kubecontainer.CommandRunner

	recorder record.EventRecorder
}

// NewProber creates a Prober, it takes a command runner and
// several container info managers.
func newProber(
	runner kubecontainer.CommandRunner,
	recorder record.EventRecorder) *prober {

	const followNonLocalRedirects = false
	return &prober{
		exec:     execprobe.New(),
		http:     httpprobe.New(followNonLocalRedirects),
		tcp:      tcpprobe.New(),
		grpc:     grpcprobe.New(),
		runner:   runner,
		recorder: recorder,
	}
}

// recordContainerEvent should be used by the prober for all container related events.
func (pb *prober) recordContainerEvent(pod *v1.Pod, container *v1.Container, eventType, reason, message string, args ...interface{}) {
	ref, err := kubecontainer.GenerateContainerRef(pod, container)
	if err != nil {
		klog.ErrorS(err, "Can't make a ref to pod and container", "pod", klog.KObj(pod), "containerName", container.Name)
		return
	}
	pb.recorder.Eventf(ref, eventType, reason, message, args...)
}

// probe probes the container.
func (pb *prober) probe(ctx context.Context, probeType probeType, pod *v1.Pod, status v1.PodStatus, container v1.Container, containerID kubecontainer.ContainerID) (results.Result, error) {
	var probeSpec *v1.Probe
	switch probeType {
	case readiness:
		probeSpec = container.ReadinessProbe
	case liveness:
		probeSpec = container.LivenessProbe
	case startup:
		probeSpec = container.StartupProbe
	default:
		return results.Failure, fmt.Errorf("unknown probe type: %q", probeType)
	}

	if probeSpec == nil {
		klog.InfoS("Probe is nil", "probeType", probeType, "pod", klog.KObj(pod), "podUID", pod.UID, "containerName", container.Name)
		return results.Success, nil
	}

	result, output, err := pb.runProbeWithRetries(ctx, probeType, probeSpec, pod, status, container, containerID, maxProbeRetries)
	if err != nil || (result != probe.Success && result != probe.Warning) {
		// Probe failed in one way or another.
		switch {
		case err != nil:
			klog.V(1).ErrorS(err, "Probe errored", "probeType", probeType, "pod", klog.KObj(pod), "podUID", pod.UID, "containerName", container.Name)
			pb.recordContainerEvent(pod, &container, v1.EventTypeWarning, events.ContainerUnhealthy, "%s probe errored: %v", probeType, err)

		case result == probe.Unknown:
			klog.V(3).InfoS("Probe result unknown", "probeType", probeType, "pod", klog.KObj(pod), "podUID", pod.UID, "containerName", container.Name, "output", output)
			pb.recordContainerEvent(pod, &container, v1.EventTypeWarning, events.ContainerProbeWarning, "%s probe unknown: %s", probeType, output)

		default: // result != probe.Success
			klog.V(1).InfoS("Probe failed", "probeType", probeType, "pod", klog.KObj(pod), "podUID", pod.UID, "containerName", container.Name, "probeResult", result, "output", output)
			pb.recordContainerEvent(pod, &container, v1.EventTypeWarning, events.ContainerUnhealthy, "%s probe failed: %s", probeType, output)
		}
		return results.Failure, err
	}
	if result == probe.Warning {
		pb.recordContainerEvent(pod, &container, v1.EventTypeWarning, events.ContainerProbeWarning, "%s probe warning: %s", probeType, output)
		klog.V(3).InfoS("Probe succeeded with a warning", "probeType", probeType, "pod", klog.KObj(pod), "podUID", pod.UID, "containerName", container.Name, "output", output)
	} else {
		klog.V(3).InfoS("Probe succeeded", "probeType", probeType, "pod", klog.KObj(pod), "podUID", pod.UID, "containerName", container.Name)
	}
	return results.Success, nil
}

// runProbeWithRetries tries to probe the container in a finite loop, it returns the last result
// if it never succeeds.
func (pb *prober) runProbeWithRetries(ctx context.Context, probeType probeType, p *v1.Probe, pod *v1.Pod, status v1.PodStatus, container v1.Container, containerID kubecontainer.ContainerID, retries int) (probe.Result, string, error) {
	var err error
	var result probe.Result
	var output string
	for i := 0; i < retries; i++ {
		result, output, err = pb.runProbe(ctx, probeType, p, pod, status, container, containerID)
		if err == nil {
			return result, output, nil
		}
	}
	return result, output, err
}

func (pb *prober) runProbe(ctx context.Context, probeType probeType, p *v1.Probe, pod *v1.Pod, status v1.PodStatus, container v1.Container, containerID kubecontainer.ContainerID) (probe.Result, string, error) {
	timeout := time.Duration(p.TimeoutSeconds) * time.Second
	switch {
	case p.Exec != nil:
		klog.V(4).InfoS("Exec-Probe runProbe", "pod", klog.KObj(pod), "containerName", container.Name, "execCommand", p.Exec.Command)
		command := kubecontainer.ExpandContainerCommandOnlyStatic(p.Exec.Command, container.Env)
		return pb.exec.Probe(pb.newExecInContainer(ctx, container, containerID, command, timeout))

	case p.HTTPGet != nil:
		req, err := httpprobe.NewRequestForHTTPGetAction(p.HTTPGet, &container, status.PodIP, "probe")
		if err != nil {
			return probe.Unknown, "", err
		}
		if klogV4 := klog.V(4); klogV4.Enabled() {
			port := req.URL.Port()
			host := req.URL.Hostname()
			path := req.URL.Path
			scheme := req.URL.Scheme
			headers := p.HTTPGet.HTTPHeaders
			klogV4.InfoS("HTTP-Probe", "scheme", scheme, "host", host, "port", port, "path", path, "timeout", timeout, "headers", headers)
		}
		return pb.http.Probe(req, timeout)

	case p.TCPSocket != nil:
		port, err := probe.ResolveContainerPort(p.TCPSocket.Port, &container)
		if err != nil {
			return probe.Unknown, "", err
		}
		host := p.TCPSocket.Host
		if host == "" {
			host = status.PodIP
		}
		klog.V(4).InfoS("TCP-Probe", "host", host, "port", port, "timeout", timeout)
		return pb.tcp.Probe(host, port, timeout)

	case p.GRPC != nil:
		host := status.PodIP
		service := ""
		if p.GRPC.Service != nil {
			service = *p.GRPC.Service
		}
		klog.V(4).InfoS("GRPC-Probe", "host", host, "service", service, "port", p.GRPC.Port, "timeout", timeout)
		return pb.grpc.Probe(host, service, int(p.GRPC.Port), timeout)

	default:
		klog.InfoS("Failed to find probe builder for container", "containerName", container.Name)
		return probe.Unknown, "", fmt.Errorf("missing probe handler for %s:%s", format.Pod(pod), container.Name)
	}
}

type execInContainer struct {
	// run executes a command in a container. Combined stdout and stderr output is always returned. An
	// error is returned if one occurred.
	run    func() ([]byte, error)
	writer io.Writer
}

func (pb *prober) newExecInContainer(ctx context.Context, container v1.Container, containerID kubecontainer.ContainerID, cmd []string, timeout time.Duration) exec.Cmd {
	return &execInContainer{run: func() ([]byte, error) {
		return pb.runner.RunInContainer(ctx, containerID, cmd, timeout)
	}}
}

func (eic *execInContainer) Run() error {
	return nil
}

func (eic *execInContainer) CombinedOutput() ([]byte, error) {
	return eic.run()
}

func (eic *execInContainer) Output() ([]byte, error) {
	return nil, fmt.Errorf("unimplemented")
}

func (eic *execInContainer) SetDir(dir string) {
	// unimplemented
}

func (eic *execInContainer) SetStdin(in io.Reader) {
	// unimplemented
}

func (eic *execInContainer) SetStdout(out io.Writer) {
	eic.writer = out
}

func (eic *execInContainer) SetStderr(out io.Writer) {
	eic.writer = out
}

func (eic *execInContainer) SetEnv(env []string) {
	// unimplemented
}

func (eic *execInContainer) Stop() {
	// unimplemented
}

func (eic *execInContainer) Start() error {
	data, err := eic.run()
	if eic.writer != nil {
		// only record the write error, do not cover the command run error
		if p, err := eic.writer.Write(data); err != nil {
			klog.ErrorS(err, "Unable to write all bytes from execInContainer", "expectedBytes", len(data), "actualBytes", p)
		}
	}
	return err
}

func (eic *execInContainer) Wait() error {
	return nil
}

func (eic *execInContainer) StdoutPipe() (io.ReadCloser, error) {
	return nil, fmt.Errorf("unimplemented")
}

func (eic *execInContainer) StderrPipe() (io.ReadCloser, error) {
	return nil, fmt.Errorf("unimplemented")
}
