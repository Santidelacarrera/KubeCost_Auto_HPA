// Package outcome summarises what actually happened to recommendations, from the audit trail alone.
//
// It measures the process (evaluated, allowed, applied, verified, rolled back) and carries the ESTIMATED potential saving of the changes
// that stayed in place. It does not measure money saved: that needs two equivalent periods of allocated or billed cost (internal/cost).
package outcome

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/execute"
)

// Summary counts distinct recommendation IDs per outcome.
type Summary struct {
	Since                                           time.Time
	Evaluated, Allowed, Denied                      int
	Applied, Verified, Degraded, RolledBack, Failed int
	// AcceptanceRate = Applied / Evaluated (a recommendation is accepted when it was approved, allowed and applied).
	AcceptanceRate float64
	// RollbackRate = RolledBack / Applied.
	RollbackRate float64
	// KeptEstimatedMonthlyUSD sums the ESTIMATED potential saving of applied changes that were verified and not rolled back.
	// PendingEstimatedMonthlyUSD is applied but neither verified nor rolled back yet; RolledBackEstimatedMonthlyUSD never took effect.
	KeptEstimatedMonthlyUSD, PendingEstimatedMonthlyUSD, RolledBackEstimatedMonthlyUSD float64
	// TopDenials counts the most frequent denial reasons (first 80 characters).
	TopDenials []Denial
	Notes      []string
}

// Denial is a reason and how many times it appeared.
type Denial struct {
	Reason string
	Count  int
}

// Summarize reads JSONL audit events (one execute.Event per line). Lines that are not valid events are counted in Notes, never silently skipped.
func Summarize(r io.Reader, since time.Time) (Summary, error) {
	s := Summary{Since: since}
	sets := map[string]map[string]bool{}
	mark := func(typ, id string) bool {
		if sets[typ] == nil {
			sets[typ] = map[string]bool{}
		}
		if sets[typ][id] {
			return false
		}
		sets[typ][id] = true
		return true
	}
	saving := map[string]float64{}
	denials := map[string]int{}
	bad := 0
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var ev execute.Event
		if err := json.Unmarshal([]byte(line), &ev); err != nil || ev.Type == "" {
			bad++
			continue
		}
		if !since.IsZero() && ev.Time.Before(since) {
			continue
		}
		id := ev.RecommendationID
		switch ev.Type {
		case "decision":
			if id == "" {
				continue
			}
			if mark("evaluated", id) {
				s.Evaluated++
			}
			if allowed, _ := ev.Details["allowed"].(bool); allowed {
				if mark("allowed", id) {
					s.Allowed++
				}
			} else if mark("denied", id) && !sets["allowed"][id] {
				s.Denied++
				if rs, ok := ev.Details["reasons"].([]any); ok && len(rs) > 0 {
					if first, ok := rs[0].(string); ok {
						if len(first) > 80 {
							first = first[:80]
						}
						denials[first]++
					}
				}
			}
		case "applied":
			if mark("applied", id) {
				s.Applied++
				c, _ := ev.Details["cpuPotentialMonthlyUSD"].(float64)
				m, _ := ev.Details["memoryPotentialMonthlyUSD"].(float64)
				saving[id] = c + m
			}
		case "verified":
			if mark("verified", id) {
				s.Verified++
			}
		case "degraded":
			if mark("degraded", id) {
				s.Degraded++
			}
		case "rolled_back":
			if mark("rolled_back", id) {
				s.RolledBack++
			}
		case "apply_failed", "rollback_failed", "inspect_failed":
			if mark("failed", id+"/"+ev.Type) {
				s.Failed++
			}
		}
	}
	if err := sc.Err(); err != nil {
		return s, err
	}
	for id, v := range saving {
		switch {
		case sets["rolled_back"][id]:
			s.RolledBackEstimatedMonthlyUSD += v
		case sets["verified"][id]:
			s.KeptEstimatedMonthlyUSD += v
		default:
			s.PendingEstimatedMonthlyUSD += v
		}
	}
	if s.Evaluated > 0 {
		s.AcceptanceRate = float64(s.Applied) / float64(s.Evaluated)
	}
	if s.Applied > 0 {
		s.RollbackRate = float64(s.RolledBack) / float64(s.Applied)
	}
	for r, c := range denials {
		s.TopDenials = append(s.TopDenials, Denial{r, c})
	}
	sort.Slice(s.TopDenials, func(i, j int) bool {
		if s.TopDenials[i].Count != s.TopDenials[j].Count {
			return s.TopDenials[i].Count > s.TopDenials[j].Count
		}
		return s.TopDenials[i].Reason < s.TopDenials[j].Reason
	})
	if len(s.TopDenials) > 5 {
		s.TopDenials = s.TopDenials[:5]
	}
	if bad > 0 {
		s.Notes = append(s.Notes, fmt.Sprintf("%d line(s) were not valid audit events and were ignored", bad))
	}
	s.Notes = append(s.Notes, "savings figures are ESTIMATES of requests reduced x unit prices; they are not observed or billed savings (see docs/COST.md)")
	return s, nil
}

