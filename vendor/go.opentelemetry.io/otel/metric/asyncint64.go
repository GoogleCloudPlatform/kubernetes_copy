// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package metric // import "go.opentelemetry.io/otel/metric"

import (
	"context"

	"go.opentelemetry.io/otel/metric/embedded"
)

// Int64Observable describes a set of instruments used asynchronously to record
// int64 measurements once per collection cycle. Observations of these
// instruments are only made within a callback.
//
// Warning: Methods may be added to this interface in minor releases.
type Int64Observable interface {
	Observable

	int64Observable()
}

// Int64ObservableCounter is an instrument used to asynchronously record
// increasing int64 measurements once per collection cycle. Observations are
// only made within a callback for this instrument. The value observed is
// assumed the to be the cumulative sum of the count.
//
// Warning: Methods may be added to this interface in minor releases. See
// package documentation on API implementation for information on how to set
// default behavior for unimplemented methods.
type Int64ObservableCounter interface {
	// Users of the interface can ignore this. This embedded type is only used
	// by implementations of this interface. See the "API Implementations"
	// section of the package documentation for more information.
	embedded.Int64ObservableCounter

	Int64Observable
}

// Int64ObservableCounterConfig contains options for asynchronous counter
// instruments that record int64 values.
type Int64ObservableCounterConfig struct {
	description string
	unit        string
	callbacks   []Int64Callback
}

// NewInt64ObservableCounterConfig returns a new [Int64ObservableCounterConfig]
// with all opts applied.
func NewInt64ObservableCounterConfig(opts ...Int64ObservableCounterOption) Int64ObservableCounterConfig {
	var config Int64ObservableCounterConfig
	for _, o := range opts {
		config = o.applyInt64ObservableCounter(config)
	}
	return config
}

// Description returns the configured description.
func (c Int64ObservableCounterConfig) Description() string {
	return c.description
}

// Unit returns the configured unit.
func (c Int64ObservableCounterConfig) Unit() string {
	return c.unit
}

// Callbacks returns the configured callbacks.
func (c Int64ObservableCounterConfig) Callbacks() []Int64Callback {
	return c.callbacks
}

// Int64ObservableCounterOption applies options to a
// [Int64ObservableCounterConfig]. See [Int64ObservableOption] and
// [InstrumentOption] for other options that can be used as an
// Int64ObservableCounterOption.
type Int64ObservableCounterOption interface {
	applyInt64ObservableCounter(Int64ObservableCounterConfig) Int64ObservableCounterConfig
}

// Int64ObservableUpDownCounter is an instrument used to asynchronously record
// int64 measurements once per collection cycle. Observations are only made
// within a callback for this instrument. The value observed is assumed the to
// be the cumulative sum of the count.
//
// Warning: Methods may be added to this interface in minor releases. See
// package documentation on API implementation for information on how to set
// default behavior for unimplemented methods.
type Int64ObservableUpDownCounter interface {
	// Users of the interface can ignore this. This embedded type is only used
	// by implementations of this interface. See the "API Implementations"
	// section of the package documentation for more information.
	embedded.Int64ObservableUpDownCounter

	Int64Observable
}

// Int64ObservableUpDownCounterConfig contains options for asynchronous counter
// instruments that record int64 values.
type Int64ObservableUpDownCounterConfig struct {
	description string
	unit        string
	callbacks   []Int64Callback
}

// NewInt64ObservableUpDownCounterConfig returns a new
// [Int64ObservableUpDownCounterConfig] with all opts applied.
func NewInt64ObservableUpDownCounterConfig(opts ...Int64ObservableUpDownCounterOption) Int64ObservableUpDownCounterConfig {
	var config Int64ObservableUpDownCounterConfig
	for _, o := range opts {
		config = o.applyInt64ObservableUpDownCounter(config)
	}
	return config
}

// Description returns the configured description.
func (c Int64ObservableUpDownCounterConfig) Description() string {
	return c.description
}

// Unit returns the configured unit.
func (c Int64ObservableUpDownCounterConfig) Unit() string {
	return c.unit
}

// Callbacks returns the configured callbacks.
func (c Int64ObservableUpDownCounterConfig) Callbacks() []Int64Callback {
	return c.callbacks
}

// Int64ObservableUpDownCounterOption applies options to a
// [Int64ObservableUpDownCounterConfig]. See [Int64ObservableOption] and
// [InstrumentOption] for other options that can be used as an
// Int64ObservableUpDownCounterOption.
type Int64ObservableUpDownCounterOption interface {
	applyInt64ObservableUpDownCounter(Int64ObservableUpDownCounterConfig) Int64ObservableUpDownCounterConfig
}

