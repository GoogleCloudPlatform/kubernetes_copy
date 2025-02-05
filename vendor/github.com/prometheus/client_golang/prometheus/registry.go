// Copyright 2014 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package prometheus

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/prometheus/client_golang/prometheus/internal"

	"github.com/cespare/xxhash/v2"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"google.golang.org/protobuf/proto"
)

const (
	// Capacity for the channel to collect metrics and descriptors.
	capMetricChan = 1000
	capDescChan   = 10
)

// DefaultRegisterer and DefaultGatherer are the implementations of the
// Registerer and Gatherer interface a number of convenience functions in this
// package act on. Initially, both variables point to the same Registry, which
// has a process collector (currently on Linux only, see NewProcessCollector)
// and a Go collector (see NewGoCollector, in particular the note about
// stop-the-world implication with Go versions older than 1.9) already
// registered. This approach to keep default instances as global state mirrors
// the approach of other packages in the Go standard library. Note that there
// are caveats. Change the variables with caution and only if you understand the
// consequences. Users who want to avoid global state altogether should not use
// the convenience functions and act on custom instances instead.
var (
	defaultRegistry              = NewRegistry()
	DefaultRegisterer Registerer = defaultRegistry
	DefaultGatherer   Gatherer   = defaultRegistry
)

func init() {
	MustRegister(NewProcessCollector(ProcessCollectorOpts{}))
	MustRegister(NewGoCollector())
}

// NewRegistry creates a new vanilla Registry without any Collectors
// pre-registered.
func NewRegistry() *Registry {
	return &Registry{
		collectorsByID:  map[uint64]Collector{},
		descIDs:         map[uint64]struct{}{},
		dimHashesByName: map[string]uint64{},
	}
}

// NewPedanticRegistry returns a registry that checks during collection if each
// collected Metric is consistent with its reported Desc, and if the Desc has
// actually been registered with the registry. Unchecked Collectors (those whose
// Describe method does not yield any descriptors) are excluded from the check.
//
// Usually, a Registry will be happy as long as the union of all collected
// Metrics is consistent and valid even if some metrics are not consistent with
// their own Desc or a Desc provided by their registered Collector. Well-behaved
// Collectors and Metrics will only provide consistent Descs. This Registry is
// useful to test the implementation of Collectors and Metrics.
func NewPedanticRegistry() *Registry {
	r := NewRegistry()
	r.pedanticChecksEnabled = true
	return r
}

// Registerer is the interface for the part of a registry in charge of
// registering and unregistering. Users of custom registries should use
// Registerer as type for registration purposes (rather than the Registry type
// directly). In that way, they are free to use custom Registerer implementation
// (e.g. for testing purposes).
type Registerer interface {
	// Register registers a new Collector to be included in metrics
	// collection. It returns an error if the descriptors provided by the
	// Collector are invalid or if they — in combination with descriptors of
	// already registered Collectors — do not fulfill the consistency and
	// uniqueness criteria described in the documentation of metric.Desc.
	//
	// If the provided Collector is equal to a Collector already registered
	// (which includes the case of re-registering the same Collector), the
	// returned error is an instance of AlreadyRegisteredError, which
	// contains the previously registered Collector.
	//
	// A Collector whose Describe method does not yield any Desc is treated
	// as unchecked. Registration will always succeed. No check for
	// re-registering (see previous paragraph) is performed. Thus, the
	// caller is responsible for not double-registering the same unchecked
	// Collector, and for providing a Collector that will not cause
	// inconsistent metrics on collection. (This would lead to scrape
	// errors.)
	Register(Collector) error
	// MustRegister works like Register but registers any number of
	// Collectors and panics upon the first registration that causes an
	// error.
	MustRegister(...Collector)
	// Unregister unregisters the Collector that equals the Collector passed
	// in as an argument.  (Two Collectors are considered equal if their
	// Describe method yields the same set of descriptors.) The function
	// returns whether a Collector was unregistered. Note that an unchecked
	// Collector cannot be unregistered (as its Describe method does not
	// yield any descriptor).
	//
	// Note that even after unregistering, it will not be possible to
	// register a new Collector that is inconsistent with the unregistered
	// Collector, e.g. a Collector collecting metrics with the same name but
	// a different help string. The rationale here is that the same registry
	// instance must only collect consistent metrics throughout its
	// lifetime.
	Unregister(Collector) bool
}

