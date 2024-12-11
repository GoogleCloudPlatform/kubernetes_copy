// Copyright 2015 The etcd Authors
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

package raft

import (
	"errors"

	pb "go.etcd.io/raft/v3/raftpb"
	"go.etcd.io/raft/v3/tracker"
)

// ErrStepLocalMsg is returned when try to step a local raft message
var ErrStepLocalMsg = errors.New("raft: cannot step raft local message")

// ErrStepPeerNotFound is returned when try to step a response message
// but there is no peer found in raft.trk for that node.
var ErrStepPeerNotFound = errors.New("raft: cannot step as peer not found")

// RawNode is a thread-unsafe Node.
// The methods of this struct correspond to the methods of Node and are described
// more fully there.
type RawNode struct {
	raft               *raft
	asyncStorageWrites bool

	// Mutable fields.
	prevSoftSt     *SoftState
	prevHardSt     pb.HardState
	stepsOnAdvance []pb.Message
}

// NewRawNode instantiates a RawNode from the given configuration.
//
// See Bootstrap() for bootstrapping an initial state; this replaces the former
// 'peers' argument to this method (with identical behavior). However, It is
// recommended that instead of calling Bootstrap, applications bootstrap their
// state manually by setting up a Storage that has a first index > 1 and which
// stores the desired ConfState as its InitialState.
func NewRawNode(config *Config) (*RawNode, error) {
	r := newRaft(config)
	rn := &RawNode{
		raft: r,
	}
	rn.asyncStorageWrites = config.AsyncStorageWrites
	ss := r.softState()
	rn.prevSoftSt = &ss
	rn.prevHardSt = r.hardState()
	return rn, nil
}

// Tick advances the internal logical clock by a single tick.
func (rn *RawNode) Tick() {
	rn.raft.tick()
}

// TickQuiesced advances the internal logical clock by a single tick without
// performing any other state machine processing. It allows the caller to avoid
// periodic heartbeats and elections when all of the peers in a Raft group are
// known to be at the same state. Expected usage is to periodically invoke Tick
// or TickQuiesced depending on whether the group is "active" or "quiesced".
//
// WARNING: Be very careful about using this method as it subverts the Raft
// state machine. You should probably be using Tick instead.
//
// DEPRECATED: This method will be removed in a future release.
func (rn *RawNode) TickQuiesced() {
	rn.raft.electionElapsed++
}

// Campaign causes this RawNode to transition to candidate state.
func (rn *RawNode) Campaign() error {
	return rn.raft.Step(pb.Message{
		Type: pb.MsgHup,
	})
}

// Propose proposes data be appended to the raft log.
func (rn *RawNode) Propose(data []byte) error {
	return rn.raft.Step(pb.Message{
		Type: pb.MsgProp,
		From: rn.raft.id,
		Entries: []pb.Entry{
			{Data: data},
		}})
}

// ProposeConfChange proposes a config change. See (Node).ProposeConfChange for
// details.
func (rn *RawNode) ProposeConfChange(cc pb.ConfChangeI) error {
	m, err := confChangeToMsg(cc)
	if err != nil {
		return err
	}
	return rn.raft.Step(m)
}

// ApplyConfChange applies a config change to the local node. The app must call
// this when it applies a configuration change, except when it decides to reject
// the configuration change, in which case no call must take place.
func (rn *RawNode) ApplyConfChange(cc pb.ConfChangeI) *pb.ConfState {
	cs := rn.raft.applyConfChange(cc.AsV2())
	return &cs
}

// Step advances the state machine using the given message.
func (rn *RawNode) Step(m pb.Message) error {
	// Ignore unexpected local messages receiving over network.
	if IsLocalMsg(m.Type) && !IsLocalMsgTarget(m.From) {
		return ErrStepLocalMsg
	}
	if IsResponseMsg(m.Type) && !IsLocalMsgTarget(m.From) && rn.raft.trk.Progress[m.From] == nil {
		return ErrStepPeerNotFound
	}
	return rn.raft.Step(m)
}

// Ready returns the outstanding work that the application needs to handle. This
// includes appending and applying entries or a snapshot, updating the HardState,
// and sending messages. The returned Ready() *must* be handled and subsequently
// passed back via Advance().
func (rn *RawNode) Ready() Ready {
	rd := rn.readyWithoutAccept()
	rn.acceptReady(rd)
	return rd
}

