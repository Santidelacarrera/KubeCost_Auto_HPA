package controller

import (
	"context"

	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/engine"
)

type spend float64

func (s spend) MonthlySpend(context.Context, string) (float64, error) { return float64(s), nil }

func engineDecision(action string, cpu int64, frozen bool, spot *bool) engine.Decision {
	return engine.Decision{Action: action, CPUMillicores: cpu, BudgetFrozen: frozen, SpotEligible: spot, Reason: "test"}
}
