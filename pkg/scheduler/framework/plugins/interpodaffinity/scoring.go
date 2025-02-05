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

package interpodaffinity

import (
	"context"
	"fmt"
	"math"
	"sync/atomic"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/framework"
)

// preScoreStateKey is the key in CycleState to InterPodAffinity pre-computed data for Scoring.
const preScoreStateKey = "PreScore" + Name

type scoreMap map[string]map[string]int64

// scoreList is a flattened version of scoreMap.
// It could have multiple entries with the same topologyKeys and topologyValues
// and merging into the scoreMap is done at the end of PreScore.
type scoreList []score

type score struct {
	topologyKey   string
	topologyValue string
	weight        int64
}

// preScoreState computed at PreScore and used at Score.
type preScoreState struct {
	topologyScore scoreMap
	podInfo       *framework.PodInfo
	// A copy of the incoming pod's namespace labels.
	namespaceLabels labels.Set
}

// Clone implements the mandatory Clone interface. We don't really copy the data since
// there is no need for that.
func (s *preScoreState) Clone() framework.StateData {
	return s
}

func (m scoreList) processTerm(term *framework.AffinityTerm, weight int32, pod *v1.Pod, nsLabels labels.Set, node *v1.Node, multiplier int32) {
	if term.Matches(pod, nsLabels) {
		if tpValue, tpValueExist := node.Labels[term.TopologyKey]; tpValueExist {
			m = append(m, score{
				topologyKey:   term.TopologyKey,
				topologyValue: tpValue,
				weight:        int64(weight * multiplier),
			})
		}
	}
}

func (m scoreList) processTerms(terms []framework.WeightedAffinityTerm, pod *v1.Pod, nsLabels labels.Set, node *v1.Node, multiplier int32) {
	for _, term := range terms {
		m.processTerm(&term.AffinityTerm, term.Weight, pod, nsLabels, node, multiplier)
	}
}

func (m scoreMap) append(scores scoreList) {
	for _, score := range scores {
		if m[score.topologyKey] == nil {
			m[score.topologyKey] = make(map[string]int64)
		}
		m[score.topologyKey][score.topologyValue] += score.weight
	}
}

func (pl *InterPodAffinity) processExistingPod(
	state *preScoreState,
	existingPod *framework.PodInfo,
	existingPodNodeInfo *framework.NodeInfo,
	incomingPod *v1.Pod,
	topoScore scoreList,
) {
	existingPodNode := existingPodNodeInfo.Node()
	if len(existingPodNode.Labels) == 0 {
		return
	}

	// For every soft pod affinity term of <pod>, if <existingPod> matches the term,
	// increment <p.counts> for every node in the cluster with the same <term.TopologyKey>
	// value as that of <existingPods>`s node by the term`s weight.
	// Note that the incoming pod's terms have the namespaceSelector merged into the namespaces, and so
	// here we don't lookup the existing pod's namespace labels, hence passing nil for nsLabels.
	topoScore.processTerms(state.podInfo.PreferredAffinityTerms, existingPod.Pod, nil, existingPodNode, 1)

	// For every soft pod anti-affinity term of <pod>, if <existingPod> matches the term,
	// decrement <p.counts> for every node in the cluster with the same <term.TopologyKey>
	// value as that of <existingPod>`s node by the term`s weight.
	// Note that the incoming pod's terms have the namespaceSelector merged into the namespaces, and so
	// here we don't lookup the existing pod's namespace labels, hence passing nil for nsLabels.
	topoScore.processTerms(state.podInfo.PreferredAntiAffinityTerms, existingPod.Pod, nil, existingPodNode, -1)

	// For every hard pod affinity term of <existingPod>, if <pod> matches the term,
	// increment <p.counts> for every node in the cluster with the same <term.TopologyKey>
	// value as that of <existingPod>'s node by the constant <args.hardPodAffinityWeight>
	if pl.args.HardPodAffinityWeight > 0 && len(existingPodNode.Labels) != 0 {
		for _, t := range existingPod.RequiredAffinityTerms {
			topoScore.processTerm(&t, pl.args.HardPodAffinityWeight, incomingPod, state.namespaceLabels, existingPodNode, 1)
		}
	}

	// For every soft pod affinity term of <existingPod>, if <pod> matches the term,
	// increment <p.counts> for every node in the cluster with the same <term.TopologyKey>
	// value as that of <existingPod>'s node by the term's weight.
	topoScore.processTerms(existingPod.PreferredAffinityTerms, incomingPod, state.namespaceLabels, existingPodNode, 1)

	// For every soft pod anti-affinity term of <existingPod>, if <pod> matches the term,
	// decrement <pm.counts> for every node in the cluster with the same <term.TopologyKey>
	// value as that of <existingPod>'s node by the term's weight.
	topoScore.processTerms(existingPod.PreferredAntiAffinityTerms, incomingPod, state.namespaceLabels, existingPodNode, -1)
}

