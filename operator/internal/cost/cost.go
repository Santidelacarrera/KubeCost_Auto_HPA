// Package cost allocates infrastructure cost to namespaces and reconciles the allocation with billing.
//
// Three different things are never mixed (docs/COST.md):
//   - Estimated: computed from unit prices that someone supplied (a price list, a flag). A model, not a bill.
//   - Billed:    cost line items from a billing source (a provider export). The only figure that is real money.
//   - Allocated: billed (or estimated) node cost split across namespaces by a documented rule. Always sums back to its input.
//
// Nothing here fetches prices or bills: adapters supply BillingItems, and the report says which basis it used.
package cost

import (
	"context"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/money"
)

// Basis states where the node cost came from.
type Basis string

const (
	BasisBilled    Basis = "billed"
	BasisEstimated Basis = "estimated"
)

// Special buckets in a report.
const (
	Idle        = "__idle__"        // node capacity nobody requested
	Unallocated = "__unallocated__" // billed items that map to no node
)

// BillingItem is one line of a billing source.
type BillingItem struct {
	Resource   string // the node name (or instance id mapped to it); empty = not tied to a node
	Cost       money.USD
	Start, End time.Time
}

// Adapter is the seam for provider billing. Implementations must return real billing data or an error, never invented numbers.
type Adapter interface {
	Name() string
	Billed(ctx context.Context, start, end time.Time) ([]BillingItem, error)
}

// Node is a node's capacity as scheduled.
type Node struct {
	Name                           string
	CPUAllocatable, MemAllocatable float64 // cores, bytes
}

// Pod is a pod's request footprint on a node.
type Pod struct {
	Namespace, Node        string
	CPURequest, MemRequest float64
}

// Config holds the allocation assumptions. They are inputs, echoed in the report.
type Config struct {
	// CPUWeight is the fraction of a node's cost attributed to CPU (the rest to memory). Default 0.5. It is a convention, not a market price.
	CPUWeight float64
	// SharedNamespaces (e.g. kube-system) are removed from the table and redistributed over the other namespaces in proportion to their cost.
	SharedNamespaces []string
	// Tolerance for Reconciled: |billed - accounted| / billed. Default 0.01.
	Tolerance float64
}

// Report is the result. All money fields are exact (micro-dollars).
type Report struct {
	Basis       Basis
	Source      string
	Start, End  time.Time
	Config      Config
	Total       money.USD            // sum of the input node costs plus unallocated items
	ByNamespace map[string]money.USD // includes Idle and, if any, Unallocated; shared namespaces already redistributed
	Shared      money.USD            // cost of the shared namespaces that was redistributed
	Accounted   money.USD            // sum of ByNamespace
	Difference  money.USD            // Total - Accounted (rounding only; non-zero beyond tolerance means a bug or missing data)
	Reconciled  bool
	Notes       []string
}

func (c Config) withDefaults() Config {
	if c.CPUWeight <= 0 || c.CPUWeight >= 1 {
		c.CPUWeight = 0.5
	}
	if c.Tolerance <= 0 {
		c.Tolerance = 0.01
	}
	return c
}

// Allocate splits each node's cost over its pods by request share (CPU and memory weighted), puts the unrequested remainder in Idle,
// redistributes shared namespaces, and adds billed items that match no node under Unallocated.
//
// nodeCost maps node name -> cost for the window. extra are billed items not tied to a node (e.g. load balancers).
func Allocate(basis Basis, source string, start, end time.Time, nodes []Node, pods []Pod, nodeCost map[string]money.USD, extra []BillingItem, cfg Config) (Report, error) {
	cfg = cfg.withDefaults()
	r := Report{Basis: basis, Source: source, Start: start, End: end, Config: cfg, ByNamespace: map[string]money.USD{}}
	if basis != BasisBilled && basis != BasisEstimated {
		return r, fmt.Errorf("unknown basis %q", basis)
	}
	if end.Before(start) {
		return r, fmt.Errorf("window ends before it starts")
	}
	byNode := map[string][]Pod{}
	for _, p := range pods {
		byNode[p.Node] = append(byNode[p.Node], p)
	}
	known := map[string]Node{}
	for _, n := range nodes {
		known[n.Name] = n
	}

	names := make([]string, 0, len(nodeCost))
	for n := range nodeCost {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		c := nodeCost[name]
		if c < 0 {
			return r, fmt.Errorf("negative cost for node %s", name)
		}
		r.Total += c
		n, ok := known[name]
		if !ok || n.CPUAllocatable <= 0 || n.MemAllocatable <= 0 {
			r.ByNamespace[Unallocated] += c
			r.Notes = append(r.Notes, fmt.Sprintf("node %q has cost but no usable capacity data: its cost is unallocated", name))
			continue
		}
		var cpuReq, memReq float64
		for _, p := range byNode[name] {
			cpuReq += p.CPURequest
			memReq += p.MemRequest
		}
		// requests above allocatable (should not happen) are scaled down so shares never exceed 100%
		scale := math.Max(1, math.Max(cpuReq/n.CPUAllocatable, memReq/n.MemAllocatable))
		var given money.USD
		type share struct {
			ns string
			f  float64
		}
		shares := map[string]float64{}
		for _, p := range byNode[name] {
			f := cfg.CPUWeight*p.CPURequest/n.CPUAllocatable/scale + (1-cfg.CPUWeight)*p.MemRequest/n.MemAllocatable/scale
			shares[p.Namespace] += f
		}
		nss := make([]string, 0, len(shares))
		for ns := range shares {
			nss = append(nss, ns)
		}
		sort.Strings(nss)
		for _, ns := range nss {
			amt, err := money.FromFloat(c.Float() * shares[ns])
			if err != nil {
				return r, err
			}
			if given+amt > c { // rounding guard
				amt = c - given
			}
			r.ByNamespace[ns] += amt
			given += amt
		}
		r.ByNamespace[Idle] += c - given
	}
	for _, it := range extra {
		if it.Cost < 0 {
			return r, fmt.Errorf("negative billed item %q", it.Resource)
		}
		r.Total += it.Cost
		r.ByNamespace[Unallocated] += it.Cost
	}

	// shared namespaces: redistribute over the remaining (non-special) namespaces in proportion to their cost
	var shared money.USD
	for _, ns := range cfg.SharedNamespaces {
		shared += r.ByNamespace[ns]
		delete(r.ByNamespace, ns)
	}
	if shared > 0 {
		var base money.USD
		for ns, v := range r.ByNamespace {
			if ns != Idle && ns != Unallocated {
				base += v
			}
		}
		if base == 0 {
			r.ByNamespace["__shared__"] = shared
			r.Notes = append(r.Notes, "shared cost kept apart: there is no other namespace to distribute it to")
		} else {
			var given money.USD
			nss := make([]string, 0)
			for ns := range r.ByNamespace {
				if ns != Idle && ns != Unallocated {
					nss = append(nss, ns)
				}
			}
			sort.Strings(nss)
			for i, ns := range nss {
				add := shared.MulRat(int64(r.ByNamespace[ns]), int64(base))
				if i == len(nss)-1 {
					add = shared - given
				}
				r.ByNamespace[ns] += add
				given += add
			}
		}
	}
	r.Shared = shared
	for _, v := range r.ByNamespace {
		r.Accounted += v
	}
	r.Difference = r.Total - r.Accounted
	if r.Total == 0 {
		r.Reconciled = r.Accounted == 0
	} else {
		r.Reconciled = math.Abs(r.Difference.Float())/r.Total.Float() <= cfg.Tolerance
	}
	if r.ByNamespace[Unallocated] > 0 {
		r.Notes = append(r.Notes, "part of the cost is unallocated (no node mapping): do not read namespace totals as the whole bill")
	}
	return r, nil
}

