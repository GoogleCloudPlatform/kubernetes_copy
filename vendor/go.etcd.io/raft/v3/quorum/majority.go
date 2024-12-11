// Copyright 2019 The etcd Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package quorum

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

// MajorityConfig is a set of IDs that uses majority quorums to make decisions.
type MajorityConfig map[uint64]struct{}

func (c MajorityConfig) String() string {
	sl := make([]uint64, 0, len(c))
	for id := range c {
		sl = append(sl, id)
	}
	sort.Slice(sl, func(i, j int) bool { return sl[i] < sl[j] })
	var buf strings.Builder
	buf.WriteByte('(')
	for i := range sl {
		if i > 0 {
			buf.WriteByte(' ')
		}
		fmt.Fprint(&buf, sl[i])
	}
	buf.WriteByte(')')
	return buf.String()
}

// Describe returns a (multi-line) representation of the commit indexes for the
// given lookuper.
func (c MajorityConfig) Describe(l AckedIndexer) string {
	if len(c) == 0 {
		return "<empty majority quorum>"
	}
	type tup struct {
		id  uint64
		idx Index
		ok  bool // idx found?
		bar int  // length of bar displayed for this tup
	}

	// Below, populate .bar so that the i-th largest commit index has bar i (we
	// plot this as sort of a progress bar). The actual code is a bit more
	// complicated and also makes sure that equal index => equal bar.

	n := len(c)
	info := make([]tup, 0, n)
	for id := range c {
		idx, ok := l.AckedIndex(id)
		info = append(info, tup{id: id, idx: idx, ok: ok})
	}

	// Sort by index
	sort.Slice(info, func(i, j int) bool {
		if info[i].idx == info[j].idx {
			return info[i].id < info[j].id
		}
		return info[i].idx < info[j].idx
	})

	// Populate .bar.
	for i := range info {
		if i > 0 && info[i-1].idx < info[i].idx {
			info[i].bar = i
		}
	}

	// Sort by ID.
	sort.Slice(info, func(i, j int) bool {
		return info[i].id < info[j].id
	})

	var buf strings.Builder

	// Print.
	fmt.Fprint(&buf, strings.Repeat(" ", n)+"    idx\n")
	for i := range info {
		bar := info[i].bar
		if !info[i].ok {
			fmt.Fprint(&buf, "?"+strings.Repeat(" ", n))
		} else {
			fmt.Fprint(&buf, strings.Repeat("x", bar)+">"+strings.Repeat(" ", n-bar))
		}
		fmt.Fprintf(&buf, " %5d    (id=%d)\n", info[i].idx, info[i].id)
	}
	return buf.String()
}

// Slice returns the MajorityConfig as a sorted slice.
func (c MajorityConfig) Slice() []uint64 {
	var sl []uint64
	for id := range c {
		sl = append(sl, id)
	}
	sort.Slice(sl, func(i, j int) bool { return sl[i] < sl[j] })
	return sl
}

func insertionSort(sl []uint64) {
	a, b := 0, len(sl)
	for i := a + 1; i < b; i++ {
		for j := i; j > a && sl[j] < sl[j-1]; j-- {
			sl[j], sl[j-1] = sl[j-1], sl[j]
		}
	}
}

// CommittedIndex computes the committed index from those supplied via the
// provided AckedIndexer (for the active config).
func (c MajorityConfig) CommittedIndex(l AckedIndexer) Index {
	n := len(c)
	if n == 0 {
		// This plays well with joint quorums which, when one half is the zero
		// MajorityConfig, should behave like the other half.
		return math.MaxUint64
	}

	// Use an on-stack slice to collect the committed indexes when n <= 7
	// (otherwise we alloc). The alternative is to stash a slice on
	// MajorityConfig, but this impairs usability (as is, MajorityConfig is just
	// a map, and that's nice). The assumption is that running with a
	// replication factor of >7 is rare, and in cases in which it happens
	// performance is a lesser concern (additionally the performance
	// implications of an allocation here are far from drastic).
	var stk [7]uint64
	var srt []uint64
	if len(stk) >= n {
		srt = stk[:n]
	} else {
		srt = make([]uint64, n)
	}

	{
		// Fill the slice with the indexes observed. Any unused slots will be
		// left as zero; these correspond to voters that may report in, but
		// haven't yet. We fill from the right (since the zeroes will end up on
		// the left after sorting below anyway).
		i := n - 1
		for id := range c {
			if idx, ok := l.AckedIndex(id); ok {
				srt[i] = uint64(idx)
				i--
			}
		}
	}

	// Sort by index. Use a bespoke algorithm (copied from the stdlib's sort
	// package) to keep srt on the stack.
	insertionSort(srt)

	// The smallest index into the array for which the value is acked by a
	// quorum. In other words, from the end of the slice, move n/2+1 to the
	// left (accounting for zero-indexing).
	pos := n - (n/2 + 1)
	return Index(srt[pos])
}

// VoteResult takes a mapping of voters to yes/no (true/false) votes and returns
// a result indicating whether the vote is pending (i.e. neither a quorum of
// yes/no has been reached), won (a quorum of yes has been reached), or lost (a
// quorum of no has been reached).
func (c MajorityConfig) VoteResult(votes map[uint64]bool) VoteResult {
	if len(c) == 0 {
		// By convention, the elections on an empty config win. This comes in
		// handy with joint quorums because it'll make a half-populated joint
		// quorum behave like a majority quorum.
		return VoteWon
	}

	var votedCnt int //vote counts for yes.
	var missing int
	for id := range c {
		v, ok := votes[id]
		if !ok {
			missing++
			continue
		}
		if v {
			votedCnt++
		}
	}

	q := len(c)/2 + 1
	if votedCnt >= q {
		return VoteWon
	}
	if votedCnt+missing >= q {
		return VotePending
	}
	return VoteLost
}