// Gatherer is the interface for the part of a registry in charge of gathering
// the collected metrics into a number of MetricFamilies. The Gatherer interface
// comes with the same general implication as described for the Registerer
// interface.
type Gatherer interface {
	// Gather calls the Collect method of the registered Collectors and then
	// gathers the collected metrics into a lexicographically sorted slice
	// of uniquely named MetricFamily protobufs. Gather ensures that the
	// returned slice is valid and self-consistent so that it can be used
	// for valid exposition. As an exception to the strict consistency
	// requirements described for metric.Desc, Gather will tolerate
	// different sets of label names for metrics of the same metric family.
	//
	// Even if an error occurs, Gather attempts to gather as many metrics as
	// possible. Hence, if a non-nil error is returned, the returned
	// MetricFamily slice could be nil (in case of a fatal error that
	// prevented any meaningful metric collection) or contain a number of
	// MetricFamily protobufs, some of which might be incomplete, and some
	// might be missing altogether. The returned error (which might be a
	// MultiError) explains the details. Note that this is mostly useful for
	// debugging purposes. If the gathered protobufs are to be used for
	// exposition in actual monitoring, it is almost always better to not
	// expose an incomplete result and instead disregard the returned
	// MetricFamily protobufs in case the returned error is non-nil.
	Gather() ([]*dto.MetricFamily, error)
}

// Register registers the provided Collector with the DefaultRegisterer.
//
// Register is a shortcut for DefaultRegisterer.Register(c). See there for more
// details.
func Register(c Collector) error {
	return DefaultRegisterer.Register(c)
}

// MustRegister registers the provided Collectors with the DefaultRegisterer and
// panics if any error occurs.
//
// MustRegister is a shortcut for DefaultRegisterer.MustRegister(cs...). See
// there for more details.
func MustRegister(cs ...Collector) {
	DefaultRegisterer.MustRegister(cs...)
}

// Unregister removes the registration of the provided Collector from the
// DefaultRegisterer.
//
// Unregister is a shortcut for DefaultRegisterer.Unregister(c). See there for
// more details.
func Unregister(c Collector) bool {
	return DefaultRegisterer.Unregister(c)
}

// GathererFunc turns a function into a Gatherer.
type GathererFunc func() ([]*dto.MetricFamily, error)

// Gather implements Gatherer.
func (gf GathererFunc) Gather() ([]*dto.MetricFamily, error) {
	return gf()
}

// AlreadyRegisteredError is returned by the Register method if the Collector to
// be registered has already been registered before, or a different Collector
// that collects the same metrics has been registered before. Registration fails
// in that case, but you can detect from the kind of error what has
// happened. The error contains fields for the existing Collector and the
// (rejected) new Collector that equals the existing one. This can be used to
// find out if an equal Collector has been registered before and switch over to
// using the old one, as demonstrated in the example.
type AlreadyRegisteredError struct {
	ExistingCollector, NewCollector Collector
}

func (err AlreadyRegisteredError) Error() string {
	return "duplicate metrics collector registration attempted"
}

// MultiError is a slice of errors implementing the error interface. It is used
// by a Gatherer to report multiple errors during MetricFamily gathering.
type MultiError []error

// Error formats the contained errors as a bullet point list, preceded by the
// total number of errors. Note that this results in a multi-line string.
func (errs MultiError) Error() string {
	if len(errs) == 0 {
		return ""
	}
	buf := &bytes.Buffer{}
	fmt.Fprintf(buf, "%d error(s) occurred:", len(errs))
	for _, err := range errs {
		fmt.Fprintf(buf, "\n* %s", err)
	}
	return buf.String()
}

// Append appends the provided error if it is not nil.
func (errs *MultiError) Append(err error) {
	if err != nil {
		*errs = append(*errs, err)
	}
}

// MaybeUnwrap returns nil if len(errs) is 0. It returns the first and only
// contained error as error if len(errs is 1). In all other cases, it returns
// the MultiError directly. This is helpful for returning a MultiError in a way
// that only uses the MultiError if needed.
func (errs MultiError) MaybeUnwrap() error {
	switch len(errs) {
	case 0:
		return nil
	case 1:
		return errs[0]
	default:
		return errs
	}
}

// Registry registers Prometheus collectors, collects their metrics, and gathers
// them into MetricFamilies for exposition. It implements Registerer, Gatherer,
// and Collector. The zero value is not usable. Create instances with
// NewRegistry or NewPedanticRegistry.
//
// Registry implements Collector to allow it to be used for creating groups of
// metrics. See the Grouping example for how this can be done.
type Registry struct {
	mtx                   sync.RWMutex
	collectorsByID        map[uint64]Collector // ID is a hash of the descIDs.
	descIDs               map[uint64]struct{}
	dimHashesByName       map[string]uint64
	uncheckedCollectors   []Collector
	pedanticChecksEnabled bool
}