// EstimateNodeCost is Estimated cost: hourly price x hours. The price is supplied by the caller and is not looked up here.
func EstimateNodeCost(hourly money.USD, start, end time.Time) money.USD {
	h := end.Sub(start).Hours()
	if h <= 0 {
		return 0
	}
	return hourly.MulRat(int64(math.Round(h*3600)), 3600)
}

// Savings keeps the three kinds of saving apart. A nil pointer means "not measurable with the data given", never zero.
type Savings struct {
	// Potential is what a recommendation would save if applied and if the freed requests were actually reclaimed (an estimate).
	Potential money.USD
	// Observed compares allocated cost of equivalent windows before and after a change (see ObservedSavings).
	Observed *money.USD
	// Billed is the difference between real bills of equivalent periods; only set from two billed reports.
	Billed *money.USD
	Notes  []string
}

// ObservedSavings compares the allocation of one namespace in two reports, normalised to cost per hour so unequal windows compare fairly.
// If requests are given (served requests in each window) the comparison is also normalised by traffic and reported in Notes so a traffic
// drop is not mistaken for a saving. Only two Billed reports yield a Billed saving; otherwise it stays nil.
func ObservedSavings(before, after Report, namespace string, potential money.USD, reqBefore, reqAfter float64) (Savings, error) {
	s := Savings{Potential: potential}
	hb, ha := before.End.Sub(before.Start).Hours(), after.End.Sub(after.Start).Hours()
	if hb <= 0 || ha <= 0 {
		return s, fmt.Errorf("both windows must have a positive length")
	}
	cb, ca := before.ByNamespace[namespace], after.ByNamespace[namespace]
	perHourBefore, perHourAfter := cb.Float()/hb, ca.Float()/ha
	diff, err := money.FromFloat(perHourBefore - perHourAfter)
	if err != nil {
		return s, err
	}
	s.Observed = &diff // USD per hour
	s.Notes = append(s.Notes, "observed saving is USD per hour of ALLOCATED cost: it is not an infrastructure saving unless nodes were actually released")
	if before.Basis != after.Basis {
		s.Notes = append(s.Notes, "the two reports use different bases: the comparison is not valid")
		s.Observed = nil
		return s, nil
	}
	if reqBefore > 0 && reqAfter > 0 {
		ub, ua := perHourBefore/(reqBefore/hb), perHourAfter/(reqAfter/ha)
		s.Notes = append(s.Notes, fmt.Sprintf("cost per request: before %.8f, after %.8f USD (traffic %.0f -> %.0f requests/h)", ub, ua, reqBefore/hb, reqAfter/ha))
		if reqAfter/ha < 0.8*reqBefore/hb {
			s.Notes = append(s.Notes, "traffic fell by more than 20%: part of any saving is lower demand, not optimisation")
		}
	}
	if before.Basis == BasisBilled && after.Basis == BasisBilled {
		b := diff
		s.Billed = &b
		s.Notes = append(s.Notes, "billed saving is the change in the namespace share of BILLED node cost (allocated from the bill by request share); a per-namespace invoice does not exist")
	} else {
		s.Notes = append(s.Notes, "no billed saving: it needs billed reports for both periods")
	}
	return s, nil
}
