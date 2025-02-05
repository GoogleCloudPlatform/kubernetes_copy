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

package container

import (
	"context"
	"k8s.io/klog/v2"
)

// GC manages garbage collection of dead containers.
//
// Implementation is thread-compatible.
type GC interface {
	// Garbage collect containers.
	GarbageCollect(ctx context.Context) error
	// Deletes all unused containers, including containers belonging to pods that are terminated but not deleted
	DeleteAllUnusedContainers(ctx context.Context) error
}

// SourcesReadyProvider knows how to determine if configuration sources are ready
type SourcesReadyProvider interface {
	// AllReady returns true if the currently configured sources have all been seen.
	AllReady() bool
}

// TODO(vmarmol): Preferentially remove pod infra containers.
type realContainerGC struct {
	// Container runtime
	runtime Runtime

	// sourcesReadyProvider provides the readiness of kubelet configuration sources.
	sourcesReadyProvider SourcesReadyProvider
}

// NewContainerGC creates a new instance of GC with the specified policy.
func NewContainerGC(runtime Runtime, sourcesReadyProvider SourcesReadyProvider) (GC, error) {
	return &realContainerGC{
		runtime:              runtime,
		sourcesReadyProvider: sourcesReadyProvider,
	}, nil
}

func (cgc *realContainerGC) GarbageCollect(ctx context.Context) error {
	return cgc.runtime.GarbageCollect(ctx, cgc.sourcesReadyProvider.AllReady(), false)
}

func (cgc *realContainerGC) DeleteAllUnusedContainers(ctx context.Context) error {
	klog.InfoS("Attempting to delete unused containers")
	return cgc.runtime.GarbageCollect(ctx, cgc.sourcesReadyProvider.AllReady(), true)
}
