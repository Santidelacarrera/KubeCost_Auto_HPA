package cost

import (
	"encoding/csv"
	"fmt"
	"io"
	"math/big"
	"sort"
	"strings"
	"time"

	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/money"
)

// FocusSummary says what a FOCUS export contained that was NOT turned into node cost, and how amounts were interpreted.
type FocusSummary struct {
	Rows int
	// Skipped is the sum of the cost column per non-Usage ChargeCategory (Purchase, Tax, Credit, Adjustment). They are reported, never
	// silently dropped.
	Skipped map[string]money.USD
	// Converted counts rows per billing currency that were converted to USD with the export's own conversion-rate column.
	Converted map[string]int
	Notes     []string
}

// FocusOptions selects how a FOCUS export is read.
type FocusOptions struct {
	// CostColumn is "BilledCost" (default: what is invoiced for the period) or "EffectiveCost" (amortised: upfront purchases spread
	// over the usage they cover, after discounts). FOCUS defines both; which one to allocate is a FinOps policy choice, not a guess.
	CostColumn string
}

// focusRateColumn is Google's FOCUS extension column: billing-currency units per 1 USD for that row (the same
// currency_conversion_rate as the detailed export). Rows billed in another currency are converted with THIS column only; without it
// a non-USD row is refused, because no exchange rate is ever invented here.
const focusRateColumn = "x_currencyconversionrate"

// ParseFOCUS reads a FOCUS export with the default options (BilledCost). See ParseFOCUSWith.
func ParseFOCUS(r io.Reader, start, end time.Time) ([]BillingItem, FocusSummary, error) {
	return ParseFOCUSWith(r, start, end, FocusOptions{})
}

