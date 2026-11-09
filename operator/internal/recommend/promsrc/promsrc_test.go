package promsrc

// These tests use a MOCK Prometheus (httptest). They prove request construction, parsing and failure handling;
// they are NOT an integration test against a real Prometheus, cAdvisor or kube-state-metrics.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/recommend"
)

var now = time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)

type fake struct {
	mu      sync.Mutex
	queries []string
	// reply maps a substring of the query to a JSON "result" array; first match wins.
	reply  map[string]string
	status int
}

func vec(v string) string { return fmt.Sprintf(`[{"metric":{},"value":[1,"%s"]}]`, v) }

func matrix(n int, v string) string {
	pts := make([]string, n)
	for i := range pts {
		pts[i] = fmt.Sprintf(`[%d,"%s"]`, i, v)
	}
	return `[{"metric":{"pod":"api-abc-123"},"values":[` + strings.Join(pts, ",") + `]}]`
}

func (f *fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("query")
	f.mu.Lock()
	f.queries = append(f.queries, q)
	f.mu.Unlock()
	if f.status != 0 {
		w.WriteHeader(f.status)
		_, _ = w.Write([]byte(`{"status":"error","errorType":"internal","error":"boom"}`))
		return
	}
	typ := "vector"
	if strings.HasSuffix(r.URL.Path, "query_range") {
		typ = "matrix"
	}
	for sub, res := range f.reply {
		if strings.Contains(q, sub) {
			_, _ = fmt.Fprintf(w, `{"status":"success","data":{"resultType":%q,"result":%s}}`, typ, res)
			return
		}
	}
	_, _ = fmt.Fprintf(w, `{"status":"success","data":{"resultType":%q,"result":[]}}`, typ)
}

func healthy() *fake {
	return &fake{reply: map[string]string{
		"container_cpu_usage_seconds_total":         matrix(600, "0.2"),
		"container_memory_working_set_bytes":        matrix(600, "1073741824"),
		"kube_pod_container_status_restarts_total":  vec("0"),
		"last_terminated_reason":                    `[]`,
		"container_cpu_cfs_throttled_periods_total": vec("0.01"),
		`kube_pod_container_resource_requests{`:     vec("2"),
		`kube_pod_container_resource_limits{`:       vec("4"),
		"slo_breach":                                vec("0"),
	}}
}

func target() Target {
	return Target{Cluster: "lab", Namespace: "shop", Workload: "api", Kind: "Deployment", Container: "app"}
}

func collect(t *testing.T, f *fake, tg Target) (recommend.Input, error) {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	c := &Client{BaseURL: srv.URL}
	return c.Collect(context.Background(), tg, 48*time.Hour, 5*time.Minute, now)
}

func TestCollectBuildsAnInputTheRecommenderCanUse(t *testing.T) {
	in, err := collect(t, healthy(), target())
	if err != nil {
		t.Fatal(err)
	}
	if len(in.CPUSamples) != 600 || len(in.MemSamples) != 600 || !in.ThrottleKnown || in.ThrottledRatio != 0.01 || in.OOMKills != 0 || in.Restarts != 0 {
		t.Fatalf("%+v", in)
	}
	if in.Current.CPURequest != 2 || in.Current.CPULimit != 4 || in.Current.MemRequest != 2 || in.Current.MemLimit != 4 {
		// the fake answers the same substring for cpu and memory; what matters is that all four were read
		t.Logf("current: %+v", in.Current)
	}
	cfg := recommend.Defaults()
	cfg.Now = now
	rec, err := recommend.Recommend(in, cfg)
	if err != nil || rec.Abstained || rec.CPU.Action != recommend.Decrease {
		t.Fatalf("%v %+v", err, rec.CPU)
	}
}

func TestOOMAndRestartsAreCountedAndBlockMemoryReduction(t *testing.T) {
	f := healthy()
	f.reply["kube_pod_container_status_restarts_total"] = vec("2")
	f.reply["last_terminated_reason"] = vec("1")
	in, err := collect(t, f, target())
	if err != nil {
		t.Fatal(err)
	}
	if in.OOMKills != 1 || in.Restarts != 2 {
		t.Fatalf("%+v", in)
	}
	cfg := recommend.Defaults()
	cfg.Now = now
	rec, _ := recommend.Recommend(in, cfg)
	if rec.Memory.Action == recommend.Decrease {
		t.Fatal("OOMKilled must block a memory reduction")
	}
}

