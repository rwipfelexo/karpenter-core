/*
Copyright The Kubernetes Authors.

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

package disruption

import (
	"context"
	"fmt"

	"github.com/samber/lo"
	"k8s.io/utils/clock"

	disruptionevents "sigs.k8s.io/karpenter/pkg/controllers/disruption/events"
	"sigs.k8s.io/karpenter/pkg/events"

	"sigs.k8s.io/karpenter/pkg/apis/v1beta1"
	"sigs.k8s.io/karpenter/pkg/metrics"
)

// Emptiness is a subreconciler that deletes empty candidates.
// Emptiness will respect TTLSecondsAfterEmpty
type Emptiness struct {
	clock    clock.Clock
	recorder events.Recorder
}

func NewEmptiness(clk clock.Clock, recorder events.Recorder) *Emptiness {
	return &Emptiness{
		clock:    clk,
		recorder: recorder,
	}
}

// ShouldDisrupt is a predicate used to filter candidates
func (e *Emptiness) ShouldDisrupt(_ context.Context, c *Candidate) bool {
	// If we don't have the "WhenEmpty" policy set, we should not do this method, but
	// we should also not fire an event here to users since this can be confusing when the field on the NodePool
	// is named "consolidationPolicy"
	if c.nodePool.Spec.Disruption.ConsolidationPolicy != v1beta1.ConsolidationPolicyWhenEmpty {
		return false
	}
	if c.nodePool.Spec.Disruption.ConsolidateAfter != nil && c.nodePool.Spec.Disruption.ConsolidateAfter.Duration == nil {
		e.recorder.Publish(disruptionevents.Unconsolidatable(c.Node, c.NodeClaim, fmt.Sprintf("NodePool %q has consolidation disabled", c.nodePool.Name))...)
		return false
	}
	return c.NodeClaim.StatusConditions().GetCondition(v1beta1.Empty).IsTrue() &&
		!e.clock.Now().Before(c.NodeClaim.StatusConditions().GetCondition(v1beta1.Empty).LastTransitionTime.Inner.Add(*c.nodePool.Spec.Disruption.ConsolidateAfter.Duration))
}

// ComputeCommand generates a disruption command given candidates
func (e *Emptiness) ComputeCommand(_ context.Context, disruptionBudgetMapping map[string]int, candidates ...*Candidate) (Command, error) {
	// First check how many nodes are empty so that we can emit a metric on how many nodes are eligible
	emptyCandidates := lo.Filter(candidates, func(cn *Candidate, _ int) bool {
		return cn.NodeClaim.DeletionTimestamp.IsZero() && len(cn.pods) == 0
	})

	disruptionEligibleNodesGauge.With(map[string]string{
		methodLabel:            e.Type(),
		consolidationTypeLabel: e.ConsolidationType(),
	}).Set(float64(len(candidates)))

	empty := make([]*Candidate, 0, len(emptyCandidates))
	for _, candidate := range emptyCandidates {
		if len(candidate.pods) > 0 {
			continue
		}
		// If there's disruptions allowed for the candidate's nodepool,
		// add it to the list of candidates, and decrement the budget.
		if disruptionBudgetMapping[candidate.nodePool.Name] > 0 {
			empty = append(empty, candidate)
			disruptionBudgetMapping[candidate.nodePool.Name]--
		}
	}

	return Command{
		candidates: empty,
	}, nil
}

func (e *Emptiness) Type() string {
	return metrics.EmptinessReason
}

func (e *Emptiness) ConsolidationType() string {
	return ""
}