// readyWithoutAccept returns a Ready. This is a read-only operation, i.e. there
// is no obligation that the Ready must be handled.
func (rn *RawNode) readyWithoutAccept() Ready {
	r := rn.raft

	rd := Ready{
		Entries:          r.raftLog.nextUnstableEnts(),
		CommittedEntries: r.raftLog.nextCommittedEnts(rn.applyUnstableEntries()),
		Messages:         r.msgs,
	}
	if softSt := r.softState(); !softSt.equal(rn.prevSoftSt) {
		// Allocate only when SoftState changes.
		escapingSoftSt := softSt
		rd.SoftState = &escapingSoftSt
	}
	if hardSt := r.hardState(); !isHardStateEqual(hardSt, rn.prevHardSt) {
		rd.HardState = hardSt
	}
	if r.raftLog.hasNextUnstableSnapshot() {
		rd.Snapshot = *r.raftLog.nextUnstableSnapshot()
	}
	if len(r.readStates) != 0 {
		rd.ReadStates = r.readStates
	}
	rd.MustSync = MustSync(r.hardState(), rn.prevHardSt, len(rd.Entries))

	if rn.asyncStorageWrites {
		// If async storage writes are enabled, enqueue messages to
		// local storage threads, where applicable.
		if needStorageAppendMsg(r, rd) {
			m := newStorageAppendMsg(r, rd)
			rd.Messages = append(rd.Messages, m)
		}
		if needStorageApplyMsg(rd) {
			m := newStorageApplyMsg(r, rd)
			rd.Messages = append(rd.Messages, m)
		}
	} else {
		// If async storage writes are disabled, immediately enqueue
		// msgsAfterAppend to be sent out. The Ready struct contract
		// mandates that Messages cannot be sent until after Entries
		// are written to stable storage.
		for _, m := range r.msgsAfterAppend {
			if m.To != r.id {
				rd.Messages = append(rd.Messages, m)
			}
		}
	}

	return rd
}

// MustSync returns true if the hard state and count of Raft entries indicate
// that a synchronous write to persistent storage is required.
func MustSync(st, prevst pb.HardState, entsnum int) bool {
	// Persistent state on all servers:
	// (Updated on stable storage before responding to RPCs)
	// currentTerm
	// votedFor
	// log entries[]
	return entsnum != 0 || st.Vote != prevst.Vote || st.Term != prevst.Term
}

func needStorageAppendMsg(r *raft, rd Ready) bool {
	// Return true if log entries, hard state, or a snapshot need to be written
	// to stable storage. Also return true if any messages are contingent on all
	// prior MsgStorageAppend being processed.
	return len(rd.Entries) > 0 ||
		!IsEmptyHardState(rd.HardState) ||
		!IsEmptySnap(rd.Snapshot) ||
		len(r.msgsAfterAppend) > 0
}

func needStorageAppendRespMsg(r *raft, rd Ready) bool {
	// Return true if raft needs to hear about stabilized entries or an applied
	// snapshot. See the comment in newStorageAppendRespMsg, which explains why
	// we check hasNextOrInProgressUnstableEnts instead of len(rd.Entries) > 0.
	return r.raftLog.hasNextOrInProgressUnstableEnts() ||
		!IsEmptySnap(rd.Snapshot)
}

// newStorageAppendMsg creates the message that should be sent to the local
// append thread to instruct it to append log entries, write an updated hard
// state, and apply a snapshot. The message also carries a set of responses
// that should be delivered after the rest of the message is processed. Used
// with AsyncStorageWrites.
func newStorageAppendMsg(r *raft, rd Ready) pb.Message {
	m := pb.Message{
		Type:    pb.MsgStorageAppend,
		To:      LocalAppendThread,
		From:    r.id,
		Entries: rd.Entries,
	}
	if !IsEmptyHardState(rd.HardState) {
		// If the Ready includes a HardState update, assign each of its fields
		// to the corresponding fields in the Message. This allows clients to
		// reconstruct the HardState and save it to stable storage.
		//
		// If the Ready does not include a HardState update, make sure to not
		// assign a value to any of the fields so that a HardState reconstructed
		// from them will be empty (return true from raft.IsEmptyHardState).
		m.Term = rd.Term
		m.Vote = rd.Vote
		m.Commit = rd.Commit
	}
	if !IsEmptySnap(rd.Snapshot) {
		snap := rd.Snapshot
		m.Snapshot = &snap
	}
	// Attach all messages in msgsAfterAppend as responses to be delivered after
	// the message is processed, along with a self-directed MsgStorageAppendResp
	// to acknowledge the entry stability.
	//
	// NB: it is important for performance that MsgStorageAppendResp message be
	// handled after self-directed MsgAppResp messages on the leader (which will
	// be contained in msgsAfterAppend). This ordering allows the MsgAppResp
	// handling to use a fast-path in r.raftLog.term() before the newly appended
	// entries are removed from the unstable log.
	m.Responses = r.msgsAfterAppend
	if needStorageAppendRespMsg(r, rd) {
		m.Responses = append(m.Responses, newStorageAppendRespMsg(r, rd))
	}
	return m
}

