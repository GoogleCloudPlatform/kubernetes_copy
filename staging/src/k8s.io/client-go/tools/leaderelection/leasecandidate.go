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

package leaderelection

import (
	"context"
	"reflect"
	"time"

	v1 "k8s.io/api/coordination/v1"
	v1alpha2 "k8s.io/api/coordination/v1alpha2"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	coordinationv1alpha2client "k8s.io/client-go/kubernetes/typed/coordination/v1alpha2"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
	"k8s.io/utils/clock"
)

const requeueInterval = 5 * time.Minute

type CacheSyncWaiter interface {
	WaitForCacheSync(stopCh <-chan struct{}) map[reflect.Type]bool
}

type LeaseCandidate struct {
	leaseClient            coordinationv1alpha2client.LeaseCandidateInterface
	leaseCandidateInformer cache.SharedIndexInformer
	informerFactory        informers.SharedInformerFactory
	hasSynced              cache.InformerSynced

	// At most there will be one item in this Queue (since we only watch one item)
	queue workqueue.TypedRateLimitingInterface[int]

	name      string
	namespace string

	// controller lease
	leaseName string

	clock clock.Clock

	binaryVersion, emulationVersion string
	strategy                        v1.CoordinatedLeaseStrategy
}

// NewCandidate creates new LeaseCandidate controller that creates a
// LeaseCandidate object if it does not exist and watches changes
// to the corresponding object and renews if PingTime is set.
// WARNING: This is an ALPHA feature. Ensure that the CoordinatedLeaderElection
// feature gate is on.
func NewCandidate(clientset kubernetes.Interface,
	candidateNamespace string,
	candidateName string,
	targetLease string,
	binaryVersion, emulationVersion string,
	strategy v1.CoordinatedLeaseStrategy,
) (*LeaseCandidate, CacheSyncWaiter, error) {
	fieldSelector := fields.OneTermEqualSelector("metadata.name", candidateName).String()
	// A separate informer factory is required because this must start before informerFactories
	// are started for leader elected components
	informerFactory := informers.NewSharedInformerFactoryWithOptions(
		clientset, 5*time.Minute,
		informers.WithTweakListOptions(func(options *metav1.ListOptions) {
			options.FieldSelector = fieldSelector
		}),
	)
	leaseCandidateInformer := informerFactory.Coordination().V1alpha2().LeaseCandidates().Informer()

	lc := &LeaseCandidate{
		leaseClient:            clientset.CoordinationV1alpha2().LeaseCandidates(candidateNamespace),
		leaseCandidateInformer: leaseCandidateInformer,
		informerFactory:        informerFactory,
		name:                   candidateName,
		namespace:              candidateNamespace,
		leaseName:              targetLease,
		clock:                  clock.RealClock{},
		binaryVersion:          binaryVersion,
		emulationVersion:       emulationVersion,
		strategy:               strategy,
	}
	lc.queue = workqueue.NewTypedRateLimitingQueueWithConfig(workqueue.DefaultTypedControllerRateLimiter[int](), workqueue.TypedRateLimitingQueueConfig[int]{Name: "leasecandidate"})

	h, err := leaseCandidateInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		UpdateFunc: func(oldObj, newObj interface{}) {
			if leasecandidate, ok := newObj.(*v1alpha2.LeaseCandidate); ok {
				if leasecandidate.Spec.PingTime != nil && leasecandidate.Spec.PingTime.After(leasecandidate.Spec.RenewTime.Time) {
					lc.enqueueLease()
				}
			}
		},
	})
	if err != nil {
		return nil, nil, err
	}
	lc.hasSynced = h.HasSynced

	return lc, informerFactory, nil
}

func (c *LeaseCandidate) Run(ctx context.Context) {
	defer c.queue.ShutDown()

	logger := klog.FromContext(ctx)
	logger = klog.LoggerWithName(logger, "leasecandidate")
	ctx = klog.NewContext(ctx, logger)

	c.informerFactory.Start(ctx.Done())
	if !cache.WaitForNamedCacheSyncWithContext(ctx, c.hasSynced) {
		return
	}

	c.enqueueLease()
	go c.runWorker(ctx)
	<-ctx.Done()
}

func (c *LeaseCandidate) runWorker(ctx context.Context) {
	for c.processNextWorkItem(ctx) {
	}
}

func (c *LeaseCandidate) processNextWorkItem(ctx context.Context) bool {
	key, shutdown := c.queue.Get()
	if shutdown {
		return false
	}
	defer c.queue.Done(key)

	err := c.ensureLease(ctx)
	if err == nil {
		c.queue.AddAfter(key, requeueInterval)
		return true
	}

	utilruntime.HandleErrorWithContext(ctx, err, "Ensuring lease failed")
	c.queue.AddRateLimited(key)

	return true
}

func (c *LeaseCandidate) enqueueLease() {
	c.queue.Add(0)
}

// ensureLease creates the lease if it does not exist and renew it if it exists. Returns the lease and
// a bool (true if this call created the lease), or any error that occurs.
func (c *LeaseCandidate) ensureLease(ctx context.Context) error {
	logger := klog.FromContext(ctx)
	lease, err := c.leaseClient.Get(ctx, c.name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		logger.V(2).Info("Creating lease candidate")
		// lease does not exist, create it.
		leaseToCreate := c.newLeaseCandidate()
		if _, err := c.leaseClient.Create(ctx, leaseToCreate, metav1.CreateOptions{}); err != nil {
			return err
		}
		logger.V(2).Info("Created lease candidate")
		return nil
	} else if err != nil {
		return err
	}
	logger.V(2).Info("Lease candidate exists. Renewing.")
	clone := lease.DeepCopy()
	clone.Spec.RenewTime = &metav1.MicroTime{Time: c.clock.Now()}
	_, err = c.leaseClient.Update(ctx, clone, metav1.UpdateOptions{})
	if err != nil {
		return err
	}
	return nil
}

func (c *LeaseCandidate) newLeaseCandidate() *v1alpha2.LeaseCandidate {
	lc := &v1alpha2.LeaseCandidate{
		ObjectMeta: metav1.ObjectMeta{
			Name:      c.name,
			Namespace: c.namespace,
		},
		Spec: v1alpha2.LeaseCandidateSpec{
			LeaseName:        c.leaseName,
			BinaryVersion:    c.binaryVersion,
			EmulationVersion: c.emulationVersion,
			Strategy:         c.strategy,
		},
	}
	lc.Spec.RenewTime = &metav1.MicroTime{Time: c.clock.Now()}
	return lc
}
