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

package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/features"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
	"k8s.io/kubernetes/pkg/kubelet/metrics"
	kubetypes "k8s.io/kubernetes/pkg/kubelet/types"
	"k8s.io/kubernetes/pkg/kubelet/util/format"
	httpprobe "k8s.io/kubernetes/pkg/probe/http"
	"k8s.io/kubernetes/pkg/security/apparmor"
)

const (
	maxRespBodyLength = 10 * 1 << 10 // 10KB

	AppArmorNotAdmittedReason = "AppArmor"
)

type handlerRunner struct {
	httpDoer         kubetypes.HTTPDoer
	commandRunner    kubecontainer.CommandRunner
	containerManager podStatusProvider
	eventRecorder    record.EventRecorder
}

type podStatusProvider interface {
	GetPodStatus(ctx context.Context, uid types.UID, name, namespace string) (*kubecontainer.PodStatus, error)
}

// NewHandlerRunner returns a configured lifecycle handler for a container.
func NewHandlerRunner(httpDoer kubetypes.HTTPDoer, commandRunner kubecontainer.CommandRunner, containerManager podStatusProvider, eventRecorder record.EventRecorder) kubecontainer.HandlerRunner {
	return &handlerRunner{
		httpDoer:         httpDoer,
		commandRunner:    commandRunner,
		containerManager: containerManager,
		eventRecorder:    eventRecorder,
	}
}

func (hr *handlerRunner) Run(ctx context.Context, containerID kubecontainer.ContainerID, pod *v1.Pod, container *v1.Container, handler *v1.LifecycleHandler, getEnvsFunc kubecontainer.GetEnvsFunc) (string, error) {
	switch {
	case handler.Exec != nil:
		var msg string
		// TODO(tallclair): Pass a proper timeout value.
		output, err := hr.commandRunner.RunInContainer(ctx, containerID, handler.Exec.Command, 0)
		if err != nil {
			msg = fmt.Sprintf("Exec lifecycle hook (%v) for Container %q in Pod %q failed - error: %v, message: %q", handler.Exec.Command, container.Name, format.Pod(pod), err, string(output))
			klog.V(1).ErrorS(err, "Exec lifecycle hook for Container in Pod failed", "execCommand", handler.Exec.Command, "containerName", container.Name, "pod", klog.KObj(pod), "message", string(output))
		}
		return msg, err
	case handler.HTTPGet != nil:
		err := hr.runHTTPHandler(ctx, pod, container, handler, hr.eventRecorder, getEnvsFunc)
		var msg string
		if err != nil {
			msg = fmt.Sprintf("HTTP lifecycle hook (%s) for Container %q in Pod %q failed - error: %v", handler.HTTPGet.Path, container.Name, format.Pod(pod), err)
			klog.V(1).ErrorS(err, "HTTP lifecycle hook for Container in Pod failed", "path", handler.HTTPGet.Path, "containerName", container.Name, "pod", klog.KObj(pod))
		}
		return msg, err
	case handler.Sleep != nil:
		err := hr.runSleepHandler(ctx, handler.Sleep.Seconds)
		var msg string
		if err != nil {
			msg = fmt.Sprintf("Sleep lifecycle hook (%d) for Container %q in Pod %q failed - error: %v", handler.Sleep.Seconds, container.Name, format.Pod(pod), err)
			klog.V(1).ErrorS(err, "Sleep lifecycle hook for Container in Pod failed", "sleepSeconds", handler.Sleep.Seconds, "containerName", container.Name, "pod", klog.KObj(pod))
		}
		return msg, err
	default:
		err := fmt.Errorf("invalid handler: %v", handler)
		msg := fmt.Sprintf("Cannot run handler: %v", err)
		klog.ErrorS(err, "Cannot run handler")
		return msg, err
	}
}

// resolvePort attempts to turn an IntOrString port reference into a concrete port number.
// If portReference has an int value, it is treated as a literal, and simply returns that value.
// If portReference is a string, an attempt is first made to parse it as an integer.  If that fails,
// an attempt is made to find a port with the same name in the container spec.
// If a port with the same name is found, it's ContainerPort value is returned.  If no matching
// port is found, an error is returned.
func resolvePort(portReference intstr.IntOrString, container *v1.Container) (int, error) {
	if portReference.Type == intstr.Int {
		return portReference.IntValue(), nil
	}
	portName := portReference.StrVal
	port, err := strconv.Atoi(portName)
	if err == nil {
		return port, nil
	}
	for _, portSpec := range container.Ports {
		if portSpec.Name == portName {
			return int(portSpec.ContainerPort), nil
		}
	}
	return -1, fmt.Errorf("couldn't find port: %v in %v", portReference, container)
}

