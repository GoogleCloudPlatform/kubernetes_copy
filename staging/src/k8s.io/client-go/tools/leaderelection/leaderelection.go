/*
Copyright 2015 The Kubernetes Authors.

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

// Package leaderelection implements leader election of a set of endpoints.
// It uses an annotation in the endpoints object to store the record of the
// election state. This implementation does not guarantee that only one
// client is acting as a leader (a.k.a. fencing).
//
// A client only acts on timestamps captured locally to infer the state of the
// leader election. The client does not consider timestamps in the leader
// election record to be accurate because these timestamps may not have been
// produced by a local clock. The implemention does not depend on their
// accuracy and only uses their change to indicate that another client has
// renewed the leader lease. Thus the implementation is tolerant to arbitrary
// clock skew, but is not tolerant to arbitrary clock skew rate.
//
// However the level of tolerance to skew rate can be configured by setting
// RenewDeadline and LeaseDuration appropriately. The tolerance expressed as a
// maximum tolerated ratio of time passed on the fastest node to time passed on
// the slowest node can be approximately achieved with a configuration that sets
// the same ratio of LeaseDuration to RenewDeadline. For example if a user wanted
// to tolerate some nodes progressing forward in time twice as fast as other nodes,
// the user could set LeaseDuration to 60 seconds and RenewDeadline to 30 seconds.
//
// While not required, some method of clock synchronization between nodes in the
// cluster is highly recommended. It's important to keep in mind when configuring
// this client that the tolerance to skew rate varies inversely to master
// availability.
//
// Larger clusters often have a more lenient SLA for API latency. This should be
// taken into account when configuring the client. The rate of leader transitions
// should be monitored and RetryPeriod and LeaseDuration should be increased
// until the rate is stable and acceptably low. It's important to keep in mind
// when configuring this client that the tolerance to API latency varies inversely
// to master availability.
//
// DISCLAIMER: this is an alpha API. This library will likely change significantly
// or even be removed entirely in subsequent releases. Depend on this API at
// your own risk.
package leaderelection

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	rl "k8s.io/client-go/tools/leaderelection/resourcelock"
	"k8s.io/klog/v2"
	"k8s.io/utils/clock"
)

const (
	JitterFactor = 1.2
)

// NewLeaderElector creates a LeaderElector from a LeaderElectionConfig
func NewLeaderElector(lec LeaderElectionConfig) (*LeaderElector, error) {
	if lec.LeaseDuration <= lec.RenewDeadline {
		return nil, fmt.Errorf("leaseDuration must be greater than renewDeadline")
	}
	if lec.RenewDeadline <= time.Duration(JitterFactor*float64(lec.RetryPeriod)) {
		return nil, fmt.Errorf("renewDeadline must be greater than retryPeriod*JitterFactor")
	}
	if lec.LeaseDuration < 1 {
		return nil, fmt.Errorf("leaseDuration must be greater than zero")
	}
	if lec.RenewDeadline < 1 {
		return nil, fmt.Errorf("renewDeadline must be greater than zero")
	}
	if lec.RetryPeriod < 1 {
		return nil, fmt.Errorf("retryPeriod must be greater than zero")
	}
	if lec.Callbacks.OnStartedLeading == nil {
		return nil, fmt.Errorf("OnStartedLeading callback must not be nil")
	}
	if lec.Callbacks.OnStoppedLeading == nil {
		return nil, fmt.Errorf("OnStoppedLeading callback must not be nil")
	}

	if lec.Lock == nil {
		return nil, fmt.Errorf("Lock must not be nil.")
	}
	id := lec.Lock.Identity()
	if id == "" {
		return nil, fmt.Errorf("Lock identity is empty")
	}

	le := LeaderElector{
		config:  lec,
		clock:   clock.RealClock{},
		metrics: globalMetricsFactory.newLeaderMetrics(),
	}
	le.metrics.leaderOff(le.config.Name)
	return &le, nil
}

type LeaderElectionConfig struct {
	// Lock is the resource that will be used for locking
	Lock rl.Interface

	// LeaseDuration is the duration that non-leader candidates will
	// wait to force acquire leadership. This is measured against time of
	// last observed ack.
	//
	// A client needs to wait a full LeaseDuration without observing a change to
	// the record before it can attempt to take over. When all clients are
	// shutdown and a new set of clients are started with different names against
	// the same leader record, they must wait the full LeaseDuration before
	// attempting to acquire the lease. Thus LeaseDuration should be as short as
	// possible (within your tolerance for clock skew rate) to avoid a possible
	// long waits in the scenario.
	//
	// Core clients default this value to 15 seconds.
	LeaseDuration time.Duration
	// RenewDeadline is the duration that the acting master will retry
	// refreshing leadership before giving up.
	//
	// Core clients default this value to 10 seconds.
	RenewDeadline time.Duration
	// RetryPeriod is the duration the LeaderElector clients should wait
	// between tries of actions.
	//
	// Core clients default this value to 2 seconds.
	RetryPeriod time.Duration

	// Callbacks are callbacks that are triggered during certain lifecycle
	// events of the LeaderElector
	Callbacks LeaderCallbacks

	// WatchDog is the associated health checker
	// WatchDog may be null if it's not needed/configured.
	WatchDog *HealthzAdaptor

	// ReleaseOnCancel should be set true if the lock should be released
	// when the run context is cancelled. If you set this to true, you must
	// ensure all code guarded by this lease has successfully completed
	// prior to cancelling the context, or you may have two processes
	// simultaneously acting on the critical path.
	ReleaseOnCancel bool

	// Name is the name of the resource lock for debugging
	Name string

	// Coordinated will use the Coordinated Leader Election feature
	// WARNING: Coordinated leader election is ALPHA.
	Coordinated bool
}

// LeaderCallbacks are callbacks that are triggered during certain
// lifecycle events of the LeaderElector. These are invoked asynchronously.
//
// possible future callbacks:
//   - OnChallenge()
type LeaderCallbacks struct {
	// OnStartedLeading is called when a LeaderElector client starts leading
	OnStartedLeading func(context.Context)
	// OnStoppedLeading is called when a LeaderElector client stops leading.
	// This callback is always called when the LeaderElector exits, even if it did not start leading.
	// Users should not assume that OnStoppedLeading is only called after OnStartedLeading.
	// see: https://github.com/kubernetes/kubernetes/pull/127675#discussion_r1780059887
	OnStoppedLeading func()
	// OnNewLeader is called when the client observes a leader that is
	// not the previously observed leader. This includes the first observed
	// leader when the client starts.
	OnNewLeader func(identity string)
}

// LeaderElector is a leader election client.
type LeaderElector struct {
	config LeaderElectionConfig
	// internal bookkeeping
	observedRecord    rl.LeaderElectionRecord
	observedRawRecord []byte
	observedTime      time.Time
	// used to implement OnNewLeader(), may lag slightly from the
	// value observedRecord.HolderIdentity if the transition has
	// not yet been reported.
	reportedLeader string

	// clock is wrapper around time to allow for less flaky testing
	clock clock.Clock

	// used to lock the observedRecord
	observedRecordLock sync.Mutex

	metrics leaderMetricsAdapter
}

// Run starts the leader election loop. Run will not return
// before leader election loop is stopped by ctx or it has
// stopped holding the leader lease
func (le *LeaderElector) Run(ctx context.Context) {
	defer runtime.HandleCrashWithContext(ctx)
	defer le.config.Callbacks.OnStoppedLeading()

	if !le.acquire(ctx) {
		return // ctx signalled done
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go le.config.Callbacks.OnStartedLeading(ctx)
	le.renew(ctx)
}

// RunOrDie starts a client with the provided config or panics if the config
// fails to validate. RunOrDie blocks until leader election loop is
// stopped by ctx or it has stopped holding the leader lease
func RunOrDie(ctx context.Context, lec LeaderElectionConfig) {
	le, err := NewLeaderElector(lec)
	if err != nil {
		panic(err)
	}
	if lec.WatchDog != nil {
		lec.WatchDog.SetLeaderElection(le)
	}
	le.Run(ctx)
}

// GetLeader returns the identity of the last observed leader or returns the empty string if
// no leader has yet been observed.
// This function is for informational purposes. (e.g. monitoring, logs, etc.)
func (le *LeaderElector) GetLeader() string {
	return le.getObservedRecord().HolderIdentity
}

// IsLeader returns true if the last observed leader was this client else returns false.
func (le *LeaderElector) IsLeader() bool {
	return le.getObservedRecord().HolderIdentity == le.config.Lock.Identity()
}

// acquire loops calling tryAcquireOrRenew and returns true immediately when tryAcquireOrRenew succeeds.
// Returns false if ctx signals done.
func (le *LeaderElector) acquire(ctx context.Context) bool {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	succeeded := false
	desc := le.config.Lock.Describe()
	logger := klog.FromContext(ctx)
	logger.Info("Attempting to acquire leader lease...", "lock", desc)
	wait.JitterUntil(func() {
		if !le.config.Coordinated {
			succeeded = le.tryAcquireOrRenew(ctx)
		} else {
			succeeded = le.tryCoordinatedRenew(ctx)
		}
		le.maybeReportTransition()
		if !succeeded {
			logger.V(4).Info("Failed to acquire lease", "lock", desc)
			return
		}
		le.config.Lock.RecordEvent("became leader")
		le.metrics.leaderOn(le.config.Name)
		logger.Info("Successfully acquired lease", "lock", desc)
		cancel()
	}, le.config.RetryPeriod, JitterFactor, true, ctx.Done())
	return succeeded
}

// renew loops calling tryAcquireOrRenew and returns immediately when tryAcquireOrRenew fails or ctx signals done.
func (le *LeaderElector) renew(ctx context.Context) {
	defer le.config.Lock.RecordEvent("stopped leading")
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	logger := klog.FromContext(ctx)
	wait.Until(func() {
		err := wait.PollUntilContextTimeout(ctx, le.config.RetryPeriod, le.config.RenewDeadline, true, func(ctx context.Context) (done bool, err error) {
			if !le.config.Coordinated {
				return le.tryAcquireOrRenew(ctx), nil
			} else {
				return le.tryCoordinatedRenew(ctx), nil
			}
		})
		le.maybeReportTransition()
		desc := le.config.Lock.Describe()
		if err == nil {
			logger.V(5).Info("Successfully renewed lease", "lock", desc)
			return
		}
		le.metrics.leaderOff(le.config.Name)
		logger.Info("Failed to renew lease", "lock", desc, "err", err)
		cancel()
	}, le.config.RetryPeriod, ctx.Done())

	// if we hold the lease, give it up
	if le.config.ReleaseOnCancel {
		le.release(logger)
	}
}

// release attempts to release the leader lease if we have acquired it.
func (le *LeaderElector) release(logger klog.Logger) bool {
	if !le.IsLeader() {
		return true
	}
	now := metav1.NewTime(le.clock.Now())
	leaderElectionRecord := rl.LeaderElectionRecord{
		LeaderTransitions:    le.observedRecord.LeaderTransitions,
		LeaseDurationSeconds: 1,
		RenewTime:            now,
		AcquireTime:          now,
	}
	// This intentionally (?) ignores the context of the caller.
	timeoutCtx, timeoutCancel := context.WithTimeout(context.Background(), le.config.RenewDeadline)
	defer timeoutCancel()
	if err := le.config.Lock.Update(timeoutCtx, leaderElectionRecord); err != nil {
		logger.Error(err, "Failed to release lease", "lock", le.config.Lock.Describe())
		return false
	}

	le.setObservedRecord(&leaderElectionRecord)
	return true
}

// tryCoordinatedRenew checks if it acquired a lease and tries to renew the
// lease if it has already been acquired. Returns true on success else returns
// false.
func (le *LeaderElector) tryCoordinatedRenew(ctx context.Context) bool {
	logger := klog.FromContext(ctx)
	now := metav1.NewTime(le.clock.Now())
	leaderElectionRecord := rl.LeaderElectionRecord{
		HolderIdentity:       le.config.Lock.Identity(),
		LeaseDurationSeconds: int(le.config.LeaseDuration / time.Second),
		RenewTime:            now,
		AcquireTime:          now,
	}

	// 1. obtain the electionRecord
	oldLeaderElectionRecord, oldLeaderElectionRawRecord, err := le.config.Lock.Get(ctx)
	if err != nil {
		if !errors.IsNotFound(err) {
			logger.Error(err, "Error retrieving lease lock", "lock", le.config.Lock.Describe())
			return false
		}
		logger.Info("Lease lock not found", "lock", le.config.Lock.Describe(), "err", err)
		return false
	}

	// 2. Record obtained, check the Identity & Time
	if !bytes.Equal(le.observedRawRecord, oldLeaderElectionRawRecord) {
		le.setObservedRecord(oldLeaderElectionRecord)

		le.observedRawRecord = oldLeaderElectionRawRecord
	}

	hasExpired := le.observedTime.Add(time.Second * time.Duration(oldLeaderElectionRecord.LeaseDurationSeconds)).Before(now.Time)
	if hasExpired {
		logger.Info("Lease has expired", "lock", le.config.Lock.Describe())
		return false
	}

	if !le.IsLeader() {
		logger.V(6).Info("Lease is held and has not yet expired", "lock", le.config.Lock.Describe(), "holder", oldLeaderElectionRecord.HolderIdentity)
		return false
	}

	// 2b. If the lease has been marked as "end of term", don't renew it
	if le.IsLeader() && oldLeaderElectionRecord.PreferredHolder != "" {
		logger.V(4).Info("Lease is marked as 'end of term'", "lock", le.config.Lock.Describe())
		// TODO: Instead of letting lease expire, the holder may deleted it directly
		// This will not be compatible with all controllers, so it needs to be opt-in behavior.
		// We must ensure all code guarded by this lease has successfully completed
		// prior to releasing or there may be two processes
		// simultaneously acting on the critical path.
		// Usually once this returns false, the process is terminated..
		// xref: OnStoppedLeading
		return false
	}

	// 3. We're going to try to update. The leaderElectionRecord is set to it's default
	// here. Let's correct it before updating.
	if le.IsLeader() {
		leaderElectionRecord.AcquireTime = oldLeaderElectionRecord.AcquireTime
		leaderElectionRecord.LeaderTransitions = oldLeaderElectionRecord.LeaderTransitions
		leaderElectionRecord.Strategy = oldLeaderElectionRecord.Strategy
		le.metrics.slowpathExercised(le.config.Name)
	} else {
		leaderElectionRecord.LeaderTransitions = oldLeaderElectionRecord.LeaderTransitions + 1
	}

	// update the lock itself
	if err = le.config.Lock.Update(ctx, leaderElectionRecord); err != nil {
		logger.Error(err, "Failed to update lock", "lock", le.config.Lock.Describe())
		return false
	}

	le.setObservedRecord(&leaderElectionRecord)
	return true
}

// tryAcquireOrRenew tries to acquire a leader lease if it is not already acquired,
// else it tries to renew the lease if it has already been acquired. Returns true
// on success else returns false.
func (le *LeaderElector) tryAcquireOrRenew(ctx context.Context) bool {
	logger := klog.FromContext(ctx)
	now := metav1.NewTime(le.clock.Now())
	leaderElectionRecord := rl.LeaderElectionRecord{
		HolderIdentity:       le.config.Lock.Identity(),
		LeaseDurationSeconds: int(le.config.LeaseDuration / time.Second),
		RenewTime:            now,
		AcquireTime:          now,
	}

	// 1. fast path for the leader to update optimistically assuming that the record observed
	// last time is the current version.
	if le.IsLeader() && le.isLeaseValid(now.Time) {
		oldObservedRecord := le.getObservedRecord()
		leaderElectionRecord.AcquireTime = oldObservedRecord.AcquireTime
		leaderElectionRecord.LeaderTransitions = oldObservedRecord.LeaderTransitions

		err := le.config.Lock.Update(ctx, leaderElectionRecord)
		if err == nil {
			le.setObservedRecord(&leaderElectionRecord)
			return true
		}
		logger.Error(err, "Failed to update lease optimistically, falling back to slow path", "lock", le.config.Lock.Describe())
	}

	// 2. obtain or create the ElectionRecord
	oldLeaderElectionRecord, oldLeaderElectionRawRecord, err := le.config.Lock.Get(ctx)
	if err != nil {
		if !errors.IsNotFound(err) {
			logger.Error(err, "Error retrieving lease lock", "lock", le.config.Lock.Describe())
			return false
		}
		if err = le.config.Lock.Create(ctx, leaderElectionRecord); err != nil {
			logger.Error(err, "Error initially creating lease lock", "lock", le.config.Lock.Describe())
			return false
		}

		le.setObservedRecord(&leaderElectionRecord)

		return true
	}

	// 3. Record obtained, check the Identity & Time
	if !bytes.Equal(le.observedRawRecord, oldLeaderElectionRawRecord) {
		le.setObservedRecord(oldLeaderElectionRecord)

		le.observedRawRecord = oldLeaderElectionRawRecord
	}
	if len(oldLeaderElectionRecord.HolderIdentity) > 0 && le.isLeaseValid(now.Time) && !le.IsLeader() {
		logger.V(4).Info("Lease is held by and has not yet expired", "lock", le.config.Lock.Describe(), "holder", oldLeaderElectionRecord.HolderIdentity)
		return false
	}

	// 4. We're going to try to update. The leaderElectionRecord is set to it's default
	// here. Let's correct it before updating.
	if le.IsLeader() {
		leaderElectionRecord.AcquireTime = oldLeaderElectionRecord.AcquireTime
		leaderElectionRecord.LeaderTransitions = oldLeaderElectionRecord.LeaderTransitions
		le.metrics.slowpathExercised(le.config.Name)
	} else {
		leaderElectionRecord.LeaderTransitions = oldLeaderElectionRecord.LeaderTransitions + 1
	}

	// update the lock itself
	if err = le.config.Lock.Update(ctx, leaderElectionRecord); err != nil {
		logger.Error(err, "Failed to update lease", "lock", le.config.Lock.Describe())
		return false
	}

	le.setObservedRecord(&leaderElectionRecord)
	return true
}

func (le *LeaderElector) maybeReportTransition() {
	if le.observedRecord.HolderIdentity == le.reportedLeader {
		return
	}
	le.reportedLeader = le.observedRecord.HolderIdentity
	if le.config.Callbacks.OnNewLeader != nil {
		go le.config.Callbacks.OnNewLeader(le.reportedLeader)
	}
}

// Check will determine if the current lease is expired by more than timeout.
func (le *LeaderElector) Check(maxTolerableExpiredLease time.Duration) error {
	if !le.IsLeader() {
		// Currently not concerned with the case that we are hot standby
		return nil
	}
	// If we are more than timeout seconds after the lease duration that is past the timeout
	// on the lease renew. Time to start reporting ourselves as unhealthy. We should have
	// died but conditions like deadlock can prevent this. (See #70819)
	if le.clock.Since(le.observedTime) > le.config.LeaseDuration+maxTolerableExpiredLease {
		return fmt.Errorf("failed election to renew leadership on lease %s", le.config.Name)
	}

	return nil
}

func (le *LeaderElector) isLeaseValid(now time.Time) bool {
	return le.observedTime.Add(time.Second * time.Duration(le.getObservedRecord().LeaseDurationSeconds)).After(now)
}

// setObservedRecord will set a new observedRecord and update observedTime to the current time.
// Protect critical sections with lock.
func (le *LeaderElector) setObservedRecord(observedRecord *rl.LeaderElectionRecord) {
	le.observedRecordLock.Lock()
	defer le.observedRecordLock.Unlock()

	le.observedRecord = *observedRecord
	le.observedTime = le.clock.Now()
}

// getObservedRecord returns observersRecord.
// Protect critical sections with lock.
func (le *LeaderElector) getObservedRecord() rl.LeaderElectionRecord {
	le.observedRecordLock.Lock()
	defer le.observedRecordLock.Unlock()

	return le.observedRecord
}