// Register implements Registerer.
func (r *Registry) Register(c Collector) error {
	var (
		descChan           = make(chan *Desc, capDescChan)
		newDescIDs         = map[uint64]struct{}{}
		newDimHashesByName = map[string]uint64{}
		collectorID        uint64 // All desc IDs XOR'd together.
		duplicateDescErr   error
	)
	go func() {
		c.Describe(descChan)
		close(descChan)
	}()
	r.mtx.Lock()
	defer func() {
		// Drain channel in case of premature return to not leak a goroutine.
		for range descChan {
		}
		r.mtx.Unlock()
	}()
	// Conduct various tests...
	for desc := range descChan {

		// Is the descriptor valid at all?
		if desc.err != nil {
			return fmt.Errorf("descriptor %s is invalid: %w", desc, desc.err)
		}

		// Is the descID unique?
		// (In other words: Is the fqName + constLabel combination unique?)
		if _, exists := r.descIDs[desc.id]; exists {
			duplicateDescErr = fmt.Errorf("descriptor %s already exists with the same fully-qualified name and const label values", desc)
		}
		// If it is not a duplicate desc in this collector, XOR it to
		// the collectorID.  (We allow duplicate descs within the same
		// collector, but their existence must be a no-op.)
		if _, exists := newDescIDs[desc.id]; !exists {
			newDescIDs[desc.id] = struct{}{}
			collectorID ^= desc.id
		}

		// Are all the label names and the help string consistent with
		// previous descriptors of the same name?
		// First check existing descriptors...
		if dimHash, exists := r.dimHashesByName[desc.fqName]; exists {
			if dimHash != desc.dimHash {
				return fmt.Errorf("a previously registered descriptor with the same fully-qualified name as %s has different label names or a different help string", desc)
			}
			continue
		}

		// ...then check the new descriptors already seen.
		if dimHash, exists := newDimHashesByName[desc.fqName]; exists {
			if dimHash != desc.dimHash {
				return fmt.Errorf("descriptors reported by collector have inconsistent label names or help strings for the same fully-qualified name, offender is %s", desc)
			}
			continue
		}
		newDimHashesByName[desc.fqName] = desc.dimHash
	}
	// A Collector yielding no Desc at all is considered unchecked.
	if len(newDescIDs) == 0 {
		r.uncheckedCollectors = append(r.uncheckedCollectors, c)
		return nil
	}
	if existing, exists := r.collectorsByID[collectorID]; exists {
		switch e := existing.(type) {
		case *wrappingCollector:
			return AlreadyRegisteredError{
				ExistingCollector: e.unwrapRecursively(),
				NewCollector:      c,
			}
		default:
			return AlreadyRegisteredError{
				ExistingCollector: e,
				NewCollector:      c,
			}
		}
	}
	// If the collectorID is new, but at least one of the descs existed
	// before, we are in trouble.
	if duplicateDescErr != nil {
		return duplicateDescErr
	}

	// Only after all tests have passed, actually register.
	r.collectorsByID[collectorID] = c
	for hash := range newDescIDs {
		r.descIDs[hash] = struct{}{}
	}
	for name, dimHash := range newDimHashesByName {
		r.dimHashesByName[name] = dimHash
	}
	return nil
}

// Unregister implements Registerer.
func (r *Registry) Unregister(c Collector) bool {
	var (
		descChan    = make(chan *Desc, capDescChan)
		descIDs     = map[uint64]struct{}{}
		collectorID uint64 // All desc IDs XOR'd together.
	)
	go func() {
		c.Describe(descChan)
		close(descChan)
	}()
	for desc := range descChan {
		if _, exists := descIDs[desc.id]; !exists {
			collectorID ^= desc.id
			descIDs[desc.id] = struct{}{}
		}
	}

	r.mtx.RLock()
	if _, exists := r.collectorsByID[collectorID]; !exists {
		r.mtx.RUnlock()
		return false
	}
	r.mtx.RUnlock()

	r.mtx.Lock()
	defer r.mtx.Unlock()

	delete(r.collectorsByID, collectorID)
	for id := range descIDs {
		delete(r.descIDs, id)
	}
	// dimHashesByName is left untouched as those must be consistent
	// throughout the lifetime of a program.
	return true
}

// MustRegister implements Registerer.
func (r *Registry) MustRegister(cs ...Collector) {
	for _, c := range cs {
		if err := r.Register(c); err != nil {
			panic(err)
		}
	}
}

