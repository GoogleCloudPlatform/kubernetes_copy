// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package global // import "go.opentelemetry.io/otel/internal/global"

import (
	"container/list"
	"context"
	"reflect"
	"sync"

	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/embedded"
)

// meterProvider is a placeholder for a configured SDK MeterProvider.
//
// All MeterProvider functionality is forwarded to a delegate once
// configured.
type meterProvider struct {
	embedded.MeterProvider

	mtx    sync.Mutex
	meters map[il]*meter

	delegate metric.MeterProvider
}

// setDelegate configures p to delegate all MeterProvider functionality to
// provider.
//
// All Meters provided prior to this function call are switched out to be
// Meters provided by provider. All instruments and callbacks are recreated and
// delegated.
//
// It is guaranteed by the caller that this happens only once.
func (p *meterProvider) setDelegate(provider metric.MeterProvider) {
	p.mtx.Lock()
	defer p.mtx.Unlock()

	p.delegate = provider

	if len(p.meters) == 0 {
		return
	}

	for _, meter := range p.meters {
		meter.setDelegate(provider)
	}

	p.meters = nil
}

// Meter implements MeterProvider.
func (p *meterProvider) Meter(name string, opts ...metric.MeterOption) metric.Meter {
	p.mtx.Lock()
	defer p.mtx.Unlock()

	if p.delegate != nil {
		return p.delegate.Meter(name, opts...)
	}

	// At this moment it is guaranteed that no sdk is installed, save the meter in the meters map.

	c := metric.NewMeterConfig(opts...)
	key := il{
		name:    name,
		version: c.InstrumentationVersion(),
		schema:  c.SchemaURL(),
		attrs:   c.InstrumentationAttributes(),
	}

	if p.meters == nil {
		p.meters = make(map[il]*meter)
	}

	if val, ok := p.meters[key]; ok {
		return val
	}

	t := &meter{name: name, opts: opts, instruments: make(map[instID]delegatedInstrument)}
	p.meters[key] = t
	return t
}

// meter is a placeholder for a metric.Meter.
//
// All Meter functionality is forwarded to a delegate once configured.
// Otherwise, all functionality is forwarded to a NoopMeter.
type meter struct {
	embedded.Meter

	name string
	opts []metric.MeterOption

	mtx         sync.Mutex
	instruments map[instID]delegatedInstrument

	registry list.List

	delegate metric.Meter
}

type delegatedInstrument interface {
	setDelegate(metric.Meter)
}

// instID are the identifying properties of a instrument.
type instID struct {
	// name is the name of the stream.
	name string
	// description is the description of the stream.
	description string
	// kind defines the functional group of the instrument.
	kind reflect.Type
	// unit is the unit of the stream.
	unit string
}

// setDelegate configures m to delegate all Meter functionality to Meters
// created by provider.
//
// All subsequent calls to the Meter methods will be passed to the delegate.
//
// It is guaranteed by the caller that this happens only once.
func (m *meter) setDelegate(provider metric.MeterProvider) {
	m.mtx.Lock()
	defer m.mtx.Unlock()

	meter := provider.Meter(m.name, m.opts...)
	m.delegate = meter

	for _, inst := range m.instruments {
		inst.setDelegate(meter)
	}

	var n *list.Element
	for e := m.registry.Front(); e != nil; e = n {
		r := e.Value.(*registration)
		r.setDelegate(meter)
		n = e.Next()
		m.registry.Remove(e)
	}

	m.instruments = nil
	m.registry.Init()
}

func (m *meter) Int64Counter(name string, options ...metric.Int64CounterOption) (metric.Int64Counter, error) {
	m.mtx.Lock()
	defer m.mtx.Unlock()

	if m.delegate != nil {
		return m.delegate.Int64Counter(name, options...)
	}

	cfg := metric.NewInt64CounterConfig(options...)
	id := instID{
		name:        name,
		kind:        reflect.TypeOf((*siCounter)(nil)),
		description: cfg.Description(),
		unit:        cfg.Unit(),
	}
	if f, ok := m.instruments[id]; ok {
		return f.(metric.Int64Counter), nil
	}
	i := &siCounter{name: name, opts: options}
	m.instruments[id] = i
	return i, nil
}

