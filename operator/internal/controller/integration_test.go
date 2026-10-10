//go:build integration

package controller

// End-to-end: reconciler -> Prometheus (httptest) -> Redis (miniredis) ->
// KubeCost (httptest) -> the real Python engine process -> in-place pod resize.

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/santidelacarrera/kubecost_auto_hpa/operator/api/v1alpha1"
	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/cache"
	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/engine"
	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/metrics"
)

func startEngine(t *testing.T) string {
	t.Helper()
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not available")
	}
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	abs, _ := filepath.Abs("../../../engine")
	cmd := exec.Command(py, "-m", "kubecost_engine.server")
	cmd.Env = append(os.Environ(), fmt.Sprintf("PORT=%d", port), "PYTHONPATH="+abs)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	url := fmt.Sprintf("http://127.0.0.1:%d", port)
	for i := 0; i < 100; i++ {
		if r, err := http.Get(url + "/healthz"); err == nil {
			r.Body.Close()
			return url
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("engine did not start")
	return ""
}

type promState struct {
	mu            sync.Mutex
	rps, p99, cpu string
}

func (p *promState) set(rps, p99, cpu string) {
	p.mu.Lock()
	p.rps, p.p99, p.cpu = rps, p99, cpu
	p.mu.Unlock()
}

func (p *promState) handler(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	q := r.URL.Query().Get("query")
	v := p.cpu
	switch {
	case strings.Contains(q, "http_requests_total"):
		v = p.rps
	case strings.Contains(q, "histogram_quantile"):
		v = p.p99
	}
	fmt.Fprintf(w, `{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1,"%s"]}]}}`, v)
}

func TestEndToEnd(t *testing.T) {
	engURL := startEngine(t)
	prom := &promState{}
	prom.set("200", "50", "300") // 2 pods, 100 rps each, p99 50ms vs SLO 200ms, 300m used
	ps := httptest.NewServer(http.HandlerFunc(prom.handler))
	defer ps.Close()
	spent := 10.0
	var spentMu sync.Mutex
	ks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		spentMu.Lock()
		defer spentMu.Unlock()
		fmt.Fprintf(w, `{"code":200,"data":[{"shop":{"totalCost":%f}}]}`, spent)
	}))
	defer ks.Close()
	mr := miniredis.RunT(t)

	h := newHPA()
	h.Spec.Budget = &v1alpha1.Budget{MonthlyLimitUSD: "100"}
	h.Spec.Cooldown.Duration = time.Minute
	e := newEnv(h, newDeployment(), newPod("api-1"), newPod("api-2"))
	e.r.Metrics = metrics.NewPrometheus(ps.URL, metrics.DefaultQueries())
	e.r.Spend = metrics.NewKubeCost(ks.URL)
	e.r.Engine = engine.NewHTTP(engURL)
	e.r.Store = cache.NewRedis(mr.Addr(), "", 0)

	// 1. SLO has slack -> shrink 1000m -> 800m on every pod, no restart.
	e.syncPods()
	if _, err := e.r.Reconcile(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"api-1", "api-2"} {
		if got := e.podCPU(t, p); got != 800 {
			t.Fatalf("%s cpu=%d want 800", p, got)
		}
	}
	st := e.hpa(t).Status
	if st.LastAction != "shrink" || st.CostPerRequest == "" {
		t.Fatalf("status %+v", st)
	}
	if !mr.Exists("kchpa:shop/api:decision") || !mr.Exists("kchpa:shop/api:metrics") {
		t.Fatal("expected scaling state cached in redis")
	}

	// 2. Latency spikes while the budget is exhausted -> growth is blocked.
	mr.FastForward(2 * time.Minute)
	e.t = e.t.Add(2 * time.Minute)
	prom.set("200", "195", "300")
	spentMu.Lock()
	spent = 100
	spentMu.Unlock()
	e.syncPods()
	if _, err := e.r.Reconcile(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	st = e.hpa(t).Status
	if !st.BudgetFrozen || st.LastAction != "hold" || e.podCPU(t, "api-1") != 800 {
		t.Fatalf("expected frozen hold, status %+v cpu=%d", st, e.podCPU(t, "api-1"))
	}

	// 3. Budget back under the limit -> growth allowed.
	mr.FastForward(2 * time.Minute)
	e.t = e.t.Add(2 * time.Minute)
	spentMu.Lock()
	spent = 20
	spentMu.Unlock()
	e.syncPods()
	if _, err := e.r.Reconcile(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if got := e.podCPU(t, "api-1"); got != 1000 {
		t.Fatalf("want grow to 1000m, got %d; status %+v", got, e.hpa(t).Status)
	}

	// 4. Usage floor: heavy actual CPU use prevents a shrink even with latency slack.
	mr.FastForward(2 * time.Minute)
	e.t = e.t.Add(2 * time.Minute)
	prom.set("200", "50", "900")
	e.syncPods()
	if _, err := e.r.Reconcile(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if got := e.podCPU(t, "api-1"); got != 1000 {
		t.Fatalf("usage floor violated: %d", got)
	}
}

// With the CPU already at its maximum and p99 near the SLO, the real engine
// asks for a scale-out; a frozen budget blocks it.
func TestEndToEndScaleOutAndFrozenBudget(t *testing.T) {
	engURL := startEngine(t)
	prom := &promState{}
	prom.set("400", "195", "700")
	ps := httptest.NewServer(http.HandlerFunc(prom.handler))
	defer ps.Close()
	var spent float64 = 10
	var mu sync.Mutex
	ks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		fmt.Fprintf(w, `{"code":200,"data":[{"shop":{"totalCost":%f}}]}`, spent)
	}))
	defer ks.Close()

	h := newHPA()
	h.Spec.Budget = &v1alpha1.Budget{MonthlyLimitUSD: "100"}
	h.Spec.Replicas = &v1alpha1.ReplicaSpec{Min: 1, Max: 6}
	h.Spec.CPU.Max = *resource.NewMilliQuantity(1000, resource.DecimalSI) // already at max
	dep := newDeployment()
	three := int32(3)
	dep.Spec.Replicas = &three
	e := newEnv(h, dep, newPod("api-1"), newPod("api-2"), newPod("api-3"))
	e.r.Metrics = metrics.NewPrometheus(ps.URL, metrics.DefaultQueries())
	e.r.Spend = metrics.NewKubeCost(ks.URL)
	e.r.Engine = engine.NewHTTP(engURL)

	replicas := func() int32 {
		sc, _ := e.cs.AppsV1().Deployments("shop").GetScale(context.Background(), "api", metav1.GetOptions{})
		return sc.Spec.Replicas
	}
	mu.Lock()
	spent = 100 // budget exhausted
	mu.Unlock()
	e.r.Reconcile(context.Background(), req)
	if replicas() != 3 {
		t.Fatalf("frozen budget must block scale-out, replicas=%d", replicas())
	}
	mu.Lock()
	spent = 10
	mu.Unlock()
	e.t = e.t.Add(2 * time.Minute) // metrics cache TTL
	e.r.Reconcile(context.Background(), req)
	if replicas() != 4 {
		t.Fatalf("expected scale-out to 4, got %d; status %+v", replicas(), e.hpa(t).Status)
	}
}