// ParseFOCUSWith reads a FinOps Open Cost and Usage Specification (FOCUS) CSV export AS THE PROVIDER DELIVERS IT and prorates its
// Usage rows to [start, end].
//
// Columns used (FOCUS names, case-insensitive): ResourceId, the cost column (BilledCost or EffectiveCost), ChargePeriodStart,
// ChargePeriodEnd and, if present, ResourceName (Google puts the instance NAME here), BillingCurrency, x_CurrencyConversionRate and
// ChargeCategory (only "Usage" becomes cost).
//
// Checked against: Google's native FOCUS export in BigQuery written with `bq --format=csv` (2026-11-11, an account billed in CLP:
// timestamps as "2006-01-02 15:04:05" in UTC, BillingCurrency CLP with x_CurrencyConversionRate, empty ListCost cells).
// Azure and AWS exports: against the FOCUS column names and Azure's documented ResourceId shape only, not a real export.
func ParseFOCUSWith(r io.Reader, start, end time.Time, opt FocusOptions) ([]BillingItem, FocusSummary, error) {
	sum := FocusSummary{Skipped: map[string]money.USD{}, Converted: map[string]int{}}
	costCol := strings.ToLower(opt.CostColumn)
	if costCol == "" {
		costCol = "billedcost"
	}
	if costCol != "billedcost" && costCol != "effectivecost" && costCol != "listcost" {
		return nil, sum, fmt.Errorf("focus cost column must be BilledCost, EffectiveCost or ListCost, not %q", opt.CostColumn)
	}
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
		col[strings.ToLower(strings.TrimSpace(strings.TrimPrefix(h, "\ufeff")))] = i
	}
	for _, need := range []string{"resourceid", costCol, "chargeperiodstart", "chargeperiodend"} {
		if _, ok := col[need]; !ok {
			return nil, sum, fmt.Errorf("focus csv lacks the %q column", need)
		}
	}
	_, hasCur := col["billingcurrency"]
	_, hasCat := col["chargecategory"]
	_, hasRate := col[focusRateColumn]
	if !hasCur {
		sum.Notes = append(sum.Notes, "no BillingCurrency column: the amounts are ASSUMED to be USD")
	}
	if costCol == "listcost" {
		// a free-trial or fully credited account bills 0 for everything (seen on a real Google export, docs/LAB.md result 22)
		sum.Notes = append(sum.Notes, "cost column: ListCost (list price, before any discount, commitment or credit): NOT what is invoiced")
	}
	if costCol == "effectivecost" {
		sum.Notes = append(sum.Notes, "cost column: EffectiveCost (amortised, after discounts), not the invoiced BilledCost")
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
		if len(row) == 1 && strings.TrimSpace(row[0]) == "" {
			continue // a trailing blank line
		}
		sum.Rows++
		raw := get(costCol)
		var cost money.USD
		cur := "USD"
		if hasCur {
			cur = strings.ToUpper(get("billingcurrency"))
		}
		if cur == "USD" {
			if cost, err = parseSigned(raw); err != nil {
				return nil, sum, fmt.Errorf("line %d: %s: %w", line, opt.costName(), err)
			}
		} else {
			rate := get(focusRateColumn)
			if !hasRate || rate == "" {
				return nil, sum, fmt.Errorf("line %d: BillingCurrency %q is not USD and the row has no x_CurrencyConversionRate (no exchange rate is invented)", line, cur)
			}
			if cost, err = convertToUSD(raw, rate); err != nil {
				return nil, sum, fmt.Errorf("line %d: converting %s %s at %s per USD: %w", line, raw, cur, rate, err)
			}
			sum.Converted[cur]++
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
		s, err := parseFocusTime(get("chargeperiodstart"))
		if err != nil {
			return nil, sum, fmt.Errorf("line %d: ChargePeriodStart: %w", line, err)
		}
		e, err := parseFocusTime(get("chargeperiodend"))
		if err != nil || !e.After(s) {
			return nil, sum, fmt.Errorf("line %d: ChargePeriodEnd must be a time after ChargePeriodStart", line)
		}
		lo, hi := maxT(s, start), minT(e, end)
		if !hi.After(lo) {
			continue
		}
		whole, part := e.Sub(s).Seconds(), hi.Sub(lo).Seconds()
		out = append(out, BillingItem{
			Resource: get("resourceid"), Name: get("resourcename"),
			Cost: cost.MulRat(int64(part*1000), int64(whole*1000)), Start: lo, End: hi,
		})
	}
	if len(sum.Skipped) > 0 {
		sum.Notes = append(sum.Notes, "non-Usage charges (credits, taxes, purchases, adjustments) were NOT allocated; they are listed in Skipped")
	}
	if len(sum.Converted) > 0 {
		curs := make([]string, 0, len(sum.Converted))
		for c, k := range sum.Converted {
			curs = append(curs, fmt.Sprintf("%s (%d rows)", c, k))
		}
		sort.Strings(curs)
		sum.Notes = append(sum.Notes, "converted to USD with the export's own per-row x_CurrencyConversionRate: "+strings.Join(curs, ", ")+
			"; the invoice is in that currency, so reconcile totals there")
	}
	if _, ok := col["x_credits"]; ok || hasRate {
		// checked on a real export (docs/LAB.md result 22): ListCost 5544.99 + x_Credits -5544.99 (PROMOTION) = BilledCost 0
		sum.Notes = append(sum.Notes, "Google's FOCUS export carries credits inside x_Credits on the Usage row, not as Credit rows, and "+
			"BilledCost already has them taken off (an account on credits bills 0: use --focus-cost-column ListCost to see the usage)")
	}
	return out, sum, nil
}

func (o FocusOptions) costName() string {
	if o.CostColumn == "" {
		return "BilledCost"
	}
	return o.CostColumn
}

// convertToUSD divides a billing-currency amount by the provider's units-per-USD rate exactly (big.Rat), keeping the sign.
func convertToUSD(amount, rate string) (money.USD, error) {
	a, ok := new(big.Rat).SetString(strings.TrimSpace(amount))
	if !ok {
		return 0, fmt.Errorf("amount %q is not a number", amount)
	}
	r, ok := new(big.Rat).SetString(strings.TrimSpace(rate))
	if !ok || r.Sign() <= 0 {
		return 0, fmt.Errorf("rate %q must be a positive number", rate)
	}
	return parseSigned(new(big.Rat).Quo(a, r).FloatString(9))
}

// parseFocusTime accepts the timestamp shapes real exports use: RFC3339 with or without fractional seconds (Azure writes seven
// digits), BigQuery's CSV form "2006-01-02 15:04:05" (optionally with " UTC"), a zone-less "2006-01-02T15:04:05", and a bare date.
// Zone-less shapes are read as UTC, which is what both providers document for these columns.
func parseFocusTime(s string) (time.Time, error) {
	s = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s), " UTC"))
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05.999999999", "2006-01-02T15:04:05.999999999", "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("%q is not a recognised timestamp", s)
}

