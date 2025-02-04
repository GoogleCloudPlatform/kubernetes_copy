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
	"fmt"
	"reflect"
	"time"

	v1 "k8s.io/api/coordination/v1"
	v1alpha2 "k8s.io/api/coordination/v1alpha2"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	coordinationv1informers "k8s.io/client-go/informers/coordination/v1"
	coordinationv1alpha2 "k8s.io/client-go/informers/coordination/v1alpha2"
	coordinationv1client "k8s.io/client-go/kubernetes/typed/coordination/v1"
	coordinationv1alpha2client "k8s.io/client-go/kubernetes/typed/coordination/v1alpha2"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
	"k8s.io/utils/clock"
	"k8s.io/utils/ptr"
)

const (
	controllerName = "leader-election-controller"

	// Requeue interval is the interval at which a Lease is requeued to verify that it is
	// being renewed properly.
	defaultRequeueInterval = 5 * time.Second
	noRequeue              = 0

	defaultLeaseDurationSeconds int32 = 5

	electionDuration = 5 * time.Second

	leaseCandidateValidDuration = 30 * time.Minute
)

// Controller is the leader election controller, which observes component identity leases for
// components that have self-nominated as candidate leaders for leases and elects leaders
// for those leases, favoring candidates with higher versions.
type Controller struct {
	leaseInformer     coordinationv1informers.LeaseInformer
	leaseClient       coordinationv1client.CoordinationV1Interface
	leaseRegistration cache.ResourceEventHandlerRegistration

	leaseCandidateInformer     coordinationv1alpha2.LeaseCandidateInformer
	leaseCandidateClient       coordinationv1alpha2client.CoordinationV1alpha2Interface
	leaseCandidateRegistration cache.ResourceEventHandlerRegistration

	queue workqueue.TypedRateLimitingInterface[types.NamespacedName]

	clock clock.Clock
}

func (c *Controller) Run(ctx context.Context, workers int) {
	defer utilruntime.HandleCrash()
	defer c.queue.ShutDown()
	defer func() {
		err := c.leaseInformer.Informer().RemoveEventHandler(c.leaseRegistration)
		if err != nil {
			klog.Warning("error removing leaseInformer eventhandler")
		}
		err = c.leaseCandidateInformer.Informer().RemoveEventHandler(c.leaseCandidateRegistration)
		if err != nil {
			klog.Warning("error removing leaseCandidateInformer eventhandler")
		}
	}()

	if !cache.WaitForNamedCacheSync(controllerName, ctx.Done(), c.leaseRegistration.HasSynced, c.leaseCandidateRegistration.HasSynced) {
		return
	}

	// This controller is leader elected and may start after informers have already started. List on startup.
	lcs, err := c.leaseCandidateInformer.Lister().List(labels.Everything())
	if err != nil {
		utilruntime.HandleError(err)
		return
	}
	for _, lc := range lcs {
		c.enqueueCandidate(lc)
	}

	klog.Infof("Workers: %d", workers)
	for i := 0; i < workers; i++ {
		klog.Infof("Starting worker")
		go wait.UntilWithContext(ctx, c.runElectionWorker, time.Second)
	}
	<-ctx.Done()
}

func NewController(leaseInformer coordinationv1informers.LeaseInformer, leaseCandidateInformer coordinationv1alpha2.LeaseCandidateInformer, leaseClient coordinationv1client.CoordinationV1Interface, leaseCandidateClient coordinationv1alpha2client.CoordinationV1alpha2Interface) (*Controller, error) {
	c := &Controller{
		leaseInformer:          leaseInformer,
		leaseCandidateInformer: leaseCandidateInformer,
		leaseClient:            leaseClient,
		leaseCandidateClient:   leaseCandidateClient,

		queue: workqueue.NewTypedRateLimitingQueueWithConfig(workqueue.DefaultTypedControllerRateLimiter[types.NamespacedName](), workqueue.TypedRateLimitingQueueConfig[types.NamespacedName]{Name: controllerName}),

		clock: clock.RealClock{},
	}
	leaseSynced, err := leaseInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			c.enqueueLease(obj)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			c.enqueueLease(newObj)
		},
		DeleteFunc: func(oldObj interface{}) {
			c.enqueueLease(oldObj)
		},
	})
	if err != nil {
		return nil, err
	}
	leaseCandidateSynced, err := leaseCandidateInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			c.enqueueCandidate(obj)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			c.enqueueCandidate(newObj)
		},
		DeleteFunc: func(oldObj interface{}) {
			c.enqueueCandidate(oldObj)
		},
	})
	if err != nil {
		return nil, err
	}
	c.leaseRegistration = leaseSynced
	c.leaseCandidateRegistration = leaseCandidateSynced
	return c, nil
}

