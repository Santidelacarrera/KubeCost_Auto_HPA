package engine

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestEvaluate(t *testing.T) {
	var got Request
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&got)
		w.Write([]byte(`{"action":"shrink","cpu_millicores":800,"reason":"x","cost_per_request":0.001,"cost_per_request_after":null,"budget_frozen":false,"spot_eligible":true}`))
	}))
	defer s.Close()
	d, err := NewHTTP(s.URL).Evaluate(context.Background(), Request{CPUMillicores: 1000, P99Ms: 50, SLOP99Ms: 200})
	if err != nil {
		t.Fatal(err)
	}
	if d.Action != "shrink" || d.CPUMillicores != 800 || d.CostPerRequestAfter != nil || d.SpotEligible == nil || !*d.SpotEligible {
		t.Fatalf("%+v", d)
	}
	if got.CPUMillicores != 1000 || got.SLOP99Ms != 200 {
		t.Fatalf("request not forwarded: %+v", got)
	}
}

func TestEvaluateError(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"bad"}`, 400)
	}))
	defer s.Close()
	if _, err := NewHTTP(s.URL).Evaluate(context.Background(), Request{}); err == nil {
		t.Fatal("expected error")
	}
}

func TestEvaluatePredictiveAndOOM(t *testing.T) {
	var got Request
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&got)
		w.Write([]byte(`{
			"action": "hold",
			"cpu_millicores": 1500,
			"reason": "predictive hold",
			"predictive": {
				"predicted_replicas": 8,
				"reactive_replicas": 4,
				"recommended_replicas": 8,
				"is_prewarming": true,
				"confidence_score": 0.95,
				"reason": "Pre-warmed ahead of diurnal rush"
			},
			"oom_protection": {
				"is_at_risk": true,
				"risk_level": "CRITICAL",
				"recommended_limit_mib": 3500,
				"recommended_request_mib": 2500,
				"headroom_percentage": 11.2,
				"action": "inflate_limit",
				"reason": "Inflated limit before OOM"
			}
		}`))
	}))
	defer s.Close()

	req := Request{
		CPUMillicores: 1500,
		Predictive: &PredictiveReq{
			WarmupLeadTimeSeconds: 120,
			TargetRPSPerPod:       50.0,
			RPSHistory:            []float64{100, 180, 260},
		},
		OOMProtection: &OOMProtectionReq{
			CriticalThreshold: 0.88,
			UsageHistoryMiB:   []float64{1000, 1500, 2800},
		},
	}
	d, err := NewHTTP(s.URL).Evaluate(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if d.Predictive == nil || !d.Predictive.IsPrewarming || d.Predictive.RecommendedReplicas != 8 {
		t.Fatalf("unexpected predictive decision: %+v", d.Predictive)
	}
	if d.OOMProtection == nil || !d.OOMProtection.IsAtRisk || d.OOMProtection.RecommendedLimitMiB != 3500 {
		t.Fatalf("unexpected oom decision: %+v", d.OOMProtection)
	}
}
