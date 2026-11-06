package stability

import (
	"testing"
	"time"
)

func TestBoundaryOscillationPrevented(t *testing.T) {
	cfg := DefaultConfig()
	cfg.UpThreshold = 0.80
	cfg.DownThreshold = 0.50
	cfg.HysteresisBand = 0.05
	cfg.SmoothingAlpha = 0.30
	cfg.MinDwellTime = 30 * time.Second
	cfg.GrowCooldown = 30 * time.Second
	cfg.ShrinkCooldown = 2 * time.Minute

	ctrl := NewController(cfg)
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)

	// Simulate utilization oscillating around the 0.80 threshold: 0.78, 0.82, 0.77, 0.83, 0.79, 0.81
	signals := []float64{0.78, 0.82, 0.77, 0.83, 0.79, 0.81, 0.78, 0.82}
	approvedCount := 0
	curCPU := 1000.0

	for i, sig := range signals {
		now = now.Add(45 * time.Second)
		proposed := curCPU
		if sig > 0.80 {
			proposed = curCPU * 1.25
		} else {
			proposed = curCPU * 0.80
		}
		v := ctrl.Evaluate("prod/checkout", now, sig, curCPU, proposed, false)
		if v.Allowed {
			approvedCount++
			curCPU = proposed
		}
		if v.Explanation == "" {
			t.Fatalf("step %d: expected non-empty explanation", i)
		}
	}

	if approvedCount > 1 {
		t.Fatalf("expected boundary jitter around threshold to be suppressed by EMA + hysteresis, got %d resizes", approvedCount)
	}
}

func TestOscillationTripsCircuitBreakerAndAdaptiveBackoff(t *testing.T) {
	cfg := DefaultConfig()
	cfg.UpThreshold = 0.75
	cfg.DownThreshold = 0.45
	cfg.HysteresisBand = 0.01
	cfg.SmoothingAlpha = 1.0 // raw signal pass-through to force rapid swings
	cfg.MinDwellTime = 10 * time.Second
	cfg.GrowCooldown = 10 * time.Second
	cfg.ShrinkCooldown = 10 * time.Second
	cfg.MaxChangesPerWindow = 10
	cfg.OscillationThreshold = 2
	cfg.BaseBackoff = 2 * time.Minute

	ctrl := NewController(cfg)
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)

	// 1. Grow: 1000 -> 1500
	v1 := ctrl.Evaluate("prod/api", now, 0.90, 1000, 1500, false)
	if !v1.Allowed {
		t.Fatalf("expected initial grow to be allowed, got %s: %s", v1.ReasonCode, v1.Explanation)
	}

	// 2. Shrink: 1500 -> 1000 (reversal #1)
	now = now.Add(30 * time.Second)
	v2 := ctrl.Evaluate("prod/api", now, 0.25, 1500, 1000, false)
	if !v2.Allowed {
		t.Fatalf("expected first shrink to be allowed, got %s: %s", v2.ReasonCode, v2.Explanation)
	}

	// 3. Grow again: 1000 -> 1500 (reversal #2 -> hits OscillationThreshold=2, trips CircuitOpen!)
	now = now.Add(30 * time.Second)
	v3 := ctrl.Evaluate("prod/api", now, 0.92, 1000, 1500, false)
	if v3.Allowed {
		t.Fatalf("expected third alternating change to trip oscillation circuit breaker")
	}
	if v3.CircuitState != CircuitOpen || v3.ReasonCode != "OscillationDetected" {
		t.Fatalf("expected CircuitOpen and OscillationDetected, got state=%s reason=%s", v3.CircuitState, v3.ReasonCode)
	}

	// 4. Subsequent attempt 30s later should still see CircuitOpen with remaining backoff
	now = now.Add(30 * time.Second)
	v4 := ctrl.Evaluate("prod/api", now, 0.92, 1000, 1500, false)
	if v4.Allowed || v4.ReasonCode != "CircuitBreakerOpen" {
		t.Fatalf("expected CircuitBreakerOpen during backoff, got allowed=%v reason=%s", v4.Allowed, v4.ReasonCode)
	}
}

func TestAsymmetricCooldownAndMinVariation(t *testing.T) {
	cfg := DefaultConfig()
	cfg.SmoothingAlpha = 1.0
	cfg.MinVariationPercent = 0.10 // 10% minimum change

	ctrl := NewController(cfg)
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)

	// Tiny 3% change should be rejected by MinVariationPercent
	vSmall := ctrl.Evaluate("prod/worker", now, 0.90, 1000, 1030, false)
	if vSmall.Allowed || vSmall.ReasonCode != "BelowMinimumVariation" {
		t.Fatalf("expected BelowMinimumVariation for 3%% change, got %v (%s)", vSmall.Allowed, vSmall.ReasonCode)
	}

	// Valid 25% grow should pass
	vGrow := ctrl.Evaluate("prod/worker", now, 0.90, 1000, 1250, false)
	if !vGrow.Allowed {
		t.Fatalf("expected 25%% grow to pass, got %s", vGrow.ReasonCode)
	}

	// Attempt shrink after 2 minutes (past 60s MinDwellTime, but before 5m ShrinkCooldown)
	now = now.Add(2 * time.Minute)
	vShrinkEarly := ctrl.Evaluate("prod/worker", now, 0.20, 1250, 900, false)
	if vShrinkEarly.Allowed || vShrinkEarly.ReasonCode != "AsymmetricCooldownActive" {
		t.Fatalf("expected AsymmetricCooldownActive for shrink at 2m, got %v (%s)", vShrinkEarly.Allowed, vShrinkEarly.ReasonCode)
	}

	// After 5m10s total, shrink should be allowed
	now = now.Add(3*time.Minute + 10*time.Second)
	vShrinkOK := ctrl.Evaluate("prod/worker", now, 0.20, 1250, 900, false)
	if !vShrinkOK.Allowed {
		t.Fatalf("expected shrink after 5m10s to be allowed, got %s: %s", vShrinkOK.ReasonCode, vShrinkOK.Explanation)
	}
}
