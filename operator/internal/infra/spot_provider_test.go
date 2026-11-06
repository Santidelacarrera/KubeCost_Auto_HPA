package infra

import (
	"context"
	"testing"
	"time"
)

func TestCloudSpotAdaptersAndRiskAssessment(t *testing.T) {
	ctx := context.Background()
	termTime := time.Now().Add(2 * time.Minute)

	aws := &AWSSpotAdapter{
		SimulatedSignals: map[string]SpotSignal{
			"node-spot-rebalance": {
				Provider:         ProviderAWS,
				NodeName:         "node-spot-rebalance",
				InstanceType:     "c6i.2xlarge",
				Region:           "us-east-1",
				AvailabilityZone: "us-east-1a",
				Signal:           SignalRebalanceRecommendation,
				ObservedAt:       time.Now(),
				Source:           "aws-eventbridge",
			},
			"node-spot-itn": {
				Provider:         ProviderAWS,
				NodeName:         "node-spot-itn",
				InstanceType:     "m6i.xlarge",
				Region:           "us-east-1",
				AvailabilityZone: "us-east-1b",
				Signal:           SignalInterruptionNotice,
				TerminateAt:      &termTime,
				ObservedAt:       time.Now(),
				Source:           "aws-imds-v2",
			},
		},
	}

	// 1. Rebalance Recommendation triggers PROACTIVE_CORDON_AND_DRAIN before 2m ITN
	sigReb, err := aws.FetchNodeSignal(ctx, "node-spot-rebalance", "c6i.2xlarge", "us-east-1", "us-east-1a")
	if err != nil {
		t.Fatal(err)
	}
	resReb := EvaluateSpotRisk(SpotRiskInput{
		Signal:                     sigReb,
		HistoricalInterruptionRate: 0.15,
		PoolCapacityTightness:      0.60,
		ReplacementAvailable:       true,
	})
	if resReb.RecommendedAction != "PROACTIVE_CORDON_AND_DRAIN" || resReb.RiskScore < 0.80 {
		t.Fatalf("expected PROACTIVE_CORDON_AND_DRAIN with score >= 0.80, got %+v", resReb)
	}

	// 2. Interruption Notice triggers EMERGENCY_DRAIN with score 1.0
	sigITN, err := aws.FetchNodeSignal(ctx, "node-spot-itn", "m6i.xlarge", "us-east-1", "us-east-1b")
	if err != nil {
		t.Fatal(err)
	}
	resITN := EvaluateSpotRisk(SpotRiskInput{
		Signal:               sigITN,
		ReplacementAvailable: true,
	})
	if resITN.RecommendedAction != "EMERGENCY_DRAIN" || resITN.RiskScore != 1.0 || !resITN.IsDeterministicNotice {
		t.Fatalf("expected EMERGENCY_DRAIN with score 1.0, got %+v", resITN)
	}

	// 3. GCP and Azure adapters default to SignalNone when healthy
	gcp := &GCPSpotAdapter{}
	sigGCP, err := gcp.FetchNodeSignal(ctx, "gke-spot-1", "n2-standard-4", "us-central1", "us-central1-a")
	if err != nil {
		t.Fatal(err)
	}
	resLow := EvaluateSpotRisk(SpotRiskInput{
		Signal:                     sigGCP,
		HistoricalInterruptionRate: 0.05,
		PoolCapacityTightness:      0.10,
		ReplacementAvailable:       true,
	})
	if resLow.RiskTier != "LOW" || resLow.RecommendedAction != "HOLD" || resLow.IsDeterministicNotice {
		t.Fatalf("expected LOW risk HOLD for healthy GCP Spot node, got %+v", resLow)
	}
}
