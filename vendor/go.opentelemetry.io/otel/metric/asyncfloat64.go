// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package metric // import "go.opentelemetry.io/otel/metric"

import (
	"context"

	"go.opentelemetry.io/otel/metric/embedded"
)

// Float64Observable describes a set of instruments used asynchronously to
// record float64 measurements once per collection cycle. Observations of
// these instruments are only made within a callback.
//
// Warning: Methods may be added to this interface in minor releases.
type Float64Observable interface {
	Observable

	float64Observable()
}

// Float64ObservableCounter is an instrument used to asynchronously record
// increasing float64 measurements once per collection cycle. Observations are
// only made within a callback for this instrument. The value observed is
// assumed the to be the cumulative sum of the count.
//
// Warning: Methods may be added to this interface in minor releases. See
// package documentation on API implementation for information on how to set
// default behavior for
// unimplemented methods.
type Float64ObservableCounter interface {
	// Users of the interface can ignore this. This embedded type is only used
	// by implementations of this interface. See the "API Implementations"
	// section of the package documentation for more information.
	embedded.Float64ObservableCounter

	Float64Observable
}

// Float64ObservableCounterConfig contains options for asynchronous counter
// instruments that record float64 values.
type Float64ObservableCounterConfig struct {
	description string
	unit        string
	callbacks   []Float64Callback
}

// NewFloat64ObservableCounterConfig returns a new
// [Float64ObservableCounterConfig] with all opts applied.
func NewFloat64ObservableCounterConfig(opts ...Float64ObservableCounterOption) Float64ObservableCounterConfig {
	var config Float64ObservableCounterConfig
	for _, o := range opts {
		config = o.applyFloat64ObservableCounter(config)
	}
	return config
}

// Description returns the configured description.
func (c Float64ObservableCounterConfig) Description() string {
	return c.description
}

// Unit returns the configured unit.
func (c Float64ObservableCounterConfig) Unit() string {
	return c.unit
}

// Callbacks returns the configured callbacks.
func (c Float64ObservableCounterConfig) Callbacks() []Float64Callback {
	return c.callbacks
}

// Float64ObservableCounterOption applies options to a
// [Float64ObservableCounterConfig]. See [Float64ObservableOption] and
// [InstrumentOption] for other options that can be used as a
// Float64ObservableCounterOption.
type Float64ObservableCounterOption interface {
	applyFloat64ObservableCounter(Float64ObservableCounterConfig) Float64ObservableCounterConfig
}

// Float64ObservableUpDownCounter is an instrument used to asynchronously
// record float64 measurements once per collection cycle. Observations are only
// made within a callback for this instrument. The value observed is assumed
// the to be the cumulative sum of the count.
//
// Warning: Methods may be added to this interface in minor releases. See
// package documentation on API implementation for information on how to set
// default behavior for unimplemented methods.
type Float64ObservableUpDownCounter interface {
	// Users of the interface can ignore this. This embedded type is only used
	// by implementations of this interface. See the "API Implementations"
	// section of the package documentation for more information.
	embedded.Float64ObservableUpDownCounter

	Float64Observable
}

// Float64ObservableUpDownCounterConfig contains options for asynchronous
// counter instruments that record float64 values.
type Float64ObservableUpDownCounterConfig struct {
	description string
	unit        string
	callbacks   []Float64Callback
}

// NewFloat64ObservableUpDownCounterConfig returns a new
// [Float64ObservableUpDownCounterConfig] with all opts applied.
func NewFloat64ObservableUpDownCounterConfig(opts ...Float64ObservableUpDownCounterOption) Float64ObservableUpDownCounterConfig {
	var config Float64ObservableUpDownCounterConfig
	for _, o := range opts {
		config = o.applyFloat64ObservableUpDownCounter(config)
	}
	return config
}

// Description returns the configured description.
func (c Float64ObservableUpDownCounterConfig) Description() string {
	return c.description
}

// Unit returns the configured unit.
func (c Float64ObservableUpDownCounterConfig) Unit() string {
	return c.unit
}

// Callbacks returns the configured callbacks.
func (c Float64ObservableUpDownCounterConfig) Callbacks() []Float64Callback {
	return c.callbacks
}

// Float64ObservableUpDownCounterOption applies options to a
// [Float64ObservableUpDownCounterConfig]. See [Float64ObservableOption] and
// [InstrumentOption] for other options that can be used as a
// Float64ObservableUpDownCounterOption.
type Float64ObservableUpDownCounterOption interface {
	applyFloat64ObservableUpDownCounter(Float64ObservableUpDownCounterConfig) Float64ObservableUpDownCounterConfig
}

