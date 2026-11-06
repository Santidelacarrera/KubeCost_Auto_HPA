// Package arbitrator implements the Horizontal and Vertical Arbitration Engine
// (Motor A — Section 8). It coordinates replica scaling vs. in-place vertical
// pod resizing while enforcing strict single-writer ownership boundaries against
// native HorizontalPodAutoscalers, VerticalPodAutoscalers, and external controllers.
package arbitrator

import (
	"fmt"
	"math"
)

// Strategy enumerates the five canonical arbitration outcomes.
type Strategy string

const (
	StrategyScaleHorizontally Strategy = "SCALE_HORIZONTALLY"
	StrategyResizeVertically  Strategy = "RESIZE_VERTICALLY"
	StrategyCoordinatedBoth   Strategy = "COORDINATED_BOTH"
	StrategyNoOp              Strategy = "NO_OP"
	StrategyWaitMoreInfo      Strategy = "WAIT_MORE_INFO"
)

// OwnershipContext captures active external controllers targeting the same workload.
type OwnershipContext struct {
	HasNativeHPA             bool
	NativeHPAName            string
	HasNativeVPA             bool
	NativeVPAName            string
	NativeVPAMode            string // "Off", "Initial", "Recreate", "Auto", "InPlaceOrRecreate"
	HasExternalController    bool
	ExternalControllerName   string
	HasConflictingAutoscaler bool
	ConflictingAutoscaler    string
}

// HorizontalBlocked returns true if another controller owns spec.replicas.
func (o OwnershipContext) HorizontalBlocked() (bool, string) {
	if o.HasNativeHPA {
		return true, fmt.Sprintf("native HPA %q owns spec.replicas", o.NativeHPAName)
	}
	if o.HasExternalController {
		return true, fmt.Sprintf("external controller %q owns spec.replicas", o.ExternalControllerName)
	}
	if o.HasConflictingAutoscaler {
		return true, fmt.Sprintf("conflicting KubeCostAutoscaler %q owns spec.replicas", o.ConflictingAutoscaler)
	}
	return false, ""
}

// VerticalBlocked returns true if an active VPA in mutating mode owns pod resources.
func (o OwnershipContext) VerticalBlocked() (bool, string) {
	if o.HasNativeVPA && o.NativeVPAMode != "" && o.NativeVPAMode != "Off" {
		return true, fmt.Sprintf("native VPA %q in mode %q owns container resources", o.NativeVPAName, o.NativeVPAMode)
	}
	return false, ""
}

// WorkloadTelemetry provides the runtime observation inputs for arbitration.
type WorkloadTelemetry struct {
	SampleCount             int
	CurrentReplicas         int32
	MinReplicas             int32
	MaxReplicas             int32
	CurrentCPUMillicores    int64
	CPUUsageMillicores      int64
	MinCPUMillicores        int64
	MaxCPUMillicores        int64
	CurrentMemoryMiB        int64
	MemoryUsageMiB          int64
	MinMemoryMiB            int64
	MaxMemoryMiB            int64
	CurrentRPS              float64
	BaselineRPS             float64
	P99LatencyMs            float64
	SLOP99LatencyMs         float64
	ErrorRate               float64
	NodeAvailableCPUMilli   int64 // -1 if unknown
	NodeAvailableMemoryMiB  int64 // -1 if unknown
	CPUCoreHourPriceUSD     float64
	MemoryGiBHourPriceUSD   float64
	Ownership               OwnershipContext
}

// Decision represents a justified, auditable arbitration outcome.
type Decision struct {
	Strategy                         Strategy
	RecommendedReplicas              int32
	RecommendedCPUMillicores         int64
	RecommendedMemoryMiB             int64
	ExpectedEconomicImpactUSDPerHour float64 // negative = savings, positive = investment for SLO
	RiskScore                        float64 // [0.0, 1.0]
	Confidence                       float64 // [0.0, 1.0]
	ConflictDetected                 bool
	ConflictReason                   string
	Justification                    string
}

// Arbitrator evaluates workload telemetry and ownership state to select the optimal
// horizontal/vertical scaling strategy.
type Arbitrator struct {
	MinSamplesRequired int
	TargetCPUUtil      float64
}

// NewArbitrator returns an Arbitrator with production defaults.
func NewArbitrator() *Arbitrator {
	return &Arbitrator{
		MinSamplesRequired: 3,
		TargetCPUUtil:      0.70,
	}
}

