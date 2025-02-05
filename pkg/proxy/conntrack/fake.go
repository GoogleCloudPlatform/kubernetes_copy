//go:build linux
// +build linux

/*
Copyright 2016 The Kubernetes Authors.

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

package conntrack

import (
	"github.com/vishvananda/netlink"
)

type fakeHandler struct {
	tableType netlink.ConntrackTableType
	ipFamily  netlink.InetFamily
	filters   []*conntrackFilter

	entries         [][]*netlink.ConntrackFlow
	tableListErrors []error
	deleteErrors    []error

	// callsConntrackTableList is a counter that gets incremented each time ConntrackTableList is called
	callsConntrackTableList int
	// callsConntrackDeleteFilters is a counter that gets incremented each time ConntrackDeleteFilters is called
	callsConntrackDeleteFilters int
}

// ConntrackTableList is part of netlinkHandler interface.
func (fake *fakeHandler) ConntrackTableList(_ netlink.ConntrackTableType, _ netlink.InetFamily) ([]*netlink.ConntrackFlow, error) {
	calls := fake.callsConntrackTableList
	fake.callsConntrackTableList++

	if len(fake.entries) <= calls {
		return nil, nil
	}
	return fake.entries[calls], fake.tableListErrors[calls]
}

// ConntrackDeleteFilters is part of netlinkHandler interface.
func (fake *fakeHandler) ConntrackDeleteFilters(tableType netlink.ConntrackTableType, family netlink.InetFamily, netlinkFilters ...netlink.CustomConntrackFilter) (uint, error) {
	fake.tableType = tableType
	fake.ipFamily = family
	deleteCalls := fake.callsConntrackDeleteFilters
	otherCalls := fake.callsConntrackTableList
	fake.callsConntrackDeleteFilters++
	fake.filters = make([]*conntrackFilter, 0, len(netlinkFilters))
	for _, netlinkFilter := range netlinkFilters {
		fake.filters = append(fake.filters, netlinkFilter.(*conntrackFilter))
	}

	var flows []*netlink.ConntrackFlow
	before := len(fake.entries)
	if before > 0 {
		for _, flow := range fake.entries[otherCalls] {
			var matched bool
			for _, filter := range fake.filters {
				matched = filter.MatchConntrackFlow(flow)
				if matched {
					break
				}
			}
			if !matched {
				flows = append(flows, flow)
			}
		}
		fake.entries[otherCalls] = flows
	}
	var err error
	if deleteCalls < len(fake.deleteErrors) {
		err = fake.deleteErrors[deleteCalls]
	} else {
		err = nil
	}

	return uint(before - len(fake.entries)), err
}

var _ netlinkHandler = (*fakeHandler)(nil)

// NewFake creates a new FakeInterface
func NewFake() Interface {
	return &conntracker{handler: &fakeHandler{}}
}
