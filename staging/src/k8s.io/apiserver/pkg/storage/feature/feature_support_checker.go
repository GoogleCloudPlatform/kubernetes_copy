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

package feature

import (
	"context"
	"fmt"
	"sync"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/version"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apiserver/pkg/storage"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"
)

var (
	// Define these static versions to use for checking version of etcd, issue on kubernetes #123192
	v3_4_31 = version.MustParseSemantic("3.4.31")
	v3_5_0  = version.MustParseSemantic("3.5.0")
	v3_5_13 = version.MustParseSemantic("3.5.13")

	// DefaultFeatureSupportChecker is a shared global etcd FeatureSupportChecker.
	DefaultFeatureSupportChecker FeatureSupportChecker = newDefaultFeatureSupportChecker()
)

// FeatureSupportChecker to define Supports functions.
type FeatureSupportChecker interface {
	// Supports check if the feature is supported or not by checking internal cache.
	// By default all calls to this function before calling CheckClient returns false.
	// Returns true if all endpoints in etcd clients are supporting the feature.
	// If client A supports and client B doesn't support the feature, the `Supports` will
	// first return true at client A initialization and then return false on client B
	// initialization, it can flip the support at runtime.
	Supports(feature storage.Feature) bool
	// CheckClient works with etcd client to recalculate feature support and cache it internally.
	// All etcd clients should support feature to cause `Supports` return true.
	// If client A supports and client B doesn't support the feature, the `Supports` will
	// first return true at client A initialization and then return false on client B
	// initialization, it can flip the support at runtime.
	CheckClient(ctx context.Context, c client, feature storage.Feature)
}

type defaultFeatureSupportChecker struct {
	lock                    sync.Mutex
	progressNotifySupported *bool
	checkingEndpoint        map[string]struct{}
}

func newDefaultFeatureSupportChecker() *defaultFeatureSupportChecker {
	return &defaultFeatureSupportChecker{
		checkingEndpoint: make(map[string]struct{}),
	}
}

// Supports can check the feature from anywhere without storage if it was cached before.
func (f *defaultFeatureSupportChecker) Supports(feature storage.Feature) bool {
	switch feature {
	case storage.RequestWatchProgress:
		f.lock.Lock()
		defer f.lock.Unlock()

		return ptr.Deref(f.progressNotifySupported, false)
	default:
		runtime.HandleError(fmt.Errorf("feature %q is not implemented in DefaultFeatureSupportChecker", feature))
		return false
	}
}

// CheckClient accepts client and calculate the support per endpoint and caches it.
func (f *defaultFeatureSupportChecker) CheckClient(ctx context.Context, c client, feature storage.Feature) {
	switch feature {
	case storage.RequestWatchProgress:
		f.checkClient(ctx, c)
	default:
		runtime.HandleError(fmt.Errorf("feature %q is not implemented in DefaultFeatureSupportChecker", feature))
	}
}

func (f *defaultFeatureSupportChecker) checkClient(ctx context.Context, c client) {
	// start with 10 ms, multiply by 2 each step, until 15 s and stays on 15 seconds.
	delayFunc := wait.Backoff{
		Duration: 10 * time.Millisecond,
		Cap:      15 * time.Second,
		Factor:   2.0,
		Steps:    11}.DelayFunc()
	f.lock.Lock()
	defer f.lock.Unlock()
	for _, ep := range c.Endpoints() {
		if _, found := f.checkingEndpoint[ep]; found {
			continue
		}
		f.checkingEndpoint[ep] = struct{}{}
		go func(ep string) {
			defer runtime.HandleCrash()
			err := delayFunc.Until(ctx, true, true, func(ctx context.Context) (done bool, err error) {
				internalErr := f.clientSupportsRequestWatchProgress(ctx, c, ep)
				return internalErr == nil, nil
			})
			if err != nil {
				klog.ErrorS(err, "Failed to check if RequestWatchProgress is supported by etcd after retrying")
			}
		}(ep)
	}
}

func (f *defaultFeatureSupportChecker) clientSupportsRequestWatchProgress(ctx context.Context, c client, ep string) error {
	supported, err := endpointSupportsRequestWatchProgress(ctx, c, ep)
	if err != nil {
		return err
	}
	f.lock.Lock()
	defer f.lock.Unlock()

	if !supported {
		klog.Infof("RequestWatchProgress feature is not supported by %q endpoint", ep)
		f.progressNotifySupported = ptr.To(false)
		return nil
	}
	if f.progressNotifySupported == nil {
		f.progressNotifySupported = ptr.To(true)
	}
	return nil
}

// Sub interface of etcd client.
type client interface {
	// Endpoints returns list of endpoints in etcd client.
	Endpoints() []string
	// Status retrieves the status information from the etcd client connected to the specified endpoint.
	// It takes a context.Context parameter for cancellation or timeout control.
	// It returns a clientv3.StatusResponse containing the status information or an error if the operation fails.
	Status(ctx context.Context, endpoint string) (*clientv3.StatusResponse, error)
}

// endpointSupportsRequestWatchProgress evaluates whether RequestWatchProgress supported by current version of etcd endpoint.
// Based on this issues:
//   - https://github.com/etcd-io/etcd/issues/15220 - Fixed in etcd v3.4.25+ and v3.5.8+
//   - https://github.com/etcd-io/etcd/issues/17507 - Fixed in etcd v3.4.31+ and v3.5.13+
func endpointSupportsRequestWatchProgress(ctx context.Context, c client, endpoint string) (bool, error) {
	resp, err := c.Status(ctx, endpoint)
	if err != nil {
		return false, fmt.Errorf("failed checking etcd version, endpoint: %q: %w", endpoint, err)
	}
	ver, err := version.ParseSemantic(resp.Version)
	if err != nil {
		// Assume feature is not supported if etcd version cannot be parsed.
		klog.ErrorS(err, "Failed to parse etcd version", "version", resp.Version)
		return false, nil
	}
	if ver.LessThan(v3_4_31) || ver.AtLeast(v3_5_0) && ver.LessThan(v3_5_13) {
		return false, nil
	}
	return true, nil
}