// newStorageAppendRespMsg creates the message that should be returned to node
// after the unstable log entries, hard state, and snapshot in the current Ready
// (along with those in all prior Ready structs) have been saved to stable
// storage.
func newStorageAppendRespMsg(r *raft, rd Ready) pb.Message {
	m := pb.Message{
		Type: pb.MsgStorageAppendResp,
		To:   r.id,
		From: LocalAppendThread,
		// Dropped after term change, see below.
		Term: r.Term,
	}
	if r.raftLog.hasNextOrInProgressUnstableEnts() {
		// If the raft log has unstable entries, attach the last index and term of the
		// append to the response message. This (index, term) tuple will be handed back
		// and consulted when the stability of those log entries is signaled to the
		// unstable. If the (index, term) match the unstable log by the time the
		// response is received (unstable.stableTo), the unstable log can be truncated.
		//
		// However, with just this logic, there would be an ABA problem[^1] that could
		// lead to the unstable log and the stable log getting out of sync temporarily
		// and leading to an inconsistent view. Consider the following example with 5
		// nodes, A B C D E:
		//
		//  1. A is the leader.
		//  2. A proposes some log entries but only B receives these entries.
		//  3. B gets the Ready and the entries are appended asynchronously.
		//  4. A crashes and C becomes leader after getting a vote from D and E.
		//  5. C proposes some log entries and B receives these entries, overwriting the
		//     previous unstable log entries that are in the process of being appended.
		//     The entries have a larger term than the previous entries but the same
		//     indexes. It begins appending these new entries asynchronously.
		//  6. C crashes and A restarts and becomes leader again after getting the vote
		//     from D and E.
		//  7. B receives the entries from A which are the same as the ones from step 2,
		//     overwriting the previous unstable log entries that are in the process of
		//     being appended from step 5. The entries have the original terms and
		//     indexes from step 2. Recall that log entries retain their original term
		//     numbers when a leader replicates entries from previous terms. It begins
		//     appending these new entries asynchronously.
		//  8. The asynchronous log appends from the first Ready complete and stableTo
		//     is called.
		//  9. However, the log entries from the second Ready are still in the
		//     asynchronous append pipeline and will overwrite (in stable storage) the
		//     entries from the first Ready at some future point. We can't truncate the
		//     unstable log yet or a future read from Storage might see the entries from
		//     step 5 before they have been replaced by the entries from step 7.
		//     Instead, we must wait until we are sure that the entries are stable and
		//     that no in-progress appends might overwrite them before removing entries
		//     from the unstable log.
		//
		// To prevent these kinds of problems, we also attach the current term to the
		// MsgStorageAppendResp (above). If the term has changed by the time the
		// MsgStorageAppendResp if returned, the response is ignored and the unstable
		// log is not truncated. The unstable log is only truncated when the term has
		// remained unchanged from the time that the MsgStorageAppend was sent to the
		// time that the MsgStorageAppendResp is received, indicating that no-one else
		// is in the process of truncating the stable log.
		//
		// However, this replaces a correctness problem with a liveness problem. If we
		// only attempted to truncate the unstable log when appending new entries but
		// also occasionally dropped these responses, then quiescence of new log entries
		// could lead to the unstable log never being truncated.
		//
		// To combat this, we attempt to truncate the log on all MsgStorageAppendResp
		// messages where the unstable log is not empty, not just those associated with
		// entry appends. This includes MsgStorageAppendResp messages associated with an
		// updated HardState, which occur after a term change.
		//
		// In other words, we set Index and LogTerm in a block that looks like:
		//
		//  if r.raftLog.hasNextOrInProgressUnstableEnts() { ... }
		//
		// not like:
		//
		//  if len(rd.Entries) > 0 { ... }
		//
		// To do so, we attach r.raftLog.lastIndex() and r.raftLog.lastTerm(), not the
		// (index, term) of the last entry in rd.Entries. If rd.Entries is not empty,
		// these will be the same. However, if rd.Entries is empty, we still want to
		// attest that this (index, term) is correct at the current term, in case the
		// MsgStorageAppend that contained the last entry in the unstable slice carried
		// an earlier term and was dropped.
		//
		// A MsgStorageAppend with a new term is emitted on each term change. This is
		// the same condition that causes MsgStorageAppendResp messages with earlier
		// terms to be ignored. As a result, we are guaranteed that, assuming a bounded
		// number of term changes, there will eventually be a MsgStorageAppendResp
		// message that is not ignored. This means that entries in the unstable log
		// which have been appended to stable storage will eventually be truncated and
		// dropped from memory.
		//
		// [^1]: https://en.wikipedia.org/wiki/ABA_problem
		m.Index = r.raftLog.lastIndex()
		m.LogTerm = r.raftLog.lastTerm()
	}
	if !IsEmptySnap(rd.Snapshot) {
		snap := rd.Snapshot
		m.Snapshot = &snap
	}
	return m
}

