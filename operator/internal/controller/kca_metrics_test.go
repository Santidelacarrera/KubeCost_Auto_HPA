package controller

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	v1alpha1 "github.com/santidelacarrera/kubecost_auto_hpa/operator/api/v1alpha1"
)

// value reads the current value of a counter or gauge series.
func value(m prometheus.Metric) float64 {
	var out dto.Metric
	if err := m.Write(&out); err != nil {
		panic(err)
	}
	if out.Counter != nil {
		return out.Counter.GetValue()
	}
	return out.Gauge.GetValue()
}

// histogramCount returns the sample count of a histogram series (0 when the series does not exist).
func histogramCount(t *testing.T, name string, want map[string]string) uint64 {
	t.Helper()
	fams, err := ctrlmetrics.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	var total uint64
	for _, f := range fams {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.GetMetric() {
			ok := true
			for _, l := range m.GetLabel() {
				if v, has := want[l.GetName()]; has && v != l.GetValue() {
					ok = false
				}
			}
			if ok {
				total += m.GetHistogram().GetSampleCount()
			}
		}
	}
	return total
}

func TestReconcileExposesTheOperationalMetrics(t *testing.T) {
	a := baseAutoscaler()
	e := newKCAEnv(t, a, 6)
	ns, name := a.Namespace, a.Name

	before := histogramCount(t, "kubecost_autoscaler_reconcile_duration_seconds", map[string]string{"result": "ok"})
	reasonBefore := value(mReasons.WithLabelValues("WithinTolerance", ns, name))
	costReqBefore := histogramCount(t, "kubecost_autoscaler_kubecost_request_duration_seconds", map[string]string{"outcome": "ok"})
	e.mustReconcile()
	if histogramCount(t, "kubecost_autoscaler_reconcile_duration_seconds", map[string]string{"result": "ok"}) != before+1 {
		t.Fatal("reconcile duration was not observed")
	}
	if histogramCount(t, "kubecost_autoscaler_kubecost_request_duration_seconds", map[string]string{"outcome": "ok"}) <= costReqBefore {
		t.Fatal("the KubeCost query latency was not observed")
	}
	// every decision is explained by a stable reason, so a refused reduction is visible
	if value(mReasons.WithLabelValues("WithinTolerance", ns, name)) <= reasonBefore {
		t.Fatal("decision reason counter not incremented")
	}
	if got := value(mDemandAge.WithLabelValues(ns, name)); got < 0 {
		t.Fatalf("demand metric age %v", got)
	}
	if value(mPaused.WithLabelValues(ns, name)) != 0 {
		t.Fatal("not paused")
	}
}

func TestPauseIsCountedOncePerActivation(t *testing.T) {
	a := baseAutoscaler()
	a.Annotations = map[string]string{v1alpha1.AnnotationPause: "true"}
	e := newKCAEnv(t, a, 6)
	ns, name := a.Namespace, a.Name
	pausesBefore := value(mPauses.WithLabelValues(ns, name))
	for i := 0; i < 3; i++ {
		e.advance(30 * time.Second)
		e.mustReconcile()
	}
	if got := value(mPauses.WithLabelValues(ns, name)) - pausesBefore; got != 1 {
		t.Fatalf("pause activations = %v, want 1 (not one per reconcile)", got)
	}
	if value(mPaused.WithLabelValues(ns, name)) != 1 {
		t.Fatal("paused gauge must be 1 while the annotation is set")
	}
	cur := e.get()
	delete(cur.Annotations, v1alpha1.AnnotationPause)
	if err := e.c.Update(context.Background(), &cur); err != nil {
		t.Fatal(err)
	}
	e.advance(30 * time.Second)
	e.mustReconcile()
	if value(mPaused.WithLabelValues(ns, name)) != 0 {
		t.Fatal("paused gauge must return to 0")
	}
}

func TestRecoveryAndFailureLatenciesAreObserved(t *testing.T) {
	a := baseAutoscaler()
	e := newKCAEnv(t, a, 6)
	recBefore := histogramCount(t, "kubecost_autoscaler_recovery_duration_seconds", nil)
	failBefore := histogramCount(t, "kubecost_autoscaler_kubecost_request_duration_seconds", map[string]string{"outcome": "transient"})

	e.costErr = transientErr()
	for i := 0; i < 4; i++ {
		e.advance(30 * time.Second)
		e.mustReconcile()
	}
	if histogramCount(t, "kubecost_autoscaler_kubecost_request_duration_seconds", map[string]string{"outcome": "transient"}) <= failBefore {
		t.Fatal("a failed KubeCost query must be observed under its error class")
	}
	e.costErr = nil
	e.advance(30 * time.Second)
	e.mustReconcile()
	if histogramCount(t, "kubecost_autoscaler_recovery_duration_seconds", nil) != recBefore+1 {
		t.Fatal("recovery time was not observed exactly once")
	}
}

// A replica change made by anyone else (a person, another autoscaler, a pipeline) skews the
// per-replica cost estimate exactly like one of ours: the cost signal must read as unstable for a
// full cost window, and therefore freeze non-safety moves, until the window has passed.
func TestReplicaChangeByAnotherActorMakesTheCostSignalUnstable(t *testing.T) {
	a := baseAutoscaler()
	a.Spec.MinReplicas = 2
	e := newKCAEnv(t, a, 6)
	e.pricePerReplica = "0.1"
	e.res.util = 0.40 // would call for a scale-down once stabilisation allows it

	e.mustReconcile()
	if c := e.get().Status.Cost; c == nil || !c.Stable {
		t.Fatalf("with no replica changes the cost signal is stable: %+v", c)
	}

	// someone else scales the Deployment (6 -> 7): the controller did not write that
	var dep appsv1.Deployment
	if err := e.c.Get(context.Background(), types.NamespacedName{Namespace: "production", Name: "payments-api"}, &dep); err != nil {
		t.Fatal(err)
	}
	n := int32(7)
	dep.Spec.Replicas = &n
	dep.Status.Replicas, dep.Status.ReadyReplicas = n, n
	if err := e.c.Update(context.Background(), &dep); err != nil {
		t.Fatal(err)
	}
	e.advance(30 * time.Second)
	e.mustReconcile()
	st := e.get().Status
	if st.LastReplicaChangeTime == nil {
		t.Fatal("the external change was not recorded")
	}
	if st.Cost == nil || st.Cost.Stable {
		t.Fatalf("the cost signal must be unstable right after an external replica change: %+v", st.Cost)
	}

	// still inside the cost window (15 min default): unstable
	e.advance(10 * time.Minute)
	e.mustReconcile()
	if c := e.get().Status.Cost; c == nil || c.Stable {
		t.Fatalf("still inside the cost window: %+v", c)
	}
	// past the window with no further change: stable again
	e.advance(10 * time.Minute)
	e.mustReconcile()
	if c := e.get().Status.Cost; c == nil || !c.Stable {
		t.Fatalf("after the window the signal must be stable again: %+v", c)
	}
}
