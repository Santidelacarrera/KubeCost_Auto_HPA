package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/approval"
	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/license"
)

func writeLicense(t *testing.T, tier license.Tier, maxNodes int, expires time.Time) (file, pub string) {
	t.Helper()
	priv, pubPEM, _, err := approval.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	key, _ := approval.ParsePrivate(priv)
	data, err := license.Issue(key, license.License{ID: "L-1", Customer: "acme", Tier: tier, MaxNodes: maxNodes, NotBefore: expires.Add(-90 * 24 * time.Hour), Expires: expires})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	file, pub = filepath.Join(dir, "license.json"), filepath.Join(dir, "license.pub")
	if err := os.WriteFile(file, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pub, pubPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	return file, pub
}

var (
	licNow    = time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	writing   = license.Requested{ObserveOnly: false, Applier: true}
	fiveNodes = func() int { return 5 }
)

// Without --enforce-license nothing changes, whatever the licence situation: existing installations keep their behaviour.
func TestWithoutEnforcementTheConfigurationIsUntouched(t *testing.T) {
	for name, st := range map[string]licenseState{
		"no licence at all": resolveLicense("", "", false, writing, fiveNodes, licNow),
		"unreadable file":   resolveLicense("/nonexistent/license.json", "/nonexistent/key.pub", false, writing, fiveNodes, licNow),
	} {
		if st.Effective.ObserveOnly || !st.Effective.Applier || st.Effective.ForceApproval || st.Enforced {
			t.Errorf("%s: configuration changed without enforcement: %+v", name, st)
		}
	}
	if st := resolveLicense("", "", false, writing, fiveNodes, licNow); st.Problem != "" {
		t.Errorf("no licence and no enforcement is not a problem to report: %q", st.Problem)
	}
	file, pub := writeLicense(t, license.Enterprise, 0, licNow.Add(24*time.Hour))
	st := resolveLicense(file, pub, false, writing, fiveNodes, licNow)
	if st.Tier != license.Enterprise || st.Customer != "acme" || st.Problem != "" {
		t.Errorf("a valid licence must be reported even when it is not enforced: %+v", st)
	}
}

func TestEnforcementFailsSafeToObserveOnly(t *testing.T) {
	for name, st := range map[string]licenseState{
		"no licence": resolveLicense("", "", true, writing, fiveNodes, licNow),
		"unreadable": resolveLicense("/nonexistent/license.json", "/nonexistent/key.pub", true, writing, fiveNodes, licNow),
	} {
		if !st.Effective.ObserveOnly || st.Effective.Applier || st.Tier != license.Starter || st.Problem == "" || len(st.Effective.Reasons) == 0 {
			t.Errorf("%s: %+v", name, st)
		}
	}
	file, pub := writeLicense(t, license.Enterprise, 0, licNow.Add(-time.Hour)) // expired an hour ago
	if st := resolveLicense(file, pub, true, writing, fiveNodes, licNow); !st.Effective.ObserveOnly || st.Effective.Applier {
		t.Errorf("an expired licence must drop to observe-only: %+v", st)
	}
}

func TestEnforcedTiers(t *testing.T) {
	file, pub := writeLicense(t, license.Professional, 10, licNow.Add(24*time.Hour))
	st := resolveLicense(file, pub, true, writing, fiveNodes, licNow)
	if st.Effective.ObserveOnly || !st.Effective.Applier || !st.Effective.ForceApproval || st.Tier != license.Professional {
		t.Errorf("professional applies with approval: %+v", st)
	}
	if st := resolveLicense(file, pub, true, writing, func() int { return 11 }, licNow); !st.Effective.ObserveOnly || st.Tier != license.Starter {
		t.Errorf("over the node limit: %+v", st)
	}
	file, pub = writeLicense(t, license.Enterprise, 0, licNow.Add(24*time.Hour))
	called := false
	st = resolveLicense(file, pub, true, writing, func() int { called = true; return 9999 }, licNow)
	if st.Effective.ObserveOnly || !st.Effective.Applier || st.Effective.ForceApproval {
		t.Errorf("enterprise may automate: %+v", st)
	}
	if called {
		t.Error("an unlimited licence must not need the node count")
	}
}
