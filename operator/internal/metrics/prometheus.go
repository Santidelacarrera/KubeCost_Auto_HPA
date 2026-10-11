// Package metrics reads live workload signals (RPS, p99 latency, CPU usage)
// from Prometheus and monthly spend from the KubeCost allocation API.
package metrics

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"text/template"
	"time"
)

// ErrNoData is returned when a query yields no usable sample (e.g. no traffic).
var ErrNoData = errors.New("no data")

// Workload identifies the pods a query is scoped to.
type Workload struct {
	Namespace string
	Name      string
	// Overrides replaces the default query for each non-empty field.
	Overrides *Queries
}

// PodRegex matches pods of a Deployment (name-<hash>-<suffix>).
func (w Workload) PodRegex() string { return w.Name + "-.*" }

// Snapshot is one reading of the workload. RPS and CPU usage are totals / per-pod averages
// as documented on each field.
type Snapshot struct {
	// RPS is the total requests/second across all pods of the workload.
	RPS float64 `json:"rps"`
	// P99Ms is the p99 latency in milliseconds.
	P99Ms float64 `json:"p99Ms"`
	// CPUUsageMillicores is the average CPU usage per pod in millicores.
	CPUUsageMillicores float64 `json:"cpuUsageMillicores"`
	// MemUsageMiB is the average working-set memory per pod in MiB (0 = unknown).
	MemUsageMiB float64   `json:"memUsageMiB"`
	At          time.Time `json:"at"`
}

// Source yields snapshots for a workload.
type Source interface {
	Snapshot(ctx context.Context, w Workload) (Snapshot, error)
}

// Queries holds PromQL templates. Available fields: .Namespace, .PodRegex.
type Queries struct {
	RPS      string
	P99Ms    string
	CPUUsage string
	MemUsage string
}

// DefaultQueries assume the conventional http_requests_total /
// http_request_duration_seconds metrics and cAdvisor CPU counters.
func DefaultQueries() Queries {
	return Queries{
		RPS:      `sum(rate(http_requests_total{namespace="{{.Namespace}}",pod=~"{{.PodRegex}}"}[2m]))`,
		P99Ms:    `histogram_quantile(0.99, sum by (le) (rate(http_request_duration_seconds_bucket{namespace="{{.Namespace}}",pod=~"{{.PodRegex}}"}[2m]))) * 1000`,
		MemUsage: `avg(sum by (pod) (container_memory_working_set_bytes{namespace="{{.Namespace}}",pod=~"{{.PodRegex}}",container!="",container!="POD"})) / 1048576`,
		CPUUsage: `avg(sum by (pod) (rate(container_cpu_usage_seconds_total{namespace="{{.Namespace}}",pod=~"{{.PodRegex}}",container!="",container!="POD"}[2m]))) * 1000`,
	}
}

// Prometheus is a Source backed by the Prometheus HTTP API.
type Prometheus struct {
	BaseURL string
	HTTP    *http.Client
	Queries Queries
}

// NewPrometheus builds a client with a bounded timeout.
func NewPrometheus(baseURL string, q Queries) *Prometheus {
	return &Prometheus{BaseURL: baseURL, HTTP: &http.Client{Timeout: 10 * time.Second}, Queries: q}
}

// Snapshot queries all three signals. RPS and p99 are required; CPU usage is
// optional (zero means unknown and disables the usage-based shrink floor).
func (p *Prometheus) Snapshot(ctx context.Context, w Workload) (Snapshot, error) {
	q := p.Queries
	if o := w.Overrides; o != nil {
		for dst, src := range map[*string]string{&q.RPS: o.RPS, &q.P99Ms: o.P99Ms, &q.CPUUsage: o.CPUUsage, &q.MemUsage: o.MemUsage} {
			if src != "" {
				*dst = src
			}
		}
	}
	rps, err := p.query(ctx, q.RPS, w)
	if err != nil {
		return Snapshot{}, fmt.Errorf("rps: %w", err)
	}
	p99, err := p.query(ctx, q.P99Ms, w)
	if err != nil {
		return Snapshot{}, fmt.Errorf("p99: %w", err)
	}
	cpu, err := p.query(ctx, q.CPUUsage, w)
	if err != nil && !errors.Is(err, ErrNoData) {
		return Snapshot{}, fmt.Errorf("cpu usage: %w", err)
	}
	var mem float64
	if q.MemUsage != "" {
		mem, err = p.query(ctx, q.MemUsage, w)
		if err != nil && !errors.Is(err, ErrNoData) {
			return Snapshot{}, fmt.Errorf("memory usage: %w", err)
		}
	}
	return Snapshot{RPS: rps, P99Ms: p99, CPUUsageMillicores: cpu, MemUsageMiB: mem, At: time.Now()}, nil
}

type promResponse struct {
	Status string `json:"status"`
	Error  string `json:"error"`
	Data   struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Value [2]json.RawMessage `json:"value"`
		} `json:"result"`
	} `json:"data"`
}

func (p *Prometheus) query(ctx context.Context, tmpl string, w Workload) (float64, error) {
	t, err := template.New("q").Option("missingkey=error").Parse(tmpl)
	if err != nil {
		return 0, fmt.Errorf("bad query template: %w", err)
	}
	var q bytes.Buffer
	if err := t.Execute(&q, struct{ Namespace, PodRegex string }{w.Namespace, w.PodRegex()}); err != nil {
		return 0, err
	}
	u := p.BaseURL + "/api/v1/query?query=" + url.QueryEscape(q.String())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, err
	}
	resp, err := p.HTTP.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return 0, err
	}
	var pr promResponse
	if err := json.Unmarshal(body, &pr); err != nil {
		return 0, fmt.Errorf("prometheus returned %d: %w", resp.StatusCode, err)
	}
	if resp.StatusCode != http.StatusOK || pr.Status != "success" {
		return 0, fmt.Errorf("prometheus error (%d): %s", resp.StatusCode, pr.Error)
	}
	if len(pr.Data.Result) == 0 {
		return 0, ErrNoData
	}
	var raw string
	if err := json.Unmarshal(pr.Data.Result[0].Value[1], &raw); err != nil {
		return 0, fmt.Errorf("bad sample: %w", err)
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, fmt.Errorf("bad sample %q: %w", raw, err)
	}
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, ErrNoData
	}
	return v, nil
}
