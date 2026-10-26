package controller

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/yaml"

	v1alpha1 "github.com/santidelacarrera/kubecost_auto_hpa/operator/api/v1alpha1"
	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/actuator"
	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/kubecost"
	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/ownership"
	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/resmetrics"
)

func fallbackMode(a *v1alpha1.KubeCostAutoscaler) {
	a.Spec.FailurePolicy.Mode = v1alpha1.FailureModeResourceMetricsFallback
}

func TestResolveAppliesCRDDefaultsToZeroValues(t *testing.T) {
	s, err := Resolve(&baseAutoscaler().Spec)
	if err != nil {
		t.Fatal(err)
	}
	c := s.Cfg
	if c.TargetUtil != 0.7 || c.Ceiling != 0.9 || c.CostUtil != 0.8 || c.Tolerance != 0.1 || c.DownWindow != 10*time.Minute ||
		c.UpWindow != 30*time.Second || c.MinInterval != time.Minute || c.MinHealthy != 2 || !c.Guardrail || !c.AllowSafetyScaleUp ||
		s.MaxMetricAge != 5*time.Minute || s.Interval != 30*time.Second || s.Mode != v1alpha1.FailureModeFreeze || s.CostWindow != 15*time.Minute ||
		len(s.Resources) != 1 || c.SpotBuffer != 0.2 {
		t.Fatalf("%+v", s)
	}
	zero := int32(0)
	a := baseAutoscaler()
	a.Spec.Demand.TolerancePercent, a.Spec.InfrastructurePolicy.SpotInterruptionBufferPercent = &zero, &zero
	s, _ = Resolve(&a.Spec)
	if s.Cfg.Tolerance != 0 || s.Cfg.SpotBuffer != 0 {
		t.Fatal("an explicit 0 must be honoured, not replaced by the default")
	}
}