// Gather implements Gatherer.
func (r *Registry) Gather() ([]*dto.MetricFamily, error) {
	r.mtx.RLock()

	if len(r.collectorsByID) == 0 && len(r.uncheckedCollectors) == 0 {
		// Fast path.
		r.mtx.RUnlock()
		return nil, nil
	}

	var (
		checkedMetricChan   = make(chan Metric, capMetricChan)
		uncheckedMetricChan = make(chan Metric, capMetricChan)
		metricHashes        = map[uint64]struct{}{}
		wg                  sync.WaitGroup
		errs                MultiError          // The collected errors to return in the end.
		registeredDescIDs   map[uint64]struct{} // Only used for pedantic checks
	)

	goroutineBudget := len(r.collectorsByID) + len(r.uncheckedCollectors)
	metricFamiliesByName := make(map[string]*dto.MetricFamily, len(r.dimHashesByName))
	checkedCollectors := make(chan Collector, len(r.collectorsByID))
	uncheckedCollectors := make(chan Collector, len(r.uncheckedCollectors))
	for _, collector := range r.collectorsByID {
		checkedCollectors <- collector
	}
	for _, collector := range r.uncheckedCollectors {
		uncheckedCollectors <- collector
	}
	// In case pedantic checks are enabled, we have to copy the map before
	// giving up the RLock.
	if r.pedanticChecksEnabled {
		registeredDescIDs = make(map[uint64]struct{}, len(r.descIDs))
		for id := range r.descIDs {
			registeredDescIDs[id] = struct{}{}
		}
	}
	r.mtx.RUnlock()

	wg.Add(goroutineBudget)

	collectWorker := func() {
		for {
			select {
			case collector := <-checkedCollectors:
				collector.Collect(checkedMetricChan)
			case collector := <-uncheckedCollectors:
				collector.Collect(uncheckedMetricChan)
			default:
				return
			}
			wg.Done()
		}
	}

	// Start the first worker now to make sure at least one is running.
	go collectWorker()
	goroutineBudget--

	// Close checkedMetricChan and uncheckedMetricChan once all collectors
	// are collected.
	go func() {
		wg.Wait()
		close(checkedMetricChan)
		close(uncheckedMetricChan)
	}()

	// Drain checkedMetricChan and uncheckedMetricChan in case of premature return.
	defer func() {
		if checkedMetricChan != nil {
			for range checkedMetricChan {
			}
		}
		if uncheckedMetricChan != nil {
			for range uncheckedMetricChan {
			}
		}
	}()

	// Copy the channel references so we can nil them out later to remove
	// them from the select statements below.
	cmc := checkedMetricChan
	umc := uncheckedMetricChan

	for {
		select {
		case metric, ok := <-cmc:
			if !ok {
				cmc = nil
				break
			}
			errs.Append(processMetric(
				metric, metricFamiliesByName,
				metricHashes,
				registeredDescIDs,
			))
		case metric, ok := <-umc:
			if !ok {
				umc = nil
				break
			}
			errs.Append(processMetric(
				metric, metricFamiliesByName,
				metricHashes,
				nil,
			))
		default:
			if goroutineBudget <= 0 || len(checkedCollectors)+len(uncheckedCollectors) == 0 {
				// All collectors are already being worked on or
				// we have already as many goroutines started as
				// there are collectors. Do the same as above,
				// just without the default.
				select {
				case metric, ok := <-cmc:
					if !ok {
						cmc = nil
						break
					}
					errs.Append(processMetric(
						metric, metricFamiliesByName,
						metricHashes,
						registeredDescIDs,
					))
				case metric, ok := <-umc:
					if !ok {
						umc = nil
						break
					}
					errs.Append(processMetric(
						metric, metricFamiliesByName,
						metricHashes,
						nil,
					))
				}
				break
			}
			// Start more workers.
			go collectWorker()
			goroutineBudget--
			runtime.Gosched()
		}
		// Once both checkedMetricChan and uncheckedMetricChan are closed
		// and drained, the contraption above will nil out cmc and umc,
		// and then we can leave the collect loop here.
		if cmc == nil && umc == nil {
			break
		}
	}
	return internal.NormalizeMetricFamilies(metricFamiliesByName), errs.MaybeUnwrap()
}

// Describe implements Collector.
func (r *Registry) Describe(ch chan<- *Desc) {
	r.mtx.RLock()
	defer r.mtx.RUnlock()

	// Only report the checked Collectors; unchecked collectors don't report any
	// Desc.
	for _, c := range r.collectorsByID {
		c.Describe(ch)
	}
}

// Collect implements Collector.
func (r *Registry) Collect(ch chan<- Metric) {
	r.mtx.RLock()
	defer r.mtx.RUnlock()

	for _, c := range r.collectorsByID {
		c.Collect(ch)
	}
	for _, c := range r.uncheckedCollectors {
		c.Collect(ch)
	}
}

