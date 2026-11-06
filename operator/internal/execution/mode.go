// Package execution implements operational execution modes (OBSERVE, SHADOW,
// DRY_RUN, CANARY, ACTIVE, EMERGENCY_STOP) and local safety guardrails that
// take strict priority over external SaaS recommendations.
package execution

import (
	"fmt"
	"math"
	"strings"
)

// Mode defines the operational execution mode of the autonomous agent.
type Mode string

const (
	ModeObserve       Mode = "OBSERVE"        // Only collects and reports telemetry; generates no decisions.
	ModeShadow        Mode = "SHADOW"         // Generates and audits decisions without executing them.
	ModeDryRun        Mode = "DRY_RUN"        // Simulates actuation outcomes without modifying Kubernetes objects.
	ModeCanary        Mode = "CANARY"         // Executes changes only on workloads/namespaces in the canary allow-list.
	ModeActive        Mode = "ACTIVE"         // Full autonomous actuation within local safety guardrails.
	ModeEmergencyStop Mode = "EMERGENCY_STOP" // Immediately halts all automated actions; returns control to manual operators.
)

// AnnotationExecutionMode allows per-workload or per-autoscaler override of the execution mode.
const AnnotationExecutionMode = "autoscaling.kubecost.io/execution-mode"

// AnnotationEmergencyStop is an explicit kill-switch annotation.
const AnnotationEmergencyStop = "autoscaling.kubecost.io/emergency-stop"

// ParseMode normalizes and validates an execution mode string.
func ParseMode(raw string, dryRunFlag bool, pausedFlag bool) Mode {
	if pausedFlag {
		return ModeEmergencyStop
	}
	switch strings.ToUpper(strings.TrimSpace(raw)) {
	case string(ModeObserve):
		return ModeObserve
	case string(ModeShadow):
		return ModeShadow
	case string(ModeDryRun):
		return ModeDryRun
	case string(ModeCanary):
		return ModeCanary
	case string(ModeActive):
		if dryRunFlag {
			return ModeDryRun
		}
		return ModeActive
	case string(ModeEmergencyStop):
		return ModeEmergencyStop
	default:
		if dryRunFlag {
			return ModeDryRun
		}
		return ModeActive
	}
}

// ResolveFromAnnotations inspects resource annotations and flags to determine the effective Mode.
func ResolveFromAnnotations(annotations map[string]string, dryRunFlag bool) Mode {
	if annotations != nil {
		if strings.EqualFold(annotations[AnnotationEmergencyStop], "true") ||
			strings.EqualFold(annotations["autoscaling.kubecost.io/pause"], "true") {
			return ModeEmergencyStop
		}
		if m, ok := annotations[AnnotationExecutionMode]; ok && m != "" {
			return ParseMode(m, dryRunFlag, false)
		}
	}
	return ParseMode("", dryRunFlag, false)
}

// Guardrails defines the local safety boundaries that take priority over any SaaS recommendation.
type Guardrails struct {
	MaxIncreasePercent        float64 // e.g., 1.00 = max +100% per step
	MaxDecreasePercent        float64 // e.g., 0.30 = max -30% per step
	MinCPUMillicores          int64
	MaxCPUMillicores          int64
	MinMemoryMiB              int64
	MaxMemoryMiB              int64
	MinReplicas               int32
	MaxReplicas               int32
	MaxConcurrentOperations   int
	MaxMigrationsPerWindow    int
	MaxVoluntaryDisruptions   int
	MonthlyBudgetUSD          float64
	CurrentMonthlySpendUSD    float64
	CanaryAllowedWorkloads    map[string]bool
}

// DefaultGuardrails returns production-safe local boundaries.
func DefaultGuardrails() Guardrails {
	return Guardrails{
		MaxIncreasePercent:      1.00, // at most double in a single step
		MaxDecreasePercent:      0.30, // at most 30% reduction in a single step
		MinCPUMillicores:        100,
		MaxCPUMillicores:        8000,
		MinMemoryMiB:            64,
		MaxMemoryMiB:            32768,
		MinReplicas:             1,
		MaxReplicas:             50,
		MaxConcurrentOperations: 5,
		MaxMigrationsPerWindow:  2,
		MaxVoluntaryDisruptions: 1,
	}
}

