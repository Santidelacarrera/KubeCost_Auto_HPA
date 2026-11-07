package castsuite

import "testing"

func TestEvaluateCommitmentArbitrage(t *testing.T) {
	c := CommitmentContract{
		ID:                 "sp-aws-us-east-1-001",
		Provider:           "aws",
		CommitmentType:     "SAVINGS_PLAN",
		InstanceFamily:     "m6i",
		Region:             "us-east-1",
		CommittedCores:     64,
		UsedCores:          44,
		HourlyCommittedUSD: 1.92,
	}

	dec := EvaluateCommitmentArbitrage(c, 16, 0.015)
	if dec.Action != "ABSORB_INTO_PREPAID_COMMITMENT" {
		t.Fatalf("expected ABSORB_INTO_PREPAID_COMMITMENT, got %s", dec.Action)
	}
	if dec.AllocatedFromCommitment != 16 || dec.RemainingSpotCores != 0 {
		t.Fatalf("expected 16 cores absorbed and 0 spot cores, got %d and %d", dec.AllocatedFromCommitment, dec.RemainingSpotCores)
	}
	if dec.EffectiveIncrementalUSD != 0 {
		t.Fatalf("expected $0 incremental cost for pre-paid commitment cores, got %f", dec.EffectiveIncrementalUSD)
	}
	if dec.MonthlyWasteRecovered <= 300 {
		t.Fatalf("expected >$300/mo recovered commitment value, got %.2f", dec.MonthlyWasteRecovered)
	}
}

func TestEvaluateHibernation_StagingNightSleep(t *testing.T) {
	s := HibernationSchedule{
		ClusterID:         "eks-staging-01",
		Environment:       "staging",
		SleepStartHourUTC: 20,
		WakeHourUTC:       8,
		WeekendFullSleep:  true,
		ActiveNodes:       10,
		SkeletonNodes:     1,
		NodeHourlyCostUSD: 0.384,
	}

	dec := EvaluateHibernation(s, 22, false)
	if dec.Action != "HIBERNATE_NOW" {
		t.Fatalf("expected HIBERNATE_NOW at 22:00 UTC, got %s", dec.Action)
	}
	if dec.TargetNodeCount != 1 {
		t.Fatalf("expected target node count 1, got %d", dec.TargetNodeCount)
	}
	if dec.SavingsPercentage < 55.0 {
		t.Fatalf("expected >=55%% savings from 108h/week hibernation, got %.2f%%", dec.SavingsPercentage)
	}
}