func TestResolveRejectsInvalidSpecs(t *testing.T) {
	for name, mut := range map[string]func(*v1alpha1.KubeCostAutoscalerSpec){
		"bad budget":       func(s *v1alpha1.KubeCostAutoscalerSpec) { s.CostPolicy.MaxHourlyCostUSD = "lots" },
		"negative target":  func(s *v1alpha1.KubeCostAutoscalerSpec) { s.CostPolicy.TargetHourlyCostUSD = "-1" },
		"target above max": func(s *v1alpha1.KubeCostAutoscalerSpec) { s.CostPolicy.TargetHourlyCostUSD = "9" },
		"zero multiplier":  func(s *v1alpha1.KubeCostAutoscalerSpec) { s.InfrastructurePolicy.SpotMultiplier = "0" },
		"bad multiplier":   func(s *v1alpha1.KubeCostAutoscalerSpec) { s.InfrastructurePolicy.OnDemandMultiplier = "x" },
		"max<min":          func(s *v1alpha1.KubeCostAutoscalerSpec) { s.MaxReplicas = 1 },
		"bad mode":         func(s *v1alpha1.KubeCostAutoscalerSpec) { s.FailurePolicy.Mode = "Yolo" },
		"util order":       func(s *v1alpha1.KubeCostAutoscalerSpec) { s.Demand.TargetUtilizationPercent = 95 },
		"tiny window":      func(s *v1alpha1.KubeCostAutoscalerSpec) { s.CostPolicy.Window = metav1.Duration{Duration: time.Second} },
		"tiny interval": func(s *v1alpha1.KubeCostAutoscalerSpec) {
			s.EvaluationInterval = metav1.Duration{Duration: time.Second}
		},
	} {
		a := baseAutoscaler()
		mut(&a.Spec)
		if _, err := Resolve(&a.Spec); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestInvalidSpecDoesNothing(t *testing.T) {
	a := baseAutoscaler()
	a.Spec.CostPolicy.MaxHourlyCostUSD = "lots"
	e := newKCAEnv(t, a, 6)
	e.mustReconcile()
	e.requireCond("Ready", metav1.ConditionFalse, "InvalidSpec")
	if len(e.scl.writes) != 0 || e.cost.calls != 0 {
		t.Fatal("an invalid spec must not trigger any I/O or writes")
	}
}

func TestHappyPathStatusAndConditions(t *testing.T) {
	e := newKCAEnv(t, baseAutoscaler(), 6)
	e.res.util = 0.7 // within tolerance: nothing to do
	res := e.mustReconcile()
	if res.RequeueAfter != 30*time.Second {
		t.Fatalf("requeue %v", res.RequeueAfter)
	}
	a := e.get()
	if a.Status.CurrentReplicas != 6 || a.Status.DesiredReplicas != 6 || a.Status.Utilization != "70.0" || a.Status.ScalingOwner != v1alpha1.OwnerOperator ||
		a.Status.LastDecision == nil || a.Status.LastDecision.Type != "Hold" || a.Status.LastSuccessfulReconcileTime == nil || a.Status.ObservedGeneration != 1 {
		t.Fatalf("%+v", a.Status)
	}
	if a.Status.Cost == nil || a.Status.Cost.HourlyUSD != "3.000000" || !a.Status.Cost.Stable || a.Status.Infrastructure.OnDemand != 6 {
		t.Fatalf("%+v", a.Status.Cost)
	}
	e.requireCond("Ready", metav1.ConditionTrue, "Reconciled")
	e.requireCond("MetricsAvailable", metav1.ConditionTrue, "Fresh")
	e.requireCond("Degraded", metav1.ConditionFalse, "Healthy")
	e.requireCond("ScalingActive", metav1.ConditionTrue, "Owner")
	if len(e.scl.writes) != 0 {
		t.Fatal("no write expected")
	}
}

func TestSafetyScaleUpIsAppliedEvenOverBudget(t *testing.T) {
	// scenario 9: budget exceeded ($12/h vs $5/h) while under heavy demand
	e := newKCAEnv(t, baseAutoscaler(), 12)
	e.pricePerReplica = "1"
	e.res.util = 0.95
	e.mustReconcile()
	if len(e.scl.writes) != 1 || e.scl.writes[0].To != 13 || e.replicas() != 13 {
		t.Fatalf("overloaded workload must scale to its safety size even over budget: %+v", e.scl.writes)
	}
	a := e.get()
	if a.Status.LastScaleTime == nil || a.Status.LastScaleFrom != 12 || a.Status.LastScaleTo != 13 || !a.Status.Cost.BudgetExceeded || !a.Status.Cost.BudgetLimited {
		t.Fatalf("%+v %+v", a.Status, a.Status.Cost)
	}
}

func TestDryRunReportsButNeverWrites(t *testing.T) {
	a := baseAutoscaler()
	a.Spec.DryRun = true
	e := newKCAEnv(t, a, 4)
	e.pricePerReplica = "0.2"
	e.res.util = 0.97
	e.mustReconcile()
	st := e.get().Status
	if len(e.scl.writes) != 0 || e.replicas() != 4 {
		t.Fatal("dry-run must not write")
	}
	if st.LastDecision.Type != "ScaleUp" || !st.LastDecision.DryRun || !strings.HasPrefix(st.LastDecision.Message, "dry-run") || st.ScalingOwner != v1alpha1.OwnerNone || st.LastScaleTime != nil {
		t.Fatalf("%+v", st)
	}
	e.requireCond("ScalingActive", metav1.ConditionFalse, "DryRun")
}

// The emergency stop: an annotation halts every write at once and is lifted without a restart.
func TestPauseAnnotationStopsAllWritesAndResumesWhenRemoved(t *testing.T) {
	a := baseAutoscaler()
	a.Annotations = map[string]string{v1alpha1.AnnotationPause: "true"}
	e := newKCAEnv(t, a, 4)
	e.pricePerReplica = "0.2"
	e.res.util = 0.97 // would trigger a safety scale-up
	e.mustReconcile()
	st := e.get().Status
	if len(e.scl.writes) != 0 || e.replicas() != 4 {
		t.Fatal("a paused autoscaler must not write replicas")
	}
	if st.LastDecision.Type != "ScaleUp" || !strings.HasPrefix(st.LastDecision.Message, "paused: would scale 4 ->") || st.ScalingOwner != v1alpha1.OwnerNone {
		t.Fatalf("the decision must still be explained while paused: %+v", st)
	}
	e.requireCond("ScalingActive", metav1.ConditionFalse, "Paused")

	// Lift the stop: the very next reconcile acts, with no restart.
	cur := e.get()
	delete(cur.Annotations, v1alpha1.AnnotationPause)
	if err := e.c.Update(context.Background(), &cur); err != nil {
		t.Fatal(err)
	}
	e.advance(time.Minute)
	e.mustReconcile()
	if len(e.scl.writes) == 0 || e.replicas() <= 4 {
		t.Fatalf("scaling must resume once the annotation is removed (writes %v)", e.scl.writes)
	}
}

func TestPauseDoesNotTouchAnActiveFallbackHPA(t *testing.T) {
	a := baseAutoscaler()
	fallbackMode(a)
	e := newKCAEnv(t, a, 6)
	e.costErr = transientErr()
	for i := 0; i < 15; i++ {
		e.advance(20 * time.Second)
		e.mustReconcile()
	}
	if _, found := e.hpa(); !found {
		t.Fatal("fallback HPA expected before pausing")
	}
	cur := e.get()
	cur.Annotations = map[string]string{v1alpha1.AnnotationPause: "true"}
	if err := e.c.Update(context.Background(), &cur); err != nil {
		t.Fatal(err)
	}
	e.costErr = nil // recovery would normally remove the fallback HPA after the recovery delay
	for i := 0; i < 15; i++ {
		e.advance(30 * time.Second)
		e.mustReconcile()
	}
	if _, found := e.hpa(); !found {
		t.Fatal("pausing must leave the running fallback HPA alone: the workload keeps its autoscaler")
	}
}

func TestScaleDownOnlyAfterStabilisationWindowAndSurvivesRestart(t *testing.T) {
	a := baseAutoscaler()
	a.Spec.CostPolicy = nil // demand-only
	e := newKCAEnv(t, a, 10)
	e.res.util = 0.35 // needs 5 replicas
	var scaledAt time.Duration = -1
	start := e.now
	for i := 0; i <= 40; i++ {
		e.mustReconcile()
		if len(e.scl.writes) > 0 {
			scaledAt = e.now.Sub(start)
			break
		}
		e.advance(30 * time.Second)
		if i == 8 { // restart mid-window (scenario 7): only API objects survive
			e.restart()
		}
	}
	if scaledAt < 10*time.Minute || scaledAt > 11*time.Minute {
		t.Fatalf("scaled down after %v; want within [10m,11m] despite the restart", scaledAt)
	}
	if e.replicas() != 5 {
		t.Fatalf("replicas=%d", e.replicas())
	}
}

func TestRestartDuringCooldownDoesNotRescale(t *testing.T) {
	a := baseAutoscaler()
	a.Spec.CostPolicy = nil
	e := newKCAEnv(t, a, 4)
	e.res.util = 0.97
	e.mustReconcile() // safety scale-up 4 -> 5 (bypasses cooldown by design)
	if len(e.scl.writes) != 1 {
		t.Fatal("setup")
	}
	e.restart()
	e.advance(20 * time.Second)
	e.res.util = 0.8 // non-emergency: needs 6 replicas
	e.mustReconcile()
	if len(e.scl.writes) != 1 {
		t.Fatalf("cooldown must survive a restart: %+v", e.scl.writes)
	}
	e.advance(61 * time.Second)
	e.mustReconcile()
	e.advance(31 * time.Second)
	e.mustReconcile()
	if e.replicas() <= 5 {
		t.Fatalf("expected a later scale-up, replicas=%d", e.replicas())
	}
}

// ---- failure scenarios 1-8 ----

func TestKubeCostOutageFreezesThenRecovers(t *testing.T) { // scenario 1
	e := newKCAEnv(t, baseAutoscaler(), 10)
	e.pricePerReplica = "0.2"
	e.res.util = 0.2 // would normally scale down a lot
	e.mustReconcile()
	e.costErr = transientErr()
	for i := 0; i < 4; i++ { // 30 seconds of outage in 10s steps
		e.advance(10 * time.Second)
		e.mustReconcile()
		if e.replicas() != 10 {
			t.Fatalf("replicas changed during the outage: %d", e.replicas())
		}
	}
	e.requireCond("MetricsAvailable", metav1.ConditionFalse, "CostTransient")
	e.requireCond("Degraded", metav1.ConditionTrue, "Frozen")
	a := e.get()
	if a.Status.Failure.ConsecutiveFailures == 0 || a.Status.Failure.LastErrorClass != "kubecost_transient" || a.Status.LastDecision.Type != "InvalidMetrics" {
		t.Fatalf("%+v", a.Status)
	}
	e.costErr = nil
	e.advance(10 * time.Second)
	e.mustReconcile()
	e.requireCond("Degraded", metav1.ConditionFalse, "Healthy")
	if e.get().Status.Failure.ConsecutiveFailures != 0 {
		t.Fatal("failure counters must reset on recovery")
	}
}

func TestFailureClassesAllFreeze(t *testing.T) { // scenarios 2, 3, 4
	for name, tc := range map[string]struct {
		err    error
		mod    func(*kubecost.Sample)
		reason string
	}{
		"http 500":       {err: &kubecost.Error{Class: kubecost.ClassTransient, Status: 500, Err: errors.New("server error")}, reason: "CostTransient"},
		"malformed json": {err: &kubecost.Error{Class: kubecost.ClassMalformed, Err: errors.New("not json")}, reason: "CostMalformed"},
		"auth":           {err: &kubecost.Error{Class: kubecost.ClassAuth, Status: 401, Err: errors.New("denied")}, reason: "CostAuth"},
		"no data":        {err: &kubecost.Error{Class: kubecost.ClassNoData, Err: errors.New("absent")}, reason: "CostNoData"},
		"stale data":     {mod: func(s *kubecost.Sample) { s.WindowEnd = s.WindowEnd.Add(-30 * time.Minute) }, reason: "CostStale"},
		"partial window": {mod: func(s *kubecost.Sample) { s.Minutes = 2 }, reason: "CostStale"},
	} {
		t.Run(name, func(t *testing.T) {
			e := newKCAEnv(t, baseAutoscaler(), 10)
			e.pricePerReplica = "0.2"
			e.res.util = 0.2
			e.costErr, e.costMod = tc.err, tc.mod
			for i := 0; i < 5; i++ {
				e.mustReconcile()
				e.advance(30 * time.Second)
			}
			if e.replicas() != 10 || len(e.scl.writes) != 0 {
				t.Fatalf("cost failure must never authorise a scale-down: %+v", e.scl.writes)
			}
			e.requireCond("MetricsAvailable", metav1.ConditionFalse, tc.reason)
			if got := e.get().Status.LastDecision.Type; got != "InvalidMetrics" {
				t.Fatalf("decision %s", got)
			}
		})
	}
}

func TestFreezeStillAllowsAuthorisedSafetyScaleUp(t *testing.T) {
	e := newKCAEnv(t, baseAutoscaler(), 4)
	e.costErr = transientErr()
	e.res.util = 0.97
	e.mustReconcile()
	if len(e.scl.writes) != 1 || e.scl.writes[0].To <= 4 {
		t.Fatalf("authorised safety scale-up expected: %+v", e.scl.writes)
	}
	f := false
	a := baseAutoscaler()
	a.Spec.FailurePolicy.AllowSafetyScaleUp = &f
	e2 := newKCAEnv(t, a, 4)
	e2.costErr = transientErr()
	e2.res.util = 0.97
	e2.mustReconcile()
	if len(e2.scl.writes) != 0 {
		t.Fatal("safety scale-up must need explicit authorisation")
	}
}

func TestKubernetesAPIOutageDoesNothingAndRecovers(t *testing.T) { // scenario 5
	failing := true
	e := newKCAEnv(t, baseAutoscaler(), 10, func(e *kcaEnv, _ *[]client.Object, f *interceptor.Funcs) {
		f.Get = func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*appsv1.Deployment); ok && failing {
				return apierrors.NewServiceUnavailable("etcd leader changed")
			}
			return c.Get(ctx, key, obj, opts...)
		}
	})
	e.res.util = 0.2
	for i := 0; i < 3; i++ {
		if _, err := e.reconcile(); err == nil {
			t.Fatal("transient API errors must be returned so controller-runtime backs off")
		}
	}
	if len(e.scl.writes) != 0 || e.cost.calls != 0 {
		t.Fatal("no decisions without a readable target")
	}
	e.requireCond("Ready", metav1.ConditionFalse, "TargetUnreadable")
	failing = false
	e.mustReconcile()
	e.requireCond("Ready", metav1.ConditionTrue, "Reconciled")
}