// WriteToTextfile calls Gather on the provided Gatherer, encodes the result in the
// Prometheus text format, and writes it to a temporary file. Upon success, the
// temporary file is renamed to the provided filename.
//
// This is intended for use with the textfile collector of the node exporter.
// Note that the node exporter expects the filename to be suffixed with ".prom".
func WriteToTextfile(filename string, g Gatherer) error {
	tmp, err := os.CreateTemp(filepath.Dir(filename), filepath.Base(filename))
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())

	mfs, err := g.Gather()
	if err != nil {
		return err
	}
	for _, mf := range mfs {
		if _, err := expfmt.MetricFamilyToText(tmp, mf); err != nil {
			return err
		}
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filename)
}

// processMetric is an internal helper method only used by the Gather method.
func processMetric(
	metric Metric,
	metricFamiliesByName map[string]*dto.MetricFamily,
	metricHashes map[uint64]struct{},
	registeredDescIDs map[uint64]struct{},
) error {
	desc := metric.Desc()
	// Wrapped metrics collected by an unchecked Collector can have an
	// invalid Desc.
	if desc.err != nil {
		return desc.err
	}
	dtoMetric := &dto.Metric{}
	if err := metric.Write(dtoMetric); err != nil {
		return fmt.Errorf("error collecting metric %v: %w", desc, err)
	}
	metricFamily, ok := metricFamiliesByName[desc.fqName]
	if ok { // Existing name.
		if metricFamily.GetHelp() != desc.help {
			return fmt.Errorf(
				"collected metric %s %s has help %q but should have %q",
				desc.fqName, dtoMetric, desc.help, metricFamily.GetHelp(),
			)
		}
		// TODO(beorn7): Simplify switch once Desc has type.
		switch metricFamily.GetType() {
		case dto.MetricType_COUNTER:
			if dtoMetric.Counter == nil {
				return fmt.Errorf(
					"collected metric %s %s should be a Counter",
					desc.fqName, dtoMetric,
				)
			}
		case dto.MetricType_GAUGE:
			if dtoMetric.Gauge == nil {
				return fmt.Errorf(
					"collected metric %s %s should be a Gauge",
					desc.fqName, dtoMetric,
				)
			}
		case dto.MetricType_SUMMARY:
			if dtoMetric.Summary == nil {
				return fmt.Errorf(
					"collected metric %s %s should be a Summary",
					desc.fqName, dtoMetric,
				)
			}
		case dto.MetricType_UNTYPED:
			if dtoMetric.Untyped == nil {
				return fmt.Errorf(
					"collected metric %s %s should be Untyped",
					desc.fqName, dtoMetric,
				)
			}
		case dto.MetricType_HISTOGRAM:
			if dtoMetric.Histogram == nil {
				return fmt.Errorf(
					"collected metric %s %s should be a Histogram",
					desc.fqName, dtoMetric,
				)
			}
		default:
			panic("encountered MetricFamily with invalid type")
		}
	} else { // New name.
		metricFamily = &dto.MetricFamily{}
		metricFamily.Name = proto.String(desc.fqName)
		metricFamily.Help = proto.String(desc.help)
		// TODO(beorn7): Simplify switch once Desc has type.
		switch {
		case dtoMetric.Gauge != nil:
			metricFamily.Type = dto.MetricType_GAUGE.Enum()
		case dtoMetric.Counter != nil:
			metricFamily.Type = dto.MetricType_COUNTER.Enum()
		case dtoMetric.Summary != nil:
			metricFamily.Type = dto.MetricType_SUMMARY.Enum()
		case dtoMetric.Untyped != nil:
			metricFamily.Type = dto.MetricType_UNTYPED.Enum()
		case dtoMetric.Histogram != nil:
			metricFamily.Type = dto.MetricType_HISTOGRAM.Enum()
		default:
			return fmt.Errorf("empty metric collected: %s", dtoMetric)
		}
		if err := checkSuffixCollisions(metricFamily, metricFamiliesByName); err != nil {
			return err
		}
		metricFamiliesByName[desc.fqName] = metricFamily
	}
	if err := checkMetricConsistency(metricFamily, dtoMetric, metricHashes); err != nil {
		return err
	}
	if registeredDescIDs != nil {
		// Is the desc registered at all?
		if _, exist := registeredDescIDs[desc.id]; !exist {
			return fmt.Errorf(
				"collected metric %s %s with unregistered descriptor %s",
				metricFamily.GetName(), dtoMetric, desc,
			)
		}
		if err := checkDescConsistency(metricFamily, dtoMetric, desc); err != nil {
			return err
		}
	}
	metricFamily.Metric = append(metricFamily.Metric, dtoMetric)
	return nil
}

