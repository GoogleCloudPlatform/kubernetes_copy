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

package grpc

import (
	"context"
	"strings"
	"sync"

	"google.golang.org/grpc/internal/channelz"
	"google.golang.org/grpc/internal/grpcsync"
	"google.golang.org/grpc/internal/pretty"
	"google.golang.org/grpc/resolver"
	"google.golang.org/grpc/serviceconfig"
)

// ccResolverWrapper is a wrapper on top of cc for resolvers.
// It implements resolver.ClientConn interface.
type ccResolverWrapper struct {
	// The following fields are initialized when the wrapper is created and are
	// read-only afterwards, and therefore can be accessed without a mutex.
	cc                  *ClientConn
	ignoreServiceConfig bool
	serializer          *grpcsync.CallbackSerializer
	serializerCancel    context.CancelFunc

	resolver resolver.Resolver // only accessed within the serializer

	// The following fields are protected by mu.  Caller must take cc.mu before
	// taking mu.
	mu       sync.Mutex
	curState resolver.State
	closed   bool
}

// newCCResolverWrapper initializes the ccResolverWrapper.  It can only be used
// after calling start, which builds the resolver.
func newCCResolverWrapper(cc *ClientConn) *ccResolverWrapper {
	ctx, cancel := context.WithCancel(cc.ctx)
	return &ccResolverWrapper{
		cc:                  cc,
		ignoreServiceConfig: cc.dopts.disableServiceConfig,
		serializer:          grpcsync.NewCallbackSerializer(ctx),
		serializerCancel:    cancel,
	}
}

// start builds the name resolver using the resolver.Builder in cc and returns
// any error encountered.  It must always be the first operation performed on
// any newly created ccResolverWrapper, except that close may be called instead.
func (ccr *ccResolverWrapper) start() error {
	errCh := make(chan error)
	ccr.serializer.TrySchedule(func(ctx context.Context) {
		if ctx.Err() != nil {
			return
		}
		opts := resolver.BuildOptions{
			DisableServiceConfig: ccr.cc.dopts.disableServiceConfig,
			DialCreds:            ccr.cc.dopts.copts.TransportCredentials,
			CredsBundle:          ccr.cc.dopts.copts.CredsBundle,
			Dialer:               ccr.cc.dopts.copts.Dialer,
			Authority:            ccr.cc.authority,
		}
		var err error
		ccr.resolver, err = ccr.cc.resolverBuilder.Build(ccr.cc.parsedTarget, ccr, opts)
		errCh <- err
	})
	return <-errCh
}

func (ccr *ccResolverWrapper) resolveNow(o resolver.ResolveNowOptions) {
	ccr.serializer.TrySchedule(func(ctx context.Context) {
		if ctx.Err() != nil || ccr.resolver == nil {
			return
		}
		ccr.resolver.ResolveNow(o)
	})
}

// close initiates async shutdown of the wrapper.  To determine the wrapper has
// finished shutting down, the channel should block on ccr.serializer.Done()
// without cc.mu held.
func (ccr *ccResolverWrapper) close() {
	channelz.Info(logger, ccr.cc.channelz, "Closing the name resolver")
	ccr.mu.Lock()
	ccr.closed = true
	ccr.mu.Unlock()

	ccr.serializer.TrySchedule(func(context.Context) {
		if ccr.resolver == nil {
			return
		}
		ccr.resolver.Close()
		ccr.resolver = nil
	})
	ccr.serializerCancel()
}

// UpdateState is called by resolver implementations to report new state to gRPC
// which includes addresses and service config.
func (ccr *ccResolverWrapper) UpdateState(s resolver.State) error {
	ccr.cc.mu.Lock()
	ccr.mu.Lock()
	if ccr.closed {
		ccr.mu.Unlock()
		ccr.cc.mu.Unlock()
		return nil
	}
	if s.Endpoints == nil {
		s.Endpoints = make([]resolver.Endpoint, 0, len(s.Addresses))
		for _, a := range s.Addresses {
			ep := resolver.Endpoint{Addresses: []resolver.Address{a}, Attributes: a.BalancerAttributes}
			ep.Addresses[0].BalancerAttributes = nil
			s.Endpoints = append(s.Endpoints, ep)
		}
	}
	ccr.addChannelzTraceEvent(s)
	ccr.curState = s
	ccr.mu.Unlock()
	return ccr.cc.updateResolverStateAndUnlock(s, nil)
}

// ReportError is called by resolver implementations to report errors
// encountered during name resolution to gRPC.
func (ccr *ccResolverWrapper) ReportError(err error) {
	ccr.cc.mu.Lock()
	ccr.mu.Lock()
	if ccr.closed {
		ccr.mu.Unlock()
		ccr.cc.mu.Unlock()
		return
	}
	ccr.mu.Unlock()
	channelz.Warningf(logger, ccr.cc.channelz, "ccResolverWrapper: reporting error to cc: %v", err)
	ccr.cc.updateResolverStateAndUnlock(resolver.State{}, err)
}

// NewAddress is called by the resolver implementation to send addresses to
// gRPC.
func (ccr *ccResolverWrapper) NewAddress(addrs []resolver.Address) {
	ccr.cc.mu.Lock()
	ccr.mu.Lock()
	if ccr.closed {
		ccr.mu.Unlock()
		ccr.cc.mu.Unlock()
		return
	}
	s := resolver.State{Addresses: addrs, ServiceConfig: ccr.curState.ServiceConfig}
	ccr.addChannelzTraceEvent(s)
	ccr.curState = s
	ccr.mu.Unlock()
	ccr.cc.updateResolverStateAndUnlock(s, nil)
}

// ParseServiceConfig is called by resolver implementations to parse a JSON
// representation of the service config.
func (ccr *ccResolverWrapper) ParseServiceConfig(scJSON string) *serviceconfig.ParseResult {
	return parseServiceConfig(scJSON, ccr.cc.dopts.maxCallAttempts)
}

// addChannelzTraceEvent adds a channelz trace event containing the new
// state received from resolver implementations.
func (ccr *ccResolverWrapper) addChannelzTraceEvent(s resolver.State) {
	if !logger.V(0) && !channelz.IsOn() {
		return
	}
	var updates []string
	var oldSC, newSC *ServiceConfig
	var oldOK, newOK bool
	if ccr.curState.ServiceConfig != nil {
		oldSC, oldOK = ccr.curState.ServiceConfig.Config.(*ServiceConfig)
	}
	if s.ServiceConfig != nil {
		newSC, newOK = s.ServiceConfig.Config.(*ServiceConfig)
	}
	if oldOK != newOK || (oldOK && newOK && oldSC.rawJSONString != newSC.rawJSONString) {
		updates = append(updates, "service config updated")
	}
	if len(ccr.curState.Addresses) > 0 && len(s.Addresses) == 0 {
		updates = append(updates, "resolver returned an empty address list")
	} else if len(ccr.curState.Addresses) == 0 && len(s.Addresses) > 0 {
		updates = append(updates, "resolver returned new addresses")
	}
	channelz.Infof(logger, ccr.cc.channelz, "Resolver state updated: %s (%v)", pretty.ToJSON(s), strings.Join(updates, "; "))
}
