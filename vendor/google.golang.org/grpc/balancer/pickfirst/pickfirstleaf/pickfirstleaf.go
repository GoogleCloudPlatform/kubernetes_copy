/*
 *
 * Copyright 2024 gRPC authors.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 */

// Package pickfirstleaf contains the pick_first load balancing policy which
// will be the universal leaf policy after dualstack changes are implemented.
//
// # Experimental
//
// Notice: This package is EXPERIMENTAL and may be changed or removed in a
// later release.
package pickfirstleaf

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"google.golang.org/grpc/balancer"
	"google.golang.org/grpc/balancer/pickfirst/internal"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/grpclog"
	"google.golang.org/grpc/internal/envconfig"
	internalgrpclog "google.golang.org/grpc/internal/grpclog"
	"google.golang.org/grpc/internal/pretty"
	"google.golang.org/grpc/resolver"
	"google.golang.org/grpc/serviceconfig"
)

func init() {
	if envconfig.NewPickFirstEnabled {
		// Register as the default pick_first balancer.
		Name = "pick_first"
	}
	balancer.Register(pickfirstBuilder{})
}

var (
	logger = grpclog.Component("pick-first-leaf-lb")
	// Name is the name of the pick_first_leaf balancer.
	// It is changed to "pick_first" in init() if this balancer is to be
	// registered as the default pickfirst.
	Name = "pick_first_leaf"
)

// TODO: change to pick-first when this becomes the default pick_first policy.
const logPrefix = "[pick-first-leaf-lb %p] "

type pickfirstBuilder struct{}

func (pickfirstBuilder) Build(cc balancer.ClientConn, _ balancer.BuildOptions) balancer.Balancer {
	b := &pickfirstBalancer{
		cc:          cc,
		addressList: addressList{},
		subConns:    resolver.NewAddressMap(),
		state:       connectivity.Connecting,
		mu:          sync.Mutex{},
	}
	b.logger = internalgrpclog.NewPrefixLogger(logger, fmt.Sprintf(logPrefix, b))
	return b
}

func (b pickfirstBuilder) Name() string {
	return Name
}

func (pickfirstBuilder) ParseConfig(js json.RawMessage) (serviceconfig.LoadBalancingConfig, error) {
	var cfg pfConfig
	if err := json.Unmarshal(js, &cfg); err != nil {
		return nil, fmt.Errorf("pickfirst: unable to unmarshal LB policy config: %s, error: %v", string(js), err)
	}
	return cfg, nil
}

type pfConfig struct {
	serviceconfig.LoadBalancingConfig `json:"-"`

	// If set to true, instructs the LB policy to shuffle the order of the list
	// of endpoints received from the name resolver before attempting to
	// connect to them.
	ShuffleAddressList bool `json:"shuffleAddressList"`
}

// scData keeps track of the current state of the subConn.
// It is not safe for concurrent access.
type scData struct {
	// The following fields are initialized at build time and read-only after
	// that.
	subConn balancer.SubConn
	addr    resolver.Address

	state   connectivity.State
	lastErr error
}

func (b *pickfirstBalancer) newSCData(addr resolver.Address) (*scData, error) {
	sd := &scData{
		state: connectivity.Idle,
		addr:  addr,
	}
	sc, err := b.cc.NewSubConn([]resolver.Address{addr}, balancer.NewSubConnOptions{
		StateListener: func(state balancer.SubConnState) {
			b.updateSubConnState(sd, state)
		},
	})
	if err != nil {
		return nil, err
	}
	sd.subConn = sc
	return sd, nil
}

type pickfirstBalancer struct {
	// The following fields are initialized at build time and read-only after
	// that and therefore do not need to be guarded by a mutex.
	logger *internalgrpclog.PrefixLogger
	cc     balancer.ClientConn

	// The mutex is used to ensure synchronization of updates triggered
	// from the idle picker and the already serialized resolver,
	// SubConn state updates.
	mu    sync.Mutex
	state connectivity.State
	// scData for active subonns mapped by address.
	subConns    *resolver.AddressMap
	addressList addressList
	firstPass   bool
	numTF       int
}

// ResolverError is called by the ClientConn when the name resolver produces
// an error or when pickfirst determined the resolver update to be invalid.
func (b *pickfirstBalancer) ResolverError(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.resolverErrorLocked(err)
}

