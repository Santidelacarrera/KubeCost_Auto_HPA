package main

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/capacity"
)

func invNode(name, zone string) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{corev1.LabelTopologyZone: zone}},
		Status: corev1.NodeStatus{Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("16Gi")}}}
}

func invPod(name, node string, mutate func(*corev1.Pod)) *corev1.Pod {
	yes := true
	p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: name, Labels: map[string]string{"app": name},
		OwnerReferences: []metav1.OwnerReference{{Kind: "ReplicaSet", Name: name, Controller: &yes}}},
		Spec: corev1.PodSpec{NodeName: node, Containers: []corev1.Container{{Name: "c", Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m"), corev1.ResourceMemory: resource.MustParse("1Gi")}}}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning}}
	if mutate != nil {
		mutate(p)
	}
	return p
}

func find(t *testing.T, s snapshot, name string) capacity.Pod {
	t.Helper()
	for _, p := range s.pods {
		if p.Name == name {
			return p
		}
	}
	t.Fatalf("pod %s not in the snapshot", name)
	return capacity.Pod{}
}

// The snapshot must carry the scheduling facts of the real objects, and nothing may be removed on a cluster that is short of capacity.
func TestSnapshotCarriesSchedulingFactsAndSaturationSignals(t *testing.T) {
	n1, n2 := invNode("n1", "a"), invNode("n2", "b")
	n2.Spec.Taints = []corev1.Taint{{Key: "dedicated", Value: "db", Effect: corev1.TaintEffectNoSchedule}}
	n2.Spec.Unschedulable = true
	n2.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeMemoryPressure, Status: corev1.ConditionTrue}, {Type: corev1.NodeDiskPressure, Status: corev1.ConditionFalse}}

	zonal := &corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: "pv-zonal"}, Spec: corev1.PersistentVolumeSpec{
		ClaimRef: &corev1.ObjectReference{Namespace: "shop", Name: "data"},
		NodeAffinity: &corev1.VolumeNodeAffinity{Required: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{MatchExpressions: []corev1.NodeSelectorRequirement{
			{Key: corev1.LabelTopologyZone, Operator: corev1.NodeSelectorOpIn, Values: []string{"a"}}}}}}}}}
	local := &corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: "pv-local"}, Spec: corev1.PersistentVolumeSpec{ClaimRef: &corev1.ObjectReference{Namespace: "shop", Name: "scratch"}}}

	claim := func(name string) func(*corev1.Pod) {
		return func(p *corev1.Pod) {
			p.Spec.Volumes = []corev1.Volume{{Name: "v", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: name}}}}
		}
	}
	objs := []runtime.Object{n1, n2, zonal, local,
		invPod("plain", "n1", func(p *corev1.Pod) {
			p.Spec.Tolerations = []corev1.Toleration{{Key: "dedicated", Operator: corev1.TolerationOpExists}}
			p.Spec.NodeSelector = map[string]string{"pool": "general"}
		}),
		invPod("zonal", "n1", claim("data")),
		invPod("local", "n1", claim("scratch")),
		invPod("bare", "n1", func(p *corev1.Pod) { p.OwnerReferences = nil }),
		invPod("affine", "n1", func(p *corev1.Pod) {
			p.Spec.Affinity = &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{}}}
		}),
		invPod("spread", "n1", func(p *corev1.Pod) {
			p.Spec.Affinity = &corev1.Affinity{PodAntiAffinity: &corev1.PodAntiAffinity{RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{
				{TopologyKey: corev1.LabelHostname, LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "spread"}}}}}}
		}),
		invPod("guarded", "n1", nil),
		invPod("waiting", "", func(p *corev1.Pod) {
			p.Status = corev1.PodStatus{Phase: corev1.PodPending, Conditions: []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: corev1.PodReasonUnschedulable}}}
		}),
		&policyv1.PodDisruptionBudget{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "guarded", Generation: 2},
			Spec:   policyv1.PodDisruptionBudgetSpec{Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "guarded"}}},
			Status: policyv1.PodDisruptionBudgetStatus{ObservedGeneration: 2, DisruptionsAllowed: 0}},
	}
	s, err := take(context.Background(), fake.NewSimpleClientset(objs...), nil, 0)
	if err != nil {
		t.Fatal(err)
	}

	var tainted capacity.Node
	for _, n := range s.nodes {
		if n.Name == "n2" {
			tainted = n
		}
	}
	if tainted.Zone != "b" || !tainted.Unschedulable || len(tainted.Taints) != 1 || tainted.Taints[0].Effect != "NoSchedule" {
		t.Fatalf("node facts lost: %+v", tainted)
	}
	if got := s.signals.NodesUnderPressure; len(got) != 1 || got[0] != "n2: MemoryPressure" {
		t.Fatalf("pressure: %v", got)
	}
	if got := s.signals.PendingUnschedulable; len(got) != 1 || got[0] != "shop/waiting" {
		t.Fatalf("pending: %v", got)
	}

	if p := find(t, s, "plain"); p.Pinned || len(p.Tolerations) != 1 || p.NodeSelector["pool"] != "general" {
		t.Fatalf("plain: %+v", p)
	}
	if p := find(t, s, "zonal"); p.Pinned || p.VolumeZone != "a" {
		t.Fatalf("a pod with a zonal volume is movable inside its zone: %+v", p)
	}
	if p := find(t, s, "local"); !p.Pinned || !strings.Contains(p.PinnedReason, "without a known zone") {
		t.Fatalf("local: %+v", p)
	}
	if p := find(t, s, "bare"); !p.Pinned || !strings.Contains(p.PinnedReason, "no controller") {
		t.Fatalf("bare: %+v", p)
	}
	if p := find(t, s, "affine"); !p.Pinned || !strings.Contains(p.PinnedReason, "node affinity") {
		t.Fatalf("affine: %+v", p)
	}
	if p := find(t, s, "spread"); p.Pinned || p.AntiAffinityGroup == "" {
		t.Fatalf("spread: %+v", p)
	}
	if p := find(t, s, "guarded"); !p.ProtectedByPDB || p.PDBDisruptionsAllowed == nil || *p.PDBDisruptionsAllowed != 0 {
		t.Fatalf("guarded: %+v", p)
	}

	rep, err := capacity.Analyze(s.nodes, s.pods, 0.85)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Removable) != 0 {
		t.Fatalf("nothing is removable here (pinned pods on n1, n2 is the last node of its zone): %+v", rep.Nodes)
	}
}

