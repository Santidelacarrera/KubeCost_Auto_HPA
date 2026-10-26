package sim

import (
	"math"
	"time"

	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/money"
	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/policy"
)

// DefaultWorkload is the fixed assumption set used by every published result.
func DefaultWorkload() Workload {
	return Workload{PerPodRPS: 100, StartupDelay: 30 * time.Second, PricePerHour: 0.05, MinReplicas: 2, MaxReplicas: 60}
}

// Scenarios are the demand patterns (requests per second).
func Scenarios() []Scenario {
	h := time.Hour
	m := time.Minute
	return []Scenario{
		{"steady", 2 * h, func(time.Duration) float64 { return 1500 }},
		{"ramp-up-and-down", 2 * h, func(t time.Duration) float64 {
			switch {
			case t < 40*m:
				return 500 + 2500*float64(t)/float64(40*m)
			case t < 80*m:
				return 3000
			default:
				return 3000 - 2500*math.Min(1, float64(t-80*m)/float64(40*m))
			}
		}},
		{"burst", 2 * h, func(t time.Duration) float64 {
			if (t >= 30*m && t < 35*m) || (t >= 75*m && t < 80*m) {
				return 3200
			}
			return 800
		}},
		{"gradual-decline", 2 * h, func(t time.Duration) float64 {
			if t < 20*m {
				return 3000
			}
			return math.Max(500, 3000-2500*float64(t-20*m)/float64(60*m))
		}},
		{"intermittent", 2 * h, func(t time.Duration) float64 {
			if (t/(10*m))%2 == 0 {
				return 400
			}
			return 2000
		}},
	}
}

func usd(s string) money.USD {
	v, err := money.Parse(s)
	if err != nil {
		panic(err)
	}
	return v
}

func kcaConfig(w Workload, cost bool) policy.Config {
	c := policy.Config{
		MinReplicas: w.MinReplicas, MaxReplicas: w.MaxReplicas, MinHealthy: w.MinReplicas,
		TargetUtil: 0.70, Ceiling: 0.90, CostUtil: 0.80, Tolerance: 0.10,
		DownWindow: 10 * time.Minute, UpWindow: 30 * time.Second, MinInterval: time.Minute, MaxGap: 3 * time.Minute,
		SpotMult: money.One(), OnDemandMult: money.One(), SpotBuffer: 0.2, AllowSafetyScaleUp: true,
	}
	if cost {
		// 20 replicas = $1.00/h target, 40 replicas = $2.00/h budget at $0.05/replica-hour.
		c.TargetHourly, c.MaxHourly, c.Guardrail = usd("1.00"), usd("2.00"), true
	}
	return c
}

// Scalers returns fresh scalers (they are stateful) for one run.
func Scalers(w Workload) []Scaler {
	return []Scaler{
		&HPA{Label: "HPA default (down 5m)", Target: 0.70, Tolerance: 0.10, DownWindow: 5 * time.Minute, Min: w.MinReplicas, Max: w.MaxReplicas},
		&HPA{Label: "HPA (down 10m)", Target: 0.70, Tolerance: 0.10, DownWindow: 10 * time.Minute, Min: w.MinReplicas, Max: w.MaxReplicas},
		&KCA{Label: "KCA demand-only", Cfg: kcaConfig(w, false)},
		&KCA{Label: "KCA with cost policy", Cfg: kcaConfig(w, true), UseCost: true},
	}
}