func TestDemandMetricsUnavailableHolds(t *testing.T) {
	e := newKCAEnv(t, baseAutoscaler(), 10)
	e.res.err = resmetrics.ErrUnavailable
	for i := 0; i < 3; i++ {
		e.mustReconcile()
		e.advance(30 * time.Second)
	}
	if len(e.scl.writes) != 0 {
		t.Fatal("no demand signal, no scaling")
	}
	e.requireCond("MetricsAvailable", metav1.ConditionFalse, "DemandUnavailable")
	if e.get().Status.Failure.LastErrorClass != "resource_metrics" {
		t.Fatal("failure class")
	}
}

func TestTargetDeletedIsHandled(t *testing.T) { // scenario 8
	e := newKCAEnv(t, baseAutoscaler(), 6)
	if err := e.c.Delete(context.Background(), paymentsDeployment(6)); err != nil {
		t.Fatal(err)
	}
	res := e.mustReconcile()
	e.requireCond("Ready", metav1.ConditionFalse, "TargetNotFound")
	e.requireCond("ScalingActive", metav1.ConditionFalse, "TargetNotFound")
	if res.RequeueAfter == 0 || e.get().Status.ScalingOwner != v1alpha1.OwnerNone || len(e.scl.writes) != 0 {
		t.Fatal("must keep watching without writing")
	}
	// the Deployment comes back: normal operation resumes
	if err := e.c.Create(context.Background(), paymentsDeployment(6)); err != nil {
		t.Fatal(err)
	}
	e.mustReconcile()
	e.requireCond("Ready", metav1.ConditionTrue, "Reconciled")
}

