package safety

import (
	"strings"
	"testing"
	"time"

	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/recommend"
)

var now = time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC) // a Monday

func rec() recommend.Recommendation {
	return recommend.Recommendation{
		ID: "abc", Namespace: "shop", Workload: "api", Kind: "Deployment", Container: "app", Confidence: recommend.ConfHigh,
		CPU:    recommend.ResourceResult{Action: recommend.Decrease, Current: 2, Recommended: 1},
		Memory: recommend.ResourceResult{Action: recommend.NoChange, Current: 1 << 30, Recommended: 1 << 30},
	}
}

func facts() Facts {
	return Facts{Namespace: "shop", Kind: "Deployment", Name: "api", Known: true, Approved: "abc"}
}

func allow() Policy { return Policy{AllowNamespaces: []string{"shop"}} }

func eval(p Policy, r recommend.Recommendation, f Facts) Decision {
	return Evaluate(p, r, f, now, nil)
}

func mustDeny(t *testing.T, name string, d Decision, sub string) {
	t.Helper()
	if d.Allowed || !strings.Contains(strings.Join(d.Reasons, " | "), sub) {
		t.Errorf("%s: want denial containing %q, got %+v", name, sub, d)
	}
}

func TestExplicitPolicyAndApprovalAllow(t *testing.T) {
	if d := eval(allow(), rec(), facts()); !d.Allowed || len(d.Reasons) != 0 {
		t.Fatalf("%+v", d)
	}
}

func TestEmptyPolicyDeniesEverything(t *testing.T) {
	mustDeny(t, "empty", eval(Policy{}, rec(), facts()), "allowNamespaces")
}

func TestKillSwitchOverridesEverything(t *testing.T) {
	p := allow()
	p.KillSwitch = true
	mustDeny(t, "kill", eval(p, rec(), facts()), "kill switch")
}

func TestApprovalRequiredByDefaultAndCanBeDisabledExplicitly(t *testing.T) {
	f := facts()
	f.Approved = ""
	d := eval(allow(), rec(), f)
	mustDeny(t, "no approval", d, "not approved")
	if !d.NeedsApproval {
		t.Fatal("must say approval is what is missing")
	}
	f.Approved = "other-id"
	mustDeny(t, "wrong id", eval(allow(), rec(), f), "not approved")
	off := false
	p := allow()
	p.RequireApproval = &off
	f.Approved = ""
	if d := eval(p, rec(), f); !d.Allowed {
		t.Fatalf("%+v", d)
	}
}

func TestFailClosedOnUnknownFactsAbstentionAndNoop(t *testing.T) {
	f := facts()
	f.Known = false
	mustDeny(t, "unknown", eval(allow(), rec(), f), "fail closed")
	r := rec()
	r.Abstained = true
	mustDeny(t, "abstained", eval(allow(), r, facts()), "abstained")
	r = rec()
	r.CPU.Action = recommend.NoChange
	mustDeny(t, "noop", eval(allow(), r, facts()), "changes nothing")
	f = facts()
	f.Name = "other"
	mustDeny(t, "mismatch", eval(allow(), rec(), f), "facts describe")
}

func TestAllowDenyListsAndLabels(t *testing.T) {
	p := allow()
	p.AllowWorkloads = []string{"shop/Deployment/web-*"}
	mustDeny(t, "not in allow list", eval(p, rec(), facts()), "allowWorkloads")
	p.AllowWorkloads = []string{"shop/Deployment/*"}
	p.DenyWorkloads = []string{"shop/Deployment/api"}
	mustDeny(t, "deny wins", eval(p, rec(), facts()), "denyWorkloads")
	p = allow()
	p.DenyLabels = map[string]string{"autoscaling.kubecost.io/exclude": "true"}
	f := facts()
	f.Labels = map[string]string{"autoscaling.kubecost.io/exclude": "true"}
	mustDeny(t, "label", eval(p, rec(), f), "exclusion label")
	f.Labels = map[string]string{"autoscaling.kubecost.io/exclude": "false"}
	if d := eval(p, rec(), f); !d.Allowed {
		t.Fatalf("a different label value must not exclude: %+v", d)
	}
}

func TestLimitsOnChangeSizeAndBounds(t *testing.T) {
	p := allow()
	p.MaxChangeFraction = 0.25
	mustDeny(t, "too big", eval(p, rec(), facts()), "maxChangeFraction")
	p = allow()
	p.MinCPU = 1.5
	mustDeny(t, "min", eval(p, rec(), facts()), "below the policy minimum")
	p = allow()
	r := rec()
	r.CPU.Action, r.CPU.Recommended = recommend.Increase, 3
	p.MaxCPU = 2.5
	mustDeny(t, "max", eval(p, r, facts()), "above the policy maximum")
}

func TestConfidenceFloor(t *testing.T) {
	r := rec()
	r.Confidence = recommend.ConfLow
	mustDeny(t, "low", eval(allow(), r, facts()), "confidence")
	p := allow()
	p.MinConfidence = "Low"
	if d := eval(p, r, facts()); !d.Allowed {
		t.Fatalf("%+v", d)
	}
}

