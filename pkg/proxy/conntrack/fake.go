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

	entries [][]*netlink.ConntrackFlow
	errors  []error
	// calls is a counter that gets incremented each time ConntrackTableList is called
	calls int
}

// ConntrackTableList is part of netlinkHandler interface.
func (fake *fakeHandler) ConntrackTableList(_ netlink.ConntrackTableType, _ netlink.InetFamily) ([]*netlink.ConntrackFlow, error) {
	calls := fake.calls
	fake.calls++
	return fake.entries[calls], fake.errors[calls]
}

// ConntrackDeleteFilters is part of netlinkHandler interface.
func (fake *fakeHandler) ConntrackDeleteFilters(tableType netlink.ConntrackTableType, family netlink.InetFamily, netlinkFilters ...netlink.CustomConntrackFilter) (uint, error) {
	fake.tableType = tableType
	fake.ipFamily = family
	fake.filters = make([]*conntrackFilter, 0, len(netlinkFilters))
	for _, netlinkFilter := range netlinkFilters {
		fake.filters = append(fake.filters, netlinkFilter.(*conntrackFilter))
	}

	var flows []*netlink.ConntrackFlow
	before := len(fake.entries)
	for _, flow := range fake.entries[fake.calls] {
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
	fake.entries[fake.calls] = flows
	return uint(before - len(fake.entries)), nil
}

var _ netlinkHandler = (*fakeHandler)(nil)

// NewFake creates a new FakeInterface
func NewFake() Interface {
	return &conntracker{handler: &fakeHandler{}}
}
