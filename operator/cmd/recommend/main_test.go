package main

// MOCK Prometheus: this exercises flag handling, the exit contract and the JSON shape, not a real Prometheus.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func pts(n int, v string) string {
	p := make([]string, n)
	for i := range p {
		p[i] = fmt.Sprintf(`[%d,"%s"]`, i, v)
	}
	return strings.Join(p, ",")
}

func prom() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("query")
		rng := strings.HasSuffix(r.URL.Path, "query_range")
		res := `[]`
		switch {
		case strings.Contains(q, "container_cpu_usage"):
			res = `[{"metric":{},"values":[` + pts(600, "0.2") + `]}]`
		case strings.Contains(q, "working_set"):
			res = `[{"metric":{},"values":[` + pts(600, "1073741824") + `]}]`
		case strings.Contains(q, "restarts_total"):
			res = `[{"metric":{},"value":[1,"0"]}]`
		case strings.Contains(q, "cfs_throttled"):
			res = `[{"metric":{},"value":[1,"0.0"]}]`
		case strings.Contains(q, "resource_requests"):
			res = `[{"metric":{},"value":[1,"4"]}]`
		}
		typ := "vector"
		if rng {
			typ = "matrix"
		}
		fmt.Fprintf(w, `{"status":"success","data":{"resultType":%q,"result":%s}}`, typ, res)
	}))
}

func runCLI(t *testing.T, args ...string) (map[string]any, error) {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "out")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	err = run(args, f, func() time.Time { return time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC) })
	if err != nil {
		return nil, err
	}
	b, _ := os.ReadFile(f.Name())
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, b)
	}
	return m, nil
}

func TestCLIPrintsARecommendation(t *testing.T) {
	s := prom()
	defer s.Close()
	m, err := runCLI(t, "--prometheus-url", s.URL, "--namespace", "shop", "--workload", "api", "--container", "app", "--window", "48h")
	if err != nil {
		t.Fatal(err)
	}
	if m["Abstained"] != false || m["ID"] == "" {
		t.Fatalf("%v", m)
	}
	cpu := m["CPU"].(map[string]any)
	if cpu["Action"] != "Decrease" {
		t.Fatalf("%v", cpu)
	}
}

func TestCLIAbstainsOnShortWindowButStillSucceeds(t *testing.T) {
	s := prom()
	defer s.Close()
	m, err := runCLI(t, "--prometheus-url", s.URL, "--namespace", "shop", "--workload", "api", "--container", "app", "--window", "2h")
	if err != nil || m["Abstained"] != true {
		t.Fatalf("%v %v", err, m)
	}
}

func TestCLIErrorsOnMissingFlagsAndUnreachablePrometheus(t *testing.T) {
	if _, err := runCLI(t); err == nil {
		t.Fatal("missing flags accepted")
	}
	s := prom()
	s.Close()
	if _, err := runCLI(t, "--prometheus-url", s.URL, "--namespace", "shop", "--workload", "api", "--container", "app"); err == nil {
		t.Fatal("a dead Prometheus must be an error, not an empty recommendation")
	}
	if _, err := runCLI(t, "--prometheus-url", "http://x", "--namespace", "shop", "--workload", "api", "--container", "app", "--cpu-percentile", "2"); err == nil {
		t.Fatal("invalid config accepted")
	}
}