// Float64ObservableGauge is an instrument used to asynchronously record
// instantaneous float64 measurements once per collection cycle. Observations
// are only made within a callback for this instrument.
//
// Warning: Methods may be added to this interface in minor releases. See
// package documentation on API implementation for information on how to set
// default behavior for unimplemented methods.
type Float64ObservableGauge interface {
	// Users of the interface can ignore this. This embedded type is only used
	// by implementations of this interface. See the "API Implementations"
	// section of the package documentation for more information.
	embedded.Float64ObservableGauge

	Float64Observable
}

// Float64ObservableGaugeConfig contains options for asynchronous counter
// instruments that record float64 values.
type Float64ObservableGaugeConfig struct {
	description string
	unit        string
	callbacks   []Float64Callback
}

// NewFloat64ObservableGaugeConfig returns a new [Float64ObservableGaugeConfig]
// with all opts applied.
func NewFloat64ObservableGaugeConfig(opts ...Float64ObservableGaugeOption) Float64ObservableGaugeConfig {
	var config Float64ObservableGaugeConfig
	for _, o := range opts {
		config = o.applyFloat64ObservableGauge(config)
	}
	return config
}

// Description returns the configured description.
func (c Float64ObservableGaugeConfig) Description() string {
	return c.description
}

// Unit returns the configured unit.
func (c Float64ObservableGaugeConfig) Unit() string {
	return c.unit
}

// Callbacks returns the configured callbacks.
func (c Float64ObservableGaugeConfig) Callbacks() []Float64Callback {
	return c.callbacks
}

// Float64ObservableGaugeOption applies options to a
// [Float64ObservableGaugeConfig]. See [Float64ObservableOption] and
// [InstrumentOption] for other options that can be used as a
// Float64ObservableGaugeOption.
type Float64ObservableGaugeOption interface {
	applyFloat64ObservableGauge(Float64ObservableGaugeConfig) Float64ObservableGaugeConfig
}

// Float64Observer is a recorder of float64 measurements.
//
// Warning: Methods may be added to this interface in minor releases. See
// package documentation on API implementation for information on how to set
// default behavior for unimplemented methods.
type Float64Observer interface {
	// Users of the interface can ignore this. This embedded type is only used
	// by implementations of this interface. See the "API Implementations"
	// section of the package documentation for more information.
	embedded.Float64Observer

	// Observe records the float64 value.
	//
	// Use the WithAttributeSet (or, if performance is not a concern,
	// the WithAttributes) option to include measurement attributes.
	Observe(value float64, options ...ObserveOption)
}

// Float64Callback is a function registered with a Meter that makes
// observations for a Float64Observable instrument it is registered with.
// Calls to the Float64Observer record measurement values for the
// Float64Observable.
//
// The function needs to complete in a finite amount of time and the deadline
// of the passed context is expected to be honored.
//
// The function needs to make unique observations across all registered
// Float64Callbacks. Meaning, it should not report measurements with the same
// attributes as another Float64Callbacks also registered for the same
// instrument.
//
// The function needs to be concurrent safe.
type Float64Callback func(context.Context, Float64Observer) error

// Float64ObservableOption applies options to float64 Observer instruments.
type Float64ObservableOption interface {
	Float64ObservableCounterOption
	Float64ObservableUpDownCounterOption
	Float64ObservableGaugeOption
}

type float64CallbackOpt struct {
	cback Float64Callback
}

func (o float64CallbackOpt) applyFloat64ObservableCounter(cfg Float64ObservableCounterConfig) Float64ObservableCounterConfig {
	cfg.callbacks = append(cfg.callbacks, o.cback)
	return cfg
}

func (o float64CallbackOpt) applyFloat64ObservableUpDownCounter(cfg Float64ObservableUpDownCounterConfig) Float64ObservableUpDownCounterConfig {
	cfg.callbacks = append(cfg.callbacks, o.cback)
	return cfg
}

func (o float64CallbackOpt) applyFloat64ObservableGauge(cfg Float64ObservableGaugeConfig) Float64ObservableGaugeConfig {
	cfg.callbacks = append(cfg.callbacks, o.cback)
	return cfg
}

// WithFloat64Callback adds callback to be called for an instrument.
func WithFloat64Callback(callback Float64Callback) Float64ObservableOption {
	return float64CallbackOpt{callback}
}