func (m *meter) Int64UpDownCounter(name string, options ...metric.Int64UpDownCounterOption) (metric.Int64UpDownCounter, error) {
	m.mtx.Lock()
	defer m.mtx.Unlock()

	if m.delegate != nil {
		return m.delegate.Int64UpDownCounter(name, options...)
	}

	cfg := metric.NewInt64UpDownCounterConfig(options...)
	id := instID{
		name:        name,
		kind:        reflect.TypeOf((*siUpDownCounter)(nil)),
		description: cfg.Description(),
		unit:        cfg.Unit(),
	}
	if f, ok := m.instruments[id]; ok {
		return f.(metric.Int64UpDownCounter), nil
	}
	i := &siUpDownCounter{name: name, opts: options}
	m.instruments[id] = i
	return i, nil
}

func (m *meter) Int64Histogram(name string, options ...metric.Int64HistogramOption) (metric.Int64Histogram, error) {
	m.mtx.Lock()
	defer m.mtx.Unlock()

	if m.delegate != nil {
		return m.delegate.Int64Histogram(name, options...)
	}

	cfg := metric.NewInt64HistogramConfig(options...)
	id := instID{
		name:        name,
		kind:        reflect.TypeOf((*siHistogram)(nil)),
		description: cfg.Description(),
		unit:        cfg.Unit(),
	}
	if f, ok := m.instruments[id]; ok {
		return f.(metric.Int64Histogram), nil
	}
	i := &siHistogram{name: name, opts: options}
	m.instruments[id] = i
	return i, nil
}

func (m *meter) Int64Gauge(name string, options ...metric.Int64GaugeOption) (metric.Int64Gauge, error) {
	m.mtx.Lock()
	defer m.mtx.Unlock()

	if m.delegate != nil {
		return m.delegate.Int64Gauge(name, options...)
	}

	cfg := metric.NewInt64GaugeConfig(options...)
	id := instID{
		name:        name,
		kind:        reflect.TypeOf((*siGauge)(nil)),
		description: cfg.Description(),
		unit:        cfg.Unit(),
	}
	if f, ok := m.instruments[id]; ok {
		return f.(metric.Int64Gauge), nil
	}
	i := &siGauge{name: name, opts: options}
	m.instruments[id] = i
	return i, nil
}

func (m *meter) Int64ObservableCounter(name string, options ...metric.Int64ObservableCounterOption) (metric.Int64ObservableCounter, error) {
	m.mtx.Lock()
	defer m.mtx.Unlock()

	if m.delegate != nil {
		return m.delegate.Int64ObservableCounter(name, options...)
	}

	cfg := metric.NewInt64ObservableCounterConfig(options...)
	id := instID{
		name:        name,
		kind:        reflect.TypeOf((*aiCounter)(nil)),
		description: cfg.Description(),
		unit:        cfg.Unit(),
	}
	if f, ok := m.instruments[id]; ok {
		return f.(metric.Int64ObservableCounter), nil
	}
	i := &aiCounter{name: name, opts: options}
	m.instruments[id] = i
	return i, nil
}

func (m *meter) Int64ObservableUpDownCounter(name string, options ...metric.Int64ObservableUpDownCounterOption) (metric.Int64ObservableUpDownCounter, error) {
	m.mtx.Lock()
	defer m.mtx.Unlock()

	if m.delegate != nil {
		return m.delegate.Int64ObservableUpDownCounter(name, options...)
	}

	cfg := metric.NewInt64ObservableUpDownCounterConfig(options...)
	id := instID{
		name:        name,
		kind:        reflect.TypeOf((*aiUpDownCounter)(nil)),
		description: cfg.Description(),
		unit:        cfg.Unit(),
	}
	if f, ok := m.instruments[id]; ok {
		return f.(metric.Int64ObservableUpDownCounter), nil
	}
	i := &aiUpDownCounter{name: name, opts: options}
	m.instruments[id] = i
	return i, nil
}

func (m *meter) Int64ObservableGauge(name string, options ...metric.Int64ObservableGaugeOption) (metric.Int64ObservableGauge, error) {
	m.mtx.Lock()
	defer m.mtx.Unlock()

	if m.delegate != nil {
		return m.delegate.Int64ObservableGauge(name, options...)
	}

	cfg := metric.NewInt64ObservableGaugeConfig(options...)
	id := instID{
		name:        name,
		kind:        reflect.TypeOf((*aiGauge)(nil)),
		description: cfg.Description(),
		unit:        cfg.Unit(),
	}
	if f, ok := m.instruments[id]; ok {
		return f.(metric.Int64ObservableGauge), nil
	}
	i := &aiGauge{name: name, opts: options}
	m.instruments[id] = i
	return i, nil
}

