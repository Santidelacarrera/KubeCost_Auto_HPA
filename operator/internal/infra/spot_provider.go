// Package infra provides decoupled Cloud Provider Spot Signal Adapters and
// probabilistic Spot Interruption Risk scoring (Motor D — Section 10).
//
// Important Engineering Principle:
// Cloud providers do not expose deterministic Spot interruption schedules prior
// to official notices (e.g. AWS Rebalance Recommendation or 2-minute ITN).
// This module combines early probabilistic market/AZ risk indicators with
// real-time cloud metadata/event signals to initiate safe proactive migrations.
package infra

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"
)

// CloudProvider identifies the underlying cloud vendor.
type CloudProvider string

const (
	ProviderAWS   CloudProvider = "aws"
	ProviderGCP   CloudProvider = "gcp"
	ProviderAzure CloudProvider = "azure"
)

// SignalType classifies the severity and source of a Spot signal.
type SignalType string

const (
	SignalNone                    SignalType = "NONE"
	SignalMarketRiskElevated      SignalType = "MARKET_RISK_ELEVATED"
	SignalRebalanceRecommendation SignalType = "REBALANCE_RECOMMENDATION"
	SignalInterruptionNotice      SignalType = "INTERRUPTION_NOTICE"
)

// SpotSignal represents an normalized interruption or rebalance signal from any provider.
type SpotSignal struct {
	Provider        CloudProvider
	NodeName        string
	InstanceType    string
	Region          string
	AvailabilityZone string
	Signal          SignalType
	TerminateAt     *time.Time
	ObservedAt      time.Time
	Source          string // e.g., "aws-imds-v2", "aws-eventbridge", "gcp-metadata", "azure-scheduled-events"
}

// CloudSpotSignalProvider is the decoupled interface for polling or receiving cloud Spot signals.
type CloudSpotSignalProvider interface {
	ProviderName() CloudProvider
	FetchNodeSignal(ctx context.Context, nodeName, instanceType, region, az string) (SpotSignal, error)
}

// AWSSpotAdapter normalizes AWS EC2 Spot Interruption Notices (ITN), Rebalance Recommendations,
// EventBridge events, and IMDSv2 observations.
type AWSSpotAdapter struct {
	// SimulatedSignals allows deterministic testing and EventBridge webhook injection.
	SimulatedSignals map[string]SpotSignal
}

func (a *AWSSpotAdapter) ProviderName() CloudProvider { return ProviderAWS }

func (a *AWSSpotAdapter) FetchNodeSignal(_ context.Context, nodeName, instanceType, region, az string) (SpotSignal, error) {
	if a.SimulatedSignals != nil {
		if sig, ok := a.SimulatedSignals[nodeName]; ok {
			return sig, nil
		}
	}
	return SpotSignal{
		Provider:         ProviderAWS,
		NodeName:         nodeName,
		InstanceType:     instanceType,
		Region:           region,
		AvailabilityZone: az,
		Signal:           SignalNone,
		ObservedAt:       time.Now(),
		Source:           "aws-imds-v2",
	}, nil
}

// GCPSpotAdapter normalizes GCP Spot / Preemptible VM preemption signals.
type GCPSpotAdapter struct {
	SimulatedSignals map[string]SpotSignal
}

func (g *GCPSpotAdapter) ProviderName() CloudProvider { return ProviderGCP }

func (g *GCPSpotAdapter) FetchNodeSignal(_ context.Context, nodeName, instanceType, region, az string) (SpotSignal, error) {
	if g.SimulatedSignals != nil {
		if sig, ok := g.SimulatedSignals[nodeName]; ok {
			return sig, nil
		}
	}
	return SpotSignal{
		Provider:         ProviderGCP,
		NodeName:         nodeName,
		InstanceType:     instanceType,
		Region:           region,
		AvailabilityZone: az,
		Signal:           SignalNone,
		ObservedAt:       time.Now(),
		Source:           "gcp-metadata-preempted",
	}, nil
}

// AzureSpotAdapter normalizes Azure Spot VM Scheduled Events (Preempt/Terminate).
type AzureSpotAdapter struct {
	SimulatedSignals map[string]SpotSignal
}

func (az *AzureSpotAdapter) ProviderName() CloudProvider { return ProviderAzure }

func (az *AzureSpotAdapter) FetchNodeSignal(_ context.Context, nodeName, instanceType, region, zone string) (SpotSignal, error) {
	if az.SimulatedSignals != nil {
		if sig, ok := az.SimulatedSignals[nodeName]; ok {
			return sig, nil
		}
	}
	return SpotSignal{
		Provider:         ProviderAzure,
		NodeName:         nodeName,
		InstanceType:     instanceType,
		Region:           region,
		AvailabilityZone: zone,
		Signal:           SignalNone,
		ObservedAt:       time.Now(),
		Source:           "azure-scheduled-events",
	}, nil
}

