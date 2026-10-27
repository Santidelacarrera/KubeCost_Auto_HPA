// Package kubecost is a read-only client for the KubeCost / OpenCost
// allocation API (GET /model/allocation).
//
// Response contract (verified against the OpenCost source, core/pkg/opencost/
// allocation_json.go and allocationprops.go, which KubeCost's allocation API
// extends; it has NOT been validated against a live KubeCost installation):
//
//	{"code":200,"status":"success","data":[{"<key>":{
//	   "name":"...","properties":{"cluster","namespace","controllerKind","controller",...},
//	   "window":{"start":RFC3339,"end":RFC3339},"start":RFC3339,"end":RFC3339,
//	   "minutes":number,"totalCost":number,...}}]}
//
// With accumulate=true the window is summed into one set, so totalCost is the
// cost in USD over `minutes`. Every field this package relies on is a nullable
// pointer upstream, so absence is treated as an invalid sample.
package kubecost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/money"
)

// Class categorises failures so the controller can pick a policy.
type Class string

const (
	// ClassTransient: network error, timeout, HTTP 408/429/5xx. Retried.
	ClassTransient Class = "transient"
	// ClassAuth: HTTP 401/403. Not retried; needs operator attention.
	ClassAuth Class = "auth"
	// ClassClient: other HTTP 4xx or a redirect. Not retried.
	ClassClient Class = "client"
	// ClassMalformed: HTTP 200 with an undecodable or contract-violating body.
	ClassMalformed Class = "malformed"
	// ClassNoData: valid response that does not contain the workload.
	ClassNoData Class = "no_data"
	// ClassAmbiguous: more than one allocation matches the workload.
	ClassAmbiguous Class = "ambiguous"
	// ClassStale: sample older than allowed or window not covered.
	ClassStale Class = "stale"
)

// Error is a classified failure. Err never contains credentials.
type Error struct {
	Class  Class
	Status int
	Err    error
	// RetryAfter is the server's Retry-After hint, zero when absent.
	RetryAfter time.Duration
}

func (e *Error) Error() string {
	if e.Status != 0 {
		return fmt.Sprintf("kubecost %s (HTTP %d): %v", e.Class, e.Status, e.Err)
	}
	return fmt.Sprintf("kubecost %s: %v", e.Class, e.Err)
}
func (e *Error) Unwrap() error { return e.Err }

// ClassOf returns the class of err, or "" when it is not a *Error.
func ClassOf(err error) Class {
	var e *Error
	if errors.As(err, &e) {
		return e.Class
	}
	return ""
}

// maxInFlight bounds concurrent requests from one client across every
// workload, so many KubeCostAutoscaler objects cannot stampede the API.
const maxInFlight = 4

// maxRetryAfter caps how long a server hint can delay a retry.
const maxRetryAfter = 30 * time.Second

// Query identifies one workload.
type Query struct {
	Namespace      string
	Controller     string
	ControllerKind string // e.g. "deployment"; matched case-insensitively
	Cluster        string // optional: disambiguates multi-cluster installs
	Window         time.Duration
}

// Sample is a validated cost observation.
type Sample struct {
	// TotalCost is the allocated cost over Minutes.
	TotalCost money.USD
	// HourlyCost is TotalCost normalised to one hour: allocated (not marginal)
	// run-rate averaged over the window.
	HourlyCost money.USD
	// Minutes the allocation existed inside the window.
	Minutes float64
	// WindowEnd is the end of the allocation data, used as the sample time.
	WindowEnd time.Time
	// FetchedAt is the local time the response was received.
	FetchedAt time.Time
	// Window is the requested window.
	Window time.Duration
}

// Age is how old the data is at now.
func (s Sample) Age(now time.Time) time.Duration { return now.Sub(s.WindowEnd) }

// Validate checks freshness and coverage. The workload must have existed for
// at least minCoverage of the window; otherwise the normalised hourly cost is
// not representative (e.g. it was just created).
func (s Sample) Validate(now time.Time, maxAge time.Duration, minCoverage float64) error {
	if age := s.Age(now); age > maxAge {
		return &Error{Class: ClassStale, Err: fmt.Errorf("sample is %s old (max %s)", age.Round(time.Second), maxAge)}
	}
	if s.WindowEnd.After(now.Add(5 * time.Minute)) {
		return &Error{Class: ClassMalformed, Err: errors.New("sample timestamp is in the future")}
	}
	if s.Window > 0 && s.Minutes < minCoverage*s.Window.Minutes() {
		return &Error{Class: ClassStale, Err: fmt.Errorf("allocation covers %.1f of %.1f window minutes", s.Minutes, s.Window.Minutes())}
	}
	return nil
}

