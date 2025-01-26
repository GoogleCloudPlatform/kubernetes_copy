/*
Copyright 2023 The Kubernetes Authors.

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

package wait

import (
	"context"
	"math"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/utils/clock"
)

// Backoff holds parameters applied to a Backoff function.
type Backoff struct {
	// The initial duration.
	Duration time.Duration
	// Duration is multiplied by factor each iteration, if factor is not zero
	// and the limits imposed by Steps and Cap have not been reached.
	// Should not be negative.
	// The jitter does not contribute to the updates to the duration parameter.
	Factor float64
	// The sleep at each iteration is the duration plus an additional
	// amount chosen uniformly at random from the interval between
	// zero and `jitter*duration`.
	Jitter float64
	// The remaining number of iterations in which the duration
	// parameter may change (but progress can be stopped earlier by
	// hitting the cap). If not positive, the duration is not
	// changed. Used for exponential backoff in combination with
	// Factor and Cap.
	Steps int
	// A limit on revised values of the duration parameter. If a
	// multiplication by the factor parameter would make the duration
	// exceed the cap then the duration is set to the cap and the
	// steps parameter is set to zero.
	Cap time.Duration
}

// Step returns an amount of time to sleep determined by the original
// Duration and Jitter. The backoff is mutated to update its Steps and
// Duration. A nil Backoff always has a zero-duration step.
func (b *Backoff) Step() time.Duration {
	if b == nil {
		return 0
	}
	var nextDuration time.Duration
	nextDuration, b.Duration, b.Steps = delay(b.Steps, b.Duration, b.Cap, b.Factor, b.Jitter)
	return nextDuration
}

// DelayFunc returns a function that will compute the next interval to
// wait given the arguments in b. It does not mutate the original backoff
// but the function is safe to use only from a single goroutine.
func (b Backoff) DelayFunc() DelayFunc {
	steps := b.Steps
	duration := b.Duration
	cap := b.Cap
	factor := b.Factor
	jitter := b.Jitter

	return func() time.Duration {
		var nextDuration time.Duration
		// jitter is applied per step and is not cumulative over multiple steps
		nextDuration, duration, steps = delay(steps, duration, cap, factor, jitter)
		return nextDuration
	}
}

// Timer returns a timer implementation appropriate to this backoff's parameters
// for use with wait functions.
func (b Backoff) Timer() Timer {
	if b.Steps > 1 || b.Jitter != 0 {
		return &variableTimer{new: internalClock.NewTimer, fn: b.DelayFunc()}
	}
	if b.Duration > 0 {
		return &fixedTimer{new: internalClock.NewTicker, interval: b.Duration}
	}
	return newNoopTimer()
}

// delay implements the core delay algorithm used in this package.
func delay(steps int, duration, cap time.Duration, factor, jitter float64) (_ time.Duration, next time.Duration, nextSteps int) {
	// when steps is non-positive, do not alter the base duration
	if steps < 1 {
		if jitter > 0 {
			return Jitter(duration, jitter), duration, 0
		}
		return duration, duration, 0
	}
	steps--

	// calculate the next step's interval
	if factor != 0 {
		next = time.Duration(float64(duration) * factor)
		if cap > 0 && next > cap {
			next = cap
			steps = 0
		}
	} else {
		next = duration
	}

	// add jitter for this step
	if jitter > 0 {
		duration = Jitter(duration, jitter)
	}

	return duration, next, steps

}

// DelayWithReset returns a DelayFunc that will return the appropriate next interval to
// wait. Every resetInterval the backoff parameters are reset to their initial state.
// This method is safe to invoke from multiple goroutines, but all calls will advance
// the backoff state when Factor is set. If Factor is zero, this method is the same as
// invoking b.DelayFunc() since Steps has no impact without Factor. If resetInterval is
// zero no backoff will be performed as the same calling DelayFunc with a zero factor
// and steps.
func (b Backoff) DelayWithReset(c clock.Clock, resetInterval time.Duration) DelayFunc {
	if b.Factor <= 0 {
		return b.DelayFunc()
	}
	if resetInterval <= 0 {
		b.Steps = 0
		b.Factor = 0
		return b.DelayFunc()
	}
	return (&backoffManager{
		backoff:        b,
		initialBackoff: b,
		resetInterval:  resetInterval,

		clock:     c,
		lastStart: c.Now(),
		timer:     nil,
	}).Step
}

// Until loops until stop channel is closed, running f every period.
//
// Until is syntactic sugar on top of JitterUntil with zero jitter factor and
// with sliding = true (which means the timer for period starts after the f
// completes).
//
// Contextual logging: UntilWithContext should be used instead of Until in code which supports contextual logging.
func Until(f func(), period time.Duration, stopCh <-chan struct{}) {
	JitterUntil(f, period, 0.0, true, stopCh)
}

// UntilWithContext loops until context is done, running f every period.
//
// UntilWithContext is syntactic sugar on top of JitterUntilWithContext
// with zero jitter factor and with sliding = true (which means the timer
// for period starts after the f completes).
func UntilWithContext(ctx context.Context, f func(context.Context), period time.Duration) {
	JitterUntilWithContext(ctx, f, period, 0.0, true)
}

// NonSlidingUntil loops until stop channel is closed, running f every
// period.
//
// NonSlidingUntil is syntactic sugar on top of JitterUntil with zero jitter
// factor, with sliding = false (meaning the timer for period starts at the same
// time as the function starts).
//
// Contextual logging: NonSlidingUntilWithContext should be used instead of NonSlidingUntil in code which supports contextual logging.
func NonSlidingUntil(f func(), period time.Duration, stopCh <-chan struct{}) {
	JitterUntil(f, period, 0.0, false, stopCh)
}

// NonSlidingUntilWithContext loops until context is done, running f every
// period.
//
// NonSlidingUntilWithContext is syntactic sugar on top of JitterUntilWithContext
// with zero jitter factor, with sliding = false (meaning the timer for period
// starts at the same time as the function starts).
func NonSlidingUntilWithContext(ctx context.Context, f func(context.Context), period time.Duration) {
	JitterUntilWithContext(ctx, f, period, 0.0, false)
}

// JitterUntil loops until stop channel is closed, running f every period.
//
// If jitterFactor is positive, the period is jittered before every run of f.
// If jitterFactor is not positive, the period is unchanged and not jittered.
//
// If sliding is true, the period is computed after f runs. If it is false then
// period includes the runtime for f.
//
// Close stopCh to stop. f may not be invoked if stop channel is already
// closed. Pass NeverStop to if you don't want it stop.
//
// Contextual logging: JitterUntilWithContext should be used instead of JitterUntil in code which supports contextual logging.
func JitterUntil(f func(), period time.Duration, jitterFactor float64, sliding bool, stopCh <-chan struct{}) {
	BackoffUntil(f, NewJitteredBackoffManager(period, jitterFactor, &clock.RealClock{}), sliding, stopCh)
}

// JitterUntilWithContext loops until context is done, running f every period.
//
// If jitterFactor is positive, the period is jittered before every run of f.
// If jitterFactor is not positive, the period is unchanged and not jittered.
//
// If sliding is true, the period is computed after f runs. If it is false then
// period includes the runtime for f.
//
// Cancel context to stop. f may not be invoked if context is already done.
func JitterUntilWithContext(ctx context.Context, f func(context.Context), period time.Duration, jitterFactor float64, sliding bool) {
	BackoffUntilWithContext(ctx, f, NewJitteredBackoffManager(period, jitterFactor, &clock.RealClock{}), sliding)
}

// Function that changes the sleep period during runtime (based on conditions)
func DyanmicPeriodUntil(ctx context.Context, f func(), period func() time.Duration, jitterFactor float64, sliding bool) {
	BackoffUntil(f, NewDynamicPeriodBackoffManagerImpl(period, jitterFactor, &clock.RealClock{}), sliding, ctx.Done())
}

// BackoffUntil loops until stop channel is closed, run f every duration given by BackoffManager.
//
// If sliding is true, the period is computed after f runs. If it is false then
// period includes the runtime for f.
//
// Contextual logging: BackoffUntilWithContext should be used instead of BackoffUntil in code which supports contextual logging.
func BackoffUntil(f func(), backoff BackoffManager, sliding bool, stopCh <-chan struct{}) {
	BackoffUntilWithContext(ContextForChannel(stopCh), func(context.Context) { f() }, backoff, sliding)
}

// BackoffUntilWithContext loops until context is done, run f every duration given by BackoffManager.
//
// If sliding is true, the period is computed after f runs. If it is false then
// period includes the runtime for f.
func BackoffUntilWithContext(ctx context.Context, f func(ctx context.Context), backoff BackoffManager, sliding bool) {
	var t clock.Timer
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		if !sliding {
			t = backoff.Backoff()
		}

		func() {
			defer runtime.HandleCrashWithContext(ctx)
			f(ctx)
		}()

		if sliding {
			t = backoff.Backoff()
		}

		// dynamically change the sleep period
		dynamicPeriod, ok := backoff.(*dynamicPeriodBackoffManagerImpl)
		if ok {
			t.Reset(dynamicPeriod.duration())
		}

		// NOTE: b/c there is no priority selection in golang
		// it is possible for this to race, meaning we could
		// trigger t.C and stopCh, and t.C select falls through.
		// In order to mitigate we re-check stopCh at the beginning
		// of every loop to prevent extra executions of f().
		select {
		case <-ctx.Done():
			if !t.Stop() {
				<-t.C()
			}
			return
		case <-t.C():
		}
	}
}

// backoffManager provides simple backoff behavior in a threadsafe manner to a caller.
type backoffManager struct {
	backoff        Backoff
	initialBackoff Backoff
	resetInterval  time.Duration

	clock clock.Clock

	lock      sync.Mutex
	lastStart time.Time
	timer     clock.Timer
}

// Step returns the expected next duration to wait.
func (b *backoffManager) Step() time.Duration {
	b.lock.Lock()
	defer b.lock.Unlock()

	switch {
	case b.resetInterval == 0:
		b.backoff = b.initialBackoff
	case b.clock.Now().Sub(b.lastStart) > b.resetInterval:
		b.backoff = b.initialBackoff
		b.lastStart = b.clock.Now()
	}
	return b.backoff.Step()
}

// Backoff implements BackoffManager.Backoff, it returns a timer so caller can block on the timer
// for exponential backoff. The returned timer must be drained before calling Backoff() the second
// time.
func (b *backoffManager) Backoff() clock.Timer {
	b.lock.Lock()
	defer b.lock.Unlock()
	if b.timer == nil {
		b.timer = b.clock.NewTimer(b.Step())
	} else {
		b.timer.Reset(b.Step())
	}
	return b.timer
}

// Timer returns a new Timer instance that shares the clock and the reset behavior with all other
// timers.
func (b *backoffManager) Timer() Timer {
	return DelayFunc(b.Step).Timer(b.clock)
}

// BackoffManager manages backoff with a particular scheme based on its underlying implementation.
type BackoffManager interface {
	// Backoff returns a shared clock.Timer that is Reset on every invocation. This method is not
	// safe for use from multiple threads. It returns a timer for backoff, and caller shall backoff
	// until Timer.C() drains. If the second Backoff() is called before the timer from the first
	// Backoff() call finishes, the first timer will NOT be drained and result in undetermined
	// behavior.
	Backoff() clock.Timer
}

// Deprecated: Will be removed when the legacy polling functions are removed.
type exponentialBackoffManagerImpl struct {
	backoff              *Backoff
	backoffTimer         clock.Timer
	lastBackoffStart     time.Time
	initialBackoff       time.Duration
	backoffResetDuration time.Duration
	clock                clock.Clock
}

// NewExponentialBackoffManager returns a manager for managing exponential backoff. Each backoff is jittered and
// backoff will not exceed the given max. If the backoff is not called within resetDuration, the backoff is reset.
// This backoff manager is used to reduce load during upstream unhealthiness.
//
// Deprecated: Will be removed when the legacy Poll methods are removed. Callers should construct a
// Backoff struct, use DelayWithReset() to get a DelayFunc that periodically resets itself, and then
// invoke Timer() when calling wait.BackoffUntil.
//
// Instead of:
//
//	bm := wait.NewExponentialBackoffManager(init, max, reset, factor, jitter, clock)
//	...
//	wait.BackoffUntil(..., bm.Backoff, ...)
//
// Use:
//
//	delayFn := wait.Backoff{
//	  Duration: init,
//	  Cap:      max,
//	  Steps:    int(math.Ceil(float64(max) / float64(init))), // now a required argument
//	  Factor:   factor,
//	  Jitter:   jitter,
//	}.DelayWithReset(reset, clock)
//	wait.BackoffUntil(..., delayFn.Timer(), ...)
func NewExponentialBackoffManager(initBackoff, maxBackoff, resetDuration time.Duration, backoffFactor, jitter float64, c clock.Clock) BackoffManager {
	return &exponentialBackoffManagerImpl{
		backoff: &Backoff{
			Duration: initBackoff,
			Factor:   backoffFactor,
			Jitter:   jitter,

			// the current impl of wait.Backoff returns Backoff.Duration once steps are used up, which is not
			// what we ideally need here, we set it to max int and assume we will never use up the steps
			Steps: math.MaxInt32,
			Cap:   maxBackoff,
		},
		backoffTimer:         nil,
		initialBackoff:       initBackoff,
		lastBackoffStart:     c.Now(),
		backoffResetDuration: resetDuration,
		clock:                c,
	}
}

func (b *exponentialBackoffManagerImpl) getNextBackoff() time.Duration {
	if b.clock.Now().Sub(b.lastBackoffStart) > b.backoffResetDuration {
		b.backoff.Steps = math.MaxInt32
		b.backoff.Duration = b.initialBackoff
	}
	b.lastBackoffStart = b.clock.Now()
	return b.backoff.Step()
}

// Backoff implements BackoffManager.Backoff, it returns a timer so caller can block on the timer for exponential backoff.
// The returned timer must be drained before calling Backoff() the second time
func (b *exponentialBackoffManagerImpl) Backoff() clock.Timer {
	if b.backoffTimer == nil {
		b.backoffTimer = b.clock.NewTimer(b.getNextBackoff())
	} else {
		b.backoffTimer.Reset(b.getNextBackoff())
	}
	return b.backoffTimer
}

// Deprecated: Will be removed when the legacy polling functions are removed.
type jitteredBackoffManagerImpl struct {
	clock        clock.Clock
	duration     time.Duration
	jitter       float64
	backoffTimer clock.Timer
}

type dynamicPeriodBackoffManagerImpl struct {
	clock        clock.Clock
	duration     func() time.Duration
	jitter       float64
	backoffTimer clock.Timer
}

func NewDynamicPeriodBackoffManagerImpl(duration func() time.Duration, jitter float64, c clock.Clock) BackoffManager {
	return &dynamicPeriodBackoffManagerImpl{
		clock:        c,
		duration:     duration,
		jitter:       jitter,
		backoffTimer: nil,
	}
}

// NewJitteredBackoffManager returns a BackoffManager that backoffs with given duration plus given jitter. If the jitter
// is negative, backoff will not be jittered.
//
// Deprecated: Will be removed when the legacy Poll methods are removed. Callers should construct a
// Backoff struct and invoke Timer() when calling wait.BackoffUntil.
//
// Instead of:
//
//	bm := wait.NewJitteredBackoffManager(duration, jitter, clock)
//	...
//	wait.BackoffUntil(..., bm.Backoff, ...)
//
// Use:
//
//	wait.BackoffUntil(..., wait.Backoff{Duration: duration, Jitter: jitter}.Timer(), ...)
func NewJitteredBackoffManager(duration time.Duration, jitter float64, c clock.Clock) BackoffManager {
	return &jitteredBackoffManagerImpl{
		clock:        c,
		duration:     duration,
		jitter:       jitter,
		backoffTimer: nil,
	}
}

func (d *dynamicPeriodBackoffManagerImpl) getNextBackoff() time.Duration {
	jitteredPeriod := d.duration()
	if d.jitter > 0.0 {
		jitteredPeriod = Jitter(d.duration(), d.jitter)
	}
	return jitteredPeriod
}

func (j *jitteredBackoffManagerImpl) getNextBackoff() time.Duration {
	jitteredPeriod := j.duration
	if j.jitter > 0.0 {
		jitteredPeriod = Jitter(j.duration, j.jitter)
	}
	return jitteredPeriod
}

// Backoff implements BackoffManager.Backoff, it returns a timer so caller can block on the timer for jittered backoff.
// The returned timer must be drained before calling Backoff() the second time
func (j *jitteredBackoffManagerImpl) Backoff() clock.Timer {
	backoff := j.getNextBackoff()
	if j.backoffTimer == nil {
		j.backoffTimer = j.clock.NewTimer(backoff)
	} else {
		j.backoffTimer.Reset(backoff)
	}
	return j.backoffTimer
}

func (d *dynamicPeriodBackoffManagerImpl) Backoff() clock.Timer {
	backoff := d.getNextBackoff()
	if d.backoffTimer == nil {
		d.backoffTimer = d.clock.NewTimer(backoff)
	} else {
		d.backoffTimer.Reset(backoff)
	}
	return d.backoffTimer
}

// ExponentialBackoff repeats a condition check with exponential backoff.
//
// It repeatedly checks the condition and then sleeps, using `backoff.Step()`
// to determine the length of the sleep and adjust Duration and Steps.
// Stops and returns as soon as:
// 1. the condition check returns true or an error,
// 2. `backoff.Steps` checks of the condition have been done, or
// 3. a sleep truncated by the cap on duration has been completed.
// In case (1) the returned error is what the condition function returned.
// In all other cases, ErrWaitTimeout is returned.
//
// Since backoffs are often subject to cancellation, we recommend using
// ExponentialBackoffWithContext and passing a context to the method.
func ExponentialBackoff(backoff Backoff, condition ConditionFunc) error {
	for backoff.Steps > 0 {
		if ok, err := runConditionWithCrashProtection(condition); err != nil || ok {
			return err
		}
		if backoff.Steps == 1 {
			break
		}
		time.Sleep(backoff.Step())
	}
	return ErrWaitTimeout
}

// ExponentialBackoffWithContext repeats a condition check with exponential backoff.
// It immediately returns an error if the condition returns an error, the context is cancelled
// or hits the deadline, or if the maximum attempts defined in backoff is exceeded (ErrWaitTimeout).
// If an error is returned by the condition the backoff stops immediately. The condition will
// never be invoked more than backoff.Steps times.
func ExponentialBackoffWithContext(ctx context.Context, backoff Backoff, condition ConditionWithContextFunc) error {
	for backoff.Steps > 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		if ok, err := runConditionWithCrashProtectionWithContext(ctx, condition); err != nil || ok {
			return err
		}

		if backoff.Steps == 1 {
			break
		}

		waitBeforeRetry := backoff.Step()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(waitBeforeRetry):
		}
	}

	return ErrWaitTimeout
}
