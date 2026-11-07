package castsuite

import (
	"fmt"
	"math"
)

// CommitmentContract represents a pre-paid cloud commitment (RI, Savings Plan, GCP CUD).
type CommitmentContract struct {
	ID                 string
	Provider           string // aws | gcp | azure
	CommitmentType     string // SAVINGS_PLAN | RESERVED_INSTANCE | CUD
	InstanceFamily     string // m6i | c6i | n2 | Dsv5
	Region             string
	CommittedCores     int
	UsedCores          int
	HourlyCommittedUSD float64
}

// CommitmentArbitrageDecision prioritizes unallocated pre-paid commitments before Spot/On-Demand.
type CommitmentArbitrageDecision struct {
	ContractID              string
	AllocatedFromCommitment int
	RemainingSpotCores      int
	EffectiveIncrementalUSD float64
	MonthlyWasteRecovered   float64
	UtilizationAfterPct     float64
	Action                  string
	Reason                  string
}

// EvaluateCommitmentArbitrage routes requested cores onto underutilized RIs/Savings Plans first.
func EvaluateCommitmentArbitrage(c CommitmentContract, requestedCores int, spotCoreHourlyUSD float64) CommitmentArbitrageDecision {
	if requestedCores <= 0 {
		requestedCores = 1
	}
	unused := c.CommittedCores - c.UsedCores
	if unused < 0 {
		unused = 0
	}

	covered := requestedCores
	if covered > unused {
		covered = unused
	}
	remSpot := requestedCores - covered

	perCoreCommitHourly := 0.0
	if c.CommittedCores > 0 {
		perCoreCommitHourly = c.HourlyCommittedUSD / float64(c.CommittedCores)
	}

	wasteRecoveredHourly := float64(covered) * perCoreCommitHourly
	monthlyRecovered := math.Round(wasteRecoveredHourly*730.0*100) / 100
	incrementalHourly := math.Round(float64(remSpot)*spotCoreHourlyUSD*10000) / 10000

	newUsed := c.UsedCores + covered
	utilAfter := 100.0
	if c.CommittedCores > 0 {
		utilAfter = math.Min(100.0, math.Round((float64(newUsed)/float64(c.CommittedCores))*10000)/100)
	}

	action := "PROVISION_SPOT"
	if covered > 0 && remSpot == 0 {
		action = "ABSORB_INTO_PREPAID_COMMITMENT"
	} else if covered > 0 {
		action = "HYBRID_COMMITMENT_AND_SPOT"
	}

	return CommitmentArbitrageDecision{
		ContractID:              c.ID,
		AllocatedFromCommitment: covered,
		RemainingSpotCores:      remSpot,
		EffectiveIncrementalUSD: incrementalHourly,
		MonthlyWasteRecovered:   monthlyRecovered,
		UtilizationAfterPct:     utilAfter,
		Action:                  action,
		Reason: fmt.Sprintf(
			"%s: absorbed %d/%d cores into %s (%s) recovering $%.2f/mo of pre-paid commitment",
			action, covered, requestedCores, c.ID, c.CommitmentType, monthlyRecovered,
		),
	}
}

// HibernationSchedule defines off-hours sleep/wake windows for non-prod clusters.
type HibernationSchedule struct {
	ClusterID         string
	Environment       string // dev | staging | qa | prod
	SleepStartHourUTC int    // e.g. 20 (20:00 UTC)
	WakeHourUTC       int    // e.g. 8  (08:00 UTC)
	WeekendFullSleep  bool
	ActiveNodes       int
	SkeletonNodes     int
	NodeHourlyCostUSD float64
}

// HibernationDecision reports the current state and monthly savings of cluster hibernation.
type HibernationDecision struct {
	Action            string // HIBERNATE_NOW | PREWARM_WAKE | KEEP_AWAKE | PROD_PROTECTED
	TargetNodeCount   int
	SleepHoursPerWeek int
	MonthlySavingsUSD float64
	SavingsPercentage float64
	Reason            string
}

// EvaluateHibernation computes sleep/wake state and annualized/monthly non-prod savings.
func EvaluateHibernation(s HibernationSchedule, currentHourUTC int, isWeekend bool) HibernationDecision {
	if s.Environment == "prod" || s.Environment == "production" {
		return HibernationDecision{
			Action:            "PROD_PROTECTED",
			TargetNodeCount:   s.ActiveNodes,
			SleepHoursPerWeek: 0,
			MonthlySavingsUSD: 0,
			SavingsPercentage: 0,
			Reason:            "Production cluster is protected from hibernation schedules",
		}
	}

	weekdaySleepHours := 0
	if s.SleepStartHourUTC > s.WakeHourUTC {
		weekdaySleepHours = (24 - s.SleepStartHourUTC) + s.WakeHourUTC
	} else if s.WakeHourUTC > s.SleepStartHourUTC {
		weekdaySleepHours = s.WakeHourUTC - s.SleepStartHourUTC
	}
	sleepHoursWeek := weekdaySleepHours * 5
	if s.WeekendFullSleep {
		sleepHoursWeek += 48
	}

	isSleepingHour := false
	if isWeekend && s.WeekendFullSleep {
		isSleepingHour = true
	} else if s.SleepStartHourUTC > s.WakeHourUTC {
		isSleepingHour = currentHourUTC >= s.SleepStartHourUTC || currentHourUTC < s.WakeHourUTC
	} else {
		isSleepingHour = currentHourUTC >= s.SleepStartHourUTC && currentHourUTC < s.WakeHourUTC
	}

	skeleton := s.SkeletonNodes
	if skeleton < 0 {
		skeleton = 0
	}
	reclaimedNodes := s.ActiveNodes - skeleton
	if reclaimedNodes < 0 {
		reclaimedNodes = 0
	}

	sleepFraction := float64(sleepHoursWeek) / 168.0
	fullMonthlyCost := float64(s.ActiveNodes) * s.NodeHourlyCostUSD * 730.0
	monthlySavings := math.Round(float64(reclaimedNodes)*s.NodeHourlyCostUSD*730.0*sleepFraction*100) / 100
	savingsPct := 0.0
	if fullMonthlyCost > 0 {
		savingsPct = math.Round((monthlySavings/fullMonthlyCost)*10000) / 100
	}

	action := "KEEP_AWAKE"
	targetNodes := s.ActiveNodes
	if isSleepingHour {
		action = "HIBERNATE_NOW"
		targetNodes = skeleton
	} else if currentHourUTC == s.WakeHourUTC {
		action = "PREWARM_WAKE"
		targetNodes = s.ActiveNodes
	}

	return HibernationDecision{
		Action:            action,
		TargetNodeCount:   targetNodes,
		SleepHoursPerWeek: sleepHoursWeek,
		MonthlySavingsUSD: monthlySavings,
		SavingsPercentage: savingsPct,
		Reason: fmt.Sprintf(
			"%s (%s): %d->%d nodes during %dh/week off-hours saving $%.2f/mo (%.1f%%)",
			action, s.Environment, s.ActiveNodes, targetNodes, sleepHoursWeek, monthlySavings, savingsPct,
		),
	}
}