func (hr *handlerRunner) runSleepHandler(ctx context.Context, seconds int64) error {
	if !utilfeature.DefaultFeatureGate.Enabled(features.PodLifecycleSleepAction) {
		return nil
	}
	c := time.After(time.Duration(seconds) * time.Second)
	select {
	case <-ctx.Done():
		// unexpected termination
		metrics.LifecycleHandlerSleepTerminated.Inc()
		return fmt.Errorf("container terminated before sleep hook finished")
	case <-c:
		return nil
	}
}

func (hr *handlerRunner) runHTTPHandler(ctx context.Context, pod *v1.Pod, container *v1.Container, handler *v1.LifecycleHandler, eventRecorder record.EventRecorder, getEnvsFunc kubecontainer.GetEnvsFunc) error {
	host := handler.HTTPGet.Host
	podIP := host
	podIPs := make([]string, 0)
	if len(host) == 0 {
		status, err := hr.containerManager.GetPodStatus(ctx, pod.UID, pod.Name, pod.Namespace)
		if err != nil {
			klog.ErrorS(err, "Unable to get pod info, event handlers may be invalid.", "pod", klog.KObj(pod))
			return err
		}
		if len(status.IPs) == 0 {
			return fmt.Errorf("failed to find networking container: %v", status)
		}
		host = status.IPs[0]
		podIP = host
		podIPs = status.IPs
	}

	envs := getEnvsFunc(pod, container, podIP, podIPs)
	req, err := httpprobe.NewRequestForHTTPGetAction(handler.HTTPGet, container, podIP, "lifecycle", envs)
	if err != nil {
		return err
	}
	resp, err := hr.httpDoer.Do(req)
	discardHTTPRespBody(resp)

	if isHTTPResponseError(err) {
		klog.V(1).ErrorS(err, "HTTPS request to lifecycle hook got HTTP response, retrying with HTTP.", "pod", klog.KObj(pod), "host", req.URL.Host)

		req := req.Clone(context.Background())
		req.URL.Scheme = "http"
		req.Header.Del("Authorization")
		resp, httpErr := hr.httpDoer.Do(req)

		// clear err since the fallback succeeded
		if httpErr == nil {
			metrics.LifecycleHandlerHTTPFallbacks.Inc()
			if eventRecorder != nil {
				// report the fallback with an event
				eventRecorder.Event(pod, v1.EventTypeWarning, "LifecycleHTTPFallback", fmt.Sprintf("request to HTTPS lifecycle hook %s got HTTP response, retry with HTTP succeeded", req.URL.Host))
			}
			err = nil
		}
		discardHTTPRespBody(resp)
	}
	return err
}

func discardHTTPRespBody(resp *http.Response) {
	if resp == nil {
		return
	}

	// Ensure the response body is fully read and closed
	// before we reconnect, so that we reuse the same TCP
	// connection.
	defer resp.Body.Close()

	if resp.ContentLength <= maxRespBodyLength {
		io.Copy(io.Discard, &io.LimitedReader{R: resp.Body, N: maxRespBodyLength})
	}
}

// NewAppArmorAdmitHandler returns a PodAdmitHandler which is used to evaluate
// if a pod can be admitted from the perspective of AppArmor.
func NewAppArmorAdmitHandler(validator apparmor.Validator) PodAdmitHandler {
	return &appArmorAdmitHandler{
		Validator: validator,
	}
}

type appArmorAdmitHandler struct {
	apparmor.Validator
}

func (a *appArmorAdmitHandler) Admit(attrs *PodAdmitAttributes) PodAdmitResult {
	// If the pod is already running or terminated, no need to recheck AppArmor.
	if attrs.Pod.Status.Phase != v1.PodPending {
		return PodAdmitResult{Admit: true}
	}

	err := a.Validate(attrs.Pod)
	if err == nil {
		return PodAdmitResult{Admit: true}
	}
	return PodAdmitResult{
		Admit:   false,
		Reason:  AppArmorNotAdmittedReason,
		Message: fmt.Sprintf("Cannot enforce AppArmor: %v", err),
	}
}

func isHTTPResponseError(err error) bool {
	if err == nil {
		return false
	}
	urlErr := &url.Error{}
	if !errors.As(err, &urlErr) {
		return false
	}
	return strings.Contains(urlErr.Err.Error(), "server gave HTTP response to HTTPS client")
}