func TestScaledToZeroPausesAutoscaling(t *testing.T) {
	e := newKCAEnv(t, baseAutoscaler(), 0)
	e.mustReconcile()
	e.requireCond("ScalingActive", metav1.ConditionFalse, "ScaledToZero")
	if len(e.scl.writes) != 0 {
		t.Fatal("a Deployment scaled to zero is not revived (HPA convention)")
	}
}

// ---- scale-write failures ----

func TestScaleFailureRevertsIntentAndRetries(t *testing.T) {
	e := newKCAEnv(t, baseAutoscaler(), 4)
	e.pricePerReplica = "0.2"
	e.res.util = 0.97
	e.scl.fail = errors.New("admission webhook denied")
	e.mustReconcile()
	a := e.get()
	if a.Status.LastScaleTime != nil {
		t.Fatal("a failed write must not start the cooldown")
	}
	e.requireCond("ScalingActive", metav1.ConditionFalse, "ScaleFailed")
	e.scl.fail = nil
	e.advance(10 * time.Second)
	e.mustReconcile()
	if len(e.scl.writes) != 1 {
		t.Fatalf("retry expected: %+v", e.scl.writes)
	}
	e.requireCond("ScalingActive", metav1.ConditionTrue, "Owner")
}

func TestReplicaDriftIsDetected(t *testing.T) {
	e := newKCAEnv(t, baseAutoscaler(), 4)
	e.pricePerReplica = "0.2"
	e.res.util = 0.97
	e.scl.fail = actuatorDrift()
	e.mustReconcile()
	e.requireCond("ScalingActive", metav1.ConditionFalse, "ReplicaDrift")
}

