package pricing

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestLookupMultiCloudAndSpotDiscount(t *testing.T) {
	odCPU, odMem, spec := Lookup("m6i.2xlarge", "us-east-1", false, 0.031, 0.004)
	if spec.Provider != ProviderAWS || spec.VCPUs != 8 {
		t.Fatalf("unexpected spec: %+v", spec)
	}
	spCPU, spMem, _ := Lookup("m6i.2xlarge", "us-east-1", true, 0.031, 0.004)
	if spCPU >= odCPU || spMem >= odMem {
		t.Fatalf("expected spot rates (%f, %f) < on-demand rates (%f, %f)", spCPU, spMem, odCPU, odMem)
	}

	// Regional multiplier check (sa-east-1 > us-east-1)
	saCPU, _, _ := Lookup("m6i.2xlarge", "sa-east-1", false, 0.031, 0.004)
	if saCPU <= odCPU {
		t.Fatalf("expected sa-east-1 CPU rate (%f) > us-east-1 (%f)", saCPU, odCPU)
	}
}

func TestResolveNodePricingExtractsK8sLabels(t *testing.T) {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "gke-spot-node-1",
			Labels: map[string]string{
				"node.kubernetes.io/instance-type": "n2-standard-8",
				"topology.kubernetes.io/region":    "us-central1",
				"cloud.google.com/gke-spot":        "true",
			},
		},
	}
	cpuHr, memHr, spec := ResolveNodePricing(node, 0.031, 0.004)
	if spec.Provider != ProviderGCP || cpuHr <= 0 || memHr <= 0 {
		t.Fatalf("expected GCP spot node pricing, got cpu=%f mem=%f spec=%+v", cpuHr, memHr, spec)
	}
}
