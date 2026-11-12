package recommend

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
)

// Combine joins per-container recommendations of ONE workload into a single record, so that the pod template is changed once (one
// rollout, one restart of each pod) and a rollback restores every container together. The first recommendation is the primary
// container (the one a WorkloadRecommendation names in spec.target.container); the others become Additional.
//
// The combined record is no more confident than its least confident container, abstains only when every container abstains, and
// gets a new ID derived from the members' IDs.
func Combine(recs ...Recommendation) (Recommendation, error) {
	if len(recs) == 0 {
		return Recommendation{}, fmt.Errorf("no recommendation to combine")
	}
	p := recs[0]
	if len(p.Additional) > 0 {
		return Recommendation{}, fmt.Errorf("the primary recommendation is already combined")
	}
	if len(recs) == 1 {
		return p, nil
	}
	seen := map[string]bool{p.Container: true}
	ids := []string{p.ID}
	all := p.Abstained
	conf := p.Confidence
	for _, r := range recs[1:] {
		switch {
		case len(r.Additional) > 0:
			return Recommendation{}, fmt.Errorf("container %q is already a combined record", r.Container)
		case r.Cluster != p.Cluster || r.Namespace != p.Namespace || r.Kind != p.Kind || r.Workload != p.Workload:
			return Recommendation{}, fmt.Errorf("container %q is for %s/%s/%s, not %s/%s/%s", r.Container, r.Namespace, r.Kind, r.Workload, p.Namespace, p.Kind, p.Workload)
		case seen[r.Container]:
			return Recommendation{}, fmt.Errorf("container %q appears twice", r.Container)
		}
		seen[r.Container] = true
		ids = append(ids, r.ID)
		all = all && r.Abstained
		if confRank(r.Confidence) < confRank(conf) {
			conf = r.Confidence
		}
		p.Additional = append(p.Additional, ContainerResult{Container: r.Container, Samples: r.Samples, Confidence: r.Confidence,
			Abstained: r.Abstained, CPU: r.CPU, Memory: r.Memory, Risks: r.Risks})
		for _, risk := range r.Risks {
			p.Risks = append(p.Risks, "container "+r.Container+": "+risk)
		}
	}
	sort.Strings(ids[1:])
	h := sha256.Sum256([]byte(fmt.Sprint(ids)))
	p.ID = hex.EncodeToString(h[:8])
	p.Confidence = conf
	p.Abstained = all
	return p, nil
}

// Views returns one single-container Recommendation per container of the record, primary first, each carrying the record's
// identity (ID, target, window, config) and that container's own results. The policy and the executor check every view.
func (r Recommendation) Views() []Recommendation {
	base := r
	base.Additional = nil
	out := []Recommendation{base}
	for _, c := range r.Additional {
		v := base
		v.Container, v.Samples, v.Confidence, v.Abstained, v.CPU, v.Memory, v.Risks = c.Container, c.Samples, c.Confidence, c.Abstained, c.CPU, c.Memory, c.Risks
		out = append(out, v)
	}
	return out
}

// Changes reports whether this container's results would change its requests.
func (r Recommendation) Changes() bool {
	ch := func(a Action) bool { return a == Increase || a == Decrease }
	return ch(r.CPU.Action) || ch(r.Memory.Action)
}

// PotentialMonthlyUSD sums the ESTIMATED saving over every container of the record.
func (r Recommendation) PotentialMonthlyUSD() (cpu, mem float64) {
	for _, v := range r.Views() {
		cpu += v.CPU.PotentialMonthlyUSD
		mem += v.Memory.PotentialMonthlyUSD
	}
	return cpu, mem
}

func confRank(c Confidence) int {
	switch c {
	case ConfHigh:
		return 3
	case ConfMed:
		return 2
	case ConfLow:
		return 1
	}
	return 0
}
