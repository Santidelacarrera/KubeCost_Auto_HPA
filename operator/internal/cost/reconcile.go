package cost

import (
	"encoding/csv"
	"fmt"
	"io"
	"math"
	"strings"
	"time"

	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/money"
)

// SavingsLevel is how much evidence stands behind a saving. Each level needs everything the one before it needs, plus one
// thing. A number is always reported with its level, and never promoted by wording.
type SavingsLevel string

const (
	// LevelEstimated: a recommendation exists. Requests delta x a unit price. Nothing was changed.
	LevelEstimated SavingsLevel = "estimated"
	// LevelProjected: the change was applied and kept (verified, not rolled back). The estimate is now a projection of a real
	// configuration, but no cost was measured.
	LevelProjected SavingsLevel = "projected"
	// LevelObserved: cost was measured in two equivalent windows, before and after, on the same basis (allocated cost, or
	// node-hours at list price). It is not an invoice.
	LevelObserved SavingsLevel = "observed"
	// LevelReconciled: two provider billing exports for equivalent periods, in one currency, show the difference in what was
	// invoiced (or amortised), with tax, credits, purchases and unattributable rows accounted for.
	LevelReconciled SavingsLevel = "reconciled"
)

// Evidence is what is known about one optimisation.
type Evidence struct {
	// Applied: the change was made. Kept: it passed its observation and was not rolled back.
	Applied, Kept bool
	// Measured: Savings.Observed came out of ObservedSavings (two comparable reports).
	Measured *Savings
	// Bills: the result of ReconcileBills.
	Bills *BillReconciliation
}

// Classify returns the highest level the evidence supports and, for the reader, what is missing for the next one.
func Classify(e Evidence) (SavingsLevel, []string) {
	switch {
	case !e.Applied:
		return LevelEstimated, []string{"the change has not been applied: this is requests x unit price"}
	case !e.Kept:
		return LevelEstimated, []string{"the change was applied but not kept (rolled back, or still under observation)"}
	}
	if e.Bills != nil && e.Bills.Comparable && e.Bills.Invoiced {
		if e.Bills.BilledFell {
			return LevelReconciled, nil
		}
		return LevelObserved, []string{"the billing exports are comparable but the invoiced amount did not fall (see the reconciliation notes): only the list-price difference is shown"}
	}
	if e.Measured != nil && e.Measured.Observed != nil {
		return LevelObserved, []string{"no comparable provider billing exports for both periods: this is measured cost, not an invoice"}
	}
	why := "no cost was measured in equivalent windows before and after"
	if e.Bills != nil && !e.Bills.Comparable {
		why = "the billing periods are not comparable: " + strings.Join(e.Bills.NotComparable, "; ")
	} else if e.Bills != nil && !e.Bills.Invoiced {
		why = "the billing data is labelled as simulated or is not a provider export"
	}
	return LevelProjected, []string{why}
}

// PeriodBill is the total of one billing export for one period, in the export's own billing currency. Amounts are exact
// (micro-units); the type is named USD for history, the unit is Currency.
type PeriodBill struct {
	Label      string
	Currency   string
	Start, End time.Time
	Rows       int

	// Usage rows (ChargeCategory "Usage", or every row when the export has no category column).
	ListUsage      money.USD // ListCost: before discounts, commitments and credits. Zero when the column is absent (HasList false).
	BilledUsage    money.USD // BilledCost: what the period's invoice charges for usage
	EffectiveUsage money.USD // EffectiveCost: amortised; zero when the column is absent (HasEffective false)
	HasList        bool
	HasEffective   bool
	// Everything that is not usage, by ChargeCategory, on the BilledCost column: Tax, Credit, Purchase (commitments bought in the
	// period), Adjustment. Reported, and kept out of the usage comparison.
	Other map[string]money.USD
	// NoResource is the billed usage of rows without a ResourceId: cost that cannot be attributed to any resource (support,
	// shared services, some network charges).
	NoResource money.USD
	// OutOfScope is the billed usage of rows a scope filter excluded.
	OutOfScope money.USD
	// Simulated marks data that is not a provider export. It can never reach LevelReconciled.
	Simulated bool
}

func (p PeriodBill) days() float64 { return p.End.Sub(p.Start).Hours() / 24 }

