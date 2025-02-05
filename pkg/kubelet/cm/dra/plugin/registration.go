/*
Copyright 2022 The Kubernetes Authors.

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

package plugin

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	v1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1beta1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
	drapbv1alpha4 "k8s.io/kubelet/pkg/apis/dra/v1alpha4"
	drapbv1beta1 "k8s.io/kubelet/pkg/apis/dra/v1beta1"
	"k8s.io/kubernetes/pkg/kubelet/pluginmanager/cache"
)

// defaultClientCallTimeout is the default amount of time that a DRA driver has
// to respond to any of the gRPC calls. kubelet uses this value by passing nil
// to RegisterPlugin. Some tests use a different, usually shorter timeout to
// speed up testing.
//
// This is half of the kubelet retry period (according to
// https://github.com/kubernetes/kubernetes/commit/0449cef8fd5217d394c5cd331d852bd50983e6b3).
const defaultClientCallTimeout = 45 * time.Second

// RegistrationHandler is the handler which is fed to the pluginwatcher API.
type RegistrationHandler struct {
	// backgroundCtx is used for all future activities of the handler.
	// This is necessary because it implements APIs which don't
	// provide a context.
	backgroundCtx context.Context
	kubeClient    kubernetes.Interface
	getNode       func() (*v1.Node, error)
	wipingDelay   time.Duration

	mutex sync.Mutex

	// pendingWiping maps a plugin name to a cancel function for
	// wiping of that plugin's ResourceSlices. Entries get added
	// in DeRegisterPlugin and check in RegisterPlugin. If
	// wiping is pending during RegisterPlugin, it gets canceled.
	//
	// Must use pointers to functions because the entries have to
	// be comparable.
	pendingWiping map[string]*context.CancelCauseFunc
}

var _ cache.PluginHandler = &RegistrationHandler{}

// NewPluginHandler returns new registration handler.
//
// Must only be called once per process because it manages global state.
// If a kubeClient is provided, then it synchronizes ResourceSlices
// with the resource information provided by plugins.
func NewRegistrationHandler(kubeClient kubernetes.Interface, getNode func() (*v1.Node, error), wipingDelay time.Duration) *RegistrationHandler {
	handler := &RegistrationHandler{
		// The context and thus logger should come from the caller.
		backgroundCtx: klog.NewContext(context.TODO(), klog.LoggerWithName(klog.TODO(), "DRA registration handler")),
		kubeClient:    kubeClient,
		getNode:       getNode,
		wipingDelay:   wipingDelay,
		pendingWiping: make(map[string]*context.CancelCauseFunc),
	}

	// When kubelet starts up, no DRA driver has registered yet. None of
	// the drivers are usable until they come back, which might not happen
	// at all. Therefore it is better to not advertise any local resources
	// because pods could get stuck on the node waiting for the driver
	// to start up.
	//
	// This has to run in the background.
	logger := klog.LoggerWithName(klog.FromContext(handler.backgroundCtx), "startup")
	ctx := klog.NewContext(handler.backgroundCtx, logger)
	go handler.wipeResourceSlices(ctx, 0 /* no delay */, "" /* all drivers */)

	return handler
}

// wipeResourceSlices deletes ResourceSlices of the node, optionally just for a specific driver.
// Wiping will delay for a while and can be canceled by canceling the context.
func (h *RegistrationHandler) wipeResourceSlices(ctx context.Context, delay time.Duration, driver string) {
	if h.kubeClient == nil {
		return
	}
	logger := klog.FromContext(ctx)

	if delay != 0 {
		// Before we start deleting, give the driver time to bounce back.
		// Perhaps it got removed as part of a DaemonSet update and the
		// replacement pod is about to start.
		logger.V(4).Info("Starting to wait before wiping ResourceSlices", "delay", delay)
		select {
		case <-ctx.Done():
			logger.V(4).Info("Aborting wiping of ResourceSlices", "reason", context.Cause(ctx))
		case <-time.After(delay):
			logger.V(4).Info("Starting to wipe ResourceSlices after waiting", "delay", delay)
		}
	}

	backoff := wait.Backoff{
		Duration: time.Second,
		Factor:   2,
		Jitter:   0.2,
		Cap:      5 * time.Minute,
		Steps:    100,
	}

	// Error logging is done inside the loop. Context cancellation doesn't get logged.
	_ = wait.ExponentialBackoffWithContext(ctx, backoff, func(ctx context.Context) (bool, error) {
		node, err := h.getNode()
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		if err != nil {
			logger.Error(err, "Unexpected error checking for node")
			return false, nil
		}
		fieldSelector := fields.Set{resourceapi.ResourceSliceSelectorNodeName: node.Name}
		if driver != "" {
			fieldSelector[resourceapi.ResourceSliceSelectorDriver] = driver
		}

		err = h.kubeClient.ResourceV1beta1().ResourceSlices().DeleteCollection(ctx, metav1.DeleteOptions{}, metav1.ListOptions{FieldSelector: fieldSelector.String()})
		switch {
		case err == nil:
			logger.V(3).Info("Deleted ResourceSlices", "fieldSelector", fieldSelector)
			return true, nil
		case apierrors.IsUnauthorized(err):
			// This can happen while kubelet is still figuring out
			// its credentials.
			logger.V(5).Info("Deleting ResourceSlice failed, retrying", "fieldSelector", fieldSelector, "err", err)
			return false, nil
		default:
			// Log and retry for other errors.
			logger.V(3).Info("Deleting ResourceSlice failed, retrying", "fieldSelector", fieldSelector, "err", err)
			return false, nil
		}
	})
}