func TestMaintenanceWindows(t *testing.T) {
	p := allow()
	p.Windows = []Window{{Days: []string{"Mon"}, Start: "13:00", End: "15:00"}}
	mustDeny(t, "outside", eval(p, rec(), facts()), "maintenance window")
	p.Windows = []Window{{Days: []string{"Mon"}, Start: "11:00", End: "13:00"}}
	if d := eval(p, rec(), facts()); !d.Allowed {
		t.Fatalf("%+v", d)
	}
	p.Windows = []Window{{Days: []string{"Tue"}, Start: "11:00", End: "13:00"}}
	mustDeny(t, "wrong day", eval(p, rec(), facts()), "maintenance window")
	// wraps midnight: Sunday 22:00-02:00 covers Monday 01:00
	p.Windows = []Window{{Days: []string{"Sun"}, Start: "22:00", End: "02:00"}}
	at := time.Date(2026, 11, 9, 1, 0, 0, 0, time.UTC)
	if d := Evaluate(p, rec(), facts(), at, nil); !d.Allowed {
		t.Fatalf("%+v", d)
	}
	if d := Evaluate(p, rec(), facts(), at.Add(2*time.Hour), nil); d.Allowed {
		t.Fatal("03:00 is outside the wrapped window")
	}
}

func TestRateLimit(t *testing.T) {
	recent := []time.Time{now.Add(-10 * time.Minute)}
	mustDeny(t, "rate", Evaluate(allow(), rec(), facts(), now, recent), "rate limit")
	if d := Evaluate(allow(), rec(), facts(), now, []time.Time{now.Add(-2 * time.Hour)}); !d.Allowed {
		t.Fatalf("an old change must not count: %+v", d)
	}
	p := allow()
	p.MaxChanges, p.Per = 3, "1h"
	if d := Evaluate(p, rec(), facts(), now, []time.Time{now.Add(-time.Minute), now.Add(-2 * time.Minute)}); !d.Allowed {
		t.Fatalf("%+v", d)
	}
}

func TestGitOpsHPAVPAIdempotencyAndRollbackMemory(t *testing.T) {
	f := facts()
	f.GitOps = "argocd"
	mustDeny(t, "gitops", eval(allow(), rec(), f), "argocd")
	p := allow()
	p.AllowGitOps = true
	if d := eval(p, rec(), f); !d.Allowed {
		t.Fatalf("%+v", d)
	}
	f = facts()
	f.HPAOnCPU = true
	mustDeny(t, "hpa", eval(allow(), rec(), f), "HPA")
	r := rec()
	r.CPU.Action = recommend.NoChange
	r.Memory = recommend.ResourceResult{Action: recommend.Decrease, Current: 2 << 30, Recommended: 1 << 30}
	if d := eval(allow(), r, f); !d.Allowed {
		t.Fatalf("an HPA on CPU must not block a memory-only change: %+v", d)
	}
	f = facts()
	f.VPAAuto = true
	mustDeny(t, "vpa", eval(allow(), rec(), f), "VPA")
	f = facts()
	f.AlreadyApplied = "abc"
	mustDeny(t, "dup", eval(allow(), rec(), f), "already applied")
	f = facts()
	f.RolledBack = "abc"
	mustDeny(t, "rolled back", eval(allow(), rec(), f), "rolled back")
}

func TestInvalidPolicyDenies(t *testing.T) {
	for _, p := range []Policy{
		{AllowNamespaces: []string{"shop"}, MaxChangeFraction: 1.5},
		{AllowNamespaces: []string{"shop"}, MinCPU: 5, MaxCPU: 1},
		{AllowNamespaces: []string{"shop"}, MinConfidence: "Maybe"},
		{AllowNamespaces: []string{"shop"}, AllowWorkloads: []string{"["}},
		{AllowNamespaces: []string{"shop"}, Windows: []Window{{Start: "25:00", End: "01:00"}}},
		{AllowNamespaces: []string{"shop"}, Windows: []Window{{Days: []string{"Funday"}, Start: "01:00", End: "02:00"}}},
	} {
		mustDeny(t, "invalid", eval(p, rec(), facts()), "invalid policy")
	}
}

func TestApproversAllowList(t *testing.T) {
	p := allow()
	p.Approvers = []string{"alice@example.com"}
	f := facts()
	f.ApprovedBy = "mallory@example.com"
	d := eval(p, rec(), f)
	mustDeny(t, "wrong approver", d, "not in approvers")
	f.ApprovedBy = ""
	mustDeny(t, "no stamp", eval(p, rec(), f), "not in approvers")
	f.ApprovedBy = "alice@example.com"
	if d := eval(p, rec(), f); !d.Allowed {
		t.Fatalf("%+v", d)
	}
	// without an approvers list the stamp is informational
	if d := eval(allow(), rec(), f); !d.Allowed {
		t.Fatalf("%+v", d)
	}
}