// FocusTotals totals a FOCUS export AS DELIVERED, in its billing currency (nothing is converted here: two periods are compared in
// the currency they were billed in). scope, when not empty, keeps only rows whose ResourceId or ResourceName contains it,
// case-insensitively: a cluster name, a resource group, a project. The period is the span of the kept rows' charge periods.
func FocusTotals(r io.Reader, scope string) (PeriodBill, error) {
	b := PeriodBill{Other: map[string]money.USD{}}
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1
	rows, err := cr.ReadAll()
	if err != nil {
		return b, fmt.Errorf("focus csv: %w", err)
	}
	if len(rows) == 0 {
		return b, fmt.Errorf("focus csv is empty")
	}
	col := map[string]int{}
	for i, h := range rows[0] {
		col[strings.ToLower(strings.TrimSpace(strings.TrimPrefix(h, "\ufeff")))] = i
	}
	for _, need := range []string{"billedcost", "chargeperiodstart", "chargeperiodend"} {
		if _, ok := col[need]; !ok {
			return b, fmt.Errorf("focus csv lacks the %q column", need)
		}
	}
	_, b.HasList = col["listcost"]
	_, b.HasEffective = col["effectivecost"]
	_, hasCur := col["billingcurrency"]
	_, hasCat := col["chargecategory"]
	scope = strings.ToLower(strings.TrimSpace(scope))
	for n, row := range rows[1:] {
		line := n + 2
		get := func(c string) string {
			if i, ok := col[c]; ok && i < len(row) {
				return strings.TrimSpace(row[i])
			}
			return ""
		}
		if len(row) == 1 && strings.TrimSpace(row[0]) == "" {
			continue
		}
		amount := func(c string) (money.USD, error) {
			raw := get(c)
			if raw == "" { // real exports leave ListCost empty on some rows
				return 0, nil
			}
			v, err := parseSigned(raw)
			if err != nil {
				return 0, fmt.Errorf("line %d: %s: %w", line, c, err)
			}
			return v, nil
		}
		cur := "USD"
		if hasCur {
			cur = strings.ToUpper(get("billingcurrency"))
		}
		switch {
		case cur == "":
			return b, fmt.Errorf("line %d: empty BillingCurrency", line)
		case b.Currency == "":
			b.Currency = cur
		case b.Currency != cur:
			return b, fmt.Errorf("line %d: the export mixes billing currencies (%s and %s); totals in one currency need one currency", line, b.Currency, cur)
		}
		billed, err := amount("billedcost")
		if err != nil {
			return b, err
		}
		if cat := get("chargecategory"); hasCat && cat != "" && !strings.EqualFold(cat, "Usage") {
			b.Other[cat] += billed
			b.Rows++
			continue
		}
		id := get("resourceid")
		if scope != "" && !strings.Contains(strings.ToLower(id), scope) && !strings.Contains(strings.ToLower(get("resourcename")), scope) {
			b.OutOfScope += billed
			continue
		}
		from, err := parseFocusTime(get("chargeperiodstart"))
		if err != nil {
			return b, fmt.Errorf("line %d: ChargePeriodStart: %w", line, err)
		}
		to, err := parseFocusTime(get("chargeperiodend"))
		if err != nil {
			return b, fmt.Errorf("line %d: ChargePeriodEnd: %w", line, err)
		}
		if !to.After(from) {
			return b, fmt.Errorf("line %d: charge period ends before it starts", line)
		}
		if b.Start.IsZero() || from.Before(b.Start) {
			b.Start = from
		}
		if to.After(b.End) {
			b.End = to
		}
		list, err := amount("listcost")
		if err != nil {
			return b, err
		}
		eff, err := amount("effectivecost")
		if err != nil {
			return b, err
		}
		b.Rows++
		b.BilledUsage += billed
		b.ListUsage += list
		b.EffectiveUsage += eff
		if id == "" {
			b.NoResource += billed
		}
	}
	if b.Start.IsZero() {
		return b, fmt.Errorf("no usage rows in scope: nothing to total")
	}
	return b, nil
}

// BillReconciliation compares two periods. Every amount is per day, so periods of slightly different length compare fairly, and
// in the bills' currency.
type BillReconciliation struct {
	Currency      string
	Before, After PeriodBill
	// Comparable is false when the two bills cannot honestly be compared; NotComparable says why. The differences are then zero.
	Comparable    bool
	NotComparable []string
	// Invoiced: both are provider exports (neither is marked Simulated).
	Invoiced bool
	// Basis is the column the headline saving uses: "BilledCost", or "EffectiveCost" when commitment purchases fall in a period.
	Basis string
	// SavingPerDay is before minus after on Basis, usage rows only, tax excluded. Negative means the cost rose.
	SavingPerDay money.USD
	// ListSavingPerDay is the same at list price (before discounts, commitments and credits); nil without ListCost in both.
	ListSavingPerDay *money.USD
	// BilledFell: SavingPerDay is positive. A list-price fall that credits or commitments absorb is not a billed saving.
	BilledFell bool
	// TaxPerDayBefore/After are reported apart: tax follows the invoice and is never counted as an optimisation saving.
	TaxPerDayBefore, TaxPerDayAfter money.USD
	Notes                           []string
}

func perDay(v money.USD, days float64) money.USD {
	out, err := money.FromFloat(v.Float() / days)
	if err != nil {
		return 0
	}
	return out
}