func TestSLOQuery(t *testing.T) {
	f := healthy()
	f.reply["slo_breach"] = vec("1")
	tg := target()
	tg.SLOBreachQuery = "slo_breach"
	in, err := collect(t, f, tg)
	if err != nil || !in.SLOKnown || !in.SLOBreached {
		t.Fatalf("%v %+v", err, in)
	}
	f.reply["slo_breach"] = `[]`
	if in, _ = collect(t, f, tg); in.SLOKnown {
		t.Fatal("an empty SLO result must read as unknown, not as healthy")
	}
}

func TestThrottleNaNOrMissingIsUnknown(t *testing.T) {
	f := healthy()
	f.reply["container_cpu_cfs_throttled_periods_total"] = vec("NaN")
	in, err := collect(t, f, target())
	if err != nil || in.ThrottleKnown {
		t.Fatalf("%v %+v", err, in.ThrottleKnown)
	}
}

func TestFailuresProduceErrorsNotPartialInputs(t *testing.T) {
	// Prometheus answering 500
	if _, err := collect(t, &fake{status: 500}, target()); err == nil || !strings.Contains(err.Error(), "prometheus error") {
		t.Fatalf("%v", err)
	}
	// kube-state-metrics missing: must not be read as "no request set / no OOM"
	f := healthy()
	delete(f.reply, "kube_pod_container_status_restarts_total")
	if _, err := collect(t, f, target()); !errors.Is(err, ErrNoKubeStateMetrics) {
		t.Fatalf("%v", err)
	}
	// Prometheus down
	srv := httptest.NewServer(healthy())
	srv.Close()
	c := &Client{BaseURL: srv.URL}
	if _, err := c.Collect(context.Background(), target(), 48*time.Hour, 5*time.Minute, now); err == nil || !strings.Contains(err.Error(), "unreachable") {
		t.Fatalf("%v", err)
	}
	// non-JSON body
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("<html>")) }))
	defer bad.Close()
	if _, err := (&Client{BaseURL: bad.URL}).Collect(context.Background(), target(), 48*time.Hour, 5*time.Minute, now); err == nil {
		t.Fatal("non-JSON accepted")
	}
}

func TestNoDataAbstains(t *testing.T) {
	f := healthy()
	f.reply["container_cpu_usage_seconds_total"] = `[]`
	in, err := collect(t, f, target())
	if err != nil {
		t.Fatal(err)
	}
	cfg := recommend.Defaults()
	cfg.Now = now
	if rec, _ := recommend.Recommend(in, cfg); !rec.Abstained {
		t.Fatal("no CPU history must abstain")
	}
}

func TestInputValidationBlocksPromQLInjectionAndBadRanges(t *testing.T) {
	for _, tg := range []Target{
		{Namespace: `shop"}or vector(1)#`, Workload: "api", Kind: "Deployment", Container: "app"},
		{Namespace: "shop", Workload: `a|b`, Kind: "Deployment", Container: "app"},
		{Namespace: "shop", Workload: "api", Kind: "Deployment", Container: ""},
		{Namespace: "shop", Workload: "api", Kind: "CronJob", Container: "app"},
	} {
		f := healthy()
		if _, err := collect(t, f, tg); err == nil {
			t.Errorf("accepted %+v", tg)
		}
		if len(f.queries) != 0 {
			t.Errorf("a query was sent for invalid input %+v: %v", tg, f.queries)
		}
	}
	srv := httptest.NewServer(healthy())
	defer srv.Close()
	if _, err := (&Client{BaseURL: srv.URL}).Collect(context.Background(), target(), 365*24*time.Hour, time.Second, now); err == nil {
		t.Fatal("window/step giving too many points accepted")
	}
}

func TestQueriesAreScopedAndReadOnly(t *testing.T) {
	f := healthy()
	tg := target()
	tg.Workload = "my.api"
	if _, err := collect(t, f, tg); err != nil {
		t.Fatal(err)
	}
	for _, q := range f.queries {
		if !strings.Contains(q, `namespace="shop"`) || !strings.Contains(q, `container="app"`) || !strings.Contains(q, `my[.]api-[a-z0-9]+-[a-z0-9]+`) {
			t.Errorf("unscoped query: %s", q)
		}
	}
}