func needStorageApplyMsg(rd Ready) bool     { return len(rd.CommittedEntries) > 0 }
func needStorageApplyRespMsg(rd Ready) bool { return needStorageApplyMsg(rd) }

// newStorageApplyMsg creates the message that should be sent to the local
// apply thread to instruct it to apply committed log entries. The message
// also carries a response that should be delivered after the rest of the
// message is processed. Used with AsyncStorageWrites.
func newStorageApplyMsg(r *raft, rd Ready) pb.Message {
	ents := rd.CommittedEntries
	return pb.Message{
		Type:    pb.MsgStorageApply,
		To:      LocalApplyThread,
		From:    r.id,
		Term:    0, // committed entries don't apply under a specific term
		Entries: ents,
		Responses: []pb.Message{
			newStorageApplyRespMsg(r, ents),
		},
	}
}

// newStorageApplyRespMsg creates the message that should be returned to node
// after the committed entries in the current Ready (along with those in all
// prior Ready structs) have been applied to the local state machine.
func newStorageApplyRespMsg(r *raft, ents []pb.Entry) pb.Message {
	return pb.Message{
		Type:    pb.MsgStorageApplyResp,
		To:      r.id,
		From:    LocalApplyThread,
		Term:    0, // committed entries don't apply under a specific term
		Entries: ents,
	}
}

// acceptReady is called when the consumer of the RawNode has decided to go
// ahead and handle a Ready. Nothing must alter the state of the RawNode between
// this call and the prior call to Ready().
func (rn *RawNode) acceptReady(rd Ready) {
	if rd.SoftState != nil {
		rn.prevSoftSt = rd.SoftState
	}
	if !IsEmptyHardState(rd.HardState) {
		rn.prevHardSt = rd.HardState
	}
	if len(rd.ReadStates) != 0 {
		rn.raft.readStates = nil
	}
	if !rn.asyncStorageWrites {
		if len(rn.stepsOnAdvance) != 0 {
			rn.raft.logger.Panicf("two accepted Ready structs without call to Advance")
		}
		for _, m := range rn.raft.msgsAfterAppend {
			if m.To == rn.raft.id {
				rn.stepsOnAdvance = append(rn.stepsOnAdvance, m)
			}
		}
		if needStorageAppendRespMsg(rn.raft, rd) {
			m := newStorageAppendRespMsg(rn.raft, rd)
			rn.stepsOnAdvance = append(rn.stepsOnAdvance, m)
		}
		if needStorageApplyRespMsg(rd) {
			m := newStorageApplyRespMsg(rn.raft, rd.CommittedEntries)
			rn.stepsOnAdvance = append(rn.stepsOnAdvance, m)
		}
	}
	rn.raft.msgs = nil
	rn.raft.msgsAfterAppend = nil
	rn.raft.raftLog.acceptUnstable()
	if len(rd.CommittedEntries) > 0 {
		ents := rd.CommittedEntries
		index := ents[len(ents)-1].Index
		rn.raft.raftLog.acceptApplying(index, entsSize(ents), rn.applyUnstableEntries())
	}
}

// applyUnstableEntries returns whether entries are allowed to be applied once
// they are known to be committed but before they have been written locally to
// stable storage.
func (rn *RawNode) applyUnstableEntries() bool {
	return !rn.asyncStorageWrites
}

