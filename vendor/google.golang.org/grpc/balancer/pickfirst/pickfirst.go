/*
 *
 * Copyright 2017 gRPC authors.
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

// Package pickfirst contains the pick_first load balancing policy.
package pickfirst

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"

	"google.golang.org/grpc/balancer"
	"google.golang.org/grpc/balancer/pickfirst/internal"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/grpclog"
	"google.golang.org/grpc/internal/envconfig"
	internalgrpclog "google.golang.org/grpc/internal/grpclog"
	"google.golang.org/grpc/internal/pretty"
	"google.golang.org/grpc/resolver"
	"google.golang.org/grpc/serviceconfig"

	_ "google.golang.org/grpc/balancer/pickfirst/pickfirstleaf" // For automatically registering the new pickfirst if required.
)

func init() {
	if envconfig.NewPickFirstEnabled {
		return
	}
	balancer.Register(pickfirstBuilder{})
}

var logger = grpclog.Component("pick-first-lb")

const (
	// Name is the name of the pick_first balancer.
	Name      = "pick_first"
	logPrefix = "[pick-first-lb %p] "
)

type pickfirstBuilder struct{}

func (pickfirstBuilder) Build(cc balancer.ClientConn, _ balancer.BuildOptions) balancer.Balancer {
	b := &pickfirstBalancer{cc: cc}
	b.logger = internalgrpclog.NewPrefixLogger(logger, fmt.Sprintf(logPrefix, b))
	return b
}

func (pickfirstBuilder) Name() string {
	return Name
}

type pfConfig struct {
	serviceconfig.LoadBalancingConfig `json:"-"`

	// If set to true, instructs the LB policy to shuffle the order of the list
	// of endpoints received from the name resolver before attempting to
	// connect to them.
	ShuffleAddressList bool `json:"shuffleAddressList"`
}

func (pickfirstBuilder) ParseConfig(js json.RawMessage) (serviceconfig.LoadBalancingConfig, error) {
	var cfg pfConfig
	if err := json.Unmarshal(js, &cfg); err != nil {
		return nil, fmt.Errorf("pickfirst: unable to unmarshal LB policy config: %s, error: %v", string(js), err)
	}
	return cfg, nil
}

type pickfirstBalancer struct {
	logger  *internalgrpclog.PrefixLogger
	state   connectivity.State
	cc      balancer.ClientConn
	subConn balancer.SubConn
}

func (b *pickfirstBalancer) ResolverError(err error) {
	if b.logger.V(2) {
		b.logger.Infof("Received error from the name resolver: %v", err)
	}
	if b.subConn == nil {
		b.state = connectivity.TransientFailure
	}

	if b.state != connectivity.TransientFailure {
		// The picker will not change since the balancer does not currently
		// report an error.
		return
	}
	b.cc.UpdateState(balancer.State{
		ConnectivityState: connectivity.TransientFailure,
		Picker:            &picker{err: fmt.Errorf("name resolver error: %v", err)},
	})
}

// Shuffler is an interface for shuffling an address list.
type Shuffler interface {
	ShuffleAddressListForTesting(n int, swap func(i, j int))
}

// ShuffleAddressListForTesting pseudo-randomizes the order of addresses.  n
// is the number of elements.  swap swaps the elements with indexes i and j.
func ShuffleAddressListForTesting(n int, swap func(i, j int)) { rand.Shuffle(n, swap) }

func (b *pickfirstBalancer) UpdateClientConnState(state balancer.ClientConnState) error {
	if len(state.ResolverState.Addresses) == 0 && len(state.ResolverState.Endpoints) == 0 {
		// The resolver reported an empty address list. Treat it like an error by
		// calling b.ResolverError.
		if b.subConn != nil {
			// Shut down the old subConn. All addresses were removed, so it is
			// no longer valid.
			b.subConn.Shutdown()
			b.subConn = nil
		}
		b.ResolverError(errors.New("produced zero addresses"))
		return balancer.ErrBadResolverState
	}
	// We don't have to guard this block with the env var because ParseConfig
	// already does so.
	cfg, ok := state.BalancerConfig.(pfConfig)
	if state.BalancerConfig != nil && !ok {
		return fmt.Errorf("pickfirst: received illegal BalancerConfig (type %T): %v", state.BalancerConfig, state.BalancerConfig)
	}

	if b.logger.V(2) {
		b.logger.Infof("Received new config %s, resolver state %s", pretty.ToJSON(cfg), pretty.ToJSON(state.ResolverState))
	}

	var addrs []resolver.Address
	if endpoints := state.ResolverState.Endpoints; len(endpoints) != 0 {
		// Perform the optional shuffling described in gRFC A62. The shuffling will
		// change the order of endpoints but not touch the order of the addresses
		// within each endpoint. - A61
		if cfg.ShuffleAddressList {
			endpoints = append([]resolver.Endpoint{}, endpoints...)
			internal.RandShuffle(len(endpoints), func(i, j int) { endpoints[i], endpoints[j] = endpoints[j], endpoints[i] })
		}

		// "Flatten the list by concatenating the ordered list of addresses for each
		// of the endpoints, in order." - A61
		for _, endpoint := range endpoints {
			// "In the flattened list, interleave addresses from the two address
			// families, as per RFC-8304 section 4." - A61
			// TODO: support the above language.
			addrs = append(addrs, endpoint.Addresses...)
		}
	} else {
		// Endpoints not set, process addresses until we migrate resolver
		// emissions fully to Endpoints. The top channel does wrap emitted
		// addresses with endpoints, however some balancers such as weighted
		// target do not forward the corresponding correct endpoints down/split
		// endpoints properly. Once all balancers correctly forward endpoints
		// down, can delete this else conditional.
		addrs = state.ResolverState.Addresses
		if cfg.ShuffleAddressList {
			addrs = append([]resolver.Address{}, addrs...)
			rand.Shuffle(len(addrs), func(i, j int) { addrs[i], addrs[j] = addrs[j], addrs[i] })
		}
	}

	if b.subConn != nil {
		b.cc.UpdateAddresses(b.subConn, addrs)
		return nil
	}

	var subConn balancer.SubConn
	subConn, err := b.cc.NewSubConn(addrs, balancer.NewSubConnOptions{
		StateListener: func(state balancer.SubConnState) {
			b.updateSubConnState(subConn, state)
		},
	})
	if err != nil {
		if b.logger.V(2) {
			b.logger.Infof("Failed to create new SubConn: %v", err)
		}
		b.state = connectivity.TransientFailure
		b.cc.UpdateState(balancer.State{
			ConnectivityState: connectivity.TransientFailure,
			Picker:            &picker{err: fmt.Errorf("error creating connection: %v", err)},
		})
		return balancer.ErrBadResolverState
	}
	b.subConn = subConn
	b.state = connectivity.Idle
	b.cc.UpdateState(balancer.State{
		ConnectivityState: connectivity.Connecting,
		Picker:            &picker{err: balancer.ErrNoSubConnAvailable},
	})
	b.subConn.Connect()
	return nil
}

// UpdateSubConnState is unused as a StateListener is always registered when
// creating SubConns.
func (b *pickfirstBalancer) UpdateSubConnState(subConn balancer.SubConn, state balancer.SubConnState) {
	b.logger.Errorf("UpdateSubConnState(%v, %+v) called unexpectedly", subConn, state)
}

func (b *pickfirstBalancer) updateSubConnState(subConn balancer.SubConn, state balancer.SubConnState) {
	if b.logger.V(2) {
		b.logger.Infof("Received SubConn state update: %p, %+v", subConn, state)
	}
	if b.subConn != subConn {
		if b.logger.V(2) {
			b.logger.Infof("Ignored state change because subConn is not recognized")
		}
		return
	}
	if state.ConnectivityState == connectivity.Shutdown {
		b.subConn = nil
		return
	}

	switch state.ConnectivityState {
	case connectivity.Ready:
		b.cc.UpdateState(balancer.State{
			ConnectivityState: state.ConnectivityState,
			Picker:            &picker{result: balancer.PickResult{SubConn: subConn}},
		})
	case connectivity.Connecting:
		if b.state == connectivity.TransientFailure {
			// We stay in TransientFailure until we are Ready. See A62.
			return
		}
		b.cc.UpdateState(balancer.State{
			ConnectivityState: state.ConnectivityState,
			Picker:            &picker{err: balancer.ErrNoSubConnAvailable},
		})
	case connectivity.Idle:
		if b.state == connectivity.TransientFailure {
			// We stay in TransientFailure until we are Ready. Also kick the
			// subConn out of Idle into Connecting. See A62.
			b.subConn.Connect()
			return
		}
		b.cc.UpdateState(balancer.State{
			ConnectivityState: state.ConnectivityState,
			Picker:            &idlePicker{subConn: subConn},
		})
	case connectivity.TransientFailure:
		b.cc.UpdateState(balancer.State{
			ConnectivityState: state.ConnectivityState,
			Picker:            &picker{err: state.ConnectionError},
		})
	}
	b.state = state.ConnectivityState
}

func (b *pickfirstBalancer) Close() {
}

func (b *pickfirstBalancer) ExitIdle() {
	if b.subConn != nil && b.state == connectivity.Idle {
		b.subConn.Connect()
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
	subConn balancer.SubConn
}

func (i *idlePicker) Pick(balancer.PickInfo) (balancer.PickResult, error) {
	i.subConn.Connect()
	return balancer.PickResult{}, balancer.ErrNoSubConnAvailable
}