func actuatorDrift() error {
	return errors.Join(actuator.ErrReplicaDrift, errors.New("expected 4, found 6"))
}

func TestStatusConflictsAreRetried(t *testing.T) {
	conflicts := 0
	e := newKCAEnv(t, baseAutoscaler(), 6, func(e *kcaEnv, _ *[]client.Object, f *interceptor.Funcs) {
		f.SubResourceUpdate = func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			if conflicts < 2 {
				conflicts++
				return apierrors.NewConflict(schema.GroupResource{Group: "autoscaling.kubecost.io", Resource: "kubecostautoscalers"}, obj.GetName(), errors.New("the object has been modified"))
			}
			return c.SubResource(sub).Update(ctx, obj, opts...)
		}
	})
	e.mustReconcile()
	if conflicts != 2 || e.get().Status.LastDecision == nil {
		t.Fatalf("conflicts=%d: status must be written after retrying", conflicts)
	}
}

func TestSteadyStateDoesNotSpamEvents(t *testing.T) {
	e := newKCAEnv(t, baseAutoscaler(), 6)
	e.mustReconcile()
	e.events() // first-time conditions
	for i := 0; i < 20; i++ {
		e.advance(30 * time.Second)
		e.mustReconcile()
	}
	if evs := e.events(); len(evs) != 0 {
		t.Fatalf("a steady state must be silent, got %v", evs)
	}
}

// ---- ownership: the single-writer guarantee ----

func hpaFor(name, target string) *autoscalingv2.HorizontalPodAutoscaler {
	min := int32(1)
	return &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "production"},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{MinReplicas: &min, MaxReplicas: 5,
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{APIVersion: "apps/v1", Kind: "Deployment", Name: target}},
	}
}

func withObjects(o ...client.Object) envOpt {
	return func(_ *kcaEnv, objs *[]client.Object, _ *interceptor.Funcs) { *objs = append(*objs, o...) }
}

func TestNeverWritesReplicasWhileAnIndependentHPAIsActive(t *testing.T) {
	e := newKCAEnv(t, baseAutoscaler(), 4, withObjects(hpaFor("team-hpa", "payments-api")))
	e.pricePerReplica = "0.2"
	for i := 0; i < 30; i++ { // overloaded, then idle, for a long time
		e.res.util = 0.99
		if i >= 15 {
			e.res.util = 0.05
		}
		e.mustReconcile()
		e.advance(time.Minute)
	}
	if len(e.scl.writes) != 0 {
		t.Fatalf("competing writers: %+v", e.scl.writes)
	}
	e.requireCond("Ready", metav1.ConditionFalse, "Conflict")
	e.requireCond("ScalingActive", metav1.ConditionFalse, "Conflict")
	if e.get().Status.ScalingOwner != v1alpha1.OwnerNone || e.cost.calls != 0 {
		t.Fatal("a conflicted autoscaler must not even query KubeCost")
	}
	if _, found := e.hpa(); found {
		t.Fatal("no fallback HPA may be created next to an existing HPA")
	}
}

func TestHPAForAnotherDeploymentIsNotAConflict(t *testing.T) {
	e := newKCAEnv(t, baseAutoscaler(), 4, withObjects(hpaFor("other", "something-else")))
	e.res.util = 0.99
	e.mustReconcile()
	if len(e.scl.writes) != 1 {
		t.Fatal("unrelated HPAs must not block scaling")
	}
}

