/*
Portions Copyright (c) Microsoft Corporation.

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

package regional

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/Azure/karpenter-provider-azure/pkg/apis/v1beta1"
	corev1 "k8s.io/api/core/v1"
)

// PlacementAnnotation records the region and subnet selected for a NodeClaim. It is
// persisted before any Azure write and is the source of truth for placement drift.
const PlacementAnnotation = "karpenter.azure.com/region-placement"

var errIncompletePlacement = errors.New("decoding persisted regional placement: name and vnetSubnetID are required")

// DecodePlacement parses a persisted placement annotation.
func DecodePlacement(raw string) (v1beta1.Region, error) {
	var placement v1beta1.Region
	if err := json.Unmarshal([]byte(raw), &placement); err != nil {
		return placement, fmt.Errorf("decoding persisted regional placement: %w", err)
	}
	if placement.Name == "" || placement.VNETSubnetID == "" {
		return placement, errIncompletePlacement
	}
	return placement, nil
}

// PlacementAllowed reports whether regions still lists the placement's region with the
// same subnet. ARM resource IDs are case-insensitive.
func PlacementAllowed(regions []v1beta1.Region, placement v1beta1.Region) bool {
	for i := range regions {
		if regions[i].Name == placement.Name && strings.EqualFold(regions[i].VNETSubnetID, placement.VNETSubnetID) {
			return true
		}
	}
	return false
}

// PlacementDrift explains why a NodeClaim's recorded placement no longer matches
// spec.regions, or returns "" when it still does. spec.regions is excluded from the
// NodeClass hash, so adding, removing or reordering regions replaces only NodeClaims
// whose own region or subnet is no longer listed.
func PlacementDrift(regions []v1beta1.Region, raw string) string {
	switch {
	case raw == "" && len(regions) == 0:
		return ""
	case raw == "":
		return "the NodeClass lists regions but the NodeClaim has no regional placement"
	case len(regions) == 0:
		return "the NodeClaim has a regional placement but the NodeClass no longer lists regions"
	}
	placement, err := DecodePlacement(raw)
	if err != nil {
		// An unreadable placement cannot be verified; replacement needs no manual repair.
		return err.Error()
	}
	if PlacementAllowed(regions, placement) {
		return ""
	}
	return fmt.Sprintf("region %s with subnet %s is no longer listed in spec.regions", placement.Name, placement.VNETSubnetID)
}

// ImageMatchesRegion reports whether an image's requirements admit the region. Drift
// evaluates every image for every NodeClaim, so the requirements are read in place.
func ImageMatchesRegion(requirements []corev1.NodeSelectorRequirement, region string) bool {
	for i := range requirements {
		if requirements[i].Key == corev1.LabelTopologyRegion && !admits(&requirements[i], region) {
			return false
		}
	}
	return true
}

func admits(requirement *corev1.NodeSelectorRequirement, value string) bool {
	switch requirement.Operator {
	case corev1.NodeSelectorOpIn:
		return slices.Contains(requirement.Values, value)
	case corev1.NodeSelectorOpNotIn:
		return !slices.Contains(requirement.Values, value)
	case corev1.NodeSelectorOpExists:
		return true
	case corev1.NodeSelectorOpDoesNotExist, corev1.NodeSelectorOpGt, corev1.NodeSelectorOpLt:
		return false
	}
	return false
}
