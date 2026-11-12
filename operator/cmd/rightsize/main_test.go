package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These test only the argument guards that must hold before any cluster is contacted.

func TestExecuteWithoutExplicitContextIsRefusedBeforeAnythingElse(t *testing.T) {
	dir := t.TempDir()
	rec, pol := filepath.Join(dir, "rec.json"), filepath.Join(dir, "pol.yaml")
	_ = os.WriteFile(rec, []byte("{}"), 0o600)
	_ = os.WriteFile(pol, []byte("{}"), 0o600)
	err := apply([]string{"--recommendation", rec, "--policy", pol, "--execute"}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "explicit --context") {
		t.Fatalf("%v", err)
	}
}

func TestApplyRequiresRecommendationAndPolicy(t *testing.T) {
	if err := apply(nil, io.Discard); err == nil {
		t.Fatal("accepted no arguments")
	}
}

func TestPolicyWithUnknownKeysIsRejected(t *testing.T) {
	dir := t.TempDir()
	rec, pol := filepath.Join(dir, "rec.json"), filepath.Join(dir, "pol.yaml")
	_ = os.WriteFile(rec, []byte("{}"), 0o600)
	_ = os.WriteFile(pol, []byte("allowNamespace: [prod]\n"), 0o600) // typo: would silently authorise nothing or the wrong thing
	err := apply([]string{"--recommendation", rec, "--policy", pol, "--context", "x"}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "policy") {
		t.Fatalf("%v", err)
	}
}

func TestRollbackAndReadOnlyCommandsNeedAContext(t *testing.T) {
	if err := rollback([]string{"--namespace", "a", "--workload", "b"}, io.Discard); err == nil {
		t.Error("rollback without --context accepted")
	}
	if err := nodesCmd(nil, io.Discard); err == nil {
		t.Error("nodes without --context accepted")
	}
	if err := costCmd([]string{"--node-hourly-usd", "1"}, io.Discard); err == nil {
		t.Error("cost without --context accepted")
	}
}

func TestCostNeedsExactlyOneBasis(t *testing.T) {
	if err := costCmd([]string{"--context", "x"}, io.Discard); err == nil || !strings.Contains(err.Error(), "exactly one") {
		t.Fatalf("%v", err)
	}
	if err := costCmd([]string{"--context", "x", "--billing-file", "b.csv", "--node-hourly-usd", "1"}, io.Discard); err == nil || !strings.Contains(err.Error(), "exactly one") {
		t.Fatalf("%v", err)
	}
}

func TestParsePrices(t *testing.T) {
	m, err := parsePrices("a=0.2,b=1")
	if err != nil || m["a"] != 0.2 || m["b"] != 1 {
		t.Fatalf("%v %v", m, err)
	}
	for _, bad := range []string{"a", "a=x", "a=-1"} {
		if _, err := parsePrices(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestNewCommandsGuardTheirArguments(t *testing.T) {
	for name, f := range map[string]func([]string, io.Writer) error{
		"approve": approveCmd, "plan": planCmd, "report": reportCmd, "propose": proposeCmd, "publish": publishCmd,
	} {
		if err := f(nil, io.Discard); err == nil {
			t.Errorf("%s accepted no arguments", name)
		}
	}
	if err := approveCmd([]string{"--recommendation", "x.json"}, io.Discard); err == nil || !strings.Contains(err.Error(), "--context") {
		t.Fatalf("approve without --context: %v", err)
	}
	if err := planCmd([]string{"--prometheus-url", "http://x", "--namespace", "a", "--workload", "b", "--container", "c"}, io.Discard); err == nil {
		t.Fatal("plan without --context accepted (it reads the cluster)")
	}
}

func TestReportSummarisesAnAuditFile(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "audit.jsonl")
	_ = os.WriteFile(f, []byte(`{"time":"2026-11-09T10:00:00Z","type":"decision","recommendationId":"r1","details":{"allowed":true}}
{"time":"2026-11-09T10:00:01Z","type":"applied","recommendationId":"r1","details":{"cpuPotentialMonthlyUSD":3}}
`), 0o600)
	var b strings.Builder
	if err := reportCmd([]string{"--audit-file", f}, &b); err != nil || !strings.Contains(b.String(), `"Applied": 1`) {
		t.Fatalf("%v %s", err, b.String())
	}
	if err := reportCmd([]string{"--audit-file", f, "--since", "yesterday"}, io.Discard); err == nil {
		t.Fatal("bad --since accepted")
	}
}

func TestBudgetsAndBaselineParsing(t *testing.T) {
	b, err := parseBudgets("shop=10.5,dev=2")
	if err != nil || len(b) != 2 {
		t.Fatalf("%v %v", b, err)
	}
	for _, bad := range []string{"shop", "=3", "shop=-1", "shop=x"} {
		if _, err := parseBudgets(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "r.json")
	_ = os.WriteFile(p, []byte(`{"basis":"billed","start":"2026-11-01T00:00:00Z","end":"2026-11-02T00:00:00Z","byNamespace":{"shop":"24.000000"}}`), 0o600)
	r, err := loadReport(p)
	if err != nil || string(r.Basis) != "billed" || r.ByNamespace["shop"].String() != "24.000000" {
		t.Fatalf("%v %+v", err, r)
	}
	_ = os.WriteFile(p, []byte(`{"byNamespace":{"shop":"abc"}}`), 0o600)
	if _, err := loadReport(p); err == nil {
		t.Fatal("garbage money accepted")
	}
}

// MOCK Prometheus: argument handling and output shape of the backtest, not a measurement.
func TestBacktestAgainstAMockPrometheus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pts := make([]string, 1500)
		for i := range pts {
			v := "0.2"
			if strings.Contains(r.URL.Query().Get("query"), "working_set") {
				v = "104857600"
			}
			pts[i] = fmt.Sprintf(`[%d,"%s"]`, i, v)
		}
		fmt.Fprintf(w, `{"status":"success","data":{"resultType":"matrix","result":[{"metric":{},"values":[%s]}]}}`, strings.Join(pts, ","))
	}))
	defer srv.Close()
	var b strings.Builder
	err := backtestCmd([]string{"--prometheus-url", srv.URL, "--namespace", "a", "--workload", "b", "--container", "c", "--window", "125h", "--holdout", "25h", "--step", "5m"}, &b)
	if err != nil || !strings.Contains(b.String(), `"Best"`) || !strings.Contains(b.String(), `"cpu"`) || !strings.Contains(b.String(), `"memory"`) {
		t.Fatalf("%v %s", err, b.String())
	}
	if err := backtestCmd([]string{"--prometheus-url", srv.URL, "--namespace", "a", "--workload", "b", "--container", "c", "--window", "1h", "--holdout", "2h"}, io.Discard); err == nil {
		t.Fatal("holdout longer than the window accepted")
	}
	if err := backtestCmd(nil, io.Discard); err == nil {
		t.Fatal("no arguments accepted")
	}
}
