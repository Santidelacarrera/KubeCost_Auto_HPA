package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	v1alpha1 "github.com/santidelacarrera/kubecost_auto_hpa/operator/api/v1alpha1"
	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/engine"
	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/metrics"
)

var req = ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "shop", Name: "api"}}

func (e *env) hpa(t *testing.T) v1alpha1.KubeCostHPA {
	t.Helper()
	var h v1alpha1.KubeCostHPA
	if err := e.c.Get(context.Background(), req.NamespacedName, &h); err != nil {
		t.Fatal(err)
	}
	return h
}

func (e *env) podCPU(t *testing.T, name string) int64 {
	t.Helper()
	p, err := e.cs.CoreV1().Pods("shop").Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return p.Spec.Containers[0].Resources.Requests.Cpu().MilliValue()
}

func TestShrinkResizesPodsInPlaceAndRecordsStatus(t *testing.T) {
	e := newEnv(newHPA(), newDeployment(), newPod("api-1"), newPod("api-2"))
	res, err := e.r.Reconcile(context.Background(), req)
	if err != nil || res.RequeueAfter != time.Minute {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if e.podCPU(t, "api-1") != 800 || e.podCPU(t, "api-2") != 800 {
		t.Fatal("pods not resized")
	}
	h := e.hpa(t)
	if h.Status.DesiredCPUMillicores != 800 || h.Status.LastAction != "shrink" || h.Status.LastResizeTime == nil {
		t.Fatalf("status %+v", h.Status)
	}
	if !meta.IsStatusConditionTrue(h.Status.Conditions, condReady) {
		t.Fatal("not ready")
	}
	// engine input: per-pod RPS and template CPU
	if e.e.last.RPS != 100 || e.e.last.CPUMillicores != 1000 || e.e.last.MinMillicores != 100 || e.e.last.MaxMillicores != 4000 {
		t.Fatalf("engine request %+v", e.e.last)
	}
}

func TestCooldownSuppressesSecondResize(t *testing.T) {
	e := newEnv(newHPA(), newDeployment(), newPod("api-1"))
	e.r.Reconcile(context.Background(), req)
	e.e.dec.CPUMillicores = 640
	e.t = e.t.Add(time.Minute)
	e.r.Reconcile(context.Background(), req)
	if got := e.podCPU(t, "api-1"); got != 800 {
		t.Fatalf("resized during cooldown: %d", got)
	}
	e.t = e.t.Add(6 * time.Minute)
	e.m.snap.At = e.t // metrics cache expired by now
	e.r.Reconcile(context.Background(), req)
	if got := e.podCPU(t, "api-1"); got != 640 {
		t.Fatalf("expected resize after cooldown, got %d", got)
	}
}

func TestNewPodsConvergeToDesired(t *testing.T) {
	e := newEnv(newHPA(), newDeployment(), newPod("api-1"))
	e.r.Reconcile(context.Background(), req)
	// a new pod starts from the template (1000m); the next reconcile resizes it
	if _, err := e.cs.CoreV1().Pods("shop").Create(context.Background(), newPod("api-2"), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := e.c.Create(context.Background(), newPod("api-2")); err != nil {
		t.Fatal(err)
	}
	e.r.Reconcile(context.Background(), req)
	if e.podCPU(t, "api-2") != 800 {
		t.Fatal("new pod did not converge")
	}
}

func TestDryRunDoesNotTouchPods(t *testing.T) {
	h := newHPA()
	h.Spec.DryRun = true
	e := newEnv(h, newDeployment(), newPod("api-1"))
	e.r.Reconcile(context.Background(), req)
	if e.podCPU(t, "api-1") != 1000 || e.hpa(t).Status.DesiredCPUMillicores != 0 {
		t.Fatal("dry-run modified state")
	}
	if got := e.hpa(t).Status.LastReason; got == "" {
		t.Fatal("expected dry-run reason")
	}
}

func TestMetricsFailureHoldsAndFallsBackToStale(t *testing.T) {
	e := newEnv(newHPA(), newDeployment(), newPod("api-1"))
	e.m.err = errors.New("prometheus down")
	e.r.Reconcile(context.Background(), req)
	h := e.hpa(t)
	if e.podCPU(t, "api-1") != 1000 || meta.IsStatusConditionTrue(h.Status.Conditions, condReady) {
		t.Fatal("must not act without metrics")
	}
	// healthy once, then Prometheus fails after the fresh TTL: stale value is used
	e.m.err = nil
	e.e.dec.Action = "hold"
	e.r.Reconcile(context.Background(), req)
	e.m.err = errors.New("prometheus down")
	e.t = e.t.Add(time.Minute) // fresh cache expired (30s), stale (15m) still valid
	e.r.Reconcile(context.Background(), req)
	if !meta.IsStatusConditionTrue(e.hpa(t).Status.Conditions, condReady) {
		t.Fatal("expected stale fallback to keep the loop working")
	}
}

func TestMissingTargetAndInvalidSpec(t *testing.T) {
	e := newEnv(newHPA())
	e.r.Reconcile(context.Background(), req)
	if c := meta.FindStatusCondition(e.hpa(t).Status.Conditions, condReady); c == nil || c.Reason != "TargetNotFound" {
		t.Fatalf("cond %+v", c)
	}
	h := newHPA()
	h.Spec.UnitEconomics.MaxCostPerRequest = "abc"
	e = newEnv(h, newDeployment())
	e.r.Reconcile(context.Background(), req)
	if c := meta.FindStatusCondition(e.hpa(t).Status.Conditions, condReady); c == nil || c.Reason != "InvalidSpec" {
		t.Fatalf("cond %+v", c)
	}
}

func TestEngineErrorLeavesPodsAlone(t *testing.T) {
	e := newEnv(newHPA(), newDeployment(), newPod("api-1"))
	e.e.err = errors.New("engine down")
	if _, err := e.r.Reconcile(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if e.podCPU(t, "api-1") != 1000 {
		t.Fatal("pod changed")
	}
}

func TestBudgetAndSpotInputsAreForwarded(t *testing.T) {
	h := newHPA()
	h.Spec.Budget = &v1alpha1.Budget{MonthlyLimitUSD: "100"}
	h.Spec.Spot = v1alpha1.SpotSpec{Enabled: true, FaultTolerant: true, MinReplicas: 2}
	e := newEnv(h, newDeployment(), newPod("api-1"))
	e.r.Spend = spend(120)
	yes := true
	e.e.dec = engineDecision("hold", 1000, true, &yes)
	e.r.Reconcile(context.Background(), req)
	if e.e.last.Budget == nil || e.e.last.Budget.Spent != 120 || e.e.last.Budget.MonthlyLimit != 100 {
		t.Fatalf("budget %+v", e.e.last.Budget)
	}
	if e.e.last.Spot == nil || !e.e.last.Spot.FaultTolerant {
		t.Fatal("spot not forwarded")
	}
	st := e.hpa(t).Status
	if !st.BudgetFrozen || !st.SpotEligible {
		t.Fatalf("status %+v", st)
	}
}

func TestMemoryAndReplicasApplied(t *testing.T) {
	h := newHPA()
	h.Spec.Memory = &v1alpha1.MemorySpec{Min: resource.MustParse("64Mi"), Max: resource.MustParse("2Gi")}
	h.Spec.Replicas = &v1alpha1.ReplicaSpec{Min: 1, Max: 5}
	dep := newDeployment()
	two := int32(2)
	dep.Spec.Replicas = &two
	e := newEnv(h, dep, newPod("api-1"), newPod("api-2"))
	e.m.snap.MemUsageMiB = 900
	e.e.dec = engine.Decision{Action: "hold", CPUMillicores: 1000, Reason: "x",
		Memory:   &engine.MemoryDec{Action: "grow", MiB: 1280, Reason: "mem hot"},
		Replicas: &engine.ReplicasDec{Target: 3, Reason: "scale out"}}
	if _, err := e.r.Reconcile(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if e.e.last.Memory == nil || e.e.last.Memory.RequestMiB != 1024 || e.e.last.Memory.MaxMiB != 2048 {
		t.Fatalf("memory request %+v", e.e.last.Memory)
	}
	if e.e.last.Replicas == nil || e.e.last.Replicas.Current != 2 || e.e.last.Replicas.Max != 5 {
		t.Fatalf("replicas request %+v", e.e.last.Replicas)
	}
	p, _ := e.cs.CoreV1().Pods("shop").Get(context.Background(), "api-1", metav1.GetOptions{})
	if p.Spec.Containers[0].Resources.Requests.Memory().Value() != 1280<<20 {
		t.Fatalf("memory not resized: %v", p.Spec.Containers[0].Resources.Requests.Memory())
	}
	sc, _ := e.cs.AppsV1().Deployments("shop").GetScale(context.Background(), "api", metav1.GetOptions{})
	if sc.Spec.Replicas != 3 {
		t.Fatalf("replicas=%d", sc.Spec.Replicas)
	}
	st := e.hpa(t).Status
	if st.DesiredMemoryMiB != 1280 || st.DesiredReplicas != 3 {
		t.Fatalf("status %+v", st)
	}
}

func TestMemoryUntouchedWhenNotEnabled(t *testing.T) {
	e := newEnv(newHPA(), newDeployment(), newPod("api-1"))
	e.m.snap.MemUsageMiB = 900
	e.r.Reconcile(context.Background(), req)
	if e.e.last.Memory != nil || e.e.last.Replicas != nil {
		t.Fatal("memory/replicas must be opt-in")
	}
	p, _ := e.cs.CoreV1().Pods("shop").Get(context.Background(), "api-1", metav1.GetOptions{})
	if p.Spec.Containers[0].Resources.Requests.Memory().Value() != 1<<30 {
		t.Fatal("memory changed without opt-in")
	}
}

type failingSpend struct{}

func (failingSpend) MonthlySpend(context.Context, string) (float64, error) {
	return 0, errors.New("kubecost down")
}

func TestSpendUnavailablePolicy(t *testing.T) {
	for policy, wantBudget := range map[string]bool{"Ignore": false, "FreezeGrowth": true} {
		h := newHPA()
		h.Spec.Budget = &v1alpha1.Budget{MonthlyLimitUSD: "100", OnSpendUnavailable: policy}
		e := newEnv(h, newDeployment(), newPod("api-1"))
		e.r.Spend = failingSpend{}
		e.r.Reconcile(context.Background(), req)
		if got := e.e.last.Budget != nil; got != wantBudget {
			t.Fatalf("%s: budget forwarded=%v want %v", policy, got, wantBudget)
		}
		if wantBudget && e.e.last.Budget.Spent < e.e.last.Budget.MonthlyLimit {
			t.Fatal("FreezeGrowth must report the budget as exhausted")
		}
	}
}

func TestSavingsCounterAccrues(t *testing.T) {
	e := newEnv(newHPA(), newDeployment(), newPod("api-1"), newPod("api-2"))
	e.r.Reconcile(context.Background(), req) // shrink 1000m -> 800m, 2 pods
	name := "kubecost_hpa_savings_usd_total"
	before := counterValue(t, name)
	e.t = e.t.Add(30 * time.Minute)
	e.m.snap.At = e.t
	e.r.Reconcile(context.Background(), req)
	got := counterValue(t, name) - before
	want := 0.2 * 2 * 0.03 * 0.5 // 200m saved x 2 pods x $0.03/core-h x 0.5h
	if diff := got - want; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("accrued %v want %v", got, want)
	}
}

func counterValue(t *testing.T, name string) float64 {
	t.Helper()
	mfs, err := ctrlmetrics.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	total := 0.0
	for _, mf := range mfs {
		if mf.GetName() == name {
			for _, m := range mf.GetMetric() {
				total += m.GetCounter().GetValue()
			}
		}
	}
	return total
}

func TestGrowthBypassesCooldownAfterShrinkButNotBackToBack(t *testing.T) {
	e := newEnv(newHPA(), newDeployment(), newPod("api-1"))
	ctx := context.Background()
	e.r.Reconcile(ctx, req) // shrink 1000 -> 800
	if e.podCPU(t, "api-1") != 800 {
		t.Fatal("setup: expected shrink")
	}
	// SLO breach right after the shrink: growth is allowed immediately.
	e.e.dec = engine.Decision{Action: "grow", CPUMillicores: 1000, Reason: "slo"}
	e.t = e.t.Add(35 * time.Second) // metrics cache (30s) expired
	e.syncPods()
	e.r.Reconcile(ctx, req)
	if got := e.podCPU(t, "api-1"); got != 1000 {
		t.Fatalf("growth should bypass the shrink cooldown, got %d", got)
	}
	if e.hpa(t).Status.LastResizeAction != "grow" {
		t.Fatalf("status %+v", e.hpa(t).Status)
	}
	// A second growth 5s later is held back (anti-flap), 40s later it goes through.
	e.e.dec = engine.Decision{Action: "grow", CPUMillicores: 1250, Reason: "slo"}
	e.t = e.t.Add(5 * time.Second)
	e.syncPods()
	e.r.Reconcile(ctx, req)
	if got := e.podCPU(t, "api-1"); got != 1000 {
		t.Fatalf("back-to-back growth must wait, got %d", got)
	}
	e.t = e.t.Add(40 * time.Second)
	e.syncPods()
	e.r.Reconcile(ctx, req)
	if got := e.podCPU(t, "api-1"); got != 1250 {
		t.Fatalf("growth after growCooldown should apply, got %d", got)
	}
	// ...and a shrink right after growth still waits the full cooldown.
	e.e.dec = engine.Decision{Action: "shrink", CPUMillicores: 1000, Reason: "slack"}
	e.t = e.t.Add(40 * time.Second)
	e.syncPods()
	e.r.Reconcile(ctx, req)
	if got := e.podCPU(t, "api-1"); got != 1250 {
		t.Fatalf("shrink must respect the full cooldown, got %d", got)
	}
}

func TestFinalizerRestoresTemplateResourcesOnDelete(t *testing.T) {
	ctx := context.Background()
	e := newEnv(newHPA(), newDeployment(), newPod("api-1"), newPod("api-2"))
	e.r.Reconcile(ctx, req) // adds finalizer, shrinks to 800m
	if !controllerutil.ContainsFinalizer(ptr(e.hpa(t)), Finalizer) {
		t.Fatal("finalizer not added")
	}
	if e.podCPU(t, "api-1") != 800 {
		t.Fatal("setup: expected shrink")
	}
	h := e.hpa(t)
	if err := e.c.Delete(ctx, &h); err != nil {
		t.Fatal(err)
	}
	e.syncPods()
	if _, err := e.r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"api-1", "api-2"} {
		if got := e.podCPU(t, p); got != 1000 {
			t.Fatalf("%s not restored: %dm", p, got)
		}
	}
	var gone v1alpha1.KubeCostHPA
	if err := e.c.Get(ctx, req.NamespacedName, &gone); !apierrors.IsNotFound(err) {
		t.Fatalf("resource should be released after restoring, err=%v", err)
	}
}

func TestFinalizerNeverBlocksDeletion(t *testing.T) {
	ctx := context.Background()
	// 1) nothing was ever resized: release without touching pods
	e := newEnv(newHPA(), newDeployment(), newPod("api-1"))
	e.e.dec = engine.Decision{Action: "hold", CPUMillicores: 1000}
	e.r.Reconcile(ctx, req)
	h := e.hpa(t)
	_ = e.c.Delete(ctx, &h)
	e.r.Reconcile(ctx, req)
	if err := e.c.Get(ctx, req.NamespacedName, &v1alpha1.KubeCostHPA{}); !apierrors.IsNotFound(err) {
		t.Fatalf("untouched resource must be released, err=%v", err)
	}
	// 2) target Deployment already deleted
	e = newEnv(newHPA(), newDeployment(), newPod("api-1"))
	e.r.Reconcile(ctx, req)
	_ = e.c.Delete(ctx, newDeployment())
	h = e.hpa(t)
	_ = e.c.Delete(ctx, &h)
	e.r.Reconcile(ctx, req)
	if err := e.c.Get(ctx, req.NamespacedName, &v1alpha1.KubeCostHPA{}); !apierrors.IsNotFound(err) {
		t.Fatalf("missing target must not block deletion, err=%v", err)
	}
	// 3) restore keeps failing: released after the deadline
	e = newEnv(newHPA(), newDeployment(), newPod("api-1"))
	e.r.Reconcile(ctx, req)
	e.syncPods()
	var p corev1.Pod
	_ = e.c.Get(ctx, client.ObjectKey{Namespace: "shop", Name: "api-1"}, &p)
	p.Status.Conditions = []corev1.PodCondition{{Type: "PodResizeInProgress", Status: corev1.ConditionTrue}}
	if err := e.c.Status().Update(ctx, &p); err != nil {
		t.Fatal(err)
	}
	h = e.hpa(t)
	_ = e.c.Delete(ctx, &h)
	e.syncPods()
	e.t = time.Now() // deletion timestamp is "now": still within the deadline
	res, rerr := e.r.Reconcile(ctx, req)
	if res.RequeueAfter == 0 {
		t.Fatalf("a resize in flight should requeue the restore (err=%v, cpu=%d, hpa=%+v)", rerr, e.podCPU(t, "api-1"), e.hpa(t).Status.DesiredCPUMillicores)
	}
	e.t = time.Now().Add(restoreDeadline + time.Minute)
	e.r.Reconcile(ctx, req)
	if err := e.c.Get(ctx, req.NamespacedName, &v1alpha1.KubeCostHPA{}); !apierrors.IsNotFound(err) {
		t.Fatalf("deadline must release the resource, err=%v", err)
	}
}

func TestMetricQueryOverridesReachTheSource(t *testing.T) {
	h := newHPA()
	h.Spec.Metrics = &v1alpha1.MetricsSpec{RPSQuery: "my_rps", P99Query: "my_p99"}
	e := newEnv(h, newDeployment(), newPod("api-1"))
	var got metrics.Workload
	e.r.Metrics = metricsFunc(func(w metrics.Workload) { got = w })
	e.r.Reconcile(context.Background(), req)
	if got.Overrides == nil || got.Overrides.RPS != "my_rps" || got.Overrides.P99Ms != "my_p99" {
		t.Fatalf("overrides not forwarded: %+v", got.Overrides)
	}
}

// brokenStore simulates Redis being unreachable.
type brokenStore struct{}

func (brokenStore) Get(context.Context, string, any) (bool, error) {
	return false, errors.New("redis down")
}
func (brokenStore) Set(context.Context, string, any, time.Duration) error {
	return errors.New("redis down")
}
func (brokenStore) Delete(context.Context, string) error { return errors.New("redis down") }

func TestRedisDownDegradesGracefully(t *testing.T) {
	e := newEnv(newHPA(), newDeployment(), newPod("api-1"))
	e.r.Store = brokenStore{}
	if _, err := e.r.Reconcile(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if got := e.podCPU(t, "api-1"); got != 800 {
		t.Fatalf("scaling must keep working without the cache, got %dm", got)
	}
	// the cooldown falls back to status.LastResizeTime
	e.e.dec.CPUMillicores = 640
	e.t = e.t.Add(time.Minute)
	e.r.Reconcile(context.Background(), req)
	if got := e.podCPU(t, "api-1"); got != 800 {
		t.Fatalf("cooldown must hold without Redis, got %dm", got)
	}
}

func replicasHPA() *v1alpha1.KubeCostHPA {
	h := newHPA()
	h.Spec.Replicas = &v1alpha1.ReplicaSpec{Min: 1, Max: 5}
	return h
}

func TestLegacyHorizontalScalingYieldsToANativeHPA(t *testing.T) {
	min := int32(1)
	native := &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Name: "team-hpa", Namespace: "shop"},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{MinReplicas: &min, MaxReplicas: 9,
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{APIVersion: "apps/v1", Kind: "Deployment", Name: "api"}},
	}
	dep := newDeployment()
	two := int32(2)
	dep.Spec.Replicas = &two
	e := newEnv(replicasHPA(), dep, newPod("api-1"), newPod("api-2"), native)
	e.e.dec = engine.Decision{Action: "hold", CPUMillicores: 1000, Replicas: &engine.ReplicasDec{Target: 4, Reason: "scale out"}}
	e.r.Reconcile(context.Background(), req)
	if e.e.last.Replicas != nil {
		t.Fatal("the engine must not even be asked for a replica recommendation while an HPA owns the Deployment")
	}
	sc, _ := e.cs.AppsV1().Deployments("shop").GetScale(context.Background(), "api", metav1.GetOptions{})
	if sc.Spec.Replicas != 2 {
		t.Fatalf("replicas must not be written: %d", sc.Spec.Replicas)
	}
	c := meta.FindStatusCondition(e.hpa(t).Status.Conditions, "ReplicaOwnership")
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != "Conflict" {
		t.Fatalf("%+v", c)
	}
}

func TestLegacyHorizontalScalingYieldsToAKubeCostAutoscaler(t *testing.T) {
	kca := &v1alpha1.KubeCostAutoscaler{ObjectMeta: metav1.ObjectMeta{Name: "new", Namespace: "shop"},
		Spec: v1alpha1.KubeCostAutoscalerSpec{TargetRef: v1alpha1.WorkloadRef{Name: "api"}, MinReplicas: 1, MaxReplicas: 3}}
	e := newEnv(replicasHPA(), newDeployment(), newPod("api-1"), kca)
	e.r.Reconcile(context.Background(), req)
	if e.e.last.Replicas != nil {
		t.Fatal("KubeCostAutoscaler owns replicas now")
	}
	// without a competing writer the legacy resource keeps its behaviour
	e2 := newEnv(replicasHPA(), newDeployment(), newPod("api-1"))
	e2.r.Reconcile(context.Background(), req)
	if e2.e.last.Replicas == nil {
		t.Fatal("legacy horizontal scaling must still work when it is the only writer")
	}
	if c := meta.FindStatusCondition(e2.hpa(t).Status.Conditions, "ReplicaOwnership"); c == nil || c.Status != metav1.ConditionTrue {
		t.Fatalf("%+v", c)
	}
}
