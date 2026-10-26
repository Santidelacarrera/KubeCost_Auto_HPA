package kubecost

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Retry-After from a 429 replaces a shorter backoff, but never exceeds the cap.
func TestRetryAfterHonouredAndCapped(t *testing.T) {
	cases := []struct {
		name, header string
		want         time.Duration
	}{
		{"delta seconds", "7", 7 * time.Second},
		{"capped", "3600", maxRetryAfter},
		{"http date", t0.Add(10 * time.Second).UTC().Format(http.TimeFormat), 10 * time.Second},
		{"past http date ignored", t0.Add(-time.Minute).UTC().Format(http.TimeFormat), 0},
		{"garbage ignored", "soon", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			c, _ := newTest(t, func(w http.ResponseWriter, r *http.Request) {
				if calls.Add(1) == 1 {
					w.Header().Set("Retry-After", tc.header)
					w.WriteHeader(http.StatusTooManyRequests)
					return
				}
				w.Write([]byte(body(alloc("production", "payments-api", "deployment", "c1", "2.5", "60", t0))))
			})
			c.jit = func() float64 { return 0 } // isolate the server hint from jittered backoff
			var slept []time.Duration
			c.sleep = func(_ context.Context, d time.Duration) error { slept = append(slept, d); return nil }
			if _, err := c.Allocation(context.Background(), q); err != nil {
				t.Fatal(err)
			}
			if len(slept) != 1 || slept[0] != tc.want {
				t.Fatalf("slept %v, want [%v]", slept, tc.want)
			}
		})
	}
}

// Concurrent callers across workloads never exceed maxInFlight outstanding requests.
func TestInFlightRequestsAreBounded(t *testing.T) {
	var cur, peak atomic.Int32
	c, _ := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		n := cur.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(30 * time.Millisecond)
		cur.Add(-1)
		w.Write([]byte(body(alloc("production", "payments-api", "deployment", "c1", "2.5", "60", t0))))
	})
	c.sleep = sleepCtx
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.Allocation(context.Background(), q); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if p := peak.Load(); p > maxInFlight {
		t.Fatalf("peak in-flight %d exceeds limit %d", p, maxInFlight)
	}
}

// A caller waiting for a slot gives up on context cancellation without sending a request.
func TestWaitingForSlotHonoursContext(t *testing.T) {
	var hits atomic.Int32
	c, _ := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Write([]byte(body(alloc("production", "payments-api", "deployment", "c1", "2.5", "60", t0))))
	})
	for i := 0; i < maxInFlight; i++ {
		c.sem <- struct{}{} // saturate every slot
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := c.Allocation(ctx, q)
	if ClassOf(err) != ClassTransient {
		t.Fatalf("want transient error while waiting for a slot, got %v", err)
	}
	if hits.Load() != 0 {
		t.Fatalf("request was sent without a slot: %d hits", hits.Load())
	}
}
