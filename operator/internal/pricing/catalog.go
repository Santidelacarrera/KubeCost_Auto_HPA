// Package pricing provides a dynamic Multi-Cloud (AWS, GCP, Azure) instance pricing catalog
// with On-Demand and Spot rates per vCPU-hour and GiB-hour across regions.
package pricing

import (
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// CloudProvider identifies AWS, GCP, Azure, or Generic.
type CloudProvider string

const (
	ProviderAWS     CloudProvider = "AWS"
	ProviderGCP     CloudProvider = "GCP"
	ProviderAzure   CloudProvider = "AZURE"
	ProviderGeneric CloudProvider = "GENERIC"
)

// InstanceSpec holds hardware specs and regional pricing for a cloud VM type.
type InstanceSpec struct {
	Provider           CloudProvider
	InstanceType       string
	VCPUs              int
	MemoryGiB          float64
	OnDemandHourlyUSD  float64
	SpotHourlyUSD      float64
	CPUPricePerCoreHr  float64
	MemPricePerGiBHr   float64
	SpotCPUPriceCoreHr float64
	SpotMemPriceGiBHr  float64
}

// RegionMultipliers adjusts base US-East pricing for regional data-center cost differences.
var RegionMultipliers = map[string]float64{
	"us-east-1":      1.00,
	"us-east-2":      0.98,
	"us-west-2":      1.04,
	"eu-west-1":      1.11,
	"eu-central-1":   1.15,
	"ap-southeast-1": 1.18,
	"sa-east-1":      1.38,
	"us-central1":    1.00,
	"europe-west1":   1.10,
	"eastus":         1.00,
	"westeurope":     1.12,
}

var baseCatalog = map[string]InstanceSpec{
	// AWS General Purpose & Compute Optimized
	"m6i.large":   {Provider: ProviderAWS, InstanceType: "m6i.large", VCPUs: 2, MemoryGiB: 8, OnDemandHourlyUSD: 0.096, SpotHourlyUSD: 0.034},
	"m6i.xlarge":  {Provider: ProviderAWS, InstanceType: "m6i.xlarge", VCPUs: 4, MemoryGiB: 16, OnDemandHourlyUSD: 0.192, SpotHourlyUSD: 0.068},
	"m6i.2xlarge": {Provider: ProviderAWS, InstanceType: "m6i.2xlarge", VCPUs: 8, MemoryGiB: 32, OnDemandHourlyUSD: 0.384, SpotHourlyUSD: 0.135},
	"c6i.xlarge":  {Provider: ProviderAWS, InstanceType: "c6i.xlarge", VCPUs: 4, MemoryGiB: 8, OnDemandHourlyUSD: 0.170, SpotHourlyUSD: 0.058},
	"c6i.2xlarge": {Provider: ProviderAWS, InstanceType: "c6i.2xlarge", VCPUs: 8, MemoryGiB: 16, OnDemandHourlyUSD: 0.340, SpotHourlyUSD: 0.115},
	"c6i.4xlarge": {Provider: ProviderAWS, InstanceType: "c6i.4xlarge", VCPUs: 16, MemoryGiB: 32, OnDemandHourlyUSD: 0.680, SpotHourlyUSD: 0.230},
	"c6g.xlarge":  {Provider: ProviderAWS, InstanceType: "c6g.xlarge", VCPUs: 4, MemoryGiB: 8, OnDemandHourlyUSD: 0.136, SpotHourlyUSD: 0.046},
	"r6i.xlarge":  {Provider: ProviderAWS, InstanceType: "r6i.xlarge", VCPUs: 4, MemoryGiB: 32, OnDemandHourlyUSD: 0.252, SpotHourlyUSD: 0.082},
	"t3.medium":   {Provider: ProviderAWS, InstanceType: "t3.medium", VCPUs: 2, MemoryGiB: 4, OnDemandHourlyUSD: 0.0416, SpotHourlyUSD: 0.0125},
	"t3.large":    {Provider: ProviderAWS, InstanceType: "t3.large", VCPUs: 2, MemoryGiB: 8, OnDemandHourlyUSD: 0.0832, SpotHourlyUSD: 0.0250},
	// GCP Compute Engine
	"e2-standard-4": {Provider: ProviderGCP, InstanceType: "e2-standard-4", VCPUs: 4, MemoryGiB: 16, OnDemandHourlyUSD: 0.134, SpotHourlyUSD: 0.040},
	"n2-standard-4": {Provider: ProviderGCP, InstanceType: "n2-standard-4", VCPUs: 4, MemoryGiB: 16, OnDemandHourlyUSD: 0.194, SpotHourlyUSD: 0.058},
	"n2-standard-8": {Provider: ProviderGCP, InstanceType: "n2-standard-8", VCPUs: 8, MemoryGiB: 32, OnDemandHourlyUSD: 0.388, SpotHourlyUSD: 0.116},
	"c2-standard-8": {Provider: ProviderGCP, InstanceType: "c2-standard-8", VCPUs: 8, MemoryGiB: 32, OnDemandHourlyUSD: 0.417, SpotHourlyUSD: 0.125},
	// Azure AKS Virtual Machines
	"standard_d4s_v5": {Provider: ProviderAzure, InstanceType: "Standard_D4s_v5", VCPUs: 4, MemoryGiB: 16, OnDemandHourlyUSD: 0.192, SpotHourlyUSD: 0.059},
	"standard_d8s_v5": {Provider: ProviderAzure, InstanceType: "Standard_D8s_v5", VCPUs: 8, MemoryGiB: 32, OnDemandHourlyUSD: 0.384, SpotHourlyUSD: 0.118},
	"standard_f8s_v2": {Provider: ProviderAzure, InstanceType: "Standard_F8s_v2", VCPUs: 8, MemoryGiB: 16, OnDemandHourlyUSD: 0.338, SpotHourlyUSD: 0.098},
}

// Lookup returns the regionalized InstanceSpec for a given instanceType and region.
// It attributes 75% of instance cost to vCPU and 25% to Memory GiB (standard FinOps split).
func Lookup(instanceType, region string, isSpot bool, defaultCPUPrice, defaultMemPrice float64) (cpuCoreHr, memGiBHr float64, spec InstanceSpec) {
	mult := 1.0
	if m, ok := RegionMultipliers[strings.ToLower(strings.TrimSpace(region))]; ok {
		mult = m
	}
	key := strings.ToLower(strings.TrimSpace(instanceType))
	base, ok := baseCatalog[key]
	if !ok {
		c := defaultCPUPrice * mult
		m := defaultMemPrice * mult
		if isSpot {
			c *= 0.35
			m *= 0.35
		}
		return c, m, InstanceSpec{
			Provider:          ProviderGeneric,
			InstanceType:      instanceType,
			VCPUs:             4,
			MemoryGiB:         16,
			CPUPricePerCoreHr: c,
			MemPricePerGiBHr:  m,
		}
	}

	base.OnDemandHourlyUSD *= mult
	base.SpotHourlyUSD *= mult
	base.CPUPricePerCoreHr = (base.OnDemandHourlyUSD * 0.75) / float64(base.VCPUs)
	base.MemPricePerGiBHr = (base.OnDemandHourlyUSD * 0.25) / base.MemoryGiB
	base.SpotCPUPriceCoreHr = (base.SpotHourlyUSD * 0.75) / float64(base.VCPUs)
	base.SpotMemPriceGiBHr = (base.SpotHourlyUSD * 0.25) / base.MemoryGiB

	if isSpot {
		return base.SpotCPUPriceCoreHr, base.SpotMemPriceGiBHr, base
	}
	return base.CPUPricePerCoreHr, base.MemPricePerGiBHr, base
}

// ResolveNodePricing extracts instance-type, region, and Spot capacity labels from a Kubernetes Node.
func ResolveNodePricing(node *corev1.Node, defaultCPUPrice, defaultMemPrice float64) (cpuCoreHr, memGiBHr float64, spec InstanceSpec) {
	if node == nil {
		return defaultCPUPrice, defaultMemPrice, InstanceSpec{Provider: ProviderGeneric}
	}
	inst := node.Labels["node.kubernetes.io/instance-type"]
	if inst == "" {
		inst = node.Labels["beta.kubernetes.io/instance-type"]
	}
	reg := node.Labels["topology.kubernetes.io/region"]
	if reg == "" {
		reg = node.Labels["failure-domain.beta.kubernetes.io/region"]
	}
	spot := node.Labels["eks.amazonaws.com/capacityType"] == "SPOT" ||
		node.Labels["karpenter.sh/capacity-type"] == "spot" ||
		node.Labels["cloud.google.com/gke-spot"] == "true" ||
		node.Labels["cloud.google.com/gke-provisioning"] == "spot" ||
		node.Labels["kubernetes.azure.com/scalesetpriority"] == "spot"
	return Lookup(inst, reg, spot, defaultCPUPrice, defaultMemPrice)
}