func (b *pickfirstBalancer) resolverErrorLocked(err error) {
	if b.logger.V(2) {
		b.logger.Infof("Received error from the name resolver: %v", err)
	}

	// The picker will not change since the balancer does not currently
	// report an error. If the balancer hasn't received a single good resolver
	// update yet, transition to TRANSIENT_FAILURE.
	if b.state != connectivity.TransientFailure && b.addressList.size() > 0 {
		if b.logger.V(2) {
			b.logger.Infof("Ignoring resolver error because balancer is using a previous good update.")
		}
		return
	}

	b.cc.UpdateState(balancer.State{
		ConnectivityState: connectivity.TransientFailure,
		Picker:            &picker{err: fmt.Errorf("name resolver error: %v", err)},
	})
}

func (b *pickfirstBalancer) UpdateClientConnState(state balancer.ClientConnState) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(state.ResolverState.Addresses) == 0 && len(state.ResolverState.Endpoints) == 0 {
		// Cleanup state pertaining to the previous resolver state.
		// Treat an empty address list like an error by calling b.ResolverError.
		b.state = connectivity.TransientFailure
		b.closeSubConnsLocked()
		b.addressList.updateAddrs(nil)
		b.resolverErrorLocked(errors.New("produced zero addresses"))
		return balancer.ErrBadResolverState
	}
	cfg, ok := state.BalancerConfig.(pfConfig)
	if state.BalancerConfig != nil && !ok {
		return fmt.Errorf("pickfirst: received illegal BalancerConfig (type %T): %v: %w", state.BalancerConfig, state.BalancerConfig, balancer.ErrBadResolverState)
	}

	if b.logger.V(2) {
		b.logger.Infof("Received new config %s, resolver state %s", pretty.ToJSON(cfg), pretty.ToJSON(state.ResolverState))
	}

	var newAddrs []resolver.Address
	if endpoints := state.ResolverState.Endpoints; len(endpoints) != 0 {
		// Perform the optional shuffling described in gRFC A62. The shuffling
		// will change the order of endpoints but not touch the order of the
		// addresses within each endpoint. - A61
		if cfg.ShuffleAddressList {
			endpoints = append([]resolver.Endpoint{}, endpoints...)
			internal.RandShuffle(len(endpoints), func(i, j int) { endpoints[i], endpoints[j] = endpoints[j], endpoints[i] })
		}

		// "Flatten the list by concatenating the ordered list of addresses for
		// each of the endpoints, in order." - A61
		for _, endpoint := range endpoints {
			// "In the flattened list, interleave addresses from the two address
			// families, as per RFC-8305 section 4." - A61
			// TODO: support the above language.
			newAddrs = append(newAddrs, endpoint.Addresses...)
		}
	} else {
		// Endpoints not set, process addresses until we migrate resolver
		// emissions fully to Endpoints. The top channel does wrap emitted
		// addresses with endpoints, however some balancers such as weighted
		// target do not forward the corresponding correct endpoints down/split
		// endpoints properly. Once all balancers correctly forward endpoints
		// down, can delete this else conditional.
		newAddrs = state.ResolverState.Addresses
		if cfg.ShuffleAddressList {
			newAddrs = append([]resolver.Address{}, newAddrs...)
			internal.RandShuffle(len(endpoints), func(i, j int) { endpoints[i], endpoints[j] = endpoints[j], endpoints[i] })
		}
	}

	// If an address appears in multiple endpoints or in the same endpoint
	// multiple times, we keep it only once. We will create only one SubConn
	// for the address because an AddressMap is used to store SubConns.
	// Not de-duplicating would result in attempting to connect to the same
	// SubConn multiple times in the same pass. We don't want this.
	newAddrs = deDupAddresses(newAddrs)

	// Since we have a new set of addresses, we are again at first pass.
	b.firstPass = true

	// If the previous ready SubConn exists in new address list,
	// keep this connection and don't create new SubConns.
	prevAddr := b.addressList.currentAddress()
	prevAddrsCount := b.addressList.size()
	b.addressList.updateAddrs(newAddrs)
	if b.state == connectivity.Ready && b.addressList.seekTo(prevAddr) {
		return nil
	}

	b.reconcileSubConnsLocked(newAddrs)
	// If it's the first resolver update or the balancer was already READY
	// (but the new address list does not contain the ready SubConn) or
	// CONNECTING, enter CONNECTING.
	// We may be in TRANSIENT_FAILURE due to a previous empty address list,
	// we should still enter CONNECTING because the sticky TF behaviour
	//  mentioned in A62 applies only when the TRANSIENT_FAILURE is reported
	// due to connectivity failures.
	if b.state == connectivity.Ready || b.state == connectivity.Connecting || prevAddrsCount == 0 {
		// Start connection attempt at first address.
		b.state = connectivity.Connecting
		b.cc.UpdateState(balancer.State{
			ConnectivityState: connectivity.Connecting,
			Picker:            &picker{err: balancer.ErrNoSubConnAvailable},
		})
		b.requestConnectionLocked()
	} else if b.state == connectivity.TransientFailure {
		// If we're in TRANSIENT_FAILURE, we stay in TRANSIENT_FAILURE until
		// we're READY. See A62.
		b.requestConnectionLocked()
	}
	return nil
}

