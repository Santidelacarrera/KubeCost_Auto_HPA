package recommend

import (
	"math"
	"strings"
	"testing"
)

// ramp returns n samples that rise linearly from lo to hi.
func ramp(n int, lo, hi float64) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = lo + (hi-lo)*float64(i)/float64(n-1)
	}
	return out
}

func TestBacktestIsOutOfSampleAndPicksTheLeastWasteUnderTheTarget(t *testing.T) {
	train := ramp(400, 0.1, 0.3)
	test := ramp(200, 0.1, 0.3)
	r, err := Backtest(train, test, DefaultGrid(), 0.01, 100)
	if err != nil {
		t.Fatal(err)
	}
	if r.Best == nil {
		t.Fatalf("a stationary load must have a qualifying setting: %+v", r.Notes)
	}
	if r.Best.ExceedFraction > 0.01 {
		t.Fatalf("%+v", r.Best)
	}
	for _, row := range r.Rows {
		if row.ExceedFraction <= 0.01 && row.Waste < r.Best.Waste-1e-12 {
			t.Fatalf("a better qualifying row exists: %+v vs %+v", row, r.Best)
		}
	}
	// p100 with 5% headroom covers a stationary test fully
	for _, row := range r.Rows {
		if row.Percentile == 1.0 && row.Headroom == 0.05 && row.ExceedFraction != 0 {
			t.Fatalf("%+v", row)
		}
	}
}

func TestBacktestSeesThePeakTheTrainingNeverHad(t *testing.T) {
	train := series(400, 0.2)
	test := append(series(190, 0.2), series(10, 1.5)...) // a spike after training
	r, err := Backtest(train, test, DefaultGrid(), 0.01, 100)
	if err != nil {
		t.Fatal(err)
	}
	if r.Best != nil {
		t.Fatalf("no setting can have covered an unseen 7x spike: %+v", r.Best)
	}
	joined := strings.Join(r.Notes, " ")
	if !strings.Contains(joined, "no setting") || !strings.Contains(joined, "spike") {
		t.Fatalf("%v", r.Notes)
	}
	for _, row := range r.Rows {
		if row.PeakRatio < 1 {
			t.Fatalf("peak ratio must exceed 1 when the peak is uncovered: %+v", row)
		}
	}
}

func TestBacktestRefusesTooLittleDataAndBadInput(t *testing.T) {
	if _, err := Backtest(series(10, 1), series(10, 1), DefaultGrid(), 0.01, 100); err == nil || !strings.Contains(err.Error(), "not enough data") {
		t.Fatalf("%v", err)
	}
	if _, err := Backtest(series(400, 1), series(200, 1), []Setting{{1.5, 0}}, 0.01, 100); err == nil {
		t.Fatal("invalid percentile accepted")
	}
	if _, err := Backtest(series(400, 1), series(200, 1), DefaultGrid(), 1, 100); err == nil {
		t.Fatal("targetExceed of 1 accepted")
	}
	if _, err := Backtest(append(series(400, 1), math.NaN()), series(200, 1), DefaultGrid(), 0.01, 100); err != nil {
		t.Fatalf("NaN samples must be ignored, not fatal: %v", err)
	}
}

func TestSplitSeriesKeepsTimeOrderPerPod(t *testing.T) {
	a, b := []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}, []float64{11, 12, 13, 14, 15, 16, 17, 18, 19, 20}
	train, test := SplitSeries([][]float64{a, b}, 0.2)
	if len(train) != 16 || len(test) != 4 || test[0] != 9 || test[1] != 10 || test[2] != 19 || test[3] != 20 {
		t.Fatalf("%v | %v", train, test)
	}
	for _, v := range train {
		if v == 10 || v == 20 || v == 9 || v == 19 {
			t.Fatalf("test data leaked into training: %v", train)
		}
	}
}
