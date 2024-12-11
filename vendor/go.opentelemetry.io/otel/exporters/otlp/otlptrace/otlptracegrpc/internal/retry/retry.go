// Code created by gotmpl. DO NOT MODIFY.
// source: internal/shared/otlp/retry/retry.go.tmpl

// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

// Package retry provides request retry functionality that can perform
// configurable exponential backoff for transient errors and honor any
// explicit throttle responses received.
package retry // import "go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc/internal/retry"

import (
	"context"
	"fmt"
	"time"

	"github.com/cenkalti/backoff/v4"
)

// DefaultConfig are the recommended defaults to use.
var DefaultConfig = Config{
	Enabled:         true,
	InitialInterval: 5 * time.Second,
	MaxInterval:     30 * time.Second,
	MaxElapsedTime:  time.Minute,
}

// Config defines configuration for retrying batches in case of export failure
// using an exponential backoff.
type Config struct {
	// Enabled indicates whether to not retry sending batches in case of
	// export failure.
	Enabled bool
	// InitialInterval the time to wait after the first failure before
	// retrying.
	InitialInterval time.Duration
	// MaxInterval is the upper bound on backoff interval. Once this value is
	// reached the delay between consecutive retries will always be
	// `MaxInterval`.
	MaxInterval time.Duration
	// MaxElapsedTime is the maximum amount of time (including retries) spent
	// trying to send a request/batch.  Once this value is reached, the data
	// is discarded.
	MaxElapsedTime time.Duration
}

// RequestFunc wraps a request with retry logic.
type RequestFunc func(context.Context, func(context.Context) error) error

// EvaluateFunc returns if an error is retry-able and if an explicit throttle
// duration should be honored that was included in the error.
//
// The function must return true if the error argument is retry-able,
// otherwise it must return false for the first return parameter.
//
// The function must return a non-zero time.Duration if the error contains
// explicit throttle duration that should be honored, otherwise it must return
// a zero valued time.Duration.
type EvaluateFunc func(error) (bool, time.Duration)

// RequestFunc returns a RequestFunc using the evaluate function to determine
// if requests can be retried and based on the exponential backoff
// configuration of c.
func (c Config) RequestFunc(evaluate EvaluateFunc) RequestFunc {
	if !c.Enabled {
		return func(ctx context.Context, fn func(context.Context) error) error {
			return fn(ctx)
		}
	}

	return func(ctx context.Context, fn func(context.Context) error) error {
		// Do not use NewExponentialBackOff since it calls Reset and the code here
		// must call Reset after changing the InitialInterval (this saves an
		// unnecessary call to Now).
		b := &backoff.ExponentialBackOff{
			InitialInterval:     c.InitialInterval,
			RandomizationFactor: backoff.DefaultRandomizationFactor,
			Multiplier:          backoff.DefaultMultiplier,
			MaxInterval:         c.MaxInterval,
			MaxElapsedTime:      c.MaxElapsedTime,
			Stop:                backoff.Stop,
			Clock:               backoff.SystemClock,
		}
		b.Reset()

		for {
			err := fn(ctx)
			if err == nil {
				return nil
			}

			retryable, throttle := evaluate(err)
			if !retryable {
				return err
			}

			bOff := b.NextBackOff()
			if bOff == backoff.Stop {
				return fmt.Errorf("max retry time elapsed: %w", err)
			}

			// Wait for the greater of the backoff or throttle delay.
			var delay time.Duration
			if bOff > throttle {
				delay = bOff
			} else {
				elapsed := b.GetElapsedTime()
				if b.MaxElapsedTime != 0 && elapsed+throttle > b.MaxElapsedTime {
					return fmt.Errorf("max retry time would elapse: %w", err)
				}
				delay = throttle
			}

			if ctxErr := waitFunc(ctx, delay); ctxErr != nil {
				return fmt.Errorf("%w: %w", ctxErr, err)
			}
		}
	}
}

// Allow override for testing.
var waitFunc = wait

// wait takes the caller's context, and the amount of time to wait.  It will
// return nil if the timer fires before or at the same time as the context's
// deadline.  This indicates that the call can be retried.
func wait(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		// Handle the case where the timer and context deadline end
		// simultaneously by prioritizing the timer expiration nil value
		// response.
		select {
		case <-timer.C:
		default:
			return ctx.Err()
		}
	case <-timer.C:
	}

	return nil
}
