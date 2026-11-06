package arbitrator

import (
	"strings"
	"testing"
)

func TestSituacionATrafficSurgePrioritizesHorizontalScaling(t *testing.T) {
	arb := NewArbitrator()
	dec := arb.Evaluate(WorkloadTelemetry{
		SampleCount:            5,
		CurrentReplicas:        3,
		MinReplicas:            2,
		MaxReplicas:            10,
		CurrentCPUMillicores:   1000,
		CPUUsageMillicores:     750,
		MinCPUMillicores:       200,
		MaxCPUMillicores:       4000,
		CurrentMemoryMiB:       1024,
		MemoryUsageMiB:         600,
		MinMemoryMiB:           256,
		MaxMemoryMiB:           4096,
		CurrentRPS:             450,
		BaselineRPS:            300, // 1.5x traffic surge
		P99LatencyMs:           185,
		SLOP99LatencyMs:        200, // 0.925 SLO ratio
		NodeAvailableCPUMilli:  4000,
		NodeAvailableMemoryMiB: 8192,
	})

	if dec.Strategy != StrategyScaleHorizontally {
		t.Fatalf("expected StrategyScaleHorizontally for Situación A, got %s (%s)", dec.Strategy, dec.Justification)
	}
	if dec.RecommendedReplicas <= 3 {
		t.Fatalf("expected replicas > 3, got %d", dec.RecommendedReplicas)
	}
	if !strings.Contains(dec.Justification, "Situación A") {
		t.Fatalf("expected Situación A justification, got %s", dec.Justification)
	}
}

func TestSituacionBLowUtilizationPrioritizesVerticalRightsizeDown(t *testing.T) {
	arb := NewArbitrator()
	dec := arb.Evaluate(WorkloadTelemetry{
		SampleCount:            6,
		CurrentReplicas:        3,
		MinReplicas:            2,
		MaxReplicas:            10,
		CurrentCPUMillicores:   2000,
		CPUUsageMillicores:     400, // 20% CPU usage
		MinCPUMillicores:       250,
		MaxCPUMillicores:       4000,
		CurrentMemoryMiB:       2048,
		MemoryUsageMiB:         512, // 25% Memory usage
		MinMemoryMiB:           256,
		MaxMemoryMiB:           4096,
		CurrentRPS:             200,
		BaselineRPS:            200,
		P99LatencyMs:           60,
		SLOP99LatencyMs:        200, // 0.30 SLO ratio (comfortable)
		NodeAvailableCPUMilli:  4000,
		NodeAvailableMemoryMiB: 8192,
	})

	if dec.Strategy != StrategyResizeVertically {
		t.Fatalf("expected StrategyResizeVertically for Situación B, got %s (%s)", dec.Strategy, dec.Justification)
	}
	if dec.RecommendedReplicas != 3 {
		t.Fatalf("expected replicas to remain at 3 to preserve availability, got %d", dec.RecommendedReplicas)
	}
	if dec.RecommendedCPUMillicores >= 2000 {
		t.Fatalf("expected CPU to shrink below 2000m, got %dm", dec.RecommendedCPUMillicores)
	}
	if dec.ExpectedEconomicImpactUSDPerHour >= 0 {
		t.Fatalf("expected negative economic delta (cost savings), got %f", dec.ExpectedEconomicImpactUSDPerHour)
	}
}

func TestOwnershipConflictNativeHPAYieldsReplicasAndUsesVerticalResize(t *testing.T) {
	arb := NewArbitrator()
	dec := arb.Evaluate(WorkloadTelemetry{
		SampleCount:            5,
		CurrentReplicas:        3,
		MinReplicas:            2,
		MaxReplicas:            10,
		CurrentCPUMillicores:   1000,
		CPUUsageMillicores:     850,
		MinCPUMillicores:       200,
		MaxCPUMillicores:       4000,
		CurrentMemoryMiB:       1024,
		MemoryUsageMiB:         600,
		MinMemoryMiB:           256,
		MaxMemoryMiB:           4096,
		CurrentRPS:             500,
		BaselineRPS:            300,
		P99LatencyMs:           190,
		SLOP99LatencyMs:        200,
		NodeAvailableCPUMilli:  2000,
		NodeAvailableMemoryMiB: 4096,
		Ownership: OwnershipContext{
			HasNativeHPA:  true,
			NativeHPAName: "payments-hpa",
		},
	})

	if !dec.ConflictDetected {
		t.Fatalf("expected ConflictDetected=true when native HPA is present")
	}
	if dec.Strategy != StrategyResizeVertically {
		t.Fatalf("expected StrategyResizeVertically when HPA blocks horizontal writes, got %s", dec.Strategy)
	}
	if dec.RecommendedReplicas != 3 {
		t.Fatalf("must not mutate replicas when native HPA owns them, got %d", dec.RecommendedReplicas)
	}
	if dec.RecommendedCPUMillicores <= 1000 {
		t.Fatalf("expected vertical CPU boost above 1000m, got %dm", dec.RecommendedCPUMillicores)
	}
}

func TestWaitMoreInfoWhenSamplesInsufficient(t *testing.T) {
	arb := NewArbitrator()
	dec := arb.Evaluate(WorkloadTelemetry{
		SampleCount:          1,
		CurrentReplicas:      2,
		CurrentCPUMillicores: 500,
		CurrentMemoryMiB:     512,
		SLOP99LatencyMs:      200,
	})
	if dec.Strategy != StrategyWaitMoreInfo {
		t.Fatalf("expected StrategyWaitMoreInfo, got %s", dec.Strategy)
	}
}
