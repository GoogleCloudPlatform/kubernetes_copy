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

package tracker

import (
	"fmt"
	"sort"
	"strings"

	"go.etcd.io/raft/v3/quorum"
	pb "go.etcd.io/raft/v3/raftpb"
)

// Config reflects the configuration tracked in a ProgressTracker.
type Config struct {
	Voters quorum.JointConfig
	// AutoLeave is true if the configuration is joint and a transition to the
	// incoming configuration should be carried out automatically by Raft when
	// this is possible. If false, the configuration will be joint until the
	// application initiates the transition manually.
	AutoLeave bool
	// Learners is a set of IDs corresponding to the learners active in the
	// current configuration.
	//
	// Invariant: Learners and Voters does not intersect, i.e. if a peer is in
	// either half of the joint config, it can't be a learner; if it is a
	// learner it can't be in either half of the joint config. This invariant
	// simplifies the implementation since it allows peers to have clarity about
	// its current role without taking into account joint consensus.
	Learners map[uint64]struct{}
	// When we turn a voter into a learner during a joint consensus transition,
	// we cannot add the learner directly when entering the joint state. This is
	// because this would violate the invariant that the intersection of
	// voters and learners is empty. For example, assume a Voter is removed and
	// immediately re-added as a learner (or in other words, it is demoted):
	//
	// Initially, the configuration will be
	//
	//   voters:   {1 2 3}
	//   learners: {}
	//
	// and we want to demote 3. Entering the joint configuration, we naively get
	//
	//   voters:   {1 2} & {1 2 3}
	//   learners: {3}
	//
	// but this violates the invariant (3 is both voter and learner). Instead,
	// we get
	//
	//   voters:   {1 2} & {1 2 3}
	//   learners: {}
	//   next_learners: {3}
	//
	// Where 3 is now still purely a voter, but we are remembering the intention
	// to make it a learner upon transitioning into the final configuration:
	//
	//   voters:   {1 2}
	//   learners: {3}
	//   next_learners: {}
	//
	// Note that next_learners is not used while adding a learner that is not
	// also a voter in the joint config. In this case, the learner is added
	// right away when entering the joint configuration, so that it is caught up
	// as soon as possible.
	LearnersNext map[uint64]struct{}
}

func (c Config) String() string {
	var buf strings.Builder
	fmt.Fprintf(&buf, "voters=%s", c.Voters)
	if c.Learners != nil {
		fmt.Fprintf(&buf, " learners=%s", quorum.MajorityConfig(c.Learners).String())
	}
	if c.LearnersNext != nil {
		fmt.Fprintf(&buf, " learners_next=%s", quorum.MajorityConfig(c.LearnersNext).String())
	}
	if c.AutoLeave {
		fmt.Fprint(&buf, " autoleave")
	}
	return buf.String()
}

// Clone returns a copy of the Config that shares no memory with the original.
func (c *Config) Clone() Config {
	clone := func(m map[uint64]struct{}) map[uint64]struct{} {
		if m == nil {
			return nil
		}
		mm := make(map[uint64]struct{}, len(m))
		for k := range m {
			mm[k] = struct{}{}
		}
		return mm
	}
	return Config{
		Voters:       quorum.JointConfig{clone(c.Voters[0]), clone(c.Voters[1])},
		Learners:     clone(c.Learners),
		LearnersNext: clone(c.LearnersNext),
	}
}

// ProgressTracker tracks the currently active configuration and the information
// known about the nodes and learners in it. In particular, it tracks the match
// index for each peer which in turn allows reasoning about the committed index.
type ProgressTracker struct {
	Config

	Progress ProgressMap

	Votes map[uint64]bool

	MaxInflight      int
	MaxInflightBytes uint64
}

// MakeProgressTracker initializes a ProgressTracker.
func MakeProgressTracker(maxInflight int, maxBytes uint64) ProgressTracker {
	p := ProgressTracker{
		MaxInflight:      maxInflight,
		MaxInflightBytes: maxBytes,
		Config: Config{
			Voters: quorum.JointConfig{
				quorum.MajorityConfig{},
				nil, // only populated when used
			},
			Learners:     nil, // only populated when used
			LearnersNext: nil, // only populated when used
		},
		Votes:    map[uint64]bool{},
		Progress: map[uint64]*Progress{},
	}
	return p
}