// HasReady called when RawNode user need to check if any Ready pending.
func (rn *RawNode) HasReady() bool {
	// TODO(nvanbenschoten): order these cases in terms of cost and frequency.
	r := rn.raft
	if softSt := r.softState(); !softSt.equal(rn.prevSoftSt) {
		return true
	}
	if hardSt := r.hardState(); !IsEmptyHardState(hardSt) && !isHardStateEqual(hardSt, rn.prevHardSt) {
		return true
	}
	if r.raftLog.hasNextUnstableSnapshot() {
		return true
	}
	if len(r.msgs) > 0 || len(r.msgsAfterAppend) > 0 {
		return true
	}
	if r.raftLog.hasNextUnstableEnts() || r.raftLog.hasNextCommittedEnts(rn.applyUnstableEntries()) {
		return true
	}
	if len(r.readStates) != 0 {
		return true
	}
	return false
}

// Advance notifies the RawNode that the application has applied and saved progress in the
// last Ready results.
//
// NOTE: Advance must not be called when using AsyncStorageWrites. Response messages from
// the local append and apply threads take its place.
func (rn *RawNode) Advance(_ Ready) {
	// The actions performed by this function are encoded into stepsOnAdvance in
	// acceptReady. In earlier versions of this library, they were computed from
	// the provided Ready struct. Retain the unused parameter for compatibility.
	if rn.asyncStorageWrites {
		rn.raft.logger.Panicf("Advance must not be called when using AsyncStorageWrites")
	}
	for i, m := range rn.stepsOnAdvance {
		_ = rn.raft.Step(m)
		rn.stepsOnAdvance[i] = pb.Message{}
	}
	rn.stepsOnAdvance = rn.stepsOnAdvance[:0]
}

// Status returns the current status of the given group. This allocates, see
// BasicStatus and WithProgress for allocation-friendlier choices.
func (rn *RawNode) Status() Status {
	status := getStatus(rn.raft)
	return status
}

// BasicStatus returns a BasicStatus. Notably this does not contain the
// Progress map; see WithProgress for an allocation-free way to inspect it.
func (rn *RawNode) BasicStatus() BasicStatus {
	return getBasicStatus(rn.raft)
}

// ProgressType indicates the type of replica a Progress corresponds to.
type ProgressType byte

const (
	// ProgressTypePeer accompanies a Progress for a regular peer replica.
	ProgressTypePeer ProgressType = iota
	// ProgressTypeLearner accompanies a Progress for a learner replica.
	ProgressTypeLearner
)

// WithProgress is a helper to introspect the Progress for this node and its
// peers.
func (rn *RawNode) WithProgress(visitor func(id uint64, typ ProgressType, pr tracker.Progress)) {
	rn.raft.trk.Visit(func(id uint64, pr *tracker.Progress) {
		typ := ProgressTypePeer
		if pr.IsLearner {
			typ = ProgressTypeLearner
		}
		p := *pr
		p.Inflights = nil
		visitor(id, typ, p)
	})
}

// ReportUnreachable reports the given node is not reachable for the last send.
func (rn *RawNode) ReportUnreachable(id uint64) {
	_ = rn.raft.Step(pb.Message{Type: pb.MsgUnreachable, From: id})
}

// ReportSnapshot reports the status of the sent snapshot.
func (rn *RawNode) ReportSnapshot(id uint64, status SnapshotStatus) {
	rej := status == SnapshotFailure

	_ = rn.raft.Step(pb.Message{Type: pb.MsgSnapStatus, From: id, Reject: rej})
}

// TransferLeader tries to transfer leadership to the given transferee.
func (rn *RawNode) TransferLeader(transferee uint64) {
	_ = rn.raft.Step(pb.Message{Type: pb.MsgTransferLeader, From: transferee})
}

// ForgetLeader forgets a follower's current leader, changing it to None.
// See (Node).ForgetLeader for details.
func (rn *RawNode) ForgetLeader() error {
	return rn.raft.Step(pb.Message{Type: pb.MsgForgetLeader})
}

// ReadIndex requests a read state. The read state will be set in ready.
// Read State has a read index. Once the application advances further than the read
// index, any linearizable read requests issued before the read request can be
// processed safely. The read state will have the same rctx attached.
func (rn *RawNode) ReadIndex(rctx []byte) {
	_ = rn.raft.Step(pb.Message{Type: pb.MsgReadIndex, Entries: []pb.Entry{{Data: rctx}}})
}
