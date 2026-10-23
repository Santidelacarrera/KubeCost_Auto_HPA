package controller

import (
	"context"

	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/engine"
	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/metrics"
)

type spend float64

func (s spend) MonthlySpend(context.Context, string) (float64, error) { return float64(s), nil }

func engineDecision(action string, cpu int64, frozen bool, spot *bool) engine.Decision {
	return engine.Decision{Action: action, CPUMillicores: cpu, BudgetFrozen: frozen, SpotEligible: spot, Reason: "test"}
}

type metricsFunc func(metrics.Workload)

func (f metricsFunc) Snapshot(_ context.Context, w metrics.Workload) (metrics.Snapshot, error) {
	f(w)
	return metrics.Snapshot{RPS: 100, P99Ms: 50}, nil
}

func ptr[T any](v T) *T { return &v }
