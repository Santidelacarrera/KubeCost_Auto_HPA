package gpu

import "testing"

func TestEvaluateGPUWorkload_MIGPartitioning(t *testing.T) {
	rec := EvaluateGPUWorkload(WorkloadGPUTelemetry{
		CurrentGPUModel:  "A100-80GB",
		RequestedGPUs:    1,
		SMUtilizationPct: 18.0,
		VRAMUsedGiB:      12.0,
		TokensPerSecond:  420.0,
		InferenceRPS:     14.0,
		SpotTolerant:     false,
		LatencyP99Ms:     180.0,
		LatencySLOMs:     350.0,
	})

	if rec.Action != "PARTITION_NVIDIA_MIG" {
		t.Fatalf("expected PARTITION_NVIDIA_MIG, got %s (%s)", rec.Action, rec.Reason)
	}
	if rec.RecommendedResourceName != "nvidia.com/mig-2g.20gb" {
		t.Fatalf("expected nvidia.com/mig-2g.20gb for 12GiB + 25%% buffer, got %s", rec.RecommendedResourceName)
	}
	if rec.MonthlySavingsUSD <= 1000.0 {
		t.Fatalf("expected >$1000/mo savings from 2g.20gb MIG slice on A100-80GB, got %.2f", rec.MonthlySavingsUSD)
	}
	if rec.CostPer1kTokensUSD <= 0 {
		t.Fatalf("expected positive CostPer1kTokensUSD, got %f", rec.CostPer1kTokensUSD)
	}
}

func TestEvaluateGPUWorkload_HighLoadMaintainsFullGPU(t *testing.T) {
	rec := EvaluateGPUWorkload(WorkloadGPUTelemetry{
		CurrentGPUModel:  "H100-80GB",
		RequestedGPUs:    2,
		SMUtilizationPct: 84.0,
		VRAMUsedGiB:      68.0,
		TokensPerSecond:  2800.0,
		InferenceRPS:     65.0,
		SpotTolerant:     false,
		LatencyP99Ms:     210.0,
		LatencySLOMs:     250.0,
	})

	if rec.Action != "MAINTAIN_FULL_GPU" {
		t.Fatalf("expected MAINTAIN_FULL_GPU under 84%% SM load, got %s", rec.Action)
	}
	if rec.RecommendedResourceName != "nvidia.com/gpu" {
		t.Fatalf("expected full nvidia.com/gpu, got %s", rec.RecommendedResourceName)
	}
}