func (c *Controller) runElectionWorker(ctx context.Context) {
	for c.processNextElectionItem(ctx) {
	}
}

func (c *Controller) processNextElectionItem(ctx context.Context) bool {
	key, shutdown := c.queue.Get()
	if shutdown {
		return false
	}

	intervalForRequeue, err := c.reconcileElectionStep(ctx, key)
	utilruntime.HandleError(err)
	if intervalForRequeue != noRequeue {
		defer c.queue.AddAfter(key, intervalForRequeue)
	}
	c.queue.Done(key)
	return true
}

func (c *Controller) enqueueCandidate(obj any) {
	lc, ok := obj.(*v1alpha2.LeaseCandidate)
	if !ok {
		return
	}
	if lc == nil {
		return
	}
	// Ignore candidates that transitioned to Pending because reelection is already in progress
	if lc.Spec.PingTime != nil && lc.Spec.RenewTime.Before(lc.Spec.PingTime) {
		return
	}
	c.queue.Add(types.NamespacedName{Namespace: lc.Namespace, Name: lc.Spec.LeaseName})
}

func (c *Controller) enqueueLease(obj any) {
	lease, ok := obj.(*v1.Lease)
	if !ok {
		return
	}
	c.queue.Add(types.NamespacedName{Namespace: lease.Namespace, Name: lease.Name})
}

func (c *Controller) electionNeeded(candidates []*v1alpha2.LeaseCandidate, leaseNN types.NamespacedName) (bool, error) {
	lease, err := c.leaseInformer.Lister().Leases(leaseNN.Namespace).Get(leaseNN.Name)
	if err != nil && !apierrors.IsNotFound(err) {
		return false, fmt.Errorf("error reading lease: %w", err)
	} else if apierrors.IsNotFound(err) {
		return true, nil
	}

	if isLeaseExpired(c.clock, lease) || lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity == "" {
		return true, nil
	}

	// every 15min enforce an election to update all candidates. Every 30min we garbage collect.
	for _, candidate := range candidates {
		if candidate.Spec.RenewTime != nil && candidate.Spec.RenewTime.Add(leaseCandidateValidDuration/2).Before(c.clock.Now()) {
			return true, nil
		}
	}

	prelimStrategy, err := pickBestStrategy(candidates)
	if err != nil {
		return false, err
	}
	if prelimStrategy != v1.OldestEmulationVersion {
		klog.V(5).Infof("Strategy %q is ignored by CLE", prelimStrategy)
		return false, nil
	}

	prelimElectee := pickBestLeaderOldestEmulationVersion(candidates)
	if prelimElectee == nil {
		return false, nil
	} else if lease != nil && lease.Spec.HolderIdentity != nil && prelimElectee.Name == *lease.Spec.HolderIdentity {
		klog.V(5).Infof("Leader %s is already most optimal for lease %s", prelimElectee.Name, leaseNN)
		return false, nil
	}
	return true, nil
}

