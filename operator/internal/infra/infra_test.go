package infra

import (
	"math"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func node(labels map[string]string) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Labels: labels}}
}

func TestClassifyNode(t *testing.T) {
	c := DefaultClassifier()
	for name, tc := range map[string]struct {
		labels map[string]string
		want   Class
	}{
		"eks spot":            {map[string]string{"eks.amazonaws.com/capacityType": "SPOT"}, Spot},
		"eks on-demand":       {map[string]string{"eks.amazonaws.com/capacityType": "ON_DEMAND"}, OnDemand},
		"karpenter spot":      {map[string]string{"karpenter.sh/capacity-type": "spot"}, Spot},
		"karpenter on-demand": {map[string]string{"karpenter.sh/capacity-type": "on-demand"}, OnDemand},
		"gke spot":            {map[string]string{"cloud.google.com/gke-spot": "true"}, Spot},
		"gke preemptible":     {map[string]string{"cloud.google.com/gke-preemptible": "true"}, Spot},
		"aks spot":            {map[string]string{"kubernetes.azure.com/scalesetpriority": "spot"}, Spot},
		"aks regular":         {map[string]string{"kubernetes.azure.com/scalesetpriority": "regular"}, OnDemand},
		"case-insensitive":    {map[string]string{"eks.amazonaws.com/capacityType": "spot"}, Spot},
		"no labels":           {nil, Unknown},
		"unrelated labels":    {map[string]string{"kubernetes.io/os": "linux"}, Unknown},
		"gke flag false":      {map[string]string{"cloud.google.com/gke-spot": "false"}, Unknown},
		"contradiction": {map[string]string{
			"eks.amazonaws.com/capacityType": "SPOT", "karpenter.sh/capacity-type": "on-demand"}, Unknown},
	} {
		if got := c.ClassifyNode(node(tc.labels)); got != tc.want {
			t.Errorf("%s: %s want %s", name, got, tc.want)
		}
	}
	if c.ClassifyNode(nil) != Unknown {
		t.Fatal("nil node must be Unknown")
	}
}

func TestCustomRules(t *testing.T) {
	c := &Classifier{
		SpotRules:     []Rule{{Key: "lifecycle", Values: []string{"spot"}}},
		OnDemandRules: []Rule{{Key: "pool-type"}}, // presence is enough
	}
	if c.ClassifyNode(node(map[string]string{"lifecycle": "spot"})) != Spot ||
		c.ClassifyNode(node(map[string]string{"pool-type": "anything"})) != OnDemand ||
		c.ClassifyNode(node(map[string]string{"eks.amazonaws.com/capacityType": "SPOT"})) != Unknown {
		t.Fatal("custom rules must replace the defaults")
	}
}

func pod(name, nodeName string, mod func(*corev1.Pod)) corev1.Pod {
	p := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: corev1.PodSpec{NodeName: nodeName},
		Status: corev1.PodStatus{Phase: corev1.PodRunning}}
	if mod != nil {
		mod(&p)
	}
	return p
}

func TestSummarize(t *testing.T) {
	nodes := map[string]*corev1.Node{
		"spot-1": node(map[string]string{"karpenter.sh/capacity-type": "spot"}),
		"od-1":   node(map[string]string{"karpenter.sh/capacity-type": "on-demand"}),
		"plain":  node(nil),
	}
	lookups := 0
	lookup := func(n string) (*corev1.Node, bool) { lookups++; v, ok := nodes[n]; return v, ok }
	now := metav1.NewTime(time.Now())
	pods := []corev1.Pod{
		pod("a", "spot-1", nil), pod("b", "spot-1", nil), pod("c", "od-1", nil), pod("d", "plain", nil),
		pod("e", "gone-node", nil), // node not in cache -> Unknown
		pod("f", "", func(p *corev1.Pod) { // pending, unschedulable
			p.Status.Phase = corev1.PodPending
			p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: corev1.PodReasonUnschedulable}}
		}),
		pod("g", "", func(p *corev1.Pod) { p.Status.Phase = corev1.PodPending }), // pending, not yet judged
		pod("h", "od-1", func(p *corev1.Pod) { p.DeletionTimestamp = &now }),     // terminating
		pod("i", "od-1", func(p *corev1.Pod) { p.Status.Phase = corev1.PodSucceeded }),
	}
	s := DefaultClassifier().Summarize(pods, lookup)
	want := Summary{Spot: 2, OnDemand: 1, Unknown: 2, Pending: 2, Unschedulable: 1, Terminating: 1}
	if s != want {
		t.Fatalf("%+v want %+v", s, want)
	}
	if lookups != 4 { // spot-1, od-1, plain, gone-node: one lookup per distinct node
		t.Fatalf("node lookups should be cached per call, got %d", lookups)
	}
	if s.Placed() != 5 || math.Abs(s.SpotFraction()-0.4) > 1e-9 {
		t.Fatalf("placed=%d frac=%v", s.Placed(), s.SpotFraction())
	}
}

func TestCapacityFactor(t *testing.T) {
	cases := []struct {
		s      Summary
		buffer float64
		want   float64
	}{
		{Summary{}, 0.2, 1},
		{Summary{OnDemand: 4}, 0.2, 1},
		{Summary{Spot: 4}, 0.2, 0.8},
		{Summary{Spot: 2, OnDemand: 2}, 0.2, 0.9},
		{Summary{Spot: 2, Unknown: 2}, 0.2, 0.9}, // unknown is not discounted
		{Summary{Spot: 4}, 5, 0.1},               // buffer clamped to 0.9
		{Summary{Spot: 4}, -1, 1},
	}
	for _, c := range cases {
		if got := CapacityFactor(c.s, c.buffer); math.Abs(got-c.want) > 1e-9 {
			t.Errorf("%+v buffer %v: %v want %v", c.s, c.buffer, got, c.want)
		}
	}
}
