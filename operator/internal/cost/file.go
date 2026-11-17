package cost

import (
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/money"
)

// FileAdapter reads a provider billing export that has been reduced to the columns
//
//	resource,cost_usd,start,end        (RFC3339 timestamps; resource = node name or instance id)
//
// It is deliberately provider-neutral: an AWS CUR, GCP billing export or Azure cost export is converted to this shape by
// a query or script that belongs to the provider's own tooling. No provider API is called and no credential is needed here.
// Items are prorated by the overlap between their period and the requested window.
type FileAdapter struct {
	Path string
	// Label is shown in reports; use it to mark data as simulated or real, e.g. "aws-cur-2026-10" or "SIMULATED-lab".
	Label string
}

func (f FileAdapter) Name() string {
	if f.Label != "" {
		return "file:" + f.Label
	}
	return "file:" + f.Path
}

func (f FileAdapter) Billed(_ context.Context, start, end time.Time) ([]BillingItem, error) {
	fh, err := os.Open(f.Path)
	if err != nil {
		return nil, err
	}
	defer fh.Close()
	return ParseBilling(fh, start, end)
}

// ParseBilling parses the CSV described on FileAdapter and prorates each row to [start, end].
func ParseBilling(r io.Reader, start, end time.Time) ([]BillingItem, error) {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1
	rows, err := cr.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("billing csv: %w", err)
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("billing csv is empty")
	}
	col := map[string]int{}
	for i, h := range rows[0] {
		col[strings.ToLower(strings.TrimSpace(h))] = i
	}
	for _, need := range []string{"resource", "cost_usd", "start", "end"} {
		if _, ok := col[need]; !ok {
			return nil, fmt.Errorf("billing csv lacks the %q column", need)
		}
	}
	var out []BillingItem
	for n, row := range rows[1:] {
		get := func(c string) string {
			if i := col[c]; i < len(row) {
				return strings.TrimSpace(row[i])
			}
			return ""
		}
		line := n + 2
		cost, err := money.Parse(get("cost_usd"))
		if err != nil {
			return nil, fmt.Errorf("line %d: cost_usd: %w", line, err)
		}
		if cost < 0 {
			return nil, fmt.Errorf("line %d: negative cost (credits/refunds must be handled before export)", line)
		}
		s, err := time.Parse(time.RFC3339, get("start"))
		if err != nil {
			return nil, fmt.Errorf("line %d: start: %w", line, err)
		}
		e, err := time.Parse(time.RFC3339, get("end"))
		if err != nil || !e.After(s) {
			return nil, fmt.Errorf("line %d: end must be an RFC3339 time after start", line)
		}
		lo, hi := maxT(s, start), minT(e, end)
		if !hi.After(lo) {
			continue
		}
		whole := e.Sub(s).Seconds()
		part := hi.Sub(lo).Seconds()
		out = append(out, BillingItem{Resource: get("resource"), Cost: cost.MulRat(int64(part*1000), int64(whole*1000)), Start: lo, End: hi})
	}
	return out, nil
}

func maxT(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func minT(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

// SplitByNode sums the items that name a known node into nodeCost and returns the rest as extra (unallocated).
func SplitByNode(items []BillingItem, nodes []Node) (nodeCost map[string]money.USD, extra []BillingItem) {
	known := map[string]bool{}
	for _, n := range nodes {
		known[n.Name] = true
	}
	nodeCost = map[string]money.USD{}
	for _, it := range items {
		if known[it.Resource] {
			nodeCost[it.Resource] += it.Cost
		} else {
			extra = append(extra, it)
		}
	}
	return nodeCost, extra
}
