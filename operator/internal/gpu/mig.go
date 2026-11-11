package gpu

import (
	"fmt"
	"math"
)

// GPUAcceleratorSpec defines hardware capabilities and hourly pricing for a GPU model.
type GPUAcceleratorSpec struct {
	Model          string
	VRAMGiB        float64
	TDPWatts       float64
	HourlyOnDemand float64
	HourlySpot     float64
	SupportsMIG    bool
}

// MIGSliceProfile represents an NVIDIA Multi-Instance GPU hardware partition.
type MIGSliceProfile struct {
	ResourceName  string  // e.g. "nvidia.com/mig-1g.10gb"
	ComputeSlices int     // e.g. 1, 2, 3, 7
	TotalSlices   int     // 7 for A100/H100
	VRAMGiB       float64 // e.g. 10, 20, 40, 80
	CostFraction  float64 // fraction of full GPU cost
}

// WorkloadGPUTelemetry captures real-time AI/ML inference or training telemetry.
type WorkloadGPUTelemetry struct {
	CurrentGPUModel  string
	RequestedGPUs    int
	SMUtilizationPct float64 // 0..100 Streaming Multiprocessor active %
	VRAMUsedGiB      float64 // Active GPU memory in GiB
	TokensPerSecond  float64 // LLM token throughput (tokens/s)
	InferenceRPS     float64 // Inference requests/sec
	SpotTolerant     bool
	LatencyP99Ms     float64
	LatencySLOMs     float64
}

// GPUOptimizationRecommendation contains the deterministic right-sizing & MIG plan.
type GPUOptimizationRecommendation struct {
	Action                  string
	RecommendedGPUModel     string
	RecommendedResourceName string
	RecommendedSliceCount   int
	UseSpotInstance         bool
	CurrentHourlyCostUSD    float64
	ProjectedHourlyCostUSD  float64
	MonthlySavingsUSD       float64
	CostPer1kTokensUSD      float64
	CostPer1kInferencesUSD  float64
	VRAMHeadroomPct         float64
	Reason                  string
}

var gpuCatalog = map[string]GPUAcceleratorSpec{
	"H100-80GB": {Model: "H100-80GB", VRAMGiB: 80, TDPWatts: 700, HourlyOnDemand: 4.25, HourlySpot: 1.65, SupportsMIG: true},
	"A100-80GB": {Model: "A100-80GB", VRAMGiB: 80, TDPWatts: 400, HourlyOnDemand: 2.85, HourlySpot: 0.98, SupportsMIG: true},
	"A100-40GB": {Model: "A100-40GB", VRAMGiB: 40, TDPWatts: 400, HourlyOnDemand: 2.10, HourlySpot: 0.72, SupportsMIG: true},
	"L4-24GB":   {Model: "L4-24GB", VRAMGiB: 24, TDPWatts: 72, HourlyOnDemand: 0.81, HourlySpot: 0.28, SupportsMIG: false},
	"T4-16GB":   {Model: "T4-16GB", VRAMGiB: 16, TDPWatts: 70, HourlyOnDemand: 0.526, HourlySpot: 0.16, SupportsMIG: false},
}

var migProfiles80GB = []MIGSliceProfile{
	{ResourceName: "nvidia.com/mig-1g.10gb", ComputeSlices: 1, TotalSlices: 7, VRAMGiB: 10, CostFraction: 1.0 / 7.0},
	{ResourceName: "nvidia.com/mig-2g.20gb", ComputeSlices: 2, TotalSlices: 7, VRAMGiB: 20, CostFraction: 2.0 / 7.0},
	{ResourceName: "nvidia.com/mig-3g.40gb", ComputeSlices: 3, TotalSlices: 7, VRAMGiB: 40, CostFraction: 3.0 / 7.0},
	{ResourceName: "nvidia.com/mig-7g.80gb", ComputeSlices: 7, TotalSlices: 7, VRAMGiB: 80, CostFraction: 1.0},
}

var migProfiles40GB = []MIGSliceProfile{
	{ResourceName: "nvidia.com/mig-1g.5gb", ComputeSlices: 1, TotalSlices: 7, VRAMGiB: 5, CostFraction: 1.0 / 7.0},
	{ResourceName: "nvidia.com/mig-2g.10gb", ComputeSlices: 2, TotalSlices: 7, VRAMGiB: 10, CostFraction: 2.0 / 7.0},
	{ResourceName: "nvidia.com/mig-3g.20gb", ComputeSlices: 3, TotalSlices: 7, VRAMGiB: 20, CostFraction: 3.0 / 7.0},
	{ResourceName: "nvidia.com/mig-7g.40gb", ComputeSlices: 7, TotalSlices: 7, VRAMGiB: 40, CostFraction: 1.0},
}

