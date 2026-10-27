package kubecost

import (
	"context"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The hand-written OpenCost-shaped response (see testdata/README.md: NOT captured from a live
// KubeCost) parses to the workload's own allocation, ignoring other controllers, the other
// namespace with the same controller name, and the synthetic __idle__ / __unallocated__ entries.
func TestContractOpenCostShapedResponse(t *testing.T) {
	var gotReq *http.Request
	c, _ := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		gotReq = r.Clone(r.Context())
		w.Write(fixture(t, "opencost_accumulated.json"))
	})
	s, err := c.Allocation(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	if s.TotalCost.String() != "0.152500" || s.HourlyCost.String() != "0.152500" || s.Minutes != 60 {
		t.Fatalf("wrong allocation picked: %+v", s)
	}
	if !s.WindowEnd.Equal(time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("window end %v", s.WindowEnd)
	}
	// The request is exactly the documented, read-only allocation query.
	if gotReq.Method != http.MethodGet || gotReq.URL.Path != "/model/allocation" || gotReq.Header.Get("Accept") != "application/json" {
		t.Fatalf("request %s %s", gotReq.Method, gotReq.URL)
	}
	v := gotReq.URL.Query()
	if len(v) != 3 || v.Get("window") != "1h" || v.Get("aggregate") != "controller" || v.Get("accumulate") != "true" {
		t.Fatalf("query %v", v)
	}
	if err := s.Validate(t0, 5*time.Minute, 0.5); err != nil {
		t.Fatalf("fresh, fully covered sample must validate: %v", err)
	}
}

func TestContractIdleAndUnallocatedAreNeverTheWorkload(t *testing.T) {
	c, _ := newTest(t, func(w http.ResponseWriter, r *http.Request) { w.Write(fixture(t, "opencost_accumulated.json")) })
	for _, name := range []string{"__idle__", "__unallocated__", ""} {
		_, err := c.Allocation(context.Background(), Query{Namespace: "production", Controller: name, ControllerKind: "deployment", Window: time.Hour})
		if name == "" && ClassOf(err) != ClassClient {
			t.Fatalf("empty controller must be rejected as a client error, got %v", err)
		}
		if name != "" && ClassOf(err) != ClassNoData {
			t.Fatalf("%q must read as no data, got %v", name, err)
		}
	}
}

// Schema drift must fail closed: the workload is "not present", never "costs nothing".
func TestContractSchemaDriftFailsClosed(t *testing.T) {
	c, _ := newTest(t, func(w http.ResponseWriter, r *http.Request) { w.Write(fixture(t, "drift_namespace_is_number.json")) })
	s, err := c.Allocation(context.Background(), q)
	if err == nil || ClassOf(err) != ClassNoData {
		t.Fatalf("drifted schema must be NoData, got %v", err)
	}
	if s != (Sample{}) {
		t.Fatalf("a failed read must not return a usable sample: %+v", s)
	}
}

// Whatever is wrong, a failure never carries a sample that could be read as zero cost.
func TestContractAFailureNeverReturnsAUsableSample(t *testing.T) {
	good := alloc("production", "payments-api", "deployment", "c", "1", "60", t0)
	bodies := map[string]string{
		"empty":            ``,
		"null":             `null`,
		"array":            `[]`,
		"no data":          `{"code":200,"data":null}`,
		"cost is prose":    strings.Replace(body(good), `"totalCost":1`, `"totalCost":"one dollar"`, 1),
		"cost has symbol":  strings.Replace(body(good), `"totalCost":1`, `"totalCost":"$1.00"`, 1),
		"cost 1e999":       strings.Replace(body(good), `"totalCost":1`, `"totalCost":1e999`, 1),
		"cost 1e30":        strings.Replace(body(good), `"totalCost":1`, `"totalCost":1e30`, 1),
		"minutes 1e999":    strings.Replace(body(good), `"minutes":60`, `"minutes":1e999`, 1),
		"minutes negative": strings.Replace(body(good), `"minutes":60`, `"minutes":-5`, 1),
		"end missing":      strings.Replace(strings.Replace(body(good), `"end":"`+t0.Format(time.RFC3339)+`"`, `"x":1`, 2), `"end":`, `"endx":`, 0),
	}
	for name, b := range bodies {
		b := b
		t.Run(name, func(t *testing.T) {
			c, _ := newTest(t, func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(b)) })
			s, err := c.Allocation(context.Background(), q)
			if err == nil {
				t.Fatalf("expected an error, got sample %+v", s)
			}
			if s != (Sample{}) {
				t.Fatalf("error with a non-zero sample: %+v", s)
			}
			if ClassOf(err) == "" {
				t.Fatalf("unclassified error %v", err)
			}
		})
	}
}

// Documented leniency: a numeric value serialised as a JSON string ("1.0") still reads as that number.
// It is unambiguous, so it is accepted rather than freezing the controller; any non-numeric text is not.
func TestContractNumericStringIsReadAsThatNumber(t *testing.T) {
	good := alloc("production", "payments-api", "deployment", "c", "1", "60", t0)
	c, _ := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(strings.Replace(body(good), `"totalCost":1`, `"totalCost":"2.5"`, 1)))
	})
	s, err := c.Allocation(context.Background(), q)
	if err != nil || s.TotalCost.String() != "2.500000" {
		t.Fatalf("%+v %v", s, err)
	}
}

// A hostile or broken endpoint cannot make the operator buffer an unbounded body.
func TestContractOversizedBodyIsBounded(t *testing.T) {
	var written atomic.Int64
	c, _ := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"code":200,"data":[{"x":"`))
		chunk := []byte(strings.Repeat("a", 1<<20))
		for i := 0; i < 64; i++ { // would be 64 MiB
			n, err := w.Write(chunk)
			written.Add(int64(n))
			if err != nil {
				return
			}
		}
	})
	_, err := c.Allocation(context.Background(), q)
	if ClassOf(err) != ClassMalformed && ClassOf(err) != ClassTransient {
		t.Fatalf("oversized body must fail closed, got %v", err)
	}
	if n := written.Load(); n > 40<<20 {
		t.Fatalf("client kept reading: %d bytes consumed", n)
	}
}

// A server that sends headers and then stalls must be cut off by the timeout, within the retry bound.
func TestContractStalledBodyTimesOutWithinTheRetryBound(t *testing.T) {
	c, _ := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte(`{"code":200,"data":[{`))
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	})
	c.http.Timeout = 150 * time.Millisecond
	c.MaxAttempts = 2
	start := time.Now()
	_, err := c.Allocation(context.Background(), q)
	if ClassOf(err) != ClassTransient {
		t.Fatalf("a stalled body is a transient failure, got %v", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("took %s: timeout/retry bound not enforced", d)
	}
}

// A slow KubeCost must not hold the reconcile past the caller's deadline.
func TestContractCallerDeadlineWins(t *testing.T) {
	c, _ := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := c.Allocation(ctx, q)
	if err == nil || time.Since(start) > 2*time.Second {
		t.Fatalf("deadline not honoured: %v after %s", err, time.Since(start))
	}
}
