package cost

import (
	"encoding/csv"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/money"
)

// FocusSummary says what a FOCUS export contained that was NOT turned into node cost.
type FocusSummary struct {
	Rows int
	// Skipped is the sum of BilledCost per non-Usage ChargeCategory (Purchase, Tax, Credit, Adjustment). They are reported, never silently dropped.
	Skipped map[string]money.USD
	Notes   []string
}

// ParseFOCUS reads a FinOps Open Cost and Usage Specification (FOCUS) CSV export and prorates its Usage rows to [start, end].
//
// Columns used (names as in the FOCUS specification): ResourceId, BilledCost, ChargePeriodStart, ChargePeriodEnd, and, if present,
// BillingCurrency (must be USD: no exchange rate is ever invented) and ChargeCategory (only "Usage" becomes cost).
// This parser was checked against hand-written fixtures that follow the specification's column names. It was NOT checked against an
// export produced by AWS, GCP or Azure.
func ParseFOCUS(r io.Reader, start, end time.Time) ([]BillingItem, FocusSummary, error) {
	sum := FocusSummary{Skipped: map[string]money.USD{}}
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1
	rows, err := cr.ReadAll()
	if err != nil {
		return nil, sum, fmt.Errorf("focus csv: %w", err)
	}
	if len(rows) == 0 {
		return nil, sum, fmt.Errorf("focus csv is empty")
	}
	col := map[string]int{}
	for i, h := range rows[0] {
		col[strings.ToLower(strings.TrimSpace(h))] = i
	}
	for _, need := range []string{"resourceid", "billedcost", "chargeperiodstart", "chargeperiodend"} {
		if _, ok := col[need]; !ok {
			return nil, sum, fmt.Errorf("focus csv lacks the %q column", need)
		}
	}
	_, hasCur := col["billingcurrency"]
	_, hasCat := col["chargecategory"]
	if !hasCur {
		sum.Notes = append(sum.Notes, "no BillingCurrency column: the amounts are ASSUMED to be USD")
	}
	var out []BillingItem
	for n, row := range rows[1:] {
		get := func(c string) string {
			if i, ok := col[c]; ok && i < len(row) {
				return strings.TrimSpace(row[i])
			}
			return ""
		}
		line := n + 2
		sum.Rows++
		if hasCur {
			if cur := get("billingcurrency"); !strings.EqualFold(cur, "USD") {
				return nil, sum, fmt.Errorf("line %d: BillingCurrency %q is not USD (no currency conversion is done)", line, cur)
			}
		}
		cost, err := parseSigned(get("billedcost"))
		if err != nil {
			return nil, sum, fmt.Errorf("line %d: BilledCost: %w", line, err)
		}
		if hasCat {
			if cat := get("chargecategory"); cat != "" && !strings.EqualFold(cat, "Usage") {
				sum.Skipped[cat] += cost
				continue
			}
		}
		if cost < 0 {
			return nil, sum, fmt.Errorf("line %d: negative Usage cost (corrections must be netted before export)", line)
		}
		s, err := time.Parse(time.RFC3339, get("chargeperiodstart"))
		if err != nil {
			return nil, sum, fmt.Errorf("line %d: ChargePeriodStart: %w", line, err)
		}
		e, err := time.Parse(time.RFC3339, get("chargeperiodend"))
		if err != nil || !e.After(s) {
			return nil, sum, fmt.Errorf("line %d: ChargePeriodEnd must be an RFC3339 time after ChargePeriodStart", line)
		}
		lo, hi := maxT(s, start), minT(e, end)
		if !hi.After(lo) {
			continue
		}
		whole, part := e.Sub(s).Seconds(), hi.Sub(lo).Seconds()
		out = append(out, BillingItem{Resource: get("resourceid"), Cost: cost.MulRat(int64(part*1000), int64(whole*1000)), Start: lo, End: hi})
	}
	if len(sum.Skipped) > 0 {
		sum.Notes = append(sum.Notes, "non-Usage charges (credits, taxes, purchases, adjustments) were NOT allocated; they are listed in Skipped")
	}
	return out, sum, nil
}

func lastSegment(s string) string {
	s = strings.TrimRight(s, "/")
	if i := strings.LastIndexAny(s, "/:"); i >= 0 {
		s = s[i+1:]
	}
	return strings.ToLower(s)
}

// SplitByNodeIdentity assigns billing items to nodes by node name OR by the last segment of the provider id
// (aws:///zone/i-0abc, gce://project/zone/name, azure:///subscriptions/.../virtualMachines/name) against the last segment of the
// billed ResourceId (an ARN, a resource path or a plain id). Items that match no node are returned in extra (unallocated).
func SplitByNodeIdentity(items []BillingItem, nodes []Node) (nodeCost map[string]money.USD, extra []BillingItem) {
	byKey := map[string]string{}
	for _, n := range nodes {
		byKey[strings.ToLower(n.Name)] = n.Name
		if n.ProviderID != "" {
			byKey[lastSegment(n.ProviderID)] = n.Name
		}
	}
	nodeCost = map[string]money.USD{}
	for _, it := range items {
		if name, ok := byKey[lastSegment(it.Resource)]; ok && it.Resource != "" {
			nodeCost[name] += it.Cost
		} else {
			extra = append(extra, it)
		}
	}
	return nodeCost, extra
}

// parseSigned accepts a leading minus (credits and corrections are negative in real exports); money.Parse alone rejects it.
func parseSigned(s string) (money.USD, error) {
	if strings.HasPrefix(s, "-") {
		v, err := money.Parse(strings.TrimPrefix(s, "-"))
		return -v, err
	}
	return money.Parse(s)
}
