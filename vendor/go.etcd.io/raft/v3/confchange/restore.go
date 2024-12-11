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

package confchange

import (
	pb "go.etcd.io/raft/v3/raftpb"
	"go.etcd.io/raft/v3/tracker"
)

// toConfChangeSingle translates a conf state into 1) a slice of operations creating
// first the config that will become the outgoing one, and then the incoming one, and
// b) another slice that, when applied to the config resulted from 1), represents the
// ConfState.
func toConfChangeSingle(cs pb.ConfState) (out []pb.ConfChangeSingle, in []pb.ConfChangeSingle) {
	// Example to follow along this code:
	// voters=(1 2 3) learners=(5) outgoing=(1 2 4 6) learners_next=(4)
	//
	// This means that before entering the joint config, the configuration
	// had voters (1 2 4 6) and perhaps some learners that are already gone.
	// The new set of voters is (1 2 3), i.e. (1 2) were kept around, and (4 6)
	// are no longer voters; however 4 is poised to become a learner upon leaving
	// the joint state.
	// We can't tell whether 5 was a learner before entering the joint config,
	// but it doesn't matter (we'll pretend that it wasn't).
	//
	// The code below will construct
	// outgoing = add 1; add 2; add 4; add 6
	// incoming = remove 1; remove 2; remove 4; remove 6
	//            add 1;    add 2;    add 3;
	//            add-learner 5;
	//            add-learner 4;
	//
	// So, when starting with an empty config, after applying 'outgoing' we have
	//
	//   quorum=(1 2 4 6)
	//
	// From which we enter a joint state via 'incoming'
	//
	//   quorum=(1 2 3)&&(1 2 4 6) learners=(5) learners_next=(4)
	//
	// as desired.

	for _, id := range cs.VotersOutgoing {
		// If there are outgoing voters, first add them one by one so that the
		// (non-joint) config has them all.
		out = append(out, pb.ConfChangeSingle{
			Type:   pb.ConfChangeAddNode,
			NodeID: id,
		})

	}

	// We're done constructing the outgoing slice, now on to the incoming one
	// (which will apply on top of the config created by the outgoing slice).

	// First, we'll remove all of the outgoing voters.
	for _, id := range cs.VotersOutgoing {
		in = append(in, pb.ConfChangeSingle{
			Type:   pb.ConfChangeRemoveNode,
			NodeID: id,
		})
	}
	// Then we'll add the incoming voters and learners.
	for _, id := range cs.Voters {
		in = append(in, pb.ConfChangeSingle{
			Type:   pb.ConfChangeAddNode,
			NodeID: id,
		})
	}
	for _, id := range cs.Learners {
		in = append(in, pb.ConfChangeSingle{
			Type:   pb.ConfChangeAddLearnerNode,
			NodeID: id,
		})
	}
	// Same for LearnersNext; these are nodes we want to be learners but which
	// are currently voters in the outgoing config.
	for _, id := range cs.LearnersNext {
		in = append(in, pb.ConfChangeSingle{
			Type:   pb.ConfChangeAddLearnerNode,
			NodeID: id,
		})
	}
	return out, in
}

func chain(chg Changer, ops ...func(Changer) (tracker.Config, tracker.ProgressMap, error)) (tracker.Config, tracker.ProgressMap, error) {
	for _, op := range ops {
		cfg, trk, err := op(chg)
		if err != nil {
			return tracker.Config{}, nil, err
		}
		chg.Tracker.Config = cfg
		chg.Tracker.Progress = trk
	}
	return chg.Tracker.Config, chg.Tracker.Progress, nil
}

// Restore takes a Changer (which must represent an empty configuration), and
// runs a sequence of changes enacting the configuration described in the
// ConfState.
//
// TODO(tbg) it's silly that this takes a Changer. Unravel this by making sure
// the Changer only needs a ProgressMap (not a whole Tracker) at which point
// this can just take LastIndex and MaxInflight directly instead and cook up
// the results from that alone.
func Restore(chg Changer, cs pb.ConfState) (tracker.Config, tracker.ProgressMap, error) {
	outgoing, incoming := toConfChangeSingle(cs)

	var ops []func(Changer) (tracker.Config, tracker.ProgressMap, error)

	if len(outgoing) == 0 {
		// No outgoing config, so just apply the incoming changes one by one.
		for _, cc := range incoming {
			cc := cc // loop-local copy
			ops = append(ops, func(chg Changer) (tracker.Config, tracker.ProgressMap, error) {
				return chg.Simple(cc)
			})
		}
	} else {
		// The ConfState describes a joint configuration.
		//
		// First, apply all of the changes of the outgoing config one by one, so
		// that it temporarily becomes the incoming active config. For example,
		// if the config is (1 2 3)&(2 3 4), this will establish (2 3 4)&().
		for _, cc := range outgoing {
			cc := cc // loop-local copy
			ops = append(ops, func(chg Changer) (tracker.Config, tracker.ProgressMap, error) {
				return chg.Simple(cc)
			})
		}
		// Now enter the joint state, which rotates the above additions into the
		// outgoing config, and adds the incoming config in. Continuing the
		// example above, we'd get (1 2 3)&(2 3 4), i.e. the incoming operations
		// would be removing 2,3,4 and then adding in 1,2,3 while transitioning
		// into a joint state.
		ops = append(ops, func(chg Changer) (tracker.Config, tracker.ProgressMap, error) {
			return chg.EnterJoint(cs.AutoLeave, incoming...)
		})
	}

	return chain(chg, ops...)
}
