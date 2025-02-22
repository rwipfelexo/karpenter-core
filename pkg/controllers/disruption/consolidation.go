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
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/samber/lo"
	v1 "k8s.io/api/core/v1"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"sigs.k8s.io/karpenter/pkg/apis/v1alpha5"
	"sigs.k8s.io/karpenter/pkg/apis/v1beta1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	disruptionevents "sigs.k8s.io/karpenter/pkg/controllers/disruption/events"
	"sigs.k8s.io/karpenter/pkg/controllers/disruption/orchestration"
	"sigs.k8s.io/karpenter/pkg/controllers/provisioning"
	pscheduling "sigs.k8s.io/karpenter/pkg/controllers/provisioning/scheduling"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/events"
	"sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/scheduling"
)

// consolidationTTL is the TTL between creating a consolidation command and validating that it still works.
const consolidationTTL = 15 * time.Second

// MinInstanceTypesForSpotToSpotConsolidation is the minimum number of instanceTypes in a NodeClaim needed to trigger spot-to-spot single-node consolidation
const MinInstanceTypesForSpotToSpotConsolidation = 15

// consolidation is the base consolidation controller that provides common functionality used across the different
// consolidation methods.
type consolidation struct {
	// Consolidation needs to be aware of the queue for validation
	queue                  *orchestration.Queue
	clock                  clock.Clock
	cluster                *state.Cluster
	kubeClient             client.Client
	provisioner            *provisioning.Provisioner
	cloudProvider          cloudprovider.CloudProvider
	recorder               events.Recorder
	lastConsolidationState time.Time
}

func MakeConsolidation(clock clock.Clock, cluster *state.Cluster, kubeClient client.Client, provisioner *provisioning.Provisioner,
	cloudProvider cloudprovider.CloudProvider, recorder events.Recorder, queue *orchestration.Queue) consolidation {
	return consolidation{
		queue:         queue,
		clock:         clock,
		cluster:       cluster,
		kubeClient:    kubeClient,
		provisioner:   provisioner,
		cloudProvider: cloudProvider,
		recorder:      recorder,
	}
}

// sortAndFilterCandidates orders candidates by the disruptionCost, removing any that we already know won't
// be viable consolidation options.
func (c *consolidation) sortAndFilterCandidates(ctx context.Context, candidates []*Candidate) ([]*Candidate, error) {
	candidates, err := filterCandidates(ctx, c.kubeClient, c.recorder, candidates)
	if err != nil {
		return nil, fmt.Errorf("filtering candidates, %w", err)
	}

	sort.Slice(candidates, func(i int, j int) bool {
		return candidates[i].disruptionCost < candidates[j].disruptionCost
	})
	return candidates, nil
}

// IsConsolidated returns true if nothing has changed since markConsolidated was called.
func (c *consolidation) IsConsolidated() bool {
	return c.lastConsolidationState.Equal(c.cluster.ConsolidationState())
}

// markConsolidated records the current state of the cluster.
func (c *consolidation) markConsolidated() {
	c.lastConsolidationState = c.cluster.ConsolidationState()
}

// ShouldDisrupt is a predicate used to filter candidates
func (c *consolidation) ShouldDisrupt(_ context.Context, cn *Candidate) bool {
	// TODO: Remove the check for do-not-consolidate at v1
	if cn.Annotations()[v1alpha5.DoNotConsolidateNodeAnnotationKey] == "true" {
		c.recorder.Publish(disruptionevents.Unconsolidatable(cn.Node, cn.NodeClaim, fmt.Sprintf("%s annotation exists", v1alpha5.DoNotConsolidateNodeAnnotationKey))...)
		return false
	}
	// If we don't have the "WhenUnderutilized" policy set, we should not do any of the consolidation methods, but
	// we should also not fire an event here to users since this can be confusing when the field on the NodePool
	// is named "consolidationPolicy"
	if cn.nodePool.Spec.Disruption.ConsolidationPolicy != v1beta1.ConsolidationPolicyWhenUnderutilized {
		return false
	}
	if cn.nodePool.Spec.Disruption.ConsolidateAfter != nil && cn.nodePool.Spec.Disruption.ConsolidateAfter.Duration == nil {
		c.recorder.Publish(disruptionevents.Unconsolidatable(cn.Node, cn.NodeClaim, fmt.Sprintf("NodePool %q has consolidation disabled", cn.nodePool.Name))...)
		return false
	}
	return true
}

