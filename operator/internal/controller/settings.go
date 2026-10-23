package controller

import (
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/santidelacarrera/kubecost_auto_hpa/operator/api/v1alpha1"
	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/infra"
	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/money"
	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/policy"
)

// Settings is a KubeCostAutoscalerSpec resolved into validated runtime values.
type Settings struct {
	Cfg        policy.Config
	Resources  []corev1.ResourceName
	CostWindow time.Duration
	Cluster    string

	Interval, MaxMetricAge, ActivationDelay, RecoveryDelay time.Duration
	Mode                                                   string
	FallbackCPU                                            int32
	DryRun                                                 bool
	Classifier                                             *infra.Classifier
}

func dur(d metav1.Duration, def time.Duration) time.Duration {
	if d.Duration == 0 {
		return def
	}
	return d.Duration
}

func pct(v, def int32) float64 {
	if v == 0 {
		v = def
	}
	return float64(v) / 100
}

func pctPtr(v *int32, def int32) float64 {
	if v == nil {
		return float64(def) / 100
	}
	return float64(*v) / 100
}

func boolOr(b *bool, def bool) bool {
	if b == nil {
		return def
	}
	return *b
}

// Resolve applies the same defaults as the CRD (typed clients serialise zero
// values, which would otherwise bypass API-server defaulting) and validates.
func Resolve(spec *v1alpha1.KubeCostAutoscalerSpec) (Settings, error) {
	s := Settings{
		Interval:        dur(spec.EvaluationInterval, 30*time.Second),
		MaxMetricAge:    dur(spec.FailurePolicy.MaxMetricAge, 5*time.Minute),
		ActivationDelay: dur(spec.FailurePolicy.FallbackActivationDelay, 2*time.Minute),
		RecoveryDelay:   dur(spec.FailurePolicy.RecoveryDelay, 2*time.Minute),
		Mode:            spec.FailurePolicy.Mode,
		DryRun:          spec.DryRun,
		CostWindow:      15 * time.Minute,
	}
	s.FallbackCPU = spec.FailurePolicy.FallbackTargetCPUUtilizationPercent
	if s.FallbackCPU == 0 {
		s.FallbackCPU = 70
	}
	if s.FallbackCPU < 1 || s.FallbackCPU > 100 {
		return s, fmt.Errorf("failurePolicy.fallbackTargetCPUUtilizationPercent must be in [1,100]")
	}
	if s.Mode == "" {
		s.Mode = v1alpha1.FailureModeFreeze
	}
	if s.Mode != v1alpha1.FailureModeFreeze && s.Mode != v1alpha1.FailureModeResourceMetricsFallback {
		return s, fmt.Errorf("failurePolicy.mode %q is not supported", s.Mode)
	}
	if s.Interval < 5*time.Second {
		return s, fmt.Errorf("evaluationInterval must be at least 5s")
	}

	for _, r := range spec.Demand.Resources {
		s.Resources = append(s.Resources, corev1.ResourceName(r))
	}
	if len(s.Resources) == 0 {
		s.Resources = []corev1.ResourceName{corev1.ResourceCPU}
	}

	minHealthy := spec.MinReplicas
	if spec.MinHealthyReplicas != nil {
		minHealthy = *spec.MinHealthyReplicas
	}
	c := policy.Config{
		MinReplicas: spec.MinReplicas, MaxReplicas: spec.MaxReplicas, MinHealthy: minHealthy,
		TargetUtil: pct(spec.Demand.TargetUtilizationPercent, 70), Ceiling: pct(spec.Demand.CeilingUtilizationPercent, 90),
		CostUtil: pct(spec.Demand.CostPressureUtilizationPercent, 80), Tolerance: pctPtr(spec.Demand.TolerancePercent, 10),
		DownWindow:  dur(spec.Stabilization.DownscaleWindow, 10*time.Minute),
		UpWindow:    dur(spec.Stabilization.ScaleUpWindow, 30*time.Second),
		MinInterval: dur(spec.Stabilization.MinimumScaleActionInterval, 60*time.Second),
		Guardrail:   true, SpotBuffer: pctPtr(spec.InfrastructurePolicy.SpotInterruptionBufferPercent, 20),
		AllowSafetyScaleUp: boolOr(spec.FailurePolicy.AllowSafetyScaleUp, true),
		SpotMult:           money.One(), OnDemandMult: money.One(),
	}
	// Silence between evaluations longer than this restarts stabilisation.
	c.MaxGap = 6 * s.Interval
	if c.MaxGap < 3*time.Minute {
		c.MaxGap = 3 * time.Minute
	}

	var err error
	if v := spec.InfrastructurePolicy.SpotMultiplier; v != "" {
		if c.SpotMult, err = money.ParseRatio(v); err != nil {
			return s, fmt.Errorf("infrastructurePolicy.spotMultiplier: %w", err)
		}
	}
	if v := spec.InfrastructurePolicy.OnDemandMultiplier; v != "" {
		if c.OnDemandMult, err = money.ParseRatio(v); err != nil {
			return s, fmt.Errorf("infrastructurePolicy.onDemandMultiplier: %w", err)
		}
	}
	if cp := spec.CostPolicy; cp != nil {
		if cp.MaxHourlyCostUSD != "" {
			if c.MaxHourly, err = money.Parse(cp.MaxHourlyCostUSD); err != nil {
				return s, fmt.Errorf("costPolicy.maxHourlyCostUSD: %w", err)
			}
		}
		if cp.TargetHourlyCostUSD != "" {
			if c.TargetHourly, err = money.Parse(cp.TargetHourlyCostUSD); err != nil {
				return s, fmt.Errorf("costPolicy.targetHourlyCostUSD: %w", err)
			}
		}
		c.Guardrail = boolOr(cp.ScaleUpCostGuardrail, true)
		s.CostWindow = dur(cp.Window, 15*time.Minute)
		s.Cluster = cp.Cluster
		if s.CostWindow < time.Minute {
			return s, fmt.Errorf("costPolicy.window must be at least 1m")
		}
	}
	if err := c.Validate(); err != nil {
		return s, err
	}
	s.Cfg = c

	s.Classifier = infra.DefaultClassifier()
	if r := spec.InfrastructurePolicy.SpotNodeLabels; len(r) > 0 {
		s.Classifier.SpotRules = toRules(r)
	}
	if r := spec.InfrastructurePolicy.OnDemandNodeLabels; len(r) > 0 {
		s.Classifier.OnDemandRules = toRules(r)
	}
	return s, nil
}

func toRules(in []v1alpha1.NodeLabelRule) []infra.Rule {
	out := make([]infra.Rule, 0, len(in))
	for _, r := range in {
		out = append(out, infra.Rule{Key: r.Key, Values: r.Values})
	}
	return out
}
