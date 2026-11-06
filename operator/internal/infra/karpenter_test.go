package infra

import (
	"testing"
)

func TestRecommendKarpenterNodePoolAdjustment(t *testing.T) {
	sig := SpotSignal{
		Provider:         ProviderAWS,
		NodeName:         "node-1",
		InstanceType:     "c6i.2xlarge",
		Region:           "us-east-1",
		AvailabilityZone: "us-east-1a",
		Signal:           SignalInterruptionNotice,
	}
	risk := EvaluateSpotRisk(SpotRiskInput{
		Signal:               sig,
		ReplacementAvailable: true,
	})
	rec := RecommendKarpenterNodePoolAdjustment("spot-compute-pool", 80, risk, sig)
	if rec.RecommendedWeight >= 80 || !rec.FallbackToOnDemand {
		t.Fatalf("expected reduced weight and FallbackToOnDemand on IMMINENT risk, got %+v", rec)
	}
	if len(rec.ExcludedInstanceTypes) != 1 || rec.ExcludedInstanceTypes[0] != "c6i.2xlarge" {
		t.Fatalf("expected c6i.2xlarge excluded, got %+v", rec.ExcludedInstanceTypes)
	}
}