// computeConsolidation computes a consolidation action to take
//
// nolint:gocyclo
func (c *consolidation) computeConsolidation(ctx context.Context, candidates ...*Candidate) (Command, error) {
	// Run scheduling simulation to compute consolidation option
	results, err := simulateScheduling(ctx, c.kubeClient, c.cluster, c.provisioner, candidates...)
	if err != nil {
		// if a candidate node is now deleting, just retry
		if errors.Is(err, errCandidateDeleting) {
			return Command{}, nil
		}
		return Command{}, err
	}

	// if not all of the pods were scheduled, we can't do anything
	if !results.AllNonPendingPodsScheduled() {
		// This method is used by multi-node consolidation as well, so we'll only report in the single node case
		if len(candidates) == 1 {
			c.recorder.Publish(disruptionevents.Unconsolidatable(candidates[0].Node, candidates[0].NodeClaim, results.NonPendingPodSchedulingErrors())...)
		}
		return Command{}, nil
	}

	// were we able to schedule all the pods on the inflight candidates?
	if len(results.NewNodeClaims) == 0 {
		return Command{
			candidates: candidates,
		}, nil
	}

	// we're not going to turn a single node into multiple candidates
	if len(results.NewNodeClaims) != 1 {
		if len(candidates) == 1 {
			c.recorder.Publish(disruptionevents.Unconsolidatable(candidates[0].Node, candidates[0].NodeClaim, fmt.Sprintf("Can't remove without creating %d candidates", len(results.NewNodeClaims)))...)
		}
		return Command{}, nil
	}

	// get the current node price based on the offering
	// fallback if we can't find the specific zonal pricing data
	candidatePrice, err := getCandidatePrices(candidates)
	if err != nil {
		return Command{}, fmt.Errorf("getting offering price from candidate node, %w", err)
	}

	allExistingAreSpot := true
	for _, cn := range candidates {
		if cn.capacityType != v1beta1.CapacityTypeSpot {
			allExistingAreSpot = false
		}
	}

	if allExistingAreSpot &&
		results.NewNodeClaims[0].Requirements.Get(v1beta1.CapacityTypeLabelKey).Has(v1beta1.CapacityTypeSpot) {
		return c.computeSpotToSpotConsolidation(ctx, candidates, results, candidatePrice)
	}

	// filterByPrice returns the instanceTypes that are lower priced than the current candidate. If we use this directly for spot-to-spot consolidation
	// we are bound to get repeated consolidations because the strategy that chooses to launch the spot instance from the list does it based on availability and price which could
	// result in selection/launch of non-lowest priced instance in the list. So, we would keep repeating this loop till we get to lowest priced instance
	// causing churns and landing onto lower available spot instance ultimately resulting in higher interruptions.
	results.NewNodeClaims[0].NodeClaimTemplate.InstanceTypeOptions = filterByPrice(results.NewNodeClaims[0].InstanceTypeOptions, results.NewNodeClaims[0].Requirements, candidatePrice)
	if len(results.NewNodeClaims[0].NodeClaimTemplate.InstanceTypeOptions) == 0 {
		if len(candidates) == 1 {
			c.recorder.Publish(disruptionevents.Unconsolidatable(candidates[0].Node, candidates[0].NodeClaim, "Can't replace with a cheaper node")...)
		}
		// no instance types remain after filtering by price
		return Command{}, nil
	}

	// We are consolidating a node from OD -> [OD,Spot] but have filtered the instance types by cost based on the
	// assumption, that the spot variant will launch. We also need to add a requirement to the node to ensure that if
	// spot capacity is insufficient we don't replace the node with a more expensive on-demand node.  Instead the launch
	// should fail and we'll just leave the node alone.
	ctReq := results.NewNodeClaims[0].Requirements.Get(v1beta1.CapacityTypeLabelKey)
	if ctReq.Has(v1beta1.CapacityTypeSpot) && ctReq.Has(v1beta1.CapacityTypeOnDemand) {
		results.NewNodeClaims[0].Requirements.Add(scheduling.NewRequirement(v1beta1.CapacityTypeLabelKey, v1.NodeSelectorOpIn, v1beta1.CapacityTypeSpot))
	}

	return Command{
		candidates:   candidates,
		replacements: results.NewNodeClaims,
	}, nil
}

