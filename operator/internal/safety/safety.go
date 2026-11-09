// Package safety decides whether a recommendation MAY be applied. It is pure and fail-closed:
// an empty policy denies everything, any unknown fact denies, and every denial carries its reasons.
//
// It is independent of the recommender (ADR-0002): the recommender never sees the policy and the policy
// never computes a number; it only says yes or no, and why.
package safety

import (
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/recommend"
)

// Window is a UTC maintenance window. Start/End are "HH:MM"; End <= Start wraps past midnight.
type Window struct {
	Days  []string `json:"days,omitempty"` // "Mon".."Sun"; empty = every day
	Start string   `json:"start,omitempty"`
	End   string   `json:"end,omitempty"`
}

// Policy is the explicit authorisation. The zero value authorises nothing.
type Policy struct {
	// KillSwitch denies everything while true.
	KillSwitch bool `json:"killSwitch,omitempty"`
	// AllowNamespaces: only these namespaces may be changed. Empty = none.
	AllowNamespaces []string `json:"allowNamespaces,omitempty"`
	// AllowWorkloads / DenyWorkloads are "namespace/Kind/name" globs (path.Match). Deny wins. Empty allow = every workload of an allowed namespace.
	AllowWorkloads []string `json:"allowWorkloads,omitempty"`
	DenyWorkloads  []string `json:"denyWorkloads,omitempty"`
	// DenyLabels excludes workloads carrying any of these labels (value "" matches any value).
	DenyLabels map[string]string `json:"denyLabels,omitempty"`

	// MaxChangeFraction bounds |new-old|/old per resource per step (default 0.5).
	MaxChangeFraction float64 `json:"maxChangeFraction,omitempty"`
	MinCPU            float64 `json:"minCPU,omitempty"`
	MaxCPU            float64 `json:"maxCPU,omitempty"`
	MinMem            float64 `json:"minMem,omitempty"`
	MaxMem            float64 `json:"maxMem,omitempty"`
	// MinConfidence: "Low", "Medium" (default) or "High".
	MinConfidence string `json:"minConfidence,omitempty"`

	// Windows: if non-empty, changes are only allowed inside one of them.
	Windows []Window `json:"windows,omitempty"`
	// MaxChanges per Per (default 1 per hour) across the namespace; counted from the recent applies passed in.
	MaxChanges int    `json:"maxChanges,omitempty"`
	Per        string `json:"per,omitempty"` // Go duration, e.g. "1h"

	// RequireApproval defaults to TRUE: a human must annotate the workload with the recommendation ID.
	RequireApproval *bool `json:"requireApproval,omitempty"`
	// AllowGitOps lets the apply proceed on a workload managed by Argo CD / Flux. Default false.
	AllowGitOps bool `json:"allowGitOps,omitempty"`
	// AllowCPUWithHPA lets a CPU request change proceed when an HPA scales on CPU utilisation. Default false.
	AllowCPUWithHPA bool `json:"allowCPUWithHPA,omitempty"`
}

// Facts are what the executor observed about the target. Unknown facts must be reported as Known=false.
type Facts struct {
	Namespace, Kind, Name string
	Labels                map[string]string
	// Approved is the value of the approval annotation (the recommendation ID a human approved).
	Approved string
	// RolledBack is the ID of the last recommendation that was rolled back on this workload.
	RolledBack string
	// AlreadyApplied is the ID of the recommendation currently applied (idempotency).
	AlreadyApplied string
	GitOps         string // "argocd", "flux" or ""
	HPAOnCPU       bool
	VPAAuto        bool
	// Known is false when the facts could not be gathered (API error): the policy then denies.
	Known bool
}

// Decision is the verdict plus every reason, for the audit log.
type Decision struct {
	Allowed bool
	Reasons []string
	// Approved, if true, means only the human approval is missing (useful to tell "ask a human" from "never").
	NeedsApproval bool
}

func (p Policy) approval() bool { return p.RequireApproval == nil || *p.RequireApproval }

func (p Policy) maxFrac() float64 {
	if p.MaxChangeFraction <= 0 {
		return 0.5
	}
	return p.MaxChangeFraction
}

