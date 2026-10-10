//go:build envtest

package controller

// Runs the reconciler and drainer against a REAL kube-apiserver + etcd
// (controller-runtime envtest). There is no kubelet, so pod status is set by
// the test. Needs KUBEBUILDER_ASSETS (dir with kube-apiserver and etcd).

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"k8s.io/apimachinery/pkg/runtime"

	v1alpha1 "github.com/santidelacarrera/kubecost_auto_hpa/operator/api/v1alpha1"
	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/actuator"
	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/cache"
	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/config"
	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/engine"
	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/metrics"
)

func startEnv(t *testing.T) (client.Client, *kubernetes.Clientset) {
	t.Helper()
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set")
	}
	crd, _ := filepath.Abs("../../config/crd/bases")
	env := &envtest.Environment{CRDDirectoryPaths: []string{crd}, ErrorIfCRDPathMissing: true}
	cfg, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = v1alpha1.AddToScheme(s)
	c, err := client.New(cfg, client.Options{Scheme: s})
	if err != nil {
		t.Fatal(err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c, cs
}

func mkPod(t *testing.T, c client.Client, name, node string) {
	t.Helper()
	ctx := context.Background()
	trueV := true
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "shop", Labels: map[string]string{"app": "api"},
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "rs", UID: "11111111-1111-1111-1111-111111111111", Controller: &trueV}}},
		Spec: corev1.PodSpec{NodeName: node, Containers: []corev1.Container{{
			Name: "app", Image: "nginx",
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("128Mi")},
				Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2"), corev1.ResourceMemory: resource.MustParse("256Mi")},
			},
			ResizePolicy: []corev1.ContainerResizePolicy{{ResourceName: corev1.ResourceCPU, RestartPolicy: corev1.NotRequired}},
		}}},
	}
	if err := c.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	p.Status.Phase = corev1.PodRunning
	// PDBs only protect Ready pods; unready ones are always evictable.
	p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	if err := c.Status().Update(ctx, p); err != nil {
		t.Fatal(err)
	}
}

func TestRealAPIServer_InPlaceResizeAndDefaults(t *testing.T) {
	c, cs := startEnv(t)
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "shop"}}))
	dep := newDeployment()
	dep.Spec.Template.Spec.Containers[0].Image = "nginx"
	must(c.Create(ctx, dep))
	mkPod(t, c, "api-1", "node-a")
	mkPod(t, c, "api-2", "node-a")

	// minimal CR: CRD defaulting must fill cpu/cooldown/interval
	// (created unstructured, as kubectl would: the typed client serializes zero-valued structs and defeats defaulting)
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "autoscaling.kubecost.io/v1alpha1", "kind": "KubeCostHPA",
		"metadata": map[string]any{"name": "api", "namespace": "shop"},
		"spec":     map[string]any{"targetRef": map[string]any{"name": "api"}, "slo": map[string]any{"p99LatencyMs": int64(200)}},
	}}
	must(c.Create(ctx, u))
	hpa := &v1alpha1.KubeCostHPA{ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "shop"}}
	var got v1alpha1.KubeCostHPA
	must(c.Get(ctx, client.ObjectKeyFromObject(hpa), &got))
	if got.Spec.CPU.Min.MilliValue() != 100 || got.Spec.CPU.Max.MilliValue() != 4000 ||
		got.Spec.Cooldown.Duration != 5*time.Minute || got.Spec.Interval.Duration != time.Minute || got.Spec.TargetRef.Kind != "Deployment" {
		t.Fatalf("defaults not applied: %+v", got.Spec)
	}

	r := &KubeCostHPAReconciler{
		Client: c, Resizer: &actuator.Resizer{Client: cs},
		Metrics: &fakeMetrics{snap: metrics.Snapshot{RPS: 200, P99Ms: 50}},
		Engine:  &fakeEngine{dec: engine.Decision{Action: "shrink", CPUMillicores: 800, Reason: "slack"}},
		Store:   cache.NewMemory(), Recorder: record.NewFakeRecorder(20), Defaults: config.Defaults{CPUPricePerCoreHour: 0.03},
	}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"api-1", "api-2"} {
		var p corev1.Pod
		must(c.Get(ctx, client.ObjectKey{Namespace: "shop", Name: n}, &p))
		res := p.Spec.Containers[0].Resources
		if res.Requests.Cpu().MilliValue() != 800 || res.Limits.Cpu().MilliValue() != 1600 {
			t.Fatalf("%s: requests=%v limits=%v", n, res.Requests.Cpu(), res.Limits.Cpu())
		}
		if res.Requests.Memory().Value() != 128<<20 {
			t.Fatalf("memory must be untouched: %v", res.Requests.Memory())
		}
	}
	must(c.Get(ctx, client.ObjectKeyFromObject(hpa), &got))
	if got.Status.DesiredCPUMillicores != 800 || got.Status.LastAction != "shrink" {
		t.Fatalf("status %+v", got.Status)
	}
}