// Compute command to execute spot-to-spot consolidation if:
//  1. The SpotToSpotConsolidation feature flag is set to true.
//  2. For single-node consolidation:
//     a. There are at least 15 cheapest instance type replacement options to consolidate.
//     b. The current candidate is NOT part of the first 15 cheapest instance types inorder to avoid repeated consolidation.
func (c *consolidation) computeSpotToSpotConsolidation(ctx context.Context, candidates []*Candidate, results *pscheduling.Results,
	candidatePrice float64) (Command, error) {

	// Spot consolidation is turned off.
	if !options.FromContext(ctx).FeatureGates.SpotToSpotConsolidation {
		if len(candidates) == 1 {
			c.recorder.Publish(disruptionevents.Unconsolidatable(candidates[0].Node, candidates[0].NodeClaim, "SpotToSpotConsolidation is disabled, can't replace a spot node with a spot node")...)
		}
		return Command{}, nil
	}

	// Since we are sure that the replacement nodeclaim considered for the spot candidates are spot, we will enforce it through the requirements.
	results.NewNodeClaims[0].Requirements.Add(scheduling.NewRequirement(v1beta1.CapacityTypeLabelKey, v1.NodeSelectorOpIn, v1beta1.CapacityTypeSpot))
	// All possible replacements for the current candidate compatible with spot offerings
	instanceTypeOptionsWithSpotOfferings :=
		results.NewNodeClaims[0].NodeClaimTemplate.InstanceTypeOptions.Compatible(results.NewNodeClaims[0].Requirements)

	// Possible replacements that are lower priced than the current candidate
	results.NewNodeClaims[0].NodeClaimTemplate.InstanceTypeOptions = filterByPrice(instanceTypeOptionsWithSpotOfferings, results.NewNodeClaims[0].Requirements, candidatePrice)

	if len(results.NewNodeClaims[0].NodeClaimTemplate.InstanceTypeOptions) == 0 {
		if len(candidates) == 1 {
			c.recorder.Publish(disruptionevents.Unconsolidatable(candidates[0].Node, candidates[0].NodeClaim, "Can't replace spot node with a cheaper spot node")...)
		}
		// no instance types remain after filtering by price
		return Command{}, nil
	}

	// For multi-node consolidation:
	// We don't have any requirement to check the remaining instance type flexibility, so exit early in this case.
	if len(candidates) > 1 {
		return Command{
			candidates:   candidates,
			replacements: results.NewNodeClaims,
		}, nil
	}

	// For single-node consolidation:
	// We check whether we have 15 cheaper instances than the current candidate instance. If this is the case, we know the following things:
	//   1) The current candidate is not in the set of the 15 cheapest instance types and
	//   2) There were at least 15 options cheaper than the current candidate.
	if len(results.NewNodeClaims[0].NodeClaimTemplate.InstanceTypeOptions) < MinInstanceTypesForSpotToSpotConsolidation {
		c.recorder.Publish(disruptionevents.Unconsolidatable(candidates[0].Node, candidates[0].NodeClaim, fmt.Sprintf("SpotToSpotConsolidation requires %d cheaper instance type options than the current candidate to consolidate, got %d",
			MinInstanceTypesForSpotToSpotConsolidation, len(results.NewNodeClaims[0].NodeClaimTemplate.InstanceTypeOptions)))...)
		return Command{}, nil
	}

	// Restrict the InstanceTypeOptions for launch to 15 so we don't get into a continual consolidation situation.
	// For example:
	// 1) Suppose we have 5 instance types, (A, B, C, D, E) in order of price with the minimum flexibility 3 and they’ll all work for our pod.  We send CreateInstanceFromTypes(A,B,C,D,E) and it gives us a E type based on price and availability of spot.
	// 2) We check if E is part of (A,B,C) and it isn't, so we will immediately have consolidation send a CreateInstanceFromTypes(A,B,C,D), since they’re cheaper than E.
	// 3) Assuming CreateInstanceFromTypes(A,B,C,D) returned D, we check if D is part of (A,B,C) and it isn't, so will have another consolidation send a CreateInstanceFromTypes(A,B,C), since they’re cheaper than D resulting in continual consolidation.
	// If we had restricted instance types to min flexibility at launch at step (1) i.e CreateInstanceFromTypes(A,B,C), we would have received the instance type part of the list preventing immediate consolidation.
	// Taking this to 15 types, we need to only send the 15 cheapest types in the CreateInstanceFromTypes call so that the resulting instance is always in that set of 15 and we won’t immediately consolidate.
	results.NewNodeClaims[0].NodeClaimTemplate.InstanceTypeOptions = lo.Slice(results.NewNodeClaims[0].NodeClaimTemplate.InstanceTypeOptions, 0, MinInstanceTypesForSpotToSpotConsolidation)

	return Command{
		candidates:   candidates,
		replacements: results.NewNodeClaims,
	}, nil
}

// getCandidatePrices returns the sum of the prices of the given candidates
func getCandidatePrices(candidates []*Candidate) (float64, error) {
	var price float64
	for _, c := range candidates {
		offering, ok := c.instanceType.Offerings.Get(c.capacityType, c.zone)
		if !ok {
			return 0.0, fmt.Errorf("unable to determine offering for %s/%s/%s", c.instanceType.Name, c.capacityType, c.zone)
		}
		price += offering.Price
	}
	return price, nil
}