func lastSegment(s string) string {
	s = strings.TrimRight(s, "/")
	if i := strings.LastIndexAny(s, "/:"); i >= 0 {
		s = s[i+1:]
	}
	return strings.ToLower(s)
}

// azureScaleSet returns the lower-cased ARM path of the scale set an AKS node's provider id belongs to
// ("azure:///subscriptions/s/resourceGroups/rg/providers/Microsoft.Compute/virtualMachineScaleSets/vmss/virtualMachines/0" ->
// "/subscriptions/s/resourcegroups/rg/providers/microsoft.compute/virtualmachinescalesets/vmss"), or "".
func azureScaleSet(providerID string) string {
	p := strings.ToLower(providerID)
	if !strings.HasPrefix(p, "azure://") {
		return ""
	}
	p = "/" + strings.TrimLeft(strings.TrimPrefix(p, "azure://"), "/")
	i := strings.Index(p, "/virtualmachines/")
	if i < 0 || !strings.Contains(p[:i], "/virtualmachinescalesets/") {
		return ""
	}
	return p[:i]
}

// SplitByNodeIdentity assigns billing items to nodes. An item belongs to a node when, in this order:
//  1. the last segment of its ResourceId equals the node name or the last segment of the node's provider id (aws:///zone/i-0abc,
//     gce://project/zone/name, azure:///.../virtualMachines/name, an ARN, a resource path or a plain id);
//  2. the last segment of its ResourceName does (Google's FOCUS export names the instance there);
//  3. its ResourceId is an Azure VM SCALE SET the node is an instance of. Azure bills AKS node pools per scale set, not per node,
//     so the item is split across the scale set's CURRENT nodes by allocatable CPU. Nodes removed during the period are not in that
//     split; the item's total is kept, never dropped.
//
// A bare instance number ("0", the last segment of a scale-set instance's provider id) is never used as a key: it would match any
// resource ending in /0. Items that match no node are returned in extra (unallocated).
func SplitByNodeIdentity(items []BillingItem, nodes []Node) (nodeCost map[string]money.USD, extra []BillingItem) {
	byKey := map[string]string{}
	sets := map[string][]Node{}
	for _, n := range nodes {
		byKey[strings.ToLower(n.Name)] = n.Name
		ss := azureScaleSet(n.ProviderID)
		if n.ProviderID != "" && ss == "" {
			byKey[lastSegment(n.ProviderID)] = n.Name
		}
		if ss != "" {
			sets[ss] = append(sets[ss], n)
		}
	}
	nodeCost = map[string]money.USD{}
	for _, it := range items {
		if it.Resource != "" {
			if name, ok := byKey[lastSegment(it.Resource)]; ok {
				nodeCost[name] += it.Cost
				continue
			}
		}
		if it.Name != "" {
			if name, ok := byKey[lastSegment(it.Name)]; ok {
				nodeCost[name] += it.Cost
				continue
			}
		}
		if members := sets[strings.TrimRight(strings.ToLower(it.Resource), "/")]; len(members) > 0 {
			for name, c := range splitByCPU(it.Cost, members) {
				nodeCost[name] += c
			}
			continue
		}
		extra = append(extra, it)
	}
	return nodeCost, extra
}

// splitByCPU divides c across nodes by allocatable CPU (equally when none is known); the parts always sum to c exactly.
func splitByCPU(c money.USD, nodes []Node) map[string]money.USD {
	ns := append([]Node(nil), nodes...)
	sort.Slice(ns, func(i, j int) bool { return ns[i].Name < ns[j].Name })
	w := make([]int64, len(ns))
	var total int64
	for i, n := range ns {
		w[i] = int64(n.CPUAllocatable * 1000)
		total += w[i]
	}
	if total <= 0 {
		for i := range w {
			w[i] = 1
		}
		total = int64(len(w))
	}
	out := map[string]money.USD{}
	var given money.USD
	for i, n := range ns {
		part := c.MulRat(w[i], total)
		if i == len(ns)-1 {
			part = c - given
		}
		out[n.Name] += part
		given += part
	}
	return out
}

// parseSigned accepts a leading minus (credits and corrections are negative in real exports); money.Parse alone rejects it.
func parseSigned(s string) (money.USD, error) {
	if strings.HasPrefix(s, "-") {
		v, err := money.Parse(strings.TrimPrefix(s, "-"))
		return -v, err
	}
	return money.Parse(s)
}
