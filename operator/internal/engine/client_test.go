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
