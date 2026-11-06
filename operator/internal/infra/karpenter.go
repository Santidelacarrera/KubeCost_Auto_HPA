package infra

import (
	"fmt"
)

// KarpenterNodePoolRecommendation advises dynamic weight adjustments and instance-type
// exclusions for a Karpenter NodePool based on Spot interruption risk assessments.
type KarpenterNodePoolRecommendation struct {
	NodePoolName          string
	CurrentWeight         int32
	RecommendedWeight     int32
	FallbackToOnDemand    bool
	ExcludedInstanceTypes []string
	ExcludedZones         []string
	Reason                string
}

// RecommendKarpenterNodePoolAdjustment translates a SpotRiskAssessment into a safe
// Karpenter NodePool weight and constraint update.
func RecommendKarpenterNodePoolAdjustment(
	nodePoolName string,
	currentWeight int32,
	assessment SpotRiskAssessment,
	signal SpotSignal,
) KarpenterNodePoolRecommendation {
	if currentWeight <= 0 {
		currentWeight = 50
	}

	switch assessment.RiskTier {
	case "IMMINENT", "HIGH":
		newWeight := max32(1, currentWeight/4)
		var exclTypes []string
		if signal.InstanceType != "" {
			exclTypes = []string{signal.InstanceType}
		}
		var exclZones []string
		if signal.AvailabilityZone != "" && assessment.RiskTier == "IMMINENT" {
			exclZones = []string{signal.AvailabilityZone}
		}
		return KarpenterNodePoolRecommendation{
			NodePoolName:          nodePoolName,
			CurrentWeight:         currentWeight,
			RecommendedWeight:     newWeight,
			FallbackToOnDemand:    assessment.RiskTier == "IMMINENT",
			ExcludedInstanceTypes: exclTypes,
			ExcludedZones:         exclZones,
			Reason: fmt.Sprintf(
				"Karpenter NodePool %q weight reduced %d->%d due to %s Spot risk (%.2f) on %s in %s",
				nodePoolName, currentWeight, newWeight, assessment.RiskTier, assessment.RiskScore, signal.InstanceType, signal.AvailabilityZone,
			),
		}
	case "MODERATE":
		newWeight := max32(10, int32(float64(currentWeight)*0.70))
		return KarpenterNodePoolRecommendation{
			NodePoolName:      nodePoolName,
			CurrentWeight:     currentWeight,
			RecommendedWeight: newWeight,
			Reason: fmt.Sprintf(
				"Karpenter NodePool %q weight throttled %d->%d due to MODERATE Spot pool tightness",
				nodePoolName, currentWeight, newWeight,
			),
		}
	default:
		return KarpenterNodePoolRecommendation{
			NodePoolName:      nodePoolName,
			CurrentWeight:     currentWeight,
			RecommendedWeight: currentWeight,
			Reason:            fmt.Sprintf("Karpenter NodePool %q healthy (LOW Spot risk)", nodePoolName),
		}
	}
}

func max32(a, b int32) int32 {
	if a > b {
		return a
	}
	return b
}