func TestLegacyKubeCostHPAWithReplicasIsAConflict(t *testing.T) {
	legacy := &v1alpha1.KubeCostHPA{ObjectMeta: metav1.ObjectMeta{Name: "legacy", Namespace: "production"},
		Spec: v1alpha1.KubeCostHPASpec{TargetRef: v1alpha1.TargetRef{Name: "payments-api"}, Replicas: &v1alpha1.ReplicaSpec{Min: 1, Max: 3}}}
	e := newKCAEnv(t, baseAutoscaler(), 4, withObjects(legacy))
	e.res.util = 0.99
	e.mustReconcile()
	if len(e.scl.writes) != 0 {
		t.Fatal("the legacy resource owns replicas")
	}
	e.requireCond("ScalingActive", metav1.ConditionFalse, "Conflict")
	// ...but a legacy resource that only resizes vertically is fine
	vertical := legacy.DeepCopy()
	vertical.Spec.Replicas = nil
	e2 := newKCAEnv(t, baseAutoscaler(), 4, withObjects(vertical))
	e2.res.util = 0.99
	e2.mustReconcile()
	if len(e2.scl.writes) != 1 {
		t.Fatal("vertical-only KubeCostHPA must coexist")
	}
}

func TestOnlyOneOfTwoAutoscalersOwnsTheTarget(t *testing.T) {
	younger := baseAutoscaler()
	younger.Name, younger.UID = "zz-younger", "uid-2"
	younger.CreationTimestamp = metav1.NewTime(time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC))
	e := newKCAEnv(t, baseAutoscaler(), 4, withObjects(younger))
	e.res.util = 0.99
	e.mustReconcile() // the older one (payments-autoscaler) wins
	if len(e.scl.writes) != 1 {
		t.Fatal("the older autoscaler must own the target")
	}
	res, err := e.r.Reconcile(context.Background(), reqFor("zz-younger"))
	if err != nil {
		t.Fatal(err)
	}
	_ = res
	var y v1alpha1.KubeCostAutoscaler
	_ = e.c.Get(context.Background(), types.NamespacedName{Namespace: "production", Name: "zz-younger"}, &y)
	if len(e.scl.writes) != 1 || y.Status.ScalingOwner != v1alpha1.OwnerNone {
		t.Fatalf("the younger one must stand down: writes=%+v owner=%q", e.scl.writes, y.Status.ScalingOwner)
	}
}

func TestSameCreationTimeBreaksTiesByName(t *testing.T) {
	b := baseAutoscaler()
	b.Name, b.UID = "aaa-first", "uid-3" // sorts before payments-autoscaler
	e := newKCAEnv(t, baseAutoscaler(), 4, withObjects(b))
	e.res.util = 0.99
	e.mustReconcile()
	if len(e.scl.writes) != 0 {
		t.Fatal("tie-break by name: aaa-first wins")
	}
}

// ---- fallback ----

func TestFallbackActivationOwnershipAndRecovery(t *testing.T) { // scenarios 1+6 happy path of the fallback
	a := baseAutoscaler()
	fallbackMode(a)
	e := newKCAEnv(t, a, 6)
	e.pricePerReplica = "0.2"
	e.res.util = 0.99
	e.mustReconcile() // healthy: safety scale-up 6 -> 7
	writesBefore := len(e.scl.writes)

	e.costErr = &kubecost.Error{Class: kubecost.ClassTransient, Status: 500, Err: errors.New("server error")}
	for i := 0; i < 3; i++ { // 0..60s: shorter than the 2m activation delay
		e.advance(20 * time.Second)
		e.mustReconcile()
	}
	if _, found := e.hpa(); found {
		t.Fatal("a short outage must not hand control to the HPA")
	}
	for i := 0; i < 8; i++ { // past the activation delay
		e.advance(20 * time.Second)
		e.mustReconcile()
	}
	h, found := e.hpa()
	if !found {
		t.Fatal("fallback HPA expected after the activation delay")
	}
	if h.Spec.ScaleTargetRef.Name != "payments-api" || *h.Spec.MinReplicas != 2 || h.Spec.MaxReplicas != 20 ||
		*h.Spec.Metrics[0].Resource.Target.AverageUtilization != 70 || !metav1.IsControlledBy(h, ptrObj(e.get())) {
		t.Fatalf("%+v", h.Spec)
	}
	a2 := e.get()
	if !a2.Status.Fallback.Active || a2.Status.ScalingOwner != v1alpha1.OwnerFallbackHPA || a2.Status.LastDecision.Type != "Fallback" {
		t.Fatalf("%+v", a2.Status)
	}
	e.requireCond("ScalingActive", metav1.ConditionFalse, "FallbackActive")
	e.requireCond("Degraded", metav1.ConditionTrue, "FallbackActive")

	// while the HPA owns scaling this controller never writes replicas, whatever the load
	for i := 0; i < 20; i++ {
		e.res.util = []float64{0.99, 0.05}[i%2]
		e.advance(20 * time.Second)
		e.mustReconcile()
	}
	if len(e.scl.writes) != writesBefore {
		t.Fatalf("competing writers during fallback: %+v", e.scl.writes[writesBefore:])
	}

	// recovery needs RecoveryDelay of valid data
	e.costErr = nil
	e.res.util = 0.7
	e.advance(20 * time.Second)
	e.mustReconcile()
	if _, found := e.hpa(); !found {
		t.Fatal("must not hand back control on the first healthy sample")
	}
	for i := 0; i < 8; i++ {
		e.advance(20 * time.Second)
		e.mustReconcile()
	}
	if _, found := e.hpa(); found {
		t.Fatal("fallback HPA must be removed after recovery")
	}
	a3 := e.get()
	if a3.Status.Fallback.Active || a3.Status.ScalingOwner != v1alpha1.OwnerOperator {
		t.Fatalf("%+v", a3.Status)
	}
	e.requireCond("Degraded", metav1.ConditionFalse, "")
}

