package metrics

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func promServer(t *testing.T, answers map[string]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("query")
		for needle, body := range answers {
			if strings.Contains(q, needle) {
				w.Write([]byte(body))
				return
			}
		}
		w.WriteHeader(500)
		w.Write([]byte(`{"status":"error","error":"unexpected query"}`))
	}))
}

func vec(v string) string {
	return `{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1,"` + v + `"]}]}}`
}

const empty = `{"status":"success","data":{"resultType":"vector","result":[]}}`

func TestSnapshot(t *testing.T) {
	s := promServer(t, map[string]string{"http_requests_total": vec("120.5"), "histogram_quantile": vec("85"), "container_cpu_usage": vec("300")})
	defer s.Close()
	snap, err := NewPrometheus(s.URL, DefaultQueries()).Snapshot(context.Background(), Workload{"shop", "api"})
	if err != nil {
		t.Fatal(err)
	}
	if snap.RPS != 120.5 || snap.P99Ms != 85 || snap.CPUUsageMillicores != 300 {
		t.Fatalf("unexpected snapshot %+v", snap)
	}
}

func TestSnapshotQueryIsScoped(t *testing.T) {
	var got string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Query().Get("query")
		w.Write([]byte(vec("1")))
	}))
	defer s.Close()
	_, _ = NewPrometheus(s.URL, DefaultQueries()).Snapshot(context.Background(), Workload{"shop", "api"})
	if !strings.Contains(got, `namespace="shop"`) || !strings.Contains(got, `pod=~"api-.*"`) {
		t.Fatalf("query not scoped: %s", got)
	}
}

func TestNaNAndEmptyAreNoData(t *testing.T) {
	for _, body := range []string{vec("NaN"), empty} {
		s := promServer(t, map[string]string{"": body})
		_, err := NewPrometheus(s.URL, DefaultQueries()).Snapshot(context.Background(), Workload{"a", "b"})
		s.Close()
		if !errors.Is(err, ErrNoData) {
			t.Fatalf("want ErrNoData, got %v", err)
		}
	}
}

func TestCPUUsageOptional(t *testing.T) {
	s := promServer(t, map[string]string{"http_requests_total": vec("10"), "histogram_quantile": vec("50"), "container_cpu_usage": empty})
	defer s.Close()
	snap, err := NewPrometheus(s.URL, DefaultQueries()).Snapshot(context.Background(), Workload{"a", "b"})
	if err != nil || snap.CPUUsageMillicores != 0 {
		t.Fatalf("snap=%+v err=%v", snap, err)
	}
}

func TestPrometheusError(t *testing.T) {
	s := promServer(t, nil)
	defer s.Close()
	if _, err := NewPrometheus(s.URL, DefaultQueries()).Snapshot(context.Background(), Workload{"a", "b"}); err == nil {
		t.Fatal("expected error")
	}
}

func TestKubeCostMonthlySpend(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/model/allocation" || r.URL.Query().Get("aggregate") != "namespace" {
			w.WriteHeader(404)
			return
		}
		w.Write([]byte(`{"code":200,"data":[{"shop":{"name":"shop","totalCost":42.5},"other":{"totalCost":1}}]}`))
	}))
	defer s.Close()
	k := NewKubeCost(s.URL)
	v, err := k.MonthlySpend(context.Background(), "shop")
	if err != nil || v != 42.5 {
		t.Fatalf("v=%v err=%v", v, err)
	}
	if _, err := k.MonthlySpend(context.Background(), "missing"); !errors.Is(err, ErrNoData) {
		t.Fatalf("want ErrNoData, got %v", err)
	}
}