// A budget whose status is stale (the disruption controller has not seen the latest spec) must not be read as "zero allowed".
func TestStalePDBStatusIsNotTrusted(t *testing.T) {
	objs := []runtime.Object{invNode("n1", ""), invPod("guarded", "n1", nil),
		&policyv1.PodDisruptionBudget{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "guarded", Generation: 3},
			Spec:   policyv1.PodDisruptionBudgetSpec{Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "guarded"}}},
			Status: policyv1.PodDisruptionBudgetStatus{ObservedGeneration: 2, DisruptionsAllowed: 0}}}
	s, err := take(context.Background(), fake.NewSimpleClientset(objs...), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if p := find(t, s, "guarded"); !p.ProtectedByPDB || p.PDBDisruptionsAllowed != nil {
		t.Fatalf("%+v", p)
	}
}

func TestReconcileCommandLabelsTheLevel(t *testing.T) {
	dir := t.TempDir()
	write := func(name, day, billed, list string) string {
		body := "ChargeCategory,ResourceId,BilledCost,ListCost,BillingCurrency,ChargePeriodStart,ChargePeriodEnd\n" +
			"Usage,gke-prod-pool-1," + billed + "," + list + ",USD," + day + "T00:00:00Z," + day[:8] + "08T00:00:00Z\n"
		path := dir + "/" + name
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	before, after := write("before.csv", "2026-10-01", "70", "84"), write("after.csv", "2026-11-01", "42", "56")
	run := func(args ...string) string {
		var out strings.Builder
		if err := reconcileCmd(args, &out); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		return out.String()
	}
	if got := run("--before", before, "--after", after, "--scope", "gke-prod"); !strings.Contains(got, `"Level": "reconciled"`) || !strings.Contains(got, `"SavingPerDay": 4000000`) {
		t.Fatalf("%s", got)
	}
	if got := run("--before", before, "--after", after, "--simulated"); strings.Contains(got, `"Level": "reconciled"`) || !strings.Contains(got, "simulated") {
		t.Fatalf("simulated data must not be reconciled: %s", got)
	}
	if got := run("--before", before, "--after", after, "--kept=false"); !strings.Contains(got, `"Level": "estimated"`) {
		t.Fatalf("a change that was not kept saves nothing: %s", got)
	}
	if err := reconcileCmd([]string{"--before", before}, io.Discard); err == nil {
		t.Fatal("--after is required")
	}
	if err := reconcileCmd([]string{"--before", before, "--after", after, "--scope", "aks-other"}, io.Discard); err == nil {
		t.Fatal("a scope that matches nothing must fail")
	}
}