func (m *meter) Float64Counter(name string, options ...metric.Float64CounterOption) (metric.Float64Counter, error) {
	m.mtx.Lock()
	defer m.mtx.Unlock()

	if m.delegate != nil {
		return m.delegate.Float64Counter(name, options...)
	}

	cfg := metric.NewFloat64CounterConfig(options...)
	id := instID{
		name:        name,
		kind:        reflect.TypeOf((*sfCounter)(nil)),
		description: cfg.Description(),
		unit:        cfg.Unit(),
	}
	if f, ok := m.instruments[id]; ok {
		return f.(metric.Float64Counter), nil
	}
	i := &sfCounter{name: name, opts: options}
	m.instruments[id] = i
	return i, nil
}

func (m *meter) Float64UpDownCounter(name string, options ...metric.Float64UpDownCounterOption) (metric.Float64UpDownCounter, error) {
	m.mtx.Lock()
	defer m.mtx.Unlock()

	if m.delegate != nil {
		return m.delegate.Float64UpDownCounter(name, options...)
	}

	cfg := metric.NewFloat64UpDownCounterConfig(options...)
	id := instID{
		name:        name,
		kind:        reflect.TypeOf((*sfUpDownCounter)(nil)),
		description: cfg.Description(),
		unit:        cfg.Unit(),
	}
	if f, ok := m.instruments[id]; ok {
		return f.(metric.Float64UpDownCounter), nil
	}
	i := &sfUpDownCounter{name: name, opts: options}
	m.instruments[id] = i
	return i, nil
}

func (m *meter) Float64Histogram(name string, options ...metric.Float64HistogramOption) (metric.Float64Histogram, error) {
	m.mtx.Lock()
	defer m.mtx.Unlock()

	if m.delegate != nil {
		return m.delegate.Float64Histogram(name, options...)
	}

	cfg := metric.NewFloat64HistogramConfig(options...)
	id := instID{
		name:        name,
		kind:        reflect.TypeOf((*sfHistogram)(nil)),
		description: cfg.Description(),
		unit:        cfg.Unit(),
	}
	if f, ok := m.instruments[id]; ok {
		return f.(metric.Float64Histogram), nil
	}
	i := &sfHistogram{name: name, opts: options}
	m.instruments[id] = i
	return i, nil
}

func (m *meter) Float64Gauge(name string, options ...metric.Float64GaugeOption) (metric.Float64Gauge, error) {
	m.mtx.Lock()
	defer m.mtx.Unlock()

	if m.delegate != nil {
		return m.delegate.Float64Gauge(name, options...)
	}

	cfg := metric.NewFloat64GaugeConfig(options...)
	id := instID{
		name:        name,
		kind:        reflect.TypeOf((*sfGauge)(nil)),
		description: cfg.Description(),
		unit:        cfg.Unit(),
	}
	if f, ok := m.instruments[id]; ok {
		return f.(metric.Float64Gauge), nil
	}
	i := &sfGauge{name: name, opts: options}
	m.instruments[id] = i
	return i, nil
}

func (m *meter) Float64ObservableCounter(name string, options ...metric.Float64ObservableCounterOption) (metric.Float64ObservableCounter, error) {
	m.mtx.Lock()
	defer m.mtx.Unlock()

	if m.delegate != nil {
		return m.delegate.Float64ObservableCounter(name, options...)
	}

	cfg := metric.NewFloat64ObservableCounterConfig(options...)
	id := instID{
		name:        name,
		kind:        reflect.TypeOf((*afCounter)(nil)),
		description: cfg.Description(),
		unit:        cfg.Unit(),
	}
	if f, ok := m.instruments[id]; ok {
		return f.(metric.Float64ObservableCounter), nil
	}
	i := &afCounter{name: name, opts: options}
	m.instruments[id] = i
	return i, nil
}

func (m *meter) Float64ObservableUpDownCounter(name string, options ...metric.Float64ObservableUpDownCounterOption) (metric.Float64ObservableUpDownCounter, error) {
	m.mtx.Lock()
	defer m.mtx.Unlock()

	if m.delegate != nil {
		return m.delegate.Float64ObservableUpDownCounter(name, options...)
	}

	cfg := metric.NewFloat64ObservableUpDownCounterConfig(options...)
	id := instID{
		name:        name,
		kind:        reflect.TypeOf((*afUpDownCounter)(nil)),
		description: cfg.Description(),
		unit:        cfg.Unit(),
	}
	if f, ok := m.instruments[id]; ok {
		return f.(metric.Float64ObservableUpDownCounter), nil
	}
	i := &afUpDownCounter{name: name, opts: options}
	m.instruments[id] = i
	return i, nil
}

