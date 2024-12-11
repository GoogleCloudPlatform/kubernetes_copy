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

/*
Package raft sends and receives messages in the Protocol Buffer format
defined in the raftpb package.

Raft is a protocol with which a cluster of nodes can maintain a replicated state machine.
The state machine is kept in sync through the use of a replicated log.
For more details on Raft, see "In Search of an Understandable Consensus Algorithm"
(https://raft.github.io/raft.pdf) by Diego Ongaro and John Ousterhout.

A simple example application, _raftexample_, is also available to help illustrate
how to use this package in practice:
https://github.com/etcd-io/etcd/tree/main/contrib/raftexample

# Usage

The primary object in raft is a Node. You either start a Node from scratch
using raft.StartNode or start a Node from some initial state using raft.RestartNode.

To start a node from scratch:

	storage := raft.NewMemoryStorage()
	c := &Config{
	  ID:              0x01,
	  ElectionTick:    10,
	  HeartbeatTick:   1,
	  Storage:         storage,
	  MaxSizePerMsg:   4096,
	  MaxInflightMsgs: 256,
	}
	n := raft.StartNode(c, []raft.Peer{{ID: 0x02}, {ID: 0x03}})

To restart a node from previous state:

	storage := raft.NewMemoryStorage()

	// recover the in-memory storage from persistent
	// snapshot, state and entries.
	storage.ApplySnapshot(snapshot)
	storage.SetHardState(state)
	storage.Append(entries)

	c := &Config{
	  ID:              0x01,
	  ElectionTick:    10,
	  HeartbeatTick:   1,
	  Storage:         storage,
	  MaxSizePerMsg:   4096,
	  MaxInflightMsgs: 256,
	}

	// restart raft without peer information.
	// peer information is already included in the storage.
	n := raft.RestartNode(c)

Now that you are holding onto a Node you have a few responsibilities:

First, you must read from the Node.Ready() channel and process the updates
it contains. These steps may be performed in parallel, except as noted in step
2.

1. Write HardState, Entries, and Snapshot to persistent storage if they are
not empty. Note that when writing an Entry with Index i, any
previously-persisted entries with Index >= i must be discarded.

2. Send all Messages to the nodes named in the To field. It is important that
no messages be sent until the latest HardState has been persisted to disk,
and all Entries written by any previous Ready batch (Messages may be sent while
entries from the same batch are being persisted). To reduce the I/O latency, an
optimization can be applied to make leader write to disk in parallel with its
followers (as explained at section 10.2.1 in Raft thesis). If any Message has type
MsgSnap, call Node.ReportSnapshot() after it has been sent (these messages may be
large).

Note: Marshalling messages is not thread-safe; it is important that you
make sure that no new entries are persisted while marshalling.
The easiest way to achieve this is to serialize the messages directly inside
your main raft loop.

3. Apply Snapshot (if any) and CommittedEntries to the state machine.
If any committed Entry has Type EntryConfChange, call Node.ApplyConfChange()
to apply it to the node. The configuration change may be cancelled at this point
by setting the NodeID field to zero before calling ApplyConfChange
(but ApplyConfChange must be called one way or the other, and the decision to cancel
must be based solely on the state machine and not external information such as
the observed health of the node).

4. Call Node.Advance() to signal readiness for the next batch of updates.
This may be done at any time after step 1, although all updates must be processed
in the order they were returned by Ready.

Second, all persisted log entries must be made available via an
implementation of the Storage interface. The provided MemoryStorage
type can be used for this (if you repopulate its state upon a
restart), or you can supply your own disk-backed implementation.

Third, when you receive a message from another node, pass it to Node.Step:

	func recvRaftRPC(ctx context.Context, m raftpb.Message) {
		n.Step(ctx, m)
	}

Finally, you need to call Node.Tick() at regular intervals (probably
via a time.Ticker). Raft has two important timeouts: heartbeat and the
election timeout. However, internally to the raft package time is
represented by an abstract "tick".

The total state machine handling loop will look something like this:

	for {
	  select {
	  case <-s.Ticker:
	    n.Tick()
	  case rd := <-s.Node.Ready():
	    saveToStorage(rd.State, rd.Entries, rd.Snapshot)
	    send(rd.Messages)
	    if !raft.IsEmptySnap(rd.Snapshot) {
	      processSnapshot(rd.Snapshot)
	    }
	    for _, entry := range rd.CommittedEntries {
	      process(entry)
	      if entry.Type == raftpb.EntryConfChange {
	        var cc raftpb.ConfChange
	        cc.Unmarshal(entry.Data)
	        s.Node.ApplyConfChange(cc)
	      }
	    }
	    s.Node.Advance()
	  case <-s.done:
	    return
	  }
	}

To propose changes to the state machine from your node take your application
data, serialize it into a byte slice and call:

	n.Propose(ctx, data)

If the proposal is committed, data will appear in committed entries with type
raftpb.EntryNormal. There is no guarantee that a proposed command will be
committed; you may have to re-propose after a timeout.

To add or remove a node in a cluster, build ConfChange struct 'cc' and call:

	n.ProposeConfChange(ctx, cc)

After config change is committed, some committed entry with type
raftpb.EntryConfChange will be returned. You must apply it to node through:

	var cc raftpb.ConfChange
	cc.Unmarshal(data)
	n.ApplyConfChange(cc)

Note: An ID represents a unique node in a cluster for all time. A
given ID MUST be used only once even if the old node has been removed.
This means that for example IP addresses make poor node IDs since they
may be reused. Node IDs must be non-zero.

# Usage with Asynchronous Storage Writes

The library can be configured with an alternate interface for local storage
writes that can provide better performance in the presence of high proposal
concurrency by minimizing interference between proposals. This feature is called
AsynchronousStorageWrites, and can be enabled using the flag on the Config
struct with the same name.

When Asynchronous Storage Writes is enabled, the responsibility of code using
the library is different from what was presented above. Users still read from
the Node.Ready() channel. However, they process the updates it contains in a
different manner. Users no longer consult the HardState, Entries, and Snapshot
fields (steps 1 and 3 above). They also no longer call Node.Advance() to
indicate that they have processed all entries in the Ready (step 4 above).
Instead, all local storage operations are also communicated through messages
present in the Ready.Message slice.

The local storage messages come in two flavors. The first flavor is log append
messages, which target a LocalAppendThread and carry Entries, HardState, and a
Snapshot. The second flavor is entry application messages, which target a
LocalApplyThread and carry CommittedEntries. Messages to the same target must be
reliably processed in order. Messages to different targets can be processed in
any order.

Each local storage message carries a slice of response messages that must
delivered after the corresponding storage write has been completed. These
responses may target the same node or may target other nodes.

With Asynchronous Storage Writes enabled, the total state machine handling loop
will look something like this:

	for {
	  select {
	  case <-s.Ticker:
	    n.Tick()
	  case rd := <-s.Node.Ready():
	    for _, m := range rd.Messages {
	      switch m.To {
	      case raft.LocalAppendThread:
	        toAppend <- m
	      case raft.LocalApplyThread:
	        toApply <-m
	      default:
	        sendOverNetwork(m)
	      }
	    }
	  case <-s.done:
	    return
	  }
	}

Usage of Asynchronous Storage Writes will typically also contain a pair of
storage handler threads, one for log writes (append) and one for entry
application to the local state machine (apply). Those will look something like:

	// append thread
	go func() {
	  for {
	    select {
	    case m := <-toAppend:
	      saveToStorage(m.State, m.Entries, m.Snapshot)
	      send(m.Responses)
	    case <-s.done:
	      return
	    }
	  }
	}

	// apply thread
	go func() {
	  for {
	    select {
	    case m := <-toApply:
	      for _, entry := range m.CommittedEntries {
	        process(entry)
	        if entry.Type == raftpb.EntryConfChange {
	          var cc raftpb.ConfChange
	          cc.Unmarshal(entry.Data)
	          s.Node.ApplyConfChange(cc)
	        }
	      }
	      send(m.Responses)
	    case <-s.done:
	      return
	    }
	  }
	}

# Implementation notes

This implementation is up to date with the final Raft thesis
(https://github.com/ongardie/dissertation/blob/master/stanford.pdf), although our
implementation of the membership change protocol differs somewhat from
that described in chapter 4. The key invariant that membership changes
happen one node at a time is preserved, but in our implementation the
membership change takes effect when its entry is applied, not when it
is added to the log (so the entry is committed under the old
membership instead of the new). This is equivalent in terms of safety,
since the old and new configurations are guaranteed to overlap.

To ensure that we do not attempt to commit two membership changes at
once by matching log positions (which would be unsafe since they
should have different quorum requirements), we simply disallow any
proposed membership change while any uncommitted change appears in
the leader's log.

This approach introduces a problem when you try to remove a member
from a two-member cluster: If one of the members dies before the
other one receives the commit of the confchange entry, then the member
cannot be removed any more since the cluster cannot make progress.
For this reason it is highly recommended to use three or more nodes in
every cluster.

# MessageType

Package raft sends and receives message in Protocol Buffer format (defined
in raftpb package). Each state (follower, candidate, leader) implements its
own 'step' method ('stepFollower', 'stepCandidate', 'stepLeader') when
advancing with the given raftpb.Message. Each step is determined by its
raftpb.MessageType. Note that every step is checked by one common method
'Step' that safety-checks the terms of node and incoming message to prevent
stale log entries:

	'MsgHup' is used for election. If a node is a follower or candidate, the
	'tick' function in 'raft' struct is set as 'tickElection'. If a follower or
	candidate has not received any heartbeat before the election timeout, it
	passes 'MsgHup' to its Step method and becomes (or remains) a candidate to
	start a new election.

	'MsgBeat' is an internal type that signals the leader to send a heartbeat of
	the 'MsgHeartbeat' type. If a node is a leader, the 'tick' function in
	the 'raft' struct is set as 'tickHeartbeat', and triggers the leader to
	send periodic 'MsgHeartbeat' messages to its followers.

	'MsgProp' proposes to append data to its log entries. This is a special
	type to redirect proposals to leader. Therefore, send method overwrites
	raftpb.Message's term with its HardState's term to avoid attaching its
	local term to 'MsgProp'. When 'MsgProp' is passed to the leader's 'Step'
	method, the leader first calls the 'appendEntry' method to append entries
	to its log, and then calls 'bcastAppend' method to send those entries to
	its peers. When passed to candidate, 'MsgProp' is dropped. When passed to
	follower, 'MsgProp' is stored in follower's mailbox(msgs) by the send
	method. It is stored with sender's ID and later forwarded to leader by
	rafthttp package.

	'MsgApp' contains log entries to replicate. A leader calls bcastAppend,
	which calls sendAppend, which sends soon-to-be-replicated logs in 'MsgApp'
	type. When 'MsgApp' is passed to candidate's Step method, candidate reverts
	back to follower, because it indicates that there is a valid leader sending
	'MsgApp' messages. Candidate and follower respond to this message in
	'MsgAppResp' type.

	'MsgAppResp' is response to log replication request('MsgApp'). When
	'MsgApp' is passed to candidate or follower's Step method, it responds by
	calling 'handleAppendEntries' method, which sends 'MsgAppResp' to raft
	mailbox.

	'MsgVote' requests votes for election. When a node is a follower or
	candidate and 'MsgHup' is passed to its Step method, then the node calls
	'campaign' method to campaign itself to become a leader. Once 'campaign'
	method is called, the node becomes candidate and sends 'MsgVote' to peers
	in cluster to request votes. When passed to leader or candidate's Step
	method and the message's Term is lower than leader's or candidate's,
	'MsgVote' will be rejected ('MsgVoteResp' is returned with Reject true).
	If leader or candidate receives 'MsgVote' with higher term, it will revert
	back to follower. When 'MsgVote' is passed to follower, it votes for the
	sender only when sender's last term is greater than MsgVote's term or
	sender's last term is equal to MsgVote's term but sender's last committed
	index is greater than or equal to follower's.

	'MsgVoteResp' contains responses from voting request. When 'MsgVoteResp' is
	passed to candidate, the candidate calculates how many votes it has won. If
	it's more than majority (quorum), it becomes leader and calls 'bcastAppend'.
	If candidate receives majority of votes of denials, it reverts back to
	follower.

	'MsgPreVote' and 'MsgPreVoteResp' are used in an optional two-phase election
	protocol. When Config.PreVote is true, a pre-election is carried out first
	(using the same rules as a regular election), and no node increases its term
	number unless the pre-election indicates that the campaigning node would win.
	This minimizes disruption when a partitioned node rejoins the cluster.

	'MsgSnap' requests to install a snapshot message. When a node has just
	become a leader or the leader receives 'MsgProp' message, it calls
	'bcastAppend' method, which then calls 'sendAppend' method to each
	follower. In 'sendAppend', if a leader fails to get term or entries,
	the leader requests snapshot by sending 'MsgSnap' type message.

	'MsgSnapStatus' tells the result of snapshot install message. When a
	follower rejected 'MsgSnap', it indicates the snapshot request with
	'MsgSnap' had failed from network issues which causes the network layer
	to fail to send out snapshots to its followers. Then leader considers
	follower's progress as probe. When 'MsgSnap' were not rejected, it
	indicates that the snapshot succeeded and the leader sets follower's
	progress to probe and resumes its log replication.

	'MsgHeartbeat' sends heartbeat from leader. When 'MsgHeartbeat' is passed
	to candidate and message's term is higher than candidate's, the candidate
	reverts back to follower and updates its committed index from the one in
	this heartbeat. And it sends the message to its mailbox. When
	'MsgHeartbeat' is passed to follower's Step method and message's term is
	higher than follower's, the follower updates its leaderID with the ID
	from the message.

	'MsgHeartbeatResp' is a response to 'MsgHeartbeat'. When 'MsgHeartbeatResp'
	is passed to leader's Step method, the leader knows which follower
	responded. And only when the leader's last committed index is greater than
	follower's Match index, the leader runs 'sendAppend` method.

	'MsgUnreachable' tells that request(message) wasn't delivered. When
	'MsgUnreachable' is passed to leader's Step method, the leader discovers
	that the follower that sent this 'MsgUnreachable' is not reachable, often
	indicating 'MsgApp' is lost. When follower's progress state is replicate,
	the leader sets it back to probe.

	'MsgStorageAppend' is a message from a node to its local append storage
	thread to write entries, hard state, and/or a snapshot to stable storage.
	The message will carry one or more responses, one of which will be a
	'MsgStorageAppendResp' back to itself. The responses can also contain
	'MsgAppResp', 'MsgVoteResp', and 'MsgPreVoteResp' messages. Used with
	AsynchronousStorageWrites.

	'MsgStorageApply' is a message from a node to its local apply storage
	thread to apply committed entries. The message will carry one response,
	which will be a 'MsgStorageApplyResp' back to itself. Used with
	AsynchronousStorageWrites.
*/
package raft