// Validate rejects policies that cannot be enforced as written.
func (p Policy) Validate() error {
	if p.MaxChangeFraction < 0 || p.MaxChangeFraction >= 1 && p.MaxChangeFraction != 0 {
		return fmt.Errorf("maxChangeFraction must be in (0,1)")
	}
	if p.MinCPU < 0 || p.MinMem < 0 || p.MaxCPU < 0 || p.MaxMem < 0 || (p.MaxCPU > 0 && p.MinCPU > p.MaxCPU) || (p.MaxMem > 0 && p.MinMem > p.MaxMem) {
		return fmt.Errorf("invalid min/max bounds")
	}
	switch p.MinConfidence {
	case "", "Low", "Medium", "High":
	default:
		return fmt.Errorf("minConfidence %q is not Low, Medium or High", p.MinConfidence)
	}
	for _, g := range append(append([]string{}, p.AllowWorkloads...), p.DenyWorkloads...) {
		if _, err := path.Match(g, "x"); err != nil {
			return fmt.Errorf("bad workload pattern %q: %w", g, err)
		}
	}
	for _, w := range p.Windows {
		if _, _, err := w.bounds(); err != nil {
			return err
		}
		for _, d := range w.Days {
			if _, ok := weekdays[strings.ToLower(d)]; !ok {
				return fmt.Errorf("bad weekday %q", d)
			}
		}
	}
	if p.MaxChanges < 0 {
		return fmt.Errorf("maxChanges must not be negative")
	}
	if p.Per != "" {
		if d, err := time.ParseDuration(p.Per); err != nil || d <= 0 {
			return fmt.Errorf("per %q must be a positive duration", p.Per)
		}
	}
	return nil
}

var weekdays = map[string]time.Weekday{"sun": time.Sunday, "mon": time.Monday, "tue": time.Tuesday, "wed": time.Wednesday, "thu": time.Thursday, "fri": time.Friday, "sat": time.Saturday}

func (w Window) bounds() (int, int, error) {
	parse := func(s string) (int, error) {
		t, err := time.Parse("15:04", s)
		if err != nil {
			return 0, fmt.Errorf("window time %q must be HH:MM", s)
		}
		return t.Hour()*60 + t.Minute(), nil
	}
	a, err := parse(w.Start)
	if err != nil {
		return 0, 0, err
	}
	b, err := parse(w.End)
	return a, b, err
}

func (w Window) contains(t time.Time) bool {
	a, b, err := w.bounds()
	if err != nil {
		return false
	}
	t = t.UTC()
	m := t.Hour()*60 + t.Minute()
	day := t.Weekday()
	inDay := func(d time.Weekday) bool {
		if len(w.Days) == 0 {
			return true
		}
		for _, x := range w.Days {
			if weekdays[strings.ToLower(x)] == d {
				return true
			}
		}
		return false
	}
	if a < b { // same day
		return inDay(day) && m >= a && m < b
	}
	// wraps midnight: the part after midnight belongs to the previous day's window
	return (inDay(day) && m >= a) || (inDay((day+6)%7) && m < b)
}

func rank(c recommend.Confidence) int {
	switch c {
	case recommend.ConfHigh:
		return 3
	case recommend.ConfMed:
		return 2
	}
	return 1
}

func minRank(s string) int {
	switch s {
	case "High":
		return 3
	case "Low":
		return 1
	}
	return 2
}