func (m *meter) Float64ObservableGauge(name string, options ...metric.Float64ObservableGaugeOption) (metric.Float64ObservableGauge, error) {
	m.mtx.Lock()
	defer m.mtx.Unlock()

	if m.delegate != nil {
		return m.delegate.Float64ObservableGauge(name, options...)
	}

	cfg := metric.NewFloat64ObservableGaugeConfig(options...)
	id := instID{
		name:        name,
		kind:        reflect.TypeOf((*afGauge)(nil)),
		description: cfg.Description(),
		unit:        cfg.Unit(),
	}
	if f, ok := m.instruments[id]; ok {
		return f.(metric.Float64ObservableGauge), nil
	}
	i := &afGauge{name: name, opts: options}
	m.instruments[id] = i
	return i, nil
}

// RegisterCallback captures the function that will be called during Collect.
func (m *meter) RegisterCallback(f metric.Callback, insts ...metric.Observable) (metric.Registration, error) {
	m.mtx.Lock()
	defer m.mtx.Unlock()

	if m.delegate != nil {
		return m.delegate.RegisterCallback(unwrapCallback(f), unwrapInstruments(insts)...)
	}

	reg := &registration{instruments: insts, function: f}
	e := m.registry.PushBack(reg)
	reg.unreg = func() error {
		m.mtx.Lock()
		_ = m.registry.Remove(e)
		m.mtx.Unlock()
		return nil
	}
	return reg, nil
}

func unwrapInstruments(instruments []metric.Observable) []metric.Observable {
	out := make([]metric.Observable, 0, len(instruments))

	for _, inst := range instruments {
		if in, ok := inst.(unwrapper); ok {
			out = append(out, in.unwrap())
		} else {
			out = append(out, inst)
		}
	}

	return out
}

type registration struct {
	embedded.Registration

	instruments []metric.Observable
	function    metric.Callback

	unreg   func() error
	unregMu sync.Mutex
}

type unwrapObs struct {
	embedded.Observer
	obs metric.Observer
}

// unwrapFloat64Observable returns an expected metric.Float64Observable after
// unwrapping the global object.
func unwrapFloat64Observable(inst metric.Float64Observable) metric.Float64Observable {
	if unwrapped, ok := inst.(unwrapper); ok {
		if floatObs, ok := unwrapped.unwrap().(metric.Float64Observable); ok {
			// Note: if the unwrapped object does not
			// unwrap as an observable for either of the
			// predicates here, it means an internal bug in
			// this package.  We avoid logging an error in
			// this case, because the SDK has to try its
			// own type conversion on the object.  The SDK
			// will see this and be forced to respond with
			// its own error.
			//
			// This code uses a double-nested if statement
			// to avoid creating a branch that is
			// impossible to cover.
			inst = floatObs
		}
	}
	return inst
}

// unwrapInt64Observable returns an expected metric.Int64Observable after
// unwrapping the global object.
func unwrapInt64Observable(inst metric.Int64Observable) metric.Int64Observable {
	if unwrapped, ok := inst.(unwrapper); ok {
		if unint, ok := unwrapped.unwrap().(metric.Int64Observable); ok {
			// See the comment in unwrapFloat64Observable().
			inst = unint
		}
	}
	return inst
}

func (uo *unwrapObs) ObserveFloat64(inst metric.Float64Observable, value float64, opts ...metric.ObserveOption) {
	uo.obs.ObserveFloat64(unwrapFloat64Observable(inst), value, opts...)
}

func (uo *unwrapObs) ObserveInt64(inst metric.Int64Observable, value int64, opts ...metric.ObserveOption) {
	uo.obs.ObserveInt64(unwrapInt64Observable(inst), value, opts...)
}

func unwrapCallback(f metric.Callback) metric.Callback {
	return func(ctx context.Context, obs metric.Observer) error {
		return f(ctx, &unwrapObs{obs: obs})
	}
}

func (c *registration) setDelegate(m metric.Meter) {
	c.unregMu.Lock()
	defer c.unregMu.Unlock()

	if c.unreg == nil {
		// Unregister already called.
		return
	}

	reg, err := m.RegisterCallback(unwrapCallback(c.function), unwrapInstruments(c.instruments)...)
	if err != nil {
		GetErrorHandler().Handle(err)
		return
	}

	c.unreg = reg.Unregister
}

func (c *registration) Unregister() error {
	c.unregMu.Lock()
	defer c.unregMu.Unlock()
	if c.unreg == nil {
		// Unregister already called.
		return nil
	}

	var err error
	err, c.unreg = c.unreg(), nil
	return err
}