// Client talks to the allocation API. The zero value is not usable; use New.
type Client struct {
	base string
	http *http.Client
	// MaxAttempts bounds total tries per call (>=1).
	MaxAttempts int
	// BaseBackoff/MaxBackoff shape the exponential backoff with full jitter.
	BaseBackoff time.Duration
	MaxBackoff  time.Duration

	now   func() time.Time
	sleep func(context.Context, time.Duration) error
	jit   func() float64
	sem   chan struct{}
}

// ParseBaseURL validates and normalises the configured endpoint: http(s) only,
// host required, no embedded credentials, query or fragment.
func ParseBaseURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", fmt.Errorf("invalid KubeCost URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", errors.New("KubeCost URL scheme must be http or https")
	}
	if u.Hostname() == "" {
		return "", errors.New("KubeCost URL has no host")
	}
	if u.User != nil {
		return "", errors.New("KubeCost URL must not embed credentials; use a bearer token file")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("KubeCost URL must not contain a query or fragment")
	}
	return strings.TrimRight(u.String(), "/"), nil
}

// New builds a client. httpClient carries TLS and authentication (see httpx);
// redirects are never followed so a bearer token cannot leak to another host.
func New(baseURL string, httpClient *http.Client) (*Client, error) {
	base, err := ParseBaseURL(baseURL)
	if err != nil {
		return nil, err
	}
	hc := *httpClient
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if hc.Timeout == 0 {
		hc.Timeout = 15 * time.Second
	}
	return &Client{
		base: base, http: &hc, MaxAttempts: 3, BaseBackoff: 250 * time.Millisecond, MaxBackoff: 4 * time.Second,
		now: time.Now, sleep: sleepCtx, jit: rand.Float64, sem: make(chan struct{}, maxInFlight),
	}, nil
}