func otherCI(m map[string]money.USD, name string) money.USD {
	var t money.USD
	for k, v := range m {
		if strings.EqualFold(k, name) {
			t += v
		}
	}
	return t
}

// ReconcileBills compares the bill of the period before an optimisation with the bill of the period after it.
//
// It refuses to compare (Comparable=false) bills in different currencies (no exchange rate is invented), periods that overlap,
// and periods whose lengths differ by more than 10%. It never returns a saving for those.
func ReconcileBills(before, after PeriodBill) BillReconciliation {
	r := BillReconciliation{Before: before, After: after, Currency: before.Currency, Basis: "BilledCost", Invoiced: !before.Simulated && !after.Simulated}
	db, da := before.days(), after.days()
	switch {
	case before.Currency == "" || after.Currency == "":
		r.NotComparable = append(r.NotComparable, "a bill has no currency")
	case before.Currency != after.Currency:
		r.NotComparable = append(r.NotComparable, fmt.Sprintf("different billing currencies (%s and %s): no exchange rate is invented", before.Currency, after.Currency))
	}
	if db <= 0 || da <= 0 {
		r.NotComparable = append(r.NotComparable, "a period has no length")
	} else {
		if before.End.After(after.Start) && after.End.After(before.Start) {
			r.NotComparable = append(r.NotComparable, "the periods overlap")
		}
		if ratio := da / db; ratio < 0.9 || ratio > 1.1 {
			r.NotComparable = append(r.NotComparable, fmt.Sprintf("the periods differ in length by more than 10%% (%.1f and %.1f days)", db, da))
		}
		if after.Start.Before(before.Start) {
			r.NotComparable = append(r.NotComparable, "the 'after' period starts before the 'before' period")
		}
	}
	if !r.Invoiced {
		r.Notes = append(r.Notes, "at least one bill is marked simulated: the result can illustrate the method, never confirm a saving")
	}
	if len(r.NotComparable) > 0 {
		return r
	}
	r.Comparable = true

	// commitment purchases distort the invoiced column in the period they are bought: compare amortised cost when both have it
	if otherCI(before.Other, "Purchase") != 0 || otherCI(after.Other, "Purchase") != 0 {
		if before.HasEffective && after.HasEffective {
			r.Basis = "EffectiveCost"
			r.Notes = append(r.Notes, "a commitment purchase falls in one of the periods: the comparison uses EffectiveCost (amortised) instead of BilledCost")
		} else {
			r.Notes = append(r.Notes, "a commitment purchase falls in one of the periods and the export has no EffectiveCost: the purchase is excluded from the usage comparison, but usage it covers may be billed at zero")
		}
	}
	ub, ua := before.BilledUsage, after.BilledUsage
	if r.Basis == "EffectiveCost" {
		ub, ua = before.EffectiveUsage, after.EffectiveUsage
	}
	r.SavingPerDay = perDay(ub, db) - perDay(ua, da)
	r.BilledFell = r.SavingPerDay > 0
	if before.HasList && after.HasList {
		d := perDay(before.ListUsage, db) - perDay(after.ListUsage, da)
		r.ListSavingPerDay = &d
		if d > 0 && !r.BilledFell {
			r.Notes = append(r.Notes, fmt.Sprintf("usage fell at list price but the %s did not: credits, a free tier or commitments absorb it, so nothing less was invoiced", r.Basis))
		}
	}
	r.TaxPerDayBefore, r.TaxPerDayAfter = perDay(otherCI(before.Other, "Tax"), db), perDay(otherCI(after.Other, "Tax"), da)
	if r.TaxPerDayBefore != 0 || r.TaxPerDayAfter != 0 {
		r.Notes = append(r.Notes, "tax rows are reported apart and are not part of the saving")
	}
	if c := otherCI(before.Other, "Credit") + otherCI(after.Other, "Credit"); c != 0 {
		r.Notes = append(r.Notes, "credit rows are present: they lower the invoice for reasons unrelated to the optimisation and are excluded from the usage comparison")
	}
	for i, p := range []PeriodBill{before, after} {
		name := []string{"before", "after"}[i]
		if p.BilledUsage != 0 && math.Abs(p.NoResource.Float()) > 0.10*math.Abs(p.BilledUsage.Float()) {
			r.Notes = append(r.Notes, fmt.Sprintf("more than 10%% of the %q usage has no ResourceId: that share cannot be attributed to the cluster", name))
		}
	}
	if before.OutOfScope == 0 && after.OutOfScope == 0 {
		r.Notes = append(r.Notes, "no scope filter excluded anything: the comparison is of everything in the exports, so unrelated resources move the result")
	}
	r.Notes = append(r.Notes, "a lower bill is attributed to the optimisation only if demand was equivalent in both periods; compare request volume before reading this as a saving")
	return r
}