// Gatherers is a slice of Gatherer instances that implements the Gatherer
// interface itself. Its Gather method calls Gather on all Gatherers in the
// slice in order and returns the merged results. Errors returned from the
// Gather calls are all returned in a flattened MultiError. Duplicate and
// inconsistent Metrics are skipped (first occurrence in slice order wins) and
// reported in the returned error.
//
// Gatherers can be used to merge the Gather results from multiple
// Registries. It also provides a way to directly inject existing MetricFamily
// protobufs into the gathering by creating a custom Gatherer with a Gather
// method that simply returns the existing MetricFamily protobufs. Note that no
// registration is involved (in contrast to Collector registration), so
// obviously registration-time checks cannot happen. Any inconsistencies between
// the gathered MetricFamilies are reported as errors by the Gather method, and
// inconsistent Metrics are dropped. Invalid parts of the MetricFamilies
// (e.g. syntactically invalid metric or label names) will go undetected.
type Gatherers []Gatherer

// Gather implements Gatherer.
func (gs Gatherers) Gather() ([]*dto.MetricFamily, error) {
	var (
		metricFamiliesByName = map[string]*dto.MetricFamily{}
		metricHashes         = map[uint64]struct{}{}
		errs                 MultiError // The collected errors to return in the end.
	)

	for i, g := range gs {
		mfs, err := g.Gather()
		if err != nil {
			multiErr := MultiError{}
			if errors.As(err, &multiErr) {
				for _, err := range multiErr {
					errs = append(errs, fmt.Errorf("[from Gatherer #%d] %w", i+1, err))
				}
			} else {
				errs = append(errs, fmt.Errorf("[from Gatherer #%d] %w", i+1, err))
			}
		}
		for _, mf := range mfs {
			existingMF, exists := metricFamiliesByName[mf.GetName()]
			if exists {
				if existingMF.GetHelp() != mf.GetHelp() {
					errs = append(errs, fmt.Errorf(
						"gathered metric family %s has help %q but should have %q",
						mf.GetName(), mf.GetHelp(), existingMF.GetHelp(),
					))
					continue
				}
				if existingMF.GetType() != mf.GetType() {
					errs = append(errs, fmt.Errorf(
						"gathered metric family %s has type %s but should have %s",
						mf.GetName(), mf.GetType(), existingMF.GetType(),
					))
					continue
				}
			} else {
				existingMF = &dto.MetricFamily{}
				existingMF.Name = mf.Name
				existingMF.Help = mf.Help
				existingMF.Type = mf.Type
				if err := checkSuffixCollisions(existingMF, metricFamiliesByName); err != nil {
					errs = append(errs, err)
					continue
				}
				metricFamiliesByName[mf.GetName()] = existingMF
			}
			for _, m := range mf.Metric {
				if err := checkMetricConsistency(existingMF, m, metricHashes); err != nil {
					errs = append(errs, err)
					continue
				}
				existingMF.Metric = append(existingMF.Metric, m)
			}
		}
	}
	return internal.NormalizeMetricFamilies(metricFamiliesByName), errs.MaybeUnwrap()
}

// checkSuffixCollisions checks for collisions with the “magic” suffixes the
// Prometheus text format and the internal metric representation of the
// Prometheus server add while flattening Summaries and Histograms.
func checkSuffixCollisions(mf *dto.MetricFamily, mfs map[string]*dto.MetricFamily) error {
	var (
		newName              = mf.GetName()
		newType              = mf.GetType()
		newNameWithoutSuffix = ""
	)
	switch {
	case strings.HasSuffix(newName, "_count"):
		newNameWithoutSuffix = newName[:len(newName)-6]
	case strings.HasSuffix(newName, "_sum"):
		newNameWithoutSuffix = newName[:len(newName)-4]
	case strings.HasSuffix(newName, "_bucket"):
		newNameWithoutSuffix = newName[:len(newName)-7]
	}
	if newNameWithoutSuffix != "" {
		if existingMF, ok := mfs[newNameWithoutSuffix]; ok {
			switch existingMF.GetType() {
			case dto.MetricType_SUMMARY:
				if !strings.HasSuffix(newName, "_bucket") {
					return fmt.Errorf(
						"collected metric named %q collides with previously collected summary named %q",
						newName, newNameWithoutSuffix,
					)
				}
			case dto.MetricType_HISTOGRAM:
				return fmt.Errorf(
					"collected metric named %q collides with previously collected histogram named %q",
					newName, newNameWithoutSuffix,
				)
			}
		}
	}
	if newType == dto.MetricType_SUMMARY || newType == dto.MetricType_HISTOGRAM {
		if _, ok := mfs[newName+"_count"]; ok {
			return fmt.Errorf(
				"collected histogram or summary named %q collides with previously collected metric named %q",
				newName, newName+"_count",
			)
		}
		if _, ok := mfs[newName+"_sum"]; ok {
			return fmt.Errorf(
				"collected histogram or summary named %q collides with previously collected metric named %q",
				newName, newName+"_sum",
			)
		}
	}
	if newType == dto.MetricType_HISTOGRAM {
		if _, ok := mfs[newName+"_bucket"]; ok {
			return fmt.Errorf(
				"collected histogram named %q collides with previously collected metric named %q",
				newName, newName+"_bucket",
			)
		}
	}
	return nil
}

