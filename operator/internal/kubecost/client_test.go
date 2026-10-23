package kubecost

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/httpx"
)

var t0 = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

func alloc(ns, ctl, kind, cluster string, total, minutes string, end time.Time) string {
	return fmt.Sprintf(`"%[1]s/%[2]s/%[3]s":{"name":"%[1]s/%[2]s","properties":{"cluster":"%[3]s","namespace":"%[1]s","controller":"%[2]s","controllerKind":"%[4]s","services":["svc"],"labels":{"a":"b"}},
		"window":{"start":"%[5]s","end":"%[6]s"},"start":"%[7]s","end":"%[8]s","minutes":%[9]s,"totalCost":%[10]s,"cpuCost":1}`,
		ns, ctl, cluster, kind, end.Add(-time.Hour).Format(time.RFC3339), end.Format(time.RFC3339),
		end.Add(-time.Hour).Format(time.RFC3339), end.Format(time.RFC3339), minutes, total)
}

func body(allocs ...string) string {
	return `{"code":200,"status":"success","data":[{` + strings.Join(allocs, ",") + `}]}`
}

func newTest(t *testing.T, h http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	c, err := New(s.URL, s.Client())
	if err != nil {
		t.Fatal(err)
	}
	c.sleep = func(context.Context, time.Duration) error { return nil }
	c.now = func() time.Time { return t0 }
	return c, s
}

var q = Query{Namespace: "production", Controller: "payments-api", ControllerKind: "deployment", Window: time.Hour}

func TestValidResponseNormalisesToHourly(t *testing.T) {
	var gotPath, gotQuery string
	c, _ := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		// 30 minutes of data worth $2.50 -> $5.00/hour
		w.Write([]byte(body(
			alloc("production", "payments-api", "deployment", "c1", "2.5", "30", t0),
			alloc("production", "other", "deployment", "c1", "99", "60", t0))))
	})
	s, err := c.Allocation(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	if s.HourlyCost.String() != "5.000000" || s.TotalCost.String() != "2.500000" || s.Minutes != 30 {
		t.Fatalf("%+v", s)
	}
	if gotPath != "/model/allocation" || !strings.Contains(gotQuery, "aggregate=controller") ||
		!strings.Contains(gotQuery, "accumulate=true") || !strings.Contains(gotQuery, "window=1h") {
		t.Fatalf("request: %s?%s", gotPath, gotQuery)
	}
	if !s.WindowEnd.Equal(t0) || s.Age(t0.Add(90*time.Second)) != 90*time.Second {
		t.Fatalf("timestamps: %+v", s)
	}
}

func TestExponentNotationAndStrictMoney(t *testing.T) {
	c, _ := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(body(alloc("production", "payments-api", "Deployment", "", "1.5e-05", "60", t0))))
	})
	s, err := c.Allocation(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	if s.HourlyCost != 15 { // 0.000015 USD/h
		t.Fatalf("got %v", s.HourlyCost)
	}
}