// PreScore builds and writes cycle state used by Score and NormalizeScore.
func (pl *InterPodAffinity) PreScore(
	pCtx context.Context,
	cycleState *framework.CycleState,
	pod *v1.Pod,
	nodes []*framework.NodeInfo,
) *framework.Status {
	if len(nodes) == 0 {
		// No nodes to score.
		return framework.NewStatus(framework.Skip)
	}

	if pl.sharedLister == nil {
		return framework.NewStatus(framework.Error, "empty shared lister in InterPodAffinity PreScore")
	}

	affinity := pod.Spec.Affinity
	hasPreferredAffinityConstraints := affinity != nil && affinity.PodAffinity != nil && len(affinity.PodAffinity.PreferredDuringSchedulingIgnoredDuringExecution) > 0
	hasPreferredAntiAffinityConstraints := affinity != nil && affinity.PodAntiAffinity != nil && len(affinity.PodAntiAffinity.PreferredDuringSchedulingIgnoredDuringExecution) > 0
	hasConstraints := hasPreferredAffinityConstraints || hasPreferredAntiAffinityConstraints

	// Optionally ignore calculating preferences of existing pods' affinity rules
	// if the incoming pod has no inter-pod affinities.
	if pl.args.IgnorePreferredTermsOfExistingPods && !hasConstraints {
		return framework.NewStatus(framework.Skip)
	}

	// Unless the pod being scheduled has preferred affinity terms, we only
	// need to process nodes hosting pods with affinity.
	var allNodes []*framework.NodeInfo
	var err error
	if hasConstraints {
		allNodes, err = pl.sharedLister.NodeInfos().List()
		if err != nil {
			return framework.AsStatus(fmt.Errorf("failed to get all nodes from shared lister: %w", err))
		}
	} else {
		allNodes, err = pl.sharedLister.NodeInfos().HavePodsWithAffinityList()
		if err != nil {
			return framework.AsStatus(fmt.Errorf("failed to get pods with affinity list: %w", err))
		}
	}

	state := &preScoreState{
		topologyScore: make(map[string]map[string]int64),
	}

	if state.podInfo, err = framework.NewPodInfo(pod); err != nil {
		// Ideally we never reach here, because errors will be caught by PreFilter
		return framework.AsStatus(fmt.Errorf("failed to parse pod: %w", err))
	}

	for i := range state.podInfo.PreferredAffinityTerms {
		if err := pl.mergeAffinityTermNamespacesIfNotEmpty(&state.podInfo.PreferredAffinityTerms[i].AffinityTerm); err != nil {
			return framework.AsStatus(fmt.Errorf("updating PreferredAffinityTerms: %w", err))
		}
	}
	for i := range state.podInfo.PreferredAntiAffinityTerms {
		if err := pl.mergeAffinityTermNamespacesIfNotEmpty(&state.podInfo.PreferredAntiAffinityTerms[i].AffinityTerm); err != nil {
			return framework.AsStatus(fmt.Errorf("updating PreferredAntiAffinityTerms: %w", err))
		}
	}
	logger := klog.FromContext(pCtx)
	state.namespaceLabels = GetNamespaceLabelsSnapshot(logger, pod.Namespace, pl.nsLister)

	topoScores := make([]scoreList, len(allNodes))
	index := int32(-1)
	processNode := func(i int) {
		nodeInfo := allNodes[i]

		// Unless the pod being scheduled has preferred affinity terms, we only
		// need to process pods with affinity in the node.
		podsToProcess := nodeInfo.PodsWithAffinity
		if hasConstraints {
			// We need to process all the pods.
			podsToProcess = nodeInfo.Pods
		}

		topoScore := make(scoreList, 0)
		for _, existingPod := range podsToProcess {
			pl.processExistingPod(state, existingPod, nodeInfo, pod, topoScore)
		}
		if len(topoScore) > 0 {
			topoScores[atomic.AddInt32(&index, 1)] = topoScore
		}
	}
	pl.parallelizer.Until(pCtx, len(allNodes), processNode, pl.Name())

	if index == -1 {
		return framework.NewStatus(framework.Skip)
	}

	for i := 0; i <= int(index); i++ {
		state.topologyScore.append(topoScores[i])
	}

	cycleState.Write(preScoreStateKey, state)
	return nil
}