// EvaluateGPUWorkload computes the optimal GPU/MIG configuration and AI unit economics.
func EvaluateGPUWorkload(t WorkloadGPUTelemetry) GPUOptimizationRecommendation {
	spec, ok := gpuCatalog[t.CurrentGPUModel]
	if !ok {
		spec = gpuCatalog["A100-80GB"]
	}
	gpus := t.RequestedGPUs
	if gpus <= 0 {
		gpus = 1
	}

	currentHourly := spec.HourlyOnDemand * float64(gpus)
	requiredVRAM := math.Max(t.VRAMUsedGiB*1.25, 2.0) // 25% safety buffer above active KV-cache/weights
	requiredSMFraction := math.Max(t.SMUtilizationPct/100.0*1.25, 0.10)

	sloBreached := t.LatencySLOMs > 0 && t.LatencyP99Ms > t.LatencySLOMs
	recommendedModel := spec.Model
	recommendedResource := "nvidia.com/gpu"
	costFraction := 1.0
	action := "MAINTAIN_FULL_GPU"

	if !sloBreached && gpus == 1 {
		if spec.SupportsMIG && (requiredVRAM <= spec.VRAMGiB*0.55) && requiredSMFraction <= 0.50 {
			profiles := migProfiles80GB
			if spec.VRAMGiB <= 40 {
				profiles = migProfiles40GB
			}
			for _, p := range profiles {
				sliceSMCap := float64(p.ComputeSlices) / float64(p.TotalSlices)
				if p.VRAMGiB >= requiredVRAM && sliceSMCap >= requiredSMFraction {
					recommendedResource = p.ResourceName
					costFraction = p.CostFraction
					if p.ComputeSlices < p.TotalSlices {
						action = "PARTITION_NVIDIA_MIG"
					}
					break
				}
			}
		} else if requiredVRAM <= 20.0 && t.SMUtilizationPct <= 45.0 && (spec.Model == "H100-80GB" || spec.Model == "A100-80GB") {
			l4 := gpuCatalog["L4-24GB"]
			recommendedModel = l4.Model
			spec = l4
			action = "RIGHTSIZE_GPU_FAMILY_L4"
		}
	}

	useSpot := t.SpotTolerant && !sloBreached
	unitHourly := spec.HourlyOnDemand
	if useSpot {
		unitHourly = spec.HourlySpot
		if action == "MAINTAIN_FULL_GPU" {
			action = "SHIFT_GPU_TO_SPOT"
		}
	}

	projectedHourly := math.Round(unitHourly*costFraction*float64(gpus)*10000) / 10000
	if projectedHourly > currentHourly {
		projectedHourly = currentHourly
	}
	monthlySavings := math.Max(0.0, math.Round((currentHourly-projectedHourly)*730.0*100)/100)

	var costPer1kTokens, costPer1kInf float64
	if t.TokensPerSecond > 0 {
		tokensPerHour := t.TokensPerSecond * 3600.0
		costPer1kTokens = math.Round((projectedHourly/(tokensPerHour/1000.0))*1000000) / 1000000
	}
	if t.InferenceRPS > 0 {
		infPerHour := t.InferenceRPS * 3600.0
		costPer1kInf = math.Round((projectedHourly/(infPerHour/1000.0))*1000000) / 1000000
	}

	allocVRAM := spec.VRAMGiB * costFraction
	if allocVRAM < requiredVRAM {
		allocVRAM = spec.VRAMGiB
	}
	vramHeadroom := math.Max(0.0, math.Round(((allocVRAM-t.VRAMUsedGiB)/allocVRAM)*10000)/100)

	return GPUOptimizationRecommendation{
		Action:                  action,
		RecommendedGPUModel:     recommendedModel,
		RecommendedResourceName: recommendedResource,
		RecommendedSliceCount:   gpus,
		UseSpotInstance:         useSpot,
		CurrentHourlyCostUSD:    math.Round(currentHourly*10000) / 10000,
		ProjectedHourlyCostUSD:  projectedHourly,
		MonthlySavingsUSD:       monthlySavings,
		CostPer1kTokensUSD:      costPer1kTokens,
		CostPer1kInferencesUSD:  costPer1kInf,
		VRAMHeadroomPct:         vramHeadroom,
		Reason: fmt.Sprintf(
			"SM=%.1f%% VRAM=%.1fGiB -> %s on %s (%s, Spot=%t) saving $%.2f/mo",
			t.SMUtilizationPct, t.VRAMUsedGiB, action, recommendedModel, recommendedResource, useSpot, monthlySavings,
		),
	}
}