func TestClassification(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   Class
	}{
		{"auth 401", 401, "nope", ClassAuth},
		{"auth 403", 403, "nope", ClassAuth},
		{"server 500", 500, "boom", ClassTransient},
		{"throttled 429", 429, "slow down", ClassTransient},
		{"bad request", 400, "bad window", ClassClient},
		{"redirect not followed", 302, "", ClassClient},
		{"malformed json", 200, `{"code":200,"data":[{`, ClassMalformed},
		{"html page", 200, `<html>login</html>`, ClassMalformed},
		{"missing code", 200, `{"data":[{}]}`, ClassMalformed},
		{"non 200 code", 200, `{"code":500,"data":[{}]}`, ClassMalformed},
		{"empty data", 200, `{"code":200,"data":[]}`, ClassNoData},
		{"empty set", 200, `{"code":200,"data":[{}]}`, ClassNoData},
		{"two sets", 200, `{"code":200,"data":[{},{}]}`, ClassMalformed},
		{"other workload only", 200, body(alloc("production", "x", "deployment", "c", "1", "60", t0)), ClassNoData},
		{"other kind only", 200, body(alloc("production", "payments-api", "statefulset", "c", "1", "60", t0)), ClassNoData},
		{"null totalCost", 200, strings.Replace(body(alloc("production", "payments-api", "deployment", "c", "1", "60", t0)), `"totalCost":1`, `"totalCost":null`, 1), ClassMalformed},
		{"missing minutes", 200, strings.Replace(body(alloc("production", "payments-api", "deployment", "c", "1", "60", t0)), `"minutes":60,`, ``, 1), ClassMalformed},
		{"negative cost", 200, body(alloc("production", "payments-api", "deployment", "c", "-1", "60", t0)), ClassMalformed},
		{"zero minutes", 200, body(alloc("production", "payments-api", "deployment", "c", "1", "0", t0)), ClassMalformed},
		{"string cost", 200, strings.Replace(body(alloc("production", "payments-api", "deployment", "c", "1", "60", t0)), `"totalCost":1`, `"totalCost":"NaN"`, 1), ClassMalformed},
		{"bad end time", 200, strings.Replace(body(alloc("production", "payments-api", "deployment", "c", "1", "60", t0)), t0.Format(time.RFC3339), "yesterday", 2), ClassMalformed},
		{"ambiguous clusters", 200, body(
			alloc("production", "payments-api", "deployment", "c1", "1", "60", t0),
			alloc("production", "payments-api", "deployment", "c2", "1", "60", t0)), ClassAmbiguous},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := newTest(t, func(w http.ResponseWriter, r *http.Request) {
				if tc.status == 302 {
					http.Redirect(w, r, "http://169.254.169.254/", 302)
					return
				}
				w.WriteHeader(tc.status)
				w.Write([]byte(tc.body))
			})
			_, err := c.Allocation(context.Background(), q)
			if got := ClassOf(err); got != tc.want {
				t.Fatalf("class=%q want %q (err=%v)", got, tc.want, err)
			}
		})
	}
}

func TestClusterDisambiguation(t *testing.T) {
	c, _ := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(body(
			alloc("production", "payments-api", "deployment", "c1", "1", "60", t0),
			alloc("production", "payments-api", "deployment", "c2", "3", "60", t0))))
	})
	qq := q
	qq.Cluster = "c2"
	s, err := c.Allocation(context.Background(), qq)
	if err != nil || s.HourlyCost.String() != "3.000000" {
		t.Fatalf("%v %v", s, err)
	}
}

func TestRetriesTransientThenSucceeds(t *testing.T) {
	var calls atomic.Int32
	c, _ := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) < 3 {
			w.WriteHeader(503)
			return
		}
		w.Write([]byte(body(alloc("production", "payments-api", "deployment", "c", "1", "60", t0))))
	})
	var slept []time.Duration
	c.sleep = func(_ context.Context, d time.Duration) error { slept = append(slept, d); return nil }
	c.jit = func() float64 { return 1 }
	if _, err := c.Allocation(context.Background(), q); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 3 || len(slept) != 2 || slept[0] != c.BaseBackoff || slept[1] != 2*c.BaseBackoff {
		t.Fatalf("calls=%d slept=%v", calls.Load(), slept)
	}
}

func TestRetryBoundedAndNotForPermanent(t *testing.T) {
	var calls atomic.Int32
	c, _ := newTest(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(500) })
	_, err := c.Allocation(context.Background(), q)
	if ClassOf(err) != ClassTransient || calls.Load() != 3 {
		t.Fatalf("class=%v calls=%d", ClassOf(err), calls.Load())
	}
	calls.Store(0)
	c2, _ := newTest(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(401) })
	if _, err := c2.Allocation(context.Background(), q); ClassOf(err) != ClassAuth || calls.Load() != 1 {
		t.Fatalf("auth must not be retried: %v calls=%d", err, calls.Load())
	}
}

func TestBackoffIsCappedAndJittered(t *testing.T) {
	c := &Client{BaseBackoff: time.Second, MaxBackoff: 4 * time.Second, jit: func() float64 { return 0.5 }}
	for attempt, want := range map[int]time.Duration{1: 500 * time.Millisecond, 2: time.Second, 3: 2 * time.Second, 10: 2 * time.Second} {
		if got := c.backoff(attempt); got != want {
			t.Errorf("attempt %d: %v want %v", attempt, got, want)
		}
	}
}