func TestRealAPIServer_EvictionHonoursPDB(t *testing.T) {
	c, cs := startEnv(t)
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "shop"}}))
	for _, n := range []string{"spot-1", "od-1"} {
		node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: n}}
		must(c.Create(ctx, node))
		node.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}
		must(c.Status().Update(ctx, node))
	}
	mkPod(t, c, "api-1", "spot-1")
	one := intstr.FromInt32(1)
	pdb := &policyv1.PodDisruptionBudget{ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "shop"},
		Spec: policyv1.PodDisruptionBudgetSpec{MinAvailable: &one, Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "api"}}}}
	must(c.Create(ctx, pdb))
	// the disruption controller is absent, so set the PDB status ourselves: 0 disruptions allowed.
	pdb.Status = policyv1.PodDisruptionBudgetStatus{DisruptionsAllowed: 0, CurrentHealthy: 1, DesiredHealthy: 1, ExpectedPods: 1, ObservedGeneration: pdb.Generation}
	must(c.Status().Update(ctx, pdb))

	zero := int64(0)
	d := &actuator.Drainer{Client: cs, PollInterval: 50 * time.Millisecond, Timeout: 500 * time.Millisecond, GracePeriodSeconds: &zero}
	if err := d.Drain(ctx, "spot-1"); err == nil {
		t.Fatal("drain must fail while the PDB allows no disruptions")
	}
	var node corev1.Node
	must(c.Get(ctx, client.ObjectKey{Name: "spot-1"}, &node))
	if !node.Spec.Unschedulable {
		t.Fatal("node should be cordoned")
	}
	var p corev1.Pod
	must(c.Get(ctx, client.ObjectKey{Namespace: "shop", Name: "api-1"}, &p))
	if p.DeletionTimestamp != nil {
		t.Fatal("PDB-protected pod was evicted")
	}

	// allow one disruption: the drain now succeeds
	must(c.Get(ctx, client.ObjectKeyFromObject(pdb), pdb))
	pdb.Status.DisruptionsAllowed = 1
	must(c.Status().Update(ctx, pdb))
	d.Timeout = 5 * time.Second
	if err := d.Drain(ctx, "spot-1"); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, client.ObjectKey{Namespace: "shop", Name: "api-1"}, &p); err == nil {
		t.Fatal("pod should be gone")
	}
}

func TestRealAPIServer_MemoryGrowAndReplicaScale(t *testing.T) {
	c, cs := startEnv(t)
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "shop"}}))
	dep := newDeployment()
	dep.Spec.Template.Spec.Containers[0].Image = "nginx"
	two := int32(2)
	dep.Spec.Replicas = &two
	must(c.Create(ctx, dep))
	mkPod(t, c, "api-1", "node-a")

	r := &actuator.Resizer{Client: cs}
	var p corev1.Pod
	must(c.Get(ctx, client.ObjectKey{Namespace: "shop", Name: "api-1"}, &p))
	if _, err := r.Resize(ctx, &p, "app", actuator.Target{MemoryBytes: 192 << 20}); err != nil {
		t.Fatal(err)
	}
	must(c.Get(ctx, client.ObjectKey{Namespace: "shop", Name: "api-1"}, &p))
	rs := p.Spec.Containers[0].Resources
	if rs.Requests.Memory().Value() != 192<<20 || rs.Limits.Memory().Value() != 384<<20 || rs.Requests.Cpu().MilliValue() != 1000 {
		t.Fatalf("requests=%v limits=%v", rs.Requests, rs.Limits)
	}

	must(r.SetReplicas(ctx, "shop", "api", 5))
	var d appsv1.Deployment
	must(c.Get(ctx, client.ObjectKey{Namespace: "shop", Name: "api"}, &d))
	if *d.Spec.Replicas != 5 {
		t.Fatalf("replicas=%d", *d.Spec.Replicas)
	}
}

func TestRealAPIServer_CRDValidation(t *testing.T) {
	c, _ := startEnv(t)
	ctx := context.Background()
	if err := c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "shop"}}); err != nil {
		t.Fatal(err)
	}
	mk := func(name string, spec map[string]any) error {
		base := map[string]any{"targetRef": map[string]any{"name": "api"}, "slo": map[string]any{"p99LatencyMs": int64(200)}}
		for k, v := range spec {
			base[k] = v
		}
		return c.Create(ctx, &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "autoscaling.kubecost.io/v1alpha1", "kind": "KubeCostHPA",
			"metadata": map[string]any{"name": name, "namespace": "shop"}, "spec": base}})
	}
	if err := mk("ok", map[string]any{
		"cpu": map[string]any{"min": "100m", "max": "2"}, "replicas": map[string]any{"min": int64(1), "max": int64(5)},
		"memory": map[string]any{"min": "64Mi", "max": "1Gi"}, "budget": map[string]any{"monthlyLimitUSD": "5000.50"}}); err != nil {
		t.Fatalf("valid CR rejected: %v", err)
	}
	for name, spec := range map[string]map[string]any{
		"cpu-inverted":      {"cpu": map[string]any{"min": "2", "max": "100m"}},
		"replicas-inverted": {"replicas": map[string]any{"min": int64(5), "max": int64(2)}},
		"replicas-zero":     {"replicas": map[string]any{"min": int64(0), "max": int64(2)}},
		"memory-inverted":   {"memory": map[string]any{"min": "2Gi", "max": "64Mi"}},
		"budget-not-number": {"budget": map[string]any{"monthlyLimitUSD": "lots"}},
		"price-negative":    {"unitEconomics": map[string]any{"cpuPricePerCoreHour": "-1"}},
		"bad-policy":        {"budget": map[string]any{"monthlyLimitUSD": "10", "onSpendUnavailable": "Maybe"}},
	} {
		if err := mk(name, spec); err == nil {
			t.Errorf("%s: invalid CR was accepted", name)
		}
	}
}
