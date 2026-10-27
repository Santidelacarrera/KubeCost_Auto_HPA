//go:build kubecost_live

package kubecost

// Contract check against a REAL KubeCost / OpenCost installation. It is excluded from every normal
// build and from CI (build tag kubecost_live) because it needs a reachable installation. Until it
// has been run against one, the documented response contract is verified only against the OpenCost
// source and hand-written fixtures; do NOT describe the integration as validated.
//
//	KUBECOST_LIVE_URL=http://kubecost-cost-analyzer.kubecost:9090 \
//	KUBECOST_LIVE_NAMESPACE=production KUBECOST_LIVE_CONTROLLER=payments-api \
//	[KUBECOST_LIVE_KIND=deployment] [KUBECOST_LIVE_WINDOW=1h] [KUBECOST_LIVE_MAX_AGE=5m] [KUBECOST_LIVE_TOKEN=...] \
//	  go test -tags kubecost_live -count=1 -v -run Live ./internal/kubecost
//
// (kubectl port-forward -n kubecost svc/kubecost-cost-analyzer 9090 gives a local URL.)

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/httpx"
)

func liveEnv(t *testing.T, key, def string) string {
	t.Helper()
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func TestLiveAllocationContract(t *testing.T) {
	url := os.Getenv("KUBECOST_LIVE_URL")
	if url == "" {
		t.Skip("KUBECOST_LIVE_URL not set: no live KubeCost to test against")
	}
	ns, ctl := os.Getenv("KUBECOST_LIVE_NAMESPACE"), os.Getenv("KUBECOST_LIVE_CONTROLLER")
	if ns == "" || ctl == "" {
		t.Fatal("KUBECOST_LIVE_NAMESPACE and KUBECOST_LIVE_CONTROLLER are required")
	}
	window, err := time.ParseDuration(liveEnv(t, "KUBECOST_LIVE_WINDOW", "1h"))
	if err != nil {
		t.Fatal(err)
	}
	hc, err := httpx.NewClient(httpx.Options{BearerToken: os.Getenv("KUBECOST_LIVE_TOKEN")}, 20*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	c, err := New(url, hc)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	s, err := c.Allocation(ctx, Query{Namespace: ns, Controller: ctl, ControllerKind: liveEnv(t, "KUBECOST_LIVE_KIND", "deployment"), Window: window})
	if err != nil {
		t.Fatalf("the documented contract did not hold against this installation: %v", err)
	}
	t.Logf("sample: total=%s hourly=%s minutes=%.1f windowEnd=%s age=%s", s.TotalCost, s.HourlyCost, s.Minutes, s.WindowEnd, s.Age(time.Now()).Round(time.Second))
	if s.HourlyCost <= 0 {
		t.Errorf("a running workload must have a positive hourly cost, got %s", s.HourlyCost)
	}
	// Freshness uses the CRD default for failurePolicy.maxMetricAge (5m). A looser bound here would
	// accept samples that the operator itself would freeze on.
	maxAge, err := time.ParseDuration(liveEnv(t, "KUBECOST_LIVE_MAX_AGE", "5m"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Validate(time.Now(), maxAge, 0.5); err != nil {
		t.Errorf("sample failed freshness/coverage validation (max age %s): %v", maxAge, err)
	}
}