func TestTimeoutIsTransientAndBounded(t *testing.T) {
	c, s := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(3 * time.Second):
		case <-r.Context().Done():
		}
	})
	c.http.Timeout = 50 * time.Millisecond
	c.MaxAttempts = 2
	start := time.Now()
	_, err := c.Allocation(context.Background(), q)
	if ClassOf(err) != ClassTransient || time.Since(start) > 2*time.Second {
		t.Fatalf("err=%v took=%v", err, time.Since(start))
	}
	_ = s
}

func TestContextCancellationStopsRetries(t *testing.T) {
	var calls atomic.Int32
	c, _ := newTest(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(500) })
	ctx, cancel := context.WithCancel(context.Background())
	c.sleep = func(context.Context, time.Duration) error { cancel(); return ctx.Err() }
	if _, err := c.Allocation(ctx, q); err == nil || calls.Load() != 1 {
		t.Fatalf("err=%v calls=%d", err, calls.Load())
	}
}

func TestCredentialsNeverAppearInErrors(t *testing.T) {
	const secret = "SUPER-SECRET-TOKEN-123"
	var seen string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("Authorization")
		w.WriteHeader(403)
		w.Write([]byte("token " + secret + " rejected"))
	}))
	defer s.Close()
	hc, _ := httpx.NewClient(httpx.Options{BearerToken: secret}, time.Second)
	c, _ := New(s.URL, hc)
	c.sleep = func(context.Context, time.Duration) error { return nil }
	_, err := c.Allocation(context.Background(), q)
	if seen != "Bearer "+secret {
		t.Fatalf("token not sent: %q", seen)
	}
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("credential leaked into error: %v", err)
	}
	var ke *Error
	if !errors.As(err, &ke) || ke.Class != ClassAuth {
		t.Fatalf("%v", err)
	}
}

func TestParseBaseURL(t *testing.T) {
	for in, ok := range map[string]bool{
		"http://kubecost.kubecost:9090": true, "https://kc.example.com/": true, "https://kc.example.com/sub": true,
		"ftp://x": false, "": false, "http://": false, "https://user:pw@kc.example.com": false,
		"https://kc.example.com?x=1": false, "https://kc.example.com#f": false, "not a url": false,
	} {
		_, err := ParseBaseURL(in)
		if (err == nil) != ok {
			t.Errorf("ParseBaseURL(%q) err=%v want ok=%v", in, err, ok)
		}
	}
}

func TestInputValidation(t *testing.T) {
	c, _ := newTest(t, func(w http.ResponseWriter, r *http.Request) { t.Fatal("must not call the API") })
	for _, bad := range []Query{{Controller: "x", Window: time.Hour}, {Namespace: "x", Window: time.Hour}, {Namespace: "x", Controller: "y"}} {
		if _, err := c.Allocation(context.Background(), bad); ClassOf(err) != ClassClient {
			t.Errorf("%+v: %v", bad, err)
		}
	}
}

func TestSampleValidate(t *testing.T) {
	ok := Sample{WindowEnd: t0.Add(-30 * time.Second), Minutes: 58, Window: time.Hour}
	if err := ok.Validate(t0, 2*time.Minute, 0.5); err != nil {
		t.Fatal(err)
	}
	stale := Sample{WindowEnd: t0.Add(-10 * time.Minute), Minutes: 60, Window: time.Hour}
	if ClassOf(stale.Validate(t0, 2*time.Minute, 0.5)) != ClassStale {
		t.Fatal("stale not detected")
	}
	partial := Sample{WindowEnd: t0, Minutes: 5, Window: time.Hour}
	if ClassOf(partial.Validate(t0, 2*time.Minute, 0.5)) != ClassStale {
		t.Fatal("partial window not detected")
	}
	future := Sample{WindowEnd: t0.Add(time.Hour), Minutes: 60, Window: time.Hour}
	if ClassOf(future.Validate(t0, 2*time.Minute, 0.5)) != ClassMalformed {
		t.Fatal("future sample not rejected")
	}
}

func TestFormatWindow(t *testing.T) {
	for in, want := range map[time.Duration]string{time.Hour: "1h", 15 * time.Minute: "15m", 90 * time.Second: "90s", 36 * time.Hour: "36h"} {
		if got := formatWindow(in); got != want {
			t.Errorf("%v: %s", in, got)
		}
	}
}