// checkMetricConsistency checks if the provided Metric is consistent with the
// provided MetricFamily. It also hashes the Metric labels and the MetricFamily
// name. If the resulting hash is already in the provided metricHashes, an error
// is returned. If not, it is added to metricHashes.
func checkMetricConsistency(
	metricFamily *dto.MetricFamily,
	dtoMetric *dto.Metric,
	metricHashes map[uint64]struct{},
) error {
	name := metricFamily.GetName()

	// Type consistency with metric family.
	if metricFamily.GetType() == dto.MetricType_GAUGE && dtoMetric.Gauge == nil ||
		metricFamily.GetType() == dto.MetricType_COUNTER && dtoMetric.Counter == nil ||
		metricFamily.GetType() == dto.MetricType_SUMMARY && dtoMetric.Summary == nil ||
		metricFamily.GetType() == dto.MetricType_HISTOGRAM && dtoMetric.Histogram == nil ||
		metricFamily.GetType() == dto.MetricType_UNTYPED && dtoMetric.Untyped == nil {
		return fmt.Errorf(
			"collected metric %q { %s} is not a %s",
			name, dtoMetric, metricFamily.GetType(),
		)
	}

	previousLabelName := ""
	for _, labelPair := range dtoMetric.GetLabel() {
		labelName := labelPair.GetName()
		if labelName == previousLabelName {
			return fmt.Errorf(
				"collected metric %q { %s} has two or more labels with the same name: %s",
				name, dtoMetric, labelName,
			)
		}
		if !checkLabelName(labelName) {
			return fmt.Errorf(
				"collected metric %q { %s} has a label with an invalid name: %s",
				name, dtoMetric, labelName,
			)
		}
		if dtoMetric.Summary != nil && labelName == quantileLabel {
			return fmt.Errorf(
				"collected metric %q { %s} must not have an explicit %q label",
				name, dtoMetric, quantileLabel,
			)
		}
		if !utf8.ValidString(labelPair.GetValue()) {
			return fmt.Errorf(
				"collected metric %q { %s} has a label named %q whose value is not utf8: %#v",
				name, dtoMetric, labelName, labelPair.GetValue())
		}
		previousLabelName = labelName
	}

	// Is the metric unique (i.e. no other metric with the same name and the same labels)?
	h := xxhash.New()
	h.WriteString(name)
	h.Write(separatorByteSlice)
	// Make sure label pairs are sorted. We depend on it for the consistency
	// check.
	if !sort.IsSorted(internal.LabelPairSorter(dtoMetric.Label)) {
		// We cannot sort dtoMetric.Label in place as it is immutable by contract.
		copiedLabels := make([]*dto.LabelPair, len(dtoMetric.Label))
		copy(copiedLabels, dtoMetric.Label)
		sort.Sort(internal.LabelPairSorter(copiedLabels))
		dtoMetric.Label = copiedLabels
	}
	for _, lp := range dtoMetric.Label {
		h.WriteString(lp.GetName())
		h.Write(separatorByteSlice)
		h.WriteString(lp.GetValue())
		h.Write(separatorByteSlice)
	}
	if dtoMetric.TimestampMs != nil {
		h.WriteString(strconv.FormatInt(*(dtoMetric.TimestampMs), 10))
		h.Write(separatorByteSlice)
	}
	hSum := h.Sum64()
	if _, exists := metricHashes[hSum]; exists {
		return fmt.Errorf(
			"collected metric %q { %s} was collected before with the same name and label values",
			name, dtoMetric,
		)
	}
	metricHashes[hSum] = struct{}{}
	return nil
}

