package controller

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Stabilisation timestamps written in the future (clock skew, a hand-edited or
// restored status) must not freeze scaling indefinitely. The controller treats a
// future timestamp as "happened now", so the window still ends within CostWindow.
func TestFutureStabilisationTimestampCannotFreezeScaling(t *testing.T) {
	a := baseAutoscaler()
	a.Spec.CostPolicy = nil // demand-only: the scale-down is driven by utilisation
	e := newKCAEnv(t, a, 10)
	e.res.util = 0.35 // needs 5 replicas

	obj := e.get()
	future := metav1.NewTime(e.now.Add(2 * time.Hour))
	obj.Status.LastScaleTime = &future
	obj.Status.LastReplicaChangeTime = &future
	if err := e.c.Status().Update(context.Background(), &obj); err != nil {
		t.Fatal(err)
	}

	start := e.now
	var scaledAt time.Duration = -1
	for i := 0; i <= 40; i++ {
		e.mustReconcile()
		if len(e.scl.writes) > 0 {
			scaledAt = e.now.Sub(start)
			break
		}
		e.advance(30 * time.Second)
	}
	if scaledAt < 0 {
		t.Fatalf("a future stabilisation timestamp froze scaling for %v", e.now.Sub(start))
	}
	if scaledAt < 10*time.Minute || scaledAt > 11*time.Minute {
		t.Fatalf("scaled down after %v; want within [10m,11m] (window bounded by now)", scaledAt)
	}
	if e.replicas() != 5 {
		t.Fatalf("replicas=%d, want 5", e.replicas())
	}
}