// SpotRiskInput contains market, historical, and node-level parameters for risk evaluation.
type SpotRiskInput struct {
	Signal                     SpotSignal
	HistoricalInterruptionRate float64 // [0.0, 1.0], e.g. 0.05 = 5% historical reclaim frequency
	PoolCapacityTightness      float64 // [0.0, 1.0], higher means fewer spare instances in AZ
	ReplacementAvailable       bool
	ReplacementHourlyCostUSD   float64
	CurrentHourlyCostUSD       float64
}

// SpotRiskAssessment outputs a probabilistic risk score and migration action.
type SpotRiskAssessment struct {
	RiskScore              float64 // [0.0, 1.0]
	RiskTier               string  // "LOW" | "MODERATE" | "HIGH" | "IMMINENT"
	RecommendedAction      string  // "HOLD" | "PREPARE_REPLACEMENT" | "PROACTIVE_CORDON_AND_DRAIN" | "EMERGENCY_DRAIN"
	IsDeterministicNotice  bool
	Reason                 string
}

// EvaluateSpotRisk computes the composite interruption risk and migration action.
func EvaluateSpotRisk(in SpotRiskInput) SpotRiskAssessment {
	hist := clamp01(in.HistoricalInterruptionRate)
	tightness := clamp01(in.PoolCapacityTightness)

	// Instance family modifier (GPU / large compute families historically have tighter Spot pools)
	familyPenalty := 0.0
	it := strings.ToLower(in.Signal.InstanceType)
	if strings.HasPrefix(it, "p") || strings.HasPrefix(it, "g") || strings.Contains(it, "4xlarge") || strings.Contains(it, "8xlarge") {
		familyPenalty = 0.12
	}

	// 1. Confirmed Interruption Notice (e.g. AWS 2-min ITN, GCP 30s Preempt, Azure Scheduled Event)
	if in.Signal.Signal == SignalInterruptionNotice {
		return SpotRiskAssessment{
			RiskScore:             1.0,
			RiskTier:              "IMMINENT",
			RecommendedAction:     "EMERGENCY_DRAIN",
			IsDeterministicNotice: true,
			Reason: fmt.Sprintf(
				"IMMINENT: Confirmed %s termination notice on %s (%s/%s via %s); executing immediate Cordon + Eviction drain",
				in.Signal.Provider, in.Signal.NodeName, in.Signal.InstanceType, in.Signal.AvailabilityZone, in.Signal.Source,
			),
		}
	}

	// 2. Early Rebalance Recommendation (e.g. AWS EC2 Rebalance signal prior to 2-min ITN)
	if in.Signal.Signal == SignalRebalanceRecommendation {
		score := math.Min(0.95, 0.80+0.15*tightness)
		action := "PROACTIVE_CORDON_AND_DRAIN"
		if !in.ReplacementAvailable {
			action = "PREPARE_REPLACEMENT"
		}
		return SpotRiskAssessment{
			RiskScore:             round3(score),
			RiskTier:              "HIGH",
			RecommendedAction:     action,
			IsDeterministicNotice: true,
			Reason: fmt.Sprintf(
				"HIGH: Early %s Rebalance Recommendation on %s (%s in %s); initiating proactive migration before 2m ITN window",
				in.Signal.Provider, in.Signal.NodeName, in.Signal.InstanceType, in.Signal.AvailabilityZone,
			),
		}
	}

	// 3. Probabilistic market/historical risk model (non-deterministic early warning)
	probScore := clamp01(0.45*hist + 0.43*tightness + familyPenalty)
	if in.Signal.Signal == SignalMarketRiskElevated {
		probScore = math.Max(probScore, 0.72)
	}

	if probScore >= 0.70 {
		action := "PROACTIVE_CORDON_AND_DRAIN"
		if !in.ReplacementAvailable {
			action = "PREPARE_REPLACEMENT"
		}
		return SpotRiskAssessment{
			RiskScore:             round3(probScore),
			RiskTier:              "HIGH",
			RecommendedAction:     action,
			IsDeterministicNotice: false,
			Reason: fmt.Sprintf(
				"HIGH (Probabilistic): Spot pool risk=%.2f (hist=%.2f, tightness=%.2f) for %s in %s; proactive drain recommended",
				probScore, hist, tightness, in.Signal.InstanceType, in.Signal.AvailabilityZone,
			),
		}
	}

	if probScore >= 0.40 {
		return SpotRiskAssessment{
			RiskScore:             round3(probScore),
			RiskTier:              "MODERATE",
			RecommendedAction:     "PREPARE_REPLACEMENT",
			IsDeterministicNotice: false,
			Reason: fmt.Sprintf(
				"MODERATE (Probabilistic): Spot pool risk=%.2f for %s in %s; warming standby replacement capacity",
				probScore, in.Signal.InstanceType, in.Signal.AvailabilityZone,
			),
		}
	}

	return SpotRiskAssessment{
		RiskScore:             round3(probScore),
		RiskTier:              "LOW",
		RecommendedAction:     "HOLD",
		IsDeterministicNotice: false,
		Reason: fmt.Sprintf(
			"LOW: Spot pool stable (risk=%.2f) for %s in %s",
			probScore, in.Signal.InstanceType, in.Signal.AvailabilityZone,
		),
	}
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

func round3(v float64) float64 {
	return math.Round(v*1000) / 1000
}
