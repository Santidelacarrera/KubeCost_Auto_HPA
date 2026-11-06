package execution

import "testing"

func TestExecutionModesAndKillSwitch(t *testing.T) {
	g := DefaultGuardrails()
	p := Proposal{
		WorkloadKey:      "prod/checkout",
		CurrentCPUMilli:  1000,
		ProposedCPUMilli: 1500,
		CurrentMemMiB:    1024,
		ProposedMemMiB:   1536,
		CurrentReplicas:  3,
		ProposedReplicas: 5,
		IsFromSaaS:       true,
	}

	// 1. EMERGENCY_STOP blocks everything and reverts to current
	resStop := Enforce(ModeEmergencyStop, g, p)
	if resStop.ShouldExecute || resStop.ClampedCPUMilli != 1000 || resStop.ClampedReplicas != 3 {
		t.Fatalf("expected EMERGENCY_STOP to block execution and preserve current values, got %+v", resStop)
	}

	// 2. OBSERVE mode skips evaluation and execution
	resObs := Enforce(ModeObserve, g, p)
	if resObs.ShouldEvaluate || resObs.ShouldExecute {
		t.Fatalf("expected OBSERVE to disable evaluation and execution, got %+v", resObs)
	}

	// 3. SHADOW mode evaluates and audits, but does not execute
	resShadow := Enforce(ModeShadow, g, p)
	if !resShadow.ShouldEvaluate || resShadow.ShouldExecute {
		t.Fatalf("expected SHADOW to evaluate without executing, got %+v", resShadow)
	}

	// 4. CANARY mode blocks workloads not in the allow-list and allows canary workloads
	g.CanaryAllowedWorkloads = map[string]bool{"canary/payment": true}
	resNonCanary := Enforce(ModeCanary, g, p)
	if resNonCanary.ShouldExecute {
		t.Fatalf("expected non-canary workload to be blocked in CANARY mode")
	}
	pCanary := p
	pCanary.WorkloadKey = "canary/payment"
	resCanary := Enforce(ModeCanary, g, pCanary)
	if !resCanary.ShouldExecute {
		t.Fatalf("expected canary workload to execute in CANARY mode")
	}
}

func TestLocalGuardrailsOverrideSaaSProposal(t *testing.T) {
	g := DefaultGuardrails()
	g.MaxIncreasePercent = 0.50 // max +50% step
	g.MaxDecreasePercent = 0.20 // max -20% step
	g.MaxCPUMillicores = 3000
	g.MinCPUMillicores = 250

	// SaaS proposes jumping CPU from 1000m to 4000m (+300%)
	pGrow := Proposal{
		WorkloadKey:      "prod/api",
		CurrentCPUMilli:  1000,
		ProposedCPUMilli: 4000,
		CurrentMemMiB:    1024,
		ProposedMemMiB:   1024,
		CurrentReplicas:  2,
		ProposedReplicas: 2,
		IsFromSaaS:       true,
	}
	resGrow := Enforce(ModeActive, g, pGrow)
	if !resGrow.ClampedByLocal || resGrow.ClampedCPUMilli != 1500 {
		t.Fatalf("expected local guardrail to clamp SaaS +300%% jump to 1500m (+50%%), got %dm", resGrow.ClampedCPUMilli)
	}

	// SaaS proposes shrinking CPU from 1000m to 400m (-60%)
	pShrink := Proposal{
		WorkloadKey:      "prod/api",
		CurrentCPUMilli:  1000,
		ProposedCPUMilli: 400,
		IsFromSaaS:       true,
	}
	resShrink := Enforce(ModeActive, g, pShrink)
	if !resShrink.ClampedByLocal || resShrink.ClampedCPUMilli != 800 {
		t.Fatalf("expected local guardrail to clamp SaaS -60%% shrink to 800m (-20%%), got %dm", resShrink.ClampedCPUMilli)
	}
}