// Evaluate computes the coordinated scaling strategy.
func (a *Arbitrator) Evaluate(in WorkloadTelemetry) Decision {
	minSamples := a.MinSamplesRequired
	if minSamples <= 0 {
		minSamples = 3
	}
	targetUtil := a.TargetCPUUtil
	if targetUtil <= 0 || targetUtil > 1.0 {
		targetUtil = 0.70
	}

	// Default prices if not supplied
	cpuPrice := in.CPUCoreHourPriceUSD
	if cpuPrice <= 0 {
		cpuPrice = 0.031611
	}
	memPrice := in.MemoryGiBHourPriceUSD
	if memPrice <= 0 {
		memPrice = 0.004237
	}

	curReplicas := maxInt32(1, in.CurrentReplicas)
	minReplicas := maxInt32(1, in.MinReplicas)
	maxReplicas := maxInt32(minReplicas, in.MaxReplicas)
	curCPU := maxInt64(50, in.CurrentCPUMillicores)
	minCPU := maxInt64(50, in.MinCPUMillicores)
	maxCPU := maxInt64(curCPU, in.MaxCPUMillicores)
	curMem := maxInt64(64, in.CurrentMemoryMiB)
	minMem := maxInt64(64, in.MinMemoryMiB)
	maxMem := maxInt64(curMem, in.MaxMemoryMiB)

	// 1. Check sufficiency of telemetry samples
	if in.SampleCount < minSamples || in.SLOP99LatencyMs <= 0 {
		return Decision{
			Strategy:                 StrategyWaitMoreInfo,
			RecommendedReplicas:      curReplicas,
			RecommendedCPUMillicores: curCPU,
			RecommendedMemoryMiB:     curMem,
			RiskScore:                0.15,
			Confidence:               0.30,
			Justification: fmt.Sprintf(
				"WAIT_MORE_INFO: sample_count=%d (min=%d) or invalid SLO=%.1fms; holding resources steady",
				in.SampleCount, minSamples, in.SLOP99LatencyMs,
			),
		}
	}

	horizBlocked, horizConflictReason := in.Ownership.HorizontalBlocked()
	vertBlocked, vertConflictReason := in.Ownership.VerticalBlocked()
	conflictDetected := horizBlocked || vertBlocked
	conflictReason := horizConflictReason
	if vertBlocked {
		if conflictReason != "" {
			conflictReason += "; " + vertConflictReason
		} else {
			conflictReason = vertConflictReason
		}
	}

	sloRatio := in.P99LatencyMs / in.SLOP99LatencyMs
	cpuUtil := float64(in.CPUUsageMillicores) / float64(curCPU)
	memUtil := float64(in.MemoryUsageMiB) / float64(curMem)

	rpsRatio := 1.0
	if in.BaselineRPS > 0 {
		rpsRatio = in.CurrentRPS / in.BaselineRPS
	}

	calcHourlyCost := func(replicas int32, cpuMilli int64, memMiB int64) float64 {
		cpuCores := float64(cpuMilli) / 1000.0
		memGiB := float64(memMiB) / 1024.0
		return float64(replicas) * (cpuCores*cpuPrice + memGiB*memPrice)
	}
	currentHourlyCost := calcHourlyCost(curReplicas, curCPU, curMem)

	// Situación A: Sustained traffic surge (rpsRatio >= 1.20) + SLO pressure (sloRatio >= 0.85)
	// OR severe SLO breach with high concurrency where horizontal replicas distribute load best.
	isTrafficSurgeWithSLODegradation := (rpsRatio >= 1.20 && sloRatio >= 0.85) || (sloRatio >= 1.05 && curReplicas < maxReplicas)
	if isTrafficSurgeWithSLODegradation {
		desiredReplicas := minInt32(maxReplicas, maxInt32(curReplicas+1, int32(math.Ceil(float64(curReplicas)*math.Min(1.5, math.Max(rpsRatio, sloRatio))))))

		// If horizontal is NOT blocked and we have replica headroom:
		if !horizBlocked && desiredReplicas > curReplicas {
			// Check if per-pod memory or CPU is ALSO critically saturated (> 90%) -> Coordinated Both
			if !vertBlocked && (cpuUtil >= 0.92 || memUtil >= 0.88) {
				targetCPU := minInt64(maxCPU, int64(math.Round(float64(curCPU)*1.20)))
				targetMem := curMem
				if memUtil >= 0.88 {
					targetMem = minInt64(maxMem, int64(math.Round(float64(curMem)*1.25)))
				}
				impact := calcHourlyCost(desiredReplicas, targetCPU, targetMem) - currentHourlyCost
				return Decision{
					Strategy:                         StrategyCoordinatedBoth,
					RecommendedReplicas:              desiredReplicas,
					RecommendedCPUMillicores:         targetCPU,
					RecommendedMemoryMiB:             targetMem,
					ExpectedEconomicImpactUSDPerHour: round4(impact),
					RiskScore:                        0.75,
					Confidence:                       0.88,
					ConflictDetected:                 conflictDetected,
					ConflictReason:                   conflictReason,
					Justification: fmt.Sprintf(
						"Situación A (Coordinated): Traffic surge (RPS ratio=%.2f) + SLO pressure (p99/SLO=%.2f) + pod saturation (CPU=%.0f%%, Mem=%.0f%%); scaling replicas %d->%d and resizing CPU %dm->%dm",
						rpsRatio, sloRatio, cpuUtil*100, memUtil*100, curReplicas, desiredReplicas, curCPU, targetCPU,
					),
				}
			}

			impact := calcHourlyCost(desiredReplicas, curCPU, curMem) - currentHourlyCost
			return Decision{
				Strategy:                         StrategyScaleHorizontally,
				RecommendedReplicas:              desiredReplicas,
				RecommendedCPUMillicores:         curCPU,
				RecommendedMemoryMiB:             curMem,
				ExpectedEconomicImpactUSDPerHour: round4(impact),
				RiskScore:                        0.55,
				Confidence:                       0.90,
				ConflictDetected:                 conflictDetected,
				ConflictReason:                   conflictReason,
				Justification: fmt.Sprintf(
					"Situación A (Horizontal Priority): Sustained traffic increase (RPS ratio=%.2f) and p99 latency degradation (p99/SLO=%.2f); scaling horizontally %d->%d replicas",
					rpsRatio, sloRatio, curReplicas, desiredReplicas,
				),
			}
		}

		// If horizontal IS blocked (e.g., native HPA owns replicas) or replicas at maxReplicas,
		// fall back to vertical resize if allowed and node has capacity.
		if !vertBlocked {
			targetCPU := minInt64(maxCPU, int64(math.Round(float64(curCPU)*1.25)))
			if in.NodeAvailableCPUMilli >= 0 && (targetCPU-curCPU) > in.NodeAvailableCPUMilli {
				targetCPU = curCPU + in.NodeAvailableCPUMilli
			}
			if targetCPU > curCPU {
				impact := calcHourlyCost(curReplicas, targetCPU, curMem) - currentHourlyCost
				return Decision{
					Strategy:                         StrategyResizeVertically,
					RecommendedReplicas:              curReplicas,
					RecommendedCPUMillicores:         targetCPU,
					RecommendedMemoryMiB:             curMem,
					ExpectedEconomicImpactUSDPerHour: round4(impact),
					RiskScore:                        0.60,
					Confidence:                       0.82,
					ConflictDetected:                 conflictDetected,
					ConflictReason:                   conflictReason,
					Justification: fmt.Sprintf(
						"SLO pressure (p99/SLO=%.2f) while horizontal scaling unavailable (%s); applying vertical in-place CPU boost %dm->%dm",
						sloRatio, defaultStr(horizConflictReason, "at maxReplicas"), curCPU, targetCPU,
					),
				}
			}
		}
	}

	// Single-instance vertical bottleneck without traffic surge (e.g. heavy compute per request)
	if sloRatio >= 0.90 && !vertBlocked {
		targetCPU := minInt64(maxCPU, int64(math.Round(float64(curCPU)*1.25)))
		if in.NodeAvailableCPUMilli >= 0 && (targetCPU-curCPU) > in.NodeAvailableCPUMilli {
			// Node exhausted vertically -> arbitrate to horizontal scale-out if allowed
			if !horizBlocked && curReplicas < maxReplicas {
				desiredReplicas := curReplicas + 1
				impact := calcHourlyCost(desiredReplicas, curCPU, curMem) - currentHourlyCost
				return Decision{
					Strategy:                         StrategyScaleHorizontally,
					RecommendedReplicas:              desiredReplicas,
					RecommendedCPUMillicores:         curCPU,
					RecommendedMemoryMiB:             curMem,
					ExpectedEconomicImpactUSDPerHour: round4(impact),
					RiskScore:                        0.65,
					Confidence:                       0.85,
					ConflictDetected:                 conflictDetected,
					ConflictReason:                   conflictReason,
					Justification: fmt.Sprintf(
						"Vertical resize blocked by node CPU capacity (available=%dm); arbitrating to horizontal scale-out %d->%d replicas",
						in.NodeAvailableCPUMilli, curReplicas, desiredReplicas,
					),
				}
			}
		}
		if targetCPU > curCPU {
			impact := calcHourlyCost(curReplicas, targetCPU, curMem) - currentHourlyCost
			return Decision{
				Strategy:                         StrategyResizeVertically,
				RecommendedReplicas:              curReplicas,
				RecommendedCPUMillicores:         targetCPU,
				RecommendedMemoryMiB:             curMem,
				ExpectedEconomicImpactUSDPerHour: round4(impact),
				RiskScore:                        0.50,
				Confidence:                       0.86,
				ConflictDetected:                 conflictDetected,
				ConflictReason:                   conflictReason,
				Justification: fmt.Sprintf(
					"Per-instance latency approaching SLO (p99/SLO=%.2f) with stable replica count; resizing CPU vertically %dm->%dm",
					sloRatio, curCPU, targetCPU,
				),
			}
		}
	}

	// Situación B: Comfortable latency (p99/SLO <= 0.55) and low resource utilization per instance.
	// Prioritize vertical downsizing without compromising replica availability floor.
	if sloRatio <= 0.55 && cpuUtil <= 0.50 && !vertBlocked {
		safeCPUFloor := maxInt64(minCPU, int64(math.Ceil(float64(in.CPUUsageMillicores)/targetUtil)))
		targetCPU := maxInt64(safeCPUFloor, int64(math.Round(float64(curCPU)*0.80)))
		targetMem := curMem
		if memUtil <= 0.45 {
			safeMemFloor := maxInt64(minMem, int64(math.Ceil(float64(in.MemoryUsageMiB)*1.30)))
			targetMem = maxInt64(safeMemFloor, int64(math.Round(float64(curMem)*0.90)))
		}

		if targetCPU < curCPU || targetMem < curMem {
			impact := calcHourlyCost(curReplicas, targetCPU, targetMem) - currentHourlyCost
			return Decision{
				Strategy:                         StrategyResizeVertically,
				RecommendedReplicas:              curReplicas,
				RecommendedCPUMillicores:         targetCPU,
				RecommendedMemoryMiB:             targetMem,
				ExpectedEconomicImpactUSDPerHour: round4(impact),
				RiskScore:                        0.20,
				Confidence:                       0.92,
				ConflictDetected:                 conflictDetected,
				ConflictReason:                   conflictReason,
				Justification: fmt.Sprintf(
					"Situación B (Vertical Rightsize Down): Comfortable latency (p99/SLO=%.2f) and low instance utilization (CPU=%.0f%%, Mem=%.0f%%); rightsizing CPU %dm->%dm, Mem %dMi->%dMi while preserving %d replicas for HA",
					sloRatio, cpuUtil*100, memUtil*100, curCPU, targetCPU, curMem, targetMem, curReplicas,
				),
			}
		}

		// Only scale in horizontally once vertical resources are already at minimum floor
		if !horizBlocked && curReplicas > minReplicas && curCPU <= minCPU {
			projectedCPUUtil := ( float64(in.CPUUsageMillicores) * float64(curReplicas) / float64(curReplicas-1) ) / float64(curCPU)
			if projectedCPUUtil <= targetUtil {
				desiredReplicas := curReplicas - 1
				impact := calcHourlyCost(desiredReplicas, curCPU, curMem) - currentHourlyCost
				return Decision{
					Strategy:                         StrategyScaleHorizontally,
					RecommendedReplicas:              desiredReplicas,
					RecommendedCPUMillicores:         curCPU,
					RecommendedMemoryMiB:             curMem,
					ExpectedEconomicImpactUSDPerHour: round4(impact),
					RiskScore:                        0.25,
					Confidence:                       0.88,
					ConflictDetected:                 conflictDetected,
					ConflictReason:                   conflictReason,
					Justification: fmt.Sprintf(
						"Vertical floor reached (%dm) with ample SLO slack (p99/SLO=%.2f); scaling in replicas %d->%d safely",
						curCPU, sloRatio, curReplicas, desiredReplicas,
					),
				}
			}
		}
	}

	return Decision{
		Strategy:                         StrategyNoOp,
		RecommendedReplicas:              curReplicas,
		RecommendedCPUMillicores:         curCPU,
		RecommendedMemoryMiB:             curMem,
		ExpectedEconomicImpactUSDPerHour: 0,
		RiskScore:                        0.10,
		Confidence:                       0.95,
		ConflictDetected:                 conflictDetected,
		ConflictReason:                   conflictReason,
		Justification: fmt.Sprintf(
			"NO_OP: Workload balanced within SLO and utilization bands (p99/SLO=%.2f, CPU=%.0f%%, Mem=%.0f%%)",
			sloRatio, cpuUtil*100, memUtil*100,
		),
	}
}

func maxInt32(a, b int32) int32 {
	if a > b {
		return a
	}
	return b
}

func minInt32(a, b int32) int32 {
	if a < b {
		return a
	}
	return b
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func minInt64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func round4(v float64) float64 {
	return math.Round(v*10000) / 10000
}

func defaultStr(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
