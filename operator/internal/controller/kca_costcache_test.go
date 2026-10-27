package controller

import (
	"testing"
	"time"
)

// The cost lookup is cached per autoscaler for costCacheTTL, errors included, so a KubeCost
// outage costs one request per window instead of one per reconcile.
func TestCostErrorsAreCachedForOneWindowThenRetried(t *testing.T) {
	e := newKCAEnv(t, baseAutoscaler(), 4)
	e.costErr = transientErr()
	calls := func() int { e.cost.mu.Lock(); defer e.cost.mu.Unlock(); return e.cost.calls }

	e.mustReconcile()
	before := calls()
	if before == 0 {
		t.Fatal("setup: the first reconcile must query the cost provider")
	}
	for i := 0; i < 3; i++ { // all inside costCacheTTL (20s)
		e.now = e.now.Add(5 * time.Second) // not e.advance: that also clears the cache
		e.mustReconcile()
	}
	if got := calls(); got != before {
		t.Fatalf("errors were not cached: %d queries inside the TTL, want %d", got, before)
	}

	e.now = e.now.Add(costCacheTTL)
	e.mustReconcile()
	if got := calls(); got != before+1 {
		t.Fatalf("expected a retry once the TTL expired: %d queries, want %d", got, before+1)
	}
}
