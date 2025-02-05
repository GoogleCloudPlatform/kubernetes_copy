/*
Copyright 2019 The Kubernetes Authors.

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

package dynamiccertificates

import (
	"bytes"
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"sync/atomic"
	"time"

	"github.com/fsnotify/fsnotify"
	"k8s.io/client-go/util/cert"

	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
)

// FileRefreshDuration is exposed so that integration tests can crank up the reload speed.
var FileRefreshDuration = 1 * time.Minute

// ControllerRunner is a generic interface for starting a controller
type ControllerRunner interface {
	// RunOnce runs the sync loop a single time.  This useful for synchronous priming
	RunOnce(ctx context.Context) error

	// Run should be called a go .Run
	Run(ctx context.Context, workers int)
}

// DynamicFileCAContent provides a CAContentProvider that can dynamically react to new file content
// It also fulfills the authenticator interface to provide verifyoptions
type DynamicFileCAContent struct {
	name string

	// filename is the name the file to read.
	filename string

	// caBundle is a caBundleAndVerifier that contains the last read, non-zero length content of the file
	caBundle atomic.Value

	listeners []Listener

	// queue only ever has one item, but it has nice error handling backoff/retry semantics
	queue workqueue.TypedRateLimitingInterface[string]
}

var _ Notifier = &DynamicFileCAContent{}
var _ CAContentProvider = &DynamicFileCAContent{}
var _ ControllerRunner = &DynamicFileCAContent{}

type caBundleAndVerifier struct {
	caBundle      []byte
	verifyOptions x509.VerifyOptions
}

// NewDynamicCAContentFromFile returns a CAContentProvider based on a filename that automatically reloads content
func NewDynamicCAContentFromFile(ctx context.Context, purpose, filename string) (*DynamicFileCAContent, error) {
	if len(filename) == 0 {
		return nil, fmt.Errorf("missing filename for ca bundle")
	}
	name := fmt.Sprintf("%s::%s", purpose, filename)

	ret := &DynamicFileCAContent{
		name:     name,
		filename: filename,
		queue: workqueue.NewTypedRateLimitingQueueWithConfig(
			workqueue.DefaultTypedControllerRateLimiter[string](),
			workqueue.TypedRateLimitingQueueConfig[string]{Name: fmt.Sprintf("DynamicCABundle-%s", purpose)},
		),
	}
	if err := ret.loadCABundle(ctx); err != nil {
		return nil, err
	}

	return ret, nil
}

// AddListener adds a listener to be notified when the CA content changes.
func (c *DynamicFileCAContent) AddListener(listener Listener) {
	c.listeners = append(c.listeners, listener)
}

// loadCABundle determines the next set of content for the file.
func (c *DynamicFileCAContent) loadCABundle(ctx context.Context) error {
	caBundle, err := os.ReadFile(c.filename)
	if err != nil {
		return err
	}
	if len(caBundle) == 0 {
		return fmt.Errorf("missing content for CA bundle %q", c.Name())
	}

	// check to see if we have a change. If the values are the same, do nothing.
	if !c.hasCAChanged(caBundle) {
		return nil
	}

	caBundleAndVerifier, err := newCABundleAndVerifier(c.Name(), caBundle)
	if err != nil {
		return err
	}
	c.caBundle.Store(caBundleAndVerifier)
	logger := klog.FromContext(ctx)
	logger.V(2).Info("Loaded a new CA Bundle and Verifier", "name", c.Name())

	for _, listener := range c.listeners {
		listener.Enqueue()
	}

	return nil
}

// hasCAChanged returns true if the caBundle is different than the current.
func (c *DynamicFileCAContent) hasCAChanged(caBundle []byte) bool {
	uncastExisting := c.caBundle.Load()
	if uncastExisting == nil {
		return true
	}

	// check to see if we have a change. If the values are the same, do nothing.
	existing, ok := uncastExisting.(*caBundleAndVerifier)
	if !ok {
		return true
	}
	if !bytes.Equal(existing.caBundle, caBundle) {
		return true
	}

	return false
}

// RunOnce runs a single sync loop
func (c *DynamicFileCAContent) RunOnce(ctx context.Context) error {
	return c.loadCABundle(ctx)
}

// Run starts the controller and blocks until stopCh is closed.
func (c *DynamicFileCAContent) Run(ctx context.Context, workers int) {
	defer utilruntime.HandleCrash()
	defer c.queue.ShutDown()

	logger := klog.FromContext(ctx)
	logger.Info("Starting controller", "name", c.name)
	defer logger.Info("Shutting down controller", "name", c.name)

	// doesn't matter what workers say, only start one.
	go wait.UntilWithContext(ctx, func(ctx context.Context) {
		c.runWorker(ctx)
	}, time.Second)

	// start the loop that watches the CA file until stopCh is closed.
	go wait.Until(func() {
		if err := c.watchCAFile(ctx); err != nil {
			logger.Error(err, "Failed to watch CA file, will retry later")
		}
	}, time.Minute, ctx.Done())

	<-ctx.Done()
}

func (c *DynamicFileCAContent) watchCAFile(ctx context.Context) error {
	// Trigger a check here to ensure the content will be checked periodically even if the following watch fails.
	c.queue.Add(workItemKey)

	w, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("error creating fsnotify watcher: %v", err)
	}
	defer w.Close()

	if err = w.Add(c.filename); err != nil {
		return fmt.Errorf("error adding watch for file %s: %v", c.filename, err)
	}
	// Trigger a check in case the file is updated before the watch starts.
	c.queue.Add(workItemKey)

	for {
		select {
		case e := <-w.Events:
			if err := c.handleWatchEvent(ctx, e, w); err != nil {
				return err
			}
		case err := <-w.Errors:
			return fmt.Errorf("received fsnotify error: %v", err)
		case <-ctx.Done():
			return nil
		}
	}
}

// handleWatchEvent triggers reloading the CA file, and restarts a new watch if it's a Remove or Rename event.
func (c *DynamicFileCAContent) handleWatchEvent(ctx context.Context, e fsnotify.Event, w *fsnotify.Watcher) error {
	// This should be executed after restarting the watch (if applicable) to ensure no file event will be missing.
	defer c.queue.Add(workItemKey)
	if !e.Has(fsnotify.Remove) && !e.Has(fsnotify.Rename) {
		return nil
	}
	if err := w.Remove(c.filename); err != nil && !errors.Is(err, fsnotify.ErrNonExistentWatch) {
		logger := klog.FromContext(ctx)
		logger.Info("Failed to remove file watch, it may have been deleted", "file", c.filename, "err", err)
	}
	if err := w.Add(c.filename); err != nil {
		return fmt.Errorf("error adding watch for file %s: %v", c.filename, err)
	}
	return nil
}

func (c *DynamicFileCAContent) runWorker(ctx context.Context) {
	for c.processNextWorkItem(ctx) {
	}
}

func (c *DynamicFileCAContent) processNextWorkItem(ctx context.Context) bool {
	dsKey, quit := c.queue.Get()
	if quit {
		return false
	}
	defer c.queue.Done(dsKey)

	err := c.loadCABundle(ctx)
	if err == nil {
		c.queue.Forget(dsKey)
		return true
	}

	utilruntime.HandleError(fmt.Errorf("%v failed with : %v", dsKey, err))
	c.queue.AddRateLimited(dsKey)

	return true
}

// Name is just an identifier
func (c *DynamicFileCAContent) Name() string {
	return c.name
}

// CurrentCABundleContent provides ca bundle byte content
func (c *DynamicFileCAContent) CurrentCABundleContent() (cabundle []byte) {
	return c.caBundle.Load().(*caBundleAndVerifier).caBundle
}

// VerifyOptions provides verifyoptions compatible with authenticators
func (c *DynamicFileCAContent) VerifyOptions() (x509.VerifyOptions, bool) {
	uncastObj := c.caBundle.Load()
	if uncastObj == nil {
		return x509.VerifyOptions{}, false
	}

	return uncastObj.(*caBundleAndVerifier).verifyOptions, true
}

// newVerifyOptions creates a new verification func from a file.  It reads the content and then fails.
// It will return a nil function if you pass an empty CA file.
func newCABundleAndVerifier(name string, caBundle []byte) (*caBundleAndVerifier, error) {
	if len(caBundle) == 0 {
		return nil, fmt.Errorf("missing content for CA bundle %q", name)
	}

	// Wrap with an x509 verifier
	var err error
	verifyOptions := defaultVerifyOptions()
	verifyOptions.Roots, err = cert.NewPoolFromBytes(caBundle)
	if err != nil {
		return nil, fmt.Errorf("error loading CA bundle for %q: %v", name, err)
	}

	return &caBundleAndVerifier{
		caBundle:      caBundle,
		verifyOptions: verifyOptions,
	}, nil
}

// defaultVerifyOptions returns VerifyOptions that use the system root certificates, current time,
// and requires certificates to be valid for client auth (x509.ExtKeyUsageClientAuth)
func defaultVerifyOptions() x509.VerifyOptions {
	return x509.VerifyOptions{
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
}
