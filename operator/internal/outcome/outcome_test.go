package outcome

import (
	"strings"
	"testing"
	"time"
)

const audit = `{"time":"2026-11-09T10:00:00Z","type":"decision","actor":"a","recommendationId":"r1","target":"t","details":{"allowed":true}}
{"time":"2026-11-09T10:00:01Z","type":"applied","actor":"a","recommendationId":"r1","target":"t","details":{"cpuPotentialMonthlyUSD":10.5,"memoryPotentialMonthlyUSD":1.5}}
{"time":"2026-11-09T10:05:00Z","type":"verified","actor":"a","recommendationId":"r1","target":"t"}
{"time":"2026-11-09T11:00:00Z","type":"decision","actor":"a","recommendationId":"r2","target":"t","details":{"allowed":true}}
{"time":"2026-11-09T11:00:01Z","type":"applied","actor":"a","recommendationId":"r2","target":"t","details":{"cpuPotentialMonthlyUSD":20,"memoryPotentialMonthlyUSD":0}}
{"time":"2026-11-09T11:01:00Z","type":"degraded","actor":"a","recommendationId":"r2","target":"t"}
{"time":"2026-11-09T11:01:01Z","type":"rolled_back","actor":"a","recommendationId":"r2","target":"t"}
{"time":"2026-11-09T12:00:00Z","type":"decision","actor":"a","recommendationId":"r3","target":"t","details":{"allowed":false,"reasons":["not approved: run rightsize approve"]}}
{"time":"2026-11-09T12:01:00Z","type":"decision","actor":"a","recommendationId":"r3","target":"t","details":{"allowed":false,"reasons":["not approved: run rightsize approve"]}}
{"time":"2026-11-09T13:00:00Z","type":"decision","actor":"a","recommendationId":"r4","target":"t","details":{"allowed":true}}
{"time":"2026-11-09T13:00:01Z","type":"applied","actor":"a","recommendationId":"r4","target":"t","details":{"cpuPotentialMonthlyUSD":5}}
{"time":"2026-11-09T14:00:00Z","type":"rollback_failed","actor":"a","recommendationId":"r5","target":"t"}
this is not json
`

func TestSummaryCountsDistinctRecommendationsAndSplitsEstimatedSavings(t *testing.T) {
	s, err := Summarize(strings.NewReader(audit), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if s.Evaluated != 4 || s.Allowed != 3 || s.Denied != 1 || s.Applied != 3 || s.Verified != 1 || s.Degraded != 1 || s.RolledBack != 1 || s.Failed != 1 {
		t.Fatalf("%+v", s)
	}
	if s.AcceptanceRate != 0.75 || s.RollbackRate < 0.333 || s.RollbackRate > 0.334 {
		t.Fatalf("%v %v", s.AcceptanceRate, s.RollbackRate)
	}
	if s.KeptEstimatedMonthlyUSD != 12 || s.RolledBackEstimatedMonthlyUSD != 20 || s.PendingEstimatedMonthlyUSD != 5 {
		t.Fatalf("%+v", s)
	}
	if len(s.TopDenials) != 1 || s.TopDenials[0].Count != 1 {
		t.Fatalf("a denied ID counts once even when evaluated twice: %+v", s.TopDenials)
	}
	if !strings.Contains(strings.Join(s.Notes, " "), "not valid audit events") || !strings.Contains(strings.Join(s.Notes, " "), "ESTIMATES") {
		t.Fatalf("%v", s.Notes)
	}
}

func TestSinceFiltersEvents(t *testing.T) {
	s, _ := Summarize(strings.NewReader(audit), time.Date(2026, 11, 9, 11, 30, 0, 0, time.UTC))
	if s.Evaluated != 2 || s.Applied != 1 || s.RolledBack != 0 {
		t.Fatalf("%+v", s)
	}
}

func TestEmptyAuditHasZeroRatesNotNaN(t *testing.T) {
	s, err := Summarize(strings.NewReader(""), time.Time{})
	if err != nil || s.AcceptanceRate != 0 || s.RollbackRate != 0 {
		t.Fatalf("%v %+v", err, s)
	}
}

func TestAllowedLaterOverridesAnEarlierDenial(t *testing.T) {
	in := `{"time":"2026-11-09T10:00:00Z","type":"decision","recommendationId":"r1","details":{"allowed":false,"reasons":["not approved"]}}
{"time":"2026-11-09T10:01:00Z","type":"decision","recommendationId":"r1","details":{"allowed":true}}
`
	s, _ := Summarize(strings.NewReader(in), time.Time{})
	if s.Allowed != 1 || s.Evaluated != 1 {
		t.Fatalf("%+v", s)
	}
}