// ConfState returns a ConfState representing the active configuration.
func (p *ProgressTracker) ConfState() pb.ConfState {
	return pb.ConfState{
		Voters:         p.Voters[0].Slice(),
		VotersOutgoing: p.Voters[1].Slice(),
		Learners:       quorum.MajorityConfig(p.Learners).Slice(),
		LearnersNext:   quorum.MajorityConfig(p.LearnersNext).Slice(),
		AutoLeave:      p.AutoLeave,
	}
}

// IsSingleton returns true if (and only if) there is only one voting member
// (i.e. the leader) in the current configuration.
func (p *ProgressTracker) IsSingleton() bool {
	return len(p.Voters[0]) == 1 && len(p.Voters[1]) == 0
}

type matchAckIndexer map[uint64]*Progress

var _ quorum.AckedIndexer = matchAckIndexer(nil)

// AckedIndex implements IndexLookuper.
func (l matchAckIndexer) AckedIndex(id uint64) (quorum.Index, bool) {
	pr, ok := l[id]
	if !ok {
		return 0, false
	}
	return quorum.Index(pr.Match), true
}

// Committed returns the largest log index known to be committed based on what
// the voting members of the group have acknowledged.
func (p *ProgressTracker) Committed() uint64 {
	return uint64(p.Voters.CommittedIndex(matchAckIndexer(p.Progress)))
}

func insertionSort(sl []uint64) {
	a, b := 0, len(sl)
	for i := a + 1; i < b; i++ {
		for j := i; j > a && sl[j] < sl[j-1]; j-- {
			sl[j], sl[j-1] = sl[j-1], sl[j]
		}
	}
}

// Visit invokes the supplied closure for all tracked progresses in stable order.
func (p *ProgressTracker) Visit(f func(id uint64, pr *Progress)) {
	n := len(p.Progress)
	// We need to sort the IDs and don't want to allocate since this is hot code.
	// The optimization here mirrors that in `(MajorityConfig).CommittedIndex`,
	// see there for details.
	var sl [7]uint64
	var ids []uint64
	if len(sl) >= n {
		ids = sl[:n]
	} else {
		ids = make([]uint64, n)
	}
	for id := range p.Progress {
		n--
		ids[n] = id
	}
	insertionSort(ids)
	for _, id := range ids {
		f(id, p.Progress[id])
	}
}

// QuorumActive returns true if the quorum is active from the view of the local
// raft state machine. Otherwise, it returns false.
func (p *ProgressTracker) QuorumActive() bool {
	votes := map[uint64]bool{}
	p.Visit(func(id uint64, pr *Progress) {
		if pr.IsLearner {
			return
		}
		votes[id] = pr.RecentActive
	})

	return p.Voters.VoteResult(votes) == quorum.VoteWon
}

// VoterNodes returns a sorted slice of voters.
func (p *ProgressTracker) VoterNodes() []uint64 {
	m := p.Voters.IDs()
	nodes := make([]uint64, 0, len(m))
	for id := range m {
		nodes = append(nodes, id)
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i] < nodes[j] })
	return nodes
}

// LearnerNodes returns a sorted slice of learners.
func (p *ProgressTracker) LearnerNodes() []uint64 {
	if len(p.Learners) == 0 {
		return nil
	}
	nodes := make([]uint64, 0, len(p.Learners))
	for id := range p.Learners {
		nodes = append(nodes, id)
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i] < nodes[j] })
	return nodes
}

// ResetVotes prepares for a new round of vote counting via recordVote.
func (p *ProgressTracker) ResetVotes() {
	p.Votes = map[uint64]bool{}
}

// RecordVote records that the node with the given id voted for this Raft
// instance if v == true (and declined it otherwise).
func (p *ProgressTracker) RecordVote(id uint64, v bool) {
	_, ok := p.Votes[id]
	if !ok {
		p.Votes[id] = v
	}
}

// TallyVotes returns the number of granted and rejected Votes, and whether the
// election outcome is known.
func (p *ProgressTracker) TallyVotes() (granted int, rejected int, _ quorum.VoteResult) {
	// Make sure to populate granted/rejected correctly even if the Votes slice
	// contains members no longer part of the configuration. This doesn't really
	// matter in the way the numbers are used (they're informational), but might
	// as well get it right.
	for id, pr := range p.Progress {
		if pr.IsLearner {
			continue
		}
		v, voted := p.Votes[id]
		if !voted {
			continue
		}
		if v {
			granted++
		} else {
			rejected++
		}
	}
	result := p.Voters.VoteResult(p.Votes)
	return granted, rejected, result
}