// UpdateSubConnState is unused as a StateListener is always registered when
// creating SubConns.
func (b *pickfirstBalancer) UpdateSubConnState(subConn balancer.SubConn, state balancer.SubConnState) {
	b.logger.Errorf("UpdateSubConnState(%v, %+v) called unexpectedly", subConn, state)
}

func (b *pickfirstBalancer) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closeSubConnsLocked()
	b.state = connectivity.Shutdown
}

// ExitIdle moves the balancer out of idle state. It can be called concurrently
// by the idlePicker and clientConn so access to variables should be
// synchronized.
func (b *pickfirstBalancer) ExitIdle() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state == connectivity.Idle && b.addressList.currentAddress() == b.addressList.first() {
		b.firstPass = true
		b.requestConnectionLocked()
	}
}

func (b *pickfirstBalancer) closeSubConnsLocked() {
	for _, sd := range b.subConns.Values() {
		sd.(*scData).subConn.Shutdown()
	}
	b.subConns = resolver.NewAddressMap()
}

// deDupAddresses ensures that each address appears only once in the slice.
func deDupAddresses(addrs []resolver.Address) []resolver.Address {
	seenAddrs := resolver.NewAddressMap()
	retAddrs := []resolver.Address{}

	for _, addr := range addrs {
		if _, ok := seenAddrs.Get(addr); ok {
			continue
		}
		retAddrs = append(retAddrs, addr)
	}
	return retAddrs
}

// reconcileSubConnsLocked updates the active subchannels based on a new address
// list from the resolver. It does this by:
//   - closing subchannels: any existing subchannels associated with addresses
//     that are no longer in the updated list are shut down.
//   - removing subchannels: entries for these closed subchannels are removed
//     from the subchannel map.
//
// This ensures that the subchannel map accurately reflects the current set of
// addresses received from the name resolver.
func (b *pickfirstBalancer) reconcileSubConnsLocked(newAddrs []resolver.Address) {
	newAddrsMap := resolver.NewAddressMap()
	for _, addr := range newAddrs {
		newAddrsMap.Set(addr, true)
	}

	for _, oldAddr := range b.subConns.Keys() {
		if _, ok := newAddrsMap.Get(oldAddr); ok {
			continue
		}
		val, _ := b.subConns.Get(oldAddr)
		val.(*scData).subConn.Shutdown()
		b.subConns.Delete(oldAddr)
	}
}

// shutdownRemainingLocked shuts down remaining subConns. Called when a subConn
// becomes ready, which means that all other subConn must be shutdown.
func (b *pickfirstBalancer) shutdownRemainingLocked(selected *scData) {
	for _, v := range b.subConns.Values() {
		sd := v.(*scData)
		if sd.subConn != selected.subConn {
			sd.subConn.Shutdown()
		}
	}
	b.subConns = resolver.NewAddressMap()
	b.subConns.Set(selected.addr, selected)
}