// acquire waits for a free in-flight slot or the context to end.
func (c *Client) acquire(ctx context.Context) error {
	if c.sem == nil {
		return nil
	}
	select {
	case c.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *Client) release() {
	if c.sem != nil {
		<-c.sem
	}
}

// parseRetryAfter accepts the delta-seconds and HTTP-date forms of Retry-After.
func parseRetryAfter(v string, now time.Time) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs <= 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil && t.After(now) {
		return t.Sub(now)
	}
	return 0
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Allocation fetches and validates the allocation of one workload.
func (c *Client) Allocation(ctx context.Context, q Query) (Sample, error) {
	if q.Namespace == "" || q.Controller == "" || q.Window <= 0 {
		return Sample{}, &Error{Class: ClassClient, Err: errors.New("namespace, controller and window are required")}
	}
	attempts := c.MaxAttempts
	if attempts < 1 {
		attempts = 1
	}
	var lastErr error
	for i := 0; i < attempts; i++ {
		if i > 0 {
			d := c.backoff(i)
			var ce *Error
			if errors.As(lastErr, &ce) && ce.RetryAfter > d {
				d = min(ce.RetryAfter, maxRetryAfter)
			}
			if err := c.sleep(ctx, d); err != nil {
				return Sample{}, &Error{Class: ClassTransient, Err: err}
			}
		}
		if err := c.acquire(ctx); err != nil {
			return Sample{}, &Error{Class: ClassTransient, Err: err}
		}
		s, err := c.once(ctx, q)
		c.release()
		if err == nil {
			return s, nil
		}
		lastErr = err
		if ClassOf(err) != ClassTransient || ctx.Err() != nil {
			break
		}
	}
	return Sample{}, lastErr
}

func (c *Client) backoff(attempt int) time.Duration {
	d := float64(c.BaseBackoff) * math.Pow(2, float64(attempt-1))
	if max := float64(c.MaxBackoff); max > 0 && d > max {
		d = max
	}
	return time.Duration(d * c.jit()) // full jitter
}

func (c *Client) once(ctx context.Context, q Query) (Sample, error) {
	v := url.Values{}
	v.Set("window", formatWindow(q.Window))
	v.Set("aggregate", "controller")
	v.Set("accumulate", "true")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/model/allocation?"+v.Encode(), nil)
	if err != nil {
		return Sample{}, &Error{Class: ClassClient, Err: err}
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		// url.Error embeds the request URL, never headers: safe to wrap.
		return Sample{}, &Error{Class: ClassTransient, Err: err}
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusOK:
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return Sample{}, &Error{Class: ClassAuth, Status: resp.StatusCode, Err: errors.New("authentication or authorisation failed")}
	case resp.StatusCode == http.StatusRequestTimeout || resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		return Sample{}, &Error{
			Class: ClassTransient, Status: resp.StatusCode, Err: errors.New("server error"),
			RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After"), c.now()),
		}
	default:
		return Sample{}, &Error{Class: ClassClient, Status: resp.StatusCode, Err: errors.New("request rejected")}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return Sample{}, &Error{Class: ClassTransient, Err: fmt.Errorf("reading body: %w", err)}
	}
	s, err := decode(body, q)
	if err != nil {
		return Sample{}, err
	}
	s.FetchedAt = c.now()
	s.Window = q.Window
	return s, nil
}

func formatWindow(d time.Duration) string {
	if d%time.Hour == 0 {
		return strconv.Itoa(int(d/time.Hour)) + "h"
	}
	if d%time.Minute == 0 {
		return strconv.Itoa(int(d/time.Minute)) + "m"
	}
	return strconv.Itoa(int(math.Ceil(d.Seconds()))) + "s"
}

type envelope struct {
	Code *int                         `json:"code"`
	Data []map[string]json.RawMessage `json:"data"`
}

type allocation struct {
	Name       string                     `json:"name"`
	Properties map[string]json.RawMessage `json:"properties"`
	Window     struct {
		End *string `json:"end"`
	} `json:"window"`
	End       *string      `json:"end"`
	Minutes   *json.Number `json:"minutes"`
	TotalCost *json.Number `json:"totalCost"`
}

func malformed(format string, a ...any) error {
	return &Error{Class: ClassMalformed, Err: fmt.Errorf(format, a...)}
}

func prop(a allocation, key string) string {
	raw, ok := a.Properties[key]
	if !ok {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return s
}

func decode(body []byte, q Query) (Sample, error) {
	var env envelope
	if err := json.Unmarshal(body, &env); err != nil {
		return Sample{}, malformed("response is not valid JSON: %v", err)
	}
	if env.Code == nil || *env.Code != http.StatusOK {
		return Sample{}, malformed("missing or non-200 code field")
	}
	if len(env.Data) == 0 {
		return Sample{}, &Error{Class: ClassNoData, Err: errors.New("empty data set")}
	}
	if len(env.Data) > 1 {
		return Sample{}, malformed("expected one accumulated set, got %d", len(env.Data))
	}
	var matches []allocation
	for key, raw := range env.Data[0] {
		var a allocation
		if err := json.Unmarshal(raw, &a); err != nil {
			return Sample{}, malformed("allocation %q is not decodable: %v", key, err)
		}
		if prop(a, "namespace") != q.Namespace || prop(a, "controller") != q.Controller {
			continue
		}
		if k := prop(a, "controllerKind"); k != "" && q.ControllerKind != "" && !strings.EqualFold(k, q.ControllerKind) {
			continue
		}
		if q.Cluster != "" && prop(a, "cluster") != q.Cluster {
			continue
		}
		matches = append(matches, a)
	}
	switch len(matches) {
	case 0:
		return Sample{}, &Error{Class: ClassNoData, Err: errors.New("workload not present in allocation data")}
	case 1:
	default:
		return Sample{}, &Error{Class: ClassAmbiguous, Err: fmt.Errorf("%d allocations match; set the cluster to disambiguate", len(matches))}
	}
	a := matches[0]
	if a.TotalCost == nil || a.Minutes == nil {
		return Sample{}, malformed("totalCost or minutes missing")
	}
	total, err := money.Parse(a.TotalCost.String())
	if err != nil {
		return Sample{}, malformed("totalCost: %v", err)
	}
	mins, err := a.Minutes.Float64()
	if err != nil || math.IsNaN(mins) || math.IsInf(mins, 0) || mins <= 0 {
		return Sample{}, malformed("minutes must be a positive finite number")
	}
	endStr := a.End
	if endStr == nil {
		endStr = a.Window.End
	}
	if endStr == nil {
		return Sample{}, malformed("allocation end time missing")
	}
	end, err := time.Parse(time.RFC3339, *endStr)
	if err != nil {
		return Sample{}, malformed("allocation end time: %v", err)
	}
	// hourly = total * 60 / minutes, exact in rationals, rounded half up.
	hourly := total.MulRat(60_000_000, int64(math.Round(mins*1_000_000)))
	return Sample{TotalCost: total, HourlyCost: hourly, Minutes: mins, WindowEnd: end}, nil
}
