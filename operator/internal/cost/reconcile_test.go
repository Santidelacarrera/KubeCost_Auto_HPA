package cost

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/money"
)

const focusHead = "ChargeCategory,ResourceId,ResourceName,BilledCost,EffectiveCost,ListCost,BillingCurrency,ChargePeriodStart,ChargePeriodEnd\n"

// bill builds a 7-day FOCUS export starting at `start` (YYYY-MM-DD): one node row per day plus the extra lines given.
func bill(t *testing.T, start string, days int, billedPerDay, listPerDay, currency string, extra ...string) PeriodBill {
	t.Helper()
	t0, err := time.Parse("2006-01-02", start)
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	b.WriteString(focusHead)
	for d := 0; d < days; d++ {
		from, to := t0.AddDate(0, 0, d), t0.AddDate(0, 0, d+1)
		b.WriteString("Usage,//compute/projects/p/zones/z/instances/gke-prod-pool-1,gke-prod-pool-1," + billedPerDay + "," + billedPerDay + "," + listPerDay + "," + currency + "," +
			from.Format(time.RFC3339) + "," + to.Format(time.RFC3339) + "\n")
	}
	for _, e := range extra {
		b.WriteString(e + "\n")
	}
	p, err := FocusTotals(strings.NewReader(b.String()), "")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func amt(t *testing.T, s string) money.USD {
	t.Helper()
	neg := strings.HasPrefix(s, "-")
	v, err := money.Parse(strings.TrimPrefix(s, "-"))
	if err != nil {
		t.Fatal(err)
	}
	if neg {
		return -v
	}
	return v
}

func TestFocusTotalsKeepsEveryCategoryApart(t *testing.T) {
	p := bill(t, "2026-10-01", 7, "10.00", "12.50", "USD",
		"Tax,,,13.30,13.30,,USD,2026-10-01T00:00:00Z,2026-10-08T00:00:00Z",
		"Credit,,,-5.00,-5.00,,USD,2026-10-01T00:00:00Z,2026-10-08T00:00:00Z",
		"Usage,,,2.00,2.00,2.00,USD,2026-10-03T00:00:00Z,2026-10-04T00:00:00Z") // support: no resource
	if p.Currency != "USD" || p.days() != 7 || p.Rows != 10 {
		t.Fatalf("%+v", p)
	}
	if p.BilledUsage != amt(t, "72.00") || p.ListUsage != amt(t, "89.50") || p.NoResource != amt(t, "2.00") {
		t.Fatalf("usage totals: billed %s list %s noResource %s", p.BilledUsage, p.ListUsage, p.NoResource)
	}
	if p.Other["Tax"] != amt(t, "13.30") || p.Other["Credit"] != amt(t, "-5.00") {
		t.Fatalf("other: %v", p.Other)
	}
}

func TestFocusTotalsScopeFilter(t *testing.T) {
	csv := focusHead +
		"Usage,//compute/projects/p/zones/z/instances/gke-prod-pool-1,gke-prod-pool-1,10,10,10,USD,2026-10-01T00:00:00Z,2026-10-02T00:00:00Z\n" +
		"Usage,//compute/projects/p/zones/z/instances/build-agent,build-agent,40,40,40,USD,2026-10-01T00:00:00Z,2026-10-02T00:00:00Z\n"
	p, err := FocusTotals(strings.NewReader(csv), "GKE-PROD")
	if err != nil || p.BilledUsage != amt(t, "10") || p.OutOfScope != amt(t, "40") {
		t.Fatalf("%v %+v", err, p)
	}
	if _, err := FocusTotals(strings.NewReader(csv), "nothing-matches"); err == nil {
		t.Fatal("an empty scope result must be an error, not a zero bill")
	}
}

func TestFocusTotalsRefusesMixedCurrenciesAndBadRows(t *testing.T) {
	mixed := focusHead +
		"Usage,a,a,10,10,10,USD,2026-10-01T00:00:00Z,2026-10-02T00:00:00Z\n" +
		"Usage,b,b,10,10,10,EUR,2026-10-01T00:00:00Z,2026-10-02T00:00:00Z\n"
	if _, err := FocusTotals(strings.NewReader(mixed), ""); err == nil || !strings.Contains(err.Error(), "mixes billing currencies") {
		t.Fatalf("%v", err)
	}
	for name, body := range map[string]string{
		"no cost column": "ResourceId,ChargePeriodStart,ChargePeriodEnd\na,2026-10-01,2026-10-02\n",
		"bad amount":     focusHead + "Usage,a,a,ten,10,10,USD,2026-10-01T00:00:00Z,2026-10-02T00:00:00Z\n",
		"inverted":       focusHead + "Usage,a,a,10,10,10,USD,2026-10-02T00:00:00Z,2026-10-01T00:00:00Z\n",
		"empty":          "",
	} {
		if _, err := FocusTotals(strings.NewReader(body), ""); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestReconciledSavingNeedsTheInvoiceToFall(t *testing.T) {
	before := bill(t, "2026-10-01", 7, "10.00", "12.50", "USD")
	after := bill(t, "2026-10-08", 7, "6.00", "7.50", "USD")
	r := ReconcileBills(before, after)
	if !r.Comparable || !r.Invoiced || !r.BilledFell || r.Basis != "BilledCost" {
		t.Fatalf("%+v", r)
	}
	if r.SavingPerDay != amt(t, "4.00") || r.ListSavingPerDay == nil || *r.ListSavingPerDay != amt(t, "5.00") {
		t.Fatalf("saving %s list %v", r.SavingPerDay, r.ListSavingPerDay)
	}
	if lvl, missing := Classify(Evidence{Applied: true, Kept: true, Bills: &r}); lvl != LevelReconciled || len(missing) != 0 {
		t.Fatalf("%s %v", lvl, missing)
	}
}

// An account on credits (seen on a real Google export, docs/LAB.md result 22): usage falls at list price, the invoice stays at 0.
func TestCreditsAbsorbingTheFallIsNotAReconciledSaving(t *testing.T) {
	before := bill(t, "2026-10-01", 7, "0", "12.50", "USD")
	after := bill(t, "2026-10-08", 7, "0", "7.50", "USD")
	r := ReconcileBills(before, after)
	if !r.Comparable || r.BilledFell || r.SavingPerDay != 0 || *r.ListSavingPerDay != amt(t, "5.00") {
		t.Fatalf("%+v", r)
	}
	if !strings.Contains(strings.Join(r.Notes, " "), "nothing less was invoiced") {
		t.Fatalf("%v", r.Notes)
	}
	lvl, missing := Classify(Evidence{Applied: true, Kept: true, Bills: &r})
	if lvl != LevelObserved || len(missing) == 0 {
		t.Fatalf("a list-price fall with an unchanged invoice is observed, not reconciled: %s", lvl)
	}
}

func TestBillsThatCannotBeComparedYieldNoSaving(t *testing.T) {
	base := bill(t, "2026-10-01", 7, "10.00", "12.50", "USD")
	for name, c := range map[string]struct {
		after PeriodBill
		want  string
	}{
		"currency": {bill(t, "2026-10-08", 7, "6.00", "7.50", "EUR"), "no exchange rate is invented"},
		"overlap":  {bill(t, "2026-10-05", 7, "6.00", "7.50", "USD"), "overlap"},
		"length":   {bill(t, "2026-10-08", 3, "6.00", "7.50", "USD"), "differ in length"},
		"order":    {bill(t, "2026-09-01", 7, "6.00", "7.50", "USD"), "starts before"},
	} {
		r := ReconcileBills(base, c.after)
		if r.Comparable || r.SavingPerDay != 0 || r.ListSavingPerDay != nil || !strings.Contains(strings.Join(r.NotComparable, " "), c.want) {
			t.Errorf("%s: %+v", name, r)
		}
		if lvl, _ := Classify(Evidence{Applied: true, Kept: true, Bills: &r}); lvl != LevelProjected {
			t.Errorf("%s: level %s", name, lvl)
		}
	}
}

func TestSimulatedBillsNeverReachReconciled(t *testing.T) {
	before := bill(t, "2026-10-01", 7, "10.00", "12.50", "USD")
	after := bill(t, "2026-10-08", 7, "6.00", "7.50", "USD")
	after.Simulated = true
	r := ReconcileBills(before, after)
	if r.Invoiced {
		t.Fatal("a simulated bill is not an invoice")
	}
	if lvl, _ := Classify(Evidence{Applied: true, Kept: true, Bills: &r}); lvl == LevelReconciled || lvl == LevelObserved {
		t.Fatalf("level %s from simulated data", lvl)
	}
}

func TestCommitmentPurchaseSwitchesToAmortisedCost(t *testing.T) {
	head := focusHead
	mk := func(day, billed, effective string, purchase bool) PeriodBill {
		body := head + "Usage,n1,n1," + billed + "," + effective + ",70,USD," + day + "T00:00:00Z," + day[:8] + "08T00:00:00Z\n"
		if purchase {
			body += "Purchase,,,500,0,,USD," + day + "T00:00:00Z," + day[:8] + "08T00:00:00Z\n"
		}
		p, err := FocusTotals(strings.NewReader(body), "")
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	before := mk("2026-10-01", "70", "70", false)
	after := mk("2026-11-01", "0", "42", true) // usage covered by the commitment: billed 0, amortised 42
	r := ReconcileBills(before, after)
	if r.Basis != "EffectiveCost" || r.SavingPerDay != amt(t, "4.00") {
		t.Fatalf("basis %s saving %s (%v)", r.Basis, r.SavingPerDay, r.Notes)
	}
}

func TestTaxAndCreditsAreReportedApart(t *testing.T) {
	before := bill(t, "2026-10-01", 7, "10.00", "12.50", "USD", "Tax,,,13.30,13.30,,USD,2026-10-01T00:00:00Z,2026-10-08T00:00:00Z")
	after := bill(t, "2026-10-08", 7, "6.00", "7.50", "USD", "Tax,,,7.98,7.98,,USD,2026-10-08T00:00:00Z,2026-10-15T00:00:00Z",
		"Credit,,,-20,-20,,USD,2026-10-08T00:00:00Z,2026-10-15T00:00:00Z")
	r := ReconcileBills(before, after)
	if r.SavingPerDay != amt(t, "4.00") {
		t.Fatalf("tax or credits leaked into the saving: %s", r.SavingPerDay)
	}
	if r.TaxPerDayBefore != amt(t, "1.90") || r.TaxPerDayAfter != amt(t, "1.14") {
		t.Fatalf("tax %s %s", r.TaxPerDayBefore, r.TaxPerDayAfter)
	}
	notes := strings.Join(r.Notes, " ")
	if !strings.Contains(notes, "tax rows") || !strings.Contains(notes, "credit rows") {
		t.Fatalf("%v", r.Notes)
	}
}

func TestClassifyLadder(t *testing.T) {
	one := amt(t, "1")
	for name, c := range map[string]struct {
		e    Evidence
		want SavingsLevel
	}{
		"only a recommendation": {Evidence{}, LevelEstimated},
		"rolled back":           {Evidence{Applied: true}, LevelEstimated},
		"applied and kept":      {Evidence{Applied: true, Kept: true}, LevelProjected},
		"measured windows":      {Evidence{Applied: true, Kept: true, Measured: &Savings{Observed: &one}}, LevelObserved},
		"measured, not applied": {Evidence{Measured: &Savings{Observed: &one}}, LevelEstimated},
		"measurement invalid":   {Evidence{Applied: true, Kept: true, Measured: &Savings{}}, LevelProjected},
	} {
		got, missing := Classify(c.e)
		if got != c.want {
			t.Errorf("%s: %s, want %s", name, got, c.want)
		}
		if got != LevelReconciled && len(missing) == 0 {
			t.Errorf("%s: no explanation of what is missing", name)
		}
	}
}

// The export Google really delivers (CLP, hourly rows, Adjustment rows, empty ListCost cells) must total without an error.
func TestFocusTotalsOnTheRealGoogleExport(t *testing.T) {
	f, err := os.Open("testdata/gcp-native-focus-raw.csv")
	if err != nil {
		t.Skip(err)
	}
	defer f.Close()
	p, err := FocusTotals(f, "")
	if err != nil {
		t.Fatal(err)
	}
	if p.Currency != "CLP" || p.Rows == 0 || p.days() <= 0 || !p.HasList || !p.HasEffective {
		t.Fatalf("%+v", p)
	}
	if _, ok := p.Other["Adjustment"]; !ok {
		t.Fatalf("adjustment rows must be kept apart: %v", p.Other)
	}
}