// Int64ObservableGauge is an instrument used to asynchronously record
// instantaneous int64 measurements once per collection cycle. Observations are
// only made within a callback for this instrument.
//
// Warning: Methods may be added to this interface in minor releases. See
// package documentation on API implementation for information on how to set
// default behavior for unimplemented methods.
type Int64ObservableGauge interface {
	// Users of the interface can ignore this. This embedded type is only used
	// by implementations of this interface. See the "API Implementations"
	// section of the package documentation for more information.
	embedded.Int64ObservableGauge

	Int64Observable
}

// Int64ObservableGaugeConfig contains options for asynchronous counter
// instruments that record int64 values.
type Int64ObservableGaugeConfig struct {
	description string
	unit        string
	callbacks   []Int64Callback
}

// NewInt64ObservableGaugeConfig returns a new [Int64ObservableGaugeConfig]
// with all opts applied.
func NewInt64ObservableGaugeConfig(opts ...Int64ObservableGaugeOption) Int64ObservableGaugeConfig {
	var config Int64ObservableGaugeConfig
	for _, o := range opts {
		config = o.applyInt64ObservableGauge(config)
	}
	return config
}

// Description returns the configured description.
func (c Int64ObservableGaugeConfig) Description() string {
	return c.description
}

// Unit returns the configured unit.
func (c Int64ObservableGaugeConfig) Unit() string {
	return c.unit
}

// Callbacks returns the configured callbacks.
func (c Int64ObservableGaugeConfig) Callbacks() []Int64Callback {
	return c.callbacks
}

// Int64ObservableGaugeOption applies options to a
// [Int64ObservableGaugeConfig]. See [Int64ObservableOption] and
// [InstrumentOption] for other options that can be used as an
// Int64ObservableGaugeOption.
type Int64ObservableGaugeOption interface {
	applyInt64ObservableGauge(Int64ObservableGaugeConfig) Int64ObservableGaugeConfig
}

// Int64Observer is a recorder of int64 measurements.
//
// Warning: Methods may be added to this interface in minor releases. See
// package documentation on API implementation for information on how to set
// default behavior for unimplemented methods.
type Int64Observer interface {
	// Users of the interface can ignore this. This embedded type is only used
	// by implementations of this interface. See the "API Implementations"
	// section of the package documentation for more information.
	embedded.Int64Observer

	// Observe records the int64 value.
	//
	// Use the WithAttributeSet (or, if performance is not a concern,
	// the WithAttributes) option to include measurement attributes.
	Observe(value int64, options ...ObserveOption)
}

// Int64Callback is a function registered with a Meter that makes observations
// for an Int64Observable instrument it is registered with. Calls to the
// Int64Observer record measurement values for the Int64Observable.
//
// The function needs to complete in a finite amount of time and the deadline
// of the passed context is expected to be honored.
//
// The function needs to make unique observations across all registered
// Int64Callbacks. Meaning, it should not report measurements with the same
// attributes as another Int64Callbacks also registered for the same
// instrument.
//
// The function needs to be concurrent safe.
type Int64Callback func(context.Context, Int64Observer) error

// Int64ObservableOption applies options to int64 Observer instruments.
type Int64ObservableOption interface {
	Int64ObservableCounterOption
	Int64ObservableUpDownCounterOption
	Int64ObservableGaugeOption
}

type int64CallbackOpt struct {
	cback Int64Callback
}

func (o int64CallbackOpt) applyInt64ObservableCounter(cfg Int64ObservableCounterConfig) Int64ObservableCounterConfig {
	cfg.callbacks = append(cfg.callbacks, o.cback)
	return cfg
}

func (o int64CallbackOpt) applyInt64ObservableUpDownCounter(cfg Int64ObservableUpDownCounterConfig) Int64ObservableUpDownCounterConfig {
	cfg.callbacks = append(cfg.callbacks, o.cback)
	return cfg
}

func (o int64CallbackOpt) applyInt64ObservableGauge(cfg Int64ObservableGaugeConfig) Int64ObservableGaugeConfig {
	cfg.callbacks = append(cfg.callbacks, o.cback)
	return cfg
}

// WithInt64Callback adds callback to be called for an instrument.
func WithInt64Callback(callback Int64Callback) Int64ObservableOption {
	return int64CallbackOpt{callback}
}