// requestConnectionLocked starts connecting on the subchannel corresponding to
// the current address. If no subchannel exists, one is created. If the current
// subchannel is in TransientFailure, a connection to the next address is
// attempted until a subchannel is found.
func (b *pickfirstBalancer) requestConnectionLocked() {
	if !b.addressList.isValid() {
		return
	}
	var lastErr error
	for valid := true; valid; valid = b.addressList.increment() {
		curAddr := b.addressList.currentAddress()
		sd, ok := b.subConns.Get(curAddr)
		if !ok {
			var err error
			// We want to assign the new scData to sd from the outer scope,
			// hence we can't use := below.
			sd, err = b.newSCData(curAddr)
			if err != nil {
				// This should never happen, unless the clientConn is being shut
				// down.
				if b.logger.V(2) {
					b.logger.Infof("Failed to create a subConn for address %v: %v", curAddr.String(), err)
				}
				// Do nothing, the LB policy will be closed soon.
				return
			}
			b.subConns.Set(curAddr, sd)
		}

		scd := sd.(*scData)
		switch scd.state {
		case connectivity.Idle:
			scd.subConn.Connect()
		case connectivity.TransientFailure:
			// Try the next address.
			lastErr = scd.lastErr
			continue
		case connectivity.Ready:
			// Should never happen.
			b.logger.Errorf("Requesting a connection even though we have a READY SubConn")
		case connectivity.Shutdown:
			// Should never happen.
			b.logger.Errorf("SubConn with state SHUTDOWN present in SubConns map")
		case connectivity.Connecting:
			// Wait for the SubConn to report success or failure.
		}
		return
	}
	// All the remaining addresses in the list are in TRANSIENT_FAILURE, end the
	// first pass.
	b.endFirstPassLocked(lastErr)
}

func (b *pickfirstBalancer) updateSubConnState(sd *scData, newState balancer.SubConnState) {
	b.mu.Lock()
	defer b.mu.Unlock()
	oldState := sd.state
	sd.state = newState.ConnectivityState
	// Previously relevant SubConns can still callback with state updates.
	// To prevent pickers from returning these obsolete SubConns, this logic
	// is included to check if the current list of active SubConns includes this
	// SubConn.
	if activeSD, found := b.subConns.Get(sd.addr); !found || activeSD != sd {
		return
	}
	if newState.ConnectivityState == connectivity.Shutdown {
		return
	}

	if newState.ConnectivityState == connectivity.Ready {
		b.shutdownRemainingLocked(sd)
		if !b.addressList.seekTo(sd.addr) {
			// This should not fail as we should have only one SubConn after
			// entering READY. The SubConn should be present in the addressList.
			b.logger.Errorf("Address %q not found address list in  %v", sd.addr, b.addressList.addresses)
			return
		}
		b.state = connectivity.Ready
		b.cc.UpdateState(balancer.State{
			ConnectivityState: connectivity.Ready,
			Picker:            &picker{result: balancer.PickResult{SubConn: sd.subConn}},
		})
		return
	}

	// If the LB policy is READY, and it receives a subchannel state change,
	// it means that the READY subchannel has failed.
	// A SubConn can also transition from CONNECTING directly to IDLE when
	// a transport is successfully created, but the connection fails
	// before the SubConn can send the notification for READY. We treat
	// this as a successful connection and transition to IDLE.
	if (b.state == connectivity.Ready && newState.ConnectivityState != connectivity.Ready) || (oldState == connectivity.Connecting && newState.ConnectivityState == connectivity.Idle) {
		// Once a transport fails, the balancer enters IDLE and starts from
		// the first address when the picker is used.
		b.shutdownRemainingLocked(sd)
		b.state = connectivity.Idle
		b.addressList.reset()
		b.cc.UpdateState(balancer.State{
			ConnectivityState: connectivity.Idle,
			Picker:            &idlePicker{exitIdle: sync.OnceFunc(b.ExitIdle)},
		})
		return
	}

	if b.firstPass {
		switch newState.ConnectivityState {
		case connectivity.Connecting:
			// The balancer can be in either IDLE, CONNECTING or
			// TRANSIENT_FAILURE. If it's in TRANSIENT_FAILURE, stay in
			// TRANSIENT_FAILURE until it's READY. See A62.
			// If the balancer is already in CONNECTING, no update is needed.
			if b.state == connectivity.Idle {
				b.state = connectivity.Connecting
				b.cc.UpdateState(balancer.State{
					ConnectivityState: connectivity.Connecting,
					Picker:            &picker{err: balancer.ErrNoSubConnAvailable},
				})
			}
		case connectivity.TransientFailure:
			sd.lastErr = newState.ConnectionError
			// Since we're re-using common SubConns while handling resolver
			// updates, we could receive an out of turn TRANSIENT_FAILURE from
			// a pass over the previous address list. We ignore such updates.

			if curAddr := b.addressList.currentAddress(); !equalAddressIgnoringBalAttributes(&curAddr, &sd.addr) {
				return
			}
			if b.addressList.increment() {
				b.requestConnectionLocked()
				return
			}
			// End of the first pass.
			b.endFirstPassLocked(newState.ConnectionError)
		}
		return
	}

	// We have finished the first pass, keep re-connecting failing SubConns.
	switch newState.ConnectivityState {
	case connectivity.TransientFailure:
		b.numTF = (b.numTF + 1) % b.subConns.Len()
		sd.lastErr = newState.ConnectionError
		if b.numTF%b.subConns.Len() == 0 {
			b.cc.UpdateState(balancer.State{
				ConnectivityState: connectivity.TransientFailure,
				Picker:            &picker{err: newState.ConnectionError},
			})
		}
		// We don't need to request re-resolution since the SubConn already
		// does that before reporting TRANSIENT_FAILURE.
		// TODO: #7534 - Move re-resolution requests from SubConn into
		// pick_first.
	case connectivity.Idle:
		sd.subConn.Connect()
	}
}