func checkDescConsistency(
	metricFamily *dto.MetricFamily,
	dtoMetric *dto.Metric,
	desc *Desc,
) error {
	// Desc help consistency with metric family help.
	if metricFamily.GetHelp() != desc.help {
		return fmt.Errorf(
			"collected metric %s %s has help %q but should have %q",
			metricFamily.GetName(), dtoMetric, metricFamily.GetHelp(), desc.help,
		)
	}

	// Is the desc consistent with the content of the metric?
	lpsFromDesc := make([]*dto.LabelPair, len(desc.constLabelPairs), len(dtoMetric.Label))
	copy(lpsFromDesc, desc.constLabelPairs)
	for _, l := range desc.variableLabels.names {
		lpsFromDesc = append(lpsFromDesc, &dto.LabelPair{
			Name: proto.String(l),
		})
	}
	if len(lpsFromDesc) != len(dtoMetric.Label) {
		return fmt.Errorf(
			"labels in collected metric %s %s are inconsistent with descriptor %s",
			metricFamily.GetName(), dtoMetric, desc,
		)
	}
	sort.Sort(internal.LabelPairSorter(lpsFromDesc))
	for i, lpFromDesc := range lpsFromDesc {
		lpFromMetric := dtoMetric.Label[i]
		if lpFromDesc.GetName() != lpFromMetric.GetName() ||
			lpFromDesc.Value != nil && lpFromDesc.GetValue() != lpFromMetric.GetValue() {
			return fmt.Errorf(
				"labels in collected metric %s %s are inconsistent with descriptor %s",
				metricFamily.GetName(), dtoMetric, desc,
			)
		}
	}
	return nil
}

var _ TransactionalGatherer = &MultiTRegistry{}

// MultiTRegistry is a TransactionalGatherer that joins gathered metrics from multiple
// transactional gatherers.
//
// It is caller responsibility to ensure two registries have mutually exclusive metric families,
// no deduplication will happen.
type MultiTRegistry struct {
	tGatherers []TransactionalGatherer
}

// NewMultiTRegistry creates MultiTRegistry.
func NewMultiTRegistry(tGatherers ...TransactionalGatherer) *MultiTRegistry {
	return &MultiTRegistry{
		tGatherers: tGatherers,
	}
}

// Gather implements TransactionalGatherer interface.
func (r *MultiTRegistry) Gather() (mfs []*dto.MetricFamily, done func(), err error) {
	errs := MultiError{}

	dFns := make([]func(), 0, len(r.tGatherers))
	// TODO(bwplotka): Implement concurrency for those?
	for _, g := range r.tGatherers {
		// TODO(bwplotka): Check for duplicates?
		m, d, err := g.Gather()
		errs.Append(err)

		mfs = append(mfs, m...)
		dFns = append(dFns, d)
	}

	// TODO(bwplotka): Consider sort in place, given metric family in gather is sorted already.
	sort.Slice(mfs, func(i, j int) bool {
		return *mfs[i].Name < *mfs[j].Name
	})
	return mfs, func() {
		for _, d := range dFns {
			d()
		}
	}, errs.MaybeUnwrap()
}

// TransactionalGatherer represents transactional gatherer that can be triggered to notify gatherer that memory
// used by metric family is no longer used by a caller. This allows implementations with cache.
type TransactionalGatherer interface {
	// Gather returns metrics in a lexicographically sorted slice
	// of uniquely named MetricFamily protobufs. Gather ensures that the
	// returned slice is valid and self-consistent so that it can be used
	// for valid exposition. As an exception to the strict consistency
	// requirements described for metric.Desc, Gather will tolerate
	// different sets of label names for metrics of the same metric family.
	//
	// Even if an error occurs, Gather attempts to gather as many metrics as
	// possible. Hence, if a non-nil error is returned, the returned
	// MetricFamily slice could be nil (in case of a fatal error that
	// prevented any meaningful metric collection) or contain a number of
	// MetricFamily protobufs, some of which might be incomplete, and some
	// might be missing altogether. The returned error (which might be a
	// MultiError) explains the details. Note that this is mostly useful for
	// debugging purposes. If the gathered protobufs are to be used for
	// exposition in actual monitoring, it is almost always better to not
	// expose an incomplete result and instead disregard the returned
	// MetricFamily protobufs in case the returned error is non-nil.
	//
	// Important: done is expected to be triggered (even if the error occurs!)
	// once caller does not need returned slice of dto.MetricFamily.
	Gather() (_ []*dto.MetricFamily, done func(), err error)
}

// ToTransactionalGatherer transforms Gatherer to transactional one with noop as done function.
func ToTransactionalGatherer(g Gatherer) TransactionalGatherer {
	return &noTransactionGatherer{g: g}
}

type noTransactionGatherer struct {
	g Gatherer
}

// Gather implements TransactionalGatherer interface.
func (g *noTransactionGatherer) Gather() (_ []*dto.MetricFamily, done func(), err error) {
	mfs, err := g.g.Gather()
	return mfs, func() {}, err
}
