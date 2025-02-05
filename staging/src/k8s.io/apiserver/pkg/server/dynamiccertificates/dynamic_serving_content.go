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
	"context"
	"crypto/tls"
	"fmt"
	"os"
	"sync/atomic"
	"time"

	"github.com/fsnotify/fsnotify"

	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
)

// DynamicCertKeyPairContent provides a CertKeyContentProvider that can dynamically react to new file content
type DynamicCertKeyPairContent struct {
	name string

	// certFile is the name of the certificate file to read.
	certFile string
	// keyFile is the name of the key file to read.
	keyFile string

	// certKeyPair is a certKeyContent that contains the last read, non-zero length content of the key and cert
	certKeyPair atomic.Value

	listeners []Listener

	// queue only ever has one item, but it has nice error handling backoff/retry semantics
	queue workqueue.TypedRateLimitingInterface[string]
}

var _ CertKeyContentProvider = &DynamicCertKeyPairContent{}
var _ ControllerRunner = &DynamicCertKeyPairContent{}

// NewDynamicServingContentFromFiles returns a dynamic CertKeyContentProvider based on a cert and key filename
func NewDynamicServingContentFromFiles(ctx context.Context, purpose, certFile, keyFile string) (*DynamicCertKeyPairContent, error) {
	if len(certFile) == 0 || len(keyFile) == 0 {
		return nil, fmt.Errorf("missing filename for serving cert")
	}
	name := fmt.Sprintf("%s::%s::%s", purpose, certFile, keyFile)

	ret := &DynamicCertKeyPairContent{
		name:     name,
		certFile: certFile,
		keyFile:  keyFile,
		queue: workqueue.NewTypedRateLimitingQueueWithConfig(
			workqueue.DefaultTypedControllerRateLimiter[string](),
			workqueue.TypedRateLimitingQueueConfig[string]{Name: fmt.Sprintf("DynamicCABundle-%s", purpose)},
		),
	}

	if err := ret.loadCertKeyPair(ctx); err != nil {
		return nil, err
	}

	return ret, nil
}

// AddListener adds a listener to be notified when the serving cert content changes.
func (c *DynamicCertKeyPairContent) AddListener(listener Listener) {
	c.listeners = append(c.listeners, listener)
}

// loadCertKeyPair determines the next set of content for the file.
func (c *DynamicCertKeyPairContent) loadCertKeyPair(ctx context.Context) error {
	cert, err := os.ReadFile(c.certFile)
	if err != nil {
		return err
	}
	key, err := os.ReadFile(c.keyFile)
	if err != nil {
		return err
	}
	if len(cert) == 0 || len(key) == 0 {
		return fmt.Errorf("missing content for serving cert %q", c.Name())
	}

	// Ensure that the key matches the cert and both are valid
	_, err = tls.X509KeyPair(cert, key)
	if err != nil {
		return err
	}

	newCertKey := &certKeyContent{
		cert: cert,
		key:  key,
	}

	// check to see if we have a change. If the values are the same, do nothing.
	existing, ok := c.certKeyPair.Load().(*certKeyContent)
	if ok && existing != nil && existing.Equal(newCertKey) {
		return nil
	}

	c.certKeyPair.Store(newCertKey)
	logger := klog.FromContext(ctx)
	logger.V(2).Info("Loaded a new cert/key pair", "name", c.Name())

	for _, listener := range c.listeners {
		listener.Enqueue()
	}

	return nil
}

// RunOnce runs a single sync loop
func (c *DynamicCertKeyPairContent) RunOnce(ctx context.Context) error {
	return c.loadCertKeyPair(ctx)
}

// Run starts the controller and blocks until context is killed.
func (c *DynamicCertKeyPairContent) Run(ctx context.Context, workers int) {
	defer utilruntime.HandleCrashWithContext(ctx)
	defer c.queue.ShutDown()

	logger := klog.FromContext(ctx)
	logger.Info("Starting controller", "name", c.name)
	defer logger.Info("Shutting down controller", "name", c.name)

	// doesn't matter what workers say, only start one.
	go wait.UntilWithContext(ctx, func(ctx context.Context) {
		c.runWorker(ctx)
	}, time.Second)

	// start the loop that watches the cert and key files until stopCh is closed.
	go wait.UntilWithContext(ctx, func(ctx context.Context) {
		if err := c.watchCertKeyFile(ctx); err != nil {
			logger.Error(err, "Failed to watch cert and key file, will retry later")
		}
	}, time.Minute)

	<-ctx.Done()
}

func (c *DynamicCertKeyPairContent) watchCertKeyFile(ctx context.Context) error {
	// Trigger a check here to ensure the content will be checked periodically even if the following watch fails.
	c.queue.Add(workItemKey)

	w, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("error creating fsnotify watcher: %v", err)
	}
	defer w.Close()

	if err := w.Add(c.certFile); err != nil {
		return fmt.Errorf("error adding watch for file %s: %v", c.certFile, err)
	}
	if err := w.Add(c.keyFile); err != nil {
		return fmt.Errorf("error adding watch for file %s: %v", c.keyFile, err)
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

// handleWatchEvent triggers reloading the cert and key file, and restarts a new watch if it's a Remove or Rename event.
// If one file is updated before the other, the loadCertKeyPair method will catch the mismatch and will not apply the
// change. When an event of the other file is received, it will trigger reloading the files again and the new content
// will be loaded and used.
func (c *DynamicCertKeyPairContent) handleWatchEvent(ctx context.Context, e fsnotify.Event, w *fsnotify.Watcher) error {
	// This should be executed after restarting the watch (if applicable) to ensure no file event will be missing.
	defer c.queue.Add(workItemKey)
	if !e.Has(fsnotify.Remove) && !e.Has(fsnotify.Rename) {
		return nil
	}
	if err := w.Remove(e.Name); err != nil {
		logger := klog.FromContext(ctx)
		logger.Info("Failed to remove file watch, it may have been deleted", "file", e.Name, "err", err)
	}
	if err := w.Add(e.Name); err != nil {
		return fmt.Errorf("error adding watch for file %s: %v", e.Name, err)
	}
	return nil
}

func (c *DynamicCertKeyPairContent) runWorker(ctx context.Context) {
	for c.processNextWorkItem(ctx) {
	}
}

func (c *DynamicCertKeyPairContent) processNextWorkItem(ctx context.Context) bool {
	dsKey, quit := c.queue.Get()
	if quit {
		return false
	}
	defer c.queue.Done(dsKey)

	err := c.loadCertKeyPair(ctx)
	if err == nil {
		c.queue.Forget(dsKey)
		return true
	}

	utilruntime.HandleError(fmt.Errorf("%v failed with : %v", dsKey, err))
	c.queue.AddRateLimited(dsKey)

	return true
}

// Name is just an identifier
func (c *DynamicCertKeyPairContent) Name() string {
	return c.name
}

// CurrentCertKeyContent provides cert and key byte content
func (c *DynamicCertKeyPairContent) CurrentCertKeyContent() ([]byte, []byte) {
	certKeyContent := c.certKeyPair.Load().(*certKeyContent)
	return certKeyContent.cert, certKeyContent.key
}