func getPreScoreState(cycleState *framework.CycleState) (*preScoreState, error) {
	c, err := cycleState.Read(preScoreStateKey)
	if err != nil {
		return nil, fmt.Errorf("failed to read %q from cycleState: %w", preScoreStateKey, err)
	}

	s, ok := c.(*preScoreState)
	if !ok {
		return nil, fmt.Errorf("%+v  convert to interpodaffinity.preScoreState error", c)
	}
	return s, nil
}

// Score invoked at the Score extension point.
// The "score" returned in this function is the sum of weights got from cycleState which have its topologyKey matching with the node's labels.
// it is normalized later.
// Note: the returned "score" is positive for pod-affinity, and negative for pod-antiaffinity.
func (pl *InterPodAffinity) Score(ctx context.Context, cycleState *framework.CycleState, pod *v1.Pod, nodeName string) (int64, *framework.Status) {
	nodeInfo, err := pl.sharedLister.NodeInfos().Get(nodeName)
	if err != nil {
		return 0, framework.AsStatus(fmt.Errorf("failed to get node %q from Snapshot: %w", nodeName, err))
	}
	node := nodeInfo.Node()

	s, err := getPreScoreState(cycleState)
	if err != nil {
		return 0, framework.AsStatus(err)
	}
	var score int64
	for tpKey, tpValues := range s.topologyScore {
		if v, exist := node.Labels[tpKey]; exist {
			score += tpValues[v]
		}
	}

	return score, nil
}

// NormalizeScore normalizes the score for each filteredNode.
func (pl *InterPodAffinity) NormalizeScore(ctx context.Context, cycleState *framework.CycleState, pod *v1.Pod, scores framework.NodeScoreList) *framework.Status {
	s, err := getPreScoreState(cycleState)
	if err != nil {
		return framework.AsStatus(err)
	}
	if len(s.topologyScore) == 0 {
		return nil
	}

	var minCount int64 = math.MaxInt64
	var maxCount int64 = math.MinInt64
	for i := range scores {
		score := scores[i].Score
		if score > maxCount {
			maxCount = score
		}
		if score < minCount {
			minCount = score
		}
	}

	maxMinDiff := maxCount - minCount
	for i := range scores {
		fScore := float64(0)
		if maxMinDiff > 0 {
			fScore = float64(framework.MaxNodeScore) * (float64(scores[i].Score-minCount) / float64(maxMinDiff))
		}

		scores[i].Score = int64(fScore)
	}

	return nil
}

// ScoreExtensions of the Score plugin.
func (pl *InterPodAffinity) ScoreExtensions() framework.ScoreExtensions {
	return pl
}
