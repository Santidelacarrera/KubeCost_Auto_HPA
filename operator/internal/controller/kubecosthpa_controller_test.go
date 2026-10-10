package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	v1alpha1 "github.com/santidelacarrera/kubecost_auto_hpa/operator/api/v1alpha1"
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
