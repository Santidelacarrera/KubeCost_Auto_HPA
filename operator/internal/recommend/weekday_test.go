package recommend

import (
	"strings"
	"testing"
	"time"
)

// hourly returns one timestamp per hour in [start, end), skipping the hours whose weekday is in skip.
func hourly(start, end time.Time, skip ...time.Weekday) []time.Time {
	var out []time.Time
	for h := start; h.Before(end); h = h.Add(time.Hour) {
		drop := false
		for _, d := range skip {
			if h.Weekday() == d {
				drop = true
			}
		}
		if !drop {
			out = append(out, h)
		}
	}
	return out
}

func weekInput() Input {
	in := good()
	in.WindowStart = t0.Add(-8 * 24 * time.Hour)
	in.CPUSamples, in.MemSamples = series(2000, 0.2), series(2000, 1<<30)
	return in
}

func TestFullWeekKeepsHighConfidence(t *testing.T) {
	in := weekInput()
	in.CPUTimes = hourly(in.WindowStart, in.WindowEnd)
	r := mustRec(t, in, cfg())
	if r.Confidence != ConfHigh {
		t.Fatalf("a full week of data should stay High, got %s (risks %v)", r.Confidence, r.Risks)
	}
	for _, x := range r.Risks {
		if strings.Contains(x, "weekly seasonality") {
			t.Fatalf("unexpected seasonality risk: %s", x)
		}
	}
}

func TestWeekWithAMissingWeekdayIsNotHighConfidence(t *testing.T) {
	in := weekInput()
	in.CPUTimes = hourly(in.WindowStart, in.WindowEnd, time.Monday) // e.g. scraping was down every Monday, or the data has a gap
	r := mustRec(t, in, cfg())
	if r.Confidence != ConfMed {
		t.Fatalf("a week without Monday must drop to Medium, got %s", r.Confidence)
	}
	found := false
	for _, x := range r.Risks {
		if strings.Contains(x, "Monday") && strings.Contains(x, "weekly seasonality is not fully observed") {
			found = true
		}
	}
	if !found {
		t.Fatalf("the risk must name the missing day: %v", r.Risks)
	}
}

func TestMissingWeekdaysEdges(t *testing.T) {
	start := t0.Add(-8 * 24 * time.Hour)
	if got := MissingWeekdays(start, t0, nil); got != nil {
		t.Fatalf("no timestamps means nothing to say, got %v", got)
	}
	short := t0.Add(-3 * 24 * time.Hour)
	if got := MissingWeekdays(short, t0, hourly(short, t0)); got != nil {
		t.Fatalf("a short window is the window-length caveat's job, got %v", got)
	}
	// Data for only a quarter of Saturday's hours: Saturday is reported, in Monday-first order with Sunday last.
	var ts []time.Time
	for _, h := range hourly(start, t0) {
		if h.Weekday() == time.Saturday && h.Hour()%4 != 0 {
			continue
		}
		if h.Weekday() == time.Sunday {
			continue
		}
		ts = append(ts, h)
	}
	got := MissingWeekdays(start, t0, ts)
	if strings.Join(got, ",") != "Saturday,Sunday" {
		t.Fatalf("got %v", got)
	}
	// Samples outside the window do not count.
	outside := hourly(start.Add(-30*24*time.Hour), start)
	if got := MissingWeekdays(start, t0, outside); len(got) != 7 {
		t.Fatalf("samples outside the window must not count, got %v", got)
	}
}