// Evaluate checks the recommendation against the policy. recent are the timestamps of changes already applied in the
// same namespace (for the rate limit). It never returns Allowed for a recommendation that changes nothing.
func Evaluate(p Policy, rec recommend.Recommendation, f Facts, now time.Time, recent []time.Time) Decision {
	var deny []string
	d := func(format string, a ...any) { deny = append(deny, fmt.Sprintf(format, a...)) }

	if err := p.Validate(); err != nil {
		d("invalid policy: %v", err)
	}
	if p.KillSwitch {
		d("kill switch is on")
	}
	if !f.Known {
		d("target facts could not be established (fail closed)")
	}
	if rec.Abstained {
		d("the recommender abstained")
	}
	changes := rec.CPU.Action == recommend.Increase || rec.CPU.Action == recommend.Decrease || rec.Memory.Action == recommend.Increase || rec.Memory.Action == recommend.Decrease
	if !changes {
		d("the recommendation changes nothing")
	}
	if f.Namespace != rec.Namespace || f.Name != rec.Workload || f.Kind != rec.Kind {
		d("facts describe %s/%s/%s but the recommendation is for %s/%s/%s", f.Namespace, f.Kind, f.Name, rec.Namespace, rec.Kind, rec.Workload)
	}

	if !contains(p.AllowNamespaces, f.Namespace) {
		d("namespace %q is not in allowNamespaces", f.Namespace)
	}
	id := f.Namespace + "/" + f.Kind + "/" + f.Name
	for _, g := range p.DenyWorkloads {
		if ok, _ := path.Match(g, id); ok {
			d("workload %s matches denyWorkloads %q", id, g)
		}
	}
	if len(p.AllowWorkloads) > 0 && !matchAny(p.AllowWorkloads, id) {
		d("workload %s is not in allowWorkloads", id)
	}
	for k, v := range p.DenyLabels {
		if lv, ok := f.Labels[k]; ok && (v == "" || lv == v) {
			d("workload carries the exclusion label %s", k)
		}
	}

	if rank(rec.Confidence) < minRank(p.MinConfidence) {
		d("confidence %s is below the required %s", rec.Confidence, orDefault(p.MinConfidence, "Medium"))
	}
	check := func(name string, r recommend.ResourceResult, lo, hi float64) {
		if r.Action != recommend.Increase && r.Action != recommend.Decrease {
			return
		}
		if r.Current > 0 {
			if ch := abs(r.Recommended-r.Current) / r.Current; ch > p.maxFrac()+1e-9 {
				d("%s change %.0f%% exceeds maxChangeFraction %.0f%%", name, ch*100, p.maxFrac()*100)
			}
		}
		if lo > 0 && r.Recommended < lo {
			d("%s %.4g is below the policy minimum %.4g", name, r.Recommended, lo)
		}
		if hi > 0 && r.Recommended > hi {
			d("%s %.4g is above the policy maximum %.4g", name, r.Recommended, hi)
		}
	}
	check("cpu", rec.CPU, p.MinCPU, p.MaxCPU)
	check("memory", rec.Memory, p.MinMem, p.MaxMem)

	if len(p.Windows) > 0 {
		in := false
		for _, w := range p.Windows {
			in = in || w.contains(now)
		}
		if !in {
			d("outside every maintenance window")
		}
	}
	per, max := time.Hour, p.MaxChanges
	if d, err := time.ParseDuration(p.Per); err == nil && d > 0 {
		per = d
	}
	if max <= 0 {
		max = 1
	}
	n := 0
	for _, t := range recent {
		if now.Sub(t) < per && !t.After(now) {
			n++
		}
	}
	if n >= max {
		d("rate limit: %d change(s) in the last %s (max %d)", n, per, max)
	}

	if f.GitOps != "" && !p.AllowGitOps {
		d("workload is managed by %s: a direct change would be reverted or fight the reconciler (use a pull request)", f.GitOps)
	}
	if f.VPAAuto {
		d("a VPA in Auto mode owns these requests")
	}
	if f.HPAOnCPU && (rec.CPU.Action == recommend.Increase || rec.CPU.Action == recommend.Decrease) && !p.AllowCPUWithHPA {
		d("an HPA scales on CPU utilisation: changing the CPU request shifts its target (allowCPUWithHPA is false)")
	}
	if f.AlreadyApplied == rec.ID {
		d("recommendation %s is already applied", rec.ID)
	}
	if f.RolledBack == rec.ID {
		d("recommendation %s was rolled back before and will not be retried", rec.ID)
	}

	dec := Decision{Reasons: deny}
	if p.approval() && f.Approved != rec.ID {
		dec.NeedsApproval = true
		dec.Reasons = append(dec.Reasons, fmt.Sprintf("not approved: annotate the workload with autoscaling.kubecost.io/approved-recommendation=%s", rec.ID))
	}
	dec.Allowed = len(dec.Reasons) == 0
	return dec
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

func matchAny(globs []string, s string) bool {
	for _, g := range globs {
		if ok, _ := path.Match(g, s); ok {
			return true
		}
	}
	return false
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

func abs(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}