// Proposal represents a candidate actuation from either the local engine or the SaaS Control Plane.
type Proposal struct {
	WorkloadKey       string // namespace/name
	CurrentCPUMilli   int64
	ProposedCPUMilli  int64
	CurrentMemMiB     int64
	ProposedMemMiB    int64
	CurrentReplicas   int32
	ProposedReplicas  int32
	IsFromSaaS        bool
	InFlightOps       int
}

// EnforcementResult contains the clamped, safe values and whether execution may proceed.
type EnforcementResult struct {
	Mode              Mode
	ShouldEvaluate    bool
	ShouldExecute     bool
	ClampedCPUMilli   int64
	ClampedMemMiB     int64
	ClampedReplicas   int32
	ClampedByLocal    bool
	BlockedReason     string
	AuditExplanation  string
}

// Enforce applies operational mode gating and local safety guardrails to a Proposal.
func Enforce(mode Mode, g Guardrails, p Proposal) EnforcementResult {
	res := EnforcementResult{
		Mode:             mode,
		ShouldEvaluate:   true,
		ShouldExecute:    false,
		ClampedCPUMilli:  p.ProposedCPUMilli,
		ClampedMemMiB:    p.ProposedMemMiB,
		ClampedReplicas:  p.ProposedReplicas,
	}

	// 1. Operational Mode Gating
	switch mode {
	case ModeEmergencyStop:
		res.ShouldEvaluate = true
		res.ShouldExecute = false
		res.ClampedCPUMilli = p.CurrentCPUMilli
		res.ClampedMemMiB = p.CurrentMemMiB
		res.ClampedReplicas = p.CurrentReplicas
		res.BlockedReason = "EmergencyStopActive"
		res.AuditExplanation = "EMERGENCY_STOP active: all automated mutations are blocked; manual operator control enforced"
		return res
	case ModeObserve:
		res.ShouldEvaluate = false
		res.ShouldExecute = false
		res.ClampedCPUMilli = p.CurrentCPUMilli
		res.ClampedMemMiB = p.CurrentMemMiB
		res.ClampedReplicas = p.CurrentReplicas
		res.BlockedReason = "ObserveModeOnly"
		res.AuditExplanation = "OBSERVE mode: collecting telemetry only without generating or executing changes"
		return res
	case ModeShadow:
		res.ShouldExecute = false
		res.BlockedReason = "ShadowModeNoExecution"
	case ModeDryRun:
		res.ShouldExecute = false
		res.BlockedReason = "DryRunSimulation"
	case ModeCanary:
		if len(g.CanaryAllowedWorkloads) == 0 || !g.CanaryAllowedWorkloads[p.WorkloadKey] {
			res.ShouldExecute = false
			res.BlockedReason = "OutsideCanaryScope"
		} else {
			res.ShouldExecute = true
		}
	case ModeActive:
		res.ShouldExecute = true
	}

	// 2. Concurrent Operations Limit
	if g.MaxConcurrentOperations > 0 && p.InFlightOps >= g.MaxConcurrentOperations {
		res.ShouldExecute = false
		res.BlockedReason = "MaxConcurrentOperationsExceeded"
		res.AuditExplanation = fmt.Sprintf("local guardrail blocked execution: %d in-flight operations >= limit %d", p.InFlightOps, g.MaxConcurrentOperations)
		return res
	}

	var notes []string

	// 3. Clamp CPU step percentage and hard bounds
	if p.CurrentCPUMilli > 0 && res.ClampedCPUMilli > 0 {
		if g.MaxIncreasePercent > 0 {
			maxGrow := int64(math.Round(float64(p.CurrentCPUMilli) * (1.0 + g.MaxIncreasePercent)))
			if res.ClampedCPUMilli > maxGrow {
				res.ClampedCPUMilli = maxGrow
				res.ClampedByLocal = true
				notes = append(notes, fmt.Sprintf("CPU growth capped at +%.0f%% (%dm)", g.MaxIncreasePercent*100, maxGrow))
			}
		}
		if g.MaxDecreasePercent > 0 {
			minShrink := int64(math.Round(float64(p.CurrentCPUMilli) * (1.0 - g.MaxDecreasePercent)))
			if res.ClampedCPUMilli < minShrink {
				res.ClampedCPUMilli = minShrink
				res.ClampedByLocal = true
				notes = append(notes, fmt.Sprintf("CPU reduction capped at -%.0f%% (%dm)", g.MaxDecreasePercent*100, minShrink))
			}
		}
	}
	if g.MinCPUMillicores > 0 && res.ClampedCPUMilli > 0 && res.ClampedCPUMilli < g.MinCPUMillicores {
		res.ClampedCPUMilli = g.MinCPUMillicores
		res.ClampedByLocal = true
		notes = append(notes, fmt.Sprintf("CPU floored at min %dm", g.MinCPUMillicores))
	}
	if g.MaxCPUMillicores > 0 && res.ClampedCPUMilli > g.MaxCPUMillicores {
		res.ClampedCPUMilli = g.MaxCPUMillicores
		res.ClampedByLocal = true
		notes = append(notes, fmt.Sprintf("CPU capped at max %dm", g.MaxCPUMillicores))
	}

	// 4. Clamp Memory step percentage and hard bounds
	if p.CurrentMemMiB > 0 && res.ClampedMemMiB > 0 {
		if g.MaxIncreasePercent > 0 {
			maxGrow := int64(math.Round(float64(p.CurrentMemMiB) * (1.0 + g.MaxIncreasePercent)))
			if res.ClampedMemMiB > maxGrow {
				res.ClampedMemMiB = maxGrow
				res.ClampedByLocal = true
				notes = append(notes, fmt.Sprintf("Memory growth capped at +%.0f%% (%dMi)", g.MaxIncreasePercent*100, maxGrow))
			}
		}
		if g.MaxDecreasePercent > 0 {
			minShrink := int64(math.Round(float64(p.CurrentMemMiB) * (1.0 - g.MaxDecreasePercent)))
			if res.ClampedMemMiB < minShrink {
				res.ClampedMemMiB = minShrink
				res.ClampedByLocal = true
				notes = append(notes, fmt.Sprintf("Memory reduction capped at -%.0f%% (%dMi)", g.MaxDecreasePercent*100, minShrink))
			}
		}
	}
	if g.MinMemoryMiB > 0 && res.ClampedMemMiB > 0 && res.ClampedMemMiB < g.MinMemoryMiB {
		res.ClampedMemMiB = g.MinMemoryMiB
		res.ClampedByLocal = true
		notes = append(notes, fmt.Sprintf("Memory floored at min %dMi", g.MinMemoryMiB))
	}
	if g.MaxMemoryMiB > 0 && res.ClampedMemMiB > g.MaxMemoryMiB {
		res.ClampedMemMiB = g.MaxMemoryMiB
		res.ClampedByLocal = true
		notes = append(notes, fmt.Sprintf("Memory capped at max %dMi", g.MaxMemoryMiB))
	}

	// 5. Clamp Replicas
	if g.MinReplicas > 0 && res.ClampedReplicas > 0 && res.ClampedReplicas < g.MinReplicas {
		res.ClampedReplicas = g.MinReplicas
		res.ClampedByLocal = true
		notes = append(notes, fmt.Sprintf("Replicas floored at min %d", g.MinReplicas))
	}
	if g.MaxReplicas > 0 && res.ClampedReplicas > g.MaxReplicas {
		res.ClampedReplicas = g.MaxReplicas
		res.ClampedByLocal = true
		notes = append(notes, fmt.Sprintf("Replicas capped at max %d", g.MaxReplicas))
	}

	// 6. Budget Guardrail Priority
	if g.MonthlyBudgetUSD > 0 && g.CurrentMonthlySpendUSD >= g.MonthlyBudgetUSD {
		if res.ClampedCPUMilli > p.CurrentCPUMilli {
			res.ClampedCPUMilli = p.CurrentCPUMilli
			res.ClampedByLocal = true
			notes = append(notes, "CPU expansion blocked by exhausted monthly budget")
		}
		if res.ClampedReplicas > p.CurrentReplicas {
			res.ClampedReplicas = p.CurrentReplicas
			res.ClampedByLocal = true
			notes = append(notes, "Replica scale-out blocked by exhausted monthly budget")
		}
	}

	if len(notes) > 0 {
		res.AuditExplanation = fmt.Sprintf("mode=%s (SaaS=%v): local guardrails enforced [%s]", mode, p.IsFromSaaS, strings.Join(notes, "; "))
	} else {
		res.AuditExplanation = fmt.Sprintf("mode=%s (SaaS=%v): proposal within all local safety guardrails", mode, p.IsFromSaaS)
	}
	return res
}
