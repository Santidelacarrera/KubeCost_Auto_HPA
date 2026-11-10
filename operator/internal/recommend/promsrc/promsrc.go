// Package promsrc reads the history the recommender needs from Prometheus (cAdvisor + kube-state-metrics).
//
// It only issues read-only HTTP GETs against /api/v1/query and /api/v1/query_range. Any failure (Prometheus down, bad
// response, missing kube-state-metrics) is returned as an error so the caller produces NO recommendation rather than a
// recommendation built from partial data.
package promsrc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/recommend"
)

// ErrNoKubeStateMetrics: requests/limits/restarts come from kube-state-metrics; without it "no request set" and "no OOM" would be guesses.
var ErrNoKubeStateMetrics = errors.New("kube-state-metrics series not found for this container (kube_pod_container_status_restarts_total is empty): refusing to guess requests, restarts or OOMKilled")

const (
	maxPoints   = 11000 // Prometheus' own limit per series for query_range
	maxBodySize = 32 << 20
)

var ident = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9._-]{0,251}[a-zA-Z0-9])?$`)

// Client queries one Prometheus.
type Client struct {
	BaseURL string
	HTTP    *http.Client
}

// Target identifies one container of one workload.
type Target struct {
	Cluster, Namespace, Workload, Kind, Container string
	// SLOBreachQuery is an optional instant PromQL expression whose result is > 0 while the SLO is breached.
	SLOBreachQuery string
}

// podRegex matches the pods of a workload by naming convention. Characters are validated first, so no PromQL can be injected.
func (t Target) podRegex() (string, error) {
	n := strings.ReplaceAll(t.Workload, ".", "[.]")
	switch t.Kind {
	case "Deployment":
		return n + "-[a-z0-9]+-[a-z0-9]+", nil
	case "StatefulSet":
		return n + "-[0-9]+", nil
	case "DaemonSet":
		return n + "-[a-z0-9]+", nil
	}
	return "", fmt.Errorf("kind %q is not supported (Deployment, StatefulSet, DaemonSet)", t.Kind)
}

func (t Target) validate() error {
	for name, v := range map[string]string{"namespace": t.Namespace, "workload": t.Workload, "container": t.Container} {
		if !ident.MatchString(v) {
			return fmt.Errorf("%s %q is not a valid Kubernetes name", name, v)
		}
	}
	return nil
}

// Collect gathers `window` of history ending at `now`, sampled every `step`.
func (c *Client) Collect(ctx context.Context, t Target, window, step time.Duration, now time.Time) (recommend.Input, error) {
	in := recommend.Input{Cluster: t.Cluster, Namespace: t.Namespace, Workload: t.Workload, Kind: t.Kind, Container: t.Container,
		WindowStart: now.Add(-window), WindowEnd: now}
	if err := t.validate(); err != nil {
		return in, err
	}
	pods, err := t.podRegex()
	if err != nil {
		return in, err
	}
	if step <= 0 || window <= 0 || float64(window)/float64(step) > maxPoints {
		return in, fmt.Errorf("window/step must be positive and give at most %d points", maxPoints)
	}
	sel := fmt.Sprintf(`namespace="%s",pod=~"%s",container="%s"`, t.Namespace, pods, t.Container)
	w := promDuration(window)

	if in.CPUSeries, err = c.rangeSeries(ctx, fmt.Sprintf(`rate(container_cpu_usage_seconds_total{%s}[5m])`, sel), in.WindowStart, now, step); err != nil {
		return in, fmt.Errorf("cpu usage: %w", err)
	}
	for _, s := range in.CPUSeries {
		in.CPUSamples = append(in.CPUSamples, s...)
	}
	if in.MemSamples, err = c.rangeValues(ctx, fmt.Sprintf(`container_memory_working_set_bytes{%s}`, sel), in.WindowStart, now, step); err != nil {
		return in, fmt.Errorf("memory working set: %w", err)
	}

	restarts, err := c.instant(ctx, fmt.Sprintf(`sum(increase(kube_pod_container_status_restarts_total{%s}[%s]))`, sel, w), now)
	if err != nil {
		return in, fmt.Errorf("restarts: %w", err)
	}
	if len(restarts) == 0 {
		return in, ErrNoKubeStateMetrics
	}
	in.Restarts = int(math.Round(restarts[0]))

	oom, err := c.instant(ctx, fmt.Sprintf(`sum(max_over_time(kube_pod_container_status_last_terminated_reason{%s,reason="OOMKilled"}[%s]))`, sel, w), now)
	if err != nil {
		return in, fmt.Errorf("oomkilled: %w", err)
	}
	if len(oom) > 0 {
		in.OOMKills = int(math.Round(oom[0]))
	}

	// throttling: 0/0 gives NaN and a missing series gives nothing; both mean "not measured".
	th, err := c.instant(ctx, fmt.Sprintf(`sum(increase(container_cpu_cfs_throttled_periods_total{%s}[%s])) / sum(increase(container_cpu_cfs_periods_total{%s}[%s]))`, sel, w, sel, w), now)
	if err != nil {
		return in, fmt.Errorf("cpu throttling: %w", err)
	}
	if len(th) > 0 {
		in.ThrottledRatio, in.ThrottleKnown = th[0], true
	}

	get := func(metric, res string) (float64, error) {
		v, err := c.instant(ctx, fmt.Sprintf(`max(%s{%s,resource="%s"})`, metric, sel, res), now)
		if err != nil || len(v) == 0 {
			return 0, err
		}
		return v[0], nil
	}
	if in.Current.CPURequest, err = get("kube_pod_container_resource_requests", "cpu"); err != nil {
		return in, fmt.Errorf("cpu request: %w", err)
	}
	if in.Current.CPULimit, err = get("kube_pod_container_resource_limits", "cpu"); err != nil {
		return in, fmt.Errorf("cpu limit: %w", err)
	}
	if in.Current.MemRequest, err = get("kube_pod_container_resource_requests", "memory"); err != nil {
		return in, fmt.Errorf("memory request: %w", err)
	}
	if in.Current.MemLimit, err = get("kube_pod_container_resource_limits", "memory"); err != nil {
		return in, fmt.Errorf("memory limit: %w", err)
	}

	if t.SLOBreachQuery != "" {
		v, err := c.instant(ctx, t.SLOBreachQuery, now)
		if err != nil {
			return in, fmt.Errorf("slo query: %w", err)
		}
		if len(v) > 0 {
			in.SLOKnown, in.SLOBreached = true, v[0] > 0
		}
	}
	return in, nil
}

// Breached evaluates an SLO-breach expression at `at`: breached is true when the first value is > 0; known is false when it returned no data.
func (c *Client) Breached(ctx context.Context, query string, at time.Time) (breached, known bool, err error) {
	v, err := c.instant(ctx, query, at)
	if err != nil || len(v) == 0 {
		return false, false, err
	}
	return v[0] > 0, true, nil
}

func promDuration(d time.Duration) string { return strconv.FormatInt(int64(d.Seconds()), 10) + "s" }

type response struct {
	Status    string `json:"status"`
	ErrorType string `json:"errorType"`
	Error     string `json:"error"`
	Data      struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Value  [2]json.RawMessage   `json:"value"`
			Values [][2]json.RawMessage `json:"values"`
		} `json:"result"`
	} `json:"data"`
}

func (c *Client) get(ctx context.Context, path string, q url.Values) (*response, error) {
	u := strings.TrimRight(c.BaseURL, "/") + path + "?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	res, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("prometheus unreachable: %w", err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, maxBodySize+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxBodySize {
		return nil, errors.New("prometheus response too large")
	}
	var r response
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("prometheus returned HTTP %d with a non-JSON body", res.StatusCode)
	}
	if res.StatusCode != http.StatusOK || r.Status != "success" {
		return nil, fmt.Errorf("prometheus error (HTTP %d): %s %s", res.StatusCode, r.ErrorType, truncate(r.Error, 200))
	}
	return &r, nil
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func parse(raw json.RawMessage) (float64, bool) {
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return 0, false
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	return f, true
}

// instant returns the finite values of an instant query (NaN/Inf are dropped, so "0/0" reads as "no data").
func (c *Client) instant(ctx context.Context, query string, at time.Time) ([]float64, error) {
	r, err := c.get(ctx, "/api/v1/query", url.Values{"query": {query}, "time": {strconv.FormatInt(at.Unix(), 10)}})
	if err != nil {
		return nil, err
	}
	var out []float64
	for _, s := range r.Data.Result {
		if v, ok := parse(s.Value[1]); ok {
			out = append(out, v)
		}
	}
	return out, nil
}

// rangeValues flattens every series (one per pod) into one slice of finite samples.
func (c *Client) rangeValues(ctx context.Context, query string, start, end time.Time, step time.Duration) ([]float64, error) {
	ss, err := c.rangeSeries(ctx, query, start, end, step)
	var out []float64
	for _, s := range ss {
		out = append(out, s...)
	}
	return out, err
}

// rangeSeries returns one time-ordered slice of finite samples per series (per pod).
func (c *Client) rangeSeries(ctx context.Context, query string, start, end time.Time, step time.Duration) ([][]float64, error) {
	r, err := c.get(ctx, "/api/v1/query_range", url.Values{"query": {query}, "start": {strconv.FormatInt(start.Unix(), 10)},
		"end": {strconv.FormatInt(end.Unix(), 10)}, "step": {strconv.FormatInt(int64(step.Seconds()), 10)}})
	if err != nil {
		return nil, err
	}
	var out [][]float64
	for _, s := range r.Data.Result {
		var one []float64
		for _, p := range s.Values {
			if v, ok := parse(p[1]); ok {
				one = append(one, v)
			}
		}
		out = append(out, one)
	}
	return out, nil
}

// Series returns the raw per-pod CPU and memory working-set series for a window (no kube-state-metrics needed). It is what the backtest uses.
func (c *Client) Series(ctx context.Context, t Target, window, step time.Duration, now time.Time) (cpu, mem [][]float64, err error) {
	if err := t.validate(); err != nil {
		return nil, nil, err
	}
	pods, err := t.podRegex()
	if err != nil {
		return nil, nil, err
	}
	if step <= 0 || window <= 0 || float64(window)/float64(step) > maxPoints {
		return nil, nil, fmt.Errorf("window/step must be positive and give at most %d points", maxPoints)
	}
	sel := fmt.Sprintf(`namespace="%s",pod=~"%s",container="%s"`, t.Namespace, pods, t.Container)
	start := now.Add(-window)
	if cpu, err = c.rangeSeries(ctx, fmt.Sprintf(`rate(container_cpu_usage_seconds_total{%s}[5m])`, sel), start, now, step); err != nil {
		return nil, nil, fmt.Errorf("cpu usage: %w", err)
	}
	if mem, err = c.rangeSeries(ctx, fmt.Sprintf(`container_memory_working_set_bytes{%s}`, sel), start, now, step); err != nil {
		return nil, nil, fmt.Errorf("memory working set: %w", err)
	}
	return cpu, mem, nil
}