func (b *pickfirstBalancer) endFirstPassLocked(lastErr error) {
	b.firstPass = false
	b.numTF = 0
	b.state = connectivity.TransientFailure

	b.cc.UpdateState(balancer.State{
		ConnectivityState: connectivity.TransientFailure,
		Picker:            &picker{err: lastErr},
	})
	// Start re-connecting all the SubConns that are already in IDLE.
	for _, v := range b.subConns.Values() {
		sd := v.(*scData)
		if sd.state == connectivity.Idle {
			sd.subConn.Connect()
		}
	}
}

type picker struct {
	result balancer.PickResult
	err    error
}

func (p *picker) Pick(balancer.PickInfo) (balancer.PickResult, error) {
	return p.result, p.err
}

// idlePicker is used when the SubConn is IDLE and kicks the SubConn into
// CONNECTING when Pick is called.
type idlePicker struct {
	exitIdle func()
}

func (i *idlePicker) Pick(balancer.PickInfo) (balancer.PickResult, error) {
	i.exitIdle()
	return balancer.PickResult{}, balancer.ErrNoSubConnAvailable
}

// addressList manages sequentially iterating over addresses present in a list
// of endpoints. It provides a 1 dimensional view of the addresses present in
// the endpoints.
// This type is not safe for concurrent access.
type addressList struct {
	addresses []resolver.Address
	idx       int
}

func (al *addressList) isValid() bool {
	return al.idx < len(al.addresses)
}

func (al *addressList) size() int {
	return len(al.addresses)
}

// increment moves to the next index in the address list.
// This method returns false if it went off the list, true otherwise.
func (al *addressList) increment() bool {
	if !al.isValid() {
		return false
	}
	al.idx++
	return al.idx < len(al.addresses)
}

// currentAddress returns the current address pointed to in the addressList.
// If the list is in an invalid state, it returns an empty address instead.
func (al *addressList) currentAddress() resolver.Address {
	if !al.isValid() {
		return resolver.Address{}
	}
	return al.addresses[al.idx]
}

// first returns the first address in the list. If the list is empty, it returns
// an empty address instead.
func (al *addressList) first() resolver.Address {
	if len(al.addresses) == 0 {
		return resolver.Address{}
	}
	return al.addresses[0]
}

func (al *addressList) reset() {
	al.idx = 0
}

func (al *addressList) updateAddrs(addrs []resolver.Address) {
	al.addresses = addrs
	al.reset()
}

// seekTo returns false if the needle was not found and the current index was
// left unchanged.
func (al *addressList) seekTo(needle resolver.Address) bool {
	for ai, addr := range al.addresses {
		if !equalAddressIgnoringBalAttributes(&addr, &needle) {
			continue
		}
		al.idx = ai
		return true
	}
	return false
}

// equalAddressIgnoringBalAttributes returns true is a and b are considered
// equal. This is different from the Equal method on the resolver.Address type
// which considers all fields to determine equality. Here, we only consider
// fields that are meaningful to the SubConn.
func equalAddressIgnoringBalAttributes(a, b *resolver.Address) bool {
	return a.Addr == b.Addr && a.ServerName == b.ServerName &&
		a.Attributes.Equal(b.Attributes) &&
		a.Metadata == b.Metadata
}
