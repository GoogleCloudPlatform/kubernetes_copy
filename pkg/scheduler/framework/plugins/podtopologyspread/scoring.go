/*
Copyright 2019 The Kubernetes Authors.

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

package podtopologyspread

import (
	"context"
	"fmt"
	"math"
	"sync/atomic"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/component-helpers/scheduling/corev1/nodeaffinity"
	"k8s.io/kubernetes/pkg/scheduler/framework"
)

const preScoreStateKey = "PreScore" + Name
const invalidScore = -1

// preScoreState computed at PreScore and used at Score.
// Fields are exported for comparison during testing.
type preScoreState struct {
	Constraints []topologySpreadConstraint
	// IgnoredNodes is a set of node names which miss some Constraints[*].topologyKey.
	IgnoredNodes sets.Set[string]
	// TopologyValueToPodCounts is a slice indexed by constraint index.
	// Each entry is keyed with topology value, and valued with the number of matching pods.
	TopologyValueToPodCounts []map[string]*int64
	// TopologyNormalizingWeight is the weight we give to the counts per topology.
	// This allows the pod counts of smaller topologies to not be watered down by
	// bigger ones.
	TopologyNormalizingWeight []float64
}

// Clone implements the mandatory Clone interface. We don't really copy the data since
// there is no need for that.
func (s *preScoreState) Clone() framework.StateData {
	return s
}

// initPreScoreState iterates "filteredNodes" to filter out the nodes which
// don't have required topologyKey(s), and initialize:
// 1) s.TopologyPairToPodCounts: keyed with both eligible topology pair and node names.
// 2) s.IgnoredNodes: the set of nodes that shouldn't be scored.
// 3) s.TopologyNormalizingWeight: The weight to be given to each constraint based on the number of values in a topology.
func (pl *PodTopologySpread) initPreScoreState(s *preScoreState, pod *v1.Pod, filteredNodes []*framework.NodeInfo, requireAllTopologies bool) error {
	var err error
	if len(pod.Spec.TopologySpreadConstraints) > 0 {
		s.Constraints, err = pl.filterTopologySpreadConstraints(
			pod.Spec.TopologySpreadConstraints,
			pod.Labels,
			v1.ScheduleAnyway,
		)
		if err != nil {
			return fmt.Errorf("obtaining pod's soft topology spread constraints: %w", err)
		}
	} else {
		s.Constraints, err = pl.buildDefaultConstraints(pod, v1.ScheduleAnyway)
		if err != nil {
			return fmt.Errorf("setting default soft topology spread constraints: %w", err)
		}
	}
	if len(s.Constraints) == 0 {
		return nil
	}
	s.TopologyValueToPodCounts = make([]map[string]*int64, len(s.Constraints))
	for i := 0; i < len(s.Constraints); i++ {
		s.TopologyValueToPodCounts[i] = make(map[string]*int64)
	}
	topoSize := make([]int, len(s.Constraints))
	for _, node := range filteredNodes {
		if requireAllTopologies && !nodeLabelsMatchSpreadConstraints(node.Node().Labels, s.Constraints) {
			// Nodes which don't have all required topologyKeys present are ignored
			// when scoring later.
			s.IgnoredNodes.Insert(node.Node().Name)
			continue
		}
		for i, constraint := range s.Constraints {
			// per-node counts are calculated during Score.
			if constraint.TopologyKey == v1.LabelHostname {
				continue
			}
			value := node.Node().Labels[constraint.TopologyKey]
			if s.TopologyValueToPodCounts[i][value] == nil {
				s.TopologyValueToPodCounts[i][value] = new(int64)
				topoSize[i]++
			}
		}
	}

	s.TopologyNormalizingWeight = make([]float64, len(s.Constraints))
	for i, c := range s.Constraints {
		sz := topoSize[i]
		if c.TopologyKey == v1.LabelHostname {
			sz = len(filteredNodes) - len(s.IgnoredNodes)
		}
		s.TopologyNormalizingWeight[i] = topologyNormalizingWeight(sz)
	}
	return nil
}

// PreScore builds and writes cycle state used by Score and NormalizeScore.
func (pl *PodTopologySpread) PreScore(
	ctx context.Context,
	cycleState *framework.CycleState,
	pod *v1.Pod,
	filteredNodes []*framework.NodeInfo,
) *framework.Status {
	allNodes, err := pl.sharedLister.NodeInfos().List()
	if err != nil {
		return framework.AsStatus(fmt.Errorf("getting all nodes: %w", err))
	}

	if len(allNodes) == 0 {
		// No need to score.
		return framework.NewStatus(framework.Skip)
	}

	state := &preScoreState{
		IgnoredNodes: sets.New[string](),
	}
	// Only require that nodes have all the topology labels if using
	// non-system-default spreading rules. This allows nodes that don't have a
	// zone label to still have hostname spreading.
	requireAllTopologies := len(pod.Spec.TopologySpreadConstraints) > 0 || !pl.systemDefaulted
	err = pl.initPreScoreState(state, pod, filteredNodes, requireAllTopologies)
	if err != nil {
		return framework.AsStatus(fmt.Errorf("calculating preScoreState: %w", err))
	}

	// return Skip if incoming pod doesn't have soft topology spread Constraints.
	if len(state.Constraints) == 0 {
		return framework.NewStatus(framework.Skip)
	}

	// Ignore parsing errors for backwards compatibility.
	requiredNodeAffinity := nodeaffinity.GetRequiredNodeAffinity(pod)
	processAllNode := func(n int) {
		nodeInfo := allNodes[n]
		node := nodeInfo.Node()

		if !pl.enableNodeInclusionPolicyInPodTopologySpread {
			// `node` should satisfy incoming pod's NodeSelector/NodeAffinity
			if match, _ := requiredNodeAffinity.Match(node); !match {
				return
			}
		}

		// All topologyKeys need to be present in `node`
		if requireAllTopologies && !nodeLabelsMatchSpreadConstraints(node.Labels, state.Constraints) {
			return
		}

		for i, c := range state.Constraints {
			if pl.enableNodeInclusionPolicyInPodTopologySpread &&
				!c.matchNodeInclusionPolicies(pod, node, requiredNodeAffinity) {
				continue
			}

			value := node.Labels[c.TopologyKey]
			// If current topology pair is not associated with any candidate node,
			// continue to avoid unnecessary calculation.
			// Per-node counts are also skipped, as they are done during Score.
			tpCount := state.TopologyValueToPodCounts[i][value]
			if tpCount == nil {
				continue
			}
			count := 0
			if len(pod.OwnerReferences) > 0 {
				if v, ok := nodeInfo.PodsWithOwnerRef[pod.OwnerReferences[0].UID]; ok {
					count = len(v)
				}
			} else {
				count = countPodsMatchSelector(nodeInfo.Pods, c.Selector, pod.Namespace)
			}
			atomic.AddInt64(tpCount, int64(count))
		}
	}
	pl.parallelizer.Until(ctx, len(allNodes), processAllNode, pl.Name())

	cycleState.Write(preScoreStateKey, state)
	return nil
}

// Score invoked at the Score extension point.
// The "score" returned in this function is the matching number of pods on the `nodeName`,
// it is normalized later.
func (pl *PodTopologySpread) Score(ctx context.Context, cycleState *framework.CycleState, pod *v1.Pod, nodeName string) (int64, *framework.Status) {
	nodeInfo, err := pl.sharedLister.NodeInfos().Get(nodeName)
	if err != nil {
		return 0, framework.AsStatus(fmt.Errorf("getting node %q from Snapshot: %w", nodeName, err))
	}

	node := nodeInfo.Node()
	s, err := getPreScoreState(cycleState)
	if err != nil {
		return 0, framework.AsStatus(err)
	}

	// Return if the node is not qualified.
	if s.IgnoredNodes.Has(node.Name) {
		return 0, nil
	}

	// For each present <pair>, current node gets a credit of <matchSum>.
	// And we sum up <matchSum> and return it as this node's score.
	var score float64
	for i, c := range s.Constraints {
		if tpVal, ok := node.Labels[c.TopologyKey]; ok {
			var cnt int64
			if c.TopologyKey == v1.LabelHostname {
				if len(pod.OwnerReferences) > 0 {
					if v, ok := nodeInfo.PodsWithOwnerRef[pod.OwnerReferences[0].UID]; ok {
						cnt = int64(len(v))
					}
				} else {
					cnt = int64(countPodsMatchSelector(nodeInfo.Pods, c.Selector, pod.Namespace))
				}
			} else {
				cnt = *s.TopologyValueToPodCounts[i][tpVal]
			}
			score += scoreForCount(cnt, c.MaxSkew, s.TopologyNormalizingWeight[i])
		}
	}
	return int64(math.Round(score)), nil
}

// NormalizeScore invoked after scoring all nodes.
func (pl *PodTopologySpread) NormalizeScore(ctx context.Context, cycleState *framework.CycleState, pod *v1.Pod, scores framework.NodeScoreList) *framework.Status {
	s, err := getPreScoreState(cycleState)
	if err != nil {
		return framework.AsStatus(err)
	}
	if s == nil {
		return nil
	}

	// Calculate <minScore> and <maxScore>
	var minScore int64 = math.MaxInt64
	var maxScore int64
	for i, score := range scores {
		// it's mandatory to check if <score.Name> is present in m.IgnoredNodes
		if s.IgnoredNodes.Has(score.Name) {
			scores[i].Score = invalidScore
			continue
		}
		if score.Score < minScore {
			minScore = score.Score
		}
		if score.Score > maxScore {
			maxScore = score.Score
		}
	}

	for i := range scores {
		if scores[i].Score == invalidScore {
			scores[i].Score = 0
			continue
		}
		if maxScore == 0 {
			scores[i].Score = framework.MaxNodeScore
			continue
		}
		s := scores[i].Score
		scores[i].Score = framework.MaxNodeScore * (maxScore + minScore - s) / maxScore
	}
	return nil
}

// ScoreExtensions of the Score plugin.
func (pl *PodTopologySpread) ScoreExtensions() framework.ScoreExtensions {
	return pl
}

func getPreScoreState(cycleState *framework.CycleState) (*preScoreState, error) {
	c, err := cycleState.Read(preScoreStateKey)
	if err != nil {
		return nil, fmt.Errorf("error reading %q from cycleState: %w", preScoreStateKey, err)
	}

	s, ok := c.(*preScoreState)
	if !ok {
		return nil, fmt.Errorf("%+v  convert to podtopologyspread.preScoreState error", c)
	}
	return s, nil
}

// topologyNormalizingWeight calculates the weight for the topology, based on
// the number of values that exist for a topology.
// Since <size> is at least 1 (all nodes that passed the Filters are in the
// same topology), and k8s supports 5k nodes, the result is in the interval
// <1.09, 8.52>.
//
// Note: <size> could also be zero when no nodes have the required topologies,
// however we don't care about topology weight in this case as we return a 0
// score for all nodes.
func topologyNormalizingWeight(size int) float64 {
	return math.Log(float64(size + 2))
}

// scoreForCount calculates the score based on number of matching pods in a
// topology domain, the constraint's maxSkew and the topology weight.
// `maxSkew-1` is added to the score so that differences between topology
// domains get watered down, controlling the tolerance of the score to skews.
func scoreForCount(cnt int64, maxSkew int32, tpWeight float64) float64 {
	return float64(cnt)*tpWeight + float64(maxSkew-1)
}