// reconcileElectionStep steps through a step in an election.
// A step looks at the current state of Lease and LeaseCandidates and takes one of the following action
// - do nothing (because leader is already optimal or still waiting for an event)
// - request ack from candidates (update LeaseCandidate PingTime)
// - finds the most optimal candidate and elect (update the Lease object)
// Instead of keeping a map and lock on election, the state is
// calculated every time by looking at the lease, and set of available candidates.
// PingTime + electionDuration > time.Now: We just asked all candidates to ack and are still waiting for response
// PingTime + electionDuration < time.Now: Candidate has not responded within the appropriate PingTime. Continue the election.
// RenewTime + 5 seconds > time.Now: All candidates acked in the last 5 seconds, continue the election.
func (c *Controller) reconcileElectionStep(ctx context.Context, leaseNN types.NamespacedName) (requeue time.Duration, err error) {
	candidates, err := c.listAdmissableCandidates(leaseNN)
	if err != nil {
		return defaultRequeueInterval, err
	} else if len(candidates) == 0 {
		return noRequeue, nil
	}
	klog.V(6).Infof("Reconciling election for %s, candidates: %d", leaseNN, len(candidates))

	// Check if an election is really needed by looking at the current lease and candidates
	needElection, err := c.electionNeeded(candidates, leaseNN)
	if !needElection {
		return defaultRequeueInterval, err
	}
	if err != nil {
		return defaultRequeueInterval, err
	}

	now := c.clock.Now()
	canVoteYet := true
	for _, candidate := range candidates {
		if candidate.Spec.PingTime != nil && candidate.Spec.PingTime.Add(electionDuration).After(now) &&
			candidate.Spec.RenewTime != nil && candidate.Spec.RenewTime.Before(candidate.Spec.PingTime) {

			// continue waiting for the election to timeout
			canVoteYet = false
			continue
		}
		if candidate.Spec.RenewTime != nil && candidate.Spec.RenewTime.Add(electionDuration).After(now) {
			continue
		}

		if candidate.Spec.PingTime == nil ||
			// If PingTime is outdated, send another PingTime only if it already acked the first one.
			// This checks for pingTime <= renewTime because equality is possible in unit tests using a fake clock.
			(candidate.Spec.PingTime.Add(electionDuration).Before(now) && !candidate.Spec.RenewTime.Before(candidate.Spec.PingTime)) {
			// TODO(jefftree): We should randomize the order of sending pings and do them in parallel
			// so that all candidates have equal opportunity to ack.
			clone := candidate.DeepCopy()
			clone.Spec.PingTime = &metav1.MicroTime{Time: now}
			_, err := c.leaseCandidateClient.LeaseCandidates(clone.Namespace).Update(ctx, clone, metav1.UpdateOptions{})
			if err != nil {
				return defaultRequeueInterval, err
			}
			canVoteYet = false
		}
	}
	if !canVoteYet {
		return defaultRequeueInterval, nil
	}

	// election is ongoing as long as unexpired PingTimes exist
	for _, candidate := range candidates {
		if candidate.Spec.PingTime == nil {
			continue // shouldn't be the case after the above
		}

		if candidate.Spec.RenewTime != nil && !candidate.Spec.RenewTime.Before(candidate.Spec.PingTime) {
			continue // this has renewed already
		}

		// If a candidate has a PingTime within the election duration, they have not acked
		// and we should wait until we receive their response
		if candidate.Spec.PingTime.Add(electionDuration).After(now) {
			// continue waiting for the election to timeout
			return noRequeue, nil
		}
	}

	var ackedCandidates []*v1alpha2.LeaseCandidate
	for _, candidate := range candidates {
		if candidate.Spec.RenewTime.Add(electionDuration).After(now) {
			ackedCandidates = append(ackedCandidates, candidate)
		}
	}
	if len(ackedCandidates) == 0 {
		return noRequeue, fmt.Errorf("no available candidates")
	}

	strategy, err := pickBestStrategy(ackedCandidates)
	if err != nil {
		return noRequeue, err
	}

	leaderLease := &v1.Lease{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: leaseNN.Namespace,
			Name:      leaseNN.Name,
		},
		Spec: v1.LeaseSpec{
			Strategy:             &strategy,
			LeaseDurationSeconds: ptr.To(defaultLeaseDurationSeconds),
			RenewTime:            &metav1.MicroTime{Time: c.clock.Now()},
		},
	}

	switch strategy {
	case v1.OldestEmulationVersion:
		electee := pickBestLeaderOldestEmulationVersion(ackedCandidates)
		if electee == nil {
			return noRequeue, fmt.Errorf("should not happen, could not find suitable electee")
		}
		leaderLease.Spec.HolderIdentity = &electee.Name
	default:
		// do not set the holder identity, but leave it to some other controller. But fall
		// through to create the lease (without holder).
		klog.V(2).Infof("Election for strategy %q is not handled by %s", strategy, controllerName)
	}

	// create the leader election lease
	_, err = c.leaseClient.Leases(leaseNN.Namespace).Create(ctx, leaderLease, metav1.CreateOptions{})
	if err == nil {
		if leaderLease.Spec.HolderIdentity != nil {
			klog.Infof("Created lease %s for %q", leaseNN, *leaderLease.Spec.HolderIdentity)
		} else {
			klog.Infof("Created lease %s without leader", leaseNN)
		}
		return defaultRequeueInterval, nil
	} else if !apierrors.IsAlreadyExists(err) {
		return noRequeue, err
	}

	// Get existing lease
	existing, err := c.leaseClient.Leases(leaseNN.Namespace).Get(ctx, leaseNN.Name, metav1.GetOptions{})
	if err != nil {
		return noRequeue, err
	}
	orig := existing.DeepCopy()

	isExpired := isLeaseExpired(c.clock, existing)
	noHolderIdentity := leaderLease.Spec.HolderIdentity != nil && (existing.Spec.HolderIdentity == nil || *existing.Spec.HolderIdentity == "")
	expiredAndNewHolder := isExpired && leaderLease.Spec.HolderIdentity != nil && *existing.Spec.HolderIdentity != *leaderLease.Spec.HolderIdentity
	strategyChanged := existing.Spec.Strategy == nil || *existing.Spec.Strategy != strategy
	differentHolder := leaderLease.Spec.HolderIdentity != nil && *leaderLease.Spec.HolderIdentity != *existing.Spec.HolderIdentity

	// Update lease
	if strategyChanged {
		klog.Infof("Lease %s strategy changed to %q", leaseNN, strategy)
		existing.Spec.Strategy = &strategy
	}
	if noHolderIdentity || expiredAndNewHolder {
		if noHolderIdentity {
			klog.Infof("Lease %s had no holder, setting holder to %q", leaseNN, *leaderLease.Spec.HolderIdentity)
		} else {
			klog.Infof("Lease %s expired, resetting it and setting holder to %q", leaseNN, *leaderLease.Spec.HolderIdentity)
		}

		existing.Spec.PreferredHolder = nil
		existing.Spec.HolderIdentity = leaderLease.Spec.HolderIdentity
		existing.Spec.RenewTime = &metav1.MicroTime{Time: time.Now()}
		existing.Spec.LeaseDurationSeconds = ptr.To(defaultLeaseDurationSeconds)
		existing.Spec.AcquireTime = nil
	} else if differentHolder {
		klog.Infof("Lease %s holder changed from %q to %q", leaseNN, *existing.Spec.HolderIdentity, *leaderLease.Spec.HolderIdentity)
		existing.Spec.PreferredHolder = leaderLease.Spec.HolderIdentity
	}

	if reflect.DeepEqual(existing, orig) {
		leader := ""
		if existing.Spec.HolderIdentity != nil {
			leader = *existing.Spec.HolderIdentity
			klog.V(5).Infof("Lease %s is managed by a third party strategy", leader)
		} else {
			klog.V(5).Infof("Lease %s already has the most optimal leader %q", leaseNN, leader)
		}
		// We need to requeue to ensure that we are aware of an expired lease
		return defaultRequeueInterval, nil
	}

	_, err = c.leaseClient.Leases(leaseNN.Namespace).Update(ctx, existing, metav1.UpdateOptions{})
	if err != nil {
		return noRequeue, err
	}

	return defaultRequeueInterval, nil
}

func (c *Controller) listAdmissableCandidates(leaseNN types.NamespacedName) ([]*v1alpha2.LeaseCandidate, error) {
	leases, err := c.leaseCandidateInformer.Lister().LeaseCandidates(leaseNN.Namespace).List(labels.Everything())
	if err != nil {
		return nil, err
	}
	var results []*v1alpha2.LeaseCandidate
	for _, l := range leases {
		if l.Spec.LeaseName != leaseNN.Name {
			continue
		}
		if !isLeaseCandidateExpired(c.clock, l) {
			results = append(results, l)
		} else {
			klog.Infof("LeaseCandidate %s is expired", l.Name)
		}
	}
	return results, nil
}