// RegisterPlugin is called when a plugin can be registered.
//
// DRA uses the version array in the registration API to enumerate all gRPC
// services that the plugin provides, using the "<gRPC package name>.<service
// name>" format (e.g. "v1beta1.DRAPlugin"). This allows kubelet to determine
// in advance which version to use resp. which optional services the plugin
// supports.
func (h *RegistrationHandler) RegisterPlugin(pluginName string, endpoint string, supportedServices []string, pluginClientTimeout *time.Duration) error {
	// Prepare a context with its own logger for the plugin.
	//
	// The lifecycle of the plugin's background activities is tied to our
	// root context, so canceling that will also cancel the plugin.
	//
	// The logger injects the plugin name as additional value
	// into all log output related to the plugin.
	ctx := h.backgroundCtx
	logger := klog.FromContext(ctx)
	logger = klog.LoggerWithValues(logger, "pluginName", pluginName, "endpoint", endpoint)
	ctx = klog.NewContext(ctx, logger)

	logger.V(3).Info("Register new DRA plugin")

	chosenService, err := h.validateSupportedServices(pluginName, supportedServices)
	if err != nil {
		return fmt.Errorf("version check of plugin %s failed: %w", pluginName, err)
	}

	var timeout time.Duration
	if pluginClientTimeout == nil {
		timeout = defaultClientCallTimeout
	} else {
		timeout = *pluginClientTimeout
	}

	ctx, cancel := context.WithCancelCause(ctx)

	pluginInstance := &Plugin{
		name:              pluginName,
		backgroundCtx:     ctx,
		cancel:            cancel,
		conn:              nil,
		endpoint:          endpoint,
		chosenService:     chosenService,
		clientCallTimeout: timeout,
	}

	// Storing endpoint of newly registered DRA Plugin into the map, where plugin name will be the key
	// all other DRA components will be able to get the actual socket of DRA plugins by its name.
	if err := draPlugins.add(pluginInstance); err != nil {
		cancel(err)
		// No wrapping, the error already contains details.
		return err
	}

	// Now cancel any pending ResourceSlice wiping for this plugin.
	// Only needs to be done once.
	h.mutex.Lock()
	defer h.mutex.Unlock()
	if cancel := h.pendingWiping[pluginName]; cancel != nil {
		(*cancel)(errors.New("new plugin instance registered"))
		h.pendingWiping[pluginName] = nil
	}

	return nil
}

// validateSupportedServices identifies the highest supported gRPC service for
// NodePrepareResources and NodeUnprepareResources and returns its name
// (e.g. [drapbv1beta1.DRAPluginService]). An error is returned if the plugin
// is unusable.
func (h *RegistrationHandler) validateSupportedServices(pluginName string, supportedServices []string) (string, error) {
	if len(supportedServices) == 0 {
		return "", errors.New("empty list of supported gRPC services (aka supported versions)")
	}

	// Pick most recent version if available.
	chosenService := ""
	for _, service := range []string{
		// Sorted by most recent first, oldest last.
		drapbv1beta1.DRAPluginService,
		drapbv1alpha4.NodeService,
	} {
		if slices.Contains(supportedServices, service) {
			chosenService = service
			break
		}
	}

	// Fall back to alpha if necessary because
	// plugins at that time didn't advertise gRPC services.
	if chosenService == "" {
		chosenService = drapbv1alpha4.NodeService
	}

	return chosenService, nil
}

// DeRegisterPlugin is called when a plugin has removed its socket,
// signaling it is no longer available.
func (h *RegistrationHandler) DeRegisterPlugin(pluginName, endpoint string) {
	if p, last := draPlugins.remove(pluginName, endpoint); p != nil {
		// This logger includes endpoint and pluginName.
		logger := klog.FromContext(p.backgroundCtx)
		logger.V(3).Info("Deregister DRA plugin", "lastInstance", last)
		if !last {
			return
		}

		// Prepare for canceling the background wiping. This needs to run
		// in the context of the registration handler, the one from
		// the plugin is canceled.
		logger = klog.FromContext(h.backgroundCtx)
		logger = klog.LoggerWithName(logger, "driver-cleanup")
		logger = klog.LoggerWithValues(logger, "pluginName", pluginName)
		ctx, cancel := context.WithCancelCause(h.backgroundCtx)
		ctx = klog.NewContext(ctx, logger)

		// Clean up the ResourceSlices for the deleted Plugin since it
		// may have died without doing so itself and might never come
		// back.
		//
		// May get canceled if the plugin comes back quickly enough
		// (see RegisterPlugin).
		h.mutex.Lock()
		defer h.mutex.Unlock()
		if cancel := h.pendingWiping[pluginName]; cancel != nil {
			(*cancel)(errors.New("plugin deregistered a second time"))
		}
		h.pendingWiping[pluginName] = &cancel

		go func() {
			defer func() {
				h.mutex.Lock()
				defer h.mutex.Unlock()

				// Cancel our own context, but remove it from the map only if it
				// is the current entry. Perhaps it already got replaced.
				cancel(errors.New("wiping done"))
				if h.pendingWiping[pluginName] == &cancel {
					h.pendingWiping[pluginName] = nil
				}
			}()
			h.wipeResourceSlices(ctx, h.wipingDelay, pluginName)
		}()
		return
	}

	logger := klog.FromContext(h.backgroundCtx)
	logger.V(3).Info("Deregister DRA plugin not necessary, was already removed")
}

// ValidatePlugin is called by kubelet's plugin watcher upon detection
// of a new registration socket opened by DRA plugin.
func (h *RegistrationHandler) ValidatePlugin(pluginName string, endpoint string, supportedServices []string) error {
	_, err := h.validateSupportedServices(pluginName, supportedServices)
	if err != nil {
		return fmt.Errorf("invalid versions of plugin %s: %w", pluginName, err)
	}

	return err
}