func TestFallbackRequiresWorkingResourceMetrics(t *testing.T) { // scenario 6
	a := baseAutoscaler()
	fallbackMode(a)
	e := newKCAEnv(t, a, 6)
	e.res.availErr = resmetrics.ErrUnavailable // metrics-server not installed
	e.costErr = transientErr()
	for i := 0; i < 15; i++ {
		e.advance(20 * time.Second)
		e.mustReconcile()
	}
	if _, found := e.hpa(); found {
		t.Fatal("an HPA without a metrics pipeline would be useless: stay frozen instead")
	}
	e.requireCond("Degraded", metav1.ConditionTrue, "FallbackUnavailable")
	if len(e.scl.writes) != 0 || e.replicas() != 6 {
		t.Fatal("frozen")
	}
}

func TestFallbackNotActivatedInDryRun(t *testing.T) {
	a := baseAutoscaler()
	fallbackMode(a)
	a.Spec.DryRun = true
	e := newKCAEnv(t, a, 6)
	e.costErr = transientErr()
	for i := 0; i < 15; i++ {
		e.advance(20 * time.Second)
		e.mustReconcile()
	}
	if _, found := e.hpa(); found {
		t.Fatal("dry-run must not create objects")
	}
	if got := e.get().Status.LastDecision; got.Type != "Fallback" || !got.DryRun {
		t.Fatalf("%+v", got)
	}
}

func TestForeignObjectWithTheFallbackNameIsNeverTouched(t *testing.T) {
	a := baseAutoscaler()
	fallbackMode(a)
	foreign := hpaFor(ownership.HPAName("payments-autoscaler"), "unrelated")
	e := newKCAEnv(t, a, 6, withObjects(foreign))
	e.costErr = transientErr()
	for i := 0; i < 15; i++ {
		e.advance(20 * time.Second)
		e.reconcile()
	}
	h, _ := e.hpa()
	if h.Spec.ScaleTargetRef.Name != "unrelated" || len(h.OwnerReferences) != 0 {
		t.Fatalf("a foreign HPA must be left alone: %+v", h.Spec)
	}
	if len(e.scl.writes) != 0 {
		t.Fatal("no writes")
	}
}

func TestFallbackHPASurvivesStatusLoss(t *testing.T) {
	a := baseAutoscaler()
	fallbackMode(a)
	e := newKCAEnv(t, a, 6)
	e.costErr = transientErr()
	for i := 0; i < 15; i++ {
		e.advance(20 * time.Second)
		e.mustReconcile()
	}
	if _, found := e.hpa(); !found {
		t.Fatal("setup: fallback expected")
	}
	// lose the status entirely (e.g. restored from a backup) while the HPA exists
	cur := e.get()
	cur.Status = v1alpha1.KubeCostAutoscalerStatus{}
	if err := e.c.Status().Update(context.Background(), &cur); err != nil {
		t.Fatal(err)
	}
	e.res.util = 0.99
	e.costErr = nil
	e.restart()
	e.advance(20 * time.Second)
	e.mustReconcile()
	if len(e.scl.writes) != 0 {
		t.Fatalf("the operator must adopt the existing fallback HPA instead of writing replicas next to it: %+v", e.scl.writes)
	}
	if !e.get().Status.Fallback.Active {
		t.Fatal("status must be rebuilt from the cluster")
	}
}