// WritePrometheus writes the summary in the Prometheus text exposition format, for the node_exporter textfile collector or a
// Pushgateway, so that a team running only the CLI (observe-only, the default) gets the same dashboards as the operator's
// /metrics. The figures are those of Summarize; the USD gauges are ESTIMATES (see the HELP lines).
func WritePrometheus(w io.Writer, s Summary) error {
	var b strings.Builder
	b.WriteString("# HELP kubecost_audit_recommendations Distinct recommendation IDs per outcome in the audit trail.\n")
	b.WriteString("# TYPE kubecost_audit_recommendations gauge\n")
	for _, kv := range []struct {
		k string
		v int
	}{{"evaluated", s.Evaluated}, {"allowed", s.Allowed}, {"denied", s.Denied}, {"applied", s.Applied}, {"verified", s.Verified},
		{"degraded", s.Degraded}, {"rolled_back", s.RolledBack}, {"failed", s.Failed}} {
		fmt.Fprintf(&b, "kubecost_audit_recommendations{outcome=%q} %d\n", kv.k, kv.v)
	}
	if s.Evaluated > 0 {
		b.WriteString("# HELP kubecost_audit_acceptance_ratio Applied / evaluated recommendations in the audit trail.\n")
		b.WriteString("# TYPE kubecost_audit_acceptance_ratio gauge\n")
		fmt.Fprintf(&b, "kubecost_audit_acceptance_ratio %g\n", s.AcceptanceRate)
	}
	if s.Applied > 0 {
		b.WriteString("# HELP kubecost_audit_rollback_ratio Rolled back / applied recommendations in the audit trail.\n")
		b.WriteString("# TYPE kubecost_audit_rollback_ratio gauge\n")
		fmt.Fprintf(&b, "kubecost_audit_rollback_ratio %g\n", s.RollbackRate)
	}
	b.WriteString("# HELP kubecost_audit_estimated_monthly_usd ESTIMATED monthly saving of applied changes by state (kept, pending, rolled_back). Not a bill and not an observed saving.\n")
	b.WriteString("# TYPE kubecost_audit_estimated_monthly_usd gauge\n")
	fmt.Fprintf(&b, "kubecost_audit_estimated_monthly_usd{state=\"kept\"} %g\n", s.KeptEstimatedMonthlyUSD)
	fmt.Fprintf(&b, "kubecost_audit_estimated_monthly_usd{state=\"pending\"} %g\n", s.PendingEstimatedMonthlyUSD)
	fmt.Fprintf(&b, "kubecost_audit_estimated_monthly_usd{state=\"rolled_back\"} %g\n", s.RolledBackEstimatedMonthlyUSD)
	_, err := io.WriteString(w, b.String())
	return err
}
