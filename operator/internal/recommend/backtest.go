package recommend

import (
	"fmt"
	"math"
	"sort"
)

// Setting is one candidate (percentile, headroom) pair.
type Setting struct {
	Percentile float64
	Headroom   float64
}

// BacktestRow is what a setting WOULD have recommended from the training part and how it fared on the unseen test part.
type BacktestRow struct {
	Setting
	Recommended float64
	// ExceedFraction is the share of test samples whose usage was above the recommendation (CPU: demand beyond the request;
	// memory: a sample above the request, which is OOM risk only when it also approaches the limit).
	ExceedFraction float64
	// PeakRatio is the largest test sample divided by the recommendation (above 1 means the peak was not covered).
	PeakRatio float64
	// Waste is the share of the recommendation that the test p95 did not use: (recommended - test p95) / recommended.
	Waste float64
}

// BacktestResult is a sweep over settings for one resource.
type BacktestResult struct {
	TrainSamples, TestSamples int
	TestP95, TestMax          float64
	Rows                      []BacktestRow
	// Best is the row with the least Waste among those with ExceedFraction <= the target; nil if none qualifies.
	Best *BacktestRow
	// Notes lists the reasons the result must not be trusted blindly.
	Notes []string
}

// DefaultGrid is a small, readable sweep.
func DefaultGrid() []Setting {
	var g []Setting
	for _, p := range []float64{0.90, 0.95, 0.99, 1.0} {
		for _, h := range []float64{0.05, 0.10, 0.15, 0.20, 0.30} {
			g = append(g, Setting{p, h})
		}
	}
	return g
}

// Backtest evaluates every setting: it computes the recommendation from train only and measures it on test only, so the figures are out-of-sample.
// targetExceed is the tolerated exceed fraction (for example 0.01). It needs enough data on both sides and says so otherwise.
func Backtest(train, test []float64, grid []Setting, targetExceed float64, minSamples int) (BacktestResult, error) {
	var r BacktestResult
	tr, te := clean(train), clean(test)
	r.TrainSamples, r.TestSamples = len(tr), len(te)
	if targetExceed < 0 || targetExceed >= 1 {
		return r, fmt.Errorf("targetExceed must be in [0,1)")
	}
	if len(tr) < minSamples || len(te) < minSamples/2 || len(te) == 0 {
		return r, fmt.Errorf("not enough data: %d training and %d test samples (need %d and %d)", len(tr), len(te), minSamples, minSamples/2)
	}
	r.TestP95, r.TestMax = Percentile(te, 0.95), te[len(te)-1]
	for _, s := range grid {
		if s.Percentile <= 0 || s.Percentile > 1 || s.Headroom < 0 {
			return r, fmt.Errorf("invalid setting %+v", s)
		}
		rec := Percentile(tr, s.Percentile) * (1 + s.Headroom)
		row := BacktestRow{Setting: s, Recommended: rec}
		if rec > 0 {
			over := 0
			for _, v := range te {
				if v > rec {
					over++
				}
			}
			row.ExceedFraction = float64(over) / float64(len(te))
			row.PeakRatio = r.TestMax / rec
			row.Waste = math.Max(0, (rec-r.TestP95)/rec)
		} else {
			row.ExceedFraction = 1
		}
		r.Rows = append(r.Rows, row)
	}
	sort.SliceStable(r.Rows, func(i, j int) bool {
		if r.Rows[i].Percentile != r.Rows[j].Percentile {
			return r.Rows[i].Percentile < r.Rows[j].Percentile
		}
		return r.Rows[i].Headroom < r.Rows[j].Headroom
	})
	for i := range r.Rows {
		row := &r.Rows[i]
		if row.ExceedFraction <= targetExceed && (r.Best == nil || row.Waste < r.Best.Waste) {
			r.Best = row
		}
	}
	if r.Best == nil {
		r.Notes = append(r.Notes, "no setting in the grid kept the exceed fraction under the target: the load is too spiky for these percentiles, or the test period had a peak the training period never saw")
	}
	if r.TestMax > 2*r.TestP95 && r.TestP95 > 0 {
		r.Notes = append(r.Notes, "the test period has a spike above twice its own p95: a percentile-based request will not cover it")
	}
	r.Notes = append(r.Notes, "one split is one sample of the future: repeat with other holdout periods (including a weekly peak) before changing defaults")
	return r, nil
}

// SplitSeries cuts every per-pod series at the same fraction and concatenates the parts, so train always precedes test in time.
func SplitSeries(series [][]float64, testFraction float64) (train, test []float64) {
	for _, s := range series {
		n := len(s)
		cut := n - int(math.Round(float64(n)*testFraction))
		if cut < 0 {
			cut = 0
		}
		train = append(train, s[:cut]...)
		test = append(test, s[cut:]...)
	}
	return train, test
}