func TestSwitchingBackToFreezeReleasesTheFallbackHPA(t *testing.T) {
	a := baseAutoscaler()
	fallbackMode(a)
	e := newKCAEnv(t, a, 6)
	e.costErr = transientErr()
	for i := 0; i < 15; i++ {
		e.advance(20 * time.Second)
		e.mustReconcile()
	}
	cur := e.get()
	cur.Spec.FailurePolicy.Mode = v1alpha1.FailureModeFreeze
	if err := e.c.Update(context.Background(), &cur); err != nil {
		t.Fatal(err)
	}
	e.mustReconcile()
	if _, found := e.hpa(); found {
		t.Fatal("changing the policy to Freeze must remove the fallback HPA immediately")
	}
}

// ---- infrastructure ----

func TestSpotPlacementIsReportedAndRaisesRequiredCapacity(t *testing.T) {
	a := baseAutoscaler()
	a.Spec.CostPolicy = nil
	odEnv := newKCAEnv(t, a, 6)
	odEnv.res.util = 0.6
	odEnv.mustReconcile()

	spot := baseAutoscaler()
	spot.Spec.CostPolicy = nil
	se := newKCAEnv(t, spot, 6, func(e *kcaEnv, objs *[]client.Object, _ *interceptor.Funcs) {
		kept := (*objs)[:0]
		for _, o := range *objs {
			if p, ok := o.(interface{ GetNamespace() string }); ok && p.GetNamespace() == "production" && o.GetName() != "payments-api" && o.GetName() != "payments-autoscaler" {
				continue // drop the on-demand pods
			}
			kept = append(kept, o)
		}
		*objs = append(kept, paymentsPods(6, "spot-1")...)
	})
	se.res.util = 0.6
	se.mustReconcile()
	st := se.get().Status
	if st.Infrastructure.Spot != 6 || st.Infrastructure.OnDemand != 0 {
		t.Fatalf("%+v", st.Infrastructure)
	}
	if odEnv.get().Status.LastDecision.CandidateReplicas >= st.LastDecision.CandidateReplicas {
		t.Fatalf("spot-only (cand=%d) must require more replicas than on-demand (cand=%d)",
			st.LastDecision.CandidateReplicas, odEnv.get().Status.LastDecision.CandidateReplicas)
	}
}

func TestUnknownNodesAreCountedUnknown(t *testing.T) {
	e := newKCAEnv(t, baseAutoscaler(), 3, func(e *kcaEnv, objs *[]client.Object, _ *interceptor.Funcs) {
		*objs = append(*objs, paymentsPods(0, "")...)
	})
	cur := e.get()
	_ = cur
	var pods []client.Object
	for _, p := range paymentsPods(3, "missing-node") {
		p.SetName("x-" + p.GetName())
		pods = append(pods, p)
	}
	for _, p := range pods {
		if err := e.c.Create(context.Background(), p); err != nil {
			t.Fatal(err)
		}
	}
	e.mustReconcile()
	if got := e.get().Status.Infrastructure.Unknown; got != 3 {
		t.Fatalf("unknown=%d", got)
	}
}

func TestPendingPodsBlockScaling(t *testing.T) {
	e := newKCAEnv(t, baseAutoscaler(), 4)
	e.res.util = 0.97
	p := paymentsPods(1, "")[0]
	p.SetName("pending-pod")
	if err := e.c.Create(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	e.mustReconcile()
	if len(e.scl.writes) != 0 || e.get().Status.LastDecision.Reason != "PendingPods" {
		t.Fatalf("%+v", e.get().Status.LastDecision)
	}
}

func reqFor(name string) reconcileRequest {
	return reconcileRequest{NamespacedName: types.NamespacedName{Namespace: "production", Name: name}}
}

func ptrObj(a v1alpha1.KubeCostAutoscaler) *v1alpha1.KubeCostAutoscaler { return &a }

func loadSample(t *testing.T) *v1alpha1.KubeCostAutoscaler {
	t.Helper()
	raw, err := os.ReadFile("../../config/samples/autoscaling_v1alpha1_kubecostautoscaler.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var a v1alpha1.KubeCostAutoscaler
	if err := yaml.UnmarshalStrict(raw, &a); err != nil { // strict: unknown fields in the sample are errors
		t.Fatalf("the sample does not match the API types: %v", err)
	}
	return &a
}

func TestSampleManifestMatchesTheAPIAndResolves(t *testing.T) {
	a := loadSample(t)
	s, err := Resolve(&a.Spec)
	if err != nil {
		t.Fatal(err)
	}
	if !s.DryRun || s.Cfg.MaxHourly.String() != "5.000000" || s.Cfg.TargetHourly.String() != "4.000000" || s.Cfg.MinReplicas != 3 {
		t.Fatalf("%+v", s)
	}
}
