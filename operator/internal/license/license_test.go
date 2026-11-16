package license

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/approval"
)

var now = time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)

func keys(t *testing.T) (priv []byte, pub string) {
	t.Helper()
	p, q, _, err := approval.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return p, string(q)
}

func issue(t *testing.T, priv []byte, l License) []byte {
	t.Helper()
	k, err := approval.ParsePrivate(priv)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Issue(k, l)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func lic(tier Tier) License {
	return License{ID: "L-1", Customer: "acme", Tier: tier, MaxNodes: 50, NotBefore: now.Add(-24 * time.Hour), Expires: now.Add(30 * 24 * time.Hour)}
}

func TestIssuedLicenceVerifies(t *testing.T) {
	priv, pub := keys(t)
	got, err := Verify(issue(t, priv, lic(Professional)), []string{pub}, now)
	if err != nil || got.Tier != Professional || got.Customer != "acme" || got.MaxNodes != 50 {
		t.Fatalf("%v %+v", err, got)
	}
}

func TestTamperingAndWrongKeysAreRefused(t *testing.T) {
	priv, pub := keys(t)
	_, otherPub := keys(t)
	good := issue(t, priv, lic(Professional))

	if _, err := Verify(good, []string{otherPub}, now); err == nil {
		t.Fatal("a licence signed by another key verified")
	}
	if _, err := Verify(good, nil, now); err == nil {
		t.Fatal("verified without any trusted key")
	}
	// raise the tier inside the payload, keep the signature
	var f File
	if err := json.Unmarshal(good, &f); err != nil {
		t.Fatal(err)
	}
	raw, _ := base64.StdEncoding.DecodeString(f.Payload)
	f.Payload = base64.StdEncoding.EncodeToString([]byte(strings.Replace(string(raw), `"professional"`, `"enterprise"`, 1)))
	forged, _ := json.Marshal(f)
	if _, err := Verify(forged, []string{pub}, now); err == nil || !strings.Contains(err.Error(), "does not verify") {
		t.Fatalf("an edited payload verified: %v", err)
	}
	for name, data := range map[string][]byte{"garbage": []byte("not json"), "empty": nil, "huge": make([]byte, maxFile+1)} {
		if _, err := Verify(data, []string{pub}, now); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestValidityPeriod(t *testing.T) {
	priv, pub := keys(t)
	data := issue(t, priv, lic(Enterprise))
	if _, err := Verify(data, []string{pub}, now.Add(31*24*time.Hour)); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("%v", err)
	}
	if _, err := Verify(data, []string{pub}, now.Add(-48*time.Hour)); err == nil || !strings.Contains(err.Error(), "not valid before") {
		t.Fatalf("%v", err)
	}
}

func TestIssueRejectsNonsense(t *testing.T) {
	priv, _ := keys(t)
	k, _ := approval.ParsePrivate(priv)
	bad := []License{
		{Customer: "a", Tier: Starter, NotBefore: now, Expires: now.Add(time.Hour)},
		{ID: "x", Customer: "a", Tier: "platinum", NotBefore: now, Expires: now.Add(time.Hour)},
		{ID: "x", Customer: "a", Tier: Starter, NotBefore: now, Expires: now},
		{ID: "x", Customer: "a", Tier: Starter, MaxNodes: -1, NotBefore: now, Expires: now.Add(time.Hour)},
	}
	for i, l := range bad {
		if _, err := Issue(k, l); err == nil {
			t.Errorf("case %d accepted", i)
		}
	}
}

// The table that matters: what each tier may do, and that enforcement only ever restricts.
func TestEnforceMapsTiersToModes(t *testing.T) {
	want := Requested{ObserveOnly: false, Applier: true} // the operator was configured to apply
	for name, c := range map[string]struct {
		l            License
		err          error
		nodes        int
		tier         Tier
		observe      bool
		applier      bool
		forceApprove bool
	}{
		"no licence":            {err: errors.New("no licence file"), tier: Starter, observe: true, applier: false, forceApprove: true},
		"starter":               {l: lic(Starter), tier: Starter, observe: true, applier: false, forceApprove: true},
		"professional":          {l: lic(Professional), tier: Professional, observe: false, applier: true, forceApprove: true},
		"enterprise":            {l: lic(Enterprise), tier: Enterprise, observe: false, applier: true, forceApprove: false},
		"enterprise over nodes": {l: lic(Enterprise), nodes: 51, tier: Starter, observe: true, applier: false, forceApprove: true},
		"enterprise at limit":   {l: lic(Enterprise), nodes: 50, tier: Enterprise, observe: false, applier: true, forceApprove: false},
	} {
		e := Enforce(want, c.l, c.err, c.nodes)
		if e.Tier != c.tier || e.ObserveOnly != c.observe || e.Applier != c.applier || e.ForceApproval != c.forceApprove {
			t.Errorf("%s: %+v", name, e)
		}
		if (e.ObserveOnly != want.ObserveOnly || e.Applier != want.Applier) && len(e.Reasons) == 0 {
			t.Errorf("%s: a restriction without a reason", name)
		}
	}
}

func TestEnforceNeverLoosensWhatWasConfigured(t *testing.T) {
	safe := Requested{ObserveOnly: true, Applier: false}
	for _, tier := range []Tier{Starter, Professional, Enterprise} {
		e := Enforce(safe, lic(tier), nil, 1)
		if !e.ObserveOnly || e.Applier {
			t.Fatalf("an %s licence turned writing on: %+v", tier, e)
		}
	}
}
